package adapters_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/adapters"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
)

const (
	hintHashA = "0123456789abcdef0123456789abcdef01234567"
	hintHashB = "89abcdef0123456789abcdef0123456789abcdef"
)

func installCacheHintPlugin(t *testing.T, h *listTestHarness, id string) plugins.Record {
	t.Helper()
	man := map[string]any{"id": id, "version": "1.0.0", "name": "Test cache hints", "ether": map[string]any{
		"protocol": 1, "kind": "source", "summary": "Cache hints for testing",
		"runtime":      map[string]any{"kind": "hosted", "entry": "dist/plugin.js"},
		"capabilities": []string{"debrid_cache_hints"},
	}}
	raw, _ := json.Marshal(man)
	p, err := h.mgr.PreviewInstall(t.Context(), plugins.Source{Kind: "studio", Manifest: raw, Files: map[string][]byte{"dist/plugin.js": []byte("// fixture")}})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := h.mgr.Install(t.Context(), p, plugins.InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

type hintAnswers struct {
	mu  sync.Mutex
	got []map[string][]string
}

func (a *hintAnswers) found(m map[string][]string) {
	a.mu.Lock()
	a.got = append(a.got, m)
	a.mu.Unlock()
}

func (a *hintAnswers) all() []map[string][]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]map[string][]string(nil), a.got...)
}

var hintCands = []debrid.CacheHintCandidate{
	{InfoHash: hintHashA, ProviderKinds: []string{config.DebridRealDebrid}},
	{InfoHash: hintHashB, Private: true, ProviderKinds: []string{config.DebridRealDebrid, config.DebridTorBox}},
}

// An enabled plugin with debrid_cache_hints gets the shortlist on
// POST /ether/debrid/hints; its answer is handed on with hashes in lower
// case. Disabling the plugin stops the calls.
func TestDebridCacheHintPluginWireAndLifecycle(t *testing.T) {
	h := newListTestHarness(t)
	rec := installCacheHintPlugin(t, h, "community.hints.test")
	hints := adapters.AttachDebridCacheHints(h.mgr, debrid.NewService(config.Debrid{}, debrid.Options{}))
	var body json.RawMessage
	h.rt.routes["/ether/debrid/hints"] = func(req plugins.Request) (plugins.Response, error) {
		if req.Method != "POST" {
			t.Errorf("method %s", req.Method)
		}
		body = req.Body
		return plugins.Response{Status: 200, Body: json.RawMessage(`{"cached":{"0123456789ABCDEF0123456789ABCDEF01234567":["realdebrid"]}}`)}, nil
	}
	var answers hintAnswers
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	hints.CacheHints(ctx, hintCands, answers.found)
	want := `{"candidates":[{"infoHash":"` + hintHashA + `","private":false,"providerKinds":["realdebrid"]},{"infoHash":"` + hintHashB + `","private":true,"providerKinds":["realdebrid","torbox"]}]}`
	if string(body) != want {
		t.Fatalf("request body\n got %s\nwant %s", body, want)
	}
	got := answers.all()
	if len(got) != 1 || len(got[0]) != 1 || len(got[0][hintHashA]) != 1 || got[0][hintHashA][0] != config.DebridRealDebrid {
		t.Fatalf("answers %+v", got)
	}

	if err := h.mgr.SetEnabled(t.Context(), rec.ID, false); err != nil {
		t.Fatal(err)
	}
	h.rt.resetCalls()
	hints.CacheHints(ctx, hintCands, answers.found)
	if paths := h.rt.paths(); len(paths) != 0 {
		t.Fatalf("disabled plugin was called: %v", paths)
	}
}

