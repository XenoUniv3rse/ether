package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/stremio"
	qrcode "github.com/skip2/go-qrcode"
)

// Protocol identities exposed by the console app chooser.
var connectableSurfaces = []string{"jellyfin", "emby", "silo", "stremio", "webdav", "m3u", "xtream"}

type connectAdapterMeta struct {
	Name string `json:"name"`
	Logo string `json:"logo,omitempty"`
	Auth string `json:"auth,omitempty"`
	// ProtocolHint lets the chooser draw a known protocol's mark (Subsonic).
	ProtocolHint string `json:"protocolHint,omitempty"`
}

func isConnectablePluginAdapter(d frontend.Descriptor) bool {
	if !strings.HasPrefix(string(d.ID), "plugin:") {
		return false
	}
	if !strings.HasPrefix(d.PathPrefix, "/") || d.PathPrefix == "/" {
		return false
	}
	switch d.Auth {
	case "", "ether_token", "basic", "query_credentials":
		return true
	default:
		return false
	}
}

func connectAppUsername(viewer *core.Viewer) string {
	if viewer == nil {
		return ""
	}
	if u := strings.TrimSpace(viewer.Username); u != "" {
		return u
	}
	if t := strings.TrimSpace(viewer.Title); t != "" {
		return t
	}
	return strconv.FormatInt(viewer.ID, 10)
}

func (h *Handler) portalConnectApps(w http.ResponseWriter, r *http.Request) {
	viewer, err := h.portalViewer(r)
	if err != nil || viewer == nil {
		writeError(w, 401, "unauthorized", "sign in required")
		return
	}
	h.connectApps(w, r, viewer)
}

