package transcode

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestStageExternalSubtitlesForBurnInAndMatroskaMux(t *testing.T) {
	dir := t.TempDir()
	var gotID, gotFormat, gotSource string
	alignedSRT := "1\n00:00:02,500 --> 00:00:04,000\nAligned burn-in cue\n\n"

	o := defaultOptions()
	WithSubtitleOpener(func(id, format, source string) (io.ReadCloser, string, error) {
		gotID, gotFormat, gotSource = id, format, source
		return io.NopCloser(strings.NewReader(alignedSRT)), "srt", nil
	})(&o)
	m := &Manager{opts: o}

	orig := core.MediaInfo{
		Container: "mkv",
		Duration:  10 * time.Minute,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080, FrameRate: 24},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000},
			{Index: 3, Type: "subtitle", Codec: "srt", Language: "en", External: true, ExternalURL: "ether-subtitle:sub123"},
			{Index: 4, Type: "subtitle", Codec: "srt", Language: "fr", External: true, ExternalURL: "ether-subtitle:sub456"},
		},
	}

	staged := m.stageExternalSubtitle(dir, orig, "rel-burn", core.Decision{SubtitleStream: 3, BurnSubtitles: true})
	if gotID != "ether-subtitle:sub123" || gotFormat != "srt" || gotSource != "rel-burn" {
		t.Fatalf("opener args = (%q, %q, %q)", gotID, gotFormat, gotSource)
	}
	if orig.Streams[2].ExternalURL != "ether-subtitle:sub123" {
		t.Fatalf("original MediaInfo mutated: %q", orig.Streams[2].ExternalURL)
	}
	if staged.Streams[3].ExternalURL != orig.Streams[3].ExternalURL {
		t.Fatal("staged an unselected sidecar")
	}
	wantPath := filepath.Join(dir, "sidecar-3.srt")
	if staged.Streams[2].ExternalURL != wantPath {
		t.Fatalf("staged ExternalURL = %q, want %q", staged.Streams[2].ExternalURL, wantPath)
	}
	disk, err := os.ReadFile(wantPath)
	if err != nil || string(disk) != alignedSRT {
		t.Fatalf("staged file = %q, err = %v", string(disk), err)
	}

	// Burn-in filter graph uses the staged aligned file path.
	burnDecision := core.Decision{
		Kind:           core.DecisionTranscode,
		VideoCodec:     "h264",
		AudioCodec:     "aac",
		AudioChannels:  2,
		SubtitleStream: 3,
		BurnSubtitles:  true,
	}
	burnSpec := baseSpec(staged, burnDecision)
	burnArgs := buildArgs(burnSpec)
	mustContain(t, burnArgs, escapeFilterValue(wantPath))
	mustNotContain(t, burnArgs, "ether-subtitle:sub123")

	// HTTP Matroska soft-subtitle muxing passes the staged aligned file as -i.
	mkvDecision := core.Decision{
		Kind:           core.DecisionRemux,
		Container:      ContainerMatroska,
		VideoCopy:      true,
		AudioCopy:      true,
		AudioStream:    1,
		SubtitleStream: 3,
		BurnSubtitles:  false,
	}
	mkvSpec := baseSpec(staged, mkvDecision)
	mkvSpec.Protocol = core.ProtocolHTTP
	mkvArgs := buildArgs(mkvSpec)
	mustContain(t, mkvArgs, "-i "+wantPath)
	mustNotContain(t, mkvArgs, "ether-subtitle:sub123")
}
