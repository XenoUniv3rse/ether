package playback

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestAudioProbedAcceptable(t *testing.T) {
	m := &Manager{}
	s := &session{
		title: &core.Title{Language: "en"},
		user:  core.User{AudioLanguages: []string{"en"}},
	}
	profile := core.QualityProfile{Language: "en"}

	if m.probedAudioAcceptable(s, profile, core.MediaInfo{
		Streams: []core.StreamInfo{{Type: "audio", Language: "fr"}},
	}) {
		t.Fatal("expected false when orig matches wanted but probed audio is only fr")
	}

	if !m.probedAudioAcceptable(s, profile, core.MediaInfo{
		Streams: []core.StreamInfo{
			{Type: "audio", Language: "fr"},
			{Type: "audio", Language: "en"},
		},
	}) {
		t.Fatal("expected true when probed audio includes en")
	}

	if !m.probedAudioAcceptable(s, profile, core.MediaInfo{
		Streams: []core.StreamInfo{{Type: "audio", Language: "und"}},
	}) {
		t.Fatal("expected true when probed audio is untagged und")
	}
}

// Regression for #674: a viewer with preferOriginal=true must accept the
// title's original language (including ISO 639-2/639-1 pairs like baq/eu)
// instead of falling back to Catalog.Language and rejecting original releases.
func TestAudioProbedAcceptablePreferOriginal(t *testing.T) {
	cfg := config.Config{Catalog: config.Catalog{Language: "en-US"}}
	m := &Manager{ctx: context.Background(), cfg: cfg}
	m.deps.ResolveViewerLanguages = func(_ context.Context, user *core.User) *core.User {
		user.PreferOriginal = true
		if user.ID == 2 {
			user.AudioLanguages = []string{"en"}
		}
		return user
	}

	// Empty AudioLanguages + preferOriginal=true on a Basque title (ISO 639-1 "eu",
	// MKV audio tagged ISO 639-2/B "baq"): accept original, reject English-only dub.
	basque := &session{
		title: &core.Title{Language: "eu"},
		user:  core.User{ID: 1},
	}
	if !m.probedAudioAcceptable(basque, core.QualityProfile{}, core.MediaInfo{
		Streams: []core.StreamInfo{{Type: "audio", Language: "baq"}},
	}) {
		t.Fatal("expected preferOriginal viewer to accept Basque (baq) original audio")
	}
	if m.probedAudioAcceptable(basque, core.QualityProfile{}, core.MediaInfo{
		Streams: []core.StreamInfo{{Type: "audio", Language: "eng"}},
	}) {
		t.Fatal("expected preferOriginal viewer with empty AudioLanguages to reject English-only dub instead of falling back to Catalog.Language")
	}

	// preferOriginal=true with fallback AudioLanguages=["en"] on a Japanese title:
	// accept Japanese original and leave dubs for the last-resort open path.
	anime := &session{
		title: &core.Title{Language: "ja"},
		user:  core.User{ID: 2},
	}
	if !m.probedAudioAcceptable(anime, core.QualityProfile{}, core.MediaInfo{
		Streams: []core.StreamInfo{{Type: "audio", Language: "jpn"}},
	}) {
		t.Fatal("expected preferOriginal viewer with fallback en to accept Japanese original audio")
	}
	if m.probedAudioAcceptable(anime, core.QualityProfile{}, core.MediaInfo{
		Streams: []core.StreamInfo{{Type: "audio", Language: "deu"}},
	}) {
		t.Fatal("expected preferOriginal viewer to reject unrelated German dub")
	}

	// Unknown title language + preferOriginal=true must not fall back to Catalog.Language.
	unknown := &session{
		title: &core.Title{},
		user:  core.User{ID: 1},
	}
	if !m.probedAudioAcceptable(unknown, core.QualityProfile{}, core.MediaInfo{
		Streams: []core.StreamInfo{{Type: "audio", Language: "jpn"}},
	}) {
		t.Fatal("expected preferOriginal viewer with unknown title language not to fall back to Catalog.Language")
	}
}

// Regression for #674: once a release is rejected for probed audio during a
// start, content-equivalent reposts (same release name or payload hash under
// distinct release IDs) must not be reopened/reprobed in a loop within the
// same start.
func TestAudioRejectedReleaseNotRetriedViaReposts(t *testing.T) {
	rels := []*fakeRelease{
		mkv("idx1-bad", 20<<20, time.Minute),
		mkv("idx2-bad-repost", 20<<20, time.Minute),
		mkv("vault-bad-copy", 20<<20, time.Minute),
		mkv("idx1-good", 20<<20, time.Minute),
	}
	probedByID := map[string]int{}
	h := newHarness(t, rels, []string{"idx1-bad", "idx2-bad-repost", "vault-bad-copy", "idx1-good"},
		withNativeProbe(func(_ context.Context, src core.MediaSource) (*core.MediaInfo, error) {
			probedByID[src.ReleaseID()]++
			lang := "deu"
			if src.ReleaseID() == "idx1-good" {
				lang = "eng"
			}
			return &core.MediaInfo{
				Container: "mkv",
				Duration:  time.Minute,
				Size:      src.Size(),
				Streams: []core.StreamInfo{
					{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080, Default: true},
					{Index: 1, Type: "audio", Codec: "eac3", Channels: 6, Language: lang, Default: true},
				},
			}, nil
		}, nil),
	)
	h.catalog.titles[epKey].Language = "en"
	// idx1-bad, idx2-bad-repost (same release name on a second indexer), and
	// vault-bad-copy (same release name/content from vault) are all the same release.
	const dupName = "Show.S01E01.1080p.WEB-DL.H264-DUPGRP"
	h.resolver.candidates[0].Name = dupName
	h.resolver.candidates[1].Name = dupName
	h.resolver.candidates[2].Name = "Show S01E01 1080p WEB-DL H264-DUPGRP"
	h.resolver.candidates[3].Name = "Show.S01E01.1080p.WEB-DL.H264-GOODGRP"

	user := &core.User{ID: 1, Username: "owner", IsOwner: true, AudioLanguages: []string{"en"}}
	ps, err := h.m.Prepare(context.Background(), user, epKey, defaultCaps("ios-674"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if ps.Resolution == nil || ps.Resolution.Release.ID != "idx1-good" {
		t.Fatalf("selected release = %+v, want idx1-good", ps.Resolution)
	}
	if probedByID["idx1-bad"] != 1 {
		t.Fatalf("idx1-bad probe count = %d, want 1", probedByID["idx1-bad"])
	}
	if probedByID["idx2-bad-repost"] != 0 || probedByID["vault-bad-copy"] != 0 {
		t.Fatalf("repost/vault copies were retried after audio rejection: %+v", probedByID)
	}
	if probedByID["idx1-good"] != 1 {
		t.Fatalf("idx1-good probe count = %d, want 1", probedByID["idx1-good"])
	}
}
