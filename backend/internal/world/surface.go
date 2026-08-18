// Package world defines the render-ready view of a Minecraft world.
//
// Renderers never see NBT, region files or block states. They see a Surface:
// a compact grid describing, for each X/Z column, which block is visible from
// above, how high it is, what biome it is in and how deep any water above it
// is. Anything that can produce that grid -- an Anvil world, a demo terrain
// generator, a future Bedrock reader -- can drive the entire tile pipeline
// without the renderers knowing the difference.
//
// The representation is deliberately struct-of-arrays and fixed-width. One
// chunk's surface is a little over 2 KB, so a bounded cache can hold hundreds
// of thousands of chunks in RAM while the world on disk grows to hundreds of
// gigabytes.
package world

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
)

// Column flag bits.
const (
	// FlagPresent marks a column that has real world data. Columns without it
	// are unexplored and render as the neutral unexplored background.
	FlagPresent uint8 = 1 << iota
	// FlagWater marks a column whose visible surface is under water.
	FlagWater
	// FlagFoliage marks leaves and plants, used by the terrain style.
	FlagFoliage
	// FlagFloating marks a column whose surface sits above a gap, which the
	// height shader treats as a cliff edge.
	FlagFloating
)

// ColumnCount is the number of columns in one chunk surface.
const ColumnCount = mcmath.ChunkSize * mcmath.ChunkSize

// ChunkSurface is the render-ready summary of one 16x16 chunk column.
//
// Struct-of-arrays keeps each field densely packed, which matters because the
// renderers sweep one field at a time across a whole tile.
type ChunkSurface struct {
	Pos mcmath.ChunkPos

	// Height is the Y of the topmost rendered block in each column. For a
	// submerged column this is the sea floor, not the water top.
	Height [ColumnCount]int16
	// WaterY is the Y of the water surface where FlagWater is set.
	WaterY [ColumnCount]int16
	// Block is the registry index of the visible block.
	Block [ColumnCount]uint16
	// Decoration is the registry index of a thin plant (grass, flower,
	// sapling, crop) sitting on top of Block, or 0 for none. It never
	// changes the column's height or side faces -- only the top face gets
	// its texture (or colour) composited over Block's.
	Decoration [ColumnCount]uint16
	// Biome is the biome-table index at the surface.
	Biome [ColumnCount]uint16
	// Light is the stored block/sky light at the surface, 0..15. Worlds without
	// light data report 15.
	Light [ColumnCount]uint8
	// Flags carries the FlagPresent family.
	Flags [ColumnCount]uint8
}

// Index returns the array offset for a block position inside this chunk.
// Accepts world coordinates and reduces them via floor-mod, so callers never
// have to get negative-coordinate arithmetic right themselves.
func Index(x, z int) int {
	return mcmath.FloorMod(z, mcmath.ChunkSize)*mcmath.ChunkSize + mcmath.FloorMod(x, mcmath.ChunkSize)
}

// At returns the column data at a world block position within this chunk.
func (c *ChunkSurface) At(x, z int) Column {
	i := Index(x, z)
	return Column{
		Height:     int(c.Height[i]),
		WaterY:     int(c.WaterY[i]),
		Block:      c.Block[i],
		Decoration: c.Decoration[i],
		Biome:      c.Biome[i],
		Light:      c.Light[i],
		Flags:      c.Flags[i],
	}
}

// Present reports whether a column inside this chunk has world data.
func (c *ChunkSurface) Present(x, z int) bool {
	return c.Flags[Index(x, z)]&FlagPresent != 0
}

// Column is the unpacked view of a single X/Z column.
type Column struct {
	Height     int
	WaterY     int
	Block      uint16
	Decoration uint16
	Biome      uint16
	Light      uint8
	Flags      uint8
}

// Present reports whether the column has world data.
func (c Column) Present() bool { return c.Flags&FlagPresent != 0 }

// Water reports whether the column's surface is submerged.
func (c Column) Water() bool { return c.Flags&FlagWater != 0 }

// WaterDepth returns how many blocks of water sit above the surface.
func (c Column) WaterDepth() int {
	if !c.Water() {
		return 0
	}
	d := c.WaterY - c.Height
	if d < 0 {
		return 0
	}
	return d
}

// RenderY is the elevation the isometric renderer should draw the column at:
// the water surface for submerged columns, the solid surface otherwise.
func (c Column) RenderY() int {
	if c.Water() {
		return c.WaterY
	}
	return c.Height
}

// ---------------------------------------------------------------------------
// Surface: a rectangular window assembled from chunk surfaces
// ---------------------------------------------------------------------------

