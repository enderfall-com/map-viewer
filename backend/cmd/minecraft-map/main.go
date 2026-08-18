// Command minecraft-map serves and generates the Minecraft web map.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/enderfall/minecraft-map/backend/internal/api"
	"github.com/enderfall/minecraft-map/backend/internal/app"
	"github.com/enderfall/minecraft-map/backend/internal/config"
	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/mcworld"
	"github.com/enderfall/minecraft-map/backend/internal/render"
	"github.com/enderfall/minecraft-map/backend/internal/tiles"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

const usage = `minecraft-map - a scalable Minecraft web map

Usage:
  minecraft-map [command] [flags]

Commands:
  serve         Run the HTTP API and tile server (default)
  scan-world    Report the dimensions and region files found in a world
  dimensions    List discovered dimensions as JSON
  generate      Pre-generate tiles for a dimension
  invalidate    Regenerate the tiles affected by a changed chunk
  config        Write a starter configuration file
  version       Print the version

Run "minecraft-map <command> -h" for command flags.
`

var version = "0.1.0"

func main() {
	if err := run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return cmdServe(nil)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	// Allow flags before the command, so "minecraft-map -config x.yml" works.
	if strings.HasPrefix(cmd, "-") {
		return cmdServe(os.Args[1:])
	}

	switch cmd {
	case "serve":
		return cmdServe(args)
	case "scan-world":
		return cmdScanWorld(args)
	case "dimensions":
		return cmdDimensions(args)
	case "generate":
		return cmdGenerate(args)
	case "invalidate":
		return cmdInvalidate(args)
	case "config":
		return cmdConfig(args)
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// commonFlags registers the flags every command shares.
func commonFlags(fs *flag.FlagSet) (*string, *bool, *string, *string) {
	cfgPath := fs.String("config", "", "path to config.yml")
	demo := fs.Bool("demo", false, "use the generated demo world instead of a Minecraft save")
	worldPath := fs.String("world", "", "override minecraft.worldPath")
	logLevel := fs.String("log-level", "", "override log level (debug, info, warn, error)")
	return cfgPath, demo, worldPath, logLevel
}

// loadConfig resolves configuration from flags and file.
func loadConfig(cfgPath string, demo bool, worldPath, logLevel string) (config.Config, *slog.Logger, error) {
	// Fall back to a config file in the conventional location if one exists.
	if cfgPath == "" {
		for _, candidate := range []string{"config/config.yml", "config.yml", "../config/config.yml"} {
			if _, err := os.Stat(candidate); err == nil {
				cfgPath = candidate
				break
			}
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return cfg, nil, err
	}
	if demo {
		cfg.Minecraft.Demo = true
	}
	if worldPath != "" {
		cfg.Minecraft.WorldPath = worldPath
		cfg.Minecraft.Demo = false
	}
	if logLevel != "" {
		cfg.Log.Level = logLevel
	}
	if err := cfg.Validate(); err != nil {
		return cfg, nil, err
	}
	return cfg, newLogger(cfg.Log), nil
}

// newLogger builds the structured logger.
func newLogger(c config.Log) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(c.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if strings.EqualFold(c.Format, "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}

// signalContext cancels on SIGINT/SIGTERM so shutdown is graceful.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath, demo, worldPath, logLevel := commonFlags(fs)
	port := fs.Int("port", 0, "override server.port")
	frontend := fs.String("frontend", "", "serve a built frontend from this directory")
	pregen := fs.Bool("pregenerate", false, "pre-generate tiles in the background on startup")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, log, err := loadConfig(*cfgPath, *demo, *worldPath, *logLevel)
	if err != nil {
		return err
	}
	if *port != 0 {
		cfg.Server.Port = *port
	}
	if *frontend != "" {
		cfg.Server.FrontendDir = *frontend
	}
	if *pregen {
		cfg.Tiles.Pregenerate = true
	}

	ctx, cancel := signalContext()
	defer cancel()

	application, err := app.Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()

	server, err := api.New(cfg, application.Tiles, application.Provider,
		application.Features, application.Hub, application.Blocks, application.Biomes, log)
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(cfg.Server.Host, fmt.Sprint(cfg.Server.Port))
	httpServer := &http.Server{
		Addr:         addr,
		Handler:      server.Handler(),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		// A tile request may legitimately wait on the render queue, so the
		// write timeout is generous; idle connections still get reaped.
		IdleTimeout: 120 * time.Second,
	}

	go application.Run(ctx)

	errCh := make(chan error, 1)
	go func() {
		dims, _ := application.Provider.Dimensions(ctx)
		names := make([]string, 0, len(dims))
		for _, d := range dims {
			names = append(names, d.ID)
		}
		log.Info("listening",
			"addr", addr, "demo", cfg.Minecraft.Demo,
			"tiles", cfg.Tiles.Directory, "format", cfg.Tiles.Format,
			"workers", cfg.Tiles.Workers, "dimensions", strings.Join(names, ", "))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelShutdown()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// ---------------------------------------------------------------------------
// scan-world / dimensions
// ---------------------------------------------------------------------------

func cmdScanWorld(args []string) error {
	fs := flag.NewFlagSet("scan-world", flag.ExitOnError)
	cfgPath, demo, worldPath, logLevel := commonFlags(fs)
	countChunks := fs.Bool("count-chunks", false, "open every region file to count chunks (slower)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, log, err := loadConfig(*cfgPath, *demo, *worldPath, *logLevel)
	if err != nil {
		return err
	}
	if cfg.Minecraft.Demo {
		return fmt.Errorf("scan-world needs a real Minecraft world; pass -world <path>")
	}

	ctx, cancel := signalContext()
	defer cancel()

	application, err := app.Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()

	aw, ok := application.Provider.Inner().(*mcworld.World)
	if !ok {
		return fmt.Errorf("world is not an Anvil world")
	}
	scans, err := aw.ScanWorld(ctx, *countChunks)
	if err != nil {
		return err
	}

	fmt.Printf("World: %s\n\n", cfg.Minecraft.WorldPath)
	for _, s := range scans {
		d := s.Dimension
		fmt.Printf("  %s\n", d.ID)
		fmt.Printf("    name       %s\n", d.Name)
		fmt.Printf("    height     %d .. %d\n", d.MinY, d.MaxY)
		fmt.Printf("    regions    %d\n", s.Regions)
		if *countChunks {
			fmt.Printf("    chunks     %d\n", s.Chunks)
		}
		fmt.Printf("    size       %s\n", humanBytes(s.Bytes))
		if s.Regions > 0 {
			fmt.Printf("    extent     X %d..%d  Z %d..%d\n",
				s.Bounds.MinX, s.Bounds.MaxX, s.Bounds.MinZ, s.Bounds.MaxZ)
		}
		if d.WorldBorder > 0 {
			fmt.Printf("    border     %d blocks around %d,%d\n", d.WorldBorder, d.CenterX, d.CenterZ)
		}
		fmt.Println()
	}
	return nil
}

func cmdDimensions(args []string) error {
	fs := flag.NewFlagSet("dimensions", flag.ExitOnError)
	cfgPath, demo, worldPath, logLevel := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, log, err := loadConfig(*cfgPath, *demo, *worldPath, *logLevel)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	application, err := app.Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()

	dims, err := application.Provider.Dimensions(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(dims)
}

// ---------------------------------------------------------------------------
// generate
// ---------------------------------------------------------------------------

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	cfgPath, demo, worldPath, logLevel := commonFlags(fs)
	dimension := fs.String("dimension", "", "dimension id (default: every enabled dimension)")
	modeFlag := fs.String("mode", "", "tile mode: top, iso (default: every configured mode)")
	styleFlag := fs.String("style", "", "render style (default: every configured style)")
	minX := fs.Int("min-x", 0, "minimum block X")
	maxX := fs.Int("max-x", 0, "maximum block X")
	minZ := fs.Int("min-z", 0, "minimum block Z")
	maxZ := fs.Int("max-z", 0, "maximum block Z")
	minZoom := fs.Int("min-zoom", -1, "lowest zoom to build")
	maxZoom := fs.Int("max-zoom", 0, "deepest zoom to build (default: the mode's base render zoom)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, log, err := loadConfig(*cfgPath, *demo, *worldPath, *logLevel)
	if err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()

	application, err := app.Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()

	dims, err := application.Provider.Dimensions(ctx)
	if err != nil {
		return err
	}
	targets := make([]world.DimensionInfo, 0, len(dims))
	for _, d := range dims {
		if !d.Enabled {
			continue
		}
		if *dimension == "" || d.ID == *dimension {
			targets = append(targets, d)
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("no matching dimension (have: %s)", strings.Join(dimensionIDs(dims), ", "))
	}

	modes := cfg.Tiles.Modes
	if *modeFlag != "" {
		modes = []string{*modeFlag}
	}
	styles := cfg.Styles()
	if *styleFlag != "" {
		st, ok := render.ParseStyle(*styleFlag)
		if !ok {
			return fmt.Errorf("unknown style %q", *styleFlag)
		}
		styles = []render.Style{st}
	}

	var bounds mcmath.BlockBounds
	if *minX != 0 || *maxX != 0 || *minZ != 0 || *maxZ != 0 {
		bounds = mcmath.BlockBounds{MinX: *minX, MinZ: *minZ, MaxX: *maxX, MaxZ: *maxZ}
		if bounds.Empty() {
			return fmt.Errorf("empty bounds: X %d..%d Z %d..%d", *minX, *maxX, *minZ, *maxZ)
		}
	}

	start := time.Now()
	for _, d := range targets {
		for _, mstr := range modes {
			mode, ok := tiles.ParseMode(mstr)
			if !ok {
				return fmt.Errorf("unknown mode %q", mstr)
			}
			for _, style := range styles {
				lastReport := time.Now()
				fmt.Printf("generating %s %s/%s ...\n", d.ID, mode, style)
				err := application.Pregenerate(ctx, d, mode, style, app.PregenOptions{
					Bounds:   bounds,
					MinZoom:  maxInt(*minZoom, cfg.Map.MinZoom),
					MaxZoom:  *maxZoom,
					Priority: tiles.PriorityBackground,
					Progress: func(done, total int, pos mcmath.TilePos) {
						// Rate-limit progress so redirecting to a file does not
						// produce megabytes of output.
						if time.Since(lastReport) < 500*time.Millisecond && done != total {
							return
						}
						lastReport = time.Now()
						pct := 0.0
						if total > 0 {
							pct = float64(done) / float64(total) * 100
						}
						fmt.Printf("\r  %d/%d tiles (%.1f%%) zoom %d   ", done, total, pct, pos.Zoom)
					},
				})
				fmt.Println()
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return err
					}
					return err
				}
			}
		}
	}
	st := application.Tiles.Stats()
	fmt.Printf("\ndone in %s: %d tiles generated, avg %.1f ms/tile\n",
		time.Since(start).Round(time.Second), st.Generated, st.AvgRenderMs)
	return nil
}

// ---------------------------------------------------------------------------
// invalidate
// ---------------------------------------------------------------------------

func cmdInvalidate(args []string) error {
	fs := flag.NewFlagSet("invalidate", flag.ExitOnError)
	cfgPath, demo, worldPath, logLevel := commonFlags(fs)
	dimension := fs.String("dimension", "", "dimension id (required)")
	chunkX := fs.Int("chunk-x", 0, "chunk X")
	chunkZ := fs.Int("chunk-z", 0, "chunk Z")
	regenerate := fs.Bool("regenerate", true, "rebuild the affected tiles immediately")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dimension == "" {
		return fmt.Errorf("-dimension is required")
	}
	cfg, log, err := loadConfig(*cfgPath, *demo, *worldPath, *logLevel)
	if err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()

	application, err := app.Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()

	dim, ok := application.Provider.Dimension(ctx, *dimension)
	if !ok {
		dims, _ := application.Provider.Dimensions(ctx)
		return fmt.Errorf("unknown dimension %q (have: %s)", *dimension, strings.Join(dimensionIDs(dims), ", "))
	}

	pos := mcmath.ChunkPos{X: *chunkX, Z: *chunkZ}
	application.Provider.Invalidate(dim.ID, pos)

	modes := make([]tiles.Mode, 0, len(cfg.Tiles.Modes))
	for _, m := range cfg.Tiles.Modes {
		mode, _ := tiles.ParseMode(m)
		modes = append(modes, mode)
	}
	affected := tiles.AffectedTiles(
		[]tiles.DirtyChunk{{Dimension: dim.ID, Pos: pos}},
		modes, cfg.Map.MinZoom, cfg.Map.MaxZoom, dim.MinY, dim.MaxY)

	styles := cfg.Styles()
	fmt.Printf("chunk %d,%d in %s affects %d tiles\n", pos.X, pos.Z, dim.ID, len(affected))

	for _, id := range affected {
		rev := application.Tiles.Invalidate(ctx, id, styles)
		if !*regenerate {
			fmt.Printf("  invalidated %s z%d %d,%d -> revision %d\n", id.Mode, id.Zoom, id.X, id.Y, rev)
			continue
		}
		for _, style := range styles {
			req := tiles.Request{
				Dimension: dim.ID, Mode: id.Mode, Style: style,
				Pos: mcmath.TilePos{Zoom: id.Zoom, X: id.X, Y: id.Y},
			}
			if err := application.Tiles.Regenerate(ctx, req, tiles.PriorityDirty); err != nil {
				fmt.Printf("  %s z%d %d,%d: %v\n", id.Mode, id.Zoom, id.X, id.Y, err)
			}
		}
	}
	if *regenerate {
		fmt.Printf("regenerated %d tiles across %d styles\n", len(affected), len(styles))
	}
	return nil
}

// ---------------------------------------------------------------------------
// config
// ---------------------------------------------------------------------------

func cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	out := fs.String("out", "config/config.yml", "where to write the configuration")
	force := fs.Bool("force", false, "overwrite an existing file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(*out); err == nil && !*force {
		return fmt.Errorf("%s already exists; pass -force to overwrite", *out)
	}
	cfg := config.Default()
	if err := cfg.Write(*out); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", *out)
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func dimensionIDs(dims []world.DimensionInfo) []string {
	out := make([]string, 0, len(dims))
	for _, d := range dims {
		out = append(out, d.ID)
	}
	sort.Strings(out)
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
