package perfharness

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

func addMatrixPlexMutations(c *Client) {
	movieURI := url.QueryEscape("server://") + "{machineId}" + url.QueryEscape("/com.plexapp.plugins.library/library/metadata/") + "{movieKey}"
	episodeURI := url.QueryEscape("server://") + "{machineId}" + url.QueryEscape("/com.plexapp.plugins.library/library/metadata/") + "{episodeKey}"
	c.Steps = append(c.Steps,
		Step{Name: "scratch queue create", Class: classState, Method: http.MethodPost, Path: "/playQueues?type=video&own=1&uri=" + movieURI, Discover: discoverPlayQueue},
		Step{Name: "play queue HEAD", Class: classDetail, Method: http.MethodHead, Path: "/playQueues/{playQueueID}?own=1"},
		Step{Name: "scratch queue shuffle", Class: classState, Method: http.MethodPut, Path: "/playQueues/{playQueueID}/shuffle"},
		Step{Name: "scratch queue unshuffle", Class: classState, Method: http.MethodPut, Path: "/playQueues/{playQueueID}/unshuffle"},
		Step{Name: "scratch queue reset", Class: classState, Method: http.MethodPut, Path: "/playQueues/{playQueueID}/reset"},
		Step{Name: "scratch queue add", Class: classState, Method: http.MethodPut, Path: "/playQueues/{playQueueID}?uri=" + episodeURI},
		Step{Name: "scratch queue move", Class: classState, Method: http.MethodPut, Path: "/playQueues/{playQueueID}/items/{playQueueItemID}/move?after=0"},
		Step{Name: "scratch queue remove", Class: classState, Method: http.MethodDelete, Path: "/playQueues/{playQueueID}/items/{playQueueItemID}"},
		Step{Name: "scratch queue clear", Class: classState, Method: http.MethodDelete, Path: "/playQueues/{playQueueID}/items"},
		Step{Name: "scratch playlist create", Class: classState, Method: http.MethodPost, Path: "/playlists?type=video&title=Matrix+scratch+playlist&uri=" + movieURI, Discover: combineDiscover(discoverPlexItem("matrixPlexPlaylist", "playlist"), discoverMatrixPlexPlaylistArt)},
		Step{Name: "scratch playlist artwork", Class: classDetail, Path: "/{matrixPlexPlaylistArt}"},
		Step{Name: "scratch playlist artwork HEAD", Class: classDetail, Method: http.MethodHead, Path: "/{matrixPlexPlaylistArt}"},
		Step{Name: "scratch playlist edit", Class: classState, Method: http.MethodPut, Path: "/playlists/{matrixPlexPlaylist}?title=Matrix+renamed"},
		Step{Name: "scratch playlist add", Class: classState, Method: http.MethodPut, Path: "/playlists/{matrixPlexPlaylist}/items?uri=" + episodeURI},
		Step{Name: "scratch playlist item IDs", Class: classDetail, Path: "/playlists/{matrixPlexPlaylist}/items", Discover: discoverMatrixPlexField("Metadata", "playlistItemID", "matrixPlexPlaylistItem")},
		Step{Name: "scratch playlist move", Class: classState, Method: http.MethodPut, Path: "/playlists/{matrixPlexPlaylist}/items/{matrixPlexPlaylistItem}/move?after=0"},
		Step{Name: "scratch playlist remove", Class: classState, Method: http.MethodDelete, Path: "/playlists/{matrixPlexPlaylist}/items/{matrixPlexPlaylistItem}"},
		Step{Name: "scratch playlist compatibility delete", Class: classState, Method: http.MethodDelete, Path: "/library/metadata/{matrixPlexPlaylist}"},
		Step{Name: "scratch playlist clear", Class: classState, Method: http.MethodDelete, Path: "/playlists/{matrixPlexPlaylist}/items"},
		Step{Name: "scratch playlist delete", Class: classState, Method: http.MethodDelete, Path: "/playlists/{matrixPlexPlaylist}"},
		Step{Name: "dismiss continue watching", Class: classState, Method: http.MethodPut, Path: "/actions/removeFromContinueWatching?ratingKey={episodeKey}"},
	)
}

func discoverMatrixPlexPlaylistArt(vars map[string]string, body []byte) {
	var art string
	if json.Unmarshal(plexMetadata0(body)["composite"], &art) == nil && strings.HasPrefix(art, "/playlists/") {
		setOnce(vars, "matrixPlexPlaylistArt", strings.TrimPrefix(art, "/"))
	}
}
