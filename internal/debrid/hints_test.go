package debrid

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/newznab"
)

// A source's cached markers stand in for a provider's own answer: a cached
// hint ranks the torrent as known cached, and with CachedOnly a not-cached
// hint skips the provider without adding the torrent. A provider's own
// answer always wins, and a hint for another service changes nothing.
func TestApplyHints(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid, config.DebridTorBox)
	rd, tb := h.provs["realdebrid-0"], h.provs["torbox-1"]
	rel := torrentRelease("r1")

	hinted := rel
	hinted.DebridCached = []string{config.DebridAllDebrid}
	h.svc.ApplyHints(t.Context(), []core.Release{hinted})
	if h.svc.KnownCached(testHash) {
		t.Fatal("a hint for a service without an account counted")
	}

	hinted.DebridCached = []string{config.DebridTorBox}
	h.svc.ApplyHints(t.Context(), []core.Release{hinted})
	if !h.svc.KnownCached(testHash) {
		t.Fatal("cached hint ignored")
	}

	// Real-Debrid reported as not holding it: skipped without an add;
	// TorBox, hinted cached and holding it, serves the release.
	hinted.DebridUncached = []string{config.DebridRealDebrid}
	h.svc.ApplyHints(t.Context(), []core.Release{hinted})
	tb.mu.Lock()
	tb.cached[testHash] = true
	tb.mu.Unlock()
	if _, err := h.svc.Acquire(t.Context(), rel); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if n := rd.callCount("acquire"); n != 0 {
		t.Fatalf("hinted-uncached provider was asked %d times", n)
	}

	// TorBox's own answer (cached, now recorded) wins over a later hint.
	contrary := rel
	contrary.DebridUncached = []string{config.DebridTorBox}
	h.svc.ApplyHints(t.Context(), []core.Release{contrary})
	if !h.svc.KnownCached(testHash) {
		t.Fatal("a hint replaced the provider's own answer")
	}
}

// Without CachedOnly an uncached torrent is left to download, so a
// not-cached hint must not stop the add.
func TestApplyHintsUncachedIgnoredWhenDownloading(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid)
	cfg := h.svc.Config()
	cfg.CachedOnly = false
	h.svc.UpdateConfig(cfg)
	rd := h.provs["realdebrid-0"]
	rel := torrentRelease("r1")
	rel.DebridUncached = []string{config.DebridRealDebrid}
	h.svc.ApplyHints(t.Context(), []core.Release{rel})
	h.cacheEverywhere(testHash)
	if _, err := h.svc.Acquire(t.Context(), rel); err != nil || rd.callCount("acquire") != 1 {
		t.Fatalf("acquire: %v (calls %d)", err, rd.callCount("acquire"))
	}
}

// torznabReleases searches a Torznab indexer serving items through the
// debrid indexer wrapper, as a search does, so the releases carry what the
// feed's parser made of them and their hints are applied.
func torznabReleases(t *testing.T, h *harness, items ...string) []core.Release {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("t") == "caps" {
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><caps><searching><search available="yes" supportedParams="q"/></searching></caps>`)
			return
		}
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>`+
			strings.Join(items, "")+`</channel></rss>`)
	}))
	t.Cleanup(srv.Close)
	cl := newznab.New(config.Indexer{ID: "tz", Name: "tz", URL: srv.URL, APIKey: "k", Enabled: true, Protocol: "torrent"}, nil, newznab.Options{})
	rels, err := NewIndexer(cl, h.svc).Search(t.Context(), core.SearchRequest{Title: "Movie"})
	if err != nil || len(rels) == 0 {
		t.Fatalf("search: %v (%d releases)", err, len(rels))
	}
	return rels
}

// torznabItem is a feed item for testHash marked cached on the given codes.
func torznabItem(cachedOn string) string {
	return `<item><title>Movie.2024.1080p.WEB-DL-GRP</title><guid>tz-1</guid><link>https://tracker.example/1.torrent</link>` +
		`<enclosure url="https://tracker.example/1.torrent" length="4000000000" type="application/x-bittorrent"/>` +
		`<torznab:attr name="infohash" value="` + testHash + `"/><torznab:attr name="cachedon" value="` + cachedOn + `"/></item>`
}

// A Torznab feed's cachedon marker reaches the service as a hint, like an
// add-on's: in memory, for 30 minutes.
func TestTorznabCachedOnHint(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid, config.DebridTorBox)
	start := time.Now()
	h.svc.now = func() time.Time { return start }
	rels := torznabReleases(t, h, torznabItem("tb"))
	if !slices.Equal(rels[0].DebridCached, []string{config.DebridTorBox}) {
		t.Fatalf("DebridCached = %q", rels[0].DebridCached)
	}
	if cached, ok, _ := h.svc.knownFor(t.Context(), "torbox-1", testHash); !ok || !cached {
		t.Fatalf("torbox hint = cached %v known %v, want cached", cached, ok)
	}
	if _, ok, _ := h.svc.knownFor(t.Context(), "realdebrid-0", testHash); ok {
		t.Fatal("a marker for TorBox gave Real-Debrid an answer")
	}
	h.svc.now = func() time.Time { return start.Add(hintTTL + time.Second) }
	if h.svc.KnownCached(testHash) {
		t.Fatal("hint outlived its 30 minutes")
	}
}

