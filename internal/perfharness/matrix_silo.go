package perfharness

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
)

func addMatrixSiloCoverage(c *Client) {
	c.SignIn.Discover = discoverMatrixSiloAuth
	for i := range c.Steps {
		step := &c.Steps[i]
		switch step.Name {
		case "catalog movies title":
			step.Discover = combineDiscover(step.Discover, discoverMatrixSiloPlaylistTitles)
		case "catalog item movie":
			step.Discover = func(vars map[string]string, body []byte) {
				items, err := json.Marshal([]json.RawMessage{body})
				if err == nil {
					discoverMatrixArtwork(vars, items, "siloPoster", "/silo/api/v2/images/", "poster_url")
				}
			}
		case "home/sections":
			step.Discover = combineDiscover(step.Discover, discoverSiloCollection)
		}
	}
	roomHeader := func(h http.Header, vars map[string]string) {
		h.Set("X-Room-Token", vars["siloRoomToken"])
	}
	scratchDevice := func(h http.Header, _ map[string]string) {
		h.Set("X-Silo-Device-Id", h.Get("X-Silo-Device-Id")+"-scratch")
	}
	steps := []Step{
		{Name: "playback contract", Class: classStatic, Path: "/playback/capabilities", Discover: discoverMatrixSiloInstallation},
		{Name: "movie versions", Class: classDetail, Path: "/catalog/items/{movieId}/versions"},
		{Name: "home collection section", Class: classHome, Path: "/home/sections/{siloSection}/items"},
		{Name: "library collection items", Class: classBrowse, Path: "/library/{siloCollectionLibrary}/collections/{siloCollection}/items?limit=24"},
		{Name: "movie poster", Class: classDetail, Path: "/images/{siloPoster}"},
		{Name: "movie poster HEAD", Class: classDetail, Method: http.MethodHead, Path: "/images/{siloPoster}"},
		{Name: "scratch playlist create", Class: classState, Method: http.MethodPost, Path: "/playlists", Body: `{"name":"Matrix scratch playlist","content_ids":{siloPlaylistTitles}}`, Discover: discoverMatrixID("siloPlaylist")},
		{Name: "scratch playlist detail", Class: classDetail, Path: "/playlists/{siloPlaylist}"},
		{Name: "scratch playlist items", Class: classDetail, Path: "/playlists/{siloPlaylist}/items"},
		{Name: "scratch playlist add item", Class: classState, Method: http.MethodPost, Path: "/playlists/{siloPlaylist}/items", Body: `{"content_ids":["{seriesId}"]}`},
		{Name: "scratch playlist move item", Class: classState, Method: http.MethodPost, Path: "/playlists/{siloPlaylist}/items/{siloPlaylistMovie}/move", Body: `{"index":1}`},
		{Name: "scratch playlist remove item", Class: classState, Method: http.MethodDelete, Path: "/playlists/{siloPlaylist}/items/{siloPlaylistMovie}"},
		{Name: "scratch playlist delete", Class: classState, Method: http.MethodDelete, Path: "/playlists/{siloPlaylist}"},
		{Name: "set not interested", Class: classState, Method: http.MethodPut, Path: "/not-interested/{movieId}"},
		{Name: "get not interested", Class: classDetail, Path: "/not-interested/{movieId}"},
		{Name: "clear not interested", Class: classState, Method: http.MethodDelete, Path: "/not-interested/{movieId}"},
		{Name: "room create", Class: classState, Method: http.MethodPost, Path: "/watch-together/rooms", Body: `{"selection_mode":"host_pick"}`, Discover: discoverMatrixSiloRoom},
		{Name: "room reconnect", Class: classState, Method: http.MethodPost, Path: "/watch-together/join", Body: `{"code":"{siloRoomCode}"}`},
		{Name: "room state", Class: classDetail, Path: "/watch-together/rooms/{siloRoom}", Headers: roomHeader},
		{Name: "room policy", Class: classState, Method: http.MethodPatch, Path: "/watch-together/rooms/{siloRoom}/policy", Body: `{"guest_control_policy":"guest_play_pause"}`, Headers: roomHeader},
		{Name: "room socket ticket", Class: classState, Method: http.MethodPost, Path: "/watch-together/rooms/{siloRoom}/ws-ticket", Headers: roomHeader},
		{Name: "room stage a movie", Class: classState, Method: http.MethodPut, Path: "/watch-together/rooms/{siloRoom}/staged-selection", Body: `{"content_id":"{movieId}","library_id":"{movieLibrary}"}`, Headers: roomHeader},
		{Name: "room select a movie", Class: classState, Method: http.MethodPut, Path: "/watch-together/rooms/{siloRoom}/selection", Body: `{"content_id":"{movieId}","library_id":"{movieLibrary}"}`, Headers: roomHeader},
		{Name: "room start", Class: classState, Method: http.MethodPost, Path: "/watch-together/rooms/{siloRoom}/playback/start", Headers: roomHeader},
		{Name: "room stop", Class: classState, Method: http.MethodPost, Path: "/watch-together/rooms/{siloRoom}/playback/stop", Headers: roomHeader},
		{Name: "room close", Class: classState, Method: http.MethodDelete, Path: "/watch-together/rooms/{siloRoom}", Headers: roomHeader},
		{Name: "events socket ticket", Class: classState, Method: http.MethodPost, Path: "/events/ws-ticket"},
		{Name: "device login start", Class: classState, Method: http.MethodPost, Path: "/auth/device/start", Body: `{"device_name":"Matrix Apple TV","device_platform":"tvos","client_purpose":"device_login"}`, Discover: discoverMatrixSiloDevice},
		{Name: "device login poll", Class: classState, Method: http.MethodPost, Path: "/auth/device/poll", Body: `{"device_code":"{siloDeviceCode}"}`},
		{Name: "set series audio preference", Class: classState, Method: http.MethodPut, Path: "/audio-prefs/{seriesId}", Body: `{"audio_language":"en"}`},
		{Name: "clear series audio preference", Class: classState, Method: http.MethodDelete, Path: "/audio-prefs/{seriesId}"},
		{Name: "set series subtitle preference", Class: classState, Method: http.MethodPut, Path: "/subtitle-prefs/{seriesId}", Body: `{"subtitle_language":"en"}`},
		{Name: "clear series subtitle preference", Class: classState, Method: http.MethodDelete, Path: "/subtitle-prefs/{seriesId}"},
		{Name: "update scratch profile", Class: classState, Method: http.MethodPatch, Path: "/profiles/{siloProfile}", Body: `{"language":"en"}`},
		{Name: "set device auto-skip", Class: classState, Method: http.MethodPut, Path: "/settings/values/playback.auto_skip_intro?scope=profile_device", Body: `{"value":true}`},
		{Name: "clear device auto-skip", Class: classState, Method: http.MethodDelete, Path: "/settings/values/playback.auto_skip_intro?scope=profile_device"},
		{Name: "record route event", Class: classState, Method: http.MethodPost, Path: "/playback/route-events", Body: `{"installation_id":"{siloInstallation}","event_id":"matrix-event","protocol_version":3,"playback_attempt_id":"matrix-attempt","event":"plan_failed","diagnostics":{"error_cause":"ValidationError"}}`},
		{Name: "refresh session", Class: classState, Method: http.MethodPost, Path: "/auth/refresh", Body: `{"refresh_token":"{siloRefresh}"}`, Discover: discoverMatrixSiloAuth},
		{Name: "sign in scratch device", Class: classSignIn, Method: http.MethodPost, Path: "/auth/login", Body: c.SignIn.Body, Headers: scratchDevice, Discover: func(vars map[string]string, body []byte) {
			temp := make(map[string]string)
			discoverMatrixSiloAuth(temp, body)
			setOnce(vars, "siloScratchAccess", temp["token"])
		}},
		{Name: "sign out scratch device", Class: classState, Method: http.MethodPost, Path: "/auth/logout", Headers: func(h http.Header, vars map[string]string) {
			scratchDevice(h, vars)
			h.Set("Authorization", "Bearer "+vars["siloScratchAccess"])
		}},
	}
	for _, step := range steps {
		step.Path = "/api/v2" + step.Path
		c.Steps = append(c.Steps, step)
	}
	for i := range c.Steps {
		if c.Steps[i].Name == "watchlist movie" {
			step := Step{Name: "watchlist item", Class: classDetail, Path: "/api/v2/watchlist/{movieId}"}
			c.Steps = slices.Insert(c.Steps, i+1, step)
			break
		}
	}
}

