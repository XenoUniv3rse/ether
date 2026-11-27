package lineup

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSnapshotRefreshAndViewerIsolation(t *testing.T) {
	var catalog, rules atomic.Int64
	var held atomic.Pointer[chan struct{}]
	store := New(t.TempDir(), Options{
		Viewers: func(context.Context) ([]Key, error) { return []Key{{Viewer: 1}, {Viewer: 2, Restricted: true}}, nil },
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
	if err := store.Wait(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := store.Wait(ctx, restricted); err != nil {
		t.Fatal(err)
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
