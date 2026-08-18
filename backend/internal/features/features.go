// Package features holds the dynamic overlay data: players, claims, regions,
// waypoints and other points of interest.
//
// None of this is ever baked into terrain tiles. Terrain is immutable and
// cached for a year; overlays change by the second and are delivered as vector
// data the client draws itself. Keeping the two apart is what allows both.
package features

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
)

// Player is a live player position.
type Player struct {
	UUID      string  `json:"uuid"`
	Name      string  `json:"name"`
	Dimension string  `json:"dimension"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Z         float64 `json:"z"`
	// Rotation is the yaw in degrees; 0 is south, matching Minecraft.
	Rotation float64 `json:"rotation"`
	// Health and Armor are optional and omitted when unknown.
	Health *float64 `json:"health,omitempty"`
	Armor  *float64 `json:"armor,omitempty"`
	// Hidden players are tracked but not published, for vanish support.
	Hidden bool `json:"-"`
	// UpdatedAt lets the client reason about staleness.
	UpdatedAt time.Time `json:"updatedAt"`
}

// Shape is the geometry kind of an area feature.
type Shape string

const (
	// ShapeRect is an axis-aligned rectangle in block coordinates.
	ShapeRect Shape = "rect"
	// ShapePolygon is an arbitrary ring of block coordinates.
	ShapePolygon Shape = "polygon"
	// ShapeChunks is a set of individual chunk coordinates, which is how most
	// claim plugins actually model land.
	ShapeChunks Shape = "chunks"
)

// Point is a block-space X/Z position.
type Point struct {
	X float64 `json:"x"`
	Z float64 `json:"z"`
}

// Area is a claim, region or any other bounded piece of the world.
//
// One type covers claims and server regions because they differ only in
// styling and provenance, not in structure. The geometry model is deliberately
// wider than rectangles from the start: chunk-based claims and arbitrary
// polygons are both first-class, so supporting a plugin that grew beyond
// rectangles is a data change rather than a schema migration.
type Area struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"` // "claim", "region", "protected", ...
	Name      string `json:"name,omitempty"`
	Owner     string `json:"owner,omitempty"`
	Dimension string `json:"dimension"`

	Shape Shape `json:"shape"`
	// MinX/MinZ/MaxX/MaxZ define ShapeRect and always carry the bounding box of
	// the other shapes, so spatial queries need not special-case geometry.
	MinX int `json:"minX"`
	MinZ int `json:"minZ"`
	MaxX int `json:"maxX"`
	MaxZ int `json:"maxZ"`

	// Polygon carries ShapePolygon rings.
	Polygon []Point `json:"polygon,omitempty"`
	// Chunks carries ShapeChunks members.
	Chunks []ChunkRef `json:"chunks,omitempty"`

	// Style overrides the client's default appearance for this kind.
	Fill        string  `json:"fill,omitempty"`
	Stroke      string  `json:"stroke,omitempty"`
	FillOpacity float64 `json:"fillOpacity,omitempty"`
	Label       string  `json:"label,omitempty"`
	// MinZoom and MaxZoom bound the zooms at which this feature is drawn, which
	// is how progressive detail is expressed per feature rather than per layer.
	MinZoom *int `json:"minZoom,omitempty"`
	MaxZoom *int `json:"maxZoom,omitempty"`

	Properties map[string]any `json:"properties,omitempty"`
}

// ChunkRef is a chunk coordinate pair.
type ChunkRef struct {
	X int `json:"x"`
	Z int `json:"z"`
}

// Marker is a point of interest: spawn, a warp, a home, a waypoint.
type Marker struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"` // "spawn", "warp", "home", "waypoint", "poi"
	Name      string `json:"name"`
	Dimension string `json:"dimension"`
	X         int    `json:"x"`
	Y         *int   `json:"y,omitempty"`
	Z         int    `json:"z"`
	Icon      string `json:"icon,omitempty"`
	Color     string `json:"color,omitempty"`
	MinZoom   *int   `json:"minZoom,omitempty"`
	MaxZoom   *int   `json:"maxZoom,omitempty"`

	Properties map[string]any `json:"properties,omitempty"`
}

// Bounds returns the query rectangle for a marker.
func (m Marker) Bounds() mcmath.BlockBounds {
	return mcmath.BlockBounds{MinX: m.X, MinZ: m.Z, MaxX: m.X + 1, MaxZ: m.Z + 1}
}

