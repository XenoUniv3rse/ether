package stream

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	at "github.com/braedonsaunders/ether/internal/archive/archivetest"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nzb"
)

func TestListArchiveMembersAndTrackHintSceneRARZIPAnd7z(t *testing.T) {
	t.Run("scene RAR5 members cached + track hint + unchanged fileHint", func(t *testing.T) {
		f := newFakeFetcher()
		e := newTestEngine(t, f, nil, Options{})

		track1 := flacBytes(501, 96_000)
		track5 := flacBytes(505, 180_000)
		nfo := []byte("Metallica - 72 Seasons (2023) - MOONBLOOD\n")

		vols, err := at.BuildRAR([]at.RARFile{
			{Name: "00-metallica-72_seasons-2023-moonblood.nfo", Data: nfo},
			{Name: "01-metallica-72_seasons-moonblood.flac", Data: track1},
			{Name: "05-metallica-you_must_burn-moonblood.flac", Data: track5},
		}, at.RAROptions{Version: 5, VolumeSize: 160_000})
		if err != nil {
			t.Fatalf("BuildRAR: %v", err)
		}
		var files []postFile
		for i, v := range vols {
			files = append(files, postFile{name: fmt.Sprintf("metallica.part%02d.rar", i+1), data: v})
		}
		rawNZB := post(t, f, 64*1024, files...)

		// 1. ListArchiveMembers reads the archive directory and keys it by first volume index (0).
		byFile, err := e.ListArchiveMembers(context.Background(), rawNZB)
		if err != nil {
			t.Fatalf("ListArchiveMembers(RAR5): %v", err)
		}
		members := byFile[0]
		if len(members) != 3 {
			t.Fatalf("RAR5 members = %+v, want 3 entries", members)
		}
		var foundTrack5 bool
		for _, m := range members {
			if m.Path == "05-metallica-you_must_burn-moonblood.flac" && m.Size == int64(len(track5)) {
				foundTrack5 = true
			}
		}
		if !foundTrack5 {
			t.Fatalf("expected scene track 5 in members: %+v", members)
		}

		// 2. Second call hits cache with ZERO additional NNTP article fetches.
		fetchesBefore := f.totalCalls()
		byFile2, err := e.ListArchiveMembers(context.Background(), rawNZB)
		if err != nil {
			t.Fatalf("ListArchiveMembers cached: %v", err)
		}
		if len(byFile2[0]) != 3 {
			t.Fatalf("cached members = %+v", byFile2[0])
		}
		if got := f.totalCalls(); got != fetchesBefore {
			t.Fatalf("cached ListArchiveMembers performed %d extra NNTP fetches, want 0", got-fetchesBefore)
		}

		// 3. A track-style fileHint that names no member exactly ("05 You Must
		// Burn!") falls back to track matching (#753); an unmatched real path
		// still fails with archive holds no member.
		fuzzyHintNZB := annotateNonVideoNZB(t, rawNZB, nzb.NonVideoHint{
			Member: "05 You Must Burn!",
			MIME:   "audio/flac",
		})
		fuzzySrc := openSource(t, e, "rar-fuzzy-hint", fuzzyHintNZB)
		if fuzzySrc.Name() != "05-metallica-you_must_burn-moonblood.flac" {
			t.Fatalf("track-style fileHint opened %q", fuzzySrc.Name())
		}
		badHintNZB := annotateNonVideoNZB(t, rawNZB, nzb.NonVideoHint{
			Member: "Other Album/05-someone-else.flac",
			MIME:   "audio/flac",
		})
		if _, err := e.Open(context.Background(), "rar-bad-hint", badHintNZB); !errors.Is(err, core.ErrNoUsableFile) || !strings.Contains(err.Error(), "archive holds no member") || strings.Contains(err.Error(), "video") {
			t.Fatalf("unmatched path fileHint err = %v, want media-neutral 'archive holds no member'", err)
		}

		// 4. Exact member name discovered from ListArchiveMembers succeeds.
		exactHintNZB := annotateNonVideoNZB(t, rawNZB, nzb.NonVideoHint{
			Member: "05-metallica-you_must_burn-moonblood.flac",
			MIME:   "audio/flac",
		})
		exactSrc := openSource(t, e, "rar-exact-member", exactHintNZB)
		if !bytes.Equal(readAll(t, exactSrc, 32*1024), track5) {
			t.Fatal("exact member bytes mismatch")
		}

		// 5. Structured release.track hint matches scene member without needing exact inner name.
		trackHintNZB := annotateNonVideoNZB(t, rawNZB, nzb.NonVideoHint{
			MIME:  "audio/flac",
			Track: &nzb.TrackHint{Number: 5, Title: "You Must Burn!"},
		})
		trackSrc := openSource(t, e, "rar-track-hint", trackHintNZB)
		if trackSrc.Name() != "05-metallica-you_must_burn-moonblood.flac" {
			t.Fatalf("trackSrc.Name() = %q", trackSrc.Name())
		}
		if !bytes.Equal(readAll(t, trackSrc, 32*1024), track5) {
			t.Fatal("track-hint member bytes mismatch")
		}
	})

	t.Run("ZIP archive members + ambiguous track hint fails", func(t *testing.T) {
		f := newFakeFetcher()
		e := newTestEngine(t, f, nil, Options{})

		cd1 := flacBytes(601, 48_000)
		cd2 := flacBytes(602, 52_000)
		zipData := buildZipArchive(t, zip.Store, []struct {
			name string
			data []byte
		}{
			{name: "CD1/05 - Alpha.flac", data: cd1},
			{name: "CD2/05 - Beta.flac", data: cd2},
		})
		rawNZB := post(t, f, 64*1024, postFile{name: "album.zip", data: zipData})

		byFile, err := e.ListArchiveMembers(context.Background(), rawNZB)
		if err != nil {
			t.Fatalf("ListArchiveMembers(ZIP): %v", err)
		}
		if len(byFile[0]) != 2 {
			t.Fatalf("ZIP members = %+v, want 2", byFile[0])
		}

		// Ambiguous track {number: 5} without disc or title MUST fail.
		ambigNZB := annotateNonVideoNZB(t, rawNZB, nzb.NonVideoHint{
			MIME:  "audio/flac",
			Track: &nzb.TrackHint{Number: 5},
		})
		if _, err := e.Open(context.Background(), "zip-ambig", ambigNZB); !errors.Is(err, core.ErrNoUsableFile) {
			t.Fatalf("ambiguous track err = %v, want ErrNoUsableFile", err)
		}

		// Disc=2 disambiguates cleanly.
		disc2NZB := annotateNonVideoNZB(t, rawNZB, nzb.NonVideoHint{
			MIME:  "audio/flac",
			Track: &nzb.TrackHint{Disc: 2, Number: 5},
		})
		src := openSource(t, e, "zip-disc2", disc2NZB)
		if !bytes.Equal(readAll(t, src, 32*1024), cd2) {
			t.Fatal("disc 2 bytes mismatch")
		}
	})

	t.Run("7z archive members", func(t *testing.T) {
		f := newFakeFetcher()
		e := newTestEngine(t, f, nil, Options{})

		t1 := flacBytes(701, 40_000)
		t2 := flacBytes(702, 44_000)
		szData := at.Build7zCopy([]at.SevenZipFile{
			{Name: "01-opening.flac", Data: t1},
			{Name: "02-finale.flac", Data: t2},
		}, false)
		rawNZB := post(t, f, 64*1024, postFile{name: "album.7z", data: szData})

		byFile, err := e.ListArchiveMembers(context.Background(), rawNZB)
		if err != nil {
			t.Fatalf("ListArchiveMembers(7z): %v", err)
		}
		if len(byFile[0]) != 2 || byFile[0][1].Path != "02-finale.flac" || byFile[0][1].Size != int64(len(t2)) {
			t.Fatalf("7z members = %+v", byFile[0])
		}
	})
}

