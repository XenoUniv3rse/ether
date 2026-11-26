package silo

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func (i *instance) catalog(w http.ResponseWriter, r *http.Request) {
	v, token, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	q := r.URL.Query()
	limit := 50
	if x := q.Get("limit"); x != "" {
		limit, err = paging.ParseLimit(x, limit, 100)
		if err != nil {
			http.Error(w, "Bad Request", 400)
			return
		}
	}
	offset := 0
	if x := q.Get("cursor"); x != "" {
		offset, err = paging.ParseOffset(x, limit)
		if err != nil || offset < 0 {
			http.Error(w, "Bad Request", 400)
			return
		}
	}
	if q.Get("library_id") == frontend.CatchUpLibraryID {
		i.catchUpCatalog(w, r, v, token, offset, limit)
		return
	}
	if q.Get("source") == "section" && q.Get("section_id") != "" {
		i.sectionBrowse(w, r, offset, limit)
		return
	}
	query := frontend.ItemQuery{Offset: offset, Limit: limit}
	if personID := q.Get("person_id"); personID != "" {
		if _, peErr := ids.ParseGUID(ids.Person, personID); peErr == nil {
			reader, ok := i.host.Experience.(frontend.PeopleReader)
			if !ok {
				http.Error(w, "Unavailable", 503)
				return
			}
			person, e := reader.Person(r.Context(), v, device(r), personID, frontend.PersonReadOptions{})
			if e != nil || person.TMDB <= 0 {
				if errors.Is(e, frontend.ErrNotFound) || e == nil {
					notFound(w)
					return
				}
				http.Error(w, "Unavailable", 503)
				return
			}
			query.Filter.Person = person.TMDB
		} else {
			facetView := q.Get("library_id")
			if facetView != "" && facetView != nowAvailableLibraryID {
				facetView, err = i.nativeID(r.Context(), "library", facetView)
				if err != nil {
					notFound(w)
					return
				}
			} else {
				facetView = ""
			}
			facet, e := i.host.Experience.ResolveFacet(r.Context(), v, device(r), facetView, "person", personID)
			if e != nil {
				if errors.Is(e, frontend.ErrNotFound) {
					notFound(w)
					return
				}
				http.Error(w, "Bad Request", 400)
				return
			}
			n, e := strconv.ParseInt(facet.Key, 10, 64)
			if e != nil || n <= 0 {
				http.Error(w, "Bad Request", 400)
				return
			}
			query.Filter.Person = n
		}
	}
	var page frontend.Page
	var archive []frontend.CatchUpItem
	exact := true
	more := false
	if collectionID := r.PathValue("collection_id"); collectionID != "" {
		key, keyErr := ids.ParseRatingKeyGUID(collectionID)
		view, viewErr := i.nativeID(r.Context(), "library", r.PathValue("id"))
		if keyErr != nil || viewErr != nil || key.Kind() != frontend.KindSmart {
			notFound(w)
			return
		}
		detail, detailErr := i.host.Experience.Item(frontend.WithNoContainerExpand(r.Context()), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key})
		views, err := i.host.Experience.Views(r.Context(), v)
		_, section := key.CollectionSection()
		if detailErr != nil || err != nil || detail.Collection == nil || detail.Collection.OwnerUserID != 0 ||
			!collectionVideoView(view, section, views) {
			notFound(w)
			return
		}
		page, err = i.host.Experience.Children(r.Context(), v, device(r), key, query)
		if err != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		more = offset+len(page.Items) < page.Total
	} else if term := q.Get("q"); term != "" {
		scopedView := ""
		if rawLib := q.Get("library_id"); rawLib != "" && rawLib != nowAvailableLibraryID {
			view, e := i.nativeID(r.Context(), "library", rawLib)
			if e != nil {
				notFound(w)
				return
			}
			if sr, ok := i.host.Experience.(frontend.LibraryScopeReader); ok {
				if _, scoped, se := sr.LibraryBrowseFilter(r.Context(), v, view, frontend.Filter{}); se == nil && scoped {
					scopedView = view
				}
			}
		}
		if scopedView != "" {
			query.Filter.Query = term
			page, err = i.host.Experience.LibraryItems(r.Context(), v, device(r), scopedView, query)
			if err != nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			more = offset+len(page.Items) < page.Total
		} else {
			// The global search window starts with matching Catch-up shows.
			// Offset the regular search by the length of that bounded prefix.
			archiveCount := 0
			if lib, ok := i.host.LiveTV.(frontend.CatchUpLibrary); ok && q.Get("library_id") == "" && query.Filter.Person == 0 && strings.TrimSpace(term) != "" {
				matches, e := lib.CatchUpItems(r.Context(), v, frontend.CatchUpQuery{Search: term, Limit: 100})
				if e != nil {
					http.Error(w, "Unavailable", 503)
					return
				}
				archiveCount = len(matches.Items)
				archive = paging.Slice(matches.Items, offset, limit)
				query.Offset = max(0, offset-archiveCount)
				query.Limit = max(1, limit-len(archive))
			}
			result, e := i.host.Experience.Search(r.Context(), v, device(r), term, query)
			if e != nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			page = result.Page
			more = result.Continuation == frontend.ContinuationMore
			room := limit - len(archive)
			if len(page.Items) > room {
				page.Items = page.Items[:room]
				more = true
			}
			page.Total += archiveCount
			exact = result.TotalExact
			more = more || offset+len(archive) < archiveCount
		}
	} else if q.Get("library_id") == nowAvailableLibraryID {
		titles, _ := i.nowAvailableTitles(r, v)
		total := len(titles)
		start := min(offset, total)
		end := total
		if limit > 0 {
			end = min(start+limit, total)
		}
		page = frontend.Page{Items: titles[start:end], Total: total}
		more = end < total
	} else {
		view := q.Get("library_id")
		if view != "" {
			view, err = i.nativeID(r.Context(), "library", view)
			if err != nil {
				notFound(w)
				return
			}
		}
		page, err = i.host.Experience.LibraryItems(r.Context(), v, device(r), view, query)
		if err != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		more = offset+len(page.Items) < page.Total
	}
	keys := make([]frontend.RatingKey, 0, len(page.Items))
	for _, t := range page.Items {
		keys = append(keys, t.Key)
	}
	state, err := i.host.Experience.UserData(r.Context(), v, keys)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	cards := i.catalogCards(r, v, token, page.Items)
	editions, batched := i.pageEditions(r, v, keys)
	items := make([]any, 0, len(archive)+len(page.Items))
	for _, item := range archive {
		items = append(items, i.catchUpCard(r, token, item))
	}
	for _, t := range page.Items {
		item := titleDTO(t, state[t.Key])
		if granted, ok := cards[t.Key]; ok {
			i.paintCatalogArtwork(r, granted, item)
		} else {
			i.artwork(r, v, token, t.Key, item)
		}
		if batched {
			if summary := bestSiloOverlay(editions[t.Key]); len(summary) > 0 {
				item["overlay_summary"] = summary
			}
		} else {
			i.applyEditionOverlay(r, v, t.Key, item)
		}
		items = append(items, item)
	}
	var next any
	if more && len(items) > 0 {
		next = strconv.Itoa(offset + len(items))
	}
	reply(w, 200, map[string]any{"items": items, "total": page.Total, "total_exact": exact, "window_cursor": "", "page": map[string]any{"has_more": next != nil, "next_cursor": next}})
}
func (i *instance) childItems(w http.ResponseWriter, r *http.Request) {
	v, token, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	wire := r.PathValue("id")
	key, err := ids.ParseRatingKeyGUID(wire)
	if err != nil && !frontend.IsCatchUpGUID(wire) {
		http.NotFound(w, r)
		return
	}
	episode := r.PathValue("season") != "" || r.PathValue("all") != ""
	limit, offset := 0, 0
	if episode {
		limit = 50
		if x := r.URL.Query().Get("limit"); x != "" {
			n, e := paging.ParseLimit(x, 50, 100)
			if e != nil {
				http.Error(w, "Bad Request", 400)
				return
			}
			limit = n
		}
		n, e := decodeEpisodeCursor(r.URL.Query().Get("cursor"))
		if e != nil {
			http.Error(w, "Bad Request", 400)
			return
		}
		offset = n
	}
	query := frontend.ItemQuery{Limit: limit, Offset: offset}
	if r.PathValue("season") != "" {
		n, e := strconv.Atoi(r.PathValue("season"))
		if e != nil || n < 0 {
			http.Error(w, "Bad Request", 400)
			return
		}
		query.Season = &n
		query.Recursive = true
	}
	if episode {
		query.Types = []frontend.MediaType{"episode"}
	} else {
		query.Types = []frontend.MediaType{"season"}
	}
	if parent, kind := frontend.CatchUpGUIDGroup(wire); kind != "" {
		if query.Season != nil && *query.Season != 1 {
			reply(w, 200, map[string]any{"items": []any{}, "page": map[string]any{"has_more": false}})
			return
		}
		items, page, ok := i.catchUpPage(w, r, v, token, frontend.CatchUpQuery{Parent: parent, Recursive: episode, Offset: offset, Limit: limit})
		if ok {
			reply(w, 200, map[string]any{"items": items, "page": catchUpNext(page, offset, encodeEpisodeCursor)})
		}
		return
	}
	if frontend.IsCatchUpGUID(wire) {
		notFound(w)
		return
	}
	page, err := i.host.Experience.Children(r.Context(), v, device(r), key, query)
	if err != nil {
		if r.PathValue("season") != "" {
			reply(w, 200, map[string]any{"items": []any{}, "page": map[string]any{"has_more": false}})
			return
		}
		http.Error(w, "Unavailable", 503)
		return
	}
	if raw := r.PathValue("season"); raw != "" {
		n, _ := strconv.Atoi(raw)
		kept := make([]frontend.Title, 0, len(page.Items))
		for _, t := range page.Items {
			if t.Type == frontend.MediaType("episode") && t.Season == n {
				kept = append(kept, t)
			}
		}
		if len(kept) != len(page.Items) {
			page.Items = kept
			if len(kept) == 0 {
				page.Total = 0
			}
		}
	}
	keys := make([]frontend.RatingKey, 0, len(page.Items))
	for _, t := range page.Items {
		keys = append(keys, t.Key)
	}
	states, err := i.host.Experience.UserData(r.Context(), v, keys)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	overlays, batched := i.editionOverlays(r, v, keys)
	artwork := i.sectionArtwork(r, v, token, page.Items)
	items := make([]any, 0, len(page.Items))
	for _, t := range page.Items {
		item := titleDTO(t, states[t.Key])
		item["season_number"] = t.Season
		item["episode_number"] = t.Episode
		item["is_specials"] = t.Season == 0
		if t.Type == "season" {
			item["episode_count"] = t.ChildCount
		} else {
			item["episode_count"] = t.LeafCount
		}
		i.paintCardArtwork(r, artwork[t.Key], item)
		if t.Type == "episode" {
			// The series page labels its next-up episode "Resume S4·E2" and
			// draws watched/progress state from each episode's user_data.
			st := states[t.Key]
			// The app decodes this as the watch rollup: the three counts are
			// required, and a missing one fails the whole episode list.
			played := st.ViewCount > 0
			inProgress := st.ViewOffset > 0 && !played
			watched, unplayed, progressing := 0, 1, 0
			if played {
				watched, unplayed = 1, 0
			}
			if inProgress {
				progressing = 1
			}
			data := map[string]any{"played": played, "is_in_progress": inProgress, "watched_count": watched, "unplayed_count": unplayed, "in_progress_count": progressing}
			if st.ViewOffset > 0 {
				data["position_seconds"] = st.ViewOffset.Seconds()
			}
			switch {
			case st.Duration > 0:
				data["duration_seconds"] = st.Duration.Seconds()
			case t.Runtime > 0:
				data["duration_seconds"] = t.Runtime.Seconds()
			}
			item["user_data"] = data
		}
		if t.Type == "episode" && (t.StillPath != "" || t.PosterPath != "" || t.BackdropPath != "") {
			item["poster_is_episode_still"] = t.StillPath != "" && t.PosterPath == t.StillPath
			item["backdrop_is_episode_still"] = t.StillPath != "" && t.BackdropPath == t.StillPath
		}
		if t.Type == "episode" {
			if still, ok := item["still_url"].(string); ok && still != "" {
				if _, hasPoster := item["poster_url"]; !hasPoster {
					item["poster_url"] = still
					item["poster_is_episode_still"] = true
				}
			}
		}
		if summary := overlays[t.Key]; len(summary) > 0 {
			item["overlay_summary"] = summary
		} else if !batched {
			i.applyEditionOverlay(r, v, t.Key, item)
		}
		items = append(items, item)
	}
	more := limit > 0 && page.Total > offset+limit
	info := map[string]any{"has_more": more}
	if more {
		info["next_cursor"] = encodeEpisodeCursor(offset + limit)
	}
	reply(w, 200, map[string]any{"items": items, "page": info})
}

