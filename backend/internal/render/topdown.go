package render

import (
	"image"
	"image/color"
	"math"

	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

// TopDown renders the plan view: for every X/Z column, the colour of the block
// visible from directly above.
//
// # Where this sits in the pyramid
//
// Tiles are rendered from world data at one base zoom only -- by default zoom 6,
// where one pixel is exactly one block. Lower zooms are built by downsampling
// four children, which is both far cheaper and more stable than re-sampling the
// world at every level. Higher zooms need no tiles at all: magnifying the
// 1-block-per-pixel data with nearest-neighbour filtering is exactly what
// Minecraft terrain should look like when you zoom in, and it keeps block edges
// perfectly crisp. The renderer still supports rendering at other zooms so a
// future texture-based mode can raise the base level without restructuring
// anything.
type TopDown struct {
	Shader *Shader
}

// NewTopDown builds a top-down renderer.
func NewTopDown(s *Shader) *TopDown { return &TopDown{Shader: s} }

// SurfaceBounds returns the world window a tile needs, including the one-block
// margin the relief shader reads for its north and west neighbours.
func (r *TopDown) SurfaceBounds(pos mcmath.TilePos) mcmath.BlockBounds {
	return pos.Bounds().Expand(1)
}

// Render draws one tile. The surface must cover at least SurfaceBounds(pos).
//
// The pixel loop handles both the zoomed-out case, where a pixel averages many
// blocks, and the zoomed-in case, where a block spans many pixels, without
// branching per pixel on which regime it is in.
func (r *TopDown) Render(pos mcmath.TilePos, surf *world.Surface) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, mcmath.TileSize, mcmath.TileSize))
	b := pos.Bounds()
	bpp := mcmath.BlocksPerPixel(pos.Zoom)

	if bpp <= 1 {
		r.renderMagnified(img, b, bpp, surf)
	} else {
		r.renderAveraged(img, b, int(bpp), surf)
	}
	return img
}

// renderMagnified handles zoom >= base, where each pixel samples exactly one
// block or, once the base zoom is raised high enough for real textures, one
// texel within a block. Nearest-neighbour sampling is intentional: it is
// what keeps individual blocks crisp instead of smearing them.
//
// ppb (pixels per block) is the reciprocal of bpp. At the traditional base
// zoom bpp is exactly 1 and ppb is 1, so every pixel is a fresh block and
// this reduces to exactly one FaceSampler call per pixel, same as the old
// one-colour-per-block renderer. Once the base zoom is raised for real
// texture detail, ppb pixels share one block: iterating block-by-block and
// building one sampler per block (rather than per pixel) is what keeps
// shading (relief, light) a once-per-block cost instead of a once-per-texel
// one for any block without a resolved texture -- stairs, slabs, water, and
// everything else the simplified model resolver does not attempt.
//
// Block boundaries always land on a multiple of ppb tile pixels, because a
// tile's span in blocks (TileSpanBlocks) is always an integer, so iterating
// blocks and filling a ppb x ppb square needs no extra bookkeeping to stay
// aligned.
func (r *TopDown) renderMagnified(img *image.NRGBA, b mcmath.BlockBounds, bpp float64, surf *world.Surface) {
	ppb := 1
	if bpp < 1 {
		ppb = int(math.Round(1 / bpp))
	}
	blocksPerEdge := mcmath.TileSize / ppb
	for bz := 0; bz < blocksPerEdge; bz++ {
		wz := b.MinZ + bz
		baseY := bz * ppb
		for bx := 0; bx < blocksPerEdge; bx++ {
			wx := b.MinX + bx
			baseX := bx * ppb
			sample, _ := r.Shader.FaceSampler(surf, wx, wz, FaceKindTop, ppb)
			for ty := 0; ty < ppb; ty++ {
				row := img.PixOffset(baseX, baseY+ty)
				for tx := 0; tx < ppb; tx++ {
					c := sample(tx, ty)
					o := row + tx*4
					img.Pix[o+0] = c.R
					img.Pix[o+1] = c.G
					img.Pix[o+2] = c.B
					img.Pix[o+3] = c.A
				}
			}
		}
	}
}

// renderAveraged handles zoom < base, where each pixel covers an n x n block
// footprint. Averaging rather than point-sampling is what stops a zoomed-out
// map from shimmering as it pans, and it keeps thin features like rivers and
// roads visible instead of aliasing them away.
//
// Unexplored columns are excluded from the average so a partially-generated
// area blends toward its real terrain rather than toward the background.
func (r *TopDown) renderAveraged(img *image.NRGBA, b mcmath.BlockBounds, n int, surf *world.Surface) {
	if n < 1 {
		n = 1
	}
	unexplored := r.Shader.Opts.UnexploredColor
	for py := 0; py < mcmath.TileSize; py++ {
		z0 := b.MinZ + py*n
		row := img.PixOffset(0, py)
		for px := 0; px < mcmath.TileSize; px++ {
			x0 := b.MinX + px*n
			var sr, sg, sb, count uint32
			for dz := 0; dz < n; dz++ {
				for dx := 0; dx < n; dx++ {
					c, ok := r.Shader.ColumnColor(surf, x0+dx, z0+dz)
					if !ok {
						continue
					}
					sr += uint32(c.R)
					sg += uint32(c.G)
					sb += uint32(c.B)
					count++
				}
			}
			o := row + px*4
			if count == 0 {
				img.Pix[o+0] = unexplored.R
				img.Pix[o+1] = unexplored.G
				img.Pix[o+2] = unexplored.B
				img.Pix[o+3] = unexplored.A
				continue
			}
			img.Pix[o+0] = uint8(sr / count)
			img.Pix[o+1] = uint8(sg / count)
			img.Pix[o+2] = uint8(sb / count)
			// Partially-explored pixels stay fully opaque; coverage is conveyed
			// by the terrain itself, not by transparency.
			img.Pix[o+3] = 255
		}
	}
}

// FillUnexplored paints a whole tile as unexplored terrain. Tile generation
// uses this to answer for regions with no chunks at all without running the
// renderer or touching the world.
func FillUnexplored(c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, mcmath.TileSize, mcmath.TileSize))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i+0] = c.R
		img.Pix[i+1] = c.G
		img.Pix[i+2] = c.B
		img.Pix[i+3] = c.A
	}
	return img
}
