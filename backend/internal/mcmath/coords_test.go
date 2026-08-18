package mcmath

import (
	"math"
	"testing"
)

func TestFloorDivNeverTruncatesTowardZero(t *testing.T) {
	cases := []struct{ a, b, want int }{
		{0, 16, 0}, {1, 16, 0}, {15, 16, 0}, {16, 16, 1}, {17, 16, 1},
		{-1, 16, -1}, {-15, 16, -1}, {-16, 16, -1}, {-17, 16, -2}, {-32, 16, -2}, {-33, 16, -3},
		{5, 2, 2}, {-5, 2, -3},
	}
	for _, c := range cases {
		if got := FloorDiv(c.a, c.b); got != c.want {
			t.Errorf("FloorDiv(%d,%d) = %d, want %d", c.a, c.b, got, c.want)
		}
		// Go's built-in division must differ for exactly the negative
		// non-multiple cases; this guards against a "simplification" back to /.
		if c.a < 0 && c.a%c.b != 0 && c.a/c.b == c.want {
			t.Errorf("test case %d/%d does not actually exercise floor semantics", c.a, c.b)
		}
	}
}

func TestFloorDivModIdentity(t *testing.T) {
	for a := -80; a <= 80; a++ {
		for _, b := range []int{2, 3, 16, 32, 512} {
			q, m := FloorDiv(a, b), FloorMod(a, b)
			if q*b+m != a {
				t.Fatalf("identity broken: a=%d b=%d q=%d m=%d", a, b, q, m)
			}
			if m < 0 || m >= b {
				t.Fatalf("FloorMod(%d,%d)=%d out of range [0,%d)", a, b, m, b)
			}
		}
	}
}

// The exact cases named in the specification.
func TestBlockToChunkSpecCases(t *testing.T) {
	cases := []struct{ block, chunk int }{
		{0, 0}, {1, 0}, {15, 0}, {16, 1}, {17, 1}, {31, 1}, {32, 2},
		{-1, -1}, {-16, -1}, {-17, -2},
	}
	for _, c := range cases {
		if got := BlockToChunk(c.block); got != c.chunk {
			t.Errorf("BlockToChunk(%d) = %d, want %d", c.block, got, c.chunk)
		}
	}
}

func TestBlockToChunkOffsetAlwaysInRange(t *testing.T) {
	for b := -2000; b <= 2000; b++ {
		off := BlockToChunkOffset(b)
		if off < 0 || off > 15 {
			t.Fatalf("BlockToChunkOffset(%d) = %d out of range", b, off)
		}
		if ChunkToBlock(BlockToChunk(b))+off != b {
			t.Fatalf("chunk decomposition of %d does not reassemble", b)
		}
	}
}

func TestChunkToRegion(t *testing.T) {
	cases := []struct{ chunk, region int }{
		{0, 0}, {31, 0}, {32, 1}, {63, 1}, {64, 2},
		{-1, -1}, {-32, -1}, {-33, -2},
	}
	for _, c := range cases {
		if got := ChunkToRegion(c.chunk); got != c.region {
			t.Errorf("ChunkToRegion(%d) = %d, want %d", c.chunk, got, c.region)
		}
	}
}

func TestBlockToRegionCrossesOriginCleanly(t *testing.T) {
	// Block -1 is in chunk -1, which is in region -1.
	if got := BlockToRegion(-1); got != -1 {
		t.Errorf("BlockToRegion(-1) = %d, want -1", got)
	}
	if got := BlockToRegion(-512); got != -1 {
		t.Errorf("BlockToRegion(-512) = %d, want -1", got)
	}
	if got := BlockToRegion(-513); got != -2 {
		t.Errorf("BlockToRegion(-513) = %d, want -2", got)
	}
	if got := BlockToRegion(511); got != 0 {
		t.Errorf("BlockToRegion(511) = %d, want 0", got)
	}
}

// Minecraft -> map -> Minecraft must return the original coordinate.
func TestMapRoundTripIsExact(t *testing.T) {
	inputs := [][2]float64{
		{0, 0}, {1532, -4290}, {-1, -1}, {-8462, 1254}, {30000000, -30000000},
		{0.5, -0.5}, {1254.75, -8462.25},
	}
	for _, in := range inputs {
		mx, my := BlockToMap(in[0], in[1])
		bx, bz := MapToBlock(mx, my)
		if bx != in[0] || bz != in[1] {
			t.Errorf("round trip of (%v,%v) gave (%v,%v)", in[0], in[1], bx, bz)
		}
	}
}

