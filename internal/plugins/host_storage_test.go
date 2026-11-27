package plugins

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestAdapterUploadStorageContract(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	rec := installContent(t, env)
	rec.Manifest.Ether.Capabilities = append(rec.Manifest.Ether.Capabilities, CapFrontendAdapter)
	rec.Manifest.Ether.Permissions.Storage = "durable"
	rec.Manifest.Ether.Contributes.FrontendAdapters = []FrontendAdapterMeta{{
		Key: "feed", Label: "Feed", ProtocolHint: "opds", PathPrefix: "/opds",
		UploadPaths: []FrontendUploadPath{{Path: "/save/*", MaxBytes: 192 << 20}, {Path: "/save/small", MaxBytes: 4}},
	}}
	var err error
	rec.ManifestRaw, err = json.Marshal(rec.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.m.store.upsert(ctx, rec); err != nil {
		t.Fatal(err)
	}
	storageDir := t.TempDir()
	saved, err := blobcache.OpenDurableStore(storageDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	env.m.deps.DurableStorage = saved
	cfg := config.Plugins{RuntimeOverrides: map[string]config.PluginRuntimeLimits{}}
	env.m.deps.ReadRuntimeConfig = func() config.Plugins { return cfg }
	if _, _, err := env.m.BeginAdapterUpload(ctx, rec.ID, "feed", "/save/large", 64<<20+1); !errors.Is(err, blobcache.ErrTooLarge) {
		t.Fatalf("default cap: %v", err)
	}
	cfg.RuntimeOverrides[rec.ID] = config.PluginRuntimeLimits{UploadBytes: 256 << 20}
	upload, limit, err := env.m.BeginAdapterUpload(ctx, rec.ID, "feed", "/save/large", 80<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer upload.Close()
	if limit != 192<<20 {
		t.Fatalf("effective cap = %d, want 192 MiB", limit)
	}
	if _, _, err := env.m.BeginAdapterUpload(ctx, rec.ID, "feed", "/save/small", 5); !errors.Is(err, blobcache.ErrTooLarge) {
		t.Fatalf("smaller route cap: %v", err)
	}
	if adapterUploadCeiling(MaxAdapterUploadBytes+1) != MaxAdapterUploadBytes {
		t.Fatal("configuration may not exceed the host maximum")
	}

	data := bytes.Repeat([]byte("staged example"), 25000)
	staged, _, err := upload.Stage(ctx, "save.bin", "", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	durable, err := saved.Promote(rec.ID, staged)
	if err != nil {
		t.Fatal(err)
	}
	// A file already present at startup has no in-memory digest entry.
	coldData := []byte("pre-existing saved file")
	coldDigest := fmt.Sprintf("%x", sha256.Sum256(coldData))
	pluginDir := fmt.Sprintf("%x", sha256.Sum256([]byte(rec.ID)))
	if err := os.WriteFile(filepath.Join(storageDir, "saved", pluginDir, coldDigest), coldData, 0o600); err != nil {
		t.Fatal(err)
	}

	cache, err := blobcache.Open(t.TempDir(), blobcache.Options{SweepPeriod: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	put, _, _ := cache.HostBackends(nil)
	env.m.SetKindBackends(KindBackends{CacheOpenForStat: cache.OpenPluginBlobForStat})
	cacheData := bytes.Repeat([]byte("different cached example"), 21000)
	library := core.PluginLibraryKey(rec.ID, "recording")
	cached, err := put(ctx, rec.ID, library, "cache.bin", bytes.NewReader(cacheData))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := put(ctx, rec.ID+"-other", library+"-other", "cache.bin", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	h := env.m.HostFor(rec.ID)
	request := func(ctx context.Context, ref string) (blobcache.Stat, error) {
		var stat blobcache.Stat
		raw, err := CallDataHost(ctx, h, "cache.stat", json.RawMessage(fmt.Sprintf("[%q]", ref)))
		if err == nil {
			err = json.Unmarshal(raw, &stat)
		}
		return stat, err
	}
	check := func(ctx context.Context, ref string, data []byte) {
		t.Helper()
		got, err := request(ctx, ref)
		want := blobcache.Stat{
			Size: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)),
			SHA1: fmt.Sprintf("%x", sha1.Sum(data)), MD5: fmt.Sprintf("%x", md5.Sum(data)),
			CRC32: fmt.Sprintf("%08x", crc32.ChecksumIEEE(data)),
		}
		if err != nil || got != want {
			t.Fatalf("cache.stat(%s) = %+v, %v, want %+v", ref, got, err, want)
		}
	}
	viewer := withCallPath(ctx, "/ether/v1/frontend/feed/save/large")
	check(viewer, staged.String(), data)
	check(viewer, durable.String(), data)
	check(viewer, blobcache.DurableRefPrefix+coldDigest, coldData)
	if _, err := request(viewer, cached); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("cold large blob on viewer path: %v", err)
	}
	remote := rec.Manifest
	remoteEther := *remote.Ether
	remote.Ether = &remoteEther
	remote.Ether.Runtime.Kind = "remote"
	release := env.m.trackRemoteClientCall(remote, "/stream/movie/example.json")
	if _, err := request(ctx, cached); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("cold large blob during a remote viewer call without a token: %v", err)
	}
	release()

	check(withCallPath(ctx, "/ether/job/digests"), cached, cacheData)
	check(viewer, cached, cacheData)
	if _, err := request(viewer, foreign); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cached digest for an unowned blob: %v", err)
	}
}
