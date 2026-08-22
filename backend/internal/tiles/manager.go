package tiles

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	xwebp "golang.org/x/image/webp"

	"github.com/enderfall/minecraft-map/backend/internal/blocks"
	"github.com/enderfall/minecraft-map/backend/internal/cache"
	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/render"
	"github.com/enderfall/minecraft-map/backend/internal/textures"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

// Mode selects the projection a tile is rendered in.
type Mode string

const (
	// ModeTop is the plan view.
	ModeTop Mode = "top"
	// ModeIso is the isometric view.
	ModeIso Mode = "iso"
)

// ParseMode resolves a mode name.
func ParseMode(s string) (Mode, bool) {
	switch Mode(strings.ToLower(s)) {
	case ModeTop, "":
		return ModeTop, true
	case ModeIso:
		return ModeIso, true
	}
	return ModeTop, false
}

// Request identifies one tile to produce.
type Request struct {
	Dimension string // canonical id, e.g. "minecraft:overworld"
	Mode      Mode
	Style     render.Style
	Pos       mcmath.TilePos
	// Camera is the corner an isometric tile is viewed from. Ignored in
	// top-down mode, which has no viewing corner. Its zero value is
	// mcmath.DefaultCamera, so a Request built without one renders the
	// default view.
	Camera mcmath.IsoCamera

	// Sliced and SliceY cut the isometric view off above a Y level, exposing
	// caves and interiors. Sliced tiles are written to the tile store like
	// any other variant (PERF_PLAN.md §5.1) -- variant() folds the slice
	// level into the storage path, so a level the user drags back to is a
	// lookup, not a re-render. Unbounded slice variants would multiply a
	// world's pyramid by however many distinct levels have ever been
	// visited; Manager.sliceVariants bounds that by evicting the
	// least-recently-used level's tiles once a (dimension, mode, camera)
	// family holds more than Cfg.MaxSliceVariants of them.
	Sliced bool
	SliceY int
}

// variant is the storage-path and cache-key component distinguishing tiles of
// the same area rendered differently within one mode. Empty for anything that
// matches the historical defaults, so those tiles keep their existing paths
// and keys rather than being regenerated under new ones.
//
// Every new field that affects imagery must be folded in here, not just
// checked ad hoc at each store call site: childImage and generate both derive
// their store key from this, so a variant left out here is a variant two
// different images can silently collide under (PERF_PLAN.md §5.1's own
// pitfall, hit once already by the camera corner before slice existed).
func (r Request) variant() string {
	if r.Mode != ModeIso {
		return ""
	}
	v := ""
	if r.Camera != mcmath.DefaultCamera {
		v = r.Camera.String()
	}
	if r.Sliced {
		if v != "" {
			v += "_"
		}
		v += "y" + strconv.Itoa(r.SliceY)
	}
	return v
}

// storeKey builds the storage key for a request.
func (r Request) storeKey(format Format) cache.Key {
	return cache.Key{
		Dimension: cache.SafeID(r.Dimension),
		Mode:      string(r.Mode),
		Variant:   r.variant(),
		Style:     string(r.Style),
		Zoom:      r.Pos.Zoom,
		X:         r.Pos.X,
		Y:         r.Pos.Y,
		Format:    format.Ext(),
	}
}

// cacheKey is the in-memory and deduplication key.
func (r Request) cacheKey() string {
	var b strings.Builder
	b.Grow(64)
	b.WriteString(r.Dimension)
	b.WriteByte('|')
	b.WriteString(string(r.Mode))
	if v := r.variant(); v != "" {
		b.WriteByte('_')
		b.WriteString(v)
	}
	b.WriteByte('|')
	b.WriteString(string(r.Style))
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(r.Pos.Zoom))
	b.WriteByte('/')
	b.WriteString(strconv.Itoa(r.Pos.X))
	b.WriteByte('/')
	b.WriteString(strconv.Itoa(r.Pos.Y))
	return b.String()
}

// TileID returns the style-independent identity used for revisions.
func (r Request) TileID() TileID {
	return TileID{Dimension: r.Dimension, Mode: r.Mode, Zoom: r.Pos.Zoom, X: r.Pos.X, Y: r.Pos.Y}
}

