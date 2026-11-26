package mediabrowser

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

const catchUpViewNumber = liveTVViewNumber + 2

func catchUpViewID() string {
	id, _ := ids.GUID(ids.View, catchUpViewNumber)
	return id
}

func (i *instance) catchUpView(r *http.Request, v *frontend.Viewer) (map[string]any, bool) {
	if lib, ok := i.host.LiveTV.(frontend.CatchUpLibrary); ok {
		if visible, err := lib.CatchUpLibraryVisible(r.Context(), v); err == nil && visible {
			return i.collectionFolder(r, v, frontend.View{ID: ids.Decimal(catchUpViewNumber), Name: frontend.CatchUpLibraryName, Type: frontend.TypeShow})
		}
	}
	return nil, false
}

func (i *instance) catchUpDTO(r *http.Request, item frontend.CatchUpItem, credential string) map[string]any {
	p := item.Programme
	id := frontend.CatchUpGUID(item)
	kind := map[frontend.MediaType]string{frontend.TypeShow: "Series", frontend.TypeSeason: "Season", frontend.TypeEpisode: "Episode"}[item.Type]
	m := map[string]any{"Id": id, "Name": p.Item.Title, "Type": kind, "Overview": p.Item.Summary, "ServerId": i.info.ID,
		"IsFolder": item.Type != frontend.TypeEpisode, "CanDownload": false, "CanDelete": false, "ParentId": catchUpViewID(),
		"UserData": i.userDataDTO(id, frontend.WatchStatus{}), "ProviderIds": map[string]string{}}
	if item.Type != frontend.TypeShow {
		m["SeriesId"], m["SeriesName"] = frontend.CatchUpGroupGUID(item.ShowID), p.Item.Title
	}
	if item.Type == frontend.TypeSeason {
		m["Name"], m["IndexNumber"], m["ParentId"] = frontend.CatchUpLibraryName, 1, m["SeriesId"]
	}
	if item.Type == frontend.TypeEpisode {
		if p.Item.EpisodeTitle != "" {
			m["Name"] = p.Item.EpisodeTitle
		}
		season := frontend.CatchUpGroupGUID(item.SeasonID)
		m["ParentId"], m["SeasonId"], m["ParentIndexNumber"] = season, season, 1
		m["MediaType"], m["RunTimeTicks"] = "Video", p.End.Sub(p.Start).Nanoseconds()/100
		m["PremiereDate"], m["ProductionYear"] = p.Start.UTC().Format(time.RFC3339Nano), p.Start.Year()
	}
	if i.host.ImageGrants != nil {
		for _, img := range item.Artwork {
			if img.Kind != frontend.ImageStill {
				continue
			}
			grant, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: credential, Device: device(r), Channel: p.ChannelID, Image: img})
			if err == nil && grant != "" {
				m["ImageTags"] = map[string]string{"Primary": grant}
			}
			break
		}
	}
	return m
}

func (i *instance) catchUpBrowse(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, token, parent string, recursive bool) bool {
	if n, err := ids.ParseGUID(ids.View, parent); err == nil && n == catchUpViewNumber {
		parent = frontend.CatchUpLibraryID
	} else if canonical, kind := frontend.CatchUpGUIDGroup(parent); kind != "" {
		parent = canonical
	} else {
		return false
	}
	lib, ok := i.host.LiveTV.(frontend.CatchUpLibrary)
	if !ok {
		http.NotFound(w, r)
		return true
	}
	q := itemQuery(r)
	page, err := lib.CatchUpItems(r.Context(), viewer, frontend.CatchUpQuery{
		Parent: parent, Recursive: recursive, Offset: q.Offset, Limit: q.Limit, Sort: frontend.CatchUpSortFor(q.Sort, q.Desc),
	})
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return true
	}
	items := make([]any, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, i.catchUpDTO(r, item, token))
	}
	i.replyItems(w, r, i.queryResult(items, page.Total, q.Offset))
	return true
}

func (i *instance) catchUpDetail(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, token, id string) bool {
	if !frontend.IsCatchUpGUID(id) {
		return false
	}
	lib, ok := i.host.LiveTV.(frontend.CatchUpLibrary)
	if !ok {
		http.NotFound(w, r)
		return true
	}
	item, err := lib.CatchUpItem(r.Context(), v, id)
	if err != nil {
		http.NotFound(w, r)
		return true
	}
	jsonReply(w, i.catchUpDTO(r, item, token))
	return true
}

