package blocks

import (
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"sort"
	"strings"
	"sync"
)

// Biome holds the tint colours and overlay colour for one biome. Modded biomes
// that have no entry get deterministic fallbacks, exactly like modded blocks.
type Biome struct {
	ID      uint16
	Name    string
	Grass   color.NRGBA
	Foliage color.NRGBA
	Water   color.NRGBA
	// Overlay is the flat colour used by the "biome" render style and the
	// biome overlay layer.
	Overlay color.NRGBA
	Known   bool
}

// PlainsBiomeID is guaranteed to be index 0 and acts as the neutral default.
const PlainsBiomeID uint16 = 0

// Biomes is a concurrent, append-only biome table with the same
// never-fail-on-unknown contract as Registry.
type Biomes struct {
	mu      sync.RWMutex
	byName  map[string]uint16
	list    []Biome
	unknown map[string]int
}

// NewBiomes returns a biome table seeded with a neutral default.
func NewBiomes() *Biomes {
	b := &Biomes{
		byName:  make(map[string]uint16, 256),
		unknown: make(map[string]int),
	}
	b.list = append(b.list, Biome{
		ID:      PlainsBiomeID,
		Name:    "minecraft:plains",
		Grass:   color.NRGBA{0x91, 0xbd, 0x59, 255},
		Foliage: color.NRGBA{0x77, 0xab, 0x2f, 255},
		Water:   color.NRGBA{0x3f, 0x76, 0xe4, 255},
		Overlay: color.NRGBA{0x8d, 0xb3, 0x60, 255},
		Known:   true,
	})
	b.byName["minecraft:plains"] = PlainsBiomeID
	return b
}

type biomeJSON struct {
	Grass   string `json:"grass"`
	Foliage string `json:"foliage"`
	Water   string `json:"water"`
	Overlay string `json:"overlay"`
}

// LoadBiomesFile merges a biomes.json mapping into the table.
func (b *Biomes) LoadBiomesFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read biome colours: %w", err)
	}
	return b.LoadBiomesJSON(raw)
}

