package newznab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

const (
	// DefaultSearchSettle is the default settle window after the first results
	// arrive when SearchOptions.FirstResults is true and SettleSet is false.
	DefaultSearchSettle = 250 * time.Millisecond

	// maxDetachedSearchFills bounds how many in-flight per-indexer cache-fill
	// searches may remain detached after early return or WaitTimeout across a
	// Manager at once.
	maxDetachedSearchFills = 64

	// Keep a single playback search from using up the detached fan-out.
	maxContinuingSearchParallel = 12

	// maxIndexerTimingSamples caps the per-indexer sliding window used to
	// report recent median/last latency on sources.resolve({kind: "indexers"}).
	maxIndexerTimingSamples = 20
)

// SearchOptions aliases core.IndexerSearchOptions for Manager.SearchWithOptions.
type SearchOptions = core.IndexerSearchOptions

// SearchResult aliases core.IndexerSearchResult for Manager.SearchWithOptions.
type SearchResult = core.IndexerSearchResult

type indexerTimingHistory struct {
	samples   []int64
	lastMS    int64
	hasSample bool
	lastError string
}

type searchFlight struct {
	done    chan struct{}
	results []core.Release
	err     error
}

type searchSlot struct {
	client         *Client
	id             string
	name           string
	configuredName string
	skipped        bool
	started        time.Time
	finished       bool
	duration       time.Duration
	results        []core.Release
	err            error
}

// EnabledIndexerStatuses returns one status entry per currently enabled indexer
// in priority order, all tagged with status and 0 ms.
func (m *Manager) EnabledIndexerStatuses(status string) []core.IndexerSearchStatus {
	if m == nil {
		return nil
	}
	clients := m.Clients()
	out := make([]core.IndexerSearchStatus, 0, len(clients))
	for _, cl := range clients {
		if !cl.Config().Enabled {
			continue
		}
		out = append(out, core.IndexerSearchStatus{
			IndexerID:      cl.indexerID(),
			Indexer:        cl.Name(),
			ConfiguredName: cl.Config().Name,
			Status:         status,
			MS:             0,
		})
	}
	return out
}

func indexerKind(cfg config.Indexer) string {
	switch {
	case cfg.IsBuiltin():
		return "builtin"
	case cfg.IsStremio():
		return "stremio"
	case cfg.IsZilean():
		return "zilean"
	case cfg.IsAddon():
		return "addon"
	case cfg.IsTorrent():
		return "torznab"
	default:
		return "newznab"
	}
}

// EnabledIndexers lists the owner's currently enabled indexers in priority
// order with kind, recent median/last latency (from Manager's timing history),
// and sanitized lastError. It never returns API keys, URLs, secrets, or cookies.
func (m *Manager) EnabledIndexers() []core.IndexerSearchStatus {
	if m == nil {
		return []core.IndexerSearchStatus{}
	}
	clients := m.Clients()
	m.searchTimingMu.RLock()
	out := make([]core.IndexerSearchStatus, 0, len(clients))
	for _, cl := range clients {
		cfg := cl.Config()
		if !cfg.Enabled {
			continue
		}
		id := cl.indexerID()
		entry := core.IndexerSearchStatus{
			IndexerID:      id,
			Indexer:        cl.Name(),
			ConfiguredName: cfg.Name,
			Kind:           indexerKind(cfg),
		}
		if h := m.timingHistory[id]; h != nil && h.hasSample {
			med := medianInt64(h.samples)
			last := h.lastMS
			entry.MedianMS = &med
			entry.LastMS = &last
			entry.LastError = h.lastError
		}
		out = append(out, entry)
	}
	m.searchTimingMu.RUnlock()
	return out
}

func medianInt64(samples []int64) int64 {
	if len(samples) == 0 {
		return 0
	}
	cp := make([]int64, len(samples))
	copy(cp, samples)
	slices.Sort(cp)
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}

func (m *Manager) recordIndexerTiming(id string, dur time.Duration, err error) {
	if m == nil || id == "" {
		return
	}
	// Caller cancellation (for example indexer_search_cancel) and breaker
	// backoff skips are neither indexer latency samples nor new errors.
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, ErrIndexerPaused)) {
		return
	}
	ms := max(int64(0), dur.Milliseconds())
	errCode := ""
	if err != nil {
		errCode = classifyIndexerError(err)
	}
	m.searchTimingMu.Lock()
	m.recordIndexerTimingLocked(id, ms, errCode)
	m.searchTimingMu.Unlock()
}

