package plex

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/clientnet"
	"github.com/braedonsaunders/ether/internal/resolver"
)

func TestTrustedLANProxyClientIPAndSessions(t *testing.T) {
	// ParseProxyEntry accepts both bare IPs and CIDRs, and rejects wildcards/invalid input.
	for _, valid := range []string{"172.16.50.5", "172.16.50.5/32", "10.20.0.0/24", "fd00::5"} {
		if _, err := clientnet.ParseProxyEntry(valid); err != nil {
			t.Fatalf("ParseProxyEntry(%q) unexpected error: %v", valid, err)
		}
	}
	for _, invalid := range []string{"0.0.0.0/0", "::/0", "caddy.lan", "172.16.50.5/99", ""} {
		if _, err := clientnet.ParseProxyEntry(invalid); err == nil {
			t.Fatalf("ParseProxyEntry(%q) expected error, got nil", invalid)
		}
	}
	// Host names are resolved (see clientnet proxyhosts_test.go); malformed
	// entries still grant nothing.
	if got := clientnet.ProxyPrefixes([]string{"0.0.0.0/0", "::/0", "caddy lan", "http://"}); len(got) != 0 {
		t.Fatalf("wildcard/invalid ProxyPrefixes granted trust: %v", got)
	}

	quality := config.Quality{
		Default:     "home",
		DefaultAway: "away",
		Profiles: []core.QualityProfile{
			{Name: "home", MaxHeight: 2160},
			{Name: "away", MaxHeight: 720},
		},
	}

	// Configure Caddy on a separate LAN host using a bare IP address (no /32 required).
	f := newFixture(func(d *testServerDeps) {
		setDepsConfig(d, func() config.Config {
			c := config.Config{}
			c.Server.Port = 32400
			c.Server.FriendlyName = "Ether Test"
			c.Server.MachineIdentifier = "0123456789abcdef0123456789abcdef01234567"
			c.Server.TrustedProxyCIDRs = []string{"172.16.50.5"}
			c.Plex.Claimed = true
			c.Plex.OwnerUsername = "owner"
			c.Quality = quality
			c.Transcode.RemoteMaxBitrateKbps = 8000
			return c
		})
	})
	defer f.srv.Close()

	trusted := clientnet.ProxyPrefixes([]string{"172.16.50.5"})

	// Verify X-Real-IP, Forwarded, and chained trusted LAN proxy handling + Home/Away profile selection.
	xRealReq := httptest.NewRequest(http.MethodGet, "/", nil)
	xRealReq.RemoteAddr = "172.16.50.5:41236"
	xRealReq.Header.Set("X-Real-IP", "203.0.113.55")
	if pr := ParseRequest(xRealReq, trusted...); !pr.Remote || pr.RemoteIP.String() != "203.0.113.55" {
		t.Fatalf("X-Real-IP from trusted LAN proxy = %s remote=%v", pr.RemoteIP, pr.Remote)
	}

	fwdReq := httptest.NewRequest(http.MethodGet, "/", nil)
	fwdReq.RemoteAddr = "172.16.50.5:41237"
	fwdReq.Header.Set("Forwarded", "for=203.0.113.56;proto=https")
	if pr := ParseRequest(fwdReq, trusted...); !pr.Remote || !pr.Secure || pr.RemoteIP.String() != "203.0.113.56" {
		t.Fatalf("Forwarded from trusted LAN proxy = %s remote=%v secure=%v", pr.RemoteIP, pr.Remote, pr.Secure)
	}

	lanChainReq := httptest.NewRequest(http.MethodGet, "/", nil)
	lanChainReq.RemoteAddr = "127.0.0.1:41238"
	lanChainReq.Header.Set("X-Forwarded-For", "172.16.50.20, 172.16.50.5")
	lanPR := ParseRequest(lanChainReq, trusted...)
	if lanPR.Remote || lanPR.RemoteIP.String() != "172.16.50.20" {
		t.Fatalf("chained trusted LAN proxy = %s remote=%v, want 172.16.50.20 remote=false", lanPR.RemoteIP, lanPR.Remote)
	}
	if prof := resolver.SelectProfile(quality, lanPR.ClientCaps(0), &ownerUser); prof.Name != "home" {
		t.Fatalf("LAN client profile = %q, want home", prof.Name)
	}

	// Send decision (without location=wan) and timeline requests through Caddy, plus spoofing attempts.
	sendSession := func(remoteAddr, clientID, sessionID, token string, hdrs map[string]string) {
		for _, target := range []string{
			"/video/:/transcode/universal/decision?path=/library/metadata/1000000000000550&protocol=hls&directPlay=1&directStream=1&session=" + sessionID,
			"/:/timeline?ratingKey=1000000000000550&state=playing&time=10000&duration=8340000",
		} {
			req := httptest.NewRequest(http.MethodGet, target, nil)
			req.RemoteAddr = remoteAddr
			req.Header.Set("X-Plex-Token", token)
			req.Header.Set("X-Plex-Client-Identifier", clientID)
			req.Header.Set("X-Plex-Session-Identifier", sessionID)
			req.Header.Set("X-Plex-Product", "Plex Web")
			for k, v := range hdrs {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			f.srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s (%s): status %d %s", clientID, target, rec.Code, rec.Body.String())
			}
		}
	}

	// 1. Remote client through trusted Caddy on 172.16.50.5 -> remote IP, local=0, secure=1, location=wan, Away profile.
	remoteReq := httptest.NewRequest(http.MethodGet, "/", nil)
	remoteReq.RemoteAddr = "172.16.50.5:41234"
	remoteReq.Header.Set("X-Forwarded-For", "198.51.100.42")
	remoteReq.Header.Set("X-Forwarded-Proto", "https")
	remotePR := ParseRequest(remoteReq, trusted...)
	if prof := resolver.SelectProfile(quality, remotePR.ClientCaps(8000), &friendUser); prof.Name != "away" {
		t.Fatalf("remote client via trusted proxy profile = %q, want away", prof.Name)
	}
	sendSession("172.16.50.5:41234", "caddy-wan", "sess-caddy-wan", friendToken, map[string]string{
		"X-Forwarded-For":   "198.51.100.42",
		"X-Forwarded-Proto": "https",
	})

	// 2. Remote client spoofing a LAN hop ahead of its real IP behind Caddy -> spoofed LAN hop ignored.
	sendSession("172.16.50.5:41235", "caddy-spoof-lan", "sess-caddy-spoof", friendToken, map[string]string{
		"X-Forwarded-For":   "192.168.1.9, 198.51.100.77",
		"X-Forwarded-Proto": "https",
	})

	// 3. Untrusted LAN peer trying to spoof forwarding headers -> headers ignored, peer IP kept.
	sendSession("172.16.50.99:50000", "untrusted-lan", "sess-untrusted-lan", ownerToken, map[string]string{
		"X-Forwarded-For":   "198.51.100.42",
		"X-Forwarded-Proto": "https",
	})

	// 4. Untrusted WAN peer trying to spoof a LAN IP -> headers ignored, public peer IP kept.
	sendSession("203.0.113.9:50001", "untrusted-wan", "sess-untrusted-wan", friendToken, map[string]string{
		"X-Forwarded-For":   "192.168.1.10",
		"X-Forwarded-Proto": "https",
	})

	// Tautulli polling /status/sessions directly from another LAN host sees accurate client IPs and locations.
	poll := httptest.NewRequest(http.MethodGet, "/status/sessions", nil)
	poll.RemoteAddr = "172.16.50.9:52000"
	poll.Header.Set("X-Plex-Token", ownerToken)
	for i := 0; i < len(tautulliHeaders); i += 2 {
		poll.Header.Set(tautulliHeaders[i], tautulliHeaders[i+1])
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, poll)
	if rec.Code != http.StatusOK {
		t.Fatalf("/status/sessions: %d %s", rec.Code, rec.Body.String())
	}

	type videoSession struct {
		Player struct {
			MachineID           string `xml:"machineIdentifier,attr"`
			Address             string `xml:"address,attr"`
			RemotePublicAddress string `xml:"remotePublicAddress,attr"`
			Local               string `xml:"local,attr"`
			Secure              string `xml:"secure,attr"`
		} `xml:"Player"`
		Session struct {
			Location string `xml:"location,attr"`
		} `xml:"Session"`
	}
	var doc struct {
		Videos []videoSession `xml:"Video"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal /status/sessions: %v\n%s", err, rec.Body.String())
	}

	byClient := map[string]videoSession{}
	for _, v := range doc.Videos {
		byClient[v.Player.MachineID] = v
	}

	want := map[string]struct {
		addr, remotePub, local, secure, location string
	}{
		"caddy-wan":       {"198.51.100.42", "198.51.100.42", "0", "1", "wan"},
		"caddy-spoof-lan": {"198.51.100.77", "198.51.100.77", "0", "1", "wan"},
		"untrusted-lan":   {"172.16.50.99", "", "1", "0", "lan"},
		"untrusted-wan":   {"203.0.113.9", "203.0.113.9", "0", "0", "wan"},
	}
	for id, w := range want {
		got, ok := byClient[id]
		if !ok {
			t.Fatalf("missing session for %s in /status/sessions", id)
		}
		if got.Player.Address != w.addr ||
			got.Player.RemotePublicAddress != w.remotePub ||
			got.Player.Local != w.local ||
			got.Player.Secure != w.secure ||
			got.Session.Location != w.location {
			t.Fatalf("%s: got player=%+v session=%+v, want %+v", id, got.Player, got.Session, w)
		}
	}
}