func discoverSiloCollection(vars map[string]string, body []byte) {
	var result struct {
		Sections []struct {
			ID          string `json:"id"`
			Collections []struct {
				ID      string `json:"id"`
				Library string `json:"library_id"`
			} `json:"collections"`
		} `json:"sections"`
	}
	if json.Unmarshal(body, &result) != nil {
		return
	}
	for _, section := range result.Sections {
		for _, collection := range section.Collections {
			if section.ID != "" && collection.ID != "" && collection.Library != "" {
				setOnce(vars, "siloSection", url.PathEscape(section.ID))
				setOnce(vars, "siloCollection", url.PathEscape(collection.ID))
				setOnce(vars, "siloCollectionLibrary", url.PathEscape(collection.Library))
				return
			}
		}
	}
}

func discoverMatrixSiloRoom(vars map[string]string, body []byte) {
	var result struct {
		Token string `json:"room_access_token"`
		Room  struct {
			ID   string `json:"room_id"`
			Code string `json:"code"`
		} `json:"room"`
	}
	if json.Unmarshal(body, &result) == nil && result.Room.ID != "" && result.Token != "" {
		vars["siloRoom"] = url.PathEscape(result.Room.ID)
		vars["siloRoomToken"] = result.Token
		setOnce(vars, "siloRoomCode", result.Room.Code)
	}
}

