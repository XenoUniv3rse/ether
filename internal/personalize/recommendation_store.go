package personalize

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("personalize", 7, `
CREATE TABLE personalize_recommendation_days (
  day INTEGER NOT NULL,
  kind TEXT NOT NULL,
  user_id INTEGER NOT NULL,
  hub_id TEXT NOT NULL,
  app TEXT NOT NULL,
  title TEXT NOT NULL,
  position INTEGER NOT NULL,
  play_id INTEGER NOT NULL DEFAULT 0,
  completed INTEGER NOT NULL DEFAULT 0,
  watched_ms INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (day, user_id, hub_id, app)
);
CREATE INDEX personalize_rec_days_kind ON personalize_recommendation_days(kind, day);
CREATE INDEX personalize_rec_days_viewer ON personalize_recommendation_days(user_id, day);
CREATE TABLE personalize_recommendation_entries (
  user_id INTEGER NOT NULL,
  item_key INTEGER NOT NULL,
  day INTEGER NOT NULL,
  hub_id TEXT NOT NULL,
  app TEXT NOT NULL,
  last_at INTEGER NOT NULL,
  rank INTEGER NOT NULL,
  PRIMARY KEY (user_id, item_key, day, hub_id, app)
);
CREATE INDEX personalize_rec_entries_day ON personalize_recommendation_entries(day);
CREATE TABLE personalize_recommendation_titles (
  kind TEXT NOT NULL,
  day INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  item_key INTEGER NOT NULL,
  impressions INTEGER NOT NULL DEFAULT 0,
  plays INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (kind, day, user_id, item_key)
);
CREATE INDEX personalize_rec_titles_day ON personalize_recommendation_titles(day);
CREATE INDEX personalize_rec_titles_viewer ON personalize_recommendation_titles(user_id, kind, day);
CREATE TABLE personalize_recommendation_starts (
  day INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  starts INTEGER NOT NULL,
  attributed INTEGER NOT NULL,
  PRIMARY KEY (day, user_id)
);
CREATE INDEX personalize_rec_starts_viewer ON personalize_recommendation_starts(user_id, day);
CREATE TABLE personalize_recommendation_deliveries (
  id TEXT PRIMARY KEY,
  day INTEGER NOT NULL,
  user_id INTEGER NOT NULL
);
CREATE INDEX personalize_rec_deliveries_day ON personalize_recommendation_deliveries(day);
CREATE INDEX personalize_rec_deliveries_viewer ON personalize_recommendation_deliveries(user_id);
`)
}

func recommendationApp(surface core.Protocol) string {
	switch surface {
	case "plex", "jellyfin", "emby", "vela", "silo", "stremio", "xtream":
		return string(surface)
	default:
		return "other"
	}
}

func recommendationKind(kind string) bool {
	switch kind {
	case "", KindContinue, KindUpNext, KindPlaylists, KindFavorites:
		return false
	default:
		return true
	}
}

type recommendationCohort struct {
	day      int
	user     int64
	hub, app string
}

type recommendationExposure struct {
	kind, title string
	at          int64
	position    int
}

type recommendationEntry struct {
	recommendationCohort
	key core.RatingKey
}

type recommendationTitleDay struct {
	kind string
	day  int
	user int64
	key  core.RatingKey
}

