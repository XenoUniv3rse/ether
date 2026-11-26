package experience

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const (
	defaultListLimit = 50
	maxListLimit     = 500
)

// FranchiseStore answers page-first TMDB franchise listing, set-based
// visibility counting, metadata loading and fallback artwork from the indexed
// catalog_franchises summary table so collection listings never walk every
// franchise or hydrate titles and credits per candidate.
type FranchiseStore interface {
	FranchiseTotal(ctx context.Context) (int, error)
	FranchisesPage(ctx context.Context, offset, limit int) ([]core.Collection, error)
	FranchisesCursor(ctx context.Context, afterCount, afterID, limit int) ([]core.Collection, error)
	FranchisesSearch(ctx context.Context, query string, afterCount, afterID, limit int) ([]core.Collection, error)
	FranchiseVisibleCounts(ctx context.Context, f core.Filter, collectionIDs []int) (map[int]int, error)
	FranchiseVisibleKeys(ctx context.Context, f core.Filter, collectionIDs []int) (map[int][]core.RatingKey, error)
	FranchisesInSmallFilter(ctx context.Context, f core.Filter, query string, offset, limit int) (items []core.Collection, hasMore, usedDirect bool, err error)
	FranchiseSummaryBatch(ctx context.Context, collectionIDs []int) (map[int]core.Collection, error)
	FranchiseFallbackArt(ctx context.Context, f core.Filter, collectionIDs []int) (map[int][2]string, error)
	HasActiveViewerRules(viewerID int64) bool
}

type ownedCompleteLister interface {
	ListOwnedComplete(context.Context, *core.User, core.MediaType) ([]core.Collection, bool, error)
}

func normalizeListLimit(limit int) int {
	if limit <= 0 {
		return defaultListLimit
	}
	if limit > maxListLimit {
		return maxListLimit
	}
	return limit
}

func sliceBounds(total, offset, limit int) (int, int) {
	limit = normalizeListLimit(limit)
	start := min(max(offset, 0), total)
	end := total
	if limit > 0 && limit < total-start {
		end = start + limit
	}
	return start, end
}

// Collections filters member visibility before calculating totals and paging.
func (s *Service) Collections(ctx context.Context, viewer *core.Viewer, _ core.Device, q frontend.CollectionQuery) (frontend.CollectionPage, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.CollectionPage{}, err
	}
	if s.d.Collections == nil {
		return frontend.CollectionPage{}, core.ErrNotAvailable
	}
	if q.ViewID != "" {
		return s.viewCollections(ctx, v, q)
	}
	requested := map[core.MediaType]bool{}
	for _, typ := range q.Types {
		if typ != core.TypeMovie && typ != core.TypeShow {
			return frontend.CollectionPage{}, core.ErrNotFound
		}
		requested[typ] = true
	}
	published := map[core.MediaType]bool{}
	if s.d.PluginLibraries != nil {
		views, e := s.storedViews(ctx, v)
		if e != nil {
			return frontend.CollectionPage{}, e
		}
		for _, view := range views {
			if view.KindKey == "" {
				published[view.Type] = true
			}
		}
	}

	var allowedTypes []core.MediaType
	seenTypes := map[core.MediaType]bool{}
	for _, lib := range s.libraries() {
		typ := lib.Type
		if (s.d.PluginLibraries != nil && !published[typ]) || (typ != core.TypeMovie && typ != core.TypeShow) || !SectionAllowed(v, lib.ID) || (len(requested) > 0 && !requested[typ]) || seenTypes[typ] {
			continue
		}
		seenTypes[typ] = true
		allowedTypes = append(allowedTypes, typ)
	}

	if page, ok, err := s.collectionsPageFirst(ctx, v, allowedTypes, q); err != nil {
		return frontend.CollectionPage{}, err
	} else if ok {
		return page, nil
	}

	complete, ok := s.d.Collections.(interface {
		ListComplete(context.Context, *core.User, core.MediaType) ([]core.Collection, error)
	})
	if !ok {
		return frontend.CollectionPage{}, core.ErrNotAvailable
	}
	seenKeys := map[core.RatingKey]bool{}
	var visible []core.Collection
	for _, typ := range allowedTypes {
		if q.ExistsOnly {
			finder, ok := s.d.Collections.(interface {
				FindVisible(context.Context, *core.User, core.MediaType, func(core.Collection) (bool, error)) (bool, error)
			})
			if !ok {
				return frontend.CollectionPage{}, core.ErrNotAvailable
			}
			found, e := finder.FindVisible(ctx, v, typ, func(c core.Collection) (bool, error) {
				if c.Type != typ || !s.d.Visibility.CollectionAllowed(v, &c) || (c.OwnerUserID != 0 && c.OwnerUserID != v.ID) {
					return false, nil
				}
				return s.collectionHasVisibleMember(ctx, v, c)
			})
			if e != nil {
				return frontend.CollectionPage{}, e
			}
			if found {
				return frontend.CollectionPage{Total: 1}, nil
			}
			continue
		}
		collections, e := complete.ListComplete(ctx, v, typ)
		if e != nil {
			return frontend.CollectionPage{}, e
		}
		for _, c := range collections {
			if (q.Query != "" && !strings.Contains(strings.ToLower(c.Title), strings.ToLower(q.Query))) || c.Type != typ || seenKeys[c.Key] || !s.d.Visibility.CollectionAllowed(v, &c) || (c.OwnerUserID != 0 && c.OwnerUserID != v.ID) {
				continue
			}
			members, e := s.collectionItemsFor(ctx, v, c.Key, c.Upcoming)
			if e != nil {
				return frontend.CollectionPage{}, e
			}
			if len(members) == 0 {
				continue
			}
			c.ChildCount = len(members)
			seenKeys[c.Key] = true
			visible = append(visible, c)
		}
	}
	start, end := sliceBounds(len(visible), q.Offset, q.Limit)
	return frontend.CollectionPage{Items: visible[start:end], Total: len(visible)}, nil
}

