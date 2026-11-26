package vela

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reading"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reqmemo"
)

func hashJSON(v any) string { b, _ := json.Marshal(v); return hash(string(b)) }
func limit(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 {
		n = 50
	}
	if n > 100 {
		n = 100
	}
	return n
}
func noCursor(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Query().Get("cursor") != "" {
		problem(w, 409, "conflict", "cursor_expired")
		return false
	}
	return true
}
func key(r *http.Request) (frontend.RatingKey, error) {
	return ids.ParseRatingKeyGUID(r.PathValue("id"))
}
func titleWire(t frontend.Title) object {
	return titleWireForRegions(t, []string{"US"})
}
func titleWireForRegions(t frontend.Title, regions []string) object {
	kind := "movie"
	switch t.Key.Kind() {
	case frontend.KindShow:
		kind = "show"
	case frontend.KindSeason:
		kind = "season"
	case frontend.KindEpisode:
		kind = "episode"
	default:
		switch t.Type {
		case frontend.TypeShow:
			kind = "show"
		case frontend.TypeSeason:
			kind = "season"
		case frontend.TypeEpisode:
			kind = "episode"
		}
	}
	genres := t.Genres
	if genres == nil {
		genres = []string{}
	}
	out := object{"id": ids.RatingKeyGUID(t.Key), "kind": kind, "name": t.Title, "summary": t.Summary, "genres": genres, "artwork": []object{}}
	if t.Year > 0 {
		out["year"] = t.Year
	}
	if date := cmp.Or(t.EpisodeAirDate, t.Released); !date.IsZero() {
		out["airDate"] = date.UTC().Format("2006-01-02")
	}
	if t.DateDisplay == "date" {
		out["dateDisplay"] = "date"
	}
	if t.Runtime > 0 {
		out["durationSeconds"] = t.Runtime.Seconds()
	}
	if label, country := contentrating.Display(t.ContentRating, t.ContentRatings, regions...); label != "" {
		out["rating"] = label
		if country != "" {
			out["ratingCountry"] = country
		}
	}
	if !nonVideoMedia(t) {
		showKey := videoShowKey(t)
		if showKey != 0 {
			out["showId"] = ids.RatingKeyGUID(showKey)
			if kind == "season" || kind == "episode" {
				out["seasonNumber"] = t.Season
			}
			if kind == "episode" {
				out["episodeNumber"] = t.Episode
			}
		}
		if name := videoShowName(t); name != "" && (showKey != 0 || kind == "season" || kind == "episode") {
			out["showName"] = name
		}
	}
	if media := titleMediaWire(t); media != nil {
		out["media"] = media
	}
	return out
}
func watchWire(s frontend.WatchStatus) object {
	out := object{
		"positionSeconds": s.ViewOffset.Seconds(),
		"played":          s.ViewCount > 0,
		"favorite":        s.IsFavorite,
		"watchlisted":     s.InWatchlist,
	}
	if !s.LastViewedAt.IsZero() {
		out["lastViewedAt"] = s.LastViewedAt.UTC().Format(time.RFC3339)
	}
	if s.Reading != nil {
		reading := object{
			"locator":  s.Reading.Locator,
			"fraction": s.Reading.Fraction,
		}
		if s.Reading.Label != "" {
			reading["label"] = s.Reading.Label
		}
		if !s.Reading.UpdatedAt.IsZero() {
			reading["updatedAt"] = s.Reading.UpdatedAt.UTC().Format(time.RFC3339)
		}
		out["reading"] = reading
	}
	return out
}

type homeTitlesKey struct{}
type showNamesKey struct{}

func (i *instance) titles(r *http.Request, v *frontend.Viewer, token string, ts []frontend.Title) ([]object, error) {
	if len(ts) == 0 {
		return []object{}, nil
	}
	if cached, ok := r.Context().Value(homeTitlesKey{}).(map[frontend.RatingKey]object); ok {
		out := make([]object, 0, len(ts))
		complete := true
		for _, title := range ts {
			wire, found := cached[title.Key]
			if !found {
				complete = false
				break
			}
			out = append(out, wire)
		}
		if complete {
			return out, nil
		}
	}
	keys := make([]frontend.RatingKey, 0, len(ts))
	for _, t := range ts {
		keys = append(keys, t.Key)
	}
	states, err := i.host.Experience.UserData(r.Context(), v, keys)
	if err != nil {
		return nil, err
	}
	return i.titlesWithUserData(r, v, token, ts, states)
}

// Detail already owns the watch/favorite/watchlist state. Do not load it again
// only to overwrite it with the authorized detail's state below.
func (i *instance) titleArtworkBatch(r *http.Request, v *frontend.Viewer, token string, ts []frontend.Title) (map[frontend.RatingKey][]object, error) {
	art := map[frontend.RatingKey][]object{}
	if len(ts) == 0 || i.host.ImageGrants == nil {
		return art, nil
	}
	kinds := []string{frontend.ImagePoster, frontend.ImageBackdrop, frontend.ImageStill}
	hasLandscape := false
	for _, title := range ts {
		if title.LandscapeCard && title.Key.IsPluginKey() {
			hasLandscape = true
			break
		}
	}
	if hasLandscape {
		kinds = append(kinds, frontend.ImageThumb)
	}
	if signer, ok := i.host.ImageGrants.(frontend.ImagePageSigner); ok {
		// Include series in the same authorization, artwork and signing batch.
		// Their poster keeps its show scope, including overlays and policy checks.
		page := append([]frontend.Title(nil), ts...)
		seen := make(map[frontend.RatingKey]bool, len(ts))
		for _, title := range ts {
			seen[title.Key] = true
		}
		for _, title := range ts {
			if !title.LandscapeCard {
				if show := episodePosterShow(title); show != 0 && !seen[show] {
					seen[show] = true
					page = append(page, frontend.Title{Key: show})
				}
			}
		}
		images, e := signer.SignPage(r.Context(), frontend.ImagePageRequest{Credential: token, Device: requestDevice(r), Titles: page, Kinds: kinds})
		if e != nil {
			return nil, e
		}
		byKey := make(map[frontend.RatingKey][]frontend.GrantedImage, len(images))
		for _, item := range images {
			byKey[item.Item] = item.Images
		}
		seen = make(map[frontend.RatingKey]bool, len(ts))
		for _, title := range ts {
			if seen[title.Key] {
				continue
			}
			seen[title.Key] = true
			selected := titleCardArtwork(title, byKey[title.Key])
			if !title.LandscapeCard {
				if show := episodePosterShow(title); show != 0 && !hasPortrait(selected) {
					for _, image := range byKey[show] {
						if image.Image.Kind == frontend.ImagePoster && image.Image.Path != "" && image.Secret != "" {
							// A poster-only landscape fallback is replaced by the
							// actual series poster; stills and backdrops keep priority.
							if len(selected) > 0 {
								selected = selected[:1]
								if selected[0].Image.Kind == frontend.ImagePoster {
									selected = nil
								}
							}
							selected = append(selected, image)
							break
						}
					}
				}
			}
			for _, g := range selected {
				im := g.Image
				art[title.Key] = append(art[title.Key], object{"id": hash(im.Path), "kind": im.Kind, "tag": im.Tag, "sizes": []object{imageSize(im, g.Secret)}, "blurHash": im.BlurHash, "thumbHash": im.ThumbHash})
			}
		}
	} else if artReader, hasArt := i.host.Experience.(frontend.ItemArtwork); hasArt {
		wanted := make(map[string]bool, len(kinds))
		for _, k := range kinds {
			wanted[k] = true
		}
		seen := make(map[frontend.RatingKey]bool, len(ts))
		for _, title := range ts {
			if seen[title.Key] {
				continue
			}
			seen[title.Key] = true
			imgs, ok, err := artReader.ItemArtwork(frontend.WithPrimaryArtworkOnly(frontend.WithArtworkKinds(r.Context(), wanted)), v, requestDevice(r), title.Key)
			if err != nil || !ok {
				continue
			}
			var granted []frontend.GrantedImage
			for _, im := range imgs {
				if im.Path == "" || !slices.Contains(kinds, im.Kind) {
					continue
				}
				secret, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{
					Credential: token,
					Device:     requestDevice(r),
					Item:       title.Key,
					Image:      im,
				})
				if err != nil || secret == "" {
					continue
				}
				granted = append(granted, frontend.GrantedImage{Image: im, Secret: secret})
			}
			for _, g := range titleCardArtwork(title, granted) {
				im := g.Image
				art[title.Key] = append(art[title.Key], object{"id": hash(im.Path), "kind": im.Kind, "tag": im.Tag, "sizes": []object{imageSize(im, g.Secret)}, "blurHash": im.BlurHash, "thumbHash": im.ThumbHash})
			}
		}
	}
	return art, nil
}

