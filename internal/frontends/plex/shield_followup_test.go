package plex

import (
	"context"
	"fmt"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/trackprefs"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// The support capture preserves methods/routes and client identity, but
// redacts queries and carries no raw profile headers. Query/profile fields
// below are reconstructed from the existing Android TV contract fixture.
type shieldMetadataPlayback struct {
	core.Playback
	facts  core.MediaInfo
	ready  bool
	err    error
	probes int
}

func (p *shieldMetadataPlayback) ProbeMetadata(context.Context, *core.User, core.RatingKey, core.ClientCaps) error {
	p.probes++
	p.ready = p.err == nil
	return p.err
}
func (p *shieldMetadataPlayback) Prepare(ctx context.Context, u *core.User, k core.RatingKey, caps core.ClientCaps) (*core.PlaySession, error) {
	ps, err := p.Playback.Prepare(ctx, u, k, caps)
	if ps != nil {
		ps.Resolution = &core.Resolution{Release: core.Release{ID: "rel-auto"}}
	}
	return ps, err
}
func (p *shieldMetadataPlayback) WarmProbeInfo(*core.User, core.ClientCaps, core.RatingKey) (core.MediaInfo, bool) {
	return p.facts, p.ready
}
func (p *shieldMetadataPlayback) Estimate(context.Context, *core.Title, core.ClientCaps) core.MediaInfo {
	return core.MediaInfo{Estimated: true, Container: "mkv", Streams: p.facts.Streams[:2]}
}

func TestShieldTroyCaptureFollowup(t *testing.T) {
	for _, product := range []string{"Plex for Android (TV)", "Plex for Android (Mobile)"} {
		t.Run(product, func(t *testing.T) {
			for _, warm := range []bool{false, true} {
				for _, mode := range []string{"hevc-copy-audio-transcode", "tonemap"} {
					t.Run(fmt.Sprintf("warm=%v/%s", warm, mode), func(t *testing.T) {
						f := newFixture()
						defer f.srv.Close()
						troy := fightClub
						troy.Title = "Troy"
						troy.Year = 2004
						f.catalog.titles[troy.Key] = troy
						f.srv.setTranscoder(shieldContractTranscoder{f.trans})
						prefs := trackprefs.New(store.OpenTest(t))
						f.srv.setTrackPreferences(prefs)
						rules := trackprefs.Default()
						rules.SubtitleLanguages = []string{"en"}
						rules.SubtitleMode = "always"
						if err := prefs.PutRules(context.Background(), ownerUser.ID, rules); err != nil {
							t.Fatal(err)
						}
						facts := core.MediaInfo{Container: "mkv", Duration: 30 * time.Second, Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10", BitDepth: 10, Width: 3840, Height: 2160, ColorTransfer: "smpte2084", ColorPrimaries: "bt2020", DolbyVision: true, DolbyVisionProfile: 7, DolbyVisionCompatibilityID: 6},
							{Index: 1, Type: "audio", Codec: "dca", Profile: "DTS-HD MA", Channels: 6, Default: true},
							{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en", Default: true},
							{Index: 3, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "en"},
						}}
						f.playback.info = facts
						f.srv.setVariants(&fakeVariantResolver{byKey: testVariantLists()})
						pb := &shieldMetadataPlayback{Playback: f.playback, facts: facts, ready: warm}
						f.srv.setPlayback(pb)
						device := "SHIELD Android TV"
						if product == "Plex for Android (Mobile)" {
							device = "Pixel 8"
						}
						h := map[string]string{"X-Plex-Product": product, "X-Plex-Platform": "Android", "X-Plex-Device": device, "X-Plex-Model": "darcy", "X-Plex-Version": "2026.20.0"}
						path := "/library/metadata/" + fightClub.Key.String()
						read := func(target string) string {
							t.Helper()
							r := f.request(t, "GET", target, h)
							if r.Code != 200 {
								t.Fatalf("%s: %d %s", target, r.Code, r.Body.String())
							}
							return r.Body.String()
						}
						first := subtitleTags(read(path))
						if (warm && (len(first) != 2 || streamAttr(selectedTag(first), "codec") != "srt")) ||
							(!warm && len(first) != 0) || pb.probes != 0 || f.playback.prepared != 0 {
							t.Fatalf("first metadata: tracks=%v probes=%d prepares=%d", first, pb.probes, f.playback.prepared)
						}
						r := f.request(t, "POST", "/playQueues?type=video&uri="+url.QueryEscape(path), h)
						if r.Code != 200 {
							t.Fatalf("queue: %d %s", r.Code, r.Body.String())
						}
						if !warm {
							first = subtitleTags(r.Body.String())
							if len(first) != 2 {
								t.Fatalf("cold play queue omitted probed tracks: %v", first)
							}
						}
						ids := []string{streamAttr(first[0], "id"), streamAttr(first[1], "id")}
						check := func(body, selected string) {
							t.Helper()
							tags := subtitleTags(body)
							if len(tags) != 2 || streamAttr(tags[0], "id") != ids[0] || streamAttr(tags[1], "id") != ids[1] || streamAttr(selectedTag(tags), "id") != selected {
								t.Fatalf("track list changed: %v (want %v selected %s)", tags, ids, selected)
							}
						}
						check(r.Body.String(), ids[0])
						check(read(path), ids[0])
						profile := "add-transcode-target(type=videoProfile&context=all&protocol=hls&container=mkv&videoCodec=h264,hevc&audioCodec=aac&subtitleCodec=ass,ssa,pgs,srt)"
						if mode == "tonemap" {
							profile += "+add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.bitDepth&value=8)"
						}
						// The fixture advertises multiple versions; select its Auto entry.
						q := url.Values{"mediaIndex": {"0"}, "path": {path}, "protocol": {"hls"}, "directPlay": {"1"}, "directStream": {"1"}, "hasMDE": {"1"}, "subtitles": {"none"}, "advancedSubtitles": {"text"}, "session": {"troy-" + mode}, "X-Plex-Client-Profile-Name": {"Generic"}, "X-Plex-Client-Profile-Extra": {profile}}
						for i, id := range ids {
							if i > 0 {
								r = f.request(t, "PUT", "/library/parts/"+fightClub.Key.String()+"?subtitleStreamID="+id, h)
								if r.Code != 200 {
									t.Fatalf("PUT: %d %s", r.Code, r.Body.String())
								}
								check(read(path), id)
							}
							decision := read("/video/:/transcode/universal/decision?" + q.Encode())
							check(decision, id)
							read("/video/:/transcode/universal/start.m3u8?" + q.Encode())
							d := f.trans.sessions[q.Get("session")].req.Decision
							if d.Container != transcode.ContainerMatroska || d.SubtitleStream != 2+i {
								t.Fatalf("encoder lost subtitle: %+v", d)
							}
							if i == 0 {
								row := selectedTag(subtitleTags(decision))
								if d.AudioCopy || d.BurnSubtitles || streamAttr(row, "decision") != "copy" || streamAttr(row, "location") != "segments-av" {
									t.Fatalf("SRT conversion: %+v %s", d, row)
								}
								if mode == "tonemap" {
									if d.VideoCopy || !d.ToneMap {
										t.Fatalf("missing tone map: %+v", d)
									}
								} else if product != "Plex for Android (Mobile)" && !d.VideoCopy {
									t.Fatalf("HEVC not copied: %+v", d)
								}
							} else if !d.BurnSubtitles || d.VideoCopy {
								t.Fatalf("PGS not burned: %+v", d)
							}
							check(read(path), id)
						}
					})
				}
			}
		})
	}
}

func TestShieldMetadataProbeFailureKeepsNormalResponse(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setPlayback(&shieldMetadataPlayback{Playback: f.playback, facts: sampleInfo, err: core.ErrPrepareSlow})
	r := f.get(t, "/library/metadata/"+fightClub.Key.String(), "X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android")
	if r.Code != 200 || r.Header().Get("Retry-After") != "" {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
}

func TestAndroidMetadataProbeLeavesOtherClientsUnchanged(t *testing.T) {
	for _, product := range []string{"Plex Web", "Plex for Apple TV", "Plezy"} {
		t.Run(product, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			p := &shieldMetadataPlayback{Playback: f.playback, facts: sampleInfo, err: core.ErrPrepareSlow}
			f.srv.setPlayback(p)
			r := f.get(t, "/library/metadata/"+fightClub.Key.String(), "X-Plex-Product", product)
			if r.Code != 200 || p.probes != 0 {
				t.Fatalf("other client changed: status=%d metadata probes=%d", r.Code, p.probes)
			}
		})
	}
}

func TestShieldDisabledDetailUsesPersistedProbe(t *testing.T) {
	x := newEtheLab(t, true, "offline-0", 3)
	x.cfg.Cache.DisableSpeculation = true
	x.mgr.SetConfig(x.cfg)
	r := x.f.get(t, "/library/metadata/"+x.title.Key.String(), "X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android")
	if r.Code != 200 || len(subtitleTags(r.Body.String())) < 1 {
		t.Fatalf("persisted tracks missing: %d %s", r.Code, r.Body.String())
	}
	if x.idx.searches.Load() != 0 || x.idx.fetches.Load() != 0 {
		t.Fatal("disabled detail fetched fresh source")
	}
}

type shieldLocalIndexer struct {
	release  core.Release
	searches atomic.Int32
	fetches  atomic.Int32
}

func (*shieldLocalIndexer) Name() string { return "offline-shield-local" }
func (i *shieldLocalIndexer) Search(context.Context, core.SearchRequest) ([]core.Release, error) {
	i.searches.Add(1)
	return []core.Release{i.release}, nil
}
func (i *shieldLocalIndexer) FetchNZB(_ context.Context, r core.Release) ([]byte, error) {
	i.fetches.Add(1)
	return []byte("nzb-" + r.ID), nil
}

type shieldLocalEngine struct {
	size  int64
	opens atomic.Int32
}

func (e *shieldLocalEngine) Open(_ context.Context, id string, _ []byte) (core.MediaSource, error) {
	e.opens.Add(1)
	return &playedSource{id: id, size: e.size}, nil
}
func (*shieldLocalEngine) Prefetch(context.Context, string, []byte, int64, int64) error { return nil }

func TestAndroidColdLocalDetailSchedulesBackgroundPrewarm(t *testing.T) {
	run := func(t *testing.T, disableSpeculation bool) {
		t.Helper()
		dir := t.TempDir()
		cfg := config.Defaults(dir)
		cfg.Quality.Default = "lab1080"
		cfg.Quality.DefaultAway = ""
		cfg.Quality.Rules = nil
		cfg.Quality.Profiles = []core.QualityProfile{{Name: "lab1080", MaxHeight: 1080}}
		cfg.Cache.DisableSpeculation = disableSpeculation

		relName := "Fjord.Harbor.1999.1080p.WEB-DL.DDP5.1.H.264-LAB"
		const relSize = int64(4) << 30
		idx := &shieldLocalIndexer{
			release: core.Release{
				ID: "shield-local-rel", GUID: "shield-local-rel", Indexer: "offline-shield-local",
				Name: relName, Size: relSize, PubDate: fixedNow.Add(-24 * time.Hour),
				NZBURL: "offline", Info: releaseparse.Parse(relName),
			},
		}
		engine := &shieldLocalEngine{size: relSize}

		f := newFixture(func(d *testServerDeps) {
			setDepsConfig(d, func() config.Config { return cfg })
			d.DisableEnrichment = true
			d.HomeCache = &testHomeCache{NoProviderWait: func(ctx context.Context) context.Context {
				return core.WithLocalResponse(catalog.WithReadWaitBudget(ctx, 0))
			}}
		})
		defer f.srv.Close()

		rs := resolver.New(idx, nil, nil, cfg, resolver.Options{Now: func() time.Time { return fixedNow }})
		defer rs.Close()

		db, err := store.Open(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		mgr, err := playback.New(playback.Deps{
			Catalog:       f.catalog,
			State:         f.state,
			Resolver:      rs,
			Engine:        engine,
			Transcoder:    f.trans,
			SelectProfile: func(c core.ClientCaps, u *core.User, _ string) core.QualityProfile { return rs.ProfileFor(c, u) },
			DB:            db,
			NativeProbe: func(_ context.Context, s core.MediaSource) (*core.MediaInfo, error) {
				info := core.CloneMediaInfo(sampleInfo)
				info.Duration = fightClub.Runtime
				info.Size = s.Size()
				return &info, nil
			},
		}, cfg, playback.Options{
			SessionSurface: func(surface string) string {
				if surface == "plex" {
					return ""
				}
				return surface
			},
			DataDir: dir,
			Now:     func() time.Time { return fixedNow },
		})
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()

		f.srv.setPlayback(mgr)
		f.srv.setVariants(rs)

		headers := map[string]string{
			"X-Plex-Product":           "Plex for Android (TV)",
			"X-Plex-Platform":          "Android",
			"X-Plex-Device":            "SHIELD Android TV",
			"X-Plex-Client-Identifier": "shield-cold-local",
		}
		path := "/library/metadata/" + fightClub.Key.String()

		start := time.Now()
		first := f.request(t, "GET", path, headers)
		elapsed := time.Since(start)
		if first.Code != 200 {
			t.Fatalf("cold detail: %d %s", first.Code, first.Body.String())
		}
		if elapsed >= time.Second {
			t.Fatalf("cold local detail blocked for %s", elapsed)
		}

		if disableSpeculation {
			if idx.searches.Load() != 0 || idx.fetches.Load() != 0 || engine.opens.Load() != 0 {
				t.Fatalf("disabled speculation touched sources: searches=%d fetches=%d opens=%d",
					idx.searches.Load(), idx.fetches.Load(), engine.opens.Load())
			}
			return
		}

		caps := (&Request{
			User:     &ownerUser,
			ClientID: headers["X-Plex-Client-Identifier"],
			Product:  headers["X-Plex-Product"],
			Platform: headers["X-Plex-Platform"],
			Device:   headers["X-Plex-Device"],
		}).ClientCaps(cfg.Transcode.RemoteMaxBitrateKbps)
		info, ok := mgr.WaitWarmProbe(t.Context(), &ownerUser, caps, fightClub.Key, 2*time.Second)
		if !ok || info.Estimated || len(info.Streams) < 4 {
			t.Fatalf("background Prewarm did not publish real tracks: ok=%v info=%+v", ok, info)
		}

		second := f.request(t, "GET", path, headers)
		if second.Code != 200 {
			t.Fatalf("warm detail: %d %s", second.Code, second.Body.String())
		}
		if gotA, gotS := len(audioTags(second.Body.String())), len(subtitleTags(second.Body.String())); gotA < 2 || gotS < 2 {
			t.Fatalf("warm detail missing probed tracks: audio=%d subs=%d", gotA, gotS)
		}

		pq := f.request(t, "POST", "/playQueues?type=video&uri="+url.QueryEscape(path), headers)
		if pq.Code != 200 {
			t.Fatalf("playQueue: %d %s", pq.Code, pq.Body.String())
		}
		if gotA, gotS := len(audioTags(pq.Body.String())), len(subtitleTags(pq.Body.String())); gotA < 2 || gotS < 2 {
			t.Fatalf("playQueue missing probed tracks: audio=%d subs=%d", gotA, gotS)
		}
	}

	t.Run("schedules-prewarm", func(t *testing.T) { run(t, false) })
	t.Run("disable-speculation", func(t *testing.T) { run(t, true) })
}
