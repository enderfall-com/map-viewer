// Package app wires the components together and runs the background loops.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/enderfall/minecraft-map/backend/internal/blocks"
	"github.com/enderfall/minecraft-map/backend/internal/cache"
	"github.com/enderfall/minecraft-map/backend/internal/config"
	"github.com/enderfall/minecraft-map/backend/internal/demo"
	"github.com/enderfall/minecraft-map/backend/internal/features"
	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/mcworld"
	"github.com/enderfall/minecraft-map/backend/internal/realtime"
	"github.com/enderfall/minecraft-map/backend/internal/render"
	"github.com/enderfall/minecraft-map/backend/internal/textures"
	"github.com/enderfall/minecraft-map/backend/internal/tiles"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

// App holds every constructed component.
type App struct {
	Cfg      config.Config
	Log      *slog.Logger
	Blocks   *blocks.Registry
	Biomes   *blocks.Biomes
	Provider *world.Cached
	Store    *cache.FSStore
	Tiles    *tiles.Manager
	Features *features.Memory
	Hub      *realtime.Hub

	// TextureStore is non-nil only when texture rendering is enabled and at
	// least one source was opened; kept on App so Close can release the
	// jars/zips it holds open.
	TextureStore *textures.Store

	// sim is non-nil only in demo mode.
	sim *demo.SimulatedPlayers
	// anvil is non-nil only when reading a real world.
	anvil *mcworld.World

	// regenMu guards regenQueue/regenRunning, which cap dirty-tile regeneration
	// to one worker goroutine per dimension. Without this, a dirty-flush tick
	// firing every second while a busy world keeps regeneration behind would
	// spawn a new unbounded goroutine per tick instead of applying backpressure.
	regenMu      sync.Mutex
	regenQueue   map[string][]tiles.TileID
	regenRunning map[string]bool
}