// Config tunes tile generation.
type Config struct {
	// MinZoom and MaxZoom bound the pyramid that is served.
	MinZoom, MaxZoom int
	// TopBaseZoom is the zoom at which top-down tiles are rendered from world
	// data. Below it tiles are composited from children; above it the client
	// magnifies with nearest-neighbour filtering, which is exactly right for
	// block terrain and costs no storage at all.
	TopBaseZoom int
	// IsoBaseZoom is the equivalent for isometric tiles. It cannot go below
	// render.MinDirectZoom, where the diamond lattice stops being
	// pixel-alignable.
	IsoBaseZoom int
	// MaxCompositeDepth bounds how many pyramid levels a single on-demand
	// request may build recursively. Without it, asking for a zoom-0 tile of an
	// ungenerated world would synchronously render millions of children.
	MaxCompositeDepth int
	// MemoryTileBytes bounds the decoded-tile memory cache.
	MemoryTileBytes int64
	// Format is the encoding for stored tiles.
	Format Format
	// StoreBlankTiles controls whether fully-unexplored tiles are written to
	// storage. Skipping them saves an enormous amount of space on sparse worlds
	// at the cost of re-deciding they are blank on each request.
	StoreBlankTiles bool
	// IsoEdgeSkirt is how many blocks of side face the isometric renderer
	// draws where the neighbouring column is unexplored. <= 0 uses render.NewIso's
	// default.
	IsoEdgeSkirt int
	// IsoVoxel switches the isometric renderer to real per-column voxel data
	// (ISO_VOXEL_PLAN.md) whenever the active provider supports it. false
	// keeps the heightmap extrusion renderer, byte-identical to before this
	// feature existed.
	IsoVoxel bool
	// IsoVoxelMaxDepth bounds how far below a tile's lowest surface height
	// the voxel window is allowed to reach (§4.5's window tightening). Must
	// be at least as large as the real per-chunk slab depth can ever be, or
	// the basement skirt fallback simply does a little more work -- it is a
	// safety margin, not a hard requirement.
	IsoVoxelMaxDepth int
	// MaxSliceVariants bounds how many distinct Y-slice levels' tiles are
	// kept on disk per (dimension, mode, camera) family (PERF_PLAN.md §5.1).
	// <= 0 uses a sensible default rather than disabling the bound, since an
	// unbounded value is never what's wanted here.
	MaxSliceVariants int
}

// DefaultConfig returns sensible production defaults.
func DefaultConfig() Config {
	return Config{
		MinZoom:           0,
		MaxZoom:           10,
		TopBaseZoom:       mcmath.BaseZoom,
		IsoBaseZoom:       render.MinDirectZoom + 1, // 8
		MaxCompositeDepth: 3,
		MemoryTileBytes:   192 << 20,
		Format:            FormatWebP,
		StoreBlankTiles:   false,
		IsoEdgeSkirt:      4,
		IsoVoxel:          false,
		IsoVoxelMaxDepth:  64,
		MaxSliceVariants:  12,
	}
}

// Manager produces, caches and invalidates tiles.
type Manager struct {
	Provider *world.Cached
	Store    cache.Store
	Cfg      Config
	Rev      *Revisions
	Dirty    *DirtySet
	Log      *slog.Logger

	enc    *Encoder
	sched  *Scheduler
	memory *cache.LRU[string, []byte]
	// images caches decoded tiles, which is what makes building a pyramid level
	// cheap: a parent reads four children that are usually still in memory from
	// having just been generated.
	images *cache.LRU[string, *image.NRGBA]

	registry *blocks.Registry
	biomes   *blocks.Biomes
	textures *textures.Set // nil when no texture source is configured
	opts     render.Options

	// jobCtx scopes rendering work, not any individual caller. A tile job may
	// be shared by many concurrent requests via the scheduler's dedup key;
	// tying its execution to whichever caller happened to submit first would
	// mean that caller navigating away silently aborts the render for
	// everyone else still waiting on it. jobCancel is invoked from Close so a
	// server shutdown still stops in-flight renders promptly.
	jobCtx    context.Context
	jobCancel context.CancelFunc

	generated atomic.Int64
	served    atomic.Int64
	failures  atomic.Int64
	renderNs  atomic.Int64

	// voxelUnsupportedLogged ensures the "isoVoxel enabled but unsupported"
	// notice (config.go §6: "log once at info ... never fatal") fires once
	// per process, not once per tile.
	voxelUnsupportedLogged atomic.Bool

	// bands holds each dimension's learned isometric render window
	// (PERF_PLAN.md §4.1/§4.2, heightband.go), keyed by dimension ID.
	bandsMu sync.Mutex
	bands   map[string]*heightBand

	// slices bounds how many Y-slice levels' tiles stay on disk per
	// dimension/mode/camera (PERF_PLAN.md §5.1, slicevariants.go).
	slices *sliceVariants
}

