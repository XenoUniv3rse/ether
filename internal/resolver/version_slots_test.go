package resolver

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// A configured menu is a settings contract, not a projection of discovery.
// Verify cold/warm/expired caches, optional modes and admission of an explicit
// logical choice, including its unavailable-source fallback.
func TestConfiguredVersionSlots(t *testing.T) {
	for _, mode := range []string{"default", "higher", "resolution", "source", "both"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testCfg()
			cfg.Resolver.SameQualityAlternatives = 2
			cfg.Resolver.AlternativesPerResolution = mode == "resolution" || mode == "both"
			cfg.Resolver.AlternativesPerSource = mode == "source" || mode == "both"
			cfg.Resolver.OfferHigherVersions = mode != "default"
			idx := &fakeIndexer{releases: variantReleases()}
			svc := New(idx, fakeHealth{}, nil, cfg, Options{})
			defer svc.Close()
			req := core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}
			ctx := context.Background()
			cold, err := svc.VersionSlots(ctx, req, core.MaxVariants, false)
			if err != nil || len(cold) < 2 || len(cold) > core.MaxVariants {
				t.Fatalf("cold slots %d: %v", len(cold), err)
			}
			ids := func(vs []core.ReleaseVariant) []string {
				out := []string{}
				for _, v := range vs {
					out = append(out, v.ID)
				}
				return out
			}
			want := 3
			if mode == "higher" {
				want = 6
			} else if mode != "default" {
				want = core.MaxVariants
			}
			if len(cold) != want {
				t.Fatalf("configured slot count %d, want %d", len(cold), want)
			}
			before := ids(cold)
			if idx.searches.Load() != 0 {
				t.Fatal("browse searched")
			}
			primeSearchCache(t, svc, req.Client)
			searches := idx.searches.Load()
			warm, err := svc.VersionSlots(ctx, req, core.MaxVariants, true)
			if err != nil || !reflect.DeepEqual(ids(warm), before) {
				t.Fatalf("cache changed slots: %v", err)
			}
			if idx.searches.Load() != searches {
				t.Fatal("labels searched")
			}
			found := false
			for _, v := range warm {
				if strings.Contains(v.Label, "GRP") {
					found = true
				}
			}
			if !found {
				t.Fatal("known releases lost their labels")
			}
			// Local responses keep the configured identities immediately and
			// acquire labels only through the bounded background snapshot.
			local := core.WithLocalResponse(ctx)
			immediate, e := svc.VersionSlots(local, req, core.MaxVariants, true)
			if e != nil || !reflect.DeepEqual(ids(immediate), before) {
				t.Fatalf("local slots: %v", e)
			}
			svc.bgWG.Wait()
			labelled, e := svc.VersionSlots(local, req, core.MaxVariants, true)
			if e != nil || !reflect.DeepEqual(labelled, warm) {
				t.Fatalf("local labels differ: %v", e)
			}
			for _, v := range warm[1:] {
				if v.Release.ID == v.ID {
					continue
				} // an unavailable configured reservation
				selected := req
				selected.Prefer = v.ID
				result, e := svc.Resolve(ctx, selected)
				if e != nil || !result.MatchesVersion(v.ID) {
					t.Fatalf("slot admission %s: %v", v.Label, e)
				}
				break
			}
			for _, v := range warm[1:] {
				if v.Release.ID != v.ID {
					continue
				}
				selected := req
				selected.Prefer = v.ID
				result, e := svc.Resolve(ctx, selected)
				if e != nil || result == nil || result.VersionSelector != "" {
					t.Fatalf("unavailable configured slot: %v", e)
				}
				break
			}
			// This configured but absent source remains visible and safely falls
			// back to normal ranking. It must not gain a false selector receipt.
			missing := versionSlot{Kind: "tier", Height: 480, Source: "remux"}.id()
			selected := req
			selected.Prefer = missing
			result, e := svc.Resolve(ctx, selected)
			if e != nil || result == nil || result.VersionSelector != "" {
				t.Fatalf("missing fallback: %v", e)
			}
		})
	}
	t.Run("higher slot and device ceiling", func(t *testing.T) {
		cfg := testCfg()
		cfg.Quality.Rules = nil
		cfg.Resolver.AlternativesPerResolution = true
		cfg.Resolver.OfferHigherVersions = true
		idx := &fakeIndexer{releases: []core.Release{rel("high", "The.Signal.1999.2160p.WEB-DL.HEVC.DDP5.1-GRP", 20000), rel("hd", "The.Signal.1999.1080p.WEB-DL.H.264.DDP5.1-GRP", 8000)}}
		svc := New(idx, fakeHealth{}, nil, cfg, Options{})
		defer svc.Close()
		req := core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}
		slots, err := svc.VersionSlots(context.Background(), req, core.MaxVariants, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range slots {
			slot, _ := parseVersionSlot(v.ID)
			if slot.Height == 2160 {
				req.Prefer = v.ID
				break
			}
		}
		if req.Prefer == "" {
			t.Fatal("owner-enabled higher tier missing")
		}
		res, err := svc.Resolve(context.Background(), req)
		if err != nil || !res.MatchesVersion(req.Prefer) || res.Release.ID != "high" {
			t.Fatalf("higher tier: %+v %v", res, err)
		}
		req.Client.MaxHeight = 1080
		res, err = svc.Resolve(context.Background(), req)
		if err != nil || res.Release.ID != "hd" || res.VersionSelector != "" {
			t.Fatalf("device ceiling: %+v %v", res, err)
		}
	})

}