func (i *instance) titlesWithUserData(r *http.Request, v *frontend.Viewer, token string, ts []frontend.Title, states map[frontend.RatingKey]frontend.WatchStatus) ([]object, error) {
	art, err := i.titleArtworkBatch(r, v, token, ts)
	if err != nil {
		return nil, err
	}
	ratings := map[frontend.RatingKey][]frontend.RatingScore{}
	if source, ok := i.host.Experience.(frontend.CachedRatings); ok {
		ratings = source.PageCachedRatings(r.Context(), v, ts)
	}
	showNames := i.resolveShowNames(r, v, ts)
	regions := i.preferredRatingRegions(r)
	loud := i.pageLoudness(r, ts)
	out := []object{}
	for _, t := range ts {
		if showKey := videoShowKey(t); showKey != 0 && videoShowName(t) == "" {
			if name := showNames[showKey]; name != "" {
				t.ShowTitle = name
			}
		}
		item := titleWireForRegions(t, regions)
		if l, ok := loud[t.Key]; ok {
			if media, ok := item["media"].(object); ok {
				media["loudness"] = loudnessWire(l)
			}
		}
		scores := append([]frontend.RatingScore(nil), ratings[t.Key]...)
		if t.VoteCount > 0 && t.Rating >= 0 && t.Rating <= 10 && !math.IsNaN(t.Rating) {
			scores = append(scores, frontend.RatingScore{Source: "tmdb", Kind: "audience", Value: t.Rating, Scale: 10, Votes: int64(t.VoteCount)})
		}
		if len(scores) > 0 {
			item["ratings"] = scores
		}
		item["watch"] = watchWire(states[t.Key])
		if a := art[t.Key]; a != nil {
			item["artwork"] = a
		}
		out = append(out, item)
	}
	return out, nil
}

// videoShowKey resolves the parent show key for a video season or episode.
// Non-video media classes never emit showId/showName/seasonNumber/episodeNumber.
func videoShowKey(t frontend.Title) frontend.RatingKey {
	if nonVideoMedia(t) {
		return 0
	}
	if t.ShowKey != 0 {
		return t.ShowKey
	}
	if t.Type == frontend.TypeEpisode || t.Type == frontend.TypeSeason {
		for _, p := range t.MediaParents {
			if p.Key != 0 && (p.Role == frontend.MediaRoleShow || p.Role == frontend.MediaRoleSeries) {
				return p.Key
			}
		}
	}
	return 0
}

func videoShowName(t frontend.Title) string {
	if nonVideoMedia(t) {
		return ""
	}
	if name := strings.TrimSpace(t.ShowTitle); name != "" {
		return name
	}
	if t.Type == frontend.TypeEpisode || t.Type == frontend.TypeSeason || t.Key.Kind() == frontend.KindEpisode || t.Key.Kind() == frontend.KindSeason {
		for _, p := range t.MediaParents {
			if p.Role == frontend.MediaRoleShow || p.Role == frontend.MediaRoleSeries {
				if name := strings.TrimSpace(p.Title); name != "" {
					return name
				}
			}
		}
		if t.ShowKey != 0 {
			for _, p := range t.MediaParents {
				if p.Key == t.ShowKey {
					if name := strings.TrimSpace(p.Title); name != "" {
						return name
					}
				}
			}
		}
	}
	return ""
}

// resolveShowNames batch-loads missing parent show names for any video episodes
// or seasons in ts using at most one TitleBatchReader lookup per response.
// Shows already present in ts are reused without a lookup.
func (i *instance) resolveShowNames(r *http.Request, v *frontend.Viewer, ts []frontend.Title) map[frontend.RatingKey]string {
	names, _ := r.Context().Value(showNamesKey{}).(map[frontend.RatingKey]string)
	if names == nil {
		names = make(map[frontend.RatingKey]string)
	}
	// A show card or another episode can already carry the parent name.
	for _, t := range ts {
		if key := videoShowKey(t); key != 0 {
			if name := videoShowName(t); name != "" {
				names[key] = name
			}
		}
		if t.Type == frontend.TypeShow && !nonVideoMedia(t) {
			if name := strings.TrimSpace(t.Title); name != "" {
				names[t.Key] = name
			}
		}
	}
	var need []frontend.RatingKey
	for _, t := range ts {
		key := videoShowKey(t)
		if _, known := names[key]; key != 0 && !known {
			need = append(need, key)
			names[key] = ""
		}
	}
	if len(need) == 0 || v == nil {
		return names
	}
	if batch, ok := i.host.Experience.(frontend.TitleBatchReader); ok {
		if shows, err := batch.BatchTitles(r.Context(), v, need); err == nil {
			for _, show := range shows {
				names[show.Key] = strings.TrimSpace(show.Title)
			}
		}
	}
	return names
}

// episodePosterShow requests a show fallback only when the episode has no
// portrait of its own. Packed catalogue keys also cover cards without ShowKey.
func episodePosterShow(title frontend.Title) frontend.RatingKey {
	// Episodes often carry an inherited or unmeasured PosterPath that
	// titleArtwork does not select as a portrait, so the show is always
	// fetched; it is only used when the card ends up without a portrait.
	if title.Key.Kind() != frontend.KindEpisode && title.Type != frontend.TypeEpisode {
		return 0
	}
	if nonVideoMedia(title) {
		// Music tracks, audiobook parts and book items carry their own
		// (square or cover) art; a grandparent artist photo is never theirs.
		return 0
	}
	return title.ShowKey
}

func hasPortrait(images []frontend.GrantedImage) bool {
	for i, image := range images {
		// A poster in first place is a landscape fallback, not a portrait.
		if i > 0 && image.Image.Kind == frontend.ImagePoster {
			return true
		}
	}
	return false
}

