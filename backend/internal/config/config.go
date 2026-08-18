// Package config loads and validates the server configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/render"
)

// Config is the whole server configuration.
type Config struct {
	Server    Server    `yaml:"server"`
	Minecraft Minecraft `yaml:"minecraft"`
	Tiles     Tiles     `yaml:"tiles"`
	Map       MapCfg    `yaml:"map"`
	Overlays  Overlays  `yaml:"overlays"`
	Render    Render    `yaml:"render"`
	Data      Data      `yaml:"data"`
	Textures  Textures  `yaml:"textures"`
	Live      Live      `yaml:"live"`
	Log       Log       `yaml:"log"`
}

// Server holds HTTP settings.
type Server struct {
	Port int    `yaml:"port"`
	Host string `yaml:"host"`
	// FrontendDir, when set, serves the built frontend from the same origin,
	// which removes the need for CORS in a single-container deployment.
	FrontendDir string `yaml:"frontendDir"`
	// CORSOrigins lists origins allowed to call the API cross-origin. Empty
	// means same-origin only.
	CORSOrigins []string `yaml:"corsOrigins"`
	// ReadTimeout and WriteTimeout bound slow clients.
	ReadTimeout  time.Duration `yaml:"readTimeout"`
	WriteTimeout time.Duration `yaml:"writeTimeout"`
}

// Minecraft points at the world on disk.
type Minecraft struct {
	// WorldPath is the world directory, e.g. "./world".
	WorldPath string `yaml:"worldPath"`
	// Demo replaces the world reader with a generated sample world, so the
	// stack runs with no Minecraft installation at all.
	Demo bool `yaml:"demo"`
	// DemoSeed and DemoRadius tune the sample world.
	DemoSeed   int64 `yaml:"demoSeed"`
	DemoRadius int   `yaml:"demoRadius"`
	// ExtraDimensionPaths maps a dimension id to a directory, for modpacks
	// whose layout the discovery layer cannot infer.
	ExtraDimensionPaths map[string]string `yaml:"extraDimensionPaths"`
	// ChunkCacheBytes bounds decoded chunk surfaces held in memory.
	ChunkCacheBytes int64 `yaml:"chunkCacheBytes"`
	// RegionCacheFiles bounds how many region files stay open and mapped.
	RegionCacheFiles int `yaml:"regionCacheFiles"`
	// VoxelCacheBytes bounds the separate cache of per-chunk voxel slabs used
	// by the isometric voxel renderer (see Render.IsoVoxel). Kept apart from
	// ChunkCacheBytes because slab size varies far more per chunk than a
	// fixed-width chunk surface does.
	VoxelCacheBytes int64 `yaml:"voxelCacheBytes"`
}

// Tiles configures the tile pyramid.
type Tiles struct {
	Directory string `yaml:"directory"`
	Size      int    `yaml:"size"`
	Format    string `yaml:"format"`
	Quality   int    `yaml:"quality"`
	Workers   int    `yaml:"workers"`
	MaxQueued int    `yaml:"maxQueuedTiles"`
	// TopBaseZoom / IsoBaseZoom are the zooms rendered from world data.
	TopBaseZoom int `yaml:"topBaseZoom"`
	IsoBaseZoom int `yaml:"isoBaseZoom"`
	// MaxCompositeDepth bounds recursive on-demand pyramid building.
	MaxCompositeDepth int `yaml:"maxCompositeDepth"`
	// MemoryCacheBytes bounds the in-process encoded tile cache.
	MemoryCacheBytes int64 `yaml:"memoryCacheBytes"`
	// StoreBlankTiles writes fully-unexplored tiles to disk when true.
	StoreBlankTiles bool `yaml:"storeBlankTiles"`
	// Pregenerate runs a background sweep over the world border on startup.
	Pregenerate bool `yaml:"pregenerate"`
	// PregenerateMaxZoom bounds how deep the startup sweep goes.
	PregenerateMaxZoom int `yaml:"pregenerateMaxZoom"`
	// Styles are the render styles to generate and serve.
	Styles []string `yaml:"styles"`
	// Modes are the projections to generate and serve.
	Modes []string `yaml:"modes"`
}

