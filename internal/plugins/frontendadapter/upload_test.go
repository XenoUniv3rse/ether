package frontendadapter

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
)

type uploadTestManager struct {
	*fakeManager
	store *blobcache.DurableStore
	meta  plugins.FrontendAdapterMeta
}

func (m *uploadTestManager) BeginAdapterUpload(_ context.Context, id, _ string, subpath string, size int64) (*blobcache.Upload, int64, error) {
	limit := m.meta.UploadLimit(subpath)
	if size > limit {
		return nil, 0, blobcache.ErrTooLarge
	}
	if size <= 0 {
		size = limit
	}
	upload, err := m.store.BeginUpload(id, size)
	return upload, limit, err
}

func checkUploadDigests(t *testing.T, blob plugins.FrontendUploadBlob, data []byte) {
	t.Helper()
	if blob.SHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) ||
		blob.SHA1 != fmt.Sprintf("%x", sha1.Sum(data)) ||
		blob.MD5 != fmt.Sprintf("%x", md5.Sum(data)) ||
		blob.CRC32 != fmt.Sprintf("%08x", crc32.ChecksumIEEE(data)) {
		t.Fatalf("unexpected upload digests: %+v", blob)
	}
}

// The body is generated in bounded reads and checks that bytes are reaching
// disk while the request is still arriving.
type generatedUploadBody struct {
	t         *testing.T
	dir       string
	remaining int64
	read      int64
}

func (b *generatedUploadBody) Read(p []byte) (int, error) {
	b.t.Helper()
	if len(p) > 128<<10 {
		b.t.Fatalf("upload requested an unbounded read buffer of %d bytes", len(p))
	}
	if b.read == 1<<20 {
		paths, err := filepath.Glob(filepath.Join(b.dir, "uploads", "*"))
		if err != nil || len(paths) != 1 {
			b.t.Fatalf("upload has not started staging on disk: %v", err)
		}
		if info, err := os.Stat(paths[0]); err != nil || info.Size() != b.read {
			b.t.Fatalf("staged file did not receive bytes during the request: %v", err)
		}
	}
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), b.remaining)
	clear(p[:n])
	b.remaining -= n
	b.read += n
	return int(n), nil
}

