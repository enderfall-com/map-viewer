package render

import (
	"image"
	"image/color"
	"math"

	"github.com/enderfall/minecraft-map/backend/internal/blocks"
	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

// Iso renders the isometric view.
//
// # Occlusion
//
// Columns are drawn far-to-near in ascending camera depth (a+b), so a nearer
// column always overpaints anything behind it. This is a painter's algorithm
// with no depth buffer, which is correct here because every column is a single
// convex prism standing on the ground plane: two columns with the same depth
// key never overlap, and any pair with different keys is strictly ordered.
//
// # Seams
//
// A tile is rendered from every column that could possibly paint into it, not
// merely the columns whose ground footprint lies inside it. That set comes from
// inverting the projection across the entire world height range
// (mcmath.WorldFootprint). A mountain hundreds of blocks outside the tile's own
// footprint still gets drawn, then the result is cropped to the tile. Since
// neighbouring tiles derive their column sets the same way from overlapping
// world data, adjacent tiles agree exactly on every shared pixel, so no seam
// and no clipped peak is possible.
//
// # Diamond tessellation
//
// Each column's top face is a 2:1 diamond of W x H pixels with W = 2H. Adjacent
// columns are offset by exactly (W/2, H/2), so the rasterisation must tile the
// plane perfectly under that offset or the terrain shows a moire of gaps. Using
// run starts of |2j+1-H| and widths of W-2*start gives each diamond exactly
// W*H/2 pixels, which equals the lattice's fundamental domain area -- an exact
// tessellation with no gaps and no double-coverage.
type Iso struct {
	Shader *Shader
	Proj   mcmath.IsoProjection

	// EdgeSkirt is how many blocks of side face to draw where the neighbouring
	// column is unexplored. A small value renders the frontier of generated
	// terrain as a clean lip rather than a wall dropping into the void.
	EdgeSkirt int

	// Volume carries real per-column block data for the top layers of each
	// column (ISO_VOXEL_PLAN.md). nil (the default) means the heightmap path
	// below is used unchanged; callers set this only when voxel rendering is
	// enabled and the active world provider actually supports it.
	Volume *world.Volume
}

// NewIso builds an isometric renderer for a camera direction. edgeSkirt <= 0
// falls back to 4, the long-standing default, so existing callers that pass
// a zero-value config keep today's behaviour exactly.
func NewIso(s *Shader, cam mcmath.IsoCamera, edgeSkirt int) *Iso {
	if edgeSkirt <= 0 {
		edgeSkirt = 4
	}
	return &Iso{Shader: s, Proj: mcmath.NewIsoProjection(cam), EdgeSkirt: edgeSkirt}
}

// SurfaceBounds returns the world window required to render a tile without
// seams, given the dimension's height range.
func (r *Iso) SurfaceBounds(pos mcmath.TilePos, minY, maxY int) mcmath.BlockBounds {
	// One extra block on each side covers the neighbour lookups the skirt
	// depth calculation performs.
	return r.Proj.WorldFootprint(mcmath.IsoTileBounds(pos), minY, maxY).Expand(1)
}

// MinDirectZoom is the lowest zoom at which rendering directly from world data
// is possible, and it is a hard geometric limit rather than a tuning choice.
//
// Neighbouring columns sit exactly half a diamond apart, at (ppu, ppu/2) pixels.
// At zoom 6 there is one pixel per iso unit, so that vertical offset is half a
// pixel and no integer rasterisation can tessellate the lattice -- terrain would
// show a permanent moire of gaps and double-covered pixels. From zoom 7 (two
// pixels per iso unit) the offset is a whole number and the tessellation is
// exact.
//
// Levels below this are built by downsampling children, which is both cheaper
// and visually better: at zoom 6 and out, a single tile spans so much world
// that iterating its columns would dominate everything else.
const MinDirectZoom = mcmath.BaseZoom + 1 // 7: two pixels per iso unit

// CanRenderDirect reports whether a zoom can be rendered from world data.
func (r *Iso) CanRenderDirect(zoom int) bool { return zoom >= MinDirectZoom }

// Render draws one isometric tile. The surface must cover at least
// SurfaceBounds(pos, minY, maxY). If r.Volume is set, it dispatches to the
// real voxel renderer (iso_voxel.go) instead of this heightmap extrusion.
func (r *Iso) Render(pos mcmath.TilePos, surf *world.Surface) *image.NRGBA {
	if r.Volume != nil {
		return r.renderVoxel(pos, surf)
	}

	img := FillUnexplored(r.Shader.Opts.UnexploredColor)

	ppu := int(mcmath.PixelsPerBlock(pos.Zoom)) // pixels per iso unit
	if pos.Zoom < MinDirectZoom || ppu < 2 {
		// Callers must composite these from children; returning the unexplored
		// fill is the safe answer rather than rendering something wrong.
		return img
	}
	ib := mcmath.IsoTileBounds(pos)
	cam := r.Proj.Camera

	w := 2 * ppu // diamond width in pixels
	h := ppu     // diamond height in pixels

	// Convert the surface's block window into camera-space (a,b) bounds by
	// rotating its corners; the rotation is a signed axis swap so the rectangle
	// stays a rectangle.
	b := surf.Bounds
	aMin, bMin := math.MaxInt32, math.MaxInt32
	aMax, bMax := math.MinInt32, math.MinInt32
	for _, c := range [4][2]int{
		{b.MinX, b.MinZ}, {b.MaxX - 1, b.MinZ},
		{b.MinX, b.MaxZ - 1}, {b.MaxX - 1, b.MaxZ - 1},
	} {
		ca, cb := cam.RotateInt(c[0], c[1])
		aMin, aMax = min(aMin, ca), max(aMax, ca)
		bMin, bMax = min(bMin, cb), max(bMax, cb)
	}

	// Sweep depth planes far to near. Iterating by depth key avoids sorting
	// hundreds of thousands of columns and allocates nothing.
	for d := aMin + bMin; d <= aMax+bMax; d++ {
		aLo := max(aMin, d-bMax)
		aHi := min(aMax, d-bMin)
		for a := aLo; a <= aHi; a++ {
			bb := d - a
			x, z := cam.UnrotateInt(a, bb)

			col := surf.At(x, z)
			if !col.Present() {
				continue
			}
			r.drawColumn(img, surf, ib, ppu, w, h, a, bb, x, z, col)
		}
	}
	return img
}

// drawColumn rasterises one column's top face and its two visible side faces.
func (r *Iso) drawColumn(
	img *image.NRGBA,
	surf *world.Surface,
	ib mcmath.IsoBounds,
	ppu, w, h int,
	a, b, x, z int,
	col world.Column,
) {
	y := col.RenderY()
	ppuF := float64(ppu)

	// Top vertex of the top face, in tile pixel space.
	u := float64(a-b) * mcmath.IsoHalfWidth
	v := float64(a+b)*mcmath.IsoHalfHeight - float64(y+1)*mcmath.IsoBlockHeight
	topPx := int(math.Round((u - ib.MinU) * ppuF))
	topPy := int(math.Round((v - ib.MinV) * ppuF))
	left := topPx - w/2

	// Horizontal reject before doing any more work.
	if left >= mcmath.TileSize || left+w <= 0 || topPy >= mcmath.TileSize {
		return
	}

	// Side faces run down to whichever neighbour they are exposed against: the
	// +b neighbour for the screen-left face, the +a neighbour for the
	// screen-right face. Clamping to the actual neighbour height means flat
	// terrain costs almost no skirt while a cliff draws exactly as much wall as
	// it needs -- no arbitrary cap, so tall drops never leave a hole.
	leftDepth := r.exposedDepth(surf, a, b+1, y)
	rightDepth := r.exposedDepth(surf, a+1, b, y)
	maxDepth := max(leftDepth, rightDepth)

	if topPy+h+maxDepth*ppu <= 0 {
		return // entirely above the tile
	}

	// sampleTop/sampleSide return the colour for one texel. For the
	// overwhelming majority of columns -- anything with no resolved texture,
	// which is every column at all when no texture source is configured --
	// these ignore their (tu,tv) argument and return the same precomputed
	// flat colour for every pixel, so shading (relief, light) is computed
	// once per column exactly as it always was, not once per pixel.
	sampleTop, sampleSide, ok := r.faceSamplers(surf, x, z, ppu)
	if !ok {
		return
	}

	leftPx := leftDepth * ppu
	rightPx := rightDepth * ppu

	for i := 0; i < w; i++ {
		px := left + i
		if px < 0 || px >= mcmath.TileSize {
			continue
		}
		// Diamond rows covering this pixel column. e is the column's distance
		// from the nearer diamond edge.
		e := min(i, w-1-i)
		jLo := (h - e - 1 + 1) / 2 // ceil((h-e-1)/2)
		jHi := (h + e - 1) / 2

		// Top-face texel coordinates for this column of the diamond.
		//
		// (i,j) is inverted back to a fractional position (fa,fb) in [0,1]^2
		// across the block's top face: the diamond's top vertex (i=w/2,j=0)
		// is (fa,fb)=(0,0), its left and right vertices are (0,1) and (1,0),
		// and its bottom vertex is (1,1). This falls out of solving the same
		// two equations drawColumn's top-vertex projection uses, for a
		// fractional offset instead of a whole block. fa,fb map directly to
		// U,V on the block's top texture because the top-down projection has
		// no rotation for the default camera (a=world X, b=world Z).
		dpx := i - ppu
		for j := jLo; j <= jHi; j++ {
			fa := (float64(dpx) + 2*float64(j)) / (2 * float64(ppu))
			fb := (2*float64(j) - float64(dpx)) / (2 * float64(ppu))
			tu := int(fa * float64(ppu))
			tv := int(fb * float64(ppu))
			setPixel(img, px, topPy+j, sampleTop(tu, tv))
		}

		// Side face hangs directly below the diamond's lower edge. Its
		// horizontal texel coordinate varies linearly across the face --
		// exact, not approximate, because the underlying projection is
		// affine -- and its vertical coordinate tiles once per exposed Y
		// level, so a ten-block cliff shows ten repeats of the texture
		// rather than one stretched smear.
		depthPx := rightPx
		shade := FaceRight
		sideTu := i - ppu // right face spans i in [ppu, 2ppu)
		if i < w/2 {
			depthPx = leftPx
			shade = FaceLeft
			sideTu = i // left face spans i in [0, ppu)
		}
		if depthPx <= 0 {
			continue
		}
		for k := 1; k <= depthPx; k++ {
			py := topPy + jHi + k
			if py >= mcmath.TileSize {
				break
			}
			tv := (k - 1) % ppu
			setPixel(img, px, py, blocks.Scale(sampleSide(sideTu, tv), shade))
		}
	}
}

// faceSamplers returns per-texel colour functions for a column's top and
// side faces, and whether the column should be drawn at all (false for an
// absent column, mirroring ColumnColor/TexelColor).
//
// A block with no resolved texture gets a constant sampler: the shaded
// colour is computed exactly once here, matching the pre-texture renderer's
// cost precisely, rather than being recomputed for every one of a diamond's
// pixels only to discard the (tu,tv) it never needed.
func (r *Iso) faceSamplers(surf *world.Surface, x, z, ppu int) (top, side func(tu, tv int) color.NRGBA, ok bool) {
	top, ok = r.Shader.FaceSampler(surf, x, z, FaceKindTop, ppu)
	if !ok {
		return nil, nil, false
	}
	side, _ = r.Shader.FaceSampler(surf, x, z, FaceKindSide, ppu)
	return top, side, true
}

// exposedDepth returns how many blocks of side face are visible against the
// neighbour at camera-space (na, nb), for a column whose surface is at y.
func (r *Iso) exposedDepth(surf *world.Surface, na, nb, y int) int {
	nx, nz := r.Proj.Camera.UnrotateInt(na, nb)
	n := surf.At(nx, nz)
	if !n.Present() {
		return r.EdgeSkirt
	}
	d := y - n.RenderY()
	if d < 0 {
		return 0
	}
	return d
}

// setPixel writes one opaque pixel with bounds checking.
func setPixel(img *image.NRGBA, x, y int, c color.NRGBA) {
	if x < 0 || y < 0 || x >= mcmath.TileSize || y >= mcmath.TileSize {
		return
	}
	o := img.PixOffset(x, y)
	img.Pix[o+0] = c.R
	img.Pix[o+1] = c.G
	img.Pix[o+2] = c.B
	img.Pix[o+3] = c.A
}
