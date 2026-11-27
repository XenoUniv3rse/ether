package catalog

import (
	"context"
	"database/sql"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// NextLineupBoundary finds the next possible clock-driven membership change.
// Each timestamp range returns only its first candidate. This is independent
// of viewer policy: a boundary may be irrelevant to a particular viewer.
func (c *Catalog) NextLineupBoundary(ctx context.Context, now time.Time, availability config.Availability) (time.Time, error) {
	var next time.Time
	add := func(at time.Time) {
		if at.After(now) && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	seek := func(query string, offset time.Duration, args ...any) error {
		queryArgs := append(args, now.Add(-offset).Unix())
		var stamp sql.NullInt64
		if err := c.db.QueryRowContext(ctx, query, queryArgs...).Scan(&stamp); err != nil {
			return err
		}
		if stamp.Valid {
			add(time.Unix(stamp.Int64, 0).Add(offset))
		}
		return nil
	}
	const release = `SELECT MIN(released_ts) FROM catalog_titles
		WHERE type=? AND adult=0 AND release_known=1 AND released_ts>?`
	for _, kind := range []core.MediaType{core.TypeMovie, core.TypeShow, core.TypeSeason, core.TypeEpisode} {
		if err := seek(release, 0, kind); err != nil {
			return time.Time{}, err
		}
	}

	const home = `SELECT MIN(earliest_ts) FROM catalog_home_release
		WHERE checked=1 AND earliest_ts<>0 AND earliest_ts>?`
	homeOffset := time.Duration(availability.HomeReleaseOffsetDays) * 24 * time.Hour
	movieWindow := time.Duration(availability.EffectiveMovieEvidenceDays()) * 24 * time.Hour
	mode := availability.ModeFor("movie")
	if mode == config.ModeDigital || c.config().ReleaseMode == config.ModeDigital {
		if err := seek(home, homeOffset); err != nil {
			return time.Time{}, err
		}
		fallback := `SELECT MIN(t.released_ts) FROM catalog_titles t
			WHERE t.type='movie' AND t.adult=0 AND t.release_known=1 AND t.released_ts>?
			AND NOT EXISTS (SELECT 1 FROM catalog_home_release h WHERE h.key=t.key AND h.checked=1 AND h.earliest_ts<>0)`
		if err := seek(fallback, homeReleaseAge+time.Second); err != nil {
			return time.Time{}, err
		}
	}
	if c.config().ReleaseMode == config.ModeDigital && c.config().HomeReleaseOffsetDays != availability.HomeReleaseOffsetDays {
		if err := seek(home, time.Duration(c.config().HomeReleaseOffsetDays)*24*time.Hour); err != nil {
			return time.Time{}, err
		}
	}
	if mode == config.ModeDigital {
		if err := seek(home, homeOffset+movieWindow); err != nil {
			return time.Time{}, err
		}
	}
	if mode == config.ModePlayable && c.config().ShowUpcomingMovies {
		if err := seek(home, 0); err != nil {
			return time.Time{}, err
		}
	}
	if mode == config.ModePlayable && !availability.DisableMovieEvidenceGate {
		if err := seek(release, movieWindow, core.TypeMovie); err != nil {
			return time.Time{}, err
		}
	}
	if availability.ModeFor("show") == config.ModePlayable {
		for _, kind := range []core.MediaType{core.TypeShow, core.TypeEpisode} {
			if err := seek(release, 24*time.Hour, kind); err != nil {
				return time.Time{}, err
			}
		}
	}
	if c.canProjectAlternateOrders() {
		for _, table := range []string{"catalog_order_episodes", "catalog_order_seasons"} {
			var date sql.NullString
			if err := c.db.QueryRowContext(ctx, "SELECT MIN(air_date) FROM "+table+" WHERE air_date>?", now.UTC().Format(time.DateOnly)).Scan(&date); err != nil {
				return time.Time{}, err
			}
			if date.Valid {
				if at, err := time.Parse(time.DateOnly, date.String); err == nil {
					add(at)
				}
			}
		}
	}
	return next, nil
}
