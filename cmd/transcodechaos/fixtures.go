package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// makeFixture generates small deterministic inputs. An unavailable encoder is
// reported as a skipped scenario rather than silently substituting a fixture.
func makeFixture(ctx context.Context, ff, root, name string, healthy []byte) ([]byte, error) {
	if !strings.HasPrefix(name, "input-") {
		return healthy, nil
	}
	out := filepath.Join(root, name+".ts")
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "12", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-g", "48", "-c:a", "aac"}
	switch name {
	case "input-open-gop":
		args = append(args, "-x264-params", "open-gop=1:keyint=240:min-keyint=240:scenecut=0")
	case "input-long-gop":
		args = append(args, "-g", "240", "-keyint_min", "240", "-sc_threshold", "0")
	case "input-audio-late":
		args = []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-itsoffset", "2.5", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "12", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-g", "48", "-c:a", "aac"}
	case "input-audio-early-end":
		args = append(args, "-af", "atrim=duration=5")
	case "input-vfr":
		args = append(args, "-vf", "select='not(mod(n,3))+not(mod(n,7))'", "-fps_mode", "vfr")
	case "input-interlaced":
		args = append(args, "-vf", "tinterlace=interleave_top", "-flags", "+ilme+ildct", "-x264-params", "tff=1")
	case "input-hevc10-hdr":
		args = append(args, "-c:v", "libx265", "-pix_fmt", "yuv420p10le", "-x265-params", "pools=1:frame-threads=1:log-level=error:hdr10=1:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc", "-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc")
	case "input-pts-wrap":
		args = append(args, "-output_ts_offset", "95440")
	case "input-multi-audio":
		args = []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-f", "lavfi", "-i", "anullsrc=channel_layout=5.1(side):sample_rate=48000", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "12", "-map", "0:v", "-map", "1:a", "-map", "2:a", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-g", "48", "-c:a", "aac"}
	case "input-missing-keyframe":
		original := filepath.Join(root, "keyframe-original.ts")
		if err := os.WriteFile(original, healthy, 0600); err != nil {
			return nil, err
		}
		args = []string{"-hide_banner", "-loglevel", "error", "-y", "-ss", "0.5", "-i", original, "-t", "10", "-map", "0", "-c", "copy", "-copyinkf"}
	case "input-resolution-change", "input-samplerate-change":
		p1, p2 := filepath.Join(root, name+"-a.ts"), filepath.Join(root, name+"-b.ts")
		for i, p := range []string{p1, p2} {
			size, rate := "320x180", "48000"
			if i == 1 && name == "input-resolution-change" {
				size = "640x360"
			}
			if i == 1 && name == "input-samplerate-change" {
				rate = "44100"
			}
			a := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=" + size + ":rate=24", "-f", "lavfi", "-i", "sine=sample_rate=" + rate, "-t", "6", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-g", "48", "-c:a", "aac"}
			if i == 1 {
				a = append(a, "-output_ts_offset", "6")
			}
			a = append(a, "-f", "mpegts", p)
			if err := command(ctx, ff, a...); err != nil {
				return nil, err
			}
		}
		a, err := os.ReadFile(p1)
		if err != nil {
			return nil, err
		}
		b, err := os.ReadFile(p2)
		if err != nil {
			return nil, err
		}
		return append(a, b...), nil
	default:
		return nil, fmt.Errorf("unknown input %s", name)
	}
	args = append(args, "-f", "mpegts", out)
	if err := command(ctx, ff, args...); err != nil {
		return nil, err
	}
	return os.ReadFile(out)
}
