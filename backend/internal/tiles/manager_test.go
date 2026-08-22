package tiles

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/enderfall/minecraft-map/backend/internal/blocks"
	"github.com/enderfall/minecraft-map/backend/internal/cache"
	"github.com/enderfall/minecraft-map/backend/internal/demo"
	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/render"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

// newTestManager builds a Manager against the deterministic demo world
// (world.Provider only, no voxel support -- demo.World deliberately does not
// implement VolumeProvider) and a filesystem store rooted in a scratch
// directory, so these tests exercise the real production tile pipeline
// end to end without touching a real Minecraft save.
func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	return newTestManagerWithConfig(t, DefaultConfig())
}

func newTestManagerWithConfig(t *testing.T, cfg Config) (*Manager, string) {
	t.Helper()
	reg, err := blocks.NewDefaultRegistry()
	if err != nil {
		t.Fatalf("NewDefaultRegistry: %v", err)
	}
	bio, err := blocks.NewDefaultBiomes()
	if err != nil {
		t.Fatalf("NewDefaultBiomes: %v", err)
	}
	dw := demo.New(reg, bio, demo.Options{Seed: 42, Radius: 2000})
	provider := world.NewCached(dw, 64<<20, 64<<20)
	store, err := cache.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}

	m := NewManager(provider, store, reg, bio, render.DefaultOptions(), cfg, 4, 64, nil)
	t.Cleanup(m.Close)

	dims, err := provider.Dimensions(context.Background())
	if err != nil || len(dims) == 0 {
		t.Fatalf("Dimensions: %v, %v", dims, err)
	}
	return m, dims[0].ID
}

func TestTileGeneratesTopDown(t *testing.T) {
	m, dim := newTestManager(t)
	req := Request{
		Dimension: dim, Mode: ModeTop, Style: render.StyleTerrain,
		Pos: mcmath.TilePos{Zoom: m.Cfg.TopBaseZoom, X: 0, Y: 0},
	}
	data, err := m.Tile(context.Background(), req, PriorityUser)
	if err != nil {
		t.Fatalf("Tile: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty tile bytes")
	}
}

// waitForPrefetch gives Tile()'s fire-and-forget neighbour prefetch
// (prefetchNeighbours, PERF_PLAN.md §5.3) time to finish before a test reads
// m.generated or checks store contents, so those background renders are not
// mistaken for -- or don't race with -- the specific behaviour under test.
// The demo world renders synchronously and in-process with no real I/O, so
// this margin is generous, not a source of flakiness.
func waitForPrefetch(t *testing.T) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
}

