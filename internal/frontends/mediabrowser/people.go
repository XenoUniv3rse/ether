package mediabrowser

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

// peopleDTO uses parent-authorized facet IDs and title-bound portrait grants.
func (i *instance) peopleDTO(r *http.Request, viewer *frontend.Viewer, token string, detail frontend.ItemDetail) []any {
	type credit struct {
		id               int64
		wire             string
		name, role, kind string
	}
	var credits []credit
	for _, p := range detail.Title.Cast {
		credits = append(credits, credit{p.TMDB, p.ID, p.Name, p.Role, "Actor"})
	}
	for _, p := range detail.Title.Directors {
		credits = append(credits, credit{p.TMDB, p.ID, p.Name, p.Role, "Director"})
	}
	for _, p := range detail.Title.Writers {
		credits = append(credits, credit{p.TMDB, p.ID, p.Name, p.Role, "Writer"})
	}
	for _, p := range detail.Title.Crew {
		if strings.EqualFold(p.Role, "Producer") {
			credits = append(credits, credit{p.TMDB, p.ID, p.Name, p.Role, "Producer"})
		}
	}
	ids := make([]int64, 0, min(len(credits), 256))
	seen := map[int64]bool{}
	for _, c := range credits {
		if c.id <= 0 || seen[c.id] {
			continue
		}
		if len(ids) == 256 {
			break
		}
		seen[c.id] = true
		ids = append(ids, c.id)
	}
	facets := map[int64]string{}
	if len(ids) > 0 {
		resolved, err := i.host.Experience.PersonFacetIDs(r.Context(), viewer, device(r), detail.Title.Key, ids)
		if err != nil {
			return nil
		}
		facets = resolved
	}
	portraits := map[string]frontend.Image{}
	if detail.ImagesAuthoritative && i.host.ImageGrants != nil {
		for _, img := range detail.Images {
			if img.Kind == frontend.ImagePerson && img.Subject != "" {
				if _, ok := portraits[img.Subject]; !ok {
					portraits[img.Subject] = img
				}
			}
		}
	}
	var people []frontend.PersonDetail
	portraitIndex := map[int64]int{}
	for _, c := range credits {
		if _, added := portraitIndex[c.id]; added || c.name == "" || facets[c.id] == "" {
			continue
		}
		if img, ok := portraits["tmdb:"+strconv.FormatInt(c.id, 10)]; ok {
			portraitIndex[c.id] = len(people)
			people = append(people, frontend.PersonDetail{TMDB: c.id, ArtworkItem: detail.Title.Key, Images: []frontend.Image{img}})
		}
	}
	grants, images := i.issuePersonGrantsFor(r, token, people)
	result := make([]any, 0, len(credits))
	for _, c := range credits {
		if c.name == "" {
			continue
		}
		id := ""
		if c.id > 0 {
			id = facets[c.id]
			if id == "" {
				continue
			}
		} else {
			id = c.wire
		}
		// BaseItemPerson.Id is a required UUID in the Jellyfin SDK: one
		// credit without a usable id fails decoding of the whole item
		// (Jellyfin Android TV then exits the detail page, #614). Drop the
		// credit rather than the page.
		if id == "" || (i.protocol == "jellyfin" && !guidLike(id)) {
			continue
		}
		dto := map[string]any{"Name": c.name, "Type": c.kind, "Id": id}
		if c.role != "" {
			dto["Role"] = c.role
		}
		if n, ok := portraitIndex[c.id]; ok && grants[n] != "" {
			dto["PrimaryImageTag"] = grants[n]
			i.addImageMetadata(dto, "Primary", grants[n], images[n])
			delete(dto, "PrimaryImageAspectRatio")
		}
		result = append(result, dto)
	}
	return result
}

func personPortrait(person frontend.PersonDetail) (frontend.Image, bool) {
	if person.TMDB > 0 {
		want := "tmdb:" + strconv.FormatInt(person.TMDB, 10)
		for _, img := range person.Images {
			if img.Kind == frontend.ImagePerson && img.Subject == want && img.Path != "" {
				return img, true
			}
		}
	}
	for _, img := range person.Images {
		if img.Kind == frontend.ImagePerson && img.Path != "" {
			return img, true
		}
	}
	if person.KnownFor == "MusicArtist" {
		for _, img := range person.Images {
			if img.Kind == frontend.ImagePoster && img.Path != "" {
				return img, true
			}
		}
	}
	return frontend.Image{}, false
}

