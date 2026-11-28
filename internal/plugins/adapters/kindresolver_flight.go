package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/plugins"
)

func (r *KindResolver) resolveSourcesFlight(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, partIdx *int, source string, accept, exclude []string) (kindResolveResult, error) {
	if item.Unavailable != nil {
		return kindResolveResult{}, core.ErrNotAvailable
	}
	if r.mgr == nil {
		return kindResolveResult{}, core.ErrSourceUnavailable
	}
	if err := ctx.Err(); err != nil {
		return kindResolveResult{}, err
	}
	fk := flightKeyForItemSource(item.Ref, partIdx, source)
	key := fk
	key.background = core.SpeculativeIndexerWork(ctx) || core.IsBackgroundCheck(ctx)
	play := !key.background && r.preparingWait > 0 &&
		(ctx.Value(preparingWaitKey{}) != nil || core.IndexerPurpose(ctx) == core.IndexerPurposePlayback)
	// A real play must not join a single-slice detail or speculative call.
	if play || len(accept) > 0 || len(exclude) > 0 {
		variant, _ := json.Marshal(struct {
			Play    bool
			Accept  []string
			Exclude []string
		}{play, accept, exclude})
		key.variant = string(variant)
	}

	r.flightMu.Lock()
	if r.flightClosed {
		r.flightMu.Unlock()
		return kindResolveResult{}, context.Canceled
	}
	now := time.Now()
	for k, f := range r.flights {
		if !f.expires.IsZero() && !now.Before(f.expires) {
			delete(r.flights, k)
		}
	}
	fl := r.flights[key]
	if fl == nil {
		if len(r.flights) >= maxKindCandidateCacheEntries {
			r.flightMu.Unlock()
			return kindResolveResult{}, core.ErrJobsBusy
		}
		if partIdx != nil {
			part := *partIdx
			partIdx = &part
		}
		accept, exclude = slices.Clone(accept), slices.Clone(exclude)
		budget := plugins.ResolverTotal + 5*time.Second
		if play {
			budget = kindResolveFlightBudget
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
		rctx, state := withResolveSearchState(rctx)
		if play {
			state.playbackCtx = rctx
		}
		fl = &resolveFlight{done: make(chan struct{}), cancel: cancel, search: state}
		r.flights[key] = fl
		r.flightWG.Add(1)
		go func() {
			defer r.flightWG.Done()
			defer func() {
				if p := recover(); p != nil {
					fl.err = fmt.Errorf("kindresolver: resolve panic: %v", p)
				}
				cancel()
				r.flightMu.Lock()
				if r.flights[key] == fl {
					if fl.keep {
						fl.expires = time.Now().Add(kindResolveRetryTTL)
					} else {
						delete(r.flights, key)
					}
				}
				close(fl.done)
				r.flightMu.Unlock()
			}()
			for {
				fl.result, fl.err = r.callPluginResolve(rctx, rec, lib, item, partIdx, source, accept, exclude)
				var decision *kindDecisionError
				if !play || !errors.Is(fl.err, core.ErrPreparing) || errors.Is(fl.err, core.ErrDownloadQueued) ||
					errors.As(fl.err, &decision) {
					break
				}
				timer := time.NewTimer(preparingPollDelay(fl.err))
				select {
				case <-rctx.Done():
					timer.Stop()
				case <-timer.C:
				}
				if rctx.Err() != nil {
					fl.err = state.resolveEndError(rctx.Err())
					return
				}
			}
			if rctx.Err() != nil {
				fl.err = state.resolveEndError(rctx.Err())
				return
			}
			// A mixed reply includes a request-bound fallback. Resolve again on
			// the next play so a completed job can supply its prepared source.
			if fl.result.pendingFallback == nil && (source == "" || play) && fl.err == nil && len(fl.result.sources) > 0 && rctx.Err() == nil {
				r.storeCandidates(fk, fl.result.sources)
				if source == "" && len(exclude) == 0 && r.items != nil {
					_ = r.items.SavePartCandidates(rctx, item.Ref, partIdx, fl.result.sources)
				}
			}
		}()
	}
	r.flightMu.Unlock()

	var wait <-chan time.Time
	if play && !plugins.FrontendPreparationHold(ctx) {
		budget := r.preparingWait
		if deadline, ok := ctx.Deadline(); ok {
			budget = min(budget, time.Until(deadline)-kindPreparingDeadlineMargin)
		}
		timer := time.NewTimer(budget)
		defer timer.Stop()
		wait = timer.C
	}

	select {
	case <-fl.done:
		return r.takeResolveResult(key, fl)
	case <-wait:
		if r.keepResolveForRetry(key, fl) {
			return kindResolveResult{}, fl.search.searchError(core.ErrPrepareSlow, "timed out")
		}
		return r.takeResolveResult(key, fl)
	case <-ctx.Done():
		if play {
			r.keepResolveForRetry(key, fl)
		}
		return kindResolveResult{}, fl.search.resolveEndError(ctx.Err())
	}
}

// keepResolveForRetry retains the result if the request leaves before the
// flight finishes. The next waiter consumes it, including a terminal error.
func (r *KindResolver) keepResolveForRetry(key resolveFlightKey, fl *resolveFlight) bool {
	r.flightMu.Lock()
	defer r.flightMu.Unlock()
	select {
	case <-fl.done:
		return false
	default:
		if r.flights[key] == fl {
			fl.keep = true
		}
		return true
	}
}

func (r *KindResolver) takeResolveResult(key resolveFlightKey, fl *resolveFlight) (kindResolveResult, error) {
	r.flightMu.Lock()
	if r.flights[key] == fl {
		delete(r.flights, key)
	}
	r.flightMu.Unlock()
	return fl.result, fl.err
}

func (s *resolveSearchState) resolveEndError(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	return s.searchError(core.PreparingFromPending(2000), "timed out")
}
