package plex

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

type catchUpTestLive struct {
	frontend.LiveTV
	item   frontend.CatchUpItem
	opened frontend.CatchUpRequest
}

func (*catchUpTestLive) Enabled() bool                                  { return true }
func (*catchUpTestLive) VisibleTo(context.Context, *frontend.User) bool { return true }
func (*catchUpTestLive) ViewingSessions() []*frontend.PlaySession       { return nil }
func (*catchUpTestLive) CatchUpLibraryVisible(context.Context, *frontend.Viewer) (bool, error) {
	return true, nil
}
func (f *catchUpTestLive) CatchUpItems(context.Context, *frontend.Viewer, frontend.CatchUpQuery) (frontend.CatchUpPage, error) {
	return frontend.CatchUpPage{Items: []frontend.CatchUpItem{f.item}, Total: 1}, nil
}
func (f *catchUpTestLive) CatchUpItem(_ context.Context, _ *frontend.Viewer, id string) (frontend.CatchUpItem, error) {
	if id != f.item.ID {
		return frontend.CatchUpItem{}, frontend.ErrNotFound
	}
	return f.item, nil
}
func (f *catchUpTestLive) StartCatchUp(_ context.Context, _ *frontend.User, req frontend.CatchUpRequest, _ frontend.ClientCaps, _ string) (frontend.CatchUpInfo, error) {
	f.opened = req
	return frontend.CatchUpInfo{ID: "archive-session", Start: f.item.Programme.Start, End: f.item.Programme.End}, nil
}
func (*catchUpTestLive) SeekCatchUp(context.Context, *frontend.User, string, time.Duration) (frontend.CatchUpInfo, error) {
	return frontend.CatchUpInfo{}, frontend.ErrNotFound
}
func (*catchUpTestLive) TouchLiveSession(context.Context, *frontend.User, string) error { return nil }
func (*catchUpTestLive) CatchUpLiveWindow(context.Context, string) (frontend.LiveWindow, error) {
	return frontend.LiveWindow{}, frontend.ErrNotFound
}
func (*catchUpTestLive) ServeCatchUpTS(http.ResponseWriter, *http.Request, string, *frontend.User) {}
func (*catchUpTestLive) ServePlaylist(w http.ResponseWriter, _ *http.Request, _ string, _ *frontend.User) {
	_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:4,\n0.ts\n#EXT-X-ENDLIST\n"))
}

func (f *catchUpTestLive) LibraryCatchUpSession(access frontend.SessionAccess, id string) (frontend.CatchUpInfo, bool) {
	return frontend.CatchUpInfo{ID: id}, access.Viewer != nil && access.Protocol == "plex" && id == "archive-session" && f.opened.ProgrammeID != ""
}
func (*catchUpTestLive) ServeResource(w http.ResponseWriter, _ *http.Request, _, file string, _ *frontend.User) {
	if file != "0.ts" {
		http.NotFound(w, nil)
		return
	}
	_, _ = w.Write([]byte("segment"))
}

func (*catchUpTestLive) ServeDASH(w http.ResponseWriter, _ *http.Request, _ string, _ *frontend.User) error {
	w.Header().Set("Content-Type", "application/dash+xml")
	_, _ = w.Write([]byte(`<MPD type="static"/>`))
	return nil
}

