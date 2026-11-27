package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/conform"
	mbconform "github.com/braedonsaunders/ether/internal/conform/mediabrowser"
	"github.com/braedonsaunders/ether/internal/conform/openapi/spectest"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func TestMediaBrowserRealExperience(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	if err = local.Create(ctx, owner.ID, "viewer", "password-1234"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		surface, profile string
		movieID, liveID  int64
	}{
		{"jellyfin", "findroid", 23, 30},
		{"emby", "emby-sdk", 24, 31},
	} {
		surface := tc.surface
		t.Run(surface, func(t *testing.T) {
			key := core.MovieKey(tc.movieID)
			title := core.Title{Key: key, Type: core.TypeMovie, IDs: core.IDs{TMDB: tc.movieID}, Title: "Fixture Film", Runtime: time.Hour, Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
			if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
				t.Fatal(err)
			}
			inst, err := a.frontends.registry.Mount(ctx, core.Protocol(surface))
			if err != nil {
				t.Fatal(err)
			}
			defer a.frontends.registry.Unmount(ctx, core.Protocol(surface))
			call := func(method, path, body, token string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.RemoteAddr = "192.0.2.20:1234"
				r.Header.Set("Authorization", `MediaBrowser Client="TV", DeviceId="tv", Token="`+token+`"`)
				inst.Handler().ServeHTTP(w, r)
				return w
			}
			login := call("POST", "/Users/AuthenticateByName", `{"Username":"viewer","Pw":"password-1234"}`, "")
			if login.Code != 200 {
				t.Fatalf("login %d %s", login.Code, login.Body)
			}
			var signed struct{ AccessToken string }
			if err = json.Unmarshal(login.Body.Bytes(), &signed); err != nil {
				t.Fatal(err)
			}
			// Findroid startup/home replay through the mounted, authenticated
			// front end (including the shared listener prefix). Keep suggestions
			// here: an isolated handler test missed the original startup failure.
			var loginBody struct {
				User struct {
					ID string `json:"Id"`
				}
			}
			if err := json.Unmarshal(login.Body.Bytes(), &loginBody); err != nil {
				t.Fatal(err)
			}
			uid := loginBody.User.ID
			for _, path := range []string{
				"/System/Info/Public", "/Branding/Configuration", "/Users/Public",
				"/Users/Me", "/UserViews?userId=" + uid,
				"/Items/Suggestions?userId=" + uid + "&type=Movie,Series&limit=6",
				"/UserItems/Resume?userId=" + uid + "&limit=12&mediaTypes=Video",
				"/Shows/NextUp?userId=" + uid + "&limit=24",
				"/Users/" + uid + "/Items/Latest?limit=16",
			} {
				response := call("GET", "/"+surface+path, "", signed.AccessToken)
				if response.Code != 200 || !json.Valid(response.Body.Bytes()) {
					t.Fatalf("Findroid replay %s: %d %s", path, response.Code, response.Body)
				}
			}
			views := call("GET", "/UserViews", "", signed.AccessToken)
			if views.Code != 200 || !strings.Contains(views.Body.String(), "Movies") {
				t.Fatalf("views %d %s", views.Code, views.Body)
			}
			view, _ := ids.GUID(ids.View, 1)
			list := call("GET", "/Items?ParentId="+view, "", signed.AccessToken)
			if list.Code != 200 || !strings.Contains(list.Body.String(), "Fixture Film") {
				t.Fatalf("list %d %s", list.Code, list.Body)
			}
			// Android TV and Jellyfin Web optional browse/detail requests.
			for _, path := range []string{
				"/Items/Filters?parentId=" + view, "/Items/Filters2?parentId=" + view,
				"/Shows/Upcoming?userId=" + uid,
				"/Videos/" + ids.RatingKeyGUID(key) + "/AdditionalParts",
				"/Items/" + ids.RatingKeyGUID(key) + "/ThemeMedia",
				"/Items/" + ids.RatingKeyGUID(key) + "/ThemeSongs",
				"/Items/" + ids.RatingKeyGUID(key) + "/ThemeVideos",
			} {
				response := call("GET", "/"+surface+path, "", signed.AccessToken)
				if response.Code != 200 || !json.Valid(response.Body.Bytes()) {
					t.Fatalf("client replay %s: %d %s", path, response.Code, response.Body)
				}
			}
			id := ids.RatingKeyGUID(key)
			paths := []string{"/UserItems/" + id + "/UserData?userId=" + uid, "/Users/" + uid + "/Items/" + id + "/UserData"}
			for _, path := range paths {
				other, _ := ids.GUID(ids.User, owner.ID+100)
				if res := call("POST", strings.ReplaceAll(path, uid, other), `{}`, signed.AccessToken); res.Code != 403 {
					t.Fatalf("cross-viewer update returned %d", res.Code)
				}
			}
			checkOfflineUserDataGolden(t, surface, tc.profile, uid, id, func(path string, body []byte) *httptest.ResponseRecorder {
				return call("POST", path, string(body), signed.AccessToken)
			})
			doc := spectest.Document(t, surface)
			check := func(path, body string, ticks int64, count int, favorite bool, rating float64) {
				t.Helper()
				res := call("POST", path, body, signed.AccessToken)
				legacy := strings.HasPrefix(path, "/Users/")
				if doc != nil && (surface == "emby") == legacy {
					if err := doc.Validate("POST", strings.SplitN(path, "?", 2)[0], res.Code, res.Header().Get("Content-Type"), res.Body.Bytes()); err != nil {
						t.Fatal(err)
					}
				}
				if surface == "emby" && legacy {
					if res.Code != 200 || res.Body.Len() != 0 {
						t.Fatalf("Emby legacy update %d %s", res.Code, res.Body)
					}
					res = call("GET", "/UserItems/"+id+"/UserData", "", signed.AccessToken)
				}

				var got struct {
					PlaybackPositionTicks int64
					PlayCount             int
					IsFavorite            bool
					Rating                float64
					Likes                 bool
				}
				if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &got) != nil ||
					got.PlaybackPositionTicks != ticks || got.PlayCount != count || got.IsFavorite != favorite ||
					got.Rating != rating || got.Likes != (rating >= 7) {
					t.Fatalf("user data %d %s", res.Code, res.Body)
				}
			}
			check(paths[0], `{"IsFavorite":true,"Likes":true}`, 6000000000, 0, true, 10)
			check(paths[1], `{"PlaybackPositionTicks":0,"Played":true,"PlayCount":3,"LastPlayedDate":"2022-01-01T03:04:05Z","Likes":false}`, 6000000000, 0, true, 2)
			check(paths[1], `{"PlaybackPositionTicks":10000000,"LastPlayedDate":"2022-01-03T03:04:05Z"}`, 0, 0, true, 2)
			check(paths[0], `{"PlaybackPositionTicks":33000000000,"PlayCount":2,"LastPlayedDate":"2022-01-04T03:04:05Z"}`, 0, 2, true, 2)

			liveTitle := title
			liveTitle.Key, liveTitle.IDs.TMDB = core.MovieKey(tc.liveID), tc.liveID
			if err := a.catalog.UpsertTitle(ctx, liveTitle, true); err != nil {
				t.Fatal(err)
			}
			liveID := ids.RatingKeyGUID(liveTitle.Key)
			for _, step := range []struct {
				suffix        string
				ticks, resume int64
			}{{"Progress", 10000000, 0}, {"Progress", 6000000000, 6000000000}, {"Stopped", 9000000000, 9000000000}} {
				suffix, ticks := step.suffix, step.ticks
				res := call("POST", "/"+surface+"/Sessions/Playing/"+suffix, fmt.Sprintf(`{"ItemId":%q,"PositionTicks":%d}`, liveID, ticks), signed.AccessToken)
				if res.Code != 204 {
					t.Fatalf("%s report %d %s", suffix, res.Code, res.Body)
				}
				res = call("GET", "/"+surface+"/UserItems/"+liveID+"/UserData", "", signed.AccessToken)
				if res.Code != 200 || !strings.Contains(res.Body.String(), fmt.Sprintf(`"PlaybackPositionTicks":%d`, step.resume)) {
					t.Fatalf("%s read-back %d %s", suffix, res.Code, res.Body)
				}
			}
			check("/UserItems/"+liveID+"/UserData", `{"PlaybackPositionTicks":0,"Played":true,"LastPlayedDate":"2022-01-02T03:04:05Z"}`, 9000000000, 0, false, 0)

			watched := call("POST", "/UserPlayedItems/"+ids.RatingKeyGUID(key), "", signed.AccessToken)
			if watched.Code != 200 || !strings.Contains(watched.Body.String(), `"Played":true`) {
				t.Fatalf("watched %d %s", watched.Code, watched.Body)
			}
		})
	}
}

