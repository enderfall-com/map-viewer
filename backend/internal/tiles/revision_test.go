package tiles

import (
	"testing"
	"time"

	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
)

// The core incremental-update guarantee: a changed chunk must dirty its own
// tile at every zoom, and every one of those tiles must actually cover it.
func TestAffectedTilesCoverTheChangedChunk(t *testing.T) {
	chunk := DirtyChunk{Dimension: "minecraft:overworld", Pos: mcmath.ChunkPos{X: 52, Z: -31}}
	const minZoom, maxZoom = 0, 10

	affected := AffectedTiles([]DirtyChunk{chunk}, []Mode{ModeTop}, minZoom, maxZoom, -64, 320)
	if len(affected) == 0 {
		t.Fatal("a changed chunk affected no tiles")
	}

	bounds := mcmath.ChunkBounds(chunk.Pos)
	seenZooms := map[int]int{}
	for _, id := range affected {
		if id.Mode != ModeTop {
			t.Errorf("unexpected mode %s", id.Mode)
		}
		tp := mcmath.TilePos{Zoom: id.Zoom, X: id.X, Y: id.Y}
		if !tp.Bounds().Intersects(bounds) {
			t.Errorf("tile %+v (bounds %+v) does not cover the changed chunk %+v",
				id, tp.Bounds(), bounds)
		}
		seenZooms[id.Zoom]++
	}
	// Every level of the pyramid must be represented exactly once: one chunk is
	// smaller than a tile at every zoom in this range.
	for z := minZoom; z <= maxZoom; z++ {
		if seenZooms[z] != 1 {
			t.Errorf("zoom %d has %d affected tiles, want exactly 1", z, seenZooms[z])
		}
	}
}

// Parents are built from children, so a parent regenerated before its children
// would composite stale images. Deepest-first ordering is what prevents that.
func TestAffectedTilesAreOrderedDeepestFirst(t *testing.T) {
	chunks := []DirtyChunk{
		{Dimension: "d", Pos: mcmath.ChunkPos{X: 0, Z: 0}},
		{Dimension: "d", Pos: mcmath.ChunkPos{X: -1, Z: -1}},
	}
	affected := AffectedTiles(chunks, []Mode{ModeTop}, 0, 10, -64, 320)

	last := 1 << 30
	for _, id := range affected {
		if id.Zoom > last {
			t.Fatalf("zoom %d appeared after %d; ordering is not deepest-first", id.Zoom, last)
		}
		last = id.Zoom
	}
	if affected[0].Zoom != 10 {
		t.Errorf("first entry is zoom %d, want the deepest (10)", affected[0].Zoom)
	}
	if affected[len(affected)-1].Zoom != 0 {
		t.Errorf("last entry is zoom %d, want the shallowest (0)", affected[len(affected)-1].Zoom)
	}
}

// Many chunks inside one tile must collapse to a single regeneration. Without
// this, a player clearing an area would queue the same tile hundreds of times.
func TestAffectedTilesDeduplicate(t *testing.T) {
	// A zoom-6 tile spans 512 blocks, i.e. 32x32 chunks. Dirty 64 of them.
	var chunks []DirtyChunk
	for cx := 0; cx < 8; cx++ {
		for cz := 0; cz < 8; cz++ {
			chunks = append(chunks, DirtyChunk{Dimension: "d", Pos: mcmath.ChunkPos{X: cx, Z: cz}})
		}
	}
	affected := AffectedTiles(chunks, []Mode{ModeTop}, 6, 6, -64, 320)
	if len(affected) != 1 {
		t.Errorf("64 chunks inside one zoom-6 tile produced %d tiles, want 1: %+v",
			len(affected), affected)
	}

	// At zoom 10 a tile is 2x2 chunks, so 8x8 chunks must produce 4x4 tiles.
	affected = AffectedTiles(chunks, []Mode{ModeTop}, 10, 10, -64, 320)
	if len(affected) != 16 {
		t.Errorf("8x8 chunks at zoom 10 produced %d tiles, want 16", len(affected))
	}
}

func TestAffectedTilesHandlesNegativeChunks(t *testing.T) {
	// Chunk -1,-1 sits in tile -1,-1 at every zoom where a tile exceeds a chunk.
	affected := AffectedTiles(
		[]DirtyChunk{{Dimension: "d", Pos: mcmath.ChunkPos{X: -1, Z: -1}}},
		[]Mode{ModeTop}, 10, 10, -64, 320)
	if len(affected) != 1 {
		t.Fatalf("expected 1 tile, got %d", len(affected))
	}
	if affected[0].X != -1 || affected[0].Y != -1 {
		t.Errorf("chunk -1,-1 mapped to tile %d,%d, want -1,-1", affected[0].X, affected[0].Y)
	}
}