// Menu labels, reservation and admission must share the same Auto identity as
// discovery changes, whether Auto is fresh, remembered, or owner-pinned.
func TestVersionSlotSourceAgreement(t *testing.T) {
	for _, mode := range []string{"fresh", "remembered", "pinned"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testCfg()
			cfg.Quality.Rules = nil
			cfg.Quality.Default, cfg.Quality.DefaultAway = "4K", "4K"
			cfg.Resolver.AlternativesPerResolution = true
			cfg.Resolver.OfferHigherVersions = true
			releases := []core.Release{
				rel("high", "The.Signal.1999.2160p.WEB-DL.HEVC.DDP5.1-GRP", 20000),
				rel("hd", "The.Signal.1999.1080p.WEB-DL.H.264.DDP5.1-GRP", 8000),
			}
			svc := New(&fakeIndexer{releases: releases}, fakeHealth{}, nil, cfg, Options{})
			defer svc.Close()
			ctx := context.Background()
			req := core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}
			primeSearchCache(t, svc, req.Client)
			if mode == "remembered" {
				req.PlayedRelease = "hd"
			}
			if mode == "pinned" {
				pinned, _ := svc.LookupRelease("hd")
				if _, err := svc.Pin(ctx, req.Title.Key, "", pinned, ""); err != nil {
					t.Fatal(err)
				}
				req.PlayedRelease = "high" // a pin wins over previous playback
			}
			versions, err := svc.VersionSlots(ctx, req, core.MaxVariants, true)
			if err != nil {
				t.Fatal(err)
			}
			wantAuto := "high"
			if mode != "fresh" {
				wantAuto = "hd"
			}
			if versions[0].Release.ID != wantAuto {
				t.Fatalf("Auto = %s, want %s", versions[0].Release.ID, wantAuto)
			}
			for _, version := range versions {
				if version.Release.ID == version.ID {
					continue
				}
				pick := req
				pick.Prefer, pick.PreferredOnly = version.ID, true
				got, err := svc.Resolve(ctx, pick)
				if err != nil || got == nil || got.Release.ID != version.Release.ID || !got.MatchesVersion(version.ID) {
					t.Fatalf("menu %s (%s) admitted %+v: %v", version.Label, version.Release.ID, got, err)
				}
			}
			if mode != "fresh" && versions[1].Release.ID != "high" {
				t.Fatalf("4K source consumed by Auto: %+v", versions[1])
			}
		})
	}
}

func TestColdVersionSlotsPlaceholderHasNoPresumedAudioLanguage(t *testing.T) {
	cfg := testCfg()
	svc := New(&fakeIndexer{}, fakeHealth{}, nil, cfg, Options{})
	defer svc.Close()
	title := matrixTitle()
	title.Language = "en"
	slots, err := svc.VersionSlots(context.Background(), core.ResolveRequest{Title: title, Client: appleTVCaps()}, core.MaxVariants, false)
	if err != nil || len(slots) == 0 {
		t.Fatalf("cold VersionSlots = %d, %v", len(slots), err)
	}
	for i, slot := range slots {
		if !slot.Info.Estimated {
			t.Fatalf("slot %d not estimated: %+v", i, slot.Info)
		}
		var audio *core.StreamInfo
		for j := range slot.Info.Streams {
			if slot.Info.Streams[j].Type == "audio" {
				audio = &slot.Info.Streams[j]
				break
			}
		}
		if audio == nil {
			t.Fatalf("slot %d missing placeholder audio stream: %+v", i, slot.Info.Streams)
		}
		if audio.Language != "" {
			t.Fatalf("slot %d cold placeholder claimed audio language %q, want empty", i, audio.Language)
		}
	}
}