// MapCfg holds client-facing map defaults.
type MapCfg struct {
	MinZoom          int     `yaml:"minZoom"`
	MaxZoom          int     `yaml:"maxZoom"`
	DefaultZoom      float64 `yaml:"defaultZoom"`
	DefaultDimension string  `yaml:"defaultDimension"`
	DefaultMode      string  `yaml:"defaultMode"`
	DefaultStyle     string  `yaml:"defaultStyle"`
	// IsoMinZoom raises the effective minimum zoom (how far the view may
	// zoom out) in isometric mode only, leaving MinZoom -- and top-down
	// mode -- untouched. <= 0 disables this, matching MinZoom exactly as
	// before this option existed. Enforced client-side only (see
	// MapEngine's zoom listener): the shared OpenLayers View still spans
	// MinZoom..MaxZoom for both modes, since MinZoom itself must stay 0 for
	// engine.zoom() to report correct values (a resolutions-array View
	// treats its zoom as an index from MinZoom, not an absolute number).
	IsoMinZoom int `yaml:"isoMinZoom"`
	// ConstrainToBorder stops panning outside the world border when true.
	ConstrainToBorder bool   `yaml:"constrainToBorder"`
	Title             string `yaml:"title"`
}

// Overlays holds zoom thresholds for progressive detail.
//
// This is serialised straight into the /api/config response (see
// api.ClientConfig.Overlays), so the json tags matter just as much as the
// yaml ones: without them encoding/json falls back to the Go field names
// verbatim (PascalCase), while the frontend's OverlayThresholds type expects
// the same camelCase names the yaml config uses. That mismatch silently
// zeroed every threshold client-side (JS reads a nonexistent property as
// undefined, and `zoom >= undefined` is always false) -- every label overlay
// this fed (claims, regions, markers, player nametags, in both render
// modes) never showed, ever, until these tags were added.
type Overlays struct {
	ChunkGridMinZoom int `yaml:"chunkGridMinZoom" json:"chunkGridMinZoom"`
	BlockGridMinZoom int `yaml:"blockGridMinZoom" json:"blockGridMinZoom"`
	PlayersMinZoom   int `yaml:"playersMinZoom" json:"playersMinZoom"`
	ClaimsMinZoom    int `yaml:"claimsMinZoom" json:"claimsMinZoom"`
	RegionsMinZoom   int `yaml:"regionsMinZoom" json:"regionsMinZoom"`
	MarkersMinZoom   int `yaml:"markersMinZoom" json:"markersMinZoom"`
	LabelsMinZoom    int `yaml:"labelsMinZoom" json:"labelsMinZoom"`
}

// Render tunes the visual treatment.
type Render struct {
	HeightShading     bool    `yaml:"heightShading"`
	ShadingStrength   float64 `yaml:"shadingStrength"`
	ShadingClamp      float64 `yaml:"shadingClamp"`
	AmbientHeight     float64 `yaml:"ambientHeight"`
	BiomeTint         bool    `yaml:"biomeTint"`
	WaterDepthShading bool    `yaml:"waterDepthShading"`
	MaxWaterDepth     float64 `yaml:"maxWaterDepth"`
	LightShading      bool    `yaml:"lightShading"`
	UnexploredColor   string  `yaml:"unexploredColor"`
	IsoCamera         string  `yaml:"isoCamera"`
	IsoEdgeSkirt      int     `yaml:"isoEdgeSkirt"`

	// IsoVoxel switches the isometric renderer from the heightmap extrusion
	// path to the real voxel path (ISO_VOXEL_PLAN.md). false keeps the
	// existing renderer byte-identical. Falls back silently, logged once,
	// when the active world provider does not support voxel data (e.g. demo
	// mode).
	IsoVoxel bool `yaml:"isoVoxel"`
	// IsoVoxelBelowGround is how many voxel layers must be stored below each
	// chunk's deepest solid surface, beyond whatever canopy height demands.
	IsoVoxelBelowGround int `yaml:"isoVoxelBelowGround"`
	// IsoVoxelMinDepth / IsoVoxelMaxDepth bound the per-chunk voxel slab
	// depth regardless of IsoVoxelBelowGround.
	IsoVoxelMinDepth int `yaml:"isoVoxelMinDepth"`
	IsoVoxelMaxDepth int `yaml:"isoVoxelMaxDepth"`
}

