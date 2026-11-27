package personalize

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestRecommendationHistoryUpgrade(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
	shown := now.Add(-25 * time.Hour)
	item := int64(core.MovieKey(17))
	src := store.OpenTest(t)
	exec := func(db *sql.DB, query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	show := func(hub, device string, viewer int, at time.Time) {
		t.Helper()
		exec(src, `INSERT INTO personalize_row_impressions
			(user_id,day,hub_id,kind,title,serves,best_position,first_at,last_at,device)
			VALUES (?,?,?,?,?,3,2,?,?,?)`, viewer, dayOf(at, time.UTC), hub, KindTop10, "Top 10", ms(at), ms(at), device)
		exec(src, `INSERT INTO personalize_item_impressions
			(user_id,day,item_key,hub_id,kind,shows,first_at,last_at) VALUES (?,?,?,?,?,3,?,?)`,
			viewer, dayOf(at, time.UTC), item, hub, KindTop10, ms(at), ms(at))
	}
	play := func(hub string, viewer int, at time.Time, completed int) {
		t.Helper()
		exec(src, `INSERT INTO personalize_plays
			(user_id,item_key,taste_key,hub_id,kind,started_at,updated_at,duration_ms,start_fraction,max_fraction,completed)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, viewer, item, item, hub, KindTop10, ms(at), ms(at), 3_600_000, 0.2, 1, completed)
	}
	show("home.top10", "tv", 1, shown)
	show("home.second", "Plex", 1, shown)
	show("home.expired", "plex", 1, shown.AddDate(0, 0, -16))
	for n := range recommendationHistoryBatch {
		show(fmt.Sprintf("home.extra-%d", n), "tv", 2, shown)
	}
	play("home.top10", 1, shown.Add(20*time.Hour), 1)
	play("home.top10", 1, shown.Add(21*time.Hour), 0)
	play("home.missing", 1, shown.Add(22*time.Hour), 0)
	for range recommendationHistoryBatch {
		play("", 1, shown.Add(23*time.Hour), 0)
	}
	play("home.expired", 1, shown.AddDate(0, 0, -16).Add(time.Hour), 0)

	upgrade := func(db *sql.DB) {
		t.Helper()
		opts := Options{DB: db, Now: func() time.Time { return now }, Location: time.UTC, Retention: 14 * 24 * time.Hour}
		if err := opts.UpgradeRecommendationHistory(ctx); err != nil {
			t.Fatal(err)
		}
	}
	restore := func(db *sql.DB) *sql.DB {
		t.Helper()
		var image bytes.Buffer
		if err := store.ExportLogical(ctx, db, &image); err != nil {
			t.Fatal(err)
		}
		target := store.OpenTest(t)
		if err := store.ImportLogical(ctx, target, &image); err != nil {
			t.Fatal(err)
		}
		var markers int
		if err := target.QueryRowContext(ctx, `SELECT COUNT(*) FROM personalize_recommendation_upgrades`).Scan(&markers); err != nil || markers != 0 {
			t.Fatalf("restore copied local markers: %d, %v", markers, err)
		}
		return target
	}
	verify := func(db *sql.DB) {
		t.Helper()
		engine := &Engine{store: &sqlStore{db: db, loc: time.UTC}, loc: time.UTC, opts: Options{Now: func() time.Time { return now }}}
		report, err := engine.RecommendationReport(ctx, 90, 0)
		if err != nil || report.Impressions != recommendationHistoryBatch+2 || report.Plays != 1 ||
			report.Completed != 1 || report.Measured != 1 || report.WatchedMS != 2_880_000 ||
			report.Starts != recommendationHistoryBatch+3 || report.AttributedStarts != 2 {
			t.Fatalf("backfilled report = %+v, %v", report, err)
		}
		detail, err := engine.RecommendationDetail(ctx, 90, KindTop10, 1)
		if err != nil || len(detail.Apps) != 2 || detail.Apps[0].Key != "unknown" || detail.Apps[1].Key != "plex" ||
			len(detail.Positions) != 1 || detail.Positions[0].Key != "3" ||
			len(detail.Top) != 1 || detail.Top[0].Impressions != 2 || detail.Top[0].Plays != 2 {
			t.Fatalf("backfilled row detail = %+v, %v", detail, err)
		}
	}

	legacyRestore := restore(src)
	upgrade(legacyRestore)
	verify(legacyRestore)
	upgrade(legacyRestore)
	verify(legacyRestore)
	modernRestore := restore(legacyRestore)
	upgrade(modernRestore)
	verify(modernRestore)
}
