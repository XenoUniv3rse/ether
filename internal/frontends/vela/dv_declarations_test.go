package vela

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/sessions"
)

// dvHarness plays through the Vela handlers with the declared capabilities
// built as the host builds them: registry profiles, then forced owner rules.
type dvHarness struct {
	i         *instance
	plans     []frontend.PlanRequest
	overrides []core.ClientCapabilityOverride
}

func newDVHarness() *dvHarness {
	h := &dvHarness{}
	broker := frontendtest.Sessions{
		PlanFunc: func(_ context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
			h.plans = append(h.plans, req)
			return frontend.PlayPlan{ID: "plan", ETag: "src", HLS: true, Media: core.MediaInfo{Container: "mkv"}, Decision: core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioStream: -1, SubtitleStream: -1}}, nil
		},
		OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
			return frontend.Session{ID: "session"}, nil
		},
		StartStreamFunc: func(context.Context, frontend.SessionAccess, string, frontend.StreamRequest) (string, error) {
			return "/sessions/media/session/grant/master.m3u8", nil
		},
	}
	declare := frontendtest.Capabilities{BuildFunc: func(ctx context.Context, req frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
		c, err := caps.Builder{}.Build(ctx, req.Device, req.Profile, req.ProtocolInfo, req.UserAgent)
		if len(c.DVProfiles) == 0 { // the host fills undeclared profiles from the registry
			c.DVProfiles = frontend.ClientPolicyFor(frontend.ApplyForcedOverrides(c, "", h.overrides)).DolbyVisionProfiles
		}
		return frontend.CapabilityResult{Client: frontend.ApplyForcedOverrides(c, "", h.overrides)}, err
	}}
	h.i = &instance{ctx: context.Background(), host: frontend.Host{Identity: identityFake{}, Sessions: broker, Capabilities: declare}, sessions: map[string]*playSession{}}
	return h
}

func (h *dvHarness) post(path string, b object) (*httptest.ResponseRecorder, map[string]any) {
	raw, _ := json.Marshal(b)
	r := httptest.NewRequest("POST", "/vela/v0"+path, nil)
	r.Header.Set("X-Vela-Device-Id", "apple-tv")
	r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, json.RawMessage(raw)))
	w := httptest.NewRecorder()
	if path == "/negotiate" {
		h.i.negotiate(w, r) // signed out, as a first launch negotiates
	} else {
		r.Header.Set("Authorization", "Bearer access")
		h.i.playback(w, r)
	}
	var doc map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &doc)
	return w, doc
}

// dvClient is Vela's declaration as its Apple probe sends it, with the Dolby
// Vision and audio-output members set (or left out) by the caller.
func dvClient(hdr []string, extra map[string]any) object {
	c := object{"protocolVersions": []int{0}, "device": object{"id": "apple-tv", "name": "Den", "platform": "tvos", "appVersion": "0.2.0"}, "hls": true, "mse": false, "containers": []string{"mp4"}, "videoCodecs": []string{"h264", "hevc"}, "audioCodecs": []string{"aac", "ac3", "eac3"}, "subtitleFormats": []string{"webvtt"}, "hdr": hdr}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

var dvDeclarationShapes = []struct {
	name  string
	hdr   []string
	extra map[string]any
}{
	{"absent", []string{"hdr10", "hlg", "dv"}, nil},
	{"empty", []string{"hdr10", "hlg", "dv"}, map[string]any{"dolbyVisionProfiles": []string{}}},
	{"8.1", []string{"hdr10", "hlg", "dv"}, map[string]any{"dolbyVisionProfiles": []string{"8.1"}}},
	{"8.4", []string{"hdr10", "hlg", "dv"}, map[string]any{"dolbyVisionProfiles": []string{"8.4"}}},
	{"reshape-only", []string{}, map[string]any{"dolbyVisionReshape": []string{"5"}, "audioOutput": map[string]any{"passthrough": []string{}, "immersive": []string{"eac3-joc"}, "maxChannels": 2, "lossless": false}}},
	{"apple-dv", []string{"hdr10", "hlg", "dv"}, map[string]any{"dolbyVisionProfiles": []string{"5", "7", "8.1", "8.4"}, "audioOutput": map[string]any{"passthrough": []string{"ac3", "eac3", "eac3-joc"}, "immersive": []string{"eac3-joc", "truehd-atmos"}, "maxChannels": 12, "maxSampleRate": 48000, "lossless": true}}},
}

// dvMembers keeps only the declaration members this test follows; an
// omitted member stays omitted and an empty list stays an empty list.
func dvMembers(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]any
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for _, k := range []string{"dolbyVisionProfiles", "dolbyVisionReshape", "audioOutput"} {
		if v, ok := all[k]; ok {
			out[k] = v
		}
	}
	return out
}

