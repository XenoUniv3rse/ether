package experience

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const (
	// DescriptiveSearchWaitBound caps how long a live search request waits for
	// AI descriptive results before returning partial AI or keyword results.
	// Production models answer a new query in about 12-15 s; 10 s missed them.
	DescriptiveSearchWaitBound = 20 * time.Second
	// DescriptiveSearchSettleDelay holds an uncached descriptive query briefly
	// so search-as-you-type prefixes are superseded before starting an AI job.
	DescriptiveSearchSettleDelay = 200 * time.Millisecond
)

// Search resolves discovery-first keyword searches for the viewer's libraries.
// Natural is optional; false falls back to the catalogue.
type Search struct {
	Visibility   Visibility
	Vibe         VibeSearch
	Natural      func(context.Context, *core.User, string, int) ([]core.Title, bool)
	Catalog      func(context.Context, string, []core.MediaType, int) ([]core.Title, error)
	PeopleTitles func(context.Context, *core.User, string, int) ([]core.Title, error)
	// Unrestricted is a trusted host policy capability. Nil uses the
	// restricted, inherited-policy and availability path.
	Unrestricted func(*core.User) bool
	// Familiar, when set, reports the viewer's affinity (core.AffinityViewer
	// or core.AffinityServer) with titles among keys; the catalogue ranks
	// those first among equal matches.
	Familiar func(ctx context.Context, viewer *core.User, keys []core.RatingKey) map[core.RatingKey]int
	// Details reads in full the titles a candidate search returned partial
	// (core.SearchCandidates); nil when the catalogue cannot.
	Details func(context.Context, []core.Title) ([]core.Title, error)
	// PrefetchParents batches the inherited-policy reads for eligible child hits.
	PrefetchParents func(context.Context, []core.RatingKey)

	// WaitBound overrides DescriptiveSearchWaitBound when > 0.
	WaitBound time.Duration
	// SettleDelay overrides DescriptiveSearchSettleDelay when > 0; negative disables settle delay.
	SettleDelay time.Duration

	gate *descriptiveGate
}

type descriptiveFlightKey struct {
	viewer int64
	client string
}

type descriptiveFlight struct {
	query  string
	ctx    context.Context
	cancel context.CancelFunc
	refs   int
}

type descriptiveGate struct {
	active   atomic.Int64
	mu       sync.Mutex
	byClient map[descriptiveFlightKey]*descriptiveFlight
}

var fallbackDescriptiveGate = newDescriptiveGate()

func newDescriptiveGate() *descriptiveGate {
	return &descriptiveGate{byClient: map[descriptiveFlightKey]*descriptiveFlight{}}
}

func normalizeSearchQuery(query string) string {
	return strings.Join(strings.Fields(strings.ToLower(query)), " ")
}

func searchViewerID(viewer *core.User) int64 {
	if viewer == nil {
		return 0
	}
	return viewer.ID
}

// supersede cancels any active descriptive wait for (viewer, client) whose
// normalized query differs from query. Idle fast path is a single atomic load.
func (g *descriptiveGate) supersede(viewer *core.User, client, query string) (string, bool) {
	if g == nil || g.active.Load() == 0 {
		return "", false
	}
	norm := normalizeSearchQuery(query)
	key := descriptiveFlightKey{viewer: searchViewerID(viewer), client: client}
	g.mu.Lock()
	cur := g.byClient[key]
	if cur == nil || cur.query == norm {
		g.mu.Unlock()
		return "", false
	}
	prev := cur.query
	delete(g.byClient, key)
	g.active.Add(-1)
	cancel := cur.cancel
	g.mu.Unlock()
	cancel()
	return prev, true
}

