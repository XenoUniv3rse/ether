package livetv

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type catchUpWatchTestHost interface {
	frontend.CatchUpLibrary
	CatchUpProgress(ctx context.Context, viewer *frontend.Viewer, id string, offset, duration time.Duration, state string, resumeAfter time.Duration) error
	CatchUpSetWatched(ctx context.Context, viewer *frontend.Viewer, id string, watched bool) error
}

func catchUpWatchFixture(t *testing.T, now time.Time) (*Service, Channel, catchUpWatchTestHost) {
	t.Helper()
	s := testService(t, Options{Now: func() time.Time { return now }})
	ch, err := s.SaveChannel(t.Context(), Channel{ID: "archive-watch", Name: "Archive", Number: "1", Kind: "iptv", Enabled: true, Private: true, GuideID: "guide", StreamURL: "https://example.invalid/live.ts"})
	if err != nil {
		t.Fatal(err)
	}
	ch.Archive = &ChannelArchive{Days: 2, Mode: "xtream"}
	data, err := json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.o.DB.ExecContext(t.Context(), "UPDATE livetv_channels SET data=? WHERE id=?", data, ch.ID); err != nil {
		t.Fatal(err)
	}
	for _, hours := range []int{-5, -3} {
		start := now.Add(time.Duration(hours) * time.Hour)
		p := Programme{Start: start, End: start.Add(time.Hour), Item: Item{Title: "News"}}
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.o.DB.ExecContext(t.Context(), "INSERT INTO livetv_programmes(source_id,channel_id,start_ms,end_ms,data) VALUES(?,?,?,?,?)", guideSource(ch), ch.GuideID, start.UnixMilli(), p.End.UnixMilli(), raw); err != nil {
			t.Fatal(err)
		}
	}
	return s, ch, s.Frontend().(catchUpWatchTestHost)
}

func catchUpEpisode(t *testing.T, h catchUpWatchTestHost, u *core.User, id string) frontend.CatchUpItem {
	t.Helper()
	item, err := h.CatchUpItem(t.Context(), u, id)
	if err != nil {
		t.Fatalf("catch-up item %s: %v", id, err)
	}
	return item
}

