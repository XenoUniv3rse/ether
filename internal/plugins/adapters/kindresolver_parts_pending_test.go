package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/adapters"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
	"github.com/braedonsaunders/ether/internal/resolver"
)

func TestKindResolverMultiPartSequenceAndOpenKindPart(t *testing.T) {
	partPayloads := map[string][]byte{
		"/parts/p0.bin": []byte("Alpha-"),
		"/parts/p1.bin": []byte("Bravo-"),
		"/parts/p2.bin": []byte("Charlie"),
		"/parts/solo":   []byte("Solo"),
		"/parts/f0.bin": []byte("One+"),
		"/parts/f1.bin": []byte("Two"),
	}
	mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := partPayloads[r.URL.Path]
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
		mu        sync.Mutex
		seenParts = map[string][]int{}
	)
	h.rt.callContext = func(_ context.Context, req plugins.Request) (plugins.Response, error) {
		switch req.Path {
		case "/ether/health":
			return plugins.Response{Status: 200, Body: json.RawMessage(`{"status":"ok"}`)}, nil
		case "/ether/v1/kinds/recordings/resolve":
			var body core.KindResolveRequest
			if err := json.Unmarshal(req.Body, &body); err != nil {
				return plugins.Response{Status: 400}, nil
			}
			partVal := -1
			if body.PartIndex != nil {
				partVal = *body.PartIndex
			}
			mu.Lock()
			seenParts[body.Item.ID] = append(seenParts[body.Item.ID], partVal)
			mu.Unlock()

			switch body.Item.ID {
			case "multi-3":
				if partVal < 0 || partVal > 2 {
					return plugins.Response{Status: 400, Body: json.RawMessage(`{"error":"missing partIndex"}`)}, nil
				}
				u := fmt.Sprintf("https://media.example/parts/p%d.bin", partVal)
				sz := len(partPayloads[fmt.Sprintf("/parts/p%d.bin", partVal)])
				return plugins.Response{
					Status: 200,
					Body:   json.RawMessage(fmt.Sprintf(`{"sources":[{"id":"s-%d","pattern":"range","mime":"application/octet-stream","size":%d,"url":%q}]}`, partVal, sz, u)),
				}, nil
			case "solo-1":
				if partVal != 0 {
					return plugins.Response{Status: 400, Body: json.RawMessage(`{"error":"expected partIndex 0"}`)}, nil
				}
				return plugins.Response{
					Status: 200,
					Body:   json.RawMessage(`{"sources":[{"id":"solo","pattern":"range","mime":"application/octet-stream","size":4,"url":"https://media.example/parts/solo"}]}`),
				}, nil
			case "field-2":
				if partVal < 0 || partVal > 1 {
					return plugins.Response{Status: 400, Body: json.RawMessage(`{"error":"missing partIndex"}`)}, nil
				}
				u := fmt.Sprintf("https://media.example/parts/f%d.bin", partVal)
				sz := len(partPayloads[fmt.Sprintf("/parts/f%d.bin", partVal)])
				return plugins.Response{
					Status: 200,
					Body:   json.RawMessage(fmt.Sprintf(`{"sources":[{"id":"f-%d","pattern":"range","mime":"application/octet-stream","size":%d,"url":%q}]}`, partVal, sz, u)),
				}, nil
			}
			return plugins.Response{Status: 404, Body: json.RawMessage(`{"error":"not found"}`)}, nil
		default:
			return plugins.Response{Status: 200, Body: json.RawMessage(`{"items":[]}`)}, nil
		}
	}

	const pluginID = "community.example.multipart"
	decl := recordingsKindDecl(true)
	decl["sync"] = map[string]any{"mode": "push"}
	decl["fields"] = append(decl["fields"].([]map[string]any), map[string]any{
		"key": "parts", "type": "strings", "label": "Parts",
	})
	installStudioManifest(t, h.mgr, map[string]any{
		"id":      pluginID,
		"version": "1.0.0",
		"name":    "Multipart Recordings",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "content",
			"summary":      "Multipart test kind",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"catalog_provider", "kind_resolver"},
			"permissions":  map[string]any{"network": []string{"media.example"}},
			"strings":      map[string]any{"en": "i18n/en.json"},
			"contributes":  map[string]any{"mediaKinds": []map[string]any{decl}},
		},
	}, map[string][]byte{"i18n/en.json": []byte(`{"lib_name":"Multipart"}`)})

	host := h.mgr.HostFor(pluginID)
	if _, err := host.KindUpsert(ctx, "recordings", []core.KindItem{
		{ID: "alb-1", Level: "album", Title: "Set 1"},
		{
			ID: "multi-3", Level: "track", ParentID: "alb-1", Index: 1, Title: "Three Discs", Playable: true,
			Hints: json.RawMessage(`{"parts":[` +
				`{"name":"Disc 1","size":6,"mime":"application/octet-stream"},` +
				`{"name":"Disc 2","size":6,"mime":"application/octet-stream"},` +
				`{"name":"Disc 3","size":7,"mime":"application/octet-stream"}` +
				`]}`),
		},
		{
			ID: "solo-1", Level: "track", ParentID: "alb-1", Index: 2, Title: "Single Disc", Playable: true,
			Hints: json.RawMessage(`{"parts":[{"name":"Disc 1","size":4,"mime":"application/octet-stream"}]}`),
		},
		{
			ID: "field-2", Level: "track", ParentID: "alb-1", Index: 3, Title: "Field Parts", Playable: true,
			Fields: map[string]json.RawMessage{
				"parts": json.RawMessage(`["Side A","Side B"]`),
			},
		},
	}); err != nil {
		t.Fatalf("KindUpsert: %v", err)
	}

	multiItem, _, err := h.items.GetByExternal(ctx, pluginID, "recordings", "multi-3")
	if err != nil {
		t.Fatalf("GetByExternal(multi-3): %v", err)
	}
	soloItem, _, err := h.items.GetByExternal(ctx, pluginID, "recordings", "solo-1")
	if err != nil {
		t.Fatalf("GetByExternal(solo-1): %v", err)
	}
	fieldItem, _, err := h.items.GetByExternal(ctx, pluginID, "recordings", "field-2")
	if err != nil {
		t.Fatalf("GetByExternal(field-2): %v", err)
	}
	viewer := &core.Viewer{ID: 1, IsOwner: true}

	// 1. KindParts reports declared parts in order.
	parts, err := h.resolver.KindParts(ctx, viewer, multiItem.Key)
	if err != nil || len(parts) != 3 {
		t.Fatalf("KindParts = %+v, err = %v", parts, err)
	}
	if parts[0].Index != 0 || parts[0].Name != "Disc 1" || parts[2].Index != 2 || parts[2].Size != 7 {
		t.Fatalf("unexpected KindParts metadata: %+v", parts)
	}

	// 2. Single-part hints.parts resolves with PartIndex=0.
	soloMedia, err := h.resolver.OpenKind(ctx, viewer, soloItem.Key, nil)
	if err != nil {
		t.Fatalf("OpenKind(solo-1): %v", err)
	}
	defer soloMedia.Closer.Close()
	soloBytes, err := io.ReadAll(io.NewSectionReader(soloMedia.ReaderAt, 0, soloMedia.Size))
	if err != nil || string(soloBytes) != "Solo" {
		t.Fatalf("solo read = %q, err = %v", soloBytes, err)
	}

	// 3. Multi-part OpenKind builds a concatenated Sequence across parts 0..2.
	multiMedia, err := h.resolver.OpenKind(ctx, viewer, multiItem.Key, nil)
	if err != nil {
		t.Fatalf("OpenKind(multi-3): %v", err)
	}
	defer multiMedia.Closer.Close()
	if multiMedia.Size != 19 {
		t.Fatalf("multiMedia.Size = %d, want 19", multiMedia.Size)
	}
	allBytes, err := io.ReadAll(io.NewSectionReader(multiMedia.ReaderAt, 0, multiMedia.Size))
	if err != nil || string(allBytes) != "Alpha-Bravo-Charlie" {
		t.Fatalf("concatenated read = %q, err = %v", allBytes, err)
	}
	// Range read spanning part 0 -> part 1 -> part 2 boundary.
	span := make([]byte, 10)
	if n, err := multiMedia.ReaderAt.ReadAt(span, 4); err != nil || n != 10 || string(span) != "a-Bravo-Ch" {
		t.Fatalf("cross-part ReadAt = %q (n=%d, err=%v), want %q", span[:n], n, err, "a-Bravo-Ch")
	}

	mu.Lock()
	gotMultiCalls := append([]int(nil), seenParts["multi-3"]...)
	gotSoloCalls := append([]int(nil), seenParts["solo-1"]...)
	mu.Unlock()
	if len(gotSoloCalls) != 1 || gotSoloCalls[0] != 0 {
		t.Fatalf("solo-1 resolve PartIndex calls = %v, want [0]", gotSoloCalls)
	}
	if len(gotMultiCalls) != 3 || gotMultiCalls[0] != 0 || gotMultiCalls[1] != 1 || gotMultiCalls[2] != 2 {
		t.Fatalf("multi-3 resolve PartIndex calls = %v, want [0 1 2]", gotMultiCalls)
	}

	// 4. OpenKindPart serves an individual part and reuses its per-part cached resolution.
	part1Media, err := h.resolver.OpenKindPart(ctx, viewer, multiItem.Key, 1, nil)
	if err != nil {
		t.Fatalf("OpenKindPart(1): %v", err)
	}
	defer part1Media.Closer.Close()
	part1Bytes, err := io.ReadAll(io.NewSectionReader(part1Media.ReaderAt, 0, part1Media.Size))
	if err != nil || string(part1Bytes) != "Bravo-" {
		t.Fatalf("OpenKindPart(1) bytes = %q, err = %v", part1Bytes, err)
	}
	mu.Lock()
	afterCachedPart := len(seenParts["multi-3"])
	mu.Unlock()
	if afterCachedPart != 3 {
		t.Fatalf("expected cached #part:1 resolution reuse, got %d total calls", afterCachedPart)
	}

	// Out-of-bounds part indices return ErrNotFound.
	for _, bad := range []int{-1, 3} {
		if _, err := h.resolver.OpenKindPart(ctx, viewer, multiItem.Key, bad, nil); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("OpenKindPart(%d) err = %v, want ErrNotFound", bad, err)
		}
	}

	// 5. Fallback to declared fields["parts"] also resolves per part.
	fieldPart1, err := h.resolver.OpenKindPart(ctx, viewer, fieldItem.Key, 1, nil)
	if err != nil {
		t.Fatalf("OpenKindPart(field-2, 1): %v", err)
	}
	defer fieldPart1.Closer.Close()
	fb, err := io.ReadAll(io.NewSectionReader(fieldPart1.ReaderAt, 0, fieldPart1.Size))
	if err != nil || string(fb) != "Two" {
		t.Fatalf("field-2 part 1 = %q, err = %v", fb, err)
	}
}

