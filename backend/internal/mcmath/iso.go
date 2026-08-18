package mcmath

import "math"

// Isometric projection.
//
// # Why 2:1 dimetric rather than true isometric
//
// A mathematically true isometric projection (camera along (1,1,1)) yields
//
//	u = (X - Z)/sqrt(2)        v = (X + Z - 2Y)/sqrt(6)
//
// which makes every block face equal in area but puts irrational numbers in the
// middle of a raster tile pipeline. Block edges then land on fractional pixels,
// tiles cannot be composited without resampling, and the pixel-art crispness
// Minecraft terrain depends on is destroyed.
//
// This renderer therefore uses the standard 2:1 dimetric projection, where a
// block's top face is a diamond exactly twice as wide as it is tall and one Y
// level is exactly one diamond-height. Everything lands on exact binary
// fractions, so at every pyramid level block edges fall on integer pixels.
//
// One block, in iso units:
//
//	   (U0,V0)              top face  : diamond 2 wide, 1 tall
//	      /\                +A face   : lower-right edge, IsoBlockHeight tall
//	     /  \               +B face   : lower-left  edge, IsoBlockHeight tall
//	    /    \
//	   /  top \             At zoom 10 (16 px per iso unit) this is exactly
//	  \        /            the classic 32x32 isometric block sprite:
//	   \      /             32 px wide, 16 px top diamond, 16 px sides.
//	    \    /
//	+B   \  /   +A
//	 |    \/     |
//	 |    ||     |
//
// # Forward projection
//
//	u = (a - b) * IsoHalfWidth
//	v = (a + b) * IsoHalfHeight - y * IsoBlockHeight
//
// where (a, b) are the world X/Z axes rotated into camera space. The camera
// rotation is what makes the four diagonal view directions possible without
// special-casing anything downstream: only Rotate/Unrotate know about it.
//
// v grows downward, matching both screen space and the tile grid's row order.
const (
	// IsoHalfWidth is the u distance from a top-face diamond's centre vertex to
	// its side vertex, i.e. half the diamond width.
	IsoHalfWidth = 1.0
	// IsoHalfHeight is the v distance covered by one step along a or b, i.e.
	// half the diamond height.
	IsoHalfHeight = 0.5
	// IsoBlockHeight is the v distance covered by one Y level. Equal to the
	// full diamond height, so a cube renders as a regular hexagon.
	IsoBlockHeight = 1.0
	// IsoDiamondWidth / IsoDiamondHeight are the top face's extents in iso units.
	IsoDiamondWidth  = 2 * IsoHalfWidth  // 2
	IsoDiamondHeight = 2 * IsoHalfHeight // 1
)

// IsoCamera names the four diagonal viewing directions by the world corner the
// camera sits over. Only the default is rendered today, but no code outside
// Rotate/Unrotate/DepthKey may assume which one is active.
type IsoCamera uint8

const (
	// CameraSE views the world from the +X/+Z corner: north and west faces of
	// terrain are hidden, south and east faces are visible. This is the default.
	CameraSE IsoCamera = iota
	// CameraSW views from -X/+Z.
	CameraSW
	// CameraNW views from -X/-Z.
	CameraNW
	// CameraNE views from +X/-Z.
	CameraNE
)

// DefaultCamera is the single camera direction the pipeline renders today.
const DefaultCamera = CameraSE

// ParseIsoCamera resolves a camera name, defaulting to DefaultCamera.
func ParseIsoCamera(s string) (IsoCamera, bool) {
	switch s {
	case "", "se", "SE", "south_east":
		return CameraSE, true
	case "sw", "SW", "south_west":
		return CameraSW, true
	case "nw", "NW", "north_west":
		return CameraNW, true
	case "ne", "NE", "north_east":
		return CameraNE, true
	}
	return DefaultCamera, false
}

// String returns the lowercase camera name used in tile paths and URLs.
func (c IsoCamera) String() string {
	switch c {
	case CameraSW:
		return "sw"
	case CameraNW:
		return "nw"
	case CameraNE:
		return "ne"
	default:
		return "se"
	}
}

// Rotate maps world X/Z onto the camera's depth axes (a, b). The transform is a
// signed axis swap, so it is exact in both integers and floats and its own
// inverse family. Larger a+b always means nearer to the camera.
func (c IsoCamera) Rotate(x, z float64) (a, b float64) {
	switch c {
	case CameraSW:
		return z, -x
	case CameraNW:
		return -x, -z
	case CameraNE:
		return -z, x
	default: // CameraSE
		return x, z
	}
}

