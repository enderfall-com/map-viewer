// Package api exposes the HTTP surface: tiles, world metadata, overlay
// features and the realtime endpoint.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/enderfall/minecraft-map/backend/internal/blocks"
	"github.com/enderfall/minecraft-map/backend/internal/config"
	"github.com/enderfall/minecraft-map/backend/internal/features"
	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/realtime"
	"github.com/enderfall/minecraft-map/backend/internal/render"
	"github.com/enderfall/minecraft-map/backend/internal/tiles"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

// Server holds everything the HTTP handlers need.
type Server struct {
	Cfg      config.Config
	Tiles    *tiles.Manager
	Provider *world.Cached
	Features *features.Memory
	Hub      *realtime.Hub
	Blocks   *blocks.Registry
	Biomes   *blocks.Biomes
	Log      *slog.Logger

	started time.Time
	// safeDims maps a sanitised dimension token back to its canonical id.
	// Requests are resolved through this map rather than by transforming the
	// URL into a path, which is what makes directory traversal impossible: an
	// identifier that is not a known dimension simply has no entry.
	safeDims map[string]string
	// skins resolves and caches real player skin textures for handleSkin.
	skins *skinResolver
}

// New builds the API server.
func New(
	cfg config.Config,
	tm *tiles.Manager,
	provider *world.Cached,
	feat *features.Memory,
	hub *realtime.Hub,
	reg *blocks.Registry,
	bio *blocks.Biomes,
	log *slog.Logger,
) (*Server, error) {
	s := &Server{
		Cfg: cfg, Tiles: tm, Provider: provider, Features: feat, Hub: hub,
		Blocks: reg, Biomes: bio, Log: log, started: time.Now(),
		skins: newSkinResolver(),
	}
	if err := s.refreshDimensions(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

// refreshDimensions rebuilds the sanitised-id lookup table.
func (s *Server) refreshDimensions(ctx context.Context) error {
	dims, err := s.Provider.Dimensions(ctx)
	if err != nil {
		return fmt.Errorf("list dimensions: %w", err)
	}
	m := make(map[string]string, len(dims)*2)
	for _, d := range dims {
		if !d.Enabled {
			continue
		}
		m[cacheSafe(d.ID)] = d.ID
		m[d.ID] = d.ID // accept the canonical id directly too
	}
	s.safeDims = m
	return nil
}

// Handler builds the HTTP router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/config", s.handleClientConfig)
	mux.HandleFunc("GET /api/worlds", s.handleWorlds)
	mux.HandleFunc("GET /api/dimensions", s.handleDimensions)
	mux.HandleFunc("GET /api/dimensions/{id...}", s.handleDimension)
	mux.HandleFunc("GET /api/features", s.handleFeatures)
	mux.HandleFunc("GET /api/players", s.handlePlayers)
	mux.HandleFunc("GET /api/claims", s.handleClaims)
	mux.HandleFunc("GET /api/regions", s.handleRegions)
	mux.HandleFunc("GET /api/markers", s.handleMarkers)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/pick", s.handlePick)
	mux.HandleFunc("GET /api/metrics", s.handleMetrics)
	mux.HandleFunc("GET /api/blocks/unknown", s.handleUnknownBlocks)
	mux.HandleFunc("GET /api/skin/{uuid}", s.handleSkin)

	// Ingestion: a server plugin pushing real player positions and features.
	// Every handler enforces its own bearer-token check, since it is disabled
	// by default and unrelated to the CORS/session model the read endpoints use.
	mux.HandleFunc("POST /api/ingest/players", s.handleIngestPlayers)
	mux.HandleFunc("POST /api/ingest/areas", s.handleIngestAreas)
	mux.HandleFunc("POST /api/ingest/markers", s.handleIngestMarkers)

	// Chunk selection actions, driven directly by the map UI. Unlike
	// ingestion these are not token-gated: they are a map-side bookkeeping
	// feature, not a channel for a trusted server plugin to push authoritative
	// world state.
	mux.HandleFunc("POST /api/chunks/claim", s.handleClaimChunks)
	mux.HandleFunc("POST /api/chunks/unclaim", s.handleUnclaimChunks)
	mux.HandleFunc("POST /api/chunks/forceload", s.handleForceLoadChunks)

	// Tile route. The revision segment is a cache-busting token: it is
	// validated but never used to select content, because storage keeps only
	// the current image. See tiles.Revisions for why.
	mux.HandleFunc("GET /tiles/{dim}/{mode}/{rev}/{z}/{x}/{y}", s.handleTile)
	// Revision-less form, useful for debugging and for clients that do their
	// own cache busting.
	mux.HandleFunc("GET /tiles/{dim}/{mode}/{z}/{x}/{y}", s.handleTileNoRev)

	if s.Hub != nil {
		mux.HandleFunc("GET /ws", s.Hub.ServeHTTP)
	}

	if dir := s.Cfg.Server.FrontendDir; dir != "" {
		s.mountFrontend(mux, dir)
	}

	return s.withMiddleware(mux)
}

// mountFrontend serves the built single-page app, falling back to index.html so
// deep links like /map?x=... work on refresh.
func (s *Server) mountFrontend(mux *http.ServeMux, dir string) {
	fs := http.FileServer(http.Dir(dir))
	index := filepath.Join(dir, "index.html")

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if clean == "." || clean == "" {
			http.ServeFile(w, r, index)
			return
		}
		full := filepath.Join(dir, clean)
		// Confine to the frontend directory.
		if rel, err := filepath.Rel(dir, full); err != nil || strings.HasPrefix(rel, "..") {
			http.NotFound(w, r)
			return
		}
		if st, err := os.Stat(full); err == nil && !st.IsDir() {
			// Hashed build assets are safe to cache hard; index.html is not.
			if strings.HasPrefix(clean, "assets"+string(os.PathSeparator)) {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fs.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, index)
	})
}

