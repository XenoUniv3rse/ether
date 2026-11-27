package catalog

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
)

// ScheduledEpisodes is deliberately separate from Children/Get: discovery,
// autoplay, downloads and playback must never acquire future episodes through it.
func (c *Catalog) ScheduledEpisodes(ctx context.Context, key core.RatingKey) ([]core.Title, error) {
	if !c.config().ShowUpcomingEpisodes || key.Kind() != core.KindSeason && key.Kind() != core.KindEpisode {
		return nil, core.ErrNotFound
	}
	if items, ok := c.exportSchedule(ctx, key); ok {
		return items, ctx.Err()
	}
	parent, show, order, err := c.orderForParent(ctx, key)
	if err != nil {
		return nil, err
	}
	if parent == nil || show == nil || show.Adult || parent.Adult {
		return nil, core.ErrNotFound
	}
	if key.Kind() == core.KindEpisode {
		if err := c.projectSingle(ctx, parent); err != nil {
			return nil, err
		}
		date := parent.EpisodeAirDate
		if !date.IsZero() && date.After(c.now()) {
			return []core.Title{*parent}, nil
		}
		return []core.Title{}, nil
	}
	if err := c.projectSingle(ctx, parent); err != nil {
		return nil, err
	}
	if !c.visibleTitle(parent) {
		return nil, core.ErrNotFound
	}

	if providerLayout(show, order) {
		season, err := c.orderSeasonByKey(ctx, show.Key, order, key)
		if errors.Is(err, core.ErrNotFound) {
			return []core.Title{}, nil
		}
		if err != nil {
			return nil, err
		}
		if season.Season <= 0 {
			return nil, core.ErrNotFound
		}
		page, err := c.orderedEpisodePage(ctx, show, order, season.Season, 0, 1000, true)
		return page.Items, err
	}
	if parent.Season <= 0 {
		return nil, core.ErrNotFound
	}
	out, err := c.scanTitleKeys(ctx, `SELECT `+titleCols+` FROM catalog_titles
 WHERE type='episode' AND show_key=? AND season=? AND key<5000000000000000 AND adult=0 AND released_ts>?
 ORDER BY episode LIMIT 1000`, show.Key, parent.Season, c.now().Unix())
	if err != nil {
		return nil, err
	}
	if err := c.hydrateSlice(ctx, out); err != nil {
		return nil, err
	}
	if err := c.ProjectEpisodeTitles(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Catalog) refreshProviderSchedule(ctx context.Context, show *core.Title, order core.EpisodeOrder) error {
	seasons, err := c.ensureOrderManifest(ctx, show, order)
	if err != nil {
		return err
	}
	var newest []orderSeason
	for i := len(seasons) - 1; i >= 0 && len(newest) < 2; i-- {
		if seasons[i].Season > 0 {
			newest = append(newest, seasons[i])
		}
	}
	for i, season := range newest {
		if i > 0 {
			d := metadataDate(newest[0].AirDate)
			if !d.IsZero() && !d.After(c.now()) {
				break
			}
		}
		if err := c.ensureOrderSeason(ctx, show, order, season); err != nil {
			return err
		}
	}
	return nil
}
