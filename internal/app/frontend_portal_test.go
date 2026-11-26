package app

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/remote"
)

func TestPortalAddresses(t *testing.T) {
	_, network, _ := net.ParseCIDR("192.168.1.0/24")
	lan := []remote.InterfaceAddr{{IP: net.ParseIP("192.168.1.5"), Net: network}}
	got, err := portalLANAddress(netip.MustParseAddr("192.168.1.9"), "0.0.0.0", "32400", lan)
	if err != nil || got != "http://192.168.1.5:32400/ether/me" {
		t.Fatalf("LAN %q %v", got, err)
	}
	if _, err = portalLANAddress(netip.MustParseAddr("192.168.2.9"), "0.0.0.0", "32400", lan); err == nil {
		t.Fatal("unreachable subnet selected")
	}
	if _, err = portalLANAddress(netip.MustParseAddr("192.168.1.9"), "192.168.1.6", "32400", lan); err == nil {
		t.Fatal("unbound interface selected")
	}
	candidates := []remote.Connection{{URI: "http://192.168.1.5:32400", Local: true}, {URI: "http://172.17.0.1:32400"}, {URI: "https://8.8.8.8:443"}}
	got, err = portalPublicAddress(context.Background(), candidates)
	if err != nil || got != "https://8.8.8.8:443/ether/me" {
		t.Fatalf("public %q %v", got, err)
	}
}

func TestPortalAndSurfaceBridgeModeUsesRequestHostAndAppPort(t *testing.T) {
	_, bridgeNet, _ := net.ParseCIDR("172.20.0.0/16")
	containerInterfaces := []remote.InterfaceAddr{{IP: net.ParseIP("172.20.0.2"), Net: bridgeNet}}
	peer := netip.MustParseAddr("192.168.68.10")

	req := httptest.NewRequest(http.MethodPost, "http://192.168.68.66:33200/ether/api/me/apps/jellyfin", nil)
	req.RemoteAddr = "172.20.0.1:54321"
	got, err := portalAddress(context.Background(), req, peer, "0.0.0.0", "33200", config.Config{}, nil, containerInterfaces, nil)
	if err != nil || got != "http://192.168.68.66:33200/ether/me" {
		t.Fatalf("bridge portal: got %q err %v", got, err)
	}

	local := &net.TCPAddr{IP: net.ParseIP("172.20.0.2"), Port: 8096}
	gotSurface, ok := surfaceAddress(req, peer, local, []string{"0.0.0.0:8096"}, []string{""}, nil, containerInterfaces)
	if !ok || gotSurface != "http://192.168.68.66:8096" {
		t.Fatalf("bridge jellyfin listener: got %q ok %v", gotSurface, ok)
	}
}

func TestPortalAndSurfaceReverseProxyHonoursTrustedHeaders(t *testing.T) {
	_, bridgeNet, _ := net.ParseCIDR("172.20.0.0/16")
	containerInterfaces := []remote.InterfaceAddr{{IP: net.ParseIP("172.20.0.2"), Net: bridgeNet}}
	trusted := []netip.Prefix{netip.MustParsePrefix("172.20.0.0/16")}
	peer := netip.MustParseAddr("198.51.100.25")

	req := httptest.NewRequest(http.MethodPost, "http://172.20.0.2:32400/ether/api/me/apps/jellyfin", nil)
	req.RemoteAddr = "172.20.0.5:41000"
	req.Header.Set("X-Forwarded-Host", "ether.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")

	got, err := portalAddress(context.Background(), req, peer, "0.0.0.0", "32400", config.Config{}, trusted, containerInterfaces, nil)
	if err != nil || got != "https://ether.example.com/ether/me" {
		t.Fatalf("trusted proxy portal: got %q err %v", got, err)
	}

	local := &net.TCPAddr{IP: net.ParseIP("172.20.0.2"), Port: 8096}
	gotSurface, ok := surfaceAddress(req, peer, local, []string{"0.0.0.0:8096"}, []string{""}, trusted, containerInterfaces)
	if !ok || gotSurface != "https://ether.example.com" {
		t.Fatalf("trusted proxy surface: got %q ok %v", gotSurface, ok)
	}

	// Trusted proxy with path prefix must carry the prefix automatically.
	reqPrefix := httptest.NewRequest(http.MethodPost, "http://172.20.0.2:32400/ether/api/me/apps/jellyfin", nil)
	reqPrefix.RemoteAddr = "172.20.0.5:41001"
	reqPrefix.Header.Set("X-Forwarded-Host", "media.example.com")
	reqPrefix.Header.Set("X-Forwarded-Proto", "https")
	reqPrefix.Header.Set("X-Forwarded-Prefix", "/ether-app")
	got, err = portalAddress(context.Background(), reqPrefix, peer, "0.0.0.0", "32400", config.Config{}, trusted, containerInterfaces, nil)
	if err != nil || got != "https://media.example.com/ether-app/ether/me" {
		t.Fatalf("trusted proxy prefix portal: got %q err %v", got, err)
	}
	gotSurface, ok = surfaceAddress(reqPrefix, peer, local, []string{"0.0.0.0:8096"}, []string{""}, trusted, containerInterfaces)
	if !ok || gotSurface != "https://media.example.com/ether-app" {
		t.Fatalf("trusted proxy prefix surface: got %q ok %v", gotSurface, ok)
	}

	// Untrusted proxy headers must be ignored.
	untrustedReq := httptest.NewRequest(http.MethodPost, "http://192.168.68.66:32400/ether/api/me/apps/jellyfin", nil)
	untrustedReq.RemoteAddr = "192.168.68.10:41000"
	untrustedReq.Header.Set("X-Forwarded-Host", "evil.example.com")
	untrustedReq.Header.Set("X-Forwarded-Proto", "https")
	untrustedReq.Header.Set("X-Forwarded-Prefix", "/spoof")

	got, err = portalAddress(context.Background(), untrustedReq, netip.MustParseAddr("192.168.68.10"), "0.0.0.0", "32400", config.Config{}, trusted, containerInterfaces, nil)
	if err != nil || got != "http://192.168.68.66:32400/ether/me" {
		t.Fatalf("untrusted proxy portal: got %q err %v", got, err)
	}
}

