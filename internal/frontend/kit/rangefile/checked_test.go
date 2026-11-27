package rangefile

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testGap = 32 << 10

var errTestMissing = errors.New("missing article")

// The length is accurate, but an interior span cannot be read. Like a real
// source, the reader can return valid bytes and the failure in the same call.
type gapReader struct{}

func (gapReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= 2*testGap {
		clear(p)
		return len(p), nil
	}
	n := int(min(int64(len(p)), max(0, testGap-off)))
	clear(p[:n])
	if n < len(p) {
		return n, errTestMissing
	}
	return n, nil
}

func TestCheckedRangeInteriorReadFailure(t *testing.T) {
	const size = 3 * testGap
	var failures atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/unchecked" {
			Serve(w, r, "video.mkv", "source", size, time.Time{}, io.NewSectionReader(gapReader{}, 0, size))
			return
		}
		ServeChecked(w, r, "video.mkv", "source", size, time.Time{}, gapReader{}, func(offset int64, err error) {
			failures.Add(1)
			if offset != testGap || !errors.Is(err, errTestMissing) {
				t.Errorf("read failure at %d: %v", offset, err)
			}
		})
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	for _, tt := range []struct {
		name, path, method, ranges string
		status, bytes              int
		failure                    string
	}{
		{"old response ends normally on the wire", "/unchecked", "GET", "bytes=0-", 206, testGap, "unexpected EOF"},
		{"first read fails", "/", "GET", "bytes=32768-", 503, 0, ""},
		{"first read fails before multipart framing", "/", "GET", "bytes=32768-32770,65536-65538", 503, 0, ""},
		{"failure after data resets stream", "/", "GET", "bytes=0-", 206, testGap, "INTERNAL_ERROR"},
		{"range after gap", "/", "GET", "bytes=65536-", 206, testGap, ""},
		{"head never reads", "/", "HEAD", "bytes=32768-", 206, 0, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, server.URL+tt.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Range", tt.ranges)
			res, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if res.ProtoMajor != 2 || res.StatusCode != tt.status || len(body) != tt.bytes {
				t.Fatalf("%s status=%d bytes=%d err=%v", res.Proto, res.StatusCode, len(body), err)
			}
			if tt.failure == "" && err != nil || tt.failure != "" && (err == nil || !strings.Contains(err.Error(), tt.failure)) {
				t.Fatalf("body error = %v, want %q", err, tt.failure)
			}
			if tt.status == 503 && (res.Header.Get("Content-Range") != "" || res.Header.Get("ETag") != "") {
				t.Fatalf("read error published representation headers: %v", res.Header)
			}
		})
	}
	if got := failures.Load(); got != 3 {
		t.Fatalf("reported source failures = %d, want 3", got)
	}
}
