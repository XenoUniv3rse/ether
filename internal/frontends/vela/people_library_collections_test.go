package vela

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

type peopleTestExperience struct {
	frontendtest.Experience
	personImage   frontend.Image
	personDetail  frontend.PersonDetail
	visiblePerson bool
	itemCallsMu   sync.Mutex
	itemCalls     []string
}

func (e *peopleTestExperience) People(context.Context, *frontend.Viewer, frontend.Device, frontend.PeopleQuery) ([]frontend.PersonDetail, error) {
	return nil, nil
}

func (e *peopleTestExperience) Person(_ context.Context, v *frontend.Viewer, _ frontend.Device, id string, opts frontend.PersonReadOptions) (frontend.PersonDetail, error) {
	if !e.visiblePerson || v.ID != 1 || id != e.personDetail.ID {
		return frontend.PersonDetail{}, core.ErrNotFound
	}
	out := e.personDetail
	if opts.Prefetch {
		out.Images = nil
	}
	return out, nil
}

func (e *peopleTestExperience) PersonCredit(_ context.Context, v *frontend.Viewer, _ frontend.Device, item frontend.RatingKey, tmdb int64) (frontend.PersonDetail, error) {
	if !e.visiblePerson || v.ID != 1 || item != e.personDetail.ArtworkItem || tmdb != e.personDetail.TMDB {
		return frontend.PersonDetail{}, core.ErrNotFound
	}
	return e.personDetail, nil
}

func (e *peopleTestExperience) AuthorizePersonImages(_ context.Context, viewer *frontend.Viewer, device core.Device, targets []frontend.PersonImageTarget) ([]bool, error) {
	allowed := make([]bool, len(targets))
	for n, target := range targets {
		allowed[n] = e.visiblePerson && viewer.ID == 1 && device.ID == "tv" && target.Item == e.personDetail.ArtworkItem && frontend.SameImage(target.Image, e.personImage)
	}
	return allowed, nil
}

func (e *peopleTestExperience) AuthorizePersonImage(ctx context.Context, viewer *core.Viewer, _ core.Device, item core.RatingKey, image frontend.Image, admission func(context.Context) error) error {
	if !e.visiblePerson || viewer.ID != 1 || item != e.personDetail.ArtworkItem || !frontend.SameImage(image, e.personImage) {
		return core.ErrUnauthorized
	}
	if admission != nil {
		return admission(ctx)
	}
	return nil
}

