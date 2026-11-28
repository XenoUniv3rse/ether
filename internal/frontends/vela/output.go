package vela

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// refreshOutput reads only existing sessions. Codec claims come from real init
// bytes; planned and unused rungs never start work. The session must be
// unpublished or a private snapshot; never hold the live UI lock here.
func (i *instance) refreshOutput(ctx context.Context, id string, s *playSession) bool {
	if s.archive != nil {
		if s.archive.info.ChannelID != "" {
			return refreshStartOver(s, i.catchUpNow())
		}
		return i.refreshLiveWindow(ctx, s.archive.tuneID, s)
	}
	liveChanged := i.refreshLiveWindow(ctx, id, s)
	hs, ok := i.host.Sessions.TranscodeFor(ctx, id)
	paths, _ := s.plan["paths"].([]object)
	before, _ := json.Marshal(object{"paths": paths, "preparation": s.plan["preparation"]})
	for _, path := range paths {
		url, _ := path["url"].(string)
		hls := path["transport"] == "hls"
		masterURL, query, _ := strings.Cut(url, "?")
		master := hls && strings.HasSuffix(masterURL, "/master.m3u8")
		if s.client.Features.AdaptiveHLS && hls && path["id"] == adaptivePathID && path["readiness"] == "on-demand" && url == "" {
			path["adaptive"] = true
			delete(path, "adaptiveUnavailable")
			continue
		}
		handoff := master && path["id"] == adaptivePathID && s.plan["selectedPathId"] == adaptivePathID && s.plan["serverHandoff"] == "completed"
		if handoff {
			path["url"] = masterURL + "?start=low"
		}
		if s.client.Features.AdaptiveHLS && (hls || path["id"] == s.plan["selectedPathId"]) &&
			(!master || !ok || hs == nil) && path["adaptiveUnavailable"] == nil {
			reason := "hls_unavailable"
			switch {
			case path["original"] == true:
				reason = "original_selected"
			case s.plan["media"] == "audio":
				reason = "audio_only"
			case master:
				reason = "adaptive_not_started"
			case hls && s.rungFor(path) != "":
				reason = "fixed_quality_selected"
			case url == "":
				reason = "adaptive_not_started"
			case s.channelID != "":
				reason = "live_timeline"
			}
			delete(path, "adaptive")
			setAdaptiveUnavailable(path, reason, s.accessDevice.Language)
			if url != "" {
				slog.Debug("adaptive HLS unavailable", "session", id, "reason", reason)
			}
		}
		if !ok || hs == nil || path["transport"] != "hls" || url == "" {
			continue
		}
		if clock, ok := hs.(interface{ TimelineOrigin() time.Duration }); ok {
			path["timelineOriginSeconds"] = clock.TimelineOrigin().Seconds()
		}
		wasAdaptive := path["adaptive"] == true
		delete(path, "adaptive")
		var adaptiveRungs []frontend.HLSRung
		if s.client.Features.AdaptiveHLS && master {
			if adaptive, ok := hs.(frontend.AdaptiveHLSSession); ok {
				adaptiveRungs = adaptive.AdaptiveRungs()
			}
			if strings.HasPrefix(query, "maxBitrate=") {
				adaptiveRungs = frontend.AdaptiveRungsAtOrBelow(adaptiveRungs, pathBitrate(path))
				if len(adaptiveRungs) == 1 {
					path["maxBitrate"] = max(pathBitrate(path), adaptiveRungs[0].Bandwidth)
				}
			} else if capped, ok := hs.(interface{ AdaptiveBitrateLimit() int }); ok {
				if limit := capped.AdaptiveBitrateLimit(); limit > 0 {
					path["maxBitrate"] = limit
				} else if handoff {
					delete(path, "maxBitrate")
				}
			}
			if d, ok := hs.(interface{ Decision() frontend.Decision }); ok && d.Decision().Kind == frontend.DecisionTranscode ||
				len(adaptiveRungs) > 0 && adaptiveRungs[0].ID != "base" {
				path["kind"] = "transcode"
			}
			if len(adaptiveRungs) > 1 {
				path["adaptive"] = true
				delete(path, "adaptiveUnavailable")
			} else {
				reason := "adaptive_not_started"
				if diagnostics, ok := hs.(interface{ AdaptiveHLSUnavailable() string }); ok {
					if code := diagnostics.AdaptiveHLSUnavailable(); code != "" {
						reason = code
					}
				}
				if strings.HasPrefix(query, "maxBitrate=") {
					reason = "bitrate_limit"
				} else if s.channelID != "" {
					reason = "live_timeline"
				} else if s.plan["media"] == "audio" {
					reason = "audio_only"
				}
				previous, _ := path["adaptiveUnavailable"].(object)
				if (wasAdaptive || adaptiveRungs == nil) && previous["reason"] != reason {
					slog.Debug("adaptive HLS unavailable", "session", id, "reason", reason)
				}
				setAdaptiveUnavailable(path, reason, s.accessDevice.Language)
			}
		}
		if ladder, ok := hs.(interface{ Rungs() []frontend.HLSRung }); ok {
			qualities := []object{}
			delete(path, "initialRenditionId")
			base := hlsDirectory(masterURL)
			rungs := adaptiveRungs
			if rungs == nil {
				rungs = ladder.Rungs()
			}
			for _, r := range rungs {
				if r.Width <= 0 || r.Height <= 0 || r.Bandwidth <= 0 || r.ID == "" || strings.ContainsAny(r.ID, "/\\?#%\r\n") {
					continue
				}
				q := qualityTarget(r)
				q["id"], q["url"] = r.ID, base+r.ID+"/index.m3u8"
				if r.OutputCodecs != "" {
					codecs := strings.Split(r.OutputCodecs, ",")
					q["videoCodec"], q["codecStatus"] = codecs[0], "verified"
					if len(codecs) > 1 {
						q["audioCodec"] = codecs[1]
						if path["id"] == s.plan["selectedPathId"] {
							tracks, _ := s.plan["audioTracks"].([]object)
							for _, track := range tracks {
								if track["id"] == s.plan["selectedAudioTrackId"] && r.OutputAudioChannels > 0 {
									track["channels"] = r.OutputAudioChannels
								}
							}
						}
					}
				}
				initial := s.rungFor(path)
				if initial == "" {
					initial = "base"
					if len(adaptiveRungs) == 1 {
						initial = adaptiveRungs[0].ID
					}
				}
				if r.ID == initial && path["adaptive"] != true {
					path["initialRenditionId"] = r.ID
					if path["quality"] != nil {
						target := qualityTarget(r)
						target["videoCodec"], target["codecStatus"] = q["videoCodec"], q["codecStatus"]
						path["quality"] = target
					}
				}
				qualities = append(qualities, q)
			}
			path["qualities"] = qualities
		}
	}
	if diagnostics, ok := hs.(frontend.HLSDiagnosticSession); ok {
		d := diagnostics.PlaybackDiagnostics()
		s.diagnostics = d.Records
		events := append([]frontend.HLSPreparationEvent{}, s.preparation...)
		events = append(events, d.Events...)
		s.plan["preparation"] = object{"sourceGeneration": s.source, "seekGeneration": s.seek, "events": events}
	}
	after, _ := json.Marshal(object{"paths": paths, "preparation": s.plan["preparation"]})
	return liveChanged || string(before) != string(after)
}

