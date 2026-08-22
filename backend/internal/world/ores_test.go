package world

import "testing"

func TestClassifyOreHandlesRealBlockNames(t *testing.T) {
	cases := map[string]OreKind{
		// Both stone and deepslate variants must land on the same kind, which
		// is the whole reason matching is on substrings.
		"minecraft:diamond_ore":           OreDiamond,
		"minecraft:deepslate_diamond_ore": OreDiamond,
		"minecraft:coal_ore":              OreCoal,
		"minecraft:deepslate_coal_ore":    OreCoal,
		"minecraft:nether_gold_ore":       OreGold,
		"minecraft:ancient_debris":        OreDebris,
		"minecraft:nether_quartz_ore":     OreOther,
		// A modded ore nobody has heard of still has to appear on the map.
		"somemod:tin_ore": OreOther,
		// Things that merely mention a metal, or are the refined block rather
		// than the ore, must not be counted as ore.
		"minecraft:diamond_block": OreNone,
		"minecraft:iron_bars":     OreNone,
		"minecraft:stone":         OreNone,
		"minecraft:air":           OreNone,
	}
	for name, want := range cases {
		if got := ClassifyOre(name); got != want {
			t.Errorf("ClassifyOre(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestOreKindRanksByValue guards the ordering the enum's own doc comment
// promises: a column is summarised by max(OreKind), so if these ever stop
// ascending by value, a column containing both coal and diamond would start
// reporting coal.
func TestOreKindRanksByValue(t *testing.T) {
	ascending := []OreKind{
		OreNone, OreOther, OreCoal, OreCopper, OreIron,
		OreLapis, OreRedstone, OreGold, OreEmerald, OreDiamond, OreDebris,
	}
	for i := 1; i < len(ascending); i++ {
		if ascending[i] <= ascending[i-1] {
			t.Fatalf("%v (%d) does not rank above %v (%d); max() would pick the wrong ore",
				ascending[i], ascending[i], ascending[i-1], ascending[i-1])
		}
	}
}

func TestClassifyOreIgnoresNamespace(t *testing.T) {
	// A pack that re-registers vanilla ores under its own namespace should
	// still get the vanilla classification.
	if got := ClassifyOre("otherpack:emerald_ore"); got != OreEmerald {
		t.Errorf("namespaced emerald ore = %v, want %v", got, OreEmerald)
	}
	if got := ClassifyOre("EMERALD_ORE"); got != OreEmerald {
		t.Errorf("uppercase name = %v, want %v (matching must be case-insensitive)", got, OreEmerald)
	}
}
