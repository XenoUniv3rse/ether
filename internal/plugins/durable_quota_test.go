package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/config"
)

func TestDurableQuotaAndUsage(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	rec := installContent(t, env)
	rec.Manifest.Ether.Permissions.Storage = "durable"
	rec.ManifestRaw, _ = json.Marshal(rec.Manifest)
	if err := env.m.store.upsert(ctx, rec); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	limits := config.PluginRuntimeLimits{DurableBytes: 4, DurableFiles: 1}
	cfg := config.Plugins{RuntimeOverrides: map[string]config.PluginRuntimeLimits{rec.ID: limits}}
	free := int64(8 << 30)
	store, err := blobcache.OpenDurableStore(root, func() (int64, int64, error) {
		return 16 << 30, free, nil
	}, func(plugin string) (blobcache.DurableQuota, int) {
		l := cfg.RuntimeOverrides[plugin]
		return blobcache.DurableQuota{Bytes: l.DurableBytes, Files: l.DurableFiles}, cfg.RuntimeLimits.DurableFreePercent
	})
	if err != nil {
		t.Fatal(err)
	}
	env.m.deps.DurableStorage = store
	env.m.deps.ReadRuntimeConfig = func() config.Plugins { return cfg }
	env.m.deps.SaveRuntimeConfig = func(id string, l config.PluginRuntimeLimits) error {
		cfg.RuntimeOverrides[id] = l
		return nil
	}

	save := func(bytes int64, files int, want int) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"plugin": rec.ID,
			"limits": config.PluginRuntimeLimits{DurableBytes: bytes, DurableFiles: files}})
		w := httptest.NewRecorder()
		env.m.handleRuntimeLimits(w, httptest.NewRequest(http.MethodPut, "/runtime-limits", strings.NewReader(string(body))))
		if w.Code != want {
			t.Fatalf("saving (%d, %d) = %d, want %d: %s", bytes, files, w.Code, want, w.Body.String())
		}
	}
	h := env.m.HostFor(rec.ID)
	write := func(data string) (string, error) {
		args, _ := json.Marshal([]any{"", "save", data, map[string]bool{"durable": true}})
		out, err := CallDataHost(ctx, h, "cache.put", args)
		var result struct{ Blob string }
		if err == nil {
			err = json.Unmarshal(out, &result)
		}
		return result.Blob, err
	}
	checkUsage := func(want blobcache.DurableUsage) {
		t.Helper()
		raw, err := CallDataHost(withCallPath(ctx, "/ether/v1/frontend/save"), h, "cache.usage", json.RawMessage(`[{"durable":true}]`))
		var got blobcache.DurableUsage
		if err != nil || json.Unmarshal(raw, &got) != nil || got != want {
			t.Fatalf("usage = %s, %v, want %+v", raw, err, want)
		}
	}
	first, err := write("abcd")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := write("efgh"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("write above old quota: %v", err)
	}
	save(8, 2, http.StatusOK)
	if _, err := write("efgh"); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := write("abcd"); err != nil || duplicate != first {
		t.Fatalf("duplicate = %q, %v; want %q", duplicate, err, first)
	}
	checkUsage(blobcache.DurableUsage{Bytes: 8, Files: 2, DurableQuota: blobcache.DurableQuota{Bytes: 8, Files: 2}})

	save(2, 1, http.StatusOK)
	if _, err := write("ijkl"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("write after quota reduction: %v", err)
	}
	checkUsage(blobcache.DurableUsage{Bytes: 8, Files: 2, DurableQuota: blobcache.DurableQuota{Bytes: 2, Files: 1}})
	src, err := store.Open(rec.ID, blobcache.Ref(first))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	_, err = src.Read(got)
	src.Close()
	if err != nil || !bytes.Equal(got, []byte("abcd")) {
		t.Fatalf("saved data changed: %q, %v", got, err)
	}

	save(2<<30+1, 2, http.StatusBadRequest)
	save(8, (2<<30)/4096+1, http.StatusBadRequest)
	save(2<<30, 2, http.StatusOK)
	free = 2 << 30
	checkUsage(blobcache.DurableUsage{Bytes: 8, Files: 2, DurableQuota: blobcache.DurableQuota{Bytes: 1 << 30, Files: 2}})

	// A sparse existing blob models a full 1 GiB namespace without allocating
	// or hashing a large upload. Reopening reconstructs its on-disk usage.
	paths, err := filepath.Glob(filepath.Join(root, "saved", "*", "*"))
	if err != nil || len(paths) != 2 {
		t.Fatalf("saved paths: %d, %v", len(paths), err)
	}
	if err := os.Truncate(paths[0], blobcache.DurableDefaultBytes); err != nil {
		t.Fatal(err)
	}
	reopened, err := blobcache.OpenDurableStore(root, func() (int64, int64, error) {
		return 16 << 30, 8 << 30, nil
	}, func(string) (blobcache.DurableQuota, int) {
		return blobcache.DurableQuota{Bytes: 2 << 30, Files: 3}, 25
	})
	if err != nil {
		t.Fatal(err)
	}
	env.m.deps.DurableStorage = reopened
	if _, err := write("mnop"); err != nil {
		t.Fatalf("write past 1 GiB with raised quota: %v", err)
	}
	checkUsage(blobcache.DurableUsage{Bytes: (1 << 30) + 8, Files: 3, DurableQuota: blobcache.DurableQuota{Bytes: 2 << 30, Files: 3}})
}
