package blocks

import "testing"

func TestInferHeightCoversGeneratedBlockFamilies(t *testing.T) {
	cases := []struct {
		name string
		want float32
		ok   bool
	}{
		// The whole point: variants nobody will ever list in blocks.json.
		{"minecraft:spruce_stairs", 0.5, true},
		{"minecraft:deepslate_brick_stairs", 0.5, true},
		{"somemod:ruby_stairs", 0.5, true},
		{"minecraft:petrified_oak_slab", 0.5, true},
		{"somemod:ruby_slab", 0.5, true},
		{"minecraft:red_carpet", 0.0625, true},
		{"minecraft:oak_trapdoor", 0.1875, true},
		{"minecraft:red_bed", 0.5625, true},
		{"minecraft:farmland", 0.9375, true},
		// Ordinary cubes must report nothing so the caller keeps height 1.
		{"minecraft:stone", 0, false},
		{"minecraft:oak_planks", 0, false},
		// A door is not a trapdoor, and must not borrow its height.
		{"minecraft:oak_door", 0, false},
	}
	for _, c := range cases {
		got, ok := InferHeight(c.name)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("InferHeight(%q) = (%v, %v), want (%v, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

// TestUnknownStairGetsInferredHeight checks the wiring, not just the helper:
// a block the registry has never seen must come back half-height.
func TestUnknownStairGetsInferredHeight(t *testing.T) {
	r := NewRegistry()
	id := r.ID("somemod:mystery_stairs")
	if h := r.Get(id).Height; h != 0.5 {
		t.Errorf("unknown modded stair height = %v, want 0.5", h)
	}
	if h := r.Get(r.ID("somemod:mystery_block")).Height; h != 1 {
		t.Errorf("unknown ordinary block height = %v, want 1", h)
	}
}