func (m *Manager) recordIndexerTimingLocked(id string, ms int64, errCode string) {
	if m.timingHistory == nil {
		m.timingHistory = map[string]*indexerTimingHistory{}
	}
	h := m.timingHistory[id]
	if h == nil {
		h = &indexerTimingHistory{}
		m.timingHistory[id] = h
	}
	h.samples = append(h.samples, ms)
	if len(h.samples) > maxIndexerTimingSamples {
		h.samples = append(h.samples[:0], h.samples[len(h.samples)-maxIndexerTimingSamples:]...)
	}
	h.lastMS = ms
	h.hasSample = true
	h.lastError = errCode
}

func buildIndexerStatus(ctx context.Context, id, name, configuredName string, res []core.Release, err error, dur time.Duration) core.IndexerSearchStatus {
	st := core.IndexerSearchStatus{
		IndexerID:      id,
		Indexer:        name,
		ConfiguredName: configuredName,
		MS:             max(int64(0), dur.Milliseconds()),
	}
	if err != nil {
		switch {
		case errors.Is(err, ErrIndexerPaused):
			st.Status = core.IndexerStatusSkipped
			st.Reason = core.IndexerSkipReasonPaused
			st.MS = 0
		case errors.Is(err, context.DeadlineExceeded) || (ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)):
			st.Status = core.IndexerStatusTimedOut
		default:
			st.Status = core.IndexerStatusError
			st.Error = classifyIndexerError(err)
		}
		return st
	}
	st.Status = core.IndexerStatusAnswered
	st.Results = len(res)
	return st
}

func normalizeIndexerFilter(list []string) map[string]bool {
	var set map[string]bool
	for _, raw := range list {
		k := strings.ToLower(strings.TrimSpace(raw))
		if k == "" {
			continue
		}
		if set == nil {
			set = map[string]bool{}
		}
		set[k] = true
	}
	return set
}

func matchesIndexerFilter(set map[string]bool, id, name string) bool {
	if len(set) == 0 {
		return false
	}
	if set[strings.ToLower(strings.TrimSpace(id))] {
		return true
	}
	if n := strings.ToLower(strings.TrimSpace(name)); n != "" && set[n] {
		return true
	}
	return false
}

