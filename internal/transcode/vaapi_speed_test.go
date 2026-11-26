package transcode

import "testing"

func TestVAAPIToneAdmission(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		cpu, gpu             float64
		busy                 bool
		activeCPU, activeGPU int
		route                string
	}{
		{"cold", 0, 0, false, 0, 0, "cpu"},
		{"faster CPU", 5.8, 1.38, false, 0, 0, "cpu"},
		{"busy CPU with GPU headroom", 3, 1.6, true, 0, 0, "gpu"},
		{"busy CPU with marginal GPU", 3, 1.38, true, 0, 0, "cpu"},
		{"faster GPU", 1.3, 1.4, false, 0, 0, "gpu"},
		{"GPU contention prefers CPU with spare CPU throughput", 3, 1.79, false, 0, 1, "cpu"},
		{"second 4K conversion fits on GPU", 1.46, 1.81, false, 0, 1, "gpu"},
		{"third 4K conversion exhausts shared bandwidth", 1.46, 1.81, false, 0, 2, "unavailable"},
		{"third 720p conversion can offload", 4.07, 3.48, true, 2, 0, "gpu"},
		{"GPU contention exhausts headroom", 1.1, 1.5, true, 0, 1, "unavailable"},
		{"only GPU meets floor", 1.1, 1.3, false, 0, 0, "gpu"},
		{"neither meets floor", 1.1, 1.15, true, 0, 0, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := vaapiSpeedResult{cpu: tc.cpu, gpu: tc.gpu, method: vaapiToneVulkanVPP}
			got := result.choose(720, tc.busy, tc.activeCPU, tc.activeGPU)
			if got.Route != tc.route || (got.method != "") != (tc.route == "gpu") {
				t.Fatalf("got %+v, want %s", got, tc.route)
			}
		})
	}
}
