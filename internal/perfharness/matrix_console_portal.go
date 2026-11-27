package perfharness

import (
	"encoding/json"
	"net/http"
	"net/url"
)

func matrixConsolePortalScenarios() []Step {
	portal := func(h http.Header, vars map[string]string) {
		h.Del("X-Ether-Admin-Token")
		h.Set("Authorization", "Bearer "+vars["consoleScratchPortalToken"])
	}
	return []Step{
		{Name: "fixture scratch portal login", Method: http.MethodPost, Path: "/me/login",
			Body: `{"username":"matrix-console-scratch","password":"Matrix-local-002-fixture"}`, Discover: discoverConsoleScratchPortalToken},
		{Name: "fixture create portal invitation", Method: http.MethodPost, Path: "/me/invite", Headers: portal, Discover: discoverConsoleInvite},
		{Name: "fixture redeem portal invitation", Method: http.MethodPost, Path: "/me/invite/redeem", Body: `{"secret":"{consolePortalInvite}"}`, Discover: discoverConsoleScratchPortalToken},
		{Name: "fixture set portal episode alerts", Method: http.MethodPost, Path: "/me/favorite-episode-alert", Headers: portal, Body: `{"enabled":false}`},
		{Name: "fixture set portal favorites mirror", Method: http.MethodPost, Path: "/me/favorites-mirror", Headers: portal, Body: `{"enabled":true}`},
		{Name: "fixture create portal IPTV grant", Method: http.MethodPost, Path: "/me/apps/xtream", Headers: portal, Body: `{}`},
		{Name: "fixture discover portal IPTV grant", Path: "/me/devices", Headers: portal, Discover: discoverMatrixConsoleAppGrant},
		{Name: "fixture revoke portal IPTV grant", Method: http.MethodPost, Path: "/me/devices/revoke", Headers: portal, Body: `{"hash":"{consoleXtreamGrant}"}`},
		{Name: "fixture update portal credentials", Method: http.MethodPost, Path: "/me/password", Headers: portal,
			Body: `{"username":"matrix-console-scratch","password":"Matrix-local-004-fixture"}`},
	}
}

func discoverConsoleScratchPortalToken(vars map[string]string, body []byte) {
	var response struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(body, &response) == nil && response.Token != "" {
		vars["consoleScratchPortalToken"] = response.Token
	}
}

func discoverConsoleInvite(vars map[string]string, body []byte) {
	var response struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(body, &response) == nil {
		if u, err := url.Parse(response.URL); err == nil {
			if fragment, err := url.ParseQuery(u.Fragment); err == nil {
				setOnce(vars, "consolePortalInvite", fragment.Get("invite"))
			}
		}
	}
}

func discoverMatrixConsoleLockout(vars map[string]string, body []byte) {
	var response struct {
		Lockouts []struct {
			Key string `json:"key"`
			IP  string `json:"remoteIp"`
		} `json:"lockouts"`
	}
	if json.Unmarshal(body, &response) == nil {
		for _, lockout := range response.Lockouts {
			if lockout.IP == "192.0.2.1" {
				setOnce(vars, "consoleLockout", url.PathEscape(lockout.Key))
				return
			}
		}
	}
}

func discoverMatrixSupportDevice(vars map[string]string, body []byte) {
	var response struct {
		Devices []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"devices"`
	}
	if json.Unmarshal(body, &response) == nil {
		for _, device := range response.Devices {
			if device.Name == "Matrix fixture playback" {
				setOnce(vars, "consoleSupportDevice", device.ID)
				return
			}
		}
	}
}

func discoverMatrixReleaseVersion(vars map[string]string, body []byte) {
	var response struct {
		Version string `json:"current"`
	}
	if json.Unmarshal(body, &response) == nil {
		setOnce(vars, "consoleReleaseVersion", response.Version)
	}
}
