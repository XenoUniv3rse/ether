package clientnet

import (
	"context"
	"net"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeProxyDNS swaps the proxy-name resolver for a table the test controls.
func fakeProxyDNS(t *testing.T, table map[string][]netip.Addr) *sync.Mutex {
	t.Helper()
	var mu sync.Mutex
	prev := lookupProxyHost
	lookupProxyHost = func(_ context.Context, host string) ([]netip.Addr, error) {
		mu.Lock()
		defer mu.Unlock()
		addrs, ok := table[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return addrs, nil
	}
	resetProxyHosts()
	t.Cleanup(func() {
		lookupProxyHost = prev
		resetProxyHosts()
	})
	return &mu
}

func TestValidateProxyEntryAcceptsAddressesRangesAndNames(t *testing.T) {
	for _, ok := range []string{"192.168.1.5", "10.0.0.0/8", "fd00::/8", "nginx-proxy-manager", "npm", "proxy.lan", "caddy_1", "NPM.",
		// Written the way a reverse proxy's own config names an upstream.
		"http://nginx-proxy-manager:81/health?ready=1#ok", "https://npm", "npm:81", "http://172.18.0.2:81", "[fd00::5]:443"} {
		if err := ValidateProxyEntry(ok); err != nil {
			t.Errorf("ValidateProxyEntry(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"", "0.0.0.0/0", "::/0", "1.2.3", "-proxy", "prox y", "proxy..lan", "http://", "http://prox y:81", "[fd00::/8]:443", "npm:broken", "10.0.0.0/33"} {
		if err := ValidateProxyEntry(bad); err == nil {
			t.Errorf("ValidateProxyEntry(%q) = nil, want an error", bad)
		}
	}
	if !HasProxyHostNames([]string{"10.0.0.1", "npm"}) || !HasProxyHostNames([]string{"http://npm:81"}) ||
		HasProxyHostNames([]string{"10.0.0.1", "10.0.0.0/8", "http://10.0.0.2:81"}) {
		t.Fatal("HasProxyHostNames must report only real names")
	}
}

func TestProxyPrefixesResolvesContainerNames(t *testing.T) {
	fakeProxyDNS(t, map[string][]netip.Addr{
		"nginx-proxy-manager": {
			netip.MustParseAddr("::ffff:172.18.0.2"),
			netip.MustParseAddr("172.18.0.2"),
			netip.MustParseAddr("fd00::2"),
			netip.MustParseAddr("fe80::1"),
			netip.MustParseAddr("::ffff:203.0.113.1"),
			netip.MustParseAddr("2001:db8::1"),
			netip.MustParseAddr("100.64.1.2"),
		},
		"public-only": {netip.MustParseAddr("203.0.113.1")},
	})
	got := ProxyPrefixes([]string{"192.168.1.5", "nginx-proxy-manager", "gone-container", "public-only"})
	want := []netip.Prefix{
		netip.MustParsePrefix("192.168.1.5/32"),
		netip.MustParsePrefix("172.18.0.2/32"),
		netip.MustParsePrefix("fd00::2/128"),
		netip.MustParsePrefix("fe80::1/128"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ProxyPrefixes = %v, want %v", got, want)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "172.18.0.2:41000"
	r.Header.Set("X-Forwarded-For", "203.0.113.10")
	ip, local := ClientNet(r, got...)
	if ip != netip.MustParseAddr("203.0.113.10") || local {
		t.Fatalf("ClientNet behind a named proxy = %v local=%v, want 203.0.113.10 remote", ip, local)
	}
}

func TestProxyNameFollowsTheContainerToANewAddress(t *testing.T) {
	table := map[string][]netip.Addr{"npm": {netip.MustParseAddr("172.18.0.2")}}
	mu := fakeProxyDNS(t, table)
	entries := []string{"npm"}
	if got := ProxyPrefixes(entries); len(got) != 1 || got[0].Addr() != netip.MustParseAddr("172.18.0.2") {
		t.Fatalf("first lookup = %v", got)
	}

	refresh := func() {
		t.Helper()
		proxyHosts.mu.Lock()
		proxyHosts.entries["npm"].expires = time.Unix(1, 0)
		proxyHosts.mu.Unlock()
		_ = ProxyPrefixes(entries)
		proxyHosts.mu.Lock()
		ready := proxyHosts.entries["npm"].ready
		proxyHosts.mu.Unlock()
		<-ready
	}

	mu.Lock()
	table["npm"] = []netip.Addr{netip.MustParseAddr("172.18.0.9")}
	mu.Unlock()
	refresh()
	if got := ProxyPrefixes(entries); len(got) != 1 || got[0].Addr() != netip.MustParseAddr("172.18.0.9") {
		t.Fatalf("after the container moved, prefixes = %v", got)
	}

	mu.Lock()
	delete(table, "npm")
	mu.Unlock()
	refresh()
	if got := ProxyPrefixes(entries); len(got) != 0 {
		t.Fatalf("a removed container is still trusted: %v", got)
	}
}

func TestProxyNameTimeoutCannotExtendLastGoodAnswer(t *testing.T) {
	fakeProxyDNS(t, map[string][]netip.Addr{"npm": {netip.MustParseAddr("172.18.0.2")}})
	entries := []string{"npm"}
	good := ProxyPrefixes(entries)
	if len(good) != 1 {
		t.Fatalf("initial lookup = %v", good)
	}
	entry := proxyHosts.entries["npm"]
	refresh := func() {
		t.Helper()
		entry.ready = make(chan struct{})
		resolveProxyHost("npm", entry, func(context.Context, string) ([]netip.Addr, error) {
			return nil, &net.DNSError{IsTimeout: true}
		})
	}
	lastGoodDeadline := entry.staleUntil
	refresh()
	if got := ProxyPrefixes(entries); !slices.Equal(got, good) || entry.staleUntil != lastGoodDeadline {
		t.Fatalf("timeout changed the last good answer or its deadline: %v", got)
	}

	entry.staleUntil = time.Time{}
	if got := ProxyPrefixes(entries); len(got) != 0 {
		t.Fatalf("a stale answer is still trusted between retries: %v", got)
	}
	refresh()
	if len(entry.prefixes) != 0 {
		t.Fatal("timeout retained an expired answer")
	}
}

func TestUntrustedForwarderIsReportedNotBelieved(t *testing.T) {
	resetForwarders()
	t.Cleanup(resetForwarders)
	send := func(peer, xff string) (netip.Addr, bool) {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = peer + ":5000"
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return ClientNet(r)
	}
	ip, local := send("172.18.0.2", "203.0.113.10")
	if ip != netip.MustParseAddr("172.18.0.2") || !local {
		t.Fatalf("untrusted proxy header must not be believed: got %v local=%v", ip, local)
	}
	send("172.18.0.2", "203.0.113.11")
	send("192.168.1.7", "")              // no header: not a proxy
	send("192.168.1.8", "10.0.0.4")      // only private hops: nothing to fix
	send("203.0.113.50", "203.0.113.10") // public peer: not a home proxy

	got := UntrustedForwarders()
	if len(got) != 1 || got[0].Peer != netip.MustParseAddr("172.18.0.2") || got[0].Requests != 2 {
		t.Fatalf("UntrustedForwarders = %+v, want only 172.18.0.2 with 2 requests", got)
	}
	if len(UntrustedForwarders(netip.MustParsePrefix("172.18.0.2/32"))) != 0 {
		t.Fatal("a proxy that is now trusted must not be reported")
	}
}

func TestUntrustedForwardersStayBounded(t *testing.T) {
	resetForwarders()
	t.Cleanup(resetForwarders)
	for i := 1; i <= maxForwarders+10; i++ {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = netip.AddrFrom4([4]byte{10, 0, 1, byte(i)}).String() + ":5000"
		r.Header.Set("X-Real-IP", "203.0.113.10")
		ClientNet(r)
	}
	if n := len(UntrustedForwarders()); n != maxForwarders {
		t.Fatalf("remembered %d forwarders, want %d", n, maxForwarders)
	}
}
