package plex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/favorites"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

// kindExperience is a frontend.Experience that publishes two plugin
// libraries: a music library (artist -> album -> track) and a photo library,
// backed by in-memory items. It exists to prove the Plex presentation mapping
// (spec §6.4) without the plugin store.
type kindExperience struct {
	frontend.Experience
}

func newKindExperience() *kindExperience {
	return &kindExperience{Experience: frontend.NoopExperience()}
}

func (e *kindExperience) PublishedViews(context.Context) ([]frontend.View, error) {
	return []frontend.View{
		{ID: "7", LibraryID: 7, Name: "Field Recordings", Type: core.TypeShow, Virtual: true, KindKey: "recordings", PluginID: "fieldrec", Native: plexBaseArtist},
		{ID: "8", LibraryID: 8, Name: "Snapshots", Type: core.TypeShow, Virtual: true, KindKey: "snapshots", PluginID: "fieldrec", Native: plexBasePhoto},
	}, nil
}

// Music library 7: artist 101 -> album 102 -> track 103.
// Photo library 8: album 1201 -> photo 1202.
func kindMusicArtist() core.Title {
	return core.Title{Key: core.PluginKey(101), Type: core.TypeShow, Title: "Artist One", ChildCount: 1, KindLevel: "artist", LibraryID: 7}
}
func kindMusicAlbum() core.Title {
	return core.Title{Key: core.PluginKey(102), Type: core.TypeSeason, Title: "Album One", ChildCount: 1, ShowKey: core.PluginKey(101), ShowTitle: "Artist One", KindLevel: "album", LibraryID: 7}
}
func kindMusicTrack() core.Title {
	return core.Title{Key: core.PluginKey(103), Type: core.TypeEpisode, Title: "Track One", Episode: 1, Season: 1, ShowKey: core.PluginKey(101), ShowTitle: "Artist One", KindLevel: "track", LibraryID: 7, Runtime: 210 * time.Second}
}
func kindPhotoAlbum() core.Title {
	return core.Title{Key: core.PluginKey(1201), Type: core.TypeShow, Title: "Album One", ChildCount: 1, KindLevel: "album", LibraryID: 8}
}
func kindPhoto() core.Title {
	return core.Title{Key: core.PluginKey(1202), Type: core.TypeEpisode, Title: "Photo One", Episode: 1, Season: 1, ShowKey: core.PluginKey(1201), ShowTitle: "Album One", KindLevel: "photo", LibraryID: 8}
}

func (e *kindExperience) LibraryItems(_ context.Context, _ *frontend.Viewer, _ frontend.Device, viewID string, _ frontend.ItemQuery) (core.Page, error) {
	switch viewID {
	case "7":
		return core.Page{Items: []core.Title{kindMusicArtist()}, Total: 1}, nil
	case "8":
		return core.Page{Items: []core.Title{kindPhotoAlbum()}, Total: 1}, nil
	}
	return core.Page{}, nil
}

func (e *kindExperience) Item(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
	for _, t := range []core.Title{kindMusicArtist(), kindMusicAlbum(), kindMusicTrack(), kindPhotoAlbum(), kindPhoto()} {
		if t.Key == req.Key {
			return frontend.ItemDetail{Title: t}, nil
		}
	}
	return frontend.ItemDetail{}, core.ErrNotFound
}

func (e *kindExperience) Children(_ context.Context, _ *frontend.Viewer, _ frontend.Device, key core.RatingKey, _ frontend.ItemQuery) (core.Page, error) {
	switch key {
	case kindMusicArtist().Key:
		return core.Page{Items: []core.Title{kindMusicAlbum()}, Total: 1}, nil
	case kindMusicAlbum().Key:
		return core.Page{Items: []core.Title{kindMusicTrack()}, Total: 1}, nil
	case kindPhotoAlbum().Key:
		return core.Page{Items: []core.Title{kindPhoto()}, Total: 1}, nil
	}
	return core.Page{}, nil
}

