package render

import (
	"image"
	"image/color"
	"math"

	"github.com/enderfall/minecraft-map/backend/internal/blocks"
	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

// renderVoxel is Iso.Render's real-voxel path (ISO_VOXEL_PLAN.md §4). It
// keeps Render's outer structure exactly -- sweep depth planes d=a+b far to
// near, iterate columns within a plane -- and, within each column, draws
// every stored voxel bottom-up (ascending y) instead of one extruded
// surface. See iso_voxel_test.go's painter's-order equivalence test for why
// this ordering is safe; do not change the sweep direction without rerunning
// it.
func (r *Iso) renderVoxel(pos mcmath.TilePos, surf *world.Surface) *image.NRGBA {
	img := FillUnexplored(r.Shader.Opts.UnexploredColor)

	ppu := int(mcmath.PixelsPerBlock(pos.Zoom))
	if pos.Zoom < MinDirectZoom || ppu < 2 {
		return img
	}
	ib := mcmath.IsoTileBounds(pos)
	cam := r.Proj.Camera

	w := 2 * ppu
	h := ppu

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
			r.drawVoxelColumn(img, surf, ib, ppu, w, h, a, bb, x, z, col)
		}
	}
	return img
}

// drawVoxelColumn draws every stored voxel of one column, bottom-up, plus
// the basement skirt below the stored slab.
func (r *Iso) drawVoxelColumn(
	img *image.NRGBA,
	surf *world.Surface,
	ib mcmath.IsoBounds,
	ppu, w, h int,
	a, b, x, z int,
	col world.Column,
) {
	vol := r.Volume
	top, ok := vol.TopY(x, z)
	depth := vol.Depth(x, z)
	if !ok || depth <= 0 {
		// The column's chunk was not loaded into the volume window at all.
		// Should not happen given the window is derived the same way as the
		// surface window (see tiles.Manager's window tightening), but fail
		// safe onto the heightmap skirt rather than drawing nothing.
		r.drawColumn(img, surf, ib, ppu, w, h, a, b, x, z, col)
		return
	}
	floor := top - depth + 1
	reg := r.Shader.Blocks

	// A decoration (grass, flower, sapling, crop) is never its own voxel --
	// ISO_VOXEL_PLAN.md §4.4 and HANDOFF.md §4's "never on a side face"
	// invariant both apply just as much to the voxel path. If the column's
	// very top voxel is one, it is composited onto the top face of the
	// solid voxel one below it instead of drawn on its own.
	drawTop := top
	var decorID uint16
	var decorLight uint8
	if id, light, ok := vol.BlockAt(x, top, z); ok && id != 0 && reg.Get(id).Decoration {
		decorID, decorLight = id, light
		drawTop = top - 1
	}

	for y := floor; y <= drawTop; y++ {
		id, light, ok := vol.BlockAt(x, y, z)
		if !ok || id == 0 {
			continue // air, or a gap the descent never wrote
		}
		blk := reg.Get(id)
		if blk.Decoration {
			continue // a stray decoration below the column top: still not its own voxel
		}

		topFace, leftFace, rightFace := r.voxelFaceVisibility(vol, a, b, x, y, z)
		if !topFace && !leftFace && !rightFace {
			continue
		}

		var vDecorID uint16
		var vDecorLight uint8
		if topFace && y == drawTop {
			vDecorID, vDecorLight = decorID, decorLight
		}
		isColumnTop := y == drawTop

		r.drawVoxel(img, surf, ib, ppu, w, h, a, b, x, y, z,
			id, light, topFace, leftFace, rightFace, isColumnTop, vDecorID, vDecorLight)
	}

	r.drawBasementSkirt(img, surf, ib, ppu, w, h, a, b, x, z, floor)
}

// voxelFaceVisibility reports which of a voxel's three faces are visible,
// purely from occlusion -- independent of paint order, so both the real
// renderer above and the painter's-order equivalence test share exactly one
// definition of "what's visible" while testing "in what order" separately.
//
// Matches iso.go's screen-left/screen-right convention exactly: screen-left
// is the +b neighbour, screen-right is the +a neighbour.
func (r *Iso) voxelFaceVisibility(vol *world.Volume, a, b, x, y, z int) (top, left, right bool) {
	top = !r.voxelOccludes(vol, x, y+1, z)
	lx, lz := r.Proj.Camera.UnrotateInt(a, b+1)
	left = !r.voxelOccludes(vol, lx, y, lz)
	rx, rz := r.Proj.Camera.UnrotateInt(a+1, b)
	right = !r.voxelOccludes(vol, rx, y, rz)
	return
}

// voxelOccludes reports whether the block at a world position fully hides
// what is behind it. ok=false from Volume.BlockAt -- outside the stored slab
// or ungenerated -- must never be treated as occluding: guessing "occluded"
// punches holes in the image, while guessing "not occluded" only costs a few
// extra drawn faces that get overpainted (ISO_VOXEL_PLAN.md §4.3, §8).
//
// HasTexture is called before reading Occludes, not just when sampling a
// face's colour, because Block.Occludes can be downgraded by real texture
// alpha the first time a block's texture is resolved (§5 Phase 4,
// textures.Set.Get -> Registry.DowngradeOccludes). Without forcing that
// resolution here too, the very first occlusion check for a given block id
// in the process's lifetime could run before its own face-sampling step
// resolves it, using the stale flag-derived value for that one check.
func (r *Iso) voxelOccludes(vol *world.Volume, x, y, z int) bool {
	id, _, ok := vol.BlockAt(x, y, z)
	if !ok || id == 0 {
		return false
	}
	r.Shader.HasTexture(id)
	return r.Shader.Blocks.Get(id).Occludes
}

