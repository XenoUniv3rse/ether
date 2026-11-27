package etherplugin_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	etherplugin "github.com/braedonsaunders/ether/sdk/go"
	"github.com/braedonsaunders/ether/sdk/go/kit/feedparse"
	"github.com/braedonsaunders/ether/sdk/go/kit/hints"
)

type kindsFixture struct {
	Kind  json.RawMessage        `json:"kind"`
	Items []etherplugin.KindItem `json:"items"`
	Calls []kindsFixtureCall     `json:"calls"`
}

type kindsFixtureCall struct {
	Name               string          `json:"name"`
	Method             string          `json:"method"`
	Path               string          `json:"path"`
	Body               json.RawMessage `json:"body"`
	ExpectStatus       int             `json:"expectStatus"`
	Expect             json.RawMessage `json:"expect"`
	ExpectIDs          []string        `json:"expectIds"`
	ExpectComplete     *bool           `json:"expectComplete"`
	ExpectTopRef       string          `json:"expectTopRef"`
	ExpectBodyContains string          `json:"expectBodyContains"`
}

func loadKindsFixture(t *testing.T) kindsFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "conformance", "kinds", "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx kindsFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

// datasetProvider is the generic fixture dataset server every SDK
// re-implements; it proves the SDK routes carry the contract.
type datasetProvider struct{ items []etherplugin.KindItem }

func (d *datasetProvider) byID(id string) (etherplugin.KindItem, bool) {
	for _, it := range d.items {
		if it.ID == id {
			return it, true
		}
	}
	return etherplugin.KindItem{}, false
}

func pageOf(items []etherplugin.KindItem, cursor string, limit int) etherplugin.KindPage {
	if limit <= 0 {
		limit = 200
	}
	off, _ := strconv.Atoi(cursor)
	if off > len(items) {
		off = len(items)
	}
	end := off + limit
	page := etherplugin.KindPage{}
	if end >= len(items) {
		page.Items = append(page.Items, items[off:]...)
	} else {
		page.Items = append(page.Items, items[off:end]...)
		page.NextCursor = strconv.Itoa(end)
	}
	sort.SliceStable(page.Items, func(i, j int) bool { return page.Items[i].Index < page.Items[j].Index })
	return page
}

func (d *datasetProvider) Browse(_ *etherplugin.Context, req etherplugin.KindBrowseRequest) (etherplugin.KindPage, error) {
	var matched []etherplugin.KindItem
	for _, it := range d.items {
		if it.Level == req.Level && it.ParentID == req.ParentID {
			matched = append(matched, it)
		}
	}
	return pageOf(matched, req.Cursor, req.Limit), nil
}

func (d *datasetProvider) Search(_ *etherplugin.Context, req etherplugin.KindBrowseRequest) (etherplugin.KindPage, error) {
	var matched []etherplugin.KindItem
	for _, it := range d.items {
		if strings.Contains(strings.ToLower(it.Title), strings.ToLower(req.Query)) {
			matched = append(matched, it)
		}
	}
	return pageOf(matched, req.Cursor, req.Limit), nil
}

func (d *datasetProvider) Detail(_ *etherplugin.Context, _, id string) (etherplugin.KindItem, error) {
	if it, ok := d.byID(id); ok {
		return it, nil
	}
	return etherplugin.KindItem{}, etherplugin.ErrKindNotFound
}

func (d *datasetProvider) Changes(_ *etherplugin.Context, _ etherplugin.KindChangesRequest) (etherplugin.KindChangePage, error) {
	return etherplugin.KindChangePage{Upserts: d.items, Complete: true}, nil
}

type itemHints struct {
	Member  string `json:"member"`
	PageURL string `json:"pageUrl"`
}
type albumHints struct {
	ZipURL  string `json:"zipUrl"`
	ZipSize int64  `json:"zipSize"`
}

