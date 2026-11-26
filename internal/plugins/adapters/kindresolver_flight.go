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

func (r *KindResolver) resolveSourcesFlight(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, partIdx *int, source string, accept, exclude []string) ([]core.PlayableSource, error) {
	if item.Unavailable != nil {
		return nil, core.ErrNotAvailable
	}
	if r.mgr == nil {
		return nil, core.ErrSourceUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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
		return nil, context.Canceled
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
			return nil, core.ErrJobsBusy
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
		fl = &resolveFlight{done: make(chan struct{}), cancel: cancel}
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
			if play {
				var state *resolveSearchState
				rctx, state = withResolveSearchState(rctx)
				state.playbackCtx = rctx
			}
			for {
				fl.sources, fl.err = r.callPluginResolve(rctx, rec, lib, item, partIdx, source, accept, exclude)
				if !play || !errors.Is(fl.err, core.ErrPreparing) || errors.Is(fl.err, core.ErrDownloadQueued) {
					break
				}
				timer := time.NewTimer(preparingPollDelay(fl.err))
				select {
				case <-rctx.Done():
					timer.Stop()
				case <-timer.C:
				}
				if rctx.Err() != nil {
					fl.err = kindResolveEndError(rctx.Err())
					return
				}
			}
			if play && rctx.Err() != nil {
				fl.err = kindResolveEndError(rctx.Err())
				return
			}
			if (source == "" || play) && fl.err == nil && len(fl.sources) > 0 && rctx.Err() == nil {
				r.storeCandidates(fk, fl.sources)
				if source == "" && len(exclude) == 0 && r.items != nil {
					_ = r.items.SavePartCandidates(rctx, item.Ref, partIdx, fl.sources)
				}
			}
		}()
	}
	r.flightMu.Unlock()

	var wait <-chan time.Time
	if play {
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
			return nil, core.ErrPrepareSlow
		}
		return r.takeResolveResult(key, fl)
	case <-ctx.Done():
		if play {
			r.keepResolveForRetry(key, fl)
		}
		return nil, ctx.Err()
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

func (r *KindResolver) takeResolveResult(key resolveFlightKey, fl *resolveFlight) ([]core.PlayableSource, error) {
	r.flightMu.Lock()
	if r.flights[key] == fl {
		delete(r.flights, key)
	}
	r.flightMu.Unlock()
	return fl.sources, fl.err
}

func kindResolveEndError(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	return core.ErrSourceUnavailable
}