// withMiddleware adds logging, panic recovery and CORS.
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		if origin := r.Header.Get("Origin"); origin != "" && s.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if rv := recover(); rv != nil {
				// A panic in one handler must not take down the server.
				s.Log.Error("handler panic", "path", r.URL.Path, "panic", rv)
				if !rec.wrote {
					http.Error(rec, "internal error", http.StatusInternalServerError)
				}
			}
			// API latency, at debug level so tile floods do not drown the log.
			s.Log.Debug("http",
				"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"duration_ms", time.Since(start).Milliseconds())
		}()

		next.ServeHTTP(rec, r)
	})
}

func (s *Server) originAllowed(origin string) bool {
	for _, o := range s.Cfg.Server.CORSOrigins {
		if o == "*" || strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
		r.ResponseWriter.WriteHeader(code)
	}
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// Hijack forwards to the underlying ResponseWriter.
//
// Wrapping a ResponseWriter hides any optional interface it implements. The
// WebSocket upgrade needs http.Hijacker to take over the raw connection, so
// without this the realtime endpoint fails for every client while the rest of
// the API keeps working -- a failure that is easy to miss.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("connection does not support hijacking")
	}
	r.wrote = true
	return hj.Hijack()
}

// Flush forwards to the underlying ResponseWriter for the same reason, so
// streaming responses are not buffered until the handler returns.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The response is already committed; nothing useful remains to be done.
		return
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// resolveDimension maps a request's dimension parameter to a known dimension,
// rejecting anything unrecognised. This is the single choke point that keeps
// user input from reaching the filesystem.
func (s *Server) resolveDimension(ctx context.Context, raw string) (world.DimensionInfo, bool) {
	if raw == "" {
		raw = s.Cfg.Map.DefaultDimension
	}
	id, ok := s.safeDims[raw]
	if !ok {
		id, ok = s.safeDims[cacheSafe(raw)]
	}
	if !ok {
		return world.DimensionInfo{}, false
	}
	return s.Provider.Dimension(ctx, id)
}

func intParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func floatParam(r *http.Request, name string, def float64) float64 {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// ---------------------------------------------------------------------------
// Metadata endpoints
// ---------------------------------------------------------------------------

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	areas, markers, players := s.Features.Counts()
	dims, _ := s.Provider.Dimensions(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"uptime":      time.Since(s.started).Round(time.Second).String(),
		"dimensions":  len(dims),
		"blocks":      s.Blocks.Len(),
		"biomes":      s.Biomes.Len(),
		"areas":       areas,
		"markers":     markers,
		"players":     players,
		"connections": s.Hub.Connections(),
	})
}

