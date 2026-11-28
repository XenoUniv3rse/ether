package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"testing/iotest"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// A 15 GiB disc should reach body streaming with the appropriate download
// grant and available pool capacity. A sentinel error avoids writing the file.
func TestDiscDownloadPoolAdmission(t *testing.T) {
	const disc int64 = 15 << 30
	const capacity int64 = 24 << 30
	env := newTestEnv(t)
	ctx := withCallPath(context.Background(), "/ether/job/import")
	rec := installContent(t, env)
	rec.Manifest.Ether.Permissions.Network = []string{"data.example.test"}
	var err error
	rec.ManifestRaw, err = json.Marshal(rec.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = env.m.store.upsert(ctx, rec); err != nil {
		t.Fatal(err)
	}
	library := core.PluginLibraryKey(rec.ID, "recording")
	var grant, reserved int64
	env.m.deps.ReadRuntimeConfig = func() config.Plugins {
		return config.Plugins{RuntimeLimits: config.PluginRuntimeLimits{FetchBytes: grant}}
	}
	blobs, err := blobcache.Open(t.TempDir(), blobcache.Options{Unified: true, SweepPeriod: -1, PoolCapacity: func() int64 { return capacity }, MakeFileRoom: func(_, _, additional int64) (int64, error) { reserved = max(reserved, additional); return 0, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.Close()
	ns := blobs.Namespace(library, blobcache.Quota{})
	put, open, del := blobs.HostBackends(nil)
	env.m.SetKindBackends(KindBackends{
		CachePut: put, CacheOpen: open, CacheDelete: del, CachePartial: blobs.HostPartialBackend(nil),
		CacheLimit: func(_, _ string) int64 { return ns.Quota().MaxBytes },
	})
	stop := errors.New("test reached disc body")
	env.m.client = &http.Client{Transport: dataRoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: disc, Header: http.Header{"Etag": {`"disc"`}}, Body: io.NopCloser(iotest.ErrReader(stop)), Request: r}, nil
	})}
	for _, resume := range []bool{false, true} {
		for _, tc := range []struct {
			name                string
			grant, libraryLimit int64
			want                error
		}{
			{"default download grant", 0, 0, errDataExceedsSizeLimit},
			{"explicit library cap", capacity, 3 << 30, ErrQuotaExceeded},
			{"inherit media pool", capacity, 0, stop},
		} {
			t.Run(fmt.Sprintf("%s/resume=%v", tc.name, resume), func(t *testing.T) {
				grant, reserved = tc.grant, 0
				if err := ns.SetOwnerQuota(blobcache.Quota{MaxBytes: tc.libraryLimit}); err != nil {
					t.Fatal(err)
				}
				args, err := json.Marshal([]any{map[string]any{"url": "https://data.example.test/disc.iso", "libraryKey": library, "name": "disc.iso", "resume": resume}})
				if err != nil {
					t.Fatal(err)
				}
				_, err = CallDataHost(ctx, env.m.HostFor(rec.ID), "http.fetchToCache", args)
				if !errors.Is(err, tc.want) {
					t.Fatalf("host error = %v, want %v", err, tc.want)
				}
				if tc.want == stop && reserved != disc {
					t.Fatalf("reserved %d bytes before reading, want %d", reserved, disc)
				}
				if tc.want != stop && reserved != 0 {
					t.Fatalf("reserved %d bytes after rejected grant", reserved)
				}
				if _, ok := ns.LookupKey("disc.iso"); ok {
					t.Fatal("failed body was published")
				}
			})
		}
	}
}
