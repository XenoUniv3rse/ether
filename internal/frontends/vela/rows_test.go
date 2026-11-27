package vela

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type rowSizeSource struct {
	frontendtest.Experience
	pages    []frontend.RowViewPageRequest
	distinct bool
}

func (f *rowSizeSource) ListRowViews(context.Context, frontend.RowViewRequest) ([]frontend.RowView, error) {
	rows := make([]frontend.RowView, 10)
	for n := range rows {
		rows[n] = frontend.RowView{ID: fmt.Sprint(n), Title: "Shelf", Kind: "taste", Layout: frontend.RowLayout{Style: "shelf", PreviewOnFocus: true}}
	}
	return rows, nil
}
func (f *rowSizeSource) ResolveRowView(_ context.Context, q frontend.RowViewPageRequest) (frontend.RowViewPage, error) {
	f.pages = append(f.pages, q)
	base := 0
	if f.distinct {
		row, _ := strconv.Atoi(q.ID)
		base = row * 100
	}
	entries := make([]frontend.RowEntry, min(q.Limit, 100-q.Offset))
	for n := range entries {
		entries[n] = frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &frontend.Title{Key: frontend.MovieKey(int64(base + q.Offset + n + 1)), Title: "Movie"}, Handle: fmt.Sprintf("receipt-%d", q.Offset+n)}
		if q.Offset+n == 0 {
			entries[n].Badge = "Not yet available"
		}
	}
	return frontend.RowViewPage{Entries: entries, ImpressionHandle: "row-receipt", More: q.Offset+len(entries) < 100}, nil
}

type rowSizeImages struct {
	frontend.ImageGrants
	batches [][]frontend.RatingKey
}

func (f *rowSizeImages) SignPage(_ context.Context, q frontend.ImagePageRequest) ([]frontend.ImageBatchItem, error) {
	keys := make([]frontend.RatingKey, len(q.Titles))
	for n, title := range q.Titles {
		keys[n] = title.Key
	}
	f.batches = append(f.batches, keys)
	return nil, nil
}

