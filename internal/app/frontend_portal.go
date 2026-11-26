package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/clientnet"
	"github.com/braedonsaunders/ether/internal/frontends/stremio"
	"github.com/braedonsaunders/ether/internal/frontends/vela"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/remote"
)

var portalDockerBridge = netip.MustParsePrefix("172.16.0.0/12")

// surfaceURL resolves the reachable base URL for frontend id while answering
// req via the shared appAddress resolver.
func (r *frontendRuntime) surfaceURL(ctx context.Context, id frontend.Protocol, req *http.Request) (string, error) {
	return r.appAddress(ctx, id, req, false)
}

// appAddress is the single shared resolver for an app's reachable base URL
// across every Ether surface: protocol handlers (System/Info, pairing, stream
// and image links), owner console, viewer portal (/ether/me -> Connect apps),
// QR codes, and web player launch.
//
// Precedence:
//  1. Request-derived when req is answering a request on app id's own surface
//     (honouring X-Forwarded-Proto/-Host/-Prefix only from trusted proxies,
//     plus the active mount prefix for id).
//  2. The app's configured published address(es) (frontends.<id>.publishedAddresses,
//     with legacy publicUrl migrated into it; Plex uses Plex.CustomURLs),
//     filtered to HTTPS when requireHTTPS is true.
//  3. The server address plus the app's path prefix (derived from req when
//     present, or the server's configured origin when out-of-band).
func (r *frontendRuntime) appAddress(ctx context.Context, id frontend.Protocol, req *http.Request, requireHTTPS bool) (string, error) {
	var ip netip.Addr
	var hasIP bool
	if req != nil && r.app != nil {
		ip, hasIP = r.app.frontendClientIP(req)
	}
	var addresses, prefixes, shared []string
	var running bool
	if r.control != nil && r.control.listeners != nil {
		r.control.listeners.mu.Lock()
		if run := r.control.listeners.running[id]; run != nil {
			running = true
			for _, listener := range run.listeners {
				addresses = append(addresses, listener.Addr().String())
			}
			prefixes = append(prefixes, run.prefixes...)
			shared = append(shared, run.sharedPrefixes...)
		}
		r.control.listeners.mu.Unlock()
	}
	var cfg config.Config
	var trusted []netip.Prefix
	if r.app != nil && r.app.cfg != nil {
		cfg = r.app.cfg.Get()
		trusted = r.app.trustedProxyPrefixes()
	}

	var local net.Addr
	if req != nil {
		local, _ = req.Context().Value(http.LocalAddrContextKey).(net.Addr)
	}
	var interfaces []remote.InterfaceAddr
	lanLoaded := false
	loadLAN := func() []remote.InterfaceAddr {
		if !lanLoaded {
			lanLoaded = true
			if hasIP && identity.LAN(ip) {
				interfaces, _ = remote.LANAddresses(remote.LANOptions{IncludeIPv6: true})
			}
		}
		return interfaces
	}

	// 1. Request-derived when req is answering a request on app id's own surface.
	if req != nil {
		if mountPrefix, hasMount := frontend.MountPrefix(req.Context(), id); hasMount {
			if base, ok := requestSurfaceBase(req, ip, mountPrefix, trusted); ok {
				if !requireHTTPS || strings.HasPrefix(base, "https://") {
					return base, nil
				}
			}
		} else if matchedPrefix, matched := matchedDedicatedListener(local, addresses, prefixes); matched {
			if base, ok := requestSurfaceBase(req, ip, matchedPrefix, trusted); ok {
				if !requireHTTPS || strings.HasPrefix(base, "https://") {
					return base, nil
				}
			}
			if !requireHTTPS && hasIP {
				if base, ok := surfaceLANAddress(ip, local, addresses, prefixes, loadLAN()); ok {
					return base, nil
				}
			}
		}
	}

	// 2. App's configured published address(es) / migrated publicUrl.
	if base, ok := frontendPublishedOrigin(cfg, string(id), requireHTTPS); ok {
		if id == "stremio" {
			u, err := url.Parse(base)
			if err != nil {
				return "", errors.New("app address unavailable")
			}
			u.Path = stremio.AddonBasePath(u.Path)
			u.RawPath = ""
			return u.String(), nil
		}
		return strings.TrimRight(base, "/"), nil
	}

	// 3. Dedicated listener (when the app is not served on the main port or on
	// a direct local request to Vela) and/or server main address plus app path.
	hasDedicated := len(addresses) > 0
	appPrefix := r.appPathPrefix(id, shared)
	servedOnMain := id == "plex" || len(shared) > 0 || (!running && appPrefix != "")

	if hasDedicated && !requireHTTPS {
		directReq := false
		if req != nil {
			if origin, ok := clientnet.Origin(req, trusted...); ok && !origin.Proxied {
				directReq = true
			} else if hasIP && ip.IsLoopback() {
				directReq = true
			}
		}
		if !servedOnMain || (id == "vela" && directReq) {
			if base, ok := dedicatedAppAddress(req, ip, hasIP, addresses, prefixes, trusted, loadLAN); ok {
				return base, nil
			}
		}
	}

	if servedOnMain {
		if req != nil {
			var portal string
			var err error
			if requireHTTPS {
				portal, err = r.mainPortalSecureURL(ctx, req)
			} else {
				portal, err = r.mainPortalURL(ctx, req)
			}
			if err == nil {
				if resolved, err := joinPortalPrefix(portal, appPrefix); err == nil {
					return resolved, nil
				}
			}
		}
		if base, ok := configuredOrigin(cfg, r.mainPort()); ok && (!requireHTTPS || strings.HasPrefix(base, "https://")) {
			if appPrefix != "" {
				return strings.TrimRight(strings.TrimRight(base, "/")+appPrefix, "/"), nil
			}
			return strings.TrimRight(base, "/"), nil
		}
	}
	if !requireHTTPS && hasDedicated {
		if base, ok := dedicatedAppAddress(req, ip, hasIP, addresses, prefixes, trusted, loadLAN); ok {
			return base, nil
		}
	}
	if !running && appPrefix == "" && id != "plex" {
		return "", errors.New("frontend unavailable")
	}
	if requireHTTPS {
		return "", errors.New("secure address unavailable")
	}
	return "", errors.New("no reachable frontend address")
}

