package plextv

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memBlobs struct {
	mu   sync.Mutex
	data map[string][]byte
	ver  map[string]int64
}

func (b *memBlobs) Get(_ context.Context, name string) ([]byte, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.data[name]...), b.ver[name], nil
}

func (b *memBlobs) Put(_ context.Context, name string, data []byte) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.data == nil {
		b.data, b.ver = map[string][]byte{}, map[string]int64{}
	}
	b.data[name] = append([]byte(nil), data...)
	b.ver[name]++
	return b.ver[name], nil
}

func (b *memBlobs) Version(_ context.Context, name string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ver[name], nil
}

type memEvents struct {
	mu   sync.Mutex
	subs map[string][]func([]byte)
}

func (e *memEvents) Publish(_ context.Context, topic string, p []byte) error {
	e.mu.Lock()
	fns := append([]func([]byte){}, e.subs[topic]...)
	e.mu.Unlock()
	for _, f := range fns {
		f(p)
	}
	return nil
}

func (e *memEvents) Subscribe(topic string, fn func([]byte)) func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.subs == nil {
		e.subs = map[string][]func([]byte){}
	}
	e.subs[topic] = append(e.subs[topic], fn)
	return func() {}
}

func TestCertManagerSharedAcrossNodes(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	c := newServerClient(t, f)
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	blobs := &memBlobs{}
	events := &memEvents{}
	newNode := func(dir string) *CertManager {
		m, err := NewCertManager(CertManagerOptions{Dir: dir, Client: c, Now: clock, Shared: blobs,
			Request: CertRequest{PollInterval: time.Millisecond, Timeout: time.Second}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m.Close) // before t.TempDir's cleanup: no sync outlives the test
		m.SetEventBus(events)
		return m
	}
	ctx := context.Background()

	leader := newNode(t.TempDir())
	info, err := leader.Ensure(ctx, "server-token-42", nil, false)
	if err != nil || info.SelfSigned {
		t.Fatalf("leader: %+v %v", info, err)
	}
	if v, _ := blobs.Version(ctx, SharedCertName); v == 0 {
		t.Fatal("leader did not store the certificate for the cluster")
	}
	requests := f.count("PUT /api/v2/devices/")

	// A follower starting later serves the leader's certificate at once.
	followerDir := t.TempDir()
	follower := newNode(followerDir)
	follower.SetRenewalAllowed(func() bool { return false })
	if got := follower.Info(); got.Fingerprint != info.Fingerprint || got.Hash != fakeHash || got.CertFile == "" {
		t.Fatalf("follower = %+v", got)
	}
	if _, err := follower.GetCertificate(nil); err != nil {
		t.Fatal(err)
	}
	// Its local copy survives a restart without the database.
	cached, err := NewCertManager(CertManagerOptions{Dir: followerDir, Now: clock})
	if err == nil {
		t.Cleanup(cached.Close)
	}
	if err != nil || cached.Info().Fingerprint != info.Fingerprint {
		t.Fatalf("local cache: %+v %v", cached.Info(), err)
	}

	// A node without the event bus.
	quiet, err := NewCertManager(CertManagerOptions{Dir: t.TempDir(), Client: c, Now: clock, Shared: blobs})
	if err == nil {
		t.Cleanup(quiet.Close)
	}
	if err != nil || quiet.Info().Fingerprint != info.Fingerprint {
		t.Fatalf("quiet node: %+v %v", quiet.Info(), err)
	}

	// Near expiry the follower does not ask plex.tv; the leader renews and
	// the follower picks the new certificate up through the bus.
	advance(80 * 24 * time.Hour)
	if _, err := follower.Ensure(ctx, "server-token-42", nil, false); !errors.Is(err, ErrRenewalDeferred) {
		t.Fatalf("follower renewal: %v", err)
	}
	if f.count("PUT /api/v2/devices/") != requests {
		t.Fatal("follower requested a certificate")
	}
	renewed, err := leader.Ensure(ctx, "server-token-42", nil, false)
	if err != nil || renewed.Fingerprint == info.Fingerprint {
		t.Fatalf("leader renewal: %+v %v", renewed, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for follower.Info().Fingerprint != renewed.Fingerprint {
		if time.Now().After(deadline) {
			t.Fatal("follower did not pick up the renewal")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Without the bus, SyncShared (the poll) picks renewals up too.
	if changed, err := quiet.SyncShared(ctx); err != nil || !changed || quiet.Info().Fingerprint != renewed.Fingerprint {
		t.Fatalf("poll sync: changed=%v err=%v", changed, err)
	}
	if changed, _ := quiet.SyncShared(ctx); changed {
		t.Fatal("unchanged version re-installed")
	}
}

func TestCertManagerSharesBackoff(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	f.mu.Lock()
	f.csrStatus = 429
	f.mu.Unlock()
	c := newServerClient(t, f)
	blobs := &memBlobs{}
	now := time.Now()
	opts := func() CertManagerOptions {
		return CertManagerOptions{Dir: t.TempDir(), Client: c, Shared: blobs, Now: func() time.Time { return now },
			Request: CertRequest{PollInterval: time.Millisecond, Timeout: time.Second}}
	}
	a, _ := NewCertManager(opts())
	t.Cleanup(a.Close)
	if _, err := a.Ensure(context.Background(), "server-token-42", nil, false); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v", err)
	}
	n := f.count("PUT /api/v2/devices/")
	// A new leader (after failover) honours the back-off the old one hit.
	b, _ := NewCertManager(opts())
	t.Cleanup(b.Close)
	if b.Info().LastError == "" {
		t.Fatal("back-off state not shared")
	}
	if _, err := b.Ensure(context.Background(), "server-token-42", nil, false); err == nil || f.count("PUT /api/v2/devices/") != n {
		t.Fatal("new leader ignored the shared back-off")
	}
	if !b.Info().SelfSigned {
		t.Fatal("no fallback certificate")
	}
	if v, _ := blobs.Version(context.Background(), SharedCertName); v != 1 {
		t.Fatalf("self-signed fallback was shared (version %d)", v)
	}
}

// stallingBlobs holds Version calls until their context ends, then answers
// anyway, like a store that completes a query the caller gave up on.
type stallingBlobs struct {
	memBlobs
	stall    atomic.Bool
	stalled  chan struct{} // receives once per stalled call
	versions atomic.Int32
}

func (b *stallingBlobs) Version(ctx context.Context, name string) (int64, error) {
	b.versions.Add(1)
	if b.stall.Load() {
		b.stalled <- struct{}{}
		<-ctx.Done()
	}
	return b.memBlobs.Version(ctx, name)
}

// A sync started by a bus event is cancelled and finished by Close: nothing
// is written into the certificate directory after Close returns, and later
// events start no sync.
func TestCertManagerCloseStopsBusSyncs(t *testing.T) {
	blobs := &stallingBlobs{stalled: make(chan struct{}, 1)}
	events := &memEvents{}
	dir := t.TempDir()
	m, err := NewCertManager(CertManagerOptions{Dir: dir, Shared: blobs})
	if err != nil {
		t.Fatal(err)
	}
	m.SetEventBus(events)

	// Another node stores a certificate and announces it.
	b, err := GenerateSelfSigned([]string{"node-b"}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(sharedCertBlob{CertPEM: b.CertPEM, KeyPEM: b.KeyPEM})
	if _, err := blobs.Put(context.Background(), SharedCertName, raw); err != nil {
		t.Fatal(err)
	}
	blobs.stall.Store(true)
	_ = events.Publish(context.Background(), CertTopic, nil)
	<-blobs.stalled // the bus-triggered sync is in flight

	m.Close()
	select {
	case m.op <- struct{}{}:
		t.Fatal("Close returned before in-flight operations finished")
	default:
	}
	if _, err := os.Stat(m.certPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled sync wrote the certificate: %v", err)
	}
	if got := m.Info(); got.Present {
		t.Fatalf("cancelled sync installed a certificate: %+v", got)
	}

	n := blobs.versions.Load()
	_ = events.Publish(context.Background(), CertTopic, nil)
	if _, err := m.SyncShared(context.Background()); !errors.Is(err, ErrCertManagerClosed) {
		t.Fatalf("SyncShared after Close: %v", err)
	}
	if blobs.versions.Load() != n {
		t.Fatal("an event after Close started a sync")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("certificate directory not empty: %v", entries)
	}
}