// NewManager wires up a tile manager.
func NewManager(
	provider *world.Cached,
	store cache.Store,
	reg *blocks.Registry,
	bio *blocks.Biomes,
	opts render.Options,
	cfg Config,
	workers, maxQueued int,
	log *slog.Logger,
) *Manager {
	if cfg.IsoBaseZoom < render.MinDirectZoom {
		cfg.IsoBaseZoom = render.MinDirectZoom
	}
	if cfg.MaxCompositeDepth < 1 {
		cfg.MaxCompositeDepth = 1
	}
	if log == nil {
		log = slog.Default()
	}
	jobCtx, jobCancel := context.WithCancel(context.Background())
	return &Manager{
		Provider:  provider,
		Store:     store,
		Cfg:       cfg,
		Rev:       NewRevisions(),
		Dirty:     NewDirtySet(),
		Log:       log,
		enc:       NewEncoder(cfg.Format),
		sched:     NewScheduler(workers, maxQueued),
		memory:    cache.NewLRU[string, []byte](cfg.MemoryTileBytes),
		images:    cache.NewLRU[string, *image.NRGBA](cfg.MemoryTileBytes),
		registry:  reg,
		biomes:    bio,
		opts:      opts,
		jobCtx:    jobCtx,
		jobCancel: jobCancel,
		bands:     make(map[string]*heightBand),
		slices:    newSliceVariants(cfg.MaxSliceVariants),
	}
}

// SetTextures attaches a resolved texture set, so subsequent renders sample
// real block textures instead of flat colours wherever a block resolves.
// Optional: a Manager with no texture set attached renders exactly as before.
func (m *Manager) SetTextures(t *textures.Set) { m.textures = t }

// Close stops the worker pool.
func (m *Manager) Close() {
	m.jobCancel()
	m.sched.Close()
}

// Scheduler exposes the pool for metrics.
func (m *Manager) Scheduler() *Scheduler { return m.sched }

// shader builds a shader for a style. Shaders are cheap value-like objects, so
// constructing one per render avoids any shared mutable state between workers.
func (m *Manager) shader(style render.Style, dim world.DimensionInfo) *render.Shader {
	s := &render.Shader{
		Blocks:   m.registry,
		Biomes:   m.biomes,
		Textures: m.textures,
		Style:    style,
		Opts:     m.opts,
	}
	// Scale the height style to the dimension's actual build range so a
	// -512..128 mining dimension is as readable as a vanilla overworld.
	s.HeightLo, s.HeightHi = dim.MinY, dim.MaxY
	return s
}

// canRenderDirect reports whether a zoom is rendered from world data.
func (m *Manager) canRenderDirect(mode Mode, zoom int) bool {
	if mode == ModeIso {
		return zoom >= max(m.Cfg.IsoBaseZoom, render.MinDirectZoom)
	}
	return zoom >= m.Cfg.TopBaseZoom
}