func TestPortalPrefersRequestAndFallsBackToConfiguredURL(t *testing.T) {
	_, bridgeNet, _ := net.ParseCIDR("172.20.0.0/16")
	containerInterfaces := []remote.InterfaceAddr{{IP: net.ParseIP("172.20.0.2"), Net: bridgeNet}}
	peer := netip.MustParseAddr("192.168.68.10")

	// Request-derived wins when the request carries a usable host.
	req := httptest.NewRequest(http.MethodPost, "http://192.168.68.66:32400/ether/api/me/apps/jellyfin", nil)
	req.RemoteAddr = "172.20.0.1:54321"
	cfg := config.Config{Plex: config.Plex{CustomURLs: []string{"https://watch.example.com:8443"}}}
	got, err := portalAddress(context.Background(), req, peer, "0.0.0.0", "32400", cfg, nil, containerInterfaces, nil)
	if err != nil || got != "http://192.168.68.66:32400/ether/me" {
		t.Fatalf("request-derived portal: got %q err %v", got, err)
	}

	// Without a usable request host or matching LAN interface, configured URLs are used.
	remotePeer := netip.MustParseAddr("198.51.100.25")
	containerReq := httptest.NewRequest(http.MethodPost, "http://172.20.0.2:32400/ether/api/me/apps/jellyfin", nil)
	containerReq.RemoteAddr = "198.51.100.25:54321"
	got, err = portalAddress(context.Background(), containerReq, remotePeer, "0.0.0.0", "32400", cfg, nil, containerInterfaces, nil)
	if err != nil || got != "https://watch.example.com:8443/ether/me" {
		t.Fatalf("custom URL fallback portal: got %q err %v", got, err)
	}

	cfgPublic := config.Config{Plex: config.Plex{PublicHost: "media.example.com", PublicPort: 443}}
	got, err = portalAddress(context.Background(), containerReq, remotePeer, "0.0.0.0", "32400", cfgPublic, nil, containerInterfaces, nil)
	if err != nil || got != "https://media.example.com/ether/me" {
		t.Fatalf("public host fallback portal: got %q err %v", got, err)
	}
}

