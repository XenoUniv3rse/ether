package personalize

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Recommendations pages: how a page's rows bind to served rows and arrange.

func pageServedRow(id, kind string, items int) core.HomeRow {
	r := core.HomeRow{HubID: id, Kind: kind, Title: id}
	for i := 0; i < items; i++ {
		r.Items = append(r.Items, core.Title{Key: core.MovieKey(int64(len(id)*100 + i + 1))})
	}
	r.Hub = core.Hub{ID: id, Items: r.Items}
	return r
}

func pageVisibleIDs(rows []core.HomeRow) []string {
	var out []string
	for _, r := range rows {
		if !r.Hidden {
			out = append(out, r.HubID)
		}
	}
	return out
}

func pageHiddenReason(rows []core.HomeRow, id string) string {
	for _, r := range rows {
		if r.HubID == id && r.Hidden {
			return r.HiddenCode
		}
	}
	return ""
}

func TestCuratedPageIsExactlyItsRowsInOrder(t *testing.T) {
	rows := []core.HomeRow{
		pageServedRow("home.top10", KindTop10, 10),
		pageServedRow("home.trending", KindTrending, 12),
		pageServedRow("home.continue", KindContinue, 3),
		pageServedRow("home.popular", KindPopular, 0),
	}
	p := core.RowPage{Key: core.PageHome, Mode: core.PageCurated, Rows: []core.Row{
		prow(KindTrending, core.PlacementShow, 0),
		prow(KindContinue, core.PlacementShow, 0),
		prow(KindPopular, core.PlacementShow, 0), // empty right now: skipped
	}}
	got := arrangePage(rows, compilePage(p, true, 1, nil), 1, hubContext(testNow, core.DeviceTV), 20)
	if ids := pageVisibleIDs(got); !slices.Equal(ids, []string{"home.trending", "home.continue"}) {
		t.Fatalf("curated = %v", ids)
	}
	if pageHiddenReason(got, "home.top10") != "not_on_page" || pageHiddenReason(got, "home.popular") != "empty" {
		t.Fatalf("hidden reasons: %+v", got)
	}
}

func TestPersonalizedPinsShowsHidesAndPicks(t *testing.T) {
	rows := []core.HomeRow{
		pageServedRow("home.top10", KindTop10, 10),
		pageServedRow("home.trending", KindTrending, 12),
		pageServedRow("home.popular", KindPopular, 12),
		pageServedRow("home.hiddengems", KindHiddenGems, 12),
	}
	p := rpage(core.PageHome, prow(KindPopular, core.PlacementPinned, 1), prow(KindTop10, core.PlacementHide, 0), prow(KindTrending, core.PlacementShow, 0))
	got := arrangePage(rows, compilePage(p, true, 1, nil), 1, hubContext(testNow, core.DeviceTV), 3)
	ids := pageVisibleIDs(got)
	if len(ids) == 0 || ids[0] != "home.popular" || !slices.Contains(ids, "home.trending") || slices.Contains(ids, "home.top10") {
		t.Fatalf("personalized = %v", ids)
	}
	if pageHiddenReason(got, "home.top10") != "hidden" {
		t.Fatalf("hidden row reason = %q", pageHiddenReason(got, "home.top10"))
	}
	// With Ether's picks off only the page's rows (and the app's anchors) show.
	p.Picks = false
	got = arrangePage(rows, compilePage(p, true, 1, nil), 1, hubContext(testNow, core.DeviceTV), 3)
	if ids := pageVisibleIDs(got); slices.Contains(ids, "home.hiddengems") || pageHiddenReason(got, "home.hiddengems") != "picks_off" {
		t.Fatalf("picks off = %v", ids)
	}
	// A pinned or shown row is never dropped by the length.
	got = arrangePage(rows, compilePage(p, true, 1, nil), 1, hubContext(testNow, core.DeviceTV), 1)
	if ids := pageVisibleIDs(got); !slices.Contains(ids, "home.trending") || !slices.Contains(ids, "home.popular") {
		t.Fatalf("length dropped a shown row: %v", ids)
	}
}

