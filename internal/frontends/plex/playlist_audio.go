package plex

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Plugin keys share a numeric namespace, so their media type must come from
// the title projection. Batch only playlist members, never section contents.
func (s *Server) playlistPluginTitles(ctx context.Context, keys []frontend.RatingKey) map[frontend.RatingKey]frontend.Title {
	var pluginKeys []frontend.RatingKey
	seen := make(map[frontend.RatingKey]bool)
	for _, key := range keys {
		if key.IsPluginKey() && !seen[key] {
			pluginKeys = append(pluginKeys, key)
			seen[key] = true
		}
	}
	return s.kindItemsBatch(ctx, FromContext(ctx), pluginKeys)
}

func (s *Server) playlistTrackKeys(ctx context.Context, keys []frontend.RatingKey) map[frontend.RatingKey]bool {
	return s.playlistTrackTypes(s.playlistPluginTitles(ctx, keys))
}

func (s *Server) playlistTrackTypes(titles map[frontend.RatingKey]frontend.Title) map[frontend.RatingKey]bool {
	tracks := make(map[frontend.RatingKey]bool, len(titles))
	for key, title := range titles {
		tracks[key] = s.plexTypeFor(&title) == plexBaseTrack
	}
	return tracks
}

func playlistMembershipType(keys []frontend.RatingKey, tracks map[frontend.RatingKey]bool) string {
	hasTrack := false
	for _, key := range keys {
		track, known := tracks[key]
		if key.IsPluginKey() && !known {
			continue
		}
		if !track {
			return "video"
		}
		hasTrack = true
	}
	if hasTrack {
		return "audio"
	}
	return "video"
}

func (s *Server) playlistType(ctx context.Context, pl *frontend.Playlist) string {
	if smartAudioSection(pl.SmartURI) != 0 {
		return "audio"
	}
	return playlistMembershipType(pl.Items, s.playlistTrackKeys(ctx, pl.Items))
}

// Normalized smart audio URIs always select tracks from one music section.
func smartAudioSection(uri string) int {
	path, raw, _ := strings.Cut(uri, "?")
	m := reSectionPath.FindStringSubmatch(path)
	if len(m) == 0 {
		return 0
	}
	id, _ := strconv.Atoi(m[1])
	q, _ := url.ParseQuery(raw)
	if typeFromParam(q.Get("type")) != plexBaseTrack {
		return 0
	}
	return id
}

func smartAudioQuery(sec Section, q url.Values) (frontend.ItemQuery, error) {
	iq := frontend.ItemQuery{
		Limit: maxExpand, Types: []frontend.MediaType{plexBaseTrack}, Recursive: true,
	}
	if sort := q.Get("sort"); sort != "" {
		key, dir := parseSortParam(sort)
		var valid, lastViewed bool
		iq.Sort, iq.Desc, lastViewed, valid = sortSpecFor(key, dir)
		if !valid || lastViewed || strings.Contains(sort, ",") || !slices.Contains(sec.PluginSorts, string(iq.Sort)) {
			return frontend.ItemQuery{}, frontend.ErrBadPlaylist
		}
	}
	iq = sanitizePluginSectionQuery(sec, q, iq)
	iq.SuppressExpand = true
	iq.NoTotal = true
	iq.PluginScanLimit = maxExpand
	for key, values := range q {
		if key == "type" || key == "sort" || key == "query" || key == "limit" || queryParamIgnored(key) {
			continue
		}
		for _, value := range values {
			field, op, _ := parseFilterKeyOp(key, value)
			if (op != "=" && op != "==") || len(iq.Filter.Facets[field]) == 0 &&
				!(field == "genre" && len(iq.Filter.Genres) > 0 || field == "studio" && iq.Filter.Studio != "") {
				return frontend.ItemQuery{}, frontend.ErrBadPlaylist
			}
		}
	}
	if limit, _ := strconv.Atoi(q.Get("limit")); limit > 0 {
		iq.Limit = min(limit, maxExpand)
	}
	return iq, nil
}

func (s *Server) smartAudioBrowse(ctx context.Context, pr *Request, sec Section, q url.Values) ([]frontend.Title, int, int, error) {
	iq, err := smartAudioQuery(sec, q)
	if err != nil {
		return nil, 0, 0, err
	}
	markHomePluginRead(ctx)
	page, err := s.exp().LibraryItems(ctx, pr.User, plexDevice(pr), sec.IDString(), iq)
	if err != nil {
		return nil, 0, 0, err
	}
	titles := slices.DeleteFunc(slices.Clone(page.Items), func(t frontend.Title) bool {
		return kindTypeForBase(plexBaseArtist, string(t.Type), &t) != plexBaseTrack
	})
	offset, size := pr.pageOf(iq.Limit)
	start := min(offset, len(titles))
	return titles[start:min(start+size, len(titles))], len(titles), offset, nil
}

func (s *Server) pluginPlaylistTitle(ctx context.Context, pr *Request, key frontend.RatingKey) (*frontend.Title, error) {
	t, err := s.kindItem(frontend.WithNoContainerExpand(ctx), pr, key)
	if err != nil {
		return nil, err
	}
	if t == nil || s.plexTypeFor(t) != "playlist" || !s.canSeePluginTitle(pr.User, t) {
		return nil, frontend.ErrNotFound
	}
	return t, nil
}

func (s *Server) pluginPlaylistTracks(ctx context.Context, pr *Request, key frontend.RatingKey) ([]frontend.Title, error) {
	// Plugin membership is bounded by the plugin contract (500 entries).
	page, err := s.exp().Children(ctx, pr.User, plexDevice(pr), key, frontend.ItemQuery{
		Limit: maxExpand, SuppressExpand: true,
	})
	if err != nil {
		return nil, err
	}
	// The member query preserves the list's positions. Check the member keys
	// through the title policy as well, since a list can name hidden items.
	allowed := s.playlistTrackKeys(ctx, titleKeys(page.Items))
	return slices.DeleteFunc(slices.Clone(page.Items), func(t frontend.Title) bool {
		return !allowed[t.Key]
	}), nil
}

func (s *Server) writePluginPlaylistItems(w http.ResponseWriter, r *http.Request, parent *frontend.Title) {
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	if status := playlistQueryStatus(q, true); status != 0 {
		writeHTTPError(w, status)
		return
	}
	tracks, err := s.pluginPlaylistTracks(ctx, pr, parent.Key)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if q.Has("type") && q.Get("type") != "-1" && q.Get("type") != strconv.Itoa(typeNumTrack) {
		tracks = nil
	}
	total := len(tracks)
	lo, hi := pr.window(total)
	rows := s.renderTitles(ctx, pr, tracks[lo:hi], renderOpts{media: true, storedOnly: true})
	for i := range rows {
		pos := tracks[lo+i].ItemIndex
		rows[i].PlaylistItemID = int64(pos)
		rows[i].Index = model.Ptr(pos)
		rows[i].ParentIndex = nil
	}
	mc := &model.MediaContainer{
		RatingKey: parent.Key.String(), Key: "/playlists/" + parent.Key.String() + "/items",
		Title1: parent.Title, PlaylistType: "audio", Smart: model.Ptr(false),
		LeafCount: model.Ptr(total), Metadata: rows, Size: len(rows), Composite: s.thumbURL(parent),
	}
	for _, t := range tracks {
		mc.Duration += t.Runtime.Milliseconds() / 1000
	}
	if q.Get("includeMeta") == "1" {
		mc.Meta = playlistItemsMeta(r, nil)
	}
	if pr.HasPage {
		mc.SetPage(lo, total)
	}
	s.write(w, r, mc)
}
