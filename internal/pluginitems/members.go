package pluginitems

import (
	"context"
	"database/sql"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// A list item (a playlist, a chart: a level declared members: true) orders
// other items of its kind. Its members are stored by position and read in
// pages joined to the items, so a member not stored yet is simply skipped.
func init() {
	store.Register("pluginitems", 20, `
CREATE TABLE IF NOT EXISTS plugin_item_members (
  item_ref TEXT NOT NULL,
  position INTEGER NOT NULL,
  member_ref TEXT NOT NULL,
  PRIMARY KEY (item_ref, position)
);`)
}

// An item's recording identity (the same song on an album, a single and a
// compilation), indexed per library for duplicate lookups.
func init() {
	store.Register("pluginitems", 21, `
ALTER TABLE plugin_items ADD COLUMN recording TEXT NOT NULL DEFAULT '';
CREATE INDEX plugin_items_recording ON plugin_items(library_id, recording) WHERE recording <> '';`)
}

// How many items of a library carry each recording: duplicate review reads
// the recordings with more than one copy without touching plugin_items.
func init() {
	store.Register("pluginitems", 22, `
CREATE TABLE IF NOT EXISTS plugin_item_recording_tally (
  library_id BIGINT NOT NULL,
  recording  TEXT   NOT NULL,
  cnt        BIGINT NOT NULL,
  PRIMARY KEY (library_id, recording)
);
CREATE INDEX IF NOT EXISTS plugin_item_recording_tally_dupes ON plugin_item_recording_tally(library_id, cnt DESC, recording) WHERE cnt > 1;
DELETE FROM plugin_item_recording_tally;
INSERT INTO plugin_item_recording_tally(library_id, recording, cnt)
  SELECT library_id, recording, COUNT(*) FROM plugin_items WHERE recording <> '' GROUP BY library_id, recording;`)
}

// An expansion remembers the kind declaration it ran under: a newer plugin
// declaration makes it stale, so the next open expands again.
func init() {
	store.Register("pluginitems", 23, `ALTER TABLE plugin_facet_expansions ADD COLUMN decl_hash TEXT NOT NULL DEFAULT '';`)
}

// applyRecordingTallyDeltaTx adds each recording's change in copies.
func applyRecordingTallyDeltaTx(ctx context.Context, tx *sql.Tx, libraryID core.LibraryID, delta map[string]int64) error {
	for rec, d := range delta {
		if rec == "" || d == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_item_recording_tally(library_id, recording, cnt) VALUES(?, ?, ?)
			ON CONFLICT(library_id, recording) DO UPDATE SET cnt = plugin_item_recording_tally.cnt + excluded.cnt`, int64(libraryID), rec, d); err != nil {
			return err
		}
		if d < 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_recording_tally WHERE library_id = ? AND recording = ? AND cnt <= 0`, int64(libraryID), rec); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeMembersTx(ctx context.Context, tx *sql.Tx, isPG bool, ref core.ItemRef, count int, membersJSON string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_members WHERE item_ref = ? AND position >= ?`, string(ref), count); err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	members := `SELECT ?, CAST(m.key AS INTEGER), m.value FROM json_each(?) m WHERE TRUE`
	if isPG {
		members = `SELECT ?, m.ord - 1, m.value FROM jsonb_array_elements_text(CAST(? AS jsonb)) WITH ORDINALITY AS m(value, ord)`
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO plugin_item_members(item_ref, position, member_ref)
`+members+`
ON CONFLICT(item_ref, position) DO UPDATE SET member_ref = excluded.member_ref
WHERE plugin_item_members.member_ref <> excluded.member_ref`, string(ref), membersJSON)
	return err
}

// CountMembersMany counts the stored members of explicit lists in batches.
func (s *Service) CountMembersMany(ctx context.Context, refs []core.ItemRef) (map[core.ItemRef]int, error) {
	counts := make(map[core.ItemRef]int, len(refs))
	const chunkSize = 250
	for start := 0; start < len(refs); start += chunkSize {
		chunk := refs[start:min(start+chunkSize, len(refs))]
		args := make([]any, len(chunk))
		for i, ref := range chunk {
			args[i] = string(ref)
		}
		params := strings.Repeat("?,", len(args)-1) + "?"
		rows, err := s.db.QueryContext(ctx, `
			SELECT m.item_ref, COUNT(*) FROM plugin_item_members m
			JOIN plugin_items i ON i.item_ref = m.member_ref
			WHERE m.item_ref IN (`+params+`)
			GROUP BY m.item_ref`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var ref core.ItemRef
			var n int
			if err := rows.Scan(&ref, &n); err != nil {
				rows.Close()
				return nil, err
			}
			counts[ref] = n
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return counts, nil
}

// MembersPage lists a list item's stored members in order from offset, at
// most limit, and how many members it names. Members not stored (yet) are
// skipped; the count is of named members.
func (s *Service) MembersPage(ctx context.Context, listRef core.ItemRef, offset, limit int) ([]core.KindItem, int, error) {
	if s == nil || s.db == nil || listRef == "" {
		return nil, 0, nil
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_item_members WHERE item_ref = ?`, string(listRef)).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.member_ref FROM plugin_item_members m JOIN plugin_items i ON i.item_ref = m.member_ref
		WHERE m.item_ref = ? ORDER BY m.position LIMIT ? OFFSET ?`, string(listRef), limit, max(offset, 0))
	if err != nil {
		return nil, 0, err
	}
	var refs []core.ItemRef
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			rows.Close()
			return nil, 0, err
		}
		refs = append(refs, core.ItemRef(ref))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	items, err := s.fetchItemsByRefsWithArt(ctx, refs)
	if err != nil {
		return nil, 0, err
	}
	out := make([]core.KindItem, 0, len(refs))
	for _, ref := range refs {
		if item, ok := items[ref]; ok {
			out = append(out, item)
		}
	}
	return out, total, nil
}

// DuplicateGroup is one recording with more than one copy in a library.
type DuplicateGroup struct {
	Recording string
	Count     int
	Items     []core.KindItem
}

// maxRecordingCopies bounds the copies listed for one recording.
const maxRecordingCopies = 24

// Duplicates pages a library's recordings that have more than one copy,
// most copies first, with their items; total counts all such recordings.
// It reads the recording tally and one indexed lookup per group.
func (s *Service) Duplicates(ctx context.Context, libraryID core.LibraryID, offset, limit int) ([]DuplicateGroup, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_item_recording_tally WHERE library_id = ? AND cnt > 1`, int64(libraryID)).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT recording, cnt FROM plugin_item_recording_tally WHERE library_id = ? AND cnt > 1
		ORDER BY cnt DESC, recording LIMIT ? OFFSET ?`, int64(libraryID), limit, max(offset, 0))
	if err != nil {
		return nil, 0, err
	}
	var groups []DuplicateGroup
	for rows.Next() {
		var g DuplicateGroup
		if err := rows.Scan(&g.Recording, &g.Count); err != nil {
			rows.Close()
			return nil, 0, err
		}
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	for i := range groups {
		items, err := s.RecordingItems(ctx, libraryID, groups[i].Recording)
		if err != nil {
			return nil, 0, err
		}
		groups[i].Items = items
	}
	return groups, total, nil
}

// RecordingItems lists a library's copies of one recording (at most 24).
func (s *Service) RecordingItems(ctx context.Context, libraryID core.LibraryID, recording string) ([]core.KindItem, error) {
	if s == nil || s.db == nil || recording == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+itemCols+` FROM plugin_items WHERE library_id = ? AND recording = ? ORDER BY seq LIMIT ?`,
		int64(libraryID), recording, maxRecordingCopies)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.KindItem
	for rows.Next() {
		it, _, _, err := scanItemRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
