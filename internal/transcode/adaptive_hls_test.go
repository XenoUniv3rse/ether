package transcode

import (
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestAdaptiveHLSCopySwitchClock(t *testing.T) {
	avc := []string{"-c:v", "libx264", "-preset", "veryfast", "-sc_threshold", "0"}
	for _, fixture := range []struct {
		name, codec, audio, videoRange string
		channels                       string
		videoArgs                      []string
	}{
		{"h264-aac-webdl", "h264", "aac", "sdr", "2", avc},
		{"h264-dts-remux", "h264", "dca", "sdr", "6", avc},
		{"hevc-hdr10-truehd-remux", "hevc", "truehd", "hdr10", "6", []string{
			"-c:v", "libx265", "-preset", "ultrafast",
			"-x265-params", "pools=2:frame-threads=1:log-level=error:scenecut=0:open-gop=0:hdr10=1",
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			testAdaptiveHLSCopySwitchClock(t, fixture.codec, fixture.audio, fixture.videoRange, fixture.channels, fixture.videoArgs...)
		})
	}
}

func testAdaptiveHLSCopySwitchClock(t *testing.T, codec, audio, videoRange, channels string, encoderArgs ...string) {
	t.Helper()
	e := newRealEnv(t, WithoutHWDetection())
	filter := "scale=854:480"
	if videoRange == "hdr10" {
		filter += ",format=yuv420p10le,setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc"
	}
	args := append([]string{"-vf", filter, "-threads", "2", "-g", "48", "-c:a", audio, "-ac", channels, "-strict", "experimental"}, encoderArgs...)
	e.gen(t, "abr.mkv", 12, args...)
	info, err := e.m.Probe(t.Context(), e.url("abr.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	info.Keyframes = e.keyframeIndex(t, "abr.mkv")
	containers := []string{ContainerFMP4}
	if codec == "h264" && audio == "aac" {
		containers = append(containers, ContainerMPEGTS)
	}
	for _, container := range containers {
		t.Run(container, func(t *testing.T) {
			d := core.Decision{Kind: core.DecisionRemux, VideoCopy: true, VideoCodec: codec, AudioCopy: true, AudioCodec: info.Streams[1].Codec, AudioStream: 1, SubtitleStream: -1, Container: container}
			caps := core.ClientCaps{VideoCodecs: []string{codec}, AudioCodecs: []string{"aac", d.AudioCodec}, HDRFormats: []string{videoRange}}
			d, shared := AdaptiveAudio(*info, d, caps)
			if !shared || d.AudioCopy || d.AudioCodec != "aac" || !d.VideoCopy {
				t.Fatalf("unexpected shared audio decision: %+v", d)
			}
			variants, limits := AdaptiveQualityVariants(*info, d, caps, 0, 0, 0, e.m.AdaptiveQualitySupport(*info, d))
			var lower core.QualityVariant
			for _, q := range variants {
				if q.Rung.Height == 360 {
					lower = q
					break
				}
			}
			if codec == "h264" {
				lower.Decision.H264Level = 31
			}
			if lower.Rung.ID == "" {
				t.Fatalf("no lower rendition: %+v", limits)
			}
			// Give the source enough headroom to include the 360p rendition.
			info.BitrateKbps = max(info.BitrateKbps, 2500)
			hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{
				InputURL: e.url("abr.mkv"), Info: *info, Decision: d, SegmentDuration: 3 * time.Second,
				Continuous: true, LazyStart: true, AdaptiveHLS: true, QualityRungs: true,
				QualityRungIDs: []string{lower.Rung.ID}, QualityVariants: []core.QualityVariant{lower},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer hs.Close()
			s := hs.(*Session)
			rungs := s.AdaptiveRungs()
			if len(rungs) != 2 || rungs[0].ID != "base" || rungs[1].ID != lower.Rung.ID {
				t.Fatalf("unexpected adaptive ladder (%s): %+v", s.AdaptiveHLSUnavailable(), rungs)
			}
			for _, r := range rungs {
				if r.Codec != codec || r.DynamicRange != videoRange || !strings.HasSuffix(r.Codecs, ",mp4a.40.2") {
					t.Fatalf("unexpected variant declaration: %+v", r)
				}
			}
			s.adaptiveMaxKbps = lower.Rung.Bandwidth / 1000
			if got := s.AdaptiveRungs(); len(got) != 1 || got[0].ID != lower.Rung.ID {
				t.Fatalf("peak cap did not exclude base: %+v", got)
			}
			s.adaptiveMaxKbps = 0
			durations := func(id string) []string {
				w := httptest.NewRecorder()
				s.ServeRungPlaylist(w, httptest.NewRequest(http.MethodGet, "/", nil), id)
				var out []string
				for _, line := range strings.Split(w.Body.String(), "\n") {
					if strings.HasPrefix(line, "#EXTINF:") || strings.HasPrefix(line, "#EXT-X-DISCONTINUITY") {
						out = append(out, line)
					}
				}
				return out
			}
			if base, low := durations("base"), durations(lower.Rung.ID); len(base) < 3 || !slices.Equal(base, low) || s.runSeq != 0 {
				t.Fatalf("expected aligned, lazy lists: base=%v low=%v runs=%d", base, low, s.runSeq)
			}
			span := func(id string, n int, name string) (float64, float64, float64, float64) {
				fetch := func(segment int) []byte {
					w := httptest.NewRecorder()
					r := mediaRequest(s, httptest.NewRequest(http.MethodGet, "/", nil), segment)
					s.ServeRungSegment(w, r, id, segment)
					if w.Code != 200 {
						t.Fatalf("rung %s segment %d: %d %s", id, segment, w.Code, w.Body.String())
					}
					return w.Body.Bytes()
				}
				var init []byte
				if container == ContainerFMP4 {
					init = fetch(InitSegment)
				}
				body := fetch(n)
				start, end, key := e.videoSpanKey(t, container, init, body, name)
				if !key {
					t.Fatalf("%s starts without random access", name)
				}
				if videoRange == "hdr10" && name == "lower" {
					streams := e.probeFile(t, filepath.Join(e.dir, name+"."+segmentExt(container)))
					if len(streams) != 2 || streams[0].ColorTransfer != "smpte2084" || streams[0].PixelFormat != "yuv420p10le" || streams[1].CodecName != "aac" {
						t.Fatalf("expected 10-bit PQ and AAC: %+v", streams)
					}
				}
				audioStart, audioEnd := e.audioPacketSpan(t, append(append([]byte(nil), init...), body...), "audio-"+name+"."+segmentExt(container))
				t.Logf("%s video PTS %.6f..%.6f, audio PTS %.6f..%.6f", name, start, end, audioStart, audioEnd)
				return start, end, audioStart, audioEnd
			}
			_, p1, _, pa1 := span("base", 0, "base-opening")
			b0, b1, ba0, ba1 := span("base", 1, "base-before")
			l0, l1, la0, la1 := span(lower.Rung.ID, 1, "lower")
			u0, u1, ua0, _ := span("base", 2, "base-after")
			// MKV/MP4 edit lists and TS use different time bases. Compare the
			// presentation boundaries within Matroska's millisecond clock quantization.
			if container == ContainerFMP4 {
				for i, verified := range s.AdaptiveRungs() {
					if verified.OutputCodecs == "" || rungs[i].Codecs != verified.OutputCodecs {
						t.Fatalf("cold and authored codecs differ for %s: %q vs %q", verified.ID, rungs[i].Codecs, verified.OutputCodecs)
					}
				}
			}
			// Independently primed AAC can round a cut to adjacent access
			// units. A switch must not introduce drift beyond that rounding.
			const audioTolerance = 1024.0/48000 + .00001
			if math.Abs(pa1-la0) > audioTolerance || math.Abs(ba0-la0) > audioTolerance ||
				math.Abs(ba1-la1) > audioTolerance || math.Abs(la1-ua0) > audioTolerance {
				t.Fatalf("unaligned audio: opening ends %.6f, base %.6f..%.6f lower %.6f..%.6f next %.6f", pa1, ba0, ba1, la0, la1, ua0)
			}
			if math.Abs(p1-l0) > .0011 || math.Abs(b0-l0) > .0011 || math.Abs(b1-l1) > .0011 || math.Abs(l1-u0) > .0011 || u1 <= u0 {
				t.Fatalf("unaligned down/up switch: base %.6f..%.6f lower %.6f..%.6f next %.6f..%.6f", b0, b1, l0, l1, u0, u1)
			}
		})
	}
}

func TestAdaptiveHLSRetiresIdleRendition(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{ABRLadder: true, MaxTranscodes: 1})
	req := ladderEncodeRequest(t, "adaptive-idle", 2*time.Minute)
	req.LazyStart, req.Continuous, req.AdaptiveHLS = true, true, true
	s := startSession(t, m, req)
	req.SessionID = "adaptive-waiting"
	other := startSession(t, m, req)
	rungs := s.AdaptiveRungs()
	if len(rungs) < 2 || fr.startCount() != 0 {
		t.Fatalf("expected lazy ladder, got %d rungs and %d starts", len(rungs), fr.startCount())
	}
	fetch := func(session *Session) int {
		w := httptest.NewRecorder()
		r := mediaRequest(session, httptest.NewRequest(http.MethodGet, "/", nil), 0)
		session.ServeRungSegment(w, r, rungs[1].ID, 0)
		return w.Code
	}
	if code := fetch(s); code != http.StatusOK {
		t.Fatalf("first rendition fetch: %d", code)
	}
	if code := fetch(other); code != http.StatusTooManyRequests {
		t.Fatalf("concurrent rendition exceeded encoder capacity: %d", code)
	}
	s.mu.Lock()
	retired := s.cur != nil && s.cur.track == 1 &&
		s.retireIdleAdaptiveRunLocked(s.adaptiveLastRequest.Add(adaptiveRungIdle)) && s.cur == nil && s.resumeAt == -1
	s.mu.Unlock()
	if !retired {
		t.Fatal("idle rendition kept an encoder or scheduled a restart")
	}
}
