package catalog

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// CollectionMemberReleasedSQL mirrors Title.IsReleased for the catalog_titles
// alias t. Pass the current Unix second twice to the two cutoffs.
const CollectionMemberReleasedSQL = `((t.released_ts <> 0 AND t.released_ts <= ?)
				    OR (t.type = 'movie' AND EXISTS (
				      SELECT 1 FROM catalog_home_release h WHERE h.key = t.key
				        AND h.checked <> 0 AND h.earliest_ts <> 0 AND h.earliest_ts <= ? LIMIT 1 OFFSET 0
				    )))`

type CollectionMemberHead struct {
	Keys  []core.RatingKey
	Count int
}

// CollectionMemberHeads returns a released prefix and an exact count per collection.
// In postersOnly mode it seeks the same prefix and returns only poster candidates.
func (m *Mirror) CollectionMemberHeads(ctx context.Context, ids []int64, limit int, postersOnly bool) (map[int64]CollectionMemberHead, error) {
	out := make(map[int64]CollectionMemberHead, len(ids))
	count := "COUNT(*) OVER ()"
	if postersOnly {
		count = "0"
	}
	now := m.now().Unix()
	for part := range store.Chunks(ids, 150) {
		// Separate branches constrain each scan by the (spec_id,pos) index.
		query := make([]string, 0, len(part))
		args := make([]any, 0, 4*len(part))
		for _, id := range part {
			query = append(query, `SELECT spec_id, pos, key, member_count FROM (
				SELECT cm.spec_id, cm.pos, cm.key, t.poster, `+count+` AS member_count
				FROM collections_members cm
				JOIN catalog_titles t ON t.key = cm.key
				WHERE cm.spec_id = ? AND t.adult = 0
				  AND `+CollectionMemberReleasedSQL+`
				ORDER BY cm.pos LIMIT ?
			) AS head`)
			if postersOnly {
				query[len(query)-1] += " WHERE poster <> ''"
			}
			args = append(args, id, now, now, limit)
		}
		rows, err := m.db.QueryContext(ctx, strings.Join(query, " UNION ALL ")+` ORDER BY spec_id, pos`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, pos int64
			var key core.RatingKey
			var n int
			if err := rows.Scan(&id, &pos, &key, &n); err != nil {
				rows.Close()
				return nil, err
			}
			head := out[id]
			head.Keys = append(head.Keys, key)
			head.Count = n
			out[id] = head
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// PersistedOwnedCollectionPage counts and pages plain persisted collections
// with at least one released member. The membership join stays on the mirror,
// which owns the release and adult predicates.
func (m *Mirror) PersistedOwnedCollectionPage(ctx context.Context, owner int64, typ core.MediaType, offset, limit int) ([]int64, int, error) {
	where := ` FROM collections_specs s WHERE enabled=1 AND browse_mode=1 AND (?='' OR browse_type=?)
		AND (owner_user_id=0 OR owner_user_id=?) AND EXISTS (
			SELECT 1 FROM collections_members cm JOIN catalog_titles t ON t.key=cm.key
			WHERE cm.spec_id=s.id AND t.adult=0 AND ` + CollectionMemberReleasedSQL + `
			LIMIT 1 OFFSET 0
		)`
	now := m.now().Unix()
	args := []any{typ, typ, owner, now, now}
	var total int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*)`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 || offset >= total {
		return nil, total, nil
	}
	rows, err := m.db.QueryContext(ctx, `SELECT s.id`+where+
		` ORDER BY builtin DESC,browse_anchor,browse_rank,id LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	return ids, total, rows.Err()
}