func TestVelaPeopleAndFilmographyVisibility(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, castMembers{}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := grants.Issue(ctx, 1, nil, "vela", core.Device{ID: "tv"})
	if err != nil {
		t.Fatal(err)
	}

	personID, _ := ids.GUID(ids.Person, 77)
	portrait := frontend.Image{Kind: frontend.ImagePerson, Path: "person-77", Tag: "v1", Subject: "tmdb:77", Width: 185, Height: 278}
	movieOld := core.Title{
		Key:       core.MovieKey(42),
		Type:      core.TypeMovie,
		Title:     "Arrival",
		Year:      2016,
		Released:  time.Date(2016, 11, 11, 0, 0, 0, 0, time.UTC),
		Cast:      []core.Person{{TMDB: 77, Name: "Denis Talent", Role: "Cameo"}},
		Directors: []core.Person{{TMDB: 77, Name: "Denis Talent", Role: "Director"}},
	}
	movieNew := core.Title{
		Key:       core.MovieKey(99),
		Type:      core.TypeMovie,
		Title:     "Dune: Part Two",
		Year:      2024,
		Released:  time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
		Directors: []core.Person{{TMDB: 77, Name: "Denis Talent", Role: "Director"}},
		Writers:   []core.Person{{TMDB: 77, Name: "Denis Talent", Role: "Writer"}},
	}
	showMid := core.Title{
		Key:      core.ShowKey(55),
		Type:     core.TypeShow,
		Title:    "The Son",
		Year:     2021,
		Released: time.Date(2021, 5, 15, 0, 0, 0, 0, time.UTC),
		Cast:     []core.Person{{TMDB: 77, Name: "Denis Talent", Role: "Lead"}},
	}

	views := []frontend.View{
		{ID: "1", Name: "Movies", Type: frontend.TypeMovie, Revision: "r1"},
		{ID: "23", Name: "Sci-Fi Picks", Type: frontend.TypeMovie, Revision: "r1"},
		{ID: "2", Name: "TV Shows", Type: frontend.TypeShow, Revision: "r1"},
		{ID: "71", Name: "Plugin Archive", Type: frontend.TypeMovie, PluginID: "plugin", KindKey: "clip"},
	}

	exp := &peopleTestExperience{
		personImage:   portrait,
		visiblePerson: true,
		personDetail: frontend.PersonDetail{
			ID:          personID,
			Name:        "Denis Talent",
			TMDB:        77,
			ArtworkItem: movieOld.Key,
			Images:      []frontend.Image{portrait},
			KnownFor:    "Directing",
			Biography:   "Canadian filmmaker.",
			Birthday:    "1967-10-03",
			Roles:       []string{"Directing", "Acting", "Writing"},
		},
	}
	exp.ViewsFunc = func(_ context.Context, v *frontend.Viewer) ([]frontend.View, error) {
		if v.ID != 1 {
			t.Fatalf("unexpected viewer %v", v)
		}
		return views, nil
	}
	exp.ItemFunc = func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if req.Key != movieOld.Key {
			return frontend.ItemDetail{}, core.ErrNotFound
		}
		return frontend.ItemDetail{Title: movieOld, Images: []frontend.Image{portrait}, ImagesAuthoritative: true}, nil
	}
	exp.PersonFacetIDsFunc = func(_ context.Context, _ *frontend.Viewer, _ core.Device, key core.RatingKey, requested []int64) (map[int64]string, error) {
		if key != movieOld.Key {
			return nil, core.ErrNotFound
		}
		out := map[int64]string{}
		for _, id := range requested {
			if id == 77 {
				out[77] = personID
			}
		}
		return out, nil
	}
	exp.LibraryItemsFunc = func(_ context.Context, _ *frontend.Viewer, _ core.Device, viewID string, q frontend.ItemQuery) (frontend.Page, error) {
		exp.itemCallsMu.Lock()
		exp.itemCalls = append(exp.itemCalls, viewID)
		exp.itemCallsMu.Unlock()
		if q.Filter.Person != 77 || q.Sort != frontend.SortReleased || !q.Desc || !q.NoTotal {
			t.Fatalf("unexpected filmography query on %s: %+v", viewID, q)
		}
		var all []frontend.Title
		switch viewID {
		case "1":
			all = []frontend.Title{movieNew, movieOld}
		case "23":
			// Duplicate movieNew in custom library must be deduplicated when libraryId is omitted.
			all = []frontend.Title{movieNew}
		case "2":
			all = []frontend.Title{showMid}
		default:
			t.Fatalf("queried unexpected library %q", viewID)
		}
		start := min(q.Offset, len(all))
		end := min(start+q.Limit, len(all))
		return frontend.Page{Items: all[start:end]}, nil
	}

	imgService, err := identity.NewArtworkImageGrantService(db, "vela", grants, func() frontend.Experience { return exp }, castArtwork{}, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	seal, err := encryption(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	inst := &instance{seal: seal, mux: http.NewServeMux(), host: frontend.Host{Experience: exp, Identity: identityFake{}, ImageGrants: imgService}}
	inst.routes()

	doGET := func(path string) (int, object) {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+credential)
		r.Header.Set("X-Vela-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.serve(w, r)
		var out object
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s decode: %v body=%s", path, err, w.Body.String())
		}
		return w.Code, out
	}

	// 1. TitleDetail.cast[] and TitleDetail.crew[] carry person id.
	code, detail := doGET("/vela/v0/titles/" + ids.RatingKeyGUID(movieOld.Key))
	if code != 200 {
		t.Fatalf("detail status=%d body=%v", code, detail)
	}
	castList := detail["cast"].([]any)
	crewList := detail["crew"].([]any)
	if len(castList) != 1 || castList[0].(map[string]any)["id"] != personID || len(crewList) != 1 || crewList[0].(map[string]any)["id"] != personID {
		t.Fatalf("missing person id on credits: cast=%v crew=%v", castList, crewList)
	}

	// 2. GET /vela/v0/people/:id returns signed photo + profile metadata + roles.
	code, personBody := doGET("/vela/v0/people/" + personID)
	if code != 200 || personBody["id"] != personID || personBody["name"] != "Denis Talent" || personBody["knownFor"] != "Directing" || personBody["biography"] != "Canadian filmmaker." || personBody["birthday"] != "1967-10-03" {
		t.Fatalf("person status=%d body=%v", code, personBody)
	}
	photo, ok := personBody["photo"].(map[string]any)
	if !ok || photo["kind"] != "person" {
		t.Fatalf("expected signed person photo, got %v", personBody["photo"])
	}
	photoURL := photo["sizes"].([]any)[0].(map[string]any)["url"].(string)
	u, err := url.Parse(photoURL)
	if err != nil {
		t.Fatal(err)
	}
	imgReq := httptest.NewRequest(http.MethodGet, u.String(), nil)
	imgW := httptest.NewRecorder()
	inst.serve(imgW, imgReq)
	if imgW.Code != 200 {
		t.Fatalf("signed person photo status=%d", imgW.Code)
	}

	// 3. GET /vela/v0/people/:id/titles across visible libraries: deduplicated, newest-first, paged, with role.
	code, page1 := doGET("/vela/v0/people/" + personID + "/titles?limit=2")
	if code != 200 || page1["continuation"] != "more" {
		t.Fatalf("titles page1 status=%d body=%v", code, page1)
	}
	items1 := page1["items"].([]any)
	if len(items1) != 2 {
		t.Fatalf("expected 2 items on page1, got %v", items1)
	}
	first := items1[0].(map[string]any)
	second := items1[1].(map[string]any)
	if first["id"] != ids.RatingKeyGUID(movieNew.Key) || first["role"] != "Director, Writer" {
		t.Fatalf("unexpected first filmography entry: %v", first)
	}
	if second["id"] != ids.RatingKeyGUID(showMid.Key) || second["role"] != "Lead" {
		t.Fatalf("unexpected second filmography entry: %v", second)
	}
	cursor := page1["nextCursor"].(string)
	code, page2 := doGET("/vela/v0/people/" + personID + "/titles?limit=2&cursor=" + url.QueryEscape(cursor))
	if code != 200 || page2["continuation"] != "exhausted" {
		t.Fatalf("titles page2 status=%d body=%v", code, page2)
	}
	items2 := page2["items"].([]any)
	if len(items2) != 1 || items2[0].(map[string]any)["id"] != ids.RatingKeyGUID(movieOld.Key) || items2[0].(map[string]any)["role"] != "Cameo, Director" {
		t.Fatalf("unexpected page2 items: %v", items2)
	}

	// 4. Scoped libraryId=23 queries only library 23; hidden library 404s; invisible person 404s.
	exp.itemCalls = nil
	code, scoped := doGET("/vela/v0/people/" + personID + "/titles?libraryId=23")
	if code != 200 || len(scoped["items"].([]any)) != 1 || len(exp.itemCalls) != 1 || exp.itemCalls[0] != "23" {
		t.Fatalf("libraryId=23 failed: code=%d calls=%v body=%v", code, exp.itemCalls, scoped)
	}
	if code, _ := doGET("/vela/v0/people/" + personID + "/titles?libraryId=hidden"); code != 404 {
		t.Fatalf("hidden library status=%d, want 404", code)
	}
	exp.visiblePerson = false
	if code, _ := doGET("/vela/v0/people/" + personID); code != 404 {
		t.Fatalf("invisible person status=%d, want 404", code)
	}
	if code, _ := doGET("/vela/v0/people/" + personID + "/titles"); code != 404 {
		t.Fatalf("invisible person titles status=%d, want 404", code)
	}
}

