package mcworld

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/enderfall/minecraft-map/backend/internal/anvil"
	"github.com/enderfall/minecraft-map/backend/internal/blocks"
	"github.com/enderfall/minecraft-map/backend/internal/cache"
	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/nbt"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

// Options configures opening a world.
type Options struct {
	// Path is the world directory.
	Path string
	// Blocks and Biomes are the shared registries.
	Blocks *blocks.Registry
	Biomes *blocks.Biomes
	// ExtraDimensions maps dimension ids to region-parent directories, for
	// layouts the discovery layer cannot infer.
	ExtraDimensions map[string]string
	// RegionCacheFiles bounds how many region files stay open.
	RegionCacheFiles int
	// VoxelDepth bounds how many Y layers of voxel data ChunkVoxels stores
	// per chunk. The zero value resolves to DefaultVoxelDepthConfig().
	VoxelDepth VoxelDepthConfig
	Log        *slog.Logger
}

// World reads chunk surfaces from Anvil region files.
//
// It implements world.Provider, so the demo world and a real save are
// interchangeable everywhere downstream.
type World struct {
	path   string
	blocks *blocks.Registry
	biomes *blocks.Biomes
	log    *slog.Logger

	waterID uint16

	mu   sync.RWMutex
	dims []world.DimensionInfo
	byID map[string]world.DimensionInfo

	voxelDepth VoxelDepthConfig

	regions *cache.LRU[regionKey, *anvil.Region]
	// regionFlight stops several workers opening the same region file at once.
	regionFlight *cache.Group[regionKey, *anvil.Region]

	// watch remembers per-chunk write timestamps so world changes can be
	// detected precisely rather than a whole region at a time.
	watchMu sync.Mutex
	watch   map[regionKey]*regionWatch
}

type regionKey struct {
	dim  string
	x, z int
}

type regionWatch struct {
	modTime time.Time
	stamps  map[int]time.Time
}

// Open discovers a world's dimensions and prepares it for reading.
func Open(opts Options) (*World, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.RegionCacheFiles <= 0 {
		opts.RegionCacheFiles = 64
	}
	st, err := os.Stat(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("world path %s: %w", opts.Path, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("world path %s is not a directory", opts.Path)
	}

	voxelDepth := opts.VoxelDepth
	if voxelDepth == (VoxelDepthConfig{}) {
		voxelDepth = DefaultVoxelDepthConfig()
	}
	w := &World{
		path:         opts.Path,
		blocks:       opts.Blocks,
		biomes:       opts.Biomes,
		log:          opts.Log,
		waterID:      opts.Blocks.ID("minecraft:water"),
		voxelDepth:   voxelDepth.clamp(),
		regions:      cache.NewLRU[regionKey, *anvil.Region](int64(opts.RegionCacheFiles)),
		regionFlight: cache.NewGroup[regionKey, *anvil.Region](),
		watch:        make(map[regionKey]*regionWatch),
	}

	// A region file evicted from the cache must have its handle closed, or the
	// server would leak one descriptor per eviction while panning a large world.
	// Retire (not Close) defers the actual close if a worker is still mid-read
	// on this same handle -- Close would risk closing a file another goroutine
	// is in the middle of ReadAt-ing.
	w.regions.SetOnEvict(func(_ regionKey, r *anvil.Region) {
		if r != nil {
			r.Retire()
		}
	})

	dims, err := w.discover(opts.ExtraDimensions)
	if err != nil {
		return nil, err
	}
	if len(dims) == 0 {
		return nil, fmt.Errorf("no dimensions with region files found under %s", opts.Path)
	}
	w.dims = dims
	w.byID = make(map[string]world.DimensionInfo, len(dims))
	for _, d := range dims {
		w.byID[d.ID] = d
	}
	return w, nil
}

// Close releases cached region file handles.
func (w *World) Close() {
	w.regions.Clear()
}

// Dimensions implements world.Provider.
func (w *World) Dimensions(context.Context) ([]world.DimensionInfo, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]world.DimensionInfo, len(w.dims))
	copy(out, w.dims)
	return out, nil
}

// Dimension implements world.Provider.
func (w *World) Dimension(_ context.Context, id string) (world.DimensionInfo, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	d, ok := w.byID[id]
	return d, ok
}

