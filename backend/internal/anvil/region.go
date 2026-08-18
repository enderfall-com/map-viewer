// Package anvil reads Minecraft Java Edition region files (.mca).
//
// A region file holds a 32x32 grid of chunks in 4 KiB sectors, preceded by an
// 8 KiB header: 4 KiB of chunk locations and 4 KiB of timestamps. Each chunk
// payload is a length, a compression byte and a compressed NBT document.
package anvil

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/nbt"
)

const (
	// sectorSize is the allocation unit of a region file.
	sectorSize = 4096
	// headerSectors is the number of sectors the header occupies.
	headerSectors = 2
	// chunksPerRegion is the number of chunk slots in a region file.
	chunksPerRegion = mcmath.RegionChunks * mcmath.RegionChunks
)

// Compression schemes a chunk payload may use.
const (
	compressionGzip   = 1
	compressionZlib   = 2
	compressionNone   = 3
	compressionLZ4    = 4
	compressionCustom = 127
)

// ErrChunkAbsent reports that a chunk slot in a region file is empty. This is
// the normal state for unexplored chunks, not a failure.
var ErrChunkAbsent = errors.New("chunk not present in region")

// ErrUnsupportedCompression reports a compression scheme this reader cannot
// decode, most often LZ4 written by a mod.
var ErrUnsupportedCompression = errors.New("unsupported chunk compression")

// maxChunkPayload bounds a single chunk's compressed size, so a corrupt length
// field cannot drive an enormous allocation.
const maxChunkPayload = 32 << 20

// Region is an open region file.
//
// The whole header is read once at open time and kept in memory; chunk payloads
// are read on demand. Reads are safe for concurrent use because the file is
// only ever accessed through ReadAt, which does not share a file offset.
//
// A Region held in a shared cache can be evicted by one goroutine while
// another is mid-ReadChunk on it. Retire/Acquire/Release implement reference
// counting so eviction never closes the file out from under an in-flight
// read: Retire marks it for closing but defers the actual close until every
// Acquire has a matching Release.
type Region struct {
	Pos mcmath.RegionPos

	path string
	file *os.File

	// offsets and sizes are the decoded location table, indexed by chunk slot.
	offsets [chunksPerRegion]uint32
	sizes   [chunksPerRegion]uint8
	stamps  [chunksPerRegion]uint32

	size    int64
	modTime time.Time

	closed sync.Once

	refMu   sync.Mutex
	refs    int
	retired bool
}

// slotIndex returns the header slot for a chunk, using floor-mod so negative
// chunk coordinates land in the right slot.
func slotIndex(c mcmath.ChunkPos) int {
	return mcmath.FloorMod(c.X, mcmath.RegionChunks) +
		mcmath.FloorMod(c.Z, mcmath.RegionChunks)*mcmath.RegionChunks
}

// Open reads a region file's header.
func Open(path string, pos mcmath.RegionPos) (*Region, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	r := &Region{Pos: pos, path: path, file: f, size: st.Size(), modTime: st.ModTime()}

	// A file shorter than the header holds no chunks. Empty region files do
	// occur; treating them as an error would break scanning a live world.
	if st.Size() < sectorSize*headerSectors {
		if st.Size() != 0 {
			f.Close()
			return nil, fmt.Errorf("region %s is %d bytes, too short for a header", path, st.Size())
		}
		return r, nil
	}

	var header [sectorSize * headerSectors]byte
	if _, err := f.ReadAt(header[:], 0); err != nil && !errors.Is(err, io.EOF) {
		f.Close()
		return nil, fmt.Errorf("read region header %s: %w", path, err)
	}
	for i := 0; i < chunksPerRegion; i++ {
		loc := binary.BigEndian.Uint32(header[i*4:])
		r.offsets[i] = loc >> 8 // sector offset
		r.sizes[i] = uint8(loc) // sector count
		r.stamps[i] = binary.BigEndian.Uint32(header[sectorSize+i*4:])
	}
	return r, nil
}

// Path returns the file path.
func (r *Region) Path() string { return r.path }

// ModTime returns the file's modification time as seen at open.
func (r *Region) ModTime() time.Time { return r.modTime }

