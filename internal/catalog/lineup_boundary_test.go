package catalog

import (
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestNextLineupBoundary(t *testing.T) {
	c := newChunkTestCatalog(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	add := func(key core.RatingKey, kind core.MediaType, at time.Time) {
		t.Helper()
		if err := c.UpsertTitle(ctx, core.Title{Key: key, Type: kind, Title: "Lineup title", Released: at}, false); err != nil {
			t.Fatal(err)
		}
	}
	check := func(at time.Time, cfg config.Availability, want time.Time) {
		t.Helper()
		got, err := c.NextLineupBoundary(ctx, at, cfg)
		if err != nil || !got.Equal(want) {
			t.Fatalf("next lineup boundary at %v: got %v (%v), want %v", at, got, err, want)
		}
	}
	immediate := config.Availability{Mode: config.ModeImmediate}
	check(now, immediate, time.Time{})
	air := now.Add(time.Hour)
	add(core.EpisodeKey(99, 1, 1), core.TypeEpisode, air)
	check(now, immediate, air)
	check(air, immediate, time.Time{})
	playableShows := config.Availability{MovieMode: config.ModeImmediate, ShowMode: config.ModePlayable}
	check(air, playableShows, air.Add(24*time.Hour))

	movie := core.MovieKey(101)
	add(movie, core.TypeMovie, now.Add(-180*24*time.Hour))
	home := now.Add(-24*time.Hour + 10*time.Minute)
	if _, err := c.db.ExecContext(ctx, "INSERT INTO catalog_home_release(key,checked,earliest_ts) VALUES (?,1,?)", movie, home.Unix()); err != nil {
		t.Fatal(err)
	}
	check(now, config.Availability{Mode: config.ModeDigital, HomeReleaseOffsetDays: 1}, now.Add(10*time.Minute))
}