func (g *descriptiveGate) enter(viewer *core.User, client, query string) (context.Context, string, bool, func()) {
	norm := normalizeSearchQuery(query)
	key := descriptiveFlightKey{viewer: searchViewerID(viewer), client: client}
	g.mu.Lock()
	var prevQuery string
	var superseded bool
	var prevCancel context.CancelFunc
	cur := g.byClient[key]
	if cur != nil && cur.query == norm && cur.ctx.Err() == nil {
		cur.refs++
		entry := cur
		g.mu.Unlock()
		return entry.ctx, "", false, func() { g.leave(key, entry) }
	}
	if cur != nil {
		prevQuery = cur.query
		superseded = cur.query != norm
		prevCancel = cur.cancel
		delete(g.byClient, key)
		g.active.Add(-1)
	}
	slotCtx, cancel := context.WithCancel(context.Background())
	entry := &descriptiveFlight{query: norm, ctx: slotCtx, cancel: cancel, refs: 1}
	g.byClient[key] = entry
	g.active.Add(1)
	g.mu.Unlock()
	if prevCancel != nil {
		prevCancel()
	}
	return slotCtx, prevQuery, superseded, func() { g.leave(key, entry) }
}

func (g *descriptiveGate) leave(key descriptiveFlightKey, entry *descriptiveFlight) {
	g.mu.Lock()
	entry.refs--
	var cancel context.CancelFunc
	if entry.refs <= 0 && g.byClient[key] == entry {
		delete(g.byClient, key)
		g.active.Add(-1)
		cancel = entry.cancel
	}
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s Search) flightGate() *descriptiveGate {
	if s.gate != nil {
		return s.gate
	}
	return fallbackDescriptiveGate
}

// FamiliarFrom adapts a watch-state store to Search.Familiar; nil when the
// store cannot report it.
func FamiliarFrom(st any) func(context.Context, *core.User, []core.RatingKey) map[core.RatingKey]int {
	src, ok := st.(FamiliarSource)
	if !ok || src == nil {
		return nil
	}
	return func(ctx context.Context, viewer *core.User, keys []core.RatingKey) map[core.RatingKey]int {
		known, err := src.Familiar(ctx, viewer.ID, keys)
		if err != nil {
			return nil
		}
		return known
	}
}

// withViewer attaches the viewer's search affinity to ctx.
func (s Search) withViewer(ctx context.Context, viewer *core.User) context.Context {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if s.Familiar == nil || viewer == nil {
		return ctx
	}
	return core.WithSearchAffinity(ctx, func(ctx context.Context, keys []core.RatingKey) map[core.RatingKey]int {
		return s.Familiar(ctx, viewer, keys)
	})
}

func mergeRankedSearchTitles(titles, people []core.Title) []core.Title {
	if len(people) == 0 {
		return titles
	}
	if len(titles) == 0 {
		return people
	}
	out := make([]core.Title, 0, len(titles)+len(people))
	at := make(map[core.RatingKey]int, len(titles)+len(people))
	for _, t := range titles {
		if idx, ok := at[t.Key]; ok {
			if t.SearchScore > out[idx].SearchScore {
				out[idx] = t
			}
			continue
		}
		at[t.Key] = len(out)
		out = append(out, t)
	}
	for _, t := range people {
		if idx, ok := at[t.Key]; ok {
			if t.SearchScore > out[idx].SearchScore {
				out[idx] = t
			}
			continue
		}
		at[t.Key] = len(out)
		out = append(out, t)
	}
	slices.SortStableFunc(out, func(a, b core.Title) int {
		if c := cmp.Compare(b.SearchScore, a.SearchScore); c != 0 {
			return c
		}
		return cmp.Compare(b.Popularity, a.Popularity)
	})
	return out
}

func titleWindowCoversWeakPeople(items []core.Title, want int) bool {
	return want > 0 && len(items) >= want && items[want-1].SearchScore >= 0.50
}

