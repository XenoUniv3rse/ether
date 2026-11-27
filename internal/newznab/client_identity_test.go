package newznab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type wireRequest struct {
	ua          string
	accept      string
	traceparent string
	rawQuery    string
}

type wireServer struct {
	mu  sync.Mutex
	got []wireRequest
	srv *httptest.Server
}

func newWireServer(t *testing.T) *wireServer {
	t.Helper()
	ws := &wireServer{}
	ws.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws.mu.Lock()
		ws.got = append(ws.got, wireRequest{
			ua:          r.Header.Get("User-Agent"),
			accept:      r.Header.Get("Accept"),
			traceparent: r.Header.Get("traceparent"),
			rawQuery:    r.URL.RawQuery,
		})
		ws.mu.Unlock()
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, strings.Replace(loadFixture(t, "caps.xml", ws.srv.URL), "</categories>", `<category id="3000"/><category id="7000"/></categories>`, 1))
		case "get":
			fmt.Fprint(w, `<?xml version="1.0"?><nzb><file/></nzb>`)
		case "tvsearch", "movie":
			fmt.Fprint(w, loadFixture(t, "tv.xml", ws.srv.URL))
		default:
			if r.URL.Path == "/dl/file.nzb" {
				fmt.Fprint(w, `<?xml version="1.0"?><nzb><file/></nzb>`)
				return
			}
			fmt.Fprint(w, loadFixture(t, "movie.xml", ws.srv.URL))
		}
	}))
	t.Cleanup(ws.srv.Close)
	return ws
}

func (ws *wireServer) last() wireRequest {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.got[len(ws.got)-1]
}

func (ws *wireServer) reset() {
	ws.mu.Lock()
	ws.got = nil
	ws.mu.Unlock()
}

func (ws *wireServer) all() []wireRequest {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return append([]wireRequest(nil), ws.got...)
}

func wireConfig(url string) config.Indexer {
	return config.Indexer{ID: "idx1", Name: "Fake", URL: url, APIKey: "secret", Enabled: true}
}

func tvSearch() core.SearchRequest {
	return core.SearchRequest{
		Type: core.TypeEpisode, IDs: core.IDs{TVDB: 81797}, Title: "Show", Season: 1, Episode: 2,
	}
}

func TestClientIdentityOnWire(t *testing.T) {
	ws := newWireServer(t)
	ctx := context.Background()
	wantDefaultUA := config.DefaultUserAgent()

	// Empty rules send Ether's honest operator-linked user agent and normal query order.
	c := New(wireConfig(ws.srv.URL), nil, Options{})
	if _, err := c.Caps(ctx); err != nil {
		t.Fatal(err)
	}
	if got := ws.last(); got.ua != wantDefaultUA || got.rawQuery != "apikey=secret&t=caps" || got.accept != "" {
		t.Fatalf("default caps: %+v (want ua=%q)", got, wantDefaultUA)
	}
	ws.reset()
	if _, err := c.Search(ctx, tvSearch()); err != nil {
		t.Fatal(err)
	}
	const defaultTV = "apikey=secret&cat=5000&ep=2&extended=1&limit=100&season=1&t=tvsearch&tvdbid=81797"
	if got := ws.last(); got.rawQuery != defaultTV || got.ua != wantDefaultUA {
		t.Fatalf("default tv search: %+v", got)
	}

	// Compatibility profile sets headers only: no query reshaping, no traceparent.
	ws.reset()
	c = New(wireConfig(ws.srv.URL), nil, Options{
		ClientIdentity: config.ClientIdentity{
			Rules: []config.IdentityRule{
				{ID: "r-search", Target: config.TargetAllSearches, Profile: "prowlarr"},
				{ID: "r-nzb", Target: config.TargetNZBDownloads, Profile: "sabnzbd"},
			},
		},
	})
	if _, err := c.Search(ctx, tvSearch()); err != nil {
		t.Fatal(err)
	}
	if got := ws.last(); got.rawQuery != defaultTV || got.ua != config.ProfileUserAgent("prowlarr") || got.traceparent != "" || got.accept == "" {
		t.Fatalf("prowlarr search wire: %+v", got)
	}

	// Feed-supplied NZB download URL resolves through the NZB download rule.
	ws.reset()
	movie := core.Release{ID: "r1", Name: "Movie.2026.1080p", GUID: "g1", NZBURL: ws.srv.URL + "/dl/file.nzb", Category: 2040}
	if _, err := c.FetchNZB(ctx, movie); err != nil {
		t.Fatal(err)
	}
	if got := ws.last(); got.ua != config.ProfileUserAgent("sabnzbd") {
		t.Fatalf("feed NZB grab wire: %+v", got)
	}
}

