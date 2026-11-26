package images

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"testing"
)

func TestRasterFitStaysWithinLimitAndFailsSoft(t *testing.T) {
	var src bytes.Buffer
	if err := png.Encode(&src, image.NewNRGBA(image.Rect(0, 0, 4500, 20))); err != nil {
		t.Fatal(err)
	}
	out, _, err := RasterVariant(context.Background(), nil, src.Bytes(), "image/png", RasterTransform{Width: 4400, Fit: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(out))
	if err != nil || cfg.Width != 4000 {
		t.Fatalf("fitted width %d, %v; want 4000", cfg.Width, err)
	}

	junk := []byte("\x89PNG\r\n\x1a\nnot really")
	out, ct, err := RasterVariant(context.Background(), nil, junk, "image/png", RasterTransform{Width: 300, Fit: true})
	if err != nil || !bytes.Equal(out, junk) || ct != "image/png" {
		t.Fatalf("undecodable fit: %v %q", err, ct)
	}
	if _, _, err := RasterVariant(context.Background(), nil, junk, "image/png", RasterTransform{Width: 300, Fit: true, Format: "webp"}); err == nil {
		t.Fatal("a requested format must still fail on undecodable bytes")
	}
}
