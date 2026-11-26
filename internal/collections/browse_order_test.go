package collections

import (
	"log/slog"
	"slices"
	"testing"
	"time"

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
}
