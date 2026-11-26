package mediabrowser

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// TestItemDTODeclaredKindTypes proves a plugin kind's declared MediaBrowser
// item types drive the wire DTO: Type, MediaType, IsFolder and the
// track -> album parent naming, while an undeclared movie keeps the video
// shape it has always had.
func TestItemDTODeclaredKindTypes(t *testing.T) {
	jellyfin := &instance{protocol: "jellyfin"}
	albumKey := core.PluginKey(41)
	artistKey := core.PluginKey(40)

	track := jellyfin.itemDTO(frontend.Title{
		Key: core.PluginKey(42), Type: "episode", MBItemType: "Audio", KindLevel: "track",
		Title: "Fog Horn", ItemIndex: 3, ParentKey: albumKey, ParentTitle: "Harbour",
		ShowKey: artistKey, ShowTitle: "The Recordists",
	}, frontend.WatchStatus{})
	if track["Type"] != "Audio" || track["MediaType"] != "Audio" || track["IsFolder"] != false {
		t.Fatalf("track dto = %v %v %v", track["Type"], track["MediaType"], track["IsFolder"])
	}
	if track["Album"] != "Harbour" || track["AlbumId"] != ids.RatingKeyGUID(albumKey) || track["ParentId"] != ids.RatingKeyGUID(albumKey) {
		t.Fatalf("track album identity = %v %v %v", track["Album"], track["AlbumId"], track["ParentId"])
	}
	if track["IndexNumber"] != 3 {
		t.Fatalf("track index = %v", track["IndexNumber"])
	}
	if _, ok := track["SeriesId"]; ok {
		t.Fatalf("track carries a SeriesId: %v", track)
	}
	if _, ok := track["SeasonId"]; ok {
		t.Fatalf("track carries a SeasonId: %v", track)
	}

	album := jellyfin.itemDTO(frontend.Title{
		Key: albumKey, Type: "season", MBItemType: "MusicAlbum", KindLevel: "album",
		Title: "Harbour", ParentKey: artistKey, ParentTitle: "The Recordists",
	}, frontend.WatchStatus{})
	if album["Type"] != "MusicAlbum" || album["IsFolder"] != true || album["ParentId"] != ids.RatingKeyGUID(artistKey) {
		t.Fatalf("album dto = %v %v %v", album["Type"], album["IsFolder"], album["ParentId"])
	}
	if _, ok := album["MediaType"]; ok {
		t.Fatalf("album carries a MediaType: %v", album["MediaType"])
	}

	artist := jellyfin.itemDTO(frontend.Title{
		Key: artistKey, Type: "show", MBItemType: "MusicArtist", KindLevel: "artist", Title: "The Recordists",
	}, frontend.WatchStatus{})
	if artist["Type"] != "MusicArtist" || artist["IsFolder"] != true {
		t.Fatalf("artist dto = %v %v", artist["Type"], artist["IsFolder"])
	}

	book := jellyfin.itemDTO(frontend.Title{
		Key: core.PluginKey(43), Type: "movie", MBItemType: "Book", KindLevel: "book", Title: "Dune",
	}, frontend.WatchStatus{})
	if book["Type"] != "Book" || book["MediaType"] != "Book" || book["IsFolder"] != false {
		t.Fatalf("book dto = %v %v %v", book["Type"], book["MediaType"], book["IsFolder"])
	}

	audiobook := jellyfin.itemDTO(frontend.Title{
		Key: core.PluginKey(44), Type: "movie", MBItemType: "AudioBook", KindLevel: "audiobook", Title: "Dune (audio)",
	}, frontend.WatchStatus{})
	if audiobook["Type"] != "AudioBook" || audiobook["MediaType"] != "Audio" || audiobook["IsFolder"] != false {
		t.Fatalf("audiobook dto = %v %v %v", audiobook["Type"], audiobook["MediaType"], audiobook["IsFolder"])
	}

	movie := jellyfin.itemDTO(frontend.Title{Key: 5, Type: "movie", Title: "Film"}, frontend.WatchStatus{})
	if movie["Type"] != "Movie" || movie["MediaType"] != "Video" || movie["IsFolder"] != false {
		t.Fatalf("movie dto changed: %v %v %v", movie["Type"], movie["MediaType"], movie["IsFolder"])
	}
}

// TestItemDTOEmbyKindRootNamesOwnLibrary proves an Emby root item of a
// declared non-video kind names its plugin library view as parent, not a
// built-in section.
func TestItemDTOEmbyKindRootNamesOwnLibrary(t *testing.T) {
	emby := (&instance{protocol: "emby"}).itemDTO(frontend.Title{
		Key: core.PluginKey(40), Type: "show", MBItemType: "MusicArtist", KindLevel: "artist",
		Title: "The Recordists", LibraryID: 7,
	}, frontend.WatchStatus{})
	want, err := ids.GUID(ids.View, 7)
	if err != nil {
		t.Fatal(err)
	}
	if emby["ParentId"] != want {
		t.Fatalf("emby artist ParentId = %v, want %v", emby["ParentId"], want)
	}
	if emby["Type"] != "MusicArtist" {
		t.Fatalf("emby artist Type = %v", emby["Type"])
	}
}

// TestJellyfinAudioSourceKeepsAudioOnlyStreams proves the Jellyfin list-file
// shaping never invents a video stream (or a VideoType stamp) for a declared
// audio item whose source has none.
func TestJellyfinAudioSourceKeepsAudioOnlyStreams(t *testing.T) {
	i := &instance{protocol: "jellyfin"}
	title := frontend.Title{Key: core.PluginKey(42), Type: "episode", MBItemType: "Audio", Title: "Fog Horn"}
	item := map[string]any{}
	source := map[string]any{"MediaStreams": []any{}}
	i.presentJellyfinFile(nil, item, source, title)
	if streams, _ := source["MediaStreams"].([]any); len(streams) != 0 {
		t.Fatalf("audio source gained streams: %v", streams)
	}
	if _, ok := source["VideoType"]; ok {
		t.Fatalf("audio source stamped as video: %v", source)
	}
	if _, ok := item["Path"]; ok {
		t.Fatalf("audio item gained a video library path: %v", item)
	}

	audioOnly := map[string]any{"MediaStreams": []any{map[string]any{"Type": "Audio", "Codec": "flac"}}}
	jellyfinMediaSourceShape(audioOnly, "Fog Horn")
	if _, ok := audioOnly["VideoType"]; ok {
		t.Fatalf("audio-only source stamped VideoType: %v", audioOnly)
	}
	withVideo := map[string]any{"MediaStreams": []any{map[string]any{"Type": "Video", "Codec": "h264"}}}
	jellyfinMediaSourceShape(withVideo, "Film")
	if withVideo["VideoType"] != "VideoFile" {
		t.Fatalf("video source lost VideoType: %v", withVideo)
	}
}
