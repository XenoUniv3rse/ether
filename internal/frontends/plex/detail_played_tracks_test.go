package plex

// The pre-play detail page of a title the viewer already played keeps
// listing that file's subtitle tracks, whatever capabilities the same device
// sends on later requests and whether or not a search can answer. Real
// playback.Manager, resolver profile selection, store and Plex request
// parsing; the indexer, engine bytes and container probe are offline
// stand-ins. Each case plays once with the decision capabilities and then
// reads the detail page with the capabilities of a later request.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/store"
)

// playedSource reports a configurable size so the average bitrate can exceed
// a remote ceiling; reads return zeros.
type playedSource struct {
	id   string
	size int64
}

func (s *playedSource) ReadAt(p []byte, off int64) (int, error) {
	if off >= s.size {
		return 0, io.EOF
	}
	n := min(int64(len(p)), s.size-off)
	clear(p[:n])
	if n < int64(len(p)) {
		return int(n), io.EOF
	}
	return int(n), nil
}
func (s *playedSource) Size() int64       { return s.size }
func (s *playedSource) Name() string      { return s.id + ".mkv" }
func (s *playedSource) ReleaseID() string { return s.id }
func (s *playedSource) Close() error      { return nil }

type playedEngine struct{ size int64 }

func (e *playedEngine) Open(_ context.Context, id string, _ []byte) (core.MediaSource, error) {
	return &playedSource{id: id, size: e.size}, nil
}
func (e *playedEngine) Prefetch(context.Context, string, []byte, int64, int64) error { return nil }

// playedProbe is the container probe of the file: embedded tracks only.
func playedProbe(runtime time.Duration, size int64) core.MediaInfo {
	return core.MediaInfo{
		Container: "mkv", Duration: runtime, Size: size,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Profile: "High", Width: 1920, Height: 1080, FrameRate: 23.976, BitDepth: 8},
			{Index: 1, Type: "audio", Codec: "eac3", Language: "en", Channels: 6, Default: true},
			{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en"},
			{Index: 3, Type: "subtitle", Codec: "subrip", Language: "pt"},
		},
	}
}

const playedSubtitles = 2

type playedLab struct {
	dir    string
	cfg    config.Config
	title  core.Title
	idx    *etheLabIndexer
	engine *playedEngine
	f      *fixture
	rs     *resolver.Service
	mgr    *playback.Manager
	db     *sql.DB
	clock  atomic.Int64 // manager and resolver time, unix nanoseconds
}

func (x *playedLab) now() time.Time { return time.Unix(0, x.clock.Load()) }

// age moves manager and resolver time forward; the Plex fixture keeps its own.
func (x *playedLab) age(d time.Duration) { x.clock.Add(int64(d)) }

const (
	playedLAN    = "192.168.1.50:51000"
	playedPublic = "203.0.113.7:51000"
)

var playedAndroid = map[string]string{
	"X-Plex-Product":            "Plex for Android (Mobile)",
	"X-Plex-Platform":           "Android",
	"X-Plex-Device":             "Pixel 7",
	"X-Plex-Client-Identifier":  "android-played",
	"X-Plex-Session-Identifier": "android-played-play",
}

func newPlayedLab(t *testing.T, remoteMaxKbps int, sourceSize int64) *playedLab {
	t.Helper()
	x := &playedLab{dir: t.TempDir()}
	x.cfg = config.Defaults(x.dir)
	x.cfg.Quality.Profiles = []core.QualityProfile{{Name: "home", MaxHeight: 2160}, {Name: "away", MaxHeight: 1080}}
	x.cfg.Quality.Default = "home"
	x.cfg.Quality.DefaultAway = "away"
	x.cfg.Quality.Rules = nil
	x.cfg.Transcode.RemoteMaxBitrateKbps = remoteMaxKbps
	x.title = core.Title{Key: core.MovieKey(990001), Type: core.TypeMovie, Title: "Fjord Harbor", Year: 1999, Runtime: 2 * time.Hour, Language: "en"}
	name := "Fjord.Harbor.1999.1080p.WEB-DL.DDP5.1.H.264-GRP"
	x.idx = &etheLabIndexer{releases: []core.Release{{ID: "rel-1080", GUID: "rel-1080", Indexer: "offline", Name: name, Size: sourceSize, PubDate: fixedNow.Add(-24 * time.Hour), NZBURL: "offline", Info: releaseparse.Parse(name)}}}
	x.engine = &playedEngine{size: sourceSize}
	x.clock.Store(fixedNow.UnixNano())
	x.open(t)
	t.Cleanup(x.close)
	return x
}

