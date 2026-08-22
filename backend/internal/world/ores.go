package world

import "strings"

// OreKind classifies a block as a mineable resource, for the ore-heatmap
// render style.
//
// It is a small closed enum rather than a block id so that the producer
// (mcworld's chunk scanner) and the consumer (render's shader) share one
// vocabulary without the renderer needing the block registry, and so a
// column's summary costs one byte instead of two.
//
// The order is deliberately worst-to-best: a column is summarised by the
// single most valuable thing under it, which is simply the maximum OreKind
// seen while scanning it. Inserting a new kind therefore means putting it in
// the right place in this list, not appending to the end.
type OreKind uint8

const (
	// OreNone means no ore at all was found in the column.
	OreNone OreKind = iota
	// OreOther is any unrecognised "*_ore" block, which is how a modded ore
	// still shows up on the map instead of being invisible. It ranks lowest
	// of the real ores because nothing is known about its value.
	OreOther
	OreCoal
	OreCopper
	OreIron
	OreLapis
	OreRedstone
	OreGold
	OreEmerald
	OreDiamond
	// OreDebris is ancient debris. Not named "*_ore" and the rarest thing in
	// the game, so it ranks above everything.
	OreDebris
)

// ClassifyOre maps a block identifier to its OreKind.
//
// Matching is on substrings rather than exact names because every ore has at
// least two variants (stone and deepslate) and modded packs add many more
// prefixes; "diamond_ore" catches minecraft:diamond_ore,
// minecraft:deepslate_diamond_ore and somemod:compressed_diamond_ore alike.
// The namespace is deliberately ignored for the same reason.
func ClassifyOre(name string) OreKind {
	n := strings.ToLower(name)
	if i := strings.IndexByte(n, ':'); i >= 0 {
		n = n[i+1:]
	}
	switch {
	case strings.Contains(n, "ancient_debris"):
		return OreDebris
	case strings.Contains(n, "diamond_ore"):
		return OreDiamond
	case strings.Contains(n, "emerald_ore"):
		return OreEmerald
	case strings.Contains(n, "gold_ore"):
		return OreGold
	case strings.Contains(n, "redstone_ore"):
		return OreRedstone
	case strings.Contains(n, "lapis_ore"):
		return OreLapis
	case strings.Contains(n, "iron_ore"):
		return OreIron
	case strings.Contains(n, "copper_ore"):
		return OreCopper
	case strings.Contains(n, "coal_ore"):
		return OreCoal
	case strings.HasSuffix(n, "_ore"), strings.Contains(n, "_ore_"):
		return OreOther
	}
	return OreNone
}

// OrePoints is how much one block of each kind contributes to a column's
// OreScore.
//
// The spread is deliberately brutal, and it is what makes an ore map usable
// at all. Summarising a 384-block column by the best ore in it saturates:
// virtually every column that reaches the deepslate layer contains some
// redstone, some lapis and often a diamond, so a "best ore" map colours
// essentially every pixel and reads as rainbow static. Scoring instead --
// with the ores nobody travels for worth literally nothing, and the ones
// worth a trip worth an order of magnitude more -- leaves the map dark
// except where something genuinely worth mining is concentrated.
//
// Coal, copper and iron score zero on purpose: they are everywhere, and a
// map that highlights them highlights nothing.
var OrePoints = [...]int{
	OreNone:     0,
	OreOther:    1,
	OreCoal:     0,
	OreCopper:   0,
	OreIron:     0,
	OreLapis:    1,
	OreRedstone: 1,
	OreGold:     4,
	OreEmerald:  10,
	OreDiamond:  12,
	OreDebris:   20,
}

// Points returns an ore kind's contribution to a column's score.
func (o OreKind) Points() int {
	if int(o) >= len(OrePoints) {
		return OrePoints[OreOther]
	}
	return OrePoints[o]
}

// String names an ore kind, for the block info panel and debugging.
func (o OreKind) String() string {
	switch o {
	case OreOther:
		return "ore"
	case OreCoal:
		return "coal"
	case OreCopper:
		return "copper"
	case OreIron:
		return "iron"
	case OreLapis:
		return "lapis"
	case OreRedstone:
		return "redstone"
	case OreGold:
		return "gold"
	case OreEmerald:
		return "emerald"
	case OreDiamond:
		return "diamond"
	case OreDebris:
		return "ancient debris"
	}
	return "none"
}