// Data points at the block and biome colour files.
type Data struct {
	BlocksFile  string   `yaml:"blocksFile"`
	BiomesFile  string   `yaml:"biomesFile"`
	OverlayDirs []string `yaml:"overlayDirs"`
	// FeaturesFile seeds claims, regions and markers for deployments without a
	// live server plugin.
	FeaturesFile string `yaml:"featuresFile"`
}

// Textures configures pulling real Minecraft block textures from a game
// installation, instead of the flat colours in blocks.json.
//
// Texture packs are licensed game assets nobody but the operator has the
// right to distribute, so none are ever bundled with the server: this
// section only ever points at files already on the operator's own machine.
// Resolution always degrades gracefully -- a block whose model this
// resolver cannot handle (stairs, slabs, fences, fluids, and anything with
// non-cube geometry), or whose pack does not cover it, or a deployment with
// this whole section left disabled, simply renders with its flat colour
// exactly as before. Textures are additive, never a hard requirement.
type Textures struct {
	Enabled bool `yaml:"enabled"`
	// Sources are read in priority order; a later source overrides an
	// earlier one at the same asset path, exactly like Minecraft's own
	// resource-pack stacking. Each entry is either a .jar/.zip archive, a
	// directory of *.jar/*.zip archives (e.g. a mods/ folder, loaded
	// alphabetically), or a directory of already-extracted loose assets.
	// A typical setup lists the vanilla client jar first, then a mods
	// folder, then any resource packs.
	Sources []string `yaml:"sources"`
}

// Live configures realtime updates.
type Live struct {
	Enabled bool `yaml:"enabled"`
	// PlayerPollSeconds is how often players are refreshed when no plugin is
	// pushing updates.
	PlayerPollSeconds int `yaml:"playerPollSeconds"`
	// WatchWorld polls region file modification times to detect chunk changes.
	WatchWorld bool `yaml:"watchWorld"`
	// WatchIntervalSeconds is the world polling period.
	WatchIntervalSeconds int `yaml:"watchIntervalSeconds"`
	// DirtySettleSeconds is how long a chunk must be quiet before its tiles are
	// regenerated, which coalesces rapid edits.
	DirtySettleSeconds int `yaml:"dirtySettleSeconds"`
	// MaxConnections bounds concurrent WebSocket clients.
	MaxConnections int `yaml:"maxConnections"`
	// IngestToken authorizes POST /api/ingest/* -- a server plugin pushing real
	// player positions and features, rather than a client asking for them.
	// Ingestion is refused entirely while this is empty, so a fresh deployment
	// never carries an unauthenticated remote-write endpoint by accident; an
	// operator must deliberately set a token to enable it.
	IngestToken string `yaml:"ingestToken"`
}

// Log configures structured logging.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"` // "text" or "json"
}

