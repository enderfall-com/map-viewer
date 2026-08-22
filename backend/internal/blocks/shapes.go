package blocks

import "strings"

// InferHeight guesses a block's rendered height in blocks from its
// identifier, for blocks blocks.json does not describe explicitly.
//
// # Why this is name-based
//
// The real answer lives in the block's model JSON inside the game's assets,
// which the texture layer can read -- but only when texture sources are
// configured at all, and the isometric renderer must not look different
// depending on whether the operator pointed at a Minecraft install. Meanwhile
// blocks.json declares a height for just nine blocks, four of them stairs, so
// relying on it alone leaves every spruce, deepslate and modded stair
// standing a full block tall.
//
// Naming in Minecraft is strongly conventional precisely because these are
// generated block families: anything ending "_slab" is half a block in every
// vanilla and modded pack, and the same holds for "_stairs", "_carpet" and
// "_trapdoor". Matching the suffix therefore covers the hundreds of variants
// a modpack adds without naming a single one of them.
//
// Returns ok=false when nothing is known about the name, which callers must
// treat as a full cube rather than as a zero-height block.
func InferHeight(name string) (float32, bool) {
	n := strings.ToLower(name)
	if i := strings.IndexByte(n, ':'); i >= 0 {
		n = n[i+1:]
	}
	// Ordered longest-suffix-first where families overlap, so
	// "oak_trapdoor" is not mistaken for a door and "petrified_oak_slab"
	// still resolves as a slab.
	switch {
	case strings.HasSuffix(n, "_slab"), n == "slab":
		return 0.5, true
	case strings.HasSuffix(n, "_stairs"), n == "stairs":
		// A stair is a half slab plus a half-footprint step on one side. It
		// is drawn as a half-height block: the top of the lower half is the
		// surface you actually walk on and the silhouette that reads
		// correctly from above, whereas drawing the whole cell full height
		// -- what happened before this existed -- makes every staircase and
		// stair-built roof a block too tall.
		return 0.5, true
	case strings.HasSuffix(n, "_carpet"), n == "carpet", strings.HasSuffix(n, "_moss_carpet"):
		return 0.0625, true
	case strings.HasSuffix(n, "_trapdoor"):
		return 0.1875, true
	case strings.HasSuffix(n, "_bed"):
		return 0.5625, true
	case n == "farmland", n == "dirt_path", n == "grass_path":
		return 0.9375, true
	case n == "enchanting_table":
		return 0.75, true
	case n == "soul_sand", n == "mud":
		return 0.875, true
	}
	return 0, false
}
