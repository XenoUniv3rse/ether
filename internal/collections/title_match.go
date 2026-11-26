package collections

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.RegisterAfter("collections_title_match", "collections")
	store.Register("collections_title_match", 1, `
CREATE INDEX IF NOT EXISTS collections_members_title_match ON collections_members(key);
`)
}

// RekeyTitle moves explicit title sources and materialized members. The returned
// collection IDs can be invalidated after the caller commits.
func RekeyTitle(ctx context.Context, tx *sql.Tx, from, to core.RatingKey, ranges []core.TitleKeyRange) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,spec_json FROM collections_specs WHERE id IN
		(SELECT spec_id FROM collections_members WHERE key=?
		UNION SELECT spec_id FROM collections_title_refs WHERE title_key=?)`, int64(from), from.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type specRow struct {
		id  int64
		raw string
	}
	var affected []specRow
	for rows.Next() {
		var row specRow
		if err := rows.Scan(&row.id, &row.raw); err != nil {
			return nil, err
		}
		affected = append(affected, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	ids := make([]int64, 0, len(affected))
	for _, row := range affected {
		var spec core.CollectionSpec
		if err := json.Unmarshal([]byte(row.raw), &spec); err != nil {
			return nil, err
		}
		raw := row.raw
		dirty := false
		for i := range spec.Sources {
			source := &spec.Sources[i]
			if source.Kind != "titles" || !slices.Contains(source.Keys, from.String()) {
				continue
			}
			for j, key := range source.Keys {
				if key == from.String() {
					source.Keys[j] = to.String()
				}
			}
			var seen bool
			source.Keys = slices.DeleteFunc(source.Keys, func(key string) bool {
				if key != to.String() {
					return false
				}
				duplicate := seen
				seen = true
				return duplicate
			})
			dirty = true
		}
		if dirty {
			encoded, err := json.Marshal(spec)
			if err != nil {
				return nil, err
			}
			raw = string(encoded)
		}
		res, err := tx.ExecContext(ctx, `UPDATE collections_specs SET `+specDocumentColumns+`,build_hash='',changed_at=?
			WHERE id=? AND spec_json=?`, append(specDocumentArgs(raw), time.Now().Unix(), row.id, row.raw)...)
		if err != nil {
			return nil, err
		}
		if updated, err := res.RowsAffected(); err != nil || updated != 1 {
			return nil, core.ErrNotAvailable
		}
		// Preserve the chosen entry's position unless the destination was
		// already in the collection.
		if _, err := tx.ExecContext(ctx, `DELETE FROM collections_members WHERE spec_id=? AND key=?
			AND EXISTS (SELECT 1 FROM collections_members WHERE spec_id=? AND key=?)`, row.id, int64(from), row.id, int64(to)); err != nil {
			return nil, err
		}
		ids = append(ids, row.id)
	}
	for _, r := range ranges {
		if _, err := tx.ExecContext(ctx, `UPDATE collections_members SET key=key+? WHERE key BETWEEN ? AND ?`, r.Delta, r.First, r.Last); err != nil {
			return nil, err
		}
	}
	return ids, nil
}