func personImagesEnabled(r *http.Request) bool {
	if v := route.Query(r.URL.Query(), "EnableImages"); len(v) == 1 && strings.EqualFold(v[0], "false") {
		return false
	}
	types := route.Query(r.URL.Query(), "EnableImageTypes")
	if len(types) == 0 {
		return true
	}
	for _, value := range types {
		for _, t := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(t), "primary") {
				return true
			}
		}
	}
	return false
}

func (i *instance) issuePersonGrants(r *http.Request, people []frontend.PersonDetail) ([]string, []frontend.Image) {
	_, token, _ := i.viewer(r)
	return i.issuePersonGrantsFor(r, token, people)
}

func (i *instance) issuePersonGrantsFor(r *http.Request, token string, people []frontend.PersonDetail) ([]string, []frontend.Image) {
	grants := make([]string, len(people))
	imgs := make([]frontend.Image, len(people))
	if i.host.ImageGrants == nil || len(people) == 0 || !personImagesEnabled(r) {
		return grants, imgs
	}
	if batch, ok := i.host.ImageGrants.(frontend.PersonImageGrantBatch); ok {
		targets := make([]frontend.PersonImageTarget, 0, len(people))
		positions := make([]int, 0, len(people))
		for n, person := range people {
			if person.ArtworkItem == 0 || person.ArtworkItem.IsPluginKey() {
				continue
			}
			if img, ok := personPortrait(person); ok {
				targets = append(targets, frontend.PersonImageTarget{Item: person.ArtworkItem, Image: img})
				positions = append(positions, n)
				imgs[n] = img
			}
		}
		for start := 0; start < len(targets); start += 100 {
			end := min(start+100, len(targets))
			tokens, err := batch.IssuePersonImages(r.Context(), token, device(r), targets[start:end])
			if err == nil && len(tokens) == end-start {
				for n, pos := range positions[start:end] {
					grants[pos] = tokens[n]
				}
			}
		}
		// The batch admits catalogue credits only; people from plugin
		// library items are granted one at a time, which checks the item.
		for n, person := range people {
			if grants[n] != "" || person.ArtworkItem == 0 || !person.ArtworkItem.IsPluginKey() {
				continue
			}
			img, ok := personPortrait(person)
			if !ok {
				continue
			}
			if grant, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: token, Device: device(r), Item: person.ArtworkItem, Image: img}); err == nil && grant != "" {
				grants[n], imgs[n] = grant, img
			}
		}
		return grants, imgs
	}
	for n, person := range people {
		if person.ArtworkItem == 0 {
			continue
		}
		img, ok := personPortrait(person)
		if !ok {
			continue
		}
		grant, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: token, Device: device(r), Item: person.ArtworkItem, Image: img})
		if err == nil && grant != "" {
			grants[n] = grant
			imgs[n] = img
		}
	}
	return grants, imgs
}

func (i *instance) personItemDTO(person frontend.PersonDetail, grant string, img frontend.Image, detail bool) map[string]any {
	itemType := "Person"
	isFolder := false
	if person.KnownFor == "MusicArtist" {
		itemType = "MusicArtist"
		isFolder = true
	}
	dto := map[string]any{"Id": person.ID, "Name": person.Name, "Type": itemType, "ServerId": i.info.ID, "IsFolder": isFolder}
	if itemType == "MusicArtist" {
		dto["SortName"] = person.Name
	}
	if grant != "" {
		dto["ImageTags"] = map[string]string{"Primary": grant}
		i.addImageMetadata(dto, "Primary", grant, img)
		delete(dto, "PrimaryImageAspectRatio")
	}
	if !detail {
		return dto
	}
	if person.Biography != "" {
		dto["Overview"] = person.Biography
	}
	if person.Birthday != "" {
		if born, err := time.Parse("2006-01-02", person.Birthday); err == nil {
			dto["PremiereDate"] = wireTime(born)
		}
	}
	pids := map[string]string{}
	if person.TMDB > 0 && !frontend.IsPluginPersonID(person.TMDB) {
		pids["Tmdb"] = strconv.FormatInt(person.TMDB, 10)
	}
	if person.IMDB != "" {
		pids["Imdb"] = person.IMDB
	}
	if len(pids) > 0 {
		dto["ProviderIds"] = pids
	}
	dto["BackdropImageTags"] = []string{}
	return dto
}

