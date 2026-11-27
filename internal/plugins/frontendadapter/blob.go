package frontendadapter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
)

// BlobManager opens bytes only after rechecking current plugin and viewer access.
// The returned reader owns all underlying readers and is bounded by size.
type BlobManager interface {
	OpenFrontendBlob(context.Context, string, int64, []core.LibraryID, plugins.FrontendBlob) (io.ReadCloser, int64, error)
}

func (f *Frontend) writeBlob(w http.ResponseWriter, r *http.Request, auth authenticated, resp plugins.Response) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mgr, ok := f.mgr.(BlobManager)
	if !ok || (resp.Status != 0 && resp.Status != http.StatusOK) || len(resp.Body) != 0 {
		http.Error(w, "invalid blob response", http.StatusBadGateway)
		return
	}
	reader, size, err := mgr.OpenFrontendBlob(r.Context(), f.decl.PluginID, auth.viewer.ID, auth.libraries, *resp.Blob)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer reader.Close()
	if size < 0 {
		http.Error(w, "invalid blob size", http.StatusBadGateway)
		return
	}
	rangeValue := r.Header.Get("Range")
	if r.Method == http.MethodHead {
		rangeValue = ""
	}
	start, length, partial, err := blobRange(rangeValue, size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	cr := contextReader{ctx: r.Context(), r: reader}
	if start > 0 {
		if seeker, ok := reader.(io.Seeker); ok {
			_, err = seeker.Seek(start, io.SeekStart)
		} else {
			_, err = io.CopyN(io.Discard, cr, start)
		}
		if err != nil {
			http.Error(w, "blob unavailable", http.StatusBadGateway)
			return
		}
	}
	// Binary content is always a download. Plugin-controlled MIME/filenames must
	// not turn a cached file into executable same-origin content or inject headers.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="download"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Accept-Ranges", "bytes")
	// HEAD ignores Range, matching HTTP range semantics for GET.
	if r.Method == http.MethodHead {
		start, length, partial = 0, size, false
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, size))
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	w.WriteHeader(status)
	if _, err := io.CopyN(w, cr, length); err != nil {
		panic(http.ErrAbortHandler)
	}
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// blobRange deliberately supports only a single range; multipart amplification
// and malformed/unsatisfiable ranges are rejected before any bytes are emitted.
func blobRange(value string, size int64) (start, length int64, partial bool, err error) {
	if value == "" {
		return 0, size, false, nil
	}
	bad := func() (int64, int64, bool, error) { return 0, 0, false, fmt.Errorf("invalid range") }
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") || size == 0 {
		return bad()
	}
	a, b, ok := strings.Cut(strings.TrimPrefix(value, "bytes="), "-")
	if !ok {
		return bad()
	}
	number := func(s string) (int64, error) {
		if s == "" {
			return 0, fmt.Errorf("empty")
		}
		for _, c := range s {
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("invalid")
			}
		}
		return strconv.ParseInt(s, 10, 64)
	}
	if a == "" {
		n, e := number(b)
		if e != nil || n <= 0 {
			return bad()
		}
		n = min(n, size)
		return size - n, n, true, nil
	}
	start, e := number(a)
	if e != nil || start >= size {
		return bad()
	}
	end := size - 1
	if b != "" {
		end, e = number(b)
		if e != nil || end < start {
			return bad()
		}
		end = min(end, size-1)
	}
	return start, end - start + 1, true, nil
}
