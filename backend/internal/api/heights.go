package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/enderfall/minecraft-map/backend/internal/features"
	"github.com/enderfall/minecraft-map/backend/internal/mcmath"
	"github.com/enderfall/minecraft-map/backend/internal/world"
)

// maxHeightChunks bounds one heights request. Comfortably above the largest
// selection the UI can build (maxChunkSelection), with headroom so a client
// that batches a few gestures together still gets one round trip.
const maxHeightChunks = 1024

// groundPercentile picks which of a chunk's 256 column heights stands in as
// its single representative ground level.
//
// Deliberately low rather than the median or the maximum: a chunk's columns
// include whatever sits on top of the terrain, and in a forest most of them
// are tree canopy tens of blocks above the actual ground. A median can
// therefore land in the treetops of a densely wooded chunk, and the maximum
// almost always does. Taking a low percentile ducks under the canopy while
// still ignoring the odd ravine or cave mouth that a strict minimum would
// fall into.
const groundPercentile = 25

// concurrentHeightChunks bounds how many chunks a heights request resolves at
// once. Matches the surface assembler's own fan-out, which is tuned for the
// same mix of region-file I/O and NBT decoding.
const concurrentHeightChunks = 16

// chunkHeightsRequest asks for the representative ground level of each of a
// set of chunks.
type chunkHeightsRequest struct {
	Dimension string              `json:"dimension"`
	Chunks    []features.ChunkRef `json:"chunks"`
}

// chunkHeight reports one chunk's ground level. Found is false for a chunk
// with no world data at all (never generated), whose Y carries no meaning.
type chunkHeight struct {
	X     int  `json:"x"`
	Z     int  `json:"z"`
	Y     int  `json:"y"`
	Found bool `json:"found"`
}

type chunkHeightsResponse struct {
	Heights []chunkHeight `json:"heights"`
}

// handleChunkHeights resolves a batch of chunks to one ground level each.
//
// This exists so a client placing an overlay across a chunk selection makes a
// single request instead of one per chunk: a 12x12 selection is 144 chunks,
// and asking for those one at a time costs 144 round trips to say something
// the server can answer in one pass. Read-only, and deliberately separate from
// the mutating chunk actions in chunks.go.
func (s *Server) handleChunkHeights(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxChunkActionBody)
	var req chunkHeightsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if len(req.Chunks) == 0 {
		writeError(w, http.StatusBadRequest, "no chunks requested")
		return
	}
	if len(req.Chunks) > maxHeightChunks {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("too many chunks requested (max %d)", maxHeightChunks))
		return
	}
	dim, ok := s.resolveDimension(r.Context(), req.Dimension)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown dimension %q", req.Dimension))
		return
	}

	// Resolved per chunk rather than over the whole selection's bounding box:
	// a selection assembled from separate clicks can be sparse, and two
	// clicks far apart would otherwise force a window spanning everything
	// between them just to read two chunks.
	out := make([]chunkHeight, len(req.Chunks))
	sem := make(chan struct{}, concurrentHeightChunks)
	var wg sync.WaitGroup
	for i, c := range req.Chunks {
		wg.Add(1)
		go func(i int, c features.ChunkRef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			y, found := s.chunkGroundY(r.Context(), dim.ID, dim.MinY, dim.MaxY, c)
			out[i] = chunkHeight{X: c.X, Z: c.Z, Y: y, Found: found}
		}(i, c)
	}
	wg.Wait()

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, chunkHeightsResponse{Heights: out})
}

// chunkGroundY reduces one chunk's columns to a single representative ground
// level, at groundPercentile of the heights actually present.
func (s *Server) chunkGroundY(ctx context.Context, dimension string, minY, maxY int, c features.ChunkRef) (int, bool) {
	x0 := c.X * mcmath.ChunkSize
	z0 := c.Z * mcmath.ChunkSize
	bounds := mcmath.BlockBounds{
		MinX: x0,
		MinZ: z0,
		MaxX: x0 + mcmath.ChunkSize,
		MaxZ: z0 + mcmath.ChunkSize,
	}
	surf, err := world.Assemble(ctx, s.Provider, dimension, bounds, minY, maxY, nil)
	if err != nil {
		return 0, false
	}

	heights := make([]int, 0, mcmath.ChunkSize*mcmath.ChunkSize)
	for z := z0; z < z0+mcmath.ChunkSize; z++ {
		for x := x0; x < x0+mcmath.ChunkSize; x++ {
			col := surf.At(x, z)
			if !col.Present() {
				continue
			}
			// The visible surface, which over an ocean is the water top
			// rather than the seabed the overlay would otherwise sink to.
			h := col.Height
			if col.Water() {
				h = col.WaterY
			}
			heights = append(heights, h)
		}
	}
	if len(heights) == 0 {
		return 0, false
	}
	sort.Ints(heights)
	idx := len(heights) * groundPercentile / 100
	if idx >= len(heights) {
		idx = len(heights) - 1
	}
	return heights[idx], true
}
