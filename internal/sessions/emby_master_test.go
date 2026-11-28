package sessions

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"testing"
)

func TestEmbyMasterMetadataRequiresProvenIndependence(t *testing.T) {
	original := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nbase/index.m3u8\n"
	got := embyMasterMetadata(original, 3, false)
	if !strings.HasPrefix(got, "#EXTM3U\n#EXT-X-VERSION:3\n") || strings.Contains(got, "#EXT-X-INDEPENDENT-SEGMENTS") {
		t.Fatalf("unknown independence: %q", got)
	}
	got = embyMasterMetadata(original, 7, true)
	if !strings.Contains(got, "#EXT-X-INDEPENDENT-SEGMENTS\n") || !strings.HasSuffix(got, "base/index.m3u8\n") {
		t.Fatalf("proven independence: %q", got)
	}
}

type provenLadder struct{ fakeLadderSession }

func (*provenLadder) IndependentSegments() bool { return true }

func TestBrokerEmbyMasterUsesSessionGuarantee(t *testing.T) {
	play := &fakePlayback{sessions: map[string]*core.PlaySession{"p": {ID: "p"}}}
	bindings := NewBindingTable[frontend.PlayIntent]()
	bindings.Put(&Binding[frontend.PlayIntent]{ID: "s", Decision: core.Decision{Container: "mpegts"}})
	broker := &Broker{deps: BrokerDeps{SurfacePolicy: testSessionPolicy, Playback: play}, bindings: bindings}
	session := &brokerSession{public: frontend.Session{ID: "s", Protocol: "emby"}, playID: "p"}
	for _, tc := range []struct {
		name    string
		session core.HLSSession
		want    bool
	}{{"unknown", &fakeLadderSession{}, false}, {"proven", &provenLadder{}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			broker.serveMaster(w, httptest.NewRequest(http.MethodGet, "/", nil), session, tc.session)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "#EXT-X-VERSION:3\n") || strings.Contains(w.Body.String(), "#EXT-X-INDEPENDENT-SEGMENTS\n") != tc.want || w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
				t.Fatalf("response: %d %q headers=%v", w.Code, w.Body.String(), w.Header())
			}
		})
	}
	bindings.Put(&Binding[frontend.PlayIntent]{ID: "s", Decision: core.Decision{Container: "fmp4"}})
	w := httptest.NewRecorder()
	broker.serveMaster(w, httptest.NewRequest(http.MethodHead, "/", nil), session, &fakeLadderSession{})
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD: %d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	broker.serveMaster(w, httptest.NewRequest(http.MethodGet, "/", nil), session, &fakeLadderSession{})
	if !strings.Contains(w.Body.String(), "#EXT-X-VERSION:7\n") {
		t.Fatalf("fMP4 version: %q", w.Body.String())
	}
	session.public.Protocol = "plex"
	w = httptest.NewRecorder()
	broker.serveMaster(w, httptest.NewRequest(http.MethodGet, "/", nil), session, &provenLadder{})
	if strings.Contains(w.Body.String(), "#EXT-X-VERSION") || strings.Contains(w.Body.String(), "#EXT-X-INDEPENDENT-SEGMENTS") {
		t.Fatalf("Plex master changed: %q", w.Body.String())
	}
}

type adaptiveTestLadder struct{ fakeLadderSession }

func (*adaptiveTestLadder) AdaptiveRungs() []core.HLSRung {
	return []core.HLSRung{
		{ID: "base", Bandwidth: 34000000, AverageBandwidth: 25000000, Width: 1920, Height: 1080, FrameRate: 23.976, Codecs: "hvc1.2.4.L150.90,ec-3", DynamicRange: "hdr10"},
		{ID: "720p", Bandwidth: 4600000, AverageBandwidth: 4000000, Width: 1280, Height: 720, FrameRate: 23.976, Codecs: "hvc1.2.4.L120.90,ec-3", DynamicRange: "hdr10"},
	}
}

func TestBrokerAdaptiveHLSMaster(t *testing.T) {
	play := &fakePlayback{sessions: map[string]*core.PlaySession{"p": {ID: "p"}}}
	bindings := NewBindingTable[frontend.PlayIntent]()
	bindings.Put(&Binding[frontend.PlayIntent]{ID: "s", Decision: core.Decision{Container: "fmp4"}})
	broker := &Broker{deps: BrokerDeps{SurfacePolicy: testSessionPolicy, Playback: play}, bindings: bindings}
	for _, surface := range []frontend.Protocol{"vela", "jellyfin", "emby"} {
		s := &brokerSession{public: frontend.Session{ID: "s", Protocol: surface}, playID: "p", adaptiveHLS: true}
		w := httptest.NewRecorder()
		broker.serveMaster(w, httptest.NewRequest(http.MethodGet, "/", nil), s, &adaptiveTestLadder{})
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.HasPrefix(body, "#EXTM3U\n") || strings.Count(body, "#EXT-X-STREAM-INF:") != 2 ||
			!strings.Contains(body, "BANDWIDTH=34000000,AVERAGE-BANDWIDTH=25000000,RESOLUTION=1920x1080,CODECS=\"hvc1.2.4.L150.90,ec-3\",FRAME-RATE=23.976,VIDEO-RANGE=PQ\nbase/index.m3u8\n") ||
			!strings.Contains(body, "BANDWIDTH=4600000,AVERAGE-BANDWIDTH=4000000,RESOLUTION=1280x720,CODECS=\"hvc1.2.4.L120.90,ec-3\",FRAME-RATE=23.976,VIDEO-RANGE=PQ\n720p/index.m3u8\n") {
			t.Fatalf("%s master: %d %s", surface, w.Code, body)
		}
	}
	capped := &brokerSession{public: frontend.Session{ID: "s", Protocol: "vela"}, playID: "p", adaptiveHLS: true}
	w := httptest.NewRecorder()
	broker.serveMaster(w, httptest.NewRequest(http.MethodGet, "/?maxBitrate=5000000", nil), capped, &adaptiveTestLadder{})
	if w.Code != http.StatusOK || strings.Count(w.Body.String(), "#EXT-X-STREAM-INF:") != 1 ||
		!strings.Contains(w.Body.String(), "720p/index.m3u8") {
		t.Fatalf("capped master: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	broker.serveMaster(w, httptest.NewRequest(http.MethodGet, "/?maxBitrate=400000", nil), capped, &adaptiveTestLadder{})
	if w.Code != http.StatusOK || strings.Count(w.Body.String(), "#EXT-X-STREAM-INF:") != 1 ||
		!strings.Contains(w.Body.String(), "720p/index.m3u8") {
		t.Fatalf("master below the bitrate floor: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	broker.serveMaster(w, httptest.NewRequest(http.MethodGet, "/?start=low", nil), capped, &adaptiveTestLadder{})
	if w.Code != http.StatusOK || strings.Count(w.Body.String(), "#EXT-X-STREAM-INF:") != 2 ||
		strings.Index(w.Body.String(), "720p/index.m3u8") >= strings.Index(w.Body.String(), "base/index.m3u8") {
		t.Fatalf("start-low master: %d %s", w.Code, w.Body.String())
	}
	s := &brokerSession{public: frontend.Session{ID: "s", Protocol: "vela"}, playID: "p"}
	w = httptest.NewRecorder()
	broker.serveMaster(w, httptest.NewRequest(http.MethodGet, "/", nil), s, &adaptiveTestLadder{})
	if w.Code != http.StatusOK || strings.Count(w.Body.String(), "#EXT-X-STREAM-INF:") != 1 {
		t.Fatalf("Vela without opt-in: %d %s", w.Code, w.Body.String())
	}
}