// Episode cards keep their still (or landscape fallback) first, followed by
// an available portrait. Selection happens after batch signing, so every URL
// retains its item scope.
func titleArtwork(key frontend.RatingKey, images []frontend.GrantedImage) []frontend.GrantedImage {
	if key.Kind() != frontend.KindEpisode {
		return images
	}
	return selectEpisodeArtwork(images)
}

func titleCardArtwork(title frontend.Title, images []frontend.GrantedImage) []frontend.GrantedImage {
	if title.LandscapeCard {
		return selectLandscapeCardArtwork(images)
	}
	if title.Key.Kind() != frontend.KindEpisode && title.Type != frontend.TypeEpisode {
		return images
	}
	if nonVideoMedia(title) {
		return images
	}
	return selectEpisodeArtwork(images)
}

// nonVideoMedia reports a plugin title of a non-video media class (music,
// audiobook, book, podcast, photo), whose artwork is not episode artwork.
func nonVideoMedia(title frontend.Title) bool {
	return title.MediaClass != "" && title.MediaClass != frontend.MediaClassVideo
}

func selectLandscapeCardArtwork(images []frontend.GrantedImage) []frontend.GrantedImage {
	if len(images) == 0 {
		return nil
	}
	out := make([]frontend.GrantedImage, 0, len(images))
	for _, kind := range []string{frontend.ImageThumb, frontend.ImageStill, frontend.ImageBackdrop} {
		for _, image := range images {
			if image.Image.Kind == kind && image.Image.Path != "" && image.Secret != "" {
				out = append(out, image)
			}
		}
	}
	for _, image := range images {
		if image.Image.Kind != frontend.ImageThumb && image.Image.Kind != frontend.ImageStill && image.Image.Kind != frontend.ImageBackdrop && image.Image.Path != "" && image.Secret != "" {
			out = append(out, image)
		}
	}
	return out
}

func selectEpisodeArtwork(images []frontend.GrantedImage) []frontend.GrantedImage {
	for _, kind := range []string{frontend.ImageStill, frontend.ImageBackdrop, frontend.ImagePoster} {
		for _, image := range images {
			if image.Image.Kind == kind && image.Image.Path != "" && image.Secret != "" {
				out := []frontend.GrantedImage{image}
				if kind != frontend.ImagePoster {
					for _, portrait := range images {
						if portrait.Image.Kind == frontend.ImagePoster && portrait.Image.Width > 0 && portrait.Image.Height > portrait.Image.Width && portrait.Image.Path != "" && portrait.Secret != "" {
							out = append(out, portrait)
							break
						}
					}
				}
				return out
			}
		}
	}
	return nil
}

func (i *instance) home(r *http.Request, v *frontend.Viewer, token string, sources ...map[string]heroSource) ([]object, bool, error) {
	rows, more, err := i.homeVisit(r, v, token, false, sources...)
	if retryBootstrap(err) && r.Context().Err() == nil && r.PathValue("id") == "" && r.URL.Query().Get("cursor") == "" {
		// A first page can start a fresh visit. Continuations retain their
		// original authorization and snapshot contract.
		ctx := frontend.WithFreshRequestMemo(reqmemo.With(r.Context()))
		return i.homeVisit(r.WithContext(ctx), v, token, true, sources...)
	}
	return rows, more, err
}

func (i *instance) homeVisit(r *http.Request, v *frontend.Viewer, token string, refresh bool, sources ...map[string]heroSource) ([]object, bool, error) {
	placement := frontend.Placement(r.URL.Query().Get("placement"))
	if placement == "" {
		placement = frontend.PlacementHome
	}
	if placement != frontend.PlacementHome && placement != frontend.PlacementDetail && placement != frontend.PlacementLibrary {
		return nil, false, frontend.ErrNotFound
	}
	if rawLib, exists := r.URL.Query()["libraryId"]; exists {
		if len(rawLib) != 1 || (rawLib[0] != "" && placement != frontend.PlacementLibrary) {
			return nil, false, frontend.ErrInvalidPreferences
		}
	}
	libraryID := r.URL.Query().Get("libraryId")
	var matched *frontend.View
	if placement == frontend.PlacementLibrary && libraryID != "" {
		visibleViews, err := i.host.Experience.Views(r.Context(), v)
		if err != nil {
			return nil, false, err
		}
		for idx := range visibleViews {
			if visibleViews[idx].ID == libraryID {
				matched = &visibleViews[idx]
				break
			}
		}
		if r.PathValue("id") == "" {
			if matched == nil {
				return nil, false, frontend.ErrNotFound
			}
			if matched.PluginID != "" || matched.KindKey != "" {
				return []object{}, false, nil
			}
		}
	}
	var item frontend.RatingKey
	if s := r.URL.Query().Get("titleId"); s != "" {
		var err error
		item, err = ids.ParseRatingKeyGUID(s)
		if err != nil {
			return nil, false, err
		}
	}
	rows := []frontend.Row{}
	type rowLease struct {
		id      string
		expires time.Time
	}
	var rowLeases map[string]rowLease
	var excludedHeroes []string
	outerMore := false
	views, hasViews := i.host.Experience.(frontend.RowViews)
	useRowViews := hasViews && !(placement == frontend.PlacementDetail && item.IsPluginKey())
	if id := r.PathValue("id"); id != "" && useRowViews {
		offset := 0
		if token := r.URL.Query().Get("cursor"); token != "" {
			c, e := i.readRowToken(r, v, token)
			if e != nil || c.Row != id {
				return nil, false, frontend.ErrRowViewStale
			}
			offset = c.Offset
			excludedHeroes = c.Exclude
		}
		row, e := i.rowPage(r, v, id, offset, rowItemLimit(r))
		if errors.Is(e, frontend.ErrNotFound) {
			// A newer visit replaced or dropped this snapshot: the same
			// answer as any superseded id, so the client re-reads bootstrap.
			e = frontend.ErrRowViewStale
		}
		if e != nil {
			return nil, false, e
		}
		i.rowMu.Lock()
		meta, found := i.rowMeta[id]
		i.rowMu.Unlock()
		if !found {
			return nil, false, frontend.ErrRowViewStale
		}
		row.Key = meta.Key
		row.Title = meta.Title
		row.Purpose = meta.Purpose
		row.Kind = meta.Kind
		row.Layout = meta.Layout
		row.Style = meta.Layout.Style
		row.PreviewOnFocus = meta.Layout.PreviewOnFocus
		row.RankedTop10 = row.RankedTop10 || meta.Layout.RankedTop10
		rows = append(rows, row)
	} else if useRowViews {
		vv, e := views.ListRowViews(r.Context(), frontend.RowViewRequest{Viewer: v, Device: requestDevice(r), Protocol: "vela", Placement: placement, ViewID: libraryID, Item: item, Refresh: refresh})
		if e != nil {
			return nil, false, e
		}
		outerMore = len(vv) > limit(r)
		vv = vv[:min(len(vv), limit(r))]
		var pages []frontend.RowViewPage
		if batch, ok := i.host.Experience.(frontend.RowViewPages); ok && len(vv) > 1 {
			reqs := make([]frontend.RowViewPageRequest, len(vv))
			for n, view := range vv {
				reqs[n] = frontend.RowViewPageRequest{Viewer: v, Device: requestDevice(r), Protocol: "vela", ID: view.ID, Limit: rowItemLimit(r)}
			}
			pages, e = batch.ResolveRowViews(r.Context(), reqs)
			if retryBootstrap(e) {
				return nil, false, e
			}
			if e != nil || len(pages) != len(vv) {
				if r.Context().Err() != nil {
					return nil, false, r.Context().Err()
				}
				// Recover the healthy rows when a bounded batch fails.
				pages = nil
			}
		}
		rowLeases = make(map[string]rowLease, len(vv))
		for n, view := range vv {
			expiry := view.ExpiresAt
			var row frontend.Row
			var e error
			if pages != nil {
				row = rowFromPage(view.ID, pages[n])
				expiry = earlierRowExpiry(expiry, pages[n].ExpiresAt)
			} else {
				page, err := views.ResolveRowView(r.Context(), frontend.RowViewPageRequest{Viewer: v, Device: requestDevice(r), Protocol: "vela", ID: view.ID, Limit: rowItemLimit(r)})
				e = err
				row = rowFromPage(view.ID, page)
				expiry = earlierRowExpiry(expiry, page.ExpiresAt)
			}
			if e != nil {
				if r.Context().Err() != nil {
					return nil, false, r.Context().Err()
				}
				if retryBootstrap(e) || (!refresh && errors.Is(e, frontend.ErrNotFound)) {
					return nil, false, frontend.ErrRowViewStale
				}
				logDroppedHomeRow(r, cmp.Or(view.Layout.Present.RowID, view.Key, view.ID), "resolve", e)
				continue
			}
			i.rowMu.Lock()
			if len(i.rowMeta) >= 2000 {
				i.rowMeta = map[string]frontend.RowView{}
			}
			i.rowMeta[view.ID] = view
			i.rowMu.Unlock()
			row.Key = view.Key
			row.Title = view.Title
			row.Purpose = view.Purpose
			row.Kind = view.Kind
			row.Layout = view.Layout
			row.Style = view.Layout.Style
			row.PreviewOnFocus = view.Layout.PreviewOnFocus
			row.RankedTop10 = row.RankedTop10 || view.Layout.RankedTop10
			wireID := row.ID
			if row.Layout.Present.Surface != nil {
				wireID = surfaceRowWire(row)["id"].(string)
			}
			rowLeases[wireID] = rowLease{id: view.ID, expires: expiry}
			rows = append(rows, row)
		}
	} else {
		var e error
		rows, e = i.host.Experience.Rows(r.Context(), frontend.RowRequest{Viewer: v, Device: requestDevice(r), Placement: placement, ViewID: libraryID, Item: item, Limit: rowItemLimit(r)})
		if e != nil {
			return nil, false, e
		}
	}
	if r.PathValue("id") == "" && len(rows) > limit(r) {
		outerMore = true
		rows = rows[:limit(r)]
	}
	if r.PathValue("id") == "" || !useRowViews {
		assignRowKeys(rows)
		if useRowViews {
			// /rows/{id} answers with the key this listing gave the row.
			i.rowMu.Lock()
			for _, row := range rows {
				if meta, ok := i.rowMeta[row.ID]; ok {
					meta.Key = row.Key
					i.rowMeta[row.ID] = meta
				}
			}
			i.rowMu.Unlock()
		}
	}
	if matched != nil {
		markLandscapeRows(rows, *matched)
	}
	out, err := i.rowsToWire(r, v, token, rows, matched, excludedHeroes, useRowViews, rowItemLimit(r), sources...)
	if err != nil {
		return nil, false, err
	}
	for _, row := range out {
		id, _ := row["id"].(string)
		if lease, ok := rowLeases[id]; ok {
			noteBootstrapDeadline(r, lease.expires)
			noteBootstrapRow(r, frontend.RowViewPageRequest{Viewer: v, Device: requestDevice(r), Protocol: "vela", ID: lease.id, Limit: rowItemLimit(r)})
			delete(rowLeases, id)
		}
	}
	return out, outerMore, nil
}