// SearchWithOptions queries enabled indexers in parallel subject to opts,
// returning merged priority-ordered releases and per-indexer status for every
// enabled indexer.
func (m *Manager) SearchWithOptions(ctx context.Context, req core.SearchRequest, opts SearchOptions) (SearchResult, error) {
	allClients := m.Clients()
	skipSet := normalizeIndexerFilter(opts.SkipIndexers)
	onlySet := normalizeIndexerFilter(opts.OnlyIndexers)

	slots := make([]searchSlot, 0, len(allClients))
	skipIDs := map[string]bool{}
	active := 0
	for _, cl := range allClients {
		cfg := cl.Config()
		if !cfg.Enabled {
			continue
		}
		id := cl.indexerID()
		name := cl.Name()
		skipped := core.IndexerSkipped(ctx, id) ||
			(core.TorrentOnlySearch(ctx) && !cfg.IsTorrent()) ||
			matchesIndexerFilter(skipSet, id, name) ||
			(len(onlySet) > 0 && !matchesIndexerFilter(onlySet, id, name))
		if skipped {
			skipIDs[id] = true
		} else {
			active++
		}
		slots = append(slots, searchSlot{
			client:         cl,
			id:             id,
			name:           name,
			configuredName: cfg.Name,
			skipped:        skipped,
		})
	}

	if active == 0 {
		statuses := make([]core.IndexerSearchStatus, len(slots))
		for i, s := range slots {
			statuses[i] = core.IndexerSearchStatus{
				IndexerID: s.id,
				Indexer:   s.name,
				Status:    core.IndexerStatusSkipped,
				MS:        0,
			}
		}
		return SearchResult{Indexers: statuses}, nil
	}

	if len(skipIDs) > 0 {
		ctx = core.WithIndexerSkip(ctx, skipIDs)
	}

	earlyReturn := opts.FirstResults || opts.MinResults > 0
	hasWaitTimeout := opts.WaitTimeout > 0
	detachOnCutoff := earlyReturn || hasWaitTimeout

	var settle time.Duration
	switch {
	case opts.SettleSet:
		settle = max(0, opts.Settle)
	case opts.FirstResults:
		settle = DefaultSearchSettle
	default:
		settle = 0
	}

	bgLimit := opts.BackgroundTimeout
	if bgLimit <= 0 {
		bgLimit = defaultHTTPTimeout
	}

	runCtx := ctx
	var bgCancel context.CancelFunc
	var bgID int
	if detachOnCutoff {
		m.bgMu.Lock()
		if m.bgClosed {
			m.bgMu.Unlock()
			return SearchResult{}, context.Canceled
		}
		parent := context.WithoutCancel(ctx)
		if opts.ContinuationContext != nil {
			// Preserve search-specific values on ctx while taking the
			// lifetime from the enclosing resolve, not the guest slice.
			var cancel context.CancelFunc
			parent, cancel = context.WithCancel(parent)
			stop := context.AfterFunc(opts.ContinuationContext, cancel)
			var timeoutCancel context.CancelFunc
			runCtx, timeoutCancel = context.WithTimeout(withSearchCeiling(parent), bgLimit)
			bgCancel = func() {
				stop()
				cancel()
				timeoutCancel()
			}
		} else {
			runCtx, bgCancel = context.WithTimeout(withSearchCeiling(parent), bgLimit)
		}
		if m.bgCancels == nil {
			m.bgCancels = map[int]context.CancelFunc{}
		}
		m.bgNextID++
		bgID = m.bgNextID
		m.bgCancels[bgID] = bgCancel
		m.bgWG.Add(1)
		m.bgMu.Unlock()
	}

	finishBG := func() {
		if !detachOnCutoff {
			return
		}
		m.bgMu.Lock()
		delete(m.bgCancels, bgID)
		m.bgMu.Unlock()
		bgCancel()
		m.bgWG.Done()
	}

	parallel := m.par
	if parallel <= 0 {
		parallel = max(1, active)
	}
	if opts.ContinueSearch != nil {
		parallel = min(parallel, maxContinuingSearchParallel)
	}
	sem := make(chan struct{}, parallel)

	var stateMu sync.Mutex
	seenKeys := make(map[string]struct{})
	doneCount := 0
	notify := make(chan struct{}, 1)
	signal := func() {
		select {
		case notify <- struct{}{}:
		default:
		}
	}

	searchStart := time.Now()
	var wg sync.WaitGroup
	for i := range slots {
		if slots[i].skipped {
			continue
		}
		wg.Add(1)
		go func(idx int, cl *Client) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-runCtx.Done():
				stateMu.Lock()
				slots[idx].err = runCtx.Err()
				slots[idx].duration = time.Since(searchStart)
				slots[idx].finished = true
				doneCount++
				stateMu.Unlock()
				signal()
				return
			}
			started := time.Now()
			stateMu.Lock()
			slots[idx].started = started
			stateMu.Unlock()

			res, err := m.searchClientCoalesced(runCtx, cl, req)
			dur := time.Since(started)

			stateMu.Lock()
			slots[idx].results = res
			slots[idx].err = err
			slots[idx].duration = dur
			slots[idx].finished = true
			if err == nil {
				for _, r := range res {
					seenKeys[mergeDedupeKey(r)] = struct{}{}
				}
			}
			doneCount++
			stateMu.Unlock()
			signal()
		}(i, slots[i].client)
	}

	// When WaitTimeout reaches/exceeds bgLimit, wait on runCtx.Done() rather
	// than a competing waitTimer so runCtx expires naturally with
	// DeadlineExceeded (recording the ceiling timeout in each client's breaker)
	// instead of racing bgCancel() into context.Canceled.
	waitOnRunCtx := hasWaitTimeout && opts.WaitTimeout >= bgLimit
	var waitTimer *time.Timer
	var waitCh <-chan time.Time
	var ceilingCh <-chan struct{}
	if waitOnRunCtx {
		ceilingCh = runCtx.Done()
	} else if hasWaitTimeout {
		waitTimer = time.NewTimer(opts.WaitTimeout)
		defer waitTimer.Stop()
		waitCh = waitTimer.C
	}

	var settleTimer *time.Timer
	var settleCh <-chan time.Time
	defer func() {
		if settleTimer != nil {
			settleTimer.Stop()
		}
	}()
	settleStarted := false

	earlyCut := false
	timedOutWait := false
	parentDone := false