func TestAudienceVariantsScheduleAndDevices(t *testing.T) {
	rows := []core.HomeRow{pageServedRow("home.trending", KindTrending, 12), pageServedRow("home.popular", KindPopular, 12)}
	kids := prow(KindPopular, core.PlacementShow, 0)
	kids.Placement.Audience = core.Audience{Mode: core.AudienceOnly, Viewers: []int64{5}}
	tr := prow(KindTrending, core.PlacementShow, 0)
	p := rpage(core.PageHome, tr, kids)
	p.Picks = false
	viewerOverride(&p, 7, tr.ID, core.PlacementHide, 0)
	hc := hubContext(testNow, core.DeviceTV)
	if ids := pageVisibleIDs(arrangePage(rows, compilePage(p, true, 5, nil), 5, hc, 8)); !slices.Equal(ids, []string{"home.trending", "home.popular"}) && !slices.Equal(ids, []string{"home.popular", "home.trending"}) {
		t.Fatalf("viewer 5 = %v", ids)
	}
	got := arrangePage(rows, compilePage(p, true, 7, nil), 7, hc, 8)
	if ids := pageVisibleIDs(got); len(ids) != 0 || pageHiddenReason(got, "home.popular") != "picks_off" || pageHiddenReason(got, "home.trending") != "hidden_viewer" {
		t.Fatalf("viewer 7 = %v %+v", ids, got)
	}
	// Weekend evenings on TV only.
	tr.Placement.When = &core.Schedule{Days: []int{0, 6}}
	tr.Placement.Devices = []string{core.DeviceTV}
	p2 := rpage(core.PageHome, tr)
	p2.Picks = false
	weekday := time.Date(2026, 11, 24, 21, 0, 0, 0, time.UTC) // a Tuesday
	saturday := time.Date(2026, 11, 28, 21, 0, 0, 0, time.UTC)
	if got := arrangePage(rows, compilePage(p2, true, 1, nil), 1, hubContext(weekday, core.DeviceTV), 8); pageHiddenReason(got, "home.trending") != "outside_schedule" {
		t.Fatalf("weekday: %+v", got)
	}
	if got := arrangePage(rows, compilePage(p2, true, 1, nil), 1, hubContext(saturday, core.DeviceMobile), 8); pageHiddenReason(got, "home.trending") != "device" {
		t.Fatalf("phone: %+v", got)
	}
	if ids := pageVisibleIDs(arrangePage(rows, compilePage(p2, true, 1, nil), 1, hubContext(saturday, core.DeviceTV), 8)); !slices.Contains(ids, "home.trending") {
		t.Fatalf("Saturday TV: %v", ids)
	}
}

func TestPersonalContinueWatchingOnlyClaimsItsAudience(t *testing.T) {
	h := newHarness(t, nil)
	personal := prow(KindContinue, core.PlacementPinned, 2)
	personal.Placement.Audience = core.Audience{Mode: core.AudienceOnly, Viewers: []int64{15}}
	hc := hubContext(testNow, core.DeviceTV)
	for _, page := range []struct {
		key                    core.PageKey
		continueID, trendingID string
	}{
		{core.PageHome, "home.continue", "home.trending"},
		{libPage(1), "movie.inprogress.1", "movie.trending@1"},
	} {
		t.Run(string(page.key), func(t *testing.T) {
			p := rpage(page.key, personal)
			ctx := withDraftPage(context.Background(), p)
			rows := []core.HomeRow{
				pageServedRow(page.trendingID, KindTrending, 8),
				pageServedRow(page.continueID, KindContinue, 3),
			}
			for _, viewer := range []int64{1, 2, 15} {
				pl := compilePage(p, true, viewer, nil)
				got := arrangePage(rows, pl, viewer, hc, 8)
				slot, rowID := 1, ""
				if viewer == 15 {
					slot, rowID = 2, personal.ID
				}
				if len(got) < slot || got[slot-1].Hidden || got[slot-1].HubID != page.continueID || got[slot-1].RowID != rowID || got[slot-1].Pinned != (viewer == 15) {
					t.Fatalf("viewer %d: want Continue Watching at slot %d with row ID %q, got %+v", viewer, slot, rowID, got)
				}
				if page.key != core.PageHome {
					continue
				}
				user := &core.User{ID: viewer}
				if !h.e.HomeRowEnabled(ctx, user, hc, page.continueID) {
					t.Errorf("viewer %d: app-built Continue Watching is disabled", viewer)
				}
				if viewer != 15 {
					preview := h.e.appendAbsentPageRows(ctx, got, pl, user, hc, "", nil)
					if pageHiddenReason(preview, "home."+personal.ID) != "audience" {
						t.Errorf("viewer %d: personal row missing from the preview's hidden rows", viewer)
					}
				}
			}
			shared := prow(KindContinue, core.PlacementShow, 0)
			p.Rows = append(p.Rows, shared)
			if r := compilePage(p, true, 1, nil).match(rows[1]); r == nil || r.RowID != shared.ID {
				t.Fatal("the first in-audience plain row should bind the built-in hub")
			}
		})
	}
}