func earlierRowExpiry(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

func (i *instance) rowsToWire(r *http.Request, v *frontend.Viewer, token string, rows []frontend.Row, view *frontend.View, excludedHeroes []string, hasViews bool, itemLimit int, sources ...map[string]heroSource) ([]object, error) {
	if _, ok := r.Context().Value(showNamesKey{}).(map[frontend.RatingKey]string); !ok {
		r = r.WithContext(context.WithValue(r.Context(), showNamesKey{}, map[frontend.RatingKey]string{}))
	}
	// Share card state, ratings and artwork across all shelves in this page.
	var pageTitles []frontend.Title
	seenTitles := map[frontend.RatingKey]bool{}
	for _, row := range rows {
		ts := row.Items
		if len(row.Entries) > 0 {
			ts = nil
			for _, entry := range row.Entries {
				if entry.Title != nil {
					ts = append(ts, *entry.Title)
				}
			}
		}
		if row.RankedTop10 {
			ts = ts[:min(len(ts), 10)]
		}
		for _, title := range ts {
			if !seenTitles[title.Key] {
				seenTitles[title.Key] = true
				pageTitles = append(pageTitles, title)
			}
		}
	}
	converted, err := i.titles(r, v, token, pageTitles)
	if err != nil && (retryBootstrap(err) || r.PathValue("id") != "" || r.Context().Err() != nil) {
		return nil, err
	}
	if err == nil {
		cardCache := make(map[frontend.RatingKey]object, len(converted))
		for n, title := range pageTitles {
			cardCache[title.Key] = converted[n]
		}
		r = r.WithContext(context.WithValue(r.Context(), homeTitlesKey{}, cardCache))
	}
	out := []object{}
	var surfaces map[string]bool
	for _, row := range rows {
		if s := row.Layout.Present.Surface; s != nil {
			// A plugin surface row sits at its page position; the client
			// draws it from the plugin's document when it may.
			if surfaces == nil {
				surfaces = i.surfaceGate(r, v)
			}
			if surfaces[s.Surface] {
				out = append(out, surfaceRowWire(row))
			}
			continue
		}
		if len(sources) > 0 {
			sources[0][row.ID] = heroSource{kind: row.Kind, pinned: row.Layout.Always, hero: row.Layout.Present.Layout == "hero", previewDefault: i.previewAvailable(), previewPolicy: row.Layout.PreviewOnFocusPolicy}
		}
		offset := 0
		if old := r.URL.Query().Get("cursor"); old != "" {
			c, _ := i.readRowToken(r, v, old)
			offset = c.Offset
		}
		wire, e := i.rowWire(r, v, token, row, offset, excludedHeroes, hasViews)
		if e == nil {
			e = i.backfillShelf(r, v, token, wire, itemLimit)
		}
		if e != nil {
			if retryBootstrap(e) || r.PathValue("id") != "" || r.Context().Err() != nil {
				return nil, e
			}
			logDroppedHomeRow(r, cmp.Or(row.Key, row.ID), "render", e)
			continue
		}
		libraryDefaults(wire, view)
		out = append(out, wire)
	}
	return out, nil
}

// rowWire retains source entry receipts while converting one authorized page.
func (i *instance) rowWire(r *http.Request, v *frontend.Viewer, token string, row frontend.Row, sourceOffset int, excludedHeroes []string, hasViews bool) (object, error) {
	if row.Layout.Present.Numbered {
		row.RankedTop10 = true
	}
	if row.RankedTop10 {
		row.Entries = row.Entries[:min(len(row.Entries), 10)]
		row.Items = row.Items[:min(len(row.Items), 10)]
		row.More = false
	}
	sourceEntries := len(row.Entries)
	row = tracksEntries(row)
	ts := []frontend.Title{}
	cols := []frontend.Collection{}
	for _, e := range row.Entries {
		if e.Kind == frontend.RowEntryTitle && e.Title != nil {
			ts = append(ts, *e.Title)
		} else if e.Kind == frontend.RowEntryCollection && e.Collection != nil {
			cols = append(cols, *e.Collection)
		}
	}
	if len(row.Entries) == 0 {
		ts = row.Items
	}
	converted, e := i.titles(r, v, token, ts)
	if e != nil {
		return nil, e
	}
	byID := map[string]object{}
	for _, t := range converted {
		byID[t["id"].(string)] = t
	}
	colByID := map[string]object{}
	if len(cols) > 0 {
		convertedCols, err := i.collectionsWire(r.WithContext(frontend.WithCollectionCardArtwork(r.Context())), v, token, cols)
		if err != nil {
			return nil, err
		}
		for _, c := range convertedCols {
			colByID[c["id"].(string)] = c
		}
	}
	entries := []object{}
	for _, e := range row.Entries {
		base := object{"receiptToken": e.Handle}
		switch {
		case e.Title != nil:
			base["id"] = ids.RatingKeyGUID(e.Title.Key)
			base["kind"] = "title"
			base["title"] = byID[base["id"].(string)]
			if e.Badge != "" {
				base["badge"] = e.Badge
			}
		case e.Collection != nil:
			colID := ids.RatingKeyGUID(e.Collection.Key)
			base["id"] = colID
			base["kind"] = "collection"
			base["targetId"] = colID
			base["name"] = e.Collection.Title
			// Only the owner's card count is the collection's size; for
			// other viewers it only proves one visible member, so leave
			// the optional count out rather than show "1 item".
			if v != nil && v.IsOwner && e.Collection.ChildCount > 0 {
				base["itemCount"] = e.Collection.ChildCount
			}
			base["artwork"] = colByID[colID]["artwork"]
		case e.LiveChannel != nil:
			c := e.LiveChannel
			base["id"] = c.ID
			base["kind"] = "channel"
			base["channel"] = object{"id": c.ID, "number": c.Number, "name": c.Name, "kind": "", "favorite": false, "artwork": []object{}}
		default:
			continue
		}
		entries = append(entries, base)
	}
	if len(row.Entries) == 0 {
		for _, t := range converted {
			entries = append(entries, object{"id": t["id"], "kind": "title", "title": t, "receiptToken": ""})
		}
	}

	if len(excludedHeroes) > 0 {
		omitted := map[string]bool{}
		for _, id := range excludedHeroes {
			omitted[id] = true
		}
		filtered := entries[:0]
		for _, entry := range entries {
			if entry["kind"] == "title" {
				if title, ok := entry["title"].(object); ok {
					if omitted[heroFamily(title)] {
						continue
					}
				}
			}
			filtered = append(filtered, entry)
		}
		entries = filtered
	}
	continuation := "exhausted"
	if row.More || row.Kind == "truncated" {
		continuation = "unknown"
	}
	wire := object{"id": row.ID, "name": row.Title, "purpose": row.Purpose, "receiptToken": row.Handle, "items": entries, "continuation": continuation}
	if row.Key != "" {
		wire["key"] = row.Key
	}
	presentRow(wire, row)
	if wire["role"] == "collection" && wire["card"] == nil {
		wire["card"] = "poster"
	}
	if wire["presentation"] == "ranked" {
		wire["continuation"] = "exhausted"
	}
	if row.PreviewOnFocus {
		wire["previewOnFocus"] = true
	}
	if row.More && hasViews {
		offset := sourceOffset + sourceEntries
		wire["nextCursor"] = i.rowToken(r, v, row.ID, offset, excludedHeroes)
		wire["continuation"] = "more"
	}
	return wire, nil
}

func (i *instance) buildBootstrap(r *http.Request, v *frontend.Viewer, t string, avatar object, spoilerFree, refresh, plexHomeSwitchAllowed bool) (object, error) {
	r = r.WithContext(context.WithValue(r.Context(), showNamesKey{}, map[frontend.RatingKey]string{}))
	sources := map[string]heroSource{}
	rows, _, e := i.homeVisit(r, v, t, refresh, sources)
	if e != nil {
		return nil, e
	}
	heroes, shelves := heroesFromRows(rows, sources)
	if len(heroes) < 6 {
		fill, err := i.heroFallback(r, v, t, heroes, sources)
		if err != nil {
			return nil, err
		}
		if len(fill) > 0 {
			heroes, shelves = heroesFromRows(append(rows, fill...), sources)
		}
	}
	viewer, err := i.viewerPolicyWire(r, v, requestDevice(r), avatar)
	if err != nil {
		return nil, err
	}
	viewer["spoilerFree"] = spoilerFree
	appDoc := i.appearanceDocument()
	navigation, err := i.screenNavigation(r, v)
	if err != nil {
		return nil, err
	}
	surfaces, err := i.screenSurfaces(r, v)
	if err != nil {
		return nil, err
	}
	out := object{
		"viewer":        viewer,
		"capabilities":  i.requestCapabilitiesFor(r, v, plexHomeSwitchAllowed),
		"navigation":    navigation,
		"surfaces":      surfaces,
		"defaultSkinId": appDoc.skinID,
		"branding":      appDoc.branding,
		"force":         appDoc.force,
		"packs":         appDoc.packs,
		"documents":     appDoc.documents,
		"appearance":    json.RawMessage(appDoc.payload),
		"rows":          shelves,
	}
	if len(heroes) > 0 {
		ids := make([]string, 0, len(heroes))
		for _, hero := range heroes {
			ids = append(ids, heroFamily(hero["title"].(object)))
		}
		kept := shelves[:0]
		for _, shelf := range shelves {
			if token, ok := shelf["nextCursor"].(string); ok && token != "" {
				id, _ := shelf["id"].(string)
				cursor, err := i.readRowToken(r, v, token)
				if err != nil {
					return nil, err
				}
				cursor.Exclude = ids
				next := i.sealRowCursor(cursor)
				if next == "" {
					logDroppedHomeRow(r, id, "hero cursor", errors.New("cursor encryption failed"))
					continue
				}
				shelf["nextCursor"] = next
				if err := i.backfillShelf(r, v, t, shelf, rowItemLimit(r)); err != nil {
					if r.Context().Err() != nil || retryBootstrap(err) {
						return nil, err
					}
					logDroppedHomeRow(r, id, "hero backfill", err)
					continue
				}
			}
			kept = append(kept, shelf)
		}
		out["rows"] = kept
		out["heroes"] = heroes
	}
	return out, nil
}
func (i *instance) rows(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(i.host.Experience.NoProviderWait(r.Context()))
	v, t, e := i.viewer(r)
	if e != nil {
		fail(w, e, r)
		return
	}
	if r.PathValue("id") == "" && !noCursor(w, r) {
		return
	}
	rows, more, e := i.home(r, v, t)
	if e != nil {
		fail(w, e, r)
		return
	}
	if id := r.PathValue("id"); id != "" {
		for _, row := range rows {
			if row["id"] == id {
				varyLanguage(w)
				cache(w, r, row)
				return
			}
		}
		problem(w, 404, "not-found", "not_found")
		return
	}
	continuation := "exhausted"
	if more {
		continuation = "unknown"
	}
	varyLanguage(w)
	cache(w, r, object{"items": rows, "continuation": continuation})
}
func (i *instance) detail(w http.ResponseWriter, r *http.Request) {
	v, t, e := i.viewer(r)
	if e != nil {
		fail(w, e, r)
		return
	}
	if rawID := r.PathValue("id"); strings.HasPrefix(rawID, "facet:") {
		i.facetSeriesDetail(w, r, v, t, rawID)
		return
	}
	k, e := key(r)
	if e != nil {
		problem(w, 404, "not-found", "not_found")
		return
	}
	ctx, pending := frontend.WithDetailPending(frontend.WithColdDetail(r.Context(), k), k)
	r = r.WithContext(ctx)
	d, e := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: v, Device: requestDevice(r), Key: k})
	if e != nil {
		fail(w, e, r)
		return
	}
	extras := []object{}
	for _, x := range d.Extras {
		availability := "unavailable"
		if i.extraPlaybackAvailable() && (x.Kind == "trailer" || x.Kind == "teaser") {
			availability = "unresolved"
		}
		kind := x.Kind
		if kind == "teaser" {
			kind = "trailer"
		}
		extras = append(extras, object{"id": ids.RatingKeyGUID(x.Key), "parentId": ids.RatingKeyGUID(x.ParentKey), "kind": kind, "name": x.Title, "durationSeconds": x.Duration.Seconds(), "availability": availability})
	}
	if strings.HasSuffix(r.URL.Path, "/extras") {
		cacheDetail(w, r, object{"items": extras}, pending)
		return
	}
	ts, e := i.titlesWithUserData(r, v, t, []frontend.Title{d.Title}, map[frontend.RatingKey]frontend.WatchStatus{d.Title.Key: d.UserData})
	if e != nil {
		fail(w, e, r)
		return
	}
	if theme := i.detailTheme(r, t, d.Title); theme != nil {
		ts[0]["themeMusic"] = theme
	}
	ts[0]["watch"] = watchWire(d.UserData)
	cast, crew, e := i.credits(r, v, t, d)
	if e != nil {
		fail(w, e, r)
		return
	}
	out := object{"title": ts[0], "extras": extras, "chapters": detailChapters(d), "cast": cast, "crew": crew}
	if reading.IsBookTitle(d.Title) {
		out["canPlay"] = false
		canRead := i.host.KindStreams != nil && d.Title.Key.IsPluginKey() && d.Unavailable == nil &&
			d.Title.Type != frontend.TypeShow && d.Title.Type != frontend.TypeSeason
		if d.CanRead != nil {
			canRead = canRead && *d.CanRead
		}
		out["canRead"] = canRead
	} else if d.CanPlay != nil {
		out["canPlay"] = *d.CanPlay
	}
	if summary := mediaSummary(d); summary != nil {
		out["mediaSummary"] = summary
	}
	if err := i.detailChildren(r, v, t, d, out); err != nil {
		fail(w, err, r)
		return
	}
	// If detailChildren hydrated a previously unhydrated plugin container,
	// refresh its media counts (childCount / childCounts) in one O(1) read.
	if d.Title.Key.IsPluginKey() && len(d.Title.ChildCounts) == 0 &&
		(d.Title.Type == frontend.TypeShow || d.Title.Type == frontend.TypeSeason) &&
		(d.Title.ChildCount > 0 || out["seasons"] != nil || out["episodes"] != nil) {
		if refreshed, rErr := i.host.Experience.Item(frontend.WithNoContainerExpand(r.Context()), frontend.ItemRequest{Viewer: v, Device: requestDevice(r), Key: k}); rErr == nil {
			if media := titleMediaWire(refreshed.Title); media != nil {
				ts[0]["media"] = media
			}
		}
	}
	i.warmDetail(r, v, d)
	varyLanguage(w)
	cacheDetail(w, r, out, pending)
}
func (i *instance) children(w http.ResponseWriter, r *http.Request) {
	v, t, e := i.viewer(r)
	if e != nil {
		fail(w, e, r)
		return
	}
	rawID := r.PathValue("id")
	if strings.HasPrefix(rawID, "facet:") {
		i.facetSeriesChildren(w, r, v, t, rawID)
		return
	}
	role := strings.TrimSpace(r.URL.Query().Get("role"))
	level := strings.TrimSpace(r.URL.Query().Get("level"))
	if len(role) > 64 || len(level) > 64 {
		problem(w, 400, "request", "invalid_request")
		return
	}
	lim := limit(r)
	var cur liveCursor
	if i.seal != nil {
		scope, err := i.livePageScope(r, v, t)
		if err != nil {
			fail(w, err, r)
			return
		}
		cur, err = i.readLiveCursor(r, scope, hashJSON([]any{"children", rawID, role, level, lim}))
		if err != nil {
			fail(w, err, r)
			return
		}
	} else if !noCursor(w, r) {
		return
	}
	k, e := key(r)
	if e != nil {
		problem(w, 404, "not-found", "not_found")
		return
	}
	ctx, pending := frontend.WithDetailPending(frontend.WithColdDetail(r.Context(), k), k)
	r = r.WithContext(ctx)
	p, e := i.host.Experience.Children(r.Context(), v, requestDevice(r), k, frontend.ItemQuery{Offset: cur.Offset, Limit: lim + 1, Level: level, Role: role, NoTotal: true})
	if e != nil {
		fail(w, e, r)
		return
	}
	hasMore := len(p.Items) > lim
	items := p.Items
	if hasMore {
		items = items[:lim]
	}
	ts, e := i.titles(r, v, t, items)
	if e != nil {
		fail(w, e, r)
		return
	}
	resp := object{"items": ts, "continuation": "exhausted"}
	if hasMore {
		if i.seal != nil {
			next, err := i.nextLiveCursor(cur, cur.Offset+len(items), "children")
			if err != nil {
				fail(w, err, r)
				return
			}
			resp["continuation"] = "more"
			resp["nextCursor"] = next
		} else {
			resp["continuation"] = "unknown"
		}
	}
	varyLanguage(w)
	cacheDetail(w, r, resp, pending)
}