waitLoop:
	for {
		stateMu.Lock()
		allDone := doneCount == active
		uniqueCount := len(seenKeys)
		stateMu.Unlock()

		if allDone {
			if waitOnRunCtx && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				timedOutWait = true
			}
			break
		}

		if opts.MinResults > 0 && uniqueCount >= opts.MinResults {
			if !opts.FirstResults && opts.SettleSet && settle > 0 {
				if !settleStarted {
					settleStarted = true
					settleTimer = time.NewTimer(settle)
					settleCh = settleTimer.C
				}
			} else {
				earlyCut = true
				break
			}
		}

		if opts.FirstResults && uniqueCount > 0 {
			if settle <= 0 {
				earlyCut = true
				break
			}
			if !settleStarted {
				settleStarted = true
				settleTimer = time.NewTimer(settle)
				settleCh = settleTimer.C
			}
		}

		select {
		case <-notify:
		case <-settleCh:
			earlyCut = true
			break waitLoop
		case <-waitCh:
			timedOutWait = true
			break waitLoop
		case <-ceilingCh:
			timedOutWait = true
			break waitLoop
		case <-ctx.Done():
			parentDone = true
			break waitLoop
		}
	}

	if parentDone {
		parentTimedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
		var merged []core.Release
		var statuses []core.IndexerSearchStatus
		if parentTimedOut {
			// Snapshot unfinished indexers as timed_out before bgCancel turns
			// their error into context.Canceled.
			stateMu.Lock()
			merged, statuses, _, _ = snapshotSearchSlots(slots, searchStart, core.IndexerStatusTimedOut)
			stateMu.Unlock()
		}
		if bgCancel != nil {
			bgCancel()
		}
		wg.Wait()
		finishBG()
		m.recordSlotsAfterWait(ctx, slots, parentTimedOut)
		if !parentTimedOut {
			merged, statuses, _, _ = snapshotSearchSlots(slots, searchStart, core.IndexerStatusTimedOut)
		}
		if len(merged) == 0 {
			return SearchResult{Indexers: statuses}, ctx.Err()
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return SearchResult{Indexers: statuses}, ctx.Err()
		}
		return SearchResult{Releases: merged, Indexers: statuses}, nil
	}

	stateMu.Lock()
	remaining := active - doneCount
	if remaining == 0 {
		merged, statuses, _, errs := snapshotSearchSlots(slots, searchStart, core.IndexerStatusTimedOut)
		stateMu.Unlock()
		wg.Wait()
		finishBG()
		m.recordSlotsAfterWait(ctx, slots, false)
		if len(merged) == 0 && len(errs) > 0 && !earlyCut && !timedOutWait {
			return SearchResult{Indexers: statuses}, aggregateSearchErrors(errs)
		}
		return SearchResult{Releases: merged, Indexers: statuses}, nil
	}
	stateMu.Unlock()

	// Early return (firstResults / minResults) or a WaitTimeout shorter than
	// the background ceiling leaves slow indexers running in the background
	// (within our detached cap) so their results populate Ether's search cache.
	// Those indexers are reported as pending (and listed in top-level pending).
	// When WaitTimeout reaches/exceeds the background ceiling (or the detached
	// cap is full), unfinished indexers are cancelled and reported as timed_out.
	hitCeiling := timedOutWait && opts.WaitTimeout >= bgLimit
	canDetach := detachOnCutoff && !hitCeiling
	reserved := 0
	if canDetach {
		m.bgMu.Lock()
		if !m.bgClosed && m.detachedCount+remaining <= maxDetachedSearchFills {
			m.detachedCount += remaining
			reserved = remaining
		}
		m.bgMu.Unlock()
	}

	if reserved > 0 {
		snapshot := func(context.Context) SearchResult {
			stateMu.Lock()
			defer stateMu.Unlock()
			merged, statuses, pending, _ := snapshotSearchSlots(slots, searchStart, core.IndexerStatusPending)
			return SearchResult{Releases: merged, Indexers: statuses, Pending: pending}
		}
		result := snapshot(ctx)
		if opts.ContinueSearch != nil && len(result.Pending) > 0 {
			opts.ContinueSearch(snapshot)
		}
		go func(rem int) {
			defer func() {
				m.bgMu.Lock()
				m.detachedCount -= rem
				m.bgMu.Unlock()
				finishBG()
			}()
			wg.Wait()
			m.recordSlotsAfterWait(runCtx, slots, false)
		}(reserved)
		return result, nil
	}

	// Snapshot unfinished indexers as timed_out before cancelling them so they
	// are not reclassified as error:"canceled".
	stateMu.Lock()
	merged, statuses, _, _ := snapshotSearchSlots(slots, searchStart, core.IndexerStatusTimedOut)
	stateMu.Unlock()
	if bgCancel != nil {
		bgCancel()
	}
	wg.Wait()
	finishBG()
	m.recordSlotsAfterWait(ctx, slots, hitCeiling)
	return SearchResult{Releases: merged, Indexers: statuses}, nil
}

