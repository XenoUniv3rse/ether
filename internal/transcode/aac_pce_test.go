package transcode

import (
	"encoding/hex"
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
)

// PCE is bit-aligned syntax with variable element counts and comments. Test
// both representations and reject truncated or mismatched configurations.
func TestAACProgramConfigParsing(t *testing.T) {
	asc, _ := hex.DecodeString("118004c844002000c40d4c61766336322e32382e313033")
	adts, _ := hex.DecodeString("fff14c00083ffca0990880040018800d4c61766336322e32382e313033de02004c61766336322e32382e31303300422008c118004608000630400231004608c1c0")
	if !aacASCProgramConfig(asc, 6) {
		t.Fatal("ASC PCE not detected")
	}
	if !aacADTSProgramConfig(adts, 6) {
		t.Fatal("ADTS PCE not detected")
	}
	if aacASCProgramConfig(asc, 2) || aacADTSProgramConfig(adts, 2) {
		t.Fatal("mismatched channels accepted")
	}
	for i := 0; i < 10; i++ {
		if aacASCProgramConfig(asc[:i], 6) {
			t.Fatalf("truncated ASC accepted at %d", i)
		}
		if aacADTSProgramConfig(adts[:i], 6) {
			t.Fatalf("truncated ADTS accepted at %d", i)
		}
	}
	ordinary, _ := hex.DecodeString("1190")
	if aacASCProgramConfig(ordinary, 2) {
		t.Fatal("ordinary AAC was classified as PCE")
	}
}

// The configuration fact changes TS segments and fMP4 from TS sources;
// other transport contracts and ordinary AAC keep their copy decision.
func TestAACProgramConfigTransportBoundary(t *testing.T) {
	a := core.StreamInfo{Type: "audio", Codec: "aac", Channels: 6, AACProgramConfig: true}
	caps := core.ClientCaps{}
	for _, tc := range []struct {
		name, source, video, protocol string
		download, marked, wantCopy    bool
	}{
		{"ts", "mpegts", "h264", core.ProtocolHLS, false, true, false},
		{"ordinary", "mpegts", "h264", core.ProtocolHLS, false, false, true},
		{"dash", "mpegts", "h264", core.ProtocolDASH, false, true, true},
		{"http", "mpegts", "h264", core.ProtocolHTTP, false, true, true},
		{"fmp4-from-ts", "mpegts", "hevc", core.ProtocolHLS, false, true, false},
		{"fmp4-from-mp4", "mp4", "hevc", core.ProtocolHLS, false, true, true},
		{"download", "mpegts", "h264", core.ProtocolHLS, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := a
			s.AACProgramConfig = tc.marked
			c := caps
			c.Download = tc.download
			copy, _, _, _ := pceAudioPlan(&s, c, core.DecideOptions{Protocol: tc.protocol}, tc.source, tc.video, true, "aac", 384, "")
			if copy != tc.wantCopy {
				t.Fatalf("copy=%v want %v", copy, tc.wantCopy)
			}
		})
	}
}
