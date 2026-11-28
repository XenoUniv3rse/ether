package plugins

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
)

type memHost struct {
	kv     map[string][]byte
	denied bool
}

func (h *memHost) KVGet(_ context.Context, k string) ([]byte, bool, error) {
	v, ok := h.kv[k]
	return v, ok, nil
}
func (h *memHost) KVPut(_ context.Context, k string, v []byte) error {
	if h.denied {
		return ErrPermissionDenied
	}
	h.kv[k] = v
	return nil
}
func (h *memHost) KVDelete(_ context.Context, k string) error { delete(h.kv, k); return nil }
func (h *memHost) KVList(context.Context, string) ([]string, error) {
	return nil, nil
}
func (h *memHost) KVUsage(context.Context) (KVUsage, error) {
	var used int64
	for _, v := range h.kv {
		used += int64(len(v))
	}
	keys := int64(len(h.kv))
	return KVUsage{Bytes: used + keys*KVKeyChargeBytes, Keys: keys, QuotaBytes: KVQuotaBytes, QuotaKeys: KVQuotaKeys}, nil
}
func (h *memHost) SecretGet(context.Context, string) (string, error) { return "", ErrPermissionDenied }
func (h *memHost) SecretSet(context.Context, string, string, int64) error {
	return ErrPermissionDenied
}
func (h *memHost) SecretDelete(context.Context, string) error { return ErrPermissionDenied }
func (h *memHost) HTTPFetch(_ context.Context, req FetchRequest) (FetchResponse, error) {
	if req.URL == "https://dl.example.test/file.zip" {
		return FetchResponse{Status: 200, Body: []byte{0x50, 0x4b, 0x03, 0x04, 0xff, 0xfe, 0x00, 0x80}}, nil
	}
	return FetchResponse{}, ErrPermissionDenied
}
func (h *memHost) CatalogSearch(context.Context, string, int, int) (json.RawMessage, error) {
	return json.RawMessage(`{"titles":[]}`), nil
}
func (h *memHost) CatalogGet(context.Context, string) (json.RawMessage, error) { return nil, nil }
func (h *memHost) ViewerGranted(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`[]`), nil
}
func (h *memHost) ViewerHistory(context.Context, int64, int, int) (json.RawMessage, error) {
	return nil, ErrPermissionDenied
}
func (h *memHost) SetWatchState(context.Context, int64, string, bool) error {
	return ErrPermissionDenied
}
func (h *memHost) CollectionsUpsert(context.Context, json.RawMessage) error {
	return ErrPermissionDenied
}
func (h *memHost) FoldersRoots(context.Context) ([]string, error) { return []string{}, nil }

func (h *memHost) FoldersList(context.Context, string, string, ...FolderListOptions) (FolderPage, error) {
	return FolderPage{}, ErrPermissionDenied
}
func (h *memHost) FoldersStat(context.Context, string, string) (FolderEntry, error) {
	return FolderEntry{}, ErrPermissionDenied
}
func (h *memHost) PublishEvent(context.Context, string, json.RawMessage, events.Correlation) error {
	return ErrPermissionDenied
}
func (h *memHost) ScreensInvalidate(context.Context) (int, error) { return 0, ErrPermissionDenied }
func (h *memHost) ScreensPutRecords(context.Context, json.RawMessage) (int, error) {
	return 0, ErrPermissionDenied
}
func (h *memHost) AIComplete(context.Context, AICompleteRequest) (AICompleteResponse, error) {
	return AICompleteResponse{}, ErrNotConfigured
}
func (h *memHost) MediaProbe(context.Context, core.PlayableSource) (core.MediaProbeResult, error) {
	return core.MediaProbeResult{}, ErrPermissionDenied
}
func (h *memHost) NotifyOwner(context.Context, string) error { return nil }
func (h *memHost) Settings(context.Context) (map[string]any, error) {
	return map[string]any{"greeting": "Hi"}, nil
}
func (h *memHost) Log(context.Context, string, string, map[string]any) error {
	return nil
}

const hostedTS = `
type Req = { path: string; query?: Record<string, string[]> };
function handle(req: Req): { status: number; body: string } {
  const prev = ether.kv.get("count");
  const next = String(Number(prev ?? "0") + 1);
  ether.kv.put("count", next);
  return { status: 200, body: JSON.stringify({ path: req.path, count: next }) };
}
function rank_key(c: { name: string }): number { return c.name.includes("DUB") ? 10 : 0; }
(globalThis as any).handle = handle;
(globalThis as any).rank_key = rank_key;
`

