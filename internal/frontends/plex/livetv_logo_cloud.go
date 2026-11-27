package plex

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/clientnet"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Plex's Bedrock guide (Apple TV, iOS/iPadOS, and Android) draws guide channel
// logos only from URLs that Plex's cloud image service can fetch: it never
// loads them from the server itself, so a logo on a LAN address never appears
// (#391, proven on Apple TV and reproduced on Plex for Android Mobile 2026.23.0:
// logos rendered remotely over WAN, drew blank on LAN, and the server saw no
// image request at all). Whatever is published there leaves the network, so it
// must never carry a viewer's token.

// signedLogoPrefix serves a channel's logo without a Plex token, to a URL
// signed by this server.
const signedLogoPrefix = "/livetv/logos/"

func cloudGuideLogoClient(pr *Request) bool {
	// Plex's own Apple TV, iOS/iPadOS, and Android apps share the Bedrock
	// guide and fetch channel logos through Plex's cloud. Third-party apps on
	// the same platforms (Plezy sends Platform iOS/tvOS/Android) and Plex Web
	// load logos from this server directly or through /photo/:/transcode and
	// need its ordinary logo URL.
	if pr == nil || strings.EqualFold(pr.Product, "Plex Web") {
		return false
	}
	if strings.EqualFold(pr.Product, "Plex for Apple TV") || strings.EqualFold(pr.Product, "Plex for iOS") || plexAndroidClient(pr) {
		return true
	}
	cloud := strings.EqualFold(pr.Platform, "tvOS") || strings.EqualFold(pr.Platform, "iOS") || strings.EqualFold(pr.Platform, "iPadOS") || strings.EqualFold(pr.Platform, "Android")
	return cloud && (pr.Product == "" || strings.HasPrefix(strings.ToLower(pr.Product), "plex"))
}

// applyGuideChannelThumb rewrites an airing's channelThumb to the same
// cloud-fetchable logo URL as the lineup when the client's guide fetches
// channel logos through Plex's cloud.
func (s *Server) applyGuideChannelThumb(r *http.Request, md *model.Metadata, c frontend.LiveTVChannel) {
	if c.ID == "" || !cloudGuideLogoClient(FromContext(r.Context())) || len(md.Media) == 0 {
		return
	}
	logo := s.liveGuideLogo(r, c)
	for i, a := range md.Media[0].Extra {
		if a.Name == "channelThumb" {
			md.Media[0].Extra[i].Value = logo
		}
	}
}

type guideLogosKey struct{}

func (s *Server) withGuideLogos(r *http.Request, channels []frontend.LiveTVChannel) *http.Request {
	ctx := r.Context()
	if !cloudGuideLogoClient(FromContext(ctx)) || r.Host == "" || s.svc().liveTV == nil {
		return r
	}
	var missing []frontend.LiveTVChannel
	for _, c := range channels {
		if c.Logo != "" || c.GuideID == "" {
			continue
		}
		missing = append(missing, c)
	}
	if len(missing) <= 1 {
		return r
	}
	logos := s.svc().liveTV.GuideChannelLogos(ctx, missing)
	return r.WithContext(context.WithValue(ctx, guideLogosKey{}, logos))
}

func (s *Server) withProgrammeGuideLogos(r *http.Request, programmes []frontend.LiveTVProgramme, byID map[string]frontend.LiveTVChannel) *http.Request {
	if !cloudGuideLogoClient(FromContext(r.Context())) {
		return r
	}
	seen := make(map[string]bool)
	var channels []frontend.LiveTVChannel
	for _, programme := range programmes {
		c, ok := byID[programme.ChannelID]
		if ok && !seen[c.ID] {
			seen[c.ID] = true
			channels = append(channels, c)
		}
	}
	return s.withGuideLogos(r, channels)
}

// cloudGuideLogo is the logo URL for a client whose guide fetches logos
// through Plex's cloud: the channel's own logo when that is already public
// (as PMS publishes XMLTV icons), a shipped channel's logo from Ether's
// public repository at this build's release, else this server's signed,
// token-free logo URL on its public address when remote access publishes
// one, or on origin.
func (s *Server) cloudGuideLogo(ctx context.Context, c frontend.LiveTVChannel, origin string) string {
	if publicLogoURL(c.Logo) {
		return c.Logo
	}
	if path, _, ok := frontend.ShowcaseLogo(c); ok && s.getHost().RepoFileURL != nil {
		if u := s.getHost().RepoFileURL(path); u != "" {
			return u
		}
	}
	if c.Logo == "" && s.svc().liveTV != nil {
		logos, batched := ctx.Value(guideLogosKey{}).(map[string]string)
		guide := logos[c.ID]
		if !batched {
			guide = s.svc().liveTV.GuideChannelLogo(ctx, c)
		}
		if publicLogoURL(guide) {
			return guide
		}
	}
	base := origin
	if s.getHost().PublicOrigin != nil {
		if public := strings.TrimRight(s.getHost().PublicOrigin(), "/"); public != "" {
			base = public
		}
	}
	return base + signedLogoPrefix + url.PathEscape(c.ID) + "/" + s.liveLogoSignature(c.ID) + ".png"
}

// publicLogoURL reports an http(s) image URL on a public host that Plex's
// cloud can fetch and draw as is (not SVG, which only this server rasterizes).
// Hosts are judged by name, without a DNS lookup on the request path.
func publicLogoURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Host == "" || strings.HasSuffix(strings.ToLower(u.Path), ".svg") {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if ip := net.ParseIP(host); ip != nil {
		return clientnet.IsPublicIP(ip)
	}
	if ip, _, ok := clientnet.ParsePlexDirectHost(host); ok {
		return clientnet.IsPublicIP(ip)
	}
	if !strings.Contains(host, ".") {
		return false
	}
	for _, suffix := range []string{".local", ".lan", ".home", ".home.arpa", ".internal", ".localdomain", ".localhost", ".corp", ".intranet"} {
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	return true
}

// liveLogoSignature authorizes signedLogoPrefix for one channel. The key is
// derived per node and survives restarts, so cached guide URLs stay valid.
func (s *Server) liveLogoSignature(id string) string {
	var key []byte
	if s.svc().liveTV != nil {
		key = s.svc().liveTV.LogoKey()
	}
	if len(key) == 0 {
		s.liveLogoKeyOnce.Do(func() {
			s.liveLogoKey = make([]byte, 32)
			_, _ = rand.Read(s.liveLogoKey)
		})
		key = s.liveLogoKey
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte("ether-live-logo-v1\x00" + id))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:18])
}

// handleSignedLiveLogo serves a channel's logo to a signed URL, without a
// viewer: the signature was minted for a viewer allowed that channel, and a
// logo reveals nothing beyond what the guide that carried it did.
func (s *Server) handleSignedLiveLogo(w http.ResponseWriter, r *http.Request) {
	sig, ok := strings.CutSuffix(chi.URLParam(r, "signature"), ".png")
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.serveSignedLiveLogo(w, r, chi.URLParam(r, "channel"), sig)
}

// serveSignedLiveLogo serves one channel's logo when sig is this server's
// signature for it.
func (s *Server) serveSignedLiveLogo(w http.ResponseWriter, r *http.Request, id, sig string) {
	if !s.liveTVEnabled() || !hmac.Equal([]byte(sig), []byte(s.liveLogoSignature(id))) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	c, err := s.svc().liveTV.Channel(r.Context(), id)
	if err != nil || !c.Enabled {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.writeChannelLogo(w, r, c, true, "public, max-age=86400")
}
