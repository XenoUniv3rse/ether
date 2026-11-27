package silo

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
)

func siloCatchUpSort(r *http.Request) frontend.CatchUpSort {
	switch order := frontend.CatchUpSort(r.URL.Query().Get("sort")); order {
	case frontend.CatchUpSortOldest, frontend.CatchUpSortTitle, frontend.CatchUpSortTitleDesc:
		return order
	default:
		return frontend.CatchUpSortRecent
	}
}

func (i *instance) catchUpPage(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, token string, q frontend.CatchUpQuery) ([]any, frontend.CatchUpPage, bool) {
	q.Sort = siloCatchUpSort(r)
	q.Search = strings.TrimSpace(r.URL.Query().Get("q"))
	lib, ok := i.host.LiveTV.(frontend.CatchUpLibrary)
	if !ok {
		notFound(w)
		return nil, frontend.CatchUpPage{}, false
	}
	_, q.Rollup = i.catchUpWatchState()
	page, err := lib.CatchUpItems(r.Context(), viewer, q)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return nil, frontend.CatchUpPage{}, false
	}
	items := make([]any, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, i.catchUpCard(r, token, item))
	}
	return items, page, true
}

func (i *instance) catchUpCard(r *http.Request, token string, item frontend.CatchUpItem) map[string]any {
	p := item.Programme
	id := frontend.CatchUpGUID(item)
	name := p.Item.Title
	if item.Type == frontend.TypeEpisode {
		name = frontend.CatchUpPresentationFor(r.Header.Get("Accept-Language")).EpisodeTitle(p)
	}
	if item.Type == frontend.TypeSeason {
		name = frontend.CatchUpPresentationFor(r.Header.Get("Accept-Language")).Library
	}
	card := titleDTO(frontend.Title{
		Type: item.Type, Title: name, Summary: p.Item.Summary, ShowTitle: p.Item.Title,
		Runtime: p.End.Sub(p.Start), Released: p.Start, Year: p.Start.Year(), Season: 1, ContentRating: p.Item.Rating,
	}, frontend.WatchStatus{})
	card["content_id"] = id
	if item.Type != frontend.TypeShow {
		card["series_id"] = frontend.CatchUpGroupGUID(item.ShowID)
		card["series_title"] = p.Item.Title
	}
	if data := catchUpUserData(item); data != nil {
		card["user_data"] = data
	}
	if item.Type == frontend.TypeEpisode {
		card["play_content_id"] = id
	}
	if i.host.ImageGrants != nil {
		var grants []frontend.GrantedImage
		for _, img := range item.Artwork {
			id, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: token, Device: device(r), Channel: p.ChannelID, Image: img})
			if err == nil {
				grants = append(grants, frontend.GrantedImage{Image: img, Secret: id})
			}
		}
		i.paintCardArtwork(r, grants, card)
		if still, ok := card["still_url"]; ok {
			card["poster_url"] = still
		}
	}
	return card
}

