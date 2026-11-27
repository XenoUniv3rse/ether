package app

import (
	"net/http"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/perfharness"
)

// Each entry names a specific registration. Delegating mux entries do not
// have a response of their own, so coverage is checked at their leaf routes.
var perfMatrixRouteExceptions = matrixRouteExceptions()

func matrixRouteExceptions() map[perfharness.MatrixRouteKey]string {
	allowed := map[perfharness.MatrixRouteKey]string{}
	add := func(surface, method, reason string, paths ...string) {
		for _, path := range paths {
			allowed[perfharness.MatrixRouteKey{Surface: surface, Method: method, Pattern: path}] = reason
		}
	}
	add("console", "*", "dispatches to separately inventoried owner API leaf routes",
		"/ether/api/activity", "/ether/api/activity/", "/ether/api/adoption/backups",
		"/ether/api/adoption/migration", "/ether/api/adoption/migration/",
		"/ether/api/agent", "/ether/api/agent/", "/ether/api/agent/mcp", "/ether/api/agent/mcp/",
		"/ether/api/availability", "/ether/api/availability/", "/ether/api/cache", "/ether/api/cache/",
		"/ether/api/calendar", "/ether/api/calendar/", "/ether/api/cluster",
		"/ether/api/cluster/join-tokens", "/ether/api/cluster/join-tokens/",
		"/ether/api/cluster/node", "/ether/api/cluster/settings", "/ether/api/cluster/setup", "/ether/api/cluster/setup/",
		"/ether/api/discovery", "/ether/api/discovery/", "/ether/api/discovery/overlays", "/ether/api/discovery/overlays/",
		"/ether/api/discovery/ratings", "/ether/api/discovery/ratings/", "/ether/api/experience",
		"/ether/api/extend/registry", "/ether/api/extend/registry/", "/ether/api/flows", "/ether/api/flows/",
		"/ether/api/indexer-sites", "/ether/api/indexer-sites/", "/ether/api/indexer-usage", "/ether/api/indexer-usage/",
		"/ether/api/livetv", "/ether/api/livetv/", "/ether/api/markers", "/ether/api/markers/",
		"/ether/api/mdblist", "/ether/api/mdblist/", "/ether/api/metadata", "/ether/api/metadata/",
		"/ether/api/notifications", "/ether/api/notifications/", "/ether/api/onair", "/ether/api/onair/",
		"/ether/api/playback", "/ether/api/playback/", "/ether/api/plugin-tests", "/ether/api/plugin-tests/",
		"/ether/api/plugins", "/ether/api/plugins/", "/ether/api/plugins-setup", "/ether/api/libraries/", "/ether/api/adoption/backups/",
		"/ether/api/quality", "/ether/api/quality/", "/ether/api/releases", "/ether/api/releases/",
		"/ether/api/safety/diagnostics", "/ether/api/safety/diagnostics/", "/ether/api/safety/policies", "/ether/api/safety/policies/",
		"/ether/api/server/restart", "/ether/api/simkl", "/ether/api/simkl/", "/ether/api/stats", "/ether/api/stats/",
		"/ether/api/stream-preferences", "/ether/api/stream-preferences/", "/ether/api/subtitle-sync", "/ether/api/subtitle-sync/",
		"/ether/api/updates", "/ether/api/viewer-plex-defaults", "/ether/api/viewer-plex-defaults/")
	add("console", "*", "fallback 404 for unregistered API paths", "/ether/api/")
	add("console", http.MethodGet, "requires an active Plex account-link PIN issued by plex.tv",
		"/ether/api/auth/pin/{handle}", "/ether/api/me/link/plex/{handle}", "/ether/api/me/pin/{handle}", "/ether/api/plex/pin/{id}")
	add("console", http.MethodGet, "requires an external metadata provider configured for matching", "/ether/api/catalog/title/{key}/matches")
	add("console", http.MethodGet, "requires a linked Plex Media Server with imported accounts",
		"/ether/api/plex-servers/{id}/accounts", "/ether/api/plex-servers/{id}/accounts/{account}/backfill",
		"/ether/api/plex-servers/{id}/unmatched")
	add("console", http.MethodGet, "owner log SSE requires a log producer and stream scenario", "/ether/api/logs/stream")
	add("console", http.MethodPost, "only reachable before setup with no active owner", "/ether/api/setup/local")
	add("console", http.MethodPost, "takes the fixture out of its configured state", "/ether/api/setup/reopen")
	add("console", http.MethodPost, "applies a staged backup and replaces the serving database",
		"/ether/api/adoption/backups/restore/apply/{id}", "/ether/api/adoption/backups/restore/undo")
	add("subsonic", "*", "requires a resolvable music asset from a provider; the Music fixture revokes outbound access",
		"/subsonic/rest/stream.view", "/subsonic/rest/download.view")
	add("plex", http.MethodGet, "Plex Companion notification channels require a persistent subscribed client",
		"/:/eventsource/notifications", "/:/websocket/notifications", "/:/websockets/notifications")
	add("plex", http.MethodHead, "Plex Companion notification channels require a persistent subscribed client",
		"/:/eventsource/notifications", "/:/websocket/notifications", "/:/websockets/notifications")
	add("console", http.MethodPost, "requires a linked Plex Media Server to sync accounts or history",
		"/ether/api/plex-servers/{id}/sync", "/ether/api/plex-servers/{id}/accounts/{account}/backfill-in",
		"/ether/api/plex-servers/{id}/accounts/{account}/backfill-out")
	add("console", http.MethodPost, "requires a Plex account or PIN provisioned by plex.tv",
		"/ether/api/auth/pin", "/ether/api/me/link/plex", "/ether/api/plex/claim", "/ether/api/plex/pin",
		"/ether/api/plex/friends/sync")
	add("console", http.MethodPost, "installs a release and may replace or restart the running server", "/ether/api/updates/apply")
	const transcode = "/{ttype:video|music|audio|subtitles}/:/transcode/universal/"
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		add("plex", method, "requires a resolved media source or a running transcoder; both are absent from the fixture",
			transcode+"start", transcode+"start.m3u8", transcode+"start.mpd",
			transcode+"start.{ext:mkv|mp3|aac|m4a|flac|opus|ogg|wav|mp4}",
			transcode+"session/{session}/{stream:[0-9]+}/header", transcode+"session/{session}/{stream:[0-9]+}/{segment}",
			transcode+"session/{session}/*", transcode+"subtitles", "/subtitles/:/transcode/universal/start",
			"/library/parts/{part}", "/library/parts/{part}/{file}", "/library/parts/{part}/{changestamp:[0-9]+}/{file}",
			"/library/parts/{part}/{changestamp}/", "/library/parts/{part}/indexes/{index}",
			"/library/parts/{part}/indexes/{index}/{offset}", "/downloadQueue/{queue}/item/{item}/media")
		add("plex", method, "requires an active live tune and transport session; the fixture contains guide data only",
			"/livetv/sessions/{session}", "/livetv/sessions/{session}/{consumer}/dash/{rep}/{part}",
			"/livetv/sessions/{session}/{consumer}/index.m3u8", "/livetv/sessions/{session}/{consumer}/relay/{segment}",
			"/livetv/sessions/{session}/{consumer}/resource/{resource}")
	}
	add("plex", http.MethodDelete, "requires an active live tune session", "/livetv/sessions/{session}")
	for _, surface := range []string{"jellyfin", "emby"} {
		prefix := "/" + surface
		add(surface, http.MethodGet, "requires a configured external subtitle provider and matching release",
			prefix+"/Items/{id}/RemoteSearch/Subtitles/{language}")
		add(surface, http.MethodPost, "downloads an upstream subtitle or resolves metadata from an external provider",
			prefix+"/Items/{id}/RemoteSearch/Subtitles/{subtitleId}", prefix+"/Items/RemoteSearch/Apply/{itemId}",
			prefix+"/Items/RemoteSearch/Movie", prefix+"/Items/RemoteSearch/Series")
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			add(surface, method, "requires an open playback session or an actual media file, neither is available in this fixture",
				prefix+"/Audio/{id}/{name}", prefix+"/Items/{id}/Download", prefix+"/Items/{id}/File", prefix+"/Items/{id}/theme.m4a",
				prefix+"/Videos/{id}/CatchUp/{session}/{file}", prefix+"/Videos/{id}/Trickplay/{width}/{name}",
				prefix+"/Videos/{id}/{name}", prefix+"/Videos/{id}/{source}/Subtitles/{index}/{name}",
				prefix+"/Videos/{id}/{source}/Subtitles/{index}/{start}/{name}",
				prefix+"/Items/{id}/{source}/Subtitles/{index}/{name}",
				prefix+"/sessions/lazy/{path...}", prefix+"/sessions/media/{path...}")
		}
		add(surface, http.MethodGet, "notification websocket needs an authenticated persistent client and event producer",
			prefix+"/embywebsocket", prefix+"/socket")
	}
	return allowed
}