// The fixture-only probes are replayed against a disposable Ether viewer.
func checkOfflineUserDataGolden(t *testing.T, surface, profile, userID, itemID string, post func(string, []byte) *httptest.ResponseRecorder) {
	t.Helper()
	p, err := mbconform.Load(surface, filepath.Join("..", "conform", "profiles", surface))
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range p.Corpus(profile) {
		if req.Name != "offline_user_data_resume" {
			continue
		}
		name := p.FixtureName(profile, req, true)
		want, err := conform.LoadFixture(filepath.Join("..", "..", "testdata", "conform", surface, profile, name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		path := strings.NewReplacer("${user_id}", userID, "${item_id}", itemID).Replace(req.Path)
		got := post(path, req.Body)
		body := bytes.TrimSpace(got.Body.Bytes())
		if got.Code != want.Status || !bytes.Equal(body, want.Body) {
			t.Fatalf("offline user data golden %d %s", got.Code, body)
		}
		return
	}
	t.Fatal("missing offline user data probe")
}

var errMetadataDown = errors.New("metadata down")

// searchDown refuses every remote catalogue call. Keyword search must still answer.
type searchDown struct{}

func (searchDown) MovieDetails(context.Context, int64) (*tmdb.MovieDetails, error) {
	return nil, errMetadataDown
}
func (searchDown) TVDetails(context.Context, int64) (*tmdb.TVDetails, error) {
	return nil, errMetadataDown
}
func (searchDown) SeasonDetails(context.Context, int64, int) (*tmdb.SeasonDetails, error) {
	return nil, errMetadataDown
}
func (searchDown) EpisodeExternalIDs(context.Context, int64, int, int) (*tmdb.ExternalIDs, error) {
	return nil, errMetadataDown
}
func (searchDown) CollectionDetails(context.Context, int64) (*tmdb.CollectionDetails, error) {
	return nil, errMetadataDown
}
func (searchDown) DiscoverMovies(context.Context, tmdb.DiscoverParams) (*tmdb.Paged[tmdb.Movie], error) {
	return nil, errMetadataDown
}
func (searchDown) DiscoverTV(context.Context, tmdb.DiscoverParams) (*tmdb.Paged[tmdb.TVShow], error) {
	return nil, errMetadataDown
}
func (searchDown) SearchMovies(context.Context, string, int) (*tmdb.Paged[tmdb.Movie], error) {
	return nil, errMetadataDown
}
func (searchDown) SearchTV(context.Context, string, int) (*tmdb.Paged[tmdb.TVShow], error) {
	return nil, errMetadataDown
}
func (searchDown) SearchMulti(context.Context, string, int) (*tmdb.Paged[tmdb.MultiResult], error) {
	return nil, errMetadataDown
}
func (searchDown) TrendingMovies(context.Context, string, int) (*tmdb.Paged[tmdb.Movie], error) {
	return nil, errMetadataDown
}
func (searchDown) TrendingTV(context.Context, string, int) (*tmdb.Paged[tmdb.TVShow], error) {
	return nil, errMetadataDown
}
func (searchDown) TrendingAll(context.Context, string, int) (*tmdb.Paged[tmdb.MultiResult], error) {
	return nil, errMetadataDown
}
func (searchDown) MovieList(context.Context, string, int) (*tmdb.Paged[tmdb.Movie], error) {
	return nil, errMetadataDown
}
func (searchDown) TVList(context.Context, string, int) (*tmdb.Paged[tmdb.TVShow], error) {
	return nil, errMetadataDown
}
func (searchDown) MovieSimilar(context.Context, int64, int) (*tmdb.Paged[tmdb.Movie], error) {
	return nil, errMetadataDown
}
func (searchDown) MovieRecommendations(context.Context, int64, int) (*tmdb.Paged[tmdb.Movie], error) {
	return nil, errMetadataDown
}
func (searchDown) TVSimilar(context.Context, int64, int) (*tmdb.Paged[tmdb.TVShow], error) {
	return nil, errMetadataDown
}
func (searchDown) TVRecommendations(context.Context, int64, int) (*tmdb.Paged[tmdb.TVShow], error) {
	return nil, errMetadataDown
}
func (searchDown) FindByExternalID(context.Context, string, string) (*tmdb.FindResult, error) {
	return nil, errMetadataDown
}
func (searchDown) Genres(context.Context, string) ([]tmdb.Genre, error) {
	return nil, errMetadataDown
}

var _ catalog.TMDB = searchDown{}

func TestMediaBrowserSearchAnswersWhenMetadataIsDown(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	if err = local.Create(ctx, owner.ID, "viewer", "password-1234"); err != nil {
		t.Fatal(err)
	}
	released := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	show := core.Title{Key: core.ShowKey(8), Type: core.TypeShow, Title: "Fixture Show", Released: released}
	episode := core.Title{Key: core.EpisodeKey(8, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, Season: 1, Episode: 1, Title: "Pilot Unique", Released: released}
	movie := core.Title{Key: core.MovieKey(23), Type: core.TypeMovie, IDs: core.IDs{TMDB: 23}, Title: "Fixture Film", Released: released}
	for _, title := range []core.Title{show, episode, movie} {
		if err = a.catalog.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}
	a.catalog.SetTMDB(searchDown{})
	for _, surface := range []string{"jellyfin", "emby"} {
		t.Run(surface, func(t *testing.T) {
			inst, err := a.frontends.registry.Mount(ctx, core.Protocol(surface))
			if err != nil {
				t.Fatal(err)
			}
			defer a.frontends.registry.Unmount(ctx, core.Protocol(surface))
			call := func(method, path, body, token string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.RemoteAddr = "192.0.2.20:1234"
				r.Header.Set("Authorization", `MediaBrowser Client="TV", DeviceId="tv", Token="`+token+`"`)
				inst.Handler().ServeHTTP(w, r)
				return w
			}
			login := call("POST", "/Users/AuthenticateByName", `{"Username":"viewer","Pw":"password-1234"}`, "")
			if login.Code != 200 {
				t.Fatalf("login %d %s", login.Code, login.Body)
			}
			var signed struct{ AccessToken string }
			if err = json.Unmarshal(login.Body.Bytes(), &signed); err != nil {
				t.Fatal(err)
			}
			hints := call("GET", "/Search/Hints?searchTerm=Fixture&Limit=20", "", signed.AccessToken)
			if hints.Code != 200 || !strings.Contains(hints.Body.String(), "Fixture Film") {
				t.Fatalf("hints %d %s", hints.Code, hints.Body)
			}
			episodeHints := call("GET", "/Search/Hints?searchTerm=Pilot%20Unique&Limit=20", "", signed.AccessToken)
			if episodeHints.Code != 200 || !strings.Contains(episodeHints.Body.String(), "Pilot Unique") {
				t.Fatalf("episode hints %d %s", episodeHints.Code, episodeHints.Body)
			}
			miss := call("GET", "/Search/Hints?searchTerm=zzzz-not-a-title&Limit=20", "", signed.AccessToken)
			if miss.Code != 200 || !strings.Contains(miss.Body.String(), `"SearchHints":[]`) && !strings.Contains(miss.Body.String(), `"SearchHints":null`) {
				t.Fatalf("empty hints %d %s", miss.Code, miss.Body)
			}
			if surface == "jellyfin" {
				id := ids.RatingKeyGUID(movie.Key)
				before := call("GET", "/Items/"+id, "", signed.AccessToken)
				if before.Code != 200 || !strings.Contains(before.Body.String(), `"Played":false`) {
					t.Fatalf("detail %d %s", before.Code, before.Body)
				}
				played := call("POST", "/UserPlayedItems/"+id, "", signed.AccessToken)
				if played.Code != 200 {
					t.Fatalf("played %d %s", played.Code, played.Body)
				}
				after := call("GET", "/Items/"+id, "", signed.AccessToken)
				if after.Code != 200 || !strings.Contains(after.Body.String(), `"Played":true`) {
					t.Fatalf("stale detail %d %s", after.Code, after.Body)
				}
			}
		})
	}
}

func TestMediaBrowserProviderIDAndIsPlayed(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Create(ctx, owner.ID, "viewer", "password-1234"); err != nil {
		t.Fatal(err)
	}
	kidID, err := local.CreateViewer(ctx, "kid", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	kid, err := a.users.ActiveUser(ctx, kidID)
	if err != nil {
		t.Fatal(err)
	}
	kid.MaxContentRating = "PG"
	if err := a.users.UpdateUser(ctx, *kid); err != nil {
		t.Fatal(err)
	}

	released := time.Date(2023, 7, 21, 0, 0, 0, 0, time.UTC)
	future := time.Now().UTC().AddDate(1, 0, 0)

	oppenheimer := core.Title{
		Key:           core.MovieKey(872585),
		Type:          core.TypeMovie,
		IDs:           core.IDs{TMDB: 872585, IMDB: "tt15398776", TVDB: 345678},
		Title:         "Oppenheimer",
		ContentRating: "R",
		Released:      released,
		Popularity:    100,
	}
	paddington := core.Title{
		Key:           core.MovieKey(116149),
		Type:          core.TypeMovie,
		IDs:           core.IDs{TMDB: 116149, IMDB: "tt1109624"},
		Title:         "Paddington",
		ContentRating: "PG",
		Released:      released,
		Popularity:    50,
	}
	adultMovie := core.Title{
		Key:      core.MovieKey(999111),
		Type:     core.TypeMovie,
		IDs:      core.IDs{TMDB: 999111, IMDB: "tt9991110"},
		Title:    "Restricted Adult",
		Adult:    true,
		Released: released,
	}
	futureMovie := core.Title{
		Key:           core.MovieKey(999222),
		Type:          core.TypeMovie,
		IDs:           core.IDs{TMDB: 999222, IMDB: "tt9992220"},
		Title:         "Unreleased Future",
		ContentRating: "PG",
		Released:      future,
	}
	show := core.Title{
		Key:           core.ShowKey(1396),
		Type:          core.TypeShow,
		IDs:           core.IDs{TMDB: 1396, IMDB: "tt0903747", TVDB: 81189},
		Title:         "Breaking Bad",
		ContentRating: "TV-MA",
		Released:      released,
		Popularity:    90,
	}
	season := core.Title{
		Key:        core.SeasonKey(1396, 1),
		ShowKey:    show.Key,
		Type:       core.TypeSeason,
		Season:     1,
		IDs:        core.IDs{TMDB: 3572},
		Title:      "Season 1",
		ShowTitle:  show.Title,
		Released:   released,
		ChildCount: 2,
	}
	ep1 := core.Title{
		Key:       core.EpisodeKey(1396, 1, 1),
		ShowKey:   show.Key,
		Type:      core.TypeEpisode,
		Season:    1,
		Episode:   1,
		IDs:       core.IDs{TMDB: 62085, IMDB: "tt0959621", TVDB: 349232},
		Title:     "Pilot",
		ShowTitle: show.Title,
		Released:  released,
	}
	ep2 := core.Title{
		Key:       core.EpisodeKey(1396, 1, 2),
		ShowKey:   show.Key,
		Type:      core.TypeEpisode,
		Season:    1,
		Episode:   2,
		IDs:       core.IDs{TMDB: 62086},
		Title:     "Cat's in the Bag...",
		ShowTitle: show.Title,
		Released:  released,
	}
	for _, item := range []core.Title{oppenheimer, paddington, adultMovie, futureMovie, show} {
		if err := a.catalog.UpsertTitle(ctx, item, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, child := range []core.Title{season, ep1, ep2} {
		if err := a.catalog.UpsertTitle(ctx, child, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.state.Scrobble(ctx, owner.ID, oppenheimer.Key); err != nil {
		t.Fatal(err)
	}
	if err := a.state.Scrobble(ctx, owner.ID, ep1.Key); err != nil {
		t.Fatal(err)
	}

	type itemsResp struct {
		Items []struct {
			ID          string            `json:"Id"`
			Name        string            `json:"Name"`
			Type        string            `json:"Type"`
			ProviderIds map[string]string `json:"ProviderIds"`
			UserData    struct {
				Played bool `json:"Played"`
			} `json:"UserData"`
		} `json:"Items"`
		TotalRecordCount int `json:"TotalRecordCount"`
	}

	for _, surface := range []string{"jellyfin", "emby"} {
		t.Run(surface, func(t *testing.T) {
			inst, err := a.frontends.registry.Mount(ctx, core.Protocol(surface))
			if err != nil {
				t.Fatal(err)
			}
			defer a.frontends.registry.Unmount(ctx, core.Protocol(surface))

			call := func(method, path, body, token string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.RemoteAddr = "192.0.2.20:1234"
				r.Header.Set("Authorization", `MediaBrowser Client="Floppy", DeviceId="sync", Token="`+token+`"`)
				inst.Handler().ServeHTTP(w, r)
				return w
			}
			loginAs := func(username string) (string, string) {
				res := call("POST", "/Users/AuthenticateByName", `{"Username":"`+username+`","Pw":"password-1234"}`, "")
				if res.Code != 200 {
					t.Fatalf("login %s: %d %s", username, res.Code, res.Body)
				}
				var body struct {
					AccessToken string
					User        struct {
						ID string `json:"Id"`
					}
				}
				if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				return body.AccessToken, body.User.ID
			}
			getItems := func(path, token string) itemsResp {
				t.Helper()
				res := call("GET", path, "", token)
				if res.Code != 200 {
					t.Fatalf("GET %s: %d %s", path, res.Code, res.Body)
				}
				var out itemsResp
				if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
					t.Fatalf("decode %s: %v (%s)", path, err, res.Body)
				}
				return out
			}

			ownerToken, ownerUID := loginAs("viewer")
			kidToken, kidUID := loginAs("kid")

			// Exact Floppy lookup on both /Users/{id}/Items and /Items.
			for _, prefix := range []string{"/Users/" + ownerUID + "/Items", "/Items"} {
				got := getItems(prefix+"?Recursive=true&IncludeItemTypes=Movie,Episode&Fields=ProviderIds&AnyProviderIdEquals=tmdb.872585&Limit=2", ownerToken)
				if got.TotalRecordCount != 1 || len(got.Items) != 1 || got.Items[0].Name != "Oppenheimer" {
					t.Fatalf("%s tmdb movie lookup = %+v", prefix, got)
				}
				if got.Items[0].ProviderIds["Tmdb"] != "872585" || got.Items[0].ProviderIds["Imdb"] != "tt15398776" {
					t.Fatalf("%s ProviderIds = %+v", prefix, got.Items[0].ProviderIds)
				}
			}

			// Case-insensitive provider names across IMDb, TVDB, and episode TMDB.
			if got := getItems("/Users/"+ownerUID+"/Items?Recursive=true&IncludeItemTypes=Movie,Episode&AnyProviderIdEquals=IMDB.TT15398776&Limit=2", ownerToken); got.TotalRecordCount != 1 || len(got.Items) != 1 || got.Items[0].Name != "Oppenheimer" {
				t.Fatalf("imdb lookup = %+v", got)
			}
			if got := getItems("/Users/"+ownerUID+"/Items?IncludeItemTypes=Series&AnyProviderIdEquals=Tvdb.81189&Fields=ProviderIds", ownerToken); got.TotalRecordCount != 1 || len(got.Items) != 1 || got.Items[0].Name != "Breaking Bad" || got.Items[0].ProviderIds["Tvdb"] != "81189" {
				t.Fatalf("tvdb series lookup = %+v", got)
			}
			if got := getItems("/Users/"+ownerUID+"/Items?Recursive=true&IncludeItemTypes=Movie,Episode&Fields=ProviderIds&AnyProviderIdEquals=TMDB.62085&Limit=2", ownerToken); got.TotalRecordCount != 1 || len(got.Items) != 1 || got.Items[0].Name != "Pilot" || got.Items[0].ProviderIds["Tmdb"] != "62085" {
				t.Fatalf("episode tmdb lookup = %+v", got)
			}
			// Recursive=false excludes episodes at root.
			if got := getItems("/Users/"+ownerUID+"/Items?Recursive=false&IncludeItemTypes=Episode&AnyProviderIdEquals=tmdb.62085", ownerToken); got.TotalRecordCount != 0 || len(got.Items) != 0 {
				t.Fatalf("non-recursive episode lookup = %+v, want empty", got)
			}

			// Comma-separated list deduplicates same item and respects Limit.
			if got := getItems("/Users/"+ownerUID+"/Items?Recursive=true&AnyProviderIdEquals=tmdb.872585,imdb.tt15398776&Limit=5", ownerToken); got.TotalRecordCount != 1 || len(got.Items) != 1 {
				t.Fatalf("dedup provider list = %+v", got)
			}
			if got := getItems("/Users/"+ownerUID+"/Items?Recursive=true&IncludeItemTypes=Movie&AnyProviderIdEquals=tmdb.872585,tmdb.116149&Limit=1", ownerToken); got.TotalRecordCount != 2 || len(got.Items) != 1 {
				t.Fatalf("multi provider list with Limit=1 = %+v", got)
			}

			// Type mismatch, unknown provider, malformed value, and missing id return TotalRecordCount 0.
			for _, query := range []string{
				"IncludeItemTypes=Series&AnyProviderIdEquals=tmdb.872585",
				"Recursive=true&AnyProviderIdEquals=anidb.12345",
				"Recursive=true&AnyProviderIdEquals=tmdb.404404404",
				"Recursive=true&AnyProviderIdEquals=malformed",
			} {
				if got := getItems("/Users/"+ownerUID+"/Items?"+query, ownerToken); got.TotalRecordCount != 0 || len(got.Items) != 0 {
					t.Fatalf("query %q = %+v, want empty TotalRecordCount=0", query, got)
				}
			}

			// Visibility rules: adult, unreleased future, and viewer MaxContentRating.
			for _, query := range []string{
				"Recursive=true&AnyProviderIdEquals=tmdb.999111",
				"Recursive=true&AnyProviderIdEquals=tmdb.999222",
			} {
				if got := getItems("/Users/"+ownerUID+"/Items?"+query, ownerToken); got.TotalRecordCount != 0 || len(got.Items) != 0 {
					t.Fatalf("visibility %q leaked %+v", query, got)
				}
			}
			if got := getItems("/Users/"+kidUID+"/Items?Recursive=true&IncludeItemTypes=Movie,Episode&AnyProviderIdEquals=tmdb.872585", kidToken); got.TotalRecordCount != 0 || len(got.Items) != 0 {
				t.Fatalf("kid saw R movie: %+v", got)
			}
			if got := getItems("/Users/"+kidUID+"/Items?Recursive=true&IncludeItemTypes=Movie,Episode&AnyProviderIdEquals=tmdb.62085", kidToken); got.TotalRecordCount != 0 || len(got.Items) != 0 {
				t.Fatalf("kid saw TV-MA episode: %+v", got)
			}
			if got := getItems("/Users/"+kidUID+"/Items?Recursive=true&IncludeItemTypes=Movie,Episode&AnyProviderIdEquals=tmdb.116149", kidToken); got.TotalRecordCount != 1 || len(got.Items) != 1 || got.Items[0].Name != "Paddington" {
				t.Fatalf("kid PG lookup = %+v", got)
			}

			// IsPlayed=true and IsPlayed=false on /Users/{id}/Items (and combined with AnyProviderIdEquals).
			played := getItems("/Users/"+ownerUID+"/Items?Recursive=true&IncludeItemTypes=Movie,Episode&IsPlayed=true&Limit=10", ownerToken)
			if played.TotalRecordCount != 2 || len(played.Items) != 2 {
				t.Fatalf("IsPlayed=true movies+episodes = %+v", played)
			}
			for _, it := range played.Items {
				if !it.UserData.Played || (it.Name != "Oppenheimer" && it.Name != "Pilot") {
					t.Fatalf("unexpected played item: %+v", it)
				}
			}
			unplayed := getItems("/Users/"+ownerUID+"/Items?Recursive=true&IncludeItemTypes=Movie,Episode&IsPlayed=false&Limit=10", ownerToken)
			if unplayed.TotalRecordCount != 2 || len(unplayed.Items) != 2 {
				t.Fatalf("IsPlayed=false movies+episodes = %+v", unplayed)
			}
			for _, it := range unplayed.Items {
				if it.UserData.Played || (it.Name != "Paddington" && it.Name != "Cat's in the Bag...") {
					t.Fatalf("unexpected unplayed item: %+v", it)
				}
			}
			rootPlayed := getItems("/Users/"+ownerUID+"/Items?IsPlayed=true&Limit=10", ownerToken)
			if rootPlayed.TotalRecordCount != 1 || len(rootPlayed.Items) != 1 || rootPlayed.Items[0].Name != "Oppenheimer" {
				t.Fatalf("root IsPlayed=true = %+v", rootPlayed)
			}
			if got := getItems("/Users/"+ownerUID+"/Items?Recursive=true&IncludeItemTypes=Movie,Episode&AnyProviderIdEquals=tmdb.872585&IsPlayed=true", ownerToken); got.TotalRecordCount != 1 || len(got.Items) != 1 {
				t.Fatalf("provider + IsPlayed=true = %+v", got)
			}
			if got := getItems("/Users/"+ownerUID+"/Items?Recursive=true&IncludeItemTypes=Movie,Episode&AnyProviderIdEquals=tmdb.872585&IsPlayed=false", ownerToken); got.TotalRecordCount != 0 || len(got.Items) != 0 {
				t.Fatalf("provider + IsPlayed=false = %+v", got)
			}
		})
	}
}

func TestMediaBrowserLatestHonorsLibrarySources(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	if err = local.Create(ctx, owner.ID, "viewer", "password-1234"); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	// Movies: outsideNewest is newer than both collection movies and must NEVER
	// appear in the collection library's /Items/Latest response.
	outsideNewest := core.Title{Key: core.MovieKey(9001), Type: core.TypeMovie, IDs: core.IDs{TMDB: 9001}, Title: "Outside Newest", Released: base.Add(-1 * time.Hour), Genres: []string{"Action"}}
	collPlayed := core.Title{Key: core.MovieKey(9002), Type: core.TypeMovie, IDs: core.IDs{TMDB: 9002}, Title: "Collection Played", Released: base.Add(-2 * time.Hour), Genres: []string{"Drama"}}
	collUnplayed := core.Title{Key: core.MovieKey(9003), Type: core.TypeMovie, IDs: core.IDs{TMDB: 9003}, Title: "Collection Unplayed", Released: base.Add(-4 * time.Hour), Genres: []string{"Comedy"}}
	srcOnly := core.Title{Key: core.MovieKey(9004), Type: core.TypeMovie, IDs: core.IDs{TMDB: 9004}, Title: "Custom Source Film", Released: base.Add(-6 * time.Hour), Genres: []string{"Sci-Fi"}}
	for _, m := range []core.Title{outsideNewest, collPlayed, collUnplayed, srcOnly} {
		if err := a.catalog.UpsertTitle(ctx, m, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.state.Scrobble(ctx, owner.ID, collPlayed.Key); err != nil {
		t.Fatal(err)
	}

	// Shows: collShowA premiered earlier than collShowB, but has a newer aired
	// episode; outsideShow has the newest episode overall and must not leak.
	collShowA := core.Title{Key: core.ShowKey(9101), Type: core.TypeShow, IDs: core.IDs{TMDB: 9101}, Title: "Collection Show A", Released: base.Add(-400 * time.Hour)}
	collShowB := core.Title{Key: core.ShowKey(9102), Type: core.TypeShow, IDs: core.IDs{TMDB: 9102}, Title: "Collection Show B", Released: base.Add(-200 * time.Hour)}
	outsideShow := core.Title{Key: core.ShowKey(9103), Type: core.TypeShow, IDs: core.IDs{TMDB: 9103}, Title: "Outside Show", Released: base.Add(-100 * time.Hour)}
	for _, sh := range []core.Title{collShowA, collShowB, outsideShow} {
		if err := a.catalog.UpsertTitle(ctx, sh, true); err != nil {
			t.Fatal(err)
		}
	}
	epA := core.Title{Key: core.EpisodeKey(9101, 2, 1), ShowKey: collShowA.Key, Type: core.TypeEpisode, Season: 2, Episode: 1, Title: "Show A Latest Ep", Released: base.Add(-3 * time.Hour)}
	epB := core.Title{Key: core.EpisodeKey(9102, 1, 1), ShowKey: collShowB.Key, Type: core.TypeEpisode, Season: 1, Episode: 1, Title: "Show B Ep", Released: base.Add(-10 * time.Hour)}
	epOut := core.Title{Key: core.EpisodeKey(9103, 1, 1), ShowKey: outsideShow.Key, Type: core.TypeEpisode, Season: 1, Episode: 1, Title: "Outside Ep", Released: base.Add(-30 * time.Minute)}
	for _, ep := range []core.Title{epA, epB, epOut} {
		if err := a.catalog.UpsertTitle(ctx, ep, false); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := a.db.ExecContext(ctx, `INSERT INTO collections_members(spec_id,pos,key,shared) VALUES (701,0,?,1),(701,1,?,1),(702,0,?,1),(702,1,?,1)`,
		int64(collPlayed.Key), int64(collUnplayed.Key), int64(collShowA.Key), int64(collShowB.Key)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO libraries_catalogue_members(source_id,key) VALUES ('stremio-curated',?)`, int64(srcOnly.Key)); err != nil {
		t.Fatal(err)
	}

	moviePresent := core.Presentations{
		MediaBrowser: &core.MediaBrowserPresentation{CollectionType: "movies"},
	}
	tvPresent := core.Presentations{
		MediaBrowser: &core.MediaBrowserPresentation{CollectionType: "tvshows"},
	}
	collMovieLib, err := a.libraries.CreateCollection(ctx, "Curated Films", 701, core.TypeMovie, moviePresent)
	if err != nil {
		t.Fatal(err)
	}
	collShowLib, err := a.libraries.CreateCollection(ctx, "Curated Shows", 702, core.TypeShow, tvPresent)
	if err != nil {
		t.Fatal(err)
	}
	srcLib, err := a.libraries.Create(ctx, "Stremio Films", core.TypeMovie, nil, "", moviePresent)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.libraries.SetSources(ctx, srcLib.ID, []string{"stremio-curated"}); err != nil {
		t.Fatal(err)
	}
	ruleLib, err := a.libraries.Create(ctx, "Comedy Only", core.TypeMovie, []core.LibraryExclusion{
		{Name: "Show Comedy", Action: "show", MediaType: "movie", Match: "all", Rules: []core.CollectionRule{{Field: "genre", Op: "is", Value: json.RawMessage(`"Comedy"`)}}},
	}, "hide", moviePresent)
	if err != nil {
		t.Fatal(err)
	}

	collMovieGUID, _ := ids.GUID(ids.View, int64(collMovieLib.ID))
	collShowGUID, _ := ids.GUID(ids.View, int64(collShowLib.ID))
	srcGUID, _ := ids.GUID(ids.View, int64(srcLib.ID))
	ruleGUID, _ := ids.GUID(ids.View, int64(ruleLib.ID))

	for _, surface := range []string{"jellyfin", "emby"} {
		t.Run(surface, func(t *testing.T) {
			inst, err := a.frontends.registry.Mount(ctx, core.Protocol(surface))
			if err != nil {
				t.Fatal(err)
			}
			defer a.frontends.registry.Unmount(ctx, core.Protocol(surface))
			call := func(method, path, body, token string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.RemoteAddr = "192.0.2.20:1234"
				r.Header.Set("Authorization", `MediaBrowser Client="TV", DeviceId="tv", Token="`+token+`"`)
				inst.Handler().ServeHTTP(w, r)
				return w
			}
			login := call("POST", "/Users/AuthenticateByName", `{"Username":"viewer","Pw":"password-1234"}`, "")
			if login.Code != 200 {
				t.Fatalf("login %d %s", login.Code, login.Body)
			}
			var auth struct {
				AccessToken string
				User        struct {
					ID string `json:"Id"`
				}
			}
			if err := json.Unmarshal(login.Body.Bytes(), &auth); err != nil {
				t.Fatal(err)
			}
			latestNames := func(path string) []string {
				t.Helper()
				rec := call("GET", path, "", auth.AccessToken)
				if rec.Code != 200 {
					t.Fatalf("GET %s -> %d %s", path, rec.Code, rec.Body.String())
				}
				var items []struct{ Name string }
				if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
					t.Fatalf("decode %s: %v (%s)", path, err, rec.Body.String())
				}
				names := make([]string, len(items))
				for i, it := range items {
					names[i] = it.Name
				}
				return names
			}

			// Default Latest hides played items and returns only collection members.
			if got := latestNames("/Users/" + auth.User.ID + "/Items/Latest?ParentId=" + collMovieGUID + "&Limit=8"); len(got) != 1 || got[0] != "Collection Unplayed" {
				t.Fatalf("collection movie latest (default unplayed) = %v, want [Collection Unplayed]", got)
			}
			if got := latestNames("/Items/Latest?ParentId=" + collMovieGUID + "&IsPlayed=true&Limit=8"); len(got) != 1 || got[0] != "Collection Played" {
				t.Fatalf("collection movie latest (IsPlayed=true) = %v, want [Collection Played]", got)
			}

			// Show collection: grouped orders by latest aired episode; ungrouped returns scoped episodes.
			if got := latestNames("/Users/" + auth.User.ID + "/Items/Latest?ParentId=" + collShowGUID + "&Limit=8"); len(got) != 2 || got[0] != "Collection Show A" || got[1] != "Collection Show B" {
				t.Fatalf("collection show latest grouped = %v, want [Collection Show A, Collection Show B]", got)
			}
			if got := latestNames("/Users/" + auth.User.ID + "/Items/Latest?ParentId=" + collShowGUID + "&GroupItems=false&Limit=8"); len(got) != 2 || got[0] != "Show A Latest Ep" || got[1] != "Show B Ep" {
				t.Fatalf("collection show latest ungrouped = %v, want [Show A Latest Ep, Show B Ep]", got)
			}

			// Custom catalogue source and rule-filtered catalogue libraries.
			if got := latestNames("/Users/" + auth.User.ID + "/Items/Latest?ParentId=" + srcGUID + "&Limit=8"); len(got) != 1 || got[0] != "Custom Source Film" {
				t.Fatalf("custom source latest = %v, want [Custom Source Film]", got)
			}
			if got := latestNames("/Users/" + auth.User.ID + "/Items/Latest?ParentId=" + ruleGUID + "&Limit=8"); len(got) != 1 || got[0] != "Collection Unplayed" {
				t.Fatalf("rule-filtered latest = %v, want [Collection Unplayed]", got)
			}
		})
	}
}
