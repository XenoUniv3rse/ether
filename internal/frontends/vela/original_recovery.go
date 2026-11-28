package vela

import (
	"context"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// pollOriginalRecovery updates the active plan before a player tries another
// delivery path or interprets a truncated VOD response as completion.
func (i *instance) pollOriginalRecovery(ctx context.Context, v *frontend.Viewer, device frontend.Device, id string, s *playSession) {
	reader, ok := i.host.Sessions.(frontend.OriginalPlaybackReader)
	if !ok {
		return
	}
	s.mu.Lock()
	if !s.ended.IsZero() || s.commandActive || s.channelID != "" || s.item == 0 {
		s.mu.Unlock()
		return
	}
	source, seek, item, caps := s.source, s.seek, s.item, s.declaredCaps
	active := s.active
	formats := append([]string(nil), s.subtitleFormats...)
	s.mu.Unlock()

	access := frontend.SessionAccess{Viewer: v, Device: device, Protocol: "vela"}
	state, err := reader.OriginalPlaybackState(ctx, access, id)
	if err != nil || (!state.Unavailable && state.Revision == "") {
		return
	}
	if state.Unavailable {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.source == source && s.seek == seek && !s.commandActive && s.ended.IsZero() && s.plan["sourceFailure"] == nil {
			s.plan["sourceFailure"] = "version_unavailable"
			s.publish("plan")
		}
		return
	}

	s.mu.Lock()
	seen := s.plan["replacementSourceId"] == state.Revision
	s.mu.Unlock()
	if seen {
		return
	}
	original := state.Decision.Reason == "original_runtime"
	path := object{
		"id": "primary", "kind": "direct", "transport": "file", "url": state.URL, "mimeType": sourceMIME(state.Media.Container),
		"readiness": "ready", "timelineOriginSeconds": float64(0), "sourceId": state.Revision, "timelineId": state.Revision,
		"range": true, "byteLength": state.Media.Size, "decisionReason": state.Decision.Reason,
		"qualities": []object{}, "boundaries": []object{},
	}
	if original {
		path["original"] = true
		if facts := originalVideo(state.Media); facts != nil {
			path["originalVideo"] = facts
		}
	}
	audio, subtitles, selectedAudio, selectedSub := i.playbackTracks(ctx, access, id, state.Media, state.Decision, state.Decision.SubtitleStream, original, formats, false)
	markers := i.skipMarkers(ctx, item, state.Media)
	if active.autoReason != "" && active.release.ID != state.ReleaseID {
		active.autoReason = frontend.AutoSourceSelection
	}
	active.release, active.media = frontend.Release{ID: state.ReleaseID}, state.Media
	versionPlan := object{}
	cold := i.attachPlaybackVersions(ctx, v, device, item, caps, active, versionPlan)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.source != source || s.seek != seek || s.commandActive || !s.ended.IsZero() {
		return
	}
	s.media, s.active, s.position = state.Media, active, state.Position
	s.canBurn, s.qualityRungs = false, nil
	s.versionsCold = cold
	applyVersionPlan(s.plan, versionPlan)
	delete(s.plan, "serverHandoff")
	s.plan["paths"], s.plan["selectedPathId"] = []object{path}, "primary"
	s.plan["audioTracks"], s.plan["subtitleTracks"] = audio, subtitles
	delete(s.plan, "selectedAudioTrackId")
	if selectedAudio != "" {
		s.plan["selectedAudioTrackId"] = selectedAudio
	}
	s.plan["selectedSubtitleTrackId"] = selectedSub
	s.plan["chapters"], s.plan["durationSeconds"] = chapters(state.Media), state.Media.Duration.Seconds()
	s.plan["fallback"] = object{"state": "unsupported"}
	delete(s.plan, "qualityLimitations")
	delete(s.plan, "sourceFailure")
	s.plan["sourceOrigin"] = state.Origin
	s.plan["replacementSourceId"] = state.Revision
	setSkipMarkers(s.plan, markers)
	s.retainDiagnostics()
	s.source++
	s.seek++
	s.resetTrickplayForSourceLocked()
	s.publish("plan")
}
