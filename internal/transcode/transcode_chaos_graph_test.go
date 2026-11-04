//go:build transcodechaos

package transcode

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// Graph validation on a Mac does not establish that another vendor's decoder
// exports RPU metadata. Runtime VAAPI/NVENC validation remains a separate run.
func TestTranscodeChaosDV5Graphs(t *testing.T) {
	for _, accel := range []string{AccelNone, AccelVideoToolbox, AccelNVENC, AccelVAAPI, AccelQSV} {
		t.Run(accel, func(t *testing.T) {
			info := mustParse(t, "mkv_hevc_hdr10.json")
			v := info.Video()
			v.DolbyVision, v.DolbyVisionProfile = true, 5
			s := baseSpec(info, Decide(info, plexWeb, allow(), Policy{}))
			s.Accel = core.HWAccel{Name: accel, Encoders: []string{"h264_" + accel}}
			s.Features.hwaccels = map[string]bool{"cuda": true, "videotoolbox": true, "qsv": true}
			for _, f := range []string{"scale_cuda", "tonemap_cuda", "scale_vt", "tonemap_videotoolbox", "scale_vaapi", "tonemap_vaapi", "vpp_qsv", "tonemapx"} {
				s.Features.filters[f] = true
			}
			args := buildArgs(s)
			mustContain(t, args, "tonemapx=", "apply_dovi=1")
			mustNotContain(t, args, "tonemap_vaapi=", "tonemap_cuda=", "tonemap_videotoolbox=", "zscale=")
		})
	}
}
