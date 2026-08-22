package tiles

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/enderfall/minecraft-map/backend/internal/cache"
)

// isoHeightMargin pads the observed height band so a build growing a little
// beyond what has been rendered so far does not immediately fall outside the
// tightened window. PERF_PLAN.md §4.1: "keep a small margin ... so a newly
// built tower does not pop."
const isoHeightMargin = 32

// heightBandData is the observed, persisted shape of one dimension's learned
// isometric render window (PERF_PLAN.md §4.1) and voxel slab depth (§4.2).
//
// It only ever widens at runtime (see Manager.observeHeightRange/
// observeSlabDepth): a single render seeing terrain outside the current band
// is evidence the band was too tight, never evidence it was too wide, so
// shrinking it is a deliberate operator action (delete the persisted file),
// not something a render does automatically.
type heightBandData struct {
	Observed     bool `json:"observed"`
	Lo           int  `json:"lo"`
	Hi           int  `json:"hi"`
	MaxSlabDepth int  `json:"maxSlabDepth"`
}

// heightBand is one dimension's in-memory band, lazily hydrated from the
// store on first use so a restart resumes from what was already learned
// instead of paying the full-range cost again.
type heightBand struct {
	mu     sync.Mutex
	data   heightBandData
	loaded bool
}

// heightBandKey is where a dimension's band is persisted. It reuses the tile
// Store rather than a bespoke file so the filesystem, and any future
// object-store backend, both just work: Mode "_meta" cannot collide with the
// "top"/"iso" tile modes, and there is exactly one key per dimension.
func heightBandKey(dim string) cache.Key {
	return cache.Key{
		Dimension: cache.SafeID(dim),
		Mode:      "_meta",
		Style:     "heightband",
		Format:    "json",
	}
}

// heightBand returns the (lazily loaded) band for a dimension. Never nil.
func (m *Manager) heightBand(ctx context.Context, dim string) *heightBand {
	m.bandsMu.Lock()
	b, ok := m.bands[dim]
	if !ok {
		b = &heightBand{}
		m.bands[dim] = b
	}
	m.bandsMu.Unlock()

	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.loaded {
		b.loaded = true
		if raw, err := m.Store.Get(ctx, heightBandKey(dim)); err == nil {
			var d heightBandData
			if json.Unmarshal(raw, &d) == nil {
				b.data = d
			}
		}
	}
	return b
}

// window returns the tightened iso render band for a dimension, clamped to
// [dimMinY, dimMaxY]. ok is false when nothing has been observed yet, in
// which case callers fall back to the dimension's full range -- exactly
// today's behaviour, and always correct since it can't exclude terrain.
func (b *heightBand) window(dimMinY, dimMaxY int) (lo, hi int, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.data.Observed {
		return 0, 0, false
	}
	lo = max(dimMinY, b.data.Lo-isoHeightMargin)
	hi = min(dimMaxY, b.data.Hi+isoHeightMargin)
	if hi < lo {
		hi = lo
	}
	return lo, hi, true
}

// slabDepth returns the observed max per-chunk voxel slab depth, or ceiling
// if nothing has been observed yet (PERF_PLAN.md §4.2).
func (b *heightBand) slabDepth(ceiling int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.data.MaxSlabDepth <= 0 || b.data.MaxSlabDepth > ceiling {
		return ceiling
	}
	return b.data.MaxSlabDepth
}

// observeHeightRange widens a dimension's tracked surface height range to
// cover [lo,hi], persisting only when it actually grew.
//
// The persist happens while still holding b.mu, not after releasing it: many
// tile workers can observe a widening band at once, and letting them race to
// Store.Put the same destination key independently made FSStore's
// temp-file-then-rename step fail intermittently on Windows (concurrent
// renames onto one path can lose to a sharing violation). A dimension's band
// changes rarely once warmed up, so serialising the write costs nothing that
// matters.
func (m *Manager) observeHeightRange(ctx context.Context, dim string, lo, hi int) {
	b := m.heightBand(ctx, dim)
	b.mu.Lock()
	defer b.mu.Unlock()
	changed := false
	switch {
	case !b.data.Observed:
		b.data.Observed = true
		b.data.Lo, b.data.Hi = lo, hi
		changed = true
	default:
		if lo < b.data.Lo {
			b.data.Lo = lo
			changed = true
		}
		if hi > b.data.Hi {
			b.data.Hi = hi
			changed = true
		}
	}
	if changed {
		m.persistHeightBand(ctx, dim, b.data)
	}
}

// observeSlabDepth widens a dimension's tracked max voxel slab depth to at
// least depth, persisting only when it actually grew. depth <= 0 is a no-op,
// so callers on the heightmap-only path (no volume ever assembled) need not
// special-case the call. See observeHeightRange for why the persist happens
// under b.mu.
func (m *Manager) observeSlabDepth(ctx context.Context, dim string, depth int) {
	if depth <= 0 {
		return
	}
	b := m.heightBand(ctx, dim)
	b.mu.Lock()
	defer b.mu.Unlock()
	if depth > b.data.MaxSlabDepth {
		b.data.MaxSlabDepth = depth
		m.persistHeightBand(ctx, dim, b.data)
	}
}

func (m *Manager) persistHeightBand(ctx context.Context, dim string, data heightBandData) {
	enc, err := json.Marshal(data)
	if err != nil {
		return
	}
	if err := m.Store.Put(ctx, heightBandKey(dim), enc); err != nil {
		m.Log.Warn("height band persist failed", "dimension", dim, "error", err)
	}
}