// refreshLiveWindow never opens an output. The playlist response headers remain
// authoritative when publication advances between this read and the fetch.
func (i *instance) refreshLiveWindow(ctx context.Context, id string, s *playSession) bool {
	live, ok := s.plan["live"].(object)
	if !ok {
		return false
	}
	before, _ := json.Marshal(live)
	previousAnchor, previousEdge := live["anchor"], live["liveEdgeSeconds"]
	delete(live, "anchor")
	delete(live, "windowStartSeconds")
	delete(live, "liveEdgeSeconds")
	delete(live, "timeshift")
	var window frontend.LiveWindow
	err := frontend.ErrNotAvailable
	if s.archive != nil {
		window, err = i.host.LiveTV.(frontend.LiveCatchUp).CatchUpLiveWindow(ctx, id)
	} else if reader, ok := i.host.Sessions.(frontend.LiveWindowReader); ok {
		window, err = reader.LiveWindow(ctx, id)
	}
	if err == nil && window.Anchor.MediaSequence >= 0 && window.Anchor.MediaSequence <= 1<<53-1 {
		live["anchor"] = window.Anchor
		live["windowStartSeconds"] = window.Anchor.WindowStartSeconds
		live["liveEdgeSeconds"] = window.LiveEdgeSeconds
		if previousEdge != window.LiveEdgeSeconds || previousAnchor != window.Anchor || live["edgeAt"] == nil {
			live["edgeAt"] = i.catchUpNow()
		}
		if window.TimeshiftMinutes > 0 {
			live["timeshift"] = object{"maxMinutes": window.TimeshiftMinutes}
		}
	} else {
		delete(live, "edgeAt")
	}
	s.updateBehindLive()
	after, _ := json.Marshal(live)
	return string(before) != string(after)
}

