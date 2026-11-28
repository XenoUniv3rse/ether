package debrid

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type fakeHinter struct {
	mu     sync.Mutex
	asked  [][]CacheHintCandidate
	answer func(ctx context.Context, cands []CacheHintCandidate, found func(map[string][]string))
	// off stands for no enabled plugin with the capability.
	off bool
}

func (f *fakeHinter) Enabled() bool { return !f.off }

func (f *fakeHinter) CacheHints(ctx context.Context, cands []CacheHintCandidate, found func(map[string][]string)) {
	f.mu.Lock()
	f.asked = append(f.asked, slices.Clone(cands))
	answer := f.answer
	f.mu.Unlock()
	if answer != nil {
		answer(ctx, cands, found)
	}
}

func (f *fakeHinter) calls() [][]CacheHintCandidate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

func hintHash(n int) string { return fmt.Sprintf("%040x", n) }

func hintRelease(id string, n int) core.Release {
	return core.Release{ID: id, Protocol: core.ProtocolTorrent, InfoHash: hintHash(n), Score: 100}
}

// purposeCtx is tagged playback, as every source opened for playback,
// download or failover is, without a viewer's Play.
func purposeCtx(t *testing.T) context.Context {
	return core.WithIndexerPurpose(t.Context(), core.IndexerPurposePlayback)
}

// playCtx is a viewer's Play: tagged playback and marked explicit Play.
func playCtx(t *testing.T) context.Context {
	return core.WithExplicitPlay(purposeCtx(t))
}

// On an explicit Play, a cache-hint source is asked once about the
// shortlist's hashes, for the providers with no answer of their own. Its
// positives for pairs Ether asked about become in-memory hints; anything
// else it says, and every miss, writes nothing.
func TestPrepareSourcesAsksCacheHintsOnPlay(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid, config.DebridTorBox)
	tb := h.provs["torbox-1"]
	tb.bulk = true // TorBox answers for itself: not cached anywhere
	hinter := &fakeHinter{answer: func(_ context.Context, _ []CacheHintCandidate, found func(map[string][]string)) {
		found(map[string][]string{
			hintHash(1):                   {config.DebridRealDebrid, config.DebridTorBox, config.DebridAllDebrid},
			strings.ToUpper(hintHash(2)):  {config.DebridRealDebrid},
			hintHash(99):                  {config.DebridRealDebrid},
			hintHash(3) + "not-a-request": {config.DebridRealDebrid},
		})
	}}
	h.svc.SetCacheHinter(hinter)
	x := NewIndexer(&fakeInner{}, h.svc)
	private := hintRelease("r2p", 2)
	private.Private = true
	releases := []core.Release{
		hintRelease("r1", 1),
		hintRelease("r2", 2),
		private, // a private-tracker copy of r2's torrent
		hintRelease("r3", 3),
		{ID: "u1", Protocol: core.ProtocolUsenet},
	}
	x.PrepareSources(playCtx(t), releases)

	calls := hinter.calls()
	if len(calls) != 1 {
		t.Fatalf("hint source asked %d times, want once", len(calls))
	}
	want := []CacheHintCandidate{
		{InfoHash: hintHash(1), ProviderKinds: []string{config.DebridRealDebrid}},
		{InfoHash: hintHash(2), Private: true, ProviderKinds: []string{config.DebridRealDebrid}},
		{InfoHash: hintHash(3), ProviderKinds: []string{config.DebridRealDebrid}},
	}
	if fmt.Sprint(calls[0]) != fmt.Sprint(want) {
		t.Fatalf("asked %+v, want %+v", calls[0], want)
	}
	for _, n := range []int{1, 2} {
		refs := h.svc.KnownCachedBy(hintHash(n), nil)
		if len(refs) != 1 || refs[0].ID != "realdebrid-0" {
			t.Fatalf("hash %d: cached by %+v, want only the hinted Real-Debrid", n, refs)
		}
	}
	if h.svc.KnownCached(hintHash(99)) {
		t.Fatal("a hash Ether did not ask about was recorded")
	}
	if _, known, _ := h.svc.knownFor(t.Context(), "realdebrid-0", hintHash(3)); known {
		t.Fatal("a miss wrote an answer")
	}
	if cached, known, _ := h.svc.knownFor(t.Context(), "torbox-1", hintHash(1)); !known || cached {
		t.Fatal("a hint replaced TorBox's own answer")
	}
	for id, p := range h.provs {
		if p.callCount("acquire") != 0 || p.callCount("find") != 0 {
			t.Fatalf("%s: preparation spent an add: %+v", id, p.calls)
		}
	}
}

