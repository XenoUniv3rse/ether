package adapters

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/resolver"
)

const (
	defaultStreamMaxPerPlugin = 4
	defaultStreamMaxGlobal    = 64
	defaultStreamIdleTTL      = 3 * time.Minute
	defaultStreamFirstPollTTL = 15 * time.Second
	defaultStreamMaxWait      = 10 * time.Second
)

type streamSearchSession struct {
	id        string
	pluginID  string
	query     string
	profile   string
	startedAt time.Time

	cancel context.CancelFunc

	mu            sync.Mutex
	polled        bool
	lastPollAt    time.Time
	expiresAt     time.Time
	timer         *time.Timer
	timerGen      uint64
	seenKeys      map[string]struct{}
	delivered     int
	buffered      []core.Release
	statuses      []core.IndexerSearchStatus
	statusByID    map[string]int
	statusUpdated []bool
	done          bool
	cancelled     bool
	wakeCh        chan struct{}
}

func (s *streamSearchSession) broadcastLocked() {
	if s.wakeCh != nil {
		close(s.wakeCh)
		s.wakeCh = make(chan struct{})
	}
}

func (s *streamSearchSession) stopTimerLocked() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.timerGen++
}

func (s *streamSearchSession) armTimerLocked(d time.Duration, onExpire func(gen uint64)) {
	s.stopTimerLocked()
	if d <= 0 {
		return
	}
	gen := s.timerGen
	s.timer = time.AfterFunc(d, func() { onExpire(gen) })
}

func (s *streamSearchSession) snapshotStatusesLocked(now time.Time) ([]core.IndexerSearchStatus, []core.IndexerRef) {
	statuses := make([]core.IndexerSearchStatus, len(s.statuses))
	pending := make([]core.IndexerRef, 0)
	elapsed := max(int64(0), now.Sub(s.startedAt).Milliseconds())
	for i, st := range s.statuses {
		if st.Status == core.IndexerStatusPending {
			st.MS = elapsed
			pending = append(pending, core.IndexerRef{
				IndexerID: st.IndexerID,
				Indexer:   st.Indexer,
			})
		}
		statuses[i] = st
	}
	return statuses, pending
}

func (s *streamSearchSession) abort(now time.Time) []core.IndexerSearchStatus {
	s.mu.Lock()
	s.stopTimerLocked()
	s.cancelled = true
	s.done = true
	elapsed := max(int64(0), now.Sub(s.startedAt).Milliseconds())
	for i := range s.statuses {
		if s.statuses[i].Status == core.IndexerStatusPending {
			s.statuses[i].Status = core.IndexerStatusError
			s.statuses[i].Error = "canceled"
			s.statuses[i].MS = elapsed
			s.statusUpdated[i] = true
		}
	}
	statuses := make([]core.IndexerSearchStatus, len(s.statuses))
	copy(statuses, s.statuses)
	s.buffered = nil
	s.broadcastLocked()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return statuses
}

func (r *KindResolver) streamMaxPerPluginLimit() int {
	if r.streamMaxPerPlugin > 0 {
		return r.streamMaxPerPlugin
	}
	return defaultStreamMaxPerPlugin
}

func (r *KindResolver) streamMaxGlobalLimit() int {
	if r.streamMaxGlobal > 0 {
		return r.streamMaxGlobal
	}
	return defaultStreamMaxGlobal
}

func (r *KindResolver) streamIdleTTLDuration() time.Duration {
	if r.streamIdleTTL > 0 {
		return r.streamIdleTTL
	}
	return defaultStreamIdleTTL
}

func (r *KindResolver) streamFirstPollTTLDuration() time.Duration {
	if r.streamFirstPollTTL > 0 {
		return r.streamFirstPollTTL
	}
	return defaultStreamFirstPollTTL
}

func (r *KindResolver) streamMaxWaitDuration() time.Duration {
	if r.streamMaxWait > 0 {
		return r.streamMaxWait
	}
	return defaultStreamMaxWait
}