// Surface is a rectangular block-space window of columns, assembled from one or
// more ChunkSurfaces. Renderers work exclusively against this type.
//
// Columns outside any generated chunk are simply absent: reads return a zero
// Column with FlagPresent clear. There is no error path for unexplored terrain,
// because a half-generated world is the normal case, not a failure.
type Surface struct {
	Bounds mcmath.BlockBounds

	height     []int16
	waterY     []int16
	block      []uint16
	decoration []uint16
	biome      []uint16
	light      []uint8
	flags      []uint8

	// MinY and MaxY carry the dimension's height range so renderers and the
	// hit tester know the ray-march extent.
	MinY, MaxY int
}

// NewSurface allocates an empty surface covering bounds.
func NewSurface(b mcmath.BlockBounds, minY, maxY int) *Surface {
	n := b.Width() * b.Height()
	if n < 0 {
		n = 0
	}
	return &Surface{
		Bounds:     b,
		height:     make([]int16, n),
		waterY:     make([]int16, n),
		block:      make([]uint16, n),
		decoration: make([]uint16, n),
		biome:      make([]uint16, n),
		light:      make([]uint8, n),
		flags:      make([]uint8, n),
		MinY:       minY,
		MaxY:       maxY,
	}
}

// index returns the flat offset of a world position, or -1 if outside bounds.
func (s *Surface) index(x, z int) int {
	if !s.Bounds.Contains(x, z) {
		return -1
	}
	return (z-s.Bounds.MinZ)*s.Bounds.Width() + (x - s.Bounds.MinX)
}

// At returns the column at a world position. Positions outside the surface, and
// columns in ungenerated chunks, return a zero Column whose Present is false.
func (s *Surface) At(x, z int) Column {
	i := s.index(x, z)
	if i < 0 {
		return Column{}
	}
	return Column{
		Height:     int(s.height[i]),
		WaterY:     int(s.waterY[i]),
		Block:      s.block[i],
		Decoration: s.decoration[i],
		Biome:      s.biome[i],
		Light:      s.light[i],
		Flags:      s.flags[i],
	}
}

// Present reports whether a world position has data.
func (s *Surface) Present(x, z int) bool {
	i := s.index(x, z)
	return i >= 0 && s.flags[i]&FlagPresent != 0
}

// SampleHeight implements mcmath.HeightSampler so a Surface can be ray-marched
// directly for isometric hit testing. Submerged columns report the water
// surface, because that is the pixel the user actually sees and clicks.
func (s *Surface) SampleHeight(x, z int) (int, bool) {
	i := s.index(x, z)
	if i < 0 || s.flags[i]&FlagPresent == 0 {
		return 0, false
	}
	if s.flags[i]&FlagWater != 0 {
		return int(s.waterY[i]), true
	}
	return int(s.height[i]), true
}

// Set writes one column, ignoring positions outside the surface.
func (s *Surface) Set(x, z int, c Column) {
	i := s.index(x, z)
	if i < 0 {
		return
	}
	s.height[i] = int16(c.Height)
	s.waterY[i] = int16(c.WaterY)
	s.block[i] = c.Block
	s.decoration[i] = c.Decoration
	s.biome[i] = c.Biome
	s.light[i] = c.Light
	s.flags[i] = c.Flags
}

// Blit copies a chunk surface into this surface, clipped to the overlap. This
// is how a tile's working window is assembled from cached chunks.
func (s *Surface) Blit(cs *ChunkSurface) {
	cb := mcmath.ChunkBounds(cs.Pos)
	ov := cb.Intersect(s.Bounds)
	if ov.Empty() {
		return
	}
	w := s.Bounds.Width()
	for z := ov.MinZ; z < ov.MaxZ; z++ {
		dstRow := (z-s.Bounds.MinZ)*w - s.Bounds.MinX
		srcRow := mcmath.FloorMod(z, mcmath.ChunkSize) * mcmath.ChunkSize
		for x := ov.MinX; x < ov.MaxX; x++ {
			d := dstRow + x
			sIdx := srcRow + mcmath.FloorMod(x, mcmath.ChunkSize)
			s.height[d] = cs.Height[sIdx]
			s.waterY[d] = cs.WaterY[sIdx]
			s.block[d] = cs.Block[sIdx]
			s.decoration[d] = cs.Decoration[sIdx]
			s.biome[d] = cs.Biome[sIdx]
			s.light[d] = cs.Light[sIdx]
			s.flags[d] = cs.Flags[sIdx]
		}
	}
}

// AnyPresent reports whether the surface holds any generated column at all.
// Tile generation uses this to emit a cheap "unexplored" tile without running
// the full renderer.
func (s *Surface) AnyPresent() bool {
	for _, f := range s.flags {
		if f&FlagPresent != 0 {
			return true
		}
	}
	return false
}