func (d *datasetProvider) Resolve(_ *etherplugin.Context, req etherplugin.KindResolveRequest) (etherplugin.KindResolveResponse, error) {
	var ih itemHints
	if err := hints.Unmarshal(req.Item.Hints, &ih); err != nil {
		return etherplugin.KindResolveResponse{}, err
	}
	var sources []etherplugin.PlayableSource
	if parent, ok := d.byID(req.Item.ParentID); ok {
		var ah albumHints
		_ = hints.Unmarshal(parent.Hints, &ah)
		if ah.ZipURL != "" && ih.Member != "" {
			sources = append(sources, etherplugin.PlayableSource{
				ID: "zip", Pattern: "container", MIME: "audio/flac",
				Container: &etherplugin.ContainerRef{
					Format:  "zip",
					Volumes: []etherplugin.VolumeRef{{URL: ah.ZipURL, Size: ah.ZipSize}},
					Member:  ih.Member,
				},
			})
		}
	}
	if ih.PageURL != "" {
		sources = append(sources, etherplugin.PlayableSource{ID: "direct", Pattern: "range", MIME: "audio/flac", URL: ih.PageURL})
	}
	return etherplugin.KindResolveResponse{Sources: sources}, nil
}

func (d *datasetProvider) Match(_ *etherplugin.Context, req etherplugin.KindMatchRequest) (etherplugin.KindMatchResponse, error) {
	var scores []etherplugin.KindMatchScore
	for _, c := range req.Candidates {
		score, reasons := 0.1, []string{"Title does not match"}
		if strings.Contains(strings.ToLower(c.Title), strings.ToLower(req.Item.Title)) {
			score, reasons = 0.95, []string{"Title matches"}
		}
		scores = append(scores, etherplugin.KindMatchScore{CandidateRef: c.CandidateRef(), Score: score, Reasons: reasons})
	}
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].Score > scores[j].Score })
	return etherplugin.KindMatchResponse{Matches: scores}, nil
}

type datasetSetup struct{}

func (datasetSetup) TestSetup(_ *etherplugin.Context, req etherplugin.SetupTestRequest) (etherplugin.SetupTestResult, error) {
	if fmt.Sprint(req.Fields["apiKey"]) != "" {
		return etherplugin.SetupTestResult{OK: true, Detail: "Connected"}, nil
	}
	return etherplugin.SetupTestResult{OK: false, Detail: "Add your API key first", Fields: map[string]string{"apiKey": "Required"}}, nil
}

type datasetFrontend struct{}

