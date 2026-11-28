package plex

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
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

// Plex for iOS picks the item to play from playQueueSelectedMetadataItemID;
// without it the app stops after creating the queue and never asks for a
// decision. A catch-up queue must name its airing like any other queue.
func TestCatchUpPlayQueueNamesSelectedItem(t *testing.T) {
	f := newFixture()
	t.Cleanup(func() { _ = f.srv.Close() })
	start := time.Unix(1700000000, 0).UTC()
	programme := "channel-1700000000000"
	id := frontend.CatchUpEpisodeID(programme)
	f.srv.setLiveTV(&catchUpTestLive{item: frontend.CatchUpItem{
		ID: id, Type: frontend.TypeEpisode,
		Programme: frontend.LiveTVProgramme{ID: programme, ChannelID: "channel", Start: start, End: start.Add(time.Hour)},
	}})
	uri := "server://" + f.srv.identity().MachineIdentifier + "/com.plexapp.plugins.library/library/metadata/" + id
	created := f.request(t, "POST", "/playQueues?"+url.Values{"type": {"video"}, "uri": {uri}}.Encode(), map[string]string{"Accept": "application/json"})
	if created.Code != http.StatusOK {
		t.Fatalf("create catch-up queue: %d", created.Code)
	}
	mc := decodeJSON(t, created)
	if mc["playQueueSelectedMetadataItemID"] != id {
		t.Fatalf("created queue selects %v, want %s", mc["playQueueSelectedMetadataItemID"], id)
	}
	reread := f.request(t, "GET", fmt.Sprintf("/playQueues/%v", mc["playQueueID"]), map[string]string{"Accept": "application/json"})
	if reread.Code != http.StatusOK || decodeJSON(t, reread)["playQueueSelectedMetadataItemID"] != id {
		t.Fatalf("re-read catch-up queue: %d", reread.Code)
	}
}

type catchUpWatchTestLive struct {
	catchUpTestLive
	progress []time.Duration
	states   []string
}

func (f *catchUpWatchTestLive) CatchUpProgress(_ context.Context, _ *frontend.Viewer, id string, offset, duration time.Duration, state string) error {
	if id != f.item.ID {
		return frontend.ErrNotFound
	}
	f.progress, f.states = append(f.progress, offset, duration), append(f.states, state)
	f.item.Watch.ViewOffset = offset
	return nil
}

// CatchUpItems and CatchUpItem return a show's roll-up only when asked, as
// the real host does, so a test fails if the frontend stops asking.
func (f *catchUpWatchTestLive) CatchUpItems(ctx context.Context, v *frontend.Viewer, q frontend.CatchUpQuery) (frontend.CatchUpPage, error) {
	page, err := f.catchUpTestLive.CatchUpItems(ctx, v, q)
	for i := range page.Items {
		if page.Items[i].Type != frontend.TypeEpisode && !q.Rollup {
			page.Items[i].Watch = frontend.CatchUpWatch{}
		}
	}
	return page, err
}

func (f *catchUpWatchTestLive) CatchUpItem(ctx context.Context, v *frontend.Viewer, id string) (frontend.CatchUpItem, error) {
	item, err := f.catchUpTestLive.CatchUpItem(ctx, v, id)
	if item.Type != frontend.TypeEpisode {
		item.Watch = frontend.CatchUpWatch{}
	}
	return item, err
}

func (f *catchUpWatchTestLive) CatchUpGroupWatch(_ context.Context, _ *frontend.Viewer, id string) (frontend.CatchUpWatch, error) {
	if id != f.item.ID || f.item.Type == frontend.TypeEpisode {
		return frontend.CatchUpWatch{}, frontend.ErrNotFound
	}
	return f.item.Watch, nil
}

func (f *catchUpWatchTestLive) CatchUpSetWatched(_ context.Context, _ *frontend.Viewer, id string, watched bool) error {
	if id != f.item.ID {
		return frontend.ErrNotFound
	}
	if f.item.Type != frontend.TypeEpisode {
		f.item.Watch.Watched, f.item.Watch.InProgress = 0, 0
		if watched {
			f.item.Watch.Watched = f.item.Watch.Episodes
		}
		return nil
	}
	f.item.Watch.ViewOffset = 0
	if watched {
		f.item.Watch.ViewCount++
	} else {
		f.item.Watch.ViewCount = 0
	}
	return nil
}