// Negotiation echoes the Dolby Vision and audio-output members exactly, and
// detail, versions and Play keep them, so omitted (an older client) and an
// explicit empty list stay distinct.
func TestVelaNegotiatedDVVariants(t *testing.T) {
	for _, tc := range dvDeclarationShapes {
		t.Run(tc.name, func(t *testing.T) {
			h := newDVHarness()
			sent := dvClient(tc.hdr, tc.extra)
			want := dvMembers(t, sent)
			w, doc := h.post("/negotiate", object{"client": sent})
			if w.Code != 200 {
				t.Fatalf("negotiate %d %s", w.Code, w.Body.String())
			}
			if got := dvMembers(t, doc["client"]); !reflect.DeepEqual(got, want) {
				t.Fatalf("negotiate echo = %v, want %v", got, want)
			}
			r := httptest.NewRequest("GET", "/vela/v0/titles/x/versions", nil)
			r.Header.Set("X-Vela-Device-Id", "apple-tv")
			if got := dvMembers(t, h.i.negotiatedClientFor(r, requestDevice(r))); !reflect.DeepEqual(got, want) {
				t.Fatalf("detail/versions declaration = %v, want %v", got, want)
			}
			w, _ = h.post("/playback", object{"titleId": ids.RatingKeyGUID(core.MovieKey(1)), "positionSeconds": 0.0, "client": sent})
			if w.Code != 200 {
				t.Fatalf("playback %d %s", w.Code, w.Body.String())
			}
			if got := dvMembers(t, h.i.sessions["session"].client); !reflect.DeepEqual(got, want) {
				t.Fatalf("playback declaration = %v, want %v", got, want)
			}
		})
	}
}

// dvSource is a 2160p HEVC Dolby Vision Matroska file: profile 5 has no base
// layer, 7.6 and 8.1 an HDR10 base, 8.4 an HLG base.
func dvSource(variant string) core.MediaInfo {
	v := core.StreamInfo{Type: "video", Index: 0, Codec: "hevc", Profile: "Main 10", Width: 3840, Height: 2160, BitDepth: 10, DolbyVision: true, DolbyVisionLevel: 6}
	switch variant {
	case "5":
		v.DolbyVisionProfile = 5
	case "7.6":
		v.DolbyVisionProfile, v.DolbyVisionCompatibilityID, v.ColorTransfer = 7, 6, "smpte2084"
	case "8.1":
		v.DolbyVisionProfile, v.DolbyVisionCompatibilityID, v.ColorTransfer = 8, 1, "smpte2084"
	case "8.4":
		v.DolbyVisionProfile, v.DolbyVisionCompatibilityID, v.ColorTransfer = 8, 4, "arib-std-b67"
	}
	return core.MediaInfo{Container: "mkv", BitrateKbps: 20_000, Streams: []core.StreamInfo{v, {Type: "audio", Index: 1, Codec: "eac3", Channels: 6}}}
}

func (h *dvHarness) playCaps(t *testing.T, sent object) frontend.ClientCaps {
	t.Helper()
	h.plans = nil
	if w, _ := h.post("/playback", object{"titleId": ids.RatingKeyGUID(core.MovieKey(1)), "positionSeconds": 0.0, "client": sent}); w.Code != 200 {
		t.Fatalf("playback %d %s", w.Code, w.Body.String())
	}
	return h.plans[0].Caps
}

