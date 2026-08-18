// Package mcmath holds the authoritative coordinate mathematics for the map.
//
// There is no latitude, no longitude and no geographic projection anywhere in
// this system. Minecraft block coordinates are the source of truth. Everything
// else -- map space, tile indices, isometric screen space -- is a deterministic
// and (where meaningful) reversible transform of them.
//
// # Coordinate spaces
//
//	Block space   (mcX, mcY, mcZ)  Minecraft blocks. Y is up. Z is south.
//	Map space     (mapX, mapY)     mapX = mcX, mapY = -mcZ. Y is up (north).
//	Tile space    (tileX, tileY)   tileX indexes mcX, tileY indexes mcZ.
//	Iso space     (isoU, isoV)     2:1 dimetric projection, V grows downward.
//
// Map space exists because renderers and mapping libraries conventionally use a
// Y-up cartesian plane. Minecraft's Z axis points south, so mapY = -mcZ makes
// "up on screen" mean "north in the world".
//
// Tile space deliberately does NOT use map space. Tile grids conventionally
// number rows downward from the top-left, which is exactly what mcZ already
// does (increasing Z = southward = downward on screen). So tile indices come
// straight from mcX and mcZ with no negation, which removes an entire class of
// off-by-one and sign errors around the Z=0 axis.
package mcmath

import "math"

const (
	// ChunkSize is the edge length of a Minecraft chunk in blocks.
	ChunkSize = 16
	// RegionChunks is the edge length of an Anvil region file in chunks.
	RegionChunks = 32
	// RegionBlocks is the edge length of an Anvil region file in blocks.
	RegionBlocks = ChunkSize * RegionChunks // 512
	// TileSize is the pixel edge length of every raster tile in the pyramid.
	TileSize = 512
	// BaseZoom is the pyramid level at which one pixel equals exactly one
	// Minecraft block. Levels below are zoomed out, levels above zoomed in.
	BaseZoom = 6
	// MaxIntegerZoom is the highest zoom whose tile span in blocks is still a
	// positive integer: TileSize * 2^(BaseZoom-z) >= 1.
	MaxIntegerZoom = BaseZoom + 9 // 15
)

// FloorDiv performs Euclidean floor division, rounding toward negative
// infinity. Go's built-in / truncates toward zero, which is wrong for world
// coordinates: -1/16 == 0 but block -1 belongs to chunk -1.
func FloorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// FloorMod returns the non-negative remainder consistent with FloorDiv, so
// that FloorDiv(a,b)*b + FloorMod(a,b) == a for any sign of a.
func FloorMod(a, b int) int {
	m := a % b
	if m != 0 && (m < 0) != (b < 0) {
		m += b
	}
	return m
}

// FloorDivF is FloorDiv for a float numerator, used when converting continuous
// map/mouse positions into discrete indices.
func FloorDivF(a float64, b float64) int {
	return int(math.Floor(a / b))
}

// ---------------------------------------------------------------------------
// Block <-> chunk <-> region
// ---------------------------------------------------------------------------

// BlockToChunk converts a block coordinate on one axis to its chunk
// coordinate. Correct for negatives: -1 -> -1, -16 -> -1, -17 -> -2.
func BlockToChunk(block int) int { return FloorDiv(block, ChunkSize) }

// BlockToChunkOffset returns the 0..15 offset of a block within its chunk.
func BlockToChunkOffset(block int) int { return FloorMod(block, ChunkSize) }

// ChunkToBlock returns the minimum block coordinate covered by a chunk.
func ChunkToBlock(chunk int) int { return chunk * ChunkSize }

// ChunkToRegion converts a chunk coordinate to its region-file coordinate.
func ChunkToRegion(chunk int) int { return FloorDiv(chunk, RegionChunks) }

// ChunkToRegionOffset returns the 0..31 offset of a chunk within its region.
func ChunkToRegionOffset(chunk int) int { return FloorMod(chunk, RegionChunks) }

// RegionToChunk returns the minimum chunk coordinate covered by a region.
func RegionToChunk(region int) int { return region * RegionChunks }

// BlockToRegion converts a block coordinate directly to its region coordinate.
func BlockToRegion(block int) int { return ChunkToRegion(BlockToChunk(block)) }

// ChunkPos identifies a chunk column in a dimension.
type ChunkPos struct {
	X, Z int
}

// Region returns the region file coordinates containing this chunk.
func (c ChunkPos) Region() RegionPos {
	return RegionPos{X: ChunkToRegion(c.X), Z: ChunkToRegion(c.Z)}
}

