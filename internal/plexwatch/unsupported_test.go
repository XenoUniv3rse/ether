package plexwatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// newSupported builds a Service with the dormant account-sync path switched
// on, so the other tests keep that plumbing ready for a working credential.
func newSupported(opts Options) (*Service, error) {
	s, err := New(opts)
	if s != nil {
		s.supported = true
	}
	return s, err
}

// While AccountSyncSupported is off, no code path may reach Plex: queued
// intents and held attempts stay parked, imports never run, and the viewer
// sees "unsupported" instead of an error that reads as their fault.
func TestUnsupportedNeverContactsPlex(t *testing.T) {
	if AccountSyncSupported {
		t.Skip("account sync is supported")
	}
	ctx := context.Background()
	h := newHarness(t, 42)
	h.connect(t, 2, 42)
	if _, err := h.svc.SetDirection(ctx, 2, 42, 3, 5); err != nil {
		t.Fatal(err)
	}
	// One uncertain send leaves a held attempt; a second change stays queued.
	held, queued := core.MovieKey(550), core.MovieKey(551)
	h.guid.forward[held] = "plex://movie/5d7768265af944001f1f6976"
	h.guid.forward[queued] = "plex://movie/5d7768265af944001f1f6977"
	h.fake.postStatus = 500
	if err := h.st.Scrobble(ctx, 2, held); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.st.Scrobble(ctx, 2, queued); err != nil {
		t.Fatal(err)
	}
	before, err := h.svc.Status(ctx, 2, 42)
	if err != nil || before.Held != 1 || before.Pending != 1 {
		t.Fatalf("setup status %+v %v", before, err)
	}

	h.svc.supported = false
	h.fake.mu.Lock()
	calls := len(h.fake.paths)
	h.fake.mu.Unlock()
	logged := h.logs.Len()
	h.now.Add(3 * time.Hour) // imports, reconciliation and compaction are all due
	for i := 0; i < 3; i++ {
		h.svc.deliverAvailable(ctx)
		h.svc.importDue(ctx)
	}
	if more, err := h.svc.DeliverOnce(ctx); more || err != nil {
		t.Fatalf("deliver %v %v", more, err)
	}
	if more, err := h.svc.ReconcileOnce(ctx); more || err != nil {
		t.Fatalf("reconcile %v %v", more, err)
	}
	for name, call := range map[string]func() (Status, error){
		"import":  func() (Status, error) { return h.svc.ImportCurrent(ctx, 2, 42) },
		"export":  func() (Status, error) { return h.svc.ExportCurrent(ctx, 2, 42) },
		"retry":   func() (Status, error) { return h.svc.Retry(ctx, 2, 42) },
		"resolve": func() (Status, error) { return h.svc.Resolve(ctx, 2, 42) },
		"pin":     func() (Status, error) { return h.svc.BeginPIN(ctx, 2, 42) },
		"finish":  func() (Status, error) { return h.svc.FinishPIN(ctx, 2, 42) },
		"enable":  func() (Status, error) { return h.svc.SetDirection(ctx, 2, 42, 1, 10) },
	} {
		if _, err := call(); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	h.fake.mu.Lock()
	after := append([]string(nil), h.fake.paths[calls:]...)
	h.fake.mu.Unlock()
	if len(after) != 0 {
		t.Fatalf("contacted Plex while unsupported: %v", after)
	}
	if extra := h.logs.String()[logged:]; strings.Contains(extra, "level=WARN") {
		t.Fatalf("logged while parked: %s", extra)
	}
	st, err := h.svc.Status(ctx, 2, 42)
	if err != nil {
		t.Fatal(err)
	}
	if st.Eligibility != "unsupported" || st.Problem != "unsupported" || !st.Connected || st.Direction != 3 ||
		st.Held != 1 || st.Pending != 1 {
		t.Fatalf("parked status %+v", st)
	}
	if st, err = h.svc.Disconnect(ctx, 2, 42); err != nil || st.Connected {
		t.Fatalf("disconnect %+v %v", st, err)
	}
}
