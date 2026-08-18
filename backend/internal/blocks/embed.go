package blocks

import (
	_ "embed"
	"fmt"
)

// The baseline palettes are embedded so the server renders correct terrain even
// with no configuration files present at all. Files named in the config are
// loaded afterwards and override these entries, which makes a modpack overlay
// purely additive: operators never have to copy the vanilla list to extend it.

//go:embed data/blocks.json
var defaultBlocksJSON []byte

//go:embed data/biomes.json
var defaultBiomesJSON []byte

// DefaultBlocksJSON returns the embedded baseline block palette.
func DefaultBlocksJSON() []byte { return defaultBlocksJSON }

// DefaultBiomesJSON returns the embedded baseline biome palette.
func DefaultBiomesJSON() []byte { return defaultBiomesJSON }

// NewDefaultRegistry returns a registry preloaded with the baseline palette.
func NewDefaultRegistry() (*Registry, error) {
	r := NewRegistry()
	if _, err := r.LoadBlocksJSON(defaultBlocksJSON); err != nil {
		return nil, fmt.Errorf("load embedded block palette: %w", err)
	}
	return r, nil
}

// NewDefaultBiomes returns a biome table preloaded with the baseline palette.
func NewDefaultBiomes() (*Biomes, error) {
	b := NewBiomes()
	if _, err := b.LoadBiomesJSON(defaultBiomesJSON); err != nil {
		return nil, fmt.Errorf("load embedded biome palette: %w", err)
	}
	return b, nil
}
