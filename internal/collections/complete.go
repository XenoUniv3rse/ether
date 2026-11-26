package collections

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
)

// ListComplete builds every eligible collection or returns an error. Unlike
// List, it never suppresses refresh or franchise-list failures; consumers that
// advertise an exact total must not use a partial result.
func (s *Service) ListComplete(ctx context.Context, user *core.User, typ core.MediaType) ([]core.Collection, error) {
	return s.listCompleteUntil(ctx, user, typ, nil)
}

// ListOwnedComplete builds every eligible non-franchise collection (built-in,
// owner and ephemeral) without materializing TMDB franchises, and reports
// whether TMDB franchises also belong to typ for this server configuration.
func (s *Service) ListOwnedComplete(ctx context.Context, user *core.User, typ core.MediaType) ([]core.Collection, bool, error) {
	rows, err := s.loadRows(ctx)
	if err != nil {
		return nil, false, err
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].builtin && !rows[j].builtin })
	out := make([]core.Collection, 0)
	for _, row := range rows {
		if !visible(row, user) || !row.spec.BrowseInApps || !typeMatches(row.spec, typ) {
			continue
		}
		keys, err := s.serve(ctx, row, user, true)
		if err != nil {
			return nil, false, fmt.Errorf("collections: complete list: %w", err)
		}
		if len(keys) > 0 && (row.spec.MediaType != MediaMixed || typ == "" || len(sectionKeys(keys, typ)) > 0) {
			out = append(out, s.collectionFor(ctx, row, user, typ, keys))
		}
	}
	out = append(out, s.listEphemeral(user, typ)...)
	hideFranchises := s.catCfg != nil && s.catCfg().HideFranchiseCollections
	return out, !hideFranchises && (typ == "" || typ == core.TypeMovie), nil
}

// IncludesFranchises reports whether the configured listing adds franchises
// after owned collections of this type.
func (s *Service) IncludesFranchises(typ core.MediaType) bool {
	return (typ == "" || typ == core.TypeMovie) && (s.catCfg == nil || !s.catCfg().HideFranchiseCollections)
}

// VisitOwned evaluates non-franchise collections in display order, stopping
// when visit has enough visible results for the requested page.
func (s *Service) VisitOwned(ctx context.Context, user *core.User, typ core.MediaType, query string, batchSize int, after core.RatingKey, visit func([]core.Collection) (bool, error)) (bool, error) {
	batchSize = max(1, batchSize)
	batchRows := make([]*specRow, 0, batchSize)
	flush := func() (bool, error) {
		if len(batchRows) == 0 {
			return false, nil
		}
		cols, err := s.collectionHeaders(ctx, batchRows, user, typ)
		batchRows = batchRows[:0]
		if err != nil {
			return false, err
		}
		nonempty := cols[:0]
		for _, col := range cols {
			if col.ChildCount > 0 {
				nonempty = append(nonempty, col)
			}
		}
		return visit(nonempty)
	}
	stop, err := s.visitOwnedSpecs(ctx, user, typ, query, batchSize, after, func(row *specRow) (bool, error) {
		batchRows = append(batchRows, row)
		if len(batchRows) == batchSize {
			return flush()
		}
		return false, nil
	})
	if stop || err != nil {
		return false, err
	}
	if stop, err := flush(); stop || err != nil {
		return false, err
	}
	if stop, err := visit(s.listEphemeral(user, typ)); stop || err != nil {
		return false, err
	}
	return s.IncludesFranchises(typ), nil
}

