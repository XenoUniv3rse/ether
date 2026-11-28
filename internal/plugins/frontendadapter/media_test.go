package frontendadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestMediaHandlerSurfacesArchiveUnstreamablePlainly(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	issuer := NewTicketIssuer()
	handler := NewMediaHandler(MediaDeps{
		Tickets: issuer,
		Log:     logger,
		Viewer: func(_ context.Context, id int64) (*core.Viewer, error) {
			return viewerWithLibraries(id, "Reader", core.LibraryMovies), nil
		},
		Open: func(_ context.Context, _ *core.Viewer, _ string, _ []string) (*frontend.KindMedia, error) {
			return nil, fmt.Errorf("open container: %w", core.ErrArchiveUnstreamable)
		},
	})

	token, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:item-1", Adapter: "feed"})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+token, nil))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if got := rec.Header().Get("X-Ether-Media-Error"); got != "archive_unstreamable" {
		t.Fatalf("X-Ether-Media-Error = %q, want archive_unstreamable", got)
	}
	if !strings.Contains(rec.Body.String(), "This archive format or compression method cannot be streamed.") {
		t.Fatalf("unexpected body %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "temporarily unavailable") {
		t.Fatalf("body still says temporarily unavailable: %q", rec.Body.String())
	}
	if !strings.Contains(logBuf.String(), "cannot be streamed") {
		t.Fatalf("expected plain log entry, got %q", logBuf.String())
	}
}

func TestMediaHandlerSurfacesPermanentRejectionNotTemporarilyUnavailable(t *testing.T) {
	issuer := NewTicketIssuer()
	handler := NewMediaHandler(MediaDeps{
		Tickets: issuer,
		Viewer: func(_ context.Context, id int64) (*core.Viewer, error) {
			return viewerWithLibraries(id, "Reader", core.LibraryMovies), nil
		},
		Open: func(_ context.Context, _ *core.Viewer, _ string, _ []string) (*frontend.KindMedia, error) {
			return nil, fmt.Errorf("missing volume: %w", core.ErrReleaseRejected)
		},
	})

	token, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:item-1", Adapter: "feed"})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+token, nil))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Ether-Media-Error"); got != "source_unplayable" {
		t.Fatalf("X-Ether-Media-Error = %q, want source_unplayable", got)
	}
	if strings.Contains(rec.Body.String(), "temporarily unavailable") {
		t.Fatalf("body still says temporarily unavailable: %q", rec.Body.String())
	}
}

func TestMediaHandlerDefaultsEmptyMIMEToOctetStreamNeverVideo(t *testing.T) {
	issuer := NewTicketIssuer()
	payload := append([]byte{0x00, 0x00, 0x00, 0x18}, []byte("ftypisom-opaque-binary")...)
	handler := NewMediaHandler(MediaDeps{
		Tickets: issuer,
		Viewer: func(_ context.Context, id int64) (*core.Viewer, error) {
			return viewerWithLibraries(id, "Reader", core.LibraryMovies), nil
		},
		Open: func(_ context.Context, _ *core.Viewer, _ string, _ []string) (*frontend.KindMedia, error) {
			return &frontend.KindMedia{
				ReaderAt: bytes.NewReader(payload),
				Closer:   io.NopCloser(nil),
				Size:     int64(len(payload)),
				MIME:     "",
				Name:     "payload.bin",
			}, nil
		},
	})

	token, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:bin-1", Adapter: "feed"})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+token, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", got)
	}
}