func TestClientIdentityCapsTest(t *testing.T) {
	ws := newWireServer(t)
	ctx := context.Background()
	policy := config.ClientIdentity{
		Rules: []config.IdentityRule{
			{ID: "r1", Target: config.TargetIndexer, IndexerID: "idx1", Profile: "sonarr"},
		},
	}
	id, err := TestCaps(ctx, wireConfig(ws.srv.URL), policy)
	if err != nil {
		t.Fatal(err)
	}
	if id.Profile != "sonarr" || id.RuleID != "r1" {
		t.Fatalf("TestCaps resolved identity: %+v", id)
	}
	got := ws.all()
	if len(got) != 1 || got[0].rawQuery != "apikey=secret&t=caps" || got[0].ua != config.ProfileUserAgent("sonarr") {
		t.Fatalf("TestCaps requests: %+v", got)
	}
}

func TestClientIdentityCacheSeparated(t *testing.T) {
	ws := newWireServer(t)
	ctx := context.Background()
	c := New(wireConfig(ws.srv.URL), nil, Options{
		ClientIdentity: config.ClientIdentity{
			Rules: []config.IdentityRule{{
				ID: "r1", Target: config.TargetAllSearches, Profile: "custom",
				Headers: []config.KVOverride{{Name: "User-Agent", Value: "MyClient/1.0"}},
			}},
		},
	})
	if _, err := c.Search(ctx, tvSearch()); err != nil {
		t.Fatal(err)
	}
	before := len(ws.all())
	if _, err := c.Search(ctx, tvSearch()); err != nil {
		t.Fatal(err)
	}
	if got := len(ws.all()); got != before {
		t.Fatalf("same identity re-searched the indexer: %d -> %d requests", before, got)
	}
	c.SetClientIdentity(config.ClientIdentity{
		Rules: []config.IdentityRule{{
			ID: "r1", Target: config.TargetAllSearches, Profile: "custom",
			Headers: []config.KVOverride{{Name: "User-Agent", Value: "MyClient/2.0"}},
		}},
	})
	if _, err := c.Search(ctx, tvSearch()); err != nil {
		t.Fatal(err)
	}
	if got := len(ws.all()); got == before {
		t.Fatal("editing the rule kept the old cached answer")
	}
	if got := ws.last().ua; got != "MyClient/2.0" {
		t.Fatalf("edited user agent not sent: %q", got)
	}
}

