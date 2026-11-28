package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/kindstream"
)

func TestParallelRangeStreamSharesFetchSlots(t *testing.T) {
	payload := make([]byte, 5<<20)
	for i := range payload {
		payload[i] = byte(i ^ (i >> 13))
	}
	var active, peak atomic.Int32
	var changed, stall atomic.Bool
	started := make(chan struct{}, 6)
	var conns sync.Map
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var lo, hi int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &lo, &hi); err != nil || lo < 0 || hi >= len(payload) || hi < lo {
			http.Error(w, "range", 416)
			return
		}
		if !changed.Load() {
			w.Header().Set("ETag", `"v1"`)
		} else {
			w.Header().Set("ETag", `"v2"`)
		}
		if r.Header.Get("If-Range") != "" {
			if _, shared := conns.LoadOrStore(r.RemoteAddr, true); shared {
				t.Error("concurrent requests shared one upstream HTTP/2 connection")
			}
			defer conns.Delete(r.RemoteAddr)
			n := active.Add(1)
			defer active.Add(-1)
			for prev := peak.Load(); n > prev && !peak.CompareAndSwap(prev, n); prev = peak.Load() {
			}
			if stall.Load() {
				started <- struct{}{}
				<-r.Context().Done()
				return
			}
			select {
			case <-time.After(8 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", lo, hi, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[lo : hi+1])
	}))
	origin.EnableHTTP2 = true
	origin.StartTLS()
	defer origin.Close()
	m := &Manager{fetchHosts: newFetchHostLimiter(3)}
	ctx := context.Background()
	// Keep one of the origin's shared fetch slots busy throughout playback.
	release, err := m.fetchHosts.Acquire(ctx, origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	src, err := kindstream.Open(ctx, core.PlayableSource{Pattern: "range", URL: origin.URL, Connections: 6, MIME: "application/octet-stream"}, kindstream.Deps{HTTP: origin.Client(), LimitRangeTransport: m.LimitRangeTransport})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	ra := kindstream.ReadAhead(ctx, src, src.Size())
	section := io.NewSectionReader(ra, 0, src.Size())
	var dst bytes.Buffer
	if _, err = io.Copy(&dst, section); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, dst.Bytes()) {
		t.Fatal("wrong stream bytes")
	}
	if got := peak.Load(); got != 2 {
		t.Fatalf("peak = %d, want 2 with one slot reserved", got)
	}
	seek := make([]byte, 300)
	if _, err = ra.ReadAt(seek, 73621); err != nil || !bytes.Equal(seek, payload[73621:73621+len(seek)]) {
		t.Fatalf("seek: %v", err)
	}
	stall.Store(true)
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := src.(core.ContextReaderAt).ReadAtContext(readCtx, make([]byte, 512<<10), 0)
		done <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("stalled reads never started")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled read: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("workers outlived cancellation")
	}
	stall.Store(false)
	changed.Store(true)
	if n, err := src.ReadAt(make([]byte, 300<<10), 123456); err == nil || n != 0 {
		t.Fatalf("changed representation: n=%d err=%v", n, err)
	}
}

func TestRangeStreamSingleConnection(t *testing.T) {
	for _, tc := range []struct {
		name        string
		connections int
		etag        string
	}{
		{"default", 0, `"v1"`}, {"weak validator", 6, `W/"v1"`}, {"no validator", 6, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := bytes.Repeat([]byte{0xbe, 0xef}, 256<<10)
			var requests atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if tc.etag != "" {
					w.Header().Set("ETag", tc.etag)
				}
				http.ServeContent(w, r, "large.bin", time.Time{}, bytes.NewReader(data))
			}))
			defer origin.Close()
			src, err := kindstream.Open(context.Background(), core.PlayableSource{Pattern: "range", URL: origin.URL, Connections: tc.connections}, kindstream.Deps{HTTP: origin.Client()})
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			got := make([]byte, len(data))
			if _, err := src.ReadAt(got, 0); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("read: %v", err)
			}
			if n := requests.Load(); n != 2 {
				t.Fatalf("origin requests = %d, want one probe and one read", n)
			}
		})
	}
}
