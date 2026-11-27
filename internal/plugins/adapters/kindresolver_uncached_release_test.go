package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/adapters"
)

type fakeTorBoxCreate struct {
	Hash            string
	AllowZip        string
	AddOnlyIfCached string
}

type fakeTorBoxTorrent struct {
	ID      int
	Hash    string
	Ready   bool
	Files   []fakeTorBoxFile
	Deleted bool
	Zipped  bool
}

type fakeTorBoxFile struct {
	ID   int
	Name string
	Body []byte
}

type fakeTorBoxServer struct {
	mu           sync.Mutex
	srv          *httptest.Server
	nextID       int
	torrents     map[int]*fakeTorBoxTorrent
	byHash       map[string]*fakeTorBoxTorrent
	filesByHash  map[string][]fakeTorBoxFile
	cachedHashes map[string]bool
	refuseHash   map[string]string // hash -> TorBox error code (e.g. "ACTIVE_LIMIT", "DOWNLOAD_TOO_LARGE")
	creates      []fakeTorBoxCreate
}

func newFakeTorBoxServer(t *testing.T) *fakeTorBoxServer {
	t.Helper()
	tb := &fakeTorBoxServer{
		nextID:       100,
		torrents:     map[int]*fakeTorBoxTorrent{},
		byHash:       map[string]*fakeTorBoxTorrent{},
		filesByHash:  map[string][]fakeTorBoxFile{},
		cachedHashes: map[string]bool{},
		refuseHash:   map[string]string{},
	}
	tb.srv = httptest.NewServer(http.HandlerFunc(tb.handle))
	t.Cleanup(tb.srv.Close)
	return tb
}

func (tb *fakeTorBoxServer) setFiles(hash string, files []fakeTorBoxFile, cached bool) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	h := strings.ToLower(hash)
	tb.filesByHash[h] = files
	if cached {
		tb.cachedHashes[h] = true
	} else {
		delete(tb.cachedHashes, h)
	}
}

func (tb *fakeTorBoxServer) setRefuse(hash, code string) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.refuseHash[strings.ToLower(hash)] = code
}

func (tb *fakeTorBoxServer) finish(hash string) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	h := strings.ToLower(hash)
	tb.cachedHashes[h] = true
	if tor := tb.byHash[h]; tor != nil {
		tor.Ready = true
	}
}

func (tb *fakeTorBoxServer) createCalls() []fakeTorBoxCreate {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	out := make([]fakeTorBoxCreate, len(tb.creates))
	copy(out, tb.creates)
	return out
}

func (tb *fakeTorBoxServer) activeDownloads() int {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	n := 0
	for _, tor := range tb.torrents {
		if !tor.Deleted {
			n++
		}
	}
	return n
}

