package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/plextv"
)

// velaPlexHomeRoundTripper fakes the plex.tv endpoints the Plex Home
// profile flow uses: account validation, resources, Home listing and the
// PIN-checked Home switch.
type velaPlexHomeRoundTripper struct {
	mu        sync.Mutex
	machineID string
	accounts  map[string]plextv.Account
	pins      map[int64]string
}

func (f *velaPlexHomeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	machineID := f.machineID
	tok := r.Header.Get("X-Plex-Token")
	acct, known := f.accounts[tok]
	pins := f.pins
	f.mu.Unlock()

	reply := func(status int, contentType, body string) *http.Response {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}
	}
	switch {
	case r.URL.Path == "/api/v2/user":
		if !known {
			return reply(http.StatusUnauthorized, "application/json", `{"errors":[{"code":1001,"message":"User could not be authenticated"}]}`), nil
		}
		payload, _ := json.Marshal(map[string]any{
			"id": acct.ID, "uuid": acct.UUID, "username": acct.Username,
			"title": acct.DisplayName(), "home": acct.Home, "restricted": acct.Restricted,
		})
		return reply(http.StatusOK, "application/json", string(payload)), nil
	case r.URL.Path == "/api/v2/resources":
		if !known {
			return reply(http.StatusUnauthorized, "application/json", `{}`), nil
		}
		payload, _ := json.Marshal([]map[string]any{{
			"name": "Ether", "provides": "server", "clientIdentifier": machineID, "owned": acct.ID == 42,
		}})
		return reply(http.StatusOK, "application/json", string(payload)), nil
	case r.Method == http.MethodGet && r.URL.Path == "/api/home/users":
		if tok != "plex-owner-iphone" && tok != "server-secret-token" {
			return reply(http.StatusUnauthorized, "application/json", `{}`), nil
		}
		return reply(http.StatusOK, "application/xml", `<MediaContainer size="4">
<User id="42" uuid="u-owner" username="owner" title="Owner" thumb="https://plex.tv/users/owner/avatar?c=7" protected="1" homeAdmin="1"/>
<User id="300" uuid="u-kid" username="kid" title="Kid" thumb="https://plex.tv/users/kid/avatar?c=9" protected="1" restricted="1"/>
<User id="301" uuid="u-guest" username="guest" title="Guest" protected="0"/>
<User id="400" uuid="u-ghost" username="ghost" title="Ghost" protected="0"/>
</MediaContainer>`), nil
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/users/") && strings.HasSuffix(r.URL.Path, "/avatar"):
		// Avatars require the account token header server-side; the
		// client never holds it when following avatarUrl.
		if tok != "plex-owner-iphone" && tok != "server-secret-token" {
			return reply(http.StatusUnauthorized, "application/json", `{}`), nil
		}
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/users/"), "/avatar")
		return reply(http.StatusOK, "image/png", "\x89PNG-avatar-"+name), nil
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/home/users/") && strings.HasSuffix(r.URL.Path, "/switch"):
		if tok != "plex-owner-iphone" && tok != "server-secret-token" {
			return reply(http.StatusUnauthorized, "application/json", `{}`), nil
		}
		var id int64
		if _, err := fmt.Sscanf(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/home/users/"), "/switch"), "%d", &id); err != nil {
			return reply(http.StatusBadRequest, "application/json", `{}`), nil
		}
		if want := pins[id]; want != "" && r.URL.Query().Get("pin") != want {
			return reply(http.StatusUnauthorized, "application/xml", `<MediaContainer><error code="1001" message="Incorrect PIN"/></MediaContainer>`), nil
		}
		return reply(http.StatusOK, "application/xml", fmt.Sprintf(`<MediaContainer><User id="%d" authenticationToken="switched-%d"/></MediaContainer>`, id, id)), nil
	default:
		return reply(http.StatusNotFound, "application/json", `{}`), nil
	}
}