func TestMultiDiscSetsAndNestedNonVideoRAR(t *testing.T) {
	t.Run("separate per-disc RAR sets prioritize matching disc over larger set", func(t *testing.T) {
		f := newFakeFetcher()
		e := newTestEngine(t, f, nil, Options{})

		disc1Track := flacBytes(801, 50_000)
		disc2Track := flacBytes(802, 120_000) // intentionally larger than disc 1

		cd1Vols, err := at.BuildRAR([]at.RARFile{
			{Name: "01 - Bad.flac", Data: disc1Track},
		}, at.RAROptions{Version: 5})
		if err != nil {
			t.Fatalf("BuildRAR cd1: %v", err)
		}
		cd2Vols, err := at.BuildRAR([]at.RARFile{
			{Name: "01 - Don't Be Messin' 'Round.flac", Data: disc2Track},
		}, at.RAROptions{Version: 5})
		if err != nil {
			t.Fatalf("BuildRAR cd2: %v", err)
		}

		rawNZB := post(t, f, 64*1024,
			postFile{name: "Michael.Jackson.Bad.CD1.part01.rar", data: cd1Vols[0]},
			postFile{name: "Michael.Jackson.Bad.CD2.part01.rar", data: cd2Vols[0]},
		)

		// Even without a title, Disc=1 must pick CD1 over larger CD2.
		d1NZB := annotateNonVideoNZB(t, rawNZB, nzb.NonVideoHint{
			MIME:  "audio/flac",
			Track: &nzb.TrackHint{Disc: 1, Number: 1},
		})
		src1 := openSource(t, e, "multi-set-cd1", d1NZB)
		if !bytes.Equal(readAll(t, src1, 32*1024), disc1Track) {
			t.Fatal("disc 1 set bytes mismatch")
		}

		d2NZB := annotateNonVideoNZB(t, rawNZB, nzb.NonVideoHint{
			MIME:  "audio/flac",
			Track: &nzb.TrackHint{Disc: 2, Number: 1, Title: "Don't Be Messin' 'Round"},
		})
		src2 := openSource(t, e, "multi-set-cd2", d2NZB)
		if !bytes.Equal(readAll(t, src2, 32*1024), disc2Track) {
			t.Fatal("disc 2 set bytes mismatch")
		}
	})

	t.Run("nested CD1.rar and CD2.rar inside outer multi-volume RAR", func(t *testing.T) {
		f := newFakeFetcher()
		e := newTestEngine(t, f, nil, Options{})

		disc1Track := flacBytes(901, 90_000)
		disc2Track := flacBytes(902, 110_000)

		cd1Inner, err := at.BuildRAR([]at.RARFile{
			{Name: "01 - Bad.flac", Data: disc1Track},
		}, at.RAROptions{Version: 5})
		if err != nil {
			t.Fatalf("BuildRAR inner cd1: %v", err)
		}
		cd2Inner, err := at.BuildRAR([]at.RARFile{
			{Name: "01 - Don't Be Messin' 'Round.flac", Data: disc2Track},
		}, at.RAROptions{Version: 5})
		if err != nil {
			t.Fatalf("BuildRAR inner cd2: %v", err)
		}

		outerVols, err := at.BuildRAR([]at.RARFile{
			{Name: "CD1.rar", Data: cd1Inner[0]},
			{Name: "CD2.rar", Data: cd2Inner[0]},
		}, at.RAROptions{Version: 5, VolumeSize: 120_000})
		if err != nil {
			t.Fatalf("BuildRAR outer: %v", err)
		}
		var files []postFile
		for i, v := range outerVols {
			files = append(files, postFile{name: fmt.Sprintf("obfuscated.part%02d.rar", i+1), data: v})
		}
		rawNZB := post(t, f, 64*1024, files...)

		d1NZB := annotateNonVideoNZB(t, rawNZB, nzb.NonVideoHint{
			MIME:  "audio/flac",
			Track: &nzb.TrackHint{Disc: 1, Number: 1, Title: "Bad (2012 Remaster)"},
		})
		src1 := openSource(t, e, "nested-cd1", d1NZB)
		if !bytes.Equal(readAll(t, src1, 32*1024), disc1Track) {
			t.Fatal("nested disc 1 bytes mismatch")
		}

		d2NZB := annotateNonVideoNZB(t, rawNZB, nzb.NonVideoHint{
			MIME:  "audio/flac",
			Track: &nzb.TrackHint{Disc: 2, Number: 1, Title: "Don't Be Messin' 'Round"},
		})
		src2 := openSource(t, e, "nested-cd2", d2NZB)
		if !bytes.Equal(readAll(t, src2, 32*1024), disc2Track) {
			t.Fatal("nested disc 2 bytes mismatch")
		}
	})
}
