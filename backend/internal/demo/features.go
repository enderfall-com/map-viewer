package demo

import (
	"context"
	"math"
	"math/rand"
	"time"

	"github.com/enderfall/minecraft-map/backend/internal/features"
)

// SeedFeatures populates a feature store with sample claims, regions, markers
// and points of interest matching the demo terrain, so overlays, search and the
// layer selector all have real data to work with.
func SeedFeatures(mem *features.Memory) {
	const ow = "minecraft:overworld"
	zoom := func(v int) *int { return &v }

	// Server regions: large, low-detail, visible when zoomed out.
	for _, r := range []features.Area{
		{
			ID: "region-spawn", Kind: "region", Name: "Spawn", Dimension: ow,
			Shape: features.ShapeRect, MinX: -256, MinZ: -256, MaxX: 256, MaxZ: 256,
			Fill: "#4c8dff", Stroke: "#8ab4ff", FillOpacity: 0.10, Label: "Spawn",
		},
		{
			ID: "region-market", Kind: "region", Name: "Market", Dimension: ow,
			Shape: features.ShapeRect, MinX: 360, MinZ: -320, MaxX: 600, MaxZ: -120,
			Fill: "#f0b429", Stroke: "#ffd166", FillOpacity: 0.12, Label: "Market",
		},
		{
			ID: "region-pvp", Kind: "region", Name: "PvP Zone", Dimension: ow,
			Shape: features.ShapeRect, MinX: -1500, MinZ: 600, MaxX: -900, MaxZ: 1200,
			Fill: "#e5484d", Stroke: "#ff6369", FillOpacity: 0.12, Label: "PvP Zone",
		},
		{
			// A non-rectangular region, proving the polygon path works end to end.
			ID: "region-event", Kind: "region", Name: "Event Area", Dimension: ow,
			Shape: features.ShapePolygon,
			Polygon: []features.Point{
				{X: 1100, Z: 700}, {X: 1450, Z: 640}, {X: 1600, Z: 900},
				{X: 1380, Z: 1120}, {X: 1080, Z: 990},
			},
			Fill: "#a06cf0", Stroke: "#c4a1ff", FillOpacity: 0.14, Label: "Event Area",
		},
		{
			ID: "region-protected-north", Kind: "region", Name: "Protected Area", Dimension: ow,
			Shape: features.ShapeRect, MinX: -1000, MinZ: -760, MaxX: -250, MaxZ: -560,
			Fill: "#2bb673", Stroke: "#4fd99b", FillOpacity: 0.10, Label: "Protected Area",
		},
	} {
		mem.PutArea(r)
	}

	// Claims: smaller, denser, only worth drawing closer in.
	owners := []string{"Daniel", "Aria", "Kestrel", "Mono", "Vex", "Juno", "Pike", "Rowan"}
	rng := rand.New(rand.NewSource(99))
	for i := 0; i < 140; i++ {
		owner := owners[i%len(owners)]
		cx := rng.Intn(5000) - 2500
		cz := rng.Intn(5000) - 2500
		// Claims are chunk-aligned, as real claim plugins make them.
		cx -= cx % 16
		cz -= cz % 16
		w := (1 + rng.Intn(6)) * 16
		h := (1 + rng.Intn(6)) * 16
		mem.PutArea(features.Area{
			ID:        "claim-" + itoa(i),
			Kind:      "claim",
			Owner:     owner,
			Name:      owner + "'s Claim",
			Dimension: ow,
			Shape:     features.ShapeRect,
			MinX:      cx, MinZ: cz, MaxX: cx + w, MaxZ: cz + h,
			MinZoom: zoom(3),
		})
	}

	// A chunk-set claim, so the third geometry kind is exercised too.
	var chunks []features.ChunkRef
	for dx := 0; dx < 5; dx++ {
		for dz := 0; dz < 4; dz++ {
			// A deliberately ragged, non-rectangular footprint.
			if dx == 4 && dz > 1 {
				continue
			}
			chunks = append(chunks, features.ChunkRef{X: -60 + dx, Z: 44 + dz})
		}
	}
	mem.PutArea(features.Area{
		ID: "claim-town-hall", Kind: "claim", Owner: "Rowan",
		Name: "Rowan's Town", Dimension: ow,
		Shape: features.ShapeChunks, Chunks: chunks,
		Fill: "#3ddc97", MinZoom: zoom(3),
	})

	// Markers, including the structures the terrain generator actually builds.
	markers := []features.Marker{
		{ID: "spawn", Kind: "spawn", Name: "Spawn", Dimension: ow, X: 0, Z: 0, Icon: "spawn"},
		{ID: "warp-market", Kind: "warp", Name: "Market", Dimension: ow, X: 465, Z: -215},
		{ID: "warp-quarry", Kind: "warp", Name: "Old Quarry", Dimension: ow, X: -1380, Z: 740},
		{ID: "home-daniel", Kind: "home", Name: "Daniel's Home", Dimension: ow, X: 1200, Z: 860, MinZoom: zoom(4)},
		{ID: "waypoint-peak", Kind: "waypoint", Name: "North Peak", Dimension: ow, X: -620, Z: -1480, MinZoom: zoom(3)},
		{ID: "waypoint-delta", Kind: "waypoint", Name: "River Delta", Dimension: ow, X: 820, Z: 1450, MinZoom: zoom(3)},
		{ID: "poi-nether-hub", Kind: "poi", Name: "Nether Hub", Dimension: "minecraft:the_nether", X: 0, Z: 0},
		{ID: "poi-end-island", Kind: "poi", Name: "Main Island", Dimension: "minecraft:the_end", X: 100, Z: 0},
	}
	for _, s := range demoStructures {
		markers = append(markers, features.Marker{
			ID:        "struct-" + s.name,
			Kind:      "poi",
			Name:      s.name,
			Dimension: ow,
			X:         (s.minX + s.maxX) / 2,
			Z:         (s.minZ + s.maxZ) / 2,
			MinZoom:   zoom(3),
		})
	}
	for _, m := range markers {
		mem.PutMarker(m)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// SimulatedPlayers moves a handful of fake players around the demo world so the
// player layer, its interpolation and the realtime channel can be exercised
// without a Minecraft server attached.
type SimulatedPlayers struct {
	world   *World
	players []features.Player
	phase   []float64
	speed   []float64
	radius  []float64
	center  [][2]float64
}

// NewSimulatedPlayers creates the sample player set.
func NewSimulatedPlayers(w *World) *SimulatedPlayers {
	names := []string{"Daniel", "Aria", "Kestrel", "Mono", "Vex"}
	dims := []string{
		"minecraft:overworld", "minecraft:overworld", "minecraft:overworld",
		"minecraft:the_nether", "minecraft:the_end",
	}
	s := &SimulatedPlayers{world: w}
	rng := rand.New(rand.NewSource(7))
	for i, n := range names {
		s.players = append(s.players, features.Player{
			UUID:      "demo-" + itoa(i),
			Name:      n,
			Dimension: dims[i],
		})
		s.phase = append(s.phase, rng.Float64()*math.Pi*2)
		s.speed = append(s.speed, 0.05+rng.Float64()*0.12)
		s.radius = append(s.radius, 220+rng.Float64()*900)
		s.center = append(s.center, [2]float64{
			rng.Float64()*2400 - 1200, rng.Float64()*2400 - 1200,
		})
	}
	return s
}

// Tick advances the simulation and writes the new positions into the store.
//
// Positions move along smooth circular paths, which is exactly the case that
// makes marker interpolation visible: a client rendering raw updates would show
// players stepping, while an interpolating one shows them gliding.
func (s *SimulatedPlayers) Tick(ctx context.Context, mem *features.Memory, now time.Time) {
	t := float64(now.UnixNano()) / 1e9
	for i := range s.players {
		p := &s.players[i]
		a := s.phase[i] + t*s.speed[i]
		p.X = s.center[i][0] + math.Cos(a)*s.radius[i]
		p.Z = s.center[i][1] + math.Sin(a)*s.radius[i]

		// Sample the real terrain so players sit on the surface rather than
		// floating at a fixed height.
		p.Y = 64
		if dim, ok := s.world.Dimension(ctx, p.Dimension); ok {
			var col = s.world.columnAt(p.Dimension, int(p.X), int(p.Z), dim)
			p.Y = float64(col.RenderY())
		}
		// Face the direction of travel, in Minecraft's yaw convention.
		p.Rotation = math.Mod(math.Atan2(
			math.Cos(a), -math.Sin(a))*180/math.Pi+360, 360)
		p.UpdatedAt = now
		mem.PutPlayer(*p)
	}
}

// Players returns the current simulated player list.
func (s *SimulatedPlayers) Players() []features.Player {
	out := make([]features.Player, len(s.players))
	copy(out, s.players)
	return out
}
