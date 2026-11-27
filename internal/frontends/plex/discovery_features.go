package plex

import (
	"context"
	"encoding/base64"
	"math"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	kitimages "github.com/braedonsaunders/ether/internal/frontend/kit/images"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"github.com/go-chi/chi/v5"
)

const discoveryArtworkTokenPrefix = "dp-"

func discoveryArtworkElement(kind string) (element, code string) {
	switch kind {
	case "backdrop":
		return "art", "b"
	case "logo":
		return "clearLogo", "l"
	default:
		return "thumb", "p"
	}
}

func discoveryElementMatchesKind(element, kind string) bool {
	switch kind {
	case "backdrop":
		return element == "art" || element == "background"
	case "logo":
		return element == "clearLogo" || element == "logo"
	case "", "poster":
		return element == "thumb" || element == "poster"
	default:
		return false
	}
}

// encodeDiscoveryArtworkToken packs version, kind, and language into an opaque
// URL-safe token so Plex artwork URLs keep the PMS /library/metadata/{id}/{element}/{token}
// shape with no query string.
func encodeDiscoveryArtworkToken(version, kind, lang string) string {
	_, code := discoveryArtworkElement(kind)
	version = strings.ReplaceAll(version, "\x00", "")
	lang = strings.ReplaceAll(lang, "\x00", "")
	payload := "v1\x00" + code + "\x00" + version + "\x00" + lang
	return discoveryArtworkTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(payload))
}

