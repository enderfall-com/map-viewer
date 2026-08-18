package cache

import (
	"container/list"
	"sync"
	"sync/atomic"
)

// LRU is a bounded, concurrency-safe least-recently-used cache.
//
// Capacity is expressed in caller-defined cost units rather than entry counts,
// so a chunk-surface cache can be bounded by bytes while a tile cache is
// bounded by tiles. This is what keeps a hundred-gigabyte world inside a fixed
// memory budget: the working set is whatever the current viewport touches, and
// everything else is evicted.
type LRU[K comparable, V any] struct {
	mu       sync.Mutex
	capacity int64
	cost     int64
	ll       *list.List
	items    map[K]*list.Element

	// onEvict releases resources held by a value that is leaving the cache.
	// Without it, a cache of anything holding an operating-system handle -- an
	// open region file, most importantly -- would leak one handle per eviction
	// and eventually exhaust the process's descriptor limit on a large world.
	onEvict func(K, V)

	hits   atomic.Int64
	misses atomic.Int64
	evicts atomic.Int64
}

type entry[K comparable, V any] struct {
	key   K
	value V
	cost  int64
}

// NewLRU creates a cache bounded to the given total cost. A capacity of zero or
// less disables caching entirely rather than growing without bound.
func NewLRU[K comparable, V any](capacity int64) *LRU[K, V] {
	return &LRU[K, V]{
		capacity: capacity,
		ll:       list.New(),
		items:    make(map[K]*list.Element),
	}
}

// SetOnEvict registers a release callback invoked whenever a value leaves the
// cache, whether by eviction, replacement, explicit removal or Clear. It is
// called without the cache lock held, so the callback may re-enter the cache
// safely.
func (c *LRU[K, V]) SetOnEvict(fn func(K, V)) {
	c.mu.Lock()
	c.onEvict = fn
	c.mu.Unlock()
}

// Get returns a value and promotes it to most-recently-used.
func (c *LRU[K, V]) Get(key K) (V, bool) {
	var zero V
	if c.capacity <= 0 {
		c.misses.Add(1)
		return zero, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.misses.Add(1)
		return zero, false
	}
	c.ll.MoveToFront(el)
	c.hits.Add(1)
	return el.Value.(*entry[K, V]).value, true
}

// Put inserts or replaces a value, evicting least-recently-used entries until
// the total cost fits.
func (c *LRU[K, V]) Put(key K, value V, cost int64) {
	if c.capacity <= 0 {
		return
	}
	if cost < 1 {
		cost = 1
	}
	c.mu.Lock()

	// Released values are collected under the lock and handed to the callback
	// after it is dropped, so a callback that closes a file cannot deadlock
	// against the cache it was called from.
	var released []entry[K, V]

	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry[K, V])
		if c.onEvict != nil {
			released = append(released, *e)
		}
		c.cost += cost - e.cost
		e.value, e.cost = value, cost
		c.ll.MoveToFront(el)
	} else {
		el := c.ll.PushFront(&entry[K, V]{key: key, value: value, cost: cost})
		c.items[key] = el
		c.cost += cost
	}

	for c.cost > c.capacity {
		back := c.ll.Back()
		if back == nil {
			break
		}
		e := back.Value.(*entry[K, V])
		c.ll.Remove(back)
		delete(c.items, e.key)
		c.cost -= e.cost
		c.evicts.Add(1)
		if c.onEvict != nil {
			released = append(released, *e)
		}
	}
	fn := c.onEvict
	c.mu.Unlock()

	for _, e := range released {
		fn(e.key, e.value)
	}
}

// Remove drops a key if present.
func (c *LRU[K, V]) Remove(key K) {
	if c.capacity <= 0 {
		return
	}
	c.mu.Lock()
	var released *entry[K, V]
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry[K, V])
		c.ll.Remove(el)
		delete(c.items, e.key)
		c.cost -= e.cost
		if c.onEvict != nil {
			cp := *e
			released = &cp
		}
	}
	fn := c.onEvict
	c.mu.Unlock()

	if released != nil {
		fn(released.key, released.value)
	}
}

// Clear empties the cache.
func (c *LRU[K, V]) Clear() {
	c.mu.Lock()
	var released []entry[K, V]
	if c.onEvict != nil {
		for _, el := range c.items {
			released = append(released, *el.Value.(*entry[K, V]))
		}
	}
	c.ll.Init()
	c.items = make(map[K]*list.Element)
	c.cost = 0
	fn := c.onEvict
	c.mu.Unlock()

	for _, e := range released {
		fn(e.key, e.value)
	}
}

// Stats reports cache behaviour for metrics.
type Stats struct {
	Entries   int     `json:"entries"`
	Cost      int64   `json:"cost"`
	Capacity  int64   `json:"capacity"`
	Hits      int64   `json:"hits"`
	Misses    int64   `json:"misses"`
	Evictions int64   `json:"evictions"`
	HitRatio  float64 `json:"hitRatio"`
}

// Stats returns a snapshot of cache counters.
func (c *LRU[K, V]) Stats() Stats {
	c.mu.Lock()
	entries, cost := len(c.items), c.cost
	c.mu.Unlock()

	h, m := c.hits.Load(), c.misses.Load()
	ratio := 0.0
	if h+m > 0 {
		ratio = float64(h) / float64(h+m)
	}
	return Stats{
		Entries: entries, Cost: cost, Capacity: c.capacity,
		Hits: h, Misses: m, Evictions: c.evicts.Load(), HitRatio: ratio,
	}
}

// ---------------------------------------------------------------------------
// Single-flight
// ---------------------------------------------------------------------------

// Group deduplicates concurrent work for the same key.
//
// Without this, a browser opening a fresh viewport would ask for the same
// parent tile from a dozen connections at once and the server would render it a
// dozen times. With it, the first caller renders and the rest wait on the same
// result -- the request-deduplication requirement, enforced server-side rather
// than trusted to the client.
type Group[K comparable, V any] struct {
	mu    sync.Mutex
	calls map[K]*call[V]
}

type call[V any] struct {
	wg  sync.WaitGroup
	val V
	err error
}

// NewGroup creates a single-flight group.
func NewGroup[K comparable, V any]() *Group[K, V] {
	return &Group[K, V]{calls: make(map[K]*call[V])}
}

// Do runs fn for key, ensuring only one execution is in flight at a time.
// Concurrent callers for the same key share the first call's result.
func (g *Group[K, V]) Do(key K, fn func() (V, error)) (V, error, bool) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := new(call[V])
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()

	return c.val, c.err, false
}

// InFlight reports how many distinct keys are currently executing.
func (g *Group[K, V]) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}
