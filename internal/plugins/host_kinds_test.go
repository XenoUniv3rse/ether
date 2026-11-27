package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
)

func installContent(t *testing.T, env *testEnv) Record {
	t.Helper()
	man := contentManifest()
	man.Ether.Contributes.MediaKinds[0].Sync = KindSyncDecl{Mode: KindSyncPush}
	raw, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	p, err := env.m.PreviewInstall(context.Background(), Source{Kind: "studio", Manifest: raw, Files: map[string][]byte{"dist/plugin.js": []byte("// library")}})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := env.m.Install(context.Background(), p, InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// TestHostStreamURL exercises the frontend adapter StreamURL host service:
// it requires the frontend_adapter capability, an authenticated viewer on
// the call context, and delegates to the backend which enforces item
// ownership and library visibility.
func TestHostStreamURL(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	man := contentManifest()
	man.Ether.Contributes.MediaKinds[0].Sync = KindSyncDecl{Mode: KindSyncPush}
	man.Ether.Capabilities = append(man.Ether.Capabilities, CapFrontendAdapter)
	man.Ether.Contributes.FrontendAdapters = []FrontendAdapterMeta{
		{Key: "feed", Label: "Records feed", ProtocolHint: "opds", PathPrefix: "/opds", Libraries: []string{"recording"}},
	}
	raw, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	p, err := env.m.PreviewInstall(ctx, Source{Kind: "studio", Manifest: raw, Files: map[string][]byte{"dist/plugin.js": []byte("// library")}})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := env.m.Install(ctx, p, InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	h := env.m.HostFor(rec.ID)

	var gotViewer int64
	var gotItem string
	env.m.SetKindBackends(KindBackends{
		StreamURL: func(_ context.Context, pluginID string, viewerID int64, itemRef string) (string, error) {
			if pluginID != rec.ID {
				t.Errorf("backend plugin scope: %s", pluginID)
			}
			gotViewer, gotItem = viewerID, itemRef
			return "/ether/v1/kind-media/ticket-1", nil
		},
	})

	// No viewer on the context: refused.
	if _, err := h.StreamURL(ctx, "ref"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("stream url without viewer: %v", err)
	}
	// With a viewer, the backend mints the URL.
	viewerCtx := WithViewer(ctx, 7)
	u, err := h.StreamURL(viewerCtx, "pk:recording:trk-1")
	if err != nil || u != "/ether/v1/kind-media/ticket-1" {
		t.Fatalf("stream url: %q %v", u, err)
	}
	if gotViewer != 7 || gotItem != "pk:recording:trk-1" {
		t.Fatalf("backend got viewer=%d item=%q", gotViewer, gotItem)
	}
	// A capability the owner switched off denies the call.
	if _, err := env.m.SetRoles(ctx, rec.ID, []Capability{CapFrontendAdapter}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.StreamURL(viewerCtx, "pk:recording:trk-1"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("stream url without adapter capability: %v", err)
	}
}

func TestHostKindServices(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	rec := installContent(t, env)
	h := env.m.HostFor(rec.ID)

	if got, want := core.PluginLibraryKey(rec.ID, "recording"), "pk-community-example-library-recording"; got != want {
		t.Fatalf("PluginLibraryKey(%q, %q) = %q, want %q", rec.ID, "recording", got, want)
	}

	// No backend wired yet: granted calls surface ErrUnavailable, not a panic.
	item := core.KindItem{ID: "trk-1", Level: "track", ParentID: "alb-1", Title: "Dawn chorus", Playable: true}
	if _, err := h.KindUpsert(ctx, "recording", []core.KindItem{item}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("upsert without backend: %v", err)
	}
	// An undeclared kind and a bad envelope are refused before any backend.
	if _, err := h.KindUpsert(ctx, "book", []core.KindItem{item}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("undeclared kind: %v", err)
	}
	bad := item
	bad.ID = "has:colon"
	if _, err := h.KindUpsert(ctx, "recording", []core.KindItem{bad}); err == nil {
		t.Fatal("bad external id accepted")
	}
	bad = item
	bad.ParentID = ""
	if _, err := h.KindUpsert(ctx, "recording", []core.KindItem{bad}); err == nil {
		t.Fatal("leaf without parent accepted")
	}

	var gotItems []core.KindItem
	env.m.SetKindBackends(KindBackends{
		KindUpsert: func(_ context.Context, pluginID, kindKey string, items []core.KindItem) (int, error) {
			if pluginID != rec.ID || kindKey != "recording" {
				t.Errorf("backend scope: %s %s", pluginID, kindKey)
			}
			gotItems = items
			return len(items), nil
		},
		KindItems: func(_ context.Context, _, _ string, _ string, limit int) (core.KindPage, error) {
			return core.KindPage{Items: gotItems}, nil
		},
		ResolveSources: func(_ context.Context, _ string, q core.SourceQuery) ([]core.SourceCandidate, error) {
			return []core.SourceCandidate{{Title: q.Query, Cached: true}}, nil
		},
		CachePut: func(_ context.Context, _, libraryKey, _ string, r io.Reader) (string, error) {
			b, err := io.ReadAll(r)
			return "blob:" + strings.Repeat("a", 64), errFrom(b, err)
		},
	})
	if n, err := h.KindUpsert(ctx, "recording", []core.KindItem{item}); err != nil || n != 1 {
		t.Fatalf("push upsert: %d %v", n, err)
	}
	if page, err := h.KindItems(ctx, "recording", "", 0); err != nil || len(page.Items) != 1 {
		t.Fatalf("read back: %+v %v", page, err)
	}
	// sources: search is granted, acquire is not.
	if _, err := h.ResolveSources(ctx, core.SourceQuery{Kind: "indexer_search", Query: "dawn"}); err != nil {
		t.Fatalf("source search: %v", err)
	}
	if _, err := h.ResolveSources(ctx, core.SourceQuery{Kind: "release_files", TorrentURL: "https://tracker.example/album.html"}); err != nil {
		t.Fatalf("release_files with torrentUrl: %v", err)
	}
	if _, err := h.ResolveSources(ctx, core.SourceQuery{Kind: "debrid_acquire", Magnet: "magnet:?xt=x"}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("acquire beyond the search grant: %v", err)
	}
	if _, err := h.ResolveSources(ctx, core.SourceQuery{Kind: "debrid_acquire", TorrentURL: "https://tracker.example/album.torrent"}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("debrid_acquire with torrentUrl beyond search grant: %v", err)
	}
	// The cache namespace must be one of this plugin's own libraries.
	if _, err := h.CachePut(ctx, "pk-community-example-other-book", "cover", strings.NewReader("x")); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("foreign cache namespace: %v", err)
	}
	if ref, err := h.CachePut(ctx, core.PluginLibraryKey(rec.ID, "recording"), "cover", strings.NewReader("x")); err != nil || !strings.HasPrefix(ref, "blob:") {
		t.Fatalf("own cache put: %q %v", ref, err)
	}
	if _, err := h.CacheOpen(ctx, "blob:not-a-hash"); err == nil {
		t.Fatal("malformed blob reference accepted")
	}
	// Switching the resolver role off denies source resolution.
	if _, err := env.m.SetRoles(ctx, rec.ID, []Capability{CapKindResolver}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ResolveSources(ctx, core.SourceQuery{Kind: "indexer_search", Query: "dawn"}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("role off should deny: %v", err)
	}
	for _, q := range []core.SourceQuery{
		{Kind: "indexer_search_results", SearchID: "srch_1"},
		{Kind: "indexer_search_cancel", SearchID: "srch_1"},
		{Kind: "indexers"},
	} {
		if _, err := h.ResolveSources(ctx, q); !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("role off should deny %s: %v", q.Kind, err)
		}
	}
}

func TestKindJournalAndUndo(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	rec := installContent(t, env)
	h := env.m.HostFor(rec.ID)

	const baselineCount = 50_005
	store := make(map[string]core.KindItem, baselineCount+2)
	for i := 0; i < baselineCount; i++ {
		id := fmt.Sprintf("trk-%05d", i)
		store[id] = core.KindItem{
			ID:       id,
			Level:    "track",
			ParentID: "alb-1",
			Title:    fmt.Sprintf("Track %05d", i),
			Playable: true,
		}
	}
	env.m.SetKindBackends(KindBackends{
		KindUpsert: func(_ context.Context, _, _ string, items []core.KindItem) (int, error) {
			for _, it := range items {
				store[it.ID] = it
			}
			return len(items), nil
		},
		KindDelete: func(_ context.Context, _, _ string, ids []string) error {
			for _, id := range ids {
				delete(store, id)
			}
			return nil
		},
		KindItemsByID: func(_ context.Context, _, _ string, ids []string) ([]core.KindItem, error) {
			out := make([]core.KindItem, 0, len(ids))
			for _, id := range ids {
				if it, ok := store[id]; ok {
					out = append(out, it)
				}
			}
			return out, nil
		},
	})

	// Upsert two new items and update an existing item past the 50k mark (#607).
	item1 := core.KindItem{ID: "trk-90001", Level: "track", ParentID: "alb-1", Title: "Dawn chorus", Playable: true}
	item2 := core.KindItem{ID: "trk-90002", Level: "track", ParentID: "alb-1", Title: "Evening rain", Playable: true}
	tailUpdated := store["trk-50004"]
	tailUpdated.Title = "Track 50004 (remastered)"
	if _, err := h.KindUpsert(ctx, "recording", []core.KindItem{item1, item2, tailUpdated}); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}
	// Update item1 and delete both item2 and a pre-existing item past 50k.
	item1Updated := item1
	item1Updated.Title = "Dawn chorus (remastered)"
	if _, err := h.KindUpsert(ctx, "recording", []core.KindItem{item1Updated}); err != nil {
		t.Fatalf("update upsert: %v", err)
	}
	if err := h.KindDelete(ctx, "recording", []string{"trk-90002", "trk-50003"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(store) != baselineCount || store["trk-90001"].Title != "Dawn chorus (remastered)" || store["trk-50004"].Title != "Track 50004 (remastered)" {
		t.Fatalf("unexpected store before undo: len=%d trk-90001=%+v trk-50004=%+v", len(store), store["trk-90001"], store["trk-50004"])
	}

	// UndoAll applies inverses in reverse order: restores trk-90002 and trk-50003,
	// reverts trk-90001 and trk-50004, and deletes newly created trk-90001 and trk-90002.
	undone, err := env.m.UndoAll(ctx, rec.ID)
	if err != nil || undone != 3 {
		t.Fatalf("UndoAll = %d, err = %v", undone, err)
	}
	if len(store) != baselineCount {
		t.Fatalf("store len after UndoAll = %d, want %d", len(store), baselineCount)
	}
	if _, ok := store["trk-90001"]; ok {
		t.Fatalf("trk-90001 remained after UndoAll: %+v", store["trk-90001"])
	}
	if _, ok := store["trk-90002"]; ok {
		t.Fatalf("trk-90002 remained after UndoAll: %+v", store["trk-90002"])
	}
	if got := store["trk-50003"].Title; got != "Track 50003" {
		t.Fatalf("trk-50003 after UndoAll = %q, want Track 50003", got)
	}
	if got := store["trk-50004"].Title; got != "Track 50004" {
		t.Fatalf("trk-50004 after UndoAll = %q, want Track 50004", got)
	}
}

// #766: KindUpsertWithResult through KindUpsertUndo skips empty/unchanged
// journal entries, and a kind declaring sync.undo: "ids" skips snapshot reads
// while undo removes only items created by the batch.
func TestKindUpsertUndoUnchangedSkipAndIDsMode(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	man := contentManifest()
	man.Ether.Contributes.MediaKinds[0].Sync = KindSyncDecl{Mode: KindSyncPush}
	idsKind := man.Ether.Contributes.MediaKinds[0]
	idsKind.Key = "fast"
	idsKind.Library.Name = "Fast catalog"
	idsKind.Sync = KindSyncDecl{Mode: KindSyncPush, Undo: KindSyncUndoIDs}
	man.Ether.Contributes.MediaKinds = append(man.Ether.Contributes.MediaKinds, idsKind)

	raw, _ := json.Marshal(man)
	prev, err := env.m.PreviewInstall(ctx, Source{Kind: "studio", Manifest: raw, Files: map[string][]byte{"dist/plugin.js": []byte("// v1")}})
	if err != nil {
		t.Fatalf("PreviewInstall: %v", err)
	}
	rec, err := env.m.Install(ctx, prev, InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	h := env.m.HostFor(rec.ID)

	itemsByKind := map[string]map[string]core.KindItem{
		"recording": {},
		"fast":      {"trk-pre": {ID: "trk-pre", Level: "track", ParentID: "alb-1", Title: "Pre-existing", Playable: true}},
	}
	var snapshotFlags []bool
	var snapshotReadCalls int
	env.m.SetKindBackends(KindBackends{
		KindUpsert: func(_ context.Context, _, kindKey string, items []core.KindItem) (int, error) {
			for _, it := range items {
				itemsByKind[kindKey][it.ID] = it
			}
			return len(items), nil
		},
		KindUpsertUndo: func(_ context.Context, _, kindKey string, items []core.KindItem, snapshotPrevious bool) (KindUpsertUndo, error) {
			snapshotFlags = append(snapshotFlags, snapshotPrevious)
			m := itemsByKind[kindKey]
			var out KindUpsertUndo
			out.Result.Accepted = len(items)
			for _, it := range items {
				prev, ok := m[it.ID]
				switch {
				case !ok:
					out.Result.Added++
					out.AddedIDs = append(out.AddedIDs, it.ID)
				case prev.Title == it.Title:
					out.Result.Unchanged++
					continue
				default:
					out.Result.Updated++
					out.UpdatedIDs = append(out.UpdatedIDs, it.ID)
					if snapshotPrevious {
						out.Previous = append(out.Previous, prev)
					}
				}
				m[it.ID] = it
			}
			return out, nil
		},
		KindDelete: func(_ context.Context, _, kindKey string, ids []string) error {
			for _, id := range ids {
				delete(itemsByKind[kindKey], id)
			}
			return nil
		},
		KindItemsByID: func(_ context.Context, _, _ string, _ []string) ([]core.KindItem, error) {
			snapshotReadCalls++
			return nil, nil
		},
	})

	trk1 := core.KindItem{ID: "trk-1", Level: "track", ParentID: "alb-1", Title: "Original", Playable: true}
	if _, err := h.KindUpsert(ctx, "recording", []core.KindItem{trk1}); err != nil {
		t.Fatalf("recording initial upsert: %v", err)
	}
	// Identical re-publish must write no journal entry.
	if _, err := h.KindUpsert(ctx, "recording", []core.KindItem{trk1}); err != nil {
		t.Fatalf("recording unchanged upsert: %v", err)
	}
	entries, err := env.m.store.journalList(ctx, rec.ID, 10, true)
	if err != nil || len(entries) != 1 {
		t.Fatalf("live journal entries after unchanged republish = %d, %v; want 1", len(entries), err)
	}

	// sync.undo: "ids" skips snapshot reads and records created vs touched IDs.
	trkPreUpdated := core.KindItem{ID: "trk-pre", Level: "track", ParentID: "alb-1", Title: "Updated", Playable: true}
	trkNew := core.KindItem{ID: "trk-new", Level: "track", ParentID: "alb-1", Title: "Created", Playable: true}
	if _, err := h.KindUpsert(ctx, "fast", []core.KindItem{trkPreUpdated, trkNew}); err != nil {
		t.Fatalf("fast upsert: %v", err)
	}
	if err := h.KindDelete(ctx, "fast", []string{"trk-missing"}); err != nil {
		t.Fatalf("fast delete: %v", err)
	}
	if snapshotReadCalls != 0 {
		t.Fatalf("KindItemsByID called %d times, want 0", snapshotReadCalls)
	}
	if len(snapshotFlags) != 3 || !snapshotFlags[0] || !snapshotFlags[1] || snapshotFlags[2] {
		t.Fatalf("snapshotPrevious flags = %v, want [true true false]", snapshotFlags)
	}
	entries, err = env.m.store.journalList(ctx, rec.ID, 10, true)
	if err != nil || len(entries) != 3 {
		t.Fatalf("live journal entries = %d, %v; want 3", len(entries), err)
	}
	var idsInv kindInverse
	if err := json.Unmarshal(entries[1].Inverse, &idsInv); err != nil {
		t.Fatal(err)
	}
	if len(idsInv.Previous) != 0 || len(idsInv.Items) != 1 || idsInv.Items[0] != "trk-new" || len(idsInv.Touched) != 1 || idsInv.Touched[0] != "trk-pre" {
		t.Fatalf("ids inverse = %+v, want items=[trk-new] touched=[trk-pre] previous=[]", idsInv)
	}

	// UndoAll removes created items (trk-1, trk-new) and leaves touched trk-pre intact.
	undone, err := env.m.UndoAll(ctx, rec.ID)
	if err != nil || undone != 3 {
		t.Fatalf("UndoAll = %d, %v; want 3, nil", undone, err)
	}
	if _, ok := itemsByKind["recording"]["trk-1"]; ok {
		t.Fatal("trk-1 remained after UndoAll")
	}
	if _, ok := itemsByKind["fast"]["trk-new"]; ok {
		t.Fatal("trk-new remained after UndoAll")
	}
	if got := itemsByKind["fast"]["trk-pre"].Title; got != "Updated" {
		t.Fatalf("trk-pre after ids-mode UndoAll = %q, want Updated", got)
	}
}

func TestInstallMediaKindsUpgradeGateAndPurgeSetup(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	rec := installContent(t, env)

	// Record a setup step row so Remove can verify plugins_setup purge.
	env.fake.routes["/ether/v1/setup/"] = func(req Request) (Response, error) {
		return Response{Status: 200, Body: json.RawMessage(`{"ok":true}`)}, nil
	}
	if _, err := env.m.TestSetupStep(ctx, rec.ID, "connect"); err != nil {
		t.Fatalf("TestSetupStep: %v", err)
	}

	// Non-additive upgrade (removing a field) is refused on update unless ReplaceLibraries is set.
	shrunk := contentManifest()
	shrunk.Version = "1.1.0"
	shrunk.Ether.Contributes.MediaKinds[0].Sync = KindSyncDecl{Mode: KindSyncPush}
	shrunk.Ether.Contributes.MediaKinds[0].Fields = shrunk.Ether.Contributes.MediaKinds[0].Fields[:2]
	rawShrunk, _ := json.Marshal(shrunk)
	pShrunk, err := env.m.PreviewInstall(ctx, Source{Kind: "studio", Manifest: rawShrunk, Files: map[string][]byte{"dist/plugin.js": []byte("// v1.1")}})
	if err != nil {
		t.Fatalf("PreviewInstall shrunk: %v", err)
	}
	if _, err := env.m.Install(ctx, pShrunk, InstallOptions{AcceptAddedPermissions: true}); err == nil {
		t.Fatal("expected non-additive kind upgrade to be refused without ReplaceLibraries")
	}
	if saved, err := env.m.Install(ctx, pShrunk, InstallOptions{AcceptAddedPermissions: true, ReplaceLibraries: true}); err != nil || !saved.ReplaceLibraries {
		t.Fatalf("upgrade with ReplaceLibraries: approved=%t err=%v", saved.ReplaceLibraries, err)
	}

	// Purge removes plugins_setup rows.
	if err := env.m.Remove(ctx, rec.ID, true); err != nil {
		t.Fatalf("Remove purge: %v", err)
	}
	state, err := env.m.store.setupState(ctx, rec.ID)
	if err != nil || len(state) != 0 {
		t.Fatalf("setup rows after purge: %+v %v", state, err)
	}
}

func TestHostedRuntimeKindAndCacheBridges(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	eng := wasm.NewEngine(nil)
	t.Cleanup(func() { _ = eng.Close() })
	env.m.RegisterRuntime("hosted", HostedRuntimeFactory(eng))

	var upserted []core.KindItem
	var deleted []string
	blobs := map[string][]byte{}
	env.m.SetKindBackends(KindBackends{
		KindUpsert: func(_ context.Context, _, _ string, items []core.KindItem) (int, error) {
			upserted = append(upserted, items...)
			return len(items), nil
		},
		KindDelete: func(_ context.Context, _, _ string, ids []string) error {
			deleted = append(deleted, ids...)
			return nil
		},
		KindItems: func(_ context.Context, _, _, _ string, _ int) (core.KindPage, error) {
			return core.KindPage{Items: upserted}, nil
		},
		ResolveSources: func(_ context.Context, _ string, q core.SourceQuery) ([]core.SourceCandidate, error) {
			return []core.SourceCandidate{{InfoHash: "c1", Title: q.Query, Cached: true}}, nil
		},
		CachePut: func(_ context.Context, _, _, _ string, r io.Reader) (string, error) {
			b, _ := io.ReadAll(r)
			ref := "blob:" + strings.Repeat("b", 64)
			blobs[ref] = b
			return ref, nil
		},
		CacheOpen: func(_ context.Context, _, ref string) (io.ReadSeekCloser, error) {
			return struct {
				*bytes.Reader
				io.Closer
			}{Reader: bytes.NewReader(blobs[ref]), Closer: io.NopCloser(nil)}, nil
		},
	})

	man := contentManifest()
	man.Ether.Contributes.MediaKinds[0].Sync = KindSyncDecl{Mode: KindSyncPush}
	man.Ether.Contributes.Setup = nil
	raw, _ := json.Marshal(man)
	js := `
function handle(req) {
  const up = ether.kinds.upsert("recording", [{ id: "trk-1", level: "track", parentId: "alb-1", title: "Chorus", playable: true }]);
  const page = ether.kinds.items("recording", "", 10);
  ether.kinds.delete("recording", ["trk-1"]);
  const src = ether.sources.resolve({ kind: "indexer_search", query: "chorus" });
  const put = ether.cache.put("pk-community-example-library-recording", "note.txt", "hello-blob");
  const opened = ether.cache.open(put.blob);
  return { status: 200, body: JSON.stringify({ accepted: up.accepted, count: page.items.length, cand: src.candidates[0].title, body: opened.body }) };
}
`
	p, err := env.m.PreviewInstall(ctx, Source{Kind: "studio", Manifest: raw, Files: map[string][]byte{"dist/plugin.js": []byte(js)}})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := env.m.Install(ctx, p, InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := env.m.Call(ctx, rec.ID, Request{Method: "POST", Path: "/ether/v1/kinds/recording/sync"})
	if err != nil || resp.Status != 200 {
		t.Fatalf("hosted call: %+v %v", resp, err)
	}
	var body struct {
		Accepted int    `json:"accepted"`
		Count    int    `json:"count"`
		Cand     string `json:"cand"`
		Body     string `json:"body"`
	}
	var rawStr string
	if json.Unmarshal(resp.Body, &rawStr) == nil {
		_ = json.Unmarshal([]byte(rawStr), &body)
	} else {
		_ = json.Unmarshal(resp.Body, &body)
	}
	if body.Accepted != 1 || body.Count != 1 || body.Cand != "chorus" || body.Body != "hello-blob" || len(deleted) != 1 {
		t.Fatalf("unexpected hosted kind bridge output: %+v deleted=%v raw=%s", body, deleted, string(resp.Body))
	}
}

// The kind host methods on the shared in-memory test host
// (hosted_test.go) are unavailable, matching a server with no kind
// backends wired.
func (h *memHost) KindUpsert(context.Context, string, []core.KindItem) (int, error) {
	return 0, ErrUnavailable
}
func (h *memHost) KindDelete(context.Context, string, []string) error { return ErrUnavailable }
func (h *memHost) KindItems(context.Context, string, string, int) (core.KindPage, error) {
	return core.KindPage{}, ErrUnavailable
}
func (h *memHost) KindVisible(context.Context, string, []string) ([]string, error) {
	return nil, ErrUnavailable
}
func (h *memHost) KindBrowse(context.Context, string, core.KindBrowseRequest) (core.KindPage, error) {
	return core.KindPage{}, ErrUnavailable
}
func (h *memHost) ResolveSources(context.Context, core.SourceQuery) ([]core.SourceCandidate, error) {
	return nil, ErrUnavailable
}
func (h *memHost) CachePut(context.Context, string, string, io.Reader) (string, error) {
	return "", ErrUnavailable
}
func (h *memHost) CacheOpen(context.Context, string) (io.ReadSeekCloser, error) {
	return nil, ErrUnavailable
}
func (h *memHost) CacheDelete(context.Context, string) error {
	return ErrUnavailable
}
func (h *memHost) StreamURL(context.Context, string) (string, error) { return "", ErrUnavailable }
func (h *memHost) StreamPartURL(context.Context, string, *int) (string, error) {
	return "", ErrUnavailable
}
func (h *memHost) StreamURLWithOptions(context.Context, string, StreamURLOptions) (string, error) {
	return "", ErrUnavailable
}
func (h *memHost) BadgesSet(context.Context, string, []BadgeEntry) error { return ErrUnavailable }
func (h *memHost) BadgesClear(context.Context, string) error             { return ErrUnavailable }
func (h *memHost) BadgesUsage(context.Context) (BadgeUsage, error) {
	return BadgeUsage{}, ErrUnavailable
}

func errFrom(b []byte, err error) error {
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return errors.New("empty blob")
	}
	return nil
}

func TestSetupStepGate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	rec := installContent(t, env)

	blocked, err := env.m.SetupBlockers(ctx, rec.ID)
	if err != nil || len(blocked) != 1 || blocked[0] != "connect" {
		t.Fatalf("fresh install must be blocked on setup: %v %v", blocked, err)
	}
	env.fake.routes["/ether/v1/setup/"] = func(req Request) (Response, error) {
		var body struct {
			Fields map[string]any `json:"fields"`
		}
		_ = json.Unmarshal(req.Body, &body)
		if body.Fields["endpoint"] == "https://example.test" {
			return Response{Status: 200, Body: json.RawMessage(`{"ok":true}`)}, nil
		}
		return Response{Status: 200, Body: json.RawMessage(`{"ok":false,"detail":"No answer from the endpoint"}`)}, nil
	}
	res, err := env.m.TestSetupStep(ctx, rec.ID, "connect")
	if err != nil || res.OK {
		t.Fatalf("failed test must record a failure: %+v %v", res, err)
	}
	if blocked, _ := env.m.SetupBlockers(ctx, rec.ID); len(blocked) != 1 {
		t.Fatalf("failed step must keep blocking: %v", blocked)
	}
	if _, err := env.m.Configure(ctx, rec.ID, map[string]any{"endpoint": "https://example.test", "api_key": "k"}); err != nil {
		t.Fatal(err)
	}
	res, err = env.m.TestSetupStep(ctx, rec.ID, "connect")
	if err != nil || !res.OK {
		t.Fatalf("configured test: %+v %v", res, err)
	}
	if blocked, _ := env.m.SetupBlockers(ctx, rec.ID); len(blocked) != 0 {
		t.Fatalf("passing step must unblock: %v", blocked)
	}
	state, err := env.m.SetupState(ctx, rec.ID)
	if err != nil || len(state) != 1 || state[0].Status != SetupOK {
		t.Fatalf("setup state: %+v %v", state, err)
	}
}

func TestKindVisibleAndBrowseGuardsAndHostedBridge(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	eng := wasm.NewEngine(nil)
	t.Cleanup(func() { _ = eng.Close() })
	env.m.RegisterRuntime("hosted", HostedRuntimeFactory(eng))

	man := contentManifest()
	man.Ether.Contributes.Setup = nil
	man.Ether.Capabilities = append(man.Ether.Capabilities, CapFrontendAdapter)
	man.Ether.Contributes.FrontendAdapters = []FrontendAdapterMeta{{
		Key:          "feed",
		Label:        "Feed",
		PathPrefix:   "/feed",
		ProtocolHint: "custom",
		Auth:         "none_lan",
		Libraries:    []string{"recording"},
	}}
	raw, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	script := `function handle(req) {
  if (req.path === "/ether/health") return { status: 200, body: { status: "ok" } };
  if (req.path === "/ether/v1/frontend/feed/check") {
    var visible = ether.kinds.visible("recording", ["a", "b"]);
    var page = ether.kinds.browse("recording", { query: "song", limit: 10 });
    return { status: 200, body: { visible: visible, items: page.items, tokenPresent: !!(req.headers && req.headers["X-Ether-Viewer-Token"]) } };
  }
  if (req.path === "/ether/v1/frontend/feed/overlimit") {
    var ids = [];
    for (var i = 0; i < 501; i++) ids.push("id-" + i);
    ether.kinds.visible("recording", ids);
    return { status: 200, body: { ok: true } };
  }
  if (req.path === "/ether/view") {
    ether.kinds.visible("recording", ["a"]);
    return { status: 200, body: { ok: true } };
  }
  return { status: 200, body: {} };
}`
	prev, err := env.m.PreviewInstall(ctx, Source{Kind: "studio", Manifest: raw, Files: map[string][]byte{"dist/plugin.js": []byte(script)}})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := env.m.Install(ctx, prev, InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		t.Fatal(err)
	}

	var gotViewerVisible, gotViewerBrowse int64
	var gotQuery core.KindBrowseRequest
	env.m.SetKindBackends(KindBackends{
		KindVisible: func(_ context.Context, pluginID string, viewerID int64, kindKey string, ids []string) ([]string, error) {
			gotViewerVisible = viewerID
			if pluginID != rec.ID || kindKey != "recording" {
				return nil, fmt.Errorf("unexpected %s %s", pluginID, kindKey)
			}
			out := make([]string, 0, len(ids))
			for _, id := range ids {
				if id != "b" {
					out = append(out, id)
				}
			}
			return out, nil
		},
		KindBrowse: func(_ context.Context, pluginID string, viewerID int64, kindKey string, q core.KindBrowseRequest) (core.KindPage, error) {
			gotViewerBrowse = viewerID
			gotQuery = q
			return core.KindPage{Items: []core.KindItem{{ID: "a", Level: "track", Title: "Song A"}}}, nil
		},
	})

	// Host-level guard checks: missing viewer, oversized batch, and undeclared kind fail closed.
	h := env.m.HostFor(rec.ID)
	if _, err := h.KindVisible(ctx, "recording", []string{"a"}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("KindVisible without viewer: %v", err)
	}
	if _, err := h.KindBrowse(ctx, "recording", core.KindBrowseRequest{}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("KindBrowse without viewer: %v", err)
	}
	over := make([]string, MaxKindVisibleIDs+1)
	for i := range over {
		over[i] = fmt.Sprintf("id-%d", i)
	}
	var valErr *ValidationError
	if _, err := h.KindVisible(WithViewer(ctx, 42), "recording", over); !errors.As(err, &valErr) {
		t.Fatalf("KindVisible over limit: %v", err)
	}
	if _, err := h.KindVisible(WithViewer(ctx, 42), "undeclared", []string{"a"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("KindVisible undeclared kind: %v", err)
	}
	if _, err := h.KindBrowse(WithViewer(ctx, 42), "recording", core.KindBrowseRequest{Total: "bogus"}); !errors.As(err, &valErr) {
		t.Fatalf("KindBrowse invalid total: %v", err)
	}
	for _, mode := range []string{"", "cheap", "none", "exact"} {
		if _, err := h.KindBrowse(WithViewer(ctx, 42), "recording", core.KindBrowseRequest{Total: mode}); err != nil {
			t.Fatalf("KindBrowse total=%q: %v", mode, err)
		}
		if gotQuery.Total != mode {
			t.Fatalf("KindBrowse total=%q forwarded as %q", mode, gotQuery.Total)
		}
	}
	// Unknown empty field rejected; declared + envelope empty fields pass without counting toward value bounds.
	if _, err := h.KindBrowse(WithViewer(ctx, 42), "recording", core.KindBrowseRequest{
		Empty: map[string]bool{"unknownField": true},
	}); !errors.As(err, &valErr) {
		t.Fatalf("KindBrowse unknown empty field: %v", err)
	}
	half := make([]string, MaxKindBrowseFacetValues/2)
	for i := range half {
		half[i] = "v"
	}
	if _, err := h.KindBrowse(WithViewer(ctx, 42), "recording", core.KindBrowseRequest{
		AnyGroups: []core.KindBrowseFacetGroup{{Any: []core.KindBrowseFacetAlt{
			{Field: "genre", Values: half},
			{Field: "recordist", Values: half},
		}}},
		Empty:        map[string]bool{"genre": true, "released": false},
		ExcludeQuery: "live",
	}); err != nil {
		t.Fatalf("KindBrowse valid empty + cross-field anyGroups + excludeQuery: %v", err)
	}
	if gotQuery.ExcludeQuery != "live" || !gotQuery.Empty["genre"] || gotQuery.Empty["released"] {
		t.Fatalf("KindBrowse forwarded query = %+v", gotQuery)
	}
	overHalf := append(append([]string(nil), half...), "extra")
	if _, err := h.KindBrowse(WithViewer(ctx, 42), "recording", core.KindBrowseRequest{
		AnyGroups: []core.KindBrowseFacetGroup{{Any: []core.KindBrowseFacetAlt{
			{Field: "genre", Values: half},
			{Field: "recordist", Values: overHalf},
		}}},
	}); !errors.As(err, &valErr) {
		t.Fatalf("KindBrowse over-limit cross-field anyGroups: %v", err)
	}

	// Non-adapter route carrying a forged X-Ether-Viewer header must NOT get viewer context or token.
	nonAdapterReq := Request{
		Method: "GET",
		Path:   "/ether/view",
		Headers: map[string]string{
			"X-Ether-Viewer":         "42",
			HeaderAdapterViewerToken: "forged-token",
		},
	}
	nonAdapterCtx, releaseNonAdapter := env.m.bindAdapterViewer(ctx, rec.ID, &nonAdapterReq, 0)
	defer releaseNonAdapter()
	if _, ok := ViewerFromContext(nonAdapterCtx); ok {
		t.Fatal("non-adapter route bound viewer from header")
	}
	if nonAdapterReq.Headers[HeaderAdapterViewerToken] != "" {
		t.Fatal("forged viewer token was not stripped on non-adapter route")
	}

	// Adapter route binds viewer + mints request-scoped token, and revokes it on release.
	adapterReq := Request{
		Method: "GET",
		Path:   "/ether/v1/frontend/feed/check",
		Headers: map[string]string{
			"X-Ether-Viewer":         "42",
			HeaderAdapterViewerToken: "forged-token",
		},
	}
	adapterCtx, releaseAdapter := env.m.bindAdapterViewer(ctx, rec.ID, &adapterReq, 0)
	if v, ok := ViewerFromContext(adapterCtx); !ok || v != 42 {
		t.Fatalf("adapter viewer = %d, %v; want 42", v, ok)
	}
	tok := adapterReq.Headers[HeaderAdapterViewerToken]
	if tok == "" || tok == "forged-token" {
		t.Fatalf("expected fresh viewer token, got %q", tok)
	}
	if v, ok := env.m.ResolveAdapterViewerToken(rec.ID, tok); !ok || v != 42 {
		t.Fatalf("ResolveAdapterViewerToken = %d, %v; want 42", v, ok)
	}
	if _, ok := env.m.ResolveAdapterViewerToken("other.plugin", tok); ok {
		t.Fatal("token resolved for another plugin")
	}
	releaseAdapter()
	if _, ok := env.m.ResolveAdapterViewerToken(rec.ID, tok); ok {
		t.Fatal("token still valid after release")
	}

	// Frontend adapter route with authenticated viewer succeeds in QuickJS runtime.
	resp, err := env.m.Call(ctx, rec.ID, Request{
		Method:  "GET",
		Path:    "/ether/v1/frontend/feed/check",
		Headers: map[string]string{"X-Ether-Viewer": "42"},
	})
	if err != nil {
		t.Fatalf("hosted adapter call: %v", err)
	}
	var out struct {
		Visible      []string        `json:"visible"`
		Items        []core.KindItem `json:"items"`
		TokenPresent bool            `json:"tokenPresent"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", string(resp.Body), err)
	}
	if gotViewerVisible != 42 || gotViewerBrowse != 42 || gotQuery.Query != "song" || gotQuery.Limit != 10 {
		t.Fatalf("viewer/query mismatch: vis=%d browse=%d q=%+v", gotViewerVisible, gotViewerBrowse, gotQuery)
	}
	if len(out.Visible) != 1 || out.Visible[0] != "a" || len(out.Items) != 1 || out.Items[0].ID != "a" || !out.TokenPresent {
		t.Fatalf("unexpected hosted output: %+v", out)
	}

	// Over-limit batch from guest fails.
	if _, err := env.m.Call(ctx, rec.ID, Request{
		Method:  "GET",
		Path:    "/ether/v1/frontend/feed/overlimit",
		Headers: map[string]string{"X-Ether-Viewer": "42"},
	}); err == nil {
		t.Fatal("over-limit kinds.visible succeeded")
	}

	// Non-adapter route calling kinds.visible fails even if X-Ether-Viewer header is present.
	if _, err := env.m.Call(ctx, rec.ID, Request{
		Method:  "GET",
		Path:    "/ether/view",
		Headers: map[string]string{"X-Ether-Viewer": "42"},
	}); err == nil {
		t.Fatal("non-adapter kinds.visible succeeded")
	}
}

func TestCacheDelete(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	eng := wasm.NewEngine(nil)
	t.Cleanup(func() { _ = eng.Close() })
	env.m.RegisterRuntime("hosted", HostedRuntimeFactory(eng))

	blobs, err := blobcache.Open(t.TempDir(), blobcache.Options{SweepPeriod: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })

	const quotaBytes = 1024
	cachePut, cacheOpen, cacheDelete := blobs.HostBackends(func(_, _ string) blobcache.Quota {
		return blobcache.Quota{MaxBytes: quotaBytes}
	})
	env.m.SetKindBackends(KindBackends{
		CachePut:    cachePut,
		CacheOpen:   cacheOpen,
		CacheDelete: cacheDelete,
	})

	// Install content plugin A with a hosted JS handler that exercises ether.cache.delete.
	manA := contentManifest()
	manA.Ether.Contributes.MediaKinds[0].Sync = KindSyncDecl{Mode: KindSyncPush}
	rawA, err := json.Marshal(manA)
	if err != nil {
		t.Fatal(err)
	}
	guestJS := `function handle(req) {
		if (req.path === "/ether/health") return { status: 200, body: { status: "ok" } };
		var body = typeof req.body === "string" ? JSON.parse(req.body) : req.body;
		ether.cache.delete(body.ref);
		return { status: 200, body: { ok: true } };
	}`
	prevA, err := env.m.PreviewInstall(ctx, Source{
		Kind:     "studio",
		Manifest: rawA,
		Files:    map[string][]byte{"dist/plugin.js": []byte(guestJS)},
	})
	if err != nil {
		t.Fatal(err)
	}
	recA, err := env.m.Install(ctx, prevA, InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	libA := core.PluginLibraryKey(recA.ID, "recording")
	hA := env.m.HostFor(recA.ID)

	// Install a second content plugin B to verify cross-plugin isolation on identical content.
	manB := contentManifest()
	manB.ID = "community.example.other-library"
	manB.Name = "Other Library"
	manB.Ether.Contributes.MediaKinds[0].Sync = KindSyncDecl{Mode: KindSyncPush}
	rawB, err := json.Marshal(manB)
	if err != nil {
		t.Fatal(err)
	}
	prevB, err := env.m.PreviewInstall(ctx, Source{
		Kind:     "studio",
		Manifest: rawB,
		Files:    map[string][]byte{"dist/plugin.js": []byte("// other")},
	})
	if err != nil {
		t.Fatal(err)
	}
	recB, err := env.m.Install(ctx, prevB, InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	libB := core.PluginLibraryKey(recB.ID, "recording")
	hB := env.m.HostFor(recB.ID)

	// Store a small kept blob (128 B) and a large temporary blob (768 B) under a 1024 B quota.
	keptData := bytes.Repeat([]byte("k"), 128)
	temp1Data := bytes.Repeat([]byte("a"), 768)
	temp2Data := bytes.Repeat([]byte("b"), 768)

	keptRef, err := hA.CachePut(ctx, libA, "kept.rom", bytes.NewReader(keptData))
	if err != nil {
		t.Fatal(err)
	}
	temp1Ref, err := hA.CachePut(ctx, libA, "temp1.xml", bytes.NewReader(temp1Data))
	if err != nil {
		t.Fatal(err)
	}

	// Delete temp1 through hosted ether.cache.delete; its quota is reclaimed immediately.
	delBody, _ := json.Marshal(map[string]string{"ref": temp1Ref})
	resp, err := env.m.Call(ctx, recA.ID, Request{Method: "POST", Path: "/ether/job/run", Body: delBody})
	if err != nil || resp.Status != 200 {
		t.Fatalf("hosted cache.delete: resp=%+v err=%v", resp, err)
	}
	if _, err := hA.CacheOpen(ctx, temp1Ref); !errors.Is(err, blobcache.ErrNotFound) {
		t.Fatalf("CacheOpen after delete = %v, want ErrNotFound", err)
	}

	// Writing a second large temporary blob fits without evicting the older small kept blob.
	if _, err := hA.CachePut(ctx, libA, "temp2.xml", bytes.NewReader(temp2Data)); err != nil {
		t.Fatal(err)
	}
	rc, err := hA.CacheOpen(ctx, keptRef)
	if err != nil {
		t.Fatalf("kept blob was evicted despite deleting temp1: %v", err)
	}
	gotKept, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(gotKept, keptData) {
		t.Fatalf("kept blob content mismatch")
	}

	// Deleting an absent/already-deleted ref succeeds idempotently.
	if err := hA.CacheDelete(ctx, temp1Ref); err != nil {
		t.Fatalf("idempotent delete failed: %v", err)
	}

	// Cross-plugin isolation: deleting a ref with identical content never deletes plugin B's copy.
	keptRefB, err := hB.CachePut(ctx, libB, "kept.rom", bytes.NewReader(keptData))
	if err != nil || keptRefB != keptRef {
		t.Fatalf("plugin B CachePut = %q %v (want %q)", keptRefB, err, keptRef)
	}
	if err := hA.CacheDelete(ctx, keptRef); err != nil {
		t.Fatalf("plugin A delete: %v", err)
	}
	if _, err := hA.CacheOpen(ctx, keptRef); !errors.Is(err, blobcache.ErrNotFound) {
		t.Fatalf("plugin A blob still present after delete: %v", err)
	}
	rcB, err := hB.CacheOpen(ctx, keptRefB)
	if err != nil {
		t.Fatalf("plugin A deleted plugin B's blob: %v", err)
	}
	_ = rcB.Close()

	// Validation, read-only tool, capability, and role checks match cache write services.
	var ve *ValidationError
	if err := hA.CacheDelete(ctx, "blob:not-a-hash"); !errors.As(err, &ve) {
		t.Fatalf("malformed ref err = %v, want *ValidationError", err)
	}
	roCtx := context.WithValue(ctx, readOnlyToolContextKey{}, true)
	if err := hA.CacheDelete(roCtx, keptRef); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("read-only tool CacheDelete = %v, want ErrPermissionDenied", err)
	}
	// Non-content plugin (no media kinds or cache capabilities) is denied.
	plainMan := baseManifest()
	plainMan.ID = "community.example.plain"
	plainMan.Name = "Plain"
	rawPlain, _ := json.Marshal(plainMan)
	prevPlain, err := env.m.PreviewInstall(ctx, Source{
		Kind:     "studio",
		Manifest: rawPlain,
		Files:    map[string][]byte{"dist/plugin.js": []byte("// plain")},
	})
	if err != nil {
		t.Fatal(err)
	}
	recPlain, err := env.m.Install(ctx, prevPlain, InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.m.HostFor(recPlain.ID).CacheDelete(ctx, keptRef); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("non-content plugin CacheDelete = %v, want ErrPermissionDenied", err)
	}
	// Switching off both content capabilities denies CacheDelete.
	if _, err := env.m.SetRoles(ctx, recA.ID, []Capability{CapCatalogProvider, CapKindResolver}); err != nil {
		t.Fatal(err)
	}
	if err := hA.CacheDelete(ctx, keptRef); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("disabled roles CacheDelete = %v, want ErrPermissionDenied", err)
	}
}

func TestHostUsenetCheckValidationRateLimitAndBackgroundGuard(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	env.m.now = func() time.Time { return now }

	installResolver := func(id, sourcesLevel string) Record {
		t.Helper()
		man := contentManifest()
		man.ID = id
		man.Name = id
		man.Ether.Contributes.MediaKinds[0].Key = strings.ReplaceAll(id, ".", "-")
		man.Ether.Contributes.MediaKinds[0].Sync = KindSyncDecl{Mode: KindSyncPush}
		man.Ether.Permissions.Sources = sourcesLevel
		raw, err := json.Marshal(man)
		if err != nil {
			t.Fatal(err)
		}
		p, err := env.m.PreviewInstall(ctx, Source{
			Kind:     "studio",
			Manifest: raw,
			Files:    map[string][]byte{"dist/plugin.js": []byte("// resolver")},
		})
		if err != nil {
			t.Fatal(err)
		}
		rec, err := env.m.Install(ctx, p, InstallOptions{AcceptAddedPermissions: true})
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}

	recA := installResolver("community.example.useneta", "search")
	recB := installResolver("community.example.usenetb", "search")
	recNoPerm := installResolver("community.example.usenetnone", "none")

	var backendCalls int
	env.m.SetKindBackends(KindBackends{
		ResolveSources: func(_ context.Context, _ string, q core.SourceQuery) ([]core.SourceCandidate, error) {
			backendCalls++
			nzbURL := q.NZBURL
			if nzbURL == "" && q.Candidate != nil {
				nzbURL = q.Candidate.NZBURL
			}
			return []core.SourceCandidate{{
				NZBURL:      nzbURL,
				UsenetCheck: &core.UsenetCheckResult{Available: true, Checked: 12, Missing: 0},
			}}, nil
		},
	})

	hA := env.m.HostFor(recA.ID)
	hB := env.m.HostFor(recB.ID)
	hNone := env.m.HostFor(recNoPerm.ID)

	// Missing nzbUrl fails validation without consuming rate limit.
	var ve *ValidationError
	if _, err := hA.ResolveSources(ctx, core.SourceQuery{Kind: "usenet_check"}); !errors.As(err, &ve) {
		t.Fatalf("empty usenet_check err = %v, want *ValidationError", err)
	}

	// Missing sources permission is denied.
	if _, err := hNone.ResolveSources(ctx, core.SourceQuery{Kind: "usenet_check", NZBURL: "https://idx.example/1.nzb"}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("no-perm usenet_check err = %v, want ErrPermissionDenied", err)
	}

	// Background / scheduled contexts are refused (on-demand only).
	if _, err := hA.ResolveSources(WithBackgroundCall(ctx), core.SourceQuery{
		Kind:   "usenet_check",
		NZBURL: "https://idx.example/bg.nzb",
	}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("background usenet_check err = %v, want ErrPermissionDenied", err)
	}
	if backendCalls != 0 {
		t.Fatalf("backend called %d times for rejected queries, want 0", backendCalls)
	}

	// Candidate form and direct nzbUrl form both succeed up to DefaultUsenetCheckPerMin.
	for i := 0; i < DefaultUsenetCheckPerMin; i++ {
		var q core.SourceQuery
		if i%2 == 0 {
			q = core.SourceQuery{Kind: "usenet_check", NZBURL: fmt.Sprintf("https://idx.example/%d.nzb", i)}
		} else {
			q = core.SourceQuery{
				Kind:      "usenet_check",
				Candidate: &core.SourceCandidate{NZBURL: fmt.Sprintf("https://idx.example/%d.nzb", i)},
			}
		}
		got, err := hA.ResolveSources(ctx, q)
		if err != nil {
			t.Fatalf("call %d/%d failed: %v", i+1, DefaultUsenetCheckPerMin, err)
		}
		if len(got) != 1 || got[0].UsenetCheck == nil || !got[0].UsenetCheck.Available {
			t.Fatalf("call %d unexpected result: %+v", i+1, got)
		}
	}

	// Next call from plugin A exceeds its per-plugin rate limit.
	if _, err := hA.ResolveSources(ctx, core.SourceQuery{Kind: "usenet_check", NZBURL: "https://idx.example/over.nzb"}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("call %d err = %v, want ErrRateLimited", DefaultUsenetCheckPerMin+1, err)
	}

	// Plugin B has an independent per-plugin bucket and still succeeds.
	if _, err := hB.ResolveSources(ctx, core.SourceQuery{Kind: "usenet_check", NZBURL: "https://idx.example/other-plugin.nzb"}); err != nil {
		t.Fatalf("plugin B unexpectedly limited: %v", err)
	}

	// Advancing clock past 1 minute resets plugin A's window.
	now = now.Add(61 * time.Second)
	if _, err := hA.ResolveSources(ctx, core.SourceQuery{Kind: "usenet_check", NZBURL: "https://idx.example/after-window.nzb"}); err != nil {
		t.Fatalf("plugin A after rate window failed: %v", err)
	}
}

func TestHostResolveSourcesWithResultAndValidation(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	rec := installContent(t, env)
	h := env.m.HostFor(rec.ID)

	var seenQuery core.SourceQuery
	env.m.SetKindBackends(KindBackends{
		ResolveSourcesResult: func(_ context.Context, _ string, q core.SourceQuery) (core.SourceResolveResult, error) {
			seenQuery = q
			if q.Kind == "indexer_search" {
				return core.SourceResolveResult{
					Candidates: []core.SourceCandidate{{Title: q.Query, IndexerID: "fast", Indexer: "Fast"}},
					Indexers: []core.IndexerSearchStatus{
						{IndexerID: "fast", Indexer: "Fast", Status: core.IndexerStatusAnswered, Results: 1, MS: 12},
						{IndexerID: "slow", Indexer: "Slow", Status: core.IndexerStatusTimedOut, MS: 250},
					},
				}, nil
			}
			enabled, cachedOnly := true, false
			if q.Kind == "debrid" {
				return core.SourceResolveResult{Enabled: &enabled, CachedOnly: &cachedOnly}, nil
			}
			return core.SourceResolveResult{
				Candidates: []core.SourceCandidate{{InfoHash: q.InfoHash, Cached: true}},
				CachedOnly: &cachedOnly,
			}, nil
		},
	})

	// Validation rejects negative settleMs / minResults / timeoutMs and >256 filter entries.
	neg := -5
	for _, q := range []core.SourceQuery{
		{Kind: "indexer_search", Query: "a", SettleMS: &neg},
		{Kind: "indexer_search", Query: "a", MinResults: -1},
		{Kind: "indexer_search", Query: "a", TimeoutMS: -1},
		{Kind: "indexer_search", Query: "a", SkipIndexers: make([]string, MaxSourceIndexerFilters+1)},
		{Kind: "indexer_search", Query: "a", OnlyIndexers: make([]string, MaxSourceIndexerFilters+1)},
	} {
		var ve *ValidationError
		if _, err := h.ResolveSources(ctx, q); !errors.As(err, &ve) {
			t.Fatalf("query %+v err = %v, want *ValidationError", q, err)
		}
	}

	// Hosted bridge returns {candidates, indexers} for indexer_search and forwards all options.
	bridge := hostBridge{h: h}
	raw, err := bridge.CallHost(ctx, "sources.resolve", json.RawMessage(`[{"kind":"indexer_search","query":"Dune","firstResults":true,"settleMs":150,"minResults":2,"skipIndexers":["slow"],"timeoutMs":4000}]`))
	if err != nil {
		t.Fatalf("hosted sources.resolve: %v", err)
	}
	if !seenQuery.FirstResults || seenQuery.SettleMS == nil || *seenQuery.SettleMS != 150 || seenQuery.MinResults != 2 || seenQuery.TimeoutMS != 4000 || len(seenQuery.SkipIndexers) != 1 || seenQuery.SkipIndexers[0] != "slow" {
		t.Fatalf("forwarded query = %+v", seenQuery)
	}
	var envObj map[string]any
	if err := json.Unmarshal(raw, &envObj); err != nil {
		t.Fatalf("unmarshal hosted reply: %v", err)
	}
	if idxList, ok := envObj["indexers"].([]any); !ok || len(idxList) != 2 {
		t.Fatalf("hosted reply indexers = %#v, want 2 entries", envObj["indexers"])
	}

	// Non-indexer_search omits indexers from the hosted JSON envelope.
	rawDC, err := bridge.CallHost(ctx, "sources.resolve", json.RawMessage(`[{"kind":"debrid_check","infoHash":"abcd"}]`))
	if err != nil {
		t.Fatalf("hosted debrid_check: %v", err)
	}
	var dcObj map[string]any
	if err := json.Unmarshal(rawDC, &dcObj); err != nil {
		t.Fatalf("unmarshal hosted debrid_check: %v", err)
	}
	if _, hasIndexers := dcObj["indexers"]; hasIndexers {
		t.Fatalf("debrid_check reply unexpectedly included indexers: %#v", dcObj)
	}

	if dcObj["cachedOnly"] != false {
		t.Fatal("debrid_check did not return the cache policy")
	}

	policyQuery := json.RawMessage(`[{"kind":"debrid"}]`)
	policy, err := bridge.CallHost(ctx, "sources.resolve", policyQuery)
	if err != nil || string(policy) != `{"enabled":true,"cachedOnly":false}` {
		t.Fatalf("unexpected debrid policy response: %v", err)
	}
	t.Cleanup(func() {
		if err := env.m.SetGrants(ctx, rec.ID, Grants{Revoked: []string{"sources"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.ResolveSources(ctx, core.SourceQuery{Kind: "debrid"}); !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("debrid policy without source permission: %v", err)
		}
	})

	// Streaming & listing validation + hosted not_found mapping.
	for _, q := range []core.SourceQuery{
		{Kind: "indexer_search_results"},
		{Kind: "indexer_search_results", SearchID: "s1", WaitMS: -1},
		{Kind: "indexer_search_cancel"},
	} {
		var ve *ValidationError
		if _, err := h.ResolveSources(ctx, q); !errors.As(err, &ve) {
			t.Fatalf("query %+v err = %v, want *ValidationError", q, err)
		}
	}

	// kind:"indexers" and streaming kinds use the same search permission.
	env.m.SetKindBackends(KindBackends{
		ResolveSourcesResult: func(_ context.Context, _ string, q core.SourceQuery) (core.SourceResolveResult, error) {
			if q.Kind == "indexer_search_results" && q.SearchID == "missing" {
				return core.SourceResolveResult{}, ErrNotFound
			}
			med, last := int64(120), int64(90)
			return core.SourceResolveResult{
				Candidates: []core.SourceCandidate{},
				Indexers: []core.IndexerSearchStatus{
					{IndexerID: "idx1", Indexer: "One", Kind: "newznab", MedianMS: &med, LastMS: &last},
				},
			}, nil
		},
	})
	rawIdx, err := bridge.CallHost(ctx, "sources.resolve", json.RawMessage(`[{"kind":"indexers"}]`))
	if err != nil {
		t.Fatalf("hosted indexers: %v", err)
	}
	var idxObj map[string]any
	if err := json.Unmarshal(rawIdx, &idxObj); err != nil {
		t.Fatalf("unmarshal indexers: %v", err)
	}
	if list, ok := idxObj["indexers"].([]any); !ok || len(list) != 1 {
		t.Fatalf("hosted indexers = %#v, want 1 entry", idxObj["indexers"])
	}
	if _, err := bridge.CallHost(ctx, "sources.resolve", json.RawMessage(`[{"kind":"indexer_search_results","searchId":"missing"}]`)); err == nil {
		t.Fatalf("expected fault for missing searchId")
	} else {
		var fault *wasm.Fault
		if !errors.As(err, &fault) || fault.Code != "not_found" {
			t.Fatalf("missing searchId fault = %v, want code not_found", err)
		}
	}
}

func TestHostUsenetCheckBackgroundPermissionAllowanceAndRevocation(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC)
	env.m.now = func() time.Time { return now }
	resetAt := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)

	installWithSources := func(id, sourcesLevel string) Record {
		t.Helper()
		man := contentManifest()
		man.ID = id
		man.Name = id
		man.Ether.Contributes.MediaKinds[0].Key = strings.ReplaceAll(id, ".", "-")
		man.Ether.Contributes.MediaKinds[0].Sync = KindSyncDecl{Mode: KindSyncPush}
		man.Ether.Permissions.Sources = sourcesLevel
		raw, err := json.Marshal(man)
		if err != nil {
			t.Fatal(err)
		}
		prev, err := env.m.PreviewInstall(ctx, Source{
			Kind:     "studio",
			Manifest: raw,
			Files:    map[string][]byte{"dist/plugin.js": []byte("// resolver")},
		})
		if err != nil {
			t.Fatalf("preview %s: %v", id, err)
		}
		rec, err := env.m.Install(ctx, prev, InstallOptions{AcceptAddedPermissions: true})
		if err != nil {
			t.Fatalf("install %s: %v", id, err)
		}
		return rec
	}

	recSearch := installWithSources("community.example.bgsearch", "search")
	recSearchCheck := installWithSources("community.example.bgsearchcheck", "search+check")
	recAcquireCheck := installWithSources("community.example.bgacquirecheck", "acquire+check")

	spent := map[string]int{}
	dailyCap := 2
	cachedURL := "https://idx.example/cached-bad.nzb"
	var backendCalls int

	env.m.SetKindBackends(KindBackends{
		PeekUsenetCheck: func(q core.SourceQuery) (core.SourceCandidate, bool) {
			if q.NZBURL == cachedURL {
				return core.SourceCandidate{
					NZBURL: cachedURL,
					UsenetCheck: &core.UsenetCheckResult{
						Available: false,
						Cached:    true,
						Reason:    "health_unhealthy",
					},
				}, true
			}
			return core.SourceCandidate{}, false
		},
		AdmitBackgroundUsenetCheck: func(ctx context.Context, pluginID string) (context.Context, func(), time.Time, error) {
			if spent[pluginID] >= dailyCap {
				return nil, nil, resetAt, core.ErrBudgetExhausted
			}
			spent[pluginID]++
			return ctx, func() { spent[pluginID]-- }, resetAt, nil
		},
		ResolveSourcesResult: func(_ context.Context, _ string, q core.SourceQuery) (core.SourceResolveResult, error) {
			backendCalls++
			return core.SourceResolveResult{
				Candidates: []core.SourceCandidate{{
					NZBURL:      q.NZBURL,
					UsenetCheck: &core.UsenetCheckResult{Available: true, Checked: 8},
				}},
			}, nil
		},
	})

	hSearch := env.m.HostFor(recSearch.ID)
	hSearchCheck := env.m.HostFor(recSearchCheck.ID)
	hAcquireCheck := env.m.HostFor(recAcquireCheck.ID)

	// Plain "search" still refuses background calls (including wasm-tagged contexts).
	if _, err := hSearch.ResolveSources(wasm.WithBackgroundCall(ctx), core.SourceQuery{
		Kind:   "usenet_check",
		NZBURL: "https://idx.example/1.nzb",
	}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("search-only wasm background err = %v, want ErrPermissionDenied", err)
	}

	// "search+check" spends 2 background checks, then refuses the 3rd with RateLimitError + retryAt.
	for i := 1; i <= dailyCap; i++ {
		res, err := hSearchCheck.ResolveSources(WithBackgroundCall(ctx), core.SourceQuery{
			Kind:   "usenet_check",
			NZBURL: fmt.Sprintf("https://idx.example/bg-%d.nzb", i),
		})
		if err != nil || len(res) != 1 || res[0].UsenetCheck == nil || !res[0].UsenetCheck.Available {
			t.Fatalf("background check %d: res=%+v err=%v", i, res, err)
		}
	}
	_, err := hSearchCheck.ResolveSources(WithBackgroundCall(ctx), core.SourceQuery{
		Kind:   "usenet_check",
		NZBURL: "https://idx.example/bg-over.nzb",
	})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("exhausted background err = %v, want ErrRateLimited", err)
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) || !rl.RetryAt.Equal(resetAt) || rl.RetryAfterSeconds(now) != int64((6*time.Hour)/time.Second) {
		t.Fatalf("RateLimitError = %#v (err=%v), want retryAt=%v retryAfter=21600s", rl, err, resetAt)
	}

	// Hosted fault preserves retryAfterSec and retryAt on rate_limited.
	bridge := hostBridge{h: hSearchCheck}
	if _, ferr := bridge.CallHost(WithBackgroundCall(ctx), "sources.resolve", json.RawMessage(`[{"kind":"usenet_check","nzbUrl":"https://idx.example/bg-over.nzb"}]`)); ferr == nil {
		t.Fatal("expected hosted rate_limited fault")
	} else {
		var fault *wasm.Fault
		if !errors.As(ferr, &fault) || fault.Code != "rate_limited" || fault.RetryAfterSec <= 0 || fault.RetryAt != resetAt.Format(time.RFC3339) {
			t.Fatalf("hosted fault = %#v, want code=rate_limited retryAt=%s", fault, resetAt.Format(time.RFC3339))
		}
	}

	// Cached verdict hit succeeds in background even after daily allowance is spent,
	// without calling backend or incrementing spent.
	beforeCalls := backendCalls
	cachedRes, err := hSearchCheck.ResolveSources(WithBackgroundCall(ctx), core.SourceQuery{
		Kind:   "usenet_check",
		NZBURL: cachedURL,
	})
	if err != nil || len(cachedRes) != 1 || cachedRes[0].UsenetCheck == nil || !cachedRes[0].UsenetCheck.Cached || cachedRes[0].UsenetCheck.Available {
		t.Fatalf("cached background check = (%+v, %v), want cached unavailable", cachedRes, err)
	}
	if backendCalls != beforeCalls || spent[recSearchCheck.ID] != dailyCap {
		t.Fatalf("cached hit invoked backend (%d vs %d) or changed spent (%d)", backendCalls, beforeCalls, spent[recSearchCheck.ID])
	}

	// On-demand viewer call never counts against the daily allowance and still
	// succeeds even after the plugin's background allowance is exhausted.
	if _, err := hSearchCheck.ResolveSources(ctx, core.SourceQuery{
		Kind:   "usenet_check",
		NZBURL: "https://idx.example/viewer-ondemand.nzb",
	}); err != nil {
		t.Fatalf("on-demand usenet_check failed after background exhausted: %v", err)
	}
	if spent[recSearchCheck.ID] != dailyCap {
		t.Fatalf("on-demand call changed background spent to %d, want %d", spent[recSearchCheck.ID], dailyCap)
	}

	// "acquire+check" also permits background usenet_check and supports revoking
	// "sources.check" independently from on-demand "sources".
	if _, err := hAcquireCheck.ResolveSources(WithBackgroundCall(ctx), core.SourceQuery{
		Kind:   "usenet_check",
		NZBURL: "https://idx.example/acq-bg.nzb",
	}); err != nil {
		t.Fatalf("acquire+check background check: %v", err)
	}
	if err := env.m.SetGrants(ctx, recAcquireCheck.ID, Grants{Revoked: []string{"sources.check"}}); err != nil {
		t.Fatalf("revoke sources.check: %v", err)
	}
	if _, err := hAcquireCheck.ResolveSources(WithBackgroundCall(ctx), core.SourceQuery{
		Kind:   "usenet_check",
		NZBURL: "https://idx.example/acq-bg-revoked.nzb",
	}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("revoked sources.check background err = %v, want ErrPermissionDenied", err)
	}
	if _, err := hAcquireCheck.ResolveSources(ctx, core.SourceQuery{
		Kind:   "usenet_check",
		NZBURL: "https://idx.example/acq-ondemand-still-ok.nzb",
	}); err != nil {
		t.Fatalf("on-demand usenet_check failed when only sources.check was revoked: %v", err)
	}
}
