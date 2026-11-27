package frontendtest

import (
	"context"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// CatchUpLive is a single-airing fixture for native frontend routing tests.
type CatchUpLive struct {
	frontend.LiveTV
	Item   frontend.CatchUpItem
	Opened frontend.CatchUpRequest
}

func NewCatchUpLive() *CatchUpLive {
	programme := frontend.LiveTVProgramme{
		ID: "channel-1700000000000", ChannelID: "channel",
		Start: time.Unix(1700000000, 0).UTC(), End: time.Unix(1700003600, 0).UTC(),
		Item: frontend.LiveTVItem{Title: "News"},
	}
	return &CatchUpLive{Item: frontend.CatchUpItem{
		ID: frontend.CatchUpEpisodeID(programme.ID), Type: frontend.TypeEpisode, Programme: programme,
		ShowID:   frontend.CatchUpGroupID(frontend.TypeShow, programme.Item.Title),
		SeasonID: frontend.CatchUpGroupID(frontend.TypeSeason, programme.Item.Title),
	}}
}

func (*CatchUpLive) Enabled() bool                                  { return true }
func (*CatchUpLive) NoteClientActivity()                            {}
func (*CatchUpLive) VisibleTo(context.Context, *frontend.User) bool { return true }
func (*CatchUpLive) CatchUpLibraryVisible(context.Context, *frontend.Viewer) (bool, error) {
	return true, nil
}
func (f *CatchUpLive) CatchUpItems(_ context.Context, _ *frontend.Viewer, q frontend.CatchUpQuery) (frontend.CatchUpPage, error) {
	item := f.Item
	if _, kind := frontend.CatchUpGUIDGroup(q.Parent); !q.Recursive && kind != frontend.TypeSeason {
		item.Type, item.ID = frontend.TypeShow, item.ShowID
		if kind == frontend.TypeShow {
			item.Type, item.ID = frontend.TypeSeason, item.SeasonID
		}
	}
	return frontend.CatchUpPage{Items: []frontend.CatchUpItem{item}, Total: 1}, nil
}
func (f *CatchUpLive) CatchUpItem(_ context.Context, _ *frontend.Viewer, id string) (frontend.CatchUpItem, error) {
	if id != f.Item.ID && id != frontend.CatchUpGUID(f.Item) {
		return frontend.CatchUpItem{}, frontend.ErrNotFound
	}
	return f.Item, nil
}

// CatchUpProgress keeps the latest position as the airing's resume point.
func (f *CatchUpLive) CatchUpProgress(_ context.Context, _ *frontend.Viewer, id string, offset, _ time.Duration, _ string) error {
	if id != f.Item.ID && id != frontend.CatchUpGUID(f.Item) {
		return frontend.ErrNotFound
	}
	f.Item.Watch.ViewOffset = offset
	return nil
}

// CatchUpSetWatched counts a viewing or clears the airing's state.
func (f *CatchUpLive) CatchUpSetWatched(_ context.Context, _ *frontend.Viewer, id string, watched bool) error {
	if id != f.Item.ID && id != frontend.CatchUpGUID(f.Item) {
		return frontend.ErrNotFound
	}
	f.Item.Watch.ViewOffset = 0
	if watched {
		f.Item.Watch.ViewCount++
	} else {
		f.Item.Watch.ViewCount = 0
	}
	return nil
}

func (f *CatchUpLive) StartCatchUp(_ context.Context, _ *frontend.User, req frontend.CatchUpRequest, _ frontend.ClientCaps, _ string) (frontend.CatchUpInfo, error) {
	f.Opened = req
	info, _ := f.LibraryCatchUpSession(frontend.SessionAccess{}, "archive-session")
	return info, nil
}
func (*CatchUpLive) SeekCatchUp(context.Context, *frontend.User, string, time.Duration) (frontend.CatchUpInfo, error) {
	return frontend.CatchUpInfo{}, frontend.ErrNotFound
}
func (*CatchUpLive) TouchLiveSession(context.Context, *frontend.User, string) error { return nil }
func (*CatchUpLive) CatchUpLiveWindow(context.Context, string) (frontend.LiveWindow, error) {
	return frontend.LiveWindow{}, frontend.ErrNotFound
}
func (*CatchUpLive) ServeCatchUpTS(http.ResponseWriter, *http.Request, string, *frontend.User) {}
func (f *CatchUpLive) LibraryCatchUpSession(_ frontend.SessionAccess, id string) (frontend.CatchUpInfo, bool) {
	p := f.Item.Programme
	return frontend.CatchUpInfo{ID: id, ChannelID: p.ChannelID, ProgrammeID: p.ID,
		Start: p.Start, End: p.End, MediaGeneration: "archive-generation"}, id == "archive-session" && f.Opened.ProgrammeID != ""
}
func (*CatchUpLive) ServePlaylist(w http.ResponseWriter, _ *http.Request, _ string, _ *frontend.User) {
	_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:4,\n0.ts\n#EXT-X-ENDLIST\n"))
}
