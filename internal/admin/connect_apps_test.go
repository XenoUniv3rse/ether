package admin_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/frontendadapter"
	"github.com/braedonsaunders/ether/internal/store"
)

type connectFrontends struct{}

func (connectFrontends) List(context.Context) ([]admin.FrontendInfo, error) {
	return []admin.FrontendInfo{{Descriptor: frontend.Descriptor{ID: "stremio"}, Enabled: true}, {Descriptor: frontend.Descriptor{ID: "webdav"}, Enabled: false}}, nil
}

type allConnectFrontends struct{}

func (allConnectFrontends) List(context.Context) ([]admin.FrontendInfo, error) {
	return []admin.FrontendInfo{
		{Descriptor: frontend.Descriptor{ID: "plex"}, Enabled: true, Status: frontend.Status{Mounted: true}},
		{Descriptor: frontend.Descriptor{ID: "jellyfin"}, Enabled: true, Status: frontend.Status{Mounted: true}},
		{Descriptor: frontend.Descriptor{ID: "emby"}, Enabled: true, Status: frontend.Status{Mounted: true}},
		{Descriptor: frontend.Descriptor{ID: "silo"}, Enabled: true},
		{Descriptor: frontend.Descriptor{ID: "stremio"}, Enabled: true},
		{Descriptor: frontend.Descriptor{ID: "webdav"}, Enabled: true},
		{Descriptor: frontend.Descriptor{ID: "m3u"}, Enabled: true},
		{Descriptor: frontend.Descriptor{ID: "xtream"}, Enabled: true},
	}, nil
}
func (allConnectFrontends) Settings(context.Context, frontend.Protocol) (frontend.Settings, error) {
	return nil, nil
}
func (allConnectFrontends) PutSettings(context.Context, frontend.Protocol, frontend.Settings) error {
	return nil
}
func (allConnectFrontends) Toggle(context.Context, frontend.Protocol, bool) error { return nil }