func (r *frontendRuntime) appPathPrefix(id frontend.Protocol, shared []string) string {
	if id == "plex" {
		return ""
	}
	for _, prefix := range shared {
		if p := strings.TrimRight(strings.TrimSpace(prefix), "/"); p != "" && p != "/" {
			return p
		}
	}
	if r.registry != nil {
		for _, desc := range r.registry.Descriptors() {
			if desc.ID == id {
				if p := strings.TrimRight(strings.TrimSpace(desc.PathPrefix), "/"); p != "" && p != "/" {
					return p
				}
				break
			}
		}
	}
	return ""
}

func matchedDedicatedListener(local net.Addr, addresses, prefixes []string) (string, bool) {
	if local == nil || len(addresses) == 0 {
		return "", false
	}
	_, localPort, err := net.SplitHostPort(local.String())
	if err != nil || localPort == "" {
		return "", false
	}
	for index, address := range addresses {
		_, port, err := net.SplitHostPort(address)
		if err != nil || port != localPort {
			continue
		}
		prefix := ""
		if index < len(prefixes) {
			prefix = prefixes[index]
		}
		return prefix, true
	}
	return "", false
}

func joinPortalPrefix(portal, appPrefix string) (string, error) {
	u, err := url.Parse(portal)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, "/ether/me") {
		return "", errors.New("portal address unavailable")
	}
	u.Path = strings.TrimSuffix(u.Path, "/ether/me") + appPrefix
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func requestSurfaceBase(req *http.Request, peer netip.Addr, mountPrefix string, trusted []netip.Prefix) (string, bool) {
	origin, ok := clientnet.Origin(req, trusted...)
	if !ok {
		return "", false
	}
	host := origin.Hostname()
	allowLoopback := !origin.Proxied && peer.IsValid() && peer.IsLoopback()
	usable := usableRequestHost(host, origin.Proxied)
	if !usable && allowLoopback {
		if strings.EqualFold(host, "localhost") {
			usable = true
		} else if ip, err := netip.ParseAddr(host); err == nil && ip.Unmap().IsLoopback() {
			usable = true
		}
	}
	if !usable {
		return "", false
	}
	base := origin.BaseURL(mountPrefix)
	if base == "" {
		return "", false
	}
	if !allowLoopback {
		if _, err := remote.ParsePublishedURL(base); err != nil {
			return "", false
		}
	}
	return base, true
}