// Tile returns a tile's encoded bytes, generating it if necessary.
//
// The fast path is a memory-cache hit, then a store hit; only a miss enters the
// scheduler. Because the scheduler deduplicates by key, a hundred simultaneous
// requests for the same missing tile cause exactly one render.
func (m *Manager) Tile(ctx context.Context, req Request, prio Priority) ([]byte, error) {
	m.served.Add(1)
	// A real browser request is a signal to warm the tiles just outside the
	// viewport too, so the next pan is more likely to land on one already
	// generated (PERF_PLAN.md §5.3). Gated on PriorityUser specifically so
	// this never chains into itself: the prefetch jobs it starts run at
	// PriorityBackground, which fails this check, so a pan warms one ring of
	// neighbours, not an ever-expanding flood of them.
	if prio == PriorityUser {
		m.prefetchNeighbours(req)
	}
	key := req.cacheKey()

	if data, ok := m.memory.Get(key); ok {
		return data, nil
	}
	sk := req.storeKey(m.Cfg.Format)
	if data, err := m.Store.Get(ctx, sk); err == nil {
		m.memory.Put(key, data, int64(len(data)))
		m.touchSlice(ctx, req, sk)
		return data, nil
	} else if !errors.Is(err, cache.ErrNotFound) {
		m.Log.Warn("tile store read failed", "key", key, "error", err)
	}

	result, err := m.sched.Submit(ctx, key, prio, func() (any, error) {
		// Re-check inside the job: a concurrent caller may have produced it
		// while this one was queued.
		if data, ok := m.memory.Get(key); ok {
			return data, nil
		}
		// Use the job's own context, not this particular caller's: the render
		// is shared by everyone who joined this key, and must not abort just
		// because the first caller to submit it disconnected.
		return m.generate(m.jobCtx, req, 0)
	})
	if err != nil {
		// Do not count this caller's own cancellation, or the scheduler
		// shedding load, as a render failure -- only an actual generate error.
		if ctx.Err() == nil && !errors.Is(err, ErrQueueFull) && !errors.Is(err, ErrShutdown) {
			m.failures.Add(1)
		}
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	return result.([]byte), nil
}

// neighbourOffsets are the four tiles sharing an edge with a given tile --
// the ones a pan in any single direction lands on next.
var neighbourOffsets = [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}

// prefetchNeighbours opportunistically warms the tiles immediately around
// req at background priority, so a pan in any direction is more likely to
// land on a tile that is already generated (PERF_PLAN.md §5.3). Fire-and-
// forget: callers never wait on this, and a neighbour already warm or
// already being produced for someone else costs nothing extra -- Tile()'s
// own cache checks and the scheduler's dedup-by-key both short-circuit it.
func (m *Manager) prefetchNeighbours(req Request) {
	if req.Pos.Zoom < m.Cfg.MinZoom || req.Pos.Zoom > m.Cfg.MaxZoom {
		return
	}
	for _, d := range neighbourOffsets {
		nreq := req
		nreq.Pos.X += d[0]
		nreq.Pos.Y += d[1]
		go func(r Request) {
			// Errors (a full queue, a shut-down manager) are exactly what
			// background priority exists to absorb silently -- a failed
			// prefetch just means that neighbour stays cold, same as if it
			// had never been attempted.
			_, _ = m.Tile(m.jobCtx, r, PriorityBackground)
		}(nreq)
	}
}

// renderStats collects PERF_PLAN.md §3 (Phase 0) per-stage timings for one
// tile's cold render, threaded through renderImage/renderDirect/attachVolume
// and logged by generate. All fields stay zero for a composited tile, whose
// cost is dominated by fetching children rather than by these stages.
type renderStats struct {
	surfaceMs, volumeMs, renderMs, encodeMs float64
	chunkCount                              int
	// chunkErrs counts chunks that failed to READ during this render --
	// never chunks that are merely ungenerated, which are an ordinary
	// unexplored result and not an error (world.Assemble only reports
	// non-ErrChunkAbsent failures). A tile rendered over a failed read is
	// missing real terrain it should have drawn, so generate refuses to
	// persist it: see storeTile's caller.
	chunkErrs int
}

// generate renders, encodes and stores one tile.
func (m *Manager) generate(ctx context.Context, req Request, depth int) ([]byte, error) {
	start := time.Now()
	st := &renderStats{}

	img, err := m.renderImage(ctx, req, depth, st)
	if err != nil {
		return nil, err
	}

	u := m.blankColor(req.Style)
	blank := render.IsBlank(img, u.R, u.G, u.B, u.A)

	encStart := time.Now()
	data, err := m.enc.Encode(img)
	st.encodeMs = msSince(encStart)
	if err != nil {
		return nil, err
	}

	key := req.cacheKey()
	m.memory.Put(key, data, int64(len(data)))
	m.images.Put(key, img, int64(len(img.Pix)))

	// A tile whose chunks failed to read is missing terrain that really
	// exists, and the tile store has no expiry: persisting it would serve
	// that hole to everyone, forever, with nothing to ever rebuild it (the
	// server reads from the store, and the generate CLI skips tiles that
	// already exist). Region files are routinely locked or mid-write while
	// Minecraft is running, so this is a transient, expected failure -- the
	// tile is still returned to this caller and memory-cached, it just never
	// becomes the permanent copy on disk, so the next request re-renders it.
	if st.chunkErrs > 0 {
		m.Log.Warn("not persisting a tile rendered with chunk read errors; it will be re-rendered on the next request",
			"dimension", req.Dimension, "mode", string(req.Mode),
			"tile_z", req.Pos.Zoom, "tile_x", req.Pos.X, "tile_y", req.Pos.Y,
			"chunk_errors", st.chunkErrs)
	} else if !blank || m.Cfg.StoreBlankTiles {
		m.storeTile(ctx, req, key, data)
	}

	dur := time.Since(start)
	m.generated.Add(1)
	m.renderNs.Add(int64(dur))
	m.Log.Debug("tile generated",
		"dimension", req.Dimension, "mode", string(req.Mode), "style", string(req.Style),
		"tile_z", req.Pos.Zoom, "tile_x", req.Pos.X, "tile_y", req.Pos.Y,
		"duration_ms", dur.Milliseconds(), "bytes", len(data), "blank", blank, "depth", depth,
		"surface_ms", st.surfaceMs, "volume_ms", st.volumeMs,
		"render_ms", st.renderMs, "encode_ms", st.encodeMs, "chunks", st.chunkCount)

	return data, nil
}

// msSince returns the elapsed time since start in fractional milliseconds,
// precise enough for stage timings well under a millisecond.
func msSince(start time.Time) float64 { return float64(time.Since(start)) / float64(time.Millisecond) }

// storeTile persists a rendered tile's bytes to the store, and -- for a
// sliced request -- records the write with sliceVariants so a level that
// stops being visited is eventually evicted rather than accumulating forever
// (PERF_PLAN.md §5.1). logKey is only for the failure log line, so the
// caller's own cacheKey() need not be recomputed here.
func (m *Manager) storeTile(ctx context.Context, req Request, logKey string, data []byte) {
	sk := req.storeKey(m.Cfg.Format)
	if err := m.Store.Put(ctx, sk, data); err != nil {
		// A storage failure must not fail the request: the tile is rendered
		// and can still be served, it just will not be cached on disk.
		m.Log.Error("tile store write failed", "key", logKey, "error", err)
		return
	}
	m.touchSlice(ctx, req, sk)
}

// touchSlice marks a sliced request's level most-recently-used, whether the
// tile was just written or served from an existing store hit -- a level
// visited only through cache hits, never rewritten, must not look cold to
// the LRU and get evicted out from under active use. A no-op for an unsliced
// request.
func (m *Manager) touchSlice(ctx context.Context, req Request, sk cache.Key) {
	if !req.Sliced {
		return
	}
	fam := sliceFamily{dimension: req.Dimension, mode: string(req.Mode), camera: req.Camera.String()}
	m.slices.touch(ctx, m.Store, m.Log, fam, req.SliceY, sk)
}

// renderImage produces a tile image, either directly from world data or by
// compositing four children.
func (m *Manager) renderImage(ctx context.Context, req Request, depth int, st *renderStats) (*image.NRGBA, error) {
	dim, ok := m.Provider.Dimension(ctx, req.Dimension)
	if !ok {
		return nil, fmt.Errorf("unknown dimension %q", req.Dimension)
	}
	if m.canRenderDirect(req.Mode, req.Pos.Zoom) {
		return m.renderDirect(ctx, req, dim, st)
	}
	return m.renderComposite(ctx, req, depth, st)
}

// isoWindow returns the Y range to assemble a tile's surface over: the
// dimension's learned band (PERF_PLAN.md §4.1) tightened to a slice's ceiling
// when one is active (§4.3 -- the surface window and the volume window must
// agree, since everything visible under a slice sits at or below the cut),
// falling back to the dimension's full range when nothing has been observed
// yet.
func (m *Manager) isoWindow(ctx context.Context, req Request, dim world.DimensionInfo) (lo, hi int) {
	lo, hi = dim.MinY, dim.MaxY
	if ol, oh, ok := m.heightBand(ctx, req.Dimension).window(dim.MinY, dim.MaxY); ok {
		lo, hi = ol, oh
	}
	if req.Sliced && req.SliceY < hi {
		hi = req.SliceY
	}
	if hi < lo {
		hi = lo
	}
	return lo, hi
}

// renderDirect renders a tile from world data.
func (m *Manager) renderDirect(ctx context.Context, req Request, dim world.DimensionInfo, st *renderStats) (*image.NRGBA, error) {
	sh := m.shader(req.Style, dim)

	var bounds mcmath.BlockBounds
	var iso *render.Iso
	if req.Mode == ModeIso {
		iso = render.NewIso(sh, req.Camera, m.Cfg.IsoEdgeSkirt)
		iso.Sliced, iso.SliceY = req.Sliced, req.SliceY
		lo, hi := m.isoWindow(ctx, req, dim)
		bounds = iso.SurfaceBounds(req.Pos, lo, hi)
	} else {
		td := render.NewTopDown(sh)
		bounds = td.SurfaceBounds(req.Pos)
	}

	surfStart := time.Now()
	var chunkErrs int
	surf, err := world.Assemble(ctx, m.Provider, req.Dimension, bounds, dim.MinY, dim.MaxY,
		func(pos mcmath.ChunkPos, err error) {
			// One malformed chunk costs a blank 16x16 patch, never the tile.
			chunkErrs++
			if chunkErrs <= 3 {
				m.Log.Warn("chunk read failed",
					"dimension", req.Dimension, "chunk_x", pos.X, "chunk_z", pos.Z, "error", err)
			}
		})
	if err != nil {
		return nil, err
	}
	if st != nil {
		st.surfaceMs = msSince(surfStart)
		minCX, minCZ, maxCX, maxCZ := bounds.ChunkRange()
		st.chunkCount = (maxCX - minCX) * (maxCZ - minCZ)
		st.chunkErrs += chunkErrs
	}
	if chunkErrs > 0 {
		m.Log.Warn("tile rendered with chunk errors",
			"dimension", req.Dimension, "tile_z", req.Pos.Zoom,
			"tile_x", req.Pos.X, "tile_y", req.Pos.Y, "chunk_errors", chunkErrs)
	}

	// An entirely unexplored window needs no renderer at all.
	if !surf.AnyPresent() {
		return render.FillUnexplored(m.blankColor(req.Style)), nil
	}

	if req.Mode == ModeIso {
		// Feed this render's actual terrain extent back into the dimension's
		// learned band (PERF_PLAN.md §4.1) so later tiles -- anywhere in the
		// dimension -- can assemble a narrower window than the full range.
		if lo, hi, ok := surf.HeightRange(); ok {
			m.observeHeightRange(ctx, req.Dimension, lo, hi)
		}
		switch {
		case !m.Cfg.IsoVoxel:
			// Feature disabled: iso.Volume stays nil, byte-identical to
			// before this feature existed.
		case !m.Provider.SupportsVoxels():
			if m.voxelUnsupportedLogged.CompareAndSwap(false, true) {
				m.Log.Info("render.isoVoxel is enabled but the active world provider does not support voxel data; using the heightmap iso renderer")
			}
		default:
			volStart := time.Now()
			verr := m.attachVolume(ctx, req, dim, iso, surf, st)
			if st != nil {
				st.volumeMs = msSince(volStart)
			}
			if verr != nil {
				m.Log.Warn("voxel volume assembly failed, falling back to the heightmap iso renderer",
					"dimension", req.Dimension, "tile_z", req.Pos.Zoom,
					"tile_x", req.Pos.X, "tile_y", req.Pos.Y, "error", verr)
			}
		}
		renderStart := time.Now()
		img := iso.Render(req.Pos, surf)
		if st != nil {
			st.renderMs = msSince(renderStart)
		}
		return img, nil
	}
	renderStart := time.Now()
	img := render.NewTopDown(sh).Render(req.Pos, surf)
	if st != nil {
		st.renderMs = msSince(renderStart)
	}
	return img, nil
}

// attachVolume implements ISO_VOXEL_PLAN.md §4.5's mandatory window
// tightening. The Surface window above stays conservative -- it must, since
// adjacent tiles only agree on shared pixels because both derive their
// column set the same way from overlapping overscan -- but the voxel window
// is recomputed tightly from the surface's own actual height range: for
// terrain confined to a narrow band of a tall dimension, this is a 4x+
// reduction in columns visited versus using the dimension's full MinY..MaxY.
func (m *Manager) attachVolume(ctx context.Context, req Request, dim world.DimensionInfo, iso *render.Iso, surf *world.Surface, st *renderStats) error {
	lo, hi, ok := surf.HeightRange()
	if !ok {
		return nil // AnyPresent() was already checked by the caller; unreachable in practice
	}
	// Subtracting the dimension's observed max slab depth (PERF_PLAN.md §4.2)
	// covers the deepest a per-chunk slab has actually been seen to reach, so
	// the basement skirt fallback never has to activate for terrain still
	// within the tightened window. IsoVoxelMaxDepth remains the ceiling until
	// enough of the dimension has been rendered to learn a tighter real value.
	loEff := lo - m.heightBand(ctx, req.Dimension).slabDepth(m.Cfg.IsoVoxelMaxDepth)
	hiEff := hi
	// Under a Y slice the band stops at the cut. Elevation is what makes the
	// isometric footprint wide -- every Y level shifts terrain another block
	// along the view ray -- so a tile sliced at Y=64 through terrain reaching
	// Y=200 was loading the voxel slabs of a footprint sized for 136 levels of
	// terrain it then discarded. Those chunks cannot project into this tile at
	// all once everything above the cut is gone.
	if req.Sliced && req.SliceY < hiEff {
		hiEff = req.SliceY
	}
	// A slice below every surface in the window leaves only the columns whose
	// slabs reach under it; the band must stay non-empty to still cover them.
	if hiEff < loEff {
		hiEff = loEff
	}
	bounds := iso.Proj.WorldFootprint(mcmath.IsoTileBounds(req.Pos), loEff, hiEff).Expand(1)

	var chunkErrs int
	vol, err := world.AssembleVolume(ctx, m.Provider, req.Dimension, bounds, dim.MinY, dim.MaxY,
		func(pos mcmath.ChunkPos, err error) {
			chunkErrs++
			if chunkErrs <= 3 {
				m.Log.Warn("voxel chunk read failed",
					"dimension", req.Dimension, "chunk_x", pos.X, "chunk_z", pos.Z, "error", err)
			}
		})
	if err != nil {
		return err
	}
	if chunkErrs > 0 {
		// Counted the same as a surface read failure: a voxel slab that
		// failed to load leaves real geometry undrawn, which must not become
		// the permanent stored copy of this tile either.
		if st != nil {
			st.chunkErrs += chunkErrs
		}
		m.Log.Warn("tile's voxel volume assembled with chunk errors",
			"dimension", req.Dimension, "tile_z", req.Pos.Zoom,
			"tile_x", req.Pos.X, "tile_y", req.Pos.Y, "chunk_errors", chunkErrs)
	}
	iso.Volume = vol
	m.observeSlabDepth(ctx, req.Dimension, vol.MaxChunkDepth())
	return nil
}

// renderComposite builds a tile by downsampling its four children.
//
// Children already in storage are reused. Missing children are rendered inline,
// bounded by MaxCompositeDepth so a single request cannot cascade into an
// unbounded amount of work; beyond that depth a missing child is treated as
// unexplored and the tile is completed by the pre-generation sweep later.
// st, when set, accumulates the children's chunk read errors, so a parent
// built from an incomplete child is recognised as incomplete itself rather
// than being persisted as the permanent copy of that area.
func (m *Manager) renderComposite(ctx context.Context, req Request, depth int, st *renderStats) (*image.NRGBA, error) {
	var children [4]*image.NRGBA
	var blanks [4]bool

	for i, cp := range req.Pos.Children() {
		if cp.Zoom > m.Cfg.MaxZoom {
			continue
		}
		childReq := req
		childReq.Pos = cp

		img, err := m.childImage(ctx, childReq, depth, st)
		if err != nil {
			if !errors.Is(err, errTooDeep) {
				m.Log.Warn("child tile unavailable",
					"tile_z", cp.Zoom, "tile_x", cp.X, "tile_y", cp.Y, "error", err)
			}
			blanks[i] = true
			continue
		}
		children[i] = img
	}

	fill := render.FillUnexplored(m.blankColor(req.Style))
	return render.Composite(children, blanks, fill), nil
}

var errTooDeep = errors.New("composite depth limit reached")

// childImage fetches or renders one child tile as a decoded image. parentStats,
// when set, receives any chunk read errors this child hit, so an incomplete
// child makes its parent incomplete too.
func (m *Manager) childImage(ctx context.Context, req Request, depth int, parentStats *renderStats) (*image.NRGBA, error) {
	key := req.cacheKey()
	if img, ok := m.images.Get(key); ok {
		return img, nil
	}
	// The store key folds in the slice level (Request.variant()), so a
	// sliced child tile of one level can never be confused for another's or
	// for the unsliced tile -- same read/write path as any other variant.
	sk := req.storeKey(m.Cfg.Format)
	if data, err := m.Store.Get(ctx, sk); err == nil {
		img, derr := decodeTile(data)
		if derr == nil {
			m.images.Put(key, img, int64(len(img.Pix)))
			m.touchSlice(ctx, req, sk)
			return img, nil
		}
		m.Log.Warn("stored tile failed to decode, regenerating", "key", key, "error", derr)
	} else if !errors.Is(err, cache.ErrNotFound) {
		return nil, err
	}

	if depth+1 > m.Cfg.MaxCompositeDepth {
		return nil, errTooDeep
	}

	// A child gets its own stats purely to learn whether its chunks read
	// cleanly; a child rendered over a failed read must not be persisted
	// either, for exactly the reason generate gives.
	childStats := &renderStats{}
	img, err := m.renderImage(ctx, req, depth+1, childStats)
	if err != nil {
		return nil, err
	}
	if parentStats != nil {
		parentStats.chunkErrs += childStats.chunkErrs
	}
	// Persist the freshly rendered child so sibling parents and later requests
	// do not repeat the work -- sliced children included (PERF_PLAN.md §5.1).
	u := m.blankColor(req.Style)
	storable := childStats.chunkErrs == 0
	if storable && (!render.IsBlank(img, u.R, u.G, u.B, u.A) || m.Cfg.StoreBlankTiles) {
		if data, encErr := m.enc.Encode(img); encErr == nil {
			m.storeTile(ctx, req, key, data)
			m.memory.Put(key, data, int64(len(data)))
		}
	}
	m.images.Put(key, img, int64(len(img.Pix)))
	m.generated.Add(1)
	return img, nil
}

func (m *Manager) blankColor(style render.Style) color.NRGBA {
	if style == render.StyleContour {
		return color.NRGBA{}
	}
	return m.opts.UnexploredColor
}

// decodeTile decodes stored tile bytes back into an editable image.
func decodeTile(data []byte) (*image.NRGBA, error) {
	var img image.Image
	var err error
	// Sniff the container rather than trusting configuration, so a store that
	// changed format still reads back correctly.
	if len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		img, err = xwebp.Decode(bytes.NewReader(data))
	} else {
		img, _, err = image.Decode(bytes.NewReader(data))
	}
	if err != nil {
		return nil, err
	}
	if n, ok := img.(*image.NRGBA); ok && n.Rect.Dx() == mcmath.TileSize && n.Rect.Dy() == mcmath.TileSize {
		return n, nil
	}
	out := image.NewNRGBA(image.Rect(0, 0, mcmath.TileSize, mcmath.TileSize))
	b := img.Bounds()
	for y := 0; y < mcmath.TileSize && y < b.Dy(); y++ {
		for x := 0; x < mcmath.TileSize && x < b.Dx(); x++ {
			r, g, bb, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			o := out.PixOffset(x, y)
			out.Pix[o+0] = uint8(r >> 8)
			out.Pix[o+1] = uint8(g >> 8)
			out.Pix[o+2] = uint8(bb >> 8)
			out.Pix[o+3] = uint8(a >> 8)
		}
	}
	return out, nil
}

