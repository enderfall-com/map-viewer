package mcmath

import (
	"math"
	"testing"
)

const eps = 1e-9

func TestIsoProjectOriginIsZero(t *testing.T) {
	p := NewIsoProjection(CameraSE)
	if u, v := p.Project(0, 0, 0); math.Abs(u) > eps || math.Abs(v) > eps {
		t.Errorf("world origin projects to (%v,%v), want (0,0)", u, v)
	}
}

func TestIsoForwardInverseRoundTrip(t *testing.T) {
	for _, cam := range []IsoCamera{CameraSE, CameraSW, CameraNW, CameraNE} {
		p := NewIsoProjection(cam)
		for _, pt := range [][3]float64{
			{0, 0, 0}, {1, 0, 0}, {0, 0, 1}, {1532, 78, -4290},
			{-1, -64, -1}, {-8462.5, 319, 1254.25}, {12345, 200, 67890},
		} {
			u, v := p.Project(pt[0], pt[1], pt[2])
			x, z := p.Unproject(u, v, pt[1])
			if math.Abs(x-pt[0]) > 1e-6 || math.Abs(z-pt[2]) > 1e-6 {
				t.Errorf("camera %s: round trip of %v gave x=%v z=%v", cam, pt, x, z)
			}
		}
	}
}

func TestIsoTopFaceIsTwoByOneDiamond(t *testing.T) {
	p := NewIsoProjection(CameraSE)
	// Block (0,0,0): top face at elevation 1, corners at the four X/Z corners.
	u0, v0 := p.Project(0, 1, 0) // top vertex
	uE, vE := p.Project(1, 1, 0)
	_, vS := p.Project(1, 1, 1) // bottom vertex
	uW, vW := p.Project(0, 1, 1)

	if math.Abs(u0-0) > eps || math.Abs(v0-(-1)) > eps {
		t.Errorf("top vertex = (%v,%v)", u0, v0)
	}
	// Width: east and west vertices are +/-1 iso unit from centre.
	if math.Abs(uE-1) > eps || math.Abs(uW-(-1)) > eps {
		t.Errorf("side vertices u = %v / %v, want 1 / -1", uE, uW)
	}
	// Height: bottom vertex is exactly 1 iso unit below the top vertex.
	if math.Abs((vS-v0)-IsoDiamondHeight) > eps {
		t.Errorf("diamond height = %v, want %v", vS-v0, IsoDiamondHeight)
	}
	// Side vertices sit exactly halfway down.
	if math.Abs(vE-(v0+0.5)) > eps || math.Abs(vW-(v0+0.5)) > eps {
		t.Errorf("side vertex v = %v / %v, want %v", vE, vW, v0+0.5)
	}
	// 2:1 aspect ratio.
	width := uE - uW
	if math.Abs(width/(vS-v0)-2) > eps {
		t.Errorf("aspect ratio = %v, want 2", width/(vS-v0))
	}
}

func TestIsoBlockTopVertexHelper(t *testing.T) {
	p := NewIsoProjection(CameraSE)
	// The helper must agree with projecting the block's min corner at y+1.
	for _, b := range [][3]int{{0, 0, 0}, {5, 70, -3}, {-17, -64, 128}} {
		hu, hv := p.ProjectBlockTop(b[0], b[1], b[2])
		wu, wv := p.Project(float64(b[0]), float64(b[1]+1), float64(b[2]))
		if math.Abs(hu-wu) > eps || math.Abs(hv-wv) > eps {
			t.Errorf("ProjectBlockTop%v = (%v,%v), want (%v,%v)", b, hu, hv, wu, wv)
		}
	}
}

func TestIsoHigherTerrainDrawsHigherOnScreen(t *testing.T) {
	p := NewIsoProjection(CameraSE)
	_, vLow := p.ProjectBlockTop(100, 64, 100)
	_, vHigh := p.ProjectBlockTop(100, 200, 100)
	if !(vHigh < vLow) {
		t.Errorf("taller terrain must have smaller v (higher on screen): low=%v high=%v", vLow, vHigh)
	}
	// One Y level moves exactly IsoBlockHeight.
	_, v1 := p.ProjectBlockTop(0, 10, 0)
	_, v2 := p.ProjectBlockTop(0, 11, 0)
	if math.Abs((v1-v2)-IsoBlockHeight) > eps {
		t.Errorf("one Y level moved v by %v, want %v", v1-v2, IsoBlockHeight)
	}
}

