package playback

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// playMarkResolver holds Resolve at gate (when set), then records whether
// the resolution reads as a viewer's explicit Play and its purpose, as
// source preparation does when it decides whether to ask hint plugins.
type playMarkResolver struct {
	core.Resolver
	gate    chan struct{}
	entered chan struct{}
	mu      sync.Mutex
	marks   []bool
	purpose []string
}

func (r *playMarkResolver) Resolve(ctx context.Context, req core.ResolveRequest) (*core.Resolution, error) {
	if r.entered != nil {
		select {
		case r.entered <- struct{}{}:
		default:
		}
	}
	if r.gate != nil {
		select {
		case <-r.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	r.mu.Lock()
	r.marks = append(r.marks, core.ExplicitPlay(ctx))
	r.purpose = append(r.purpose, core.IndexerPurpose(ctx))
	r.mu.Unlock()
	return r.Resolver.Resolve(ctx, req)
}

func (r *playMarkResolver) seen() ([]bool, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.marks...), append([]string(nil), r.purpose...)
}

// A surface plan that is not a Play (a detail plan) and a Play share one
// preparation flight. The flight reads as explicit Play when the Play
// started it, or joined it before preparation looked; a detail plan alone
// never does.
func TestPreparationFlightLatchesExplicitPlay(t *testing.T) {
	detail := context.Background()
	play := core.WithExplicitPlay(context.Background())
	for _, tc := range []struct {
		name          string
		first, second context.Context
		want          bool
	}{
		{"detail then play", detail, play, true},
		{"play then detail", play, detail, true},
		{"detail alone", detail, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
			r := &playMarkResolver{Resolver: h.resolver, gate: make(chan struct{}), entered: make(chan struct{}, 1)}
			h.m.deps.Resolver = r
			caps := defaultCaps("silo")
			errs := make(chan error, 2)
			prepare := func(ctx context.Context) {
				_, err := h.m.PrepareSurface(ctx, owner, movieKey, caps, "", false)
				errs <- err
			}
			go prepare(tc.first)
			<-r.entered
			callers := 1
			if tc.second != nil {
				callers = 2
				go prepare(tc.second)
				waitFor(t, func() bool {
					h.m.mu.Lock()
					defer h.m.mu.Unlock()
					for _, f := range h.m.flights {
						if f.waiters == 2 {
							return true
						}
					}
					return false
				})
			}
			close(r.gate)
			for range callers {
				if err := <-errs; err != nil {
					t.Fatal(err)
				}
			}
			marks, purposes := r.seen()
			if len(marks) != 1 || marks[0] != tc.want {
				t.Fatalf("one shared resolution read explicit Play %v, want [%v]", marks, tc.want)
			}
			if purposes[0] != core.IndexerPurposePlayback {
				t.Fatalf("purpose %q: the playback purpose alone must not decide", purposes[0])
			}
		})
	}
}

// A download job prepares under the playback purpose, but nobody pressed
// Play: it never reads as explicit Play.
func TestDownloadPreparationIsNotExplicitPlay(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	r := &playMarkResolver{Resolver: h.resolver}
	h.m.deps.Resolver = r
	caps := defaultCaps("web/download/1")
	caps.Download = true
	if _, err := h.m.Prepare(context.Background(), owner, movieKey, caps); err != nil {
		t.Fatal(err)
	}
	marks, purposes := r.seen()
	if len(marks) == 0 || marks[0] || purposes[0] != core.IndexerPurposePlayback {
		t.Fatalf("download preparation: explicit Play %v, purpose %v; want false under playback", marks, purposes)
	}
}

// The pre-resolve play probe (the Plex frontend's guess that a play is
// coming: a queue window read, a detail page revisited) never reads as
// explicit Play, for a guessed play or the next episode prepared ahead.
func TestPlayProbeIsNotExplicitPlay(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func(context.Context) context.Context
	}{
		{"guessed play", func(ctx context.Context) context.Context { return ctx }},
		{"next episode", func(ctx context.Context) context.Context {
			return core.WithNextEpisodePreparation(ctx, movie2, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, []*fakeRelease{mkv("r1", 2<<30, 90*time.Minute)}, []string{"r1"})
			r := &playMarkResolver{Resolver: h.resolver}
			h.m.deps.Resolver = r
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			_ = h.m.ProbePlayMetadata(tc.ctx(ctx), owner, movieKey, defaultCaps("probe"))
			waitFor(t, func() bool { marks, _ := r.seen(); return len(marks) > 0 })
			marks, purposes := r.seen()
			if marks[0] || purposes[0] != core.IndexerPurposePlayback {
				t.Fatalf("play probe: explicit Play %v, purpose %v; want unmarked under playback", marks, purposes)
			}
		})
	}
}