func TestSecurePortalPrefersReachableLocalPlexDirect(t *testing.T) {
	_, network, _ := net.ParseCIDR("192.168.1.0/24")
	interfaces := []remote.InterfaceAddr{{IP: net.ParseIP("192.168.1.5"), Net: network}}
	connections := []remote.Connection{
		{URI: "https://8.8.8.8:443"},
		{URI: "https://192-168-1-5.hash.plex.direct:32400", Local: true},
	}
	got, err := portalSecureAddress(context.Background(), netip.MustParseAddr("192.168.1.9"), "0.0.0.0", "32400", interfaces, connections)
	if err != nil || got != "https://192-168-1-5.hash.plex.direct:32400/ether/me" {
		t.Fatalf("local: %q %v", got, err)
	}
	got, err = portalSecureAddress(context.Background(), netip.MustParseAddr("192.168.2.9"), "0.0.0.0", "32400", interfaces, connections)
	if err != nil || got != "https://8.8.8.8:443/ether/me" {
		t.Fatalf("remote: %q %v", got, err)
	}
	if _, err = portalSecureAddress(context.Background(), netip.MustParseAddr("192.168.1.9"), "0.0.0.0", "32400", interfaces, []remote.Connection{{URI: "http://192.168.1.5:32400", Local: true}}); err == nil {
		t.Fatal("accepted insecure address")
	}
	got, err = portalSecureAddress(context.Background(), netip.MustParseAddr("192.168.1.9"), "192.168.1.6", "32400", interfaces, connections)
	if err != nil || got != "https://8.8.8.8:443/ether/me" {
		t.Fatalf("unbound local: %q %v", got, err)
	}
}

func TestSecurePortalLoopbackUsesServerLANPlexDirect(t *testing.T) {
	_, network, _ := net.ParseCIDR("192.168.68.0/24")
	_, other, _ := net.ParseCIDR("10.0.0.0/24")
	interfaces := []remote.InterfaceAddr{
		{IP: net.ParseIP("192.168.68.65"), Net: network},
		{IP: net.ParseIP("10.0.0.4"), Net: other},
	}
	connections := []remote.Connection{
		{URI: "https://8.8.8.8:443"},
		{URI: "http://192.168.68.65:32400", Local: true},
		{URI: "https://10-0-0-4.hash.plex.direct:32400", Local: true},
		{URI: "https://192-168-68-65.hash.plex.direct:32400", Local: true},
	}
	want := "https://192-168-68-65.hash.plex.direct:32400/ether/me"
	for _, peer := range []string{"127.0.0.1", "::1"} {
		got, err := portalSecureAddress(context.Background(), netip.MustParseAddr(peer), "0.0.0.0", "32400", interfaces, connections)
		if err != nil || got != want {
			t.Fatalf("peer %s: %q %v", peer, got, err)
		}
	}
	got, err := portalSecureAddress(context.Background(), netip.MustParseAddr("127.0.0.1"), "10.0.0.4", "32400", interfaces, connections)
	if err != nil || got != "https://10-0-0-4.hash.plex.direct:32400/ether/me" {
		t.Fatalf("bound interface: %q %v", got, err)
	}
	localOnly := []remote.Connection{{URI: "https://192-168-68-65.hash.plex.direct:32400", Local: true}}
	if _, err = portalSecureAddress(context.Background(), netip.MustParseAddr("127.0.0.1"), "127.0.0.1", "32400", interfaces, localOnly); err == nil {
		t.Fatal("loopback-only listener advertised a LAN certificate")
	}
	if _, err = portalSecureAddress(context.Background(), netip.MustParseAddr("127.0.0.1"), "0.0.0.0", "32400", interfaces, []remote.Connection{{URI: "http://192.168.68.65:32400", Local: true}}); err == nil {
		t.Fatal("accepted insecure address for loopback")
	}
}

func TestSurfaceLANAddressUsesActualBoundPort(t *testing.T) {
	_, network, _ := net.ParseCIDR("192.168.1.0/24")
	interfaces := []remote.InterfaceAddr{{IP: net.ParseIP("192.168.1.5"), Net: network}}
	peer := netip.MustParseAddr("192.168.1.9")
	local := &net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 8097}
	got, ok := surfaceLANAddress(peer, local, []string{"0.0.0.0:8097"}, []string{""}, interfaces)
	if !ok || got != "http://192.168.1.5:8097" {
		t.Fatalf("dedicated port: %q %v", got, ok)
	}
	if _, ok = surfaceLANAddress(peer, &net.TCPAddr{IP: local.IP, Port: 32400}, []string{"0.0.0.0:8097"}, []string{""}, interfaces); ok {
		t.Fatal("accepted different listener")
	}
	if _, ok = surfaceLANAddress(netip.MustParseAddr("8.8.8.8"), local, []string{"0.0.0.0:8097"}, []string{""}, interfaces); ok {
		t.Fatal("exposed LAN URL to remote peer")
	}
}

