// Package blocks maps Minecraft block and biome identifiers to render
// attributes. Nothing in the renderer hard-codes a block colour; every colour
// comes from data files loaded here, so a modpack with thousands of custom
// blocks is a configuration change rather than a code change.
//
// Unknown identifiers never fail. They receive a deterministic fallback colour
// derived from the identifier itself, and are recorded so operators can see
// exactly which blocks a modpack needs mappings for.
package blocks

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"image/color"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Tint selects the biome-dependent colouring applied to a block.
type Tint uint8

const (
	// TintNone leaves the block's configured colour untouched.
	TintNone Tint = iota
	// TintGrass multiplies by the biome's grass colour.
	TintGrass
	// TintFoliage multiplies by the biome's foliage colour (leaves, vines).
	TintFoliage
	// TintWater multiplies by the biome's water colour.
	TintWater
	// TintDryFoliage is used by biome-independent dry plants that still vary.
	TintDryFoliage
)

func parseTint(s string) Tint {
	switch strings.ToLower(s) {
	case "grass":
		return TintGrass
	case "foliage", "leaves":
		return TintFoliage
	case "water":
		return TintWater
	case "dry", "dry_foliage":
		return TintDryFoliage
	default:
		return TintNone
	}
}

// Block holds everything the renderers need to know about one block type.
type Block struct {
	// ID is the registry-local index, used instead of the string in hot paths
	// and in the compact per-column surface arrays.
	ID uint16
	// Name is the full namespaced identifier, e.g. "minecraft:grass_block".
	Name string
	// Color is the representative colour used when no texture is available.
	Color color.NRGBA
	// Tint selects biome-dependent recolouring.
	Tint Tint
	// Water marks fluids that the surface scanner should see through, so the
	// sea floor is known and water can be shaded by depth.
	Water bool
	// Transparent marks blocks the surface scanner should skip entirely (air,
	// barriers, structure voids). Glass and leaves are NOT transparent for this
	// purpose: they are genuinely the visible surface from above.
	Transparent bool
	// Height is the block's rendered top height in blocks, letting slabs and
	// stairs sit at a half step. 1.0 for full blocks.
	Height float32
	// Grassy marks blocks whose top surface should receive the grass-like
	// speckle in the terrain style. Purely cosmetic.
	Grassy bool
	// Decoration marks a thin plant (grass, fern, flower, sapling, crop) that
	// sits on top of another block rather than filling its own column. The
	// surface scanner skips it like any other Transparent block when finding
	// the solid ground beneath, but -- unlike air or glass -- also remembers
	// it, so the renderer can paint its texture as a decal over the surface
	// block's top face instead of either discarding it or, worse, treating a
	// mostly see-through cross-shaped sprite as if it were an opaque cube.
	Decoration bool
	// Occludes reports whether this block fully hides what is behind it in
	// the isometric voxel renderer (ISO_VOXEL_PLAN.md §4.3). It is derived,
	// never loaded from blocks.json: putLocked recomputes it from
	// Transparent/Water/Decoration on every insert, so it can never drift
	// out of sync with them. False for air, water, and decorations -- the
	// same things that ChunkSurface's own top-down scan already sees through.
	Occludes bool
	// Known is false for identifiers that fell back to a generated colour.
	Known bool
}

// Registry is a concurrent, append-only block table. Lookups take a read lock;
// only first sightings of an unknown block take the write lock, so steady-state
// tile rendering is effectively lock-free on the read path.
type Registry struct {
	mu                 sync.RWMutex
	byName             map[string]uint16
	blocks             []Block
	unknown            map[string]int
	fallback           FallbackFunc
	classifyDecoration DecorationClassifier
}

// FallbackFunc produces a colour for an identifier that has no mapping.
type FallbackFunc func(name string) color.NRGBA

// DecorationClassifier reports whether an identifier outside blocks.json is
// a thin plant that should be treated as sitting on top of whatever is
// beneath it (see Block.Decoration), rather than as an opaque cube. A
// modpack can add hundreds of custom plants; naming each one in blocks.json
// does not scale, so SetDecorationClassifier lets a caller with access to
// the real block model data (see textures.ClassifyDecoration) answer this
// automatically for anything blocks.json does not already cover explicitly.
type DecorationClassifier func(name string) bool