// Plex timelines, Mark as Watched and Mark as Unwatched reach a catch-up
// airing's watch state, and the airing's metadata shows it.
func TestCatchUpWatchStateThroughPlex(t *testing.T) {
	f := newFixture()
	t.Cleanup(func() { _ = f.srv.Close() })
	start := time.Unix(1700000000, 0).UTC()
	programme := "channel-1700000000000"
	id := frontend.CatchUpEpisodeID(programme)
	live := &catchUpWatchTestLive{catchUpTestLive: catchUpTestLive{item: frontend.CatchUpItem{
		ID: id, Type: frontend.TypeEpisode,
		Programme: frontend.LiveTVProgramme{ID: programme, ChannelID: "channel", Start: start, End: start.Add(time.Hour)},
	}}}
	f.srv.setLiveTV(live)
	metadata := func() map[string]any {
		t.Helper()
		got := f.request(t, "GET", "/library/metadata/"+id, map[string]string{"Accept": "application/json"})
		if got.Code != http.StatusOK {
			t.Fatalf("catch-up metadata: %d", got.Code)
		}
		return decodeJSON(t, got)["Metadata"].([]any)[0].(map[string]any)
	}
	timeline := url.Values{"ratingKey": {id}, "key": {"/library/metadata/" + id}, "state": {"stopped"}, "time": {"600000"}, "duration": {"3600000"}}
	if got := f.request(t, "GET", "/:/timeline?"+timeline.Encode(), nil); got.Code != http.StatusOK {
		t.Fatalf("catch-up timeline: %d", got.Code)
	}
	if len(live.progress) != 2 || live.progress[0] != 10*time.Minute || live.progress[1] != time.Hour || live.states[0] != "stopped" {
		t.Fatalf("timeline progress: %v %v", live.progress, live.states)
	}
	if md := metadata(); md["viewOffset"] != float64(600000) {
		t.Fatalf("resume point not shown: %v", md["viewOffset"])
	}
	mark := url.Values{"key": {id}, "identifier": {"com.plexapp.plugins.library"}}
	if got := f.request(t, "PUT", "/:/scrobble?"+mark.Encode(), nil); got.Code != http.StatusOK {
		t.Fatalf("mark watched: %d", got.Code)
	}
	if md := metadata(); md["viewCount"] != float64(1) || md["viewOffset"] != nil {
		t.Fatalf("watched not shown: %v %v", md["viewCount"], md["viewOffset"])
	}
	if got := f.request(t, "PUT", "/:/unscrobble?"+mark.Encode(), nil); got.Code != http.StatusOK {
		t.Fatalf("mark unwatched: %d", got.Code)
	}
	if md := metadata(); md["viewCount"] != nil {
		t.Fatalf("unwatched not shown: %v", md["viewCount"])
	}
	other := url.Values{"key": {frontend.CatchUpEpisodeID("channel-1700003600000")}, "identifier": {"com.plexapp.plugins.library"}}
	if got := f.request(t, "PUT", "/:/scrobble?"+other.Encode(), nil); got.Code != http.StatusNotFound {
		t.Fatalf("unknown airing: %d", got.Code)
	}
}

// Mark as Watched and Unwatched on a catch-up show reach every airing in it,
// and the show's leaf counts show its airings' state (the watched badge).
func TestCatchUpShowWatchStateThroughPlex(t *testing.T) {
	f := newFixture()
	t.Cleanup(func() { _ = f.srv.Close() })
	start := time.Unix(1700000000, 0).UTC()
	id := frontend.CatchUpGroupID(frontend.TypeShow, "News")
	live := &catchUpWatchTestLive{catchUpTestLive: catchUpTestLive{item: frontend.CatchUpItem{
		ID: id, ShowID: id, SeasonID: frontend.CatchUpGroupID(frontend.TypeSeason, "News"), Type: frontend.TypeShow,
		Programme: frontend.LiveTVProgramme{ID: "channel-1700000000000", ChannelID: "channel", Start: start, End: start.Add(time.Hour), Item: frontend.LiveTVItem{Title: "News"}},
		Watch:     frontend.CatchUpWatch{Episodes: 3, Watched: 1, InProgress: 1, LastViewedAt: start.Add(2 * time.Hour)},
	}}}
	f.srv.setLiveTV(live)
	show := func(path string) map[string]any {
		t.Helper()
		got := f.request(t, "GET", path, map[string]string{"Accept": "application/json"})
		if got.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, got.Code)
		}
		return decodeJSON(t, got)["Metadata"].([]any)[0].(map[string]any)
	}
	for _, path := range []string{"/library/metadata/" + id, "/library/sections/2147483646/all"} {
		md := show(path)
		if md["leafCount"] != float64(3) || md["viewedLeafCount"] != float64(1) || md["lastViewedAt"] != float64(start.Add(2*time.Hour).Unix()) {
			t.Fatalf("%s counts: %v %v %v", path, md["leafCount"], md["viewedLeafCount"], md["lastViewedAt"])
		}
	}
	// Some clients send the show's key, which is its children listing.
	mark := url.Values{"key": {"/library/metadata/" + id + "/children"}, "identifier": {"com.plexapp.plugins.library"}}
	if got := f.request(t, "PUT", "/:/scrobble?"+mark.Encode(), nil); got.Code != http.StatusOK {
		t.Fatalf("mark show watched by its key: %d", got.Code)
	}
	mark = url.Values{"key": {id}, "identifier": {"com.plexapp.plugins.library"}}
	if md := show("/library/metadata/" + id); md["viewedLeafCount"] != float64(3) {
		t.Fatalf("watched show: %v", md["viewedLeafCount"])
	}
	if got := f.request(t, "PUT", "/:/unscrobble?"+mark.Encode(), nil); got.Code != http.StatusOK {
		t.Fatalf("mark show unwatched: %d", got.Code)
	}
	if md := show("/library/metadata/" + id); md["viewedLeafCount"] != float64(0) {
		t.Fatalf("unwatched show: %v", md["viewedLeafCount"])
	}
	// A timeline names an airing, never a show.
	timeline := url.Values{"ratingKey": {id}, "key": {"/library/metadata/" + id}, "state": {"playing"}, "time": {"600000"}}
	f.request(t, "GET", "/:/timeline?"+timeline.Encode(), nil)
	if len(live.progress) != 0 {
		t.Fatalf("a show timeline recorded progress: %v", live.progress)
	}
	other := url.Values{"key": {frontend.CatchUpGroupID(frontend.TypeShow, "Weather")}, "identifier": {"com.plexapp.plugins.library"}}
	if got := f.request(t, "PUT", "/:/scrobble?"+other.Encode(), nil); got.Code != http.StatusNotFound {
		t.Fatalf("unknown show: %d", got.Code)
	}
}