func TestAdapterUploadLifecycle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	storageDir := filepath.Join(root, "storage")
	store, err := blobcache.OpenDurableStore(storageDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	var staged, saved blobcache.Ref
	mgr := &uploadTestManager{fakeManager: &fakeManager{}, store: store}
	mgr.handler = func(req plugins.Request) (plugins.Response, io.ReadCloser, error) {
		if req.Upload == nil || req.BodyBase64 != "" || len(req.Body) != 0 {
			t.Fatal("declared route did not deliver upload metadata in place of inline bytes")
		}
		if strings.HasSuffix(req.Path, "/multipart") {
			if len(req.Upload.Fields["slot"]) != 2 || req.Upload.Fields["slot"][1] != "two" || len(req.Upload.Files) != 1 {
				t.Fatalf("unexpected multipart metadata: %+v", req.Upload)
			}
			file := req.Upload.Files[0]
			if file.Field != "save" || file.Filename != "state.bin" || file.Size != 4 || file.ContentType != "application/octet-stream" {
				t.Fatalf("unexpected file metadata: %+v", file)
			}
			checkUploadDigests(t, file, []byte("save"))
			staged = blobcache.Ref(file.Ref)
		} else {
			if req.Upload.Blob == nil || req.Upload.Blob.Size != 20<<20 || req.Upload.Blob.ContentType != "application/octet-stream" {
				t.Fatalf("unexpected raw upload: %+v", req.Upload)
			}
			checkUploadDigests(t, *req.Upload.Blob, make([]byte, 20<<20))
			staged = blobcache.Ref(req.Upload.Blob.Ref)
			if src, err := store.Open("other.plugin", staged); !errors.Is(err, blobcache.ErrNotFound) {
				if src != nil {
					src.Close()
				}
				t.Fatalf("staged upload accessible across plugins: %v", err)
			}
			saved, err = store.Promote(req.PluginID, staged)
			if err != nil {
				t.Fatal(err)
			}
			paths, _ := filepath.Glob(filepath.Join(storageDir, "uploads", "*"))
			savedPaths, _ := filepath.Glob(filepath.Join(storageDir, "saved", "*", "*"))
			if len(paths) != 1 || len(savedPaths) != 1 {
				t.Fatal("expected one staged file and one saved file")
			}
			original, originalErr := os.Stat(paths[0])
			promoted, promotedErr := os.Stat(savedPaths[0])
			if originalErr != nil || promotedErr != nil || !os.SameFile(original, promoted) {
				t.Fatal("promotion must link the staged file")
			}
		}
		return plugins.Response{Status: http.StatusNoContent}, io.NopCloser(strings.NewReader("")), nil
	}
	decl := testDecl(core.LibraryMovies)
	decl.Meta.UploadPaths = []plugins.FrontendUploadPath{{Path: "/upload/*", MaxBytes: 24 << 20}, {Path: "/upload/limited", MaxBytes: 8}}
	mgr.meta = decl.Meta
	f, err := New(decl, mgr)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := f.Mount(ctx, testHost(map[string]*core.Viewer{"tok1": viewerWithLibraries(7, "Alice", core.LibraryMovies)}))
	if err != nil {
		t.Fatal(err)
	}
	send := func(path, ct string, body io.Reader, length int64, want int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/opds/"+path, body)
		req.ContentLength = length
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Authorization", "Bearer tok1")
		resp := httptest.NewRecorder()
		inst.Handler().ServeHTTP(resp, req)
		if resp.Code != want {
			t.Fatalf("%s: got %d, want %d", path, resp.Code, want)
		}
	}
	send("upload/raw", "application/octet-stream", &generatedUploadBody{t: t, dir: storageDir, remaining: 20 << 20}, -1, http.StatusNoContent)
	if src, err := store.Open(decl.PluginID, staged); !errors.Is(err, blobcache.ErrNotFound) {
		if src != nil {
			src.Close()
		}
		t.Fatalf("staged ref outlived request: %v", err)
	}
	send("upload/limited", "application/octet-stream", strings.NewReader("012345678"), -1, http.StatusRequestEntityTooLarge)
	send("upload/limited", "application/octet-stream", strings.NewReader("012345678"), 9, http.StatusRequestEntityTooLarge)

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("slot", "one"); err != nil {
		t.Fatal(err)
	}
	if err := form.WriteField("slot", "two"); err != nil {
		t.Fatal(err)
	}
	part, err := form.CreateFormFile("save", "state.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "save"); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	send("upload/multipart", form.FormDataContentType(), &body, -1, http.StatusNoContent)
	if len(mgr.calls) != 2 {
		t.Fatalf("rejected request reached plugin: %d successful calls", len(mgr.calls))
	}

	// Exercise independent eviction and cold-start quota accounting without
	// writing a gigabyte of fixture data.
	cache, err := blobcache.Open(filepath.Join(root, "cache"), blobcache.Options{SharedCap: 6, SweepPeriod: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ns := cache.Namespace("test", blobcache.Quota{MaxBytes: 6})
	evicted, err := ns.Put(ctx, "", strings.NewReader("abcdef"), 6)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ns.Put(ctx, "", strings.NewReader("ghijkl"), 6); err != nil {
		t.Fatal(err)
	}
	cache.SweepAll()
	if src, err := ns.Open(evicted); !errors.Is(err, blobcache.ErrNotFound) {
		if src != nil {
			src.Close()
		}
		t.Fatalf("ordinary cache did not evict: %v", err)
	}

	store, err = blobcache.OpenDurableStore(storageDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	src, err := store.Open(decl.PluginID, saved)
	if err != nil {
		t.Fatal(err)
	}
	if src.Size() != 20<<20 {
		t.Fatalf("saved size = %d", src.Size())
	}
	src.Close()
	savedPaths, _ := filepath.Glob(filepath.Join(storageDir, "saved", "*", "*"))
	if len(savedPaths) != 1 {
		t.Fatal("expected one durable file")
	}
	if err := os.Truncate(savedPaths[0], blobcache.DurableMaxBytes); err != nil {
		t.Fatal(err)
	}
	u, err := store.BeginUpload(decl.PluginID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	ref, _, err := u.Stage(ctx, "", "", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Promote(decl.PluginID, ref); !errors.Is(err, blobcache.ErrQuotaFull) {
		t.Fatalf("promotion over durable quota returned %v", err)
	}
	if deleted, err := store.Delete(decl.PluginID, saved); !deleted || err != nil {
		t.Fatalf("delete saved blob: %v", err)
	}
	if _, err := store.Promote(decl.PluginID, ref); err != nil {
		t.Fatalf("delete did not free quota: %v", err)
	}
	if err := store.Purge(decl.PluginID); err != nil {
		t.Fatal(err)
	}
}
