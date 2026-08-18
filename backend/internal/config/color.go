package config

import (
	"image/color"

	"github.com/enderfall/minecraft-map/backend/internal/blocks"
)

// parseHex is a thin alias so the config package does not expose the blocks
// package in its signatures.
func parseHex(s string) (color.NRGBA, error) { return blocks.ParseHexColor(s) }