// Two plugins are asked at the same time, under the caller's deadline.
func TestDebridCacheHintsAsksPluginsConcurrently(t *testing.T) {
	h := newListTestHarness(t)
	installCacheHintPlugin(t, h, "community.hints.one")
	installCacheHintPlugin(t, h, "community.hints.two")
	hints := adapters.AttachDebridCacheHints(h.mgr, debrid.NewService(config.Debrid{}, debrid.Options{}))
	var started sync.WaitGroup
	started.Add(2)
	both := make(chan struct{})
	go func() { started.Wait(); close(both) }()
	h.rt.callContext = func(ctx context.Context, req plugins.Request) (plugins.Response, error) {
		if req.Path != "/ether/debrid/hints" {
			return plugins.Response{Status: 404}, nil
		}
		started.Done()
		select {
		case <-both: // answers only once the other plugin was asked too
		case <-ctx.Done():
			return plugins.Response{}, ctx.Err()
		}
		hash := hintHashA
		if req.PluginID == "community.hints.two" {
			hash = hintHashB
		}
		b, _ := json.Marshal(map[string]any{"cached": map[string][]string{hash: {config.DebridRealDebrid}}})
		return plugins.Response{Status: 200, Body: b}, nil
	}
	var answers hintAnswers
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	hints.CacheHints(ctx, hintCands, answers.found)
	seen := map[string]bool{}
	for _, m := range answers.all() {
		for hash := range m {
			seen[hash] = true
		}
	}
	if !seen[hintHashA] || !seen[hintHashB] {
		t.Fatalf("answers %+v", answers.all())
	}
}

// A plugin that runs out of time, on Ether's deadline or its own route
// budget, or answers late hands nothing on, and none of these counts
// against its health.
func TestDebridCacheHintTimeoutsKeepPluginHealthy(t *testing.T) {
	for name, answer := range map[string]func(context.Context) (plugins.Response, error){
		"cancelled": func(ctx context.Context) (plugins.Response, error) {
			<-ctx.Done()
			return plugins.Response{}, ctx.Err()
		},
		"late answer": func(ctx context.Context) (plugins.Response, error) {
			<-ctx.Done()
			return plugins.Response{Status: 200, Body: json.RawMessage(`{"cached":{"` + hintHashA + `":["realdebrid"]}}`)}, nil
		},
		"route budget spent first": func(context.Context) (plugins.Response, error) {
			return plugins.Response{}, &wasm.Error{Status: wasm.StatusTimeout, Message: "deadline exceeded", Trap: true}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newListTestHarness(t)
			rec := installCacheHintPlugin(t, h, "community.hints.slow")
			hints := adapters.AttachDebridCacheHints(h.mgr, debrid.NewService(config.Debrid{}, debrid.Options{}))
			h.rt.callContext = func(ctx context.Context, req plugins.Request) (plugins.Response, error) {
				if req.Path != "/ether/debrid/hints" {
					return plugins.Response{Status: 404}, nil
				}
				return answer(ctx)
			}
			before, err := h.mgr.HealthOf(t.Context(), rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			var answers hintAnswers
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			hints.CacheHints(ctx, hintCands, answers.found)
			if got := answers.all(); len(got) != 0 {
				t.Fatalf("handed on %+v", got)
			}
			after, err := h.mgr.HealthOf(t.Context(), rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != before.Status || after.Slow != before.Slow || len(after.Failures) != len(before.Failures) || after.Restarts != before.Restarts || after.LastError != before.LastError {
				t.Fatalf("health changed: before %+v after %+v", before, after)
			}
		})
	}
}

// The hook reports itself off while no enabled plugin declares the
// capability, so preparation skips it before reading anything.
func TestDebridCacheHintsEnabledOnlyWithAPlugin(t *testing.T) {
	h := newListTestHarness(t)
	hints := adapters.AttachDebridCacheHints(h.mgr, debrid.NewService(config.Debrid{}, debrid.Options{}))
	if hints.Enabled() {
		t.Fatal("enabled with no hint plugin installed")
	}
	rec := installCacheHintPlugin(t, h, "community.hints.test")
	if !hints.Enabled() {
		t.Fatal("not enabled with an enabled hint plugin")
	}
	if err := h.mgr.SetEnabled(t.Context(), rec.ID, false); err != nil {
		t.Fatal(err)
	}
	if hints.Enabled() {
		t.Fatal("still enabled after the only hint plugin was disabled")
	}
}
