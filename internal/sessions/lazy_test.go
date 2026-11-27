package sessions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/clientnet"
	"github.com/braedonsaunders/ether/internal/store"
)

type fakeLazyGrants struct {
	mu       sync.Mutex
	revoked  bool
	checks   int
	revokeAt int
}

func (g *fakeLazyGrants) GrantReference(_ context.Context, surface, credential string) (string, *core.Viewer, error) {
	if surface != "stremio" || credential != "account-credential-secret" {
		return "", nil, core.ErrUnauthorized
	}
	return "grant-reference", &core.Viewer{ID: 1}, nil
}
func (g *fakeLazyGrants) ValidateGrantReference(_ context.Context, surface, ref string, viewer int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checks++
	if g.revokeAt > 0 && g.checks >= g.revokeAt {
		g.revoked = true
	}
	if g.revoked || surface != "stremio" || ref != "grant-reference" || viewer != 1 {
		return core.ErrUnauthorized
	}
	return nil
}
func TestLazyStreamCapabilityReusesSessionAndRevalidatesGrant(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Now()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	pb := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 3}
	networkLookups := 0
	b, err := NewBroker(BrokerDeps{SurfacePolicy: testSessionPolicy, Events: events, Playback: pb, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(_ *core.Viewer, remote bool, _ time.Time) bool { return !remote }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Now: func() time.Time { return now }, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		networkLookups++
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	mediaHidden := false
	b.deps.MediaLookup = func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		if mediaHidden {
			return nil, core.ErrNotFound
		}
		return &core.Title{Key: k}, nil
	}
	defer b.Close(ctx)
	grants := &fakeLazyGrants{}
	lazy, err := NewLazyStreams(db, b, "stremio", grants, time.Hour, func(r *http.Request) bool { return r.Header.Get("X-Test-Remote") == "yes" })
	if err != nil {
		t.Fatal(err)
	}
	ticketKey := []byte("local playback test key")
	if err := EnableLazyDirectTickets(lazy, ticketKey); err != nil {
		t.Fatal(err)
	}
	req := frontend.PlanRequest{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "install-device"}, Protocol: "stremio", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}}
	path, err := lazy.Issue(ctx, "account-credential-secret", req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(path, "account-credential-secret") || strings.Contains(path, "grant-reference") || pb.prepares != 0 {
		t.Fatalf("issue resolved or exposed grant data: prepares=%d", pb.prepares)
	}
	variantCalls := 0
	b.deps.Variants = func(context.Context, *core.Viewer, *core.Title, core.ClientCaps) ([]core.ReleaseVariant, error) {
		variantCalls++
		return nil, core.ErrNotAvailable
	}
	variantRequest := req
	variantRequest.Version = "chosen-version"
	if _, err := lazy.Issue(ctx, "account-credential-secret", variantRequest); err != nil || variantCalls != 0 {
		t.Fatalf("issue resolved variant: calls=%d err=%v", variantCalls, err)
	}
	head := httptest.NewRecorder()
	lazy.ServeHTTP(head, httptest.NewRequest(http.MethodHead, path, nil))
	if head.Code != http.StatusOK || pb.prepares != 0 {
		t.Fatalf("HEAD status=%d, prepares=%d", head.Code, pb.prepares)
	}
	var issued int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions_lazy_streams`).Scan(&issued); err != nil || issued != 0 {
		t.Fatalf("stream issue or HEAD persisted a capability: count=%d err=%v", issued, err)
	}
	tokenOffset := len("/sessions/lazy/") + len(lazyDirectPrefix)
	damagedByte := 'A'
	if path[tokenOffset] == 'A' {
		damagedByte = 'B'
	}
	damaged := path[:tokenOffset] + string(damagedByte) + path[tokenOffset+1:]
	tampered := httptest.NewRecorder()
	lazy.ServeHTTP(tampered, httptest.NewRequest(http.MethodHead, damaged, nil))
	if tampered.Code != http.StatusNotFound {
		t.Fatalf("tampered stream ticket status=%d", tampered.Code)
	}
	var wg sync.WaitGroup
	statuses := make(chan int, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Range", "bytes=0-2")
			lazy.ServeHTTP(out, r)
			statuses <- out.Code
		}()
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("range status %d", status)
		}
	}
	if pb.prepares != 1 || len(pb.Sessions()) != 1 {
		t.Fatalf("opens %d sessions %d", pb.prepares, len(pb.Sessions()))
	}
	remoteRequest := httptest.NewRequest(http.MethodGet, path, nil)
	remoteRequest.Header.Set("X-Test-Remote", "yes")
	remoteResponse := httptest.NewRecorder()
	lazy.ServeHTTP(remoteResponse, remoteRequest)
	if remoteResponse.Code != http.StatusNotFound {
		t.Fatalf("remote policy bypass: %d", remoteResponse.Code)
	}
	// A fresh surface handler can authenticate the same ticket and reuse the redeemed capability.
	restarted, err := NewLazyStreams(db, b, "stremio", grants, time.Hour, func(r *http.Request) bool { return r.Header.Get("X-Test-Remote") == "yes" })
	if err != nil {
		t.Fatal(err)
	}
	if err := EnableLazyDirectTickets(restarted, ticketKey); err != nil {
		t.Fatal(err)
	}
	reused := httptest.NewRecorder()
	restarted.ServeHTTP(reused, httptest.NewRequest(http.MethodGet, path, nil))
	if reused.Code != http.StatusOK {
		t.Fatalf("durable capability status %d", reused.Code)
	}
	if pb.prepares != 1 || len(pb.Sessions()) != 1 {
		t.Fatalf("handler recreation opened another session: %d", pb.prepares)
	}
	// Range playback remains authorized over three hours without opening a
	// second session; each request slides only the capability, not the grant.
	for n := 0; n < 7; n++ {
		now = now.Add(30 * time.Minute)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Range", "bytes=0-2")
		lazy.ServeHTTP(w, r)
		if w.Code != http.StatusOK || pb.prepares != 1 {
			t.Fatalf("range after %s: status=%d prepares=%d", time.Duration(n+1)*30*time.Minute, w.Code, pb.prepares)
		}
	}
	var serialized string
	if err := db.QueryRow(`SELECT request FROM sessions_lazy_streams LIMIT 1`).Scan(&serialized); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(serialized, "account-credential-secret") || strings.Contains(serialized, "grant-reference") {
		t.Fatal("credential or grant reference in serialized playback request")
	}
	authenticated := req
	authenticated.Device.ID = "webdav-device"
	var firstLookups int
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/dav/movie.mkv", nil)
		r.Header.Set("Range", "bytes=0-2")
		current := authenticated
		if i == 1 {
			current.Device.Client = "renamed client"
			current.Device.DeviceName = "renamed device"
			current.Caps.DeviceName = "renamed device"
		}
		lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(w, r, "account-credential-secret", current)
		if w.Code != http.StatusOK {
			t.Fatalf("authenticated range %d: %d", i, w.Code)
		}
		if i == 0 {
			firstLookups = networkLookups
		} else if networkLookups != firstLookups {
			t.Fatal("reused authenticated range fetched network metadata")
		}
	}
	if pb.prepares != 2 || len(pb.Sessions()) != 2 {
		t.Fatalf("authenticated ranges did not reuse a session: prepares=%d", pb.prepares)
	}
	changed := authenticated
	changed.Caps.MaxHeight = 720
	deniedDecision := httptest.NewRecorder()
	lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(deniedDecision, httptest.NewRequest(http.MethodGet, "/dav/movie.mkv", nil), "account-credential-secret", changed)
	if deniedDecision.Code != http.StatusNotFound || pb.prepares != 2 {
		t.Fatalf("changed decision was reused: status=%d prepares=%d", deniedDecision.Code, pb.prepares)
	}
	var concurrent sync.WaitGroup
	statuses = make(chan int, 4)
	for n := 0; n < 4; n++ {
		concurrent.Add(1)
		go func() {
			defer concurrent.Done()
			w := httptest.NewRecorder()
			lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(w, httptest.NewRequest(http.MethodGet, "/dav/movie.mkv", nil), "account-credential-secret", authenticated)
			statuses <- w.Code
		}()
	}
	concurrent.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("concurrent authenticated range: %d", status)
		}
	}
	if pb.prepares != 2 || len(pb.Sessions()) != 2 {
		t.Fatalf("concurrent ranges opened another session: %d", pb.prepares)
	}
	foreign := authenticated
	foreign.Viewer = &core.Viewer{ID: 2}
	for _, tc := range []struct {
		credential string
		request    frontend.PlanRequest
	}{{"invalid", authenticated}, {"account-credential-secret", foreign}} {
		w := httptest.NewRecorder()
		lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(w, httptest.NewRequest(http.MethodGet, "/dav/movie.mkv", nil), tc.credential, tc.request)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("foreign credential/viewer accepted: %d", w.Code)
		}
	}
	grants.mu.Lock()
	grants.revoked = true
	grants.mu.Unlock()
	revokedRange := httptest.NewRecorder()
	lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(revokedRange, httptest.NewRequest(http.MethodGet, "/dav/movie.mkv", nil), "account-credential-secret", authenticated)
	if revokedRange.Code != http.StatusNotFound || len(pb.Sessions()) != 2 {
		t.Fatalf("revoked range status=%d sessions=%d", revokedRange.Code, len(pb.Sessions()))
	}
	grants.mu.Lock()
	grants.revoked = false
	grants.mu.Unlock()
	unstartedReq := req
	unstartedReq.Device.ID = "not-yet-playing"
	unstartedPath, err := lazy.Issue(ctx, "account-credential-secret", unstartedReq)
	if err != nil {
		t.Fatal(err)
	}
	originalPlanLookup := b.deps.Lookup
	originalMediaLookup := b.deps.MediaLookup
	b.deps.CurrentMediaLookup = originalMediaLookup
	deleted := func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Title, error) {
		return nil, core.ErrNotFound
	}
	b.deps.Lookup, b.deps.MediaLookup = deleted, deleted
	if _, err := lazy.Issue(ctx, "account-credential-secret", req); err == nil {
		t.Fatal("deleted membership issued lazy capability")
	}
	replay := httptest.NewRecorder()
	lazy.ServeHTTP(replay, httptest.NewRequest(http.MethodGet, unstartedPath, nil))
	if replay.Code != http.StatusNotFound {
		t.Fatalf("unstarted capability replay admitted: %d", replay.Code)
	}
	running := httptest.NewRecorder()
	lazy.ServeHTTP(running, httptest.NewRequest(http.MethodGet, path, nil))
	if running.Code != http.StatusOK {
		t.Fatalf("running lazy stream interrupted: %d", running.Code)
	}
	runningAuth := httptest.NewRecorder()
	lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(runningAuth, httptest.NewRequest(http.MethodGet, "/dav/movie.mkv", nil), "account-credential-secret", authenticated)
	if runningAuth.Code != http.StatusOK {
		t.Fatalf("running authenticated stream interrupted: %d", runningAuth.Code)
	}
	b.deps.Lookup, b.deps.MediaLookup = originalPlanLookup, originalMediaLookup
	b.deps.CurrentMediaLookup = nil
	mediaHidden = true
	hiddenRange := httptest.NewRecorder()
	lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(hiddenRange, httptest.NewRequest(http.MethodGet, "/dav/movie.mkv", nil), "account-credential-secret", authenticated)
	if hiddenRange.Code != http.StatusNotFound || len(pb.Sessions()) != 2 {
		t.Fatalf("hidden item status=%d sessions=%d", hiddenRange.Code, len(pb.Sessions()))
	}
	mediaHidden = false
	headAuth := httptest.NewRecorder()
	lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(headAuth, httptest.NewRequest(http.MethodHead, "/dav/movie.mkv", nil), "account-credential-secret", authenticated)
	if headAuth.Code != http.StatusOK || headAuth.Body.Len() != 0 || pb.prepares != 2 {
		t.Fatalf("authenticated HEAD status=%d body=%d prepares=%d", headAuth.Code, headAuth.Body.Len(), pb.prepares)
	}
	other := req
	other.Device.ID = "other-install"
	secondPath, err := lazy.Issue(ctx, "account-credential-secret", other)
	if err != nil {
		t.Fatal(err)
	}
	grants.mu.Lock()
	grants.checks = 0
	grants.revokeAt = 3
	grants.mu.Unlock()
	midOpen := httptest.NewRecorder()
	lazy.ServeHTTP(midOpen, httptest.NewRequest(http.MethodGet, secondPath, nil))
	if midOpen.Code != http.StatusNotFound || len(pb.Sessions()) != 2 {
		t.Fatalf("revocation during open: status=%d sessions=%d", midOpen.Code, len(pb.Sessions()))
	}
	grants.mu.Lock()
	grants.revoked = true
	grants.mu.Unlock()
	denied := httptest.NewRecorder()
	lazy.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, path, nil))
	if denied.Code != http.StatusNotFound {
		t.Fatalf("revoked status %d", denied.Code)
	}
	grants.mu.Lock()
	grants.revoked = false
	grants.revokeAt = 0
	grants.mu.Unlock()
	now = now.Add(30 * time.Minute)
	idleHead := httptest.NewRecorder()
	lazy.ServeHTTP(idleHead, httptest.NewRequest(http.MethodHead, path, nil))
	if idleHead.Code != http.StatusOK {
		t.Fatalf("idle HEAD status %d", idleHead.Code)
	}
	now = now.Add(30 * time.Minute)
	expired := httptest.NewRecorder()
	lazy.ServeHTTP(expired, httptest.NewRequest(http.MethodGet, path, nil))
	if expired.Code != http.StatusNotFound {
		t.Fatalf("expired status %d", expired.Code)
	}
}

func TestAuthenticatedLazyConcurrentFirstRangesReuseSession(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	pb := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key}, nil
	}
	broker, err := NewBroker(BrokerDeps{SurfacePolicy: testSessionPolicy, Events: events, Playback: pb, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: lookup, MediaLookup: lookup})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	lazy, err := NewLazyStreams(db, broker, "stremio", &fakeLazyGrants{}, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	request := frontend.PlanRequest{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "webdav-device"}, Protocol: "stremio", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}}
	var wg sync.WaitGroup
	statuses := make(chan int, 8)
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(w, httptest.NewRequest(http.MethodGet, "/movie", nil), "account-credential-secret", request)
			statuses <- w.Code
		}()
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("concurrent status: %d", status)
		}
	}
	if pb.prepares != 1 || len(pb.Sessions()) != 1 {
		t.Fatalf("prepared=%d sessions=%d", pb.prepares, len(pb.Sessions()))
	}
	seedLazyUpgrade(t, db, maxLazyViewerCapabilities)
	if _, err := lazy.(*lazyStreams).issue(ctx, "account-credential-secret", request, true); err == nil {
		t.Fatal("oversized browse stable issuance accepted")
	}
	w := httptest.NewRecorder()
	lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(w, httptest.NewRequest(http.MethodGet, "/movie", nil), "account-credential-secret", request)
	if w.Code != http.StatusOK || pb.prepares != 1 {
		t.Fatalf("active oversized range status=%d prepares=%d", w.Code, pb.prepares)
	}
	different := request
	different.Device.ID = "new-device"
	w = httptest.NewRecorder()
	lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(w, httptest.NewRequest(http.MethodGet, "/movie", nil), "account-credential-secret", different)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("new oversized range admitted: %d", w.Code)
	}

}

func TestAuthenticatedLazyConflictAndExpiry(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Now()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	pb := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key}, nil
	}
	broker, err := NewBroker(BrokerDeps{SurfacePolicy: testSessionPolicy, Events: events, Playback: pb, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: lookup, MediaLookup: lookup, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	iface, err := NewLazyStreams(db, broker, "stremio", &fakeLazyGrants{}, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	lazy := iface.(*lazyStreams)
	first := frontend.PlanRequest{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "same-device"}, Protocol: "stremio", Item: core.MovieKey(1)}
	second := first
	second.Caps.MaxHeight = 720
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, request := range []frontend.PlanRequest{first, second} {
		go func(r frontend.PlanRequest) {
			<-start
			_, err := lazy.issue(ctx, "account-credential-secret", r, true)
			results <- err
		}(request)
	}
	close(start)
	firstErr, secondErr := <-results, <-results
	if (firstErr == nil) == (secondErr == nil) {
		t.Fatalf("conflicting decisions: %v, %v", firstErr, secondErr)
	}
	if firstErr != nil && !errors.Is(firstErr, core.ErrNotAvailable) || secondErr != nil && !errors.Is(secondErr, core.ErrNotAvailable) {
		t.Fatalf("conflict failed unexpectedly: %v, %v", firstErr, secondErr)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions_lazy_streams`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows=%d err=%v", count, err)
	}
	now = now.Add(time.Hour)
	if _, err := lazy.issue(ctx, "account-credential-secret", second, true); err != nil {
		t.Fatalf("replacement after expiry: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions_lazy_streams`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expiry cleanup rows=%d err=%v", count, err)
	}
	if pb.prepares != 0 {
		t.Fatal("issuing or expiring capabilities opened playback")
	}
}