// Default returns a fully-populated default configuration.
func Default() Config {
	return Config{
		Server: Server{
			Port: 8080, Host: "0.0.0.0",
			ReadTimeout: 30 * time.Second, WriteTimeout: 120 * time.Second,
		},
		Minecraft: Minecraft{
			WorldPath: "./world", Demo: false,
			DemoSeed: 0x5EED1234, DemoRadius: 6000,
			ChunkCacheBytes: 256 << 20, RegionCacheFiles: 64,
			VoxelCacheBytes: 512 << 20,
		},
		Tiles: Tiles{
			Directory: "./data/tiles", Size: mcmath.TileSize,
			Format: "webp", Quality: 90,
			Workers: max(2, runtime.NumCPU()), MaxQueued: 5000,
			TopBaseZoom: 6, IsoBaseZoom: 8, MaxCompositeDepth: 3,
			MemoryCacheBytes: 192 << 20, StoreBlankTiles: false,
			Pregenerate: false, PregenerateMaxZoom: 6,
			Styles: []string{"terrain"}, Modes: []string{"top", "iso"},
		},
		Map: MapCfg{
			MinZoom: 0, MaxZoom: 10, DefaultZoom: 5,
			DefaultDimension: "minecraft:overworld",
			DefaultMode:      "top", DefaultStyle: "terrain",
			IsoMinZoom:        mcmath.BaseZoom,
			ConstrainToBorder: false, Title: "EnderFall Map",
		},
		Overlays: Overlays{
			ChunkGridMinZoom: 7, BlockGridMinZoom: 9,
			PlayersMinZoom: 2, ClaimsMinZoom: 1, RegionsMinZoom: 0,
			MarkersMinZoom: 3, LabelsMinZoom: 4,
		},
		Render: Render{
			HeightShading: true, ShadingStrength: 0.35, ShadingClamp: 3,
			AmbientHeight: 0.10, BiomeTint: true,
			WaterDepthShading: true, MaxWaterDepth: 28, LightShading: false,
			UnexploredColor: "#14161a", IsoCamera: "se", IsoEdgeSkirt: 4,
			IsoVoxel: true, IsoVoxelBelowGround: 16, IsoVoxelMinDepth: 8, IsoVoxelMaxDepth: 64,
		},
		Data: Data{
			BlocksFile: "./config/blocks.json",
			BiomesFile: "./config/biomes.json",
		},
		Live: Live{
			Enabled: true, PlayerPollSeconds: 2,
			WatchWorld: true, WatchIntervalSeconds: 5,
			DirtySettleSeconds: 3, MaxConnections: 500,
		},
		Log: Log{Level: "info", Format: "text"},
	}
}

// Load reads a YAML configuration file, applying defaults for anything absent.
// An empty path returns the defaults, so the server runs with no config file.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, cfg.Validate()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	// Unmarshalling onto the defaults means absent keys keep their default
	// rather than becoming zero, which is what makes partial config files safe.
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	// Paths in a config file are naturally relative to that file.
	base := filepath.Dir(path)
	cfg.resolvePaths(base)
	return cfg, cfg.Validate()
}

// resolvePaths makes relative paths relative to the config file's directory.
func (c *Config) resolvePaths(base string) {
	rel := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	c.Minecraft.WorldPath = rel(c.Minecraft.WorldPath)
	c.Tiles.Directory = rel(c.Tiles.Directory)
	c.Data.BlocksFile = rel(c.Data.BlocksFile)
	c.Data.BiomesFile = rel(c.Data.BiomesFile)
	c.Data.FeaturesFile = rel(c.Data.FeaturesFile)
	c.Server.FrontendDir = rel(c.Server.FrontendDir)
	for i, d := range c.Data.OverlayDirs {
		c.Data.OverlayDirs[i] = rel(d)
	}
	for i, s := range c.Textures.Sources {
		c.Textures.Sources[i] = rel(s)
	}
	for k, v := range c.Minecraft.ExtraDimensionPaths {
		c.Minecraft.ExtraDimensionPaths[k] = rel(v)
	}
}