// Build constructs the application from configuration.
func Build(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	reg, err := blocks.NewDefaultRegistry()
	if err != nil {
		return nil, err
	}
	bio, err := blocks.NewDefaultBiomes()
	if err != nil {
		return nil, err
	}

	// Config files layer on top of the embedded baseline, so a missing file is
	// a non-event rather than a startup failure.
	if p := cfg.Data.BlocksFile; p != "" {
		if n, err := reg.LoadBlocksFile(p); err == nil {
			log.Info("loaded block colours", "file", p, "entries", n)
		} else if !errors.Is(err, os.ErrNotExist) && !os.IsNotExist(errors.Unwrap(err)) {
			log.Warn("could not load block colours", "file", p, "error", err)
		}
	}
	if p := cfg.Data.BiomesFile; p != "" {
		if n, err := bio.LoadBiomesFile(p); err == nil {
			log.Info("loaded biome colours", "file", p, "entries", n)
		} else if !errors.Is(err, os.ErrNotExist) && !os.IsNotExist(errors.Unwrap(err)) {
			log.Warn("could not load biome colours", "file", p, "error", err)
		}
	}
	for _, dir := range cfg.Data.OverlayDirs {
		if n, err := loadOverlayDir(reg, bio, dir); err != nil {
			log.Warn("could not load overlay directory", "dir", dir, "error", err)
		} else if n > 0 {
			log.Info("loaded palette overlay", "dir", dir, "entries", n)
		}
	}

	app := &App{
		Cfg: cfg, Log: log, Blocks: reg, Biomes: bio,
		regenQueue:   make(map[string][]tiles.TileID),
		regenRunning: make(map[string]bool),
	}

	// Choose the terrain source. Both satisfy world.Provider, so nothing
	// downstream knows or cares which one is active.
	var provider world.Provider
	if cfg.Minecraft.Demo {
		dw := demo.New(reg, bio, demo.Options{
			Seed: cfg.Minecraft.DemoSeed, Radius: cfg.Minecraft.DemoRadius,
		})
		provider = dw
		app.sim = demo.NewSimulatedPlayers(dw)
		log.Info("using generated demo world",
			"seed", cfg.Minecraft.DemoSeed, "radius", cfg.Minecraft.DemoRadius)
	} else {
		aw, err := mcworld.Open(mcworld.Options{
			Path:             cfg.Minecraft.WorldPath,
			Blocks:           reg,
			Biomes:           bio,
			ExtraDimensions:  cfg.Minecraft.ExtraDimensionPaths,
			RegionCacheFiles: cfg.Minecraft.RegionCacheFiles,
			VoxelDepth: mcworld.VoxelDepthConfig{
				BelowGround: cfg.Render.IsoVoxelBelowGround,
				MinDepth:    cfg.Render.IsoVoxelMinDepth,
				MaxDepth:    cfg.Render.IsoVoxelMaxDepth,
			},
			Log: log,
		})
		if err != nil {
			return nil, fmt.Errorf("open world: %w", err)
		}
		provider = aw
		app.anvil = aw
		dims, _ := aw.Dimensions(ctx)
		log.Info("opened Minecraft world",
			"path", cfg.Minecraft.WorldPath, "dimensions", len(dims))
	}

	app.Provider = world.NewCached(provider, cfg.Minecraft.ChunkCacheBytes, cfg.Minecraft.VoxelCacheBytes)

	store, err := cache.NewFSStore(cfg.Tiles.Directory)
	if err != nil {
		return nil, err
	}
	app.Store = store

	opts, err := cfg.RenderOptions()
	if err != nil {
		return nil, err
	}
	format, _ := tiles.ParseFormat(cfg.Tiles.Format)

	app.Tiles = tiles.NewManager(
		app.Provider, store, reg, bio, opts,
		tiles.Config{
			MinZoom:           cfg.Map.MinZoom,
			MaxZoom:           cfg.Map.MaxZoom,
			TopBaseZoom:       cfg.Tiles.TopBaseZoom,
			IsoBaseZoom:       cfg.Tiles.IsoBaseZoom,
			MaxCompositeDepth: cfg.Tiles.MaxCompositeDepth,
			MemoryTileBytes:   cfg.Tiles.MemoryCacheBytes,
			Format:            format,
			StoreBlankTiles:   cfg.Tiles.StoreBlankTiles,
			IsoEdgeSkirt:      cfg.Render.IsoEdgeSkirt,
			IsoVoxel:          cfg.Render.IsoVoxel,
			IsoVoxelMaxDepth:  cfg.Render.IsoVoxelMaxDepth,
		},
		cfg.Tiles.Workers, cfg.Tiles.MaxQueued, log,
	)

	// Texture sources are opened after the tile manager exists so a source
	// error can be logged without unwinding everything already built; a
	// missing or misconfigured texture source is never fatal; rendering
	// simply keeps using blocks.json's flat colours.
	if cfg.Textures.Enabled && len(cfg.Textures.Sources) > 0 {
		texStore, srcErrs := textures.OpenSources(cfg.Textures.Sources)
		for _, e := range srcErrs {
			log.Warn("texture source skipped", "error", e)
		}
		if texStore.SourceCount() == 0 {
			log.Warn("texture rendering enabled but no sources could be opened; using flat colours")
		} else {
			log.Info("loaded texture sources", "opened", texStore.SourceCount(), "skipped", len(srcErrs))
			app.TextureStore = texStore
			app.Tiles.SetTextures(textures.NewSet(texStore, reg))
			// A modpack can add hundreds of custom plants; naming each one in
			// blocks.json does not scale. Any block blocks.json does not
			// already map explicitly is classified from its real model data
			// instead, so an unlisted modded flower or crop still gets
			// treated as a ground decoration rather than an opaque cube.
			reg.SetDecorationClassifier(func(name string) bool {
				return textures.ClassifyDecoration(texStore, name)
			})
		}
	}

	app.Features = features.NewMemory()
	if cfg.Minecraft.Demo {
		demo.SeedFeatures(app.Features)
		log.Info("seeded demo overlay features")
	}
	if p := cfg.Data.FeaturesFile; p != "" {
		if n, err := app.Features.LoadFile(p); err != nil {
			log.Warn("could not load features file", "file", p, "error", err)
		} else {
			log.Info("loaded features", "file", p, "entries", n)
		}
	}
	// Every dimension gets a spawn marker derived from its own metadata.
	app.seedSpawnMarkers(ctx)

	app.Hub = realtime.NewHub(cfg.Live.MaxConnections, log)
	return app, nil
}

// loadOverlayDir merges every blocks.json / biomes.json found in a directory.
func loadOverlayDir(reg *blocks.Registry, bio *blocks.Biomes, dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		full := dir + string(os.PathSeparator) + name
		switch {
		case len(name) > 12 && name[len(name)-12:] == "-blocks.json", name == "blocks.json":
			if n, err := reg.LoadBlocksFile(full); err == nil {
				total += n
			}
		case len(name) > 12 && name[len(name)-12:] == "-biomes.json", name == "biomes.json":
			if n, err := bio.LoadBiomesFile(full); err == nil {
				total += n
			}
		}
	}
	return total, nil
}

