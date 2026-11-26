package plex

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	kitimages "github.com/braedonsaunders/ether/internal/frontend/kit/images"
)

// setArtworkHeaders keeps remote SVG (or an upstream HTML error page) from
// executing on Ether's origin when opened directly. Plex and the owner UI share
// an origin, and the latter stores its owner credential in localStorage. Image
// requests can carry a viewer's query token, so viewer authentication alone does
// not make an active artwork document safe for the owner's browser.
func setArtworkHeaders(h http.Header, contentType string) {
	h.Set("Content-Type", contentType)
	h.Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
}

// ---------------------------------------------------------------------------
// Artwork cache: an in-memory LRU of fetched TMDB images, with request
// coalescing so a grid of posters is fetched once.

func tmdbSize(w int) string { return kitimages.TMDBSize(w) }

var tmdbSizeRe = regexp.MustCompile(`/t/p/(w\d+|h\d+|original)/`)

// resizeTMDBURL rewrites the size segment of a TMDB image URL.
func resizeTMDBURL(u string, width int) string {
	if !tmdbSizeRe.MatchString(u) {
		return u
	}
	return tmdbSizeRe.ReplaceAllString(u, "/t/p/"+tmdbSize(width)+"/")
}

// ---------------------------------------------------------------------------
// Handlers

// artPath picks the TMDB path for an item's element.
type refreshingArtworkCatalog struct{ frontend.Catalog }

// memoryTitles is the catalogue's read of titles held without a row
// (catalog.MemoryTitle).
type memoryTitles interface {
	MemoryTitle(context.Context, frontend.RatingKey) (*frontend.Title, bool, error)
}

// StoredTitle reads a stored title through Get, which refreshes a stale
// row. A search or list hit without a row is read from memory: fetching its
// thumbnail must not promote it into the library (Get would).
func (c refreshingArtworkCatalog) StoredTitle(ctx context.Context, key frontend.RatingKey) (*frontend.Title, error) {
	if m, ok := c.Catalog.(memoryTitles); ok {
		t, stored, err := m.MemoryTitle(ctx, key)
		switch {
		case err != nil:
			return nil, err
		case t != nil:
			return t, nil
		case !stored:
			return nil, frontend.ErrNotFound
		}
	}
	return c.Get(ctx, key)
}

// UndatedSearchHit forwards catalog.UndatedSearchHit to the artwork source.
func (c refreshingArtworkCatalog) UndatedSearchHit(ctx context.Context, key frontend.RatingKey) bool {
	u, ok := c.Catalog.(frontend.UndatedSearchHitCatalog)
	return ok && u.UndatedSearchHit(ctx, key)
}

// serveGeneratedPoster serves Ether's poster for a movie or show the viewer
// may see that has no artwork. Seasons and episodes point at their show's
// thumb, so they reach this through the show.
func (s *Server) serveGeneratedPoster(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, element string, width int) bool {
	if s.svc().generatedPosters == nil || (element != "thumb" && element != "poster") || (key.Kind() != frontend.KindMovie && key.Kind() != frontend.KindShow) {
		return false
	}
	ctx := r.Context()
	t, err := (refreshingArtworkCatalog{s.catalog()}).StoredTitle(ctx, key)
	if err != nil || t == nil || t.Key != key || t.Adult || t.IsFutureRelease(s.now()) || !s.visibleTitle(ctx, FromContext(ctx).User, t) {
		return false
	}
	data, ct, ok, err := frontend.RenderTitlePoster(ctx, s.svc().generatedPosters, t, width)
	if !ok || err != nil {
		return false
	}
	if data, ct, err = s.androidPhotoFormat(r, data, ct); err != nil {
		return false
	}
	h := w.Header()
	h.Add("Vary", "X-Plex-Product")
	h.Add("Vary", "X-Plex-Token")
	writeArtworkBytes(w, r, data, ct, kitimages.Revalidate)
	return true
}