// An airing keeps a resume point and a watched count per viewer, with the
// thresholds normal titles use, through either id the frontends serve.
func TestCatchUpWatchState(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	_, _, h := catchUpWatchFixture(t, now)
	owner := &core.User{ID: 1, IsOwner: true}
	shows, err := h.CatchUpItems(t.Context(), owner, frontend.CatchUpQuery{})
	if err != nil || len(shows.Items) != 1 {
		t.Fatalf("shows: %d, %v", len(shows.Items), err)
	}
	episodes, err := h.CatchUpItems(t.Context(), owner, frontend.CatchUpQuery{Parent: shows.Items[0].SeasonID})
	if err != nil || len(episodes.Items) != 2 {
		t.Fatalf("episodes: %d, %v", len(episodes.Items), err)
	}
	id := episodes.Items[0].ID
	hour := time.Hour
	progressAs := func(st string, offset time.Duration, resumeAfter time.Duration) frontend.CatchUpWatch {
		t.Helper()
		if err := h.CatchUpProgress(t.Context(), owner, id, offset, hour, st, resumeAfter); err != nil {
			t.Fatalf("progress %v: %v", offset, err)
		}
		return catchUpEpisode(t, h, owner, id).Watch
	}
	progress := func(offset, resumeAfter time.Duration) frontend.CatchUpWatch {
		t.Helper()
		return progressAs("playing", offset, resumeAfter)
	}
	if w := catchUpEpisode(t, h, owner, id).Watch; w != (frontend.CatchUpWatch{}) {
		t.Fatalf("unplayed airing has state: %+v", w)
	}
	if w := progressAs("stopped", 30*time.Second, 0); w != (frontend.CatchUpWatch{}) {
		t.Fatalf("a not-started report created state: %+v", w)
	}
	if w := progress(10*time.Minute, 0); w.ViewOffset != 10*time.Minute || w.ViewCount != 0 || !w.LastViewedAt.Equal(now) {
		t.Fatalf("resume point: %+v", w)
	}
	page, err := h.CatchUpItems(t.Context(), owner, frontend.CatchUpQuery{Parent: shows.Items[0].SeasonID})
	if err != nil || page.Items[0].Watch.ViewOffset != 10*time.Minute || page.Items[1].Watch != (frontend.CatchUpWatch{}) {
		t.Fatalf("listing state: %+v, %v", page.Items, err)
	}
	// A resumed play reports 0 (buffering, then playing) before it seeks:
	// neither clears the resume point. Stopping near the start does.
	if w := progressAs("buffering", 0, 0); w.ViewOffset != 10*time.Minute {
		t.Fatalf("buffering at 0 cleared the resume point: %+v", w)
	}
	if w := progress(0, 0); w.ViewOffset != 10*time.Minute {
		t.Fatalf("playing at 0 cleared the resume point: %+v", w)
	}
	if w := progressAs("stopped", time.Minute, 0); w.ViewOffset != 0 || w.ViewCount != 0 {
		t.Fatalf("stopping under 5%% keeps a resume point: %+v", w)
	}
	if w := progress(55*time.Minute, 0); w.ViewOffset != 0 || w.ViewCount != 1 {
		t.Fatalf("crossing 90%% did not mark watched: %+v", w)
	}
	if w := progress(58*time.Minute, 0); w.ViewCount != 1 {
		t.Fatalf("the same viewing counted twice: %+v", w)
	}
	if w := progress(20*time.Minute, 0); w.ViewOffset != 20*time.Minute || w.ViewCount != 1 {
		t.Fatalf("rewatch resume point: %+v", w)
	}
	if w := progress(57*time.Minute, 0); w.ViewCount != 2 || w.ViewOffset != 0 {
		t.Fatalf("second viewing not counted: %+v", w)
	}
	progress(10*time.Minute, 0)
	if w := progressAs("stopped", 90*time.Second, 2*time.Minute); w.ViewOffset != 0 {
		t.Fatalf("configured resume threshold ignored: %+v", w)
	}

	// Mark and unmark, through the GUID the Jellyfin and Emby frontends use.
	guid := frontend.CatchUpGUID(episodes.Items[0])
	if err := h.CatchUpSetWatched(t.Context(), owner, guid, false); err != nil {
		t.Fatal(err)
	}
	if w := catchUpEpisode(t, h, owner, id).Watch; w.ViewCount != 0 || w.ViewOffset != 0 || !w.LastViewedAt.Equal(now) {
		t.Fatalf("unwatched (when it was last viewed is kept): %+v", w)
	}
	progress(30*time.Minute, 0)
	if err := h.CatchUpSetWatched(t.Context(), owner, guid, true); err != nil {
		t.Fatal(err)
	}
	if w := catchUpEpisode(t, h, owner, id).Watch; w.ViewCount != 1 || w.ViewOffset != 0 {
		t.Fatalf("watched: %+v", w)
	}

	// State is per viewer, and a viewer who can't see the archive can't write it.
	other := &core.User{ID: 3, IsOwner: true}
	if w := catchUpEpisode(t, h, other, id).Watch; w != (frontend.CatchUpWatch{}) {
		t.Fatalf("another viewer sees the owner's state: %+v", w)
	}
	denied := &core.User{ID: 2}
	if err := h.CatchUpSetWatched(t.Context(), denied, id, true); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("restricted viewer wrote state: %v", err)
	}
	if err := h.CatchUpSetWatched(t.Context(), owner, shows.Items[0].ID, true); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("a show is not an airing: %v", err)
	}
}