func TestKindResolverPendingReturnsErrPreparingNotNoSource(t *testing.T) {
	mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/flac")
		http.ServeContent(w, r, "ready.flac", time.Unix(1_700_000_000, 0), bytes.NewReader([]byte("ready-audio")))
	}))
	defer mediaSrv.Close()

	httpClient := &http.Client{
		Transport: hostRewriteTransport{target: mediaSrv.URL, base: mediaSrv.Client().Transport},
	}
	h := newKindTestHarness(t, 0, httpClient)
	ctx := context.Background()

	var mode atomic.Int32 // 0 = budget-expired pending, 1 = ready, 2 = empty no_source
	h.rt.callContext = func(_ context.Context, req plugins.Request) (plugins.Response, error) {
		switch req.Path {
		case "/ether/health":
			return plugins.Response{Status: 200, Body: json.RawMessage(`{"status":"ok"}`)}, nil
		case "/ether/v1/kinds/recordings/resolve":
			switch mode.Load() {
			case 0:
				return plugins.Response{
					Status: 200,
					Body:   json.RawMessage(`{"pending":true,"retryInMs":4000}`),
				}, nil
			case 1:
				return plugins.Response{
					Status: 200,
					Body:   json.RawMessage(`{"sources":[{"id":"ready","pattern":"range","mime":"audio/flac","size":11,"url":"https://media.example/ready.flac"}]}`),
				}, nil
			default:
				return plugins.Response{
					Status: 200,
					Body:   json.RawMessage(`{"sources":[]}`),
				}, nil
			}
		default:
			return plugins.Response{Status: 200, Body: json.RawMessage(`{"items":[]}`)}, nil
		}
	}

	const pluginID = "community.example.pending"
	decl := recordingsKindDecl(true)
	decl["sync"] = map[string]any{"mode": "push"}
	installStudioManifest(t, h.mgr, map[string]any{
		"id":      pluginID,
		"version": "1.0.0",
		"name":    "Pending Recordings",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "content",
			"summary":      "Pending resolver test",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"catalog_provider", "kind_resolver"},
			"permissions":  map[string]any{"network": []string{"media.example"}},
			"strings":      map[string]any{"en": "i18n/en.json"},
			"contributes":  map[string]any{"mediaKinds": []map[string]any{decl}},
		},
	}, map[string][]byte{"i18n/en.json": []byte(`{"lib_name":"Pending"}`)})

	host := h.mgr.HostFor(pluginID)
	if _, err := host.KindUpsert(ctx, "recordings", []core.KindItem{
		{ID: "alb-1", Level: "album", Title: "Album"},
		{ID: "slow-1", Level: "track", ParentID: "alb-1", Index: 1, Title: "Slow Track", Playable: true},
		{ID: "empty-1", Level: "track", ParentID: "alb-1", Index: 2, Title: "Empty Track", Playable: true},
	}); err != nil {
		t.Fatalf("KindUpsert: %v", err)
	}

	slowItem, _, err := h.items.GetByExternal(ctx, pluginID, "recordings", "slow-1")
	if err != nil {
		t.Fatalf("GetByExternal(slow-1): %v", err)
	}
	emptyItem, _, err := h.items.GetByExternal(ctx, pluginID, "recordings", "empty-1")
	if err != nil {
		t.Fatalf("GetByExternal(empty-1): %v", err)
	}
	viewer := &core.Viewer{ID: 1, IsOwner: true}

	// 1. When /resolve is still pending when ResolverTotal expires, OpenKind
	// returns ErrPreparing (NOT ErrNotAvailable) with retry advice and does
	// NOT negative-cache the item.
	_, err = h.resolver.OpenKind(ctx, viewer, slowItem.Key, nil)
	if !errors.Is(err, core.ErrPreparing) {
		t.Fatalf("pending expiry OpenKind err = %v, want core.ErrPreparing", err)
	}
	if errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("pending expiry must not masquerade as core.ErrNotAvailable: %v", err)
	}
	if got := core.PreparingRetrySeconds(err, 5); got != 4 {
		t.Fatalf("PreparingRetrySeconds = %d, want 4", got)
	}

	// 2. Subsequent call once the plugin finishes succeeds immediately (proving
	// pending was never negative-cached).
	mode.Store(1)
	readyMedia, err := h.resolver.OpenKind(ctx, viewer, slowItem.Key, nil)
	if err != nil {
		t.Fatalf("OpenKind after pending completed: %v", err)
	}
	defer readyMedia.Closer.Close()
	got, err := io.ReadAll(io.NewSectionReader(readyMedia.ReaderAt, 0, readyMedia.Size))
	if err != nil || string(got) != "ready-audio" {
		t.Fatalf("ready media = %q, err = %v", got, err)
	}

	// 3. Real empty sources[] still returns ErrNotAvailable (409 no_source).
	mode.Store(2)
	_, err = h.resolver.OpenKind(ctx, viewer, emptyItem.Key, nil)
	if !errors.Is(err, core.ErrNotAvailable) || errors.Is(err, core.ErrPreparing) {
		t.Fatalf("empty sources OpenKind err = %v, want core.ErrNotAvailable", err)
	}
}

