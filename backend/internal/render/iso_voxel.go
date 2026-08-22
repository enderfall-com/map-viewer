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
	fill := r.Shader.Opts.UnexploredColor
	if r.Shader.Style == StyleContour {
		fill = color.NRGBA{}
	}
	img := FillUnexplored(fill)

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
//
// The self, left and right neighbour chunk pointers are each resolved once
// here rather than per voxel: a column's whole vertical loop, and every one
// of its per-voxel occlusion probes against the same two neighbour columns,
// touch exactly three columns in total, not one per Y level (PERF_PLAN.md
// §6, "reuse the chunk pointer per column").
func (r *Iso) drawVoxelColumn(
	img *image.NRGBA,
	surf *world.Surface,
	ib mcmath.IsoBounds,
	ppu, w, h int,
	a, b, x, z int,
	col world.Column,
) {
	vol := r.Volume
	self := vol.ColumnAt(x, z)
	rawTop, ok := self.TopY()
	depth := self.Depth()
	if !ok || depth <= 0 {
		// The column's chunk was not loaded into the volume window at all.
		// Should not happen given the window is derived the same way as the
		// surface window (see tiles.Manager's window tightening), but fail
		// safe onto the heightmap skirt rather than drawing nothing.
		r.drawColumn(img, surf, ib, ppu, w, h, a, b, x, z, col)
		return
	}
	top := rawTop
	if r.Sliced && top > r.SliceY {
		top = r.SliceY
	}
	// Depth is measured from the column's real top, so the floor must be too:
	// the slice only hides what is above it and never moves the stored slab.
	floor := rawTop - depth + 1
	if top < floor {
		// The slice is below everything this column has stored, so there is
		// nothing left of it to draw -- not even the basement skirt, which
		// would otherwise hang in the air under a fully cut-away column.
		return
	}
	reg := r.Shader.Blocks

	lx, lz := r.Proj.Camera.UnrotateInt(a, b+1)
	rx, rz := r.Proj.Camera.UnrotateInt(a+1, b)
	left := vol.ColumnAt(lx, lz)
	right := vol.ColumnAt(rx, rz)

	// A decoration (grass, flower, sapling, crop) is never its own voxel --
	// ISO_VOXEL_PLAN.md §4.4 and HANDOFF.md §4's "never on a side face"
	// invariant both apply just as much to the voxel path. If the column's
	// very top voxel is one, it is composited onto the top face of the
	// solid voxel one below it instead of drawn on its own.
	drawTop := top
	var decorID uint16
	var decorLight uint8
	if id, light, ok := r.sliceColumnBlockAt(self, top); ok && id != 0 && reg.Get(id).Decoration {
		decorID, decorLight = id, light
		drawTop = top - 1
	}

	for y := floor; y <= drawTop; y++ {
		id, light, ok := r.sliceColumnBlockAt(self, y)
		if !ok || id == 0 {
			continue // air, or a gap the descent never wrote
		}
		blk := reg.Get(id)
		if blk.Decoration {
			continue // a stray decoration below the column top: still not its own voxel
		}

		topFace, leftFace, rightFace := r.voxelFaceVisibility(self, left, right, y)
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
			id, light, topFace, leftFace, rightFace, isColumnTop, vDecorID, vDecorLight,
			blockHeight(blk))
	}

	r.drawBasementSkirt(img, surf, ib, ppu, w, h, a, b, x, z, floor)
}

// voxelFaceVisibility reports which of a voxel's three faces are visible,
// purely from occlusion -- independent of paint order, so both the real
// renderer above and the painter's-order equivalence test share exactly one
// definition of "what's visible" while testing "in what order" separately.
//
// Matches iso.go's screen-left/screen-right convention exactly: screen-left
// is the +b neighbour, screen-right is the +a neighbour. self/left/right are
// resolved once per column by the caller (drawVoxelColumn), not re-derived
// here, since all three stay the same chunk for every y in that column.
func (r *Iso) voxelFaceVisibility(self, left, right world.ColumnVoxels, y int) (top, lf, rf bool) {
	top = !r.voxelOccludesCol(self, y+1)
	lf = !r.voxelOccludesCol(left, y)
	rf = !r.voxelOccludesCol(right, y)
	return
}

// sliceColumnBlockAt is ColumnVoxels.At with the Y slice applied: anything
// above SliceY reads as air rather than as whatever is really there.
//
// It reports ok=true for those positions, not ok=false. The two answers mean
// different things to the occlusion predicate -- ok=false is "unknown, so
// assume it does not occlude", while air is "known empty" -- and above the cut
// the block genuinely is not there. Routing both drawing and occlusion through
// here is what makes the cut face visible: without it the terrain still
// standing above the slice would go on hiding the top face of the voxel the
// slice exposes, and the cut would render as a hole.
func (r *Iso) sliceColumnBlockAt(col world.ColumnVoxels, y int) (uint16, uint8, bool) {
	if r.Sliced && y > r.SliceY {
		return 0, 0, true
	}
	return col.At(y)
}

