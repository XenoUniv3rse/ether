package catalog

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// OwnedCollectionSelection proves ordinary persisted collections by indexed
// membership. Its exception IDs need the collection engine's serving rules.
type OwnedCollectionSelection struct {
	db            *sql.DB
	base, witness string
	args          []any
}

const ownedCollectionOrder = ` ORDER BY builtin DESC,browse_anchor,browse_rank,id`

// OwnedCollectionSelection builds the membership proof for Plex's unscoped
// collection listing.
func (m *Mirror) OwnedCollectionSelection(owner int64, typ core.MediaType) *OwnedCollectionSelection {
	return newOwnedCollectionSelection(m.db, m.now(), owner, typ, "", nil)
}

// ScopedOwnedCollectionSelection adds the exact SQL scope already used to
// count the viewer's shared collection members.
func (c *Catalog) ScopedOwnedCollectionSelection(ctx context.Context, owner int64, typ core.MediaType, filter core.Filter) (*OwnedCollectionSelection, error) {
	where, args, err := c.collectionMemberPredicate(ctx, filter)
	if err != nil {
		return nil, err
	}
	return newOwnedCollectionSelection(c.db, c.now(), owner, typ, where, args), nil
}

func newOwnedCollectionSelection(db *sql.DB, now time.Time, owner int64, typ core.MediaType, scope string, scopeArgs []any) *OwnedCollectionSelection {
	base := ` FROM collections_specs s WHERE enabled=1 AND (owner_user_id=0 OR owner_user_id=?)
		AND (browse_mode<0 OR (browse_mode>0 AND (?='' OR browse_type IN (?, 'mixed'))))`
	witness := `(browse_mode=1 AND last_built_at<>0 AND build_hash<>'' AND EXISTS (
		SELECT 1 FROM collections_members cm JOIN catalog_titles t ON t.key=cm.key
		WHERE cm.spec_id=s.id AND t.adult=0 AND ` + collectionMemberReleasedSQL + ` LIMIT 1 OFFSET 0
	)`
	args := []any{owner, typ, typ, now.Unix(), now.Unix()}
	if scope != "" {
		witness += ` AND EXISTS (
			SELECT 1 FROM collections_members cm JOIN catalog_titles t ON t.key=cm.key
			WHERE cm.spec_id=s.id AND cm.shared=1 AND (` + scope + `) LIMIT 1 OFFSET 0
		)`
		args = append(args, scopeArgs...)
	}
	return &OwnedCollectionSelection{db: db, base: base, witness: witness + ")", args: args}
}

func (s *OwnedCollectionSelection) ExceptionIDs(ctx context.Context) ([]int64, error) {
	return s.ids(ctx, s.base+` AND NOT `+s.witness+`
		AND (browse_mode<>1 OR last_built_at=0 OR build_hash='' OR
		     EXISTS(SELECT 1 FROM collections_members cm WHERE cm.spec_id=s.id LIMIT 1 OFFSET 0))`+ownedCollectionOrder, s.args)
}

func (s *OwnedCollectionSelection) Page(ctx context.Context, visibleExceptions []int64, offset, limit int) ([]int64, int, error) {
	where := s.base + ` AND (` + s.witness
	if len(visibleExceptions) > 0 {
		// IDs are strongly typed. Using decimal literals avoids the engine's
		// bind-parameter limit when there are many rule-derived collections.
		ids := make([]string, 0, len(visibleExceptions))
		for _, id := range visibleExceptions {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		where += ` OR s.id IN (` + strings.Join(ids, ",") + `)`
	}
	where += ")"
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+where, s.args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 || offset >= total {
		return nil, total, nil
	}
	ids, err := s.ids(ctx, where+ownedCollectionOrder+` LIMIT ? OFFSET ?`, append(s.args, limit, offset))
	return ids, total, err
}

func (s *OwnedCollectionSelection) ids(ctx context.Context, where string, args []any) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id`+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
