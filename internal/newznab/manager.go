package newznab

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/vault"
)

// Manager holds all configured indexers and spreads searches across
// them within their budgets. It implements core.Indexer so the
// resolver can treat the fleet as one indexer; FetchNZB routes by
// the release's Indexer name.
type Manager struct {
	events eventBus
	db     *sql.DB
	opts   Options
	log    *slog.Logger
	par    int
	hosts  *HostLimiter

	mu             sync.RWMutex
	clients        []*Client
	searchTimingMu sync.RWMutex
	searchTimings  []SearchTiming
	timingHistory  map[string]*indexerTimingHistory
	shareLogMu     sync.Mutex
	shareLogDay    map[string]string

	bgMu          sync.Mutex
	bgClosed      bool
	bgNextID      int
	bgCancels     map[int]context.CancelFunc
	detachedCount int
	bgWG          sync.WaitGroup
	flights       searchFlights
	pluginRefs    sync.Map // standalone managers without a database
}

// Close cancels any detached background cache-fill searches started by
// SearchWithOptions and waits for them to finish.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.bgMu.Lock()
	m.bgClosed = true
	cancels := make([]context.CancelFunc, 0, len(m.bgCancels))
	for _, cancel := range m.bgCancels {
		cancels = append(cancels, cancel)
	}
	m.bgMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	m.bgWG.Wait()
}

// SearchTiming records the most recent aggregate search's per-indexer cost.
type SearchTiming struct {
	Indexer  string
	Duration time.Duration
	Err      string
}

// SearchTimings returns a copy of the most recent per-indexer search timings.
func (m *Manager) SearchTimings() []SearchTiming {
	m.searchTimingMu.RLock()
	defer m.searchTimingMu.RUnlock()
	return append([]SearchTiming(nil), m.searchTimings...)
}

func (m *Manager) shouldLogSpeculativeShare(key, day string) bool {
	m.shareLogMu.Lock()
	defer m.shareLogMu.Unlock()
	if m.shareLogDay == nil {
		m.shareLogDay = map[string]string{}
	}
	if m.shareLogDay[key] == day {
		return false
	}
	m.shareLogDay[key] = day
	return true
}

func (m *Manager) clearSpeculativeShareLog(key string) {
	m.shareLogMu.Lock()
	delete(m.shareLogDay, key)
	m.shareLogMu.Unlock()
}

var (
	_ core.Indexer                 = (*Manager)(nil)
	_ core.StreamingIndexer        = (*Manager)(nil)
	_ core.ControlledSearchIndexer = (*Manager)(nil)
	_ core.IndexerDirectory        = (*Manager)(nil)
)

// NewManager builds a Manager from indexer configs.
func NewManager(db *sql.DB, cfgs []config.Indexer, opts Options) *Manager {
	if opts.HostLimiter == nil {
		opts.HostLimiter = NewHostLimiter(opts.MaxPerHost)
	}
	m := &Manager{db: db, opts: opts, par: opts.MaxParallel, hosts: opts.HostLimiter}
	m.log = opts.Logger
	if m.log == nil {
		m.log = slog.Default()
	}
	m.Reload(cfgs)
	return m
}

// Reload replaces the indexer set (call on config change). Existing
// clients with an unchanged ID keep their in-memory caches.
func (m *Manager) Reload(cfgs []config.Indexer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := map[string]*Client{}
	for _, cl := range m.clients {
		old[cl.indexerID()] = cl
	}
	opts := m.opts
	opts.HostLimiter = m.hosts
	opts.onEvent = m.emitEvent
	next := make([]*Client, 0, len(cfgs))
	for _, cfg := range cfgs {
		id := cfg.ID
		if id == "" {
			id = cfg.Name
		}
		if cl, ok := old[id]; ok {
			cl.hosts = m.hosts
			cl.UpdateConfig(cfg)
			cl.SetClientIdentity(m.opts.ClientIdentity)
			cl.setOnEvent(m.emitEvent)
			next = append(next, cl)
			continue
		}
		next = append(next, New(cfg, m.db, opts))
	}
	sort.SliceStable(next, func(i, j int) bool {
		return next[i].Config().Priority < next[j].Config().Priority
	})
	m.clients = next
	keep := make(map[string]struct{}, len(next))
	for _, cl := range next {
		keep[cl.indexerID()] = struct{}{}
	}
	m.searchTimingMu.Lock()
	for id := range m.timingHistory {
		if _, ok := keep[id]; !ok {
			delete(m.timingHistory, id)
		}
	}
	m.searchTimingMu.Unlock()
}