func (s *Service) viewCollections(ctx context.Context, v *core.User, q frontend.CollectionQuery) (frontend.CollectionPage, error) {
	id, err := strconv.Atoi(q.ViewID)
	if err != nil {
		return frontend.CollectionPage{}, core.ErrNotFound
	}
	var typ core.MediaType
	var stored *core.Library
	if s.d.PluginLibraries != nil {
		lib, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(id))
		if err != nil {
			return frontend.CollectionPage{}, core.ErrNotFound
		}
		stored = &lib
		mt := filterMediaType(lib)
		if lib.Source != core.LibraryCollection {
			var ok bool
			mt, _, ok = s.publishedLibrary(ctx, lib)
			if !ok {
				return frontend.CollectionPage{}, core.ErrNotFound
			}
		} else if lib.Hidden {
			return frontend.CollectionPage{}, core.ErrNotFound
		}
		if !v.AllowsLibrary(lib.ID) {
			return frontend.CollectionPage{}, core.ErrNotFound
		}
		if lib.Source == core.LibraryPlugin {
			return frontend.CollectionPage{Items: []core.Collection{}, Total: 0}, nil
		}
		typ = mt
	} else {
		var found bool
		for _, lib := range s.libraries() {
			if lib.ID == id {
				typ = lib.Type
				found = true
				break
			}
		}
		if !found || !SectionAllowed(v, id) {
			return frontend.CollectionPage{}, core.ErrNotFound
		}
	}
	if typ != core.TypeMovie && typ != core.TypeShow {
		return frontend.CollectionPage{Items: []core.Collection{}, Total: 0}, nil
	}
	for _, r := range q.Types {
		if r != core.TypeMovie && r != core.TypeShow {
			return frontend.CollectionPage{}, core.ErrNotFound
		}
	}
	if len(q.Types) > 0 {
		match := false
		for _, r := range q.Types {
			if r == typ {
				match = true
				break
			}
		}
		if !match {
			return frontend.CollectionPage{Items: []core.Collection{}, Total: 0}, nil
		}
	}

	if page, ok, err := s.viewCollectionsPageFirst(ctx, v, typ, stored, q); err != nil {
		return frontend.CollectionPage{}, err
	} else if ok {
		return page, nil
	}

	complete, ok := s.d.Collections.(interface {
		ListComplete(context.Context, *core.User, core.MediaType) ([]core.Collection, error)
	})
	if !ok {
		return frontend.CollectionPage{}, core.ErrNotAvailable
	}
	collections, err := complete.ListComplete(ctx, v, typ)
	if err != nil {
		return frontend.CollectionPage{}, err
	}
	type candidate struct {
		col     core.Collection
		members []core.Title
	}
	var candidates []candidate
	var allKeys []core.RatingKey
	seenMemberKey := map[core.RatingKey]bool{}
	seenColKey := map[core.RatingKey]bool{}
	for _, c := range collections {
		if (q.Query != "" && !strings.Contains(strings.ToLower(c.Title), strings.ToLower(q.Query))) || c.Type != typ || seenColKey[c.Key] || !s.d.Visibility.CollectionAllowed(v, &c) || (c.OwnerUserID != 0 && c.OwnerUserID != v.ID) {
			continue
		}
		members, err := s.collectionItemsFor(ctx, v, c.Key, c.Upcoming)
		if err != nil {
			return frontend.CollectionPage{}, err
		}
		if len(members) == 0 {
			continue
		}
		seenColKey[c.Key] = true
		candidates = append(candidates, candidate{col: c, members: members})
		for _, m := range members {
			if !seenMemberKey[m.Key] {
				seenMemberKey[m.Key] = true
				allKeys = append(allKeys, m.Key)
			}
		}
	}
	var allowedInLib map[core.RatingKey]bool
	scopedToLib := false
	if stored != nil && stored.ID != core.LibraryMovies && stored.ID != core.LibraryShows && len(allKeys) > 0 {
		if s.d.LibraryMembers != nil {
			var ok bool
			allowedInLib, _, ok, err = s.d.LibraryMembers(ctx, stored.ID, allKeys)
			if err != nil {
				return frontend.CollectionPage{}, err
			}
			scopedToLib = ok
		}
	}
	var visible []core.Collection
	for _, cand := range candidates {
		count := 0
		for _, m := range cand.members {
			if m.Type != typ {
				continue
			}
			if scopedToLib && !allowedInLib[m.Key] {
				continue
			}
			count++
		}
		if count == 0 {
			continue
		}
		c := cand.col
		c.ChildCount = count
		visible = append(visible, c)
		if q.ExistsOnly {
			return frontend.CollectionPage{Total: 1}, nil
		}
	}
	if q.ExistsOnly {
		return frontend.CollectionPage{Total: 0}, nil
	}
	start, end := sliceBounds(len(visible), q.Offset, q.Limit)
	return frontend.CollectionPage{Items: visible[start:end], Total: len(visible)}, nil
}

// collectionsPageFirst serves Collections without ViewID by evaluating only
// the small set of owner/built-in/ephemeral collections and paging TMDB
// franchises directly from catalog_franchises with set-based visibility checks.
func (s *Service) collectionsPageFirst(ctx context.Context, v *core.User, allowedTypes []core.MediaType, q frontend.CollectionQuery) (frontend.CollectionPage, bool, error) {
	if s.d.Franchises == nil {
		return frontend.CollectionPage{}, false, nil
	}
	seen := make(map[core.RatingKey]bool)
	var ownedVisible []core.Collection
	includeFranchises := false
	for _, typ := range allowedTypes {
		hasFr, err := s.visitVisibleOwned(ctx, v, typ, nil, q, seen, &ownedVisible)
		if errors.Is(err, core.ErrNotAvailable) {
			return frontend.CollectionPage{}, false, nil
		}
		if err != nil {
			return frontend.CollectionPage{}, true, err
		}
		if q.ExistsOnly && len(ownedVisible) > 0 {
			return frontend.CollectionPage{Total: 1}, true, nil
		}
		if q.NoTotal && len(ownedVisible)-max(q.Offset, 0) >= normalizeListLimit(q.Limit) {
			start, end := sliceBounds(len(ownedVisible), q.Offset, q.Limit)
			return frontend.CollectionPage{Items: ownedVisible[start:end], Total: len(ownedVisible)}, true, nil
		}
		includeFranchises = includeFranchises || hasFr && typ == core.TypeMovie
	}
	if !includeFranchises {
		if q.ExistsOnly {
			return frontend.CollectionPage{Total: 0}, true, nil
		}
		start, end := sliceBounds(len(ownedVisible), q.Offset, q.Limit)
		return frontend.CollectionPage{Items: ownedVisible[start:end], Total: len(ownedVisible)}, true, nil
	}
	return s.combineOwnedAndFranchises(ctx, v, nil, ownedVisible, q)
}

