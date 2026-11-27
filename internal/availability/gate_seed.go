package availability

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

const gateSeedBatch = 64

func init() {
	store.Register("availability_gate", 2, `
CREATE TABLE availability_gate_seed (
 policy TEXT PRIMARY KEY,
 cursor_key INTEGER NOT NULL,
 scanned INTEGER NOT NULL,
 admitted INTEGER NOT NULL
);`)
}

type gateSeedState struct {
	running, complete bool
	retryAt           time.Time
}

// requestGateSeed schedules work on the service lifetime. The caller never
// checks the database or waits for a batch.
func (s *Service) requestGateSeed(input discoverySnapshot) bool {
	if !s.d.Async || !s.hasGateTable || input.gatePolicy == "" ||
		(input.availability.ModeFor(hiddenShows) != config.ModePlayable &&
			(input.availability.ModeFor(hiddenMovies) != config.ModePlayable || input.availability.DisableMovieEvidenceGate)) {
		return false
	}
	policy := input.gatePolicy
	s.visibilityMu.Lock()
	defer s.visibilityMu.Unlock()
	state := s.gateSeeds[policy]
	if s.closed || state.complete {
		return false
	}
	if state.running || time.Now().Before(state.retryAt) {
		return true
	}
	if s.gateSeeds == nil {
		s.gateSeeds = make(map[string]gateSeedState)
	}
	s.gateSeeds[policy] = gateSeedState{running: true}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		complete, err := s.seedGatePolicy(core.WithLocalResponse(s.bgCtx), input)
		if err != nil && s.bgCtx.Err() == nil {
			s.log.Warn("availability gate seeding paused", "error", err)
		}
		s.visibilityMu.Lock()
		s.gateSeeds[policy] = gateSeedState{complete: complete, retryAt: time.Now().Add(time.Minute)}
		s.visibilityMu.Unlock()
	}()
	return true
}

func (s *Service) seedGatePolicy(ctx context.Context, input discoverySnapshot) (bool, error) {
	policy := input.gatePolicy
	unlock, err := store.SessionLock(ctx, s.db, "availability-gate-seed:"+policy)
	if err != nil {
		return false, err
	}
	if unlock != nil {
		defer unlock()
	}
	var done int64
	err = s.db.QueryRowContext(ctx, `SELECT seeded_at FROM availability_gate_policy WHERE policy=?`, policy).Scan(&done)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	var cursor core.RatingKey
	var scanned, admitted int
	err = s.db.QueryRowContext(ctx, `SELECT cursor_key,scanned,admitted FROM availability_gate_seed WHERE policy=?`, policy).Scan(&cursor, &scanned, &admitted)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	start := time.Now()
	s.log.Info("availability gate seeding started", "scanned", scanned, "admitted", admitted)
	feeds := make(map[string]core.ReleaseFeed, len(input.feeds))
	for _, feed := range input.feeds {
		feeds[feed.ID] = feed
	}
	ctx = offRequest(ctx)
	for batch := 1; ; batch++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if s.currentGatePolicy() != policy {
			s.log.Info("availability gate seeding paused after policy change", "scanned", scanned, "admitted", admitted, "duration", time.Since(start))
			return false, nil
		}
		keys, err := s.gateSeedKeys(ctx, input, cursor)
		if err != nil {
			return false, err
		}
		if len(keys) == 0 {
			break
		}
		open := s.openGateKeys(ctx, policy, keys)
		pending := make([]core.RatingKey, 0, len(keys))
		for _, key := range keys {
			if !open[key] {
				pending = append(pending, key)
			}
		}
		titles, err := s.gateSeedTitles(ctx, pending, input.now)
		if err != nil {
			return false, err
		}
		if err := s.reopenRetainedGates(ctx, titles, feeds, input, open); err != nil {
			return false, err
		}
		cursor = keys[len(keys)-1]
		scanned += len(keys)
		admitted += len(open)
		// Gates commit first. A restart between commits only replays one
		// batch and sees its previously opened gates.
		if _, err := s.db.ExecContext(ctx, `INSERT INTO availability_gate_seed(policy,cursor_key,scanned,admitted) VALUES(?,?,?,?)
 ON CONFLICT(policy) DO UPDATE SET cursor_key=excluded.cursor_key,scanned=excluded.scanned,admitted=excluded.admitted`, policy, int64(cursor), scanned, admitted); err != nil {
			return false, err
		}
		if batch%16 == 0 {
			s.log.Info("availability gate seeding progress", "scanned", scanned, "admitted", admitted, "duration", time.Since(start))
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO availability_gate_policy(policy,seeded_at) VALUES(?,?) ON CONFLICT(policy) DO NOTHING`, policy, s.now().Unix()); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM availability_gate_seed WHERE policy=?`, policy); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	s.gateChanged()
	s.log.Info("availability gate seeding finished", "scanned", scanned, "admitted", admitted, "duration", time.Since(start))
	return true, nil
}

