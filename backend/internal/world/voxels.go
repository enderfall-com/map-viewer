package world

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"

	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
)

// ChunkVoxels holds the top Depth layers of every column in one chunk, stored
// relative to each column's own top so the slab stays dense and small no
// matter how much relief the chunk has. See ISO_VOXEL_PLAN.md §3.1.
type ChunkVoxels struct {
	Pos   mcmath.ChunkPos
	Depth int // number of Y layers stored per column

	// TopY is the Y of the topmost non-air block in each column -- the
	// canopy, not the ground. This is deliberately NOT ChunkSurface.Height,
	// which is the topmost *solid* block; the gap between them is the whole
	// point of storing voxels at all.
	TopY [ColumnCount]int16

	// Block and Light are Depth*ColumnCount, indexed layer*ColumnCount + col,
	// where layer = TopY[col] - y. Layer 0 is always the column's top block.
	// A layer below a void column, or below the lowest block Anvil actually
	// stored, is left at its zero value (air, light 0).
	Block []uint16
	Light []uint8
}

// NewChunkVoxels allocates an empty slab of the given depth. depth is clamped
// to at least 1 so a misconfigured caller cannot produce a slab that can
// never hold anything.
func NewChunkVoxels(pos mcmath.ChunkPos, depth int) *ChunkVoxels {
	if depth < 1 {
		depth = 1
	}
	return &ChunkVoxels{
		Pos:   pos,
		Depth: depth,
		Block: make([]uint16, depth*ColumnCount),
		Light: make([]uint8, depth*ColumnCount),
	}
}

// layerIndex returns the flat Block/Light offset for a column index (0..255,
// see Index) and world Y, and whether that Y falls inside the stored slab.
func (cv *ChunkVoxels) layerIndex(col, y int) (int, bool) {
	layer := int(cv.TopY[col]) - y
	if layer < 0 || layer >= cv.Depth {
		return 0, false
	}
	return layer*ColumnCount + col, true
}

// At returns the block id and light level at a local column index and world
// Y. ok is false when y falls outside the stored slab for that column --
// callers must treat that as "unknown", never as "air" or "occluded".
func (cv *ChunkVoxels) At(col, y int) (id uint16, light uint8, ok bool) {
	i, ok := cv.layerIndex(col, y)
	if !ok {
		return 0, 0, false
	}
	return cv.Block[i], cv.Light[i], true
}

// SetVoxel stores a block id and light level at a local column index and
// world Y. It is a no-op when y falls outside the stored slab, so a producer
// can iterate a generous Y range without bounds-checking itself.
func (cv *ChunkVoxels) SetVoxel(col, y int, id uint16, light uint8) {
	i, ok := cv.layerIndex(col, y)
	if !ok {
		return
	}
	cv.Block[i] = id
	cv.Light[i] = light
}

// ---------------------------------------------------------------------------
// Volume: a rectangular window assembled from chunk voxel slabs
// ---------------------------------------------------------------------------

// Volume is a rectangular block-space window of voxel data, assembled from
// one or more ChunkVoxels. Unlike Surface.Blit, it copies nothing: it indexes
// the cached *ChunkVoxels directly, so the whole window costs one slice of
// pointers regardless of how many blocks it spans (ISO_VOXEL_PLAN.md §3.2).
// Do not add a Blit-style copy here -- that is the 20 MB-per-tile mistake
// this design exists to avoid.
type Volume struct {
	Bounds mcmath.BlockBounds

	minCX, minCZ, wCX int
	chunks            []*ChunkVoxels // nil entry = ungenerated or not loaded

	MinY, MaxY int
}

// NewVolume allocates an empty volume covering bounds.
func NewVolume(bounds mcmath.BlockBounds, minY, maxY int) *Volume {
	minCX, minCZ, maxCX, maxCZ := bounds.ChunkRange()
	wCX := maxCX - minCX
	hCZ := maxCZ - minCZ
	n := wCX * hCZ
	if n < 0 {
		n = 0
	}
	return &Volume{
		Bounds: bounds,
		minCX:  minCX, minCZ: minCZ, wCX: wCX,
		chunks: make([]*ChunkVoxels, n),
		MinY:   minY, MaxY: maxY,
	}
}

// putChunk installs a chunk's voxel slab at its position, ignoring positions
// outside the volume. Each chunk position maps to a disjoint slot, so
// concurrent putChunk calls for different chunks need no lock (mirrors
// Surface.Blit's reasoning).
func (v *Volume) putChunk(cv *ChunkVoxels) {
	cx, cz := cv.Pos.X-v.minCX, cv.Pos.Z-v.minCZ
	if cx < 0 || cz < 0 || cx >= v.wCX {
		return
	}
	idx := cz*v.wCX + cx
	if idx < 0 || idx >= len(v.chunks) {
		return
	}
	v.chunks[idx] = cv
}

