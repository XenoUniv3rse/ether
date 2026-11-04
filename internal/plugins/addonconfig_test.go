package plugins

import (
	"encoding/base64"
	"net/url"
	"testing"
)

// An add-on reads its settings back exactly as it wrote them, so Ether
// must re-encode an untouched configuration byte for byte, and an edited
// one in the add-on's own shape. Secrets never reach the console, and a
// secret left empty keeps its saved value.
func TestAddonConfigRoundTrip(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(`{"debridService":"realdebrid","debridApiKey":"AbC123dEf456GhI789jKl0","maxResults":5,"cachedOnly":true,"languages":["en","fr"]}`))
	cases := []struct {
		name, seg, format string
	}{
		{"torrentio key=value", "providers=yts,eztv|sort=qualitysize|qualityfilter=480p,scr|realdebrid=ABCDEFGH1234567890ijkl", "kv"},
		{"comet base64 json", url.PathEscape(b64), "b64json"},
		{"sdk json", url.PathEscape(`{"language":"en-US","rpdbkey":"","includeAdult":"false"}`), "json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc, ok := decodeSegment(c.seg)
			if !ok || doc.format != c.format {
				t.Fatalf("decode %q: ok=%v format=%q", c.seg, ok, doc.format)
			}
			again, err := doc.encode()
			if err != nil || again != c.seg {
				t.Fatalf("untouched re-encode changed the link:\n got %q\nwant %q (%v)", again, c.seg, err)
			}
			for _, f := range fieldsOf(doc) {
				if f.Kind == "secret" && f.Value != nil {
					t.Fatalf("secret %s sent to the console", f.Key)
				}
			}
		})
	}

	// Edits keep the add-on's shape; an empty secret keeps its value.
	doc, _ := decodeSegment(cases[0].seg)
	fields := fieldsOf(doc)
	edited, err := doc.apply(fields, map[string]any{"providers": []any{"yts"}, "realdebrid": "", "sort": "size"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := edited.encode(); got != "providers=yts|sort=size|qualityfilter=480p,scr|realdebrid=ABCDEFGH1234567890ijkl" {
		t.Fatalf("edited torrentio link: %q", got)
	}
	doc, _ = decodeSegment(cases[1].seg)
	edited, err = doc.apply(fieldsOf(doc), map[string]any{"maxResults": "12", "cachedOnly": false})
	if err != nil {
		t.Fatal(err)
	}
	seg, _ := edited.encode()
	back, ok := decodeSegment(seg)
	if !ok {
		t.Fatalf("edited comet link unreadable: %q", seg)
	}
	want := map[string]string{"maxResults": "12", "cachedOnly": "false", "debridApiKey": `"AbC123dEf456GhI789jKl0"`}
	for _, v := range back.values {
		if w, ok := want[v.key]; ok && string(v.raw) != w {
			t.Fatalf("%s = %s, want %s", v.key, v.raw, w)
		}
	}

	// Nested settings are fields of their own and are written back in place.
	nested := url.PathEscape(base64.StdEncoding.EncodeToString([]byte(`{"maxSize":0,"languages":{"required":[],"preferred":["en"]},"options":{"remove_ranks_under":-10,"allow_english":false}}`)))
	doc, _ = decodeSegment(nested)
	fields = fieldsOf(doc)
	if untouched, _ := doc.apply(fields, map[string]any{}); func() string { s, _ := untouched.encode(); return s }() != nested {
		t.Fatal("untouched nested settings changed the link")
	}
	edited, err = doc.apply(fields, map[string]any{"languages/preferred": []any{"en", "fr"}, "options/allow_english": true})
	if err != nil {
		t.Fatal(err)
	}
	seg, _ = edited.encode()
	b, _ := url.PathUnescape(seg)
	raw, _ := base64.StdEncoding.DecodeString(b)
	if string(raw) != `{"maxSize":0,"languages":{"required":[],"preferred":["en","fr"]},"options":{"remove_ranks_under":-10,"allow_english":true}}` {
		t.Fatalf("edited nested settings: %s", raw)
	}

	// An opaque (encrypted) segment is not mistaken for settings.
	if _, ok := decodeSegment("D-mSYoc6t9wPXbV0sGnTXoeqtnYkqvz3l0dA"); ok {
		t.Fatal("opaque segment decoded as settings")
	}
}
