package livetv

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// artworkRefPrefix marks an On Air artwork reference that names a title's own
// poster or backdrop by key, for artwork that is not a bare TMDB image path
// (Plex-sourced art is stored as a sentinel and resolved per title).
const artworkRefPrefix = "title:"

// ArtworkRef returns the On Air reference for a programme image: a TMDB path
// unchanged, otherwise the title's artwork by key. element is "poster" or
// "art". An image with neither a path nor a title yields "".
func ArtworkRef(key core.RatingKey, path, element string) string {
	if path == "" {
		return ""
	}
	if artwork.ValidPath(path) {
		return path
	}
	if key == 0 {
		return ""
	}
	return artworkRefPrefix + key.String() + "/" + element
}

// ParseArtworkRef reverses ArtworkRef's title form.
func ParseArtworkRef(ref string) (core.RatingKey, string, bool) {
	rest, ok := strings.CutPrefix(ref, artworkRefPrefix)
	if !ok {
		return 0, "", false
	}
	raw, element, ok := strings.Cut(rest, "/")
	if !ok || (element != "poster" && element != "art") {
		return 0, "", false
	}
	key, err := core.ParseRatingKey(raw)
	if err != nil || key == 0 {
		return 0, "", false
	}
	return key, element, true
}

// enrichImportedArtwork memoizes bounded local title lookups during an
// import. Programme rows carry the result for guide and catch-up reads.
func (s *Service) enrichImportedArtwork(ctx context.Context, programmes []Programme) {
	catalog, ok := s.o.Catalog.(interface {
		GuideArtwork(context.Context, string, int, core.MediaType) (string, string, error)
	})
	if !ok || s.o.ImageURL == nil {
		return
	}
	type identity struct {
		title string
		year  int
		kind  core.MediaType
	}
	type images struct{ poster, backdrop string }
	memo := make(map[identity]images)
	const maxLookups = 2048
	for i := range programmes {
		item := &programmes[i].Item
		if item.Thumb != "" || item.Key != 0 || item.Slate != "" || item.Live ||
			frontend.LiveCategory(item.Genres, "sports", item.MediaType) ||
			frontend.LiveCategory(item.Genres, "news", item.MediaType) {
			continue
		}
		title := strings.TrimSpace(item.Title)
		var kind core.MediaType
		switch item.MediaType {
		case "movie":
			kind = core.TypeMovie
		case "episode", "show":
			kind = core.TypeShow
			if item.ShowTitle != "" {
				title = strings.TrimSpace(item.ShowTitle)
			}
		case "":
		default:
			continue
		}
		if title == "" {
			continue
		}
		key := identity{title, item.Year, kind}
		image, exists := memo[key]
		if !exists && len(memo) < maxLookups {
			poster, backdrop, err := catalog.GuideArtwork(ctx, title, item.Year, kind)
			if err != nil {
				s.o.Logger.Debug("catalogue guide artwork lookup failed", "err", err)
				return
			}
			if artwork.ValidPath(poster) {
				image.poster = s.o.ImageURL(poster, "w780")
			}
			if artwork.ValidPath(backdrop) {
				image.backdrop = s.o.ImageURL(backdrop, "w1280")
			}
			memo[key] = image
		}
		item.Thumb = image.poster
		if item.Art == "" {
			item.Art = image.backdrop
		}
	}
}