// Validate checks the configuration for values that would misbehave at runtime,
// correcting what is safely correctable and rejecting what is not.
func (c *Config) Validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d out of range", c.Server.Port)
	}
	if c.Tiles.Size != mcmath.TileSize {
		return fmt.Errorf("tiles.size must be %d; the pyramid maths depends on it", mcmath.TileSize)
	}
	if c.Map.MinZoom < 0 || c.Map.MaxZoom > mcmath.MaxIntegerZoom || c.Map.MinZoom >= c.Map.MaxZoom {
		return fmt.Errorf("map zoom range %d..%d invalid", c.Map.MinZoom, c.Map.MaxZoom)
	}
	if c.Tiles.Workers < 1 {
		c.Tiles.Workers = 1
	}
	if c.Tiles.MaxQueued < c.Tiles.Workers {
		c.Tiles.MaxQueued = c.Tiles.Workers * 16
	}
	// The isometric lattice is only pixel-alignable from render.MinDirectZoom.
	if c.Tiles.IsoBaseZoom < render.MinDirectZoom {
		c.Tiles.IsoBaseZoom = render.MinDirectZoom
	}
	if c.Tiles.TopBaseZoom < c.Map.MinZoom {
		c.Tiles.TopBaseZoom = c.Map.MinZoom
	}
	if c.Tiles.TopBaseZoom > c.Map.MaxZoom || c.Tiles.IsoBaseZoom > c.Map.MaxZoom {
		return fmt.Errorf("base render zooms must fall inside the map zoom range")
	}
	if len(c.Tiles.Styles) == 0 {
		c.Tiles.Styles = []string{"terrain"}
	}
	for _, s := range c.Tiles.Styles {
		if _, ok := render.ParseStyle(s); !ok {
			return fmt.Errorf("unknown render style %q", s)
		}
	}
	if len(c.Tiles.Modes) == 0 {
		c.Tiles.Modes = []string{"top"}
	}
	for _, m := range c.Tiles.Modes {
		if m != "top" && m != "iso" {
			return fmt.Errorf("unknown tile mode %q", m)
		}
	}
	if _, ok := mcmath.ParseIsoCamera(c.Render.IsoCamera); !ok {
		return fmt.Errorf("unknown iso camera %q", c.Render.IsoCamera)
	}
	if c.Minecraft.WorldPath == "" && !c.Minecraft.Demo {
		return fmt.Errorf("minecraft.worldPath is required unless minecraft.demo is true")
	}
	if c.Tiles.Directory == "" {
		return fmt.Errorf("tiles.directory is required")
	}
	if c.Live.DirtySettleSeconds < 0 {
		c.Live.DirtySettleSeconds = 0
	}
	if c.Render.IsoEdgeSkirt < 0 {
		c.Render.IsoEdgeSkirt = 0
	}
	if c.Map.IsoMinZoom > c.Map.MaxZoom {
		c.Map.IsoMinZoom = c.Map.MaxZoom
	}
	if c.Map.IsoMinZoom > 0 && c.Map.IsoMinZoom < c.Map.MinZoom {
		c.Map.IsoMinZoom = c.Map.MinZoom
	}
	if c.Render.IsoVoxelMinDepth <= 0 {
		c.Render.IsoVoxelMinDepth = 8
	}
	if c.Render.IsoVoxelMaxDepth < c.Render.IsoVoxelMinDepth {
		c.Render.IsoVoxelMaxDepth = c.Render.IsoVoxelMinDepth
	}
	if c.Render.IsoVoxelBelowGround < 0 {
		c.Render.IsoVoxelBelowGround = 0
	}
	return nil
}

// RenderOptions converts the config into renderer options.
func (c *Config) RenderOptions() (render.Options, error) {
	opts := render.DefaultOptions()
	opts.HeightShading = c.Render.HeightShading
	opts.BiomeTint = c.Render.BiomeTint
	opts.WaterDepthShading = c.Render.WaterDepthShading
	opts.LightShading = c.Render.LightShading
	if c.Render.ShadingStrength > 0 {
		opts.ShadingStrength = c.Render.ShadingStrength
	}
	if c.Render.ShadingClamp > 0 {
		opts.ShadingClamp = c.Render.ShadingClamp
	}
	if c.Render.AmbientHeight >= 0 {
		opts.AmbientHeight = c.Render.AmbientHeight
	}
	if c.Render.MaxWaterDepth > 0 {
		opts.MaxWaterDepth = c.Render.MaxWaterDepth
	}
	if c.Render.UnexploredColor != "" {
		col, err := parseHex(c.Render.UnexploredColor)
		if err != nil {
			return opts, fmt.Errorf("render.unexploredColor: %w", err)
		}
		opts.UnexploredColor = col
	}
	return opts, nil
}

// Styles returns the configured styles as parsed values.
func (c *Config) Styles() []render.Style {
	out := make([]render.Style, 0, len(c.Tiles.Styles))
	for _, s := range c.Tiles.Styles {
		st, _ := render.ParseStyle(s)
		out = append(out, st)
	}
	return out
}

// HasMode reports whether a projection is enabled.
func (c *Config) HasMode(m string) bool {
	for _, s := range c.Tiles.Modes {
		if strings.EqualFold(s, m) {
			return true
		}
	}
	return false
}

// Write saves the configuration to a YAML file, used by the CLI to emit a
// starter config.
func (c *Config) Write(path string) error {
	raw, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}