func decodeDiscoveryArtworkToken(token string) (version, kind, lang string, ok bool) {
	encoded, hasPrefix := strings.CutPrefix(token, discoveryArtworkTokenPrefix)
	if !hasPrefix || encoded == "" {
		return "", "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return "", "", "", false
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 4 || parts[0] != "v1" || parts[2] == "" {
		return "", "", "", false
	}
	switch parts[1] {
	case "p":
		kind = "poster"
	case "b":
		kind = "backdrop"
	case "l":
		kind = "logo"
	default:
		return "", "", "", false
	}
	return parts[2], kind, parts[3], true
}

func discoveryArtworkURL(key frontend.RatingKey, kind, version, lang string) string {
	element, _ := discoveryArtworkElement(kind)
	return "/library/metadata/" + key.String() + "/" + element + "/" + encodeDiscoveryArtworkToken(version, kind, lang)
}

// DiscoveryFeatures is the shared discovery contract adapted by Plex.
type DiscoveryFeatures = frontend.DiscoveryFeatures

func (s *Server) discoveryHubs(ctx context.Context, pr *Request, limit int) []hubSpec {
	return s.discoveryHubsFor(ctx, pr, limit, false)
}

func (s *Server) discoveryHubsFor(ctx context.Context, pr *Request, limit int, section bool) []hubSpec {
	d := s.discovery()
	if d == nil || pr.User == nil {
		return nil
	}
	var out []hubSpec
	add := func(purpose frontend.RowPurpose, load func(context.Context, *frontend.User, int) ([]frontend.Hub, error)) {
		var hubs []frontend.Hub
		selector := s.exp().NewDiscoverySelection(frontend.DiscoverySelectionOptions{Visibility: s.visibility(), Unrestricted: func(u *frontend.User) bool { return !s.viewerRestricted(u) }})
		rows := selector.Load(ctx, pr.User, limit, func(ctx context.Context, fetch int) []frontend.DiscoveryCandidate {
			hubs, _ = load(ctx, pr.User, fetch)
			candidates := make([]frontend.DiscoveryCandidate, len(hubs))
			for i, hub := range hubs {
				candidates[i] = frontend.DiscoveryCandidate{Index: i, Purpose: purpose, Items: hub.Items}
			}
			return candidates
		})
		for _, row := range rows {
			h := hubs[row.Index]
			h.Items = row.Items
			out = append(out, hubSpec{Hub: h})
		}
	}
	if d.Watchlist != nil {
		add(frontend.RowWatchlist, func(ctx context.Context, viewer *frontend.User, limit int) ([]frontend.Hub, error) {
			return d.Watchlist.Hubs(ctx, viewer, limit, pr.Language)
		})
	}
	if d.Personal != nil {
		load := d.Personal.Hubs
		if section {
			// Year in Review is a Home-only row. The section never renders it.
			load = func(ctx context.Context, viewer *frontend.User, limit int) ([]frontend.Hub, error) {
				hub, err := d.Personal.Rediscover(ctx, viewer, limit)
				if err != nil || len(hub.Items) == 0 {
					return nil, err
				}
				return []frontend.Hub{hub}, nil
			}
		}
		add(frontend.RowPurpose("recommendation"), load)
	}
	return out
}
func (s *Server) discoveryRemoveContinue(ctx context.Context, pr *Request, key frontend.RatingKey) error {
	if s.discovery() == nil || s.discovery().Personal == nil || pr.User == nil {
		return nil
	}
	t, e := s.catalog().Get(ctx, key)
	if e != nil {
		return e
	}
	if !s.visibleTitle(ctx, pr.User, t) {
		return frontend.ErrNotFound
	}
	return s.discovery().Personal.RemoveContinue(ctx, pr.User.ID, key)
}

func (s *Server) decorateDiscoveryMetadata(ctx context.Context, pr *Request, t, show *frontend.Title, m *model.Metadata, detail bool) {
	d := s.discovery()
	if d == nil || t == nil || pr.User == nil || !s.titleAllowed(pr.User, t) {
		return
	}
	selected := d.SelectArtwork(ctx, pr.User, t, pr.Language)
	localized := selected.Title
	if localized != nil && localized != t {
		m.Title = localized.Title
		m.Summary = localized.Summary
		m.Tagline = localized.Tagline
	}
	if d.OverviewTranslate != nil && pr.Language != "" {
		if summary, tagline, ok := d.OverviewTranslate(ctx, t, pr.Language, detail); ok {
			if summary != "" {
				m.Summary = summary
			}
			if tagline != "" {
				m.Tagline = tagline
			}
		}
	}
	if selected.PosterVersion != "" {
		m.Thumb = discoveryArtworkURL(t.Key, "poster", selected.PosterVersion, pr.Language)
		if t.Type != frontend.TypeEpisode {
			hasPoster := false
			for i := range m.Image {
				if m.Image[i].Type == "coverPoster" {
					m.Image[i].URL = m.Thumb
					hasPoster = true
				}
			}
			if !hasPoster {
				m.Image = append(m.Image, model.Image{Alt: m.Title, Type: "coverPoster", URL: m.Thumb})
			}
		}
	}
	if selected.BackdropVersion != "" {
		m.Art = discoveryArtworkURL(t.Key, "backdrop", selected.BackdropVersion, pr.Language)
		for i := range m.Image {
			if m.Image[i].Type == "background" {
				m.Image[i].URL = m.Art
			}
		}
	}
	if (t.Type == frontend.TypeEpisode || t.Type == frontend.TypeSeason) && !selected.Enabled && d.Overlays != nil && d.Overlays.CustomPostersEnabled() {
		showKey := t.ShowKey
		if showKey == 0 {
			showKey = t.Key.Parent()
			if t.Type == frontend.TypeEpisode {
				showKey = showKey.Parent()
			}
		}
		if showKey.Kind() == frontend.KindShow && (show == nil || s.titleAllowed(pr.User, show)) {
			target := show
			if target == nil {
				target = &frontend.Title{Key: showKey, Type: frontend.TypeShow}
			}
			showSelected := d.SelectArtwork(ctx, pr.User, target, pr.Language)
			if showSelected.PosterVersion != "" {
				showThumb := discoveryArtworkURL(showKey, "poster", showSelected.PosterVersion, pr.Language)
				if t.Type == frontend.TypeEpisode {
					m.GrandparentThumb = showThumb
					for i := range m.Image {
						if m.Image[i].Type == "coverPoster" {
							m.Image[i].URL = showThumb
						}
					}
				} else {
					oldParent := m.ParentThumb
					m.ParentThumb = showThumb
					if m.Thumb == oldParent {
						m.Thumb = showThumb
					}
					for i := range m.Image {
						if m.Image[i].Type == "coverPoster" && m.Image[i].URL == oldParent {
							m.Image[i].URL = showThumb
						}
					}
				}
			}
		}
	}
	if selected.Enabled {
		hasLogo := false
		for i := range m.Image {
			m.Image[i].Alt = m.Title
			switch m.Image[i].Type {
			case "coverPoster":
				if t.Type != frontend.TypeEpisode && m.Thumb != "" {
					m.Image[i].URL = m.Thumb
				}
			case "background":
				if m.Art != "" {
					m.Image[i].URL = m.Art
				}
			case "clearLogo":
				hasLogo = true
				if selected.LogoVersion != "" {
					m.Image[i].URL = discoveryArtworkURL(t.Key, "logo", selected.LogoVersion, pr.Language)
				}
			}
		}
		if !hasLogo && selected.LogoVersion != "" {
			m.Image = append(m.Image, model.Image{Alt: m.Title, Type: "clearLogo", URL: discoveryArtworkURL(t.Key, "logo", selected.LogoVersion, pr.Language)})
		}
	}
	if d.Ratings != nil && (t.Type == frontend.TypeMovie || t.Type == frontend.TypeShow) {
		bs, e := d.Ratings.Cached(ctx, t)
		cfg, ce := d.Ratings.Settings(ctx)
		if e == nil && ce == nil && cfg.Enabled {
			d.Ratings.Queue(*t)
			if cfg.Audience != "" && cfg.Audience != "none" && cfg.Missing == "tmdb" {
				m.AudienceRating, m.AudienceRatingImage = 0, ""
				if t.VoteCount > 0 && t.Rating > 0 && t.Rating <= 10 {
					m.AudienceRating, m.AudienceRatingImage = math.Round(t.Rating*10)/10, "themoviedb://image.rating"
				}
			}
			if cfg.Audience == "none" || (cfg.Audience != "" && cfg.Missing == "none") {
				m.AudienceRating, m.AudienceRatingImage = 0, ""
			}
			if cfg.Critic != "" && cfg.Critic != "none" && cfg.Missing == "tmdb" {
				m.Rating, m.RatingImage = 0, ""
				if t.VoteCount > 0 && t.Rating > 0 && t.Rating <= 10 {
					m.Rating, m.RatingImage = math.Round(t.Rating*10)/10, "themoviedb://image.rating"
				}
			}
			if cfg.Critic == "none" || (cfg.Critic != "" && cfg.Missing == "none") {
				m.Rating, m.RatingImage = 0, ""
			}
			for _, b := range bs {
				if b.Source == cfg.Critic && b.Image != "" {
					m.Rating = b.Value
					m.RatingImage = b.Image
				}
				if b.Source == cfg.Audience && b.Image != "" {
					m.AudienceRating = b.Value
					m.AudienceRatingImage = b.Image
				}
			}
		}
		if e == nil && ce == nil && cfg.Enabled && (len(bs) > 0 || cfg.Audience == "none" || cfg.Missing != "") && len(m.Rating_) > 0 {
			m.Rating_ = nil
			if m.RatingImage != "" {
				m.Rating_ = append(m.Rating_, model.RatingTag{Type: "critic", Image: m.RatingImage, Value: m.Rating})
			}
			if m.AudienceRatingImage != "" {
				m.Rating_ = append(m.Rating_, model.RatingTag{Type: "audience", Image: m.AudienceRatingImage, Value: m.AudienceRating})
			}
		}

	}
	if d.LLM != nil && d.Watchlist != nil && (t.Type == frontend.TypeSeason || t.Type == frontend.TypeEpisode) {
		if d.Watchlist.RecapsEnabled(ctx) {
			entry := d.CachedRecap(pr.User, t, s.state(), s.now())
			if entry != "" {
				m.Summary = "Previously watched: " + entry + "\n\n" + m.Summary
			}

		}
	}
}
func (s *Server) handleDiscoveryPoster(w http.ResponseWriter, r *http.Request) {
	k, e := frontend.ParseRatingKey(chi.URLParam(r, "id"))
	if e != nil {
		writeHTTPError(w, 400)
		return
	}
	q := r.URL.Query()
	s.serveDiscoveryPoster(w, r, k, q.Get("kind"), q.Get("lang"))
}

// serveDiscoveryPoster also serves Plex's photo transcode wrapper. Keep the
// authenticated outer request: credentials inside an artwork URL grant nothing.
func (s *Server) serveDiscoveryPoster(w http.ResponseWriter, r *http.Request, k frontend.RatingKey, kind, lang string) {
	d := s.discovery()
	if d == nil || d.Overlays == nil {
		writeHTTPError(w, 404)
		return
	}
	ctx := kitimages.TrackCachePolicy(r.Context())
	r = r.WithContext(ctx)
	pr := FromContext(ctx)
	t, e := s.artworkTitle(ctx, k)
	if e != nil || pr.User == nil || !s.visibleTitle(ctx, pr.User, t) {
		writeHTTPError(w, 404)
		return
	}
	if lang == "" {
		lang = pr.Language
	}
	t, _, e = d.PrepareArtwork(ctx, pr.User, t, lang)
	if e != nil {
		writeHTTPError(w, 404)
		return
	}
	width, height := photoDimensions(r)
	var data []byte
	var ct string
	if kind == "backdrop" && (width > 1280 || width == 0 && height > 0) {
		_, path := d.Overlays.Paths(ctx, pr.User, t, lang)
		if path == "" {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") {
			data, ct, e = d.RenderArtwork(ctx, pr.User, t, kind, lang)
		} else {
			svc := s.svc()
			img, err := svc.images.Get(ctx, svc.imageURL(path, tmdbSize(width)), s.now())
			e = err
			if err == nil {
				data, ct = img.Data, img.ContentType
			}
		}
	} else {
		data, ct, e = d.RenderArtwork(ctx, pr.User, t, kind, lang)
	}
	if e != nil {
		writeHTTPError(w, 404)
		return
	}
	data, ct, e = s.transformPhoto(r, data, ct)
	if e != nil {
		writeHTTPError(w, http.StatusBadGateway)
		return
	}
	w.Header().Set("Vary", "X-Plex-Token, X-Plex-Language, X-Plex-Product")
	writeArtworkBytes(w, r, data, ct, kitimages.Revalidate)
}
