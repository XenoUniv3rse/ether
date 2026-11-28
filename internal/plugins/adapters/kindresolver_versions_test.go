package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/adapters"
	"github.com/braedonsaunders/ether/internal/plugins/frontendadapter"
	"github.com/braedonsaunders/ether/internal/resolver"
)

func TestKindVersionsCachingOpenSourceAndRememberedChoice(t *testing.T) {
	h := newKindTestHarness(t, 0, nil)
	ctx := context.Background()

	var nowMu sync.Mutex
	curNow := time.Now().UTC().Truncate(time.Second)
	nowFn := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return curNow
	}

	const (
		hash4K    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		hash1080p = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		nzb720p   = "https://media.example/releases/concert-720p.nzb"
	)
	bytes4K := []byte("4K-REMUX-PAYLOAD")
	bytes1080p := []byte("1080P-WEBDL-PAYLOAD")
	bytes720p := []byte("720P-USENET-PAYLOAD")

	idx := &kindFailoverIndexer{
		nzbPayloads: map[string][]byte{nzb720p: bytes720p},
	}
	deb := &kindFailoverDebrid{
		payloads: map[string][]byte{
			hash4K:    bytes4K,
			hash1080p: bytes1080p,
		},
	}
	resSvc := resolver.New(idx, nil, nil, config.Defaults(""), resolver.Options{})

	newResolver := func() *adapters.KindResolver {
		return adapters.NewKindResolver(adapters.KindResolverDeps{
			Manager:   h.mgr,
			Libraries: h.libs,
			Items:     h.items,
			Blobs:     h.blobs,
			Catalog:   h.catalog,
			Indexer:   idx,
			Resolver:  resSvc,
			Debrid:    deb,
			Engine:    kindFailoverEngine{},
			Now:       nowFn,
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}
	kr := newResolver()

	var resolveCalls atomic.Int32
	h.rt.callContext = func(_ context.Context, req plugins.Request) (plugins.Response, error) {
		switch req.Path {
		case "/ether/health":
			return plugins.Response{Status: http.StatusOK, Body: json.RawMessage(`{"status":"ok"}`)}, nil
		case "/ether/v1/kinds/recordings/resolve":
			resolveCalls.Add(1)
			resp := core.KindResolveResponse{
				TTLSec: 600,
				Sources: []core.PlayableSource{
					{
						ID:      "src-4k",
						Label:   "4K Remux",
						Pattern: "release",
						MIME:    "video/x-matroska",
						Release: &core.ReleaseHint{
							InfoHash: hash4K,
							Candidate: &core.SourceCandidate{
								InfoHash:   hash4K,
								Title:      "Concert.2025.2160p.UHD.BluRay.REMUX.HEVC.TrueHD.7.1-GRP",
								Size:       40 << 30,
								Cached:     true,
								Resolution: 2160,
								Codec:      "hevc",
								Source:     "remux",
							},
						},
					},
					{
						ID:      "src-1080p",
						Label:   "1080p WEB-DL",
						Pattern: "release",
						MIME:    "video/x-matroska",
						Release: &core.ReleaseHint{
							InfoHash: hash1080p,
							Candidate: &core.SourceCandidate{
								InfoHash:   hash1080p,
								Title:      "Concert.2025.1080p.WEB-DL.DDP5.1.H.264-GRP",
								Size:       8 << 30,
								Cached:     true,
								Resolution: 1080,
								Codec:      "h264",
								Source:     "web-dl",
							},
						},
					},
					{
						ID:      "src-720p",
						Label:   "720p Usenet",
						Pattern: "release",
						MIME:    "video/x-matroska",
						Release: &core.ReleaseHint{
							NZBURL: nzb720p,
							Candidate: &core.SourceCandidate{
								NZBURL:     nzb720p,
								Title:      "Concert.2025.720p.WEB-DL.H.264-USENET",
								Size:       3 << 30,
								Resolution: 720,
								Codec:      "h264",
								Source:     "web-dl",
							},
						},
					},
				},
			}
			raw, _ := json.Marshal(resp)
			return plugins.Response{Status: http.StatusOK, Body: raw}, nil
		default:
			return plugins.Response{Status: http.StatusNotFound}, nil
		}
	}

	const pluginID = "community.example.kindversions"
	decl := recordingsKindDecl(true)
	decl["sync"] = map[string]any{"mode": "push"}
	installStudioManifest(t, h.mgr, map[string]any{
		"id":      pluginID,
		"version": "1.0.0",
		"name":    "Kind Versions Test",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "content",
			"summary":      "Kind versions test",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"catalog_provider", "kind_resolver"},
			"permissions":  map[string]any{"network": []string{"media.example"}},
			"strings":      map[string]any{"en": "i18n/en.json"},
			"contributes":  map[string]any{"mediaKinds": []map[string]any{decl}},
		},
	}, map[string][]byte{"i18n/en.json": []byte(`{"lib_name":"Recordings"}`)})

	host := h.mgr.HostFor(pluginID)
	if _, err := host.KindUpsert(ctx, "recordings", []core.KindItem{
		{ID: "alb-1", Level: "album", Title: "Album 1"},
		{ID: "rec-versions", Level: "track", ParentID: "alb-1", Index: 1, Title: "Live Concert 2025", Playable: true},
	}); err != nil {
		t.Fatalf("KindUpsert: %v", err)
	}
	recItem, _, err := h.items.GetByExternal(ctx, pluginID, "recordings", "rec-versions")
	if err != nil {
		t.Fatalf("GetByExternal: %v", err)
	}
	key := recItem.Key
	viewer1 := &core.Viewer{ID: 1, IsOwner: true}
	viewer2 := &core.Viewer{ID: 2}

	// 1. Cold allowResolve=false and LocalResponse must NEVER call plugin /resolve.
	if vs, err := kr.KindVersions(ctx, viewer1, key, nil, false); err != nil || len(vs) != 0 {
		t.Fatalf("cold allowResolve=false = (%+v, %v), want empty", vs, err)
	}
	if vs, err := kr.KindVersions(core.WithLocalResponse(ctx), viewer1, key, nil, true); err != nil || len(vs) != 0 {
		t.Fatalf("cold LocalResponse = (%+v, %v), want empty", vs, err)
	}
	if resolveCalls.Load() != 0 {
		t.Fatalf("resolve called %d times on cold non-resolving listing, want 0", resolveCalls.Load())
	}

	// 2. Detail listing resolves once and returns W152 shape: index 0 Auto backed by 4K,
	// followed by every source as an explicit row, the 4K Auto pick included, so
	// the 4K release can be forced separately from Auto.
	versions, err := kr.KindVersions(ctx, viewer1, key, nil, true)
	if err != nil {
		t.Fatalf("KindVersions: %v", err)
	}
	if len(versions) != 4 {
		t.Fatalf("len(versions) = %d, want 4: %+v", len(versions), versions)
	}
	if !versions[0].Auto || versions[0].ID != core.KindAutoVersionID || versions[0].Release.ID != "kind:"+hash4K {
		t.Fatalf("versions[0] = %+v, want Auto backed by 4K", versions[0])
	}
	if !versions[0].Info.Estimated || versions[0].Release.Info.Height != 2160 || versions[0].Availability != core.VersionCached {
		t.Fatalf("versions[0] metadata = info:%+v rel:%+v avail:%q", versions[0].Info, versions[0].Release.Info, versions[0].Availability)
	}
	if versions[1].Auto || versions[1].ID != "kind:"+hash4K || versions[1].Release.ID != "kind:"+hash4K || versions[1].Release.Info.Height != 2160 {
		t.Fatalf("versions[1] = %+v, want explicit 4K row", versions[1])
	}
	if versions[2].Auto || versions[2].ID != "kind:"+hash1080p || versions[2].Release.Info.Height != 1080 {
		t.Fatalf("versions[2] = %+v, want 1080p", versions[2])
	}
	if versions[3].Auto || versions[3].ID != "kind:"+nzb720p || versions[3].Release.Info.Height != 720 {
		t.Fatalf("versions[3] = %+v, want 720p", versions[3])
	}
	if resolveCalls.Load() != 1 {
		t.Fatalf("resolveCalls = %d, want 1", resolveCalls.Load())
	}

	// 3. Repeat KindVersions, persisted-cache lookup on a fresh KindResolver (simulating
	// restart), OpenKindExcluding (Auto), and OpenKindSource (picked 720p) ALL reuse cache
	// without re-running plugin /resolve.
	krFresh := newResolver()
	versions2, err := krFresh.KindVersions(ctx, viewer1, key, nil, false)
	if err != nil || len(versions2) != 4 {
		t.Fatalf("persisted KindVersions = (%d, %v), want 4", len(versions2), err)
	}
	kmAuto, err := krFresh.OpenKindExcluding(ctx, viewer1, key, nil, nil)
	if err != nil {
		t.Fatalf("OpenKindExcluding(Auto): %v", err)
	}
	autoBuf := make([]byte, len(bytes4K))
	_, _ = kmAuto.ReadAt(autoBuf, 0)
	_ = kmAuto.Close()
	if string(autoBuf) != string(bytes4K) || kmAuto.Release.ID != "kind:"+hash4K {
		t.Fatalf("Auto opened %q (%s), want 4K", string(autoBuf), kmAuto.Release.ID)
	}

	// OpenKindSource opens ONLY the picked 720p release and does NOT clobber Auto winner.
	km720, err := krFresh.OpenKindSource(ctx, viewer1, key, "kind:"+nzb720p, nil)
	if err != nil {
		t.Fatalf("OpenKindSource(720p): %v", err)
	}
	buf720 := make([]byte, len(bytes720p))
	_, _ = km720.ReadAt(buf720, 0)
	_ = km720.Close()
	if string(buf720) != string(bytes720p) || km720.Release.ID != "kind:"+nzb720p {
		t.Fatalf("OpenKindSource opened %q (%s), want 720p", string(buf720), km720.Release.ID)
	}
	// Unknown source id returns ErrNotAvailable.
	if _, err := krFresh.OpenKindSource(ctx, viewer1, key, "kind:does-not-exist", nil); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("unknown source err = %v, want ErrNotAvailable", err)
	}
	if resolveCalls.Load() != 1 {
		t.Fatalf("resolveCalls after cached versions+auto+pick = %d, want 1", resolveCalls.Load())
	}

	// 4. Remembered viewer choice: recording viewer1's pick of 1080p promotes 1080p into
	// viewer1's Auto slot and Auto playback without affecting viewer2.
	title := &core.Title{Key: key, Type: core.TypeMovie, Title: "Live Concert 2025"}
	if err := resSvc.RecordViewerChoice(ctx, viewer1.ID, title, versions[2].ID, versions[2].Release, nil); err != nil {
		t.Fatalf("RecordViewerChoice: %v", err)
	}
	v1List, err := krFresh.KindVersions(ctx, viewer1, key, nil, true)
	if err != nil || len(v1List) != 4 {
		t.Fatalf("viewer1 KindVersions after choice = (%+v, %v)", v1List, err)
	}
	if !v1List[0].Auto || !v1List[0].Remembered || v1List[0].Release.ID != "kind:"+hash1080p {
		t.Fatalf("viewer1 Auto slot = %+v, want remembered 1080p", v1List[0])
	}
	if v1List[1].Release.ID != "kind:"+hash4K || v1List[2].Release.ID != "kind:"+hash1080p {
		t.Fatalf("viewer1 rows = %+v, want explicit 4K then 1080p", v1List[1:])
	}
	// Auto playback for viewer1 opens remembered 1080p; viewer2 still gets 4K default.
	kmV1, err := krFresh.OpenKindExcluding(ctx, viewer1, key, nil, nil)
	if err != nil {
		t.Fatalf("viewer1 Auto open: %v", err)
	}
	_ = kmV1.Close()
	if kmV1.Release.ID != "kind:"+hash1080p {
		t.Fatalf("viewer1 Auto opened %s, want remembered 1080p", kmV1.Release.ID)
	}
	kmV2, err := krFresh.OpenKindExcluding(ctx, viewer2, key, nil, nil)
	if err != nil {
		t.Fatalf("viewer2 Auto open: %v", err)
	}
	_ = kmV2.Close()
	if kmV2.Release.ID != "kind:"+hash4K {
		t.Fatalf("viewer2 Auto opened %s, want default 4K (not viewer1's choice)", kmV2.Release.ID)
	}
	if resolveCalls.Load() != 1 {
		t.Fatalf("resolveCalls after viewer choice flow = %d, want 1", resolveCalls.Load())
	}

	// Clearing viewer1's choice restores default 4K in Auto.
	if cleared, err := resSvc.ClearViewerChoice(ctx, viewer1.ID, title, "all"); err != nil || !cleared {
		t.Fatalf("ClearViewerChoice = (%v, %v), want (true, nil)", cleared, err)
	}
	v1Cleared, err := krFresh.KindVersions(ctx, viewer1, key, nil, true)
	if err != nil || len(v1Cleared) != 4 || v1Cleared[0].Remembered || v1Cleared[0].Release.ID != "kind:"+hash4K {
		t.Fatalf("viewer1 KindVersions after clear = %+v, err=%v", v1Cleared, err)
	}
}

