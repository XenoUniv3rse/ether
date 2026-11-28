package catalog

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPopularGenreCandidatesMatchIndependentBrowse(t *testing.T) {
	c := newChunkTestCatalog(t)
	c.FreezeCacheMinute()
	ctx := context.Background()
	var titles []core.Title
	for i := int64(1); i <= 15; i++ {
		kind, key := core.TypeMovie, core.MovieKey(i)
		if i%2 == 0 {
			kind, key = core.TypeShow, core.ShowKey(i)
		}
		genres := []string{"Drama"}
		if i%3 != 0 {
			genres = append(genres, "Mystery")
		}
		titles = append(titles, core.Title{
			Key: key, Type: kind, IDs: core.IDs{TMDB: i}, Title: "Title " + key.String(),
			Genres: genres, Rating: float64(9 - i%4), VoteCount: 200,
			Released: time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), Year: 2001,
			Popularity: float64(100 - i/3), ContentRating: "PG",
			Directors: []core.Person{{Name: "Fixture Director", TMDB: 7}},
		})
	}
	if err := c.upsertMany(ctx, titles, upsertOpts{browsable: true, detail: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RebuildRank(ctx); err != nil {
		t.Fatal(err)
	}
	genres := []string{"Mystery", "Drama"}
	const limit = 5
	qualifies := func(title core.Title) bool { return title.Rating >= 8 }
	got, supported, err := c.PopularGenreCandidates(ctx, genres, "PG", limit, qualifies)
	if err != nil || !supported {
		t.Fatalf("batch supported=%t: %v", supported, err)
	}
	for _, genre := range genres {
		page, err := c.Browse(ctx, core.BrowseQuery{
			Filter: core.Filter{Genres: []string{genre}, MaxContentRating: "PG"},
			Sort:   core.SortPopularity, Desc: true, Limit: limit, NoTotal: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		var want []core.Title
		for _, title := range page.Items {
			if qualifies(title) {
				want = append(want, title)
			}
		}
		if len(want) == 0 || len(want) >= limit || !reflect.DeepEqual(got[genre], want) {
			t.Fatalf("%s window: got %+v, want %+v", genre, got[genre], want)
		}
	}
}
