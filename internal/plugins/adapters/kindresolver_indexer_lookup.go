package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/braedonsaunders/ether/internal/core"
)

// Bound the number of distinct indexer fan-outs a plugin can request in one
// play. Repeating a search to inspect new results does not spend another slot.
const maxResolveIndexerLookups = 24

type resolveIndexerLookup struct {
	mu       sync.Mutex
	started  bool
	snapshot func(context.Context) core.IndexerSearchResult
	scored   *core.SourceResolveResult
}

func indexerLookupKey(q core.SourceQuery) string {
	raw, _ := json.Marshal(struct {
		Query, Category, Profile string
		Lookup                   *core.SourceLookup
		Skip, Only               []string
	}{strings.TrimSpace(q.Query), strings.TrimSpace(q.Category), q.Profile, q.Lookup, q.SkipIndexers, q.OnlyIndexers})
	return "query:" + string(raw)
}

func (r *KindResolver) noteIndexerSearchStarted(ctx context.Context, key string, opts core.IndexerSearchOptions) {
	dir, ok := r.indexer.(core.IndexerDirectory)
	if !ok || resolveSearchStateFrom(ctx) == nil {
		return
	}
	skip, only := normalizeFilterSet(opts.SkipIndexers), normalizeFilterSet(opts.OnlyIndexers)
	var active []core.IndexerSearchStatus
	for _, status := range dir.EnabledIndexerStatuses(core.IndexerStatusPending) {
		if core.IndexerSkipped(ctx, status.IndexerID) || filterMatchesIndexer(skip, status.IndexerID, status.Indexer) ||
			(len(only) > 0 && !filterMatchesIndexer(only, status.IndexerID, status.Indexer)) {
			continue
		}
		active = append(active, status)
	}
	noteResolveSearchResult(ctx, key, core.SourceResolveResult{Indexers: active}, nil)
}

func (r *KindResolver) continueIndexerSearch(ctx context.Context, st *resolveSearchState, key string, indexer core.ControlledSearchIndexer, q core.SourceQuery, req core.SearchRequest, opts core.IndexerSearchOptions) (core.SourceResolveResult, error) {
	var err error

	st.mu.Lock()
	lookup := st.indexerSearches[key]
	if lookup == nil {
		if len(st.indexerSearches) >= maxResolveIndexerLookups {
			st.lookupLimitHit = true
			st.mu.Unlock()
			return core.SourceResolveResult{}, fmt.Errorf("%w: too many indexer searches for one play", core.ErrSourceUnavailable)
		}
		if st.indexerSearches == nil {
			st.indexerSearches = make(map[string]*resolveIndexerLookup)
		}
		lookup = &resolveIndexerLookup{}
		st.indexerSearches[key] = lookup
	}
	st.mu.Unlock()

	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	if ctx.Err() != nil {
		return core.SourceResolveResult{}, ctx.Err()
	}
	var sr core.IndexerSearchResult
	switch {
	case !lookup.started:
		r.noteIndexerSearchStarted(ctx, key, opts)
		opts.ContinuationContext = st.playbackCtx
		opts.ContinueSearch = func(snapshot func(context.Context) core.IndexerSearchResult) { lookup.snapshot = snapshot }
		sr, err = indexer.SearchWithOptions(ctx, req, opts)
		if errors.Is(err, core.ErrBudgetExhausted) {
			noteResolveBudgetSkip(ctx)
		}
		if ctx.Err() != nil || (err != nil && len(sr.Indexers) == 0 && len(sr.Pending) == 0) {
			// Keep a continuation installed even if the guest ran out of
			// time before it could consume the first snapshot.
			lookup.started = lookup.snapshot != nil
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return core.SourceResolveResult{Indexers: sr.Indexers, Pending: sr.Pending}, err
		}
		lookup.started = true
	default:
		sr = lookup.snapshot(ctx)
		if lookup.scored != nil && len(lookup.scored.Pending) == len(sr.Pending) {
			return *lookup.scored, nil
		}
	}
	if lookup.snapshot == nil {
		// Scoring can run out of guest time. Keep the raw response so
		// another slice can finish without starting a second search.
		snapshot := sr
		lookup.snapshot = func(context.Context) core.IndexerSearchResult { return snapshot }
	}
	res, err := r.finishIndexerSearch(ctx, req.Title, q.Profile, sr.Releases, sr.Indexers, sr.Pending)
	if err == nil {
		lookup.scored = &res
	}
	return res, err
}

func (s *resolveSearchState) indexerLookupLimitHit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookupLimitHit
}