func (h *Handler) ownerConnectApps(w http.ResponseWriter, r *http.Request) {
	access, ok := RequestAccess(r.Context())
	if !ok || !access.Owner || !h.isOwner(r, tokenFromRequest(r)) || h.deps.ActiveViewer == nil {
		writeError(w, 403, "forbidden", "owner required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 400, "invalid", "invalid viewer")
		return
	}
	viewer, err := h.deps.ActiveViewer(r.Context(), id)
	if err != nil || viewer == nil {
		writeError(w, 404, "not_found", "viewer unavailable")
		return
	}
	h.connectApps(w, r, viewer)
}

func (h *Handler) connectApps(w http.ResponseWriter, r *http.Request, viewer *core.Viewer) {
	w.Header().Set("Cache-Control", "no-store")
	if h.deps.Frontends == nil || h.deps.IdentityGrants == nil {
		writeError(w, 503, "unavailable", "apps unavailable")
		return
	}
	infos, err := h.deps.Frontends.List(r.Context())
	if err != nil {
		writeError(w, 503, "unavailable", "apps unavailable")
		return
	}
	allowed := map[string]bool{}
	for _, s := range connectableSurfaces {
		allowed[s] = true
	}
	enabled := map[string]bool{}
	pluginAdapters := map[string]frontend.Descriptor{}
	var pluginOrder []string
	for _, info := range infos {
		if !info.Enabled {
			continue
		}
		id := string(info.Descriptor.ID)
		if allowed[id] || consoleFrontendPolicy(id).ExternalIdentity {
			enabled[id] = true
		} else if isConnectablePluginAdapter(info.Descriptor) {
			enabled[id] = true
			allowed[id] = true
			pluginAdapters[id] = info.Descriptor
			pluginOrder = append(pluginOrder, id)
		}
	}
	sort.Strings(pluginOrder)
	if r.Method == http.MethodGet {
		out := make([]string, 0, len(enabled))
		// The externally managed account frontend appears first in the chooser.
		if enabled["plex"] {
			out = append(out, "plex")
		}
		for _, id := range connectableSurfaces {
			if enabled[id] {
				out = append(out, id)
			}
		}
		out = append(out, pluginOrder...)
		resp := map[string]any{"apps": out}
		if len(pluginOrder) > 0 {
			adapters := make(map[string]connectAdapterMeta, len(pluginOrder))
			for _, id := range pluginOrder {
				d := pluginAdapters[id]
				auth := d.Auth
				if auth == "" {
					auth = "ether_token"
				}
				name := strings.TrimSpace(d.Name)
				if name == "" {
					name = id
				}
				logo := d.Logo
				// Package icons are served on the owner API, which a
				// viewer's browser cannot load; the chooser's glyph stands in.
				if strings.HasPrefix(logo, "/ether/api/plugins/") {
					logo = ""
				}
				adapters[id] = connectAdapterMeta{Name: name, Logo: logo, Auth: auth, ProtocolHint: d.ProtocolHint}
			}
			resp["adapters"] = adapters
		}
		writeJSON(w, 200, resp)
		return
	}
	if !csrfOK(r) {
		writeError(w, 403, "csrf", "request rejected")
		return
	}
	surface := r.PathValue("surface")
	if !enabled[surface] || !allowed[surface] {
		writeError(w, 404, "not_found", "app unavailable")
		return
	}
	pluginDesc, isPlugin := pluginAdapters[surface]
	pathPrefix := "/" + surface
	if isPlugin {
		pathPrefix = pluginDesc.PathPrefix
	}
	u, err := h.resolveAppAddress(r.Context(), r, surface, pathPrefix, surface == "stremio")
	if err != nil {
		if surface == "stremio" {
			writeError(w, 503, "stremio_https_required", "Stremio needs HTTPS. Ask the server owner to give this server a secure (HTTPS) address, then try again.")
			return
		}
		writeError(w, 503, "address_unavailable", "Server address unavailable. Ask the server owner to set a reachable server address, then try again.")
		return
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		writeError(w, 503, "unavailable", "app unavailable")
		return
	}
	clientName := surface
	if isPlugin && strings.TrimSpace(pluginDesc.Name) != "" {
		clientName = strings.TrimSpace(pluginDesc.Name)
	}
	device := core.Device{ID: surface + "-" + hex.EncodeToString(id[:]), Client: clientName, Product: clientName}
	var secret, xtreamUsername, xtreamPassword string
	appLogin := surface == "xtream" || isPlugin && pluginDesc.Auth == "query_credentials"
	if appLogin {
		xtreamUsername, xtreamPassword, secret, err = h.deps.IdentityGrants.IssueAppLogin(r.Context(), viewer.ID, surface, device)
	} else {
		secret, err = h.deps.IdentityGrants.Issue(r.Context(), viewer.ID, nil, surface, device)
	}
	if err != nil {
		writeError(w, 404, "not_found", "viewer unavailable")
		return
	}
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	basePath := strings.TrimRight(u.Path, "/")
	result := map[string]string{"surface": surface}
	switch {
	case consoleFrontendPolicy(surface).PasswordConnect:
		u.Path = basePath
		result["url"] = strings.TrimRight(u.String(), "/")
		result["username"] = connectAppUsername(viewer)
		result["password"] = secret
	case surface == "stremio":
		u.Path = basePath + "/" + secret + "/manifest.json"
		result["url"] = u.String()
	case surface == "xtream":
		u.Path = basePath
		result["url"] = strings.TrimRight(u.String(), "/")
		result["username"] = xtreamUsername
		result["password"] = xtreamPassword
	case surface == "m3u":
		u.Path = basePath + "/" + secret + "/playlist.m3u"
		result["url"] = u.String()
	case surface == "webdav":
		u.Path = basePath + "/"
		result["url"] = u.String()
		result["username"] = strconv.FormatInt(viewer.ID, 10)
		result["password"] = secret
	case isPlugin:
		u.Path = basePath
		result["url"] = strings.TrimRight(u.String(), "/")
		if appLogin {
			result["username"] = xtreamUsername
			result["password"] = xtreamPassword
		} else if pluginDesc.Auth == "basic" {
			result["username"] = connectAppUsername(viewer)
			result["password"] = secret
		} else {
			result["token"] = secret
		}
	}
	qrValue := result["url"]
	if surface == "webdav" {
		qrURL := *u
		qrURL.User = url.UserPassword(result["username"], secret)
		qrValue = qrURL.String()
	}
	png, err := qrcode.Encode(qrValue, qrcode.Medium, 256)
	if err != nil {
		hash := sha256.Sum256([]byte(secret))
		_ = h.deps.IdentityGrants.RevokeGrant(r.Context(), viewer.ID, hex.EncodeToString(hash[:]))
		writeError(w, 503, "unavailable", "could not create connection code")
		return
	}
	result["qr"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	writeJSON(w, 200, result)
}

// resolveAppAddress delegates to the shared app-level address resolver when
// wired (Deps.AppAddress), and applies the exact same published-address then
// server-origin+path fallback when Deps.AppAddress is absent in unit tests.
func (h *Handler) resolveAppAddress(ctx context.Context, r *http.Request, surface, pathPrefix string, requireHTTPS bool) (*url.URL, error) {
	if h.deps.AppAddress != nil {
		raw, err := h.deps.AppAddress(ctx, frontend.Protocol(surface), r, requireHTTPS)
		if err != nil {
			return nil, err
		}
		u, err := url.Parse(raw)
		if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || (requireHTTPS && u.Scheme != "https") || u.Host == "" {
			return nil, errors.New("app address unavailable")
		}
		u.RawPath, u.RawQuery, u.Fragment = "", "", ""
		return u, nil
	}
	if h.deps.Config != nil {
		for _, raw := range frontendPublishedURLs(h.deps.Config.Get(), surface) {
			parsed, err := url.Parse(raw)
			if err != nil || parsed == nil || parsed.Host == "" {
				continue
			}
			if requireHTTPS && parsed.Scheme != "https" {
				continue
			}
			if surface == "stremio" {
				parsed.Path = stremio.AddonBasePath(parsed.Path)
			}
			parsed.RawPath, parsed.RawQuery, parsed.Fragment = "", "", ""
			return parsed, nil
		}
	}
	address := h.deps.ViewerPortalURL
	if requireHTTPS {
		address = h.deps.StremioPortalURL
	}
	if address == nil {
		return nil, errors.New("portal address unavailable")
	}
	portal, err := address(ctx, r)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(portal)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || (requireHTTPS && u.Scheme != "https") || u.Host == "" || !strings.HasSuffix(u.Path, "/ether/me") {
		return nil, errors.New("portal address unavailable")
	}
	u.Path = strings.TrimSuffix(u.Path, "/ether/me") + pathPrefix
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	return u, nil
}

// Credentials are decrypted only after owner authentication and explicit CSRF admission.
// xtreamServerURL is the server address IPTV apps sign in to, or "" when the
// server has no reachable viewer address. It is not secret, so existing logins
// can show it after the one-time create response is gone.
func (h *Handler) xtreamServerURL(r *http.Request) string {
	u, err := h.resolveAppAddress(r.Context(), r, "xtream", "/xtream", false)
	if err != nil {
		return ""
	}
	return strings.TrimRight(u.String(), "/")
}

// adapterServerURL is the address a plugin adapter's clients sign in to, or ""
// when the adapter is not enabled or the server has no reachable address.
func (h *Handler) adapterServerURL(r *http.Request, surface string) string {
	if h.deps.Frontends == nil {
		return ""
	}
	infos, err := h.deps.Frontends.List(r.Context())
	if err != nil {
		return ""
	}
	for _, info := range infos {
		if string(info.Descriptor.ID) != surface || !info.Enabled || !isConnectablePluginAdapter(info.Descriptor) {
			continue
		}
		u, err := h.resolveAppAddress(r.Context(), r, surface, info.Descriptor.PathPrefix, false)
		if err != nil {
			return ""
		}
		return strings.TrimRight(u.String(), "/")
	}
	return ""
}

func (h *Handler) ownerXtreamLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	access, ok := RequestAccess(r.Context())
	if !ok || !access.Owner || !h.isOwner(r, tokenFromRequest(r)) || h.deps.IdentityGrants == nil {
		writeError(w, 403, "forbidden", "owner required")
		return
	}
	if !csrfOK(r) {
		writeError(w, 403, "csrf", "request rejected")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 400, "invalid", "invalid viewer")
		return
	}
	var body struct {
		Hash    string `json:"hash"`
		Limit   int    `json:"limit"`
		Surface string `json:"surface"`
	}
	if decodeJSON(r, &body) != nil {
		writeError(w, 400, "invalid", "invalid request")
		return
	}
	// Reveal also serves plugin adapters' app logins (query credentials);
	// the stream limit stays Xtream-only.
	surface := "xtream"
	if body.Surface != "" && !strings.HasSuffix(r.URL.Path, "/limit") {
		if !strings.HasPrefix(body.Surface, "plugin:") {
			writeError(w, 400, "invalid", "invalid request")
			return
		}
		surface = body.Surface
	}
	if strings.HasSuffix(r.URL.Path, "/limit") {
		if h.deps.IdentityGrants.SetXtreamStreamLimit(r.Context(), id, body.Hash, body.Limit) != nil {
			writeError(w, 400, "invalid", "invalid request")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	username, password, available, err := h.deps.IdentityGrants.RevealAppLogin(r.Context(), id, surface, body.Hash)
	if err != nil {
		writeError(w, 404, "not_found", "login unavailable")
		return
	}
	serverURL := h.xtreamServerURL(r)
	if surface != "xtream" {
		serverURL = h.adapterServerURL(r, surface)
	}
	writeJSON(w, 200, map[string]any{"available": available, "url": serverURL, "username": username, "password": password})
}
