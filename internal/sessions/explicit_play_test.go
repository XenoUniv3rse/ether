package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
	"github.com/braedonsaunders/ether/internal/store"
)

// playMarkPlayback records whether each source preparation is a viewer's
// explicit Play (core.ExplicitPlay), the mark the cache-hint ask needs.
type playMarkPlayback struct {
	*fakePlayback
	mu    sync.Mutex
	marks []bool
	byKey map[core.RatingKey][]bool
}

func (p *playMarkPlayback) note(ctx context.Context, key core.RatingKey) {
	p.mu.Lock()
	p.marks = append(p.marks, core.ExplicitPlay(ctx))
	if p.byKey == nil {
		p.byKey = map[core.RatingKey][]bool{}
	}
	p.byKey[key] = append(p.byKey[key], core.ExplicitPlay(ctx))
	p.mu.Unlock()
}

func (p *playMarkPlayback) seenFor(key core.RatingKey) []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bool(nil), p.byKey[key]...)
}

func (p *playMarkPlayback) seen() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bool(nil), p.marks...)
}

func (p *playMarkPlayback) PrepareSurface(ctx context.Context, u *core.User, key core.RatingKey, c core.ClientCaps, release string, exact bool) (*core.PlaySession, error) {
	p.note(ctx, key)
	return p.fakePlayback.PrepareSurface(ctx, u, key, c, release, exact)
}

func playMarkBroker(t *testing.T) (*Broker, *playMarkPlayback) {
	t.Helper()
	playback := &playMarkPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4}}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	t.Cleanup(events.Close)
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key}, nil
	}
	b, err := NewBroker(BrokerDeps{SurfacePolicy: testSessionPolicy, Events: events, Playback: playback, Lookup: lookup, MediaLookup: lookup, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, Progress: &fakeProgress{}, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(context.Background()) })
	return b, playback
}

// The broker marks only a start its frontend says a viewer pressed Play
// on (PlayIntent.ViewerPlay): a plan without it, a detail plan and a
// queued next item prepare unmarked, ViewerPlay or not.
func TestBrokerMarksOnlyAViewersPlay(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan func(*Broker, frontend.PlanRequest) error
		want bool
	}{
		{"viewer play", func(b *Broker, req frontend.PlanRequest) error {
			req.Intent.ViewerPlay = true
			_, err := b.Plan(context.Background(), req)
			return err
		}, true},
		{"plan without ViewerPlay", func(b *Broker, req frontend.PlanRequest) error {
			_, err := b.Plan(context.Background(), req)
			return err
		}, false},
		{"detail plan", func(b *Broker, req frontend.PlanRequest) error {
			req.Intent.ViewerPlay = true
			_, err := b.DetailPlan(context.Background(), req)
			return err
		}, false},
		{"queued", func(b *Broker, req frontend.PlanRequest) error {
			req.Intent.ViewerPlay, req.Intent.Queued = true, true
			_, err := b.Plan(context.Background(), req)
			return err
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, playback := playMarkBroker(t)
			req := frontend.PlanRequest{Viewer: &core.Viewer{ID: 2}, Device: core.Device{ID: "tv"}, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}}
			if err := tc.plan(b, req); err != nil {
				t.Fatal(err)
			}
			marks := playback.seen()
			if len(marks) == 0 {
				t.Fatal("no source preparation")
			}
			for _, m := range marks {
				if m != tc.want {
					t.Fatalf("preparation explicit Play %v, want %v", marks, tc.want)
				}
			}
		})
	}
}