func TestMapYIsNegatedZ(t *testing.T) {
	// The single most important sign convention in the system.
	mx, my := BlockToMap(1532, -4290)
	if mx != 1532 || my != 4290 {
		t.Errorf("BlockToMap(1532,-4290) = (%v,%v), want (1532,4290)", mx, my)
	}
}

func TestZoomResolutions(t *testing.T) {
	want := map[int]float64{
		0: 64, 1: 32, 2: 16, 3: 8, 4: 4, 5: 2, 6: 1,
		7: 0.5, 8: 0.25, 9: 0.125, 10: 0.0625,
	}
	for z, bpp := range want {
		if got := BlocksPerPixel(z); got != bpp {
			t.Errorf("BlocksPerPixel(%d) = %v, want %v", z, got, bpp)
		}
		if got := PixelsPerBlock(z); math.Abs(got-1/bpp) > 1e-12 {
			t.Errorf("PixelsPerBlock(%d) = %v, want %v", z, got, 1/bpp)
		}
	}
}

func TestTileSpans(t *testing.T) {
	cases := []struct{ zoom, blocks int }{
		{0, 32768}, {1, 16384}, {2, 8192}, {3, 4096}, {4, 2048},
		{5, 1024}, {6, 512}, {7, 256}, {8, 128}, {9, 64}, {10, 32},
	}
	for _, c := range cases {
		if got := TileSpanBlocks(c.zoom); got != c.blocks {
			t.Errorf("TileSpanBlocks(%d) = %d, want %d", c.zoom, got, c.blocks)
		}
		if got := TileSpanBlocksF(c.zoom); got != float64(c.blocks) {
			t.Errorf("TileSpanBlocksF(%d) = %v, want %d", c.zoom, got, c.blocks)
		}
	}
	// Zoom 10 must be exactly 2x2 chunks, per spec.
	if got := TileSpanChunks(10); got != 2 {
		t.Errorf("TileSpanChunks(10) = %d, want 2", got)
	}
	if got := TileSpanChunks(6); got != 32 {
		t.Errorf("TileSpanChunks(6) = %d, want 32", got)
	}
}

// Blocks at the boundary values named in the specification.
func TestBlockToTileSpecCases(t *testing.T) {
	// Zoom 10: 32-block tiles.
	cases := []struct {
		block, tile int
	}{
		{0, 0}, {15, 0}, {16, 0}, {31, 0}, {32, 1},
		{-1, -1}, {-16, -1}, {-17, -1}, {-32, -1}, {-33, -2},
	}
	for _, c := range cases {
		got := BlockToTile(c.block, c.block, 10)
		if got.X != c.tile || got.Y != c.tile {
			t.Errorf("BlockToTile(%d,%d,10) = (%d,%d), want (%d,%d)",
				c.block, c.block, got.X, got.Y, c.tile, c.tile)
		}
	}
	// Zoom 6: 512-block tiles. A world crossing the origin must not glitch.
	if got := BlockToTile(-1, -1, 6); got.X != -1 || got.Y != -1 {
		t.Errorf("BlockToTile(-1,-1,6) = %+v, want X=-1 Y=-1", got)
	}
	if got := BlockToTile(0, 0, 6); got.X != 0 || got.Y != 0 {
		t.Errorf("BlockToTile(0,0,6) = %+v, want X=0 Y=0", got)
	}
	if got := BlockToTile(-512, 511, 6); got.X != -1 || got.Y != 0 {
		t.Errorf("BlockToTile(-512,511,6) = %+v, want X=-1 Y=0", got)
	}
	if got := BlockToTile(-513, 512, 6); got.X != -2 || got.Y != 1 {
		t.Errorf("BlockToTile(-513,512,6) = %+v, want X=-2 Y=1", got)
	}
}

func TestTileYIndexesMinecraftZDirectly(t *testing.T) {
	// Increasing Z (southward) must increase tile Y, matching the downward row
	// order that raster tile grids use.
	north := BlockToTile(0, -600, 6)
	south := BlockToTile(0, 600, 6)
	if !(north.Y < south.Y) {
		t.Errorf("tile Y must grow with Z: north=%d south=%d", north.Y, south.Y)
	}
}

