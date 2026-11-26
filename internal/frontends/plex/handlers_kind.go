package plex

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// sectionKindList serves /library/sections/{id}/{sub} for catalogue-backed
// custom libraries and plugin libraries through Experience.LibraryItems.
func (s *Server) sectionKindList(w http.ResponseWriter, r *http.Request, sec Section, sub string) {
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	_, allTitle := sectionTypeTitle(sec)
	mc := s.sectionContainer(sec)
	mc.Title2 = allTitle
	mc.ViewGroup = kindViewGroup(sec)
	mc.ViewMode = viewModeList

	switch sub {
	case "all", "allLeaves", "unwatched", "newest", "recentlyAdded", "recentlyViewedShows", "search", "autocomplete":
	default:
		// Facets, collections and prefs stay with their existing handlers; only
		// browse and section-scoped search listings dispatch here.
		mc.SetPage(0, 0)
		s.write(w, r, mc)
		return
	}

	if sub == "search" || sub == "autocomplete" {
		queryStr := q.Get("query")
		if queryStr == "" {
			queryStr = q.Get("title")
		}
		if queryStr == "" {
			queryStr = q.Get("title.query")
		}
		mc.Title2 = "Search for '" + queryStr + "'"
		trimmed := strings.TrimSpace(queryStr)
		if trimmed == "" {
			mc.SetPage(0, 0)
			s.write(w, r, mc)
			return
		}
		limit := 0
		if n, _ := strconv.Atoi(q.Get("limit")); n > 0 {
			limit = n
		}
		offset, size := pr.pageOf(limit)
		iq := frontend.ItemQuery{
			Offset: offset,
			Limit:  max(size, 1),
			Filter: frontend.Filter{Query: trimmed},
		}
		page, err := s.exp().LibraryItems(ctx, pr.User, plexDevice(pr), sec.IDString(), iq)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		items := page.Items
		if size == 0 {
			items = nil
		}
		total := page.Total
		if limit > 0 && total > limit {
			total = limit
		}
		mc.Metadata = s.renderKindSectionItems(ctx, pr, sec, sec.Type, sub, q, items)
		mc.Size = len(mc.Metadata)
		mc.SetPage(offset, total)
		s.write(w, r, mc)
		return
	}

	isPlugin := sec.PluginID != ""
	t := sec.Type
	var pluginRequestedType frontend.MediaType
	if isPlugin {
		if tp := typeFromParam(q.Get("type")); tp != "" {
			if tp == "collection" {
				s.sectionCollections(w, r, sec, "collections")
				return
			}
			t = tp
			pluginRequestedType = tp
			mc.ViewGroup = string(tp)
			switch string(tp) {
			case plexBaseArtist:
				mc.Title2 = "All Artists"
			case plexBaseAlbum:
				mc.Title2 = "All Albums"
			case plexBaseTrack:
				mc.Title2 = "All Tracks"
			case plexBasePhoto:
				mc.Title2 = "All Photos"
			case string(frontend.TypeSeason):
				mc.Title2 = "All Seasons"
			case string(frontend.TypeEpisode):
				mc.Title2 = "All Episodes"
			}
		} else if sub == "allLeaves" {
			switch string(sec.Type) {
			case plexBaseArtist:
				t = frontend.MediaType(plexBaseTrack)
				pluginRequestedType = t
				mc.ViewGroup = plexBaseTrack
				mc.Title2 = "All Tracks"
			case string(frontend.TypeShow):
				t = frontend.TypeEpisode
				pluginRequestedType = t
				mc.ViewGroup = string(frontend.TypeEpisode)
				mc.Title2 = "All Episodes"
			}
		}
	} else {
		if sub == "unwatched" && q.Has("type") {
			if _, err := strconv.ParseInt(q.Get("type"), 10, 32); err != nil {
				writeHTTPError(w, http.StatusInternalServerError)
				return
			}
		}
		if tp := typeFromParam(q.Get("type")); tp != "" {
			t = tp
		} else if sub == "allLeaves" && sec.Type == frontend.TypeShow {
			t = frontend.TypeEpisode
		}
		if sub == "unwatched" && q.Has("type") && typeFromParam(q.Get("type")) != sec.Type {
			mc.Title2 = "Unplayed"
			mc.ViewGroup = string(frontend.TypeMovie)
			if sec.Type == frontend.TypeShow {
				mc.ViewGroup = string(frontend.TypeEpisode)
			}
			mc.SetPage(0, 0)
			s.write(w, r, mc)
			return
		}
		mc.ViewGroup = string(t)
		if t == "collection" {
			s.sectionCollections(w, r, sec, "collections")
			return
		}
		switch {
		case t == sec.Type:
		case sec.Type == frontend.TypeShow && t == frontend.TypeSeason:
			mc.Title2 = "All Seasons"
		case sec.Type == frontend.TypeShow && t == frontend.TypeEpisode:
			mc.Title2 = "All Episodes"
		default:
			mc.SetPage(0, 0)
			s.write(w, r, mc)
			return
		}
	}

	br := s.parseBrowse(ctx, sec, t, pr.User, q, r.URL.RawQuery)
	switch sub {
	case "unwatched":
		br.q.Sort, br.q.Desc = frontend.SortTitle, false
		br.q.SecondarySorts = nil
		br.sortLastViewed = false
		want := true
		br.unwatched = &want
		mc.Title2 = "Unplayed"
	case "newest":
		br.q.Sort, br.q.Desc = frontend.SortReleased, true
		br.q.SecondarySorts = nil
		br.sortLastViewed = false
		mc.Title2 = "Recently Released"
	case "recentlyViewedShows":
		br.sortLastViewed = true
		br.lastViewedDesc = true
		br.lastViewedAfter = 1
	case "recentlyAdded":
		br.q.Sort, br.q.Desc = frontend.SortAdded, true
		br.q.SecondarySorts = nil
		br.sortLastViewed = false
		br.q.Filter.KnownAddedDate = true
		mc.Title2 = "Recently Added"
	}

	if !isPlugin {
		if pr.User != nil && s.playback() != nil {
			for _, session := range s.playback().Sessions() {
				if session != nil && session.UserID == pr.User.ID {
					br.q.Filter.AvailabilityKeepKeys = append(br.q.Filter.AvailabilityKeepKeys, session.Key)
				}
			}
		}
		if pr.User != nil && t == frontend.TypeEpisode {
			policy := frontend.Filter{MaxContentRating: pr.User.MaxContentRating, SaferPlexRating: s.needPlexRatingFilter(pr.User)}
			if blocks, ok := s.policy().(frontend.BrowseBlocksPolicy); ok {
				policy.ExcludeGenres, policy.ExcludeKeywords = blocks.BrowseBlocks(pr.User)
			}
			if share, ok := s.policy().(frontend.BrowseShareFilterPolicy); ok {
				policy, _ = s.exp().MergeShareFilter(policy, share.BrowseShareFilter(pr.User, frontend.TypeEpisode))
			}
			br.q.Filter.ChildVisibility = &policy
		}
		if br.invalidSort {
			s.writeErr(w, r, fmt.Errorf("plex: unknown sort"))
			return
		}
		if br.none {
			offset, _ := pr.pageOf(br.limit)
			mc.SetPage(offset, 0)
			if q.Get("includeMeta") == "1" {
				mc.Meta = s.meta(sec, t, q.Get("sort"))
			}
			s.write(w, r, mc)
			return
		}
	}

	query, offset, size := s.browseItemQuery(pr, br)
	if v := strings.TrimSpace(q.Get("query")); v != "" {
		query.Filter.Query = v
	}
	if isPlugin {
		if sub == "allLeaves" {
			query.Recursive = true
		}
		if pluginRequestedType != "" {
			query.Recursive = true
			query.Types = []frontend.MediaType{pluginRequestedType}
		}
		query = sanitizePluginSectionQuery(sec, q, query)
	} else if sec.Type == frontend.TypeShow && (t == frontend.TypeSeason || t == frontend.TypeEpisode) {
		query.Recursive = true
		query.Types = []frontend.MediaType{t}
	}

	page, err := s.exp().LibraryItems(ctx, pr.User, plexDevice(pr), sec.IDString(), query)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	items := page.Items
	if size == 0 {
		items = nil
	}
	total := page.Total
	if br.limit > 0 && total > br.limit {
		total = br.limit
	}
	mc.Metadata = s.renderKindSectionItems(ctx, pr, sec, t, sub, q, items)
	if !isPlugin && len(mc.Metadata) < len(items) && offset+len(items) >= total {
		total = max(offset+len(mc.Metadata), 0)
	}
	mc.Size = len(mc.Metadata)
	mc.SetPage(offset, total)
	if q.Get("includeMeta") == "1" {
		mc.Meta = s.meta(sec, t, q.Get("sort"))
	}
	s.write(w, r, mc)
}