func (r *KindResolver) streamCeilingDuration() time.Duration {
	if r.streamCeiling > 0 {
		return r.streamCeiling
	}
	return sourceIndexerSearchTimeout
}

// Close cancels and drains source resolutions and streaming searches.
func (r *KindResolver) Close() {
	if r == nil {
		return
	}
	r.flightMu.Lock()
	r.flightClosed = true
	for _, flight := range r.flights {
		if flight.cancel != nil {
			flight.cancel()
		}
	}
	clear(r.flights)
	r.flightMu.Unlock()

	now := r.now()
	r.streamMu.Lock()
	r.streamClosed = true
	sessions := make([]*streamSearchSession, 0, len(r.streamSearches))
	for id, sess := range r.streamSearches {
		sessions = append(sessions, sess)
		delete(r.streamSearches, id)
	}
	r.streamMu.Unlock()
	for _, sess := range sessions {
		sess.abort(now)
	}
	r.streamWG.Wait()
	r.closeSourceFlights()
	r.flightWG.Wait()
}

func (r *KindResolver) cancelPluginStreamingSearches(pluginID string) {
	if r == nil || pluginID == "" {
		return
	}
	now := r.now()
	r.streamMu.Lock()
	var victim []*streamSearchSession
	for id, sess := range r.streamSearches {
		if sess.pluginID == pluginID {
			victim = append(victim, sess)
			delete(r.streamSearches, id)
		}
	}
	r.streamMu.Unlock()
	for _, sess := range victim {
		sess.abort(now)
	}
}

func (r *KindResolver) sweepStreamingSearchesLocked(now time.Time) []*streamSearchSession {
	var expired []*streamSearchSession
	for id, sess := range r.streamSearches {
		sess.mu.Lock()
		isExpired := !sess.expiresAt.IsZero() && !now.Before(sess.expiresAt)
		sess.mu.Unlock()
		if isExpired {
			expired = append(expired, sess)
			delete(r.streamSearches, id)
		}
	}
	return expired
}

func (r *KindResolver) expireStreamingSearch(id string, gen uint64) {
	now := r.now()
	r.streamMu.Lock()
	sess := r.streamSearches[id]
	if sess == nil {
		r.streamMu.Unlock()
		return
	}
	sess.mu.Lock()
	stale := sess.timerGen != gen || now.Before(sess.expiresAt)
	sess.mu.Unlock()
	if stale {
		r.streamMu.Unlock()
		return
	}
	delete(r.streamSearches, id)
	r.streamMu.Unlock()
	sess.abort(now)
}

func newStreamSearchID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "srch_" + hex.EncodeToString(b[:]), nil
}

func streamReleaseDedupeKey(r core.Release) string {
	key := strings.ToLower(strings.TrimSpace(r.Name)) + "|" + fmt.Sprint(r.Size)
	if r.Size == 0 {
		key = strings.ToLower(strings.TrimSpace(r.Name))
	}
	if r.Protocol != core.ProtocolUsenet {
		key = r.Protocol + "|" + key
		if r.InfoHash != "" {
			key = r.Protocol + "|" + r.InfoHash
		}
	}
	return key
}

func normalizeFilterSet(list []string) map[string]bool {
	var set map[string]bool
	for _, raw := range list {
		if k := strings.ToLower(strings.TrimSpace(raw)); k != "" {
			if set == nil {
				set = map[string]bool{}
			}
			set[k] = true
		}
	}
	return set
}