func TestKindResolverOpenKindPartSourceAndBoundedPartIndex(t *testing.T) {
	payloads := map[string][]byte{
		"/book/epub":   []byte("EPUB-DATA"),
		"/book/pdf":    []byte("PDF-DATA"),
		"/multi/p0/hd": []byte("PART0-HD"),
		"/multi/p0/sd": []byte("PART0-SD"),
		"/multi/p1/hd": []byte("PART1-HD"),
		"/multi/p1/sd": []byte("PART1-SD"),
	}
	mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/book/broken" {
			http.Error(w, "upstream broken", http.StatusInternalServerError)
			return
		}
		data, ok := payloads[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, r.URL.Path, time.Unix(1_700_000_000, 0), bytes.NewReader(data))
	}))
	defer mediaSrv.Close()

	httpClient := &http.Client{
		Transport: hostRewriteTransport{target: mediaSrv.URL, base: mediaSrv.Client().Transport},
	}
	h := newKindTestHarness(t, 0, httpClient)
	ctx := context.Background()

	var (
		mu       sync.Mutex
		seenReqs []core.KindResolveRequest
	)
	h.rt.callContext = func(_ context.Context, req plugins.Request) (plugins.Response, error) {
		switch req.Path {
		case "/ether/health":
			return plugins.Response{Status: http.StatusOK, Body: json.RawMessage(`{"status":"ok"}`)}, nil
		case "/ether/v1/kinds/recordings/resolve":
			var body core.KindResolveRequest
			if err := json.Unmarshal(req.Body, &body); err != nil {
				return plugins.Response{Status: http.StatusBadRequest}, nil
			}
			mu.Lock()
			seenReqs = append(seenReqs, body)
			mu.Unlock()

			switch body.Item.ID {
			case "aware-book":
				// Resolver honours KindResolveRequest.Source when set.
				var srcs []core.PlayableSource
				switch body.Source {
				case "":
					srcs = []core.PlayableSource{
						{ID: "epub", Pattern: "range", MIME: "application/epub+zip", Size: int64(len(payloads["/book/epub"])), URL: "https://media.example/book/epub"},
						{ID: "pdf", Pattern: "range", MIME: "application/pdf", Size: int64(len(payloads["/book/pdf"])), URL: "https://media.example/book/pdf"},
					}
				case "pdf":
					srcs = []core.PlayableSource{
						{ID: "pdf", Pattern: "range", MIME: "application/pdf", Size: int64(len(payloads["/book/pdf"])), URL: "https://media.example/book/pdf"},
					}
				case "broken":
					srcs = []core.PlayableSource{
						{ID: "broken", Pattern: "range", MIME: "application/octet-stream", Size: 10, URL: "https://media.example/book/broken"},
						{ID: "epub", Pattern: "range", MIME: "application/epub+zip", Size: int64(len(payloads["/book/epub"])), URL: "https://media.example/book/epub"},
					}
				default:
					return plugins.Response{Status: http.StatusNotFound, Body: json.RawMessage(`{"sources":[]}`)}, nil
				}
				raw, _ := json.Marshal(core.KindResolveResponse{TTLSec: 600, Sources: srcs})
				return plugins.Response{Status: http.StatusOK, Body: raw}, nil

			case "ignore-book":
				// Legacy resolver ignores KindResolveRequest.Source and returns all sources.
				raw, _ := json.Marshal(core.KindResolveResponse{
					TTLSec: 600,
					Sources: []core.PlayableSource{
						{ID: "epub", Pattern: "range", MIME: "application/epub+zip", Size: int64(len(payloads["/book/epub"])), URL: "https://media.example/book/epub"},
						{ID: "broken", Pattern: "range", MIME: "application/octet-stream", Size: 10, URL: "https://media.example/book/broken"},
						{ID: "pdf", Pattern: "range", MIME: "application/pdf", Size: int64(len(payloads["/book/pdf"])), URL: "https://media.example/book/pdf"},
					},
				})
				return plugins.Response{Status: http.StatusOK, Body: raw}, nil

			case "multi-book":
				p := -1
				if body.PartIndex != nil {
					p = *body.PartIndex
				}
				if p < 0 || p > 1 {
					return plugins.Response{Status: http.StatusBadRequest}, nil
				}
				hdPath := "/multi/p" + string(rune('0'+p)) + "/hd"
				sdPath := "/multi/p" + string(rune('0'+p)) + "/sd"
				raw, _ := json.Marshal(core.KindResolveResponse{
					TTLSec: 600,
					Sources: []core.PlayableSource{
						{ID: "hd", Pattern: "range", MIME: "application/octet-stream", Size: int64(len(payloads[hdPath])), URL: "https://media.example" + hdPath},
						{ID: "sd", Pattern: "range", MIME: "application/octet-stream", Size: int64(len(payloads[sdPath])), URL: "https://media.example" + sdPath},
					},
				})
				return plugins.Response{Status: http.StatusOK, Body: raw}, nil
			}
			return plugins.Response{Status: http.StatusNotFound}, nil
		default:
			return plugins.Response{Status: http.StatusNotFound}, nil
		}
	}

	const pluginID = "community.example.sourcepick"
	decl := recordingsKindDecl(true)
	decl["sync"] = map[string]any{"mode": "push"}
	installStudioManifest(t, h.mgr, map[string]any{
		"id":      pluginID,
		"version": "1.0.0",
		"name":    "Source Pick Test",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "content",
			"summary":      "Source pick test",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"catalog_provider", "kind_resolver"},
			"permissions":  map[string]any{"network": []string{"media.example"}},
			"strings":      map[string]any{"en": "i18n/en.json"},
			"contributes":  map[string]any{"mediaKinds": []map[string]any{decl}},
		},
	}, map[string][]byte{"i18n/en.json": []byte(`{"lib_name":"Books"}`)})

	host := h.mgr.HostFor(pluginID)
	if _, err := host.KindUpsert(ctx, "recordings", []core.KindItem{
		{ID: "alb-1", Level: "album", Title: "Shelf"},
		{ID: "aware-book", Level: "track", ParentID: "alb-1", Index: 1, Title: "Aware Book", Playable: true},
		{ID: "ignore-book", Level: "track", ParentID: "alb-1", Index: 2, Title: "Legacy Book", Playable: true},
		{
			ID: "multi-book", Level: "track", ParentID: "alb-1", Index: 3, Title: "Multi Book", Playable: true,
			Hints: json.RawMessage(`{"parts":[{"name":"Part 1","size":8},{"name":"Part 2","size":8}]}`),
		},
	}); err != nil {
		t.Fatalf("KindUpsert: %v", err)
	}

	awareItem, _, err := h.items.GetByExternal(ctx, pluginID, "recordings", "aware-book")
	if err != nil {
		t.Fatal(err)
	}
	ignoreItem, _, err := h.items.GetByExternal(ctx, pluginID, "recordings", "ignore-book")
	if err != nil {
		t.Fatal(err)
	}
	multiItem, _, err := h.items.GetByExternal(ctx, pluginID, "recordings", "multi-book")
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.Viewer{ID: 1, IsOwner: true}

	readAll := func(t *testing.T, km interface {
		ReadAt([]byte, int64) (int, error)
		Close() error
	}, sz int64) string {
		t.Helper()
		defer km.Close()
		b := make([]byte, sz)
		if _, err := km.ReadAt(b, 0); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("ReadAt: %v", err)
		}
		return string(b)
	}

	// 1. Bounded PartIndex: playable item with NO declared parts rejects any partIndex without calling /resolve.
	if _, err := h.resolver.OpenKindPart(ctx, viewer, awareItem.Key, 0, nil); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("OpenKindPart on item with no parts err = %v, want ErrNotFound", err)
	}
	zero := 0
	if _, err := h.resolver.OpenKindPartSource(ctx, viewer, awareItem.Key, &zero, "pdf", nil); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("OpenKindPartSource with partIndex on item with no parts err = %v, want ErrNotFound", err)
	}
	mu.Lock()
	if len(seenReqs) != 0 {
		t.Fatalf("expected 0 /resolve calls for rejected partIndex, got %d", len(seenReqs))
	}
	mu.Unlock()

	// 2. Source-aware plugin receives KindResolveRequest.Source and opens only that source.
	kmPDF, err := h.resolver.OpenKindPartSource(ctx, viewer, awareItem.Key, nil, "pdf", nil)
	if err != nil {
		t.Fatalf("OpenKindPartSource(aware, pdf): %v", err)
	}
	if got := readAll(t, kmPDF, kmPDF.Size); got != "PDF-DATA" {
		t.Fatalf("aware pdf bytes = %q, want PDF-DATA", got)
	}
	mu.Lock()
	if len(seenReqs) != 1 || seenReqs[0].Source != "pdf" || seenReqs[0].PartIndex != nil {
		t.Fatalf("aware /resolve request = %+v, want Source=pdf PartIndex=nil", seenReqs)
	}
	mu.Unlock()

	// 3. Unknown source returns ErrNotAvailable and must NOT negative-cache the item for default OpenKind.
	if _, err := h.resolver.OpenKindPartSource(ctx, viewer, awareItem.Key, nil, "missing", nil); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("missing source err = %v, want ErrNotAvailable", err)
	}
	kmDefault, err := h.resolver.OpenKind(ctx, viewer, awareItem.Key, nil)
	if err != nil {
		t.Fatalf("default OpenKind after missing/source-scoped open: %v", err)
	}
	if got := readAll(t, kmDefault, kmDefault.Size); got != "EPUB-DATA" {
		t.Fatalf("default OpenKind bytes = %q, want EPUB-DATA (shared default untouched)", got)
	}

	// 4. Broken chosen source fails with NO failover to the healthy epub source.
	if _, err := h.resolver.OpenKindPartSource(ctx, viewer, awareItem.Key, nil, "broken", nil); err == nil {
		t.Fatal("broken source succeeded (unexpected failover)")
	}

	// Redeem a real adapter ticket and read the returned bytes. Query parameters
	// cannot expand the strict ticket into a preference.
	issuer := frontendadapter.NewTicketIssuer()
	handler := frontendadapter.NewMediaHandler(frontendadapter.MediaDeps{
		Tickets: issuer,
		Viewer:  func(context.Context, int64) (*core.Viewer, error) { return viewer, nil },
		Open: func(context.Context, *core.Viewer, string, []string) (*frontend.KindMedia, error) {
			t.Fatal("source-scoped ticket dispatched to whole-item open")
			return nil, nil
		},
		OpenSource: func(ctx context.Context, v *core.Viewer, ref string, part *int, source string, fallback bool, accept []string) (*frontend.KindMedia, error) {
			item, _, err := h.items.GetByRef(ctx, core.ItemRef(ref))
			if err != nil {
				return nil, err
			}
			return h.resolver.OpenKindPartSourceWithFallback(ctx, v, item.Key, part, source, fallback, accept)
		},
	})
	partOne := 1
	for _, tc := range []struct {
		ref      core.ItemRef
		part     *int
		source   string
		fallback bool
		want     string
	}{
		{awareItem.Ref, nil, "pdf", true, "PDF-DATA"},
		{awareItem.Ref, nil, "broken", true, "EPUB-DATA"},
		{awareItem.Ref, nil, "missing", true, "EPUB-DATA"},
		{multiItem.Ref, &partOne, "missing", true, "PART1-HD"},
		{awareItem.Ref, nil, "broken", false, ""},
	} {
		url, err := issuer.StreamURL(frontendadapter.StreamGrant{ViewerID: viewer.ID, ItemID: tc.ref.String(), Adapter: pluginID, PartIndex: tc.part, Source: tc.source, Fallback: tc.fallback})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.URL.RawQuery += "&fallback=true"
		handler.ServeHTTP(rec, req)
		if tc.want == "" {
			if rec.Code < 400 {
				t.Fatalf("strict broken source returned %d", rec.Code)
			}
		} else if rec.Code != http.StatusOK || rec.Body.String() != tc.want {
			t.Errorf("open %s preferred %q: status=%d body=%q want %q", tc.ref, tc.source, rec.Code, rec.Body.String(), tc.want)
		}
	}
	// 5. Plugin that ignores KindResolveRequest.Source still works: Ether picks matching id,
	// refuses failover on broken id, and returns ErrNotAvailable when absent.
	kmLegacyPDF, err := h.resolver.OpenKindPartSource(ctx, viewer, ignoreItem.Key, nil, "pdf", nil)
	if err != nil {
		t.Fatalf("OpenKindPartSource(ignore, pdf): %v", err)
	}
	if got := readAll(t, kmLegacyPDF, kmLegacyPDF.Size); got != "PDF-DATA" {
		t.Fatalf("legacy pdf bytes = %q, want PDF-DATA", got)
	}
	if _, err := h.resolver.OpenKindPartSource(ctx, viewer, ignoreItem.Key, nil, "broken", nil); err == nil {
		t.Fatal("legacy broken source succeeded (unexpected failover)")
	}
	if _, err := h.resolver.OpenKindPartSource(ctx, viewer, ignoreItem.Key, nil, "absent", nil); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("legacy absent source err = %v, want ErrNotAvailable", err)
	}

	// 6. Multi-part item combines partIndex + source, rejects missing partIndex (ErrNotAvailable) and out-of-range (ErrNotFound).
	part1 := 1
	kmMultiSD, err := h.resolver.OpenKindPartSource(ctx, viewer, multiItem.Key, &part1, "sd", nil)
	if err != nil {
		t.Fatalf("OpenKindPartSource(multi, part=1, sd): %v", err)
	}
	if got := readAll(t, kmMultiSD, kmMultiSD.Size); got != "PART1-SD" {
		t.Fatalf("multi part=1 sd bytes = %q, want PART1-SD", got)
	}
	if _, err := h.resolver.OpenKindPartSource(ctx, viewer, multiItem.Key, nil, "sd", nil); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("multi without partIndex err = %v, want ErrNotAvailable", err)
	}
	part9 := 9
	if _, err := h.resolver.OpenKindPartSource(ctx, viewer, multiItem.Key, &part9, "sd", nil); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("multi out-of-range partIndex err = %v, want ErrNotFound", err)
	}
}
