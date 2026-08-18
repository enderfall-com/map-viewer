package world

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/enderfall/minecraft-map/backend/internal/cache"
	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
)

// chunkKey identifies a cached chunk surface.
type chunkKey struct {
	dim  string
	x, z int
}

// Cached wraps a Provider with a bounded chunk-surface cache and request
// deduplication.
//
// This is the layer that makes tile generation affordable. Neighbouring tiles
// overlap heavily -- an isometric tile in particular pulls in columns from far
// outside its own footprint -- so the same chunk is asked for many times in
// quick succession. Without caching, each request would re-read and re-inflate
// a region file; with it, the second and subsequent reads are a map lookup.
//
// Deduplication matters just as much: when eight workers start on adjacent
// tiles simultaneously they request the same chunks at the same moment. The
// single-flight group ensures the expensive decode happens once and the other
// seven wait for it rather than each doing their own.
type Cached struct {
	inner  Provider
	lru    *cache.LRU[chunkKey, *ChunkSurface]
	flight *cache.Group[chunkKey, *ChunkSurface]

	// absent remembers chunks known not to exist, so repeatedly panning over
	// unexplored terrain does not repeatedly hit the disk. It is bounded the
	// same way as the positive cache.
	absent *cache.LRU[chunkKey, struct{}]

	// Voxel data is cached separately from the chunk surface: it is optional
	// (only present when the inner provider implements VolumeProvider), and
	// its per-chunk size varies far more than a fixed-width ChunkSurface does
	// (ISO_VOXEL_PLAN.md §3.1), so it gets its own budget rather than sharing
	// lru's sizing assumptions.
	voxelLRU    *cache.LRU[chunkKey, *ChunkVoxels]
	voxelFlight *cache.Group[chunkKey, *ChunkVoxels]
	voxelAbsent *cache.LRU[chunkKey, struct{}]
	// voxels is the inner provider re-asserted as VolumeProvider, or nil when
	// it does not implement it (e.g. demo.World).
	voxels VolumeProvider

	mu     sync.RWMutex
	dims   []DimensionInfo
	byID   map[string]DimensionInfo
	loaded bool
}

// chunkSurfaceCost is the approximate heap cost of one cached chunk surface, in
// bytes: six fixed arrays of 256 entries plus overhead.
const chunkSurfaceCost = ColumnCount*(2+2+2+2+1+1) + 128

// nominalChunkVoxelsCost estimates one cached chunk's voxel slab size for
// sizing the absent-chunk memo. Unlike chunkSurfaceCost, actual voxel slab
// size varies a lot with per-chunk Depth, so this just picks a representative
// mid-size chunk (~24 layers) -- it only bounds how many absent markers are
// kept, not correctness.
const nominalChunkVoxelsCost = ColumnCount*24*(2+1) + 128

// NewCached wraps a provider. capacityBytes bounds the chunk-surface cache; a
// typical value of 256 MB holds well over a hundred thousand chunks.
// voxelCapacityBytes bounds the separate voxel cache in the same way; it is
// unused if inner does not implement VolumeProvider.
func NewCached(inner Provider, capacityBytes, voxelCapacityBytes int64) *Cached {
	if capacityBytes <= 0 {
		capacityBytes = 256 << 20
	}
	if voxelCapacityBytes <= 0 {
		voxelCapacityBytes = 512 << 20
	}
	c := &Cached{
		inner:       inner,
		lru:         cache.NewLRU[chunkKey, *ChunkSurface](capacityBytes),
		flight:      cache.NewGroup[chunkKey, *ChunkSurface](),
		absent:      cache.NewLRU[chunkKey, struct{}](capacityBytes / chunkSurfaceCost),
		voxelLRU:    cache.NewLRU[chunkKey, *ChunkVoxels](voxelCapacityBytes),
		voxelFlight: cache.NewGroup[chunkKey, *ChunkVoxels](),
		voxelAbsent: cache.NewLRU[chunkKey, struct{}](voxelCapacityBytes / nominalChunkVoxelsCost),
	}
	c.voxels, _ = inner.(VolumeProvider)
	return c
}