func TestVersionSlotsExplicitNonAutoPlayStaysInOwnSlot(t *testing.T) {
	ctx := context.Background()
	cfg := testCfg()
	cfg.Resolver.SameQualityAlternatives = 0
	cfg.Resolver.OfferHigherVersions = true
	cfg.Resolver.AlternativesPerResolution = true
	cfg.Quality.Default = "hd"
	cfg.Quality.Profiles = []core.QualityProfile{{Name: "hd", MaxHeight: 1080}}

	rels := []core.Release{
		rel("rel-1080", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-FLUX", 4096),
		rel("rel-4k-a", "The.Signal.1999.2160p.WEB-DL.DV.DDP5.1.HEVC-TOP", 9216),
		rel("rel-4k-b", "The.Signal.1999.2160p.WEB-DL.HDR10.DDP5.1.HEVC-SECOND", 8192),
	}
	idx := &fakeIndexer{releases: rels}
	db := store.OpenTest(t)
	svc := New(idx, fakeHealth{}, db, cfg, Options{})
	defer svc.Close()

	user := &core.User{ID: 7, Username: "viewer"}
	req := core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps(), User: user}
	primeSearchCache(t, svc, req.Client)

	slot4K := versionSlot{Kind: "tier", Height: 2160}.id()

	// Explicit non-Auto play of rel-4k-b (<5%, no remembered choice):
	// Auto stays on rel-1080, 4K stays pinned to rel-4k-b (not refilled with rel-4k-a).
	playedReq := req
	playedReq.PlayedRelease = "rel-4k-b"
	playedReq.PlayedSelector = slot4K
	playedReq.PlayedReleaseRecord = &rels[2]
	slots, err := svc.VersionSlots(ctx, playedReq, core.MaxVariants, true)
	if err != nil || len(slots) < 2 {
		t.Fatalf("VersionSlots = %d, %v", len(slots), err)
	}
	if slots[0].Release.ID != "rel-1080" {
		t.Fatalf("Auto was hijacked by explicit 4K pick: got %s, want rel-1080", slots[0].Release.ID)
	}
	var got4K string
	for _, s := range slots[1:] {
		if s.ID == slot4K {
			got4K = s.Release.ID
		}
	}
	if got4K != "rel-4k-b" {
		t.Fatalf("4K slot refilled with %q, want played release rel-4k-b", got4K)
	}
	pick := playedReq
	pick.Prefer, pick.PreferredOnly = slot4K, true
	if res, err := svc.Resolve(ctx, pick); err != nil || res == nil || res.Release.ID != "rel-4k-b" {
		t.Fatalf("Resolve(slot4K) = %+v, %v; want rel-4k-b", res, err)
	}

	// Cold search cache with PlayedReleaseRecord still keeps rel-4k-b in 4K slot.
	svcCold := New(&fakeIndexer{}, fakeHealth{}, db, cfg, Options{})
	defer svcCold.Close()
	coldSlots, err := svcCold.VersionSlots(ctx, playedReq, core.MaxVariants, true)
	if err != nil {
		t.Fatalf("cold VersionSlots: %v", err)
	}
	var cold4K string
	for _, s := range coldSlots {
		if s.ID == slot4K {
			cold4K = s.Release.ID
		}
	}
	if cold4K != "rel-4k-b" {
		t.Fatalf("cold 4K slot = %q, want rel-4k-b", cold4K)
	}

	// When remembered choice (≥5% watch) promotes rel-4k-b into Auto, 4K slot
	// must NOT be refilled with unprobed rel-4k-a (dedupe folds the duplicate).
	if err := svc.RecordViewerChoice(ctx, user.ID, req.Title, slot4K, rels[2], nil); err != nil {
		t.Fatal(err)
	}
	remSlots, err := svc.VersionSlots(ctx, playedReq, core.MaxVariants, true)
	if err != nil || len(remSlots) == 0 {
		t.Fatalf("remembered VersionSlots = %d, %v", len(remSlots), err)
	}
	if !remSlots[0].Remembered || remSlots[0].Release.ID != "rel-4k-b" {
		t.Fatalf("Auto after remembered choice = %+v, want Remembered rel-4k-b", remSlots[0])
	}
	for _, s := range remSlots[1:] {
		if s.ID == slot4K || s.Release.ID == "rel-4k-a" {
			t.Fatalf("remembered 4K slot was refilled instead of folded: %+v", s)
		}
	}
}