// ClientConfig is the bootstrap payload the frontend loads before building the
// map, so zoom thresholds and defaults live in one place on the server.
type ClientConfig struct {
	Title           string `json:"title"`
	TileURLTemplate string `json:"tileUrlTemplate"`
	TileSize        int    `json:"tileSize"`
	BaseZoom        int    `json:"baseZoom"`
	MinZoom         int    `json:"minZoom"`
	MaxZoom         int    `json:"maxZoom"`
	// IsoMinZoom raises the effective minimum zoom in isometric mode only.
	// <= 0 means no restriction beyond MinZoom.
	IsoMinZoom       int                   `json:"isoMinZoom"`
	TopMaxDataZoom   int                   `json:"topMaxDataZoom"`
	IsoMaxDataZoom   int                   `json:"isoMaxDataZoom"`
	DefaultZoom      float64               `json:"defaultZoom"`
	DefaultDimension string                `json:"defaultDimension"`
	DefaultMode      string                `json:"defaultMode"`
	DefaultStyle     string                `json:"defaultStyle"`
	Modes            []string              `json:"modes"`
	Styles           []string              `json:"styles"`
	Overlays         config.Overlays       `json:"overlays"`
	IsoCamera        string                `json:"isoCamera"`
	Iso              IsoClientConfig       `json:"iso"`
	Live             bool                  `json:"live"`
	ConstrainBorder  bool                  `json:"constrainToBorder"`
	Dimensions       []world.DimensionInfo `json:"dimensions"`
}

// IsoClientConfig carries the projection constants so the frontend never has to
// hard-code them and can never drift from the renderer.
type IsoClientConfig struct {
	HalfWidth     float64 `json:"halfWidth"`
	HalfHeight    float64 `json:"halfHeight"`
	BlockHeight   float64 `json:"blockHeight"`
	MinDirectZoom int     `json:"minDirectZoom"`
}

func (s *Server) handleClientConfig(w http.ResponseWriter, r *http.Request) {
	dims, err := s.Provider.Dimensions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot list dimensions")
		return
	}
	enabled := make([]world.DimensionInfo, 0, len(dims))
	for _, d := range dims {
		if d.Enabled {
			enabled = append(enabled, d)
		}
	}
	writeJSON(w, http.StatusOK, ClientConfig{
		Title:            s.Cfg.Map.Title,
		TileURLTemplate:  "/tiles/{dimension}/{mode}/{revision}/{z}/{x}/{y}." + s.Cfg.Tiles.Format,
		TileSize:         mcmath.TileSize,
		BaseZoom:         mcmath.BaseZoom,
		MinZoom:          s.Cfg.Map.MinZoom,
		MaxZoom:          s.Cfg.Map.MaxZoom,
		IsoMinZoom:       s.Cfg.Map.IsoMinZoom,
		TopMaxDataZoom:   s.Cfg.Tiles.TopBaseZoom,
		IsoMaxDataZoom:   s.Cfg.Tiles.IsoBaseZoom,
		DefaultZoom:      s.Cfg.Map.DefaultZoom,
		DefaultDimension: s.Cfg.Map.DefaultDimension,
		DefaultMode:      s.Cfg.Map.DefaultMode,
		DefaultStyle:     s.Cfg.Map.DefaultStyle,
		Modes:            s.Cfg.Tiles.Modes,
		Styles:           s.Cfg.Tiles.Styles,
		Overlays:         s.Cfg.Overlays,
		IsoCamera:        s.Cfg.Render.IsoCamera,
		Iso: IsoClientConfig{
			HalfWidth:     mcmath.IsoHalfWidth,
			HalfHeight:    mcmath.IsoHalfHeight,
			BlockHeight:   mcmath.IsoBlockHeight,
			MinDirectZoom: render.MinDirectZoom,
		},
		Live:            s.Cfg.Live.Enabled,
		ConstrainBorder: s.Cfg.Map.ConstrainToBorder,
		Dimensions:      enabled,
	})
}

func (s *Server) handleWorlds(w http.ResponseWriter, r *http.Request) {
	dims, err := s.Provider.Dimensions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot list dimensions")
		return
	}
	writeJSON(w, http.StatusOK, []map[string]any{{
		"id":         "default",
		"name":       s.Cfg.Map.Title,
		"dimensions": len(dims),
	}})
}