func (i *instance) writePersonItem(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, wireID string) bool {
	if i.host.Experience == nil {
		return false
	}
	people, ok := i.host.Experience.(frontend.PeopleReader)
	if !ok {
		return false
	}
	person, err := people.Person(i.identityContext(r), v, device(r), wireID, frontend.PersonReadOptions{})
	if err != nil {
		if errors.Is(err, frontend.ErrNotFound) || errors.Is(err, frontend.ErrPeopleQuery) {
			return false
		}
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return true
	}
	grants, imgs := i.issuePersonGrants(r, []frontend.PersonDetail{person})
	dto := i.personItemDTO(person, grants[0], imgs[0], true)
	i.addPersonWorkCounts(r, v, person, dto)
	i.replyItems(w, r, dto)
	return true
}

// addPersonWorkCounts sets MovieCount and SeriesCount on a person's detail
// item. Jellyfin Web (itemsByName.js) and Emby build a person's Movies and
// Shows sections only when these counts are set, then list each with
// IncludeItemTypes and PersonIds; the counts come from that same query so a
// shown section is never empty. Plugin people (authors, narrators) have no
// catalogue credits.
func (i *instance) addPersonWorkCounts(r *http.Request, v *frontend.Viewer, person frontend.PersonDetail, dto map[string]any) {
	if r == nil || person.TMDB <= 0 || frontend.IsPluginPersonID(person.TMDB) || person.KnownFor == "MusicArtist" {
		return
	}
	for _, c := range []struct {
		typ   frontend.MediaType
		field string
	}{{frontend.TypeMovie, "MovieCount"}, {frontend.TypeShow, "SeriesCount"}} {
		q := frontend.ItemQuery{Limit: 1, Types: []frontend.MediaType{c.typ}}
		q.Filter.Person = person.TMDB
		page, err := i.host.Experience.LibraryItems(r.Context(), v, device(r), "", q)
		if err != nil || page.Total <= 0 {
			continue
		}
		dto[c.field] = page.Total
	}
}

func personExcluded(q frontend.ItemQuery) bool {
	for _, typ := range q.ExcludedTypes {
		if typ == "person" {
			return true
		}
	}
	return false
}

func personTypesOnly(q frontend.ItemQuery) (only bool, excluded bool) {
	if len(q.Types) > 0 {
		return false, false
	}
	hasPerson := false
	for _, typ := range q.UnservedTypes {
		if typ == "person" {
			hasPerson = true
		} else {
			return false, false
		}
	}
	if !hasPerson {
		return false, false
	}
	return true, personExcluded(q)
}

func (i *instance) personSearchHints(r *http.Request, v *frontend.Viewer, term string, offset, limit int) ([]any, int, error) {
	reader, ok := i.host.Experience.(frontend.PeopleReader)
	if !ok {
		return nil, 0, nil
	}
	term = strings.TrimSpace(term)
	if term == "" || len([]rune(term)) > 200 {
		return []any{}, 0, nil
	}
	offset = max(offset, 0)
	if limit <= 0 {
		limit = 50
	}
	people, err := reader.People(r.Context(), v, device(r), frontend.PeopleQuery{Query: term, Limit: min(offset+limit, 100)})
	if err != nil {
		return nil, 0, err
	}
	page := paging.Slice(people, offset, limit)
	grants, _ := i.issuePersonGrants(r, page)
	hints := make([]any, 0, len(page))
	for n, person := range page {
		// Jellyfin's Kotlin SDK decodes SearchHint.Id / ItemId as UUID.
		if i.protocol == "jellyfin" && !guidLike(person.ID) {
			continue
		}
		hint := map[string]any{"Id": person.ID, "ItemId": person.ID, "Name": person.Name, "Type": "Person", "IsFolder": false}
		if grants[n] != "" {
			hint["PrimaryImageTag"] = grants[n]
		}
		hints = append(hints, hint)
	}
	return hints, len(people), nil
}