func decodeEpisodeCursor(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		if _, e := paging.ParseOffset(strconv.Itoa(n), 100); e != nil {
			return 0, errors.New("cursor")
		}
		return n, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(s)
	}
	if err != nil {
		return 0, err
	}
	var c struct {
		Offset int `json:"offset"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Offset < 0 || c.Offset > int(^uint(0)>>1)-100 {
		return 0, errors.New("cursor")
	}
	return c.Offset, nil
}

func encodeEpisodeCursor(offset int) string {
	raw, err := json.Marshal(struct {
		Offset int `json:"offset"`
	}{Offset: offset})
	if err != nil {
		return ""
	}
	return base64.RawStdEncoding.EncodeToString(raw)
}
func (i *instance) catalogRoutes() error {
	for _, e := range []struct {
		path string
		h    http.HandlerFunc
	}{{"/api/v2/catalog", i.catalog}, {"/api/v2/library/{id}/collections/{collection_id}/items", i.catalog}, {"/api/v2/catalog/series/{id}/seasons", i.childItems}, {"/api/v2/catalog/series/{id}/seasons/{season}/episodes", i.childItems}} {
		if err := i.router.Handle("GET", e.path, e.h); err != nil {
			return err
		}
	}
	return nil
}

// catalogCards signs a catalogue page's movie and series artwork in one
// stateless pass, as the home rows do, instead of loading each title's
// detail and writing a grant per image. An episode, or a title the signer
// could not list, keeps the per-title path.
func (i *instance) catalogCards(r *http.Request, v *frontend.Viewer, token string, titles []frontend.Title) map[frontend.RatingKey][]frontend.GrantedImage {
	if _, ok := i.host.ImageGrants.(frontend.ImagePageSigner); !ok {
		return nil
	}
	cards := make([]frontend.Title, 0, len(titles))
	for _, t := range titles {
		if t.Type == "movie" || t.Type == "show" {
			cards = append(cards, t)
		}
	}
	return i.sectionArtwork(r, v, token, cards)
}

// paintCatalogArtwork writes the fields artwork writes for a list card: the
// first signed image of each kind and its thumbhash.
func (i *instance) paintCatalogArtwork(r *http.Request, granted []frontend.GrantedImage, out map[string]any) {
	prefix := i.pathPrefix(r)
	fields := map[string]string{frontend.ImagePoster: "poster_url", frontend.ImageBackdrop: "backdrop_url", frontend.ImageLogo: "logo_url"}
	hashes := map[string]string{frontend.ImagePoster: "poster_thumbhash", frontend.ImageBackdrop: "backdrop_thumbhash"}
	for _, grant := range granted {
		field := fields[grant.Image.Kind]
		if field == "" || grant.Secret == "" || grant.Image.Path == "" {
			continue
		}
		if _, exists := out[field]; exists {
			continue
		}
		out[field] = siloImageURL(r, prefix+"/api/v2/images/"+grant.Secret, grant.Image.Kind)
		if hash := hashes[grant.Image.Kind]; hash != "" && grant.Image.ThumbHash != "" {
			out[hash] = grant.Image.ThumbHash
		}
	}
}

// pageEditions reads a page's editions in one batch when the host can.
func (i *instance) pageEditions(r *http.Request, v *frontend.Viewer, keys []frontend.RatingKey) (map[frontend.RatingKey][]frontend.Version, bool) {
	batch, ok := i.host.Editions.(frontend.EditionBatch)
	if !ok || len(keys) == 0 {
		return nil, false
	}
	versions, err := batch.EditionsMany(r.Context(), v, device(r), keys)
	if err != nil {
		return nil, false
	}
	return versions, true
}

// sectionBrowse serves a section's "Explore all" page: the items of that one
// Home or library section, paged. The catalog used to ignore section_id and
// answer with the whole catalogue (53,000 titles behind "On Now").
func (i *instance) sectionBrowse(w http.ResponseWriter, r *http.Request, offset, limit int) {
	q := r.URL.Query()
	sectionID := q.Get("section_id")
	path := "/api/v2/home/sections"
	library := q.Get("library_id")
	if library != "" && q.Get("scope") != "home" {
		path = "/api/v2/library/" + library + "/sections"
	}
	params := url.Values{"item_limit": {"100"}}
	for _, key := range []string{"image_size", "backdrop_width", "poster_width", "logo_width", "still_width"} {
		if value := q.Get(key); value != "" {
			params.Set(key, value)
		}
	}
	sub := r.Clone(r.Context())
	u := *r.URL
	u.Path, u.RawPath, u.RawQuery = path, "", params.Encode()
	sub.URL = &u
	sub.RequestURI = u.RequestURI()
	if path != "/api/v2/home/sections" {
		sub.SetPathValue("library", library)
	}
	capture := &captureWriter{header: http.Header{}, status: 200}
	i.sections(capture, sub)
	if capture.status != 200 {
		http.Error(w, http.StatusText(capture.status), capture.status)
		return
	}
	var doc struct {
		Sections []struct {
			ID    string `json:"id"`
			Items []any  `json:"items"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(capture.body.Bytes(), &doc); err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	var items []any
	found := false
	for _, section := range doc.Sections {
		if section.ID == sectionID {
			items, found = section.Items, true
			break
		}
	}
	if !found {
		notFound(w)
		return
	}
	start := min(offset, len(items))
	end := len(items)
	if limit > 0 {
		end = min(start+limit, len(items))
	}
	var next any
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	reply(w, 200, map[string]any{"items": items[start:end], "total": len(items), "total_exact": true, "window_cursor": "", "page": map[string]any{"has_more": next != nil, "next_cursor": next}})
}

type captureWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (c *captureWriter) Header() http.Header         { return c.header }
func (c *captureWriter) Write(b []byte) (int, error) { return c.body.Write(b) }
func (c *captureWriter) WriteHeader(status int)      { c.status = status }