func TestVelaPlexHomeProfilesEndToEnd(t *testing.T) {
	fake := &velaPlexHomeRoundTripper{
		accounts: map[string]plextv.Account{
			"plex-owner-iphone": {ID: 42, UUID: "u-owner", Username: "owner", Title: "Owner"},
			"switched-42":       {ID: 42, UUID: "u-owner", Username: "owner", Title: "Owner"},
			"switched-300":      {ID: 300, UUID: "u-kid", Username: "kid", Title: "Kid", Home: true, Restricted: true},
			"switched-301":      {ID: 301, UUID: "u-guest", Username: "guest", Title: "Guest", Home: true},
		},
		pins: map[int64]string{42: "9999", 300: "1234"},
	}
	a := setupTestAppConfigured(t, true, Options{
		plextvHTTP: &http.Client{Transport: fake},
	}, func(c *config.Config) {
		c.Plex.Claimed = false
		c.Plex.OwnerPlexID = 0
	})
	fake.mu.Lock()
	fake.machineID = a.cfg.Read().Server.MachineIdentifier
	fake.mu.Unlock()

	ctx := t.Context()
	if err := a.cfg.Update(func(c *config.Config) error {
		c.Plex.Claimed = true
		c.Plex.OwnerPlexID = 42
		c.Plex.OwnerUsername = "owner"
		c.Plex.Token = "server-secret-token"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.users.BootstrapOwner(ctx, a.cfg.Read().Plex); err != nil {
		t.Fatal(err)
	}
	if _, known := a.users.PlexHomeSwitchAvailable(a.users.ActivePlexUserByPlexID(42)); known {
		t.Fatal("owner bootstrap alone should leave Home eligibility unknown")
	}
	if err := a.users.SyncFriends(ctx, []plextv.Friend{
		{Owned: true, PlexID: 42, Token: "owner-synced-token"},
		{PlexID: 300, UUID: "u-kid", Username: "kid", Title: "Kid", Token: "kid-synced-token", Home: true, Restricted: true, AllLibs: true},
		{PlexID: 301, UUID: "u-guest", Username: "guest", Title: "Guest", Token: "guest-synced-token", Home: true, AllLibs: true},
	}); err != nil {
		t.Fatal(err)
	}

	if err := a.frontends.control.listeners.start(ctx, "vela"); err != nil {
		t.Fatal(err)
	}
	velaInst, ok := a.frontends.registry.Instance("vela")
	if !ok {
		t.Fatal("vela frontend not mounted")
	}
	handler := velaInst.Handler()

	const device = `{"id":"iphone-home","name":"iPhone","platform":"iOS","appVersion":"0.2.0"}`
	post := func(path, body, access string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.60:54321"
		if access != "" {
			req.Header.Set("Authorization", "Bearer "+access)
			req.Header.Set("X-Vela-Device-Id", "iphone-home")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	getLib := func(access string) int {
		req := httptest.NewRequest(http.MethodGet, "/vela/v0/libraries", nil)
		req.Header.Set("Authorization", "Bearer "+access)
		req.Header.Set("X-Vela-Device-Id", "iphone-home")
		req.RemoteAddr = "198.51.100.60:54321"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// Capability advertised on the fully configured stack.
	capsReq := httptest.NewRequest(http.MethodGet, "/vela/v0/capabilities", nil)
	capsReq.RemoteAddr = "198.51.100.60:54321"
	capsRec := httptest.NewRecorder()
	handler.ServeHTTP(capsRec, capsReq)
	var caps map[string]any
	if err := json.Unmarshal(capsRec.Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	if caps["plexHomeProfiles"] != true || caps["plexAccountSignIn"] != true {
		t.Fatalf("capabilities = plexHomeProfiles %v plexAccountSignIn %v", caps["plexHomeProfiles"], caps["plexAccountSignIn"])
	}

	// Selection lists only active Ether viewers mapped to the Home:
	// Ghost (Plex 400, no viewer) is absent.
	selRes := post("/vela/v0/auth/plex", `{"plexToken":"plex-owner-iphone","selectProfile":true,"idempotencyKey":"home-sel-1","device":`+device+`}`, "")
	if selRes.Code != http.StatusOK {
		t.Fatalf("selection = %d: %s", selRes.Code, selRes.Body.String())
	}
	if strings.Contains(selRes.Body.String(), "plex-owner-iphone") || strings.Contains(selRes.Body.String(), "Ghost") {
		t.Fatalf("selection leaked token or unmapped member: %s", selRes.Body.String())
	}
	if strings.Contains(selRes.Body.String(), "plex.tv/users") {
		t.Fatalf("selection leaked a raw thumb URL: %s", selRes.Body.String())
	}
	var sel struct {
		SelectionToken string `json:"selectionToken"`
		ExpiresAt      string `json:"expiresAt"`
		Profiles       []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Protected  bool   `json:"protected"`
			Restricted *bool  `json:"restricted"`
			AvatarURL  string `json:"avatarUrl"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(selRes.Body.Bytes(), &sel); err != nil {
		t.Fatal(err)
	}
	if sel.SelectionToken == "" || sel.ExpiresAt == "" || len(sel.Profiles) != 3 {
		t.Fatalf("selection = %+v", sel)
	}
	byName := map[string]string{}
	protected := map[string]bool{}
	restricted := map[string]*bool{}
	avatars := map[string]string{}
	for _, p := range sel.Profiles {
		byName[p.Name] = p.ID
		protected[p.Name] = p.Protected
		restricted[p.Name] = p.Restricted
		avatars[p.Name] = p.AvatarURL
	}
	kidProfile, guestProfile := byName["Kid"], byName["Guest"]
	if kidProfile == "" || guestProfile == "" || !protected["Kid"] || protected["Guest"] {
		t.Fatalf("profiles = %+v", sel.Profiles)
	}
	// Kid is a restricted Home member with a proxied avatar; Guest has
	// no thumb upstream, so no avatarUrl and no restricted.
	if restricted["Kid"] == nil || !*restricted["Kid"] || restricted["Guest"] != nil {
		t.Fatalf("restricted = kid %v guest %v", restricted["Kid"], restricted["Guest"])
	}
	if !strings.HasPrefix(avatars["Kid"], "/vela/v0/images/plexavatar-") || avatars["Guest"] != "" {
		t.Fatalf("avatars = %+v", avatars)
	}
	// The avatar is fetchable before sign-in completes, proxied through
	// Ether with the Plex token applied server-side only.
	avatarReq := httptest.NewRequest(http.MethodGet, avatars["Kid"], nil)
	avatarReq.RemoteAddr = "198.51.100.60:54321"
	avatarRec := httptest.NewRecorder()
	handler.ServeHTTP(avatarRec, avatarReq)
	if avatarRec.Code != http.StatusOK || avatarRec.Header().Get("Content-Type") != "image/png" || !strings.Contains(avatarRec.Body.String(), "PNG-avatar-kid") {
		t.Fatalf("kid avatar = %d ct=%q body=%q", avatarRec.Code, avatarRec.Header().Get("Content-Type"), avatarRec.Body.String())
	}
	// Owner sorts first.
	if !protected[sel.Profiles[0].Name] || sel.Profiles[0].ID == kidProfile {
		// Name may be the username; just require the owner entry protected.
		ownerFound := false
		for name, id := range byName {
			if id != kidProfile && id != guestProfile && protected[name] {
				ownerFound = true
			}
		}
		if !ownerFound {
			t.Fatalf("owner profile missing: %+v", sel.Profiles)
		}
	}

	complete := func(profileID, pin, idem string) *httptest.ResponseRecorder {
		pinJSON := ""
		if pin != "" {
			pinJSON = fmt.Sprintf(`,"pin":%q`, pin)
		}
		return post("/vela/v0/auth/plex/profile", fmt.Sprintf(`{"selectionToken":%q,"profileId":%q%s,"idempotencyKey":%q,"device":%s}`, sel.SelectionToken, profileID, pinJSON, idem, device), "")
	}

	// Bad PIN -> 401 through the real plex.tv switch; good PIN -> kid Tokens.
	if w := complete(kidProfile, "0000", "home-bad-pin"); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad PIN = %d: %s", w.Code, w.Body.String())
	}
	kidRes := complete(kidProfile, "1234", "home-good-pin")
	if kidRes.Code != http.StatusOK {
		t.Fatalf("kid completion = %d: %s", kidRes.Code, kidRes.Body.String())
	}
	var kidTokens struct {
		AccessToken string `json:"accessToken"`
		Viewer      struct {
			Name string `json:"name"`
		} `json:"viewer"`
	}
	if err := json.Unmarshal(kidRes.Body.Bytes(), &kidTokens); err != nil {
		t.Fatal(err)
	}
	if kidTokens.Viewer.Name != "kid" || kidTokens.AccessToken == "" {
		t.Fatalf("kid tokens = %+v", kidTokens)
	}
	if got := getLib(kidTokens.AccessToken); got != http.StatusOK {
		t.Fatalf("kid libraries = %d", got)
	}
	// Completing the selection consumed its avatar capabilities.
	deadAvatarReq := httptest.NewRequest(http.MethodGet, avatars["Kid"], nil)
	deadAvatarReq.RemoteAddr = "198.51.100.60:54321"
	deadAvatarRec := httptest.NewRecorder()
	handler.ServeHTTP(deadAvatarRec, deadAvatarReq)
	if deadAvatarRec.Code != http.StatusConflict {
		t.Fatalf("avatar after completion = %d: %s", deadAvatarRec.Code, deadAvatarRec.Body.String())
	}

	// Switch profile as kid -> owner (PIN 9999), revoking kid's session.
	chal := post("/vela/v0/auth/plex/profiles/challenge", `{"idempotencyKey":"home-chal-1","device":`+device+`}`, kidTokens.AccessToken)
	if chal.Code != http.StatusOK {
		t.Fatalf("challenge = %d: %s", chal.Code, chal.Body.String())
	}
	var switchSel struct {
		SelectionToken string `json:"selectionToken"`
		Profiles       []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Protected  bool   `json:"protected"`
			Restricted *bool  `json:"restricted"`
			AvatarURL  string `json:"avatarUrl"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(chal.Body.Bytes(), &switchSel); err != nil || len(switchSel.Profiles) != 3 {
		t.Fatalf("switch selection = %+v %v", switchSel, err)
	}
	ownerProfile, ownerAvatar := "", ""
	for _, p := range switchSel.Profiles {
		if p.ID != kidProfile && p.ID != guestProfile {
			ownerProfile, ownerAvatar = p.ID, p.AvatarURL
		}
		if p.ID == kidProfile && (p.Restricted == nil || !*p.Restricted) {
			t.Fatalf("switch kid restricted = %v", p.Restricted)
		}
	}
	// Switch avatars proxy through the retained account link.
	switchAvatarReq := httptest.NewRequest(http.MethodGet, ownerAvatar, nil)
	switchAvatarReq.RemoteAddr = "198.51.100.60:54321"
	switchAvatarRec := httptest.NewRecorder()
	handler.ServeHTTP(switchAvatarRec, switchAvatarReq)
	if switchAvatarRec.Code != http.StatusOK || !strings.Contains(switchAvatarRec.Body.String(), "PNG-avatar-owner") {
		t.Fatalf("switch owner avatar = %d: %s", switchAvatarRec.Code, switchAvatarRec.Body.String())
	}
	ownerRes := post("/vela/v0/auth/plex/profile", fmt.Sprintf(`{"selectionToken":%q,"profileId":%q,"pin":"9999","idempotencyKey":"home-switch-1","device":%s}`, switchSel.SelectionToken, ownerProfile, device), "")
	if ownerRes.Code != http.StatusOK {
		t.Fatalf("switch completion = %d: %s", ownerRes.Code, ownerRes.Body.String())
	}
	var ownerTokens struct {
		AccessToken string `json:"accessToken"`
		Viewer      struct {
			Name string `json:"name"`
		} `json:"viewer"`
	}
	if err := json.Unmarshal(ownerRes.Body.Bytes(), &ownerTokens); err != nil {
		t.Fatal(err)
	}
	if ownerTokens.Viewer.Name != "owner" {
		t.Fatalf("owner tokens = %+v", ownerTokens)
	}
	if got := getLib(ownerTokens.AccessToken); got != http.StatusOK {
		t.Fatalf("owner libraries = %d", got)
	}
	if got := getLib(kidTokens.AccessToken); got != http.StatusUnauthorized {
		t.Fatalf("kid session after switch = %d, want 401 (revoked)", got)
	}

	checkSwitchCapability := func(path string, want bool, etag string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+ownerTokens.AccessToken)
		req.Header.Set("X-Vela-Device-Id", "iphone-home")
		req.Header.Set("If-None-Match", etag)
		req.RemoteAddr = "198.51.100.60:54321"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var doc struct {
			PlexHomeProfiles  bool `json:"plexHomeProfiles"`
			PlexAccountSignIn bool `json:"plexAccountSignIn"`
			Capabilities      *struct {
				PlexHomeProfiles  bool `json:"plexHomeProfiles"`
				PlexAccountSignIn bool `json:"plexAccountSignIn"`
			} `json:"capabilities"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &doc) != nil {
			t.Fatalf("%s status = %d, want capability document", path, rec.Code)
		}
		if doc.Capabilities != nil {
			doc.PlexHomeProfiles = doc.Capabilities.PlexHomeProfiles
			doc.PlexAccountSignIn = doc.Capabilities.PlexAccountSignIn
		}
		if doc.PlexHomeProfiles != want || !doc.PlexAccountSignIn {
			t.Fatalf("%s Home switching = %v, account sign-in = %v, want %v and true", path, doc.PlexHomeProfiles, doc.PlexAccountSignIn, want)
		}
		return rec.Header().Get("ETag")
	}
	capsTag := checkSwitchCapability("/vela/v0/capabilities", true, "")
	bootstrapTag := checkSwitchCapability("/vela/v0/bootstrap", true, "")
	for _, plexID := range []int64{300, 301} {
		if err := a.users.PauseViewer(ctx, a.users.ActivePlexUserByPlexID(plexID).ID); err != nil {
			t.Fatal(err)
		}
	}
	checkSwitchCapability("/vela/v0/capabilities", false, capsTag)
	checkSwitchCapability("/vela/v0/bootstrap", false, bootstrapTag)

	chal = post("/vela/v0/auth/plex/profiles/challenge", `{"idempotencyKey":"home-chal-solo","device":`+device+`}`, ownerTokens.AccessToken)
	var conflict struct {
		Error struct{ Code string } `json:"error"`
	}
	if chal.Code != http.StatusConflict || json.Unmarshal(chal.Body.Bytes(), &conflict) != nil || conflict.Error.Code != "no_other_profiles" {
		t.Fatalf("solo challenge = %d, code %q", chal.Code, conflict.Error.Code)
	}

	// Native Plex clients use the account service for Home listing and
	// switching. The PMS server still accepts the active owner's credential.
	plexInst, ok := a.frontends.registry.Instance("plex")
	if !ok {
		t.Fatal("plex frontend not mounted")
	}
	for _, route := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodGet, "/api/home/users", http.StatusNotFound},
		{http.MethodPost, "/api/home/users/42/switch", http.StatusNotFound},
	} {
		req := httptest.NewRequest(route.method, route.path, nil)
		req.Header.Set("X-Plex-Token", "owner-synced-token")
		rec := httptest.NewRecorder()
		plexInst.Handler().ServeHTTP(rec, req)
		if rec.Code != route.status {
			t.Fatalf("Plex %s %s returned %d, want %d", route.method, route.path, rec.Code, route.status)
		}
	}
}
