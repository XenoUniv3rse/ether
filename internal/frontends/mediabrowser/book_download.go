package mediabrowser

import (
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/lru"
	"github.com/braedonsaunders/ether/internal/frontend/kit/mediasafety"
	"github.com/braedonsaunders/ether/internal/frontend/kit/rangefile"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reading"
	"github.com/braedonsaunders/ether/internal/logredact"
)

// bookReadable reports whether detail represents a leaf plugin book that this
// instance can serve through /Items/{id}/Download and /Items/{id}/File.
func (i *instance) bookReadable(detail frontend.ItemDetail) bool {
	if i == nil || i.host.KindStreams == nil || !detail.Title.Key.IsPluginKey() {
		return false
	}
	if !mbIsBookItem(detail.Title) || mbIsFolder(detail.Title) || detail.Unavailable != nil {
		return false
	}
	if detail.CanRead != nil && !*detail.CanRead {
		return false
	}
	return true
}

type bookFileKey struct {
	viewer int64
	title  frontend.RatingKey
}

type bookFileMetadata struct {
	filename, mimeType, identity string
	size                         int64
	modified, expires            time.Time
}

// bookFileCache only remembers metadata from an actual read request.
type bookFileCache struct {
	mu    sync.Mutex
	files *lru.Cache[bookFileKey, bookFileMetadata]
}

// serveBookFile serves readable plugin books (EPUB, PDF, CBZ) via KindStreams.
func (i *instance) serveBookFile(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, detail frontend.ItemDetail, attachment bool) {
	if i.host.KindStreams == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	key := detail.Title.Key
	if !i.bookReadable(detail) {
		http.NotFound(w, r)
		return
	}
	cacheKey := bookFileKey{viewer: viewer.ID, title: key}
	if r.Method == http.MethodHead {
		i.bookFiles.mu.Lock()
		var file bookFileMetadata
		if i.bookFiles.files != nil {
			file, _ = i.bookFiles.files.Get(cacheKey)
		}
		i.bookFiles.mu.Unlock()
		if time.Now().Before(file.expires) {
			setBookFileHeaders(w, file, attachment)
			rangefile.Serve(w, r, file.filename, file.identity, file.size, file.modified, io.NewSectionReader(nil, 0, file.size))
		} else {
			mediasafety.SetHeaders(w)
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
		}
		return
	}
	var (
		dlPolicy frontend.DownloadLimitPolicy
		reserved bool
		served   bool
	)
	if attachment {
		if p, ok := i.host.ViewerPolicy.(frontend.DownloadLimitPolicy); ok {
			dlPolicy = p
			dec := p.AdmitDownload(r.Context(), viewer, key, 0)
			if !dec.Allowed {
				writeDownloadLimit(w, dec)
				return
			}
			reserved = dec.Reserved
			defer func() {
				if reserved && !served {
					dlPolicy.ReleaseDownloadReservation(context.Background(), viewer, key)
				}
			}()
		}
	}
	media, err := i.host.KindStreams.OpenKind(r.Context(), viewer, key, reading.AcceptPatterns)
	if err != nil {
		if errors.Is(err, frontend.ErrArchiveUnstreamable) {
			http.Error(w, "Unsupported book format", http.StatusUnprocessableEntity)
			return
		}
		streamError(i.protocol, w, err, r.Header.Get("Accept-Language"))
		return
	}
	if media == nil {
		streamError(i.protocol, w, frontend.ErrNotAvailable, r.Header.Get("Accept-Language"))
		return
	}
	if media.Closer != nil {
		defer media.Closer.Close()
	}
	if media.ReaderAt == nil || media.Size < 0 {
		streamError(i.protocol, w, frontend.ErrNotAvailable, r.Header.Get("Accept-Language"))
		return
	}
	format, mimeType, ok := reading.DetectFormat(media, detail.Title)
	if !ok {
		http.Error(w, "Unsupported book format", http.StatusUnprocessableEntity)
		return
	}
	if dlPolicy != nil && media.Size > 0 {
		if rec := dlPolicy.ReconcileDownloadBytes(r.Context(), viewer, key, media.Size); !rec.Allowed {
			writeDownloadLimit(w, rec)
			return
		}
	}
	served = true
	filename := reading.Filename(media.Name, detail.Title.Title, format)
	file := bookFileMetadata{
		filename: filename, mimeType: mimeType, size: media.Size, expires: time.Now().Add(5 * time.Minute),
		identity: cmp.Or(media.Release.ID, "book:"+key.String()+":"+filename), modified: media.Release.PubDate,
	}
	if file.modified.IsZero() {
		file.modified = detail.Title.UpdatedAt
	}
	i.bookFiles.mu.Lock()
	if i.bookFiles.files == nil {
		i.bookFiles.files = lru.New(lru.Options[bookFileKey, bookFileMetadata]{MaxEntries: 512})
	}
	i.bookFiles.files.Add(cacheKey, file)
	i.bookFiles.mu.Unlock()
	setBookFileHeaders(w, file, attachment)
	r = r.WithContext(frontend.WithDownloadRange(r.Context()))
	rangefile.ServeChecked(w, r, filename, file.identity, file.size, file.modified, media.ReaderAt, func(offset int64, cause error) {
		slog.Warn("book source read failed", "key", key, "offset", offset, "cause", logredact.String(cause.Error()))
		i.bookFiles.mu.Lock()
		if cached, _ := i.bookFiles.files.Peek(cacheKey); cached == file {
			i.bookFiles.files.Remove(cacheKey)
		}
		i.bookFiles.mu.Unlock()
	})
}

func setBookFileHeaders(w http.ResponseWriter, file bookFileMetadata, attachment bool) {
	dispType := "inline"
	if attachment {
		dispType = "attachment"
	}
	w.Header().Set("Content-Type", file.mimeType)
	if disp := mime.FormatMediaType(dispType, map[string]string{"filename": file.filename}); disp != "" {
		w.Header().Set("Content-Disposition", disp)
	}
	mediasafety.SetHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
}
