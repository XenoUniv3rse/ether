//go:build transcodechaos

package plex

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type chaosByteWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (w chaosByteWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.n.Add(int64(n))
	return n, err
}

// The source facts model a long title; guard verification must not download
// the source from byte zero just because a late seek was requested.
func TestTranscodeChaosPCERejectsUnbounded(t *testing.T) {
	ff, fp, corpus := os.Getenv("CHAOS_FFMPEG"), os.Getenv("CHAOS_FFPROBE"), os.Getenv("CHAOS_CORPUS")
	if ff == "" || fp == "" || corpus == "" {
		t.Skip("requires generated chaos corpus and pinned ffmpeg")
	}
	if _, err := os.Stat(filepath.Join(corpus, "input-multi-audio.good")); err != nil {
		t.Skip("PCE fixture unavailable")
	}
	var count atomic.Int64
	files := http.FileServer(http.Dir(corpus))
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		files.ServeHTTP(chaosByteWriter{w, &count}, r)
	}))
	defer source.Close()
	m, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "none", TempDir: filepath.Join(t.TempDir(), "work")}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	url := source.URL + "/input-multi-audio.good"
	info, err := m.Probe(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	info.Duration = 2 * time.Hour
	f := newFixture()
	defer f.srv.Close()
	f.playback.info = *info
	f.srv.d.Playback = &chaosPlayback{f.playback, url}
	f.srv.d.Transcoder = m
	count.Store(0)
	r := f.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F"+fightClub.Key.String()+"&session=pce-late&protocol=hls&directPlay=0&directStream=1&offset=4200", "X-Plex-Product", "Plex Web", "X-Plex-Platform", "Chrome")
	if r.Code == http.StatusOK {
		t.Fatal("unbounded remote PCE seek was accepted")
	}
	if got := count.Load(); got != 0 {
		t.Fatalf("late PCE seek read %d source bytes", got)
	}
}