type pendingSearchIndexer struct {
	pending  atomic.Bool
	timedOut atomic.Bool
}

func (p *pendingSearchIndexer) Name() string { return "pending-indexer" }
func (p *pendingSearchIndexer) Search(_ context.Context, _ core.SearchRequest) ([]core.Release, error) {
	return nil, nil
}
func (p *pendingSearchIndexer) FetchNZB(_ context.Context, _ core.Release) ([]byte, error) {
	return nil, core.ErrNotAvailable
}
func (p *pendingSearchIndexer) SearchWithOptions(_ context.Context, _ core.SearchRequest, _ core.IndexerSearchOptions) (core.IndexerSearchResult, error) {
	if p.pending.Load() {
		return core.IndexerSearchResult{
			Indexers: []core.IndexerSearchStatus{{IndexerID: "slow", Indexer: "Slow", Status: core.IndexerStatusPending}},
			Pending:  []core.IndexerRef{{IndexerID: "slow", Indexer: "Slow"}},
		}, nil
	}
	if p.timedOut.Load() {
		return core.IndexerSearchResult{Indexers: []core.IndexerSearchStatus{
			{IndexerID: "fast", Status: core.IndexerStatusAnswered},
			{IndexerID: "slow", ConfiguredName: "Test indexer", Status: core.IndexerStatusTimedOut},
		}}, nil
	}
	return core.IndexerSearchResult{
		Indexers: []core.IndexerSearchStatus{{IndexerID: "slow", Indexer: "Slow", Status: core.IndexerStatusAnswered}},
	}, nil
}

