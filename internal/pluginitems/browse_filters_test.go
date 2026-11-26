package pluginitems

import (
	"context"
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

func sampleAlbumFilterDecl() plugins.MediaKindMeta {
	return plugins.MediaKindMeta{
		Key: "album",
		Levels: []plugins.KindLevel{
			{Key: "album", Label: "Album", Playable: true},
		},
		Fields: []plugins.KindField{
			{Key: "genres", Label: "Genres", Type: "strings", Indexed: true},
			{Key: "label", Label: "Label", Type: "string", Indexed: true},
			{Key: "parentLabel", Label: "Parent Label", Type: "string", Indexed: true},
			{Key: "artists", Label: "Artists", Type: "people", Indexed: true},
			{Key: "bpm", Label: "BPM", Type: "int", Indexed: true},
			{Key: "explicit", Label: "Explicit", Type: "bool", Indexed: true},
			{Key: "recordedAt", Label: "Recorded", Type: "date", Indexed: true},
		},
		Browse: plugins.KindBrowseDecl{
			Sorts:        []string{"title", "year", "released", "rating"},
			Facets:       []string{"genres", "label", "parentLabel", "artists", "year", "released", "rating"},
			Search:       true,
			SearchFields: []string{"label", "parentLabel", "genres"},
		},
	}
}

func TestBrowseEmptyCrossFieldAnyAndExcludeQuery(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestService(t, Options{})
	if _, err := s.UpsertKind(ctx, "community.music", 20, sampleAlbumFilterDecl(), false); err != nil {
		t.Fatal(err)
	}

	items := []core.KindItem{
		{
			ID: "a1", Level: "album", Title: "Alpha Live at Town Hall", Summary: "Concert recording",
			Year: 1965, Released: -157766400, Rating: 8.5, Recording: "isrc:US001", Playable: true,
			Fields: map[string]json.RawMessage{
				"genres":      rawJSON(t, []string{"Rock", "Live"}),
				"label":       rawJSON(t, "North"),
				"parentLabel": rawJSON(t, "Universal"),
				"artists":     rawJSON(t, []string{"Miles Davis"}),
				"bpm":         rawJSON(t, 120),
				"explicit":    rawJSON(t, true),
				"recordedAt":  rawJSON(t, int64(1700000000)),
			},
		},
		{
			ID: "a2", Level: "album", Title: "Beta Studio Album", Summary: "",
			Year: 2022, Released: 1650000000, Rating: 0, Recording: "", Playable: true,
			Fields: map[string]json.RawMessage{
				"genres":      rawJSON(t, []string{"Jazz"}),
				"label":       rawJSON(t, "Sub Pop"),
				"parentLabel": rawJSON(t, "North"),
				"artists":     rawJSON(t, []string{"Bill Evans"}),
				"bpm":         rawJSON(t, 0),
				"explicit":    rawJSON(t, false),
			},
		},
		{
			ID: "a3", Level: "album", Title: "Gamma Unclassified", Summary: "Unlabeled tape",
			Year: 0, Released: 0, Rating: 7.0, Recording: "", Playable: true,
			Fields: map[string]json.RawMessage{
				"genres":  rawJSON(t, []string{}),
				"label":   rawJSON(t, ""),
				"artists": rawJSON(t, []string{"Miles Davis"}),
			},
		},
		{
			ID: "a4", Level: "album", Title: "Delta Acoustic Sessions", Summary: "Quiet album",
			Year: 2024, Released: 1710000000, Rating: 9.0, Recording: "isrc:US004", Playable: true,
			Fields: map[string]json.RawMessage{
				"genres": rawJSON(t, []string{"Folk"}),
				"label":  rawJSON(t, "South"),
			},
		},
		{
			ID: "a5", Level: "album", Title: "Epsilon Rare Live Bootleg", Summary: "",
			Year: 0, Released: 0, Rating: 0, Recording: "", Playable: true,
			Fields: map[string]json.RawMessage{
				"parentLabel": rawJSON(t, "East"),
			},
		},
	}
	if _, err := s.UpsertItems(ctx, "community.music", "album", items); err != nil {
		t.Fatal(err)
	}

	runBrowse := func(q BrowseQuery) BrowsePage {
		t.Helper()
		q.LibraryID = 20
		q.Level = "album"
		q.Sort = "title"
		if q.Limit == 0 {
			q.Limit = 50
		}
		page, err := s.Browse(ctx, q)
		if err != nil {
			t.Fatalf("Browse %+v: %v", q, err)
		}
		return page
	}
	pageIDs := func(p BrowsePage) []string {
		out := make([]string, len(p.Items))
		for i, it := range p.Items {
			out[i] = it.ID
		}
		return out
	}

	for _, tc := range []struct {
		name string
		q    BrowseQuery
		want []string
	}{
		{
			name: "empty strings field true",
			q:    BrowseQuery{Empty: map[string]bool{"genres": true}},
			want: []string{"a5", "a3"},
		},
		{
			name: "empty strings field false",
			q:    BrowseQuery{Empty: map[string]bool{"genres": false}},
			want: []string{"a1", "a2", "a4"},
		},
		{
			name: "empty string field treats empty string as empty",
			q:    BrowseQuery{Empty: map[string]bool{"label": true}},
			want: []string{"a5", "a3"},
		},
		{
			name: "empty people field false",
			q:    BrowseQuery{Empty: map[string]bool{"artists": false}},
			want: []string{"a1", "a2", "a3"},
		},
		{
			name: "empty int field keeps zero as non-empty",
			q:    BrowseQuery{Empty: map[string]bool{"bpm": false}},
			want: []string{"a1", "a2"},
		},
		{
			name: "empty bool field keeps false as non-empty",
			q:    BrowseQuery{Empty: map[string]bool{"explicit": false}},
			want: []string{"a1", "a2"},
		},
		{
			name: "empty date and date-decade field",
			q:    BrowseQuery{Empty: map[string]bool{"recordedAt-decade": false}},
			want: []string{"a1"},
		},
		{
			name: "empty envelope released true (pre-1970 negative is non-empty)",
			q:    BrowseQuery{Empty: map[string]bool{"released": true}},
			want: []string{"a5", "a3"},
		},
		{
			name: "empty envelope released false includes pre-1970",
			q:    BrowseQuery{Empty: map[string]bool{"released": false}},
			want: []string{"a1", "a2", "a4"},
		},
		{
			name: "empty envelope year rating summary recording",
			q:    BrowseQuery{Empty: map[string]bool{"year": false, "rating": false, "summary": false, "recording": false}},
			want: []string{"a1", "a4"},
		},
		{
			name: "combined empty genres+released+artists from issue 807",
			q:    BrowseQuery{Empty: map[string]bool{"genres": true, "released": true, "artists": false}},
			want: []string{"a3"},
		},
		{
			name: "anyGroups cross-field label OR parentLabel",
			q: BrowseQuery{AnyGroups: []core.KindBrowseFacetGroup{
				{Any: []core.KindBrowseFacetAlt{
					{Field: "label", Values: []string{"North"}},
					{Field: "parentLabel", Values: []string{"North"}},
				}},
			}},
			want: []string{"a1", "a2"},
		},
		{
			name: "anyGroups cross-field mixed types (label OR year range)",
			q: BrowseQuery{AnyGroups: []core.KindBrowseFacetGroup{
				{Any: []core.KindBrowseFacetAlt{
					{Field: "label", Values: []string{"North"}},
					{Field: "year", Values: []string{"2024..2025"}},
				}},
			}},
			want: []string{"a1", "a4"},
		},
		{
			name: "anyGroups cross-field AND legacy single-field group",
			q: BrowseQuery{AnyGroups: []core.KindBrowseFacetGroup{
				{Any: []core.KindBrowseFacetAlt{
					{Field: "label", Values: []string{"North"}},
					{Field: "parentLabel", Values: []string{"North"}},
				}},
				{Field: "genres", Values: []string{"Jazz", "Folk"}},
			}},
			want: []string{"a2"},
		},
		{
			name: "anyGroups single-alternative {any: [...]} behaves like group of one",
			q: BrowseQuery{AnyGroups: []core.KindBrowseFacetGroup{
				{Any: []core.KindBrowseFacetAlt{{Field: "parentLabel", Values: []string{"East"}}}},
			}},
			want: []string{"a5"},
		},
		{
			name: "excludeQuery drops live matches",
			q:    BrowseQuery{ExcludeQuery: "live"},
			want: []string{"a2", "a4", "a3"},
		},
		{
			name: "excludeQuery matches searchFields and people too",
			q:    BrowseQuery{ExcludeQuery: "Miles"},
			want: []string{"a2", "a4", "a5"},
		},
		{
			name: "excludeQuery punctuation-only is a no-op",
			q:    BrowseQuery{ExcludeQuery: "   ---!!!   "},
			want: []string{"a1", "a2", "a4", "a5", "a3"},
		},
		{
			name: "all three new filters combined",
			q: BrowseQuery{
				Empty: map[string]bool{"released": false},
				AnyGroups: []core.KindBrowseFacetGroup{
					{Any: []core.KindBrowseFacetAlt{
						{Field: "label", Values: []string{"North"}},
						{Field: "parentLabel", Values: []string{"North"}},
					}},
				},
				ExcludeQuery: "live",
			},
			want: []string{"a2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := runBrowse(tc.q)
			if got := pageIDs(p); !slices.Equal(got, tc.want) {
				t.Errorf("items = %v, want %v", got, tc.want)
			}
			if !p.TotalExact || p.Total != len(tc.want) {
				t.Errorf("total = %d (exact=%v), want %d (exact=true)", p.Total, p.TotalExact, len(tc.want))
			}
		})
	}

	// Paging + exact total with excludeQuery across 2 pages.
	p1 := runBrowse(BrowseQuery{ExcludeQuery: "live", Limit: 2})
	if got := pageIDs(p1); !slices.Equal(got, []string{"a2", "a4"}) || !p1.TotalExact || p1.Total != 3 || p1.NextCursor == "" {
		t.Fatalf("page1 = %v total=%d exact=%v cursor=%q", got, p1.Total, p1.TotalExact, p1.NextCursor)
	}
	p2 := runBrowse(BrowseQuery{ExcludeQuery: "live", Limit: 2, Cursor: p1.NextCursor})
	if got := pageIDs(p2); !slices.Equal(got, []string{"a3"}) || !p2.TotalExact || p2.Total != 3 || p2.NextCursor != "" {
		t.Fatalf("page2 = %v total=%d exact=%v cursor=%q", got, p2.Total, p2.TotalExact, p2.NextCursor)
	}

	// Search combines query + excludeQuery + empty + cross-field anyGroups.
	sp, err := s.Search(ctx, SearchQuery{
		LibraryID: 20, Level: "album", Limit: 50,
		Query:        "north",
		ExcludeQuery: "live",
		Empty:        map[string]bool{"genres": false},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := pageIDs(sp); !slices.Equal(got, []string{"a2"}) || !sp.TotalExact || sp.Total != 1 {
		t.Fatalf("Search = %v total=%d exact=%v, want [a2] total=1", got, sp.Total, sp.TotalExact)
	}
	// query == excludeQuery yields empty set.
	spEmpty, err := s.Search(ctx, SearchQuery{
		LibraryID: 20, Level: "album", Limit: 50,
		Query: "live", ExcludeQuery: "live",
	})
	if err != nil {
		t.Fatalf("Search live/live: %v", err)
	}
	if len(spEmpty.Items) != 0 || spEmpty.Total != 0 {
		t.Fatalf("Search live excluding live = %v total=%d, want empty", pageIDs(spEmpty), spEmpty.Total)
	}

	// BrowseBatch supports all three filters without false tally shortcuts.
	batchPages, err := s.BrowseBatch(ctx, []BrowseQuery{
		{LibraryID: 20, Level: "album", Sort: "title", NoTotal: true, Limit: 10, Empty: map[string]bool{"genres": true}},
		{LibraryID: 20, Level: "album", Sort: "title", NoTotal: true, Limit: 10, ExcludeQuery: "live"},
		{LibraryID: 20, Level: "album", Sort: "title", NoTotal: true, Limit: 10, AnyGroups: []core.KindBrowseFacetGroup{
			{Any: []core.KindBrowseFacetAlt{
				{Field: "label", Values: []string{"North"}},
				{Field: "parentLabel", Values: []string{"North"}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("BrowseBatch: %v", err)
	}
	if got := pageIDs(batchPages[0]); !slices.Equal(got, []string{"a5", "a3"}) {
		t.Errorf("BrowseBatch[empty] = %v, want [a5 a3]", got)
	}
	if got := pageIDs(batchPages[1]); !slices.Equal(got, []string{"a2", "a4", "a3"}) {
		t.Errorf("BrowseBatch[excludeQuery] = %v, want [a2 a4 a3]", got)
	}
	if got := pageIDs(batchPages[2]); !slices.Equal(got, []string{"a1", "a2"}) {
		t.Errorf("BrowseBatch[anyGroups] = %v, want [a1 a2]", got)
	}

	// Unknown fields in empty are rejected in both Browse and Search (even with blank/punctuation query).
	if _, err := s.Browse(ctx, BrowseQuery{LibraryID: 20, Level: "album", Empty: map[string]bool{"noSuchField": true}}); !errors.Is(err, ErrInvalidBrowseField) {
		t.Fatalf("Browse unknown empty field err = %v, want ErrInvalidBrowseField", err)
	}
	if _, err := s.Search(ctx, SearchQuery{LibraryID: 20, Level: "album", Query: "!!!", Empty: map[string]bool{"noSuchField": false}}); !errors.Is(err, ErrInvalidBrowseField) {
		t.Fatalf("Search unknown empty field err = %v, want ErrInvalidBrowseField", err)
	}

	// Value bound counts every alternative's values; empty does not count toward the bound.
	half := make([]string, MaxBrowseFacetValues/2)
	for i := range half {
		half[i] = "v"
	}
	if _, err := s.Browse(ctx, BrowseQuery{
		LibraryID: 20, Level: "album",
		AnyGroups: []core.KindBrowseFacetGroup{{Any: []core.KindBrowseFacetAlt{
			{Field: "label", Values: half},
			{Field: "parentLabel", Values: half},
		}}},
		Empty: map[string]bool{"genres": true, "released": true, "rating": false},
	}); err != nil {
		t.Fatalf("256 values across any alternatives + empty should pass: %v", err)
	}
	overHalf := append(slices.Clone(half), "extra")
	if _, err := s.Browse(ctx, BrowseQuery{
		LibraryID: 20, Level: "album",
		AnyGroups: []core.KindBrowseFacetGroup{{Any: []core.KindBrowseFacetAlt{
			{Field: "label", Values: half},
			{Field: "parentLabel", Values: overHalf},
		}}},
	}); err == nil {
		t.Fatal("expected value bound error for 257 values across any alternatives")
	}

	// Verify SQLite query plan for excludeQuery, empty, and cross-field anyGroups
	// stays correlated / index-driven without temp B-tree sorts or full table scans.
	if store.DialectOf(s.db) == store.SQLite {
		eqpRows, err := s.db.QueryContext(ctx, `EXPLAIN QUERY PLAN
SELECT i.item_ref FROM plugin_items i
WHERE i.library_id = ? AND i.level = ?
  AND NOT EXISTS (SELECT 1 FROM plugin_item_fields f WHERE f.item_ref = i.item_ref AND f.field = ? AND f.v_text <> '')
  AND ((EXISTS (SELECT 1 FROM plugin_item_fields f WHERE f.item_ref = i.item_ref AND f.field = ? AND f.v_text IN (?)))
       OR (EXISTS (SELECT 1 FROM plugin_item_fields f WHERE f.item_ref = i.item_ref AND f.field = ? AND f.v_text IN (?))))
  AND NOT EXISTS (SELECT 1 FROM plugin_item_fts ef WHERE ef.rowid = i.seq AND ef.plugin_item_fts MATCH ?)
ORDER BY i.sort_title ASC, i.item_ref ASC
LIMIT 11`, int64(20), "album", "genres", "label", "North", "parentLabel", "North", `"live"*`)
		if err != nil {
			t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
		}
		defer eqpRows.Close()
		var planLines []string
		for eqpRows.Next() {
			var id, parent, notused int
			var detail string
			if err := eqpRows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			planLines = append(planLines, detail)
		}
		planText := strings.Join(planLines, "\n")
		t.Logf("SQLite EXPLAIN QUERY PLAN:\n%s", planText)
		if strings.Contains(planText, "USE TEMP B-TREE") {
			t.Errorf("unexpected TEMP B-TREE in SQLite plan:\n%s", planText)
		}
	}

	// Verify PostgreSQL SQL shape uses correlated EXISTS + md5 + direct search_tsv.
	var pgWhere strings.Builder
	var pgArgs []any
	appendAnyGroupClause(&pgWhere, &pgArgs, map[string]string{"label": "string", "parentLabel": "string"}, core.KindBrowseFacetGroup{
		Any: []core.KindBrowseFacetAlt{
			{Field: "label", Values: []string{"North"}},
			{Field: "parentLabel", Values: []string{"North"}},
		},
	}, true)
	appendBrowseExcludeQuery(&pgWhere, &pgArgs, "live", true)
	pgSQL := pgWhere.String()
	if !strings.Contains(pgSQL, "EXISTS (SELECT 1 FROM plugin_item_fields f WHERE f.item_ref = i.item_ref") ||
		!strings.Contains(pgSQL, " OR ") ||
		!strings.Contains(pgSQL, "md5(f.v_text)") ||
		!strings.Contains(pgSQL, "NOT (i.search_tsv @@ to_tsquery('simple', ?))") {
		t.Fatalf("unexpected Postgres filter SQL: %s", pgSQL)
	}
}

func TestBrowseRareEmptyFieldPresence(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestService(t, Options{})
	const pluginID, kindKey, libID = "test.presence", "album", 83
	if _, err := s.UpsertKind(ctx, pluginID, libID, sampleAlbumFilterDecl(), false); err != nil {
		t.Fatal(err)
	}
	items := make([]core.KindItem, 160)
	genres := rawJSON(t, []string{"Rock", "Live"})
	for i := range items {
		items[i] = core.KindItem{
			ID: fmt.Sprintf("album-%03d", i), Level: kindKey, Title: "Album",
			Released: int64(i + 1), Playable: true,
		}
		if i > 1 {
			items[i].Fields = map[string]json.RawMessage{"genres": genres}
		}
	}
	upsert := func(items []core.KindItem) {
		t.Helper()
		if _, err := s.UpsertItems(ctx, pluginID, kindKey, items); err != nil {
			t.Fatal(err)
		}
	}
	upsert(items)
	q := BrowseQuery{
		LibraryID: libID, Sort: "released:desc", Limit: 1,
		Empty: map[string]bool{"genres": true}, NoTotal: true, CheapTotal: true,
	}
	check := func(q BrowseQuery, id string, total int, more bool) BrowsePage {
		t.Helper()
		page, err := s.Browse(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Items[0].ID != id || page.Total != total ||
			!page.TotalExact || (page.NextCursor != "") != more {
			t.Fatalf("want %s, total %d, more %t: got %+v", id, total, more, page)
		}
		return page
	}
	first := check(q, items[1].ID, 2, true)
	q.Cursor = first.NextCursor
	check(q, items[0].ID, 2, false)

	q.Cursor = ""
	items[1].Fields = map[string]json.RawMessage{"genres": genres}
	upsert(items[1:2])
	if err := s.DeleteItems(ctx, pluginID, kindKey, []string{items[2].ID}); err != nil {
		t.Fatal(err)
	}
	check(q, items[0].ID, 1, false)

	q.Empty = map[string]bool{"genres": false}
	check(q, items[len(items)-1].ID, len(items)-2, true)
}
