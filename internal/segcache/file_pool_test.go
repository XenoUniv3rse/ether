package segcache_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/segcache"
	"github.com/braedonsaunders/ether/internal/store"
)

type mediaNode struct {
	seg   *segcache.Cache
	files *blobcache.Cache
	games *blobcache.Namespace
}

func startMediaNode(t *testing.T, dir string, db *sql.DB, id string) mediaNode {
	t.Helper()
	var seg *segcache.Cache
	var err error
	if db == nil {
		seg, err = segcache.Open(dir, 1000, nil)
	} else {
		seg, err = segcache.OpenShared(dir, 1000, db, id, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seg.Close() })
	files, err := blobcache.Open(filepath.Join(dir, "plugin-files"), blobcache.Options{Unified: true, Shared: db != nil, SweepPeriod: -1, PoolCapacity: seg.MediaFileCapacity, MakeFileRoom: seg.MakeFileRoom, CheckVolume: seg.CheckVolume})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seg.SetFilePool(nil); files.Close() })
	seg.SetFilePool(files)
	return mediaNode{seg, files, files.Namespace("games", blobcache.Quota{})}
}

func TestMediaPoolMixedAdmission(t *testing.T) {
	for _, mode := range []string{"local", "shared"} {
		t.Run(mode, func(t *testing.T) {
			var db *sql.DB
			if mode == "shared" {
				var err error
				db, err = store.OpenMemory(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Close() })
			}
			dir := t.TempDir()
			a := startMediaNode(t, dir, db, "a")
			b := a
			if db != nil {
				b = startMediaNode(t, dir, db, "b")
			}
			if err := a.seg.Put("movie", 0, make([]byte, 500)); err != nil {
				t.Fatal(err)
			}
			payload := bytes.Repeat([]byte{0xA5}, 700)
			ref, err := b.games.Put(context.Background(), "disc.iso", bytes.NewReader(payload), int64(len(payload)))
			if err != nil {
				t.Fatal(err)
			}
			if a.seg.Has("movie", 0) {
				t.Fatal("eligible segments remained above budget")
			}
			game, err := a.games.Open(ref)
			if err != nil {
				t.Fatal(err)
			}
			defer game.Close()
			if u, budget := a.seg.Usage(); u != 700 || budget != 1000 {
				t.Fatalf("usage %d/%d", u, budget)
			}
			if err := b.seg.Put("too-large-with-reader", 0, make([]byte, 500)); err == nil {
				t.Fatal("active disc reader lost its protection")
			}
			b.games.Purge()
			if _, ok := b.games.LookupKey("disc.iso"); !ok {
				t.Fatal("clear removed a file during a read")
			}
			got, err := io.ReadAll(game)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("peer disc read: %v", err)
			}
			game.Close()
			if err := a.seg.Put("movie2", 0, make([]byte, 500)); err != nil {
				t.Fatal(err)
			}
			if _, ok := b.games.LookupKey("disc.iso"); ok {
				t.Fatal("idle disc was not evicted")
			}
			if err := a.seg.Pin("movie2", "owner"); err != nil {
				t.Fatal(err)
			}
			p, err := b.games.OpenPartial(context.Background(), "next-disc")
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if err := p.Reserve(600); err == nil {
				t.Fatal("pinned segments lost budget protection")
			}
			if err := p.Reserve(400); err != nil {
				t.Fatal(err)
			}
			if err := a.seg.Put("late", 0, make([]byte, 200)); err == nil {
				t.Fatal("segment spent outstanding file reservation")
			}
			if !a.seg.Has("movie2", 0) {
				t.Fatal("pinned movie disappeared")
			}
		})
	}
}

// The volume floor can impose a tighter bound than the configured budget.
// A pinned playback release survives while an idle library file yields space.
func TestMediaPoolVolumeFloor(t *testing.T) {
	node := startMediaNode(t, t.TempDir(), nil, "a")
	var free atomic.Int64
	free.Store(1500)
	node.seg.SetDiskUsage(func() (int64, int64, error) { return 10000, free.Load(), nil })
	if err := node.seg.Pin("playing", "session"); err != nil {
		t.Fatal(err)
	}
	if err := node.seg.Put("playing", 0, make([]byte, 200)); err != nil {
		t.Fatal(err)
	}
	_, err := node.games.Put(context.Background(), "disc", bytes.NewReader(make([]byte, 300)), 300)
	if err != nil {
		t.Fatal(err)
	}
	free.Store(550)
	if err := node.seg.Put("next", 0, make([]byte, 250)); err != nil {
		t.Fatal(err)
	}
	if _, ok := node.games.LookupKey("disc"); ok {
		t.Fatal("free space floor did not reclaim idle library file")
	}
	if err := node.seg.ClearUnpinned("playing"); err == nil {
		t.Fatal("clear removed a pinned release")
	}
	if !node.seg.Has("playing", 0) {
		t.Fatal("space pressure displaced a pinned release")
	}
}

func TestMediaPoolConcurrentStoreAdmissions(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	dir := t.TempDir()
	a := startMediaNode(t, dir, db, "a")
	b := startMediaNode(t, dir, db, "b")
	if err := a.seg.Pin("playing", "session"); err != nil {
		t.Fatal(err)
	}
	partial, err := b.games.OpenPartial(ctx, "fetching")
	if err != nil {
		t.Fatal(err)
	}
	defer partial.Close()

	start := make(chan struct{})
	segment, file := make(chan error, 1), make(chan error, 1)
	go func() { <-start; segment <- a.seg.Put("playing", 0, make([]byte, 600)) }()
	go func() { <-start; file <- partial.Reserve(600) }()
	close(start)
	segmentErr, fileErr := <-segment, <-file
	if (segmentErr == nil) == (fileErr == nil) {
		t.Fatalf("one of two claims must fit: segment=%v file=%v", segmentErr, fileErr)
	}
}

func TestMediaPoolAccountsForSegmentsOnRestart(t *testing.T) {
	dir := t.TempDir()
	old, err := segcache.Open(dir, 1000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Pin("pinned", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := old.Put("pinned", 0, make([]byte, 700)); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	node := startMediaNode(t, dir, nil, "a")
	_, err = node.games.Put(context.Background(), "disc", bytes.NewReader(make([]byte, 500)), 500)
	if err == nil {
		t.Fatal("unindexed pinned segments were omitted from admission")
	}
	if err := node.seg.Put("too-large", 0, make([]byte, 500)); err == nil {
		t.Fatal("segment claimed bytes held before restart")
	}
	if used, _ := node.seg.Usage(); used != 700 {
		t.Fatalf("usage after restart %d, want 700", used)
	}
}