// voxelOccludesCol reports whether the block at a column's world Y fully
// hides what is behind it. ok=false from ColumnVoxels.At -- outside the
// stored slab or ungenerated -- must never be treated as occluding: guessing
// "occluded" punches holes in the image, while guessing "not occluded" only
// costs a few extra drawn faces that get overpainted (ISO_VOXEL_PLAN.md
// §4.3, §8).
//
// The result is cached per block id for the rest of this tile's render
// (Iso.occludesCache, PERF_PLAN.md §6): both Shader.HasTexture and
// Blocks.Get take a lock over their own id-keyed map, and a tile's occlusion
// scan revisits the same handful of distinct ids far more often than it
// meets a new one. HasTexture is still resolved before Occludes is read on
// that first, uncached visit -- not just when sampling a face's colour --
// because Block.Occludes can be downgraded by real texture alpha the first
// time a block's texture is resolved (§5 Phase 4, textures.Set.Get ->
// Registry.DowngradeOccludes); caching only the final Occludes value means
// that ordering only has to happen once per id, not once per voxel.
func (r *Iso) voxelOccludesCol(col world.ColumnVoxels, y int) bool {
	id, _, ok := r.sliceColumnBlockAt(col, y)
	if !ok || id == 0 {
		return false
	}
	if occ, cached := r.occludesCache[id]; cached {
		return occ
	}
	r.Shader.HasTexture(id)
	blk := r.Shader.Blocks.Get(id)
	// A block that does not fill its cell cannot hide its neighbour: a slab
	// or a stair leaves the upper half of the face behind it in plain view,
	// and treating it as opaque punches that half out of the image.
	occ := blk.Occludes && blockHeight(blk) >= 1
	if r.occludesCache == nil {
		r.occludesCache = make(map[uint16]bool, 32)
	}
	r.occludesCache[id] = occ
	return occ
}

// drawVoxel rasterises one voxel's visible faces: the same top-face diamond
// and side-face texel maths as drawColumn (iso.go), reused verbatim, with
// exactly one change per ISO_VOXEL_PLAN.md §4.2 -- a side face is now
// exactly one Y level tall (ppu pixels) at this voxel's own y, rather than a
// depthPx-tall run of the surface block's texture tiled once per level.
//
// blockH is the block's real height in blocks (Block.Height): 1 for an
// ordinary cube, 0.5 for a slab or a stair. It moves the top face down by the
// missing fraction and shortens the side faces to match, which is what stops
// slabs and stairs standing a full block tall. The top face's own diamond is
// unchanged in shape -- only where it sits -- so the lattice still tessellates
// exactly (see Iso's "Diamond tessellation" note).
func (r *Iso) drawVoxel(
	img *image.NRGBA,
	surf *world.Surface,
	ib mcmath.IsoBounds,
	ppu, w, h int,
	a, b, x, y, z int,
	blockID uint16, light uint8,
	topFace, leftFace, rightFace, isColumnTop bool,
	decorID uint16, decorLight uint8,
	blockH float64,
) {
	ppuF := float64(ppu)

	// Side faces reach from the block's own top down to the bottom of its
	// cell. At least one pixel, so a very thin block (a carpet) still reads as
	// having a side rather than vanishing edge-on.
	sidePx := int(math.Round(blockH * ppuF))
	if sidePx < 1 {
		sidePx = 1
	}

	u := float64(a-b) * mcmath.IsoHalfWidth
	v := float64(a+b)*mcmath.IsoHalfHeight - (float64(y)+blockH)*mcmath.IsoBlockHeight
	topPx := int(math.Round((u - ib.MinU) * ppuF))
	topPy := int(math.Round((v - ib.MinV) * ppuF))
	left := topPx - w/2

	if left >= mcmath.TileSize || left+w <= 0 || topPy >= mcmath.TileSize || topPy+h+sidePx <= 0 {
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
		for k := 1; k <= sidePx; k++ {
			py := topPy + jHi + k
			if py >= mcmath.TileSize {
				break
			}
			// Texel row still walks the full-block texture so a half-height
			// block shows the top half of its side texture, not a squashed
			// copy of the whole thing.
			tv := (k - 1) % ppu
			setPixel(img, px, py, blocks.Scale(sampleSide(sideTu, tv), shade))
		}
	}
}

// blockHeight is a block's rendered height in blocks, clamped to (0, 1].
//
// blocks.json carries this for slabs, stairs and similar (Block.Height), and
// until partial-height rendering existed nothing read it, so every one of
// them was drawn standing a full block tall. A zero or missing value means
// "not specified", which is a full cube -- not a zero-height one.
func blockHeight(blk blocks.Block) float64 {
	h := float64(blk.Height)
	if h <= 0 || h > 1 {
		return 1
	}
	return h
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
