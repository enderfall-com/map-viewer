package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/enderfall/minecraft-map/backend/internal/features"
	"github.com/enderfall/minecraft-map/backend/internal/realtime"
)

// maxChunkActionBody bounds one claim/unclaim/force-load request body.
const maxChunkActionBody = 1 << 20

// maxChunkSelection bounds how many chunks one request may touch, so a
// pathological selection cannot create (or unclaim, or force-load) an
// enormous area in a single call. 144 is a 12x12 chunk square (192x192
// blocks) -- comfortably more than one build site, without letting a single
// action sweep up a whole region.
const maxChunkSelection = 144

// chunkActionRequest is the shared body shape for all three chunk actions.
// Name/Owner are only meaningful for claim; Loaded is only meaningful for
// force-load.
type chunkActionRequest struct {
	Dimension string              `json:"dimension"`
	Chunks    []features.ChunkRef `json:"chunks"`
	Name      string              `json:"name,omitempty"`
	Owner     string              `json:"owner,omitempty"`
	Loaded    *bool               `json:"loaded,omitempty"`
}

// decodeChunkAction reads, validates and resolves the common request shape.
// It writes its own error response and returns ok=false on any problem, so
// handlers can return immediately without duplicating the checks.
func (s *Server) decodeChunkAction(w http.ResponseWriter, r *http.Request) (req chunkActionRequest, dimension string, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxChunkActionBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return req, "", false
	}
	if len(req.Chunks) == 0 {
		writeError(w, http.StatusBadRequest, "no chunks selected")
		return req, "", false
	}
	if len(req.Chunks) > maxChunkSelection {
		writeError(w, http.StatusBadRequest, "too many chunks selected")
		return req, "", false
	}
	dim, ok := s.resolveDimension(r.Context(), req.Dimension)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown dimension %q", req.Dimension))
		return req, "", false
	}
	return req, dim.ID, true
}

// handleClaimChunks creates a claim covering exactly the selected chunks, or
// reports a 409 with the specific chunks that are already claimed.
func (s *Server) handleClaimChunks(w http.ResponseWriter, r *http.Request) {
	req, dim, ok := s.decodeChunkAction(w, r)
	if !ok {
		return
	}
	area, conflicts, err := s.Features.ClaimChunks(dim, req.Chunks, req.Name, req.Owner)
	if err != nil {
		if errors.Is(err, features.ErrChunksClaimed) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":     "some chunks are already claimed",
				"conflicts": conflicts,
			})
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.Hub != nil {
		s.Hub.Broadcast(realtime.EventFeatureUpdated(dim, "claim", nil))
	}
	writeJSON(w, http.StatusOK, area)
}

// handleUnclaimChunks removes the selected chunks from any claim they
// belong to.
func (s *Server) handleUnclaimChunks(w http.ResponseWriter, r *http.Request) {
	req, dim, ok := s.decodeChunkAction(w, r)
	if !ok {
		return
	}
	s.Features.UnclaimChunks(dim, req.Chunks)
	if s.Hub != nil {
		s.Hub.Broadcast(realtime.EventFeatureUpdated(dim, "claim", nil))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleForceLoadChunks marks (or clears) the selected chunks as force-loaded.
// Defaults to marking them when "loaded" is omitted, since the common case is
// a dedicated "Force Load" action rather than a generic toggle.
func (s *Server) handleForceLoadChunks(w http.ResponseWriter, r *http.Request) {
	req, dim, ok := s.decodeChunkAction(w, r)
	if !ok {
		return
	}
	loaded := true
	if req.Loaded != nil {
		loaded = *req.Loaded
	}
	s.Features.SetForceLoaded(dim, req.Chunks, loaded)
	if s.Hub != nil {
		s.Hub.Broadcast(realtime.EventFeatureUpdated(dim, "forceload", nil))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
