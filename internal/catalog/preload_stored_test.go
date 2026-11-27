package catalog

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// PreloadStored caches what loadByKey would read, detail time included, so a
// list's series checks are cache hits that Get still judges fresh or stale
// exactly as before.
func TestPreloadStoredMatchesLoadByKey(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	// This test deletes the backing rows; keep its cache minute stable.
	c.FreezeCacheMinute()
	now := c.now()
	var keys []core.RatingKey
	for id := int64(1); id <= 3; id++ {
		show := core.Title{Key: core.ShowKey(id), Type: core.TypeShow, Title: "Show", Genres: []string{"Drama"}, Released: now.AddDate(-1, 0, 0)}
		if err := c.upsert(ctx, show, upsertOpts{browsable: true, detail: id != 2}); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, show.Key)
	}
	want := map[core.RatingKey]*storedTitle{}
	for _, k := range keys {
		st, err := c.loadByKey(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		want[k] = st
	}
	c.cacheMu.Lock()
	c.titleCache = nil
	c.cacheMu.Unlock()
	c.PreloadStored(ctx, append(keys, keys[0], core.ShowKey(99)))
	// Served from the cache: the rows are gone, the generation is not.
	if _, err := c.db.ExecContext(ctx, `DELETE FROM catalog_titles WHERE type='show'`); err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		got, err := c.loadByKey(ctx, k)
		if err != nil {
			t.Fatalf("%v not preloaded: %v", k, err)
		}
		if !reflect.DeepEqual(got.t, want[k].t) || !got.detailAt.Equal(want[k].detailAt) || got.detail != want[k].detail {
			t.Fatalf("%v preloaded %+v, loaded %+v", k, got, want[k])
		}
	}
}

// Playlist membership is decided before page hydration. Partial rows must
// keep the same inherited rating and duration as the full title.
func TestPlaylistTitleRowsInheritShowPolicy(t *testing.T) {
	c := newChunkTestCatalog(t)
	ctx := core.WithCatalogViewer(context.Background(), &core.User{ID: 10, MaxContentRating: "TV-14"})
	premiere := c.now().AddDate(-1, 0, 0)
	show := core.Title{Key: core.ShowKey(7), Type: core.TypeShow, Title: "Series",
		Released: premiere, ContentRating: "TV-MA", Keywords: []string{"drama"}}
	episode := core.Title{Key: core.EpisodeKey(7, 1, 1), Type: core.TypeEpisode, Title: "Pilot",
		ShowKey: show.Key, ShowIDs: core.IDs{TMDB: 7}, Season: 1, Episode: 1, Released: premiere, Runtime: 43 * time.Minute}
	for _, item := range []core.Title{show, episode} {
		if err := c.upsert(ctx, item, upsertOpts{browsable: true, detail: true}); err != nil {
			t.Fatal(err)
		}
	}
	keys := []core.RatingKey{episode.Key, episode.Key}
	rows, err := c.PlaylistTitleRows(ctx, keys, core.FactKeywords)
	if err != nil {
		t.Fatal(err)
	}
	full, err := c.CachedGetMany(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	got, want := rows[episode.Key], full[episode.Key]
	if got.Key != episode.Key || got.ContentRating != "TV-MA" || got.ContentRating != want.ContentRating ||
		got.EnforcementRating != want.EnforcementRating || got.Runtime != want.Runtime ||
		!reflect.DeepEqual(got.Keywords, want.Keywords) {
		t.Fatalf("partial policy and runtime differ from display metadata: %+v versus %+v", got, want)
	}
}
