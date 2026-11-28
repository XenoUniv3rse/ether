package vela

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/extras"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/reqmemo"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

type detailColdState struct{ core.State }

func (detailColdState) Get(context.Context, int64, core.RatingKey) (core.WatchStatus, error) {
	return core.WatchStatus{}, nil
}
func (detailColdState) GetMany(context.Context, int64, []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
	return nil, nil
}

func detailColdService(t *testing.T, d experience.ServiceDeps) *experience.Service {
	t.Helper()
	d.Active = func(context.Context, int64) (*core.User, error) { return &core.User{ID: 1, IsOwner: true}, nil }
	d.State = detailColdState{}
	d.Rows = &experience.RowsService{}
	d.Protocol = "vela"
	d.Search.Catalog = func(context.Context, string, []core.MediaType, int) ([]core.Title, error) { return nil, nil }
	d.Visibility = experience.Visibility{GetTitle: func(ctx context.Context, key core.RatingKey) (*core.Title, error) { return d.Get(ctx, nil, key) }, SectionFor: func(core.MediaType) (int, bool) { return 1, true }}
	if d.CachedGet != nil {
		d.Visibility.GetTitle = d.CachedGet
	}
	d.NoProviderWait = func(ctx context.Context) context.Context {
		return core.WithLocalResponse(catalog.WithReadWaitBudget(ctx, 0))
	}
	s, err := experience.NewService(d)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func detailColdRead(t *testing.T, i *instance, key core.RatingKey, suffix, etag string) (*httptest.ResponseRecorder, map[string]json.RawMessage) {
	t.Helper()
	r := httptest.NewRequest("GET", "/vela/v0/titles/"+ids.RatingKeyGUID(key)+suffix, nil)
	r.SetPathValue("id", ids.RatingKeyGUID(key))
	r.Header.Set("Authorization", "Bearer token")
	r.Header.Set("X-Vela-Device-Id", "device")
	r.Header.Set("If-None-Match", etag)
	// Attach QueryStats so detailColdProvider can detect foreground reads.
	ctx, _ := store.WithQueryStats(frontend.WithRequestMemo(reqmemo.With(r.Context())))
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	if suffix == "/children" {
		i.children(w, r)
	} else {
		i.detail(w, r)
	}
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return w, out
}

type detailColdProvider struct {
	catalog.TMDB
	release    chan struct{}
	foreground atomic.Int64
}

func (p *detailColdProvider) TVDetails(ctx context.Context, id int64) (*tmdb.TVDetails, error) {
	if store.QueryStatsFrom(ctx) != nil {
		p.foreground.Add(1)
	}
	d := &tmdb.TVDetails{TVShow: tmdb.TVShow{ID: id, Name: "Cold show", FirstAirDate: "2000-01-01"}, NumberOfSeasons: 30, NumberOfEpisodes: 30, Status: "Ended"}
	for n := 1; n <= 30; n++ {
		d.Seasons = append(d.Seasons, tmdb.SeasonSummary{SeasonNumber: n, Name: fmt.Sprint("Season ", n), AirDate: "2000-01-01", EpisodeCount: 1})
	}
	return d, nil
}
func (p *detailColdProvider) SeasonDetails(ctx context.Context, id int64, season int) (*tmdb.SeasonDetails, error) {
	if store.QueryStatsFrom(ctx) != nil {
		p.foreground.Add(1)
	} else {
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &tmdb.SeasonDetails{SeasonNumber: season, Name: fmt.Sprint("Season ", season), AirDate: "2000-01-01", Episodes: []tmdb.Episode{{ShowID: id, SeasonNumber: season, EpisodeNumber: 1, Name: "Episode", AirDate: "2000-01-01"}}}, nil
}

func (p *detailColdProvider) EpisodeExternalIDs(context.Context, int64, int, int) (*tmdb.ExternalIDs, error) {
	return &tmdb.ExternalIDs{}, nil
}

func TestVelaDetailFillContract(t *testing.T) {
	for _, suffix := range []string{"", "/children"} {
		t.Run("cold show"+suffix, func(t *testing.T) {
			db := store.OpenTest(t)
			p := &detailColdProvider{release: make(chan struct{})}
			c, err := catalog.New(db, p, config.Catalog{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { close(p.release); c.WaitRefreshes() }()
			s := detailColdService(t, experience.ServiceDeps{
				CachedGet: c.CachedGet,
				Get:       func(ctx context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) { return c.Get(ctx, k) },
				Children: func(ctx context.Context, _ *core.User, k core.RatingKey) ([]core.Title, error) {
					return c.Children(ctx, k)
				},
			})
			i := &instance{host: frontend.Host{Identity: identityFake{}, Experience: s}}
			w, out := detailColdRead(t, i, core.ShowKey(7331), suffix, "")
			field := "seasons"
			if suffix != "" {
				field = "items"
			}
			var seasons []json.RawMessage
			if err := json.Unmarshal(out[field], &seasons); err != nil {
				t.Fatal(err)
			}
			if len(seasons) != 30 || p.foreground.Load() != 1 {
				t.Fatalf("seasons=%d foreground=%d", len(seasons), p.foreground.Load())
			}
			if w.Header().Get("Cache-Control") != "private, no-store" || len(out["pending"]) == 0 {
				t.Fatalf("missing partial signal: %s %s", w.Header(), w.Body)
			}
			// Selecting a season permits its own cold call, while repeat navigation
			// uses the committed page even while the show-wide worker is blocked.
			_, out = detailColdRead(t, i, core.SeasonKey(7331, 30), "/children", "")
			var episodes []json.RawMessage
			if err := json.Unmarshal(out["items"], &episodes); err != nil {
				t.Fatal(err)
			}
			if len(out["pending"]) != 0 {
				t.Fatalf("completed season inherited another season fill: %s", out["pending"])
			}
			if len(episodes) != 1 || p.foreground.Load() != 2 {
				t.Fatalf("episodes=%d foreground=%d", len(episodes), p.foreground.Load())
			}
			detailColdRead(t, i, core.SeasonKey(7331, 30), "/children", "")
			if p.foreground.Load() != 2 {
				t.Fatal("stored season fetched again")
			}
		})
	}
	t.Run("extras snapshots", func(t *testing.T) {
		p := &detailColdVideos{release: make(chan struct{}), finished: make(chan struct{})}
		x := extras.New(extras.Options{Videos: p, Config: func() config.Extras { return config.Extras{Enabled: true, ResolverCommand: "fake"} }, LookPath: func(string) (string, error) { return "/fake", nil }})
		defer x.Close()
		title := core.Title{Key: core.MovieKey(42), Type: core.TypeMovie, Title: "Movie", IDs: core.IDs{TMDB: 42}}
		s := detailColdService(t, experience.ServiceDeps{
			Get:      func(context.Context, *core.User, core.RatingKey) (*core.Title, error) { return &title, nil },
			Children: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) { return nil, nil },
			Enrich: func(ctx context.Context, _ *core.User, _ frontend.ItemRequest, title *core.Title) (frontend.ItemDetail, error) {
				clips, err := x.List(ctx, title, "en")
				d := frontend.ItemDetail{Title: *title}
				for _, clip := range clips {
					d.Extras = append(d.Extras, frontend.Extra{Key: clip.Key, ParentKey: clip.ParentKey, Kind: clip.Subtype, Title: clip.Title})
				}
				return d, err
			},
		})
		i := &instance{host: frontend.Host{Identity: identityFake{}, Experience: s}}
		w, out := detailColdRead(t, i, title.Key, "", "")
		if string(out["pending"]) != `["extras"]` || string(out["refreshAfterMs"]) != "2000" || w.Header().Get("Cache-Control") != "private, no-store" || !strings.HasPrefix(w.Header().Get("ETag"), `W/"`) {
			t.Fatalf("partial %v %s", w.Header(), w.Body)
		}
		detailColdRead(t, i, title.Key, "/extras", w.Header().Get("ETag"))
		close(p.release)
		select {
		case <-p.finished:
		case <-time.After(5 * time.Second):
			t.Fatal("extras fill did not finish")
		}
		// Completion of the provider happens before the cache commit. Wait only
		// for the public cache to become available, without scheduling more work.
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, out = detailColdRead(t, i, title.Key, "", w.Header().Get("ETag"))
			if len(out["pending"]) == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("extras cache did not settle")
			}
			time.Sleep(time.Millisecond)
		}
		complete, out := detailColdRead(t, i, title.Key, "", w.Header().Get("ETag"))
		var clips []json.RawMessage
		if err := json.Unmarshal(out["extras"], &clips); err != nil {
			t.Fatal(err)
		}
		if len(clips) != 1 || complete.Header().Get("Cache-Control") != "private, max-age=0, must-revalidate" || strings.HasPrefix(complete.Header().Get("ETag"), "W/") {
			t.Fatalf("complete %v %s", complete.Header(), complete.Body)
		}
	})
}

type detailColdVideos struct {
	extras.VideoProvider
	release, finished chan struct{}
}

func (p *detailColdVideos) MovieVideos(ctx context.Context, _ int64, _ string) ([]tmdb.Video, error) {
	defer close(p.finished)
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []tmdb.Video{{Site: "YouTube", Key: "trlOffEN001", Name: "Trailer", Type: "Trailer", Official: true, ISO639: "en"}}, nil
}