// An isometric tile shows columns from far outside its own ground footprint, so
// a changed chunk must dirty every tile its projected sprite can reach. Dirtying
// only the tile containing the chunk would leave stale mountains behind.
func TestAffectedIsoTilesExceedTheGroundFootprint(t *testing.T) {
	chunk := DirtyChunk{Dimension: "d", Pos: mcmath.ChunkPos{X: 10, Z: 10}}

	iso := AffectedTiles([]DirtyChunk{chunk}, []Mode{ModeIso}, 8, 8, -64, 320)
	top := AffectedTiles([]DirtyChunk{chunk}, []Mode{ModeTop}, 8, 8, -64, 320)

	if len(iso) <= len(top) {
		t.Errorf("isometric dirtied %d tiles and top-down %d; isometric must dirty more "+
			"because elevation spreads a chunk across neighbouring tiles", len(iso), len(top))
	}

	// Every tile a column of this chunk could paint into must be present.
	proj := mcmath.NewIsoProjection(mcmath.DefaultCamera)
	present := map[TileID]bool{}
	for _, id := range iso {
		present[id] = true
	}
	bounds := mcmath.ChunkBounds(chunk.Pos)
	checked := 0
	for x := bounds.MinX; x < bounds.MaxX; x += 5 {
		for z := bounds.MinZ; z < bounds.MaxZ; z += 5 {
			for _, y := range []int{-64, 0, 128, 320} {
				u, v := proj.ProjectBlockTop(x, y, z)
				tp := mcmath.IsoTileAt(u, v, 8)
				checked++
				if !present[(TileID{Dimension: "d", Mode: ModeIso, Zoom: 8, X: tp.X, Y: tp.Y})] {
					t.Fatalf("column (%d,%d) at y=%d paints into iso tile %d,%d "+
						"which was not marked dirty", x, z, y, tp.X, tp.Y)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no columns were checked; the test proves nothing")
	}
	t.Logf("isometric dirtied %d tiles vs %d top-down; verified %d projected columns",
		len(iso), len(top), checked)
}

func TestAffectedTilesAcrossBothModes(t *testing.T) {
	chunk := DirtyChunk{Dimension: "d", Pos: mcmath.ChunkPos{X: 3, Z: 4}}
	both := AffectedTiles([]DirtyChunk{chunk}, []Mode{ModeTop, ModeIso}, 8, 8, 0, 128)

	modes := map[Mode]int{}
	for _, id := range both {
		modes[id.Mode]++
	}
	if modes[ModeTop] == 0 || modes[ModeIso] == 0 {
		t.Errorf("both modes must be dirtied, got %+v", modes)
	}
}

// ---------------------------------------------------------------------------
// Revisions
// ---------------------------------------------------------------------------

func TestRevisionsIncreaseMonotonically(t *testing.T) {
	r := NewRevisions()
	seen := map[uint64]bool{}
	last := uint64(0)
	for i := 0; i < 200; i++ {
		v := r.Next()
		if seen[v] {
			t.Fatalf("revision %d issued twice", v)
		}
		if v <= last && last != 0 {
			t.Fatalf("revision went backwards: %d after %d", v, last)
		}
		seen[v] = true
		last = v
	}
}

func TestRevisionsStartAheadOfWallClock(t *testing.T) {
	// Seeding from the clock means a restart never reissues a revision a client
	// already has cached against different content.
	r := NewRevisions()
	if r.Current() < uint64(time.Now().Unix())-5 {
		t.Errorf("revision counter %d is not seeded from the clock", r.Current())
	}
}

func TestTileRevisionOverridesBaseline(t *testing.T) {
	r := NewRevisions()
	id := TileID{Dimension: "d", Mode: ModeTop, Zoom: 6, X: 1, Y: -2}

	base := r.Baseline("d")
	if got := r.For(id); got != base {
		t.Errorf("an untouched tile should use the dimension baseline: %d vs %d", got, base)
	}

	bumped := r.Bump(id)
	if bumped <= base {
		t.Errorf("bumped revision %d is not newer than baseline %d", bumped, base)
	}
	if got := r.For(id); got != bumped {
		t.Errorf("For returned %d after bumping to %d", got, bumped)
	}

	// A sibling tile must be unaffected: that is the whole point of per-tile
	// revisions rather than invalidating the entire dimension.
	sibling := TileID{Dimension: "d", Mode: ModeTop, Zoom: 6, X: 2, Y: -2}
	if got := r.For(sibling); got != base {
		t.Errorf("sibling tile revision changed to %d; it should still be %d", got, base)
	}
}

func TestBumpDimensionInvalidatesEverythingInIt(t *testing.T) {
	r := NewRevisions()
	a := TileID{Dimension: "d1", Mode: ModeTop, Zoom: 6, X: 1, Y: 1}
	b := TileID{Dimension: "d2", Mode: ModeTop, Zoom: 6, X: 1, Y: 1}

	r.Bump(a)
	r.Bump(b)
	beforeOther := r.For(b)

	newBase := r.BumpDimension("d1")
	if got := r.For(a); got != newBase {
		t.Errorf("tile in the bumped dimension has revision %d, want the new baseline %d", got, newBase)
	}
	if got := r.For(b); got != beforeOther {
		t.Errorf("a tile in another dimension changed from %d to %d", beforeOther, got)
	}
}

func TestChangedListsOnlyExplicitBumps(t *testing.T) {
	r := NewRevisions()
	id := TileID{Dimension: "d", Mode: ModeIso, Zoom: 8, X: -4, Y: 7}
	r.Bump(id)

	changed := r.Changed("d")
	if len(changed) != 1 {
		t.Fatalf("Changed returned %d entries, want 1", len(changed))
	}
	if _, ok := changed[id]; !ok {
		t.Errorf("Changed is missing the bumped tile: %+v", changed)
	}
	if len(r.Changed("other")) != 0 {
		t.Error("Changed leaked tiles from another dimension")
	}
}

// ---------------------------------------------------------------------------
// Dirty set coalescing
// ---------------------------------------------------------------------------

func TestDirtySetCoalescesRepeatedMarks(t *testing.T) {
	d := NewDirtySet()
	pos := mcmath.ChunkPos{X: 1, Z: 2}
	// A player mining produces a burst of marks for one chunk.
	for i := 0; i < 500; i++ {
		d.Mark("dim", pos)
	}
	if got := d.Len(); got != 1 {
		t.Errorf("500 marks on one chunk produced %d entries, want 1", got)
	}
	drained := d.Drain(0)
	if len(drained) != 1 {
		t.Errorf("drained %d chunks, want 1", len(drained))
	}
	if d.Len() != 0 {
		t.Error("draining did not empty the set")
	}
}

// Chunks still being actively edited should be left for the next pass, so a
// mid-build player does not cause a regeneration storm.
func TestDirtySetHoldsBackActiveChunks(t *testing.T) {
	d := NewDirtySet()
	d.Mark("dim", mcmath.ChunkPos{X: 0, Z: 0})

	if got := d.Drain(time.Hour); len(got) != 0 {
		t.Errorf("a just-marked chunk was drained despite the settle window: %+v", got)
	}
	if d.Len() != 1 {
		t.Error("the held-back chunk was lost")
	}
	if got := d.Drain(0); len(got) != 1 {
		t.Errorf("with no settle window the chunk should drain, got %d", len(got))
	}
}

func TestDirtySetSeparatesDimensions(t *testing.T) {
	d := NewDirtySet()
	pos := mcmath.ChunkPos{X: 5, Z: 5}
	d.Mark("overworld", pos)
	d.Mark("nether", pos)

	if got := d.Len(); got != 2 {
		t.Errorf("the same chunk in two dimensions collapsed to %d entries", got)
	}
	drained := d.Drain(0)
	dims := map[string]bool{}
	for _, c := range drained {
		dims[c.Dimension] = true
	}
	if !dims["overworld"] || !dims["nether"] {
		t.Errorf("expected both dimensions, got %+v", dims)
	}
}

func TestParseModeAndFormat(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeTop, "top": ModeTop, "iso": ModeIso, "ISO": ModeIso} {
		if got, ok := ParseMode(in); !ok || got != want {
			t.Errorf("ParseMode(%q) = %v,%v want %v,true", in, got, ok, want)
		}
	}
	if _, ok := ParseMode("sideways"); ok {
		t.Error("an unknown mode should report not-ok")
	}
	for in, want := range map[string]Format{"": FormatWebP, "webp": FormatWebP, "png": FormatPNG, ".png": FormatPNG} {
		if got, ok := ParseFormat(in); !ok || got != want {
			t.Errorf("ParseFormat(%q) = %v,%v want %v,true", in, got, ok, want)
		}
	}
	if FormatWebP.ContentType() != "image/webp" || FormatPNG.ContentType() != "image/png" {
		t.Error("content types are wrong")
	}
}