func TestConnectAppsFailsBeforeMintingWhenHTTPSUnavailable(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	users, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := identity.NewLocal(db)
	viewerID, err := local.CreateViewer(ctx, "viewer", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	if err = users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	grants, _ := identity.NewGrants(db, users, time.Hour, nil)
	portal, _ := grants.Issue(ctx, viewerID, nil, "viewer-portal", core.Device{})
	h := admin.NewHandler(admin.Deps{Config: newStore(t, nil), IdentityGrants: grants, Frontends: connectFrontends{},
		ViewerPortalURL:  func(context.Context, *http.Request) (string, error) { return "http://192.168.1.5/ether/me", nil },
		StremioPortalURL: func(context.Context, *http.Request) (string, error) { return "", errors.New("no certificate") },
	})
	r := req("POST", "/ether/api/me/apps/stremio", "")
	r.Header.Set("Authorization", "Bearer "+portal)
	w := serve(h, r)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "stremio_https_required") {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	devices, err := grants.List(ctx, viewerID)
	if err != nil || len(devices) != 1 || devices[0].Surface != "viewer-portal" {
		t.Fatalf("unexpected grant: %+v %v", devices, err)
	}
}
func (connectFrontends) Settings(context.Context, frontend.Protocol) (frontend.Settings, error) {
	return nil, nil
}
func (connectFrontends) PutSettings(context.Context, frontend.Protocol, frontend.Settings) error {
	return nil
}
func (connectFrontends) Toggle(context.Context, frontend.Protocol, bool) error { return nil }

func TestConnectAppsGrantScopeAndAuthorization(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	users, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := identity.NewLocal(db)
	ownerID, _ := local.CreateOwner(ctx, "owner", "password-1234")
	viewerID, _ := local.CreateViewer(ctx, "viewer", "password-1234")
	if err := users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	grants, _ := identity.NewGrants(db, users, time.Hour, nil)
	portal, _ := grants.Issue(ctx, viewerID, nil, "viewer-portal", core.Device{})
	owner, _ := grants.Issue(ctx, ownerID, nil, "owner", core.Device{})
	h := admin.NewHandler(admin.Deps{Config: newStore(t, nil), IdentityGrants: grants, Frontends: connectFrontends{}, ActiveViewer: users.ActiveUser,
		ViewerPortalURL: func(context.Context, *http.Request) (string, error) {
			return "https://ether.example/prefix/ether/me", nil
		},
		StremioPortalURL: func(context.Context, *http.Request) (string, error) {
			return "https://ether.example/prefix/ether/me", nil
		},
	})
	do := func(method, path, token string) *http.Response {
		r := req(method, path, "")
		r.Header.Set("Authorization", "Bearer "+token)
		return serve(h, r).Result()
	}
	res := do("GET", "/ether/api/me/apps", portal)
	var listing struct {
		Apps []string `json:"apps"`
	}
	_ = json.NewDecoder(res.Body).Decode(&listing)
	res.Body.Close()
	if res.StatusCode != 200 || len(listing.Apps) != 1 || listing.Apps[0] != "stremio" {
		t.Fatalf("listing: %d %+v", res.StatusCode, listing)
	}
	res = do("POST", "/ether/api/me/apps/stremio", portal)
	var created struct {
		URL string `json:"url"`
		QR  string `json:"qr"`
	}
	_ = json.NewDecoder(res.Body).Decode(&created)
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(created.URL, "https://ether.example/prefix/stremio/") || !strings.HasPrefix(created.QR, "data:image/png;base64,") {
		t.Fatalf("create: %d %q", res.StatusCode, created.URL)
	}
	secret := strings.TrimSuffix(strings.TrimPrefix(created.URL, "https://ether.example/prefix/stremio/"), "/manifest.json")
	viewer, err := grants.Resolve(ctx, "stremio", secret)
	if err != nil || viewer.ID != viewerID {
		t.Fatalf("grant: %+v %v", viewer, err)
	}
	if _, err := grants.Resolve(ctx, "webdav", secret); err == nil {
		t.Fatal("cross-surface grant accepted")
	}
	devices, err := grants.List(ctx, viewerID)
	if err != nil {
		t.Fatal(err)
	}
	var hash string
	for _, device := range devices {
		if device.Surface == "stremio" {
			hash = device.Hash
		}
	}
	if hash == "" {
		t.Fatal("grant absent from devices")
	}
	if err := grants.RevokeGrant(ctx, viewerID, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := grants.Resolve(ctx, "stremio", secret); err == nil {
		t.Fatal("revoked grant accepted")
	}
	for _, tc := range []struct {
		path, token string
		want        int
	}{
		{"/ether/api/me/apps/webdav", portal, 404},
		{"/ether/api/viewers/2/apps/stremio", portal, 403},
		{"/ether/api/viewers/999/apps/stremio", owner, 404},
	} {
		res = do("POST", tc.path, tc.token)
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Fatalf("%s: %d", tc.path, res.StatusCode)
		}
	}
	res = do("POST", "/ether/api/viewers/2/apps/stremio", owner)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("owner create: %d", res.StatusCode)
	}
	r := req("POST", "/ether/api/me/apps/stremio", "")
	r.Header.Set("Authorization", "Bearer "+portal)
	r.Header.Del("X-Ether-Request")
	if response := serve(h, r); response.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF: %d", response.Code)
	}
	if _, err := db.ExecContext(ctx, `UPDATE auth_users SET active=0 WHERE id=?`, viewerID); err != nil {
		t.Fatal(err)
	}
	if err := users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	res = do("POST", "/ether/api/me/apps/stremio", portal)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("inactive viewer: %d", res.StatusCode)
	}
}

