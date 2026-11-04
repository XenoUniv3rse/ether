package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// pceChannels parses a bounded MPEG-4 Audio program_config_element. The
// optional ID is present in an ADTS raw_data_block but absent in ASC.
func pceChannels(r *bitReader, withID bool) (int, bool) {
	if withID && r.u(3) != 5 {
		return 0, false
	}
	r.skip(4)
	r.skip(2)
	freq := r.u(4)
	if freq >= 13 {
		return 0, false
	}
	front, side, back, lfe, assoc, cc := int(r.u(4)), int(r.u(4)), int(r.u(4)), int(r.u(2)), int(r.u(3)), int(r.u(4))
	if r.flag() {
		r.skip(4)
	}
	if r.flag() {
		r.skip(4)
	}
	if r.flag() {
		r.skip(3)
	}
	channels := 0
	for i := 0; i < front+side+back; i++ {
		if r.flag() {
			channels += 2
		} else {
			channels++
		}
		r.skip(4)
	}
	channels += lfe
	r.skip(lfe * 4)
	r.skip(assoc * 4)
	r.skip(cc * 5)
	if r.err != nil {
		return 0, false
	}
	if mod := r.pos % 8; mod != 0 {
		r.skip(8 - mod)
	}
	comment := int(r.u(8))
	r.skip(comment * 8)
	return channels, r.err == nil && channels > 0
}
func aacASCProgramConfig(data []byte, channels int) bool {
	r := &bitReader{b: data}
	aot := r.u(5)
	if aot == 31 {
		aot = 32 + r.u(6)
	}
	freq := r.u(4)
	if freq == 15 {
		r.skip(24)
	} else if freq >= 13 {
		return false
	}
	config := r.u(4)
	if aot < 1 || aot > 4 || config != 0 {
		return false
	}
	r.skip(1)
	if r.flag() {
		r.skip(14)
	}
	r.skip(1)
	n, ok := pceChannels(r, false)
	return ok && (channels <= 0 || channels == n)
}
func aacADTSProgramConfig(data []byte, channels int) bool {
	if len(data) < 8 || data[0] != 0xff || data[1]&0xf6 != 0xf0 {
		return false
	}
	if ((data[2]&1)<<2)|(data[3]>>6) != 0 {
		return false
	}
	n := (int(data[3]&3) << 11) | (int(data[4]) << 3) | int(data[5]>>5)
	header := 7
	if data[1]&1 == 0 {
		header = 9
	}
	if n < header+1 || n > len(data) {
		return false
	}
	count, ok := pceChannels(&bitReader{b: data[header:n]}, true)
	return ok && (channels <= 0 || channels == count)
}

type cappedProbeBuffer struct{ buf bytes.Buffer }

func (b *cappedProbeBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > 128<<10 {
		return 0, errors.New("AAC packet probe exceeds limit")
	}
	return b.buf.Write(p)
}

// FFprobe does not expose ASC for AAC carried as ADTS in TS. Only when its
// probe reports an unspecified layout do we inspect the first packet; failure
// leaves the fact unknown and does not turn an otherwise usable probe into an
// error. The extra read is bounded and never occurs for normal AAC layouts.
func markADTSProgramConfigs(ctx context.Context, runner Runner, ffprobe, input string, info *core.MediaInfo) {
	if info.Container != "mpegts" {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	inspected := 0
	for i := range info.Streams {
		st := &info.Streams[i]
		layout := strings.TrimSpace(st.ChannelLayout)
		if st.Type != "audio" || st.Codec != "aac" || st.AACProgramConfig || st.Channels <= 0 || (layout != "" && !strings.EqualFold(layout, "unknown")) {
			continue
		}
		if probeCtx.Err() != nil || inspected >= 16 {
			return
		}
		inspected++
		args := []string{"-hide_banner", "-v", "error", "-show_packets", "-show_format", "-show_data", "-show_entries", "packet=stream_index,data,pts_time:format=start_time", "-select_streams", strconv.Itoa(st.Index), "-read_intervals", "%+#1", "-of", "json"}
		args = append(args, inputSafetyArgs(input, false)...)
		args = append(args, httpInputArgs(input, false)...)
		args = append(args, "-i", input)
		var output cappedProbeBuffer
		err := runner.Run(probeCtx, Command{Path: ffprobe, Args: args, Stdout: &output})
		if err != nil {
			continue
		}
		var packets struct {
			Packets []struct {
				Stream int    `json:"stream_index"`
				Data   string `json:"data"`
				PTS    string `json:"pts_time"`
			} `json:"packets"`
			Format struct {
				Start string `json:"start_time"`
			} `json:"format"`
		}
		if json.Unmarshal(output.buf.Bytes(), &packets) != nil || len(packets.Packets) != 1 || packets.Packets[0].Stream != st.Index {
			continue
		}
		st.AACProgramConfig = aacADTSProgramConfig(probeExtradata(packets.Packets[0].Data), st.Channels)
		if st.AACProgramConfig {
			pts, ptsOK := pceTimestamp(packets.Packets[0].PTS)
			start, startOK := pceTimestamp(packets.Format.Start)
			if ptsOK && startOK {
				st.AACConfigPTS, st.AACSourceStart = &pts, &start
			}
		}
	}
}

func pceTimestamp(s string) (int64, bool) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > 1e8 {
		return 0, false
	}
	return int64(math.Round(f * 1e6)), true
}
