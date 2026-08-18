package mcworld

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/enderfall/minecraft-map/backend/internal/blocks"
	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/nbt"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

// ---------------------------------------------------------------------------
// Bit packing
// ---------------------------------------------------------------------------

// The two packings differ only for palettes whose bit width does not divide 64.
// Reading a world with the wrong one yields plausible-looking but entirely wrong
// terrain, so both are pinned here.
func TestPackedNonSpanning(t *testing.T) {
	// 5 bits per entry, 12 entries per long, high 4 bits padding.
	var word uint64
	values := []int{1, 2, 3, 31, 0, 17}
	for i, v := range values {
		word |= uint64(v) << (i * 5)
	}
	p := newPacked([]int64{int64(word)}, 5, false)
	for i, want := range values {
		if got := p.at(i); got != want {
			t.Errorf("entry %d = %d, want %d", i, got, want)
		}
	}
}

func TestPackedSpanningCrossesLongBoundary(t *testing.T) {
	// 5 bits per entry packed tightly: entry 12 straddles the two longs.
	values := make([]int, 20)
	for i := range values {
		values[i] = (i * 7) % 32
	}
	bitLen := len(values) * 5
	words := make([]int64, (bitLen+63)/64)
	for i, v := range values {
		bit := i * 5
		idx := bit / 64
		off := bit % 64
		words[idx] |= int64(uint64(v) << off)
		if off+5 > 64 && idx+1 < len(words) {
			words[idx+1] |= int64(uint64(v) >> (64 - off))
		}
	}
	p := newPacked(words, 5, true)
	for i, want := range values {
		if got := p.at(i); got != want {
			t.Errorf("spanning entry %d = %d, want %d", i, got, want)
		}
	}
	// Entry 12 begins at bit 60 and genuinely crosses the boundary; without
	// spanning support it would decode as a truncated value.
	if p.at(12) != values[12] {
		t.Errorf("boundary-crossing entry decoded incorrectly")
	}
}

func TestPackedOutOfRangeIsSafe(t *testing.T) {
	p := newPacked([]int64{0}, 4, false)
	if got := p.at(10_000); got != 0 {
		t.Errorf("out-of-range read = %d, want 0", got)
	}
	if got := p.at(-1); got != 0 {
		t.Errorf("negative read = %d, want 0", got)
	}
	empty := newPacked(nil, 4, false)
	if got := empty.at(0); got != 0 {
		t.Errorf("empty read = %d, want 0", got)
	}
}

