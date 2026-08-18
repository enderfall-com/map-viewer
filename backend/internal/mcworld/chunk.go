// Package mcworld turns Anvil region files into render-ready surfaces.
package mcworld

import (
	"fmt"
	"strings"

	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/nbt"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

// Data versions that changed chunk storage in ways this decoder must know about.
const (
	// dvPacking is 20w17a: block state entries stopped spanning long boundaries.
	dvPacking = 2529
	// dvSections is 21w39a: sections moved to the chunk root and biomes became
	// palettised per section.
	dvSections = 2825
)

// packed reads fixed-width entries out of a Minecraft long array.
//
// Two packings exist. Before 1.16 entries were stored tightly and could span a
// long boundary; from 1.16 each long holds a whole number of entries and the
// remaining high bits are padding. Reading a modern world with the legacy
// unpacker (or the reverse) does not fail loudly -- it silently yields the
// wrong blocks -- so the data version selects the scheme explicitly.
type packed struct {
	data     []int64
	bits     int
	spanning bool
	mask     uint64
	perLong  int
}

func newPacked(data []int64, bits int, spanning bool) packed {
	if bits <= 0 {
		bits = 1
	}
	if bits > 64 {
		bits = 64
	}
	p := packed{data: data, bits: bits, spanning: spanning}
	if bits == 64 {
		p.mask = ^uint64(0)
	} else {
		p.mask = (uint64(1) << bits) - 1
	}
	p.perLong = 64 / bits
	return p
}

// at returns entry i, or 0 if the array is too short. A short array is a
// corrupt chunk rather than a reason to panic mid-render.
func (p packed) at(i int) int {
	if len(p.data) == 0 || i < 0 {
		return 0
	}
	if !p.spanning {
		if p.perLong == 0 {
			return 0
		}
		idx := i / p.perLong
		if idx >= len(p.data) {
			return 0
		}
		shift := (i % p.perLong) * p.bits
		return int((uint64(p.data[idx]) >> shift) & p.mask)
	}

	bitPos := i * p.bits
	idx := bitPos >> 6
	if idx >= len(p.data) {
		return 0
	}
	off := bitPos & 63
	v := uint64(p.data[idx]) >> off
	if off+p.bits > 64 {
		if idx+1 >= len(p.data) {
			return int(v & p.mask)
		}
		v |= uint64(p.data[idx+1]) << (64 - off)
	}
	return int(v & p.mask)
}

// bitsNeeded returns how many bits index n distinct values.
func bitsNeeded(n int) int {
	if n <= 1 {
		return 1
	}
	b := 0
	for (1 << b) < n {
		b++
	}
	return b
}

// palEntry is a resolved palette slot.
type palEntry struct {
	id          uint16
	transparent bool
	water       bool
	// waterlogged marks a solid block that also contains water, so submerged
	// stairs and slabs still read as ocean floor rather than as dry land.
	waterlogged bool
	// decoration marks a thin plant that should be remembered and painted
	// over the solid ground beneath it rather than simply skipped like air.
	decoration bool
}

// section is one 16x16x16 slice of a chunk.
type section struct {
	y int

	palette []palEntry
	states  packed
	// single is set when a section has a one-entry palette and no data array,
	// which is how Minecraft stores solid stone and (very commonly) pure air.
	single    bool
	singleIdx int

	biomePalette []uint16
	biomes       packed
	biomeSingle  bool
	biomeSingleV uint16

	blockLight []byte
	skyLight   []byte

	// allTransparent short-circuits the surface scan for air-only sections,
	// which is the overwhelming majority of sections above the terrain.
	allTransparent bool
}

// blockAt returns the palette entry at a local position.
func (s *section) blockAt(x, y, z int) palEntry {
	if len(s.palette) == 0 {
		return palEntry{transparent: true}
	}
	idx := s.singleIdx
	if !s.single {
		idx = s.states.at((y*16+z)*16 + x)
	}
	if idx < 0 || idx >= len(s.palette) {
		// A palette index out of range means a corrupt chunk; treat the block
		// as air so the column scan continues rather than aborting the tile.
		return palEntry{transparent: true}
	}
	return s.palette[idx]
}

// biomeAt returns the biome table index at a local position. Biomes are stored
// per 4x4x4 cell, so the coordinates are divided down.
func (s *section) biomeAt(x, y, z int) (uint16, bool) {
	if len(s.biomePalette) == 0 {
		return 0, false
	}
	if s.biomeSingle {
		return s.biomeSingleV, true
	}
	i := ((y>>2)*4+(z>>2))*4 + (x >> 2)
	idx := s.biomes.at(i)
	if idx < 0 || idx >= len(s.biomePalette) {
		return 0, false
	}
	return s.biomePalette[idx], true
}

// lightAt returns the stored light level, preferring block light and falling
// back to sky light.
func (s *section) lightAt(x, y, z int) uint8 {
	i := (y*16+z)*16 + x
	best := uint8(0)
	found := false
	for _, arr := range [][]byte{s.blockLight, s.skyLight} {
		if len(arr)*2 <= i {
			continue
		}
		b := arr[i/2]
		var v uint8
		if i%2 == 0 {
			v = b & 0x0F
		} else {
			v = b >> 4
		}
		if v > best {
			best = v
		}
		found = true
	}
	if !found {
		// A world saved without light data must not render pitch black.
		return 15
	}
	return best
}

// decodedChunk is a chunk parsed into sections.
type decodedChunk struct {
	pos         mcmath.ChunkPos
	dataVersion int
	sections    map[int]*section
	minSectionY int
	maxSectionY int
	// legacyBiomes carries the pre-1.18 whole-chunk biome array.
	legacyBiomes []uint16
	legacy3D     bool
	empty        bool
}

// decodeChunk converts chunk NBT into sections.
//
// It handles both the pre-1.18 layout, where everything lives under a "Level"
// compound and biomes are a chunk-wide numeric array, and the modern layout,
// where sections sit at the root and biomes are palettised per section. Modded
// worlds sit on both sides of that line, so neither can be dropped.
func (w *World) decodeChunk(pos mcmath.ChunkPos, root nbt.Tag) (*decodedChunk, error) {
	dc := &decodedChunk{pos: pos, sections: make(map[int]*section, 24), minSectionY: 1 << 30, maxSectionY: -(1 << 30)}
	dc.dataVersion = int(root.GetInt(0, "DataVersion"))

	// Pre-1.18 wraps everything in "Level".
	level := root
	if lv, ok := root.Get("Level"); ok && lv.Type == nbt.TagCompound {
		level = lv
	}

	// A chunk that has not finished generating has no usable surface.
	status := strings.TrimPrefix(level.GetString("", "Status"), "minecraft:")
	if status == "" {
		status = strings.TrimPrefix(root.GetString("", "Status"), "minecraft:")
	}
	if status != "" && status != "full" && status != "postprocessed" && status != "fullchunk" && status != "heightmaps" && status != "spawn" && status != "light" {
		dc.empty = true
		return dc, nil
	}

	sectionsTag, _ := level.GetAny("sections", "Sections")
	if sectionsTag.Type != nbt.TagList {
		if st, ok := root.GetAny("sections", "Sections"); ok && st.Type == nbt.TagList {
			sectionsTag = st
		} else {
			dc.empty = true
			return dc, nil
		}
	}

	spanning := dc.dataVersion < dvPacking

	for _, st := range sectionsTag.List {
		if st.Type != nbt.TagCompound {
			continue
		}
		sy := int(st.GetInt(-1000, "Y"))
		if sy == -1000 {
			continue
		}
		sec, err := w.decodeSection(st, spanning)
		if err != nil {
			// Skip the bad section; the rest of the column still renders.
			continue
		}
		if sec == nil {
			continue
		}
		sec.y = sy
		dc.sections[sy] = sec
		if sy < dc.minSectionY {
			dc.minSectionY = sy
		}
		if sy > dc.maxSectionY {
			dc.maxSectionY = sy
		}
	}
	if len(dc.sections) == 0 {
		dc.empty = true
		return dc, nil
	}

	// Pre-1.18 chunk-wide biome array: 256 entries for 2D, 1024 for 3D.
	if bt, ok := level.Get("Biomes"); ok {
		switch {
		case bt.Type == nbt.TagIntArray && len(bt.Ints) > 0:
			dc.legacyBiomes = make([]uint16, len(bt.Ints))
			for i, v := range bt.Ints {
				dc.legacyBiomes[i] = w.biomes.ID(legacyBiomeName(int(v)))
			}
			dc.legacy3D = len(bt.Ints) >= 1024
		case bt.Type == nbt.TagByteArray && len(bt.Bytes) > 0:
			dc.legacyBiomes = make([]uint16, len(bt.Bytes))
			for i, v := range bt.Bytes {
				dc.legacyBiomes[i] = w.biomes.ID(legacyBiomeName(int(v)))
			}
		}
	}

	return dc, nil
}

// decodeSection decodes one section's blocks, biomes and light.
func (w *World) decodeSection(st nbt.Tag, spanning bool) (*section, error) {
	sec := &section{}

	// Modern layout: block_states { palette, data }.
	// Legacy layout: Palette + BlockStates at the section root.
	var paletteTag nbt.Tag
	var dataLongs []int64
	var havePalette bool

	if bs, ok := st.Get("block_states"); ok && bs.Type == nbt.TagCompound {
		if p, ok := bs.Get("palette"); ok && p.Type == nbt.TagList {
			paletteTag, havePalette = p, true
		}
		dataLongs = bs.GetLongs("data")
	} else if p, ok := st.Get("Palette"); ok && p.Type == nbt.TagList {
		paletteTag, havePalette = p, true
		dataLongs = st.GetLongs("BlockStates")
	}

	if !havePalette || len(paletteTag.List) == 0 {
		// A section with no palette is empty air; there is nothing to render but
		// it is not an error.
		return nil, nil
	}

	sec.palette = make([]palEntry, 0, len(paletteTag.List))
	allTransparent := true
	for _, entry := range paletteTag.List {
		name := entry.GetString("", "Name")
		if name == "" {
			name = entry.String_("")
		}
		if name == "" {
			name = "minecraft:air"
		}
		id := w.blocks.ID(name)
		blk := w.blocks.Get(id)

		pe := palEntry{id: id, transparent: blk.Transparent, water: blk.Water, decoration: blk.Decoration}
		// Waterlogged state is carried in the block properties, not the name.
		if props, ok := entry.Get("Properties"); ok && props.Type == nbt.TagCompound {
			if wl, ok := props.Get("waterlogged"); ok && wl.String_("") == "true" {
				pe.waterlogged = true
			}
		}
		if !pe.transparent || pe.decoration {
			// A decoration is still "transparent" for surface-height purposes
			// (see below), but it must not make the section eligible for the
			// bulk allTransparent skip: that skip jumps straight past every
			// block in the section, including the one decoration the scan
			// exists to find.
			allTransparent = false
		}
		sec.palette = append(sec.palette, pe)
	}
	sec.allTransparent = allTransparent

	if len(sec.palette) == 1 || len(dataLongs) == 0 {
		sec.single = true
		sec.singleIdx = 0
	} else {
		bits := bitsNeeded(len(sec.palette))
		if !spanning && bits < 4 {
			// Modern worlds store block states at a minimum of 4 bits.
			bits = 4
		}
		sec.states = newPacked(dataLongs, bits, spanning)
	}

	// Modern per-section biome palette.
	if bt, ok := st.Get("biomes"); ok && bt.Type == nbt.TagCompound {
		if p, ok := bt.Get("palette"); ok && p.Type == nbt.TagList && len(p.List) > 0 {
			sec.biomePalette = make([]uint16, 0, len(p.List))
			for _, e := range p.List {
				sec.biomePalette = append(sec.biomePalette, w.biomes.ID(e.String_("minecraft:plains")))
			}
			bdata := bt.GetLongs("data")
			if len(sec.biomePalette) == 1 || len(bdata) == 0 {
				sec.biomeSingle = true
				sec.biomeSingleV = sec.biomePalette[0]
			} else {
				// Biome packing never spans longs and has no 4-bit minimum.
				sec.biomes = newPacked(bdata, bitsNeeded(len(sec.biomePalette)), false)
			}
		}
	}

	if bl, ok := st.Get("BlockLight"); ok && bl.Type == nbt.TagByteArray {
		sec.blockLight = bl.Bytes
	}
	if sl, ok := st.Get("SkyLight"); ok && sl.Type == nbt.TagByteArray {
		sec.skyLight = sl.Bytes
	}

	return sec, nil
}

// biomeFor resolves a column's biome, preferring the modern per-section palette
// and falling back to the legacy chunk-wide array.
func (dc *decodedChunk) biomeFor(x, y, z int) (uint16, bool) {
	sy := mcmath.FloorDiv(y, 16)
	if sec, ok := dc.sections[sy]; ok {
		if b, ok := sec.biomeAt(x, mcmath.FloorMod(y, 16), z); ok {
			return b, true
		}
	}
	if len(dc.legacyBiomes) == 0 {
		return 0, false
	}
	if dc.legacy3D && len(dc.legacyBiomes) >= 1024 {
		// 3D legacy biomes index 4x4x4 cells over a 256-tall world.
		yc := (y - dc.minSectionY*16) >> 2
		if yc < 0 {
			yc = 0
		}
		if yc > 63 {
			yc = 63
		}
		i := yc*16 + (z>>2)*4 + (x >> 2)
		if i >= 0 && i < len(dc.legacyBiomes) {
			return dc.legacyBiomes[i], true
		}
	}
	i := z*16 + x
	if i >= 0 && i < len(dc.legacyBiomes) {
		return dc.legacyBiomes[i], true
	}
	return 0, false
}

// surface scans every column of a decoded chunk top-down and produces the
// render-ready surface.
//
// The scan descends from the top of the highest populated section rather than
// from the build limit, and skips whole sections whose palette is entirely
// transparent. Deriving the start from the data instead of trusting a stored
// heightmap keeps floating islands, modded structures and worlds with stale
// heightmaps rendering correctly, and costs almost nothing because the sections
// above terrain are exactly the ones that get skipped wholesale.
func (w *World) surface(dc *decodedChunk, dim world.DimensionInfo) *world.ChunkSurface {
	cs := &world.ChunkSurface{Pos: dc.pos}
	if dc.empty {
		return cs
	}

	topY := dc.maxSectionY*16 + 15
	bottomY := dc.minSectionY * 16
	if dim.HasCeiling && dim.MaxY-1 < topY {
		// In a Nether-like dimension the bedrock roof would otherwise be the
		// only thing ever visible, so start below it.
		topY = dim.MaxY - 1
	}

	for z := 0; z < mcmath.ChunkSize; z++ {
		for x := 0; x < mcmath.ChunkSize; x++ {
			i := z*mcmath.ChunkSize + x

			waterTop := 0
			inWater := false
			found := false
			var decorID uint16

			for y := topY; y >= bottomY; y-- {
				sy := mcmath.FloorDiv(y, 16)
				sec, ok := dc.sections[sy]
				if !ok || sec.allTransparent {
					// Skip the whole section in one step.
					y = sy * 16 // loop decrement takes it below the section
					continue
				}
				ly := mcmath.FloorMod(y, 16)
				pe := sec.blockAt(x, ly, z)

				if pe.water {
					if !inWater {
						waterTop = y
						inWater = true
					}
					continue
				}
				if pe.transparent {
					if pe.decoration {
						// Overwritten on every hit while descending, so by the
						// time solid ground is found this holds the lowest
						// (closest-to-surface) decoration seen -- the one
						// actually sitting on top of that ground.
						decorID = pe.id
					}
					continue
				}
				// A waterlogged solid marks the water surface if nothing above
				// it did, then acts as the floor.
				if pe.waterlogged && !inWater {
					waterTop = y
					inWater = true
				}

				cs.Height[i] = int16(y)
				cs.Block[i] = pe.id
				cs.Decoration[i] = decorID
				cs.Light[i] = sec.lightAt(x, min(ly+1, 15), z)
				cs.Flags[i] = world.FlagPresent
				if inWater && waterTop > y {
					cs.Flags[i] |= world.FlagWater
					cs.WaterY[i] = int16(waterTop)
				}
				if b, ok := dc.biomeFor(x, y, z); ok {
					cs.Biome[i] = b
				}
				found = true
				break
			}

			if !found {
				if inWater {
					// Water all the way down: render the water with the world
					// floor as its bed.
					cs.Height[i] = int16(bottomY)
					cs.WaterY[i] = int16(waterTop)
					cs.Block[i] = w.waterID
					cs.Light[i] = 15
					cs.Flags[i] = world.FlagPresent | world.FlagWater
					if b, ok := dc.biomeFor(x, waterTop, z); ok {
						cs.Biome[i] = b
					}
				}
				// Otherwise the column is genuinely empty (void), and stays
				// unset so it renders as unexplored.
			}
		}
	}
	return cs
}

// legacyBiomeName maps a pre-1.18 numeric biome id to its modern identifier.
// Anything unrecognised becomes plains, which is visually neutral, and modded
// numeric ids in old worlds fall into the same bucket rather than failing.
func legacyBiomeName(id int) string {
	if n, ok := legacyBiomes[id]; ok {
		return n
	}
	return "minecraft:plains"
}

var legacyBiomes = map[int]string{
	0: "minecraft:ocean", 1: "minecraft:plains", 2: "minecraft:desert",
	3: "minecraft:windswept_hills", 4: "minecraft:forest", 5: "minecraft:taiga",
	6: "minecraft:swamp", 7: "minecraft:river", 8: "minecraft:nether_wastes",
	9: "minecraft:the_end", 10: "minecraft:frozen_ocean", 11: "minecraft:frozen_river",
	12: "minecraft:snowy_plains", 13: "minecraft:snowy_plains", 14: "minecraft:mushroom_fields",
	15: "minecraft:mushroom_fields", 16: "minecraft:beach", 17: "minecraft:desert",
	18: "minecraft:forest", 19: "minecraft:taiga", 20: "minecraft:windswept_hills",
	21: "minecraft:jungle", 22: "minecraft:jungle", 23: "minecraft:sparse_jungle",
	24: "minecraft:deep_ocean", 25: "minecraft:stony_shore", 26: "minecraft:snowy_beach",
	27: "minecraft:birch_forest", 28: "minecraft:birch_forest", 29: "minecraft:dark_forest",
	30: "minecraft:snowy_taiga", 31: "minecraft:snowy_taiga", 32: "minecraft:old_growth_pine_taiga",
	33: "minecraft:old_growth_pine_taiga", 34: "minecraft:windswept_forest",
	35: "minecraft:savanna", 36: "minecraft:savanna_plateau", 37: "minecraft:badlands",
	38: "minecraft:wooded_badlands", 39: "minecraft:badlands", 40: "minecraft:small_end_islands",
	41: "minecraft:end_midlands", 42: "minecraft:end_highlands", 43: "minecraft:end_barrens",
	44: "minecraft:warm_ocean", 45: "minecraft:lukewarm_ocean", 46: "minecraft:cold_ocean",
	47: "minecraft:deep_lukewarm_ocean", 48: "minecraft:deep_cold_ocean",
	49: "minecraft:deep_frozen_ocean", 50: "minecraft:deep_frozen_ocean",
	127: "minecraft:the_void",
	129: "minecraft:sunflower_plains", 130: "minecraft:desert", 131: "minecraft:windswept_gravelly_hills",
	132: "minecraft:flower_forest", 133: "minecraft:taiga", 134: "minecraft:swamp",
	140: "minecraft:ice_spikes", 149: "minecraft:jungle", 151: "minecraft:sparse_jungle",
	155: "minecraft:old_growth_birch_forest", 156: "minecraft:old_growth_birch_forest",
	157: "minecraft:dark_forest", 158: "minecraft:snowy_taiga",
	160: "minecraft:old_growth_spruce_taiga", 161: "minecraft:old_growth_spruce_taiga",
	162: "minecraft:windswept_hills", 163: "minecraft:windswept_savanna",
	164: "minecraft:savanna_plateau", 165: "minecraft:eroded_badlands",
	166: "minecraft:wooded_badlands", 167: "minecraft:badlands",
	168: "minecraft:bamboo_jungle", 169: "minecraft:bamboo_jungle",
	174: "minecraft:dripstone_caves", 175: "minecraft:lush_caves",
	177: "minecraft:meadow", 178: "minecraft:grove", 179: "minecraft:snowy_slopes",
	180: "minecraft:jagged_peaks", 181: "minecraft:frozen_peaks", 182: "minecraft:stony_peaks",
}

// describeChunk renders a short chunk summary for diagnostics.
func describeChunk(dc *decodedChunk) string {
	if dc.empty {
		return "empty"
	}
	return fmt.Sprintf("dv=%d sections=%d y=%d..%d",
		dc.dataVersion, len(dc.sections), dc.minSectionY*16, dc.maxSectionY*16+15)
}