type searchRequest struct {
	Query, Mode, Cursor string
	Generation, Limit   int
	// Sort orders the results (searchSorts); "relevance" or empty keeps
	// the search's own order. Kind limits results to movies, shows, or a
	// plugin kind; Library scopes results to one visible Vela library.
	Sort, Direction, Kind string
	Library               string
}

func (i *instance) search(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(frontend.WithSearchMemo(r.Context()))
	v, t, e := i.viewer(r)
	if e != nil {
		fail(w, e, r)
		return
	}
	var b searchRequest
	if body(r, &b) != nil || b.Query == "" {
		problem(w, 400, "request", "invalid_request")
		return
	}
	// The next keystroke from this device cancels this search, scoped or
	// not, and drops the remote lookups only it wanted.
	sctx, leave := frontend.EnterSearchGeneration(r.Context(), v.ID, requestDevice(r).ID, b.Query, int64(b.Generation))
	defer leave()
	r = r.WithContext(sctx)
	var targetView *frontend.View
	if b.Library != "" {
		views, err := i.host.Experience.Views(r.Context(), v)
		if err != nil {
			fail(w, err, r)
			return
		}
		for idx := range views {
			if views[idx].ID == b.Library {
				targetView = &views[idx]
				break
			}
		}
		if targetView == nil {
			problem(w, 400, "request", "invalid_library")
			return
		}
	}
	iq := frontend.ItemQuery{NoTotal: true}
	switch b.Sort {
	case "", "relevance":
	case "title", "released", "added", "rating", "popularity":
		iq.Sort = frontend.SortOrder(b.Sort)
	case "year", "index":
		offered := false
		if targetView != nil {
			wire := libraryWire(*targetView)
			for _, candidate := range wire["browse"].(object)["sorts"].([]object) {
				if candidate["id"] == b.Sort {
					offered = true
					break
				}
			}
		}
		if !offered {
			problem(w, 400, "request", "invalid_sort")
			return
		}
		iq.Sort = frontend.SortOrder(b.Sort)
	default:
		problem(w, 400, "request", "invalid_sort")
		return
	}
	switch b.Direction {
	case "", "asc":
	case "desc":
		iq.Desc = true
	default:
		problem(w, 400, "request", "invalid_direction")
		return
	}
	// Kind is one of searchKinds: movie, show, or a plugin kind the viewer
	// can see ("<pluginID>/<kindKey>"), matched by its library.
	var kindLibrary frontend.LibraryID
	switch b.Kind {
	case "":
	case "movie", "show":
		// Movies and Shows are the catalogue's films and series. Plugin
		// libraries that borrow their card shape (books, music, any plugin
		// kind) have their own searchKinds entries.
		iq.Types = []frontend.MediaType{frontend.MediaType(b.Kind)}
		iq.CatalogueOnly = true
	default:
		_, libs := i.searchKinds(r, v)
		lib, ok := libs[b.Kind]
		if !ok {
			problem(w, 400, "request", "invalid_kind")
			return
		}
		kindLibrary = lib
	}
	unified, _ := i.host.Experience.(frontend.UnifiedSearcher)
	if b.Mode == "auto" && unified == nil {
		problem(w, 400, "request", "invalid_search_mode")
		return
	}
	if b.Cursor != "" {
		offset, ok := decodeSearchCursor(b.Cursor, b)
		if !ok || b.Mode != "auto" {
			problem(w, 409, "conflict", "cursor_expired")
			return
		}
		iq.Offset = offset
	}
	n := b.Limit
	if n <= 0 {
		n = 50
	}
	n = min(n, 100)
	iq.Limit = n
	if b.Mode == "auto" {
		i.autoSearch(w, r, v, t, unified, b, iq, targetView, kindLibrary)
		return
	}

	incompatible := false
	if targetView != nil {
		if kindLibrary != 0 {
			if (targetView.PluginID == "" && targetView.KindKey == "") || (targetView.LibraryID != 0 && targetView.LibraryID != kindLibrary) {
				incompatible = true
			}
		}
		if len(iq.Types) > 0 && targetView.Type != "" && !slices.Contains(iq.Types, targetView.Type) {
			incompatible = true
		}
	}

	var p frontend.SearchPage
	scoped := false
	if incompatible && b.Mode == "fast" {
		p.Page = frontend.Page{Items: []frontend.Title{}}
		p.Continuation = frontend.ContinuationExhausted
		kindLibrary, scoped = 0, true
	} else if (targetView != nil || kindLibrary != 0) && b.Mode == "fast" {
		// Scoped search queries inside the target library directly, so sorting
		// and the page limit apply to that library's matches only.
		viewID := ""
		if targetView != nil {
			viewID = targetView.ID
		} else {
			viewID = i.viewForLibrary(r, v, kindLibrary)
			if viewID == "" {
				problem(w, 400, "request", "invalid_kind")
				return
			}
		}
		lq := iq
		lq.Filter.Query = b.Query
		page, err := i.host.Experience.LibraryItems(i.host.Experience.NoProviderWait(r.Context()), v, requestDevice(r), viewID, lq)
		if err != nil {
			fail(w, err, r)
			return
		}
		if len(page.Items) > n {
			page.Items = page.Items[:n]
		}
		if targetView != nil {
			markLandscapeTitles(page.Items, *targetView)
		}
		p.Page = page
		p.Continuation = frontend.ContinuationUnknown
		kindLibrary, scoped = 0, true
	}
	switch b.Mode {
	case "descriptive":
		source, ok := i.host.Experience.(frontend.DescriptiveSearcher)
		if !ok || !source.DescriptiveSearchAvailable() || !i.aiSearchAvailable() {
			problem(w, http.StatusServiceUnavailable, "policy", "feature_unavailable")
			return
		}
		p, e = source.DescriptiveSearch(r.Context(), v, requestDevice(r), b.Query, iq)
	case "fast":
		if !scoped {
			p, e = i.host.Experience.Search(i.host.Experience.NoProviderWait(r.Context()), v, requestDevice(r), b.Query, iq)
		}
	default:
		problem(w, 400, "request", "invalid_search_mode")
		return
	}
	if e != nil {
		fail(w, e, r)
		return
	}
	if b.Mode == "descriptive" && incompatible {
		p.Page.Items = []frontend.Title{}
	} else if b.Mode == "descriptive" && targetView != nil {
		isPluginView := targetView.PluginID != "" || targetView.KindKey != ""
		var libMembers map[frontend.RatingKey]bool
		var libScoped bool
		if !isPluginView && targetView.LibraryID != 0 && len(p.Page.Items) > 0 {
			if sr, ok := i.host.Experience.(frontend.LibraryScopeReader); ok {
				keys := make([]frontend.RatingKey, len(p.Page.Items))
				for idx, it := range p.Page.Items {
					keys[idx] = it.Key
				}
				if m, ok, err := sr.LibraryMembers(r.Context(), targetView.LibraryID, keys); err == nil && ok {
					libMembers, libScoped = m, true
				}
			}
		}
		kept := p.Page.Items[:0]
		for _, item := range p.Page.Items {
			match := false
			if isPluginView {
				match = targetView.LibraryID != 0 && item.LibraryID == targetView.LibraryID
			} else if targetView.LibraryID != 0 && item.LibraryID != 0 {
				match = item.LibraryID == targetView.LibraryID
			} else {
				match = targetView.Type == "" || item.Type == targetView.Type
			}
			if libScoped && !libMembers[item.Key] {
				match = false
			}
			if match && (kindLibrary == 0 || item.LibraryID == kindLibrary) && len(kept) < n {
				kept = append(kept, item)
			}
		}
		p.Page.Items = kept
		markLandscapeTitles(p.Page.Items, *targetView)
	} else if kindLibrary != 0 {
		// Descriptive search for a plugin kind keeps only that library.
		kept := p.Page.Items[:0]
		for _, item := range p.Page.Items {
			if item.LibraryID == kindLibrary && len(kept) < n {
				kept = append(kept, item)
			}
		}
		p.Page.Items = kept
	}
	ts, e := i.titles(r, v, t, p.Page.Items)
	if e != nil {
		fail(w, e, r)
		return
	}
	out := object{"query": b.Query, "generation": b.Generation, "items": ts, "continuation": p.Continuation}
	if b.Mode == "descriptive" {
		out["descriptive"] = descriptiveWire(p.Descriptive)
	}
	send(w, 200, out)
}
func (i *instance) interactions(w http.ResponseWriter, r *http.Request) {
	v, _, e := i.viewer(r)
	if e != nil {
		fail(w, e, r)
		return
	}
	var b struct {
		Interactions []struct {
			ReceiptToken, Kind  string
			OccurredAt          time.Time
			VisibleMilliseconds int
		}
	}
	if body(r, &b) != nil || len(b.Interactions) > 100 {
		problem(w, 400, "request", "invalid_request")
		return
	}
	reporter, ok := i.host.Experience.(frontend.RowInteractionReporter)
	if !ok {
		i.unavailable(w, r)
		return
	}
	for _, x := range b.Interactions {
		kind := frontend.RowInteractionKind("")
		switch x.Kind {
		case "visible":
			kind = frontend.RowImpression
		case "selected":
			kind = frontend.RowClick
		case "played":
			kind = frontend.RowPlay
		default:
			problem(w, 400, "request", "invalid_interaction")
			return
		}
		if e = reporter.ReportRowInteraction(r.Context(), frontend.RowInteraction{Viewer: v, Device: requestDevice(r), Protocol: "vela", Handle: x.ReceiptToken, Kind: kind}); e != nil {
			fail(w, e, r)
			return
		}
	}
	send(w, 200, object{"accepted": true})
}
func (i *instance) watch(w http.ResponseWriter, r *http.Request) {
	received, _ := r.Context().Value(requestReceivedKey{}).(time.Time)
	if received.IsZero() {
		received = i.serverNow()
	}
	v, _, e := i.viewer(r)
	if e != nil {
		fail(w, e, r)
		return
	}
	k, e := key(r)
	if e != nil {
		problem(w, 404, "not-found", "not_found")
		return
	}
	detail, e := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: v, Device: requestDevice(r), Key: k})
	if e != nil {
		fail(w, e, r)
		return
	}
	var b struct {
		Played           *bool    `json:"played"`
		PositionSeconds  *float64 `json:"positionSeconds"`
		ObservedAt       string   `json:"observedAt"`
		Offline          bool     `json:"offline"`
		Favorite         *bool    `json:"favorite"`
		Watchlisted      *bool    `json:"watchlisted"`
		ContinueWatching *bool    `json:"continueWatching"`
	}
	if body(r, &b) != nil {
		problem(w, 400, "request", "invalid_request")
		return
	}
	if b.ContinueWatching != nil && *b.ContinueWatching {
		problem(w, 400, "request", "invalid_request")
		return
	}
	if b.Offline {
		observed, err := time.Parse(time.RFC3339Nano, b.ObservedAt)
		var position time.Duration
		ok := true
		if b.PositionSeconds != nil {
			position, ok = seconds(*b.PositionSeconds)
		}
		played := b.PositionSeconds == nil && b.Played != nil && *b.Played
		if err != nil || !ok || position < 0 || observed.Before(time.Unix(0, 0)) {
			problem(w, 400, "request", "invalid_request")
			return
		}
		if b.PositionSeconds != nil || played {
			writer, ok := i.host.Experience.(frontend.OfflineProgressWriter)
			if !ok {
				i.unavailable(w, r)
				return
			}
			duration := detail.UserData.Duration
			if duration <= 0 {
				duration = detail.Title.Runtime
			}
			origin := frontend.WatchOrigin{Surface: "vela", ClientID: requestDevice(r).ID, HistoryTitle: &detail.Title}
			e = writer.SetOfflineProgress(frontend.WithWatchOrigin(r.Context(), origin), v, k, frontend.OfflineProgress{
				Position: position, Duration: duration, ObservedAt: observed, ReceivedAt: received, Played: played,
			})
		}
	} else if b.Played != nil {
		if *b.Played {
			e = i.host.Experience.SetPlayed(r.Context(), v, k)
		} else {
			e = i.host.Experience.SetUnplayed(r.Context(), v, k)
		}
	}
	if e == nil && b.Favorite != nil {
		e = i.host.Experience.SetFavorite(r.Context(), v, k, *b.Favorite)
	}
	if e == nil && b.Watchlisted != nil {
		e = i.host.Experience.SetWatchlist(r.Context(), v, k, *b.Watchlisted)
	}
	if e == nil && b.ContinueWatching != nil {
		if !k.IsPluginKey() && (k.Kind() == frontend.KindPlaylist || k.Kind() == frontend.KindCollection || k.Kind() == frontend.KindSmart) {
			problem(w, 400, "request", "invalid_request")
			return
		}
		target := k
		if detail.Title.Type == frontend.TypeEpisode || k.Kind() == frontend.KindEpisode {
			if detail.Title.ShowKey != 0 {
				target = detail.Title.ShowKey
			}
		}
		e = i.host.Experience.RemoveFromContinue(r.Context(), v, target)
	}
	if e != nil {
		fail(w, e, r)
		return
	}
	send(w, 200, object{"accepted": true})
}