func (s *Server) handleDimensions(w http.ResponseWriter, r *http.Request) {
	dims, err := s.Provider.Dimensions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot list dimensions")
		return
	}
	out := make([]world.DimensionInfo, 0, len(dims))
	for _, d := range dims {
		if d.Enabled {
			out = append(out, d)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDimension(w http.ResponseWriter, r *http.Request) {
	dim, ok := s.resolveDimension(r.Context(), r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown dimension")
		return
	}
	writeJSON(w, http.StatusOK, dim)
}

func (s *Server) handleUnknownBlocks(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 200)
	writeJSON(w, http.StatusOK, map[string]any{
		"blocks": s.Blocks.Unknown(limit),
		"biomes": s.Biomes.Unknown(limit),
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"tiles":       s.Tiles.Stats(),
		"connections": s.Hub.Connections(),
		"uptime_s":    int(time.Since(s.started).Seconds()),
	})
}

// ---------------------------------------------------------------------------
// Feature endpoints
// ---------------------------------------------------------------------------

// maxFeatureQuerySpan caps how much world one features request may cover, so a
// client cannot ask for every claim in a hundred-million-block world at once.
const maxFeatureQuerySpan = 2_000_000

func (s *Server) handleFeatures(w http.ResponseWriter, r *http.Request) {
	dim, ok := s.resolveDimension(r.Context(), r.URL.Query().Get("dimension"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown dimension")
		return
	}
	b := mcmath.BlockBounds{
		MinX: intParam(r, "minX", -30_000_000),
		MinZ: intParam(r, "minZ", -30_000_000),
		MaxX: intParam(r, "maxX", 30_000_000),
		MaxZ: intParam(r, "maxZ", 30_000_000),
	}
	if b.MaxX < b.MinX || b.MaxZ < b.MinZ {
		writeError(w, http.StatusBadRequest, "inverted bounds")
		return
	}
	if b.Width() > maxFeatureQuerySpan || b.Height() > maxFeatureQuerySpan {
		writeError(w, http.StatusBadRequest, "query area too large")
		return
	}

	var kinds []string
	if k := r.URL.Query().Get("kinds"); k != "" {
		kinds = strings.Split(k, ",")
	}
	set, err := s.Features.Query(r.Context(), features.Query{
		Dimension: dim.ID,
		Bounds:    b,
		Zoom:      intParam(r, "zoom", -1),
		Limit:     min(intParam(r, "limit", 5000), 20000),
		Kinds:     kinds,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "feature query failed")
		return
	}
	// Overlays change constantly; never let a proxy hold them.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, set)
}

func (s *Server) handlePlayers(w http.ResponseWriter, r *http.Request) {
	dimension := ""
	if raw := r.URL.Query().Get("dimension"); raw != "" {
		dim, ok := s.resolveDimension(r.Context(), raw)
		if !ok {
			writeError(w, http.StatusNotFound, "unknown dimension")
			return
		}
		dimension = dim.ID
	}
	players, err := s.Features.Players(r.Context(), dimension)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "player query failed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, players)
}

func (s *Server) handleAreasOfKind(w http.ResponseWriter, r *http.Request, kind string) {
	dimension := ""
	if raw := r.URL.Query().Get("dimension"); raw != "" {
		dim, ok := s.resolveDimension(r.Context(), raw)
		if !ok {
			writeError(w, http.StatusNotFound, "unknown dimension")
			return
		}
		dimension = dim.ID
	}
	areas, err := s.Features.Areas(r.Context(), dimension, kind)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "area query failed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, areas)
}

func (s *Server) handleClaims(w http.ResponseWriter, r *http.Request) {
	s.handleAreasOfKind(w, r, "claim")
}

func (s *Server) handleRegions(w http.ResponseWriter, r *http.Request) {
	s.handleAreasOfKind(w, r, "region")
}

func (s *Server) handleMarkers(w http.ResponseWriter, r *http.Request) {
	dimension := ""
	if raw := r.URL.Query().Get("dimension"); raw != "" {
		dim, ok := s.resolveDimension(r.Context(), raw)
		if !ok {
			writeError(w, http.StatusNotFound, "unknown dimension")
			return
		}
		dimension = dim.ID
	}
	markers, err := s.Features.Markers(r.Context(), dimension, r.URL.Query().Get("kind"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marker query failed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, markers)
}

// handleSearch resolves a search term to map locations.
//
// Bare coordinates are recognised directly, so typing "1250 -8220" jumps
// straight there without a round trip through the feature index.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	term := strings.TrimSpace(r.URL.Query().Get("q"))
	if term == "" {
		writeJSON(w, http.StatusOK, []features.SearchResult{})
		return
	}
	dimension := ""
	if raw := r.URL.Query().Get("dimension"); raw != "" {
		if dim, ok := s.resolveDimension(r.Context(), raw); ok {
			dimension = dim.ID
		}
	}

	results := []features.SearchResult{}
	if x, z, ok := parseCoordinates(term); ok {
		results = append(results, features.SearchResult{
			Type: "coordinates", ID: "coord", Name: fmt.Sprintf("%d, %d", x, z),
			Dimension: dimension, X: x, Z: z,
		})
	}
	results = append(results, s.Features.Search(dimension, term, intParam(r, "limit", 15))...)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, results)
}

// parseCoordinates recognises "1250 -8220", "1250,-8220" and "1250 72 -8220".
func parseCoordinates(s string) (x, z int, ok bool) {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == ';'
	})
	nums := make([]int, 0, 3)
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil {
			return 0, 0, false
		}
		nums = append(nums, n)
	}
	switch len(nums) {
	case 2:
		return nums[0], nums[1], true
	case 3:
		// X Y Z, as copied out of the F3 screen.
		return nums[0], nums[2], true
	}
	return 0, 0, false
}

