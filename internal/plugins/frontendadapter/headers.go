package frontendadapter

import (
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plugins"
)

// Viewer-identity and origin headers the proxy injects (spec §6.5). The plugin
// never sees an Ether credential; these are the only Ether-issued values it
// receives.
const (
	HeaderViewer     = "X-Ether-Viewer"
	HeaderViewerName = "X-Ether-Viewer-Name"
	HeaderLibraries  = "X-Ether-Libraries"
	HeaderOrigin     = "X-Ether-Origin"
)

// dockerBridge is the range Docker uses for bridge networks (172.16.0.0/12),
// matching admin.requestOriginInfo so a container-internal Host never leaks
// into X-Ether-Origin.
var dockerBridge = netip.MustParsePrefix("172.16.0.0/12")

// forwardedRequestHeaders is the allowlist of client headers the proxy
// relays to a plugin. Anything not listed is dropped, so an adapter cannot
// be used to smuggle host-internal headers (Range is handled separately so
// media seeking works without trusting arbitrary values).
var forwardedRequestHeaders = []string{
	"Accept",
	"Accept-Language",
	"Accept-Encoding",
	"Content-Type",
	"If-Modified-Since",
	"If-None-Match",
	"User-Agent",
}

// forwardedResponseHeaders is the allowlist of plugin response headers the
// proxy relays to the client.
var forwardedResponseHeaders = []string{
	"Content-Type",
	"Content-Length",
	"Content-Disposition",
	"ETag",
	"Last-Modified",
	"Cache-Control",
	"Accept-Ranges",
	"Content-Range",
	"Retry-After",
	"Vary",
}

// isEtherHeader reports whether a header name belongs to the reserved
// X-Ether-* namespace that must never cross the proxy boundary in either
// direction.
func isEtherHeader(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "x-ether-")
}

// isCredentialHeader reports whether a header carries a credential the
// plugin must never see.
func isCredentialHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "cookie", "x-api-key", "x-auth-token":
		return true
	default:
		return false
	}
}

// outboundHeaders builds the header set the plugin receives: the allowlist
// plus the injected viewer identity and trusted client-visible origin. Inbound
// X-Ether-* (including X-Ether-Origin) and credential headers are never copied.
func outboundHeaders(r *http.Request, auth authenticated, origin string) map[string]string {
	headers := make(map[string]string, len(forwardedRequestHeaders)+5)
	for _, name := range forwardedRequestHeaders {
		if v := r.Header.Get(name); v != "" {
			headers[name] = v
		}
	}
	if rng := r.Header.Get("Range"); rng != "" && validRange(rng) {
		headers["Range"] = rng
	}
	headers[HeaderViewer] = strconv.FormatInt(auth.viewer.ID, 10)
	headers[HeaderViewerName] = viewerName(auth.viewer)
	headers[HeaderLibraries] = libraryCSV(auth.libraries)
	if origin != "" {
		headers[HeaderOrigin] = origin
	}
	return headers
}

// requestOrigin derives the client-visible origin ("scheme://host[:port][/prefix]")
// for r using the host's trusted-proxy policy, matching admin.requestOriginInfo.
// Untrusted forwarding headers are ignored and container bridge addresses are
// rejected so an internal Docker Host never leaks.
func requestOrigin(r *http.Request, host frontend.Host) string {
	origin, ok := host.Origin(r)
	if !ok || containerHost(origin.Hostname()) {
		return ""
	}
	return origin.BaseURL("")
}

func containerHost(host string) bool {
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false
	}
	return dockerBridge.Contains(ip.Unmap())
}

// viewerName is the display name a plugin may show. Ether owns it; a plugin
// must not infer identity from anything else.
func viewerName(v *core.Viewer) string {
	if v == nil {
		return ""
	}
	if name := strings.TrimSpace(v.Title); name != "" {
		return name
	}
	return strings.TrimSpace(v.Username)
}

// libraryCSV renders the allowed library ids as a comma-separated list.
func libraryCSV(ids []core.LibraryID) string {
	if len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(int64(id), 10)
	}
	return strings.Join(parts, ",")
}

// validRange accepts a single byte range so a plugin cannot be handed a
// header that would confuse its reader; malformed ranges are dropped and
// the plugin sees a full request.
func validRange(v string) bool {
	const prefix = "bytes="
	if !strings.HasPrefix(v, prefix) {
		return false
	}
	spec := strings.TrimPrefix(v, prefix)
	if spec == "" || strings.Contains(spec, ",") {
		return false
	}
	start, end, ok := strings.Cut(spec, "-")
	if !ok {
		return false
	}
	if start == "" && end == "" {
		return false
	}
	if start != "" {
		if _, err := strconv.ParseInt(start, 10, 64); err != nil {
			return false
		}
	}
	if end != "" {
		if _, err := strconv.ParseInt(end, 10, 64); err != nil {
			return false
		}
	}
	return true
}

// Redirects stay on this origin. The host's minted media tickets are root-relative;
// credentials, authority changes and browser URL normalization tricks are refused.
func copyRedirect(w http.ResponseWriter, resp plugins.Response) {
	switch resp.Status {
	case 301, 302, 303, 307, 308:
	default:
		return
	}
	v := headerValue(resp.Headers, "Location")
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return
	}
	decoded, err := url.PathUnescape(v)
	if err != nil {
		return
	}
	for _, c := range decoded {
		if c <= 32 || c == 127 || c == '\\' {
			return
		}
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return
	}
	for key := range q {
		switch strings.ToLower(key) {
		case "token", "x-plex-token", "access_token", "refresh_token", "api_key", "apikey", "password", "authorization", "x-auth-token":
			return
		}
	}
	w.Header().Set("Location", v)
	w.Header().Set("Referrer-Policy", "no-referrer")
}
