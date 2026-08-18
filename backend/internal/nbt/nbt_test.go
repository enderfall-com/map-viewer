package nbt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

// builder assembles NBT documents so tests can describe data declaratively
// instead of hand-writing byte slices.
type builder struct {
	buf bytes.Buffer
}

func (b *builder) u8(v byte)    { b.buf.WriteByte(v) }
func (b *builder) u16(v uint16) { binary.Write(&b.buf, binary.BigEndian, v) }
func (b *builder) u32(v uint32) { binary.Write(&b.buf, binary.BigEndian, v) }
func (b *builder) u64(v uint64) { binary.Write(&b.buf, binary.BigEndian, v) }
func (b *builder) str(s string) { b.u16(uint16(len(s))); b.buf.WriteString(s) }
func (b *builder) tag(t Type, name string) {
	b.u8(byte(t))
	b.str(name)
}
func (b *builder) end() { b.u8(byte(TagEnd)) }

func TestParseRejectsNonCompoundRoot(t *testing.T) {
	var b builder
	b.u8(byte(TagInt))
	b.str("x")
	b.u32(5)
	if _, _, err := Parse(b.buf.Bytes()); err == nil {
		t.Fatal("expected an error for a non-compound root")
	} else if !errors.Is(err, ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestParseAllScalarTypes(t *testing.T) {
	var b builder
	b.tag(TagCompound, "Root")

	b.tag(TagByte, "b")
	b.u8(0xFF) // -1 signed
	b.tag(TagShort, "s")
	b.u16(0xFFFE) // -2
	b.tag(TagInt, "i")
	b.u32(0xFFFFFFFD) // -3
	b.tag(TagLong, "l")
	b.u64(0xFFFFFFFFFFFFFFFC) // -4
	b.tag(TagFloat, "f")
	b.u32(math.Float32bits(1.5))
	b.tag(TagDouble, "d")
	b.u64(math.Float64bits(-2.25))
	b.tag(TagString, "str")
	b.str("hello world")
	b.end()

	root, name, err := Parse(b.buf.Bytes())
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if name != "Root" {
		t.Errorf("root name = %q, want Root", name)
	}

	// Signed interpretation matters: an unsigned read would turn section Y = -4
	// into 252 and place the whole bottom of the world in the wrong place.
	if got := root.GetInt(0, "b"); got != -1 {
		t.Errorf("byte = %d, want -1", got)
	}
	if got := root.GetInt(0, "s"); got != -2 {
		t.Errorf("short = %d, want -2", got)
	}
	if got := root.GetInt(0, "i"); got != -3 {
		t.Errorf("int = %d, want -3", got)
	}
	if got := root.GetInt(0, "l"); got != -4 {
		t.Errorf("long = %d, want -4", got)
	}
	if v, _ := root.Get("f"); v.Float != 1.5 {
		t.Errorf("float = %v, want 1.5", v.Float)
	}
	if v, _ := root.Get("d"); v.Float != -2.25 {
		t.Errorf("double = %v, want -2.25", v.Float)
	}
	if got := root.GetString("", "str"); got != "hello world" {
		t.Errorf("string = %q", got)
	}
}

func TestParseArrays(t *testing.T) {
	var b builder
	b.tag(TagCompound, "")

	b.tag(TagByteArray, "bytes")
	b.u32(3)
	b.buf.Write([]byte{1, 2, 3})

	b.tag(TagIntArray, "ints")
	b.u32(2)
	b.u32(0xFFFFFFFF) // -1
	b.u32(7)

	b.tag(TagLongArray, "longs")
	b.u32(2)
	b.u64(0x0123456789ABCDEF)
	b.u64(0xFFFFFFFFFFFFFFFF)
	b.end()

	root, _, err := Parse(b.buf.Bytes())
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if v, _ := root.Get("bytes"); !bytes.Equal(v.Bytes, []byte{1, 2, 3}) {
		t.Errorf("bytes = %v", v.Bytes)
	}
	if v, _ := root.Get("ints"); len(v.Ints) != 2 || v.Ints[0] != -1 || v.Ints[1] != 7 {
		t.Errorf("ints = %v", v.Ints)
	}
	longs := root.GetLongs("longs")
	if len(longs) != 2 || longs[0] != 0x0123456789ABCDEF || longs[1] != -1 {
		t.Errorf("longs = %v", longs)
	}
}

func TestParseNestedListsAndCompounds(t *testing.T) {
	// Shaped like a real section palette: a list of compounds each with a Name.
	var b builder
	b.tag(TagCompound, "")
	b.tag(TagList, "palette")
	b.u8(byte(TagCompound))
	b.u32(2)

	b.tag(TagString, "Name")
	b.str("minecraft:stone")
	b.end()

	b.tag(TagString, "Name")
	b.str("minecraft:dirt")
	b.tag(TagCompound, "Properties")
	b.tag(TagString, "waterlogged")
	b.str("true")
	b.end()
	b.end()

	b.end()

	root, _, err := Parse(b.buf.Bytes())
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	pal := root.GetList("palette")
	if len(pal) != 2 {
		t.Fatalf("palette has %d entries, want 2", len(pal))
	}
	if got := pal[0].GetString("", "Name"); got != "minecraft:stone" {
		t.Errorf("entry 0 name = %q", got)
	}
	if got := pal[1].GetString("", "Properties", "waterlogged"); got != "true" {
		t.Errorf("waterlogged = %q, want true", got)
	}
}

func TestParseEmptyList(t *testing.T) {
	// An empty list is written with element type End and count 0, which must be
	// accepted rather than treated as corruption.
	var b builder
	b.tag(TagCompound, "")
	b.tag(TagList, "empty")
	b.u8(byte(TagEnd))
	b.u32(0)
	b.end()

	root, _, err := Parse(b.buf.Bytes())
	if err != nil {
		t.Fatalf("empty list rejected: %v", err)
	}
	if l := root.GetList("empty"); len(l) != 0 {
		t.Errorf("expected empty list, got %v", l)
	}
}

func TestParseTruncatedDataFailsCleanly(t *testing.T) {
	var b builder
	b.tag(TagCompound, "")
	b.tag(TagIntArray, "ints")
	b.u32(1000) // claims 1000 entries but supplies none
	full := b.buf.Bytes()

	// A truncated chunk must produce an error, never a panic or a silent
	// half-decoded tree; one bad chunk should cost one chunk.
	if _, _, err := Parse(full); err == nil {
		t.Fatal("expected an error for truncated data")
	}

	for cut := 1; cut < len(full); cut++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic parsing %d-byte prefix: %v", cut, r)
				}
			}()
			_, _, _ = Parse(full[:cut])
		}()
	}
}

