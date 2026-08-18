package world

import (
	"context"
	"errors"
	"testing"

	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
)

// TestChunkVoxelsSetAtRoundTrip pins the layer-index arithmetic: every voxel
// written within the stored slab must read back exactly, and anything
// outside it must report ok=false rather than a wrong value.
func TestChunkVoxelsSetAtRoundTrip(t *testing.T) {
	cv := NewChunkVoxels(mcmath.ChunkPos{X: 2, Z: -3}, 5)
	col := Index(3, 7)
	cv.TopY[col] = 100

	for y := 96; y <= 100; y++ {
		cv.SetVoxel(col, y, uint16(y), uint8(y%16))
	}
	for y := 96; y <= 100; y++ {
		id, light, ok := cv.At(col, y)
		if !ok {
			t.Fatalf("y=%d: not ok, want a stored voxel", y)
		}
		if id != uint16(y) || light != uint8(y%16) {
			t.Errorf("y=%d: got (id=%d light=%d), want (id=%d light=%d)", y, id, light, y, y%16)
		}
	}

	// One below the slab (floor-1) and one above the top must both miss.
	if _, _, ok := cv.At(col, 95); ok {
		t.Error("y=95 (below the 5-layer slab) reported ok=true")
	}
	if _, _, ok := cv.At(col, 101); ok {
		t.Error("y=101 (above TopY) reported ok=true")
	}

	// A write outside the slab must be a silent no-op, not corrupt a
	// neighbouring column's data.
	other := Index(4, 7)
	cv.SetVoxel(col, 200, 999, 9)
	if id, _, ok := cv.At(other, 100); ok && id != 0 {
		t.Error("out-of-range SetVoxel touched a neighbouring column")
	}
}

// TestNewChunkVoxelsClampsDepth guards against a caller-supplied depth of
// zero or less producing a slab that can never hold anything.
func TestNewChunkVoxelsClampsDepth(t *testing.T) {
	cv := NewChunkVoxels(mcmath.ChunkPos{}, 0)
	if cv.Depth != 1 {
		t.Errorf("Depth = %d, want clamped to 1", cv.Depth)
	}
	if len(cv.Block) != ColumnCount || len(cv.Light) != ColumnCount {
		t.Errorf("backing arrays sized for Depth=%d, want %d", cv.Depth, ColumnCount)
	}
}

// fakeVolumeProvider serves ChunkVoxels from an in-memory map, so
// AssembleVolume can be exercised without touching mcworld or disk.
type fakeVolumeProvider struct {
	chunks map[mcmath.ChunkPos]*ChunkVoxels
	errAt  map[mcmath.ChunkPos]error
}

func (p *fakeVolumeProvider) ChunkVoxels(_ context.Context, _ string, pos mcmath.ChunkPos) (*ChunkVoxels, error) {
	if err, ok := p.errAt[pos]; ok {
		return nil, err
	}
	if cv, ok := p.chunks[pos]; ok {
		return cv, nil
	}
	return nil, ErrChunkAbsent
}

// TestAssembleVolumeReadsAcrossChunkBoundary is the round-trip test for the
// window layer: two adjacent chunks, each with one distinctive voxel, must
// both be readable through a single Volume spanning both of them, and
// positions outside any loaded chunk must report ok=false rather than a
// zero value that could be mistaken for air.
func TestAssembleVolumeReadsAcrossChunkBoundary(t *testing.T) {
	west := NewChunkVoxels(mcmath.ChunkPos{X: 0, Z: 0}, 4)
	west.TopY[Index(15, 0)] = 50
	west.SetVoxel(Index(15, 0), 50, 111, 12)

	east := NewChunkVoxels(mcmath.ChunkPos{X: 1, Z: 0}, 4)
	east.TopY[Index(0, 0)] = 60
	east.SetVoxel(Index(0, 0), 60, 222, 8)

	p := &fakeVolumeProvider{chunks: map[mcmath.ChunkPos]*ChunkVoxels{
		{X: 0, Z: 0}: west,
		{X: 1, Z: 0}: east,
	}}

	bounds := mcmath.BlockBounds{MinX: 0, MinZ: 0, MaxX: 32, MaxZ: 16}
	var callbackErrs []error
	vol, err := AssembleVolume(context.Background(), p, "minecraft:overworld", bounds, -64, 320,
		func(_ mcmath.ChunkPos, err error) { callbackErrs = append(callbackErrs, err) })
	if err != nil {
		t.Fatalf("AssembleVolume: %v", err)
	}
	if len(callbackErrs) != 0 {
		t.Fatalf("unexpected chunk errors: %v", callbackErrs)
	}

	if id, light, ok := vol.BlockAt(15, 50, 0); !ok || id != 111 || light != 12 {
		t.Errorf("west chunk voxel = (id=%d light=%d ok=%v), want (111, 12, true)", id, light, ok)
	}
	if id, light, ok := vol.BlockAt(16, 60, 0); !ok || id != 222 || light != 8 {
		t.Errorf("east chunk voxel = (id=%d light=%d ok=%v), want (222, 8, true)", id, light, ok)
	}
	if top, ok := vol.TopY(15, 0); !ok || top != 50 {
		t.Errorf("west TopY = (%d, %v), want (50, true)", top, ok)
	}
	if top, ok := vol.TopY(16, 0); !ok || top != 60 {
		t.Errorf("east TopY = (%d, %v), want (60, true)", top, ok)
	}

	// Outside the requested bounds entirely.
	if _, _, ok := vol.BlockAt(1000, 50, 0); ok {
		t.Error("position outside the volume's bounds reported ok=true")
	}
}

