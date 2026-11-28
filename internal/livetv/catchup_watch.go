package livetv

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
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
	store.Register("livetv", 29, `ALTER TABLE livetv_catchup_watch ADD COLUMN watch_at_ms BIGINT NOT NULL DEFAULT 0;
		ALTER TABLE livetv_catchup_watch ADD COLUMN favorite INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE livetv_catchup_watch ADD COLUMN rating DOUBLE PRECISION NOT NULL DEFAULT 0;`)
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
	case state.ReachedWatchedThreshold(offset, dur):
		return a.upsert(ctx, h.s, 0, `offset_ms=0, view_count=CASE WHEN livetv_catchup_watch.counted=0 THEN livetv_catchup_watch.view_count+1 ELSE livetv_catchup_watch.view_count END, counted=1`, 1, 1, true)
	case state.NotStarted(offset, dur, resumeAfter):
		if st != "stopped" {
			return nil
		}
		return catchUpExec(ctx, h.s, `UPDATE livetv_catchup_watch SET offset_ms=0, last_viewed_ms=?, watch_at_ms=? WHERE user_id=? AND programme_id=?`, a.now, a.now, u.ID, a.programme)
	default:
		return a.upsert(ctx, h.s, offset, `offset_ms=excluded.offset_ms, counted=0`, 0, 0, true)
	}
}

