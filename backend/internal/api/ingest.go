package api

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/enderfall/minecraft-map/backend/internal/features"
	"github.com/enderfall/minecraft-map/backend/internal/realtime"
)

// maxIngestBody bounds a single ingest request, so a misbehaving or hostile
// plugin cannot make the server buffer an unbounded payload in memory.
const maxIngestBody = 4 << 20

// maxIngestItems bounds how many players, areas or markers one request may
// carry, independent of the byte cap, so a request built of many tiny
// objects cannot still produce an enormous in-memory batch.
const maxIngestItems = 20000

// requireIngestAuth checks the bearer token against the configured ingest
// token. Ingestion is refused entirely when no token is configured, so a
// fresh deployment never carries an unauthenticated remote-write endpoint by
// accident -- an operator must deliberately opt in by setting live.ingestToken.
func (s *Server) requireIngestAuth(w http.ResponseWriter, r *http.Request) bool {
	token := s.Cfg.Live.IngestToken
	if token == "" {
		writeError(w, http.StatusServiceUnavailable, "ingestion is not enabled")
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid or missing ingest token")
		return false
	}
	return true
}

// decodeIngestBody reads and decodes a size-bounded JSON request body.
func decodeIngestBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxIngestBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Players
// ---------------------------------------------------------------------------

type ingestPlayersRequest struct {
	Players []features.Player `json:"players"`
}

// handleIngestPlayers implements a full snapshot sync: whatever a plugin
// currently sees online replaces the map's entire tracked player set.
//
// This is deliberately a sync, not an upsert. A plugin naturally has its full
// online-player list on hand every tick, and pushing exactly that means a
// missed or out-of-order "player left" event can never leave a stale ghost
// player on the map -- the next snapshot simply omits them.
func (s *Server) handleIngestPlayers(w http.ResponseWriter, r *http.Request) {
	if !s.requireIngestAuth(w, r) {
		return
	}
	var req ingestPlayersRequest
	if !decodeIngestBody(w, r, &req) {
		return
	}
	if len(req.Players) > maxIngestItems {
		writeError(w, http.StatusBadRequest, "too many players in one request")
		return
	}
	for i, p := range req.Players {
		if p.UUID == "" {
			writeError(w, http.StatusBadRequest, "player missing uuid")
			return
		}
		dim, ok := s.resolveDimension(r.Context(), p.Dimension)
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("player %s: unknown dimension %q", p.UUID, p.Dimension))
			return
		}
		req.Players[i].Dimension = dim.ID
	}
	s.Features.SyncPlayers(req.Players)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(req.Players)})
}

// ---------------------------------------------------------------------------
// Areas (claims and regions)
// ---------------------------------------------------------------------------

type ingestAreasRequest struct {
	Upsert    []features.Area `json:"upsert"`
	RemoveIDs []string        `json:"removeIds"`
}

// handleIngestAreas upserts and/or removes claims and regions. Unlike
// players, areas are created and destroyed sparsely, so the contract is
// incremental rather than a full-snapshot sync.
func (s *Server) handleIngestAreas(w http.ResponseWriter, r *http.Request) {
	if !s.requireIngestAuth(w, r) {
		return
	}
	var req ingestAreasRequest
	if !decodeIngestBody(w, r, &req) {
		return
	}
	if len(req.Upsert)+len(req.RemoveIDs) > maxIngestItems {
		writeError(w, http.StatusBadRequest, "too many areas in one request")
		return
	}

	touched := make(map[string]struct{}, len(req.Upsert)+len(req.RemoveIDs))
	for i, a := range req.Upsert {
		if a.ID == "" {
			writeError(w, http.StatusBadRequest, "area missing id")
			return
		}
		dim, ok := s.resolveDimension(r.Context(), a.Dimension)
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("area %s: unknown dimension %q", a.ID, a.Dimension))
			return
		}
		req.Upsert[i].Dimension = dim.ID
	}
	for _, a := range req.Upsert {
		s.Features.PutArea(a)
		touched[a.Dimension] = struct{}{}
	}
	for _, id := range req.RemoveIDs {
		if dim, ok := s.Features.AreaDimension(id); ok {
			touched[dim] = struct{}{}
		}
		s.Features.RemoveArea(id)
	}

	// Both event names go out for every touched dimension: the frontend's
	// realtime dispatch only reacts to the exact strings "claim.updated" and
	// "region.updated", and a claim and a region are the same Area type here,
	// so there is no reliable way to know which label a given client cares
	// about without it -- broadcasting both is cheap and always correct.
	if s.Hub != nil {
		for dim := range touched {
			s.Hub.Broadcast(realtime.EventFeatureUpdated(dim, "claim", nil))
			s.Hub.Broadcast(realtime.EventFeatureUpdated(dim, "region", nil))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "upserted": len(req.Upsert), "removed": len(req.RemoveIDs),
	})
}

// ---------------------------------------------------------------------------
// Markers
// ---------------------------------------------------------------------------

type ingestMarkersRequest struct {
	Upsert    []features.Marker `json:"upsert"`
	RemoveIDs []string          `json:"removeIds"`
}

// handleIngestMarkers upserts and/or removes waypoints, warps and other
// points of interest.
func (s *Server) handleIngestMarkers(w http.ResponseWriter, r *http.Request) {
	if !s.requireIngestAuth(w, r) {
		return
	}
	var req ingestMarkersRequest
	if !decodeIngestBody(w, r, &req) {
		return
	}
	if len(req.Upsert)+len(req.RemoveIDs) > maxIngestItems {
		writeError(w, http.StatusBadRequest, "too many markers in one request")
		return
	}

	touched := make(map[string]struct{}, len(req.Upsert)+len(req.RemoveIDs))
	for i, mk := range req.Upsert {
		if mk.ID == "" {
			writeError(w, http.StatusBadRequest, "marker missing id")
			return
		}
		dim, ok := s.resolveDimension(r.Context(), mk.Dimension)
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("marker %s: unknown dimension %q", mk.ID, mk.Dimension))
			return
		}
		req.Upsert[i].Dimension = dim.ID
	}
	for _, mk := range req.Upsert {
		s.Features.PutMarker(mk)
		touched[mk.Dimension] = struct{}{}
	}
	for _, id := range req.RemoveIDs {
		if dim, ok := s.Features.MarkerDimension(id); ok {
			touched[dim] = struct{}{}
		}
		s.Features.RemoveMarker(id)
	}

	if s.Hub != nil {
		for dim := range touched {
			s.Hub.Broadcast(realtime.EventFeatureUpdated(dim, "marker", nil))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "upserted": len(req.Upsert), "removed": len(req.RemoveIDs),
	})
}