func TestOneRowPerLibraryBindsAndStaysTogether(t *testing.T) {
	row := prow(KindTrending, core.PlacementPinned, 2)
	row.Scope = core.RowScope{Libraries: []core.LibraryID{1, 2}, PerLibrary: true}
	p := rpage(core.PageHome, prow(KindContinue, core.PlacementPinned, 1), row)
	pl := compilePage(p, true, 1, nil)
	rows := []core.HomeRow{
		pageServedRow("home.continue", KindContinue, 2),
		pageServedRow("home."+row.ID+"@2", KindTrending, 12),
		pageServedRow("home."+row.ID+"@1", KindTrending, 12),
		pageServedRow("home.trending", KindTrending, 12), // the engine's own row: not a pick
	}
	got := arrangePage(rows, pl, 1, hubContext(testNow, core.DeviceTV), 8)
	ids := pageVisibleIDs(got)
	want := []string{"home.continue", "home." + row.ID + "@1", "home." + row.ID + "@2"}
	if len(ids) < 3 || !slices.Equal(ids[:3], want) || slices.Contains(ids, "home.trending") {
		t.Fatalf("fan-out = %v, want %v first and no plain trending", ids, want)
	}
	for _, r := range got {
		if r.HubID == "home."+row.ID+"@2" && (r.RowID != row.ID || r.Library != 2) {
			t.Fatalf("fan-out row identity: %+v", r)
		}
	}
	// Titles: "{library}" in the owner's name, else source + library.
	base := &pageRowBase{kind: KindTrending}
	named := &rule{Name: "Hot in {library}"}
	if c := pageCandRow(named, base, nil, "Anime Shows", "x", "Trending"); c.title != "Hot in Anime Shows" {
		t.Fatalf("named = %q", c.title)
	}
	if c := pageCandRow(&rule{}, base, nil, "Movies", "x", "Trending"); c.title != "Trending Movies" {
		t.Fatalf("plain = %q", c.title)
	}
}

