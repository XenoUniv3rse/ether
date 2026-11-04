package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/availability"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
)

// Keep optional catalogue contracts by embedding the concrete catalogue. The
// admin mirror and feed matcher still use the unfiltered catalogue.
type availabilityCatalog struct {
	*catalog.Catalog
	availability *availability.Service
}

func (c *availabilityCatalog) excludeHidden(ctx context.Context, f core.Filter) core.Filter {
	if f.IncludeKeys != nil {
		return f
	}
	f.DigitalDiscovery, f.DigitalDateEvidence = c.availability.DigitalDiscovery(ctx)
	if !f.DigitalDiscovery {
		f.ExcludeKeys = append(append([]core.RatingKey{}, f.ExcludeKeys...), c.availability.HiddenMovies(ctx)...)
	}
	f.ExcludeKeys = append(f.ExcludeKeys, c.availability.HiddenShows(ctx)...)
	if f.Type == core.TypeEpisode {
		for _, key := range c.availability.HiddenEpisodes(ctx) {
			keep := false
			for _, active := range f.AvailabilityKeepKeys {
				if active == key {
					keep = true
					break
				}
			}
			if !keep {
				f.ExcludeKeys = append(f.ExcludeKeys, key)
			}
		}
	}
	return f
}

func (c *availabilityCatalog) Browse(ctx context.Context, q core.BrowseQuery) (core.Page, error) {
	if q.Filter.IncludeKeys == nil {
		decision, err := c.availability.PagedExclusions(ctx, q.Filter.Type == core.TypeEpisode)
		if err != nil {
			return core.Page{}, err
		}
		q.Filter.DigitalDiscovery, q.Filter.DigitalDateEvidence = decision.Digital, decision.DigitalEvidence
		q.Filter.HiddenRecentMoviesAfter, q.Filter.HiddenRecentShowsAfter, q.Filter.HiddenRecentAsOf = decision.MoviesAfter, decision.ShowsAfter, decision.AsOf
		for _, key := range decision.Keys {
			keep := false
			if key.Kind() == core.KindEpisode {
				for _, active := range q.Filter.AvailabilityKeepKeys {
					if active == key {
						keep = true
						break
					}
				}
			}
			if !keep {
				q.Filter.ExcludeKeys = append(q.Filter.ExcludeKeys, key)
			}
		}
	}
	return c.Catalog.Browse(ctx, q)
}

// BrowseAvailabilityInSQL marks pages whose availability decision was applied
// before counting and pagination.
func (c *availabilityCatalog) BrowseAvailabilityInSQL() bool { return true }
func (c *availabilityCatalog) Facet(ctx context.Context, t core.MediaType, facet string) ([]core.FacetValue, error) {
	return c.Catalog.FacetFilter(ctx, c.excludeHidden(ctx, core.Filter{Type: t}), facet)
}
func (c *availabilityCatalog) FacetFilter(ctx context.Context, f core.Filter, facet string) ([]core.FacetValue, error) {
	return c.Catalog.FacetFilter(ctx, c.excludeHidden(ctx, f), facet)
}
func (c *availabilityCatalog) Children(ctx context.Context, k core.RatingKey) ([]core.Title, error) {
	ts, e := c.Catalog.Children(ctx, k)
	if e != nil {
		return nil, e
	}
	return c.availability.FilterDiscovery(ctx, c.Catalog.FilterVisibility(ctx, ts)), nil
}
func (c *availabilityCatalog) Grandchildren(ctx context.Context, k core.RatingKey) ([]core.Title, error) {
	ts, e := c.Catalog.Grandchildren(ctx, k)
	if e != nil {
		return nil, e
	}
	return c.availability.FilterDiscovery(ctx, c.Catalog.FilterVisibility(ctx, ts)), nil
}
func (c *availabilityCatalog) Search(ctx context.Context, q string, types []core.MediaType, n int) ([]core.Title, error) {
	ts, e := c.Catalog.Search(ctx, q, types, n)
	if e != nil {
		return nil, e
	}
	return c.availability.FilterDiscovery(ctx, c.Catalog.FilterVisibility(ctx, ts)), nil
}