// findDirectory returns the /library/sections directory for the given title.
func findDirectory(t *testing.T, body string, title string) map[string]any {
	t.Helper()
	var out struct {
		MediaContainer struct {
			Directory []map[string]any `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode sections: %v", err)
	}
	for _, d := range out.MediaContainer.Directory {
		if d["title"] == title {
			return d
		}
	}
	return nil
}

func serveSections(t *testing.T, srv *Server, user *frontend.User) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
	req.Header.Set("Accept", "application/json")
	req = req.WithContext(withRequest(req.Context(), &Request{User: user, Format: model.JSON}))
	srv.handleSections(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sections status = %d", rec.Code)
	}
	return rec.Body.String()
}

func TestKindSectionPresentation(t *testing.T) {
	srv := newTestServer(testServerDeps{Experience: newKindExperience()})
	body := serveSections(t, srv, &ownerUser)

	music := findDirectory(t, body, "Field Recordings")
	if music == nil {
		t.Fatalf("music library missing from sections: %s", body)
	}
	if music["type"] != plexBaseArtist {
		t.Errorf("music section type = %v, want %s", music["type"], plexBaseArtist)
	}
	photo := findDirectory(t, body, "Snapshots")
	if photo == nil {
		t.Fatalf("photo library missing from sections: %s", body)
	}
	if photo["type"] != plexBasePhoto {
		t.Errorf("photo section type = %v, want %s", photo["type"], plexBasePhoto)
	}
	if movies := findDirectory(t, body, "Movies"); movies != nil {
		t.Errorf("unpublished Movies section was invented: %v", movies)
	}
}

func TestKindSectionViewerRestricted(t *testing.T) {
	srv := newTestServer(testServerDeps{Experience: newKindExperience()})
	viewer := &frontend.User{ID: 42, AllowedLibraries: []core.LibraryID{7}}
	body := serveSections(t, srv, viewer)

	if findDirectory(t, body, "Field Recordings") == nil {
		t.Errorf("allowed music library missing: %s", body)
	}
	if findDirectory(t, body, "Snapshots") != nil {
		t.Errorf("restricted viewer saw the photo library: %s", body)
	}
}

func TestKindSectionListsArtists(t *testing.T) {
	srv := newTestServer(testServerDeps{Experience: newKindExperience()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/library/sections/7/all", nil)
	req.Header.Set("Accept", "application/json")
	req = req.WithContext(withRequest(req.Context(), &Request{User: &ownerUser, Format: model.JSON}))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("section", "7")
	rctx.URLParams.Add("sub", "all")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	srv.handleSectionSub(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		MediaContainer struct {
			Metadata []map[string]any `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.MediaContainer.Metadata) != 1 {
		t.Fatalf("artist list = %s", rec.Body.String())
	}
	if got := out.MediaContainer.Metadata[0]["type"]; got != plexBaseArtist {
		t.Errorf("artist item type = %v, want %s", got, plexBaseArtist)
	}
	if got := out.MediaContainer.Metadata[0]["title"]; got != "Artist One" {
		t.Errorf("artist title = %v", got)
	}
}

// serveKindRoute calls a handler with {key}/{section}/{sub} URL params set.
func serveKindRoute(t *testing.T, srv *Server, handler http.HandlerFunc, target string, params map[string]string, user *frontend.User) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Accept", "application/json")
	req = req.WithContext(withRequest(req.Context(), &Request{User: user, Format: model.JSON}))
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	handler(rec, req)
	return rec
}

