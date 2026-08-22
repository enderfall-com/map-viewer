// Package render turns world surfaces into raster tiles.
//
// Both the top-down and the isometric renderer resolve column colours through
// the same Shader, so a block looks the same in both modes and switching views
// never changes the palette. Only the geometry differs.
package render

import (
	"image/color"
	"math"

	"github.com/enderfall/minecraft-map/backend/internal/blocks"
	"github.com/enderfall/minecraft-map/backend/internal/textures"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

// Style selects what the terrain colours mean.
type Style string

const (
	// StyleTerrain paints blocks in their representative colours with biome
	// tinting, water depth and relief shading. The default.
	StyleTerrain Style = "terrain"
	// StyleBiome paints a flat colour per biome, for reading biome layout.
	StyleBiome Style = "biome"
	// StyleHeight paints an elevation gradient, for reading terrain relief.
	StyleHeight Style = "height"
	// StyleLight paints a mob-spawn safety map from block light alone: where
	// the world is dark enough for hostile mobs to spawn after dusk.
	StyleLight Style = "light"
	// StyleOre paints each column by the most valuable ore anywhere beneath
	// it, for deciding where to dig.
	StyleOre Style = "ore"
	// StyleContour paints transparent tiles with elevation contour lines only,
	// for use as a client-side raster overlay.
	StyleContour Style = "contour"
)

// ParseStyle resolves a style name, falling back to terrain.
func ParseStyle(s string) (Style, bool) {
	switch Style(s) {
	case StyleTerrain, StyleBiome, StyleHeight, StyleLight, StyleOre, StyleContour:
		return Style(s), true
	case "":
		return StyleTerrain, true
	}
	return StyleTerrain, false
}

// Spawn-safety thresholds for StyleLight.
//
// Modern Minecraft (1.18+) spawns hostile mobs only at block light 0, so that
// is the one threshold that genuinely matters and it gets the alarming
// colour. Levels 1..7 are drawn as a caution band rather than lumped in with
// "safe": they are safe now, but they were spawnable before 1.18 and they are
// one removed torch away from being spawnable again, which is exactly what
// someone lighting up a base wants to see.
const (
	spawnableLight = 0
	cautionLight   = 7
)

// oreColors is each OreKind's map colour, chosen to match the ore's own
// in-game appearance so the legend is guessable without one.
var oreColors = [...]color.NRGBA{
	world.OreNone:     {0x1b, 0x1e, 0x24, 0xff}, // near-background: nothing here
	world.OreOther:    {0xa8, 0x8c, 0xc0, 0xff}, // unknown/modded: neutral violet
	world.OreCoal:     {0x3d, 0x43, 0x4d, 0xff},
	world.OreCopper:   {0xc8, 0x7b, 0x4a, 0xff},
	world.OreIron:     {0xd8, 0xb0, 0x8c, 0xff},
	world.OreLapis:    {0x2c, 0x5a, 0xc4, 0xff},
	world.OreRedstone: {0xd0, 0x2a, 0x2a, 0xff},
	world.OreGold:     {0xf0, 0xc2, 0x3a, 0xff},
	world.OreEmerald:  {0x2c, 0xd4, 0x6a, 0xff},
	world.OreDiamond:  {0x4d, 0xe0, 0xdc, 0xff},
	world.OreDebris:   {0x7a, 0x4a, 0x3c, 0xff},
}

// oreScoreSaturates is the column score that paints an ore at full strength.
// One diamond block scores 12, so a single diamond already reads clearly and
// a proper vein saturates -- which is the intent, since the map exists to
// find the first one, not to grade the size of the deposit.
const oreScoreSaturates = 16.0

// oreStyleColor paints a column's ore summary over a desaturated rendering of
// the terrain itself.
//
// Keeping the terrain visible underneath is deliberate: an abstract field of
// ore colours is impossible to navigate, because nothing in it says which
// coastline or which base you are looking at. Draining the terrain of colour
// leaves the shape of the world legible as context while surrendering the
// entire colour channel to the ore on top.
//
// Intensity comes from the rarity-weighted score rather than from which ore
// is present, so the common ores fade out entirely (they score zero) instead
// of colouring every pixel; the hue is still the best ore in the column, so a
// bright spot also says what it is.
func (s *Shader) oreStyleColor(surf *world.Surface, c world.Column) color.NRGBA {
	base := desaturate(s.terrainColor(surf, c), 0.88)
	if c.OreScore == 0 {
		return base
	}
	best := c.OreBest
	if int(best) >= len(oreColors) {
		best = world.OreOther
	}
	w := math.Min(1, float64(c.OreScore)/oreScoreSaturates)
	return blocks.Lerp(base, oreColors[best], w)
}

// desaturate pulls a colour toward its own luminance. amount 0 leaves it
// untouched, 1 makes it fully grey.
func desaturate(col color.NRGBA, amount float64) color.NRGBA {
	// Rec. 601 luma: matches how the eye weights the channels, so terrain
	// keeps its relative light and dark rather than flattening out.
	y := 0.299*float64(col.R) + 0.587*float64(col.G) + 0.114*float64(col.B)
	grey := color.NRGBA{uint8(y + 0.5), uint8(y + 0.5), uint8(y + 0.5), col.A}
	return blocks.Lerp(col, grey, amount)
}

// lightStyleColor maps a column's block light to its spawn-safety colour.
func lightStyleColor(blockLight uint8) color.NRGBA {
	switch {
	case blockLight <= spawnableLight:
		return color.NRGBA{0xe0, 0x3b, 0x3b, 0xff} // red: mobs spawn here
	case blockLight <= cautionLight:
		// Ramp amber -> yellow across the caution band so the edge of a
		// torch's reach is legible rather than one flat colour.
		t := float64(blockLight-spawnableLight-1) / float64(cautionLight-spawnableLight-1)
		return blocks.Lerp(color.NRGBA{0xe8, 0x8a, 0x2a, 0xff}, color.NRGBA{0xe8, 0xd0, 0x44, 0xff}, t)
	default:
		// Comfortably lit: a calm green that darkens slightly toward the
		// threshold, so "just barely safe" still reads differently from
		// "brightly lit".
		t := float64(blockLight-cautionLight) / float64(15-cautionLight)
		return blocks.Lerp(color.NRGBA{0x2f, 0x6d, 0x4a, 0xff}, color.NRGBA{0x54, 0xc9, 0x82, 0xff}, t)
	}
}

// Options tunes the visual treatment. Zero values are not useful; use
// DefaultOptions and adjust.
type Options struct {
	// HeightShading enables relief shading from neighbour elevation deltas.
	HeightShading bool
	// ShadingStrength scales the relief effect. Around 0.08 reads clearly
	// without turning terrain into a noisy mess.
	ShadingStrength float64
	// ShadingClamp caps how many blocks of elevation delta contribute, so a
	// cliff and a mountainside do not saturate to pure white and black.
	ShadingClamp float64
	// AmbientHeight adds a gentle absolute-elevation brightness ramp so high
	// ground reads as high even on flat plateaus.
	AmbientHeight float64
	// BiomeTint enables biome-dependent grass, foliage and water colouring.
	BiomeTint bool
	// WaterDepthShading darkens water by depth so coastlines and drop-offs are
	// legible.
	WaterDepthShading bool
	// MaxWaterDepth is the depth at which water reaches full darkness.
	MaxWaterDepth float64
	// LightShading modulates by stored block light, revealing lit interiors and
	// caves at night. Worlds without light data are unaffected.
	LightShading bool
	// UnexploredColor fills columns with no world data.
	UnexploredColor color.NRGBA
	// Contour enables StyleContour output.
	Contour bool
	// ContourInterval is the world-Y spacing between minor contour lines.
	ContourInterval int
	// ContourMajorEvery makes every Nth minor contour an index line.
	ContourMajorEvery int
	// ContourColor is the minor contour line colour.
	ContourColor color.NRGBA
	// ContourMajorColor is the index contour line colour.
	ContourMajorColor color.NRGBA
	// HeightGradient defines the StyleHeight ramp from low to high.
	HeightGradient []color.NRGBA
}

// DefaultOptions returns a readable treatment with pronounced relief
// shading -- strong enough that individual tree canopies and hillside
// contours read as real depth in top-down mode, not just a faint tint.
func DefaultOptions() Options {
	return Options{
		HeightShading:     true,
		ShadingStrength:   0.35,
		ShadingClamp:      3,
		AmbientHeight:     0.10,
		BiomeTint:         true,
		WaterDepthShading: true,
		MaxWaterDepth:     28,
		LightShading:      false,
		UnexploredColor:   color.NRGBA{0x14, 0x16, 0x1a, 0xff},
		Contour:           false,
		ContourInterval:   8,
		ContourMajorEvery: 4,
		ContourColor:      color.NRGBA{0x4a, 0x33, 0x20, 0x9b},
		ContourMajorColor: color.NRGBA{0x6b, 0x48, 0x28, 0xe6},
		HeightGradient: []color.NRGBA{
			{0x0b, 0x1b, 0x3a, 0xff}, // deep
			{0x1c, 0x4c, 0x6b, 0xff},
			{0x2e, 0x7d, 0x63, 0xff},
			{0x74, 0xa5, 0x44, 0xff},
			{0xc7, 0xb8, 0x56, 0xff},
			{0x9c, 0x6f, 0x45, 0xff},
			{0xf2, 0xf4, 0xf7, 0xff}, // peaks
		},
	}
}

// Shader resolves a world column to a pixel colour.
//
// It is stateless apart from its configuration and is safe for concurrent use
// by the whole tile worker pool.
type Shader struct {
	Blocks *blocks.Registry
	Biomes *blocks.Biomes
	Style  Style
	Opts   Options

	// Textures resolves real block textures. Nil (the default) means every
	// column renders with its flat blocks.json colour, exactly as if
	// texture support did not exist -- textures are purely additive.
	Textures *textures.Set

	// HeightLo and HeightHi bound the StyleHeight gradient. When equal, the
	// shader falls back to the dimension's own build range.
	HeightLo, HeightHi int
}

// NewShader builds a shader with the default options.
func NewShader(reg *blocks.Registry, bio *blocks.Biomes, style Style) *Shader {
	return &Shader{Blocks: reg, Biomes: bio, Style: style, Opts: DefaultOptions()}
}

// ColumnColor returns the colour for one column, and whether the column exists.
// Absent columns return false and must be drawn as unexplored, not as black.
func (s *Shader) ColumnColor(surf *world.Surface, x, z int) (color.NRGBA, bool) {
	c := surf.At(x, z)
	if s.Style == StyleContour {
		return s.contourColor(surf, x, z, c)
	}
	if !c.Present() {
		return s.Opts.UnexploredColor, false
	}
	base := s.topColor(surf, c)
	if s.Opts.HeightShading {
		base = blocks.Scale(base, s.reliefFactor(surf, x, z, c))
	}
	if s.Opts.LightShading {
		base = blocks.Scale(base, lightFactor(c.Light))
	}
	return base, true
}

// baseColor resolves the unshaded colour for a column under the active style,
// for the surface Block itself -- never any Decoration sitting on top of it.
// Callers that want the colour actually visible looking straight down, which
// includes any decoration, use topColor instead.
func (s *Shader) baseColor(surf *world.Surface, c world.Column) color.NRGBA {
	switch s.Style {
	case StyleBiome:
		return s.Biomes.Get(c.Biome).Overlay

	case StyleHeight:
		lo, hi := s.HeightLo, s.HeightHi
		if hi <= lo {
			lo, hi = surf.MinY, surf.MaxY
		}
		t := float64(c.RenderY()-lo) / float64(max(1, hi-lo))
		return sampleGradient(s.Opts.HeightGradient, t)

	case StyleLight:
		return lightStyleColor(c.BlockLight)

	case StyleOre:
		return s.oreStyleColor(surf, c)

	default: // StyleTerrain
		return s.terrainColor(surf, c)
	}
}

// terrainColor is the natural-colour rendering of a column, independent of
// the active style. Split out from baseColor's terrain branch because the ore
// style draws over a desaturated copy of it, and would otherwise recurse
// through baseColor back into itself.
func (s *Shader) terrainColor(_ *world.Surface, c world.Column) color.NRGBA {
	col := s.blockColor(c.Block, c, 1, 0, 0)
	if c.Water() {
		col = s.waterColor(col, c, s.Biomes.Get(c.Biome))
	}
	return col
}

// topColor is baseColor with any Decoration (grass, flower, sapling, crop)
// alpha-composited over it. This is the colour actually visible looking
// straight down at the column, as opposed to an isometric side skirt, which
// always shows the surface Block alone -- a plant sitting on the ground does
// not paint the dirt one block down.
func (s *Shader) topColor(surf *world.Surface, c world.Column) color.NRGBA {
	col := s.baseColor(surf, c)
	if c.Decoration != 0 {
		col = alphaOver(s.blockColor(c.Decoration, c, 1, 0, 0), col)
	}
	return col
}

// blockColor resolves one block identifier's own colour at (tu,tv), real
// texture if resolved, flat blocks.json colour otherwise. It never looks at
// c.Block or c.Decoration itself -- id is passed explicitly so the same logic
// serves both the column's surface block and a decoration sitting on it.
func (s *Shader) blockColor(id uint16, c world.Column, mipSize, tu, tv int) color.NRGBA {
	if faces, ok := s.textureFacesFor(id); ok && faces.Top.Base != nil {
		return s.faceColor(faces.Top, id, c, mipSize, tu, tv)
	}
	blk := s.Blocks.Get(id)
	col := blk.Color
	if s.Opts.BiomeTint {
		col = blocks.ApplyTint(col, blk.Tint, s.Biomes.Get(c.Biome))
	}
	return col
}

// ---------------------------------------------------------------------------
// Per-texel sampling
// ---------------------------------------------------------------------------

// FaceKind selects which face of a block a texel is sampled from.
type FaceKind int

const (
	// FaceKindTop is the upward-facing texture (grass_block_top, oak_log_top).
	FaceKindTop FaceKind = iota
	// FaceKindSide is the representative side texture (grass_block_side,
	// the oak_log bark texture), used for the isometric side skirt.
	FaceKindSide
)

// FaceSampler returns a function that yields the colour of one texel of a
// column's face, and whether the column is present at all (false only
// mirrors ColumnColor's absent-column case; the returned sample function is
// nil then, since there is nothing to sample).
//
// Shading (relief, light) is resolved once here, not inside the returned
// closure, and reused for however many texels the caller goes on to sample.
// That is what keeps this exactly as cheap as the old one-colour-per-block
// renderer for any block with no resolved texture -- every block, for a
// deployment with no texture source configured at all -- instead of
// recomputing the same neighbour-height lookups and square roots for every
// one of a diamond's 100+ pixels or a magnified block's 256 texels only to
// discard the (tu,tv) they never needed.
func (s *Shader) FaceSampler(surf *world.Surface, x, z int, face FaceKind, mipSize int) (sample func(tu, tv int) color.NRGBA, present bool) {
	transparent := func(_, _ int) color.NRGBA { return color.NRGBA{} }
	c := surf.At(x, z)
	if s.Style == StyleContour {
		if !c.Present() {
			return transparent, false
		}
		if face != FaceKindTop {
			return transparent, true
		}
		col, ok := s.contourColor(surf, x, z, c)
		if !ok {
			return transparent, true
		}
		return func(_, _ int) color.NRGBA { return col }, true
	}
	if !c.Present() {
		u := s.Opts.UnexploredColor
		return func(_, _ int) color.NRGBA { return u }, false
	}

	relief := 1.0
	if s.Opts.HeightShading {
		relief = s.reliefFactor(surf, x, z, c)
	}
	light := 1.0
	if s.Opts.LightShading {
		light = lightFactor(c.Light)
	}
	shade := func(col color.NRGBA) color.NRGBA {
		if s.Opts.HeightShading {
			col = blocks.Scale(col, relief)
		}
		if s.Opts.LightShading {
			col = blocks.Scale(col, light)
		}
		return col
	}

	// A decoration only ever paints the top face -- a plant does not wrap
	// around the side skirt of the block it stands on.
	hasDecor := c.Decoration != 0 && face == FaceKindTop

	if !s.HasTexture(c.Block) {
		var flat color.NRGBA
		if hasDecor {
			flat = shade(s.topColor(surf, c))
		} else {
			flat = shade(s.baseColor(surf, c))
		}
		return func(_, _ int) color.NRGBA { return flat }, true
	}
	return func(tu, tv int) color.NRGBA {
		base, ok := s.texelBase(c, face, mipSize, tu, tv)
		if !ok {
			base = s.baseColor(surf, c)
		}
		if hasDecor {
			base = alphaOver(s.blockColor(c.Decoration, c, mipSize, tu, tv), base)
		}
		return shade(base)
	}, true
}

// VoxelSampler returns a per-texel colour function for one face of one real
// voxel -- a specific (x,y,z), not a column's single flattened surface block
// -- for the isometric voxel renderer (ISO_VOXEL_PLAN.md §4.4).
//
// isColumnTop must be true only when y is the column's own topmost drawn
// voxel (Volume.TopY, or one below it if that top voxel is a decoration):
// relief shading is a broad neighbour-height comparison belonging to the
// terrain silhouette, and applying it to every exposed top face down a tall
// column (a mid-air ledge, a cave ceiling) would double-count the voxel
// renderer's own per-face shading and look muddy. It is only ever applied
// to FaceKindTop, matching FaceSampler.
//
// Biome and water depth are still read from the flattened Surface at (x,z)
// (surf.At), not derived per voxel: biomes barely vary vertically, and the
// existing depth-shaded water look is defined in terms of the column's own
// WaterDepth, not any one voxel's.
func (s *Shader) VoxelSampler(
	surf *world.Surface, x, z, y int,
	blockID uint16, light uint8,
	face FaceKind, isColumnTop bool, mipSize int,
) func(tu, tv int) color.NRGBA {
	if s.Style == StyleContour {
		if face != FaceKindTop || !isColumnTop {
			return func(_, _ int) color.NRGBA { return color.NRGBA{} }
		}
		col, ok := s.contourColor(surf, x, z, surf.At(x, z))
		if !ok {
			return func(_, _ int) color.NRGBA { return color.NRGBA{} }
		}
		return func(_, _ int) color.NRGBA { return col }
	}

	applyRelief := s.Opts.HeightShading && face == FaceKindTop && isColumnTop
	relief := 1.0
	if applyRelief {
		relief = s.reliefFactor(surf, x, z, surf.At(x, z))
	}
	light01 := 1.0
	if s.Opts.LightShading {
		light01 = lightFactor(light)
	}
	shade := func(col color.NRGBA) color.NRGBA {
		if applyRelief {
			col = blocks.Scale(col, relief)
		}
		if s.Opts.LightShading {
			col = blocks.Scale(col, light01)
		}
		return col
	}

	if !s.HasTexture(blockID) {
		flat := shade(s.voxelBaseColor(surf, x, z, blockID))
		return func(_, _ int) color.NRGBA { return flat }
	}
	return func(tu, tv int) color.NRGBA {
		base, ok := s.voxelTexelBase(surf, x, z, blockID, face, mipSize, tu, tv)
		if !ok {
			base = s.voxelBaseColor(surf, x, z, blockID)
		}
		return shade(base)
	}
}

// voxelColumn builds a synthetic Column carrying one voxel's own block id and
// light level but the real surface column's biome, so blockColor/faceColor
// (which take a Column only to read c.Biome) resolve the same tint a
// flattened-surface lookup would.
func (s *Shader) voxelColumn(surf *world.Surface, x, z int, blockID uint16, light uint8) world.Column {
	real := surf.At(x, z)
	return world.Column{Block: blockID, Biome: real.Biome, Light: light, Flags: world.FlagPresent}
}

// voxelBaseColor is blockColor for a specific voxel, with water compositing
// applied using the column's own WaterDepth (see VoxelSampler's doc comment).
func (s *Shader) voxelBaseColor(surf *world.Surface, x, z int, blockID uint16) color.NRGBA {
	// Every style except terrain describes the *column* (its biome, its
	// elevation, whether mobs can spawn on it), not the individual block, so
	// it resolves identically for every voxel in the column -- and must, or
	// the isometric view would disagree with top-down about what a style
	// means, which is exactly what routing both renderers through one Shader
	// exists to prevent.
	if s.Style != StyleTerrain {
		return s.baseColor(surf, surf.At(x, z))
	}
	vc := s.voxelColumn(surf, x, z, blockID, 0)
	col := s.blockColor(blockID, vc, 1, 0, 0)
	if s.Blocks.Get(blockID).Water {
		col = s.waterColor(col, surf.At(x, z), s.Biomes.Get(vc.Biome))
	}
	return col
}

// voxelTexelBase is texelBase for a specific voxel rather than a column's
// flattened surface block.
func (s *Shader) voxelTexelBase(surf *world.Surface, x, z int, blockID uint16, face FaceKind, mipSize, tu, tv int) (color.NRGBA, bool) {
	if s.Style != StyleTerrain {
		return color.NRGBA{}, false
	}
	faces, ok := s.textureFacesFor(blockID)
	if !ok {
		return color.NRGBA{}, false
	}
	ft := faces.Top
	if face == FaceKindSide {
		ft = faces.Side
	}
	if ft.Base == nil {
		return color.NRGBA{}, false
	}
	vc := s.voxelColumn(surf, x, z, blockID, 0)
	col := s.faceColor(ft, blockID, vc, mipSize, tu, tv)
	if s.Blocks.Get(blockID).Water {
		col = s.waterColor(col, surf.At(x, z), s.Biomes.Get(vc.Biome))
	}
	return col, true
}

// texelBase resolves one texel's unshaded colour from a real texture, or
// reports ok=false when the style, a missing texture set, or an unresolved
// block means the caller should fall back to the flat baseColor.
func (s *Shader) texelBase(c world.Column, face FaceKind, mipSize, tu, tv int) (color.NRGBA, bool) {
	if s.Style != StyleTerrain {
		return color.NRGBA{}, false
	}
	faces, ok := s.textureFacesFor(c.Block)
	if !ok {
		return color.NRGBA{}, false
	}
	ft := faces.Top
	if face == FaceKindSide {
		ft = faces.Side
	}
	if ft.Base == nil {
		return color.NRGBA{}, false
	}
	col := s.faceColor(ft, c.Block, c, mipSize, tu, tv)
	if c.Water() {
		col = s.waterColor(col, c, s.Biomes.Get(c.Biome))
	}
	return col, true
}

// HasTexture reports whether a block resolved a real texture. Callers that
// draw a block's whole face in one pass -- the isometric renderer's diamond
// and skirt -- use this to choose a cheap once-per-column path when a block
// has none, rather than sampling per pixel only to get the same flat colour
// back every time.
func (s *Shader) HasTexture(id uint16) bool {
	faces, ok := s.textureFacesFor(id)
	return ok && faces.Top.Base != nil
}

// textureFacesFor looks up a block's resolved textures, safe to call
// whether or not a texture set is configured.
func (s *Shader) textureFacesFor(id uint16) (textures.BlockFaces, bool) {
	if s.Textures == nil {
		return textures.BlockFaces{}, false
	}
	return s.Textures.Get(id)
}

// faceColor samples one face's texture at (tu,tv) within mipSize, applying
// biome tint and alpha-compositing the overlay layer if the model resolved
// one (grass_block's tinted side strip; see textures.FaceTexture). id names
// the block the tint should be looked up for, which is the column's own
// surface block for a normal face but a Decoration's id when painting a
// plant's texture over that surface's top face.
func (s *Shader) faceColor(ft textures.FaceTexture, id uint16, c world.Column, mipSize, tu, tv int) color.NRGBA {
	col := ft.Base.At(mipSize, tu, tv)
	if ft.BaseTint {
		col = s.tintFor(id, col, c)
	}
	if ft.Overlay != nil {
		ov := ft.Overlay.At(mipSize, tu, tv)
		if ft.OverlayTint {
			ov = s.tintFor(id, ov, c)
		}
		col = alphaOver(ov, col)
	}
	return col
}

// tintFor applies id's biome tint, if any and if enabled.
func (s *Shader) tintFor(id uint16, col color.NRGBA, c world.Column) color.NRGBA {
	if !s.Opts.BiomeTint {
		return col
	}
	blk := s.Blocks.Get(id)
	bio := s.Biomes.Get(c.Biome)
	return blocks.ApplyTint(col, blk.Tint, bio)
}

// alphaOver composites top over bottom using top's own alpha, standard
// "source-over" blending.
func alphaOver(top, bottom color.NRGBA) color.NRGBA {
	a := float64(top.A) / 255
	if a <= 0 {
		return bottom
	}
	if a >= 1 {
		return top
	}
	inv := 1 - a
	return color.NRGBA{
		R: uint8(float64(top.R)*a + float64(bottom.R)*inv + 0.5),
		G: uint8(float64(top.G)*a + float64(bottom.G)*inv + 0.5),
		B: uint8(float64(top.B)*a + float64(bottom.B)*inv + 0.5),
		A: uint8(math.Min(255, float64(top.A)+float64(bottom.A)*inv+0.5)),
	}
}

// waterColor blends the sea floor with the biome water colour by depth, so
// shallows show the bottom through the water and deep ocean goes opaque.
func (s *Shader) waterColor(floor color.NRGBA, c world.Column, bio blocks.Biome) color.NRGBA {
	water := bio.Water
	if !s.Opts.BiomeTint {
		water = color.NRGBA{0x3f, 0x76, 0xe4, 0xff}
	}
	if !s.Opts.WaterDepthShading {
		return water
	}
	depth := float64(c.WaterDepth())
	maxDepth := s.Opts.MaxWaterDepth
	if maxDepth <= 0 {
		maxDepth = 28
	}
	// Opacity ramps quickly in the first few blocks then saturates, matching how
	// water actually reads from above.
	t := 1 - math.Exp(-depth/(maxDepth*0.38))
	if t > 0.94 {
		t = 0.94
	}
	col := blocks.Lerp(floor, water, t)
	// Deep water additionally darkens, which is what makes ocean trenches and
	// drop-offs visible at a glance.
	darken := 1 - 0.34*math.Min(1, depth/maxDepth)
	return blocks.Scale(col, darken)
}

// reliefFactor computes the brightness multiplier for relief shading.
//
// The light is treated as coming from the north-west: a column standing above
// its northern or western neighbour catches the light and brightens, one that
// sits below them falls into shadow. Deltas are clamped and the response is
// square-rooted so a two-block step is clearly visible while a 200-block cliff
// does not blow out to pure white.
//
// An absolute-elevation ramp is layered on top at a much lower weight so broad
// high ground reads as high even where it is locally flat.
func (s *Shader) reliefFactor(surf *world.Surface, x, z int, c world.Column) float64 {
	h := float64(c.RenderY())

	// Water is shaded by its own surface, so ripples of sea floor relief do not
	// bleed through as noise.
	north, nOK := neighbourHeight(surf, x, z-1)
	west, wOK := neighbourHeight(surf, x-1, z)

	delta := 0.0
	n := 0
	if nOK {
		delta += h - north
		n++
	}
	if wOK {
		delta += h - west
		n++
	}
	if n > 0 {
		delta /= float64(n)
	}

	clampAt := s.Opts.ShadingClamp
	if clampAt <= 0 {
		clampAt = 6
	}
	if delta > clampAt {
		delta = clampAt
	} else if delta < -clampAt {
		delta = -clampAt
	}

	// Square-root response: small steps stay visible, large ones compress.
	norm := delta / clampAt
	shaped := math.Copysign(math.Sqrt(math.Abs(norm)), norm)
	f := 1 + shaped*s.Opts.ShadingStrength

	if s.Opts.AmbientHeight > 0 {
		span := float64(max(1, surf.MaxY-surf.MinY))
		t := (h - float64(surf.MinY)) / span
		f *= 1 - s.Opts.AmbientHeight*0.5 + s.Opts.AmbientHeight*t
	}
	return f
}

// neighbourHeight returns a neighbour's rendered elevation, reporting false for
// unexplored columns so a map edge does not fake a cliff.
func neighbourHeight(surf *world.Surface, x, z int) (float64, bool) {
	c := surf.At(x, z)
	if !c.Present() {
		return 0, false
	}
	return float64(c.RenderY()), true
}

// contourColor reports the contour-line colour for one surface column. Lines
// are drawn where this column crosses an elevation interval boundary relative
// to its north or west neighbour, matching reliefFactor's neighbour convention.
func (s *Shader) contourColor(surf *world.Surface, x, z int, c world.Column) (color.NRGBA, bool) {
	if !s.Opts.Contour || !c.Present() {
		return color.NRGBA{}, false
	}
	interval := s.Opts.ContourInterval
	if interval <= 0 {
		interval = 8
	}
	majorEvery := s.Opts.ContourMajorEvery
	if majorEvery <= 0 {
		majorEvery = 4
	}

	hLevel := int(math.Floor(float64(c.RenderY()) / float64(interval)))
	major := false
	crossed := false
	for _, n := range [][2]int{{x, z - 1}, {x - 1, z}} {
		nh, ok := neighbourHeight(surf, n[0], n[1])
		if !ok {
			continue
		}
		nLevel := int(math.Floor(nh / float64(interval)))
		if hLevel == nLevel {
			continue
		}
		boundary := min(hLevel, nLevel) + 1
		if boundary%majorEvery == 0 {
			major = true
		}
		crossed = true
	}
	if !crossed {
		return color.NRGBA{}, false
	}
	if major {
		return s.Opts.ContourMajorColor, true
	}
	return s.Opts.ContourColor, true
}

// lightFactor maps stored light 0..15 onto a brightness multiplier. Fully lit
// terrain is untouched; unlit terrain bottoms out at a readable level rather
// than going black.
func lightFactor(light uint8) float64 {
	if light >= 15 {
		return 1
	}
	return 0.55 + 0.45*float64(light)/15
}

// sampleGradient interpolates a colour ramp at t in [0,1].
func sampleGradient(g []color.NRGBA, t float64) color.NRGBA {
	if len(g) == 0 {
		return color.NRGBA{0x80, 0x80, 0x80, 0xff}
	}
	if len(g) == 1 || t <= 0 {
		return g[0]
	}
	if t >= 1 {
		return g[len(g)-1]
	}
	pos := t * float64(len(g)-1)
	i := int(pos)
	if i >= len(g)-1 {
		return g[len(g)-1]
	}
	return blocks.Lerp(g[i], g[i+1], pos-float64(i))
}

// FaceShade returns the brightness multiplier for an isometric block face.
//
// Keeping these constants here rather than in the isometric renderer means the
// top-down and isometric views agree on which direction the light comes from.
const (
	// FaceTop is the fully-lit upward face.
	FaceTop = 1.0
	// FaceLeft is the face toward the camera's left, in partial shadow.
	FaceLeft = 0.82
	// FaceRight is the face toward the camera's right, in deeper shadow.
	FaceRight = 0.68
)