func TestMediaHandlerPartScopedTicketsAndQuery(t *testing.T) {
	issuer := NewTicketIssuer()
	var wholeOpens int
	var partOpens []int

	handler := NewMediaHandler(MediaDeps{
		Tickets: issuer,
		Viewer: func(_ context.Context, id int64) (*core.Viewer, error) {
			return viewerWithLibraries(id, "Reader", core.LibraryMovies), nil
		},
		Open: func(_ context.Context, _ *core.Viewer, _ string, _ []string) (*frontend.KindMedia, error) {
			wholeOpens++
			data := []byte("whole-item")
			return &frontend.KindMedia{
				ReaderAt: bytes.NewReader(data),
				Closer:   io.NopCloser(nil),
				Size:     int64(len(data)),
				MIME:     "application/octet-stream",
				Name:     "whole.bin",
			}, nil
		},
		OpenPart: func(_ context.Context, _ *core.Viewer, _ string, partIndex int, _ []string) (*frontend.KindMedia, error) {
			partOpens = append(partOpens, partIndex)
			data := []byte(fmt.Sprintf("part-%d", partIndex))
			return &frontend.KindMedia{
				ReaderAt: bytes.NewReader(data),
				Closer:   io.NopCloser(nil),
				Size:     int64(len(data)),
				MIME:     "application/octet-stream",
				Name:     fmt.Sprintf("part-%d.bin", partIndex),
			}, nil
		},
	})

	unpinned, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:multi", Adapter: "feed"})
	if err != nil {
		t.Fatal(err)
	}
	// Whole-item open when no part is specified.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+unpinned, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "whole-item" || wholeOpens != 1 {
		t.Fatalf("whole open: code=%d body=%q opens=%d", rec.Code, rec.Body.String(), wholeOpens)
	}
	// ?part=2 on unpinned ticket opens part 2.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+unpinned+"?part=2", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "part-2" {
		t.Fatalf("query part=2: code=%d body=%q", rec.Code, rec.Body.String())
	}
	// Malformed or negative ?part returns 400 invalid_part.
	for _, bad := range []string{"-1", "abc"} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+unpinned+"?part="+bad, nil))
		if rec.Code != http.StatusBadRequest || rec.Header().Get("X-Ether-Media-Error") != "invalid_part" {
			t.Fatalf("query part=%s: code=%d hdr=%q body=%s", bad, rec.Code, rec.Header().Get("X-Ether-Media-Error"), rec.Body.String())
		}
	}

	// Part-scoped ticket serves only its bound part.
	part1 := 1
	pinned, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:multi", Adapter: "feed", PartIndex: &part1})
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+pinned, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "part-1" {
		t.Fatalf("pinned ticket: code=%d body=%q", rec.Code, rec.Body.String())
	}
	// Conflicting ?part=0 on a ticket pinned to part 1 is rejected with 404 ticket_invalid.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+pinned+"?part=0", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("X-Ether-Media-Error") != "ticket_invalid" {
		t.Fatalf("conflicting part query: code=%d hdr=%q body=%s", rec.Code, rec.Header().Get("X-Ether-Media-Error"), rec.Body.String())
	}
}

func TestMediaHandlerPreparingReturns503RetryAfterWhileNoSourceStays409(t *testing.T) {
	for _, tc := range []struct {
		name       string
		openErr    error
		wantStatus int
		wantCode   string
		wantRetry  string
	}{
		{
			name:       "preparing with explicit retryAfter",
			openErr:    &core.PreparingError{RetryAfter: 7 * time.Second},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "preparing",
			wantRetry:  "7",
		},
		{
			name:       "preparing sentinel defaults to 5s",
			openErr:    core.ErrPreparing,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "preparing",
			wantRetry:  "5",
		},
		{
			name:       "preparing wrapping queued download stays 503 preparing",
			openErr:    &core.PreparingError{RetryAfter: 5 * time.Second, Err: &core.UncachedError{Err: core.ErrDownloadQueued}},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "preparing",
			wantRetry:  "5",
		},
		{
			name:       "real no_source stays 409",
			openErr:    core.ErrNotAvailable,
			wantStatus: http.StatusConflict,
			wantCode:   "no_source",
			wantRetry:  "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer := NewTicketIssuer()
			handler := NewMediaHandler(MediaDeps{
				Tickets: issuer,
				Viewer: func(_ context.Context, id int64) (*core.Viewer, error) {
					return viewerWithLibraries(id, "Reader", core.LibraryMovies), nil
				},
				Open: func(_ context.Context, _ *core.Viewer, _ string, _ []string) (*frontend.KindMedia, error) {
					return nil, tc.openErr
				},
			})
			token, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:item", Adapter: "feed"})
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+token, nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := rec.Header().Get("Retry-After"); got != tc.wantRetry {
				t.Fatalf("Retry-After = %q, want %q", got, tc.wantRetry)
			}
			if got := rec.Header().Get("X-Ether-Media-Error"); got != tc.wantCode {
				t.Fatalf("X-Ether-Media-Error = %q, want %q", got, tc.wantCode)
			}
			var wire struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil || wire.Error.Code != tc.wantCode {
				t.Fatalf("json body = %s, err = %v, want code %q", rec.Body.String(), err, tc.wantCode)
			}
		})
	}
}

