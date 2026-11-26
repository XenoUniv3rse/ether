package plex

import (
	"bytes"
	"context"
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