// Invalidate drops a tile from every cache layer and storage, and issues a new
// revision for it.
func (m *Manager) Invalidate(ctx context.Context, id TileID, styles []render.Style) uint64 {
	for _, style := range styles {
		req := Request{
			Dimension: id.Dimension, Mode: id.Mode, Style: style,
			Pos: mcmath.TilePos{Zoom: id.Zoom, X: id.X, Y: id.Y},
		}
		key := req.cacheKey()
		m.memory.Remove(key)
		m.images.Remove(key)
		if err := m.Store.Delete(ctx, req.storeKey(m.Cfg.Format)); err != nil {
			m.Log.Warn("tile delete failed", "key", key, "error", err)
		}
	}
	return m.Rev.Bump(id)
}

// Regenerate rebuilds a tile immediately at the given priority.
func (m *Manager) Regenerate(ctx context.Context, req Request, prio Priority) error {
	key := req.cacheKey()
	m.memory.Remove(key)
	m.images.Remove(key)
	// Use the job's own context so a caller that stops waiting does not abort
	// the regeneration for other callers -- or dirty-flush callers -- sharing
	// this key.
	_, err := m.sched.Submit(ctx, key+"|regen", prio, func() (any, error) {
		return m.generate(m.jobCtx, req, 0)
	})
	return err
}

// Stats reports tile pipeline metrics.
type Stats struct {
	Generated   int64          `json:"tilesGenerated"`
	Served      int64          `json:"tilesServed"`
	Failures    int64          `json:"tileFailures"`
	AvgRenderMs float64        `json:"avgRenderMs"`
	MemoryTiles cache.Stats    `json:"memoryTileCache"`
	ChunkCache  cache.Stats    `json:"chunkCache"`
	Scheduler   SchedulerStats `json:"scheduler"`
	DirtyChunks int            `json:"dirtyChunks"`
	CurrentRev  uint64         `json:"currentRevision"`
}

// Stats snapshots the manager's counters.
func (m *Manager) Stats() Stats {
	gen := m.generated.Load()
	avg := 0.0
	if gen > 0 {
		avg = float64(m.renderNs.Load()) / float64(gen) / 1e6
	}
	return Stats{
		Generated:   gen,
		Served:      m.served.Load(),
		Failures:    m.failures.Load(),
		AvgRenderMs: avg,
		MemoryTiles: m.memory.Stats(),
		ChunkCache:  m.Provider.Stats(),
		Scheduler:   m.sched.Stats(),
		DirtyChunks: m.Dirty.Len(),
		CurrentRev:  m.Rev.Current(),
	}
}
