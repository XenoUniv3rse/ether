package silo

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func (i *instance) collectionSection(r *http.Request, v *frontend.Viewer, credential string, row frontend.Row, views []frontend.View) map[string]any {
	var cards []any
	var keys []frontend.Title
	for _, e := range row.Entries {
		if e.Kind == frontend.RowEntryCollection && e.Collection != nil {
			keys = append(keys, frontend.Title{Key: e.Collection.Key, Type: "boxset"})
		}
	}
	if len(keys) == 0 {
		return nil
	}
	art := i.sectionArtwork(r.WithContext(frontend.WithCollectionCardArtwork(r.Context())), v, credential, keys)
	for _, entry := range row.Entries {
		c := entry.Collection
		if entry.Kind != frontend.RowEntryCollection || c == nil {
			continue
		}
		view := collectionViewFor(c.Type, views)
		if view == "" {
			continue
		}
		libraryID, err := i.wireID(r.Context(), "library", view)
		if err != nil {
			continue
		}
		card := map[string]any{
			"id": ids.RatingKeyGUID(c.Key), "source": "library", "title": c.Title, "item_count": c.ChildCount,
			"library_id": libraryID, "poster_is_collage": false,
		}
		i.paintCatalogArtwork(r, art[c.Key], card)
		if row.Layout.Present.Card == "landscape" && card["backdrop_url"] != nil {
			card["poster_url"] = card["backdrop_url"]
			delete(card, "poster_thumbhash")
		}
		if card["poster_url"] == nil {
			card["poster_url"] = ""
		}
		cards = append(cards, card)
	}
	if len(cards) == 0 {
		return nil
	}
	return map[string]any{
		"id": row.ID, "title": row.Title, "section_type": "collection_posters", "customized": false,
		"featured": false, "is_custom": false, "item_limit": len(cards), "total_count": len(cards),
		"items": []any{}, "collections": cards,
	}
}

func collectionViewFor(typ frontend.MediaType, views []frontend.View) string {
	for _, view := range views {
		if !view.Subset && view.PluginID == "" && (view.Type == typ || typ == "" && (view.Type == "movie" || view.Type == "show")) {
			return view.ID
		}
	}
	return ""
}

// A shared, unscoped collection may contain both types. Its card links
// through the library of a visible artwork sample, which can differ from the
// first member returned by detail.
func collectionVideoView(id string, section frontend.MediaType, views []frontend.View) bool {
	for _, view := range views {
		if view.ID == id && !view.Subset && view.PluginID == "" &&
			(view.Type == frontend.TypeMovie || view.Type == frontend.TypeShow) && (section == "" || view.Type == section) {
			return true
		}
	}
	return false
}

func (i *instance) homeSectionItems(w http.ResponseWriter, r *http.Request) {
	v, token, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	rows, err := i.snapshotHome(r, v, 50)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	for _, row := range rows {
		if row.ID != r.PathValue("id") {
			continue
		}
		views, err := i.host.Experience.Views(r.Context(), v)
		if err == nil {
			if section := i.collectionSection(r, v, token, row, views); section != nil {
				reply(w, 200, section)
				return
			}
		}
		break
	}
	notFound(w)
}
