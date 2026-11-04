package catalog

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// A series and its episodes can carry different policy facts. The page and
// its total must apply the child's facts after applying the series predicate.
func TestEpisodePolicyCountAndPages(t *testing.T) {
	c := newChunkTestCatalog(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })
	show := core.Title{Key: core.ShowKey(101), Type: core.TypeShow, Title: "Show", Released: now.AddDate(-2, 0, 0), ContentRating: "PG"}
	if err := c.UpsertTitle(ctx, show, true); err != nil {
		t.Fatal(err)
	}
	for i, rating := range []string{"", "R", "PG", "PG"} {
		ep := core.Title{Key: core.EpisodeKey(101, 1, i+1), Type: core.TypeEpisode, ShowKey: show.Key, Season: 1, Episode: i + 1, Title: "Episode", Released: now.AddDate(-1, 0, 0), ContentRating: rating}
		if i == 3 {
			ep.Keywords = []string{"blocked"}
		}
		if err := c.UpsertTitle(ctx, ep, false); err != nil {
			t.Fatal(err)
		}
	}
	policy := &core.Filter{MaxContentRating: "PG", ExcludeKeywords: []string{"blocked"}}
	q := core.BrowseQuery{Filter: core.Filter{Type: core.TypeEpisode, ChildVisibility: policy}, Limit: 1}
	check := func() {
		t.Helper()
		for i, key := range []core.RatingKey{core.EpisodeKey(101, 1, 1), core.EpisodeKey(101, 1, 3)} {
			q.Offset = i
			p, err := c.Browse(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			if p.Total != 2 || len(p.Items) != 1 || p.Items[0].Key != key {
				t.Fatalf("offset %d: total %d items %v", i, p.Total, p.Items)
			}
		}
	}
	check()
	// The same episode can also be hidden by a mixed catalogue rule.
	policy.ExcludeKeywords = nil
	c.UpdateConfig(config.Catalog{Exclusions: []core.LibraryExclusion{{MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{{Field: "keyword", Op: "in", Value: json.RawMessage(`["blocked"]`)}}}}})
	check()
}