// Only a viewer's Play asks: the explicit-Play mark, never the playback
// purpose alone. Detail opens, console searches, warm-ups, pre-resolves,
// availability and background checks, and untagged work never send a hash
// to a hint source, marked or not; neither does a Play whose lead is
// already known cached or whose hashes every provider has answered.
func TestPrepareSourcesCacheHintsOnlyOnExplicitPlay(t *testing.T) {
	speculative, _ := core.WithSpeculative(playCtx(t))
	quiet := map[string]context.Context{
		"playback, no mark":   purposeCtx(t),
		"untagged":            t.Context(),
		"detail":              core.WithIndexerPurpose(t.Context(), core.IndexerPurposeDetail),
		"console":             core.WithIndexerPurpose(t.Context(), core.IndexerPurposeConsole),
		"warmup":              core.WithIndexerPurpose(t.Context(), core.IndexerPurposeWarmup),
		"availability":        core.WithIndexerPurpose(t.Context(), core.IndexerPurposeAvailability),
		"speculative play":    speculative,
		"unattended play":     core.WithUnattendedSearch(playCtx(t)),
		"background check":    core.WithBackgroundBudget(playCtx(t), func(context.Context, string, string, int64) error { return nil }),
		"background analysis": core.WithBackgroundAnalysis(playCtx(t)),
	}
	t.Run("no enabled hint plugin", func(t *testing.T) {
		h := newHarness(t, 1<<20, config.DebridRealDebrid)
		hinter := &fakeHinter{off: true}
		h.svc.SetCacheHinter(hinter)
		NewIndexer(&fakeInner{}, h.svc).PrepareSources(playCtx(t), []core.Release{hintRelease("r1", 1), hintRelease("r2", 2)})
		if n := len(hinter.calls()); n != 0 {
			t.Fatalf("hint source asked %d times with no enabled plugin", n)
		}
	})
	for name, ctx := range quiet {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, 1<<20, config.DebridRealDebrid)
			hinter := &fakeHinter{}
			h.svc.SetCacheHinter(hinter)
			NewIndexer(&fakeInner{}, h.svc).PrepareSources(ctx, []core.Release{hintRelease("r1", 1), hintRelease("r2", 2)})
			if n := len(hinter.calls()); n != 0 {
				t.Fatalf("hint source asked %d times", n)
			}
		})
	}
	t.Run("cached lead", func(t *testing.T) {
		h := newHarness(t, 1<<20, config.DebridRealDebrid, config.DebridTorBox)
		tb := h.provs["torbox-1"]
		tb.bulk = true
		tb.cached[hintHash(1)] = true
		hinter := &fakeHinter{}
		h.svc.SetCacheHinter(hinter)
		NewIndexer(&fakeInner{}, h.svc).PrepareSources(playCtx(t), []core.Release{hintRelease("r1", 1), hintRelease("r2", 2)})
		if n := len(hinter.calls()); n != 0 {
			t.Fatalf("hint source asked %d times with a cached lead", n)
		}
	})
	t.Run("all answered", func(t *testing.T) {
		h := newHarness(t, 1<<20, config.DebridTorBox)
		h.provs["torbox-0"].bulk = true
		hinter := &fakeHinter{}
		h.svc.SetCacheHinter(hinter)
		NewIndexer(&fakeInner{}, h.svc).PrepareSources(playCtx(t), []core.Release{hintRelease("r1", 1), hintRelease("r2", 2)})
		if n := len(hinter.calls()); n != 0 {
			t.Fatalf("hint source asked %d times about answered hashes", n)
		}
	})
	t.Run("play", func(t *testing.T) {
		h := newHarness(t, 1<<20, config.DebridRealDebrid)
		hinter := &fakeHinter{}
		h.svc.SetCacheHinter(hinter)
		NewIndexer(&fakeInner{}, h.svc).PrepareSources(playCtx(t), []core.Release{hintRelease("r1", 1), hintRelease("r2", 2)})
		if n := len(hinter.calls()); n != 1 {
			t.Fatalf("hint source asked %d times on Play, want once", n)
		}
	})
}