// Has reports whether a chunk slot is populated.
func (r *Region) Has(c mcmath.ChunkPos) bool {
	i := slotIndex(c)
	return r.offsets[i] != 0 && r.sizes[i] != 0
}

// Timestamp returns when a chunk was last written.
func (r *Region) Timestamp(c mcmath.ChunkPos) time.Time {
	i := slotIndex(c)
	if r.stamps[i] == 0 {
		return time.Time{}
	}
	return time.Unix(int64(r.stamps[i]), 0)
}

// ChunkCount reports how many chunk slots are populated.
func (r *Region) ChunkCount() int {
	n := 0
	for i := 0; i < chunksPerRegion; i++ {
		if r.offsets[i] != 0 && r.sizes[i] != 0 {
			n++
		}
	}
	return n
}

// ReadChunk returns a chunk's decoded NBT.
//
// Every failure is scoped to the single chunk: a bad length, an unknown
// compression scheme or corrupt compressed data all return an error for that
// chunk while leaving the rest of the region readable. That is what lets one
// damaged chunk cost a 16x16 blank patch instead of a whole tile.
func (r *Region) ReadChunk(c mcmath.ChunkPos) (nbt.Tag, error) {
	i := slotIndex(c)
	offset, sectors := r.offsets[i], r.sizes[i]
	if offset == 0 || sectors == 0 {
		return nbt.Tag{}, ErrChunkAbsent
	}

	start := int64(offset) * sectorSize
	span := int64(sectors) * sectorSize
	if start < sectorSize*headerSectors || start+span > r.size+sectorSize {
		return nbt.Tag{}, fmt.Errorf("chunk %d,%d: sector range %d..%d outside region of %d bytes",
			c.X, c.Z, start, start+span, r.size)
	}

	var lenBuf [5]byte
	if _, err := r.file.ReadAt(lenBuf[:], start); err != nil {
		return nbt.Tag{}, fmt.Errorf("chunk %d,%d: read header: %w", c.X, c.Z, err)
	}
	length := int64(binary.BigEndian.Uint32(lenBuf[:4]))
	scheme := lenBuf[4]

	if length < 1 || length > maxChunkPayload {
		return nbt.Tag{}, fmt.Errorf("chunk %d,%d: implausible payload length %d", c.X, c.Z, length)
	}
	payloadLen := length - 1 // the length includes the compression byte
	if start+5+payloadLen > r.size {
		return nbt.Tag{}, fmt.Errorf("chunk %d,%d: payload runs past end of file", c.X, c.Z)
	}

	payload := make([]byte, payloadLen)
	if _, err := r.file.ReadAt(payload, start+5); err != nil {
		return nbt.Tag{}, fmt.Errorf("chunk %d,%d: read payload: %w", c.X, c.Z, err)
	}

	// The high bit marks a chunk stored in an external .mcc file, used when a
	// chunk grows past what the sector table can address.
	if scheme&0x80 != 0 {
		external, err := r.readExternal(c)
		if err != nil {
			return nbt.Tag{}, err
		}
		payload = external
		scheme &^= 0x80
	}

	raw, err := decompress(scheme, payload)
	if err != nil {
		return nbt.Tag{}, fmt.Errorf("chunk %d,%d: %w", c.X, c.Z, err)
	}

	tag, _, err := nbt.Parse(raw)
	if err != nil {
		return nbt.Tag{}, fmt.Errorf("chunk %d,%d: %w", c.X, c.Z, err)
	}
	return tag, nil
}

// readExternal loads an oversized chunk stored beside the region file.
func (r *Region) readExternal(c mcmath.ChunkPos) ([]byte, error) {
	name := fmt.Sprintf("c.%d.%d.mcc", c.X, c.Z)
	p := filepath.Join(filepath.Dir(r.path), name)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("chunk %d,%d: read external %s: %w", c.X, c.Z, name, err)
	}
	if len(data) > maxChunkPayload {
		return nil, fmt.Errorf("chunk %d,%d: external chunk too large", c.X, c.Z)
	}
	return data, nil
}