func (r *frontendRuntime) mainPort() string {
	r.mu.Lock()
	addr := r.mainAddr
	r.mu.Unlock()
	if addr == nil {
		return ""
	}
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return ""
	}
	return port
}

func surfaceSharedAddress(portal string, shared []string) (string, error) {
	u, err := url.Parse(portal)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, "/ether/me") {
		return "", errors.New("portal address unavailable")
	}
	for _, prefix := range shared {
		if prefix != "" {
			u.Path = strings.TrimSuffix(u.Path, "/ether/me") + prefix
			return strings.TrimRight(u.String(), "/"), nil
		}
	}
	return "", errors.New("no reachable frontend address")
}

func surfaceAddress(req *http.Request, peer netip.Addr, local net.Addr, addresses, prefixes []string, trusted []netip.Prefix, interfaces []remote.InterfaceAddr) (string, bool) {
	localPort := ""
	if local != nil {
		var err error
		_, localPort, err = net.SplitHostPort(local.String())
		if err != nil {
			return "", false
		}
	}
	for index, address := range addresses {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			continue
		}
		if local != nil && port != localPort {
			continue
		}
		if base, ok := requestOriginWithPort(req, host, port, trusted); ok {
			u, err := url.Parse(base)
			if err == nil && u != nil {
				if index < len(prefixes) && prefixes[index] != "" {
					u.Path = clientnet.JoinPrefix(u.Path, prefixes[index])
				}
				return strings.TrimRight(u.String(), "/"), true
			}
		}
		if local == nil {
			break
		}
	}
	return surfaceLANAddress(peer, local, addresses, prefixes, interfaces)
}

func surfaceLANAddress(peer netip.Addr, local net.Addr, addresses, prefixes []string, interfaces []remote.InterfaceAddr) (string, bool) {
	if local == nil || !identity.LAN(peer) {
		return "", false
	}
	_, localPort, err := net.SplitHostPort(local.String())
	if err != nil {
		return "", false
	}
	for index, address := range addresses {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != localPort {
			continue
		}
		if peer.IsLoopback() {
			bound := net.ParseIP(host)
			if bound != nil && (bound.IsUnspecified() || bound.IsLoopback()) {
				u := url.URL{Scheme: "http", Host: net.JoinHostPort(peer.String(), port)}
				if index < len(prefixes) {
					u.Path = prefixes[index]
				}
				return strings.TrimRight(u.String(), "/"), true
			}
		}
		portal, err := portalLANAddress(peer, host, port, interfaces)
		if err == nil {
			u, _ := url.Parse(portal)
			u.Path = ""
			if index < len(prefixes) {
				u.Path = prefixes[index]
			}
			return strings.TrimRight(u.String(), "/"), true
		}
	}
	return "", false
}

