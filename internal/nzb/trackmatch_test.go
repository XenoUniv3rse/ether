package nzb

import "testing"

func TestMatchTrackIndexSceneRARAndMultiDisc(t *testing.T) {
	sceneAlbum := []string{
		"00-metallica-72_seasons-2023-moonblood.nfo",
		"00-metallica-72_seasons-2023-moonblood.m3u",
		"00-metallica-72_seasons-2023-moonblood.sfv",
		"cover.jpg",
		"01-metallica-72_seasons-moonblood.flac",
		"02-metallica-shadows_follow-moonblood.flac",
		"03-metallica-screaming_suicide-moonblood.flac",
		"04-metallica-sleepwalk_my_life_away-moonblood.flac",
		"05-metallica-you_must_burn-moonblood.flac",
		"06-metallica-lux_aeterna-moonblood.flac",
	}

	// Track number + title matches scene member even though exact fileHint wouldn't.
	idx, ok, ambig := MatchTrackIndex(sceneAlbum, TrackHint{Number: 5, Title: "You Must Burn!"}, "audio/flac")
	if !ok || ambig || idx != 8 {
		t.Fatalf("MatchTrackIndex(num=5, title) = (%d, %v, %v), want (8, true, false)", idx, ok, ambig)
	}

	// Single-disc album accepts disc=1 when files carry no disc tags.
	idx, ok, ambig = MatchTrackIndex(sceneAlbum, TrackHint{Disc: 1, Number: 5, Title: "You Must Burn!"}, "audio/*")
	if !ok || ambig || idx != 8 {
		t.Fatalf("MatchTrackIndex(disc=1, num=5) = (%d, %v, %v), want (8, true, false)", idx, ok, ambig)
	}

	// Title-only fallback matches normalised tokens (ignoring punctuation & underscores).
	idx, ok, ambig = MatchTrackIndex(sceneAlbum, TrackHint{Title: "You Must Burn!"}, "audio/flac")
	if !ok || ambig || idx != 8 {
		t.Fatalf("MatchTrackIndex(title only) = (%d, %v, %v), want (8, true, false)", idx, ok, ambig)
	}

	// Non-matching track number + title fails cleanly.
	idx, ok, ambig = MatchTrackIndex(sceneAlbum, TrackHint{Number: 99, Title: "Missing Song"}, "audio/flac")
	if ok || ambig || idx != -1 {
		t.Fatalf("MatchTrackIndex(missing) = (%d, %v, %v), want (-1, false, false)", idx, ok, ambig)
	}

	// Exact fileHint (MatchPathIndex) semantics remain unchanged: "05 You Must Burn!" does NOT match.
	if _, ok, _ := MatchPathIndex(sceneAlbum, "05 You Must Burn!"); ok {
		t.Fatalf("MatchPathIndex should keep exact semantics and reject fuzzy hint")
	}

	// Multi-disc release: disc disambiguates, missing disc without title is ambiguous, title breaks tie.
	multiDisc := []string{
		"CD1/05-artist-first_song.flac",
		"CD2/05-artist-second_song.flac",
		"CD1/01-artist-interlude.flac",
		"CD2/09-artist-interlude.flac",
	}
	if _, ok, ambig := MatchTrackIndex(multiDisc, TrackHint{Number: 5}, "audio/flac"); ok || !ambig {
		t.Fatalf("multi-disc num=5 without disc/title: ok=%v ambig=%v, want false/true", ok, ambig)
	}
	if idx, ok, ambig := MatchTrackIndex(multiDisc, TrackHint{Disc: 2, Number: 5}, "audio/flac"); !ok || ambig || idx != 1 {
		t.Fatalf("multi-disc disc=2 num=5 = (%d, %v, %v), want (1, true, false)", idx, ok, ambig)
	}
	if idx, ok, ambig := MatchTrackIndex(multiDisc, TrackHint{Number: 5, Title: "Second Song"}, "audio/flac"); !ok || ambig || idx != 1 {
		t.Fatalf("multi-disc num=5 title tiebreak = (%d, %v, %v), want (1, true, false)", idx, ok, ambig)
	}
	if _, ok, ambig := MatchTrackIndex(multiDisc, TrackHint{Title: "Interlude"}, "audio/flac"); ok || !ambig {
		t.Fatalf("duplicate title without number: ok=%v ambig=%v, want false/true", ok, ambig)
	}
	if idx, ok, ambig := MatchTrackIndex(multiDisc, TrackHint{Disc: 2, Title: "Interlude"}, "audio/flac"); !ok || ambig || idx != 3 {
		t.Fatalf("duplicate title disambiguated by disc = (%d, %v, %v), want (3, true, false)", idx, ok, ambig)
	}

	// Compact disc-track prefix "2-05" works too.
	compact := []string{"1-05_alpha.mp3", "2-05_beta.mp3"}
	if idx, ok, ambig := MatchTrackIndex(compact, TrackHint{Disc: 2, Number: 5}, "audio/mpeg"); !ok || ambig || idx != 1 {
		t.Fatalf("compact 2-05 = (%d, %v, %v), want (1, true, false)", idx, ok, ambig)
	}
}

