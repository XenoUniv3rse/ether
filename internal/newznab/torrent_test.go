package newznab

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

const torznabFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
<channel>
<item>
  <title>Movie.2024.1080p.WEB-DL.DDP5.1.H.264-GRP</title>
  <guid>https://tracker.example/details/1</guid>
  <link>https://tracker.example/download/1.torrent</link>
  <pubDate>Mon, 02 Jan 2006 15:04:05 -0700</pubDate>
  <enclosure url="https://tracker.example/download/1.torrent" length="4000000000" type="application/x-bittorrent"/>
  <torznab:attr name="seeders" value="42"/>
  <torznab:attr name="peers" value="50"/>
  <torznab:attr name="infohash" value="0123456789ABCDEF0123456789ABCDEF01234567"/>
</item>
<item>
  <title>Movie.2024.2160p.WEB-DL-GRP</title>
  <guid>https://tracker.example/details/2</guid>
  <link>magnet:?xt=urn:btih:AERUKZ4JVPG66AJDIVTYTK6N54ASGRSH&amp;dn=Movie</link>
  <enclosure url="magnet:?xt=urn:btih:AERUKZ4JVPG66AJDIVTYTK6N54ASGRSH&amp;dn=Movie" length="9000000000" type="application/x-bittorrent"/>
</item>
<item>
  <title>Movie.2024.720p.WEB-GRP</title>
  <guid>https://tracker.example/details/3</guid>
  <link>https://tracker.example/download/3.torrent</link>
  <size>1000000000</size>
