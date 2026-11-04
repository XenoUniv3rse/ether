package plexguid

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestDirectChildMatchCannotProveParentIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, kind, ids, indices string
		key                      core.RatingKey
		want                     bool
	}{
		{"season lacks index", "season", "tmdb://1396", "", core.SeasonKey(1396, 1), false},
		{"episode lacks indices", "episode", "tmdb://1396", "", core.EpisodeKey(1396, 1, 2), false},
		{"episode lacks parent", "episode", "tmdb://1396", `,"index":2`, core.EpisodeKey(1396, 1, 2), false},
		{"season exact external ID", "season", "tmdb://1396/1", "", core.SeasonKey(1396, 1), false},
		{"episode exact external ID", "episode", "tmdb://1396/1/2", "", core.EpisodeKey(1396, 1, 2), false},
		{"episode exact indices", "episode", "tmdb://1396", `,"index":2,"parentIndex":1`, core.EpisodeKey(1396, 1, 2), false},
		{"episode contradicts external ID", "episode", "tmdb://1396/1/2", `,"index":3,"parentIndex":1`, core.EpisodeKey(1396, 1, 2), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.OpenMemory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			guid := "plex://" + tc.kind + "/602e5e2294f08e002d003344"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				typ := r.URL.Query().Get("type")
				if (tc.kind == "season" && typ == "3") || (tc.kind == "episode" && typ == "4") {
					fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"guid":%q,"type":%q,"Guid":[{"id":%q}]%s}]}}`, guid, tc.kind, tc.ids, tc.indices)
					return
				}
				fmt.Fprint(w, `{"MediaContainer":{"Metadata":[]}}`)
			}))
			defer srv.Close()
			s := New(db, Options{BaseURL: srv.URL, Token: func() string { return "fixture" }})
			defer s.Close()
			got, err := s.Resolve(ctx, tc.key)
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if tc.want {
				want = guid
			}
			if got != want {
				t.Fatalf("Resolve=%q, want %q", got, want)
			}
		})
	}
}

// A child proof belongs to one specific parent mapping, including during a
// concurrent rematch. Old unverified rows must never escape through reads.
func TestChildProofFollowsParentMapping(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, Options{})
	defer s.Close()
	show, season, episode := core.ShowKey(1396), core.SeasonKey(1396, 1), core.EpisodeKey(1396, 1, 2)
	now := time.Now().Unix()
	const oldShow = "plex://show/aaaaaaaaaaaaaaaaaaaaaaaa"
	const newShow = "plex://show/bbbbbbbbbbbbbbbbbbbbbbbb"
	const seasonGUID = "plex://season/cccccccccccccccccccccccc"
	const episodeGUID = "plex://episode/dddddddddddddddddddddddd"
	if err = s.storeMapping(ctx, show, oldShow, now); err != nil {
		t.Fatal(err)
	}
	if err = s.storeMappingUnder(ctx, season, seasonGUID, now, show, oldShow); err != nil {
		t.Fatal(err)
	}
	if err = s.storeMappingUnder(ctx, episode, episodeGUID, now, season, seasonGUID); err != nil {
		t.Fatal(err)
	}
	if err = s.storeMapping(ctx, show, newShow, now); err != nil {
		t.Fatal(err)
	}
	if s.Cached(ctx, season) != "" || len(s.CachedMany(ctx, []core.RatingKey{season, episode})) != 0 {
		t.Fatal("stale children exposed")
	}
	if _, err = s.ExactKey(ctx, episodeGUID); err == nil {
		t.Fatal("stale reverse mapping exposed")
	}
	if err = s.storeMappingUnder(ctx, season, seasonGUID, now, show, oldShow); err == nil {
		t.Fatal("stale parent accepted")
	}
}