// drawVoxel rasterises one voxel's visible faces: the same top-face diamond
// and side-face texel maths as drawColumn (iso.go), reused verbatim, with
// exactly one change per ISO_VOXEL_PLAN.md §4.2 -- a side face is now
// exactly one Y level tall (ppu pixels) at this voxel's own y, rather than a
// depthPx-tall run of the surface block's texture tiled once per level.
func (r *Iso) drawVoxel(
	img *image.NRGBA,
	surf *world.Surface,
	ib mcmath.IsoBounds,
	ppu, w, h int,
	a, b, x, y, z int,
	blockID uint16, light uint8,
	topFace, leftFace, rightFace, isColumnTop bool,
	decorID uint16, decorLight uint8,
) {
	ppuF := float64(ppu)

	u := float64(a-b) * mcmath.IsoHalfWidth
	v := float64(a+b)*mcmath.IsoHalfHeight - float64(y+1)*mcmath.IsoBlockHeight
	topPx := int(math.Round((u - ib.MinU) * ppuF))
	topPy := int(math.Round((v - ib.MinV) * ppuF))
	left := topPx - w/2

	if left >= mcmath.TileSize || left+w <= 0 || topPy >= mcmath.TileSize || topPy+h+ppu <= 0 {
		return
	}

	var sampleTop, sampleSide func(tu, tv int) color.NRGBA
	if topFace {
		sampleTop = r.Shader.VoxelSampler(surf, x, z, y, blockID, light, FaceKindTop, isColumnTop, ppu)
		if decorID != 0 {
			base := sampleTop
			decorSample := r.Shader.VoxelSampler(surf, x, z, y, decorID, decorLight, FaceKindTop, false, ppu)
			sampleTop = func(tu, tv int) color.NRGBA {
				return alphaOver(decorSample(tu, tv), base(tu, tv))
			}
		}
	}
	if leftFace || rightFace {
		sampleSide = r.Shader.VoxelSampler(surf, x, z, y, blockID, light, FaceKindSide, false, ppu)
	}

	for i := 0; i < w; i++ {
		px := left + i
		if px < 0 || px >= mcmath.TileSize {
			continue
		}
		e := min(i, w-1-i)
		jLo := (h - e - 1 + 1) / 2 // ceil((h-e-1)/2), identical to drawColumn -- do not touch
		jHi := (h + e - 1) / 2

		if topFace {
			dpx := i - ppu
			for j := jLo; j <= jHi; j++ {
				fa := (float64(dpx) + 2*float64(j)) / (2 * float64(ppu))
				fb := (2*float64(j) - float64(dpx)) / (2 * float64(ppu))
				tu := int(fa * float64(ppu))
				tv := int(fb * float64(ppu))
				setPixel(img, px, topPy+j, sampleTop(tu, tv))
			}
		}

		faceOn := rightFace
		shade := FaceRight
		sideTu := i - ppu
		if i < w/2 {
			faceOn = leftFace
			shade = FaceLeft
			sideTu = i
		}
		if !faceOn {
			continue
		}
		for k := 1; k <= ppu; k++ {
			py := topPy + jHi + k
			if py >= mcmath.TileSize {
				break
			}
			tv := k - 1
			setPixel(img, px, py, blocks.Scale(sampleSide(sideTu, tv), shade))
		}
	}
}

// drawBasementSkirt extends a column's side faces below the stored voxel
// slab using the pre-voxel extrusion skirt, so a cliff taller than the
// slab's Depth never leaves a hole (§4.3). It textures the fallback run with
// the deepest stored voxel's own side texture and uses the flattened
// Surface's neighbour heights to decide how far down to go -- exactly
// drawColumn's own mechanism, just anchored at floor instead of the column's
// single rendered Y.
func (r *Iso) drawBasementSkirt(
	img *image.NRGBA,
	surf *world.Surface,
	ib mcmath.IsoBounds,
	ppu, w, h int,
	a, b, x, z, floor int,
) {
	leftDepth := r.exposedDepth(surf, a, b+1, floor)
	rightDepth := r.exposedDepth(surf, a+1, b, floor)
	maxDepth := max(leftDepth, rightDepth)
	if maxDepth <= 0 {
		return
	}

	id, light, ok := r.Volume.BlockAt(x, floor, z)
	if !ok || id == 0 {
		return
	}
	sampleSide := r.Shader.VoxelSampler(surf, x, z, floor, id, light, FaceKindSide, false, ppu)

	ppuF := float64(ppu)
	u := float64(a-b) * mcmath.IsoHalfWidth
	v := float64(a+b)*mcmath.IsoHalfHeight - float64(floor+1)*mcmath.IsoBlockHeight
	topPx := int(math.Round((u - ib.MinU) * ppuF))
	topPy := int(math.Round((v - ib.MinV) * ppuF))
	left := topPx - w/2

	leftPx := leftDepth * ppu
	rightPx := rightDepth * ppu
	if left >= mcmath.TileSize || left+w <= 0 || topPy+h+max(leftPx, rightPx) <= 0 {
		return
	}

	for i := 0; i < w; i++ {
		px := left + i
		if px < 0 || px >= mcmath.TileSize {
			continue
		}
		e := min(i, w-1-i)
		jHi := (h + e - 1) / 2

		depthPx := rightPx
		shade := FaceRight
		sideTu := i - ppu
		if i < w/2 {
			depthPx = leftPx
			shade = FaceLeft
			sideTu = i
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
