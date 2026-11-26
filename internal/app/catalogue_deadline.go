package app

import (
	"net/http"
	"strings"
	"time"
)

// Only buffer finite catalogue responses. Playback, exports and event streams
// need to keep writing as data arrives and have their own deadlines.
func catalogueDeadline(surface string, next http.Handler) http.Handler {
	budget := 15 * time.Second
	if surface == "console" {
		// The preview may wait for the viewer's first personalization model.
		budget = 45 * time.Second
	}
	bounded := http.TimeoutHandler(next, budget, http.StatusText(http.StatusGatewayTimeout))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if catalogueDeadlineRoute(surface, r) {
			bounded.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func catalogueDeadlineRoute(surface string, r *http.Request) bool {
	path := r.URL.Path
	if surface == "console" {
		return r.Method == http.MethodPost && strings.HasPrefix(path, "/ether/api/pages/") && strings.HasSuffix(path, "/preview")
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	switch surface {
	case "plex":
		return path == "/library/sections" || strings.HasPrefix(path, "/library/sections/") ||
			path == "/hubs" || strings.HasPrefix(path, "/hubs/")
	case "jellyfin", "emby":
		path = strings.TrimPrefix(strings.ToLower(path), "/"+surface+"/")
		path = strings.TrimPrefix(path, "/")
		return path == "userviews" || path == "items" || path == "items/latest" ||
			path == "items/resume" || path == "shows/nextup" ||
			(strings.HasPrefix(path, "users/") && (strings.HasSuffix(path, "/items") || strings.HasSuffix(path, "/views") ||
				strings.HasSuffix(path, "/items/latest") || strings.HasSuffix(path, "/items/resume")))
	case "silo":
		path = strings.TrimPrefix(path, "/silo")
		return path == "/api/v2/catalog" || path == "/api/v2/user/libraries" ||
			path == "/api/v2/home/sections" || strings.HasPrefix(path, "/api/v2/home/sections/") ||
			strings.HasPrefix(path, "/api/v2/library/")
	case "vela":
		path = strings.TrimPrefix(path, "/vela/v0")
		return path == "/bootstrap" || path == "/libraries" || strings.HasPrefix(path, "/libraries/")
	case "stremio":
		// The first component after the mount is the credential.
		_, resource, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(path, "/stremio/"), "/"), "/")
		return strings.HasPrefix(resource, "catalog/")
	case "xtream":
		return strings.TrimPrefix(path, "/xtream") == "/player_api.php"
	}
	return false
}
