package livetv

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/state"
)

func (h liveTVHost) CatchUpPatchUserData(ctx context.Context, v *frontend.Viewer, id string, patch frontend.CatchUpUserDataPatch) error {
	ctx, cancel := catchUpWriteContext(ctx)
	defer cancel()
	a, err := h.catchUpAiring(ctx, v, id)
	if err != nil {
		return err
	}
	if !patch.Empty() {
		at := a.now
		if patch.LastPlayedAt != nil {
			at = min(at, patch.LastPlayedAt.UnixMilli())
		}
		position, count, watched := state.ResolveItemPlayback(patch.ItemPlaybackPatch, a.length)
		var offset, viewed *int64
		if position != nil {
			ms := position.Milliseconds()
			offset = &ms
		}
		if patch.LastPlayedAt != nil || patch.Position != nil || watched {
			viewed = &at
		}
		// The watermark includes explicit unwatch operations, which leave
		// LastPlayedDate alone. Compare during the write to fence live reports.
		// Go bools bind as 0/1 on both engines (store/pgdriver.go), so flags
		// are compared as numbers: a bare boolean ? fails on PostgreSQL. Times
		// are cast: COALESCE(?,0) alone binds as a 32-bit integer there.
		err := catchUpExec(ctx, h.s, `INSERT INTO livetv_catchup_watch
			(user_id,programme_id,guide_source,guide_id,start_ms,offset_ms,view_count,counted,last_viewed_ms,watch_at_ms)
			VALUES(?,?,?,?,?,COALESCE(CAST(? AS BIGINT),0),CASE WHEN ?<>0 AND COALESCE(?,0)=0 THEN 1 ELSE COALESCE(?,0) END,
				CASE WHEN ?<>0 OR (?=0 AND COALESCE(?,0)>0) THEN 1 ELSE 0 END,COALESCE(CAST(? AS BIGINT),0),?)
			ON CONFLICT(user_id,programme_id) DO UPDATE SET
				offset_ms=COALESCE(?,livetv_catchup_watch.offset_ms),
				view_count=CASE WHEN ?<>0 AND COALESCE(?,livetv_catchup_watch.view_count)=0 THEN 1 ELSE COALESCE(?,livetv_catchup_watch.view_count) END,
				counted=CASE WHEN ?<>0 THEN 1 WHEN ?<>0 THEN 0 WHEN ?<>0 THEN CASE WHEN ?>0 THEN 1 ELSE 0 END ELSE livetv_catchup_watch.counted END,
				last_viewed_ms=COALESCE(?,livetv_catchup_watch.last_viewed_ms), watch_at_ms=excluded.watch_at_ms
			WHERE livetv_catchup_watch.watch_at_ms<excluded.watch_at_ms AND livetv_catchup_watch.last_viewed_ms<excluded.watch_at_ms`,
			a.user, a.programme, a.source, a.gid, a.start, offset, watched, count, count, watched, offset != nil, count, viewed, at,
			offset, watched, count, count, watched, offset != nil, count != nil, count, viewed)
		if err != nil {
			return err
		}
	}
	if patch.Favorite == nil && patch.Rating == nil && patch.Likes == nil {
		return nil
	}
	favorite, rating := 0, float64(0)
	if patch.Favorite != nil && *patch.Favorite {
		favorite = 1
	}
	// As for titles, Likes preserves a numeric rating in the matching band.
	setRating := "livetv_catchup_watch.rating"
	switch {
	case patch.Rating != nil:
		rating = *patch.Rating
		if patch.Likes != nil {
			rating = core.RatingWithLikes(rating, patch.Likes)
		}
		setRating = "excluded.rating"
	case patch.Likes != nil:
		rating = core.RatingWithLikes(0, patch.Likes)
		if *patch.Likes {
			setRating = "CASE WHEN livetv_catchup_watch.rating<7 THEN excluded.rating ELSE livetv_catchup_watch.rating END"
		} else {
			setRating = "CASE WHEN livetv_catchup_watch.rating=0 OR livetv_catchup_watch.rating>3 THEN excluded.rating ELSE livetv_catchup_watch.rating END"
		}
	}
	return catchUpExec(ctx, h.s, `INSERT INTO livetv_catchup_watch(user_id,programme_id,guide_source,guide_id,start_ms,favorite,rating)
		VALUES(?,?,?,?,?,?,?) ON CONFLICT(user_id,programme_id) DO UPDATE SET
		favorite=CASE WHEN ?<>0 THEN excluded.favorite ELSE livetv_catchup_watch.favorite END, rating=`+setRating,
		a.user, a.programme, a.source, a.gid, a.start, favorite, rating, patch.Favorite != nil)
}
