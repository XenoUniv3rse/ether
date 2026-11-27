package app

import (
	"maps"
	"net/http"
	"strings"
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
		"/ether/api/plex-servers/{id}/accounts/{account}/backfill",
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
		add(surface, http.MethodDelete, "requires an active encoding job and its device-bound playback session",
			prefix+"/Videos/ActiveEncodings")
		add(surface, http.MethodPost, "requires a resolved live source or open live stream lease",
			prefix+"/LiveStreams/Open", prefix+"/LiveStreams/MediaInfo", prefix+"/LiveStreams/Close")
		add(surface, http.MethodGet, "notification websocket needs an authenticated persistent client and event producer",
			prefix+"/embywebsocket", prefix+"/socket")
	}
	add("jellyfin", http.MethodPost, "requires a playlist entry ID delivered on the SyncPlay websocket",
		"/jellyfin/SyncPlay/MovePlaylistItem", "/jellyfin/SyncPlay/SetPlaylistItem")
	add("silo", http.MethodGet, "compatibility handler always reports no explicit per-series preference",
		"/silo/api/v2/audio-prefs/{series_id}", "/silo/api/v2/subtitle-prefs/{series_id}")
	add("silo", http.MethodGet, "requires a WebSocket upgrade with the corresponding scoped ticket",
		"/silo/api/v2/events/ws", "/silo/api/v2/playback/sessions/{session}/control/ws", "/silo/api/v2/watch-together/rooms/{id}/ws")
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		add("silo", method, "requires a resolved media source and playback session",
			"/silo/api/v2/playback/transcode/{session}/{path...}", "/silo/api/v2/stream/{path...}")
		add("xtream", method, "requires a working live stream or authorized recording source",
			"/xtream/live/{username}/{password}/{file}", "/xtream/streaming/timeshift.php",
			"/xtream/timeshift/{username}/{password}/{duration}/{start}/{file}")
		add("xtream", method, "requires a resolvable media file and a deliverable playback session",
			"/xtream/movie/{username}/{password}/{file}", "/xtream/series/{username}/{password}/{file}", "/xtream/sessions/{tail...}")
		add("plex", method, "Plex compatibility handler unconditionally returns 404", "/:/resources/*")
		add("plex", method, "Plex compatibility handler refuses the singular GET/HEAD match endpoint", "/library/metadata/{key}/match")
		add("vela", method, "requires a resolved reading or offline download file",
			"/vela/v0/download/{ticket}", "/vela/v0/read/{ticket}")
		add("vela", method, "requires media with generated trickplay sheets or a theme audio file",
			"/vela/v0/sessions/{id}/trickplay/{width}/{sheet}", "/vela/v0/themes/{secret}/theme.m4a", "/vela/v0/themes/{secret}/theme.mp3")
	}
	add("stremio", http.MethodGet, "requires a resolvable source to serve the bytes behind a lazy playback ticket", "/stremio/sessions/lazy/{rest...}")
	add("plex", http.MethodPut, "compatibility endpoint rejects changes through the PMS metadata or updater API",
		"/activities/{id}", "/updater/apply", "/library/metadata/{key}/analyze", "/library/metadata/{key}/prefs",
		"/library/metadata/{key}/clearLogos", "/library/metadata/{key}/themes", "/playlists/{key}/items/{item}")
	add("plex", http.MethodPost, "compatibility endpoint rejects changes through the PMS metadata or playlist import API",
		"/library/metadata/{key}/clearLogos", "/library/metadata/{key}/themes", "/library/metadata/{key}/extras", "/playlists/upload")
	add("plex", http.MethodDelete, "PMS activity cancellation and Butler tasks are unsupported", "/activities/{id}", "/butler")
	add("plex", http.MethodPost, "PMS Butler tasks are unsupported", "/butler")
	add("plex", http.MethodPut, "requires a provider match for the requested title", "/library/metadata/{key}/match")
	add("plex", http.MethodPost, "requires an enabled offline download source and a download queue",
		"/downloadQueue", "/downloadQueue/{queue}/add", "/downloadQueue/{queue}/items/{item}/restart")
	add("plex", http.MethodDelete, "requires an enabled offline download queue item", "/downloadQueue/{queue}/items/{item}")
	add("plex", http.MethodPost, "requires an actual resolved part with selectable media tracks", "/library/parts/{part}")
	add("plex", http.MethodPut, "requires a resolved part or bound playback stream", "/library/parts/{part}", "/library/streams/{stream}")
	add("plex", http.MethodDelete, "requires a bound uploaded subtitle stream", "/library/streams/{stream}")
	add("plex", http.MethodPost, "requires a usable live channel transport source", "/livetv/dvrs/{dvr}/channels/{channel}/tune")
	add("plex", http.MethodDelete, "requires an active live capture or recording subscription",
		"/media/grabbers/operations/{operation}", "/media/subscriptions/{subscription}")
	add("plex", "*", "compatibility stub for PMS scanner or Butler tasks that Ether does not implement",
		"/butler/{task}", "/library/sections/{section}/refresh")
	add("plex", "*", "requires an active playback session that can be terminated",
		"/status/sessions/terminate", "/status/sessions/terminate/player")
	add("plex", "*", "requires a running transcoder session",
		transcode+"ping", transcode+"stop")
	add("silo", http.MethodPost, "requires an open playback session with a resolved source",
		"/silo/api/v2/playback/start", "/silo/api/v2/playback/sessions/{session}/control/ws-ticket",
		"/silo/api/v2/playback/{session}/progress", "/silo/api/v2/playback/{session}/replan")
	add("silo", http.MethodDelete, "requires an open playback session", "/silo/api/v2/playback/{session}")
	add("silo", http.MethodPost, "requires a configured subtitle provider and a matching media release",
		"/silo/api/v2/subtitles/search", "/silo/api/v2/subtitles/download")
	add("plex", "*", "requires a paired Companion player publishing a playback timeline", "/player/*")
	add("plex", http.MethodGet, "requires a catch-up item published by a live provider", "/library/catchup/art/{key}/{kind}")
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		add("plex", method, "requires a cached Plex identity verified by the Plex metadata service", "/library/metadata/plex:/*")
		add("plex", method, "requires a theme audio asset for the requested series", "/library/metadata/{key}/theme/{version}")
		add("plex", method, "requires a bound media stream or an active transcode", "/library/streams/{stream}", "/transcode/sessions/{session}")
		add("plex", method, "requires an enabled offline download queue and resolved media",
			"/downloadQueue/{queue}", "/downloadQueue/{queue}/items", "/downloadQueue/{queue}/items/{item}",
			"/downloadQueue/{queue}/item/{item}/decision")
		add("plex", method, "requires a live stream subscription or tune operation",
			"/media/subscriptions/{subscription}", "/media/grabbers/operations/{operation}")
	}
	add("vela", http.MethodPost, "requires a published HTTPS origin and a handoff bound to that origin",
		"/vela/v0/auth/web-code", "/vela/v0/auth/web-code/redeem")
	add("vela", http.MethodPost, "requires a screen or surface declaring an invokable plugin action",
		"/vela/v0/screens/{id}/actions/{action}", "/vela/v0/surfaces/{id}/actions/{action}")
	add("vela", http.MethodPost, "requires a configured Plex account and working Plex Home upstream",
		"/vela/v0/auth/plex", "/vela/v0/auth/plex/profile", "/vela/v0/auth/plex/profiles/challenge")
	add("vela", http.MethodPost, "requires a configured playlist import provider and an import job",
		"/vela/v0/playlists/import", "/vela/v0/playlists/import/{id}/save")
	add("vela", http.MethodPost, "requires a configured AI search provider and a running job",
		"/vela/v0/search/ai", "/vela/v0/search/ai/{id}/cancel")
	add("vela", http.MethodPost, "requires a resolvable media source and open playback session",
		"/vela/v0/playback", "/vela/v0/libraries/{id}/live/play", "/vela/v0/sessions/{id}/audio-fallback",
		"/vela/v0/sessions/{id}/commands", "/vela/v0/sessions/{id}/observations")
	add("vela", http.MethodDelete, "requires an audio fallback child of an open playback session",
		"/vela/v0/sessions/{id}/audio-fallback/{audioId}")
	add("vela", http.MethodPost, "requires a title with an available offline download or book source",
		"/vela/v0/titles/{id}/download", "/vela/v0/titles/{id}/read", "/vela/v0/titles/{id}/reading-progress")
	add("vela", http.MethodPost, "requires a trailer provider and resolvable preview media", "/vela/v0/titles/{id}/trailer-preview")
	add("vela", http.MethodPost, "requires at least two playable live streams to form a multiview group", "/vela/v0/multiview")
	add("vela", http.MethodDelete, "requires an active multiview group", "/vela/v0/multiview/{groupId}")
	add("vela", http.MethodGet, "requires a playlist import job from a configured source", "/vela/v0/playlists/import/{id}")
	add("vela", http.MethodGet, "requires a configured AI search provider and a started job", "/vela/v0/search/ai/{id}")
	add("vela", http.MethodGet, "requires a playable source and an active live playback session", "/vela/v0/sessions/{id}", "/vela/v0/sessions/{id}/events")
	add("vela", http.MethodGet, "requires a trailer provider and an active trailer preview", "/vela/v0/titles/{id}/trailer-preview")
	add("console", http.MethodPost, "requires live upstream credentials or an outbound server test endpoint",
		"/ether/api/test/debrid", "/ether/api/test/indexer", "/ether/api/test/indexer-identity",
		"/ether/api/test/introdb", "/ether/api/test/llm", "/ether/api/test/opensubtitles",
		"/ether/api/test/skipdb", "/ether/api/test/subdl", "/ether/api/test/subsource",
		"/ether/api/test/tmdb", "/ether/api/test/trakt", "/ether/api/test/tvdb",
		"/ether/api/test/usenet", "/ether/api/test/{kind}/{id}/speed")
	add("console", http.MethodPost, "requires a configured Trakt account and device sign-in",
		"/ether/api/me/trakt/device", "/ether/api/me/trakt/poll",
		"/ether/api/viewers/{id}/trakt/device", "/ether/api/viewers/{id}/trakt/poll")
	add("console", http.MethodPost, "creates an interactive Plex sign-in PIN through plex.tv",
		"/ether/api/me/pin")
	add("console", http.MethodPost, "requires an external MDBList account and watchlist",
		"/ether/api/mdblist/{user}/connect", "/ether/api/mdblist/{user}/sync")
	add("console", http.MethodPost, "needs a configured external Plex account and media library",
		"/ether/api/admin/prerolls/import/pms", "/ether/api/viewers/import-plex",
		"/ether/api/viewers/import-plex/all", "/ether/api/viewers/match-plex",
		"/ether/api/viewers/{id}/plex-watch/pin", "/ether/api/viewers/{id}/plex-watch/pin/finish")
	add("console", http.MethodPost, "requires a configured AI provider",
		"/ether/api/catalog/interpret", "/ether/api/llm/models", "/ether/api/onair/draft")
	add("console", http.MethodPost, "requires a provider release with an actual stream or a cached offline media file",
		"/ether/api/cache/preload", "/ether/api/cache/vault/import", "/ether/api/cache/vault/{key}/check")
	add("console", http.MethodDelete, "requires a cached vault file owned by the server", "/ether/api/cache/vault/{key}")
	add("console", http.MethodPost, "requires an external add-on supplying a catalogue to preview or import",
		"/ether/api/collections/addon-imports", "/ether/api/collections/addon-imports/preview", "/ether/api/collections/addon-imports/{id}/sync")
	add("console", http.MethodDelete, "requires an installed external add-on catalogue import", "/ether/api/collections/addon-imports/{id}")
	add("console", http.MethodPost, "changes the locally installed player or downloader binary",
		"/ether/api/onair/downloader/install", "/ether/api/vela/install", "/ether/api/vela/rollback")
	add("console", http.MethodPost, "needs a configured alert delivery destination or an active heartbeat subscription",
		"/ether/api/notifications/{id}/test", "/ether/api/notifications/heartbeat/test")
	add("console", http.MethodPost, "restarts the test process or only runs during initial setup",
		"/ether/api/server/restart/{$}", "/ether/api/setup/complete")
	add("console", http.MethodPost, "requires a real video clip and working ffprobe or encoder",
		"/ether/api/admin/prerolls/{id}/video", "/ether/api/onair/reels/{id}/clips", "/ether/api/onair/assets/{id}/trim")
	add("console", http.MethodPost, "requires a configured content downloader and a resolvable source asset",
		"/ether/api/onair/assets/import", "/ether/api/onair/reels/{id}/clips/import", "/ether/api/onair/reels/{id}/clips/{clip}/resolve")
	add("console", http.MethodPut, "requires a clip already uploaded to the scratch reel", "/ether/api/onair/reels/{id}/clips/{clip}")
	add("console", http.MethodDelete, "requires a clip already uploaded to the scratch reel", "/ether/api/onair/reels/{id}/clips/{clip}")
	add("console", http.MethodPost, "requires a resolvable plugin live stream or active playback session",
		"/ether/api/libraries/{id}/live/play", "/ether/api/stats/sessions/{id}/stop")
	add("console", http.MethodDelete, "requires a live session with a working source",
		"/ether/api/libraries/{id}/live/sessions/{session}")
	add("console", http.MethodPost, "requires a second registered cluster node to receive the control action",
		"/ether/api/cluster/nodes/{id}/control")
	add("console", http.MethodPost, "requires a reachable Prowlarr instance and its credentials",
		"/ether/api/playback/prowlarr", "/ether/api/playback/prowlarr/tags")
	add("console", http.MethodPost, "requires a linked upstream Plex server and a Plex account or history source",
		"/ether/api/viewers/{id}/plex-watch/import", "/ether/api/viewers/{id}/plex-watch/export", "/ether/api/viewers/{id}/plex-watch/retry")
	add("console", http.MethodPut, "requires a verified source match from an external metadata provider",
		"/ether/api/catalog/title/{key}/match")
	add("console", http.MethodPost, "refreshes ratings from a configured upstream provider",
		"/ether/api/discovery/ratings/refresh/{key}")
	add("console", http.MethodPost, "requires a staged restore archive or snapshot made from a separate source instance",
		"/ether/api/adoption/backups/restore", "/ether/api/adoption/backups/restore/snapshot/{name}")
	add("console", http.MethodPost, "synchronously archives the full database and media state, outside the request replay window",
		"/ether/api/adoption/backups/{$}")
	add("console", http.MethodPost, "requires a pre-existing portable snapshot to stream in encrypted form",
		"/ether/api/adoption/backups/export/{name}")
	add("console", http.MethodDelete, "requires a report or staged archive from a completed restore-plan request",
		"/ether/api/adoption/backups/restore/report", "/ether/api/adoption/backups/restore/staged/{id}")
	add("console", http.MethodPost, "requires a legacy non-incremental SQLite database with enough reclaimable pages",
		"/ether/api/status/database/compact")
	add("console", http.MethodPost, "checks the release service, which is unavailable to the offline fixture",
		"/ether/api/updates/check", "/ether/api/vela/check")
	add("console", "*", "trailing-slash alias for the measured console event stream", "/ether/api/stream/")
	add("console", http.MethodGet, "requires a staged vault import from an owner media directory", "/ether/api/cache/vault/import/{id}")
	add("console", http.MethodGet, "requires a configured media source and uploaded preroll file",
		"/ether/api/admin/prerolls/{id}/preview", "/ether/api/admin/prerolls/{id}/thumb")
	add("console", http.MethodGet, "requires a source clip with locally encoded preview media",
		"/ether/api/onair/reels/{id}/clips/{clip}/preview")
	add("console", http.MethodGet, "requires an audio asset for waveform generation", "/ether/api/onair/assets/{id}/waveform")
	add("console", "*", "dispatches to the separately inventoried owner API leaf routes",
		"/ether/api/cluster/", "/ether/api/experience/", "/ether/api/plugins-setup/", "/ether/api/server/restart/", "/ether/api/updates/")
	add("console", "*", "dispatches a registered flow webhook token; the mount has no independent response", "/ether/api/flows/hooks/")
	add("console", "*", "requires an enabled external add-on", "/ether/api/addon-relay/")
	add("console", http.MethodGet, "requires an active live tune with a transport stream",
		"/ether/api/libraries/{id}/live/sessions/{session}/index.m3u8",
		"/ether/api/libraries/{id}/live/sessions/{session}/relay/{segment}",
		"/ether/api/libraries/{id}/live/sessions/{session}/resource/{resource}",
		"/ether/api/libraries/{id}/live/thumb/{hash}")
	add("console", http.MethodGet, "requires a configured PostgreSQL cluster and a registered node",
		"/ether/api/cluster/nodes/{id}/control", "/ether/api/cluster/nodes/{id}/role-preflight")
	add("plugin-host", http.MethodPost, "requires a configured AI completion provider", "/ether/plugin-host/ai/complete")
	add("plugin-host", http.MethodPost, "requires a viewer-bound request made by a mounted frontend adapter",
		"/ether/plugin-host/kinds/{kind}/browse", "/ether/plugin-host/kinds/{kind}/visible", "/ether/plugin-host/frontend/stream-url")
	add("plugin-host", http.MethodPost, "requires a resolvable media source",
		"/ether/plugin-host/media/probe", "/ether/plugin-host/sources/resolve")
	add("plugin-host", http.MethodPost, "requires a plugin with a declared screen provider",
		"/ether/plugin-host/screens/records", "/ether/plugin-host/screens/invalidate")
	return allowed
}

