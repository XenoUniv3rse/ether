package vela

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

type adaptiveOutputFixture struct {
	outputFixture
	cap atomic.Int64
}

func (f *adaptiveOutputFixture) AdaptiveRungs() []core.HLSRung {
	if cap := f.cap.Load(); cap > 0 {
		return rungsAtOrBelow(f.rungs, int(cap))
	}
	return f.rungs
}

func (f *adaptiveOutputFixture) AdaptiveBitrateLimit() int { return int(f.cap.Load()) }

func TestVelaAdaptivePlaybackPaths(t *testing.T) {
	const reportedTitle = "000000000000000100038d7ea4cf6b8a"
	for _, route := range []string{"fresh", "resume_source", "remembered_choice", "original"} {
		t.Run(route, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			output := &adaptiveOutputFixture{outputFixture: outputFixture{rungs: []core.HLSRung{
				{ID: "base", Bandwidth: 6700000, Width: 1920, Height: 1080},
				{ID: "720p", Bandwidth: 4600000, Width: 1280, Height: 720},
				{ID: "480p", Bandwidth: 1800000, Width: 854, Height: 480},
			}}}
			var requests []frontend.StreamRequest
			sessions := frontendtest.Sessions{
				TranscodeForFunc: func(context.Context, string) (core.HLSSession, bool) { return output, true },
				PlanFunc: func(_ context.Context, _ frontend.PlanRequest) (frontend.PlayPlan, error) {
					decision := core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true}
					audio := "aac"
					if route == "original" {
						decision.Kind, decision.Reason = core.DecisionDirectPlay, "original_runtime"
						audio = "truehd"
					}
					plan := frontend.PlayPlan{ID: "plan", HLS: true, DirectStream: true, ETag: "edition", Decision: decision,
						Media: core.MediaInfo{Container: "mkv", Duration: time.Hour, Streams: []core.StreamInfo{
							{Type: "video", Codec: "h264", Width: 1920, Height: 1080},
							{Type: "audio", Codec: audio, Channels: 2},
						}}}
					if route == "resume_source" {
						plan.AutoSelectionReason = frontend.AutoResumeSource
					}
					return plan, nil
				},
				OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
					return frontend.Session{ID: "session"}, nil
				},
				StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, _ string, req frontend.StreamRequest) (string, error) {
					requests = append(requests, req)
					if req.Format == frontend.StreamDirect {
						return "/sessions/media/session/grant/file", nil
					}
					output.cap.Store(int64(req.AdaptiveMaxBitrate))
					return "/sessions/media/session/grant/master.m3u8", nil
				},
			}
			i := &instance{ctx: ctx, host: frontend.Host{Identity: identityFake{}, Sessions: sessions, Capabilities: frontendtest.Capabilities{}}, sessions: map[string]*playSession{}}
			call := func(handler http.HandlerFunc, payload object) *httptest.ResponseRecorder {
				raw, _ := json.Marshal(payload)
				r := httptest.NewRequest(http.MethodPost, "/", nil)
				r.SetPathValue("id", "session")
				r.Header.Set("Authorization", "Bearer token")
				r.Header.Set("X-Vela-Device-Id", "device")
				r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, json.RawMessage(raw)))
				w := httptest.NewRecorder()
				handler(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("%v: %d %s", payload["kind"], w.Code, w.Body.String())
				}
				return w
			}
			c := object{"device": object{"id": "device", "platform": "android"}, "hls": true, "videoCodecs": []string{"h264", "hevc"},
				"audioCodecs": []string{"aac", "ac3", "eac3"}, "maxWidth": 1920, "maxHeight": 1080, "features": object{"adaptiveHLS": true}}
			if route == "original" {
				c["device"].(object)["platform"] = "ios"
				c["originalPlayback"] = object{"version": 1, "range": true, "demuxContainers": []string{"mkv"}, "videoDecoders": []string{"h264"}, "audioDecoders": []string{"truehd", "aac"}}
			}
			payload := object{"titleId": reportedTitle, "positionSeconds": 90, "client": c}
			if route == "remembered_choice" {
				payload["versionId"] = "remembered"
			}
			if route == "remembered_choice" {
				i.host.Editions = &versionsEditionsFake{}
				i.host.VersionHandles = &versionsHandlesFake{}
				i.host.Experience = frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
					return frontend.ItemDetail{}, frontend.ErrNotFound
				}}
			}
			call(i.playback, payload)
			s := i.sessions["session"]
			assertPaths := func() []object {
				t.Helper()
				s.mu.Lock()
				defer s.mu.Unlock()
				paths := s.outputSnapshot().plan["paths"].([]object)
				for _, path := range paths {
					if path["transport"] != "hls" || path["adaptive"] == true {
						continue
					}
					why, _ := path["adaptiveUnavailable"].(object)
					if why["reason"] == nil || why["message"] == nil || why["message"] == "" {
						t.Fatalf("silent nonadaptive HLS path: %+v", path)
					}
				}
				return paths
			}
			paths := assertPaths()
			if !requests[len(requests)-1].AdaptiveHLS {
				t.Fatal("HLS request lost opt-in")
			}
			if route == "original" && (paths[0]["transport"] != "file" || len(paths) < 2 || paths[1]["id"] != adaptivePathID || paths[1]["adaptive"] != true || !requests[1].PrepareOnly) {
				t.Fatalf("original did not retain a separate adaptive master: %+v", paths)
			}
			checkMaster := func(paths []object, cap int) {
				t.Helper()
				for _, path := range paths {
					if path["id"] != adaptivePathID {
						continue
					}
					url, _ := path["url"].(string)
					rungs := path["qualities"].([]object)
					if !strings.Contains(url, "master.m3u8") || path["adaptive"] != true || len(rungs) != 2 {
						t.Fatalf("expected a capped adaptive master: %+v", path)
					}
					for _, rung := range rungs {
						if rung["bitrate"].(int) > cap {
							t.Fatalf("rendition exceeded cap: %+v", rung)
						}
					}
					return
				}
				t.Fatal("missing adaptive path")
			}
			call(i.command, object{"kind": "prepare-fallback", "maxBitrate": 5000000, "sourceGeneration": 1, "seekGeneration": 1})
			checkMaster(assertPaths(), 5000000)
			call(i.command, object{"kind": "switch", "maxBitrate": 5000000, "sourceGeneration": 1, "seekGeneration": 1})
			checkMaster(assertPaths(), 5000000)
			if req := requests[len(requests)-1]; req.AdaptiveMaxBitrate != 5000000 || req.PrepareOnly {
				t.Fatalf("switch did not commit the peak cap: %+v", req)
			}

			// Simulate the encoder tightening the master after down-failover.
			// Work through the same private refresh snapshot as GET/SSE.
			output.cap.Store(2000000)
			s.mu.Lock()
			copy := s.outputSnapshot()
			s.mu.Unlock()
			if !i.refreshOutput(ctx, "session", copy) {
				t.Fatal("down-failover did not change the output plan")
			}
			s.mu.Lock()
			s.applyOutput(copy)
			s.mu.Unlock()
			assertPaths()
		})
	}
}