func (s Search) peopleTitlesFor(ctx context.Context, viewer *core.User, query string, types []core.MediaType, want int, titleHits []core.Title, restricted bool, allowed map[core.MediaType]bool) ([]core.Title, error) {
	if s.PeopleTitles == nil {
		return nil, nil
	}
	pctx := core.WithSearchTypes(ctx, types)
	if titleWindowCoversWeakPeople(titleHits, want) {
		pctx = core.WithExactPeopleOnly(pctx)
	}
	raw, err := s.PeopleTitles(pctx, viewer, query, want)
	if err != nil || len(raw) == 0 {
		return nil, err
	}
	if !restricted {
		return s.filterCandidates(viewer, raw, allowed, len(allowed) > 0), nil
	}
	return s.visibleCandidates(ctx, viewer, raw, allowed), nil
}

// visibleCandidates checks a ranking window in one availability read. The
// playback and inherited series policy still apply to each surviving title.
func (s Search) visibleCandidates(ctx context.Context, viewer *core.User, raw []core.Title, types map[core.MediaType]bool) []core.Title {
	var eligible []core.Title
	for i := range raw {
		title := &raw[i]
		if (len(types) == 0 || types[title.Type]) && s.Visibility.TitleAllowed(viewer, title) {
			eligible = append(eligible, *title)
		}
	}
	available := s.Visibility.Available(ctx, viewer, eligible)
	if s.PrefetchParents != nil {
		parents := make([]core.RatingKey, 0, len(available))
		for _, title := range available {
			if title.ShowKey != 0 {
				parents = append(parents, title.ShowKey)
			}
		}
		if len(parents) > 0 {
			s.PrefetchParents(ctx, parents)
		}
	}
	return slices.DeleteFunc(available, func(title core.Title) bool {
		return title.ShowKey != 0 && !s.Visibility.Playable(ctx, viewer, &title)
	})
}

// Find is an explicit search: catalogue rules the owner limited to browsing
// do not apply, so those titles can still be found.
func (s Search) Find(ctx context.Context, viewer *core.User, query string, types []core.MediaType, limit int) ([]core.Title, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if strings.TrimSpace(query) == "" || len(types) == 0 {
		return nil, nil
	}
	ctx = s.withViewer(core.WithExplicitSearch(ctx), viewer)
	allowed := make(map[core.MediaType]bool, len(types))
	for _, typ := range types {
		allowed[typ] = true
	}
	want := limit
	if want <= 0 {
		want = 20
	}
	if s.Unrestricted != nil && s.Unrestricted(viewer) {
		var items []core.Title
		var natural bool
		if s.Natural != nil {
			items, natural = s.Natural(fullSearch(ctx), viewer, query, limit)
		}
		if !natural {
			var err error
			items, err = s.Catalog(ctx, query, types, limit)
			if err != nil {
				return nil, err
			}
		}
		filtered := s.filterCandidates(viewer, items, allowed, natural)
		if !natural && s.PeopleTitles != nil {
			pItems, pErr := s.peopleTitlesFor(ctx, viewer, query, types, want, filtered, false, allowed)
			if pErr != nil && len(filtered) == 0 {
				return nil, pErr
			}
			filtered = mergeRankedSearchTitles(filtered, pItems)
			if limit > 0 && len(filtered) > limit {
				filtered = filtered[:limit]
			}
		}
		return filtered, nil
	}
	// The catalogue and natural-search APIs limit candidates, not permitted
	// results. Expand only when policy removed candidates and the source may
	// have more; cap work for sources which never exhaust.
	fetch := want
	for {
		var items []core.Title
		var ok bool
		if s.Natural != nil {
			items, ok = s.Natural(fullSearch(ctx), viewer, query, fetch)
		}
		if !ok {
			var err error
			items, err = s.Catalog(ctx, query, types, fetch)
			if err != nil {
				return nil, err
			}
		}
		filtered := s.visibleCandidates(ctx, viewer, items, allowed)
		if !ok && s.PeopleTitles != nil {
			pItems, pErr := s.peopleTitlesFor(ctx, viewer, query, types, fetch, filtered, true, allowed)
			if pErr != nil && len(filtered) == 0 {
				return nil, pErr
			}
			filtered = mergeRankedSearchTitles(filtered, pItems)
		}
		if len(filtered) >= want {
			return filtered[:want], nil
		}
		if len(items) < fetch || fetch >= 1000 || ctx.Err() != nil {
			return filtered, nil
		}
		fetch = min(fetch*2, 1000)
	}
}

