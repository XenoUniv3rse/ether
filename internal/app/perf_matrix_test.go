package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/perfharness"
	"github.com/braedonsaunders/ether/internal/store"
)

type perfMatrixCase struct {
	name, movieMode, showMode, visibility                 string
	upcomingMovies, upcomingEpisodes, sources, live, rows bool
	viewer                                                int
}

var perfMatrixCases = []perfMatrixCase{
	{name: "baseline"},
	{name: "playable", movieMode: config.ModePlayable, showMode: config.ModePlayable},
	{name: "live", live: true},
	{name: "playable-restricted", movieMode: config.ModePlayable, showMode: config.ModePlayable, visibility: "hide", viewer: perfharness.RestrictedViewer},
	{name: "digital", movieMode: config.ModeDigital, showMode: config.ModeDigital},
	{name: "playable-movies", movieMode: config.ModePlayable, showMode: config.ModeDigital},
	{name: "playable-shows", movieMode: config.ModeDigital, showMode: config.ModePlayable},
	{name: "hide-default", visibility: "hide"},
	{name: "ordered-rules", visibility: "show"},
	{name: "upcoming-movies", upcomingMovies: true},
	{name: "upcoming-episodes", upcomingEpisodes: true},
	{name: "new-viewer", viewer: 1},
	{name: "restricted", viewer: perfharness.RestrictedViewer},
	{name: "plex-home", viewer: 3},
	{name: "recommendations", rows: true, live: true},
	{name: "sources", sources: true},
	{name: "playable-upcoming-sources", movieMode: config.ModePlayable, showMode: config.ModePlayable, upcomingMovies: true, upcomingEpisodes: true, sources: true},
	{name: "live-playable-rules", movieMode: config.ModePlayable, showMode: config.ModePlayable, visibility: "show", live: true},
}

var perfMatrixScale = perfharness.Scale{
	Movies: 35_000, Shows: 15_000, SeasonsPerShow: 3, EpisodesPerSeason: 6,
	Viewers: 2, HistoryPerViewer: 20_000, InProgressPerViewer: 40, PluginItems: 10_000,
}

func testPerformanceMatrix(t *testing.T) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Minute)
	defer cancel()
	cache := os.Getenv("ETHER_PERF_GATE_FIXTURES")
	if cache == "" {
		cache = t.TempDir()
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	var report perfharness.MatrixReport
	path := os.Getenv("ETHER_PERF_GATE_REPORT")
	writeReport := func() {
		if path == "" {
			return
		}
		var out bytes.Buffer
		report.Write(&out, time.Since(start))
		if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
			t.Error("write matrix report:", err)
		}
	}
	t.Cleanup(func() {
		writeReport()
		t.Logf("matrix elapsed %s, %d over-budget route/configuration pairs, %d coverage gaps", time.Since(start).Round(time.Second), report.OffenderCount(), report.GapCount())
	})
	fixture := perfMatrixFixture(t, ctx, cache)
	var music perfMusicFixture
	if dir := os.Getenv("ETHER_PERF_GATE_MUSIC_SOURCE"); dir != "" {
		var err error
		music, err = readPerfMusicFixture(dir)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range perfMatrixCases {
		if only := os.Getenv("ETHER_PERF_GATE_MATRIX_CASES"); only != "" && !strings.Contains(","+only+",", ","+c.name+",") {
			continue
		}
		if ctx.Err() != nil {
			t.Error(ctx.Err())
			break
		}
		t.Run(c.name, func(t *testing.T) {
			caseCtx, stopCase := context.WithTimeout(ctx, 3*time.Minute)
			defer stopCase()
			runPerfMatrixCase(t, caseCtx, fixture, c, music, &report, writeReport)
		})
	}
	if report.OffenderCount()+report.GapCount() != 0 {
		t.Fail()
	}
}

