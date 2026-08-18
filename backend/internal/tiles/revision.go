package tiles

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
)

// Revisions issues the cache-busting version numbers that appear in tile URLs.
//
// # Why revisions live in the URL and not in storage
//
// Tiles are served with a one-year immutable cache header, which is what lets a
// CDN and every browser keep them forever and makes panning around explored
// terrain essentially free. That is only safe if a changed tile gets a
// different URL. The revision provides that: regenerating a tile bumps its
// revision, the client learns the new number over the WebSocket, and it
// requests a URL it has never seen before. The old URL is simply never
// requested again, so nothing has to be purged from any cache anywhere.
//
// Storage keeps one current image per tile. Keeping every revision would
// multiply a large world's tile store without benefit.
type Revisions struct {
	counter atomic.Uint64

	mu sync.RWMutex
	// perTile holds revisions only for tiles that have actually changed since
	// startup. Everything else uses the dimension baseline, so this map stays
	// proportional to recent edits rather than to world size.
	perTile map[TileID]uint64
	perDim  map[string]uint64
}

// TileID identifies a tile for revision purposes, independent of style and
// format: a terrain change invalidates every style of that tile at once.
type TileID struct {
	Dimension string
	Mode      Mode
	Zoom      int
	X, Y      int
}

// NewRevisions creates a revision tracker seeded from the current time, so
// restarting the server never reissues a revision a client already has cached
// with different content.
func NewRevisions() *Revisions {
	r := &Revisions{
		perTile: make(map[TileID]uint64),
		perDim:  make(map[string]uint64),
	}
	r.counter.Store(uint64(time.Now().Unix()))
	return r
}

// Next allocates a fresh globally-increasing revision.
func (r *Revisions) Next() uint64 { return r.counter.Add(1) }

// Current returns the latest issued revision.
func (r *Revisions) Current() uint64 { return r.counter.Load() }

// Baseline returns a dimension's revision, used for tiles that have not changed
// individually since startup.
func (r *Revisions) Baseline(dimension string) uint64 {
	r.mu.RLock()
	v, ok := r.perDim[dimension]
	r.mu.RUnlock()
	if ok {
		return v
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.perDim[dimension]; ok {
		return v
	}
	v = r.counter.Load()
	r.perDim[dimension] = v
	return v
}

// For returns the revision a client should request for a tile.
func (r *Revisions) For(id TileID) uint64 {
	r.mu.RLock()
	v, ok := r.perTile[id]
	r.mu.RUnlock()
	if ok {
		return v
	}
	return r.Baseline(id.Dimension)
}

// Bump assigns a new revision to a tile and returns it.
func (r *Revisions) Bump(id TileID) uint64 {
	v := r.Next()
	r.mu.Lock()
	r.perTile[id] = v
	r.mu.Unlock()
	return v
}

// BumpDimension advances a whole dimension's baseline, invalidating every tile
// in it at once. Used when configuration that affects rendering changes.
func (r *Revisions) BumpDimension(dimension string) uint64 {
	v := r.Next()
	r.mu.Lock()
	r.perDim[dimension] = v
	// Per-tile overrides are now stale relative to the new baseline.
	for id := range r.perTile {
		if id.Dimension == dimension {
			delete(r.perTile, id)
		}
	}
	r.mu.Unlock()
	return v
}

// Changed returns every tile whose revision was individually bumped, for
// clients reconnecting after a dropped WebSocket.
func (r *Revisions) Changed(dimension string) map[TileID]uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[TileID]uint64)
	for id, v := range r.perTile {
		if dimension == "" || id.Dimension == dimension {
			out[id] = v
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Dirty chunk tracking
// ---------------------------------------------------------------------------

// DirtySet accumulates changed chunks and collapses them into the set of tiles
// that actually need regenerating.
//
// A single player mining a block can touch the same chunk hundreds of times a
// second. Regenerating a tile per event would be ruinous, so changes are
// coalesced over a short window and the affected tile set is deduplicated
// before any work is scheduled. One busy chunk therefore costs one tile
// regeneration per window, not one per block.
type DirtySet struct {
	mu     sync.Mutex
	chunks map[dirtyKey]time.Time
}

type dirtyKey struct {
	dimension string
	x, z      int
}

// NewDirtySet creates an empty set.
func NewDirtySet() *DirtySet {
	return &DirtySet{chunks: make(map[dirtyKey]time.Time)}
}

// Mark records that a chunk changed.
func (d *DirtySet) Mark(dimension string, pos mcmath.ChunkPos) {
	d.mu.Lock()
	d.chunks[dirtyKey{dimension, pos.X, pos.Z}] = time.Now()
	d.mu.Unlock()
}

// Len reports how many distinct chunks are waiting.
func (d *DirtySet) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.chunks)
}

