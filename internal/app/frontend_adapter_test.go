package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/clientnet"
	"github.com/braedonsaunders/ether/internal/identity"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraries"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/frontendadapter"
	"github.com/braedonsaunders/ether/internal/safety"
)

// TestAdapterCatalogResolvesKindLibraries proves an adapter's declared media
// kind keys resolve to the library ids those kinds created, and that a key
// with no library yet is skipped rather than guessed.
func TestAdapterCatalogResolvesKindLibraries(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if a.libraries == nil {
		t.Skip("no libraries service in this build")
	}
	created, err := a.libraries.UpsertPluginLibrary(ctx, "plug.records", "recording", libraries.PluginLibraryDecl{Name: "Field recordings"})
	if err != nil {
		t.Fatal(err)
	}
	cat := adapterCatalog{app: a}
	ids := cat.resolveLibraries(ctx, "plug.records", []string{"recording"})
	if len(ids) != 1 || ids[0] != created.ID {
		t.Fatalf("resolved %v, want [%d]", ids, created.ID)
	}
	// Unknown kind key contributes nothing.
	if ids := cat.resolveLibraries(ctx, "plug.records", []string{"nope"}); len(ids) != 0 {
		t.Fatalf("unknown kind key resolved to %v", ids)
	}
	// A different plugin's library is not reachable by key alone.
	if ids := cat.resolveLibraries(ctx, "other.plugin", []string{"recording"}); len(ids) != 0 {
		t.Fatalf("cross-plugin key resolved to %v", ids)
	}
	if got := core.PluginLibraryKey("plug.records", "recording"); got != created.Key {
		t.Fatalf("library key = %q, want %q", got, created.Key)
	}
}

// TestAdapterStreamURLRefusesForeignAndUnknown proves the StreamURL backend
// rejects items that do not belong to the calling plugin and items that are
// not stored, before any ticket is minted.
func TestAdapterStreamURLRefusesForeignAndUnknown(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	issuer := frontendadapter.NewTicketIssuer()
	fn := a.adapterStreamURL(issuer)
	if fn == nil {
		t.Skip("no plugin runtime in this build")
	}

	// An item ref from another plugin is refused.
	if _, err := fn(ctx, "plug.mine", 7, "pk:other:kind:item-1"); !errors.Is(err, plugins.ErrPermissionDenied) {
		t.Fatalf("foreign item ref: %v", err)
	}
	// A malformed ref is not found.
	if _, err := fn(ctx, "plug.mine", 7, "not-a-ref"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("malformed ref: %v", err)
	}
	// A missing viewer is refused.
	if _, err := fn(ctx, "plug.mine", 0, "pk:plug.mine:kind:item-1"); !errors.Is(err, plugins.ErrPermissionDenied) {
		t.Fatalf("no viewer: %v", err)
	}
}

