package plextv

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

func TestFailedConfigPersistenceDoesNotReleaseClaim(t *testing.T) {
	dir := t.TempDir()
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *config.Config) error { c.Plex.Claimed = true; c.Plex.Token = "server-token-42"; return nil }); err != nil {
		t.Fatal(err)
	}
	f := newFakePlexTV(t, store.Get().Server.MachineIdentifier)
	f.addUser("server-token-42", owner())
	f.deviceOwner = 42
	_, err = NewRegistrar(RegistrarOptions{Config: store, Client: newServerClient(t, f)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *config.Config) error { c.Plex.Claimed = false; return nil }); err == nil {
		t.Fatal("expected persistence failure")
	}
	if !store.Get().Plex.Claimed || f.deviceDeletes != 0 {
		t.Fatal("failed write released claim")
	}
}

// subConfig is memConfig with config.Store's Subscribe.
type subConfig struct {
	*memConfig
	mu   sync.Mutex
	subs []func(config.Config)
}

func (s *subConfig) Subscribe(fn func(config.Config)) {
	s.mu.Lock()
	s.subs = append(s.subs, fn)
	s.mu.Unlock()
}

func (s *subConfig) Update(fn func(*config.Config) error) error {
	if err := s.memConfig.Update(fn); err != nil {
		return err
	}
	s.mu.Lock()
	subs := append([]func(config.Config){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(s.Get())
	}
	return nil
}

func newReclaimRegistrar(t *testing.T, f *fakePlexTV, store ConfigStore) *Registrar {
	t.Helper()
	r, err := NewRegistrar(RegistrarOptions{
		Config: store, Client: newServerClient(t, f), Users: &fakeUsers{},
		WAN: &fakeWAN{ip: "198.51.100.1"}, Mapper: &fakeMapper{},
		LAN: func() ([]net.IP, error) { return []net.IP{net.ParseIP("192.168.1.5")}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func accountB() Account {
	return Account{ID: 43, UUID: "uuid-43", Username: "other", Title: "Other Owner", Email: "b@example.com"}
}

func TestReclaimByAnotherAccountReleasesTheDevice(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("user-token", owner())
	f.addUser("user-b", accountB())
	store := &subConfig{memConfig: &memConfig{cfg: testConfig()}}
	r := newReclaimRegistrar(t, f, store)
	ctx := context.Background()

	if _, err := r.Claim(ctx, "user-token"); err != nil {
		t.Fatal(err)
	}
	r.runDue(ctx)
	if f.deviceOwner != 42 {
		t.Fatalf("device owner = %d after publishing, want 42", f.deviceOwner)
	}

	// The owner switches Claimed off in Settings.
	if err := store.Update(func(c *config.Config) error { c.Plex.Claimed = false; return nil }); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for store.Get().Plex.Token != "" {
		if time.Now().After(deadline) {
			t.Fatal("old server token was not released")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f.deviceOwner != 0 || f.deviceDeletes != 1 {
		t.Fatalf("device owner %d, deletes %d: the old account kept the device", f.deviceOwner, f.deviceDeletes)
	}

	// Account B signs in and claims: publishing works under B.
	if _, err := r.Claim(ctx, "user-b"); err != nil {
		t.Fatal(err)
	}
	r.runDue(ctx)
	st := r.Status()
	if st.LastPublishError != "" || st.TokenRejected || f.deviceOwner != 43 {
		t.Fatalf("publish under the new account: err=%q rejected=%v owner=%d", st.LastPublishError, st.TokenRejected, f.deviceOwner)
	}
}

func TestReclaimReportsATokenPlexRejects(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("user-b", accountB())
	f.deviceOwner = 42 // still on the previous account (e.g. Ether 0.2.18 never released it)
	f.accessXML = accessTokensXML
	f.usersXML = usersXML
	store := &subConfig{memConfig: &memConfig{cfg: testConfig()}}
	r := newReclaimRegistrar(t, f, store)
	ctx := context.Background()

	if _, err := r.Claim(ctx, "user-b"); err != nil {
		t.Fatal(err)
	}
	r.runDue(ctx)
	st := r.Status()
	if !st.TokenRejected || st.LastPublishError == "" {
		t.Fatalf("a 401 from plex.tv must be reported: %+v", st)
	}

	// Unclaim from Ether cannot remove another account's device; the
	// rejection status clears, and the token is forgotten.
	if err := r.Unclaim(ctx); err != nil {
		t.Fatal(err)
	}
	if st := r.Status(); st.TokenRejected || st.LastPublishError != "" || store.Get().Plex.Token != "" {
		t.Fatalf("after unclaim: %+v token=%q", st, store.Get().Plex.Token)
	}
}

// Restart and an unrelated settings edit must not release a persisted claim.
func TestClaimSurvivesRestartAndSettingsUpdate(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	f.deviceOwner = 42
	cfg := testConfig()
	cfg.Plex.Token, cfg.Plex.Claimed = "server-token-42", true
	store := &subConfig{memConfig: &memConfig{cfg: cfg}}
	r := newReclaimRegistrar(t, f, store)
	if err := store.Update(func(c *config.Config) error { c.Server.FriendlyName = "Updated"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.PublishNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A new registrar models startup against the persisted configuration.
	r2 := newReclaimRegistrar(t, f, store)
	if err := r2.PublishNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.deviceDeletes != 0 || f.deviceOwner != 42 || !store.Get().Plex.Claimed || store.Get().Plex.Token != "server-token-42" {
		t.Fatal("claim changed during settings update or restart")
	}
}

func TestQueuedReleaseDoesNotDeleteNewClaim(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	f.deviceOwner = 42
	cfg := testConfig()
	cfg.Plex.Token, cfg.Plex.Claimed = "server-token-42", true
	store := &subConfig{memConfig: &memConfig{cfg: cfg}}
	r := newReclaimRegistrar(t, f, store)
	// Hold the claim lock to force the asynchronous release to wait until the
	// configuration has been reclaimed.
	r.claimMu.Lock()
	if err := store.Update(func(c *config.Config) error { c.Plex.Claimed = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *config.Config) error { c.Plex.Claimed = true; return nil }); err != nil {
		t.Fatal(err)
	}
	r.claimMu.Unlock()
	// Acquire the lock after the queued worker has run, using a bounded wait.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		r.claimMu.Lock()
		deleted := f.deviceDeletes
		r.claimMu.Unlock()
		if deleted != 0 {
			t.Fatal("queued release removed a reclaimed device")
		}
	}
	if !store.Get().Plex.Claimed || f.deviceOwner != 42 {
		t.Fatal("reclaimed device changed")
	}
}

func TestReopenRestoredConfigDoesNotReleaseClaim(t *testing.T) {
	dir := t.TempDir()
	s, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(c *config.Config) error { c.Plex.Claimed = true; c.Plex.Token = "server-token-42"; return nil }); err != nil {
		t.Fatal(err)
	}
	machine := s.Get().Server.MachineIdentifier
	// Restore/recovery applies files before startup; reopen the persisted config.
	restored, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakePlexTV(t, machine)
	f.deviceOwner = 42
	_, err = NewRegistrar(RegistrarOptions{Config: restored, Client: newServerClient(t, f)})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Get().Server.MachineIdentifier != machine || !restored.Get().Plex.Claimed || restored.Get().Plex.Token != "server-token-42" || f.deviceDeletes != 0 {
		t.Fatal("reopened config changed claim or identity")
	}
}
