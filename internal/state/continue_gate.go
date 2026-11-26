package state

import (
	"context"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("state_history_playback_gate", 1, `CREATE INDEX state_continue_playback_gate ON state_history(user_id,view_type,completed,rating_key);`)
}

// PlaybackCompletedShows checks the complete history, independent of current
// watched flags and shelf recency. Each show is an indexed EXISTS range seek.
func (s *State) PlaybackCompletedShows(ctx context.Context, uid int64, shows []core.RatingKey) (map[core.RatingKey]bool, error) {
	out := make(map[core.RatingKey]bool, len(shows))
	for chunk := range store.Chunks(shows, 200) {
		args := make([]any, 0, 3*len(chunk)+2)
		for _, show := range chunk {
			var lo, hi core.RatingKey
			if !show.Allocated() {
				lo, hi = core.EpisodeKey(show.TMDB(), 0, 0), core.EpisodeKey(show.TMDB()+1, 0, 0)
			}
			args = append(args, show, lo, hi)
		}
		args = append(args, uid, uid)
		values := strings.TrimSuffix(strings.Repeat("(CAST(? AS BIGINT),CAST(? AS BIGINT),CAST(? AS BIGINT)),", len(chunk)), ",")
		rows, err := s.db.QueryContext(ctx, `WITH wanted(show_key,lo,hi) AS (VALUES `+values+`)
SELECT w.show_key, EXISTS(SELECT 1 FROM state_history WHERE user_id=? AND view_type=0 AND completed=1 AND rating_key>=w.lo AND rating_key<w.hi
 UNION ALL SELECT 1 FROM catalog_allocated_child a JOIN state_history h ON h.rating_key=a.key
 WHERE a.show_key=w.show_key AND a.kind=4 AND h.user_id=? AND h.view_type=0 AND h.completed=1)
FROM wanted w`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var show core.RatingKey
			var yes bool
			if err := rows.Scan(&show, &yes); err != nil {
				rows.Close()
				return nil, err
			}
			out[show] = yes
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func init() {
	store.Register("state_watch_recent_settings", 1, `CREATE INDEX state_watch_recent_settings ON state_watch(user_id,updated_at DESC,rating_key) WHERE last_viewed_at>0;`)
}

func init() {
	// state_watch_played serves a viewer's played-episodes listing (Jellyfin
	// Filters=IsPlayed, SortBy=DatePlayed): the played rows in last-played
	// order, so the page follows the viewer's history, not the library
	// (#405). view_count>0 keeps unplayed rows out of the index entirely.
	store.Register("state_watch_played", 1, `CREATE INDEX state_watch_played ON state_watch(user_id,last_viewed_at DESC,rating_key) WHERE view_count>0;`)
}

// RecentlyViewedSettings reads a bounded viewer prefix in settings-write order.
func (s *State) RecentlyViewedSettings(ctx context.Context, uid int64, _ time.Time, limit int) ([]core.HistoryEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rating_key,last_viewed_at FROM state_watch WHERE user_id=? AND last_viewed_at>0 ORDER BY updated_at DESC,rating_key ASC LIMIT ?`, uid, min(max(limit, 1), 1000))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.HistoryEntry
	for rows.Next() {
		var h core.HistoryEntry
		var at int64
		if err := rows.Scan(&h.Key, &at); err != nil {
			return nil, err
		}
		h.ViewedAt = time.Unix(0, at)
		out = append(out, h)
	}
	return out, rows.Err()
}

// Match both recency terms of InProgress, including imported rows sharing a
// last-viewed timestamp. Both SQLite and PostgreSQL support this partial index.
func init() {
	store.Register("state_resume_order", 1, `CREATE INDEX state_resume_order ON state_watch(user_id,last_viewed_at DESC,updated_at DESC) WHERE view_offset_ns>0;`)
}