// A provider's own answer stands: a Torznab marker neither replaces a
// recorded "cached" (with its longer memory) nor a recorded "not cached".
func TestTorznabCachedOnKeepsProviderAnswer(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid, config.DebridTorBox)
	start := time.Now()
	h.svc.now = func() time.Time { return start }
	h.svc.setKnown(t.Context(), "realdebrid-0", testHash, "", true)
	h.svc.setKnown(t.Context(), "torbox-1", testHash, "", false)
	torznabReleases(t, h, torznabItem("rd,tb"))
	if cached, ok, _ := h.svc.knownFor(t.Context(), "torbox-1", testHash); !ok || cached {
		t.Fatalf("torbox = cached %v known %v, want its own not-cached answer", cached, ok)
	}
	h.svc.now = func() time.Time { return start.Add(hintTTL + time.Minute) }
	if cached, ok, _ := h.svc.knownFor(t.Context(), "realdebrid-0", testHash); !ok || !cached {
		t.Fatalf("realdebrid = cached %v known %v, want its own cached answer past the hint TTL", cached, ok)
	}
}

// A provider a release marks as cached is tried first; the others are not
// asked once it serves the release.
func TestAcquireTriesMarkedProviderFirst(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid, config.DebridTorBox)
	h.cacheEverywhere(testHash)
	rel := torznabReleases(t, h, torznabItem("tb"))[0]
	b, err := h.svc.Acquire(t.Context(), rel)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if p, _ := DecodePayload(b); p.Provider != "torbox-1" {
		t.Fatalf("served by %q, want torbox-1", p.Provider)
	}
	if n := h.provs["realdebrid-0"].callCount("acquire"); n != 0 {
		t.Fatalf("unmarked first provider asked %d times", n)
	}
}

// Without a marker on the release, owner order stands, even when a
// lower-priority provider is already known cached (e.g. from a bulk check).
func TestAcquireOwnerOrderWithoutMarker(t *testing.T) {
	for _, knownTB := range []bool{false, true} {
		h := newHarness(t, 1<<20, config.DebridRealDebrid, config.DebridTorBox)
		h.cacheEverywhere(testHash)
		if knownTB {
			h.svc.setKnown(t.Context(), "torbox-1", testHash, "", true)
		}
		b, err := h.svc.Acquire(t.Context(), torrentRelease("r1"))
		if err != nil {
			t.Fatalf("acquire (known %v): %v", knownTB, err)
		}
		if p, _ := DecodePayload(b); p.Provider != "realdebrid-0" || h.provs["torbox-1"].callCount("acquire") != 0 {
			t.Fatalf("known %v: served by %q, torbox asked %d times; want owner order", knownTB, p.Provider, h.provs["torbox-1"].callCount("acquire"))
		}
	}
}

// A provider's own "not cached" answer wins over the marker: it is not
// moved ahead (and, as before, not asked).
func TestAcquireMarkerYieldsToProviderUncached(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid, config.DebridTorBox, config.DebridAllDebrid)
	h.cacheEverywhere(testHash)
	h.svc.setKnown(t.Context(), "torbox-1", testHash, "", false)
	rel := torznabReleases(t, h, torznabItem("tb,ad"))[0]
	b, err := h.svc.Acquire(t.Context(), rel)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if p, _ := DecodePayload(b); p.Provider != "alldebrid-2" {
		t.Fatalf("served by %q, want alldebrid-2", p.Provider)
	}
	if rd, tb := h.provs["realdebrid-0"].callCount("acquire"), h.provs["torbox-1"].callCount("acquire"); rd != 0 || tb != 0 {
		t.Fatalf("realdebrid asked %d, torbox asked %d; want neither", rd, tb)
	}
}

// A link-only Torznab item has no hash when hints are applied; once its
// hash is known, the marker it carries still puts its provider first.
func TestAcquireMarkerOnLinkOnlyItem(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid, config.DebridTorBox)
	h.cacheEverywhere(testHash)
	item := `<item><title>Movie.2024.1080p.WEB-DL-GRP</title><guid>tz-2</guid><link>https://tracker.example/2.torrent</link>` +
		`<torznab:attr name="cachedon" value="tb"/></item>`
	rel := torznabReleases(t, h, item)[0]
	if rel.InfoHash != "" || rel.Protocol != core.ProtocolTorrent {
		t.Fatalf("fixture: %+v", rel)
	}
	if h.svc.KnownCached(testHash) {
		t.Fatal("hashless item gave a hint")
	}
	rel.InfoHash = testHash // resolved from the .torrent later
	b, err := h.svc.Acquire(t.Context(), rel)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if p, _ := DecodePayload(b); p.Provider != "torbox-1" || h.provs["realdebrid-0"].callCount("acquire") != 0 {
		t.Fatalf("served by %q, realdebrid asked %d times; want torbox first", p.Provider, h.provs["realdebrid-0"].callCount("acquire"))
	}
}

// File listing and pack matching try the marked provider first too.
func TestListAndPackMatchTryMarkedProviderFirst(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid, config.DebridTorBox)
	h.cacheEverywhere(testHash)
	rel := torznabReleases(t, h, torznabItem("tb"))[0]
	if files, err := h.svc.ListReleaseFiles(t.Context(), rel); err != nil || len(files) == 0 {
		t.Fatalf("list: %v (%d files)", err, len(files))
	}
	if match, definitive := h.svc.CachedPackMatcher(t.Context(), rel); match == nil || !definitive {
		t.Fatalf("pack matcher: %v %v", match != nil, definitive)
	}
	if rd, tb := h.provs["realdebrid-0"].callCount("acquire"), h.provs["torbox-1"].callCount("acquire"); rd != 0 || tb != 2 {
		t.Fatalf("realdebrid asked %d, torbox asked %d; want 0 and 2", rd, tb)
	}
}
