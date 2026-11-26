package plex

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"strconv"

	"golang.org/x/image/draw"
)

// PMS's photo transcoder returns the size a client asks for. Ether serves
// TMDB's fixed widths, rounded up: Plezy in TV mode asks for ~520-640 px
// posters and received w780 originals of 135-450 KB, so a 6-at-a-time grid
// on a tablet's Wi-Fi filled at about one poster per second. A source more
// than 15% wider than the request is scaled to the requested width and
// re-encoded once, then served from a bounded cache.

type photoFitWidthKey struct{}

func withPhotoFitWidth(ctx context.Context, width int) context.Context {
	if width <= 0 {
		return ctx
	}
	return context.WithValue(ctx, photoFitWidthKey{}, width)
}

// fitPhoto returns data scaled to the request's photo-transcode width, or
// data unchanged when no width was asked, the source is not a JPEG, or it is
// already within 15% of the request.
func (s *Server) fitPhoto(ctx context.Context, data []byte, contentType string) []byte {
	width, _ := ctx.Value(photoFitWidthKey{}).(int)
	if width <= 0 || width > 4096 || contentType != "image/jpeg" {
		return data
	}
	fitted, _, err := s.cachePhotoVariant(ctx, data, contentType, "plex-fit-v1/"+strconv.Itoa(width), func(context.Context) ([]byte, string, error) {
		return fitJPEG(data, width), contentType, nil
	})
	if err != nil {
		return data
	}
	return fitted
}

func fitJPEG(data []byte, width int) []byte {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 40_000_000 || cfg.Width*100 <= width*115 {
		return data
	}
	src, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return data
	}
	height := max(1, cfg.Height*width/cfg.Width)
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	var out bytes.Buffer
	if jpeg.Encode(&out, dst, &jpeg.Options{Quality: 85}) != nil || out.Len() >= len(data) {
		return data
	}
	return out.Bytes()
}
