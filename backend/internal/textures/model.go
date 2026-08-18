package textures

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// faceRef names one face's texture, and optionally a second texture layered
// on top of it with its own tint. The overlay exists because of grass_block:
// vanilla models it as two identical full-cube elements, a base pass (dirt
// sides, tinted top) and a second pass that draws just a tinted green strip
// (grass_block_side_overlay) over the plain dirt-brown sides. Without
// supporting a second layered element, the single most recognisable block in
// the game would fall back to a flat colour.
type faceRef struct {
	Texture     string
	Tint        bool
	OverlayTex  string
	OverlayTint bool
}

// resolvedFaces names the concrete texture reference (e.g.
// "minecraft:block/stone") assigned to each side of a simple cuboid.
type resolvedFaces struct {
	Up, Down, Side faceRef
}

// maxParentDepth guards against a cyclic or absurdly deep parent chain in a
// malformed pack; every real Minecraft model resolves in well under this.
const maxParentDepth = 16

// DebugFaces is a human-readable summary of one block's resolved faces, for
// diagnostics -- confirming why a particular modded block did or did not
// pick up a texture, without needing a debugger.
type DebugFaces struct {
	Up, Down, Side             string
	OverlayUp, OverlaySide     string
	TintUp, TintDown, TintSide bool
}

// DebugResolve exposes the model resolver for diagnostics and tooling.
func DebugResolve(store *Store, blockID string) (DebugFaces, bool) {
	f, ok := resolveBlockModel(store, blockID)
	if !ok {
		return DebugFaces{}, false
	}
	return DebugFaces{
		Up: f.Up.Texture, Down: f.Down.Texture, Side: f.Side.Texture,
		OverlayUp: f.Up.OverlayTex, OverlaySide: f.Side.OverlayTex,
		TintUp: f.Up.Tint, TintDown: f.Down.Tint, TintSide: f.Side.Tint,
	}, true
}

// ClassifyDecoration reports whether blockID's real model resolves to a
// billboard shape (see isBillboardShape): a thin plant that the world
// scanner should treat as sitting on top of whatever is beneath it rather
// than as an opaque cube filling its own column (see blocks.Block.Decoration
// and world.ChunkSurface.Decoration). It shares resolveBlockModel's
// blockstate and model-chain loading but skips texture-variable resolution,
// since only the geometry is needed here.
func ClassifyDecoration(store *Store, blockID string) bool {
	chain, ok := loadModelChainForBlock(store, blockID)
	if !ok {
		return false
	}
	return isBillboardShape(firstElements(chain))
}

// loadModelChainForBlock resolves a namespaced block identifier down to its
// leaf-first model chain, or reports ok=false for a block this simplified
// resolver does not attempt: no blockstate, or a multipart-only one (used by
// connected/directional shapes like fences and iron bars, which this package
// does not model).
func loadModelChainForBlock(store *Store, blockID string) ([]modelFile, bool) {
	ns, local, ok := splitRef(blockID)
	if !ok {
		return nil, false
	}
	bs, ok := readJSON[blockstateFile](store, fmt.Sprintf("assets/%s/blockstates/%s.json", ns, local))
	if !ok || len(bs.Variants) == 0 {
		return nil, false
	}
	modelRef, ok := pickVariantModel(bs.Variants)
	if !ok {
		return nil, false
	}
	return loadModelChain(store, modelRef)
}

