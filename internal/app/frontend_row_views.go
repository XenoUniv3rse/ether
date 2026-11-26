package app

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/livetv"
)

func (a *App) newRowViews(protocol core.Protocol, rows *experience.RowsService, receipts *experience.Receipts, visibility experience.Visibility, libraries []experience.Library, live ...func() []experience.Library) (*experience.RowViewService, error) {
	var liveFn func() []experience.Library
	if len(live) > 0 {
		liveFn = live[0]
	}
	d := experience.RuntimeRowViews{DB: a.db, PendingWatch: a.state.PendingWatchRevision, Protocol: protocol, Receipts: receipts, Visibility: visibility, Libraries: libraries, LiveLibraries: liveFn, Active: a.users.PolicyUser, CachedGet: a.catalog.CachedGet, CachedGetMany: a.catalog.CachedGetMany, Playlists: a.playlists, Situation: situationFor,
		Snapshot: func(ctx context.Context, request frontend.RowRequest, situation core.HomeSituation) ([]frontend.Row, error) {
			return rows.SnapshotRows(context.WithValue(ctx, experienceSituationKey{}, situation), request, protocol)
		}}
	if a.collections != nil {
		d.Collections = a.collections
	}
	if a.availability != nil {
		d.CurrentAvailable = a.availability.FilterDiscoveryCurrent
	}
	if a.episodeorder != nil {
		d.CachedVirtualSeason = a.episodeorder.CachedVirtualSeason
	}
	if a.livetv != nil {
		d.Live = liveForApp{svc: a.livetv, app: string(protocol)}
	}
	return experience.NewRuntimeRowViews(d)
}

type rowViewCollectionSource = experience.RowViewCollectionSource

func rowViewCollectionMembers(ctx context.Context, source rowViewCollectionSource, user *core.User, collection core.RatingKey, refs []core.NeutralEntryRef) bool {
	return experience.RowViewCollectionMembers(ctx, source, user, collection, refs)
}

// liveForApp applies the owner's per-app channel visibility to Home rows.
type liveForApp struct {
	svc *livetv.Service
	app string
}

func (l liveForApp) AllowedChannels(ctx context.Context, u *core.User) ([]livetv.Channel, error) {
	return l.svc.AllowedChannels(core.WithLiveApp(ctx, l.app), u)
}

func (l liveForApp) OnNowForIDs(ctx context.Context, u *core.User, now, until time.Time, ids []string) ([]livetv.Channel, []livetv.Programme, error) {
	return l.svc.OnNowForIDs(core.WithLiveApp(ctx, l.app), u, now, until, ids)
}
