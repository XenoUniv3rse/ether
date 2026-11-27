package perfharness

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
)

func MatrixPluginHost() Client {
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
	}}
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
		query := "?u={subsonicUser}&p={subsonicPass}&v=1.16.1&c=etherperf&f=json"
		step := Step{Name: name, Class: classBrowse, Path: "/" + name + ".view" + query + params[name], Accept: subsonicOK}
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