// Bounds returns the area's bounding rectangle.
func (a Area) Bounds() mcmath.BlockBounds {
	return mcmath.BlockBounds{MinX: a.MinX, MinZ: a.MinZ, MaxX: a.MaxX, MaxZ: a.MaxZ}
}

// Normalize fills in the bounding box from the detailed geometry and repairs
// inverted rectangles, so downstream spatial queries can trust the box.
func (a *Area) Normalize() {
	switch a.Shape {
	case ShapePolygon:
		if len(a.Polygon) > 0 {
			minX, minZ := math.Inf(1), math.Inf(1)
			maxX, maxZ := math.Inf(-1), math.Inf(-1)
			for _, p := range a.Polygon {
				minX, maxX = math.Min(minX, p.X), math.Max(maxX, p.X)
				minZ, maxZ = math.Min(minZ, p.Z), math.Max(maxZ, p.Z)
			}
			a.MinX, a.MinZ = int(math.Floor(minX)), int(math.Floor(minZ))
			a.MaxX, a.MaxZ = int(math.Ceil(maxX)), int(math.Ceil(maxZ))
		}
	case ShapeChunks:
		if len(a.Chunks) > 0 {
			minCX, minCZ := math.MaxInt32, math.MaxInt32
			maxCX, maxCZ := math.MinInt32, math.MinInt32
			for _, c := range a.Chunks {
				minCX, maxCX = min(minCX, c.X), max(maxCX, c.X)
				minCZ, maxCZ = min(minCZ, c.Z), max(maxCZ, c.Z)
			}
			a.MinX, a.MinZ = mcmath.ChunkToBlock(minCX), mcmath.ChunkToBlock(minCZ)
			a.MaxX = mcmath.ChunkToBlock(maxCX) + mcmath.ChunkSize
			a.MaxZ = mcmath.ChunkToBlock(maxCZ) + mcmath.ChunkSize
		}
	}
	if a.MaxX < a.MinX {
		a.MinX, a.MaxX = a.MaxX, a.MinX
	}
	if a.MaxZ < a.MinZ {
		a.MinZ, a.MaxZ = a.MaxZ, a.MinZ
	}
	if a.Shape == "" {
		a.Shape = ShapeRect
	}
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// Query bounds a spatial feature request.
type Query struct {
	Dimension string
	Bounds    mcmath.BlockBounds
	// Zoom filters features by their own visibility range. Negative disables it.
	Zoom int
	// Limit caps the response size so a client asking for the whole world
	// cannot pull a million features into a browser.
	Limit int
	Kinds []string
}

// Set is the response to a spatial query.
type Set struct {
	Areas   []Area   `json:"areas"`
	Markers []Marker `json:"markers"`
	Players []Player `json:"players"`
	// Truncated reports that Limit clipped the result, so the client can tell
	// the difference between "nothing here" and "too much here".
	Truncated bool `json:"truncated"`
}

// Source supplies overlay features.
//
// The interface is what allows the in-memory implementation below to be
// replaced by a PostgreSQL or Redis-backed one without touching the API layer:
// the query is already expressed as a bounded spatial request rather than as
// "give me everything".
type Source interface {
	Query(ctx context.Context, q Query) (Set, error)
	Players(ctx context.Context, dimension string) ([]Player, error)
	Areas(ctx context.Context, dimension, kind string) ([]Area, error)
	Markers(ctx context.Context, dimension, kind string) ([]Marker, error)
}

// Memory is an in-memory feature source with a uniform grid spatial index.
//
// The grid keeps viewport queries proportional to what is on screen rather than
// to how many features exist in total, which is what stops a server with tens
// of thousands of claims from slowing down as you pan.
type Memory struct {
	mu sync.RWMutex

	areas   map[string]Area
	markers map[string]Marker
	players map[string]Player

	// grid indexes areas and markers by cell for bounded spatial lookup.
	areaGrid   map[gridKey][]string
	markerGrid map[gridKey][]string
}

// gridCell is the index cell size in blocks. 512 matches a region file, which
// is a natural scale for claims and keeps the index small.
const gridCell = 512

type gridKey struct {
	dim  string
	x, z int
}

// NewMemory creates an empty in-memory feature source.
func NewMemory() *Memory {
	return &Memory{
		areas:      make(map[string]Area),
		markers:    make(map[string]Marker),
		players:    make(map[string]Player),
		areaGrid:   make(map[gridKey][]string),
		markerGrid: make(map[gridKey][]string),
	}
}

// cellsFor returns the grid cells a rectangle touches.
func cellsFor(dim string, b mcmath.BlockBounds) []gridKey {
	minX := mcmath.FloorDiv(b.MinX, gridCell)
	maxX := mcmath.FloorDiv(max(b.MinX, b.MaxX-1), gridCell)
	minZ := mcmath.FloorDiv(b.MinZ, gridCell)
	maxZ := mcmath.FloorDiv(max(b.MinZ, b.MaxZ-1), gridCell)

	// A feature spanning an enormous area would otherwise be inserted into
	// millions of cells; past a threshold it goes in a single overflow cell that
	// every query checks.
	if (maxX-minX+1)*(maxZ-minZ+1) > 4096 {
		return []gridKey{{dim, math.MinInt32, math.MinInt32}}
	}
	out := make([]gridKey, 0, (maxX-minX+1)*(maxZ-minZ+1))
	for z := minZ; z <= maxZ; z++ {
		for x := minX; x <= maxX; x++ {
			out = append(out, gridKey{dim, x, z})
		}
	}
	return out
}

// PutArea inserts or replaces an area.
func (m *Memory) PutArea(a Area) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putAreaLocked(a)
}

