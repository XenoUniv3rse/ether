package vela

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) openCollectionCard(w http.ResponseWriter, r *http.Request) {
	viewer, token, err := i.viewer(r)
	if err != nil {
		fail(w, err, r)
		return
	}
	source, ok := i.host.Experience.(frontend.CollectionCardReader)
	k, err := key(r)
	if !ok || err != nil || k.Kind() != frontend.KindSmart {
		fail(w, frontend.ErrNotFound, r)
		return
	}
	scope, err := i.livePageScope(r, viewer, token)
	if err != nil {
		fail(w, err, r)
		return
	}
	size := limit(r)
	cursor, err := i.readLiveCursor(r, scope, hashJSON([]any{"collection", k, size, i.bootstrapFreshness()}))
	if err != nil {
		fail(w, err, r)
		return
	}
	after := int64(-1)
	if cursor.Offset > 0 {
		after = cursor.MemberAfter
	}
	page, err := source.OpenCollectionCard(r.Context(), viewer, k, after, cursor.Offset, size)
	if err != nil {
		fail(w, err, r)
		return
	}
	revision := hashJSON(page.Revision)
	if cursor.Offset > 0 && cursor.Snapshot != revision {
		fail(w, frontend.ErrRowViewStale, r)
		return
	}
	artRequest := r.WithContext(frontend.WithCollectionCardArtwork(r.Context()))
	collections, err := i.collectionsWire(artRequest, viewer, token, []frontend.Collection{page.Collection})
	if err != nil {
		fail(w, err, r)
		return
	}
	collection := collections[0]
	collection["kind"] = "collection"
	titles, err := i.titles(r, viewer, token, page.Titles)
	if err != nil {
		fail(w, err, r)
		return
	}
	items := make([]object, 0, len(titles))
	for _, title := range titles {
		items = append(items, object{"id": title["id"], "kind": "title", "title": title, "receiptToken": ""})
	}
	out := object{"collection": collection, "items": items, "continuation": "exhausted"}
	if page.More {
		cursor.MemberAfter = page.Next
		next, err := i.nextLiveCursor(cursor, cursor.Offset+page.Consumed, revision)
		if err != nil {
			fail(w, err, r)
			return
		}
		out["nextCursor"], out["continuation"] = next, "more"
	}
	varyLanguage(w)
	cache(w, r, out)
}