func TestKindResolverDeadlinePendingAndSpeculativeNotNegativeCached(t *testing.T) {
	mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "concert.mp4", time.Unix(1_700_000_000, 0), bytes.NewReader([]byte("concert-bytes")))
	}))
	defer mediaSrv.Close()

	httpClient := &http.Client{
		Transport: hostRewriteTransport{target: mediaSrv.URL, base: mediaSrv.Client().Transport},
	}
	h := newKindTestHarness(t, 0, httpClient)
	ctx := context.Background()

	idx := &pendingSearchIndexer{}
	idx.pending.Store(true)
	history := resolver.New(idx, nil, nil, config.Defaults(""), resolver.Options{})
	defer history.Close()
	kr := adapters.NewKindResolver(adapters.KindResolverDeps{
		Manager:   h.mgr,
		Libraries: h.libs,
		Items:     h.items,
		Blobs:     h.blobs,
		Catalog:   h.catalog,
		Indexer:   idx,
		Resolver:  history,
		HTTP:      httpClient,
	})
	defer kr.Close()

	var (
		mode        atomic.Int32 // 0 = DeadlineExceeded, 1 = indexer_search pending + empty, 2 = empty, 3 = ready
		resolveHits atomic.Int32
	)
	const pluginID = "community.example.preparing"
	h.rt.callContext = func(callCtx context.Context, req plugins.Request) (plugins.Response, error) {
		switch req.Path {
		case "/ether/health":
			return plugins.Response{Status: 200, Body: json.RawMessage(`{"status":"ok"}`)}, nil
		case "/ether/v1/kinds/recordings/resolve":
			resolveHits.Add(1)
			switch mode.Load() {
			case 0:
				return plugins.Response{}, context.DeadlineExceeded
			case 1:
				if _, err := kr.ResolveSourcesWithResult(callCtx, pluginID, core.SourceQuery{
					Kind:  "indexer_search",
					Query: "Metallica S&M",
				}); err != nil {
					return plugins.Response{}, err
				}
				return plugins.Response{Status: 200, Body: json.RawMessage(`{"sources":[]}`)}, nil
			case 2:
				return plugins.Response{Status: 200, Body: json.RawMessage(`{"sources":[]}`)}, nil
			default:
				return plugins.Response{
					Status: 200,
					Body:   json.RawMessage(`{"sources":[{"id":"concert-1","pattern":"range","mime":"video/mp4","size":13,"url":"https://media.example/concert.mp4"}]}`),
				}, nil
			}
		default:
			return plugins.Response{Status: 200, Body: json.RawMessage(`{"items":[]}`)}, nil
		}
	}

	decl := recordingsKindDecl(true)
	decl["sync"] = map[string]any{"mode": "push"}
	installStudioManifest(t, h.mgr, map[string]any{
		"id":      pluginID,
		"version": "1.0.0",
		"name":    "Preparing Concerts",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "content",
			"summary":      "Preparing vs not available test",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"catalog_provider", "kind_resolver"},
			"permissions":  map[string]any{"network": []string{"media.example"}, "sources": "search"},
			"strings":      map[string]any{"en": "i18n/en.json"},
			"contributes":  map[string]any{"mediaKinds": []map[string]any{decl}},
		},
	}, map[string][]byte{"i18n/en.json": []byte(`{"lib_name":"Concerts"}`)})

	host := h.mgr.HostFor(pluginID)
	if _, err := host.KindUpsert(ctx, "recordings", []core.KindItem{
		{ID: "alb-1", Level: "album", Title: "Concerts"},
		{ID: "deadline-1", Level: "track", ParentID: "alb-1", Index: 1, Title: "Deadline Concert", Playable: true},
		{ID: "pending-1", Level: "track", ParentID: "alb-1", Index: 2, Title: "Pending Search Concert", Playable: true},
		{ID: "spec-1", Level: "track", ParentID: "alb-1", Index: 3, Title: "Speculative Miss Concert", Playable: true},
	}); err != nil {
		t.Fatalf("KindUpsert: %v", err)
	}

	viewer := &core.Viewer{ID: 1, IsOwner: true}
	deadlineItem, _, _ := h.items.GetByExternal(ctx, pluginID, "recordings", "deadline-1")
	pendingItem, _, _ := h.items.GetByExternal(ctx, pluginID, "recordings", "pending-1")
	specItem, _, _ := h.items.GetByExternal(ctx, pluginID, "recordings", "spec-1")

	// 1. /resolve hitting context.DeadlineExceeded must surface ErrPreparing
	// (503 + Retry-After), never terminal ErrNotAvailable (409), and must not
	// poison negative cache.
	mode.Store(0)
	_, err := kr.OpenKind(ctx, viewer, deadlineItem.Key, nil)
	if !errors.Is(err, core.ErrPreparing) || errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("deadline OpenKind err = %v, want core.ErrPreparing (not ErrNotAvailable)", err)
	}
	if _, ok, rerr := h.items.GetPartResolution(ctx, deadlineItem.Ref, nil); ok || rerr != nil {
		t.Fatalf("deadline cached negative resolution: ok=%v err=%v", ok, rerr)
	}

	// 2. /resolve returning empty sources while an indexer search still has
	// pending background indexers must return ErrPreparing (not ErrNotAvailable)
	// and must NOT save a 60s negative cache row.
	mode.Store(1)
	_, err = kr.OpenKind(ctx, viewer, pendingItem.Key, nil)
	if !errors.Is(err, core.ErrPreparing) || errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("pending-indexer OpenKind err = %v, want core.ErrPreparing (not ErrNotAvailable)", err)
	}
	if _, ok, rerr := h.items.GetPartResolution(ctx, pendingItem.Ref, nil); ok || rerr != nil {
		t.Fatalf("pending-indexer saved negative resolution: ok=%v err=%v", ok, rerr)
	}
	// A terminal timeout alongside an answered indexer is still incomplete.
	idx.pending.Store(false)
	idx.timedOut.Store(true)
	if _, err := kr.OpenKind(ctx, viewer, pendingItem.Key, nil); !errors.Is(err, core.ErrPreparing) {
		t.Fatalf("partially timed-out OpenKind error = %v", err)
	}
	if _, found, err := h.items.GetPartResolution(ctx, pendingItem.Ref, nil); found || err != nil {
		t.Fatalf("partial search cached a negative resolution: found=%v err=%v", found, err)
	}
	rows, _, err := history.History(ctx, resolver.HistoryFilter{TitleKey: pendingItem.Key})
	if err != nil || len(rows) == 0 || !strings.Contains(rows[0].Error, "timed out (Test indexer)") {
		t.Fatalf("timeout missing from filtered history: %+v, %v", rows, err)
	}

	// Once the search settles with a playable source, Play immediately opens it.
	idx.timedOut.Store(false)
	mode.Store(3)
	media, err := kr.OpenKind(ctx, viewer, pendingItem.Key, nil)
	if err != nil {
		t.Fatalf("OpenKind after pending search settled: %v", err)
	}
	_ = media.Closer.Close()
	rows, _, err = history.History(ctx, resolver.HistoryFilter{TitleKey: pendingItem.Key})
	if err != nil || len(rows) == 0 || rows[0].ReleaseID == "" || rows[0].Error != "" {
		t.Fatalf("successful play missing from filtered history: %+v, %v", rows, err)
	}
	entry, err := history.HistoryEntry(ctx, rows[0].ID)
	if err != nil || entry.Trace == nil || !entry.Trace.PluginKind ||
		len(entry.Trace.Candidates) == 0 || entry.Trace.Candidates[0].ReleaseID != entry.Trace.Winner {
		t.Fatalf("successful play missing its winner trace: %+v, %v", entry, err)
	}

	// 3. A speculative detail warm (KindVersions with SpeculativeIndexerWork)
	// returning 0 sources must NOT persist a 60s negative cache row that blocks
	// a subsequent explicit Play.
	mode.Store(2)
	specCtx := core.WithIndexerPurpose(ctx, core.IndexerPurposeDetail)
	if _, err := kr.KindVersions(specCtx, viewer, specItem.Key, nil, true); err != nil {
		t.Fatalf("speculative KindVersions: %v", err)
	}
	if _, ok, rerr := h.items.GetPartResolution(ctx, specItem.Ref, nil); ok || rerr != nil {
		t.Fatalf("speculative detail miss persisted negative cache: ok=%v err=%v", ok, rerr)
	}
	// Explicit Play runs /resolve fresh and succeeds.
	mode.Store(3)
	media2, err := kr.OpenKind(core.WithIndexerPurpose(ctx, core.IndexerPurposePlayback), viewer, specItem.Key, nil)
	if err != nil {
		t.Fatalf("explicit Play after speculative miss: %v", err)
	}
	_ = media2.Closer.Close()

	// 4. Genuine non-speculative empty result with no pending indexers still
	// returns ErrNotAvailable and caches negatively.
	mode.Store(2)
	_, err = kr.OpenKind(core.WithIndexerPurpose(ctx, core.IndexerPurposePlayback), viewer, deadlineItem.Key, nil)
	if !errors.Is(err, core.ErrNotAvailable) || errors.Is(err, core.ErrPreparing) {
		t.Fatalf("genuine empty Play err = %v, want core.ErrNotAvailable", err)
	}
	if _, ok, rerr := h.items.GetPartResolution(ctx, deadlineItem.Ref, nil); !ok || !errors.Is(rerr, core.ErrNotAvailable) {
		t.Fatalf("genuine empty Play negative cache = (ok=%v, err=%v), want (true, ErrNotAvailable)", ok, rerr)
	}
}