// fullSearch is ctx for a search that keeps its results: natural search
// caches what it resolves, so it never reads a candidate window.
func fullSearch(ctx context.Context) context.Context {
	if core.SearchCandidatesFrom(ctx) == nil {
		return ctx
	}
	return core.WithSearchCandidates(ctx, nil)
}

// TitleMatchScore is the catalogue relevance (core.Title.SearchScore) from
// which a keyword result is the title the viewer typed, give or take an
// article: such a title outranks a collection reading of the same words
// ("1917" is the film, not the films of 1917).
const TitleMatchScore = 0.96

// PersonMatchScore is the relevance floor of an exact or near-exact full-name
// person match in PeopleTitles: a viewer typing "brad pitt" gets that person's
// filmography immediately rather than waiting on descriptive AI search.
const PersonMatchScore = 0.935

// HasTitleMatch reports whether keyword results contain a title matching the
// query at or above TitleMatchScore.
func HasTitleMatch(keyword []core.Title) bool {
	return slices.ContainsFunc(keyword, func(t core.Title) bool {
		return t.SearchScore >= TitleMatchScore
	})
}

// HasPersonOrTitleMatch reports whether keyword results contain an exact or
// near-exact title or person match.
func HasPersonOrTitleMatch(keyword []core.Title) bool {
	return slices.ContainsFunc(keyword, func(t core.Title) bool {
		return t.SearchScore >= PersonMatchScore
	})
}

// IsDescriptiveQuery classifies a search from its already-computed keyword
// results: any query of two or more words without a strong title or full-name
// person match is descriptive when AI descriptive search is configured.
func IsDescriptiveQuery(query string, keyword []core.Title, aiAvailable bool) bool {
	if !aiAvailable || core.IsShortSearch(query) || HasPersonOrTitleMatch(keyword) {
		return false
	}
	return len(strings.Fields(query)) >= 2
}

// Resolve runs keyword search first to classify title vs descriptive queries.
// Title searches return immediately without starting AI work; descriptive
// queries wait up to DescriptiveSearchWaitBound and lead with AI items.
func (s Search) Resolve(ctx context.Context, viewer *core.User, query string, types []core.MediaType, limit int, collections bool) (SearchResults, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if ds, ok := s.Vibe.Source.(core.DescriptiveSearcher); ok {
		keyword, err := s.Find(ctx, viewer, query, types, limit)
		if err != nil {
			return SearchResults{}, err
		}
		if !IsDescriptiveQuery(query, keyword, ds.DescriptiveSearchAvailable()) {
			if prev, superseded := s.flightGate().supersede(viewer, core.SearchClientFrom(ctx), query); superseded {
				if ctrl, ok := s.Vibe.Source.(core.DescriptiveSearchController); ok {
					ctrl.CancelDescribeSearch(viewer, prev)
				}
			}
			return SearchResults{Items: keyword}, nil
		}
		collection, aiItems, handled, err := s.resolveDescriptive(ctx, viewer, query, types, collections)
		if err != nil {
			return SearchResults{}, err
		}
		if !handled {
			return SearchResults{Items: keyword}, nil
		}
		items := mergeAIAndKeyword(aiItems, keyword)
		return SearchResults{
			Collection:       collection,
			Items:            items,
			CollectionSearch: true,
			AIItems:          len(aiItems),
		}, nil
	}

	// Non-AI VibeSource path (static/test collection sources).
	if collections {
		collection, items, handled, err := s.Vibe.Find(ctx, viewer, query)
		if err != nil {
			return SearchResults{}, err
		}
		if handled {
			keyword, err := s.Find(ctx, viewer, query, types, limit)
			if err == nil && HasTitleMatch(keyword) {
				items = keyword
			}
			return SearchResults{Collection: collection, Items: items, CollectionSearch: true}, nil
		}
	}
	items, err := s.Find(ctx, viewer, query, types, limit)
	if err == nil && len(items) == 0 && !collections {
		collection, members, handled, vibeErr := s.Vibe.Find(ctx, viewer, query)
		if vibeErr != nil {
			return SearchResults{}, vibeErr
		}
		if handled {
			return SearchResults{Collection: collection, Items: members, CollectionSearch: true}, nil
		}
	}
	return SearchResults{Items: items}, err
}