// ---------------------------------------------------------------------------
// Block picking
// ---------------------------------------------------------------------------

// PickResult is the structured answer to a map click.
type PickResult struct {
	Dimension string `json:"dimension"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	Z         int    `json:"z"`
	ChunkX    int    `json:"chunkX"`
	ChunkZ    int    `json:"chunkZ"`
	RegionX   int    `json:"regionX"`
	RegionZ   int    `json:"regionZ"`
	Block     string `json:"block"`
	Biome     string `json:"biome"`
	Water     bool   `json:"water,omitempty"`
	WaterY    int    `json:"waterY,omitempty"`
	Light     int    `json:"light"`
	Found     bool   `json:"found"`
}

// handlePick resolves a map position to a Minecraft block.
//
// In top-down mode the position is already block coordinates and the answer
// is exact by construction. In isometric mode the request carries iso-space
// coordinates. When real voxel data is available (render.isoVoxel enabled
// and the active provider supports it), the server marches the ray against
// actual per-voxel occlusion (ISO_VOXEL_PLAN.md §3, Phase 3), so a click on
// a canopy, the ground visible through a real gap beneath it, or a lower
// voxel like a trunk each resolve to the specific block actually visible
// there -- matching what render/iso_voxel.go draws -- rather than always
// answering with the column's topmost block. If that does not resolve a hit
// (feature disabled, provider unsupported, or the click falls outside the
// tightened voxel window), this falls back to the flattened-height-field ray
// march exactly as before Phase 3 existed: voxel picking can only improve
// accuracy, never regress it.
func (s *Server) handlePick(w http.ResponseWriter, r *http.Request) {
	dim, ok := s.resolveDimension(r.Context(), r.URL.Query().Get("dimension"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown dimension")
		return
	}
	mode, _ := tiles.ParseMode(r.URL.Query().Get("mode"))

	var bx, bz int
	var surf *world.Surface
	ctx := r.Context()

	if mode == tiles.ModeIso {
		u := floatParam(r, "u", 0)
		v := floatParam(r, "v", 0)
		cam, _ := mcmath.ParseIsoCamera(s.Cfg.Render.IsoCamera)
		proj := mcmath.NewIsoProjection(cam)

		// The ray can only reach columns inside the elevation band, so load a
		// window sized by the same inverse-projection maths the tile renderer
		// uses for overscan -- just for a single pixel rather than a tile.
		probe := mcmath.IsoBounds{MinU: u, MinV: v, MaxU: u + 1, MaxV: v + 1}
		bounds := proj.WorldFootprint(probe, dim.MinY, dim.MaxY)
		var err error
		surf, err = world.Assemble(ctx, s.Provider, dim.ID, bounds, dim.MinY, dim.MaxY, nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "world read failed")
			return
		}

		if s.Cfg.Render.IsoVoxel && s.Provider.SupportsVoxels() {
			if hx, hy, hz, id, light, vok := s.voxelPick(ctx, dim, proj, probe, surf, u, v); vok {
				col := surf.At(hx, hz)
				res := PickResult{
					Dimension: dim.ID, X: hx, Y: hy, Z: hz,
					ChunkX:  mcmath.BlockToChunk(hx),
					ChunkZ:  mcmath.BlockToChunk(hz),
					RegionX: mcmath.BlockToRegion(hx),
					RegionZ: mcmath.BlockToRegion(hz),
					Found:   true,
					Block:   s.Blocks.Get(id).Name,
					Biome:   s.Biomes.Get(col.Biome).Name,
					Light:   int(light),
				}
				if col.Water() {
					res.Water = true
					res.WaterY = col.WaterY
				}
				w.Header().Set("Cache-Control", "no-store")
				writeJSON(w, http.StatusOK, res)
				return
			}
		}

		x, _, z, hit := proj.RayMarch(u, v, dim.MinY, dim.MaxY, surf)
		if !hit {
			writeJSON(w, http.StatusOK, PickResult{Dimension: dim.ID, Found: false})
			return
		}
		bx, bz = x, z
	} else {
		bx = intParam(r, "x", 0)
		bz = intParam(r, "z", 0)
		b := mcmath.BlockBounds{MinX: bx - 1, MinZ: bz - 1, MaxX: bx + 2, MaxZ: bz + 2}
		var err error
		surf, err = world.Assemble(ctx, s.Provider, dim.ID, b, dim.MinY, dim.MaxY, nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "world read failed")
			return
		}
	}

	col := surf.At(bx, bz)
	res := PickResult{
		Dimension: dim.ID,
		X:         bx,
		Z:         bz,
		ChunkX:    mcmath.BlockToChunk(bx),
		ChunkZ:    mcmath.BlockToChunk(bz),
		RegionX:   mcmath.BlockToRegion(bx),
		RegionZ:   mcmath.BlockToRegion(bz),
		Found:     col.Present(),
	}
	if col.Present() {
		res.Y = col.Height
		res.Block = s.Blocks.Get(col.Block).Name
		res.Biome = s.Biomes.Get(col.Biome).Name
		res.Light = int(col.Light)
		if col.Water() {
			res.Water = true
			res.WaterY = col.WaterY
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, res)
}

// voxelPick attempts the voxel-aware hit test for one isometric pick request.
// ok=false covers every reason to fall back to the flattened-height ray
// march -- world-read failure, or the ray finding no occluding voxel within
// the tightened voxel window -- never "confirmed nothing is there", which is
// what a plain "not found" answer must mean instead.
func (s *Server) voxelPick(ctx context.Context, dim world.DimensionInfo, proj mcmath.IsoProjection, probe mcmath.IsoBounds, surf *world.Surface, u, v float64) (x, y, z int, blockID uint16, light uint8, ok bool) {
	lo, hi, hok := surf.HeightRange()
	if !hok {
		return 0, 0, 0, 0, 0, false
	}
	// Mirrors tiles.Manager.attachVolume's §4.5 window tightening exactly,
	// so a pick and the tile the user is looking at agree on what "the
	// voxel window" means.
	bounds := proj.WorldFootprint(probe, lo-s.Cfg.Render.IsoVoxelMaxDepth, hi).Expand(1)
	vol, err := world.AssembleVolume(ctx, s.Provider, dim.ID, bounds, dim.MinY, dim.MaxY, nil)
	if err != nil {
		s.Log.Warn("voxel pick assembly failed, falling back to heightmap pick", "dimension", dim.ID, "error", err)
		return 0, 0, 0, 0, 0, false
	}
	hx, hy, hz, hit := world.VoxelRayMarch(proj, u, v, dim.MinY, dim.MaxY, vol, func(id uint16) bool {
		return s.Blocks.Get(id).Occludes
	})
	if !hit {
		return 0, 0, 0, 0, 0, false
	}
	id, lgt, bok := vol.BlockAt(hx, hy, hz)
	if !bok {
		return 0, 0, 0, 0, 0, false
	}
	return hx, hy, hz, id, lgt, true
}

// ---------------------------------------------------------------------------
// Tiles
// ---------------------------------------------------------------------------

func (s *Server) handleTileNoRev(w http.ResponseWriter, r *http.Request) {
	s.serveTile(w, r, r.PathValue("dim"), r.PathValue("mode"),
		r.PathValue("z"), r.PathValue("x"), r.PathValue("y"), false)
}

func (s *Server) handleTile(w http.ResponseWriter, r *http.Request) {
	// The revision must be a plausible token, but its value never selects
	// content. Validating it keeps junk out of logs and metrics.
	rev := r.PathValue("rev")
	if len(rev) > 24 {
		writeError(w, http.StatusBadRequest, "bad revision")
		return
	}
	for _, c := range rev {
		if c < '0' || c > '9' {
			writeError(w, http.StatusBadRequest, "bad revision")
			return
		}
	}
	s.serveTile(w, r, r.PathValue("dim"), r.PathValue("mode"),
		r.PathValue("z"), r.PathValue("x"), r.PathValue("y"), true)
}

func (s *Server) serveTile(w http.ResponseWriter, r *http.Request, dimRaw, modeRaw, zRaw, xRaw, yRaw string, immutable bool) {
	dim, ok := s.resolveDimension(r.Context(), dimRaw)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown dimension")
		return
	}
	mode, ok := tiles.ParseMode(modeRaw)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown mode")
		return
	}
	if !s.Cfg.HasMode(string(mode)) {
		writeError(w, http.StatusNotFound, "mode not enabled")
		return
	}

	// Strip the extension from the final segment.
	ext := ""
	if i := strings.LastIndexByte(yRaw, '.'); i >= 0 {
		ext, yRaw = yRaw[i+1:], yRaw[:i]
	}
	if ext != "" && ext != s.Cfg.Tiles.Format {
		writeError(w, http.StatusNotFound, "unsupported tile format")
		return
	}

	z, err1 := strconv.Atoi(zRaw)
	x, err2 := strconv.Atoi(xRaw)
	y, err3 := strconv.Atoi(yRaw)
	if err1 != nil || err2 != nil || err3 != nil {
		writeError(w, http.StatusBadRequest, "bad tile coordinates")
		return
	}
	if z < s.Cfg.Map.MinZoom || z > s.Cfg.Map.MaxZoom {
		writeError(w, http.StatusNotFound, "zoom out of range")
		return
	}
	// Reject tile indices that cannot correspond to any real world position.
	// Without this a client could walk the server through an unbounded number
	// of empty tiles far outside the world.
	if !tileIndexPlausible(z, x, y) {
		writeError(w, http.StatusNotFound, "tile out of range")
		return
	}

	style, ok := render.ParseStyle(r.URL.Query().Get("style"))
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown style")
		return
	}

	req := tiles.Request{
		Dimension: dim.ID,
		Mode:      mode,
		Style:     style,
		Pos:       mcmath.TilePos{Zoom: z, X: x, Y: y},
	}

	data, err := s.Tiles.Tile(r.Context(), req, tiles.PriorityUser)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			// The browser panned away; nothing to report.
			return
		case errors.Is(err, tiles.ErrQueueFull):
			w.Header().Set("Retry-After", "2")
			writeError(w, http.StatusServiceUnavailable, "tile queue saturated")
		default:
			s.Log.Error("tile generation failed",
				"dimension", dim.ID, "mode", string(mode),
				"tile_z", z, "tile_x", x, "tile_y", y, "error", err)
			writeError(w, http.StatusInternalServerError, "tile generation failed")
		}
		return
	}

	w.Header().Set("Content-Type", s.tileContentType())
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if immutable {
		// Safe because a changed tile is served under a new revision URL.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=60")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) tileContentType() string {
	f, _ := tiles.ParseFormat(s.Cfg.Tiles.Format)
	return f.ContentType()
}

// tileIndexPlausible rejects tile coordinates outside Minecraft's maximum world
// extent of +/-30,000,000 blocks, with a generous margin.
func tileIndexPlausible(zoom, x, y int) bool {
	span := mcmath.TileSpanBlocks(zoom)
	const limit = 40_000_000
	maxIndex := limit/span + 2
	return x >= -maxIndex && x <= maxIndex && y >= -maxIndex && y <= maxIndex
}