func (*catchUpTestLive) NoteClientActivity() {}

type catchUpArtTestLive struct {
	catchUpTestLive
	image []byte
}

func (f *catchUpArtTestLive) CatchUpArtwork(_ context.Context, viewer *frontend.Viewer, id, kind string) ([]byte, string, error) {
	if viewer == nil || viewer.ID != ownerUser.ID || id != f.item.ID || kind != frontend.ImageStill {
		return nil, "", frontend.ErrNotFound
	}
	return f.image, "image/jpeg", nil
}

// Plex apps fetch every thumb through the photo transcoder, so a catch-up
// item's artwork must resolve there as well as on its own route.
func TestCatchUpArtworkThroughPhotoTranscoder(t *testing.T) {
	f := newFixture()
	t.Cleanup(func() { _ = f.srv.Close() })
	src := image.NewRGBA(image.Rect(0, 0, 960, 540))
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, src, nil); err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1700000000, 0).UTC()
	programme := "channel-1700000000000"
	live := &catchUpArtTestLive{image: jpg.Bytes(), catchUpTestLive: catchUpTestLive{item: frontend.CatchUpItem{
		ID: frontend.CatchUpEpisodeID(programme), Type: frontend.TypeEpisode,
		Programme: frontend.LiveTVProgramme{ID: programme, ChannelID: "channel", Start: start, End: start.Add(time.Hour)},
		Artwork:   []frontend.Image{{Kind: frontend.ImageStill}},
	}}}
	f.srv.setLiveTV(live)
	result := f.request(t, "GET", "/library/sections/2147483646/allLeaves", map[string]string{"Accept": "application/json"})
	thumb, _ := decodeJSON(t, result)["Metadata"].([]any)[0].(map[string]any)["thumb"].(string)
	if thumb == "" {
		t.Fatal("catch-up episode has no thumb")
	}
	if got := f.request(t, "GET", thumb, nil); got.Code != http.StatusOK {
		t.Fatalf("catch-up art route: %d", got.Code)
	}
	got := f.request(t, "GET", "/photo/:/transcode?"+url.Values{"url": {thumb}, "width": {"320"}, "height": {"180"}}.Encode(), nil)
	if got.Code != http.StatusOK || got.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("catch-up art through the photo transcoder: %d %q", got.Code, got.Header().Get("Content-Type"))
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(got.Body.Bytes()))
	if err != nil || cfg.Width != 320 {
		t.Fatalf("catch-up art not fitted to the requested width: %v %d", err, cfg.Width)
	}
	nested := "/photo/:/transcode?" + url.Values{
		"url":   {"http://previous-server.invalid" + thumb + "?X-Plex-Token=" + ownerToken},
		"width": {"320"},
	}.Encode()
	if got := f.get(t, nested, "X-Plex-Product", "Plex for Android", "X-Plex-Token", ownerToken); got.Code != http.StatusOK {
		t.Fatalf("Android art for the allowed outer viewer: %d", got.Code)
	}
	if got := f.get(t, nested, "X-Plex-Product", "Plex for Android", "X-Plex-Token", friendToken); got.Code != http.StatusNotFound {
		t.Fatalf("Android art with a denied outer viewer and allowed nested token: %d", got.Code)
	}
	missing := f.request(t, "GET", "/photo/:/transcode?"+url.Values{"url": {"/library/catchup/art/" + live.item.ID + "/backdrop"}, "width": {"320"}}.Encode(), nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing catch-up art: %d", missing.Code)
	}
}
