package mediabrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/safety"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/store"
)

type videoDownloadPlayback struct{ *mbTestPlayback }

func (videoDownloadPlayback) ServeRange(w http.ResponseWriter, r *http.Request, _ string) {
	http.ServeContent(w, r, "Film.mp4", time.Time{}, bytes.NewReader([]byte("original bytes")))
}

func (p videoDownloadPlayback) PrepareExactVariant(ctx context.Context, viewer *core.User, item core.RatingKey, caps core.ClientCaps, release string) (*core.PlaySession, error) {
	return p.prepare(ctx, viewer, item, caps, release, true)
}

type downloadVersionHandles struct{ testVersionHandles }

func (h downloadVersionHandles) Resolve(ctx context.Context, scope frontend.VersionScope, id string) (frontend.VersionTarget, error) {
	target, err := h.testVersionHandles.Resolve(ctx, scope, id)
	target.Release = "fixture-release"
	return target, err
}

func TestVideoDownloadViewerPermission(t *testing.T) {
	for _, surface := range []struct {
		prefix string
		mount  func(context.Context, frontend.Host) (frontend.Instance, error)
	}{
		{"", (Jellyfin{MachineID: "test"}).Mount},
		{"/emby", (Emby{MachineID: "test"}).Mount},
	} {
		t.Run(surface.prefix, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.OpenMemory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			policy, err := safety.New(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			yes, no := true, false
			for id, allow := range map[int64]*bool{7: &yes, 8: &no} {
				if err := policy.Put(ctx, id, safety.Policy{AllowDownloads: allow, DownloadLimitUnit: "titles", DownloadLimitAmount: 1, DownloadLimitPeriod: "day"}); err != nil {
					t.Fatal(err)
				}
			}

			movie, episode := core.MovieKey(101), core.EpisodeKey(102, 1, 1)
			calls := 0
			preparing := true
			knownSize := false
			var downloadedRelease string
			playback := videoDownloadPlayback{&mbTestPlayback{prepare: func(_ context.Context, viewer *core.User, key core.RatingKey, caps core.ClientCaps, release string, exact bool) (*core.PlaySession, error) {
				calls++
				if exact {
					downloadedRelease = release
				}
				if viewer.ID != 7 || key != movie || caps.ClientID == "" {
					t.Fatalf("preparing with viewer %d, item %s, device %q", viewer.ID, key, caps.ClientID)
				}
				if preparing {
					return nil, &frontend.PreparingError{RetryAfter: 3 * time.Second}
				}
				return &core.PlaySession{ID: "part", Info: core.MediaInfo{Container: "mp4", Size: 14}}, nil
			}}}
			host := frontend.Host{
				Identity: frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
					if token == "allowed" {
						return &frontend.Viewer{ID: 7}, nil
					}
					return &frontend.Viewer{ID: 8}, nil
				}},
				Experience: frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
					kind := core.TypeMovie
					if req.Key == episode {
						kind = core.TypeEpisode
					}
					media := core.MediaInfo{Container: "mp4", Size: 600, Estimated: true}
					if knownSize {
						media.FileSize = 14
					}
					return frontend.ItemDetail{Title: frontend.Title{Key: req.Key, Type: kind, Title: "Film"},
						Versions: []frontend.Version{{ID: "private-release", Media: media}}}, nil
				}},
				ViewerPolicy:   bookLimitPolicy{policy},
				Capabilities:   frontendtest.Capabilities{},
				VersionHandles: &downloadVersionHandles{testVersionHandles{item: movie, handle: "4e0f0a56-014b-4883-89b7-e9465f03462e"}},
				Sessions:       sessions.NewLocalRuntime(sessions.LocalRuntimeDeps{Playback: playback}),
			}
			inst, err := surface.mount(ctx, host)
			if err != nil {
				t.Fatal(err)
			}
			defer inst.Close(ctx)
			request := func(method, path, token, byteRange string) *httptest.ResponseRecorder {
				t.Helper()
				req := httptest.NewRequest(method, surface.prefix+path+"?api_key="+token, nil)
				if byteRange != "" {
					req.Header.Set("Range", byteRange)
				}
				res := httptest.NewRecorder()
				inst.Handler().ServeHTTP(res, req)
				return res
			}

			item := "/Items/" + ids.RatingKeyGUID(movie)
			for token, allowed := range map[string]bool{"allowed": true, "denied": false} {
				res := request(http.MethodGet, item, token, "")
				var dto map[string]any
				if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &dto) != nil || dto["CanDownload"] != allowed {
					t.Fatalf("%s item = %d %s", token, res.Code, res.Body.String())
				}
				res = request(http.MethodGet, "/Users/Me", token, "")
				var user struct{ Policy map[string]any }
				if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &user) != nil || user.Policy["EnableContentDownloading"] != allowed {
					t.Fatalf("%s user policy = %d %s", token, res.Code, res.Body.String())
				}
			}
			sourceSize := func() (string, *int64) {
				t.Helper()
				res := request(http.MethodGet, item, "allowed", "")
				var dto struct {
					MediaSources []struct {
						ID   string
						Size *int64
					}
				}
				if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &dto) != nil || len(dto.MediaSources) != 1 {
					t.Fatalf("item sources %d %s", res.Code, res.Body)
				}
				return dto.MediaSources[0].ID, dto.MediaSources[0].Size
			}
			if _, size := sourceSize(); size != nil {
				t.Fatalf("estimated size was exposed: %d", *size)
			}

			for _, route := range []string{"/Download", "/File"} {
				if res := request(http.MethodGet, item+route, "denied", ""); res.Code != http.StatusForbidden {
					t.Fatalf("denied %s = %d", route, res.Code)
				}
			}
			if calls != 0 {
				t.Fatal("permission checks triggered source preparation")
			}
			if res := request(http.MethodGet, item+"/Download", "allowed", ""); res.Code != http.StatusServiceUnavailable || res.Header().Get("Retry-After") != "3" {
				t.Fatalf("preparing = %d, headers %v", res.Code, res.Header())
			}
			preparing = false
			knownSize = true
			source, size := sourceSize()
			if size == nil || *size != 14 {
				t.Fatalf("expected the selected file size, got %v", size)
			}
			if res := request(http.MethodHead, "/Items/"+source+"/Download", "allowed", ""); res.Code != http.StatusOK || res.Header().Get("Content-Length") != strconv.FormatInt(*size, 10) || res.Body.Len() != 0 {
				t.Fatalf("HEAD = %d, headers %v, body %q", res.Code, res.Header(), res.Body.String())
			}
			if downloadedRelease != "fixture-release" {
				t.Fatalf("HEAD selected %q", downloadedRelease)
			}
			for _, route := range []string{"/Download", "/File"} {
				res := request(http.MethodGet, item+route, "allowed", "bytes=9-13")
				if res.Code != http.StatusPartialContent || res.Body.String() != "bytes" || res.Header().Get("Content-Range") != "bytes 9-13/14" || res.Header().Get("Content-Length") != "5" || res.Header().Get("Content-Disposition") != "attachment; filename=Film.mp4" {
					t.Fatalf("%s Range = %d, headers %v, body %q", route, res.Code, res.Header(), res.Body.String())
				}
			}
			if res := request(http.MethodGet, "/Items/"+ids.RatingKeyGUID(episode)+"/Download", "allowed", ""); res.Code != http.StatusTooManyRequests {
				t.Fatalf("second title = %d", res.Code)
			}
		})
	}
}