// CatchUpSetWatched marks an airing watched (one more view, no resume point)
// or unwatched (no views, no resume point; when it was last viewed is kept,
// as for titles). For a show or season it does the same to every airing in
// it the viewer can play, as a show scrobble does to its episodes.
func (h liveTVHost) CatchUpSetWatched(ctx context.Context, u *frontend.Viewer, id string, watched bool) error {
	ctx, cancel := catchUpWriteContext(ctx)
	defer cancel()
	airings, err := h.catchUpAirings(ctx, u, id)
	if err != nil {
		return err
	}
	for batch := range slices.Chunk(airings, catchUpWriteBatch) {
		if watched {
			err = catchUpUpsert(ctx, h.s, batch, 0, `offset_ms=0, view_count=livetv_catchup_watch.view_count+1, counted=1`, 1, 1, true)
		} else {
			err = catchUpUpsert(ctx, h.s, batch, 0, `offset_ms=0, view_count=0, counted=0`, 0, 0, false)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// CatchUpGroupWatch is a show's or season's roll-up of its airings' state for
// one viewer. It reads that show's airings in the archive window.
func (h liveTVHost) CatchUpGroupWatch(ctx context.Context, u *frontend.Viewer, id string) (frontend.CatchUpWatch, error) {
	item, members, err := h.catchUpGroup(ctx, u, id)
	if err != nil {
		return frontend.CatchUpWatch{}, err
	}
	items := []frontend.CatchUpItem{item}
	if err := h.s.fillCatchUpWatch(ctx, u, items, members); err != nil {
		return frontend.CatchUpWatch{}, err
	}
	return items[0].Watch, nil
}

// catchUpGroup resolves a show or season id for this viewer, with its
// airings' programme ids (keyed by the item's id), from one indexed read of
// that title's airings.
func (h liveTVHost) catchUpGroup(ctx context.Context, u *frontend.Viewer, id string) (frontend.CatchUpItem, map[string][]string, error) {
	kind := frontend.TypeShow
	if _, guidKind := frontend.CatchUpGUIDGroup(id); guidKind != "" {
		kind = guidKind
	} else if strings.HasPrefix(id, "catchup-season-") {
		kind = frontend.TypeSeason
	} else if !strings.HasPrefix(id, "catchup-show-") {
		return frontend.CatchUpItem{}, nil, core.ErrNotFound
	}
	airings, err := h.catchUpGroupAirings(ctx, u, nil, []string{id})
	if err != nil {
		return frontend.CatchUpItem{}, nil, err
	}
	found := airings[id]
	if len(found) == 0 {
		return frontend.CatchUpItem{}, nil, core.ErrNotFound
	}
	item := catchUpLibraryItem(found[0], kind)
	ids := make([]string, 0, len(found))
	for _, p := range found {
		ids = append(ids, p.ID)
	}
	return item, map[string][]string{item.ID: ids}, nil
}

// catchUpWriteBatch is how many airings one mark statement writes (ten
// parameters each, well under SQLite's limit).
const catchUpWriteBatch = 200

// catchUpAirings resolves a write's id to its airings: an episode to itself,
// a show or season to every airing in it the viewer can play, from one
// indexed read of that title's airings.
func (h liveTVHost) catchUpAirings(ctx context.Context, u *frontend.Viewer, id string) ([]catchUpAiring, error) {
	if u == nil || u.ID <= 0 {
		return nil, core.ErrUnauthorized
	}
	if _, kind := frontend.CatchUpGUIDGroup(id); kind == "" && !strings.HasPrefix(id, "catchup-show-") && !strings.HasPrefix(id, "catchup-season-") {
		a, err := h.catchUpAiring(ctx, u, id)
		return []catchUpAiring{a}, err
	}
	item, members, err := h.catchUpGroup(ctx, u, id)
	if err != nil {
		return nil, err
	}
	channels := make(map[string]Channel)
	out := make([]catchUpAiring, 0, len(members[item.ID]))
	for _, programme := range members[item.ID] {
		at := strings.LastIndexByte(programme, '-')
		start, err := strconv.ParseInt(programme[at+1:], 10, 64)
		if at <= 0 || err != nil {
			return nil, core.ErrNotFound
		}
		a, err := h.airingOf(ctx, u, programme[:at], programme, time.UnixMilli(start), 0, channels)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
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
	p := item.Programme
	return h.airingOf(ctx, u, p.ChannelID, p.ID, p.Start, p.End.Sub(p.Start), nil)
}

// airingOf is the write target for an airing the viewer was just shown to be
// able to play. channels, when not nil, caches channel lookups across the
// airings of one write.
func (h liveTVHost) airingOf(ctx context.Context, u *frontend.Viewer, channel, programme string, start time.Time, length time.Duration, channels map[string]Channel) (catchUpAiring, error) {
	ch, cached := channels[channel]
	if !cached {
		var err error
		if ch, err = h.s.Channel(ctx, channel); err != nil {
			return catchUpAiring{}, err
		}
		if channels != nil {
			channels[channel] = ch
		}
	}
	return catchUpAiring{user: u.ID, programme: programme, source: guideSource(ch), gid: ch.GuideID,
		start: start.UnixMilli(), now: h.s.clockNow().UnixMilli(), length: length}, nil
}

// upsert writes the airing's row: a new row gets offset, views and counted as
// given; an existing one gets set (which may read its own columns).
func (a catchUpAiring) upsert(ctx context.Context, s *Service, offset time.Duration, set string, views, counted int, viewed bool) error {
	return catchUpUpsert(ctx, s, []catchUpAiring{a}, offset, set, views, counted, viewed)
}

// catchUpUpsert writes several airings' rows in one statement, as upsert
// does for one. Every write stamps watch_at_ms; viewed also sets
// last_viewed_ms. The airings must be distinct (PostgreSQL refuses to update
// one row twice in a statement).
func catchUpUpsert(ctx context.Context, s *Service, airings []catchUpAiring, offset time.Duration, set string, views, counted int, viewed bool) error {
	if viewed {
		set += ", last_viewed_ms=excluded.last_viewed_ms"
	}
	values := make([]string, 0, len(airings))
	args := make([]any, 0, 10*len(airings))
	for _, a := range airings {
		var lastViewed int64
		if viewed {
			lastViewed = a.now
		}
		values = append(values, "(?,?,?,?,?,?,?,?,?,?)")
		args = append(args, a.user, a.programme, a.source, a.gid, a.start, offset.Milliseconds(), views, counted, lastViewed, a.now)
	}
	return catchUpExec(ctx, s, `INSERT INTO livetv_catchup_watch(user_id,programme_id,guide_source,guide_id,start_ms,offset_ms,view_count,counted,last_viewed_ms,watch_at_ms)
		VALUES `+strings.Join(values, ",")+`
		ON CONFLICT(user_id,programme_id) DO UPDATE SET guide_source=excluded.guide_source, guide_id=excluded.guide_id, start_ms=excluded.start_ms,
			watch_at_ms=excluded.watch_at_ms, `+set, args...)
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

// fillCatchUpWatch sets Watch on items for one viewer: an episode's own
// state, and for a show or season the roll-up of its airings in members
// (group id to programme ids, from catchUpItems). A group without members
// keeps a zero Watch.
func (s *Service) fillCatchUpWatch(ctx context.Context, u *core.User, items []frontend.CatchUpItem, members map[string][]string) error {
	if u == nil || u.ID <= 0 {
		return nil
	}
	var ids []string
	for _, item := range items {
		if item.Type == frontend.TypeEpisode {
			if item.Programme.ID != "" {
				ids = append(ids, item.Programme.ID)
			}
		} else {
			ids = append(ids, members[item.ID]...)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	state, err := s.catchUpWatchRows(ctx, u.ID, ids)
	if err != nil {
		return err
	}
	for i, item := range items {
		if item.Type == frontend.TypeEpisode {
			items[i].Watch = state[item.Programme.ID]
			continue
		}
		airings, ok := members[item.ID]
		if !ok {
			continue
		}
		var w frontend.CatchUpWatch
		for _, id := range airings {
			st := state[id]
			w.Episodes++
			if st.ViewCount > 0 {
				w.Watched++
			} else if st.ViewOffset > 0 {
				w.InProgress++
			}
			if st.LastViewedAt.After(w.LastViewedAt) {
				w.LastViewedAt = st.LastViewedAt
			}
		}
		items[i].Watch = w
	}
	return nil
}

// catchUpWatchRowsBatch is how many airings one catchUpWatchRows statement
// looks up: a page's airings are one statement, well under SQLite's 32766
// variable limit (store.restoreBatchParams relies on the same).
const catchUpWatchRowsBatch = 5000

// catchUpWatchRows reads one viewer's stored state for the given airings, by
// primary key.
func (s *Service) catchUpWatchRows(ctx context.Context, user int64, ids []string) (map[string]frontend.CatchUpWatch, error) {
	out := make(map[string]frontend.CatchUpWatch)
	for batch := range slices.Chunk(ids, catchUpWatchRowsBatch) {
		args := make([]any, 0, len(batch)+1)
		args = append(args, user)
		for _, id := range batch {
			args = append(args, id)
		}
		rows, err := s.o.DB.QueryContext(ctx, `SELECT programme_id, offset_ms, view_count, last_viewed_ms, favorite, rating FROM livetv_catchup_watch
			WHERE user_id=? AND programme_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var offsetMS, lastMS int64
			var w frontend.CatchUpWatch
			if err := rows.Scan(&id, &offsetMS, &w.ViewCount, &lastMS, &w.IsFavorite, &w.UserRating); err != nil {
				rows.Close()
				return nil, err
			}
			w.ViewOffset = time.Duration(offsetMS) * time.Millisecond
			if lastMS > 0 {
				w.LastViewedAt = time.UnixMilli(lastMS)
			}
			out[id] = w
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
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
