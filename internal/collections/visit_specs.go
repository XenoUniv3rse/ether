package collections

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

const groupedSpecsSQLite = `builtin=0 AND CASE WHEN json_valid(spec_json) THEN (
	COALESCE(json_extract(spec_json, '$.folder'), '') <> ''
	OR json_type(spec_json, '$.addonSync') = 'object') ELSE FALSE END`
const groupedSpecsPostgres = `builtin=0 AND (
	COALESCE(spec_json::jsonb->>'folder', '') <> ''
	OR jsonb_typeof(spec_json::jsonb->'addonSync') = 'object')`

func init() {
	store.Register("collections", 13, `CREATE INDEX collections_specs_browse_order
		ON collections_specs (builtin DESC, id ASC) WHERE enabled=1;`)
	store.RegisterDialect("collections", 14, store.SQLite, `CREATE INDEX collections_specs_grouped
		ON collections_specs (id) WHERE `+groupedSpecsSQLite+`;`)
	store.RegisterDialect("collections", 14, store.Postgres, `CREATE INDEX collections_specs_grouped
		ON collections_specs (id) WHERE `+groupedSpecsPostgres+`;`)
}

// visitOwnedSpecs reads enabled candidates in indexed display order.
func (s *Service) visitOwnedSpecs(ctx context.Context, user *core.User, typ core.MediaType, query string, batchSize int, after core.RatingKey, visit func(*specRow) (bool, error)) (bool, error) {
	var userID int64
	if user != nil {
		userID = user.ID
	}
	var pivotBuiltin int
	var pivotAnchor, pivotRank, pivotID int64
	if after != 0 {
		base, _ := after.CollectionSection()
		if after.IsEphemeral() || base.Kind() != core.KindSmart {
			return false, core.ErrNotAvailable
		}
		pivotID = base.TMDB()
		err := s.db.QueryRowContext(ctx, `SELECT builtin,browse_anchor,browse_rank FROM collections_specs WHERE id=?`, pivotID).
			Scan(&pivotBuiltin, &pivotAnchor, &pivotRank)
		if errors.Is(err, sql.ErrNoRows) {
			return false, core.ErrNotAvailable
		}
		if err != nil {
			return false, err
		}
	}
	// Built-ins precede owner collections. Keep separate index ranges so
	// each query can seek to the last spec it read without sorting.
	for _, builtin := range []int{1, 0} {
		afterAnchor, afterRank, afterID := int64(math.MinInt64), int64(0), int64(0)
		if after != 0 {
			if builtin > pivotBuiltin {
				continue
			}
			if builtin == pivotBuiltin {
				afterAnchor, afterRank, afterID = pivotAnchor, pivotRank, pivotID
			}
		}
		for {
			rows, err := s.db.QueryContext(ctx, `SELECT `+specCols+`,browse_anchor,browse_rank FROM collections_specs
				WHERE enabled=1 AND builtin=? AND (browse_anchor,browse_rank,id)>(?,?,?)
				AND (owner_user_id=0 OR owner_user_id=?)
				AND (browse_mode<0 OR (browse_mode>0 AND (?='' OR browse_type IN (?, 'mixed'))))
				ORDER BY browse_anchor,browse_rank,id LIMIT ?`, builtin, afterAnchor, afterRank, afterID, userID, typ, typ, batchSize)
			if err != nil {
				return false, err
			}
			var batch []*specRow
			n := 0
			for rows.Next() {
				n++
				row, err := scanSpecRow(rows, &afterAnchor, &afterRank)
				if row == nil {
					rows.Close()
					return false, err
				}
				afterID = row.id
				if err != nil {
					s.log.Warn("collections: skipping unreadable collection", "err", err)
					continue
				}
				if visible(row, user) && row.spec.BrowseInApps && typeMatches(row.spec, typ) &&
					(query == "" || strings.Contains(strings.ToLower(row.spec.Name), query)) {
					batch = append(batch, row)
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return false, err
			}
			for _, row := range batch {
				if stop, err := visit(row); stop || err != nil {
					return stop, err
				}
			}
			if n < batchSize {
				break
			}
		}
	}
	return false, nil
}