func (s *Server) artPath(ctx context.Context, key frontend.RatingKey, element string) (string, int, error) {
	cfg := (FrontendArtwork{Server: s}).cfg()
	cfg.Catalog = refreshingArtworkCatalog{s.catalog()}
	pr := FromContext(ctx)
	cfg.CollectionDevice = pr.Device
	return frontend.NewArtworkSource(cfg).ArtPath(ctx, frontend.ArtworkRequest{Viewer: pr.User, Item: key, Device: frontend.Device{Product: pr.Product, Platform: pr.Platform}}, element)
}

// handleImage serves /library/metadata/{key}/{element}[/{ts}] and the
// collection/playlist equivalents.
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	key, err := frontend.ParseRatingKey(chi.URLParam(r, "key"))
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	element := chi.URLParam(r, "element")
	switch element {
	case "thumb", "art", "poster", "banner", "clearLogo", "logo", "background", "composite", "squareArt":
	default:
		s.handleUnknown(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/library/metadata/") && !r.URL.Query().Has("ranked") {
		if _, kind, lang, ok := decodeDiscoveryArtworkToken(chi.URLParam(r, "ts")); ok && discoveryElementMatchesKind(element, kind) {
			s.serveDiscoveryPoster(w, r, key, kind, lang)
			return
		}
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("width"))
	s.serveItemImage(w, r, key, element, width)
}

