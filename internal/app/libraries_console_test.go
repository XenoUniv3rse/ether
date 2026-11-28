package app

import (
	"context"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/sessions"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraries"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/plugins"
)

func TestLibrariesConsoleCRUDAndFilterBrowse(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	for _, title := range []core.Title{
		{Key: core.MovieKey(101), Type: core.TypeMovie, Title: "Alpha Drama", Year: 2001, Released: time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), HomeReleased: time.Date(2001, 6, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Drama"}},
		{Key: core.MovieKey(102), Type: core.TypeMovie, Title: "Beta Laughs", Year: 2002, Released: time.Date(2002, 1, 1, 0, 0, 0, 0, time.UTC), HomeReleased: time.Date(2002, 6, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Comedy"}},
	} {
		if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}
	c := &librariesConsole{app: a}
	h := c.handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	// Built-ins are seeded and listed.
	w := call("GET", "/", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"key":"movies"`) {
		t.Fatalf("list %d %s", w.Code, w.Body)
	}

	// Create a console-only filter library (spec §15.3).
	w = call("POST", "/", `{"name":"Dramas","mediaType":"movie","rules":[{"action":"show","mediaType":"movie","match":"all","rules":[{"field":"genre","op":"in","value":["Drama"]}]}],"visibilityDefault":"hide"}`)
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	var lib libraryDTO
	if err := json.Unmarshal(w.Body.Bytes(), &lib); err != nil {
		t.Fatal(err)
	}
	if lib.Source != core.LibraryCatalog || lib.Present.Vela != nil || lib.Present.Plex != nil || lib.Present.MediaBrowser != nil {
		t.Fatalf("new filter library must be console-only: %+v", lib.Present)
	}
	id := strconv.FormatInt(int64(lib.ID), 10)

	// Browsing it shows only the filtered set.
	w = call("GET", "/"+id+"/items", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Alpha Drama") || strings.Contains(w.Body.String(), "Beta Laughs") {
		t.Fatalf("filter browse %d %s", w.Code, w.Body)
	}

	// Rename + hide + show-in-apps (Vela + Plex) through PATCH; reject invalid Vela type.
	if bad := call("PATCH", "/"+id, `{"present":{"vela":{"type":"bad"}}}`); bad.Code != 400 {
		t.Fatalf("invalid vela type = %d %s, want 400", bad.Code, bad.Body)
	}
	w = call("PATCH", "/"+id, `{"name":"Serious films","hidden":true,"present":{"vela":{"type":"movies"},"plex":{"base":"movie"}}}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Serious films") || !strings.Contains(w.Body.String(), `"hidden":true`) || !strings.Contains(w.Body.String(), `"vela":{"type":"movies"}`) {
		t.Fatalf("patch %d %s", w.Code, w.Body)
	}

	// The saved filter can be replaced under the same id.
	if w := call("PATCH", "/"+id, `{"rules":[],"visibilityDefault":"show"}`); w.Code != 200 {
		t.Fatalf("filter patch %d %s", w.Code, w.Body)
	}

	// Both preseeded and owner-created libraries can be deleted.
	if w := call("DELETE", "/1", ""); w.Code != 204 {
		t.Fatalf("builtin delete %d", w.Code)
	}
	if w := call("DELETE", "/"+id, ""); w.Code != 204 {
		t.Fatalf("delete %d %s", w.Code, w.Body)
	}
	if w := call("GET", "/"+id, ""); w.Code != 404 {
		t.Fatalf("get deleted %d", w.Code)
	}
}

func TestLibrariesConsoleCollectionSource(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	for _, title := range []core.Title{
		{Key: core.MovieKey(201), Type: core.TypeMovie, Title: "Listed Film", Year: 2005, Released: time.Date(2005, 1, 1, 0, 0, 0, 0, time.UTC), HomeReleased: time.Date(2005, 6, 1, 0, 0, 0, 0, time.UTC)},
		{Key: core.MovieKey(202), Type: core.TypeMovie, Title: "Unlisted Film", Year: 2006, Released: time.Date(2006, 1, 1, 0, 0, 0, 0, time.UTC), HomeReleased: time.Date(2006, 6, 1, 0, 0, 0, 0, time.UTC)},
	} {
		if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}
	// Persist a collection whose members are exactly the listed film.
	now := time.Now().UTC()
	for _, stmt := range []string{
		`INSERT INTO collections_specs(id,slug,builtin,owner_user_id,spec_json,enabled,created_at,updated_at,last_built_at,build_hash,changed_at)
		 VALUES(31,'',0,0,'{}',1,?,?,?,'built',?)`,
		`INSERT INTO collections_members(spec_id,pos,key,shared) VALUES(31,0,?,1)`,
	} {
		args := []any{now.Unix(), now.Unix(), now.Unix(), now.Unix()}
		if strings.Contains(stmt, "collections_members") {
			args = []any{int64(core.MovieKey(201))}
		}
		if _, err := a.db.ExecContext(ctx, stmt, args...); err != nil {
			t.Fatal(err)
		}
	}

	c := &librariesConsole{app: a}
	h := c.handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := call("GET", "/collection-sources", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":31`) {
		t.Fatalf("collection sources %d %s", w.Code, w.Body)
	}

	w = call("POST", "/", `{"name":"MDBList picks","collectionId":31,"mediaType":"movie"}`)
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	var lib libraryDTO
	if err := json.Unmarshal(w.Body.Bytes(), &lib); err != nil {
		t.Fatal(err)
	}
	if lib.Source != core.LibraryCollection || lib.CollectionID != 31 || lib.MediaType != core.TypeMovie {
		t.Fatalf("created library = %+v", lib)
	}
	if lib.Present.Vela != nil || lib.Present.Plex != nil || lib.Present.MediaBrowser != nil {
		t.Fatalf("new collection library must be console-only: %+v", lib.Present)
	}
	id := strconv.FormatInt(int64(lib.ID), 10)
	w = call("GET", "/"+id+"/items", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Listed Film") || strings.Contains(w.Body.String(), "Unlisted Film") {
		t.Fatalf("collection browse %d %s", w.Code, w.Body)
	}

	// Deleting the collection leaves the library empty, never a 500.
	if _, err := a.db.ExecContext(ctx, `DELETE FROM collections_members WHERE spec_id=31`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `DELETE FROM collections_specs WHERE id=31`); err != nil {
		t.Fatal(err)
	}
	w = call("GET", "/"+id+"/items", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "Listed Film") {
		t.Fatalf("deleted collection browse %d %s", w.Code, w.Body)
	}
	if w = call("GET", "/"+id, ""); w.Code != 200 {
		t.Fatalf("deleted collection decorate %d %s", w.Code, w.Body)
	}
}

func TestLibrariesConsolePluginItemMatchPinClear(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	lib, err := a.libraries.UpsertPluginLibrary(ctx, "demo", "book", libraries.PluginLibraryDecl{Name: "Books"})
	if err != nil {
		t.Fatal(err)
	}
	c := &librariesConsole{app: a}
	items, _, err := c.services()
	if err != nil {
		t.Fatal(err)
	}
	decl := plugins.MediaKindMeta{
		Key: "book", Label: "Books",
		Library: plugins.KindLibraryDecl{Name: "Books"},
		Levels:  []plugins.KindLevel{{Key: "book", Label: "Book", Playable: true}},
		Fields: []plugins.KindField{
			{Key: "author", Type: "string", Label: "Author", Indexed: true},
		},
		Browse: plugins.KindBrowseDecl{Facets: []string{"author"}, Search: true},
		Sync:   plugins.KindSyncDecl{Mode: plugins.KindSyncPush},
	}
	if _, err := items.UpsertKind(ctx, "demo", lib.ID, decl, false); err != nil {
		t.Fatal(err)
	}
	if _, err := items.UpsertItems(ctx, "demo", "book", []core.KindItem{{
		ID: "b1", Level: "book", Title: "Dune", Playable: true,
		Fields: map[string]json.RawMessage{"author": json.RawMessage(`"Herbert"`)},
	}}); err != nil {
		t.Fatal(err)
	}
	ref := string(core.NewItemRef("demo", "book", "b1"))
	base := "/" + strconv.FormatInt(int64(lib.ID), 10)
	h := c.handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call("GET", base+"/items", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "Dune") {
		t.Fatalf("items %d %s", w.Code, w.Body)
	}
	if w := call("GET", base+"/facets/author", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "Herbert") {
		t.Fatalf("facets %d %s", w.Code, w.Body)
	}
	if w := call("PUT", base+"/item/"+ref+"/match", `{"candidateRef":"file-dune-epub"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"chosen":true`) {
		t.Fatalf("pin %d %s", w.Code, w.Body)
	}
	if w := call("DELETE", base+"/item/"+ref+"/match", ""); w.Code != 200 || strings.Contains(w.Body.String(), "file-dune-epub") {
		t.Fatalf("clear %d %s", w.Code, w.Body)
	}
	// Owner deletion removes publication, while explicit restoration keeps items.
	if w := call("DELETE", base, ""); w.Code != 204 {
		t.Fatalf("plugin delete %d", w.Code)
	}
	if _, err := a.libraries.UpsertPluginLibrary(ctx, "demo", "book", libraries.PluginLibraryDecl{Name: "Books"}); err != libraries.ErrDeleted {
		t.Fatalf("automatic resurrection: %v", err)
	}
	if _, err := a.libraries.RestorePluginLibrary(ctx, "demo", "book", libraries.PluginLibraryDecl{Name: "Books"}); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", base+"/items", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "Dune") {
		t.Fatalf("restored items: %d %s", w.Code, w.Body)
	}
	// Cache view is available for plugin libraries only.
	if w := call("GET", base+"/cache", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"maxBytes":0`) {
		t.Fatalf("cache %d %s", w.Code, w.Body)
	}
	if w := call("GET", "/1/cache", ""); w.Code != 404 {
		t.Fatalf("builtin cache %d", w.Code)
	}
	_ = pluginitems.DefaultMatchThreshold
}

type fakeBookMatchRuntime struct {
	resolveCalls    int
	matchCalls      int
	matchCandidates int
}

func (f *fakeBookMatchRuntime) Call(_ context.Context, req plugins.Request) (plugins.Response, error) {
	switch req.Path {
	case "/ether/v1/kinds/books/resolve":
		f.resolveCalls++
		return plugins.Response{
			Status: 200,
			Body: json.RawMessage(`{"sources":[{
				"id":"nzb-dune-epub",
				"pattern":"release",
				"label":"Dune - Frank Herbert.epub",
				"mime":"application/epub+zip",
				"size":2400000,
				"release":{
					"nzbUrl":"https://indexer.example/nzb/dune-epub",
					"candidate":{"nzbUrl":"https://indexer.example/nzb/dune-epub","title":"Dune - Frank Herbert.epub","size":2400000}
				}
			}]}`),
		}, nil
	case "/ether/v1/kinds/books/match":
		f.matchCalls++
		var body struct {
			Candidates []core.SourceCandidate `json:"candidates"`
		}
		_ = json.Unmarshal(req.Body, &body)
		f.matchCandidates = len(body.Candidates)
		matches := make([]core.KindMatchScore, 0, len(body.Candidates))
		for _, cand := range body.Candidates {
			ref := cand.NZBURL
			if ref == "" {
				ref = cand.Title
			}
			matches = append(matches, core.KindMatchScore{
				CandidateRef: ref,
				Score:        0.95,
				Reasons:      []string{"title and epub format match"},
			})
		}
		raw, _ := json.Marshal(map[string]any{"matches": matches})
		return plugins.Response{Status: 200, Body: raw}, nil
	default:
		return plugins.Response{Status: 200, Body: json.RawMessage(`{"status":"ok"}`)}, nil
	}
}

func (f *fakeBookMatchRuntime) Close() error { return nil }

func TestLibrariesConsoleBookMatchRunsResolverAndFeedsMatcher(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	rt := &fakeBookMatchRuntime{}
	a.pluginsRT.mgr.RegisterRuntime("hosted", func(plugins.Installed) (plugins.Runtime, error) {
		return rt, nil
	})
	rawMan, err := json.Marshal(map[string]any{
		"id":      "ether.books.test",
		"version": "1.0.5",
		"name":    "Books",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "content",
			"summary":      "Test book plugin",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"catalog_provider", "kind_resolver", "matcher"},
			"contributes": map[string]any{
				"mediaKinds": []map[string]any{{
					"key":     "books",
					"label":   "Books",
					"media":   "book",
					"library": map[string]any{"name": "Books"},
					"levels":  []map[string]any{{"key": "book", "label": "Book", "playable": true}},
					"sync":    map[string]any{"mode": "push"},
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	prev, err := a.pluginsRT.mgr.PreviewInstall(ctx, plugins.Source{
		Kind:     "studio",
		Manifest: rawMan,
		Files:    map[string][]byte{"dist/plugin.js": []byte(`function handle(req) { return { status: 200, body: { status: "ok" } }; }`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.pluginsRT.mgr.Install(ctx, prev, plugins.InstallOptions{AcceptAddedPermissions: true}); err != nil {
		t.Fatal(err)
	}
	lib, err := a.libraries.UpsertPluginLibrary(ctx, "ether.books.test", "books", libraries.PluginLibraryDecl{Name: "Books"})
	if err != nil {
		t.Fatal(err)
	}
	c := &librariesConsole{app: a}
	items, _, err := c.services()
	if err != nil {
		t.Fatal(err)
	}
	decl := plugins.MediaKindMeta{
		Key: "books", Label: "Books", Media: "book",
		Library: plugins.KindLibraryDecl{Name: "Books"},
		Levels:  []plugins.KindLevel{{Key: "book", Label: "Book", Playable: true}},
		Sync:    plugins.KindSyncDecl{Mode: plugins.KindSyncPush},
	}
	if _, err := items.UpsertKind(ctx, "ether.books.test", lib.ID, decl, false); err != nil {
		t.Fatal(err)
	}
	if _, err := items.UpsertItems(ctx, "ether.books.test", "books", []core.KindItem{{
		ID: "ol-OL893414W", Level: "book", Title: "Dune", Playable: true,
	}}); err != nil {
		t.Fatal(err)
	}

	ref := string(core.NewItemRef("ether.books.test", "books", "ol-OL893414W"))
	req := httptest.NewRequest(http.MethodPost, "/"+strconv.FormatInt(int64(lib.ID), 10)+"/item/"+ref+"/match", nil)
	rec := httptest.NewRecorder()
	c.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /match = (%d) %s", rec.Code, rec.Body.String())
	}
	if rt.resolveCalls != 1 {
		t.Fatalf("resolveCalls = %d, want 1", rt.resolveCalls)
	}
	if rt.matchCalls != 1 || rt.matchCandidates != 1 {
		t.Fatalf("matchCalls=%d matchCandidates=%d, want 1/1", rt.matchCalls, rt.matchCandidates)
	}
	if !strings.Contains(rec.Body.String(), `"candidateRef":"https://indexer.example/nzb/dune-epub"`) || !strings.Contains(rec.Body.String(), `"score":0.95`) {
		t.Fatalf("unexpected match response: %s", rec.Body.String())
	}
}

// This exercises the durable declaration, policy cache and already-running
// protocol experiences together; deleting a section never deletes shared data.
func TestLibraryDeletionProtocolLifecycle(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	title := core.Title{Key: core.MovieKey(991), Type: core.TypeMovie, Title: "Kept", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	if err := a.state.Scrobble(ctx, owner.ID, title.Key); err != nil {
		t.Fatal(err)
	}
	before, err := a.state.Get(ctx, owner.ID, title.Key)
	if err != nil {
		t.Fatal(err)
	}
	experiences := map[core.Protocol]frontend.Experience{}
	for _, protocol := range []core.Protocol{"plex", "jellyfin", "emby", "stremio", "silo", "vela", "xtream", "dlna", "webdav", "m3u"} {
		exp, err := a.newSurfaceExperience(protocol)
		if err != nil {
			t.Fatal(err)
		}
		experiences[protocol] = exp
		views, err := exp.Views(ctx, owner)
		if err != nil || len(views) != 2 {
			t.Fatalf("%s seed views: %+v %v", protocol, views, err)
		}
	}
	srv := plex.NewServer(plex.Deps{Host: frontend.Host{Experience: experiences["plex"], Sessions: sessions.NewLocalRuntime(sessions.LocalRuntimeDeps{})}, DisableEnrichment: true})
	defer srv.Close()
	if err := a.libraries.SetRules(ctx, 1, nil, "hide"); err != nil {
		t.Fatal(err)
	}
	if err := a.refreshLibraryPolicies(ctx); err != nil {
		t.Fatal(err)
	}
	h := (&librariesConsole{app: a}).handler()
	for _, id := range []string{"1", "2"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("DELETE", "/"+id, nil))
		if w.Code != 204 {
			t.Fatalf("delete %s: %d %s", id, w.Code, w.Body)
		}
	}
	if len(srv.LibrarySections()) != 0 {
		t.Fatal("Plex retained phantom sections")
	}
	for protocol, exp := range experiences {
		views, err := exp.Views(ctx, owner)
		if err != nil || len(views) != 0 {
			t.Fatalf("%s deleted views: %+v %v", protocol, views, err)
		}
		if _, err := exp.LibraryItems(ctx, owner, core.Device{}, "1", frontend.ItemQuery{}); err == nil {
			t.Fatalf("%s retained deleted route", protocol)
		}
		rows, err := exp.Rows(ctx, frontend.RowRequest{Viewer: owner, Device: core.Device{ID: "lifecycle"}, Placement: frontend.PlacementHome, Limit: 10})
		if err != nil {
			t.Fatalf("%s empty Home: %v", protocol, err)
		}
		for _, row := range rows {
			if len(row.Items) > 0 || len(row.Entries) > 0 {
				t.Fatalf("%s empty Home has titles: %+v", protocol, row)
			}
		}
	}
	if _, err := a.catalog.StoredTitle(ctx, title.Key); err != nil {
		t.Fatalf("media lost: %v", err)
	}
	after, err := a.state.Get(ctx, owner.ID, title.Key)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("history changed: %+v %+v %v", before, after, err)
	}
	var present core.Presentations
	if err := json.Unmarshal([]byte(`{"plex":{"base":"movie"}}`), &present); err != nil {
		t.Fatal(err)
	}
	lib, err := a.libraries.Create(ctx, "New films", core.TypeMovie, nil, "show", present)
	if err != nil || lib.ID <= 2 {
		t.Fatalf("recreation: %+v %v", lib, err)
	}
	page, err := experiences["plex"].LibraryItems(ctx, owner, core.Device{}, strconv.Itoa(int(lib.ID)), frontend.ItemQuery{Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("recreated library inherits deleted policy: %+v %v", page, err)
	}
	if velaViews, err := experiences["vela"].Views(ctx, owner); err != nil || len(velaViews) != 0 {
		t.Fatalf("plex-only library leaked into Vela: %+v %v", velaViews, err)
	}
	if sections := srv.LibrarySections(); len(sections) != 1 || sections[0].ID != int(lib.ID) {
		t.Fatalf("recreated sections: %+v", sections)
	}
}

func TestLibrariesConsolePresentAppsAndFreshness(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()

	// Seed a book plugin library and a video plugin library with declared kinds.
	bookDecl := plugins.MediaKindMeta{
		Key:     "book",
		Label:   "Books",
		Media:   string(core.MediaClassBook),
		Library: plugins.KindLibraryDecl{Name: "Books"},
		Levels:  []plugins.KindLevel{{Key: "book", Label: "Book", Playable: true}},
		Browse:  plugins.KindBrowseDecl{Search: true},
		Sync:    plugins.KindSyncDecl{Mode: plugins.KindSyncPush},
	}
	bookRes := libraries.ResolveKindPresentations(libraries.KindPresentationInput{
		Media:  bookDecl.Media,
		Levels: []libraries.KindPresentationLevel{{Key: "book", Playable: true}},
	}, core.PresentationOverrides{})
	bookLib, err := a.libraries.UpsertPluginLibrary(ctx, "demo.books", "book", libraries.PluginLibraryDecl{
		Name:    "Books",
		Present: bookRes.Effective,
	})
	if err != nil {
		t.Fatal(err)
	}

	clipDecl := plugins.MediaKindMeta{
		Key:     "clip",
		Label:   "Clips",
		Media:   string(core.MediaClassVideo),
		Library: plugins.KindLibraryDecl{Name: "Studio Clips"},
		Levels:  []plugins.KindLevel{{Key: "clip", Label: "Clip", Playable: true}},
		Browse:  plugins.KindBrowseDecl{Search: true},
		Sync:    plugins.KindSyncDecl{Mode: plugins.KindSyncPush},
	}
	clipRes := libraries.ResolveKindPresentations(libraries.KindPresentationInput{
		Media:  clipDecl.Media,
		Levels: []libraries.KindPresentationLevel{{Key: "clip", Playable: true}},
	}, core.PresentationOverrides{})
	clipLib, err := a.libraries.UpsertPluginLibrary(ctx, "demo.clips", "clip", libraries.PluginLibraryDecl{
		Name:    "Studio Clips",
		Present: clipRes.Effective,
	})
	if err != nil {
		t.Fatal(err)
	}

	c := &librariesConsole{app: a}
	items, _, err := c.services()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := items.UpsertKind(ctx, "demo.books", bookLib.ID, bookDecl, false); err != nil {
		t.Fatal(err)
	}
	if _, err := items.UpsertKind(ctx, "demo.clips", clipLib.ID, clipDecl, false); err != nil {
		t.Fatal(err)
	}

	h := c.handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	bookPath := "/" + strconv.FormatInt(int64(bookLib.ID), 10)
	clipPath := "/" + strconv.FormatInt(int64(clipLib.ID), 10)

	// GET returns resolved presentations showing Plex unavailable on Books.
	w := call("GET", bookPath, "")
	if w.Code != 200 {
		t.Fatalf("GET book %d %s", w.Code, w.Body)
	}
	var dto libraryDTO
	if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	if dto.HasVideo || dto.Presentations == nil || dto.Presentations.MediaClass != core.MediaClassBook {
		t.Fatalf("book hasVideo=%v, presentations=%+v", dto.HasVideo, dto.Presentations)
	}
	var plexApp core.ResolvedAppPresentation
	for _, ap := range dto.Presentations.Apps {
		if ap.App == "plex" {
			plexApp = ap
		}
	}
	if plexApp.Available || !strings.Contains(plexApp.Reason, "book") {
		t.Fatalf("book plex app = %+v, want unavailable with book reason", plexApp)
	}

	// Attempting to turn Plex ON for Books returns HTTP 400 with plain-language reason.
	bad := call("PATCH", bookPath, `{"presentApps":{"plex":true}}`)
	if bad.Code != 400 || !strings.Contains(bad.Body.String(), "Plex does not have a book library type.") {
		t.Fatalf("enabling plex on book = %d %s, want 400", bad.Code, bad.Body)
	}

	// Turning MediaBrowser OFF saves sticky override and bumps Libraries freshness.
	genBefore := a.browseFreshness().Libraries
	w = call("PATCH", bookPath, `{"presentApps":{"mediaBrowser":false}}`)
	if w.Code != 200 {
		t.Fatalf("PATCH mediaBrowser off = %d %s", w.Code, w.Body)
	}
	if a.browseFreshness().Libraries <= genBefore {
		t.Fatalf("Libraries freshness did not advance after PATCH presentApps")
	}
	stored, err := a.libraries.Get(ctx, bookLib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Present.MediaBrowser != nil || stored.Present.Vela == nil {
		t.Fatalf("book stored present = %+v", stored.Present)
	}
	if stored.PresentOverrides.MediaBrowser == nil || *stored.PresentOverrides.MediaBrowser {
		t.Fatalf("book stored overrides = %+v", stored.PresentOverrides)
	}

	// AdapterOnly toggles all apps off and back while keeping sticky mediaBrowser=false.
	if w := call("PATCH", bookPath, `{"presentApps":{"adapterOnly":true}}`); w.Code != 200 {
		t.Fatalf("PATCH adapterOnly true = %d %s", w.Code, w.Body)
	}
	var adapterDTO libraryDTO
	w = call("GET", bookPath, "")
	if err := json.Unmarshal(w.Body.Bytes(), &adapterDTO); err != nil {
		t.Fatal(err)
	}
	if adapterDTO.HasVideo || adapterDTO.Presentations == nil || !adapterDTO.Presentations.AdapterOnly {
		t.Fatalf("adapter-only book hasVideo=%v, presentations=%+v", adapterDTO.HasVideo, adapterDTO.Presentations)
	}
	stored, _ = a.libraries.Get(ctx, bookLib.ID)
	if !stored.Present.AdapterOnly || stored.Present.Vela != nil {
		t.Fatalf("adapterOnly true present = %+v", stored.Present)
	}
	if w := call("PATCH", bookPath, `{"presentApps":{"adapterOnly":false}}`); w.Code != 200 {
		t.Fatalf("PATCH adapterOnly false = %d %s", w.Code, w.Body)
	}
	stored, _ = a.libraries.Get(ctx, bookLib.ID)
	if stored.Present.AdapterOnly || stored.Present.Vela == nil || stored.Present.MediaBrowser != nil {
		t.Fatalf("adapterOnly false present = %+v (want vela on, mediaBrowser still off)", stored.Present)
	}

	// Turning Plex OFF on a video plugin library persists sticky Plex=false override.
	w = call("PATCH", clipPath, `{"presentApps":{"plex":false}}`)
	if w.Code != 200 {
		t.Fatalf("PATCH clip plex off = %d %s", w.Code, w.Body)
	}
	var clipDTO libraryDTO
	if err := json.Unmarshal(w.Body.Bytes(), &clipDTO); err != nil || !clipDTO.HasVideo {
		t.Fatalf("clip hasVideo=%v, error=%v", clipDTO.HasVideo, err)
	}
	storedClip, err := a.libraries.Get(ctx, clipLib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedClip.Present.Plex != nil || storedClip.PresentOverrides.Plex == nil || *storedClip.PresentOverrides.Plex {
		t.Fatalf("clip library after plex off = present:%+v overrides:%+v", storedClip.Present, storedClip.PresentOverrides)
	}

	// Turning Plex OFF on a published catalogue library immediately removes it from Plex sections.
	customLib, err := a.libraries.Create(ctx, "Studio Picks", core.TypeMovie, nil, "show", core.Presentations{
		Plex: &core.PlexPresentation{Base: "movie"},
	})
	if err != nil {
		t.Fatal(err)
	}
	customPath := "/" + strconv.FormatInt(int64(customLib.ID), 10)
	plexExp, err := a.newSurfaceExperience("plex")
	if err != nil {
		t.Fatal(err)
	}
	srv := plex.NewServer(plex.Deps{Host: frontend.Host{Experience: plexExp, Sessions: sessions.NewLocalRuntime(sessions.LocalRuntimeDeps{})}, DisableEnrichment: true})
	defer srv.Close()

	hasCustom := func() bool {
		for _, s := range srv.LibrarySections() {
			if s.ID == int(customLib.ID) {
				return true
			}
		}
		return false
	}
	if !hasCustom() {
		t.Fatalf("custom library missing from Plex before toggle: %+v", srv.LibrarySections())
	}
	if w := call("PATCH", customPath, `{"presentApps":{"plex":false}}`); w.Code != 200 {
		t.Fatalf("PATCH custom plex off = %d %s", w.Code, w.Body)
	}
	if hasCustom() {
		t.Fatalf("custom library still in Plex after turning Plex switch off: %+v", srv.LibrarySections())
	}
}

func TestLibrariesConsoleFieldValuesAPI(t *testing.T) {
	ctx := context.Background()
	a := setupTestApp(t, true)
	decl := plugins.MediaKindMeta{
		Key:     "book",
		Label:   "Books",
		Library: plugins.KindLibraryDecl{Name: "Books"},
		Levels:  []plugins.KindLevel{{Key: "book", Label: "Book", Playable: true}},
		Fields: []plugins.KindField{
			{Key: "tags", Type: "strings", Label: "Tags", Indexed: true, Values: []string{"Sci-Fi", "Unseen", "Café"}},
		},
		Browse: plugins.KindBrowseDecl{Sorts: []string{"title"}, Facets: []string{"tags"}},
		Sync:   plugins.KindSyncDecl{Mode: plugins.KindSyncPush},
	}
	lib, err := a.libraries.UpsertPluginLibrary(ctx, "demo.books", "book", libraries.PluginLibraryDecl{Name: "Books"})
	if err != nil {
		t.Fatal(err)
	}
	c := &librariesConsole{app: a}
	items, _, err := c.services()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := items.UpsertKind(ctx, "demo.books", lib.ID, decl, false); err != nil {
		t.Fatal(err)
	}
	if _, err := items.UpsertItems(ctx, "demo.books", "book", []core.KindItem{
		{ID: "1", Level: "book", Title: "One", Playable: true, Fields: map[string]json.RawMessage{"tags": json.RawMessage(`["Sci-Fi","Extra"]`)}},
		{ID: "2", Level: "book", Title: "Two", Playable: true, Fields: map[string]json.RawMessage{"tags": json.RawMessage(`["Sci-Fi"]`)}},
	}); err != nil {
		t.Fatal(err)
	}

	h := c.handler()
	get := func(query string) (int, struct {
		Field     string                   `json:"field"`
		Values    []pluginitems.FacetCount `json:"values"`
		More      bool                     `json:"more"`
		Truncated bool                     `json:"truncated"`
	}) {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/"+strconv.FormatInt(int64(lib.ID), 10)+"/facets/tags?"+query, nil)
		h.ServeHTTP(w, r)
		var out struct {
			Field     string                   `json:"field"`
			Values    []pluginitems.FacetCount `json:"values"`
			More      bool                     `json:"more"`
			Truncated bool                     `json:"truncated"`
		}
		if w.Code == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return w.Code, out
	}

	// Count-ordered with declared zero-count values paged 2 at a time.
	code, p1 := get("order=count&declared=1&limit=2&offset=0")
	if code != 200 || !p1.More || len(p1.Values) != 2 || p1.Values[0].Value != "Sci-Fi" || p1.Values[0].Count != 2 || p1.Values[1].Value != "Extra" {
		t.Fatalf("page 1 = %d %+v", code, p1)
	}
	code, p2 := get("order=count&declared=1&limit=2&offset=2")
	if code != 200 || p2.More || len(p2.Values) != 2 || p2.Values[0].Value != "Unseen" || p2.Values[0].Count != 0 || p2.Values[1].Value != "Café" {
		t.Fatalf("page 2 = %d %+v", code, p2)
	}

	// Folded search finds diacritic declared value even before any item has it.
	code, pq := get("order=count&declared=1&q=cafe")
	if code != 200 || pq.More || len(pq.Values) != 1 || pq.Values[0].Value != "Café" || pq.Values[0].Count != 0 {
		t.Fatalf("folded query = %d %+v", code, pq)
	}
}