// dedicatedAppAddress resolves a frontend's own dedicated listener port across
// surfaces (for example when the owner console or viewer portal resolves Vela's
// dedicated port). Dedicated frontend listeners serve plain HTTP directly.
func dedicatedAppAddress(req *http.Request, peer netip.Addr, hasPeer bool, addresses, prefixes []string, trusted []netip.Prefix, loadLAN func() []remote.InterfaceAddr) (string, bool) {
	for index, address := range addresses {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port == "" {
			continue
		}
		prefix := ""
		if index < len(prefixes) {
			prefix = prefixes[index]
		}
		if req != nil {
			if origin, ok := clientnet.Origin(req, trusted...); ok && !origin.Proxied {
				if bound := net.ParseIP(host); bound == nil || !bound.IsLoopback() {
					reqHost := origin.Hostname()
					if usableRequestHost(reqHost, false) {
						u := url.URL{Scheme: "http", Host: net.JoinHostPort(reqHost, port), Path: prefix}
						candidate := strings.TrimRight(u.String(), "/")
						if _, err := remote.ParsePublishedURL(candidate); err == nil {
							return candidate, true
						}
					}
				}
			}
		}
		if hasPeer && identity.LAN(peer) {
			if peer.IsLoopback() {
				bound := net.ParseIP(host)
				if bound != nil && (bound.IsUnspecified() || bound.IsLoopback()) {
					u := url.URL{Scheme: "http", Host: net.JoinHostPort(peer.String(), port), Path: prefix}
					return strings.TrimRight(u.String(), "/"), true
				}
			}
			if portal, err := portalLANAddress(peer, host, port, loadLAN()); err == nil {
				if u, err := url.Parse(portal); err == nil && u != nil {
					u.Path = prefix
					return strings.TrimRight(u.String(), "/"), true
				}
			}
		}
	}
	return "", false
}

func sameAddrPort(a, b net.Addr) bool {
	if a == nil || b == nil {
		return false
	}
	_, pa, erra := net.SplitHostPort(a.String())
	_, pb, errb := net.SplitHostPort(b.String())
	return erra == nil && errb == nil && pa != "" && pa == pb
}

// consoleListener returns the active listener serving /ether/me for req and
// whether reverse-proxy headers on req can be treated as targeting /ether/me.
func (r *frontendRuntime) consoleListener(req *http.Request) (net.Addr, bool) {
	r.mu.Lock()
	mainAddr := r.mainAddr
	consoleAddr := r.consoleAddr
	onMain := r.consoleOnMain
	r.mu.Unlock()
	var local net.Addr
	if req != nil {
		local, _ = req.Context().Value(http.LocalAddrContextKey).(net.Addr)
	}
	if consoleAddr == nil {
		// No dedicated console listener; console is served on mainAddr.
		return mainAddr, local == nil || sameAddrPort(local, mainAddr)
	}
	if !onMain {
		return consoleAddr, local == nil || sameAddrPort(local, consoleAddr)
	}
	if local != nil && sameAddrPort(local, mainAddr) {
		return mainAddr, true
	}
	return consoleAddr, local == nil || sameAddrPort(local, consoleAddr)
}

// viewerPortalURL chooses a reachable portal URL in order:
// 1. the request Host (with the console listener's port) or X-Forwarded-Host/-Proto/-Prefix from a trusted proxy;
// 2. the matching LAN interface;
// 3. the owner's configured published/custom URL or published remote connection.
func (r *frontendRuntime) viewerPortalURL(ctx context.Context, req *http.Request) (string, error) {
	addr, allowProxy := r.consoleListener(req)
	return r.portalURLForAddr(ctx, req, addr, allowProxy)
}

// mainPortalURL resolves the portal URL anchored on Ether's main listener, used
// by appAddress for protocols mounted on the main port.
func (r *frontendRuntime) mainPortalURL(ctx context.Context, req *http.Request) (string, error) {
	r.mu.Lock()
	addr := r.mainAddr
	r.mu.Unlock()
	var local net.Addr
	if req != nil {
		local, _ = req.Context().Value(http.LocalAddrContextKey).(net.Addr)
	}
	allowProxy := local == nil || sameAddrPort(local, addr)
	return r.portalURLForAddr(ctx, req, addr, allowProxy)
}

