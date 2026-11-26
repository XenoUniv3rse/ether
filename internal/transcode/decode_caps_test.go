package transcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

type fakeDecodeCapabilities struct {
	allow func(core.HWAccel, *core.StreamInfo) bool
}

func (f fakeDecodeCapabilities) supports(hw core.HWAccel, v *core.StreamInfo) bool {
	return f.allow(hw, v)
}
func (fakeDecodeCapabilities) reject(core.HWAccel, *core.StreamInfo) {}

// Each accelerator must independently admit the source's profile/depth/chroma,
// retain its encoder on rejection, and honour the explicit runtime retry flag.
func TestDecodeCapabilityGraphs(t *testing.T) {
	for _, tc := range []struct{ accel, encoder, filter string }{
		{AccelVAAPI, "h264_vaapi", "scale_vaapi="},
		{AccelQSV, "h264_qsv", "vpp_qsv="},
		{AccelNVENC, "h264_nvenc", "scale_cuda="},
		{AccelVideoToolbox, "h264_videotoolbox", "scale_vt="},
	} {
		t.Run(tc.accel, func(t *testing.T) {
			info := mustParse(t, "mp4_h264_aac.json")
			opts := allow()
			opts.MaxHeight = 720
			s := baseSpec(info, Decide(info, appleTV, opts, Policy{}))
			s.Accel = core.HWAccel{Name: tc.accel, Device: "test-device", Encoders: []string{tc.encoder}}
			s.Features.hwaccels = map[string]bool{"cuda": true, "qsv": true, "videotoolbox": true}
			for _, filter := range []string{"scale_cuda", "vpp_qsv", "scale_vt", "scale_vaapi"} {
				s.Features.filters[filter] = true
			}
			allowed := sourceDecodeKey(s.Accel, s.Info.Video())
			s.Features.decode = fakeDecodeCapabilities{allow: func(hw core.HWAccel, v *core.StreamInfo) bool { return sourceDecodeKey(hw, v) == allowed }}
			mustContain(t, buildArgs(s), "-hwaccel_output_format", tc.filter, "-c:v "+tc.encoder)
			original := *s.Info.Video()
			for _, change := range []func(*core.StreamInfo){
				func(v *core.StreamInfo) { v.Codec = "mpeg4"; v.Profile = "Simple Profile" },
				func(v *core.StreamInfo) { v.Profile = "High 10"; v.BitDepth = 10; v.PixelFormat = "yuv420p10le" },
				func(v *core.StreamInfo) { v.PixelFormat = "yuv422p" },
			} {
				*s.Info.Video() = original
				change(s.Info.Video())
				args := buildArgs(s)
				mustNotContain(t, args, "-hwaccel ", "-hwaccel_output_format", tc.filter)
				mustContain(t, args, "-c:v "+tc.encoder, "scale=1280:720")
				if tc.accel == AccelVAAPI {
					mustContain(t, args, "-vaapi_device test-device", "format=nv12,hwupload")
				}
			}
			*s.Info.Video() = original
			s.SoftwareDecode = true
			mustNotContain(t, buildArgs(s), "-hwaccel ", tc.filter)
			mustContain(t, buildArgs(s), "-c:v "+tc.encoder)
			s.SoftwareDecode = false
			s.Features.decode = nil
			mustNotContain(t, buildArgs(s), "-hwaccel ", tc.filter)
		})
	}
}

