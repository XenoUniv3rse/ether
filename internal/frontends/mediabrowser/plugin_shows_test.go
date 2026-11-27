package mediabrowser_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/extend"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
	"github.com/braedonsaunders/ether/internal/libraries"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/adapters"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

type noopPluginRuntime struct{}

func (noopPluginRuntime) Call(context.Context, plugins.Request) (plugins.Response, error) {
	return plugins.Response{Status: 200, Body: []byte(`{"items":[]}`)}, nil
}

func (noopPluginRuntime) Close() error { return nil }

type threeLevelPluginShowFixture struct {
	libID    core.LibraryID
	league1  core.RatingKey
	league2  core.RatingKey
	season25 core.RatingKey
	season26 core.RatingKey
	otherS26 core.RatingKey
	exp      *experience.Service
	viewer   *frontend.Viewer
	ident    frontendtest.Identity
	jellyfin frontend.Instance
	emby     frontend.Instance
}

func newThreeLevelPluginShowFixture(t *testing.T) *threeLevelPluginShowFixture {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	db := store.OpenTest(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := config.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	libs, err := libraries.New(ctx, db, libraries.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	st := state.New(db, state.Options{Logger: log})

	mgr, err := plugins.New(plugins.Deps{
		DB:       db,
		Sealer:   cfg,
		Registry: extend.New(),
		Tools:    tools.NewCatalogue(),
		Logger:   log,
		IsLeader: func() bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr.RegisterRuntime("hosted", func(plugins.Installed) (plugins.Runtime, error) {
		return noopPluginRuntime{}, nil
	})

	items, err := pluginitems.New(ctx, db, pluginitems.Options{
		Caller:   mgr,
		Health:   mgr,
		Logger:   log,
		IsLeader: func() bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(items.Close)

	blobDir := filepath.Join(dataDir, "blobcache")
	blobs, err := blobcache.Open(blobDir, blobcache.Options{Logger: log, SweepPeriod: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })

	kc := adapters.AttachKindCatalog(ctx, adapters.KindCatalogDeps{
		Manager:   mgr,
		Libraries: libs,
		Items:     items,
		Blobs:     blobs,
		BlobDir:   blobDir,
		Logger:    log,
	})
	mgr.SetKindBackends(plugins.KindBackends{
		KindUpsert:    items.KindUpsert,
		KindDelete:    items.KindDelete,
		KindItems:     items.KindItems,
		KindItemsByID: items.KindItemsByID,
	})
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}

	const (
		pluginID = "community.test.sports"
		kindKey  = "motorsport"
	)
	manifest := map[string]any{
		"id":      pluginID,
		"name":    "Sports Test",
		"version": "1.0.0",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "content",
			"summary":      "Three-level motorsport TV shows fixture",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"catalog_provider"},
			"contributes": map[string]any{
				"mediaKinds": []map[string]any{{
					"key":   kindKey,
					"label": "Motorsport",
					"library": map[string]any{
						"name": "Motorsport",
					},
					"levels": []map[string]any{
						{"key": "league", "label": "League", "container": true},
						{"key": "season", "label": "Season", "container": true},
						{"key": "event", "label": "Event", "playable": true},
					},
					"present": map[string]any{
						"plex":         map[string]any{"base": "show"},
						"mediaBrowser": map[string]any{"collectionType": "tvshows", "itemTypes": map[string]string{"league": "Series", "season": "Season", "event": "Episode"}},
					},
					"sync": map[string]any{"mode": "push"},
				}},
			},
		},
	}
	rawMan, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := mgr.PreviewInstall(ctx, plugins.Source{
		Kind:     "studio",
		Manifest: rawMan,
		Files:    map[string][]byte{"dist/plugin.js": []byte("// test")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Install(ctx, preview, plugins.InstallOptions{AcceptAddedPermissions: true}); err != nil {
		t.Fatal(err)
	}

	// Upsert a 3-level hierarchy (Series -> Season indexed by year -> Episode)
	// without setting ChildCount or LeafCount on any container item.
	if _, err := items.KindUpsert(ctx, pluginID, kindKey, []core.KindItem{
		{ID: "f1", Level: "league", Title: "Formula One", Year: 2025},
		{ID: "f1-2025", Level: "season", ParentID: "f1", Index: 2025, Title: "2025 Season", Year: 2025},
		{ID: "f1-2025-r1", Level: "event", ParentID: "f1-2025", Index: 1, Title: "Australian Grand Prix", Year: 2025, Playable: true},
		{ID: "f1-2025-r2", Level: "event", ParentID: "f1-2025", Index: 2, Title: "Chinese Grand Prix", Year: 2025, Playable: true},
		{ID: "f1-2026", Level: "season", ParentID: "f1", Index: 2026, Title: "2026 Season", Year: 2026},
		{ID: "f1-2026-r1", Level: "event", ParentID: "f1-2026", Index: 1, Title: "Bahrain Grand Prix", Year: 2026, Playable: true},
		{ID: "f1-2026-r2", Level: "event", ParentID: "f1-2026", Index: 2, Title: "Saudi Arabian Grand Prix", Year: 2026, Playable: true},
		{ID: "f1-2026-r3", Level: "event", ParentID: "f1-2026", Index: 3, Title: "Japanese Grand Prix", Year: 2026, Playable: true},
		{ID: "wec", Level: "league", Title: "World Endurance", Year: 2026},
		{ID: "wec-2026", Level: "season", ParentID: "wec", Index: 2026, Title: "2026 WEC", Year: 2026},
		{ID: "wec-2026-r1", Level: "event", ParentID: "wec-2026", Index: 1, Title: "24 Hours of Le Mans", Year: 2026, Playable: true},
	}); err != nil {
		t.Fatal(err)
	}

	mustKey := func(extID string) core.RatingKey {
		t.Helper()
		it, _, err := items.GetByExternal(ctx, pluginID, kindKey, extID)
		if err != nil {
			t.Fatalf("lookup %s: %v", extID, err)
		}
		return it.Key
	}
	kindRec, err := items.GetKind(ctx, pluginID, kindKey)
	if err != nil {
		t.Fatal(err)
	}

	owner := &core.User{ID: 1, Username: "owner", IsOwner: true}
	builtins := []experience.Library{
		{ID: int(core.LibraryShows), Name: "TV Shows", Type: core.TypeShow, Virtual: true},
	}
	vis := experience.BindVisibility(nil, func(context.Context, core.RatingKey) (*core.Title, error) {
		return nil, core.ErrNotFound
	}, nil, nil, builtins)

	exp, err := experience.NewService(experience.ServiceDeps{
		Active: func(_ context.Context, id int64) (*core.User, error) {
			if id == owner.ID {
				cp := *owner
				return &cp, nil
			}
			return nil, core.ErrNotFound
		},
		Libraries:           builtins,
		Visibility:          vis,
		State:               st,
		Rows:                &experience.RowsService{},
		Protocol:            "jellyfin",
		LibraryPresentation: mediabrowser.ProjectLibrary,
		PluginLibraries:     kc,
		Get:                 func(context.Context, *core.User, core.RatingKey) (*core.Title, error) { return nil, core.ErrNotFound },
		CachedGet:           func(context.Context, core.RatingKey) (*core.Title, error) { return nil, core.ErrNotFound },
		Children:            func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) { return nil, nil },
		Browse:              func(context.Context, core.BrowseQuery) (core.Page, error) { return core.Page{}, nil },
		Search: experience.Search{
			Catalog: func(context.Context, string, []core.MediaType, int) ([]core.Title, error) { return nil, nil },
		},
		IsFavorite:          func(context.Context, *core.User, core.RatingKey) (bool, error) { return false, nil },
		IsFavoriteMany:      func(context.Context, *core.User, []core.RatingKey) (map[core.RatingKey]bool, error) { return nil, nil },
		IsNotInterested:     func(context.Context, *core.User, core.RatingKey) (bool, error) { return false, nil },
		IsNotInterestedMany: func(context.Context, *core.User, []core.RatingKey) (map[core.RatingKey]bool, error) { return nil, nil },
		IsWatchlisted:       func(context.Context, *core.User, core.RatingKey) (bool, error) { return false, nil },
		IsWatchlistedMany:   func(context.Context, int64, []core.RatingKey) (map[core.RatingKey]bool, error) { return nil, nil },
		Enrich: func(_ context.Context, _ *core.User, _ frontend.ItemRequest, title *core.Title) (frontend.ItemDetail, error) {
			return frontend.ItemDetail{Title: *title}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	viewer := &frontend.Viewer{ID: owner.ID, Username: owner.Username, IsOwner: true}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	host := frontend.Host{Identity: ident, Experience: exp}
	jf, err := (mediabrowser.Jellyfin{MachineID: "test-jf"}).Mount(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jf.Close(ctx) })
	emby, err := (mediabrowser.Emby{MachineID: "test-emby"}).Mount(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = emby.Close(ctx) })

	return &threeLevelPluginShowFixture{
		libID:    kindRec.LibraryID,
		league1:  mustKey("f1"),
		league2:  mustKey("wec"),
		season25: mustKey("f1-2025"),
		season26: mustKey("f1-2026"),
		otherS26: mustKey("wec-2026"),
		exp:      exp,
		viewer:   viewer,
		ident:    ident,
		jellyfin: jf,
		emby:     emby,
	}
}

func TestThreeLevelPluginShowsSeasonsEpisodesAndCounts(t *testing.T) {
	f := newThreeLevelPluginShowFixture(t)
	uid, err := ids.GUID(ids.User, f.viewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	showID := ids.RatingKeyGUID(f.league1)
	s25ID := ids.RatingKeyGUID(f.season25)
	s26ID := ids.RatingKeyGUID(f.season26)
	otherS26ID := ids.RatingKeyGUID(f.otherS26)

	for _, surface := range []struct {
		name string
		inst frontend.Instance
	}{
		{name: "jellyfin", inst: f.jellyfin},
		{name: "emby", inst: f.emby},
	} {
		t.Run(surface.name, func(t *testing.T) {
			getJSON := func(path string) map[string]any {
				t.Helper()
				r := httptest.NewRequest(http.MethodGet, path, nil)
				r.Header.Set("X-Emby-Token", "grant")
				w := httptest.NewRecorder()
				surface.inst.Handler().ServeHTTP(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("%s: HTTP %d: %s", path, w.Code, w.Body.String())
				}
				var out map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
					t.Fatalf("%s: json: %v", path, err)
				}
				return out
			}

			// Series detail carries ChildCount (seasons) and RecursiveItemCount (episodes).
			series := getJSON("/Users/" + uid + "/Items/" + showID)
			if series["Type"] != "Series" || series["ChildCount"] != float64(2) || series["RecursiveItemCount"] != float64(5) {
				t.Fatalf("series counts: Type=%v ChildCount=%v RecursiveItemCount=%v", series["Type"], series["ChildCount"], series["RecursiveItemCount"])
			}

			// /Shows/{id}/Seasons lists year-indexed seasons with ChildCount and RecursiveItemCount.
			seasonsResp := getJSON("/Shows/" + showID + "/Seasons")
			seasonItems, _ := seasonsResp["Items"].([]any)
			if len(seasonItems) != 2 || seasonsResp["TotalRecordCount"] != float64(2) {
				t.Fatalf("seasons response: total=%v items=%d", seasonsResp["TotalRecordCount"], len(seasonItems))
			}
			byID := map[string]map[string]any{}
			for _, raw := range seasonItems {
				it := raw.(map[string]any)
				byID[it["Id"].(string)] = it
			}
			s25 := byID[s25ID]
			s26 := byID[s26ID]
			if s25 == nil || s26 == nil {
				t.Fatalf("missing expected season IDs in %+v", byID)
			}
			if s25["Type"] != "Season" || s25["IndexNumber"] != float64(2025) || s25["ChildCount"] != float64(2) || s25["RecursiveItemCount"] != float64(2) || s25["SeriesId"] != showID {
				t.Fatalf("2025 season mismatch: %+v", s25)
			}
			if s26["Type"] != "Season" || s26["IndexNumber"] != float64(2026) || s26["ChildCount"] != float64(3) || s26["RecursiveItemCount"] != float64(3) || s26["SeriesId"] != showID {
				t.Fatalf("2026 season mismatch: %+v", s26)
			}

			// /Shows/{id}/Episodes?seasonId= returns only that season's episodes.
			epsByID := getJSON("/Shows/" + showID + "/Episodes?seasonId=" + s26ID)
			items26, _ := epsByID["Items"].([]any)
			if len(items26) != 3 || epsByID["TotalRecordCount"] != float64(3) {
				t.Fatalf("seasonId=2026: total=%v len=%d", epsByID["TotalRecordCount"], len(items26))
			}
			for _, raw := range items26 {
				ep := raw.(map[string]any)
				if ep["Type"] != "Episode" || ep["SeriesId"] != showID || ep["SeasonId"] != s26ID || ep["ParentIndexNumber"] != float64(2026) || ep["SeasonName"] != "2026 Season" {
					t.Fatalf("episode under seasonId=2026 mismatch: %+v", ep)
				}
			}

			// /Shows/{id}/Episodes?season=2025 resolves year-indexed season by number.
			epsByYear := getJSON("/Shows/" + showID + "/Episodes?season=2025")
			items25, _ := epsByYear["Items"].([]any)
			if len(items25) != 2 || epsByYear["TotalRecordCount"] != float64(2) {
				t.Fatalf("season=2025: total=%v len=%d", epsByYear["TotalRecordCount"], len(items25))
			}
			for _, raw := range items25 {
				ep := raw.(map[string]any)
				if ep["SeasonId"] != s25ID || ep["ParentIndexNumber"] != float64(2025) || ep["SeasonName"] != "2025 Season" {
					t.Fatalf("episode under season=2025 mismatch: %+v", ep)
				}
			}

			// Unknown season number and another show's seasonId both return empty.
			missing := getJSON("/Shows/" + showID + "/Episodes?season=1999")
			if missing["TotalRecordCount"] != float64(0) || len(missing["Items"].([]any)) != 0 {
				t.Fatalf("season=1999 expected empty, got %+v", missing)
			}
			crossShow := getJSON("/Shows/" + showID + "/Episodes?seasonId=" + otherS26ID)
			if crossShow["TotalRecordCount"] != float64(0) || len(crossShow["Items"].([]any)) != 0 {
				t.Fatalf("cross-show seasonId leaked items: %+v", crossShow)
			}

			// /Shows/{id}/Episodes without a season filter lists all episodes across seasons.
			allEps := getJSON("/Shows/" + showID + "/Episodes")
			if allEps["TotalRecordCount"] != float64(5) || len(allEps["Items"].([]any)) != 5 {
				t.Fatalf("all episodes: total=%v len=%d", allEps["TotalRecordCount"], len(allEps["Items"].([]any)))
			}
		})
	}
}