// Older experience implementations omit the status after a successful read.
func descriptiveWire(status *frontend.DescriptiveSearchStatus) object {
	state := frontend.DescriptiveReady
	if status != nil {
		state = status.State
	}
	retry := object{"allowed": false}
	if state == frontend.DescriptivePartial || state == frontend.DescriptivePending || state == frontend.DescriptiveBusy || state == frontend.DescriptiveFailed {
		seconds := 2
		if state == frontend.DescriptiveFailed {
			seconds = 60
		}
		if status != nil && status.RetryAfterSeconds > 0 {
			seconds = status.RetryAfterSeconds
		}
		retry = object{"allowed": true, "afterSeconds": seconds}
	}
	return object{"state": state, "retry": retry}
}

// searchKinds lists the result kinds a viewer can filter search by: movies,
// shows, and every plugin library kind the viewer can open. Plugin kinds are
// keyed "<pluginID>/<kindKey>" and labelled with their library's name.
func (i *instance) searchKinds(r *http.Request, v *frontend.Viewer) ([]object, map[string]frontend.LibraryID) {
	kinds := []object{{"id": "movie", "label": "Movies"}, {"id": "show", "label": "Shows"}}
	libs := map[string]frontend.LibraryID{}
	if v == nil {
		return kinds, libs
	}
	views, err := i.host.Experience.Views(r.Context(), v)
	if err != nil {
		return kinds, libs
	}
	for _, view := range views {
		if view.PluginID == "" || view.KindKey == "" || view.LibraryID == 0 {
			continue
		}
		id := view.PluginID + "/" + view.KindKey
		if _, seen := libs[id]; seen {
			continue
		}
		libs[id] = view.LibraryID
		kinds = append(kinds, object{"id": id, "label": view.Name})
	}
	return kinds, libs
}

// viewForLibrary returns the viewer's published view id for a library.
func (i *instance) viewForLibrary(r *http.Request, v *frontend.Viewer, lib frontend.LibraryID) string {
	views, err := i.host.Experience.Views(r.Context(), v)
	if err != nil {
		return ""
	}
	for _, view := range views {
		if view.LibraryID == lib {
			return view.ID
		}
	}
	return ""
}