</item>
</channel>
</rss>`

func TestParseTorznabFeed(t *testing.T) {
	rels, err := parseFeed([]byte(torznabFeed), "tz")
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 3 {
		t.Fatalf("got %d releases", len(rels))
	}
	a := rels[0]
	if a.Protocol != core.ProtocolTorrent || a.InfoHash != "0123456789abcdef0123456789abcdef01234567" || a.Seeders != 42 || a.Peers != 50 || a.Size != 4000000000 {
		t.Fatalf("first: %+v", a)
	}
	if a.NZBURL != "https://tracker.example/download/1.torrent" {
		t.Fatalf("torrent url %q", a.NZBURL)
	}
	b := rels[1]
	if b.Protocol != core.ProtocolTorrent || b.InfoHash != NormalizeInfoHash("AERUKZ4JVPG66AJDIVTYTK6N54ASGRSH") || len(b.InfoHash) != 40 || b.NZBURL != "" {
		t.Fatalf("magnet item: %+v", b)
	}
	// Third item has no torrent markers; the client marks it for Torznab indexers.
	if rels[2].Protocol != "" {
		t.Fatalf("plain item protocol %q", rels[2].Protocol)
	}
}

func TestInfoHashFormats(t *testing.T) {
	raw := make([]byte, 20)
	for i := range raw {
		raw[i] = byte(i * 7)
	}
	hexHash := hex.EncodeToString(raw)
	if got := NormalizeInfoHash(strings.ToUpper(hexHash)); got != hexHash {
		t.Fatalf("hex: %q", got)
	}
	b32 := "AAHBEHCLDQSXAJLTVIAKDGBN5YYWBVSE" // arbitrary 32-char base32
	if got := NormalizeInfoHash(b32); len(got) != 40 {
		t.Fatalf("base32: %q", got)
	}
	if NormalizeInfoHash("nope") != "" || NormalizeInfoHash(strings.Repeat("z", 40)) != "" {
		t.Fatal("garbage accepted")
	}
	m := "magnet:?dn=x&xt=urn:btih:" + hexHash + "&tr=udp%3A%2F%2Ft"
	if InfoHashFromMagnet(m) != hexHash {
		t.Fatal("magnet hash")
	}
	if InfoHashFromMagnet("https://x") != "" {
		t.Fatal("non-magnet")
	}
	if got := InfoHashFromMagnet(MagnetFor(hexHash, "A B", []string{"udp://t:1"})); got != hexHash {
		t.Fatalf("round trip %q", got)
	}
}

func bstr(s string) string { return fmt.Sprintf("%d:%s", len(s), s) }

func sampleTorrent() ([]byte, string) {
	info := "d" + bstr("length") + "i1234e" + bstr("name") + bstr("Show.S01.1080p") + bstr("piece length") + "i262144e" + bstr("pieces") + bstr(strings.Repeat("x", 20)) + "e"
	sum := sha1.Sum([]byte(info))
	doc := "d" + bstr("announce") + bstr("udp://a:1") + bstr("announce-list") + "ll" + bstr("udp://a:1") + bstr("udp://b:2") + "ee" + bstr("comment") + bstr("hi") + bstr("creation date") + "i1700000000e" + bstr("info") + info + "e"
	return []byte(doc), hex.EncodeToString(sum[:])
}

func TestParseTorrent(t *testing.T) {
	doc, want := sampleTorrent()
	meta, err := ParseTorrent(doc)
	if err != nil {
		t.Fatal(err)
	}
	if meta.InfoHash != want || meta.Name != "Show.S01.1080p" || len(meta.Trackers) != 2 {
		t.Fatalf("meta %+v want %s", meta, want)
	}
	if len(meta.Files) != 1 || meta.Files[0].Index != 0 || meta.Files[0].Path != "Show.S01.1080p" || meta.Files[0].Size != 1234 {
		t.Fatalf("single-file meta.Files = %+v", meta.Files)
	}
	for _, bad := range []string{"", "<html>", "d4:infoi1e", "d4:info", "l1:ae"} {
		if _, err := ParseTorrent([]byte(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestParseTorrentMultiFileAndPadding(t *testing.T) {
	file1 := "d" + bstr("length") + "i25000000e" + bstr("path") + "l" + bstr("Disc 1") + bstr("01 - Opening.flac") + "ee"
	pad := "d" + bstr("attr") + bstr("p") + bstr("length") + "i4096e" + bstr("path") + "l" + bstr(".pad") + bstr("4096") + "ee"
	file2 := "d" + bstr("length") + "i18000000e" + bstr("path") + "l" + bstr("02 - Fallback.flac") + "e" + bstr("path.utf-8") + "l" + bstr("02 - Café.flac") + "ee"
	info := "d" + bstr("files") + "l" + file1 + pad + file2 + "e" + bstr("name") + bstr("Album") + bstr("piece length") + "i262144e" + bstr("pieces") + bstr(strings.Repeat("x", 20)) + "e"
	doc := []byte("d" + bstr("info") + info + "e")

	meta, err := ParseTorrent(doc)
	if err != nil {
		t.Fatalf("ParseTorrent: %v", err)
	}
	if len(meta.Files) != 2 {
		t.Fatalf("len(Files) = %d, want 2 (padding excluded): %+v", len(meta.Files), meta.Files)
	}
	if meta.Files[0].Index != 0 || meta.Files[0].Path != "Album/Disc 1/01 - Opening.flac" || meta.Files[0].Size != 25000000 {
		t.Fatalf("file 0 = %+v", meta.Files[0])
	}
	if meta.Files[1].Index != 1 || meta.Files[1].Path != "Album/02 - Café.flac" || meta.Files[1].Size != 18000000 {
		t.Fatalf("file 1 = %+v", meta.Files[1])
	}
}

func TestResolveTorrentDownloadsAndFollowsMagnetRedirects(t *testing.T) {
	doc, want := sampleTorrent()
	magnetHash := strings.Repeat("ab", 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/file.torrent":
			w.Header().Set("Content-Type", "application/x-bittorrent")
			w.Write(doc)
		case "/dl/redirect":
			http.Redirect(w, r, "magnet:?xt=urn:btih:"+magnetHash, http.StatusFound)
		case "/dl/html":
			fmt.Fprint(w, "<html>nope</html>")
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	cfg := config.Indexer{ID: "tz", Name: "tz", URL: srv.URL, Enabled: true, Protocol: "torrent", DailyGrabs: 10}
	m := NewManager(nil, []config.Indexer{cfg}, Options{})
	ctx := context.Background()

	got, err := m.ResolveTorrent(ctx, core.Release{Name: "Show.S01", Indexer: "tz", NZBURL: srv.URL + "/dl/file.torrent", Protocol: core.ProtocolTorrent})
	if err != nil || got.InfoHash != want || InfoHashFromMagnet(got.MagnetURL) != want {
		t.Fatalf("torrent file: %+v %v", got, err)
	}
	got, err = m.ResolveTorrent(ctx, core.Release{Name: "x", Indexer: "tz", NZBURL: "/dl/redirect", Protocol: core.ProtocolTorrent})
	if err != nil || got.InfoHash != magnetHash {
		t.Fatalf("redirect: %+v %v", got, err)
	}
	if _, err = m.ResolveTorrent(ctx, core.Release{Name: "x", Indexer: "tz", NZBURL: srv.URL + "/dl/html"}); !errors.Is(err, errNotTorrent) {
		t.Fatalf("html: %v", err)
	}
	// Known hashes need no request and no budget.
	got, err = m.ResolveTorrent(ctx, core.Release{Name: "x", Indexer: "gone", InfoHash: strings.ToUpper(magnetHash)})
	if err != nil || got.InfoHash != magnetHash || got.MagnetURL == "" {
		t.Fatalf("known: %+v %v", got, err)
	}
	if _, err := m.FetchNZB(ctx, core.Release{Name: "x", Indexer: "tz", Protocol: core.ProtocolTorrent, NZBURL: srv.URL + "/dl/file.torrent"}); err == nil {
		t.Fatal("FetchNZB served a torrent")
	}
}

func TestTorznabClientMarksResultsAndManagerKeepsProtocolsApart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, `<caps><searching><search available="yes" supportedParams="q"/><movie-search available="yes" supportedParams="q,imdbid"/></searching></caps>`)
		default:
			fmt.Fprint(w, torznabFeed)
		}
	}))
	defer srv.Close()
	usenetFeed := `<rss><channel><item><title>Movie.2024.720p.WEB-GRP</title><guid>u1</guid><link>http://x/nzb</link><size>1000000000</size></item></channel></rss>`
	nz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("t") == "caps" {
			fmt.Fprint(w, `<caps><searching><search available="yes" supportedParams="q"/></searching></caps>`)
			return
		}
		fmt.Fprint(w, usenetFeed)
	}))
	defer nz.Close()
	m := NewManager(nil, []config.Indexer{
		{ID: "tz", Name: "tz", URL: srv.URL, Enabled: true, Protocol: "torrent"},
		{ID: "nz", Name: "nz", URL: nz.URL, Enabled: true, Priority: 1},
	}, Options{SearchCacheTTL: -1})
	rels, err := m.Search(context.Background(), core.SearchRequest{Type: core.TypeMovie, Title: "Movie", Year: 2024})
	if err != nil {
		t.Fatal(err)
	}
	var torrents, usenet int
	for _, r := range rels {
		if r.Protocol == core.ProtocolTorrent {
			torrents++
		} else {
			usenet++
		}
	}
	if torrents != 3 || usenet != 1 {
		t.Fatalf("torrents %d usenet %d: %+v", torrents, usenet, rels)
	}
}