// One exposure per viewer, concrete row, app and local day. The first
// observed position is kept so refreshing Home cannot shift its denominator.
// Entries deduplicate the title counters without reading the raw impression log.
func (s *sqlStore) writeRecommendationImpressions(ctx context.Context, tx *sql.Tx, batch []impression) error {
	cohorts := map[recommendationCohort]recommendationExposure{}
	type observation struct {
		at   int64
		rank int
	}
	entries := map[recommendationEntry]observation{}
	for _, imp := range batch {
		observed := imp.observedAt
		if observed.IsZero() {
			observed = imp.at
		}
		day, at := dayOf(observed, s.loc), ms(observed)
		for _, row := range imp.rows {
			if imp.userID <= 0 || len(row.keys) == 0 {
				continue
			}
			cohort := recommendationCohort{day, imp.userID, row.hubID, imp.app}
			if old, exists := cohorts[cohort]; (!exists || at < old.at) && !row.skipAnalytics && recommendationKind(row.kind) {
				pos := 0
				if imp.home {
					pos = row.position + 1
				}
				cohorts[cohort] = recommendationExposure{row.kind, row.title, at, pos}
			}
			for index, key := range row.keys {
				if key != 0 {
					entry := recommendationEntry{cohort, key}
					rank := row.position*1000 + row.itemOffset + index
					if old, exists := entries[entry]; !exists || at > old.at || (at == old.at && rank < old.rank) {
						entries[entry] = observation{at, rank}
					}
				}
			}
		}
	}
	cohortKeys := slices.SortedFunc(maps.Keys(cohorts), compareRecommendationCohort)
	for chunk := range slices.Chunk(cohortKeys, 100) {
		args := make([]any, 0, len(chunk)*7)
		for _, key := range chunk {
			row := cohorts[key]
			args = append(args, key.day, row.kind, key.user, key.hub, key.app, row.title, row.position)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO personalize_recommendation_days
			(day, kind, user_id, hub_id, app, title, position) VALUES `+recommendationValues(len(chunk), 7)+
			` ON CONFLICT(day, user_id, hub_id, app) DO NOTHING`, args...)
		if err != nil {
			return err
		}
	}

	titles := map[recommendationTitleDay]int{}
	entryKeys := slices.SortedFunc(maps.Keys(entries), func(a, b recommendationEntry) int {
		return cmp.Or(cmp.Compare(a.user, b.user), cmp.Compare(a.key, b.key), compareRecommendationCohort(a.recommendationCohort, b.recommendationCohort))
	})
	for chunk := range slices.Chunk(entryKeys, 100) {
		args := make([]any, 0, len(chunk)*7)
		for _, entry := range chunk {
			seen := entries[entry]
			args = append(args, entry.user, int64(entry.key), entry.day, entry.hub, entry.app, seen.at, seen.rank)
		}
		insert := `INSERT INTO personalize_recommendation_entries
			(user_id, item_key, day, hub_id, app, last_at, rank) VALUES `
		conflict := ` ON CONFLICT(user_id, item_key, day, hub_id, app) `
		rows, err := tx.QueryContext(ctx, insert+recommendationValues(len(chunk), 7)+conflict+
			`DO NOTHING RETURNING user_id, item_key, day, hub_id, app`, args...)
		if err != nil {
			return err
		}
		fresh := make(map[recommendationEntry]bool, len(chunk))
		for rows.Next() {
			var entry recommendationEntry
			if err := rows.Scan(&entry.user, &entry.key, &entry.day, &entry.hub, &entry.app); err != nil {
				rows.Close()
				return err
			}
			fresh[entry] = true
			if row, ok := cohorts[entry.recommendationCohort]; ok {
				titles[recommendationTitleDay{row.kind, entry.day, entry.user, entry.key}]++
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		// Refresh attribution evidence for repeated deliveries without
		// creating new row or title impressions.
		args = args[:0]
		for _, entry := range chunk {
			if !fresh[entry] {
				seen := entries[entry]
				args = append(args, entry.user, int64(entry.key), entry.day, entry.hub, entry.app, seen.at, seen.rank)
			}
		}
		if len(args) > 0 {
			_, err := tx.ExecContext(ctx, insert+recommendationValues(len(args)/7, 7)+conflict+
				`DO UPDATE SET last_at = excluded.last_at, rank = excluded.rank
				WHERE personalize_recommendation_entries.last_at < excluded.last_at
				   OR (personalize_recommendation_entries.last_at = excluded.last_at
				       AND personalize_recommendation_entries.rank > excluded.rank)`, args...)
			if err != nil {
				return err
			}
		}
	}

	ordered := slices.SortedFunc(maps.Keys(titles), func(a, b recommendationTitleDay) int {
		return cmp.Or(cmp.Compare(a.kind, b.kind), cmp.Compare(a.day, b.day), cmp.Compare(a.user, b.user), cmp.Compare(a.key, b.key))
	})
	for chunk := range slices.Chunk(ordered, 100) {
		args := make([]any, 0, len(chunk)*5)
		for _, title := range chunk {
			args = append(args, title.kind, title.day, title.user, int64(title.key), titles[title])
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO personalize_recommendation_titles
			(kind, day, user_id, item_key, impressions) VALUES `+recommendationValues(len(chunk), 5)+
			` ON CONFLICT(kind, day, user_id, item_key) DO UPDATE SET
			impressions = personalize_recommendation_titles.impressions + excluded.impressions`, args...)
		if err != nil {
			return err
		}
	}
	return nil
}