func TestAdapterStreamURLOptionsSourceAndBoundedPartIndex(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if a.libraries == nil || a.pluginsRT == nil || a.pluginsRT.pluginitems == nil {
		t.Skip("no libraries or plugin runtime in this build")
	}

	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	viewerID, err := local.CreateViewer(ctx, "ticket-viewer", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}

	const pluginID = "plug.books"
	lib, err := a.libraries.UpsertPluginLibrary(ctx, pluginID, "book", libraries.PluginLibraryDecl{Name: "Books"})
	if err != nil {
		t.Fatal(err)
	}
	decl := plugins.MediaKindMeta{
		Key:     "book",
		Label:   "Books",
		Library: plugins.KindLibraryDecl{Name: "Books"},
		Levels:  []plugins.KindLevel{{Key: "book", Label: "Book", Playable: true}},
		Present: core.Presentations{AdapterOnly: true},
		Sync:    plugins.KindSyncDecl{Mode: plugins.KindSyncPush},
	}
	if _, err := a.pluginsRT.pluginitems.UpsertKind(ctx, pluginID, lib.ID, decl, false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.pluginsRT.pluginitems.UpsertItems(ctx, pluginID, "book", []core.KindItem{
		{ID: "solo", Level: "book", Title: "Solo Book", Playable: true},
		{
			ID: "multi", Level: "book", Title: "Multi Book", Playable: true,
			Hints: json.RawMessage(`{"parts":[{"name":"Part 1"},{"name":"Part 2"}]}`),
		},
	}); err != nil {
		t.Fatal(err)
	}

	issuer := frontendadapter.NewTicketIssuer()
	fn := a.adapterStreamURLOptions(issuer)
	soloRef := core.NewItemRef(pluginID, "book", "solo").String()
	multiRef := core.NewItemRef(pluginID, "book", "multi").String()

	// Playable item with NO declared parts rejects any partIndex at mint time.
	zero := 0
	if _, err := fn(ctx, pluginID, viewerID, soloRef, plugins.StreamURLOptions{PartIndex: &zero}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("partIndex on item with no parts err = %v, want ErrNotFound", err)
	}
	// A source-scoped download ticket carries the source and a host-bounded wait.
	u, err := fn(ctx, pluginID, viewerID, soloRef, plugins.StreamURLOptions{Source: "pdf", Wait: &plugins.StreamWaitPolicy{MaxSeconds: 3600}})
	if err != nil {
		t.Fatalf("source-scoped mint: %v", err)
	}
	tok := u[len(frontendadapter.KindMediaPath):]
	grant, err := issuer.Redeem(tok)
	if err != nil || grant.Source != "pdf" || grant.PartIndex != nil || grant.ItemID != soloRef || grant.Wait != 30*time.Minute {
		t.Fatalf("redeemed source grant = %+v, err = %v", grant, err)
	}

	// Multi-part item accepts valid partIndex + source and rejects out-of-range partIndex.
	one := 1
	u2, err := fn(ctx, pluginID, viewerID, multiRef, plugins.StreamURLOptions{PartIndex: &one, Source: "epub"})
	if err != nil {
		t.Fatalf("part+source mint: %v", err)
	}
	grant2, err := issuer.Redeem(u2[len(frontendadapter.KindMediaPath):])
	if err != nil || grant2.Source != "epub" || grant2.PartIndex == nil || *grant2.PartIndex != 1 {
		t.Fatalf("redeemed part+source grant = %+v, err = %v", grant2, err)
	}
	two := 2
	if _, err := fn(ctx, pluginID, viewerID, multiRef, plugins.StreamURLOptions{PartIndex: &two, Source: "epub"}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("out-of-range partIndex err = %v, want ErrNotFound", err)
	}
}

// Exercise the real controller, registry, listener owner and HTTP mux across
// version/prefix replacement, repeated sync, disable, and re-enable.
func TestAdapterListenerUpgrade(t *testing.T) {
	ctx := context.Background()
	reg := frontend.NewRegistry(func(frontend.Protocol) frontend.Host {
		return frontend.Host{LANViewer: func(context.Context, *http.Request) (*core.Viewer, error) { return &core.Viewer{ID: 7}, nil }}
	})
	control := newFrontendControl(reg, nil)
	a := &App{log: slog.Default(), frontends: &frontendRuntime{control: control}}
	cat := &adapterLifecycleCatalog{decl: frontendadapter.Declaration{PluginID: "upgrade", Version: "1", Meta: plugins.FrontendAdapterMeta{Key: "test", PathPrefix: "/old", Auth: frontendadapter.AuthNoneLAN}}}
	ctrl, err := frontendadapter.NewController(adapterRegistry{Registry: reg, app: a}, lanAdapterManager{}, cat)
	if err != nil {
		t.Fatal(err)
	}
	id := frontendadapter.AdapterID("upgrade", "test")
	if err := ctrl.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	control.Reconcile(ctx, config.Config{Frontends: map[string]config.Frontend{string(id): {Enabled: true}}})
	t.Cleanup(func() { _ = control.listeners.close(ctx) })
	serve := func(path string) int {
		w := httptest.NewRecorder()
		h := control.listeners.sharedHandler(path)
		if h == nil {
			return 404
		}
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w.Code
	}
	if got := serve("/old/file"); got != 200 {
		t.Fatal(got)
	}
	cat.decl.Version = "2"
	cat.decl.Meta.PathPrefix = "/new"
	if err := ctrl.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	control.Reconcile(ctx, config.Config{Frontends: map[string]config.Frontend{string(id): {Enabled: true}}})
	if serve("/old/file") != 404 || serve("/new/file") != 200 {
		t.Fatal("upgrade routes incorrect")
	}
	if err := ctrl.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if serve("/new/file") != 200 {
		t.Fatal("idempotent sync lost mount")
	}
	cat.disabled = true
	if err := ctrl.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if serve("/new/file") != 404 {
		t.Fatal("disabled adapter still mounted")
	}
	cat.disabled = false
	if err := ctrl.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	control.Reconcile(ctx, config.Config{Frontends: map[string]config.Frontend{string(id): {Enabled: true}}})
	if serve("/new/file") != 200 {
		t.Fatal("enable failed")
	}
}

type adapterLifecycleCatalog struct {
	decl     frontendadapter.Declaration
	disabled bool
}

func (c *adapterLifecycleCatalog) EnabledAdapters(context.Context) ([]frontendadapter.Declaration, error) {
	if c.disabled {
		return nil, nil
	}
	return []frontendadapter.Declaration{c.decl}, nil
}

func TestAdapterHostedBlobAuthorization(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	viewerID, err := local.CreateViewer(ctx, "blob-viewer", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	const pluginID = "community.example.blob"
	libraryKey := core.PluginLibraryKey(pluginID, "file")
	lib, err := a.libraries.UpsertPluginLibrary(ctx, pluginID, "file", libraries.PluginLibraryDecl{Name: "Files"})
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	entry, _ := z.Create("firmware.bin")
	_, _ = io.WriteString(entry, "0123456789")
	_ = z.Close()
	ref, err := a.pluginsRT.blobcache.Namespace(libraryKey, blobcache.Quota{}).Put(ctx, "fixture", bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"id":"community.example.blob","version":"1.0.0","name":"Files","description":"Serves test files","ether":{"protocol":1,"kind":"content","summary":"Serves test files","runtime":{"kind":"hosted","entry":"dist/plugin.js"},"capabilities":["catalog_provider","kind_resolver","frontend_adapter"],"contributes":{"mediaKinds":[{"key":"file","label":"Files","library":{"name":"Files"},"levels":[{"key":"file","label":"File","playable":true}],"present":{"plex":{"base":"movie"}},"sync":{"mode":"push"}}],"frontendAdapters":[{"key":"files","label":"Files","pathPrefix":"/files","protocolHint":"custom","auth":"none_lan","libraries":["file"]}]}}}`)
	spec := plugins.FrontendBlob{LibraryKey: libraryKey, Ref: ref.String(), MemberIndex: new(int)}
	response, _ := json.Marshal(plugins.Response{Status: 200, Blob: &spec})
	script := []byte(fmt.Sprintf(`function handle(req) { if(req.path === "/ether/health") return {status:200,body:{status:"ok"}}; if (!req.path.startsWith("/ether/v1/frontend/")) return {status:200,body:{}}; return %s; }`, response))
	preview, err := a.pluginsRT.mgr.PreviewInstall(ctx, plugins.Source{Kind: "studio", Manifest: raw, Files: map[string][]byte{"dist/plugin.js": script}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.pluginsRT.mgr.Install(ctx, preview, plugins.InstallOptions{AcceptAddedPermissions: true}); err != nil {
		t.Fatal(err)
	}
	mgr := adapterManager{Manager: a.pluginsRT.mgr, app: a}
	decl := frontendadapter.Declaration{PluginID: pluginID, Libraries: []core.LibraryID{lib.ID}, Meta: plugins.FrontendAdapterMeta{Key: "files", PathPrefix: "/files", Auth: frontendadapter.AuthNoneLAN}}
	f, err := frontendadapter.New(decl, mgr)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := f.Mount(ctx, frontend.Host{LANViewer: func(context.Context, *http.Request) (*core.Viewer, error) { return a.users.PolicyUser(ctx, viewerID) }})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/files/firmware", nil)
	req.Header.Set("Range", "bytes=2-5")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, req)
	if w.Code != 206 || w.Body.String() != "2345" {
		t.Fatalf("hosted archive response: %d %q", w.Code, w.Body.String())
	}
	assertDenied := func(viewer int64, allowed []core.LibraryID, body plugins.FrontendBlob) {
		t.Helper()
		r, _, err := mgr.OpenFrontendBlob(ctx, pluginID, viewer, allowed, body)
		if err == nil {
			r.Close()
			t.Fatal("unauthorized blob accepted")
		}
	}
	if err := a.libraries.SetHidden(ctx, lib.ID, true); err != nil {
		t.Fatal(err)
	}
	assertDenied(viewerID, []core.LibraryID{lib.ID}, spec)
	if err := a.libraries.SetHidden(ctx, lib.ID, false); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	reader, _, err := mgr.OpenFrontendBlob(cancelled, pluginID, viewerID, []core.LibraryID{lib.ID}, spec)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_, readErr := reader.Read(make([]byte, 1))
	reader.Close()
	if !errors.Is(readErr, context.Canceled) {
		t.Fatalf("cancelled read: %v", readErr)
	}
	assertDenied(viewerID, nil, spec)
	assertDenied(999999, []core.LibraryID{lib.ID}, spec)
	foreign := spec
	foreign.LibraryKey = core.PluginLibraryKey("other.plugin", "file")
	assertDenied(viewerID, []core.LibraryID{lib.ID}, foreign)
	if err := a.pluginsRT.mgr.SetEnabled(ctx, pluginID, false); err != nil {
		t.Fatal(err)
	}
	assertDenied(viewerID, []core.LibraryID{lib.ID}, spec)
}

// TestAdapterKindVisibleAndBrowseRules verifies end-to-end (#615) that a
// frontend adapter calling kinds.visible and kinds.browse sees the exact same
// library Rules, visibility default, discovery-vs-search scope, hidden library,
// viewer library grant, and viewer safety policy as Ether's console and
// protocol projectors, and that non-adapter calls or oversized batches fail.
func TestAdapterKindVisibleAndBrowseRules(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if a.libraries == nil || a.pluginsRT == nil || a.pluginsRT.pluginitems == nil {
		t.Skip("no libraries or plugin runtime in this build")
	}

	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	ownerID, err := local.CreateViewer(ctx, "rules-owner", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	kidID, err := local.CreateViewer(ctx, "rules-kid", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	noGrantID, err := local.CreateViewer(ctx, "rules-nogrant", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}

	const pluginID = "community.example.comics"
	manifest := []byte(`{
	  "id": "community.example.comics",
	  "version": "1.0.0",
	  "name": "Comics",
	  "description": "Comics adapter test",
	  "ether": {
	    "protocol": 1,
	    "kind": "content",
	    "summary": "Comics adapter test",
	    "runtime": { "kind": "hosted", "entry": "dist/plugin.js" },
	    "capabilities": ["catalog_provider", "frontend_adapter"],
	    "contributes": {
	      "mediaKinds": [{
	        "key": "comic",
	        "label": "Comics",
	        "library": { "name": "Comics" },
	        "levels": [{ "key": "series", "label": "Series", "playable": true }],
	        "fields": [{ "key": "tags", "type": "strings", "label": "Tags", "indexed": true }],
	        "present": { "adapterOnly": true },
	        "sync": { "mode": "push" }
	      }],
	      "frontendAdapters": [{
	        "key": "opds",
	        "label": "OPDS",
	        "pathPrefix": "/opds",
	        "protocolHint": "opds",
	        "auth": "none_lan",
	        "libraries": ["comic"]
	      }]
	    }
	  }
	}`)
	script := []byte(`function handle(req) {
	  if (req.path === "/ether/health") return { status: 200, body: { status: "ok" } };
	  if (req.path === "/ether/configure") return { status: 200, body: { errors: {} } };
	  if (req.path === "/ether/view") {
	    ether.kinds.visible("comic", ["c1"]);
	    return { status: 200, body: { ok: true } };
	  }
	  if (req.path === "/ether/v1/frontend/opds/visible") {
	    var ids = ether.kinds.visible("comic", ["c3", "c1", "c2", "missing", "c1"]);
	    return { status: 200, headers: { "Content-Type": "application/json" }, body: { ids: ids } };
	  }
	  if (req.path === "/ether/v1/frontend/opds/browse") {
	    var rawQ = req.query && req.query.q;
	    var q = Array.isArray(rawQ) ? (rawQ[0] || "") : (rawQ || "");
	    var page = ether.kinds.browse("comic", q ? { query: q, limit: 20 } : { level: "series", limit: 20 });
	    return { status: 200, headers: { "Content-Type": "application/json" }, body: { ids: (page.items || []).map(function(it) { return it.id; }) } };
	  }
	  if (req.path === "/ether/v1/frontend/opds/overlimit") {
	    var many = [];
	    for (var i = 0; i < 501; i++) many.push("c" + i);
	    ether.kinds.visible("comic", many);
	    return { status: 200, body: {} };
	  }
	  return { status: 404, body: { error: { code: "not_found", message: "not found" } } };
	}`)

	preview, err := a.pluginsRT.mgr.PreviewInstall(ctx, plugins.Source{
		Kind:     "studio",
		Manifest: manifest,
		Files:    map[string][]byte{"dist/plugin.js": script},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := a.pluginsRT.mgr.Install(ctx, preview, plugins.InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	lib, err := a.libraries.UpsertPluginLibrary(ctx, pluginID, "comic", libraries.PluginLibraryDecl{Name: "Comics"})
	if err != nil {
		t.Fatalf("upsert plugin library: %v", err)
	}
	if _, err := a.pluginsRT.pluginitems.UpsertKind(ctx, pluginID, lib.ID, rec.Manifest.Ether.Contributes.MediaKinds[0], false); err != nil {
		t.Fatalf("upsert kind: %v", err)
	}

	// Upsert three items: c1 (adventure, rating 8), c2 (horror, rating 9), c3 (adventure, rating 6).
	if _, err := a.pluginsRT.pluginitems.UpsertItems(ctx, pluginID, "comic", []core.KindItem{
		{ID: "c1", Level: "series", Title: "Star Quest", Rating: 8.0, Fields: map[string]json.RawMessage{"tags": json.RawMessage(`["Adventure"]`)}},
		{ID: "c2", Level: "series", Title: "Dark Crypt", Rating: 9.0, Fields: map[string]json.RawMessage{"tags": json.RawMessage(`["Horror"]`)}},
		{ID: "c3", Level: "series", Title: "Quiet Meadow", Rating: 6.0, Fields: map[string]json.RawMessage{"tags": json.RawMessage(`["Adventure"]`)}},
	}); err != nil {
		t.Fatal(err)
	}

	// Restrict kid's safety policy (block Horror) and deny noGrant access to the comics library.
	if err := a.safety.Put(ctx, kidID, safety.Policy{BlockedGenres: []string{"Horror"}}); err != nil {
		t.Fatal(err)
	}
	otherLib, err := a.libraries.UpsertPluginLibrary(ctx, "other.plugin", "other", libraries.PluginLibraryDecl{Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	noGrantUser, err := a.users.ActiveUser(ctx, noGrantID)
	if err != nil {
		t.Fatal(err)
	}
	noGrantUser.AllowedLibraries = []core.LibraryID{otherLib.ID}
	if err := a.users.UpdateUser(ctx, *noGrantUser); err != nil {
		t.Fatal(err)
	}

	var activeViewer int64 = ownerID
	mgr := adapterManager{Manager: a.pluginsRT.mgr, app: a}
	decl := frontendadapter.Declaration{
		PluginID:  pluginID,
		Libraries: []core.LibraryID{lib.ID},
		Meta:      plugins.FrontendAdapterMeta{Key: "opds", PathPrefix: "/opds", Auth: frontendadapter.AuthNoneLAN},
	}
	f, err := frontendadapter.New(decl, mgr)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := f.Mount(ctx, frontend.Host{
		LANViewer: func(context.Context, *http.Request) (*core.Viewer, error) {
			return a.users.PolicyUser(ctx, activeViewer)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	callIDs := func(viewer int64, target string) (int, []string) {
		t.Helper()
		activeViewer = viewer
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, httptest.NewRequest("GET", target, nil))
		if w.Code != http.StatusOK {
			return w.Code, nil
		}
		var doc struct {
			IDs []string `json:"ids"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode %s (%q): %v", target, w.Body.String(), err)
		}
		return w.Code, doc.IDs
	}

	// 1) Owner adds a discovery-scoped hide rule for rating >= 9 (hides c2 in browse/visible, still searchable by query).
	discoveryHide := []core.LibraryExclusion{{
		Name:      "Hide 9+ in discovery",
		Action:    "hide",
		Scope:     "discovery",
		MediaType: "mixed",
		Match:     "all",
		Rules:     []core.CollectionRule{{Field: "rating", Op: "gte", Value: json.RawMessage(`9`)}},
	}}
	if err := a.libraries.SetRules(ctx, lib.ID, discoveryHide, "show"); err != nil {
		t.Fatal(err)
	}
	code, ids := callIDs(ownerID, "/opds/visible")
	if code != 200 || fmt.Sprint(ids) != "[c3 c1]" {
		t.Fatalf("hide rule visible: code=%d ids=%v, want [c3 c1]", code, ids)
	}
	code, ids = callIDs(ownerID, "/opds/browse")
	if code != 200 || len(ids) != 2 || ids[0] == "c2" || ids[1] == "c2" {
		t.Fatalf("hide rule browse: code=%d ids=%v (must exclude c2)", code, ids)
	}
	// Explicit search query drops discovery-only hide rule (matching console libraries_console.go).
	code, ids = callIDs(ownerID, "/opds/browse?q=Crypt")
	if code != 200 || fmt.Sprint(ids) != "[c2]" {
		t.Fatalf("discovery search browse: code=%d ids=%v, want [c2]", code, ids)
	}

	// 2) Clear library Rules and verify kid's BlockedGenres safety policy hides c2 (Horror) from BOTH visible and browse/search.
	if err := a.libraries.SetRules(ctx, lib.ID, nil, "show"); err != nil {
		t.Fatal(err)
	}
	code, ids = callIDs(kidID, "/opds/visible")
	if code != 200 || fmt.Sprint(ids) != "[c3 c1]" {
		t.Fatalf("kid safety visible: code=%d ids=%v, want [c3 c1]", code, ids)
	}
	code, ids = callIDs(kidID, "/opds/browse?q=Crypt")
	if code != 200 || len(ids) != 0 {
		t.Fatalf("kid safety search: code=%d ids=%v, want empty", code, ids)
	}

	// 3) visibilityDefault = "hide" with an explicit show rule for rating >= 8 exposes only c1 & c2 (owner) or c1 (kid).
	defaultHide := []core.LibraryExclusion{{
		Name:      "Allow 8+",
		Action:    "show",
		MediaType: "mixed",
		Match:     "all",
		Rules:     []core.CollectionRule{{Field: "rating", Op: "gte", Value: json.RawMessage(`8`)}},
	}}
	if err := a.libraries.SetRules(ctx, lib.ID, defaultHide, "hide"); err != nil {
		t.Fatal(err)
	}
	code, ids = callIDs(ownerID, "/opds/visible")
	if code != 200 || fmt.Sprint(ids) != "[c1 c2]" {
		t.Fatalf("default-hide owner visible: code=%d ids=%v, want [c1 c2]", code, ids)
	}
	code, ids = callIDs(kidID, "/opds/visible")
	if code != 200 || fmt.Sprint(ids) != "[c1]" {
		t.Fatalf("default-hide kid visible: code=%d ids=%v, want [c1]", code, ids)
	}

	// 4) Viewer without access to the library gets empty results (and 403 at adapter middleware if blocked, or direct backend empty).
	visFn := a.adapterKindVisible()
	gotNoGrant, err := visFn(ctx, pluginID, noGrantID, "comic", []string{"c1", "c2", "c3"})
	if err != nil || len(gotNoGrant) != 0 {
		t.Fatalf("noGrant viewer visible = %v, err=%v; want empty", gotNoGrant, err)
	}
	browseFn := a.adapterKindBrowse()
	gotNoGrantPage, err := browseFn(ctx, pluginID, noGrantID, "comic", core.KindBrowseRequest{Limit: 10})
	if err != nil || len(gotNoGrantPage.Items) != 0 {
		t.Fatalf("noGrant viewer browse = %+v, err=%v; want empty", gotNoGrantPage, err)
	}

	// 5) Hidden library returns empty from both visible and browse.
	if err := a.libraries.SetHidden(ctx, lib.ID, true); err != nil {
		t.Fatal(err)
	}
	code, ids = callIDs(ownerID, "/opds/visible")
	if code != 200 || len(ids) != 0 {
		t.Fatalf("hidden library visible: code=%d ids=%v, want empty", code, ids)
	}
	code, ids = callIDs(ownerID, "/opds/browse")
	if code != 200 || len(ids) != 0 {
		t.Fatalf("hidden library browse: code=%d ids=%v, want empty", code, ids)
	}
	if err := a.libraries.SetHidden(ctx, lib.ID, false); err != nil {
		t.Fatal(err)
	}

	// 6) Batch > 500 IDs fails.
	code, _ = callIDs(ownerID, "/opds/overlimit")
	if code == http.StatusOK {
		t.Fatal("oversized kinds.visible batch returned 200")
	}

	// 7) Non-adapter call to kinds.visible fails even when X-Ether-Viewer header is forged.
	if _, err := a.pluginsRT.mgr.Call(ctx, pluginID, plugins.Request{
		Method:  "GET",
		Path:    "/ether/view",
		Headers: map[string]string{"X-Ether-Viewer": fmt.Sprint(ownerID)},
	}); err == nil {
		t.Fatal("non-adapter route calling kinds.visible succeeded")
	}

	// 8) kinds.browse total modes: default ("cheap") returns O(1) tallies and
	// omits Total for un-tallied multi-page filters; "none" omits Total even on
	// unfiltered queries; "exact" computes Total on demand (#682).
	if err := a.libraries.SetRules(ctx, lib.ID, nil, "show"); err != nil {
		t.Fatal(err)
	}
	pUnfiltered, err := browseFn(ctx, pluginID, ownerID, "comic", core.KindBrowseRequest{Level: "series", Limit: 1})
	if err != nil || pUnfiltered.Total == nil || *pUnfiltered.Total != 3 {
		t.Fatalf("default cheap unfiltered Total = %v, err=%v; want 3", pUnfiltered.Total, err)
	}
	pSingleFacet, err := browseFn(ctx, pluginID, ownerID, "comic", core.KindBrowseRequest{
		Level:  "series",
		Facets: map[string][]string{"tags": {"Adventure"}},
		Limit:  1,
	})
	if err != nil || pSingleFacet.Total == nil || *pSingleFacet.Total != 2 {
		t.Fatalf("default cheap single-facet Total = %v, err=%v; want 2", pSingleFacet.Total, err)
	}
	pMultiFacetCheap, err := browseFn(ctx, pluginID, ownerID, "comic", core.KindBrowseRequest{
		Level:  "series",
		Facets: map[string][]string{"tags": {"Adventure", "Horror"}},
		Limit:  1,
	})
	if err != nil || pMultiFacetCheap.Total != nil {
		t.Fatalf("default cheap multi-value strings multi-page Total = %v, err=%v; want nil", pMultiFacetCheap.Total, err)
	}
	pNone, err := browseFn(ctx, pluginID, ownerID, "comic", core.KindBrowseRequest{
		Level: "series",
		Limit: 1,
		Total: "none",
	})
	if err != nil || pNone.Total != nil {
		t.Fatalf("total=none Total = %v, err=%v; want nil", pNone.Total, err)
	}
	pExact, err := browseFn(ctx, pluginID, ownerID, "comic", core.KindBrowseRequest{
		Level:  "series",
		Facets: map[string][]string{"tags": {"Adventure", "Horror"}},
		Limit:  1,
		Total:  "exact",
	})
	if err != nil || pExact.Total == nil || *pExact.Total != 3 {
		t.Fatalf("total=exact multi-value strings Total = %v, err=%v; want 3", pExact.Total, err)
	}
}

