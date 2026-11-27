package silo

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
)

func (i *instance) mutation(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		notFound(w)
		return
	}
	if _, err = i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key}); err != nil {
		notFound(w)
		return
	}
	switch r.PathValue("kind") {
	case "not_interested":
		err = i.host.Experience.SetNotInterested(r.Context(), v, key, r.Method == "PUT")
	case "watchlist":
		err = i.host.Experience.SetWatchlist(r.Context(), v, key, r.Method == "PUT")
	default:
		if r.Method == "PUT" {
			err = i.host.Experience.SetFavorite(r.Context(), v, key, true)
		} else if r.Method == "DELETE" {
			err = i.host.Experience.SetFavorite(r.Context(), v, key, false)
		}
	case "watched":
		if r.Method == "POST" {
			err = i.host.Experience.SetPlayed(r.Context(), v, key)
		} else {
			err = i.host.Experience.SetUnplayed(r.Context(), v, key)
		}
	}
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	i.dropViewerSections(v.ID)
	w.WriteHeader(http.StatusNoContent)
}
func (i *instance) mutationRoutes() error {
	for _, e := range []struct{ method, path, kind string }{{"PUT", "/api/v2/not-interested/{id}", "not_interested"}, {"DELETE", "/api/v2/not-interested/{id}", "not_interested"}, {"PUT", "/api/v2/watchlist/{id}", "watchlist"}, {"DELETE", "/api/v2/watchlist/{id}", "watchlist"}, {"PUT", "/api/v2/favorites/{id}", "favorite"}, {"DELETE", "/api/v2/favorites/{id}", "favorite"}, {"POST", "/api/v2/watched/{id}", "watched"}, {"DELETE", "/api/v2/watched/{id}", "watched"}} {
		kind := e.kind
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { r.SetPathValue("kind", kind); i.mutation(w, r) })
		if err := i.router.Handle(e.method, e.path, handler); err != nil {
			return err
		}
	}
	for _, e := range []struct{ path, kind string }{{"/api/v2/favorites/{id}", "favorite"}, {"/api/v2/watchlist/{id}", "watchlist"}} {
		kind := e.kind
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("kind", kind)
			i.membershipItem(w, r)
		})
		if err := i.router.Handle("GET", e.path, handler); err != nil {
			return err
		}
	}
	for _, e := range []struct{ path, kind string }{{"/api/v2/favorites", "favorite"}, {"/api/v2/watchlist", "watchlist"}} {
		kind := e.kind
		if err := i.router.Handle("GET", e.path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { i.personalList(w, r, kind) })); err != nil {
			return err
		}
	}
	return nil
}

// personalList serves the profile's whole favorites or watchlist, paged by
// cursor: the app's Favorites and Watchlist screens read it. It was missing,
// so both screens failed.
func (i *instance) personalList(w http.ResponseWriter, r *http.Request, kind string) {
	v, credential, err := i.viewer(r)
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
		if limit, err = paging.ParseLimit(x, limit, 200); err != nil {
			http.Error(w, "Bad Request", 400)
			return
		}
	}
	offset := 0
	if x := q.Get("cursor"); x != "" {
		if offset, err = paging.ParseOffset(x, limit); err != nil || offset < 0 {
			http.Error(w, "Bad Request", 400)
			return
		}
	}
	var titles []frontend.Title
	more := false
	if kind == "favorite" {
		yes := true
		page, e := i.host.Experience.LibraryItems(r.Context(), v, device(r), "", frontend.ItemQuery{Offset: offset, Limit: limit, Favorite: &yes})
		if e != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		titles = page.Items
		more = offset+len(page.Items) < page.Total
	} else {
		rows, e := i.snapshotRows(r, frontend.RowRequest{Viewer: v, Device: device(r), Placement: frontend.PlacementHome, Purpose: frontend.RowWatchlist, Limit: 500})
		if e != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		var all []frontend.Title
		for _, row := range rows {
			if row.Purpose == frontend.RowWatchlist {
				all = append(all, rowTitles(row)...)
			}
		}
		start := min(offset, len(all))
		end := min(start+limit, len(all))
		titles, more = all[start:end], end < len(all)
	}
	keys := make([]frontend.RatingKey, 0, len(titles))
	for _, t := range titles {
		keys = append(keys, t.Key)
	}
	states, err := i.host.Experience.UserData(r.Context(), v, keys)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	artwork := i.sectionArtwork(r, v, credential, titles)
	items := make([]any, 0, len(titles))
	for _, t := range titles {
		item := titleDTO(t, states[t.Key])
		i.paintCardArtwork(r, artwork[t.Key], item)
		items = append(items, item)
	}
	var next any
	if more {
		next = strconv.Itoa(offset + len(titles))
	}
	reply(w, 200, map[string]any{"items": items, "page": map[string]any{"has_more": next != nil, "next_cursor": next}})
}

// membershipItem serves one favorite or watchlist entry. added_at is the stored
// membership time, not the title's date added. Absence and inaccessibility are
// both 404; a present row without a real timestamp is unavailable.
func (i *instance) membershipItem(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	reader, ok := i.host.Experience.(frontend.MembershipReader)
	if !ok {
		http.Error(w, "Unavailable", 503)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		notFound(w)
		return
	}
	var got frontend.Membership
	if r.PathValue("kind") == "watchlist" {
		got, err = reader.WatchlistItem(r.Context(), v, device(r), key)
	} else {
		got, err = reader.FavoriteItem(r.Context(), v, device(r), key)
	}
	if errors.Is(err, frontend.ErrNotFound) || (err == nil && !got.Present) {
		notFound(w)
		return
	}
	if err != nil || got.AddedAt == nil || got.AddedAt.IsZero() {
		http.Error(w, "Membership timestamp unavailable", 503)
		return
	}
	// The app reads membership as {item_id, added_at}; without item_id the
	// read failed and detail pages never showed an item as favourited or
	// on the watchlist.
	id := ids.RatingKeyGUID(key)
	reply(w, 200, map[string]any{"item_id": id, "content_id": id, "added_at": got.AddedAt.UTC().Format("2006-01-02T15:04:05.000Z")})
}