func (s *Server) serveItemImage(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, element string, width int) {
	if r.URL.Query().Get("collectionCard") == "1" && key.Kind() == frontend.KindSmart {
		s.serveCardArtwork(w, r, key, element, width)
		return
	}
	if r.URL.Query().Has("ranked") {
		s.serveRankedArtwork(w, r, key, width)
		return
	}
	if reader, ok := s.getHost().Artwork.(frontend.OwnerArtworkReader); ok {
		kind := ""
		switch element {
		case "poster", "thumb":
			kind = frontend.ImagePoster
		case "art", "background":
			kind = frontend.ImageBackdrop
		}
		if kind != "" {
			if data, ct, err := reader.OwnerArtwork(r.Context(), frontend.ArtworkRequest{Viewer: FromContext(r.Context()).User, Item: key}, kind); err == nil {
				writeArtworkBytes(w, r, data, ct, kitimages.Revalidate)
				return
			}
		}
	}

	if key.IsPreroll() && s.prerollThumb(w, r, key) {
		return
	}
	if element == "composite" || element == "squareArt" {
		element = "thumb"
	}
	height, _ := strconv.Atoi(r.URL.Query().Get("height"))
	if key.IsPluginKey() && s.servePluginItemImage(w, r, key, element, width, height) {
		return
	}
	raw := r.URL.Query().Get("collection")
	if raw == "" && s.serveCollectionArtwork(w, r, key, element, width) {
		return
	}
	path, defWidth, err := s.artPath(r.Context(), key, element)
	if raw != "" || err != nil || path == "" {
		t, _, e := s.lookupPlaceholder(r.Context(), FromContext(r.Context()), key, raw)
		if e != nil && raw == "" {
			t, e = s.scheduledEpisode(r.Context(), FromContext(r.Context()), key)
		}
		if e != nil {
			if s.servePlexOverlayImage(w, r, key, element) || (raw == "" && s.serveGeneratedPoster(w, r, key, element, width)) {
				return
			}
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		path := t.PosterPath
		if t.Type == frontend.TypeEpisode {
			path = t.StillPath
		}
		if element == "art" || element == "background" {
			path = t.BackdropPath
		}
		if element != "thumb" && element != "poster" && element != "art" && element != "background" {
			path = ""
		}
		if path == "" {
			if s.servePlexOverlayImage(w, r, key, element) || (raw == "" && s.serveGeneratedPoster(w, r, key, element, width)) {
				return
			}
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if width <= 0 {
			width = unsizedImageWidth(element, 500)
		}
		s.serveRemoteImage(w, r, s.svc().imageURL(path, tmdbSize(width)))
		return
	}
	if width <= 0 {
		width = unsizedImageWidth(element, defWidth)
	}
	s.serveRemoteImage(w, r, s.svc().imageURL(path, tmdbSize(width)))
}

// unsizedImageWidth is the source width for an image request that names no
// size. Clients such as Plezy load /thumb and /art without transcode
// parameters; the TMDB original is a 2-8 MB 4K file that took up to 8 s per
// image. Serve what a 1080p TV can show instead: w1280 art, w780 for posters
// and stills, or the artwork source's own smaller default.
func unsizedImageWidth(element string, def int) int {
	if element == "art" || element == "background" {
		return 1280
	}
	if def > 0 && def <= 780 {
		return def
	}
	return 780
}

// servePlexOverlayImage is the last-resort artwork path: only when the owner
// toggle is on and this key already has a cached overlay URL for the element.
func (s *Server) servePlexOverlayImage(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, element string) bool {
	remote := s.plexMetaRemoteImage(r.Context(), key, element)
	if remote == "" {
		return false
	}
	// This runs when the catalogue art path refused or had nothing, which
	// includes titles this viewer may not see: check before serving.
	t, err := s.catalog().Get(r.Context(), key)
	if err != nil || t == nil || t.Adult || !t.IsReleased(s.now()) || !s.visibleTitle(r.Context(), FromContext(r.Context()).User, t) {
		return false
	}
	if err := s.writeRemoteImage(w, r, remote); err != nil {
		writeHTTPError(w, http.StatusNotFound)
	}
	return true
}

func (s *Server) serveRemoteImage(w http.ResponseWriter, r *http.Request, u string) {
	if err := s.writeRemoteImage(w, r, u); err != nil {
		if s.svc().images != nil && s.svc().images.IsNotFound(err) {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if r.Context().Err() == nil {
			s.log.Debug("image fetch failed", "url", u, "err", err)
		}
		writeHTTPError(w, http.StatusBadGateway)
	}
}

func (s *Server) writeRemoteImage(w http.ResponseWriter, r *http.Request, u string) error {
	img, err := s.svc().images.Get(r.Context(), u, s.now())
	if err != nil {
		return err
	}
	data, ct, err := s.androidPhotoFormat(r, s.fitPhoto(r.Context(), img.Data, img.ContentType), img.ContentType)
	if err != nil {
		return err
	}
	h := w.Header()
	h.Add("Vary", "X-Plex-Product")
	h.Add("Vary", "X-Plex-Token")
	cacheControl := kitimages.Revalidate
	if explicit, _ := r.Context().Value(explicitPhotoURLKey{}).(bool); explicit {
		if cache, ok := s.svc().images.(frontend.ArtworkImmutableURLCache); ok && cache.IsImmutableURL(u) {
			cacheControl = kitimages.Immutable
		}
	}
	writeArtworkBytes(w, r, data, ct, cacheControl)
	return nil
}

func (s *Server) cachePhotoVariant(ctx context.Context, data []byte, contentType, key string, render func(context.Context) ([]byte, string, error)) ([]byte, string, error) {
	if cache, ok := s.svc().images.(kitimages.VariantCache); ok {
		return cache.Variant(ctx, data, contentType, key, render)
	}
	return render(ctx)
}

type explicitPhotoURLKey struct{}

func writeArtworkBytes(w http.ResponseWriter, r *http.Request, data []byte, contentType, cacheControl string) {
	h := w.Header()
	setArtworkHeaders(h, contentType)
	h.Set("Cache-Control", cacheControl)
	if kitimages.NoStore(r.Context()) {
		h.Set("Cache-Control", "private, no-store")
	} else if kitimages.NotModified(w, r, kitimages.ETag(data, contentType)) {
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

// Person artwork is scoped to one title so authorization requires one catalog lookup.
func (s *Server) servePersonImage(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, kind, identity string) {
	s.servePersonImageSized(w, r, key, kind, identity, 0, 0)
}

func (s *Server) servePersonImageSized(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, kind, identity string, width, height int) {
	if r.Context().Err() != nil {
		return
	}
	t, err := s.artworkTitle(r.Context(), key)
	if r.Context().Err() != nil || err != nil || t == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if key.IsPluginKey() {
		if !s.canSeePluginTitle(FromContext(r.Context()).User, t) {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
	} else if !t.IsReleased(s.now()) || !s.visibleTitle(r.Context(), FromContext(r.Context()).User, t) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	var people []frontend.Person
	switch kind {
	case "actor":
		people = t.Cast
	case "director":
		people = t.Directors
	case "writer":
		people = t.Writers
	case "producer":
		people = t.Crew
	default:
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(identity)
	if err != nil || len(raw) == 0 {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	var (
		imagePath     string
		matchedPerson frontend.Person
	)
	for _, person := range people {
		if person.ThumbPath == string(raw) {
			imagePath = person.ThumbPath
			matchedPerson = person
			break
		}
	}
	if imagePath == "" {
		if ov, ok := s.plexMetaCached(r.Context(), key); ok {
			for _, person := range overlayPeople(ov, kind) {
				if person.Thumb == string(raw) {
					imagePath = person.Thumb
					break
				}
			}
		}
	}
	if imagePath == "" {
		if credits, ok := s.plexCreditsCached(r.Context(), key); ok && credits.HasThumb(string(raw)) {
			imagePath = string(raw)
		}
	}
	if imagePath == "" {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if key.IsPluginKey() && (strings.HasPrefix(imagePath, "https://") || strings.HasPrefix(imagePath, "http://")) {
		s.servePluginPersonImage(w, r, key, matchedPerson.TMDB, imagePath, width, height)
		return
	}
	if plexImageURL(imagePath) {
		s.serveRemoteImage(w, r, imagePath)
		return
	}
	// Keep the same w185 profile URL emitted by metadata before proxying; poster widths are not profile configuration. Width and height are intentionally ignored.
	s.serveRemoteImage(w, r, s.svc().imageURL(imagePath, "w185"))
}

func (s *Server) handlePersonImage(w http.ResponseWriter, r *http.Request) {
	key, err := frontend.ParseRatingKey(chi.URLParam(r, "key"))
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("width"))
	height, _ := strconv.Atoi(r.URL.Query().Get("height"))
	s.servePersonImageSized(w, r, key, chi.URLParam(r, "kind"), chi.URLParam(r, "identity"), width, height)
}

var personImageRe = regexp.MustCompile(`^/library/metadata/(\d+)/person/(actor|director|writer|producer)/([A-Za-z0-9_-]+)$`)

var metadataImageRe = regexp.MustCompile(`^/library/(?:metadata|collections)/(\d+)/(thumb|art|poster|banner|clearLogo|logo|background|composite|squareArt)(?:/([A-Za-z0-9_-]+))?$`)
var playlistImageRe = regexp.MustCompile(`^/playlists/(\d+)/(composite|thumb|art)(?:/\d+)?$`)
var discoveryImageRe = regexp.MustCompile(`^/library/metadata/(\d+)/discoveryPoster/[^/]+$`)
var partIndexFrameRe = regexp.MustCompile(`^/library/parts/(\d+)/indexes/([^/]+)/(\d+)$`)

// handlePhotoTranscode serves /photo/:/transcode. The url parameter may be
// one of our own artwork paths (relative, or absolute on this server) or an
// absolute TMDB image URL. Ether does no pixel work: it serves the TMDB
// variant closest to the requested width.
func (s *Server) handlePhotoTranscode(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// PMS validates dimensions before resolving the image.
	for _, name := range []string{"width", "height"} {
		if q.Has(name) {
			if _, err := strconv.Atoi(photoQueryValue(q, name)); err != nil {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
		}
	}
	// Validate even options handled by the upstream image service.
	for _, name := range []string{"minSize", "upscale", "opacity", "quality", "blur"} {
		if raw, present := firstQuery(q, name); present {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || ((name == "minSize" || name == "upscale") && n != 0 && n != 1) {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
			if name == "blur" && n < 0 {
				writeHTTPError(w, http.StatusInternalServerError)
				return
			}
		}
	}
	raw := photoQueryValue(q, "url")
	if !q.Has("url") {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if raw == "" {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	rawWidth, _ := strconv.Atoi(photoQueryValue(q, "width"))
	height, _ := strconv.Atoi(photoQueryValue(q, "height"))
	width := rawWidth
	if width <= 0 && height > 0 {
		width = height * 2 / 3 // poster aspect
	} else if strings.HasSuffix(photoQueryValue(q, "minSize"), "1") && height > 0 && height*2/3 > width {
		width = height * 2 / 3
	}
	r = r.WithContext(withPhotoFitWidth(r.Context(), width))
	u, err := url.Parse(raw)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	// Android can reuse a nested URL from a different server address.
	// Resolve the route under the outer viewer without dialing that address.
	httpURL := u.Scheme == "http" || u.Scheme == "https"
	if m := catchUpArtRe.FindStringSubmatch(u.Path); m != nil &&
		(!u.IsAbs() && u.Host == "" || httpURL && (s.isSelfHost(r, u.Scheme, u.Host) || plexAndroidClient(FromContext(r.Context())))) {
		s.serveCatchUpArt(w, r, m[1], m[2])
		return
	}
	if u.IsAbs() && s.allowedImageHost(u.Host) {
		if !s.remoteArtworkAllowed(r, u) {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		s.serveRemoteImage(w, r.WithContext(context.WithValue(r.Context(), explicitPhotoURLKey{}, true)), resizeTMDBURL(u.String(), width))
		return
	}
	// Android may wrap an artwork URL from a previous local/remote/relay
	// connection. Resolve only known server artwork routes under the OUTER
	// viewer; never dial that hostname or use credentials from the nested URL.
	androidLocal := plexAndroidClient(FromContext(r.Context())) && (u.Scheme == "http" || u.Scheme == "https") &&
		(metadataImageRe.MatchString(u.Path) || discoveryImageRe.MatchString(u.Path) || personImageRe.MatchString(u.Path) || playlistImageRe.MatchString(u.Path) || liveChannelLogoRe.MatchString(u.Path))
	// A signed guide logo needs no host check: the signature names the
	// channel and nothing is dialled. A client may wrap the public address
	// Plex's cloud was given.
	if m := signedLogoRe.FindStringSubmatch(u.Path); m != nil && (u.Scheme == "" || u.Scheme == "http" || u.Scheme == "https") {
		s.serveSignedLiveLogo(w, r, m[1], m[2])
		return
	}
	if !u.IsAbs() || s.isSelfHost(r, u.Scheme, u.Host) || androidLocal {
		path := u.Path
		if m := liveChannelLogoRe.FindStringSubmatch(path); m != nil {
			s.serveLiveLogo(w, r, m[1], true)
			return
		}
		if m := discoveryImageRe.FindStringSubmatch(path); m != nil {
			key, err := frontend.ParseRatingKey(m[1])
			if err != nil {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
			params := u.Query()
			s.serveTranscodedDiscoveryPoster(w, r, key, params.Get("kind"), params.Get("lang"), width)
			return
		}
		if m := personImageRe.FindStringSubmatch(path); m != nil {
			key, err := frontend.ParseRatingKey(m[1])
			if err != nil {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
			s.servePersonImageSized(w, r, key, m[2], m[3], rawWidth, height)
			return
		}
		if m := metadataImageRe.FindStringSubmatch(path); m != nil {
			key, err := frontend.ParseRatingKey(m[1])
			if err != nil {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
			inner := u.Query()
			if strings.HasPrefix(path, "/library/metadata/") && !inner.Has("ranked") {
				if _, kind, lang, ok := decodeDiscoveryArtworkToken(m[3]); ok && discoveryElementMatchesKind(m[2], kind) {
					s.serveTranscodedDiscoveryPoster(w, r, key, kind, lang, width)
					return
				}
			}
			// The inner URL carries the variant (collection scope, Top 10 rank
			// and shape). Clients such as Plex for Apple TV fetch every poster
			// through this transcoder, so the variant must survive the hop.
			if inner.Has("collection") || inner.Has("ranked") || inner.Has("collectionCard") {
				r = r.Clone(r.Context())
				q := r.URL.Query()
				for _, name := range []string{"collection", "ranked", "rankshape", "rv", "collectionCard"} {
					if v := inner.Get(name); v != "" {
						q.Set(name, v)
					}
				}
				r.URL.RawQuery = q.Encode()
			}
			s.serveItemImage(w, r, key, m[2], width)
			return
		}
		if m := playlistImageRe.FindStringSubmatch(path); m != nil {
			key, err := frontend.ParseRatingKey(m[1])
			if err != nil {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
			s.serveItemImage(w, r, key, m[2], width)
			return
		}
		if m := partIndexFrameRe.FindStringSubmatch(path); m != nil {
			partID, err1 := strconv.ParseInt(m[1], 10, 64)
			offsetMS, err2 := strconv.ParseInt(m[3], 10, 64)
			if err1 != nil || err2 != nil {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
			s.serveIndexFrame(w, r, partID, m[2], offsetMS)
			return
		}
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if !s.allowedImageHost(u.Host) {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	s.serveRemoteImage(w, r.WithContext(context.WithValue(r.Context(), explicitPhotoURLKey{}, true)), resizeTMDBURL(u.String(), width))
}

func (s *Server) serveTranscodedDiscoveryPoster(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, kind, lang string, width int) {
	r = r.Clone(r.Context())
	effective := r.URL.Query()
	effective.Set("width", strconv.Itoa(width))
	r.URL.RawQuery = effective.Encode()
	s.serveDiscoveryPoster(w, r, key, kind, lang)
}

var liveChannelLogoRe = regexp.MustCompile(`^/livetv/channels/([^/]+)/logo\.(?:svg|png)$`)

var signedLogoRe = regexp.MustCompile(`^/livetv/logos/([^/]+)/([A-Za-z0-9_-]+)\.png$`)

// isSelfHost reports whether host refers to this server.
func (s *Server) isSelfHost(r *http.Request, scheme, host string) bool {
	host = withoutDefaultPort(scheme, host)
	if origin := s.getHost().PublicOrigin; origin != nil {
		if u, err := url.Parse(origin()); err == nil && u.Host != "" && strings.EqualFold(withoutDefaultPort(u.Scheme, u.Host), host) {
			return true
		}
	}
	h := strings.ToLower(host)
	if h == "" || strings.EqualFold(h, withoutDefaultPort(scheme, r.Host)) {
		return true
	}
	name := h
	if i := strings.LastIndexByte(h, ':'); i > 0 && !strings.HasSuffix(h, "]") {
		name = h[:i]
	}
	name = strings.Trim(name, "[]")
	return name == "localhost" || name == "127.0.0.1" || name == "::1" || strings.HasSuffix(name, ".plex.direct")
}

// withoutDefaultPort drops the scheme's default port, so a nested URL that
// names "https://host:443" matches the server reached as "host" (#801).
func withoutDefaultPort(scheme, host string) string {
	switch {
	case strings.EqualFold(scheme, "https") && strings.HasSuffix(host, ":443"):
		return strings.TrimSuffix(host, ":443")
	case strings.EqualFold(scheme, "http") && strings.HasSuffix(host, ":80"):
		return strings.TrimSuffix(host, ":80")
	}
	return host
}

// allowedImageHost limits the proxy to the artwork CDN, so it is not an
// open proxy.
func (s *Server) allowedImageHost(host string) bool {
	if frontend.MetadataImageHost(host) {
		return true
	}
	ref, err := url.Parse(s.svc().imageURL("/x.jpg", "original"))
	if err != nil {
		return false
	}
	return strings.EqualFold(ref.Host, host)
}

// photoQueryValue follows PMS duplicate-parameter semantics, including an empty
// final value.
func photoQueryValue(q url.Values, name string) string {
	vs := q[name]
	if len(vs) == 0 {
		return ""
	}
	return vs[len(vs)-1]
}

// Honor explicit Android formats through the shared, byte-bounded variant
// cache. Authorization remains in the source handler, before cache lookup.
func (s *Server) androidPhotoFormat(r *http.Request, data []byte, ct string) ([]byte, string, error) {
	return s.svc().images.RasterFormat(r.Context(), data, ct, androidPhotoOutputFormat(r), s.now())
}

func androidPhotoOutputFormat(r *http.Request) string {
	if !plexAndroidClient(FromContext(r.Context())) || r.URL.Path != "/photo/:/transcode" {
		return ""
	}
	format := strings.ToLower(photoQueryValue(r.URL.Query(), "format"))
	if format == "jpg" {
		format = "jpeg"
	}
	if format == "jpeg" || format == "png" {
		return format
	}
	return ""
}
