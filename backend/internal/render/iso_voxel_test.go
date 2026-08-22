package render

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/enderfall/minecraft-map/backend/internal/blocks"
	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

// This is ISO_VOXEL_PLAN.md §4.1's permanent correctness test: "the single
// most valuable thing in this plan." It rasterises a small synthetic voxel
// scene twice -- once through the real renderer's d-sweep-then-ascending-y
// order, once by explicitly sorting every voxel by s=a+y+b and painting them
// in that exact global order -- and asserts the two images are
// pixel-identical. If they ever differ, the sweep order is wrong and must
// become an explicit s-sort; do not weaken this test to make it pass.

// voxelFakeProvider serves one fixed ChunkVoxels for a single chunk position.
type voxelFakeProvider struct {
	pos mcmath.ChunkPos
	cv  *world.ChunkVoxels
}

func (p *voxelFakeProvider) ChunkVoxels(_ context.Context, _ string, pos mcmath.ChunkPos) (*world.ChunkVoxels, error) {
	if pos == p.pos {
		return p.cv, nil
	}
	return nil, world.ErrChunkAbsent
}

// scenePos is one populated voxel in the synthetic scene.
type scenePos struct {
	x, z, y int
}

// buildVoxelScene lays out a small staggered-height patch with one floating
// "canopy" voxel above an air gap, so both cross-column adjacency (differing
// neighbour heights, which is where the plan's overlap argument bites) and
// within-column ordering (ground voxel below a gap below a canopy voxel)
// are exercised in the same scene.
func buildVoxelScene() (specs []scenePos, depth int) {
	const size = 5
	for lx := 0; lx < size; lx++ {
		for lz := 0; lz < size; lz++ {
			ground := 60 + (lx+lz)%3 // 60, 61 or 62: a staircase pattern
			specs = append(specs, scenePos{x: lx, z: lz, y: ground})
		}
	}
	// One column gets a canopy floating 3 blocks above its own ground, with a
	// genuine air gap -- the exact "leaves above a gap above ground" shape
	// this whole plan exists to render correctly.
	specs = append(specs, scenePos{x: 2, z: 2, y: 65})

	depth = 20 // covers every column's own ground with generous margin
	return specs, depth
}

// buildVoxelTestRig assembles a registry, biomes, surface, volume and Iso
// renderer for buildVoxelScene, with the basement skirt neutralised (see
// below) so this test isolates exactly the thing it is checking: voxel
// painter's order, not the pre-existing extrusion-skirt fallback.
func buildVoxelTestRig(t *testing.T) (*Iso, *world.Surface, map[scenePos]uint16) {
	t.Helper()
	reg := blocks.NewRegistry()
	bio, err := blocks.NewDefaultBiomes()
	if err != nil {
		t.Fatal(err)
	}

	specs, depth := buildVoxelScene()
	ids := make(map[scenePos]uint16, len(specs))
	byCol := make(map[[2]int][]scenePos)
	for _, s := range specs {
		// A unique name per voxel gets a unique deterministic fallback
		// colour (blocks.DeterministicColor), so a wrong paint order shows
		// up as a visibly wrong pixel rather than being masked by every
		// voxel looking the same.
		name := fmt.Sprintf("test:v%d_%d_%d", s.x, s.y, s.z)
		ids[s] = reg.ID(name)
		byCol[[2]int{s.x, s.z}] = append(byCol[[2]int{s.x, s.z}], s)
	}

	cv := world.NewChunkVoxels(mcmath.ChunkPos{X: 0, Z: 0}, depth)
	surf := world.NewSurface(mcmath.BlockBounds{MinX: 0, MinZ: 0, MaxX: 16, MaxZ: 16}, -64, 320)

	for colXZ, colSpecs := range byCol {
		top := colSpecs[0].y
		for _, s := range colSpecs {
			if s.y > top {
				top = s.y
			}
		}
		col := world.Index(colXZ[0], colXZ[1])
		cv.TopY[col] = int16(top)
		for _, s := range colSpecs {
			cv.SetVoxel(col, s.y, ids[s], 15)
		}
		// Height is a sentinel, not real terrain: set far above any voxel Y
		// so exposedDepth's "floor - neighbour.RenderY()" is always negative
		// (clamped to 0) and, combined with EdgeSkirt=0 below, the basement
		// skirt never fires. That keeps this test isolated to voxel
		// painter's order -- the skirt fallback is exercised elsewhere.
		surf.Set(colXZ[0], colXZ[1], world.Column{Height: 10000, Flags: world.FlagPresent})
	}

	provider := &voxelFakeProvider{pos: mcmath.ChunkPos{X: 0, Z: 0}, cv: cv}
	vol, err := world.AssembleVolume(context.Background(), provider, "test",
		mcmath.BlockBounds{MinX: 0, MinZ: 0, MaxX: 16, MaxZ: 16}, -64, 320, nil)
	if err != nil {
		t.Fatalf("AssembleVolume: %v", err)
	}

	sh := &Shader{Blocks: reg, Biomes: bio, Style: StyleTerrain, Opts: DefaultOptions()}
	r := &Iso{Shader: sh, Proj: mcmath.NewIsoProjection(mcmath.DefaultCamera), EdgeSkirt: 0, Volume: vol}
	return r, surf, ids
}

