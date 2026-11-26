package mediabrowser

import (
	"net/http"
	"net/url"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// completeFileSource fills in the wire description of a VOD source from the
// facts already on hand. These estimates are for browsing only. The broker
// still discovers the actual representation and track indexes on playback.
func (i *instance) completeFileSource(r *http.Request, source map[string]any, title frontend.Title, itemPath string) {
	id, _ := source["Id"].(string)
	streams, _ := source["MediaStreams"].([]any)
	audio := title.MBItemType == "Audio" || title.MBItemType == "AudioBook"
	for _, raw := range streams {
		stream := raw.(map[string]any)
		if stream["Type"] == "Video" {
			audio = false
			break
		}
		audio = audio || stream["Type"] == "Audio"
	}
	container, _ := source["Container"].(string)
	if container == "" {
		if audio {
			container = "mp4"
		} else {
			container = "mkv"
		}
		source["Container"] = container
	}
	if len(streams) == 0 {
		tracks := []frontend.StreamInfo{{Type: "audio", Codec: "aac", Channels: 2}}
		if !audio {
			tracks = []frontend.StreamInfo{
				{Index: 0, Type: "video", Codec: "h264"},
				{Index: 1, Type: "audio", Codec: "aac", Channels: 2},
			}
		}
		source["MediaStreams"] = i.mediaStreams(tracks)
	}
	size, _ := source["Size"].(int64)
	if size <= 0 {
		ticks, _ := source["RunTimeTicks"].(int64)
		if ticks <= 0 {
			ticks = title.Runtime.Nanoseconds() / 100
		}
		if ticks <= 0 {
			duration := 110 * time.Minute
			if audio {
				duration = 4 * time.Minute
			} else if title.Type == "episode" {
				duration = 45 * time.Minute
			}
			ticks = duration.Nanoseconds() / 100
		}
		bitrate, _ := source["Bitrate"].(int)
		if bitrate <= 0 {
			bitrate = 5_000_000
			if audio {
				bitrate = 192_000
			}
		}
		source["Size"] = int64(float64(ticks) / 1e7 * float64(bitrate) / 8)
	}
	if path, _ := source["Path"].(string); path != "" {
		return
	}
	if itemPath == "" && !audio {
		itemPath = jellyfinLibraryPath(title, container)
	}
	if itemPath != "" {
		source["Path"] = itemPath
		if _, transcoding := source["TranscodingUrl"]; !transcoding {
			source["Protocol"] = "File"
			source["IsRemote"] = false
		}
		return
	}
	source["Path"] = staticSourcePath(r, title.Key, id, audio)
}

// This is an authenticated server route, with a scoped source id for a
// version. No upstream path or account credential is included.
func staticSourcePath(r *http.Request, key frontend.RatingKey, sourceID string, audio bool) string {
	id := sourceID
	if key != 0 {
		id = ids.RatingKeyGUID(key)
	}
	prefix := ""
	if r != nil {
		prefix, _ = r.Context().Value(prefixKey{}).(string)
	}
	route := "/Videos/"
	if audio {
		route = "/Audio/"
	}
	path := prefix + route + id + "/stream?Static=true"
	if sourceID != id && sourceID != "" {
		path += "&MediaSourceId=" + url.QueryEscape(sourceID)
	}
	return path
}

// completeDetailSources keeps the item-level tracks aligned with its default
// source. Other versions keep their own estimates and probed metadata.
func (i *instance) completeDetailSources(r *http.Request, dto map[string]any, detail frontend.ItemDetail, sources []any) {
	path, _ := dto["Path"].(string)
	for n, raw := range sources {
		source := raw.(map[string]any)
		if n == 0 && len(detail.Tracks) > 0 && (!hasMediaStreams(source) || (!detail.TracksEstimated && source["SupportsProbing"] != true)) {
			source["MediaStreams"] = i.mediaStreams(detail.Tracks)
		}
		i.completeFileSource(r, source, detail.Title, path)
		if n == 0 {
			dto["MediaStreams"] = source["MediaStreams"]
			dto["Container"] = source["Container"]
			if source["SupportsProbing"] == true {
				if ticks, _ := source["RunTimeTicks"].(int64); ticks > 0 {
					dto["RunTimeTicks"] = ticks
				}
			}
			setMissing(dto, "Path", source["Path"])
		}
	}
}