func (r *frontendRuntime) portalURLForAddr(ctx context.Context, req *http.Request, addr net.Addr, allowProxy bool) (string, error) {
	ip, ok := r.app.frontendClientIP(req)
	if !ok {
		return "", errors.New("client address unavailable")
	}
	if addr == nil {
		return "", errors.New("main listener unavailable")
	}
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", err
	}
	var cfg config.Config
	var trusted []netip.Prefix
	if r.app != nil && r.app.cfg != nil {
		cfg = r.app.cfg.Get()
		trusted = r.app.trustedProxyPrefixes()
	}
	var interfaces []remote.InterfaceAddr
	if identity.LAN(ip) && !ip.IsLoopback() {
		interfaces, _ = remote.LANAddresses(remote.LANOptions{IncludeIPv6: true})
	}
	var connections []remote.Connection
	for _, raw := range cfg.Plex.CustomURLs {
		connections = append(connections, remote.Connection{URI: raw, Custom: true})
	}
	if r.app != nil && r.app.registrar != nil {
		connections = append(connections, r.app.registrar.Status().Connections...)
	}
	return portalAddressWithProxyPolicy(ctx, req, ip, host, port, cfg, trusted, interfaces, connections, allowProxy)
}

func portalAddress(ctx context.Context, req *http.Request, ip netip.Addr, host, port string, cfg config.Config, trusted []netip.Prefix, interfaces []remote.InterfaceAddr, connections []remote.Connection) (string, error) {
	return portalAddressWithProxyPolicy(ctx, req, ip, host, port, cfg, trusted, interfaces, connections, true)
}

func portalAddressWithProxyPolicy(ctx context.Context, req *http.Request, ip netip.Addr, host, port string, cfg config.Config, trusted []netip.Prefix, interfaces []remote.InterfaceAddr, connections []remote.Connection, allowProxy bool) (string, error) {
	if base, ok := requestOriginWithPortPolicy(req, host, port, trusted, allowProxy); ok {
		return strings.TrimRight(base, "/") + "/ether/me", nil
	}
	// A loopback client talking to a loopback or wildcard listener already
	// knows the answer. Enumerating interfaces is the slow first call.
	if ip.IsLoopback() {
		if bound := net.ParseIP(host); bound != nil && (bound.IsUnspecified() || bound.IsLoopback()) {
			return (&url.URL{Scheme: "http", Host: net.JoinHostPort(ip.String(), port), Path: "/ether/me"}).String(), nil
		}
	}
	if identity.LAN(ip) {
		if lan, err := portalLANAddress(ip, host, port, interfaces); err == nil {
			return lan, nil
		}
	}
	if base, ok := configuredOrigin(cfg, port); ok {
		return strings.TrimRight(base, "/") + "/ether/me", nil
	}
	return portalPublicAddress(ctx, connections)
}

// viewerPortalSecureURL selects a reachable HTTPS address for clients, such as
// Stremio, that reject non-loopback HTTP addon manifests.
func (r *frontendRuntime) viewerPortalSecureURL(ctx context.Context, req *http.Request) (string, error) {
	addr, allowProxy := r.consoleListener(req)
	return r.portalSecureURLForAddr(ctx, req, addr, allowProxy)
}

func (r *frontendRuntime) mainPortalSecureURL(ctx context.Context, req *http.Request) (string, error) {
	r.mu.Lock()
	addr := r.mainAddr
	r.mu.Unlock()
	var local net.Addr
	if req != nil {
		local, _ = req.Context().Value(http.LocalAddrContextKey).(net.Addr)
	}
	allowProxy := local == nil || sameAddrPort(local, addr)
	return r.portalSecureURLForAddr(ctx, req, addr, allowProxy)
}