// MinBlockX / MinBlockZ give the north-west block corner of the chunk.
func (c ChunkPos) MinBlockX() int { return ChunkToBlock(c.X) }
func (c ChunkPos) MinBlockZ() int { return ChunkToBlock(c.Z) }

// RegionPos identifies an Anvil region file.
type RegionPos struct {
	X, Z int
}

// MinChunkX / MinChunkZ give the north-west chunk corner of the region.
func (r RegionPos) MinChunkX() int { return RegionToChunk(r.X) }
func (r RegionPos) MinChunkZ() int { return RegionToChunk(r.Z) }

// ---------------------------------------------------------------------------
// Block <-> map space
// ---------------------------------------------------------------------------

// BlockToMap converts Minecraft X/Z into map-space X/Y.
func BlockToMap(mcX, mcZ float64) (mapX, mapY float64) { return mcX, -mcZ }

// MapToBlock converts map-space X/Y back into Minecraft X/Z. This is the exact
// inverse of BlockToMap.
func MapToBlock(mapX, mapY float64) (mcX, mcZ float64) { return mapX, -mapY }

// ---------------------------------------------------------------------------
// Zoom / resolution
// ---------------------------------------------------------------------------

// BlocksPerPixel is the conceptual resolution of a pyramid level: how many
// Minecraft blocks one tile pixel covers. Zoom 0 = 64, zoom 6 = 1,
// zoom 10 = 1/16 (i.e. 16 pixels per block).
func BlocksPerPixel(zoom int) float64 { return math.Ldexp(1, BaseZoom-zoom) }

// PixelsPerBlock is the reciprocal of BlocksPerPixel, which is the more
// intuitive quantity at high zoom.
func PixelsPerBlock(zoom int) float64 { return math.Ldexp(1, zoom-BaseZoom) }

// TileSpanBlocks is the edge length in Minecraft blocks of one tile at a given
// zoom: 32768 at zoom 0, 512 at zoom 6, 32 at zoom 10.
func TileSpanBlocks(zoom int) int {
	if zoom > MaxIntegerZoom {
		return 1
	}
	shift := MaxIntegerZoom - zoom
	return 1 << shift
}

// TileSpanBlocksF is TileSpanBlocks without the integer clamp, for zoom levels
// beyond MaxIntegerZoom where a tile covers a fraction of a block.
func TileSpanBlocksF(zoom int) float64 {
	return TileSize * BlocksPerPixel(zoom)
}

// TileSpanChunks is the edge length in chunks of one tile at a given zoom.
// Returns 0 when a tile is smaller than a chunk (zoom > 10).
func TileSpanChunks(zoom int) int { return TileSpanBlocks(zoom) / ChunkSize }

// ---------------------------------------------------------------------------
// Tile addressing
// ---------------------------------------------------------------------------

// TilePos identifies one raster tile within one pyramid level.
type TilePos struct {
	Zoom int
	X    int
	Y    int
}

// BlockToTile maps a Minecraft X/Z block position to the tile containing it.
// tileX indexes mcX and tileY indexes mcZ, both via floor division, so the
// grid is continuous and correct across X=0 and Z=0.
func BlockToTile(mcX, mcZ, zoom int) TilePos {
	span := TileSpanBlocks(zoom)
	return TilePos{Zoom: zoom, X: FloorDiv(mcX, span), Y: FloorDiv(mcZ, span)}
}

// ChunkToTile maps a chunk to the tile containing it at the given zoom.
func ChunkToTile(c ChunkPos, zoom int) TilePos {
	return BlockToTile(c.MinBlockX(), c.MinBlockZ(), zoom)
}

// Bounds returns the half-open Minecraft block extent [MinX,MaxX) x [MinZ,MaxZ)
// covered by the tile.
func (t TilePos) Bounds() BlockBounds {
	span := TileSpanBlocks(t.Zoom)
	return BlockBounds{
		MinX: t.X * span,
		MinZ: t.Y * span,
		MaxX: t.X*span + span,
		MaxZ: t.Y*span + span,
	}
}

// Parent returns the tile one pyramid level lower (zoomed out) that contains
// this tile. Each parent covers exactly 4 children.
func (t TilePos) Parent() TilePos {
	return TilePos{Zoom: t.Zoom - 1, X: FloorDiv(t.X, 2), Y: FloorDiv(t.Y, 2)}
}

// ChildQuadrant returns which quadrant of its parent this tile occupies, as
// (dx, dy) each 0 or 1. Correct for negative tile indices.
func (t TilePos) ChildQuadrant() (dx, dy int) {
	return FloorMod(t.X, 2), FloorMod(t.Y, 2)
}