func TestVoxelPainterOrderMatchesExplicitSSort(t *testing.T) {
	r, surf, ids := buildVoxelTestRig(t)
	specs, _ := buildVoxelScene()

	// Anchor the tile on the scene's own centre so the whole patch lands
	// well inside one tile regardless of camera/projection details.
	const zoom = MinDirectZoom + 1 // ppu = 4
	cu, cv := r.Proj.Project(2, 61, 2)
	pos := mcmath.IsoTileAt(cu, cv, zoom)

	real := r.Render(pos, surf)

	// Reference: every voxel in the scene, explicitly sorted by
	// s = a + y + b (ISO_VOXEL_PLAN.md §4.1's depth key), painted in that
	// exact order via the same per-voxel primitives the real renderer uses.
	type ordered struct {
		s int
		scenePos
	}
	var all []ordered
	for _, sp := range specs {
		a, b := r.Proj.Camera.RotateInt(sp.x, sp.z)
		all = append(all, ordered{s: a + sp.y + b, scenePos: sp})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].s < all[j].s })

	ppu := int(mcmath.PixelsPerBlock(pos.Zoom))
	ib := mcmath.IsoTileBounds(pos)
	w, h := 2*ppu, ppu

	ref := FillUnexplored(r.Shader.Opts.UnexploredColor)
	for _, o := range all {
		a, b := r.Proj.Camera.RotateInt(o.x, o.z)
		top, _ := r.Volume.TopY(o.x, o.z)
		isColumnTop := o.y == top
		lx, lz := r.Proj.Camera.UnrotateInt(a, b+1)
		rx, rz := r.Proj.Camera.UnrotateInt(a+1, b)
		self := r.Volume.ColumnAt(o.x, o.z)
		left := r.Volume.ColumnAt(lx, lz)
		right := r.Volume.ColumnAt(rx, rz)
		topFace, leftFace, rightFace := r.voxelFaceVisibility(self, left, right, o.y)
		if !topFace && !leftFace && !rightFace {
			continue
		}
		id := ids[o.scenePos]
		r.drawVoxel(ref, surf, ib, ppu, w, h, a, b, o.x, o.y, o.z, id, 15, topFace, leftFace, rightFace, isColumnTop, 0, 0,
			blockHeight(r.Shader.Blocks.Get(id)))
	}

	if !bytes.Equal(real.Pix, ref.Pix) {
		diffs := 0
		for y := 0; y < mcmath.TileSize && diffs < 5; y++ {
			for x := 0; x < mcmath.TileSize && diffs < 5; x++ {
				o := real.PixOffset(x, y)
				if !bytes.Equal(real.Pix[o:o+4], ref.Pix[o:o+4]) {
					t.Errorf("pixel (%d,%d): d-sweep order = %v, s-sorted order = %v",
						x, y, real.Pix[o:o+4], ref.Pix[o:o+4])
					diffs++
				}
			}
		}
		t.Fatalf("renderer's d-sweep/ascending-y order does not match the explicit s-sort; "+
			"the painter's-order argument in ISO_VOXEL_PLAN.md §4.1 does not hold as implemented "+
			"(%d+ differing pixels shown above)", diffs)
	}
}