func TestTileGeneratesIsoAndServesFromCacheOnSecondCall(t *testing.T) {
	m, dim := newTestManager(t)
	req := Request{
		Dimension: dim, Mode: ModeIso, Style: render.StyleTerrain,
		Pos: mcmath.TilePos{Zoom: m.Cfg.IsoBaseZoom, X: 0, Y: 0},
	}
	ctx := context.Background()

	first, err := m.Tile(ctx, req, PriorityUser)
	if err != nil {
		t.Fatalf("Tile: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("expected non-empty tile bytes")
	}
	waitForPrefetch(t)
	settled := m.generated.Load()
	if settled < 1 {
		t.Fatalf("generated = %d, want at least 1 after the first request", settled)
	}

	second, err := m.Tile(ctx, req, PriorityUser)
	if err != nil {
		t.Fatalf("Tile (cached): %v", err)
	}
	if string(second) != string(first) {
		t.Fatal("second request returned different bytes than the first for an unchanged tile")
	}
	waitForPrefetch(t)
	if got := m.generated.Load(); got != settled {
		t.Fatalf("generated = %d after a repeat request (was %d before it), want unchanged (should be a cache hit, "+
			"and its neighbours already warm from the first request's own prefetch)", got, settled)
	}
}

// TestHeightBandLearnsFromRealRenders is a black-box check of PERF_PLAN.md
// §4.1: after one direct isometric render over real terrain, the dimension's
// learned height band must be persisted and observed, so later tiles assemble
// a narrower window than the dimension's full MinY..MaxY instead of paying
// for -64..320 (or the demo world's equivalent) forever.
func TestHeightBandLearnsFromRealRenders(t *testing.T) {
	m, dim := newTestManager(t)
	ctx := context.Background()

	req := Request{
		Dimension: dim, Mode: ModeIso, Style: render.StyleTerrain,
		// Zoom 0,0 in the demo world sits well within its generated radius,
		// so this tile is real terrain, not unexplored void.
		Pos: mcmath.TilePos{Zoom: m.Cfg.IsoBaseZoom, X: 0, Y: 0},
	}
	if _, err := m.Tile(ctx, req, PriorityUser); err != nil {
		t.Fatalf("Tile: %v", err)
	}

	raw, err := m.Store.Get(ctx, heightBandKey(dim))
	if err != nil {
		t.Fatalf("expected the height band to be persisted after a real render: %v", err)
	}
	var band heightBandData
	if err := json.Unmarshal(raw, &band); err != nil {
		t.Fatalf("unmarshal height band: %v", err)
	}
	if !band.Observed {
		t.Fatal("height band was persisted but not marked observed")
	}
	if band.Hi < band.Lo {
		t.Fatalf("height band is inverted: lo=%d hi=%d", band.Lo, band.Hi)
	}

	dimInfo, ok := m.Provider.Dimension(ctx, dim)
	if !ok {
		t.Fatal("dimension not found")
	}
	if band.Lo < dimInfo.MinY || band.Hi > dimInfo.MaxY {
		t.Fatalf("learned band [%d,%d] falls outside the dimension's own range [%d,%d]",
			band.Lo, band.Hi, dimInfo.MinY, dimInfo.MaxY)
	}
}

// TestIsoSurfaceWindowClampsToSlice is a manager-level check of PERF_PLAN.md
// §4.3: a sliced request's surface window must stop at the slice ceiling
// rather than fetching the dimension's full height range, and doing so must
// not turn a real tile into an error or an empty response.
func TestIsoSurfaceWindowClampsToSlice(t *testing.T) {
	m, dim := newTestManager(t)
	ctx := context.Background()
	dimInfo, ok := m.Provider.Dimension(ctx, dim)
	if !ok {
		t.Fatal("dimension not found")
	}

	req := Request{
		Dimension: dim, Mode: ModeIso, Style: render.StyleTerrain,
		Pos:    mcmath.TilePos{Zoom: m.Cfg.IsoBaseZoom, X: 0, Y: 0},
		Sliced: true, SliceY: dimInfo.MinY + 20,
	}
	lo, hi := m.isoWindow(ctx, req, dimInfo)
	if hi > req.SliceY {
		t.Fatalf("surface window hi=%d exceeds the slice ceiling %d", hi, req.SliceY)
	}
	if lo > hi {
		t.Fatalf("surface window is inverted: lo=%d hi=%d", lo, hi)
	}

	if _, err := m.Tile(ctx, req, PriorityUser); err != nil {
		t.Fatalf("sliced Tile: %v", err)
	}
}

// TestSlicedTilesArePersisted is a regression test for PERF_PLAN.md §5.1:
// sliced tiles used to live in memory only, so every first visit to a level
// paid the full render forever. A sliced tile must now land in the store
// like any other variant, and a second request -- even with the in-memory
// cache cleared, so it can only be served from disk -- must be a cache hit
// rather than a second render.
func TestSlicedTilesArePersisted(t *testing.T) {
	m, dim := newTestManager(t)
	ctx := context.Background()
	req := Request{
		Dimension: dim, Mode: ModeIso, Style: render.StyleTerrain,
		Pos:    mcmath.TilePos{Zoom: m.Cfg.IsoBaseZoom, X: 0, Y: 0},
		Sliced: true, SliceY: 80,
	}

	if _, err := m.Tile(ctx, req, PriorityUser); err != nil {
		t.Fatalf("Tile: %v", err)
	}
	if _, err := m.Store.Get(ctx, req.storeKey(m.Cfg.Format)); err != nil {
		t.Fatalf("sliced tile was not persisted to the store: %v", err)
	}
	waitForPrefetch(t)
	settled := m.generated.Load()
	if settled < 1 {
		t.Fatalf("generated = %d, want at least 1 after the first request", settled)
	}

	// Force the second request to come from disk, not the memory cache, so
	// this actually proves store persistence rather than just memory caching
	// (which sliced tiles already had before this feature existed).
	m.memory.Remove(req.cacheKey())

	if _, err := m.Tile(ctx, req, PriorityUser); err != nil {
		t.Fatalf("Tile (repeat, memory cleared): %v", err)
	}
	waitForPrefetch(t)
	if got := m.generated.Load(); got != settled {
		t.Fatalf("generated = %d after a repeat sliced request (was %d before it), want unchanged (should be a store hit)", got, settled)
	}
}

// TestSlicedTileKeyDoesNotCollideWithUnslicedOrOtherLevels guards the exact
// pitfall PERF_PLAN.md §5.1 calls out by name: a store key that does not
// distinguish every variant it can hold silently corrupts the pyramid for
// everyone. Camera, slice flag and slice level must all be part of the key.
func TestSlicedTileKeyDoesNotCollideWithUnslicedOrOtherLevels(t *testing.T) {
	base := Request{
		Dimension: "minecraft:overworld", Mode: ModeIso, Style: render.StyleTerrain,
		Pos: mcmath.TilePos{Zoom: 8, X: 0, Y: 0},
	}
	unsliced := base
	slicedA := base
	slicedA.Sliced, slicedA.SliceY = true, 64
	slicedB := base
	slicedB.Sliced, slicedB.SliceY = true, 80

	keys := []cache.Key{unsliced.storeKey(FormatWebP), slicedA.storeKey(FormatWebP), slicedB.storeKey(FormatWebP)}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] == keys[j] {
				t.Fatalf("store keys %d and %d collide: %+v", i, j, keys[i])
			}
		}
	}
}

