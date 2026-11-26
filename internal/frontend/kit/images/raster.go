package images

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"

	"github.com/HugoSmits86/nativewebp"
	_ "golang.org/x/image/webp"
)

// RasterTransform describes the requested output. Zero dimensions leave an
// axis unconstrained when fitting, and Quality zero uses the encoder default.
type RasterTransform struct {
	Width, Height int
	Quality       int
	Fit           bool
	Format        string
	Blur          bool
}

func (t RasterTransform) changesRaster(contentType string) bool {
	switch contentType {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return t.Format != "" || t.Blur || t.Height > 0 || (t.Fit && t.Width > 0) || (t.Quality > 0 && contentType == "image/jpeg")
	default:
		return t.Format != "" || t.Blur
	}
}

// RasterVariant transforms authorized bytes using the shared variant cache.
func RasterVariant(ctx context.Context, cache VariantCache, data []byte, contentType string, transform RasterTransform) ([]byte, string, error) {
	if (!transform.changesRaster(contentType)) ||
		(transform.Width == 0 && transform.Height == 0 && transform.Quality == 0 && !transform.Blur && contentType == "image/"+transform.Format) {
		return data, contentType, nil
	}
	render := func(context.Context) ([]byte, string, error) {
		return transformRaster(data, contentType, transform)
	}
	if cache == nil {
		return render(ctx)
	}
	key := fmt.Sprintf("raster-transform-v2/%d/%d/%s/%t/%t/%d", transform.Width, transform.Height, transform.Format, transform.Fit, transform.Blur, transform.Quality)
	return cache.Variant(ctx, data, contentType, key, render)
}

func transformRaster(data []byte, contentType string, transform RasterTransform) ([]byte, string, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, "", ErrDimensions
	}
	if transform.Fit && transform.Format != "" && contentType == "image/"+transform.Format {
		transform.Format = ""
	}
	if transform.Format != "jpeg" && contentType != "image/jpeg" {
		transform.Quality = 0
	}
	if transform.Fit {
		maxWidth, maxHeight := cfg.Width, cfg.Height
		if transform.Width > 0 {
			maxWidth = min(maxWidth, transform.Width)
		}
		if transform.Height > 0 {
			maxHeight = min(maxHeight, transform.Height)
		}
		if maxWidth == cfg.Width && maxHeight == cfg.Height && transform.Format == "" && !transform.Blur && transform.Quality == 0 {
			return data, contentType, nil
		}
		transform.Width, transform.Height = maxWidth, maxHeight
		if int64(maxWidth)*int64(cfg.Height) <= int64(maxHeight)*int64(cfg.Width) {
			transform.Height = max(1, int(int64(cfg.Height)*int64(maxWidth)/int64(cfg.Width)))
		} else {
			transform.Width = max(1, int(int64(cfg.Width)*int64(maxHeight)/int64(cfg.Height)))
		}
	}
	if cfg.Width > 40_000_000/cfg.Height {
		return nil, "", ErrDimensions
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	width, height := transform.Width, transform.Height
	if width == 0 && height > 0 {
		width = max(1, cfg.Width*height/cfg.Height)
	}
	if height == 0 && width > 0 {
		height = max(1, cfg.Height*width/cfg.Width)
	}
	if width > 4000 || height > 4000 {
		return nil, "", ErrDimensions
	}
	if width > 0 && height > 0 && (width != cfg.Width || height != cfg.Height) {
		src, err = Resize(src, width, height)
		if err != nil {
			return nil, "", err
		}
	}
	if transform.Blur {
		src, err = Blur(src)
		if err != nil {
			return nil, "", err
		}
	}
	if transform.Fit && transform.Format == "" {
		switch contentType {
		case "image/png":
			transform.Format = "png"
		case "image/webp", "image/gif":
			if opaque, ok := src.(interface{ Opaque() bool }); ok && !opaque.Opaque() {
				transform.Format = "png"
			}
		}
	}
	var out bytes.Buffer
	switch transform.Format {
	case "webp":
		err = nativewebp.Encode(&out, src, &nativewebp.Options{CompressionLevel: nativewebp.BestSpeed})
		contentType = "image/webp"
	case "png":
		err = png.Encode(&out, src)
		contentType = "image/png"
	default:
		quality := transform.Quality
		if quality <= 0 || quality > 100 {
			quality = 90
		}
		err = jpeg.Encode(&out, src, &jpeg.Options{Quality: quality})
		contentType = "image/jpeg"
	}
	if err != nil {
		return nil, "", err
	}
	return out.Bytes(), contentType, nil
}