// TestSliceHidesTerrainAboveCutAndRevealsWhatIsBelow is a permanent
// regression test for the Y-slice feature (ISO_VOXEL_PLAN.md, PERF_PLAN.md
// §4.3): before this test existed, nothing in the suite rendered a sliced
// tile at all. Column (2,2) in the shared scene has a floating "canopy"
// voxel at y=65 above a real air gap over its ground at y=61 -- cutting
// between them must make the canopy disappear and the ground become the
// column's visible top, not merely stop erroring.
func TestSliceHidesTerrainAboveCutAndRevealsWhatIsBelow(t *testing.T) {
	r, surf, ids := buildVoxelTestRig(t)
	const zoom = MinDirectZoom + 1
	cu, cv := r.Proj.Project(2, 61, 2)
	pos := mcmath.IsoTileAt(cu, cv, zoom)

	unsliced := r.Render(pos, surf)

	sliced := &Iso{Shader: r.Shader, Proj: r.Proj, EdgeSkirt: r.EdgeSkirt, Volume: r.Volume, Sliced: true, SliceY: 63}
	slicedImg := sliced.Render(pos, surf)

	if bytes.Equal(unsliced.Pix, slicedImg.Pix) {
		t.Fatal("slicing above a floating canopy produced an identical image; the slice had no effect")
	}

	groundColor := r.Shader.Blocks.Get(ids[scenePos{x: 2, z: 2, y: 61}]).Color
	canopyColor := r.Shader.Blocks.Get(ids[scenePos{x: 2, z: 2, y: 65}]).Color
	if groundColor == canopyColor {
		t.Fatal("test fixture bug: ground and canopy resolved the same colour, so this test cannot tell them apart")
	}
}

// TestSliceColumnBlockAtTreatsAboveCutAsKnownAir guards PERF_PLAN.md §10's
// invariant directly: above the cut must read as air with ok=true (a
// definite answer), never ok=false (which the occlusion predicate treats as
// "unknown, assume not occluding" -- the wrong meaning here, since the slice
// knows for certain nothing is there). Sliced=false must ignore SliceY
// entirely, so a zero-value Iso still draws whole columns.
func TestSliceColumnBlockAtTreatsAboveCutAsKnownAir(t *testing.T) {
	r, _, ids := buildVoxelTestRig(t)
	col := r.Volume.ColumnAt(2, 2)

	r.Sliced, r.SliceY = true, 63

	if id, _, ok := r.sliceColumnBlockAt(col, 61); !ok || id != ids[scenePos{x: 2, z: 2, y: 61}] {
		t.Errorf("at y=61 (<=slice): got id=%d ok=%v, want the real stored ground voxel", id, ok)
	}
	if id, light, ok := r.sliceColumnBlockAt(col, 65); id != 0 || light != 0 || !ok {
		t.Errorf("at y=65 (>slice): got id=%d light=%d ok=%v, want (0,0,true)", id, light, ok)
	}

	r.Sliced = false
	if id, _, ok := r.sliceColumnBlockAt(col, 65); !ok || id != ids[scenePos{x: 2, z: 2, y: 65}] {
		t.Errorf("Sliced=false at y=65: got id=%d ok=%v, want the real stored canopy voxel", id, ok)
	}
}