func TestCatchUpSectionPlayback(t *testing.T) {
	f := newFixture()
	t.Cleanup(func() { _ = f.srv.Close() })
	start := time.Unix(1700000000, 0).UTC()
	programme := "channel-1700000000000"
	live := &catchUpTestLive{item: frontend.CatchUpItem{
		ID: frontend.CatchUpEpisodeID(programme), Type: frontend.TypeEpisode,
		Programme: frontend.LiveTVProgramme{ID: programme, ChannelID: "channel", Start: start, End: start.Add(time.Hour)},
	}}
	f.srv.setLiveTV(live)
	if got := f.request(t, "GET", "/library/sections", nil); got.Code != 200 || !strings.Contains(got.Body.String(), "Catch-up") {
		t.Fatalf("section list: %d", got.Code)
	}
	result := f.request(t, "GET", "/library/sections/2147483646/allLeaves", map[string]string{"Accept": "application/json"})
	if result.Code != 200 {
		t.Fatalf("catch-up browse: %d", result.Code)
	}
	key := decodeJSON(t, result)["Metadata"].([]any)[0].(map[string]any)["key"].(string)
	path := url.Values{"path": {key}, "session": {"archive-test"}}.Encode()
	if got := f.request(t, "GET", "/video/:/transcode/universal/decision?"+path, nil); got.Code != 200 || live.opened.ChannelID != "" {
		t.Fatalf("catch-up decision: %d", got.Code)
	}
	got := f.request(t, "GET", "/video/:/transcode/universal/start.m3u8?"+path, nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), "#EXT-X-ENDLIST") || live.opened.ChannelID != "channel" || live.opened.ProgrammeID != programme {
		t.Fatalf("catch-up start: %d", got.Code)
	}
	for _, line := range strings.Split(got.Body.String(), "\n") {
		if strings.HasPrefix(line, "/livetv/sessions/") {
			segment := f.request(t, "GET", line, nil)
			if segment.Code != http.StatusOK || segment.Body.String() != "segment" {
				t.Fatalf("catch-up segment: %d", segment.Code)
			}
			break
		}
	}
	got = f.request(t, "GET", "/video/:/transcode/universal/start.mpd?"+path, nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), `type="static"`) || !live.opened.DASH || live.opened.ProgrammeID != programme {
		t.Fatalf("catch-up DASH start: %d", got.Code)
	}

}

func (*catchUpTestLive) NoteClientActivity() {}

type catchUpSortTestLive struct {
	catchUpTestLive
	got frontend.CatchUpQuery
}

func (f *catchUpSortTestLive) CatchUpItems(_ context.Context, _ *frontend.Viewer, q frontend.CatchUpQuery) (frontend.CatchUpPage, error) {
	f.got = q
	return frontend.CatchUpPage{Items: []frontend.CatchUpItem{f.item}, Total: 1}, nil
}

func TestCatchUpSectionSorts(t *testing.T) {
	f := newFixture()
	t.Cleanup(func() { _ = f.srv.Close() })
	start := time.Unix(1700000000, 0).UTC()
	live := &catchUpSortTestLive{catchUpTestLive: catchUpTestLive{item: frontend.CatchUpItem{
		ID: frontend.CatchUpGroupID(frontend.TypeShow, "News"), Type: frontend.TypeShow,
		Programme: frontend.LiveTVProgramme{ID: "channel-1700000000000", ChannelID: "channel", Start: start, End: start.Add(time.Hour), Item: frontend.LiveTVItem{Title: "News"}},
	}}}
	f.srv.setLiveTV(live)
	for raw, want := range map[string]frontend.CatchUpSort{
		"":                          frontend.CatchUpSortRecent,
		"titleSort":                 frontend.CatchUpSortTitle,
		"titleSort:desc":            frontend.CatchUpSortTitleDesc,
		"originallyAvailableAt":     frontend.CatchUpSortRecent,
		"originallyAvailableAt:asc": frontend.CatchUpSortOldest,
		"rating:desc":               frontend.CatchUpSortRecent,
	} {
		if got := f.request(t, "GET", "/library/sections/2147483646/all?"+url.Values{"sort": {raw}}.Encode(), nil); got.Code != http.StatusOK || live.got.Sort != want {
			t.Fatalf("sort %q: %d, got %q want %q", raw, got.Code, live.got.Sort, want)
		}
	}
	sorts := f.request(t, "GET", "/library/sections/2147483646/sorts", map[string]string{"Accept": "application/json"})
	body := sorts.Body.String()
	if sorts.Code != http.StatusOK || !strings.Contains(body, `"Date Aired"`) || !strings.Contains(body, `"titleSort"`) || strings.Contains(body, `"rating"`) {
		t.Fatalf("catch-up sorts: %d %s", sorts.Code, body)
	}
	meta := f.request(t, "GET", "/library/sections/2147483646/all?includeMeta=1&sort=titleSort", map[string]string{"Accept": "application/json"})
	if !strings.Contains(meta.Body.String(), `"Date Aired"`) || strings.Contains(meta.Body.String(), `"Genre"`) {
		t.Fatalf("catch-up meta: %s", meta.Body.String())
	}
}