func (x *playedLab) open(t *testing.T) {
	t.Helper()
	var err error
	x.db, err = store.Open(context.Background(), x.dir)
	if err != nil {
		t.Fatal(err)
	}
	x.f = newFixture(func(d *testServerDeps) {
		setDepsConfig(d, func() config.Config { return x.cfg })
		d.DisableEnrichment = true
	})
	x.f.catalog.titles[x.title.Key] = x.title
	quiet := slog.New(slog.DiscardHandler)
	// Shared store as in internal/app/build_sources.go: remembered
	// candidates and pins survive a restart the same way.
	x.rs = resolver.New(x.idx, nil, x.db, x.cfg, resolver.Options{Now: x.now, Logger: quiet})
	// Profile selection is wired as in internal/app/build_playback.go.
	x.mgr, err = playback.New(playback.Deps{
		Catalog: x.f.catalog, State: x.f.state, Resolver: x.rs, Engine: x.engine, Transcoder: x.f.trans, DB: x.db, Logger: quiet,
		SelectTitleProfile: func(c core.ClientCaps, u *core.User, k core.RatingKey) core.QualityProfile {
			return x.rs.ProfileForTitle(context.Background(), c, u, &core.Title{Key: k})
		},
		SelectProfile: func(c core.ClientCaps, u *core.User, media string) core.QualityProfile {
			return resolver.SelectProfileForMedia(x.cfg.Quality, c, u, media)
		},
		NativeProbe: func(_ context.Context, s core.MediaSource) (*core.MediaInfo, error) {
			info := playedProbe(x.title.Runtime, s.Size())
			return &info, nil
		},
	}, x.cfg, playback.Options{SessionSurface: func(surface string) string {
		if surface == "plex" {
			return ""
		}
		return surface
	}, DataDir: x.dir, Now: x.now, PinInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	x.f.srv.setPlayback(x.mgr)
	x.f.srv.setVariants(x.rs)
}

func (x *playedLab) close() {
	x.f.srv.Close()
	x.mgr.Close()
	x.rs.Close()
	x.db.Close()
}

// restart is a clean shutdown and reopen: the resume row and probe stay on
// disk, the in-process copy and warm entries do not.
func (x *playedLab) restart(t *testing.T) {
	t.Helper()
	x.close()
	x.open(t)
}

// setSearch makes every later search answer with the release (up) or fail
// (an indexer outage or rate limit). Background warm-ups are drained first
// so they never observe a half-written switch.
func (x *playedLab) setSearch(t *testing.T, up bool, release []core.Release) {
	t.Helper()
	x.drainWarm(t)
	if up {
		x.idx.failure, x.idx.releases = nil, release
		return
	}
	x.idx.failure, x.idx.releases = errors.New("indexer unavailable"), nil
}

// drainWarm waits for the device's detail warm-up, if any, to finish.
func (x *playedLab) drainWarm(t *testing.T) {
	t.Helper()
	for _, remote := range []bool{false, true} {
		x.mgr.WaitWarmProbe(context.Background(), &ownerUser, x.detailCaps(remote), x.title.Key, 5*time.Second)
	}
}

func (x *playedLab) request(t *testing.T, target, remoteAddr string, extra ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = remoteAddr
	for k, v := range playedAndroid {
		req.Header.Set(k, v)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	req.Header.Set("X-Plex-Token", ownerToken)
	rec := httptest.NewRecorder()
	x.f.srv.ServeHTTP(rec, req)
	return rec
}

// detailSubs renders GET /library/metadata/{key} and counts the subtitle
// streams on the selected (or first) Media.
func (x *playedLab) detailSubs(t *testing.T, remoteAddr string) int {
	t.Helper()
	rec := x.request(t, "/library/metadata/"+x.title.Key.String(), remoteAddr, "Accept", "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		MediaContainer struct {
			Metadata []struct {
				Media []struct {
					Selected bool `json:"selected"`
					Part     []struct {
						Stream []struct {
							StreamType int `json:"streamType"`
						} `json:"Stream"`
					} `json:"Part"`
				} `json:"Media"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.MediaContainer.Metadata) != 1 || len(resp.MediaContainer.Metadata[0].Media) == 0 {
		t.Fatalf("detail without media: %s", rec.Body.String())
	}
	media := resp.MediaContainer.Metadata[0].Media
	pick := 0
	for i, m := range media {
		if m.Selected {
			pick = i
			break
		}
	}
	subs := 0
	for _, p := range media[pick].Part {
		for _, st := range p.Stream {
			if st.StreamType == 3 {
				subs++
			}
		}
	}
	x.drainWarm(t)
	return subs
}

// play sends the Plex decision for this device, then stops the session the
// way a client leaving the player does, so SessionFor no longer answers.
func (x *playedLab) play(t *testing.T, remoteAddr string, query url.Values) {
	t.Helper()
	q := url.Values{"path": {"/library/metadata/" + x.title.Key.String()}, "session": {"played-play"}, "protocol": {"dash"}, "directPlay": {"0"}, "directStream": {"1"}, "hasMDE": {"1"}, "mediaIndex": {"0"}, "partIndex": {"0"}, "offset": {"0"}}
	for k, v := range query {
		q[k] = v
	}
	rec := x.request(t, "/video/:/transcode/universal/decision?"+q.Encode(), remoteAddr)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "generalDecisionCode") {
		t.Fatalf("decision %d: %s", rec.Code, rec.Body.String())
	}
	sessions := x.mgr.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("decision admitted %d sessions", len(sessions))
	}
	x.mgr.Stop(sessions[0].ID)
	if _, ok := x.mgr.SessionFor(ownerUser.ID, playedAndroid["X-Plex-Client-Identifier"], x.title.Key); ok {
		t.Fatal("stopped session still answers SessionFor")
	}
}

// detailCaps are the caps the Plex server derives for this device's detail
// request (no decision query), as warmDetail and mediaInfoWithSource use them.
func (x *playedLab) detailCaps(remote bool) core.ClientCaps {
	pr := &Request{User: &ownerUser, ClientID: playedAndroid["X-Plex-Client-Identifier"], Product: playedAndroid["X-Plex-Product"], Platform: playedAndroid["X-Plex-Platform"], Device: playedAndroid["X-Plex-Device"], Remote: remote}
	return x.f.srv.clientCapsForKey(pr, x.title.Key)
}

// 1080p WEB-DL, two hours: 4 GB is about 4.4 Mbps, 9 GB about 10 Mbps.
const (
	playedLowBitrate  = int64(4) << 30
	playedHighBitrate = int64(9) << 30
)

// The detail page lists the played file's subtitle tracks after play under
// every capability change below, in every phase:
//   - right after play, in the same process;
//   - seven hours later, past the warm entry Play kept (60 min) and the
//     resolver's search cache (6 h);
//   - when search answers again, and sixteen minutes after that;
//   - after a clean restart, without and with search.
//
// With searchDown every search after play fails. ResumedTracks answers while
// the profile selection key and the source fit still match (wantResumed);
// otherwise the detail warm-up lists the tracks of the release the viewer
// last played.
func TestDetailKeepsPlayedReleaseTracksWhenSearchFails(t *testing.T) {
	wan := url.Values{"location": {"wan"}, "maxVideoBitrate": {"20000"}, "videoResolution": {"1920x1080"}}
	cases := []struct {
		name        string
		remoteMax   int
		size        int64
		playAddr    string
		playQuery   url.Values
		detailAddr  string
		wantResumed bool
	}{
		// LAN decision, LAN detail.
		{"lan", 0, playedLowBitrate, playedLAN, url.Values{}, playedLAN, true},
		// The player asks a lower bitrate than the detail request declares.
		{"decision-bitrate-only", 0, playedHighBitrate, playedLAN, url.Values{"maxVideoBitrate": {"4000"}}, playedLAN, true},
		// A LAN client sending location=wan stays local (source IP decides).
		{"decision-wan-detail-lan", 0, playedLowBitrate, playedLAN, wan, playedLAN, true},
		// Same with a remote ceiling configured: LAN client stays local and is not remote-capped.
		{"decision-wan-detail-lan-remote-cap", 8000, playedHighBitrate, playedLAN, wan, playedLAN, true},
		// An actual public WAN play with a remote ceiling the stored release exceeds.
		{"wan-play-lan-detail-remote-cap", 8000, playedHighBitrate, playedPublic, wan, playedLAN, false},
		// Played at home, detail page later opened away from home.
		{"lan-play-wan-detail", 0, playedLowBitrate, playedLAN, url.Values{}, playedPublic, false},
	}
	for _, searchDown := range []bool{false, true} {
		for _, tc := range cases {
			name := tc.name
			if searchDown {
				name += "/search-down"
			}
			t.Run(name, func(t *testing.T) {
				x := newPlayedLab(t, tc.remoteMax, tc.size)
				releases := x.idx.releases
				search := func(up bool) { x.setSearch(t, up || !searchDown, releases) }
				check := func(phase string) {
					t.Helper()
					if got := x.detailSubs(t, tc.detailAddr); got != playedSubtitles {
						// The helper drains any background warm-up after the read.
						if got = x.detailSubs(t, tc.detailAddr); got != playedSubtitles {
							t.Errorf("%s: detail lists %d subtitles after background warm-up, want %d", phase, got, playedSubtitles)
						}
					}
				}

				x.detailSubs(t, tc.playAddr) // Trigger and drain the background warm-up.
				if got := x.detailSubs(t, tc.playAddr); got != playedSubtitles {
					t.Fatalf("before play: warm detail lists %d subtitles, want %d", got, playedSubtitles)
				}
				x.play(t, tc.playAddr, tc.playQuery)
				_, _, resumed := x.mgr.ResumedTracks(context.Background(), &ownerUser, x.title.Key, x.detailCaps(tc.detailAddr == playedPublic))
				if resumed != tc.wantResumed {
					t.Errorf("ResumedTracks answered=%v, want %v", resumed, tc.wantResumed)
				}
				search(false)
				check("after play")
				x.age(7 * time.Hour)
				check("seven hours later")
				search(true)
				check("search answers again")
				x.age(16 * time.Minute)
				check("sixteen minutes later")
				search(false)
				x.restart(t)
				check("after restart")
				search(true)
				check("after restart, search answers")
			})
		}
	}
}
