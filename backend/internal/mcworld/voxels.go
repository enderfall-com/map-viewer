package mcworld

import (
	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

// VoxelDepthConfig bounds how many Y layers of voxel data are stored per
// chunk. See ISO_VOXEL_PLAN.md §3.1: a fixed depth anchored at each column's
// canopy would lose the ground under tall trees, so depth is instead derived
// per chunk from the tallest canopy-to-ground span actually present.
type VoxelDepthConfig struct {
	// BelowGround is how many layers must be stored below the deepest solid
	// surface in the chunk, on top of whatever canopy height demands.
	BelowGround int
	// MinDepth and MaxDepth bound the result regardless of BelowGround.
	MinDepth, MaxDepth int
}

// DefaultVoxelDepthConfig matches ISO_VOXEL_PLAN.md §6's defaults.
func DefaultVoxelDepthConfig() VoxelDepthConfig {
	return VoxelDepthConfig{BelowGround: 16, MinDepth: 8, MaxDepth: 64}
}

// clamp corrects an invalid config rather than letting it produce a
// zero-or-negative depth, so a caller that constructs one directly (tests,
// or a config file that failed its own validation) cannot wedge the
// producer.
func (c VoxelDepthConfig) clamp() VoxelDepthConfig {
	if c.MinDepth <= 0 {
		c.MinDepth = 8
	}
	if c.MaxDepth < c.MinDepth {
		c.MaxDepth = c.MinDepth
	}
	if c.BelowGround < 0 {
		c.BelowGround = 0
	}
	return c
}

// voxels descends every column of a decoded chunk and records the top Depth
// layers of block id and light level below each column's own canopy.
//
// # Why this does not use ChunkSurface.Height for depth sizing
//
// ISO_VOXEL_PLAN.md §3.1 sizes Depth from "span = TopY[i] - GroundY[i]" with
// GroundY read off ChunkSurface.Height. That assumes leaves are marked
// Transparent, so a canopy-topped column's Height looks straight through the
// leaves to the real ground. This registry deliberately does the opposite
// (see surface.go's Block.Transparent doc and HANDOFF.md): leaves are NOT
// transparent for surface purposes, specifically so they read as the visible
// top-down surface. One consequence, easy to miss, is that Height and TopY
// are then defined by the exact same "topmost non-transparent block" rule
// for any solid-topped column, leaves included -- they are literally the
// same value, so Height carries no ground information for a canopy column at
// all, and span would always compute to 0 exactly where it matters most.
//
// Instead this walks each column's own descent one stage further: past the
// canopy's contiguous non-air run, through the air gap beneath it (if any),
// to the first non-air block after that gap -- operationally "the real
// ground", however deep it sits, with no name-based classification of what
// counts as canopy. A column with no such gap (a trunk column, packed solid
// from leaf cap to bedrock) contributes nothing to the span; that is fine,
// because the extrusion "basement skirt" fallback below the stored slab
// (§4.3) covers exactly that case no worse than the pre-voxel renderer did.
//
// Reuses sec.allTransparent to skip whole air sections while descending,
// exactly like surface(), but does NOT bulk-skip while filling layers below
// the canopy -- a section can be mostly air and still contain the one block
// (leaves, a torch) the slab exists to capture.
func (w *World) voxels(dc *decodedChunk, dim world.DimensionInfo) *world.ChunkVoxels {
	if dc.empty {
		return world.NewChunkVoxels(dc.pos, w.voxelDepth.MinDepth)
	}

	topY := dc.maxSectionY*16 + 15
	bottomY := dc.minSectionY * 16
	if dim.HasCeiling && dim.MaxY-1 < topY {
		topY = dim.MaxY - 1
	}

	var canopyY [world.ColumnCount]int
	var present [world.ColumnCount]bool
	maxSpan := 0

	for z := 0; z < mcmath.ChunkSize; z++ {
		for x := 0; x < mcmath.ChunkSize; x++ {
			i := z*mcmath.ChunkSize + x

			foundCanopy := false
			inGap := false
			foundGround := false
			var top, ground int

			for y := topY; y >= bottomY; y-- {
				sy := mcmath.FloorDiv(y, 16)
				sec, ok := dc.sections[sy]
				if !ok || sec.allTransparent {
					if foundCanopy {
						inGap = true
					}
					y = sy * 16 // loop decrement takes it below the section
					continue
				}
				ly := mcmath.FloorMod(y, 16)
				pe := sec.blockAt(x, ly, z)
				isAir := pe.id == 0

				switch {
				case !foundCanopy && !isAir:
					top = y
					foundCanopy = true
				case foundCanopy && !inGap && isAir:
					inGap = true
				case foundCanopy && inGap && !isAir:
					ground = y
					foundGround = true
				}
				if foundGround {
					break
				}
			}
			if !foundCanopy {
				continue // genuinely void column; leave TopY at 0
			}
			canopyY[i] = top
			present[i] = true
			if foundGround {
				if span := top - ground; span > maxSpan {
					maxSpan = span
				}
			}
		}
	}

	depth := maxSpan + w.voxelDepth.BelowGround
	if depth < w.voxelDepth.MinDepth {
		depth = w.voxelDepth.MinDepth
	}
	if depth > w.voxelDepth.MaxDepth {
		depth = w.voxelDepth.MaxDepth
	}

	cv := world.NewChunkVoxels(dc.pos, depth)
	for i, y := range canopyY {
		if present[i] {
			cv.TopY[i] = int16(y)
		}
	}

	for z := 0; z < mcmath.ChunkSize; z++ {
		for x := 0; x < mcmath.ChunkSize; x++ {
			i := z*mcmath.ChunkSize + x
			if !present[i] {
				continue
			}
			floor := canopyY[i] - depth + 1
			for y := canopyY[i]; y >= floor; y-- {
				sy := mcmath.FloorDiv(y, 16)
				sec, ok := dc.sections[sy]
				if !ok {
					continue // whole section absent means air; leave the zero value
				}
				ly := mcmath.FloorMod(y, 16)
				pe := sec.blockAt(x, ly, z)
				if pe.id == 0 {
					continue // air: leave the zero value, saves a write
				}
				cv.SetVoxel(i, y, pe.id, sec.lightAt(x, ly, z))
			}
		}
	}
	return cv
}