// Dimensions implements Provider, caching the discovery result.
func (c *Cached) Dimensions(ctx context.Context) ([]DimensionInfo, error) {
	c.mu.RLock()
	if c.loaded {
		out := make([]DimensionInfo, len(c.dims))
		copy(out, c.dims)
		c.mu.RUnlock()
		return out, nil
	}
	c.mu.RUnlock()

	dims, err := c.inner.Dimensions(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.dims = dims
	c.byID = make(map[string]DimensionInfo, len(dims))
	for _, d := range dims {
		c.byID[d.ID] = d
	}
	c.loaded = true
	c.mu.Unlock()

	out := make([]DimensionInfo, len(dims))
	copy(out, dims)
	return out, nil
}

// Dimension implements Provider.
func (c *Cached) Dimension(ctx context.Context, id string) (DimensionInfo, bool) {
	c.mu.RLock()
	loaded := c.loaded
	d, ok := c.byID[id]
	c.mu.RUnlock()
	if loaded {
		return d, ok
	}
	if _, err := c.Dimensions(ctx); err != nil {
		return DimensionInfo{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	d, ok = c.byID[id]
	return d, ok
}

// ChunkSurface implements Provider with caching and deduplication.
func (c *Cached) ChunkSurface(ctx context.Context, dimension string, pos mcmath.ChunkPos) (*ChunkSurface, error) {
	key := chunkKey{dim: dimension, x: pos.X, z: pos.Z}

	if cs, ok := c.lru.Get(key); ok {
		return cs, nil
	}
	if _, ok := c.absent.Get(key); ok {
		return nil, ErrChunkAbsent
	}

	cs, err, _ := c.flight.Do(key, func() (*ChunkSurface, error) {
		return c.inner.ChunkSurface(ctx, dimension, pos)
	})
	if err != nil {
		if errors.Is(err, ErrChunkAbsent) {
			c.absent.Put(key, struct{}{}, 1)
		}
		return nil, err
	}
	if cs != nil {
		c.lru.Put(key, cs, chunkSurfaceCost)
	}
	return cs, nil
}

// ChunkVoxels implements VolumeProvider with caching and deduplication,
// mirroring ChunkSurface exactly. It returns ErrVoxelsUnsupported when the
// inner provider does not implement VolumeProvider (e.g. demo.World) or the
// feature is otherwise unavailable, which callers treat like ErrChunkAbsent
// and fall back to the heightmap iso path.
func (c *Cached) ChunkVoxels(ctx context.Context, dimension string, pos mcmath.ChunkPos) (*ChunkVoxels, error) {
	if c.voxels == nil {
		return nil, ErrVoxelsUnsupported
	}
	key := chunkKey{dim: dimension, x: pos.X, z: pos.Z}

	if cv, ok := c.voxelLRU.Get(key); ok {
		return cv, nil
	}
	if _, ok := c.voxelAbsent.Get(key); ok {
		return nil, ErrChunkAbsent
	}

	cv, err, _ := c.voxelFlight.Do(key, func() (*ChunkVoxels, error) {
		return c.voxels.ChunkVoxels(ctx, dimension, pos)
	})
	if err != nil {
		if errors.Is(err, ErrChunkAbsent) {
			c.voxelAbsent.Put(key, struct{}{}, 1)
		}
		return nil, err
	}
	if cv != nil {
		c.voxelLRU.Put(key, cv, chunkVoxelsCost(cv))
	}
	return cv, nil
}

// chunkVoxelsCost is the approximate heap cost of one cached chunk voxel
// slab: two backing arrays of Depth*ColumnCount entries plus overhead.
func chunkVoxelsCost(cv *ChunkVoxels) int64 {
	return int64(cv.Depth)*ColumnCount*(2+1) + 128
}

// Invalidate drops a chunk from the cache so the next read sees fresh data.
// This is what a live world update calls before regenerating tiles. It
// clears both the chunk-surface and the voxel cache -- forgetting the voxel
// half would mean live world edits update top-down tiles but leave iso tiles
// showing stale geometry indefinitely.
func (c *Cached) Invalidate(dimension string, pos mcmath.ChunkPos) {
	key := chunkKey{dim: dimension, x: pos.X, z: pos.Z}
	c.lru.Remove(key)
	c.absent.Remove(key)
	c.voxelLRU.Remove(key)
	c.voxelAbsent.Remove(key)
}

// InvalidateAll clears every cached chunk, e.g. after a world reload.
func (c *Cached) InvalidateAll() {
	c.lru.Clear()
	c.absent.Clear()
	c.voxelLRU.Clear()
	c.voxelAbsent.Clear()
}

// Stats reports chunk cache behaviour.
func (c *Cached) Stats() cache.Stats { return c.lru.Stats() }

// Inner returns the wrapped provider.
func (c *Cached) Inner() Provider { return c.inner }

// SupportsVoxels reports whether the wrapped provider can supply voxel data
// at all. Cached always exposes ChunkVoxels (returning ErrVoxelsUnsupported
// when this is false), so callers that want to skip the voxel path entirely
// -- rather than pay for a call that will only ever fail -- check this first.
func (c *Cached) SupportsVoxels() bool { return c.voxels != nil }

// String describes the cache for logs.
func (c *Cached) String() string {
	s := c.lru.Stats()
	return fmt.Sprintf("chunk cache %d entries, %.1f%% hit", s.Entries, s.HitRatio*100)
}
