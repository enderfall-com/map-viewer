package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/enderfall/minecraft-map/backend/internal/cache"
)

// skinHTTPClient is used only for outbound calls to Mojang's session server
// and texture CDN -- the one place this server reaches the network itself;
// everything else reads local files. A short timeout keeps a slow or
// unreachable Mojang endpoint from ever blocking a request indefinitely.
var skinHTTPClient = &http.Client{Timeout: 8 * time.Second}

// uuidPattern accepts a Mojang UUID with or without dashes, matching both
// forms real profile data and ingested plugin data use.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}$`)

// skinResolver caches resolved skin textures and deduplicates concurrent
// lookups, mirroring world.Cached's LRU + single-flight + absent-set shape
// for the same reason: many simultaneous viewers of a crowded area would
// otherwise each trigger their own Mojang round trip for the same player.
type skinResolver struct {
	cache  *cache.LRU[string, []byte]
	flight *cache.Group[string, []byte]
	absent *cache.LRU[string, struct{}]
}

// nominalSkinCost estimates one cached skin's size for sizing the
// absent-lookup memo; real skins are typically a few KB.
const nominalSkinCost = 4 << 10

func newSkinResolver() *skinResolver {
	const capacityBytes = 64 << 20 // a few thousand cached player skins
	return &skinResolver{
		cache:  cache.NewLRU[string, []byte](capacityBytes),
		flight: cache.NewGroup[string, []byte](),
		absent: cache.NewLRU[string, struct{}](capacityBytes / nominalSkinCost),
	}
}

// mojangProfile is the shape of Mojang's session-server profile response.
type mojangProfile struct {
	Properties []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"properties"`
}

// mojangTextures is the base64-decoded payload of a profile's "textures"
// property.
type mojangTextures struct {
	Textures struct {
		Skin struct {
			URL string `json:"url"`
		} `json:"SKIN"`
	} `json:"textures"`
}

// handleSkin proxies a player's real Minecraft skin texture, resolved from
// their UUID via Mojang's own session server, so the frontend can render an
// actual isometric character model from the real texture instead of
// depending on a third-party render service's framing and visual style.
//
// This is the server's only outbound network call. Fetching it server-side
// rather than having the browser call Mojang directly sidesteps whatever
// CORS policy the profile-lookup endpoint has, and lets every viewer of a
// crowded area share one cached result instead of each hitting Mojang
// independently.
func (s *Server) handleSkin(w http.ResponseWriter, r *http.Request) {
	uuid := strings.ToLower(r.PathValue("uuid"))
	if !uuidPattern.MatchString(uuid) {
		writeError(w, http.StatusBadRequest, "invalid uuid")
		return
	}
	key := strings.ReplaceAll(uuid, "-", "")

	if data, ok := s.skins.cache.Get(key); ok {
		writeSkinPNG(w, data)
		return
	}
	if _, ok := s.skins.absent.Get(key); ok {
		writeError(w, http.StatusNotFound, "no skin found for this player")
		return
	}

	data, err, _ := s.skins.flight.Do(key, func() ([]byte, error) {
		return fetchSkin(r.Context(), key)
	})
	if err != nil {
		s.skins.absent.Put(key, struct{}{}, 1)
		s.Log.Debug("skin lookup failed", "uuid", key, "error", err)
		writeError(w, http.StatusNotFound, "no skin found for this player")
		return
	}
	s.skins.cache.Put(key, data, int64(len(data)))
	writeSkinPNG(w, data)
}

func writeSkinPNG(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "image/png")
	// Skins change rarely, and the cache would keep serving a stale one for
	// up to its own lifetime anyway, so a modest client-side cache is safe.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(data)
}

// fetchSkin resolves a lowercase, undashed uuid to its skin texture bytes
// via Mojang's session server, then downloads the texture itself.
func fetchSkin(ctx context.Context, undashedUUID string) ([]byte, error) {
	profileURL := "https://sessionserver.mojang.com/session/minecraft/profile/" + undashedUUID
	var profile mojangProfile
	if err := fetchJSON(ctx, profileURL, &profile); err != nil {
		return nil, fmt.Errorf("fetch profile: %w", err)
	}

	var texturesB64 string
	for _, p := range profile.Properties {
		if p.Name == "textures" {
			texturesB64 = p.Value
			break
		}
	}
	if texturesB64 == "" {
		return nil, fmt.Errorf("profile has no textures property")
	}
	raw, err := base64.StdEncoding.DecodeString(texturesB64)
	if err != nil {
		return nil, fmt.Errorf("decode textures property: %w", err)
	}
	var textures mojangTextures
	if err := json.Unmarshal(raw, &textures); err != nil {
		return nil, fmt.Errorf("parse textures property: %w", err)
	}
	if textures.Textures.Skin.URL == "" {
		return nil, fmt.Errorf("profile has no skin texture")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, textures.Textures.Skin.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := skinHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch skin texture: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("skin texture request: status %d", resp.StatusCode)
	}
	// Real skins are a few KB; this ceiling is generous headroom, not an
	// expected size.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("read skin texture: %w", err)
	}
	return data, nil
}

func fetchJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := skinHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}