// seedSpawnMarkers adds a spawn marker for any dimension that lacks one.
func (a *App) seedSpawnMarkers(ctx context.Context) {
	dims, err := a.Provider.Dimensions(ctx)
	if err != nil {
		return
	}
	existing, _ := a.Features.Markers(ctx, "", "spawn")
	have := make(map[string]bool, len(existing))
	for _, m := range existing {
		have[m.Dimension] = true
	}
	for _, d := range dims {
		if !d.Enabled || have[d.ID] {
			continue
		}
		a.Features.PutMarker(features.Marker{
			ID:        "spawn-" + cache.SafeID(d.ID),
			Kind:      "spawn",
			Name:      "Spawn",
			Dimension: d.ID,
			X:         d.SpawnX,
			Z:         d.SpawnZ,
			Icon:      "spawn",
		})
	}
}

// Close releases resources.
func (a *App) Close() {
	if a.Tiles != nil {
		a.Tiles.Close()
	}
	if a.anvil != nil {
		a.anvil.Close()
	}
	if a.TextureStore != nil {
		_ = a.TextureStore.Close()
	}
}

// ---------------------------------------------------------------------------
// Background loops
// ---------------------------------------------------------------------------

// Run starts every background loop and blocks until the context is cancelled.
func (a *App) Run(ctx context.Context) {
	var wg sync.WaitGroup

	if a.Cfg.Live.Enabled {
		wg.Add(1)
		go func() { defer wg.Done(); a.runPlayers(ctx) }()

		wg.Add(1)
		go func() { defer wg.Done(); a.runDirtyFlush(ctx) }()

		if a.Cfg.Live.WatchWorld && a.anvil != nil {
			wg.Add(1)
			go func() { defer wg.Done(); a.runWorldWatch(ctx) }()
		}
	}
	if a.Cfg.Tiles.Pregenerate {
		wg.Add(1)
		go func() { defer wg.Done(); a.runPregenerate(ctx) }()
	}

	wg.Wait()
}

// runPlayers refreshes player positions and pushes them to clients.
func (a *App) runPlayers(ctx context.Context) {
	interval := time.Duration(max(1, a.Cfg.Live.PlayerPollSeconds)) * time.Second
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if a.sim != nil {
				a.sim.Tick(ctx, a.Features, now)
			}
			// Broadcast per dimension so a client only receives the players it
			// is actually displaying.
			dims, err := a.Provider.Dimensions(ctx)
			if err != nil {
				continue
			}
			for _, d := range dims {
				if !d.Enabled {
					continue
				}
				players, err := a.Features.Players(ctx, d.ID)
				if err != nil || len(players) == 0 {
					continue
				}
				a.Hub.Broadcast(realtime.EventPlayerMove(d.ID, players))
			}
		}
	}
}

// runDirtyFlush turns changed chunks into regenerated tiles and invalidation
// events.
//
// This implements the incremental update flow end to end: coalesce chunk
// changes, compute exactly which tiles they affect at every zoom, regenerate
// deepest-first so parents composite fresh children, issue new revisions, and
// tell clients which URLs changed.
func (a *App) runDirtyFlush(ctx context.Context) {
	settle := time.Duration(a.Cfg.Live.DirtySettleSeconds) * time.Second
	t := time.NewTicker(time.Second)
	defer t.Stop()

	modes := make([]tiles.Mode, 0, 2)
	for _, m := range a.Cfg.Tiles.Modes {
		mode, _ := tiles.ParseMode(m)
		modes = append(modes, mode)
	}
	styles := a.Cfg.Styles()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			chunks := a.Tiles.Dirty.Drain(settle)
			if len(chunks) == 0 {
				continue
			}
			a.flushDirty(ctx, chunks, modes, styles)
		}
	}
}

// flushDirty regenerates the tiles affected by a batch of chunk changes.
func (a *App) flushDirty(ctx context.Context, chunks []tiles.DirtyChunk, modes []tiles.Mode, styles []render.Style) {
	start := time.Now()

	// Chunk surfaces must be dropped before anything re-reads the world.
	for _, c := range chunks {
		a.Provider.Invalidate(c.Dimension, c.Pos)
	}

	// The isometric footprint depends on the dimension's height range, so group
	// by dimension and use each one's real bounds.
	byDim := make(map[string][]tiles.DirtyChunk)
	for _, c := range chunks {
		byDim[c.Dimension] = append(byDim[c.Dimension], c)
	}

	total := 0
	for dimID, list := range byDim {
		dim, ok := a.Provider.Dimension(ctx, dimID)
		if !ok {
			continue
		}
		affected := tiles.AffectedTiles(
			list, modes, a.Cfg.Map.MinZoom, a.Cfg.Map.MaxZoom, dim.MinY, dim.MaxY)

		changed := make([]map[string]any, 0, len(affected))
		for _, id := range affected {
			rev := a.Tiles.Invalidate(ctx, id, styles)
			changed = append(changed, map[string]any{
				"mode": string(id.Mode), "z": id.Zoom, "x": id.X, "y": id.Y, "revision": rev,
			})
		}
		total += len(affected)

		// Regenerate at dirty priority: promptly, but never ahead of a user
		// waiting on a tile they are looking at right now. Queued rather than a
		// bare goroutine per tick, so a dimension whose regeneration runs
		// behind coalesces backlog instead of accumulating one goroutine per
		// second of sustained edits.
		a.scheduleRegenerate(ctx, dimID, affected, styles)

		for _, c := range list {
			a.Hub.Broadcast(realtime.EventChunkUpdated(
				dimID, c.Pos.X, c.Pos.Z, a.Tiles.Rev.Current(), changed))
		}
	}

	a.Log.Info("regenerated dirty tiles",
		"chunks", len(chunks), "tiles", total, "duration_ms", time.Since(start).Milliseconds())
}