func TestTileBoundsRoundTrip(t *testing.T) {
	for _, zoom := range []int{0, 3, 6, 9, 10} {
		for _, blk := range []int{0, 15, 16, 31, 32, -1, -16, -17, 1532, -4290} {
			tp := BlockToTile(blk, blk, zoom)
			b := tp.Bounds()
			if !b.Contains(blk, blk) {
				t.Errorf("zoom %d: tile %+v bounds %+v does not contain block %d", zoom, tp, b, blk)
			}
			if b.Width() != TileSpanBlocks(zoom) || b.Height() != TileSpanBlocks(zoom) {
				t.Errorf("zoom %d: tile bounds %+v wrong size", zoom, b)
			}
			// Every block in the tile must map back to the same tile.
			for _, probe := range []int{b.MinX, b.MaxX - 1} {
				if got := BlockToTile(probe, probe, zoom); got != tp {
					t.Errorf("zoom %d: block %d in %+v mapped to %+v", zoom, probe, tp, got)
				}
			}
			// One block outside must not.
			if got := BlockToTile(b.MinX-1, b.MinZ, zoom); got == tp {
				t.Errorf("zoom %d: block just west of %+v still maps to it", zoom, tp)
			}
		}
	}
}

func TestParentChildConsistency(t *testing.T) {
	for _, tp := range []TilePos{
		{10, 0, 0}, {10, 1, 1}, {10, -1, -1}, {10, -2, 3}, {7, -13, 44}, {6, 12, 44},
	} {
		p := tp.Parent()
		if p.Zoom != tp.Zoom-1 {
			t.Fatalf("parent of %+v has wrong zoom", tp)
		}
		// The parent must cover the child geometrically.
		cb, pb := tp.Bounds(), p.Bounds()
		if cb.Intersect(pb) != cb {
			t.Errorf("parent %+v bounds %+v does not contain child %+v bounds %+v", p, pb, tp, cb)
		}
		// The child must be one of the parent's four children.
		found := false
		for _, c := range p.Children() {
			if c == tp {
				found = true
			}
		}
		if !found {
			t.Errorf("%+v is not among children of its parent %+v: %+v", tp, p, p.Children())
		}
		dx, dy := tp.ChildQuadrant()
		if dx < 0 || dx > 1 || dy < 0 || dy > 1 {
			t.Errorf("quadrant of %+v out of range: %d,%d", tp, dx, dy)
		}
		if p.Children()[dy*2+dx] != tp {
			t.Errorf("quadrant %d,%d of %+v does not select %+v", dx, dy, p, tp)
		}
	}
}

func TestChildrenTileNegativeQuadrants(t *testing.T) {
	// Tile -1 must be a child of tile -1 at the level below, in quadrant 1.
	tp := TilePos{Zoom: 10, X: -1, Y: -1}
	p := tp.Parent()
	if p.X != -1 || p.Y != -1 {
		t.Fatalf("Parent of (-1,-1) = (%d,%d), want (-1,-1)", p.X, p.Y)
	}
	dx, dy := tp.ChildQuadrant()
	if dx != 1 || dy != 1 {
		t.Errorf("quadrant of (-1,-1) = (%d,%d), want (1,1)", dx, dy)
	}
}

// Given a chunk update, every affected parent tile must be computed correctly.
func TestAncestorsCoverChangedChunk(t *testing.T) {
	chunk := ChunkPos{X: 52, Z: -31}
	deepest := 10
	tp := ChunkToTile(chunk, deepest)

	cb := ChunkBounds(chunk)
	if !tp.Bounds().Intersects(cb) {
		t.Fatalf("deepest tile %+v does not cover chunk %+v", tp, chunk)
	}

	anc := tp.Ancestors(0)
	if len(anc) != deepest {
		t.Fatalf("Ancestors(0) from zoom %d returned %d entries, want %d", deepest, len(anc), deepest)
	}
	for i, a := range anc {
		wantZoom := deepest - 1 - i
		if a.Zoom != wantZoom {
			t.Errorf("ancestor %d has zoom %d, want %d", i, a.Zoom, wantZoom)
		}
		if !a.Bounds().Intersects(cb) {
			t.Errorf("ancestor %+v (bounds %+v) does not cover chunk bounds %+v", a, a.Bounds(), cb)
		}
	}
	if anc[len(anc)-1].Zoom != 0 {
		t.Errorf("ancestor chain does not reach zoom 0")
	}
}