func discoverMatrixSiloDevice(vars map[string]string, body []byte) {
	var result struct {
		Code string `json:"device_code"`
	}
	if json.Unmarshal(body, &result) == nil && result.Code != "" {
		vars["siloDeviceCode"] = result.Code
	}
}

// Use a full client page of real items so the playlist read exercises artwork
// and state projection at the same scale as other library listings.
func discoverMatrixSiloPlaylistTitles(vars map[string]string, body []byte) {
	var result struct {
		Items []struct {
			ID string `json:"content_id"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &result) != nil {
		return
	}
	titles := make([]string, 0, 24)
	for _, item := range result.Items {
		if item.ID != "" {
			titles = append(titles, item.ID)
			if len(titles) == cap(titles) {
				break
			}
		}
	}
	if len(titles) < 2 {
		return
	}
	encoded, err := json.Marshal(titles)
	if err == nil {
		vars["siloPlaylistTitles"] = string(encoded)
		vars["siloPlaylistMovie"] = url.PathEscape(titles[0])
	}
}

func discoverMatrixSiloAuth(vars map[string]string, body []byte) {
	var result struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		User    struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if json.Unmarshal(body, &result) == nil && result.Access != "" {
		vars["token"] = result.Access
		vars["siloRefresh"] = result.Refresh
		setOnce(vars, "siloProfile", url.PathEscape(result.User.ID))
	}
}

func discoverMatrixSiloInstallation(vars map[string]string, body []byte) {
	var result struct {
		InstallationID string `json:"installation_id"`
	}
	if json.Unmarshal(body, &result) == nil {
		setOnce(vars, "siloInstallation", result.InstallationID)
	}
}