func liveWindowBounds(v any) (start, edge float64, ok bool) {
	live, ok := v.(object)
	if !ok {
		return 0, 0, false
	}
	start, okStart := live["windowStartSeconds"].(float64)
	edge, okEdge := live["liveEdgeSeconds"].(float64)
	if !okStart || !okEdge || math.IsNaN(start) || math.IsInf(start, 0) || math.IsNaN(edge) || math.IsInf(edge, 0) || start < 0 || edge < start {
		return 0, 0, false
	}
	return start, edge, true
}

func durationFromSeconds(sec float64) time.Duration {
	if sec <= 0 || math.IsNaN(sec) || math.IsInf(sec, 0) {
		return 0
	}
	return time.Duration(math.Round(sec * float64(time.Second)))
}

// rebaseLivePosition maps a live session's playback position onto the new
// rendition's live window so positionSeconds, windowStartSeconds and
// liveEdgeSeconds stay on one timeline across a replan (whether the new lease
// starts fresh at 0 or joins an already-running rendition).
func rebaseLivePosition(live any, prevPos time.Duration, oldStart, oldEdge float64, carryBehind bool) time.Duration {
	newStart, newEdge, hasNew := liveWindowBounds(live)
	if !hasNew {
		return 0
	}
	posSec := newEdge
	if carryBehind {
		prevSec := prevPos.Seconds()
		if prevSec >= oldStart {
			behind := oldEdge - prevSec
			if behind < 0 {
				behind = 0
			}
			posSec = newEdge - behind
			if posSec < newStart {
				posSec = newStart
			}
		}
	}
	return durationFromSeconds(posSec)
}

func (s *playSession) updateBehindLive() {
	if live, ok := s.plan["live"].(object); ok {
		_, edge, _ := liveWindowBounds(live)
		live["behindLiveSeconds"] = max(0, edge-s.position.Seconds())
	}
}

// alignUnobservedLivePosition starts viewers at the live edge until they
// report a position or set one through a command.
func alignUnobservedLivePosition(s *playSession) {
	if s.observedSource == s.source || s.plan["state"] == "paused" {
		return
	}
	live, _ := s.plan["live"].(object)
	if live["mode"] == "startOver" {
		return
	}
	start, edge, ok := liveWindowBounds(live)
	if !ok {
		return
	}
	if sec := s.position.Seconds(); sec == 0 || sec < start || sec > edge {
		s.position = durationFromSeconds(edge)
		s.plan["positionSeconds"] = s.position.Seconds()
		s.updateBehindLive()
	}
}
