package livetv

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func makeValidTSPacket() []byte {
	// 3 valid 188-byte MPEG-TS packets with 0x47 sync bytes.
	pkt := make([]byte, 3*tsPacket)
	for i := 0; i < 3; i++ {
		pkt[i*tsPacket] = 0x47
		pkt[i*tsPacket+1] = 0x40
	}
	return pkt
}

func TestDirectStreamSingleUseMasterMuxedTS(t *testing.T) {
	var masterHits atomic.Int32
	var resolveCalls atomic.Int32
	var variantHits atomic.Int32
	var sawHeader atomic.Bool
	tsSeg := makeValidTSPacket()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			if r.Header.Get("X-Stream-Auth") == "secret-hdr" {
				sawHeader.Store(true)
			}
			if masterHits.Add(1) > 1 {
				http.Error(w, "token already used", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720,CODECS=\"avc1.64001f,mp4a.40.2\"\n/variant.m3u8\n")
		case "/variant.m3u8":
			variantHits.Add(1)
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:2.0,\n/seg1.ts\n#EXTINF:2.0,\n/seg2.ts\n#EXTINF:2.0,\n/seg3.ts\n")
		case "/seg1.ts", "/seg2.ts", "/seg3.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = w.Write(tsSeg)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	// No Live TV channels configured: s.Enabled() is false, yet direct live
	// streams from plugin libraries must still tune and serve.
	s := testService(t, Options{HTTPClient: upstream.Client()})
	if s.Enabled() {
		t.Fatal("expected Live TV to be disabled with no channels configured")
	}

	var allowed atomic.Bool
	allowed.Store(true)
	u := &core.User{ID: 7, Username: "alice", StreamLimit: 2}
	caps := core.ClientCaps{
		Product:       "Vela",
		Protocol:      "vela",
		ClientID:      "vela-client-1",
		HLSContainers: []string{"mpegts"},
	}

	info, err := s.TuneDirectStream(context.Background(), u, DirectStream{
		ID:    "esports:ch-1",
		Title: "Finals Stage A",
		URL:   upstream.URL + "/missing",
		Resolve: func(context.Context) (DirectStreamResult, error) {
			resolveCalls.Add(1)
			return DirectStreamResult{
				URL:     upstream.URL + "/master.m3u8?token=once",
				Headers: map[string]string{"X-Stream-Auth": "secret-hdr"},
			}, nil
		},
		AllowCheck: func(context.Context, *core.User) error {
			if !allowed.Load() {
				return core.ErrNotFound
			}
			return nil
		},
	}, caps, "vela-sess-1")
	if err != nil {
		t.Fatalf("TuneDirectStream failed: %v", err)
	}
	defer func() { _ = s.Stop(context.Background(), info.ID, u) }()

	if resolveCalls.Load() != 1 {
		t.Fatal("unavailable input did not refresh once")
	}
	if got := masterHits.Load(); got != 1 {
		t.Fatalf("master playlist fetched %d times, want exactly 1", got)
	}
	if !sawHeader.Load() {
		t.Fatal("custom header X-Stream-Auth was not forwarded to upstream")
	}
	if variantHits.Load() == 0 {
		t.Fatal("variant media playlist was never fetched")
	}

	// Verify ServePlaylist returns the owned playlist and ServeRelaySegment works.
	rec := httptest.NewRecorder()
	s.ServePlaylist(rec, httptest.NewRequest(http.MethodGet, "/index.m3u8", nil), info.ID, u)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "#EXTM3U") || !strings.Contains(rec.Body.String(), "/relay/") {
		t.Fatalf("ServePlaylist status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Direct streams must never register as Live TV channels.
	if chs, err := s.Channels(context.Background()); err != nil || len(chs) != 0 {
		t.Fatalf("expected 0 Live TV channels, got %d (err=%v)", len(chs), err)
	}

	// Revoking access via AllowCheck immediately denies playlist reads.
	allowed.Store(false)
	denied := httptest.NewRecorder()
	s.ServePlaylist(denied, httptest.NewRequest(http.MethodGet, "/index.m3u8", nil), info.ID, u)
	if denied.Code != http.StatusNotFound {
		t.Fatalf("revoked ServePlaylist status=%d, want 404", denied.Code)
	}
	if got := masterHits.Load(); got != 1 {
		t.Fatalf("master playlist fetched %d times after serving, want 1", got)
	}
}

type singleUseRelayTranscoder struct {
	core.Transcoder
	client *http.Client

	mu            sync.Mutex
	probeManifest string
	relayManifest string
	probeErr      error
	relayErr      error
	startHLSCount int
}

func (f *singleUseRelayTranscoder) Decide(_ core.MediaInfo, _ core.ClientCaps, opts core.DecideOptions) core.Decision {
	if !opts.DirectStream {
		return core.Decision{
			Kind:       core.DecisionTranscode,
			Container:  "mpegts",
			VideoCodec: "h264",
			AudioCodec: "aac",
			VideoCopy:  false,
			AudioCopy:  true,
		}
	}
	return core.Decision{
		Kind:       core.DecisionRemux,
		Container:  "mpegts",
		VideoCodec: "h264",
		AudioCodec: "aac",
		VideoCopy:  true,
		AudioCopy:  true,
	}
}

func (f *singleUseRelayTranscoder) Probe(ctx context.Context, input string) (*core.MediaInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, input, nil)
	if err != nil {
		return nil, err
	}
	res, err := f.client.Do(req)
	if err != nil {
		f.mu.Lock()
		f.probeErr = err
		f.mu.Unlock()
		return nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	f.mu.Lock()
	f.probeManifest = string(body)
	f.mu.Unlock()
	if res.StatusCode != http.StatusOK {
		return nil, providerResponseError(res.StatusCode)
	}
	return &core.MediaInfo{
		Container: "hls",
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1280, Height: 720, BitDepth: 8},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2},
		},
	}, nil
}

