package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
)

func (a *App) extraHomeRows(ctx context.Context, u *core.User, hc core.HubContext, section string, limit int) []core.Hub {
	rows := experience.PreviewRows{}
	if a.availability != nil {
		rows.Availability = a.availability.Hubs
	}
	if a.personalize != nil {
		rows.Filter = a.personalize.FilterRecommendations
	}
	if a.watchlist != nil {
		rows.Watchlist = func(ctx context.Context, viewer *core.User, limit int) ([]core.Hub, error) {
			return a.watchlist.Hubs(ctx, viewer, limit, hc.Language)
		}
	}
	if a.discoveryPersonal != nil {
		rows.Personal = a.discoveryPersonal.Hubs
	}
	if a.livetv != nil {
		rows.Live = a.livetv.HomeHub
	}
	if a.registrar != nil {
		rows.LiveEnabled = func() bool { return a.registrar.Status().PlexPass }
	}
	return rows.Rows(ctx, u, hc, section, limit)
}
