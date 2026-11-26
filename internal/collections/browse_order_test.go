package collections

import (
	"log/slog"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestIndexedBrowseOrderTracksGroupAnchorAndCursor(t *testing.T) {
	ctx := t.Context()
	s := &Service{db: stampsDB(t), now: time.Now, log: slog.Default()}
	insert := func(name, folder string, rank int, enabled bool, addon *core.CollectionAddonSync) *specRow {
		t.Helper()
		sp := core.CollectionSpec{Name: name, MediaType: MediaMovie, Folder: folder, SortOrder: rank, BrowseInApps: true, Enabled: enabled, AddonSync: addon}
		Normalize(&sp)
		row, err := s.insertRow(ctx, sp, 0)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	anchor := insert("anchor", "ÉTÉ", 0, false, nil)
	middle := insert("middle", "", 0, true, nil)
	last := insert("last", "été", 2, true, nil)
	first := insert("first", "Été", 1, true, nil)
	addon := &core.CollectionAddonSync{AddonID: "test-addon", CatalogKey: "items"}
	imported := insert("imported", "été", 9, true, addon)
	importedFirst := insert("imported first", "other", 1, true, addon)

	check := func(after core.RatingKey, want ...string) {
		t.Helper()
		var got []string
		_, err := s.visitOwnedSpecs(ctx, nil, core.TypeMovie, "", 2, after, func(row *specRow) (bool, error) {
			got = append(got, row.spec.Name)
			return false, nil
		})
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("after %v = %v, want %v, err %v", after, got, want, err)
		}
	}
	check(0, "first", "last", "middle", "imported first", "imported")
	check(first.key(), "last", "middle", "imported first", "imported")
	if err := s.deleteRow(ctx, anchor.id); err != nil {
		t.Fatal(err)
	}
	check(0, "middle", "first", "last", "imported first", "imported")
	moved := last.spec
	moved.Folder = "other"
	if err := s.updateRow(ctx, last.id, moved, true); err != nil {
		t.Fatal(err)
	}
	check(middle.key(), "last", "first", "imported first", "imported")
	if err := s.deleteRow(ctx, imported.id); err != nil {
		t.Fatal(err)
	}
	check(last.key(), "first", importedFirst.spec.Name)

	// Build resolution rewrites the document directly. Its projected group,
	// browse eligibility and type must change with the document.
	resolved := first.spec
	resolved.Folder = "other"
	resolved.SortOrder = 3
	resolved.BrowseInApps = false
	resolved.MediaType = MediaShow
	raw, err := specJSON(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.saveBuild(ctx, first.id, first.rawSpec, raw, nil, nil, 0, nil, "resolved", time.Now()); err != nil {
		t.Fatal(err)
	}
	check(last.key(), importedFirst.spec.Name)
	var group, typ string
	var rank int64
	var mode int
	if err := s.db.QueryRowContext(ctx, `SELECT browse_group,browse_type,browse_rank,browse_mode FROM collections_specs WHERE id=?`, first.id).
		Scan(&group, &typ, &rank, &mode); err != nil {
		t.Fatal(err)
	}
	if group != "folder:other" || typ != MediaShow || rank != browseRank(3) || mode != 0 {
		t.Fatalf("resolved projection = %q %q %d %d", group, typ, rank, mode)
	}
}

func TestIndexedOwnedPageKeepsDynamicCollectionsInWarmDisplayOrder(t *testing.T) {
	ctx := t.Context()
	db := stampsDB(t)
	cat, err := catalog.New(db, nil, config.Catalog{}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(db, cat, nil, WithCatalogConfig(func() config.Catalog {
		return config.Catalog{HideFranchiseCollections: true}
	}))
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"First", "Dynamic", "Third", "Fourth"} {
		spec := core.CollectionSpec{Name: name, MediaType: MediaMovie, BrowseInApps: true, Enabled: true}
		if i == 1 {
			spec.Folder, spec.SortOrder, spec.Limit = "group", 2, 1
		}
		if i == 3 {
			spec.Folder, spec.SortOrder = "GROUP", 1
		}
		Normalize(&spec)
		row, err := s.insertRow(ctx, spec, 0)
		if err != nil {
			t.Fatal(err)
		}
		for j := int64(1); j <= 2; j++ {
			id := row.id*10 + j
			k := core.MovieKey(id)
			if _, err := db.ExecContext(ctx, `INSERT INTO catalog_titles(key,type,tmdb_id,title,sort_title,released_ts,browsable)
				VALUES(?,?,?,?,?,1600000000,1)`, k, MediaMovie, id, "Movie", "movie"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO collections_members(spec_id,pos,key) VALUES(?,?,?)`, row.id, j, k); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.ExecContext(ctx, `UPDATE collections_specs SET last_built_at=1600000000,build_hash='fixture',item_count=2 WHERE id=?`, row.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.idx.get(ctx); err != nil {
		t.Fatal(err)
	}
	full, err := s.list(ctx, nil, core.TypeMovie, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 4 || full[1].Title != "Fourth" || full[2].Title != "Dynamic" || full[2].ChildCount != 1 {
		t.Fatalf("unexpected full listing: %+v", full)
	}
	for offset := 0; offset <= len(full); offset++ {
		page, total, err := s.ListPage(ctx, nil, core.TypeMovie, offset, 2)
		want := full[offset:min(offset+2, len(full))]
		if err != nil || total != len(full) || !slices.EqualFunc(page, want, func(a, b core.Collection) bool { return reflect.DeepEqual(a, b) }) {
			t.Fatalf("offset %d page=%+v total=%d err=%v", offset, page, total, err)
		}
	}
}
