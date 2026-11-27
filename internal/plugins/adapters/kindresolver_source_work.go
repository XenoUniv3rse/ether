package adapters

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const (
	maxSourceWorkFlights        = 256
	maxReleaseFilesCacheEntries = 256
	releaseFilesCacheTTL        = 15 * time.Minute
	maxSourceHandoffEntries     = 128
	sourceHandoffTTL            = 60 * time.Second

	resolvePrepareBudget       = 90 * time.Second
	resolveProgressIdleTTL     = 2 * time.Minute
	maxStalledResolveSlices    = 3
	maxResolveProgressEntries  = 512
	maxResolveProgressWorkKeys = 128
)

type sourceWorkFlight struct {
	done       chan struct{}
	cancel     context.CancelFunc
	abandoned  bool
	finished   bool
	ok         bool
	handoffRun bool
	res        any
	err        error
}

type cachedReleaseFiles struct {
	files     []core.ReleaseFile
	expiresAt time.Time
	lastUsed  time.Time
}

type sourceHandoffEntry struct {
	cands     []core.SourceCandidate
	expiresAt time.Time
}

type resolveProgressEntry struct {
	startedAt     time.Time
	lastAt        time.Time
	stalledSlices int
	seenWorkKeys  map[string]struct{}
}

// runDetachedSourceFlight executes fn on a context detached from caller
// cancellation (preserving context values such as background budgets and
// indexer purpose) and bounded by timeout. Concurrent callers with the same
// flightKey join the single in-flight probe. If the caller's context expires
// first (e.g. a 5 s plugin /resolve guest deadline), the probe finishes in the
// background so its cached result is ready for the next resolve slice.
//
// When onAbandonedDone is non-nil, it is invoked at most once if the probe
// succeeds after at least one waiter timed out, letting non-cached backends
// (such as torrent file listings) hand off their finished result to the next
// slice without caching normal synchronous calls.
func (r *KindResolver) runDetachedSourceFlight(
	ctx context.Context,
	flightKey string,
	timeout time.Duration,
	fn func(workCtx context.Context) (any, error),
	onAbandonedDone func(res any),
) (res any, joined bool, err error) {
	if r == nil {
		return nil, false, core.ErrNotAvailable
	}

	r.sourceFlightMu.Lock()
	if r.sourceFlightsClosed {
		r.sourceFlightMu.Unlock()
		return nil, false, context.Canceled
	}
	fl, exists := r.sourceFlights[flightKey]
	if !exists {
		if cerr := ctx.Err(); cerr != nil {
			r.sourceFlightMu.Unlock()
			return nil, false, cerr
		}
		if len(r.sourceFlights) >= maxSourceWorkFlights {
			r.sourceFlightMu.Unlock()
			wctx, cancel := withHardTimeout(ctx, timeout)
			defer cancel()
			out, ferr := fn(wctx)
			return out, false, ferr
		}
		workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		fl = &sourceWorkFlight{
			done:   make(chan struct{}),
			cancel: cancel,
		}
		if r.sourceFlights == nil {
			r.sourceFlights = make(map[string]*sourceWorkFlight)
		}
		r.sourceFlights[flightKey] = fl
		r.sourceFlightWG.Add(1)
		go func() {
			defer r.sourceFlightWG.Done()
			defer cancel()
			var (
				out  any
				ferr error
			)
			defer func() {
				if p := recover(); p != nil {
					ferr = fmt.Errorf("kindresolver: source probe panic: %v", p)
				}
				ok := ferr == nil && workCtx.Err() == nil
				r.sourceFlightMu.Lock()
				fl.res = out
				fl.err = ferr
				fl.finished = true
				fl.ok = ok
				if r.sourceFlights[flightKey] == fl {
					delete(r.sourceFlights, flightKey)
				}
				runHandoff := fl.abandoned && ok && !fl.handoffRun && onAbandonedDone != nil
				if runHandoff {
					fl.handoffRun = true
				}
				r.sourceFlightMu.Unlock()
				if runHandoff {
					onAbandonedDone(out)
				}
				close(fl.done)
			}()
			out, ferr = fn(workCtx)
		}()
	}
	r.sourceFlightMu.Unlock()

	select {
	case <-fl.done:
		return fl.res, exists, fl.err
	default:
	}

	select {
	case <-fl.done:
		return fl.res, exists, fl.err
	case <-ctx.Done():
		r.sourceFlightMu.Lock()
		fl.abandoned = true
		runHandoff := fl.finished && fl.ok && !fl.handoffRun && onAbandonedDone != nil
		var handoffRes any
		if runHandoff {
			fl.handoffRun = true
			handoffRes = fl.res
		}
		r.sourceFlightMu.Unlock()
		if runHandoff {
			onAbandonedDone(handoffRes)
		}
		return nil, exists, ctx.Err()
	}
}