// ApplyConfig reloads indexers and the search-cache TTL from a full
// config snapshot; subscribe it to config.Store changes.
func (m *Manager) ApplyConfig(cfg config.Config) {
	m.opts.ClientIdentity = cfg.ClientIdentity
	if d := cfg.Resolver.SearchCacheTTL.D(); d > 0 {
		m.opts.SearchCacheTTL = d
		for _, cl := range m.Clients() {
			cl.SetCacheTTL(d)
		}
	}
	indexers := append([]config.Indexer(nil), cfg.Indexers...)
	if cfg.Prowlarr.Disabled {
		for i := range indexers {
			if indexers[i].ProwlarrImport != nil {
				indexers[i].Enabled = false
			}
		}
	}
	m.Reload(indexers)
}

// Clients returns the enabled and disabled clients in priority order.
func (m *Manager) Clients() []*Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]*Client(nil), m.clients...)
}

// Indexers returns the clients as core.Indexer values.
func (m *Manager) Indexers() []core.Indexer {
	cs := m.Clients()
	out := make([]core.Indexer, len(cs))
	for i, c := range cs {
		out[i] = c
	}
	return out
}

// Name implements core.Indexer.
func (m *Manager) Name() string { return "indexers" }

// MissingSources reports core.ErrNoSources when no indexer is enabled:
// a search would return nothing whatever the title.
func (m *Manager) MissingSources() error {
	for _, cl := range m.Clients() {
		if cl.Config().Enabled {
			return nil
		}
	}
	return core.ErrNoSources
}

// HasTorrentIndexer reports whether an enabled indexer returns torrents
// (Torznab, a Stremio addon or a torrent built-in site).
func (m *Manager) HasTorrentIndexer() bool {
	for _, cl := range m.Clients() {
		if cfg := cl.Config(); cfg.Enabled && cfg.IsTorrent() {
			return true
		}
	}
	return false
}

