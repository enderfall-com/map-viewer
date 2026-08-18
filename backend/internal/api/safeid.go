package api

import "github.com/enderfall/minecraft-map/backend/internal/cache"

// cacheSafe is a local alias for the shared identifier sanitiser, so tile paths
// and API dimension lookups can never disagree about what a dimension token
// looks like.
func cacheSafe(id string) string { return cache.SafeID(id) }