// chunkAt returns the chunk slab covering a world position, or nil if that
// chunk was never loaded into this volume.
func (v *Volume) chunkAt(x, z int) *ChunkVoxels {
	if !v.Bounds.Contains(x, z) {
		return nil
	}
	cx := mcmath.BlockToChunk(x) - v.minCX
	cz := mcmath.BlockToChunk(z) - v.minCZ
	if cx < 0 || cz < 0 || cx >= v.wCX {
		return nil
	}
	idx := cz*v.wCX + cx
	if idx < 0 || idx >= len(v.chunks) {
		return nil
	}
	return v.chunks[idx]
}

// BlockAt returns the block id and light level at a world position, and
// whether that position is known.
//
// ok=false means "outside the stored slab or ungenerated", not "occluded" or
// "air". Callers -- specifically the isometric voxel renderer's occlusion
// predicate -- must treat ok=false as not occluding: guessing "occluded"
// punches holes in the image, while guessing "not occluded" only costs a few
// extra drawn faces that get overpainted. See ISO_VOXEL_PLAN.md §4.3 and §8.
func (v *Volume) BlockAt(x, y, z int) (id uint16, light uint8, ok bool) {
	cv := v.chunkAt(x, z)
	if cv == nil {
		return 0, 0, false
	}
	return cv.At(Index(x, z), y)
}

// TopY returns the topmost non-air Y at a world column, and whether the
// column's chunk is loaded at all. A loaded but genuinely void column (no
// block anywhere) reports TopY=0, ok=true; distinguishing that from
// unexplored terrain is the caller's job via the chunk surface's
// FlagPresent, exactly as ChunkSurface.Height already requires.
func (v *Volume) TopY(x, z int) (int, bool) {
	cv := v.chunkAt(x, z)
	if cv == nil {
		return 0, false
	}
	return int(cv.TopY[Index(x, z)]), true
}

// Depth returns the slab depth of the chunk covering a world column, or 0 if
// that chunk is not loaded.
func (v *Volume) Depth(x, z int) int {
	cv := v.chunkAt(x, z)
	if cv == nil {
		return 0
	}
	return cv.Depth
}

// ---------------------------------------------------------------------------
// VolumeProvider
// ---------------------------------------------------------------------------

// ErrVoxelsUnsupported reports that a provider cannot supply voxel data --
// the demo world, or a real world with the feature disabled in config.
// Callers treat this exactly like ErrChunkAbsent: fall back to the existing
// heightmap iso path rather than failing the tile.
var ErrVoxelsUnsupported = errors.New("voxel data not supported")

// VolumeProvider optionally supplies per-chunk voxel slabs alongside the
// plain Provider interface. mcworld.World implements it; demo.World
// deliberately does not. The isometric renderer takes the existing
// heightmap path whenever the active provider (or Cached wrapping it) does
// not implement this interface, or when the feature is disabled in config.
type VolumeProvider interface {
	// ChunkVoxels returns one chunk's voxel slab. It returns ErrChunkAbsent
	// when the chunk has never been generated, mirroring Provider.ChunkSurface.
	ChunkVoxels(ctx context.Context, dimension string, pos mcmath.ChunkPos) (*ChunkVoxels, error)
}

