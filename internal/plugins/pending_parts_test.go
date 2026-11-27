package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/plugins/wasm"
)

func TestPendingBudgetBodyAndPartJobIdentity(t *testing.T) {
	var p PendingReply
	if err := json.Unmarshal(pendingBudgetBody(4200), &p); err != nil {
		t.Fatalf("unmarshal pendingBudgetBody(4200): %v", err)
	}
	if !p.Pending || p.RetryInMS != 4200 {
		t.Fatalf("pendingBudgetBody(4200) = %+v, want Pending=true RetryInMS=4200", p)
	}
	if got := string(pendingBudgetBody(0)); got != `{"pending":true}` {
		t.Fatalf("pendingBudgetBody(0) = %s, want {\"pending\":true}", got)
	}

	// Concurrent pending /resolve calls for different partIndex values of the
	// same item must launch distinct background jobs rather than colliding.
	env := newTestEnv(t)
	rec := env.installHello(t, "1.0.0")
	defer env.m.cancelActionJobs(rec.ID)

	var (
		mu       sync.Mutex
		donePart = map[int]bool{}
		jobParts []int
	)
	path := "/ether/v1/kinds/recording/resolve"
	env.fake.routes[path] = func(req Request) (Response, error) {
		var body struct {
			PartIndex *int `json:"partIndex"`
		}
		_ = json.Unmarshal(req.Body, &body)
		p := -1
		if body.PartIndex != nil {
			p = *body.PartIndex
		}
		mu.Lock()
		ready := donePart[p]
		mu.Unlock()
		if !ready {
			return Response{Status: 200, Body: json.RawMessage(`{"pending":true,"retryInMs":5000,"job":"/ether/job/resolve"}`)}, nil
		}
		return Response{Status: 200, Body: json.RawMessage(`{"sources":[{"id":"ok"}]}`)}, nil
	}
	env.fake.routes["/ether/job/resolve"] = func(req Request) (Response, error) {
		var body struct {
			PartIndex *int `json:"partIndex"`
		}
		_ = json.Unmarshal(req.Body, &body)
		p := -1
		if body.PartIndex != nil {
			p = *body.PartIndex
		}
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		donePart[p] = true
		jobParts = append(jobParts, p)
		mu.Unlock()
		return Response{Status: 200, Body: json.RawMessage(`{"done":true}`)}, nil
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, part := range []int{0, 1} {
		wg.Add(1)
		go func(part int) {
			defer wg.Done()
			raw, _ := json.Marshal(map[string]any{
				"item":      map[string]any{"ref": "plug:recording:set-1", "id": "set-1"},
				"partIndex": part,
			})
			resp, err := env.m.Call(context.Background(), rec.ID, Request{
				Path: path, Method: "POST", Body: raw,
			})
			if err != nil {
				errs <- err
				return
			}
			if resp.Status != 200 {
				errs <- errors.New("unexpected status")
			}
		}(part)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent part Call failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(jobParts) != 2 || !donePart[0] || !donePart[1] {
		t.Fatalf("expected separate jobs for parts 0 and 1, got jobParts=%v donePart=%v", jobParts, donePart)
	}
}

func TestHostStreamPartURL(t *testing.T) {
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
	var gotPart *int
	env.m.SetKindBackends(KindBackends{
		StreamPartURL: func(_ context.Context, pluginID string, viewerID int64, itemRef string, partIndex *int) (string, error) {
			if pluginID != rec.ID {
				t.Errorf("backend plugin scope: %s", pluginID)
			}
			gotViewer, gotItem, gotPart = viewerID, itemRef, partIndex
			return "/ether/v1/kind-media/ticket-part-2", nil
		},
	})

	viewerCtx := WithViewer(ctx, 9)
	part2 := 2
	badPart := -1
	if _, err := h.StreamPartURL(ctx, "pk:recording:trk-1", &part2); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("StreamPartURL without viewer: %v", err)
	}
	if _, err := h.StreamPartURL(viewerCtx, "pk:recording:trk-1", &badPart); err == nil {
		t.Fatal("negative partIndex accepted")
	}
	u, err := h.StreamPartURL(viewerCtx, "pk:recording:trk-1", &part2)
	if err != nil || u != "/ether/v1/kind-media/ticket-part-2" {
		t.Fatalf("StreamPartURL = %q, err = %v", u, err)
	}
	if gotViewer != 9 || gotItem != "pk:recording:trk-1" || gotPart == nil || *gotPart != 2 {
		t.Fatalf("backend got viewer=%d item=%q part=%v", gotViewer, gotItem, gotPart)
	}
	// Requesting a source without a StreamURLOptions backend fails closed (ErrUnavailable).
	if _, err := h.StreamURLWithOptions(viewerCtx, "pk:recording:trk-1", StreamURLOptions{Source: "pdf"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("StreamURLWithOptions without options backend = %v, want ErrUnavailable", err)
	}
}

func TestPendingSourceJobIdentityAndHostedStreamURLOptions(t *testing.T) {
	// 1. Concurrent pending /resolve calls for different source ids on the same item
	// launch distinct background prepare jobs rather than colliding.
	env := newTestEnv(t)
	rec := env.installHello(t, "1.0.0")
	defer env.m.cancelActionJobs(rec.ID)

	var (
		mu         sync.Mutex
		doneSource = map[string]bool{}
		jobSources []string
	)
	path := "/ether/v1/kinds/recording/resolve"
	env.fake.routes[path] = func(req Request) (Response, error) {
		var body struct {
			Source string `json:"source"`
		}
		_ = json.Unmarshal(req.Body, &body)
		mu.Lock()
		ready := doneSource[body.Source]
		mu.Unlock()
		if !ready {
			return Response{Status: 200, Body: json.RawMessage(`{"pending":true,"retryInMs":5000,"job":"/ether/job/resolve"}`)}, nil
		}
		return Response{Status: 200, Body: json.RawMessage(`{"sources":[{"id":"` + body.Source + `"}]}`)}, nil
	}
	env.fake.routes["/ether/job/resolve"] = func(req Request) (Response, error) {
		var body struct {
			Source string `json:"source"`
		}
		_ = json.Unmarshal(req.Body, &body)
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		doneSource[body.Source] = true
		jobSources = append(jobSources, body.Source)
		mu.Unlock()
		return Response{Status: 200, Body: json.RawMessage(`{"done":true}`)}, nil
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, src := range []string{"epub", "pdf"} {
		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			raw, _ := json.Marshal(map[string]any{
				"item":   map[string]any{"ref": "plug:recording:book-1", "id": "book-1"},
				"source": src,
			})
			resp, err := env.m.Call(context.Background(), rec.ID, Request{
				Path: path, Method: "POST", Body: raw,
			})
			if err != nil {
				errs <- err
				return
			}
			if resp.Status != 200 {
				errs <- errors.New("unexpected status")
			}
		}(src)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent source Call failed: %v", err)
	}
	mu.Lock()
	if len(jobSources) != 2 || !doneSource["epub"] || !doneSource["pdf"] {
		t.Fatalf("expected separate jobs for epub and pdf, got jobSources=%v doneSource=%v", jobSources, doneSource)
	}
	mu.Unlock()

	// 2. Hosted QuickJS ether.frontend.streamURL supports positional partIndex and { partIndex?, source? } options.
	ctx := context.Background()
	hostedEnv := newTestEnv(t)
	eng := wasm.NewEngine(nil)
	t.Cleanup(func() { _ = eng.Close() })
	hostedEnv.m.RegisterRuntime("hosted", HostedRuntimeFactory(eng))

	var gotCalls []StreamURLOptions
	hostedEnv.m.SetKindBackends(KindBackends{
		StreamURLOptions: func(_ context.Context, _ string, viewerID int64, itemRef string, opts StreamURLOptions) (string, error) {
			if viewerID != 42 || itemRef != "pk:recording:book-1" {
				t.Errorf("unexpected viewer=%d item=%q", viewerID, itemRef)
			}
			gotCalls = append(gotCalls, opts)
			return "/ether/v1/kind-media/t", nil
		},
	})

	man := contentManifest()
	man.Ether.Contributes.Setup = nil
	man.Ether.Capabilities = append(man.Ether.Capabilities, CapFrontendAdapter)
	man.Ether.Contributes.FrontendAdapters = []FrontendAdapterMeta{{
		Key: "feed", Label: "Feed", PathPrefix: "/feed", ProtocolHint: "opds", Auth: "none_lan", Libraries: []string{"recording"},
	}}
	rawMan, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	script := `function handle(req) {
  if (req.path === "/ether/health") return { status: 200, body: { status: "ok" } };
  if (req.path === "/ether/v1/frontend/feed/ok") {
    var a = ether.frontend.streamURL("pk:recording:book-1").url;
    var b = ether.frontend.streamURL("pk:recording:book-1", 1).url;
    var c = ether.frontend.streamURL("pk:recording:book-1", { source: "pdf" }).url;
    var d = ether.frontend.streamURL("pk:recording:book-1", { partIndex: 2, source: "epub", wait: { maxSeconds: 1200 } }).url;
    return { status: 200, body: { a: a, b: b, c: c, d: d } };
  }
  if (req.path === "/ether/v1/frontend/feed/bad-unknown") {
    ether.frontend.streamURL("pk:recording:book-1", { unknown: true });
    return { status: 200, body: {} };
  }
  if (req.path === "/ether/v1/frontend/feed/bad-blank") {
    ether.frontend.streamURL("pk:recording:book-1", { source: "   " });
    return { status: 200, body: {} };
  }
  return { status: 404, body: {} };
}`
	prev, err := hostedEnv.m.PreviewInstall(ctx, Source{Kind: "studio", Manifest: rawMan, Files: map[string][]byte{"dist/plugin.js": []byte(script)}})
	if err != nil {
		t.Fatal(err)
	}
	hrec, err := hostedEnv.m.Install(ctx, prev, InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := hostedEnv.m.Call(ctx, hrec.ID, Request{
		Method: "GET", Path: "/ether/v1/frontend/feed/ok", Headers: map[string]string{"X-Ether-Viewer": "42"},
	})
	if err != nil || resp.Status != 200 {
		t.Fatalf("hosted streamURL ok: resp=%+v err=%v", resp, err)
	}
	if len(gotCalls) != 4 {
		t.Fatalf("got %d calls, want 4: %+v", len(gotCalls), gotCalls)
	}
	if gotCalls[0].PartIndex != nil || gotCalls[0].Source != "" || gotCalls[0].Wait != nil {
		t.Fatalf("call 0 = %+v, want empty", gotCalls[0])
	}
	if gotCalls[1].PartIndex == nil || *gotCalls[1].PartIndex != 1 || gotCalls[1].Source != "" {
		t.Fatalf("call 1 = %+v, want partIndex=1", gotCalls[1])
	}
	if gotCalls[2].PartIndex != nil || gotCalls[2].Source != "pdf" {
		t.Fatalf("call 2 = %+v, want source=pdf", gotCalls[2])
	}
	if gotCalls[3].PartIndex == nil || *gotCalls[3].PartIndex != 2 || gotCalls[3].Source != "epub" || gotCalls[3].Wait == nil || gotCalls[3].Wait.MaxSeconds != 1200 {
		t.Fatalf("call 3 = %+v, want partIndex=2 source=epub wait=1200", gotCalls[3])
	}
	for _, badPath := range []string{"/ether/v1/frontend/feed/bad-unknown", "/ether/v1/frontend/feed/bad-blank"} {
		if _, err := hostedEnv.m.Call(ctx, hrec.ID, Request{
			Method: "GET", Path: badPath, Headers: map[string]string{"X-Ether-Viewer": "42"},
		}); err == nil {
			t.Fatalf("%s unexpectedly succeeded", badPath)
		}
	}
}
