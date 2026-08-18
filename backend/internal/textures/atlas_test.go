package textures

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestDecodeMipsHasAlpha pins ISO_VOXEL_PLAN.md §5 Phase 4's detection
// primitive: a fully opaque texture must report HasAlpha=false, and a
// texture with even one non-fully-opaque texel (a real leaf canopy's gaps)
// must report true.
func TestDecodeMipsHasAlphaOpaqueTexture(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 100, G: 120, B: 80, A: 255})
		}
	}
	m, err := decodeMips(encodePNG(t, img))
	if err != nil {
		t.Fatal(err)
	}
	if m.HasAlpha {
		t.Error("fully opaque texture reported HasAlpha=true")
	}
}

func TestDecodeMipsHasAlphaTransparentTexel(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 40, G: 140, B: 40, A: 255})
		}
	}
	// One transparent corner texel, exactly like a leaf texture's gaps.
	img.SetNRGBA(0, 0, color.NRGBA{R: 0, G: 0, B: 0, A: 0})

	m, err := decodeMips(encodePNG(t, img))
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasAlpha {
		t.Error("texture with a transparent texel reported HasAlpha=false")
	}
}