func TestBitsNeeded(t *testing.T) {
	cases := map[int]int{0: 1, 1: 1, 2: 1, 3: 2, 4: 2, 5: 3, 8: 3, 9: 4, 16: 4, 17: 5, 256: 8}
	for n, want := range cases {
		if got := bitsNeeded(n); got != want {
			t.Errorf("bitsNeeded(%d) = %d, want %d", n, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Synthetic world construction
// ---------------------------------------------------------------------------

// nbtWriter builds NBT documents for the synthetic world.
type nbtWriter struct{ buf bytes.Buffer }

func (w *nbtWriter) u8(v byte)    { w.buf.WriteByte(v) }
func (w *nbtWriter) u16(v uint16) { binary.Write(&w.buf, binary.BigEndian, v) }
func (w *nbtWriter) u32(v uint32) { binary.Write(&w.buf, binary.BigEndian, v) }
func (w *nbtWriter) u64(v uint64) { binary.Write(&w.buf, binary.BigEndian, v) }
func (w *nbtWriter) str(s string) { w.u16(uint16(len(s))); w.buf.WriteString(s) }
func (w *nbtWriter) tag(t nbt.Type, name string) {
	w.u8(byte(t))
	w.str(name)
}
func (w *nbtWriter) end() { w.u8(byte(nbt.TagEnd)) }

// sectionSpec describes one section of the synthetic chunk.
type sectionSpec struct {
	y       int
	palette []string
	// blockAt returns a palette index for a local position.
	blockAt func(x, y, z int) int
	biome   string
}

// writeSection emits a modern (1.18+) section.
func writeSection(w *nbtWriter, s sectionSpec) {
	w.tag(nbt.TagCompound, "")
	w.tag(nbt.TagByte, "Y")
	w.u8(byte(int8(s.y)))

	w.tag(nbt.TagCompound, "block_states")
	w.tag(nbt.TagList, "palette")
	w.u8(byte(nbt.TagCompound))
	w.u32(uint32(len(s.palette)))
	for _, name := range s.palette {
		w.tag(nbt.TagString, "Name")
		w.str(name)
		w.end()
	}

	if len(s.palette) > 1 {
		// Modern packing: minimum 4 bits, entries never span longs.
		bits := bitsNeeded(len(s.palette))
		if bits < 4 {
			bits = 4
		}
		perLong := 64 / bits
		total := 4096
		words := make([]uint64, (total+perLong-1)/perLong)
		for i := 0; i < total; i++ {
			x := i % 16
			z := (i / 16) % 16
			y := i / 256
			v := uint64(s.blockAt(x, y, z))
			words[i/perLong] |= v << ((i % perLong) * bits)
		}
		w.tag(nbt.TagLongArray, "data")
		w.u32(uint32(len(words)))
		for _, v := range words {
			w.u64(v)
		}
	}
	w.end() // block_states

	w.tag(nbt.TagCompound, "biomes")
	w.tag(nbt.TagList, "palette")
	w.u8(byte(nbt.TagString))
	w.u32(1)
	w.str(s.biome)
	w.end() // biomes
	w.end() // section
}

// buildChunkNBT writes a complete modern chunk.
func buildChunkNBT(cx, cz int, sections []sectionSpec) []byte {
	var w nbtWriter
	w.tag(nbt.TagCompound, "")

	w.tag(nbt.TagInt, "DataVersion")
	w.u32(3465) // 1.20.1
	w.tag(nbt.TagInt, "xPos")
	w.u32(uint32(int32(cx)))
	w.tag(nbt.TagInt, "zPos")
	w.u32(uint32(int32(cz)))
	w.tag(nbt.TagString, "Status")
	w.str("minecraft:full")

	w.tag(nbt.TagList, "sections")
	w.u8(byte(nbt.TagCompound))
	w.u32(uint32(len(sections)))
	for _, s := range sections {
		// The list element payload has no tag header or name.
		var inner nbtWriter
		writeSection(&inner, s)
		// writeSection wrote a tag header; strip it (1 byte type + 2 byte name len).
		raw := inner.buf.Bytes()
		w.buf.Write(raw[3:])
	}
	w.end() // root
	return w.buf.Bytes()
}

// writeRegion assembles a valid .mca file containing the given chunks.
func writeRegion(t *testing.T, path string, chunks map[mcmath.ChunkPos][]byte) {
	t.Helper()

	const sector = 4096
	header := make([]byte, sector*2)
	var body bytes.Buffer
	nextSector := 2

	for pos, raw := range chunks {
		var compressed bytes.Buffer
		zw := zlib.NewWriter(&compressed)
		if _, err := zw.Write(raw); err != nil {
			t.Fatal(err)
		}
		zw.Close()

		payload := compressed.Bytes()
		var entry bytes.Buffer
		binary.Write(&entry, binary.BigEndian, uint32(len(payload)+1))
		entry.WriteByte(2) // zlib
		entry.Write(payload)

		// Pad to a whole number of sectors.
		pad := (sector - entry.Len()%sector) % sector
		entry.Write(make([]byte, pad))
		sectors := entry.Len() / sector

		slot := mcmath.FloorMod(pos.X, 32) + mcmath.FloorMod(pos.Z, 32)*32
		loc := uint32(nextSector)<<8 | uint32(sectors)
		binary.BigEndian.PutUint32(header[slot*4:], loc)
		binary.BigEndian.PutUint32(header[sector+slot*4:], 1700000000)

		body.Write(entry.Bytes())
		nextSector += sectors
	}

	out := append(header, body.Bytes()...)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildTestWorld creates a world directory with one region file.
//
// Terrain: stone up to y=59, dirt 60..62, grass at 63, and water filling
// 64..66 over the western half, so the surface scan, water handling and
// biome lookup are all exercised against known ground truth.
func buildTestWorld(t *testing.T, dir string, positions []mcmath.ChunkPos) {
	t.Helper()
	regionDir := filepath.Join(dir, "region")
	if err := os.MkdirAll(regionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	palette := []string{
		"minecraft:air",         // 0
		"minecraft:stone",       // 1
		"minecraft:dirt",        // 2
		"minecraft:grass_block", // 3
		"minecraft:water",       // 4
	}
	blockAt := func(sectionY int) func(x, y, z int) int {
		return func(x, ly, z int) int {
			worldY := sectionY*16 + ly
			switch {
			case worldY <= 59:
				return 1
			case worldY <= 62:
				return 2
			case worldY == 63:
				return 3
			case worldY <= 66 && x < 8:
				return 4 // water over the western half
			default:
				return 0
			}
		}
	}

	byRegion := map[mcmath.RegionPos]map[mcmath.ChunkPos][]byte{}
	for _, pos := range positions {
		sections := []sectionSpec{
			{y: 3, palette: palette, blockAt: blockAt(3), biome: "minecraft:plains"},
			{y: 4, palette: palette, blockAt: blockAt(4), biome: "minecraft:plains"},
		}
		rp := pos.Region()
		if byRegion[rp] == nil {
			byRegion[rp] = map[mcmath.ChunkPos][]byte{}
		}
		byRegion[rp][pos] = buildChunkNBT(pos.X, pos.Z, sections)
	}
	for rp, chunks := range byRegion {
		writeRegion(t, filepath.Join(regionDir, regionFileName(rp)), chunks)
	}
}

func regionFileName(p mcmath.RegionPos) string {
	return "r." + itoa(p.X) + "." + itoa(p.Z) + ".mca"
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func openTestWorld(t *testing.T, dir string) *World {
	t.Helper()
	reg, err := blocks.NewDefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	bio, err := blocks.NewDefaultBiomes()
	if err != nil {
		t.Fatal(err)
	}
	w, err := Open(Options{
		Path:   dir,
		Blocks: reg,
		Biomes: bio,
		Log:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	if err != nil {
		t.Fatalf("open world: %v", err)
	}
	return w
}

// ---------------------------------------------------------------------------
// End-to-end decoding
// ---------------------------------------------------------------------------

func TestReadsSyntheticWorld(t *testing.T) {
	dir := t.TempDir()
	// Chunks on both sides of the origin, which is where sign errors surface.
	positions := []mcmath.ChunkPos{{X: 0, Z: 0}, {X: 1, Z: 0}, {X: -1, Z: -1}, {X: -2, Z: 3}}
	buildTestWorld(t, dir, positions)

	w := openTestWorld(t, dir)
	defer w.Close()

	ctx := context.Background()
	dims, err := w.Dimensions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dims) != 1 || dims[0].ID != "minecraft:overworld" {
		t.Fatalf("dimensions = %+v, want one overworld", dims)
	}

	reg, _ := blocks.NewDefaultRegistry()
	grassID := reg.ID("minecraft:grass_block")
	_ = grassID

	for _, pos := range positions {
		cs, err := w.ChunkSurface(ctx, "minecraft:overworld", pos)
		if err != nil {
			t.Fatalf("chunk %+v: %v", pos, err)
		}
		if cs == nil {
			t.Fatalf("chunk %+v returned nil", pos)
		}

		baseX := pos.MinBlockX()
		baseZ := pos.MinBlockZ()

		for _, probe := range []struct {
			lx, lz     int
			wantHeight int
			wantWater  bool
			wantWaterY int
			wantBlock  string
		}{
			// Eastern half: dry grass surface at y=63.
			{lx: 12, lz: 4, wantHeight: 63, wantBlock: "minecraft:grass_block"},
			{lx: 15, lz: 15, wantHeight: 63, wantBlock: "minecraft:grass_block"},
			// Western half: submerged, floor still grass, water top at y=66.
			{lx: 0, lz: 0, wantHeight: 63, wantWater: true, wantWaterY: 66, wantBlock: "minecraft:grass_block"},
			{lx: 7, lz: 9, wantHeight: 63, wantWater: true, wantWaterY: 66, wantBlock: "minecraft:grass_block"},
		} {
			col := cs.At(baseX+probe.lx, baseZ+probe.lz)
			if !col.Present() {
				t.Fatalf("chunk %+v local (%d,%d) has no data", pos, probe.lx, probe.lz)
			}
			if col.Height != probe.wantHeight {
				t.Errorf("chunk %+v local (%d,%d): height %d, want %d",
					pos, probe.lx, probe.lz, col.Height, probe.wantHeight)
			}
			if col.Water() != probe.wantWater {
				t.Errorf("chunk %+v local (%d,%d): water %v, want %v",
					pos, probe.lx, probe.lz, col.Water(), probe.wantWater)
			}
			if probe.wantWater && col.WaterY != probe.wantWaterY {
				t.Errorf("chunk %+v local (%d,%d): water surface %d, want %d",
					pos, probe.lx, probe.lz, col.WaterY, probe.wantWaterY)
			}
			blk := w.blocks.Get(col.Block)
			if blk.Name != probe.wantBlock {
				t.Errorf("chunk %+v local (%d,%d): block %s, want %s",
					pos, probe.lx, probe.lz, blk.Name, probe.wantBlock)
			}
			if bio := w.biomes.Get(col.Biome); bio.Name != "minecraft:plains" {
				t.Errorf("chunk %+v local (%d,%d): biome %s", pos, probe.lx, probe.lz, bio.Name)
			}
		}
	}
}

func TestWaterDepthIsComputed(t *testing.T) {
	dir := t.TempDir()
	buildTestWorld(t, dir, []mcmath.ChunkPos{{X: 0, Z: 0}})
	w := openTestWorld(t, dir)
	defer w.Close()

	cs, err := w.ChunkSurface(context.Background(), "minecraft:overworld", mcmath.ChunkPos{X: 0, Z: 0})
	if err != nil {
		t.Fatal(err)
	}
	col := cs.At(0, 0)
	// Water occupies 64..66 over a floor at 63, so the depth is 3.
	if got := col.WaterDepth(); got != 3 {
		t.Errorf("water depth = %d, want 3", got)
	}
	if got := col.RenderY(); got != 66 {
		t.Errorf("render elevation = %d, want the water surface 66", got)
	}
}

func TestAbsentChunkReportsNotGenerated(t *testing.T) {
	dir := t.TempDir()
	buildTestWorld(t, dir, []mcmath.ChunkPos{{X: 0, Z: 0}})
	w := openTestWorld(t, dir)
	defer w.Close()

	// A chunk slot inside an existing region file, but never written.
	_, err := w.ChunkSurface(context.Background(), "minecraft:overworld", mcmath.ChunkPos{X: 5, Z: 5})
	if err != world.ErrChunkAbsent {
		t.Errorf("expected ErrChunkAbsent, got %v", err)
	}
	// A chunk in a region file that does not exist at all.
	_, err = w.ChunkSurface(context.Background(), "minecraft:overworld", mcmath.ChunkPos{X: 500, Z: 500})
	if err != world.ErrChunkAbsent {
		t.Errorf("expected ErrChunkAbsent for a missing region, got %v", err)
	}
}

func TestHeightRangeProbedFromChunkData(t *testing.T) {
	dir := t.TempDir()
	buildTestWorld(t, dir, []mcmath.ChunkPos{{X: 0, Z: 0}})
	w := openTestWorld(t, dir)
	defer w.Close()

	d, ok := w.Dimension(context.Background(), "minecraft:overworld")
	if !ok {
		t.Fatal("overworld missing")
	}
	// Sections 3 and 4 cover y 48..79, which is what the probe should report
	// rather than the vanilla -64..320 default.
	if d.MinY != 48 || d.MaxY != 80 {
		t.Errorf("probed height range = %d..%d, want 48..80", d.MinY, d.MaxY)
	}
}

func TestCorruptRegionDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	buildTestWorld(t, dir, []mcmath.ChunkPos{{X: 0, Z: 0}})

	// Corrupt the payload while leaving the header intact, which is what a
	// partially-written or damaged region file looks like.
	path := filepath.Join(dir, "region", "r.0.0.mca")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 8192; i < len(raw) && i < 8300; i++ {
		raw[i] = 0xFF
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	w := openTestWorld(t, dir)
	defer w.Close()

	// The read must fail cleanly for that chunk rather than taking down the
	// renderer, which is what keeps one bad chunk from costing a whole tile.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("corrupt region caused a panic: %v", r)
		}
	}()
	if _, err := w.ChunkSurface(context.Background(), "minecraft:overworld", mcmath.ChunkPos{X: 0, Z: 0}); err == nil {
		t.Log("corrupt chunk still decoded; acceptable as long as it did not panic")
	}
}

func TestUnknownBlocksGetDeterministicFallback(t *testing.T) {
	dir := t.TempDir()
	regionDir := filepath.Join(dir, "region")
	if err := os.MkdirAll(regionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A palette full of modded blocks nothing has a mapping for.
	palette := []string{"air", "create:limestone", "bigmod:weird_ore", "othermod:strange_leaves"}
	spec := sectionSpec{
		y: 4, palette: palette, biome: "somemod:alien_biome",
		blockAt: func(x, ly, z int) int {
			if ly < 8 {
				return 1 + (x+z)%3
			}
			return 0
		},
	}
	pos := mcmath.ChunkPos{X: 0, Z: 0}
	writeRegion(t, filepath.Join(regionDir, "r.0.0.mca"), map[mcmath.ChunkPos][]byte{
		pos: buildChunkNBT(0, 0, []sectionSpec{spec}),
	})

	w := openTestWorld(t, dir)
	defer w.Close()

	cs, err := w.ChunkSurface(context.Background(), "minecraft:overworld", pos)
	if err != nil {
		t.Fatalf("modded chunk failed to decode: %v", err)
	}
	col := cs.At(0, 0)
	if !col.Present() {
		t.Fatal("modded chunk produced no surface")
	}
	blk := w.blocks.Get(col.Block)
	if blk.Known {
		t.Errorf("expected %s to be reported as unknown", blk.Name)
	}
	if blk.Color.A == 0 {
		t.Error("unknown block has a transparent fallback colour")
	}
	// The same identifier must produce the same colour on every server and
	// restart, or pyramid levels would disagree with each other.
	again := blocks.DeterministicColor(blk.Name)
	if again != blk.Color {
		t.Errorf("fallback colour is not deterministic: %v vs %v", again, blk.Color)
	}
	if len(w.blocks.Unknown(10)) == 0 {
		t.Error("unknown blocks were not recorded for reporting")
	}
	if len(w.biomes.Unknown(10)) == 0 {
		t.Error("unknown biomes were not recorded for reporting")
	}
}
