package collections

import (
	"context"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
)

// plainOwnedPage answers exact offset paging for persisted collections whose
// membership is unchanged by the viewer or serving-time rules. Discarded
// rows need only a membership witness, so only the page gets headers.
func (s *Service) plainOwnedPage(ctx context.Context, user *core.User, typ core.MediaType, offset, limit int) ([]core.Collection, int, bool, error) {
	if s.idx.current() != nil || !s.plainMemberViewer(user) {
		return nil, 0, false, nil
	}
	var userID int64
	if user != nil {
		userID = user.ID
	}
	var complex bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM collections_specs WHERE enabled=1 AND (owner_user_id=0 OR owner_user_id=?)
		AND (browse_mode<0 OR (browse_mode>0 AND (?='' OR browse_type IN (?, 'mixed'))
		AND (browse_mode<>1 OR last_built_at=0 OR build_hash='')))
	)`, userID, typ, typ).Scan(&complex); err != nil || complex {
		return nil, 0, false, err
	}
	where := ` FROM collections_specs s WHERE enabled=1 AND browse_mode=1 AND (?='' OR browse_type=?)
		AND (owner_user_id=0 OR owner_user_id=?) AND EXISTS (
			SELECT 1 FROM collections_members cm JOIN catalog_titles t ON t.key=cm.key
			WHERE cm.spec_id=s.id AND t.adult=0 AND ` + catalog.CollectionMemberReleasedSQL + `
			LIMIT 1 OFFSET 0
		)`
	now := s.now().Unix()
	args := []any{typ, typ, userID, now, now}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+where, args...).Scan(&total); err != nil {
		return nil, 0, true, err
	}
	var out []core.Collection
	if limit > 0 && offset < total {
		rows, err := s.db.QueryContext(ctx, `SELECT `+specCols+where+
			` ORDER BY builtin DESC,browse_anchor,browse_rank,id LIMIT ? OFFSET ?`, append(args, limit, offset)...)
		if err != nil {
			return nil, 0, true, err
		}
		var specs []*specRow
		for rows.Next() {
			row, err := scanSpecRow(rows)
			if err != nil {
				rows.Close()
				return nil, 0, true, err
			}
			specs = append(specs, row)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, 0, true, err
		}
		out, err = s.collectionHeaders(ctx, specs, user, typ)
		if err != nil {
			return nil, 0, true, err
		}
	}
	ephemeral := s.listEphemeral(user, typ)
	start := max(0, offset-total)
	left := limit - len(out)
	if left > 0 && start < len(ephemeral) {
		out = append(out, ephemeral[start:min(start+left, len(ephemeral))]...)
	}
	return out, total + len(ephemeral), true, nil
}
