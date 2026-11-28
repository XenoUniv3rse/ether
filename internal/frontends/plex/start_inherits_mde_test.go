package plex

import (
	"net/url"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// Plex for Roku sends hasMDE=1 on /decision but omits it on the
// start.mkv request that continues that decision. The start must keep the
// decision's MDE mode; otherwise it no longer matches the stored decision,
// re-plans through the legacy Roku profile target (h264 first, level 4.1)
// and serves a 1080p transcode after /decision promised a 2160p video copy.
func TestStartWithoutHasMDEKeepsDecisionVideoCopy(t *testing.T) {
	f := newFixture()
	f.srv.setTranscoder(&realDecider{f.trans})
	defer f.srv.Close()
	f.playback.info = core.MediaInfo{Container: "mkv", Duration: sampleInfo.Duration, Size: sampleInfo.Size, BitrateKbps: 51095, Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10", BitDepth: 10, Width: 3840, Height: 2160, FrameRate: 23.976, ColorTransfer: "smpte2084", ColorPrimaries: "bt2020", BitrateKbps: 48000},
		{Index: 1, Type: "audio", Codec: "dts", Profile: "DTS-HD MA", Language: "eng", Channels: 6, ChannelLayout: "5.1(side)", SampleRate: 48000, Default: true},
	}}

	extra := "append-transcode-target-codec(type=videoProfile&context=streaming&protocol=http&videoCodec=vp9)" +
		"+add-limitation(scope=videoCodec&scopeName=vp9&type=upperBound&name=video.bitDepth&value=10&isRequired=false)" +
		"+append-transcode-target-codec(type=videoProfile&context=streaming&protocol=http&videoCodec=av1)" +
		"+add-limitation(scope=videoCodec&scopeName=av1&type=upperBound&name=video.bitDepth&value=10&isRequired=false)" +
		"+add-limitation(scope=videoCodec&scopeName=hevc&type=notMatch&name=video.DOVIProfile&value=5)" +
		"+add-limitation(scope=videoCodec&scopeName=av1&type=notMatch&name=video.DOVIProfile&value=5)" +
		"+append-transcode-target-codec(type=videoProfile&context=streaming&protocol=http&videoCodec=hevc)" +
		"+add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.bitDepth&value=10&isRequired=false)" +
		"+add-limitation(scope=videoCodec&scopeName=h264&type=upperBound&name=video.level&value=41)" +
		"+add-limitation(scope=videoCodec&scopeName=h264&type=upperBound&name=video.refFrames&value=8&replace=true)" +
		"+add-limitation(scope=videoAudioCodec&scopeName=mp3&type=upperBound&name=audio.channels&replace=true&value=0)"
	hdr := []string{
		"X-Plex-Product", "Plex for Roku", "X-Plex-Platform", "Roku", "X-Plex-Device", "Roku Express 4K+",
		"X-Plex-Model", "3940X2", "X-Plex-Version", "9.20.12.12103-7d9eb942f",
		"X-Plex-Client-Identifier", "roku-mde-start", "X-Plex-Session-Identifier", "roku-mde-playback",
	}
	common := "path=/library/metadata/" + fightClub.Key.String() +
		"&mediaIndex=0&partIndex=0&protocol=http&directPlay=0&directStream=1&videoQuality=100&videoResolution=3840x2160" +
		"&session=roku-mde-1&X-Plex-Client-Profile-Extra=" + url.QueryEscape(extra)

	rec := f.get(t, "/video/:/transcode/universal/decision?"+common+"&hasMDE=1", append(jsonAccept, hdr...)...)
	if rec.Code != 200 {
		t.Fatalf("decision: %d %s", rec.Code, rec.Body.String())
	}
	f.trans.mu.Lock()
	decided := f.trans.lastCaps.MaxHeight
	f.trans.mu.Unlock()
	if decided != 0 && decided < 2160 {
		t.Fatalf("decision caps MaxHeight = %d, want 2160", decided)
	}

	rec = f.get(t, "/video/:/transcode/universal/start.mkv?"+common, hdr...)
	if rec.Code != 200 {
		t.Fatalf("start.mkv: %d %s", rec.Code, rec.Body.String())
	}
	f.trans.mu.Lock()
	h := f.trans.sessions["roku-mde-1"]
	f.trans.mu.Unlock()
	if h == nil {
		t.Fatalf("start.mkv started no encoder session")
	}
	if d := h.req.Decision; !d.VideoCopy || (d.MaxHeight != 0 && d.MaxHeight < 2160) {
		t.Fatalf("start.mkv decision = %+v (reason %q), want the /decision video copy at 2160p", d, d.Reason)
	}
}