func TestParseRejectsAbsurdArrayLength(t *testing.T) {
	var b builder
	b.tag(TagCompound, "")
	b.tag(TagByteArray, "huge")
	b.u32(0x7FFFFFFF)
	if _, _, err := Parse(b.buf.Bytes()); err == nil {
		t.Fatal("expected a length-bound error")
	}
}

func TestParseRejectsNegativeArrayLength(t *testing.T) {
	var b builder
	b.tag(TagCompound, "")
	b.tag(TagIntArray, "neg")
	b.u32(0xFFFFFFFF) // -1 as int32
	if _, _, err := Parse(b.buf.Bytes()); err == nil {
		t.Fatal("expected an error for a negative array length")
	}
}

func TestGetAnyPrefersFirstPresentKey(t *testing.T) {
	// This is how the chunk decoder reads a field that was renamed between
	// world versions.
	var b builder
	b.tag(TagCompound, "")
	b.tag(TagInt, "Sections")
	b.u32(1)
	b.end()

	root, _, err := Parse(b.buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := root.GetAny("sections", "Sections"); !ok || v.Int(0) != 1 {
		t.Errorf("GetAny did not find the legacy key: %v %v", v, ok)
	}
	if _, ok := root.GetAny("nope", "also_nope"); ok {
		t.Error("GetAny reported a key that does not exist")
	}
}

func TestGetMissingPathIsSafe(t *testing.T) {
	var b builder
	b.tag(TagCompound, "")
	b.end()
	root, _, err := Parse(b.buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := root.Get("a", "b", "c"); ok {
		t.Error("expected a missing deep path to report not-found")
	}
	if got := root.GetInt(42, "missing"); got != 42 {
		t.Errorf("default not returned: %d", got)
	}
	if got := root.GetString("fallback", "missing", "deeper"); got != "fallback" {
		t.Errorf("default not returned: %q", got)
	}
	if root.GetLongs("missing") != nil {
		t.Error("expected nil longs for a missing key")
	}
}