// AssembleVolume builds a Volume covering bounds by fetching every
// intersecting chunk's voxel slab from a provider, up to
// concurrentChunkFetch at a time. Mirrors Assemble's policy exactly: absent
// and malformed chunks are skipped rather than aborting the window.
func AssembleVolume(
	ctx context.Context,
	p VolumeProvider,
	dimension string,
	bounds mcmath.BlockBounds,
	minY, maxY int,
	onError func(pos mcmath.ChunkPos, err error),
) (*Volume, error) {
	v := NewVolume(bounds, minY, maxY)
	if bounds.Empty() {
		return v, nil
	}
	minCX, minCZ, maxCX, maxCZ := bounds.ChunkRange()

	type chunkErr struct {
		pos mcmath.ChunkPos
		err error
	}
	n := (maxCX - minCX) * (maxCZ - minCZ)
	errsCh := make(chan chunkErr, n)

	sem := make(chan struct{}, concurrentChunkFetch)
	var wg sync.WaitGroup
	var canceled atomic.Bool

fanout:
	for cz := minCZ; cz < maxCZ; cz++ {
		for cx := minCX; cx < maxCX; cx++ {
			if ctx.Err() != nil {
				canceled.Store(true)
				break fanout
			}
			pos := mcmath.ChunkPos{X: cx, Z: cz}
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				cv, err := p.ChunkVoxels(ctx, dimension, pos)
				if err != nil {
					if !errors.Is(err, ErrChunkAbsent) && !errors.Is(err, ErrVoxelsUnsupported) {
						errsCh <- chunkErr{pos, err}
					}
					return
				}
				if cv != nil {
					v.putChunk(cv)
				}
			}()
		}
	}
	wg.Wait()
	close(errsCh)

	if canceled.Load() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	for e := range errsCh {
		if onError != nil {
			onError(e.pos, e.err)
		}
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// Voxel-aware hit testing (ISO_VOXEL_PLAN.md §3, Phase 3)
// ---------------------------------------------------------------------------

// VoxelRayMarch resolves an isometric screen position to the specific real
// voxel whose face is visible there, instead of mcmath.IsoProjection.RayMarch's
// flattened-height-field answer.
//
// It marches the identical slab-by-slab ray RayMarch does -- same reasoning
// for why the ray is a straight diagonal and why stepping by column slab
// visits every crossed column in nearest-first order with no float epsilon,
// see RayMarch's doc comment -- but where RayMarch's height-field model
// assumes a column is solid everywhere at or below its stored height (so a
// single "surf+1 >= yLow" comparison suffices), a real voxel column can have
// gaps. So this additionally tracks the elevation at which the ray entered
// the column currently under examination, and searches for the highest
// occluding voxel within that column's actual elevation span for this slab,
// top-down, so the nearest genuine surface wins.
//
// occludes classifies a block id as opaque; callers pass something backed by
// blocks.Registry (Block.Occludes) so this package stays decoupled from the
// blocks registry itself.
func VoxelRayMarch(p mcmath.IsoProjection, u, v float64, minY, maxY int, vol *Volume, occludes func(id uint16) bool) (x, y, z int, ok bool) {
	if maxY < minY || vol == nil {
		return 0, 0, 0, false
	}
	du := u / mcmath.IsoHalfWidth / 2

	sHi := v + float64(maxY+1)*mcmath.IsoBlockHeight/(2*mcmath.IsoHalfHeight)
	sLo := v + float64(minY)*mcmath.IsoBlockHeight/(2*mcmath.IsoHalfHeight)

	a := int(math.Floor(sHi + du))
	b := int(math.Floor(sHi - du))

	// prevYHigh is the elevation at which the ray enters the column about to
	// be examined: the world ceiling for the first column, and the previous
	// column's exit point after that.
	prevYHigh := float64(maxY + 1)

	maxSteps := 4*(maxY-minY) + 16
	for i := 0; i < maxSteps; i++ {
		lowA := float64(a) - du
		lowB := float64(b) + du
		s := math.Max(lowA, lowB)

		yLow := (s - v) * (2 * mcmath.IsoHalfHeight) / mcmath.IsoBlockHeight
		if yLow < float64(minY) {
			yLow = float64(minY)
		}

		bx, bz := p.Camera.UnrotateInt(a, b)
		hiY := int(math.Ceil(prevYHigh)) - 1
		loY := int(math.Floor(yLow))
		if hiY >= loY {
			if hy, hok := highestOccluding(vol, bx, bz, loY, hiY, occludes); hok {
				return bx, hy, bz, true
			}
		}

		if s <= sLo {
			break // the ray has left the bottom of the world
		}
		prevYHigh = yLow
		if lowA >= lowB {
			a--
		}
		if lowB >= lowA {
			b--
		}
	}
	return 0, 0, 0, false
}

// highestOccluding returns the highest y in [loY,hiY] at which a column's
// stored voxel occludes, and whether any such y exists. A y outside the
// stored slab (Volume.BlockAt reporting ok=false) is skipped, never treated
// as a hit -- the same fail-open rule the renderer's own occlusion check
// uses, and for the same reason: this window is deliberately tightened
// (ISO_VOXEL_PLAN.md §4.5) and may simply not reach this deep for this
// column, which is not the same thing as "definitely empty here".
func highestOccluding(vol *Volume, x, z, loY, hiY int, occludes func(id uint16) bool) (int, bool) {
	for y := hiY; y >= loY; y-- {
		id, _, ok := vol.BlockAt(x, y, z)
		if !ok || id == 0 {
			continue
		}
		if occludes(id) {
			return y, true
		}
	}
	return 0, false
}
