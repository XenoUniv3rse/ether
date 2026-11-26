package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func (r *KindResolver) continueIndexerSearch(ctx context.Context, st *resolveSearchState, indexer core.ControlledSearchIndexer, q core.SourceQuery, req core.SearchRequest, opts core.IndexerSearchOptions) (core.SourceResolveResult, error) {
	q.Query, q.Category = req.Title, req.Category
	raw, err := json.Marshal(struct {
		Query, Category, Profile string
		Lookup                   *core.SourceLookup
		Skip, Only               []string
	}{q.Query, q.Category, q.Profile, q.Lookup, q.SkipIndexers, q.OnlyIndexers})
	if err != nil {
		return core.SourceResolveResult{}, err
	}
	key := string(raw)

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
		opts.ContinuationContext = st.playbackCtx
		opts.ContinueSearch = func(snapshot func(context.Context) core.IndexerSearchResult) { lookup.snapshot = snapshot }
		sr, err = indexer.SearchWithOptions(ctx, req, opts)
		if ctx.Err() != nil {
			return core.SourceResolveResult{}, ctx.Err()
		}
		if errors.Is(err, core.ErrBudgetExhausted) {
			noteResolveBudgetSkip(ctx)
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
	res, err := r.finishIndexerSearch(ctx, q.Query, q.Profile, sr.Releases, sr.Indexers, sr.Pending)
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