// DirtyChunk is one coalesced change.
type DirtyChunk struct {
	Dimension string
	Pos       mcmath.ChunkPos
}

// Drain removes and returns every chunk that has been stable for at least
// settle, leaving chunks still being actively edited for the next pass. This
// keeps a player who is mid-build from causing a regeneration storm.
func (d *DirtySet) Drain(settle time.Duration) []DirtyChunk {
	cutoff := time.Now().Add(-settle)
	d.mu.Lock()
	defer d.mu.Unlock()

	var out []DirtyChunk
	for k, at := range d.chunks {
		if at.After(cutoff) {
			continue
		}
		out = append(out, DirtyChunk{
			Dimension: k.dimension,
			Pos:       mcmath.ChunkPos{X: k.x, Z: k.z},
		})
		delete(d.chunks, k)
	}
	return out
}

// AffectedTiles returns every tile that must be regenerated for a set of
// changed chunks, ordered deepest zoom first.
//
// The ordering is essential: parents are built by compositing children, so a
// parent regenerated before its children would composite stale images. Emitting
// deepest-first guarantees that by the time a level is processed, everything it
// reads has already been refreshed.
//
// Deduplication across chunks matters too. Sixty-four changed chunks inside one
// zoom-6 tile produce one zoom-6 regeneration, not sixty-four.
func AffectedTiles(chunks []DirtyChunk, modes []Mode, minZoom, maxZoom int, isoMinY, isoMaxY int) []TileID {
	seen := make(map[TileID]struct{})
	byZoom := make(map[int][]TileID)

	add := func(id TileID) {
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		byZoom[id.Zoom] = append(byZoom[id.Zoom], id)
	}

	for _, c := range chunks {
		bounds := mcmath.ChunkBounds(c.Pos)
		for _, mode := range modes {
			switch mode {
			case ModeIso:
				// An isometric tile shows columns from far outside its own
				// footprint, so a changed chunk dirties every tile its whole
				// projected sprite range touches -- not just the one tile its
				// ground footprint lands in.
				proj := mcmath.NewIsoProjection(mcmath.DefaultCamera)
				ib := proj.IsoFootprintOfBlocks(bounds, isoMinY, isoMaxY)
				for z := maxZoom; z >= minZoom; z-- {
					lo := mcmath.IsoTileAt(ib.MinU, ib.MinV, z)
					hi := mcmath.IsoTileAt(ib.MaxU, ib.MaxV, z)
					for ty := lo.Y; ty <= hi.Y; ty++ {
						for tx := lo.X; tx <= hi.X; tx++ {
							add(TileID{c.Dimension, ModeIso, z, tx, ty})
						}
					}
				}
			default:
				for z := maxZoom; z >= minZoom; z-- {
					for _, tp := range mcmath.TilesCovering(bounds, z) {
						add(TileID{c.Dimension, ModeTop, z, tp.X, tp.Y})
					}
				}
			}
		}
	}

	out := make([]TileID, 0, len(seen))
	for z := maxZoom; z >= minZoom; z-- {
		out = append(out, byZoom[z]...)
	}
	return out
}