// Remaining returns budget state for every indexer.
func (m *Manager) Remaining(ctx context.Context) ([]Remaining, error) {
	var out []Remaining
	for _, cl := range m.Clients() {
		r, err := cl.Remaining(ctx)
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Search implements core.Indexer: it queries enabled indexers in parallel
// (up to MaxParallel), letting each client check its cache before budgets, merges by priority order
// and de-duplicates by normalised name + size.
func (m *Manager) Search(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	return m.SearchWith(ctx, req, nil)
}

// SearchRescue runs a second-pass rescue fan-out across all enabled indexers
// when the primary Search results had no playable candidate.
func (m *Manager) SearchRescue(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	return m.SearchRescueWith(ctx, req, nil)
}

// SearchRescueWith is SearchRescue with per-indexer early arrival callbacks.
// A rescue is Play's (or the owner's) second pass and is tagged rescue.
// Speculative work never fans out into one: under the rescue label it would
// escape the speculative share, so it sends nothing.
func (m *Manager) SearchRescueWith(ctx context.Context, req core.SearchRequest, arrived func([]core.Release)) ([]core.Release, error) {
	if core.SpeculativeIndexerWork(ctx) {
		return nil, nil
	}
	ctx = core.WithIndexerPurpose(ctx, core.IndexerPurposeRescue)
	clients := m.Clients()
	type slot struct {
		results []core.Release
		err     error
	}
	slots := make([]slot, len(clients))
	parallel := m.par
	if parallel <= 0 {
		parallel = max(1, len(clients))
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, cl := range clients {
		cfg := cl.Config()
		if !cfg.Enabled || core.IndexerSkipped(ctx, cl.indexerID()) || (core.TorrentOnlySearch(ctx) && !cfg.IsTorrent()) {
			continue
		}
		wg.Add(1)
		go func(i int, cl *Client) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				slots[i].err = ctx.Err()
				return
			}
			res, err := cl.SearchRescue(ctx, req)
			slots[i].results, slots[i].err = res, err
			if arrived != nil && err == nil && len(res) > 0 {
				arrived(res)
			}
		}(i, cl)
	}
	wg.Wait()
	seen := map[string]int{}
	var merged []core.Release
	for _, s := range slots {
		if missingAnswer(s.err) {
			core.NoteSearchIncomplete(ctx)
		}
		for _, r := range s.results {
			key := mergeDedupeKey(r)
			if idx, ok := seen[key]; ok {
				foldDuplicate(&merged[idx], r)
				continue
			}
			seen[key] = len(merged)
			merged = append(merged, r)
		}
	}
	return merged, nil
}

// SearchWith is Search, also handing each indexer's results to arrived as
// soon as that indexer answers (concurrently, from its own goroutine), so
// callers can start follow-up work, such as debrid cache checks, before
// the slowest indexer is done.
func (m *Manager) SearchWith(ctx context.Context, req core.SearchRequest, arrived func([]core.Release)) ([]core.Release, error) {
	clients := m.Clients()
	ctx = m.reuseWarmSearch(ctx, req, clients)
	type slot struct {
		results  []core.Release
		err      error
		skipped  bool
		duration time.Duration
	}
	slots := make([]slot, len(clients))
	parallel := m.par
	if parallel <= 0 {
		parallel = max(1, len(clients))
	}
	sem := make(chan struct{}, parallel)
	searchStart := time.Now()
	var wg sync.WaitGroup
	for i, cl := range clients {
		cfg := cl.Config()
		if !cfg.Enabled {
			slots[i].skipped = true
			continue
		}
		id := cl.indexerID()
		name := cl.Name()
		if core.IndexerSkipped(ctx, id) || (core.TorrentOnlySearch(ctx) && !cfg.IsTorrent()) {
			slots[i].skipped = true
			core.ObserveIndexerStatus(ctx, core.IndexerSearchStatus{
				IndexerID: id,
				Indexer:   name,
				Status:    core.IndexerStatusSkipped,
				MS:        0,
			})
			continue
		}
		wg.Add(1)
		go func(i int, cl *Client, id, name string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				dur := time.Since(searchStart)
				slots[i].duration = dur
				slots[i].err = ctx.Err()
				core.ObserveIndexerStatus(ctx, buildIndexerStatus(ctx, id, name, cfg.Name, nil, slots[i].err, dur))
				m.recordIndexerTiming(id, dur, slots[i].err)
				return
			}
			started := time.Now()
			res, err := m.searchClientCoalescedWith(ctx, cl, req, arrived)
			dur := time.Since(started)
			slots[i].duration = dur
			slots[i].results, slots[i].err = res, err
			core.ObserveIndexerStatus(ctx, buildIndexerStatus(ctx, id, name, cfg.Name, res, err, dur))
			m.recordIndexerTiming(id, dur, err)
		}(i, cl, id, name)
	}
	wg.Wait()
	timings := make([]SearchTiming, 0, len(clients))
	for i, s := range slots {
		if missingAnswer(s.err) {
			core.NoteSearchIncomplete(ctx)
		}
		if !s.skipped {
			label := clients[i].Name()
			e := ""
			if s.err != nil {
				e = s.err.Error()
			}
			timings = append(timings, SearchTiming{Indexer: label, Duration: s.duration, Err: e})
		}
	}
	m.searchTimingMu.Lock()
	m.searchTimings = timings
	m.searchTimingMu.Unlock()
	seen := map[string]int{}
	var merged []core.Release
	var errs []error
	anySearched := false
	purpose := usagePurpose(ctx)
	day := time.Now().UTC().Format("2006-01-02")
	for i, s := range slots {
		shareKey := clients[i].Name() + "|" + purpose
		if s.err != nil {
			errs = append(errs, s.err)
			if errors.Is(s.err, errSpeculativeShare) {
				if m.shouldLogSpeculativeShare(shareKey, day) {
					m.log.Debug("indexer search failed", "indexer", clients[i].Name(), "err", s.err)
				}
			} else {
				m.log.Debug("indexer search failed", "indexer", clients[i].Name(), "err", s.err)
			}
		} else if !s.skipped {
			m.clearSpeculativeShareLog(shareKey)
		}
		if !s.skipped {
			anySearched = true
		}
		for _, r := range s.results {
			key := mergeDedupeKey(r)
			if idx, ok := seen[key]; ok {
				foldDuplicate(&merged[idx], r)
				continue
			}
			seen[key] = len(merged)
			merged = append(merged, r)
		}
	}
	if len(merged) == 0 && len(errs) > 0 {
		allBudget := true
		for _, e := range errs {
			if !errors.Is(e, core.ErrBudgetExhausted) {
				allBudget = false
			}
		}
		if allBudget {
			return nil, fmt.Errorf("all indexers: %w", core.ErrBudgetExhausted)
		}
		if !anySearched && len(clients) > 0 {
			return nil, errs[0]
		}
		if anySearched {
			// Some indexers errored and none returned results.
			return nil, errs[0]
		}
	}
	return merged, nil
}