func perfMatrixInventory(t *testing.T, a *App, surfaces map[string]bool, subsonic []string) perfharness.MatrixRoutes {
	t.Helper()
	inventory := perfharness.MatrixRoutes{}
	if surfaces["plex"] {
		inventory["plex"] = plex.RouteInventory().Routes
		if len(inventory["plex"]) == 0 {
			t.Fatal("Plex route inventory is empty")
		}
	}
	for surface, prefix := range map[string]string{
		"jellyfin": "/jellyfin", "emby": "/emby", "silo": "/silo", "stremio": "/stremio", "xtream": "/xtream", "vela": "",
	} {
		if !surfaces[surface] {
			continue
		}
		inst, _ := a.frontends.registry.Instance(frontend.Protocol(surface))
		source, ok := inst.(frontend.RouteSource)
		if !ok {
			t.Fatalf("%s frontend route inventory unavailable", surface)
		}
		routes := source.Routes()
		if len(routes) == 0 {
			t.Fatalf("%s frontend route inventory is empty", surface)
		}
		for i := range routes {
			routes[i].Pattern = prefix + routes[i].Pattern
		}
		inventory[surface] = routes
	}
	if surfaces["console"] {
		if a.adminRoutes == nil {
			t.Fatal("owner API route inventory unavailable")
		}
		for _, entry := range a.adminRoutes.Routes() {
			if len(entry.Pattern) >= 5 && entry.Pattern[:5] == "/api/" {
				entry.Pattern = "/ether" + entry.Pattern
				inventory["console"] = append(inventory["console"], entry)
			}
		}
		if len(inventory["console"]) == 0 {
			t.Fatal("owner API route inventory is empty")
		}
	}
	if surfaces["plugin-host"] {
		if a.pluginsRT != nil {
			inventory["plugin-host"], _ = route.Registered(a.pluginsRT.pluginHost)
		}
		if len(inventory["plugin-host"]) == 0 {
			t.Fatal("plugin host route inventory unavailable")
		}
	}
	if surfaces["subsonic"] && len(subsonic) == 0 {
		t.Fatal("Subsonic route inventory unavailable")
	}
	for _, name := range subsonic {
		inventory["subsonic"] = append(inventory["subsonic"], frontend.Route{Method: "*", Pattern: "/subsonic/rest/" + name + ".view"})
	}
	return inventory
}
