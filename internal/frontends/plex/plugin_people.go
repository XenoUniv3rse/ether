package plex

import (
	"context"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	kitimages "github.com/braedonsaunders/ether/internal/frontend/kit/images"
)

// canSeePluginTitle reports whether u may see a projected plugin item in Plex.
// A plugin item belongs to its own plugin library rather than a built-in TMDB
// section, so section access is checked against t.LibraryID when present.
func (s *Server) canSeePluginTitle(u *frontend.User, t *frontend.Title) bool {
	if t == nil || !t.Key.IsPluginKey() {
		return false
	}
	if t.LibraryID != 0 {
		sec, ok := s.sectionForLibrary(t.LibraryID)
		if !ok || !s.canSeeSection(u, sec.ID) {
			return false
		}
	}
	return s.titleAllowed(u, t)
}

func (s *Server) pluginKindSections(u *frontend.User, sectionID int) []Section {
	var out []Section
	for _, sec := range s.userSections(u) {
		if !isKindSection(sec) {
			continue
		}
		if sectionID > 0 && sec.ID != sectionID {
			continue
		}
		out = append(out, sec)
	}
	return out
}

// searchPluginTitles queries visible plugin libraries through Experience,
// returning both direct title/field hits and all titles of matched cast
// with title-first SearchScore values.
func (s *Server) searchPluginTitles(ctx context.Context, pr *Request, query string, sectionID, limit int) []frontend.Title {
	query = strings.TrimSpace(query)
	if query == "" || s.exp() == nil || pr == nil {
		return nil
	}
	secs := s.pluginKindSections(pr.User, sectionID)
	if len(secs) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = 50
	}
	seen := make(map[frontend.RatingKey]int)
	var out []frontend.Title
	for _, sec := range secs {
		page, err := s.exp().LibraryItems(ctx, pr.User, plexDevice(pr), sec.IDString(), frontend.ItemQuery{
			Filter: frontend.Filter{Query: query},
			Limit:  limit,
		})
		if err != nil {
			continue
		}
		for _, t := range page.Items {
			if !t.Key.IsPluginKey() || !s.canSeePluginTitle(pr.User, &t) {
				continue
			}
			if t.SearchScore <= 0 {
				t.SearchScore = fallbackPluginSearchScore(t.Title, query)
			}
			if idx, dup := seen[t.Key]; dup {
				if t.SearchScore > out[idx].SearchScore {
					out[idx].SearchScore = t.SearchScore
				}
				continue
			}
			seen[t.Key] = len(out)
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].SearchScore > out[j].SearchScore
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func fallbackPluginSearchScore(title, query string) float64 {
	qFold := searchFoldName(query)
	tFold := searchFoldName(title)
	switch {
	case qFold != "" && tFold == qFold:
		return 1.0
	case qFold != "" && strings.HasPrefix(tFold, qFold):
		return 0.97
	case qFold != "" && strings.Contains(tFold, qFold):
		return 0.93
	default:
		return 0.60
	}
}

// mergePluginSearchResults merges catalogue search hits with plugin library
// search hits, preserving title-first ordering across both sources.
func mergePluginSearchResults(catalogItems, pluginItems []frontend.Title, aiItems, limit int) []frontend.Title {
	if len(pluginItems) == 0 {
		return catalogItems
	}
	if len(catalogItems) == 0 {
		if limit > 0 && len(pluginItems) > limit {
			return pluginItems[:limit]
		}
		return pluginItems
	}
	aiPrefix := min(max(aiItems, 0), len(catalogItems))
	leadingAI := catalogItems[:aiPrefix]
	restCatalog := catalogItems[aiPrefix:]

	type scored struct {
		title frontend.Title
		score float64
	}
	seen := make(map[frontend.RatingKey]bool, len(catalogItems)+len(pluginItems))
	for _, t := range leadingAI {
		seen[t.Key] = true
	}
	combined := make([]scored, 0, len(restCatalog)+len(pluginItems))
	total := len(restCatalog) + len(pluginItems)
	for i, t := range restCatalog {
		if seen[t.Key] {
			continue
		}
		seen[t.Key] = true
		sc := t.SearchScore
		if sc <= 0 {
			sc = max(0.75, searchScore(i, total))
		}
		combined = append(combined, scored{title: t, score: sc})
	}
	for i, t := range pluginItems {
		if seen[t.Key] {
			continue
		}
		seen[t.Key] = true
		sc := t.SearchScore
		if sc <= 0 {
			sc = searchScore(len(restCatalog)+i, total)
		}
		combined = append(combined, scored{title: t, score: sc})
	}
	sort.SliceStable(combined, func(i, j int) bool {
		return combined[i].score > combined[j].score
	})
	out := make([]frontend.Title, 0, len(leadingAI)+len(combined))
	out = append(out, leadingAI...)
	for _, entry := range combined {
		out = append(out, entry.title)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// searchPluginPeople searches people across visible plugin libraries, building
// Plex actor/director Directory entries and reasoned titles for matched people.
func (s *Server) searchPluginPeople(ctx context.Context, pr *Request, query string, sectionID, limit int, inHub bool) peopleSearchResult {
	out := peopleSearchResult{titles: map[frontend.MediaType][]reasonedTitle{}}
	if s.exp() == nil || pr == nil || strings.TrimSpace(query) == "" {
		return out
	}
	secs := s.pluginKindSections(pr.User, sectionID)
	if len(secs) == 0 {
		return out
	}
	reader, ok := s.exp().(frontend.PeopleReader)
	if !ok {
		return out
	}
	if limit <= 0 {
		limit = 10
	}
	people, err := reader.People(ctx, pr.User, plexDevice(pr), frontend.PeopleQuery{
		Query:             query,
		Limit:             max(limit*2, 20),
		PluginResultsOnly: true,
	})
	if err != nil || len(people) == 0 {
		return out
	}
	var pluginCands []frontend.PersonDetail
	for _, p := range people {
		if frontend.IsPluginPersonID(p.TMDB) && p.Name != "" {
			pluginCands = append(pluginCands, p)
		}
	}
	if len(pluginCands) == 0 {
		return out
	}
	sort.SliceStable(pluginCands, func(i, j int) bool {
		return personSearchScore(query, pluginCands[i].Name) > personSearchScore(query, pluginCands[j].Name)
	})

	creditLimit := max(limit*3, 30)
	var actorTitles, directorTitles []reasonedTitle
	for _, cand := range pluginCands {
		type secHit struct {
			sec       Section
			count     int
			thumbPath string
			thumbKey  frontend.RatingKey
		}
		actorSecs := map[int]*secHit{}
		directorSecs := map[int]*secHit{}
		var secOrder []int
		noteSec := func(sec Section) {
			if !slices.Contains(secOrder, sec.ID) {
				secOrder = append(secOrder, sec.ID)
			}
		}
		seenTitles := map[frontend.RatingKey]bool{}
		idStr := strconv.FormatInt(cand.TMDB, 10)

		for _, sec := range secs {
			page, err := s.exp().LibraryItems(ctx, pr.User, plexDevice(pr), sec.IDString(), frontend.ItemQuery{
				Filter:         frontend.Filter{Person: cand.TMDB},
				Limit:          creditLimit,
				SuppressExpand: true,
			})
			if err != nil || len(page.Items) == 0 {
				continue
			}
			for i := range page.Items {
				t := page.Items[i]
				if !t.Key.IsPluginKey() || !s.canSeePluginTitle(pr.User, &t) || seenTitles[t.Key] {
					continue
				}
				targetSec := sec
				if t.LibraryID != 0 {
					if ownerSec, ok := s.sectionForLibrary(t.LibraryID); ok {
						targetSec = ownerSec
					}
				}
				if sectionID > 0 && targetSec.ID != sectionID {
					continue
				}
				seenTitles[t.Key] = true
				castP, isCast := personInList(t.Cast, cand.TMDB)
				dirP, isDir := personInList(t.Directors, cand.TMDB)
				wriP, isWri := personInList(t.Writers, cand.TMDB)
				if !isCast && !isDir && isWri {
					// Plex search hubs expose Actors and Directors; surface
					// writer-only plugin credits under actor so the person card
					// and reasoned titles remain reachable.
					castP, isCast = wriP, true
				}
				if isCast {
					noteSec(targetSec)
					h := actorSecs[targetSec.ID]
					if h == nil {
						h = &secHit{sec: targetSec}
						actorSecs[targetSec.ID] = h
					}
					h.count++
					if h.thumbPath == "" && castP.ThumbPath != "" {
						h.thumbPath = castP.ThumbPath
						h.thumbKey = t.Key
					}
					actorTitles = append(actorTitles, reasonedTitle{
						title:       t,
						reason:      "actor",
						reasonID:    idStr,
						reasonTitle: cand.Name,
					})
				}
				if isDir {
					noteSec(targetSec)
					h := directorSecs[targetSec.ID]
					if h == nil {
						h = &secHit{sec: targetSec}
						directorSecs[targetSec.ID] = h
					}
					h.count++
					if h.thumbPath == "" && dirP.ThumbPath != "" {
						h.thumbPath = dirP.ThumbPath
						h.thumbKey = t.Key
					}
					directorTitles = append(directorTitles, reasonedTitle{
						title:       t,
						reason:      "director",
						reasonID:    idStr,
						reasonTitle: cand.Name,
					})
				}
			}
		}

		for _, secID := range secOrder {
			if h := actorSecs[secID]; h != nil && h.count > 0 {
				if len(out.actors) < limit {
					person := frontend.Person{TMDB: cand.TMDB, Name: cand.Name, ThumbPath: h.thumbPath}
					out.actors = append(out.actors, personSearchDirectory(h.sec, person, "actor", h.count, h.thumbKey, inHub, ""))
					out.actorScores = append(out.actorScores, personSearchScore(query, cand.Name))
				} else {
					out.actorsMore = true
				}
			}
			if h := directorSecs[secID]; h != nil && h.count > 0 {
				if len(out.directors) < limit {
					person := frontend.Person{TMDB: cand.TMDB, Name: cand.Name, ThumbPath: h.thumbPath}
					out.directors = append(out.directors, personSearchDirectory(h.sec, person, "director", h.count, h.thumbKey, inHub, ""))
					out.directorScores = append(out.directorScores, personSearchScore(query, cand.Name))
				} else {
					out.directorsMore = true
				}
			}
		}
	}

	seenByType := map[frontend.MediaType]map[frontend.RatingKey]bool{}
	addReasoned := func(rt reasonedTitle) {
		typ := frontend.MediaType(s.plexTypeFor(&rt.title))
		if typ == "" {
			typ = rt.title.Type
		}
		seen := seenByType[typ]
		if seen == nil {
			seen = map[frontend.RatingKey]bool{}
			seenByType[typ] = seen
		}
		if seen[rt.title.Key] {
			return
		}
		seen[rt.title.Key] = true
		out.titles[typ] = append(out.titles[typ], rt)
	}
	for _, rt := range actorTitles {
		addReasoned(rt)
	}
	for _, rt := range directorTitles {
		addReasoned(rt)
	}
	return out
}

func mergePeopleSearchResults(base, extra peopleSearchResult, limit int) peopleSearchResult {
	if len(extra.actors) == 0 && len(extra.directors) == 0 && len(extra.titles) == 0 {
		return base
	}
	if base.titles == nil {
		base.titles = map[frontend.MediaType][]reasonedTitle{}
	}
	for _, i := range RangeSlice(len(extra.actors)) {
		if limit <= 0 || len(base.actors) < limit {
			base.actors = append(base.actors, extra.actors[i])
			base.actorScores = append(base.actorScores, extra.actorScores[i])
		} else {
			base.actorsMore = true
		}
	}
	if extra.actorsMore {
		base.actorsMore = true
	}
	for _, i := range RangeSlice(len(extra.directors)) {
		if limit <= 0 || len(base.directors) < limit {
			base.directors = append(base.directors, extra.directors[i])
			base.directorScores = append(base.directorScores, extra.directorScores[i])
		} else {
			base.directorsMore = true
		}
	}
	if extra.directorsMore {
		base.directorsMore = true
	}
	for typ, rts := range extra.titles {
		seen := make(map[frontend.RatingKey]bool, len(base.titles[typ]))
		for _, rt := range base.titles[typ] {
			seen[rt.title.Key] = true
		}
		for _, rt := range rts {
			if !seen[rt.title.Key] {
				seen[rt.title.Key] = true
				base.titles[typ] = append(base.titles[typ], rt)
			}
		}
	}
	return base
}

func RangeSlice(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// resolvePluginPerson resolves /library/people/{id} when {id} is a deterministic
// plugin person ID (or tmdb-person-<id> tagKey).
func (s *Server) resolvePluginPerson(ctx context.Context, pr *Request, id int64, tagKeyForm bool) (resolvedPerson, bool) {
	if !frontend.IsPluginPersonID(id) || s.exp() == nil || pr == nil {
		return resolvedPerson{}, false
	}
	titles := s.pluginPersonTitles(ctx, pr, id, personMediaCap)
	if len(titles) == 0 {
		return resolvedPerson{}, false
	}
	var (
		person   frontend.Person
		kinds    []string
		thumbKey frontend.RatingKey
	)
	for i := range titles {
		t := &titles[i]
		if p, ok := personInList(t.Cast, id); ok {
			if person.Name == "" {
				person = p
			}
			if person.ThumbPath == "" && p.ThumbPath != "" {
				person.ThumbPath = p.ThumbPath
			}
			if thumbKey == 0 && p.ThumbPath != "" {
				thumbKey = t.Key
			}
			if !slices.Contains(kinds, "cast") {
				kinds = append(kinds, "cast")
			}
		}
		if p, ok := personInList(t.Directors, id); ok {
			if person.Name == "" {
				person = p
			}
			if person.ThumbPath == "" && p.ThumbPath != "" {
				person.ThumbPath = p.ThumbPath
			}
			if thumbKey == 0 && p.ThumbPath != "" {
				thumbKey = t.Key
			}
			if !slices.Contains(kinds, "director") {
				kinds = append(kinds, "director")
			}
		}
		if p, ok := personInList(t.Writers, id); ok {
			if person.Name == "" {
				person = p
			}
			if person.ThumbPath == "" && p.ThumbPath != "" {
				person.ThumbPath = p.ThumbPath
			}
			if thumbKey == 0 && p.ThumbPath != "" {
				thumbKey = t.Key
			}
			if !slices.Contains(kinds, "writer") {
				kinds = append(kinds, "writer")
			}
		}
	}
	if person.Name == "" {
		return resolvedPerson{}, false
	}
	person.TMDB = id
	out := resolvedPerson{id: id, tagKeyForm: tagKeyForm, person: person, thumbKey: thumbKey}
	for _, k := range personKinds {
		if slices.Contains(kinds, k.credit) {
			out.primary = k
			return out, true
		}
	}
	return resolvedPerson{}, false
}

// pluginPersonTitles lists all visible plugin items credited to person id.
func (s *Server) pluginPersonTitles(ctx context.Context, pr *Request, id int64, limit int) []frontend.Title {
	if !frontend.IsPluginPersonID(id) || s.exp() == nil || pr == nil {
		return nil
	}
	secs := s.pluginKindSections(pr.User, 0)
	if len(secs) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = personMediaCap
	}
	seen := map[frontend.RatingKey]bool{}
	var out []frontend.Title
	for _, sec := range secs {
		page, err := s.exp().LibraryItems(ctx, pr.User, plexDevice(pr), sec.IDString(), frontend.ItemQuery{
			Filter: frontend.Filter{Person: id},
			Limit:  limit,
		})
		if err != nil {
			continue
		}
		for _, t := range page.Items {
			if t.Key.IsPluginKey() && s.canSeePluginTitle(pr.User, &t) && !seen[t.Key] {
				seen[t.Key] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// servePluginItemImage serves item artwork (/library/metadata/{key}/{element})
// for a plugin item, routing absolute URLs through Artwork or channelLogos and
// resizing when dimensions are requested.
func (s *Server) servePluginItemImage(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, element string, width int) bool {
	if !key.IsPluginKey() {
		return false
	}
	pr := FromContext(r.Context())
	t, err := s.kindItem(r.Context(), pr, key)
	if err != nil || t == nil || !s.canSeePluginTitle(pr.User, t) {
		writeHTTPError(w, http.StatusNotFound)
		return true
	}
	if art := s.getHost().Artwork; art != nil {
		req := frontend.ArtworkRequest{
			Viewer: pr.User,
			Device: plexDevice(pr),
			Item:   key,
		}
		if images, _, err := art.List(r.Context(), req); err == nil {
			if chosen, ok := pickPluginArtwork(images, t.Type, element); ok {
				tr := frontend.ArtworkTransform{Width: width}
				if data, ct, err := art.Read(r.Context(), req, chosen, tr); err == nil && len(data) > 0 {
					s.writePluginImageBytes(w, r, data, ct)
					return true
				}
			}
		}
	}
	rawURL := pluginTitleElementPath(t, element)
	if rawURL == "" {
		if s.serveGeneratedPoster(w, r, key, element, width) {
			return true
		}
		writeHTTPError(w, http.StatusNotFound)
		return true
	}
	if strings.HasPrefix(rawURL, "https://") || strings.HasPrefix(rawURL, "http://") {
		if s.channelLogos == nil {
			writeHTTPError(w, http.StatusBadGateway)
			return true
		}
		data, ct, err := s.channelLogos.GetUntrustedURL(r.Context(), rawURL)
		if err != nil || len(data) == 0 {
			writeHTTPError(w, http.StatusBadGateway)
			return true
		}
		s.writePluginImageBytes(w, r, data, ct)
		return true
	}
	s.serveRemoteImage(w, r, s.svc().imageURL(rawURL, tmdbSize(width)))
	return true
}

func pickPluginArtwork(images []frontend.Image, typ frontend.MediaType, element string) (frontend.Image, bool) {
	var want []string
	switch element {
	case "art", "background":
		want = []string{frontend.ImageBackdrop}
	case "clearLogo":
		want = []string{frontend.ImageLogo}
	case "banner":
		want = []string{frontend.ImageBanner}
	case "thumb", "poster":
		if typ == frontend.TypeEpisode && element == "thumb" {
			want = []string{frontend.ImageThumb, frontend.ImagePoster}
		} else {
			want = []string{frontend.ImagePoster, frontend.ImageThumb}
		}
	default:
		return frontend.Image{}, false
	}
	for _, kind := range want {
		for _, img := range images {
			if img.Kind == kind && img.Path != "" {
				return img, true
			}
		}
	}
	return frontend.Image{}, false
}

func pluginTitleElementPath(t *frontend.Title, element string) string {
	switch element {
	case "art", "background":
		return t.BackdropPath
	case "clearLogo":
		return t.LogoPath
	case "thumb", "poster":
		if t.Type == frontend.TypeEpisode && element == "thumb" && t.StillPath != "" {
			return t.StillPath
		}
		if t.PosterPath != "" {
			return t.PosterPath
		}
		return t.StillPath
	default:
		return ""
	}
}

// servePluginPersonImage serves an http(s) cast member portrait for a plugin
// item, resizing when /photo/:/transcode supplies width or height.
func (s *Server) servePluginPersonImage(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, personID int64, imagePath string, width int) {
	pr := FromContext(r.Context())
	wantSubject := ""
	if personID > 0 {
		wantSubject = "tmdb:" + strconv.FormatInt(personID, 10)
	}
	if art := s.getHost().Artwork; art != nil && wantSubject != "" {
		req := frontend.ArtworkRequest{
			Viewer: pr.User,
			Device: plexDevice(pr),
			Item:   key,
		}
		if images, _, err := art.List(r.Context(), req); err == nil {
			for _, img := range images {
				if img.Kind != frontend.ImagePerson || img.Subject != wantSubject {
					continue
				}
				tr := frontend.ArtworkTransform{Width: width}
				if data, ct, err := art.Read(r.Context(), req, img, tr); err == nil && len(data) > 0 {
					s.writePluginImageBytes(w, r, data, ct)
					return
				}
				break
			}
		}
	}
	if s.channelLogos == nil {
		writeHTTPError(w, http.StatusBadGateway)
		return
	}
	data, ct, err := s.channelLogos.GetUntrustedURL(r.Context(), imagePath)
	if err != nil || len(data) == 0 {
		writeHTTPError(w, http.StatusBadGateway)
		return
	}
	s.writePluginImageBytes(w, r, data, ct)
}

func (s *Server) writePluginImageBytes(w http.ResponseWriter, r *http.Request, data []byte, ct string) {
	out, outCT, err := s.transformPhoto(r, data, ct)
	if err != nil {
		writeHTTPError(w, http.StatusBadGateway)
		return
	}
	h := w.Header()
	h.Add("Vary", "X-Plex-Product")
	h.Add("Vary", "X-Plex-Token")
	writeArtworkBytes(w, r, out, outCT, kitimages.Revalidate)
}
