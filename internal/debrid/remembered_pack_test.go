package debrid

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// The same mutation must be safe across a lost response, simultaneous pack
// consumers, and a service restart with a cold account-list snapshot.
func TestRealDebridAmbiguousPackAcquisitionSurvivesRestart(t *testing.T) {
	fake := (&fakeRD{cached: true}).handler(t)
	var added, selected, emptyList, blockedUnlock atomic.Bool
	const files = `[{"id":1,"path":"/Show.S01E01.mkv","bytes":4000000000,"selected":%d},{"id":2,"path":"/Show.S01E02.mkv","bytes":4000000000,"selected":%d},{"id":3,"path":"/Show.S01E03.mkv","bytes":4000000000,"selected":%d}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/torrents":
			if added.Load() && !emptyList.Load() {
				status := "waiting_files_selection"
				if selected.Load() {
					status = "downloaded"
				}
				fmt.Fprintf(w, `[{"id":"T1","hash":%q,"status":%q}]`, testHash, status)
				return
			}
			fmt.Fprint(w, `[]`)
		case "/torrents/addMagnet":
			if added.Swap(true) {
				t.Error("issued a second mutation for the same account torrent")
				w.WriteHeader(http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusBadGateway)
		case "/torrents/info/T1":
			if selected.Load() {
				fmt.Fprintf(w, `{"id":"T1","hash":%q,"status":"downloaded","files":`+files+`,"links":["https://rd.example/d/E1","https://rd.example/d/E2","https://rd.example/d/E3"]}`, testHash, 1, 1, 1)
				return
			}
			fmt.Fprintf(w, `{"id":"T1","hash":%q,"status":"waiting_files_selection","files":`+files+`,"links":[]}`, testHash, 0, 0, 0)
		case "/torrents/selectFiles/T1":
			if selected.Swap(true) {
				t.Error("issued a second selection for the same account torrent")
			}
			w.WriteHeader(http.StatusNoContent)
		case "/unrestrict/link":
			_ = r.ParseForm()
			if strings.HasSuffix(r.PostForm.Get("link"), "/E3") {
				if blockedUnlock.Swap(true) {
					t.Error("retried a fresh blocked-file answer after restart")
				}
				w.WriteHeader(http.StatusUnavailableForLegalReasons)
				return
			}
			fake.ServeHTTP(w, r)
		default:
			fake.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	db := store.OpenTest(t)
	cfg := config.Debrid{CachedOnly: true, RemoveUncached: true, Providers: []config.DebridProvider{
		{ID: "rd", Kind: config.DebridRealDebrid, APIKey: testKey, Enabled: true},
	}}
	newService := func() *Service { return NewService(cfg, Options{DB: db, ProviderOptions: fastOpts(srv.URL)}) }
	release := func(ep int) core.Release {
		return core.Release{ID: fmt.Sprintf("episode-%d", ep), InfoHash: testHash, Protocol: core.ProtocolTorrent,
			Target: &core.ReleaseTarget{Season: 1, Episode: ep}}
	}
	play := func(service *Service, ep int) error {
		data, err := service.Acquire(t.Context(), release(ep))
		if err != nil {
			return err
		}
		p, err := DecodePayload(data)
		if err == nil && !strings.HasSuffix(p.Path, fmt.Sprintf("E%02d.mkv", ep)) {
			return fmt.Errorf("wrong pack file %q for episode %d", p.Path, ep)
		}
		return err
	}
	service := newService()
	results := make(chan error, 2)
	start := make(chan struct{})
	for ep := range 2 {
		go func() {
			<-start
			results <- play(service, ep+1)
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	// Drop playback payloads and hide the pack from the account's recent
	// list to exercise the independently persisted account answer.
	emptyList.Store(true)
	if _, err := db.ExecContext(t.Context(), "DELETE FROM debrid_payloads"); err != nil {
		t.Fatal(err)
	}
	reopened := newService()
	if err := play(reopened, 1); err != nil {
		t.Fatal(err)
	}
	if err := play(reopened, 3); !errors.Is(err, ErrBlockedFile) || errors.Is(err, ErrBlockedHash) {
		t.Fatalf("bare 451 classified as %v", err)
	}
	if _, err := db.ExecContext(t.Context(), "DELETE FROM debrid_payloads"); err != nil {
		t.Fatal(err)
	}
	reopened = newService()
	if err := play(reopened, 3); !errors.Is(err, ErrBlockedFile) {
		t.Fatalf("blocked-file answer after restart: %v", err)
	}
	match, ok := reopened.CachedPackMatcher(t.Context(), release(2))
	if !ok || match == nil || !match(release(2).Target) || match(release(3).Target) {
		t.Fatal("remembered pack did not exclude the blocked episode")
	}
	if err := play(reopened, 2); err != nil {
		t.Fatalf("sibling of blocked file: %v", err)
	}
}