func (r *KindResolver) closeSourceFlights() {
	if r == nil {
		return
	}
	r.sourceFlightMu.Lock()
	r.sourceFlightsClosed = true
	cancels := make([]context.CancelFunc, 0, len(r.sourceFlights))
	for key, fl := range r.sourceFlights {
		if fl.cancel != nil {
			cancels = append(cancels, fl.cancel)
		}
		delete(r.sourceFlights, key)
	}
	r.sourceFlightMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	r.sourceFlightWG.Wait()
}

func (r *KindResolver) lookupUsenetReleaseFiles(releaseKey string) ([]core.ReleaseFile, bool) {
	if r == nil || releaseKey == "" {
		return nil, false
	}
	now := r.now()
	r.releaseFilesMu.Lock()
	defer r.releaseFilesMu.Unlock()
	ent, ok := r.releaseFilesCache[releaseKey]
	if !ok || !ent.expiresAt.After(now) {
		if ok {
			delete(r.releaseFilesCache, releaseKey)
		}
		return nil, false
	}
	ent.lastUsed = now
	r.releaseFilesCache[releaseKey] = ent
	return cloneReleaseFiles(ent.files), true
}

func (r *KindResolver) storeUsenetReleaseFiles(releaseKey string, files []core.ReleaseFile) {
	if r == nil || releaseKey == "" || len(files) == 0 {
		return
	}
	now := r.now()
	r.releaseFilesMu.Lock()
	defer r.releaseFilesMu.Unlock()
	if r.releaseFilesCache == nil {
		r.releaseFilesCache = make(map[string]cachedReleaseFiles)
	}
	for k, ent := range r.releaseFilesCache {
		if !ent.expiresAt.After(now) {
			delete(r.releaseFilesCache, k)
		}
	}
	for len(r.releaseFilesCache) >= maxReleaseFilesCacheEntries {
		var oldestKey string
		var oldestAt time.Time
		for k, ent := range r.releaseFilesCache {
			if oldestAt.IsZero() || ent.lastUsed.Before(oldestAt) {
				oldestKey = k
				oldestAt = ent.lastUsed
			}
		}
		if oldestKey == "" {
			break
		}
		delete(r.releaseFilesCache, oldestKey)
	}
	r.releaseFilesCache[releaseKey] = cachedReleaseFiles{
		files:     cloneReleaseFiles(files),
		expiresAt: now.Add(releaseFilesCacheTTL),
		lastUsed:  now,
	}
}

func (r *KindResolver) takeSourceHandoff(key string) ([]core.SourceCandidate, bool) {
	if r == nil || key == "" {
		return nil, false
	}
	now := r.now()
	r.releaseFilesMu.Lock()
	defer r.releaseFilesMu.Unlock()
	ent, ok := r.sourceHandoffs[key]
	if !ok {
		return nil, false
	}
	delete(r.sourceHandoffs, key)
	if !ent.expiresAt.After(now) {
		return nil, false
	}
	return cloneSourceCandidates(ent.cands), true
}

