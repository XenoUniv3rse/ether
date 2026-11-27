package livetv

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

// Catch-up watch state: one row per viewer and airing. The guide keys
// (source, guide channel, start) are the airing's livetv_programmes row, so a
// guide import that drops the airing from the archive prunes its state too.
func init() {
	store.Register("livetv", 28, `CREATE TABLE livetv_catchup_watch(
		user_id BIGINT NOT NULL,
		programme_id TEXT NOT NULL,
		guide_source TEXT NOT NULL,
		guide_id TEXT NOT NULL,
		start_ms BIGINT NOT NULL,
		offset_ms BIGINT NOT NULL DEFAULT 0,
		view_count INTEGER NOT NULL DEFAULT 0,
		counted INTEGER NOT NULL DEFAULT 0,
		last_viewed_ms BIGINT NOT NULL DEFAULT 0,
		PRIMARY KEY(user_id, programme_id));
	CREATE INDEX livetv_catchup_watch_guide ON livetv_catchup_watch(guide_source, guide_id, start_ms);
	CREATE INDEX livetv_catchup_watch_start ON livetv_catchup_watch(start_ms);`)
}

// catchUpWatchMaxAge bounds state for airings whose source no longer imports
// (archives are capped at 30 days).
const catchUpWatchMaxAge = 31 * 24 * time.Hour

// CatchUpProgress records a timeline update for a catch-up airing with the
// thresholds normal titles use: crossing state.WatchedThreshold marks it
// watched once per viewing and clears the resume point; any other started
// position is the resume point. Buffering reports are ignored, and a position
// before the resume threshold clears an existing resume point only when the
// player stops there (a resumed play reports 0 before it seeks). Each write is
// one statement, so concurrent reports cannot lose an update.
func (h liveTVHost) CatchUpProgress(ctx context.Context, u *frontend.Viewer, id string, offset, duration time.Duration, st string, resumeAfter time.Duration) error {
	if st == "buffering" {
		return nil
	}
	ctx, cancel := catchUpWriteContext(ctx)
	defer cancel()
	a, err := h.catchUpAiring(ctx, u, id)
	if err != nil {
		return err
	}
	// The airing's guide length is the duration; a client's figure can be a
	// stale estimate. A position well past the end is a previous item's
	// playhead, not this airing's (as for titles), so it is ignored.
	dur := a.length
	if dur <= 0 {
		dur = duration
	}
	offset = max(offset, 0)
	if dur > 0 && offset-dur > 2*time.Second {
		return nil
	}
	if dur > 0 {
		offset = min(offset, dur)
	}
	switch {
	case dur > 0 && float64(offset) >= state.WatchedThreshold*float64(dur):
		return a.upsert(ctx, h.s, 0, `offset_ms=0, view_count=CASE WHEN livetv_catchup_watch.counted=0 THEN livetv_catchup_watch.view_count+1 ELSE livetv_catchup_watch.view_count END, counted=1`, 1, 1)
	case state.NotStarted(offset, dur, resumeAfter):
		if st != "stopped" {
			return nil
		}
		return catchUpExec(ctx, h.s, `UPDATE livetv_catchup_watch SET offset_ms=0, last_viewed_ms=? WHERE user_id=? AND programme_id=?`, a.now, u.ID, a.programme)
	default:
		return a.upsert(ctx, h.s, offset, `offset_ms=excluded.offset_ms, counted=0`, 0, 0)
	}
}

// CatchUpSetWatched marks an airing watched (one more view, no resume point)
// or unwatched (no views, no resume point; when it was last viewed is kept,
// as for titles).
func (h liveTVHost) CatchUpSetWatched(ctx context.Context, u *frontend.Viewer, id string, watched bool) error {
	ctx, cancel := catchUpWriteContext(ctx)
	defer cancel()
	a, err := h.catchUpAiring(ctx, u, id)
	if err != nil {
		return err
	}
	if watched {
		return a.upsert(ctx, h.s, 0, `offset_ms=0, view_count=livetv_catchup_watch.view_count+1, counted=1`, 1, 1)
	}
	return catchUpExec(ctx, h.s, `UPDATE livetv_catchup_watch SET offset_ms=0, view_count=0, counted=0 WHERE user_id=? AND programme_id=?`, u.ID, a.programme)
}

// catchUpAiring is the airing a write names, resolved through CatchUpItem so
// the archive window and the viewer's channel and rating rules apply.
type catchUpAiring struct {
	user                   int64
	programme, source, gid string
	start, now             int64
	length                 time.Duration
}