// browseItemQuery translates a parsed Plex browseRequest into a neutral
// frontend.ItemQuery, preserving sort direction, every filter parseBrowse
// populates, watch-state predicates, limit, and paging.
func (s *Server) browseItemQuery(pr *Request, br browseRequest) (iq frontend.ItemQuery, offset, size int) {
	offset, size = pr.pageOf(br.limit)
	limit := size
	if limit == 0 {
		// Keep zero-size container probes on the indexed path; callers discard
		// the returned item while preserving the total.
		limit = 1
	}
	iq = frontend.ItemQuery{
		Offset: offset,
		Limit:  limit,
		Sort:   br.q.Sort,
		Desc:   br.q.Desc,
		Filter: br.q.Filter,
	}
	// Experience.LibraryItems resolves library and child types itself.
	iq.Filter.Type = ""
	if br.sortLastViewed {
		iq.Sort = frontend.SortPlayed
		iq.Desc = br.lastViewedDesc
	}
	if br.unwatched != nil {
		watched := !*br.unwatched
		iq.Watched = &watched
	}
	if br.inProgress != nil || br.lastViewedAfter > 0 || br.lastViewedBefore > 0 {
		var userID int64
		if pr != nil && pr.User != nil && s.state() != nil {
			userID = pr.User.ID
		}
		state := frontend.FilterGroup{
			UserID:           userID,
			InProgress:       br.inProgress,
			LastViewedAfter:  br.lastViewedAfter,
			LastViewedBefore: br.lastViewedBefore,
		}
		if iq.Filter.Group == nil {
			iq.Filter.Group = &state
		} else {
			iq.Filter.Group = &frontend.FilterGroup{
				Operator: "and",
				Children: []frontend.FilterGroup{*iq.Filter.Group, state},
			}
		}
	}
	return iq, offset, size
}

