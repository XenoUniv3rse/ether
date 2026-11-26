package vela

import (
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// mediaSummary reports only facts already present on the authorized detail.
// Reading a title must not resolve a source just to fill a badge.
func mediaSummary(d frontend.ItemDetail) object {
	tracks := d.Tracks
	if d.TracksEstimated {
		tracks = nil
		for _, v := range d.Versions {
			if !v.Media.Estimated && len(v.Media.Streams) > 0 {
				tracks = v.Media.Streams
				break
			}
		}
		if len(tracks) == 0 {
			return nil
		}
	}
	video, audio, subtitles := []object{}, []object{}, []object{}
	for _, s := range tracks {
		entry := object{}
		if s.Codec != "" {
			entry["codec"] = s.Codec
		}
		switch s.Type {
		case "video":
			if s.Width > 0 {
				entry["width"] = s.Width
			}
			if s.Height > 0 {
				entry["height"] = s.Height
			}
			depth := s.BitDepth
			if s.BitsPerRawSample != nil && *s.BitsPerRawSample > 0 {
				depth = *s.BitsPerRawSample
			}
			if depth > 0 {
				entry["bitDepth"] = depth
			}
			if s.ColorTransfer != "" {
				entry["colorTransfer"] = s.ColorTransfer
			}
			profile := s.DolbyVisionProfile
			if s.ProbedDolbyVisionProfile != nil {
				profile = *s.ProbedDolbyVisionProfile
			}
			if profile > 0 {
				entry["dolbyVisionProfile"] = profile
			}
			if len(entry) > 0 {
				video = append(video, entry)
			}
		case "audio":
			if s.Channels > 0 {
				entry["channels"] = s.Channels
			}
			if s.Language != "" {
				entry["language"] = s.Language
			}
			if len(entry) > 0 {
				audio = append(audio, entry)
			}
		case "subtitle":
			entry["index"] = s.Index
			if s.Language != "" {
				entry["language"] = s.Language
			}
			if s.Title != "" {
				entry["title"] = s.Title
			}
			subtitles = append(subtitles, entry)
		}
	}
	if len(video) == 0 && len(audio) == 0 && len(subtitles) == 0 {
		return nil
	}
	summary := object{"video": video, "audio": audio}
	if len(subtitles) > 0 {
		summary["subtitles"] = subtitles
	}
	return summary
}

// detailChapters surfaces already-known chapters (probed container chapters,
// declared plugin chapters, or part-derived chapters) on title detail without
// triggering a network probe.
func detailChapters(d frontend.ItemDetail) []object {
	if len(d.Chapters) > 0 {
		return chapters(frontend.MediaInfo{Chapters: d.Chapters})
	}
	for _, v := range d.Versions {
		if !v.Media.Estimated && len(v.Media.Chapters) > 0 {
			return chapters(v.Media)
		}
	}
	return []object{}
}

func crewPeople(t frontend.Title) []frontend.Person {
	out := []frontend.Person{}
	seen := map[string]bool{}
	for _, group := range [][]frontend.Person{t.Crew, t.Directors, t.Writers} {
		for _, p := range group {
			if p.Name == "" {
				continue
			}
			identity := p.Name
			if p.TMDB > 0 {
				identity = "tmdb:" + strconv.FormatInt(p.TMDB, 10)
			} else if p.IMDB != "" {
				identity = "imdb:" + p.IMDB
			} else if p.ID != "" {
				identity = "id:" + p.ID
			}
			key := identity + "\x00" + p.Role
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, p)
		}
	}
	return out
}

// Detail children are bounded first pages in the neutral service's order.
// Subsequent selections use /titles/:id/children; no episode is inferred.
// For a branching container (e.g. artist -> album + concert), the season-like
// list is the primary child level only; secondary branches are queried via
// /titles/:id/children?role=.
func (i *instance) detailChildren(r *http.Request, v *frontend.Viewer, token string, d frontend.ItemDetail, out object) error {
	key := d.Title.Key
	isShow := key.Kind() == frontend.KindShow || d.Title.Type == frontend.TypeShow
	isSeason := key.Kind() == frontend.KindSeason || d.Title.Type == frontend.TypeSeason
	if !isShow && !isSeason {
		return nil
	}
	childQuery := frontend.ItemQuery{Limit: 50, NoTotal: true}
	if isShow && d.Title.PrimaryChildLevel != "" {
		childQuery.Level = d.Title.PrimaryChildLevel
	}
	page, err := i.host.Experience.Children(r.Context(), v, requestDevice(r), key, childQuery)
	if err != nil {
		return err
	}
	items := page.Items
	if isShow && d.Title.PrimaryChildLevel == "" {
		items = filterPrimaryBranchItems(d.Title, items)
	}
	converted, err := i.titles(r, v, token, items)
	if err != nil {
		return err
	}
	if isSeason {
		out["episodes"] = converted
		return nil
	}
	out["seasons"] = converted
	for _, season := range items {
		if season.Key.Kind() != frontend.KindSeason && season.Type != frontend.TypeSeason {
			continue
		}
		episodes, err := i.host.Experience.Children(r.Context(), v, requestDevice(r), season.Key, frontend.ItemQuery{Limit: 50, NoTotal: true})
		if err != nil {
			return err
		}
		converted, err = i.titles(r, v, token, episodes.Items)
		if err != nil {
			return err
		}
		out["episodes"] = converted
		break
	}
	return nil
}

