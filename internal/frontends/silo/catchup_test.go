package silo

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

func TestCatchUpLibraryPlayback(t *testing.T) {
	live := frontendtest.NewCatchUpLive()
	viewer := &frontend.Viewer{ID: 7, IsOwner: true}
	identity := testDeviceIdentity(frontendtest.Identity{
		ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil },
	}, viewer)
	inst, err := (Frontend{MachineID: "test"}).Mount(t.Context(), frontend.Host{
		Identity: identity, Experience: frontendtest.Experience{}, Sessions: frontendtest.Sessions{},
		Capabilities: frontendtest.Capabilities{}, LiveTV: live,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer grant")
		r.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	if list := request(http.MethodGet, "/api/v2/user/libraries", ""); list.Code != 200 || !strings.Contains(list.Body.String(), "Catch-up") {
		t.Fatalf("library list: %d", list.Code)
	}
	type result struct {
		Items []struct {
			ID string `json:"content_id"`
		}
	}
	shows := request(http.MethodGet, "/api/v2/catalog?library_id=catchup", "")
	var listed result
	if shows.Code != 200 || json.Unmarshal(shows.Body.Bytes(), &listed) != nil || len(listed.Items) != 1 {
		t.Fatalf("series list: %d", shows.Code)
	}
	episodes := request(http.MethodGet, "/api/v2/catalog/series/"+listed.Items[0].ID+"/seasons/1/episodes", "")
	if episodes.Code != 200 || json.Unmarshal(episodes.Body.Bytes(), &listed) != nil || len(listed.Items) != 1 {
		t.Fatalf("episode list: %d", episodes.Code)
	}
	detail := request(http.MethodGet, "/api/v2/watch/"+listed.Items[0].ID, "")
	var watch struct {
		Versions []struct {
			FileID string `json:"file_id"`
		}
	}
	if detail.Code != 200 || json.Unmarshal(detail.Body.Bytes(), &watch) != nil || len(watch.Versions) != 1 {
		t.Fatalf("watch detail: %d", detail.Code)
	}
	body, err := json.Marshal(map[string]any{"protocol_version": 3, "installation_id": inst.(*instance).installationID(),
		"playback_attempt_id": "test", "file_id": watch.Versions[0].FileID})
	if err != nil {
		t.Fatal(err)
	}
	start := request(http.MethodPost, "/api/v2/playback/start", string(body))
	var plan struct {
		PlaybackPlan struct{ Stream struct{ URL string } } `json:"playback_plan"`
	}
	if start.Code != 201 || json.Unmarshal(start.Body.Bytes(), &plan) != nil || plan.PlaybackPlan.Stream.URL == "" ||
		live.Opened.ChannelID != live.Item.Programme.ChannelID || live.Opened.ProgrammeID != live.Item.Programme.ID {
		t.Fatalf("playback start: %d", start.Code)
	}
	stream := request(http.MethodGet, plan.PlaybackPlan.Stream.URL, "")
	if stream.Code != 200 || !strings.Contains(stream.Body.String(), "segment/0.ts") || !strings.Contains(stream.Body.String(), "#EXT-X-ENDLIST") {
		t.Fatalf("playback stream: %d", stream.Code)
	}

	// Watch state: a progress sample keeps the resume point, the episode's
	// user_data shows it, and watched marks the airing or its series.
	var session struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(start.Body.Bytes(), &session) != nil || session.SessionID == "" {
		t.Fatal("catch-up session id")
	}
	sample := `{"installation_id":"` + inst.(*instance).installationID() + `","position":600,"sequence":1,"is_paused":false}`
	if got := request(http.MethodPost, "/api/v2/playback/"+session.SessionID+"/progress", sample); got.Code != 200 || live.Item.Watch.ViewOffset != 10*time.Minute {
		t.Fatalf("catch-up progress: %d, %v", got.Code, live.Item.Watch.ViewOffset)
	}
	type userData struct {
		Items []struct {
			ID   string `json:"content_id"`
			Data struct {
				Played     bool    `json:"played"`
				InProgress bool    `json:"is_in_progress"`
				Position   float64 `json:"position_seconds"`
				Unplayed   int     `json:"unplayed_count"`
			} `json:"user_data"`
		}
	}
	var cards userData
	series := frontend.CatchUpGroupGUID(live.Item.ShowID)
	episodes = request(http.MethodGet, "/api/v2/catalog/series/"+series+"/seasons/1/episodes", "")
	if json.Unmarshal(episodes.Body.Bytes(), &cards) != nil || len(cards.Items) != 1 || !cards.Items[0].Data.InProgress || cards.Items[0].Data.Position != 600 {
		t.Fatalf("episode user_data: %s", episodes.Body.String())
	}
	episode := cards.Items[0].ID
	if got := request(http.MethodPost, "/api/v2/watched/"+episode, ""); got.Code != http.StatusNoContent || live.Item.Watch.ViewCount != 1 {
		t.Fatalf("mark episode watched: %d, %+v", got.Code, live.Item.Watch)
	}
	shows = request(http.MethodGet, "/api/v2/catalog?library_id=catchup", "")
	if json.Unmarshal(shows.Body.Bytes(), &cards) != nil || len(cards.Items) != 1 || !cards.Items[0].Data.Played || cards.Items[0].Data.Unplayed != 0 {
		t.Fatalf("series user_data: %s", shows.Body.String())
	}
	if got := request(http.MethodDelete, "/api/v2/watched/"+series, ""); got.Code != http.StatusNoContent || live.Item.Watch.ViewCount != 0 {
		t.Fatalf("mark series unwatched: %d, %+v", got.Code, live.Item.Watch)
	}
	if got := request(http.MethodPost, "/api/v2/watched/"+frontend.CatchUpGroupGUID(frontend.CatchUpGroupID(frontend.TypeShow, "Weather")), ""); got.Code != http.StatusNotFound {
		t.Fatalf("unknown series: %d", got.Code)
	}
}
