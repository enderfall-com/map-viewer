// Package demo generates a synthetic Minecraft-like world so the tile pipeline,
// API and frontend can be exercised without an actual Minecraft server.
//
// It is coupled to nothing but world.Provider. The renderers, tile manager and
// API cannot tell a demo world from an Anvil world, which is the point: the
// demo exercises the real production path rather than a parallel one.
package demo

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/enderfall/minecraft-map/backend/internal/blocks"
	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

// World is a deterministic procedural world implementing world.Provider.
//
// Generation is a pure function of (dimension, x, z, seed), so every worker
// goroutine can generate any chunk at any time with no shared state and no
// locking, and tiles are byte-identical across restarts.
type World struct {
	seed  int64
	reg   *blocks.Registry
	bio   *blocks.Biomes
	dims  []world.DimensionInfo
	byID  map[string]world.DimensionInfo
	ids   sync.Map // cached block/biome registry ids
	limit int      // generated radius in blocks; beyond it the world is unexplored
}

// Options configures the demo world.
type Options struct {
	Seed int64
	// Radius is how far from the origin terrain exists, in blocks. Outside it
	// chunks report as ungenerated, which exercises the unexplored-terrain path.
	Radius int
}

// DefaultOptions returns a world large enough to pan around convincingly.
func DefaultOptions() Options { return Options{Seed: 0x5EED1234, Radius: 6000} }

// New builds a demo world.
//
// The dimension list deliberately includes non-vanilla identifiers with
// different height ranges, so dimension handling is exercised against exactly
// the kind of modded world the system has to support.
func New(reg *blocks.Registry, bio *blocks.Biomes, opts Options) *World {
	if opts.Radius <= 0 {
		opts.Radius = DefaultOptions().Radius
	}
	if opts.Seed == 0 {
		opts.Seed = DefaultOptions().Seed
	}
	w := &World{seed: opts.Seed, reg: reg, bio: bio, limit: opts.Radius}
	w.dims = []world.DimensionInfo{
		{
			ID: "minecraft:overworld", Name: "Overworld",
			MinY: -64, MaxY: 320, WorldBorder: 12000,
			Enabled: true, SpawnX: 0, SpawnZ: 0,
		},
		{
			ID: "minecraft:the_nether", Name: "Nether",
			MinY: 0, MaxY: 128, WorldBorder: 4000,
			Enabled: true, SpawnX: 0, SpawnZ: 0, HasCeiling: true,
		},
		{
			ID: "minecraft:the_end", Name: "The End",
			MinY: 0, MaxY: 256, WorldBorder: 6000,
			Enabled: true, SpawnX: 100, SpawnZ: 0,
		},
		{
			ID: "enderfall:player_839272", Name: "Player Dimension - Daniel",
			MinY: -64, MaxY: 320, WorldBorder: 2000,
			Enabled: true, SpawnX: 0, SpawnZ: 0,
		},
		{
			// A deliberately unusual height range, to prove nothing assumes
			// the vanilla -64..320.
			ID: "some_mod:mining_dimension", Name: "Mining Dimension",
			MinY: -512, MaxY: 128, WorldBorder: 8000,
			Enabled: true, SpawnX: 0, SpawnZ: 0, HasCeiling: true,
		},
	}
	w.byID = make(map[string]world.DimensionInfo, len(w.dims))
	for _, d := range w.dims {
		w.byID[d.ID] = d
	}
	return w
}

// Dimensions implements world.Provider.
func (w *World) Dimensions(context.Context) ([]world.DimensionInfo, error) {
	out := make([]world.DimensionInfo, len(w.dims))
	copy(out, w.dims)
	return out, nil
}

// Dimension implements world.Provider.
func (w *World) Dimension(_ context.Context, id string) (world.DimensionInfo, bool) {
	d, ok := w.byID[id]
	return d, ok
}

// id memoises registry lookups so the hot generation loop does not take the
// registry's read lock for every single column.
func (w *World) id(kind, name string) uint16 {
	key := kind + "\x00" + name
	if v, ok := w.ids.Load(key); ok {
		return v.(uint16)
	}
	var v uint16
	if kind == "b" {
		v = w.reg.ID(name)
	} else {
		v = w.bio.ID(name)
	}
	w.ids.Store(key, v)
	return v
}