func (tb *fakeTorBoxServer) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/torrents/createtorrent":
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		magnet := r.FormValue("magnet")
		allowZip := r.FormValue("allow_zip")
		onlyCached := r.FormValue("add_only_if_cached")
		hash := strings.ToLower(newznab.InfoHashFromMagnet(magnet))

		tb.mu.Lock()
		tb.creates = append(tb.creates, fakeTorBoxCreate{
			Hash:            hash,
			AllowZip:        allowZip,
			AddOnlyIfCached: onlyCached,
		})
		if code := tb.refuseHash[hash]; code != "" {
			tb.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   code,
				"detail":  code,
			})
			return
		}
		cached := tb.cachedHashes[hash]
		if onlyCached == "true" && !cached {
			tb.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   "NO_CACHED_TORRENT",
				"detail":  "Torrent is not cached",
			})
			return
		}
		if existing := tb.byHash[hash]; existing != nil && !existing.Deleted {
			id := existing.ID
			tb.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"torrent_id": id},
			})
			return
		}
		tb.nextID++
		id := tb.nextID
		tor := &fakeTorBoxTorrent{
			ID:    id,
			Hash:  hash,
			Ready: cached,
			Files: tb.filesByHash[hash],
		}
		tb.torrents[id] = tor
		tb.byHash[hash] = tor
		tb.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"torrent_id": id},
		})

	case "/torrents/mylist":
		tb.mu.Lock()
		defer tb.mu.Unlock()
		if rawID := r.URL.Query().Get("id"); rawID != "" {
			id, _ := strconv.Atoi(rawID)
			tor := tb.torrents[id]
			if tor == nil || tor.Deleted {
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": nil})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": torBoxTorrentJSON(tor)})
			return
		}
		list := make([]map[string]any, 0, len(tb.torrents))
		for _, tor := range tb.torrents {
			if !tor.Deleted {
				list = append(list, torBoxTorrentJSON(tor))
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": list})

	case "/torrents/requestdl":
		id, _ := strconv.Atoi(r.URL.Query().Get("torrent_id"))
		fileID, _ := strconv.Atoi(r.URL.Query().Get("file_id"))
		tb.mu.Lock()
		tor := tb.torrents[id]
		var found bool
		if tor != nil && !tor.Deleted && tor.Ready {
			for _, f := range tor.Files {
				if f.ID == fileID {
					found = true
					break
				}
			}
		}
		tb.mu.Unlock()
		if !found {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "FILE_NOT_FOUND"})
			return
		}
		link := fmt.Sprintf("%s/cdn/%d/%d", tb.srv.URL, id, fileID)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": link})

	case "/torrents/controltorrent":
		var body struct {
			TorrentID json.Number `json:"torrent_id"`
			Operation string      `json:"operation"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Operation == "delete" {
			id, _ := strconv.Atoi(body.TorrentID.String())
			tb.mu.Lock()
			if tor := tb.torrents[id]; tor != nil {
				tor.Deleted = true
				if tb.byHash[tor.Hash] == tor {
					delete(tb.byHash, tor.Hash)
				}
			}
			tb.mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})

	default:
		if strings.HasPrefix(r.URL.Path, "/cdn/") {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/cdn/"), "/")
			if len(parts) == 2 {
				id, _ := strconv.Atoi(parts[0])
				fileID, _ := strconv.Atoi(parts[1])
				tb.mu.Lock()
				tor := tb.torrents[id]
				var data []byte
				if tor != nil {
					for _, f := range tor.Files {
						if f.ID == fileID {
							data = f.Body
							break
						}
					}
				}
				tb.mu.Unlock()
				if data != nil {
					w.Header().Set("Content-Type", "application/octet-stream")
					http.ServeContent(w, r, "file.bin", time.Unix(1_700_000_000, 0), bytes.NewReader(data))
					return
				}
			}
		}
		http.NotFound(w, r)
	}
}

func torBoxTorrentJSON(tor *fakeTorBoxTorrent) map[string]any {
	files := make([]map[string]any, 0, len(tor.Files))
	for _, f := range tor.Files {
		files = append(files, map[string]any{
			"id":       f.ID,
			"name":     f.Name,
			"size":     len(f.Body),
			"mimetype": "application/octet-stream",
			"zipped":   tor.Zipped,
		})
	}
	return map[string]any{
		"id":                tor.ID,
		"hash":              tor.Hash,
		"download_present":  tor.Ready,
		"download_finished": tor.Ready,
		"files":             files,
	}
}

type payloadMediaEngine struct {
	core.Engine
	filesByURL map[string][]byte
}

func (e payloadMediaEngine) Open(_ context.Context, id string, data []byte) (core.MediaSource, error) {
	p, err := debrid.DecodePayload(data)
	if err != nil {
		return nil, err
	}
	body := e.filesByURL[p.Path]
	if body == nil {
		body = []byte("bytes:" + p.Path)
	}
	return &kindContextMediaSource{id: id, data: body}, nil
}

func newUncachedKindRig(t *testing.T, cachedOnly bool, tb *fakeTorBoxServer, resolveFn func(core.KindResolveRequest) (core.KindResolveResponse, error)) (*kindTestHarness, *debrid.Service, *adapters.KindResolver, *core.Viewer, map[string]core.RatingKey) {
	t.Helper()
	h := newKindTestHarness(t, 0, nil)
	ctx := context.Background()

	svc := debrid.NewService(config.Debrid{
		CachedOnly:     cachedOnly,
		RemoveUncached: true,
		Providers: []config.DebridProvider{
			{ID: "tb", Kind: config.DebridTorBox, Name: "TorBox", Priority: 1, Enabled: true, APIKey: "tb-test-key"},
		},
	}, debrid.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ProviderOptions: debrid.ProviderOptions{
			BaseURL:      tb.srv.URL,
			HTTPClient:   tb.srv.Client(),
			PollInterval: time.Millisecond,
			ReadyPolls:   2,
		},
	})

	h.rt.callContext = func(_ context.Context, req plugins.Request) (plugins.Response, error) {
		switch req.Path {
		case "/ether/health":
			return plugins.Response{Status: 200, Body: json.RawMessage(`{"status":"ok"}`)}, nil
		case "/ether/v1/kinds/recordings/resolve":
			var body core.KindResolveRequest
			if err := json.Unmarshal(req.Body, &body); err != nil {
				return plugins.Response{Status: 400}, nil
			}
			resp, err := resolveFn(body)
			if err != nil {
				return plugins.Response{Status: 500, Body: json.RawMessage(`{"error":"resolve failed"}`)}, nil
			}
			b, _ := json.Marshal(resp)
			return plugins.Response{Status: 200, Body: b}, nil
		default:
			return plugins.Response{Status: 200, Body: json.RawMessage(`{"items":[]}`)}, nil
		}
	}

	const pluginID = "com.example.roms"
	decl := recordingsKindDecl(true)
	decl["sync"] = map[string]any{"mode": "push"}
	installStudioManifest(t, h.mgr, map[string]any{
		"id":      pluginID,
		"version": "1.0.0",
		"name":    "Uncached Release Test",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "content",
			"summary":      "Uncached release test",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"catalog_provider", "kind_resolver"},
			"permissions":  map[string]any{"sources": "acquire"},
			"strings":      map[string]any{"en": "i18n/en.json"},
			"contributes":  map[string]any{"mediaKinds": []map[string]any{decl}},
		},
	}, map[string][]byte{"i18n/en.json": []byte(`{"lib_name":"Recordings"}`)})

	host := h.mgr.HostFor(pluginID)
	if _, err := host.KindUpsert(ctx, "recordings", []core.KindItem{
		{ID: "alb-1", Level: "album", Title: "Pack"},
		{ID: "game-1", Level: "track", ParentID: "alb-1", Index: 1, Title: "Game One", Playable: true},
		{ID: "game-2", Level: "track", ParentID: "alb-1", Index: 2, Title: "Game Two", Playable: true},
		{ID: "multi", Level: "track", ParentID: "alb-1", Index: 3, Title: "Multi Source", Playable: true},
		{ID: "optout", Level: "track", ParentID: "alb-1", Index: 4, Title: "Opt Out", Playable: true},
		{ID: "limit", Level: "track", ParentID: "alb-1", Index: 5, Title: "Limit Item", Playable: true},
		{ID: "refused", Level: "track", ParentID: "alb-1", Index: 6, Title: "Refused Failover", Playable: true},
	}); err != nil {
		t.Fatal(err)
	}

	keys := map[string]core.RatingKey{}
	for _, id := range []string{"game-1", "game-2", "multi", "optout", "limit", "refused"} {
		it, _, err := h.items.GetByExternal(ctx, pluginID, "recordings", id)
		if err != nil {
			t.Fatalf("load item %s: %v", id, err)
		}
		keys[id] = it.Key
	}

	kr := adapters.NewKindResolver(adapters.KindResolverDeps{
		Manager:   h.mgr,
		Libraries: h.libs,
		Items:     h.items,
		Blobs:     h.blobs,
		Catalog:   h.catalog,
		Debrid:    svc,
		Engine: payloadMediaEngine{
			filesByURL: map[string][]byte{
				"Pack/Game1.rom": []byte("ROM-ONE-DATA"),
				"Pack/Game2.rom": []byte("ROM-TWO-DATA"),
				"Alt/Game1.rom":  []byte("ALT-ONE-DATA"),
				"Cached/Hit.rom": []byte("CACHED-HIT-DATA"),
			},
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	viewer := &core.Viewer{ID: 1, IsOwner: true}
	return h, svc, kr, viewer, keys
}

func TestKindResolverReleaseUncachedFollowsOwnerPolicyAndCollectionSharing(t *testing.T) {
	ctx := context.Background()
	packHash := strings.Repeat("11", 20)
	altHash := strings.Repeat("22", 20)
	cachedHash := strings.Repeat("33", 20)
	limitHash := strings.Repeat("44", 20)
	refuseHash := strings.Repeat("55", 20)
	okHash := strings.Repeat("66", 20)

	tb := newFakeTorBoxServer(t)
	tb.setFiles(packHash, []fakeTorBoxFile{
		{ID: 1, Name: "Pack/Game1.rom", Body: []byte("ROM-ONE-DATA")},
		{ID: 2, Name: "Pack/Game2.rom", Body: []byte("ROM-TWO-DATA")},
	}, false)
	tb.setFiles(altHash, []fakeTorBoxFile{
		{ID: 1, Name: "Alt/Game1.rom", Body: []byte("ALT-ONE-DATA")},
	}, false)
	tb.setFiles(cachedHash, []fakeTorBoxFile{
		{ID: 1, Name: "Cached/Hit.rom", Body: []byte("CACHED-HIT-DATA")},
	}, true)
	tb.setFiles(limitHash, []fakeTorBoxFile{
		{ID: 1, Name: "Pack/Game1.rom", Body: []byte("ROM-ONE-DATA")},
	}, false)
	tb.setRefuse(limitHash, "ACTIVE_LIMIT")
	tb.setFiles(refuseHash, []fakeTorBoxFile{
		{ID: 1, Name: "Pack/Game1.rom", Body: []byte("ROM-ONE-DATA")},
	}, false)
	tb.setRefuse(refuseHash, "DOWNLOAD_TOO_LARGE")
	tb.setFiles(okHash, []fakeTorBoxFile{
		{ID: 1, Name: "Pack/Game1.rom", Body: []byte("ROM-ONE-DATA")},
	}, false)

	resolveCount := map[string]int{}
	_, svc, kr, viewer, keys := newUncachedKindRig(t, true, tb, func(req core.KindResolveRequest) (core.KindResolveResponse, error) {
		resolveCount[req.Item.ID]++
		switch req.Item.ID {
		case "game-1":
			return core.KindResolveResponse{
				Sources: []core.PlayableSource{
					{
						ID:      "pack-1",
						Pattern: "release",
						MIME:    "application/octet-stream",
						Release: &core.ReleaseHint{
							Magnet: "magnet:?xt=urn:btih:" + packHash,
							File:   "Pack/Game1.rom",
						},
					},
					{
						ID:      "alt-1",
						Pattern: "release",
						MIME:    "application/octet-stream",
						Release: &core.ReleaseHint{
							Magnet: "magnet:?xt=urn:btih:" + altHash,
							File:   "Alt/Game1.rom",
						},
					},
				},
			}, nil
		case "game-2":
			// Second item naming a different file in the SAME whole-collection torrent.
			return core.KindResolveResponse{
				Sources: []core.PlayableSource{
					{
						ID:      "pack-2",
						Pattern: "release",
						MIME:    "application/octet-stream",
						Release: &core.ReleaseHint{
							Magnet: "magnet:?xt=urn:btih:" + packHash,
							File:   "Pack/Game2.rom",
						},
					},
				},
			}, nil
		case "multi":
			// First candidate is uncached, second is cached: cached must win without queueing the first.
			return core.KindResolveResponse{
				Sources: []core.PlayableSource{
					{
						ID:      "uncached-first",
						Pattern: "release",
						MIME:    "application/octet-stream",
						Release: &core.ReleaseHint{
							Magnet: "magnet:?xt=urn:btih:" + altHash,
							File:   "Alt/Game1.rom",
						},
					},
					{
						ID:      "cached-second",
						Pattern: "release",
						MIME:    "application/octet-stream",
						Release: &core.ReleaseHint{
							Magnet: "magnet:?xt=urn:btih:" + cachedHash,
							File:   "Cached/Hit.rom",
						},
					},
				},
			}, nil
		case "optout":
			return core.KindResolveResponse{
				Sources: []core.PlayableSource{
					{
						ID:      "strict-cached",
						Pattern: "release",
						MIME:    "application/octet-stream",
						Release: &core.ReleaseHint{
							Magnet:     "magnet:?xt=urn:btih:" + altHash,
							File:       "Alt/Game1.rom",
							CachedOnly: true,
						},
					},
				},
			}, nil
		case "limit":
			return core.KindResolveResponse{
				Sources: []core.PlayableSource{
					{
						ID:      "at-limit",
						Pattern: "release",
						MIME:    "application/octet-stream",
						Release: &core.ReleaseHint{
							Magnet: "magnet:?xt=urn:btih:" + limitHash,
							File:   "Pack/Game1.rom",
						},
					},
					{
						ID:      "after-limit",
						Pattern: "release",
						MIME:    "application/octet-stream",
						Release: &core.ReleaseHint{
							Magnet: "magnet:?xt=urn:btih:" + okHash,
							File:   "Pack/Game1.rom",
						},
					},
				},
			}, nil
		case "refused":
			return core.KindResolveResponse{
				Sources: []core.PlayableSource{
					{
						ID:      "too-large",
						Pattern: "release",
						MIME:    "application/octet-stream",
						Release: &core.ReleaseHint{
							Magnet: "magnet:?xt=urn:btih:" + refuseHash,
							File:   "Pack/Game1.rom",
						},
					},
					{
						ID:      "fallback-ok",
						Pattern: "release",
						MIME:    "application/octet-stream",
						Release: &core.ReleaseHint{
							Magnet: "magnet:?xt=urn:btih:" + okHash,
							File:   "Pack/Game1.rom",
						},
					},
				},
			}, nil
		}
		return core.KindResolveResponse{}, nil
	})

	// 1. Default CachedOnly=true: refuses immediately with UncachedError{CachedOnly:true},
	// not ErrPreparing, and leaves zero downloads on TorBox.
	if _, err := kr.OpenKind(ctx, viewer, keys["game-1"], nil); err == nil {
		t.Fatal("cached-only OpenKind unexpectedly succeeded")
	} else {
		var ue *core.UncachedError
		if !errors.Is(err, core.ErrNotCached) || errors.Is(err, core.ErrPreparing) || !errors.As(err, &ue) || !ue.CachedOnly {
			t.Fatalf("cached-only err = %v (ue=%+v), want UncachedError{CachedOnly:true} without ErrPreparing", err, ue)
		}
	}
	if tb.activeDownloads() != 0 {
		t.Fatalf("cached-only left %d active downloads on TorBox, want 0", tb.activeDownloads())
	}

	// 2. Owner turns CachedOnly off live (no restart).
	cfg := svc.Config()
	cfg.CachedOnly = false
	svc.UpdateConfig(cfg)

	// 2a. Background / speculative / warmup contexts still NEVER queue downloads.
	specCtx, _ := core.WithSpeculative(ctx)
	for name, guardCtx := range map[string]context.Context{
		"background":  core.WithBackgroundBudget(ctx, func(context.Context, string, string, int64) error { return nil }),
		"speculative": specCtx,
		"warmup":      core.WithIndexerPurpose(ctx, core.IndexerPurposeWarmup),
		"unattended":  core.WithUnattendedSearch(ctx),
	} {
		if _, err := kr.OpenKind(guardCtx, viewer, keys["game-1"], nil); err == nil || errors.Is(err, core.ErrPreparing) || errors.Is(err, core.ErrDownloadQueued) {
			t.Fatalf("%s OpenKind = %v, must not queue download", name, err)
		}
	}
	if tb.activeDownloads() != 0 {
		t.Fatalf("guard contexts left %d downloads, want 0", tb.activeDownloads())
	}

	// 2b. Plugin opt-out release.cachedOnly=true stays cached-only even when owner allows downloads.
	if _, err := kr.OpenKind(ctx, viewer, keys["optout"], nil); err == nil {
		t.Fatal("optout OpenKind unexpectedly succeeded")
	} else {
		var ue *core.UncachedError
		if !errors.Is(err, core.ErrNotCached) || errors.Is(err, core.ErrPreparing) || !errors.As(err, &ue) || ue.CachedOnly {
			t.Fatalf("optout err = %v (ue=%+v), want UncachedError{CachedOnly:false} without ErrPreparing", err, ue)
		}
	}
	if tb.activeDownloads() != 0 {
		t.Fatalf("plugin cachedOnly opt-out left %d downloads, want 0", tb.activeDownloads())
	}

	// 2c. Multi-candidate with a cached fallback plays cached immediately without queueing the uncached first candidate.
	km, err := kr.OpenKind(ctx, viewer, keys["multi"], nil)
	if err != nil {
		t.Fatalf("multi cached fallback failed: %v", err)
	}
	buf := make([]byte, km.Size)
	if _, err := km.ReaderAt.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read cached fallback: %v", err)
	}
	_ = km.Closer.Close()
	if string(buf) != "CACHED-HIT-DATA" {
		t.Fatalf("cached fallback bytes = %q, want CACHED-HIT-DATA", string(buf))
	}
	if tb.activeDownloads() != 1 { // only cachedHash was added (and is already cached)
		// cachedHash is cached on TorBox; altHash must NOT have been sent with add_only_if_cached=""
		for _, c := range tb.createCalls() {
			if c.Hash == altHash && c.AddOnlyIfCached != "true" {
				t.Fatalf("uncached candidate %s was queued despite cached fallback: %+v", altHash, c)
			}
		}
	}

	// 3. Foreground OpenKind on game-1 sends ONLY packHash (not altHash) with allow_zip=false
	// and without add_only_if_cached, answering ErrPreparing wrapping ErrDownloadQueued.
	err = func() error {
		_, e := kr.OpenKind(ctx, viewer, keys["game-1"], nil)
		return e
	}()
	if !errors.Is(err, core.ErrPreparing) || !errors.Is(err, core.ErrDownloadQueued) {
		t.Fatalf("first uncached OpenKind err = %v, want ErrPreparing + ErrDownloadQueued", err)
	}
	if sec := core.PreparingRetrySeconds(err, 0); sec != 5 {
		t.Fatalf("PreparingRetrySeconds = %d, want 5", sec)
	}
	foundQueueCreate := false
	for _, c := range tb.createCalls() {
		if c.Hash == packHash && c.AddOnlyIfCached == "" {
			if c.AllowZip != "false" {
				t.Fatalf("TorBox createtorrent allow_zip = %q, want \"false\"", c.AllowZip)
			}
			foundQueueCreate = true
		}
		if c.Hash == altHash && c.AddOnlyIfCached == "" {
			t.Fatalf("second uncached candidate %s should not have been queued", altHash)
		}
	}
	if !foundQueueCreate {
		t.Fatalf("expected TorBox createtorrent without add_only_if_cached for %s; calls = %+v", packHash, tb.createCalls())
	}

	// 4. Retry on game-1 and open of game-2 (same collection torrent) while downloading:
	// both answer ErrPreparing without adding packHash again or queueing altHash.
	createsBeforeRetry := len(tb.createCalls())
	if _, err := kr.OpenKind(ctx, viewer, keys["game-1"], nil); !errors.Is(err, core.ErrPreparing) || !errors.Is(err, core.ErrDownloadQueued) {
		t.Fatalf("retry game-1 while downloading = %v, want ErrPreparing + ErrDownloadQueued", err)
	}
	if _, err := kr.OpenKind(ctx, viewer, keys["game-2"], nil); !errors.Is(err, core.ErrPreparing) || !errors.Is(err, core.ErrDownloadQueued) {
		t.Fatalf("game-2 same collection torrent while downloading = %v, want ErrPreparing + ErrDownloadQueued", err)
	}
	for _, c := range tb.createCalls()[createsBeforeRetry:] {
		if c.AddOnlyIfCached == "" {
			t.Fatalf("retry/sibling unexpectedly created a new download: %+v", c)
		}
	}

	// 5. Once the download finishes on TorBox, both game-1 and game-2 play immediately,
	// even if the owner turned CachedOnly back on in the meantime.
	tb.finish(packHash)
	cfg.CachedOnly = true
	svc.UpdateConfig(cfg)

	for _, tc := range []struct {
		id   string
		want string
	}{
		{"game-2", "ROM-TWO-DATA"},
		{"game-1", "ROM-ONE-DATA"},
	} {
		km, err := kr.OpenKind(ctx, viewer, keys[tc.id], nil)
		if err != nil {
			t.Fatalf("open %s after finish: %v", tc.id, err)
		}
		got := make([]byte, km.Size)
		if _, err := km.ReaderAt.ReadAt(got, 0); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("read %s: %v", tc.id, err)
		}
		_ = km.Closer.Close()
		if string(got) != tc.want {
			t.Fatalf("%s bytes = %q, want %q", tc.id, string(got), tc.want)
		}
	}

	// 6. Active download limit stops immediately without trying later candidates and does NOT report preparing.
	cfg.CachedOnly = false
	svc.UpdateConfig(cfg)
	if _, err := kr.OpenKind(ctx, viewer, keys["limit"], nil); !errors.Is(err, core.ErrDownloadLimit) || errors.Is(err, core.ErrPreparing) {
		t.Fatalf("limit err = %v, want ErrDownloadLimit without ErrPreparing", err)
	}
	for _, c := range tb.createCalls() {
		if c.Hash == okHash && c.AddOnlyIfCached == "" {
			t.Fatalf("candidate after ACTIVE_LIMIT should not have been queued: %+v", c)
		}
	}

	// 7. Per-torrent refusal (DOWNLOAD_TOO_LARGE) fails over to the next queueable candidate and queues it.
	if _, err := kr.OpenKind(ctx, viewer, keys["refused"], nil); !errors.Is(err, core.ErrPreparing) || !errors.Is(err, core.ErrDownloadQueued) {
		t.Fatalf("refused failover err = %v, want ErrPreparing + ErrDownloadQueued via second candidate", err)
	}

	// 8. Source-scoped OpenKindPartSource queues on first open, caches the candidate on ErrPreparing,
	// and plays once ready without re-invoking /resolve while preparing.
	beforePartResolves := resolveCount["refused"]
	if _, err := kr.OpenKindPartSource(ctx, viewer, keys["refused"], nil, "fallback-ok", nil); !errors.Is(err, core.ErrPreparing) || !errors.Is(err, core.ErrDownloadQueued) {
		t.Fatalf("OpenKindPartSource while downloading = %v, want ErrPreparing + ErrDownloadQueued", err)
	}
	afterFirstPart := resolveCount["refused"]
	if _, err := kr.OpenKindPartSource(ctx, viewer, keys["refused"], nil, "fallback-ok", nil); !errors.Is(err, core.ErrPreparing) || !errors.Is(err, core.ErrDownloadQueued) {
		t.Fatalf("second OpenKindPartSource while downloading = %v, want ErrPreparing + ErrDownloadQueued", err)
	}
	if resolveCount["refused"] != afterFirstPart || afterFirstPart != beforePartResolves+1 {
		t.Fatalf("OpenKindPartSource resolve count = %d -> %d -> %d, expected single resolve cached while preparing", beforePartResolves, afterFirstPart, resolveCount["refused"])
	}
	tb.finish(okHash)
	km, err = kr.OpenKindPartSource(ctx, viewer, keys["refused"], nil, "fallback-ok", nil)
	if err != nil {
		t.Fatalf("OpenKindPartSource after finish: %v", err)
	}
	_ = km.Closer.Close()
}

func TestKindResolveMixedPendingFallback(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ownerOnly  bool
		sourceOnly bool
		open       string
		alternate  string
		wantJob    bool
		wantQueued bool
	}{
		{name: "owner policy", ownerOnly: true, wantJob: true},
		{name: "source policy explicit version", sourceOnly: true, open: "version", wantJob: true},
		{name: "source scoped ticket", ownerOnly: true, open: "ticket", wantJob: true},
		{name: "queue allowed", wantQueued: true},
		{name: "cached source wins", ownerOnly: true, alternate: "cached"},
		{name: "unrelated failure", ownerOnly: true, alternate: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tb := newFakeTorBoxServer(t)
			uncachedHash, cachedHash := strings.Repeat("11", 20), strings.Repeat("33", 20)
			tb.setFiles(uncachedHash, []fakeTorBoxFile{{ID: 1, Name: "Pack/Game1.rom"}}, false)
			tb.setFiles(cachedHash, []fakeTorBoxFile{{ID: 1, Name: "Cached/Hit.rom", Body: []byte("CACHED-HIT-DATA")}}, true)
			uncached := core.PlayableSource{ID: "torrent", Pattern: "release", Release: &core.ReleaseHint{
				InfoHash: uncachedHash, File: "Pack/Game1.rom", CachedOnly: tc.sourceOnly,
			}}
			cached := core.PlayableSource{ID: "prepared", Pattern: "release", Release: &core.ReleaseHint{
				InfoHash: cachedHash, File: "Cached/Hit.rom",
			}}
			prepared := make(chan core.KindResolveRequest, 1)
			var ready atomic.Bool
			h, _, kr, viewer, keys := newUncachedKindRig(t, tc.ownerOnly, tb, func(req core.KindResolveRequest) (core.KindResolveResponse, error) {
				if ready.Load() {
					return core.KindResolveResponse{Sources: []core.PlayableSource{cached}}, nil
				}
				sources := []core.PlayableSource{uncached}
				switch tc.alternate {
				case "cached":
					sources = append(sources, cached)
				case "invalid":
					sources = append(sources, core.PlayableSource{ID: "invalid", Pattern: "invalid"})
				}
				return core.KindResolveResponse{Sources: sources, Pending: true, RetryInMS: 1700, Job: "/ether/job/prepare"}, nil
			})
			call := h.rt.callContext
			h.rt.callContext = func(ctx context.Context, req plugins.Request) (plugins.Response, error) {
				if req.Path == "/ether/job/prepare" {
					var request core.KindResolveRequest
					_ = json.Unmarshal(req.Body, &request)
					ready.Store(true)
					prepared <- request
					return plugins.Response{Status: 200, Body: json.RawMessage(`{"done":true}`)}, nil
				}
				return call(ctx, req)
			}
			const pluginID = "com.example.roms"
			policy, err := kr.ResolveSourcesWithResult(ctx, pluginID, core.SourceQuery{Kind: "debrid"})
			if err != nil || policy.Enabled == nil || !*policy.Enabled || policy.CachedOnly == nil || *policy.CachedOnly != tc.ownerOnly {
				t.Fatalf("debrid policy differs from configured policy: %v", err)
			}
			check, err := kr.ResolveSourcesWithResult(ctx, pluginID, core.SourceQuery{Kind: "debrid_check", InfoHash: uncachedHash})
			if err != nil || check.CachedOnly == nil || *check.CachedOnly != tc.ownerOnly || len(check.Candidates) != 1 || check.Candidates[0].Cached {
				t.Fatalf("debrid check did not return the uncached candidate and owner policy: %v", err)
			}

			key := keys["game-1"]
			switch tc.open {
			case "version", "ticket":
				const source = "torrent"
				if tc.open == "version" {
					_, err = kr.OpenKindSource(ctx, viewer, key, source, nil)
				} else {
					_, err = kr.OpenKindPartSource(ctx, viewer, key, nil, source, nil)
				}
			default:
				media, openErr := kr.OpenKind(ctx, viewer, key, nil)
				err = openErr
				if media != nil {
					_ = media.Closer.Close()
				}
			}
			switch {
			case tc.wantJob:
				var pending *core.PreparingError
				if !errors.As(err, &pending) || pending.RetryAfter != 1700*time.Millisecond || errors.Is(err, core.ErrDownloadQueued) {
					t.Fatalf("open = %v, want preparation with plugin retry hint", err)
				}
				select {
				case req := <-prepared:
					if req.Item.ID != "game-1" || (tc.open == "ticket" && req.Source != "torrent") {
						t.Fatal("job did not receive the original resolve identity")
					}
				case <-ctx.Done():
					t.Fatal("fallback job did not start")
				}
				media, err := kr.OpenKind(ctx, viewer, key, nil)
				if err != nil {
					t.Fatalf("retry did not open prepared source: %v", err)
				}
				_ = media.Closer.Close()
			case tc.wantQueued:
				if !errors.Is(err, core.ErrPreparing) || !errors.Is(err, core.ErrDownloadQueued) {
					t.Fatalf("open = %v, want queued torrent", err)
				}
			case tc.alternate == "cached":
				if err != nil {
					t.Fatalf("cached alternate did not play: %v", err)
				}
			default:
				if err == nil || errors.Is(err, core.ErrPreparing) {
					t.Fatalf("open = %v, want terminal source error", err)
				}
			}
		})
	}
}