// scheduleRegenerate enqueues affected tiles for a dimension and ensures
// exactly one worker goroutine is draining that dimension's queue. A batch
// that arrives while a worker is already running is appended to the queue
// rather than spawning a second goroutine, so regeneration backlog is bounded
// by one worker per dimension no matter how far behind it falls.
func (a *App) scheduleRegenerate(ctx context.Context, dimID string, affected []tiles.TileID, styles []render.Style) {
	a.regenMu.Lock()
	a.regenQueue[dimID] = append(a.regenQueue[dimID], affected...)
	if a.regenRunning[dimID] {
		a.regenMu.Unlock()
		return
	}
	a.regenRunning[dimID] = true
	a.regenMu.Unlock()

	go a.drainRegenerateQueue(ctx, dimID, styles)
}

// drainRegenerateQueue regenerates a dimension's queued tiles until the queue
// is empty, picking up anything appended while it was working.
func (a *App) drainRegenerateQueue(ctx context.Context, dimID string, styles []render.Style) {
	for {
		a.regenMu.Lock()
		pending := a.regenQueue[dimID]
		a.regenQueue[dimID] = nil
		if len(pending) == 0 {
			a.regenRunning[dimID] = false
			a.regenMu.Unlock()
			return
		}
		a.regenMu.Unlock()

		a.regenerate(ctx, dimID, pending, styles)
		if ctx.Err() != nil {
			a.regenMu.Lock()
			a.regenRunning[dimID] = false
			a.regenMu.Unlock()
			return
		}
	}
}

// regenerate rebuilds invalidated tiles in the order AffectedTiles produced
// them, which is deepest zoom first so parents see fresh children.
func (a *App) regenerate(ctx context.Context, dimension string, affected []tiles.TileID, styles []render.Style) {
	for _, id := range affected {
		for _, style := range styles {
			req := tiles.Request{
				Dimension: dimension, Mode: id.Mode, Style: style,
				Pos: mcmath.TilePos{Zoom: id.Zoom, X: id.X, Y: id.Y},
			}
			if err := a.Tiles.Regenerate(ctx, req, tiles.PriorityDirty); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				a.Log.Debug("tile regeneration skipped",
					"tile_z", id.Zoom, "tile_x", id.X, "tile_y", id.Y, "error", err)
			}
		}
	}
}

// runWorldWatch polls region file modification times and marks changed chunks
// dirty.
//
// Polling modification times is the portable way to notice a running Minecraft
// server saving chunks. It marks the whole region's chunks dirty, which the
// dirty set then coalesces; a plugin pushing exact chunk coordinates would be
// finer-grained, and the same downstream path serves both.
func (a *App) runWorldWatch(ctx context.Context) {
	interval := time.Duration(max(1, a.Cfg.Live.WatchIntervalSeconds)) * time.Second
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			changed, err := a.anvil.PollChanges(ctx)
			if err != nil {
				a.Log.Warn("world watch failed", "error", err)
				continue
			}
			for _, ch := range changed {
				a.Tiles.Dirty.Mark(ch.Dimension, ch.Pos)
			}
			if len(changed) > 0 {
				a.Log.Debug("world changes detected", "chunks", len(changed))
			}
		}
	}
}