// ChunkSurface implements world.Provider.
func (w *World) ChunkSurface(ctx context.Context, dimension string, pos mcmath.ChunkPos) (*world.ChunkSurface, error) {
	dim, ok := w.byID[dimension]
	if !ok {
		return nil, fmt.Errorf("unknown dimension %q", dimension)
	}
	// Outside the generated radius the world simply does not exist yet.
	cx, cz := pos.MinBlockX(), pos.MinBlockZ()
	if abs(cx) > w.limit || abs(cz) > w.limit {
		return nil, world.ErrChunkAbsent
	}

	cs := &world.ChunkSurface{Pos: pos}
	for dz := 0; dz < mcmath.ChunkSize; dz++ {
		for dx := 0; dx < mcmath.ChunkSize; dx++ {
			x, z := cx+dx, cz+dz
			i := dz*mcmath.ChunkSize + dx
			var col world.Column
			switch {
			case strings.Contains(dimension, "nether"):
				col = w.netherColumn(x, z, dim)
			case strings.Contains(dimension, "the_end"):
				col = w.endColumn(x, z, dim)
			case strings.Contains(dimension, "mining"):
				col = w.miningColumn(x, z, dim)
			default:
				col = w.overworldColumn(x, z, dim)
			}
			cs.Height[i] = int16(col.Height)
			cs.WaterY[i] = int16(col.WaterY)
			cs.Block[i] = col.Block
			cs.Biome[i] = col.Biome
			cs.Light[i] = col.Light
			cs.Flags[i] = col.Flags
		}
	}
	return cs, nil
}

const seaLevel = 63

// overworldColumn generates one surface column of temperate terrain with
// oceans, beaches, forests, mountains, snow caps, trees and a few structures.
func (w *World) overworldColumn(x, z int, dim world.DimensionInfo) world.Column {
	fx, fz := float64(x), float64(z)

	// Large-scale land/sea split, then hills, then ridged mountains that only
	// bite where the continent is already high.
	continent := fbm(w.seed, fx/900, fz/900, 4, 2, 0.5)
	hills := fbm(w.seed+11, fx/220, fz/220, 4, 2, 0.5)
	ridge := ridged(w.seed+29, fx/460, fz/460, 4)

	land := continent*1.35 + 0.06
	h := float64(seaLevel) + land*44 + hills*11

	mountainMask := smoothstep(0.18, 0.62, land)
	h += ridge * 78 * mountainMask

	// Rivers: a narrow band where a separate noise field crosses zero.
	river := fbm(w.seed+53, fx/700, fz/700, 3, 2, 0.5)
	riverness := 1 - smoothstep(0.0, 0.045, math.Abs(river))
	if riverness > 0 && land > 0 {
		h -= riverness * 9
	}

	height := int(math.Round(h))
	if height < dim.MinY+1 {
		height = dim.MinY + 1
	}
	if height > dim.MaxY-1 {
		height = dim.MaxY - 1
	}

	temp := fbm(w.seed+71, fx/1400, fz/1400, 3, 2, 0.5)
	humid := fbm(w.seed+97, fx/1100, fz/1100, 3, 2, 0.5)

	col := world.Column{Height: height, Light: 15, Flags: world.FlagPresent}

	// Water fills anything below sea level.
	if height < seaLevel {
		col.WaterY = seaLevel
		col.Flags |= world.FlagWater
		depth := seaLevel - height
		switch {
		case depth <= 3:
			col.Block = w.id("b", "minecraft:sand")
		case depth <= 12:
			col.Block = w.id("b", "minecraft:gravel")
		default:
			col.Block = w.id("b", "minecraft:dirt")
		}
		switch {
		case depth > 26:
			col.Biome = w.id("bi", "minecraft:deep_ocean")
		case riverness > 0.5:
			col.Biome = w.id("bi", "minecraft:river")
		default:
			col.Biome = w.id("bi", "minecraft:ocean")
		}
		return col
	}

	biome, surface := w.overworldBiome(x, z, height, temp, humid)
	col.Biome = w.id("bi", biome)
	col.Block = w.id("b", surface)

	// Structures overwrite terrain entirely.
	if b, top, ok := w.structureAt(x, z, height); ok {
		col.Block = w.id("b", b)
		col.Height = top
		return col
	}

	// Trees sit on top of the terrain and read as foliage.
	if leaf, top, ok := w.treeAt(x, z, height, biome); ok {
		col.Block = w.id("b", leaf)
		col.Height = top
		col.Flags |= world.FlagFoliage
	}
	return col
}