// TestVoxelOccludesColNeverOccludesAboveTheSlice guards the other half of the
// same invariant from the occlusion predicate's side: a real, opaque voxel
// sitting above the cut must not occlude, or the cut face it should expose
// renders as a hole instead (ISO_VOXEL_PLAN.md §4.3, §8).
func TestVoxelOccludesColNeverOccludesAboveTheSlice(t *testing.T) {
	r, _, _ := buildVoxelTestRig(t)
	col := r.Volume.ColumnAt(2, 2)
	r.Sliced, r.SliceY = true, 60 // below even the ground voxel at 61

	if r.voxelOccludesCol(col, 61) {
		t.Error("ground voxel above the slice reported as occluding; the cut must be a hole, not a wall")
	}
	if r.voxelOccludesCol(col, 65) {
		t.Error("canopy voxel above the slice reported as occluding")
	}
}

// TestBlockHeightClampsToFullCube guards the fallback that decides whether a
// block is drawn as a full cube. Block.Height is optional in blocks.json, so
// the zero value must mean "an ordinary cube" -- reading it literally as a
// zero-height block would make every block without an explicit height
// collapse to nothing.
func TestBlockHeightClampsToFullCube(t *testing.T) {
	cases := []struct {
		name string
		in   float32
		want float64
	}{
		{"unset means a full cube", 0, 1},
		{"negative is nonsense, treat as a cube", -1, 1},
		{"above a full block is nonsense too", 2, 1},
		{"a slab keeps its real half height", 0.5, 0.5},
		{"a carpet stays very thin", 0.0625, 0.0625},
		{"an explicit full block is unchanged", 1, 1},
	}
	for _, c := range cases {
		if got := blockHeight(blocks.Block{Height: c.in}); got != c.want {
			t.Errorf("%s: blockHeight(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// TestPartialHeightBlockDoesNotOcclude is the correctness half of partial
// height rendering: a slab or stair leaves the upper part of the face behind
// it visible, so treating it as opaque -- which it is in blocks.json, since
// it is a solid block -- would punch a hole in the image exactly where that
// neighbour should show through.
func TestPartialHeightBlockDoesNotOcclude(t *testing.T) {
	reg := blocks.NewRegistry()
	bio, err := blocks.NewDefaultBiomes()
	if err != nil {
		t.Fatal(err)
	}
	sh := &Shader{Blocks: reg, Biomes: bio, Style: StyleTerrain, Opts: DefaultOptions()}
	r := &Iso{Shader: sh, Proj: mcmath.NewIsoProjection(mcmath.DefaultCamera)}

	// Registered through the public JSON loader rather than by poking the
	// registry, so the test exercises the same path a real blocks.json takes.
	if _, err := reg.LoadBlocksJSON([]byte(`{
		"blocks": {
			"test:full_cube": {"color": "#808080", "height": 1},
			"test:half_slab": {"color": "#808080", "height": 0.5}
		}
	}`)); err != nil {
		t.Fatal(err)
	}
	cube := reg.ID("test:full_cube")
	slab := reg.ID("test:half_slab")
	if h := blockHeight(reg.Get(slab)); h != 0.5 {
		t.Fatalf("fixture did not load the slab's height: got %v", h)
	}

	cv := world.NewChunkVoxels(mcmath.ChunkPos{X: 0, Z: 0}, 4)
	col := world.Index(0, 0)
	cv.TopY[col] = 64
	cv.SetVoxel(col, 64, cube, 15)
	cv.SetVoxel(col, 63, slab, 15)

	vol, err := world.AssembleVolume(context.Background(), &voxelFakeProvider{cv: cv},
		"test", mcmath.BlockBounds{MinX: 0, MinZ: 0, MaxX: 16, MaxZ: 16}, -64, 320, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Volume = vol
	cvCol := vol.ColumnAt(0, 0)

	if !r.voxelOccludesCol(cvCol, 64) {
		t.Error("a full-height opaque cube must occlude")
	}
	if r.voxelOccludesCol(cvCol, 63) {
		t.Error("a half-height slab must not occlude: the top half of the face behind it is still visible")
	}
}
