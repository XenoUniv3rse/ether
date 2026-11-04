package plexwatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

// fakePMS is an in-memory Plex Media Server: accounts with tokens, items
// with Guid arrays, per-account watch state, and a view history that
// /:/scrobble appends to, as a real server's does.
type fakePMS struct {
	mu       sync.Mutex
	machine  string
	clock    func() time.Time
	tokens   map[string]int64 // token → account id (1 is the owner)
	names    map[int64]string
	items    map[int64]pmsMetadata
	viewed   map[[2]int64]*pmsMetadata // (account, item) → state
	history  []pmsMetadata
	nextHist int64
	writes   []string
	requests []string
	limitFor int // answer this many history requests with 429
}

func newFakePMS(machine string, clock func() time.Time) *fakePMS {
	return &fakePMS{machine: machine, clock: clock,
		tokens: map[string]int64{"owner-" + machine: 1}, names: map[int64]string{1: "owner", 2: "kid"},
		items: map[int64]pmsMetadata{}, viewed: map[[2]int64]*pmsMetadata{}}
}

func (f *fakePMS) addItem(md pmsMetadata) {
	f.mu.Lock()
	f.items[int64(md.RatingKey)] = md
	f.mu.Unlock()
}

// view records a real viewing on the server at the given time.
func (f *fakePMS) view(account, key int64, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.viewLocked(account, key, at)
}

func (f *fakePMS) viewLocked(account, key int64, at time.Time) {
	st := f.stateLocked(account, key)
	st.ViewCount++
	st.ViewOffset = 0
	st.LastViewedAt = flexInt(at.Unix())
	it := f.items[key]
	f.nextHist++
	f.history = append(f.history, pmsMetadata{RatingKey: flexInt(key), Type: it.Type, Title: it.Title,
		ViewedAt: flexInt(at.Unix()), AccountID: flexInt(account), Index: flexInt(f.nextHist)})
}

func (f *fakePMS) stateLocked(account, key int64) *pmsMetadata {
	k := [2]int64{account, key}
	if f.viewed[k] == nil {
		f.viewed[k] = &pmsMetadata{}
	}
	return f.viewed[k]
}

func (f *fakePMS) watched(account, key int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stateLocked(account, key).ViewCount > 0
}

func (f *fakePMS) takeWrites() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.writes
	f.writes = nil
	return w
}