// ChunkSurface implements world.Provider.
func (w *World) ChunkSurface(ctx context.Context, dimension string, pos mcmath.ChunkPos) (*world.ChunkSurface, error) {
	dc, dim, err := w.chunkAt(ctx, dimension, pos)
	if err != nil {
		return nil, err
	}
	return w.surface(dc, dim), nil
}

// ChunkVoxels implements world.VolumeProvider, supplying the top layers of
// block data the isometric voxel renderer needs (ISO_VOXEL_PLAN.md §3.4).
// Unlike the plan's sketch, this does not need to run surface() first: see
// voxels()'s doc comment for why ChunkSurface.Height cannot drive its depth
// sizing in this registry, which also means there is nothing here for a
// surface scan to feed.
func (w *World) ChunkVoxels(ctx context.Context, dimension string, pos mcmath.ChunkPos) (*world.ChunkVoxels, error) {
	dc, dim, err := w.chunkAt(ctx, dimension, pos)
	if err != nil {
		return nil, err
	}
	return w.voxels(dc, dim), nil
}

// chunkAt reads and decodes one chunk's raw NBT, resolving every "not
// generated yet" case to world.ErrChunkAbsent so ChunkSurface and
// ChunkVoxels agree exactly on what counts as absent.
func (w *World) chunkAt(ctx context.Context, dimension string, pos mcmath.ChunkPos) (*decodedChunk, world.DimensionInfo, error) {
	dim, ok := w.Dimension(ctx, dimension)
	if !ok {
		return nil, world.DimensionInfo{}, fmt.Errorf("unknown dimension %q", dimension)
	}

	reg, release, err := w.acquireRegion(dim, pos.Region())
	if err != nil {
		return nil, dim, err
	}
	if reg == nil {
		return nil, dim, world.ErrChunkAbsent
	}
	defer release()

	if !reg.Has(pos) {
		return nil, dim, world.ErrChunkAbsent
	}

	root, err := reg.ReadChunk(pos)
	if err != nil {
		if errors.Is(err, anvil.ErrChunkAbsent) {
			return nil, dim, world.ErrChunkAbsent
		}
		return nil, dim, err
	}

	dc, err := w.decodeChunk(pos, root)
	if err != nil {
		return nil, dim, err
	}
	if dc.empty {
		return nil, dim, world.ErrChunkAbsent
	}
	return dc, dim, nil
}

// acquireRegion returns a region file pinned against concurrent eviction,
// along with a release func the caller must invoke exactly once when done
// reading. If the file does not exist it returns (nil, noop, nil).
//
// A plain cache lookup is not enough: the region could be retired by an
// eviction (or PollChanges invalidation) in the window between this
// goroutine reading the cache and it calling ReadChunk. Acquire closes that
// window -- if it reports the region was already retired, the cache slot is
// already gone, so looking it up again opens or finds a fresh handle.
func (w *World) acquireRegion(dim world.DimensionInfo, pos mcmath.RegionPos) (*anvil.Region, func(), error) {
	noop := func() {}
	for {
		reg, err := w.region(dim, pos)
		if err != nil || reg == nil {
			return reg, noop, err
		}
		if reg.Acquire() {
			return reg, reg.Release, nil
		}
	}
}

// region returns an open region file, or nil if the file does not exist.
func (w *World) region(dim world.DimensionInfo, pos mcmath.RegionPos) (*anvil.Region, error) {
	key := regionKey{dim: dim.ID, x: pos.X, z: pos.Z}
	if r, ok := w.regions.Get(key); ok {
		return r, nil
	}

	// Region files are expensive to open and are shared by up to 1024 chunks,
	// so several workers starting on neighbouring tiles must not each open one.
	r, err, _ := w.regionFlight.Do(key, func() (*anvil.Region, error) {
		if r, ok := w.regions.Get(key); ok {
			return r, nil
		}
		path := filepath.Join(dim.Path, anvil.RegionFileName(pos))
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
		reg, err := anvil.Open(path, pos)
		if err != nil {
			return nil, err
		}
		w.regions.Put(key, reg, 1)
		return reg, nil
	})
	return r, err
}

// ---------------------------------------------------------------------------
// Dimension discovery
// ---------------------------------------------------------------------------

