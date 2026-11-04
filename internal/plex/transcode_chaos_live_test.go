//go:build transcodechaos

package plex

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type liveChaosRunner struct {
	mu sync.Mutex
	p  transcode.Process
}

func (r *liveChaosRunner) Run(ctx context.Context, c transcode.Command) error {
	return (transcode.ExecRunner{}).Run(ctx, c)
}
func (r *liveChaosRunner) Start(c transcode.Command) (transcode.Process, error) {
	for i, a := range c.Args {
		if a == "-i" {
			c.Args = append(c.Args[:i], append([]string{"-re"}, c.Args[i:]...)...)
			break
		}
	}
	p, e := (transcode.ExecRunner{}).Start(c)
	if e == nil {
		r.mu.Lock()
		r.p = p
		r.mu.Unlock()
	}
	return p, e
}
func (r *liveChaosRunner) interrupt() error {
	r.mu.Lock()
	p := r.p
	r.mu.Unlock()
	if p == nil {
		return fmt.Errorf("no live encoder")
	}
	select {
	case <-p.Done():
		return fmt.Errorf("encoder ended before injection")
	default:
	}
	p.Stop()
	return nil
}
func TestTranscodeChaosLiveRelay(t *testing.T) {
	ff, fp := os.Getenv("CHAOS_FFMPEG"), os.Getenv("CHAOS_FFPROBE")
	if ff == "" || fp == "" {
		t.Fatal("set CHAOS_FFMPEG and CHAOS_FFPROBE")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "live.ts")
	chaosCmd(t, ff, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "15", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-g", "48", "-c:a", "aac", "-f", "mpegts", file)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(data) / 2 / 188 * 188; i < len(data)/2/188*188+30*188 && i < len(data); i++ {
		if i%188 != 0 {
			data[i] = 0xff
		}
	}
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer upstream.Close()
	runner := &liveChaosRunner{}
	m, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "none", TempDir: filepath.Join(dir, "work")}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithRunner(runner), transcode.WithSegmentTimeout(8*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service, err := livetv.New(livetv.Options{Transcoder: m, DB: db, Now: func() time.Time { return fixedNow }, HTTPClient: upstream.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	_, err = service.ImportSource(context.Background(), livetv.Source{Name: "Fixture", Playlist: fmt.Sprintf("#EXTM3U\n#EXTINF:-1 tvg-id=\"fixture\" tvg-chno=\"7\",Fixture\n%s/live.ts\n", upstream.URL)})
	if err != nil {
		t.Fatal(err)
	}
	channels, err := service.Channels(context.Background())
	if err != nil || len(channels) != 1 {
		t.Fatal("missing fixture channel")
	}
	channel := channels[0]
	channel.Transcode = true
	if _, err = service.SaveChannel(context.Background(), channel); err != nil {
		t.Fatal(err)
	}
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.LiveTV = service
	f.srv.d.Identity.MyPlexSubscription = true
	tune := f.request(t, "POST", "/livetv/dvrs/1/channels/"+channel.ID+"/tune", map[string]string{"Accept": "application/json"})
	if tune.Code != 200 {
		t.Fatalf("tune HTTP %d", tune.Code)
	}
	metadata := decodeJSON(t, tune)["Metadata"].([]any)[0].(map[string]any)
	session := metadata["key"].(string)
	playlist := f.get(t, session+"/0/index.m3u8")
	if playlist.Code != 200 {
		t.Fatalf("playlist HTTP %d", playlist.Code)
	}
	var media string
	for _, line := range strings.Split(playlist.Body.String(), "\n") {
		if strings.Contains(line, "/relay/0.ts") {
			media = line
			break
		}
	}
	if media == "" {
		t.Fatal("first live relay segment missing")
	}
	first := f.get(t, media)
	if first.Code != 200 || first.Body.Len() == 0 {
		t.Fatalf("first live segment HTTP %d", first.Code)
	}
	path := filepath.Join(dir, "first.ts")
	if err = os.WriteFile(path, first.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	chaosCmd(t, fp, "-v", "error", "-show_streams", path)
	chaosCmd(t, ff, "-v", "error", "-xerror", "-i", path, "-f", "null", "-")
	if err = runner.interrupt(); err != nil {
		t.Fatal(err)
	}
	// A watcher requests the next segment after the interruption. Its URI uses
	// the same relay authorization as the first and is never printed or logged.
	next := strings.Replace(media, "/relay/0.ts", "/relay/1.ts", 1)
	r := f.get(t, next)
	if r.Code != 200 || r.Body.Len() == 0 {
		t.Fatalf("live continuation HTTP %d", r.Code)
	}
	path = filepath.Join(dir, "next.ts")
	if err = os.WriteFile(path, r.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	chaosCmd(t, fp, "-v", "error", "-show_streams", path)
	chaosCmd(t, ff, "-v", "error", "-xerror", "-i", path, "-f", "null", "-")
	updated := f.get(t, session+"/0/index.m3u8")
	if updated.Code != 200 || !strings.Contains(updated.Body.String(), "/relay/1.ts") || !strings.Contains(updated.Body.String(), "#EXT-X-DISCONTINUITY") {
		t.Fatal("live playlist did not advance with a discontinuity")
	}
}