// SetDecorationClassifier installs fn, consulted only the first time an
// identifier outside blocks.json is seen; entries loaded explicitly from
// blocks.json are never second-guessed. Passing nil disables it, which is
// also the zero-value default: without it, an unmapped block is simply an
// opaque cube, exactly as before this classifier existed.
func (r *Registry) SetDecorationClassifier(fn DecorationClassifier) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.classifyDecoration = fn
}

// AirID is guaranteed to be registry index 0 in every Registry.
const AirID uint16 = 0

// NewRegistry returns a registry containing only air.
func NewRegistry() *Registry {
	r := &Registry{
		byName:   make(map[string]uint16, 4096),
		unknown:  make(map[string]int),
		fallback: DeterministicColor,
	}
	// Air must be index 0 so a zeroed surface array reads as empty.
	// Occludes: false falls straight out of the same derivation putLocked
	// applies elsewhere (Transparent implies not Occludes); air is
	// constructed directly rather than via putLocked only because it must
	// land at index 0 before any write-lock machinery exists to enforce it.
	r.blocks = append(r.blocks, Block{
		ID: AirID, Name: "minecraft:air", Transparent: true, Height: 1, Known: true, Occludes: false,
	})
	r.byName["minecraft:air"] = AirID
	return r
}

// blockJSON is the on-disk shape of one entry in blocks.json.
type blockJSON struct {
	Color       string   `json:"color"`
	Tint        string   `json:"tint"`
	Water       bool     `json:"water"`
	Transparent bool     `json:"transparent"`
	Height      *float32 `json:"height"`
	Grassy      bool     `json:"grassy"`
	Decoration  bool     `json:"decoration"`
	Alias       string   `json:"alias"`
}

// LoadBlocksFile merges a blocks.json mapping into the registry. Later files
// override earlier ones, so a modpack overlay can be layered on the vanilla
// base without editing it.
func (r *Registry) LoadBlocksFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read block colours: %w", err)
	}
	return r.LoadBlocksJSON(raw)
}