func TestHostedRuntimeFactoryRunsTypeScriptThroughHost(t *testing.T) {
	eng := wasm.NewEngine(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = eng.Close() })
	host := &memHost{kv: map[string][]byte{}}
	inst := Installed{
		ID: "dev.example.hello",
		Manifest: Manifest{
			Version: "1.0.0",
			Ether:   &EtherBlock{Runtime: RuntimeDecl{Kind: "hosted", Entry: "plugin.ts"}},
		},
		Files: func(p string) ([]byte, bool) {
			if p == "plugin.ts" {
				return []byte(hostedTS), true
			}
			return nil, false
		},
		Host: host,
	}
	rt, err := HostedRuntimeFactory(eng)(inst)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	ctx := context.Background()
	for want := 1; want <= 2; want++ {
		resp, err := rt.Call(ctx, Request{Method: "GET", Path: "/ether/health"})
		if err != nil {
			t.Fatal(err)
		}
		var body string
		if err := json.Unmarshal(resp.Body, &body); err != nil {
			t.Fatalf("body %s: %v", resp.Body, err)
		}
		if resp.Status != 200 || !strings.Contains(body, `"count":"`+string(rune('0'+want))+`"`) {
			t.Fatalf("call %d: status %d body %s", want, resp.Status, body)
		}
	}
	hot, ok := rt.(HotRuntime)
	if !ok || !hot.HasRankKey() {
		t.Fatal("hosted runtime should expose rank_key")
	}
	keys, out, err := hot.RankBatch(ctx, []CandidateFacts{{ReleaseID: "a", Name: "Movie.DUB.1080p"}, {ReleaseID: "b", Name: "Movie.1080p"}}, nil)
	if err != nil || len(keys) != 2 || keys[0] != 10 || keys[1] != 0 {
		t.Fatalf("rank keys = %v, %v", keys, err)
	}
	if out.Status != "ok" || out.Timeout() {
		t.Fatalf("rank outcome = %+v", out)
	}

	host.denied = true
	if _, err := rt.Call(ctx, Request{Method: "GET", Path: "/"}); err == nil {
		t.Fatal("a revoked grant must fail the call")
	} else {
		var we *wasm.Error
		if !errors.As(err, &we) || we.Status != wasm.StatusForbidden {
			t.Fatalf("revoked grant error = %v", err)
		}
	}
}

func TestHostedRuntimeFactoryReportsCompileErrors(t *testing.T) {
	eng := wasm.NewEngine(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = eng.Close() })
	inst := Installed{
		ID:       "dev.example.broken",
		Manifest: Manifest{Version: "1.0.0", Ether: &EtherBlock{Runtime: RuntimeDecl{Kind: "hosted", Entry: "plugin.ts"}}},
		Files:    func(string) ([]byte, bool) { return []byte("function (("), true },
		Host:     &memHost{kv: map[string][]byte{}},
	}
	if _, err := HostedRuntimeFactory(eng)(inst); err == nil || !strings.Contains(err.Error(), "compile") {
		t.Fatalf("compile error = %v", err)
	}
}

func TestHotLimitsLowerButNeverRaiseBudgets(t *testing.T) {
	m := Manifest{Ether: &EtherBlock{Contributes: Contributes{RankingSteps: []RankingStepMeta{
		{Key: "a", Budget: json.RawMessage(`{"perCandidateMicros":500,"perBatchMillis":20}`)},
		{Key: "b", Budget: json.RawMessage(`{"perCandidateMicros":1500}`)},
	}}}}
	got := EffectiveBudgets(HotLimits(m))
	if got.PerCandidate != 500*time.Microsecond || got.PerBatch != 20*time.Millisecond {
		t.Fatalf("lowered budgets = %+v", got)
	}
	m.Ether.Contributes.RankingSteps = []RankingStepMeta{{Key: "c", Budget: json.RawMessage(`{"perCandidateMicros":9000,"perBatchMillis":900}`)}}
	got = EffectiveBudgets(HotLimits(m))
	if got.PerCandidate != wasm.DefaultRankPerCandidate || got.PerBatch != wasm.DefaultRankBatch {
		t.Fatalf("budgets must never exceed the defaults: %+v", got)
	}
}

