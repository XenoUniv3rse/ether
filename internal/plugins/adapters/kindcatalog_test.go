package adapters_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/extend"
	_ "github.com/braedonsaunders/ether/internal/favorites"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/libraries"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	_ "github.com/braedonsaunders/ether/internal/notinterested"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/adapters"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	_ "github.com/braedonsaunders/ether/internal/watchlist"
)

type kindTestHarness struct {
	db       *sql.DB
	cfg      *config.Store
	libs     *libraries.Service
	items    *pluginitems.Service
	blobs    *blobcache.Cache
	blobDir  string
	state    *state.State
	mgr      *plugins.Manager
	catalog  *adapters.KindCatalog
	resolver *adapters.KindResolver
	rt       *listChannelFakeRuntime
	users    map[int64]*core.User
	titles   map[core.RatingKey]core.Title
}

type hostRewriteTransport struct {
	target string
	base   http.RoundTripper
}

func (t hostRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(t.target)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme = u.Scheme
	clone.URL.Host = u.Host
	rt := t.base
	if rt == nil {
		rt = http.DefaultTransport
	}
	return rt.RoundTrip(clone)
}

func newKindTestHarness(t *testing.T, liveTimeout time.Duration, httpClient *http.Client) *kindTestHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	dataDir := t.TempDir()
	cfg, err := config.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	db := store.OpenTest(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	libs, err := libraries.New(ctx, db, libraries.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	st := state.New(db, state.Options{Logger: log})
	rt := &listChannelFakeRuntime{routes: map[string]func(plugins.Request) (plugins.Response, error){}}

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
	mgr.RegisterRuntime("hosted", func(_ plugins.Installed) (plugins.Runtime, error) {
		return rt, nil
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

	titles := map[core.RatingKey]core.Title{}
	kc := adapters.AttachKindCatalog(ctx, adapters.KindCatalogDeps{
		TitleInLibrary: func(ctx context.Context, lib core.Library, key core.RatingKey) (bool, error) {
			member, err := libs.SourceMember(ctx, lib, key)
			if err != nil || !member {
				return member, err
			}
			title, ok := titles[key]
			if !ok {
				return false, nil
			}
			policy, err := libraryfilter.CompilePolicy(lib.Rules, lib.VisibilityDefault)
			if err != nil {
				return false, err
			}
			return !policy.Hidden(&title), nil
		},
		Manager:           mgr,
		Libraries:         libs,
		Items:             items,
		Blobs:             blobs,
		BlobDir:           blobDir,
		Logger:            log,
		LiveSearchTimeout: liveTimeout,
	})

	users := map[int64]*core.User{
		1: {ID: 1, Username: "owner", IsOwner: true},
	}
	kr := adapters.NewKindResolver(adapters.KindResolverDeps{
		Manager:   mgr,
		Libraries: libs,
		Items:     items,
		Blobs:     blobs,
		Catalog:   kc,
		HTTP:      httpClient,
		Logger:    log,
	})

	cachePut, cacheOpen, cacheDelete := blobs.HostBackends(kc.QuotaFor)
	mgr.SetKindBackends(plugins.KindBackends{
		KindUpsert:     items.KindUpsert,
		KindDelete:     items.KindDelete,
		KindItems:      items.KindItems,
		KindItemsByID:  items.KindItemsByID,
		ResolveSources: kr.ResolveSources,
		CachePut:       cachePut,
		CacheOpen:      cacheOpen,
		CacheDelete:    cacheDelete,
	})
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}

	return &kindTestHarness{
		db:       db,
		cfg:      cfg,
		libs:     libs,
		items:    items,
		blobs:    blobs,
		blobDir:  blobDir,
		state:    st,
		mgr:      mgr,
		catalog:  kc,
		resolver: kr,
		rt:       rt,
		users:    users,
		titles:   titles,
	}
}

func (h *kindTestHarness) experienceFor(t *testing.T, protocol core.Protocol) *experience.Service {
	t.Helper()
	get := func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		if title, ok := h.titles[k]; ok {
			cp := title
			return &cp, nil
		}
		return nil, core.ErrNotFound
	}
	builtins := []experience.Library{
		{ID: int(core.LibraryMovies), Name: "Movies", Type: core.TypeMovie, Virtual: true},
		{ID: int(core.LibraryShows), Name: "TV Shows", Type: core.TypeShow, Virtual: true},
	}
	vis := experience.BindVisibility(nil, func(ctx context.Context, k core.RatingKey) (*core.Title, error) {
		return get(ctx, nil, k)
	}, nil, nil, builtins)

	svc, err := experience.NewService(experience.ServiceDeps{
		Active: func(_ context.Context, id int64) (*core.User, error) {
			if u, ok := h.users[id]; ok {
				cp := *u
				return &cp, nil
			}
			return nil, core.ErrNotFound
		},
		Libraries:  builtins,
		Visibility: vis,
		State:      h.state,
		Rows:       &experience.RowsService{},
		Protocol:   protocol,
		LibraryPresentation: func(p core.Presentations, mt core.MediaType) (core.MediaType, string, bool) {
			if p.AdapterOnly {
				return "", "", false
			}
			if mt == "" {
				mt = core.TypeMovie
			}
			if protocol == "plex" {
				return plex.ProjectLibrary(p, mt)
			}
			return mediabrowser.ProjectLibrary(p, mt)
		},
		PluginLibraries: h.catalog,
		Get:             get,
		CachedGet: func(ctx context.Context, k core.RatingKey) (*core.Title, error) {
			return get(ctx, nil, k)
		},
		Children: func(_ context.Context, _ *core.User, _ core.RatingKey) ([]core.Title, error) {
			return nil, nil
		},
		Browse: func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
			var out []core.Title
			for _, title := range h.titles {
				if q.Filter.Type != "" && title.Type != q.Filter.Type {
					continue
				}
				out = append(out, title)
			}
			return core.Page{Items: out, Total: len(out)}, nil
		},
		Search: experience.Search{
			Catalog: func(_ context.Context, query string, _ []core.MediaType, _ int) ([]core.Title, error) {
				var out []core.Title
				for _, title := range h.titles {
					if strings.Contains(strings.ToLower(title.Title), strings.ToLower(query)) {
						out = append(out, title)
					}
				}
				return out, nil
			},
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
	return svc
}

func installStudioManifest(t *testing.T, mgr *plugins.Manager, man map[string]any, extraFiles map[string][]byte) plugins.Record {
	t.Helper()
	rawMan, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"dist/plugin.js": []byte("// test plugin"),
	}
	for k, v := range extraFiles {
		files[k] = v
	}
	preview, err := mgr.PreviewInstall(context.Background(), plugins.Source{
		Kind:     "studio",
		Manifest: rawMan,
		Files:    files,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := mgr.Install(context.Background(), preview, plugins.InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func recordingsKindDecl(withLibrary bool) map[string]any {
	decl := map[string]any{
		"key":   "recordings",
		"label": "Field recordings",
		"levels": []map[string]any{
			{"key": "album", "label": "Album", "container": true},
			{"key": "track", "label": "Track", "playable": true},
		},
		"fields": []map[string]any{
			{"key": "genre", "type": "string", "label": "Genre", "indexed": true},
			{"key": "durationSec", "type": "duration", "label": "Duration"},
			{"key": "contentRating", "type": "string", "label": "Content rating"},
		},
		"browse": map[string]any{
			"sorts":      []string{"title", "added"},
			"facets":     []string{"genre"},
			"search":     true,
			"liveSearch": withLibrary,
		},
	}
	if withLibrary {
		decl["library"] = map[string]any{"name": "$t:lib_name"}
		decl["present"] = map[string]any{
			"plex": map[string]any{"base": "show"},
			"mediaBrowser": map[string]any{
				"collectionType": "music",
				"itemTypes":      map[string]any{"album": "MusicAlbum", "track": "Audio"},
			},
		}
		decl["sync"] = map[string]any{"mode": "pull", "refresh": "1h"}
		decl["cache"] = map[string]any{"maxBytes": 16 << 20, "ttl": "24h"}
	}
	return decl
}

func TestNonKindAndLibrarylessKindCreateNoLibrary(t *testing.T) {
	h := newKindTestHarness(t, 0, nil)
	ctx := context.Background()

	genBefore := h.libs.Generation()
	initial, err := h.libs.List(ctx)
	if err != nil || len(initial) != 2 {
		t.Fatalf("expected 2 builtin libraries, got %+v %v", initial, err)
	}

	// 1. Installing a non-kind plugin must not create a library or touch libraries.
	installStudioManifest(t, h.mgr, map[string]any{
		"id":      "community.example.hello",
		"version": "1.0.0",
		"name":    "Hello",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "extension",
			"summary":      "Simple greeting page",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"ui"},
			"contributes": map[string]any{
				"pages": []map[string]any{{"key": "main", "label": "Hello", "route": "/ether/view/main"}},
			},
		},
	}, nil)

	if h.libs.Generation() != genBefore {
		t.Fatalf("non-kind plugin mutated libraries generation: got %d, want %d", h.libs.Generation(), genBefore)
	}
	afterHello, err := h.libs.List(ctx)
	if err != nil || len(afterHello) != 2 {
		t.Fatalf("non-kind plugin altered libraries: %+v %v", afterHello, err)
	}
	if entries, err := os.ReadDir(h.blobDir); err != nil || len(entries) != 0 {
		t.Fatalf("expected 0 blobcache dirs, got %v %v", entries, err)
	}

	// 2. Installing a content plugin whose mediaKind has NO library block creates no library.
	const kindPluginID = "community.example.recordings"
	installStudioManifest(t, h.mgr, map[string]any{
		"id":      kindPluginID,
		"version": "1.0.0",
		"name":    "Field Recordings",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "content",
			"summary":      "Resolver-only field recordings",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"kind_resolver"},
			"strings":      map[string]any{"en": "i18n/en.json"},
			"contributes": map[string]any{
				"mediaKinds": []map[string]any{recordingsKindDecl(false)},
			},
		},
	}, map[string][]byte{
		"i18n/en.json": []byte(`{"lib_name":"Field Recordings"}`),
	})

	if h.libs.Generation() != genBefore {
		t.Fatalf("kind without library block mutated libraries generation: got %d, want %d", h.libs.Generation(), genBefore)
	}
	if kinds, err := h.items.ListKinds(ctx, kindPluginID); err != nil || len(kinds) != 0 {
		t.Fatalf("kind without library must not register a library-backed kind row: %+v %v", kinds, err)
	}

	// 3. Updating to v1.1.0 to add the library declaration creates the library and syncs items.
	h.rt.routes["/ether/v1/kinds/recordings/browse"] = func(req plugins.Request) (plugins.Response, error) {
		var b core.KindBrowseRequest
		_ = json.Unmarshal(req.Body, &b)
		if b.ParentID == "" {
			return plugins.Response{
				Status: 200,
				Body:   json.RawMessage(`{"items":[{"id":"alb-1","level":"album","title":"Harbour","year":2024,"childCount":1,"leafCount":1,"fields":{"genre":"ambient"}}]}`),
			}, nil
		}
		return plugins.Response{
			Status: 200,
			Body:   json.RawMessage(`{"items":[{"id":"trk-1","level":"track","parentId":"alb-1","index":1,"title":"Fog Horn","playable":true,"fields":{"genre":"ambient","durationSec":84}}]}`),
		}, nil
	}

	installStudioManifest(t, h.mgr, map[string]any{
		"id":      kindPluginID,
		"version": "1.1.0",
		"name":    "Field Recordings",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "content",
			"summary":      "Field recordings with library",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"catalog_provider", "kind_resolver"},
			"strings":      map[string]any{"en": "i18n/en.json"},
			"contributes": map[string]any{
				"mediaKinds": []map[string]any{recordingsKindDecl(true)},
			},
		},
	}, map[string][]byte{
		"i18n/en.json": []byte(`{"lib_name":"Field Recordings"}`),
	})

	var pluginLib core.Library
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		all, _ := h.libs.List(ctx)
		for _, lib := range all {
			if lib.Source == core.LibraryPlugin && lib.PluginID == kindPluginID {
				pluginLib = lib
			}
		}
		if pluginLib.ID != 0 {
			if cnt, err := h.catalog.Count(ctx, pluginLib); err == nil && cnt == 1 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pluginLib.ID == 0 || pluginLib.Name != "Field Recordings" {
		t.Fatalf("expected localized Field Recordings plugin library, got %+v", pluginLib)
	}

	exp := h.experienceFor(t, "jellyfin")
	views, err := exp.Views(ctx, h.users[1])
	if err != nil || len(views) != 3 {
		t.Fatalf("expected 3 views after adding library decl, got %+v %v", views, err)
	}

	// Owner deletion survives plugin synchronization without purging its items.
	if err := h.libs.Delete(ctx, pluginLib.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.SetEnabled(ctx, kindPluginID, false); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.SetEnabled(ctx, kindPluginID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.libs.Get(ctx, pluginLib.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("sync resurrected deleted library: %v", err)
	}
	restored, err := h.libs.RestorePluginLibrary(ctx, kindPluginID, pluginLib.KindKey, libraries.PluginLibraryDecl{Name: "Default"})
	if err != nil || restored.ID != pluginLib.ID || restored.Name != pluginLib.Name {
		t.Fatalf("restore: %+v %v", restored, err)
	}
	if cnt, err := h.catalog.Count(ctx, restored); err != nil || cnt != 1 {
		t.Fatalf("deletion lost plugin items: %d %v", cnt, err)
	}

	// 4. Updating to v1.2.0 removing the library block purges the library and items.
	installStudioManifest(t, h.mgr, map[string]any{
		"id":      kindPluginID,
		"version": "1.2.0",
		"name":    "Field Recordings",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "content",
			"summary":      "Resolver-only field recordings again",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"kind_resolver"},
			"strings":      map[string]any{"en": "i18n/en.json"},
			"contributes": map[string]any{
				"mediaKinds": []map[string]any{recordingsKindDecl(false)},
			},
		},
	}, map[string][]byte{
		"i18n/en.json": []byte(`{"lib_name":"Field Recordings"}`),
	})

	afterDrop, err := h.libs.List(ctx)
	if err != nil || len(afterDrop) != 2 {
		t.Fatalf("expected plugin library removed after dropping library decl, got %+v %v", afterDrop, err)
	}
	if kinds, err := h.items.ListKinds(ctx, kindPluginID); err != nil || len(kinds) != 0 {
		t.Fatalf("expected plugin kinds purged after dropping library decl, got %+v %v", kinds, err)
	}
	viewsAfter, err := exp.Views(ctx, h.users[1])
	if err != nil || len(viewsAfter) != 2 {
		t.Fatalf("expected 2 views after dropping library decl, got %+v %v", viewsAfter, err)
	}
}

func TestSetupGatingOfflineBrowseRolesAndRemovePurge(t *testing.T) {
	track1Bytes := []byte("gulls audio payload\n")
	track2Bytes := []byte("fog horn audio payload\n")
	var gotAuthHeader atomic.Value
	mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader.Store(r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/trk-1.flac":
			http.ServeContent(w, r, "trk-1.flac", time.Time{}, bytes.NewReader(track1Bytes))
		case "/trk-2.flac":
			http.ServeContent(w, r, "trk-2.flac", time.Time{}, bytes.NewReader(track2Bytes))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mediaSrv.Close()

	hc := &http.Client{Transport: hostRewriteTransport{target: mediaSrv.URL}}
	h := newKindTestHarness(t, 0, hc)
	ctx := context.Background()

	var syncCalls atomic.Int32
	var resolveCalls atomic.Int32
	var pluginBroken atomic.Bool
	var leakRawSecret atomic.Bool

	h.rt.callContext = func(_ context.Context, req plugins.Request) (plugins.Response, error) {
		if pluginBroken.Load() {
			return plugins.Response{}, errors.New("plugin runtime crashed")
		}
		switch req.Path {
		case "/ether/health":
			return plugins.Response{Status: 200, Body: json.RawMessage(`{"status":"ok"}`)}, nil
		case "/ether/configure":
			return plugins.Response{Status: 200, Body: json.RawMessage(`{"errors":{}}`)}, nil
		case "/ether/v1/setup/connect/test":
			return plugins.Response{Status: 200, Body: json.RawMessage(`{"ok":true,"detail":"Connected"}`)}, nil
		case "/ether/v1/kinds/recordings/browse":
			syncCalls.Add(1)
			var b core.KindBrowseRequest
			_ = json.Unmarshal(req.Body, &b)
			if b.ParentID == "" {
				return plugins.Response{
					Status: 200,
					Body:   json.RawMessage(`{"items":[{"id":"alb-1","level":"album","title":"Harbour","year":2024,"childCount":2,"leafCount":2,"fields":{"genre":"ambient","contentRating":"PG-13"},"art":[{"role":"poster","url":"https://media.example/poster.jpg"}]}]}`),
				}, nil
			}
			return plugins.Response{
				Status: 200,
				Body: json.RawMessage(`{"items":[` +
					`{"id":"trk-1","level":"track","parentId":"alb-1","index":1,"title":"Gulls","playable":true,"fields":{"genre":"ambient","durationSec":60,"contentRating":"PG"}},` +
					`{"id":"trk-2","level":"track","parentId":"alb-1","index":2,"title":"Fog Horn","playable":true,"fields":{"genre":"ambient","durationSec":84,"contentRating":"R"}}` +
					`]}`),
			}, nil
		case "/ether/v1/kinds/recordings/resolve":
			resolveCalls.Add(1)
			var body struct {
				Item core.KindItem `json:"item"`
			}
			_ = json.Unmarshal(req.Body, &body)
			if leakRawSecret.Load() {
				return plugins.Response{
					Status: 200,
					Body:   json.RawMessage(`{"sources":[{"id":"leaked","pattern":"range","mime":"audio/flac","url":"https://media.example/trk-1.flac","headers":{"Authorization":"Bearer top-secret-token"}}]}`),
				}, nil
			}
			switch body.Item.ID {
			case "trk-1":
				return plugins.Response{
					Status: 200,
					Body:   json.RawMessage(`{"sources":[{"id":"s1","pattern":"range","mime":"audio/flac","url":"https://media.example/trk-1.flac","headers":{"Authorization":"Bearer {secret:api_token}"}}]}`),
				}, nil
			case "trk-2":
				return plugins.Response{
					Status: 200,
					Body:   json.RawMessage(`{"sources":[{"id":"s2","pattern":"range","mime":"audio/flac","url":"https://media.example/trk-2.flac","headers":{"Authorization":"Bearer {secret:api_token}"}}]}`),
				}, nil
			}
		}
		return plugins.Response{Status: 404, Body: json.RawMessage(`{"error":"not found"}`)}, nil
	}

	const pluginID = "community.example.recordings"
	installStudioManifest(t, h.mgr, map[string]any{
		"id":      pluginID,
		"version": "1.0.0",
		"name":    "Field Recordings",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "content",
			"summary":      "Field recordings",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"catalog_provider", "kind_resolver"},
			"permissions": map[string]any{
				"network": []string{"media.example"},
				"secrets": []string{"api_token"},
			},
			"settings": map[string]any{
				"fields": []map[string]any{
					{"key": "api_token", "kind": "secret", "label": "API token"},
				},
			},
			"strings": map[string]any{"en": "i18n/en.json"},
			"contributes": map[string]any{
				"mediaKinds": []map[string]any{recordingsKindDecl(true)},
				"setup": map[string]any{
					"steps": []map[string]any{
						{
							"key":      "connect",
							"label":    "Connect account",
							"fields":   []string{"api_token"},
							"test":     "/ether/v1/setup/connect/test",
							"required": true,
						},
					},
				},
			},
		},
	}, map[string][]byte{
		"i18n/en.json": []byte(`{"lib_name":"Field Recordings"}`),
	})

	exp := h.experienceFor(t, "plex")

	// While required setup step is pending, library is gated from client Views and pull sync does not run.
	if syncCalls.Load() != 0 {
		t.Fatalf("pull sync ran before required setup step completed: %d calls", syncCalls.Load())
	}
	viewsPending, err := exp.Views(ctx, h.users[1])
	if err != nil || len(viewsPending) != 2 {
		t.Fatalf("expected pending setup library hidden from client Views, got %+v %v", viewsPending, err)
	}

	// Store secret via Configure and complete setup step -> unblocks sync and visibility immediately.
	if _, err := h.mgr.Configure(ctx, pluginID, map[string]any{"api_token": "top-secret-token"}); err != nil {
		t.Fatal(err)
	}
	stepRes, err := h.mgr.TestSetupStep(ctx, pluginID, "connect")
	if err != nil || !stepRes.OK {
		t.Fatalf("TestSetupStep: %+v %v", stepRes, err)
	}

	var pluginView frontend.View
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		views, _ := exp.Views(ctx, h.users[1])
		for _, v := range views {
			if v.PluginID == pluginID {
				pluginView = v
			}
		}
		if pluginView.ID != "" {
			if cnt, err := exp.LibraryCount(ctx, h.users[1], pluginView.ID); err == nil && cnt == 1 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pluginView.ID == "" || pluginView.Name != "Field Recordings" || pluginView.Native != "show" {
		t.Fatalf("expected active Field Recordings view after setup, got %+v", pluginView)
	}

	// Wait until child tracks have also finished upserting from the initial walk.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tally, _ := h.items.LibraryTally(ctx, pluginView.LibraryID)
		// The initial walk ends with a /changes probe after its upserts; wait for it so
		// the background sync is finished before calls are counted below.
		if tally["track"] == 2 && slices.Contains(h.rt.paths(), "/ether/v1/kinds/recordings/changes") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Offline browse proof: crash the plugin runtime and reset calls; browse, children,
	// ancestors, item detail, facets, and watch state must all succeed with 0 plugin calls.
	pluginBroken.Store(true)
	h.rt.resetCalls()

	rootPage, err := exp.LibraryItems(ctx, h.users[1], core.Device{}, pluginView.ID, frontend.ItemQuery{Limit: 10})
	if err != nil || len(rootPage.Items) != 1 || rootPage.Items[0].Title != "Harbour" {
		t.Fatalf("offline LibraryItems: %+v %v", rootPage, err)
	}
	albumKey := rootPage.Items[0].Key
	if !albumKey.IsPluginKey() {
		t.Fatalf("expected plugin RatingKey, got %d", albumKey)
	}

	childrenPage, err := exp.Children(ctx, h.users[1], core.Device{}, albumKey, frontend.ItemQuery{Limit: 10})
	if err != nil || len(childrenPage.Items) != 2 || childrenPage.Items[0].Title != "Gulls" || childrenPage.Items[1].Title != "Fog Horn" {
		t.Fatalf("offline Children: %+v %v", childrenPage, err)
	}
	track1Key := childrenPage.Items[0].Key
	track2Key := childrenPage.Items[1].Key

	ancestors, err := exp.Ancestors(ctx, h.users[1], core.Device{}, track2Key)
	if err != nil || len(ancestors) != 1 || ancestors[0].Key != albumKey {
		t.Fatalf("offline Ancestors: %+v %v", ancestors, err)
	}

	detail, err := exp.Item(ctx, frontend.ItemRequest{Viewer: h.users[1], Key: track2Key})
	if err != nil || detail.Title.Title != "Fog Horn" || detail.CanPlay == nil || !*detail.CanPlay {
		t.Fatalf("offline Item: %+v %v", detail, err)
	}

	facets, err := exp.Facets(ctx, h.users[1], core.Device{}, pluginView.ID, "genre")
	if err != nil || len(facets) != 1 || facets[0].Title != "ambient" {
		t.Fatalf("offline Facets: %+v %v", facets, err)
	}

	if err := exp.SetPlayed(ctx, h.users[1], track1Key); err != nil {
		t.Fatal(err)
	}
	ud, err := exp.UserData(ctx, h.users[1], []core.RatingKey{track1Key})
	if err != nil || ud[track1Key].ViewCount == 0 {
		t.Fatalf("offline UserData: %+v %v", ud, err)
	}
	if calls := h.rt.paths(); len(calls) != 0 {
		t.Fatalf("offline browse paths must never call plugin runtime, got %v", calls)
	}

	// Restore runtime and verify raw secret leakage in resolver headers is rejected.
	pluginBroken.Store(false)
	leakRawSecret.Store(true)
	if _, err := h.resolver.OpenKind(ctx, h.users[1], track1Key, nil); err == nil {
		t.Fatal("expected resolver header containing raw secret value to be rejected")
	}
	leakRawSecret.Store(false)

	// Verify KindResolver OpenKind, resolution cache, {secret:name}, KindLeaves, and Sequence.
	resolveCalls.Store(0)

	km, err := h.resolver.OpenKind(ctx, h.users[1], track1Key, nil)
	if err != nil {
		t.Fatalf("OpenKind track1: %v", err)
	}
	buf := make([]byte, km.Size)
	if _, err := km.ReadAt(buf, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	_ = km.Close()
	if !bytes.Equal(buf, track1Bytes) {
		t.Fatalf("track1 bytes %q, want %q", buf, track1Bytes)
	}
	if got, _ := gotAuthHeader.Load().(string); got != "Bearer top-secret-token" {
		t.Fatalf("{secret:api_token} expansion failed: Authorization header = %q", got)
	}
	if resolveCalls.Load() != 1 {
		t.Fatalf("expected 1 resolve call on cold open, got %d", resolveCalls.Load())
	}

	// Second open hits the persisted resolution cache without calling the plugin runtime.
	km2, err := h.resolver.OpenKind(ctx, h.users[1], track1Key, nil)
	if err != nil {
		t.Fatalf("OpenKind cached track1: %v", err)
	}
	_ = km2.Close()
	if resolveCalls.Load() != 1 {
		t.Fatalf("expected cached resolution hit (still 1 resolve call), got %d", resolveCalls.Load())
	}

	// KindLeaves and container Sequence over albumKey.
	leaves, err := h.resolver.KindLeaves(ctx, h.users[1], albumKey)
	if err != nil || len(leaves) != 2 || leaves[0].Key != track1Key || leaves[1].Key != track2Key {
		t.Fatalf("KindLeaves: %+v %v", leaves, err)
	}
	albumMedia, err := h.resolver.OpenKind(ctx, h.users[1], albumKey, nil)
	if err != nil {
		t.Fatalf("OpenKind container album: %v", err)
	}
	wantAlbumBytes := append(append([]byte(nil), track1Bytes...), track2Bytes...)
	gotAlbum := make([]byte, albumMedia.Size)
	if _, err := albumMedia.ReadAt(gotAlbum, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	_ = albumMedia.Close()
	if !bytes.Equal(gotAlbum, wantAlbumBytes) {
		t.Fatalf("album sequence bytes %q, want %q", gotAlbum, wantAlbumBytes)
	}

	// Viewer policy: child restricted by AllowedLibraries and MaxContentRating.
	h.users[2] = &core.User{ID: 2, Username: "kid", AllowedLibraries: []core.LibraryID{core.LibraryMovies}, MaxContentRating: "PG"}
	if _, err := exp.Item(ctx, frontend.ItemRequest{Viewer: h.users[2], Key: track1Key}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("viewer without library access got %v, want ErrNotFound", err)
	}
	if _, err := h.resolver.OpenKind(ctx, h.users[2], track1Key, nil); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("OpenKind without library access got %v, want ErrNotFound", err)
	}
	h.users[2].AllowedLibraries = []core.LibraryID{pluginView.LibraryID}
	if _, err := exp.Item(ctx, frontend.ItemRequest{Viewer: h.users[2], Key: track1Key}); err != nil {
		t.Fatalf("PG track should be allowed for PG viewer: %v", err)
	}
	if _, err := exp.Item(ctx, frontend.ItemRequest{Viewer: h.users[2], Key: track2Key}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("R-rated track must be hidden from PG viewer, got %v", err)
	}

	// KeyForRef / RefsForKeys round-trip the ItemRef identity used by Stremio.
	// Owner may resolve either; the restricted viewer may map a visible key to
	// its ref but a hidden library's key stays unresolvable.
	track1Refs, err := h.resolver.RefsForKeys(ctx, h.users[1], []core.RatingKey{track1Key, albumKey})
	if err != nil || len(track1Refs) != 2 {
		t.Fatalf("RefsForKeys owner: %+v %v", track1Refs, err)
	}
	trackRef := track1Refs[track1Key]
	if !trackRef.Valid() || trackRef.Kind() != "recordings" {
		t.Fatalf("track ref: %q", trackRef)
	}
	gotKey, err := h.resolver.KeyForRef(ctx, h.users[1], trackRef)
	if err != nil || gotKey != track1Key {
		t.Fatalf("KeyForRef round trip: %v %v", gotKey, err)
	}
	if _, err := h.resolver.KeyForRef(ctx, h.users[1], trackRef); err != nil {
		t.Fatalf("KeyForRef owner: %v", err)
	}
	outsider := &core.User{ID: 3, Username: "outsider", AllowedLibraries: []core.LibraryID{core.LibraryMovies}}
	if _, err := h.resolver.KeyForRef(ctx, outsider, trackRef); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("KeyForRef for a viewer outside the library: %v", err)
	}
	if _, err := h.resolver.KeyForRef(ctx, h.users[1], core.NewItemRef("field", "recordings", "nope")); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("KeyForRef for an unknown ref: %v", err)
	}

	// RolesOff and SetEnabled(false) hide without deleting items.
	if _, err := h.mgr.SetRoles(ctx, pluginID, []plugins.Capability{plugins.CapCatalogProvider}); err != nil {
		t.Fatal(err)
	}
	if viewsOff, err := exp.Views(ctx, h.users[1]); err != nil || len(viewsOff) != 2 {
		t.Fatalf("RolesOff(catalog_provider) must hide library, got %+v %v", viewsOff, err)
	}
	if _, err := h.mgr.SetRoles(ctx, pluginID, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.SetEnabled(ctx, pluginID, false); err != nil {
		t.Fatal(err)
	}
	if viewsDis, err := exp.Views(ctx, h.users[1]); err != nil || len(viewsDis) != 2 {
		t.Fatalf("SetEnabled(false) must hide library, got %+v %v", viewsDis, err)
	}
	if err := h.mgr.SetEnabled(ctx, pluginID, true); err != nil {
		t.Fatal(err)
	}
	if viewsRe, err := exp.Views(ctx, h.users[1]); err != nil || len(viewsRe) != 3 {
		t.Fatalf("re-enabling plugin must restore library, got %+v %v", viewsRe, err)
	}

	// Write a blob into the plugin library's blobcache namespace, KV, and match row, then Remove.
	libRow, err := h.libs.Get(ctx, pluginView.LibraryID)
	if err != nil {
		t.Fatal(err)
	}
	cachePut, _, _ := h.blobs.HostBackends(h.catalog.QuotaFor)
	if _, err := cachePut(ctx, pluginID, libRow.Key, "cover-1", bytes.NewReader([]byte("cached-blob"))); err != nil {
		t.Fatal(err)
	}
	nsDir := filepath.Join(h.blobDir, libRow.Key)
	if _, err := os.Stat(nsDir); err != nil {
		t.Fatalf("expected blobcache namespace dir on disk before Remove: %v", err)
	}
	if err := h.mgr.HostFor(pluginID).KVPut(ctx, "cursor", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	fogItem, _, err := h.items.GetByKey(ctx, track2Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.items.PinMatch(ctx, fogItem.Ref, "infohash:abc", []string{"manual"}); err != nil {
		t.Fatal(err)
	}

	// Verify state_watch has the plugin rating key row before Remove.
	var watchBefore int
	if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_watch WHERE rating_key >= ?`, int64(core.PluginKey(1))).Scan(&watchBefore); err != nil || watchBefore != 1 {
		t.Fatalf("expected 1 plugin state_watch row before Remove, got %d %v", watchBefore, err)
	}

	// Manager.Remove must leave zero plugin rows, zero plugin watch state, zero plugin libraries, and remove nsDir.
	if err := h.mgr.Remove(ctx, pluginID, false); err != nil {
		t.Fatalf("Manager.Remove: %v", err)
	}

	for _, table := range []string{
		"plugins_installed",
		"plugins_kv",
		"plugins_settings",
		"plugins_setup",
		"plugin_kinds",
		"plugin_items",
		"plugin_item_tally",
		"plugin_item_field_tally",
		"plugin_matches",
		"plugin_resolutions",
		"plugin_sync_state",
	} {
		var n int
		if err := h.db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("table %s retained %d rows after Manager.Remove", table, n)
		}
	}
	var watchRemaining int
	if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_watch WHERE rating_key >= ?`, int64(core.PluginKey(1))).Scan(&watchRemaining); err != nil {
		t.Fatal(err)
	}
	if watchRemaining != 0 {
		t.Fatalf("state_watch retained %d plugin rows after Manager.Remove", watchRemaining)
	}
	libsAfter, err := h.libs.List(ctx)
	if err != nil || len(libsAfter) != 2 {
		t.Fatalf("expected only 2 builtin libraries after Remove, got %+v %v", libsAfter, err)
	}
	if _, err := os.Stat(nsDir); !os.IsNotExist(err) {
		t.Fatalf("expected blobcache dir %s removed, stat err = %v", nsDir, err)
	}
}

func TestFilterLibraryAccess(t *testing.T) {
	h := newKindTestHarness(t, 0, nil)
	ctx := context.Background()
	exp := h.experienceFor(t, "jellyfin")

	// Viewer restricted to a Comedy filter library sees comedy movies but not drama movies.
	comedy := core.Title{Key: core.MovieKey(501), Type: core.TypeMovie, Title: "Funny One", Genres: []string{"Comedy"}}
	drama := core.Title{Key: core.MovieKey(502), Type: core.TypeMovie, Title: "Serious One", Genres: []string{"Drama"}}
	h.titles[comedy.Key] = comedy
	h.titles[drama.Key] = drama

	present := core.Presentations{
		MediaBrowser: &core.MediaBrowserPresentation{CollectionType: "movies"},
	}
	filterLib, err := h.libs.Create(ctx, "Comedy Movies", core.TypeMovie, libraryRules(core.Filter{Type: core.TypeMovie, Genres: []string{"Comedy"}}), "hide", present)
	if err != nil {
		t.Fatal(err)
	}
	h.users[3] = &core.User{ID: 3, Username: "comedy-only", AllowedLibraries: []core.LibraryID{filterLib.ID}}

	if _, err := exp.Item(ctx, frontend.ItemRequest{Viewer: h.users[3], Key: comedy.Key}); err != nil {
		t.Fatalf("viewer with filter library access must see matching title: %v", err)
	}
	if _, err := exp.Item(ctx, frontend.ItemRequest{Viewer: h.users[3], Key: drama.Key}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("viewer restricted to Comedy filter library must not see Drama title, got %v", err)
	}
}

func mustListLibs(t *testing.T, svc *libraries.Service) []core.Library {
	t.Helper()
	libs, err := svc.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return libs
}

func libraryRules(f core.Filter) []core.LibraryExclusion {
	return []core.LibraryExclusion{{Action: "show", MediaType: string(f.Type), Match: "all", Rules: core.LibraryPredicateConditions(f)}}
}

func TestKindCatalogPresentationTabSearchAndPlaceholder(t *testing.T) {
	h := newKindTestHarness(t, 250*time.Millisecond, nil)
	ctx := context.Background()

	const pluginID = "community.example.games"
	installStudioManifest(t, h.mgr, map[string]any{
		"id":      pluginID,
		"version": "1.0.0",
		"name":    "Video Games",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "content",
			"summary":      "Video games",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"catalog_provider", "kind_resolver"},
			"strings":      map[string]any{"en": "i18n/en.json"},
			"contributes": map[string]any{
				"mediaKinds": []map[string]any{{
					"key":   "game",
					"label": "Game",
					"library": map[string]any{
						"name": "Games",
						"card": "landscape",
						"tabs": []map[string]any{
							{"key": "recommended", "label": "Recommended", "kind": "recommended"},
							{"key": "library", "label": "Library", "kind": "library"},
							{"key": "developers", "label": "Developers", "kind": "collections", "facet": "developers", "order": "name", "searchPlaceholder": "$t:search_devs"},
							{"key": "cast", "label": "Voice cast", "kind": "people", "facet": "voiceCast", "order": "name", "searchPlaceholder": "Search voice cast"},
							{"key": "publishers", "label": "Publishers", "kind": "collections", "facet": "developers", "search": false},
						},
					},
					"levels": []map[string]any{{"key": "game", "label": "Game", "playable": true}},
					"fields": []map[string]any{
						{"key": "developers", "type": "strings", "label": "Developers", "indexed": true},
						{"key": "voiceCast", "type": "people", "label": "Voice cast", "indexed": true},
					},
					"present": map[string]any{"vela": map[string]any{"type": "movies"}},
					"browse": map[string]any{
						"sorts":  []string{"released", "title"},
						"facets": []string{"developers", "voiceCast"},
					},
					"sync": map[string]any{"mode": "push"},
				}},
			},
		},
	}, map[string][]byte{
		"i18n/en.json": []byte(`{"search_devs":"Search developers"}`),
	})

	var gameLib core.Library
	for _, lib := range mustListLibs(t, h.libs) {
		if lib.PluginID == pluginID {
			gameLib = lib
		}
	}
	if gameLib.ID == 0 {
		t.Fatal("expected game plugin library")
	}

	pres, ok := h.catalog.Presentation(ctx, gameLib)
	if !ok || len(pres.Tabs) != 5 {
		t.Fatalf("Presentation = (%+v, %v), want 5 tabs", pres, ok)
	}
	if pres.Tabs[0].Search || pres.Tabs[1].Search {
		t.Fatalf("recommended/library tabs must not set Search: %+v", pres.Tabs[:2])
	}
	if !pres.Tabs[2].Search || pres.Tabs[2].SearchPlaceholder != "Search developers" {
		t.Fatalf("developers tab = %+v, want Search=true and resolved $t: SearchPlaceholder", pres.Tabs[2])
	}
	if !pres.Tabs[3].Search || pres.Tabs[3].SearchPlaceholder != "Search voice cast" {
		t.Fatalf("cast tab = %+v, want Search=true and literal SearchPlaceholder", pres.Tabs[3])
	}
	if pres.Tabs[4].Search || pres.Tabs[4].SearchPlaceholder != "" {
		t.Fatalf("publishers tab = %+v, want Search=false", pres.Tabs[4])
	}

	_, err := h.items.KindUpsert(ctx, pluginID, "game", []core.KindItem{
		{
			ID:       "g1",
			Level:    "game",
			Index:    1,
			Title:    "Echoes of Zoë",
			Playable: true,
			Fields: map[string]json.RawMessage{
				"developers": json.RawMessage(`["Zoë Mode","Nintendo"]`),
				"voiceCast":  json.RawMessage(`[{"name":"Zoë Wanamaker"},{"name":"Troy Baker"}]`),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	fc, err := h.catalog.FacetCollections(ctx, gameLib, "developers", "name", "zoe", 0, 10)
	if err != nil || len(fc.Items) != 1 || fc.Items[0].Name != "Zoë Mode" {
		t.Fatalf("KindCatalog.FacetCollections(query=zoe) = %+v, err=%v", fc, err)
	}
	fp, err := h.catalog.FacetPeople(ctx, gameLib, "voiceCast", "name", "zoe", 0, 10)
	if err != nil || len(fp.Items) != 1 || fp.Items[0].Name != "Zoë Wanamaker" {
		t.Fatalf("KindCatalog.FacetPeople(query=zoe) = %+v, err=%v", fp, err)
	}
}

func TestKindCatalogPresentationShelvesAndIndexedBrowse(t *testing.T) {
	h := newKindTestHarness(t, 250*time.Millisecond, nil)
	ctx := context.Background()

	pluginID := "community.docs.archive"
	installStudioManifest(t, h.mgr, map[string]any{
		"id":      pluginID,
		"version": "1.0.0",
		"name":    "Documentary Archive",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "content",
			"summary":      "Adds a curated documentary library",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"catalog_provider", "kind_resolver"},
			"strings":      map[string]any{"en": "i18n/en.json"},
			"contributes": map[string]any{
				"mediaKinds": []any{map[string]any{
					"key":   "documentary",
					"label": "Documentaries",
					"library": map[string]any{
						"name": "Documentaries",
						"card": "landscape",
						"shelves": []any{
							map[string]any{"key": "latest", "label": "Latest releases"},
							map[string]any{"key": "earth-science", "label": "$t:shelf_earth_science", "sort": "rating", "direction": "desc", "facet": "topic", "values": []string{"Science", "Nature"}},
							map[string]any{"key": "feature-history", "label": "Feature history", "sort": "title", "direction": "asc", "filters": map[string]any{"topic": []string{"History"}, "format": []string{"Feature"}}},
						},
					},
					"levels": []any{map[string]any{"key": "film", "label": "Film", "playable": true}},
					"fields": []any{
						map[string]any{"key": "topic", "type": "strings", "label": "Topic", "indexed": true},
						map[string]any{"key": "format", "type": "enum", "label": "Format", "indexed": true, "enum": []string{"Feature", "Short"}},
					},
					"present": map[string]any{"vela": map[string]any{"type": "movies"}},
					"browse": map[string]any{
						"sorts":  []string{"title", "released"},
						"facets": []string{"topic"},
					},
					"sync": map[string]any{"mode": "push"},
				}},
			},
		},
	}, map[string][]byte{
		"i18n/en.json": []byte(`{"shelf_earth_science":"Earth and science"}`),
	})

	var docLib core.Library
	for _, lib := range mustListLibs(t, h.libs) {
		if lib.PluginID == pluginID {
			docLib = lib
		}
	}
	if docLib.ID == 0 {
		t.Fatal("expected documentary plugin library")
	}

	pres, ok := h.catalog.Presentation(ctx, docLib)
	if !ok || len(pres.Shelves) != 3 {
		t.Fatalf("Presentation = (%+v, %v), want 3 shelves", pres, ok)
	}
	if pres.Shelves[0].Key != "latest" || pres.Shelves[0].Sort != core.SortReleased || pres.Shelves[0].Direction != "desc" || pres.Shelves[0].Filters != nil {
		t.Fatalf("shelf[0] = %+v", pres.Shelves[0])
	}
	if pres.Shelves[1].Label != "Earth and science" || pres.Shelves[1].Sort != core.SortRating || len(pres.Shelves[1].Filters["topic"]) != 2 {
		t.Fatalf("shelf[1] = %+v", pres.Shelves[1])
	}
	if pres.Shelves[2].Sort != core.SortTitle || pres.Shelves[2].Direction != "asc" || len(pres.Shelves[2].Filters) != 2 {
		t.Fatalf("shelf[2] = %+v", pres.Shelves[2])
	}

	_, err := h.items.KindUpsert(ctx, pluginID, "documentary", []core.KindItem{
		{
			ID: "d1", Level: "film", Title: "Deep Ocean", Playable: true, Rating: 9.1,
			Fields: map[string]json.RawMessage{"topic": json.RawMessage(`["Nature"]`), "format": json.RawMessage(`"Feature"`)},
		},
		{
			ID: "d2", Level: "film", Title: "Quantum Fields", Playable: true, Rating: 8.4,
			Fields: map[string]json.RawMessage{"topic": json.RawMessage(`["Science"]`), "format": json.RawMessage(`"Short"`)},
		},
		{
			ID: "d3", Level: "film", Title: "Ancient Roads", Playable: true, Rating: 8.8,
			Fields: map[string]json.RawMessage{"topic": json.RawMessage(`["History"]`), "format": json.RawMessage(`"Feature"`)},
		},
		{
			ID: "d4", Level: "film", Title: "Brief History", Playable: true, Rating: 7.5,
			Fields: map[string]json.RawMessage{"topic": json.RawMessage(`["History"]`), "format": json.RawMessage(`"Short"`)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// OR within a facet ("Science" OR "Nature") ordered by rating desc, NoTotal: true.
	orPage, err := h.catalog.Browse(ctx, docLib, frontend.ItemQuery{
		Sort:    pres.Shelves[1].Sort,
		Desc:    pres.Shelves[1].Direction != "asc",
		NoTotal: true,
		Filter:  core.Filter{Facets: pres.Shelves[1].Filters},
	}, "", 20)
	if err != nil || len(orPage.Items) != 2 || orPage.Items[0].Title != "Deep Ocean" || orPage.Items[1].Title != "Quantum Fields" {
		t.Fatalf("OR shelf browse = %+v, err=%v", orPage.Items, err)
	}

	// AND across facets (topic="History" AND format="Feature"), even though "format" is indexed but not in browse.facets.
	andPage, err := h.catalog.Browse(ctx, docLib, frontend.ItemQuery{
		Sort:    pres.Shelves[2].Sort,
		Desc:    pres.Shelves[2].Direction != "asc",
		NoTotal: true,
		Filter:  core.Filter{Facets: pres.Shelves[2].Filters},
	}, "", 20)
	if err != nil || len(andPage.Items) != 1 || andPage.Items[0].Title != "Ancient Roads" {
		t.Fatalf("AND shelf browse = %+v, err=%v", andPage.Items, err)
	}
}

func TestKindCatalogManifestViewHistoryTab(t *testing.T) {
	ctx := context.Background()
	h := newKindTestHarness(t, 250*time.Millisecond, nil)
	if _, err := h.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS state_watch (
		user_id BIGINT NOT NULL,
		rating_key BIGINT NOT NULL,
		view_offset_ns BIGINT NOT NULL DEFAULT 0,
		duration_ns BIGINT NOT NULL DEFAULT 0,
		view_count BIGINT NOT NULL DEFAULT 0,
		counted BIGINT NOT NULL DEFAULT 0,
		last_viewed_at BIGINT NOT NULL DEFAULT 0,
		user_rating DOUBLE PRECISION NOT NULL DEFAULT 0,
		updated_at BIGINT NOT NULL DEFAULT 0,
		PRIMARY KEY (user_id, rating_key)
	)`); err != nil {
		t.Fatal(err)
	}
	const pluginID = "community.example.history-docs"
	installStudioManifest(t, h.mgr, map[string]any{
		"id":      pluginID,
		"version": "1.0.0",
		"name":    "History Docs",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "content",
			"summary":      "Documentaries with a manifest History view tab",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"catalog_provider", "kind_resolver"},
			"contributes": map[string]any{
				"mediaKinds": []any{map[string]any{
					"key":   "doc",
					"label": "Documentaries",
					"library": map[string]any{
						"name": "Documentaries",
						"tabs": []any{
							map[string]any{"key": "recommended", "label": "Recommended", "kind": "recommended"},
							map[string]any{"key": "library", "label": "Library", "kind": "library"},
							map[string]any{
								"key":               "history",
								"label":             "History",
								"kind":              "view",
								"layout":            "timeline",
								"sort":              "lastViewed",
								"direction":         "desc",
								"searchPlaceholder": "Search watch history",
								"filters": map[string]any{
									"match": "all",
									"rules": []any{
										map[string]any{"field": "viewer", "op": "is", "value": map[string]any{"history": true}},
									},
								},
							},
						},
					},
					"levels":  []any{map[string]any{"key": "film", "label": "Film", "playable": true}},
					"present": map[string]any{"vela": map[string]any{"type": "movies"}},
					"browse":  map[string]any{"sorts": []string{"title", "released"}},
					"sync":    map[string]any{"mode": "push"},
				}},
			},
		},
	}, nil)

	var docLib core.Library
	for _, lib := range mustListLibs(t, h.libs) {
		if lib.PluginID == pluginID {
			docLib = lib
		}
	}
	if docLib.ID == 0 {
		t.Fatal("expected plugin library")
	}

	pres, ok := h.catalog.Presentation(ctx, docLib)
	if !ok || len(pres.Tabs) != 3 {
		t.Fatalf("Presentation tabs = %+v (ok=%v)", pres.Tabs, ok)
	}
	hist := pres.Tabs[2]
	if hist.ID != "history" || hist.Kind != core.TabView || hist.Layout != core.TabLayoutTimeline || hist.Sort != "lastViewed" || hist.Direction != "desc" || !hist.Search || hist.SearchPlaceholder != "Search watch history" || hist.Filters == nil {
		t.Fatalf("manifest history ViewTab = %+v", hist)
	}

	if _, err := h.items.KindUpsert(ctx, pluginID, "doc", []core.KindItem{
		{ID: "d1", Level: "film", Title: "Alpha", Playable: true},
		{ID: "d2", Level: "film", Title: "Beta", Playable: true},
		{ID: "d3", Level: "film", Title: "Gamma", Playable: true},
	}); err != nil {
		t.Fatal(err)
	}
	k1, _, _ := h.items.GetByRef(ctx, core.NewItemRef(pluginID, "doc", "d1"))
	k2, _, _ := h.items.GetByRef(ctx, core.NewItemRef(pluginID, "doc", "d2"))
	k3, _, _ := h.items.GetByRef(ctx, core.NewItemRef(pluginID, "doc", "d3"))
	// Viewer 1 watched d1 earlier and d3 later; Viewer 2 watched d2 only.
	if _, err := h.db.ExecContext(ctx, `INSERT INTO state_watch(user_id,rating_key,view_count,last_viewed_at) VALUES (1,?,1,1000),(1,?,1,3000),(2,?,1,2000)`, int64(k1.Key), int64(k3.Key), int64(k2.Key)); err != nil {
		t.Fatal(err)
	}

	rules, fallback := hist.Filters.ToPolicy("")
	q := frontend.ItemQuery{
		Sort: core.SortLastViewed,
		Desc: true,
		Filter: core.Filter{
			TabRules:    rules,
			TabFallback: fallback,
		},
	}

	v1Page, err := h.catalog.Browse(core.WithCatalogViewer(ctx, &core.User{ID: 1}), docLib, q, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(v1Page.Items) != 2 || v1Page.Items[0].Title != "Gamma" || v1Page.Items[1].Title != "Alpha" {
		t.Fatalf("viewer 1 history = %+v, want [Gamma, Alpha]", v1Page.Items)
	}

	v2Page, err := h.catalog.Browse(core.WithCatalogViewer(ctx, &core.User{ID: 2}), docLib, q, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(v2Page.Items) != 1 || v2Page.Items[0].Title != "Beta" {
		t.Fatalf("viewer 2 history = %+v, want [Beta]", v2Page.Items)
	}
}
