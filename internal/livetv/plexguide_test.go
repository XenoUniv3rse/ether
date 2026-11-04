package livetv

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The budget and backoff protect a shared provider, including when the
// provider fails. These checks intentionally exercise the real HTTP client.
func TestPlexGuideRequestBudgetAndBackoff(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("X-Plex-Token") != "test-token" {
			t.Error("missing server token")
		}
		if r.URL.Path == "/rate" {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, `{"MediaContainer":{"Lineup":[{"id":"a","title":"A (2 channels)","type":"ota"}]}}`)
	}))
	defer server.Close()
	s := &Service{o: Options{HTTPClient: server.Client(), PlexGuideURL: server.URL, PlexGuideToken: func() string { return "test-token" }}}
	budget := 1
	if _, err := s.plexGuideRequest(context.Background(), "/ok", nil, &budget); err != nil {
		t.Fatal(err)
	}
	if _, err := s.plexGuideRequest(context.Background(), "/ok", nil, &budget); err == nil || requests.Load() != 1 {
		t.Fatal("budget did not prevent request")
	}
	s.plexGate.next = time.Time{}
	if _, err := s.plexGuideRequest(context.Background(), "/rate", nil, nil); err == nil || !strings.Contains(err.Error(), "rate-limited") || strings.Contains(err.Error(), "test-token") {
		t.Fatalf("unexpected error: %v", err)
	}
	if time.Until(s.plexGate.next) < 29*time.Second {
		t.Fatal("Retry-After was not honoured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := s.plexGuideRequest(ctx, "/ok", nil, nil); err == nil || requests.Load() != 2 {
		t.Fatal("backoff did not prevent request")
	}
}

func TestPlexGuideGridMapping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/grid" || r.URL.Query().Get("channelGridKey") != "key" || r.URL.Query().Get("type") != "1,4" || r.URL.Query().Get("date") != "2026-10-19" {
			t.Errorf("unexpected request: %s", r.URL.String())
		}
		fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"type":"episode","title":"Episode","grandparentTitle":"Show","summary":"Summary","guid":"plex://episode/example","grandparentGuid":"plex://show/example","parentIndex":2,"index":3,"originallyAvailableAt":"2026-10-18","grandparentThumb":"https://example.invalid/art","Genre":[{"tag":"Drama"}],"Media":[{"beginsAt":1792411200,"endsAt":1792414800,"premiere":true,"live":true}]}]}}`)
	}))
	defer server.Close()
	s := &Service{o: Options{HTTPClient: server.Client(), PlexGuideURL: server.URL}}
	budget := 1
	ps, err := s.plexGrid(context.Background(), "key", guideTestNow, &budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 {
		t.Fatalf("got %d programmes", len(ps))
	}
	p := ps[0]
	if p.ChannelID != "key" || p.Item.Title != "Show" || p.Item.EpisodeTitle != "Episode" || p.Item.Season != 2 || p.Item.Episode != 3 || !p.Item.Premiere || !p.Item.Live || p.Item.GUID != "plex://episode/example" || p.Item.ShowGUID != "plex://show/example" || len(p.Item.Genres) != 1 || p.Item.ReleasedMS == 0 {
		t.Fatalf("bad mapping: %+v", p)
	}
}

func TestPlexGuideMappedRefreshAndReviewedAliases(t *testing.T) {
	var grids atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/lineups/lineup/channels":
			fmt.Fprint(w, `{"MediaContainer":{"Channel":[{"gridKey":"one","title":"WCBS-DT","callSign":"WCBSDT","affiliateCallSign":"CBS","vcn":"2.1"},{"gridKey":"two","title":"Other"}]}}`)
		case "/grid":
			if r.URL.Query().Get("channelGridKey") != "one" {
				t.Error("requested an unmapped channel")
			}
			grids.Add(1)
			fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"type":"movie","title":"Movie","guid":"plex://movie/example","Media":[{"beginsAt":%d,"endsAt":%d}]}]}}`, guideTestNow.Unix(), guideTestNow.Add(time.Hour).Unix())
		default:
			t.Error("unexpected request")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	ctx := context.Background()
	s := testService(t, Options{Now: func() time.Time { return guideTestNow }, HTTPClient: server.Client(), PlexGuideURL: server.URL})
	_, err := s.ImportSource(ctx, Source{ID: "guide", Name: "Guide", Kind: "plex", LineupID: "lineup"})
	if err != nil {
		t.Fatal(err)
	}
	if grids.Load() != 0 {
		t.Fatal("fetched grid before a mapping")
	}
	found, err := s.GuideChannels(ctx, "CBS", "", 50)
	if err != nil || len(found) != 1 || found[0].GuideID != "one" {
		t.Fatalf("search: %+v %v", found, err)
	}
	_, err = s.ImportSource(ctx, Source{ID: "playlist", Name: "Playlist", Playlist: m3uFixture("=CBS New York")})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := s.MatchGuides(ctx, GuideMatchRequest{DryRun: true})
	if err != nil || len(proposal.Changes) != 1 || proposal.Changes[0].Method != "alias" {
		t.Fatalf("proposal: %+v %v", proposal, err)
	}
	if result, e := s.MatchGuides(ctx, GuideMatchRequest{}); e != nil || len(result.Changes) != 0 {
		t.Fatalf("alias applied without review: %+v %v", result, e)
	}
	if _, err = s.MatchGuides(ctx, GuideMatchRequest{ChannelIDs: []string{proposal.Changes[0].ChannelID}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RefreshSource(ctx, "guide"); err != nil {
		t.Fatal(err)
	}
	if grids.Load() != 2 {
		t.Fatalf("got %d grid requests", grids.Load())
	}
	src := sourceByID(t, s, "guide")
	if !src.HasGuide || src.ProgrammeCount != 1 || src.NextRefresh == nil {
		t.Fatalf("source: %+v", src)
	}
}
