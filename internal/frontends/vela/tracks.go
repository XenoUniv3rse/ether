package vela

import (
	"context"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// playbackTracks preserves source stream identities across output generations.
// Only the selected audio's delivery facts change; another track never inherits
// the previous encoder's codec or channel count.
func (i *instance) playbackTracks(ctx context.Context, access frontend.SessionAccess, id string, media frontend.MediaInfo, d frontend.Decision, selectedSubtitle int, original bool, formats []string, canBurn bool) ([]object, []object, string, any) {
	audio, subs := []object{}, []object{}
	selectedAudio := ""
	var selectedSub any
	var subReqs []frontend.SubtitleURLRequest
	for _, track := range media.Streams {
		if track.Type != "subtitle" || !frontend.IsTextSubtitle(track.Codec) {
			continue
		}
		if !original || !supportsEmbeddedSubtitle(track.Codec, formats) {
			subReqs = append(subReqs, frontend.SubtitleURLRequest{Index: track.Index, Format: "vtt"})
		}
	}
	vttURLs := map[int]string{}
	if len(subReqs) > 0 {
		var urls []string
		if batch, ok := i.host.Sessions.(frontend.SubtitleURLBatch); ok {
			if got, err := batch.SubtitleURLs(ctx, access, id, subReqs); err == nil && len(got) == len(subReqs) {
				urls = got
			}
		}
		if urls == nil {
			urls = make([]string, len(subReqs))
			for idx, req := range subReqs {
				if u, err := i.host.Sessions.SubtitleURL(ctx, access, id, req.Index, req.Format); err == nil {
					urls[idx] = u
				}
			}
		}
		for idx, req := range subReqs {
			if urls[idx] != "" {
				vttURLs[req.Index] = urls[idx]
			}
		}
	}
	for _, track := range media.Streams {
		if track.Type != "audio" && track.Type != "subtitle" {
			continue
		}
		t := sourceTrack(track)
		if track.Type == "audio" {
			if track.Index == d.AudioStream {
				selectedAudio = t["id"].(string)
				if !d.AudioCopy && d.AudioCodec != "" {
					t["codec"] = d.AudioCodec
					delete(t, "channels")
				}
			}
			audio = append(audio, t)
			continue
		}
		switch {
		case original && supportsEmbeddedSubtitle(track.Codec, formats):
			t["delivery"] = object{"kind": "embedded"}
		case frontend.IsImageSubtitle(track.Codec):
			if !canBurn {
				continue
			}
			t["delivery"] = object{"kind": "burn-in"}
		case frontend.IsTextSubtitle(track.Codec):
			url := vttURLs[track.Index]
			if url == "" {
				continue
			}
			if original && strings.Contains(url, "ether_window=1") {
				// No active playback tee exists for the original. Offer this
				// track after HLS activates, rather than claiming burn-in or
				// exposing an unusable cue window.
				continue
			}
			format := "webvtt"
			if strings.Contains(url, "ether_window=1") {
				format = "webvtt-window"
			}
			t["codec"], t["url"] = "webvtt", url
			t["delivery"] = object{"kind": "external", "format": format, "url": url}
		default:
			continue
		}
		subs = append(subs, t)
		if track.Index == selectedSubtitle {
			selectedSub = t["id"]
		}
	}
	return audio, subs, selectedAudio, selectedSub
}

func supportsEmbeddedSubtitle(codec string, formats []string) bool {
	for _, format := range formats {
		if strings.EqualFold(format, codec) {
			return true
		}
	}
	return false
}

func sourceTrack(track frontend.StreamInfo) object {
	t := object{
		"id": strconv.Itoa(track.Index), "streamIndex": track.Index, "kind": track.Type,
		"language": track.Language, "name": track.Title, "codec": track.Codec,
		"default": track.Default, "forced": track.Forced,
	}
	if track.Type == "audio" {
		t["channels"] = track.Channels
	}
	return t
}

// Downloads only offer subtitles the client can read from the original bytes.
func offlineDownloadTracks(media frontend.MediaInfo, formats []string) ([]object, []object) {
	audio, subs := []object{}, []object{}
	for _, track := range media.Streams {
		switch {
		case track.Type == "audio":
			audio = append(audio, sourceTrack(track))
		case track.Type == "subtitle" && supportsEmbeddedSubtitle(track.Codec, formats):
			t := sourceTrack(track)
			t["delivery"] = object{"kind": "embedded"}
			subs = append(subs, t)
		}
	}
	return audio, subs
}

func (i *instance) refreshTrackDelivery(ctx context.Context, access frontend.SessionAccess, id string, s *playSession) {
	reader, ok := i.host.Sessions.(frontend.StreamDecisionReader)
	if !ok || len(s.media.Streams) == 0 {
		return
	}
	d, err := reader.StreamDecision(ctx, access, id)
	if err != nil {
		return
	}
	selected := -1
	if track, ok := s.plan["selectedSubtitleTrackId"].(string); ok {
		selected, _ = strconv.Atoi(track)
	}
	audio, subs, selectedAudio, _ := i.playbackTracks(ctx, access, id, s.media, d, selected, false, s.subtitleFormats, s.canBurn)
	s.plan["audioTracks"], s.plan["subtitleTracks"] = audio, subs
	if selectedAudio != "" {
		s.plan["selectedAudioTrackId"] = selectedAudio
	}
	path := selectedPlaybackPath(s.plan, s.plan["paths"].([]object))
	kind := "remux"
	// A quality rung is always re-encoded, whatever the session's main
	// decision is.
	if d.Kind == frontend.DecisionTranscode || s.rungFor(path) != "" {
		kind = "transcode"
	}
	path["kind"], path["decisionReason"] = kind, d.Reason
}
