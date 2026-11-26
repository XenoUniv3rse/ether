package watchlist

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

func (s *Service) Hubs(ctx context.Context, u *core.User, limit int, language string) ([]core.Hub, error) {
	cfg, err := s.settings(ctx)
	if err != nil || !cfg.Enabled {
		return nil, err
	}
	if u == nil {
		return nil, core.ErrUnauthorized
	}
	items, err := s.itemsForUser(ctx, u.ID, u, 0, true)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	if limit <= 0 {
		limit = 12
	}

	labels := watchlistLabels(language)
	now := s.o.Now()
	var out []core.Hub
	for _, row := range s.homeRows(ctx, u) {
		name := strings.TrimSpace(row.Name)
		if name == "" {
			name = labels.name
		}
		id := watchlistRowID(row)
		unified := core.Hub{ID: id, Title: name, Context: "hub.home.watchlist", Promoted: true, ItemBadges: map[core.RatingKey]string{}}
		waiting := core.Hub{ID: id + ".coming", Title: labels.split(name, labels.waiting), Context: unified.Context, Promoted: true}
		ready := core.Hub{ID: id + ".available", Title: labels.split(name, labels.ready), Context: "hub.home.watchlist.available", Promoted: true}
		split := splitByAvailability(row)
		for _, item := range items {
			hub := &unified
			available := !item.AvailableAt.IsZero() && item.Title.IsReleased(now)
			if split {
				hub = &waiting
				if available {
					hub = &ready
				}
			}
			if len(hub.Items) == limit {
				hub.More = true
				continue
			}
			hub.Items = append(hub.Items, item.Title)
			if !split && !available {
				hub.ItemBadges[item.Title.Key] = labels.badge
			}
		}
		for _, hub := range []core.Hub{unified, ready, waiting} {
			if len(hub.Items) > 0 {
				out = append(out, hub)
			}
		}
	}
	return out, nil
}

func (s *Service) homeRows(ctx context.Context, viewer *core.User) []core.Row {
	if s.o.Rows != nil {
		if rows := s.o.Rows(ctx, viewer); len(rows) > 0 {
			return rows
		}
	}
	return []core.Row{{ID: "saved"}}
}

func watchlistRowID(row core.Row) string { return "watchlist." + row.ID }
func splitByAvailability(row core.Row) bool {
	return row.Source.Param(core.WatchlistSplitParam, "false") == "true"
}

// HomeHubIDs names the shelves whose contents may change on a transition.
func (s *Service) HomeHubIDs(ctx context.Context, viewer *core.User) []string {
	var ids []string
	for _, row := range s.homeRows(ctx, viewer) {
		id := watchlistRowID(row)
		if splitByAvailability(row) {
			ids = append(ids, id+".available", id+".coming")
		} else {
			ids = append(ids, id)
		}
	}
	return ids
}