func TestConnectAppsPlexViewerWithoutLocalPasswordMintsJellyfinEmbySilo(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	users, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,username,title,created_at,updated_at,active) VALUES(77,777,'plexviewer','Plex Viewer',0,0,1)`); err != nil {
		t.Fatal(err)
	}
	if err := users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, users, time.Hour, nil, newStore(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	portal, err := grants.Issue(ctx, 77, nil, "viewer-portal", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	h := admin.NewHandler(admin.Deps{
		Config:         newStore(t, nil),
		IdentityGrants: grants,
		Frontends:      allConnectFrontends{},
		ActiveViewer:   users.ActiveUser,
		ViewerPortalURL: func(context.Context, *http.Request) (string, error) {
			return "https://ether.example/ether/me", nil
		},
		StremioPortalURL: func(context.Context, *http.Request) (string, error) {
			return "https://ether.example/ether/me", nil
		},
	})
	do := func(method, path string) *http.Response {
		r := req(method, path, "")
		r.Header.Set("Authorization", "Bearer "+portal)
		return serve(h, r).Result()
	}
	res := do("GET", "/ether/api/me/apps")
	var listing struct {
		Apps []string `json:"apps"`
	}
	_ = json.NewDecoder(res.Body).Decode(&listing)
	res.Body.Close()
	wantApps := []string{"plex", "jellyfin", "emby", "silo", "stremio", "webdav", "m3u", "xtream"}
	if res.StatusCode != 200 || strings.Join(listing.Apps, ",") != strings.Join(wantApps, ",") {
		t.Fatalf("listing: %d %+v", res.StatusCode, listing.Apps)
	}
	for _, surface := range []string{"jellyfin", "emby", "silo"} {
		res = do("POST", "/ether/api/me/apps/"+surface)
		var created struct {
			Surface  string `json:"surface"`
			URL      string `json:"url"`
			Username string `json:"username"`
			Password string `json:"password"`
			QR       string `json:"qr"`
		}
		_ = json.NewDecoder(res.Body).Decode(&created)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s status: %d", surface, res.StatusCode)
		}
		if created.Surface != surface || created.URL != "https://ether.example/"+surface || created.Username != "plexviewer" || !core.IsGrantCredential(created.Password) || !strings.HasPrefix(created.QR, "data:image/png;base64,") {
			t.Fatalf("%s created: %+v", surface, created)
		}
		tok, err := grants.SignInWithGrant(ctx, surface, created.Username, created.Password, core.Device{ID: "infuse-tv", Client: "Infuse", Product: "Apple TV", Version: "8.0"})
		if err != nil || tok != created.Password {
			t.Fatalf("%s sign-in with grant: tok=%q err=%v", surface, tok, err)
		}
		viewer, dev, _, err := grants.ResolveGrantDevice(ctx, surface, tok, "infuse-tv")
		if err != nil || viewer.ID != 77 || dev.ID != "infuse-tv" || dev.Client != "Infuse" {
			t.Fatalf("%s resolve device: viewer=%+v dev=%+v err=%v", surface, viewer, dev, err)
		}
	}
	res = do("POST", "/ether/api/me/apps/xtream")
	var xt struct {
		URL      string `json:"url"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(res.Body).Decode(&xt); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	credential, short := frontend.XtreamGrantCredential(xt.Username, xt.Password)
	if res.StatusCode != 200 || !short || len(xt.Username) > 12 || len(xt.Password) != 12 || xt.URL != "https://ether.example/xtream" {
		t.Fatalf("invalid app login: status=%d", res.StatusCode)
	}
	viewer, err := grants.Resolve(ctx, "xtream", credential)
	if err != nil || viewer.ID != 77 {
		t.Fatal("unscoped login", err)
	}
	if _, err = grants.Resolve(ctx, "jellyfin", credential); err == nil {
		t.Fatal("cross-surface credential")
	}

	// Later reveal requires an owner credential and explicit CSRF admission.
	sum := sha256.Sum256([]byte(credential))
	body := `{"hash":"` + hex.EncodeToString(sum[:]) + `"}`
	reveal := func(token string, csrf bool) *http.Response {
		r := req("POST", "/ether/api/users/77/devices/xtream/reveal", body)
		r.Header.Set("Authorization", "Bearer "+token)
		if !csrf {
			r.Header.Del("X-Ether-Request")
		}
		return serve(h, r).Result()
	}
	res = reveal(portal, true)
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("viewer reveal admitted: %d", res.StatusCode)
	}
	local, _ := identity.NewLocal(db)
	ownerID, err := local.CreateOwner(ctx, "owner-reveal", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	if err := users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	ownerToken, err := grants.Issue(ctx, ownerID, nil, "owner", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	res = reveal(ownerToken, false)
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("reveal without CSRF: %d", res.StatusCode)
	}
	res = reveal(ownerToken, true)
	var revealed struct {
		Available bool   `json:"available"`
		Username  string `json:"username"`
		Password  string `json:"password"`
	}
	if err := json.NewDecoder(res.Body).Decode(&revealed); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || !revealed.Available || revealed.Username != xt.Username || revealed.Password != xt.Password || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("owner reveal failed", res.StatusCode)
	}
}

