package app

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/availability"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/watchlist"
)

func (a *App) buildAvailability() error {
	s, err := availability.New(availability.Deps{Clustered: a.clustered(), DB: a.db, Config: a.cfg.Read, ConfigGeneration: a.cfg.Generation, Feeds: a.indexers, Catalog: a.catalog, Validator: a.resolver,
		CacheCheck: a.debrid.CheckCached, PackFiles: a.debrid.CachedPackMatcher, Search: func(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
			if a.indexers == nil {
				return nil, nil
			}
			return a.indexers.Search(ctx, req)
		}, Watchlist: func(ctx context.Context) (map[core.RatingKey]bool, error) {
			out := map[core.RatingKey]bool{}
			keys, err := watchlist.ListedTitleKeys(ctx, a.db)
			if err != nil {
				return nil, err
			}
			if a.state != nil {
				recent, err := a.state.RecentlyViewedKeys(ctx, time.Now().Add(-30*24*time.Hour), 200)
				if err != nil {
					return nil, err
				}
				keys = append(keys, recent...)
			}
			parents, err := a.catalog.ChildAncestry(ctx, keys)
			if err != nil {
				return nil, err
			}
			for _, key := range keys {
				out[key] = true
				if show := parents[key].ShowKey; show != 0 {
					out[show] = true
				}
			}
			return out, nil
		}, Active: func() bool { return len(a.playback.Sessions()) > 0 },
		Seal: func(raw string) (string, error) { return a.cfg.Seal("availability-release", []byte(raw)) },
		Open: func(raw string) (string, error) {
			b, e := a.cfg.Unseal("availability-release", raw)
			return string(b), e
		},
		Logger: a.log.With("component", "availability"),
		// Section and home loads can start policy seeding, then read the
		// published and provisional evidence while the worker runs.
		Async: true,
		// Background requests to one indexer stay at least this far apart.
		Pace: 3 * time.Second,
	})
	a.availability = s
	a.availabilityLive.Store(s)
	if err == nil {
		a.closers = append(a.closers, s.Close)
	}
	return err
}
func (a *App) discoveredReleases(ctx context.Context, t *core.Title) []core.Release {
	if a.availability == nil {
		return nil
	}
	return a.availability.Candidates(ctx, t)
}
func (a *App) observeRelease(ctx context.Context, t *core.Title, r core.Release) {
	if a.availability != nil {
		a.availability.Observe(ctx, t, r)
	}
}

func (a *App) invalidateRelease(ctx context.Context, id string) {
	if a.availability != nil {
		a.availability.Invalidate(ctx, id)
	}
}

func (a *App) observeSearch(ctx context.Context, t *core.Title, rs []core.Release) {
	if a.availability != nil {
		a.availability.RecordSearch(ctx, t, rs)
	}
}

func (a *App) revealTitleSeason(key core.RatingKey) {
	if a.availability != nil {
		a.availability.RevealTitleSeason(key)
	}
}
