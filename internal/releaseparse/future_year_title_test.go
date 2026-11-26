package releaseparse

import "testing"

// A year-like number after next year belongs to the title, never the release year.
func TestFutureYearStaysInTitle(t *testing.T) {
	for _, tc := range []struct {
		name, title string
		year        int
	}{
		{"Blade.Runner.2099.S01E01.Hades.1080p.WEBRip.10Bit.DDP5.1.H265-d3g", "Blade Runner 2099", 0},
		{"Blade.Runner.2099.2026.S01E02.1080p.WEB.h264-GRP", "Blade Runner 2099", 2026},
		{"Blade.Runner.2049.2017.1080p.BluRay.x264-SPARKS", "Blade Runner 2049", 2017},
	} {
		info := Parse(tc.name)
		if !TitlesMatch(info.Title, tc.title) || info.Year != tc.year {
			t.Errorf("%s: title %q year %d; want %q %d", tc.name, info.Title, info.Year, tc.title, tc.year)
		}
	}
}