// TestSliceVariantsAreEvicted guards the other half of §5.1: unbounded slice
// persistence would multiply the pyramid by however many levels a user has
// ever visited, so the least-recently-used level's tiles must actually be
// deleted once a (dimension, mode, camera) family exceeds MaxSliceVariants.
func TestSliceVariantsAreEvicted(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSliceVariants = 2
	m, dim := newTestManagerWithConfig(t, cfg)
	ctx := context.Background()

	reqAt := func(y int) Request {
		return Request{
			Dimension: dim, Mode: ModeIso, Style: render.StyleTerrain,
			Pos:    mcmath.TilePos{Zoom: m.Cfg.IsoBaseZoom, X: 0, Y: 0},
			Sliced: true, SliceY: y,
		}
	}

	levels := []int{60, 80, 100} // three levels, budget for two
	for _, y := range levels {
		if _, err := m.Tile(ctx, reqAt(y), PriorityUser); err != nil {
			t.Fatalf("Tile(y=%d): %v", y, err)
		}
		// Let this level's own background neighbour prefetch settle before
		// moving to the next one, so a straggling prefetch write for this
		// level cannot re-touch it after a later level's touch and upset the
		// recency order this test depends on.
		waitForPrefetch(t)
	}

	if _, err := m.Store.Get(ctx, reqAt(60).storeKey(m.Cfg.Format)); !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("least-recently-used level (60) was not evicted: err=%v", err)
	}
	for _, y := range []int{80, 100} {
		if _, err := m.Store.Get(ctx, reqAt(y).storeKey(m.Cfg.Format)); err != nil {
			t.Fatalf("level %d should still be on disk: %v", y, err)
		}
	}
}

// TestSliceVariantReadTouchRefreshesRecency confirms that revisiting a level
// through cache hits alone -- never rewriting it -- still counts as use, so
// an actively-visited level is not evicted just because it was never
// rendered again after its first visit.
func TestSliceVariantReadTouchRefreshesRecency(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSliceVariants = 2
	m, dim := newTestManagerWithConfig(t, cfg)
	ctx := context.Background()

	reqAt := func(y int) Request {
		return Request{
			Dimension: dim, Mode: ModeIso, Style: render.StyleTerrain,
			Pos:    mcmath.TilePos{Zoom: m.Cfg.IsoBaseZoom, X: 0, Y: 0},
			Sliced: true, SliceY: y,
		}
	}

	// Each step waits out its own background neighbour prefetch (PERF_PLAN.md
	// §5.3) before the next one starts, so a straggling async touch for a
	// level can never land after a later step and upset the exact recency
	// order this test depends on.
	if _, err := m.Tile(ctx, reqAt(60), PriorityUser); err != nil {
		t.Fatalf("Tile(y=60): %v", err)
	}
	waitForPrefetch(t)
	if _, err := m.Tile(ctx, reqAt(80), PriorityUser); err != nil {
		t.Fatalf("Tile(y=80): %v", err)
	}
	waitForPrefetch(t)
	// Revisit 60 -- forced to come from disk, not memory, so this is
	// specifically exercising the read-side touch -- which must count as
	// using it, moving it ahead of 80 in recency.
	m.memory.Remove(reqAt(60).cacheKey())
	if _, err := m.Tile(ctx, reqAt(60), PriorityUser); err != nil {
		t.Fatalf("Tile(y=60, revisit): %v", err)
	}
	waitForPrefetch(t)
	// A third distinct level now pushes the family over budget; 80, not 60,
	// is the one that has not been touched since, so it must be evicted.
	if _, err := m.Tile(ctx, reqAt(100), PriorityUser); err != nil {
		t.Fatalf("Tile(y=100): %v", err)
	}
	waitForPrefetch(t)

	if _, err := m.Store.Get(ctx, reqAt(80).storeKey(m.Cfg.Format)); !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("level 80 should have been evicted (least recently touched): err=%v", err)
	}
	if _, err := m.Store.Get(ctx, reqAt(60).storeKey(m.Cfg.Format)); err != nil {
		t.Fatalf("level 60 should still be on disk (touched more recently via a read): %v", err)
	}
}