// RotateInt is Rotate for integer block coordinates.
func (c IsoCamera) RotateInt(x, z int) (a, b int) {
	switch c {
	case CameraSW:
		return z, -x
	case CameraNW:
		return -x, -z
	case CameraNE:
		return -z, x
	default:
		return x, z
	}
}

// Unrotate inverts Rotate, mapping camera-space (a, b) back to world X/Z.
func (c IsoCamera) Unrotate(a, b float64) (x, z float64) {
	switch c {
	case CameraSW:
		return -b, a
	case CameraNW:
		return -a, -b
	case CameraNE:
		return b, -a
	default:
		return a, b
	}
}

// UnrotateInt is Unrotate for integer coordinates.
func (c IsoCamera) UnrotateInt(a, b int) (x, z int) {
	switch c {
	case CameraSW:
		return -b, a
	case CameraNW:
		return -a, -b
	case CameraNE:
		return b, -a
	default:
		return a, b
	}
}

// DepthKey returns the painter's-algorithm sort key for a column. Columns must
// be drawn in ascending DepthKey order: smallest is farthest from the camera,
// so later draws correctly overpaint earlier ones. Two columns with equal keys
// never overlap, so their relative order is irrelevant.
func (c IsoCamera) DepthKey(x, z int) int {
	a, b := c.RotateInt(x, z)
	return a + b
}

// IsoProjection projects Minecraft block space into isometric map space.
//
// The zero value is a valid CameraSE projection.
type IsoProjection struct {
	Camera IsoCamera
}

// NewIsoProjection returns a projection for the given camera.
func NewIsoProjection(cam IsoCamera) IsoProjection { return IsoProjection{Camera: cam} }

// Project maps a world-space point (a corner or any continuous position, not a
// block index) to iso map space.
func (p IsoProjection) Project(x, y, z float64) (u, v float64) {
	a, b := p.Camera.Rotate(x, z)
	u = (a - b) * IsoHalfWidth
	v = (a+b)*IsoHalfHeight - y*IsoBlockHeight
	return
}

// Unproject inverts Project for a known elevation y. Iso space alone is
// ambiguous -- a screen position corresponds to an entire ray through the
// world -- so recovering a block requires either a known y or a ray march
// against terrain heights (see RayMarch).
func (p IsoProjection) Unproject(u, v, y float64) (x, z float64) {
	// v = (a+b)/2 - y  =>  a+b = 2*(v + y)
	// u = a-b          =>  a-b = u
	half := (v + y*IsoBlockHeight) / IsoHalfHeight / 2 // (a+b)/2
	du := u / IsoHalfWidth / 2                         // (a-b)/2
	a := half + du
	b := half - du
	return p.Camera.Unrotate(a, b)
}

// ProjectBlockTop returns the iso position of the top vertex of a block's top
// face. Block (x,y,z) occupies the world cube [x,x+1) x [y,y+1) x [z,z+1), so
// its top face sits at elevation y+1 and its top vertex at the block's minimum
// a/b corner.
//
// The full top-face diamond has vertices:
//
//	(u,   v)     (u+1, v+0.5)     (u,   v+1)     (u-1, v+0.5)
func (p IsoProjection) ProjectBlockTop(x, y, z int) (u, v float64) {
	return p.Project(float64(x), float64(y+1), float64(z))
}

// SpriteHeight is the v extent, in iso units, of a column whose top face is at
// elevation topY and whose exposed side faces run down to elevation floorY.
// This is what tile overscan must account for: a tall column paints far above
// and below its own X/Z footprint.
func SpriteHeight(topY, floorY int) float64 {
	depth := float64(topY - floorY)
	if depth < 0 {
		depth = 0
	}
	return IsoDiamondHeight + depth*IsoBlockHeight
}

// ---------------------------------------------------------------------------
// Iso bounds and tile footprints
// ---------------------------------------------------------------------------

// IsoBounds is a half-open rectangle in isometric map space.
type IsoBounds struct {
	MinU, MinV, MaxU, MaxV float64
}

// Empty reports whether the rectangle covers no area.
func (b IsoBounds) Empty() bool { return b.MaxU <= b.MinU || b.MaxV <= b.MinV }

// Expand grows the rectangle by n iso units on every side.
func (b IsoBounds) Expand(n float64) IsoBounds {
	return IsoBounds{b.MinU - n, b.MinV - n, b.MaxU + n, b.MaxV + n}
}