// TestAssembleVolumeSkipsAbsentAndUnsupportedChunks mirrors Assemble's
// policy: a chunk that has never been generated, or a provider that cannot
// supply voxel data for one region, must not fail the whole window or be
// reported through onError -- only genuine errors are.
func TestAssembleVolumeSkipsAbsentAndUnsupportedChunks(t *testing.T) {
	genuine := errors.New("disk on fire")
	p := &fakeVolumeProvider{
		chunks: map[mcmath.ChunkPos]*ChunkVoxels{},
		errAt: map[mcmath.ChunkPos]error{
			{X: 0, Z: 0}: ErrChunkAbsent,
			{X: 1, Z: 0}: ErrVoxelsUnsupported,
			{X: 0, Z: 1}: genuine,
		},
	}
	bounds := mcmath.BlockBounds{MinX: 0, MinZ: 0, MaxX: 32, MaxZ: 32}
	var reported []error
	vol, err := AssembleVolume(context.Background(), p, "minecraft:overworld", bounds, -64, 320,
		func(_ mcmath.ChunkPos, err error) { reported = append(reported, err) })
	if err != nil {
		t.Fatalf("AssembleVolume: %v", err)
	}
	if len(reported) != 1 || !errors.Is(reported[0], genuine) {
		t.Errorf("onError calls = %v, want exactly one genuine error", reported)
	}
	if _, _, ok := vol.BlockAt(0, 0, 0); ok {
		t.Error("absent chunk's column reported ok=true")
	}
}

// TestVoxelRayMarchResolvesActualHitElevation is Phase 3's core claim: aiming
// at different elevations of the same column resolves to the specific voxel
// actually there -- the canopy, the ground beneath a real gap, or nothing at
// all when aimed into the gap itself -- rather than always answering with
// the column's topmost block the way the flattened heightmap ray march does.
func TestVoxelRayMarchResolvesActualHitElevation(t *testing.T) {
	const groundID, decorID, canopyID = 1, 2, 3
	occludes := func(id uint16) bool { return id == groundID || id == canopyID }

	cv := NewChunkVoxels(mcmath.ChunkPos{X: 0, Z: 0}, 20)
	col := Index(5, 5)
	cv.TopY[col] = 70
	cv.SetVoxel(col, 60, groundID, 15)
	cv.SetVoxel(col, 61, decorID, 15) // a decoration: never occludes
	cv.SetVoxel(col, 70, canopyID, 15)

	p := &fakeVolumeProvider{chunks: map[mcmath.ChunkPos]*ChunkVoxels{{X: 0, Z: 0}: cv}}
	vol, err := AssembleVolume(context.Background(), p, "test",
		mcmath.BlockBounds{MinX: 0, MinZ: 0, MaxX: 16, MaxZ: 16}, -64, 320, nil)
	if err != nil {
		t.Fatal(err)
	}

	proj := mcmath.NewIsoProjection(mcmath.DefaultCamera)
	aimAt := func(y int) (float64, float64) {
		return proj.Project(5.5, float64(y)+0.5, 5.5)
	}

	t.Run("hits the canopy when aimed at it", func(t *testing.T) {
		u, v := aimAt(70)
		x, y, z, ok := VoxelRayMarch(proj, u, v, -64, 320, vol, occludes)
		if !ok || x != 5 || y != 70 || z != 5 {
			t.Errorf("got (%d,%d,%d,ok=%v), want (5,70,5,true)", x, y, z, ok)
		}
	})
	t.Run("hits the ground when aimed at it", func(t *testing.T) {
		u, v := aimAt(60)
		x, y, z, ok := VoxelRayMarch(proj, u, v, -64, 320, vol, occludes)
		if !ok || x != 5 || y != 60 || z != 5 {
			t.Errorf("got (%d,%d,%d,ok=%v), want (5,60,5,true)", x, y, z, ok)
		}
	})
	t.Run("finds nothing when aimed into the real air gap", func(t *testing.T) {
		u, v := aimAt(65)
		_, _, _, ok := VoxelRayMarch(proj, u, v, -64, 320, vol, occludes)
		if ok {
			t.Error("aiming into the empty gap between ground and canopy reported a hit")
		}
	})
	t.Run("a decoration never occludes the ray", func(t *testing.T) {
		u, v := aimAt(61)
		_, _, _, ok := VoxelRayMarch(proj, u, v, -64, 320, vol, occludes)
		if ok {
			t.Error("aiming at a non-occluding decoration voxel reported a hit")
		}
	})
}