// putAreaLocked is PutArea's body, for callers that already hold m.mu for
// writing -- claiming and force-loading need to check-then-insert atomically,
// which calling the public, self-locking PutArea from inside another locked
// section cannot do.
func (m *Memory) putAreaLocked(a Area) {
	a.Normalize()
	if old, ok := m.areas[a.ID]; ok {
		m.removeFromGrid(m.areaGrid, old.Dimension, old.Bounds(), old.ID)
	}
	m.areas[a.ID] = a
	for _, k := range cellsFor(a.Dimension, a.Bounds()) {
		m.areaGrid[k] = append(m.areaGrid[k], a.ID)
	}
}

// PutMarker inserts or replaces a marker.
func (m *Memory) PutMarker(mk Marker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.markers[mk.ID]; ok {
		m.removeFromGrid(m.markerGrid, old.Dimension, old.Bounds(), old.ID)
	}
	m.markers[mk.ID] = mk
	for _, k := range cellsFor(mk.Dimension, mk.Bounds()) {
		m.markerGrid[k] = append(m.markerGrid[k], mk.ID)
	}
}

// PutPlayer inserts or replaces a player position.
func (m *Memory) PutPlayer(p Player) {
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = time.Now()
	}
	m.mu.Lock()
	m.players[p.UUID] = p
	m.mu.Unlock()
}

// SyncPlayers atomically replaces the entire tracked player set with exactly
// the given list.
//
// This is the contract for a plugin pushing its full online-player snapshot
// on every tick: whoever is not in the new list is treated as having left,
// with no separate "player left" message to miss, arrive out of order, or
// leave a stale entry behind forever if it is dropped.
func (m *Memory) SyncPlayers(players []Player) {
	next := make(map[string]Player, len(players))
	now := time.Now()
	for _, p := range players {
		if p.UpdatedAt.IsZero() {
			p.UpdatedAt = now
		}
		next[p.UUID] = p
	}
	m.mu.Lock()
	m.players = next
	m.mu.Unlock()
}

// RemovePlayer drops a player, e.g. on disconnect.
func (m *Memory) RemovePlayer(uuid string) {
	m.mu.Lock()
	delete(m.players, uuid)
	m.mu.Unlock()
}

// RemoveArea drops an area.
func (m *Memory) RemoveArea(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeAreaLocked(id)
}

// removeAreaLocked is RemoveArea's body, for callers already holding m.mu.
func (m *Memory) removeAreaLocked(id string) {
	if old, ok := m.areas[id]; ok {
		m.removeFromGrid(m.areaGrid, old.Dimension, old.Bounds(), id)
		delete(m.areas, id)
	}
}

// RemoveMarker drops a marker.
func (m *Memory) RemoveMarker(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.markers[id]; ok {
		m.removeFromGrid(m.markerGrid, old.Dimension, old.Bounds(), id)
		delete(m.markers, id)
	}
}

