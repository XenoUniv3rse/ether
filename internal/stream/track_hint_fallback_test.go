package stream

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/archive"
	"github.com/braedonsaunders/ether/internal/nzb"
)

// A plugin hint that names no archive member exactly but reads as a track
// ("1-03 Title", "01 Title") finds the track instead of failing (#753).
func TestChooseNonVideoMemberFallsBackToTrackFromHint(t *testing.T) {
	members := []archive.Member{
		{Name: "Artist - Album/CD1/1-01 Jam.flac", Size: 10},
		{Name: "Artist - Album/CD1/1-02 Why You Wanna Trip on Me.flac", Size: 10},
		{Name: "Artist - Album/CD1/1-03 They Don't Care About Us.flac", Size: 10},
		{Name: "Artist - Album/CD2/2-01 I'll Be There.flac", Size: 10},
		{Name: "Artist - Album/cover.jpg", Size: 5},
	}
	cases := []struct{ hint, want string }{
		{"1-03 They Don't Care About Us", "Artist - Album/CD1/1-03 They Don't Care About Us.flac"},
		{"2-01 I'll Be There", "Artist - Album/CD2/2-01 I'll Be There.flac"},
	}
	for _, c := range cases {
		got, err := chooseNonVideoMember(members, nzb.NonVideoHint{Member: c.hint, MIME: "audio/flac"})
		if err != nil {
			t.Fatalf("%q: %v", c.hint, err)
		}
		if got.Name != c.want {
			t.Fatalf("%q: got %q, want %q", c.hint, got.Name, c.want)
		}
	}
	// A real file name that is not in the archive still fails.
	if _, err := chooseNonVideoMember(members, nzb.NonVideoHint{Member: "Other Album/05 Song.flac", MIME: "audio/flac"}); err == nil {
		t.Fatal("unmatched real path should fail")
	}
}

func TestTrackHintFromName(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		disc  int
		num   int
		title string
	}{
		{"1-03 They Don't Care About Us", true, 1, 3, "They Don't Care About Us"},
		{"CD2/03 Interlude", true, 2, 3, "Interlude"},
		{"01 I'll Be There", true, 0, 1, "I'll Be There"},
		{"07 - Mr. Brightside", true, 0, 7, "Mr. Brightside"},
		{"03 Song.flac", false, 0, 0, ""},
		{"Intro", false, 0, 0, ""},
	}
	for _, c := range cases {
		h, ok := trackHintFromName(c.in)
		if ok != c.ok || (ok && (h.Disc != c.disc || h.Number != c.num || h.Title != c.title)) {
			t.Fatalf("%q: got %+v ok=%v", c.in, h, ok)
		}
	}
}