func matrixCaseRouteExceptions(inventory perfharness.MatrixRoutes, live bool) map[perfharness.MatrixRouteKey]string {
	if live {
		return perfMatrixRouteExceptions
	}
	exceptions := maps.Clone(perfMatrixRouteExceptions)
	for surface, routes := range inventory {
		for _, entry := range routes {
			path := entry.Pattern
			read := entry.Method == http.MethodGet || entry.Method == http.MethodHead
			reason := ""
			switch surface {
			case "plex":
				if read && (strings.HasPrefix(path, "/tv.plex.providers.epg.xmltv:1") ||
					strings.HasPrefix(path, "/media/grabbers") || strings.HasPrefix(path, "/media/subscriptions") ||
					(strings.HasPrefix(path, "/livetv/") && path != "/livetv/dvrs")) {
					reason = "Live TV is disabled in this configuration"
				}
			case "emby", "jellyfin":
				if (read && (path == "/"+surface+"/LiveTv/Channels/{id}" || path == "/"+surface+"/LiveTv/Programs/{id}")) ||
					(entry.Method == http.MethodPost && path == "/"+surface+"/LiveTv/Programs") {
					reason = "this configuration has no channel or programme fixture"
				}
			case "vela":
				if read && (path == "/vela/v0/live/stream" || path == "/vela/v0/surfaces/{id}/document" || path == "/vela/v0/surfaces/{id}/data") {
					reason = "the sports screen and live topic fixture are installed in live configurations"
				}
				if entry.Method == http.MethodPut && path == "/vela/v0/surfaces/{id}/setting" {
					reason = "the settable sports overlay fixture is installed in live configurations"
				}
				if entry.Method == http.MethodPost && path == "/vela/v0/live/guide/window" {
					reason = "this configuration has no channel fixture for a guide window"
				}
			}
			key := perfharness.MatrixRouteKey{Surface: surface, Method: entry.Method, Pattern: path}
			if reason != "" && exceptions[key] == "" {
				exceptions[key] = reason
			}
		}
	}
	return exceptions
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