// overworldBiome picks a biome and its surface block from climate and altitude.
func (w *World) overworldBiome(x, z, height int, temp, humid float64) (biome, surface string) {
	switch {
	case height <= seaLevel+2:
		return "minecraft:beach", "minecraft:sand"
	case height > 150:
		return "minecraft:jagged_peaks", "minecraft:snow_block"
	case height > 128:
		return "minecraft:frozen_peaks", "minecraft:stone"
	case height > 104:
		return "minecraft:windswept_hills", "minecraft:stone"
	}
	switch {
	case temp > 0.30 && humid < -0.15:
		return "minecraft:desert", "minecraft:sand"
	case temp > 0.22 && humid < 0.05:
		return "minecraft:savanna", "minecraft:grass_block"
	case temp < -0.28:
		return "minecraft:snowy_taiga", "minecraft:snow_block"
	case temp < -0.10:
		return "minecraft:taiga", "minecraft:grass_block"
	case humid > 0.30:
		return "minecraft:swamp", "minecraft:grass_block"
	case humid > 0.14:
		return "minecraft:forest", "minecraft:grass_block"
	default:
		return "minecraft:plains", "minecraft:grass_block"
	}
}

// treeAt reports whether a canopy covers this column.
//
// Trees are placed by hashing a coarse cell, then testing every nearby cell's
// trunk against a canopy radius. This gives round, overlapping canopies rather
// than isolated single-column dots, and stays a pure function of position.
func (w *World) treeAt(x, z, ground int, biome string) (block string, top int, ok bool) {
	density, leaf := 0.0, "minecraft:oak_leaves"
	switch biome {
	case "minecraft:forest":
		density, leaf = 0.62, "minecraft:oak_leaves"
	case "minecraft:plains":
		density, leaf = 0.10, "minecraft:oak_leaves"
	case "minecraft:taiga", "minecraft:snowy_taiga":
		density, leaf = 0.55, "minecraft:spruce_leaves"
	case "minecraft:swamp":
		density, leaf = 0.35, "minecraft:oak_leaves"
	case "minecraft:savanna":
		density, leaf = 0.12, "minecraft:acacia_leaves"
	default:
		return "", 0, false
	}

	const cell = 6
	best := -1
	for dz := -1; dz <= 1; dz++ {
		for dx := -1; dx <= 1; dx++ {
			gx := mcmath.FloorDiv(x, cell) + dx
			gz := mcmath.FloorDiv(z, cell) + dz
			hsh := hash2(w.seed+7, gx, gz)
			if float64(hsh%1000)/1000 > density {
				continue
			}
			// Trunk position jittered inside its cell.
			tx := gx*cell + int(hsh>>10%uint64(cell))
			tz := gz*cell + int(hsh>>20%uint64(cell))
			radius := 2 + int(hsh>>30%2)
			ddx, ddz := x-tx, z-tz
			if ddx*ddx+ddz*ddz > radius*radius {
				continue
			}
			height := 4 + int(hsh>>34%4)
			// Canopy domes: taller at the trunk, lower at the rim.
			drop := (ddx*ddx + ddz*ddz) / 2
			if t := ground + height - drop; t > best {
				best = t
			}
		}
	}
	if best < 0 {
		return "", 0, false
	}
	return leaf, best, true
}

// structureAt places a handful of hand-placed builds so the demo has recognisable
// landmarks for testing search, markers and isometric occlusion.
func (w *World) structureAt(x, z, ground int) (block string, top int, ok bool) {
	for _, s := range demoStructures {
		if x < s.minX || x >= s.maxX || z < s.minZ || z >= s.maxZ {
			continue
		}
		lx, lz := x-s.minX, z-s.minZ
		wdt, dep := s.maxX-s.minX, s.maxZ-s.minZ

		switch s.kind {
		case structTower:
			// A square keep with corner turrets, tall enough to test isometric
			// occlusion and tile overlap properly.
			cx, cz := wdt/2, dep/2
			dx, dz := abs(lx-cx), abs(lz-cz)
			r := max(dx, dz)
			switch {
			case r > cx-1:
				return "", 0, false
			case r == cx-1 || r == cx-2:
				return "minecraft:stone_bricks", ground + s.height, true
			case dx < 2 && dz < 2:
				return "minecraft:oak_planks", ground + s.height + 14, true
			default:
				return "minecraft:oak_planks", ground + 2, true
			}

		case structVillage:
			// A grid of small huts with streets between them.
			hx, hz := lx%9, lz%9
			if hx < 7 && hz < 7 {
				if hx == 0 || hz == 0 || hx == 6 || hz == 6 {
					return "minecraft:oak_planks", ground + 5, true
				}
				return "minecraft:oak_planks", ground + 7, true
			}
			return "minecraft:gravel", ground, true

		case structWall:
			// A long rampart, good for checking that thin tall features survive
			// downsampling to low zoom.
			return "minecraft:stone_bricks", ground + s.height, true
		}
	}
	return "", 0, false
}

