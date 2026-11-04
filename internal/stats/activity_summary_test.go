package stats

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

func TestActivitySummary(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	start := time.Date(2026, 10, 18, 23, 30, 0, 0, time.UTC)
	for _, r := range []struct {
		id, decision                string
		offset, end, watched, bytes int64
		audio                       any
	}{{"a", "directplay", 0, 7200, 1000, 1000, nil}, {"b", "copy", 3600, 7200, 2000, 2000, 0}} {
		_, err = db.Exec(`INSERT INTO stats_plays(id,user_id,rating_key,username,title,platform,device,decision,started,updated,ended,watched_ms,bytes,audio_copy) VALUES(?,1,1000000000000001,'u','t','p','d',?,?,?,?,?,?,?)`, r.id, r.decision, start.Unix()+r.offset, start.Unix()+r.end, start.Unix()+r.end, r.watched, r.bytes, r.audio)
		if err != nil {
			t.Fatal(err)
		}
	}
	s := New(db, Options{})
	sum, err := s.Summary(ctx, start.Add(-time.Hour), start.Add(4*time.Hour), 0, 120)
	if err != nil {
		t.Fatal(err)
	}
	if sum.PeakStreams != 2 || sum.AvgBitrateKbps != 8 || len(sum.Daily) != 1 || sum.Daily[0].Label != "2026-10-19" || len(sum.Heatmap) != 2 || sum.Heatmap[0].Weekday != 1 || sum.Heatmap[0].Hour != 1 || sum.DailyDelivery[0].Direct != 1 || sum.DailyDelivery[0].Transcode != 1 {
		t.Fatalf("summary: %+v", sum)
	}
}
