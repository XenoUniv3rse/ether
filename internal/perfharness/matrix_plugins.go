package perfharness

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

func MatrixPluginHost() Client {
	const badge = `{"entries":[{"title":{"tmdb":106,"type":"movie"},"labels":["Fixture"]}]}`
	const blob = "matrix fixture blob"
	library := url.PathEscape(core.PluginLibraryKey("community.example.matrix-remote", "music"))
	return Client{Name: "remote plugin", Surface: "plugin-host", Prefix: "/ether/plugin-host", Headers: func(s *session) http.Header {
		h := make(http.Header)
		h.Set("Authorization", "Bearer "+s.Viewer.PluginHost)
		return h
	}, Steps: []Step{
		{Name: "settings", Class: classStatic, Path: "/settings"},
		{Name: "limits", Class: classStatic, Path: "/limits"},
		{Name: "catalog search", Class: classSearch, Path: "/catalog/search?q=star&limit=30"},
		{Name: "catalog title", Class: classDetail, Path: "/catalog/tmdb:106"},
		{Name: "viewer grants", Class: classStatic, Path: "/viewers/granted"},
		{Name: "viewer history", Class: classBrowse, Path: "/viewers/{viewerId}/history?limit=48"},
		{Name: "KV write", Class: classState, Method: http.MethodPut, Path: "/kv/fixture", Body: `{"value":"fixture"}`},
		{Name: "KV read", Class: classStatic, Path: "/kv/fixture"},
		{Name: "KV list", Class: classBrowse, Path: "/kv?limit=20"},
		{Name: "KV usage", Class: classStatic, Path: "/kv-usage"},
		{Name: "KV delete", Class: classState, Method: http.MethodDelete, Path: "/kv/fixture"},
		{Name: "badge usage", Class: classStatic, Path: "/badges/usage"},
		{Name: "badge usage alias", Class: classStatic, Path: "/badges-usage"},
		{Name: "badge put", Class: classState, Method: http.MethodPut, Path: "/badges/fixture", Body: badge},
		{Name: "badge post", Class: classState, Method: http.MethodPost, Path: "/badges/fixture", Body: badge},
		{Name: "badge clear", Class: classState, Method: http.MethodPost, Path: "/badges/fixture/clear"},
		{Name: "badge set", Class: classState, Method: http.MethodPost, Path: "/badges/fixture/set", Body: badge},
		{Name: "badge delete", Class: classState, Method: http.MethodDelete, Path: "/badges/fixture"},
		{Name: "secret set", Class: classState, Method: http.MethodPut, Path: "/secrets/fixture_secret", Body: `{"value":"synthetic","ttlSec":60}`},
		{Name: "secret delete", Class: classState, Method: http.MethodDelete, Path: "/secrets/fixture_secret"},
		{Name: "kind upsert", Class: classState, Method: http.MethodPost, Path: "/kinds/music/upsert", Body: `{"items":[{"id":"fixture-track","level":"track","title":"Fixture track","playable":true}]}`},
		{Name: "kind items", Class: classBrowse, Path: "/kinds/music/items?limit=24"},
		{Name: "mark fixture track watched", Class: classState, Method: http.MethodPost, Path: "/viewers/watch-state", Body: `{"viewerId":{viewerId},"titleId":"pk:community.example.matrix-remote:music:fixture-track","watched":true}`},
		{Name: "clear fixture track watched", Class: classState, Method: http.MethodPost, Path: "/viewers/watch-state", Body: `{"viewerId":{viewerId},"titleId":"pk:community.example.matrix-remote:music:fixture-track","watched":false}`},
		{Name: "kind delete", Class: classState, Method: http.MethodPost, Path: "/kinds/music/delete", Body: `{"ids":["fixture-track"]}`},
		{Name: "cache put", Class: classState, Method: http.MethodPut, Path: "/cache/" + library + "/matrix.txt", Body: blob, Discover: discoverMatrixPluginBlob},
		{Name: "cache read", Class: classDetail, Path: "/cache/{pluginBlob}", Accept: func(body []byte) bool { return string(body) == blob }},
		{Name: "cache post", Class: classState, Method: http.MethodPost, Path: "/cache/" + library + "?name=matrix.txt", Body: blob, Discover: discoverMatrixPluginBlob},
		{Name: "cache stat", Class: classState, Method: http.MethodPost, Path: "/data/cache.stat", Body: `["{pluginBlobRef}"]`},
		{Name: "cache delete", Class: classState, Method: http.MethodDelete, Path: "/cache/{pluginBlob}"},
		{Name: "publish fixture event", Class: classState, Method: http.MethodPost, Path: "/events/publish", Body: `{"key":"matrix.finished","payload":{}}`},
		{Name: "fixture owner notice", Class: classState, Method: http.MethodPost, Path: "/notify", Body: `{"text":"Matrix plugin fixture finished"}`},
		{Name: "fixture collection upsert", Class: classState, Method: http.MethodPost, Path: "/collections/upsert", Body: `{"id":"perf-matrix","name":"Matrix plugin fixture","mediaType":"movie","items":["tmdb:106"]}`},
		{Name: "log entry", Class: classState, Method: http.MethodPost, Path: "/log", Body: `{"level":"info","message":"Matrix fixture"}`},
	}}
}

