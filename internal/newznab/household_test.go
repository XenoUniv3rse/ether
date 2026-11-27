package newznab

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Replay at the common indexer boundary so HTTP counters include ID fallback,
// page reads and vault reuse without depending on client UI or polling delays.
func TestHouseholdIndexerBudget(t *testing.T) {
	type tally struct{ searches, listings, grabs, caps int }
	var mu sync.Mutex
	phase := ""
	counts := map[string]tally{}
	bySource := map[string]int{}
	wrongRoute := false
	unplannedListing := false
	quotaCalls := 0
	sourceID := regexp.MustCompile(`^/([1-4])/`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		match := sourceID.FindStringSubmatch(r.URL.Path)
		if match == nil {
			http.NotFound(w, r)
			return
		}
		id := match[1]
		q := r.URL.Query()
		query := q.Get("q")
		targeted := query != "" || q.Get("imdbid") != "" || q.Get("tmdbid") != "" || q.Get("tvdbid") != ""
		mu.Lock()
		label := phase
		c := counts[label]
		bySource[id]++
		switch q.Get("t") {
		case "caps":
			c.caps++
		case "get":
			c.grabs++
		default:
			if targeted {
				c.searches++
			} else {
				c.listings++
				unplannedListing = unplannedListing || (label != "playable-title feed" && label != "plugin recent listings")
			}
			if id == "3" && (!strings.Contains(query, "Anime") || q.Get("cat") != "5070") {
				wrongRoute = true
			}
			if id == "4" && label == "plugin kind (music)" {
				wrongRoute = true
			}
		}
		if strings.HasPrefix(label, "quota") {
			quotaCalls++
		}
		counts[label] = c
		mu.Unlock()

		switch q.Get("t") {
		case "caps":
			categories := `<category id="2000"/><category id="3000"/><category id="5000"><subcat id="5040"/></category>`
			if id == "3" {
				categories = `<category id="5000"><subcat id="5070"/></category>`
			} else if id == "4" {
				categories = `<category id="2000"/><category id="5000"><subcat id="5040"/></category>`
			}
			fmt.Fprintf(w, `<caps><categories>%s</categories></caps>`, categories)
		case "get":
			fmt.Fprint(w, fakeNZB)
		default:
			if label == "quota HTTP refusal" {
				w.Header().Set("Retry-After", "1800")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if label == "quota document refusal" {
				fmt.Fprint(w, `<error code="201" description="Indexer Grab Limit of 80 in last 1 hour(s) reached."/>`)
				return
			}
			fmt.Fprint(w, `<rss xmlns:newznab="urn:newznab"><channel>`)
			if id == "2" {
				fmt.Fprint(w, `<newznab:apilimits apimax="36" grabmax="15"/>`)
			}
			// A modest share of obscure plays need fallback or rescue.
			if q.Get("imdbid") == "9000001" ||
				(strings.Contains(query, "Obscure") && strings.Contains(query, "S01E01")) {
				fmt.Fprint(w, `</channel></rss>`)
				return
			}
			guid := id + "-" + query + q.Get("imdbid")
			if !targeted {
				guid = id + "-head"
			}
			fmt.Fprintf(w, `<item><title>Household release</title><guid>%s</guid><link>http://%s/%s/api?t=get&amp;id=%s</link></item></channel></rss>`, guid, r.Host, id, url.QueryEscape(guid))
		}
	}))
	t.Cleanup(server.Close)

	db := store.OpenTest(t)
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	var indexers []config.Indexer
	for n := 1; n <= 4; n++ {
		idx := testConfig(fmt.Sprintf("%s/%d", server.URL, n))
		idx.Name, idx.ID, idx.Priority = fmt.Sprintf("Replay source %d", n), strconv.Itoa(n), n
		indexers = append(indexers, idx)
	}
	indexers[0].DailyLimit = 300
	indexers[2].DailyLimit = 100
	indexers[3].Protocol = "torrent"
	options := Options{Now: func() time.Time { return now }, Vault: &testVault{}}
	m := NewManager(db, indexers, options)
	t.Cleanup(m.Close)
	ctx := t.Context()
	setPhase := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		phase = name
	}
	request := func(name, purpose string, req core.SearchRequest, grab, rescue bool) {
		setPhase(name)
		call := core.WithIndexerPurpose(ctx, purpose)
		if purpose == core.IndexerPurposeWarmup {
			call = core.WithUnattendedSearch(call)
		}
		if name == "next-episode prep" || name == "managed cache refill" {
			call = core.WithSpeculativeGrabs(call)
		}
		rs, _ := m.Search(call, req)
		if rescue && len(rs) == 0 {
			rs, _ = m.SearchRescue(call, req)
		}
		for _, r := range rs {
			if grab && r.Protocol != core.ProtocolTorrent {
				_, _ = m.FetchNZB(call, r)
				break
			}
		}
	}
	movie := func(i int) core.SearchRequest {
		return core.SearchRequest{Type: core.TypeMovie, Title: fmt.Sprintf("Movie %d", i), Year: 2020, Released: now.AddDate(-3, 0, 0)}
	}
	episode := func(i int) core.SearchRequest {
		return core.SearchRequest{Type: core.TypeEpisode, Title: "Evening show", Season: 1, Episode: i, Released: now.Add(-48 * time.Hour)}
	}

	// Twelve two-hour periods, two generic Usenet sources, one anime-only
	// source and one video-only Torznab source. Each potential polling cycle
	// also asks for up to two pages to model a busy feed / catch-up.
	for block := 0; block < 12; block++ {
		now = time.Date(2026, 1, 15, 2*block, 0, 0, 0, time.UTC)
		setPhase("playable-title feed")
		for _, idx := range indexers {
			for page := 0; page < 2; page++ {
				_, _ = m.Recent(ctx, idx.ID, page*100, 100)
			}
		}
		request("playable-title targeted check", core.IndexerPurposeAvailability, movie(block+100), false, false)
		request("detail warm", core.IndexerPurposeDetail, movie(block%4), true, false)
		request("detail warm", core.IndexerPurposeDetail, episode(block%4+1), true, false)
		if block%3 == 0 {
			request("watchlist addition", core.IndexerPurposeWarmup, movie(block+20), true, false)
			request("prewarm / cache refill", core.IndexerPurposeWarmup, episode(block+11), true, false)
			request("managed cache refill", core.IndexerPurposeWarmup, movie(block+60), true, false)
			request("plugin kind (music)", core.IndexerPurposePlayback, core.SearchRequest{Title: fmt.Sprintf("Album %d", block), Category: "music"}, true, false)
			request("plugin recent listings", core.IndexerPurposePlayback, core.SearchRequest{Category: "tv"}, false, false)
			request("Live TV readiness", core.IndexerPurposeAvailability, episode(block+50), true, false)
		}
		if block >= 8 {
			req := movie(block % 2)
			if block == 8 {
				req.IDs.IMDB = "tt9000001"
			}
			request("playback", core.IndexerPurposePlayback, req, true, true)
			request("playback", core.IndexerPurposePlayback, episode(block-7), true, true)
			request("next-episode prep", core.IndexerPurposeWarmup, episode(block-6), true, false)
			request("availability health check", core.IndexerPurposeAvailability, movie(block+100), true, false)
		}
		if block == 9 {
			request("playback (anime)", core.IndexerPurposePlayback, core.SearchRequest{Type: core.TypeEpisode, Title: "Anime evening show", Anime: true, Season: 1, Episode: 2}, true, true)
			request("playback (obscure episode)", core.IndexerPurposePlayback, core.SearchRequest{Type: core.TypeEpisode, Title: "Obscure show", Season: 1, Episode: 1}, true, true)
		}
	}
	mu.Lock()
	for label, n := range counts {
		t.Logf("%s: searches=%d listings=%d grabs=%d caps=%d", label, n.searches, n.listings, n.grabs, n.caps)
	}
	t.Logf("requests by source: %v", bySource)
	if bySource["1"] > 300 || bySource["2"] > 36+15 || bySource["3"] > 100 ||
		counts["playable-title feed"].listings+counts["plugin recent listings"].listings > 24*3 {
		t.Error("at least one source exceeded its daily allowance")
	}
	if wrongRoute || unplannedListing || counts["playback"].grabs == 0 || counts["playback (anime)"].searches == 0 {
		t.Errorf("unexpected routing, query-less traffic or missing playback: wrong route=%v unexpected listing=%v", wrongRoute, unplannedListing)
	}
	if counts["detail warm"].grabs > 0 || counts["availability health check"].grabs > 0 {
		t.Error("checks spent on speculative NZB grabs")
	}
	mu.Unlock()

	// Recreate the fleet to verify a persisted service pause is observed by
	// a fresh manager, independently of the local day budget.
	only := New(indexers[0], db, options)
	for _, refusal := range []struct {
		label string
		pause time.Duration
	}{{"quota HTTP refusal", 30 * time.Minute}, {"quota document refusal", time.Hour}} {
		setPhase(refusal.label)
		if _, err := only.Search(ctx, movie(801)); !errors.Is(err, core.ErrBudgetExhausted) {
			t.Errorf("%s: expected service quota refusal, got %v", refusal.label, err)
		}
		reopened := New(indexers[0], db, options)
		setPhase("quota paused")
		mu.Lock()
		before := quotaCalls
		mu.Unlock()
		_, err := reopened.Search(ctx, movie(802))
		mu.Lock()
		after := quotaCalls
		mu.Unlock()
		if !errors.Is(err, core.ErrBudgetExhausted) || after != before {
			t.Errorf("%s: ignored persisted service pause, err=%v", refusal.label, err)
		}
		now = now.Add(refusal.pause)
		setPhase("quota reset")
		if rs, err := reopened.Search(ctx, movie(802)); err != nil || len(rs) == 0 {
			t.Errorf("%s: playback unavailable after reset: %v", refusal.label, err)
		}
		now = now.Add(24 * time.Hour)
		only = New(indexers[0], db, options)
	}
}