func (s *Service) collectionHeaders(ctx context.Context, rows []*specRow, user *core.User, typ core.MediaType) ([]core.Collection, error) {
	var coldIDs []int64
	var remaining []*specRow
	var remainingIndexes []int
	cold := s.idx.current() == nil && s.plainMemberViewer(user)
	for i, row := range rows {
		if cold && s.plainPersistedMembers(row) {
			coldIDs = append(coldIDs, row.id)
		} else {
			remaining = append(remaining, row)
			remainingIndexes = append(remainingIndexes, i)
		}
	}
	heads, err := s.mirror.CollectionMemberHeads(ctx, coldIDs, 4, false)
	if err != nil {
		return nil, err
	}
	served := make([][]core.RatingKey, len(rows))
	for i, row := range rows {
		served[i] = heads[row.id].Keys
	}
	if len(remaining) > 0 {
		batch, err := s.servedMembersBatch(ctx, remaining, user)
		if err != nil {
			return nil, err
		}
		for i, pos := range remainingIndexes {
			served[pos] = batch[i]
		}
	}
	var artKeys []core.RatingKey
	artSeen := make(map[core.RatingKey]bool)
	for i, row := range rows {
		keys := served[i]
		if row.spec.MediaType == MediaMixed && typ != "" {
			keys = sectionKeys(keys, typ)
		}
		for _, key := range keys[:min(len(keys), 4)] {
			if !artSeen[key] {
				artSeen[key] = true
				artKeys = append(artKeys, key)
			}
		}
	}
	titles, _ := s.mirror.ArtSummary(ctx, artKeys)
	art := make(map[core.RatingKey]core.Title, len(titles))
	for _, title := range titles {
		art[title.Key] = title
	}
	out := make([]core.Collection, 0, len(rows))
	for i, row := range rows {
		keys := served[i]
		col := s.collectionWithArt(ctx, row, typ, keys, art)
		if head, ok := heads[row.id]; ok {
			col.ChildCount = head.Count
			if col.RankedTop10 {
				col.ChildCount = min(col.ChildCount, 10)
			}
		}
		out = append(out, col)
	}
	return out, nil
}

// A plain persisted build has no serving-time transformation of member order
// or membership beyond the mirror release check.
func (s *Service) plainPersistedMembers(row *specRow) bool {
	return !row.lastBuilt.IsZero() && row.buildHash != "" && plainCollectionSpec(row.spec)
}

func plainCollectionSpec(sp core.CollectionSpec) bool {
	if sp.Refresh == RefreshLive || sp.ReleaseMode == "upcoming" || sp.ReleaseMode == "mixed" ||
		sp.MediaType == MediaMixed || sp.Sort == "personal" || sp.Limit > 0 || sp.Rotate != nil {
		return false
	}
	if _, facet := localFacetSpec(sp); facet {
		return false
	}
	for _, rules := range [][]core.CollectionRule{sp.Rules, sp.Exclude} {
		for _, rule := range rules {
			if rule.Field == "watched" || rule.Field == "inProgress" {
				return false
			}
		}
	}
	return true
}

func (s *Service) plainMemberViewer(user *core.User) bool {
	if user != nil && (user.MaxContentRating != "" || !user.IsOwner && len(user.AllowedLibraries) > 0) {
		return false
	}
	if source, ok := s.cat.(interface{ VisibilityFilter() *libraryfilter.Filter }); ok {
		filter := source.VisibilityFilter()
		return filter == nil || filter.Passive()
	}
	return true
}

// servedMembersBatch shares catalogue and watch-state reads across a batch
// while applying each collection's own rules and ordering.
func (s *Service) servedMembersBatch(ctx context.Context, rows []*specRow, user *core.User) ([][]core.RatingKey, error) {
	if err := s.preloadMembers(ctx, rows); err != nil {
		return nil, err
	}
	members := make([]*memberSet, len(rows))
	var all [2][]core.RatingKey
	var seen [2]map[core.RatingKey]bool
	groups := make([]int, len(rows))
	for i, row := range rows {
		ms, err := s.membersFor(ctx, row, true)
		if err != nil {
			return nil, fmt.Errorf("collections: complete list: %w", err)
		}
		members[i] = ms
		if row.spec.ReleaseMode == "upcoming" || row.spec.ReleaseMode == "mixed" {
			groups[i] = 1
		}
		g := groups[i]
		if seen[g] == nil {
			seen[g] = make(map[core.RatingKey]bool)
		}
		for _, key := range ms.keys {
			if !seen[g][key] {
				seen[g][key] = true
				all[g] = append(all[g], key)
			}
		}
	}
	var snapshots [2]*snapshot
	for g, keys := range all {
		if len(keys) > 0 {
			snap, err := s.snapshotForServe(ctx, keys, g == 1)
			if err != nil {
				return nil, err
			}
			snapshots[g] = snap
		}
	}
	served := make([][]core.RatingKey, len(rows))
	var statusKeys []core.RatingKey
	state := s.stateDep()
	if user != nil && state != nil {
		seen := make(map[core.RatingKey]bool)
		for _, ms := range members {
			pr := ms.personalRules()
			if len(pr.include)+len(pr.exclude) == 0 {
				continue
			}
			for _, key := range ms.keys {
				if !seen[key] {
					seen[key] = true
					statusKeys = append(statusKeys, key)
				}
			}
		}
	}
	var statuses map[core.RatingKey]core.WatchStatus
	if len(statusKeys) > 0 {
		var err error
		statuses, err = state.GetMany(ctx, user.ID, statusKeys)
		if err != nil {
			statuses = nil
			s.log.Debug("collections: watch state", "err", err)
		}
	}
	for i, row := range rows {
		if len(members[i].keys) == 0 {
			continue
		}
		keys := s.serveMembersWithStatuses(ctx, row, user, members[i], snapshots[groups[i]], statuses, true)
		served[i] = keys
	}
	return served, nil
}