// sanitizePluginSectionQuery strips any sort or filter a plugin library did
// not declare so undeclared Plex parameters are ignored rather than failing or
// emptying the listing.
func sanitizePluginSectionQuery(sec Section, q url.Values, iq frontend.ItemQuery) frontend.ItemQuery {
	out := frontend.ItemQuery{
		Offset:    iq.Offset,
		Limit:     iq.Limit,
		Level:     iq.Level,
		Types:     append([]frontend.MediaType(nil), iq.Types...),
		Recursive: iq.Recursive,
	}
	if rawSort := strings.TrimSpace(q.Get("sort")); rawSort != "" {
		first, _, _ := strings.Cut(rawSort, ",")
		key, dir := parseSortTerm(strings.TrimSpace(first))
		if key == "index" {
			iq.Sort = frontend.SortIndex
			iq.Desc = dir == "desc"
		}
	}
	if iq.Sort != "" && slices.Contains(sec.PluginSorts, string(iq.Sort)) {
		out.Sort = iq.Sort
		out.Desc = iq.Desc
	}
	if v := strings.TrimSpace(q.Get("query")); v != "" {
		out.Filter.Query = v
	}
	if iq.Filter.Person != 0 && frontend.IsPluginPersonID(iq.Filter.Person) {
		out.Filter.Person = iq.Filter.Person
	}
	if len(sec.PluginFacets) == 0 {
		return out
	}
	declared := make(map[string]frontend.ViewFacet, len(sec.PluginFacets))
	for _, f := range sec.PluginFacets {
		declared[f.ID] = f
	}
	hasAny := func(keys ...string) bool {
		for _, k := range keys {
			if _, ok := declared[k]; ok {
				return true
			}
		}
		return false
	}
	var facets map[string][]string
	addFacet := func(id string, vals ...string) {
		for _, v := range vals {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if facets == nil {
				facets = make(map[string][]string)
			}
			facets[id] = append(facets[id], v)
		}
	}
	rawKeys := make([]string, 0, len(q))
	for k := range q {
		rawKeys = append(rawKeys, k)
	}
	sort.Strings(rawKeys)
	for _, rawKey := range rawKeys {
		for _, rawVal := range q[rawKey] {
			key, op, val := parseFilterKeyOp(rawKey, rawVal)
			if op != "=" && op != "==" {
				continue
			}
			switch {
			case key == "genre" && hasAny("genre", "genres", "tags"):
				for _, part := range strings.Split(val, ",") {
					if part = strings.TrimSpace(part); part != "" {
						out.Filter.Genres = append(out.Filter.Genres, part)
					}
				}
			case key == "studio" && hasAny("studio", "studios", "publisher", "author", "channel"):
				if v := strings.TrimSpace(val); v != "" && out.Filter.Studio == "" {
					out.Filter.Studio = v
				}
			default:
				vf, ok := declared[key]
				if !ok {
					continue
				}
				if vf.Multi {
					for _, part := range strings.Split(val, ",") {
						addFacet(vf.ID, part)
					}
				} else {
					addFacet(vf.ID, val)
				}
			}
		}
	}
	out.Filter.Facets = facets
	return out
}