type libraryRowsTestSource struct {
	frontendtest.Experience
	requests []frontend.RowViewRequest
}

func (s *libraryRowsTestSource) ListRowViews(_ context.Context, req frontend.RowViewRequest) ([]frontend.RowView, error) {
	s.requests = append(s.requests, req)
	return []frontend.RowView{
		{
			ID:      "lib-row-" + req.ViewID,
			Title:   "Popular in Library",
			Purpose: "popular",
			Kind:    "taste",
			Layout:  frontend.RowLayout{Style: "shelf"},
		},
	}, nil
}

func (s *libraryRowsTestSource) ResolveRowView(_ context.Context, req frontend.RowViewPageRequest) (frontend.RowViewPage, error) {
	all := []frontend.RowEntry{
		{Kind: frontend.RowEntryTitle, Title: &frontend.Title{Key: core.MovieKey(101), Title: "Blade Runner 2049"}, Handle: "rcpt-1"},
		{Kind: frontend.RowEntryTitle, Title: &frontend.Title{Key: core.MovieKey(102), Title: "Dune"}, Handle: "rcpt-2"},
	}
	start := min(req.Offset, len(all))
	end := min(start+req.Limit, len(all))
	return frontend.RowViewPage{
		Entries:          all[start:end],
		ImpressionHandle: "imp-lib",
		More:             end < len(all),
	}, nil
}

