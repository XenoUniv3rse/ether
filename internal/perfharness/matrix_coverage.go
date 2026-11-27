package perfharness

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func addMatrixConsoleCoverage(c *Client) {
	for i := range c.Steps {
		if c.Steps[i].Name == "plugin library items" {
			c.Steps[i].Discover = combineDiscover(discoverPluginLibrary, discoverMatrixConsoleItem)
		}
		if c.Steps[i].Name == "catalog browse movies" {
			c.Steps[i].Discover = func(vars map[string]string, body []byte) {
				movie := map[string]string{}
				discoverConsoleKey(movie, body)
				setOnce(vars, "consoleMovieKey", movie["consoleKey"])
			}
		}
	}
	c.SignIn = &Step{
		Name: "viewer portal login", Class: classState, Method: http.MethodPost,
		Path: "/me/login", Headers: matrixConsoleWriteHeader, Body: `{"username":"{username}","password":"{password}"}`,
		Discover: func(vars map[string]string, body []byte) {
			var login struct {
				Token string `json:"token"`
			}
			if json.Unmarshal(body, &login) == nil {
				vars["token"] = login.Token
			}
		},
	}
	for _, path := range []string{"/me", "/me/apps", "/me/devices", "/me/favorite-episode-alert", "/me/favorites-mirror", "/me/identities", "/me/trakt"} {
		c.Steps = append(c.Steps, Step{Name: "GET " + path, Class: classConsole, Path: path, Headers: func(h http.Header, vars map[string]string) {
			h.Del("X-Ether-Admin-Token")
			h.Set("Authorization", "Bearer "+vars["token"])
		}})
	}
	c.Steps = append(c.Steps,
		Step{Name: "fixture On-Air assets", Class: classConsole, Path: "/onair/assets", Discover: discoverMatrixConsoleAssets},
		Step{Name: "fixture On-Air asset", Class: classConsole, Path: "/onair/assets/{consoleArtwork}"},
		Step{Name: "fixture On-Air asset info", Class: classConsole, Path: "/onair/assets/{consoleArtwork}/info"},
		Step{Name: "fixture On-Air asset thumb", Class: classConsole, Path: "/onair/assets/{consoleArtwork}/thumb"},
		Step{Name: "fixture On-Air font", Class: classConsole, Path: "/onair/fonts/notosans"},
		Step{Name: "fixture On-Air looks", Class: classConsole, Path: "/onair/looks", Discover: discoverMatrixConsoleLook},
		Step{Name: "fixture On-Air look", Class: classConsole, Path: "/onair/looks/{consoleLook}"},
		Step{Name: "fixture On-Air look pack", Class: classConsole, Path: "/onair/looks/{consoleLook}/pack"},
		Step{Name: "fixture On-Air showcase bug", Class: classConsole, Path: "/onair/showcase-bugs/chroma-80.png"},
		Step{Name: "fixture cache summary", Class: classConsole, Path: "/cache/", Discover: discoverMatrixConsoleCache},
		Step{Name: "fixture cached release", Class: classConsole, Path: "/cache/releases/{consoleRelease}"},
		Step{Name: "fixture playback history", Class: classConsole, Path: "/stats/history", Discover: discoverMatrixConsolePlay},
		Step{Name: "GET /catalog/lookup", Class: classConsole, Path: "/catalog/lookup?kind=network&q=HBO"},
		Step{Name: "GET /frontends/qr", Class: classConsole, Path: "/frontends/qr?url=http%3A%2F%2F127.0.0.1%3A32450%2Fvela"},
		Step{Name: "GET /image", Class: classConsole, Path: "/image?path=%2Fp.jpg&w=500"},
		Step{Name: "GET /image/generated", Class: classConsole, Path: "/image/generated?k={consoleKey}&t=Fixture&w=185"},
		Step{Name: "GET /onair/artwork", Class: classConsole, Path: "/onair/artwork?ref=%2Fp.jpg"},
		Step{Name: "GET /plex-servers/step-aside/explain", Class: classConsole, Path: "/plex-servers/step-aside/explain?key={consoleKey}"},
	)
	for _, path := range []string{
		"/catalog/title/{consoleKey}/library",
		"/catalog/title/{consoleMovieKey}/releases", "/catalog/title/{consoleKey}/schedule",
		"/catalog/title/{consoleKey}/tags", "/availability/title/{consoleKey}",
		"/discovery/overlays/artwork/{consoleKey}",
		"/discovery/overlays/variants/{consoleKey}", "/discovery/overlays/explain/{consoleMovieKey}?userId={viewerId}",
		"/discovery/overlays/preview/{consoleMovieKey}?userId={viewerId}",
		"/stats/plays/{consolePlay}", "/stats/plays/{consolePlay}/timeline", "/activity/plays/{consolePlay}/timeline", "/frontends/jellyfin/api-keys",
		"/frontends/plex/health", "/frontends/plex/settings", "/frontends/plex/status",
		"/libraries/{pluginLibrary}", "/libraries/{pluginLibrary}/cache",
		"/config/server", "/status/catalog", "/row-sources/facets/{pluginLibrary}/genre",
		"/libraries/{pluginLibrary}/tabs", "/libraries/{consoleLiveLibrary}/setup",
		"/libraries/{pluginLibrary}/item/{consoleItemRef}", "/libraries/{pluginLibrary}/item/{consoleItemRef}/match",
		"/plugins-setup/{consolePlugin}", "/libraries/{consoleLiveLibrary}/live?tab=fixture-live&limit=24",
		"/metadata/shows/{consoleKey}/groups", "/metadata/shows/{consoleKey}/release-numbering",
		"/metadata/shows/{consoleKey}/users/{viewerId}", "/metadata/shows/{consoleKey}/users/{viewerId}/preview",
		"/mdblist/{viewerId}/status", "/users/{viewerId}", "/users/{viewerId}/devices", "/users/{viewerId}/identities",
		"/viewers/{viewerId}/apps", "/viewers/{viewerId}/catalog-rules",
		"/viewers/{viewerId}/favorite-episode-alert", "/viewers/{viewerId}/favorites-mirror",
		"/viewers/{viewerId}/not-interested", "/viewers/{viewerId}/plex-servers",
		"/viewers/{viewerId}/plex-watch", "/viewers/{viewerId}/trakt",
	} {
		c.Steps = append(c.Steps, Step{Name: "fixture GET " + path, Class: classConsole, Path: path})
	}
	c.Steps = append(c.Steps, Step{Name: "viewer safety policy", Class: classConsole, Path: "/safety/policies/{viewerId}"})
	const draftHome = `{"mode":"curated","picks":false,"length":{},"rows":[]}`
	const spec = `{"name":"Matrix scratch collection","mediaType":"movie","sources":[{"kind":"titles","keys":["{consoleMovieKey}"]}],"sort":"source","refresh":"weekly","enabled":true,"artwork":{"mode":"custom","url":"/p.jpg"}}`
	for _, step := range []Step{
		{Name: "fixture local console login", Method: http.MethodPost, Path: "/auth/local", Body: `{"username":"{username}","password":"{password}"}`},
		{Name: "fixture exchange owner session", Method: http.MethodPost, Path: "/auth/session", Headers: func(h http.Header, vars map[string]string) {
			h.Set("X-Plex-Token", vars["ownerToken"])
		}, Discover: func(vars map[string]string, body []byte) {
			var result struct {
				Token string `json:"token"`
			}
			if json.Unmarshal(body, &result) == nil {
				setOnce(vars, "consoleScratchSession", result.Token)
			}
		}},
		{Name: "fixture sign out owner session", Method: http.MethodDelete, Path: "/auth/session", Headers: func(h http.Header, vars map[string]string) {
			h.Del("X-Ether-Admin-Token")
			h.Set("Authorization", "Bearer "+vars["consoleScratchSession"])
		}},
		{Name: "fixture request web player handoff", Method: http.MethodPost, Path: "/web-player?app=jellyfin"},
		{Name: "fixture create local viewer", Method: http.MethodPost, Path: "/users/local", Body: `{"username":"matrix-console-scratch","password":"Matrix-local-001-fixture"}`, Discover: discoverMatrixID("consoleScratchViewer")},
		{Name: "fixture patch local viewer", Method: http.MethodPatch, Path: "/users/{consoleScratchViewer}", Body: `{"title":"Matrix viewer","streamLimit":1}`},
		{Name: "fixture update local viewer", Method: http.MethodPut, Path: "/users/{consoleScratchViewer}", Body: `{"maxContentRating":"PG","streamLimit":2}`},
		{Name: "fixture change local viewer password", Method: http.MethodPost, Path: "/users/{consoleScratchViewer}/password", Body: `{"username":"matrix-console-scratch","password":"Matrix-local-002-fixture"}`},
		{Name: "fixture invite local viewer", Method: http.MethodPost, Path: "/users/{consoleScratchViewer}/invite"},
		{Name: "fixture issue IPTV app login", Method: http.MethodPost, Path: "/viewers/{consoleScratchViewer}/apps/xtream", Body: `{}`},
		{Name: "fixture discover IPTV grant", Path: "/users/{consoleScratchViewer}/devices", Discover: discoverMatrixConsoleAppGrant},
		{Name: "fixture reveal IPTV login", Method: http.MethodPost, Path: "/users/{consoleScratchViewer}/devices/xtream/reveal", Body: `{"hash":"{consoleXtreamGrant}"}`},
		{Name: "fixture update IPTV stream limit", Method: http.MethodPost, Path: "/users/{consoleScratchViewer}/devices/xtream/limit", Body: `{"hash":"{consoleXtreamGrant}","limit":1}`},
		{Name: "fixture revoke IPTV login", Method: http.MethodPost, Path: "/users/{consoleScratchViewer}/devices/revoke", Body: `{"hash":"{consoleXtreamGrant}"}`},
		{Name: "fixture pause local viewer", Method: http.MethodPost, Path: "/users/{consoleScratchViewer}/pause"},
		{Name: "fixture resume local viewer", Method: http.MethodPost, Path: "/users/{consoleScratchViewer}/resume"},
		{Name: "fixture sign out local viewer", Method: http.MethodPost, Path: "/users/{consoleScratchViewer}/signout"},
		{Name: "fixture create merge source viewer", Method: http.MethodPost, Path: "/users/local", Body: `{"username":"matrix-console-merge","password":"Matrix-local-003-fixture"}`, Discover: discoverMatrixID("consoleMergeSource")},
		{Name: "fixture merge local viewers", Method: http.MethodPost, Path: "/users/{consoleMergeSource}/merge", Body: `{"into":{consoleScratchViewer}}`},
		{Name: "fixture remove local viewer", Method: http.MethodDelete, Path: "/users/{consoleScratchViewer}"},
		{Name: "fixture explain title quality", Method: http.MethodPost, Path: "/quality/explain", Body: `{"userId":{viewerId},"clientId":"type:Apple TV","titleKey":"{consoleMovieKey}","remote":true}`},
		{Name: "fixture preview library visibility", Method: http.MethodPost, Path: "/catalog/visibility/preview", Body: `{"libraryId":{pluginLibrary},"visibilityDefault":"show","exclusions":[]}`},
		{Name: "fixture preview plugin live tab", Method: http.MethodPost, Path: "/libraries/{consoleLiveLibrary}/tabs/preview", Body: `{"key":"fixture-live","label":"Live","kind":"live"}`},
		{Name: "fixture ensure plugin library", Method: http.MethodPost, Path: "/plugins-setup/{consolePlugin}/ensure", Body: `{}`},
		{Name: "fixture save plugin setup settings", Method: http.MethodPut, Path: "/plugins-setup/{consolePlugin}/settings", Body: `{}`},
		{Name: "fixture pin cache release", Method: http.MethodPost, Path: "/cache/pin", Body: `{"releaseId":"{consoleRelease}","pinned":true}`},
		{Name: "fixture unpin cache release", Method: http.MethodPost, Path: "/cache/pin", Body: `{"releaseId":"{consoleRelease}","pinned":false}`},
		{Name: "fixture clear cache release", Method: http.MethodPost, Path: "/cache/clear", Body: `{"releaseId":"{consoleRelease}"}`},
		{Name: "fixture block cache release", Method: http.MethodPost, Path: "/cache/clear-block", Body: `{"releaseId":"{consoleBlockRelease}"}`},
		{Name: "fixture save warming pin", Method: http.MethodPost, Path: "/cache/warming", Body: `{"key":{consoleMovieKey},"pinned":true,"speculative":false}`},
		{Name: "fixture remove warming pin", Method: http.MethodPost, Path: "/cache/warming", Body: `{"key":{consoleMovieKey},"pinned":false}`},
		{Name: "fixture set title tags", Method: http.MethodPut, Path: "/catalog/title/{consoleMovieKey}/tags", Body: `{"tags":["Matrix fixture"]}`},
		{Name: "fixture clear title tags", Method: http.MethodPut, Path: "/catalog/title/{consoleMovieKey}/tags", Body: `{"tags":[]}`},
		{Name: "fixture clear remaining cache", Method: http.MethodPost, Path: "/cache/clear-all"},
		{Name: "fixture rename On-Air artwork", Method: http.MethodPatch, Path: "/onair/assets/{consoleArtwork}", Body: `{"name":"Matrix renamed artwork"}`},
		{Name: "fixture delete On-Air artwork", Method: http.MethodDelete, Path: "/onair/assets/{consoleArtwork}"},
		{Name: "fixture create On-Air reel", Method: http.MethodPost, Path: "/onair/reels", Body: `{"name":"Matrix reel","order":"sequence"}`, Discover: discoverMatrixID("consoleReel")},
		{Name: "fixture read On-Air reel", Path: "/onair/reels/{consoleReel}"},
		{Name: "fixture update On-Air reel", Method: http.MethodPut, Path: "/onair/reels/{consoleReel}", Body: `{"name":"Matrix renamed reel","order":"sequence"}`},
		{Name: "fixture delete On-Air reel", Method: http.MethodDelete, Path: "/onair/reels/{consoleReel}"},
		{Name: "fixture create support capture", Method: http.MethodPost, Path: "/support/captures", Body: `{"category":"playback","note":"Matrix fixture capture"}`, Discover: discoverMatrixID("consoleCapture")},
		{Name: "fixture read support capture", Path: "/support/captures/{consoleCapture}"},
		{Name: "fixture finish support capture", Method: http.MethodPost, Path: "/support/captures/{consoleCapture}/finish", Body: `{"includeTitles":false}`},
		{Name: "fixture read support archive", Path: "/support/captures/{consoleCapture}/archive"},
		{Name: "fixture discard support capture", Method: http.MethodDelete, Path: "/support/captures/{consoleCapture}"},
		{Name: "fixture create preroll", Method: http.MethodPost, Path: "/admin/prerolls", Body: `{"name":"Matrix fixture preroll","enabled":false}`, Discover: discoverMatrixID("consolePreroll")},
		{Name: "fixture read preroll plays", Path: "/admin/prerolls/{consolePreroll}/plays?limit=24"},
		{Name: "fixture update preroll", Method: http.MethodPut, Path: "/admin/prerolls/{consolePreroll}", Body: `{"name":"Matrix preroll updated","enabled":false}`},
		{Name: "fixture simulate preroll", Method: http.MethodPost, Path: "/admin/prerolls/simulate", Body: `{"ratingKey":"{consoleMovieKey}","viewerId":{viewerId},"device":"tv"}`},
		{Name: "fixture delete preroll", Method: http.MethodDelete, Path: "/admin/prerolls/{consolePreroll}"},
		{Name: "fixture preview catalogue library", Method: http.MethodPost, Path: "/libraries/preview", Body: `{"mediaType":"movie","sources":["tmdb-movies"],"visibilityDefault":"show","rules":[]}`},
		{Name: "fixture discover Home revision", Path: "/pages/home", Discover: discoverMatrixConsolePage},
		{Name: "fixture preview Home", Method: http.MethodPost, Path: "/pages/home/preview", Body: `{"viewer":{viewerId},"device":"tv","count":4}`},
		{Name: "fixture preview Home impact", Method: http.MethodPost, Path: "/pages/home/impact", Body: `{"device":"tv","page":` + draftHome + `}`},
		{Name: "fixture save curated Home", Method: http.MethodPut, Path: "/pages/home", Body: draftHome, Headers: func(h http.Header, vars map[string]string) {
			h.Set("If-Match", vars["matrixPageRevision"])
		}},
		{Name: "fixture reset Home", Method: http.MethodDelete, Path: "/pages/home"},
		{Name: "fixture POST preview collection", Method: http.MethodPost, Path: "/collections/preview?n=1", Body: spec},
		{Name: "fixture POST create collection", Method: http.MethodPost, Path: "/collections", Body: spec, Discover: discoverMatrixID("scratchCollection")},
		{Name: "fixture GET scratch collection", Path: "/collections/{scratchCollection}"},
		{Name: "fixture GET scratch collection artwork", Path: "/collections/{scratchCollection}/artwork?w=500"},
		{Name: "fixture GET scratch collection usage", Path: "/collections/{scratchCollection}/usage"},
		{Name: "fixture PUT scratch collection", Method: http.MethodPut, Path: "/collections/{scratchCollection}", Body: spec},
		{Name: "fixture POST rebuild scratch collection", Method: http.MethodPost, Path: "/collections/{scratchCollection}/rebuild"},
		{Name: "fixture create collection library", Method: http.MethodPost, Path: "/libraries/", Body: `{"name":"Matrix collection library","mediaType":"movie","collectionId":{scratchCollection}}`, Discover: discoverMatrixID("consoleScratchLibrary")},
		{Name: "fixture rename collection library", Method: http.MethodPatch, Path: "/libraries/{consoleScratchLibrary}", Body: `{"name":"Matrix renamed library"}`},
		{Name: "fixture customize collection library tabs", Method: http.MethodPut, Path: "/libraries/{consoleScratchLibrary}/tabs", Body: `{"tabs":[{"key":"library","label":"Library","kind":"library"}]}`},
		{Name: "fixture reset collection library tabs", Method: http.MethodDelete, Path: "/libraries/{consoleScratchLibrary}/tabs"},
		{Name: "fixture remove collection library", Method: http.MethodDelete, Path: "/libraries/{consoleScratchLibrary}"},
		{Name: "fixture DELETE scratch collection", Method: http.MethodDelete, Path: "/collections/{scratchCollection}"},
		{Name: "fixture POST install indexer definition", Method: http.MethodPost, Path: "/indexer-sites/catalog/install", Body: `{"id":"fakepublic"}`},
		{Name: "fixture GET installed indexer definition", Path: "/indexer-sites/definitions/fakepublic"},
		{Name: "fixture POST update indexer definitions", Method: http.MethodPost, Path: "/indexer-sites/update"},
		{Name: "fixture DELETE installed indexer definition", Method: http.MethodDelete, Path: "/indexer-sites/definitions/fakepublic"},
	} {
		step.Class = classConsole
		headers := step.Headers
		step.Headers = func(h http.Header, vars map[string]string) {
			matrixConsoleWriteHeader(h, vars)
			if headers != nil {
				headers(h, vars)
			}
		}
		c.Steps = append(c.Steps, step)
	}
	c.Steps = append(c.Steps, Step{Name: "console event stream", Class: classStatic, Path: "/stream", Stream: time.Minute, Accept: func(body []byte) bool {
		return strings.Contains(string(body), "retry: 10000")
	}})
}