func (f *singleUseRelayTranscoder) StartHLS(ctx context.Context, r core.HLSRequest) (core.HLSSession, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.InputURL, nil)
	if err != nil {
		return nil, err
	}
	res, err := f.client.Do(req)
	if err != nil {
		f.mu.Lock()
		f.relayErr = err
		f.mu.Unlock()
		return nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	f.mu.Lock()
	f.relayManifest = string(body)
	f.startHLSCount++
	f.mu.Unlock()
	if res.StatusCode != http.StatusOK {
		return nil, providerResponseError(res.StatusCode)
	}
	return &cmafRelayHLS{fakeHLS: fakeHLS{id: r.SessionID}, seg: makeValidTSPacket()}, nil
}

type cmafRelayHLS struct {
	fakeHLS
	seg []byte
}

func (h *cmafRelayHLS) ServePlaylist(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:2.0,\n00000.ts\n#EXTINF:2.0,\n00001.ts\n#EXTINF:2.0,\n00002.ts\n")
}

func (h *cmafRelayHLS) ServeSegment(w http.ResponseWriter, _ *http.Request, _ int) {
	w.Header().Set("Content-Type", "video/mp2t")
	_, _ = w.Write(h.seg)
}

func TestDirectStreamSingleUseMasterDemuxedCMAF(t *testing.T) {
	var masterHits atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cmaf-master.m3u8":
			if masterHits.Add(1) > 1 {
				http.Error(w, "token already used", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, strings.Join([]string{
				"#EXTM3U",
				"#EXT-X-VERSION:7",
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",DEFAULT=YES,AUTOSELECT=YES,URI="/audio.m3u8"`,
				`#EXT-X-STREAM-INF:BANDWIDTH=3500000,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2",AUDIO="aud"`,
				"/video.m3u8",
				"",
			}, "\n"))
		case "/video.m3u8", "/audio.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:2.0,\n/chunk1.m4s\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	tc := &singleUseRelayTranscoder{client: &http.Client{Timeout: 5 * time.Second}}
	s := testService(t, Options{HTTPClient: upstream.Client(), Transcoder: tc})
	u := &core.User{ID: 8, Username: "bob", StreamLimit: 2}
	caps := core.ClientCaps{
		Product:       "Vela",
		Protocol:      "vela",
		ClientID:      "vela-client-cmaf",
		VideoCodecs:   []string{"h264"},
		AudioCodecs:   []string{"aac"},
		HLSContainers: []string{"mpegts"},
	}

	info, err := s.TuneDirectStream(context.Background(), u, DirectStream{
		ID:    "esports:cmaf-1",
		Title: "Arena CMAF Feed",
		URL:   upstream.URL + "/cmaf-master.m3u8?token=once",
	}, caps, "vela-sess-cmaf")
	if err != nil {
		t.Fatalf("TuneDirectStream (demuxed CMAF) failed: %v", err)
	}
	defer func() { _ = s.Stop(context.Background(), info.ID, u) }()

	if got := masterHits.Load(); got != 1 {
		t.Fatalf("demuxed CMAF master fetched %d times, want exactly 1", got)
	}

	tc.mu.Lock()
	probeMan := tc.probeManifest
	relayMan := tc.relayManifest
	startCount := tc.startHLSCount
	tc.mu.Unlock()

	if !strings.Contains(probeMan, "#EXTM3U") || !strings.Contains(probeMan, "#EXT-X-MEDIA:TYPE=AUDIO") {
		t.Fatalf("probe did not receive rewritten demuxed master: %q", probeMan)
	}
	if startCount != 1 || !strings.Contains(relayMan, "#EXTM3U") || !strings.Contains(relayMan, "#EXT-X-MEDIA:TYPE=AUDIO") {
		t.Fatalf("relay StartHLS (count=%d) did not receive cached demuxed master: %q", startCount, relayMan)
	}

	rec := httptest.NewRecorder()
	s.ServePlaylist(rec, httptest.NewRequest(http.MethodGet, "/index.m3u8", nil), info.ID, u)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "#EXTM3U") {
		t.Fatalf("ServePlaylist status=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := masterHits.Load(); got != 1 {
		t.Fatalf("demuxed CMAF master fetched %d times after playlist serve, want 1", got)
	}
}

func firstManifestURI(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

func TestDirectStreamSingleUseMasterReusedAcrossProducerSwitchAndRecover(t *testing.T) {
	var masterHits atomic.Int32
	var variantHits atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/m1/master.m3u8":
			if masterHits.Add(1) > 1 {
				http.Error(w, "token already used", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			// Relative URIs verify cached master rewrites variants to absolute URLs.
			_, _ = io.WriteString(w, strings.Join([]string{
				"#EXTM3U",
				"#EXT-X-VERSION:7",
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",DEFAULT=YES,AUTOSELECT=YES,URI="sub/audio.m3u8?v=1"`,
				`#EXT-X-STREAM-INF:BANDWIDTH=3500000,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2",AUDIO="aud"`,
				"sub/video.m3u8?v=1",
				"",
			}, "\n"))
		case "/m1/sub/video.m3u8", "/m1/sub/audio.m3u8":
			variantHits.Add(1)
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:2.0,\nseg1.m4s\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	tc := &singleUseRelayTranscoder{client: &http.Client{Timeout: 5 * time.Second}}
	s := testService(t, Options{HTTPClient: upstream.Client(), Transcoder: tc})
	u := &core.User{ID: 11, Username: "carol", StreamLimit: 2}
	caps := core.ClientCaps{
		Product:       "Vela",
		Protocol:      "vela",
		ClientID:      "vela-switch-1",
		VideoCodecs:   []string{"h264"},
		AudioCodecs:   []string{"aac"},
		HLSContainers: []string{"mpegts"},
	}

	rawMaster := upstream.URL + "/m1/master.m3u8?token=once"
	info, err := s.TuneDirectStream(context.Background(), u, DirectStream{
		ID:    "dir:switch-1",
		Title: "Live Switch Test",
		URL:   rawMaster,
	}, caps, "vela-switch-sess")
	if err != nil {
		t.Fatalf("TuneDirectStream failed: %v", err)
	}
	defer func() { _ = s.Stop(context.Background(), info.ID, u) }()

	s.mu.Lock()
	ss := s.sessions[info.ID]
	s.mu.Unlock()
	if ss == nil || ss.owned == nil || ss.owned.relay == nil {
		t.Fatalf("expected active owned relay session, got %+v", ss)
	}

	// Cached master must hold absolute variant/audio URLs.
	cm := ss.cachedMasterFor(rawMaster)
	if cm == nil {
		t.Fatal("expected cached master manifest on direct session")
	}
	wantVideoAbs := upstream.URL + "/m1/sub/video.m3u8?v=1"
	wantAudioAbs := upstream.URL + "/m1/sub/audio.m3u8?v=1"
	if !strings.Contains(string(cm.body), wantVideoAbs) || !strings.Contains(string(cm.body), wantAudioAbs) {
		t.Fatalf("cached master does not contain absolute variant URLs:\n%s", string(cm.body))
	}

	// 1. Producer switch (copy -> re-encode, livetv-<sid>-2) reuses cached master.
	ss.owned.relay.escalate(context.Background(), "test producer switch")
	// 2. Reconnect/retry (switchSource re-grant + new producer) reuses cached master.
	if err := ss.owned.relay.switchSource(context.Background(), ss, ss.info.Channel, ss.inputSource, false); err != nil {
		t.Fatalf("switchSource failed: %v", err)
	}

	if got := masterHits.Load(); got != 1 {
		t.Fatalf("single-use master fetched %d times across escalate+switchSource, want 1", got)
	}
	tc.mu.Lock()
	starts := tc.startHLSCount
	relayMan := tc.relayManifest
	tc.mu.Unlock()
	if starts != 3 {
		t.Fatalf("expected 3 StartHLS producers (initial, escalate, switchSource), got %d", starts)
	}

	// Variant URL rewritten into loopback relay must still resolve cleanly.
	varURL := firstManifestURI(relayMan)
	if varURL == "" {
		t.Fatalf("missing variant URI in relay manifest:\n%s", relayMan)
	}
	res, err := tc.client.Get(varURL)
	if err != nil {
		t.Fatalf("GET rewritten variant failed: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("rewritten variant status=%d, want 200", res.StatusCode)
	}
	if variantHits.Load() == 0 {
		t.Fatal("expected upstream variant playlist fetch")
	}
}

func TestDirectStreamSingleUseMasterReusedAcrossSteadyHandoffAndReopen(t *testing.T) {
	var masterHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/steady/master.m3u8":
			if masterHits.Add(1) > 1 {
				http.Error(w, "token already used", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, strings.Join([]string{
				"#EXTM3U",
				"#EXT-X-VERSION:7",
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",DEFAULT=YES,AUTOSELECT=YES,URI="audio.m3u8"`,
				`#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2",AUDIO="aud"`,
				"video.m3u8",
				"",
			}, "\n"))
		case "/steady/video.m3u8", "/steady/audio.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:2.0,\nseg1.m4s\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	tc := &singleUseRelayTranscoder{client: &http.Client{Timeout: 5 * time.Second}}
	s := testService(t, Options{HTTPClient: upstream.Client(), Transcoder: tc})
	u := &core.User{ID: 12, Username: "dave", StreamLimit: 2}
	caps := core.ClientCaps{
		Product:       "Vela",
		Protocol:      "vela",
		ClientID:      "vela-steady-1",
		VideoCodecs:   []string{"h264"},
		AudioCodecs:   []string{"aac"},
		HLSContainers: []string{"mpegts"},
	}

	info, err := s.TuneDirectStream(context.Background(), u, DirectStream{
		ID:    "dir:steady-1",
		Title: "Steady Handoff Test",
		URL:   upstream.URL + "/steady/master.m3u8?token=once",
	}, caps, "vela-steady-sess")
	if err != nil {
		t.Fatalf("TuneDirectStream failed: %v", err)
	}
	defer func() { _ = s.Stop(context.Background(), info.ID, u) }()

	s.mu.Lock()
	ss := s.sessions[info.ID]
	s.mu.Unlock()

	// 1. Steady hand-off via linkSessionInput + probeSteadyLink reuses cached master.
	link, err := s.linkSessionInput(ss, ss.info.Channel, ss.inputSource, ss.inputURL, ss.inputHeaders)
	if err != nil {
		t.Fatalf("linkSessionInput failed: %v", err)
	}
	defer link.close(s)

	if _, err := s.probeSteadyLink(context.Background(), 2*time.Second, ss.info.Channel, link); err != nil {
		t.Fatalf("probeSteadyLink failed: %v", err)
	}
	linkInput, err := s.relayInputLive(link.ss)
	if err != nil {
		t.Fatalf("relayInputLive failed: %v", err)
	}
	res, err := tc.client.Get(linkInput)
	if err != nil {
		t.Fatalf("GET Steady link input failed: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "#EXTM3U") {
		t.Fatalf("Steady hand-off decoder open status=%d body=%q", res.StatusCode, string(body))
	}

	// 2. Subsequent providerBroadcastPlanner decoder reopen (new link) reuses cached master.
	reopened, err := s.linkSteadySourceWithState(context.Background(), ss.info.Channel, ss.inputSource, "broadcast-reopen-1", 0, ss.direct, nil, nil)
	if err != nil {
		t.Fatalf("linkSteadySourceWithState reopen failed: %v", err)
	}
	defer reopened.close(s)

	reopenInput, err := s.relayInputLive(reopened.ss)
	if err != nil {
		t.Fatalf("relayInputLive reopened failed: %v", err)
	}
	res2, err := tc.client.Get(reopenInput)
	if err != nil {
		t.Fatalf("GET reopened Steady link input failed: %v", err)
	}
	body2, _ := io.ReadAll(res2.Body)
	_ = res2.Body.Close()
	if res2.StatusCode != http.StatusOK || !strings.Contains(string(body2), "#EXTM3U") {
		t.Fatalf("reopened Steady link status=%d body=%q", res2.StatusCode, string(body2))
	}

	if got := masterHits.Load(); got != 1 {
		t.Fatalf("single-use master fetched %d times across hand-off + probe + reopen, want 1", got)
	}
}

func TestLiveTVChannelSteadyAdmissionReusesSingleUseMaster(t *testing.T) {
	var masterHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/iptv/master.m3u8":
			if masterHits.Add(1) > 1 {
				http.Error(w, "token already used", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720\nvariant.m3u8\n")
		case "/iptv/variant.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:2.0,\nseg1.ts\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	tc := &singleUseRelayTranscoder{client: &http.Client{Timeout: 5 * time.Second}}
	s := testService(t, Options{HTTPClient: upstream.Client(), Transcoder: tc})
	ch, err := s.SaveChannel(context.Background(), Channel{
		Name:      "Single-Use IPTV",
		Number:    "42",
		Kind:      "iptv",
		Enabled:   true,
		StreamURL: upstream.URL + "/iptv/master.m3u8?token=once",
	})
	if err != nil {
		t.Fatalf("SaveChannel failed: %v", err)
	}
	src := s.orderedSourceCandidates(ch)[0]
	link, err := s.linkSteadySource(context.Background(), ch, src, "broadcast-iptv-1", 0)
	if err != nil {
		t.Fatalf("linkSteadySource failed: %v", err)
	}
	defer link.close(s)

	if _, err := s.probeSteadyLink(context.Background(), 2*time.Second, ch, link); err != nil {
		t.Fatalf("probeSteadyLink failed: %v", err)
	}
	// Decoder opens link.ss after probeSteadyLink transferred the cached master via retainProbedTap.
	linkInput, err := s.relayInputLive(link.ss)
	if err != nil {
		t.Fatalf("relayInputLive failed: %v", err)
	}
	res, err := tc.client.Get(linkInput)
	if err != nil {
		t.Fatalf("GET Live TV Steady link input failed: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "#EXTM3U") {
		t.Fatalf("Live TV Steady link status=%d body=%q", res.StatusCode, string(body))
	}
	if got := masterHits.Load(); got != 1 {
		t.Fatalf("Live TV single-use master fetched %d times, want 1", got)
	}
}

func TestDirectStreamReResolvesOnVariantExpiryAndDeduplicates(t *testing.T) {
	for _, expStatus := range []int{http.StatusForbidden, http.StatusGone} {
		name := "403"
		if expStatus == http.StatusGone {
			name = "410"
		}
		t.Run(name, func(t *testing.T) {
			var gen1MasterHits, gen2MasterHits atomic.Int32
			var expireGen1 atomic.Bool
			var gen2VariantSawFreshHeader atomic.Bool

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/gen1/master.m3u8":
					if gen1MasterHits.Add(1) > 1 {
						http.Error(w, "gen1 single-use token spent", http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
					_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"EN\",DEFAULT=YES,URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,CODECS=\"avc1.64001f,mp4a.40.2\",AUDIO=\"aud\"\nvariant.m3u8\n")
				case "/gen1/variant.m3u8", "/gen1/audio.m3u8":
					if expireGen1.Load() {
						http.Error(w, "expired", expStatus)
						return
					}
					w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
					_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:2.0,\nseg1.m4s\n")
				case "/gen2/master.m3u8":
					if r.Header.Get("X-Stream-Auth") != "tok-gen2" {
						http.Error(w, "missing refreshed header", http.StatusForbidden)
						return
					}
					if gen2MasterHits.Add(1) > 1 {
						http.Error(w, "gen2 single-use token spent", http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
					_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"EN\",DEFAULT=YES,URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,CODECS=\"avc1.64001f,mp4a.40.2\",AUDIO=\"aud\"\nvariant.m3u8\n")
				case "/gen2/variant.m3u8", "/gen2/audio.m3u8":
					if r.Header.Get("X-Stream-Auth") == "tok-gen2" {
						gen2VariantSawFreshHeader.Store(true)
					}
					w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
					_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:10\n#EXTINF:2.0,\nseg10.m4s\n")
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()

			var resolveCalls atomic.Int32
			resolveEntered := make(chan struct{})
			resolveRelease := make(chan struct{})

			tc := &singleUseRelayTranscoder{client: &http.Client{Timeout: 5 * time.Second}}
			s := testService(t, Options{HTTPClient: upstream.Client(), Transcoder: tc})
			u := &core.User{ID: 15, Username: "erin", StreamLimit: 2}
			caps := core.ClientCaps{
				Product:       "Vela",
				Protocol:      "vela",
				ClientID:      "vela-expire-" + name,
				VideoCodecs:   []string{"h264"},
				AudioCodecs:   []string{"aac"},
				HLSContainers: []string{"mpegts"},
			}

			info, err := s.TuneDirectStream(context.Background(), u, DirectStream{
				ID:      "dir:expire-" + name,
				Title:   "Expiring Direct Feed",
				URL:     upstream.URL + "/gen1/master.m3u8?token=once1",
				Headers: map[string]string{"X-Stream-Auth": "tok-gen1"},
				Resolve: func(context.Context) (DirectStreamResult, error) {
					if resolveCalls.Add(1) == 1 {
						close(resolveEntered)
						<-resolveRelease
					}
					return DirectStreamResult{
						URL:     upstream.URL + "/gen2/master.m3u8?token=once2",
						Headers: map[string]string{"X-Stream-Auth": "tok-gen2"},
					}, nil
				},
			}, caps, "vela-expire-sess-"+name)
			if err != nil {
				t.Fatalf("TuneDirectStream failed: %v", err)
			}
			defer func() { _ = s.Stop(context.Background(), info.ID, u) }()

			if gen1MasterHits.Load() != 1 || resolveCalls.Load() != 0 {
				t.Fatalf("after tune: gen1MasterHits=%d resolveCalls=%d", gen1MasterHits.Load(), resolveCalls.Load())
			}

			tc.mu.Lock()
			gen1VarRelayURL := firstManifestURI(tc.relayManifest)
			tc.mu.Unlock()

			// Expire gen1 variant; reading it through the relay marks direct expired.
			expireGen1.Store(true)
			expRes, err := tc.client.Get(gen1VarRelayURL)
			if err != nil {
				t.Fatalf("GET expired gen1 variant failed: %v", err)
			}
			_ = expRes.Body.Close()
			if expRes.StatusCode != expStatus {
				t.Fatalf("expired variant status=%d, want %d", expRes.StatusCode, expStatus)
			}

			s.mu.Lock()
			ss := s.sessions[info.ID]
			s.mu.Unlock()
			rootRelayURL, err := s.relayInput(ss)
			if err != nil {
				t.Fatalf("relayInput failed: %v", err)
			}

			// Concurrent root opens deduplicate into one Resolve call and one gen2 master GET.
			type fetchRes struct {
				status int
				body   string
				err    error
			}
			out := make([]fetchRes, 2)
			var wg sync.WaitGroup
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()
					res, err := tc.client.Get(rootRelayURL)
					if err != nil {
						out[idx] = fetchRes{err: err}
						return
					}
					b, _ := io.ReadAll(res.Body)
					_ = res.Body.Close()
					out[idx] = fetchRes{status: res.StatusCode, body: string(b)}
				}(i)
			}
			<-resolveEntered
			close(resolveRelease)
			wg.Wait()

			for i, r := range out {
				if r.err != nil || r.status != http.StatusOK || !strings.Contains(r.body, "#EXTM3U") {
					t.Fatalf("concurrent reader %d: status=%d err=%v body=%q", i, r.status, r.err, r.body)
				}
			}
			if got := resolveCalls.Load(); got != 1 {
				t.Fatalf("Resolve called %d times for concurrent expiry, want 1", got)
			}
			if got := gen1MasterHits.Load(); got != 1 {
				t.Fatalf("dead gen1 master re-fetched (%d hits), want 1", got)
			}
			if got := gen2MasterHits.Load(); got != 1 {
				t.Fatalf("fresh gen2 master fetched %d times, want 1", got)
			}

			// Variant from refreshed master works with refreshed headers.
			gen2VarRelayURL := firstManifestURI(out[0].body)
			resVar, err := tc.client.Get(gen2VarRelayURL)
			if err != nil {
				t.Fatalf("GET refreshed gen2 variant failed: %v", err)
			}
			_ = resVar.Body.Close()
			if resVar.StatusCode != http.StatusOK {
				t.Fatalf("refreshed gen2 variant status=%d, want 200", resVar.StatusCode)
			}
			if !gen2VariantSawFreshHeader.Load() {
				t.Fatal("refreshed gen2 header X-Stream-Auth=tok-gen2 was not sent")
			}

			// Another root open reuses cached gen2 master without re-fetching or re-resolving.
			res3, err := tc.client.Get(rootRelayURL)
			if err != nil {
				t.Fatalf("third root open failed: %v", err)
			}
			_ = res3.Body.Close()
			if res3.StatusCode != http.StatusOK {
				t.Fatalf("third root open status=%d, want 200", res3.StatusCode)
			}
			if gen2MasterHits.Load() != 1 || resolveCalls.Load() != 1 {
				t.Fatalf("unexpected refetch: gen2MasterHits=%d resolveCalls=%d", gen2MasterHits.Load(), resolveCalls.Load())
			}
		})
	}
}

func TestDirectStreamFailsCleanlyWithoutResolverOrOnResolveError(t *testing.T) {
	cases := []struct {
		name      string
		resolve   func(*atomic.Int32) func(context.Context) (DirectStreamResult, error)
		wantCalls int32
		wantCode  int
	}{
		{
			name:      "nil_resolver",
			resolve:   func(*atomic.Int32) func(context.Context) (DirectStreamResult, error) { return nil },
			wantCalls: 0,
			wantCode:  http.StatusForbidden,
		},
		{
			name: "resolver_error",
			resolve: func(calls *atomic.Int32) func(context.Context) (DirectStreamResult, error) {
				return func(context.Context) (DirectStreamResult, error) {
					calls.Add(1)
					return DirectStreamResult{}, core.ErrNotFound
				}
			},
			wantCalls: 1,
			wantCode:  http.StatusBadGateway,
		},
	}

	for _, tcCase := range cases {
		t.Run(tcCase.name, func(t *testing.T) {
			var masterHits atomic.Int32
			var expireVar atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/master.m3u8":
					if masterHits.Add(1) > 1 {
						http.Error(w, "token spent", http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
					_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"EN\",DEFAULT=YES,URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,CODECS=\"avc1.64001f,mp4a.40.2\",AUDIO=\"aud\"\nvariant.m3u8\n")
				case "/variant.m3u8", "/audio.m3u8":
					if expireVar.Load() {
						http.Error(w, "expired", http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
					_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:2.0,\nseg1.m4s\n")
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()

			var resolveCalls atomic.Int32
			tc := &singleUseRelayTranscoder{client: &http.Client{Timeout: 5 * time.Second}}
			s := testService(t, Options{HTTPClient: upstream.Client(), Transcoder: tc})
			u := &core.User{ID: 16, Username: "frank", StreamLimit: 2}
			caps := core.ClientCaps{
				Product:       "Vela",
				Protocol:      "vela",
				ClientID:      "vela-fail-" + tcCase.name,
				VideoCodecs:   []string{"h264"},
				AudioCodecs:   []string{"aac"},
				HLSContainers: []string{"mpegts"},
			}

			info, err := s.TuneDirectStream(context.Background(), u, DirectStream{
				ID:      "dir:fail-" + tcCase.name,
				Title:   "Failing Direct Feed",
				URL:     upstream.URL + "/master.m3u8?token=once",
				Resolve: tcCase.resolve(&resolveCalls),
			}, caps, "vela-fail-sess-"+tcCase.name)
			if err != nil {
				t.Fatalf("TuneDirectStream failed: %v", err)
			}
			defer func() { _ = s.Stop(context.Background(), info.ID, u) }()

			tc.mu.Lock()
			varURL := firstManifestURI(tc.relayManifest)
			tc.mu.Unlock()

			expireVar.Store(true)
			res, err := tc.client.Get(varURL)
			if err != nil {
				t.Fatalf("GET expired variant failed: %v", err)
			}
			_ = res.Body.Close()

			s.mu.Lock()
			ss := s.sessions[info.ID]
			s.mu.Unlock()
			rootURL, err := s.relayInput(ss)
			if err != nil {
				t.Fatalf("relayInput failed: %v", err)
			}

			res2, err := tc.client.Get(rootURL)
			if err != nil {
				t.Fatalf("GET root after expiry failed: %v", err)
			}
			_ = res2.Body.Close()
			if res2.StatusCode != tcCase.wantCode {
				t.Fatalf("status=%d, want %d", res2.StatusCode, tcCase.wantCode)
			}
			if got := resolveCalls.Load(); got != tcCase.wantCalls {
				t.Fatalf("resolveCalls=%d, want %d", got, tcCase.wantCalls)
			}
			if got := masterHits.Load(); got != 1 {
				t.Fatalf("dead single-use master re-fetched (%d hits), want 1", got)
			}
		})
	}
}

func TestDirectStreamPassthroughSingleUseMasterAndReResolveOnExpiry(t *testing.T) {
	var gen1MasterHits, gen2MasterHits atomic.Int32
	var resolveCalls atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gen1/master.m3u8":
			if gen1MasterHits.Add(1) > 1 {
				http.Error(w, "gen1 spent", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720,CODECS=\"avc1.64001f,mp4a.40.2\"\nvariant.m3u8\n")
		case "/gen2/master.m3u8":
			if gen2MasterHits.Add(1) > 1 {
				http.Error(w, "gen2 spent", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720,CODECS=\"avc1.64001f,mp4a.40.2\"\nvariant.m3u8\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	s := testService(t, Options{HTTPClient: upstream.Client()})
	raw1 := upstream.URL + "/gen1/master.m3u8?token=once1"
	direct := newDirectStreamState(context.Background(), raw1, nil, func(context.Context) (DirectStreamResult, error) {
		resolveCalls.Add(1)
		return DirectStreamResult{URL: upstream.URL + "/gen2/master.m3u8?token=once2"}, nil
	})
	tl := newOwnedTimeline("direct-pt", s.logger(), nil, 0, nil)
	p := &passthroughProducer{
		s: s, tl: tl, c: Channel{ID: "direct-pt", Kind: "iptv"}, log: s.logger(),
		caps:      core.ClientCaps{HLSContainers: []string{"mpegts"}},
		src:       StreamSource{ID: "direct-pt", URL: raw1},
		raw:       raw1,
		ephemeral: true,
		direct:    direct,
	}

	if err := p.open(context.Background(), p.src, p.raw, p.headers, false); err != nil {
		t.Fatalf("initial passthrough open failed: %v", err)
	}
	if gen1MasterHits.Load() != 1 {
		t.Fatalf("gen1MasterHits=%d, want 1", gen1MasterHits.Load())
	}

	// Healthy reresolve reuses cached master without re-fetching or calling Resolve.
	if err := p.reresolve(context.Background()); err != nil {
		t.Fatalf("healthy reresolve failed: %v", err)
	}
	if gen1MasterHits.Load() != 1 || resolveCalls.Load() != 0 {
		t.Fatalf("healthy reresolve refetched: gen1MasterHits=%d resolveCalls=%d", gen1MasterHits.Load(), resolveCalls.Load())
	}

	// Expiry invalidates cached master; reresolve calls Resolve once and opens gen2.
	p.invalidateDirect(http.StatusForbidden)
	if err := p.reresolve(context.Background()); err != nil {
		t.Fatalf("expired reresolve failed: %v", err)
	}
	if gen1MasterHits.Load() != 1 || gen2MasterHits.Load() != 1 || resolveCalls.Load() != 1 {
		t.Fatalf("after expired reresolve: gen1=%d gen2=%d resolve=%d", gen1MasterHits.Load(), gen2MasterHits.Load(), resolveCalls.Load())
	}
	po, ok := p.ingest.origin.(*providerOrigin)
	if !ok || !strings.Contains(po.mediaURL, "/gen2/variant.m3u8") {
		t.Fatalf("expected gen2 variant URL on providerOrigin, got %#v", p.ingest.origin)
	}

	// Subsequent healthy reresolve reuses cached gen2 master.
	if err := p.reresolve(context.Background()); err != nil {
		t.Fatalf("second healthy reresolve failed: %v", err)
	}
	if gen2MasterHits.Load() != 1 || resolveCalls.Load() != 1 {
		t.Fatalf("gen2 reresolve refetched: gen2=%d resolve=%d", gen2MasterHits.Load(), resolveCalls.Load())
	}
}

func TestClassifySegmentDamageStderr(t *testing.T) {
	cases := []struct {
		name        string
		stderr      string
		wantDamaged bool
		wantSample  string
	}{
		{
			name:        "empty",
			stderr:      "",
			wantDamaged: false,
		},
		{
			name: "duplicated_moov_repeated",
			stderr: strings.Join([]string{
				"[mov,mp4,m4a,3gp,3g2,mj2 @ 0x600003c10000] Found duplicated MOOV Atom. Skipped it",
				"[mov,mp4,m4a,3gp,3g2,mj2 @ 0x600003c10000] Found duplicated MOOV Atom. Skipped it",
				"[in#0/mov,mp4,m4a,3gp,3g2,mj2 @ 0x600003c10100] Found duplicated MOOV Atom. Skipped it",
				"Last message repeated 4 times",
			}, "\n") + "\n",
			wantDamaged: false,
		},
		{
			name: "benign_libva_discontinuity_and_null_muxer_dts",
			stderr: strings.Join([]string{
				"libva info: VA-API version 1.20.0",
				"libva info: Trying to open /usr/lib/x86_64-linux-gnu/dri/iHD_drv_video.so",
				"[mpegts @ 0x1] Timestamp discontinuity on stream 0",
				"[null @ 0x2] Application provided invalid, non monotonically increasing dts to muxer in stream 0: 12345 >= 12345",
				"Last message repeated 2 times",
			}, "\n") + "\n",
			wantDamaged: false,
		},
		{
			name:        "single_real_error_below_threshold",
			stderr:      "[h264 @ 0x1] error while decoding MB 12 8, bytestream -5\n",
			wantDamaged: false,
			wantSample:  "[h264 @ 0x1] error while decoding MB 12 8, bytestream -5",
		},
		{
			name: "real_h264_decode_damage",
			stderr: strings.Join([]string{
				"[h264 @ 0x1] cabac decode of qscale diff failed at 10 4",
				"[h264 @ 0x1] error while decoding MB 10 4, bytestream 1421",
			}, "\n") + "\n",
			wantDamaged: true,
			wantSample:  "[h264 @ 0x1] cabac decode of qscale diff failed at 10 4",
		},
		{
			name: "real_error_plus_last_message_repeated",
			stderr: strings.Join([]string{
				"[hevc @ 0x1] Could not find ref with POC 14",
				"Last message repeated 3 times",
			}, "\n") + "\n",
			wantDamaged: true,
			wantSample:  "[hevc @ 0x1] Could not find ref with POC 14",
		},
		{
			name: "real_mpegts_packet_corrupt",
			stderr: strings.Join([]string{
				"[mpegts @ 0x1] PES packet size mismatch",
				"[mpegts @ 0x1] Packet corrupt (stream = 0, dts = 90000).",
			}, "\n") + "\n",
			wantDamaged: true,
			wantSample:  "[mpegts @ 0x1] PES packet size mismatch",
		},
		{
			name: "mixed_benign_moov_and_real_damage",
			stderr: strings.Join([]string{
				"[mov,mp4,m4a,3gp,3g2,mj2 @ 0x1] Found duplicated MOOV Atom. Skipped it",
				"Last message repeated 2 times",
				"[h264 @ 0x2] concealing 450 DC, 450 AC, 450 MV errors in P frame",
				"[h264 @ 0x2] reference picture missing during reorder",
			}, "\n") + "\n",
			wantDamaged: true,
			wantSample:  "[h264 @ 0x2] concealing 450 DC, 450 AC, 450 MV errors in P frame",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotDamaged, gotSample := classifySegmentDamageStderr(tc.stderr)
			if gotDamaged != tc.wantDamaged {
				t.Fatalf("damaged=%v, want %v (sample=%q)", gotDamaged, tc.wantDamaged, gotSample)
			}
			if tc.wantSample != "" && gotSample != tc.wantSample {
				t.Fatalf("sample=%q, want %q", gotSample, tc.wantSample)
			}

			// Verify streaming chunked writes produce the exact same result.
			var scan segmentDamageScanner
			raw := []byte(tc.stderr)
			for i := 0; i < len(raw); i += 7 {
				end := min(i+7, len(raw))
				_, _ = scan.Write(raw[i:end])
			}
			chunkDamaged, chunkSample := scan.result()
			if chunkDamaged != gotDamaged || chunkSample != gotSample {
				t.Fatalf("chunked result=(%v, %q), want (%v, %q)", chunkDamaged, chunkSample, gotDamaged, gotSample)
			}
		})
	}
}

func TestTapRegistryAcquireOrManifestReturnsCachedAfterTapCloses(t *testing.T) {
	const raw = "http://provider.example/master.m3u8?token=once"
	masterBody := []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=2500000\nvariant.m3u8\n")
	cm := newCachedManifest(raw, masterBody, nil, true)

	// 1. After a manifest tap caches its master and closes (forgetting itself
	//    from r.byKey), a racing reader entering acquireOrManifest must get the
	//    cached manifest instead of creating a second tap (or tripping a full
	//    provider connection limit).
	var reg tapRegistry
	direct := newDirectStreamState(context.Background(), raw, nil, nil)
	g := &inputGrant{direct: direct}
	t1, gotCM, created, err := reg.acquireOrManifest("k1", "src", 1, g, func() *cachedManifest {
		return g.consumeManifest(raw)
	})
	if err != nil || !created || t1 == nil || gotCM != nil {
		t.Fatalf("first acquire: t=%v cm=%v created=%v err=%v", t1, gotCM, created, err)
	}
	g.setManifest(cm)
	t1.close(nil)

	// Exhaust the provider's connection limit to prove the cached manifest
	// short-circuits before creating a tap or checking capacity.
	if _, _, err := reg.acquire("other", "src", 1, &inputGrant{}); err != nil {
		t.Fatalf("occupy provider slot: %v", err)
	}
	t2, gotCM, created, err := reg.acquireOrManifest("k1", "src", 1, g, func() *cachedManifest {
		return g.consumeManifest(raw)
	})
	if err != nil || created || t2 != nil || gotCM == nil || gotCM.raw != raw {
		t.Fatalf("post-close acquire: t=%v cm=%v created=%v err=%v", t2, gotCM, created, err)
	}

	// 2. While a closed tap is still present in r.byKey (the window between
	//    t.closed = true and r.forget(t)), acquireOrManifest must likewise
	//    return the cached manifest rather than allocating a replacement tap.
	var reg2 tapRegistry
	g2 := &inputGrant{direct: newDirectStreamState(context.Background(), raw, nil, nil)}
	stale := &upstreamTap{key: "k2", reg: &reg2, closed: true, refs: map[*inputGrant]struct{}{}}
	reg2.byKey = map[string]*upstreamTap{"k2": stale}
	reg2.bySource = map[string]map[*upstreamTap]struct{}{}
	g2.setManifest(cm)

	t3, gotCM, created, err := reg2.acquireOrManifest("k2", "", 0, g2, func() *cachedManifest {
		return g2.peekManifest(raw)
	})
	if err != nil || created || t3 != nil || gotCM == nil || gotCM.raw != raw {
		t.Fatalf("stale-tap acquire: t=%v cm=%v created=%v err=%v", t3, gotCM, created, err)
	}
}
