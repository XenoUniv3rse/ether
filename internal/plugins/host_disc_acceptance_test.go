package plugins

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/segcache"
)

// ETHER_TEST_DISC_DIR opts into a 15 GiB physical transfer. A new temporary
// directory is created there and removed after the offline restart check.
func TestPhysicalDiscCache(t *testing.T) {
	parent := os.Getenv("ETHER_TEST_DISC_DIR")
	if parent == "" {
		t.Skip("set ETHER_TEST_DISC_DIR to a writable directory with 15 GiB free")
	}
	const disc int64 = 15 << 30
	ctx := withCallPath(context.Background(), "/ether/job/import")
	ref := os.Getenv("ETHER_TEST_DISC_REF")
	dir := parent
	if ref == "" {
		var err error
		dir, err = os.MkdirTemp(parent, "ether-disc-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)
	}
	env := newTestEnv(t)
	rec := installContent(t, env)
	rec.Manifest.Ether.Permissions.Network = []string{"disc.example.test"}
	rec.ManifestRaw, _ = json.Marshal(rec.Manifest)
	if err := env.m.store.upsert(ctx, rec); err != nil {
		t.Fatal(err)
	}
	env.m.deps.ReadRuntimeConfig = func() config.Plugins {
		return config.Plugins{RuntimeLimits: config.PluginRuntimeLimits{FetchBytes: 20 << 30}}
	}
	pool, err := segcache.Open(dir, 24<<30, nil)
	if err != nil {
		t.Fatal(err)
	}
	files, err := blobcache.Open(filepath.Join(dir, "plugin-media"), blobcache.Options{Unified: true, SweepPeriod: -1, PoolCapacity: pool.MediaFileCapacity, MakeFileRoom: pool.MakeFileRoom, CheckVolume: pool.CheckVolume})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	pool.SetFilePool(files)
	closeCache := func() { pool.SetFilePool(nil); files.Close(); pool.Close() }
	library := core.PluginLibraryKey(rec.ID, "recording")
	files.BindPluginLibrary(rec.ID, library)
	put, open, del := files.HostBackends(nil)
	env.m.SetKindBackends(KindBackends{CachePut: put, CacheOpen: open, CacheDelete: del, CachePartial: files.HostPartialBackend(nil), CacheLimit: func(_, library string) int64 { return files.Namespace(library, blobcache.Quota{}).Quota().MaxBytes }})
	host := env.m.HostFor(rec.ID)
	if ref != "" {
		defer closeCache()
		cached, ok := files.Namespace(library, blobcache.Quota{}).LookupKey("disc.iso")
		if !ok || cached.String() != ref {
			t.Fatalf("offline lookup = %s, %v", cached, ok)
		}
		r, err := host.CacheOpen(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		hasher := sha256.New()
		zero := make([]byte, 1<<20)
		chunk := make([]byte, len(zero))
		var total int64
		for {
			n, err := r.Read(chunk)
			if !bytes.Equal(chunk[:n], zero[:n]) {
				t.Fatalf("unexpected disc contents near %d", total)
			}
			hasher.Write(chunk[:n])
			total += int64(n)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if total != disc || hex.EncodeToString(hasher.Sum(nil)) != strings.TrimPrefix(ref, "blob:") {
			t.Fatalf("offline byte count %d or hash did not match %s", total, ref)
		}
		t.Logf("fresh process %d verified every byte and SHA-256 of %d bytes: %s", os.Getpid(), total, ref)
		return
	}
	// An actual HTTP origin streams deterministic bytes on loopback; no input
	// file, public resolver or external network is needed.
	var out FetchCacheResult
	func() {
		defer closeCache()
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", strconv.FormatInt(disc, 10))
			chunk := make([]byte, 1<<20)
			for remaining := disc; remaining > 0; remaining -= int64(len(chunk)) {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		}))
		defer origin.Close()
		dialer := new(net.Dialer)
		transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, origin.Listener.Addr().String())
		}}
		defer transport.CloseIdleConnections()
		env.m.client = &http.Client{Transport: transport}
		args, _ := json.Marshal([]any{map[string]any{"url": "http://disc.example.test/disc.iso", "libraryKey": library, "name": "disc.iso", "resume": true, "timeoutMs": (20 * time.Minute).Milliseconds()}})
		t.Logf("writer process %d downloading %d bytes into %s", os.Getpid(), disc, dir)
		raw, err := CallDataHost(ctx, host, "http.fetchToCache", args)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if out.Size != disc {
			t.Fatalf("downloaded %d, want %d", out.Size, disc)
		}
		if used, budget := pool.Usage(); used != disc || budget != 24<<30 {
			t.Fatalf("pool usage %d/%d", used, budget)
		}
	}()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestPhysicalDiscCache$", "-test.v", "-test.timeout=20m")
	cmd.Env = append(os.Environ(), "ETHER_TEST_DISC_DIR="+dir, "ETHER_TEST_DISC_REF="+out.Blob)
	verified, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("offline reopen: %v\n%s", err, verified)
	}
	t.Logf("origin stopped and writer cache closed before reopening:\n%s", verified)
}