func perfMatrixFixture(t *testing.T, ctx context.Context, cache string) string {
	t.Helper()
	path := filepath.Join(cache, "matrix-v2.db")
	stamp := filepath.Join(cache, "matrix-v2.date")
	if b, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(b)) == time.Now().UTC().Format(time.DateOnly) {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	scratch, err := os.MkdirTemp(cache, "build-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(scratch)
	db, err := store.Open(ctx, scratch)
	if err != nil {
		t.Fatal(err)
	}
	if err := perfharness.Generate(ctx, db, perfMatrixScale, time.Now()); err != nil {
		db.Close()
		t.Fatal(err)
	}
	// Keep one first-time viewer distinct from the heavy and restricted viewers.
	for _, query := range perfMatrixNewViewerQueries {
		if _, err := db.ExecContext(ctx, query); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	// Publish the normal serving indexes before cloning the fixture.
	cfg, err := config.Open(scratch)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	cat, err := catalog.New(db, nil, cfg.Get().Catalog, nil, catalog.WithStartupContext(ctx))
	if err == nil {
		err = cat.SeedXtreamIDs(ctx)
	}
	if err == nil {
		_, err = cat.RebuildRank(ctx)
	}
	if err == nil {
		err = cat.ReplacePopularityIndexes(ctx)
	}
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(scratch, "ether.db"), path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stamp, []byte(time.Now().UTC().Format(time.DateOnly)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var perfMatrixNewViewerQueries = []string{
	"DELETE FROM state_watch WHERE user_id=(SELECT id FROM auth_users WHERE username='viewer01')",
}

func runPerfMatrixCase(t *testing.T, ctx context.Context, fixture string, c perfMatrixCase, music perfMusicFixture, report *perfharness.MatrixReport, writeReport func()) {
	start := time.Now()
	defer func() { t.Logf("replayed in %s", time.Since(start).Round(time.Second)) }()
	dir, err := os.MkdirTemp(filepath.Dir(fixture), "case-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := cloneFile(fixture, filepath.Join(dir, "ether.db")); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(cfg *config.Config) error {
		cfg.SetupComplete = true
		cfg.Frontends = map[string]config.Frontend{"plex": {Enabled: true}, "jellyfin": {Enabled: true}, "emby": {Enabled: true}, "silo": {Enabled: true}, "vela": {Enabled: true}, "stremio": {Enabled: true}, "xtream": {Enabled: true}}
		cfg.Server.BindAddress = "127.0.0.1"
		cfg.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		if c.live {
			cfg.Plex.Token = "fixture-plex-owner"
			cfg.Plex.Claimed = true
		}
		cfg.Availability.Mode = config.ModeImmediate
		cfg.Availability.MovieMode, cfg.Availability.ShowMode = c.movieMode, c.showMode
		cfg.Availability.ApplyMode()
		cfg.Catalog.ShowUpcomingMovies, cfg.Catalog.ShowUpcomingEpisodes = c.upcomingMovies, c.upcomingEpisodes
		if c.sources {
			cfg.Debrid.Providers = []config.DebridProvider{{ID: "fixture", Kind: config.DebridTorBox, APIKey: "fixture", Enabled: true}}
			cfg.Usenet.Providers = []config.UsenetProvider{{ID: "fixture", Host: "127.0.0.1", Port: 1, Connections: 1, MinIdle: -1, Enabled: true}}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedPerfMatrixCatalogue(ctx, db, c.live); err != nil {
		db.Close()
		t.Fatal(err)
	}
	viewers, err := perfharness.MintCredentials(ctx, db, 4)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if pg := os.Getenv(PerfGatePGEnv); pg != "" {
		defer perfGatePostgres(t, ctx, dir, strings.ReplaceAll(c.name, "-", "_"), pg)()
	}

	trace := filepath.Join(dir, "trace.jsonl")
	t.Setenv(PerfTraceEnv, trace)
	options := Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if c.live {
		options.plextvHTTP = &http.Client{Transport: perfMatrixPlexTV{}}
	}
	if c.sources {
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"success":true,"data":[]}`)
		}))
		defer provider.Close()
		options.debridProviderOptions = debrid.ProviderOptions{BaseURL: provider.URL, HTTPClient: provider.Client()}
	}
	a, err := New(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// All stock matrix posters share this synthetic TMDB path. Seed its
	// cache, so authorised artwork requests exercise the route offline.
	var poster bytes.Buffer
	if err := jpeg.Encode(&poster, image.NewRGBA(image.Rect(0, 0, 10, 16)), nil); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"original/p.jpg", "w500/p.jpg"} {
		if _, _, err := a.images.GetLoaded(ctx, key, func(context.Context) ([]byte, string, error) {
			return poster.Bytes(), "image/jpeg", nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	a.catalog.FreezeCacheMinute()
	viewers[0].PluginHost, err = perfMatrixPluginCredential(ctx, a, viewers[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(music.methods) != 0 {
		if err := music.install(ctx, a, &viewers[c.viewer]); err != nil {
			report.AddGap(c.name, "subsonic", "", "", "could not install and seed the Music fixture")
			t.Error("Music fixture:", err)
			music.methods = nil
		}
	}
	if c.live {
		if err := a.registrar.RefreshOwner(ctx); err != nil {
			report.AddGap(c.name, "plex", "", "", "Plex Pass fixture unavailable")
			t.Error("Plex Pass fixture:", err)
		}
		if err := installPerfMatrixSports(ctx, a); err != nil {
			report.AddGap(c.name, "harness", "", "", "sports overlay fixture unavailable")
			t.Error("sports overlay fixture:", err)
		}
	}
	if c.rows {
		if err := seedPerfMatrixRows(ctx, a); err != nil {
			report.AddGap(c.name, "harness", "", "", "recommendation rows could not be seeded")
			t.Error("recommendations fixture:", err)
		}
	}
	if c.visibility != "" {
		show := core.LibraryExclusion{Action: "show", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{{Field: "genre", Op: "in", Value: json.RawMessage(`["Action","Comedy","Drama"]`)}}}
		rules := []core.LibraryExclusion{show}
		if c.visibility == "show" {
			rules = append(rules, core.LibraryExclusion{Action: "hide", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{{Field: "genre", Op: "in", Value: json.RawMessage(`["Horror"]`)}}})
		}
		for _, lib := range []core.LibraryID{core.LibraryMovies, core.LibraryShows} {
			if err := a.libraries.SetRules(ctx, lib, rules, c.visibility); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.refreshLibraryPolicies(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if c.viewer == 0 {
		u, err := a.users.Owner(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.personalize.Rebuild(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	a.frontends.start(ctx, server.Listener.Addr())
	checkpointCtx, stopCheckpoint := context.WithCancel(ctx)
	checkpointDone := make(chan struct{})
	go func() {
		defer close(checkpointDone)
		a.checkpointWAL(checkpointCtx)
	}()
	defer func() {
		stopCheckpoint()
		<-checkpointDone
	}()
	if len(music.methods) != 0 && a.frontends.control.listeners.sharedHandler("/subsonic/rest/ping.view") == nil {
		report.AddGap(c.name, "subsonic", "", "", "Music adapter is enabled without a shared listener")
		t.Error("Music adapter shared listener unavailable")
	}
	settleMisses := 0
	a.perf.mu.Lock()
	a.perf.asyncTrace = true
	a.perf.settleTrace = func(ctx context.Context) {
		if !a.quiesce(ctx, 200*time.Millisecond) {
			settleMisses++
		}
	}
	a.perf.mu.Unlock()
	r := perfharness.NewRunner(server.Listener.Addr().(*net.TCPAddr).Port, true, viewers)
	defer r.Close()
	// A request still running at this point has already missed its replay
	// window. Its completed trace remains useful even when the client cancels.
	r.SetTimeout(8 * time.Second)
	r.SetBefore(func(context.Context) { a.perf.waitTraces() })
	inventory := perfMatrixInventory(t, a, c, music.methods, report)
	r.SetMatrixRoutes(inventory)
	cs := append(perfharness.MatrixClients(c.live), perfharness.MatrixPluginHost())
	if len(music.methods) != 0 {
		cs = append(cs, perfharness.MatrixSubsonic(music.methods))
	}
	perfharness.AppendMatrixReads(cs, inventory, perfMatrixRouteExceptions)
	if only := os.Getenv("ETHER_PERF_GATE_CLIENTS"); only != "" {
		cs = perfharness.Only(cs, strings.Split(only, ","))
	}
	if steps := os.Getenv("ETHER_PERF_GATE_STEPS"); steps != "" {
		cs = perfharness.OnlySteps(cs, strings.Split(steps, ","))
	}
	var selected, ownerOnly []perfharness.Client
	for _, client := range cs {
		if client.Surface == "console" || client.Surface == "mcp" || client.Surface == "plugin-host" {
			ownerOnly = append(ownerOnly, client)
		} else {
			selected = append(selected, client)
		}
	}
	r.Sequential(ctx, c.name, viewers[c.viewer:c.viewer+1], selected)
	r.Sequential(ctx, c.name, viewers[:1], ownerOnly)
	if ctx.Err() != nil {
		report.AddGap(c.name, "harness", "", "", "case deadline reached before completing replay")
	}
	server.Close()
	a.perf.waitTraces()
	if settleMisses != 0 {
		report.AddGap(c.name, "harness", "", "", fmt.Sprintf("background work was still active after %d responses", settleMisses))
	}
	byTag, _, err := perfharness.ReadTrace(trace)
	if err != nil {
		t.Fatal(err)
	}
	report.Add(c.name, r.Results(), byTag, cs)
	report.CheckRoutes(c.name, inventory, r.Results(), byTag, perfMatrixRouteExceptions)
	t.Logf("recorded %d attempts", len(r.Results()))
	writeReport()
}

type perfMatrixPlexTV struct{}

func (perfMatrixPlexTV) RoundTrip(r *http.Request) (*http.Response, error) {
	status, body := http.StatusOK, `[]`
	switch r.URL.Path {
	case "/api/v2/user":
		body = `{"id":9101,"uuid":"matrix-owner","username":"owner","subscription":{"active":true}}`
	case "/api/v2/features":
	default:
		status, body = http.StatusNotFound, `{}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}
