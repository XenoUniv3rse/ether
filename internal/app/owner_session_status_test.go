package app

import (
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
)

func TestOwnerSessionDetails(t *testing.T) {
	p := &core.PlaySession{Key: core.MovieKey(1), Client: core.ClientCaps{ClientIP: "192.0.2.1"}, Decision: core.DecisionTranscode, Info: core.MediaInfo{Container: "mkv", Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "hevc", Height: 2160, ColorTransfer: "smpte2084"}, {Index: 1, Type: "audio", Codec: "eac3", Channels: 6, ChannelLayout: "5.1"}, {Index: 2, Type: "subtitle", Codec: "ass", Language: "eng"}}}, DeliveryDecision: &core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", MaxHeight: 1080, ToneMap: true, AudioCodec: "aac", AudioChannels: 2, AudioStream: 1, SubtitleStream: 2, BurnSubtitles: true, Container: "mpegts", BitrateKbps: 5000}}
	got := ownerSessionDetails(p)
	if got.IP != "192.0.2.1" || got.SourceVideo != "HEVC 2160p HDR10" || got.OutputVideo != "H.264 1080p SDR" || got.SourceAudio != "EAC3 5.1" || got.OutputAudio != "AAC 2.0" || got.Subtitle != "eng ASS burned" || got.Container != "mpegts" || got.BitrateKbps != 5000 || got.RatingKey != p.Key {
		t.Fatalf("details: %+v", got)
	}
	p.DeliveryDecision.VideoCopy = true
	p.DeliveryDecision.AudioCopy = true
	p.Decision = core.DecisionRemux
	got = ownerSessionDetails(p)
	if got.OutputVideo != got.SourceVideo || got.OutputAudio != got.SourceAudio {
		t.Fatalf("copy: %+v", got)
	}
}
