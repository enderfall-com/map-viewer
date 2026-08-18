// Package textures resolves real Minecraft block textures -- read straight
// out of a client jar and mod/resource-pack archives -- into the top and side
// images the renderers paint, in place of (or alongside) the hand-authored
// flat colours in blocks.json.
//
// # Why textures cannot simply replace the colour system
//
// Texture packs are large, licensed assets nobody but the operator has the
// right to distribute, so they are never embedded in the binary and can be
// entirely absent. Every lookup here therefore degrades to "not resolvable"
// rather than erroring, and the flat-colour renderer keeps working unchanged
// when no texture source is configured, or for any individual block a pack
// does not cover.
//
// # Why blockstates and models are parsed at all
//
// A texture pack does not map "block name -> picture"; Minecraft's own asset
// format maps a block to a blockstate file, which points at a model file,
// which -- often through a chain of parent models -- assigns a texture to
// each of a cuboid's six faces via named variables. Reproducing that chain is
// what lets an arbitrary modded block resolve correctly instead of only
// vanilla ones matching some naming convention.
package textures

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// pack is one archive or directory layered into a Store.
type pack struct {
	name string
	// files maps a path already normalised to forward slashes (e.g.
	// "assets/minecraft/textures/block/stone.png") to a reader for its bytes.
	files map[string]*zip.File
	// dirFiles is the directory-source equivalent of files, used for a loose
	// folder of extracted assets rather than a jar/zip.
	dirFiles map[string]string
	zr       *zip.ReadCloser
}

func (p *pack) read(path string) ([]byte, bool) {
	if p.zr != nil {
		f, ok := p.files[path]
		if !ok {
			return nil, false
		}
		rc, err := f.Open()
		if err != nil {
			return nil, false
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, false
		}
		return data, true
	}
	if diskPath, ok := p.dirFiles[path]; ok {
		data, err := os.ReadFile(diskPath)
		if err != nil {
			return nil, false
		}
		return data, true
	}
	return nil, false
}

func (p *pack) Close() error {
	if p.zr != nil {
		return p.zr.Close()
	}
	return nil
}

// isRelevant bounds which entries a pack indexes, so opening a large mod jar
// costs a directory scan, not a scan plus bookkeeping for thousands of
// irrelevant entries (item icons, sounds, lang files, entity models).
func isRelevant(name string) bool {
	if !strings.HasPrefix(name, "assets/") {
		return false
	}
	// Only the parts of the asset tree block-texture resolution ever reads.
	rest := name[len("assets/"):]
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return false
	}
	sub := rest[i+1:]
	return strings.HasPrefix(sub, "blockstates/") ||
		strings.HasPrefix(sub, "models/block/") ||
		strings.HasPrefix(sub, "models/item/") || // some block models parent through here
		strings.HasPrefix(sub, "textures/block/") ||
		strings.HasPrefix(sub, "textures/blocks/") // pre-1.13 pack layout, still seen in old-style packs
}

// openZipPack opens a jar or zip file as a pack, indexing only relevant
// entries.
func openZipPack(path string) (*pack, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	p := &pack{name: filepath.Base(path), zr: zr, files: make(map[string]*zip.File)}
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if isRelevant(name) {
			p.files[name] = f
		}
	}
	return p, nil
}

// openDirPack opens a loose directory of already-extracted assets (e.g.
// "assets/minecraft/textures/block/stone.png" sitting directly under root).
// Mainly useful for testing and for operators who prefer to hand-curate a
// folder rather than point at live game files.
func openDirPack(root string) (*pack, error) {
	p := &pack{name: filepath.Base(root), dirFiles: make(map[string]string)}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		name := strings.ReplaceAll(rel, "\\", "/")
		if isRelevant(name) {
			p.dirFiles[name] = path
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", root, err)
	}
	return p, nil
}

// Store is a layered, read-only view over one or more jars, zips and
// directories. A later source overrides an earlier one at the same path,
// exactly mirroring how Minecraft itself stacks a client jar, then mod
// jars, then resource packs.
type Store struct {
	packs []*pack // priority order: index 0 is lowest priority
}

// OpenSources builds a Store from an ordered list of paths. Each entry is:
//   - a .jar or .zip file, opened as an archive; or
//   - a directory containing *.jar/*.zip files, each opened and layered in
//     alphabetical order (convenient for pointing straight at a mods/ folder); or
//   - a directory containing loose, already-extracted assets.
//
// A source that cannot be opened is skipped with the error collected rather
// than aborting the whole load: one corrupt mod jar must not disable texture
// rendering for every other block.
func OpenSources(paths []string) (*Store, []error) {
	s := &Store{}
	var errs []error
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("texture source %s: %w", p, err))
			continue
		}
		if !info.IsDir() {
			pk, err := openZipPack(p)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			s.packs = append(s.packs, pk)
			continue
		}
		// A directory: is it a pile of archives, or a loose extracted tree?
		archives := findArchives(p)
		if len(archives) == 0 {
			pk, err := openDirPack(p)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			s.packs = append(s.packs, pk)
			continue
		}
		for _, a := range archives {
			pk, err := openZipPack(a)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			s.packs = append(s.packs, pk)
		}
	}
	return s, errs
}

// findArchives lists *.jar and *.zip files directly inside a directory,
// alphabetically, so a mods/ folder's load order is stable across restarts.
func findArchives(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		lower := strings.ToLower(e.Name())
		if strings.HasSuffix(lower, ".jar") || strings.HasSuffix(lower, ".zip") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// Read returns the bytes at an asset path (e.g.
// "assets/minecraft/textures/block/stone.png"), consulting packs from
// highest to lowest priority.
func (s *Store) Read(path string) ([]byte, bool) {
	for i := len(s.packs) - 1; i >= 0; i-- {
		if data, ok := s.packs[i].read(path); ok {
			return data, true
		}
	}
	return nil, false
}

// Close releases every open archive.
func (s *Store) Close() error {
	var first error
	for _, p := range s.packs {
		if err := p.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// SourceCount reports how many packs were successfully opened, for logging.
func (s *Store) SourceCount() int { return len(s.packs) }
