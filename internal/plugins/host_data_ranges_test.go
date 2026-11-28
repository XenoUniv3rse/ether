package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

func TestFetchToCacheParallelRanges(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	rec := installContent(t, env)
	rec.Manifest.Ether.Permissions.Network = []string{"127.0.0.1"}
	var err error
	rec.ManifestRaw, err = json.Marshal(rec.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.m.store.upsert(ctx, rec); err != nil {
		t.Fatal(err)
	}

	if err := env.m.SetGrants(ctx, rec.ID, Grants{LocalHosts: []string{"127.0.0.1"}}); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	var cache *blobcache.Cache
	openCache := func() {
		t.Helper()
		cache, err = blobcache.Open(root, blobcache.Options{SweepPeriod: -1})
		if err != nil {
			t.Fatal(err)
		}
		put, open, remove := cache.HostBackends(nil)
		env.m.SetKindBackends(KindBackends{
			CachePut: put, CacheOpen: open, CacheDelete: remove,
			CachePartial: cache.HostPartialBackend(nil),
		})
	}
	openCache()
	defer func() { cache.Close() }()

	const block = 8 << 10
	payload := make([]byte, 32*block)
	for i := range payload {
		payload[i] = byte(i ^ (i >> 8) ^ (i >> 15))
	}

	var active, peak atomic.Int32
	var incompatibleRange, fellBack sync.Map
	var retry atomic.Bool
	var resumedOffset atomic.Int64
	var retriedFinishedRange atomic.Bool
	var replicaRanges, replicaFallback sync.Map
	type connectionKey struct{}
	type connection struct {
		mu     sync.Mutex
		active atomic.Int32
	}
	var primary, secondary *httptest.Server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/replicas", "/replicas-different-tag", "/replicas-different-length", "/replicas-weak":
			target := primary.URL + "/primary" + r.URL.Path
			if requested := r.Header.Get("Range"); requested != "" {
				replicaRanges.Store(r.URL.Path, true)
				if !strings.HasPrefix(requested, "bytes=0-") {
					target = secondary.URL + "/secondary" + r.URL.Path
				}
			} else if _, attempted := replicaRanges.Load(r.URL.Path); attempted {
				replicaFallback.Store(r.URL.Path, true)
			}
			http.Redirect(w, r, target, http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Accept-Ranges", "bytes")
		var validator string
		switch r.URL.Path {
		case "/date":
			validator = time.Unix(1000000, 0).UTC().Format(http.TimeFormat)
			w.Header().Set("Last-Modified", validator)
		case "/primary/replicas-weak", "/secondary/replicas-weak":
			w.Header().Set("ETag", `W/"v1"`)
			w.Header().Set("Last-Modified", time.Unix(1000000, 0).UTC().Format(http.TimeFormat))
		default:
			validator = `"v1"`
			w.Header().Set("ETag", validator)
		}
		if r.URL.Path == "/primary/replicas" || r.URL.Path == "/secondary/replicas" {
			// The two copies carry the same strong ETag and independent timestamps.
			modified := int64(1000000)
			if r.URL.Path == "/secondary/replicas" {
				modified += 3600
			}
			w.Header().Set("Last-Modified", time.Unix(modified, 0).UTC().Format(http.TimeFormat))
		}

		start, end := 0, len(payload)
		ranged := r.Header.Get("Range") != ""
		switch r.URL.Path {
		case "/ignored", "/changed", "/wrong-range":
			if ranged {
				incompatibleRange.Store(r.URL.Path, true)
			} else if _, attempted := incompatibleRange.Load(r.URL.Path); attempted {
				fellBack.Store(r.URL.Path, true)
			}
		}
		if ranged && r.URL.Path != "/ignored" {
			if strings.HasPrefix(r.URL.Path, "/primary/") || strings.HasPrefix(r.URL.Path, "/secondary/") {
				validator = "" // If-Range is stripped on these cross-origin redirects.
			}
			if r.Header.Get("If-Range") != validator {
				t.Error("unexpected If-Range validator")
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil ||
				start < 0 || end < start || end >= len(payload) {
				t.Error("invalid Range header")
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			end++
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(payload)))
			if r.URL.Path == "/changed" {
				w.Header().Set("ETag", `"v2"`)
			}
			if r.URL.Path == "/wrong-range" {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", start, end-1))
			}
			switch r.URL.Path {
			case "/secondary/replicas-different-tag":
				w.Header().Set("ETag", `"v2"`)
			case "/secondary/replicas-different-length":
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(payload)+1))
			}
		}
		w.Header().Set("Content-Length", strconv.Itoa(end-start))
		if ranged && r.URL.Path != "/ignored" {
			w.WriteHeader(http.StatusPartialContent)
		}
		w.(http.Flusher).Flush()

		if r.URL.Path == "/resume" && ranged {
			if retry.Load() {
				if start >= len(payload)/4 {
					retriedFinishedRange.Store(true)
				}
				resumedOffset.Store(int64(start))
			} else if start == 0 {
				// The declared range is a quarter of the file. Close the
				// response after sending only a prefix of that range.
				end = 2 * block
			}
		}

		if r.URL.Path == "/throttled" || r.URL.Path == "/date" {
			conn := r.Context().Value(connectionKey{}).(*connection)
			if ranged {
				if conn.active.Add(1) > 1 {
					t.Error("parallel ranges shared a connection")
				}
				defer conn.active.Add(-1)
				if r.ProtoMajor != 2 {
					t.Error("expected to exercise HTTP/2")
				}
				inFlight := active.Add(1)
				defer active.Add(-1)
				for previous := peak.Load(); inFlight > previous && !peak.CompareAndSwap(previous, inFlight); {
					previous = peak.Load()
				}
			}
			writeBlock := func(data []byte) bool {
				conn.mu.Lock()
				defer conn.mu.Unlock()
				select {
				case <-r.Context().Done():
					return false
				case <-time.After(8 * time.Millisecond):
				}
				if _, err := w.Write(data); err != nil {
					return false
				}
				w.(http.Flusher).Flush()
				return true
			}
			for start < end {
				next := min(start+block, end)
				if !writeBlock(payload[start:next]) {
					return
				}
				start = next
			}
			return
		}
		_, _ = w.Write(payload[start:end])
	})
	primary = httptest.NewTLSServer(handler)
	defer primary.Close()
	secondary = httptest.NewTLSServer(handler)
	defer secondary.Close()
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, connectionKey{}, &connection{})
	}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	env.m.client = (&safefetch.Guard{}).Client(srv.Client())
	defer env.m.client.CloseIdleConnections()
	env.m.fetchHosts = newFetchHostLimiter(4)
	h := env.m.HostFor(rec.ID)
	libraryKey := core.PluginLibraryKey(rec.ID, "recording")
	job := withCallPath(ctx, "/ether/job/import")
	fetch := func(t *testing.T, callCtx context.Context, path, name string, connections int, resume bool) (FetchCacheResult, error) {
		t.Helper()
		args, err := json.Marshal([]map[string]any{{
			"url": srv.URL + path, "libraryKey": libraryKey,
			"name": name, "connections": connections, "resume": resume,
		}})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := CallDataHost(callCtx, h, "http.fetchToCache", args)
		if err != nil {
			return FetchCacheResult{}, err
		}
		var out FetchCacheResult
		err = json.Unmarshal(raw, &out)
		return out, err
	}
	fetchOK := func(t *testing.T, callCtx context.Context, path, name string, connections int, resume bool) FetchCacheResult {
		t.Helper()
		out, err := fetch(t, callCtx, path, name, connections, resume)
		if err != nil {
			t.Fatal(err)
		}
		file, err := h.CacheOpen(ctx, out.Blob)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		got, err := io.ReadAll(file)
		if err != nil || !bytes.Equal(got, payload) || out.Size != int64(len(payload)) || out.Status != http.StatusOK {
			t.Fatalf("incomplete or corrupt blob: result=%+v, error=%v", out, err)
		}
		return out
	}

	t.Run("throttled", func(t *testing.T) {
		start := time.Now()
		serial := fetchOK(t, job, "/throttled", "serial.bin", 1, true)
		serialTime := time.Since(start)
		start = time.Now()
		parallel := fetchOK(t, job, "/throttled", "parallel.bin", 8, true)
		parallelTime := time.Since(start)
		if serial != parallel {
			t.Fatalf("result changed with parallelism: %+v vs %+v", serial, parallel)
		}
		if n := peak.Load(); n <= 1 || n > 4 {
			t.Fatalf("peak of %d concurrent ranges, want overlap within host cap of 4", n)
		}
		t.Logf("throttled 256 KiB: one connection %v, four connections %v (%.2fx)",
			serialTime, parallelTime, float64(serialTime)/float64(parallelTime))
	})
	t.Run("last-modified", func(t *testing.T) {
		peak.Store(0)
		fetchOK(t, job, "/date", "date.bin", 4, true)
		if peak.Load() <= 1 {
			t.Fatal("strong Last-Modified did not enable overlapping ranges")
		}
	})
	t.Run("interactive", func(t *testing.T) {
		peak.Store(0)
		fetchOK(t, withCallPath(ctx, "/ether/action/import"), "/throttled", "interactive.bin", 4, false)
		if peak.Load() != 0 {
			t.Fatal("interactive call requested ranges")
		}
	})
	for _, path := range []string{"/ignored", "/changed", "/wrong-range"} {
		t.Run(path[1:], func(t *testing.T) {
			fetchOK(t, job, path, path[1:]+".bin", 4, path != "/ignored")
			if _, ok := fellBack.Load(path); !ok {
				t.Fatal("incompatible range response did not trigger a full GET")
			}
		})
	}
	for _, tc := range []struct {
		path     string
		ranges   bool
		fallback bool
	}{
		{path: "/replicas", ranges: true},
		{path: "/replicas-different-tag", ranges: true, fallback: true},
		{path: "/replicas-different-length", ranges: true, fallback: true},
		{path: "/replicas-weak"},
	} {
		t.Run(tc.path[1:], func(t *testing.T) {
			fetchOK(t, job, tc.path, tc.path[1:]+".bin", 4, true)
			_, ranged := replicaRanges.Load(tc.path)
			_, fallback := replicaFallback.Load(tc.path)
			if ranged != tc.ranges || fallback != tc.fallback {
				t.Fatalf("requested replica ranges = %t, fell back to full GET = %t", ranged, fallback)
			}
		})
	}
	t.Run("resume-after-break-and-reopen", func(t *testing.T) {
		out, err := fetch(t, job, "/resume", "resume.bin", 4, true)
		if !errors.Is(err, io.ErrUnexpectedEOF) || out.Blob != "" {
			t.Fatalf("broken range published a blob or returned the wrong error: %+v, %v", out, err)
		}
		cache.Close()
		openCache()
		ns := cache.Namespace(libraryKey, blobcache.Quota{})
		if accounted, _ := ns.PartialStats(); accounted != int64(len(payload)) {
			t.Fatalf("reopened sparse partial accounted %d bytes, want %d", accounted, len(payload))
		}
		retry.Store(true)
		fetchOK(t, job, "/resume", "resume.bin", 2, true)
		if offset := resumedOffset.Load(); offset != 2*block || retriedFinishedRange.Load() {
			t.Fatalf("retry lost staged progress: offset=%d, retried finished range=%v", offset, retriedFinishedRange.Load())
		}
	})
}