func (f *fakePMS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.Path)
	if r.URL.Query().Get("X-Plex-Token") != "" {
		http.Error(w, "token in URL", http.StatusBadRequest)
		return
	}
	account, ok := f.tokens[r.Header.Get("X-Plex-Token")]
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	out := map[string]any{}
	mc := map[string]any{}
	key, _ := strconv.ParseInt(q.Get("key"), 10, 64)
	switch {
	case r.URL.Path == "/identity":
		mc["machineIdentifier"], mc["version"] = f.machine, "1.43.4.10903"
	case r.URL.Path == "/myplex/account":
		if account != 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		out["MyPlex"] = map[string]any{"username": "owner", "signInState": "ok"}
	case r.URL.Path == "/accounts":
		var list []map[string]any
		for id, name := range f.names {
			list = append(list, map[string]any{"id": id, "name": name, "key": "/accounts/" + strconv.FormatInt(id, 10)})
		}
		mc["Account"] = list
	case r.URL.Path == "/library/sections":
		mc["Directory"] = []map[string]any{{"key": "1", "type": "movie"}, {"key": "2", "type": "show"}}
	case strings.HasPrefix(r.URL.Path, "/library/sections/") && strings.HasSuffix(r.URL.Path, "/all"):
		var hits []pmsMetadata
		for _, it := range f.items {
			if it.GUID == q.Get("guid") && ((q.Get("type") == "1" && it.Type == "movie") || (q.Get("type") == "4" && it.Type == "episode")) {
				hits = append(hits, it)
			}
		}
		mc["Metadata"] = hits
	case strings.HasPrefix(r.URL.Path, "/library/metadata/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/library/metadata/"), 10, 64)
		it, ok := f.items[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		st := f.stateLocked(account, id)
		it.ViewCount, it.ViewOffset, it.LastViewedAt = st.ViewCount, st.ViewOffset, st.LastViewedAt
		mc["Metadata"] = []pmsMetadata{it}
	case r.URL.Path == "/status/sessions/history/all":
		if f.limitFor > 0 {
			f.limitFor--
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		after, _ := strconv.ParseInt(q.Get("viewedAt>"), 10, 64)
		acct, _ := strconv.ParseInt(q.Get("accountID"), 10, 64)
		var rows []pmsMetadata
		for _, h := range f.history {
			if int64(h.ViewedAt) > after && int64(h.AccountID) == acct {
				rows = append(rows, h)
			}
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].ViewedAt < rows[j].ViewedAt })
		start, _ := strconv.Atoi(r.Header.Get("X-Plex-Container-Start"))
		size, _ := strconv.Atoi(r.Header.Get("X-Plex-Container-Size"))
		if start > len(rows) {
			start = len(rows)
		}
		end := min(len(rows), start+size)
		mc["Metadata"] = rows[start:end]
	case r.URL.Path == "/:/scrobble" || r.URL.Path == "/:/unscrobble" || r.URL.Path == "/:/progress":
		if q.Get("identifier") != pmsIdentifier || f.items[key].Type == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.writes = append(f.writes, strings.TrimPrefix(r.URL.Path, "/:/")+" "+strconv.FormatInt(account, 10)+" "+q.Get("key"))
		switch r.URL.Path {
		case "/:/scrobble":
			f.viewLocked(account, key, f.clock())
		case "/:/unscrobble":
			st := f.stateLocked(account, key)
			st.ViewCount, st.ViewOffset = 0, 0
		default:
			ms, _ := strconv.ParseInt(q.Get("time"), 10, 64)
			f.stateLocked(account, key).ViewOffset = flexInt(ms)
		}
		w.WriteHeader(http.StatusOK)
		return
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if len(mc) > 0 {
		out["MediaContainer"] = mc
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

type fakeCatalog struct {
	titles   map[core.RatingKey]core.Title
	external map[string]core.RatingKey // "source:id:type"
}

func (c *fakeCatalog) Get(_ context.Context, k core.RatingKey) (*core.Title, error) {
	t, ok := c.titles[k]
	if !ok {
		return nil, core.ErrNotFound
	}
	return &t, nil
}

func (c *fakeCatalog) ByExternalIDForType(_ context.Context, source, id string, typ core.MediaType) (*core.Title, error) {
	k, ok := c.external[source+":"+id+":"+string(typ)]
	if !ok {
		return nil, core.ErrNotFound
	}
	return c.Get(context.Background(), k)
}

// prefixSealer is a reversible stand-in for the config sealer.
type prefixSealer struct{}

func (prefixSealer) SealField(plain string, _ ...string) (string, error) {
	return "sealed:" + strings.Map(func(r rune) rune { return r + 1 }, plain), nil
}

func (prefixSealer) OpenField(stored string, _ ...string) (string, bool, error) {
	if !strings.HasPrefix(stored, "sealed:") {
		return "", false, errors.New("not sealed")
	}
	return strings.Map(func(r rune) rune { return r - 1 }, strings.TrimPrefix(stored, "sealed:")), false, nil
}

type pmsHarness struct {
	t       *testing.T
	ctx     context.Context
	clock   *lockedTime
	st      *state.State
	svc     *Servers
	guid    *memGUID
	catalog *fakeCatalog
}

const pmsViewer = 7

var (
	movieA   = core.MovieKey(11974)
	movieB   = core.MovieKey(603)
	movieC   = core.MovieKey(550)
	showS    = core.ShowKey(1399)
	episodeE = core.EpisodeKey(1399, 1, 2)
)

func newPMSHarness(t *testing.T) *pmsHarness {
	t.Helper()
	db := store.OpenTest(t)
	clock := &lockedTime{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	st := state.New(db, state.Options{Now: clock.Now})
	guid := &memGUID{forward: map[core.RatingKey]string{
		movieA: "plex://movie/a", movieB: "plex://movie/b", movieC: "plex://movie/c", episodeE: "plex://episode/e",
	}, reverse: map[string]core.RatingKey{}}
	cat := &fakeCatalog{titles: map[core.RatingKey]core.Title{
		movieA: {Key: movieA, Type: core.TypeMovie, Title: "The 'Burbs"},
		movieB: {Key: movieB, Type: core.TypeMovie, Title: "The Matrix"},
		movieC: {Key: movieC, Type: core.TypeMovie, Title: "Fight Club"},
		showS:  {Key: showS, Type: core.TypeShow, Title: "Thrones"},
	}, external: map[string]core.RatingKey{
		"imdb:tt0133093:movie": movieB,
		"tvdb:121361:show":     showS,
	}}
	svc, err := NewServers(ServersOptions{DB: db, State: st, Sealer: prefixSealer{}, GUID: guid, Catalog: cat, Now: clock.Now, HTTP: &http.Client{Timeout: 5 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	return &pmsHarness{t: t, ctx: context.Background(), clock: clock, st: st, svc: svc, guid: guid, catalog: cat}
}

// server starts a fake PMS whose items carry the given keys, adds it, and
// maps the owner account to the viewer with direction.
func (h *pmsHarness) server(machine string, keyBase int64, direction int) (*fakePMS, PMSServer) {
	h.t.Helper()
	f := newFakePMS(machine, h.clock.Now)
	g := func(ids ...string) []struct {
		ID string `json:"id"`
	} {
		out := make([]struct {
			ID string `json:"id"`
		}, len(ids))
		for i, id := range ids {
			out[i].ID = id
		}
		return out
	}
	f.addItem(pmsMetadata{RatingKey: flexInt(keyBase + 1), Type: "movie", Title: "The 'Burbs", GUID: "plex://movie/a", GUIDs: g("imdb://tt0096734", "tmdb://11974")})
	f.addItem(pmsMetadata{RatingKey: flexInt(keyBase + 2), Type: "movie", Title: "The Matrix", GUID: "plex://movie/b", GUIDs: g("imdb://tt0133093")})
	f.addItem(pmsMetadata{RatingKey: flexInt(keyBase + 3), Type: "movie", Title: "Home Video", GUID: "local://3"})
	f.addItem(pmsMetadata{RatingKey: flexInt(keyBase + 10), Type: "show", Title: "Thrones", GUID: "plex://show/s", GUIDs: g("tvdb://121361")})
	f.addItem(pmsMetadata{RatingKey: flexInt(keyBase + 11), Type: "episode", Title: "The Kingsroad", GUID: "plex://episode/e",
		GUIDs: g("tmdb://63057", "tvdb://3436411"), GrandparentRatingKey: flexInt(keyBase + 10), ParentIndex: 1, Index: 2})
	srv := httptest.NewServer(f)
	h.t.Cleanup(srv.Close)
	added, err := h.svc.Add(h.ctx, machine, srv.URL, "owner-"+machine)
	if err != nil {
		h.t.Fatal(err)
	}
	if direction != 0 {
		if _, err := h.svc.MapAccount(h.ctx, added.ID, 1, PMSMappingInput{UserID: pmsViewer, Direction: direction, Interval: 5}); err != nil {
			h.t.Fatal(err)
		}
	}
	return f, added
}

// pass advances past every schedule and runs all due mappings once.
func (h *pmsHarness) pass() {
	h.clock.Add(10 * time.Minute)
	h.svc.RunOnce(h.ctx)
}

func (h *pmsHarness) localWatched(key core.RatingKey) bool {
	ok, _, err := h.svc.localWatch(h.ctx, pmsViewer, key)
	if err != nil {
		h.t.Fatal(err)
	}
	return ok
}

// TestPMSSyncNeverBounces is the echo invariant across two servers and both
// directions: a change reaches every other side exactly once and nothing
// ever comes back to where it started, however many passes run.
func TestPMSSyncNeverBounces(t *testing.T) {
	h := newPMSHarness(t)
	a, _ := h.server("server-a", 100, 3)
	b, _ := h.server("server-b", 500, 3)
	quiet := func(step string) {
		t.Helper()
		for i := 0; i < 4; i++ {
			h.pass()
		}
		if w := append(a.takeWrites(), b.takeWrites()...); len(w) != 0 {
			t.Fatalf("%s: writes kept flowing: %v", step, w)
		}
	}

	// Watched on A: Ether imports it, B gets one scrobble, A gets none.
	h.clock.Add(time.Minute)
	a.view(1, 101, h.clock.Now())
	h.pass() // A pulls; B is due too but runs on its own schedule
	h.pass()
	if !h.localWatched(movieA) {
		t.Fatal("view on A was not imported")
	}
	if w := a.takeWrites(); len(w) != 0 {
		t.Fatalf("import echoed back to its own server: %v", w)
	}
	if w := b.takeWrites(); len(w) != 1 || w[0] != "scrobble 1 501" {
		t.Fatalf("server B writes = %v, want one scrobble", w)
	}
	quiet("after A→Ether→B")

	// Unwatched in Ether: one unscrobble each, then silence (an unwatch
	// leaves no history to re-import).
	if err := h.st.Unscrobble(h.ctx, pmsViewer, movieA); err != nil {
		t.Fatal(err)
	}
	h.pass()
	h.pass()
	if got := append(a.takeWrites(), b.takeWrites()...); len(got) != 2 || got[0] != "unscrobble 1 101" || got[1] != "unscrobble 1 501" {
		t.Fatalf("unwatch writes = %v", got)
	}
	if a.watched(1, 101) || b.watched(1, 501) {
		t.Fatal("unwatch did not reach both servers")
	}
	quiet("after Ether unwatch")

	// Watched in Ether: one scrobble each. Both servers log that scrobble in
	// their history; it must not come back as a new view.
	if err := h.st.Scrobble(h.ctx, pmsViewer, movieB); err != nil {
		t.Fatal(err)
	}
	_, before, _ := h.svc.localWatch(h.ctx, pmsViewer, movieB)
	h.pass()
	h.pass()
	if got := append(a.takeWrites(), b.takeWrites()...); len(got) != 2 || got[0] != "scrobble 1 102" || got[1] != "scrobble 1 502" {
		t.Fatalf("Ether watch writes = %v", got)
	}
	quiet("after Ether watch")
	if _, after, _ := h.svc.localWatch(h.ctx, pmsViewer, movieB); after != before {
		t.Fatal("a server's echo of Ether's own scrobble was imported")
	}

	// An episode watched on B lands on A by show identity and numbering.
	b.view(1, 511, h.clock.Now())
	h.pass()
	h.pass()
	if !h.localWatched(episodeE) {
		t.Fatal("episode view on B was not imported")
	}
	if w := a.takeWrites(); len(w) != 1 || w[0] != "scrobble 1 111" {
		t.Fatalf("server A writes = %v, want the episode once", w)
	}
	quiet("after B→Ether→A episode")
}

// TestPMSHistoryCursorAndMatching covers incremental paging (including many
// views in one second across page boundaries), id-only matching, unmatched
// storage, other accounts' rows and newer-wins on import.
func TestPMSHistoryCursorAndMatching(t *testing.T) {
	h := newPMSHarness(t)
	a, srv := h.server("server-a", 100, 2)
	at := h.clock.Now().Add(time.Minute)
	// 260 views stamped with one second (a bulk mark), then later ones.
	for i := 0; i < 260; i++ {
		a.view(1, 101, at)
	}
	a.view(2, 102, at)                    // another account: not this viewer's
	a.view(1, 103, at.Add(time.Second))   // no identifiers: unmatched
	a.view(1, 102, at.Add(2*time.Second)) // IMDb only
	a.view(1, 111, at.Add(3*time.Second)) // episode via show TVDB + numbering
	h.clock.Add(2 * time.Minute)
	h.svc.RunOnce(h.ctx) // 5 pages × 100: the bulk second spans pages
	h.pass()
	for _, k := range []core.RatingKey{movieA, movieB, episodeE} {
		if !h.localWatched(k) {
			t.Fatalf("%v not imported", k)
		}
	}
	var cursor, skip int64
	if err := h.svc.db.QueryRow(`SELECT in_cursor,in_skip FROM plexwatch_pms_viewer WHERE server_id=?`, srv.ID).Scan(&cursor, &skip); err != nil {
		t.Fatal(err)
	}
	if cursor != at.Add(3*time.Second).Unix() || skip != 1 {
		t.Fatalf("cursor = %d/%d", cursor, skip)
	}
	un, err := h.svc.Unmatched(h.ctx, srv.ID, pmsViewer, "")
	if err != nil || len(un) != 1 || un[0].Ref != "103" || un[0].Reason != "no_match" || un[0].Title != "Home Video" {
		t.Fatalf("unmatched = %+v, %v", un, err)
	}
	// A late view in the cursor's own second is still read once.
	a.view(1, 101, at.Add(3*time.Second))
	before := len(a.requests)
	h.pass()
	if n := len(a.requests) - before; n != 1 {
		t.Fatalf("a pass made %d requests, want one history page", n)
	}
	if err := h.svc.db.QueryRow(`SELECT in_skip FROM plexwatch_pms_viewer WHERE server_id=?`, srv.ID).Scan(&skip); err != nil || skip != 2 {
		t.Fatalf("skip = %d, %v", skip, err)
	}

	// Newer wins: unwatched in Ether after a PMS view, an older view does
	// not re-mark it.
	h.clock.Add(time.Minute)
	if err := h.st.Unscrobble(h.ctx, pmsViewer, movieB); err != nil {
		t.Fatal(err)
	}
	a.view(1, 102, at.Add(10*time.Second)) // after the cursor, before the unwatch
	h.pass()
	if h.localWatched(movieB) {
		t.Fatal("an older PMS view overrode a newer Ether unwatch")
	}
}

// TestPMSRateLimitAndNewerServer covers Retry-After for every mapping of a
// server, unmatched sends, and not unscrobbling a server that watched later.
func TestPMSRateLimitAndNewerServer(t *testing.T) {
	h := newPMSHarness(t)
	a, srv := h.server("server-a", 100, 3)
	if _, err := h.svc.MapAccount(h.ctx, srv.ID, 2, PMSMappingInput{UserID: 8, Direction: pmsSend, Interval: 5}); !errors.Is(err, ErrPMSNeedsToken) {
		t.Fatalf("send for another account without its token: %v", err)
	}
	if _, err := h.svc.MapAccount(h.ctx, srv.ID, 2, PMSMappingInput{UserID: 8, Direction: pmsReceive, Interval: 5}); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.limitFor = 1
	a.mu.Unlock()
	h.pass()
	var allowed []int64
	rows, err := h.svc.db.Query(`SELECT next_allowed FROM plexwatch_pms_viewer WHERE server_id=?`, srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n int64
		_ = rows.Scan(&n)
		allowed = append(allowed, n)
	}
	rows.Close()
	limit := h.clock.Now().Add(2 * time.Minute).UnixNano()
	if len(allowed) != 2 || allowed[0] < limit || allowed[1] < limit {
		t.Fatalf("Retry-After not applied to every mapping: %v (want ≥ %d)", allowed, limit)
	}
	before := len(a.requests)
	h.clock.Add(time.Minute)
	h.svc.RunOnce(h.ctx)
	if len(a.requests) != before {
		t.Fatal("contacted a server inside its Retry-After")
	}
	got, _ := h.svc.Server(h.ctx, srv.ID)
	if got.Viewers[0].Problem != "rate_limited" && got.Viewers[1].Problem != "rate_limited" {
		t.Fatalf("problem not reported: %+v", got.Viewers)
	}

	// A title with no Plex identity is recorded, not guessed.
	h.guid.mu.Lock()
	delete(h.guid.forward, movieC)
	h.guid.mu.Unlock()
	if err := h.st.Scrobble(h.ctx, pmsViewer, movieC); err != nil {
		t.Fatal(err)
	}
	// Ether watched, the server later watched again, then Ether unwatched
	// before syncing: the server's later view wins, nothing is unscrobbled.
	if err := h.st.Scrobble(h.ctx, pmsViewer, movieA); err != nil {
		t.Fatal(err)
	}
	h.pass()
	h.pass()
	if w := a.takeWrites(); len(w) != 1 || w[0] != "scrobble 1 101" {
		t.Fatalf("writes = %v", w)
	}
	un, _ := h.svc.Unmatched(h.ctx, srv.ID, pmsViewer, "")
	if len(un) != 1 || un[0].Direction != "out" || un[0].Reason != "no_plex_id" || un[0].Title != "Fight Club" {
		t.Fatalf("unmatched = %+v", un)
	}
	if err := h.st.Unscrobble(h.ctx, pmsViewer, movieA); err != nil {
		t.Fatal(err)
	}
	h.clock.Add(time.Minute)
	a.mu.Lock()
	a.stateLocked(1, 101).LastViewedAt = flexInt(h.clock.Now().Add(time.Hour).Unix())
	a.mu.Unlock()
	h.pass()
	if w := a.takeWrites(); len(w) != 0 {
		t.Fatalf("unscrobbled a server whose view is newer: %v", w)
	}
}

// TestPMSServerValidation: tokens are sealed and checked against the server.
func TestPMSServerValidation(t *testing.T) {
	h := newPMSHarness(t)
	_, srv := h.server("server-a", 100, 0)
	var stored string
	if err := h.svc.db.QueryRow(`SELECT token FROM plexwatch_pms WHERE id=?`, srv.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "owner-server-a") || !strings.HasPrefix(stored, "sealed:") {
		t.Fatalf("token stored as %q", stored)
	}
	if _, err := h.svc.Add(h.ctx, "dup", srv.URL, "owner-server-a"); !errors.Is(err, ErrPMSDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := h.svc.Add(h.ctx, "bad", srv.URL, "wrong"); !errors.Is(err, ErrPMSToken) {
		t.Fatalf("bad token: %v", err)
	}
	if _, err := h.svc.Update(h.ctx, srv.ID, "Renamed", srv.URL, ""); err != nil {
		t.Fatalf("update keeping token: %v", err)
	}
	accts, err := h.svc.Accounts(h.ctx, srv.ID)
	if err != nil || len(accts) != 2 {
		t.Fatalf("accounts = %+v, %v", accts, err)
	}
	for _, a := range accts {
		if a.Owner != (a.ID == 1) {
			t.Fatalf("owner flag wrong: %+v", a)
		}
	}
}
