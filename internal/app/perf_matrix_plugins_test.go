package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/perfharness"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/frontendadapter"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
)

const perfRemotePlugin = "community.example.matrix-remote"

func seedPerfRemotePlugin(ctx context.Context, tx *sql.Tx, now int64, exerciseHost bool) error {
	const hostManifest = `{
		"id":"community.example.matrix-remote","name":"Performance fixture","version":"1.0.0",
		"ether":{"protocol":1,"kind":"content","runtime":{"kind":"remote"},
			"capabilities":["catalog_provider","poster_badges","events"],
			"permissions":{"catalog":"read","collections":"write","secrets":["fixture_secret"],"viewers":{"identity":"granted_viewers","watch_history":"granted_viewers","write_watch_state":"granted_viewers"}},
			"contributes":{
				"posterBadges":[{"key":"fixture","label":"Fixture badges"}],
				"events":{"publish":[{"key":"matrix.finished","label":"Fixture event","version":1}]},
				"mediaKinds":[{"key":"music","label":"Fixture remote kind","media":"music","library":{"name":"Fixture remote kind"},
					"levels":[{"key":"track","label":"Track","role":"track","playable":true}],"sync":{"mode":"push"}}]
			}
		}
	}`
	kind := "tool"
	manifest := `{"id":"community.example.matrix-remote","name":"Performance fixture","version":"1.0.0","ether":{"protocol":1,"kind":"tool","runtime":{"kind":"remote"},"permissions":{"catalog":"read","viewers":{"identity":"granted_viewers","watch_history":"granted_viewers"}}}}`
	if exerciseHost {
		kind, manifest = "content", hostManifest
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO plugins_installed(id,name,version,kind,manifest,source_kind,installed_at,updated_at)
		VALUES(?, 'Performance fixture', '1.0.0', ?, ?, 'studio', ?,?)`, perfRemotePlugin, kind, manifest, now, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO plugins_grants(plugin,grants)
		SELECT ?, '{"viewers":[' || id || ']}' FROM auth_users WHERE is_owner=1`, perfRemotePlugin)
	return err
}

func perfMatrixPluginCredential(ctx context.Context, a *App, owner int64) (string, error) {
	grants, err := identity.NewGrants(a.db, a.users, time.Hour, nil)
	if err != nil {
		return "", err
	}
	return grants.IssueScoped(ctx, owner, "plugin", core.Device{ID: perfRemotePlugin, Client: "etherperf"}, []string{"full"})
}

const matrixAlbumCoverURL = "https://images.example.invalid/matrix/album.jpg"

type perfMusicFixture struct {
	manifest []byte
	files    map[string][]byte
	methods  []string
}

var subsonicCase = regexp.MustCompile(`(?m)^      case "([a-zA-Z0-9]+)":`)

func readPerfMusicFixture(dir string) (perfMusicFixture, error) {
	var fixture perfMusicFixture
	root := os.DirFS(dir)
	manifest, err := fs.ReadFile(root, "manifest.json")
	if err != nil {
		return fixture, err
	}
	fixture.files = map[string][]byte{}
	for _, subdir := range []string{"src", "assets"} {
		err = fs.WalkDir(root, subdir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || strings.HasSuffix(path, ".test.ts") {
				return err
			}
			b, err := fs.ReadFile(root, path)
			if err == nil {
				fixture.files[path] = b
			}
			return err
		})
		if err != nil {
			return fixture, err
		}
	}
	if fixture.files["icon.svg"], err = fs.ReadFile(root, "icon.svg"); err != nil {
		return fixture, err
	}
	_, dispatch, ok := strings.Cut(string(fixture.files["src/subsonic.ts"]), "private dispatch(")
	dispatch, _, end := strings.Cut(dispatch, "\n  private ")
	if !ok || !end {
		return fixture, fmt.Errorf("Subsonic dispatch inventory unavailable")
	}
	for _, m := range subsonicCase.FindAllStringSubmatch(dispatch, -1) {
		fixture.methods = append(fixture.methods, m[1])
	}
	slices.Sort(fixture.methods)
	fixture.methods = slices.Compact(fixture.methods)
	if len(fixture.methods) == 0 {
		return fixture, fmt.Errorf("Subsonic dispatch inventory is empty")
	}
	var parsed plugins.Manifest
	if err := json.Unmarshal(manifest, &parsed); err != nil {
		return fixture, err
	}
	if parsed.Ether == nil || parsed.Ether.Runtime.Entry == "" {
		return fixture, fmt.Errorf("Music fixture has no hosted entry")
	}
	files := make(map[string]string)
	for path, data := range fixture.files {
		if strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".js") {
			files[path] = string(data)
		}
	}
	js, _, diagnostics := wasm.Compile(files, parsed.Ether.Runtime.Entry)
	if len(js) == 0 {
		return fixture, fmt.Errorf("compile Music fixture: %v", diagnostics)
	}
	// The fixture already contains the Music library. A push declaration
	// keeps installation from starting a pull sync against real providers.
	for i := range parsed.Ether.Contributes.MediaKinds {
		kind := &parsed.Ether.Contributes.MediaKinds[i]
		kind.Sync = plugins.KindSyncDecl{Mode: plugins.KindSyncPush}
		kind.Fields = append(kind.Fields, plugins.KindField{Key: "contentRating", Type: "string", Label: "Content rating"})
	}
	const bundledEntry = "dist/perf-fixture.js"
	parsed.Ether.Runtime.Entry = bundledEntry
	fixture.files[bundledEntry] = js
	fixture.manifest, err = json.Marshal(parsed)
	return fixture, err
}

