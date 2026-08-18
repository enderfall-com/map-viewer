// Package cache provides the tile storage abstraction and the bounded in-memory
// caches that keep a very large world's working set within a fixed footprint.
package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotFound reports a tile that has not been generated.
var ErrNotFound = errors.New("tile not found")

// Key identifies one stored tile.
//
// Note what is absent: the revision. A tile's revision exists to bust HTTP and
// CDN caches, not to accumulate copies on disk -- keeping every historical
// revision of every tile of a hundred-gigabyte world would dwarf the world
// itself. Storage therefore holds exactly one current image per tile, while the
// revision travels in the URL. A client holding an old revision URL simply
// never requests it again once it learns the new one.
type Key struct {
	Dimension string // already sanitised, e.g. "minecraft_overworld"
	Mode      string // "top" or "iso"
	// Variant distinguishes tiles of the same area rendered a different way
	// within one mode -- currently the isometric camera corner, where "" means
	// the default corner. Empty leaves the path exactly as it was before
	// variants existed, so the default view keeps using tiles already on disk
	// instead of regenerating a whole pyramid under a new name.
	Variant string
	Style   string // "terrain", "biome", "height"
	Zoom    int
	X, Y    int
	Format  string // "webp" or "png"
}

// Path returns the store-relative path for a tile.
//
// Tile coordinates are signed, so they are formatted directly rather than
// offset into an unsigned space. That keeps the on-disk layout readable and,
// more importantly, means nothing breaks when a world crosses X=0 or Z=0.
// Negative components are grouped under a "-" prefixed directory so a single
// directory never mixes signs, which keeps listings tidy for very large worlds.
func (k Key) Path() string {
	mode := k.Mode
	if k.Variant != "" {
		mode += "_" + k.Variant
	}
	return filepath.Join(
		k.Dimension,
		mode,
		k.Style,
		strconv.Itoa(k.Zoom),
		// Sharding by X keeps any one directory to a manageable entry count even
		// for worlds with millions of tiles.
		coordDir(k.X),
		fmt.Sprintf("%s_%s.%s", coordName(k.X), coordName(k.Y), k.Format),
	)
}

// coordDir buckets tile X into directories of 64 to bound directory width.
func coordDir(v int) string {
	b := v >> 6 // arithmetic shift: floors for negatives too
	if b < 0 {
		return "n" + strconv.Itoa(-b)
	}
	return "p" + strconv.Itoa(b)
}

func coordName(v int) string {
	if v < 0 {
		return "n" + strconv.Itoa(-v)
	}
	return strconv.Itoa(v)
}

// Store persists rendered tiles.
//
// The interface is deliberately object-store shaped -- flat keys, whole-object
// reads and writes, no directory semantics, no partial updates -- so that an
// S3, R2 or CDN-backed implementation slots in without the tile pipeline
// noticing. The filesystem implementation is the one that has to do extra work
// (creating parent directories, atomic renames), not the other way round.
type Store interface {
	// Get returns a tile's encoded bytes, or ErrNotFound.
	Get(ctx context.Context, k Key) ([]byte, error)
	// Put stores a tile, overwriting any existing one atomically.
	Put(ctx context.Context, k Key, data []byte) error
	// Has reports whether a tile exists without transferring it.
	Has(ctx context.Context, k Key) (bool, error)
	// Delete removes a tile, succeeding if it was already absent.
	Delete(ctx context.Context, k Key) error
}

// ---------------------------------------------------------------------------
// Filesystem store
// ---------------------------------------------------------------------------

// FSStore stores tiles on the local filesystem.
type FSStore struct {
	root string

	// dirOnce guards against the thundering herd of MkdirAll calls that a full
	// worker pool would otherwise make for the same new directory.
	dirs sync.Map
}

// NewFSStore creates a filesystem-backed tile store rooted at dir.
func NewFSStore(dir string) (*FSStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create tile directory: %w", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &FSStore{root: abs}, nil
}

// Root returns the absolute tile directory.
func (s *FSStore) Root() string { return s.root }

// resolve joins the key path to the root and verifies the result cannot escape
// it. Keys are built from validated identifiers, but this is the last line of
// defence against a path traversal reaching the filesystem.
func (s *FSStore) resolve(k Key) (string, error) {
	p := filepath.Join(s.root, k.Path())
	clean := filepath.Clean(p)
	if clean != s.root && !strings.HasPrefix(clean, s.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("tile path escapes store root")
	}
	return clean, nil
}

// Get implements Store.
func (s *FSStore) Get(_ context.Context, k Key) ([]byte, error) {
	p, err := s.resolve(k)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Put implements Store. The write goes to a temporary file and is renamed into
// place, so a reader never observes a half-written tile and a crash mid-write
// cannot leave a corrupt image behind.
func (s *FSStore) Put(_ context.Context, k Key, data []byte) error {
	p, err := s.resolve(k)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if _, seen := s.dirs.Load(dir); !seen {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create tile dir: %w", err)
		}
		s.dirs.Store(dir, struct{}{})
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if errors.Is(err, os.ErrNotExist) {
		// The directory was recorded as existing but is gone now -- removed by
		// an operator or cleanup job while the server kept running. The dirs
		// cache is only ever a positive memo, never authoritative, so forget
		// it and recreate the directory rather than failing every write to
		// this shard until the process restarts.
		s.dirs.Delete(dir)
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return fmt.Errorf("recreate tile dir: %w", mkErr)
		}
		s.dirs.Store(dir, struct{}{})
		tmp, err = os.CreateTemp(dir, ".tmp-*")
	}
	if err != nil {
		return fmt.Errorf("create temp tile: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write tile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, p); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("commit tile: %w", err)
	}
	return nil
}

// Has implements Store.
func (s *FSStore) Has(_ context.Context, k Key) (bool, error) {
	p, err := s.resolve(k)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Delete implements Store.
func (s *FSStore) Delete(_ context.Context, k Key) error {
	p, err := s.resolve(k)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ModTime reports when a tile was last written, used by the CLI to report
// coverage. Absent tiles return the zero time.
func (s *FSStore) ModTime(k Key) time.Time {
	p, err := s.resolve(k)
	if err != nil {
		return time.Time{}
	}
	fi, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// ---------------------------------------------------------------------------
// Identifier validation
// ---------------------------------------------------------------------------

// safeIDPattern is what a sanitised identifier may contain. Nothing that could
// be interpreted as a path separator, a parent directory or a drive letter is
// permitted.
var safeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,127}$`)

// SafeID converts a namespaced Minecraft identifier into a filesystem- and
// URL-safe token: "minecraft:the_nether" becomes "minecraft_the_nether".
//
// This is a one-way sanitisation used for building paths. Requests are always
// matched against the known dimension list rather than being turned back into
// world paths, so a caller cannot smuggle a traversal through it.
func SafeID(id string) string {
	s := strings.ToLower(strings.TrimSpace(id))
	s = strings.NewReplacer(":", "_", "/", "_", "\\", "_", " ", "_").Replace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	// A leading dot or dash could be read as a flag or a hidden file.
	out = strings.TrimLeft(out, ".-")
	if out == "" {
		return "unknown"
	}
	if len(out) > 128 {
		out = out[:128]
	}
	return out
}

// ValidSafeID reports whether a token is already safe to place in a path.
func ValidSafeID(s string) bool { return safeIDPattern.MatchString(s) }
