package debrid

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestRealDebridBrowsingLeavesTrialAddBudgetForPlay(t *testing.T) {
	fake := (&fakeRD{cached: true}).handler(t)
	var mu sync.Mutex
	var lastAdd time.Time
	var limited atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/torrents/addMagnet" {
			mu.Lock()
			now := time.Now()
			reject := now.Sub(lastAdd) < time.Minute
			if !reject {
				lastAdd = now
			}
			mu.Unlock()
			if reject {
				limited.Store(true)
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"too_many_requests","error_code":34}`)
				return
			}
		}
		fake.ServeHTTP(w, r)
	}))
	defer srv.Close()

	s := NewService(config.Debrid{CachedOnly: true, RemoveUncached: true, Providers: []config.DebridProvider{
		{ID: "rd", Kind: config.DebridRealDebrid, APIKey: testKey, Enabled: true},
	}}, Options{ProviderOptions: fastOpts(srv.URL)})

	// Show reveal inspects season packs under the unattended-search context.
	ctx := t.Context()
	show := core.WithUnattendedSearch(core.WithIndexerPurpose(ctx, core.IndexerPurposeWarmup))
	for i := range 30 {
		rel := core.Release{Protocol: core.ProtocolTorrent, InfoHash: fmt.Sprintf("%040x", i+1)}
		if match, definitive := s.CachedPackMatcher(show, rel); match != nil || definitive {
			t.Fatalf("cold pack %d must remain unknown", i)
		}
	}

	// Detail and next-episode warming validate candidates before opening media.
	warm, _ := core.WithSpeculative(ctx)
	for ep := range 20 {
		rel := core.Release{ID: fmt.Sprint(ep), Protocol: core.ProtocolTorrent, InfoHash: fmt.Sprintf("%040x", ep+1)}
		if _, err := s.Acquire(warm, rel); !errors.Is(err, core.ErrForegroundOnly) {
			t.Fatalf("cold episode %d must wait for Play, got %v", ep, err)
		}
	}
	rel := core.Release{ID: "play", Protocol: core.ProtocolTorrent, InfoHash: testHash}
	if _, err := s.Acquire(ctx, rel); err != nil {
		t.Fatalf("play after browsing: %v", err)
	}
	if limited.Load() {
		t.Fatal("the provider throttled Ether")
	}
}

// A next-episode probe may try an add while the preceding episode plays,
// but loses admission immediately to a foreground request on the account.
func TestNextEpisodeTrialAddYieldsToForeground(t *testing.T) {
	var pacer accountPacer
	playing := true
	ctx := core.WithNextEpisodePreparation(t.Context(), core.EpisodeKey(1, 1, 1), func() bool {
		return playing
	})
	add := func() error {
		_, err := pacedCall(ctx, &pacer, apiRequest{}, true, func(context.Context, apiRequest) (struct{}, error) {
			return struct{}{}, nil
		})
		return err
	}
	_, release, err := pacer.reserve(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	err = add()
	release()
	if !errors.Is(err, core.ErrBudgetExhausted) {
		t.Fatalf("next episode should yield to a foreground add, got %v", err)
	}
	if err := add(); err != nil {
		t.Fatalf("next episode should be admitted with spare budget, got %v", err)
	}
	playing = false
	if err := add(); !errors.Is(err, core.ErrForegroundOnly) {
		t.Fatalf("an inactive preceding episode should prevent an add, got %v", err)
	}
}
