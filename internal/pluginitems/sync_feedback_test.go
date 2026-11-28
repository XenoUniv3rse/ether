package pluginitems

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
)

type recordedJob struct {
	pluginID string
	kindKey  string
	path     string
	label    string
	state    string
	code     string
	progress []string
	outcome  string
	errMsg   string
}

type fakeSyncJobTracker struct {
	mu   sync.Mutex
	jobs map[string]*recordedJob
}

func (f *fakeSyncJobTracker) BeginSyncJob(pluginID, kindKey, routePath, label string, _ context.CancelFunc) SyncJobHandle {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jobs == nil {
		f.jobs = map[string]*recordedJob{}
	}
	j := &recordedJob{
		pluginID: pluginID,
		kindKey:  kindKey,
		path:     routePath,
		label:    label,
		state:    "running",
	}
	f.jobs[pluginID+"|"+kindKey] = j
	return &fakeSyncJobHandle{tracker: f, job: j}
}

func (f *fakeSyncJobTracker) UpdateSyncJob(pluginID, kindKey, state, code, outcome, errMsg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[pluginID+"|"+kindKey]
	if j == nil {
		return
	}
	j.state = state
	j.code = code
	j.outcome = outcome
	j.errMsg = errMsg
}

func (f *fakeSyncJobTracker) get(pluginID, kindKey string) recordedJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j := f.jobs[pluginID+"|"+kindKey]; j != nil {
		cp := *j
		cp.progress = append([]string(nil), j.progress...)
		return cp
	}
	return recordedJob{}
}

type fakeSyncJobHandle struct {
	tracker *fakeSyncJobTracker
	job     *recordedJob
}

func (h *fakeSyncJobHandle) SetProgress(progress string) {
	h.tracker.mu.Lock()
	h.job.progress = append(h.job.progress, progress)
	h.tracker.mu.Unlock()
}

func (h *fakeSyncJobHandle) Finish(state, code, outcome, errMsg string, _ *plugins.RouteFailure) {
	h.tracker.mu.Lock()
	h.job.state = state
	h.job.code = code
	h.job.outcome = outcome
	h.job.errMsg = errMsg
	h.tracker.mu.Unlock()
}

