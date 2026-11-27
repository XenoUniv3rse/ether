package perfharness

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func addMatrixVelaCoverage(c *Client) {
	for i := range c.Steps {
		step := &c.Steps[i]
		switch step.Name {
		case "appearance":
			step.Discover = discoverMatrixVelaAppearance
		case "shows page":
			step.Discover = discoverMatrixVelaID("velaShow")
		case "movie detail":
			step.Discover = discoverMatrixVelaMovie
		case "recommendations rows":
			step.Discover = discoverMatrixVelaRows
		}
	}
	for _, asset := range []string{"velaPackAsset", "velaSkinDocument", "velaSkinAsset", "velaBrandingAsset"} {
		path := "/{" + asset + "}"
		c.Steps = append(c.Steps,
			Step{Name: "appearance " + asset, Class: classStatic, Path: path},
			Step{Name: "appearance HEAD " + asset, Class: classStatic, Method: http.MethodHead, Path: path},
		)
	}
	screenHeader := func(h http.Header, _ map[string]string) {
		h.Set("X-Vela-Screen-Renderer", "19")
	}
	const authDevice = `"device":{"id":"etherperf-vela-{username}-scratch","name":"Matrix fixture","platform":"web"}`
	steps := []Step{
		{Name: "negotiate client", Class: classState, Method: http.MethodPost, Path: "/negotiate", Body: `{"client":{"protocolVersions":[0],"device":{"id":"etherperf-vela-{username}","name":"etherperf","platform":"web","appVersion":"0.1.0"}}}`},
		{Name: "begin device login", Class: classState, Method: http.MethodPost, Path: "/auth/device", Body: `{` + authDevice + `}`, Discover: discoverMatrixVelaCode("velaDevice", "deviceCode", "userCode")},
		{Name: "approve device login", Class: classState, Method: http.MethodPost, Path: "/auth/device/approve", Body: `{"userCode":"{velaDeviceCode}"}`},
		{Name: "redeem device login", Class: classState, Method: http.MethodPost, Path: "/auth/device/poll", Body: `{` + authDevice + `,"deviceCode":"{velaDeviceSecret}"}`, Discover: discoverMatrixVelaScratchGrant},
		{Name: "begin Quick Connect", Class: classState, Method: http.MethodPost, Path: "/auth/quick-connect", Body: `{` + authDevice + `}`, Discover: discoverMatrixVelaCode("velaQC", "secret", "code")},
		{Name: "approve Quick Connect", Class: classState, Method: http.MethodPost, Path: "/auth/quick-connect/approve", Body: `{"code":"{velaQCCode}"}`},
		{Name: "redeem Quick Connect", Class: classState, Method: http.MethodPost, Path: "/auth/quick-connect/poll", Body: `{` + authDevice + `,"secret":"{velaQCSecret}"}`, Discover: discoverMatrixVelaScratchGrant},
		{Name: "rotate scratch grant", Class: classState, Method: http.MethodPost, Path: "/auth/refresh", Body: `{` + authDevice + `,"refreshToken":"{velaScratchRefresh}"}`, Discover: discoverMatrixVelaScratchGrant},
		{Name: "revoke scratch grant", Class: classState, Method: http.MethodPost, Path: "/auth/revoke", Headers: func(h http.Header, vars map[string]string) {
			h.Set("X-Vela-Device-Id", h.Get("X-Vela-Device-Id")+"-scratch")
			h.Set("Authorization", "Bearer "+vars["velaScratchAccess"])
		}},
		{Name: "clear movie version choice", Class: classState, Method: http.MethodDelete, Path: "/titles/{velaMovie}/versions/choice", Body: `{"scope":"title"}`},
		{Name: "appearance events", Class: classStatic, Path: "/events", Stream: time.Minute, Accept: func(body []byte) bool {
			return strings.Contains(string(body), "event: appearance")
		}},
		{Name: "fixture screen document", Class: classDetail, Path: "/screens/matrix-fixture/pages/index/document", Headers: screenHeader, Discover: discoverMatrixVelaScreenAsset},
		{Name: "fixture screen data", Class: classDetail, Path: "/screens/matrix-fixture/pages/index/data", Headers: screenHeader},
		{Name: "fixture screen asset", Class: classDetail, Path: "/{velaScreenAsset}"},
		{Name: "music live tab", Class: classBrowse, Path: "/libraries/{matrixMusicLibrary}/live?tab=fixture-live&limit=24"},
		{Name: "show children", Class: classBrowse, Path: "/titles/{velaShow}/children?limit=24"},
		{Name: "movie poster", Class: classDetail, Path: "/images/{velaPoster}"},
		{Name: "person detail", Class: classDetail, Path: "/people/{velaPerson}"},
		{Name: "person titles", Class: classBrowse, Path: "/people/{velaPerson}/titles?libraryId={movieLibrary}&limit=24"},
		{Name: "home row detail", Class: classHome, Path: "/rows/{velaHomeRow}?limit=24"},
		{Name: "record visible row item", Class: classState, Method: http.MethodPost, Path: "/interactions", Body: `{"interactions":[{"receiptToken":"{velaRowReceipt}","kind":"visible","visibleMilliseconds":1500}]}`},
		{Name: "watchlist a movie", Class: classState, Method: http.MethodPost, Path: "/titles/{velaMovie}/watch", Body: `{"watchlisted":true}`},
		{Name: "remove movie from watchlist", Class: classState, Method: http.MethodPost, Path: "/titles/{velaMovie}/watch", Body: `{"watchlisted":false}`},
		{Name: "collection contents", Class: classBrowse, Path: "/collections/{velaCollection}?limit=24"},
		{Name: "track lyrics", Class: classDetail, Path: "/titles/{matrixMusicTrack}/lyrics"},
		{Name: "track recording", Class: classDetail, Path: "/titles/{matrixMusicTrack}/recording"},
		{Name: "music duplicates", Class: classBrowse, Path: "/libraries/{matrixMusicLibrary}/duplicates?limit=24", Discover: discoverMatrixVelaDuplicate},
		{Name: "prefer recording release", Class: classState, Method: http.MethodPost, Path: "/libraries/{matrixMusicLibrary}/duplicates/{velaRecording}", Body: `{"preferredTitleId":"{velaPreferredRelease}"}`},
		{Name: "scratch playlist create", Class: classState, Method: http.MethodPost, Path: "/playlists", Body: `{"name":"Matrix scratch playlist","titleIds":["{velaMovie}"]}`, Discover: discoverMatrixID("velaPlaylist")},
		{Name: "scratch playlist add entry", Class: classState, Method: http.MethodPost, Path: "/playlists/{velaPlaylist}/entries", Body: `{"titleIds":["{velaMovie}"]}`},
		{Name: "scratch playlist detail", Class: classBrowse, Path: "/playlists/{velaPlaylist}?limit=24", Discover: discoverMatrixVelaEntry},
		{Name: "title playlists", Class: classBrowse, Path: "/titles/{velaMovie}/playlists"},
		{Name: "scratch playlist rename", Class: classState, Method: http.MethodPatch, Path: "/playlists/{velaPlaylist}", Body: `{"name":"Matrix renamed playlist"}`},
		{Name: "scratch playlist move entry", Class: classState, Method: http.MethodPost, Path: "/playlists/{velaPlaylist}/entries/{velaEntry}/move", Body: `{"afterEntryId":null}`},
		{Name: "scratch playlist remove entry", Class: classState, Method: http.MethodDelete, Path: "/playlists/{velaPlaylist}/entries/{velaEntry}"},
		{Name: "scratch playlist delete", Class: classState, Method: http.MethodDelete, Path: "/playlists/{velaPlaylist}"},
	}
	for _, step := range steps {
		if step.Method != "" {
			if step.Body == "" {
				step.Body = `{}`
			}
			key := "matrix-" + strings.ReplaceAll(step.Name, " ", "-")
			headers := step.Headers
			step.Headers = func(h http.Header, vars map[string]string) {
				if headers != nil {
					headers(h, vars)
				}
				h.Set("Idempotency-Key", key)
			}
		}
		c.Steps = append(c.Steps, step)
	}
}

