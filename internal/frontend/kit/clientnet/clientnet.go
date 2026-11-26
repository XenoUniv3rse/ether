// Package clientnet classifies authenticated frontend peer and proxy addresses.
package clientnet

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

var overlay = netip.MustParsePrefix("100.64.0.0/10")

const hostPrefixTTL = 30 * time.Second

var hostPrefixState struct {
	mu           sync.RWMutex
	fetched      time.Time
	prefixes     []netip.Prefix
	overrideSet  bool
	overrideList []netip.Prefix
}

// Locality describes the computed local-vs-remote classification for a source IP.
type Locality struct {
	Local      bool
	IP         netip.Addr
	HostPrefix netip.Prefix
	Reason     string
}

// OverrideHostPrefixes replaces detected host interface prefixes until the
// returned restore function is called. Intended for deterministic tests.
func OverrideHostPrefixes(prefixes []netip.Prefix) func() {
	var cleaned []netip.Prefix
	for _, p := range prefixes {
		if p.IsValid() {
			cleaned = append(cleaned, p.Masked())
		}
	}
	hostPrefixState.mu.Lock()
	prevSet := hostPrefixState.overrideSet
	prevList := hostPrefixState.overrideList
	hostPrefixState.overrideSet = true
	hostPrefixState.overrideList = cleaned
	hostPrefixState.mu.Unlock()
	return func() {
		hostPrefixState.mu.Lock()
		hostPrefixState.overrideSet = prevSet
		hostPrefixState.overrideList = prevList
		hostPrefixState.fetched = time.Time{}
		hostPrefixState.mu.Unlock()
	}
}

func hostPrefixes() []netip.Prefix {
	hostPrefixState.mu.RLock()
	if hostPrefixState.overrideSet {
		out := hostPrefixState.overrideList
		hostPrefixState.mu.RUnlock()
		return out
	}
	if !hostPrefixState.fetched.IsZero() && time.Since(hostPrefixState.fetched) < hostPrefixTTL {
		out := hostPrefixState.prefixes
		hostPrefixState.mu.RUnlock()
		return out
	}
	hostPrefixState.mu.RUnlock()

	hostPrefixState.mu.Lock()
	defer hostPrefixState.mu.Unlock()
	if hostPrefixState.overrideSet {
		return hostPrefixState.overrideList
	}
	if !hostPrefixState.fetched.IsZero() && time.Since(hostPrefixState.fetched) < hostPrefixTTL {
		return hostPrefixState.prefixes
	}
	ifaces, err := net.Interfaces()
	if err == nil {
		hostPrefixState.prefixes = prefixesFromInterfaces(ifaces, func(iface net.Interface) ([]net.Addr, error) {
			return iface.Addrs()
		})
	}
	hostPrefixState.fetched = time.Now()
	return hostPrefixState.prefixes
}