func (fixture perfMusicFixture) install(ctx context.Context, a *App, viewer *perfharness.Viewer) error {
	mgr := a.pluginsRT.mgr
	preview, err := mgr.PreviewInstall(ctx, plugins.Source{Kind: "studio", Manifest: fixture.manifest, Files: fixture.files})
	if err != nil {
		return err
	}
	rec, err := mgr.Install(ctx, preview, plugins.InstallOptions{AcceptAddedPermissions: true, Grants: plugins.Grants{Revoked: []string{"network"}}})
	if err != nil {
		return err
	}
	id := frontendadapter.AdapterID(rec.ID, "subsonic")
	a.syncAdapters(ctx)
	if err := a.frontends.control.Toggle(ctx, id, true); err != nil {
		return err
	}
	var items []core.KindItem
	fields := map[string]json.RawMessage{"contentRating": json.RawMessage(`"G"`)}
	for artist := range 40 {
		artistID := fmt.Sprintf("ar-%d", 1000+artist)
		items = append(items, core.KindItem{ID: artistID, Level: "artist", Title: "Fixture artist " + artistID, Fields: fields})
		for album := range 5 {
			albumID := fmt.Sprintf("al-%d", 10_000+artist*5+album)
			items = append(items, core.KindItem{ID: albumID, ParentID: artistID, Level: "album", Title: "Fixture album " + albumID, Art: []core.KindArtRef{{Role: "poster", URL: matrixAlbumCoverURL}}, Fields: fields})
			for track := range 12 {
				trackID := fmt.Sprintf("tr-%d", 100_000+(artist*5+album)*12+track)
				items = append(items, core.KindItem{ID: trackID, ParentID: albumID, Level: "track", Title: "Fixture song " + trackID, Index: track + 1, Playable: true, Fields: fields})
			}
		}
	}
	if _, err := a.pluginsRT.pluginitems.UpsertItems(ctx, rec.ID, "music", items); err != nil {
		return err
	}
	if viewer.Restricted {
		kind, err := a.pluginsRT.pluginitems.GetKind(ctx, rec.ID, "music")
		if err != nil {
			return err
		}
		u, err := a.users.ActiveUser(ctx, viewer.ID)
		if err != nil {
			return err
		}
		u.AllowedLibraries = append(u.AllowedLibraries, kind.LibraryID)
		if err := a.users.UpdateUser(ctx, *u); err != nil {
			return err
		}
	}
	grants, err := identity.NewGrants(a.db, a.users, time.Hour, nil, a.cfg)
	if err != nil {
		return err
	}
	viewer.SubsonicUser, viewer.SubsonicPass, _, err = grants.IssueAppLogin(ctx, viewer.ID, string(id), core.Device{ID: "matrix-subsonic", Client: "etherperf"})
	return err
}

