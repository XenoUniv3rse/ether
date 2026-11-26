package pluginitems

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("pluginitems", 25, `
ALTER TABLE plugin_items ADD COLUMN presence_counted INTEGER NOT NULL DEFAULT 0;
CREATE INDEX plugin_items_presence_pending ON plugin_items(library_id) WHERE presence_counted = 0;
CREATE INDEX plugin_item_fields_present ON plugin_item_fields(field, item_ref) WHERE v_text <> '';
CREATE TABLE plugin_item_field_presence (
	library_id BIGINT NOT NULL,
	level TEXT NOT NULL,
	is_root INTEGER NOT NULL,
	field TEXT NOT NULL,
	n BIGINT NOT NULL,
	text_n BIGINT NOT NULL,
	PRIMARY KEY (library_id, field, level, is_root)
);
`)
}

type fieldPresenceKey struct {
	libraryID int64
	level     string
	root      int
	field     string
}

type fieldPresenceCount struct {
	n, textN int64
}

// A write subtracts the old counted items and adds the new ones in the same
// transaction. Scope this aggregation to the write chunk. A multivalued field
// contributes once, and the empty key counts all items in the scope.
type fieldPresenceDelta struct {
	isPG   bool
	counts map[fieldPresenceKey]fieldPresenceCount
}

func (d *fieldPresenceDelta) collect(ctx context.Context, tx *sql.Tx, refs []core.ItemRef, sign int64) error {
	if len(refs) == 0 {
		return nil
	}
	args := make([]any, len(refs))
	for i, ref := range refs {
		args[i] = string(ref)
	}
	join := `CROSS JOIN plugin_item_fields f WHERE f.item_ref = i.item_ref`
	if d.isPG {
		join = `CROSS JOIN LATERAL (
		SELECT field, v_text FROM plugin_item_fields WHERE item_ref = i.item_ref OFFSET 0
	) f`
	}
	rows, err := tx.QueryContext(ctx, `
WITH touched AS MATERIALIZED (
	SELECT item_ref, library_id, level, CASE WHEN parent_ref = '' THEN 1 ELSE 0 END AS is_root
	FROM plugin_items WHERE item_ref IN (`+store.Placeholders(len(args))+`) AND presence_counted = 1
),
present AS (
	SELECT i.library_id, i.level, i.is_root, f.field, MAX(CASE WHEN f.v_text <> '' THEN 1 ELSE 0 END) AS has_text
	FROM touched i `+join+`
	GROUP BY i.item_ref, i.library_id, i.level, i.is_root, f.field
)
SELECT library_id, level, is_root, field, COUNT(*), SUM(has_text) FROM present
GROUP BY library_id, level, is_root, field
UNION ALL
SELECT library_id, level, is_root, '', COUNT(*), COUNT(*) FROM touched
GROUP BY library_id, level, is_root`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k fieldPresenceKey
		var c fieldPresenceCount
		if err := rows.Scan(&k.libraryID, &k.level, &k.root, &k.field, &c.n, &c.textN); err != nil {
			return err
		}
		if d.counts == nil {
			d.counts = make(map[fieldPresenceKey]fieldPresenceCount)
		}
		prev := d.counts[k]
		d.counts[k] = fieldPresenceCount{prev.n + sign*c.n, prev.textN + sign*c.textN}
	}
	return rows.Err()
}

func (d *fieldPresenceDelta) finish(ctx context.Context, tx *sql.Tx, refs []core.ItemRef) error {
	if len(refs) > 0 {
		args := make([]any, len(refs))
		for i, ref := range refs {
			args[i] = string(ref)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE plugin_items SET presence_counted = 1
WHERE item_ref IN (`+store.Placeholders(len(args))+`) AND presence_counted = 0`, args...); err != nil {
			return err
		}
		if err := d.collect(ctx, tx, refs, 1); err != nil {
			return err
		}
	}

	keys := make([]fieldPresenceKey, 0, len(d.counts))
	for k, c := range d.counts {
		if c.n != 0 || c.textN != 0 {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b fieldPresenceKey) int {
		switch {
		case a.libraryID < b.libraryID:
			return -1
		case a.libraryID > b.libraryID:
			return 1
		case a.field != b.field:
			return strings.Compare(a.field, b.field)
		case a.level != b.level:
			return strings.Compare(a.level, b.level)
		default:
			return a.root - b.root
		}
	})
	for start := 0; start < len(keys); start += 150 {
		chunk := keys[start:min(start+150, len(keys))]
		values := make([]string, 0, len(chunk))
		args := make([]any, 0, 6*len(chunk))
		for _, k := range chunk {
			c := d.counts[k]
			values = append(values, "(?, ?, ?, ?, ?, ?)")
			args = append(args, k.libraryID, k.level, k.root, k.field, c.n, c.textN)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_item_field_presence(library_id, level, is_root, field, n, text_n)
VALUES `+strings.Join(values, ",")+`
ON CONFLICT(library_id, field, level, is_root) DO UPDATE SET
	n = plugin_item_field_presence.n + excluded.n,
	text_n = plugin_item_field_presence.text_n + excluded.text_n`, args...); err != nil {
			return err
		}
	}
	return nil
}