type pluginConnectFrontends struct {
	infos []admin.FrontendInfo
}

func (p pluginConnectFrontends) List(context.Context) ([]admin.FrontendInfo, error) {
	return p.infos, nil
}
func (pluginConnectFrontends) Settings(context.Context, frontend.Protocol) (frontend.Settings, error) {
	return nil, nil
}
func (pluginConnectFrontends) PutSettings(context.Context, frontend.Protocol, frontend.Settings) error {
	return nil
}
func (pluginConnectFrontends) Toggle(context.Context, frontend.Protocol, bool) error { return nil }

type stubPluginManager struct{}

func (stubPluginManager) CallStream(_ context.Context, _ string, _ plugins.Request) (plugins.Response, io.ReadCloser, error) {
	return plugins.Response{Status: 200, Headers: map[string]string{"Content-Type": "application/atom+xml"}}, io.NopCloser(strings.NewReader("<feed/>")), nil
}
func (stubPluginManager) Call(context.Context, string, plugins.Request) (plugins.Response, error) {
	return plugins.Response{Status: 200, Body: []byte("<feed/>")}, nil
}

func TestConnectAppsPluginFrontendAdapters(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	users, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := identity.NewLocal(db)
	ownerID, _ := local.CreateOwner(ctx, "owner", "password-1234")
	viewerID, _ := local.CreateViewer(ctx, "alice", "password-1234")
	if err := users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, users, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	portal, _ := grants.Issue(ctx, viewerID, nil, "viewer-portal", core.Device{})
	owner, _ := grants.Issue(ctx, ownerID, nil, "owner", core.Device{})

	tokenAdapter, err := frontendadapter.New(frontendadapter.Declaration{
		PluginID: "acme.reader",
		Meta: plugins.FrontendAdapterMeta{
			Key:        "opds",
			Label:      "Reader OPDS",
			PathPrefix: "/opds",
			Auth:       frontendadapter.AuthEtherToken,
		},
		Name: "Reader OPDS",
	}, stubPluginManager{})
	if err != nil {
		t.Fatal(err)
	}
	basicAdapter, err := frontendadapter.New(frontendadapter.Declaration{
		PluginID: "acme.reader",
		Meta: plugins.FrontendAdapterMeta{
			Key:        "basic",
			Label:      "Reader Basic",
			PathPrefix: "/opds-basic",
			Auth:       frontendadapter.AuthBasic,
		},
		Name: "Reader Basic",
	}, stubPluginManager{})
	if err != nil {
		t.Fatal(err)
	}
	lanAdapter, err := frontendadapter.New(frontendadapter.Declaration{
		PluginID: "acme.reader",
		Meta: plugins.FrontendAdapterMeta{
			Key:        "lan",
			Label:      "Reader LAN",
			PathPrefix: "/opds-lan",
			Auth:       frontendadapter.AuthNoneLAN,
		},
		Name: "Reader LAN",
	}, stubPluginManager{})
	if err != nil {
		t.Fatal(err)
	}

	frontends := pluginConnectFrontends{
		infos: []admin.FrontendInfo{
			{Descriptor: frontend.Descriptor{ID: "jellyfin"}, Enabled: true},
			{Descriptor: tokenAdapter.Describe(), Enabled: true},
			{Descriptor: basicAdapter.Describe(), Enabled: true},
			{Descriptor: lanAdapter.Describe(), Enabled: true},
			{Descriptor: frontend.Descriptor{ID: "plugin:acme.reader:disabled", Name: "Disabled", PathPrefix: "/disabled", Auth: "ether_token"}, Enabled: false},
		},
	}

	h := admin.NewHandler(admin.Deps{
		Config:         newStore(t, nil),
		IdentityGrants: grants,
		Frontends:      frontends,
		ActiveViewer:   users.ActiveUser,
		ViewerPortalURL: func(context.Context, *http.Request) (string, error) {
			return "https://ether.example/prefix/ether/me", nil
		},
	})
	do := func(method, path, token string) *http.Response {
		r := req(method, path, "")
		r.Header.Set("Authorization", "Bearer "+token)
		return serve(h, r).Result()
	}

	res := do("GET", "/ether/api/me/apps", portal)
	var listing struct {
		Apps     []string `json:"apps"`
		Adapters map[string]struct {
			Name string `json:"name"`
			Auth string `json:"auth"`
		} `json:"adapters"`
	}
	if err := json.NewDecoder(res.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	wantApps := []string{"jellyfin", "plugin:acme.reader:basic", "plugin:acme.reader:opds"}
	if res.StatusCode != 200 || strings.Join(listing.Apps, ",") != strings.Join(wantApps, ",") {
		t.Fatalf("listing = %d %+v, want %v", res.StatusCode, listing.Apps, wantApps)
	}
	if listing.Adapters["plugin:acme.reader:opds"].Name != "Reader OPDS" || listing.Adapters["plugin:acme.reader:opds"].Auth != "ether_token" {
		t.Fatalf("adapters = %+v", listing.Adapters)
	}

	// Mint token grant via URL-encoded surface on viewer portal.
	res = do("POST", "/ether/api/me/apps/plugin%3Aacme.reader%3Aopds", portal)
	var created struct {
		Surface  string `json:"surface"`
		URL      string `json:"url"`
		Username string `json:"username"`
		Password string `json:"password"`
		Token    string `json:"token"`
		QR       string `json:"qr"`
	}
	if err := json.NewDecoder(res.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || created.Surface != "plugin:acme.reader:opds" || created.URL != "https://ether.example/prefix/opds" || !core.IsGrantCredential(created.Token) || created.Username != "" || created.Password != "" || !strings.HasPrefix(created.QR, "data:image/png;base64,") {
		t.Fatalf("token create = %d %+v", res.StatusCode, created)
	}
	if _, err := grants.Resolve(ctx, "plugin:acme.reader:basic", created.Token); err == nil {
		t.Fatal("cross-adapter grant accepted")
	}

	// Verify the minted token authenticates directly against the mounted plugin frontend adapter.
	inst, err := tokenAdapter.Mount(ctx, frontend.Host{Identity: frontend.BindIdentity(grants, "plugin:acme.reader:opds")})
	if err != nil {
		t.Fatal(err)
	}
	adapterReq := httptest.NewRequest(http.MethodGet, "/opds/catalog", nil)
	adapterReq.Header.Set("Authorization", "Bearer "+created.Token)
	adapterRec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(adapterRec, adapterReq)
	if adapterRec.Code != 200 || adapterRec.Body.String() != "<feed/>" {
		t.Fatalf("adapter request = %d %q", adapterRec.Code, adapterRec.Body.String())
	}

	// Owner mints basic adapter grant for viewer.
	res = do("POST", "/ether/api/viewers/2/apps/plugin:acme.reader:basic", owner)
	var basicCreated struct {
		Surface  string `json:"surface"`
		URL      string `json:"url"`
		Username string `json:"username"`
		Password string `json:"password"`
		Token    string `json:"token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&basicCreated); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || basicCreated.URL != "https://ether.example/prefix/opds-basic" || basicCreated.Username != "alice" || !core.IsGrantCredential(basicCreated.Password) || basicCreated.Token != "" {
		t.Fatalf("basic create = %d %+v", res.StatusCode, basicCreated)
	}
	basicInst, err := basicAdapter.Mount(ctx, frontend.Host{Identity: frontend.BindIdentity(grants, "plugin:acme.reader:basic")})
	if err != nil {
		t.Fatal(err)
	}
	basicReq := httptest.NewRequest(http.MethodGet, "/opds-basic/catalog", nil)
	basicReq.SetBasicAuth(basicCreated.Username, basicCreated.Password)
	basicRec := httptest.NewRecorder()
	basicInst.Handler().ServeHTTP(basicRec, basicReq)
	if basicRec.Code != 200 || basicRec.Body.String() != "<feed/>" {
		t.Fatalf("basic adapter request = %d %q", basicRec.Code, basicRec.Body.String())
	}

	// none_lan and disabled adapters are rejected.
	for _, bad := range []string{"plugin:acme.reader:lan", "plugin:acme.reader:disabled"} {
		res = do("POST", "/ether/api/me/apps/"+bad, portal)
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", bad, res.StatusCode)
		}
	}
}

func TestConnectAppsUsesPublishedAddresses(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	users, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := identity.NewLocal(db)
	_, _ = local.CreateOwner(ctx, "owner", "password-1234")
	viewerID, _ := local.CreateViewer(ctx, "alice", "password-1234")
	if err := users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	cfgStore := newStore(t, func(c *config.Config) {
		c.Frontends = map[string]config.Frontend{
			"jellyfin": {Enabled: true, PublishedAddresses: []string{"https://jellyfin.example.com/"}},
			"emby":     {Enabled: true, PublishedAddresses: []string{"https://media.example.com/emby/"}},
			"stremio":  {Enabled: true, PublishedAddresses: []string{"https://stremio.example.com/media/"}},
			"webdav":   {Enabled: true, PublishedAddresses: []string{"https://files.example.com/dav"}},
			"xtream":   {Enabled: true, PublishedAddresses: []string{"https://tv.example.com"}},
		}
	})
	grants, err := identity.NewGrants(db, users, time.Hour, nil, cfgStore)
	if err != nil {
		t.Fatal(err)
	}
	portal, _ := grants.Issue(ctx, viewerID, nil, "viewer-portal", core.Device{})

	h := admin.NewHandler(admin.Deps{
		Config:           cfgStore,
		Frontends:        allConnectFrontends{},
		IdentityGrants:   grants,
		ViewerPortalURL:  func(context.Context, *http.Request) (string, error) { return "http://192.168.1.5:32400/ether/me", nil },
		StremioPortalURL: nil, // Stremio has its own HTTPS published address so StremioPortalURL is not required.
	})

	post := func(surface string) map[string]string {
		r := req(http.MethodPost, "/ether/api/me/apps/"+surface, "")
		r.Header.Set("Authorization", "Bearer "+portal)
		rec := serve(h, r)
		if rec.Code != 200 {
			t.Fatalf("%s status = %d body = %s", surface, rec.Code, rec.Body.String())
		}
		var out map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	if got := post("jellyfin"); got["url"] != "https://jellyfin.example.com" {
		t.Fatalf("jellyfin url = %q", got["url"])
	}
	if got := post("emby"); got["url"] != "https://media.example.com/emby" {
		t.Fatalf("emby url = %q", got["url"])
	}
	if got := post("stremio"); !strings.HasPrefix(got["url"], "https://stremio.example.com/media/stremio/") || !strings.HasSuffix(got["url"], "/manifest.json") {
		t.Fatal("stremio install URL lacks the expected host, mounted prefix, or manifest path")
	}
	if got := post("webdav"); got["url"] != "https://files.example.com/dav/" {
		t.Fatalf("webdav url = %q", got["url"])
	}
	if got := post("xtream"); got["url"] != "https://tv.example.com" {
		t.Fatalf("xtream url = %q", got["url"])
	}
	// M3U has no published address so it falls back to ViewerPortalURL.
	if got := post("m3u"); !strings.HasPrefix(got["url"], "http://192.168.1.5:32400/m3u/") || !strings.HasSuffix(got["url"], "/playlist.m3u") {
		t.Fatalf("m3u fallback url = %q", got["url"])
	}
}

func TestConnectAppsAndWebPlayerHonorLegacyPublicURLOverPlexCustomURL(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	users, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := identity.NewLocal(db)
	_, _ = local.CreateOwner(ctx, "owner", "password-1234")
	viewerID, _ := local.CreateViewer(ctx, "alice", "password-1234")
	if err := users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	// Esmail's exact setup: plex.customUrls points at dashboard.<domain>,
	// while jellyfin and emby have their own per-app publicUrl entries.
	cfgStore := newStore(t, func(c *config.Config) {
		c.Plex.CustomURLs = []string{"https://dashboard.example.com/"}
		c.Frontends = map[string]config.Frontend{
			"jellyfin": {Enabled: true, PublicURL: "https://jellyfin.example.com/"},
			"emby":     {Enabled: true, PublicURL: "https://emby.example.com/"},
			"silo":     {Enabled: true},
		}
	})
	grants, err := identity.NewGrants(db, users, time.Hour, nil, cfgStore)
	if err != nil {
		t.Fatal(err)
	}
	portal, _ := grants.Issue(ctx, viewerID, nil, "viewer-portal", core.Device{})

	h := admin.NewHandler(admin.Deps{
		Config:         cfgStore,
		Frontends:      allConnectFrontends{},
		IdentityGrants: grants,
		ViewerPortalURL: func(context.Context, *http.Request) (string, error) {
			return "https://dashboard.example.com/ether/me", nil
		},
	})

	post := func(surface string) map[string]string {
		r := req(http.MethodPost, "/ether/api/me/apps/"+surface, "")
		r.Host = "dashboard.example.com"
		r.Header.Set("Authorization", "Bearer "+portal)
		rec := serve(h, r)
		if rec.Code != 200 {
			t.Fatalf("%s status = %d body = %s", surface, rec.Code, rec.Body.String())
		}
		var out map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	jf := post("jellyfin")
	if jf["url"] != "https://jellyfin.example.com" {
		t.Fatalf("jellyfin portal url = %q, want https://jellyfin.example.com", jf["url"])
	}
	if !strings.HasPrefix(jf["qr"], "data:image/png;base64,") {
		t.Fatalf("jellyfin qr missing: %q", jf["qr"])
	}
	if emby := post("emby"); emby["url"] != "https://emby.example.com" {
		t.Fatalf("emby portal url = %q, want https://emby.example.com", emby["url"])
	}
	// Silo has no published address -> falls back to dashboard.example.com/silo.
	if silo := post("silo"); silo["url"] != "https://dashboard.example.com/silo" {
		t.Fatalf("silo portal url = %q, want https://dashboard.example.com/silo", silo["url"])
	}

	// Web player launch also honours jellyfin's published address instead of dashboard/jellyfin.
	wpReq := req(http.MethodGet, "/ether/api/web-player", "")
	wpReq.Host = "dashboard.example.com"
	wpReq.Header.Set("Authorization", "Bearer "+portal)
	wpRec := serve(h, wpReq)
	if wpRec.Code != 200 {
		t.Fatalf("web-player status = %d body = %s", wpRec.Code, wpRec.Body.String())
	}
	if !strings.Contains(wpRec.Body.String(), `"https://jellyfin.example.com/web/ether-signin"`) {
		t.Fatalf("web-player body = %s, want jellyfin.example.com/web/ether-signin", wpRec.Body.String())
	}
}