// LoadBlocksJSON merges a blocks.json document from memory.
func (r *Registry) LoadBlocksJSON(raw []byte) (int, error) {
	// Accept either a bare map or an object with a "blocks" key, so the file can
	// carry metadata later without breaking older configs.
	var wrapper struct {
		Blocks map[string]blockJSON `json:"blocks"`
	}
	entries := map[string]blockJSON{}
	if err := json.Unmarshal(raw, &wrapper); err == nil && len(wrapper.Blocks) > 0 {
		entries = wrapper.Blocks
	} else if err := json.Unmarshal(raw, &entries); err != nil {
		return 0, fmt.Errorf("parse block colours: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Resolve aliases after the direct entries so order in the file is free.
	aliases := map[string]string{}
	n := 0
	for name, e := range entries {
		if e.Alias != "" {
			aliases[normalize(name)] = normalize(e.Alias)
			continue
		}
		col, err := ParseHexColor(e.Color)
		if err != nil {
			return n, fmt.Errorf("block %q: %w", name, err)
		}
		// An explicit height always wins; otherwise the name is consulted, so
		// a listed-but-heightless "spruce_stairs" is still half a block
		// rather than silently a full cube.
		h := float32(1)
		if e.Height != nil {
			h = *e.Height
		} else if inferred, ok := InferHeight(name); ok {
			h = inferred
		}
		b := Block{
			Name:        normalize(name),
			Color:       col,
			Tint:        parseTint(e.Tint),
			Water:       e.Water,
			Transparent: e.Transparent,
			Height:      h,
			Grassy:      e.Grassy,
			Decoration:  e.Decoration,
			Known:       true,
		}
		r.putLocked(b)
		n++
	}
	for from, to := range aliases {
		if id, ok := r.byName[to]; ok {
			target := r.blocks[id]
			target.Name = from
			r.putLocked(target)
			n++
		}
	}
	return n, nil
}

// putLocked inserts or replaces a block. The caller must hold the write lock.
func (r *Registry) putLocked(b Block) uint16 {
	// Occludes is fully derived and must never be settable by a caller
	// (blocks.json has no "occludes" key -- see the field doc), so it is
	// recomputed here unconditionally rather than trusted from b.
	b.Occludes = !b.Transparent && !b.Water && !b.Decoration
	if id, ok := r.byName[b.Name]; ok {
		b.ID = id
		r.blocks[id] = b
		return id
	}
	b.ID = uint16(len(r.blocks))
	r.blocks = append(r.blocks, b)
	r.byName[b.Name] = b.ID
	return b.ID
}

// ID resolves an identifier to its registry index, registering an unknown block
// with a deterministic fallback colour if necessary. It never fails, because a
// single unmapped modded block must not be able to abort a tile render.
func (r *Registry) ID(name string) uint16 {
	key := normalize(name)

	r.mu.RLock()
	id, ok := r.byName[key]
	r.mu.RUnlock()
	if ok {
		return id
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-check: another goroutine may have registered it while we waited.
	if id, ok := r.byName[key]; ok {
		r.unknown[key]++
		return id
	}
	r.unknown[key]++
	b := Block{
		Name:   key,
		Color:  r.fallback(key),
		Height: 1,
		Known:  false,
	}
	// A modpack adds hundreds of stair, slab and carpet variants that
	// blocks.json will never list one by one; their names still say what
	// shape they are (see InferHeight).
	if h, ok := InferHeight(key); ok {
		b.Height = h
	}
	if r.classifyDecoration != nil && r.classifyDecoration(key) {
		b.Transparent = true
		b.Decoration = true
	}
	return r.putLocked(b)
}

// DowngradeOccludes clears a block's Occludes flag for a reason
// Transparent/Water/Decoration cannot capture: its real resolved texture has
// transparent texels (ISO_VOXEL_PLAN.md §5 Phase 4 -- a leaf canopy has gaps
// even though blocks.json deliberately marks leaves non-transparent for
// top-down surface purposes, see HANDOFF.md). It only ever moves Occludes
// true -> false, never the reverse: putLocked remains the sole source of
// truth for why a block would occlude at all, this only ever weakens that
// answer once real texture data disagrees with it. A no-op for an
// out-of-range id.
func (r *Registry) DowngradeOccludes(id uint16) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if int(id) >= len(r.blocks) {
		return
	}
	r.blocks[id].Occludes = false
}

// Get returns the block for a registry index. Out-of-range indices return air
// rather than panicking, so a corrupt cache cannot crash a render.
func (r *Registry) Get(id uint16) Block {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if int(id) >= len(r.blocks) {
		return r.blocks[AirID]
	}
	return r.blocks[id]
}

// Lookup resolves an identifier without registering it.
func (r *Registry) Lookup(name string) (Block, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byName[normalize(name)]
	if !ok {
		return Block{}, false
	}
	return r.blocks[id], true
}

// Len returns the number of registered blocks.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.blocks)
}

// UnknownReport lists identifiers that fell back to a generated colour, most
// frequent first. This is what an operator uses to extend blocks.json for a
// modpack.
type UnknownReport struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Color string `json:"color"`
}

// Unknown returns the unknown-block report, capped at limit entries (0 = all).
func (r *Registry) Unknown(limit int) []UnknownReport {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]UnknownReport, 0, len(r.unknown))
	for name, count := range r.unknown {
		id, ok := r.byName[name]
		if !ok || r.blocks[id].Known {
			continue
		}
		out = append(out, UnknownReport{
			Name: name, Count: count, Color: HexOf(r.blocks[id].Color),
		})
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

// normalize applies Minecraft's default namespace and lowercases the id, and
// strips any block-state suffix such as "[facing=north]" so that block states
// share one colour entry unless explicitly mapped.
func normalize(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	if i := strings.IndexByte(s, '['); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "minecraft:air"
	}
	if !strings.Contains(s, ":") {
		s = "minecraft:" + s
	}
	return s
}