// viewCollectionsPageFirst serves Collections scoped to a single library view
// (e.g. Vela GET /vela/v0/libraries/:id/collections?limit=100) without walking
// every franchise or hydrating titles and credits per candidate.
func (s *Service) viewCollectionsPageFirst(ctx context.Context, v *core.User, typ core.MediaType, stored *core.Library, q frontend.CollectionQuery) (frontend.CollectionPage, bool, error) {
	if s.d.Franchises == nil {
		return frontend.CollectionPage{}, false, nil
	}
	franchises, canSeekFranchises := s.d.Collections.(interface{ IncludesFranchises(core.MediaType) bool })
	if q.NoTotal && !q.ExistsOnly && typ == core.TypeMovie && q.After.Kind() == core.KindCollection &&
		canSeekFranchises && franchises.IncludesFranchises(typ) {
		items, total, fits, err := s.pageFranchises(ctx, v, stored, q.Query, 0, normalizeListLimit(q.Limit), false, true, q.After)
		if err == nil && fits {
			return frontend.CollectionPage{Items: items, Total: q.Offset + total}, true, nil
		}
		if err != nil && !errors.Is(err, core.ErrNotAvailable) {
			return frontend.CollectionPage{}, true, err
		}
	}
	var ownedVisible []core.Collection
	if q.NoTotal && !q.ExistsOnly && q.After.Kind() == core.KindSmart && !q.After.IsEphemeral() {
		seek := q
		seek.Offset = 0
		found, err := s.visitVisibleOwned(ctx, v, typ, stored, seek, make(map[core.RatingKey]bool), &ownedVisible)
		if err == nil {
			if !found || typ != core.TypeMovie {
				return frontend.CollectionPage{Items: ownedVisible[:min(len(ownedVisible), normalizeListLimit(q.Limit))], Total: q.Offset + len(ownedVisible)}, true, nil
			}
			page, handled, err := s.combineOwnedAndFranchises(ctx, v, stored, ownedVisible, seek)
			page.Total += q.Offset
			return page, handled, err
		}
		if !errors.Is(err, core.ErrNotAvailable) {
			return frontend.CollectionPage{}, true, err
		}
		ownedVisible = nil
	}
	q.After = 0
	includeFranchises, err := s.visitVisibleOwned(ctx, v, typ, stored, q, make(map[core.RatingKey]bool), &ownedVisible)
	if errors.Is(err, core.ErrNotAvailable) {
		return frontend.CollectionPage{}, false, nil
	}
	if err != nil {
		return frontend.CollectionPage{}, true, err
	}
	if q.ExistsOnly && len(ownedVisible) > 0 {
		return frontend.CollectionPage{Total: 1}, true, nil
	}
	if !includeFranchises || typ != core.TypeMovie {
		if q.ExistsOnly {
			return frontend.CollectionPage{}, true, nil
		}
		start, end := sliceBounds(len(ownedVisible), q.Offset, q.Limit)
		return frontend.CollectionPage{Items: ownedVisible[start:end], Total: len(ownedVisible)}, true, nil
	}
	return s.combineOwnedAndFranchises(ctx, v, stored, ownedVisible, q)
}

func (s *Service) visitVisibleOwned(ctx context.Context, v *core.User, typ core.MediaType, stored *core.Library, q frontend.CollectionQuery, seen map[core.RatingKey]bool, ownedVisible *[]core.Collection) (bool, error) {
	counter, err := s.ownedMemberBatchCounter(ctx, v, stored, typ)
	if err != nil {
		return false, err
	}
	visit := func(owned []core.Collection) (bool, error) {
		visible, err := s.visibleOwnedBatch(ctx, v, typ, stored, q, counter, owned, seen)
		if err != nil {
			return false, err
		}
		*ownedVisible = append(*ownedVisible, visible...)
		if q.ExistsOnly {
			return len(*ownedVisible) > 0, nil
		}
		return q.NoTotal && len(*ownedVisible)-max(q.Offset, 0) >= normalizeListLimit(q.Limit), nil
	}

	var includeFranchises bool
	if visitor, ok := s.d.Collections.(interface {
		VisitOwned(context.Context, *core.User, core.MediaType, string, int, core.RatingKey, func([]core.Collection) (bool, error)) (bool, error)
	}); ok {
		includeFranchises, err = visitor.VisitOwned(ctx, v, typ, strings.ToLower(strings.TrimSpace(q.Query)), min(normalizeListLimit(q.Limit), 200), q.After, visit)
	} else {
		err = core.ErrNotAvailable
	}
	if errors.Is(err, core.ErrNotAvailable) {
		if q.After != 0 {
			return false, err
		}
		lister, ok := s.d.Collections.(ownedCompleteLister)
		if !ok {
			return false, core.ErrNotAvailable
		}
		var owned []core.Collection
		owned, includeFranchises, err = lister.ListOwnedComplete(ctx, v, typ)
		if errors.Is(err, core.ErrNotAvailable) {
			return false, core.ErrNotAvailable
		}
		if err == nil {
			_, err = visit(owned)
		}
	}
	if err != nil {
		return false, err
	}
	return includeFranchises, nil
}

type ownedCountBatch func([]core.Collection) (map[int64]int, error)

func (s *Service) visibleOwnedBatch(ctx context.Context, v *core.User, typ core.MediaType, stored *core.Library, q frontend.CollectionQuery, counter ownedCountBatch, owned []core.Collection, seenColKey map[core.RatingKey]bool) ([]core.Collection, error) {
	lowerQuery := strings.ToLower(strings.TrimSpace(q.Query))
	type candidate struct {
		col     core.Collection
		members []core.Title
		counted bool // ChildCount already holds the visible member count
	}
	var candidates []candidate
	var allKeys []core.RatingKey
	seenMemberKey := map[core.RatingKey]bool{}
	eligible := make([]core.Collection, 0, len(owned))
	for _, c := range owned {
		if (lowerQuery != "" && !strings.Contains(strings.ToLower(c.Title), lowerQuery)) || c.Type != typ || seenColKey[c.Key] || !s.d.Visibility.CollectionAllowed(v, &c) || (c.OwnerUserID != 0 && c.OwnerUserID != v.ID) {
			continue
		}
		eligible = append(eligible, c)
	}
	var counts map[int64]int
	if counter != nil {
		var err error
		counts, err = counter(eligible)
		if err != nil {
			return nil, err
		}
	}
	prober, canAnswer := s.d.Collections.(visibleProber)
	probeInLibrary := stored == nil || stored.ID == core.LibraryMovies || stored.ID == core.LibraryShows
	var unchecked, needScopedItems []core.Collection
	for _, c := range eligible {
		if counts[ownedCountID(c)] > 0 {
			continue
		}
		if probeInLibrary || canAnswer && prober.AnswersVisible(c.Key) {
			unchecked = append(unchecked, c)
		} else {
			needScopedItems = append(needScopedItems, c)
		}
	}
	probes, err := s.collectionVisibleProbes(ctx, v, unchecked)
	if err != nil {
		return nil, err
	}
	scopedItems, err := s.collectionItemsBatch(ctx, v, needScopedItems)
	if err != nil {
		return nil, err
	}
	for _, c := range eligible {
		if seenColKey[c.Key] {
			continue
		}
		if n := counts[ownedCountID(c)]; n > 0 {
			seenColKey[c.Key] = true
			c.ChildCount = n
			candidates = append(candidates, candidate{col: c, counted: true})
			continue
		}
		if probe, ok := probes[c.Key]; ok {
			if !probe.has {
				continue
			}
			seenColKey[c.Key] = true
			c.ChildCount = probe.total
			candidates = append(candidates, candidate{col: c, counted: true})
			continue
		}
		members := scopedItems[c.Key]
		if len(members) == 0 {
			continue
		}
		seenColKey[c.Key] = true
		candidates = append(candidates, candidate{col: c, members: members})
		for _, m := range members {
			if !seenMemberKey[m.Key] {
				seenMemberKey[m.Key] = true
				allKeys = append(allKeys, m.Key)
			}
		}
	}
	var allowedInLib map[core.RatingKey]bool
	scopedToLib := false
	if stored != nil && stored.ID != core.LibraryMovies && stored.ID != core.LibraryShows && len(allKeys) > 0 && s.d.LibraryMembers != nil {
		var ok bool
		var err error
		allowedInLib, _, ok, err = s.d.LibraryMembers(ctx, stored.ID, allKeys)
		if err != nil {
			return nil, err
		}
		scopedToLib = ok
	}
	var ownedVisible []core.Collection
	for _, cand := range candidates {
		if cand.counted {
			ownedVisible = append(ownedVisible, cand.col)
			if q.ExistsOnly {
				return ownedVisible, nil
			}
			continue
		}
		count := 0
		for _, m := range cand.members {
			if m.Type != typ {
				continue
			}
			if scopedToLib && !allowedInLib[m.Key] {
				continue
			}
			count++
		}
		if count == 0 {
			continue
		}
		c := cand.col
		c.ChildCount = count
		ownedVisible = append(ownedVisible, c)
		if q.ExistsOnly {
			return ownedVisible, nil
		}
	}
	return ownedVisible, nil
}