// IsoTileBounds returns the iso-space rectangle covered by an iso tile. Iso
// tiles share the top-down pyramid's resolutions exactly -- level z has
// 2^(6-z) iso units per pixel -- so both modes can drive an identical zoom
// ladder and fractional zoom behaves the same in each.
func IsoTileBounds(t TilePos) IsoBounds {
	span := float64(TileSpanBlocks(t.Zoom))
	if t.Zoom > MaxIntegerZoom {
		span = TileSpanBlocksF(t.Zoom)
	}
	return IsoBounds{
		MinU: float64(t.X) * span,
		MinV: float64(t.Y) * span,
		MaxU: float64(t.X)*span + span,
		MaxV: float64(t.Y)*span + span,
	}
}

// IsoTileAt returns the iso tile containing an iso-space position.
func IsoTileAt(u, v float64, zoom int) TilePos {
	span := TileSpanBlocksF(zoom)
	return TilePos{Zoom: zoom, X: FloorDivF(u, span), Y: FloorDivF(v, span)}
}

// WorldFootprint returns every block column that could possibly paint into the
// given iso rectangle, for terrain spanning elevations [minY, maxY].
//
// This is the exact solution to isometric tile overlap. A column's sprite
// extends above its own X/Z footprint by its height and below it by its
// exposed sides, so a mountain many hundreds of blocks away in X/Z can still
// paint into this tile. Inverting the projection across the *entire* elevation
// range yields a superset of contributing columns; render all of them and crop
// to the tile afterwards and no seam or clipped peak is possible.
//
// The returned bounds grow linearly with world height, not with tile size, so
// even a -64..320 world costs a bounded, predictable amount of overscan.
func (p IsoProjection) WorldFootprint(b IsoBounds, minY, maxY int) BlockBounds {
	if b.Empty() {
		return BlockBounds{}
	}
	// Elevations of interest are block *top faces*, which sit one above the
	// highest block, and exposed side faces reaching down to the lowest.
	yLo := float64(minY)
	yHi := float64(maxY + 1)

	// a = u/(2*IsoHalfWidth) + (v + y*IsoBlockHeight)/(2*IsoHalfHeight)
	// b = -u/(2*IsoHalfWidth) + (v + y*IsoBlockHeight)/(2*IsoHalfHeight)
	const uScale = 1.0 / (2 * IsoHalfWidth)
	const vScale = 1.0 / (2 * IsoHalfHeight)
	const yScale = IsoBlockHeight * vScale

	// The diamond is IsoHalfWidth wide either side of its top vertex, so pad
	// by one block in each camera axis to cover partially-visible sprites.
	aMin := b.MinU*uScale + b.MinV*vScale + yLo*yScale - 1
	aMax := b.MaxU*uScale + b.MaxV*vScale + yHi*yScale + 1
	bMin := -b.MaxU*uScale + b.MinV*vScale + yLo*yScale - 1
	bMax := -b.MinU*uScale + b.MaxV*vScale + yHi*yScale + 1

	// Un-rotating a signed axis swap maps the (a,b) box onto an (x,z) box; take
	// all four corners so the mapping stays correct for every camera.
	x0, z0 := p.Camera.Unrotate(aMin, bMin)
	x1, z1 := p.Camera.Unrotate(aMax, bMin)
	x2, z2 := p.Camera.Unrotate(aMin, bMax)
	x3, z3 := p.Camera.Unrotate(aMax, bMax)

	minX := math.Min(math.Min(x0, x1), math.Min(x2, x3))
	maxX := math.Max(math.Max(x0, x1), math.Max(x2, x3))
	minZ := math.Min(math.Min(z0, z1), math.Min(z2, z3))
	maxZ := math.Max(math.Max(z0, z1), math.Max(z2, z3))

	return BlockBounds{
		MinX: int(math.Floor(minX)),
		MinZ: int(math.Floor(minZ)),
		MaxX: int(math.Ceil(maxX)) + 1,
		MaxZ: int(math.Ceil(maxZ)) + 1,
	}
}

// IsoFootprintOfBlocks returns the iso rectangle that a block region projects
// into, across the full elevation range. This is the forward counterpart of
// WorldFootprint and is used to decide which iso tiles a changed chunk dirties.
func (p IsoProjection) IsoFootprintOfBlocks(b BlockBounds, minY, maxY int) IsoBounds {
	if b.Empty() {
		return IsoBounds{}
	}
	out := IsoBounds{MinU: math.Inf(1), MinV: math.Inf(1), MaxU: math.Inf(-1), MaxV: math.Inf(-1)}
	xs := [2]float64{float64(b.MinX), float64(b.MaxX)}
	zs := [2]float64{float64(b.MinZ), float64(b.MaxZ)}
	ys := [2]float64{float64(minY), float64(maxY + 1)}
	for _, x := range xs {
		for _, z := range zs {
			for _, y := range ys {
				u, v := p.Project(x, y, z)
				out.MinU = math.Min(out.MinU, u)
				out.MaxU = math.Max(out.MaxU, u)
				out.MinV = math.Min(out.MinV, v)
				out.MaxV = math.Max(out.MaxV, v)
			}
		}
	}
	// A column's own diamond and skirt reach beyond its corner projections.
	return out.Expand(IsoHalfWidth)
}

