// Package nbt reads Minecraft's Named Binary Tag format.
//
// The parser is deliberately generic rather than schema-driven. Chunk NBT has
// changed shape repeatedly across Minecraft versions -- sections moved out of a
// "Level" wrapper, biomes became palettised, block state packing changed -- and
// modded worlds add arbitrary extra tags. A tree the caller navigates by name
// absorbs all of that; a struct-mapped decoder would need a new schema for
// every world version and would fail closed on unfamiliar modded data.
package nbt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Type is an NBT tag type id.
type Type byte

// The NBT tag types, in specification order.
const (
	TagEnd Type = iota
	TagByte
	TagShort
	TagInt
	TagLong
	TagFloat
	TagDouble
	TagByteArray
	TagString
	TagList
	TagCompound
	TagIntArray
	TagLongArray
)

// String names a tag type for diagnostics.
func (t Type) String() string {
	names := [...]string{
		"End", "Byte", "Short", "Int", "Long", "Float", "Double",
		"ByteArray", "String", "List", "Compound", "IntArray", "LongArray",
	}
	if int(t) < len(names) {
		return names[t]
	}
	return fmt.Sprintf("Unknown(%d)", byte(t))
}

// Tag is a decoded NBT value.
//
// Only the fields relevant to Type carry meaning. Integral types share Num so
// that reading a value written as a Byte in one world version and an Int in
// another needs no special-casing at the call site.
type Tag struct {
	Type Type

	Num   int64   // Byte, Short, Int, Long
	Float float64 // Float, Double
	Str   string

	Bytes []byte
	Ints  []int32
	Longs []int64

	List     []Tag
	ListType Type

	Compound map[string]Tag
}

// ErrMalformed reports structurally invalid NBT.
var ErrMalformed = errors.New("malformed NBT")

// maxArrayLen bounds any single array, so a corrupt length field cannot make
// the decoder allocate gigabytes. Real chunk arrays are a few thousand entries.
const maxArrayLen = 1 << 24

// maxDepth bounds nesting, so a hostile or corrupt file cannot cause unbounded
// recursion.
const maxDepth = 512

// reader wraps a byte slice with big-endian primitive reads. Working from a
// slice rather than an io.Reader avoids a per-value interface call and lets
// arrays alias the buffer where safe.
type reader struct {
	buf []byte
	pos int
}

func (r *reader) need(n int) error {
	if n < 0 || r.pos+n > len(r.buf) {
		return fmt.Errorf("%w: need %d bytes at offset %d of %d", ErrMalformed, n, r.pos, len(r.buf))
	}
	return nil
}

func (r *reader) u8() (byte, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	v := r.buf[r.pos]
	r.pos++
	return v, nil
}

func (r *reader) u16() (uint16, error) {
	if err := r.need(2); err != nil {
		return 0, err
	}
	v := binary.BigEndian.Uint16(r.buf[r.pos:])
	r.pos += 2
	return v, nil
}