func TestIsoOneYLevelEqualsOneDiamondHeight(t *testing.T) {
	// A cube must render as a regular hexagon: side face height == diamond height.
	if IsoBlockHeight != IsoDiamondHeight {
		t.Errorf("IsoBlockHeight=%v IsoDiamondHeight=%v; cubes will look wrong",
			IsoBlockHeight, IsoDiamondHeight)
	}
}

func TestIsoDepthKeyOrdersFarToNear(t *testing.T) {
	for _, cam := range []IsoCamera{CameraSE, CameraSW, CameraNW, CameraNE} {
		p := NewIsoProjection(cam)
		// Two columns at the same iso u; the nearer one must have the larger key
		// and must project lower on screen at equal elevation.
		type probe struct{ x, z int }
		probes := []probe{{0, 0}, {1, 1}, {5, 5}, {-3, -3}}
		for i := 1; i < len(probes); i++ {
			a, b := probes[i-1], probes[i]
			ka, kb := cam.DepthKey(a.x, a.z), cam.DepthKey(b.x, b.z)
			_, va := p.ProjectBlockTop(a.x, 64, a.z)
			_, vb := p.ProjectBlockTop(b.x, 64, b.z)
			if (ka < kb) != (va < vb) {
				t.Errorf("camera %s: depth key order disagrees with screen v order: "+
					"keys %d,%d v %v,%v", cam, ka, kb, va, vb)
			}
		}
	}
}

func TestIsoRotateUnrotateAreInverses(t *testing.T) {
	for _, cam := range []IsoCamera{CameraSE, CameraSW, CameraNW, CameraNE} {
		for _, pt := range [][2]int{{0, 0}, {1, 0}, {0, 1}, {1532, -4290}, {-17, -33}} {
			a, b := cam.RotateInt(pt[0], pt[1])
			x, z := cam.UnrotateInt(a, b)
			if x != pt[0] || z != pt[1] {
				t.Errorf("camera %s: rotate/unrotate of %v gave (%d,%d)", cam, pt, x, z)
			}
			fa, fb := cam.Rotate(float64(pt[0]), float64(pt[1]))
			if fa != float64(a) || fb != float64(b) {
				t.Errorf("camera %s: float and int Rotate disagree on %v", cam, pt)
			}
		}
	}
}

func TestIsoCameraParsing(t *testing.T) {
	for name, want := range map[string]IsoCamera{
		"": CameraSE, "se": CameraSE, "sw": CameraSW, "nw": CameraNW, "ne": CameraNE,
	} {
		got, ok := ParseIsoCamera(name)
		if !ok || got != want {
			t.Errorf("ParseIsoCamera(%q) = %v,%v want %v,true", name, got, ok, want)
		}
		if got.String() != want.String() {
			t.Errorf("String round trip failed for %q", name)
		}
	}
	if _, ok := ParseIsoCamera("upside_down"); ok {
		t.Error("unknown camera should report not-ok")
	}
}

