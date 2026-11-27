package xtream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

// policyLibrary is a movie library whose export depends on a viewer policy
// (a cap on title numbers) and on catalogue content (removed titles).
type policyLibrary struct {
	syntheticExport
	mu      sync.Mutex
	stamp   string
	cap     int
	judged  bool
	removed map[int]bool
	catalog atomic.Uint64
}

func (p *policyLibrary) ExportLibraryPage(ctx context.Context, v *frontend.Viewer, d frontend.Device, view string, after frontend.RatingKey, limit int) (frontend.LibraryExportPage, error) {
	page, err := p.syntheticExport.ExportLibraryPage(ctx, v, d, view, after, limit)
	p.mu.Lock()
	defer p.mu.Unlock()
	items := page.Items[:0]
	for _, t := range page.Items {
		n := int(t.Key - core.MovieKey(0))
		if n <= p.cap && !p.removed[n] {
			items = append(items, t)
		}
	}
	page.Items = items
	return page, err
}

func (p *policyLibrary) ExportPolicy(context.Context, *frontend.Viewer, frontend.Device, string) (frontend.ExportPolicy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return frontend.ExportPolicy{Stamp: p.stamp, TitleJudged: p.judged}, nil
}

// set changes the library under its lock; a catalogue change also moves
// the catalogue generation, as a catalogue write does.
func (p *policyLibrary) set(fn func(*policyLibrary), catalogue bool) {
	p.mu.Lock()
	fn(p)
	p.mu.Unlock()
	if catalogue {
		p.catalog.Add(1)
	}
}

// Listings are kept per viewer policy: a policy change, or a catalogue
// write under a policy that judges titles, never serves what was listed
// before it. A catalogue write under a policy that does not judge titles
// is served stale within the bound, while the listing is rebuilt behind it.
func TestXtreamListingNeverServesStaleVisibility(t *testing.T) {
	for _, judged := range []bool{false, true} {
		t.Run(fmt.Sprint("judged=", judged), func(t *testing.T) {
			host, _ := testHost(t)
			lib := &policyLibrary{stamp: "a", cap: 600, judged: judged, removed: map[int]bool{}}
			lib.size = 600
			lib.ViewsFunc = func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
				return []frontend.View{{ID: "1", Type: core.TypeMovie, Name: "Movies"}}, nil
			}
			host.Experience = lib
			host.PageFreshness = func() frontend.PageFreshness { return frontend.PageFreshness{Catalog: lib.catalog.Load()} }
			streamed := mount(t, host)
			host.CacheDir = t.TempDir()
			inst, err := (Frontend{}).Mount(context.Background(), host)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = inst.Close(context.Background()) })
			h := inst.Handler()
			settle := func() {
				t.Helper()
				for deadline := time.Now().Add(10 * time.Second); inst.(*instance).BackgroundBusy() > 0; time.Sleep(time.Millisecond) {
					if time.Now().After(deadline) {
						t.Fatal("listing build did not finish")
					}
				}
			}
			list := func(h0 *httptest.ResponseRecorder) string {
				t.Helper()
				if h0.Code != 200 {
					t.Fatalf("%d %s", h0.Code, h0.Body.String())
				}
				return h0.Body.String()
			}
			vod := "/player_api.php?" + authQuery() + "&action=get_vod_streams"
			request(h, "/player_api.php?"+authQuery())
			settle()
			if lib.calls != 0 {
				t.Fatal("sign-in prepared a listing")
			}
			// A listing is built on first use. Its cached representation
			// matches both the first response and the uncached encoder.
			body := list(request(h, vod))
			settle()
			if body != list(request(h, vod)) || body != list(request(streamed, vod)) {
				t.Fatal("cached listing differs from encoding")
			}
			has := func(body string, n int) bool { return strings.Contains(body, fmt.Sprintf(`"name":"Film %d"`, n-1)) }
			if !has(body, 600) {
				t.Fatal("listing lost a visible title")
			}
			// A policy change: the next listing reads the new policy.
			lib.set(func(p *policyLibrary) { p.stamp, p.cap = "b", 400 }, false)
			if body = list(request(h, vod)); has(body, 450) || !has(body, 400) {
				t.Fatal("served a listing built under another policy")
			}
			settle()
			// A catalogue write removes a listed title.
			lib.set(func(p *policyLibrary) { p.removed[10] = true }, true)
			body = list(request(h, vod))
			if judged && has(body, 10) {
				t.Fatal("a judged policy served a listing read before a catalogue write")
			}
			if !judged && !has(body, 10) {
				t.Fatal("an unjudged listing was not served within its staleness bound")
			}
			settle()
			if body = list(request(h, vod)); has(body, 10) || has(body, 450) {
				t.Fatal("the rebuilt listing is stale")
			}
			if body != list(request(streamed, vod)) {
				t.Fatal("cached listing differs from encoding")
			}
		})
	}
}

func TestM3ULineupGolden(t *testing.T) {
	host, _ := testHost(t)
	movie := frontend.Title{Key: core.MovieKey(12), Type: core.TypeMovie, Title: "Film\"Cut"}
	show := frontend.Title{Key: core.ShowKey(13), Type: core.TypeShow, Title: "Series"}
	season := frontend.Title{Key: core.SeasonKey(13, 2), Type: core.TypeSeason}
	episode := frontend.Title{Key: core.EpisodeKey(13, 2, 3), Type: core.TypeEpisode, Title: "First\nPart", Season: 2, Episode: 3}
	host.Experience = frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "1", Type: core.TypeMovie}, {ID: "2", Type: core.TypeShow}}, nil
		},
		LibraryItemsFunc: func(_ context.Context, _ *core.Viewer, _ core.Device, view string, _ frontend.ItemQuery) (core.Page, error) {
			item := movie
			if view == "2" {
				item = show
			}
			return core.Page{Items: []core.Title{item}, Total: 1}, nil
		},
		ChildrenFunc: func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey, _ frontend.ItemQuery) (core.Page, error) {
			item := season
			if key == season.Key {
				item = episode
			}
			return core.Page{Items: []core.Title{item}, Total: 1}, nil
		},
	}
	response := request(mount(t, host), "/get.php?"+authQuery()+"&type=m3u_plus&output=ts")
	const golden = `#EXTM3U
#EXTINF:-1 tvg-id="allowed" tvg-logo="" group-title="Live TV",News
http://ether.test/xtream/live/USER/eg1_test/1.ts
#EXTINF:-1 group-title="Movies",Film'Cut
http://ether.test/xtream/movie/USER/eg1_test/1.mkv
#EXTINF:-1 group-title="Series",Series S02E03 First Part
http://ether.test/xtream/series/USER/eg1_test/2.mkv
`
	want := strings.ReplaceAll(golden, "USER", frontend.XtreamUsername("eg1_test"))
	if response.Code != http.StatusOK || response.Body.String() != want {
		t.Fatalf("got HTTP %d and lineup:\n%s\nwant:\n%s", response.Code, response.Body, want)
	}
}