// AreaDimension returns the dimension an area currently belongs to. Callers
// that need to announce a removal (which the caller must still make, via
// RemoveArea) use this beforehand to learn where to broadcast it, since the
// dimension is otherwise lost the moment the area is gone.
func (m *Memory) AreaDimension(id string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.areas[id]
	return a.Dimension, ok
}

// MarkerDimension returns the dimension a marker currently belongs to, for
// the same reason as AreaDimension.
func (m *Memory) MarkerDimension(id string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mk, ok := m.markers[id]
	return mk.Dimension, ok
}

// ---------------------------------------------------------------------------
// Chunk selection: claim, unclaim, force-load
// ---------------------------------------------------------------------------

// ErrChunksClaimed reports that a claim request touches chunks some other
// claim already covers. Claims are not allowed to overlap.
var ErrChunksClaimed = errors.New("some chunks are already claimed")

// chunkInArea reports whether a chunk falls inside an area's geometry.
//
// For a chunk-shaped area this is exact set membership. For any other shape
// (rect, polygon) it falls back to a bounding-box test, which is exact for a
// rect and an over-approximation for a polygon -- accepted here because the
// only areas this package itself ever creates at chunk granularity are
// chunk-shaped, and the approximation only affects claim/unclaim's overlap
// checks against pre-existing rect or polygon claims from other sources.
func chunkInArea(a Area, c ChunkRef) bool {
	switch a.Shape {
	case ShapeChunks:
		for _, ac := range a.Chunks {
			if ac == c {
				return true
			}
		}
		return false
	default:
		x0, z0 := mcmath.ChunkToBlock(c.X), mcmath.ChunkToBlock(c.Z)
		x1, z1 := x0+mcmath.ChunkSize, z0+mcmath.ChunkSize
		b := a.Bounds()
		return x0 < b.MaxX && x1 > b.MinX && z0 < b.MaxZ && z1 > b.MinZ
	}
}

