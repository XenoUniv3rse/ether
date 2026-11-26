package plex

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

const catchUpSectionID = 2147483646
const catchUpMetadataPrefix = "/library/metadata/"

func (s *Server) viewerSections(r *http.Request) []Section {
	sections := s.userSections(FromContext(r.Context()).User)
	if section, ok := s.catchUpSection(r); ok {
		return append(append([]Section(nil), sections...), section)
	}
	return sections
}

func (s *Server) catchUpSection(r *http.Request) (Section, bool) {
	sec := s.sectionDefaults(Section{ID: catchUpSectionID, Title: frontend.CatchUpLibraryName, Type: frontend.TypeShow})
	lib, ok := s.svc().liveTV.(frontend.CatchUpLibrary)
	if !ok {
		return sec, false
	}
	visible, err := lib.CatchUpLibraryVisible(r.Context(), FromContext(r.Context()).User)
	return sec, err == nil && visible
}

func catchUpMetadata(sec Section, item frontend.CatchUpItem) model.Metadata {
	p := item.Programme
	key := catchUpMetadataPrefix + item.ID
	md := model.Metadata{
		RatingKey: item.ID, Key: key, GUID: "ether://" + item.ID,
		Title: p.Item.Title, Summary: p.Item.Summary, Type: string(item.Type),
		LibrarySectionID: sec.ID, LibrarySectionTitle: sec.Title, LibrarySectionUUID: sec.UUID,
		LibrarySectionKey: "/library/sections/" + strconv.Itoa(sec.ID),
	}
	for _, img := range item.Artwork {
		switch img.Kind {
		case frontend.ImageStill:
			md.Thumb = "/library/catchup/art/" + item.ID + "/" + img.Kind
		case frontend.ImageBackdrop:
			md.Art = "/library/catchup/art/" + item.ID + "/" + img.Kind
		}
	}
	switch item.Type {
	case frontend.TypeShow:
		md.Key += "/children"
		md.ChildCount = model.Ptr(1)
	case frontend.TypeSeason:
		md.Title = frontend.CatchUpLibraryName
		md.Key += "/children"
		md.Index = model.Ptr(1)
		md.ParentTitle, md.ParentRatingKey = p.Item.Title, item.ShowID
		md.ParentKey = catchUpMetadataPrefix + item.ShowID
	case frontend.TypeEpisode:
		md.SetElement("Video")
		md.Title = p.Item.Title
		if p.Item.EpisodeTitle != "" {
			md.Title = p.Item.EpisodeTitle
		}
		md.ParentTitle, md.GrandparentTitle = frontend.CatchUpLibraryName, p.Item.Title
		md.ParentRatingKey, md.GrandparentRatingKey = item.SeasonID, item.ShowID
		md.ParentKey, md.GrandparentKey = catchUpMetadataPrefix+item.SeasonID, catchUpMetadataPrefix+item.ShowID
		md.ParentIndex = model.Ptr(1)
		md.OriginallyAvailableAt = p.Start.Format("2006-01-02")
		md.AddedAt = p.Start.Unix()
		md.Duration = p.End.Sub(p.Start).Milliseconds()
		part := "/video/:/transcode/universal/start.m3u8?" + url.Values{"path": {key}}.Encode()
		md.Media = []model.Media{{ID: 1, Duration: md.Duration, Container: "mpegts", Protocol: "hls",
			Part: []model.Part{{ID: 1, Key: part, Duration: md.Duration, Container: "mpegts", Protocol: "hls"}}}}
	}
	return md
}

func (s *Server) catchUpBrowse(w http.ResponseWriter, r *http.Request, parent string, recursive bool) {
	lib, ok := s.svc().liveTV.(frontend.CatchUpLibrary)
	if !ok {
		writeHTTPError(w, 404)
		return
	}
	pr := FromContext(r.Context())
	sec := s.sectionDefaults(Section{ID: catchUpSectionID, Title: frontend.CatchUpLibraryName, Type: frontend.TypeShow})
	offset, limit := pr.pageOf(0)
	page, err := lib.CatchUpItems(r.Context(), pr.User, frontend.CatchUpQuery{Parent: parent, Offset: offset, Limit: limit, Recursive: recursive})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	mc := s.sectionContainer(sec)
	for _, item := range page.Items {
		mc.Metadata = append(mc.Metadata, catchUpMetadata(sec, item))
	}
	mc.Size = len(mc.Metadata)
	mc.SetPage(offset, page.Total)
	s.write(w, r, mc)
}

