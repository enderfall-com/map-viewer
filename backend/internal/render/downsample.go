package render

import (
	"image"

	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
)

// Composite builds a parent tile from its four children.
//
// Children are indexed as mcmath.TilePos.Children() orders them:
//
//	0 = north-west   1 = north-east
//	2 = south-west   3 = south-east
//
// A nil child leaves its quadrant filled with the unexplored colour, so a
// partially generated world produces correct parents immediately instead of
// waiting for every child to exist.
//
// # Why a 2x2 box filter
//
// The reduction is exactly 2:1, so a 2x2 box is the true area average: every
// source pixel contributes exactly once with equal weight. Wider kernels such
// as Lanczos or bicubic would pull in neighbours the destination pixel does not
// actually cover, which on pixel-art terrain reads as blur and ringing around
// block edges. Averaging in linear-ish integer space keeps coastlines and roads
// visible as they shrink, which point sampling would drop entirely.
func Composite(children [4]*image.NRGBA, unexplored [4]bool, fill *image.NRGBA) *image.NRGBA {
	const half = mcmath.TileSize / 2
	out := image.NewNRGBA(image.Rect(0, 0, mcmath.TileSize, mcmath.TileSize))

	for idx := 0; idx < 4; idx++ {
		ox := (idx % 2) * half
		oy := (idx / 2) * half

		src := children[idx]
		if src == nil {
			src = fill
		}
		if src == nil {
			continue
		}
		downsampleInto(out, ox, oy, src)
	}
	return out
}

// downsampleInto writes a half-size box-filtered copy of src at (ox,oy) in dst.
func downsampleInto(dst *image.NRGBA, ox, oy int, src *image.NRGBA) {
	const half = mcmath.TileSize / 2
	for y := 0; y < half; y++ {
		sy0 := src.PixOffset(0, y*2)
		sy1 := src.PixOffset(0, y*2+1)
		dRow := dst.PixOffset(ox, oy+y)
		for x := 0; x < half; x++ {
			a := sy0 + x*8 // two source pixels per destination pixel
			b := a + 4
			c := sy1 + x*8
			d := c + 4

			o := dRow + x*4
			dst.Pix[o+0] = avg4(src.Pix[a+0], src.Pix[b+0], src.Pix[c+0], src.Pix[d+0])
			dst.Pix[o+1] = avg4(src.Pix[a+1], src.Pix[b+1], src.Pix[c+1], src.Pix[d+1])
			dst.Pix[o+2] = avg4(src.Pix[a+2], src.Pix[b+2], src.Pix[c+2], src.Pix[d+2])
			dst.Pix[o+3] = avg4(src.Pix[a+3], src.Pix[b+3], src.Pix[c+3], src.Pix[d+3])
		}
	}
}

// avg4 rounds to nearest so repeated pyramid levels do not drift darker.
func avg4(a, b, c, d uint8) uint8 {
	return uint8((uint32(a) + uint32(b) + uint32(c) + uint32(d) + 2) / 4)
}

// IsBlank reports whether every pixel of an image equals the given colour. Tile
// storage uses this to avoid writing megabytes of identical "unexplored" tiles
// for the empty parts of a world.
func IsBlank(img *image.NRGBA, r, g, b, a uint8) bool {
	if img == nil {
		return true
	}
	for i := 0; i < len(img.Pix); i += 4 {
		if img.Pix[i] != r || img.Pix[i+1] != g || img.Pix[i+2] != b || img.Pix[i+3] != a {
			return false
		}
	}
	return true
}
