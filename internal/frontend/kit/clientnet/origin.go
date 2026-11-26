package clientnet

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// RequestOrigin describes the scheme, host, and optional reverse-proxy prefix
// a client used to reach Ether on an HTTP request.
type RequestOrigin struct {
	Scheme  string // "http" or "https"
	Host    string // sanitized host or host:port
	Prefix  string // sanitized X-Forwarded-Prefix ("" or "/foo/bar", never trailing slash)
	Proxied bool   // true when trusted X-Forwarded-{Proto,Host,Prefix} headers were applied
}

// Origin derives the client-visible origin for r. Forwarded headers
// (X-Forwarded-Proto, X-Forwarded-Host, X-Forwarded-Prefix) are honoured only
// when the direct connecting peer is loopback or within trusted.
func Origin(r *http.Request, trusted ...netip.Prefix) (RequestOrigin, bool) {
	if r == nil {
		return RequestOrigin{}, false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := SanitizeHost(r.Host)
	prefix := ""
	proxied := false
	if TrustedPeer(r, trusted...) {
		if fh := ForwardedHost(r); fh != "" {
			host = fh
			proxied = true
		}
		if fp := ForwardedProto(r); fp == "http" || fp == "https" {
			scheme = fp
			proxied = true
		}
		if fx := ForwardedPrefix(r); fx != "" {
			prefix = fx
			proxied = true
		}
	}
	if host == "" {
		return RequestOrigin{}, false
	}
	return RequestOrigin{
		Scheme:  scheme,
		Host:    host,
		Prefix:  prefix,
		Proxied: proxied,
	}, true
}

// BaseURL returns "scheme://host[+prefix]" without a trailing slash, combining
// any trusted X-Forwarded-Prefix with mountPrefix without duplicating an
// already-present trailing mount segment.
func (o RequestOrigin) BaseURL(mountPrefix string) string {
	if o.Scheme == "" || o.Host == "" {
		return ""
	}
	u := url.URL{
		Scheme: o.Scheme,
		Host:   o.Host,
		Path:   JoinPrefix(o.Prefix, mountPrefix),
	}
	return strings.TrimRight(u.String(), "/")
}

// CanonicalBase returns BaseURL normalized for deduplication: lowercased host
// and omitted redundant default ports (:80 for http, :443 for https).
func (o RequestOrigin) CanonicalBase(mountPrefix string) string {
	if o.Scheme == "" || o.Host == "" {
		return ""
	}
	scheme := strings.ToLower(o.Scheme)
	host := o.Host
	if h, p, err := net.SplitHostPort(host); err == nil {
		h = strings.ToLower(strings.Trim(h, "[]"))
		if (scheme == "http" && p == "80") || (scheme == "https" && p == "443") {
			if strings.Contains(h, ":") {
				host = "[" + h + "]"
			} else {
				host = h
			}
		} else {
			host = net.JoinHostPort(h, p)
		}
	} else {
		host = strings.ToLower(host)
	}
	u := url.URL{
		Scheme: scheme,
		Host:   host,
		Path:   JoinPrefix(o.Prefix, mountPrefix),
	}
	return strings.TrimRight(u.String(), "/")
}

// Hostname returns the host without any port or IPv6 brackets.
func (o RequestOrigin) Hostname() string {
	if h, _, err := net.SplitHostPort(o.Host); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(o.Host, "[]")
}

// Port returns the explicit port carried on Host, or 0 if none was specified.
func (o RequestOrigin) Port() int {
	_, p, err := net.SplitHostPort(o.Host)
	if err != nil || p == "" {
		return 0
	}
	n, err := strconv.Atoi(p)
	if err != nil || n <= 0 || n > 65535 {
		return 0
	}
	return n
}

// ForwardedProto returns the first X-Forwarded-Proto value if it is "http" or "https".
func ForwardedProto(r *http.Request) string {
	if r == nil {
		return ""
	}
	p := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if p == "" {
		return ""
	}
	first, _, _ := strings.Cut(p, ",")
	switch strings.ToLower(strings.TrimSpace(first)) {
	case "http":
		return "http"
	case "https":
		return "https"
	default:
		return ""
	}
}

// ForwardedHost returns the sanitized first X-Forwarded-Host entry.
func ForwardedHost(r *http.Request) string {
	if r == nil {
		return ""
	}
	h := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if h == "" {
		return ""
	}
	first, _, _ := strings.Cut(h, ",")
	return SanitizeHost(first)
}

// ForwardedPrefix returns the sanitized first X-Forwarded-Prefix entry.
func ForwardedPrefix(r *http.Request) string {
	if r == nil {
		return ""
	}
	raw := strings.TrimSpace(r.Header.Get("X-Forwarded-Prefix"))
	if raw == "" {
		return ""
	}
	first, _, _ := strings.Cut(raw, ",")
	return SanitizePrefix(first)
}

// SanitizeHost validates and trims an HTTP Host or X-Forwarded-Host value.
func SanitizeHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" || len(host) > 255 {
		return ""
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		if c <= 0x20 || c >= 0x7f {
			return ""
		}
		switch c {
		case '/', '@', '\\', '?', '#', '<', '>', '"', '\'', '(', ')', '|':
			return ""
		}
	}
	if strings.HasSuffix(host, ":") {
		return ""
	}
	if h, p, err := net.SplitHostPort(host); err == nil {
		if h == "" || p == "" {
			return ""
		}
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return ""
		}
	} else if strings.Count(host, ":") > 0 && !(strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]")) {
		// Reject bare unbracketed IPv6 or malformed host:port.
		return ""
	}
	return host
}

// SanitizePrefix validates a reverse-proxy path prefix such as "/media" or
// "/ether/app", stripping trailing slashes and rejecting open-redirect or
// traversal forms ("//evil.com", "/../x", control characters, queries).
func SanitizePrefix(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > 256 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c >= 0x7f {
			return ""
		}
		switch c {
		case '\\', '?', '#', '<', '>', '"', '\'', '|':
			return ""
		}
	}
	if strings.Contains(s, "://") || strings.HasPrefix(s, "//") {
		return ""
	}
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	s = strings.TrimRight(s, "/")
	if s == "" {
		return ""
	}
	parts := strings.Split(s[1:], "/")
	for _, seg := range parts {
		if seg == "" || seg == "." || seg == ".." {
			return ""
		}
	}
	return s
}

// JoinPrefix combines a reverse-proxy prefix with an Ether mount prefix without
// duplicating mountPrefix when forwardedPrefix already ends with it on a path
// segment boundary.
func JoinPrefix(forwardedPrefix, mountPrefix string) string {
	fp := SanitizePrefix(forwardedPrefix)
	mp := SanitizePrefix(mountPrefix)
	if fp == "" {
		return mp
	}
	if mp == "" {
		return fp
	}
	lf, lm := strings.ToLower(fp), strings.ToLower(mp)
	if strings.HasSuffix(lf, lm) {
		return fp
	}
	return fp + mp
}

// RedirectPath builds a safe relative redirect Location combining any trusted
// X-Forwarded-Prefix, mountPrefix, and targetPath (which should start with "/").
func RedirectPath(forwardedPrefix, mountPrefix, targetPath string) string {
	if !strings.HasPrefix(targetPath, "/") {
		targetPath = "/" + targetPath
	}
	prefix := JoinPrefix(forwardedPrefix, mountPrefix)
	out := prefix + targetPath
	if strings.HasPrefix(out, "//") {
		return "/" + strings.TrimLeft(out, "/")
	}
	return out
}
