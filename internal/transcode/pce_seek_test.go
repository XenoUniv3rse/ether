package transcode

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// The generated manifest is a security boundary: the source cannot add a
// second file, bypass the nested format restriction, or lose input headers.
func TestPCESeekManifestSafety(t *testing.T) {
	first, origin := int64(1_400_000), int64(1_400_000)
	s := runSpec{
		Protocol: core.ProtocolHLS, Input: "https://example.invalid/test.ts", Start: time.Hour,
		Info:         core.MediaInfo{Container: "mpegts", Streams: []core.StreamInfo{{Index: 1, Type: "audio", Codec: "aac", AACProgramConfig: true, AACConfigPTS: &first, AACSourceStart: &origin}}},
		Decision:     core.Decision{Container: ContainerFMP4, AudioStream: 1},
		InputHeaders: map[string]string{"X-Test": "example", "User-Agent": "test-agent"},
	}
	u, lead, ok := pceSeekInput(s)
	if !ok || lead != time.Second {
		t.Fatalf("bounded PCE input unavailable: %v %v", lead, ok)
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(u, "data:text/plain;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	m := string(b)
	if strings.Count(m, "file 'https://example.invalid/test.ts'") != 2 || strings.Count(m, "option format_whitelist mpegts") != 2 || strings.Count(m, "option headers 'X-Test: example'") != 2 {
		t.Fatalf("bad bounded manifest: %q", m)
	}
	if !strings.Contains(m, "inpoint 3601.400000") || !strings.Contains(m, "outpoint 2.400000") {
		t.Fatalf("incorrect source clock: %q", m)
	}
	s.Input = "https://example.invalid/a\nfile 'https://other.invalid/'"
	if _, _, ok := pceSeekInput(s); ok {
		t.Fatal("manifest injection accepted")
	}
	s.Input = "https://example.invalid/test.ts"
	s.InputHeaders["X-Test"] = strings.Repeat("x", 8192)
	if _, _, ok := pceSeekInput(s); ok {
		t.Fatal("oversize credential-bearing manifest accepted")
	}
	s.InputHeaders["X-Test"] = "example"
	s.InputHeaders["X-Other"] = "another"
	if _, _, ok := pceSeekInput(s); ok {
		t.Fatal("multiple custom headers were silently dropped")
	}
}