func compareRecommendationCohort(a, b recommendationCohort) int {
	return cmp.Or(cmp.Compare(a.day, b.day), cmp.Compare(a.user, b.user), cmp.Compare(a.hub, b.hub), cmp.Compare(a.app, b.app))
}

// recommendationValues bounds each batched statement to 800 bind parameters.
func recommendationValues(rows, width int) string {
	tuple := "(" + placeholders(width) + ")"
	return strings.Repeat(tuple+",", rows-1) + tuple
}

type recommendationMatch struct {
	recommendationCohort
	kind string
	item int64
}

func (s *sqlStore) recordRecommendationStart(ctx context.Context, tx *sql.Tx, p *playRecord, keys []core.RatingKey, surface core.Protocol) error {
	var match recommendationMatch
	match.user = p.userID
	since := p.started.Add(-attributionWindow)
	args := []any{p.userID, ms(since), ms(p.started), dayOf(since, s.loc), dayOf(p.started, s.loc)}
	// Use the current play's app. The legacy recommendation trainer also
	// infers the last matching row, but its raw impressions combine apps.
	query := `SELECT e.day, e.hub_id, e.app, e.item_key, COALESCE(d.kind, '')
		FROM personalize_recommendation_entries e LEFT JOIN personalize_recommendation_days d
		ON d.day = e.day AND d.user_id = e.user_id AND d.hub_id = e.hub_id AND d.app = e.app
		WHERE e.user_id = ? AND e.last_at BETWEEN ? AND ? AND e.day BETWEEN ? AND ?
		AND e.item_key IN (` + placeholders(len(keys)) + `)`
	for _, key := range keys {
		args = append(args, int64(key))
	}
	if surface != "" {
		query += ` AND e.app = ?`
		args = append(args, recommendationApp(surface))
	}
	err := tx.QueryRowContext(ctx, query+` ORDER BY e.last_at DESC, e.rank, e.hub_id, e.app, e.item_key LIMIT 1`, args...).
		Scan(&match.day, &match.hub, &match.app, &match.item, &match.kind)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if match.kind != "" {
		res, err := tx.ExecContext(ctx, `UPDATE personalize_recommendation_days SET play_id = ?
			WHERE day = ? AND user_id = ? AND hub_id = ? AND app = ? AND play_id = 0`,
			p.id, match.day, match.user, match.hub, match.app)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			p.recommendation = match
		}
		_, err = tx.ExecContext(ctx, `UPDATE personalize_recommendation_titles
			SET plays = plays + 1 WHERE kind = ? AND day = ? AND user_id = ? AND item_key = ?`,
			match.kind, match.day, match.user, match.item)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO personalize_recommendation_starts (day, user_id, starts, attributed)
		VALUES (?, ?, 1, ?) ON CONFLICT(day, user_id) DO UPDATE SET
		starts = personalize_recommendation_starts.starts + 1,
		attributed = personalize_recommendation_starts.attributed + excluded.attributed`,
		dayOf(p.started, s.loc), p.userID, store.BoolInt(match.kind != ""))
	return err
}

func (s *sqlStore) insertDeliveredPlay(ctx context.Context, play deliveredPlay) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO personalize_recommendation_deliveries (id, day, user_id)
		VALUES (?, ?, ?) ON CONFLICT(id) DO NOTHING`, play.id, dayOf(play.started, s.loc), play.viewer)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	// Positive IDs point to tracked timeline sessions. A delivered start
	// converts an exposure with an unknown completion and watch duration.
	p := playRecord{id: -1, userID: play.viewer, started: play.started}
	if err := s.recordRecommendationStart(ctx, tx, &p, attributionKeys(play.key), play.app); err != nil {
		return err
	}
	return tx.Commit()
}

func updateRecommendationPlay(ctx context.Context, tx *sql.Tx, p *playRecord) error {
	m := p.recommendation
	if m.day == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE personalize_recommendation_days SET completed = ?, watched_ms = ?
		WHERE day = ? AND user_id = ? AND hub_id = ? AND app = ? AND play_id = ?`,
		store.BoolInt(p.completed), p.watched().Milliseconds(), m.day, m.user, m.hub, m.app, p.id)
	return err
}
