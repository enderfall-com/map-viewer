package tiles

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Priority orders competing tile work.
type Priority int

const (
	// PriorityUser is work a browser is actively waiting on. Always first.
	PriorityUser Priority = iota
	// PriorityDirty is regeneration triggered by a world change, which should
	// land quickly but never ahead of somebody staring at a blank tile.
	PriorityDirty
	// PriorityBackground is bulk pre-generation, which must never starve the
	// other two.
	PriorityBackground
	numPriorities
)

// String names a priority for logs and metrics.
func (p Priority) String() string {
	switch p {
	case PriorityUser:
		return "user"
	case PriorityDirty:
		return "dirty"
	default:
		return "background"
	}
}

// ErrQueueFull reports that the scheduler is saturated. Callers should shed the
// request rather than let the queue grow without bound.
var ErrQueueFull = errors.New("tile queue full")

// ErrShutdown reports that the scheduler has stopped.
var ErrShutdown = errors.New("tile scheduler stopped")

// Scheduler runs tile work on a bounded worker pool with strict priority.
//
// # Why a hand-rolled queue
//
// Buffered channels cannot express "always take user work first", and spawning
// a goroutine per tile would let a pre-generation sweep of a million tiles
// create a million goroutines and exhaust memory. A fixed pool draining a
// priority-ordered queue bounds both concurrency and backlog, which is the
// difference between a server that degrades gracefully under load and one that
// falls over.
//
// # Why jobs never wait on other jobs
//
// Building a parent tile needs its children. If a job could block waiting for
// another job, a full pool of parents waiting on unscheduled children would
// deadlock. Instead a job that needs a child renders it inline, on its own
// goroutine. Pool slots are therefore never held by a blocked worker.
type Scheduler struct {
	mu     sync.Mutex
	cond   *sync.Cond
	queues [numPriorities][]*job
	closed bool

	maxQueued int
	workers   int

	// pending deduplicates by key so the same tile is not queued twice.
	pending map[string]*job

	wg sync.WaitGroup

	queued    atomic.Int64
	active    atomic.Int64
	completed atomic.Int64
	dropped   atomic.Int64
	waitNanos atomic.Int64
}

type job struct {
	key      string
	priority Priority
	run      func() (any, error)
	enqueued time.Time
	done     chan struct{}
	// result and err hold the outcome of run(), visible to every caller that
	// joined this job -- not just the one who submitted it.
	result any
	err    error
	// waiters counts callers blocked on this job, used only for metrics.
	waiters atomic.Int32
}

// NewScheduler starts a worker pool. workers bounds concurrency; maxQueued
// bounds the backlog.
func NewScheduler(workers, maxQueued int) *Scheduler {
	if workers < 1 {
		workers = 1
	}
	if maxQueued < workers {
		maxQueued = workers * 16
	}
	s := &Scheduler{
		maxQueued: maxQueued,
		workers:   workers,
		pending:   make(map[string]*job, maxQueued),
	}
	s.cond = sync.NewCond(&s.mu)
	s.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go s.worker()
	}
	return s
}

// Submit enqueues work under a deduplication key and waits for it to finish,
// returning whatever run() returned.
//
// If the same key is already queued or running, the caller joins the existing
// job instead of duplicating it and receives the same result once the job
// completes -- joining is transparent to the result, not just the wait. A job
// already queued at a lower priority is promoted when a higher-priority caller
// arrives, so a background tile that a user suddenly needs jumps the queue
// rather than waiting behind the sweep.
//
// run is invoked at most once per key no matter how many callers join; it
// must not depend on any particular caller's context, since ctx here only
// governs how long *this* caller is willing to wait, not the job's lifetime.
func (s *Scheduler) Submit(ctx context.Context, key string, p Priority, run func() (any, error)) (any, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrShutdown
	}

	if existing, ok := s.pending[key]; ok {
		if p < existing.priority {
			s.promoteLocked(existing, p)
		}
		existing.waiters.Add(1)
		done := existing.done
		s.mu.Unlock()
		if err := waitFor(ctx, done); err != nil {
			return nil, err
		}
		return existing.result, existing.err
	}

	// Background work is shed first when saturated; user work is allowed to
	// push right up to the limit.
	limit := s.maxQueued
	if p == PriorityBackground {
		limit = s.maxQueued / 2
	}
	if len(s.pending) >= limit {
		s.mu.Unlock()
		s.dropped.Add(1)
		return nil, ErrQueueFull
	}

	j := &job{
		key: key, priority: p, run: run,
		enqueued: time.Now(), done: make(chan struct{}),
	}
	j.waiters.Add(1)
	s.pending[key] = j
	s.queues[p] = append(s.queues[p], j)
	s.queued.Add(1)
	s.mu.Unlock()

	s.cond.Signal()
	if err := waitFor(ctx, j.done); err != nil {
		return nil, err
	}
	return j.result, j.err
}