// catchUpUserData is a catch-up card's user_data: an airing's own state, or
// a show's or season's roll-up (none when it has no counts, as in a search).
func catchUpUserData(item frontend.CatchUpItem) map[string]any {
	w := item.Watch
	if item.Type != frontend.TypeEpisode {
		if w.Episodes == 0 {
			return nil
		}
		return map[string]any{"played": w.FullyWatched(), "is_in_progress": w.InProgress > 0, "watched_count": w.Watched,
			"unplayed_count": max(w.Episodes-w.Watched, 0), "in_progress_count": w.InProgress}
	}
	p := item.Programme
	played, inProgress := w.ViewCount > 0, w.ViewOffset > 0 && w.ViewCount == 0
	watched, unplayed, progressing := 0, 1, 0
	if played {
		watched, unplayed = 1, 0
	}
	if inProgress {
		progressing = 1
	}
	data := map[string]any{"played": played, "is_in_progress": inProgress, "watched_count": watched, "unplayed_count": unplayed,
		"in_progress_count": progressing, "duration_seconds": p.End.Sub(p.Start).Seconds()}
	if w.ViewOffset > 0 {
		data["position_seconds"] = w.ViewOffset.Seconds()
	}
	return data
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

// catchUpWatched marks a catch-up airing, or every airing of a catch-up show
// or season, watched (POST) or unwatched (DELETE).
func (i *instance) catchUpWatched(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, id string) {
	watch, ok := i.catchUpWatchState()
	if !ok {
		notFound(w)
		return
	}
	if err := watch.CatchUpSetWatched(r.Context(), v, id, r.Method == "POST"); err != nil {
		if errors.Is(err, frontend.ErrNotFound) {
			notFound(w)
			return
		}
		http.Error(w, "Unavailable", 503)
		return
	}
	i.dropViewerSections(v.ID)
	w.WriteHeader(http.StatusNoContent)
}

// catchUpProgress records a progress sample of a catch-up session as the
// airing's watch state. Positions are seconds into the airing.
func (i *instance) catchUpProgress(r *http.Request, v *frontend.Viewer, info frontend.CatchUpInfo, position *float64, paused *bool, stop bool) {
	watch, ok := i.catchUpWatchState()
	if !ok || info.ProgrammeID == "" || info.StartOver || position == nil || !(*position >= 0) || *position > float64((1<<63-1)/int64(time.Second)) {
		return
	}
	state := "playing"
	switch {
	case stop:
		state = "stopped"
	case paused != nil && *paused:
		state = "paused"
	}
	err := watch.CatchUpProgress(r.Context(), v, frontend.CatchUpEpisodeID(info.ProgrammeID), time.Duration(*position*float64(time.Second)), 0, state)
	if err != nil && !errors.Is(err, frontend.ErrNotFound) && i.host.Log != nil {
		i.host.Log.Warn("catch-up progress not saved", "frontend", "silo", "err", err)
	}
}

func (i *instance) catchUpDetail(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, token, id string) {
	lib, ok := i.host.LiveTV.(frontend.CatchUpLibrary)
	if !ok {
		notFound(w)
		return
	}
	item, err := lib.CatchUpItem(r.Context(), viewer, id)
	if watch, ok := i.catchUpWatchState(); ok && err == nil {
		item, err = frontend.CatchUpWithRollup(r.Context(), watch, viewer, item)
	}
	if err != nil {
		notFound(w)
		return
	}
	out := i.catchUpCard(r, token, item)
	out["cast"], out["crew"], out["ratings"], out["subtitles"] = []any{}, []any{}, []any{}, []any{}
	out["themes"] = map[string]any{"owner_id": id, "items": []any{}}
	out["versions"] = []any{}
	if item.Type == frontend.TypeEpisode {
		p := item.Programme
		duration := int64(p.End.Sub(p.Start).Seconds())
		version := map[string]any{"file_id": id, "resolution": "", "codec_video": "", "codec_audio": "",
			"hdr": false, "container": "hls", "duration": duration, "file_size": int64(0), "bitrate": 0,
			"added_at": p.Start.UTC().Format("2006-01-02T15:04:05.000Z")}
		part := map[string]any{"part_index": 1, "default_file_id": id, "versions": []any{version}, "total_duration": duration}
		out["versions"] = []any{version}
		out["playback_variants"] = []any{map[string]any{"variant_id": id, "default_file_id": id, "part_count": 1, "parts": []any{part}, "total_duration": duration}}
	}
	i.finishDetailResponse(w, r, viewer, frontend.ItemDetail{}, nil, out)
}

func (i *instance) startCatchUp(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, b startRequest, caps frontend.ClientCaps) {
	live := i.host.LiveTV
	lib, ok := live.(frontend.CatchUpLibrary)
	archive, playable := live.(frontend.LiveCatchUp)
	if !ok || !playable {
		notFound(w)
		return
	}
	item, err := lib.CatchUpItem(r.Context(), viewer, b.FileID)
	if err != nil || item.Type != frontend.TypeEpisode {
		notFound(w)
		return
	}
	req, ok := frontend.CatchUpEpisodeRequest(item.ID)
	if !ok {
		notFound(w)
		return
	}
	if b.Position >= item.Programme.End.Sub(item.Programme.Start).Seconds() {
		http.Error(w, "Bad Request", 400)
		return
	}
	d := device(r)
	caps.Protocol, caps.ClientID = "silo", d.ID
	info, err := frontend.OpenLibraryCatchUp(r.Context(), live, archive, viewer, req, caps, b.Attempt, time.Duration(b.Position*float64(time.Second)))
	if err != nil {
		i.playbackError(w, viewer, err, r.Header.Get("Accept-Language"))
		return
	}
	reply(w, http.StatusCreated, catchUpPlan(info, d, b.FileID, b.Position))
}

func catchUpPlan(info frontend.CatchUpInfo, d frontend.Device, file string, position float64) map[string]any {
	mediaURL := siloMediaURL(info.ID, frontend.StreamHLS)
	if info.MediaGeneration != "" {
		mediaURL += "?seek=" + url.QueryEscape(info.MediaGeneration)
	}
	delivery := "server_remux_hls"
	if !info.Decision.VideoCopy && info.Decision.VideoCodec != "" {
		delivery = "server_transcode_hls"
	}
	plan := map[string]any{
		"protocol_version": 3, "plan_id": info.MediaGeneration, "plan_attempt_key": info.MediaGeneration, "session_id": info.ID,
		"delivery": delivery, "requested_media_file_id": file, "effective_media_file_id": file,
		"stream": map[string]any{"url": mediaURL, "protocol": "hls", "container": "hls", "mime_type": "application/vnd.apple.mpegurl", "headers": map[string]string{"X-Silo-Device-Id": d.ID}, "header_refresh": "none"},
		"timeline": map[string]any{"source_start_seconds": position, "stream_origin_seconds": info.Origin.Seconds(),
			"player_start_seconds": max(position-info.Origin.Seconds(), 0), "timeline_offset_seconds": info.Origin.Seconds(),
			"can_seek_anywhere": true, "seek_restoration": "player_position"},
		"source":          map[string]any{"media_file_id": file, "duration_seconds": info.End.Sub(info.Start).Seconds(), "hdr10_plus": false, "dv_enhancement_layer": "unknown"},
		"selected_tracks": map[string]any{}, "subtitle": map[string]any{"delivery": "none"},
		"effective_recipe": map[string]any{"video_codec": info.Decision.VideoCodec, "audio_codec": info.Decision.AudioCodec, "bitrate_kbps": info.Decision.BitrateKbps},
		"claims": map[string]any{"audio": map[string]any{"passthrough": false, "atmos_preserved": false},
			"video":     map[string]any{"hdr10": false, "hdr10_plus": false, "hlg": false, "dolby_vision": false},
			"subtitles": map[string]any{"ass_styling_preserved": false, "bitmap_overlay": false, "bitmap_sidecar": false}},
		"transformations": []any{}, "applied_quirks": []any{}, "runtime_corrections": []any{}, "degradation_warnings": []any{}, "available_qualities": []any{},
	}
	return map[string]any{"outcome": "playable", "protocol_version": 3, "server_features": siloServerFeatures, "session_id": info.ID, "playback_plan": plan}
}

func (i *instance) catchUpSession(r *http.Request, viewer *frontend.Viewer, id string) (frontend.CatchUpInfo, bool) {
	h, ok := i.host.LiveTV.(frontend.CatchUpSessionReader)
	if !ok {
		return frontend.CatchUpInfo{}, false
	}
	return h.LibraryCatchUpSession(frontend.SessionAccess{Viewer: viewer, Device: device(r), Protocol: "silo"}, id)
}

func (i *instance) catchUpMedia(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, id, resource string) {
	live := i.host.LiveTV
	if resource == "master.m3u8" {
		if generation := r.URL.Query().Get("seek"); generation != "" {
			forward := r.Clone(r.Context())
			query := forward.URL.Query()
			query.Set("archive", generation)
			forward.URL.RawQuery = query.Encode()
			r = forward
		}
		live.ServePlaylist(w, r, id, viewer)
		return
	}
	name, ok := strings.CutPrefix(resource, siloHLSDir)
	if !ok || strings.Contains(name, "/") {
		notFound(w)
		return
	}
	live.ServeResource(w, r, id, name, viewer)
}

func (i *instance) replanCatchUp(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, info frontend.CatchUpInfo, b replanRequest) {
	archive, ok := i.host.LiveTV.(frontend.LiveCatchUp)
	if !ok || b.Operation != "seek_reanchor" {
		siloAdaptationUnavailable(w, info.ID)
		return
	}
	if b.Position >= info.End.Sub(info.Start).Seconds() {
		http.Error(w, "Bad Request", 400)
		return
	}
	next, err := archive.SeekCatchUp(r.Context(), viewer, info.ID, time.Duration(b.Position*float64(time.Second)))
	if err != nil {
		i.playbackError(w, viewer, err, r.Header.Get("Accept-Language"))
		return
	}
	file := frontend.CatchUpGUID(frontend.CatchUpItem{Type: frontend.TypeEpisode, Programme: frontend.LiveTVProgramme{ChannelID: next.ChannelID, Start: next.Start}})
	reply(w, 200, catchUpPlan(next, device(r), file, b.Position))
}

func catchUpNext(page frontend.CatchUpPage, offset int, cursor func(int) string) map[string]any {
	next := offset + len(page.Items)
	more := len(page.Items) > 0 && next < page.Total
	out := map[string]any{"has_more": more}
	if more {
		out["next_cursor"] = cursor(next)
	}
	return out
}

func (i *instance) catchUpCatalog(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, token string, offset, limit int) {
	items, page, ok := i.catchUpPage(w, r, v, token, frontend.CatchUpQuery{Parent: frontend.CatchUpLibraryID, Offset: offset, Limit: limit})
	if !ok {
		return
	}
	next := catchUpNext(page, offset, strconv.Itoa)
	reply(w, 200, map[string]any{"items": items, "total": page.Total, "total_exact": siloCatchUpSort(r) != frontend.CatchUpSortRecent || !next["has_more"].(bool), "window_cursor": "", "page": next})
}

func (i *instance) catchUpSections(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, token string) {
	limit, err := paging.ParseLimit(r.URL.Query().Get("item_limit"), 24, 100)
	if err != nil {
		http.Error(w, "Bad Request", 400)
		return
	}
	items, page, ok := i.catchUpPage(w, r, v, token, frontend.CatchUpQuery{Parent: frontend.CatchUpLibraryID, Limit: limit})
	if !ok {
		return
	}
	section := map[string]any{"id": frontend.CatchUpLibraryID, "title": frontend.CatchUpPresentationFor(r.Header.Get("Accept-Language")).Library, "section_type": "custom",
		"customized": false, "featured": false, "is_custom": false, "item_limit": len(items), "items": items}
	if len(items) >= page.Total {
		section["total_count"] = page.Total
	}
	reply(w, 200, map[string]any{"sections": []any{section}})
}