func TestTorznabNumberAttribute(t *testing.T) {
	const xmlFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
<channel><item>
<title>Movie.2026.1080p.WEB-DL</title><guid>g1</guid>
<link>http://ix.example/api?t=get&amp;id=g1</link>
<torznab:attr name="category" number="2040"/>
</item></channel></rss>`
	rs, err := parseFeed([]byte(xmlFeed), "ix")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Category != 2040 {
		t.Fatalf("xml number attribute: %+v", rs)
	}
	const jsonFeed = `{"rss":{"channel":{"item":[{"title":"Movie.2026.1080p.WEB-DL","guid":"g2","link":"http://ix.example/api?t=get&id=g2","attr":[{"@attributes":{"name":"category","number":"2040"}}]}]}}}`
	rs, err = parseFeed([]byte(jsonFeed), "ix")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Category != 2040 {
		t.Fatalf("json number attribute: %+v", rs)
	}
}

func TestClientIdentityHeadersStayOnTheIndexerOrigin(t *testing.T) {
	c := New(wireConfig("https://ix.example.com"), nil, Options{})
	id := config.Identity{
		UserAgent:  "MyClient/1.0",
		RuleTarget: config.TargetIndexer,
		Headers:    map[string]string{"X-Api-Client": "s3cret"},
		Ordered: []config.HeaderKV{
			{Name: "User-Agent", Value: "MyClient/1.0"},
			{Name: "X-Api-Client", Value: "s3cret"},
		},
	}
	// A feed-supplied download URL on another host is untrusted data.
	req, _ := http.NewRequest(http.MethodGet, "https://cdn.example.net/nzb", nil)
	c.applyIdentity(req, id)
	if got := req.Header.Get("User-Agent"); got != "MyClient/1.0" {
		t.Fatalf("user agent missing: %q", got)
	}
	if got := req.Header.Get("X-Api-Client"); got != "" {
		t.Fatalf("custom header sent to another host: %q", got)
	}
	req, _ = http.NewRequest(http.MethodGet, "https://ix.example.com/api?t=get", nil)
	c.applyIdentity(req, id)
	if got := req.Header.Get("X-Api-Client"); got != "s3cret" {
		t.Fatalf("custom header missing on the indexer origin: %q", got)
	}
	// A redirect that leaves the origin must not carry them either.
	next, _ := http.NewRequest(http.MethodGet, "https://cdn.example.net/x", nil)
	next.Header.Set("X-Api-Client", "s3cret")
	c.dropForeignIdentityHeaders(next, "https://ix.example.com", id)
	if got := next.Header.Get("X-Api-Client"); got != "" {
		t.Fatalf("redirect kept the custom header: %q", got)
	}
	next, _ = http.NewRequest(http.MethodGet, "https://ix.example.com/x", nil)
	next.Header.Set("X-Api-Client", "s3cret")
	c.dropForeignIdentityHeaders(next, "https://ix.example.com", id)
	if got := next.Header.Get("X-Api-Client"); got != "s3cret" {
		t.Fatalf("redirect within the origin dropped the header: %q", got)
	}
}

func TestTorznabStandardCategoryWins(t *testing.T) {
	const xmlFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
<channel><item>
<title>Movie.2026.1080p.WEB-DL</title><guid>g1</guid>
<link>http://ix.example/api?t=get&amp;id=g1</link>
<torznab:attr name="category" value="2000"/>
<torznab:attr name="category" value="100001"/>
</item></channel></rss>`
	rs, err := parseFeed([]byte(xmlFeed), "ix")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Category != 2000 {
		t.Fatalf("multi-category movie: %+v", rs)
	}
}

