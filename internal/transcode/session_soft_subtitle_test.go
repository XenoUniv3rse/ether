package transcode

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type subtitleFlushRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
}

func (r *subtitleFlushRecorder) Flush() {
	r.ResponseRecorder.Flush()
	select {
	case r.flushed <- struct{}{}:
	default:
	}
}

// The session, not a particular HTTP request, owns the output across encoder
// replacements. A retry can still read the current run after it has ended.
func TestSoftSubtitleFollowsEncoderRunsAndRetries(t *testing.T) {
	dir := t.TempDir()
	index := 2
	first := filepath.Join(dir, "first.ass")
	second := filepath.Join(dir, "second.ass")
	if err := os.WriteFile(first, []byte("[Script Info]\n[Events]\nFormat: Text\nDialogue: first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("[Script Info]\n[Events]\nFormat: Text\nDialogue: second\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r1 := &run{seq: 1, spec: runSpec{SoftSubtitlePath: first}}
	r2 := &run{seq: 2, spec: runSpec{SoftSubtitlePath: second}}
	s := &Session{sessionInput: sessionInput{softSubtitleStream: &index, softSubtitleInputIndex: index, info: core.MediaInfo{Streams: []core.StreamInfo{{Index: index, Type: "subtitle", Codec: "subrip"}}}}, sessionEncoding: sessionEncoding{cur: r1, resumeAt: -1}}
	out := &subtitleFlushRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}, 2)}
	done := make(chan bool, 1)
	go func() { done <- s.ServeSoftSubtitle(out, httptest.NewRequest(http.MethodGet, "/", nil), index) }()
	select {
	case <-out.flushed:
	case <-time.After(3 * time.Second):
		t.Fatal("initial header was not flushed")
	}
	select {
	case <-out.flushed:
	case <-time.After(3 * time.Second):
		t.Fatal("first output was not flushed")
	}
	s.mu.Lock()
	s.cur = r2
	s.mu.Unlock()
	select {
	case <-out.flushed:
	case <-time.After(3 * time.Second):
		t.Fatal("replacement output was not flushed")
	}
	s.mu.Lock()
	r2.finished = true
	s.mu.Unlock()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("selected stream rejected")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("finished stream did not close")
	}
	if b := out.Body.String(); !strings.Contains(b, "Dialogue: first") || !strings.Contains(b, "Dialogue: second") {
		t.Fatalf("missing run output: %q", b)
	}
	retry := httptest.NewRecorder()
	if !s.ServeSoftSubtitle(retry, httptest.NewRequest(http.MethodGet, "/", nil), index) || !strings.Contains(retry.Body.String(), "Dialogue: second") {
		t.Fatal("retry did not attach to current output")
	}
	if s.ServeSoftSubtitle(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), index+1) {
		t.Fatal("different stream was accepted")
	}
}