// decodeMetadata decodes a single-object MediaContainer's Metadata list.
func decodeMetadata(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var out struct {
		MediaContainer struct {
			Metadata []map[string]any `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode metadata: %v\n%s", err, body)
	}
	return out.MediaContainer.Metadata
}

// TestKindMusicChildrenTypeFromLevel proves a music section's artist lists its
// album child typed album (9), and the album lists its track child typed track
// (10), each carrying its Part so a client can stream it.
func TestKindMusicChildrenTypeFromLevel(t *testing.T) {
	srv := newTestServer(testServerDeps{Experience: newKindExperience()})

	artistKey := kindMusicArtist().Key.String()
	albums := decodeMetadata(t, serveKindRoute(t, srv, srv.handleChildren, "/library/metadata/"+artistKey+"/children", map[string]string{"key": artistKey}, &ownerUser).Body.Bytes())
	if len(albums) != 1 {
		t.Fatalf("album children = %d, want 1", len(albums))
	}
	if got := albums[0]["type"]; got != plexBaseAlbum {
		t.Errorf("album type = %v, want %s", got, plexBaseAlbum)
	}
	if got := albums[0]["title"]; got != "Album One" {
		t.Errorf("album title = %v", got)
	}

	albumKey := kindMusicAlbum().Key.String()
	tracks := decodeMetadata(t, serveKindRoute(t, srv, srv.handleChildren, "/library/metadata/"+albumKey+"/children", map[string]string{"key": albumKey}, &ownerUser).Body.Bytes())
	if len(tracks) != 1 {
		t.Fatalf("track children = %d, want 1", len(tracks))
	}
	if got := tracks[0]["type"]; got != plexBaseTrack {
		t.Errorf("track type = %v, want %s", got, plexBaseTrack)
	}
	if got := tracks[0]["title"]; got != "Track One" {
		t.Errorf("track title = %v", got)
	}
}

// TestKindPhotoChildrenTypeFromLevel proves a photo section's album lists its
// photo child typed photo (13) with its Part.
func TestKindPhotoChildrenTypeFromLevel(t *testing.T) {
	srv := newTestServer(testServerDeps{Experience: newKindExperience()})
	albumKey := kindPhotoAlbum().Key.String()
	photos := decodeMetadata(t, serveKindRoute(t, srv, srv.handleChildren, "/library/metadata/"+albumKey+"/children", map[string]string{"key": albumKey}, &ownerUser).Body.Bytes())
	if len(photos) != 1 {
		t.Fatalf("photo children = %d, want 1", len(photos))
	}
	if got := photos[0]["type"]; got != plexBasePhoto {
		t.Errorf("photo type = %v, want %s", got, plexBasePhoto)
	}
}

// TestKindTrackMetadataHasPart proves a plugin track's detail page renders a
// Media with a Part whose key is a Plex part URL, so the client streams bytes
// from the shared pipeline.
func TestKindTrackMetadataHasPart(t *testing.T) {
	srv := newTestServer(testServerDeps{Experience: newKindExperience()})
	trackKey := kindMusicTrack().Key.String()
	rec := serveKindRoute(t, srv, srv.handleMetadata, "/library/metadata/"+trackKey, map[string]string{"key": trackKey}, &ownerUser)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	md := decodeMetadata(t, rec.Body.Bytes())
	if len(md) != 1 {
		t.Fatalf("track metadata = %d, want 1", len(md))
	}
	if got := md[0]["type"]; got != plexBaseTrack {
		t.Errorf("track detail type = %v, want %s", got, plexBaseTrack)
	}
	media, ok := md[0]["Media"].([]any)
	if !ok || len(media) == 0 {
		t.Fatalf("track detail has no Media: %v", md[0])
	}
	first := media[0].(map[string]any)
	parts, ok := first["Part"].([]any)
	if !ok || len(parts) == 0 {
		t.Fatalf("track Media has no Part: %v", first)
	}
	key, _ := parts[0].(map[string]any)["key"].(string)
	if !strings.HasPrefix(key, "/library/parts/") {
		t.Errorf("track Part key = %q, want /library/parts/...", key)
	}
}

// kindStreamSource serves fixed bytes as a core.MediaSource for a plugin item.
type kindStreamSource struct {
	data []byte
	name string
}

func (k *kindStreamSource) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(k.data)) {
		return 0, io.EOF
	}
	n := copy(p, k.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
func (k *kindStreamSource) Size() int64       { return int64(len(k.data)) }
func (k *kindStreamSource) Name() string      { return k.name }
func (k *kindStreamSource) ReleaseID() string { return "kind:" + k.name }
func (k *kindStreamSource) Close() error      { return nil }

// TestKindPartStreamsBytes proves a plugin track/photo Part streams its bytes
// through the same Plex part endpoint a catalogue item uses, routed to the
// shared KindPlayback pipeline (spec §15.1), with Range support.
func TestKindPartStreamsBytes(t *testing.T) {
	data := bytes.Repeat([]byte("kindbytes"), 64)
	fake := newFakePlayback(sampleInfo)
	kindSrc := func(_ context.Context, _ *core.Viewer, key core.RatingKey) (*core.KindInput, error) {
		title := kindMusicTrack()
		if key == kindPhoto().Key {
			title = kindPhoto()
		}
		return &core.KindInput{
			Title:    &title,
			SourceID: key.String(),
			Open: func(context.Context) (core.MediaSource, error) {
				return &kindStreamSource{data: data, name: key.String()}, nil
			},
		}, nil
	}
	rt := sessions.NewLocalRuntime(sessions.LocalRuntimeDeps{Playback: fake, KindSource: kindSrc})
	srv := newTestServer(testServerDeps{Experience: newKindExperience(), Deps: Deps{Host: frontend.Host{Sessions: rt}, Playback: fake}})
	defer srv.Close()

	for _, key := range []core.RatingKey{kindMusicTrack().Key, kindPhoto().Key} {
		part := key.String()
		rec := serveKindRoute(t, srv, srv.handlePart, "/library/parts/"+part+"/1/file.mkv", map[string]string{"part": part, "changestamp": "1", "file": "file.mkv"}, &ownerUser)
		if rec.Code != http.StatusPartialContent && rec.Code != http.StatusOK {
			t.Fatalf("part %s status=%d body=%s", key, rec.Code, rec.Body.String())
		}
		if !bytes.Equal(rec.Body.Bytes(), data) {
			t.Fatalf("part %s served %d bytes, want %d", key, rec.Body.Len(), len(data))
		}
	}
}

type kindMusicPluginStore struct {
	lib                   core.Library
	extra                 []core.Library
	artists               []core.Title
	albums                []core.Title
	tracks                []core.Title
	rejectUndeclaredQuery bool
}

func (s *kindMusicPluginStore) Libraries(context.Context) ([]core.Library, error) {
	out := []core.Library{
		{ID: core.LibraryMovies, Name: "Movies", Source: core.LibraryBuiltin, MediaType: core.TypeMovie},
		{ID: core.LibraryShows, Name: "TV Shows", Source: core.LibraryBuiltin, MediaType: core.TypeShow},
		s.lib,
	}
	out = append(out, s.extra...)
	return out, nil
}

func (s *kindMusicPluginStore) Library(_ context.Context, id core.LibraryID) (core.Library, error) {
	switch id {
	case core.LibraryMovies:
		return core.Library{ID: core.LibraryMovies, Name: "Movies", Source: core.LibraryBuiltin, MediaType: core.TypeMovie}, nil
	case core.LibraryShows:
		return core.Library{ID: core.LibraryShows, Name: "TV Shows", Source: core.LibraryBuiltin, MediaType: core.TypeShow}, nil
	case s.lib.ID:
		return s.lib, nil
	}
	for _, lib := range s.extra {
		if lib.ID == id {
			return lib, nil
		}
	}
	return core.Library{}, core.ErrNotFound
}

func (s *kindMusicPluginStore) IsPluginLibraryActive(context.Context, core.Library) bool { return true }
func (s *kindMusicPluginStore) RootMediaType(context.Context, core.Library) core.MediaType {
	return core.TypeShow
}
func (s *kindMusicPluginStore) Count(context.Context, core.Library) (int, error) {
	return len(s.artists), nil
}
func (s *kindMusicPluginStore) Browse(_ context.Context, lib core.Library, q frontend.ItemQuery, _ string, limit int) (experience.PluginBrowsePage, error) {
	if lib.ID != s.lib.ID {
		return experience.PluginBrowsePage{}, nil
	}
	if s.rejectUndeclaredQuery && (q.Sort != "" || len(q.Filter.Genres) > 0 || q.Watched != nil) {
		return experience.PluginBrowsePage{}, errors.New("undeclared sort or filter reached plugin store")
	}
	items := s.artists
	next := ""
	if limit > 0 && len(items) > limit {
		items = items[:limit]
		next = "more"
	}
	return experience.PluginBrowsePage{Items: append([]core.Title(nil), items...), Total: len(s.artists), NextCursor: next}, nil
}
func (s *kindMusicPluginStore) Item(_ context.Context, key core.RatingKey) (experience.PluginItemView, error) {
	for _, list := range [][]core.Title{s.artists, s.albums, s.tracks} {
		for _, t := range list {
			if t.Key == key {
				return experience.PluginItemView{
					Title:    t,
					Library:  s.lib,
					Playable: t.Type == core.TypeEpisode,
				}, nil
			}
		}
	}
	return experience.PluginItemView{}, core.ErrNotFound
}
func (s *kindMusicPluginStore) ItemsMany(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]experience.PluginItemView, error) {
	out := make(map[core.RatingKey]experience.PluginItemView, len(keys))
	for _, k := range keys {
		if piv, err := s.Item(ctx, k); err == nil {
			out[k] = piv
		}
	}
	return out, nil
}
func (s *kindMusicPluginStore) Children(_ context.Context, parent core.RatingKey, _ bool, _ string, _ int) (experience.PluginBrowsePage, error) {
	switch parent {
	case s.artists[0].Key:
		return experience.PluginBrowsePage{Items: append([]core.Title(nil), s.albums...), Total: len(s.albums)}, nil
	case s.albums[0].Key:
		return experience.PluginBrowsePage{Items: append([]core.Title(nil), s.tracks...), Total: len(s.tracks)}, nil
	}
	return experience.PluginBrowsePage{}, nil
}
func (s *kindMusicPluginStore) Ancestors(context.Context, core.RatingKey) ([]core.Title, core.Library, error) {
	return nil, s.lib, nil
}
func (s *kindMusicPluginStore) Facets(context.Context, core.Library, string) ([]core.FacetValue, error) {
	return nil, nil
}
func (s *kindMusicPluginStore) Search(context.Context, core.Library, string, frontend.ItemQuery, int) ([]core.Title, error) {
	return nil, nil
}
func (s *kindMusicPluginStore) MemberOf(context.Context, core.Library, core.RatingKey) (bool, error) {
	return false, nil
}

// TestIssue558PluginMusicSectionHubsAndArtistBrowse proves (#558):
//  1. /hubs/sections/{id} on a plugin music library renders an Artists hub
//     linking to /library/sections/{id}/all?type=8 from Experience.LibraryItems
//     instead of leaking video recommendation rows, while Movies still renders
//     video recommendations.
//  2. /library/metadata/{artist|album}/children and track metadata succeed via
//     StoredItem (and check owning-library access) even when favorites/user-data
//     enrichment refuses plugin keys with favorites.ErrForbidden.
//  3. Experience.Item, Experience.UserData, and Experience.FavoriteItem log and
//     omit plugin enrichment failures instead of turning them into HTTP 500s.
func TestIssue558PluginMusicSectionHubsAndArtistBrowse(t *testing.T) {
	const musicOnlyToken = "music-only"
	const moviesOnlyToken = "movies-only"
	musicUser := core.User{ID: 51, Username: "listener", AllowedLibraries: []core.LibraryID{3}}
	moviesOnlyUser := core.User{ID: 52, Username: "watcher", AllowedLibraries: []core.LibraryID{core.LibraryMovies}}

	artist2 := core.Title{Key: core.PluginKey(104), Type: core.TypeShow, Title: "Artist Two", ChildCount: 1, KindLevel: "artist", LibraryID: 3}
	artist1 := kindMusicArtist()
	artist1.LibraryID = 3
	album1 := kindMusicAlbum()
	album1.LibraryID = 3
	track1 := kindMusicTrack()
	track1.LibraryID = 3

	store := &kindMusicPluginStore{
		lib: core.Library{
			ID:        3,
			Name:      "Subsonic Music",
			Source:    core.LibraryPlugin,
			PluginID:  "subsonic",
			KindKey:   "artists",
			MediaType: core.TypeShow,
			Present:   core.Presentations{Plex: &core.PlexPresentation{Base: plexBaseArtist}},
		},
		artists: []core.Title{artist1, artist2},
		albums:  []core.Title{album1},
		tracks:  []core.Title{track1},
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	rec := &fakeRecommender{
		section: map[core.MediaType][]core.Hub{
			core.TypeMovie: {
				{ID: "movie.trending", Title: "Trending Now", Type: core.TypeMovie, Promoted: true, Items: []core.Title{fightClub}},
				{ID: "movie.short", Title: "Short on Time", Type: core.TypeMovie, Promoted: true, Items: []core.Title{alien}},
			},
			core.TypeShow: {
				{ID: "tv.trending", Title: "Trending Now", Type: core.TypeShow, Promoted: true, Items: []core.Title{breakingBad}},
			},
			core.MediaType(plexBaseArtist): {
				{ID: "artist.trending", Title: "Trending Now", Type: core.MediaType(plexBaseArtist), Promoted: true, Items: []core.Title{fightClub}},
			},
		},
	}

	f := newFixture(func(d *testServerDeps) {
		setDepsAuth(d, &fakeAuth{users: map[string]core.User{
			ownerToken:      ownerUser,
			musicOnlyToken:  musicUser,
			moviesOnlyToken: moviesOnlyUser,
		}})
	})
	defer f.srv.Close()

	expSvc := experience.NewDirectService(experience.ServiceDeps{
		Active: func(_ context.Context, id int64) (*core.User, error) {
			switch id {
			case ownerUser.ID:
				u := ownerUser
				return &u, nil
			case musicUser.ID:
				u := musicUser
				return &u, nil
			case moviesOnlyUser.ID:
				u := moviesOnlyUser
				return &u, nil
			}
			return nil, core.ErrUnauthorized
		},
		Catalog:         f.catalog,
		State:           f.state,
		Playlists:       f.pls,
		Recommender:     rec,
		PluginLibraries: store,
		Protocol:        "plex",
		Log:             logger,
		LibraryPresentation: func(p core.Presentations, mt core.MediaType) (core.MediaType, string, bool) {
			if p.Plex != nil && p.Plex.Base != "" {
				return mt, p.Plex.Base, true
			}
			if mt == core.TypeShow {
				return mt, plexBaseShow, true
			}
			return mt, plexBaseMovie, true
		},
		BuiltinNative: func(mt core.MediaType) string {
			if mt == core.TypeShow {
				return plexBaseShow
			}
			return plexBaseMovie
		},
		Visibility: experience.Visibility{
			SectionFor: func(mt core.MediaType) (int, bool) {
				if mt == core.TypeShow || mt == core.TypeSeason || mt == core.TypeEpisode {
					return 2, true
				}
				return 1, true
			},
		},
		IsFavorite: func(_ context.Context, _ *core.User, k core.RatingKey) (bool, error) {
			if k.IsPluginKey() {
				return false, favorites.ErrForbidden
			}
			return false, nil
		},
		IsFavoriteMany: func(_ context.Context, _ *core.User, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
			for _, k := range keys {
				if k.IsPluginKey() {
					return nil, favorites.ErrForbidden
				}
			}
			return map[core.RatingKey]bool{}, nil
		},
		IsNotInterested: func(_ context.Context, _ *core.User, k core.RatingKey) (bool, error) {
			if k.IsPluginKey() {
				return false, favorites.ErrForbidden
			}
			return false, nil
		},
		IsWatchlisted: func(_ context.Context, _ *core.User, k core.RatingKey) (bool, error) {
			if k.IsPluginKey() {
				return false, favorites.ErrForbidden
			}
			return false, nil
		},
		FavoriteMembership: func(_ context.Context, _ *core.User, k core.RatingKey) (frontend.Membership, error) {
			if k.IsPluginKey() {
				return frontend.Membership{}, favorites.ErrForbidden
			}
			return frontend.Membership{}, nil
		},
	})
	f.srv.bindHost(frontend.Host{Experience: expSvc})

	// (1) Music section hubs (/hubs/sections/3?count=1) are the plugin
	// library's own rows (its rows tab), with their row ids, and never leak
	// video recommendation rows.
	musicHubResp := f.get(t, "/hubs/sections/3?count=1", "Accept", "application/json", "X-Plex-Token", musicOnlyToken)
	if musicHubResp.Code != http.StatusOK {
		t.Fatalf("/hubs/sections/3 status = %d: %s", musicHubResp.Code, musicHubResp.Body.String())
	}
	var hubPayload struct {
		MediaContainer model.MediaContainer `json:"MediaContainer"`
	}
	if err := json.Unmarshal(musicHubResp.Body.Bytes(), &hubPayload); err != nil {
		t.Fatalf("decode music section hubs: %v", err)
	}
	if len(hubPayload.MediaContainer.Hub) == 0 {
		t.Fatalf("music section hubs empty: %s", musicHubResp.Body.String())
	}
	for _, h := range hubPayload.MediaContainer.Hub {
		if !strings.HasPrefix(h.HubIdentifier, "plugin:3:") || h.Type != plexBaseArtist || !strings.HasPrefix(h.Context, "hub.artist.") || len(h.Metadata) != 1 || !h.More {
			t.Fatalf("unexpected music row: %+v", h)
		}
		if h.Metadata[0].Type != plexBaseArtist || h.Metadata[0].Title != "Artist One" {
			t.Fatalf("unexpected artist item in %s: %+v", h.HubIdentifier, h.Metadata[0])
		}
	}

	// Viewer restricted away from section 3 cannot see its hubs or its artists.
	if denied := f.get(t, "/hubs/sections/3", "Accept", "application/json", "X-Plex-Token", moviesOnlyToken); denied.Code != http.StatusNotFound {
		t.Fatalf("unauthorized /hubs/sections/3 status = %d, want 404", denied.Code)
	}
	if denied := f.get(t, "/library/metadata/"+artist1.Key.String()+"/children", "Accept", "application/json", "X-Plex-Token", moviesOnlyToken); denied.Code != http.StatusNotFound {
		t.Fatalf("unauthorized artist children status = %d, want 404", denied.Code)
	}

	// Movies section recommendations still render normally.
	movieHubResp := f.get(t, "/hubs/sections/1", "Accept", "application/json")
	if movieHubResp.Code != http.StatusOK {
		t.Fatalf("/hubs/sections/1 status = %d", movieHubResp.Code)
	}
	var movieHubs struct {
		MediaContainer model.MediaContainer `json:"MediaContainer"`
	}
	if err := json.Unmarshal(movieHubResp.Body.Bytes(), &movieHubs); err != nil || len(movieHubs.MediaContainer.Hub) == 0 {
		t.Fatalf("movies section hubs missing: err=%v body=%s", err, movieHubResp.Body.String())
	}

	// (2) Artist -> Album -> Track browsing returns 200 (not 500).
	albumsResp := f.get(t, "/library/metadata/"+artist1.Key.String()+"/children", "Accept", "application/json", "X-Plex-Token", musicOnlyToken)
	if albumsResp.Code != http.StatusOK {
		t.Fatalf("artist children status = %d: %s", albumsResp.Code, albumsResp.Body.String())
	}
	albums := decodeMetadata(t, albumsResp.Body.Bytes())
	if len(albums) != 1 || albums[0]["type"] != plexBaseAlbum || albums[0]["title"] != "Album One" {
		t.Fatalf("unexpected album children: %+v", albums)
	}

	tracksResp := f.get(t, "/library/metadata/"+album1.Key.String()+"/children", "Accept", "application/json", "X-Plex-Token", musicOnlyToken)
	if tracksResp.Code != http.StatusOK {
		t.Fatalf("album children status = %d: %s", tracksResp.Code, tracksResp.Body.String())
	}
	tracks := decodeMetadata(t, tracksResp.Body.Bytes())
	if len(tracks) != 1 || tracks[0]["type"] != plexBaseTrack || tracks[0]["title"] != "Track One" {
		t.Fatalf("unexpected track children: %+v", tracks)
	}

	// (3) Direct Experience.Item, UserData, and FavoriteItem calls on plugin items
	// log and omit favorites enrichment errors rather than failing with 500.
	ctx := context.Background()
	detail, err := expSvc.Item(ctx, frontend.ItemRequest{Viewer: &musicUser, Key: artist1.Key})
	if err != nil {
		t.Fatalf("Experience.Item on plugin artist failed: %v", err)
	}
	if detail.Title.Key != artist1.Key || detail.UserData.IsFavorite {
		t.Fatalf("unexpected plugin Item detail: %+v", detail)
	}
	ud, err := expSvc.UserData(ctx, &musicUser, []core.RatingKey{artist1.Key})
	if err != nil {
		t.Fatalf("Experience.UserData on plugin artist failed: %v", err)
	}
	if ud[artist1.Key].IsFavorite {
		t.Fatalf("unexpected favorite flag in UserData: %+v", ud)
	}
	mem, err := expSvc.FavoriteItem(ctx, &musicUser, core.Device{}, artist1.Key)
	if err != nil || mem.Present {
		t.Fatalf("Experience.FavoriteItem on plugin artist = (%+v, %v), want empty nil", mem, err)
	}
	if !strings.Contains(logBuf.String(), "favorites") {
		t.Fatalf("expected omitted favorites enrichment to be logged, got %q", logBuf.String())
	}
}

func TestCustomLibrarySectionBrowseSortAndFilter(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	cat, err := catalog.New(db, nil, config.Catalog{}, nil, catalog.WithNow(func() time.Time { return fixedNow }))
	if err != nil {
		t.Fatal(err)
	}
	st := state.New(db, state.Options{Now: func() time.Time { return fixedNow }})

	alpha := core.Title{Key: core.MovieKey(201), Type: core.TypeMovie, IDs: core.IDs{TMDB: 201}, Title: "Alpha", Year: 2020, Released: time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Drama"}}
	bravo := core.Title{Key: core.MovieKey(202), Type: core.TypeMovie, IDs: core.IDs{TMDB: 202}, Title: "Bravo", Year: 2021, Released: time.Date(2021, 3, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Drama", "Sci-Fi"}}
	delta := core.Title{Key: core.MovieKey(203), Type: core.TypeMovie, IDs: core.IDs{TMDB: 203}, Title: "Delta", Year: 2022, Released: time.Date(2022, 3, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Sci-Fi"}}
	zulu := core.Title{Key: core.MovieKey(204), Type: core.TypeMovie, IDs: core.IDs{TMDB: 204}, Title: "Zulu", Year: 1990, Released: time.Date(1990, 3, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Drama", "Sci-Fi"}}
	for _, m := range []core.Title{alpha, bravo, delta, zulu} {
		if err := cat.UpsertTitle(ctx, m, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Scrobble(ctx, ownerUser.ID, bravo.Key); err != nil {
		t.Fatal(err)
	}

	artist1 := kindMusicArtist()
	artist1.LibraryID = 5
	artist2 := core.Title{Key: core.PluginKey(104), Type: core.TypeShow, Title: "Artist Two", ChildCount: 1, KindLevel: "artist", LibraryID: 5}
	libStore := &kindMusicPluginStore{
		lib: core.Library{
			ID:        5,
			Name:      "Field Music",
			Source:    core.LibraryPlugin,
			PluginID:  "subsonic",
			KindKey:   "artists",
			MediaType: core.TypeShow,
			Present:   core.Presentations{Plex: &core.PlexPresentation{Base: plexBaseArtist}},
		},
		extra: []core.Library{{
			ID:                4,
			Key:               "modern-cinema",
			Name:              "Modern Cinema",
			Source:            core.LibraryCatalog,
			MediaType:         core.TypeMovie,
			VisibilityDefault: "hide",
			Rules: []core.LibraryExclusion{{
				Action:    "show",
				MediaType: "movie",
				Match:     "all",
				Rules:     core.LibraryPredicateConditions(core.Filter{YearFrom: 2000}),
			}},
			Present: core.Presentations{Plex: &core.PlexPresentation{Base: plexBaseMovie}},
		}},
		artists:               []core.Title{artist1, artist2},
		rejectUndeclaredQuery: true,
	}

	f := newFixture(func(d *testServerDeps) {
		d.Catalog = cat
		d.State = st
	})
	defer f.srv.Close()

	expSvc := experience.NewDirectService(experience.ServiceDeps{
		Active: func(_ context.Context, id int64) (*core.User, error) {
			if id == ownerUser.ID {
				u := ownerUser
				return &u, nil
			}
			return nil, core.ErrUnauthorized
		},
		Catalog:         cat,
		State:           st,
		Playlists:       f.pls,
		PluginLibraries: libStore,
		Protocol:        "plex",
		PageExclusions: func(context.Context, *core.User, core.MediaType) experience.LibraryPageExclusions {
			return experience.LibraryPageExclusions{}
		},
		LibraryPresentation: func(p core.Presentations, mt core.MediaType) (core.MediaType, string, bool) {
			if p.Plex != nil && p.Plex.Base != "" {
				return mt, p.Plex.Base, true
			}
			if mt == core.TypeShow {
				return mt, plexBaseShow, true
			}
			return mt, plexBaseMovie, true
		},
		BuiltinNative: func(mt core.MediaType) string {
			if mt == core.TypeShow {
				return plexBaseShow
			}
			return plexBaseMovie
		},
		Visibility: experience.Visibility{
			SectionFor: func(mt core.MediaType) (int, bool) {
				if mt == core.TypeShow || mt == core.TypeSeason || mt == core.TypeEpisode {
					return 2, true
				}
				return 1, true
			},
		},
	})
	f.srv.bindHost(frontend.Host{Experience: expSvc})

	for _, tc := range []struct {
		path string
		want []string
	}{
		{path: "/library/sections/4/all?sort=titleSort:asc", want: []string{"Alpha", "Bravo", "Delta"}},
		{path: "/library/sections/4/all?sort=titleSort:desc", want: []string{"Delta", "Bravo", "Alpha"}},
		{path: "/library/sections/4/all?sort=titleSort:desc&genre=Sci-Fi", want: []string{"Delta", "Bravo"}},
		{path: "/library/sections/4/all?sort=titleSort:desc&genre=Sci-Fi&unwatched=1", want: []string{"Delta"}},
		{path: "/library/sections/5/all?sort=titleSort:desc&genre=Rock&unwatched=1", want: []string{"Artist One", "Artist Two"}},
	} {
		got := browseTitles(t, f, tc.path)
		if !slices.Equal(got, tc.want) {
			t.Fatalf("%s = %v, want %v", tc.path, got, tc.want)
		}
	}
}

type kindVideoHubExperience struct {
	frontend.Experience
	withRows bool
}

func (e *kindVideoHubExperience) PublishedViews(context.Context) ([]frontend.View, error) {
	return []frontend.View{
		{ID: "1", LibraryID: 1, Name: "Movies", Type: core.TypeMovie, Native: plexBaseMovie},
		{ID: "7", LibraryID: 7, Name: "Field Recordings", Type: core.TypeShow, Virtual: true, KindKey: "recordings", PluginID: "fieldrec", Native: plexBaseArtist},
		{ID: "8", LibraryID: 8, Name: "Snapshots", Type: core.TypeShow, Virtual: true, KindKey: "snapshots", PluginID: "fieldrec", Native: plexBasePhoto},
		{ID: "9", LibraryID: 9, Name: "Studio Clips", Type: core.TypeMovie, Virtual: true, KindKey: "clips", PluginID: "studio", Native: plexBaseMovie},
	}, nil
}

func kindClipItems() []core.Title {
	return []core.Title{
		{Key: core.PluginKey(901), Type: core.TypeMovie, Title: "Opening Cut", Year: 2025, KindLevel: "clip", LibraryID: 9},
		{Key: core.PluginKey(902), Type: core.TypeMovie, Title: "Behind The Lens", Year: 2025, KindLevel: "clip", LibraryID: 9},
	}
}

func (e *kindVideoHubExperience) LibraryItems(_ context.Context, _ *frontend.Viewer, _ frontend.Device, viewID string, _ frontend.ItemQuery) (core.Page, error) {
	if viewID == "9" {
		items := kindClipItems()
		return core.Page{Items: items, Total: len(items)}, nil
	}
	return core.Page{}, nil
}

type kindVideoHubRowsExperience struct {
	*kindVideoHubExperience
}

func (e *kindVideoHubRowsExperience) PluginLibraryRows(_ context.Context, _ *core.Viewer, _ core.Device, viewID, _ string) ([]frontend.Row, error) {
	if viewID != "9" {
		return nil, nil
	}
	return []frontend.Row{
		{ID: "studio.featured", Title: "Featured Clips", Items: kindClipItems()},
		{ID: "studio.recent", Title: "Recently Added Clips", Items: kindClipItems()[:1]},
	}, nil
}

func TestKindVideoHubsAndContinuation(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	directExp := experience.NewDirectService(experience.ServiceDeps{
		Catalog:   f.catalog,
		State:     f.state,
		Playlists: f.pls,
	})
	base := &kindVideoHubExperience{Experience: directExp, withRows: true}
	srv := newTestServer(testServerDeps{
		Catalog:    f.catalog,
		State:      f.state,
		Experience: &kindVideoHubRowsExperience{base},
	})

	// 1. /hubs/sections/9 renders the plugin video shelves.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/hubs/sections/9?count=12", nil)
	req.Header.Set("Accept", "application/json")
	req = req.WithContext(withRequest(req.Context(), &Request{User: &ownerUser, Format: model.JSON}))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("section", "9")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	srv.handleSectionHubs(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/hubs/sections/9 status = %d: %s", rec.Code, rec.Body.String())
	}
	var secOut struct {
		MediaContainer struct {
			Hub []struct {
				Title         string `json:"title"`
				HubIdentifier string `json:"hubIdentifier"`
				HubKey        string `json:"hubKey"`
				Metadata      []struct {
					Title string `json:"title"`
				} `json:"Metadata"`
			} `json:"Hub"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &secOut); err != nil {
		t.Fatalf("decode section hubs: %v", err)
	}
	if len(secOut.MediaContainer.Hub) != 2 {
		t.Fatalf("section hubs = %+v, want 2 plugin shelves", secOut.MediaContainer.Hub)
	}
	if secOut.MediaContainer.Hub[0].Title != "Featured Clips" || len(secOut.MediaContainer.Hub[0].Metadata) != 2 {
		t.Fatalf("first shelf = %+v", secOut.MediaContainer.Hub[0])
	}

	// 2. /hubs/items continuation resolves the plugin shelf by hubIdentifier.
	contID := secOut.MediaContainer.Hub[0].HubIdentifier
	contRec := httptest.NewRecorder()
	contReq := httptest.NewRequest(http.MethodGet, "/hubs/items?identifier="+contID+"&contentDirectoryID=9", nil)
	contReq.Header.Set("Accept", "application/json")
	contReq = contReq.WithContext(withRequest(contReq.Context(), &Request{User: &ownerUser, Format: model.JSON}))
	srv.handleHubItems(contRec, contReq)
	if contRec.Code != http.StatusOK || !strings.Contains(contRec.Body.String(), "Opening Cut") {
		t.Fatalf("/hubs/items(%s) = %d %s", contID, contRec.Code, contRec.Body.String())
	}

	// 3. /hubs?contentDirectoryID=1,9 includes the plugin video library's shelf on Home.
	homeRec := httptest.NewRecorder()
	homeReq := httptest.NewRequest(http.MethodGet, "/hubs?contentDirectoryID=1,9&count=12", nil)
	homeReq.Header.Set("Accept", "application/json")
	homeReq = homeReq.WithContext(withRequest(homeReq.Context(), &Request{User: &ownerUser, Format: model.JSON}))
	srv.handleHubs(homeRec, homeReq)
	if homeRec.Code != http.StatusOK || !strings.Contains(homeRec.Body.String(), "Featured Clips") {
		t.Fatalf("/hubs?contentDirectoryID=1,9 = %d %s", homeRec.Code, homeRec.Body.String())
	}

}

func TestKindMediaProvidersTypesAndPivots(t *testing.T) {
	srv := newTestServer(testServerDeps{Experience: &kindVideoHubExperience{Experience: frontend.NoopExperience()}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/media/providers", nil)
	req.Header.Set("Accept", "application/json")
	req = req.WithContext(withRequest(req.Context(), &Request{User: &ownerUser, Format: model.JSON}))
	srv.handleProviders(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/media/providers status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		MediaContainer struct {
			MediaProvider []struct {
				Types   string `json:"types"`
				Feature []struct {
					Type      string `json:"type"`
					Directory []struct {
						ID    string `json:"id"`
						Title string `json:"title"`
						Type  string `json:"type"`
						Pivot []struct {
							ID   string `json:"id"`
							Key  string `json:"key"`
							Type string `json:"type"`
						} `json:"Pivot"`
					} `json:"Directory"`
				} `json:"Feature"`
			} `json:"MediaProvider"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode /media/providers: %v", err)
	}
	if len(out.MediaContainer.MediaProvider) == 0 {
		t.Fatalf("no MediaProvider returned: %s", rec.Body.String())
	}
	mp := out.MediaContainer.MediaProvider[0]
	if mp.Types != "video,audio,photo" {
		t.Fatalf("MediaProvider.Types = %q, want video,audio,photo", mp.Types)
	}

	var musicPlaylistsKey, musicLibraryKey, photoPlaylistsKey, photoLibraryKey string
	for _, f := range mp.Feature {
		for _, d := range f.Directory {
			switch d.ID {
			case "7":
				if d.Type != plexBaseArtist {
					t.Fatalf("music directory type = %q, want %s", d.Type, plexBaseArtist)
				}
				for _, p := range d.Pivot {
					if p.ID == "library" {
						musicLibraryKey = p.Key
					}
					if p.ID == "playlists" {
						musicPlaylistsKey = p.Key
					}
				}
			case "8":
				if d.Type != plexBasePhoto {
					t.Fatalf("photo directory type = %q, want %s", d.Type, plexBasePhoto)
				}
				for _, p := range d.Pivot {
					if p.ID == "library" {
						photoLibraryKey = p.Key
					}
					if p.ID == "playlists" {
						photoPlaylistsKey = p.Key
					}
				}
			}
		}
	}
	if !strings.Contains(musicLibraryKey, "type=8") || !strings.Contains(musicPlaylistsKey, "playlistType=audio") {
		t.Fatalf("music pivots: libraryKey=%q playlistsKey=%q", musicLibraryKey, musicPlaylistsKey)
	}
	if !strings.Contains(photoLibraryKey, "type=13") || !strings.Contains(photoPlaylistsKey, "playlistType=photo") {
		t.Fatalf("photo pivots: libraryKey=%q playlistsKey=%q", photoLibraryKey, photoPlaylistsKey)
	}
}

func TestUntitledSectionForPrefersNonKindSection(t *testing.T) {
	srv := newTestServer(testServerDeps{Experience: &kindVideoHubExperience{Experience: frontend.NoopExperience()}})
	sec, ok := srv.untitledSectionFor(core.TypeMovie)
	if !ok {
		t.Fatal("expected movie section")
	}
	if sec.ID != 1 || sec.KindKey != "" {
		t.Fatalf("untitledSectionFor(movie) = %+v, want builtin section 1", sec)
	}
}

func TestHomeStampsInvalidateOnLibrariesAndKindSync(t *testing.T) {
	fresh := frontend.PageFreshness{Layout: 1, Config: 1, Catalog: 1, Libraries: 1, KindSync: 1}
	srv := newTestServer(testServerDeps{Experience: frontend.NoopExperience()})
	srv.bindHost(frontend.Host{
		Experience:    frontend.NoopExperience(),
		PageFreshness: func() frontend.PageFreshness { return fresh },
	})
	pr := &Request{User: &ownerUser}
	rev := frontend.HomeRevision{Viewer: "v1"}

	h0, _ := srv.homeStamps(pr, rev)
	fresh.Libraries = 2
	hLib, _ := srv.homeStamps(pr, rev)
	if hLib == h0 {
		t.Fatalf("homeStamps hard stamp did not change when Libraries bumped: %+v", h0)
	}
	fresh.KindSync = 2
	hSync, _ := srv.homeStamps(pr, rev)
	if hSync.matches(hLib, true) || !hSync.matches(hLib, false) {
		t.Fatalf("kind sync changed the wrong screen dependencies: %+v versus %+v", hLib, hSync)
	}
}
