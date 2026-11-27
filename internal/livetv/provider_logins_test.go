package livetv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Two nodes compete for provider slots. Viewers on the same local channel
// share one lease; other channels never exceed the panel's per-login maximum.
func TestProviderLoginLeaseInvariant(t *testing.T) {
	s := testService(t, Options{})
	other := testService(t, Options{DB: s.o.DB})
	src := Source{ID: "pool", Auth: &SourceAuth{Type: "xtream", Server: "https://panel.example", Username: "first", Password: "secret"}, Account: &ProviderAccount{Status: "Active", MaxConnections: 1}, Logins: []ProviderLogin{{ID: "second", Username: "second", Password: "another", Account: &ProviderAccount{Status: "Active", MaxConnections: 1}}}}
	for _, node := range []*Service{s, other} {
		node.setSourceEntry(src.ID, authEntry{pool: &src, at: time.Now()})
	}
	ctx := context.Background()
	raw := "https://panel.example/live/first/secret/1.ts"
	l, err := s.leaseLogin(ctx, src.ID, raw, http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := s.leaseLogin(ctx, src.ID, raw, http.Header{})
	if err != nil || shared != l {
		t.Fatalf("channel did not share: %v", err)
	}
	s.releaseLogin(shared)
	if release, err := s.reserveVODProvider(ctx, src.ID, 1, false); !errors.Is(err, ErrProviderBusy) {
		if release != nil {
			release()
		}
		t.Fatalf("VOD bypassed primary login capacity: %v", err)
	}
	var mu sync.Mutex
	var leases []*loginLease
	var owners []*Service
	var unexpected []error
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			node := s
			if i%2 == 1 {
				node = other
			}
			lease, err := node.leaseLogin(ctx, src.ID, fmt.Sprintf("https://panel.example/live/first/secret/%d.ts", i+2), http.Header{})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				leases = append(leases, lease)
				owners = append(owners, node)
			} else if !errors.Is(err, ErrProviderBusy) {
				unexpected = append(unexpected, err)
			}
		}(i)
	}
	wg.Wait()
	if len(unexpected) > 0 || len(leases) != 1 {
		t.Fatalf("slot cap: %d leases, errors %v", len(leases), unexpected)
	}
	if leases[0].login == l.login || leases[0].raw == raw {
		t.Fatal("did not substitute the other login")
	}
	for i, lease := range leases {
		owners[i].releaseLogin(lease)
	}
	s.failedLogin(l)
	s.releaseLogin(l)
	fallback, err := s.leaseLogin(ctx, src.ID, raw, http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.releaseLogin(fallback)
	if fallback.login == l.login {
		t.Fatal("failed login reused")
	}
	var count int
	if err := s.o.DB.QueryRow(`SELECT COUNT(*) FROM livetv_login_slots WHERE until_ms>?`, time.Now().UnixMilli()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("slots: %d %v", count, err)
	}
	t.Run("HTTP failover", exerciseProviderLoginHTTPFailover)
	t.Run("playlist matching", func(t *testing.T) {
		primary := []Channel{{ID: "a", Name: "News", GuideID: "news", StreamURL: "https://a.example/1.ts"}}
		mapped, err := matchLoginLineup(primary, []Channel{{ID: "b", GuideID: "news", StreamURL: "https://b.example/2.ts"}})
		if err != nil || mapped[tapKey(primary[0].StreamURL, nil)] != "https://b.example/2.ts" {
			t.Fatalf("mapping: %v %v", mapped, err)
		}
		if _, err = matchLoginLineup(primary, nil); err == nil {
			t.Fatal("mismatch accepted")
		}
	})
}

func exerciseProviderLoginHTTPFailover(t *testing.T) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/first/") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\n1.ts\n"))
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		for {
			if _, err := w.Write([]byte("shared-provider-media")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
	}))
	defer up.Close()
	s := testService(t, Options{HTTPClient: up.Client()})
	src := Source{ID: "http-pool", Name: "Provider", Auth: &SourceAuth{Type: "xtream", Server: up.URL, Username: "first", Password: "secret"}, Account: &ProviderAccount{Status: "Active", MaxConnections: 1}, Logins: []ProviderLogin{{ID: "second", Username: "second", Password: "other", Account: &ProviderAccount{Status: "Active", MaxConnections: 1}}}}
	b, err := s.encodeSource(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.o.DB.Exec(`INSERT INTO livetv_sources(id,data) VALUES(?,?)`, src.ID, b); err != nil {
		t.Fatal(err)
	}
	c := Channel{ID: "news", SourceID: src.ID, Kind: "iptv"}
	input := StreamSource{ID: stableID(c.ID, "playlist"), URL: up.URL + "/live/first/secret/1.ts", Enabled: true}
	ss := &session{ctx: s.serviceContext(), info: SessionInfo{Channel: c}, inputSource: input, inputURL: input.URL, inputHeaders: http.Header{}}
	if err = s.grantInput(ss, input, http.Header{}); err != nil {
		t.Fatal(err)
	}
	defer s.revokeInput(ss)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	body, _, err := s.sniffInput(ctx, ss, 8)
	if err != nil || string(body) != "shared-p" {
		t.Fatalf("HTTP login failover: %q %v", body, err)
	}
	if g := s.sessionGrant(ss); g == nil || g.provider.provider.LoginID != "second" {
		t.Fatal("probe grant still attributes the failed primary login")
	}
	s.revokeInput(ss)

	// HLS passthrough fetches from the upstream directly. It must hold the
	// selected login itself and open that login's URL, just like the relay.
	input.URL = up.URL + "/live/first/secret/1.m3u8"
	p := &passthroughProducer{s: s, c: c, log: s.logger(), tl: newOwnedTimeline(c.ID, s.logger(), nil, 0, nil)}
	if err = p.open(ctx, input, input.URL, nil, false); err != nil {
		t.Fatal("HLS passthrough failed to open the serving login")
	}
	defer s.releaseLogin(p.login)
	active := p.servingProvider().active
	if active.provider.LoginID != "second" || active.provider.LoginLabel != "se***" || active.provider.LoginSlot != 1 {
		t.Fatal("HLS passthrough did not attribute the selected login")
	}
}

