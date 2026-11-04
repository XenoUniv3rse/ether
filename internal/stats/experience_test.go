package stats

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestExperienceWindowMetricsAndCause(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	svc := New(db, Options{Now: func() time.Time { return now }})
	if _, err := db.Exec(`INSERT INTO stats_plays(id,user_id,rating_key,username,title,platform,device,decision,started,updated,ended,watched_ms) VALUES('old',7,1,'Sam','Old','iOS','Phone','directplay',?,?,?,600000)`, now.Add(-48*time.Hour).Unix(), now.Add(-47*time.Hour).Unix(), now.Add(-47*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	uhd := core.MediaInfo{BitrateKbps: 40000, Streams: []core.StreamInfo{{Type: "video", Height: 2160, ColorTransfer: "smpte2084"}}}
	hd := core.MediaInfo{BitrateKbps: 12000, Streams: []core.StreamInfo{{Type: "video", Height: 1080}}}
	plays := []struct {
		p       core.PlaySession
		watched int64
	}{
		{core.PlaySession{ID: "s1", UserID: 7, Username: "Sam", Decision: core.DecisionDirectPlay, Info: uhd, Client: core.ClientCaps{Remote: true, DeviceName: "Phone"},
			BytesServed: 18e9, Experience: &core.PlayExperience{Requested: true, Pictured: true, StartTime: 8 * time.Second, BufferEvents: 2, Buffering: 30 * time.Second}}, 3600000},
		{core.PlaySession{ID: "s2", UserID: 7, Username: "Sam", Decision: core.DecisionDirectPlay, Info: uhd, Client: core.ClientCaps{Remote: true, DeviceName: "Phone"}, Failovers: 1, DownFailovers: 1,
			BytesServed: 18e9, Experience: &core.PlayExperience{Requested: true, Pictured: true, StartTime: 12 * time.Second, BufferEvents: 1}}, 3600000},
		{core.PlaySession{ID: "s3", UserID: 7, Username: "Sam", Info: uhd, Client: core.ClientCaps{Remote: true, DeviceName: "Phone"},
			BytesServed: 2 << 20, Experience: &core.PlayExperience{Requested: true, StartTime: -1}}, 0},
		{core.PlaySession{ID: "a1", UserID: 8, Username: "Ana", Decision: core.DecisionTranscode, Info: hd, Client: core.ClientCaps{DeviceName: "Living Room"},
			DecisionReason: "transcode; audio codec dts not supported; video -> h264 1080p 8000 kbps", Encoder: &core.EncoderStatus{Decision: core.Decision{BitrateKbps: 8000}},
			BytesServed: 18e8, Experience: &core.PlayExperience{Requested: true, Pictured: true, StartTime: 2 * time.Second}}, 1800000},
		{core.PlaySession{ID: "a2", UserID: 8, Username: "Ana", Decision: core.DecisionTranscode, Info: hd, Client: core.ClientCaps{DeviceName: "Living Room"},
			DecisionReason: "transcode; audio codec dts not supported; video -> h264 1080p 8000 kbps", Encoder: &core.EncoderStatus{Decision: core.Decision{BitrateKbps: 8000}},
			BytesServed: 18e8, Experience: &core.PlayExperience{Requested: true, Pictured: true, StartTime: 1 * time.Second}}, 1800000},
		// A source prepared for a detail page that no client read: no row.
		{core.PlaySession{ID: "prep", UserID: 8, Username: "Ana", Info: hd}, 0},
	}
	for i, x := range plays {
		x.p.StartedAt = now.Add(-time.Duration(len(plays)-i) * time.Hour)
		x.p.TitleName, x.p.Key = x.p.ID, core.RatingKey(100+i)
		if e := x.p.Experience; e != nil {
			e.RequestedAt = x.p.StartedAt
			if e.Pictured {
				e.PicturedAt = x.p.StartedAt.Add(e.StartTime)
			}
		}
		if err := svc.Record(ctx, &x.p, true, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE stats_plays SET watched_ms=? WHERE id=?`, x.watched, x.p.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Two sessions of one viewing (a reopen after a seek) are one row: the
	// start runs from the first request to the first picture, buffering
	// and down-switches add up, and the row is a failed start only if no
	// session ever showed a picture.
	t0 := now.Add(-30 * time.Minute)
	first := core.PlaySession{ID: "v1", UserID: 9, Username: "Lee", Key: 500, ClientID: "tv", StartedAt: t0, BytesServed: 2 << 20, DownFailovers: 1,
		Experience: &core.PlayExperience{Requested: true, RequestedAt: t0, StartFailure: "stalled", BufferEvents: 0}}
	if err := svc.Record(ctx, &first, true, ""); err != nil {
		t.Fatal(err)
	}
	second := core.PlaySession{ID: "v2", UserID: 9, Username: "Lee", Key: 500, ClientID: "tv", StartedAt: t0.Add(time.Minute), BytesServed: 3 << 20, DownFailovers: 1,
		Experience: &core.PlayExperience{Requested: true, RequestedAt: t0.Add(time.Minute), Pictured: true, PicturedAt: t0.Add(time.Minute + 4*time.Second), BufferEvents: 2, Buffering: 9 * time.Second}}
	for _, ended := range []bool{false, true} {
		second.Experience.BufferEvents++
		if err := svc.Record(ctx, &second, ended, ""); err != nil {
			t.Fatal(err)
		}
	}
	if v, err := svc.PlayByID(ctx, "v1"); err != nil || v == nil || v["startMs"] != 64000.0 || v["bufferEvents"] != 4.0 || v["bufferMs"] != 9000.0 || v["failed"] != false || v["bytes"] != float64(5<<20) {
		t.Fatalf("one viewing, two sessions: %v %v", v, err)
	}
	if v, _ := svc.PlayByID(ctx, "v2"); v != nil {
		t.Fatal("second session wrote its own row")
	}
	if _, err := db.Exec(`DELETE FROM stats_plays WHERE id='v1'`); err != nil {
		t.Fatal(err)
	}

	o, err := svc.Experience(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if o.MeasuredSince != now.Unix() || o.Household.Plays != 6 || o.Household.MeasuredPlays != 5 || o.Household.FailedStarts != 1 || o.Household.Decisions.Transcode != 2 || o.Household.Decisions.DirectPlay != 3 {
		t.Fatalf("household %+v", o.Household)
	}
	if len(o.Viewers) != 2 || o.Viewers[0].UserID != 7 {
		t.Fatalf("worst viewer first: %+v", o.Viewers)
	}
	sam := o.Viewers[0].Metrics
	if *sam.StartMedianMS != 8000 || *sam.StartP90MS != 12000 || *sam.BufferEventsPerHour != 1.5 || sam.BufferMS != 30000 || *sam.UHDPct != 100 || *sam.HDRPct != 100 || sam.DownSwitches != 1 {
		t.Fatalf("sam %+v", sam)
	}
	ana := o.Viewers[1].Metrics
	if top := ana.TopTranscodeReason; top == nil || top.Kind != "audio_codec" || top.Detail != "dts" || top.Count != 2 || *ana.DeliveredKbps != 8000 || *ana.SourceKbps != 12000 || ana.TopDevice == nil || ana.TopDevice.Name != "Living Room" || ana.TopDevice.Plays != 2 || ana.WatchedMS != 3600000 {
		t.Fatalf("ana %+v", ana)
	}

	v, err := svc.ExperienceForViewer(ctx, 7, 7)
	if err != nil {
		t.Fatal(err)
	}
	if v.Cause.Kind != "away_bitrate" || len(v.Trend) != 7 || len(v.Plays) != 4 || v.Plays[3].ID != "old" || v.Plays[3].Measured || v.Plays[3].StartMS != nil || !v.Plays[0].Failed {
		t.Fatalf("sam detail %+v", v)
	}
	if a, _ := svc.ExperienceForViewer(ctx, 8, 7); a.Cause.Kind != "transcode_device" || a.Cause.Device != "Living Room" || a.Cause.ReasonDetail != "dts" {
		t.Fatalf("ana cause %+v", a.Cause)
	}

	for path, want := range map[string]string{"/?days=30": "household", "/viewers/7?days=30": "cause"} {
		w := httptest.NewRecorder()
		svc.ExperienceHandler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		var body map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body[want] == nil {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	w := httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/plays/s1", nil))
	var play map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &play) != nil || play["startMs"] != 8000.0 || play["origin"] == nil || play["bytes"] == nil {
		t.Fatalf("play detail: %d %s", w.Code, w.Body)
	}
}

func TestClassifyTranscodeReason(t *testing.T) {
	for reason, want := range map[string][2]string{
		"transcode; audio codec truehd not supported; video -> h264 1080p":          {"audio_codec", "truehd"},
		"remux to HLS (mpegts); container avi not supported; video h264 copied":     {"container", "avi"},
		"transcode; 2160p exceeds 1080p limit; video -> h264 1080p 8000 kbps":       {"resolution", "1080"},
		"transcode; bitrate 40000 kbps exceeds 8000 kbps limit; video -> h264":      {"bitrate", ""},
		"transcode; pgs subtitles must be burned in; video -> h264 1080p 8000 kbps": {"subtitles", "pgs"},
		"transcode; client lacks HDR (HDR10 source); video -> h264":                 {"hdr", ""},
		"transcode; video -> h264 1080p 8000 kbps":                                  {"other", ""},
	} {
		if k, d := classifyReason(reason); k != want[0] || d != want[1] {
			t.Errorf("%q: %s %s", reason, k, d)
		}
	}
}
