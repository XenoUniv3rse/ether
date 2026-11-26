package transcode

import (
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// bitmapPipeline reuses the non-burn hardware tone mapper, but ends at an
// output-sized overlay. The returned overlay tail includes any encoder upload.
// Profile 5 still needs the validated RPU-aware software mapper.
func bitmapPipeline(s runSpec, v *core.StreamInfo, pic picturePlan, accel string, swFmt string) (input, chain []string, overlay, subUpload string, ok bool) {
	f := s.Features
	if !pic.tonemap || pic.tenBit || v.Interlaced || (v.DolbyVision && v.DolbyVisionProfile == 5) || s.PictureCrop != nil || pic.pad || pic.convert != "" || pic.setSAR != "" || pic.w <= 0 || pic.h <= 0 {
		return
	}
	if !f.hasFilter("scale") || !f.hasFilter("format") {
		return
	}
	switch accel {
	case AccelVideoToolbox, AccelNVENC:
		input, chain, ok = gpuPipeline(s, accel, pic.w, pic.h, true, false, false)
	case AccelVAAPI:
		method := s.vaapiToneMethod(v, pic)
		if method == "" || !f.hasFilter("overlay") || !f.hasFilter("hwdownload") {
			return
		}
		var upload string
		input, chain, upload = vaapiToneGraph(method, vaapiDevice(s.Accel), pic.w, pic.h, s.canGPUDecode(), false, true)
		return input, chain, "overlay=eof_action=pass,format=" + swFmt + "," + upload, "", true
	}
	if ok {
		switch {
		case accel == AccelNVENC && f.hasFilter("overlay_cuda") && f.hasFilter("hwupload_cuda"):
			// NV12 overlay_cuda cannot carry subtitle alpha. Convert the main
			// surface to planar 4:2:0 and upload a matching alpha-bearing canvas.
			chain[len(chain)-1] = strings.ReplaceAll(chain[len(chain)-1], "format=nv12", "format=yuv420p")
			return input, chain, "overlay_cuda=eof_action=pass", ",format=yuva420p,hwupload_cuda", true
		}
		if !f.hasFilter("hwdownload") || !f.hasFilter("overlay") {
			return nil, nil, "", "", false
		}
		chain = append(chain, "hwdownload", "format=nv12")
		overlay = "overlay=eof_action=pass,format=" + swFmt
		return input, chain, overlay, "", true
	}
	// QSV's VPP HDR mapper is intentionally not enabled by the non-burn
	// pipeline: its driver support has not been validated. OpenCL can map
	// downloaded decoder frames when that device and filter are available.
	if accel == AccelVAAPI || !f.hwaccels["opencl"] || !f.hasFilter("tonemap_opencl") || !f.hasFilter("hwupload") || !f.hasFilter("hwdownload") || !f.hasFilter("overlay") {
		return nil, nil, "", "", false
	}
	input = []string{"-init_hw_device", "opencl=ocl", "-filter_hw_device", "ocl"}
	switch accel {
	case AccelVideoToolbox:
		input = append(input, "-hwaccel", "videotoolbox")
	case AccelNVENC:
		if v.Codec == "h264" && v.BitDepth > 8 {
			return nil, nil, "", "", false
		}
		input = append(input, "-hwaccel", "cuda")
	case AccelQSV:
		if f.hwaccels["qsv"] && !(v.Codec == "h264" && v.BitDepth > 8) {
			input = append(input, "-hwaccel", "qsv")
		}
	}
	chain = []string{"format=p010le", "hwupload", "tonemap_opencl=tonemap=hable:desat=0:format=nv12:t=bt709:m=bt709:p=bt709", "hwdownload", "format=nv12", fmt.Sprintf("scale=%d:%d", pic.w, pic.h)}
	return input, chain, "overlay=eof_action=pass,format=" + swFmt, "", true
}