// One 500 ms deadline bounds the ask: an answer in time counts, Play never
// waits for a source that ignores the deadline, and what it says late
// writes nothing.
func TestPrepareSourcesCacheHintsDeadline(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid)
	release := make(chan struct{})
	lateDone := make(chan struct{})
	hinter := &fakeHinter{answer: func(_ context.Context, _ []CacheHintCandidate, found func(map[string][]string)) {
		found(map[string][]string{hintHash(1): {config.DebridRealDebrid}})
		<-release // ignores its deadline
		found(map[string][]string{hintHash(2): {config.DebridRealDebrid}})
		close(lateDone)
	}}
	h.svc.SetCacheHinter(hinter)
	start := time.Now()
	NewIndexer(&fakeInner{}, h.svc).PrepareSources(playCtx(t), []core.Release{hintRelease("r1", 1), hintRelease("r2", 2)})
	if took := time.Since(start); took > 700*time.Millisecond {
		t.Fatalf("Play waited %v for a hint source", took)
	}
	close(release)
	if len(hinter.calls()) == 0 {
		t.Fatal("hint source not asked")
	}
	<-lateDone
	if !h.svc.KnownCached(hintHash(1)) {
		t.Fatal("a timely hint was dropped")
	}
	if h.svc.KnownCached(hintHash(2)) {
		t.Fatal("a late hint was recorded")
	}
}

// The hint source sees at most the twelve torrents preparation looks at.
func TestPrepareSourcesCacheHintsShortlistCap(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid)
	hinter := &fakeHinter{}
	h.svc.SetCacheHinter(hinter)
	var releases []core.Release
	for i := 1; i <= 15; i++ {
		releases = append(releases, hintRelease(fmt.Sprint(i), i))
	}
	NewIndexer(&fakeInner{}, h.svc).PrepareSources(playCtx(t), releases)
	calls := hinter.calls()
	if len(calls) != 1 || len(calls[0]) != 12 || calls[0][0].InfoHash != hintHash(1) || calls[0][11].InfoHash != hintHash(12) {
		t.Fatalf("asked %+v", calls)
	}
}

// A hint decides which account is tried: on a Play, the second account,
// hinted cached, serves the release, and the first, with no hint and no
// answer of its own, spends no add.
func TestCacheHintedAccountIsTriedFirst(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid, config.DebridTorBox)
	h.cacheEverywhere(testHash)
	h.svc.SetCacheHinter(&fakeHinter{answer: func(_ context.Context, _ []CacheHintCandidate, found func(map[string][]string)) {
		found(map[string][]string{testHash: {config.DebridTorBox}})
	}})
	prepared := NewIndexer(&fakeInner{}, h.svc).PrepareSources(playCtx(t), []core.Release{torrentRelease("r1")})
	if len(prepared) != 1 || len(prepared[0].DebridCached) != 0 {
		t.Fatalf("prepared %+v", prepared)
	}
	b, err := h.svc.Acquire(playCtx(t), prepared[0])
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if p, _ := DecodePayload(b); p.Provider != "torbox-1" {
		t.Fatalf("served by %q, want the hinted torbox-1", p.Provider)
	}
	rd := h.provs["realdebrid-0"]
	rd.mu.Lock()
	adds := rd.adds
	rd.mu.Unlock()
	if n := rd.callCount("acquire"); n != 0 || adds != 0 {
		t.Fatalf("unhinted first account asked %d times, %d adds", n, adds)
	}
}

