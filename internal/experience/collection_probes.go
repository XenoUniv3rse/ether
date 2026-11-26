package experience

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type collectionProbe struct {
	has   bool
	total int
}

func (s *Service) collectionVisibleProbes(ctx context.Context, v *core.User, cols []core.Collection) (map[core.RatingKey]collectionProbe, error) {
	out := make(map[core.RatingKey]collectionProbe, len(cols))
	err := s.visitVisibleCollectionPages(ctx, v, cols, 32, func(pages map[core.RatingKey]core.Page) ([]core.RatingKey, error) {
		var next []core.RatingKey
		for key, page := range pages {
			out[key] = collectionProbe{has: len(page.Items) > 0, total: page.Total}
			if len(page.Items) == 0 {
				next = append(next, key)
			}
		}
		return next, nil
	})
	if err != nil {
		return nil, err
	}
	for _, c := range cols {
		if _, ok := out[c.Key]; !ok {
			has, total, err := s.collectionVisibleProbe(ctx, v, c)
			if err != nil {
				return nil, err
			}
			out[c.Key] = collectionProbe{has, total}
		}
	}
	return out, nil
}

func (s *Service) visitVisibleCollectionPages(ctx context.Context, v *core.User, cols []core.Collection, size int, visit func(map[core.RatingKey]core.Page) ([]core.RatingKey, error)) error {
	var keys []core.RatingKey
	byKey := make(map[core.RatingKey]core.Collection, len(cols))
	for _, c := range cols {
		if _, ok := byKey[c.Key]; !ok {
			keys = append(keys, c.Key)
			byKey[c.Key] = c
		}
	}
	if batch, ok := s.d.Collections.(interface {
		VisitCollectionMemberPages(context.Context, *core.User, []core.RatingKey, int, func(map[core.RatingKey]core.Page) ([]core.RatingKey, error)) error
	}); ok && len(keys) > 0 {
		return batch.VisitCollectionMemberPages(ctx, v, keys, size, func(pages map[core.RatingKey]core.Page) ([]core.RatingKey, error) {
			var released, upcoming []core.Title
			for _, key := range keys {
				if byKey[key].Upcoming {
					upcoming = append(upcoming, pages[key].Items...)
				} else {
					released = append(released, pages[key].Items...)
				}
			}
			allowed := make(map[core.RatingKey]bool)
			if len(released) > 0 {
				visible, err := s.allowTitles(ctx, v, released)
				if err != nil {
					return nil, err
				}
				for _, title := range visible {
					allowed[title.Key] = true
				}
			}
			upcomingAllowed := make(map[core.RatingKey]bool)
			for _, title := range s.d.Visibility.UpcomingItems(ctx, v, upcoming, time.Now) {
				upcomingAllowed[title.Key] = true
			}
			for key, page := range pages {
				visible := allowed
				if byKey[key].Upcoming {
					visible = upcomingAllowed
				}
				items := make([]core.Title, 0, len(page.Items))
				for _, title := range page.Items {
					if visible[title.Key] {
						items = append(items, title)
					}
				}
				page.Items = items
				pages[key] = page
			}
			return visit(pages)
		})
	}
	return nil
}

// collectionItemsBatch shares exact membership reads for a filtered library.
// Keep each collection's member order and deduplicate as collectionItemsFor does.
func (s *Service) collectionItemsBatch(ctx context.Context, v *core.User, cols []core.Collection) (map[core.RatingKey][]core.Title, error) {
	out := make(map[core.RatingKey][]core.Title, len(cols))
	seen := make(map[core.RatingKey]map[core.RatingKey]bool, len(cols))
	err := s.visitVisibleCollectionPages(ctx, v, cols, 128, func(pages map[core.RatingKey]core.Page) ([]core.RatingKey, error) {
		next := make([]core.RatingKey, 0, len(pages))
		for key, page := range pages {
			if seen[key] == nil {
				seen[key] = make(map[core.RatingKey]bool)
				out[key] = nil
			}
			for _, title := range page.Items {
				if !seen[key][title.Key] {
					seen[key][title.Key] = true
					out[key] = append(out[key], title)
				}
			}
			next = append(next, key)
		}
		return next, nil
	})
	if err != nil {
		return nil, err
	}
	for _, c := range cols {
		if _, ok := out[c.Key]; !ok {
			out[c.Key], err = s.collectionItemsFor(ctx, v, c.Key, c.Upcoming)
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
