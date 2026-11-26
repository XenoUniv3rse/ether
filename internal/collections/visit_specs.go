package collections

import (
	"context"
	"sort"
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

// visitOwnedSpecs follows the display order. Ungrouped specs use the browse
// index. Grouped specs retain the folder and add-on ordering computed by loadRows.
func (s *Service) visitOwnedSpecs(ctx context.Context, user *core.User, typ core.MediaType, query string, batchSize int, visit func(*specRow) (bool, error)) (bool, error) {
	eligible := func(row *specRow) bool {
		return visible(row, user) && row.spec.BrowseInApps && typeMatches(row.spec, typ) &&
			(query == "" || strings.Contains(strings.ToLower(row.spec.Name), query))
	}
	groupedSQL := groupedSpecsSQLite
	if store.DialectOf(s.db) == store.Postgres {
		groupedSQL = groupedSpecsPostgres
	}
	var grouped bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM collections_specs WHERE `+groupedSQL+`)`).Scan(&grouped); err != nil {
		return false, err
	}
	if grouped {
		rows, err := s.loadRows(ctx)
		if err != nil {
			return false, err
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].builtin && !rows[j].builtin })
		for _, row := range rows {
			if eligible(row) {
				if stop, err := visit(row); stop || err != nil {
					return stop, err
				}
			}
		}
		return false, nil
	}
	var userID int64
	if user != nil {
		userID = user.ID
	}
	// Built-ins precede owner collections. Keep separate index ranges so
	// each query can seek to the last spec it read without sorting.
	for _, builtin := range []int{1, 0} {
		var after int64
		for {
			rows, err := s.db.QueryContext(ctx, `SELECT `+specCols+` FROM collections_specs
				WHERE enabled=1 AND builtin=? AND id>? AND (owner_user_id=0 OR owner_user_id=?)
				ORDER BY id LIMIT ?`, builtin, after, userID, batchSize)
			if err != nil {
				return false, err
			}
			var batch []*specRow
			n := 0
			for rows.Next() {
				n++
				row, err := scanSpecRow(rows)
				if row == nil {
					rows.Close()
					return false, err
				}
				after = row.id
				if err != nil {
					s.log.Warn("collections: skipping unreadable collection", "err", err)
					continue
				}
				if eligible(row) {
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