func (r *KindResolver) storeSourceHandoff(key string, cands []core.SourceCandidate) {
	if r == nil || key == "" || len(cands) == 0 {
		return
	}
	now := r.now()
	r.releaseFilesMu.Lock()
	defer r.releaseFilesMu.Unlock()
	if r.sourceHandoffs == nil {
		r.sourceHandoffs = make(map[string]sourceHandoffEntry)
	}
	for k, ent := range r.sourceHandoffs {
		if !ent.expiresAt.After(now) {
			delete(r.sourceHandoffs, k)
		}
	}
	for len(r.sourceHandoffs) >= maxSourceHandoffEntries {
		var oldestKey string
		var oldestExp time.Time
		for k, ent := range r.sourceHandoffs {
			if oldestExp.IsZero() || ent.expiresAt.Before(oldestExp) {
				oldestKey = k
				oldestExp = ent.expiresAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(r.sourceHandoffs, oldestKey)
	}
	r.sourceHandoffs[key] = sourceHandoffEntry{
		cands:     cloneSourceCandidates(cands),
		expiresAt: now.Add(sourceHandoffTTL),
	}
}

func cloneReleaseFiles(in []core.ReleaseFile) []core.ReleaseFile {
	if len(in) == 0 {
		return nil
	}
	out := make([]core.ReleaseFile, len(in))
	for i, f := range in {
		if len(f.Members) > 0 {
			f.Members = slices.Clone(f.Members)
		}
		out[i] = f
	}
	return out
}

func cloneSourceCandidates(in []core.SourceCandidate) []core.SourceCandidate {
	if len(in) == 0 {
		return nil
	}
	out := make([]core.SourceCandidate, len(in))
	for i, c := range in {
		if c.FileIndex != nil {
			v := *c.FileIndex
			c.FileIndex = &v
		}
		if len(c.CachedBy) > 0 {
			c.CachedBy = slices.Clone(c.CachedBy)
		}
		if c.Score != nil {
			v := *c.Score
			c.Score = &v
		}
		if len(c.Files) > 0 {
			c.Files = cloneReleaseFiles(c.Files)
		}
		if c.UsenetCheck != nil {
			v := *c.UsenetCheck
			c.UsenetCheck = &v
		}
		out[i] = c
	}
	return out
}

// allowTimeoutPreparing decides whether a guest timeout on /resolve reflects
// bounded forward progress (host source calls in flight, just completed, or a
// pending indexer search) worth continuing within this flight. An idle or
// stalled guest ends the flight and leaves retrying to the client.
func (r *KindResolver) allowTimeoutPreparing(fk resolveFlightKey, st *resolveSearchState) bool {
	if r == nil || st == nil {
		return false
	}
	hasPending, hasActivity, workKeys := st.timeoutProgressSnapshot()
	if !hasPending && !hasActivity {
		return false
	}
	now := r.now()
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	if r.resolveProgress == nil {
		r.resolveProgress = make(map[resolveFlightKey]*resolveProgressEntry)
	}
	for k, e := range r.resolveProgress {
		if now.Sub(e.lastAt) > resolveProgressIdleTTL {
			delete(r.resolveProgress, k)
		}
	}
	ent := r.resolveProgress[fk]
	if ent == nil || now.Sub(ent.lastAt) > resolveProgressIdleTTL {
		if len(r.resolveProgress) >= maxResolveProgressEntries {
			var oldestKey resolveFlightKey
			var oldestAt time.Time
			for k, e := range r.resolveProgress {
				if oldestAt.IsZero() || e.lastAt.Before(oldestAt) {
					oldestKey = k
					oldestAt = e.lastAt
				}
			}
			delete(r.resolveProgress, oldestKey)
		}
		ent = &resolveProgressEntry{
			startedAt:    now,
			lastAt:       now,
			seenWorkKeys: make(map[string]struct{}),
		}
		r.resolveProgress[fk] = ent
	}
	if now.Sub(ent.startedAt) >= resolvePrepareBudget {
		delete(r.resolveProgress, fk)
		return false
	}
	ent.lastAt = now

	newProgress := false
	for _, wk := range workKeys {
		if wk == "" {
			continue
		}
		if _, seen := ent.seenWorkKeys[wk]; !seen {
			newProgress = true
			if len(ent.seenWorkKeys) < maxResolveProgressWorkKeys {
				ent.seenWorkKeys[wk] = struct{}{}
			}
		}
	}
	if newProgress {
		ent.stalledSlices = 0
		return true
	}
	ent.stalledSlices++
	if ent.stalledSlices >= maxStalledResolveSlices {
		delete(r.resolveProgress, fk)
		return false
	}
	return true
}

func (r *KindResolver) clearResolveProgress(fk resolveFlightKey) {
	if r == nil {
		return
	}
	r.progressMu.Lock()
	delete(r.resolveProgress, fk)
	r.progressMu.Unlock()
}

func (r *KindResolver) clearPluginResolveProgress(pluginID string) {
	if r == nil || pluginID == "" {
		return
	}
	prefix := "pk:" + pluginID + ":"
	r.progressMu.Lock()
	for fk := range r.resolveProgress {
		if strings.HasPrefix(string(fk.ref), prefix) {
			delete(r.resolveProgress, fk)
		}
	}
	r.progressMu.Unlock()
}