func installPerfMatrixVelaCatalogue(ctx context.Context, a *App, viewer perfharness.Viewer, artwork []byte) error {
	const manifest = `{
		"id":"community.example.matrixcatalogue","name":"Matrix Catalogue","version":"1.0.0",
		"ether":{
			"protocol":1,"kind":"content","summary":"Synthetic Vela catalogue",
			"runtime":{"kind":"hosted","entry":"plugin.js"},
			"capabilities":["catalog_provider","kind_resolver","screens"],
			"contributes":{"screens":[{"id":"matrix-fixture","title":"Matrix preview","pages":{"index":"screens/index.json"}}],"mediaKinds":[{
				"key":"music","label":"Fixture Music","media":"music",
				"library":{"name":"Matrix Catalogue","tabs":[{"key":"fixture-live","label":"Live","kind":"live"}]},
				"levels":[{"key":"track","label":"Track","role":"track","playable":true}],
				"fields":[{"key":"genre","label":"Genre","type":"string","indexed":true},
				          {"key":"contentRating","label":"Content rating","type":"string"}],
				"browse":{"sorts":["title"],"facets":["genre"],"search":true},
				"sync":{"mode":"push"},"related":{"continuation":true},"lyrics":true
			}]}
		}
	}`
	const document = `{"schema":1,"kind":"screen","renderer":19,"sources":{"viewer":{"from":"viewer"}},"tree":{"type":"image","src":"asset:assets/cover.jpg"}}`
	const script = `function handle(req) {
		if (req.path === "/ether/v1/kinds/music/live") {
			return {status:200, body:{items:[{id:"fixture-radio",title:"Matrix fixture radio",live:true}]}};
		}
		if (req.path === "/ether/v1/kinds/music/lyrics") {
			return {status:200, body:{plainLyrics:"Synthetic words for the matrix fixture"}};
		}
		if (req.path === "/ether/v1/kinds/music/related") {
			return {status:200, body:{items:[{id:"track-2",level:"track",title:"Fixture Track 2",playable:true,fields:{genre:"Folk",contentRating:"G"}}]}};
		}
		return {status:200, body:{status:"ok"}};
	}`
	mgr := a.pluginsRT.mgr
	preview, err := mgr.PreviewInstall(ctx, plugins.Source{Kind: "studio", Manifest: []byte(manifest), Files: map[string][]byte{
		"plugin.js": []byte(script), "screens/index.json": []byte(document), "assets/cover.jpg": artwork,
	}})
	if err != nil {
		return err
	}
	rec, err := mgr.Install(ctx, preview, plugins.InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		return err
	}
	items := []core.KindItem{
		{ID: "track-1", Level: "track", Title: "Fixture soundtrack 1", Playable: true, Released: 1577836800, Recording: "isrc:USAAA2600001", Fields: map[string]json.RawMessage{"genre": json.RawMessage(`"Drama"`), "contentRating": json.RawMessage(`"G"`)}},
		{ID: "track-2", Level: "track", Title: "Fixture soundtrack 2", Playable: true, Released: 1577836800, Recording: "isrc:USAAA2600001", Fields: map[string]json.RawMessage{"genre": json.RawMessage(`"Drama"`), "contentRating": json.RawMessage(`"G"`)}},
	}
	if _, err := a.pluginsRT.pluginitems.UpsertItems(ctx, rec.ID, "music", items); err != nil {
		return err
	}
	if !viewer.Restricted {
		return nil
	}
	kind, err := a.pluginsRT.pluginitems.GetKind(ctx, rec.ID, "music")
	if err != nil {
		return err
	}
	u, err := a.users.ActiveUser(ctx, viewer.ID)
	if err != nil {
		return err
	}
	u.AllowedLibraries = append(u.AllowedLibraries, kind.LibraryID)
	return a.users.UpdateUser(ctx, *u)
}

