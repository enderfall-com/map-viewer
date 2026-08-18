package render

import (
	"testing"

	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
)

// diamondRuns reproduces the rasteriser's per-column row span, so the
// tessellation property can be checked independently of pixel writing.
func diamondRows(i, w, h int) (jLo, jHi int) {
	e := min(i, w-1-i)
	jLo = (h - e - 1 + 1) / 2
	jHi = (h + e - 1) / 2
	return
}

// A diamond must cover exactly W*H/2 pixels. That is the area of the lattice's
// fundamental domain under the (W/2, H/2) neighbour offset, so any other count
// means adjacent columns either overlap or leave gaps.
func TestDiamondAreaMatchesLatticeCell(t *testing.T) {
	for _, ppu := range []int{2, 4, 8, 16, 32} {
		w, h := 2*ppu, ppu
		total := 0
		for i := 0; i < w; i++ {
			jLo, jHi := diamondRows(i, w, h)
			if jHi < jLo {
				continue
			}
			if jLo < 0 || jHi >= h {
				t.Errorf("ppu %d col %d: rows %d..%d escape the diamond height %d", ppu, i, jLo, jHi, h)
			}
			total += jHi - jLo + 1
		}
		want := w * h / 2
		if total != want {
			t.Errorf("ppu %d: diamond covers %d px, want %d (w=%d h=%d)", ppu, total, want, w, h)
		}
	}
}

// The decisive test: stamp a large grid of neighbouring diamonds at their true
// lattice offsets and confirm every interior pixel is covered exactly once.
// Gaps would show as a moire across all isometric terrain; double coverage
// would mean wasted fill and incorrect blending at column edges.
func TestDiamondLatticeTilesPlaneExactly(t *testing.T) {
	for _, ppu := range []int{2, 4, 8, 16} {
		w, h := 2*ppu, ppu
		const size = 256
		count := make([]int, size*size)

		// Neighbouring columns sit at (+w/2, +h/2) for a step in a, and
		// (-w/2, +h/2) for a step in b -- exactly the projection's geometry.
		for da := -40; da <= 40; da++ {
			for db := -40; db <= 40; db++ {
				cx := size/2 + (da-db)*(w/2)
				cy := size/2 + (da+db)*(h/2)
				left := cx - w/2
				for i := 0; i < w; i++ {
					px := left + i
					if px < 0 || px >= size {
						continue
					}
					jLo, jHi := diamondRows(i, w, h)
					for j := jLo; j <= jHi; j++ {
						py := cy + j
						if py < 0 || py >= size {
							continue
						}
						count[py*size+px]++
					}
				}
			}
		}

		// Inspect a central window that the stamped lattice fully surrounds.
		lo, hi := size/2-40, size/2+40
		gaps, overlaps := 0, 0
		for y := lo; y < hi; y++ {
			for x := lo; x < hi; x++ {
				switch c := count[y*size+x]; {
				case c == 0:
					gaps++
				case c > 1:
					overlaps++
				}
			}
		}
		if gaps != 0 || overlaps != 0 {
			t.Errorf("ppu %d: lattice has %d uncovered and %d doubly-covered pixels",
				ppu, gaps, overlaps)
		}
	}
}

// One iso unit of elevation must move a column by exactly the diamond height in
// pixels, otherwise stacked terrain would not line up with its own side faces.
func TestElevationStepMatchesDiamondHeight(t *testing.T) {
	p := mcmath.NewIsoProjection(mcmath.CameraSE)
	for _, zoom := range []int{7, 8, 9, 10} {
		ppu := mcmath.PixelsPerBlock(zoom)
		_, v0 := p.ProjectBlockTop(0, 64, 0)
		_, v1 := p.ProjectBlockTop(0, 65, 0)
		gotPx := (v0 - v1) * ppu
		wantPx := ppu * mcmath.IsoDiamondHeight
		if gotPx != wantPx {
			t.Errorf("zoom %d: one Y level = %v px, diamond height = %v px", zoom, gotPx, wantPx)
		}
	}
}

// Neighbouring columns must land exactly half a diamond apart in both axes, at
// every zoom the renderer draws directly.
func TestNeighbourOffsetIsHalfDiamond(t *testing.T) {
	p := mcmath.NewIsoProjection(mcmath.CameraSE)
	for _, zoom := range []int{7, 8, 9, 10} {
		ppu := mcmath.PixelsPerBlock(zoom)
		w, h := 2*ppu, ppu

		u0, v0 := p.ProjectBlockTop(100, 64, 100)
		uA, vA := p.ProjectBlockTop(101, 64, 100) // step in a
		uB, vB := p.ProjectBlockTop(100, 64, 101) // step in b

		if dx := (uA - u0) * ppu; dx != w/2 {
			t.Errorf("zoom %d: +a step moves %v px, want %v", zoom, dx, w/2)
		}
		if dy := (vA - v0) * ppu; dy != h/2 {
			t.Errorf("zoom %d: +a step moves %v px vertically, want %v", zoom, dy, h/2)
		}
		if dx := (uB - u0) * ppu; dx != -w/2 {
			t.Errorf("zoom %d: +b step moves %v px, want %v", zoom, dx, -w/2)
		}
		if dy := (vB - v0) * ppu; dy != h/2 {
			t.Errorf("zoom %d: +b step moves %v px vertically, want %v", zoom, dy, h/2)
		}
	}
}