func (i *instance) catchUpPlaybackInfo(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, token, wireID string, body playbackRequest, caps frontend.ClientCaps) {
	live := i.host.LiveTV
	lib, ok := live.(frontend.CatchUpLibrary)
	archive, playable := live.(frontend.LiveCatchUp)
	if !ok || !playable {
		http.NotFound(w, r)
		return
	}
	item, err := lib.CatchUpItem(r.Context(), viewer, wireID)
	if err != nil || item.Type != frontend.TypeEpisode {
		http.NotFound(w, r)
		return
	}
	req, ok := frontend.CatchUpEpisodeRequest(item.ID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	p := item.Programme
	startTicks := body.StartTimeTicks
	if values := route.Query(r.URL.Query(), "StartTimeTicks"); len(values) == 1 {
		startTicks, err = strconv.ParseInt(values[0], 10, 64)
	}
	if err != nil || startTicks < 0 || startTicks >= p.End.Sub(p.Start).Nanoseconds()/100 {
		http.Error(w, "Bad Request", 400)
		return
	}
	caps.Protocol = i.protocol
	if body.IsPlayback != nil && !*body.IsPlayback {
		jsonReply(w, map[string]any{"MediaSources": []any{catchUpMediaSource(wireID, p)}, "PlaySessionId": ""})
		return
	}
	session := body.PlaySessionId
	if session == "" {
		session = caps.ClientID + ":" + wireID
	}
	info, err := frontend.OpenLibraryCatchUp(r.Context(), live, archive, viewer, req, caps, session, time.Duration(startTicks)*100)
	if err != nil {
		i.liveTuneError(w, err)
		return
	}
	query := url.Values{"StartTimeTicks": {strconv.FormatInt(info.Origin.Nanoseconds()/100, 10)}}
	if info.MediaGeneration != "" {
		query.Set("archive", info.MediaGeneration)
	}
	if token != "" {
		query.Set("api_key", token)
	}
	mediaURL := "/Videos/" + wireID + "/CatchUp/" + info.ID + "/index.m3u8?" + query.Encode()
	media := catchUpMediaSource(wireID, p)
	media["TranscodingUrl"] = mediaURL
	media["TranscodingContainer"], media["TranscodingSubProtocol"] = "ts", "hls"
	if info.Decision.Container == "fmp4" || info.Decision.Container == "mp4" {
		media["TranscodingContainer"] = "mp4"
	}
	if i.protocol == "emby" {
		i.embyMediaSourceDTO(media, wireID, frontend.MediaInfo{})
	}
	jsonReply(w, map[string]any{"MediaSources": []any{media}, "PlaySessionId": info.ID})
}

func catchUpMediaSource(id string, p frontend.LiveTVProgramme) map[string]any {
	return map[string]any{"Id": id, "Name": p.Item.Title, "Protocol": "Http", "Type": "Default", "Container": "ts",
		"RunTimeTicks": p.End.Sub(p.Start).Nanoseconds() / 100, "IsInfiniteStream": false, "RequiresOpening": false,
		"SupportsDirectPlay": false, "SupportsDirectStream": false, "SupportsTranscoding": true, "MediaStreams": []any{}}
}

func (i *instance) catchUpSession(v *frontend.Viewer, id, itemID string) bool {
	h, ok := i.host.LiveTV.(frontend.CatchUpSessionReader)
	if !ok {
		return false
	}
	info, ok := h.LibraryCatchUpSession(frontend.SessionAccess{Viewer: v, Protocol: i.protocol}, id)
	if !ok {
		return false
	}
	key, stamp, valid := frontend.CatchUpGUIDAiring(itemID)
	return valid && key == frontend.CatchUpChannelKey(info.ChannelID) && stamp.UnixMilli() == info.Start.UnixMilli()
}

func (i *instance) catchUpStream(w http.ResponseWriter, r *http.Request) {
	viewer, token, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	live := i.host.LiveTV
	if !i.catchUpSession(viewer, r.PathValue("session"), r.PathValue("id")) {
		http.NotFound(w, r)
		return
	}
	id, file := r.PathValue("session"), r.PathValue("file")
	if file != "index.m3u8" {
		if _, ok := frontend.ParseSegmentName(file); !ok {
			http.NotFound(w, r)
			return
		}
		live.ServeResource(w, r, id, file, viewer)
		return
	}
	frontend.ServeCatchUpPlaylist(w, r, live, id, viewer, func(name string) string {
		if token == "" {
			return name
		}
		return name + "?" + url.Values{"api_key": {token}}.Encode()
	})
}

func (i *instance) catchUpTimeline(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, body timelineRequest, stopped bool) bool {
	if !frontend.IsCatchUpGUID(body.ItemId) || body.PlaySessionId == "" {
		return false
	}
	live := i.host.LiveTV
	archive, ok := live.(frontend.LiveCatchUp)
	if !ok || !i.catchUpSession(v, body.PlaySessionId, body.ItemId) {
		http.NotFound(w, r)
		return true
	}
	if stopped {
		_ = live.Stop(r.Context(), body.PlaySessionId, v)
	} else {
		_ = archive.TouchLiveSession(r.Context(), v, body.PlaySessionId)
	}
	w.WriteHeader(http.StatusNoContent)
	return true
}

func isCatchUpRecursive(r *http.Request) bool {
	return strings.EqualFold(r.URL.Query().Get("IncludeItemTypes"), "Episode")
}