// ClaimChunks creates a new chunk-shaped claim covering exactly the given
// chunks, provided none of them already belong to an existing claim in the
// dimension. The whole request is rejected -- rather than claiming whatever
// chunks are free -- so a caller never ends up with a smaller claim than it
// asked for without knowing it; conflicts are returned so the caller can
// retry with a clean selection.
func (m *Memory) ClaimChunks(dimension string, chunks []ChunkRef, name, owner string) (Area, []ChunkRef, error) {
	if len(chunks) == 0 {
		return Area{}, nil, fmt.Errorf("no chunks selected")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var conflicts []ChunkRef
	for _, c := range chunks {
		for _, a := range m.areas {
			if a.Dimension == dimension && a.Kind == "claim" && chunkInArea(a, c) {
				conflicts = append(conflicts, c)
				break
			}
		}
	}
	if len(conflicts) > 0 {
		return Area{}, conflicts, ErrChunksClaimed
	}

	a := Area{
		ID:        newID("claim"),
		Kind:      "claim",
		Name:      name,
		Owner:     owner,
		Dimension: dimension,
		Shape:     ShapeChunks,
		Chunks:    append([]ChunkRef(nil), chunks...),
	}
	// putAreaLocked takes its argument by value and normalizes that copy, so
	// the caller's own `a` needs it too, or the response handed straight back
	// to whoever just claimed would report a zero bounding box.
	a.Normalize()
	m.putAreaLocked(a)
	return a, nil, nil
}

// UnclaimChunks removes the given chunks from every chunk-shaped claim that
// contains them in a dimension, deleting a claim outright once nothing is
// left of it. Claims recorded as a rect or polygon predate chunk-level
// editing and are left untouched -- carving a sub-region out of arbitrary
// geometry is not attempted.
func (m *Memory) UnclaimChunks(dimension string, chunks []ChunkRef) {
	if len(chunks) == 0 {
		return
	}
	remove := make(map[ChunkRef]bool, len(chunks))
	for _, c := range chunks {
		remove[c] = true
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for id, a := range m.areas {
		if a.Dimension != dimension || a.Kind != "claim" || a.Shape != ShapeChunks {
			continue
		}
		var kept []ChunkRef
		changed := false
		for _, c := range a.Chunks {
			if remove[c] {
				changed = true
				continue
			}
			kept = append(kept, c)
		}
		if !changed {
			continue
		}
		if len(kept) == 0 {
			m.removeAreaLocked(id)
			continue
		}
		a.Chunks = kept
		m.putAreaLocked(a)
	}
}

// forceLoadAreaID is the single, deterministic force-load area per
// dimension. A fixed id (rather than one per toggle) is what makes repeated
// force-load/unload calls converge on updating the same area instead of
// piling up overlapping ones.
func forceLoadAreaID(dimension string) string {
	return "forceload:" + dimension
}

// SetForceLoaded adds or removes chunks from a dimension's single force-load
// marker area, creating it on first use and deleting it once empty.
//
// Unlike a claim, a force-loaded chunk has no owner and cannot conflict with
// another force-loaded chunk, so this is a plain set union or difference
// rather than a conflict check.
func (m *Memory) SetForceLoaded(dimension string, chunks []ChunkRef, loaded bool) {
	if len(chunks) == 0 {
		return
	}
	id := forceLoadAreaID(dimension)

	m.mu.Lock()
	defer m.mu.Unlock()

	set := make(map[ChunkRef]bool)
	if existing, ok := m.areas[id]; ok {
		for _, c := range existing.Chunks {
			set[c] = true
		}
	}
	for _, c := range chunks {
		if loaded {
			set[c] = true
		} else {
			delete(set, c)
		}
	}
	if len(set) == 0 {
		m.removeAreaLocked(id)
		return
	}
	merged := make([]ChunkRef, 0, len(set))
	for c := range set {
		merged = append(merged, c)
	}
	m.putAreaLocked(Area{
		ID: id, Kind: "forceload", Name: "Force Loaded", Dimension: dimension,
		Shape: ShapeChunks, Chunks: merged,
	})
}

// newID generates a short, collision-resistant identifier for an area this
// package creates itself, rather than one supplied by an ingest caller.
func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}

func (m *Memory) removeFromGrid(grid map[gridKey][]string, dim string, b mcmath.BlockBounds, id string) {
	for _, k := range cellsFor(dim, b) {
		ids := grid[k]
		for i, v := range ids {
			if v == id {
				grid[k] = append(ids[:i], ids[i+1:]...)
				break
			}
		}
		if len(grid[k]) == 0 {
			delete(grid, k)
		}
	}
}

// visibleAt reports whether a feature's own zoom range includes zoom.
func visibleAt(zoom int, minZ, maxZ *int) bool {
	if zoom < 0 {
		return true
	}
	if minZ != nil && zoom < *minZ {
		return false
	}
	if maxZ != nil && zoom > *maxZ {
		return false
	}
	return true
}

func kindMatches(kinds []string, kind string) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, k := range kinds {
		if strings.EqualFold(k, kind) {
			return true
		}
	}
	return false
}

// Query implements Source.
func (m *Memory) Query(_ context.Context, q Query) (Set, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 5000
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := Set{Areas: []Area{}, Markers: []Marker{}, Players: []Player{}}
	seen := make(map[string]struct{})

	cells := cellsFor(q.Dimension, q.Bounds)
	cells = append(cells, gridKey{q.Dimension, math.MinInt32, math.MinInt32})

	for _, k := range cells {
		for _, id := range m.areaGrid[k] {
			if _, dup := seen[id]; dup {
				continue
			}
			a, ok := m.areas[id]
			if !ok || !a.Bounds().Intersects(q.Bounds) {
				continue
			}
			if !visibleAt(q.Zoom, a.MinZoom, a.MaxZoom) || !kindMatches(q.Kinds, a.Kind) {
				continue
			}
			seen[id] = struct{}{}
			if len(out.Areas) >= limit {
				out.Truncated = true
				break
			}
			out.Areas = append(out.Areas, a)
		}
	}

	seenM := make(map[string]struct{})
	for _, k := range cells {
		for _, id := range m.markerGrid[k] {
			if _, dup := seenM[id]; dup {
				continue
			}
			mk, ok := m.markers[id]
			if !ok || !mk.Bounds().Intersects(q.Bounds) {
				continue
			}
			if !visibleAt(q.Zoom, mk.MinZoom, mk.MaxZoom) || !kindMatches(q.Kinds, mk.Kind) {
				continue
			}
			seenM[id] = struct{}{}
			if len(out.Markers) >= limit {
				out.Truncated = true
				break
			}
			out.Markers = append(out.Markers, mk)
		}
	}

	// Players are few enough that a linear scan is cheaper than an index that
	// would need updating on every movement tick.
	for _, p := range m.players {
		if p.Hidden || p.Dimension != q.Dimension {
			continue
		}
		if !q.Bounds.Contains(int(p.X), int(p.Z)) {
			continue
		}
		out.Players = append(out.Players, p)
	}

	sortSet(&out)
	return out, nil
}