func installPerfMatrixSports(ctx context.Context, a *App) error {
	const manifest = `{
		"id":"community.example.matrixsports","name":"Fixture Sports","version":"1.0.0",
		"ether":{
			"protocol":1,"kind":"extension","summary":"Synthetic live television events",
			"runtime":{"kind":"hosted","entry":"plugin.js"},
			"capabilities":["live_events","screens"],
			"contributes":{
				"liveFeeds":[{"id":"sports","title":"Fixture Sports","minIntervalSec":15}],
				"surfaces":[{"id":"scorebug","presentation":"playerOverlay","title":"Fixture score","anchor":"topLeft",
					"setting":{"id":"scorebug","label":"Fixture score","default":true},
					"when":{"event":{"feed":"sports"}},"document":"scorebug.json"}]
			}
		}
	}`
	const overlay = `{"schema":1,"kind":"screen","renderer":19,"sources":{"game":{"from":"liveEvent","feed":"sports","id":"{context.event}"}},"tree":{"type":"box"}}`
	const script = `function handle(req) {
		if (req.path === "/ether/health") return {status:200, body:{status:"ok"}};
		if (!req.path || !req.path.startsWith("/ether/v1/live/")) return {status:200, body:{}};
		const events = Array.from({length: 50}, (_, i) => ({
			id: "game-" + i, feed: "sports", category: "sports",
			league: {id: "fixture", name: "Fixture league"},
			title: "Fixture game " + i, start: new Date(Date.now() - 3600000).toISOString(),
			status: {state: "in", detail: "Second period", period: 2},
			competitors: [{id: "home-" + i, name: "Home " + i}, {id: "away-" + i, name: "Away " + i}]
		}));
		if (req.path && req.path.indexOf("/events/game-") >= 0) {
			const id = req.path.split("/").pop();
			return {status:200, body:events.find(event => event.id === id) || events[0]};
		}
		return {status:200, body:{events:events, nextPollSec:15}};
	}`
	mgr := a.pluginsRT.mgr
	preview, err := mgr.PreviewInstall(ctx, plugins.Source{
		Kind: "studio", Manifest: []byte(manifest),
		Files: map[string][]byte{"plugin.js": []byte(script), "scorebug.json": []byte(overlay)},
	})
	if err != nil {
		return err
	}
	if _, err := mgr.Install(ctx, preview, plugins.InstallOptions{AcceptAddedPermissions: true}); err != nil {
		return err
	}
	a.liveEvents.SyncFeeds(ctx)
	return nil
}

func installPerfMatrixMediaBrowserMusic(ctx context.Context, a *App, viewer perfharness.Viewer) error {
	const manifest = `{
		"id":"community.example.matrixalbums","name":"Matrix albums","version":"1.0.0",
		"ether":{"protocol":1,"kind":"content","summary":"Synthetic artists and lyrics","runtime":{"kind":"hosted","entry":"plugin.js"},
		"capabilities":["catalog_provider","kind_resolver"],"contributes":{"mediaKinds":[{
			"key":"music","label":"Fixture albums","media":"music","library":{"name":"Matrix Albums"},
			"levels":[{"key":"artist","label":"Artist","role":"artist","container":true},
				{"key":"track","label":"Track","parent":"artist","role":"track","playable":true}],
			"present":{"mediaBrowser":{"collectionType":"music","itemTypes":{"artist":"MusicArtist","track":"Audio"}}},
			"fields":[{"key":"contentRating","label":"Content rating","type":"string"},{"key":"genre","label":"Genre","type":"string"}],
			"browse":{"sorts":["title"],"search":true},"sync":{"mode":"push"},"lyrics":true
		}]}}
	}`
	const script = `function handle(req) {
		if (req.path === "/ether/v1/kinds/music/lyrics") {
			return {status:200, body:{syncedLyrics:"[00:01.00]Synthetic words for the matrix fixture"}};
		}
		return {status:200, body:{status:"ok"}};
	}`
	mgr := a.pluginsRT.mgr
	preview, err := mgr.PreviewInstall(ctx, plugins.Source{Kind: "studio", Manifest: []byte(manifest), Files: map[string][]byte{"plugin.js": []byte(script)}})
	if err != nil {
		return err
	}
	rec, err := mgr.Install(ctx, preview, plugins.InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		return err
	}
	fields := map[string]json.RawMessage{"contentRating": json.RawMessage(`"G"`), "genre": json.RawMessage(`"Drama"`)}
	items := []core.KindItem{
		{ID: "artist", Level: "artist", Title: "Matrix artist", Released: 1577836800, Fields: fields},
		{ID: "song", Level: "track", ParentID: "artist", Title: "Matrix song", Playable: true, Released: 1577836800, Fields: fields},
	}
	if _, err := a.pluginsRT.pluginitems.UpsertItems(ctx, rec.ID, "music", items); err != nil {
		return err
	}
	if !viewer.Restricted {
		return nil
	}
	kind, err := a.pluginsRT.pluginitems.GetKind(ctx, rec.ID, "music")
	if err != nil {
		return err
	}
	u, err := a.users.ActiveUser(ctx, viewer.ID)
	if err != nil {
		return err
	}
	u.AllowedLibraries = append(u.AllowedLibraries, kind.LibraryID)
	return a.users.UpdateUser(ctx, *u)
}