// Cold/warm admission, bounded waiting and rejection persistence are coupled:
// the first HDR run must neither stall nor assume support, and a later run may
// use only the exact device/profile/chroma result, including after a restart.
func TestDecodeToneMapAdmission(t *testing.T) {
	for _, tc := range []struct{ accel, hw string }{{AccelVideoToolbox, "videotoolbox"}, {AccelNVENC, "cuda"}, {AccelQSV, "qsv"}} {
		for _, dv := range []int{0, 5, 7, 8} {
			t.Run(fmt.Sprintf("%s/dv%d", tc.accel, dv), func(t *testing.T) {
				info := mustParse(t, "mkv_hevc_hdr10.json")
				v := info.Video()
				v.Codec, v.Profile, v.PixelFormat, v.BitDepth = "hevc", "Main 10", "yuv420p10le", 10
				v.Width, v.Height = 3840, 2160
				v.DolbyVision, v.DolbyVisionProfile = dv != 0, dv
				d := Decide(info, plexWeb, allow(), Policy{})
				d.Kind, d.VideoCopy, d.VideoCodec, d.MaxHeight, d.ToneMap = core.DecisionTranscode, false, "h264", 720, true
				s := baseSpec(info, d)
				s.Accel = core.HWAccel{Name: tc.accel, Encoders: []string{"h264_" + tc.accel}}
				s.Features.hwaccels = map[string]bool{tc.hw: true}
				s.Features.filters = map[string]bool{"scale": true, "format": true, "tonemapx": true}
				c := newDecodeProbeCache(main10ProbeRunner(nil), "ffmpeg", "ffprobe", t.TempDir(), map[string]bool{"libx265": true})
				defer c.close()
				s.Features.decode = c
				// Before the demand budget, identical lab facts choose software decode.
				if s.canGPUDecode() {
					t.Fatal("cold cache admitted unverified decode")
				}
				args := buildArgs(s)
				mustContain(t, args, "-hwaccel "+tc.hw, "-c:v h264_"+tc.accel, "tonemapx=", "scale=1280:720")
				mustNotContain(t, args, "-hwaccel_output_format")
				graph := joined(args)
				if dv == 5 {
					mustContain(t, args, "apply_dovi=1")
				} else if strings.Index(graph, "scale=1280:720") > strings.Index(graph, "tonemapx=") {
					t.Fatal("tone map ran before downscale")
				}
				s.SoftwareDecode = true
				mustNotContain(t, buildArgs(s), "-hwaccel ")
			})
		}
	}
	t.Run("cold-main10-dv7-full-gpu", func(t *testing.T) {
		info := mustParse(t, "mkv_hevc_hdr10.json")
		info.Video().DolbyVision, info.Video().DolbyVisionProfile = true, 7
		d := Decide(info, plexWeb, allow(), Policy{})
		d.Kind, d.VideoCopy, d.VideoCodec, d.MaxHeight, d.ToneMap = core.DecisionTranscode, false, "h264", 720, true
		s := baseSpec(info, d)
		s.Accel = core.HWAccel{Name: AccelVideoToolbox, Encoders: []string{"h264_videotoolbox"}}
		s.Features.filters["scale_vt"], s.Features.filters["tonemap_videotoolbox"] = true, true
		c := newDecodeProbeCache(main10ProbeRunner(nil), "ffmpeg", "ffprobe", t.TempDir(), map[string]bool{"libx265": true})
		defer c.close()
		s.Features.decode = c
		args := buildArgs(s)
		mustContain(t, args, "-hwaccel videotoolbox -hwaccel_output_format videotoolbox_vld", "tonemap_videotoolbox=", "scale_vt=w=1280:h=720", "-c:v h264_videotoolbox")
		mustNotContain(t, args, "tonemapx=", "scale=1280:720")
	})
	t.Run("deadline-and-next-run", func(t *testing.T) {
		release := make(chan struct{})
		c := newDecodeProbeCache(main10ProbeRunner(release), "ffmpeg", "ffprobe", t.TempDir(), map[string]bool{"libx265": true})
		defer c.close()
		info := mustParse(t, "mkv_hevc_hdr10.json")
		hw := core.HWAccel{Name: AccelVideoToolbox}
		if c.supportsWithin(hw, info.Video(), 300*time.Millisecond) {
			t.Fatal("pending probe admitted")
		}
		c.mu.Lock()
		done := c.pending[sourceDecodeKey(hw, info.Video())]
		c.mu.Unlock()
		close(release)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("probe did not finish")
		}
		if !c.supports(hw, info.Video()) {
			t.Fatal("next run did not see completed probe")
		}
	})
	t.Run("persistence-build-and-runtime-rejection", func(t *testing.T) {
		db := store.OpenTest(t)
		binary := filepath.Join(t.TempDir(), "ffmpeg")
		if err := os.WriteFile(binary, []byte("build one"), 0600); err != nil {
			t.Fatal(err)
		}
		info := mustParse(t, "mkv_hevc_hdr10.json")
		hw := core.HWAccel{Name: AccelVideoToolbox}
		newCache := func(r Runner) *decodeProbeCache {
			c := newDecodeProbeCache(r, binary, "ffprobe", t.TempDir(), map[string]bool{"libx265": true})
			c.db = db
			return c
		}
		c := newCache(main10ProbeRunner(nil))
		if !c.supportsWithin(hw, info.Video(), time.Second) {
			t.Fatal("initial probe denied")
		}
		c.close()
		fail := &fakeRunner{runFunc: func(context.Context, Command) error {
			t.Error("stored result unexpectedly reprobed")
			return errors.New("unexpected probe")
		}}
		c = newCache(fail)
		if !c.supportsWithin(hw, info.Video(), time.Second) {
			t.Fatal("stored admission lost")
		}
		c.reject(hw, info.Video())
		c.close()
		c = newCache(fail)
		if c.supportsWithin(hw, info.Video(), time.Second) {
			t.Fatal("runtime rejection resurrected")
		}
		c.close()
		if err := os.WriteFile(binary, []byte("build two"), 0600); err != nil {
			t.Fatal(err)
		}
		c = newCache(main10ProbeRunner(nil))
		defer c.close()
		if !c.supportsWithin(hw, info.Video(), time.Second) {
			t.Fatal("new FFmpeg build reused old rejection")
		}
	})
}

func main10ProbeRunner(release <-chan struct{}) Runner {
	return &fakeRunner{runFunc: func(ctx context.Context, cmd Command) error {
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if strings.Contains(cmd.Path, "ffprobe") {
			_, err := io.WriteString(cmd.Stdout, `{"streams":[{"codec_name":"hevc","profile":"Main 10","pix_fmt":"yuv420p10le"}]}`)
			return err
		}
		return nil
	}}
}
