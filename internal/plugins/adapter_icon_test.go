package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContributionIconsServedWithPluginIconFallback(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for x := 0; x < 40; x++ {
		for y := 0; y < 20; y++ {
			img.Set(x, y, color.RGBA{R: 0xe0, G: 0x60, B: 0x20, A: 0xff})
		}
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"id": "community.example.adaptericon", "version": "1.2.0", "name": "Adapter Icon",
		"logo": "https://example.com/stremio.png",
		"ether": map[string]any{
			"protocol": 1, "kind": "content", "summary": "Serves two protocols.",
			"icon":         "assets/icon.svg",
			"runtime":      map[string]any{"kind": "hosted", "entry": "plugin.js"},
			"capabilities": []string{"frontend_adapter", "catalog_provider"},
			"contributes": map[string]any{"mediaKinds": []MediaKindMeta{{
				Key: "recording", Label: "Recordings", Library: KindLibraryDecl{Name: "Recordings", Icon: "assets/recording.png"},
				Levels: []KindLevel{{Key: "recording", Label: "Recording"}}, Sync: KindSyncDecl{Mode: KindSyncPush},
			}}, "frontendAdapters": []map[string]any{
				{"key": "subsonic", "label": "Subsonic", "protocolHint": "subsonic", "pathPrefix": "/rest", "icon": "assets/subsonic.png"},
				{"key": "opds", "label": "OPDS", "protocolHint": "opds", "pathPrefix": "/opds"},
			}},
		},
	})
	_, err := env.m.Install(ctx, &Preview{
		Manifest:    mustParseTestManifest(t, raw),
		manifestRaw: raw,
		files: map[string][]byte{
			"manifest.json":        raw,
			"plugin.js":            []byte("export default {};"),
			"assets/icon.svg":      []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="12" fill="#412095"/></svg>`),
			"assets/subsonic.png":  pngBuf.Bytes(),
			"assets/recording.png": pngBuf.Bytes(),
		},
		source: Source{Kind: "pkg", ID: "community.example.adaptericon"},
	}, InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := env.m.store.record(ctx, "community.example.adaptericon")
	if err != nil {
		t.Fatal(err)
	}
	sub := env.m.AdapterIconURL(ctx, rec, "subsonic")
	if !strings.HasPrefix(sub, "/ether/api/plugins/community.example.adaptericon/icon?adapter=subsonic&v=1.2.0") {
		t.Fatalf("subsonic icon URL = %q", sub)
	}
	if got := env.m.AdapterIconURL(ctx, rec, "opds"); !strings.HasPrefix(got, "/ether/api/plugins/community.example.adaptericon/icon?v=1.2.0") {
		t.Fatalf("opds should fall back to the plugin icon, got %q", got)
	}

	kindIcon := env.m.CatalogueIconURL(ctx, rec, "recording")
	if !strings.Contains(kindIcon, "icon?kind=recording&") {
		t.Fatalf("catalogue should prefer the kind icon, got %q", kindIcon)
	}
	if got := env.m.CatalogueIconURL(ctx, rec, "missing"); got != env.m.declaredPluginIconURL(ctx, rec) {
		t.Fatalf("catalogue should fall back to ether.icon, got %q", got)
	}
	srv := env.m.Handler()
	for _, iconURL := range []string{sub, kindIcon} {
		res := httptest.NewRecorder()
		srv.ServeHTTP(res, httptest.NewRequest(http.MethodGet, strings.TrimPrefix(iconURL, "/ether/api/plugins"), nil))
		if res.Code != http.StatusOK || res.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("%s: status %d, type %q", iconURL, res.Code, res.Header().Get("Content-Type"))
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(res.Body.Bytes()))
		if err != nil || cfg.Width != 128 || cfg.Height != 128 {
			t.Fatalf("%s should serve a 128px PNG, got %+v (%v)", iconURL, cfg, err)
		}
	}
	for _, q := range []string{"adapter=opds", "adapter=missing", "kind=missing", "adapter=subsonic&kind=recording"} {
		res := httptest.NewRecorder()
		srv.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/community.example.adaptericon/icon?"+q, nil))
		if res.Code != http.StatusNotFound {
			t.Fatalf("icon selector %q: status %d, want 404", q, res.Code)
		}
	}
	ether := *rec.Manifest.Ether
	ether.Icon = ""
	rec.Manifest.Ether = &ether
	if got := env.m.CatalogueIconURL(ctx, rec, "missing"); got != "https://example.com/stremio.png" {
		t.Fatalf("catalogue should fall back to the Stremio logo, got %q", got)
	}
}

func TestFrontendAdapterIconPathValidated(t *testing.T) {
	for icon, ok := range map[string]bool{
		"assets/mark.svg": true, "assets/MARK.PNG": true,
		"../mark.svg": false, "/abs/mark.svg": false, "https://example.com/mark.svg": false, "assets/mark.webp": false,
	} {
		raw, _ := json.Marshal(map[string]any{
			"id": "community.example.adapterpath", "version": "1.0.0", "name": "Adapter Path",
			"ether": map[string]any{
				"protocol": 1, "kind": "extension", "summary": "Serves one protocol.",
				"runtime":      map[string]any{"kind": "hosted", "entry": "plugin.js"},
				"capabilities": []string{"frontend_adapter"},
				"contributes": map[string]any{"frontendAdapters": []map[string]any{
					{"key": "feed", "label": "Feed", "protocolHint": "custom", "pathPrefix": "/feed", "icon": icon},
				}},
			},
		})
		man, err := ParseManifest(raw)
		if err != nil {
			t.Fatal(err)
		}
		err = man.Validate()
		if ok != (err == nil || !strings.Contains(err.Error(), "frontendAdapters[0].icon")) {
			t.Fatalf("icon %q: valid=%v, err=%v", icon, ok, err)
		}
	}
}
