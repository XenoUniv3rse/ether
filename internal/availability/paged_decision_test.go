package availability

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type pagedEpisodeCatalog struct{ *fakeCatalog }

func (c *pagedEpisodeCatalog) RecentEvidenceEpisodes(_ context.Context, since time.Time) ([]core.Title, error) {
	var out []core.Title
	for _, t := range c.titles {
		if t.Type == core.TypeEpisode && t.Released.After(since) {
			out = append(out, t)
		}
	}
	return out, nil
}

// A page must not use an episode decision invalidated by a release change.
func TestPagedEpisodeDecisionWaitsForInvalidation(t *testing.T) {
	x := setup(t)
	ep := core.Title{Key: core.EpisodeKey(99, 1, 1), Type: core.TypeEpisode, Title: "Episode", Released: x.now.Add(-time.Hour)}
	x.cat.titles[ep.Key] = ep
	x.s.d.Catalog = &pagedEpisodeCatalog{x.cat}
	x.s.d.Async = true
	defer x.s.Close()
	input := x.s.discoverySnapshot()
	x.s.visibilityMu.Lock()
	for _, kind := range []string{hiddenMovies, hiddenShows, hiddenEpisodes} {
		x.s.hiddenSets[kind] = &hiddenSet{key: hiddenKey(kind, input), at: x.now, covered: map[core.RatingKey]bool{ep.Key: true}, hidden: map[core.RatingKey]bool{}}
	}
	x.s.visibilityMu.Unlock()
	x.s.clearVisibility()
	d, err := x.s.PagedExclusions(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range d.Keys {
		if k == ep.Key {
			return
		}
	}
	t.Fatal("invalidated unavailable episode was not excluded")
}