func TestSyncProtectsEngagedAndPinnedItemsAndAncestors(t *testing.T) {
	ctx := context.Background()
	fc := &fakeSyncCaller{
		rootPages: map[string]core.KindPage{
			"": {
				Items: []core.KindItem{
					{ID: "s1", Level: "series", Title: "Catalog Series", ETag: "v1"},
					{ID: "s2", Level: "series", Title: "Parent Of Opened Book", ETag: "v1"},
					{ID: "s3", Level: "series", Title: "Catalog Series 3", ETag: "v1"},
					{ID: "s4", Level: "series", Title: "Catalog Series 4", ETag: "v1"},
					{ID: "s5", Level: "series", Title: "Catalog Series 5", ETag: "v1"},
					{ID: "s6", Level: "series", Title: "Stale Catalog Series", ETag: "v1"},
				},
			},
		},
		childPages: map[string]core.KindPage{
			"s2|": {
				Items: []core.KindItem{
					{ID: "b_child_open", Level: "book", ParentID: "s2", Title: "Opened Child Book", ETag: "v1"},
				},
			},
			"s1|": {
				Items: []core.KindItem{
					{ID: "b_played", Level: "book", ParentID: "s1", Title: "Played Book", ETag: "v1"},
					{ID: "b_fav", Level: "book", ParentID: "s1", Title: "Favourited Book", ETag: "v1"},
				},
			},
		},
	}

	s, db := openTestService(t, Options{Caller: fc})
	if _, err := s.UpsertKind(ctx, "ether.books", 20, sampleBookDecl(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncKind(ctx, "ether.books", "book", SyncOptions{}); err != nil {
		t.Fatalf("initial SyncKind: %v", err)
	}

	// Create viewer engagement tables matching production state/favorites schemas.
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS state_watch (user_id BIGINT NOT NULL, rating_key BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS favorites_items (user_id INTEGER NOT NULL, kind TEXT NOT NULL, subject_id TEXT NOT NULL)`,
	} {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			t.Fatalf("create engagement table: %v", err)
		}
	}

	// 1. Live-searched item inserted via UpsertPinnedItems.
	if _, err := s.UpsertPinnedItems(ctx, "ether.books", "book", []core.KindItem{
		{ID: "s_search", Level: "series", Title: "Live Searched Series", ETag: "v1"},
	}); err != nil {
		t.Fatalf("UpsertPinnedItems: %v", err)
	}

	// 2. Opened child book pinned via PinItem -> must protect BOTH b_child_open and its parent s2.
	childItem, _, err := s.GetByRef(ctx, core.NewItemRef("ether.books", "book", "b_child_open"))
	if err != nil {
		t.Fatalf("get b_child_open: %v", err)
	}
	if err := s.PinItem(ctx, childItem.Ref); err != nil {
		t.Fatalf("PinItem: %v", err)
	}

	// 3. Played book in state_watch.
	playedItem, _, err := s.GetByRef(ctx, core.NewItemRef("ether.books", "book", "b_played"))
	if err != nil {
		t.Fatalf("get b_played: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO state_watch(user_id, rating_key) VALUES(1, ?)`, int64(playedItem.Key)); err != nil {
		t.Fatalf("insert state_watch: %v", err)
	}

	// 4. Favourited book in favorites_items.
	favItem, _, err := s.GetByRef(ctx, core.NewItemRef("ether.books", "book", "b_fav"))
	if err != nil {
		t.Fatalf("get b_fav: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO favorites_items(user_id, kind, subject_id) VALUES(1, 'title', ?)`, strconv.FormatInt(int64(favItem.Key), 10)); err != nil {
		t.Fatalf("insert favorites_items: %v", err)
	}

	// Next walk returns ONLY s1, s3, s4, s5 (dropping s6, s2, b_child_open, b_played, b_fav, s_search).
	// Unseen prunable = only s6 (1 of 5 prunable items = 20% <= 50%), so the guard must NOT trip
	// even though 6 of 10 total stored rows were absent from the walk.
	fc.mu.Lock()
	fc.rootPages = map[string]core.KindPage{
		"": {
			Items: []core.KindItem{
				{ID: "s1", Level: "series", Title: "Catalog Series", ETag: "v1"},
				{ID: "s3", Level: "series", Title: "Catalog Series 3", ETag: "v1"},
				{ID: "s4", Level: "series", Title: "Catalog Series 4", ETag: "v1"},
				{ID: "s5", Level: "series", Title: "Catalog Series 5", ETag: "v1"},
			},
		},
	}
	fc.childPages = map[string]core.KindPage{}
	fc.mu.Unlock()

	res, err := s.SyncKind(ctx, "ether.books", "book", SyncOptions{FullWalk: true, Force: true, Manual: true})
	if err != nil {
		t.Fatalf("forced rewalk should not trip guard when unseen items are protected: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1 (only s6)", res.Deleted)
	}
	if res.Protected != 5 {
		t.Fatalf("Protected = %d, want 5 (s_search, b_child_open, ancestor s2, b_played, b_fav)", res.Protected)
	}

	// s6 is gone; every protected item and ancestor kept its original RatingKey.
	if _, _, err := s.GetByRef(ctx, core.NewItemRef("ether.books", "book", "s6")); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected s6 deleted, got err=%v", err)
	}
	for _, id := range []string{"s_search", "s2", "b_child_open", "b_played", "b_fav"} {
		if _, _, err := s.GetByRef(ctx, core.NewItemRef("ether.books", "book", id)); err != nil {
			t.Fatalf("expected protected item %s to survive, got err=%v", id, err)
		}
	}
	afterPlayed, _, err := s.GetByRef(ctx, playedItem.Ref)
	if err != nil || afterPlayed.Key != playedItem.Key {
		t.Fatalf("played item rating key changed: before=%d after=%d err=%v", playedItem.Key, afterPlayed.Key, err)
	}
}

func TestDeleteGuardChoiceKeepAndForceRemoveWithJobProgress(t *testing.T) {
	ctx := context.Background()
	tracker := &fakeSyncJobTracker{}
	fc := &fakeSyncCaller{
		rootPages: map[string]core.KindPage{
			"": {
				Items: []core.KindItem{
					{ID: "s1", Level: "series", Title: "Series 1", ETag: "v1"},
					{ID: "s2", Level: "series", Title: "Series 2", ETag: "v1"},
					{ID: "s3", Level: "series", Title: "Series 3", ETag: "v1"},
					{ID: "s4", Level: "series", Title: "Series 4", ETag: "v1"},
					{ID: "s5", Level: "series", Title: "Series 5", ETag: "v1"},
					{ID: "s6", Level: "series", Title: "Series 6", ETag: "v1"},
				},
			},
		},
	}

	s, _ := openTestService(t, Options{Caller: fc, Jobs: tracker})
	if _, err := s.UpsertKind(ctx, "ether.books", 21, sampleBookDecl(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncKind(ctx, "ether.books", "book", SyncOptions{}); err != nil {
		t.Fatalf("initial SyncKind: %v", err)
	}
	if _, err := s.UpsertPinnedItems(ctx, "ether.books", "book", []core.KindItem{
		{ID: "s_pinned_1", Level: "series", Title: "Pinned 1", ETag: "v1"},
		{ID: "s_pinned_2", Level: "series", Title: "Pinned 2", ETag: "v1"},
	}); err != nil {
		t.Fatalf("UpsertPinnedItems: %v", err)
	}

	// Rotating walk returns only 2 of 6 prunable items -> 4 of 6 missing (>50%).
	fc.mu.Lock()
	fc.rootPages = map[string]core.KindPage{
		"": {
			Items: []core.KindItem{
				{ID: "s1", Level: "series", Title: "Series 1", ETag: "v1"},
				{ID: "s2", Level: "series", Title: "Series 2", ETag: "v1"},
			},
		},
	}
	fc.mu.Unlock()

	_, err := s.SyncKind(ctx, "ether.books", "book", SyncOptions{FullWalk: true, Manual: true})
	var dg *DeleteGuardError
	if !errors.As(err, &dg) {
		t.Fatalf("expected *DeleteGuardError, got %v", err)
	}
	if dg.Source != "walk" || dg.Missing != 4 || dg.Total != 6 || dg.Protected != 2 {
		t.Fatalf("DeleteGuardError = %+v, want walk 4/6 protected=2", dg)
	}

	st, err := s.SyncStateOf(ctx, "ether.books", "book")
	if err != nil {
		t.Fatalf("SyncStateOf: %v", err)
	}
	if st.DeleteGuard == nil || st.DeleteGuard.Missing != 4 || st.DeleteGuard.Total != 6 || st.DeleteGuard.Protected != 2 {
		t.Fatalf("SyncState.DeleteGuard = %+v", st.DeleteGuard)
	}
	job := tracker.get("ether.books", "book")
	if job.state != "error" || job.code != "delete_guard" || len(job.progress) == 0 {
		t.Fatalf("tracked job after guard = %+v", job)
	}

	// Owner chooses "Keep existing items" -> guard/error clear, items stay, job updated.
	keptState, err := s.KeepExistingItems(ctx, "ether.books", "book")
	if err != nil {
		t.Fatalf("KeepExistingItems: %v", err)
	}
	if keptState.DeleteGuard != nil || keptState.LastError != "" {
		t.Fatalf("SyncState after KeepExistingItems = %+v", keptState)
	}
	job = tracker.get("ether.books", "book")
	if job.state != "done" || job.code != "" || !strings.Contains(job.outcome, "Kept all 8 existing items") {
		t.Fatalf("tracked job after KeepExistingItems = %+v", job)
	}
	if _, _, err := s.GetByRef(ctx, core.NewItemRef("ether.books", "book", "s6")); err != nil {
		t.Fatalf("s6 should still exist after KeepExistingItems: %v", err)
	}

	// Re-trip guard, then owner chooses "Remove the 4 missing items" (Force: true).
	// Even with missed_walks == 0, Force removes all 4 missing prunable items immediately
	// while preserving the 2 pinned items.
	if _, err := s.SyncKind(ctx, "ether.books", "book", SyncOptions{FullWalk: true, Manual: true}); !errors.Is(err, ErrDeleteGuard) {
		t.Fatalf("expected ErrDeleteGuard before force, got %v", err)
	}
	res, err := s.SyncKind(ctx, "ether.books", "book", SyncOptions{FullWalk: true, Force: true, Manual: true})
	if err != nil {
		t.Fatalf("forced SyncKind: %v", err)
	}
	if res.Deleted != 4 || res.Protected != 2 {
		t.Fatalf("forced SyncResult = %+v, want Deleted=4 Protected=2", res)
	}
	st, err = s.SyncStateOf(ctx, "ether.books", "book")
	if err != nil {
		t.Fatalf("SyncStateOf after force: %v", err)
	}
	if st.DeleteGuard != nil || st.LastError != "" || st.LastOutcome == nil || st.LastOutcome.Deleted != 4 {
		t.Fatalf("SyncState after force = %+v", st)
	}
	job = tracker.get("ether.books", "book")
	if job.state != "done" || !strings.Contains(job.outcome, "Removed 4 missing items") {
		t.Fatalf("tracked job after force = %+v", job)
	}
	for _, gone := range []string{"s3", "s4", "s5", "s6"} {
		if _, _, err := s.GetByRef(ctx, core.NewItemRef("ether.books", "book", gone)); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("expected %s removed on force, got err=%v", gone, err)
		}
	}
	for _, kept := range []string{"s1", "s2", "s_pinned_1", "s_pinned_2"} {
		if _, _, err := s.GetByRef(ctx, core.NewItemRef("ether.books", "book", kept)); err != nil {
			t.Fatalf("expected %s kept, got err=%v", kept, err)
		}
	}
}

func TestSyncStateOfParsesLegacyDeleteGuardError(t *testing.T) {
	ctx := context.Background()
	s, db := openTestService(t, Options{})
	if _, err := s.UpsertKind(ctx, "ether.books", 22, sampleBookDecl(), false); err != nil {
		t.Fatal(err)
	}
	legacy := "pluginitems: sync aborted by 50% delete guard: 334 of 526 items missing in walk for ether.books/book"
	if _, err := db.ExecContext(ctx,
		`UPDATE plugin_sync_state SET last_error = ?, guard_json = '' WHERE plugin_id = ? AND kind_key = ?`,
		legacy, "ether.books", "book"); err != nil {
		t.Fatal(err)
	}
	st, err := s.SyncStateOf(ctx, "ether.books", "book")
	if err != nil {
		t.Fatal(err)
	}
	if st.DeleteGuard == nil || st.DeleteGuard.Source != "walk" || st.DeleteGuard.Missing != 334 || st.DeleteGuard.Total != 526 {
		t.Fatalf("expected parsed legacy DeleteGuard 334/526, got %+v", st.DeleteGuard)
	}
}