// preloadMembers fills cold persisted membership in one batch. Live and
// unbuilt specs still follow their normal refresh policy in membersFor.
func (s *Service) preloadMembers(ctx context.Context, rows []*specRow) error {
	var missing []*specRow
	var ids []int64
	for _, row := range rows {
		_, facet := localFacetSpec(row.spec)
		if !facet && !row.lastBuilt.IsZero() && row.buildHash != "" && s.cached(row.id) == nil {
			missing = append(missing, row)
			ids = append(ids, row.id)
		}
	}
	keys, unshared, err := s.loadMembersMany(ctx, ids)
	if err != nil {
		return err
	}
	for _, row := range missing {
		ms := s.newMemberSet(row.spec, keys[row.id], unshared[row.id], row.lastBuilt, row.buildHash)
		if row.spec.Refresh == RefreshLive {
			ms.loadedAt = row.lastBuilt
		}
		s.mu.Lock()
		if s.members[row.id] == nil {
			s.members[row.id] = ms
		}
		s.mu.Unlock()
	}
	return nil
}

// FindVisible stops evaluating collections as soon as accept finds a visible
// member. It never materializes the complete collection listing.
func (s *Service) FindVisible(ctx context.Context, user *core.User, typ core.MediaType, accept func(core.Collection) (bool, error)) (bool, error) {
	rows, err := s.listCompleteUntil(ctx, user, typ, accept)
	return len(rows) > 0, err
}

func (s *Service) listCompleteUntil(ctx context.Context, user *core.User, typ core.MediaType, accept func(core.Collection) (bool, error)) ([]core.Collection, error) {
	rows, err := s.loadRows(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].builtin && !rows[j].builtin })
	out := make([]core.Collection, 0)
	for _, row := range rows {
		if !visible(row, user) || !row.spec.BrowseInApps || !typeMatches(row.spec, typ) {
			continue
		}
		keys, err := s.serve(ctx, row, user, true)
		if err != nil {
			return nil, fmt.Errorf("collections: complete list: %w", err)
		}
		if len(keys) > 0 && (row.spec.MediaType != MediaMixed || typ == "" || len(sectionKeys(keys, typ)) > 0) {
			c := s.collectionFor(ctx, row, user, typ, keys)
			if accept != nil {
				yes, e := accept(c)
				if e != nil {
					return nil, e
				}
				if yes {
					return []core.Collection{c}, nil
				}
			} else {
				out = append(out, c)
			}
		}
	}
	for _, c := range s.listEphemeral(user, typ) {
		if accept != nil {
			yes, e := accept(c)
			if e != nil {
				return nil, e
			}
			if yes {
				return []core.Collection{c}, nil
			}
		} else {
			out = append(out, c)
		}
	}
	hideFranchises := s.catCfg != nil && s.catCfg().HideFranchiseCollections
	if !hideFranchises && (typ == "" || typ == core.TypeMovie) {
		if accept != nil {
			var after int64
			for {
				c, e := s.mirror.NextFranchise(ctx, after)
				if errors.Is(e, sql.ErrNoRows) {
					return out, nil
				}
				if e != nil {
					return nil, e
				}
				yes, e := accept(*c)
				if e != nil {
					return nil, e
				}
				if yes {
					return []core.Collection{*c}, nil
				}
				after = c.Key.TMDB()
			}
		}
		franchises, err := s.mirror.Franchises(ctx)
		if err != nil {
			return nil, fmt.Errorf("collections: complete franchises: %w", err)
		}
		out = append(out, franchises...)
	}
	return out, nil
}