type structKind int

const (
	structTower structKind = iota
	structVillage
	structWall
)

type structure struct {
	name                   string
	kind                   structKind
	minX, minZ, maxX, maxZ int
	height                 int
}

// demoStructures are also surfaced through the features API as points of
// interest, so search and markers have something real to find.
var demoStructures = []structure{
	{"Spawn Keep", structTower, -24, -24, 24, 24, 26},
	{"Riverside Village", structVillage, 420, -260, 510, -170, 0},
	{"North Rampart", structWall, -900, -640, -300, -628, 9},
	{"Watchtower", structTower, 1180, 840, 1220, 880, 34},
	{"Old Quarry Village", structVillage, -1420, 700, -1340, 780, 0},
}

// netherColumn generates a low, hot dimension with a ceiling and lava seas.
func (w *World) netherColumn(x, z int, dim world.DimensionInfo) world.Column {
	fx, fz := float64(x), float64(z)
	base := fbm(w.seed+301, fx/180, fz/180, 4, 2, 0.5)
	h := 40 + base*26 + ridged(w.seed+307, fx/90, fz/90, 3)*14
	height := clampInt(int(math.Round(h)), dim.MinY+1, dim.MaxY-1)

	col := world.Column{Height: height, Light: 12, Flags: world.FlagPresent}
	const lavaLevel = 34
	switch {
	case height < lavaLevel:
		col.WaterY = lavaLevel
		col.Flags |= world.FlagWater
		col.Block = w.id("b", "minecraft:lava")
		col.Biome = w.id("bi", "minecraft:nether_wastes")
	case base > 0.28:
		col.Block = w.id("b", "minecraft:warped_nylium")
		col.Biome = w.id("bi", "minecraft:warped_forest")
	case base < -0.26:
		col.Block = w.id("b", "minecraft:crimson_nylium")
		col.Biome = w.id("bi", "minecraft:crimson_forest")
	case fbm(w.seed+311, fx/240, fz/240, 2, 2, 0.5) > 0.34:
		col.Block = w.id("b", "minecraft:soul_sand")
		col.Biome = w.id("bi", "minecraft:soul_sand_valley")
	default:
		col.Block = w.id("b", "minecraft:netherrack")
		col.Biome = w.id("bi", "minecraft:nether_wastes")
	}
	return col
}

// endColumn generates floating islands over void, so large parts of the
// dimension are genuinely absent -- a good test of unexplored rendering.
func (w *World) endColumn(x, z int, dim world.DimensionInfo) world.Column {
	fx, fz := float64(x), float64(z)
	dist := math.Hypot(fx, fz)

	island := fbm(w.seed+401, fx/320, fz/320, 4, 2, 0.5)
	// The central island is always solid; outer islands are noise-gated.
	solid := dist < 260 || island > 0.16
	if !solid {
		return world.Column{} // void: no data at all
	}
	h := 68 + island*22 + fbm(w.seed+409, fx/70, fz/70, 3, 2, 0.5)*7
	height := clampInt(int(math.Round(h)), dim.MinY+1, dim.MaxY-1)

	col := world.Column{Height: height, Light: 10, Flags: world.FlagPresent}
	col.Block = w.id("b", "minecraft:end_stone")
	col.Biome = w.id("bi", "minecraft:the_end")
	if dist > 300 && island > 0.42 {
		col.Biome = w.id("bi", "minecraft:end_highlands")
		if fbm(w.seed+419, fx/26, fz/26, 2, 2, 0.5) > 0.42 {
			col.Block = w.id("b", "minecraft:chorus_plant")
			col.Height = height + 4
			col.Flags |= world.FlagFoliage
		}
	}
	return col
}

