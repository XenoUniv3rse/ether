package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestCatchUpViewPlayback(t *testing.T) {
	for _, protocol := range []frontend.Protocol{"jellyfin", "emby"} {
		t.Run(string(protocol), func(t *testing.T) {
			live := frontendtest.NewCatchUpLive()
			viewer := &frontend.Viewer{ID: 1, IsOwner: true}
			h, err := mount(t.Context(), frontend.Host{
				Identity:   frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }},
				Experience: frontendtest.Experience{}, Sessions: frontendtest.Sessions{}, Capabilities: frontendtest.Capabilities{}, LiveTV: live,
			}, "test", protocol)
			if err != nil {
				t.Fatal(err)
			}
			request := func(method, path, body string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.Header.Set("X-Emby-Token", "fixture-credential")
				h.Handler().ServeHTTP(w, r)
				return w
			}
			views := request(http.MethodGet, "/UserViews", "")
			if views.Code != 200 || !strings.Contains(views.Body.String(), catchUpViewID()) {
				t.Fatalf("catch-up views: %d", views.Code)
			}
			page := request(http.MethodGet, "/Items?ParentId="+catchUpViewID()+"&IncludeItemTypes=Episode", "")
			var listed struct{ Items []struct{ ID string } }
			if page.Code != 200 || json.Unmarshal(page.Body.Bytes(), &listed) != nil || len(listed.Items) != 1 {
				t.Fatalf("catch-up list: %d", page.Code)
			}
			path := "/Items/" + listed.Items[0].ID + "/PlaybackInfo"
			info := request(http.MethodPost, path, `{"IsPlayback":true}`)
			var played struct {
				MediaSources []struct{ TranscodingURL string }
			}
			if info.Code != 200 || json.Unmarshal(info.Body.Bytes(), &played) != nil || len(played.MediaSources) != 1 ||
				live.Opened.ChannelID != live.Item.Programme.ChannelID || live.Opened.ProgrammeID != live.Item.Programme.ID {
				t.Fatalf("catch-up playback info: %d", info.Code)
			}
			stream := request(http.MethodGet, played.MediaSources[0].TranscodingURL, "")
			if stream.Code != 200 || !strings.Contains(stream.Body.String(), "#EXT-X-ENDLIST") {
				t.Fatalf("catch-up stream: %d", stream.Code)
			}

			// Watch state: a progress report keeps the resume point, item user
			// data shows it, and Played / Unplayed mark the airing.
			var session struct{ PlaySessionId string }
			if json.Unmarshal(info.Body.Bytes(), &session) != nil || session.PlaySessionId == "" {
				t.Fatal("catch-up play session id")
			}
			id := listed.Items[0].ID
			report := `{"ItemId":"` + id + `","PlaySessionId":"` + session.PlaySessionId + `","PositionTicks":6000000000,"RunTimeTicks":36000000000}`
			if got := request(http.MethodPost, "/Sessions/Playing/Progress", report); got.Code != http.StatusNoContent || live.Item.Watch.ViewOffset != 10*time.Minute {
				t.Fatalf("catch-up progress: %d, %v", got.Code, live.Item.Watch.ViewOffset)
			}
			// A final report after the archive session is gone still records it.
			late := `{"ItemId":"` + id + `","PositionTicks":12000000000}`
			if got := request(http.MethodPost, "/Sessions/Playing/Stopped", late); got.Code != http.StatusNoContent || live.Item.Watch.ViewOffset != 20*time.Minute {
				t.Fatalf("catch-up stop without a session: %d, %v", got.Code, live.Item.Watch.ViewOffset)
			}
			live.Item.Watch.ViewOffset = 10 * time.Minute
			var withData struct {
				Items []struct {
					UserData struct{ PlaybackPositionTicks int64 }
				}
			}
			page = request(http.MethodGet, "/Items?ParentId="+catchUpViewID()+"&IncludeItemTypes=Episode", "")
			if json.Unmarshal(page.Body.Bytes(), &withData) != nil || len(withData.Items) != 1 || withData.Items[0].UserData.PlaybackPositionTicks != 6000000000 {
				t.Fatalf("catch-up user data: %s", page.Body.String())
			}
			var data struct{ Played bool }
			if got := request(http.MethodPost, "/UserPlayedItems/"+id, ""); got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &data) != nil || !data.Played {
				t.Fatalf("catch-up played: %d %s", got.Code, got.Body.String())
			}
			if got := request(http.MethodDelete, "/UserPlayedItems/"+id, ""); got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &data) != nil || data.Played {
				t.Fatalf("catch-up unplayed: %d %s", got.Code, got.Body.String())
			}

			// A series: Played marks its airings, and its user data shows
			// the roll-up (Jellyfin also counts unplayed airings).
			series := frontend.CatchUpGroupGUID(live.Item.ShowID)
			var group struct {
				Played            bool
				UnplayedItemCount *int
			}
			got := request(http.MethodPost, "/UserPlayedItems/"+series, "")
			if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &group) != nil || !group.Played || live.Item.Watch.ViewCount != 1 {
				t.Fatalf("catch-up series played: %d %s", got.Code, got.Body.String())
			}
			if protocol == "jellyfin" && (group.UnplayedItemCount == nil || *group.UnplayedItemCount != 0) {
				t.Fatalf("catch-up series unplayed count: %s", got.Body.String())
			}
			var shows struct {
				Items []struct {
					ID       string `json:"Id"`
					UserData struct {
						Played            bool
						UnplayedItemCount *int
					}
				}
			}
			page = request(http.MethodGet, "/Items?ParentId="+catchUpViewID(), "")
			if json.Unmarshal(page.Body.Bytes(), &shows) != nil || len(shows.Items) != 1 || shows.Items[0].ID != series || !shows.Items[0].UserData.Played {
				t.Fatalf("catch-up series listing: %s", page.Body.String())
			}
			var refreshed struct {
				Items []struct{ UserData struct{ Played bool } }
			}
			page = request(http.MethodGet, "/Items?Ids="+series, "")
			if json.Unmarshal(page.Body.Bytes(), &refreshed) != nil || len(refreshed.Items) != 1 || !refreshed.Items[0].UserData.Played {
				t.Fatalf("catch-up series by id: %s", page.Body.String())
			}
			if got := request(http.MethodDelete, "/UserPlayedItems/"+series, ""); got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &group) != nil || group.Played || live.Item.Watch.ViewCount != 0 {
				t.Fatalf("catch-up series unplayed: %d %s", got.Code, got.Body.String())
			}
			if protocol == "jellyfin" && (group.UnplayedItemCount == nil || *group.UnplayedItemCount != 1) {
				t.Fatalf("catch-up series unplayed count after unmarking: %v", group.UnplayedItemCount)
			}
		})
	}
}