func (m *Manager) searchClientCoalesced(ctx context.Context, cl *Client, req core.SearchRequest) ([]core.Release, error) {
	return m.searchClientCoalescedWith(ctx, cl, req, nil)
}

func (m *Manager) searchClientCoalescedWith(ctx context.Context, cl *Client, req core.SearchRequest, arrived func([]core.Release)) ([]core.Release, error) {
	if cl.ttl() <= 0 {
		return cl.SearchWith(ctx, req, arrived)
	}
	speculative := backgroundSearch(ctx)
	key := cl.indexerID() + "|" + cacheKey(cl, req)
	if speculative {
		key += "|bg"
	} else {
		key += "|fg"
	}
	for {
		m.flightMu.Lock()
		if f, ok := m.flights[key]; ok {
			m.flightMu.Unlock()
			if err := cl.searchPaused(ctx); err != nil {
				return nil, err
			}
			select {
			case <-f.done:
				if errors.Is(f.err, context.Canceled) && ctx.Err() == nil {
					continue
				}
				out := append([]core.Release(nil), f.results...)
				if f.err == nil && arrived != nil && len(out) > 0 {
					arrived(out)
				}
				return out, f.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		f := &searchFlight{done: make(chan struct{})}
		if m.flights == nil {
			m.flights = map[string]*searchFlight{}
		}
		m.flights[key] = f
		m.flightMu.Unlock()

		res, err := func() (res []core.Release, err error) {
			defer func() {
				f.results = append([]core.Release(nil), res...)
				f.err = err
				m.flightMu.Lock()
				if m.flights[key] == f {
					delete(m.flights, key)
				}
				m.flightMu.Unlock()
				close(f.done)
			}()
			return cl.SearchWith(ctx, req, arrived)
		}()
		return res, err
	}
}

func snapshotSearchSlots(slots []searchSlot, searchStart time.Time, unfinishedStatus string) ([]core.Release, []core.IndexerSearchStatus, []core.IndexerRef, []error) {
	now := time.Now()
	seen := map[string]int{}
	var merged []core.Release
	var errs []error
	var pending []core.IndexerRef
	statuses := make([]core.IndexerSearchStatus, len(slots))

	for i, s := range slots {
		st := core.IndexerSearchStatus{
			IndexerID:      s.id,
			Indexer:        s.name,
			ConfiguredName: s.configuredName,
		}
		switch {
		case s.skipped:
			st.Status = core.IndexerStatusSkipped
			st.MS = 0
		case !s.finished:
			st.Status = unfinishedStatus
			ref := s.started
			if ref.IsZero() {
				ref = searchStart
			}
			st.MS = max(int64(0), now.Sub(ref).Milliseconds())
			if unfinishedStatus == core.IndexerStatusPending {
				pending = append(pending, core.IndexerRef{
					IndexerID: s.id,
					Indexer:   s.name,
				})
			}
		case s.err != nil && errors.Is(s.err, ErrIndexerPaused):
			st.Status = core.IndexerStatusSkipped
			st.Reason = core.IndexerSkipReasonPaused
			st.MS = 0
		case s.err != nil:
			errs = append(errs, s.err)
			st.MS = max(int64(0), s.duration.Milliseconds())
			if errors.Is(s.err, context.DeadlineExceeded) {
				st.Status = core.IndexerStatusTimedOut
			} else {
				st.Status = core.IndexerStatusError
				st.Error = classifyIndexerError(s.err)
			}
		default:
			st.Status = core.IndexerStatusAnswered
			st.Results = len(s.results)
			st.MS = max(int64(0), s.duration.Milliseconds())
			for _, r := range s.results {
				key := mergeDedupeKey(r)
				if idx, ok := seen[key]; ok {
					dup := r
					dup.Fallbacks = nil
					merged[idx].Fallbacks = append(merged[idx].Fallbacks, dup)
					continue
				}
				seen[key] = len(merged)
				merged = append(merged, r)
			}
		}
		statuses[i] = st
	}
	return merged, statuses, pending, errs
}

func aggregateSearchErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	allBudget := true
	for _, e := range errs {
		if !errors.Is(e, core.ErrBudgetExhausted) {
			allBudget = false
			break
		}
	}
	if allBudget {
		return fmt.Errorf("all indexers: %w", core.ErrBudgetExhausted)
	}
	return errs[0]
}

func (m *Manager) recordSlotsAfterWait(ctx context.Context, slots []searchSlot, deadlineHit bool) {
	timings := make([]SearchTiming, 0, len(slots))
	purpose := usagePurpose(ctx)
	day := time.Now().UTC().Format("2006-01-02")
	for _, s := range slots {
		if missingAnswer(s.err) {
			core.NoteSearchIncomplete(ctx)
		}
		if s.skipped {
			continue
		}
		e := ""
		if s.err != nil {
			e = s.err.Error()
		}
		timings = append(timings, SearchTiming{Indexer: s.name, Duration: s.duration, Err: e})
		shareKey := s.name + "|" + purpose
		if s.err != nil {
			if errors.Is(s.err, errSpeculativeShare) {
				if m.shouldLogSpeculativeShare(shareKey, day) {
					m.log.Debug("indexer search failed", "indexer", s.name, "err", s.err)
				}
			} else {
				m.log.Debug("indexer search failed", "indexer", s.name, "err", s.err)
			}
		} else {
			m.clearSpeculativeShareLog(shareKey)
		}
	}
	m.searchTimingMu.Lock()
	m.searchTimings = timings
	for _, s := range slots {
		if s.skipped {
			continue
		}
		err := s.err
		if deadlineHit && errors.Is(err, context.Canceled) {
			err = context.DeadlineExceeded
		}
		if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, ErrIndexerPaused)) {
			continue
		}
		errCode := ""
		if err != nil {
			errCode = classifyIndexerError(err)
		}
		m.recordIndexerTimingLocked(s.id, max(int64(0), s.duration.Milliseconds()), errCode)
	}
	m.searchTimingMu.Unlock()
}

// classifyIndexerError maps an indexer error to a short, credential-free code
// safe to surface to plugins.
func classifyIndexerError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, core.ErrBudgetExhausted):
		return "budget_exhausted"
	case errors.Is(err, ErrIndexerPaused):
		return "paused"
	case errors.Is(err, ErrMissingDefinition):
		return "missing_definition"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case strings.Contains(err.Error(), "does not support category searches"):
		return "category_unsupported"
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Budget:
			return "budget_exhausted"
		case ae.Code == http.StatusUnauthorized || ae.Code == http.StatusForbidden || ae.Code == 100 || ae.Code == 101 || ae.Code == 102:
			return "auth_failed"
		case ae.Code == http.StatusTooManyRequests:
			return "rate_limited"
		case ae.Code >= 500 && ae.Code < 600:
			return "upstream_error"
		case ae.Code > 0:
			return fmt.Sprintf("api_error_%d", ae.Code)
		default:
			return "api_error"
		}
	}
	return "search_failed"
}
