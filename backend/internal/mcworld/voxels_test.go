package mcworld

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/enderfall/minecraft-map/backend/internal/blocks"
	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

// openTestWorldWithVoxelDepth is openTestWorld with an explicit, small depth
// config, so depth-sizing assertions do not need enormous synthetic chunks.
func openTestWorldWithVoxelDepth(t *testing.T, dir string, cfg VoxelDepthConfig) *World {
	t.Helper()
	reg, err := blocks.NewDefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	bio, err := blocks.NewDefaultBiomes()
	if err != nil {
		t.Fatal(err)
	}
	w, err := Open(Options{
		Path:       dir,
		Blocks:     reg,
		Biomes:     bio,
		VoxelDepth: cfg,
		Log:        slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	if err != nil {
		t.Fatalf("open world: %v", err)
	}
	return w
}

// buildCanopyChunk writes one chunk: ground (stone/dirt/grass) tops out at
// y=13 across the whole chunk, and one column (0,0) additionally has a
// single oak_leaves block floating at y=28, with air in between -- the exact
// "canopy above a gap above the ground" shape the voxel plan exists to
// capture correctly, and the heightmap renderer's bug report was about.
func buildCanopyChunk(t *testing.T, dir string) {
	t.Helper()
	regionDir := filepath.Join(dir, "region")
	if err := os.MkdirAll(regionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ground := sectionSpec{
		y:       0,
		palette: []string{"minecraft:air", "minecraft:stone", "minecraft:dirt", "minecraft:grass_block"},
		biome:   "minecraft:plains",
		blockAt: func(x, ly, z int) int {
			switch {
			case ly <= 9:
				return 1 // stone
			case ly <= 12:
				return 2 // dirt
			case ly == 13:
				return 3 // grass
			default:
				return 0 // air
			}
		},
	}
	canopy := sectionSpec{
		y:       1,
		palette: []string{"minecraft:air", "minecraft:oak_leaves"},
		biome:   "minecraft:plains",
		blockAt: func(x, ly, z int) int {
			if ly == 12 && x == 0 && z == 0 {
				return 1 // leaves at world Y = 16+12 = 28
			}
			return 0
		},
	}

	pos := mcmath.ChunkPos{X: 0, Z: 0}
	writeRegion(t, filepath.Join(regionDir, "r.0.0.mca"), map[mcmath.ChunkPos][]byte{
		pos: buildChunkNBT(0, 0, []sectionSpec{ground, canopy}),
	})
}

// buildFlatChunk writes one chunk with a uniform solid surface and nothing
// above it: no canopy, no water, so the topmost non-air block and the
// topmost solid block coincide in every column and the canopy-to-ground span
// is exactly 0 everywhere.
func buildFlatChunk(t *testing.T, dir string) {
	t.Helper()
	regionDir := filepath.Join(dir, "region")
	if err := os.MkdirAll(regionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ground := sectionSpec{
		y:       0,
		palette: []string{"minecraft:air", "minecraft:stone", "minecraft:dirt", "minecraft:grass_block"},
		biome:   "minecraft:plains",
		blockAt: func(x, ly, z int) int {
			switch {
			case ly <= 9:
				return 1
			case ly <= 12:
				return 2
			case ly == 13:
				return 3
			default:
				return 0
			}
		},
	}
	pos := mcmath.ChunkPos{X: 0, Z: 0}
	writeRegion(t, filepath.Join(regionDir, "r.0.0.mca"), map[mcmath.ChunkPos][]byte{
		pos: buildChunkNBT(0, 0, []sectionSpec{ground}),
	})
}

// TestChunkVoxelsCanopyAboveGround exercises the exact scenario
// ISO_VOXEL_PLAN.md §1 is about: a canopy floating above a gap above solid
// ground. TopY must be the canopy, the slab must still reach the ground
// below the gap, and the gap itself must read back as air, not as unknown.
func TestChunkVoxelsCanopyAboveGround(t *testing.T) {
	dir := t.TempDir()
	buildCanopyChunk(t, dir)
	w := openTestWorldWithVoxelDepth(t, dir, VoxelDepthConfig{BelowGround: 2, MinDepth: 4, MaxDepth: 100})
	defer w.Close()

	ctx := context.Background()
	cv, err := w.ChunkVoxels(ctx, "minecraft:overworld", mcmath.ChunkPos{X: 0, Z: 0})
	if err != nil {
		t.Fatalf("ChunkVoxels: %v", err)
	}

	leafCol := world.Index(0, 0)
	if got := int(cv.TopY[leafCol]); got != 28 {
		t.Errorf("leaf column TopY = %d, want 28 (the canopy, not the ground)", got)
	}

	// span = 28-13 = 15, + BelowGround 2 = 17.
	if cv.Depth != 17 {
		t.Errorf("Depth = %d, want 17 (span 15 + belowGround 2)", cv.Depth)
	}

	id, _, ok := cv.At(leafCol, 28)
	if !ok {
		t.Fatal("leaf voxel not in slab")
	}
	leavesID := w.blocks.ID("minecraft:oak_leaves")
	if id != leavesID {
		t.Errorf("voxel at y=28 = block %d, want oak_leaves (%d)", id, leavesID)
	}

	id, _, ok = cv.At(leafCol, 20)
	if !ok {
		t.Fatal("gap voxel not in slab")
	}
	if id != blocks.AirID {
		t.Errorf("voxel at y=20 (the gap) = block %d, want air", id)
	}

	id, _, ok = cv.At(leafCol, 13)
	if !ok {
		t.Fatal("ground voxel not in slab under the canopy -- the exact bug this plan fixes")
	}
	grassID := w.blocks.ID("minecraft:grass_block")
	if id != grassID {
		t.Errorf("voxel at y=13 (ground) = block %d, want grass_block (%d)", id, grassID)
	}

	// A flat column elsewhere in the same chunk has no canopy: TopY is its
	// own solid surface, not the leaf column's.
	flatCol := world.Index(15, 15)
	if got := int(cv.TopY[flatCol]); got != 13 {
		t.Errorf("flat column TopY = %d, want 13", got)
	}
	id, _, ok = cv.At(flatCol, 13)
	if !ok || id != grassID {
		t.Errorf("flat column ground voxel = (%d, ok=%v), want grass_block", id, ok)
	}

	// Above y=13, undefined for this dataset entirely: outside the slab it
	// must report ok=false, never a false "air".
	if _, _, ok := cv.At(flatCol, 5000); ok {
		t.Error("wildly out-of-range Y reported ok=true")
	}
}

// TestChunkVoxelsDepthClampsToConfig checks the MinDepth/MaxDepth clamps
// independent of the canopy span, since a real forest or ravine easily
// exceeds either bound.
func TestChunkVoxelsDepthClampsToConfig(t *testing.T) {
	dir := t.TempDir()
	buildCanopyChunk(t, dir) // span 15
	ctx := context.Background()

	// MinDepth above the natural span: the clamp must win.
	w := openTestWorldWithVoxelDepth(t, dir, VoxelDepthConfig{BelowGround: 2, MinDepth: 40, MaxDepth: 100})
	cv, err := w.ChunkVoxels(ctx, "minecraft:overworld", mcmath.ChunkPos{X: 0, Z: 0})
	if err != nil {
		t.Fatal(err)
	}
	if cv.Depth != 40 {
		t.Errorf("Depth = %d, want MinDepth 40 to win over the smaller natural span", cv.Depth)
	}
	w.Close()

	// MaxDepth below the natural span: the clamp must win the other way.
	w = openTestWorldWithVoxelDepth(t, dir, VoxelDepthConfig{BelowGround: 2, MinDepth: 4, MaxDepth: 10})
	cv, err = w.ChunkVoxels(ctx, "minecraft:overworld", mcmath.ChunkPos{X: 0, Z: 0})
	if err != nil {
		t.Fatal(err)
	}
	if cv.Depth != 10 {
		t.Errorf("Depth = %d, want MaxDepth 10 to win over the larger natural span", cv.Depth)
	}
	w.Close()
}

// TestChunkVoxelsFlatChunkIsShallow is the plan's other named accept
// criterion: a chunk with zero canopy-to-ground span everywhere pays only
// MinDepth, not BelowGround or the configured maximum.
func TestChunkVoxelsFlatChunkIsShallow(t *testing.T) {
	dir := t.TempDir()
	buildFlatChunk(t, dir)
	w := openTestWorldWithVoxelDepth(t, dir, VoxelDepthConfig{BelowGround: 5, MinDepth: 8, MaxDepth: 64})
	defer w.Close()

	cv, err := w.ChunkVoxels(context.Background(), "minecraft:overworld", mcmath.ChunkPos{X: 0, Z: 0})
	if err != nil {
		t.Fatal(err)
	}
	// span 0 + BelowGround 5 = 5, below MinDepth 8, so the clamp must win.
	if cv.Depth != 8 {
		t.Errorf("flat chunk Depth = %d, want MinDepth 8 (0 span + belowGround 5 clamped up)", cv.Depth)
	}
}

// TestCachedChunkVoxelsDelegatesToRealProvider pins world.Cached's type
// assertion against VolumeProvider: wrapping a real mcworld.World must serve
// real voxel data through the cache, not silently report
// ErrVoxelsUnsupported (which is reserved for providers, like demo.World,
// that never implement the interface at all).
func TestCachedChunkVoxelsDelegatesToRealProvider(t *testing.T) {
	dir := t.TempDir()
	buildTestWorld(t, dir, []mcmath.ChunkPos{{X: 0, Z: 0}})
	w := openTestWorld(t, dir)
	defer w.Close()

	cached := world.NewCached(w, 0, 0)
	// mcworld.World *does* implement VolumeProvider, so this should succeed
	// through the cache rather than falling back -- the fallback path is
	// exercised by demo.World elsewhere, but the cache's type assertion is
	// the thing this test pins down.
	if _, err := cached.ChunkVoxels(context.Background(), "minecraft:overworld", mcmath.ChunkPos{X: 0, Z: 0}); err != nil {
		t.Errorf("ChunkVoxels through Cached over a real World: %v", err)
	}
}
