//go:build transcodechaos

package plex

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type interruptingWriter struct {
	http.ResponseWriter
	once   sync.Once
	bytes  int
	runner *liveChaosRunner
	err    chan error
}

func (w *interruptingWriter) Write(p []byte) (int, error) {
	n, e := w.ResponseWriter.Write(p)
	w.bytes += n
	if w.bytes >= 32768 {
		w.once.Do(func() { go func() { w.err <- w.runner.interrupt(true) }() })
	}
	return n, e
}
func (w *interruptingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func TestTranscodeChaosHTTPHardware(t *testing.T) {
	ff, fp := os.Getenv("CHAOS_FFMPEG"), os.Getenv("CHAOS_FFPROBE")
	if ff == "" || fp == "" {
		t.Fatal("set CHAOS_FFMPEG and CHAOS_FFPROBE")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "input.mp4")
	chaosCmd(t, ff, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "12", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-g", "48", "-c:a", "aac", "-movflags", "+faststart", input)
	source := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer source.Close()
	runner := &liveChaosRunner{}
	m, e := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "videotoolbox", TempDir: filepath.Join(dir, "work")}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithRunner(runner))
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	if m.HWAccel().Name != "videotoolbox" {
		t.Skip("VideoToolbox unavailable")
	}
	info, e := m.Probe(context.Background(), source.URL+"/input.mp4")
	if e != nil {
		t.Fatal(e)
	}
	f := newFixture()
	defer f.srv.Close()
	f.playback.info = *info
	f.srv.d.Playback = &chaosPlayback{f.playback, source.URL + "/input.mp4"}
	f.srv.d.Transcoder = m
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", "/video/:/transcode/universal/start.mkv?path=%2Flibrary%2Fmetadata%2F"+fightClub.Key.String()+"&session=http-hardware&protocol=http&directPlay=0&directStream=0", nil).WithContext(ctx)
	req.Header.Set("X-Plex-Token", ownerToken)
	req.Header.Set("X-Plex-Client-Identifier", "client-1")
	rec := httptest.NewRecorder()
	w := &interruptingWriter{ResponseWriter: rec, runner: runner, err: make(chan error, 1)}
	f.srv.ServeHTTP(w, req)
	select {
	case err := <-w.err:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("hardware failure was not injected (status=%d, bytes=%d)", rec.Code, rec.Body.Len())
	}
	if rec.Code != 200 {
		t.Fatalf("HTTP %d", rec.Code)
	}
	if !runner.softwareStarted() {
		t.Fatal("progressive stream did not fall back to software")
	}
	output := filepath.Join(dir, "result.mkv")
	if e := os.WriteFile(output, rec.Body.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	chaosCmd(t, ff, "-v", "error", "-xerror", "-i", output, "-f", "null", "-")
	out, e := exec.CommandContext(ctx, fp, "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time", "-of", "csv=p=0", output).Output()
	if e != nil {
		t.Fatal(e)
	}
	last := 0.0
	for _, line := range strings.Split(string(out), "\n") {
		if v, e := strconv.ParseFloat(strings.TrimSpace(strings.Split(line, ",")[0]), 64); e == nil && v > last {
			last = v
		}
	}
	if last < 10 {
		t.Fatalf("progressive stream stopped early at %.2fs", last)
	}
}
