package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// faultSource is isolated to a loopback httptest.Server. Faults are armed only
// after probing, so the experiment exercises the encoder's source read.
type faultSource struct {
	good, altered []byte
	fault         string
	armed         atomic.Bool
	requests      atomic.Int64
}

func (s *faultSource) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.armed.Load() {
		http.ServeContent(w, r, "fixture", time.Time{}, bytes.NewReader(s.good))
		return
	}
	n := s.requests.Add(1)
	switch s.fault {
	case "503":
		http.Error(w, "injected failure", 503)
		return
	case "once-503":
		if n == 1 {
			http.Error(w, "injected failure", 503)
			return
		}
	case "once-416":
		if n == 1 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(s.good)))
			http.Error(w, "injected range failure", 416)
			return
		}
	case "slow-first":
		if n == 1 {
			select {
			case <-time.After(500 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
	case "stall":
		if n == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(s.good)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(s.good[:len(s.good)/4])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-time.After(2 * time.Second):
				_, _ = w.Write(s.good[len(s.good)/4:])
			case <-r.Context().Done():
			}
			return
		}
	case "drop":
		if n == 1 {
			h, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijack unavailable", 500)
				return
			}
			c, b, err := h.Hijack()
			if err != nil {
				return
			}
			defer c.Close()
			fmt.Fprintf(b, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(s.good))
			_, _ = b.Write(s.good[:len(s.good)/4])
			_ = b.Flush()
			return
		}
	case "short-writes":
		// Tiny chunks exercise short reads without violating HTTP range semantics.
		http.ServeContent(&shortWriter{ResponseWriter: w}, r, "fixture", time.Time{}, bytes.NewReader(s.good))
		return
	case "changing":
		if n%2 == 0 {
			http.ServeContent(w, r, "fixture", time.Time{}, bytes.NewReader(s.altered))
			return
		}
		http.ServeContent(w, r, "fixture", time.Time{}, bytes.NewReader(s.good))
		return
	}
	http.ServeContent(w, r, "fixture", time.Time{}, bytes.NewReader(s.altered))
}

type shortWriter struct{ http.ResponseWriter }

func (w *shortWriter) Write(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		n := len(b)
		if n > 1024 {
			n = 1024
		}
		k, err := w.ResponseWriter.Write(b[:n])
		total += k
		b = b[k:]
		if err != nil {
			return total, err
		}
		if k == 0 {
			return total, fmt.Errorf("short write made no progress")
		}
	}
	return total, nil
}