type blockingUsenetCheckIndexer struct {
	mu      sync.Mutex
	fetches int
	nzbs    map[string][]byte
}

func (i *blockingUsenetCheckIndexer) Name() string { return "blocking-usenet-idx" }
func (i *blockingUsenetCheckIndexer) Search(_ context.Context, _ core.SearchRequest) ([]core.Release, error) {
	return nil, nil
}
func (i *blockingUsenetCheckIndexer) FetchNZB(_ context.Context, rel core.Release) ([]byte, error) {
	i.mu.Lock()
	i.fetches++
	data := i.nzbs[rel.NZBURL]
	i.mu.Unlock()
	if len(data) == 0 {
		return nil, core.ErrNotFound
	}
	return data, nil
}
func (i *blockingUsenetCheckIndexer) fetchCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.fetches
}

type blockingUsenetCheckEngine struct {
	mu           sync.Mutex
	checks       int
	started      chan struct{}
	release      chan struct{}
	finished     chan struct{}
	ctxCanceled  atomic.Bool
	checkVerdict core.UsenetCheckResult
}

func (e *blockingUsenetCheckEngine) Open(_ context.Context, _ string, _ []byte) (core.MediaSource, error) {
	return nil, core.ErrNotAvailable
}
func (e *blockingUsenetCheckEngine) Prefetch(_ context.Context, _ string, _ []byte, _, _ int64) error {
	return nil
}
func (e *blockingUsenetCheckEngine) CheckArticles(ctx context.Context, _ string, _ []byte) (core.UsenetCheckResult, error) {
	e.mu.Lock()
	e.checks++
	e.mu.Unlock()
	select {
	case e.started <- struct{}{}:
	default:
	}
	select {
	case <-e.release:
		select {
		case e.finished <- struct{}{}:
		default:
		}
		return e.checkVerdict, nil
	case <-ctx.Done():
		e.ctxCanceled.Store(true)
		return core.UsenetCheckResult{}, ctx.Err()
	}
}
func (e *blockingUsenetCheckEngine) checkCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.checks
}