func discoverMatrixVelaMovie(vars map[string]string, body []byte) {
	var result struct {
		Cast []struct {
			ID string `json:"id"`
		} `json:"cast"`
		Title struct {
			Artwork []struct {
				Kind  string `json:"kind"`
				Sizes []struct {
					URL string `json:"url"`
				} `json:"sizes"`
			} `json:"artwork"`
		} `json:"title"`
	}
	if json.Unmarshal(body, &result) != nil {
		return
	}
	for _, credit := range result.Cast {
		setOnce(vars, "velaPerson", url.PathEscape(credit.ID))
	}
	for _, image := range result.Title.Artwork {
		if image.Kind != "poster" || len(image.Sizes) == 0 {
			continue
		}
		u, err := url.Parse(image.Sizes[0].URL)
		if err != nil {
			continue
		}
		if ticket, ok := strings.CutPrefix(u.Path, "/vela/v0/images/"); ok && ticket != "" {
			setOnce(vars, "velaPoster", url.PathEscape(ticket)+"?w=500")
		}
	}
}

func discoverMatrixVelaRows(vars map[string]string, body []byte) {
	var result struct {
		Items []struct {
			ID    string `json:"id"`
			Items []struct {
				Kind    string `json:"kind"`
				ID      string `json:"id"`
				Receipt string `json:"receiptToken"`
			} `json:"items"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &result) != nil {
		return
	}
	for _, row := range result.Items {
		setOnce(vars, "velaHomeRow", url.PathEscape(row.ID))
		for _, item := range row.Items {
			if item.Kind == "title" {
				setOnce(vars, "velaRowReceipt", item.Receipt)
			}
			if item.Kind == "collection" {
				setOnce(vars, "velaCollection", url.PathEscape(item.ID))
			}
		}
	}
}

func discoverMatrixVelaEntry(vars map[string]string, body []byte) {
	var result struct {
		Items []struct {
			EntryID string `json:"entryId"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &result) == nil && len(result.Items) >= 2 {
		setOnce(vars, "velaEntry", url.PathEscape(result.Items[1].EntryID))
	}
}

func discoverMatrixVelaAppearance(vars map[string]string, body []byte) {
	var result struct {
		Branding struct {
			Wordmark struct {
				URL string `json:"url"`
			} `json:"wordmark"`
		} `json:"branding"`
		Packs []struct {
			Images map[string]string `json:"images"`
		} `json:"packs"`
		Documents []struct {
			URL     string `json:"url"`
			Preview string `json:"preview"`
		} `json:"documents"`
	}
	if json.Unmarshal(body, &result) != nil {
		return
	}
	setAsset := func(key, asset string) {
		if path, ok := strings.CutPrefix(asset, "/vela/v0/appearance/"); ok {
			setOnce(vars, key, "appearance/"+path)
		}
	}
	if path, ok := strings.CutPrefix(result.Branding.Wordmark.URL, "/vela/v0/branding/"); ok {
		setOnce(vars, "velaBrandingAsset", "branding/"+path)
	}
	for _, pack := range result.Packs {
		setAsset("velaPackAsset", pack.Images["preview"])
	}
	for _, skin := range result.Documents {
		setAsset("velaSkinDocument", skin.URL)
		setAsset("velaSkinAsset", skin.Preview)
	}
}

func discoverMatrixVelaScreenAsset(vars map[string]string, body []byte) {
	var result struct {
		Tree struct {
			Src string `json:"src"`
		} `json:"tree"`
	}
	if json.Unmarshal(body, &result) != nil {
		return
	}
	if asset, ok := strings.CutPrefix(result.Tree.Src, "/vela/v0/screen-assets/"); ok && asset != "" {
		setOnce(vars, "velaScreenAsset", "screen-assets/"+asset)
	}
}

func discoverMatrixVelaCode(prefix, secret, code string) func(map[string]string, []byte) {
	return func(vars map[string]string, body []byte) {
		var result map[string]json.RawMessage
		if json.Unmarshal(body, &result) == nil {
			var secretValue, codeValue string
			if json.Unmarshal(result[secret], &secretValue) == nil && json.Unmarshal(result[code], &codeValue) == nil {
				setOnce(vars, prefix+"Secret", secretValue)
				setOnce(vars, prefix+"Code", codeValue)
			}
		}
	}
}

func discoverMatrixVelaScratchGrant(vars map[string]string, body []byte) {
	type tokenPair struct {
		AccessToken  string
		RefreshToken string
	}
	var result struct {
		tokenPair
		Tokens tokenPair
	}
	if json.Unmarshal(body, &result) != nil {
		return
	}
	pair := result.tokenPair
	if pair.AccessToken == "" {
		pair = result.Tokens
	}
	if pair.AccessToken != "" && pair.RefreshToken != "" {
		vars["velaScratchAccess"] = pair.AccessToken
		vars["velaScratchRefresh"] = pair.RefreshToken
	}
}

func discoverMatrixVelaDuplicate(vars map[string]string, body []byte) {
	var page struct {
		Items []struct {
			Recording string `json:"recordingId"`
			Titles    []struct {
				ID string `json:"id"`
			} `json:"titles"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &page) != nil {
		return
	}
	for _, item := range page.Items {
		if item.Recording != "" && len(item.Titles) > 1 && item.Titles[0].ID != "" {
			setOnce(vars, "velaRecording", url.PathEscape(item.Recording))
			setOnce(vars, "velaPreferredRelease", item.Titles[0].ID)
			return
		}
	}
}