// Both Torznab and Stremio use these values; malformed unrelated fields
// must not turn an escaped info hash into a different or missing torrent.
func TestMagnetEscapedHashWithMalformedField(t *testing.T) {
	const hash = "0123456789012345678901234567890123456789"
	magnet := "magnet:?xt=urn%3Abtih%3A" + hash + "&dn=bad%escape&tr=https%3A%2F%2Ftracker.invalid%2Fa"
	if got := InfoHashFromMagnet(magnet); got != hash {
		t.Fatalf("hash=%q", got)
	}
	if got := MagnetValues(magnet).Get("tr"); got != "https://tracker.invalid/a" {
		t.Fatalf("tracker=%q", got)
	}
}

// A feed's cachedon attribute (e.g. a cached feed's rd/ad/tb codes) becomes
// the release's DebridCached hint, in the XML and the JSON reader alike.
func TestParseTorznabCachedOn(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	item := func(guid string, attrs ...string) string {
		var b strings.Builder
		for _, v := range attrs {
			fmt.Fprintf(&b, `<torznab:attr name="cachedon" value="%s"/>`, v)
		}
		return `<item><title>Movie.2024.1080p.WEB-DL-GRP</title><guid>` + guid + `</guid>` +
			`<link>https://tracker.example/download/` + guid + `.torrent</link>` +
			`<torznab:attr name="infohash" value="` + hash + `"/>` + b.String() + `</item>`
	}
	cases := []struct {
		name  string
		attrs []string
		want  []string
	}{
		{"one", []string{"rd"}, []string{config.DebridRealDebrid}},
		{"several", []string{"rd,ad,tb"}, []string{config.DebridRealDebrid, config.DebridAllDebrid, config.DebridTorBox}},
		{"unknown", []string{"pm,xx,ad"}, []string{config.DebridAllDebrid}},
		{"upper case and spaces", []string{" RD , Tb "}, []string{config.DebridRealDebrid, config.DebridTorBox}},
		{"empty", []string{""}, nil},
		{"repeated", []string{"rd", "tb,rd", "ad"}, []string{config.DebridRealDebrid, config.DebridTorBox, config.DebridAllDebrid}},
		{"duplicates", []string{"rd,rd"}, []string{config.DebridRealDebrid}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rels, err := parseFeed([]byte(`<?xml version="1.0"?><rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>`+item("g", tc.attrs...)+`</channel></rss>`), "tz")
			if err != nil || len(rels) != 1 {
				t.Fatalf("parse: %v (%d releases)", err, len(rels))
			}
			if !slices.Equal(rels[0].DebridCached, tc.want) {
				t.Fatalf("xml DebridCached = %q, want %q", rels[0].DebridCached, tc.want)
			}
			if len(rels[0].DebridUncached) != 0 {
				t.Fatalf("xml DebridUncached = %q", rels[0].DebridUncached)
			}
			var attrs []string
			for _, v := range tc.attrs {
				attrs = append(attrs, fmt.Sprintf(`{"@attributes":{"name":"cachedon","value":%q}}`, v))
			}
			body := `{"rss":{"channel":{"item":[{"title":"Movie.2024.1080p.WEB-DL-GRP","guid":"g","link":"https://tracker.example/download/g.torrent","attr":[` +
				`{"@attributes":{"name":"infohash","value":"` + hash + `"}}` + strings.Repeat(",", min(1, len(attrs))) + strings.Join(attrs, ",") + `]}]}}}`
			rels, err = parseFeed([]byte(body), "tz")
			if err != nil || len(rels) != 1 {
				t.Fatalf("parse json: %v (%d releases)", err, len(rels))
			}
			if !slices.Equal(rels[0].DebridCached, tc.want) {
				t.Fatalf("json DebridCached = %q, want %q", rels[0].DebridCached, tc.want)
			}
		})
	}
}