func (datasetFrontend) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><feed><title>Field Recordings</title></feed>`))
	})
}

func jsonEqual(t *testing.T, want json.RawMessage, got []byte) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("fixture expect: %v", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("response body %s: %v", got, err)
	}
	if !reflect.DeepEqual(w, g) {
		wb, _ := json.Marshal(w)
		t.Fatalf("body mismatch\nwant %s\ngot  %s", wb, got)
	}
}

func TestKindsConformanceFixture(t *testing.T) {
	fx := loadKindsFixture(t)
	var decl etherplugin.MediaKindMeta
	if err := json.Unmarshal(fx.Kind, &decl); err != nil {
		t.Fatal(err)
	}
	plugin := etherplugin.New(etherplugin.Definition{
		Manifest: etherplugin.Manifest{
			ID:      "community.example.recordings",
			Version: "1.0.0",
			Name:    "Field Recordings",
			Ether: &etherplugin.EtherManifestBlock{
				Protocol: 1, Kind: etherplugin.KindContent, Summary: "Fixture provider",
				Runtime:      etherplugin.RuntimeDecl{Kind: "remote"},
				Capabilities: []etherplugin.Capability{etherplugin.CapCatalogProvider, etherplugin.CapKindResolver, etherplugin.CapMatcher, etherplugin.CapFrontendAdapter},
				Contributes:  &etherplugin.Contributes{MediaKinds: []etherplugin.MediaKindMeta{decl}},
			},
		},
		Host: etherplugin.NewMockHost(),
	})
	prov := &datasetProvider{items: fx.Items}
	plugin.OnCatalog("recordings", prov)
	plugin.OnResolve("recordings", prov)
	plugin.OnMatch("recordings", prov)
	plugin.OnSetupTest(datasetSetup{})
	plugin.OnFrontend("feed", datasetFrontend{})

	for _, call := range fx.Calls {
		t.Run(call.Name, func(t *testing.T) {
			resp := plugin.Dispatch(context.Background(), etherplugin.RawRequest{Method: call.Method, Path: call.Path, Body: call.Body})
			if resp.Status != call.ExpectStatus {
				t.Fatalf("status %d, want %d; body %s", resp.Status, call.ExpectStatus, resp.Body)
			}
			switch {
			case call.Expect != nil:
				jsonEqual(t, call.Expect, resp.Body)
			case call.ExpectIDs != nil:
				var page etherplugin.KindChangePage
				if err := json.Unmarshal(resp.Body, &page); err != nil {
					t.Fatal(err)
				}
				var ids []string
				for _, it := range page.Upserts {
					ids = append(ids, it.ID)
				}
				if !reflect.DeepEqual(ids, call.ExpectIDs) {
					t.Fatalf("ids %v, want %v", ids, call.ExpectIDs)
				}
				if call.ExpectComplete != nil && page.Complete != *call.ExpectComplete {
					t.Fatalf("complete %v, want %v", page.Complete, *call.ExpectComplete)
				}
			case call.ExpectTopRef != "":
				var res etherplugin.KindMatchResponse
				if err := json.Unmarshal(resp.Body, &res); err != nil {
					t.Fatal(err)
				}
				if len(res.Matches) == 0 || res.Matches[0].CandidateRef != call.ExpectTopRef {
					t.Fatalf("top match %+v, want ref %s", res.Matches, call.ExpectTopRef)
				}
			case call.ExpectBodyContains != "":
				if !strings.Contains(string(resp.BinaryBody)+string(resp.Body), call.ExpectBodyContains) {
					t.Fatalf("body %s%s lacks %q", resp.BinaryBody, resp.Body, call.ExpectBodyContains)
				}
			}
		})
	}
}

// recordingFrontend echoes the request line it received so the test can
// prove every method and the query survive the dispatch.
type recordingFrontend struct{}

func (recordingFrontend) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Method", r.Method)
		w.Header().Set("X-Seen-Query", r.URL.RawQuery)
		_, _ = w.Write([]byte(r.URL.Path))
	})
}

func TestFrontendDispatchMethodsAndQuery(t *testing.T) {
	plugin := etherplugin.New(etherplugin.Definition{
		Manifest: etherplugin.Manifest{ID: "community.example.dav", Version: "1.0.0", Name: "DAV"},
		Host:     etherplugin.NewMockHost(),
	})
	plugin.OnFrontend("dav", recordingFrontend{})

	// A protocol method the kind routes would reject reaches the adapter.
	propfind := plugin.Dispatch(context.Background(), etherplugin.RawRequest{
		Method: "PROPFIND", Path: "/ether/v1/frontend/dav/library/",
	})
	if propfind.Status != http.StatusOK {
		t.Fatalf("PROPFIND status %d; body %s", propfind.Status, propfind.Body)
	}
	if got := propfind.Headers["X-Seen-Method"]; got != "PROPFIND" {
		t.Fatalf("adapter saw method %q, want PROPFIND", got)
	}
	if got := string(propfind.BinaryBody); got != "/library/" {
		t.Fatalf("adapter saw path %q, want /library/", got)
	}

	// The original query reaches the adapter.
	ping := plugin.Dispatch(context.Background(), etherplugin.RawRequest{
		Method: "GET", Path: "/ether/v1/frontend/dav/rest/ping.view?u=a&t=b",
	})
	if ping.Status != http.StatusOK {
		t.Fatalf("GET status %d; body %s", ping.Status, ping.Body)
	}
	q := ping.Headers["X-Seen-Query"]
	vals, _ := url.ParseQuery(q)
	if vals.Get("u") != "a" || vals.Get("t") != "b" {
		t.Fatalf("adapter saw query %q, want u=a&t=b", q)
	}
	if got := string(ping.BinaryBody); got != "/rest/ping.view" {
		t.Fatalf("adapter saw path %q, want /rest/ping.view", got)
	}

	// A HEAD reply carries headers but no body.
	head := plugin.Dispatch(context.Background(), etherplugin.RawRequest{
		Method: "HEAD", Path: "/ether/v1/frontend/dav/library/",
	})
	if head.Status != http.StatusOK {
		t.Fatalf("HEAD status %d", head.Status)
	}
	if len(head.BinaryBody) != 0 {
		t.Fatalf("HEAD body %q, want empty", head.BinaryBody)
	}
	if got := head.Headers["X-Seen-Method"]; got != "HEAD" {
		t.Fatalf("adapter saw method %q, want HEAD", got)
	}

	// The kind family still refuses methods other than GET/POST.
	put := plugin.Dispatch(context.Background(), etherplugin.RawRequest{
		Method: "PUT", Path: "/ether/v1/kinds/recordings/browse",
	})
	if put.Status != http.StatusMethodNotAllowed {
		t.Fatalf("kinds PUT status %d, want 405", put.Status)
	}
}

func TestKindHostMockAndKit(t *testing.T) {
	host := etherplugin.NewMockHost()
	ctx := context.Background()
	accepted, err := host.KindUpsert(ctx, "recordings", []etherplugin.KindItem{{ID: "a", Level: "track", Title: "A"}})
	if err != nil || accepted != 1 {
		t.Fatalf("upsert: %d %v", accepted, err)
	}
	// StreamURL mints a scoped URL for one of this plugin's own items and
	// takes the viewer from the request headers.
	pctx := &etherplugin.Context{
		Context: ctx,
		Headers: map[string]string{"X-Ether-Viewer": "7"},
		Ether:   host,
	}
	u, err := pctx.StreamURL("pk:recordings:a")
	if err != nil || !strings.HasPrefix(u, "/ether/v1/kind-media/") {
		t.Fatalf("stream url: %q %v", u, err)
	}
	if host.LastStreamURLViewer != 7 || host.LastStreamURLItem != "pk:recordings:a" {
		t.Fatalf("stream url scope: viewer=%d item=%q", host.LastStreamURLViewer, host.LastStreamURLItem)
	}
	// A request without a viewer cannot mint a URL.
	if _, err := (&etherplugin.Context{Context: ctx, Ether: host}).StreamURL("pk:recordings:a"); err == nil {
		t.Fatal("stream url without a viewer was accepted")
	}
	page, err := host.KindItems(ctx, "recordings", "", 10)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("items: %+v %v", page.Items, err)
	}
	ref, err := host.CachePut(ctx, "pk-x-recordings", "a.flac", []byte("bytes"))
	if err != nil || !strings.HasPrefix(ref, "blob:") {
		t.Fatalf("cache put: %q %v", ref, err)
	}
	if got, err := host.CacheOpen(ctx, ref); err != nil || string(got) != "bytes" {
		t.Fatalf("cache open: %q %v", got, err)
	}
	if err := host.KindDelete(ctx, "recordings", []string{"a"}); err != nil {
		t.Fatal(err)
	}
	feed, err := feedparse.Parse([]byte(`<rss><channel><title>T</title><item><title>E</title><guid>g1</guid><enclosure url="https://x/e.mp3"/></item></channel></rss>`))
	if err != nil || len(feed.Entries) != 1 || feed.Entries[0].Audio == "" {
		t.Fatalf("feed: %+v %v", feed, err)
	}
	items, err := feed.Items("show", "track", "https://x/feed")
	if err != nil || len(items) != 2 || !items[1].Playable {
		t.Fatalf("feed items: %+v %v", items, err)
	}
}

func TestRemoteBlobHostArchiveReadAndLimits(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/ether/plugin-host/limits":
			_ = json.NewEncoder(w).Encode(etherplugin.RuntimeLimits{
				MemoryBytes: 1 << 30,
				ReadBytes:   256 << 20,
			})
		case "/ether/plugin-host/data/archive.read":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"index": 0, "member": 3, "size": 5, "bodyBase64": base64.StdEncoding.EncodeToString([]byte("alpha"))},
				{"index": 1, "member": 7, "size": 4, "bodyBase64": base64.StdEncoding.EncodeToString([]byte{0, 255, 1, 2})},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := etherplugin.NewRemoteHostClient(etherplugin.RemoteHostOptions{
		BaseURL:    srv.URL,
		Credential: "cred-1",
	})
	var blobHost etherplugin.BlobHost = client
	lim, err := blobHost.RuntimeLimits(ctx)
	if err != nil || lim.ReadBytes != 256<<20 {
		t.Fatalf("RuntimeLimits = %+v, err = %v", lim, err)
	}
	out, err := blobHost.ArchiveRead(ctx, "blob:"+strings.Repeat("a", 64), []etherplugin.ArchiveReadItem{
		{Member: 3, Offset: 0, Length: 5},
		{Member: 7, Offset: 10, Length: 4},
	})
	if err != nil || len(out) != 2 {
		t.Fatalf("ArchiveRead = %+v, err = %v", out, err)
	}
	if !bytes.Equal(out[0].Body, []byte("alpha")) || !bytes.Equal(out[1].Body, []byte{0, 255, 1, 2}) {
		t.Fatalf("unexpected decoded bodies: %+v", out)
	}
	if _, err := blobHost.ArchiveRead(ctx, "blob:x", nil); err == nil {
		t.Fatal("empty reads accepted")
	}
}

func TestKindVisibleAndBrowseSDK(t *testing.T) {
	ctx := context.Background()
	host := etherplugin.NewMockHost()
	if _, err := host.KindUpsert(ctx, "comic", []etherplugin.KindItem{
		{ID: "a", Level: "series", Title: "Alpha"},
		{ID: "b", Level: "series", Title: "Beta"},
		{ID: "c", Level: "series", Title: "Gamma"},
	}); err != nil {
		t.Fatal(err)
	}
	host.KindHiddenIDs["comic"] = map[string]bool{"b": true}

	// Missing viewer token must fail on both Context and MockHost.
	noViewerCtx := &etherplugin.Context{Context: ctx, Ether: host}
	if _, err := noViewerCtx.KindVisible("comic", []string{"a", "b"}); err == nil {
		t.Fatal("KindVisible without viewer token succeeded")
	}
	if _, err := noViewerCtx.KindBrowse("comic", etherplugin.KindBrowseQuery{}); err == nil {
		t.Fatal("KindBrowse without viewer token succeeded")
	}

	// Authenticated frontend adapter context succeeds and applies hidden rules.
	adapterCtx := &etherplugin.Context{
		Context: ctx,
		Headers: map[string]string{etherplugin.HeaderViewerToken: "tok-1"},
		Ether:   host,
	}
	gotIDs, err := adapterCtx.KindVisible("comic", []string{"c", "b", "missing", "a", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotIDs, []string{"c", "a"}) {
		t.Fatalf("KindVisible = %v, want [c a]", gotIDs)
	}
	page, err := adapterCtx.KindBrowse("comic", etherplugin.KindBrowseQuery{Query: "a", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].ID != "a" || page.Items[1].ID != "c" {
		t.Fatalf("KindBrowse = %+v", page)
	}

	// Batch larger than 500 is rejected client-side.
	tooMany := make([]string, etherplugin.MaxKindVisibleIDs+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("id-%d", i)
	}
	if _, err := adapterCtx.KindVisible("comic", tooMany); err == nil {
		t.Fatal("oversized KindVisible succeeded")
	}

	// FrontendAdapter dispatch attaches FrontendContext + viewer token onto r.Context().
	p := etherplugin.New(etherplugin.Definition{
		Manifest: etherplugin.Manifest{
			ID:      "org.example.comics",
			Name:    "Comics",
			Version: "1.0.0",
			Ether:   &etherplugin.EtherManifestBlock{Protocol: 1, Kind: "content"},
		},
		Host: host,
	})
	p.OnFrontend("opds", frontendFunc(func(w http.ResponseWriter, r *http.Request) {
		pctx, ok := etherplugin.FrontendContext(r.Context())
		if !ok {
			http.Error(w, "no frontend context", http.StatusInternalServerError)
			return
		}
		ids, err := pctx.KindVisible("comic", []string{"a", "b"})
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(ids)
	}))
	rawResp := p.Dispatch(ctx, etherplugin.RawRequest{
		Method:  http.MethodGet,
		Path:    "/ether/v1/frontend/opds/feed",
		Headers: map[string]string{etherplugin.HeaderViewerToken: "tok-live"},
	})
	if rawResp.Status != http.StatusOK || strings.TrimSpace(string(rawResp.BinaryBody)) != `["a"]` {
		t.Fatalf("frontend dispatch: status=%d body=%q", rawResp.Status, string(rawResp.BinaryBody))
	}

	// RemoteHostClient sends X-Ether-Viewer-Token header and never viewerId in the JSON body.
	var seenHeader string
	var seenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeader = r.Header.Get(etherplugin.HeaderViewerToken)
		seenBody = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/ether/plugin-host/kinds/comic/visible":
			_ = json.NewEncoder(w).Encode(map[string]any{"ids": []string{"a"}})
		case "/ether/plugin-host/kinds/comic/browse":
			_ = json.NewEncoder(w).Encode(etherplugin.KindPage{Items: []etherplugin.KindItem{{ID: "a", Level: "series", Title: "Alpha"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	remoteClient := etherplugin.NewRemoteHostClient(etherplugin.RemoteHostOptions{
		BaseURL:    srv.URL,
		Credential: "cred-1",
	})
	if _, err := remoteClient.KindVisible(ctx, "comic", []string{"a"}); err == nil {
		t.Fatal("remote KindVisible without token succeeded")
	}
	rCtx := etherplugin.WithViewerToken(ctx, "tok-remote")
	rIDs, err := remoteClient.KindVisible(rCtx, "comic", []string{"a", "b"})
	if err != nil || !reflect.DeepEqual(rIDs, []string{"a"}) {
		t.Fatalf("remote KindVisible = %v, err = %v", rIDs, err)
	}
	if seenHeader != "tok-remote" {
		t.Fatalf("remote viewer token header = %q, want tok-remote", seenHeader)
	}
	if _, hasViewerID := seenBody["viewerId"]; hasViewerID {
		t.Fatalf("remote body must not include viewerId: %+v", seenBody)
	}
	rPage, err := remoteClient.KindBrowse(rCtx, "comic", etherplugin.KindBrowseQuery{Query: "alp", Desc: true, Limit: 5})
	if err != nil || len(rPage.Items) != 1 || rPage.Items[0].ID != "a" {
		t.Fatalf("remote KindBrowse = %+v, err = %v", rPage, err)
	}
	if seenHeader != "tok-remote" || seenBody["query"] != "alp" || seenBody["desc"] != true {
		t.Fatalf("remote browse request mismatch: header=%q body=%+v", seenHeader, seenBody)
	}
	if _, hasViewerID := seenBody["viewerId"]; hasViewerID {
		t.Fatalf("remote browse body must not include viewerId: %+v", seenBody)
	}
}

type frontendFunc http.HandlerFunc

func (f frontendFunc) Handler() http.Handler { return http.HandlerFunc(f) }

type stubLiveProvider struct {
	lastReq etherplugin.KindLiveRequest
	lastID  string
}

func (s *stubLiveProvider) Live(_ *etherplugin.Context, req etherplugin.KindLiveRequest) (etherplugin.KindLivePage, error) {
	s.lastReq = req
	viewers := 420
	return etherplugin.KindLivePage{
		Items: []etherplugin.KindLiveItem{
			{ID: "ch-1", Title: "Finals", Subtitle: "Main Stage", Badge: "LIVE", Viewers: &viewers, Tags: []string{"Esports"}, Thumb: "https://cdn.example/ch1.jpg"},
		},
		Filters:    []etherplugin.KindLiveFilter{{Key: "featured", Label: "Featured"}},
		NextCursor: "cur-2",
	}, nil
}

func (s *stubLiveProvider) ResolveLive(_ *etherplugin.Context, req etherplugin.KindLiveResolveRequest) (etherplugin.KindLiveResolveResponse, error) {
	s.lastID = req.ID
	return etherplugin.KindLiveResolveResponse{
		URL:     "https://stream.example/live/ch-1/master.m3u8?token=once",
		Headers: map[string]string{"Referer": "https://stream.example/"},
		MIME:    "application/vnd.apple.mpegurl",
		Title:   "Finals",
	}, nil
}

func TestKindLiveDispatch(t *testing.T) {
	p := etherplugin.New(etherplugin.Definition{Manifest: etherplugin.Manifest{ID: "org.example.games", Name: "Games", Version: "1.0.0"}})
	lp := &stubLiveProvider{}
	p.OnLive("esports", lp)

	resp := p.Dispatch(context.Background(), etherplugin.RawRequest{
		Method: http.MethodPost,
		Path:   "/ether/v1/kinds/esports/live",
		Body:   []byte(`{"tab":"live","q":"finals","filter":"featured","limit":10}`),
	})
	if resp.Status != http.StatusOK {
		t.Fatalf("live status=%d body=%v", resp.Status, resp.Body)
	}
	if lp.lastReq.Kind != "esports" || lp.lastReq.Tab != "live" || lp.lastReq.Q != "finals" || lp.lastReq.Filter != "featured" || lp.lastReq.Limit != 10 {
		t.Fatalf("unexpected live request: %+v", lp.lastReq)
	}
	raw, _ := json.Marshal(resp.Body)
	var page etherplugin.KindLivePage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "ch-1" || page.NextCursor != "cur-2" || len(page.Filters) != 1 {
		t.Fatalf("unexpected page: %+v", page)
	}

	resResp := p.Dispatch(context.Background(), etherplugin.RawRequest{
		Method: http.MethodPost,
		Path:   "/ether/v1/kinds/esports/live/resolve",
		Body:   []byte(`{"id":"ch-1"}`),
	})
	if resResp.Status != http.StatusOK || lp.lastID != "ch-1" {
		t.Fatalf("live resolve status=%d lastID=%q body=%v", resResp.Status, lp.lastID, resResp.Body)
	}
	rawRes, _ := json.Marshal(resResp.Body)
	var resolved etherplugin.KindLiveResolveResponse
	if err := json.Unmarshal(rawRes, &resolved); err != nil {
		t.Fatalf("decode resolve: %v", err)
	}
	if resolved.URL != "https://stream.example/live/ch-1/master.m3u8?token=once" || resolved.Headers["Referer"] != "https://stream.example/" {
		t.Fatalf("unexpected resolved: %+v", resolved)
	}
}

func TestResolveSourcesWithResult(t *testing.T) {
	var seen etherplugin.SourceQuery
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ether/plugin-host/sources/resolve" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Errorf("decode query: %v", err)
		}
		_ = json.NewEncoder(w).Encode(etherplugin.SourceResolveResult{
			Candidates: []etherplugin.SourceCandidate{{Title: "Fast.Release", IndexerID: "fast", Indexer: "Fast"}},
			Indexers: []etherplugin.IndexerSearchStatus{
				{IndexerID: "fast", Indexer: "Fast", Status: etherplugin.IndexerStatusAnswered, Results: 1, MS: 22},
				{IndexerID: "slow", Indexer: "Slow", Status: etherplugin.IndexerStatusTimedOut, MS: 250},
			},
		})
	}))
	defer srv.Close()

	settle := 250
	q := etherplugin.SourceQuery{
		Kind:         "indexer_search",
		Query:        "Arrival",
		FirstResults: true,
		SettleMS:     &settle,
		MinResults:   2,
		SkipIndexers: []string{"broken"},
		TimeoutMS:    4000,
	}

	client := etherplugin.NewRemoteHostClient(etherplugin.RemoteHostOptions{
		BaseURL:    srv.URL,
		Credential: "cred",
	})
	c := &etherplugin.Context{Context: context.Background(), Ether: client}
	res, err := c.ResolveSourcesWithResult(q)
	if err != nil {
		t.Fatalf("ResolveSourcesWithResult: %v", err)
	}
	if !seen.FirstResults || seen.SettleMS == nil || *seen.SettleMS != 250 || seen.MinResults != 2 || seen.TimeoutMS != 4000 || len(seen.SkipIndexers) != 1 {
		t.Fatalf("forwarded query = %+v", seen)
	}
	if len(res.Candidates) != 1 || len(res.Indexers) != 2 || res.Indexers[1].Status != etherplugin.IndexerStatusTimedOut {
		t.Fatalf("unexpected result: %+v", res)
	}
	// Existing ResolveSources remains backward compatible.
	cands, err := client.ResolveSources(context.Background(), q)
	if err != nil || len(cands) != 1 || cands[0].Title != "Fast.Release" {
		t.Fatalf("ResolveSources = (%+v, %v)", cands, err)
	}

	// MockHost supports SourceIndexers and ResolveSourcesWithResult.
	mock := etherplugin.NewMockHost()
	mock.Sources = []etherplugin.SourceCandidate{{Title: "Mock.Release"}}
	mock.SourceIndexers = []etherplugin.IndexerSearchStatus{{IndexerID: "m", Indexer: "Mock", Status: etherplugin.IndexerStatusAnswered, Results: 1, MS: 5}}
	mc := &etherplugin.Context{Context: context.Background(), Ether: mock}
	mres, err := mc.ResolveSourcesWithResult(q)
	if err != nil || len(mres.Candidates) != 1 || len(mres.Indexers) != 1 || mres.Indexers[0].IndexerID != "m" {
		t.Fatalf("mock ResolveSourcesWithResult = (%+v, %v)", mres, err)
	}

	// Streaming, pending, and kind:"indexers" round-trip through SourceResolveResult.
	doneFalse := false
	med, last := int64(180), int64(140)
	streamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sq etherplugin.SourceQuery
		_ = json.NewDecoder(r.Body).Decode(&sq)
		switch sq.Kind {
		case "indexer_search":
			_ = json.NewEncoder(w).Encode(etherplugin.SourceResolveResult{
				Candidates: []etherplugin.SourceCandidate{},
				SearchID:   "srch_abc",
				Done:       &doneFalse,
				Pending:    []etherplugin.IndexerRef{{IndexerID: "slow", Indexer: "Slow"}},
				Indexers:   []etherplugin.IndexerSearchStatus{{IndexerID: "slow", Indexer: "Slow", Status: etherplugin.IndexerStatusPending, MS: 0}},
			})
		case "debrid":
			enabled := true
			_ = json.NewEncoder(w).Encode(etherplugin.SourceResolveResult{Enabled: &enabled, CachedOnly: &doneFalse})
		case "indexers":
			_ = json.NewEncoder(w).Encode(etherplugin.SourceResolveResult{
				Candidates: []etherplugin.SourceCandidate{},
				Indexers:   []etherplugin.IndexerSearchStatus{{IndexerID: "idx1", Indexer: "One", Kind: "newznab", MedianMS: &med, LastMS: &last}},
			})
		}
	}))
	defer streamSrv.Close()
	sc := &etherplugin.Context{Context: context.Background(), Ether: etherplugin.NewRemoteHostClient(etherplugin.RemoteHostOptions{BaseURL: streamSrv.URL, Credential: "cred"})}
	sres, err := sc.ResolveSourcesWithResult(etherplugin.SourceQuery{Kind: "indexer_search", Query: "Dune", Stream: true})
	if err != nil || sres.SearchID != "srch_abc" || sres.Done == nil || *sres.Done || len(sres.Pending) != 1 || sres.Indexers[0].Status != etherplugin.IndexerStatusPending {
		t.Fatalf("streaming start = (%+v, %v)", sres, err)
	}
	lres, err := sc.ResolveSourcesWithResult(etherplugin.SourceQuery{Kind: "indexers"})
	if err != nil || len(lres.Indexers) != 1 || lres.Indexers[0].Kind != "newznab" || lres.Indexers[0].MedianMS == nil || *lres.Indexers[0].MedianMS != 180 {
		t.Fatalf("indexers listing = (%+v, %v)", lres, err)
	}
	policy, err := sc.ResolveSourcesWithResult(etherplugin.SourceQuery{Kind: "debrid"})
	if err != nil || policy.Enabled == nil || !*policy.Enabled || policy.CachedOnly == nil || *policy.CachedOnly {
		t.Fatalf("debrid policy did not round-trip: %v", err)
	}
}
