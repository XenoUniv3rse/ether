package catalog

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// CollectionMemberCounts counts shared members under the same filter as
// Browse with CollectionMembers set, without fetching a sample title.
func (c *Catalog) CollectionMemberCounts(ctx context.Context, f core.Filter, ids []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	where, baseArgs, err := c.collectionMemberPredicate(ctx, f)
	if err != nil {
		return nil, err
	}
	for part := range store.Chunks(ids, 400) {
		args := make([]any, 0, len(part)+len(baseArgs))
		for _, id := range part {
			args = append(args, id)
		}
		args = append(args, baseArgs...)
		rows, err := c.db.QueryContext(ctx, `SELECT cm.spec_id,COUNT(DISTINCT cm.key)
			FROM collections_members cm JOIN catalog_titles t ON t.key=cm.key
			WHERE cm.spec_id IN (`+placeholders(len(part))+`) AND cm.shared=1 AND `+where+`
			GROUP BY cm.spec_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var count int
			if err := rows.Scan(&id, &count); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = count
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// collectionMemberPredicate returns the same scope used by CollectionMemberCounts.
// The caller supplies the collections_members and catalog_titles aliases cm and t.
func (c *Catalog) collectionMemberPredicate(ctx context.Context, f core.Filter) (string, []any, error) {
	if err := c.ensureCatalogueSources(ctx, f); err != nil {
		return "", nil, err
	}
	return c.browseWhere(c.viewerPaging(ctx, normalizeConsoleFilter(f)))
}
