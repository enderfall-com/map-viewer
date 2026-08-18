package textures

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/png" // registers the PNG decoder image.Decode needs
	"sync"

	"github.com/enderfall/minecraft-map/backend/internal/blocks"
)

// Mips is a power-of-two mip chain of one texture, from its native
// resolution down to 1x1. The 1x1 level is the texture's average colour,
// which is what lets a zoomed-out or averaged tile reuse exactly the same
// representative colour a flat-colour block would have used, keeping the
// pyramid visually consistent between the textured base level and every
// composited level above it.
type Mips struct {
	levels map[int]*image.NRGBA
	sizes  []int // descending, e.g. [16, 8, 4, 2, 1]

	// HasAlpha is true when the native-resolution texture has any texel that
	// is not fully opaque (ISO_VOXEL_PLAN.md §5 Phase 4). A block whose
	// resolved top or side texture reports this cannot be assumed to fully
	// hide what is behind it -- a real leaf canopy has gaps -- so it must
	// not occlude in the isometric voxel renderer regardless of what
	// blocks.json's Transparent flag says. Computed once here from the pack
	// texture itself, never hand-curated per block name.
	HasAlpha bool
}

// Level returns the largest available mip whose edge length is <= want,
// falling back to the smallest (1x1) mip if want is below every level.
func (m *Mips) Level(want int) *image.NRGBA {
	if m == nil {
		return nil
	}
	for _, s := range m.sizes {
		if s <= want {
			return m.levels[s]
		}
	}
	return m.levels[m.sizes[len(m.sizes)-1]]
}

// At samples one texel from the mip matching size, clamping out-of-range
// coordinates rather than wrapping or panicking -- callers pass texel
// coordinates derived from rounded pixel maths, which can land one unit
// outside the texture at an edge.
func (m *Mips) At(size, x, y int) color.NRGBA {
	img := m.Level(size)
	if img == nil {
		return color.NRGBA{}
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if x < 0 {
		x = 0
	} else if x >= w {
		x = w - 1
	}
	if y < 0 {
		y = 0
	} else if y >= h {
		y = h - 1
	}
	return img.NRGBAAt(b.Min.X+x, b.Min.Y+y)
}

// Average is the 1x1 mip's colour: the flat-colour representative of this
// texture, used wherever the renderer draws a block smaller than one pixel.
func (m *Mips) Average() color.NRGBA {
	if m == nil {
		return color.NRGBA{}
	}
	return m.At(1, 0, 0)
}

// decodeMips decodes a texture and builds its full mip chain by repeated
// alpha-weighted 2x2 downsampling.
//
// Only the top-left square region of the source is used. This matters for
// animated textures (water, lava, fire, magma): Minecraft stores those as a
// tall spritesheet of square frames stacked vertically, and the top-left
// square is frame 0 -- a plausible still image, rather than a smeared
// average of every frame in the animation.
func decodeMips(data []byte) (*Mips, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode texture: %w", err)
	}
	b := img.Bounds()
	size := min(b.Dx(), b.Dy())
	if size < 1 {
		return nil, fmt.Errorf("degenerate texture size %dx%d", b.Dx(), b.Dy())
	}

	base := image.NewNRGBA(image.Rect(0, 0, size, size))
	draw.Draw(base, base.Bounds(), img, b.Min, draw.Src)

	m := &Mips{levels: map[int]*image.NRGBA{size: base}, sizes: []int{size}, HasAlpha: hasAlpha(base)}
	cur, curSize := base, size
	for curSize > 1 {
		next := downsample2x(cur, curSize)
		curSize = max(1, curSize/2)
		m.levels[curSize] = next
		m.sizes = append(m.sizes, curSize)
		cur = next
	}
	return m, nil
}

// hasAlpha reports whether any texel of the native-resolution image is not
// fully opaque. Checking the native mip is deliberate: downsampling
// alpha-weight-averages neighbouring texels (see downsample2x), which can
// wash a texture with only a few fully-transparent corner texels toward
// full opacity at lower mip levels even though the real texture has gaps.
func hasAlpha(base *image.NRGBA) bool {
	b := base.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if base.NRGBAAt(x, y).A != 255 {
				return true
			}
		}
	}
	return false
}

// downsample2x halves a square image with an alpha-weighted 2x2 box filter.
// Weighting by alpha is what keeps a fully-transparent corner of a leaf or
// glass-pane texture from darkening its opaque neighbours into a grey halo
// once several mip levels have compounded the error.
func downsample2x(src *image.NRGBA, size int) *image.NRGBA {
	half := max(1, size/2)
	out := image.NewNRGBA(image.Rect(0, 0, half, half))
	for y := 0; y < half; y++ {
		for x := 0; x < half; x++ {
			var rSum, gSum, bSum, aSum uint32
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					sx, sy := x*2+dx, y*2+dy
					if sx >= size || sy >= size {
						continue
					}
					c := src.NRGBAAt(sx, sy)
					a := uint32(c.A)
					rSum += uint32(c.R) * a
					gSum += uint32(c.G) * a
					bSum += uint32(c.B) * a
					aSum += a
				}
			}
			var r, g, b uint8
			if aSum > 0 {
				r, g, b = uint8(rSum/aSum), uint8(gSum/aSum), uint8(bSum/aSum)
			}
			out.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: uint8(aSum / 4)})
		}
	}
	return out
}

