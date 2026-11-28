package pluginitems

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/store"
)

func facetTallyTestDecl() plugins.MediaKindMeta {
	return plugins.MediaKindMeta{
		Key:   "catalogue",
		Label: "Catalogue",
		Levels: []plugins.KindLevel{
			{Key: "entry", Label: "Entry", Playable: true},
			{Key: "bonus", Label: "Bonus", Playable: true},
		},
		Fields: []plugins.KindField{
			{Key: "status", Type: "enum", Label: "Status", Indexed: true, Enum: []string{"active", "archived"}},
			{Key: "tags", Type: "strings", Label: "Tags", Indexed: true},
			{Key: "cast", Type: "people", Label: "Cast"},
			{Key: "directors", Type: "people", Label: "Directors"},
		},
		Browse: plugins.KindBrowseDecl{
			Sorts:        []string{"title", "added", "released", "year", "rating"},
			Facets:       []string{"status", "tags", "cast", "directors"},
			Search:       true,
			SearchFields: []string{"tags"},
		},
	}
}

func singleLevelTallyDecl() plugins.MediaKindMeta {
	return plugins.MediaKindMeta{
		Key:   "clips",
		Label: "Clips",
		Levels: []plugins.KindLevel{
			{Key: "clip", Label: "Clip", Playable: true},
		},
		Fields: []plugins.KindField{
			{Key: "category", Type: "enum", Label: "Category", Indexed: true, Enum: []string{"alpha", "beta", "gamma"}},
			{Key: "tags", Type: "strings", Label: "Tags", Indexed: true},
			{Key: "cast", Type: "people", Label: "Cast"},
		},
		Browse: plugins.KindBrowseDecl{
			Sorts:        []string{"title", "added", "released", "year", "rating"},
			Facets:       []string{"category", "tags", "cast"},
			Search:       true,
			SearchFields: []string{"tags"},
		},
	}
}

func assertTalliesExact(t *testing.T, db *sql.DB, libID core.LibraryID, step string) {
	t.Helper()
	ctx := context.Background()

	type fKey struct{ level, field, text string }
	type fVal struct {
		cnt        int
		lower, fld string
		rep        tallyItemRank
	}
	wantField := map[fKey]fVal{}
	type pKey struct {
		level, field string
		vInt         int64
	}
	type pVal struct {
		label, lower, fld string
		cnt               int
		rep               tallyItemRank
		thumb             tallyItemRank
	}
	wantPerson := map[pKey]pVal{}

	var declJSON string
	var peopleFields []string
	if err := db.QueryRowContext(ctx, `SELECT decl FROM plugin_kinds WHERE library_id = ? LIMIT 1`, int64(libID)).Scan(&declJSON); err == nil {
		var decl plugins.MediaKindMeta
		if json.Unmarshal([]byte(declJSON), &decl) == nil {
			peopleFields = declaredPeopleFields(decl)
		}
	}

	fRows, err := db.QueryContext(ctx, `
SELECT i.level, f.field, f.v_text, f.v_int, i.item_ref, i.released, i.added_at, i.year, i.fields
FROM plugin_item_fields f
JOIN plugin_items i ON i.item_ref = f.item_ref
WHERE i.library_id = ? AND i.level <> '' AND f.v_text <> ''`, int64(libID))
	if err != nil {
		t.Fatalf("%s: ground-truth field query: %v", step, err)
	}
	for fRows.Next() {
		var lv, field, text, ref, fieldsJSON string
		var vInt, released, addedAt, yr int64
		if err := fRows.Scan(&lv, &field, &text, &vInt, &ref, &released, &addedAt, &yr, &fieldsJSON); err != nil {
			fRows.Close()
			t.Fatal(err)
		}
		lower := strings.ToLower(text)
		fold := FoldFieldText(text)
		rank := tallyItemRank{ref: ref, released: released, addedAt: addedAt, year: yr}
		for _, targetLv := range [2]string{lv, ""} {
			k := fKey{targetLv, field, text}
			cur := wantField[k]
			cur.cnt++
			cur.lower = lower
			cur.fld = fold
			if rank.beats(cur.rep) {
				cur.rep = rank
			}
			wantField[k] = cur
		}
		if core.IsPluginPersonID(vInt) {
			hasThumb := itemHasPersonThumb(fieldsJSON, peopleFields, vInt)
			for _, targetLv := range [2]string{lv, ""} {
				pk := pKey{targetLv, field, vInt}
				cur, exists := wantPerson[pk]
				if !exists || text < cur.label {
					cur.label = text
					cur.lower = lower
					cur.fld = fold
				}
				cur.cnt++
				if rank.beats(cur.rep) {
					cur.rep = rank
				}
				if hasThumb && rank.beats(cur.thumb) {
					cur.thumb = rank
				}
				wantPerson[pk] = cur
			}
		}
	}
	fRows.Close()

	gotField := map[fKey]fVal{}
	ftRows, err := db.QueryContext(ctx, `
SELECT level, field, v_text, v_lower, v_fold, cnt, rep_ref, rep_released, rep_added, rep_year
FROM plugin_item_field_tally
WHERE library_id = ?`, int64(libID))
	if err != nil {
		t.Fatalf("%s: field tally query: %v", step, err)
	}
	for ftRows.Next() {
		var lv, field, text, lower, fold string
		var cnt int
		var rep tallyItemRank
		if err := ftRows.Scan(&lv, &field, &text, &lower, &fold, &cnt, &rep.ref, &rep.released, &rep.addedAt, &rep.year); err != nil {
			ftRows.Close()
			t.Fatal(err)
		}
		gotField[fKey{lv, field, text}] = fVal{cnt: cnt, lower: lower, fld: fold, rep: rep}
	}
	ftRows.Close()
	if len(gotField) != len(wantField) {
		t.Fatalf("%s: field tally count = %d, want %d\ngot:  %v\nwant: %v", step, len(gotField), len(wantField), gotField, wantField)
	}
	for k, w := range wantField {
		if g := gotField[k]; g != w {
			t.Fatalf("%s: field tally[%+v] = %+v, want %+v", step, k, g, w)
		}
	}

	gotPerson := map[pKey]pVal{}
	ptRows, err := db.QueryContext(ctx, `
SELECT level, field, v_int, label, label_lower, label_fold, cnt,
       rep_ref, rep_released, rep_added, rep_year,
       thumb_ref, thumb_released, thumb_added, thumb_year
FROM plugin_item_person_tally
WHERE library_id = ?`, int64(libID))
	if err != nil {
		t.Fatalf("%s: person tally query: %v", step, err)
	}
	for ptRows.Next() {
		var lv, field, label, lower, fold string
		var vInt int64
		var cnt int
		var rep, thumb tallyItemRank
		if err := ptRows.Scan(&lv, &field, &vInt, &label, &lower, &fold, &cnt,
			&rep.ref, &rep.released, &rep.addedAt, &rep.year,
			&thumb.ref, &thumb.released, &thumb.addedAt, &thumb.year); err != nil {
			ptRows.Close()
			t.Fatal(err)
		}
		gotPerson[pKey{lv, field, vInt}] = pVal{label: label, lower: lower, fld: fold, cnt: cnt, rep: rep, thumb: thumb}
	}
	ptRows.Close()
	if len(gotPerson) != len(wantPerson) {
		t.Fatalf("%s: person tally count = %d, want %d\ngot:  %v\nwant: %v", step, len(gotPerson), len(wantPerson), gotPerson, wantPerson)
	}
	for k, w := range wantPerson {
		if g := gotPerson[k]; g != w {
			t.Fatalf("%s: person tally[%+v] = %+v, want %+v", step, k, g, w)
		}
	}
}