// Paging must keep the issued source offset even when hero-family exclusions
// require backfill. Card conversion batches distinct titles across all shelves
// in the response; each shelf retains its own receipts and source cursor.
func TestVelaRowSizeAndContinuation(t *testing.T) {
	source := &rowSizeSource{}
	states := [][]frontend.RatingKey{}
	source.UserDataFunc = func(_ context.Context, _ *frontend.Viewer, keys []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
		states = append(states, append([]frontend.RatingKey(nil), keys...))
		return nil, nil
	}
	images := &rowSizeImages{}
	seal, err := encryption(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	i := &instance{seal: seal, host: frontend.Host{Experience: source, ImageGrants: images}, rowMeta: map[string]frontend.RowView{}}
	viewer := &frontend.Viewer{ID: 1}
	request := func(path string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Vela-Device-Id", "device")
		return r
	}
	for _, distinct := range []bool{false, true} {
		source.distinct = distinct
		for _, path := range []string{"/vela/v0/bootstrap", "/vela/v0/rows", "/vela/v0/rows?limit=2"} {
			source.pages = nil
			states = nil
			images.batches = nil
			r := request(path)
			rows, _, err := i.home(r, viewer, "token")
			wantRows := 10
			if r.URL.Query().Get("limit") == "2" {
				wantRows = 2
			}
			if err != nil || len(rows) != wantRows {
				t.Fatalf("%s distinct=%t: rows=%d err=%v", path, distinct, len(rows), err)
			}
			// Repeated titles cost one conversion regardless of shelf count. Disjoint
			// shelves must include every title in the shared state/artwork batch.
			var wantKeys []frontend.RatingKey
			for row := 0; row < wantRows; row++ {
				if !distinct && row > 0 {
					break
				}
				base := 0
				if distinct {
					base = row * 100
				}
				for n := 1; n <= 20; n++ {
					wantKeys = append(wantKeys, frontend.MovieKey(int64(base+n)))
				}
			}
			if len(source.pages) != wantRows || len(states) != 1 || len(images.batches) != 1 {
				t.Fatalf("%s distinct=%t: expected one source page per shelf and one shared conversion, pages=%d state batches=%d image batches=%d", path, distinct, len(source.pages), len(states), len(images.batches))
			}
			if !slices.Equal(states[0], wantKeys) || !slices.Equal(images.batches[0], wantKeys) {
				t.Fatalf("%s distinct=%t: shared batches lost or duplicated titles: state=%v images=%v want=%v", path, distinct, states[0], images.batches[0], wantKeys)
			}
			for n, row := range rows {
				items := row["items"].([]object)
				if source.pages[n].Limit != 20 || len(items) != 20 {
					t.Fatal("wrong per-row page size", row)
				}
				if items[0]["badge"] != "Not yet available" || items[1]["badge"] != nil {
					t.Fatal("row card badges did not stay with their entries")
				}
				base := 0
				if distinct {
					base = n * 100
				}
				for offset, item := range items {
					title := item["title"].(object)
					if title["id"] != ids.RatingKeyGUID(frontend.MovieKey(int64(base+offset+1))) || item["receiptToken"] != fmt.Sprintf("receipt-%d", offset) {
						t.Fatal("shared conversion changed row title or receipt", item)
					}
				}
				if row["receiptToken"] != "row-receipt" || row["style"] != nil || row["role"] != "movie" || row["previewOnFocus"] != true {
					t.Fatal("row metadata lost", row)
				}
				cursor, err := i.readRowToken(r, viewer, row["nextCursor"].(string))
				if err != nil || cursor.Offset != 20 {
					t.Fatal("wrong source offset", cursor, err)
				}
			}
		}
	}
	source.distinct = false

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", 20}, {"?limit=7", 7}, {"?limit=50", 50}, {"?limit=999", 50}, {"?limit=0", 20}, {"?limit=-1", 20}, {"?limit=invalid", 20},
	} {
		r := request("/vela/v0/rows/0" + tc.query)
		r.SetPathValue("id", "0")
		rows, _, err := i.home(r, viewer, "token")
		if err != nil || len(rows) != 1 || len(rows[0]["items"].([]object)) != tc.want {
			t.Fatalf("%s: %v %v", tc.query, rows, err)
		}
		cursor, err := i.readRowToken(r, viewer, rows[0]["nextCursor"].(string))
		if err != nil {
			t.Fatal(err)
		}
		// Simulate the bootstrap's hero exclusion on a subsequent page.
		cursor.Exclude = []string{ids.RatingKeyGUID(frontend.MovieKey(int64(tc.want + 1)))}
		next := request("/vela/v0/rows/0?limit=7&cursor=" + url.QueryEscape(i.sealRowCursor(cursor)))
		next.SetPathValue("id", "0")
		rows, _, err = i.home(next, viewer, "token")
		if err != nil || len(rows[0]["items"].([]object)) != 7 {
			t.Fatal("continuation exclusion lost", rows, err)
		}
		page := source.pages[len(source.pages)-2]
		if page.Offset != tc.want || page.Limit != 7 {
			t.Fatal("continuation page changed", page)
		}
		items := rows[0]["items"].([]object)
		if items[0]["receiptToken"] != fmt.Sprintf("receipt-%d", tc.want+1) {
			t.Fatal("receipt changed", items[0])
		}
		cursor, err = i.readRowToken(next, viewer, rows[0]["nextCursor"].(string))
		if err != nil || cursor.Offset != tc.want+8 || len(cursor.Exclude) != 1 {
			t.Fatal("filtered page changed source cursor", cursor, err)
		}
	}
}

