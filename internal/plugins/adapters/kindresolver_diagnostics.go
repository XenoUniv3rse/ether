package adapters

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
)

func noteKindSourceFailure(ctx context.Context, err error) {
	state := resolveSearchStateFrom(ctx)
	if state == nil || err == nil {
		return
	}
	if errors.Is(err, core.ErrBudgetExhausted) {
		noteResolveBudgetSkip(ctx)
		return
	}
	if !isPermanentReleaseFailure(err) {
		return
	}
	state.mu.Lock()
	state.candidatesFound = true
	state.mu.Unlock()
}

func (r *KindResolver) recordKindFailure(ctx context.Context, item core.KindItem, err error) {
	if r.resolver == nil || core.IndexerPurpose(ctx) != core.IndexerPurposePlayback ||
		core.SpeculativeIndexerWork(ctx) || core.IsBackgroundCheck(ctx) || ctx.Err() != nil {
		return
	}
	switch {
	case errors.Is(err, core.ErrBudgetExhausted):
		r.resolver.RecordKindFailure(ctx, item.Key, item.Title, core.ErrBudgetExhausted)
	case errors.Is(err, core.ErrKindCandidatesRejected):
		r.resolver.RecordKindFailure(ctx, item.Key, item.Title, core.ErrKindCandidatesRejected)
	case errors.Is(err, core.ErrArticleMissing):
		r.resolver.RecordKindFailure(ctx, item.Key, item.Title, core.ErrArticleMissing)
	case errors.Is(err, core.ErrArticleCorrupt):
		r.resolver.RecordKindFailure(ctx, item.Key, item.Title, core.ErrArticleCorrupt)
	case isPermanentReleaseFailure(err):
		r.resolver.RecordKindFailure(ctx, item.Key, item.Title, core.ErrReleaseRejected)
	}
}

func (r *KindResolver) emptyKindResolve(ctx context.Context, item core.KindItem, part *int, key resolveFlightKey, state *resolveSearchState, excluded bool) (kindResolveResult, error) {
	if state.indexerLookupLimitHit() {
		return kindResolveResult{}, core.ErrSourceUnavailable
	}
	if state.hasPending() {
		if state.playbackCtx != nil {
			return kindResolveResult{}, core.PreparingFromPending(250)
		}
		return kindResolveResult{}, core.PreparingFromPending(2000)
	}
	r.clearResolveProgress(key)
	if state.hadBudgetSkip() {
		return kindResolveResult{}, core.ErrBudgetExhausted
	}
	if err := state.indexerPause(); err != nil {
		return kindResolveResult{}, err
	}
	if key.source == "" && !excluded {
		state.mu.Lock()
		found := state.candidatesFound
		state.mu.Unlock()
		if found {
			return kindResolveResult{}, core.ErrKindCandidatesRejected
		}
		if !key.background && r.items != nil {
			_ = r.items.SaveNegativePartResolution(ctx, item.Ref, part)
		}
	}
	return kindResolveResult{}, core.ErrNotAvailable
}
