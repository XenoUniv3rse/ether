package livetv

import (
	"context"
	"testing"
	"time"
)

// Dashboard polling must need neither a database nor provider requests, and
// counts people separately from sessions while excluding old/future starts.
func TestLiveUsageSnapshot(t *testing.T) {
	now := time.Now()
	s := &Service{o: Options{Now: func() time.Time { return now }}, sessions: map[string]*session{
		"a": {info: SessionInfo{UserID: 1, Channel: Channel{ID: "one"}}},
		"b": {info: SessionInfo{UserID: 1, Channel: Channel{ID: "one"}}},
		"c": {info: SessionInfo{UserID: 2, Channel: Channel{ID: "one"}}},
	}}
	tap := &upstreamTap{}
	s.inputRelay.taps.bySource = map[string]map[*upstreamTap]struct{}{"provider": {tap: {}}}
	s.inputRelay.taps.pace = map[string]*sourcePace{"provider": {opens: []time.Time{now.Add(-2 * time.Hour), now.Add(-time.Minute), now.Add(time.Minute)}}}
	got, err := s.LiveUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Sessions != 3 || got.Viewers != 2 || len(got.Watching) != 1 || got.Watching[0].Viewers != 2 {
		t.Fatalf("viewing: %+v", got)
	}
	if len(got.Sources) != 1 || got.Sources[0].Open != 1 || got.Sources[0].StartsLastHour != 1 {
		t.Fatalf("sources: %+v", got.Sources)
	}
}
