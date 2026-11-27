package lineup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSnapshotRefreshAndViewerIsolation(t *testing.T) {
	var catalog, rules atomic.Int64
	var held atomic.Pointer[chan struct{}]
	store := New(t.TempDir(), Options{
		Revision: func(key Key) Revision {
			return Revision{Policy: fmt.Sprint(rules.Load()), Content: fmt.Sprint(catalog.Load())}
		},
		Build: func(ctx context.Context, key Key, out io.Writer) error {
			r, c := rules.Load(), catalog.Load()
			if gate := held.Load(); gate != nil {
				select {
				case <-*gate:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			title := "public"
			if !key.Restricted && r == 0 {
				title = "owner-only"
			}
			_, err := fmt.Fprintf(out, "%s viewer=%d catalogue=%d\n", title, key.Viewer, c)
			return err
		},
	})
	t.Cleanup(store.Close)
	owner, restricted := Key{Viewer: 1}, Key{Viewer: 2, Restricted: true}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	read := func(key Key) string {
		t.Helper()
		f, err := store.Open(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		b, err := io.ReadAll(f)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := read(owner); got != "owner-only viewer=1 catalogue=0\n" {
		t.Fatal(got)
	}
	if got := read(restricted); got != "public viewer=2 catalogue=0\n" {
		t.Fatal(got)
	}

	catalog.Store(1)
	if got := read(owner); !strings.Contains(got, "catalogue=0") {
		t.Fatalf("catalogue refresh did not retain the last successful snapshot: %q", got)
	}
	// Advance the snapshot's age in the test to exercise the debounced
	// scheduler without sleeping through its production interval.
	store.mu.Lock()
	store.slots[owner].ready.at = time.Now().Add(-debounce)
	store.mu.Unlock()
	store.signal()
	for {
		if got := read(owner); strings.Contains(got, "catalogue=1") {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}

	gate := make(chan struct{})
	held.Store(&gate)
	rules.Store(1)
	blocked, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	r, err := store.Open(blocked, owner)
	if r != nil {
		r.Close()
		t.Fatal("served owner snapshot from the previous rules")
	}
	if err == nil {
		t.Fatal("policy mismatch returned success")
	}
	close(gate)
	if got := read(owner); got != "public viewer=1 catalogue=1\n" {
		t.Fatal(got)
	}
	if got := read(restricted); got != "public viewer=2 catalogue=1\n" {
		t.Fatal(got)
	}
}

func TestWriteSnapshotKeepsTitlesLiteral(t *testing.T) {
	body := "#EXTINF:-1,title " + UserMarker + "\n" + BaseMarker + "/movie/" + UserMarker + "/" + PassMarker + "/1.mkv\n"
	var got strings.Builder
	if err := WriteSnapshot(&got, strings.NewReader(body), "https://host/xtream", "alice", "pass"); err != nil {
		t.Fatal(err)
	}
	want := "#EXTINF:-1,title " + UserMarker + "\nhttps://host/xtream/movie/alice/pass/1.mkv\n"
	if got.String() != want {
		t.Fatalf("got %q, want %q", got.String(), want)
	}
}

func TestStoreBuildsOnUseAndClockBoundary(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "lineup-orphan")
	if err := os.Mkdir(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "other-cache")
	if err := os.Mkdir(keep, 0o700); err != nil {
		t.Fatal(err)
	}
	var builds atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	store := New(dir, Options{
		Revision: func(Key) Revision { return Revision{Policy: "policy", Content: "catalogue"} },
		Build: func(ctx context.Context, _ Key, out io.Writer) error {
			if builds.Add(1) == 1 {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			_, err := io.WriteString(out, "lineup")
			return err
		},
	})
	if store == nil {
		t.Fatal("store unavailable")
	}
	t.Cleanup(store.Close)
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan remains: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unrelated directory was removed: %v", err)
	}
	key := Key{Viewer: 1}
	if _, e, _ := store.next(time.Now()); e != nil || builds.Load() != 0 {
		t.Fatal("unused viewer scheduled a build")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	read := func(done chan<- error) {
		f, err := store.Open(ctx, key)
		if err == nil {
			err = f.Close()
		}
		done <- err
	}
	done := make(chan error, 2)
	go read(done)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go read(done)
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	// A snapshot can age without creating work.
	store.mu.Lock()
	store.slots[key].ready.at = time.Now().Add(-10 * time.Minute)
	store.mu.Unlock()
	if _, e, _ := store.next(time.Now()); e != nil || builds.Load() != 1 {
		t.Fatal("age alone scheduled a replacement")
	}
	// A known air or release boundary does create work with the same revisions.
	store.mu.Lock()
	store.slots[key].ready.rev.NextBoundary = time.Now().Add(-time.Second)
	store.mu.Unlock()
	store.signal()
	for builds.Load() < 2 {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
