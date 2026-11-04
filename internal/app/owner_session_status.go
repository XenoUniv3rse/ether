package app

import (
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// ownerSessionStatus adds console-only fields without changing protocol wire responses.
type ownerSessionStatus struct {
	*core.PlaySession
	IP          string         `json:"ip"`
	BitrateKbps int64          `json:"bitrateKbps"`
	SourceVideo string         `json:"sourceVideo"`
	OutputVideo string         `json:"outputVideo"`
	SourceAudio string         `json:"sourceAudio"`
	OutputAudio string         `json:"outputAudio"`
	Container   string         `json:"container"`
	RatingKey   core.RatingKey `json:"ratingKey"`
	Subtitle    string         `json:"subtitle"`
}

func ownerSessionStatuses(sessions []*core.PlaySession) []ownerSessionStatus {
	out := make([]ownerSessionStatus, 0, len(sessions))
	for _, p := range sessions {
		if p != nil {
			out = append(out, ownerSessionDetails(p))
		}
	}
	return out
}
func ownerSessionDetails(p *core.PlaySession) ownerSessionStatus {
	out := ownerSessionStatus{PlaySession: p, IP: p.Client.ClientIP, RatingKey: p.Key}
	if p.SpeedBps > 0 {
		out.BitrateKbps = p.SpeedBps * 8 / 1000
	}
	d := p.DeliveryDecision
	if p.Encoder != nil {
		d = &p.Encoder.Decision
	}
	if p.Decision == core.DecisionTranscode || (p.Decision == core.DecisionRemux && d != nil && !d.AudioCopy) {
		out.BitrateKbps = 0
		if d != nil && d.BitrateKbps > 0 {
			out.BitrateKbps = int64(d.BitrateKbps)
		}
	}
	if p.Decision == core.DecisionDirectPlay && !p.Info.Estimated {
		out.Container = p.Info.Container
	} else if d != nil {
		out.Container = d.Container
	}
	var video *core.StreamInfo
	if !p.Info.Estimated {
		video = p.Info.Video()
	}
	if video != nil {
		out.SourceVideo = videoLabel(video.Codec, video.Height, videoRange(video))
	}
	var audio, subtitle *core.StreamInfo
	if d != nil && !p.Info.Estimated {
		for i := range p.Info.Streams {
			st := &p.Info.Streams[i]
			if st.Type == "audio" && st.Index == d.AudioStream {
				audio = st
			}
			if st.Type == "subtitle" && st.Index == d.SubtitleStream {
				subtitle = st
			}
		}
	}
	if audio != nil {
		out.SourceAudio = audioLabel(audio.Codec, audio.ChannelLayout, audio.Channels)
	}
	if p.Decision == core.DecisionDirectPlay || (d != nil && d.VideoCopy) {
		out.OutputVideo = out.SourceVideo
	} else if d != nil {
		h := 0
		if video != nil && video.Height > 0 {
			h = video.Height
			if d.MaxHeight > 0 && d.MaxHeight < h {
				h = d.MaxHeight
			}
			h -= h % 2
		}
		rng := ""
		if d.ToneMap {
			rng = "SDR"
		}
		out.OutputVideo = videoLabel(d.VideoCodec, h, rng)
	}
	if p.Decision == core.DecisionDirectPlay || (d != nil && d.AudioCopy) {
		out.OutputAudio = out.SourceAudio
	} else if d != nil {
		channels := d.AudioChannels
		if channels == 0 && audio != nil && audio.Channels > 0 {
			channels = audio.Channels
			limit := 2
			if d.AudioCodec == "ac3" || d.AudioCodec == "eac3" {
				limit = 6
			}
			if channels > limit {
				channels = limit
			}
		}
		layout := ""
		if channels == 2 {
			layout = "stereo"
		} else if audio != nil && channels == audio.Channels {
			layout = audio.ChannelLayout
		}
		out.OutputAudio = audioLabel(d.AudioCodec, layout, channels)
	}
	if subtitle != nil {
		parts := []string{}
		if subtitle.Language != "" {
			parts = append(parts, subtitle.Language)
		}
		if subtitle.Codec != "" {
			parts = append(parts, strings.ToUpper(subtitle.Codec))
		}
		if len(parts) > 0 && d.BurnSubtitles {
			parts = append(parts, "burned")
		}
		out.Subtitle = strings.Join(parts, " ")
	}
	return out
}
func codecLabel(codec string) string {
	switch strings.ToLower(codec) {
	case "h264":
		return "H.264"
	case "hevc", "h265":
		return "HEVC"
	case "av1":
		return "AV1"
	case "vp9":
		return "VP9"
	}
	return strings.ToUpper(codec)
}
func videoRange(v *core.StreamInfo) string {
	if v.DolbyVision {
		return "Dolby Vision"
	}
	switch strings.ToLower(v.ColorTransfer) {
	case "smpte2084":
		return "HDR10"
	case "arib-std-b67":
		return "HLG"
	case "bt709", "iec61966-2-1", "smpte170m":
		return "SDR"
	}
	return ""
}
func videoLabel(codec string, height int, dynamicRange string) string {
	parts := []string{}
	if codec != "" {
		parts = append(parts, codecLabel(codec))
	}
	if height > 0 {
		parts = append(parts, fmt.Sprintf("%dp", height))
	}
	if dynamicRange != "" {
		parts = append(parts, dynamicRange)
	}
	return strings.Join(parts, " ")
}
func audioLabel(codec, layout string, channels int) string {
	parts := []string{}
	if codec != "" {
		parts = append(parts, codecLabel(codec))
	}
	switch strings.ToLower(layout) {
	case "mono":
		layout = "1.0"
	case "stereo":
		layout = "2.0"
	}
	if layout != "" {
		parts = append(parts, layout)
	} else if channels > 0 {
		parts = append(parts, fmt.Sprintf("%dch", channels))
	}
	return strings.Join(parts, " ")
}