func ownedCountID(c core.Collection) int64 {
	key, _ := c.Key.CollectionSection()
	if c.Upcoming || c.Key.IsEphemeral() || key.Kind() != core.KindSmart {
		return 0
	}
	return key.TMDB()
}

func (s *Service) ownedMemberBatchCounter(ctx context.Context, v *core.User, stored *core.Library, typ core.MediaType) (ownedCountBatch, error) {
	f, fits, err := s.ownedMemberFilter(ctx, v, stored, typ)
	if err != nil || !fits {
		return nil, err
	}
	return func(cols []core.Collection) (map[int64]int, error) {
		if s.d.CollectionMemberCounts == nil {
			counts := make(map[int64]int)
			for _, c := range cols {
				if id := ownedCountID(c); id != 0 {
					n, err := s.countOwnedMembers(ctx, f, id)
					if err != nil {
						return nil, err
					}
					counts[id] = n
				}
			}
			return counts, nil
		}
		var ids []int64
		for _, c := range cols {
			if id := ownedCountID(c); id != 0 {
				ids = append(ids, id)
			}
		}
		return s.d.CollectionMemberCounts(ctx, f, ids)
	}, nil
}

// ownedMemberCounter returns a function counting a smart collection's shared
// members that v may see in stored (or the built-in library of typ), as one
// indexed statement on collections_members. It returns nil when the viewer's
// exclusions cannot be expressed in SQL; the counter answers 0 for anything it
// cannot count (ephemeral, upcoming or non-smart collections).
func (s *Service) ownedMemberCounter(ctx context.Context, v *core.User, stored *core.Library, typ core.MediaType) (func(core.Collection) (int, error), error) {
	f, fits, err := s.ownedMemberFilter(ctx, v, stored, typ)
	if err != nil || !fits {
		return nil, err
	}
	return func(c core.Collection) (int, error) {
		id := ownedCountID(c)
		if id == 0 {
			return 0, nil
		}
		return s.countOwnedMembers(ctx, f, id)
	}, nil
}

func (s *Service) ownedMemberFilter(ctx context.Context, v *core.User, stored *core.Library, typ core.MediaType) (core.Filter, bool, error) {
	if s.d.Browse == nil {
		return core.Filter{}, false, nil
	}
	base := core.Filter{Type: typ, LocalOnly: true}
	if stored != nil && stored.ID != core.LibraryMovies && stored.ID != core.LibraryShows {
		base = s.libraryBrowseFilter(ctx, v, *stored, base)
		if base.CollectionMembers != 0 {
			return core.Filter{}, false, nil // a collection library keeps the exact path
		}
	}
	var ex LibraryPageExclusions
	if s.d.PageExclusions != nil {
		ex = s.d.PageExclusions(ctx, v, typ)
		if ex.Err != nil {
			return core.Filter{}, false, ex.Err
		}
	}
	f, fits, none := viewerBrowseFilter(base, v, ex, typ)
	if !fits || none {
		return core.Filter{}, false, nil
	}
	return f, true, nil
}

func (s *Service) countOwnedMembers(ctx context.Context, f core.Filter, id int64) (int, error) {
	f.CollectionMembers = id
	page, err := s.d.Browse(ctx, core.BrowseQuery{Filter: f, Limit: 1})
	return page.Total, err
}

func (s *Service) combineOwnedAndFranchises(ctx context.Context, v *core.User, stored *core.Library, ownedVisible []core.Collection, q frontend.CollectionQuery) (frontend.CollectionPage, bool, error) {
	offset := max(q.Offset, 0)
	limit := normalizeListLimit(q.Limit)
	ownedN := len(ownedVisible)
	var out []core.Collection
	if !q.ExistsOnly && offset < ownedN {
		end := min(offset+limit, ownedN)
		out = append(out, ownedVisible[offset:end]...)
	}
	frOffset := max(0, offset-ownedN)
	frNeed := limit - len(out)
	if q.ExistsOnly {
		frOffset, frNeed = 0, 1
	}
	frItems, frTotal, fits, err := s.pageFranchises(ctx, v, stored, q.Query, frOffset, frNeed, q.ExistsOnly, q.NoTotal, 0)
	if err != nil {
		return frontend.CollectionPage{}, true, err
	}
	if !fits {
		return frontend.CollectionPage{}, false, nil
	}
	if q.ExistsOnly {
		if frTotal > 0 {
			return frontend.CollectionPage{Total: 1}, true, nil
		}
		return frontend.CollectionPage{Total: 0}, true, nil
	}
	out = append(out, frItems...)
	return frontend.CollectionPage{Items: out, Total: ownedN + frTotal}, true, nil
}