func TestPageRowsBuiltForThePageAndScope(t *testing.T) {
	trending := testRow(KindTrending, "trending", nil)
	var items []candItem
	for i := 0; i < 6; i++ {
		typ, key := core.TypeMovie, core.MovieKey(int64(i+1))
		if i%2 == 1 {
			typ, key = core.TypeShow, core.ShowKey(int64(i+1))
		}
		items = append(items, candItem{t: core.Title{Key: key, Type: typ, Genres: []string{"Animation"}}})
	}
	trending.items = items
	trending.typed = splitTyped(items, 10)
	h := newHarness(t, func(o *Options) {
		o.PageLibraries = func(context.Context) []core.PageLibrary {
			return []core.PageLibrary{
				{ID: 1, Name: "Movies", MediaType: core.TypeMovie, Experience: "watch", Caps: []string{core.CapCharts, core.CapTaste, core.CapMovies}},
				{ID: 2, Name: "TV Shows", MediaType: core.TypeShow, Experience: "watch", Caps: []string{core.CapCharts, core.CapTaste, core.CapEpisodes}},
				{ID: 9, Name: "Music", Experience: "listen", Plugin: "Music", Caps: []string{core.CapShelves}},
			}
		}
	})
	row := prow(KindTrending, core.PlacementShow, 0)
	row.Scope = core.RowScope{PerLibrary: true}
	p := rpage(core.PageHome, row)
	pl := h.e.compilePlan(context.Background(), p, true, 1)
	got := h.e.pageRows(context.Background(), pl, emptyModel(trending), nil, &core.User{ID: 1}, "")
	if len(got) != 2 || got[0].title != "Trending Movies" || got[1].title != "Trending TV Shows" {
		t.Fatalf("any library, one row each = %+v", got)
	}
	for _, it := range got[0].items {
		if it.t.Type != core.TypeMovie {
			t.Fatalf("a show in the movies row: %+v", it)
		}
	}
	// A viewer who can't see TV Shows gets only the movies row.
	got = h.e.pageRows(context.Background(), pl, emptyModel(trending), nil, &core.User{ID: 1, AllowedLibraries: []core.LibraryID{1}}, "")
	if len(got) != 1 || got[0].id != row.ID+"@1" {
		t.Fatalf("library limits = %+v", got)
	}
}

func TestSuppressedEngineRowWhenThePageBuildsItsOwn(t *testing.T) {
	day := prow(KindTrending, core.PlacementShow, 0)
	day.Source.Params = map[string]string{"window": "day"}
	day.Placement.Audience = core.Audience{Mode: core.AudienceOnly, Viewers: []int64{15}}
	p := rpage(core.PageHome, day)
	weekly := core.HomeRow{HubID: "home.trending", Kind: KindTrending}
	pl := compilePage(p, true, 15, nil)
	if !pl.suppressed(weekly) {
		t.Fatal("the engine's weekly row should give way to the page's Today row")
	}
	if pl.match(core.HomeRow{HubID: "home." + day.ID, Kind: KindTrending}) == nil {
		t.Fatal("the page's own row must bind")
	}
	if other := compilePage(p, true, 1, nil); other.suppressed(weekly) || other.match(weekly) != nil {
		t.Fatal("the engine's weekly row should use defaults outside the Today row's audience")
	}
	p.Rows = append(p.Rows, prow(KindTrending, core.PlacementShow, 0))
	plain := compilePage(p, true, 1, nil)
	if plain.suppressed(weekly) || plain.match(weekly) == nil {
		t.Fatal("a plain row binds the engine's own row")
	}
}

func TestUnsavedPluginPageIsCuratedOverItsShelves(t *testing.T) {
	libs := map[core.LibraryID]core.PageLibrary{9: {ID: 9, Name: "Music", Plugin: "Music"}}
	pl := compilePage(DefaultPage(core.LibraryPage(9, "home")), false, 1, libs)
	e := &Engine{}
	rows := []core.HomeRow{
		pageServedRow("plugin:9:continue", KindContinue, 2),
		pageServedRow("plugin:9:facet:genre:rock", "plugin", 8),
		pageServedRow("plugin:9:shelf:new", "plugin", 8),
		pageServedRow("plugin:9:shelf:charts", "plugin", 8),
	}
	got := arrangePage(rows, e.pluginDefaultPlan(pl, rows), 1, hubContext(testNow, core.DeviceTV), 8)
	if ids := pageVisibleIDs(got); !slices.Equal(ids, []string{"plugin:9:continue", "plugin:9:shelf:new", "plugin:9:shelf:charts"}) {
		t.Fatalf("plugin default = %v", ids)
	}
}