// runPregenerate sweeps the world border building tiles in the background.
//
// It works bottom-up -- deepest zoom first -- so every parent is composited
// from children that already exist. Building top-down instead would force each
// low-zoom tile to recursively render its entire subtree.
func (a *App) runPregenerate(ctx context.Context) {
	// Let the server finish starting before competing for CPU.
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second):
	}

	dims, err := a.Provider.Dimensions(ctx)
	if err != nil {
		return
	}
	styles := a.Cfg.Styles()
	for _, d := range dims {
		if !d.Enabled {
			continue
		}
		for _, mstr := range a.Cfg.Tiles.Modes {
			mode, _ := tiles.ParseMode(mstr)
			for _, style := range styles {
				if err := a.Pregenerate(ctx, d, mode, style, PregenOptions{
					MaxZoom:  a.Cfg.Tiles.PregenerateMaxZoom,
					Priority: tiles.PriorityBackground,
				}); err != nil {
					if errors.Is(err, context.Canceled) {
						return
					}
					a.Log.Warn("pre-generation failed",
						"dimension", d.ID, "mode", mstr, "error", err)
				}
			}
		}
	}
	a.Log.Info("pre-generation complete")
}

// PregenOptions bounds a pre-generation sweep.
type PregenOptions struct {
	// Bounds limits the sweep; zero means the dimension's world border.
	Bounds   mcmath.BlockBounds
	MinZoom  int
	MaxZoom  int
	Priority tiles.Priority
	// Progress, if set, is called after each tile.
	Progress func(done, total int, pos mcmath.TilePos)
}

// Pregenerate builds every tile covering a region, from the deepest zoom up.
func (a *App) Pregenerate(ctx context.Context, dim world.DimensionInfo, mode tiles.Mode, style render.Style, opts PregenOptions) error {
	bounds := opts.Bounds
	if bounds.Empty() {
		size := dim.WorldBorder
		if size <= 0 {
			size = 4000
		}
		half := size / 2
		bounds = mcmath.BlockBounds{
			MinX: dim.CenterX - half, MinZ: dim.CenterZ - half,
			MaxX: dim.CenterX + half, MaxZ: dim.CenterZ + half,
		}
	}
	maxZoom := opts.MaxZoom
	if maxZoom <= 0 {
		maxZoom = a.Cfg.Tiles.TopBaseZoom
		if mode == tiles.ModeIso {
			maxZoom = a.Cfg.Tiles.IsoBaseZoom
		}
	}
	minZoom := opts.MinZoom
	if minZoom < a.Cfg.Map.MinZoom {
		minZoom = a.Cfg.Map.MinZoom
	}
	prio := opts.Priority

	// Count first so progress reporting is meaningful.
	total := 0
	for z := maxZoom; z >= minZoom; z-- {
		total += len(a.tilesForSweep(bounds, z, mode, dim))
	}

	done := 0
	for z := maxZoom; z >= minZoom; z-- {
		for _, pos := range a.tilesForSweep(bounds, z, mode, dim) {
			if err := ctx.Err(); err != nil {
				return err
			}
			req := tiles.Request{Dimension: dim.ID, Mode: mode, Style: style, Pos: pos}
			if _, err := a.Tiles.Tile(ctx, req, prio); err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
				if errors.Is(err, tiles.ErrQueueFull) {
					// Back off and retry rather than dropping coverage.
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(250 * time.Millisecond):
					}
					continue
				}
				a.Log.Warn("pre-generation tile failed",
					"tile_z", pos.Zoom, "tile_x", pos.X, "tile_y", pos.Y, "error", err)
			}
			done++
			if opts.Progress != nil {
				opts.Progress(done, total, pos)
			}
		}
	}
	return nil
}

// tilesForSweep lists the tiles at one zoom covering a block region, in the
// coordinate space appropriate to the mode.
func (a *App) tilesForSweep(bounds mcmath.BlockBounds, zoom int, mode tiles.Mode, dim world.DimensionInfo) []mcmath.TilePos {
	if mode != tiles.ModeIso {
		return mcmath.TilesCovering(bounds, zoom)
	}
	// Isometric tiles are indexed in iso space, so the block region has to be
	// projected before it can be turned into tile indices.
	cam, _ := mcmath.ParseIsoCamera(a.Cfg.Render.IsoCamera)
	proj := mcmath.NewIsoProjection(cam)
	ib := proj.IsoFootprintOfBlocks(bounds, dim.MinY, dim.MaxY)
	lo := mcmath.IsoTileAt(ib.MinU, ib.MinV, zoom)
	hi := mcmath.IsoTileAt(ib.MaxU, ib.MaxV, zoom)

	out := make([]mcmath.TilePos, 0, (hi.X-lo.X+1)*(hi.Y-lo.Y+1))
	for y := lo.Y; y <= hi.Y; y++ {
		for x := lo.X; x <= hi.X; x++ {
			out = append(out, mcmath.TilePos{Zoom: zoom, X: x, Y: y})
		}
	}
	return out
}
