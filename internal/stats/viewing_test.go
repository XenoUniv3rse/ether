package stats

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// One press of Play is one history row whatever the client sends: probes,
// a prepared source nobody read, and the open/stop/reopen churn of a
// direct-play client (the v0.3.2 production trace: eight sessions in six
// seconds) all fold into the viewing that delivered media. A later viewing
// past the play-session window is its own row, and a failed attempt is
// recorded, marked, and not counted as a play.
func TestOnePlayIsOneHistoryRow(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 11, 3, 20, 45, 45, 0, time.UTC)
	svc := New(db, Options{Now: func() time.Time { return now }})
	tv := core.ClientCaps{ClientID: "living-room", Platform: "tvOS", DeviceName: "Living Room TV"}
	session := func(id string, bytes int64) *core.PlaySession {
		return &core.PlaySession{ID: id, UserID: 7, ClientID: tv.ClientID, Key: 4001555370010004, Username: "Viewer", TitleName: "Black Bird - S01E04", Client: tv, Decision: core.DecisionDirectPlay, StartedAt: now, BytesServed: bytes}
	}
	record := func(p *core.PlaySession, ended bool, problem string) {
		t.Helper()
		if err := svc.Record(ctx, p, ended, problem); err != nil {
			t.Fatal(err)
		}
	}

	// A decision prepares a source the client never reads; the grace ends.
	prepared := session("prepared", 0)
	prepared.PreparedOnly = true
	record(prepared, false, "")
	record(prepared, true, "")
	// HEAD and range probes.
	record(session("probe", 2), false, "")
	record(session("probe", 64<<10), true, "")
	// Seven opens, each replaced by the next after serving its range.
	var want int64 = 64 << 10
	for i := range 7 {
		now = now.Add(800 * time.Millisecond)
		p := session(fmt.Sprintf("churn-%d", i), 3<<20)
		record(p, false, "")
		record(p, true, "")
		want += 3 << 20
	}
	// The open that plays for 41 minutes, sampled, then a sample that raced
	// its stop.
	now = now.Add(time.Second)
	live := session("watch", 1<<20)
	record(live, false, "")
	now = now.Add(41 * time.Minute)
	live.BytesServed = 4 << 30
	live.ViewerState = "playing"
	record(live, false, "")
	record(live, true, "")
	record(live, false, "")
	want += 4 << 30

	h, err := svc.History(ctx, now.Add(-time.Hour), now.Add(time.Hour), 7, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 {
		t.Fatalf("one play recorded %d rows: %+v", len(h), h)
	}
	if h[0].Ended == 0 || h[0].Bytes != want || h[0].Started != time.Date(2026, 11, 3, 20, 45, 45, 0, time.UTC).Unix() {
		t.Fatalf("row %+v, want ended, %d bytes, first open's start", h[0], want)
	}

	// Watching again after the window is a second play; a failed attempt
	// that delivered nothing is shown with its error but is not a play.
	now = now.Add(10 * time.Minute)
	again := session("again", 32<<20)
	record(again, false, "")
	record(again, true, "")
	now = now.Add(10 * time.Minute)
	failed := session("failed", 0)
	record(failed, false, "Playback failed; see server logs")
	record(failed, true, "")
	h, err = svc.History(ctx, now.Add(-2*time.Hour), now.Add(time.Hour), 7, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 3 || h[0].Error == "" || h[0].Bytes != 0 || h[1].Error != "" {
		t.Fatalf("history %+v", h)
	}
	sum, err := svc.Summary(ctx, now.Add(-2*time.Hour), now.Add(time.Hour), 7)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Plays != 2 || sum.Bytes != want+32<<20 {
		t.Fatalf("summary %+v", sum)
	}
}
