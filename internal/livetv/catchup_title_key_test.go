package livetv

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

// A listing's roll-up reads the airings of the listed shows only: more
// archive (other shows' airings) adds no statements and no rows, and a
// listed show's airings older than the page's part of the window still count.
func TestCatchUpRollupQueryBudget(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s, ch, h := catchUpWatchFixture(t, now) // News at -5h and -3h
	insertCatchUpProgramme(t, s, ch, "Alpha", now.Add(-1*time.Hour), 30*time.Minute)
	insertCatchUpProgramme(t, s, ch, "Alpha", now.Add(-40*time.Hour), 30*time.Minute)
	insertCatchUpProgramme(t, s, ch, "Bravo", now.Add(-2*time.Hour), 30*time.Minute)
	owner := &core.User{ID: 1, IsOwner: true}
	if err := h.CatchUpSetWatched(t.Context(), owner, frontend.CatchUpGroupID(frontend.TypeShow, "Alpha"), true); err != nil {
		t.Fatal(err)
	}
	measure := func(label string) (int64, int64) {
		t.Helper()
		ctx, stats := store.WithQueryStats(t.Context())
		page, err := h.CatchUpItems(ctx, owner, frontend.CatchUpQuery{Limit: 2, Rollup: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 2 || page.Items[0].Programme.Item.Title != "Alpha" || page.Items[1].Programme.Item.Title != "Bravo" {
			t.Fatalf("%s: page %+v", label, page.Items)
		}
		if w := page.Items[0].Watch; w.Episodes != 2 || w.Watched != 2 || !w.FullyWatched() {
			t.Fatalf("%s: Alpha roll-up %+v", label, w)
		}
		if w := page.Items[1].Watch; w.Episodes != 1 || w.Watched != 0 {
			t.Fatalf("%s: Bravo roll-up %+v", label, w)
		}
		t.Logf("%s: queries=%d rows=%d", label, stats.Queries(), stats.Rows())
		return stats.Queries(), stats.Rows()
	}
	queries, rows := measure("small archive")
	// 300 airings of other shows, all older than the page's 12-hour slice.
	for i := range 300 {
		insertCatchUpProgramme(t, s, ch, fmt.Sprintf("Filler %d", i), now.Add(-13*time.Hour-time.Duration(i)*5*time.Minute), 4*time.Minute) // -13h to -38h
	}
	if q, r := measure("large archive"); q != queries || r != rows {
		t.Fatalf("roll-up cost grew with the archive: queries %d -> %d, rows %d -> %d", queries, q, rows, r)
	}
}

// The migration fills in the title key of rows stored before it, and leaves
// a row it cannot read alone.
func TestCatchUpTitleKeyBackfill(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s, ch, h := catchUpWatchFixture(t, now) // News at -5h and -3h
	if _, err := s.o.DB.ExecContext(t.Context(), `UPDATE livetv_programmes SET title_key=NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.o.DB.ExecContext(t.Context(), `INSERT INTO livetv_programmes(source_id,channel_id,start_ms,end_ms,data) VALUES(?,?,?,?,?)`,
		guideSource(ch), ch.GuideID, now.Add(-10*time.Hour).UnixMilli(), now.Add(-9*time.Hour).UnixMilli(), "[1]"); err != nil { // valid JSON (PostgreSQL types the column), not an airing
		t.Fatal(err)
	}
	owner := &core.User{ID: 1, IsOwner: true}
	news := frontend.CatchUpGroupID(frontend.TypeShow, "News")
	if _, err := h.CatchUpGroupWatch(t.Context(), owner, news); err == nil {
		t.Fatal("show found with no title keys stored")
	}
	tx, err := s.o.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := backfillCatchUpTitleKeys(t.Context(), tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var keyed, unkeyed int
	if err := s.o.DB.QueryRowContext(t.Context(), `SELECT
		SUM(CASE WHEN title_key=? THEN 1 ELSE 0 END), SUM(CASE WHEN title_key IS NULL THEN 1 ELSE 0 END) FROM livetv_programmes`,
		programmeTitleKey("News")).Scan(&keyed, &unkeyed); err != nil {
		t.Fatal(err)
	}
	if keyed != 2 || unkeyed != 1 {
		t.Fatalf("after backfill: %d keyed, %d without a key", keyed, unkeyed)
	}
	if w, err := h.CatchUpGroupWatch(t.Context(), owner, news); err != nil || w.Episodes != 2 {
		t.Fatalf("show after backfill: %+v, %v", w, err)
	}
}

// The guide import stores each airing's title key, and a changed title
// changes it.
func TestCatchUpTitleKeyImport(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s, _, _ := catchUpWatchFixture(t, now)
	write := func(title string) {
		t.Helper()
		plan, err := planProgrammes([]Programme{{ChannelID: "g-import", Start: now, End: now.Add(time.Hour), Item: Item{Title: title}}})
		if err != nil {
			t.Fatal(err)
		}
		tx, err := s.o.DB.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writeProgrammes(t.Context(), tx, 150, "src-import", plan, now, nil, nil); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		var key string
		if err := s.o.DB.QueryRowContext(context.Background(), `SELECT title_key FROM livetv_programmes WHERE source_id='src-import'`).Scan(&key); err != nil {
			t.Fatal(err)
		}
		if key != programmeTitleKey(title) || key != frontend.CatchUpGroupID(frontend.TypeShow, title)[len("catchup-show-"):] {
			t.Fatalf("%q stored key %q", title, key)
		}
	}
	write("Late Show")
	write("Later Show")
}

// Migration livetv 30 itself, from a livetv 29 database: every readable row
// gets its key across several backfill pages, an unreadable one keeps none.
func TestCatchUpTitleKeyMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ether.db")
	old, err := store.OpenPath(store.WithMigrationCeiling(ctx, map[string]int{"livetv": 29}), path)
	if err != nil {
		t.Fatal(err)
	}
	rows := 2*catchUpTitleKeyPage + 17
	for i := range rows {
		raw := fmt.Sprintf(`{"item":{"title":"Show %d"}}`, i%40)
		if _, err := old.ExecContext(ctx, `INSERT INTO livetv_programmes(source_id,channel_id,start_ms,end_ms,data) VALUES(?,?,?,?,?)`, "src", fmt.Sprintf("g%d", i%7), int64(i)*60000, int64(i)*60000+30000, raw); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.ExecContext(ctx, `INSERT INTO livetv_programmes(source_id,channel_id,start_ms,end_ms,data) VALUES('src','bad',1,2,'[1]')`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenPath(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var keyed, wrong, unkeyed int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(title_key), COUNT(*) FILTER (WHERE title_key IS NOT NULL AND channel_id='g0' AND start_ms=0 AND title_key<>?), COUNT(*) FILTER (WHERE title_key IS NULL) FROM livetv_programmes`,
		programmeTitleKey("Show 0")).Scan(&keyed, &wrong, &unkeyed); err != nil {
		t.Fatal(err)
	}
	if keyed != rows || wrong != 0 || unkeyed != 1 {
		t.Fatalf("after migration: %d of %d keyed, %d wrong, %d without a key", keyed, rows, wrong, unkeyed)
	}
}

// The guide import keys a row stored without its key (a restore of an older
// dump, or an older node's write): an identical incoming row is rewritten,
// and a kept archive row no longer in the feed is keyed from its data.
func TestCatchUpTitleKeyImportRepairs(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s, _, _ := catchUpWatchFixture(t, now)
	current := Programme{ChannelID: "g-import", Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Item: Item{Title: "Late Show"}}
	archived := Programme{ChannelID: "g-import", Start: now.Add(-3 * time.Hour), End: now.Add(-2 * time.Hour), Item: Item{Title: "Early Show"}}
	write := func(ps ...Programme) {
		t.Helper()
		plan, err := planProgrammes(ps)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := s.o.DB.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writeProgrammes(t.Context(), tx, 150, "src-import", plan, now, map[string]int64{"g-import": 0}, nil); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	write(current, archived)
	if _, err := s.o.DB.ExecContext(t.Context(), `UPDATE livetv_programmes SET title_key=NULL WHERE source_id='src-import'`); err != nil {
		t.Fatal(err)
	}
	write(current) // the archived airing has left the feed and is kept
	for _, p := range []Programme{current, archived} {
		var key sql.NullString
		if err := s.o.DB.QueryRowContext(t.Context(), `SELECT title_key FROM livetv_programmes WHERE source_id='src-import' AND start_ms=?`, p.Start.UnixMilli()).Scan(&key); err != nil {
			t.Fatal(err)
		}
		if key.String != programmeTitleKey(p.Item.Title) {
			t.Fatalf("%q after import: key %+v", p.Item.Title, key)
		}
	}
}

// Both lookup forms stay on the title index on SQLite: a GUID's key prefix
// and a channel filter must not turn it into a per-channel window scan.
func TestCatchUpGroupAiringsPlan(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s, _, _ := catchUpWatchFixture(t, now)
	if store.DialectOf(s.o.DB) != store.SQLite {
		t.Skip("SQLite plan")
	}
	for i := range 2000 {
		if _, err := s.o.DB.ExecContext(t.Context(), `INSERT INTO livetv_programmes(source_id,channel_id,start_ms,end_ms,data,title_key) VALUES(?,?,?,?,'{}',?)`,
			"src", fmt.Sprintf("g%d", i%200), now.Add(-time.Duration(i)*time.Minute).UnixMilli(), now.Add(-time.Duration(i)*time.Minute+30*time.Second).UnixMilli(), programmeTitleKey(fmt.Sprint(i%500))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.o.DB.ExecContext(t.Context(), `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	guides := `AND (source_id||'', channel_id||'') IN ((?,?),(?,?),(?,?))`
	for name, filter := range map[string]string{
		"keys": `title_key IN (?,?) AND start_ms>=? AND start_ms<?`,
		"guid": `title_key>=? AND title_key<=? AND start_ms+0>=? AND start_ms+0<?`,
	} {
		for _, gf := range []string{"", " " + guides} {
			rows, err := s.o.DB.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+catchUpGroupAiringsSQL(filter, gf), "a", "b", 1, 2, "s", "c", "s", "d", "s", "e", 3)
			if err != nil {
				t.Fatal(err)
			}
			var plan strings.Builder
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan.WriteString(detail + "; ")
			}
			rows.Close()
			if !strings.Contains(plan.String(), "livetv_programmes_title") {
				t.Fatalf("%s %q: plan %s", name, gf, plan.String())
			}
		}
	}
	// The backfill pages in primary-key order: no re-sort of every unkeyed
	// row per page (the title index would give one).
	if _, err := s.o.DB.ExecContext(t.Context(), `UPDATE livetv_programmes SET title_key=NULL WHERE channel_id IN ('g1','g2')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.o.DB.ExecContext(t.Context(), `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	rows, err := s.o.DB.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+catchUpTitleKeyPageSQL, "src", "g1", 0, catchUpTitleKeyPage)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "; ")
	}
	rows.Close()
	if strings.Contains(plan.String(), "TEMP B-TREE") || strings.Contains(plan.String(), "livetv_programmes_title") {
		t.Fatalf("backfill page plan: %s", plan.String())
	}
}
