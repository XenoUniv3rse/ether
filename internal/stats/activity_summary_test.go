package stats

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
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

func TestActivityCatalogueAndLegacyCopy(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	start := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	statements := []string{
		`INSERT INTO catalog_titles(key,type,title) VALUES(2000000000000001,'show','Series')`,
		`INSERT INTO catalog_titles(key,type,show_key,show_title) VALUES(4000000010010001,'episode',2000000000000001,'Series'),(4000000010010002,'episode',2000000000000001,'Series')`,
		`INSERT INTO catalog_titles(key,type,title) VALUES(1000000000000001,'movie','Film')`,
		`INSERT INTO catalog_genres(key,genre) VALUES(2000000000000001,'Drama'),(1000000000000001,'Comedy')`,
	}
	for _, q := range statements {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for i, key := range []int64{4000000010010001, 4000000010010002, 1000000000000001} {
		_, err = db.Exec(`INSERT INTO stats_plays(id,user_id,rating_key,username,title,platform,device,decision,started,updated,ended,watched_ms,bytes) VALUES(?,1,?,'u','t','p','d','copy',?,?,?,?,?)`, string(rune('a'+i)), key, start.Unix(), start.Unix()+60, start.Unix()+60, 1000, 1000)
		if err != nil {
			t.Fatal(err)
		}
	}
	sum, err := New(db, Options{}).Summary(ctx, start.Add(-time.Second), start.Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Shows) != 1 || sum.Shows[0].Label != "Series" || sum.Shows[0].Plays != 2 || len(sum.Genres) != 2 || sum.Genres[0].Label != "Drama" || sum.Genres[0].WatchedMS != 2000 || len(sum.Decisions) != 1 || sum.Decisions[0].Label != "direct-stream" || sum.DailyDelivery[0].Direct != 3 {
		t.Fatalf("summary: %+v", sum)
	}
}

func TestActivityPlayAddressAndAudioDecision(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s := New(db, Options{Now: func() time.Time { return now }})
	p := &core.PlaySession{ID: "one", UserID: 1, Key: core.MovieKey(1), StartedAt: now, BytesServed: 8 << 20, Client: core.ClientCaps{ClientIP: "192.0.2.1"}, Decision: core.DecisionRemux, DecisionVideoCopy: true, DecisionAudioCopy: false}
	if err = s.Record(ctx, p, false, ""); err != nil {
		t.Fatal(err)
	}
	rows, err := s.History(ctx, now.Add(-time.Second), now.Add(time.Second), 0, 10)
	if err != nil || len(rows) != 1 || rows[0].IP == nil || *rows[0].IP != "192.0.2.1" {
		t.Fatalf("history: %+v %v", rows, err)
	}
	detail, err := s.PlayByID(ctx, "one")
	if err != nil || detail["ip"] != "192.0.2.1" {
		t.Fatalf("detail: %+v %v", detail, err)
	}
	sum, err := s.Summary(ctx, now.Add(-time.Second), now.Add(time.Second), 0)
	if err != nil || len(sum.Decisions) != 1 || sum.Decisions[0].Label != "audio-transcode" {
		t.Fatalf("summary: %+v %v", sum.Decisions, err)
	}
}