func TestMatchTrackIndexMultiDiscEdgeCases(t *testing.T) {
	t.Run("zero-padded DD-NN and single-digit D-N prefixes", func(t *testing.T) {
		padded := []string{
			"01-01. Bad.flac",
			"01-02. The Way You Make Me Feel.flac",
			"02-01. Don't Be Messin' 'Round.flac",
			"02-02. I'm So Blue.flac",
		}
		if idx, ok, ambig := MatchTrackIndex(padded, TrackHint{Disc: 1, Number: 1}, "audio/flac"); !ok || ambig || idx != 0 {
			t.Fatalf("01-01 got (%d, %v, %v), want (0, true, false)", idx, ok, ambig)
		}
		if idx, ok, ambig := MatchTrackIndex(padded, TrackHint{Disc: 2, Number: 1}, "audio/flac"); !ok || ambig || idx != 2 {
			t.Fatalf("02-01 got (%d, %v, %v), want (2, true, false)", idx, ok, ambig)
		}
		if idx, ok, ambig := MatchTrackIndex(padded, TrackHint{Disc: 1, Number: 2, Title: "The Way You Make Me Feel"}, "audio/flac"); !ok || ambig || idx != 1 {
			t.Fatalf("01-02 got (%d, %v, %v), want (1, true, false)", idx, ok, ambig)
		}

		// Date-like or single-disc titles with numbers must not be misread as DD-NN.
		singleWithDates := []string{
			"01 - 02-03-2020.flac",
			"02 - 02 Degrees.flac",
			"03 - 1-2 Step.flac",
		}
		if idx, ok, ambig := MatchTrackIndex(singleWithDates, TrackHint{Disc: 1, Number: 1, Title: "02-03-2020"}, "audio/flac"); !ok || ambig || idx != 0 {
			t.Fatalf("singleWithDates track 1 got (%d, %v, %v), want (0, true, false)", idx, ok, ambig)
		}
		if idx, ok, ambig := MatchTrackIndex(singleWithDates, TrackHint{Disc: 1, Number: 2, Title: "02 Degrees"}, "audio/flac"); !ok || ambig || idx != 1 {
			t.Fatalf("singleWithDates track 2 got (%d, %v, %v), want (1, true, false)", idx, ok, ambig)
		}

		// Single-digit 1-1 / 2-1 in a multi-disc album.
		singleDigitDisc := []string{
			"1-1 First.flac",
			"1-2 Second.flac",
			"2-1 Bonus.flac",
		}
		if idx, ok, ambig := MatchTrackIndex(singleDigitDisc, TrackHint{Disc: 2, Number: 1}, "audio/flac"); !ok || ambig || idx != 2 {
			t.Fatalf("1-digit D-N 2-1 got (%d, %v, %v), want (2, true, false)", idx, ok, ambig)
		}
		// Single-track "1-2 Step" without other D-N tracks stays untagged.
		singleStep := []string{"1-2 Step.flac", "02 - Other.flac"}
		if idx, ok, ambig := MatchTrackIndex(singleStep, TrackHint{Disc: 1, Number: 1, Title: "1-2 Step"}, "audio/flac"); !ok || ambig || idx != 0 {
			t.Fatalf("singleStep got (%d, %v, %v), want (0, true, false)", idx, ok, ambig)
		}
	})

	t.Run("3-digit DTT inside disc directories and flattened 2-CD fallback", func(t *testing.T) {
		bad25 := []string{
			"CD1of2/101-michael_jackson-bad.flac",
			"CD1of2/102-michael_jackson-the_way_you_make_me_feel.flac",
			"Disc2of2/201-michael_jackson-dont_be_messin_round.flac",
			"Disc2of2/202-michael_jackson-im_so_blue.flac",
		}
		if idx, ok, ambig := MatchTrackIndex(bad25, TrackHint{Disc: 1, Number: 1, Title: "Bad (2012 Remaster)"}, "audio/flac"); !ok || ambig || idx != 0 {
			t.Fatalf("CD1of2/101 got (%d, %v, %v), want (0, true, false)", idx, ok, ambig)
		}
		if idx, ok, ambig := MatchTrackIndex(bad25, TrackHint{Disc: 2, Number: 1}, "audio/flac"); !ok || ambig || idx != 2 {
			t.Fatalf("Disc2of2/201 got (%d, %v, %v), want (2, true, false)", idx, ok, ambig)
		}
		// Flattened catalog metadata (disc=1, number=12) finds disc 2 track by title.
		if idx, ok, ambig := MatchTrackIndex(bad25, TrackHint{Disc: 1, Number: 12, Title: "Don't Be Messin' 'Round"}, "audio/flac"); !ok || ambig || idx != 2 {
			t.Fatalf("flattened disc=1 num=12 got (%d, %v, %v), want (2, true, false)", idx, ok, ambig)
		}
	})

	t.Run("mixed disc tagging (untagged disc 1 + tagged disc 2)", func(t *testing.T) {
		mixed := []string{
			"Original Album/01 - Bad.flac",
			"Original Album/02 - The Way You Make Me Feel.flac",
			"Bonus Disc 2/01 - Don't Be Messin' 'Round.flac",
		}
		if idx, ok, ambig := MatchTrackIndex(mixed, TrackHint{Disc: 1, Number: 1, Title: "Bad"}, "audio/flac"); !ok || ambig || idx != 0 {
			t.Fatalf("mixed disc 1 got (%d, %v, %v), want (0, true, false)", idx, ok, ambig)
		}
		if idx, ok, ambig := MatchTrackIndex(mixed, TrackHint{Disc: 2, Number: 1, Title: "Don't Be Messin' 'Round"}, "audio/flac"); !ok || ambig || idx != 2 {
			t.Fatalf("mixed disc 2 got (%d, %v, %v), want (2, true, false)", idx, ok, ambig)
		}
	})

	t.Run("false Vol 2 album title vs real Vol 1 and Vol 2 folders", func(t *testing.T) {
		shakira := []string{
			"Shakira - Oral Fixation Vol 2/01 - How Do You Do.flac",
			"Shakira - Oral Fixation Vol 2/07 - Hips Don't Lie.flac",
		}
		if idx, ok, ambig := MatchTrackIndex(shakira, TrackHint{Disc: 1, Number: 1, Title: "How Do You Do"}, "audio/flac"); !ok || ambig || idx != 0 {
			t.Fatalf("Oral Fixation Vol 2 got (%d, %v, %v), want (0, true, false)", idx, ok, ambig)
		}

		realVols := []string{
			"Greatest Hits Vol 1/01 - Alpha.flac",
			"Greatest Hits Vol 2/01 - Beta.flac",
		}
		if idx, ok, ambig := MatchTrackIndex(realVols, TrackHint{Disc: 1, Number: 1}, "audio/flac"); !ok || ambig || idx != 0 {
			t.Fatalf("realVols disc 1 got (%d, %v, %v), want (0, true, false)", idx, ok, ambig)
		}
		if idx, ok, ambig := MatchTrackIndex(realVols, TrackHint{Disc: 2, Number: 1}, "audio/flac"); !ok || ambig || idx != 1 {
			t.Fatalf("realVols disc 2 got (%d, %v, %v), want (1, true, false)", idx, ok, ambig)
		}
	})

	t.Run("artist-prefixed filenames, edition-stripped titles, and shifted track numbers", func(t *testing.T) {
		artistPrefixed := []string{
			"Michael Jackson - 1-01 - Bad.flac",
			"Michael Jackson - 01-02 - Speed Demon.flac",
			"Michael Jackson - 201 - Don't Be Messin' 'Round.flac",
		}
		if idx, ok, ambig := MatchTrackIndex(artistPrefixed, TrackHint{Disc: 1, Number: 1}, "audio/flac"); !ok || ambig || idx != 0 {
			t.Fatalf("artist-prefixed 1-01 got (%d, %v, %v), want (0, true, false)", idx, ok, ambig)
		}
		if idx, ok, ambig := MatchTrackIndex(artistPrefixed, TrackHint{Disc: 1, Number: 2}, "audio/flac"); !ok || ambig || idx != 1 {
			t.Fatalf("artist-prefixed 01-02 got (%d, %v, %v), want (1, true, false)", idx, ok, ambig)
		}
		if idx, ok, ambig := MatchTrackIndex(artistPrefixed, TrackHint{Disc: 2, Number: 1}, "audio/flac"); !ok || ambig || idx != 2 {
			t.Fatalf("artist-prefixed 201 got (%d, %v, %v), want (2, true, false)", idx, ok, ambig)
		}

		// Edition-suffixed title matches bare filename; shifted track number resolves uniquely by title.
		shifted := []string{
			"01 - Intro.flac",
			"02 - Bad.flac",
			"10 - Smooth Criminal.flac",
		}
		if idx, ok, ambig := MatchTrackIndex(shifted, TrackHint{Disc: 1, Number: 1, Title: "Bad (2012 Remaster)"}, "audio/flac"); !ok || ambig || idx != 1 {
			t.Fatalf("shifted Bad got (%d, %v, %v), want (1, true, false)", idx, ok, ambig)
		}
		if idx, ok, ambig := MatchTrackIndex(shifted, TrackHint{Disc: 1, Number: 9, Title: "Smooth Criminal (Remastered Radio Edit)"}, "audio/flac"); !ok || ambig || idx != 2 {
			t.Fatalf("edition-stripped Smooth Criminal got (%d, %v, %v), want (2, true, false)", idx, ok, ambig)
		}

		// Purely numeric filenames still match by track number.
		numeric := []string{"01.flac", "02.flac"}
		if idx, ok, ambig := MatchTrackIndex(numeric, TrackHint{Disc: 1, Number: 1, Title: "Bad"}, "audio/flac"); !ok || ambig || idx != 0 {
			t.Fatalf("numeric 01.flac got (%d, %v, %v), want (0, true, false)", idx, ok, ambig)
		}
	})
}

func TestParseDirDisc(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"CD1", 1},
		{"Artist - Album/CD2/01.flac", 2},
		{"Disc 2 of 2", 2},
		{"CD1of2", 1},
		{"Artist.Album.CD2.2012-GRP", 2},
		{"Shakira - Oral Fixation Vol 2", 2},
		{"Artist - Vol 2 - CD1", 1},
		{"Single Album", 0},
	}
	for _, c := range cases {
		if got := ParseDirDisc(c.in); got != c.want {
			t.Fatalf("ParseDirDisc(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
