package mediabrowser

import (
	"errors"
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
			return i.collectionFolder(r, v, frontend.View{ID: ids.Decimal(catchUpViewNumber), Name: frontend.CatchUpPresentationFor(r.Header.Get("Accept-Language")).Library, Type: frontend.TypeShow})
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
		"UserData": i.catchUpUserData(item), "ProviderIds": map[string]string{}}
	if item.Type != frontend.TypeShow {
		m["SeriesId"], m["SeriesName"] = frontend.CatchUpGroupGUID(item.ShowID), p.Item.Title
	}
	if item.Type == frontend.TypeSeason {
		m["Name"], m["IndexNumber"], m["ParentId"] = frontend.CatchUpPresentationFor(r.Header.Get("Accept-Language")).Library, 1, m["SeriesId"]
	}
	if item.Type == frontend.TypeEpisode {
		m["Name"] = frontend.CatchUpPresentationFor(r.Header.Get("Accept-Language")).EpisodeTitle(p)
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
	_, rollup := i.catchUpWatchState()
	page, err := lib.CatchUpItems(r.Context(), viewer, frontend.CatchUpQuery{
		Parent: parent, Recursive: recursive, Offset: q.Offset, Limit: q.Limit, Search: q.Filter.Query, Sort: frontend.CatchUpSortFor(q.Sort, q.Desc),
		Rollup: rollup,
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
	if watch, ok := i.catchUpWatchState(); ok && err == nil {
		item, err = frontend.CatchUpWithRollup(r.Context(), watch, v, item)
	}
	if err != nil {
		http.NotFound(w, r)
		return true
	}
	jsonReply(w, i.catchUpDTO(r, item, token))
	return true
}

// catchUpWatchState is the Live TV host's catch-up watch state, when Live TV
// is on and keeps it.
func (i *instance) catchUpWatchState() (frontend.CatchUpWatchState, bool) {
	live := i.host.LiveTV
	if live == nil || !live.Enabled() {
		return nil, false
	}
	w, ok := live.(frontend.CatchUpWatchState)
	return w, ok
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
	if !frontend.IsCatchUpGUID(body.ItemId) {
		return false
	}
	// Watch state first: a final report after the archive session ended (or
	// without one) still records the position. CatchUpProgress applies the
	// viewer's access to the airing itself.
	i.catchUpProgress(r, v, body, stopped)
	if body.PlaySessionId == "" {
		w.WriteHeader(http.StatusNoContent)
		return true
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

func (i *instance) catchUpProgress(r *http.Request, v *frontend.Viewer, body timelineRequest, stopped bool) {
	live := i.host.LiveTV
	watch, ok := live.(frontend.CatchUpWatchState)
	if !ok || !live.Enabled() || body.PositionTicks == "" {
		return
	}
	ticks, err := timelineTicks(body.PositionTicks)
	runtime, rerr := timelineTicks(body.RunTimeTicks)
	if err != nil || rerr != nil {
		return
	}
	state := "playing"
	if body.IsPaused {
		state = "paused"
	}
	if stopped {
		state = "stopped"
	}
	err = watch.CatchUpProgress(r.Context(), v, body.ItemId, time.Duration(ticks)*100, time.Duration(runtime)*100, state)
	if err != nil && !errors.Is(err, frontend.ErrNotFound) && i.host.Log != nil {
		i.host.Log.Warn("catch-up progress not saved", "frontend", i.protocol, "err", err)
	}
}

// catchUpWatchStatus is a catch-up item's watch state in the shape item user
// data uses. A show or season is played once every airing in it is.
func catchUpWatchStatus(item frontend.CatchUpItem) frontend.WatchStatus {
	if item.Type != frontend.TypeEpisode {
		st := frontend.WatchStatus{LastViewedAt: item.Watch.LastViewedAt}
		if item.Watch.FullyWatched() {
			st.ViewCount = 1
		}
		return st
	}
	p := item.Programme
	return frontend.WatchStatus{ViewOffset: item.Watch.ViewOffset, ViewCount: item.Watch.ViewCount,
		LastViewedAt: item.Watch.LastViewedAt, IsFavorite: item.Watch.IsFavorite, UserRating: item.Watch.UserRating, Duration: p.End.Sub(p.Start)}
}

// catchUpUserData is a catch-up item's UserData. Jellyfin also gets a show's
// or season's UnplayedItemCount, as for library shows.
func (i *instance) catchUpUserData(item frontend.CatchUpItem) map[string]any {
	data := i.userDataDTO(frontend.CatchUpGUID(item), catchUpWatchStatus(item))
	if i.protocol == "jellyfin" && item.Type != frontend.TypeEpisode && item.Watch.Episodes > 0 {
		data["UnplayedItemCount"] = max(item.Watch.Episodes-item.Watch.Watched, 0)
	}
	return data
}

// catchUpPlayed marks a catch-up airing, or every airing of a catch-up show
// or season, played or unplayed.
func (i *instance) catchUpPlayed(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, id string) {
	lib, okLib := i.host.LiveTV.(frontend.CatchUpLibrary)
	watch, ok := i.catchUpWatchState()
	if !ok || !okLib {
		http.NotFound(w, r)
		return
	}
	if err := watch.CatchUpSetWatched(r.Context(), v, id, r.Method == "POST"); err != nil {
		switch {
		case errors.Is(err, frontend.ErrNotFound):
			http.NotFound(w, r)
		case errors.Is(err, frontend.ErrUnauthorized):
			http.Error(w, "Unauthorized", 401)
		default:
			http.Error(w, "Unavailable", 503)
		}
		return
	}
	item, err := lib.CatchUpItem(r.Context(), v, id)
	if err == nil {
		item, err = frontend.CatchUpWithRollup(r.Context(), watch, v, item)
	}
	if err != nil {
		if errors.Is(err, frontend.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Unavailable", 503)
		return
	}
	dto := i.catchUpUserData(item)
	if i.protocol == "jellyfin" {
		if pct, ok := dto["PlayedPercentage"].(float64); ok && pct == 0 {
			delete(dto, "PlayedPercentage")
		}
	}
	jsonReply(w, dto)
}

func isCatchUpRecursive(r *http.Request) bool {
	return strings.EqualFold(r.URL.Query().Get("IncludeItemTypes"), "Episode")
}

func (i *instance) patchCatchUpUserData(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, id string, patch frontend.CatchUpUserDataPatch) {
	lib, okLib := i.host.LiveTV.(frontend.CatchUpLibrary)
	writer, ok := i.host.LiveTV.(frontend.CatchUpUserDataWriter)
	if !ok || !okLib || !i.host.LiveTV.Enabled() {
		http.NotFound(w, r)
		return
	}
	ctx := i.identityContext(r)
	if err := writer.CatchUpPatchUserData(ctx, v, id, patch); err != nil {
		writeUserDataError(w, r, err)
		return
	}
	item, err := lib.CatchUpItem(ctx, v, id)
	if err != nil {
		writeUserDataError(w, r, err)
		return
	}
	i.updatedUserData(w, r, frontend.CatchUpGUID(item), catchUpWatchStatus(item))
}
