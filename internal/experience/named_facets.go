package experience

import (
	"context"
	"errors"
	"fmt"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/reqmemo"
)

// NamedFacetIDs maps exact credited genre or studio names on one visible title
// to the same identifier namespace as library facets.
func (s *Service) NamedFacetIDs(ctx context.Context, viewer *core.Viewer, _ core.Device, key core.RatingKey, kind string, names []string) (map[string]string, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if kind != "genre" && kind != "studio" {
		return nil, errors.New("experience: unsupported facet")
	}
	if len(names) > 256 {
		return nil, errors.New("experience: too many facet names")
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		if name == "" || len(name) > 1024 {
			return nil, errors.New("experience: invalid facet name")
		}
		wanted[name] = true
	}
	active, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	// Facet identifiers require current membership and policy facts, not a
	// title's credits, trailers or alternate names.
	facts := core.FactGenres
	if kind == "studio" {
		facts = core.FactStudios
	}
	loaded, err := reqmemo.Do(ctx, namedFacetMemoKey(s, active.ID, key, kind), func() (namedFacetTitle, error) {
		var title *core.Title
		rows, err := s.visibilityRows(ctx, active, []core.RatingKey{key}, facts)
		if err != nil {
			return namedFacetTitle{}, err
		}
		if rows != nil {
			if row, ok := rows[key]; ok {
				title = &row
			}
			if title == nil || title.Key != key || !s.visible(ctx, active, title) {
				err = core.ErrNotFound
			}
		} else if s.d.CachedGet != nil {
			title, err = s.d.CachedGet(ctx, key)
			if err == nil && (title == nil || title.Key != key || !s.visible(ctx, active, title)) {
				err = core.ErrNotFound
			}
		} else {
			title, err = s.item(ctx, active, key)
		}
		if err != nil {
			return namedFacetTitle{}, err
		}
		return namedFacetTitle{title}, nil
	})
	if err != nil {
		return nil, err
	}
	title := loaded.title
	values := title.Genres
	if kind == "studio" {
		values = title.Studios
	}
	out := make(map[string]string)
	seen := map[string]string{}
	for _, name := range values {
		if name == "" {
			continue
		}
		id := facetID(kind, name)
		if previous, ok := seen[id]; ok && previous != name {
			return nil, errors.New("experience: facet identifier collision")
		}
		seen[id] = name
		if wanted[name] {
			out[name] = id
		}
	}
	return out, nil
}

type namedFacetTitle struct{ title *core.Title }

func namedFacetMemoKey(s *Service, viewer int64, key core.RatingKey, kind string) string {
	return fmt.Sprintf("named-facet-title:%p:%d:%d:%s", s, viewer, key, kind)
}

// PrefetchNamedFacetIDs authorizes one page with the same current membership,
// visibility and fact checks as NamedFacetIDs. Only this read request retains
// the partial rows; scalar calls keep their existing full-title fallback.
func (s *Service) PrefetchNamedFacetIDs(ctx context.Context, viewer *core.Viewer, keys []core.RatingKey, kind string) error {
	if len(keys) > 256 {
		return errors.New("experience: too many facet titles")
	}
	if !reqmemo.Active(ctx) || len(keys) == 0 {
		return nil
	}
	var facts core.TitleFacts
	switch kind {
	case "genre":
		facts = core.FactGenres
	case "studio":
		facts = core.FactStudios
	default:
		return errors.New("experience: unsupported facet")
	}
	ctx = core.WithCatalogViewer(ctx, viewer)
	active, err := s.viewer(ctx, viewer)
	if err != nil {
		return err
	}
	rows, err := s.visibilityRows(ctx, active, keys, facts)
	if err != nil {
		return err
	}
	if rows == nil {
		return nil
	}
	titles := make([]core.Title, 0, len(rows))
	for key, row := range rows {
		if row.Key == key {
			titles = append(titles, row)
		}
	}
	members := s.sectionOrFilterAllowedBatch(ctx, active, titles, s.d.Visibility.SectionFor)
	visibility := withoutSectionCheck(s.d.Visibility)
	for _, title := range titles {
		key := title.Key
		if !members[key] || !visibility.Visible(ctx, active, &title) {
			continue
		}
		if _, err := reqmemo.Do(ctx, namedFacetMemoKey(s, active.ID, key, kind), func() (namedFacetTitle, error) { return namedFacetTitle{&title}, nil }); err != nil {
			return err
		}
	}
	return nil
}