// ---------------------------------------------------------------------------
// Hit testing
// ---------------------------------------------------------------------------

// HeightSampler reports the elevation of the topmost rendered block at a column,
// and whether that column has any data at all.
type HeightSampler interface {
	SampleHeight(x, z int) (y int, ok bool)
}

// RayMarch resolves an isometric screen position to the block whose surface is
// visible there, by intersecting the view ray with the terrain height field.
//
// # Why the ray is a straight diagonal
//
// For a fixed screen position (u, v), the projection pins two quantities:
//
//	a - b = u / IsoHalfWidth                  (constant along the ray)
//	a + b = (v + y*IsoBlockHeight)/IsoHalfHeight
//
// so with s := v + y, we get a = s + du and b = s - du for a fixed du. Both
// camera axes therefore advance in lockstep with elevation: descending y walks
// the ray directly away from the eye. Marching y downward from the world
// ceiling walks from the eye into the scene, so the first solid hit is the
// nearest surface -- exactly what the user is pointing at.
//
// # Why it steps by column slab, not by whole blocks
//
// Stepping y by 1 moves both a and b by 1, which walks the diagonal and can
// step straight over a column the ray genuinely passes through whenever u is
// not an even integer. Instead this walks the ray slab by slab: floor(a) and
// floor(b) each change at their own s boundaries, and the traversal decrements
// whichever integer boundary comes next (or both on an exact tie). That visits
// every column the ray crosses, in order, using only integer decrements -- so
// there is no floating-point epsilon to drift and no column is ever skipped.
//
// A column is hit when the terrain's top surface reaches into the ray's
// elevation range for that slab. Block surf occupies [surf, surf+1), so its
// solid top sits at elevation surf+1.
//
// This is genuinely Y-aware: pointing at a mountainside returns the mountain
// column, not the column that would sit under that pixel on a flat plane.
func (p IsoProjection) RayMarch(u, v float64, minY, maxY int, h HeightSampler) (x, y, z int, ok bool) {
	if maxY < minY {
		return 0, 0, 0, false
	}
	du := u / IsoHalfWidth / 2

	// s at the world ceiling and floor. The ceiling is maxY+1 so a surface
	// sitting exactly at maxY is still caught.
	sHi := v + float64(maxY+1)*IsoBlockHeight/(2*IsoHalfHeight)
	sLo := v + float64(minY)*IsoBlockHeight/(2*IsoHalfHeight)

	a := int(math.Floor(sHi + du))
	b := int(math.Floor(sHi - du))

	// Each unit of s crosses at most two slab boundaries (one per axis), so the
	// traversal is bounded by twice the world height plus slack.
	maxSteps := 4*(maxY-minY) + 16
	for i := 0; i < maxSteps; i++ {
		// Lower s boundary of the current slab on each axis.
		lowA := float64(a) - du
		lowB := float64(b) + du
		s := math.Max(lowA, lowB)

		// Lowest elevation the ray reaches inside this column, clamped to the
		// world floor.
		yLow := (s - v) * (2 * IsoHalfHeight) / IsoBlockHeight
		if yLow < float64(minY) {
			yLow = float64(minY)
		}

		bx, bz := p.Camera.UnrotateInt(a, b)
		if surf, has := h.SampleHeight(bx, bz); has && float64(surf+1) >= yLow {
			return bx, surf, bz, true
		}

		if s <= sLo {
			break // the ray has left the bottom of the world
		}
		// Step into the next slab by crossing whichever boundary is nearest.
		// An exact tie means both axes cross together and both must advance.
		if lowA >= lowB {
			a--
		}
		if lowB >= lowA {
			b--
		}
	}
	return 0, 0, 0, false
}

// UnprojectFlat resolves an iso position against a flat reference plane of
// blocks whose tops sit at elevation blockY+1. It is the cheap fallback for
// cursor readout before terrain heights are available; click interactions and
// the live readout both prefer RayMarch.
func (p IsoProjection) UnprojectFlat(u, v float64, blockY int) (x, z int) {
	fx, fz := p.Unproject(u, v, float64(blockY+1))
	return int(math.Floor(fx)), int(math.Floor(fz))
}