func TestKindResolverGuestTimeoutDuringUsenetCheckPreparesAndCaches(t *testing.T) {
	mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/flac")
		http.ServeContent(w, r, "track01.flac", time.Unix(1_700_000_000, 0), bytes.NewReader([]byte("flac-track-bytes")))
	}))
	defer mediaSrv.Close()

	httpClient := &http.Client{
		Transport: hostRewriteTransport{target: mediaSrv.URL, base: mediaSrv.Client().Transport},
	}
	h := newKindTestHarness(t, 0, httpClient)
	ctx := context.Background()

	const nzbURL = "https://idx.example/joshua-tree.nzb"
	nzbXML := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="p" date="1700000000" subject="track01.flac (1/1)">
    <groups><group>alt.binaries.music</group></groups>
    <segments><segment bytes="1024" number="1">seg-1@test</segment></segments>
  </file>
</nzb>`)

	idx := &blockingUsenetCheckIndexer{
		nzbs: map[string][]byte{
			nzbURL: nzbXML,
		},
	}
	eng := &blockingUsenetCheckEngine{
		started:      make(chan struct{}, 1),
		release:      make(chan struct{}),
		finished:     make(chan struct{}, 1),
		checkVerdict: core.UsenetCheckResult{Available: true, Checked: 32, Missing: 0},
	}

	var nowMu sync.Mutex
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	advanceClock := func(d time.Duration) {
		nowMu.Lock()
		now = now.Add(d)
		nowMu.Unlock()
	}

	kr := adapters.NewKindResolver(adapters.KindResolverDeps{
		Manager:   h.mgr,
		Libraries: h.libs,
		Items:     h.items,
		Blobs:     h.blobs,
		Catalog:   h.catalog,
		Indexer:   idx,
		Engine:    eng,
		HTTP:      httpClient,
		Now:       clock,
	})
	defer kr.Close()

	var (
		mode          atomic.Int32 // 0 = guest timeout mid-usenet_check, 1 = next slice reads cached verdict, 2 = timeout with no source work, 3 = timeout repeating cached check, 4 = timeout with fresh check key
		slice2Verdict core.UsenetCheckResult
		slice2Mu      sync.Mutex
		freshCheckSeq atomic.Int32
	)
	const pluginID = "community.example.usenettimeout"
	timeoutErr := &wasm.Error{
		Status:  wasm.StatusTimeout,
		Message: "call exceeded its 5 s deadline for /ether/v1/kinds/recordings/resolve",
		Trap:    true,
	}

	h.rt.callContext = func(callCtx context.Context, req plugins.Request) (plugins.Response, error) {
		switch req.Path {
		case "/ether/health":
			return plugins.Response{Status: 200, Body: json.RawMessage(`{"status":"ok"}`)}, nil
		case "/ether/v1/kinds/recordings/resolve":
			switch mode.Load() {
			case 0:
				// Simulate a WASM guest whose 5 s deadline expires while
				// usenet_check is in flight: cancel only the slice context,
				// then return wasm.StatusTimeout.
				sliceCtx, sliceCancel := context.WithCancel(callCtx)
				errCh := make(chan error, 1)
				go func() {
					_, err := kr.ResolveSources(sliceCtx, pluginID, core.SourceQuery{
						Kind:   "usenet_check",
						NZBURL: nzbURL,
					})
					errCh <- err
				}()
				<-eng.started
				sliceCancel()
				<-errCh
				return plugins.Response{}, timeoutErr

			case 1:
				cands, err := kr.ResolveSources(callCtx, pluginID, core.SourceQuery{
					Kind:   "usenet_check",
					NZBURL: nzbURL,
				})
				if err != nil {
					return plugins.Response{}, err
				}
				if len(cands) != 1 || cands[0].UsenetCheck == nil {
					return plugins.Response{}, fmt.Errorf("unexpected usenet_check result: %+v", cands)
				}
				slice2Mu.Lock()
				slice2Verdict = *cands[0].UsenetCheck
				slice2Mu.Unlock()
				return plugins.Response{
					Status: 200,
					Body:   json.RawMessage(`{"sources":[{"id":"track-1","pattern":"range","mime":"audio/flac","size":16,"url":"https://media.example/track01.flac"}]}`),
				}, nil

			case 2:
				// Guest timed out without making any host source call.
				return plugins.Response{}, timeoutErr

			case 3:
				// Guest hits only the already-cached usenet_check and times out.
				if _, err := kr.ResolveSources(callCtx, pluginID, core.SourceQuery{
					Kind:   "usenet_check",
					NZBURL: nzbURL,
				}); err != nil {
					return plugins.Response{}, err
				}
				return plugins.Response{}, timeoutErr

			case 4:
				// Guest starts distinct uncached source work and times out.
				seq := freshCheckSeq.Add(1)
				_, _ = kr.ResolveSourcesWithResult(callCtx, pluginID, core.SourceQuery{
					Kind:  "indexer_search",
					Query: fmt.Sprintf("fresh-budget-query-%d", seq),
				})
				return plugins.Response{}, timeoutErr
			}
		}
		return plugins.Response{Status: 200, Body: json.RawMessage(`{"items":[]}`)}, nil
	}

	decl := recordingsKindDecl(true)
	decl["sync"] = map[string]any{"mode": "push"}
	installStudioManifest(t, h.mgr, map[string]any{
		"id":      pluginID,
		"version": "1.0.0",
		"name":    "Usenet Timeout Music",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "content",
			"summary":      "Guest timeout during usenet_check test",
			"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
			"capabilities": []string{"catalog_provider", "kind_resolver"},
			"permissions":  map[string]any{"network": []string{"media.example"}, "sources": "search"},
			"strings":      map[string]any{"en": "i18n/en.json"},
			"contributes":  map[string]any{"mediaKinds": []map[string]any{decl}},
		},
	}, map[string][]byte{"i18n/en.json": []byte(`{"lib_name":"Music"}`)})

	host := h.mgr.HostFor(pluginID)
	if _, err := host.KindUpsert(ctx, "recordings", []core.KindItem{
		{ID: "alb-jt", Level: "album", Title: "The Joshua Tree"},
		{ID: "trk-1", Level: "track", ParentID: "alb-jt", Index: 1, Title: "Where the Streets Have No Name", Playable: true},
		{ID: "trk-idle", Level: "track", ParentID: "alb-jt", Index: 2, Title: "Hung Track", Playable: true},
		{ID: "trk-stall", Level: "track", ParentID: "alb-jt", Index: 3, Title: "Stalled Track", Playable: true},
		{ID: "trk-budget", Level: "track", ParentID: "alb-jt", Index: 4, Title: "Budget Track", Playable: true},
	}); err != nil {
		t.Fatalf("KindUpsert: %v", err)
	}

	viewer := &core.Viewer{ID: 1, IsOwner: true}
	trk1, _, _ := h.items.GetByExternal(ctx, pluginID, "recordings", "trk-1")
	trkIdle, _, _ := h.items.GetByExternal(ctx, pluginID, "recordings", "trk-idle")
	trkStall, _, _ := h.items.GetByExternal(ctx, pluginID, "recordings", "trk-stall")
	trkBudget, _, _ := h.items.GetByExternal(ctx, pluginID, "recordings", "trk-budget")

	// 1. Slice 1 times out during an in-flight usenet_check -> PreparingError
	// (never ErrSourceUnavailable), and does not poison negative cache.
	mode.Store(0)
	_, err := kr.OpenKind(ctx, viewer, trk1.Key, nil)
	var prepErr *core.PreparingError
	if !errors.Is(err, core.ErrPreparing) || !errors.As(err, &prepErr) || prepErr.RetryAfter <= 0 {
		t.Fatalf("slice 1 OpenKind err = %v, want *core.PreparingError", err)
	}
	if errors.Is(err, core.ErrSourceUnavailable) {
		t.Fatalf("slice 1 OpenKind surfaced ErrSourceUnavailable: %v", err)
	}
	if eng.ctxCanceled.Load() {
		t.Fatal("in-flight usenet_check context was canceled by the guest deadline; expected background survival")
	}

	// Let the surviving background usenet_check finish and cache its verdict.
	close(eng.release)
	<-eng.finished

	// 2. Next slice immediately receives the cached verdict without re-fetching
	// the NZB or re-checking articles, and opens the track.
	mode.Store(1)
	media, err := kr.OpenKind(ctx, viewer, trk1.Key, nil)
	if err != nil {
		t.Fatalf("slice 2 OpenKind: %v", err)
	}
	_ = media.Closer.Close()

	slice2Mu.Lock()
	gotVerdict := slice2Verdict
	slice2Mu.Unlock()
	if !gotVerdict.Cached || !gotVerdict.Available || gotVerdict.Checked != 32 || gotVerdict.Missing != 0 {
		t.Fatalf("slice 2 verdict = %+v, want cached available verdict (checked=32, missing=0)", gotVerdict)
	}
	if idx.fetchCount() != 1 || eng.checkCount() != 1 {
		t.Fatalf("fetches=%d checks=%d, want 1, 1 (next slice must reuse background probe result)", idx.fetchCount(), eng.checkCount())
	}

	// 3. Bounds: an idle guest timeout ends the flight with a retry hint.
	mode.Store(2)
	_, err = kr.OpenKind(ctx, viewer, trkIdle.Key, nil)
	if !errors.Is(err, core.ErrPreparing) {
		t.Fatalf("idle guest timeout err = %v, want a retry hint", err)
	}

	// 4. Bounds: repeated timeouts without new work (only repeating a cached
	// check) stop continuing the flight after the stall cap.
	mode.Store(3)
	for i := range 2 {
		_, err = kr.OpenKind(ctx, viewer, trkStall.Key, nil)
		if !errors.Is(err, core.ErrPreparing) {
			t.Fatalf("stall slice %d err = %v, want core.ErrPreparing before cap", i+1, err)
		}
	}
	_, err = kr.OpenKind(ctx, viewer, trkStall.Key, nil)
	if !errors.Is(err, core.ErrPreparing) {
		t.Fatalf("stalled resolve after cap err = %v, want a retry hint", err)
	}

	// 5. The overall prepare budget ends the flight even when each slice
	// starts new source work.
	mode.Store(4)
	_, err = kr.OpenKind(ctx, viewer, trkBudget.Key, nil)
	if !errors.Is(err, core.ErrPreparing) {
		t.Fatalf("budget slice 1 err = %v, want core.ErrPreparing", err)
	}
	advanceClock(91 * time.Second)
	_, err = kr.OpenKind(ctx, viewer, trkBudget.Key, nil)
	if !errors.Is(err, core.ErrPreparing) {
		t.Fatalf("over-budget resolve err = %v, want a retry hint", err)
	}
}