// Keyword preserves the catalogue's candidate window and applies title policy.
// For descriptive queries when AI is configured, it waits with the same bound
// and returns AI items matching types ahead of keyword hits.
func (s Search) Keyword(ctx context.Context, viewer *core.User, query string, types []core.MediaType, limit int) ([]core.Title, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	searchCtx := s.withViewer(core.WithExplicitSearch(ctx), viewer)
	items, err := s.Catalog(searchCtx, query, types, limit)
	if err != nil {
		return nil, err
	}
	keyword := s.filterCandidates(viewer, items, nil, false)
	if s.PeopleTitles != nil {
		want := limit
		if want <= 0 {
			want = 20
		}
		var allowed map[core.MediaType]bool
		if len(types) > 0 {
			allowed = make(map[core.MediaType]bool, len(types))
			for _, t := range types {
				allowed[t] = true
			}
		}
		pItems, pErr := s.peopleTitlesFor(searchCtx, viewer, query, types, want, keyword, false, allowed)
		if pErr != nil && len(keyword) == 0 {
			return nil, pErr
		}
		keyword = mergeRankedSearchTitles(keyword, pItems)
		if limit > 0 && len(keyword) > limit {
			keyword = keyword[:limit]
		}
	}
	ds, ok := s.Vibe.Source.(core.DescriptiveSearcher)
	if !ok {
		return keyword, nil
	}
	if !IsDescriptiveQuery(query, keyword, ds.DescriptiveSearchAvailable()) {
		if prev, superseded := s.flightGate().supersede(viewer, core.SearchClientFrom(ctx), query); superseded {
			if ctrl, ok := s.Vibe.Source.(core.DescriptiveSearchController); ok {
				ctrl.CancelDescribeSearch(viewer, prev)
			}
		}
		return keyword, nil
	}
	_, aiItems, handled, err := s.resolveDescriptive(ctx, viewer, query, types, false)
	if err != nil {
		return nil, err
	}
	if !handled || len(aiItems) == 0 {
		return keyword, nil
	}
	merged := mergeAIAndKeyword(aiItems, keyword)
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

func (s Search) resolveDescriptive(ctx context.Context, viewer *core.User, query string, types []core.MediaType, wantCollection bool) (*core.Collection, []core.Title, bool, error) {
	ds, ok := s.Vibe.Source.(core.DescriptiveSearcher)
	if !ok || !ds.DescriptiveSearchAvailable() {
		return nil, nil, false, nil
	}
	ctrl, _ := s.Vibe.Source.(core.DescriptiveSearchController)
	client := core.SearchClientFrom(ctx)
	slotCtx, prevQuery, superseded, release := s.flightGate().enter(viewer, client, query)
	defer release()
	if superseded && ctrl != nil {
		ctrl.CancelDescribeSearch(viewer, prevQuery)
	}

	if ctrl == nil || !ctrl.DescribeCached(viewer, query) {
		settle := s.SettleDelay
		if settle == 0 {
			settle = DescriptiveSearchSettleDelay
		}
		if settle > 0 {
			timer := time.NewTimer(settle)
			select {
			case <-slotCtx.Done():
				timer.Stop()
				return nil, nil, false, nil
			case <-ctx.Done():
				timer.Stop()
				return nil, nil, false, ctx.Err()
			case <-timer.C:
			}
		}
	}
	if slotCtx.Err() != nil {
		return nil, nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, false, err
	}

	bound := s.WaitBound
	if bound <= 0 {
		bound = DescriptiveSearchWaitBound
	}
	baseCtx := core.WithCatalogViewer(fullSearch(ctx), viewer)
	waitCtx, cancelWait := context.WithTimeout(baseCtx, bound)
	stop := context.AfterFunc(slotCtx, cancelWait)
	result, _ := ds.DescribeSearch(waitCtx, viewer, query, true)
	stop()
	waitExpired := waitCtx.Err() != nil
	cancelWait()

	if slotCtx.Err() != nil {
		if ctrl != nil {
			ctrl.CancelDescribeSearch(viewer, query)
		}
		return nil, nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, false, err
	}
	if waitExpired {
		// Wait bound passed: read any partial (or just-completed) items without
		// waiting; the background job finishes and caches for the next query.
		result, _ = ds.DescribeSearch(baseCtx, viewer, query, false)
	}

	allowed := make(map[core.MediaType]bool, len(types))
	for _, typ := range types {
		allowed[typ] = true
		if typ == core.TypeShow {
			allowed[core.TypeEpisode] = true
		}
	}
	filterTypes := func(in []core.Title) []core.Title {
		if len(allowed) == 0 {
			return in
		}
		out := make([]core.Title, 0, len(in))
		for _, t := range in {
			if allowed[t.Type] {
				out = append(out, t)
			}
		}
		return out
	}

	switch result.State {
	case "ready":
		if len(result.Items) == 0 {
			return nil, nil, false, nil
		}
		collection, items, handled, err := s.Vibe.Find(baseCtx, viewer, query)
		if err != nil {
			return nil, nil, false, nil
		}
		if handled {
			items = filterTypes(items)
			if !wantCollection || (len(allowed) > 0 && collection != nil && !allowed[collection.Type] && len(items) == 0) {
				collection = nil
			} else if collection != nil && len(items) > 0 && !allowed[collection.Type] {
				collection.Type = items[0].Type
			}
			if len(items) == 0 && collection == nil {
				return nil, nil, false, nil
			}
			return collection, items, true, nil
		}
		items = filterTypes(s.Vibe.filterItems(baseCtx, viewer, result.Items))
		if len(items) == 0 {
			return nil, nil, false, nil
		}
		return nil, items, true, nil
	case "partial":
		if len(result.Items) == 0 {
			return nil, nil, false, nil
		}
		items := filterTypes(s.Vibe.filterItems(baseCtx, viewer, result.Items))
		if len(items) == 0 {
			return nil, nil, false, nil
		}
		return nil, items, true, nil
	default:
		return nil, nil, false, nil
	}
}

func mergeAIAndKeyword(ai, keyword []core.Title) []core.Title {
	if len(ai) == 0 {
		return keyword
	}
	if len(keyword) == 0 {
		return ai
	}
	seen := make(map[core.RatingKey]bool, len(ai))
	out := make([]core.Title, 0, len(ai)+len(keyword))
	for _, t := range ai {
		if !seen[t.Key] {
			seen[t.Key] = true
			out = append(out, t)
		}
	}
	for _, t := range keyword {
		if !seen[t.Key] {
			seen[t.Key] = true
			out = append(out, t)
		}
	}
	return out
}

func (s Search) filterCandidates(viewer *core.User, items []core.Title, allowed map[core.MediaType]bool, filterTypes bool) []core.Title {
	filtered := make([]core.Title, 0, len(items))
	for i := range items {
		if s.Visibility.TitleAllowed(viewer, &items[i]) && (!filterTypes || allowed[items[i].Type]) {
			filtered = append(filtered, items[i])
		}
	}
	return filtered
}

type SearchResults = frontend.SearchResults
type FamiliarSource = frontend.FamiliarSource