// decompress inflates a chunk payload.
func decompress(scheme byte, payload []byte) ([]byte, error) {
	switch scheme {
	case compressionZlib:
		zr, err := zlib.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("zlib: %w", err)
		}
		defer zr.Close()
		return readAllLimited(zr)

	case compressionGzip:
		gr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer gr.Close()
		return readAllLimited(gr)

	case compressionNone:
		return payload, nil

	case compressionLZ4:
		return nil, fmt.Errorf("%w: LZ4 (scheme 4)", ErrUnsupportedCompression)

	case compressionCustom:
		return nil, fmt.Errorf("%w: custom (scheme 127)", ErrUnsupportedCompression)

	default:
		return nil, fmt.Errorf("%w: scheme %d", ErrUnsupportedCompression, scheme)
	}
}

// maxInflated bounds a decompressed chunk, guarding against a decompression
// bomb in a malformed or hostile world file.
const maxInflated = 64 << 20

func readAllLimited(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(128 << 10)
	n, err := io.Copy(&buf, io.LimitReader(r, maxInflated+1))
	if err != nil {
		return nil, err
	}
	if n > maxInflated {
		return nil, fmt.Errorf("decompressed chunk exceeds %d bytes", maxInflated)
	}
	return buf.Bytes(), nil
}

// Close releases the file handle immediately and unconditionally. Use this
// only for a Region that is never shared with another goroutine (a one-off
// open for probing or scanning); a Region held in a cache must be retired
// with Retire instead, or a concurrent reader can be left holding a closed
// file.
func (r *Region) Close() error {
	var err error
	r.closed.Do(func() { err = r.file.Close() })
	return err
}

// Acquire pins the region so Retire will not close its file until a matching
// Release is called. It reports false if the region has already been
// retired, in which case the caller must not use it and should reopen the
// region instead.
func (r *Region) Acquire() bool {
	r.refMu.Lock()
	defer r.refMu.Unlock()
	if r.retired {
		return false
	}
	r.refs++
	return true
}

// Release undoes a successful Acquire, closing the file if the region has
// since been retired and this was the last outstanding reference.
func (r *Region) Release() {
	r.refMu.Lock()
	r.refs--
	closeNow := r.retired && r.refs <= 0
	r.refMu.Unlock()
	if closeNow {
		_ = r.Close()
	}
}

// Retire marks the region as no longer live in the cache. If nothing is
// currently reading it, the file closes immediately; otherwise it closes as
// soon as the last Acquire is Released. Use Retire instead of Close for a
// Region that other goroutines might be concurrently reading via Acquire.
func (r *Region) Retire() {
	r.refMu.Lock()
	r.retired = true
	closeNow := r.refs <= 0
	r.refMu.Unlock()
	if closeNow {
		_ = r.Close()
	}
}

// ---------------------------------------------------------------------------
// Region file discovery
// ---------------------------------------------------------------------------

// regionNamePattern matches "r.<x>.<z>.mca".
var regionNamePattern = regexp.MustCompile(`^r\.(-?\d+)\.(-?\d+)\.mca$`)

// ParseRegionName extracts region coordinates from a file name.
func ParseRegionName(name string) (mcmath.RegionPos, bool) {
	m := regionNamePattern.FindStringSubmatch(name)
	if m == nil {
		return mcmath.RegionPos{}, false
	}
	x, err1 := strconv.Atoi(m[1])
	z, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return mcmath.RegionPos{}, false
	}
	return mcmath.RegionPos{X: x, Z: z}, true
}

// RegionFileName returns the canonical file name for a region.
func RegionFileName(p mcmath.RegionPos) string {
	return fmt.Sprintf("r.%d.%d.mca", p.X, p.Z)
}

// RegionEntry describes a region file found on disk.
type RegionEntry struct {
	Pos     mcmath.RegionPos
	Path    string
	Size    int64
	ModTime time.Time
}

// ListRegions enumerates the region files in a directory. A missing directory
// yields no regions rather than an error, because a dimension may exist in the
// level data before any of it has been generated.
func ListRegions(dir string) ([]RegionEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]RegionEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		pos, ok := ParseRegionName(e.Name())
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, RegionEntry{
			Pos:     pos,
			Path:    filepath.Join(dir, e.Name()),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	return out, nil
}