// Through the Jellyfin routes and the real broker: an informational
// PlaybackInfo, the lazy stream it hands out (GET and the cold HEAD file
// probe), the authenticated static stream (the path a WebDAV copy takes
// too) and a PlaybackInfo that says IsPlayback true all prepare unmarked:
// Jellyfin/Emby never mark a Play.
func TestMediaBrowserRoutesNeverMarkAPlay(t *testing.T) {
	pb := &playMarkPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 8}}
	call := mountJellyfin(t, pb)
	const base = "https://ether.example"
	lazyPath := func(key core.RatingKey) string {
		w := call("POST", "/Items/"+ids.RatingKeyGUID(key)+"/PlaybackInfo", `{"IsPlayback":false}`)
		var doc struct{ MediaSources []struct{ Path string } }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &doc) != nil || len(doc.MediaSources) == 0 || !strings.HasPrefix(doc.MediaSources[0].Path, base+"/") {
			t.Fatalf("informational PlaybackInfo %d %s", w.Code, w.Body.String())
		}
		return strings.TrimPrefix(doc.MediaSources[0].Path, base)
	}
	fetched, probed, static, played := core.MovieKey(5), core.MovieKey(6), core.MovieKey(7), core.MovieKey(8)
	if w := call("GET", lazyPath(fetched), ""); w.Code != 200 {
		t.Fatalf("lazy GET %d %s", w.Code, w.Body.String())
	}
	if w := call("HEAD", lazyPath(probed), ""); w.Code != 200 {
		t.Fatalf("lazy HEAD %d", w.Code)
	}
	if w := call("GET", "/Videos/"+ids.RatingKeyGUID(static)+"/stream?Static=true", ""); w.Code != 200 {
		t.Fatalf("static stream %d %s", w.Code, w.Body.String())
	}
	for name, key := range map[string]core.RatingKey{"lazy GET": fetched, "cold HEAD": probed, "static stream": static} {
		marks := pb.seenFor(key)
		if len(marks) == 0 {
			t.Fatalf("%s prepared nothing", name)
		}
		for _, m := range marks {
			if m {
				t.Fatalf("%s prepared as a viewer's Play: %v", name, marks)
			}
		}
	}
	if w := call("POST", "/Items/"+ids.RatingKeyGUID(played)+"/PlaybackInfo", `{"IsPlayback":true}`); w.Code != 200 {
		t.Fatalf("playback PlaybackInfo %d %s", w.Code, w.Body.String())
	}
	if marks := pb.seenFor(played); len(marks) == 0 {
		t.Fatal("playback PlaybackInfo prepared nothing")
	} else {
		for _, m := range marks {
			if m {
				t.Fatalf("playback PlaybackInfo prepared as a viewer's Play: %v", marks)
			}
		}
	}
}

// mountJellyfin mounts the Jellyfin frontend on the real broker over pb and
// returns a caller for its routes.
func mountJellyfin(t *testing.T, pb core.Playback) func(method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k, Type: core.TypeMovie, Title: "Film"}, nil
	}
	bus := NewEventBus(nil, active)
	t.Cleanup(bus.Close)
	b, err := NewBroker(BrokerDeps{SurfacePolicy: testSessionPolicy, DB: db, Events: bus, Playback: pb, Transcoder: fakeBrokerTranscoder{}, Lookup: lookup, MediaLookup: lookup, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, Progress: &fakeProgress{}, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(ctx) })
	lazy, err := NewLazyStreams(db, b, "jellyfin", &consumerHLSGrants{protocol: "jellyfin"}, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	const base = "https://ether.example"
	inst, err := (mediabrowser.Jellyfin{MachineID: "test"}).Mount(ctx, frontend.Host{
		Sessions: BindSurface(b, "jellyfin"), LazyStreams: lazy,
		Capabilities: frontendtest.Capabilities{BuildFunc: func(_ context.Context, req frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
			return frontend.CapabilityResult{Client: core.ClientCaps{Protocol: "jellyfin", ClientID: req.Device.ClientID}}, nil
		}},
		SurfaceURL: func(context.Context, *http.Request) (string, error) { return base, nil },
		Identity:   frontendtest.Identity{ResolveFunc: func(context.Context, string) (*core.Viewer, error) { return &core.Viewer{ID: 1}, nil }},
		Experience: frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
			return frontend.ItemDetail{Title: core.Title{Key: req.Key, Type: core.TypeMovie, Title: "Film"}}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inst.Close(ctx) })
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", `MediaBrowser Token="account-secret", DeviceId="device"`)
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	return call
}

// hintPlayback prepares each surface's sources through the real debrid
// indexer, as playback does, so a cache-hint source sees what a Play asks.
type hintPlayback struct {
	*fakePlayback
	x        *debrid.Indexer
	mu       sync.Mutex
	prepared map[core.RatingKey]bool
}

func (p *hintPlayback) preparedFor(key core.RatingKey) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prepared[key]
}

func (p *hintPlayback) PrepareSurface(ctx context.Context, u *core.User, key core.RatingKey, c core.ClientCaps, release string, exact bool) (*core.PlaySession, error) {
	p.mu.Lock()
	if p.prepared == nil {
		p.prepared = map[core.RatingKey]bool{}
	}
	p.prepared[key] = true
	p.mu.Unlock()
	p.x.PrepareSources(ctx, []core.Release{{ID: "r", Protocol: core.ProtocolTorrent, InfoHash: fmt.Sprintf("%040x", int64(key)), Private: true}})
	return p.fakePlayback.PrepareSurface(ctx, u, key, c, release, exact)
}