func discoverMatrixPluginBlob(vars map[string]string, body []byte) {
	var result struct {
		Blob string `json:"blob"`
	}
	if json.Unmarshal(body, &result) == nil && result.Blob != "" {
		vars["pluginBlobRef"] = result.Blob
		segments := strings.Split(result.Blob, "/")
		for i, segment := range segments {
			segments[i] = url.PathEscape(segment)
		}
		vars["pluginBlob"] = strings.Join(segments, "/")
	}
}

func MatrixSubsonic(methods []string) Client {
	client := Client{Name: "subsonic", Surface: "subsonic", Prefix: "/subsonic/rest", Headers: func(*session) http.Header {
		h := make(http.Header)
		h.Set("Accept", "application/json")
		return h
	}}
	params := map[string]string{
		"getUser":   "&username={subsonicUser}",
		"getArtist": "&id=ar-1000", "getArtistInfo": "&id=ar-1000", "getArtistInfo2": "&id=ar-1000",
		"getAlbum": "&id=al-10000", "getAlbumInfo": "&id=al-10000", "getAlbumInfo2": "&id=al-10000",
		"getMusicDirectory": "&id=al-10000", "getSong": "&id=tr-100000",
		"getAlbumList": "&type=newest&size=48", "getAlbumList2": "&type=alphabeticalByName&size=48",
		"getSimilarSongs": "&id=ar-1000&count=25", "getSimilarSongs2": "&id=ar-1000&count=25",
		"getRandomSongs": "&size=48", "getSongsByGenre": "&genre=Rock&count=48",
		"getTopSongs": "&artist=Fixture+artist+1000&count=24",
		"search":      "&query=fixture", "search2": "&query=fixture", "search3": "&query=fixture",
		"getLyrics":         "&artist=Fixture+artist+1000&title=Fixture+song+100000",
		"getLyricsBySongId": "&id=tr-100000", "getCoverArt": "&id=al-10000",
		"stream": "&id=tr-100000", "download": "&id=tr-100000",
		"star": "&id=tr-100000", "unstar": "&id=tr-100000",
		"scrobble": "&id=tr-100000&submission=true", "setRating": "&id=tr-100000&rating=4",
		"createPlaylist": "&name=Fixture&songId=tr-100000",
		"getPlaylist":    "&id={subsonicPlaylist}", "updatePlaylist": "&playlistId={subsonicPlaylist}&name=Fixture+updated",
		"deletePlaylist": "&id={subsonicPlaylist}",
	}
	mutates := map[string]bool{"createPlaylist": true, "updatePlaylist": true, "deletePlaylist": true, "star": true, "unstar": true, "scrobble": true, "setRating": true}
	playlist := []string{"createPlaylist", "getPlaylist", "updatePlaylist", "deletePlaylist"}
	order := slices.DeleteFunc(slices.Clone(methods), func(name string) bool { return slices.Contains(playlist, name) })
	for _, name := range playlist {
		if slices.Contains(methods, name) {
			order = append(order, name)
		}
	}
	for _, name := range order {
		// These are inventoried and explicitly exempted by the app fixture:
		// a catalogue row does not supply audio bytes or a resolver provider.
		if name == "stream" || name == "download" {
			continue
		}
		query := "?u={subsonicUser}&p={subsonicPass}&v=1.16.1&c=etherperf&f=json"
		step := Step{Name: name, Class: classBrowse, Path: "/" + name + ".view" + query + params[name], Accept: subsonicOK}
		if name == "getCoverArt" {
			step.Accept = func(body []byte) bool { return len(body) >= 3 && body[0] == 0xff && body[1] == 0xd8 && body[2] == 0xff }
		}
		if mutates[name] {
			step.Method, step.Class = http.MethodPost, classState
		}
		if name == "createPlaylist" {
			step.Discover = discoverSubsonicPlaylist
		}
		client.Steps = append(client.Steps, step)
	}
	return client
}

type subsonicResult struct {
	Response struct {
		Status   string `json:"status"`
		Playlist struct {
			ID string `json:"id"`
		} `json:"playlist"`
	} `json:"subsonic-response"`
}

func subsonicOK(body []byte) bool {
	var out subsonicResult
	return json.Unmarshal(body, &out) == nil && out.Response.Status == "ok"
}

func discoverSubsonicPlaylist(vars map[string]string, body []byte) {
	var out subsonicResult
	if json.Unmarshal(body, &out) == nil && out.Response.Status == "ok" {
		vars["subsonicPlaylist"] = url.QueryEscape(out.Response.Playlist.ID)
	}
}
