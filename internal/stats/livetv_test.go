package stats

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestLiveTVHistoryGroupingDurationAndSummary(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)
	svc := New(db, Options{Now: func() time.Time { return now }})

	chESPN := &core.LiveChannel{ID: "ch-espn", Name: "ESPN", Number: "206", Provider: &core.LiveProvider{
		LiveProviderIdentity: core.LiveProviderIdentity{SourceName: "Sports provider", LoginLabel: "jo***", LoginNumber: 2},
	}}
	chCNN := &core.LiveChannel{ID: "ch-cnn", Name: "CNN", Number: "202"}
	client := core.ClientCaps{ClientID: "appletv-1", Platform: "tvOS", DeviceName: "Living Room Apple TV"}

	// Channel surfing within viewingWindow (Key == 0) must keep distinct channels apart,
	// even when delivered bytes are below viewingMinBytes.
	espn1Start := now
	espn1 := &core.PlaySession{
		ID: "live-espn-1", UserID: 7, Username: "Sam", ClientID: client.ClientID, Client: client,
		TitleName: "SportsCenter", StartedAt: espn1Start, BytesServed: 64 << 10,
		Decision: core.DecisionRemux, DecisionVideoCopy: true, DecisionAudioCopy: true,
		Origin: "Live TV", ViewerState: "playing", LiveChannel: chESPN,
		Experience: &core.PlayExperience{Requested: true, RequestedAt: espn1Start, Pictured: true, PicturedAt: espn1Start.Add(time.Second), StartTime: time.Second},
	}
	now = espn1Start.Add(time.Second)
	if err := svc.Record(ctx, espn1, false, ""); err != nil {
		t.Fatal(err)
	}
	now = espn1Start.Add(3*time.Minute + time.Second)
	espn1.BytesServed = 12 << 20
	espn1.LastActive = now
	if err := svc.Record(ctx, espn1, true, ""); err != nil {
		t.Fatal(err)
	}

	// Switch to CNN 10s later: must create a separate row, NOT coalesce with ESPN.
	now = now.Add(10 * time.Second)
	cnnStart := now
	cnn := &core.PlaySession{
		ID: "live-cnn-1", UserID: 7, Username: "Sam", ClientID: client.ClientID, Client: client,
		TitleName: "CNN", StartedAt: cnnStart, BytesServed: 32 << 10,
		Decision: core.DecisionRemux, DecisionVideoCopy: true, DecisionAudioCopy: true,
		Origin: "Live TV", ViewerState: "playing", LiveChannel: chCNN,
		Experience: &core.PlayExperience{Requested: true, RequestedAt: cnnStart, Pictured: true, PicturedAt: cnnStart, StartTime: 0},
	}
	if err := svc.Record(ctx, cnn, false, ""); err != nil {
		t.Fatal(err)
	}
	// Simulate idle expiration: last media was at +2m, expired at +2m30s.
	// Active watched_ms must be 2m, not 2m30s.
	cnn.BytesServed = 8 << 20
	cnn.LastActive = cnnStart.Add(2 * time.Minute)
	now = cnnStart.Add(2*time.Minute + 30*time.Second)
	if err := svc.Record(ctx, cnn, true, ""); err != nil {
		t.Fatal(err)
	}

	// Return to ESPN within 5m of ESPN's stop: coalesces into ESPN's row and sums active watched_ms (3m + 1m = 4m).
	now = now.Add(20 * time.Second)
	espn2Start := now
	espn2 := &core.PlaySession{
		ID: "live-espn-2", UserID: 7, Username: "Sam", ClientID: client.ClientID, Client: client,
		TitleName: "NBA Tonight", StartedAt: espn2Start, BytesServed: 4 << 20,
		Decision: core.DecisionRemux, DecisionVideoCopy: true, DecisionAudioCopy: true,
		Origin: "Live TV", ViewerState: "playing", LiveChannel: chESPN,
		Experience: &core.PlayExperience{Requested: true, RequestedAt: espn2Start, Pictured: true, PicturedAt: espn2Start, StartTime: 0},
	}
	if err := svc.Record(ctx, espn2, false, ""); err != nil {
		t.Fatal(err)
	}
	now = espn2Start.Add(time.Minute)
	espn2.LastActive = now
	if err := svc.Record(ctx, espn2, true, ""); err != nil {
		t.Fatal(err)
	}

	// Virtual channel with non-zero programme key + internal channel-input feeder + VOD movie.
	vcStart := now.Add(time.Minute)
	now = vcStart
	vc := &core.PlaySession{
		ID: "live-vc-1", UserID: 7, Username: "Sam", ClientID: client.ClientID, Client: client,
		Key: core.MovieKey(42), TitleName: "The Matrix", StartedAt: vcStart,
		Info: core.MediaInfo{Duration: 2 * time.Hour}, BytesServed: 5 << 20,
		Decision: core.DecisionRemux, DecisionVideoCopy: true, DecisionAudioCopy: true,
		Origin: "Live TV", ViewerState: "playing",
		LiveChannel: &core.LiveChannel{ID: "vc-scifi", Name: "Sci-Fi 24/7", Number: "101"},
	}
	if err := svc.Record(ctx, vc, false, ""); err != nil {
		t.Fatal(err)
	}
	// Internal channel-input feeder must be ignored.
	feeder := &core.PlaySession{
		ID: "feeder-1", UserID: 7, Username: "Sam", ClientID: "channel-input/vc-scifi/42",
		Key: core.MovieKey(42), TitleName: "The Matrix", StartedAt: vcStart, BytesServed: 20 << 20,
		Decision: core.DecisionDirectPlay,
	}
	if err := svc.Record(ctx, feeder, true, ""); err != nil {
		t.Fatal(err)
	}
	// Client timeline completion must never mark a Live TV row completed.
	svc.ObserveState(state.Event{Kind: state.EventCompleted, UserID: 7, Key: core.MovieKey(42), Completed: true, At: now})
	now = vcStart.Add(5 * time.Minute)
	vc.LastActive = now
	if err := svc.Record(ctx, vc, true, ""); err != nil {
		t.Fatal(err)
	}

	// VOD movie play to verify live and movie kinds coexist cleanly.
	movieStart := now.Add(time.Minute)
	now = movieStart
	movie := &core.PlaySession{
		ID: "vod-1", UserID: 7, Username: "Sam", ClientID: client.ClientID, Client: client,
		Key: core.MovieKey(99), TitleName: "Dune (2021)", StartedAt: movieStart, BytesServed: 50 << 20,
		Decision: core.DecisionDirectPlay, Origin: "Usenet",
	}
	if err := svc.Record(ctx, movie, false, ""); err != nil {
		t.Fatal(err)
	}
	svc.ObserveState(state.Event{Kind: state.EventProgress, UserID: 7, Key: core.MovieKey(99), State: "playing", At: movieStart, Duration: time.Hour})
	svc.ObserveState(state.Event{Kind: state.EventProgress, UserID: 7, Key: core.MovieKey(99), State: "playing", At: movieStart.Add(30 * time.Second)})
	svc.ObserveState(state.Event{Kind: state.EventCompleted, UserID: 7, Key: core.MovieKey(99), Completed: true, At: movieStart.Add(30 * time.Second)})
	now = movieStart.Add(31 * time.Second)
	if err := svc.Record(ctx, movie, true, ""); err != nil {
		t.Fatal(err)
	}

	// Tune failure: recorded in History, excluded from Summary plays.
	failStart := now.Add(time.Minute)
	now = failStart
	fail := &core.PlaySession{
		ID: "live-fail-1", UserID: 7, Username: "Sam", ClientID: client.ClientID, Client: client,
		TitleName: "BBC News", StartedAt: failStart, Decision: core.DecisionRemux,
		Origin: "Live TV", LiveChannel: &core.LiveChannel{ID: "ch-bbc", Name: "BBC News", Number: "209"},
		Experience: &core.PlayExperience{Requested: true, RequestedAt: failStart, StartFailure: "The channel source isn't responding (HTTP 503 from the provider)."},
	}
	if err := svc.Record(ctx, fail, true, "The channel source isn't responding (HTTP 503 from the provider)."); err != nil {
		t.Fatal(err)
	}

	hist, err := svc.History(ctx, espn1Start.Add(-time.Hour), now.Add(time.Hour), 7, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 5 {
		t.Fatalf("expected 5 history rows (ESPN, CNN, Sci-Fi 24/7, Dune, failed BBC; no feeder), got %d: %+v", len(hist), hist)
	}
	byID := map[string]Play{}
	for _, h := range hist {
		byID[h.ID] = h
	}
	if p := byID["live-espn-1"]; p.ChannelID != "ch-espn" || p.ChannelName != "ESPN" || p.ChannelNumber != "206" || p.LiveSourceName != "Sports provider" || p.LiveLoginLabel != "jo***" || p.LiveLoginNumber != 2 || p.Key != 0 || p.Completed || p.DurationMS != 0 || p.WatchedMS != 4*60*1000 {
		t.Fatalf("espn row mismatch: %+v", p)
	}
	if p := byID["live-cnn-1"]; p.ChannelID != "ch-cnn" || p.WatchedMS != 2*60*1000 || p.Completed {
		t.Fatalf("cnn idle-expired row mismatch: %+v", p)
	}
	if p := byID["live-vc-1"]; p.ChannelID != "vc-scifi" || p.Key != 0 || p.Completed || p.DurationMS != 0 || p.WatchedMS != 5*60*1000 {
		t.Fatalf("virtual channel row mismatch: %+v", p)
	}
	if p := byID["live-fail-1"]; p.ChannelID != "ch-bbc" || p.WatchedMS != 0 || p.Bytes != 0 || p.Error == "" {
		t.Fatalf("failed tune row mismatch: %+v", p)
	}

	// PlayByID preserves channel metadata for historical Live TV plays.
	detail, err := svc.PlayByID(ctx, "live-espn-1")
	if err != nil || detail == nil {
		t.Fatalf("PlayByID live-espn-1: %v %+v", err, detail)
	}
	if detail["channelId"] != "ch-espn" || detail["channelName"] != "ESPN" || detail["channelNumber"] != "206" {
		t.Fatalf("PlayByID channel metadata missing: %+v", detail)
	}

	sum, err := svc.Summary(ctx, espn1Start.Add(-time.Hour), now.Add(time.Hour), 7)
	if err != nil {
		t.Fatal(err)
	}
	// Summary counts the 4 delivered plays (not the failed tune).
	if sum.Plays != 4 || sum.Completed != 1 {
		t.Fatalf("summary plays/completed mismatch: %+v", sum)
	}
	kinds := map[string]Bucket{}
	for _, b := range sum.Kinds {
		kinds[b.Label] = b
	}
	if k := kinds["live"]; k.Plays != 3 || k.WatchedMS != (4+2+5)*60*1000 {
		t.Fatalf("expected live kind bucket with 3 plays and 11m, got %+v (all: %+v)", k, sum.Kinds)
	}
	if k := kinds["movie"]; k.Plays != 1 || k.WatchedMS != 30*1000 {
		t.Fatalf("expected movie kind bucket, got %+v", k)
	}
	if len(sum.DailyKinds) != 1 || sum.DailyKinds[0].LiveMS != 11*60*1000 || sum.DailyKinds[0].MovieMS != 30*1000 {
		t.Fatalf("dailyKinds mismatch: %+v", sum.DailyKinds)
	}
	titles := map[string]Bucket{}
	for _, b := range sum.Titles {
		titles[b.Label] = b
	}
	if b := titles["ESPN"]; b.ChannelID != "ch-espn" || b.Key != "" {
		t.Fatalf("live title bucket must carry channelId without ratingKey: %+v", b)
	}
	if b := titles["Dune (2021)"]; b.ChannelID != "" || b.Key == "" || b.Key == "0" {
		t.Fatalf("movie title bucket must carry ratingKey: %+v", b)
	}

	// Short Live TV surfs are not counted as abandoned VOD sessions in Experience.
	exp, err := svc.ExperienceForViewer(ctx, 7, 7)
	if err != nil {
		t.Fatal(err)
	}
	if exp.Metrics.Abandoned != 0 || exp.Metrics.FailedStarts != 1 {
		t.Fatalf("experience metrics mismatch: %+v", exp.Metrics)
	}
}