func TestSavePageRevisionsAndValidation(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	p := rpage(core.PageHome, prow(KindTrending, core.PlacementPinned, 2))
	p.Rows[0].ID = "rclient01" // client-made ids are kept when well formed
	saved, err := h.e.SavePage(ctx, p)
	if err != nil || saved.Revision != 1 || saved.Rows[0].ID != "rclient01" {
		t.Fatalf("first save = %+v %v", saved, err)
	}
	if _, err := h.e.SavePage(ctx, p); !errors.Is(err, ErrPageConflict) {
		t.Fatalf("stale save = %v", err)
	}
	bad := []func(*core.RowPage){
		func(p *core.RowPage) { p.Rows[0].Source.Kind = "no-such-row" },
		func(p *core.RowPage) { p.Rows[0].Source.Params = map[string]string{"window": "month"} },
		func(p *core.RowPage) { p.Rows[0].Source.Kind = KindContinue; p.Rows[0].Scope.PerLibrary = true },
		func(p *core.RowPage) { p.Key = libPage(1); p.Rows[0].Scope.Libraries = []core.LibraryID{2} },
		func(p *core.RowPage) { p.Key = libPage(1); p.Rows[0].Source.Kind = KindLive },
	}
	for i, m := range bad {
		q := rpage(core.PageHome, prow(KindTrending, core.PlacementShow, 0))
		m(&q)
		var ve *ValidationError
		if _, err := h.e.NormalizePage(ctx, q); !errors.As(err, &ve) {
			t.Errorf("case %d: want a validation error, got %v", i, err)
		}
	}
	// A plugin's list stays on the page while the plugin isn't loaded.
	q := rpage(core.PageHome, prow("plugin:gone/list", core.PlacementShow, 0))
	if _, err := h.e.NormalizePage(ctx, q); err != nil {
		t.Fatalf("unloaded plugin list: %v", err)
	}
	if err := h.e.ResetPage(ctx, core.PageHome); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.e.Page(ctx, core.PageHome); ok {
		t.Fatal("reset page still saved")
	}
}

