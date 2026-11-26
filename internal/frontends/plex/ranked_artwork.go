package plex

import (
	"bytes"
	"context"
	"image"
	"net/http"
	"net/url"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
	kitimages "github.com/braedonsaunders/ether/internal/frontend/kit/images"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

func rankedMetadata(items []model.Metadata) { rankedMetadataOffset(items, 0) }
func rankedMetadataOffset(items []model.Metadata, offset int) {
	for n := range items {
		m := &items[n]
		if m.Type != "movie" && m.Type != "show" {
			continue
		}
		q := url.Values{"ranked": {strconv.Itoa(offset + n + 1)}, "rv": {m.Thumb}}
		portrait := "/library/metadata/" + m.RatingKey + "/thumb?" + q.Encode()
		q.Set("rankshape", "wide")
		wide := "/library/metadata/" + m.RatingKey + "/thumb?" + q.Encode()
		m.Thumb = portrait
		images := make([]model.Image, 0, len(m.Image)+2)
		for _, img := range m.Image {
			if img.Type != "coverPoster" && img.Type != "coverArt" {
				images = append(images, img)
			}
		}
		images = append(images, model.Image{Type: "coverPoster", URL: portrait}, model.Image{Type: "coverArt", URL: wide})
		m.Image = images
	}
}

func (s *Server) serveRankedArtwork(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, width int) {
	if width < 0 || width > 4000 {
		writeHTTPError(w, 404)
		return
	}
	rank, err := strconv.Atoi(r.URL.Query().Get("ranked"))
	shape := r.URL.Query().Get("rankshape")
	if err != nil || rank < 1 || rank > 10 || (shape != "" && shape != "wide") || (key.Kind() != frontend.KindMovie && key.Kind() != frontend.KindShow) {
		writeHTTPError(w, 404)
		return
	}
	art := s.getHost().Artwork
	if art == nil {
		writeHTTPError(w, 404)
		return
	}
	r = r.WithContext(kitimages.TrackCachePolicy(r.Context()))
	pr := FromContext(r.Context())
	req := frontend.ArtworkRequest{Viewer: pr.User, Item: key, Device: frontend.Device{Language: pr.Language, Product: pr.Product, Platform: pr.Platform}}
	images, complete, err := art.List(r.Context(), req)
	if err != nil || !complete {
		// Upcoming titles use collection authorization instead of the
		// released-title artwork list, including canonical Home image URLs.
		if s.serveRankedPlaceholder(w, r, key, rank, shape == "wide", width) {
			return
		}
		writeHTTPError(w, 404)
		return
	}
	// Wide cards use the authorized backdrop, never a stretched poster. A
	// missing or unavailable backdrop falls back to the portrait rank variant.
	if shape == "wide" {
		if discovery := s.discovery(); discovery != nil && discovery.Overlays != nil {
			for _, img := range images {
				if img.Kind != frontend.ImageBackdrop {
					continue
				}
				data, _, err := art.Read(r.Context(), req, img, frontend.ArtworkTransform{Width: 960})
				if err != nil || !rankedBackdrop(data) {
					continue
				}
				data, ct, err := discovery.Overlays.Ranked(r.Context(), data, rank, true)
				if err == nil && s.writeRankedImage(w, r, data, ct, width) {
					return
				}
			}
		}
	}
	for _, img := range images {
		if img.Kind != frontend.ImagePoster {
			continue
		}
		data, ct, err := art.Read(r.Context(), req, frontend.RankedArtwork(img, rank, false), frontend.ArtworkTransform{Width: width})
		if err == nil && len(data) > 0 && s.writeRankedImage(w, r, data, ct, 0) {
			return
		}
	}

	writeHTTPError(w, 404)
}

const rankedNoCoverArtDisplayImageQuirk = "plex-no-coverart-display-image"

// rankedDisplayImageMeta returns Meta.DisplayImage requesting wide "coverArt"
// cards for clients that read server-supplied Image[type="coverArt"] entries
// (such as Plex for iOS and Apple TV). Plex Web and desktop/HTML clients built
// on Plex's web-client bundle overwrite server Image arrays during metadata
// normalization and leave only coverPoster, so requesting coverArt causes them
// to draw blank 16:9 cards; omitting Meta.DisplayImage lets them draw portrait
// cards using thumb (the ranked portrait composite).
func rankedDisplayImageMeta(pr *Request, mediaType string) *model.Meta {
	if pr != nil && frontend.HasQuirk(pr.Product, pr.Platform, pr.Device, pr.Model, pr.Version, rankedNoCoverArtDisplayImageQuirk) {
		return nil
	}
	return &model.Meta{DisplayImage: []model.DisplayImage{{Type: mediaType, ImageType: "coverArt"}}}
}

func paintRankedCollection(pr *Request, mc *model.MediaContainer, offset int) {
	if offset >= 10 {
		mc.Metadata = nil
	} else {
		mc.Metadata = mc.Metadata[:min(len(mc.Metadata), 10-offset)]
	}
	rankedMetadataOffset(mc.Metadata, offset)
	total := 10
	if mc.TotalSize != nil {
		total = min(total, *mc.TotalSize)
	}
	mc.Size = len(mc.Metadata)
	mc.SetPage(min(offset, total), total)
	typ := "movie"
	if len(mc.Metadata) > 0 {
		typ = mc.Metadata[0].Type
	}
	mc.Meta = rankedDisplayImageMeta(pr, typ)
}

// serveRankedPlaceholder preserves the existing upcoming-collection artwork
// flow. rv contributes only an optional collection proof, never a fetch URL.
// Home uses canonical URLs: resolve those through the same bounded, viewer-
// authorized membership lookup as ordinary artwork.
func (s *Server) serveRankedPlaceholder(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, rank int, wide bool, width int) bool {
	original, err := url.Parse(r.URL.Query().Get("rv"))
	if err != nil {
		return false
	}
	proof := original.Query().Get("collection")
	title, _, err := s.lookupPlaceholder(r.Context(), FromContext(r.Context()), key, proof)
	if err != nil || title == nil {
		return false
	}
	discovery := s.discovery()
	if discovery == nil || discovery.Overlays == nil {
		return false
	}
	if wide && title.BackdropPath != "" && s.svc().images != nil {
		img, err := s.svc().images.Get(r.Context(), s.svc().imageURL(title.BackdropPath, "w1280"), s.now())
		if err == nil && rankedBackdrop(img.Data) {
			data, ct, err := discovery.Overlays.Ranked(r.Context(), img.Data, rank, true)
			if err == nil && s.writeRankedImage(w, r, data, ct, width) {
				return true
			}
		}
	}
	var data []byte
	if title.PosterPath != "" && s.svc().images != nil {
		image, err := s.svc().images.Get(r.Context(), s.svc().imageURL(title.PosterPath, "w500"), s.now())
		if err != nil {
			return false
		}
		data = image.Data
	} else {
		generated, _, ok, err := frontend.RenderTitlePoster(r.Context(), s.svc().generatedPosters, title, 500)
		if err != nil || !ok {
			return false
		}
		data = generated
	}
	data, ct, err := discovery.Overlays.Ranked(r.Context(), data, rank, false)
	if err != nil {
		return false
	}
	return s.writeRankedImage(w, r, data, ct, width)
}

func rankedBackdrop(data []byte) bool {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	return err == nil && cfg.Width > cfg.Height && cfg.Height > 0
}

func (s *Server) writeRankedImage(w http.ResponseWriter, r *http.Request, data []byte, ct string, width int) bool {
	var err error
	// The standard transform respects this request's output bound.
	if width < 0 || width > 4000 {
		return false
	}
	if width > 0 {
		data, ct, err = s.cachePhotoVariant(r.Context(), data, ct, "poster-width-v1/"+strconv.Itoa(width), func(context.Context) ([]byte, string, error) {
			out, err := frontend.ResizePoster(data, width)
			return out, "image/jpeg", err
		})
		if err != nil {
			return false
		}
	}
	w.Header().Add("Vary", "X-Plex-Token")
	writeArtworkBytes(w, r, data, ct, kitimages.Revalidate)
	return true
}