func filterMatchesIndexer(set map[string]bool, id, name string) bool {
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

func boolPtr(v bool) *bool { return &v }

func (r *KindResolver) validateIndexerSearchQuery(ctx context.Context, q core.SourceQuery) (string, string, error) {
	query := strings.TrimSpace(q.Query)
	category := strings.TrimSpace(q.Category)
	if query == "" && category == "" {
		return "", "", &plugins.ValidationError{Fields: []plugins.FieldError{{
			Field:   "query",
			Message: "indexer_search needs a query or category",
		}}}
	}
	if _, err := core.SearchCategories(category); err != nil {
		return "", "", &plugins.ValidationError{Fields: []plugins.FieldError{{
			Field:   "category",
			Message: err.Error(),
		}}}
	}
	if fe := plugins.ValidateSourceLookup(q); fe != nil {
		return "", "", &plugins.ValidationError{Fields: []plugins.FieldError{*fe}}
	}
	if q.SettleMS != nil && *q.SettleMS < 0 {
		return "", "", &plugins.ValidationError{Fields: []plugins.FieldError{{
			Field:   "settleMs",
			Message: "settleMs must be non-negative",
		}}}
	}
	if q.MinResults < 0 {
		return "", "", &plugins.ValidationError{Fields: []plugins.FieldError{{
			Field:   "minResults",
			Message: "minResults must be non-negative",
		}}}
	}
	if q.TimeoutMS < 0 {
		return "", "", &plugins.ValidationError{Fields: []plugins.FieldError{{
			Field:   "timeoutMs",
			Message: "timeoutMs must be non-negative",
		}}}
	}
	if len(q.SkipIndexers) > plugins.MaxSourceIndexerFilters {
		return "", "", &plugins.ValidationError{Fields: []plugins.FieldError{{
			Field:   "skipIndexers",
			Message: fmt.Sprintf("at most %d skipIndexers entries allowed", plugins.MaxSourceIndexerFilters),
		}}}
	}
	if len(q.OnlyIndexers) > plugins.MaxSourceIndexerFilters {
		return "", "", &plugins.ValidationError{Fields: []plugins.FieldError{{
			Field:   "onlyIndexers",
			Message: fmt.Sprintf("at most %d onlyIndexers entries allowed", plugins.MaxSourceIndexerFilters),
		}}}
	}
	if _, err := resolver.ScoreSourceCandidates(ctx, r.resolver, q.Profile, nil); err != nil {
		if errors.Is(err, resolver.ErrUnknownProfile) {
			return "", "", &plugins.ValidationError{Fields: []plugins.FieldError{{
				Field:   "profile",
				Message: err.Error(),
			}}}
		}
		return "", "", err
	}
	return query, category, nil
}

func (r *KindResolver) scoreIndexerReleases(ctx context.Context, query, profile string, rels []core.Release) ([]core.SourceCandidate, error) {
	if len(rels) == 0 {
		return []core.SourceCandidate{}, nil
	}
	scored, err := resolver.ScoreSourceCandidates(ctx, r.resolver, profile, rels)
	if err != nil {
		return nil, err
	}
	if scored == nil {
		scored = []core.SourceCandidate{}
	}
	var nzbRefs, torrentRefs []string
	if r.pluginNZBs != nil {
		nzbRefs, torrentRefs = r.pluginNZBs.PluginReleaseReferences(ctx, rels)
	}
	knownLookup, hasKnownLookup := r.debrid.(debridKnownCachedByBackend)
	for i := range scored {
		if r.pluginNZBs != nil {
			scored[i].NZBRef, scored[i].TorrentRef = nzbRefs[i], torrentRefs[i]
			scored[i] = r.publicReleaseLinks(scored[i])
		} else {
			scored[i] = r.stripCandidateAuth(scored[i])
		}
		if plugin, kind, ref, ok := directStream(rels[i].GUID); ok && kind == "lazy" {
			// A plugin source's category answer: no link of its own, the
			// plugin lists and opens it (#792).
			scored[i].PluginRelease = &core.PluginReleaseRef{Plugin: plugin, ID: ref}
		}
		if fc := rels[i].KnownFileCount(); fc > scored[i].FileCount {
			scored[i].FileCount = fc
		}
		if hasKnownLookup && (scored[i].Cached || scored[i].InfoHash != "") {
			if refs := knownLookup.KnownCachedBy(scored[i].InfoHash, rels[i].DebridCached); len(refs) > 0 {
				scored[i].Cached = true
				scored[i].CachedBy = refs
			}
		}
	}
	if query != "" && r.isUsenetBackingOff() && len(scored) > 1 {
		nonUsenet := make([]core.SourceCandidate, 0, len(scored))
		usenetOnly := make([]core.SourceCandidate, 0, len(scored))
		for _, c := range scored {
			if (c.NZBURL != "" || c.NZBRef != "") && c.InfoHash == "" && c.Magnet == "" && c.TorrentURL == "" && c.TorrentRef == "" {
				usenetOnly = append(usenetOnly, c)
			} else {
				nonUsenet = append(nonUsenet, c)
			}
		}
		if len(nonUsenet) > 0 && len(usenetOnly) > 0 {
			scored = append(nonUsenet, usenetOnly...)
		}
	}
	return scored, nil
}

func (r *KindResolver) startStreamingIndexerSearch(ctx context.Context, pluginID string, q core.SourceQuery) (core.SourceResolveResult, error) {
	query, category, err := r.validateIndexerSearchQuery(ctx, q)
	if err != nil {
		return core.SourceResolveResult{}, err
	}

	now := r.now()
	searchID, err := newStreamSearchID()
	if err != nil {
		return core.SourceResolveResult{}, err
	}

	// Build initial enabled indexer status list and skipIDs map.
	skipSet := normalizeFilterSet(q.SkipIndexers)
	onlySet := normalizeFilterSet(q.OnlyIndexers)
	skipIDs := map[string]bool{}
	var initialStatuses []core.IndexerSearchStatus
	active := 0
	paused := r.speculativeSourceBudgetPaused(ctx)
	if r.indexer != nil {
		if dir, ok := r.indexer.(core.IndexerDirectory); ok {
			base := dir.EnabledIndexerStatuses(core.IndexerStatusPending)
			initialStatuses = make([]core.IndexerSearchStatus, 0, len(base))
			for _, st := range base {
				skipped := paused ||
					core.IndexerSkipped(ctx, st.IndexerID) ||
					filterMatchesIndexer(skipSet, st.IndexerID, st.Indexer) ||
					(len(onlySet) > 0 && !filterMatchesIndexer(onlySet, st.IndexerID, st.Indexer))
				if skipped {
					st.Status = core.IndexerStatusSkipped
					st.MS = 0
					skipIDs[st.IndexerID] = true
				} else {
					st.Status = core.IndexerStatusPending
					st.MS = 0
					active++
				}
				initialStatuses = append(initialStatuses, st)
			}
		} else {
			name := r.indexer.Name()
			if paused || singleIndexerSkipped(name, q.SkipIndexers, q.OnlyIndexers) {
				initialStatuses = []core.IndexerSearchStatus{{
					IndexerID: name,
					Indexer:   name,
					Status:    core.IndexerStatusSkipped,
					MS:        0,
				}}
				skipIDs[name] = true
			} else {
				initialStatuses = []core.IndexerSearchStatus{{
					IndexerID: name,
					Indexer:   name,
					Status:    core.IndexerStatusPending,
					MS:        0,
				}}
				active = 1
			}
		}
	} else {
		initialStatuses = []core.IndexerSearchStatus{}
	}

	statusByID := make(map[string]int, len(initialStatuses))
	statusUpdated := make([]bool, len(initialStatuses))
	for i, st := range initialStatuses {
		statusByID[st.IndexerID] = i
		if st.Status == core.IndexerStatusSkipped {
			statusUpdated[i] = true
		}
	}

	firstPollTTL := r.streamFirstPollTTLDuration()
	sess := &streamSearchSession{
		id:            searchID,
		pluginID:      pluginID,
		query:         query,
		profile:       q.Profile,
		startedAt:     now,
		expiresAt:     now.Add(firstPollTTL),
		seenKeys:      make(map[string]struct{}),
		statuses:      initialStatuses,
		statusByID:    statusByID,
		statusUpdated: statusUpdated,
		done:          active == 0,
		wakeCh:        make(chan struct{}),
	}

	// Enforce per-plugin and global live search caps after sweeping expired sessions.
	r.streamMu.Lock()
	if r.streamClosed {
		r.streamMu.Unlock()
		return core.SourceResolveResult{}, context.Canceled
	}
	expired := r.sweepStreamingSearchesLocked(now)
	if r.streamSearches == nil {
		r.streamSearches = make(map[string]*streamSearchSession)
	}
	pluginCount := 0
	for _, existing := range r.streamSearches {
		if existing.pluginID == pluginID {
			pluginCount++
		}
	}
	maxPerPlugin := r.streamMaxPerPluginLimit()
	maxGlobal := r.streamMaxGlobalLimit()
	if pluginCount >= maxPerPlugin || len(r.streamSearches) >= maxGlobal {
		r.streamMu.Unlock()
		for _, exp := range expired {
			exp.abort(now)
		}
		return core.SourceResolveResult{}, fmt.Errorf("%w: at most %d concurrent streaming indexer searches per plugin", plugins.ErrRateLimited, maxPerPlugin)
	}

	var runCtx context.Context
	var runCancel context.CancelFunc
	if active > 0 {
		ceiling := r.streamCeilingDuration()
		if q.TimeoutMS > 0 {
			if reqLimit := time.Duration(q.TimeoutMS) * time.Millisecond; reqLimit < ceiling {
				ceiling = reqLimit
			}
		}
		runCtx, runCancel = context.WithTimeout(context.WithoutCancel(ctx), ceiling)
		sess.cancel = runCancel
		r.streamWG.Add(1)
	}

	sess.mu.Lock()
	sess.armTimerLocked(firstPollTTL, func(gen uint64) {
		r.expireStreamingSearch(searchID, gen)
	})
	statusesSnap, pendingSnap := sess.snapshotStatusesLocked(now)
	doneSnap := sess.done
	sess.mu.Unlock()

	r.streamSearches[searchID] = sess
	r.streamMu.Unlock()

	for _, exp := range expired {
		exp.abort(now)
	}

	if active > 0 {
		searchReq := core.SearchRequest{
			Title:    query,
			Category: category,
			Lookup:   q.Lookup,
		}
		onArrived := func(batch []core.Release) {
			if len(batch) == 0 {
				return
			}
			sess.mu.Lock()
			if sess.cancelled {
				sess.mu.Unlock()
				return
			}
			added := 0
			for _, rel := range batch {
				key := streamReleaseDedupeKey(rel)
				if _, dup := sess.seenKeys[key]; dup {
					continue
				}
				sess.seenKeys[key] = struct{}{}
				sess.buffered = append(sess.buffered, rel)
				added++
			}
			if added > 0 {
				sess.broadcastLocked()
			}
			sess.mu.Unlock()
		}
		onStatus := func(st core.IndexerSearchStatus) {
			sess.mu.Lock()
			if sess.cancelled {
				sess.mu.Unlock()
				return
			}
			if idx, ok := sess.statusByID[st.IndexerID]; ok {
				sess.statuses[idx] = st
				sess.statusUpdated[idx] = true
			} else {
				sess.statusByID[st.IndexerID] = len(sess.statuses)
				sess.statuses = append(sess.statuses, st)
				sess.statusUpdated = append(sess.statusUpdated, true)
			}
			sess.mu.Unlock()
		}

		fanCtx := core.WithIndexerStatusObserver(core.WithIndexerSkip(runCtx, skipIDs), onStatus)
		go func() {
			defer func() {
				runCancel()
				r.streamWG.Done()
			}()
			var (
				rels []core.Release
				err  error
			)
			if streaming, ok := r.indexer.(core.StreamingIndexer); ok {
				rels, err = streaming.SearchWith(fanCtx, searchReq, onArrived)
			} else {
				rels, err = r.indexer.Search(fanCtx, searchReq)
				if err == nil && len(rels) > 0 {
					onArrived(rels)
				}
			}
			if err != nil && errors.Is(err, core.ErrBudgetExhausted) {
				r.noteSpeculativeSourceBudgetExhausted(runCtx)
			}
			elapsed := max(int64(0), r.now().Sub(sess.startedAt).Milliseconds())
			sess.mu.Lock()
			if !sess.cancelled {
				for i := range sess.statuses {
					if sess.statusUpdated[i] {
						continue
					}
					sess.statusUpdated[i] = true
					sess.statuses[i].MS = elapsed
					switch {
					case err == nil:
						sess.statuses[i].Status = core.IndexerStatusAnswered
						if len(sess.statuses) == 1 {
							sess.statuses[i].Results = len(rels)
						}
					case errors.Is(err, context.DeadlineExceeded) || errors.Is(runCtx.Err(), context.DeadlineExceeded):
						sess.statuses[i].Status = core.IndexerStatusTimedOut
					case errors.Is(err, context.Canceled):
						sess.statuses[i].Status = core.IndexerStatusError
						sess.statuses[i].Error = "canceled"
					default:
						sess.statuses[i].Status = core.IndexerStatusError
						sess.statuses[i].Error = "search_failed"
					}
				}
			}
			sess.done = true
			sess.broadcastLocked()
			sess.mu.Unlock()
		}()
	}

	return core.SourceResolveResult{
		Candidates: []core.SourceCandidate{},
		Indexers:   statusesSnap,
		Pending:    pendingSnap,
		SearchID:   searchID,
		Done:       boolPtr(doneSnap),
	}, nil
}

func (r *KindResolver) pollStreamingIndexerSearch(ctx context.Context, pluginID string, q core.SourceQuery) (core.SourceResolveResult, error) {
	searchID := strings.TrimSpace(q.SearchID)
	if searchID == "" {
		return core.SourceResolveResult{}, &plugins.ValidationError{Fields: []plugins.FieldError{{
			Field:   "searchId",
			Message: "indexer_search_results needs a searchId",
		}}}
	}
	if q.WaitMS < 0 {
		return core.SourceResolveResult{}, &plugins.ValidationError{Fields: []plugins.FieldError{{
			Field:   "waitMs",
			Message: "waitMs must be non-negative",
		}}}
	}

	now := r.now()
	r.streamMu.Lock()
	expired := r.sweepStreamingSearchesLocked(now)
	sess := r.streamSearches[searchID]
	if sess != nil && sess.pluginID != pluginID {
		sess = nil
	}
	r.streamMu.Unlock()
	for _, exp := range expired {
		exp.abort(now)
	}
	if sess == nil {
		return core.SourceResolveResult{}, fmt.Errorf("%w: streaming search %q not found", plugins.ErrNotFound, searchID)
	}

	wait := time.Duration(q.WaitMS) * time.Millisecond
	maxWait := r.streamMaxWaitDuration()
	if wait > maxWait {
		wait = maxWait
	}
	if wait > 0 {
		if budget := defaultIndexerWaitBudget(ctx, now, maxWait); budget > 0 && wait > budget {
			wait = budget
		}
	}
	idleTTL := r.streamIdleTTLDuration()

	// Record this poll and refresh idle expiry.
	sess.mu.Lock()
	sess.polled = true
	sess.lastPollAt = now
	sess.expiresAt = now.Add(idleTTL)
	sess.armTimerLocked(idleTTL, func(gen uint64) {
		r.expireStreamingSearch(searchID, gen)
	})

	var waitTimer *time.Timer
	var waitCh <-chan time.Time
	defer func() {
		if waitTimer != nil {
			waitTimer.Stop()
		}
	}()

	for len(sess.buffered) == 0 && !sess.done && !sess.cancelled && wait > 0 {
		if waitTimer == nil {
			waitTimer = time.NewTimer(wait)
			waitCh = waitTimer.C
		}
		wake := sess.wakeCh
		sess.mu.Unlock()
		select {
		case <-wake:
			sess.mu.Lock()
		case <-waitCh:
			sess.mu.Lock()
			wait = 0
		case <-ctx.Done():
			return core.SourceResolveResult{}, ctx.Err()
		}
	}

	raw := sess.buffered
	sess.buffered = nil
	done := sess.done
	cancelled := sess.cancelled
	pollNow := r.now()
	if !done && !cancelled {
		sess.lastPollAt = pollNow
		sess.expiresAt = pollNow.Add(idleTTL)
		sess.armTimerLocked(idleTTL, func(gen uint64) {
			r.expireStreamingSearch(searchID, gen)
		})
	}
	statusesSnap, pendingSnap := sess.snapshotStatusesLocked(pollNow)
	query := sess.query
	profile := sess.profile
	if query == "" && len(raw) > 0 {
		sort.SliceStable(raw, func(i, j int) bool {
			pi, pj := raw[i].PubDate, raw[j].PubDate
			if pi.IsZero() != pj.IsZero() {
				return !pi.IsZero()
			}
			return pi.After(pj)
		})
		rem := maxRecentSourceCandidates - sess.delivered
		if rem <= 0 {
			raw = nil
		} else if len(raw) > rem {
			raw = raw[:rem]
		}
	}
	sess.delivered += len(raw)
	shouldRemove := done && len(sess.buffered) == 0
	if shouldRemove {
		sess.stopTimerLocked()
	}
	sess.mu.Unlock()

	if shouldRemove {
		r.streamMu.Lock()
		if r.streamSearches[searchID] == sess {
			delete(r.streamSearches, searchID)
		}
		r.streamMu.Unlock()
	}

	scored, err := r.scoreIndexerReleases(ctx, query, profile, raw)
	if err != nil {
		return core.SourceResolveResult{}, err
	}
	return core.SourceResolveResult{
		Candidates: scored,
		Indexers:   statusesSnap,
		Pending:    pendingSnap,
		SearchID:   searchID,
		Done:       boolPtr(done),
	}, nil
}

func (r *KindResolver) cancelStreamingIndexerSearch(_ context.Context, pluginID string, q core.SourceQuery) (core.SourceResolveResult, error) {
	searchID := strings.TrimSpace(q.SearchID)
	if searchID == "" {
		return core.SourceResolveResult{}, &plugins.ValidationError{Fields: []plugins.FieldError{{
			Field:   "searchId",
			Message: "indexer_search_cancel needs a searchId",
		}}}
	}

	now := r.now()
	r.streamMu.Lock()
	expired := r.sweepStreamingSearchesLocked(now)
	sess := r.streamSearches[searchID]
	if sess != nil && sess.pluginID != pluginID {
		sess = nil
	}
	if sess != nil {
		delete(r.streamSearches, searchID)
	}
	r.streamMu.Unlock()
	for _, exp := range expired {
		exp.abort(now)
	}
	if sess == nil {
		return core.SourceResolveResult{}, fmt.Errorf("%w: streaming search %q not found", plugins.ErrNotFound, searchID)
	}

	statuses := sess.abort(now)
	return core.SourceResolveResult{
		Candidates: []core.SourceCandidate{},
		Indexers:   statuses,
		Pending:    []core.IndexerRef{},
		SearchID:   searchID,
		Done:       boolPtr(true),
	}, nil
}

func (r *KindResolver) resolveIndexersList() core.SourceResolveResult {
	var list []core.IndexerSearchStatus
	if r.indexer != nil {
		if dir, ok := r.indexer.(core.IndexerDirectory); ok {
			list = dir.EnabledIndexers()
		}
	}
	if list == nil {
		list = []core.IndexerSearchStatus{}
	}
	return core.SourceResolveResult{
		Candidates: []core.SourceCandidate{},
		Indexers:   list,
	}
}
