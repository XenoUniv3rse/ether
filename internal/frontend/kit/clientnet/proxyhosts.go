package clientnet

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A trusted proxy may be named instead of numbered, for example a Docker
// container ("nginx-proxy-manager") whose address changes when it is
// recreated. Lookups start on use and refresh asynchronously after the first
// lookup.
const (
	proxyHostTTL      = 30 * time.Second
	proxyHostRetry    = 5 * time.Second
	proxyLookupBudget = 2 * time.Second
)

// lookupProxyHost resolves a proxy name. Tests replace it.
var lookupProxyHost = LookupProxyHost

type proxyHostEntry struct {
	prefixes     []netip.Prefix
	expires      time.Time
	staleUntil   time.Time
	initialUntil time.Time
	refreshing   bool
	ready        chan struct{}
}

var proxyHosts struct {
	mu      sync.Mutex
	entries map[string]*proxyHostEntry
}

var errProxyEntry = errors.New("enter an IP address, a CIDR range or a host or container name (for example 192.168.1.5, 10.0.0.0/8 or nginx-proxy-manager)")

// proxyHostName reports whether raw is a host or container name rather than
// an address: letters, digits, '.', '-' and '_', starting with a letter or
// digit and containing at least one letter.
func proxyHostName(raw string) (string, bool) {
	name := strings.ToLower(strings.TrimSpace(raw))
	s := strings.TrimSuffix(name, ".")
	if s == "" || len(s) > 253 || strings.Contains(name, "..") {
		return "", false
	}
	letter := false
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
			letter = true
		case c >= '0' && c <= '9':
		case (c == '.' || c == '-' || c == '_') && i > 0:
		default:
			return "", false
		}
	}
	if !letter {
		return "", false
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return "", false
	}
	return name, true
}

// ValidateProxyEntry accepts what ProxyPrefixes can trust: an IP address, a
// CIDR range or a host or container name.
func ValidateProxyEntry(raw string) error {
	raw = proxyEntryHost(raw)
	if _, err := ParseProxyEntry(raw); err == nil {
		return nil
	} else if strings.Contains(raw, "/") {
		return err
	}
	if _, ok := proxyHostName(raw); ok {
		return nil
	}
	return errProxyEntry
}

// HasProxyHostNames reports whether any entry is a name, so callers that
// cache ProxyPrefixes know the result can change without a config change.
func HasProxyHostNames(entries []string) bool {
	for _, raw := range entries {
		raw = proxyEntryHost(raw)
		if _, err := ParseProxyEntry(raw); err == nil {
			continue
		}
		if _, ok := proxyHostName(raw); ok {
			return true
		}
	}
	return false
}

// startProxyHost starts a lookup on first use or refreshes an expired entry.
// Only callers sharing the initial lookup may wait for it.
func startProxyHost(name string) (*proxyHostEntry, <-chan struct{}) {
	proxyHosts.mu.Lock()
	defer proxyHosts.mu.Unlock()
	if proxyHosts.entries == nil {
		proxyHosts.entries = make(map[string]*proxyHostEntry)
	}
	e := proxyHosts.entries[name]
	now := time.Now()
	if e == nil {
		e = &proxyHostEntry{initialUntil: now.Add(proxyLookupBudget)}
		proxyHosts.entries[name] = e
	}
	if !e.refreshing && !now.Before(e.expires) {
		e.refreshing = true
		e.ready = make(chan struct{})
		go resolveProxyHost(name, e, lookupProxyHost)
	}
	if e.expires.IsZero() && now.Before(e.initialUntil) {
		return e, e.ready
	}
	return e, nil
}

// localProxyIP is deliberately narrower than LAN: directly attached public
// subnets and CGNAT do not qualify for trust based on a DNS answer.
func localProxyIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

func proxyDNSTimedOut(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || (dnsErr != nil && dnsErr.IsTimeout)
}

func resolveProxyHost(name string, e *proxyHostEntry, lookup func(context.Context, string) ([]netip.Addr, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), proxyLookupBudget)
	defer cancel()
	addrs, err := lookup(ctx, name)
	var prefixes []netip.Prefix
	if err == nil && ctx.Err() == nil {
		seen := make(map[netip.Addr]struct{})
		for _, a := range addrs {
			a = a.Unmap().WithZone("")
			if !localProxyIP(a) {
				continue
			}
			if _, dup := seen[a]; dup {
				continue
			}
			seen[a] = struct{}{}
			prefixes = append(prefixes, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	timedOut := proxyDNSTimedOut(err)

	proxyHosts.mu.Lock()
	defer proxyHosts.mu.Unlock()
	now := time.Now()
	e.expires = now.Add(proxyHostRetry)
	switch {
	case len(prefixes) > 0:
		e.prefixes = prefixes
		e.expires = now.Add(proxyHostTTL)
		// Permit one retry window and the time for that lookup. Further
		// failures cannot move this deadline past the last good answer.
		e.staleUntil = e.expires.Add(proxyHostRetry + proxyLookupBudget)
	case !timedOut || !now.Before(e.staleUntil):
		e.prefixes = nil
		e.staleUntil = time.Time{}
	}
	e.refreshing = false
	close(e.ready)
}

// resetProxyHosts forgets every resolved name. Intended for tests.
func resetProxyHosts() {
	proxyHosts.mu.Lock()
	proxyHosts.entries = nil
	proxyHosts.mu.Unlock()
}

// proxyEntryHost reduces an entry written as an address to its host, so
// "http://nginx-proxy-manager:81" or "nginx-proxy-manager:81" (how a reverse
// proxy's own config names an upstream) means the container nginx-proxy-manager.
// Trust is about which host the request came from; scheme and port do not
// matter. CIDR ranges and plain names pass through unchanged.
func proxyEntryHost(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "://"); i > 0 {
		if u, err := url.Parse(s); err == nil {
			host := u.Hostname()
			if host != "" && !strings.Contains(host, "/") && (!strings.Contains(host, ":") || strings.HasPrefix(u.Host, "[")) {
				return host
			}
		}
		return s
	}
	if host, port, err := net.SplitHostPort(s); err == nil && host != "" && !strings.Contains(host, "/") {
		if _, err := strconv.ParseUint(port, 10, 16); err == nil {
			return host
		}
	}
	return s
}