func TestColdDetailSlotsReadProbeFromCachedSearch(t *testing.T) {
	ctx := context.Background()
	svc := New(&fakeIndexer{releases: variantReleases()}, fakeHealth{}, nil, testCfg(), Options{})
	defer svc.Close()
	title := matrixTitle()
	req := core.ResolveRequest{Title: title, Client: appleTVCaps()}
	primeSearchCache(t, svc, req.Client)
	svc.SetProbedMedia(func(core.Release) (core.MediaInfo, bool) {
		return core.MediaInfo{Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264"},
			{Index: 4, Type: "subtitle", Codec: "ass"},
		}}, true
	})
	cached, err := svc.VersionSlots(core.WithColdDetail(core.WithLocalResponse(ctx), title.Key), req, core.MaxVariants, true)
	if err != nil || len(cached) == 0 || cached[0].Info.Estimated || len(cached[0].Info.Streams) != 2 {
		t.Fatalf("first detail did not use the cached probe: %v", err)
	}
}

func TestDetailVariantCacheIgnoresVolatileTitleFieldsAndRefreshKeepsConcrete(t *testing.T) {
	ctx := context.Background()
	cfg := testCfg()
	cfg.Resolver.SameQualityAlternatives = 1
	cfg.Resolver.OfferHigherVersions = true
	idx := &fakeIndexer{releases: variantReleases()}
	svc := New(idx, fakeHealth{}, nil, cfg, Options{})
	defer svc.Close()

	baseTitle := matrixTitle()
	req := core.ResolveRequest{Title: baseTitle, Client: appleTVCaps()}
	primeSearchCache(t, svc, req.Client)

	warm, err := svc.VersionSlots(ctx, req, core.MaxVariants, true)
	if err != nil || len(warm) < 2 || warm[0].Release.ID == warm[0].ID {
		t.Fatalf("warm VersionSlots = %+v, %v", warm, err)
	}

	// Non-ranking presentation/enrichment fields on Title must not bust the detail-variant cache.
	mutated := *baseTitle
	mutated.Summary = "Updated synopsis after enrichment"
	mutated.UpdatedAt = time.Now().Add(time.Minute)
	mutated.Rating = 9.1
	mutated.Popularity = 1234.5
	req2 := core.ResolveRequest{Title: &mutated, Client: appleTVCaps()}
	localCtx := core.WithLocalResponse(ctx)

	got, err := svc.VersionSlots(localCtx, req2, core.MaxVariants, true)
	if err != nil || len(got) != len(warm) || got[0].Release.ID != warm[0].Release.ID {
		t.Fatalf("volatile Title fields missed concrete cache: got %+v, want %s", got, warm[0].Release.ID)
	}

	// RefreshDetailVariants marks stale without wiping concrete releases back
	// to cold placeholders on the immediately following LocalResponse read.
	svc.RefreshDetailVariants()
	stale, err := svc.VersionSlots(localCtx, req2, core.MaxVariants, true)
	if err != nil || len(stale) != len(warm) || stale[0].Release.ID != warm[0].Release.ID {
		t.Fatalf("RefreshDetailVariants dropped back to placeholders: got %+v, want %s", stale, warm[0].Release.ID)
	}
}