func prefixesFromInterfaces(ifaces []net.Interface, addrsOf func(net.Interface) ([]net.Addr, error)) []netip.Prefix {
	var out []netip.Prefix
	seen := make(map[netip.Prefix]struct{})
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		addrs, err := addrsOf(iface)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			p, ok := prefixFromIPNet(ipnet)
			if !ok {
				continue
			}
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

func prefixFromIPNet(ipnet *net.IPNet) (netip.Prefix, bool) {
	if ipnet == nil || ipnet.IP == nil {
		return netip.Prefix{}, false
	}
	ip, ok := netip.AddrFromSlice(ipnet.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ip = ip.Unmap().WithZone("")
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return netip.Prefix{}, false
	}
	ones, bits := ipnet.Mask.Size()
	if ip.Is4() {
		if bits == 128 && ones >= 96 {
			ones -= 96
			bits = 32
		}
		if bits != 32 || ones < 8 || ones > 32 {
			return netip.Prefix{}, false
		}
		return netip.PrefixFrom(ip, ones).Masked(), true
	}
	if ip.Is6() && bits == 128 && ones > 0 && ones <= 128 {
		// Global IPv6 LAN segments use /64 prefixes (even when the host
		// receives a /128 via DHCPv6). Never widen past /64.
		return netip.PrefixFrom(ip, 64).Masked(), true
	}
	return netip.Prefix{}, false
}

func matchHostPrefix(ip netip.Addr) (netip.Prefix, bool) {
	for _, p := range hostPrefixes() {
		if p.Contains(ip) {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// Classify evaluates whether ip is local based on well-known local ranges
// (loopback, RFC1918/ULA private, link-local unicast, CGNAT 100.64.0.0/10)
// or one of this host's own interface prefixes (IPv4 subnets and IPv6 /64
// prefixes).
func Classify(ip netip.Addr) Locality {
	if !ip.IsValid() {
		return Locality{Local: false, Reason: "Remote network"}
	}
	ip = ip.Unmap().WithZone("")
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || overlay.Contains(ip) {
		return Locality{
			Local:  true,
			IP:     ip,
			Reason: fmt.Sprintf("Home network (source %s)", ip),
		}
	}
	if p, ok := matchHostPrefix(ip); ok {
		return Locality{
			Local:      true,
			IP:         ip,
			HostPrefix: p,
			Reason:     fmt.Sprintf("Home network (source %s, local subnet %s)", ip, p),
		}
	}
	return Locality{
		Local:  false,
		IP:     ip,
		Reason: fmt.Sprintf("Remote network (source %s)", ip),
	}
}

// LAN reports whether an address belongs to a local network: loopback,
// RFC1918/ULA private space, link-local unicast, CGNAT overlay
// (100.64.0.0/10), or one of this host's own interface prefixes (IPv4
// subnets and IPv6 /64 prefixes).
func LAN(ip netip.Addr) bool {
	return Classify(ip).Local
}

// LocalityReason returns a human-readable explanation for a session's computed
// local-vs-remote classification from its resolved source IP and remote flag.
func LocalityReason(ipStr string, remote bool) string {
	ip, err := netip.ParseAddr(strings.TrimSpace(ipStr))
	if err != nil || !ip.IsValid() {
		if remote {
			return "Remote network"
		}
		return "Home network"
	}
	ip = ip.Unmap().WithZone("")
	if remote {
		return fmt.Sprintf("Remote network (source %s)", ip)
	}
	if c := Classify(ip); c.Local {
		return c.Reason
	}
	return fmt.Sprintf("Home network (source %s)", ip)
}

func peerAddr(r *http.Request) (netip.Addr, bool) {
	if r == nil {
		return netip.Addr{}, false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return peer.Unmap().WithZone(""), true
}

func isTrustedAddr(ip netip.Addr, trusted []netip.Prefix) bool {
	if !ip.IsValid() {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, p := range trusted {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// TrustedPeer reports whether r's direct connecting peer is loopback or within
// one of the explicitly trusted proxy prefixes.
func TrustedPeer(r *http.Request, trusted ...netip.Prefix) bool {
	peer, ok := peerAddr(r)
	return ok && isTrustedAddr(peer, trusted)
}

// ClientNet classifies the peer and returns its client address. Forwarding
// headers are trusted only from loopback or explicitly configured proxies.
// Malformed chains fail closed. A public hop cannot be hidden by a preceding
// client-supplied private address.
func ClientNet(r *http.Request, trusted ...netip.Prefix) (netip.Addr, bool) {
	peer, ok := peerAddr(r)
	if !ok {
		return netip.Addr{}, false
	}
	if !isTrustedAddr(peer, trusted) {
		noteUntrustedForwarder(r, peer)
		return peer, LAN(peer)
	}
	var hops []string
	if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
		hops = strings.Split(strings.Join(values, ","), ",")
	} else if values := r.Header.Values("Forwarded"); len(values) > 0 {
		for _, element := range strings.Split(strings.Join(values, ","), ",") {
			found := false
			for _, param := range strings.Split(element, ";") {
				k, v, ok := strings.Cut(strings.TrimSpace(param), "=")
				if ok && strings.EqualFold(k, "for") {
					if found {
						return netip.Addr{}, false
					}
					found = true
					v = strings.Trim(strings.TrimSpace(v), "\"")
					if h, _, e := net.SplitHostPort(v); e == nil {
						v = h
					}
					hops = append(hops, strings.Trim(v, "[]"))
				}
			}
			if !found {
				return netip.Addr{}, false
			}
		}
	} else if v := r.Header.Get("X-Real-IP"); v != "" {
		hops = []string{v}
	}
	if len(hops) == 0 {
		return peer, LAN(peer)
	}
	var nearest, lastTrusted netip.Addr
	for i := len(hops) - 1; i >= 0; i-- {
		ip, e := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if e != nil {
			return netip.Addr{}, false
		}
		ip = ip.Unmap().WithZone("")
		if isTrustedAddr(ip, trusted) {
			lastTrusted = ip
			continue
		}
		if !LAN(ip) {
			return ip, false
		}
		if !nearest.IsValid() {
			nearest = ip
		}
	}
	if nearest.IsValid() {
		return nearest, LAN(nearest)
	}
	return lastTrusted, LAN(lastTrusted)
}

// ParseProxyEntry parses one trusted proxy IP address (treated as /32 or /128)
// or CIDR prefix. Wildcard ranges (0.0.0.0/0, ::/0) are rejected so any
// internet peer cannot spoof forwarding headers.
func ParseProxyEntry(raw string) (netip.Prefix, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return netip.Prefix{}, errors.New("enter an IP address or CIDR range (for example 192.168.1.5 or 10.0.0.0/8)")
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		p = p.Masked()
		if p.Bits() == 0 {
			return netip.Prefix{}, errors.New("cannot trust every address; enter a specific proxy IP or subnet")
		}
		return p, nil
	}
	if addr, err := netip.ParseAddr(s); err == nil && addr.Zone() == "" {
		addr = addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen()), nil
	}
	return netip.Prefix{}, errors.New("enter an IP address or CIDR range (for example 192.168.1.5 or 10.0.0.0/8)")
}

// ProxyPrefixes parses configured proxy IPs and networks and resolves proxy
// names. All initial lookups share a single bounded wait. Later lookups refresh
// asynchronously on use.
func ProxyPrefixes(entries []string) []netip.Prefix {
	var out []netip.Prefix
	type pendingHost struct {
		entry *proxyHostEntry
		ready <-chan struct{}
	}
	var hosts []pendingHost
	for _, raw := range entries {
		raw = proxyEntryHost(raw)
		if p, err := ParseProxyEntry(raw); err == nil {
			out = append(out, p)
		} else if name, ok := proxyHostName(raw); ok {
			entry, ready := startProxyHost(name)
			hosts = append(hosts, pendingHost{entry, ready})
		}
	}
	if len(hosts) == 0 {
		return out
	}
	var wait *time.Timer
initial:
	for _, host := range hosts {
		if host.ready != nil {
			if wait == nil {
				wait = time.NewTimer(proxyLookupBudget)
				defer wait.Stop()
			}
			select {
			case <-host.ready:
			case <-wait.C:
				break initial
			}
		}
	}
	proxyHosts.mu.Lock()
	defer proxyHosts.mu.Unlock()
	now := time.Now()
	for _, host := range hosts {
		if now.Before(host.entry.staleUntil) {
			out = append(out, host.entry.prefixes...)
		}
	}
	return out
}