type expiringLazyGrants struct {
	*fakeLazyGrants
	expires time.Time
}

func (g *expiringLazyGrants) GrantReferenceExpiry(ctx context.Context, surface, ref string, viewer int64) (time.Time, error) {
	if err := g.ValidateGrantReference(ctx, surface, ref, viewer); err != nil {
		return time.Time{}, err
	}
	return g.expires, nil
}

func TestLazyRenewalRespectsGrantAndDatabase(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Now().Truncate(time.Millisecond)
	grant := &expiringLazyGrants{fakeLazyGrants: &fakeLazyGrants{}, expires: now.Add(20 * time.Minute)}
	broker := &Broker{deps: BrokerDeps{SurfacePolicy: testSessionPolicy, Now: func() time.Time { return now }}, lazySessions: map[string]lazySession{lazyHash("hash"): {id: "session", expires: now.Add(time.Minute).UnixMilli()}}}
	streams := &lazyStreams{db: db, broker: broker, protocol: "stremio", grants: grant, ttl: time.Hour}
	if _, err := db.Exec(`INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires) VALUES('lazy-stream',?,1,1,'stremio','',?)`, lazyHash("hash"), now.Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions_lazy_streams(hash,protocol,viewer_id,grant_reference,request,expires) VALUES(?,?,?,?,?,?)`, lazyHash("hash"), "stremio", 1, "grant-reference", "{}", now.Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := streams.renewActive(ctx, lazyHash("hash"), "grant-reference", 1, "session"); err != nil {
		t.Fatal(err)
	}
	var expires int64
	if err := db.QueryRow(`SELECT expires FROM sessions_lazy_streams WHERE hash=?`, lazyHash("hash")).Scan(&expires); err != nil || expires != grant.expires.UnixMilli() {
		t.Fatalf("expiry=%d grant=%d err=%v", expires, grant.expires.UnixMilli(), err)
	}
	grant.revoked = true
	if err := streams.renewActive(ctx, lazyHash("hash"), "grant-reference", 1, "session"); err == nil {
		t.Fatal("renewed revoked grant")
	}
	grant.revoked = false
	now = grant.expires
	if err := streams.renewActive(ctx, lazyHash("hash"), "grant-reference", 1, "session"); err == nil {
		t.Fatal("renewed expired capability or grant")
	}
	now = now.Add(-time.Minute)
	if _, err := db.Exec(`DROP TABLE sessions_lazy_streams`); err != nil {
		t.Fatal(err)
	}
	if err := streams.renewActive(ctx, lazyHash("hash"), "grant-reference", 1, "session"); err == nil {
		t.Fatal("renewed during database failure")
	}
}

func TestLazyStreamLocalityFollowsLiveSourceIP(t *testing.T) {
	restore := clientnet.OverrideHostPrefixes([]netip.Prefix{
		netip.MustParsePrefix("2001:db8:abcd:1::/64"),
	})
	t.Cleanup(restore)

	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Now().Truncate(time.Second)
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		return &core.Viewer{ID: id, Username: "sam"}, nil
	}
	events := NewEventBus(nil, active)
	defer events.Close()
	pb := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 3}
	b, err := NewBroker(BrokerDeps{
		SurfacePolicy: testSessionPolicy,
		Events:        events,
		Playback:      pb,
		Transcoder:    fakeBrokerTranscoder{},
		Progress:      &fakeProgress{},
		ActiveViewer:  active,
		CanPlay:       func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit:  func(*core.Viewer) int { return 0 },
		OnTimeline:    func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Lookup: func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: core.MovieKey(1)}, nil
		},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	b.deps.MediaLookup = func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: core.MovieKey(1)}, nil
	}
	defer b.Close(ctx)

	trusted := clientnet.ProxyPrefixes([]string{"172.16.50.5"})
	resolveIP := func(r *http.Request) (netip.Addr, bool) {
		ip, _ := clientnet.ClientNet(r, trusted...)
		return ip, ip.IsValid()
	}
	grants := &fakeLazyGrants{}
	lazy, err := NewLazyStreams(db, b, "stremio", grants, time.Hour, func(r *http.Request) bool {
		ip, ok := resolveIP(r)
		return !ok || !clientnet.LAN(ip)
	})
	if err != nil {
		t.Fatal(err)
	}
	lazy.(*lazyStreams).SetClientIP(resolveIP)

	// Issue a stream URL while the client appears remote.
	req := frontend.PlanRequest{
		Viewer:   &core.Viewer{ID: 1},
		Device:   core.Device{ID: "stremio-dev"},
		Caps:     core.ClientCaps{Remote: true, ClientIP: "203.0.113.10"},
		Protocol: "stremio",
		Item:     core.MovieKey(1),
		Intent:   frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}},
	}
	path, err := lazy.Issue(ctx, "account-credential-secret", req)
	if err != nil {
		t.Fatal(err)
	}

	// Redeem from a global IPv6 LAN client inside host /64 behind trusted proxy -> local.
	rLocal := httptest.NewRequest(http.MethodGet, path, nil)
	rLocal.RemoteAddr = "172.16.50.5:41000"
	rLocal.Header.Set("X-Forwarded-For", "2001:db8:abcd:1::42")
	wLocal := httptest.NewRecorder()
	lazy.ServeHTTP(wLocal, rLocal)
	if wLocal.Code != http.StatusOK {
		t.Fatalf("local IPv6 status = %d", wLocal.Code)
	}
	if len(pb.Sessions()) != 1 || pb.Sessions()[0].Client.Remote || pb.Sessions()[0].Client.ClientIP != "2001:db8:abcd:1::42" {
		t.Fatalf("local redeem kept remote or stale IP: %+v", pb.Sessions())
	}

	// Redeem the same capability from a public IP -> restarts as remote with public ClientIP.
	rRemote := httptest.NewRequest(http.MethodGet, path, nil)
	rRemote.RemoteAddr = "172.16.50.5:41001"
	rRemote.Header.Set("X-Forwarded-For", "203.0.113.88")
	wRemote := httptest.NewRecorder()
	lazy.ServeHTTP(wRemote, rRemote)
	if wRemote.Code != http.StatusOK {
		t.Fatalf("remote status = %d", wRemote.Code)
	}
	if len(pb.Sessions()) != 1 || !pb.Sessions()[0].Client.Remote || pb.Sessions()[0].Client.ClientIP != "203.0.113.88" {
		t.Fatalf("remote redeem kept local or stale IP: %+v", pb.Sessions())
	}
}

func TestLazyWritePlaybackErrorRequiredAudioLanguage(t *testing.T) {
	langErr := &core.RequiredAudioLanguageError{Language: "English"}
	ls := &lazyStreams{protocol: "stremio"}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/sessions/lazy/test", nil)
	ls.writePlaybackError(w, r, fmt.Errorf("plan: %w", langErr))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), langErr.Error()) {
		t.Fatalf("writePlaybackError(RequiredAudioLanguageError) = %d %q, want 422 with %q", w.Code, w.Body.String(), langErr.Error())
	}
}