// hintAccount is a debrid account with no answer of its own about any hash.
type hintAccount struct{}

func (hintAccount) Kind() string { return config.DebridRealDebrid }
func (hintAccount) CheckCached(context.Context, []string) (map[string]bool, error) {
	return nil, debrid.ErrUnsupported
}
func (hintAccount) Find(context.Context, string) (string, error) { return "", debrid.ErrUnsupported }
func (hintAccount) Acquire(context.Context, debrid.AcquireRequest) (*debrid.Acquired, error) {
	return nil, core.ErrNotCached
}
func (hintAccount) Link(context.Context, debrid.Handle) (debrid.DirectLink, error) {
	return debrid.DirectLink{}, debrid.ErrGone
}
func (hintAccount) Remove(context.Context, string) error            { return nil }
func (hintAccount) Account(context.Context) (debrid.Account, error) { return debrid.Account{}, nil }

type recordingHinter struct {
	mu    sync.Mutex
	asked [][]debrid.CacheHintCandidate
}

func (h *recordingHinter) Enabled() bool { return true }
func (h *recordingHinter) CacheHints(_ context.Context, cands []debrid.CacheHintCandidate, _ func(map[string][]string)) {
	h.mu.Lock()
	h.asked = append(h.asked, cands)
	h.mu.Unlock()
}
func (h *recordingHinter) calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.asked)
}

// Through the Jellyfin route, the real broker and the debrid indexer: no
// PlaybackInfo asks a cache-hint source, whether it omits IsPlayback, says
// false, or says true in the body or the query, POST or GET. Nor does the
// queue prefetch Findroid sends: PlaybackInfo with IsPlayback true for two
// episodes before the selected one's stream is read.
func TestPlaybackInfoNeverAsksCacheHints(t *testing.T) {
	var cfg config.Debrid
	cfg.Providers = []config.DebridProvider{{ID: "rd", Name: "rd", Kind: config.DebridRealDebrid, APIKey: "k", Enabled: true}}
	svc := debrid.NewService(cfg, debrid.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), NewProvider: func(config.DebridProvider, debrid.ProviderOptions) (debrid.Provider, error) {
		return hintAccount{}, nil
	}})
	hinter := &recordingHinter{}
	svc.SetCacheHinter(hinter)
	pb := &hintPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 8}, x: debrid.NewIndexer(nil, svc)}
	call := mountJellyfin(t, pb)
	for n, tc := range []struct{ method, query, body string }{
		{"POST", "", `{}`},
		{"POST", "", `{"IsPlayback":false}`},
		{"GET", "", ""},
		{"POST", "", `{"IsPlayback":true}`},
		{"POST", "?IsPlayback=true", `{}`},
		{"GET", "?IsPlayback=true", ""},
	} {
		key := core.MovieKey(int64(20 + n))
		if w := call(tc.method, "/Items/"+ids.RatingKeyGUID(key)+"/PlaybackInfo"+tc.query, tc.body); w.Code != 200 {
			t.Fatalf("%s %s %s: %d %s", tc.method, tc.query, tc.body, w.Code, w.Body.String())
		}
		if playback := tc.body != `{"IsPlayback":false}` && (tc.method == "POST" || tc.query != ""); playback && !pb.preparedFor(key) {
			t.Fatalf("%s %s %s: playback prepared no sources", tc.method, tc.query, tc.body)
		}
		if asked := hinter.calls(); asked != 0 {
			t.Fatalf("%s %s %s: hint source asked %d times, want none", tc.method, tc.query, tc.body, asked)
		}
	}
	// Findroid's queue: both episodes posted as playback, then the selected
	// one's stream read.
	selected, next := core.EpisodeKey(40, 1, 1), core.EpisodeKey(40, 1, 2)
	for _, key := range []core.RatingKey{selected, next} {
		if w := call("POST", "/Items/"+ids.RatingKeyGUID(key)+"/PlaybackInfo", `{"IsPlayback":true}`); w.Code != 200 {
			t.Fatalf("queued PlaybackInfo %d %s", w.Code, w.Body.String())
		}
	}
	if w := call("GET", "/Videos/"+ids.RatingKeyGUID(selected)+"/stream?Static=true", ""); w.Code != 200 {
		t.Fatalf("selected stream %d %s", w.Code, w.Body.String())
	}
	if !pb.preparedFor(selected) {
		t.Fatal("the selected episode prepared no sources")
	}
	if asked := hinter.calls(); asked != 0 {
		t.Fatalf("queue prefetch asked the hint source %d times, want none", asked)
	}
}