// franchiseScope builds the SQL filter and optional union of movie filter
// libraries governing which franchise movies v may see in stored (or across
// all allowed movie libraries when stored is nil).
func (s *Service) franchiseScope(ctx context.Context, v *core.User, stored *core.Library) (base core.Filter, extraLibs []core.Library, anyAllowed, fits bool, err error) {
	base = core.Filter{Type: core.TypeMovie, LocalOnly: true}
	if stored != nil {
		if stored.ID != core.LibraryMovies && stored.ID != core.LibraryShows {
			base = s.libraryBrowseFilter(ctx, v, *stored, base)
		}
	} else {
		unrestricted, libs := s.movieSectionLibraries(ctx, v)
		if !unrestricted {
			if len(libs) == 0 {
				return core.Filter{}, nil, false, true, nil
			}
			if len(libs) == 1 {
				base = s.libraryBrowseFilter(ctx, v, libs[0], base)
			} else {
				extraLibs = libs
			}
		}
	}
	var ex LibraryPageExclusions
	if s.d.PageExclusions != nil {
		ex = s.d.PageExclusions(ctx, v, core.TypeMovie)
		if ex.Err != nil {
			return core.Filter{}, nil, false, false, ex.Err
		}
	}
	f, ok, none := viewerBrowseFilter(base, v, ex, core.TypeMovie)
	if !ok {
		return core.Filter{}, nil, false, false, nil
	}
	if none {
		return core.Filter{}, nil, false, true, nil
	}
	return f, extraLibs, true, true, nil
}

func (s *Service) franchiseFilterUnrestricted(v *core.User, f core.Filter, extraLibs []core.Library, query string) bool {
	if len(extraLibs) > 0 || strings.TrimSpace(query) != "" {
		return false
	}
	if f.CollectionMembers != 0 || len(f.ExtraRules) > 0 || f.ExtraFallback != "" || len(f.CatalogueSources) > 0 {
		return false
	}
	if f.MaxContentRating != "" || f.MinContentRating != "" || len(f.ContentRatings) > 0 || len(f.ExcludeContentRatings) > 0 {
		return false
	}
	if len(f.ExcludeGenres) > 0 || len(f.ExcludeKeywords) > 0 || len(f.ExcludeKeys) > 0 {
		return false
	}
	if len(f.IncludeCollectionNames) > 0 || len(f.ExcludeCollectionNames) > 0 {
		return false
	}
	if v != nil && s.d.Franchises.HasActiveViewerRules(v.ID) {
		return false
	}
	return true
}

func (s *Service) franchiseVisibleCounts(ctx context.Context, v *core.User, base core.Filter, extraLibs []core.Library, ids []int) (map[int]int, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(extraLibs) == 0 {
		return s.d.Franchises.FranchiseVisibleCounts(ctx, base, ids)
	}
	seen := make(map[int]map[core.RatingKey]struct{}, len(ids))
	for _, lib := range extraLibs {
		lf := s.libraryBrowseFilter(ctx, v, lib, base)
		byFr, err := s.d.Franchises.FranchiseVisibleKeys(ctx, lf, ids)
		if err != nil {
			return nil, err
		}
		for cid, keys := range byFr {
			set := seen[cid]
			if set == nil {
				set = make(map[core.RatingKey]struct{}, len(keys))
				seen[cid] = set
			}
			for _, k := range keys {
				set[k] = struct{}{}
			}
		}
	}
	counts := make(map[int]int, len(seen))
	for cid, set := range seen {
		counts[cid] = len(set)
	}
	return counts, nil
}

