package transcode

import (
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// These are argument-only contracts: no device or FFmpeg process is needed.
func TestBitmapHDRHardwareRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, accel, mapper, scaler, frame, overlay, upload string
	}{
		{"videotoolbox", AccelVideoToolbox, "tonemap_videotoolbox", "scale_vt", "videotoolbox_vld", "overlay=eof_action=pass,format=yuv420p", ""},
		{"cuda-download", AccelNVENC, "tonemap_cuda", "scale_cuda", "cuda", "overlay=eof_action=pass,format=yuv420p", ""},
		{"cuda-overlay", AccelNVENC, "tonemap_cuda", "scale_cuda", "cuda", "overlay_cuda=eof_action=pass", ",format=yuva420p,hwupload_cuda"},
		{"vaapi-download", AccelVAAPI, "tonemap_vaapi", "scale_vaapi", "vaapi", "overlay=eof_action=pass,format=nv12,hwupload", ""},
		{"vaapi-vulkan", AccelVAAPI, "libplacebo", "scale_vaapi", "vaapi", "overlay=eof_action=pass,format=nv12,hwupload=derive_device=vaapi", ""},
		{"qsv-opencl", AccelQSV, "tonemap_opencl", "scale", "", "overlay=eof_action=pass,format=nv12", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := mustParse(t, "mkv_hevc_hdr10.json")
			for i := range info.Streams {
				if info.Streams[i].Index == 4 {
					info.Streams[i].Codec = "hdmv_pgs_subtitle"
				}
			}
			d := core.Decision{Kind: core.DecisionTranscode, Container: ContainerFMP4, VideoCodec: "h264", MaxHeight: 720, AudioStream: 1, AudioCopy: true, SubtitleStream: 4, BurnSubtitles: true, ToneMap: true}
			s := baseSpec(info, d)
			s.Accel = core.HWAccel{Name: tc.accel, Encoders: []string{"h264_" + tc.accel}}
			s.Features.hwaccels = map[string]bool{"videotoolbox": true, "cuda": true, "qsv": true, "vaapi": true}
			s.Features.filters = map[string]bool{"scale": true, "format": true, "overlay": true, "hwdownload": true, "hwupload": true, "hwupload_cuda": true, "tonemapx": true, tc.mapper: true, tc.scaler: true}
			if tc.accel == AccelVAAPI {
				s.Features.vaapiToneMap = func(vaapiToneKey) vaapiToneMethod {
					if !s.Features.filters[tc.mapper] {
						return ""
					}
					if tc.mapper == "libplacebo" {
						return vaapiToneVulkanVPP
					}
					return vaapiToneVPP
				}
			}
			if tc.accel == AccelQSV {
				s.Features.hwaccels["opencl"] = true
			}
			if strings.Contains(tc.name, "-overlay") {
				s.Features.filters[strings.Split(tc.overlay, "=")[0]] = true
			}
			args := buildArgs(s)
			graph := filterGraph(args)
			if tc.mapper == "libplacebo" {
				mustContain(t, args, "-init_hw_device vulkan=vk@va -filter_hw_device vk", "color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv")
			} else {
				mustContain(t, args, "t=bt709:m=bt709:p=bt709")
			}
			mustContain(t, args, tc.mapper+"=", "[0:4]scale=1280:720"+tc.upload+"[sub]", "[base][sub]"+tc.overlay+"[vout]", "-c:v h264_"+tc.accel)
			mustNotContain(t, args, "tonemapx=", "scale=3840:2160")
			if tc.frame != "" {
				mustContain(t, args, "-hwaccel_output_format "+tc.frame, tc.scaler+"=w=1280:h=720")
			}
			if tc.upload != "" {
				mustNotContain(t, args, "hwdownload")
				if tc.accel == AccelNVENC {
					mustContain(t, args, "scale_cuda=w=1280:h=720:format=yuv420p")
				}
			} else if tc.frame != "" {
				if strings.Index(graph, tc.scaler+"=") > strings.Index(graph, "hwdownload") {
					t.Fatalf("download before resize: %s", graph)
				}
			} else {
				mustContain(t, args, "-init_hw_device opencl=ocl -filter_hw_device ocl", "-hwaccel qsv")
			}
			// Seeked Matroska runs keep the existing symmetric timestamp reset;
			// the subtitle reset must occur before its hardware upload.
			s.Decision.Container = ContainerMatroska
			mustContain(t, buildArgs(s), "setpts=PTS-STARTPTS[base]", "[0:4]setpts=PTS-STARTPTS,scale=1280:720"+tc.upload+"[sub]")
			s.Decision.Container = ContainerFMP4
			delete(s.Features.filters, tc.mapper)
			mustContain(t, buildArgs(s), "tonemapx=tonemap=hable")
			mustNotContain(t, buildArgs(s), tc.mapper+"=", "-hwaccel_output_format")
			// RPU reshaping stays on the validated CPU path, even if all hardware
			// filters exist.
			s.Features.filters[tc.mapper] = true
			info.Video().DolbyVision, info.Video().DolbyVisionProfile = true, 5
			s.Info = info
			mustContain(t, buildArgs(s), "tonemapx=", "apply_dovi=1")
			mustNotContain(t, buildArgs(s), tc.mapper+"=")
		})
	}
}

func TestBitmapBurnCroppedCanvas(t *testing.T) {
	info := mustParse(t, "mkv_pgs.json")
	d := core.Decision{Kind: core.DecisionTranscode, Container: ContainerMatroska, VideoCodec: "h264", MaxHeight: 720, AudioStream: 1, AudioCopy: true, SubtitleStream: 2, BurnSubtitles: true}
	s := baseSpec(info, d)
	s.PictureCrop = &pictureCrop{width: 1600, height: 800, x: 160, y: 0}
	args := buildArgs(s)
	mustContain(t, args, "crop=1600:800:160:0,scale=1728:720", "[0:2]setpts=PTS-STARTPTS,crop=iw*1600/1920:ih*800/800:iw*160/1920:ih*0/800:exact=1,scale=1728:720[sub]")
	mustNotContain(t, args, "scale=1920:800")
}
