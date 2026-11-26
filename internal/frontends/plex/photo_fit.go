package plex

import (
	"errors"
	"image"
	"net/http"
	"strconv"

	kitimages "github.com/braedonsaunders/ether/internal/frontend/kit/images"
)

func photoDimensions(r *http.Request) (int, int) {
	q := r.URL.Query()
	width, _ := strconv.Atoi(photoQueryValue(q, "width"))
	height, _ := strconv.Atoi(photoQueryValue(q, "height"))
	return max(0, width), max(0, height)
}

func (s *Server) transformPhoto(r *http.Request, data []byte, contentType string) ([]byte, string, error) {
	width, height := photoDimensions(r)
	format := androidPhotoOutputFormat(r)
	cache, _ := s.svc().images.(kitimages.VariantCache)
	out, ct, err := kitimages.RasterVariant(r.Context(), cache, data, contentType, kitimages.RasterTransform{
		Width: width, Height: height, Format: format, Fit: true,
	})
	// Opaque provider responses can be proxied as-is when no conversion was requested.
	if format == "" && errors.Is(err, image.ErrFormat) {
		return data, contentType, nil
	}
	return out, ct, err
}