func lockFieldPresence(ctx context.Context, tx *sql.Tx, isPG bool, lib core.LibraryID) error {
	if !isPG {
		return nil
	}
	return store.LockKey(ctx, store.Postgres, tx, "plugin-field-presence:"+strconv.FormatInt(int64(lib), 10))
}

// probeBrowsePresence uses the same root/level scope as the page. Until all
// rows have been observed by the write path, use match-first for negative
// predicates and leave exact totals to the filtered query.
func (s *Service) probeBrowsePresence(ctx context.Context, q BrowseQuery, parentRef, level string) (browseTallyInfo, error) {
	var tally browseTallyInfo
	if parentRef != "" {
		return tally, nil
	}
	rec, err := s.GetKindByLibrary(ctx, q.LibraryID)
	if err != nil {
		return tally, err
	}
	keys, err := validateBrowseEmptyFields(rec.Decl, q.Empty)
	if err != nil {
		return tally, err
	}
	fields := browsePresenceFields(rec.Decl)
	args := []any{int64(q.LibraryID), ""}
	for _, key := range keys {
		if field, ok := fields[key]; ok {
			args = append(args, field.key)
		}
	}
	if len(args) == 2 {
		return tally, nil
	}

	where := "library_id = ? AND field IN (" + store.Placeholders(len(args)-1) + ")"
	switch {
	case len(q.Levels) > 0:
		where += " AND level IN (" + store.Placeholders(len(q.Levels)) + ")"
		for _, lv := range q.Levels {
			args = append(args, strings.TrimSpace(lv))
		}
	case level != "":
		where += " AND level = ?"
		args = append(args, level)
	case !q.AnyLevel && q.PersonID <= 0:
		where += " AND is_root = 1"
	}
	args = append(args, int64(q.LibraryID))
	rows, err := s.db.QueryContext(ctx, `
WITH counts AS (
	SELECT field, SUM(n) AS n, SUM(text_n) AS text_n FROM plugin_item_field_presence
	WHERE `+where+` GROUP BY field
), coverage AS (
	SELECT NOT EXISTS (SELECT 1 FROM plugin_items WHERE library_id = ? AND presence_counted = 0) AS complete
)
SELECT counts.field, counts.n, counts.text_n, coverage.complete
FROM coverage LEFT JOIN counts ON 1 = 1`, args...)
	if err != nil {
		return tally, err
	}
	defer rows.Close()
	counts := make(map[string]fieldPresenceCount, len(args))
	var complete bool
	for rows.Next() {
		var field sql.NullString
		var n, textN sql.NullInt64
		if err := rows.Scan(&field, &n, &textN, &complete); err != nil {
			return tally, err
		}
		if field.Valid {
			counts[field.String] = fieldPresenceCount{n.Int64, textN.Int64}
		}
	}
	if err := rows.Err(); err != nil {
		return tally, err
	}

	total := counts[""].n
	for _, key := range keys {
		field, ok := fields[key]
		if !ok {
			continue
		}
		count := counts[field.key]
		matches := count.n
		if field.textOnly {
			matches = count.textN
		}
		if q.Empty[key] {
			matches = total - matches
		}
		// A small matching set is cheaper to filter before sorting. Keep
		// the ordered walk for common matches and small, complete scopes.
		if (!complete && q.Empty[key]) || (complete && total > 128 && matches <= total/8) {
			if tally.emptyMatchFirst == nil {
				tally.emptyMatchFirst = make(map[string]bool)
			}
			tally.emptyMatchFirst[key] = true
		}
		if complete && len(keys) == 1 && q.Policy.Passive() && q.TabPolicy.Passive() && !q.Hidden &&
			q.PersonID <= 0 && q.Watched == nil && q.MaxContentRating == "" && len(q.ExcludeKeys) == 0 &&
			!hasActiveExcludeQuery(q.ExcludeQuery) && !hasActiveFacets(q.Facets) && !hasActiveFacets(q.AllFacets) &&
			!hasActiveFacets(q.ExcludeFacets) && !hasActiveAnyGroups(q.AnyGroups) {
			tally.totalExact, tally.total = true, int(matches)
		}
	}
	return tally, nil
}
