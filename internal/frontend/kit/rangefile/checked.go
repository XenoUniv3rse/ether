package rangefile

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// ServeChecked serves an immutable remote representation. ServeContent handles
// range and conditional request semantics but discards its copy error. Observe
// the source reads here and delay committing successful headers until data is
// available. A multipart response may write its first boundary before reading.
func ServeChecked(w http.ResponseWriter, r *http.Request, name, identity string, size int64, modTime time.Time, src io.ReaderAt, onFailure func(int64, error)) {
	if w.Header().Get("Content-Type") == "" {
		// Callers supply the type when known. Avoid sniffing offset zero on a
		// resumed request, since it may be unrelated to the requested range.
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	reader := &checkedReader{src: src, ctx: ctx}
	out := &checkedResponse{ResponseWriter: w, reader: reader, hasBody: r.Method != http.MethodHead && size > 0}
	if policy, _ := core.RangeResponseFrom(ctx); policy.Serve != nil {
		if identity != "" && w.Header().Get("ETag") == "" {
			w.Header().Set("ETag", StrongETag(identity, size, modTime))
		}
		if policy.MIME != nil {
			if contentType := policy.MIME(name); contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
		}
		_ = policy.Serve(out, r, core.RangeRepresentation{
			Size: size, ETag: w.Header().Get("ETag"), ModTime: modTime, ContentType: w.Header().Get("Content-Type"),
			ServeBody: func(status int, start, length int64) error {
				if length == 0 {
					out.hasBody = false
				}
				out.WriteHeader(status)
				if r.Method == http.MethodHead || length == 0 {
					return nil
				}
				_, err := io.Copy(out, io.NewSectionReader(reader, start, length))
				return err
			},
		})
	} else {
		Serve(out, r, name, identity, size, modTime, io.NewSectionReader(reader, 0, size))
	}
	// ServeContent's multipart writer may still be in a source read after
	// a downstream write fails. Cancel and join that read before returning
	// ownership of the source to the caller.
	cancel()
	reader.mu.Lock()
	failure := reader.failure
	reader.mu.Unlock()
	if failure == nil || out.writeFailed {
		return
	}
	if onFailure != nil && r.Context().Err() == nil {
		onFailure(failure.offset, failure.err)
	}
	if out.committed {
		panic(http.ErrAbortHandler)
	}
	h := w.Header()
	for _, name := range []string{"Content-Length", "Content-Range", "Content-Type", "Content-Disposition", "ETag", "Last-Modified", "Accept-Ranges"} {
		h.Del(name)
	}
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
}

type checkedFailure struct {
	offset int64
	err    error
}

type checkedReader struct {
	src     io.ReaderAt
	ctx     context.Context
	hasData atomic.Bool // ServeContent reads multipart bodies from a pipe producer.
	mu      sync.Mutex
	failure *checkedFailure
}

func (r *checkedReader) ReadAt(p []byte, off int64) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// The producer can reach this point after ServeContent releases its
	// pipe. It must never touch the source after cancellation.
	if err = r.ctx.Err(); err == nil {
		if src, ok := r.src.(core.ContextReaderAt); ok {
			n, err = src.ReadAtContext(r.ctx, p, off)
		} else {
			n, err = r.src.ReadAt(p, off)
		}
	}
	if n > 0 {
		r.hasData.Store(true)
	}
	if n == len(p) {
		return n, nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	if r.failure == nil {
		r.failure = &checkedFailure{offset: off + int64(n), err: err}
	}
	return n, err
}

type checkedResponse struct {
	http.ResponseWriter
	reader      *checkedReader
	prefix      bytes.Buffer
	status      int
	hasBody     bool
	committed   bool
	writeFailed bool
}

func (w *checkedResponse) WriteHeader(status int) {
	w.status = status
	if !w.hasBody || (status != http.StatusOK && status != http.StatusPartialContent) {
		w.commit()
	}
}

func (w *checkedResponse) commit() {
	w.ResponseWriter.WriteHeader(w.status)
	w.committed = true
}

func (w *checkedResponse) Write(p []byte) (int, error) {
	if !w.committed {
		if !w.reader.hasData.Load() {
			return w.prefix.Write(p)
		}
		w.commit()
		if w.prefix.Len() != 0 {
			if _, err := w.ResponseWriter.Write(w.prefix.Bytes()); err != nil {
				w.writeFailed = true
				return 0, err
			}
			w.prefix.Reset()
		}
	}
	n, err := w.ResponseWriter.Write(p)
	w.writeFailed = err != nil
	return n, err
}