func (r *reader) u32() (uint32, error) {
	if err := r.need(4); err != nil {
		return 0, err
	}
	v := binary.BigEndian.Uint32(r.buf[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *reader) u64() (uint64, error) {
	if err := r.need(8); err != nil {
		return 0, err
	}
	v := binary.BigEndian.Uint64(r.buf[r.pos:])
	r.pos += 8
	return v, nil
}

func (r *reader) str() (string, error) {
	n, err := r.u16()
	if err != nil {
		return "", err
	}
	if err := r.need(int(n)); err != nil {
		return "", err
	}
	s := string(r.buf[r.pos : r.pos+int(n)])
	r.pos += int(n)
	return s, nil
}

// Parse decodes a complete NBT document and returns the root tag and its name.
//
// The root of a chunk is an unnamed or empty-named compound; the name is
// returned rather than discarded because some region formats rely on it.
func Parse(data []byte) (root Tag, name string, err error) {
	r := &reader{buf: data}

	t, err := r.u8()
	if err != nil {
		return Tag{}, "", err
	}
	if Type(t) == TagEnd {
		return Tag{Type: TagEnd}, "", nil
	}
	if Type(t) != TagCompound {
		// Every real NBT document has a compound root; anything else means the
		// data is not NBT, most often a decompression failure upstream.
		return Tag{}, "", fmt.Errorf("%w: root tag is %s, expected Compound", ErrMalformed, Type(t))
	}
	name, err = r.str()
	if err != nil {
		return Tag{}, "", err
	}
	root, err = readPayload(r, TagCompound, 0)
	if err != nil {
		return Tag{}, "", err
	}
	return root, name, nil
}

// readPayload reads a tag body of a known type.
func readPayload(r *reader, t Type, depth int) (Tag, error) {
	if depth > maxDepth {
		return Tag{}, fmt.Errorf("%w: nesting deeper than %d", ErrMalformed, maxDepth)
	}
	switch t {
	case TagByte:
		v, err := r.u8()
		// NBT bytes are signed.
		return Tag{Type: t, Num: int64(int8(v))}, err

	case TagShort:
		v, err := r.u16()
		return Tag{Type: t, Num: int64(int16(v))}, err

	case TagInt:
		v, err := r.u32()
		return Tag{Type: t, Num: int64(int32(v))}, err

	case TagLong:
		v, err := r.u64()
		return Tag{Type: t, Num: int64(v)}, err

	case TagFloat:
		v, err := r.u32()
		return Tag{Type: t, Float: float64(math.Float32frombits(v))}, err

	case TagDouble:
		v, err := r.u64()
		return Tag{Type: t, Float: math.Float64frombits(v)}, err

	case TagString:
		s, err := r.str()
		return Tag{Type: t, Str: s}, err

	case TagByteArray:
		n, err := r.u32()
		if err != nil {
			return Tag{}, err
		}
		if int(int32(n)) < 0 || int(n) > maxArrayLen {
			return Tag{}, fmt.Errorf("%w: byte array length %d", ErrMalformed, int32(n))
		}
		if err := r.need(int(n)); err != nil {
			return Tag{}, err
		}
		out := make([]byte, n)
		copy(out, r.buf[r.pos:r.pos+int(n)])
		r.pos += int(n)
		return Tag{Type: t, Bytes: out}, nil

	case TagIntArray:
		n, err := r.u32()
		if err != nil {
			return Tag{}, err
		}
		if int(int32(n)) < 0 || int(n) > maxArrayLen {
			return Tag{}, fmt.Errorf("%w: int array length %d", ErrMalformed, int32(n))
		}
		if err := r.need(int(n) * 4); err != nil {
			return Tag{}, err
		}
		out := make([]int32, n)
		for i := range out {
			out[i] = int32(binary.BigEndian.Uint32(r.buf[r.pos+i*4:]))
		}
		r.pos += int(n) * 4
		return Tag{Type: t, Ints: out}, nil

	case TagLongArray:
		n, err := r.u32()
		if err != nil {
			return Tag{}, err
		}
		if int(int32(n)) < 0 || int(n) > maxArrayLen {
			return Tag{}, fmt.Errorf("%w: long array length %d", ErrMalformed, int32(n))
		}
		if err := r.need(int(n) * 8); err != nil {
			return Tag{}, err
		}
		out := make([]int64, n)
		for i := range out {
			out[i] = int64(binary.BigEndian.Uint64(r.buf[r.pos+i*8:]))
		}
		r.pos += int(n) * 8
		return Tag{Type: t, Longs: out}, nil

	case TagList:
		et, err := r.u8()
		if err != nil {
			return Tag{}, err
		}
		n, err := r.u32()
		if err != nil {
			return Tag{}, err
		}
		count := int(int32(n))
		if count < 0 {
			count = 0 // some writers emit -1 for an empty list
		}
		if count > maxArrayLen {
			return Tag{}, fmt.Errorf("%w: list length %d", ErrMalformed, count)
		}
		elemType := Type(et)
		// A list of End with a non-zero count is malformed; a zero count is the
		// canonical empty list and is fine.
		if elemType == TagEnd && count > 0 {
			return Tag{}, fmt.Errorf("%w: list of End with %d entries", ErrMalformed, count)
		}
		items := make([]Tag, 0, min(count, 4096))
		for i := 0; i < count; i++ {
			item, err := readPayload(r, elemType, depth+1)
			if err != nil {
				return Tag{}, err
			}
			items = append(items, item)
		}
		return Tag{Type: t, ListType: elemType, List: items}, nil

	case TagCompound:
		m := make(map[string]Tag, 8)
		for {
			ct, err := r.u8()
			if err != nil {
				return Tag{}, err
			}
			if Type(ct) == TagEnd {
				return Tag{Type: TagCompound, Compound: m}, nil
			}
			key, err := r.str()
			if err != nil {
				return Tag{}, err
			}
			val, err := readPayload(r, Type(ct), depth+1)
			if err != nil {
				return Tag{}, fmt.Errorf("in %q: %w", key, err)
			}
			m[key] = val
		}

	default:
		return Tag{}, fmt.Errorf("%w: unknown tag type %d", ErrMalformed, byte(t))
	}
}

// ---------------------------------------------------------------------------
// Navigation helpers
// ---------------------------------------------------------------------------

// Get walks a path of compound keys. A missing key returns a zero Tag and
// false, so callers can probe for optional and version-dependent fields without
// error handling at every step.
func (t Tag) Get(path ...string) (Tag, bool) {
	cur := t
	for _, key := range path {
		if cur.Type != TagCompound || cur.Compound == nil {
			return Tag{}, false
		}
		next, ok := cur.Compound[key]
		if !ok {
			return Tag{}, false
		}
		cur = next
	}
	return cur, true
}

// GetAny returns the first of several keys that exists, which is how the same
// field is read across world versions that renamed it (for example "sections"
// versus "Sections").
func (t Tag) GetAny(keys ...string) (Tag, bool) {
	if t.Type != TagCompound || t.Compound == nil {
		return Tag{}, false
	}
	for _, k := range keys {
		if v, ok := t.Compound[k]; ok {
			return v, true
		}
	}
	return Tag{}, false
}

// Int returns an integral tag's value, or def.
func (t Tag) Int(def int64) int64 {
	switch t.Type {
	case TagByte, TagShort, TagInt, TagLong:
		return t.Num
	case TagFloat, TagDouble:
		return int64(t.Float)
	}
	return def
}

// String returns a string tag's value, or def.
func (t Tag) String_(def string) string {
	if t.Type == TagString {
		return t.Str
	}
	return def
}

// GetInt reads an integral value at a path.
func (t Tag) GetInt(def int64, path ...string) int64 {
	v, ok := t.Get(path...)
	if !ok {
		return def
	}
	return v.Int(def)
}

// GetString reads a string value at a path.
func (t Tag) GetString(def string, path ...string) string {
	v, ok := t.Get(path...)
	if !ok {
		return def
	}
	return v.String_(def)
}

// GetLongs reads a long array at a path.
func (t Tag) GetLongs(path ...string) []int64 {
	v, ok := t.Get(path...)
	if !ok || v.Type != TagLongArray {
		return nil
	}
	return v.Longs
}

// GetList reads a list at a path.
func (t Tag) GetList(path ...string) []Tag {
	v, ok := t.Get(path...)
	if !ok || v.Type != TagList {
		return nil
	}
	return v.List
}

// Keys lists a compound's keys, for diagnostics when an unfamiliar world layout
// needs investigating.
func (t Tag) Keys() []string {
	if t.Type != TagCompound {
		return nil
	}
	out := make([]string, 0, len(t.Compound))
	for k := range t.Compound {
		out = append(out, k)
	}
	return out
}

// ParseReader decodes NBT from a reader, for callers that do not already have
// the bytes in memory.
func ParseReader(r io.Reader) (Tag, string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxArrayLen))
	if err != nil {
		return Tag{}, "", err
	}
	return Parse(data)
}
