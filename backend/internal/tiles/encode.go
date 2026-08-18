// Package tiles owns the raster tile pyramid: encoding, scheduling, generation
// and invalidation.
package tiles

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"strings"

	"github.com/HugoSmits86/nativewebp"
)

// Format is a tile image encoding.
type Format string

const (
	// FormatWebP is the default. Encoding is lossless VP8L in pure Go, so there
	// is no cgo dependency and terrain stays pixel-exact through the pyramid --
	// which matters because parent tiles are built by resampling children, and
	// lossy artefacts would compound at every level.
	FormatWebP Format = "webp"
	// FormatPNG is the fallback for environments that need maximum
	// compatibility. Larger, but identical in quality.
	FormatPNG Format = "png"
)

// ParseFormat resolves a format name, defaulting to WebP.
func ParseFormat(s string) (Format, bool) {
	switch Format(strings.ToLower(strings.TrimPrefix(s, "."))) {
	case FormatWebP:
		return FormatWebP, true
	case FormatPNG:
		return FormatPNG, true
	case "":
		return FormatWebP, true
	}
	return FormatWebP, false
}

// Ext returns the file extension, without a dot.
func (f Format) Ext() string { return string(f) }

// ContentType returns the HTTP media type.
func (f Format) ContentType() string {
	if f == FormatPNG {
		return "image/png"
	}
	return "image/webp"
}

// Encoder turns rendered images into tile bytes.
//
// It exists as a type rather than a bare function so a future encoder with
// tunable quality, or a texture-aware format, can be swapped in without
// touching the tile manager.
type Encoder struct {
	Format Format
	// PNGCompression selects the PNG encoder's effort level.
	PNGCompression png.CompressionLevel
}

// NewEncoder builds an encoder for a format.
func NewEncoder(f Format) *Encoder {
	return &Encoder{Format: f, PNGCompression: png.BestCompression}
}

// Encode renders an image to tile bytes.
func (e *Encoder) Encode(img *image.NRGBA) ([]byte, error) {
	var buf bytes.Buffer
	// Terrain tiles are large flat regions of repeated colour, so both encoders
	// compress them extremely well; a fully unexplored tile lands in well under
	// a kilobyte.
	buf.Grow(64 << 10)

	switch e.Format {
	case FormatPNG:
		enc := png.Encoder{CompressionLevel: e.PNGCompression}
		if err := enc.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("encode png tile: %w", err)
		}
	default:
		if err := nativewebp.Encode(&buf, img, nil); err != nil {
			return nil, fmt.Errorf("encode webp tile: %w", err)
		}
	}
	return buf.Bytes(), nil
}