// The parser collects cachedon on any item, as it does infohash and seeders
// (a link-only Torznab item becomes a torrent later, in the client); a
// Usenet item keeps its protocol and the debrid hints ignore it.
func TestParseCachedOnUsenetItem(t *testing.T) {
	body := `<?xml version="1.0"?><rss xmlns:newznab="http://www.newznab.com/DTD/2010/feeds/attributes/"><channel><item>` +
		`<title>Movie.2024.1080p.WEB-DL-GRP</title><guid>n1</guid><link>https://idx.example/getnzb/n1.nzb</link>` +
		`<enclosure url="https://idx.example/getnzb/n1.nzb" length="1000" type="application/x-nzb"/>` +
		`<newznab:attr name="cachedon" value="rd"/></item></channel></rss>`
	rels, err := parseFeed([]byte(body), "nz")
	if err != nil || len(rels) != 1 {
		t.Fatalf("parse: %v (%d releases)", err, len(rels))
	}
	if rels[0].Protocol == core.ProtocolTorrent || rels[0].InfoHash != "" {
		t.Fatalf("usenet item became a torrent: %+v", rels[0])
	}
	if !slices.Equal(rels[0].DebridCached, []string{config.DebridRealDebrid}) {
		t.Fatalf("DebridCached = %q", rels[0].DebridCached)
	}
}