// resolveBlockModel resolves a namespaced block identifier (e.g.
// "biomesoplenty:origin_grass_block") down to concrete per-face texture
// references, or reports ok=false for anything this simplified resolver does
// not attempt: multipart blockstates, and any model whose shape is not a
// single cuboid spanning the full 0..16 block (stairs, slabs, fences,
// crops, and similar -- these keep using the flat colour renderer).
func resolveBlockModel(store *Store, blockID string) (resolvedFaces, bool) {
	chain, ok := loadModelChainForBlock(store, blockID)
	if !ok {
		return resolvedFaces{}, false
	}

	vars := mergeTextureVariables(chain)
	elements := firstElements(chain)

	// Grass, ferns, saplings, flowers and crops all resolve to two or four
	// crossed, zero-thickness vertical planes sharing one texture -- vanilla
	// calls that shape block/cross, block/tinted_cross or block/crop, but a
	// mod that reimplements the identical shape under its own model name
	// (common: cross_with_overlay, template_crop_cross, broad_grass, ...)
	// is just as clearly a billboard plant. Detecting the shape itself
	// rather than checking for those specific parent names is what makes
	// this cover an arbitrary modpack's hundreds of custom plants without
	// naming each one. There is no true billboard renderer here, but
	// painting that one texture flat on every logical face is far closer
	// to "the real plant is here" than either a flat placeholder colour or
	// making the plant invisible.
	if isBillboardShape(elements) {
		if f := crossFace(elements, vars); f.Texture != "" {
			return resolvedFaces{Up: f, Down: f, Side: f}, true
		}
		return resolvedFaces{}, false
	}

	// Exactly one full-cube element is the common case (stone, planks, ore,
	// sand, ...). Exactly two identical full-cube elements is grass_block's
	// base-plus-tinted-overlay pattern (see faceRef). Anything else -- one
	// partial cuboid, three or more elements, non-cube shapes -- is a stair,
	// slab, fence, crop or similar and is left to the flat-colour renderer.
	if len(elements) < 1 || len(elements) > 2 {
		return resolvedFaces{}, false
	}
	for _, el := range elements {
		if !el.isFullCube() {
			return resolvedFaces{}, false
		}
	}

	base := elements[0]
	up := faceOf(base.Faces.Up, vars)
	down := faceOf(base.Faces.Down, vars)
	side := firstNonEmptySide(base.Faces, vars)
	if up.Texture == "" || side.Texture == "" {
		return resolvedFaces{}, false
	}

	if len(elements) == 2 {
		overlay := elements[1]
		if f := faceOf(overlay.Faces.Up, vars); f.Texture != "" {
			up.OverlayTex, up.OverlayTint = f.Texture, f.Tint
		}
		if f := firstNonEmptySide(overlay.Faces, vars); f.Texture != "" {
			side.OverlayTex, side.OverlayTint = f.Texture, f.Tint
		}
	}

	return resolvedFaces{Up: up, Down: down, Side: side}, true
}

// firstNonEmptySide picks a representative side face. Simple blocks use the
// same texture on all four sides (or vary only by rotation, e.g. a log's
// bark texture), so any one of them is representative for a top-down or
// isometric map; "north" is tried first purely for determinism.
func firstNonEmptySide(faces elementFaces, vars map[string]string) faceRef {
	for _, f := range []*modelFace{faces.North, faces.South, faces.East, faces.West} {
		if r := faceOf(f, vars); r.Texture != "" {
			return r
		}
	}
	return faceRef{}
}

// faceOf resolves one model face to a concrete texture reference and its
// tint flag.
func faceOf(mf *modelFace, vars map[string]string) faceRef {
	if mf == nil || mf.Texture == "" {
		return faceRef{}
	}
	tex := resolveTextureVar(mf.Texture, vars)
	if tex == "" {
		return faceRef{}
	}
	return faceRef{Texture: tex, Tint: mf.TintIndex != nil}
}

// ---------------------------------------------------------------------------
// Blockstate parsing
// ---------------------------------------------------------------------------

type blockstateFile struct {
	Variants  map[string]json.RawMessage `json:"variants"`
	Multipart []json.RawMessage          `json:"multipart"`
}

// pickVariantModel chooses one variant deterministically -- the empty-string
// "default" variant if present, otherwise the lexicographically first key, so
// the same pack resolves the same way on every restart regardless of Go's
// randomised map iteration order. Which specific property combination gets
// picked rarely matters for texture purposes: a log's "axis=x/y/z" variants
// differ only in rotation, not in which textures they use -- true for the
// vanilla logs that reuse one model rotated per axis (e.g. oak_log), but NOT
// for newer wood types (e.g. cherry_log) that ship three pre-oriented models
// instead of one rotated model, one per axis. There, "axis=x" and "axis=z"
// are lying on their side: their model's own "up" face is a bark face, not
// the cut end -- reading it as this resolver's "top" texture would put bark
// where a viewer expects a tree-ring end grain. "axis=y" -- vertical, the
// orientation an upright block would actually use -- is the one variant
// guaranteed to mean what "up" and "side" are supposed to mean here, so it
// is preferred whenever present, ahead of the alphabetical fallback below.
func pickVariantModel(variants map[string]json.RawMessage) (string, bool) {
	if raw, ok := variants[""]; ok {
		if m, ok := firstModelIn(raw); ok {
			return m, true
		}
	}
	if raw, ok := variants["axis=y"]; ok {
		if m, ok := firstModelIn(raw); ok {
			return m, true
		}
	}
	keys := make([]string, 0, len(variants))
	for k := range variants {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if m, ok := firstModelIn(variants[k]); ok {
			return m, true
		}
	}
	return "", false
}