// The offset counts consumed source entries, including excluded heroes, across
// bootstrap backfill and all continuation pages through honest exhaustion.
func TestVelaRowFeaturedBackfill(t *testing.T) {
	source := &rowSizeSource{}
	seal, err := encryption(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	i := &instance{seal: seal, host: frontend.Host{Experience: source}, rowMeta: map[string]frontend.RowView{"0": {ID: "0", Title: "Shelf"}}}
	viewer := &frontend.Viewer{ID: 1}
	r := httptest.NewRequest(http.MethodGet, "/vela/v0/bootstrap", nil)
	r.Header.Set("X-Vela-Device-Id", "device")
	excluded := []string{}
	for _, n := range []int64{2, 8, 19} {
		excluded = append(excluded, ids.RatingKeyGUID(frontend.MovieKey(n)))
	}
	row, err := i.rowPage(r, viewer, "0", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	shelf, err := i.rowWire(r, viewer, "token", row, 0, excluded, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(shelf["items"].([]object)) != 17 {
		t.Fatal("fixture did not exclude three featured titles")
	}
	if err := i.backfillShelf(r, viewer, "token", shelf, 20); err != nil {
		t.Fatal(err)
	}
	if len(source.pages) != 2 || source.pages[1].Offset != 20 || source.pages[1].Limit != 3 {
		t.Fatal("backfill fetched beyond the missing slots", source.pages)
	}
	seen := map[string]bool{}
	for page := 0; ; page++ {
		items := shelf["items"].([]object)
		want := 20
		if page == 4 {
			want = 17
		}
		if len(items) != want || shelf["receiptToken"] != "row-receipt" {
			t.Fatalf("page %d: count=%d receipt=%v", page, len(items), shelf["receiptToken"])
		}
		for _, entry := range items {
			id := entry["title"].(object)["id"].(string)
			if seen[id] {
				t.Fatal("duplicate across pages", id)
			}
			for _, hero := range excluded {
				if hero == id {
					t.Fatal("featured title leaked", id)
				}
			}
			seen[id] = true
			key, err := ids.ParseRatingKeyGUID(id)
			if err != nil || entry["receiptToken"] != fmt.Sprintf("receipt-%d", key.TMDB()-1) {
				t.Fatal("entry receipt changed", entry, err)
			}
		}
		if shelf["continuation"] == "exhausted" {
			if len(seen) != 97 || shelf["nextCursor"] != nil {
				t.Fatal("dishonest exhaustion", len(seen), shelf)
			}
			break
		}
		if page >= 4 {
			t.Fatal("source did not exhaust")
		}
		cursor, err := i.readRowToken(r, viewer, shelf["nextCursor"].(string))
		if err != nil || cursor.Offset != 23+page*20 || len(cursor.Exclude) != 3 {
			t.Fatal("wrong consumed source offset or exclusion proof", cursor, err)
		}
		next := httptest.NewRequest(http.MethodGet, "/vela/v0/rows/0?cursor="+url.QueryEscape(shelf["nextCursor"].(string)), nil)
		next.Header = r.Header.Clone()
		next.SetPathValue("id", "0")
		rows, _, err := i.home(next, viewer, "token")
		if err != nil || len(rows) != 1 {
			t.Fatal("continuation failed", rows, err)
		}
		shelf = rows[0]
	}
}

// remintSource re-mints every row view id per generation, as the experience
// does when the feed is rebuilt, and forgets the old snapshots.
type remintSource struct {
	frontendtest.Experience
	generation int
}

func (f *remintSource) ListRowViews(context.Context, frontend.RowViewRequest) ([]frontend.RowView, error) {
	present := []core.RowPresentation{{}, {RowID: "rLib", Library: 12}, {RowID: "rBecause"}, {RowID: "rBecause"}}
	keys := []string{"home.continue", "movies.trending", "home.because.1", "home.because.2"}
	rows := make([]frontend.RowView, len(keys))
	for n := range rows {
		rows[n] = frontend.RowView{ID: fmt.Sprintf("g%d-%d", f.generation, n), Key: keys[n], Title: "Shelf", Kind: "taste", Layout: frontend.RowLayout{Present: present[n]}}
	}
	return rows, nil
}

func (f *remintSource) ResolveRowView(_ context.Context, q frontend.RowViewPageRequest) (frontend.RowViewPage, error) {
	if !strings.HasPrefix(q.ID, fmt.Sprintf("g%d-", f.generation)) {
		return frontend.RowViewPage{}, frontend.ErrNotFound
	}
	return frontend.RowViewPage{Entries: []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &frontend.Title{Key: frontend.MovieKey(1), Title: "Movie"}}}}, nil
}

// A recompute re-mints every row id; key stays, and the old id is the same
// 409 cursor_expired for every row kind.
func TestVelaRowKeyStableAcrossRemint(t *testing.T) {
	source := &remintSource{}
	seal, err := encryption(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	i := &instance{seal: seal, host: frontend.Host{Experience: source}, rowMeta: map[string]frontend.RowView{}}
	viewer := &frontend.Viewer{ID: 1}
	list := func() []object {
		r := httptest.NewRequest(http.MethodGet, "/vela/v0/rows", nil)
		r.Header.Set("X-Vela-Device-Id", "device")
		rows, _, err := i.home(r, viewer, "token")
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	byID := func(id string) ([]object, error) {
		r := httptest.NewRequest(http.MethodGet, "/vela/v0/rows/"+id, nil)
		r.Header.Set("X-Vela-Device-Id", "device")
		r.SetPathValue("id", id)
		rows, _, err := i.home(r, viewer, "token")
		return rows, err
	}
	want := []string{"home.continue", "rLib@12", "rBecause", "rBecause#2"}
	before := list()
	source.generation++
	after := list()
	for n := range want {
		if before[n]["key"] != want[n] || after[n]["key"] != want[n] || before[n]["id"] == after[n]["id"] {
			t.Fatalf("row %d: before=%v/%v after=%v/%v want key %s", n, before[n]["id"], before[n]["key"], after[n]["id"], after[n]["key"], want[n])
		}
		current, err := byID(after[n]["id"].(string))
		if err != nil || current[0]["key"] != want[n] {
			t.Fatalf("row %d: /rows/{id} key=%v err=%v", n, current, err)
		}
		if _, err := byID(before[n]["id"].(string)); !errors.Is(err, frontend.ErrRowViewStale) {
			t.Fatalf("row %d: superseded id answered %v, want cursor_expired", n, err)
		}
	}
	w := httptest.NewRecorder()
	fail(w, frontend.ErrRowViewStale)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "cursor_expired") {
		t.Fatal(w.Code, w.Body.String())
	}
}

type backfillBatchSource struct {
	rowSizeSource
	batches [][]frontend.RowViewPageRequest
}

func (s *backfillBatchSource) ResolveRowViews(ctx context.Context, reqs []frontend.RowViewPageRequest) ([]frontend.RowViewPage, error) {
	s.batches = append(s.batches, slices.Clone(reqs))
	pages := make([]frontend.RowViewPage, 0, len(reqs))
	for _, req := range reqs {
		page, err := s.ResolveRowView(ctx, req)
		if err != nil {
			return nil, err
		}
		pages = append(pages, page)
	}
	return pages, nil
}

func TestVelaFeaturedBackfillBatchesShelves(t *testing.T) {
	source := &backfillBatchSource{rowSizeSource: rowSizeSource{distinct: true}}
	seal, err := encryption(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	i := &instance{seal: seal, host: frontend.Host{Experience: source}}
	viewer := &frontend.Viewer{ID: 1}
	r := httptest.NewRequest(http.MethodGet, "/vela/v0/bootstrap", nil)
	r.Header.Set("X-Vela-Device-Id", "device")
	excluded := []string{ids.RatingKeyGUID(frontend.MovieKey(2)), ids.RatingKeyGUID(frontend.MovieKey(103))}
	shelves := make([]object, 0, 2)
	for _, id := range []string{"0", "1"} {
		row, err := i.rowPage(r, viewer, id, 0, defaultRowItemLimit)
		if err != nil {
			t.Fatal(err)
		}
		shelf, err := i.rowWire(r, viewer, "token", row, 0, excluded, true)
		if err != nil {
			t.Fatal(err)
		}
		shelves = append(shelves, shelf)
	}
	for _, err := range i.backfillShelves(r, viewer, "token", shelves, defaultRowItemLimit) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(source.batches) != 1 || len(source.batches[0]) != 2 {
		t.Fatalf("expected one batch for both shelves, got %v", source.batches)
	}
	for n, q := range source.batches[0] {
		if q.Offset != defaultRowItemLimit || q.Limit != 1 || q.ID != strconv.Itoa(n) {
			t.Fatalf("unexpected backfill request: %+v", q)
		}
		items := shelves[n]["items"].([]object)
		cursor, err := i.readRowToken(r, viewer, shelves[n]["nextCursor"].(string))
		if len(items) != defaultRowItemLimit || err != nil || cursor.Offset != defaultRowItemLimit+1 ||
			items[len(items)-1]["receiptToken"] != "receipt-20" {
			t.Fatalf("shelf %d lost its receipt or source offset: %v %v", n, cursor, err)
		}
	}
}
