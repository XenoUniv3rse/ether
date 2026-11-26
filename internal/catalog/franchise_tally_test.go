package catalog

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestFranchiseTallyTriggersRebuildCursorAndFilters(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	c, err := New(db, nil, config.Catalog{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })

	released := now.Add(-48 * time.Hour).Unix()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	insertMovie := func(tmdbID, colID int64, colName, rating, country string, browsable, adult int) {
		key := core.MovieKey(tmdbID)
		exec(`INSERT INTO catalog_titles(key,type,tmdb_id,title,sort_title,released_ts,browsable,adult,content_rating,enforcement_rating,collection_id,collection_name,poster,backdrop)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			int64(key), "movie", tmdbID, "Movie", "movie", released, browsable, adult, rating, rating, colID, colName, "/p.jpg", "/b.jpg")
		if country != "" {
			exec(`INSERT INTO catalog_countries(key,country) VALUES(?,?)`, int64(key), country)
		}
	}

	// Franchise 10: 3 movies (2 US PG-13, 1 JP R).
	insertMovie(101, 10, "Starlight Saga", "PG-13", "US", 1, 0)
	// After 1 movie, franchise must not count toward total (threshold is >= 2).
	if tot, err := c.FranchiseTotal(ctx); err != nil || tot != 0 {
		t.Fatalf("after 1 member: total=%d err=%v, want 0", tot, err)
	}
	insertMovie(102, 10, "Starlight Saga", "PG-13", "US", 1, 0)
	if tot, err := c.FranchiseTotal(ctx); err != nil || tot != 1 {
		t.Fatalf("after 2 members: total=%d err=%v, want 1", tot, err)
	}
	insertMovie(103, 10, "Starlight Saga", "R", "JP", 1, 0)

	// Franchise 20: 2 movies (both JP PG) — tied with Franchise 30.
	insertMovie(201, 20, "Kaze Chronicles", "PG", "JP", 1, 0)
	insertMovie(202, 20, "Kaze Chronicles", "PG", "JP", 1, 0)

	// Franchise 30: 2 movies with empty collection_name on titles, named only in catalog_collections.
	insertMovie(301, 30, "", "PG", "US", 1, 0)
	insertMovie(302, 30, "", "PG", "US", 1, 0)
	exec(`INSERT INTO catalog_collections(tmdb_id,name,summary,poster,backdrop,fetched_at) VALUES(30,'Nebula Collection','desc','/cp.jpg','/cb.jpg',?)`, now.Unix())

	// Non-browsable / adult movies must not push Franchise 40 over the threshold.
	insertMovie(401, 40, "Hidden", "PG", "US", 1, 0)
	insertMovie(402, 40, "Hidden", "PG", "US", 0, 0)
	insertMovie(403, 40, "Hidden", "PG", "US", 1, 1)

	if tot, err := c.FranchiseTotal(ctx); err != nil || tot != 3 {
		t.Fatalf("total=%d err=%v, want 3", tot, err)
	}

	// Cursor paging across tied counts (20 and 30 both have member_count=2) must
	// return every franchise exactly once in (member_count DESC, collection_id ASC) order.
	var seen []int64
	afterCount, afterID := 0, 0
	for {
		batch, err := c.FranchisesCursor(ctx, afterCount, afterID, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) == 0 {
			break
		}
		for _, col := range batch {
			seen = append(seen, col.Key.TMDB())
		}
		last := batch[len(batch)-1]
		afterCount, afterID = last.ChildCount, int(last.Key.TMDB())
	}
	if len(seen) != 3 || seen[0] != 10 || seen[1] != 20 || seen[2] != 30 {
		t.Fatalf("cursor order = %v, want [10 20 30]", seen)
	}

	// FTS search finds Franchise 30 via its catalog_collections.name fallback.
	found, err := c.FranchisesSearch(ctx, "nebula", 0, 0, 10)
	if err != nil || len(found) != 1 || found[0].Key.TMDB() != 30 || found[0].Title != "Nebula Collection" {
		t.Fatalf("search nebula = %+v err=%v", found, err)
	}

	// Country exclusion rule (same shape as production library filters): hide movies
	// whose country is not US. Franchise 20 (all JP) drops out; Franchise 10 keeps 2;
	// Franchise 30 keeps 2.
	countryRule := core.LibraryExclusion{
		Action:    "hide",
		MediaType: "movie",
		Match:     "all",
		Rules: []core.CollectionRule{
			{Field: "country", Op: "notIn", Value: json.RawMessage(`["US"]`)},
		},
	}
	counts, err := c.FranchiseVisibleCounts(ctx, core.Filter{
		Type:       core.TypeMovie,
		LocalOnly:  true,
		ExtraRules: []core.LibraryExclusion{countryRule},
	}, []int{10, 20, 30})
	if err != nil {
		t.Fatal(err)
	}
	if counts[10] != 2 || counts[20] != 0 || counts[30] != 2 {
		t.Fatalf("country-filtered counts = %v, want {10:2 30:2}", counts)
	}

	// Small-filter direct grouping returns the exact same set and counts.
	filtered := core.Filter{
		Type:       core.TypeMovie,
		LocalOnly:  true,
		ExtraRules: []core.LibraryExclusion{countryRule},
	}
	direct, hasMore, usedDirect, err := c.FranchisesInSmallFilter(ctx, filtered, "", 0, 10)
	if err != nil || !usedDirect || hasMore || len(direct) != 2 || direct[0].Key.TMDB() != 10 || direct[0].ChildCount != 2 || direct[1].Key.TMDB() != 30 || direct[1].ChildCount != 2 {
		t.Fatalf("FranchisesInSmallFilter = %+v hasMore=%v used=%v err=%v", direct, hasMore, usedDirect, err)
	}

	// Seek on raw franchise size, which can differ from its visible size.
	next, hasMore, usedDirect, err := c.FranchisesInSmallFilterAfter(ctx, filtered, "", 0, 10, 3, 10)
	if err != nil || !usedDirect || hasMore || len(next) != 1 || next[0].Key.TMDB() != 30 {
		t.Fatalf("FranchisesInSmallFilterAfter = %+v hasMore=%v used=%v err=%v", next, hasMore, usedDirect, err)
	}

	// The rank-ordered page counts the whole scope and opens only the requested bucket.
	page, total, err := c.FranchiseFilteredPage(ctx, filtered, 1, 1, 0, 0, true)
	if err != nil || total != 2 || len(page) != 1 || page[0].Key.TMDB() != 30 || page[0].ChildCount != 2 {
		t.Fatalf("filtered franchise page=%+v total=%d err=%v", page, total, err)
	}

	// Transition back down below threshold: marking movie 202 adult removes Franchise 20.
	exec(`UPDATE catalog_titles SET adult=1 WHERE key=?`, int64(core.MovieKey(202)))
	if tot, err := c.FranchiseTotal(ctx); err != nil || tot != 2 {
		t.Fatalf("after marking 202 adult: total=%d err=%v, want 2", tot, err)
	}

	// Wipe summary + count tables and verify RebuildLibraryCounts restores exact parity.
	exec(`DELETE FROM catalog_franchises`)
	exec(`DELETE FROM catalog_franchise_counts`)
	exec(`DELETE FROM catalog_franchise_counts_delta`)
	if err := RebuildLibraryCounts(ctx, db); err != nil {
		t.Fatal(err)
	}
	if tot, err := c.FranchiseTotal(ctx); err != nil || tot != 2 {
		t.Fatalf("after rebuild: total=%d err=%v, want 2", tot, err)
	}
	rebuilt, err := c.FranchisesPage(ctx, 0, 10)
	if err != nil || len(rebuilt) != 2 || rebuilt[0].Key.TMDB() != 10 || rebuilt[1].Key.TMDB() != 30 {
		t.Fatalf("rebuilt page = %+v err=%v", rebuilt, err)
	}
}
