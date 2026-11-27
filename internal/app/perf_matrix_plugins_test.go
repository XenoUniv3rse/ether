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

func seedPerfRemotePlugin(ctx context.Context, tx *sql.Tx, now int64) error {
	const manifest = `{"id":"community.example.matrix-remote","name":"Performance fixture","version":"1.0.0","ether":{"protocol":1,"kind":"tool","runtime":{"kind":"remote"},"permissions":{"catalog":"read","viewers":{"identity":"granted_viewers","watch_history":"granted_viewers"}}}}`
	_, err := tx.ExecContext(ctx, `INSERT INTO plugins_installed(id,name,version,kind,manifest,source_kind,installed_at,updated_at)
		VALUES(?, 'Performance fixture', '1.0.0', 'tool', ?, 'studio', ?,?)`, perfRemotePlugin, manifest, now, now)
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
	for artist := range 40 {
		artistID := fmt.Sprintf("ar-%d", 1000+artist)
		items = append(items, core.KindItem{ID: artistID, Level: "artist", Title: "Fixture artist " + artistID})
		for album := range 5 {
			albumID := fmt.Sprintf("al-%d", 10_000+artist*5+album)
			items = append(items, core.KindItem{ID: albumID, ParentID: artistID, Level: "album", Title: "Fixture album " + albumID})
			for track := range 12 {
				trackID := fmt.Sprintf("tr-%d", 100_000+(artist*5+album)*12+track)
				items = append(items, core.KindItem{ID: trackID, ParentID: albumID, Level: "track", Title: "Fixture song " + trackID, Index: track + 1, Playable: true})
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
					"when":{"event":{"feed":"sports"}},"document":"scorebug.json"}]
			}
		}
	}`
	const overlay = `{"schema":1,"kind":"screen","renderer":19,"sources":{"game":{"from":"liveEvent","feed":"sports","id":"{context.event}"}},"tree":{"type":"box"}}`
	const script = `function handle(req) {
		if (req.op && req.op !== "http") return {status:200, body:{}};
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
