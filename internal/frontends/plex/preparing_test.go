package plex

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPlexPreparingErrorMappingAndRetryableDecision(t *testing.T) {
	s := &Server{log: slog.Default()}
	prepErr := &core.PreparingError{RetryAfter: 7 * time.Second}

	if got := playbackMessage(prepErr); got != "Finding a source…" {
		t.Fatalf("playbackMessage(PreparingError) = %q", got)
	}

	// API error writer returns 503 + Retry-After + code="preparing".
	w := httptest.NewRecorder()
	s.writeErr(w, httptest.NewRequest(http.MethodGet, "/", nil), prepErr)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("writeErr status = %d, want 503", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After = %q, want 7", got)
	}
	if got := w.Header().Get("X-Ether-Media-Error"); got != "preparing" {
		t.Fatalf("X-Ether-Media-Error = %q, want preparing", got)
	}
	var wire struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil || wire.Error.Code != "preparing" {
		t.Fatalf("writeErr body = %s, err = %v", w.Body.String(), err)
	}

	// Player start routes carry the same retry hint while the source is pending.
	w = httptest.NewRecorder()
	s.writeStartErr(w, httptest.NewRequest(http.MethodGet, "/", nil), nil, prepErr)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "7" {
		t.Fatalf("writeStartErr = %d hdr=%v", w.Code, w.Header())
	}

	// A preparing decision must NOT poison the following start request once preparation finishes.
	f := newFixture()
	defer f.srv.Close()
	f.playback.failures[fightClub.Key] = prepErr
	dec := f.get(t, "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=prep1", jsonAccept...)
	if dec.Code != http.StatusServiceUnavailable || dec.Header().Get("Retry-After") != "7" {
		t.Fatalf("decision status = %d", dec.Code)
	}
	delete(f.playback.failures, fightClub.Key)
	start := f.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=prep1&protocol=hls")
	if start.Code != http.StatusOK {
		t.Fatalf("start after preparing decision finished = %d %s", start.Code, start.Body.String())
	}
}