// The overscan calculation is what prevents seams and clipped mountains.
func TestIsoWorldFootprintCoversTallTerrain(t *testing.T) {
	p := NewIsoProjection(CameraSE)
	tile := TilePos{Zoom: 10, X: 3, Y: -2}
	ib := IsoTileBounds(tile)
	minY, maxY := -64, 320

	fp := p.WorldFootprint(ib, minY, maxY)
	if fp.Empty() {
		t.Fatal("footprint is empty")
	}

	// Brute force: every column in a wide search area whose sprite actually
	// intersects the tile must be inside the computed footprint. A column at
	// any elevation in range paints a diamond plus a downward skirt.
	checked := 0
	for x := fp.MinX - 400; x < fp.MaxX+400; x += 7 {
		for z := fp.MinZ - 400; z < fp.MaxZ+400; z += 7 {
			for y := minY; y <= maxY; y += 16 {
				u, v := p.ProjectBlockTop(x, y, z)
				// Sprite extent: diamond [u-1,u+1] x [v, v+1], plus skirt below.
				if u+IsoHalfWidth <= ib.MinU || u-IsoHalfWidth >= ib.MaxU {
					continue
				}
				if v >= ib.MaxV || v+SpriteHeight(y, minY) <= ib.MinV {
					continue
				}
				checked++
				if !fp.Contains(x, z) {
					t.Fatalf("column (%d,%d) at y=%d paints into tile %+v "+
						"(sprite u=%v v=%v) but is outside footprint %+v",
						x, z, y, tile, u, v, fp)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("brute force check never found a contributing column; test is vacuous")
	}
	t.Logf("verified %d contributing columns inside footprint %+v (%dx%d blocks)",
		checked, fp, fp.Width(), fp.Height())
}

func TestIsoWorldFootprintGrowsWithWorldHeight(t *testing.T) {
	p := NewIsoProjection(CameraSE)
	ib := IsoTileBounds(TilePos{Zoom: 10, X: 0, Y: 0})
	shallow := p.WorldFootprint(ib, 0, 16)
	deep := p.WorldFootprint(ib, -64, 320)
	if deep.Width() <= shallow.Width() {
		t.Errorf("taller world should need more overscan: shallow=%d deep=%d",
			shallow.Width(), deep.Width())
	}
	// Overscan must stay bounded: proportional to world height, not explosive.
	if deep.Width() > 4000 {
		t.Errorf("overscan for a 32-iso-unit tile is %d blocks wide; too large", deep.Width())
	}
}

func TestIsoFootprintOfBlocksIsForwardCounterpart(t *testing.T) {
	p := NewIsoProjection(CameraSE)
	chunk := ChunkBounds(ChunkPos{X: 52, Z: -31})
	minY, maxY := -64, 320
	ib := p.IsoFootprintOfBlocks(chunk, minY, maxY)
	if ib.Empty() {
		t.Fatal("iso footprint of a chunk is empty")
	}
	// Every block column in the chunk, at every elevation, must project inside.
	for x := chunk.MinX; x < chunk.MaxX; x++ {
		for z := chunk.MinZ; z < chunk.MaxZ; z++ {
			for _, y := range []int{minY, 0, 64, maxY} {
				u, v := p.ProjectBlockTop(x, y, z)
				if u < ib.MinU || u > ib.MaxU || v < ib.MinV || v > ib.MaxV {
					t.Fatalf("column (%d,%d,y=%d) projects to (%v,%v) outside %+v",
						x, z, y, u, v, ib)
				}
			}
		}
	}
}

func TestIsoTileBoundsTileAtOrigin(t *testing.T) {
	// Zoom 10 iso tile spans 32 iso units, matching the top-down 32-block span
	// so both modes can share one resolution ladder.
	ib := IsoTileBounds(TilePos{Zoom: 10, X: 0, Y: 0})
	if ib.MinU != 0 || ib.MinV != 0 || ib.MaxU != 32 || ib.MaxV != 32 {
		t.Errorf("zoom 10 tile 0,0 = %+v, want 0,0..32,32", ib)
	}
	// Negative tiles must tile contiguously with positive ones.
	left := IsoTileBounds(TilePos{Zoom: 10, X: -1, Y: -1})
	if left.MaxU != 0 || left.MaxV != 0 || left.MinU != -32 || left.MinV != -32 {
		t.Errorf("zoom 10 tile -1,-1 = %+v, want -32,-32..0,0", left)
	}
}

func TestIsoTileAtRoundTrip(t *testing.T) {
	for _, zoom := range []int{0, 6, 10} {
		for _, u := range []float64{0, 1, -1, 31.9, -32.1, 1000.5} {
			for _, v := range []float64{0, -1, 511, -0.001} {
				tp := IsoTileAt(u, v, zoom)
				ib := IsoTileBounds(tp)
				if u < ib.MinU || u >= ib.MaxU || v < ib.MinV || v >= ib.MaxV {
					t.Errorf("zoom %d: (%v,%v) -> %+v with bounds %+v", zoom, u, v, tp, ib)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Ray marching / hit testing
// ---------------------------------------------------------------------------

type flatHeights int

func (f flatHeights) SampleHeight(x, z int) (int, bool) { return int(f), true }

type stepHeights struct{ lowY, highY, splitX int }

func (s stepHeights) SampleHeight(x, z int) (int, bool) {
	if x >= s.splitX {
		return s.highY, true
	}
	return s.lowY, true
}

type emptyHeights struct{}

func (emptyHeights) SampleHeight(x, z int) (int, bool) { return 0, false }

func TestRayMarchOnFlatTerrainMatchesFlatInverse(t *testing.T) {
	p := NewIsoProjection(CameraSE)
	const surface = 64
	h := flatHeights(surface)
	for _, blk := range [][2]int{{0, 0}, {12, 34}, {-17, -33}, {1532, -4290}} {
		// Project the centre of the block's top face, then march back.
		u, v := p.Project(float64(blk[0])+0.5, surface+1, float64(blk[1])+0.5)
		x, y, z, ok := p.RayMarch(u, v, -64, 320, h)
		if !ok {
			t.Fatalf("ray march missed flat terrain at %v", blk)
		}
		if x != blk[0] || z != blk[1] || y != surface {
			t.Errorf("ray march of block %v gave (%d,%d,%d)", blk, x, y, z)
		}
	}
}

func TestRayMarchIsHeightAware(t *testing.T) {
	// A cliff: everything at x>=100 is 100 blocks taller. Pointing at a pixel
	// occupied by the cliff face must return a cliff-top column, NOT the column
	// a flat-plane inverse would return.
	p := NewIsoProjection(CameraSE)
	h := stepHeights{lowY: 64, highY: 164, splitX: 100}

	// Project the top face of a column on the high side.
	targetX, targetZ := 120, 40
	u, v := p.Project(float64(targetX)+0.5, 165, float64(targetZ)+0.5)

	x, y, z, ok := p.RayMarch(u, v, -64, 320, h)
	if !ok {
		t.Fatal("ray march missed cliff terrain")
	}
	if x != targetX || z != targetZ || y != 164 {
		t.Errorf("ray march gave (%d,%d,%d), want (%d,164,%d)", x, y, z, targetX, targetZ)
	}

	// A flat-plane inverse at the low surface height would land somewhere else
	// entirely -- proving the march is doing real work.
	fx, fz := p.UnprojectFlat(u, v, 64)
	if fx == targetX && fz == targetZ {
		t.Error("flat inverse coincidentally matched; test does not prove height awareness")
	}
	t.Logf("height-aware hit (%d,%d) vs naive flat inverse (%d,%d)", targetX, targetZ, fx, fz)
}

func TestRayMarchReturnsTopmostSurface(t *testing.T) {
	p := NewIsoProjection(CameraSE)
	h := flatHeights(100)
	// A pixel well above the terrain silhouette should miss entirely...
	u, v := p.Project(0.5, 100+1, 0.5)
	_, y, _, ok := p.RayMarch(u, v-500, -64, 320, h)
	if ok && y > 100 {
		t.Errorf("march above the silhouette reported a hit at y=%d", y)
	}
	// ...and a pixel on the terrain must report the surface, never above it.
	_, y, _, ok = p.RayMarch(u, v, -64, 320, h)
	if !ok || y != 100 {
		t.Errorf("expected surface hit at y=100, got y=%d ok=%v", y, ok)
	}
}

func TestRayMarchMissesUngeneratedTerrain(t *testing.T) {
	p := NewIsoProjection(CameraSE)
	if _, _, _, ok := p.RayMarch(0, 0, -64, 320, emptyHeights{}); ok {
		t.Error("ray march should miss when no column has data")
	}
}

func TestSpriteHeight(t *testing.T) {
	if got := SpriteHeight(64, 64); got != IsoDiamondHeight {
		t.Errorf("flush column sprite height = %v, want %v", got, IsoDiamondHeight)
	}
	if got := SpriteHeight(64, 60); got != IsoDiamondHeight+4*IsoBlockHeight {
		t.Errorf("4-deep skirt sprite height = %v", got)
	}
	if got := SpriteHeight(60, 64); got != IsoDiamondHeight {
		t.Errorf("inverted input should clamp, got %v", got)
	}
}