func TestLiveTVMultiviewTilesRecordPerTileChannel(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 11, 20, 20, 0, 0, 0, time.UTC)
	svc := New(db, Options{Now: func() time.Time { return now }})

	tile1 := &core.PlaySession{
		ID: "mv-tile-1", UserID: 7, Username: "Sam", ClientID: "tv-1",
		Client:    core.ClientCaps{ClientID: "tv-1", Platform: "tvOS", DeviceName: "Apple TV", MultiviewGroup: "mv-1", MultiviewTile: 0},
		TitleName: "ESPN", StartedAt: now, BytesServed: 5 << 20,
		Decision: core.DecisionRemux, DecisionVideoCopy: true, DecisionAudioCopy: true,
		Origin: "Live TV", ViewerState: "playing",
		LiveChannel: &core.LiveChannel{ID: "ch-espn", Name: "ESPN"},
	}
	tile2 := &core.PlaySession{
		ID: "mv-tile-2", UserID: 7, Username: "Sam", ClientID: "tv-1",
		Client:    core.ClientCaps{ClientID: "tv-1", Platform: "tvOS", DeviceName: "Apple TV", MultiviewGroup: "mv-1", MultiviewTile: 1},
		TitleName: "FOX", StartedAt: now, BytesServed: 5 << 20,
		Decision: core.DecisionRemux, DecisionVideoCopy: true, DecisionAudioCopy: true,
		Origin: "Live TV", ViewerState: "playing",
		LiveChannel: &core.LiveChannel{ID: "ch-fox", Name: "FOX"},
	}
	if err := svc.Record(ctx, tile1, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.Record(ctx, tile2, false, ""); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	tile1.LastActive = now
	tile2.LastActive = now
	if err := svc.Record(ctx, tile1, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.Record(ctx, tile2, true, ""); err != nil {
		t.Fatal(err)
	}

	hist, err := svc.History(ctx, now.Add(-time.Hour), now.Add(time.Hour), 7, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Fatalf("expected one history row per multiview tile channel, got %d: %+v", len(hist), hist)
	}
	for _, h := range hist {
		if h.WatchedMS != 10*60*1000 || h.Completed {
			t.Fatalf("unexpected multiview tile row: %+v", h)
		}
	}
}