// Normalize exposes identifier normalisation for callers that key their own
// maps by block name.
func Normalize(name string) string { return normalize(name) }

// ---------------------------------------------------------------------------
// Colour helpers
// ---------------------------------------------------------------------------

// ParseHexColor accepts #rgb, #rrggbb and #rrggbbaa, with or without the hash.
func ParseHexColor(s string) (color.NRGBA, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	if s == "" {
		return color.NRGBA{}, fmt.Errorf("empty colour")
	}
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 && len(s) != 8 {
		return color.NRGBA{}, fmt.Errorf("colour %q must be 3, 6 or 8 hex digits", s)
	}
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return color.NRGBA{}, fmt.Errorf("colour %q is not hexadecimal", s)
	}
	if len(s) == 6 {
		return color.NRGBA{
			R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 255,
		}, nil
	}
	return color.NRGBA{
		R: uint8(v >> 24), G: uint8(v >> 16), B: uint8(v >> 8), A: uint8(v),
	}, nil
}

// HexOf formats a colour as #rrggbb.
func HexOf(c color.NRGBA) string {
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}

// DeterministicColor derives a stable, muted colour from an identifier.
//
// The same unknown modded block therefore renders identically on every server,
// every restart and every zoom level, which keeps the pyramid self-consistent
// and makes unmapped terrain look deliberately drab rather than randomly
// psychedelic. Saturation and value are kept in a narrow band so unmapped
// blocks never out-shout mapped terrain.
func DeterministicColor(name string) color.NRGBA {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	sum := h.Sum64()

	hue := float64(sum%360) / 360.0
	sat := 0.18 + float64((sum>>16)%22)/100.0 // 0.18 .. 0.40
	val := 0.42 + float64((sum>>32)%26)/100.0 // 0.42 .. 0.68
	r, g, b := hsvToRGB(hue, sat, val)
	return color.NRGBA{R: r, G: g, B: b, A: 255}
}

func hsvToRGB(h, s, v float64) (uint8, uint8, uint8) {
	i := math.Floor(h * 6)
	f := h*6 - i
	p := v * (1 - s)
	q := v * (1 - f*s)
	t := v * (1 - (1-f)*s)
	var r, g, b float64
	switch int(i) % 6 {
	case 0:
		r, g, b = v, t, p
	case 1:
		r, g, b = q, v, p
	case 2:
		r, g, b = p, v, t
	case 3:
		r, g, b = p, q, v
	case 4:
		r, g, b = t, p, v
	default:
		r, g, b = v, p, q
	}
	return uint8(r*255 + 0.5), uint8(g*255 + 0.5), uint8(b*255 + 0.5)
}

// Multiply applies a tint colour to a base colour, as Minecraft does for grass,
// foliage and water.
func Multiply(base, tint color.NRGBA) color.NRGBA {
	return color.NRGBA{
		R: uint8(int(base.R) * int(tint.R) / 255),
		G: uint8(int(base.G) * int(tint.G) / 255),
		B: uint8(int(base.B) * int(tint.B) / 255),
		A: base.A,
	}
}

// Lerp blends two colours, t in [0,1].
func Lerp(a, b color.NRGBA, t float64) color.NRGBA {
	if t <= 0 {
		return a
	}
	if t >= 1 {
		return b
	}
	return color.NRGBA{
		R: uint8(float64(a.R) + (float64(b.R)-float64(a.R))*t + 0.5),
		G: uint8(float64(a.G) + (float64(b.G)-float64(a.G))*t + 0.5),
		B: uint8(float64(a.B) + (float64(b.B)-float64(a.B))*t + 0.5),
		A: uint8(float64(a.A) + (float64(b.A)-float64(a.A))*t + 0.5),
	}
}

// Scale multiplies a colour's brightness by f, clamping to the 0..255 range.
// This is the primitive behind height shading and isometric face shading.
func Scale(c color.NRGBA, f float64) color.NRGBA {
	return color.NRGBA{
		R: clamp8(float64(c.R) * f),
		G: clamp8(float64(c.G) * f),
		B: clamp8(float64(c.B) * f),
		A: c.A,
	}
}

func clamp8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}