// filterPrimaryBranchItems narrows a mixed-branch child slice to the parent's
// primary child level when PrimaryChildLevel was not pre-populated by the
// catalog (preferring items sharing the parent's media class, else the first
// observed child level).
func filterPrimaryBranchItems(parent frontend.Title, items []frontend.Title) []frontend.Title {
	if len(items) <= 1 {
		return items
	}
	var levels []string
	seenLevel := make(map[string]bool, 2)
	var matchingLevel string
	for _, it := range items {
		if it.KindLevel == "" {
			continue
		}
		if !seenLevel[it.KindLevel] {
			seenLevel[it.KindLevel] = true
			levels = append(levels, it.KindLevel)
		}
		if matchingLevel == "" && parent.MediaClass != "" && it.MediaClass == parent.MediaClass {
			matchingLevel = it.KindLevel
		}
	}
	if len(levels) > 1 {
		keep := matchingLevel
		if keep == "" {
			keep = levels[0]
		}
		out := make([]frontend.Title, 0, len(items))
		for _, it := range items {
			if it.KindLevel == keep {
				out = append(out, it)
			}
		}
		return out
	}
	if parent.MediaClass != "" {
		hasMatch, hasOther := false, false
		for _, it := range items {
			if it.MediaClass == "" {
				continue
			}
			if it.MediaClass == parent.MediaClass {
				hasMatch = true
			} else {
				hasOther = true
			}
		}
		if hasMatch && hasOther {
			out := make([]frontend.Title, 0, len(items))
			for _, it := range items {
				if it.MediaClass == parent.MediaClass {
					out = append(out, it)
				}
			}
			return out
		}
	}
	return items
}

func (i *instance) creditIDs(r *http.Request, v *frontend.Viewer, d frontend.ItemDetail) map[int64]string {
	var ids []int64
	seen := map[int64]bool{}
	for _, group := range [][]frontend.Person{d.Title.Cast, d.Title.Crew, d.Title.Directors, d.Title.Writers} {
		for _, person := range group {
			if person.TMDB > 0 && !seen[person.TMDB] {
				seen[person.TMDB] = true
				ids = append(ids, person.TMDB)
			}
		}
	}
	for _, credit := range d.Title.MediaCredits {
		if credit.TMDB > 0 && !seen[credit.TMDB] {
			seen[credit.TMDB] = true
			ids = append(ids, credit.TMDB)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	result := make(map[int64]string, len(ids))
	for start := 0; start < len(ids); start += 256 {
		allowed, err := i.host.Experience.PersonFacetIDs(r.Context(), v, requestDevice(r), d.Title.Key, ids[start:min(start+256, len(ids))])
		if err != nil {
			return nil
		}
		for id, value := range allowed {
			if seen[id] && value != "" {
				result[id] = value
			}
		}
	}
	return result
}

// Cast and crew share one current-credit proof and admission transaction.
func (i *instance) credits(r *http.Request, v *frontend.Viewer, token string, d frontend.ItemDetail) ([]object, []object, error) {
	facetIDs := i.creditIDs(r, v, d)
	people := append([]frontend.Person(nil), d.Title.Cast...)
	out := make([]object, 0, len(people))
	for _, person := range people {
		entry := object{"name": person.Name, "role": person.Role}
		if id := facetIDs[person.TMDB]; id != "" {
			entry["id"] = id
		}
		out = append(out, entry)
	}
	castCount := len(out)
	crew := crewPeople(d.Title)
	people = append(people, crew...)
	for _, person := range crew {
		entry := object{"name": person.Name}
		if id := facetIDs[person.TMDB]; id != "" {
			entry["id"] = id
		}
		if person.Role != "" {
			entry["role"] = person.Role
		}
		out = append(out, entry)
	}
	out, err := i.creditArtwork(r, token, d, people, out)
	if err != nil {
		return nil, nil, err
	}
	// Keep empty groups as JSON arrays.
	castOut, crewOut := []object{}, []object{}
	castOut = append(castOut, out[:castCount]...)
	crewOut = append(crewOut, out[castCount:]...)
	return castOut, crewOut, nil
}

// Each batch proves current credit membership and admits portrait grants;
// no person lookup or metadata refresh is needed here.
func (i *instance) creditArtwork(r *http.Request, token string, d frontend.ItemDetail, people []frontend.Person, out []object) ([]object, error) {
	portraits := map[string]frontend.Image{}
	if d.ImagesAuthoritative {
		for _, image := range d.Images {
			if image.Kind == frontend.ImagePerson && image.Subject != "" && image.Path != "" {
				if _, exists := portraits[image.Subject]; !exists {
					portraits[image.Subject] = image
				}
			}
		}
	}
	targets := []frontend.PersonImageTarget{}
	positions := []int{}
	for n, person := range people {
		if person.TMDB <= 0 {
			continue
		}
		if image, ok := portraits["tmdb:"+strconv.FormatInt(person.TMDB, 10)]; ok {
			targets = append(targets, frontend.PersonImageTarget{Item: d.Title.Key, Image: image})
			positions = append(positions, n)
		}
	}
	signer, ok := i.host.ImageGrants.(frontend.PersonImageGrantBatch)
	if !ok {
		return out, nil
	}
	for start := 0; start < len(targets); start += 100 {
		end := min(start+100, len(targets))
		secrets, err := signer.IssuePersonImages(r.Context(), token, requestDevice(r), targets[start:end])
		if err != nil {
			return nil, err
		}
		if len(secrets) != end-start {
			return nil, frontend.ErrNotAvailable
		}
		for n, secret := range secrets {
			if secret == "" {
				continue
			}
			image := targets[start+n].Image
			out[positions[start+n]]["artwork"] = object{"id": hash(image.Path), "kind": frontend.ImagePerson, "tag": image.Tag, "sizes": []object{imageSize(image, secret)}, "blurHash": image.BlurHash, "thumbHash": image.ThumbHash}
		}
	}
	return out, nil
}