func (s *Service) pageFranchises(ctx context.Context, v *core.User, stored *core.Library, query string, offset, limit int, existsOnly, noTotal bool, after core.RatingKey) (items []core.Collection, total int, fits bool, err error) {
	base, extraLibs, anyAllowed, ok, err := s.franchiseScope(ctx, v, stored)
	if err != nil || !ok {
		return nil, 0, ok, err
	}
	if !anyAllowed {
		return nil, 0, true, nil
	}

	startCount, startID := 0, 0
	if after != 0 {
		startID = int(after.TMDB())
		summaries, err := s.d.Franchises.FranchiseSummaryBatch(ctx, []int{startID})
		if err != nil {
			return nil, 0, true, err
		}
		pivot, ok := summaries[startID]
		if !ok {
			return nil, 0, true, core.ErrNotAvailable
		}
		startCount = pivot.ChildCount
	}

	// Fast path 1: unrestricted viewer and built-in movie library (including
	// availability gate/hidden sets, which FranchiseVisibleCounts enforces on
	// every page just as browse_tally treats them as tally-backed shapes).
	// Reads O(page_size) rows from catalog_franchises and an O(1) total.
	if after == 0 && !existsOnly && s.franchiseFilterUnrestricted(v, base, extraLibs, query) {
		var frTotal int
		if !noTotal || offset > 0 {
			frTotal, err = s.d.Franchises.FranchiseTotal(ctx)
			if err != nil {
				return nil, 0, true, err
			}
			if offset >= frTotal {
				return nil, frTotal, true, nil
			}
		}
		if limit <= 0 {
			return nil, frTotal, true, nil
		}
		page, err := s.d.Franchises.FranchisesPage(ctx, offset, limit)
		if err != nil {
			return nil, 0, true, err
		}
		if len(page) == 0 {
			if noTotal {
				frTotal = offset
			}
			return nil, frTotal, true, nil
		}
		ids := make([]int, 0, len(page))
		for _, c := range page {
			if s.d.Visibility.CollectionAllowed(v, &c) {
				ids = append(ids, int(c.Key.TMDB()))
			}
		}
		counts, err := s.d.Franchises.FranchiseVisibleCounts(ctx, base, ids)
		if err != nil {
			return nil, 0, true, err
		}
		kept := make([]core.Collection, 0, len(page))
		for _, c := range page {
			if !s.d.Visibility.CollectionAllowed(v, &c) {
				continue
			}
			n := counts[int(c.Key.TMDB())]
			if n <= 0 {
				continue
			}
			c.ChildCount = n
			kept = append(kept, c)
		}
		// Top up if any candidate on the page had only unreleased or gated movies.
		reachedEnd := len(page) < limit
		if len(kept) < limit && !reachedEnd {
			last := page[len(page)-1]
			afterCount, afterID := last.ChildCount, int(last.Key.TMDB())
			batchSize := min(max((limit-len(kept))*2, 64), 400)
			for len(kept) < limit {
				batch, err := s.d.Franchises.FranchisesCursor(ctx, afterCount, afterID, batchSize)
				if err != nil {
					return nil, 0, true, err
				}
				if len(batch) == 0 {
					reachedEnd = true
					break
				}
				last = batch[len(batch)-1]
				afterCount, afterID = last.ChildCount, int(last.Key.TMDB())
				bIDs := make([]int, 0, len(batch))
				for _, c := range batch {
					if s.d.Visibility.CollectionAllowed(v, &c) {
						bIDs = append(bIDs, int(c.Key.TMDB()))
					}
				}
				bCounts, err := s.d.Franchises.FranchiseVisibleCounts(ctx, base, bIDs)
				if err != nil {
					return nil, 0, true, err
				}
				for _, c := range batch {
					if !s.d.Visibility.CollectionAllowed(v, &c) {
						continue
					}
					n := bCounts[int(c.Key.TMDB())]
					if n <= 0 {
						continue
					}
					c.ChildCount = n
					kept = append(kept, c)
					if len(kept) >= limit {
						break
					}
				}
				if len(batch) < batchSize {
					reachedEnd = true
					break
				}
			}
		}
		if noTotal || (reachedEnd && len(kept) < limit) {
			frTotal = offset + len(kept)
		}
		return kept, frTotal, true, nil
	}

	// Fast path 2: collection-backed library (CollectionMembers != 0).
	// collections_members(spec_id) is directly indexed and bounded by the
	// collection's own size, so one grouped query answers the page without
	// walking catalog_franchises.
	canSmallFilter := len(extraLibs) == 0 && len(base.IncludeCollectionNames) == 0 && len(base.ExcludeCollectionNames) == 0 &&
		(base.CollectionMembers != 0 || len(base.ExtraRules) > 0 || base.ExtraFallback != "" || len(base.CatalogueSources) > 0)
	trySmallFilter := func() ([]core.Collection, int, bool, error) {
		reqLimit := limit
		if existsOnly || reqLimit <= 0 {
			reqLimit = 1
		}
		reqOffset := offset
		if existsOnly {
			reqOffset = 0
		}
		var direct []core.Collection
		var hasMore, usedDirect bool
		var err error
		if startID != 0 {
			if seek, ok := s.d.Franchises.(interface {
				FranchisesInSmallFilterAfter(context.Context, core.Filter, string, int, int, int, int) ([]core.Collection, bool, bool, error)
			}); ok {
				direct, hasMore, usedDirect, err = seek.FranchisesInSmallFilterAfter(ctx, base, query, reqOffset, reqLimit, startCount, startID)
			}
		} else {
			direct, hasMore, usedDirect, err = s.d.Franchises.FranchisesInSmallFilter(ctx, base, query, reqOffset, reqLimit)
		}
		if err != nil || !usedDirect {
			return nil, 0, false, err
		}
		kept := make([]core.Collection, 0, len(direct))
		for _, c := range direct {
			if s.d.Visibility.CollectionAllowed(v, &c) {
				kept = append(kept, c)
			}
		}
		if existsOnly {
			if len(kept) > 0 {
				return nil, 1, true, nil
			}
			return nil, 0, true, nil
		}
		tot := reqOffset + len(kept)
		if hasMore {
			tot = reqOffset + len(kept) + 1
		}
		if limit <= 0 {
			kept = nil
		}
		return kept, tot, true, nil
	}
	if canSmallFilter && base.CollectionMembers != 0 {
		if direct, tot, used, err := trySmallFilter(); err != nil {
			return nil, 0, true, err
		} else if used {
			return direct, tot, true, nil
		}
	}

	// Fast path 3: keyset cursor walk over catalog_franchises with one
	// set-based visibility query per candidate batch. Handles large filtered
	// libraries (such as country exclusions), rating caps, search queries and
	// multiple filter libraries in O(page_size) batches.
	if existsOnly {
		offset, limit, noTotal = 0, 1, true
	} else if offset > 0 {
		// No filter can produce more franchises than catalog_franchises holds;
		// short-circuit past-the-end offsets in O(1) without walking the table.
		if frTotal, err := s.d.Franchises.FranchiseTotal(ctx); err != nil {
			return nil, 0, true, err
		} else if offset >= frTotal {
			return nil, frTotal, true, nil
		}
	}
	target := offset + limit
	if !noTotal && !existsOnly {
		target = offset + limit + 1
	}
	batchSize := min(max(limit*2, 128), 400)
	if existsOnly {
		batchSize = 64
	}
	trimmedQuery := strings.TrimSpace(query)
	afterCount, afterID := startCount, startID
	matched, checked, batches := 0, 0, 0
	triedSmallFilter := base.CollectionMembers != 0
	var kept []core.Collection
	if !existsOnly && limit > 0 {
		kept = make([]core.Collection, 0, limit)
	}
	for {
		var batch []core.Collection
		if trimmedQuery != "" {
			batch, err = s.d.Franchises.FranchisesSearch(ctx, trimmedQuery, afterCount, afterID, batchSize)
		} else {
			batch, err = s.d.Franchises.FranchisesCursor(ctx, afterCount, afterID, batchSize)
		}
		if err != nil {
			return nil, 0, true, err
		}
		if len(batch) == 0 {
			break
		}
		batches++
		checked += len(batch)
		last := batch[len(batch)-1]
		afterCount, afterID = last.ChildCount, int(last.Key.TMDB())
		ids := make([]int, 0, len(batch))
		for _, c := range batch {
			if s.d.Visibility.CollectionAllowed(v, &c) && (c.OwnerUserID == 0 || c.OwnerUserID == v.ID) {
				ids = append(ids, int(c.Key.TMDB()))
			}
		}
		if len(ids) > 0 {
			counts, err := s.franchiseVisibleCounts(ctx, v, base, extraLibs, ids)
			if err != nil {
				return nil, 0, true, err
			}
			for _, c := range batch {
				if !s.d.Visibility.CollectionAllowed(v, &c) || (c.OwnerUserID != 0 && c.OwnerUserID != v.ID) {
					continue
				}
				n := counts[int(c.Key.TMDB())]
				if n <= 0 {
					continue
				}
				c.ChildCount = n
				if !existsOnly && matched >= offset && len(kept) < limit {
					kept = append(kept, c)
				}
				matched++
				if matched >= target {
					break
				}
			}
		}
		if matched >= target || len(batch) < batchSize {
			break
		}
		// If two cursor batches had low hit rate on a filtered catalogue
		// library, try grouping the filter's matching movies directly so an
		// ultra-sparse filter does not walk every franchise.
		if canSmallFilter && !triedSmallFilter && batches == 2 && matched*4 < checked {
			triedSmallFilter = true
			if direct, tot, used, err := trySmallFilter(); err != nil {
				return nil, 0, true, err
			} else if used {
				return direct, tot, true, nil
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, 0, true, err
		}
	}
	if existsOnly {
		if matched > 0 {
			return nil, 1, true, nil
		}
		return nil, 0, true, nil
	}
	tot := matched
	if noTotal {
		tot = offset + len(kept)
	} else if matched >= target {
		tot = offset + len(kept) + 1
	}
	return kept, tot, true, nil
}

// PageArtworkCollections authorizes and loads a page of TMDB franchise keys
// (KindCollection) with fallback artwork in bounded set-based queries,
// avoiding per-collection Get + member hydration on Vela's SignPage path.
// handled is false when the host has no FranchiseStore or exclusions exceed
// SQL bounds, signaling the caller to fall back per key.
func (s *Service) PageArtworkCollections(ctx context.Context, viewer *core.Viewer, keys []core.RatingKey) (collections map[core.RatingKey]core.Collection, handled bool, err error) {
	if s.d.Franchises == nil || len(keys) == 0 {
		return nil, false, nil
	}
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, true, err
	}
	ids := make([]int, 0, len(keys))
	seen := make(map[int]bool, len(keys))
	for _, k := range keys {
		if k.Kind() != core.KindCollection {
			continue
		}
		id := int(k.TMDB())
		if id > 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return map[core.RatingKey]core.Collection{}, true, nil
	}
	base, extraLibs, anyAllowed, fits, err := s.franchiseScope(ctx, v, nil)
	if err != nil || !fits {
		return nil, false, err
	}
	if !anyAllowed {
		return map[core.RatingKey]core.Collection{}, true, nil
	}
	summaries, err := s.d.Franchises.FranchiseSummaryBatch(ctx, ids)
	if err != nil {
		return nil, false, err
	}
	checkIDs := make([]int, 0, len(summaries))
	for _, id := range ids {
		col, ok := summaries[id]
		if !ok || !s.d.Visibility.CollectionAllowed(v, &col) {
			continue
		}
		checkIDs = append(checkIDs, id)
	}
	if len(checkIDs) == 0 {
		return map[core.RatingKey]core.Collection{}, true, nil
	}
	counts, err := s.franchiseVisibleCounts(ctx, v, base, extraLibs, checkIDs)
	if err != nil {
		return nil, false, err
	}
	out := make(map[core.RatingKey]core.Collection, len(counts))
	var needArt []int
	for _, id := range checkIDs {
		n := counts[id]
		if n <= 0 {
			continue
		}
		col := summaries[id]
		col.ChildCount = n
		if col.PosterPath == "" || col.BackdropPath == "" {
			needArt = append(needArt, id)
		}
		out[col.Key] = col
	}
	if len(needArt) > 0 {
		applyFallback := func(art map[int][2]string) {
			for id, paths := range art {
				k := core.CollectionKey(int64(id))
				col, ok := out[k]
				if !ok {
					continue
				}
				if col.PosterPath == "" && paths[0] != "" {
					col.PosterPath = paths[0]
				}
				if col.BackdropPath == "" && paths[1] != "" {
					col.BackdropPath = paths[1]
				}
				out[k] = col
			}
		}
		if len(extraLibs) == 0 {
			if art, err := s.d.Franchises.FranchiseFallbackArt(ctx, base, needArt); err == nil {
				applyFallback(art)
			}
		} else {
			for _, lib := range extraLibs {
				lf := s.libraryBrowseFilter(ctx, v, lib, base)
				if art, err := s.d.Franchises.FranchiseFallbackArt(ctx, lf, needArt); err == nil {
					applyFallback(art)
				}
			}
		}
	}
	return out, true, nil
}

