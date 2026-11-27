package vela

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

type qualityFixture struct {
	sessionFake
	prepared int
}

func (*qualityFixture) ProactiveFallbackAvailable() bool { return true }
func (*qualityFixture) QualityRungs(context.Context, frontend.SessionAccess, string) ([]frontend.HLSRung, error) {
	return []frontend.HLSRung{{ID: "720p4m", Width: 1280, Height: 720, Bandwidth: 4800000}, {ID: "480p1500k", Width: 854, Height: 480, Bandwidth: 1800000}}, nil
}
func (f *qualityFixture) PrepareQuality(_ context.Context, _ frontend.SessionAccess, _ string, rung string, p time.Duration) (string, time.Duration, error) {
	f.prepared++
	if rung != "480p1500k" {
		panic("bitrate cap ignored")
	}
	return "/sessions/media/session/grant/master.m3u8", p, nil
}
func TestQualitySessionInvariants(t *testing.T) {
	f := &qualityFixture{}
	var starts []frontend.StreamRequest
	f.StartStreamFunc = func(_ context.Context, _ frontend.SessionAccess, _ string, r frontend.StreamRequest) (string, error) {
		starts = append(starts, r)
		if r.Format == frontend.StreamDirect {
			return "/sessions/media/session/grant/file", nil
		}
		return "/sessions/media/session/grant/master.m3u8", nil
	}
	s := &playSession{scope: "fake-family", viewer: 1, device: "device", source: 1, seek: 1, position: 90 * time.Second, signal: make(chan struct{}), plan: object{"sessionId": "session", "selectedPathId": "primary", "paths": []object{{"id": "primary", "original": true, "transport": "file", "url": "/sessions/media/session/grant/file", "readiness": "ready", "sourceId": "pinned", "timelineId": "pinned"}}}}
	i := &instance{host: frontend.Host{Identity: identityFake{}, Sessions: f}, sessions: map[string]*playSession{"session": s}}
	i.addQualityPaths(context.Background(), frontend.SessionAccess{}, "session", s)
	paths := s.plan["paths"].([]object)
	if len(paths) != 3 || paths[1]["url"] != nil || paths[1]["readiness"] != "on-demand" || len(starts) != 0 || f.prepared != 0 {
		t.Fatal("eager quality work", s.plan)
	}
	call := func(b object) *httptest.ResponseRecorder {
		b["sourceGeneration"], b["seekGeneration"] = s.source, s.seek
		raw, _ := json.Marshal(b)
		r := httptest.NewRequest("POST", "/", nil)
		r.SetPathValue("id", "session")
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("X-Vela-Device-Id", "device")
		r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, json.RawMessage(raw)))
		w := httptest.NewRecorder()
		i.command(w, r)
		return w
	}
	if w := call(object{"kind": "prepare-fallback", "maxBitrate": 2000000}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if f.prepared != 1 || s.source != 1 || s.seek != 1 || s.plan["selectedPathId"] != "primary" || s.plan["fallback"].(object)["state"] != "ready" {
		t.Fatal("preparation changed playback", s.plan)
	}
	if w := call(object{"kind": "switch", "maxBitrate": 2000000, "atSeconds": 95}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if s.source != 2 || s.seek != 2 || s.position != 95*time.Second || paths[2]["timelineOriginSeconds"] != 90.0 || starts[0].Position != 90*time.Second || paths[0]["url"] == nil {
		t.Fatal("prepared switch lost clock/original", s.plan)
	}
	if w := call(object{"kind": "switch", "pathId": "primary", "atSeconds": 96}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if s.source != 3 || s.seek != 3 || starts[1].Format != frontend.StreamDirect || s.position != 96*time.Second {
		t.Fatal("original switch", s.plan)
	}
}