// promoteLocked moves a queued job to a higher priority band. A job already
// running is left alone; its priority no longer matters.
func (s *Scheduler) promoteLocked(j *job, p Priority) {
	old := s.queues[j.priority]
	for i, q := range old {
		if q == j {
			s.queues[j.priority] = append(old[:i], old[i+1:]...)
			j.priority = p
			s.queues[p] = append(s.queues[p], j)
			s.cond.Signal()
			return
		}
	}
	// Not found means it is already executing.
	j.priority = p
}

// waitFor blocks until the job finishes or the caller's context is cancelled.
// A cancelled caller does not cancel the job: other callers may still want it,
// and a half-rendered tile helps nobody.
func waitFor(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// worker drains the queue in strict priority order.
func (s *Scheduler) worker() {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		for {
			if s.closed {
				s.mu.Unlock()
				return
			}
			if j := s.popLocked(); j != nil {
				s.mu.Unlock()
				s.execute(j)
				break
			}
			s.cond.Wait()
		}
	}
}

// popLocked returns the highest-priority queued job, or nil.
func (s *Scheduler) popLocked() *job {
	for p := Priority(0); p < numPriorities; p++ {
		if len(s.queues[p]) > 0 {
			j := s.queues[p][0]
			s.queues[p] = s.queues[p][1:]
			return j
		}
	}
	return nil
}

// execute runs a job and releases everyone waiting on it, even if it panics.
// A panic in one tile render must not take down the pool and stall the map.
func (s *Scheduler) execute(j *job) {
	s.queued.Add(-1)
	s.active.Add(1)
	s.waitNanos.Add(int64(time.Since(j.enqueued)))

	defer func() {
		// Recover so a bad chunk cannot kill a worker permanently. Without this,
		// every caller joined on the job -- not just the one whose data
		// triggered the panic -- would hang until their context expired instead
		// of seeing an error.
		if r := recover(); r != nil {
			j.result, j.err = nil, fmt.Errorf("tile job panicked: %v", r)
		}

		s.mu.Lock()
		delete(s.pending, j.key)
		s.mu.Unlock()

		close(j.done)
		s.active.Add(-1)
		s.completed.Add(1)
	}()

	j.result, j.err = j.run()
}

// Close stops the pool and waits for in-flight work to finish.
func (s *Scheduler) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.cond.Broadcast()
	s.wg.Wait()
}

// SchedulerStats reports pool behaviour for metrics endpoints.
type SchedulerStats struct {
	Workers   int     `json:"workers"`
	Queued    int64   `json:"queued"`
	Active    int64   `json:"active"`
	Completed int64   `json:"completed"`
	Dropped   int64   `json:"dropped"`
	MaxQueued int     `json:"maxQueued"`
	AvgWaitMs float64 `json:"avgWaitMs"`
}

// Stats snapshots the scheduler counters.
func (s *Scheduler) Stats() SchedulerStats {
	completed := s.completed.Load()
	avg := 0.0
	if completed > 0 {
		avg = float64(s.waitNanos.Load()) / float64(completed) / 1e6
	}
	return SchedulerStats{
		Workers:   s.workers,
		Queued:    s.queued.Load(),
		Active:    s.active.Load(),
		Completed: completed,
		Dropped:   s.dropped.Load(),
		MaxQueued: s.maxQueued,
		AvgWaitMs: avg,
	}
}