// rememberAnswer stores id's own answer for hash where acquisition reads
// it (debrid_answers), and gives the account a fresh pacer, as after a
// restart: nothing about the hash is left in memory.
func rememberAnswer(t *testing.T, h *harness, id, hash, verdict string) {
	t.Helper()
	db := h.db.(*sql.DB)
	(&accountPacer{identity: id, now: time.Now, answers: &answerStore{db: db}}).
		writeAnswer(t.Context(), answerKey{hash: hash}, learnedAnswer{verdict: verdict}, time.Now())
	for _, e := range h.svc.active() {
		if e.cfg.ID == id {
			e.api = &accountPacer{identity: id, now: time.Now, answers: &answerStore{db: db}}
			return
		}
	}
	t.Fatalf("no account %s", id)
}

// An account's own remembered answer stands: Ether never asks about that
// pair, and a plugin's positive for it is dropped, even after a restart and
// once the short discovery memory has nothing.
func TestCacheHintsSkipRememberedAnswers(t *testing.T) {
	t.Run("only account of its kind", func(t *testing.T) {
		h := newHarness(t, 1<<20, config.DebridRealDebrid, config.DebridTorBox)
		rememberAnswer(t, h, "realdebrid-0", hintHash(1), answerCold)
		rememberAnswer(t, h, "realdebrid-0", hintHash(3), answerBlockedHash)
		hinter := &fakeHinter{answer: func(_ context.Context, _ []CacheHintCandidate, found func(map[string][]string)) {
			found(map[string][]string{
				hintHash(1): {config.DebridRealDebrid, config.DebridTorBox},
				hintHash(2): {config.DebridRealDebrid},
				hintHash(3): {config.DebridRealDebrid},
			})
		}}
		h.svc.SetCacheHinter(hinter)
		NewIndexer(&fakeInner{}, h.svc).PrepareSources(playCtx(t), []core.Release{hintRelease("r1", 1), hintRelease("r2", 2), hintRelease("r3", 3)})
		calls := hinter.calls()
		want := []CacheHintCandidate{
			{InfoHash: hintHash(1), ProviderKinds: []string{config.DebridTorBox}},
			{InfoHash: hintHash(2), ProviderKinds: []string{config.DebridRealDebrid, config.DebridTorBox}},
			{InfoHash: hintHash(3), ProviderKinds: []string{config.DebridTorBox}},
		}
		if len(calls) != 1 || fmt.Sprint(calls[0]) != fmt.Sprint(want) {
			t.Fatalf("asked %+v, want %+v", calls, want)
		}
		if refs := h.svc.KnownCachedBy(hintHash(1), nil); len(refs) != 1 || refs[0].ID != "torbox-1" {
			t.Fatalf("hash 1: cached by %+v, want only torbox-1", refs)
		}
		if refs := h.svc.KnownCachedBy(hintHash(3), nil); len(refs) != 0 {
			t.Fatalf("hash 3: cached by %+v, want none (Real-Debrid refused it)", refs)
		}
		if refs := h.svc.KnownCachedBy(hintHash(2), nil); len(refs) != 1 || refs[0].ID != "realdebrid-0" {
			t.Fatalf("hash 2: cached by %+v, want realdebrid-0", refs)
		}
	})
	t.Run("one of two accounts of a kind", func(t *testing.T) {
		h := newHarness(t, 1<<20, config.DebridRealDebrid, config.DebridRealDebrid)
		rememberAnswer(t, h, "realdebrid-0", hintHash(1), answerCold)
		hinter := &fakeHinter{answer: func(_ context.Context, _ []CacheHintCandidate, found func(map[string][]string)) {
			found(map[string][]string{hintHash(1): {config.DebridRealDebrid}})
		}}
		h.svc.SetCacheHinter(hinter)
		NewIndexer(&fakeInner{}, h.svc).PrepareSources(playCtx(t), []core.Release{hintRelease("r1", 1)})
		if calls := hinter.calls(); len(calls) != 1 {
			t.Fatalf("asked %+v", calls)
		}
		if refs := h.svc.KnownCachedBy(hintHash(1), nil); len(refs) != 1 || refs[0].ID != "realdebrid-1" {
			t.Fatalf("cached by %+v, want only the account with no answer (realdebrid-1)", refs)
		}
	})
}

