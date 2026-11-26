package service

import (
	"bytes"
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	kitimages "github.com/braedonsaunders/ether/internal/frontend/kit/images"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strconv"
)

// Palette holds neutral image corner colours, without protocol serialization flags.
type Palette = frontend.ArtworkPalette

// renderUltraBlur paints the same four colours Plex provides to its clients.
// Palette selection itself stays in the shared UltraBlur cache and pipeline.
func RenderUltraBlur(colors Palette, transform frontend.ArtworkTransform) ([]byte, string, error) {
	width, height := transform.Width, transform.Height
	if width == 0 && height == 0 {
		width, height = 640, 360
	}
	if width == 0 {
		width = max(1, height*16/9)
	}
	if height == 0 {
		height = max(1, width*9/16)
	}
	if width > 4000 || height > 4000 {
		return nil, "", core.ErrNotAvailable
	}
	values := [4]string{colors.TopLeft, colors.TopRight, colors.BottomLeft, colors.BottomRight}
	var rgb [4][3]uint8
	for i, value := range values {
		n, err := strconv.ParseUint(value, 16, 24)
		if err != nil || len(value) != 6 {
			return nil, "", core.ErrNotAvailable
		}
		rgb[i] = [3]uint8{uint8(n >> 16), uint8(n >> 8), uint8(n)}
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		fy := float64(y) / float64(max(1, height-1))
		for x := 0; x < width; x++ {
			fx := float64(x) / float64(max(1, width-1))
			var c [3]uint8
			for i := range c {
				top := float64(rgb[0][i])*(1-fx) + float64(rgb[1][i])*fx
				bottom := float64(rgb[2][i])*(1-fx) + float64(rgb[3][i])*fx
				c[i] = uint8(top*(1-fy) + bottom*fy + 0.5)
			}
			img.SetNRGBA(x, y, color.NRGBA{R: c[0], G: c[1], B: c[2], A: 255})
		}
	}
	var out bytes.Buffer
	if transform.Format == "png" {
		if err := png.Encode(&out, img); err != nil {
			return nil, "", err
		}
		return out.Bytes(), "image/png", nil
	}
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 90}); err != nil {
		return nil, "", err
	}
	return out.Bytes(), "image/jpeg", nil
}

func Transform(data []byte, contentType string, transform frontend.ArtworkTransform) ([]byte, string, error) {
	return kitimages.RasterVariant(context.Background(), nil, data, contentType, transform)
}