func TestClientIdentityByMediaTypeOnWire(t *testing.T) {
	ws := newWireServer(t)
	ctx := context.Background()
	wantDefaultUA := config.DefaultUserAgent()

	c := New(wireConfig(ws.srv.URL), nil, Options{
		ClientIdentity: config.ClientIdentity{
			Rules: []config.IdentityRule{
				{ID: "r-movie", Target: config.TargetIndexer, IndexerID: "idx1", MediaType: config.IdentityMediaMovie, Profile: "radarr"},
				{ID: "r-tv", Target: config.TargetIndexer, IndexerID: "idx1", MediaType: config.IdentityMediaTV, Profile: "sonarr"},
				{ID: "r-music", Target: config.TargetAllSearches, MediaType: config.IdentityMediaMusic, Profile: "prowlarr"},
				{ID: "r-books", Target: config.TargetAllSearches, MediaType: config.IdentityMediaBooks, Profile: "nzbhydra2"},
			},
		},
	})

	// Capabilities have no media type -> honest Ether default.
	if _, err := c.Caps(ctx); err != nil {
		t.Fatal(err)
	}
	if got := ws.last(); got.ua != wantDefaultUA {
		t.Fatalf("caps with only media-scoped rules: ua=%q, want default %q", got.ua, wantDefaultUA)
	}

	// Movie search sends Radarr without changing query shape; TV search sends Sonarr.
	ws.reset()
	movieReq := core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt0133093"}, Title: "The Matrix", Year: 1999}
	if _, err := c.Search(ctx, movieReq); err != nil {
		t.Fatal(err)
	}
	const wantMovieQuery = "apikey=secret&cat=2000&extended=1&imdbid=0133093&limit=100&t=movie"
	if got := ws.last(); got.ua != config.ProfileUserAgent("radarr") || got.rawQuery != wantMovieQuery {
		t.Fatalf("movie search wire: %+v", got)
	}

	ws.reset()
	if _, err := c.Search(ctx, tvSearch()); err != nil {
		t.Fatal(err)
	}
	const wantTVQuery = "apikey=secret&cat=5000&ep=2&extended=1&limit=100&season=1&t=tvsearch&tvdbid=81797"
	if got := ws.last(); got.ua != config.ProfileUserAgent("sonarr") || got.rawQuery != wantTVQuery {
		t.Fatalf("tv search wire: %+v", got)
	}

	// Plugin-kind Music and Books searches resolve through category family.
	ws.reset()
	if _, err := c.Search(ctx, core.SearchRequest{Title: "Album", Category: "music"}); err != nil {
		t.Fatal(err)
	}
	if got := ws.last(); got.ua != config.ProfileUserAgent("prowlarr") {
		t.Fatalf("music search wire: %+v", got)
	}
	ws.reset()
	if _, err := c.Search(ctx, core.SearchRequest{Title: "Book", Category: "7000,3030"}); err != nil {
		t.Fatal(err)
	}
	if got := ws.last(); got.ua != config.ProfileUserAgent("nzbhydra2") {
		t.Fatalf("books search wire: %+v", got)
	}

	// Downloads resolve by media type too: Movie NZB -> Radarr, TV NZB -> Sonarr.
	ws.reset()
	if _, err := c.FetchNZB(ctx, core.Release{ID: "m1", Name: "Movie.2026.1080p", NZBURL: ws.srv.URL + "/dl/file.nzb", Category: 2040}); err != nil {
		t.Fatal(err)
	}
	if got := ws.last(); got.ua != config.ProfileUserAgent("radarr") {
		t.Fatalf("movie nzb grab wire: %+v", got)
	}
	ws.reset()
	if _, err := c.FetchNZB(ctx, core.Release{ID: "t1", Name: "Show.S01E02.1080p", NZBURL: ws.srv.URL + "/dl/file.nzb", Category: 5040}); err != nil {
		t.Fatal(err)
	}
	if got := ws.last(); got.ua != config.ProfileUserAgent("sonarr") {
		t.Fatalf("tv nzb grab wire: %+v", got)
	}

	// Editing the movie rule invalidates the cached movie search and applies param overrides from the matched rule.
	before := len(ws.all())
	c.SetClientIdentity(config.ClientIdentity{
		Rules: []config.IdentityRule{
			{
				ID: "r-movie", Target: config.TargetIndexer, IndexerID: "idx1",
				MediaType: config.IdentityMediaMovie, Profile: "radarr",
				Params: []config.KVOverride{{Name: "custom", Value: "1"}},
			},
			{ID: "r-tv", Target: config.TargetIndexer, IndexerID: "idx1", MediaType: config.IdentityMediaTV, Profile: "sonarr"},
		},
	})
	if _, err := c.Search(ctx, movieReq); err != nil {
		t.Fatal(err)
	}
	if len(ws.all()) == before {
		t.Fatal("editing the movie rule kept the old cached movie answer")
	}
	if got := ws.last(); !strings.Contains(got.rawQuery, "custom=1") {
		t.Fatalf("movie rule param override not applied to search URL: %+v", got)
	}
}