// FaceTexture is one renderable face: a base texture, optionally tinted, and
// an optional second texture layered on top of it with its own tint. The
// overlay exists for grass_block's tinted side strip (see faceRef); most
// blocks have no overlay.
type FaceTexture struct {
	Base        *Mips
	BaseTint    bool
	Overlay     *Mips
	OverlayTint bool
}

// BlockFaces is one block's resolved top, bottom and (representative) side
// textures.
type BlockFaces struct {
	Top, Bottom, Side FaceTexture
}

type cacheEntry struct {
	faces BlockFaces
	ok    bool
}

// Set lazily resolves and caches per-block textures, keyed by registry block
// ID exactly like blocks.Registry itself, and for the same reason: a world
// scan discovers block IDs incrementally, so resolving eagerly for "every
// known block" at startup would miss anything not yet seen. Safe for
// concurrent use by the whole tile worker pool.
type Set struct {
	store *Store
	reg   *blocks.Registry

	mu    sync.RWMutex
	cache map[uint16]cacheEntry

	// texCache deduplicates decoding: many blocks share an underlying PNG
	// (dirt.png is both the "dirt" block and grass_block's bottom face), and
	// grass alone pulls in three separate textures per block that uses it.
	texMu    sync.Mutex
	texCache map[string]*Mips
}

// NewSet creates a texture set backed by store, resolving block identifiers
// against reg.
func NewSet(store *Store, reg *blocks.Registry) *Set {
	return &Set{
		store:    store,
		reg:      reg,
		cache:    make(map[uint16]cacheEntry),
		texCache: make(map[string]*Mips),
	}
}

// Get returns the resolved faces for a block ID, and whether any texture
// could be resolved at all. Callers must fall back to the flat-colour
// renderer when ok is false -- most blocks with unusual geometry, and every
// block when no texture source is configured, will always report false.
func (s *Set) Get(id uint16) (BlockFaces, bool) {
	s.mu.RLock()
	e, done := s.cache[id]
	s.mu.RUnlock()
	if done {
		return e.faces, e.ok
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if e, done := s.cache[id]; done {
		return e.faces, e.ok
	}
	name := s.reg.Get(id).Name
	faces, ok := s.resolveAndDecode(name)
	if ok && (faces.Top.Base.HasAlpha || faces.Side.Base.HasAlpha) {
		// Real texels disagree with blocks.json's Transparent flag (leaves
		// are deliberately marked non-transparent for the top-down surface
		// scan, see HANDOFF.md, but a real leaf texture still has gaps): the
		// isometric voxel renderer must not treat this block as occluding.
		// ISO_VOXEL_PLAN.md §5 Phase 4.
		s.reg.DowngradeOccludes(id)
	}
	s.cache[id] = cacheEntry{faces, ok}
	return faces, ok
}

func (s *Set) resolveAndDecode(blockID string) (BlockFaces, bool) {
	rf, ok := resolveBlockModel(s.store, blockID)
	if !ok {
		return BlockFaces{}, false
	}
	top := s.faceTexture(rf.Up)
	bottom := s.faceTexture(rf.Down)
	side := s.faceTexture(rf.Side)
	if top.Base == nil || side.Base == nil {
		// The model resolved but the texture files themselves are missing --
		// a pack that patches blockstates without shipping matching textures.
		return BlockFaces{}, false
	}
	return BlockFaces{Top: top, Bottom: bottom, Side: side}, true
}

func (s *Set) faceTexture(ref faceRef) FaceTexture {
	if ref.Texture == "" {
		return FaceTexture{}
	}
	base := s.mipsFor(ref.Texture)
	if base == nil {
		return FaceTexture{}
	}
	ft := FaceTexture{Base: base, BaseTint: ref.Tint}
	if ref.OverlayTex != "" {
		if ov := s.mipsFor(ref.OverlayTex); ov != nil {
			ft.Overlay, ft.OverlayTint = ov, ref.OverlayTint
		}
	}
	return ft
}

// mipsFor decodes (or returns the cached decoding of) a texture reference
// such as "minecraft:block/stone". A nil result is cached too, so a broken
// or missing texture reference is not retried on every subsequent lookup.
func (s *Set) mipsFor(ref string) *Mips {
	s.texMu.Lock()
	defer s.texMu.Unlock()
	if m, cached := s.texCache[ref]; cached {
		return m
	}
	m := s.loadTexture(ref)
	s.texCache[ref] = m
	return m
}

func (s *Set) loadTexture(ref string) *Mips {
	ns, local, ok := splitRef(ref)
	if !ok {
		return nil
	}
	data, ok := s.store.Read(fmt.Sprintf("assets/%s/textures/%s.png", ns, local))
	if !ok {
		return nil
	}
	m, err := decodeMips(data)
	if err != nil {
		return nil
	}
	return m
}