func (h liveTVHost) catchUpAiring(ctx context.Context, u *frontend.Viewer, id string) (catchUpAiring, error) {
	if u == nil || u.ID <= 0 {
		return catchUpAiring{}, core.ErrUnauthorized
	}
	item, err := h.CatchUpItem(ctx, u, id)
	if err != nil {
		return catchUpAiring{}, err
	}
	if item.Type != frontend.TypeEpisode {
		return catchUpAiring{}, core.ErrNotFound
	}
	ch, err := h.s.Channel(ctx, item.Programme.ChannelID)
	if err != nil {
		return catchUpAiring{}, err
	}
	p := item.Programme
	return catchUpAiring{user: u.ID, programme: p.ID, source: guideSource(ch), gid: ch.GuideID,
		start: p.Start.UnixMilli(), now: h.s.clockNow().UnixMilli(), length: p.End.Sub(p.Start)}, nil
}

// upsert writes the airing's row: a new row gets offset, views and counted as
// given; an existing one gets set (which may read its own columns).
func (a catchUpAiring) upsert(ctx context.Context, s *Service, offset time.Duration, set string, views, counted int) error {
	return catchUpExec(ctx, s, `INSERT INTO livetv_catchup_watch(user_id,programme_id,guide_source,guide_id,start_ms,offset_ms,view_count,counted,last_viewed_ms)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(user_id,programme_id) DO UPDATE SET guide_source=excluded.guide_source, guide_id=excluded.guide_id, start_ms=excluded.start_ms,
			last_viewed_ms=excluded.last_viewed_ms, `+set,
		a.user, a.programme, a.source, a.gid, a.start, offset.Milliseconds(), views, counted, a.now)
}

// catchUpWriteTimeout bounds a watch write that outlives its request.
const catchUpWriteTimeout = 30 * time.Second

// catchUpWriteContext detaches a watch write from its request: a player that
// reports "stopped" and closes the connection still has its position saved.
func catchUpWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), catchUpWriteTimeout)
}

// catchUpExec runs one watch write, waiting out SQLite write contention (a
// guide import can hold the write gate longer than one wait) until the
// write's own deadline.
func catchUpExec(ctx context.Context, s *Service, q string, args ...any) error {
	for {
		_, err := s.o.DB.ExecContext(ctx, q, args...)
		if err == nil || !store.IsWriteContention(err) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// fillCatchUpWatch sets Watch on the episodes in items for one viewer.
func (s *Service) fillCatchUpWatch(ctx context.Context, u *core.User, items []frontend.CatchUpItem) error {
	if u == nil || u.ID <= 0 {
		return nil
	}
	at := make(map[string][]int)
	for i, item := range items {
		if item.Type == frontend.TypeEpisode && item.Programme.ID != "" {
			at[item.Programme.ID] = append(at[item.Programme.ID], i)
		}
	}
	ids := make([]string, 0, len(at))
	for id := range at {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for batch := range slices.Chunk(ids, 500) {
		args := make([]any, 0, len(batch)+1)
		args = append(args, u.ID)
		for _, id := range batch {
			args = append(args, id)
		}
		rows, err := s.o.DB.QueryContext(ctx, `SELECT programme_id, offset_ms, view_count, last_viewed_ms FROM livetv_catchup_watch
			WHERE user_id=? AND programme_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+`)`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var offsetMS, lastMS int64
			var w frontend.CatchUpWatch
			if err := rows.Scan(&id, &offsetMS, &w.ViewCount, &lastMS); err != nil {
				rows.Close()
				return err
			}
			w.ViewOffset = time.Duration(offsetMS) * time.Millisecond
			if lastMS > 0 {
				w.LastViewedAt = time.UnixMilli(lastMS)
			}
			for _, i := range at[id] {
				items[i].Watch = w
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

// pruneCatchUpWatch drops state for airings the source's guide no longer
// keeps (the archive dropped them), and any state older than the longest
// archive. It runs in the guide import's transaction.
func pruneCatchUpWatch(ctx context.Context, tx *sql.Tx, sourceID string, now time.Time) (int64, error) {
	var n int64
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM livetv_catchup_watch WHERE guide_source=? AND NOT EXISTS (SELECT 1 FROM livetv_programmes p
			WHERE p.source_id=livetv_catchup_watch.guide_source AND p.channel_id=livetv_catchup_watch.guide_id AND p.start_ms=livetv_catchup_watch.start_ms)`, []any{sourceID}},
		{`DELETE FROM livetv_catchup_watch WHERE start_ms<?`, []any{now.Add(-catchUpWatchMaxAge).UnixMilli()}},
	} {
		res, err := tx.ExecContext(ctx, q.sql, q.args...)
		if err != nil {
			return n, err
		}
		m, err := res.RowsAffected()
		if err != nil {
			return n, err
		}
		n += m
	}
	return n, nil
}