func TestSurfaceSharedAddressPreservesPublishedPrefix(t *testing.T) {
	got, err := surfaceSharedAddress("https://ether.example/base/ether/me", []string{"/jellyfin"})
	if err != nil || got != "https://ether.example/base/jellyfin" {
		t.Fatalf("shared address: %q %v", got, err)
	}
	if _, err := surfaceSharedAddress("https://ether.example/base/ether/me", nil); err == nil {
		t.Fatal("missing shared mount accepted")
	}
	if _, err := surfaceSharedAddress("https://ether.example/base", []string{"/jellyfin"}); err == nil {
		t.Fatal("invalid portal path accepted")
	}
}

func TestPortalLoopbackUsesVerifiedListener(t *testing.T) {
	for _, tc := range []struct{ peer, host, want string }{
		{"127.0.0.1", "127.0.0.1", "http://127.0.0.1:1234/ether/me"},
		{"127.0.0.1", "0.0.0.0", "http://127.0.0.1:1234/ether/me"},
		{"::1", "::1", "http://[::1]:1234/ether/me"},
		{"127.0.0.1", "192.168.1.5", ""},
		{"192.168.1.5", "127.0.0.1", ""},
		{"198.51.100.2", "0.0.0.0", ""},
		{"192.168.1.5", "0.0.0.0", ""},
		{"2001:db8::5", "::", ""},
	} {
		got, err := portalLANAddress(netip.MustParseAddr(tc.peer), tc.host, "1234", nil)
		if got != tc.want || (err == nil) != (tc.want != "") {
			t.Fatalf("peer=%s bound=%s got=%q err=%v", tc.peer, tc.host, got, err)
		}
	}
}

func TestAppAddressUnifiedPrecedenceAndEsmailSetup(t *testing.T) {
	dir := t.TempDir()
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *config.Config) error {
		c.Server.TrustedProxyCIDRs = []string{"172.20.0.0/16"}
		c.Plex.CustomURLs = []string{"https://dashboard.example.com/"}
		c.Frontends = map[string]config.Frontend{
			"jellyfin": {Enabled: true, PublicURL: "https://jellyfin.example.com/"},
			"emby":     {Enabled: true, PublishedAddresses: []string{"https://emby.example.com/"}},
			"silo":     {Enabled: true},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	listeners := newFrontendListeners(nil)
	listeners.running["jellyfin"] = &runningFrontend{sharedPrefixes: []string{"/jellyfin"}}
	listeners.running["emby"] = &runningFrontend{sharedPrefixes: []string{"/emby"}}
	listeners.running["silo"] = &runningFrontend{sharedPrefixes: []string{"/silo"}}

	rt := &frontendRuntime{
		app:      &App{cfg: store},
		control:  &frontendControl{listeners: listeners},
		mainAddr: &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 32400},
	}

	// Portal / console request arriving at dashboard.example.com (NOT on jellyfin's surface):
	// Tier 2 wins for jellyfin & emby (their published/publicUrl addresses), while
	// silo has no published address and falls back to Tier 3 (server address + /silo).
	portalReq := httptest.NewRequest(http.MethodPost, "http://172.20.0.2:32400/ether/api/me/apps/jellyfin", nil)
	portalReq.RemoteAddr = "172.20.0.5:41000"
	portalReq.Header.Set("X-Forwarded-Host", "dashboard.example.com")
	portalReq.Header.Set("X-Forwarded-Proto", "https")

	if got, err := rt.appAddress(context.Background(), "jellyfin", portalReq, false); err != nil || got != "https://jellyfin.example.com" {
		t.Fatalf("portal jellyfin = %q err=%v, want https://jellyfin.example.com", got, err)
	}
	if got, err := rt.appAddress(context.Background(), "emby", portalReq, false); err != nil || got != "https://emby.example.com" {
		t.Fatalf("portal emby = %q err=%v, want https://emby.example.com", got, err)
	}
	if got, err := rt.appAddress(context.Background(), "silo", portalReq, false); err != nil || got != "https://dashboard.example.com/silo" {
		t.Fatalf("portal silo fallback = %q err=%v, want https://dashboard.example.com/silo", got, err)
	}

	// Tier 1: live request ON jellyfin's own surface derives from that request even
	// when a different published address is configured.
	jfReq := httptest.NewRequest(http.MethodGet, "http://172.20.0.2:8096/System/Info/Public", nil)
	jfReq.RemoteAddr = "172.20.0.5:41001"
	jfReq.Header.Set("X-Forwarded-Host", "alt.example.com")
	jfReq.Header.Set("X-Forwarded-Proto", "https")
	jfReq.Header.Set("X-Forwarded-Prefix", "/jf")
	jfReq = jfReq.WithContext(frontend.WithMountPrefix(jfReq.Context(), "jellyfin", ""))

	if got, err := rt.appAddress(context.Background(), "jellyfin", jfReq, false); err != nil || got != "https://alt.example.com/jf" {
		t.Fatalf("in-app jellyfin request-derived = %q err=%v, want https://alt.example.com/jf", got, err)
	}

	// Direct loopback request on jellyfin's own surface also derives cleanly.
	loopReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8096/System/Info/Public", nil)
	loopReq.RemoteAddr = "127.0.0.1:51000"
	loopReq = loopReq.WithContext(frontend.WithMountPrefix(loopReq.Context(), "jellyfin", ""))
	if got, err := rt.appAddress(context.Background(), "jellyfin", loopReq, false); err != nil || got != "http://127.0.0.1:8096" {
		t.Fatalf("loopback in-app jellyfin = %q err=%v, want http://127.0.0.1:8096", got, err)
	}

	// Out-of-band (req == nil): Tier 2 published address first, then Tier 3 server + path.
	if got, err := rt.appAddress(context.Background(), "jellyfin", nil, false); err != nil || got != "https://jellyfin.example.com" {
		t.Fatalf("out-of-band jellyfin = %q err=%v", got, err)
	}
	if got, err := rt.appAddress(context.Background(), "silo", nil, false); err != nil || got != "https://dashboard.example.com/silo" {
		t.Fatalf("out-of-band silo = %q err=%v", got, err)
	}
}