func discoverMatrixConsoleItem(vars map[string]string, body []byte) {
	var page struct {
		Items []struct {
			Ref string `json:"ref"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &page) == nil && len(page.Items) > 0 {
		setOnce(vars, "consoleItemRef", url.PathEscape(page.Items[0].Ref))
	}
}

func discoverMatrixConsoleAssets(vars map[string]string, body []byte) {
	var assets []struct {
		ID   string `json:"digest"`
		Kind string `json:"kind"`
	}
	if json.Unmarshal(body, &assets) != nil {
		return
	}
	for _, asset := range assets {
		if asset.Kind == "image" {
			setOnce(vars, "consoleArtwork", url.PathEscape(asset.ID))
		}
	}
}

func discoverMatrixConsoleLook(vars map[string]string, body []byte) {
	var looks []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &looks) == nil && len(looks) != 0 {
		setOnce(vars, "consoleLook", url.PathEscape(looks[0].ID))
	}
}

func discoverMatrixConsoleCache(vars map[string]string, body []byte) {
	var result struct {
		Releases []struct {
			ReleaseID string `json:"releaseId"`
		} `json:"releases"`
	}
	if json.Unmarshal(body, &result) != nil {
		return
	}
	for _, release := range result.Releases {
		switch release.ReleaseID {
		case "matrix-cache-release":
			setOnce(vars, "consoleRelease", url.PathEscape(release.ReleaseID))
		case "matrix-cache-block":
			setOnce(vars, "consoleBlockRelease", url.PathEscape(release.ReleaseID))
		}
	}
}

func discoverMatrixConsolePlay(vars map[string]string, body []byte) {
	var plays []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &plays) == nil && len(plays) != 0 {
		setOnce(vars, "consolePlay", url.PathEscape(plays[0].ID))
	}
}

func matrixConsoleWriteHeader(h http.Header, _ map[string]string) {
	h.Set("X-Ether-Request", "1")
}

func discoverMatrixConsoleAppGrant(vars map[string]string, body []byte) {
	var response struct {
		Devices []struct {
			Hash    string `json:"hash"`
			Surface string `json:"surface"`
		} `json:"devices"`
	}
	if json.Unmarshal(body, &response) == nil {
		for _, device := range response.Devices {
			if device.Surface == "xtream" {
				setOnce(vars, "consoleXtreamGrant", device.Hash)
				return
			}
		}
	}
}

func discoverMatrixConsolePage(vars map[string]string, body []byte) {
	var document struct {
		Page struct {
			Revision int64 `json:"revision"`
		} `json:"page"`
	}
	if json.Unmarshal(body, &document) == nil {
		vars["matrixPageRevision"] = strconv.FormatInt(document.Page.Revision, 10)
	}
}

func addMatrixMediaBrowserCoverage(c *Client, live bool) {
	for i := range c.Steps {
		if c.Steps[i].Path == "/Library/VirtualFolders?userId={userId}" {
			c.Steps[i].OwnerOnly = true
		}
		if c.Steps[i].Name == "season Episodes" && c.Steps[i].Discover == nil {
			c.Steps[i].Discover = discoverMBItem("episodeId", "Episode")
		}
	}
	c.Steps = append(c.Steps,
		Step{Name: "fixture music view", Class: classBrowse, Path: "/Users/{userId}/Views", Discover: discoverMatrixMBMusicView},
		Step{Name: "fixture artists", Class: classBrowse, Path: "/Artists?ParentId={matrixMBMusic}&Limit=24", Discover: discoverMatrixMBArtist},
		Step{Name: "fixture artist by name", Class: classDetail, Path: "/Artists/{matrixMBArtist}"},
	)
	if c.Surface == "jellyfin" {
		c.Steps = append(c.Steps,
			Step{Name: "fixture tracks", Class: classBrowse, Path: "/Items?ParentId={matrixMBMusic}&Recursive=true&IncludeItemTypes=Audio&Limit=24", Discover: discoverMBItem("matrixMBTrack", "Audio")},
			Step{Name: "fixture track lyrics", Class: classDetail, Path: "/Audio/{matrixMBTrack}/Lyrics"},
		)
	}
	c.Steps = append(c.Steps, Step{Name: "Roku episode anchor", Class: classDetail, Path: "/Shows//Episodes?StartItemId={episodeId}&Limit=2"})

	for _, path := range []string{
		"/DisplayPreferences/usersettings?client=etherperf", "/Items/{movieId}/Ancestors",
		"/Items/{movieId}/Collections", "/Items/{movieId}/Intros", "/Items/{movieId}/InstantMix",
		"/Items/{movieId}/Similar?Limit=24", "/Items/{movieId}/ThemeMedia",
		"/Items/{movieId}/ThemeSongs", "/Items/{movieId}/ThemeVideos",
		"/Items/{movieId}/PlaybackInfo", "/Items/{movieId}/Images",
		"/Items/{movieId}/Images/Primary", "/Items/{movieId}/Images/Primary/0",
		"/MediaSegments/{movieId}", "/Users/{userId}/Items/{movieId}/Intros",
		"/Users/{userId}/Items/{movieId}/LocalTrailers", "/Users/{userId}/Items/{movieId}/SpecialFeatures",
		"/UserItems/{movieId}/UserData", "/Users/{userId}/HomeSections",
		"/Branding/Configuration", "/Branding/Css", "/Branding/Css.css", "/GetUtcTime",
		"/Localization/Options", "/Playback/BitrateTest?Size=1024", "/QuickConnect/Enabled",
		"/Sessions",
		"/System/Endpoint",
		"/System/Ping", "/System/WakeOnLanInfo", "/UserItems/Resume?Limit=24",
		"/UserViews", "/UserViews/GroupingOptions", "/Users/Me", "/Users/Public", "/Users/{userId}", "/SyncPlay/List",
		"/Videos/{movieId}/AdditionalParts", "/Years", "/Years/2020",
	} {
		c.Steps = append(c.Steps, Step{Name: "fixture GET " + path, Class: classDetail, Path: path, registeredOnly: true})
		if strings.Contains(path, "/Images/Primary") {
			c.Steps = append(c.Steps, Step{Name: "fixture HEAD " + path, Class: classDetail, Method: http.MethodHead, Path: path, registeredOnly: true})
		}
	}
	for _, path := range []string{
		"/UserImage", "/Users/{userId}/Images/Primary", "/Users/{userId}/Images/Primary/0",
	} {
		c.Steps = append(c.Steps,
			Step{Name: "fixture GET " + path, Class: classDetail, Path: path},
			Step{Name: "fixture HEAD " + path, Class: classDetail, Method: http.MethodHead, Path: path},
		)
	}
	for _, path := range []string{
		"/ScheduledTasks", "/System/ActivityLog/Entries?Limit=20",
		"/System/Info/Storage", "/System/Configuration/Encoding",
		"/web/ConfigurationPages", "/Library/MediaFolders", "/Auth/Keys",
		"/Users", "/Users/Query",
	} {
		c.Steps = append(c.Steps, Step{Name: "fixture owner GET " + path, Class: classBrowse, Path: path, OwnerOnly: true, registeredOnly: true})
	}

	if live {
		c.Steps = append(c.Steps,
			Step{Name: "fixture live channel list", Class: classGuide, Path: "/LiveTv/Channels?Limit=24", Discover: discoverMBItem("mbLiveChannel", "TvChannel")},
			Step{Name: "fixture live channel detail", Class: classDetail, Path: "/LiveTv/Channels/{mbLiveChannel}"},
			Step{Name: "fixture live programme list", Class: classGuide, Path: "/LiveTv/Programs?ChannelIds={mbLiveChannel}&Limit=24", Discover: discoverMBItem("mbLiveProgram", "Program")},
			Step{Name: "fixture live programme detail", Class: classDetail, Path: "/LiveTv/Programs/{mbLiveProgram}"},
			Step{Name: "fixture live programme query", Class: classGuide, Method: http.MethodPost, Path: "/LiveTv/Programs", Body: `{"ChannelIds":["{mbLiveChannel}"],"Limit":24}`},
		)
	}
	c.Steps = append(c.Steps,
		Step{Name: "fixture Quick Connect initiate", Class: classState, Method: http.MethodPost, Path: "/QuickConnect/Initiate", Discover: discoverMatrixQuickConnect},
		Step{Name: "fixture Quick Connect poll", Class: classDetail, Path: "/QuickConnect/Connect?secret={quickConnectSecret}"},
		Step{Name: "fixture Quick Connect approve", Class: classState, Method: http.MethodPost, Path: "/QuickConnect/Authorize?code={quickConnectCode}"},
		Step{Name: "fixture Quick Connect redeem", Class: classSignIn, Method: http.MethodPost, Path: "/Users/AuthenticateWithQuickConnect", Body: `{"Secret":"{quickConnectSecret}"}`, Discover: discoverMatrixMBAccess("mbQuickConnectToken")},
		Step{Name: "fixture Quick Connect logout", Class: classState, Method: http.MethodPost, Path: "/Sessions/Logout", Headers: func(h http.Header, vars map[string]string) {
			// The fixture grant belongs to this device, independently of the
			// original session the remainder of the replay continues using.
			token := vars["mbQuickConnectToken"]
			if token == "" {
				token = "matrix-missing-quick-connect-grant"
			}
			h.Set("X-Emby-Token", token)
		}},
	)
	if c.Surface == "jellyfin" {
		c.Steps = append(c.Steps,
			Step{Name: "fixture GET root", Class: classStatic, Path: "/"},
			Step{Name: "fixture GET web sign-in", Class: classStatic, Path: "/web/ether-signin"},
			Step{Name: "fixture redeem console web sign-in", Class: classSignIn, Method: http.MethodPost, Path: "/Ether/WebSignIn", Body: `{"Code":"{mbWebCode}","Browser":"etherperf-matrix-browser"}`},
			Step{Name: "fixture GET user root", Class: classDetail, Path: "/Users/{userId}/Items/Root"},
			Step{Name: "fixture client log upload", Class: classState, Method: http.MethodPost, Path: "/ClientLog/Document", Body: "Matrix fixture client log"},
		)
		addMatrixJellyfinParty(c)
	} else {
		c.Steps = append(c.Steps,
			Step{Name: "fixture GET Emby web", Class: classStatic, Path: "/web/"},
			Step{Name: "fixture Emby movie playback info", Class: classDetail, Method: http.MethodPost, Path: "/Items/{movieId}/PlaybackInfo?userId={userId}", Body: `{}`},
		)
	}
	c.Steps = append(c.Steps,
		Step{Name: "fixture POST display preferences", Class: classState, Method: http.MethodPost, Path: "/DisplayPreferences/etherperf?client=etherperf", Body: `{"SortOrder":"Descending"}`},
		Step{Name: "fixture POST ping", Class: classStatic, Method: http.MethodPost, Path: "/System/Ping", registeredOnly: true},
		Step{Name: "fixture POST capabilities", Class: classState, Method: http.MethodPost, Path: "/Sessions/Capabilities?PlayableMediaTypes=Video&SupportsMediaControl=true"},
		Step{Name: "fixture POST full capabilities", Class: classState, Method: http.MethodPost, Path: "/Sessions/Capabilities/Full", Body: `{"PlayableMediaTypes":["Video"],"SupportsMediaControl":true}`},
		Step{Name: "fixture POST create playlist", Class: classState, Method: http.MethodPost, Path: "/Playlists", Body: `{"Name":"Matrix scratch playlist","Ids":["{movieId}"]}`, Discover: discoverMatrixMBPlaylist},
		Step{Name: "fixture GET playlist", Class: classDetail, Path: "/Playlists/{matrixPlaylist}"},
		Step{Name: "fixture POST add playlist item", Class: classState, Method: http.MethodPost, Path: "/Playlists/{matrixPlaylist}/Items?Ids={episodeId}"},
		Step{Name: "fixture GET playlist items", Class: classDetail, Path: "/Playlists/{matrixPlaylist}/Items?Limit=24", Discover: discoverMatrixMBPlaylistEntry},
		Step{Name: "fixture POST move playlist item", Class: classState, Method: http.MethodPost, Path: "/Playlists/{matrixPlaylist}/Items/{matrixPlaylistEntry}/Move/1"},
		Step{Name: "fixture DELETE playlist item", Class: classState, Method: http.MethodDelete, Path: "/Playlists/{matrixPlaylist}/Items?EntryIds={matrixPlaylistEntry}"},
		Step{Name: "fixture DELETE playlist", Class: classState, Method: http.MethodDelete, Path: "/Playlists/{matrixPlaylist}"},
	)
	for _, method := range []string{http.MethodDelete, http.MethodPost} {
		app := "Matrix" + method
		user := "matrix-scratch-" + strings.ToLower(method)
		userID := "matrixMB" + method
		deleteUser := "/Users/{" + userID + "}"
		if method == http.MethodPost {
			deleteUser += "/Delete"
		}
		c.Steps = append(c.Steps,
			Step{Name: "fixture create viewer " + method, Class: classState, Method: http.MethodPost, Path: "/Users/New", Body: `{"Name":"` + user + `","Password":"Matrix-audio-002-fixture"}`, OwnerOnly: true, Discover: discoverMatrixID(userID)},
			Step{Name: "fixture viewer policy " + method, Class: classState, Method: http.MethodPost, Path: "/Users/{" + userID + "}/Policy", Body: `{"IsHidden":true}`, OwnerOnly: true},
			Step{Name: "fixture viewer password " + method, Class: classState, Method: http.MethodPost, Path: "/Users/{" + userID + "}/Password", Body: `{"NewPw":"Matrix-audio-003-fixture"}`, OwnerOnly: true},
			Step{Name: "fixture viewer password query " + method, Class: classState, Method: http.MethodPost, Path: "/Users/Password?userId={" + userID + "}", Body: `{"NewPw":"Matrix-audio-004-fixture"}`, OwnerOnly: true},
			Step{Name: "fixture delete viewer " + method, Class: classState, Method: method, Path: deleteUser, OwnerOnly: true},
		)
		remove := "/Auth/Keys/{mbScratchKey}"
		if method == http.MethodPost {
			remove += "/Delete"
		}
		c.Steps = append(c.Steps,
			Step{Name: "fixture API key create " + method, Class: classState, Method: http.MethodPost, Path: "/Auth/Keys?App=" + app, OwnerOnly: true},
			Step{Name: "fixture API key discover " + method, Class: classBrowse, Path: "/Auth/Keys", Discover: discoverMatrixMBKey(app), OwnerOnly: true},
			Step{Name: "fixture API key revoke " + method, Class: classState, Method: method, Path: remove, OwnerOnly: true},
		)
	}
	for _, path := range []string{
		"/UserPlayedItems/{movieId}", "/UserFavoriteItems/{movieId}", "/UserItems/{movieId}/Rating?Likes=true",
		"/Users/{userId}/PlayedItems/{movieId}", "/Users/{userId}/FavoriteItems/{movieId}", "/Users/{userId}/Items/{movieId}/Rating?Likes=true",
	} {
		c.Steps = append(c.Steps,
			Step{Name: "fixture POST " + path, Class: classState, Method: http.MethodPost, Path: path, registeredOnly: true},
			Step{Name: "fixture DELETE " + path, Class: classState, Method: http.MethodDelete, Path: path, registeredOnly: true},
		)
	}
	c.Steps = append(c.Steps,
		Step{Name: "fixture viewer subtitle configuration", Class: classState, Method: http.MethodPost, Path: "/Users/{userId}/Configuration", Body: `{"SubtitleMode":"Always","SubtitleLanguagePreference":"en"}`},
		Step{Name: "fixture discover owner movie", Class: classBrowse, OwnerOnly: true, Path: "/Items?IncludeItemTypes=Movie&Recursive=true&Limit=24", Discover: discoverMBItem("mbOwnerMovie", "Movie")},
		Step{Name: "fixture refresh movie metadata", Class: classState, OwnerOnly: true, Method: http.MethodPost, Path: "/Items/{mbOwnerMovie}/Refresh"},
		Step{Name: "fixture upload movie artwork", Class: classState, OwnerOnly: true, Method: http.MethodPost, Path: "/Items/{mbOwnerMovie}/Images/Primary?url=https%3A%2F%2Fimages.example.invalid%2Fmatrix%2Falbum.jpg"},
	)
	const report = `{"ItemId":"{episodeId}","RunTimeTicks":18000000000,"PositionTicks":1000000}`
	for _, path := range []string{"/PlayingItems/{episodeId}", "/Users/{userId}/PlayingItems/{episodeId}"} {
		c.Steps = append(c.Steps,
			Step{Name: "start compatibility timeline " + path, Class: classState, Method: http.MethodPost, Path: path, Body: report},
			Step{Name: "report compatibility timeline " + path, Class: classState, Method: http.MethodPost, Path: path + "/Progress", Body: report},
			Step{Name: "stop compatibility timeline " + path, Class: classState, Method: http.MethodDelete, Path: path, Body: report},
		)
	}
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Ping", "/Sessions/Playing/Stopped"} {
		c.Steps = append(c.Steps, Step{Name: "fixture " + path, Class: classState, Method: http.MethodPost, Path: path, Body: report})
	}
}

func addMatrixJellyfinParty(c *Client) {
	c.Steps = append(c.Steps,
		Step{Name: "SyncPlay create group", Class: classState, Method: http.MethodPost, Path: "/SyncPlay/New", Body: `{"GroupName":"Matrix group"}`},
		Step{Name: "SyncPlay list groups", Class: classDetail, Path: "/SyncPlay/List", Discover: func(vars map[string]string, body []byte) {
			var groups []struct {
				ID   string `json:"GroupId"`
				Name string `json:"GroupName"`
			}
			if json.Unmarshal(body, &groups) == nil {
				for _, group := range groups {
					if group.Name == "Matrix group" {
						setOnce(vars, "mbParty", group.ID)
						return
					}
				}
			}
		}},
	)
	for _, command := range []struct{ action, body string }{
		{"Join", `{"GroupId":"{mbParty}"}`},
		{"SetNewQueue", `{"PlayingQueue":["{movieId}","{episodeId}"],"PlayingItemPosition":0}`},
		{"Queue", `{"ItemIds":["{movieId}"]}`},
		{"Buffering", `{"PositionTicks":0,"IsPlaying":false}`},
		{"Ready", `{"PositionTicks":0,"IsPlaying":false}`},
		{"Ping", `{}`},
		{"SetIgnoreWait", `{"IgnoreWait":true}`},
		{"Pause", `{}`},
		{"Seek", `{"PositionTicks":1000000}`},
		{"Unpause", `{}`},
		{"SetRepeatMode", `{"Mode":"RepeatAll"}`},
		{"SetShuffleMode", `{"Mode":"Shuffle"}`},
		{"NextItem", `{}`},
		{"PreviousItem", `{}`},
		{"RemoveFromPlaylist", `{"ClearPlaylist":true}`},
		{"Stop", `{}`},
		{"Leave", `{}`},
	} {
		c.Steps = append(c.Steps, Step{Name: "SyncPlay " + command.action, Class: classState, Method: http.MethodPost, Path: "/SyncPlay/" + command.action, Body: command.body})
	}
}

func discoverMatrixMBAccess(variable string) func(map[string]string, []byte) {
	return func(vars map[string]string, body []byte) {
		var result struct {
			Access string `json:"AccessToken"`
		}
		if json.Unmarshal(body, &result) == nil {
			setOnce(vars, variable, result.Access)
		}
	}
}

func discoverMatrixMBMusicView(vars map[string]string, body []byte) {
	var response struct {
		Items []struct {
			Name string
			ID   string
		}
	}
	if json.Unmarshal(body, &response) == nil {
		for _, item := range response.Items {
			if item.Name == "Matrix Albums" {
				setOnce(vars, "matrixMBMusic", url.PathEscape(item.ID))
			}
		}
	}
}

func discoverMatrixMBArtist(vars map[string]string, body []byte) {
	var response struct {
		Items []struct {
			Name string
		}
	}
	if json.Unmarshal(body, &response) == nil && len(response.Items) != 0 {
		setOnce(vars, "matrixMBArtist", url.PathEscape(response.Items[0].Name))
	}
}

func discoverMatrixMBKey(app string) func(map[string]string, []byte) {
	return func(vars map[string]string, body []byte) {
		delete(vars, "mbScratchKey")
		var result struct {
			Items []struct {
				AppName     string
				AccessToken string
			}
		}
		if json.Unmarshal(body, &result) != nil {
			return
		}
		for _, item := range result.Items {
			if item.AppName == app && item.AccessToken != "" {
				vars["mbScratchKey"] = url.PathEscape(item.AccessToken)
				return
			}
		}
	}
}

func discoverMatrixQuickConnect(vars map[string]string, body []byte) {
	var response struct {
		Secret string
		Code   string
	}
	if json.Unmarshal(body, &response) == nil && response.Secret != "" && response.Code != "" {
		vars["quickConnectSecret"] = response.Secret
		vars["quickConnectCode"] = response.Code
	}
}

func discoverMatrixMBPlaylist(vars map[string]string, body []byte) {
	var result struct {
		ID string `json:"Id"`
	}
	if json.Unmarshal(body, &result) == nil {
		setOnce(vars, "matrixPlaylist", result.ID)
	}
}

func discoverMatrixMBPlaylistEntry(vars map[string]string, body []byte) {
	var result struct {
		Items []struct {
			ID string `json:"PlaylistItemId"`
		} `json:"Items"`
	}
	if json.Unmarshal(body, &result) == nil && len(result.Items) != 0 {
		setOnce(vars, "matrixPlaylistEntry", result.Items[0].ID)
	}

}

func addMatrixPlexCoverage(c *Client) {
	c.Steps = append(c.Steps,
		Step{Name: "fixture person image", Class: classDetail, Path: "/{plexPersonThumb}"},
		Step{Name: "fixture person image HEAD", Class: classDetail, Method: http.MethodHead, Path: "/{plexPersonThumb}"},
	)
	c.Steps = append(c.Steps, Step{Name: "sync compatibility", Class: classStatic, Path: "/sync"})
	const page = "X-Plex-Container-Start=0&X-Plex-Container-Size=24"
	for _, path := range []string{
		"/myplex/account", "/playQueues", "/search?query=star&limit=24",
		"/library/metadata/{movieKey}/subscription/modes", "/library/metadata/{movieKey}/users/top",
		"/photo/:/transcode?url=%2Flibrary%2Fmetadata%2F{movieKey}%2Fthumb&width=500&height=800",
		"/service/ultrablur/colors?url=%2Flibrary%2Fmetadata%2F{movieKey}%2Fthumb",
		"/services/ultrablur/colors?url=%2Flibrary%2Fmetadata%2F{movieKey}%2Fthumb",
		"/library/sections/{movieSection}", "/library/sections/{movieSection}/genre/Action?" + page,
		"/library/metadata/{movieKey}/thumb", "/library/metadata/{movieKey}/thumb/1",
		"/library/metadata/{episodeKey}/parent", "/library/metadata/{episodeKey}/grandparent",
		"/library/metadata/{showKey}/allLeaves?" + page, "/library/metadata/{showKey}/grandchildren?" + page,
		"/library/metadata/{showKey}/tree?" + page, "/library/metadata/{showKey}/onDeck",
		"/library/metadata/{movieKey}/related", "/library/metadata/{movieKey}/similar",
		"/library/metadata/{movieKey}/extras", "/library/metadata/{movieKey}/extras/all",
		"/library/metadata/{movieKey}/posters", "/library/metadata/{movieKey}/arts",
		"/library/metadata/{playlistKey}/clearLogos", "/library/metadata/{playlistKey}/themes",
		"/library/collections/{collectionKey}/items?" + page,
		"/library/collections/{collectionKey}/thumb/1?width=500",
		"/hubs/metadata/{movieKey}", "/hubs/metadata/{movieKey}/related",
		"/hubs/sections/{movieSection}/continueWatching/items?" + page,
		"/hubs/sections/{movieSection}/popular?" + page, "/hubs/sections/{movieSection}/promoted?count=12",
		"/playlists/all?playlistType=video", "/playlists/sorts", "/clients", "/security/resources",
		"/status/sessions", "/transcode/sessions", "/updater/status",
		"/library/metadata/{movieKey}/subtitles?language=en",
		"/library/metadata/{movieKey}/discoveryPoster/v1",
		"/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F{movieKey}&directPlay=1&directStream=1&session=matrix-decision",
	} {
		c.Steps = append(c.Steps,
			Step{Name: "fixture GET " + path, Class: classDetail, Path: path, registeredOnly: true},
			Step{Name: "fixture HEAD " + path, Class: classDetail, Method: http.MethodHead, Path: path, registeredOnly: true},
		)
	}
	for _, path := range []string{
		"/:/prefs", "/accounts", "/devices", "/butler", "/security/token",
		"/statistics/bandwidth", "/statistics/media", "/statistics/resources",
		"/history?X-Plex-Container-Start=0&X-Plex-Container-Size=24",
		"/status/sessions/history/all?X-Plex-Container-Start=0&X-Plex-Container-Size=24",
	} {
		c.Steps = append(c.Steps,
			Step{Name: "fixture owner GET " + path, Class: classBrowse, Path: path, OwnerOnly: true, registeredOnly: true},
			Step{Name: "fixture owner HEAD " + path, Class: classBrowse, Method: http.MethodHead, Path: path, OwnerOnly: true, registeredOnly: true},
		)
	}
	c.Steps = append(c.Steps,
		Step{Name: "owner discover movie libraries", Class: classBrowse, OwnerOnly: true, Path: "/library/sections", Discover: discoverPlexSections},
		Step{Name: "owner discover movie for matching", Class: classBrowse, OwnerOnly: true, Path: "/library/sections/{movieSection}/all?type=1&" + page, Discover: discoverPlexItem("matchMovieKey", "movie")},
		Step{Name: "owner movie matches", Class: classDetail, OwnerOnly: true, Path: "/library/metadata/{matchMovieKey}/matches"},
		Step{Name: "owner movie matches HEAD", Class: classDetail, Method: http.MethodHead, OwnerOnly: true, Path: "/library/metadata/{matchMovieKey}/matches"},
		Step{Name: "owner search movie matches via PUT", Class: classState, Method: http.MethodPut, OwnerOnly: true, Path: "/library/metadata/{matchMovieKey}/matches"},
		Step{Name: "owner set compatibility preferences", Class: classState, Method: http.MethodPut, OwnerOnly: true, Path: "/:/prefs?CinemaTrailersType=0"},
		Step{Name: "owner check compatibility updater", Class: classState, Method: http.MethodPut, OwnerOnly: true, Path: "/updater/check"},
		Step{Name: "owner refresh reachability", Class: classState, Method: http.MethodPut, OwnerOnly: true, Path: "/myplex/refreshReachability"},
		Step{Name: "owner refresh movie metadata", Class: classState, Method: http.MethodPut, OwnerOnly: true, Path: "/library/metadata/{matchMovieKey}/refresh"},
		Step{Name: "owner upload subtitle", Class: classState, Method: http.MethodPost, OwnerOnly: true, Path: "/library/metadata/{matchMovieKey}/subtitles?title=matrix.en.srt&format=srt&language=en", Body: "1\n00:00:00,000 --> 00:00:01,000\nMatrix fixture\n"},
		Step{Name: "owner search subtitles via PUT", Class: classState, Method: http.MethodPut, OwnerOnly: true, Path: "/library/metadata/{matchMovieKey}/subtitles?language=en"},
		Step{Name: "owner upload fixture poster", Class: classState, Method: http.MethodPost, OwnerOnly: true, Path: "/library/metadata/{matchMovieKey}/posters?url=https%3A%2F%2Fimages.example.invalid%2Fmatrix%2Falbum.jpg"},
		Step{Name: "owner upload fixture backdrop", Class: classState, Method: http.MethodPost, OwnerOnly: true, Path: "/library/metadata/{matchMovieKey}/arts?url=https%3A%2F%2Fimages.example.invalid%2Fmatrix%2Falbum.jpg"},
	)
	for _, item := range []struct{ leaf, artwork string }{{"art", "art"}, {"arts", "art"}, {"poster", "thumb"}, {"posters", "thumb"}} {
		c.Steps = append(c.Steps, Step{Name: "owner pick " + item.leaf, Class: classState, Method: http.MethodPut, OwnerOnly: true,
			Path: "/library/metadata/{matchMovieKey}/" + item.leaf + "?url=%2Flibrary%2Fmetadata%2F{matchMovieKey}%2F" + item.artwork})
	}
	for _, item := range []struct {
		path, array, field, variable string
	}{
		{"/accounts", "Account", "id", "plexAccount"},
		{"/devices", "Device", "id", "plexDevice"},
		{"/status/sessions/history/all", "Metadata", "historyKey", "plexHistory"},
	} {
		path := item.path
		if item.variable == "plexHistory" {
			path += "?" + page
		}
		c.Steps = append(c.Steps, Step{Name: "fixture discover " + item.variable, Class: classBrowse, Path: path, OwnerOnly: true,
			Discover: discoverMatrixPlexField(item.array, item.field, item.variable),
		})
	}
	for _, path := range []string{
		"/accounts/{plexAccount}", "/devices/{plexDevice}", "/status/sessions/history/{plexHistory}",
	} {
		c.Steps = append(c.Steps,
			Step{Name: "fixture owner GET " + path, Class: classDetail, Path: path, OwnerOnly: true},
			Step{Name: "fixture owner HEAD " + path, Class: classDetail, Method: http.MethodHead, Path: path, OwnerOnly: true},
		)
	}
	c.Steps = append(c.Steps, Step{Name: "owner delete recorded play", Class: classState, Method: http.MethodDelete, OwnerOnly: true, Path: "/status/sessions/history/{plexHistory}"})
	// Probe the matching HEAD handlers using the same real IDs and paging
	// parameters. One sample per registered pattern is sufficient here.
	for _, path := range []string{
		"/", "/library/metadata/{movieKey}", "/library/metadata/{showKey}/children",
		"/library/sections/{movieSection}/all?type=1&" + page,
		"/library/people/{actorId}", "/library/people/{actorId}/media",
		"/library/collections/{collectionKey}", "/library/collections/{collectionKey}/children?" + page,
		"/hubs/sections/{movieSection}?count=12", "/hubs/items/home.continue?" + page,
		"/hubs/metadata/{movieKey}/postplay", "/playlists?playlistType=video",
		"/playlists/{playlistKey}", "/playlists/{playlistKey}/items?" + page, "/livetv/dvrs",
	} {
		c.Steps = append(c.Steps, Step{Name: "fixture HEAD " + path, Class: classDetail, Method: http.MethodHead, Path: path, registeredOnly: true})
	}
}

func discoverMatrixPlexField(array, field, variable string) func(map[string]string, []byte) {
	return func(vars map[string]string, body []byte) {
		var response struct {
			MediaContainer map[string]json.RawMessage
		}
		if json.Unmarshal(body, &response) != nil {
			return
		}
		var entries []map[string]any
		if json.Unmarshal(response.MediaContainer[array], &entries) == nil {
			for _, entry := range entries {
				if value := jsonText(entry[field]); value != "" {
					if field == "historyKey" {
						value = strings.TrimPrefix(value, "/status/sessions/history/")
					}
					setOnce(vars, variable, url.PathEscape(value))
					return
				}
			}
		}
	}
}