func TestHostFetchKeepsBinaryBodies(t *testing.T) {
	out, err := hostBridge{h: &memHost{kv: map[string][]byte{}}}.CallHost(context.Background(), "http.fetch", json.RawMessage(`[{"url":"https://dl.example.test/file.zip"}]`))
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Status     int    `json:"status"`
		BodyBase64 string `json:"bodyBase64"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	got, err := base64.StdEncoding.DecodeString(resp.BodyBase64)
	want := []byte{0x50, 0x4b, 0x03, 0x04, 0xff, 0xfe, 0x00, 0x80}
	if err != nil || resp.Status != 200 || string(got) != string(want) {
		t.Fatalf("binary body = %x (%v), want %x", got, err, want)
	}
}

func TestHostedPublicSettingsAccessor(t *testing.T) {
	eng := wasm.NewEngine(nil)
	t.Cleanup(func() { _ = eng.Close() })
	rt, err := HostedRuntimeFactory(eng)(Installed{ID: "test.settings",
		Manifest: Manifest{ID: "test.settings", Version: "1.0.0", Ether: &EtherBlock{Runtime: RuntimeDecl{Kind: "hosted", Entry: "plugin.ts"}}},
		Files: func(path string) ([]byte, bool) {
			return []byte(`globalThis.handle = () => ({status:200,body:ether.settings.get()});`), path == "plugin.ts"
		},
		Host: &memHost{kv: map[string][]byte{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	response, err := rt.Call(context.Background(), Request{Method: "GET", Path: "/ether/health"})
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Body) != `{"greeting":"Hi"}` {
		t.Fatalf("settings response: %s", response.Body)
	}
}

func (memHost) FoldersHash(context.Context, string, string, ...FolderHashOptions) (FolderHash, error) {
	return FolderHash{}, nil
}

func (memHost) FoldersHashStart(context.Context, string, string, ...FolderHashOptions) (FolderHashJob, error) {
	return FolderHashJob{}, nil
}

func (memHost) FoldersFrame(context.Context, string, string, FolderFrameOptions) (FolderFrame, error) {
	return FolderFrame{}, fmt.Errorf("folders.frame requires the hosted runtime")
}

func (memHost) FoldersHashPoll(context.Context, string) (FolderHashJob, error) {
	return FolderHashJob{}, nil
}

func (memHost) FoldersHashCancel(context.Context, string) (FolderHashJob, error) {
	return FolderHashJob{}, nil
}

func TestHostedEffectiveCallLimitsAndGovernance(t *testing.T) {
	eng := wasm.NewEngine(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = eng.Close() })

	var cfg config.Plugins
	env := newTestEnv(t)
	env.m.deps.ReadRuntimeConfig = func() config.Plugins { return cfg }
	env.m.RegisterRuntime("hosted", HostedRuntimeFactory(eng))

	files := helloFiles()
	files["dist/plugin.js"] = []byte(`
globalThis.handle = function(req) {
  const q = req.query || {};
  const burn = Number((q.burn && q.burn[0]) || 0);
  for (let i = 0; i < burn; i++) {
    ether.kv.put("k", "v");
  }
  const lim = ether.runtime.limits();
  return {
    status: 200,
    body: JSON.stringify({
      deadline: lim.deadline,
      unitBudget: lim.unitBudget,
      headerDeadline: (req.headers && req.headers["X-Ether-Deadline"]) || "",
    }),
  };
};
`)
	ctx := context.Background()
	prev, err := env.m.PreviewInstall(ctx, Source{Kind: "studio", Manifest: helloManifest("1.0.0"), Files: files})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := env.m.Install(ctx, prev, InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.m.SetEnabled(ctx, rec.ID, true); err != nil {
		t.Fatal(err)
	}

	type limitReply struct {
		Deadline       string `json:"deadline"`
		UnitBudget     int64  `json:"unitBudget"`
		HeaderDeadline string `json:"headerDeadline"`
	}
	callLimits := func(path string, burn int) (limitReply, error) {
		q := map[string]string{}
		if burn > 0 {
			q["burn"] = strconv.Itoa(burn)
		}
		resp, err := env.m.Call(ctx, rec.ID, Request{Method: "POST", Path: path, Query: q})
		if err != nil {
			return limitReply{}, err
		}
		var bodyStr string
		if err := json.Unmarshal(resp.Body, &bodyStr); err != nil {
			t.Fatalf("unmarshal outer body %s: %v", resp.Body, err)
		}
		var out limitReply
		if err := json.Unmarshal([]byte(bodyStr), &out); err != nil {
			t.Fatalf("unmarshal inner body %s: %v", bodyStr, err)
		}
		return out, nil
	}

	for _, tc := range []struct {
		path     string
		deadline string
		units    int64
		headSec  time.Duration
	}{
		{"/stream/movie/tt0133093.json", "3s", 30000, 3 * time.Second},
		{"/stream/tv/sports-1.json", "3s", 30000, 3 * time.Second},
		{"/ether/open/sports-1", "3s", 30000, 3 * time.Second},
		{"/ether/health", "5s", 1 << 60, 5 * time.Second},
		{"/ether/v1/kinds/games/resolve", "12s", 120000, 12 * time.Second},
		{"/ether/v1/kinds/games/match", "5s", 50000, 5 * time.Second},
		{"/ether/v1/kinds/games/search", "10s", 100000, 10 * time.Second},
		{"/ether/v1/kinds/games/browse", "12h0m0s", 1 << 60, 12 * time.Hour},
		{"/ether/v1/kinds/games/changes", "12h0m0s", 1 << 60, 12 * time.Hour},
		{"/ether/channels.json", "12h0m0s", 1 << 60, 12 * time.Hour},
		{"/ether/guide/ch-1.json?from=2026-11-02T00%3A00%3A00Z&to=2026-11-03T00%3A00%3A00Z", "12h0m0s", 1 << 60, 12 * time.Hour},
		{"/ether/job/sync", "12h0m0s", 1 << 60, 12 * time.Hour},
		{"/ether/test", "15s", 1 << 60, 15 * time.Second},
	} {
		before := time.Now().UTC().Truncate(time.Second)
		got, err := callLimits(tc.path, 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		if got.Deadline != tc.deadline || (tc.units < 1<<53 && got.UnitBudget != tc.units) || (tc.units >= 1<<53 && got.UnitBudget < 1<<59) {
			t.Fatalf("%s: runtime.limits = %+v, want deadline=%s unitBudget=%d", tc.path, got, tc.deadline, tc.units)
		}
		hd, err := time.Parse(time.RFC3339, got.HeaderDeadline)
		if err != nil {
			t.Fatalf("%s: parse X-Ether-Deadline %q: %v", tc.path, got.HeaderDeadline, err)
		}
		diff := hd.Sub(before)
		if diff < tc.headSec-2*time.Second || diff > tc.headSec+5*time.Second {
			t.Fatalf("%s: X-Ether-Deadline delta = %v, want ~%v", tc.path, diff, tc.headSec)
		}
	}

	// 1,500 kv.put calls = 15,000 units (> old 10,000 viewer clamp, < 120,000 resolve budget).
	if _, err := callLimits("/ether/v1/kinds/games/resolve", 1500); err != nil {
		t.Fatalf("resolve within 12s/120,000-unit budget failed: %v", err)
	}

	// Exceeding the 50,000-unit budget states the route and 5 s budget clearly.
	_, err = callLimits("/ether/v1/kinds/games/match", 5005)
	// Units are wall-time units, so the unit budget and the 5 s deadline are
	// the same limit and either can trip first on a slow host. Both must name
	// the route and its 5 s limit.
	honest := func(msg string) bool {
		return strings.Contains(msg, "used the whole 5 s budget for /ether/v1/kinds/games/match") ||
			strings.Contains(msg, "call exceeded its 5 s deadline for /ether/v1/kinds/games/match")
	}
	if err == nil || !honest(err.Error()) {
		t.Fatalf("expected honest governance error, got: %v", err)
	}
	health, err := env.m.HealthOf(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !honest(health.LastError) {
		t.Fatalf("health lastError = %q", health.LastError)
	}

	// Lowering the owner's per-plugin deadline ceiling clamps both X-Ether-Deadline and runtime.limits.
	cfg = config.Plugins{
		RuntimeOverrides: map[string]config.PluginRuntimeLimits{
			rec.ID: {Deadline: "2s"},
		},
	}
	env.m.closeRuntime(rec.ID)
	clamped, err := callLimits("/ether/v1/kinds/games/resolve", 0)
	if err != nil {
		t.Fatalf("clamped resolve: %v", err)
	}
	if clamped.Deadline != "2s" || clamped.UnitBudget != 20000 {
		t.Fatalf("clamped runtime.limits = %+v, want 2s / 20000", clamped)
	}
}