// dvDecider is the transcoder's decision, without FFmpeg.
type dvDecider struct{ core.Transcoder }

func (dvDecider) Decide(i core.MediaInfo, c core.ClientCaps, o core.DecideOptions) core.Decision {
	return frontend.DecideTranscode(i, c, o, frontend.TranscodePolicy{})
}

func (dvDecider) HWAccel() core.HWAccel { return core.HWAccel{} }

// dvBrokerPlayback prepares an on-demand source with the given media.
type dvBrokerPlayback struct {
	*mvBrokerPlaybackFake
	info core.MediaInfo
}

func (p dvBrokerPlayback) Prepare(ctx context.Context, u *core.User, key core.RatingKey, c core.ClientCaps) (*core.PlaySession, error) {
	ps, err := p.mvBrokerPlaybackFake.Prepare(ctx, u, key, c)
	if ps != nil {
		ps.Info = p.info
		ps.Resolution = &core.Resolution{Release: core.Release{ID: "usenet:dv"}, HealthVerified: true}
		ps.Source = dvRevisionSource{}
	}
	return ps, err
}

func (dvBrokerPlayback) PinSourceRevision(context.Context, string, string) error { return nil }
func (dvBrokerPlayback) UnpinSourceRevision(string)                              {}

func (p dvBrokerPlayback) PrepareSurface(ctx context.Context, u *core.User, key core.RatingKey, c core.ClientCaps, _ string, _ bool) (*core.PlaySession, error) {
	return p.Prepare(ctx, u, key, c)
}

func (dvBrokerPlayback) MarkSurfacePrepared(string) bool { return true }

// dvRevisionSource gives the prepared source a fixed revision, which an
// original-file start pins.
type dvRevisionSource struct{ core.MediaSource }

func (dvRevisionSource) SourceRevision() string { return "dv-revision" }