// foldDuplicate adds r, a duplicate of winner, to winner's Fallbacks. When
// both carry the same info hash they are one torrent, so r's cached markers
// join winner's; a name-and-size match proves no such thing.
func foldDuplicate(winner *core.Release, r core.Release) {
	if winner.InfoHash != "" && winner.InfoHash == r.InfoHash {
		for _, kind := range r.DebridCached {
			if !slices.Contains(winner.DebridCached, kind) {
				// Clipped, so append copies: the slice may be shared with a cached result.
				winner.DebridCached = append(slices.Clip(winner.DebridCached), kind)
			}
		}
	}
	dup := r
	dup.Fallbacks = nil
	winner.Fallbacks = append(winner.Fallbacks, dup)
}

func mergeDedupeKey(r core.Release) string {
	key := strings.ToLower(strings.TrimSpace(r.Name)) + "|" + fmt.Sprint(r.Size)
	if r.Size == 0 {
		key = strings.ToLower(strings.TrimSpace(r.Name))
	}
	if r.Protocol != core.ProtocolUsenet {
		// A torrent and a Usenet post of the same name are distinct
		// sources; torrents dedupe by info hash when known.
		key = r.Protocol + "|" + key
		if r.InfoHash != "" {
			key = r.Protocol + "|" + r.InfoHash
		}
	}
	return key
}

// FetchNZB implements core.Indexer by routing to the originating client.
// Events are emitted by the client at the point of cache hit, budget refusal
// or HTTP download.
func (m *Manager) FetchNZB(ctx context.Context, r core.Release) ([]byte, error) {
	// A library release is answered from the vault, never an indexer.
	if key := vault.KeyOf(r); key != "" {
		if m.opts.Vault == nil {
			return nil, vault.ErrPayloadMissing
		}
		rec, err := m.opts.Vault.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if rec.Kind != vault.KindUsenet || len(rec.XML) == 0 {
			return nil, vault.ErrPayloadMissing
		}
		_ = m.opts.Vault.Touch(ctx, key, true)
		return rec.XML, nil
	}
	cl, err := m.releaseClient(r)
	if err != nil {
		return nil, err
	}
	if r.IndexerID == "" {
		if cl.Config().UsesBackend() {
			r = cl.bindReleases([]core.Release{r})[0]
		} else {
			r = cl.feedReleases([]core.Release{r})[0]
		}
	}
	b, _, err := cl.fetchNZBReporting(ctx, r)
	return b, err
}

// missingAnswer reports an indexer whose answer a fleet search lacks because
// it was paused or did not answer, rather than because it found nothing.
func missingAnswer(err error) bool {
	return errors.Is(err, ErrIndexerPaused) || errors.Is(err, core.ErrForegroundOnly) || transientFailure(err)
}