// discover finds every dimension with region files.
//
// Real deployments use at least four different layouts. Vanilla single-player
// nests DIM-1 and DIM1 inside the world folder; Spigot and Paper split them
// into sibling world_nether and world_the_end folders; datapack and mod
// dimensions live under dimensions/<namespace>/<path>. Assuming any one of
// these would silently lose dimensions on the others, so all are probed, and
// operators can name anything unusual explicitly in the config.
func (w *World) discover(extra map[string]string) ([]world.DimensionInfo, error) {
	found := map[string]world.DimensionInfo{}

	levelName, spawn, border := w.readLevelDat()

	add := func(id, name, regionDir string, defMinY, defMaxY int, ceiling bool) {
		if _, exists := found[id]; exists {
			return
		}
		st, err := os.Stat(regionDir)
		if err != nil || !st.IsDir() {
			return
		}
		regions, err := anvil.ListRegions(regionDir)
		if err != nil || len(regions) == 0 {
			return
		}
		d := world.DimensionInfo{
			ID: id, Name: name, Path: regionDir,
			MinY: defMinY, MaxY: defMaxY, Enabled: true, HasCeiling: ceiling,
		}
		if id == "minecraft:overworld" {
			d.SpawnX, d.SpawnZ = spawn[0], spawn[2]
			d.CenterX, d.CenterZ = border.centerX, border.centerZ
			d.WorldBorder = border.size
		}
		// Derive the real height range from a chunk rather than assuming
		// vanilla, so modded dimensions with unusual builds render fully.
		if lo, hi, ok := w.probeHeightRange(d, regions); ok {
			d.MinY, d.MaxY = lo, hi
		}
		if d.WorldBorder <= 0 {
			d.WorldBorder = estimateBorder(regions)
		}
		found[id] = d
	}

	// Vanilla / single-player layout.
	add("minecraft:overworld", nonEmpty(levelName, "Overworld"),
		filepath.Join(w.path, "region"), -64, 320, false)
	add("minecraft:the_nether", "Nether",
		filepath.Join(w.path, "DIM-1", "region"), 0, 128, true)
	add("minecraft:the_end", "The End",
		filepath.Join(w.path, "DIM1", "region"), 0, 256, false)

	// Spigot / Paper layout: sibling folders next to the overworld.
	parent := filepath.Dir(w.path)
	base := filepath.Base(w.path)
	add("minecraft:the_nether", "Nether",
		filepath.Join(parent, base+"_nether", "DIM-1", "region"), 0, 128, true)
	add("minecraft:the_end", "The End",
		filepath.Join(parent, base+"_the_end", "DIM1", "region"), 0, 256, false)

	// Datapack and mod dimensions: dimensions/<namespace>/<path>/region.
	w.discoverNamespaced(filepath.Join(w.path, "dimensions"), found, add)
	// Some servers place them beside the world folder too.
	w.discoverNamespaced(filepath.Join(parent, base+"_dimensions"), found, add)

	// Explicit overrides always win.
	for id, dir := range extra {
		regionDir := dir
		if st, err := os.Stat(filepath.Join(dir, "region")); err == nil && st.IsDir() {
			regionDir = filepath.Join(dir, "region")
		}
		delete(found, id)
		add(id, prettyName(id), regionDir, -64, 320, false)
	}

	out := make([]world.DimensionInfo, 0, len(found))
	for _, d := range found {
		out = append(out, d)
	}
	// Vanilla dimensions first, then everything else alphabetically, so the
	// selector reads sensibly on a heavily modded server.
	order := map[string]int{
		"minecraft:overworld": 0, "minecraft:the_nether": 1, "minecraft:the_end": 2,
	}
	sort.Slice(out, func(i, j int) bool {
		oi, iok := order[out[i].ID]
		oj, jok := order[out[j].ID]
		if iok != jok {
			return iok
		}
		if iok && jok {
			return oi < oj
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// discoverNamespaced walks dimensions/<namespace>/<path>/region.
func (w *World) discoverNamespaced(root string, found map[string]world.DimensionInfo, add func(id, name, dir string, minY, maxY int, ceiling bool)) {
	namespaces, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, ns := range namespaces {
		if !ns.IsDir() {
			continue
		}
		paths, err := os.ReadDir(filepath.Join(root, ns.Name()))
		if err != nil {
			continue
		}
		for _, p := range paths {
			if !p.IsDir() {
				continue
			}
			id := ns.Name() + ":" + p.Name()
			dir := filepath.Join(root, ns.Name(), p.Name(), "region")
			// Modded dimensions vary wildly in height; the probe below corrects
			// this generous default from the actual chunk data.
			add(id, prettyName(id), dir, -64, 320, false)
		}
	}
}

// probeHeightRange reads a chunk to learn a dimension's real build range.
func (w *World) probeHeightRange(d world.DimensionInfo, regions []anvil.RegionEntry) (minY, maxY int, ok bool) {
	// Prefer a region near the origin, which is most likely to be fully
	// generated, and give up quickly rather than scanning a huge world.
	sort.Slice(regions, func(i, j int) bool {
		di := regions[i].Pos.X*regions[i].Pos.X + regions[i].Pos.Z*regions[i].Pos.Z
		dj := regions[j].Pos.X*regions[j].Pos.X + regions[j].Pos.Z*regions[j].Pos.Z
		return di < dj
	})
	for _, entry := range regions[:min(3, len(regions))] {
		reg, err := anvil.Open(entry.Path, entry.Pos)
		if err != nil {
			continue
		}
		baseCX := entry.Pos.MinChunkX()
		baseCZ := entry.Pos.MinChunkZ()
		for i := 0; i < 64; i++ {
			pos := mcmath.ChunkPos{X: baseCX + i%8, Z: baseCZ + i/8}
			if !reg.Has(pos) {
				continue
			}
			root, err := reg.ReadChunk(pos)
			if err != nil {
				continue
			}
			dc, err := w.decodeChunk(pos, root)
			if err != nil || dc.empty {
				continue
			}
			reg.Close()
			lo := dc.minSectionY * 16
			hi := dc.maxSectionY*16 + 16
			w.log.Debug("probed dimension height range",
				"dimension", d.ID, "minY", lo, "maxY", hi, "chunk", describeChunk(dc))
			return lo, hi, true
		}
		reg.Close()
	}
	return 0, 0, false
}

// estimateBorder derives a plausible world border from how far region files
// extend, so pre-generation and the border overlay have something sensible
// when level.dat carries no explicit border.
func estimateBorder(regions []anvil.RegionEntry) int {
	maxR := 0
	for _, r := range regions {
		for _, v := range []int{r.Pos.X, r.Pos.Z} {
			if v < 0 {
				v = -v - 1
			}
			if v > maxR {
				maxR = v
			}
		}
	}
	// Region radius to block diameter, with a little headroom.
	return (maxR + 1) * mcmath.RegionBlocks * 2
}

type borderInfo struct {
	centerX, centerZ, size int
}

// readLevelDat extracts the world name, spawn point and world border.
// A missing or unreadable level.dat is not fatal: the world still renders, it
// just loses these conveniences.
func (w *World) readLevelDat() (name string, spawn [3]int, border borderInfo) {
	border = borderInfo{size: 0}
	path := filepath.Join(w.path, "level.dat")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", spawn, border
	}
	// level.dat is gzip-compressed NBT.
	data, err := gunzip(raw)
	if err != nil {
		// Some tools write it uncompressed.
		data = raw
	}
	root, _, err := nbt.Parse(data)
	if err != nil {
		w.log.Warn("could not parse level.dat", "path", path, "error", err)
		return "", spawn, border
	}
	d, ok := root.Get("Data")
	if !ok {
		d = root
	}
	name = d.GetString("", "LevelName")
	spawn[0] = int(d.GetInt(0, "SpawnX"))
	spawn[1] = int(d.GetInt(64, "SpawnY"))
	spawn[2] = int(d.GetInt(0, "SpawnZ"))
	border.centerX = int(d.GetInt(0, "BorderCenterX"))
	border.centerZ = int(d.GetInt(0, "BorderCenterZ"))
	border.size = int(d.GetInt(0, "BorderSize"))
	// Vanilla writes a 60,000,000 default border, which is not a useful bound
	// for pre-generation; treat it as absent.
	if border.size >= 29_000_000 {
		border.size = 0
	}
	return name, spawn, border
}

func nonEmpty(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// prettyName turns "some_mod:mining_dimension" into "Mining Dimension".
func prettyName(id string) string {
	s := id
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.ReplaceAll(s, "_", " ")
	parts := strings.Fields(s)
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	if len(parts) == 0 {
		return id
	}
	return strings.Join(parts, " ")
}

// ---------------------------------------------------------------------------
// Change detection
// ---------------------------------------------------------------------------

// Change reports one chunk that was written since the last poll.
type Change struct {
	Dimension string
	Pos       mcmath.ChunkPos
}

// PollChanges detects chunks written since the previous call.
//
// It works in two stages. A region file whose modification time has not moved
// is skipped without opening it, which keeps the scan cheap on a world with
// thousands of regions. When a file has changed, its 4 KiB timestamp table is
// read and compared entry by entry, so a server saving one chunk produces one
// dirty chunk rather than a thousand.
//
// The first poll establishes a baseline and reports nothing, otherwise starting
// the server would look like the entire world had just changed.
func (w *World) PollChanges(ctx context.Context) ([]Change, error) {
	dims, err := w.Dimensions(ctx)
	if err != nil {
		return nil, err
	}

	var changes []Change
	for _, d := range dims {
		if !d.Enabled {
			continue
		}
		regions, err := anvil.ListRegions(d.Path)
		if err != nil {
			continue
		}
		for _, entry := range regions {
			if err := ctx.Err(); err != nil {
				return changes, err
			}
			key := regionKey{dim: d.ID, x: entry.Pos.X, z: entry.Pos.Z}

			w.watchMu.Lock()
			prev, seen := w.watch[key]
			w.watchMu.Unlock()

			if seen && !entry.ModTime.After(prev.modTime) {
				continue
			}

			reg, err := anvil.Open(entry.Path, entry.Pos)
			if err != nil {
				continue
			}
			stamps := make(map[int]time.Time, 64)
			baseCX, baseCZ := entry.Pos.MinChunkX(), entry.Pos.MinChunkZ()
			for i := 0; i < mcmath.RegionChunks*mcmath.RegionChunks; i++ {
				pos := mcmath.ChunkPos{X: baseCX + i%mcmath.RegionChunks, Z: baseCZ + i/mcmath.RegionChunks}
				if !reg.Has(pos) {
					continue
				}
				ts := reg.Timestamp(pos)
				stamps[i] = ts
				if !seen {
					continue
				}
				if old, ok := prev.stamps[i]; !ok || ts.After(old) {
					changes = append(changes, Change{Dimension: d.ID, Pos: pos})
				}
			}
			reg.Close()

			w.watchMu.Lock()
			w.watch[key] = &regionWatch{modTime: entry.ModTime, stamps: stamps}
			w.watchMu.Unlock()

			// Drop the cached open handle so the next read sees new data.
			w.regions.Remove(key)
		}
	}
	return changes, nil
}

// Scan reports what the world contains, for the scan-world CLI command.
type Scan struct {
	Dimension world.DimensionInfo `json:"dimension"`
	Regions   int                 `json:"regions"`
	Chunks    int                 `json:"chunks"`
	Bytes     int64               `json:"bytes"`
	Bounds    mcmath.BlockBounds  `json:"bounds"`
}

// ScanWorld summarises every dimension, reading only region headers.
func (w *World) ScanWorld(ctx context.Context, countChunks bool) ([]Scan, error) {
	dims, err := w.Dimensions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Scan, 0, len(dims))
	for _, d := range dims {
		regions, err := anvil.ListRegions(d.Path)
		if err != nil {
			continue
		}
		s := Scan{Dimension: d, Regions: len(regions)}
		if len(regions) > 0 {
			s.Bounds = mcmath.BlockBounds{
				MinX: 1 << 30, MinZ: 1 << 30, MaxX: -(1 << 30), MaxZ: -(1 << 30),
			}
		}
		for _, entry := range regions {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			s.Bytes += entry.Size
			minX := entry.Pos.MinChunkX() * mcmath.ChunkSize
			minZ := entry.Pos.MinChunkZ() * mcmath.ChunkSize
			s.Bounds.MinX = min(s.Bounds.MinX, minX)
			s.Bounds.MinZ = min(s.Bounds.MinZ, minZ)
			s.Bounds.MaxX = max(s.Bounds.MaxX, minX+mcmath.RegionBlocks)
			s.Bounds.MaxZ = max(s.Bounds.MaxZ, minZ+mcmath.RegionBlocks)

			if countChunks {
				reg, err := anvil.Open(entry.Path, entry.Pos)
				if err != nil {
					continue
				}
				s.Chunks += reg.ChunkCount()
				reg.Close()
			}
		}
		out = append(out, s)
	}
	return out, nil
}