// Children returns the four tiles one level up (zoomed in) covering this tile.
func (t TilePos) Children() [4]TilePos {
	z := t.Zoom + 1
	bx, by := t.X*2, t.Y*2
	return [4]TilePos{
		{z, bx, by}, {z, bx + 1, by},
		{z, bx, by + 1}, {z, bx + 1, by + 1},
	}
}

// Ancestors returns every tile from Zoom-1 down to minZoom that transitively
// contains this tile, ordered from nearest parent outward. This drives pyramid
// regeneration after a chunk changes.
func (t TilePos) Ancestors(minZoom int) []TilePos {
	if t.Zoom <= minZoom {
		return nil
	}
	out := make([]TilePos, 0, t.Zoom-minZoom)
	cur := t
	for cur.Zoom > minZoom {
		cur = cur.Parent()
		out = append(out, cur)
	}
	return out
}

// ---------------------------------------------------------------------------
// Block bounds
// ---------------------------------------------------------------------------

// BlockBounds is a half-open rectangle in Minecraft block space on the X/Z
// plane: X in [MinX, MaxX), Z in [MinZ, MaxZ).
type BlockBounds struct {
	MinX, MinZ, MaxX, MaxZ int
}

// Width and Height in blocks.
func (b BlockBounds) Width() int  { return b.MaxX - b.MinX }
func (b BlockBounds) Height() int { return b.MaxZ - b.MinZ }

// Empty reports whether the rectangle covers no blocks.
func (b BlockBounds) Empty() bool { return b.MaxX <= b.MinX || b.MaxZ <= b.MinZ }

// Contains reports whether a block position falls inside the bounds.
func (b BlockBounds) Contains(x, z int) bool {
	return x >= b.MinX && x < b.MaxX && z >= b.MinZ && z < b.MaxZ
}

// Intersects reports whether two rectangles overlap.
func (b BlockBounds) Intersects(o BlockBounds) bool {
	return b.MinX < o.MaxX && o.MinX < b.MaxX && b.MinZ < o.MaxZ && o.MinZ < b.MaxZ
}

// Intersect returns the overlapping rectangle, which may be empty.
func (b BlockBounds) Intersect(o BlockBounds) BlockBounds {
	return BlockBounds{
		MinX: max(b.MinX, o.MinX), MinZ: max(b.MinZ, o.MinZ),
		MaxX: min(b.MaxX, o.MaxX), MaxZ: min(b.MaxZ, o.MaxZ),
	}
}

// Expand grows the rectangle by n blocks on every side.
func (b BlockBounds) Expand(n int) BlockBounds {
	return BlockBounds{MinX: b.MinX - n, MinZ: b.MinZ - n, MaxX: b.MaxX + n, MaxZ: b.MaxZ + n}
}

// ChunkRange returns the half-open chunk rectangle covering these bounds. The
// max values are exclusive, computed so that a bound landing exactly on a chunk
// edge does not pull in an extra chunk.
func (b BlockBounds) ChunkRange() (minCX, minCZ, maxCX, maxCZ int) {
	if b.Empty() {
		return 0, 0, 0, 0
	}
	minCX = BlockToChunk(b.MinX)
	minCZ = BlockToChunk(b.MinZ)
	maxCX = BlockToChunk(b.MaxX-1) + 1
	maxCZ = BlockToChunk(b.MaxZ-1) + 1
	return
}

// ChunkBounds returns the block bounds of a chunk column.
func ChunkBounds(c ChunkPos) BlockBounds {
	return BlockBounds{
		MinX: c.MinBlockX(), MinZ: c.MinBlockZ(),
		MaxX: c.MinBlockX() + ChunkSize, MaxZ: c.MinBlockZ() + ChunkSize,
	}
}

// TilesCovering returns every tile at the given zoom that intersects bounds.
func TilesCovering(b BlockBounds, zoom int) []TilePos {
	if b.Empty() {
		return nil
	}
	span := TileSpanBlocks(zoom)
	minTX := FloorDiv(b.MinX, span)
	minTY := FloorDiv(b.MinZ, span)
	maxTX := FloorDiv(b.MaxX-1, span)
	maxTY := FloorDiv(b.MaxZ-1, span)
	out := make([]TilePos, 0, (maxTX-minTX+1)*(maxTY-minTY+1))
	for ty := minTY; ty <= maxTY; ty++ {
		for tx := minTX; tx <= maxTX; tx++ {
			out = append(out, TilePos{Zoom: zoom, X: tx, Y: ty})
		}
	}
	return out
}