func TestMigrateRowsPagesFromTheOldLayout(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	old := `{"global":[
		{"section":"home","maxRows":12,"rows":[
			{"row":"home.continue","status":"always","slot":1},
			{"row":"trending","status":"always","slot":2,"style":"shelf","hideWatched":true},
			{"row":"collection.trending-day-movie","status":"always"},
			{"row":"because.*","status":"eligible","maxPerPage":2},
			{"row":"top-10","status":"eligible"},
			{"row":"top-rated","status":"off"},
			{"row":"collection:42","status":"always","when":{"days":[5,6],"devices":["tv"]}}
		]},
		{"section":"library:1","maxRows":2,"rows":[
			{"row":"movie.era","status":"always","slot":1},
			{"row":"movie.trending","status":"always","slot":2}
		]}],
	 "viewers":[{"userId":7,"pages":[{"section":"home","rows":[
			{"row":"trending","status":"off"},
			{"row":"hidden-gems","status":"always"}
	 ]}]}]}`
	if err := h.e.SetMigrationState(ctx, pagesBackupMeta, old); err != nil {
		t.Fatal(err)
	}
	n, err := h.e.MigrateRowsPages(ctx, []LegacyCollectionPlacement{
		{ID: "42", MediaType: "movie", Enabled: true, HomeRow: true, Viewers: []int64{3, 4}},
		{ID: "77", MediaType: "movie", Enabled: true, HomeRow: true},
		{ID: "trending-week-movie", Enabled: true, HomeRow: true},
	})
	if err != nil || n != 2 {
		t.Fatalf("migrated %d pages: %v", n, err)
	}
	home, ok := h.e.Page(ctx, core.PageHome)
	if !ok || home.Mode != core.PagePersonalized || home.Length.Rows != 12 {
		t.Fatalf("home = %+v", home)
	}
	find := func(p core.RowPage, kind, param string) *core.Row {
		for i := range p.Rows {
			r := &p.Rows[i]
			if r.Source.Kind == kind && (param == "" || r.Source.Params["collection"] == param || r.Source.Params["window"] == param) {
				return r
			}
		}
		return nil
	}
	if r := find(home, KindContinue, ""); r == nil || r.Placement.Mode != core.PlacementPinned || r.Placement.Position != 1 {
		t.Fatalf("continue = %+v", r)
	}
	if r := find(home, KindTrending, ""); r == nil || r.Placement.Position != 2 || r.Look.Style != core.RowStyleStandard || !r.Look.HideWatched {
		t.Fatalf("trending = %+v", r)
	}
	if r := find(home, KindTrending, "day"); r == nil || r.Placement.Mode != core.PlacementShow || !slices.Equal(r.Scope.Libraries, []core.LibraryID{core.LibraryMovies}) {
		t.Fatalf("discovery collection → Trending Today, Movies: %+v", r)
	}
	if r := find(home, KindBecause, ""); r == nil || r.Source.Params["count"] != "2" || r.Placement.Mode != core.PlacementAuto {
		t.Fatalf("because-you-watched = %+v", r)
	}
	if find(home, KindTop10, "") != nil {
		t.Fatal("a plain Eligible is the default and isn't kept")
	}
	if r := find(home, KindTopRated, ""); r == nil || r.Placement.Mode != core.PlacementHide {
		t.Fatalf("top rated = %+v", r)
	}
	r42 := find(home, SourceCollection, "42")
	if r42 == nil || r42.Placement.When == nil || !slices.Equal(r42.Placement.Devices, []string{"tv"}) ||
		r42.Placement.Audience.Mode != core.AudienceOnly || !slices.Equal(r42.Placement.Audience.Viewers, []int64{3, 4}) {
		t.Fatalf("collection 42 = %+v", r42)
	}
	if r := find(home, SourceCollection, "77"); r == nil || r.Placement.Mode != core.PlacementAuto {
		t.Fatalf("collection 77 published to Home = %+v", r)
	}
	if find(home, SourceCollection, "trending-week-movie") != nil {
		t.Fatal("a discovery collection's own targets must not add a collection row")
	}
	v := home.VariantFor(7)
	if v == nil || len(v.Rows) != 1 {
		t.Fatalf("viewer 7 variant = %+v", v)
	}
	if hg := find(home, KindHiddenGems, ""); hg == nil || hg.Placement.Audience.Mode != core.AudienceOnly || !slices.Equal(hg.Placement.Audience.Viewers, []int64{7}) {
		t.Fatalf("a row only viewer 7 had = %+v", hg)
	}
	// Every row anchored and the length cut to them: a Curated page now.
	movies, ok := h.e.Page(ctx, libPage(1))
	if !ok || movies.Mode != core.PageCurated || len(movies.Rows) != 2 || movies.Rows[0].Source.Kind != KindEra || movies.Rows[0].Placement.Mode != core.PlacementShow {
		t.Fatalf("movies = %+v", movies)
	}
}

// Two pins on one position: the first keeps it, the next says which row
// holds the place; Ether's own rows say what they really follow.
func TestPinCollisionAndNeighbourWhy(t *testing.T) {
	a := prow(KindPopular, core.PlacementPinned, 2)
	b := prow(KindTrending, core.PlacementPinned, 2)
	p := rpage(core.PageHome, a, b)
	rows := []core.HomeRow{
		pageServedRow("home.popular", KindPopular, 12),
		pageServedRow("home.trending", KindTrending, 12),
		pageServedRow("home.top10", KindTop10, 10),
	}
	var got []core.HomeRow
	for _, r := range arrangePage(rows, compilePage(p, true, 1, nil), 1, hubContext(testNow, core.DeviceTV), 8) {
		if !r.Hidden {
			got = append(got, r)
		}
	}
	if len(got) != 3 || got[1].HubID != "home.popular" || got[2].HubID != "home.trending" {
		t.Fatalf("order = %v", pageVisibleIDs(got))
	}
	if want := "Pinned to #2 for everyone; home.popular is pinned there too, so it's at #3"; got[2].Why != want {
		t.Fatalf("collision why = %q", got[2].Why)
	}
}