func (s *Server) renderKindSectionItems(ctx context.Context, pr *Request, sec Section, t frontend.MediaType, sub string, q url.Values, items []frontend.Title) []model.Metadata {
	if sec.PluginID != "" {
		rendered := s.renderTitles(ctx, pr, items, renderOpts{media: true, inSection: true, storedOnly: true})
		for i := range rendered {
			var title *frontend.Title
			if i < len(items) {
				title = &items[i]
			}
			rendered[i].Type = kindTypeForBase(string(sec.Type), rendered[i].Type, title)
		}
		return rendered
	}
	storedOnly := t == frontend.TypeSeason || t == frontend.TypeEpisode
	sqlAvailability, _ := s.catalog().(frontend.SQLAvailabilityCatalog)
	rendered := s.renderTitles(ctx, pr, items, renderOpts{
		media:             true,
		inSection:         true,
		guids:             q.Get("includeGuids") == "1",
		storedOnly:        storedOnly,
		availabilityInSQL: sqlAvailability != nil && sqlAvailability.BrowseAvailabilityInSQL(),
	})
	if sub == "unwatched" {
		for i := range rendered {
			if rendered[i].Type == "show" || rendered[i].Type == "season" {
				rendered[i].Key += "?unwatched=1"
			}
		}
	}
	return rendered
}

// IDString is the library id as the Experience's View.ID string.
func (sec Section) IDString() string { return strconv.Itoa(sec.ID) }

// kindViewGroup is the Plex viewGroup for a kind section's root listing.
func kindViewGroup(sec Section) string {
	switch string(sec.Type) {
	case plexBaseArtist:
		return "artist"
	case plexBasePhoto:
		return "photo"
	default:
		return string(sec.Type)
	}
}

// plexDevice builds the neutral device identity for an Experience call.
func plexDevice(pr *Request) frontend.Device {
	if pr == nil {
		return frontend.Device{}
	}
	return frontend.Device{ID: pr.ClientID, Client: pr.Product, Product: pr.Product, Version: pr.Version, Platform: pr.Platform, DeviceName: pr.Device, Language: pr.Language}
}