// brokerPlan plans the Play request the Vela handler sent through the real
// session broker, which applies its own capability steps before the decision.
func brokerPlan(t *testing.T, req frontend.PlanRequest, info core.MediaInfo) frontend.PlayPlan {
	t.Helper()
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := sessions.NewEventBus(nil, active)
	t.Cleanup(events.Close)
	broker, err := sessions.NewBroker(sessions.BrokerDeps{
		Events:       events,
		Playback:     dvBrokerPlayback{&mvBrokerPlaybackFake{}, info},
		Transcoder:   dvDecider{},
		Progress:     mvBrokerProgressFake{},
		ActiveViewer: active,
		CanPlay:      func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit: func(*core.Viewer) int { return 0 },
		OnTimeline:   func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: k, Type: core.TypeMovie, Title: "Movie"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { broker.Close(ctx) })
	plan, err := sessions.BindSurface(broker, "vela").Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// dvPlan is the broker's plan for one source as "direct", "copy" (Dolby
// Vision kept), "base" (HDR10 base copied without Dolby Vision) or
// "convert", plus whether the HLS master signals Dolby Vision.
func (h *dvHarness) dvPlan(t *testing.T, sent object, variant string) (string, bool, core.Decision) {
	t.Helper()
	h.playCaps(t, sent)
	info := dvSource(variant)
	d := brokerPlan(t, h.plans[0], info).Decision
	attrs := frontend.HLSVariantAttributes(info, d)
	signalled := strings.Contains(attrs, "dvh1") || strings.Contains(attrs, "SUPPLEMENTAL-CODECS")
	switch {
	case d.Kind == core.DecisionDirectPlay:
		return "direct", signalled, d
	case d.VideoCopy && d.StripDolbyVision:
		return "base", signalled, d
	case d.VideoCopy:
		return "copy", signalled, d
	}
	return "convert", signalled, d
}

// velaClientOn is a Vela declaration on another platform and device.
func velaClientOn(platform, name string, hdr []string, extra map[string]any) object {
	c := dvClient(hdr, extra)
	c["device"] = object{"id": "apple-tv", "name": name, "platform": platform, "appVersion": "0.2.0"}
	return c
}

// The registry lists no Dolby Vision profiles for Vela on generic Android or
// Android TV, so the declared "5" and "8.1" grant them: Profile 5 is copied
// rather than encoded and 8.1 keeps Dolby Vision. An omitted list plans as
// before; an empty list, one of only "8.4", or an owner Dolby Vision off
// copies none; and a list sent without "dv" in hdr changes nothing.
func TestVelaDeclaredDVProfilesOnAndroid(t *testing.T) {
	off := false
	dv := []string{"hdr10", "dv"}
	hls := map[string]any{"containers": []string{"mp4"}}
	listed := map[string]any{"containers": []string{"mp4"}, "dolbyVisionProfiles": []string{"5", "8.1"}}
	empty := map[string]any{"containers": []string{"mp4"}, "dolbyVisionProfiles": []string{}}
	hlgOnly := map[string]any{"containers": []string{"mp4"}, "dolbyVisionProfiles": []string{"8.4"}}
	forcedOff := []core.ClientCapabilityOverride{{Force: true, Product: "Vela", ProductExact: true, DolbyVision: &off}}
	for _, tc := range []struct {
		name      string
		hdr       []string
		extra     map[string]any
		overrides []core.ClientCapabilityOverride
		want      map[string]string
	}{
		{"declared", dv, listed, nil, map[string]string{"5": "copy", "8.1": "copy", "8.4": "convert"}},
		{"omitted", dv, hls, nil, map[string]string{"5": "convert", "8.1": "base", "8.4": "convert"}},
		{"empty", dv, empty, nil, map[string]string{"5": "convert", "8.1": "base", "8.4": "convert"}},
		{"8.4 only", dv, hlgOnly, nil, map[string]string{"5": "convert", "8.1": "base", "8.4": "convert"}},
		{"owner off", dv, listed, forcedOff, map[string]string{"5": "convert", "8.1": "base", "8.4": "convert"}},
		{"no dv in hdr", []string{"hdr10"}, listed, nil, map[string]string{"5": "convert", "8.1": "base", "8.4": "convert"}},
	} {
		for _, device := range [][2]string{{"android", "Pixel"}, {"androidtv", "Android TV"}} {
			t.Run(device[0]+"/"+tc.name, func(t *testing.T) {
				for variant, want := range tc.want {
					h := newDVHarness()
					h.overrides = tc.overrides
					got, signalled, d := h.dvPlan(t, velaClientOn(device[0], device[1], tc.hdr, tc.extra), variant)
					if got != want || signalled != (want == "copy") {
						t.Errorf("source %s: %s (%s), Dolby Vision signalled %v; want %s", variant, got, d.Reason, signalled, want)
					}
				}
			})
		}
	}
	h := newDVHarness()
	if with, without := h.playCaps(t, velaClientOn("android", "Pixel", []string{"hdr10"}, listed)), h.playCaps(t, velaClientOn("android", "Pixel", []string{"hdr10"}, hls)); !reflect.DeepEqual(with, without) {
		t.Errorf("a list without dv changed the capabilities: %v, want %v", with.DVProfiles, without.DVProfiles)
	}
	for _, none := range []map[string]any{empty, {"dolbyVisionProfiles": []string{"unknown"}}} {
		if with, without := h.playCaps(t, velaClientOn("android", "Pixel", dv, none)), h.playCaps(t, velaClientOn("android", "Pixel", dv, hls)); with.DVProfilesExplicit == without.DVProfilesExplicit || len(with.DVProfiles) != 0 {
			t.Errorf("list %v is not kept as declaring no profiles: %v %v", none["dolbyVisionProfiles"], with.DVProfiles, with.DVProfilesExplicit)
		}
	}
}

// Where the registry has profiles the declared list changes nothing: Apple's
// real list on tvOS, iOS and iPadOS plans exactly as no list, the Profile 7
// Matroska original among them, and a Shield keeps its registry profiles.
func TestVelaDeclaredDVProfilesKeepRegistry(t *testing.T) {
	original := object{"version": 1, "range": true, "demuxContainers": []string{"mkv", "mp4"}, "videoDecoders": []string{"hevc"}, "audioDecoders": []string{"eac3"}}
	hdr := []string{"hdr10", "hlg", "dv"}
	for _, platform := range []string{"tvos", "ios", "ipados"} {
		plain := velaClientOn(platform, "Den", hdr, map[string]any{"originalPlayback": original})
		apple := velaClientOn(platform, "Den", hdr, map[string]any{"originalPlayback": original, "dolbyVisionProfiles": []string{"5", "7", "8.1", "8.4"}})
		for _, variant := range []string{"5", "7.6", "8.1", "8.4"} {
			got, gotDV, gd := newDVHarness().dvPlan(t, apple, variant)
			want, wantDV, wd := newDVHarness().dvPlan(t, plain, variant)
			if got != want || gotDV != wantDV || gd.Reason != wd.Reason {
				t.Errorf("%s source %s: %s (%s), want %s (%s)", platform, variant, got, gd.Reason, want, wd.Reason)
			}
			if variant == "7.6" && (gd.Kind != core.DecisionDirectPlay || gd.Reason != "original_runtime") {
				t.Errorf("%s Profile 7 original not admitted unchanged: %s (%s)", platform, gd.Kind, gd.Reason)
			}
		}
	}
	h := newDVHarness()
	shield := func(extra map[string]any) object { return velaClientOn("android", "SHIELD Android TV", hdr, extra) }
	if with, without := h.playCaps(t, shield(map[string]any{"dolbyVisionProfiles": []string{"8.1"}})), h.playCaps(t, shield(nil)); !reflect.DeepEqual(with, without) || !reflect.DeepEqual(without.DVProfiles, []int{5, 8}) {
		t.Errorf("shield profiles %v, want %v as without a list", with.DVProfiles, without.DVProfiles)
	}
}

// A client with no registry profiles can declare an exact Profile 8 variant.
func TestVelaDeclaredDVProfilesOffAndroid(t *testing.T) {
	hdr := []string{"hdr10", "hlg", "dv"}
	mac := velaClientOn("macos", "Mac", hdr, map[string]any{"dolbyVisionProfiles": []string{"5", "8.1"}})
	if _, signalled, d := newDVHarness().dvPlan(t, mac, "8.1"); !signalled {
		t.Errorf("macOS declared 8.1 lost Dolby Vision: %s", d.Reason)
	}
	if _, signalled, d := newDVHarness().dvPlan(t, mac, "8.4"); signalled {
		t.Errorf("macOS received undeclared 8.4: %s", d.Reason)
	}
	apple := velaClientOn("tvos", "Den", hdr, map[string]any{"dolbyVisionProfiles": []string{}})
	for _, overrides := range [][]core.ClientCapabilityOverride{
		nil,
		{{Force: true, Product: "Vela", DolbyVisionProfiles: []int{5}}},
	} {
		h := newDVHarness()
		h.overrides = overrides
		if _, signalled, d := h.dvPlan(t, apple, "5"); signalled || d.VideoCopy {
			t.Errorf("an empty Apple list admitted Profile 5: %s", d.Reason)
		}
	}
}

// POST /playback is Vela's start request: its plan says a viewer pressed
// Play (the broker still leaves a queued next item unmarked).
func TestVelaPlaybackPlanIsAViewerPlay(t *testing.T) {
	h := newDVHarness()
	sent := dvClient([]string{"hdr10"}, nil)
	if w, _ := h.post("/negotiate", object{"client": sent}); w.Code != 200 {
		t.Fatalf("negotiate %d %s", w.Code, w.Body.String())
	}
	if w, _ := h.post("/playback", object{"titleId": ids.RatingKeyGUID(core.MovieKey(1)), "positionSeconds": 0.0, "client": sent}); w.Code != 200 {
		t.Fatalf("playback %d %s", w.Code, w.Body.String())
	}
	if len(h.plans) != 1 || !h.plans[0].Intent.ViewerPlay {
		t.Fatalf("plans %+v, want one with ViewerPlay", h.plans)
	}
}