// smartCollectionHasVisibleMember checks whether a smart collection has at
// least one visible member without loading member pages when the viewer's
// exclusions fit an indexed SQL probe on collections_members.
func (s *Service) smartCollectionHasVisibleMember(ctx context.Context, v *core.User, c core.Collection) (bool, error) {
	if s.canCountSmartArtwork(v, c) {
		counter, err := s.ownedMemberCounter(ctx, v, nil, c.Type)
		if err != nil {
			return false, err
		}
		if counter != nil {
			n, err := counter(c)
			if err != nil || n > 0 {
				return n > 0, err
			}
		}
	}
	return s.collectionHasVisibleMember(ctx, v, c)
}

func (s *Service) canCountSmartArtwork(v *core.User, c core.Collection) bool {
	canSQL := c.Type != "" && !c.Upcoming && !c.Key.IsEphemeral() && (s.d.Visibility.Policy == nil || s.d.PageExclusions != nil)
	if canSQL && s.d.Visibility.SectionFor != nil {
		sec, ok := s.d.Visibility.SectionFor(c.Type)
		canSQL = ok && frontend.SectionAllowed(v, sec)
	}
	return canSQL
}

// PageArtworkSmartCollections authorizes and loads a page of smart collection
// keys (KindSmart), reusing the viewer lookup and per-type SQL member counters
// across the page instead of loading members per collection.
func (s *Service) PageArtworkSmartCollections(ctx context.Context, viewer *core.Viewer, keys []core.RatingKey) (map[core.RatingKey]core.Collection, error) {
	if s.d.Collections == nil || len(keys) == 0 {
		return map[core.RatingKey]core.Collection{}, nil
	}
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	if frontend.CollectionCardArtwork(ctx) {
		checker, ok := s.d.Collections.(collectionCardHeaders)
		if !ok {
			return nil, core.ErrNotAvailable
		}
		cards, err := checker.CollectionCardHeaders(ctx, v, keys)
		out := make(map[core.RatingKey]core.Collection, len(cards))
		for key, card := range cards {
			if s.d.Visibility.CollectionAllowed(v, card) {
				out[key] = *card
			}
		}
		return out, err
	}
	var headers map[core.RatingKey]*core.Collection
	if checker, ok := s.d.Collections.(RowViewCollectionBatchChecker); ok {
		headers, _, err = checker.CheckMembersBatch(ctx, v, keys, nil, 0)
		if err != nil {
			return nil, err
		}
	}
	out := make(map[core.RatingKey]core.Collection, len(keys))
	type candidate struct {
		key core.RatingKey
		col core.Collection
	}
	eligible := make(map[core.MediaType][]candidate)
	seen := make(map[core.RatingKey]bool, len(keys))
	for _, k := range keys {
		if k.Kind() != core.KindSmart || seen[k] {
			continue
		}
		seen[k] = true
		c := headers[k]
		if headers == nil {
			c, err = s.d.Collections.Get(ctx, v, k)
			if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnauthorized) {
				continue
			}
			if err != nil {
				return nil, err
			}
		}
		if c == nil || !s.d.Visibility.CollectionAllowed(v, c) || (c.OwnerUserID != 0 && c.OwnerUserID != v.ID) {
			continue
		}
		eligible[c.Type] = append(eligible[c.Type], candidate{key: k, col: *c})
	}
	for typ, cols := range eligible {
		var countable []core.Collection
		for _, item := range cols {
			if s.canCountSmartArtwork(v, item.col) {
				countable = append(countable, item.col)
			}
		}
		var counts map[int64]int
		if len(countable) > 0 {
			counter, err := s.ownedMemberBatchCounter(ctx, v, nil, typ)
			if err != nil {
				return nil, err
			}
			if counter != nil {
				counts, err = counter(countable)
				if err != nil {
					return nil, err
				}
			}
		}
		var unchecked []core.Collection
		for _, item := range cols {
			if counts[ownedCountID(item.col)] > 0 {
				out[item.key] = item.col
			} else {
				unchecked = append(unchecked, item.col)
			}
		}
		probes, err := s.collectionVisibleProbes(ctx, v, unchecked)
		if err != nil {
			return nil, err
		}
		for _, item := range cols {
			if probes[item.col.Key].has {
				out[item.key] = item.col
			}
		}
	}
	return out, nil
}