func TestVelaLibraryRowsWithLibraryID(t *testing.T) {
	source := &libraryRowsTestSource{}
	source.ViewsFunc = func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
		return []frontend.View{
			{ID: "23", Name: "Sci-Fi", Type: frontend.TypeMovie, Revision: "v1"},
			{ID: "71", Name: "Plugin", Type: frontend.TypeMovie, PluginID: "plugin", KindKey: "clip"},
		}, nil
	}
	seal, err := encryption(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	inst := &instance{
		seal:    seal,
		mux:     http.NewServeMux(),
		rowMeta: map[string]frontend.RowView{},
		host:    frontend.Host{Identity: identityFake{}, Experience: source},
	}
	inst.routes()

	doGET := func(path string) (int, object) {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("X-Vela-Device-Id", "device")
		w := httptest.NewRecorder()
		inst.serve(w, r)
		var out object
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s decode: %v body=%s", path, err, w.Body.String())
		}
		return w.Code, out
	}

	code, body := doGET("/vela/v0/rows?placement=library&libraryId=23")
	if code != 200 || len(source.requests) != 1 || source.requests[0].Placement != frontend.PlacementLibrary || source.requests[0].ViewID != "23" {
		t.Fatalf("library rows failed: code=%d reqs=%+v body=%v", code, source.requests, body)
	}
	rows := body["items"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["id"] != "lib-row-23" {
		t.Fatalf("unexpected rows: %v", rows)
	}

	// Plugin library returns empty rows without calling ListRowViews.
	before := len(source.requests)
	code, pluginBody := doGET("/vela/v0/rows?placement=library&libraryId=71")
	if code != 200 || len(pluginBody["items"].([]any)) != 0 || len(source.requests) != before {
		t.Fatalf("plugin library rows failed: code=%d body=%v", code, pluginBody)
	}

	// Invisible library returns 404; libraryId on non-library placement returns 400.
	if code, _ := doGET("/vela/v0/rows?placement=library&libraryId=hidden"); code != 404 {
		t.Fatalf("hidden library rows status=%d, want 404", code)
	}
	if code, _ := doGET("/vela/v0/rows?placement=home&libraryId=23"); code != 400 {
		t.Fatalf("home placement with libraryId status=%d, want 400", code)
	}
	if code, _ := doGET("/vela/v0/rows?placement=library&libraryId=23&libraryId=23"); code != 400 {
		t.Fatalf("duplicate libraryId status=%d, want 400", code)
	}
}