func (r *frontendRuntime) portalSecureURLForAddr(ctx context.Context, req *http.Request, addr net.Addr, allowProxy bool) (string, error) {
	ip, ok := r.app.frontendClientIP(req)
	if !ok {
		return "", errors.New("client address unavailable")
	}
	if addr == nil {
		return "", errors.New("main listener unavailable")
	}
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", err
	}
	var cfg config.Config
	var trusted []netip.Prefix
	if r.app != nil && r.app.cfg != nil {
		cfg = r.app.cfg.Get()
		trusted = r.app.trustedProxyPrefixes()
	}
	if base, ok := requestOriginWithPortPolicy(req, host, port, trusted, allowProxy); ok && strings.HasPrefix(base, "https://") {
		return strings.TrimRight(base, "/") + "/ether/me", nil
	}
	if base, ok := configuredOrigin(cfg, port); ok && strings.HasPrefix(base, "https://") {
		return strings.TrimRight(base, "/") + "/ether/me", nil
	}
	var interfaces []remote.InterfaceAddr
	if identity.LAN(ip) {
		interfaces, _ = remote.LANAddresses(remote.LANOptions{IncludeIPv6: true})
	}
	var connections []remote.Connection
	if r.app != nil && r.app.registrar != nil {
		connections = append(connections, r.app.registrar.Status().Connections...)
	}
	for _, raw := range cfg.Plex.CustomURLs {
		connections = append(connections, remote.Connection{URI: raw, Custom: true})
	}
	return portalSecureAddress(ctx, ip, host, port, interfaces, connections)
}

// frontendPublishedOrigin returns the app's first configured published address
// (filtering to HTTPS when requireHTTPS is true). Plex uses Plex.CustomURLs.
func frontendPublishedOrigin(cfg config.Config, id string, requireHTTPS bool) (string, bool) {
	var rawList []string
	if id == "plex" {
		rawList = cfg.Plex.CustomURLs
	} else {
		rawList = cfg.Frontends[id].EffectivePublishedAddresses()
	}
	for _, raw := range rawList {
		if base, ok := parseConfiguredURL(raw); ok {
			if requireHTTPS && !strings.HasPrefix(base, "https://") {
				continue
			}
			return base, true
		}
	}
	return "", false
}

func configuredOrigin(cfg config.Config, defaultPort string) (string, bool) {
	for _, raw := range cfg.Plex.CustomURLs {
		if base, ok := parseConfiguredURL(raw); ok {
			return base, true
		}
	}
	if host := strings.TrimSpace(cfg.Plex.PublicHost); host != "" {
		if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
			if base, ok := parseConfiguredURL(host); ok {
				return base, true
			}
		} else {
			var h, p string
			if sh, sp, err := net.SplitHostPort(host); err == nil {
				h, p = strings.Trim(sh, "[]"), sp
			} else {
				h = strings.Trim(host, "[]")
				if cfg.Plex.PublicPort > 0 {
					p = strconv.Itoa(cfg.Plex.PublicPort)
				} else if cfg.Server.Port > 0 {
					p = strconv.Itoa(cfg.Server.Port)
				} else {
					p = defaultPort
				}
			}
			scheme := "http"
			if p == "443" || cfg.Plex.SecureConnections == remote.SecureRequired {
				scheme = "https"
			}
			addr := h
			if p != "" && !((scheme == "http" && p == "80") || (scheme == "https" && p == "443")) {
				addr = net.JoinHostPort(h, p)
			}
			if base, ok := parseConfiguredURL(scheme + "://" + addr); ok {
				return base, true
			}
		}
	}
	return "", false
}

func parseConfiguredURL(raw string) (string, bool) {
	p, err := remote.ParsePublishedAddress(raw)
	if err != nil || !usableRequestHost(p.Host, true) {
		return "", false
	}
	return p.URI, true
}

func requestOriginWithPort(req *http.Request, boundHost, appPort string, trusted []netip.Prefix) (string, bool) {
	return requestOriginWithPortPolicy(req, boundHost, appPort, trusted, true)
}