func (s *Service) libraryCollectionItems(ctx context.Context, v *core.User, device core.Device, library *Library, stored *core.Library, q frontend.ItemQuery) (core.Page, error) {
	detail, err := s.Item(ctx, frontend.ItemRequest{Viewer: v, Device: device, Key: q.CollectionKey})
	if err != nil || detail.Collection == nil {
		if err != nil {
			return core.Page{}, err
		}
		return core.Page{}, core.ErrNotFound
	}
	items, err := s.collectionItems(ctx, v, q.CollectionKey)
	if err != nil {
		return core.Page{}, err
	}
	var allowedInLib map[core.RatingKey]bool
	scopedToLib := false
	if stored != nil && stored.ID != core.LibraryMovies && stored.ID != core.LibraryShows && len(items) > 0 && s.d.LibraryMembers != nil {
		keys := make([]core.RatingKey, len(items))
		for i := range items {
			keys[i] = items[i].Key
		}
		var ok bool
		allowedInLib, _, ok, err = s.d.LibraryMembers(ctx, stored.ID, keys)
		if err != nil {
			return core.Page{}, err
		}
		scopedToLib = ok
	}
	// Strip library-level CatalogueSources/ExtraRules/CollectionMembers already
	// enforced by LibraryMembers, while keeping user query predicates.
	userQuery := q
	userQuery.Filter.CatalogueSources = nil
	userQuery.Filter.ExtraRules = nil
	userQuery.Filter.ExtraFallback = ""
	userQuery.Filter.CollectionMembers = 0
	filtered := make([]core.Title, 0, len(items))
	for _, title := range items {
		if title.Type != library.Type {
			continue
		}
		if scopedToLib && !allowedInLib[title.Key] {
			continue
		}
		ok, err := s.matchesUserQuery(ctx, v, title, userQuery)
		if err != nil {
			return core.Page{}, err
		}
		if ok {
			filtered = append(filtered, title)
		}
	}
	if err := s.sortTitles(ctx, filtered, q); err != nil {
		return core.Page{}, err
	}
	if q.AdjacentTo != 0 {
		return adjacentTitles(filtered, q)
	}
	if q.StartAt != 0 {
		return startingTitles(filtered, q)
	}
	total := len(filtered)
	start := min(max(q.Offset, 0), total)
	end := total
	if q.Limit > 0 {
		end = min(start+q.Limit, total)
	}
	return core.Page{Items: filtered[start:end], Total: total, TotalExact: true}, nil
}

// visibleProber answers a listing's visibility probe for collections it
// already knows the viewer can see (personalize's Year in Review, built from
// the viewer's own visible history) without paging their members; handled
// is false for any other collection.
type visibleProber interface {
	ProbeVisible(ctx context.Context, v *core.User, c core.Collection) (has bool, total int, handled bool, err error)
	AnswersVisible(key core.RatingKey) bool
}

// collectionHasVisibleMember reads bounded pages and stops at the first member
// the viewer can see, without calculating a child count.
// collectionVisibleProbe reports whether c has a member v may see, reading
// only until the first one, and the collection's member total. It never loads
// every member, so a listing can show large collections cheaply.
func (s *Service) collectionVisibleProbe(ctx context.Context, v *core.User, c core.Collection) (bool, int, error) {
	if p, ok := s.d.Collections.(visibleProber); ok {
		if has, total, handled, err := p.ProbeVisible(ctx, v, c); handled || err != nil {
			return has, total, err
		}
	}
	total := -1
	for offset := 0; ; {
		page, err := s.d.Collections.Items(ctx, v, c.Key, offset, 32)
		if err != nil {
			return false, 0, err
		}
		if total < 0 {
			total = page.Total
		}
		var batch []core.Title
		if c.Upcoming {
			batch = s.d.Visibility.UpcomingItems(ctx, v, page.Items, time.Now)
		} else {
			batch, err = s.allowTitles(ctx, v, page.Items)
			if err != nil {
				return false, 0, err
			}
		}
		if len(batch) > 0 {
			return true, total, nil
		}
		offset += len(page.Items)
		if offset >= page.Total || len(page.Items) == 0 {
			return false, total, nil
		}
		if err := ctx.Err(); err != nil {
			return false, 0, err
		}
	}
}

func (s *Service) collectionHasVisibleMember(ctx context.Context, v *core.User, c core.Collection) (bool, error) {
	if p, ok := s.d.Collections.(visibleProber); ok {
		if has, _, handled, err := p.ProbeVisible(ctx, v, c); handled || err != nil {
			return has, err
		}
	}
	for offset := 0; ; {
		page, err := s.d.Collections.Items(ctx, v, c.Key, offset, 32)
		if err != nil {
			return false, err
		}
		var batch []core.Title
		if c.Upcoming {
			batch = s.d.Visibility.UpcomingItems(ctx, v, page.Items, time.Now)
		} else {
			batch, err = s.allowTitles(ctx, v, page.Items)
			if err != nil {
				return false, err
			}
		}
		if len(batch) > 0 {
			return true, nil
		}
		offset += len(page.Items)
		if offset >= page.Total {
			return false, nil
		}
		if len(page.Items) == 0 {
			return false, errors.New("experience: incomplete collection page")
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
}

func (s *Service) notInterested(ctx context.Context, v *core.User) ([]frontend.NotInterestedEntry, error) {
	if s.d.NotInterestedSource == nil {
		return nil, core.ErrNotAvailable
	}
	entries, err := s.d.NotInterestedSource(ctx, v)
	if err != nil {
		return nil, err
	}
	result := make([]frontend.NotInterestedEntry, 0, len(entries))
	for _, entry := range entries {
		if _, err := s.item(ctx, v, entry.Key); err == nil {
			result = append(result, entry)
		}
	}
	return result, nil
}
func (s *Service) NotInterested(ctx context.Context, viewer *core.Viewer, _ core.Device, q frontend.PageQuery) (frontend.NotInterestedPage, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.NotInterestedPage{}, err
	}
	entries, err := s.notInterested(ctx, v)
	if err != nil {
		return frontend.NotInterestedPage{}, err
	}
	start, end := sliceBounds(len(entries), q.Offset, q.Limit)
	return frontend.NotInterestedPage{Items: entries[start:end], Total: len(entries)}, nil
}
func (s *Service) NotInterestedItem(ctx context.Context, viewer *core.Viewer, _ core.Device, key core.RatingKey) (frontend.NotInterestedEntry, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.NotInterestedEntry{}, err
	}
	if _, err = s.item(ctx, v, key); err != nil {
		return frontend.NotInterestedEntry{}, core.ErrNotFound
	}
	entries, err := s.notInterested(ctx, v)
	if err != nil {
		return frontend.NotInterestedEntry{}, err
	}
	for _, entry := range entries {
		if entry.Key == key {
			return entry, nil
		}
	}
	return frontend.NotInterestedEntry{}, core.ErrNotFound
}
