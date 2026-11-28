package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/extend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestMaxPluginItemsRuntimeLimitsAndImpact(t *testing.T) {
	var cfg config.Plugins
	fake := &fakeRuntime{routes: map[string]func(Request) (Response, error){}}
	m, err := New(Deps{
		DB:       store.OpenTest(t),
		Registry: extend.New(),
		Tools:    tools.NewCatalogue(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		IsLeader: func() bool { return true },
		Now:      func() time.Time { return time.Now().UTC() },
		ReadRuntimeConfig: func() config.Plugins {
			return cfg
		},
		SaveRuntimeConfig: func(plugin string, limits config.PluginRuntimeLimits) error {
			if plugin == "" {
				cfg.RuntimeLimits = limits
				return nil
			}
			if cfg.RuntimeOverrides == nil {
				cfg.RuntimeOverrides = map[string]config.PluginRuntimeLimits{}
			}
			cfg.RuntimeOverrides[plugin] = limits
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.RegisterRuntime("hosted", func(inst Installed) (Runtime, error) {
		fake.inst = inst
		return fake, nil
	})
	m.SetKindBackends(KindBackends{
		KindItemCount: func(_ context.Context, pluginID string) (int, error) {
			if pluginID == "community.example.library" {
				return 1250, nil
			}
			return 0, nil
		},
	})

	ctx := context.Background()
	man := contentManifest()
	man.Ether.Contributes.MediaKinds[0].Sync = KindSyncDecl{Mode: KindSyncPush}
	man.Ether.Runtime.Limits = &RuntimeLimits{MaxItems: 500_000}
	raw, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.PreviewInstall(ctx, Source{
		Kind:     "studio",
		Manifest: raw,
		Files:    map[string][]byte{"dist/plugin.js": []byte("// library")},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := m.Install(ctx, p, InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		t.Fatal(err)
	}

	if got := m.MaxPluginItems("unconfigured.plugin"); got != DefaultMaxPluginItems {
		t.Fatalf("default MaxPluginItems = %d, want %d", got, DefaultMaxPluginItems)
	}
	if got := m.MaxPluginItems(rec.ID); got != 500_000 {
		t.Fatalf("manifest-clamped MaxPluginItems = %d, want 500000", got)
	}

	h := m.Handler()
	getDetail := func() Detail {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/"+rec.ID, nil)
		recW := httptest.NewRecorder()
		h.ServeHTTP(recW, req)
		if recW.Code != http.StatusOK {
			t.Fatalf("GET /%s: %d %s", rec.ID, recW.Code, recW.Body.String())
		}
		var d Detail
		if err := json.Unmarshal(recW.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}

	d := getDetail()
	if d.Impact.Items != 1250 || d.Impact.MaxItems != 500_000 {
		t.Fatalf("initial impact = %+v, want items=1250 maxItems=500000", d.Impact)
	}
	if d.RuntimeLimits == nil || d.RuntimeLimits.MaxItems != 500_000 {
		t.Fatalf("initial runtimeLimits = %+v, want maxItems=500000", d.RuntimeLimits)
	}

	putLimits := func(plugin string, maxItems int) int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"plugin": plugin,
			"limits": map[string]any{"maxItems": maxItems},
		})
		req := httptest.NewRequest(http.MethodPut, "/runtime-limits", bytes.NewReader(body))
		recW := httptest.NewRecorder()
		h.ServeHTTP(recW, req)
		return recW.Code
	}

	// Raising the global ceiling above 2,000,000 succeeds with no hidden cap.
	if code := putLimits("", 10_000_000); code != http.StatusOK {
		t.Fatalf("PUT global maxItems=10000000 status = %d, want 200", code)
	}
	if got := m.MaxPluginItems("unconfigured.plugin"); got != 10_000_000 {
		t.Fatalf("raised global MaxPluginItems = %d, want 10000000", got)
	}
	// Manifest request still bounds the plugin until overridden explicitly.
	if got := m.MaxPluginItems(rec.ID); got != 500_000 {
		t.Fatalf("plugin MaxPluginItems before override = %d, want 500000", got)
	}

	// Per-plugin owner override wins over manifest request and allows >2,000,000.
	if code := putLimits(rec.ID, 7_500_000); code != http.StatusOK {
		t.Fatalf("PUT plugin maxItems=7500000 status = %d, want 200", code)
	}
	if got := m.MaxPluginItems(rec.ID); got != 7_500_000 {
		t.Fatalf("overridden MaxPluginItems = %d, want 7500000", got)
	}
	if got := m.HostFor(rec.ID).(*host).DataLimits(ctx).MaxItems; got != 7_500_000 {
		t.Fatalf("DataLimits.MaxItems = %d, want 7500000", got)
	}
	d = getDetail()
	if d.Impact.MaxItems != 7_500_000 || d.RuntimeLimits == nil || d.RuntimeLimits.MaxItems != 7_500_000 {
		t.Fatalf("overridden detail = impact.maxItems=%d runtimeLimits=%+v", d.Impact.MaxItems, d.RuntimeLimits)
	}

	// Negative maxItems is rejected.
	if code := putLimits("", -1); code != http.StatusBadRequest {
		t.Fatalf("PUT negative maxItems status = %d, want 400", code)
	}
}

func TestRemoteManifestMaxItemsValidation(t *testing.T) {
	parseAndValidate := func(limits *RuntimeLimits) (Manifest, error) {
		man := contentManifest()
		man.Ether.Runtime = RuntimeDecl{
			Kind:   "remote",
			Limits: limits,
		}
		man.Ether.Contributes.MediaKinds[0].Sync = KindSyncDecl{Mode: KindSyncPush}
		raw, err := json.Marshal(man)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseManifest(raw)
		if err != nil {
			return Manifest{}, err
		}
		return parsed, parsed.Validate()
	}

	parsed, err := parseAndValidate(&RuntimeLimits{MaxItems: 300_000})
	if err != nil {
		t.Fatalf("remote manifest with maxItems rejected: %v", err)
	}
	if parsed.Ether.Runtime.Limits == nil || parsed.Ether.Runtime.Limits.MaxItems != 300_000 {
		t.Fatalf("parsed remote limits = %+v, want maxItems=300000", parsed.Ether.Runtime.Limits)
	}

	if _, err := parseAndValidate(&RuntimeLimits{MemoryBytes: 64 << 20}); err == nil {
		t.Fatal("remote manifest with hosted memoryBytes should fail validation")
	}
	if _, err := parseAndValidate(&RuntimeLimits{MaxItems: -5}); err == nil {
		t.Fatal("remote manifest with negative maxItems should fail validation")
	}
}

func TestHostResolveSourcesCategoryRecentValidation(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	rec := installContent(t, env)

	var gotQuery core.SourceQuery
	env.m.SetKindBackends(KindBackends{
		ResolveSources: func(_ context.Context, _ string, q core.SourceQuery) ([]core.SourceCandidate, error) {
			gotQuery = q
			score := 120
			return []core.SourceCandidate{{
				Title:      "Recent Release",
				NZBURL:     "https://idx.example/nzb/1",
				Score:      &score,
				Resolution: 1080,
				Source:     "webdl",
				Codec:      "hevc",
			}}, nil
		},
	})

	h := env.m.HostFor(rec.ID)

	// Empty query with no category is still rejected at the host boundary.
	var ve *ValidationError
	if _, err := h.ResolveSources(ctx, core.SourceQuery{Kind: "indexer_search"}); !errors.As(err, &ve) {
		t.Fatalf("ResolveSources empty query+category err = %v, want *ValidationError", err)
	}

	// Empty query with category is allowed for recent/RSS category searches.
	cands, err := h.ResolveSources(ctx, core.SourceQuery{
		Kind:     "indexer_search",
		Category: "audio",
		Profile:  "Standard",
	})
	if err != nil {
		t.Fatalf("ResolveSources category-only err = %v", err)
	}
	if gotQuery.Category != "audio" || gotQuery.Profile != "Standard" || gotQuery.Query != "" {
		t.Fatalf("backend got query %+v", gotQuery)
	}
	if len(cands) != 1 || cands[0].Score == nil || *cands[0].Score != 120 {
		t.Fatalf("candidates = %+v", cands)
	}
}

func TestOwnerSharedCacheRuntimeLimits(t *testing.T) {
	env := newTestEnv(t)
	rec := installContent(t, env)
	cfg := config.Plugins{}
	env.m.deps.ReadRuntimeConfig = func() config.Plugins { return cfg }
	env.m.deps.SaveRuntimeConfig = func(id string, limits config.PluginRuntimeLimits) error {
		if id != "" {
			t.Fatalf("unexpected per-plugin save: %q", id)
		}
		cfg.RuntimeLimits = limits
		return nil
	}
	h := env.m.Handler()
	request := func(method, plugin string, limits config.PluginRuntimeLimits, want int) config.PluginRuntimeLimits {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"plugin": plugin, "limits": limits})
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/runtime-limits", bytes.NewReader(raw)))
		if w.Code != want {
			t.Fatalf("%s %q = %d, want %d: %s", method, plugin, w.Code, want, w.Body)
		}
		var out config.Plugins
		if want == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return out.RuntimeLimits
	}
	raised := config.PluginRuntimeLimits{SharedCacheBytes: 64 << 30, FetchBytes: 20 << 30, ArchiveMemberBytes: 20 << 30}
	request(http.MethodPut, "", raised, http.StatusOK)
	if got := request(http.MethodGet, "", config.PluginRuntimeLimits{}, http.StatusOK); got != raised {
		t.Fatalf("GET after save = %+v, want %+v", got, raised)
	}
	grant := env.m.HostFor(rec.ID).(*host).DataLimits(context.Background())
	if grant.FetchBytes != raised.FetchBytes || grant.ArchiveMemberBytes != raised.ArchiveMemberBytes {
		t.Fatalf("host grant after save = %+v", grant)
	}
	request(http.MethodPut, rec.ID, raised, http.StatusBadRequest)
	request(http.MethodPut, "", config.PluginRuntimeLimits{SharedCacheBytes: -1}, http.StatusBadRequest)
	if cfg.RuntimeLimits != raised {
		t.Fatalf("invalid setting changed active config: %+v", cfg)
	}
	request(http.MethodPut, "", config.PluginRuntimeLimits{}, http.StatusOK)
	if got := request(http.MethodGet, "", config.PluginRuntimeLimits{}, http.StatusOK); got.SharedCacheBytes != 0 {
		t.Fatalf("GET after reset = %+v", got)
	}
}