// HeightRange returns the lowest and highest rendered elevation present in the
// surface, used to auto-scale the height render style.
func (s *Surface) HeightRange() (lo, hi int, ok bool) {
	lo, hi = 1<<30, -(1 << 30)
	for i, f := range s.flags {
		if f&FlagPresent == 0 {
			continue
		}
		h := int(s.height[i])
		if f&FlagWater != 0 {
			h = int(s.waterY[i])
		}
		if h < lo {
			lo = h
		}
		if h > hi {
			hi = h
		}
		ok = true
	}
	if !ok {
		return 0, 0, false
	}
	return lo, hi, true
}

// ---------------------------------------------------------------------------
// Providers
// ---------------------------------------------------------------------------

// ErrChunkAbsent reports that a chunk has never been generated. It is an
// expected condition, not a failure, and callers render unexplored terrain.
var ErrChunkAbsent = errors.New("chunk not generated")

// DimensionInfo describes one dimension of one world.
//
// Nothing here is vanilla-specific: ID is an arbitrary namespaced identifier,
// and the height range is per-dimension so a modpack's 512-block-tall mining
// dimension coexists with a 384-block overworld.
type DimensionInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	MinY        int    `json:"minY"`
	MaxY        int    `json:"maxY"`
	WorldBorder int    `json:"worldBorder,omitempty"`
	CenterX     int    `json:"centerX"`
	CenterZ     int    `json:"centerZ"`
	Enabled     bool   `json:"enabled"`
	// SpawnX/SpawnZ position the default camera and the spawn marker.
	SpawnX int `json:"spawnX"`
	SpawnZ int `json:"spawnZ"`
	// Path is the on-disk directory holding this dimension's region files. It
	// is never exposed through the HTTP API.
	Path string `json:"-"`
	// HasCeiling marks Nether-like dimensions, where the surface scan must
	// start below the bedrock roof instead of at the build limit.
	HasCeiling bool `json:"hasCeiling"`
}

// Height returns the dimension's total build height.
func (d DimensionInfo) Height() int { return d.MaxY - d.MinY }

// Provider supplies render-ready chunk surfaces for a world.
//
// Implementations must be safe for concurrent use: the tile worker pool calls
// ChunkSurface from many goroutines at once, often for neighbouring chunks that
// live in the same region file.
type Provider interface {
	// Dimensions lists the dimensions this provider can serve.
	Dimensions(ctx context.Context) ([]DimensionInfo, error)

	// Dimension returns metadata for one dimension.
	Dimension(ctx context.Context, id string) (DimensionInfo, bool)

	// ChunkSurface returns one chunk's surface. It returns ErrChunkAbsent when
	// the chunk has never been generated, which callers treat as unexplored
	// rather than as an error.
	ChunkSurface(ctx context.Context, dimension string, pos mcmath.ChunkPos) (*ChunkSurface, error)
}

// concurrentChunkFetch bounds how many chunk fetches Assemble runs at once. A
// base-zoom tile can span on the order of a thousand chunks; fetching them
// one at a time pays the full I/O-plus-decode latency of every one serially
// on a single goroutine, even though Provider implementations are documented
// safe for concurrent use and neighbouring chunks routinely share an already
// open region file.
const concurrentChunkFetch = 16

// Assemble builds a Surface covering bounds by fetching every intersecting
// chunk from a provider, up to concurrentChunkFetch at a time.
//
// Absent chunks and malformed chunks are both skipped rather than aborting the
// window, so one corrupt region file costs a few blank chunks instead of an
// entire tile. onError, if set, is called for genuine failures so they can be
// logged and counted.
func Assemble(
	ctx context.Context,
	p Provider,
	dimension string,
	bounds mcmath.BlockBounds,
	minY, maxY int,
	onError func(pos mcmath.ChunkPos, err error),
) (*Surface, error) {
	s := NewSurface(bounds, minY, maxY)
	if bounds.Empty() {
		return s, nil
	}
	minCX, minCZ, maxCX, maxCZ := bounds.ChunkRange()

	type chunkErr struct {
		pos mcmath.ChunkPos
		err error
	}
	n := (maxCX - minCX) * (maxCZ - minCZ)
	errsCh := make(chan chunkErr, n)

	// Each chunk blits into a disjoint region of s's backing arrays (chunks
	// never overlap in world space), so concurrent Blit calls need no lock.
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
				cs, err := p.ChunkSurface(ctx, dimension, pos)
				if err != nil {
					if !errors.Is(err, ErrChunkAbsent) {
						errsCh <- chunkErr{pos, err}
					}
					return
				}
				if cs != nil {
					s.Blit(cs)
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
	return s, nil
}
