package adapters

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
	"github.com/braedonsaunders/ether/internal/resolver"
)

type stubBackend struct {
	search func(ctx context.Context, req core.SearchRequest, reserve func(context.Context, string) error) ([]core.Release, error)
}

func (b stubBackend) Search(ctx context.Context, req core.SearchRequest, reserve func(context.Context, string) error) ([]core.Release, error) {
	return b.search(ctx, req, reserve)
}

func (b stubBackend) FetchNZB(context.Context, core.Release, func(context.Context, string) error) ([]byte, error) {
	return nil, nil
}

func (b stubBackend) ResolveTorrent(_ context.Context, r core.Release, _ func(context.Context, string) error) (core.Release, error) {
	return r, nil
}

func statusMap(in []core.IndexerSearchStatus) map[string]core.IndexerSearchStatus {
	out := make(map[string]core.IndexerSearchStatus, len(in))
	for _, st := range in {
		out[st.IndexerID] = st
	}
	return out
}

// TestKindResolverIndexerSearchDeadlineAwareAndBackoff proves the W299
// invariants under synctest without wall-clock sleeps:
//  1. The default search returns fast results with the route's matching
//     reserve intact and the hung indexer reported as pending.
//  2. An explicit wait that would consume the route deadline is clamped.
//  3. A slow indexer finishing in the background populates Ether's search cache
//     for subsequent viewer calls.
//  4. Unbounded (no-deadline) calls keep waiting past the viewer reserve
//     unchanged.
//  5. Once a hung indexer times out 3 times at the 30 s search ceiling, it
//     enters circuit-breaker backoff and subsequent viewer resolves skip it
//     immediately with status:"skipped" and reason:"paused_backoff".
func TestKindResolverIndexerSearchDeadlineAwareAndBackoff(t *testing.T) {
	t.Run("viewer resolve returns fast results before deadline and fills cache in background", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			releaseSlow := make(chan struct{})
			slowDone := make(chan struct{})
			var cacheFillStarted atomic.Bool

			fast := stubBackend{
				search: func(_ context.Context, req core.SearchRequest, _ func(context.Context, string) error) ([]core.Release, error) {
					return []core.Release{{
						ID:       "fast-" + req.Title,
						GUID:     "fast-" + req.Title,
						Name:     req.Title + " Fast FLAC",
						NZBURL:   "http://fast.example/1.nzb",
						Protocol: core.ProtocolUsenet,
					}}, nil
				},
			}
			hung := stubBackend{
				search: func(ctx context.Context, req core.SearchRequest, _ func(context.Context, string) error) ([]core.Release, error) {
					if req.Title == "CacheFill" {
						if !cacheFillStarted.CompareAndSwap(false, true) {
							return nil, fmt.Errorf("expected the slow release from cache")
						}
						select {
						case <-releaseSlow:
							close(slowDone)
							return []core.Release{{
								ID:       "slow-cf",
								GUID:     "slow-cf",
								Name:     "CacheFill Slow FLAC",
								NZBURL:   "http://slow.example/cf.nzb",
								Protocol: core.ProtocolUsenet,
							}}, nil
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					// Never answers: blocks until context cancellation.
					<-ctx.Done()
					return nil, ctx.Err()
				},
			}

			mgr := newznab.NewManager(nil, []config.Indexer{
				{ID: "fast", Name: "Fast", Kind: config.IndexerKindCardigann, Enabled: true},
				{ID: "hung", Name: "LimeTorrents", Kind: config.IndexerKindCardigann, Enabled: true},
			}, newznab.Options{
				Backend: func(cfg config.Indexer) (newznab.Backend, error) {
					if cfg.ID == "fast" {
						return fast, nil
					}
					return hung, nil
				},
			})
			defer mgr.Close()

			debSvc := debrid.NewService(config.Debrid{}, debrid.Options{})
			wrapped := debrid.NewIndexer(&livetv.VODIndexer{
				Inner:   NewStreamSources(mgr),
				Service: func() *livetv.Service { return nil },
			}, debSvc)
			kr := NewKindResolver(KindResolverDeps{
				Indexer:  wrapped,
				Resolver: resolver.New(wrapped, nil, nil, config.Defaults(""), resolver.Options{}),
			})
			defer kr.Close()

			// Use the configured route cap and assert the remaining matching budget.
			resolveDeadline := wasm.DeadlineFor("/ether/v1/kinds/music/resolve")
			reserve := max(indexerSearchReserveFloor, resolveDeadline*indexerSearchReservePct/100)
			assertReserve := func(ctx context.Context) {
				t.Helper()
				deadline, _ := ctx.Deadline()
				if remaining := time.Until(deadline); remaining < reserve {
					t.Fatalf("remaining resolve budget = %s, want at least %s", remaining, reserve)
				}
			}
			rctx, rcancel := context.WithTimeout(context.Background(), resolveDeadline)
			defer rcancel()

			res, err := kr.ResolveSourcesWithResult(rctx, "ether.music", core.SourceQuery{
				Kind:  "indexer_search",
				Query: "NeverAnswers",
			})
			if err != nil {
				t.Fatalf("resolve default search: %v", err)
			}
			assertReserve(rctx)
			if len(res.Candidates) != 1 || res.Candidates[0].IndexerID != "fast" {
				t.Fatalf("candidates = %+v, want 1 from fast", res.Candidates)
			}
			byID := statusMap(res.Indexers)
			if byID["fast"].Status != core.IndexerStatusAnswered || byID["hung"].Status != core.IndexerStatusPending {
				t.Fatalf("statuses = %+v, want fast=answered hung=pending", res.Indexers)
			}
			if len(res.Pending) != 1 || res.Pending[0].IndexerID != "hung" {
				t.Fatalf("pending = %+v, want [hung]", res.Pending)
			}

			// 2. A timeout requesting the full route budget is also clamped.
			rctx2, rcancel2 := context.WithTimeout(context.Background(), resolveDeadline)
			defer rcancel2()
			res2, err := kr.ResolveSourcesWithResult(rctx2, "ether.music", core.SourceQuery{
				Kind:      "indexer_search",
				Query:     "ClampedTimeout",
				TimeoutMS: int(resolveDeadline.Milliseconds()),
			})
			if err != nil {
				t.Fatalf("clamped search: %v", err)
			}
			assertReserve(rctx2)
			if len(res2.Candidates) != 1 {
				t.Fatalf("clamped candidates = %d, want 1", len(res2.Candidates))
			}

			// 3. Background cache fill: slow indexer finishes after the viewer cut-off
			// and populates Ether's search cache for the next viewer call.
			rctx3, rcancel3 := context.WithTimeout(context.Background(), resolveDeadline)
			defer rcancel3()
			res3, err := kr.ResolveSourcesWithResult(rctx3, "ether.music", core.SourceQuery{
				Kind:  "indexer_search",
				Query: "CacheFill",
			})
			if err != nil {
				t.Fatalf("cache-fill initial search: %v", err)
			}
			if len(res3.Candidates) != 1 || len(res3.Pending) != 1 {
				t.Fatalf("cache-fill initial = %d candidates, %+v pending; want 1 and [hung]", len(res3.Candidates), res3.Pending)
			}
			rcancel3()
			close(releaseSlow)
			<-slowDone
			synctest.Wait()

			rctx4, rcancel4 := context.WithTimeout(context.Background(), resolveDeadline)
			defer rcancel4()
			res4, err := kr.ResolveSourcesWithResult(rctx4, "ether.music", core.SourceQuery{
				Kind:  "indexer_search",
				Query: "CacheFill",
			})
			if err != nil {
				t.Fatalf("cached follow-up search: %v", err)
			}
			if len(res4.Candidates) != 2 {
				t.Fatalf("cached follow-up candidates = %d, want 2 (fast + cached hung)", len(res4.Candidates))
			}
		})
	})

	t.Run("no deadline waits unchanged past viewer reserve", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fast := stubBackend{
				search: func(_ context.Context, _ core.SearchRequest, _ func(context.Context, string) error) ([]core.Release, error) {
					return []core.Release{{
						ID:        "f-1",
						GUID:      "f-1",
						Name:      "Job Fast FLAC",
						MagnetURL: "magnet:?xt=urn:btih:3333333333333333333333333333333333333333",
						Protocol:  core.ProtocolTorrent,
					}}, nil
				},
			}
			slow := stubBackend{
				search: func(ctx context.Context, _ core.SearchRequest, _ func(context.Context, string) error) ([]core.Release, error) {
					timer := time.NewTimer(4 * time.Second)
					defer timer.Stop()
					select {
					case <-timer.C:
						return []core.Release{{
							ID:        "s-1",
							GUID:      "s-1",
							Name:      "Job Slow FLAC",
							MagnetURL: "magnet:?xt=urn:btih:4444444444444444444444444444444444444444",
							Protocol:  core.ProtocolTorrent,
						}}, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				},
			}

			mgr := newznab.NewManager(nil, []config.Indexer{
				{ID: "fast", Name: "Fast", Kind: config.IndexerKindCardigann, Protocol: "torrent", Enabled: true},
				{ID: "slow", Name: "Slow", Kind: config.IndexerKindCardigann, Protocol: "torrent", Enabled: true},
			}, newznab.Options{
				Backend: func(cfg config.Indexer) (newznab.Backend, error) {
					if cfg.ID == "fast" {
						return fast, nil
					}
					return slow, nil
				},
			})
			defer mgr.Close()

			kr := NewKindResolver(KindResolverDeps{
				Indexer:  NewStreamSources(mgr),
				Resolver: resolver.New(NewStreamSources(mgr), nil, nil, config.Defaults(""), resolver.Options{}),
			})
			defer kr.Close()

			started := time.Now()
			res, err := kr.ResolveSourcesWithResult(context.Background(), "ether.music", core.SourceQuery{
				Kind:  "indexer_search",
				Query: "JobQuery",
			})
			if err != nil {
				t.Fatalf("unbounded job search: %v", err)
			}
			if got := time.Since(started); got != 4*time.Second {
				t.Fatalf("unbounded elapsed = %s, want 4s (waited for slow indexer)", got)
			}
			if len(res.Candidates) != 2 || len(res.Pending) != 0 {
				t.Fatalf("unbounded got %d candidates, %+v pending; want 2 and empty", len(res.Candidates), res.Pending)
			}
		})
	})

	t.Run("repeatedly stalled indexer is skipped on viewer resolves during backoff", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var hungSearchCalls atomic.Int32
			hungTimedOut := make(chan struct{}, 8)

			fast := stubBackend{
				search: func(_ context.Context, req core.SearchRequest, _ func(context.Context, string) error) ([]core.Release, error) {
					return []core.Release{{
						ID:        "f-" + req.Title,
						GUID:      "f-" + req.Title,
						Name:      req.Title + " Fast FLAC",
						MagnetURL: "magnet:?xt=urn:btih:5555555555555555555555555555555555555555",
						Protocol:  core.ProtocolTorrent,
					}}, nil
				},
			}
			hung := stubBackend{
				search: func(ctx context.Context, _ core.SearchRequest, _ func(context.Context, string) error) ([]core.Release, error) {
					hungSearchCalls.Add(1)
					<-ctx.Done()
					hungTimedOut <- struct{}{}
					return nil, ctx.Err()
				},
			}

			mgr := newznab.NewManager(nil, []config.Indexer{
				{ID: "fast", Name: "Fast", Kind: config.IndexerKindCardigann, Protocol: "torrent", Enabled: true},
				{ID: "hung", Name: "LimeTorrents", Kind: config.IndexerKindCardigann, Protocol: "torrent", Enabled: true},
			}, newznab.Options{
				Backend: func(cfg config.Indexer) (newznab.Backend, error) {
					if cfg.ID == "fast" {
						return fast, nil
					}
					return hung, nil
				},
			})
			defer mgr.Close()

			kr := NewKindResolver(KindResolverDeps{
				Indexer:  NewStreamSources(mgr),
				Resolver: resolver.New(NewStreamSources(mgr), nil, nil, config.Defaults(""), resolver.Options{}),
			})
			defer kr.Close()

			// Three resolves with a tighter caller deadline return fast results while hung times
			// out at the 30 s background ceiling each time, tripping the breaker.
			for i := range 3 {
				rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
				res, err := kr.ResolveSourcesWithResult(rctx, "ether.music", core.SourceQuery{
					Kind:  "indexer_search",
					Query: fmt.Sprintf("Stall-%d", i),
				})
				rcancel()
				if err != nil {
					t.Fatalf("stall %d: %v", i, err)
				}
				if len(res.Candidates) != 1 || len(res.Pending) != 1 {
					t.Fatalf("stall %d = %d candidates, %+v pending; want 1 and [hung]", i, len(res.Candidates), res.Pending)
				}
				<-hungTimedOut
				synctest.Wait()
			}
			if got := hungSearchCalls.Load(); got != 3 {
				t.Fatalf("hung search calls = %d, want 3", got)
			}

			// 4th viewer /resolve skips hung immediately with reason:"paused_backoff".
			rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer rcancel()
			started := time.Now()
			res, err := kr.ResolveSourcesWithResult(rctx, "ether.music", core.SourceQuery{
				Kind:  "indexer_search",
				Query: "AfterBackoff",
			})
			if err != nil {
				t.Fatalf("after-backoff resolve: %v", err)
			}
			if got := time.Since(started); got != 0 {
				t.Fatalf("after-backoff waited %s, want immediate return without waiting for cutoff", got)
			}
			if got := hungSearchCalls.Load(); got != 3 {
				t.Fatalf("hung was queried during backoff: calls=%d, want 3", got)
			}
			byID := statusMap(res.Indexers)
			if byID["hung"].Status != core.IndexerStatusSkipped || byID["hung"].Reason != core.IndexerSkipReasonPaused || byID["hung"].MS != 0 {
				t.Fatalf("hung status = %+v, want skipped (%s) at 0 ms", byID["hung"], core.IndexerSkipReasonPaused)
			}
			if len(res.Pending) != 0 {
				t.Fatalf("after-backoff pending = %+v, want empty", res.Pending)
			}
		})
	})

	t.Run("defaultIndexerWaitBudget formula invariants", func(t *testing.T) {
		now := time.Unix(1_700_000_000, 0)
		ceiling := 30 * time.Second
		cases := []struct {
			name string
			rem  time.Duration
			want time.Duration
		}{
			{"12s resolve", 12 * time.Second, 7800 * time.Millisecond},
			{"5s remaining", 5 * time.Second, 3250 * time.Millisecond},
			{"4s remaining", 4 * time.Second, 2500 * time.Millisecond},
			{"3s open", 3 * time.Second, 1500 * time.Millisecond},
			{"2s join point", 2 * time.Second, 500 * time.Millisecond},
			{"1s tight", 1 * time.Second, 250 * time.Millisecond},
			{"20s stremio", 20 * time.Second, 13 * time.Second},
		}
		for _, tc := range cases {
			ctx, cancel := context.WithDeadline(context.Background(), now.Add(tc.rem))
			got := defaultIndexerWaitBudget(ctx, now, ceiling)
			cancel()
			if got != tc.want {
				t.Errorf("%s (rem=%s): got %s, want %s", tc.name, tc.rem, got, tc.want)
			}
		}
		if got := defaultIndexerWaitBudget(context.Background(), now, ceiling); got != 0 {
			t.Errorf("no deadline: got %s, want 0", got)
		}
	})
}