// Provider attribution crosses two JSON APIs and must survive an input
// switch without serializing the credentials used to obtain either lease.
func TestLiveProviderActivityShapeAndMasking(t *testing.T) {
	xtream := &Source{Auth: &SourceAuth{Type: "xtream", Username: "jordan", Password: "xtream-password"},
		Logins: []ProviderLogin{{ID: "other", Username: "ø", Password: "other-password"}}}
	m3u := &Source{PlaylistURL: "https://playlist-user:playlist-password@m3u.example/live/token.m3u?token=query-secret",
		Logins: []ProviderLogin{{ID: "other", PlaylistURL: "https://other.example/list?token=other-secret"}}}

	for _, tc := range []struct {
		src   *Source
		login ProviderLogin
		want  string
	}{
		{xtream, ProviderLogin{ID: "primary"}, "jo***"},
		{xtream, xtream.Logins[0], "***"},
		{xtream, ProviderLogin{ID: "other", Username: "jordy", Label: "Sports"}, "Sports"},
		{m3u, ProviderLogin{ID: "primary"}, "m3u.example"},
		{m3u, ProviderLogin{ID: "other", Label: m3u.Logins[0].PlaylistURL, PlaylistURL: m3u.Logins[0].PlaylistURL}, "other.example"},
		{m3u, ProviderLogin{ID: "other", Label: "other-secret", PlaylistURL: m3u.Logins[0].PlaylistURL}, "other.example"},
		{&Source{Auth: xtream.Auth}, ProviderLogin{ID: "primary"}, ""},
	} {
		if got := providerLoginLabel(tc.src, tc.login); got != tc.want {
			t.Errorf("provider login label = %q, want %q", got, tc.want)
		}
	}

	var tracker providerTracker
	tracker.set(providerForInput(authEntry{name: "Sports playlist"}, StreamSource{ID: "m3u"}, nil))
	lease := &loginLease{source: "xtream", login: "internal-key", accountID: "primary", slot: 1, limit: 3, raw: "https://panel.example/live/jordan/xtream-password/1.ts", label: providerLoginLabel(xtream, ProviderLogin{ID: "primary"})}
	active := providerForInput(authEntry{name: "Sports provider"}, StreamSource{ID: "xtream"}, lease)
	tracker.set(active)
	tracker.set(active) // a reopen using the same source and login preserves the last switch

	usage := map[providerLoginKey]int{active.loginKey: 2}
	var owner, viewer SessionInfo
	tracker.current.Load().attachSession(&owner, true, usage)
	tracker.current.Load().attachSession(&viewer, false, usage)
	if viewer.ActiveSourceID != "xtream" || viewer.Provider != nil || viewer.PreviousProvider != nil {
		t.Fatal("provider details must be limited to the owner")
	}
	for _, value := range []any{
		owner,
		core.PlaySession{LiveChannel: &core.LiveChannel{Provider: owner.Provider, PreviousProvider: owner.PreviousProvider}},
	} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Provider         *core.LiveProvider
			PreviousProvider *core.LiveProviderIdentity
			LiveChannel      *core.LiveChannel
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.LiveChannel != nil {
			payload.Provider, payload.PreviousProvider = payload.LiveChannel.Provider, payload.LiveChannel.PreviousProvider
		}
		p := payload.Provider
		if p == nil || p.SourceName != "Sports provider" || p.LoginID != "primary" || p.LoginLabel != "jo***" ||
			p.LoginSlot != 2 || p.LoginLimit != 3 || p.LocalInUse == nil || *p.LocalInUse != 2 ||
			payload.PreviousProvider == nil || payload.PreviousProvider.SourceName != "Sports playlist" {
			t.Fatal("provider JSON fields did not round-trip")
		}
		for _, secret := range []string{"jordan", "xtream-password", "panel.example", "internal-key"} {
			if bytes.Contains(data, []byte(secret)) {
				t.Fatal("provider JSON contained private input data")
			}
		}
	}
}