// The airing's own length decides the thresholds, a playhead past its end is
// ignored, and a write outlives a request the player already closed.
func TestCatchUpWatchGuards(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s, ch, h := catchUpWatchFixture(t, now)
	owner := &core.User{ID: 1, IsOwner: true}
	id := frontend.CatchUpEpisodeID(ch.ID + "-" + msText(now.Add(-3*time.Hour).UnixMilli()))
	// A 10-minute client "duration" on the 60-minute airing: 9 minutes is a
	// resume point, not 90% watched.
	if err := h.CatchUpProgress(t.Context(), owner, id, 9*time.Minute, 10*time.Minute, "playing", 0); err != nil {
		t.Fatal(err)
	}
	if w := catchUpEpisode(t, h, owner, id).Watch; w.ViewOffset != 9*time.Minute || w.ViewCount != 0 {
		t.Fatalf("client duration used: %+v", w)
	}
	if err := h.CatchUpProgress(t.Context(), owner, id, 63*time.Minute, 0, "stopped", 0); err != nil {
		t.Fatal(err)
	}
	if w := catchUpEpisode(t, h, owner, id).Watch; w.ViewOffset != 9*time.Minute || w.ViewCount != 0 {
		t.Fatalf("a playhead past the end was saved: %+v", w)
	}
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.CatchUpProgress(gone, owner, id, 20*time.Minute, 0, "stopped", 0); err != nil {
		t.Fatalf("write on a closed request: %v", err)
	}
	if w := catchUpEpisode(t, h, owner, id).Watch; w.ViewOffset != 20*time.Minute {
		t.Fatalf("closed request lost the position: %+v", w)
	}
	if err := s.DeleteSource(t.Context(), guideSource(ch)); err != nil {
		t.Fatalf("delete source: %v", err)
	}
	var left int
	if err := s.o.DB.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM livetv_catchup_watch").Scan(&left); err != nil || left != 0 {
		t.Fatalf("deleting the source left watch rows: %d, %v", left, err)
	}
}

// Concurrent marks each count: every write is one statement.
func TestCatchUpWatchConcurrentWrites(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	_, ch, h := catchUpWatchFixture(t, now)
	owner := &core.User{ID: 1, IsOwner: true}
	id := frontend.CatchUpEpisodeID(ch.ID + "-" + msText(now.Add(-3*time.Hour).UnixMilli()))
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- h.CatchUpSetWatched(t.Context(), owner, id, true)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if w := catchUpEpisode(t, h, owner, id).Watch; w.ViewCount != 20 {
		t.Fatalf("concurrent marks lost: %+v", w)
	}
}

// State lives as long as the airing's guide row: when an import drops the
// airing from the archive, its state goes with it.
func TestCatchUpWatchPrune(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s, ch, h := catchUpWatchFixture(t, now)
	owner := &core.User{ID: 1, IsOwner: true}
	keep := frontend.CatchUpEpisodeID(ch.ID + "-" + msText(now.Add(-3*time.Hour).UnixMilli()))
	drop := frontend.CatchUpEpisodeID(ch.ID + "-" + msText(now.Add(-5*time.Hour).UnixMilli()))
	for _, id := range []string{keep, drop} {
		if err := h.CatchUpProgress(t.Context(), owner, id, 10*time.Minute, time.Hour, "stopped", 0); err != nil {
			t.Fatalf("progress %s: %v", id, err)
		}
	}
	if _, err := s.o.DB.ExecContext(t.Context(), "DELETE FROM livetv_programmes WHERE source_id=? AND channel_id=? AND start_ms=?", guideSource(ch), ch.GuideID, now.Add(-5*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	prune := func(at time.Time) int64 {
		t.Helper()
		tx, err := s.o.DB.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		n, err := pruneCatchUpWatch(t.Context(), tx, guideSource(ch), at)
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := prune(now); n != 1 {
		t.Fatalf("pruned %d rows, want the dropped airing only", n)
	}
	var left int
	if err := s.o.DB.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM livetv_catchup_watch WHERE programme_id=?", ch.ID+"-"+msText(now.Add(-3*time.Hour).UnixMilli())).Scan(&left); err != nil || left != 1 {
		t.Fatalf("kept airing's state: %d, %v", left, err)
	}
	if n := prune(now.Add(32 * 24 * time.Hour)); n != 1 {
		t.Fatalf("state older than any archive not pruned: %d", n)
	}
}

func msText(n int64) string { return strconv.FormatInt(n, 10) }
