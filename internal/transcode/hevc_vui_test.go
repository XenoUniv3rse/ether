package transcode

import (
	"encoding/hex"
	"testing"
)

// SPS syntax varies independently of the decoder's choice to tolerate it.
func TestHEVCSPSValidation(t *testing.T) {
	for _, tc := range []struct {
		name, sps string
		valid     bool
	}{
		{"x265 8-bit timing", "42010101600000030090000003000003001ea020810596566924caf0168080000003008000000c04", true},
		{"x265 10-bit timing", "42010102200000030090000003000003001ea0208104d96566924caf016808000003000800000300c040", true},
		{"x265 HRD", "42010101600000030090000003000003003ca020810596566924caf0168080000003008000000c300a4b2f0007a12000f42440", true},
		{"x265 no timing", "42010101600000030090000003000003001ea020810596566924caf0168010", true},
		{"HDR10 fixture", "42010102200000030090000003000003001ea0102049365959a4932bc05a848804820000030002000003003010", true},
		{"invalid chroma locations", "420101022000000300900000030000030096a003c08010e4d96762aed20ab50102020400000fa40001770020", false},
		{"missing stop bit", "42010101600000030090000003000003001ea020810596566924caf0168000", false},
		{"truncated", "42010101600000030090000003000003001ea020810596566924caf01680", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := hex.DecodeString(tc.sps)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseHEVCSPS(b, true); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}