// Hints whose reads ran past their deadline are not written: what the
// reads did not see may be an answer.
func TestApplyHintsAfterItsDeadlineWritesNothing(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rel := torrentRelease("r1")
	rel.DebridCached = []string{config.DebridRealDebrid}
	h.svc.ApplyHints(ctx, []core.Release{rel})
	if h.svc.KnownCached(testHash) {
		t.Fatal("a hint was written after its context ended")
	}
}

// A lead the account itself remembers as cached (debrid_answers) is known
// cached after a restart too: the round is skipped.
func TestCacheHintsSkipLeadRememberedCached(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid)
	rememberAnswer(t, h, "realdebrid-0", hintHash(1), answerCached)
	hinter := &fakeHinter{}
	h.svc.SetCacheHinter(hinter)
	NewIndexer(&fakeInner{}, h.svc).PrepareSources(playCtx(t), []core.Release{hintRelease("r1", 1), hintRelease("r2", 2)})
	if calls := hinter.calls(); len(calls) != 0 {
		t.Fatalf("hint source asked %+v with a lead remembered cached", calls)
	}
}

// Without any plugin, an ordinary search ranks a torrent whose source marks
// it cached ahead, as before, when the account's own remembered answer is
// cached too and the discovery memory has expired: only a remembered "not
// cached" or refusal outranks a positive marker.
func TestApplyHintsKeepsMarkerTheAccountRemembersCached(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid)
	rememberAnswer(t, h, "realdebrid-0", hintHash(2), answerCached)
	rememberAnswer(t, h, "realdebrid-0", hintHash(3), answerCold)
	h.svc.knownMu.Lock()
	h.svc.known[knownKey("realdebrid-0", hintHash(2))] = knownState{cached: true, until: time.Now().Add(-time.Minute)}
	h.svc.knownMu.Unlock()
	marked := func(id string, n int) core.Release {
		r := hintRelease(id, n)
		r.DebridCached = []string{config.DebridRealDebrid}
		return r
	}
	res := core.IndexerSearchResult{Releases: []core.Release{hintRelease("r1", 1), marked("r3", 3), marked("r2", 2)}}
	got := NewIndexer(&fakeInner{}, h.svc).filterControlledSearch(t.Context(), res, true).Releases
	var order []string
	for _, r := range got {
		order = append(order, r.ID)
	}
	if fmt.Sprint(order) != "[r2 r1 r3]" {
		t.Fatalf("ranked %v, want the marked, remembered-cached r2 first and r3 (remembered cold) unpromoted", order)
	}
	if !h.svc.KnownCached(hintHash(2)) || h.svc.KnownCached(hintHash(3)) {
		t.Fatal("marker for the remembered-cached hash dropped, or the remembered-cold one accepted")
	}
}

// The deadline starts before the first read: a cache lock held past it does
// not hold Play, and nothing is asked or written.
func TestCacheHintsDeadlineCoversContendedReads(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid)
	hinter := &fakeHinter{}
	h.svc.SetCacheHinter(hinter)
	x := NewIndexer(&fakeInner{}, h.svc)
	h.svc.knownMu.Lock()
	held := time.AfterFunc(time.Second, h.svc.knownMu.Unlock) // a contended read, held past the deadline
	start := time.Now()
	x.askCacheHints(playCtx(t), []core.Release{hintRelease("r1", 1), hintRelease("r2", 2)})
	took := time.Since(start)
	if held.Stop() {
		h.svc.knownMu.Unlock()
	}
	if took > 700*time.Millisecond {
		t.Fatalf("Play waited %v behind a held cache lock", took)
	}
	if calls := hinter.calls(); len(calls) != 0 {
		t.Fatalf("asked %+v after the deadline", calls)
	}
}

// Ordering accounts for an unmarked acquisition reads each account's id and
// kind under its lock while a configuration edit replaces them (run with
// -race).
func TestMarkedFirstReadsAccountUnderItsLock(t *testing.T) {
	h := newHarness(t, 1<<20, config.DebridRealDebrid, config.DebridTorBox)
	cfg := h.svc.Config()
	entries := h.svc.active()
	rel := torrentRelease("r1")
	start := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-start
		for range 200 {
			h.svc.UpdateConfig(cfg)
		}
	}()
	close(start)
	for range 200 {
		h.svc.markedFirst(t.Context(), entries, rel)
	}
	<-done
}