func TestFacetAndPersonTalliesInvariants(t *testing.T) {
	ctx := context.Background()
	s, db := openTestService(t, Options{})
	defer s.Close()

	const (
		pluginID = "test.tally"
		kindKey  = "catalogue"
		libID    = core.LibraryID(91)
	)
	if _, err := s.UpsertKind(ctx, pluginID, libID, facetTallyTestDecl(), false); err != nil {
		t.Fatal(err)
	}

	// Seed items with:
	// - three casing variants of the same person ("ALICE SMITH" < "Alice Smith" < "alice smith")
	// - diacritics on both text facets and people names
	// - same person credited in both cast and directors on e1 (multi-field SearchPeople distinct count)
	items := []core.KindItem{
		{
			ID:       "e1",
			Level:    "entry",
			Title:    "First Entry",
			Released: 1_700_000_300,
			Fields: map[string]json.RawMessage{
				"status":    rawJSON(t, "active"),
				"tags":      rawJSON(t, []string{"Café Noir", "Sci-Fi"}),
				"cast":      rawJSON(t, []map[string]any{{"name": "ALICE SMITH"}, {"name": "José García"}}),
				"directors": rawJSON(t, []map[string]any{{"name": "Alice Smith"}}),
			},
		},
		{
			ID:       "e2",
			Level:    "entry",
			Title:    "Second Entry",
			Released: 1_700_000_200,
			Fields: map[string]json.RawMessage{
				"status": rawJSON(t, "active"),
				"tags":   rawJSON(t, []string{"Sci-Fi", "Action"}),
				"cast":   rawJSON(t, []map[string]any{{"name": "Alice Smith"}, {"name": "Élodie Moreau"}}),
			},
		},
		{
			ID:       "e3",
			Level:    "entry",
			Title:    "Third Entry",
			Released: 1_700_000_100,
			Fields: map[string]json.RawMessage{
				"status": rawJSON(t, "archived"),
				"tags":   rawJSON(t, []string{"Action"}),
				"cast":   rawJSON(t, []map[string]any{{"name": "alice smith"}}),
			},
		},
		{
			ID:       "b1",
			Level:    "bonus",
			ParentID: "e1",
			Title:    "Bonus Clip",
			Released: 1_700_000_050,
			Fields: map[string]json.RawMessage{
				"status": rawJSON(t, "active"),
				"tags":   rawJSON(t, []string{"Sci-Fi"}),
				"cast":   rawJSON(t, []map[string]any{{"name": "Alice Smith"}}),
			},
		},
	}
	if _, err := s.UpsertItems(ctx, pluginID, kindKey, items); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "initial upsert")

	// Multi-field SearchPeople must deduplicate e1 (where Alice Smith is in both
	// cast and directors), counting 4 distinct items across entry+bonus (e1, e2, e3, b1).
	matches, err := s.SearchPeople(ctx, libID, nil, "alice", 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].CreditCount != 4 {
		t.Fatalf("SearchPeople(alice) = %+v, want 1 match with distinct count 4", matches)
	}
	aliceID := matches[0].ID

	// Diacritic-insensitive search on people and collections tallies.
	fp, err := s.FacetPeople(ctx, FacetPeopleQuery{LibraryID: libID, Level: "entry", FieldKey: "cast", Query: "jose", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(fp.Items) != 1 || fp.Items[0].Name != "José García" || fp.Items[0].Count != 1 {
		t.Fatalf("FacetPeople(jose) = %+v, want José García count=1", fp.Items)
	}
	fc, err := s.FacetCollections(ctx, FacetCollectionsQuery{LibraryID: libID, Level: "entry", FieldKey: "tags", Query: "cafe", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.Items) != 1 || fc.Items[0].ID != "Café Noir" || fc.Items[0].Count != 1 {
		t.Fatalf("FacetCollections(cafe) = %+v, want Café Noir count=1", fc.Items)
	}

	// FindPersonID must resolve via plugin_item_person_tally (level='').
	foundID, ok, err := s.FindPersonID(ctx, libID, nil, func(_ int64, name string) bool {
		return strings.EqualFold(name, "elodie moreau") || strings.EqualFold(name, "élodie moreau")
	})
	if err != nil || !ok || !core.IsPluginPersonID(foundID) {
		t.Fatalf("FindPersonID(élodie) = (%d, %v, %v), want valid plugin person ID", foundID, ok, err)
	}

	// Casing-variant label refresh invariant:
	// Deleting e1 removes the ASCII-smallest spelling "ALICE SMITH" while
	// "Alice Smith" and "alice smith" still exist on e2/e3/b1.
	// Note: deleting e1 also cascades to child b1.
	if err := s.DeleteItems(ctx, pluginID, kindKey, []string{"e1"}); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "after deleting e1 (MIN label shifts to Alice Smith)")

	var curLabel string
	var curCnt int
	if err := db.QueryRowContext(ctx, `SELECT label, cnt FROM plugin_item_person_tally WHERE library_id = ? AND level = 'entry' AND field = 'cast' AND v_int = ?`, int64(libID), aliceID).Scan(&curLabel, &curCnt); err != nil {
		t.Fatal(err)
	}
	if curLabel != "Alice Smith" || curCnt != 2 {
		t.Fatalf("after e1 delete: person tally = (%q, %d), want (\"Alice Smith\", 2)", curLabel, curCnt)
	}

	// Updating e2 to remove Alice Smith leaves only "alice smith" on e3.
	if _, err := s.UpsertItems(ctx, pluginID, kindKey, []core.KindItem{
		{
			ID:       "e2",
			Level:    "entry",
			Title:    "Second Entry",
			Released: 1_700_000_200,
			Fields: map[string]json.RawMessage{
				"status": rawJSON(t, "active"),
				"tags":   rawJSON(t, []string{"Action"}),
				"cast":   rawJSON(t, []map[string]any{{"name": "Élodie Moreau"}}),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "after updating e2 (MIN label shifts to alice smith)")

	if err := db.QueryRowContext(ctx, `SELECT label, cnt FROM plugin_item_person_tally WHERE library_id = ? AND level = 'entry' AND field = 'cast' AND v_int = ?`, int64(libID), aliceID).Scan(&curLabel, &curCnt); err != nil {
		t.Fatal(err)
	}
	if curLabel != "alice smith" || curCnt != 1 {
		t.Fatalf("after e2 update: person tally = (%q, %d), want (\"alice smith\", 1)", curLabel, curCnt)
	}

	// Deleting e3 removes the last spelling; row must be pruned from both level='entry' and level=''.
	if err := s.DeleteItems(ctx, pluginID, kindKey, []string{"e3"}); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "after deleting e3 (person tally pruned)")

	// Chunked reindex preserves exact field and person tallies.
	decl2 := facetTallyTestDecl()
	if err := s.reindexKindChunked(ctx, store.DialectOf(db) == store.Postgres, pluginID, kindKey, libID, decl2); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "after chunked reindex")

	// PurgeKind removes all field and person tallies for the library.
	if err := s.PurgeKind(ctx, pluginID, kindKey); err != nil {
		t.Fatal(err)
	}
	var remField, remPerson int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_item_field_tally WHERE library_id = ?`, int64(libID)).Scan(&remField); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_item_person_tally WHERE library_id = ?`, int64(libID)).Scan(&remPerson); err != nil {
		t.Fatal(err)
	}
	if remField != 0 || remPerson != 0 {
		t.Fatalf("after PurgeKind: remaining field=%d person=%d, want 0/0", remField, remPerson)
	}
}

func TestMigration17BackfillFromPre17Schema(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)

	const (
		pluginID = "test.mig17"
		kindKey  = "catalogue"
		libID    = core.LibraryID(92)
	)
	decl := facetTallyTestDecl()
	declJSON, err := json.Marshal(decl)
	if err != nil {
		t.Fatal(err)
	}

	// Restore the pre-17 schema used by this fixture.
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS plugin_item_person_tally`,
		`DROP INDEX IF EXISTS plugin_item_field_tally_count`,
		`DROP INDEX IF EXISTS plugin_item_field_tally_name`,
		`DROP TABLE IF EXISTS plugin_item_field_tally`,
		`CREATE TABLE plugin_item_field_tally (
			library_id BIGINT  NOT NULL,
			level      TEXT    NOT NULL,
			field      TEXT    NOT NULL,
			v_text     TEXT    NOT NULL,
			cnt        BIGINT  NOT NULL,
			PRIMARY KEY (library_id, level, field, v_text)
		)`,
		`CREATE INDEX plugin_item_field_tally_count ON plugin_item_field_tally(library_id, level, field, cnt DESC, v_text)`,
		`DROP INDEX IF EXISTS plugin_items_recording`,
		`ALTER TABLE plugin_items DROP COLUMN recording`,
		`ALTER TABLE plugin_facet_expansions DROP COLUMN decl_hash`,
		`DROP INDEX plugin_items_presence_pending`,
		`DROP TABLE plugin_item_field_presence`,
		`ALTER TABLE plugin_items DROP COLUMN presence_counted`,
		`ALTER TABLE plugin_sync_state DROP COLUMN failure_json`,
		`DELETE FROM schema_migrations WHERE component = 'pluginitems' AND version >= 17`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("rollback pre-17 (%s): %v", stmt, err)
		}
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO plugin_kinds(plugin_id, kind_key, library_id, decl, decl_hash, schema_version, fts_version)
		VALUES (?, ?, ?, ?, ?, 1, 2)
	`, pluginID, kindKey, int64(libID), string(declJSON), decl.DeclHash()); err != nil {
		t.Fatal(err)
	}

	// Seed pre-17 plugin_items, plugin_item_fields, plugin_item_tally, and pre-17 plugin_item_field_tally.
	alicePID := core.PluginPersonID("Alice Smith")
	josePID := core.PluginPersonID("José García")
	ref1 := string(core.NewItemRef(pluginID, kindKey, "i1"))
	ref2 := string(core.NewItemRef(pluginID, kindKey, "i2"))
	for idx, row := range []struct {
		ref      string
		seq      int64
		ext      string
		released int64
		fields   string
	}{
		{ref1, 101, "i1", 1_700_000_200, `{"tags":["Café Noir","Sci-Fi"],"cast":[{"name":"ALICE SMITH"},{"name":"José García"}]}`},
		{ref2, 102, "i2", 1_700_000_100, `{"tags":["Sci-Fi"],"cast":[{"name":"alice smith"}]}`},
	} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO plugin_items(
				item_ref, seq, plugin_id, kind_key, library_id, external_id, parent_ref,
				level, idx, title, sort_title, summary, year, released, rating, playable,
				child_count, leaf_count, hints, fields, etag, added_at, updated_at, seen_at, missed_walks, unavailable
			) VALUES (?, ?, ?, ?, ?, ?, '', 'entry', ?, ?, ?, '', 2024, ?, 8.0, 1, 0, 0, '{}', ?, '', 1700000000, 1700000000, 1700000000, 0, '')
		`, row.ref, row.seq, pluginID, kindKey, int64(libID), row.ext, idx, "Title "+row.ext, "title "+row.ext, row.released, row.fields); err != nil {
			t.Fatal(err)
		}
	}
	fieldSeed := []struct {
		ref, field, vText, vFold string
		vInt                     int64
	}{
		{ref1, "tags", "Café Noir", FoldFieldText("Café Noir"), 0},
		{ref1, "tags", "Sci-Fi", FoldFieldText("Sci-Fi"), 0},
		{ref2, "tags", "Sci-Fi", FoldFieldText("Sci-Fi"), 0},
		{ref1, "cast", "ALICE SMITH", FoldFieldText("ALICE SMITH"), alicePID},
		{ref2, "cast", "alice smith", FoldFieldText("alice smith"), alicePID},
		{ref1, "cast", "José García", FoldFieldText("José García"), josePID},
	}
	for _, f := range fieldSeed {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO plugin_item_fields(item_ref, field, v_text, v_int, v_real, v_fold)
			VALUES (?, ?, ?, ?, 0, ?)
		`, f.ref, f.field, f.vText, f.vInt, f.vFold); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO plugin_item_tally(library_id, level, n) VALUES (?, 'entry', 2)`, int64(libID)); err != nil {
		t.Fatal(err)
	}
	for _, ft := range []struct {
		lv, field, text string
		cnt             int
	}{
		{"entry", "tags", "Café Noir", 1},
		{"", "tags", "Café Noir", 1},
		{"entry", "tags", "Sci-Fi", 2},
		{"", "tags", "Sci-Fi", 2},
		{"entry", "cast", "ALICE SMITH", 1},
		{"", "cast", "ALICE SMITH", 1},
		{"entry", "cast", "alice smith", 1},
		{"", "cast", "alice smith", 1},
		{"entry", "cast", "José García", 1},
		{"", "cast", "José García", 1},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO plugin_item_field_tally(library_id, level, field, v_text, cnt) VALUES (?, ?, ?, ?, ?)`, int64(libID), ft.lv, ft.field, ft.text, ft.cnt); err != nil {
			t.Fatal(err)
		}
	}

	// Re-run migrations to execute Migration 17 backfill + index creation.
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate(17): %v", err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "after Migration 17 backfill")

	s, err := New(ctx, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Verify tally-backed queries return exact results and use covering tally indexes with no TEMP B-TREE.
	peoplePage, err := s.FacetPeople(ctx, FacetPeopleQuery{LibraryID: libID, Level: "entry", FieldKey: "cast", Order: "count", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(peoplePage.Items) != 2 || peoplePage.Total != 2 || peoplePage.Items[0].PersonID != alicePID || peoplePage.Items[0].Count != 2 {
		t.Fatalf("FacetPeople after mig17 = %+v, want Alice(2), José(1)", peoplePage)
	}

	collPage, err := s.FacetCollections(ctx, FacetCollectionsQuery{LibraryID: libID, Level: "entry", FieldKey: "tags", Order: "name", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(collPage.Items) != 2 || collPage.Items[0].ID != "Café Noir" || collPage.Items[1].ID != "Sci-Fi" {
		t.Fatalf("FacetCollections after mig17 = %+v, want [Café Noir, Sci-Fi]", collPage.Items)
	}

	buckets, err := s.FacetBuckets(ctx, libID, "entry", nil, "cast")
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 || buckets[0].Count != 2 {
		t.Fatalf("FacetBuckets(cast) = %+v, want 2 buckets led by count=2", buckets)
	}

	plans := []struct {
		name      string
		sql       string
		args      []any
		wantIndex string
	}{
		{
			name:      "people_name",
			sql:       `EXPLAIN QUERY PLAN SELECT v_int, label, cnt, rep_ref, thumb_ref FROM plugin_item_person_tally WHERE library_id = ? AND level = ? AND field = ? ORDER BY label_lower ASC, label ASC, v_int ASC LIMIT 101 OFFSET 0`,
			args:      []any{int64(libID), "entry", "cast"},
			wantIndex: "plugin_item_person_tally_name",
		},
		{
			name:      "people_count",
			sql:       `EXPLAIN QUERY PLAN SELECT v_int, label, cnt, rep_ref, thumb_ref FROM plugin_item_person_tally WHERE library_id = ? AND level = ? AND field = ? ORDER BY cnt DESC, label_lower ASC, label ASC, v_int ASC LIMIT 101 OFFSET 0`,
			args:      []any{int64(libID), "entry", "cast"},
			wantIndex: "plugin_item_person_tally_count",
		},
		{
			name:      "collections_count",
			sql:       `EXPLAIN QUERY PLAN SELECT v_text, cnt, rep_ref FROM plugin_item_field_tally WHERE library_id = ? AND level = ? AND field = ? ORDER BY cnt DESC, v_lower ASC, v_text ASC LIMIT 101 OFFSET 0`,
			args:      []any{int64(libID), "entry", "tags"},
			wantIndex: "plugin_item_field_tally_count",
		},
		{
			name:      "collections_name",
			sql:       `EXPLAIN QUERY PLAN SELECT v_text, cnt, rep_ref FROM plugin_item_field_tally WHERE library_id = ? AND level = ? AND field = ? ORDER BY v_lower ASC, v_text ASC LIMIT 101 OFFSET 0`,
			args:      []any{int64(libID), "entry", "tags"},
			wantIndex: "plugin_item_field_tally_name",
		},
	}
	if store.DialectOf(db) == store.SQLite {
		for _, tc := range plans {
			plan := explainPlanText(t, db, tc.sql, tc.args...)
			if !strings.Contains(plan, "COVERING INDEX "+tc.wantIndex) {
				t.Errorf("%s: expected COVERING INDEX %s, got:\n%s", tc.name, tc.wantIndex, plan)
			}
			if strings.Contains(plan, "USE TEMP B-TREE") {
				t.Errorf("%s: unexpected TEMP B-TREE in plan:\n%s", tc.name, plan)
			}
		}
	}
}

func TestBrowseBatchMatchesBrowseAndCacheInvalidation(t *testing.T) {
	ctx := context.Background()
	s, db := openTestService(t, Options{})
	defer s.Close()

	const (
		pluginID = "test.batch"
		kindKey  = "clips"
		libID    = core.LibraryID(93)
	)
	decl := singleLevelTallyDecl()
	if _, err := s.UpsertKind(ctx, pluginID, libID, decl, false); err != nil {
		t.Fatal(err)
	}

	categories := []string{"alpha", "beta", "gamma"}
	items := make([]core.KindItem, 0, 80)
	for i := 0; i < 80; i++ {
		tags := []string{fmt.Sprintf("tag-%d", i%6)}
		if i%2 == 0 {
			tags = append(tags, "popular")
		}
		if i%3 == 0 {
			tags = append(tags, "featured")
		}
		items = append(items, core.KindItem{
			ID:       fmt.Sprintf("clip-%03d", i),
			Level:    "clip",
			Title:    fmt.Sprintf("Clip %03d", 100-i),
			AddedAt:  int64(1_700_000_000 + (i%17)*1000),
			Released: int64(1_600_000_000 + (i/2)*500),
			Year:     2015 + (i % 10),
			Rating:   float64(i%9) + 0.5,
			Playable: true,
			Art:      []core.KindArtRef{{Role: "poster", URL: fmt.Sprintf("https://img.example.test/%03d.jpg", i)}},
			Fields: map[string]json.RawMessage{
				"category": rawJSON(t, categories[i%len(categories)]),
				"tags":     rawJSON(t, tags),
			},
		})
	}
	if _, err := s.UpsertItems(ctx, pluginID, kindKey, items); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}

	// Verify single-level sort probe omits redundant level/parent_ref so SQLite uses
	// plugin_items_released directly as a covering index for multi-value strings OR,
	// including when Sort carries an explicit ":desc"/":asc" suffix (as KindCatalog emits).
	prevMin := largeFacetMinMatches
	largeFacetMinMatches = 10
	defer func() { largeFacetMinMatches = prevMin }()

	multiQ := BrowseQuery{
		LibraryID: libID,
		Level:     "clip",
		Sort:      "released:desc",
		Facets:    map[string][]string{"tags": {"popular", "featured"}},
		NoTotal:   true,
		Limit:     1,
	}
	col, kind, desc, defDesc := parseBrowseSort(multiQ)
	if !desc || !defDesc || col != "released" {
		t.Fatalf("parseBrowseSort(released:desc) = (col=%q kind=%q desc=%v defaultDesc=%v), want released/true/true", col, kind, desc, defDesc)
	}
	multiTally, err := s.browseTallyProbe(ctx, multiQ, "", "clip")
	if err != nil {
		t.Fatal(err)
	}
	if !shouldDriveFacetFromSortIndex(multiTally, col, kind, desc, defDesc, "clip", nil, false, multiQ.Limit) {
		t.Fatalf("expected multi-value strings OR with released:desc to drive from sort index (tally=%+v)", multiTally)
	}
	bw, bArgs, _, empty, err := s.buildBrowsePredicates(ctx, multiQ, "", "clip", col, kind, desc, defDesc, multiQ.Limit, multiTally)
	if err != nil || empty {
		t.Fatalf("buildBrowsePredicates: empty=%v err=%v", empty, err)
	}
	if store.DialectOf(db) == store.SQLite {
		multiPlan := explainPlanText(t, db, `EXPLAIN QUERY PLAN SELECT i.item_ref FROM plugin_items i WHERE `+bw+` ORDER BY i.released DESC, i.item_ref ASC LIMIT 2`, bArgs...)
		if !strings.Contains(multiPlan, "COVERING INDEX plugin_items_released") || strings.Contains(multiPlan, "USE TEMP B-TREE") {
			t.Fatalf("expected covering plugin_items_released scan without TEMP B-TREE for multi-value strings OR, got:\n%s", multiPlan)
		}
	}

	queries := []BrowseQuery{
		{LibraryID: libID, Level: "clip", Sort: "released:desc", NoTotal: true, Limit: 12},
		{LibraryID: libID, Level: "clip", Sort: "added:desc", Facets: map[string][]string{"category": {"alpha"}}, NoTotal: true, Limit: 10},
		{LibraryID: libID, Level: "clip", Sort: "released:desc", Facets: map[string][]string{"tags": {"popular", "featured"}}, NoTotal: true, Limit: 12},
		{LibraryID: libID, Level: "clip", Sort: "rating:desc", Facets: map[string][]string{"tags": {"tag-1", "tag-2"}}, NoTotal: true, Limit: 8},
		{LibraryID: libID, Level: "clip", Sort: "title:asc", Facets: map[string][]string{"tags": {"popular"}}, NoTotal: true, Limit: 10},
		{LibraryID: libID, Level: "clip", Sort: "year:desc", Facets: map[string][]string{"tags": {"nonexistent-1", "nonexistent-2"}}, NoTotal: true, Limit: 10},
		// Mixed fallback query (NoTotal: false) in the same batch.
		{LibraryID: libID, Level: "clip", Sort: "released:desc", Facets: map[string][]string{"category": {"beta"}}, NoTotal: false, Limit: 5},
	}

	batchPages, err := s.BrowseBatch(ctx, queries)
	if err != nil {
		t.Fatalf("BrowseBatch: %v", err)
	}
	if len(batchPages) != len(queries) {
		t.Fatalf("BrowseBatch len = %d, want %d", len(batchPages), len(queries))
	}
	for i, q := range queries {
		single, err := s.Browse(ctx, q)
		if err != nil {
			t.Fatalf("Browse[%d]: %v", i, err)
		}
		gotIDs := make([]string, len(batchPages[i].Items))
		wantIDs := make([]string, len(single.Items))
		for j := range batchPages[i].Items {
			gotIDs[j] = batchPages[i].Items[j].ID
		}
		for j := range single.Items {
			wantIDs[j] = single.Items[j].ID
		}
		if !slices.Equal(gotIDs, wantIDs) {
			t.Fatalf("shelf %d item IDs mismatch:\nbatch:  %v\nsingle: %v", i, gotIDs, wantIDs)
		}
		if batchPages[i].NextCursor != single.NextCursor {
			t.Fatalf("shelf %d NextCursor = %q, want %q", i, batchPages[i].NextCursor, single.NextCursor)
		}
		for j := range batchPages[i].Items {
			if !slices.Equal(batchPages[i].Items[j].Art, single.Items[j].Art) {
				t.Fatalf("shelf %d item %d art = %+v, want %+v", i, j, batchPages[i].Items[j].Art, single.Items[j].Art)
			}
		}
	}

	// CountPartsMany empty-table fast path transitions cleanly through empty -> populated -> empty.
	ref0 := core.NewItemRef(pluginID, kindKey, "clip-000")
	counts, err := s.CountPartsMany(ctx, []core.ItemRef{ref0})
	if err != nil || len(counts) != 0 {
		t.Fatalf("empty CountPartsMany = (%v, %v), want empty", counts, err)
	}
	if _, err := s.SaveParts(ctx, ref0, []core.KindItemPart{{Name: "Part 1"}, {Name: "Part 2"}}, "test"); err != nil {
		t.Fatal(err)
	}
	counts, err = s.CountPartsMany(ctx, []core.ItemRef{ref0})
	if err != nil || counts[ref0] != 2 {
		t.Fatalf("populated CountPartsMany = (%v, %v), want 2", counts, err)
	}
	if _, err := s.SaveParts(ctx, ref0, nil, "test"); err != nil {
		t.Fatal(err)
	}
	counts, err = s.CountPartsMany(ctx, []core.ItemRef{ref0})
	if err != nil || len(counts) != 0 {
		t.Fatalf("cleared CountPartsMany = (%v, %v), want empty", counts, err)
	}

	// KindRecord cache invalidates immediately on UpsertKind and PurgeKind.
	rec1, err := s.GetKindByLibrary(ctx, libID)
	if err != nil || rec1.Decl.Label != "Clips" {
		t.Fatalf("initial GetKindByLibrary = (%+v, %v)", rec1.Decl.Label, err)
	}
	declUpdated := decl
	declUpdated.Label = "Clips Updated"
	if _, err := s.UpsertKind(ctx, pluginID, libID, declUpdated, false); err != nil {
		t.Fatal(err)
	}
	rec2, err := s.GetKindByLibrary(ctx, libID)
	if err != nil || rec2.Decl.Label != "Clips Updated" {
		t.Fatalf("updated GetKindByLibrary = (%q, %v), want Clips Updated", rec2.Decl.Label, err)
	}
	if err := s.PurgeKind(ctx, pluginID, kindKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetKindByLibrary(ctx, libID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("after PurgeKind GetKindByLibrary err = %v, want core.ErrNotFound", err)
	}
}

func TestHybridLargeBucketNewestResolution(t *testing.T) {
	ctx := context.Background()
	s, db := openTestService(t, Options{})
	defer s.Close()

	const (
		pluginID = "test.hybrid"
		kindKey  = "clips"
		libID    = core.LibraryID(94)
	)
	if _, err := s.UpsertKind(ctx, pluginID, libID, singleLevelTallyDecl(), false); err != nil {
		t.Fatal(err)
	}

	// Lower largeFacetMinMatches so "huge-tag" (60/80 items) and "Star Lead" (60/80 items)
	// trigger the sort-index probe path while "rare-tag" and "Guest Actor" use ROW_NUMBER().
	prevMin := largeFacetMinMatches
	largeFacetMinMatches = 20
	defer func() { largeFacetMinMatches = prevMin }()

	items := make([]core.KindItem, 0, 80)
	for i := 0; i < 80; i++ {
		tags := []string{"rare-tag"}
		cast := []map[string]any{{"name": fmt.Sprintf("Guest %02d", i%15)}}
		if i < 60 {
			tags = []string{"huge-tag"}
			entry := map[string]any{"name": "Star Lead"}
			// Only the 2nd-newest Star Lead item carries a portrait thumb so
			// FacetPeople's 4-candidate probe must inspect past rank 1.
			if i == 58 {
				entry["thumb"] = "https://img.example.test/star-thumb.jpg"
			}
			cast = []map[string]any{entry}
		}
		items = append(items, core.KindItem{
			ID:       fmt.Sprintf("item-%03d", i),
			Level:    "clip",
			Title:    fmt.Sprintf("Item %03d", i),
			Released: int64(1_700_000_000 + i*100),
			Playable: true,
			Art:      []core.KindArtRef{{Role: "poster", URL: fmt.Sprintf("https://img.example.test/p-%03d.jpg", i)}},
			Fields: map[string]json.RawMessage{
				"category": rawJSON(t, "alpha"),
				"tags":     rawJSON(t, tags),
				"cast":     rawJSON(t, cast),
			},
		})
	}
	if _, err := s.UpsertItems(ctx, pluginID, kindKey, items); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}

	// FacetCollections text field: huge-tag (probe) + rare-tag (ROW_NUMBER) in one page.
	fcText, err := s.FacetCollections(ctx, FacetCollectionsQuery{
		LibraryID: libID,
		Level:     "clip",
		FieldKey:  "tags",
		Order:     "count",
		Limit:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fcText.Items) != 2 {
		t.Fatalf("FacetCollections(tags) len = %d, want 2", len(fcText.Items))
	}
	if fcText.Items[0].ID != "huge-tag" || fcText.Items[0].NewestItem == nil || fcText.Items[0].NewestItem.ID != "item-059" || len(fcText.Items[0].NewestItem.Art) == 0 || fcText.Items[0].NewestItem.Art[0].URL != "https://img.example.test/p-059.jpg" {
		t.Fatalf("huge-tag winner = %+v, want item-059 with poster", fcText.Items[0])
	}
	if fcText.Items[1].ID != "rare-tag" || fcText.Items[1].NewestItem == nil || fcText.Items[1].NewestItem.ID != "item-079" || len(fcText.Items[1].NewestItem.Art) == 0 || fcText.Items[1].NewestItem.Art[0].URL != "https://img.example.test/p-079.jpg" {
		t.Fatalf("rare-tag winner = %+v, want item-079 with poster", fcText.Items[1])
	}

	// FacetCollections people field: Star Lead (probe) + guests (ROW_NUMBER).
	fcPeople, err := s.FacetCollections(ctx, FacetCollectionsQuery{
		LibraryID: libID,
		Level:     "clip",
		FieldKey:  "cast",
		Order:     "count",
		Limit:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fcPeople.Items) < 2 || fcPeople.Items[0].Label != "Star Lead" || fcPeople.Items[0].NewestItem == nil || fcPeople.Items[0].NewestItem.ID != "item-059" {
		t.Fatalf("FacetCollections(cast) lead = %+v, want Star Lead -> item-059", fcPeople.Items[0])
	}

	// FacetPeople: Star Lead (large bucket, 4-candidate probe) must find the portrait thumb on item-058.
	fp, err := s.FacetPeople(ctx, FacetPeopleQuery{
		LibraryID: libID,
		Level:     "clip",
		FieldKey:  "cast",
		Order:     "count",
		Limit:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fp.Items) < 2 || fp.Items[0].Name != "Star Lead" || fp.Items[0].Thumb != "https://img.example.test/star-thumb.jpg" || fp.Items[0].SampleItem == nil || fp.Items[0].SampleItem.ID != "item-058" {
		t.Fatalf("FacetPeople(cast) lead = %+v, want Star Lead thumb from item-058", fp.Items[0])
	}
}

func TestTallyRepresentativeMaintenanceInvariants(t *testing.T) {
	ctx := context.Background()
	s, db := openTestService(t, Options{})
	defer s.Close()

	const (
		pluginID = "test.games"
		kindKey  = "game"
		libID    = core.LibraryID(96)
	)
	decl := plugins.MediaKindMeta{
		Key:   kindKey,
		Label: "Games",
		Levels: []plugins.KindLevel{
			{Key: "game", Label: "Game", Playable: true},
		},
		Fields: []plugins.KindField{
			{Key: "genres", Type: "strings", Label: "Genres", Indexed: true},
			{Key: "cast", Type: "people", Label: "Cast"},
			{Key: "directors", Type: "people", Label: "Directors"},
		},
		Browse: plugins.KindBrowseDecl{
			Sorts:  []string{"title", "added", "released", "year"},
			Facets: []string{"genres", "cast", "directors"},
		},
	}
	if _, err := s.UpsertKind(ctx, pluginID, libID, decl, false); err != nil {
		t.Fatal(err)
	}

	// Seed items testing:
	// - tie-breaking on (released DESC, added_at DESC, year DESC, item_ref ASC)
	// - cross-field thumb inheritance (g2 has cast without thumb + directors with thumb)
	// - large-bucket (>512) recompute via sort-index probe on delete
	items := []core.KindItem{
		{
			ID:       "g1",
			Level:    "game",
			Title:    "Alpha Quest",
			Year:     2020,
			Released: 1_700_000_100,
			Playable: true,
			Art:      []core.KindArtRef{{Role: "poster", URL: "https://img.example.test/g1.jpg"}},
			Fields: map[string]json.RawMessage{
				"genres": rawJSON(t, []string{"RPG"}),
				"cast":   rawJSON(t, []map[string]any{{"name": "Jordan Lee", "thumb": "https://img.example.test/jordan-old.jpg"}}),
			},
		},
		{
			ID:       "g2",
			Level:    "game",
			Title:    "Beta Quest",
			Year:     2022,
			Released: 1_700_000_200,
			Playable: true,
			Art:      []core.KindArtRef{{Role: "poster", URL: "https://img.example.test/g2.jpg"}},
			Fields: map[string]json.RawMessage{
				"genres":    rawJSON(t, []string{"RPG"}),
				"cast":      rawJSON(t, []map[string]any{{"name": "Jordan Lee"}}),
				"directors": rawJSON(t, []map[string]any{{"name": "Jordan Lee", "thumb": "https://img.example.test/jordan-dir.jpg"}}),
			},
		},
		{
			ID:       "g3",
			Level:    "game",
			Title:    "Gamma Quest",
			Year:     2024,
			Released: 1_700_000_300,
			Playable: true,
			Art:      []core.KindArtRef{{Role: "poster", URL: "https://img.example.test/g3.jpg"}},
			Fields: map[string]json.RawMessage{
				"genres": rawJSON(t, []string{"RPG"}),
				"cast":   rawJSON(t, []map[string]any{{"name": "Jordan Lee"}}),
			},
		},
	}
	if _, err := s.UpsertItems(ctx, pluginID, kindKey, items); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "initial seed")

	// FacetCollections uses rep_ref (g3), while FacetPeople uses thumb_ref (g2 via directors thumb).
	fc, err := s.FacetCollections(ctx, FacetCollectionsQuery{LibraryID: libID, Level: "game", FieldKey: "genres", Order: "count", Limit: 10})
	if err != nil || len(fc.Items) != 1 || fc.Items[0].NewestItem == nil || fc.Items[0].NewestItem.ID != "g3" {
		t.Fatalf("FacetCollections(genres) = %+v, err=%v, want g3", fc.Items, err)
	}
	fp, err := s.FacetPeople(ctx, FacetPeopleQuery{LibraryID: libID, Level: "game", FieldKey: "cast", Order: "name", Limit: 10})
	if err != nil || len(fp.Items) != 1 || fp.Items[0].Thumb != "https://img.example.test/jordan-dir.jpg" || fp.Items[0].SampleItem == nil || fp.Items[0].SampleItem.ID != "g2" {
		t.Fatalf("FacetPeople(cast) = %+v, err=%v, want g2 thumb", fp.Items, err)
	}

	// Update g3: lower its released date AND attach a portrait thumb (rank decreased while gaining thumb eligibility).
	g3Updated := items[2]
	g3Updated.Released = 1_700_000_250
	g3Updated.Fields = map[string]json.RawMessage{
		"genres": rawJSON(t, []string{"RPG"}),
		"cast":   rawJSON(t, []map[string]any{{"name": "Jordan Lee", "thumb": "https://img.example.test/jordan-new.jpg"}}),
	}
	if _, err := s.UpsertItems(ctx, pluginID, kindKey, []core.KindItem{g3Updated}); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "g3 demoted while gaining thumb")

	// Demote g3 below g2 (g2 becomes rep_ref and keeps thumb_ref; g3 drops thumb).
	g3Updated.Released = 1_700_000_150
	g3Updated.Fields = map[string]json.RawMessage{
		"genres": rawJSON(t, []string{"RPG"}),
		"cast":   rawJSON(t, []map[string]any{{"name": "Jordan Lee"}}),
	}
	if _, err := s.UpsertItems(ctx, pluginID, kindKey, []core.KindItem{g3Updated}); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "g3 demoted below g2 and thumb removed")

	// Remove thumb from g2 without changing its released rank -> thumb_ref falls back to g1.
	g2Updated := items[1]
	g2Updated.Fields = map[string]json.RawMessage{
		"genres":    rawJSON(t, []string{"RPG"}),
		"cast":      rawJSON(t, []map[string]any{{"name": "Jordan Lee"}}),
		"directors": rawJSON(t, []map[string]any{{"name": "Jordan Lee"}}),
	}
	if _, err := s.UpsertItems(ctx, pluginID, kindKey, []core.KindItem{g2Updated}); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "g2 thumb removed -> fallback to g1")

	// Promote g1 above g2 (rank increase without recompute).
	g1Updated := items[0]
	g1Updated.Released = 1_700_000_500
	if _, err := s.UpsertItems(ctx, pluginID, kindKey, []core.KindItem{g1Updated}); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "g1 promoted above g2")

	// Delete g1 (removes both rep_ref and last thumb_ref -> thumb_ref clears to "").
	if err := s.KindDelete(ctx, pluginID, kindKey, []string{"g1"}); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "g1 deleted (last thumb removed)")

	// Exercise the large-bucket (>= 512) sort-index recompute path on delete for
	// both field rep_ref and person rep_ref/thumb_ref, including equal-released ties.
	bulk := make([]core.KindItem, 520)
	for i := range bulk {
		rel := int64(1_710_000_000 + i*10)
		yr := 2020
		if i >= 517 {
			// 3-way tie on released broken by year DESC then item_ref ASC.
			rel = 1_710_010_000
			yr = 2020 + (i - 517)
		}
		castObj := map[string]any{"name": "Jordan Lee"}
		if i%2 == 0 {
			castObj["thumb"] = "https://img.example.test/jordan-bulk.jpg"
		}
		bulk[i] = core.KindItem{
			ID:       fmt.Sprintf("bulk-%04d", i),
			Level:    "game",
			Title:    fmt.Sprintf("Bulk Game %04d", i),
			Year:     yr,
			Released: rel,
			Playable: true,
			Fields: map[string]json.RawMessage{
				"genres": rawJSON(t, []string{"RPG"}),
				"cast":   rawJSON(t, []map[string]any{castObj}),
			},
		}
	}
	if _, err := s.UpsertItems(ctx, pluginID, kindKey, bulk); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "after 520 bulk inserts")

	// Delete the top 2 items (triggers recomputeFieldRepTx, recomputePersonRepTx,
	// and recomputePersonThumbTx large-bucket sort-index probes + tie-breaking).
	if err := s.KindDelete(ctx, pluginID, kindKey, []string{"bulk-0519", "bulk-0518"}); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "after deleting top 2 from 520-item bucket")

	// Verify tally-backed LookupPersonFacetsByID and PersonByID match ground truth.
	jordanID := core.PluginPersonID("Jordan Lee")
	matches, err := s.LookupPersonFacetsByID(ctx, libID, []string{"cast", "directors"}, jordanID)
	if err != nil || len(matches) != 2 {
		t.Fatalf("LookupPersonFacetsByID = %+v, err=%v, want 2 fields", matches, err)
	}
	pm, ok, err := s.PersonByID(ctx, libID, nil, jordanID, 5)
	if err != nil || !ok || pm.CreditCount != 520 || pm.Roles["actor"] != 520 || pm.Roles["director"] != 1 {
		t.Fatalf("PersonByID = (%+v, ok=%v, err=%v), want count=520 actor=520 director=1", pm, ok, err)
	}

	// #805: the recompute walks a bounded window of the released index from
	// the removed representative down. Put 300 newer items that do not hold
	// RPG or Jordan Lee above the bucket, then one newest holder on top:
	// removing it walks a window of non-holders and must fall back to the
	// index-first plan; removing the next representative must start below
	// the 300 non-holders. Both must leave every representative exact.
	filler := make([]core.KindItem, 0, 301)
	for i := range 300 {
		filler = append(filler, core.KindItem{
			ID:       fmt.Sprintf("filler-%04d", i),
			Level:    "game",
			Title:    fmt.Sprintf("Filler %04d", i),
			Year:     2025,
			Released: int64(1_720_000_000 + i*10),
			Playable: true,
			Fields:   map[string]json.RawMessage{"genres": rawJSON(t, []string{"Puzzle"})},
		})
	}
	filler = append(filler, core.KindItem{
		ID:       "top",
		Level:    "game",
		Title:    "Top Quest",
		Year:     2026,
		Released: 1_730_000_000,
		Playable: true,
		Fields: map[string]json.RawMessage{
			"genres": rawJSON(t, []string{"RPG"}),
			"cast":   rawJSON(t, []map[string]any{{"name": "Jordan Lee", "thumb": "https://img.example.test/jordan-top.jpg"}}),
		},
	})
	if _, err := s.UpsertItems(ctx, pluginID, kindKey, filler); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "after newer non-holders and a newest holder")
	if err := s.KindDelete(ctx, pluginID, kindKey, []string{"top"}); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "newest holder removed above a window of non-holders")
	if err := s.KindDelete(ctx, pluginID, kindKey, []string{"bulk-0517"}); err != nil {
		t.Fatal(err)
	}
	assertTalliesExact(t, db, libID, "representative removed below newer non-holders")
}