type collectionSignerFake struct {
	frontend.ImageGrants
}

func (collectionSignerFake) SignPage(_ context.Context, req frontend.ImagePageRequest) ([]frontend.ImageBatchItem, error) {
	out := make([]frontend.ImageBatchItem, 0, len(req.Titles))
	for _, t := range req.Titles {
		if t.Key == core.CollectionKey(10) {
			out = append(out, frontend.ImageBatchItem{
				Item: t.Key,
				Images: []frontend.GrantedImage{{
					Image:  frontend.Image{Kind: frontend.ImagePoster, Path: "col-10-poster", Tag: "v1", Width: 1000, Height: 1500},
					Secret: "signed-col-10",
				}},
			})
		}
	}
	return out, nil
}

func TestVelaLibraryCollectionsAndBrowseFilter(t *testing.T) {
	views := []frontend.View{
		{ID: "23", Name: "Sci-Fi", Type: frontend.TypeMovie, Revision: "rev-1"},
		{ID: "71", Name: "Plugin", Type: frontend.TypeMovie, PluginID: "plugin", KindKey: "clip"},
	}
	col1 := core.Collection{Key: core.CollectionKey(10), Type: core.TypeMovie, Title: "Dune Saga", Summary: "Arrakis.", ChildCount: 2}
	col2 := core.Collection{Key: core.SmartKey(20), Type: core.TypeMovie, Title: "90s Classics", Summary: "Smart.", ChildCount: 5}

	var gotCollectionKey core.RatingKey
	exp := frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return views, nil
		},
		CollectionsFunc: func(_ context.Context, _ *frontend.Viewer, _ core.Device, q frontend.CollectionQuery) (frontend.CollectionPage, error) {
			if q.ViewID != "23" || len(q.Types) != 1 || q.Types[0] != core.TypeMovie {
				t.Fatalf("unexpected collection query: %+v", q)
			}
			if q.Offset == 1 && q.After != col1.Key {
				t.Fatalf("collection continuation lost its seek key: %v", q.After)
			}
			all := []core.Collection{col1, col2}
			start := min(q.Offset, len(all))
			end := min(start+q.Limit, len(all))
			return frontend.CollectionPage{Items: all[start:end], Total: len(all)}, nil
		},
		LibraryItemsFunc: func(_ context.Context, _ *frontend.Viewer, _ core.Device, id string, q frontend.ItemQuery) (frontend.Page, error) {
			if id != "23" {
				t.Fatalf("unexpected library %s", id)
			}
			gotCollectionKey = q.CollectionKey
			return frontend.Page{Items: []frontend.Title{{Key: core.MovieKey(1), Title: "Dune"}}}, nil
		},
	}

	seal, err := encryption(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	inst := &instance{
		seal: seal,
		mux:  http.NewServeMux(),
		host: frontend.Host{Identity: identityFake{}, Experience: exp, ImageGrants: collectionSignerFake{}},
	}
	inst.routes()

	doGET := func(path string) (int, object) {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("X-Vela-Device-Id", "device")
		w := httptest.NewRecorder()
		inst.serve(w, r)
		var out object
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s decode: %v body=%s", path, err, w.Body.String())
		}
		return w.Code, out
	}

	// 1. Native library advertises collectionId filter; plugin library does not.
	code, libList := doGET("/vela/v0/libraries")
	if code != 200 {
		t.Fatalf("libraries status=%d", code)
	}
	libs := libList["items"].([]any)
	nativeFilters := libs[0].(map[string]any)["browse"].(map[string]any)["filters"].(map[string]any)
	pluginFilters := libs[1].(map[string]any)["browse"].(map[string]any)["filters"].(map[string]any)
	if nativeFilters["collectionId"] != true || pluginFilters["collectionId"] != nil {
		t.Fatalf("unexpected collectionId filter advertisement: native=%v plugin=%v", nativeFilters, pluginFilters)
	}

	// 2. GET /vela/v0/libraries/:id/collections pages collection RowEntries with signed artwork and itemCount.
	code, colPage1 := doGET("/vela/v0/libraries/23/collections?limit=1")
	if code != 200 || colPage1["continuation"] != "more" {
		t.Fatalf("collections page1 status=%d body=%v", code, colPage1)
	}
	entry1 := colPage1["items"].([]any)[0].(map[string]any)
	colWire1 := entry1["collection"].(map[string]any)
	if entry1["kind"] != "collection" || entry1["id"] != ids.RatingKeyGUID(col1.Key) || colWire1["name"] != "Dune Saga" || colWire1["itemCount"] != float64(2) {
		t.Fatalf("unexpected collection entry: %v", entry1)
	}
	art := colWire1["artwork"].([]any)
	if len(art) != 1 || art[0].(map[string]any)["kind"] != "poster" {
		t.Fatalf("expected signed poster artwork on collection, got %v", art)
	}
	nextCursor := colPage1["nextCursor"].(string)
	code, colPage2 := doGET("/vela/v0/libraries/23/collections?limit=1&cursor=" + url.QueryEscape(nextCursor))
	if code != 200 || colPage2["continuation"] != "exhausted" {
		t.Fatalf("collections page2 status=%d body=%v", code, colPage2)
	}
	entry2 := colPage2["items"].([]any)[0].(map[string]any)
	if entry2["id"] != ids.RatingKeyGUID(col2.Key) {
		t.Fatalf("unexpected second collection entry: %v", entry2)
	}

	// Snapshot change invalidates cursor; hidden library 404s; plugin library returns empty page.
	views[0].Revision = "rev-2"
	if code, _ := doGET("/vela/v0/libraries/23/collections?limit=1&cursor=" + url.QueryEscape(nextCursor)); code != 409 {
		t.Fatalf("stale cursor status=%d, want 409", code)
	}
	if code, _ := doGET("/vela/v0/libraries/hidden/collections"); code != 404 {
		t.Fatalf("hidden library collections status=%d, want 404", code)
	}
	if code, emptyPlugin := doGET("/vela/v0/libraries/71/collections"); code != 200 || len(emptyPlugin["items"].([]any)) != 0 {
		t.Fatalf("plugin collections status=%d body=%v", code, emptyPlugin)
	}

	// 3. Browse collection titles through GET /vela/v0/libraries/:id/items?collectionId=<guid>.
	for _, key := range []core.RatingKey{col1.Key, col2.Key} {
		gotCollectionKey = 0
		code, browse := doGET("/vela/v0/libraries/23/items?collectionId=" + ids.RatingKeyGUID(key))
		if code != 200 || gotCollectionKey != key || len(browse["items"].([]any)) != 1 {
			t.Fatalf("browse collection %v failed: code=%d gotKey=%v body=%v", key, code, gotCollectionKey, browse)
		}
	}
	// Non-collection GUID and plugin library collectionId return 400.
	if code, _ := doGET("/vela/v0/libraries/23/items?collectionId=" + ids.RatingKeyGUID(core.MovieKey(1))); code != 400 {
		t.Fatalf("movie GUID in collectionId status=%d, want 400", code)
	}
	if code, _ := doGET("/vela/v0/libraries/71/items?collectionId=" + ids.RatingKeyGUID(col1.Key)); code != 400 {
		t.Fatalf("plugin library collectionId status=%d, want 400", code)
	}
}