func requestOriginWithPortPolicy(req *http.Request, boundHost, appPort string, trusted []netip.Prefix, allowProxy bool) (string, bool) {
	if req == nil || appPort == "" {
		return "", false
	}
	origin, ok := clientnet.Origin(req, trusted...)
	if !ok {
		return "", false
	}
	if origin.Proxied {
		if !allowProxy || !usableRequestHost(origin.Hostname(), true) {
			return "", false
		}
		base := origin.BaseURL("")
		if _, err := remote.ParsePublishedURL(base); err != nil {
			return "", false
		}
		return base, true
	}
	if bound := net.ParseIP(boundHost); bound != nil && bound.IsLoopback() {
		return "", false
	}
	host := origin.Hostname()
	if !usableRequestHost(host, false) {
		return "", false
	}
	hostPort := net.JoinHostPort(host, appPort)
	if local, ok := req.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && local != nil {
		if _, localPort, err := net.SplitHostPort(local.String()); err == nil && localPort == appPort && origin.Host != "" {
			hostPort = origin.Host
		}
	}
	candidate := (&url.URL{Scheme: origin.Scheme, Host: hostPort}).String()
	if _, err := remote.ParsePublishedURL(candidate); err != nil {
		return "", false
	}
	return candidate, true
}

func usableRequestHost(host string, fromProxyOrConfig bool) bool {
	h := strings.TrimSpace(strings.Trim(host, "[]"))
	if h == "" || strings.EqualFold(h, "localhost") {
		return false
	}
	if !fromProxyOrConfig && strings.EqualFold(h, "example.com") {
		return false
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		ip = ip.Unmap()
		if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || portalDockerBridge.Contains(ip) {
			return false
		}
	}
	return true
}

func portalProxyTrusted(r *http.Request, trusted []netip.Prefix) bool {
	return identity.TrustedPeer(r, trusted...)
}

func portalForwardedHost(r *http.Request) string {
	if h := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); h != "" {
		return portalSanitizeHost(strings.TrimSpace(strings.Split(h, ",")[0]))
	}
	return ""
}

func portalForwardedProto(r *http.Request) string {
	p := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	return strings.ToLower(strings.TrimSpace(strings.Split(p, ",")[0]))
}

func portalSanitizeHost(host string) string {
	if host == "" || strings.ContainsAny(host, " \t\r\n/@") || len(host) > 255 {
		return ""
	}
	return host
}

func portalSecureAddress(ctx context.Context, peer netip.Addr, host, port string, interfaces []remote.InterfaceAddr, connections []remote.Connection) (string, error) {
	// A loopback admin session has no LAN address of its own. The certificate
	// is published on the server's LAN interfaces, which is the address Stremio
	// on this machine can still open.
	if peer.Unmap().IsLoopback() {
		if secure, ok := loopbackPlexDirect(host, port, interfaces, connections); ok {
			return secure, nil
		}
	}
	if lan, err := portalLANAddress(peer, host, port, interfaces); err == nil {
		local, _ := url.Parse(lan)
		localIP := net.ParseIP(local.Hostname())
		for _, candidate := range connections {
			u, err := url.Parse(candidate.URI)
			if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() != port {
				continue
			}
			ip, _, ok := remote.ParsePlexDirectHost(u.Hostname())
			if !candidate.Local || !ok || !ip.Equal(localIP) {
				continue
			}
			u.Path = strings.TrimRight(u.Path, "/") + "/ether/me"
			return u.String(), nil
		}
	}
	secure := make([]remote.Connection, 0, len(connections))
	for _, candidate := range connections {
		u, err := url.Parse(candidate.URI)
		if err == nil && u.Scheme == "https" {
			secure = append(secure, candidate)
		}
	}
	return portalPublicAddress(ctx, secure)
}

// loopbackPlexDirect returns the server's own LAN plex.direct HTTPS address
// when this listener can accept it. A loopback-only bind is not that listener.
func loopbackPlexDirect(host, port string, interfaces []remote.InterfaceAddr, connections []remote.Connection) (string, bool) {
	bound := net.ParseIP(host)
	if bound != nil && bound.IsLoopback() {
		return "", false
	}
	for _, iface := range interfaces {
		if iface.IP == nil || iface.IP.IsLoopback() {
			continue
		}
		if bound != nil && !bound.IsUnspecified() && !bound.Equal(iface.IP) {
			continue
		}
		for _, candidate := range connections {
			u, err := url.Parse(candidate.URI)
			if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() != port {
				continue
			}
			ip, _, ok := remote.ParsePlexDirectHost(u.Hostname())
			if !candidate.Local || !ok || !ip.Equal(iface.IP) {
				continue
			}
			u.Path = strings.TrimRight(u.Path, "/") + "/ether/me"
			return u.String(), true
		}
	}
	return "", false
}

