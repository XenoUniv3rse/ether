package experience

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// HomeRevision is the frontend host contract type; see
// internal/frontend/host_sessions_experience.go.
type HomeRevision = frontend.HomeRevision

// The (user_id, updated_at) index serves this lookup without walking watch rows.
const watchRevisionSQL = `COALESCE((SELECT MAX(updated_at) FROM state_watch WHERE user_id=?),0)`

const watchlistRevisionSQL = `(SELECT COUNT(*) FROM watchlist_items WHERE user_id=?),
COALESCE((SELECT MAX(added) FROM watchlist_items WHERE user_id=?),0),
COALESCE((SELECT MAX(checked) FROM watchlist_items WHERE user_id=?),0)`

type WatchlistRevision struct {
	Count, Added, Checked int64
}

func ReadWatchlistRevision(ctx context.Context, db *sql.DB, id int64) (WatchlistRevision, error) {
	var rev WatchlistRevision
	err := db.QueryRowContext(ctx, `SELECT `+watchlistRevisionSQL, id, id, id).Scan(&rev.Count, &rev.Added, &rev.Checked)
	return rev, err
}

type WatchRevision struct {
	Persisted int64
	Pending   uint64
}

func ReadWatchRevision(ctx context.Context, db *sql.DB, id int64, pending uint64) (WatchRevision, error) {
	rev := WatchRevision{Pending: pending}
	err := db.QueryRowContext(ctx, `SELECT `+watchRevisionSQL, id).Scan(&rev.Persisted)
	return rev, err
}

func ReadHomeRevision(ctx context.Context, db *sql.DB, user *core.User) (HomeRevision, error) {
	if db == nil || user == nil {
		return HomeRevision{}, sql.ErrNoRows
	}
	id := user.ID
	var watch, continueHidden, continueHiddenAt, playlists, playlistAt, watchlist, watchlistAt, watchlistChecked, favorites, exclusions, policy int64
	err := db.QueryRowContext(ctx, `SELECT
`+watchRevisionSQL+`,
(SELECT COUNT(*) FROM state_continue_hidden WHERE user_id=?),
COALESCE((SELECT MAX(hidden_at) FROM state_continue_hidden WHERE user_id=?),0),
(SELECT COUNT(*) FROM state_playlists WHERE user_id=?),
COALESCE((SELECT MAX(updated_at) FROM state_playlists WHERE user_id=?),0),
`+watchlistRevisionSQL+`,
COALESCE((SELECT updated_at FROM favorites_mirror WHERE viewer_id=?),0),
COALESCE((SELECT revision FROM notinterested_revisions WHERE viewer_id=?),0),
COALESCE((SELECT generation FROM viewer_policy_epoch WHERE id=1),0)`, id, id, id, id, id, id, id, id, id, id).
		Scan(&watch, &continueHidden, &continueHiddenAt, &playlists, &playlistAt, &watchlist, &watchlistAt, &watchlistChecked, &favorites, &exclusions, &policy)
	if err != nil {
		return HomeRevision{}, err
	}
	rules, err := catalog.ReadViewerRulesRevision(ctx, db, id)
	if err != nil {
		return HomeRevision{}, err
	}
	b := make([]byte, 0, 112)
	for i, n := range []int64{watch, continueHidden, continueHiddenAt, playlists, playlistAt, watchlist, watchlistAt, watchlistChecked, favorites, exclusions, rules} {
		if i > 0 {
			b = append(b, '.')
		}
		b = strconv.AppendInt(b, n, 36)
	}
	return HomeRevision{Viewer: string(b), Policy: policy}, nil
}