// TestAdapterOriginHeaderAndAbsoluteStreamURL verifies (#772) that frontend
// adapters receive X-Ether-Origin computed with trusted proxies only (stripping
// any client-supplied X-Ether-Origin and ignoring spoofed forwarding headers
// from untrusted peers), and that frontend.streamURL(item, { absolute: true })
// prepends that origin or falls back to the adapter's first path-prefixed
// published address while keeping the default root-relative.
func TestAdapterOriginHeaderAndAbsoluteStreamURL(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if a.libraries == nil || a.pluginsRT == nil || a.pluginsRT.pluginitems == nil {
		t.Skip("no libraries or plugin runtime in this build")
	}

	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	viewerID, err := local.CreateViewer(ctx, "origin-viewer", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}

	const pluginID = "community.example.kodi"
	const adapterKey = "kodi"
	adapterID := string(frontendadapter.AdapterID(pluginID, adapterKey))

	if err := a.cfg.Update(func(c *config.Config) error {
		c.Server.TrustedProxyCIDRs = []string{"10.0.0.5/32"}
		if c.Frontends == nil {
			c.Frontends = map[string]config.Frontend{}
		}
		c.Frontends[adapterID] = config.Frontend{
			Enabled:            true,
			PublishedAddresses: []string{"https://pub.example.com/ether/"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	manifest := []byte(`{
	  "id": "community.example.kodi",
	  "version": "1.0.0",
	  "name": "Kodi Bridge",
	  "description": "Origin and absolute stream URL test",
	  "ether": {
	    "protocol": 1,
	    "kind": "content",
	    "summary": "Origin and absolute stream URL test",
	    "runtime": { "kind": "hosted", "entry": "dist/plugin.js" },
	    "capabilities": ["catalog_provider", "frontend_adapter"],
	    "contributes": {
	      "mediaKinds": [{
	        "key": "video",
	        "label": "Videos",
	        "library": { "name": "Videos" },
	        "levels": [{ "key": "video", "label": "Video", "playable": true }],
	        "present": { "adapterOnly": true },
	        "sync": { "mode": "push" }
	      }],
	      "frontendAdapters": [{
	        "key": "kodi",
	        "label": "Kodi",
	        "pathPrefix": "/kodi",
	        "protocolHint": "custom",
	        "auth": "none_lan",
	        "libraries": ["video"]
	      }]
	    }
	  }
	}`)
	itemRef := core.NewItemRef(pluginID, "video", "clip-1").String()
	script := []byte(fmt.Sprintf(`function handle(req) {
	  if (req.path === "/ether/health") return { status: 200, body: { status: "ok" } };
	  if (req.path === "/ether/configure") return { status: 200, body: { errors: {} } };
	  if (req.path === "/ether/v1/frontend/kodi/play") {
	    return {
	      status: 200,
	      headers: { "Content-Type": "application/json" },
	      body: {
	        origin: (req.headers && req.headers["X-Ether-Origin"]) || "",
	        rel: ether.frontend.streamURL(%q).url,
	        abs: ether.frontend.streamURL(%q, { absolute: true }).url
	      }
	    };
	  }
	  return { status: 404, body: { error: { code: "not_found", message: "not found" } } };
	}`, itemRef, itemRef))

	preview, err := a.pluginsRT.mgr.PreviewInstall(ctx, plugins.Source{
		Kind:     "studio",
		Manifest: manifest,
		Files:    map[string][]byte{"dist/plugin.js": script},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := a.pluginsRT.mgr.Install(ctx, preview, plugins.InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	lib, err := a.libraries.UpsertPluginLibrary(ctx, pluginID, "video", libraries.PluginLibraryDecl{Name: "Videos"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.pluginsRT.pluginitems.UpsertKind(ctx, pluginID, lib.ID, rec.Manifest.Ether.Contributes.MediaKinds[0], false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.pluginsRT.pluginitems.UpsertItems(ctx, pluginID, "video", []core.KindItem{
		{ID: "clip-1", Level: "video", Title: "Clip 1", Playable: true},
	}); err != nil {
		t.Fatal(err)
	}

	mgr := adapterManager{Manager: a.pluginsRT.mgr, app: a}
	decl := frontendadapter.Declaration{
		PluginID:  pluginID,
		Libraries: []core.LibraryID{lib.ID},
		Meta:      plugins.FrontendAdapterMeta{Key: adapterKey, PathPrefix: "/kodi", Auth: frontendadapter.AuthNoneLAN},
	}
	f, err := frontendadapter.New(decl, mgr)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := f.Mount(ctx, frontend.Host{
		LANViewer: func(context.Context, *http.Request) (*core.Viewer, error) {
			return a.users.PolicyUser(ctx, viewerID)
		},
		RequestOrigin: func(req *http.Request) (clientnet.RequestOrigin, bool) {
			return clientnet.Origin(req, a.trustedProxyPrefixes()...)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	type playReply struct {
		Origin string `json:"origin"`
		Rel    string `json:"rel"`
		Abs    string `json:"abs"`
	}
	call := func(req *http.Request) playReply {
		t.Helper()
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
		var out playReply
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
		return out
	}

	// 1. Trusted proxy: honours X-Forwarded-Proto/-Host/-Prefix and strips inbound X-Ether-Origin.
	trustedReq := httptest.NewRequest(http.MethodGet, "http://172.17.0.2:32450/kodi/play", nil)
	trustedReq.RemoteAddr = "10.0.0.5:43210"
	trustedReq.Header.Set("X-Forwarded-Proto", "https")
	trustedReq.Header.Set("X-Forwarded-Host", "media.example.com")
	trustedReq.Header.Set("X-Forwarded-Prefix", "/sub/")
	trustedReq.Header.Set("X-Ether-Origin", "https://spoof.invalid")
	got := call(trustedReq)
	if got.Origin != "https://media.example.com/sub" {
		t.Fatalf("trusted origin = %q, want https://media.example.com/sub", got.Origin)
	}
	if !strings.HasPrefix(got.Rel, frontendadapter.KindMediaPath) {
		t.Fatalf("default streamURL = %q, want root-relative %s...", got.Rel, frontendadapter.KindMediaPath)
	}
	wantTrustedPrefix := "https://media.example.com/sub" + frontendadapter.KindMediaPath
	if !strings.HasPrefix(got.Abs, wantTrustedPrefix) {
		t.Fatalf("trusted absolute streamURL = %q, want prefix %q", got.Abs, wantTrustedPrefix)
	}
	if grant, err := a.pluginsRT.adapterTickets.Redeem(strings.TrimPrefix(got.Abs, wantTrustedPrefix)); err != nil || grant.ItemID != itemRef || grant.ViewerID != viewerID {
		t.Fatalf("redeem trusted ticket = %+v, err = %v", grant, err)
	}

	// 2. Untrusted spoof: ignores X-Forwarded-* and strips inbound X-Ether-Origin.
	spoofReq := httptest.NewRequest(http.MethodGet, "http://192.168.1.20:32450/kodi/play", nil)
	spoofReq.RemoteAddr = "203.0.113.9:55000"
	spoofReq.Header.Set("X-Forwarded-Proto", "https")
	spoofReq.Header.Set("X-Forwarded-Host", "evil.example.com")
	spoofReq.Header.Set("X-Forwarded-Prefix", "/evil")
	spoofReq.Header.Set("X-Ether-Origin", "https://spoof.invalid")
	got = call(spoofReq)
	if got.Origin != "http://192.168.1.20:32450" {
		t.Fatalf("untrusted origin = %q, want http://192.168.1.20:32450", got.Origin)
	}
	if wantPrefix := "http://192.168.1.20:32450" + frontendadapter.KindMediaPath; !strings.HasPrefix(got.Abs, wantPrefix) {
		t.Fatalf("untrusted absolute streamURL = %q, want prefix %q", got.Abs, wantPrefix)
	}

	// 3. Container bridge Host with no trusted forwarding headers: omits X-Ether-Origin
	// and falls back to the adapter's first path-prefixed published address.
	bridgeReq := httptest.NewRequest(http.MethodGet, "http://172.17.0.2:32450/kodi/play", nil)
	bridgeReq.RemoteAddr = "192.168.1.50:55001"
	bridgeReq.Header.Set("X-Ether-Origin", "https://spoof.invalid")
	got = call(bridgeReq)
	if got.Origin != "" {
		t.Fatalf("container bridge origin = %q, want empty", got.Origin)
	}
	if wantPrefix := "https://pub.example.com/ether" + frontendadapter.KindMediaPath; !strings.HasPrefix(got.Abs, wantPrefix) {
		t.Fatalf("published fallback streamURL = %q, want prefix %q", got.Abs, wantPrefix)
	}
}