func TestMediaHandlerSourceScopedTickets(t *testing.T) {
	issuer := NewTicketIssuer()
	var (
		wholeOpens  int
		sourceCalls []struct {
			part   *int
			source string
		}
	)
	handler := NewMediaHandler(MediaDeps{
		Tickets: issuer,
		Viewer: func(_ context.Context, id int64) (*core.Viewer, error) {
			return viewerWithLibraries(id, "Reader", core.LibraryMovies), nil
		},
		Open: func(_ context.Context, _ *core.Viewer, _ string, _ []string) (*frontend.KindMedia, error) {
			wholeOpens++
			data := []byte("default-source")
			return &frontend.KindMedia{
				ReaderAt: bytes.NewReader(data),
				Closer:   io.NopCloser(nil),
				Size:     int64(len(data)),
				MIME:     "application/octet-stream",
				Name:     "default.bin",
			}, nil
		},
		OpenSource: func(_ context.Context, _ *core.Viewer, _ string, partIndex *int, source string, fallback bool, _ []string) (*frontend.KindMedia, error) {
			var pCopy *int
			if partIndex != nil {
				v := *partIndex
				pCopy = &v
			}
			sourceCalls = append(sourceCalls, struct {
				part   *int
				source string
			}{part: pCopy, source: source})
			if source == "missing" {
				return nil, core.ErrNotAvailable
			}
			body := "src:" + source
			if partIndex != nil {
				body += fmt.Sprintf("#part:%d", *partIndex)
			}
			data := []byte(body)
			return &frontend.KindMedia{
				ReaderAt: bytes.NewReader(data),
				Closer:   io.NopCloser(nil),
				Size:     int64(len(data)),
				MIME:     "application/octet-stream",
				Name:     source + ".bin",
			}, nil
		},
	})

	// Blank or control-char source grants are refused at Issue time.
	if _, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:book", Adapter: "feed", Source: "   "}); err == nil {
		t.Fatal("blank source accepted")
	}
	if _, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:book", Adapter: "feed", Source: "bad\nid"}); err == nil {
		t.Fatal("control char source accepted")
	}

	// Source-scoped ticket fails Validate / ValidatePart / wrong source, succeeds ValidateScope.
	srcTok, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:book", Adapter: "feed", Source: "pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Validate(srcTok, 7, "plug:kind:book", "feed"); err != ErrTicketInvalid {
		t.Fatalf("Validate on source-scoped ticket = %v, want ErrTicketInvalid", err)
	}
	if _, err := issuer.ValidateScope(srcTok, 7, "plug:kind:book", "feed", nil, "epub"); err != ErrTicketInvalid {
		t.Fatalf("ValidateScope wrong source = %v, want ErrTicketInvalid", err)
	}
	if _, err := issuer.ValidateScope(srcTok, 7, "plug:kind:book", "feed", nil, "pdf"); err != nil {
		t.Fatalf("ValidateScope exact source = %v", err)
	}

	// Serving a source-scoped ticket dispatches to OpenSource; a ?source= query param cannot override it.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+srcTok+"?source=epub", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "src:pdf" {
		t.Fatalf("source-scoped serve: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// An unscoped ticket ignores ?source= query parameters and opens the default source.
	unscopedTok, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:book", Adapter: "feed"})
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+unscopedTok+"?source=pdf", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "default-source" || wholeOpens != 1 {
		t.Fatalf("unscoped with ?source query: code=%d body=%q wholeOpens=%d", rec.Code, rec.Body.String(), wholeOpens)
	}

	// Ticket carrying both partIndex and source passes both, and rejects conflicting ?part=.
	part2 := 2
	bothTok, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:book", Adapter: "feed", PartIndex: &part2, Source: "hd"})
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+bothTok, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "src:hd#part:2" {
		t.Fatalf("part+source serve: code=%d body=%q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+bothTok+"?part=0", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("X-Ether-Media-Error") != "ticket_invalid" {
		t.Fatalf("conflicting ?part on part+source ticket: code=%d hdr=%q", rec.Code, rec.Header().Get("X-Ether-Media-Error"))
	}

	// Absent source returns 409 no_source.
	missTok, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:book", Adapter: "feed", Source: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+missTok, nil))
	if rec.Code != http.StatusConflict || rec.Header().Get("X-Ether-Media-Error") != "no_source" {
		t.Fatalf("missing source serve: code=%d hdr=%q", rec.Code, rec.Header().Get("X-Ether-Media-Error"))
	}
}
