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
		topFace, leftFace, rightFace := r.voxelFaceVisibility(r.Volume, a, b, o.x, o.y, o.z)
		if !topFace && !leftFace && !rightFace {
			continue
		}
		id := ids[o.scenePos]
		r.drawVoxel(ref, surf, ib, ppu, w, h, a, b, o.x, o.y, o.z, id, 15, topFace, leftFace, rightFace, isColumnTop, 0, 0)
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