// LoadBiomesJSON merges a biomes.json document from memory.
func (b *Biomes) LoadBiomesJSON(raw []byte) (int, error) {
	var wrapper struct {
		Biomes map[string]biomeJSON `json:"biomes"`
	}
	entries := map[string]biomeJSON{}
	if err := json.Unmarshal(raw, &wrapper); err == nil && len(wrapper.Biomes) > 0 {
		entries = wrapper.Biomes
	} else if err := json.Unmarshal(raw, &entries); err != nil {
		return 0, fmt.Errorf("parse biome colours: %w", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	n := 0
	for name, e := range entries {
		key := normalize(name)
		def := b.fallbackLocked(key)
		bi := Biome{Name: key, Known: true, Grass: def.Grass, Foliage: def.Foliage, Water: def.Water, Overlay: def.Overlay}
		for _, f := range []struct {
			raw string
			dst *color.NRGBA
		}{
			{e.Grass, &bi.Grass}, {e.Foliage, &bi.Foliage},
			{e.Water, &bi.Water}, {e.Overlay, &bi.Overlay},
		} {
			if f.raw == "" {
				continue
			}
			c, err := ParseHexColor(f.raw)
			if err != nil {
				return n, fmt.Errorf("biome %q: %w", name, err)
			}
			*f.dst = c
		}
		// An entry that sets only grass still gets a sensible overlay.
		if e.Overlay == "" && e.Grass != "" {
			bi.Overlay = bi.Grass
		}
		b.putLocked(bi)
		n++
	}
	return n, nil
}

func (b *Biomes) putLocked(bi Biome) uint16 {
	if id, ok := b.byName[bi.Name]; ok {
		bi.ID = id
		b.list[id] = bi
		return id
	}
	bi.ID = uint16(len(b.list))
	b.list = append(b.list, bi)
	b.byName[bi.Name] = bi.ID
	return bi.ID
}

// fallbackLocked derives plausible tints for an unmapped biome from its name,
// so modded biomes look reasonable before anyone writes a mapping. Names
// hinting at a climate borrow that climate's palette; everything else gets a
// stable generated colour.
func (b *Biomes) fallbackLocked(key string) Biome {
	base := DeterministicColor(key)
	bi := Biome{
		Name:    key,
		Grass:   color.NRGBA{0x8e, 0xb9, 0x71, 255},
		Foliage: color.NRGBA{0x71, 0xa7, 0x4d, 255},
		Water:   color.NRGBA{0x3f, 0x76, 0xe4, 255},
		Overlay: base,
	}
	switch {
	case containsAny(key, "desert", "badlands", "mesa", "dune", "sand"):
		bi.Grass = color.NRGBA{0xbf, 0xb7, 0x55, 255}
		bi.Foliage = color.NRGBA{0xae, 0xa4, 0x2a, 255}
	case containsAny(key, "snow", "frozen", "ice", "glacier", "tundra"):
		bi.Grass = color.NRGBA{0x80, 0xb4, 0x97, 255}
		bi.Foliage = color.NRGBA{0x60, 0xa1, 0x7b, 255}
		bi.Water = color.NRGBA{0x39, 0x38, 0xc9, 255}
	case containsAny(key, "jungle", "rainforest", "tropic"):
		bi.Grass = color.NRGBA{0x59, 0xc9, 0x3c, 255}
		bi.Foliage = color.NRGBA{0x30, 0xbb, 0x0b, 255}
	case containsAny(key, "swamp", "marsh", "bog", "mangrove"):
		bi.Grass = color.NRGBA{0x6a, 0x70, 0x39, 255}
		bi.Foliage = color.NRGBA{0x6a, 0x70, 0x39, 255}
		bi.Water = color.NRGBA{0x61, 0x7b, 0x64, 255}
	case containsAny(key, "savanna", "steppe", "shrub"):
		bi.Grass = color.NRGBA{0xbf, 0xb7, 0x55, 255}
		bi.Foliage = color.NRGBA{0xae, 0xa4, 0x2a, 255}
	case containsAny(key, "taiga", "spruce", "pine", "boreal"):
		bi.Grass = color.NRGBA{0x86, 0xb7, 0x83, 255}
		bi.Foliage = color.NRGBA{0x68, 0xa4, 0x64, 255}
	case containsAny(key, "nether", "crimson", "warped", "soul", "basalt"):
		bi.Grass = color.NRGBA{0x9a, 0x50, 0x40, 255}
		bi.Foliage = color.NRGBA{0x8a, 0x40, 0x35, 255}
		bi.Water = color.NRGBA{0x90, 0x59, 0x2c, 255}
	case containsAny(key, "end", "void", "chorus"):
		bi.Grass = color.NRGBA{0xc0, 0xbd, 0x8f, 255}
		bi.Foliage = color.NRGBA{0xb0, 0xac, 0x7f, 255}
		bi.Water = color.NRGBA{0x40, 0x3a, 0x5e, 255}
	case containsAny(key, "ocean", "river", "beach", "shore"):
		bi.Grass = color.NRGBA{0x8e, 0xb9, 0x71, 255}
		bi.Water = color.NRGBA{0x3d, 0x6f, 0xd4, 255}
	}
	return bi
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ID resolves a biome identifier, registering it with generated tints if
// unknown. Never fails.
func (b *Biomes) ID(name string) uint16 {
	key := normalize(name)

	b.mu.RLock()
	id, ok := b.byName[key]
	b.mu.RUnlock()
	if ok {
		return id
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if id, ok := b.byName[key]; ok {
		b.unknown[key]++
		return id
	}
	b.unknown[key]++
	return b.putLocked(b.fallbackLocked(key))
}

// Get returns the biome for an index, falling back to the neutral default.
func (b *Biomes) Get(id uint16) Biome {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if int(id) >= len(b.list) {
		return b.list[PlainsBiomeID]
	}
	return b.list[id]
}

// Len returns the number of registered biomes.
func (b *Biomes) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.list)
}

// Names returns every registered biome identifier, sorted.
func (b *Biomes) Names() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.list))
	for _, bi := range b.list {
		out = append(out, bi.Name)
	}
	sort.Strings(out)
	return out
}

// Unknown returns biomes that fell back to generated tints, most frequent first.
func (b *Biomes) Unknown(limit int) []UnknownReport {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]UnknownReport, 0, len(b.unknown))
	for name, count := range b.unknown {
		id, ok := b.byName[name]
		if !ok || b.list[id].Known {
			continue
		}
		out = append(out, UnknownReport{Name: name, Count: count, Color: HexOf(b.list[id].Overlay)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ApplyTint returns the block colour with its biome tint applied.
func ApplyTint(base color.NRGBA, t Tint, bi Biome) color.NRGBA {
	switch t {
	case TintGrass:
		return Multiply(base, bi.Grass)
	case TintFoliage, TintDryFoliage:
		return Multiply(base, bi.Foliage)
	case TintWater:
		return Multiply(base, bi.Water)
	default:
		return base
	}
}
