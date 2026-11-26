package transcode

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const vaapiDeinterlace = "deinterlace_vaapi=rate=frame:mode=default"

// A driver can accept the VPP call and still leave the fields combed.
// Admit the filter only when it removes combing from a known interlaced
// moving picture, allowing idet's initial classification to settle.
func (d hwDetector) probeVAAPIDeinterlace(ctx context.Context, device string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	const key = "lavfi.idet.multiple.current_frame"
	var stdout bytes.Buffer
	err := d.runner.Run(ctx, Command{Path: d.ffmpeg, Stdout: &stdout, Args: []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-xerror", "-vaapi_device", device,
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=60,tinterlace=mode=interleave_top,format=nv12",
		"-vf", "hwupload," + vaapiDeinterlace + ",hwdownload,format=nv12,idet,metadata=mode=print:key=" + key + ":file=-",
		"-frames:v", "32", "-f", "null", "-",
	}})
	if err != nil {
		return false
	}
	frames, progressive := strings.Count(stdout.String(), key), strings.Count(stdout.String(), key+"=progressive")
	return frames > 0 && progressive*4 >= frames*3
}

type vaapiToneMethod string

const (
	vaapiToneVPP       vaapiToneMethod = "vaapi"
	vaapiToneOpenCL    vaapiToneMethod = "opencl"
	vaapiToneVulkan    vaapiToneMethod = "vulkan"
	vaapiToneVulkanVPP vaapiToneMethod = "vulkan-vpp"
)

type vaapiToneKey struct {
	source, encoder string
	tenBit, full    bool
}

func vaapiScale(w, h int, pixel string) string {
	return fmt.Sprintf("scale_vaapi=w=%d:h=%d:format=%s", w, h, pixel)
}

func vaapiDevice(hw core.HWAccel) string {
	if hw.Device != "" {
		return hw.Device
	}
	return DefaultVAAPIDevice
}

func (s runSpec) vaapiToneMethod(v *core.StreamInfo, pic picturePlan) vaapiToneMethod {
	if s.vaapiToneDecision != nil {
		return s.vaapiToneDecision.method
	}
	if s.Features.vaapiMeasured && vaapiCPUToneFilter(s.Features, pic.tenBit) != "" {
		return ""
	}
	if s.Features.vaapiToneMap == nil || s.Accel.NoGPUTonemap {
		return ""
	}
	codec := s.Decision.VideoCodec
	if codec == "" {
		codec = "h264"
	}
	return s.Features.vaapiToneMap(vaapiToneKey{source: encodedRange(v), encoder: codec + "_vaapi", tenBit: pic.tenBit, full: pictureOf(v).full})
}

// The Vulkan device is derived from the selected render node. The copy path
// avoids importing radeonsi's decoded DRM modifiers into RADV, which can fail
// even when both APIs individually support P010.
func vaapiToneGraph(method vaapiToneMethod, device string, w, h int, decode, tenBit, softwareOut bool) (input, chain []string, upload string) {
	pixel := "nv12"
	if tenBit {
		pixel = "p010le"
	}
	input = []string{"-init_hw_device", "vaapi=va:" + device}
	upload = "hwupload"
	switch method {
	case vaapiToneVulkan, vaapiToneVulkanVPP:
		input = append(input, "-init_hw_device", "vulkan=vk@va", "-filter_hw_device", "vk")
		upload = "hwupload=derive_device=vaapi"
	case vaapiToneOpenCL:
		input = append(input, "-init_hw_device", "opencl=ocl@va", "-filter_hw_device", "va")
	default:
		input = append(input, "-filter_hw_device", "va")
	}
	if decode {
		input = append(input, "-hwaccel", "vaapi", "-hwaccel_device", "va", "-hwaccel_output_format", "vaapi")
	} else if method != vaapiToneVulkan {
		chain = append(chain, "format=p010le", upload)
	}
	switch method {
	case vaapiToneVPP:
		chain = append(chain, "tonemap_vaapi=format="+pixel+":t=bt709:m=bt709:p=bt709", vaapiScale(w, h, pixel))
	case vaapiToneOpenCL:
		chain = append(chain, "hwmap=derive_device=opencl",
			"tonemap_opencl=tonemap=hable:desat=0:format=nv12:t=bt709:m=bt709:p=bt709",
			"hwmap=derive_device=vaapi:reverse=1:extra_hw_frames=16", "format=vaapi", vaapiScale(w, h, pixel))
	case vaapiToneVulkan, vaapiToneVulkanVPP:
		if method == vaapiToneVulkanVPP {
			chain = append(chain, vaapiScale(w, h, "p010le"))
		}
		if decode || method == vaapiToneVulkanVPP {
			chain = append(chain, "hwdownload", "format=p010le")
		}
		chain = append(chain, fmt.Sprintf("libplacebo=w=%d:h=%d:format=%s:tonemapping=hable:color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv", w, h, pixel), "format="+pixel)
		if !softwareOut {
			chain = append(chain, upload)
		}
		return input, chain, upload
	}
	if softwareOut {
		chain = append(chain, "hwdownload", "format="+pixel)
	}
	return input, chain, upload
}