// Players implements Source.
func (m *Memory) Players(_ context.Context, dimension string) ([]Player, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Player, 0, len(m.players))
	for _, p := range m.players {
		if p.Hidden {
			continue
		}
		if dimension != "" && p.Dimension != dimension {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Areas implements Source.
func (m *Memory) Areas(_ context.Context, dimension, kind string) ([]Area, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Area, 0, len(m.areas))
	for _, a := range m.areas {
		if dimension != "" && a.Dimension != dimension {
			continue
		}
		if kind != "" && !strings.EqualFold(a.Kind, kind) {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Markers implements Source.
func (m *Memory) Markers(_ context.Context, dimension, kind string) ([]Marker, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Marker, 0, len(m.markers))
	for _, mk := range m.markers {
		if dimension != "" && mk.Dimension != dimension {
			continue
		}
		if kind != "" && !strings.EqualFold(mk.Kind, kind) {
			continue
		}
		out = append(out, mk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func sortSet(s *Set) {
	sort.Slice(s.Areas, func(i, j int) bool { return s.Areas[i].ID < s.Areas[j].ID })
	sort.Slice(s.Markers, func(i, j int) bool { return s.Markers[i].ID < s.Markers[j].ID })
	sort.Slice(s.Players, func(i, j int) bool { return s.Players[i].Name < s.Players[j].Name })
}

// Search finds features whose name, owner or id matches a query string.
func (m *Memory) Search(dimension, term string, limit int) []SearchResult {
	term = strings.ToLower(strings.TrimSpace(term))
	if term == "" {
		return nil
	}
	if limit <= 0 {
		limit = 20
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out []SearchResult
	consider := func(r SearchResult, haystacks ...string) {
		for _, h := range haystacks {
			h = strings.ToLower(h)
			if h == "" {
				continue
			}
			if idx := strings.Index(h, term); idx >= 0 {
				// Prefix matches rank above substring matches.
				r.score = idx
				if h == term {
					r.score = -1
				}
				out = append(out, r)
				return
			}
		}
	}

	for _, p := range m.players {
		if p.Hidden {
			continue
		}
		consider(SearchResult{
			Type: "player", ID: p.UUID, Name: p.Name, Dimension: p.Dimension,
			X: int(p.X), Z: int(p.Z),
		}, p.Name)
	}
	for _, mk := range m.markers {
		if dimension != "" && mk.Dimension != dimension {
			continue
		}
		consider(SearchResult{
			Type: mk.Kind, ID: mk.ID, Name: mk.Name, Dimension: mk.Dimension,
			X: mk.X, Z: mk.Z,
		}, mk.Name, mk.ID)
	}
	for _, a := range m.areas {
		if dimension != "" && a.Dimension != dimension {
			continue
		}
		name := a.Name
		if name == "" {
			name = a.Owner
		}
		consider(SearchResult{
			Type: a.Kind, ID: a.ID, Name: name, Dimension: a.Dimension,
			X: (a.MinX + a.MaxX) / 2, Z: (a.MinZ + a.MaxZ) / 2,
		}, a.Name, a.Owner, a.ID)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score < out[j].score
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// SearchResult is one search hit.
type SearchResult struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Dimension string `json:"dimension"`
	X         int    `json:"x"`
	Z         int    `json:"z"`
	score     int
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// Document is the on-disk shape of a features file.
type Document struct {
	Areas   []Area   `json:"areas"`
	Markers []Marker `json:"markers"`
	Players []Player `json:"players"`
}

// LoadFile merges a features JSON document into the store.
func (m *Memory) LoadFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read features: %w", err)
	}
	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, fmt.Errorf("parse features: %w", err)
	}
	n := 0
	for _, a := range doc.Areas {
		m.PutArea(a)
		n++
	}
	for _, mk := range doc.Markers {
		m.PutMarker(mk)
		n++
	}
	for _, p := range doc.Players {
		m.PutPlayer(p)
		n++
	}
	return n, nil
}

// Counts reports how many features are held, for the status endpoint.
func (m *Memory) Counts() (areas, markers, players int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.areas), len(m.markers), len(m.players)
}