func portalLANAddress(ip netip.Addr, host, port string, interfaces []remote.InterfaceAddr) (string, error) {
	if ip.IsLoopback() {
		bound := net.ParseIP(host)
		if bound != nil && (bound.IsUnspecified() || bound.IsLoopback()) {
			return (&url.URL{Scheme: "http", Host: net.JoinHostPort(ip.String(), port), Path: "/ether/me"}).String(), nil
		}
	}

	for _, candidate := range interfaces {
		if candidate.Net == nil || !candidate.Net.Contains(net.IP(ip.AsSlice())) {
			continue
		}
		bound := net.ParseIP(host)
		if bound != nil && !bound.IsUnspecified() && !bound.Equal(candidate.IP) {
			continue
		}
		u := url.URL{Scheme: "http", Host: net.JoinHostPort(candidate.IP.String(), port), Path: "/ether/me"}
		return u.String(), nil
	}
	return "", errors.New("no reachable LAN address")
}

func portalPublicAddress(ctx context.Context, connections []remote.Connection) (string, error) {
	for _, c := range connections {
		if c.Local || c.Relay {
			continue
		}
		u, err := url.Parse(c.URI)
		if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
			continue
		}
		hostname := u.Hostname()
		if p := net.ParseIP(hostname); p != nil {
			if !remote.IsPublicIP(p) {
				continue
			}
		} else if p, _, ok := remote.ParsePlexDirectHost(hostname); ok {
			if !remote.IsPublicIP(p) {
				continue
			}
		} else {
			ips, e := net.DefaultResolver.LookupIPAddr(ctx, hostname)
			if e != nil || len(ips) == 0 {
				continue
			}
			valid := true
			for _, a := range ips {
				if !remote.IsPublicIP(a.IP) {
					valid = false
					break
				}
			}
			if !valid {
				continue
			}
		}
		u.Path = strings.TrimRight(u.Path, "/") + "/ether/me"
		u.RawPath = ""
		return u.String(), nil
	}
	return "", errors.New("public address unavailable")
}

// secureOrigins reads the same published snapshot as Plex resources. It does
// no reachability probes and never upgrades an untrusted request hostname.
func (r *frontendRuntime) secureOrigins(ctx context.Context, req *http.Request) []string {
	return r.secureOriginsFor(ctx, "", req)
}

func (r *frontendRuntime) secureOriginsFor(ctx context.Context, id frontend.Protocol, req *http.Request) []string {
	if r.app == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	addOrigin := func(origin string) {
		if origin != "" && !seen[origin] {
			out = append(out, origin)
			seen[origin] = true
		}
	}
	includeMain := true
	if r.app.cfg != nil && id != "" {
		cfg := r.app.cfg.Get()
		for _, raw := range cfg.Frontends[string(id)].EffectivePublishedAddresses() {
			u, err := url.Parse(strings.TrimSpace(raw))
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				continue
			}
			p := strings.TrimRight(u.Path, "/")
			if p != "" && p != "/"+string(id) {
				continue
			}
			addOrigin("https://" + u.Host)
		}
		if id == "vela" && cfg.FrontendPorts["vela"] > 0 && !vela.ServeOnMainPort(cfg.Frontends["vela"].Settings) {
			includeMain = false
		}
	}
	if !includeMain {
		return out
	}
	var connections []remote.Connection
	if r.app.registrar != nil {
		connections = r.app.registrar.Status().Connections
	}
	peer, _ := r.app.frontendClientIP(req)
	for _, local := range []bool{identity.LAN(peer), !identity.LAN(peer)} {
		for _, c := range connections {
			if c.Local != local {
				continue
			}
			u, err := url.Parse(c.URI)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
				continue
			}
			addOrigin("https://" + u.Host)
		}
	}
	return out
}