// firstModelIn extracts a "model" reference from a variant value, which is
// either a single {"model": "..."} object or a weighted-random array of them.
func firstModelIn(raw json.RawMessage) (string, bool) {
	var single struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &single); err == nil && single.Model != "" {
		return single.Model, true
	}
	var list []struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &list); err == nil {
		for _, v := range list {
			if v.Model != "" {
				return v.Model, true
			}
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Model parsing
// ---------------------------------------------------------------------------

type modelFile struct {
	Parent   string            `json:"parent"`
	Textures map[string]string `json:"textures"`
	Elements []modelElement    `json:"elements"`
}

type modelElement struct {
	From  [3]float64   `json:"from"`
	To    [3]float64   `json:"to"`
	Faces elementFaces `json:"faces"`
}

// isFullCube reports whether this element spans the entire 0..16 block on
// every axis -- the shape every simple terrain block (stone, dirt, planks,
// leaves, ore, ...) uses, and the only shape this resolver textures. A slab,
// stair, fence post or crop element fails this check and falls back to the
// existing flat-colour renderer.
func (e modelElement) isFullCube() bool {
	const lo, hi = 0, 16
	for i := 0; i < 3; i++ {
		if e.From[i] != lo || e.To[i] != hi {
			return false
		}
	}
	return true
}

type elementFaces struct {
	Up, Down, North, South, East, West *modelFace
}

type modelFace struct {
	Texture   string `json:"texture"`
	TintIndex *int   `json:"tintindex"`
}

// UnmarshalJSON adapts the named-key face object into the struct above.
func (f *elementFaces) UnmarshalJSON(data []byte) error {
	var raw map[string]modelFace
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	assign := func(key string) *modelFace {
		if v, ok := raw[key]; ok {
			c := v
			return &c
		}
		return nil
	}
	f.Up = assign("up")
	f.Down = assign("down")
	f.North = assign("north")
	f.South = assign("south")
	f.East = assign("east")
	f.West = assign("west")
	return nil
}

// resolveTextureVar follows "#name" chains to a concrete texture reference.
func resolveTextureVar(ref string, vars map[string]string) string {
	for i := 0; i < maxParentDepth && strings.HasPrefix(ref, "#"); i++ {
		next, ok := vars[strings.TrimPrefix(ref, "#")]
		if !ok {
			return ""
		}
		ref = next
	}
	if strings.HasPrefix(ref, "#") {
		return "" // unresolved after the depth cap; treat as missing
	}
	return ref
}

// loadModelChain follows "parent" references from a model up to its root,
// returning the chain leaf-first. A missing file, a "builtin/..." parent
// (procedurally generated, not backed by a real model file) or an excessive
// chain length ends the walk and reports failure.
func loadModelChain(store *Store, ref string) ([]modelFile, bool) {
	var chain []modelFile
	seen := map[string]bool{}
	for depth := 0; depth < maxParentDepth; depth++ {
		if ref == "" || strings.HasPrefix(ref, "builtin/") {
			break
		}
		ns, local, ok := splitRef(ref)
		if !ok || seen[ns+":"+local] {
			break
		}
		seen[ns+":"+local] = true
		m, ok := readJSON[modelFile](store, fmt.Sprintf("assets/%s/models/%s.json", ns, local))
		if !ok {
			return chain, len(chain) > 0
		}
		chain = append(chain, m)
		ref = m.Parent
	}
	return chain, len(chain) > 0
}

// mergeTextureVariables flattens a leaf-first model chain's "textures" maps
// into one table, with a more specific (closer to the leaf) definition
// overriding a more general (closer to the root) one of the same name.
func mergeTextureVariables(chain []modelFile) map[string]string {
	out := make(map[string]string)
	// Walk root-first so leaf entries are applied last and win.
	for i := len(chain) - 1; i >= 0; i-- {
		for k, v := range chain[i].Textures {
			out[k] = v
		}
	}
	return out
}

// firstElements returns the elements array from the first (leaf-most) model
// in the chain that defines one. A child model normally has none and relies
// entirely on a vanilla parent (like block/cube) for geometry; a few define
// their own, which take precedence over an ancestor's.
func firstElements(chain []modelFile) []modelElement {
	for _, m := range chain {
		if len(m.Elements) > 0 {
			return m.Elements
		}
	}
	return nil
}

// isBillboardShape reports whether a model's elements form a cross-style
// billboard: two (block/cross, block/tinted_cross) or four (block/crop, for
// age-staged crops) zero-thickness vertical planes, each spanning
// (approximately) the full block height with no up or down face.
//
// This is deliberately a shape check, not a check for those specific
// vanilla parent names: many mods reimplement the identical shape under
// their own model name (cross_with_overlay, template_crop_cross,
// broad_grass, and similar have all been seen in the wild) instead of
// inheriting from vanilla's. A modpack can add hundreds of custom plants;
// naming each one is not tractable, but this shape is the one thing they
// all share. It is also what keeps genuinely structural thin blocks --
// fence posts, iron bars, chains -- from being misidentified: those are
// thin boxes with real (if small) thickness on every axis, never a true
// zero-thickness quad, and in vanilla and every mod pack inspected so far
// they are assembled through a multipart blockstate rather than referenced
// directly, so they never even reach this shape check (multipart is
// unsupported and rejected earlier, in loadModelChainForBlock's caller).
func isBillboardShape(elements []modelElement) bool {
	if len(elements) == 0 || len(elements)%2 != 0 {
		return false
	}
	for _, el := range elements {
		if !el.isBillboardPlane() {
			return false
		}
	}
	return true
}

// isBillboardPlane reports whether one element is a flat, full-height quad:
// zero thickness along exactly one horizontal axis, with no up/down face.
// (From/To are read before any model "rotation" transform is applied, so a
// rotated-45-degrees plane -- which is how vanilla itself draws a "+" cross
// as an "X", and how several modded plants are authored too -- still passes
// this check exactly like an unrotated one. That also means vanilla's chain,
// which happens to reach the same "X" shape by rotating two perpendicular
// planes the same way, is geometrically indistinguishable from a flower
// cross and is misclassified as a decoration; no reliable field in the
// model format tells the two apart, so this is accepted as a narrow,
// low-impact exception rather than a case worth adding a name-based
// carve-out for.)
func (e modelElement) isBillboardPlane() bool {
	thinX := e.From[0] == e.To[0]
	thinZ := e.From[2] == e.To[2]
	if thinX == thinZ {
		return false // a solid box (neither thin) or a line (both thin)
	}
	if e.To[1]-e.From[1] < 14 {
		return false // not (close to) full block height
	}
	return e.Faces.Up == nil && e.Faces.Down == nil
}

// crossFace extracts the single texture shared by both crossed planes of a
// cross-shaped foliage model. Every vanilla cross model applies the same
// "#cross" variable to all four side faces of its two elements, so the
// first non-empty side face found is representative of the whole plant.
func crossFace(elements []modelElement, vars map[string]string) faceRef {
	for _, el := range elements {
		if f := firstNonEmptySide(el.Faces, vars); f.Texture != "" {
			return f
		}
	}
	return faceRef{}
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// splitRef splits a "namespace:path" reference, defaulting to the "minecraft"
// namespace when none is given, which is what an unqualified reference like
// "block/cube_all" or "stone" means throughout Minecraft's asset format.
func splitRef(ref string) (namespace, path string, ok bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", false
	}
	if i := strings.IndexByte(ref, ':'); i >= 0 {
		return ref[:i], ref[i+1:], true
	}
	return "minecraft", ref, true
}

// readJSON reads and decodes one asset path, reporting ok=false for a
// missing file or invalid JSON -- both are treated identically by callers,
// since either means this block cannot use the simplified resolver.
func readJSON[T any](store *Store, path string) (T, bool) {
	var out T
	data, ok := store.Read(path)
	if !ok {
		return out, false
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, false
	}
	return out, true
}
