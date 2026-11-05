package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestPendingStopSurvivesWriterAndCompletionIsNotDowngraded(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenPath(ctx, filepath.Join(t.TempDir(), "watch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, Options{PendingSQLite: true})
	defer s.Close(ctx)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(234)
	start := time.Now()
	if err := s.Progress(ctx, 1, key, 50*time.Minute, 100*time.Minute, "stopped"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("report waited for writer")
	}
	ws, err := s.Get(ctx, 1, key)
	if err != nil || ws.ViewOffset != 50*time.Minute {
		t.Fatalf("pending stop: %+v, %v", ws, err)
	}
	items, err := s.InProgress(ctx, 1, 10)
	if err != nil || len(items) != 1 || items[0].ViewOffset != 50*time.Minute {
		t.Fatalf("pending continue watching: %+v, %v", items, err)
	}
	other := core.MovieKey(235)
	stopped := core.MovieKey(236)
	if err := s.Progress(ctx, 1, stopped, 35*time.Minute, 100*time.Minute, "stopped"); err != nil {
		t.Fatal(err)
	}
	if err := s.Progress(ctx, 1, other, 40*time.Minute, 100*time.Minute, "stopped"); err != nil {
		t.Fatal(err)
	}
	if err := s.Scrobble(ctx, 1, other); err != nil {
		t.Fatal(err)
	}
	if err := s.Progress(ctx, 1, other, 20*time.Minute, 100*time.Minute, "playing"); err != nil {
		t.Fatal(err)
	}
	marked, err := s.Get(ctx, 1, other)
	if err != nil || marked.ViewCount != 1 || marked.ViewOffset != 0 || marked.Duration != 100*time.Minute {
		t.Fatalf("pending scrobble: %+v, %v", marked, err)
	}
	if err := s.Progress(ctx, 1, key, 95*time.Minute, 100*time.Minute, "stopped"); err != nil {
		t.Fatal(err)
	}
	if err := s.Progress(ctx, 1, key, 20*time.Minute, 100*time.Minute, "playing"); err != nil {
		t.Fatal(err)
	}
	ws, err = s.Get(ctx, 1, key)
	if err != nil || ws.ViewCount != 1 || ws.ViewOffset != 0 {
		t.Fatalf("pending completion: %+v, %v", ws, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.pending.mu.Lock()
		n := len(s.pending.entries)
		s.pending.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending write not applied")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ws, err = s.Get(ctx, 1, key)
	if err != nil || ws.ViewCount != 1 || ws.ViewOffset != 0 {
		t.Fatalf("applied completion: %+v, %v", ws, err)
	}
	marked, err = s.Get(ctx, 1, other)
	if err != nil || marked.ViewCount != 1 || marked.Duration != 100*time.Minute {
		t.Fatalf("applied scrobble: %+v, %v", marked, err)
	}
	ws, err = s.Get(ctx, 1, stopped)
	if err != nil || ws.ViewOffset != 35*time.Minute {
		t.Fatalf("applied stop: %+v, %v", ws, err)
	}
}
