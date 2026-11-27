package plugins

import (
	"fmt"
	"image"
	"image/color"
	"testing"
)

func TestFolderPerceptionHashConvention(t *testing.T) {
	var i int
	digest, err := folderPerceptionHash(100, func(at float64) (image.Image, error) {
		if want := 5 + float64(i)*3.6; at != want {
			return nil, fmt.Errorf("frame %d at %v, want %v", i, at, want)
		}
		img := image.NewRGBA(image.Rect(0, 0, 160, 90))
		for y := range 90 {
			for x := range 160 {
				img.SetRGBA(x, y, color.RGBA{R: uint8(i*47 + x), G: uint8(i*17 + y), B: uint8(i*21 + x + y), A: 255})
			}
		}
		i++
		return img, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if i != 25 || digest != "95002a74da8f3d6f" {
		t.Fatalf("sampled %d frames, digest %s", i, digest)
	}
}