// miningColumn exercises a dimension with an unusual, very deep height range.
func (w *World) miningColumn(x, z int, dim world.DimensionInfo) world.Column {
	fx, fz := float64(x), float64(z)
	h := -300 + fbm(w.seed+501, fx/140, fz/140, 4, 2, 0.5)*90
	height := clampInt(int(math.Round(h)), dim.MinY+1, dim.MaxY-1)

	col := world.Column{Height: height, Light: 4, Flags: world.FlagPresent}
	col.Biome = w.id("bi", "some_mod:deep_caverns")
	ore := fbm(w.seed+509, fx/40, fz/40, 3, 2, 0.5)
	switch {
	case ore > 0.42:
		col.Block = w.id("b", "minecraft:iron_ore")
	case ore < -0.46:
		col.Block = w.id("b", "minecraft:deepslate")
	default:
		col.Block = w.id("b", "minecraft:stone")
	}
	return col
}

// ---------------------------------------------------------------------------
// Deterministic noise
// ---------------------------------------------------------------------------

// hash2 is a fast integer hash with good avalanche, used as the lattice source.
func hash2(seed int64, x, z int) uint64 {
	h := uint64(seed)*0x9E3779B97F4A7C15 ^ uint64(int64(x))*0xBF58476D1CE4E5B9 ^ uint64(int64(z))*0x94D049BB133111EB
	h ^= h >> 30
	h *= 0xBF58476D1CE4E5B9
	h ^= h >> 27
	h *= 0x94D049BB133111EB
	h ^= h >> 31
	return h
}

// lattice returns a deterministic value in [-1,1] at integer lattice points.
func lattice(seed int64, x, z int) float64 {
	return float64(hash2(seed, x, z)%2000001)/1000000 - 1
}

// valueNoise interpolates the lattice with a smoothstep fade, giving continuous
// gradients without the visible grid artefacts of bilinear interpolation.
func valueNoise(seed int64, x, z float64) float64 {
	x0, z0 := math.Floor(x), math.Floor(z)
	ix, iz := int(x0), int(z0)
	fx, fz := x-x0, z-z0
	u, v := fade(fx), fade(fz)

	n00 := lattice(seed, ix, iz)
	n10 := lattice(seed, ix+1, iz)
	n01 := lattice(seed, ix, iz+1)
	n11 := lattice(seed, ix+1, iz+1)

	return lerp(lerp(n00, n10, u), lerp(n01, n11, u), v)
}

// fbm sums octaves of value noise, normalised so the result stays near [-1,1].
func fbm(seed int64, x, z float64, octaves int, lacunarity, gain float64) float64 {
	sum, amp, norm := 0.0, 1.0, 0.0
	freq := 1.0
	for i := 0; i < octaves; i++ {
		sum += valueNoise(seed+int64(i)*1013, x*freq, z*freq) * amp
		norm += amp
		amp *= gain
		freq *= lacunarity
	}
	if norm == 0 {
		return 0
	}
	return sum / norm
}

// ridged folds the noise around zero to make sharp crests, which is what turns
// rolling hills into recognisable mountain ranges with obvious cliffs.
func ridged(seed int64, x, z float64, octaves int) float64 {
	sum, amp, norm, freq := 0.0, 1.0, 0.0, 1.0
	for i := 0; i < octaves; i++ {
		n := 1 - math.Abs(valueNoise(seed+int64(i)*2027, x*freq, z*freq))
		sum += n * n * amp
		norm += amp
		amp *= 0.5
		freq *= 2
	}
	if norm == 0 {
		return 0
	}
	return sum/norm - 0.5
}

func fade(t float64) float64       { return t * t * t * (t*(t*6-15) + 10) }
func lerp(a, b, t float64) float64 { return a + (b-a)*t }

func smoothstep(edge0, edge1, x float64) float64 {
	if edge1 == edge0 {
		return 0
	}
	t := (x - edge0) / (edge1 - edge0)
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return t * t * (3 - 2*t)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// columnAt generates a single column, used by helpers that need terrain height
// without materialising a whole chunk.
func (w *World) columnAt(dimension string, x, z int, dim world.DimensionInfo) world.Column {
	switch {
	case strings.Contains(dimension, "nether"):
		return w.netherColumn(x, z, dim)
	case strings.Contains(dimension, "the_end"):
		return w.endColumn(x, z, dim)
	case strings.Contains(dimension, "mining"):
		return w.miningColumn(x, z, dim)
	default:
		return w.overworldColumn(x, z, dim)
	}
}