func TestPickedTierSlotFailsOverWithinTierAndStepsDownNeverUp(t *testing.T) {
	ctx := context.Background()
	slot1080 := versionSlot{Kind: "tier", Height: 1080}.id()

	newCfg := func() config.Config {
		cfg := testCfg()
		cfg.Quality.Rules = nil
		cfg.Quality.Default, cfg.Quality.DefaultAway = "4K", "4K"
		cfg.Resolver.AlternativesPerResolution = true
		cfg.Resolver.SameQualityAlternatives = 1
		return cfg
	}

	rels := []core.Release{
		rel("uhd1", "The.Signal.1999.2160p.WEB-DL.DV.DDP5.1.HEVC-UHD", 20000),
		rel("hd1", "The.Signal.1999.1080p.BluRay.x265.DTS-HD.MA.5.1-DEAD1", 12000),
		rel("hd2", "The.Signal.1999.1080p.BluRay.x264.DDP5.1-DEAD2", 10000),
		rel("hd3", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-GOOD1", 8000),
		rel("hd4", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-GOOD2", 7500),
		rel("sd1", "The.Signal.1999.720p.BluRay.x264-SD1", 4000),
	}

	t.Run("same-tier failover skips dead copies and keeps slot receipt", func(t *testing.T) {
		svc := New(&fakeIndexer{releases: rels}, fakeHealth{unhealthy: map[string]bool{"hd1": true, "hd2": true}}, nil, newCfg(), Options{})
		defer svc.Close()

		req := core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps(), Prefer: slot1080}
		res, err := svc.Resolve(ctx, req)
		if err != nil || res == nil {
			t.Fatalf("Resolve(slot1080) = %+v, %v", res, err)
		}
		if res.Release.ID != "hd3" || !res.MatchesVersion(slot1080) || res.QualityFallback != "" {
			t.Fatalf("got rel=%q selector=%q fallback=%q, want hd3 fulfilling %q", res.Release.ID, res.VersionSelector, res.QualityFallback, slot1080)
		}
		if !slices.Contains(res.FailedReleases, "hd1") || !slices.Contains(res.FailedReleases, "hd2") {
			t.Fatalf("FailedReleases = %v, want hd1 and hd2", res.FailedReleases)
		}
		for _, alt := range res.Alternates {
			if alt.Info.Height != 1080 {
				t.Fatalf("alternate %s has height %d, want only 1080p same-slot alternates", alt.ID, alt.Info.Height)
			}
		}

		// Exact concrete release pick still fails closed on a dead release.
		exactReq := core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps(), Prefer: "hd1", PreferredOnly: true}
		if got, err := svc.Resolve(ctx, exactReq); !errors.Is(err, core.ErrNotAvailable) || got != nil {
			t.Fatalf("exact dead pick = %+v, %v; want ErrNotAvailable", got, err)
		}
	})

	t.Run("exhausted tier steps down to nearest lower tier and surfaces fallback", func(t *testing.T) {
		svc := New(&fakeIndexer{releases: rels}, fakeHealth{unhealthy: map[string]bool{
			"hd1": true, "hd2": true, "hd3": true, "hd4": true,
		}}, nil, newCfg(), Options{})
		defer svc.Close()

		req := core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps(), Prefer: slot1080}
		res, err := svc.Resolve(ctx, req)
		if err != nil || res == nil {
			t.Fatalf("Resolve(slot1080 step-down) = %+v, %v", res, err)
		}
		if res.Release.ID != "sd1" {
			t.Fatalf("step-down release = %q, want sd1 (720p), never 4K uhd1", res.Release.ID)
		}
		if res.VersionSelector != "" {
			t.Fatalf("step-down VersionSelector = %q, want empty", res.VersionSelector)
		}
		if res.QualityFallback == "" {
			t.Fatal("step-down must surface QualityFallback")
		}
		if trace, ok := svc.LatestDecision(matrixTitle().Key); !ok || trace.QualityFallback == "" {
			t.Fatalf("LatestDecision QualityFallback missing: ok=%v trace=%+v", ok, trace)
		}
	})

	t.Run("exhausted tier with no lower tier never climbs up to 4K", func(t *testing.T) {
		noLower := []core.Release{
			rel("uhd1", "The.Signal.1999.2160p.WEB-DL.DV.DDP5.1.HEVC-UHD", 20000),
			rel("hd1", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-DEAD1", 8000),
		}
		svc := New(&fakeIndexer{releases: noLower}, fakeHealth{unhealthy: map[string]bool{"hd1": true}}, nil, newCfg(), Options{})
		defer svc.Close()

		req := core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps(), Prefer: slot1080}
		res, err := svc.Resolve(ctx, req)
		if !errors.Is(err, core.ErrNotAvailable) || res != nil {
			t.Fatalf("exhausted tier with only 4K left = %+v, %v; want ErrNotAvailable (never 4K)", res, err)
		}
	})
}
