package mcworld

import (
	"bytes"
	"compress/gzip"
	"io"
)

// gunzip inflates gzip data, bounded so a corrupt level.dat cannot exhaust
// memory.
func gunzip(raw []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(io.LimitReader(zr, 32<<20))
}
