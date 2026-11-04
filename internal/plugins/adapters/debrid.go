package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

// AttachDebrid registers native plugin providers without copying their sealed
// credentials into server configuration. Disable/remove immediately unregisters.
func AttachDebrid(mgr *plugins.Manager, svc *debrid.Service) {
	if mgr == nil || svc == nil {
		return
	}
	mgr.OnSync(func(_ context.Context, r plugins.Record) {
		var p debrid.Provider
		if r.Enabled && r.Manifest.HasCapability(plugins.CapDebridProvider) {
			p = &debridProvider{mgr: mgr, id: r.ID}
		}
		svc.RegisterPluginProvider(r.ID, r.Name, p)
	})
}

type debridCaller interface {
	Call(context.Context, string, plugins.Request) (plugins.Response, error)
}
type debridProvider struct {
	mgr debridCaller
	id  string
}

func (p *debridProvider) Kind() string { return "plugin:" + p.id }

// Native debrid routes use JSON POST bodies. Never forward response text into
// errors: upstream failures can contain signed links or credentials.
func (p *debridProvider) call(ctx context.Context, route string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := p.mgr.Call(ctx, p.id, plugins.Request{Method: "POST", Path: "/ether/debrid/" + route, Body: body})
	if err != nil {
		return err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		switch resp.Status {
		case 401, 403:
			return debrid.ErrAuth
		case 404, 410:
			return debrid.ErrGone
		case 409:
			return core.ErrNotCached
		case 429:
			return debrid.ErrRateLimited
		case 501:
			return debrid.ErrUnsupported
		}
		return fmt.Errorf("plugin debrid %s returned HTTP %d", route, resp.Status)
	}
	if out != nil {
		if err = json.Unmarshal(resp.Body, out); err != nil {
			return fmt.Errorf("plugin debrid %s returned invalid JSON", route)
		}
	}
	return nil
}
func (p *debridProvider) CheckCached(ctx context.Context, hashes []string) (map[string]bool, error) {
	var out struct {
		Cached map[string]bool `json:"cached"`
	}
	if err := p.call(ctx, "check", map[string]any{"hashes": hashes}, &out); err != nil {
		return nil, err
	}
	// Only return hashes the caller asked about; absence is not cached.
	result := map[string]bool{}
	for _, h := range hashes {
		result[h] = out.Cached[h]
	}
	return result, nil
}
func (p *debridProvider) Find(ctx context.Context, hash string) (string, error) {
	var out struct {
		TorrentID string `json:"torrentId"`
	}
	err := p.call(ctx, "find", map[string]string{"infoHash": hash}, &out)
	return out.TorrentID, err
}

type debridFile struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	Link string `json:"link"`
}

func (p *debridProvider) Acquire(ctx context.Context, q debrid.AcquireRequest) (*debrid.Acquired, error) {
	var out struct {
		TorrentID string       `json:"torrentId"`
		Ready     bool         `json:"ready"`
		Added     bool         `json:"added"`
		Files     []debridFile `json:"files"`
	}
	// Callbacks stay in the host. The plugin supplies files; Ether chooses the
	// wanted movie/episode with the same chooser all built-in providers use.
	err := p.call(ctx, "acquire", map[string]any{"infoHash": q.InfoHash, "magnet": q.Magnet, "torrentId": q.TorrentID, "cachedOnly": q.CachedOnly}, &out)
	if err != nil {
		return nil, err
	}
	if !out.Ready {
		if out.Added && out.TorrentID != "" && q.CachedOnly && q.RemoveUncached && (q.MayRemove == nil || q.MayRemove(ctx, out.TorrentID)) {
			_ = p.Remove(ctx, out.TorrentID)
		}
		return nil, core.ErrNotCached
	}
	if out.TorrentID == "" || len(out.Files) == 0 || len(out.Files) > 10000 || q.Choose == nil {
		return nil, fmt.Errorf("plugin debrid acquire returned no selectable files")
	}
	files := make([]debrid.File, 0, len(out.Files))
	for _, f := range out.Files {
		if f.Path != "" && f.Size > 0 {
			files = append(files, debrid.File{ID: f.ID, Path: f.Path, Size: f.Size, Link: f.Link})
		}
	}
	file, err := q.Choose(files)
	if err != nil {
		return nil, err
	}
	return &debrid.Acquired{TorrentID: out.TorrentID, File: file, Added: out.Added}, nil
}
func (p *debridProvider) Link(ctx context.Context, h debrid.Handle) (debrid.DirectLink, error) {
	var out struct {
		URL     string    `json:"url"`
		Size    int64     `json:"size"`
		Expires time.Time `json:"expires"`
	}
	err := p.call(ctx, "link", map[string]any{"torrentId": h.TorrentID, "fileId": h.FileID, "path": h.Path, "link": h.Link, "magnet": h.Magnet, "infoHash": h.InfoHash, "size": h.Size}, &out)
	if err != nil {
		return debrid.DirectLink{}, err
	}
	if err = safefetch.ValidatePublicURL(ctx, out.URL); err != nil {
		return debrid.DirectLink{}, fmt.Errorf("plugin debrid returned an unsafe media URL")
	}
	return debrid.DirectLink{URL: out.URL, Size: out.Size, Expires: out.Expires}, nil
}
func (p *debridProvider) Remove(ctx context.Context, id string) error {
	return p.call(ctx, "remove", map[string]string{"torrentId": id}, nil)
}
func (p *debridProvider) Account(ctx context.Context) (debrid.Account, error) {
	var out struct {
		User    string    `json:"user"`
		Premium bool      `json:"premium"`
		Expires time.Time `json:"expires"`
	}
	err := p.call(ctx, "account", struct{}{}, &out)
	return debrid.Account{User: out.User, Premium: out.Premium, Expires: out.Expires}, err
}
