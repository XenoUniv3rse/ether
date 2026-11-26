package collections

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
)

// IndexedOwnedCollections answers exact offset paging for persisted
// collections. SQL skips ordinary collections by index. Only collections
// whose rules or policy cannot be proven by membership witnesses are
// evaluated before paging. A nil viewer filter uses Plex's serving rules.
func (s *Service) IndexedOwnedCollections(ctx context.Context, user *core.User, typ core.MediaType, filter *core.Filter, offset, limit int,
	evaluate func([]core.Collection) ([]core.Collection, error),
) ([]core.Collection, int, bool, error) {
	if !s.plainMemberViewer(user) {
		return nil, 0, false, nil
	}
	var viewerID int64
	if user != nil {
		viewerID = user.ID
	}
	selection := s.mirror.OwnedCollectionSelection(viewerID, typ)
	if filter != nil {
		scoped, ok := s.cat.(interface {
			ScopedOwnedCollectionSelection(context.Context, int64, core.MediaType, core.Filter) (*catalog.OwnedCollectionSelection, error)
		})
		if !ok {
			return nil, 0, false, nil
		}
		var err error
		selection, err = scoped.ScopedOwnedCollectionSelection(ctx, viewerID, typ, *filter)
		if err != nil {
			return nil, 0, true, err
		}
	}
	ids, err := selection.ExceptionIDs(ctx)
	if err != nil {
		return nil, 0, true, err
	}
	exceptions, err := s.orderedSpecRows(ctx, ids)
	if err != nil {
		return nil, 0, true, err
	}
	exceptions = retainEligibleOwnedSpecs(exceptions, user, typ)
	byID := make(map[int64]core.Collection, len(exceptions))
	var exceptionIDs []int64
	deadline := time.Now().Add(2 * time.Second)
	evalRows := func(rows []*specRow) ([]core.Collection, error) {
		if evaluate != nil {
			cols, err := s.collectionHeaders(ctx, rows, user, typ)
			if err != nil {
				return nil, err
			}
			nonempty := cols[:0]
			for _, c := range cols {
				if c.ChildCount > 0 {
					nonempty = append(nonempty, c)
				}
			}
			return evaluate(nonempty)
		}
		cols := make([]core.Collection, 0, len(rows))
		for _, row := range rows {
			if col, ok := s.listCollection(ctx, user, typ, row, deadline); ok {
				cols = append(cols, col)
			}
		}
		return cols, nil
	}
	const batchSize = 100
	for start := 0; start < len(exceptions); start += batchSize {
		cols, err := evalRows(exceptions[start:min(start+batchSize, len(exceptions))])
		if err != nil {
			return nil, 0, true, err
		}
		for _, col := range cols {
			key, _ := col.Key.CollectionSection()
			id := key.TMDB()
			byID[id] = col
			exceptionIDs = append(exceptionIDs, id)
		}
	}
	pageIDs, total, err := selection.Page(ctx, exceptionIDs, offset, limit)
	if err != nil {
		return nil, 0, true, err
	}
	var out []core.Collection
	if len(pageIDs) > 0 {
		rows, err := s.orderedSpecRows(ctx, pageIDs)
		if err != nil {
			return nil, 0, true, err
		}
		rows = retainEligibleOwnedSpecs(rows, user, typ)
		var ordinary []*specRow
		for _, row := range rows {
			if _, ok := byID[row.id]; !ok {
				ordinary = append(ordinary, row)
			}
		}
		cols, err := s.collectionHeaders(ctx, ordinary, user, typ)
		if err != nil {
			return nil, 0, true, err
		}
		if evaluate != nil {
			cols, err = evaluate(cols)
			if err != nil {
				return nil, 0, true, err
			}
		}
		for _, col := range cols {
			key, _ := col.Key.CollectionSection()
			byID[key.TMDB()] = col
		}
		for _, row := range rows {
			if c, ok := byID[row.id]; ok {
				out = append(out, c)
			}
		}
	}
	ephemeral := s.listEphemeral(user, typ)
	if evaluate != nil {
		ephemeral, err = evaluate(ephemeral)
		if err != nil {
			return nil, 0, true, err
		}
	}
	start, left := max(0, offset-total), limit-len(out)
	if left > 0 && start < len(ephemeral) {
		out = append(out, ephemeral[start:min(start+left, len(ephemeral))]...)
	}
	return out, total + len(ephemeral), true, nil
}

func retainEligibleOwnedSpecs(rows []*specRow, viewer *core.User, typ core.MediaType) []*specRow {
	out := rows[:0]
	for _, row := range rows {
		if visible(row, viewer) && row.spec.BrowseInApps && typeMatches(row.spec, typ) {
			out = append(out, row)
		}
	}
	return out
}

func (s *Service) orderedSpecRows(ctx context.Context, ids []int64) ([]*specRow, error) {
	rows, err := s.rowsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]*specRow, 0, len(rows))
	for _, id := range ids {
		if row := rows[id]; row != nil {
			out = append(out, row)
		}
	}
	return out, nil
}