func TestAncestorsStopsAtMinZoom(t *testing.T) {
	tp := TilePos{Zoom: 4, X: 3, Y: -7}
	if got := tp.Ancestors(4); got != nil {
		t.Errorf("Ancestors at minZoom should be nil, got %+v", got)
	}
	anc := tp.Ancestors(2)
	if len(anc) != 2 || anc[0].Zoom != 3 || anc[1].Zoom != 2 {
		t.Errorf("Ancestors(2) = %+v", anc)
	}
}

func TestChunkRangeIsHalfOpen(t *testing.T) {
	// Bounds landing exactly on a chunk edge must not pull in an extra chunk.
	b := BlockBounds{MinX: 0, MinZ: 0, MaxX: 32, MaxZ: 16}
	minCX, minCZ, maxCX, maxCZ := b.ChunkRange()
	if minCX != 0 || minCZ != 0 || maxCX != 2 || maxCZ != 1 {
		t.Errorf("ChunkRange = %d,%d..%d,%d want 0,0..2,1", minCX, minCZ, maxCX, maxCZ)
	}
	// Negative side.
	b = BlockBounds{MinX: -17, MinZ: -16, MaxX: -16, MaxZ: 0}
	minCX, minCZ, maxCX, maxCZ = b.ChunkRange()
	if minCX != -2 || minCZ != -1 || maxCX != -1 || maxCZ != 0 {
		t.Errorf("ChunkRange = %d,%d..%d,%d want -2,-1..-1,0", minCX, minCZ, maxCX, maxCZ)
	}
}

func TestTilesCovering(t *testing.T) {
	// A single chunk at zoom 10 (32-block tiles) touches exactly one tile.
	b := ChunkBounds(ChunkPos{X: 0, Z: 0})
	tiles := TilesCovering(b, 10)
	if len(tiles) != 1 || tiles[0] != (TilePos{10, 0, 0}) {
		t.Errorf("chunk 0,0 at zoom 10 covers %+v", tiles)
	}
	// A 512-block box at zoom 10 covers 16x16 tiles.
	b = BlockBounds{MinX: 0, MinZ: 0, MaxX: 512, MaxZ: 512}
	if got := len(TilesCovering(b, 10)); got != 256 {
		t.Errorf("512-block box at zoom 10 covers %d tiles, want 256", got)
	}
	// Straddling the origin must produce contiguous negative and positive tiles.
	b = BlockBounds{MinX: -1, MinZ: -1, MaxX: 1, MaxZ: 1}
	tiles = TilesCovering(b, 10)
	if len(tiles) != 4 {
		t.Fatalf("origin-straddling box covers %d tiles, want 4: %+v", len(tiles), tiles)
	}
	seen := map[TilePos]bool{}
	for _, tp := range tiles {
		seen[tp] = true
	}
	for _, want := range []TilePos{{10, -1, -1}, {10, 0, -1}, {10, -1, 0}, {10, 0, 0}} {
		if !seen[want] {
			t.Errorf("missing tile %+v", want)
		}
	}
	if TilesCovering(BlockBounds{}, 6) != nil {
		t.Error("empty bounds should cover no tiles")
	}
}

func TestMassiveViewportFitsInFewTiles(t *testing.T) {
	// A 20,000 x 20,000 block viewport must be servable by a small number of
	// tiles at a low zoom -- this is the headline performance requirement.
	b := BlockBounds{MinX: -10000, MinZ: -10000, MaxX: 10000, MaxZ: 10000}
	if n := len(TilesCovering(b, 2)); n > 16 {
		t.Errorf("20k x 20k viewport needs %d tiles at zoom 2, expected <= 16", n)
	}
	// Centred on the origin the box straddles tiles -1 and 0 on both axes, so
	// four tiles is the correct minimum even though one tile is far larger than
	// the viewport. Offsetting into a single tile's interior must need just one.
	if n := len(TilesCovering(b, 0)); n != 4 {
		t.Errorf("origin-centred 20k viewport needs %d tiles at zoom 0, expected 4", n)
	}
	inside := BlockBounds{MinX: 1000, MinZ: 1000, MaxX: 21000, MaxZ: 21000}
	if n := len(TilesCovering(inside, 0)); n != 1 {
		t.Errorf("20k x 20k viewport inside one zoom-0 tile needs %d tiles, expected 1", n)
	}
}