// kindItem resolves one plugin item through the Experience, which authorizes
// the viewer against the item's owning library (spec §6.4). StoredItem is
// preferred so metadata and children lookups do not depend on user-data
// enrichment; test doubles without StoredItem fall back to Item.
func (s *Server) kindItem(ctx context.Context, pr *Request, k frontend.RatingKey) (*frontend.Title, error) {
	var viewer *frontend.Viewer
	if pr != nil {
		viewer = pr.User
	}
	req := frontend.ItemRequest{Viewer: viewer, Device: plexDevice(pr), Key: k}
	type storedItemReader interface {
		StoredItem(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error)
	}
	var (
		detail frontend.ItemDetail
		err    error
	)
	if stored, ok := s.exp().(storedItemReader); ok {
		detail, err = stored.StoredItem(ctx, req)
	} else {
		detail, err = s.exp().Item(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	if detail.Title.LibraryID != 0 {
		sec, ok := s.sectionForLibrary(detail.Title.LibraryID)
		if !ok || !s.canSeeSection(viewer, sec.ID) {
			return nil, frontend.ErrNotFound
		}
	}
	t := detail.Title
	return &t, nil
}

// kindChildren lists one plugin item's children (artist -> album -> track,
// album -> photos) through the Experience.
func (s *Server) kindChildren(ctx context.Context, pr *Request, k frontend.RatingKey) ([]frontend.Title, error) {
	page, err := s.exp().Children(ctx, pr.User, plexDevice(pr), k, frontend.ItemQuery{})
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

// kindChildrenForParent lists one plugin container's children for Plex's
// primary child listing (e.g. albums under an artist, seasons under a show).
// For a branching container it requests the primary child level directly so
// secondary branches (such as video concerts) are neither mixed nor counted
// against the page budget.
func (s *Server) kindChildrenForParent(ctx context.Context, pr *Request, parent *frontend.Title) ([]frontend.Title, error) {
	q := frontend.ItemQuery{}
	if parent != nil && parent.Type == frontend.TypeShow && parent.PrimaryChildLevel != "" {
		q.Level = parent.PrimaryChildLevel
	}
	page, err := s.exp().Children(ctx, pr.User, plexDevice(pr), parent.Key, q)
	if err != nil {
		return nil, err
	}
	base, _ := s.kindSectionBaseForTitle(parent)
	return filterKindChildren(parent, base, page.Items), nil
}

// filterKindChildren restricts a branching container's child list to its
// primary child level (or omits video branches under a music artist when
// PrimaryChildLevel was not populated).
func filterKindChildren(parent *frontend.Title, base string, kids []frontend.Title) []frontend.Title {
	if parent == nil || parent.Type != frontend.TypeShow || len(kids) == 0 {
		return kids
	}
	if parent.PrimaryChildLevel != "" {
		filtered := make([]frontend.Title, 0, len(kids))
		for _, k := range kids {
			if base == plexBaseArtist && k.MediaClass == frontend.MediaClassVideo {
				continue
			}
			if k.KindLevel == "" || k.KindLevel == parent.PrimaryChildLevel {
				filtered = append(filtered, k)
			}
		}
		return filtered
	}
	if base == plexBaseArtist {
		// Video children under a music artist are surfaced via Plex Extras
		// rather than mixed into the artist's album listing.
		filtered := make([]frontend.Title, 0, len(kids))
		for _, k := range kids {
			if k.MediaClass == frontend.MediaClassVideo {
				continue
			}
			filtered = append(filtered, k)
		}
		return filtered
	}
	return kids
}

// plexTypeFor is the Plex wire type of a title: the protocol-native base for a
// plugin item in a music/photo section, the projected type otherwise. It is
// the single mapping used by detail, children and listings.
func (s *Server) plexTypeFor(t *frontend.Title) string {
	if t == nil {
		return ""
	}
	if !t.Key.IsPluginKey() {
		return string(t.Type)
	}
	if base, ok := s.kindSectionBaseForTitle(t); ok {
		return kindTypeForBase(base, string(t.Type), t)
	}
	return string(t.Type)
}

// kindSectionBaseForTitle is the Plex base (movie/show/artist/photo) of the
// library a projected plugin title belongs to, from its LibraryID. The second
// value is false when the title is not a plugin item or its library is not
// Plex-published.
func (s *Server) kindSectionBaseForTitle(t *frontend.Title) (string, bool) {
	if t == nil || !t.Key.IsPluginKey() {
		return "", false
	}
	if t.LibraryID != 0 {
		if sec, ok := s.sectionForLibrary(t.LibraryID); ok {
			if _, isKind := plexBaseForView(frontend.View{Native: string(sec.Type)}); isKind {
				return string(sec.Type), true
			}
		}
	}
	return "", false
}

// kindTypeForBase maps a projected plugin item's Plex type to the native type
// its section base implies (spec §6.4). The projector renders plugin items in
// the video hierarchy; the base recovers the protocol-native music/photo type.
// A KindLevel of artist/album/track wins when present, so a music library whose
// levels are named exactly that is typed from the declaration. The section
// listing, child and detail renderers share this one mapping.
func kindTypeForBase(base, projected string, t *frontend.Title) string {
	switch base {
	case plexBaseArtist:
		if t != nil && t.MediaClass == frontend.MediaClassVideo {
			// A video level inside a music library maps to a Plex clip
			// (listed on the artist as Extras and playable as video).
			return "clip"
		}
		if t != nil && t.OrderedList {
			return "playlist"
		}
		if t != nil && t.SelfChild {
			// A flat playable album's own playable incarnation.
			return plexBaseTrack
		}
		if t != nil && t.KindLevel != "" {
			switch t.KindLevel {
			case "artist", "album", "track":
				return t.KindLevel
			}
		}
		switch projected {
		case string(frontend.TypeShow):
			return plexBaseArtist
		case string(frontend.TypeSeason):
			return plexBaseAlbum
		case string(frontend.TypeEpisode):
			return plexBaseTrack
		case string(frontend.TypeMovie):
			// A flat single-level music kind lists its items as albums,
			// never as artists.
			return plexBaseAlbum
		}
		return plexBaseArtist
	case plexBasePhoto:
		if t != nil && t.MediaClass == frontend.MediaClassVideo {
			return "clip"
		}
		return plexBasePhoto
	default:
		return projected
	}
}

// writeKindChildren renders one plugin item's children (artist -> album ->
// track, album -> photos) with the protocol-native Plex types its section base
// implies. It bypasses the catalogue/episode machinery handleChildren runs for
// TMDB titles, which does not know plugin keys.
func (s *Server) writeKindChildren(w http.ResponseWriter, r *http.Request, pr *Request, parent *frontend.Title, kids []frontend.Title) {
	ctx := r.Context()
	mc := s.titleContainer(ctx, parent)
	if parent.LibraryID != 0 {
		if sec, ok := s.sectionForLibrary(parent.LibraryID); ok {
			mc.LibrarySectionID = model.Ptr(sec.ID)
			mc.LibrarySectionTitle = sec.Title
			mc.LibrarySectionUUID = sec.UUID
		}
	}
	mc.Key = parent.Key.String()
	mc.Nocache = model.Ptr(true)
	mc.Thumb = s.thumbURL(parent)
	mc.Art = s.artURL(parent)
	base, _ := s.kindSectionBaseForTitle(parent)
	kids = filterKindChildren(parent, base, kids)
	mc.ViewGroup = kindViewGroupForBase(base, parent.Type)
	mc.ViewMode = viewModeList
	lo, hi := pr.window(len(kids))
	page := kids[lo:hi]
	rendered := s.renderTitles(ctx, pr, page, renderOpts{media: true, inSection: true, storedOnly: true})
	for i := range rendered {
		rendered[i].Type = kindTypeForBase(base, rendered[i].Type, &page[i])
		if parent.OrderedList && page[i].ItemIndex > 0 {
			rendered[i].Index = model.Ptr(page[i].ItemIndex)
			rendered[i].ParentIndex = nil
		}
	}
	mc.Metadata = rendered
	mc.Size = len(mc.Metadata)
	if pr.HasPage {
		mc.SetPage(lo, len(kids))
	}
	s.write(w, r, mc)
}

// pluginVideoExtras returns video-media children of a non-video plugin
// container (e.g. concerts under a music artist) rendered as Plex clip Extras.
func (s *Server) pluginVideoExtras(ctx context.Context, pr *Request, parent *frontend.Title) []model.Metadata {
	if parent == nil || !parent.Key.IsPluginKey() {
		return nil
	}
	base, ok := s.kindSectionBaseForTitle(parent)
	if !ok || base != plexBaseArtist || parent.Type != frontend.TypeShow {
		return nil
	}
	page, err := s.exp().Children(ctx, pr.User, plexDevice(pr), parent.Key, frontend.ItemQuery{Limit: 200})
	if err != nil || len(page.Items) == 0 {
		return nil
	}
	var videoKids []frontend.Title
	for _, k := range page.Items {
		if k.MediaClass == frontend.MediaClassVideo {
			videoKids = append(videoKids, k)
		}
	}
	if len(videoKids) == 0 {
		return nil
	}
	rendered := s.renderTitles(ctx, pr, videoKids, renderOpts{media: true, inSection: true, storedOnly: true})
	parentKeyStr := parent.Key.String()
	for i := range rendered {
		child := &videoKids[i]
		rendered[i].Type = "clip"
		subtype := strings.TrimSpace(string(child.MediaRole))
		if subtype == "" {
			subtype = "live"
		}
		rendered[i].Subtype = subtype
		rendered[i].ParentRatingKey = parentKeyStr
		rendered[i].ParentKey = "/library/metadata/" + parentKeyStr
		rendered[i].ParentTitle = parent.Title
		rendered[i].Extra = append(rendered[i].Extra, model.A("extraType", 12))
	}
	return rendered
}

// kindViewGroupForBase is the Plex viewGroup for a plugin section's child
// listing, keyed by the section base.
func kindViewGroupForBase(base string, parentType frontend.MediaType) string {
	switch base {
	case plexBaseArtist:
		if parentType == frontend.TypeShow {
			return "album"
		}
		return "track"
	case plexBasePhoto:
		return "photo"
	}
	switch parentType {
	case frontend.TypeShow:
		return "season"
	case frontend.TypeSeason:
		return "episode"
	}
	return string(parentType)
}
