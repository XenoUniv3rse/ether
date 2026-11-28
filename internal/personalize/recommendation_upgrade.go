package personalize

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

const (
	recommendationHistoryUpgrade = "recommendation-history-v1"
	recommendationHistoryBatch   = 128
)

func init() {
	store.Register("personalize", 8, `
CREATE TABLE personalize_recommendation_upgrades (name TEXT PRIMARY KEY);
`)
	store.RegisterLocalTable("personalize_recommendation_upgrades")
}

type recommendationHistoryCursor struct {
	Stage       int
	From, Until int   // local calendar days, inclusive
	End         int64 // last eligible play timestamp
	Day         int
	User, Item  int64
	Hub         string
	At, ID      int64
}

// UpgradeRecommendationHistory runs before the engine starts recording. The
// transferable checkpoint is committed with each batch so additive counters
// survive interruption and a logical restore without being applied twice. The
// local completion marker is recreated on restore, after checking the checkpoint.
func (o Options) UpgradeRecommendationHistory(ctx context.Context) error {
	o.defaults()
	s := &sqlStore{db: o.DB, loc: o.Location}
	unlock, err := store.SessionLock(ctx, s.db, recommendationHistoryUpgrade)
	if err != nil {
		return err
	}
	if unlock != nil {
		defer unlock()
	}
	var value string
	err = s.db.QueryRowContext(ctx, `SELECT name FROM personalize_recommendation_upgrades WHERE name=?`, recommendationHistoryUpgrade).Scan(&value)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	var cursor recommendationHistoryCursor
	err = s.db.QueryRowContext(ctx, `SELECT v FROM personalize_meta WHERE k=?`, recommendationHistoryUpgrade).Scan(&value)
	switch {
	case err == nil:
		if err := json.Unmarshal([]byte(value), &cursor); err != nil {
			return err
		}
	case errors.Is(err, sql.ErrNoRows):
		now := o.Now().In(s.loc)
		cursor.From = max(dayOf(now.AddDate(0, 0, -89), s.loc), dayOf(now.Add(-o.Retention), s.loc))
		cursor.Until, cursor.End = dayOf(now, s.loc), ms(now)
		// An already recording installation has authoritative counters from
		// its earliest recorded day onward, including on an imported image.
		var first sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT MIN(day) FROM (
			SELECT MIN(day) AS day FROM personalize_recommendation_days
			UNION ALL SELECT MIN(day) FROM personalize_recommendation_starts
		) AS recorded`).Scan(&first); err != nil {
			return err
		}
		if first.Valid && int(first.Int64) <= cursor.Until {
			before := dayTime(int(first.Int64), s.loc)
			cursor.Until, cursor.End = dayOf(before.AddDate(0, 0, -1), s.loc), ms(before)-1
		}
	default:
		return err
	}

	stages := []func(context.Context, *sql.Tx, *recommendationHistoryCursor) (int, error){
		backfillRecommendationRows, backfillRecommendationTitles, s.backfillRecommendationPlays,
	}
	for cursor.Stage < len(stages) {
		if err := recommendationHistoryTransaction(ctx, s.db, func(tx *sql.Tx) error {
			count, err := stages[cursor.Stage](ctx, tx, &cursor)
			if err != nil {
				return fmt.Errorf("recommendation history stage %d: %w", cursor.Stage, err)
			}
			if count < recommendationHistoryBatch {
				cursor.Stage++
				cursor.Day, cursor.User, cursor.Item, cursor.Hub = 0, 0, 0, ""
			}
			checkpoint, err := json.Marshal(cursor)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO personalize_meta(k, v) VALUES (?, ?)
				ON CONFLICT(k) DO UPDATE SET v=excluded.v`, recommendationHistoryUpgrade, string(checkpoint))
			return err
		}); err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO personalize_recommendation_upgrades(name) VALUES (?)
		ON CONFLICT(name) DO NOTHING`, recommendationHistoryUpgrade)
	return err
}

func recommendationHistoryTransaction(ctx context.Context, db *sql.DB, apply func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := apply(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func recommendationHistoryPage[T any](ctx context.Context, tx *sql.Tx, query string, args []any, scan func(*sql.Rows, *T) error) ([]T, error) {
	rows, err := tx.QueryContext(ctx, query, append(args, recommendationHistoryBatch)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := make([]T, 0, recommendationHistoryBatch)
	for rows.Next() {
		var item T
		if err := scan(rows, &item); err != nil {
			return nil, err
		}
		page = append(page, item)
	}
	return page, rows.Err()
}

func backfillRecommendationRows(ctx context.Context, tx *sql.Tx, c *recommendationHistoryCursor) (int, error) {
	type row struct {
		day, position            int
		user                     int64
		hub, kind, title, device string
	}
	page, err := recommendationHistoryPage(ctx, tx, `SELECT day, user_id, hub_id, kind, title, device, best_position
		FROM personalize_row_impressions WHERE day BETWEEN ? AND ? AND (day,user_id,hub_id)>(?,?,?)
		ORDER BY day,user_id,hub_id LIMIT ?`, []any{max(c.From, c.Day), c.Until, c.Day, c.User, c.Hub}, func(r *sql.Rows, v *row) error {
		return r.Scan(&v.day, &v.user, &v.hub, &v.kind, &v.title, &v.device, &v.position)
	})
	if err != nil {
		return 0, err
	}
	args := make([]any, 0, len(page)*7)
	for _, r := range page {
		c.Day, c.User, c.Hub = r.day, r.user, r.hub
		if r.user <= 0 || !recommendationKind(r.kind) {
			continue
		}
		position := 0
		if strings.HasPrefix(r.hub, "home.") && r.position >= 0 {
			position = r.position + 1
		}
		args = append(args, r.day, r.kind, r.user, r.hub, historicalRecommendationApp(r.device), r.title, position)
	}
	if len(args) > 0 {
		_, err := tx.ExecContext(ctx, `INSERT INTO personalize_recommendation_days
			(day, kind, user_id, hub_id, app, title, position) VALUES `+recommendationValues(len(args)/7, 7)+
			` ON CONFLICT(day, user_id, hub_id, app) DO NOTHING`, args...)
		if err != nil {
			return 0, err
		}
	}
	return len(page), nil
}

func backfillRecommendationTitles(ctx context.Context, tx *sql.Tx, c *recommendationHistoryCursor) (int, error) {
	type item struct {
		day       int
		user, key int64
		hub, kind string
	}
	page, err := recommendationHistoryPage(ctx, tx, `SELECT i.day, i.user_id, i.item_key, i.hub_id, COALESCE(r.kind,'')
		FROM personalize_item_impressions i LEFT JOIN personalize_row_impressions r
		ON r.user_id=i.user_id AND r.day=i.day AND r.hub_id=i.hub_id
		WHERE i.day BETWEEN ? AND ? AND (i.day,i.user_id,i.item_key,i.hub_id)>(?,?,?,?)
		ORDER BY i.day,i.user_id,i.item_key,i.hub_id LIMIT ?`, []any{max(c.From, c.Day), c.Until, c.Day, c.User, c.Item, c.Hub},
		func(r *sql.Rows, v *item) error { return r.Scan(&v.day, &v.user, &v.key, &v.hub, &v.kind) })
	if err != nil {
		return 0, err
	}
	titles := make(map[recommendationTitleDay]int)
	for _, item := range page {
		c.Day, c.User, c.Item, c.Hub = item.day, item.user, item.key, item.hub
		if item.user > 0 && item.key != 0 && recommendationKind(item.kind) {
			titles[recommendationTitleDay{item.kind, item.day, item.user, core.RatingKey(item.key)}]++
		}
	}
	if len(titles) > 0 {
		args := make([]any, 0, len(titles)*6)
		for title, count := range titles {
			args = append(args, title.kind, title.day, title.user, int64(title.key), count, 0)
		}
		if err := addHistoricalTitles(ctx, tx, args...); err != nil {
			return 0, err
		}
	}
	return len(page), nil
}

func (s *sqlStore) backfillRecommendationPlays(ctx context.Context, tx *sql.Tx, c *recommendationHistoryCursor) (int, error) {
	page, err := recommendationHistoryPage(ctx, tx, `SELECT id,user_id,item_key,hub_id,kind,started_at,duration_ms,start_fraction,max_fraction,completed
		FROM personalize_plays WHERE started_at BETWEEN ? AND ? AND (started_at,id)>(?,?)
		ORDER BY started_at,id LIMIT ?`, []any{max(ms(dayTime(c.From, s.loc)), c.At), c.End, c.At, c.ID}, func(r *sql.Rows, p *playRecord) error {
		var at, duration int64
		var completed int
		err := r.Scan(&p.id, &p.userID, &p.key, &p.hubID, &p.kind, &at, &duration, &p.startFraction, &p.maxFraction, &completed)
		p.started, p.duration, p.completed = fromMS(at), time.Duration(duration)*time.Millisecond, completed != 0
		return err
	})
	if err != nil {
		return 0, err
	}
	for _, p := range page {
		c.At, c.ID = ms(p.started), p.id
		if p.userID <= 0 {
			continue
		}
		attributed, err := s.convertHistoricalRecommendation(ctx, tx, c.From, p)
		if err != nil {
			return 0, err
		}
		if err := s.countRecommendationStart(ctx, tx, p.started, p.userID, attributed); err != nil {
			return 0, err
		}
	}
	return len(page), nil
}

func (s *sqlStore) convertHistoricalRecommendation(ctx context.Context, tx *sql.Tx, fromDay int, p playRecord) (bool, error) {
	if p.hubID == "" || !recommendationKind(p.kind) {
		return false, nil
	}
	var day int
	var device string
	since := p.started.Add(-attributionWindow)
	err := tx.QueryRowContext(ctx, `SELECT day,device FROM personalize_row_impressions
		WHERE user_id=? AND day BETWEEN ? AND ? AND hub_id=? AND kind=? AND first_at<=? AND last_at>=?
		ORDER BY day DESC LIMIT 1`, p.userID, max(fromDay, dayOf(since, s.loc)), dayOf(p.started, s.loc),
		p.hubID, p.kind, ms(p.started), ms(since)).Scan(&day, &device)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE personalize_recommendation_days SET play_id=?, completed=?, watched_ms=?
		WHERE day=? AND user_id=? AND hub_id=? AND app=? AND kind=? AND play_id=0`,
		p.id, store.BoolInt(p.completed), p.watched().Milliseconds(), day, p.userID, p.hubID, historicalRecommendationApp(device), p.kind)
	if err != nil {
		return false, err
	}
	// Prefer the title actually shown, for example the series containing a played episode.
	keys := attributionKeys(p.key)
	args := []any{p.userID, day, p.hubID, p.kind}
	for _, key := range keys {
		args = append(args, int64(key))
	}
	key := int64(p.key)
	err = tx.QueryRowContext(ctx, `SELECT item_key FROM personalize_item_impressions
		WHERE user_id=? AND day=? AND hub_id=? AND kind=? AND item_key IN (`+placeholders(len(keys))+`)
		ORDER BY last_at DESC,best_rank,item_key LIMIT 1`, args...).Scan(&key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return true, addHistoricalTitles(ctx, tx, p.kind, day, p.userID, key, 0, 1)
}

func addHistoricalTitles(ctx context.Context, tx *sql.Tx, args ...any) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO personalize_recommendation_titles(kind,day,user_id,item_key,impressions,plays)
		VALUES `+recommendationValues(len(args)/6, 6)+` ON CONFLICT(kind,day,user_id,item_key) DO UPDATE SET
		impressions=personalize_recommendation_titles.impressions+excluded.impressions,
		plays=personalize_recommendation_titles.plays+excluded.plays`, args...)
	return err
}

func historicalRecommendationApp(device string) string {
	app := recommendationApp(core.Protocol(strings.ToLower(strings.TrimSpace(device))))
	if app == "other" {
		return "unknown"
	}
	return app
}
