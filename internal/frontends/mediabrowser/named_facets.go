package mediabrowser

import (
	"context"
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// addNamedFacets uses only IDs issued for fields on this authorized title.
func (i *instance) addNamedFacets(r *http.Request, viewer *frontend.Viewer, detail frontend.ItemDetail, dto map[string]any) {
	for _, group := range []struct {
		kind, field string
		names       []string
	}{
		{"genre", "GenreItems", detail.Title.Genres}, {"studio", "Studios", detail.Title.Studios},
	} {
		names := make([]string, 0, min(len(group.names), 256))
		seen := map[string]bool{}
		for _, name := range group.names {
			if name == "" || seen[name] {
				continue
			}
			if len(names) == 256 {
				break
			}
			seen[name] = true
			names = append(names, name)
		}
		if len(names) == 0 {
			continue
		}
		authorized, err := i.host.Experience.NamedFacetIDs(r.Context(), viewer, device(r), detail.Title.Key, group.kind, names)
		if err != nil {
			continue
		}
		var compact map[string]int32
		if i.protocol == "emby" {
			refs := make([]string, 0, len(authorized))
			for _, name := range names {
				if id := authorized[name]; id != "" {
					refs = append(refs, id)
				}
			}
			compact = make(map[string]int32, len(refs))
			for start := 0; start < len(refs); start += 250 {
				allocated, err := i.host.IDs.CompactBatch(r.Context(), "emby", "facet-"+group.kind, refs[start:min(start+250, len(refs))])
				if err != nil {
					continue
				}
				for ref, id := range allocated {
					compact[ref] = id
				}
			}
		}
		result := make([]any, 0, len(names))
		for _, name := range names {
			id := authorized[name]
			if id == "" {
				continue
			}
			var wireID any = id
			if compact != nil {
				n := compact[id]
				if n <= 0 {
					continue
				}
				wireID = n
			}
			result = append(result, map[string]any{"Id": wireID, "Name": name})
		}
		if len(result) > 0 {
			dto[group.field] = result
		}
	}
}

// Prefetch the same authorized IDs used by shapeJellyfinItem in a page-sized
// batch. Non-catalogue kinds still use the scalar host fallback.
func (i *instance) prefetchGenreItems(r *http.Request, viewer *frontend.Viewer, titles []frontend.Title) {
	if i.protocol != "jellyfin" || (!fieldSet(r)["genres"] && jellyfinListFile(r)) {
		return
	}
	facets, ok := i.host.Experience.(interface {
		PrefetchNamedFacetIDs(context.Context, *frontend.Viewer, []frontend.RatingKey, string) error
	})
	if !ok {
		return
	}
	keys := make([]frontend.RatingKey, 0, len(titles))
	for _, title := range titles {
		if len(title.Genres) > 0 && (title.Key.Kind() == frontend.KindMovie || title.Key.Kind() == frontend.KindShow) {
			keys = append(keys, title.Key)
		}
	}
	for start := 0; start < len(keys); start += 256 {
		_ = facets.PrefetchNamedFacetIDs(r.Context(), viewer, keys[start:min(start+256, len(keys))], "genre")
	}
}