func (s *Server) catchUpDetail(w http.ResponseWriter, r *http.Request, id string) {
	lib, ok := s.svc().liveTV.(frontend.CatchUpLibrary)
	if !ok {
		writeHTTPError(w, 404)
		return
	}
	item, err := lib.CatchUpItem(r.Context(), FromContext(r.Context()).User, id)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	sec := s.sectionDefaults(Section{ID: catchUpSectionID, Title: frontend.CatchUpLibraryName, Type: frontend.TypeShow})
	mc := s.sectionContainer(sec)
	mc.Size, mc.Metadata = 1, []model.Metadata{catchUpMetadata(sec, item)}
	s.write(w, r, mc)
}

func (s *Server) catchUpArt(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.svc().liveTV.(frontend.CatchUpArtworkReader)
	if !ok {
		writeHTTPError(w, 404)
		return
	}
	data, kind, err := reader.CatchUpArtwork(r.Context(), FromContext(r.Context()).User, chi.URLParam(r, "key"), chi.URLParam(r, "kind"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", kind)
	_, _ = w.Write(data)
}

func (s *Server) handleCatchUpPlayback(w http.ResponseWriter, r *http.Request) bool {
	path, _, _ := strings.Cut(r.URL.Query().Get("path"), "?")
	id, ok := strings.CutPrefix(path, catchUpMetadataPrefix)
	if !ok || !frontend.IsCatchUpID(id) {
		return false
	}
	if s.refuseCapability(w, r) {
		return true
	}
	live := s.svc().liveTV
	lib, hasLibrary := live.(frontend.CatchUpLibrary)
	archive, hasPlayback := live.(frontend.LiveCatchUp)
	req, episode := frontend.CatchUpEpisodeRequest(id)
	pr := FromContext(r.Context())
	if !hasLibrary || !hasPlayback || !episode || pr.User == nil {
		writeHTTPError(w, 404)
		return true
	}
	if p, ok := s.policy().(frontend.PlaybackAccessPolicy); ok && !p.CanPlay(pr.User, pr.Remote, s.now()) {
		writeHTTPError(w, 403)
		return true
	}
	item, err := lib.CatchUpItem(r.Context(), pr.User, id)
	if err != nil {
		s.writeErr(w, r, err)
		return true
	}
	dash := strings.HasSuffix(r.URL.Path, "/start.mpd") || r.URL.Query().Get("protocol") == "dash"
	if strings.HasSuffix(r.URL.Path, "/decision") {
		md := catchUpMetadata(Section{ID: catchUpSectionID, Title: frontend.CatchUpLibraryName}, item)
		md.Media[0].Part[0].Decision = "transcode"
		if dash {
			md.Media[0].Protocol, md.Media[0].Part[0].Protocol = "dash", "dash"
			md.Media[0].Container, md.Media[0].Part[0].Container = "mp4", "mp4"
			md.Media[0].Part[0].Key = strings.Replace(md.Media[0].Part[0].Key, "/start.m3u8?", "/start.mpd?", 1)
		}
		s.write(w, r, &model.MediaContainer{Size: 1, Identifier: model.LibraryIdentifier, Metadata: []model.Metadata{md},
			Extra: []model.Attr{model.A("generalDecisionCode", 1001), model.A("transcodeDecisionCode", 1001)}})
		return true
	}
	off, err := strconv.ParseFloat(r.URL.Query().Get("offset"), 64)
	duration := item.Programme.End.Sub(item.Programme.Start)
	if r.URL.Query().Has("offset") && (err != nil || !(off >= 0 && off < duration.Seconds())) {
		writeHTTPError(w, 400)
		return true
	}
	session := r.URL.Query().Get("session")
	if session == "" {
		session = pr.SessionID
	}
	unlock, err := s.lockPlaybackOwner(r.Context(), pr.User.ID, pr.ClientID)
	if err != nil {
		s.writeErr(w, r, err)
		return true
	}
	defer unlock()
	req.DASH = dash
	info, err := frontend.OpenLibraryCatchUp(r.Context(), live, archive, pr.User, req, s.clientCaps(pr), session, time.Duration(off*float64(time.Second)))
	if err != nil {
		s.writeErr(w, r, err)
		return true
	}
	s.noteLiveSessionAlias(r, info.ID, pr)
	if dash {
		if err := live.ServeDASH(w, r, info.ID, pr.User); err != nil {
			s.writeErr(w, r, err)
		}
		return true
	}
	frontend.ServeCatchUpPlaylist(w, r, live, info.ID, pr.User, func(name string) string {
		path := "/livetv/sessions/" + info.ID + "/0/resource/" + name
		if pr.Token != "" {
			path += "?" + url.Values{"X-Plex-Token": {pr.Token}}.Encode()
		}
		return path
	})
	return true
}

func (s *Server) createCatchUpQueue(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	uri := q.Get("uri")
	if strings.HasPrefix(uri, "server://") {
		authority, _, _ := strings.Cut(strings.TrimPrefix(uri, "server://"), "/")
		if authority != s.identity().MachineIdentifier {
			return false
		}
	}
	path, _ := uriPath(uri)
	id, ok := strings.CutPrefix(path, catchUpMetadataPrefix)
	if !ok {
		return false
	}
	id, _, _ = strings.Cut(id, "/")
	id, _, _ = strings.Cut(id, "?")
	if !frontend.IsCatchUpID(id) {
		return false
	}
	lib, ok := s.svc().liveTV.(frontend.CatchUpLibrary)
	if !ok || q.Get("type") != "video" {
		writeHTTPError(w, 404)
		return true
	}
	pr := FromContext(r.Context())
	ctx := r.Context()
	if _, episode := frontend.CatchUpEpisodeRequest(id); !episode {
		page, err := lib.CatchUpItems(ctx, pr.User, frontend.CatchUpQuery{Parent: id, Limit: 1, Recursive: true})
		if err != nil {
			s.writeErr(w, r, err)
			return true
		}
		if len(page.Items) == 0 {
			writeHTTPError(w, 404)
			return true
		}
		id = page.Items[0].ID
	}
	if _, err := lib.CatchUpItem(ctx, pr.User, id); err != nil {
		s.writeErr(w, r, err)
		return true
	}
	// The queue's one slot is synthetic. Use the live queue's opaque target
	// field so the normal catalogue path never resolves that placeholder.
	pq, err := s.queues.CreateWith(pr.User.ID, pr.ClientID, []frontend.RatingKey{frontend.MovieKey(1)}, s.now(), func(pq *playQueue) {
		pq.LiveSession, pq.SourceURI = id, uri
	})
	if err != nil {
		s.writeErr(w, r, err)
		return true
	}
	s.writeCatchUpQueue(w, r, pq)
	return true
}

func (s *Server) writeCatchUpQueue(w http.ResponseWriter, r *http.Request, pq playQueue) {
	lib, ok := s.svc().liveTV.(frontend.CatchUpLibrary)
	if !ok || len(pq.Items) != 1 {
		writeHTTPError(w, 404)
		return
	}
	item, err := lib.CatchUpItem(r.Context(), FromContext(r.Context()).User, pq.LiveSession)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	md := catchUpMetadata(Section{ID: catchUpSectionID, Title: frontend.CatchUpLibraryName}, item)
	md.PlayQueueItemID = pq.Items[0].ID
	s.write(w, r, &model.MediaContainer{Size: 1, Identifier: model.LibraryIdentifier, Metadata: []model.Metadata{md},
		Extra: []model.Attr{model.A("playQueueID", pq.ID), model.A("playQueueSelectedItemID", pq.Items[0].ID),
			model.A("playQueueSelectedItemOffset", 0), model.A("playQueueShuffled", false), model.A("playQueueSourceURI", pq.SourceURI),
			model.A("playQueueTotalCount", 1), model.A("playQueueVersion", pq.Version)}})
}

func (s *Server) isCatchUpSession(user *frontend.User, id string) bool {
	reader, ok := s.svc().liveTV.(frontend.CatchUpSessionReader)
	if !ok {
		return false
	}
	_, ok = reader.LibraryCatchUpSession(frontend.SessionAccess{Viewer: user, Protocol: "plex"}, id)
	return ok
}

func (s *Server) isCatchUpMediaRequest(r *http.Request, pr *Request) bool {
	id := chi.URLParam(r, "session")
	return id != "" && (chi.URLParam(r, "resource") != "" || chi.URLParam(r, "part") != "") && s.isCatchUpSession(pr.User, id)
}

// catchUpSearch returns the Catch-up shows whose airings match query, for the
// search routes. Past airings are only reachable through the Catch-up
// section, so without this a search finds a programme's upcoming airings but
// not the ones a viewer can still watch.
func (s *Server) catchUpSearch(r *http.Request, query string, limit int) []model.Metadata {
	query = strings.TrimSpace(query)
	lib, ok := s.svc().liveTV.(frontend.CatchUpLibrary)
	if !ok || query == "" || limit <= 0 {
		return nil
	}
	pr := FromContext(r.Context())
	page, err := lib.CatchUpItems(r.Context(), pr.User, frontend.CatchUpQuery{Parent: frontend.CatchUpLibraryID, Search: query, Limit: limit})
	if err != nil || len(page.Items) == 0 {
		return nil
	}
	sec := s.sectionDefaults(Section{ID: catchUpSectionID, Title: frontend.CatchUpLibraryName, Type: frontend.TypeShow})
	out := make([]model.Metadata, 0, len(page.Items))
	for _, item := range page.Items {
		out = append(out, catchUpMetadata(sec, item))
	}
	return out
}