// Register a fixed set of demand-only probes for this manager's device and
// FFmpeg build. Concurrent sessions requesting the same conversion share it.
func newVAAPIToneMapper(d hwDetector, f features, hw core.HWAccel, ffprobe, dir string) func(vaapiToneKey) vaapiToneMethod {
	probes := map[vaapiToneKey]func() vaapiToneMethod{}
	if hw.Name == AccelVAAPI && !hw.NoGPUTonemap && f.hasEncoder("libx265") && f.hwaccels["vaapi"] {
		for _, source := range []string{rangeHDR10, rangeHLG} {
			for _, encoder := range hw.Encoders {
				for _, tenBit := range []bool{false, true} {
					if tenBit && (encoder != "hevc_vaapi" || hw.VAAPIQualityRC[vaapiMain10] == "") {
						continue
					}
					for _, full := range []bool{false, true} {
						key := vaapiToneKey{source: source, encoder: encoder, tenBit: tenBit, full: full}
						candidates := vaapiToneCandidates(f, hw, key)
						if len(candidates) > 0 {
							probes[key] = sync.OnceValue(func() vaapiToneMethod {
								return probeVAAPIToneMap(d, hw, ffprobe, dir, key, candidates)
							})
						}
					}
				}
			}
		}
	}
	if len(probes) == 0 {
		return nil
	}
	return func(key vaapiToneKey) vaapiToneMethod {
		if probe := probes[key]; probe != nil {
			return probe()
		}
		return ""
	}
}

func vaapiToneCandidates(f features, hw core.HWAccel, key vaapiToneKey) []vaapiToneMethod {
	var candidates []vaapiToneMethod
	if !key.full && key.source == rangeHDR10 && !hw.NoGPUScale && f.hasFilter("tonemap_vaapi") {
		candidates = append(candidates, vaapiToneVPP)
	}
	if !key.full && !key.tenBit && !hw.NoGPUScale && f.hwaccels["opencl"] && f.hasFilter("hwmap") && f.hasFilter("tonemap_opencl") {
		candidates = append(candidates, vaapiToneOpenCL)
	}
	if f.hwaccels["vulkan"] && f.hasFilter("libplacebo") && f.hasFilter("hwdownload") && f.hasFilter("hwupload") {
		if !hw.NoGPUScale {
			candidates = append(candidates, vaapiToneVulkanVPP)
		}
		candidates = append(candidates, vaapiToneVulkan)
	}
	return candidates
}

func probeVAAPIToneMap(d hwDetector, hw core.HWAccel, ffprobe, dir string, key vaapiToneKey, candidates []vaapiToneMethod) vaapiToneMethod {
	tmp, err := os.MkdirTemp(dir, "vaapi-tonemap-")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	sample, out := filepath.Join(tmp, "hdr.mkv"), filepath.Join(tmp, "sdr.mkv")
	src := vaapiHDRSample(key, 320, 240, 1)
	prefix := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-xerror", "-abort_on", "empty_output"}
	args := append(append([]string{}, prefix...), "-f", "lavfi", "-i", src, "-c:v", "libx265", "-preset", "ultrafast",
		"-x265-params", "pools=none:frame-threads=1:log-level=error"+vaapiHDRMetadata(key.source), sample)
	if err := d.testRun(ctx, args); err != nil {
		d.log.Debug("VAAPI HDR probe sample failed", "err", err)
		return ""
	}
	codec := strings.TrimSuffix(key.encoder, "_vaapi")
	spec := runSpec{Accel: hw, Rate: rateControl{TargetKbps: 1000}}
	pic := picturePlan{w: 160, h: 120, tenBit: key.tenBit}
	encode := planEncodeArgs(spec, pic, key.encoder, codec, true)
	wantPixel := "yuv420p"
	if key.tenBit {
		wantPixel = "yuv420p10le"
	}
	for _, method := range candidates {
		input, chain, _ := vaapiToneGraph(method, hw.Device, pic.w, pic.h, true, key.tenBit, false)
		args = append(append([]string{}, prefix...), input...)
		args = append(args, "-i", sample, "-vf", strings.Join(chain, ","), "-an")
		args = append(args, encode...)
		args = append(args, "-y", out)
		if err := d.testRun(ctx, args); err != nil {
			d.log.Debug("VAAPI HDR probe failed", "device", hw.Device, "method", method, "err", err)
			continue
		}
		var stdout bytes.Buffer
		err := d.runner.Run(ctx, Command{Path: ffprobe, Args: []string{"-v", "error", "-select_streams", "v:0",
			"-show_entries", "stream=pix_fmt,color_range,color_space,color_transfer,color_primaries", "-of", "csv=p=0", out}, Stdout: &stdout})
		if err == nil && strings.HasPrefix(strings.TrimSpace(stdout.String())+",", wantPixel+",tv,bt709,bt709,bt709,") {
			d.log.Debug("VAAPI HDR probe passed", "device", hw.Device, "method", method, "source", key.source, "encoder", key.encoder, "tenBit", key.tenBit)
			return method
		}
	}
	return ""
}

func vaapiHDRSample(key vaapiToneKey, w, h, seconds int) string {
	transfer, rng := "smpte2084", "tv"
	if key.source == rangeHLG {
		transfer = "arib-std-b67"
	}
	if key.full {
		rng = "pc"
	}
	return fmt.Sprintf("testsrc2=size=%dx%d:rate=30:duration=%d,format=yuv420p10le,setparams=range=%s:color_primaries=bt2020:colorspace=bt2020nc:color_trc=%s", w, h, seconds, rng, transfer)
}

func vaapiHDRMetadata(source string) string {
	if source == rangeHLG {
		return ""
	}
	return ":hdr10=1:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,50):max-cll=1000,400"
}