func TestStremioAppAddressKeepsMountAfterPublicPrefix(t *testing.T) {
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	listeners := newFrontendListeners(nil)
	listeners.running["stremio"] = &runningFrontend{sharedPrefixes: []string{"/stremio"}}
	rt := &frontendRuntime{
		app:      &App{cfg: store},
		control:  &frontendControl{listeners: listeners},
		mainAddr: &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 32400},
	}
	consoleReq := httptest.NewRequest(http.MethodPost, "http://ether.example/ether/api/me/apps/stremio", nil)
	for _, tc := range []struct{ published, want string }{
		{"https://addon.example", "https://addon.example/stremio"},
		{"https://addon.example/media/", "https://addon.example/media/stremio"},
		{"https://addon.example/media/stremio/", "https://addon.example/media/stremio"},
	} {
		if err := store.Update(func(c *config.Config) error {
			c.Frontends = map[string]config.Frontend{
				"stremio": {Enabled: true, PublishedAddresses: []string{tc.published}},
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if got, err := rt.appAddress(t.Context(), "stremio", consoleReq, true); err != nil || got != tc.want {
			t.Errorf("console base for %q = %q, err = %v, want %q", tc.published, got, err, tc.want)
		}
		if got, err := rt.surfaceURL(t.Context(), "stremio", nil); err != nil || got != tc.want {
			t.Errorf("published surface base for %q = %q, err = %v, want %q", tc.published, got, err, tc.want)
		}
	}
	proxyReq := httptest.NewRequest(http.MethodGet, "http://ether.example/stremio/image/example", nil)
	proxyReq.RemoteAddr = "127.0.0.1:12345"
	proxyReq.Header.Set("X-Forwarded-Proto", "https")
	proxyReq.Header.Set("X-Forwarded-Host", "proxy.example")
	proxyReq = proxyReq.WithContext(frontend.WithMountPrefix(proxyReq.Context(), "stremio", "/stremio"))
	for _, prefix := range []string{"/media", "/media/stremio"} {
		proxyReq.Header.Set("X-Forwarded-Prefix", prefix)
		if got, err := rt.surfaceURL(t.Context(), "stremio", proxyReq); err != nil || got != "https://proxy.example/media/stremio" {
			t.Errorf("proxied surface base for %q = %q, err = %v", prefix, got, err)
		}
	}
}

func TestAppAddressVelaDedicatedAndConsolePort(t *testing.T) {
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *config.Config) error {
		c.Server.Port = 32400
		c.Server.ConsolePort = 8099
		c.Server.DisableConsoleOnMainPort = true
		c.Server.TrustedProxyCIDRs = []string{"172.20.0.0/16"}
		c.Plex.CustomURLs = []string{"https://plex.example.com"}
		c.FrontendPorts = map[string]int{"vela": 8098}
		c.Frontends = map[string]config.Frontend{
			"vela":     {Enabled: true, Settings: map[string]any{"serveOnMainPort": false}},
			"jellyfin": {Enabled: true},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	velaLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer velaLn.Close()
	velaPort := velaLn.Addr().(*net.TCPAddr).Port

	listeners := newFrontendListeners(nil)
	listeners.running["vela"] = &runningFrontend{
		listeners: []net.Listener{velaLn},
		prefixes:  []string{""},
	}
	listeners.running["jellyfin"] = &runningFrontend{sharedPrefixes: []string{"/jellyfin"}}

	rt := &frontendRuntime{
		app:           &App{cfg: store},
		control:       &frontendControl{listeners: listeners},
		mainAddr:      &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 32400},
		consoleAddr:   &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 8099},
		consoleOnMain: false,
	}

	// Console request arriving on dedicated console port 8099 over LAN:
	// - viewerPortalURL resolves :8099/ether/me
	// - Vela (dedicated-only, serveOnMainPort=false) resolves its dedicated port (never Plex :32400/vela)
	// - Jellyfin (shared on main port) resolves main port :32400/jellyfin (never console :8099)
	consoleReq := httptest.NewRequest(http.MethodGet, "http://192.168.1.5:8099/ether/api/web-player", nil)
	consoleReq.RemoteAddr = "192.168.1.20:51234"
	consoleReq = consoleReq.WithContext(context.WithValue(consoleReq.Context(), http.LocalAddrContextKey, rt.consoleAddr))

	if got, err := rt.viewerPortalURL(context.Background(), consoleReq); err != nil || got != "http://192.168.1.5:8099/ether/me" {
		t.Fatalf("viewerPortalURL on dedicated console port = %q err=%v", got, err)
	}
	wantVela := "http://192.168.1.5:" + velaLn.Addr().String()[len("127.0.0.1:"):]
	// dedicatedAppAddress uses loopback-only guard when bound to 127.0.0.1; verify loopback request gets loopback dedicated port.
	loopConsoleReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8099/ether/api/web-player", nil)
	loopConsoleReq.RemoteAddr = "127.0.0.1:51234"
	loopConsoleReq = loopConsoleReq.WithContext(context.WithValue(loopConsoleReq.Context(), http.LocalAddrContextKey, rt.consoleAddr))
	_ = wantVela
	wantLoopVela := "http://127.0.0.1:" + velaLn.Addr().String()[len("127.0.0.1:"):]
	if got, err := rt.appAddress(context.Background(), "vela", loopConsoleReq, false); err != nil || got != wantLoopVela {
		t.Fatalf("vela dedicated-only appAddress = %q err=%v, want %q", got, err, wantLoopVela)
	}
	if got, err := rt.appAddress(context.Background(), "jellyfin", consoleReq, false); err != nil || got != "http://192.168.1.5:32400/jellyfin" {
		t.Fatalf("jellyfin appAddress from dedicated console port = %q err=%v, want http://192.168.1.5:32400/jellyfin", got, err)
	}

	// Remote request for dedicated-only Vela with NO published address must NOT leak disabled Plex port /vela.
	remoteReq := httptest.NewRequest(http.MethodGet, "http://172.20.0.2:8099/ether/api/web-player", nil)
	remoteReq.RemoteAddr = "172.20.0.5:41000"
	remoteReq.Header.Set("X-Forwarded-Host", "admin.example.com")
	remoteReq.Header.Set("X-Forwarded-Proto", "https")
	remoteReq.Header.Set("X-Forwarded-For", "198.51.100.9")
	remoteReq = remoteReq.WithContext(context.WithValue(remoteReq.Context(), http.LocalAddrContextKey, rt.consoleAddr))
	if got, err := rt.appAddress(context.Background(), "vela", remoteReq, false); err == nil {
		t.Fatalf("remote dedicated-only vela without published address = %q, want error instead of disabled main port", got)
	}

	// Once Vela has its own published HTTPS subdomain, W189 resolver returns it and secureOriginsFor uses it.
	if err := store.Update(func(c *config.Config) error {
		v := c.Frontends["vela"]
		v.PublishedAddresses = []string{"https://vela.example.com"}
		c.Frontends["vela"] = v
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := rt.appAddress(context.Background(), "vela", remoteReq, false); err != nil || got != "https://vela.example.com" {
		t.Fatalf("vela published appAddress = %q err=%v, want https://vela.example.com", got, err)
	}
	if origins := rt.secureOriginsFor(context.Background(), "vela", remoteReq); len(origins) != 1 || origins[0] != "https://vela.example.com" {
		t.Fatalf("vela secureOriginsFor = %#v, want [https://vela.example.com]", origins)
	}
	_ = velaPort
}