func (s *Service) gateSeedKeys(ctx context.Context, input discoverySnapshot, cursor core.RatingKey) ([]core.RatingKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT rating_key FROM availability_releases
 WHERE rating_key>? AND last_seen>=? AND state<>'unavailable' AND NOT (state='unknown' AND revision>0)
 ORDER BY rating_key LIMIT ?`, int64(cursor), input.retentionCutoff().Unix(), gateSeedBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []core.RatingKey
	for rows.Next() {
		var key core.RatingKey
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Service) gateSeedTitles(ctx context.Context, keys []core.RatingKey, now time.Time) (map[core.RatingKey]*core.Title, error) {
	titles := make(map[core.RatingKey]*core.Title, len(keys))
	if len(keys) == 0 {
		return titles, nil
	}
	rows, err := s.d.Catalog.CachedTitles(ctx, keys)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		titles[rows[i].Key] = &rows[i]
	}
	// A feed-first S01E01 can have evidence before it has metadata.
	var parents []core.RatingKey
	for _, key := range keys {
		if titles[key] == nil && key.Kind() == core.KindEpisode && key.TMDB() > 0 && key.Season() == 1 && key.Episode() == 1 {
			parents = append(parents, core.ShowKey(key.TMDB()))
		}
	}
	if len(parents) > 0 {
		shows, err := s.d.Catalog.CachedTitles(ctx, parents)
		if err != nil {
			return nil, err
		}
		for _, show := range shows {
			key := core.EpisodeKey(show.Key.TMDB(), 1, 1)
			if titles[key] == nil && show.Key.TMDB() > 0 && show.EpisodeOrder.Source != core.MetadataTVDB && !show.Adult && show.IsReleased(now) {
				titles[key] = &core.Title{Key: key, Type: core.TypeEpisode, ShowKey: show.Key, ShowIDs: show.IDs,
					ShowTitle: show.Title, ShowOriginalTitle: show.OriginalTitle, ShowYear: show.Year,
					Season: 1, Episode: 1, Released: show.Released, Language: show.Language}
			}
		}
	}
	return titles, nil
}

// provisionalGateKeys mirrors the SQL listing admission for the bounded set
// of titles already loaded for detail, children or another direct read.
func (s *Service) provisionalGateKeys(ctx context.Context, titles map[core.RatingKey]*core.Title, input discoverySnapshot, acceptable map[core.RatingKey]bool) {
	var keys []core.RatingKey
	for key := range titles {
		if !acceptable[key] {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return
	}
	passed := s.officialDatesPassed(ctx, titles, input.now)
	for batch := range store.Chunks(keys, 500) {
		args := []any{input.gatePolicy, input.retentionCutoff().Unix()}
		for _, key := range batch {
			args = append(args, int64(key))
		}
		rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT ap.rating_key,ap.state FROM availability_releases ap
 WHERE NOT EXISTS (SELECT 1 FROM availability_gate_policy apc WHERE apc.policy=?)
 AND ap.last_seen>=? AND ap.state IN ('verified','observed') AND ap.rating_key IN (`+store.Placeholders(len(batch))+`)`, args...)
		if err != nil {
			return
		}
		for rows.Next() {
			var key core.RatingKey
			var state string
			if rows.Scan(&key, &state) == nil && (state == "verified" || passed[key]) {
				acceptable[key] = true
			}
		}
		rows.Close()
	}
}
