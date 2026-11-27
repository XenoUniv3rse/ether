package perfharness

import (
	"encoding/json"
	"net/http"
	"strings"
)

func addMatrixConsoleCoverage(c *Client) {
	for i := range c.Steps {
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
		"/discovery/overlays/variants/{consoleKey}", "/frontends/jellyfin/api-keys",
		"/frontends/plex/health", "/frontends/plex/settings", "/frontends/plex/status",
		"/libraries/{pluginLibrary}", "/libraries/{pluginLibrary}/cache",
		"/config/server", "/status/catalog", "/row-sources/facets/{pluginLibrary}/genre",
		"/libraries/{pluginLibrary}/tabs",
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
	const spec = `{"name":"Matrix scratch collection","mediaType":"movie","sources":[{"kind":"titles","keys":["{consoleMovieKey}"]}],"sort":"source","refresh":"weekly","enabled":true,"artwork":{"mode":"custom","url":"/p.jpg"}}`
	for _, step := range []Step{
		{Name: "fixture POST preview collection", Method: http.MethodPost, Path: "/collections/preview?n=1", Body: spec},
		{Name: "fixture POST create collection", Method: http.MethodPost, Path: "/collections", Body: spec, Discover: discoverMatrixCollection},
		{Name: "fixture GET scratch collection", Path: "/collections/{scratchCollection}"},
		{Name: "fixture GET scratch collection artwork", Path: "/collections/{scratchCollection}/artwork?w=500"},
		{Name: "fixture GET scratch collection usage", Path: "/collections/{scratchCollection}/usage"},
		{Name: "fixture PUT scratch collection", Method: http.MethodPut, Path: "/collections/{scratchCollection}", Body: spec},
		{Name: "fixture POST rebuild scratch collection", Method: http.MethodPost, Path: "/collections/{scratchCollection}/rebuild"},
		{Name: "fixture DELETE scratch collection", Method: http.MethodDelete, Path: "/collections/{scratchCollection}"},
		{Name: "fixture POST install indexer definition", Method: http.MethodPost, Path: "/indexer-sites/catalog/install", Body: `{"id":"fakepublic"}`},
		{Name: "fixture GET installed indexer definition", Path: "/indexer-sites/definitions/fakepublic"},
		{Name: "fixture POST update indexer definitions", Method: http.MethodPost, Path: "/indexer-sites/update"},
		{Name: "fixture DELETE installed indexer definition", Method: http.MethodDelete, Path: "/indexer-sites/definitions/fakepublic"},
	} {
		step.Class = classConsole
		step.Headers = matrixConsoleWriteHeader
		c.Steps = append(c.Steps, step)
	}
}

func matrixConsoleWriteHeader(h http.Header, _ map[string]string) {
	h.Set("X-Ether-Request", "1")
}

func discoverMatrixCollection(vars map[string]string, body []byte) {
	var result struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &result) == nil {
		setOnce(vars, "scratchCollection", result.ID)
	}

}

func addMatrixMediaBrowserCoverage(c *Client) {
	for i := range c.Steps {
		if c.Steps[i].Path == "/Library/VirtualFolders?userId={userId}" {
			c.Steps[i].OwnerOnly = true
		}
		if c.Steps[i].Name == "season Episodes" && c.Steps[i].Discover == nil {
			c.Steps[i].Discover = discoverMBItem("episodeId", "Episode")
		}
	}
	c.Steps = append(c.Steps, Step{Name: "Roku episode anchor", Class: classDetail, Path: "/Shows//Episodes?StartItemId={episodeId}&Limit=2"})

	for _, path := range []string{
		"/DisplayPreferences/usersettings?client=etherperf", "/Items/{movieId}/Ancestors",
		"/Items/{movieId}/Collections", "/Items/{movieId}/Intros",
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
		"/UserViews/GroupingOptions", "/Users/Me", "/Users/Public", "/Users/{userId}", "/SyncPlay/List",
		"/Videos/{movieId}/AdditionalParts", "/Years", "/Years/2020",
	} {
		c.Steps = append(c.Steps, Step{Name: "fixture GET " + path, Class: classDetail, Path: path, registeredOnly: true})
		if strings.Contains(path, "/Images/Primary") {
			c.Steps = append(c.Steps, Step{Name: "fixture HEAD " + path, Class: classDetail, Method: http.MethodHead, Path: path, registeredOnly: true})
		}
	}
	for _, path := range []string{
		"/ScheduledTasks", "/System/ActivityLog/Entries?Limit=20",
		"/System/Info/Storage", "/System/Configuration/Encoding",
		"/web/ConfigurationPages", "/Library/MediaFolders", "/Auth/Keys",
		"/Users", "/Users/Query",
	} {
		c.Steps = append(c.Steps, Step{Name: "fixture owner GET " + path, Class: classBrowse, Path: path, OwnerOnly: true, registeredOnly: true})
	}

	if c.Surface == "jellyfin" {
		c.Steps = append(c.Steps, Step{Name: "fixture GET user root", Class: classDetail, Path: "/Users/{userId}/Items/Root"})
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
	for _, path := range []string{
		"/UserPlayedItems/{movieId}", "/UserFavoriteItems/{movieId}", "/UserItems/{movieId}/Rating?Likes=true",
		"/Users/{userId}/PlayedItems/{movieId}", "/Users/{userId}/FavoriteItems/{movieId}", "/Users/{userId}/Items/{movieId}/Rating?Likes=true",
	} {
		c.Steps = append(c.Steps,
			Step{Name: "fixture POST " + path, Class: classState, Method: http.MethodPost, Path: path, registeredOnly: true},
			Step{Name: "fixture DELETE " + path, Class: classState, Method: http.MethodDelete, Path: path, registeredOnly: true},
		)
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
	const page = "X-Plex-Container-Start=0&X-Plex-Container-Size=24"
	for _, path := range []string{
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
		"/hubs/metadata/{movieKey}", "/hubs/metadata/{movieKey}/related",
		"/hubs/sections/{movieSection}/continueWatching/items?" + page,
		"/hubs/sections/{movieSection}/popular?" + page, "/hubs/sections/{movieSection}/promoted?count=12",
		"/playlists/all?playlistType=video", "/playlists/sorts", "/clients", "/security/resources",
		"/status/sessions", "/transcode/sessions", "/updater/status",
	} {
		c.Steps = append(c.Steps,
			Step{Name: "fixture GET " + path, Class: classDetail, Path: path, registeredOnly: true},
			Step{Name: "fixture HEAD " + path, Class: classDetail, Method: http.MethodHead, Path: path, registeredOnly: true},
		)
	}
	for _, path := range []string{
		"/:/prefs", "/accounts", "/devices", "/butler",
		"/statistics/bandwidth", "/statistics/media", "/statistics/resources",
		"/history?X-Plex-Container-Start=0&X-Plex-Container-Size=24",
		"/status/sessions/history/all?X-Plex-Container-Start=0&X-Plex-Container-Size=24",
	} {
		c.Steps = append(c.Steps,
			Step{Name: "fixture owner GET " + path, Class: classBrowse, Path: path, OwnerOnly: true, registeredOnly: true},
			Step{Name: "fixture owner HEAD " + path, Class: classBrowse, Method: http.MethodHead, Path: path, OwnerOnly: true, registeredOnly: true},
		)
	}
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
