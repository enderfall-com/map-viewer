package tiles

import (
	"container/list"
	"context"
	"log/slog"
	"sync"

	"github.com/enderfall/minecraft-map/backend/internal/cache"
)

// sliceFamily identifies one group of Y-slice variants that compete for the
// same retention budget: every slice level of one (dimension, mode, camera)
// combination. Style is deliberately not part of this -- a "terrain" and a
// "biome" render of the same slice level should count as one visit, not two,
// since they were both caused by the same drag of the slider.
type sliceFamily struct {
	dimension, mode, camera string
}

// sliceVariants bounds how many distinct Y-slice levels' tiles are kept on
// disk per sliceFamily, evicting the least-recently-used level's own tiles
// once a family holds more than the configured limit (PERF_PLAN.md §5.1).
//
// Unbounded slice persistence would multiply a world's tile pyramid by
// however many distinct levels a user has ever dragged the slider through --
// with a 4-level step across a 384-block dimension that is up to ~96 levels,
// each a full zoom pyramid. This is the "keep only the N most recently used
// ... on a background sweep" bound the plan calls for, done inline at write
// time rather than as a separate sweep: the manager already knows exactly
// which store keys it writes for a given level, so there is nothing to
// discover later and no need to give the store interface directory or
// prefix-listing semantics it does not otherwise need.
//
// In-memory only -- it does not survive a restart. A stale slice variant left
// on disk from a previous process is harmless (it is just an ordinary
// cache-hit if revisited, or otherwise inert), so this is a bound on ongoing
// growth within one process's lifetime, not a guarantee of a hard disk cap.
type sliceVariants struct {
	mu    sync.Mutex
	limit int

	// order tracks recency per family: front is least-recently-used.
	order map[sliceFamily]*list.List
	elems map[sliceFamily]map[int]*list.Element
	// keys holds every store key written for a given family+level, so an
	// eviction knows exactly what to delete.
	keys map[sliceFamily]map[int]map[cache.Key]struct{}
}

// newSliceVariants creates a tracker. limit <= 0 falls back to
// DefaultConfig's MaxSliceVariants rather than being unbounded.
func newSliceVariants(limit int) *sliceVariants {
	if limit <= 0 {
		limit = DefaultConfig().MaxSliceVariants
	}
	return &sliceVariants{
		limit: limit,
		order: make(map[sliceFamily]*list.List),
		elems: make(map[sliceFamily]map[int]*list.Element),
		keys:  make(map[sliceFamily]map[int]map[cache.Key]struct{}),
	}
}

// touch records that k was just written for one (family, sliceY), marks that
// level most-recently-used, and -- if the family now holds more levels than
// the configured limit -- evicts the least-recently-used level's tiles from
// store. The actual deletes happen outside the lock, after it decides what
// to remove, so a slow store does not stall unrelated tile generation.
func (v *sliceVariants) touch(ctx context.Context, store cache.Store, log *slog.Logger, fam sliceFamily, sliceY int, k cache.Key) {
	v.mu.Lock()
	l, ok := v.order[fam]
	if !ok {
		l = list.New()
		v.order[fam] = l
		v.elems[fam] = make(map[int]*list.Element)
		v.keys[fam] = make(map[int]map[cache.Key]struct{})
	}
	if e, ok := v.elems[fam][sliceY]; ok {
		l.MoveToBack(e)
	} else {
		v.elems[fam][sliceY] = l.PushBack(sliceY)
	}
	if v.keys[fam][sliceY] == nil {
		v.keys[fam][sliceY] = make(map[cache.Key]struct{})
	}
	v.keys[fam][sliceY][k] = struct{}{}

	var evictKeys map[cache.Key]struct{}
	if l.Len() > v.limit {
		front := l.Front()
		evictY := front.Value.(int)
		l.Remove(front)
		delete(v.elems[fam], evictY)
		evictKeys = v.keys[fam][evictY]
		delete(v.keys[fam], evictY)
	}
	v.mu.Unlock()

	for dk := range evictKeys {
		if err := store.Delete(ctx, dk); err != nil && log != nil {
			log.Warn("slice variant eviction failed", "key", dk, "error", err)
		}
	}
}
