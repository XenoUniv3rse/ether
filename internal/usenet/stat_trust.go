package usenet

import (
	"container/list"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/yenc"
)

const (
	statTrustTTL        = 10 * time.Minute
	statTrustMaxEntries = 16_384
	statTrustWindow     = 64
	statTrustMinSamples = 3
	statUntrustRate     = 0.25
	statRecoverRate     = 0.10
)

// statTrust tracks per-provider agreement between STAT 223 ("article present")
// and subsequent BODY downloads. Some providers answer 223 to STAT for
// taken-down or damaged articles that then fail BODY (430/451, empty/corrupt
// payload, or mid-body drop). Once the correlated failure rate crosses
// statUntrustRate over at least statTrustMinSamples distinct articles, STAT on
// that pool is marked untrusted until real BODY downloads bring the sliding
// failure rate back down to statRecoverRate or below.
type statTrust struct {
	mu    sync.Mutex
	now   func() time.Time
	ttl   time.Duration
	max   int
	items map[string]*list.Element // normalized message-id -> *statTrustItem
	order list.List

	window         [statTrustWindow]bool // true = BODY failed despite STAT 223
	count          int
	fails          int
	pos            int
	untrusted      bool
	verified       int64
	falsePositives int64
}

type statTrustItem struct {
	id          string
	statPresent bool
	bodyDone    bool
	bodyOK      bool
	counted     bool
	expires     time.Time
}

func newStatTrust(now func() time.Time) *statTrust {
	if now == nil {
		now = time.Now
	}
	return &statTrust{
		now:   now,
		ttl:   statTrustTTL,
		max:   statTrustMaxEntries,
		items: make(map[string]*list.Element),
	}
}

func (s *statTrust) getOrCreateLocked(id string, now time.Time) *statTrustItem {
	if el, ok := s.items[id]; ok {
		it := el.Value.(*statTrustItem)
		if now.After(it.expires) {
			*it = statTrustItem{id: id, expires: now.Add(s.ttl)}
			s.order.MoveToBack(el)
		}
		return it
	}
	it := &statTrustItem{id: id, expires: now.Add(s.ttl)}
	s.items[id] = s.order.PushBack(it)
	for s.order.Len() > 0 {
		front := s.order.Front()
		oldest := front.Value.(*statTrustItem)
		if s.order.Len() <= s.max && now.Before(oldest.expires) {
			break
		}
		s.order.Remove(front)
		delete(s.items, oldest.id)
	}
	return it
}

func (s *statTrust) noteStatPresent(id string) {
	if s == nil {
		return
	}
	if nid, err := normalizeMessageID(id); err == nil {
		id = nid
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.getOrCreateLocked(id, now)
	it.statPresent = true
	if it.bodyDone && !it.counted {
		it.counted = true
		s.recordSampleLocked(!it.bodyOK)
	}
}

func (s *statTrust) noteBodyOutcome(id string, ok bool) {
	if s == nil {
		return
	}
	if nid, err := normalizeMessageID(id); err == nil {
		id = nid
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.getOrCreateLocked(id, now)
	it.bodyDone = true
	it.bodyOK = ok
	if it.statPresent && !it.counted {
		it.counted = true
		s.recordSampleLocked(!ok)
	}
}

func (s *statTrust) noteBodyResult(id string, err error, bodyStarted bool, callerCtx context.Context) {
	if s == nil {
		return
	}
	failed, ok := isBodyDownloadFailure(err, bodyStarted, callerCtx)
	if !failed && !ok {
		return
	}
	s.noteBodyOutcome(id, ok)
}

func (s *statTrust) recordSampleLocked(failed bool) {
	s.verified++
	if failed {
		s.falsePositives++
	}
	if s.count < statTrustWindow {
		s.window[s.pos] = failed
		if failed {
			s.fails++
		}
		s.count++
		s.pos = (s.pos + 1) % statTrustWindow
	} else {
		if s.window[s.pos] {
			s.fails--
		}
		s.window[s.pos] = failed
		if failed {
			s.fails++
		}
		s.pos = (s.pos + 1) % statTrustWindow
	}
	if s.count < statTrustMinSamples {
		return
	}
	rate := float64(s.fails) / float64(s.count)
	if !s.untrusted && rate >= statUntrustRate {
		s.untrusted = true
	} else if s.untrusted && rate <= statRecoverRate {
		s.untrusted = false
	}
}

func (s *statTrust) isUntrusted() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.untrusted
}

// healthWeight returns [0.1, 1.0] for weighting this provider in tier ordering
// and health decisions. Fresh or accurate providers keep 1.0.
func (s *statTrust) healthWeight() float64 {
	if s == nil {
		return 1.0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count < statTrustMinSamples || (s.fails == 0 && !s.untrusted) {
		return 1.0
	}
	rate := float64(s.fails) / float64(s.count)
	if s.untrusted {
		return max(0.1, 0.25*(1.0-rate))
	}
	return max(0.5, 1.0-2.0*rate)
}

func (s *statTrust) snapshot() (verified, falsePositives int64, untrusted bool, failureRate float64) {
	if s == nil {
		return 0, 0, false, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count > 0 {
		failureRate = float64(s.fails) / float64(s.count)
	}
	return s.verified, s.falsePositives, s.untrusted, failureRate
}

// isBodyDownloadFailure classifies whether a BODY result proves the article
// downloaded cleanly (ok=true) or failed to download despite STAT 223
// (failed=true). Caller cancellations, budget exhaustion, auth lockouts, and
// pre-BODY connection/capacity errors are ignored (both false).
func isBodyDownloadFailure(err error, bodyStarted bool, callerCtx context.Context) (failed, ok bool) {
	if err == nil {
		return false, true
	}
	if callerCtx != nil && ctxErr(callerCtx) != nil {
		return false, false
	}
	switch {
	case errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, errPipelineUnanswered),
		errors.Is(err, errStaleReuse),
		errors.Is(err, ErrPoolClosed),
		errors.Is(err, ErrProviderDisabled),
		errors.Is(err, ErrAuthFailed),
		errors.Is(err, ErrAuthRequired),
		errors.Is(err, ErrProviderBackoff),
		errors.Is(err, ErrServiceUnavailable),
		errors.Is(err, errAtLimit),
		errors.Is(err, core.ErrBudgetExhausted),
		errors.Is(err, ErrInvalidMessageID):
		return false, false
	case errors.Is(err, ErrNoSuchArticle), errors.Is(err, ErrCorrupt):
		return true, false
	case bodyStarted:
		return true, false
	default:
		return false, false
	}
}

// demoteUntrustedStatPools lowers providers whose STAT is untrusted behind
// STAT-trusted providers of the same backfill class while preserving tier and
// load order within each group. When all pools are trusted it returns pools as-is.
func demoteUntrustedStatPools(pools []*pool) []*pool {
	hasUntrusted := false
	for _, p := range pools {
		if p.statTrust.isUntrusted() {
			hasUntrusted = true
			break
		}
	}
	if !hasUntrusted {
		return pools
	}
	out := make([]*pool, 0, len(pools))
	for _, backfill := range []bool{false, true} {
		for _, untrusted := range []bool{false, true} {
			for _, p := range pools {
				if p.cfg.Backfill == backfill && p.statTrust.isUntrusted() == untrusted {
					out = append(out, p)
				}
			}
		}
	}
	return out
}

func (p *pool) verifyBodyHeader(ctx context.Context, id string, groups []string, prio core.Priority) error {
	if core.IsBackgroundCheck(ctx) {
		prio = core.PriorityPrefetch
	}
	settle, err := p.reserveBackgroundBody(ctx)
	if err != nil {
		return err
	}
	var bodyStarted bool
	var used int64
	defer func() { settle(used) }()
	err = p.withConn(ctx, prio, func(c *Conn) error {
		before := c.wireBytes()
		defer func() { used += c.wireBytes() - before }()
		if p.o.UseGroup && len(groups) > 0 && !slices.Contains(groups, c.CurrentGroup()) {
			if err := c.Group(ctx, groups[0]); err != nil && !errors.Is(err, ErrNoSuchGroup) {
				return err
			}
		}
		reader, err := c.Body(ctx, id)
		bodyStarted = c.bodyStarted
		if err == nil {
			var unread int64
			unread, err = probeBodyHeader(c, reader, p.o.MaxArticleSize, core.ArticlePart{})
			used += unread
		}
		if errors.Is(err, ErrNoSuchArticle) {
			p.bodyMisses.Add(1)
			var pe *ProtocolError
			if errors.As(err, &pe) && pe.Code == 451 {
				p.bodyTakedowns.Add(1)
			}
		}
		return err
	})
	p.statTrust.noteBodyResult(id, err, bodyStarted, ctx)
	if err != nil {
		return err
	}
	p.brk.success()
	return nil
}

func (p *pool) verifyFullBody(ctx context.Context, id string, groups []string, prio core.Priority) error {
	if core.IsBackgroundCheck(ctx) {
		prio = core.PriorityPrefetch
	}
	settle, err := p.reserveBackgroundBody(ctx)
	if err != nil {
		return err
	}
	var (
		bodyStarted bool
		rtt, took   time.Duration
		used        int64
	)
	defer func() { settle(used) }()
	err = p.withConn(ctx, prio, func(c *Conn) error {
		before := c.wireBytes()
		defer func() { used += c.wireBytes() - before }()
		start := time.Now()
		if p.o.UseGroup && len(groups) > 0 && !slices.Contains(groups, c.CurrentGroup()) {
			if err := c.Group(ctx, groups[0]); err != nil && !errors.Is(err, ErrNoSuchGroup) {
				return err
			}
		}
		_, err := c.BodyDecode(ctx, id, &yenc.DecodeOptions{MaxSize: p.o.MaxArticleSize})
		bodyStarted = c.bodyStarted
		if err != nil && bodyStarted {
			// An abandoned body's unread remainder is unknown: keep the reservation.
			used += maxEncodedBody(p.o.MaxArticleSize)
		}
		rtt = c.lastRTT
		took = time.Since(start)
		if errors.Is(err, ErrNoSuchArticle) {
			p.bodyMisses.Add(1)
			var pe *ProtocolError
			if errors.As(err, &pe) && pe.Code == 451 {
				p.bodyTakedowns.Add(1)
			}
		}
		return err
	})
	p.statTrust.noteBodyResult(id, err, bodyStarted, ctx)
	if err != nil {
		return err
	}
	p.brk.success()
	p.articles.Add(1)
	p.latMu.Lock()
	p.latency.add(rtt)
	p.fetching.add(took)
	p.latMu.Unlock()
	return nil
}

// StatUntrusted reports whether any currently usable provider pool has an
// elevated STAT-present -> BODY-failure rate and therefore cannot be trusted
// on raw STAT alone.
func (f *Fetcher) StatUntrusted() bool {
	if f == nil {
		return false
	}
	f.mu.RLock()
	pools := slices.Clone(f.pools)
	f.mu.RUnlock()
	for _, p := range pools {
		if p.serves() && p.statTrust.isUntrusted() {
			return true
		}
	}
	return false
}

// VerifyArticles checks whether every article in ids is available across
// configured providers, combining pipelined STAT with sampled BODY checks and
// full/header BODY verification on providers whose STAT is untrusted.
func (f *Fetcher) VerifyArticles(ctx context.Context, ids []string, groups []string) (bool, error) {
	if len(ids) == 0 {
		return true, nil
	}
	missing := false
	err := f.StatStream(ctx, ids, func(_ string, present bool) bool {
		if !present {
			missing = true
			return false
		}
		return true
	})
	if err != nil {
		return false, err
	}
	if missing {
		return false, nil
	}
	prio := commandPriority(ctx)
	untrusted := f.StatUntrusted()
	mid := len(ids) / 2
	seen := make(map[string]bool, 3)
	for _, idx := range []int{0, mid, len(ids) - 1} {
		normalized, nerr := normalizeMessageID(ids[idx])
		if nerr != nil {
			return false, nerr
		}
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		var verr error
		if untrusted && idx == mid {
			_, verr = f.Body(ctx, ids[idx], groups, prio)
		} else {
			verr = f.CheckBody(ctx, ids[idx], groups, prio)
		}
		switch {
		case verr == nil:
		case errors.Is(verr, core.ErrArticleMissing), errors.Is(verr, ErrCorrupt):
			return false, nil
		default:
			return false, verr
		}
	}
	return true, nil
}

type statPending struct {
	caller    string
	missed    bool
	uncertain bool
	backbones []string // backbones whose accounts answered 430 (StatStream)
}

// verifyUntrustedStatClaims validates STAT-223 claims from a pool whose STAT
// has been failing real downloads: a spread sample (including one full decoded
// article fetch and header probes on the rest) runs first, and if any sample
// fails, remaining claims on this pool are verified rather than trusted.
func (f *Fetcher) verifyUntrustedStatClaims(
	ctx context.Context,
	p *pool,
	claimed []string,
	lastPool bool,
	firstErr *error,
	pend map[string]*statPending,
	fn func(id string, present bool) bool,
) (stopped bool) {
	if len(claimed) == 0 {
		return false
	}
	prio := commandPriority(ctx)
	m := len(claimed)
	var sampleIDs []string
	fullID := claimed[m/2]
	if m <= 4 {
		sampleIDs = slices.Clone(claimed)
	} else {
		fullID = claimed[m/3]
		for _, idx := range []int{0, m / 3, (2 * m) / 3, m - 1} {
			if !slices.Contains(sampleIDs, claimed[idx]) {
				sampleIDs = append(sampleIDs, claimed[idx])
			}
		}
	}

	runProbes := func(ids []string, full string) map[string]error {
		errs := make(map[string]error, len(ids))
		if len(ids) == 0 {
			return errs
		}
		workers := max(1, min(4, p.target(), len(ids)))
		jobs := make(chan string, len(ids))
		for _, id := range ids {
			jobs <- id
		}
		close(jobs)
		var mu sync.Mutex
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for id := range jobs {
					if ctx.Err() != nil {
						return
					}
					var err error
					if id == full {
						err = p.verifyFullBody(ctx, id, nil, prio)
					} else {
						err = p.verifyBodyHeader(ctx, id, nil, prio)
					}
					mu.Lock()
					errs[id] = err
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		return errs
	}

	applyResult := func(id string, verr error) (liedOrErrored, stop bool) {
		st, still := pend[id]
		if !still {
			return false, false
		}
		switch {
		case verr == nil:
			delete(pend, id)
			if !fn(st.caller, true) {
				return false, true
			}
			return false, false
		case errors.Is(verr, ErrNoSuchArticle), errors.Is(verr, ErrCorrupt):
			st.missed = true
			if errors.Is(verr, ErrNoSuchArticle) {
				// A real 430 is the store's answer: share it with the
				// backbone. A corrupt copy is this server's alone.
				f.noteMissing(id, p)
				if b := p.backbone(); b != "" {
					st.backbones = append(st.backbones, b)
				}
			} else {
				f.misses.add(id, p.key)
			}
			if lastPool && !st.uncertain {
				delete(pend, id)
				if !fn(st.caller, false) {
					return true, true
				}
			}
			return true, false
		default:
			if *firstErr == nil {
				*firstErr = verr
			}
			if !errors.Is(verr, ErrProviderDisabled) && !errors.Is(verr, ErrAuthFailed) {
				st.uncertain = true
			}
			return true, false
		}
	}

	sampleErrs := runProbes(sampleIDs, fullID)
	sampled := make(map[string]bool, len(sampleIDs))
	distrustBatch := false
	for _, id := range sampleIDs {
		sampled[id] = true
		bad, stop := applyResult(id, sampleErrs[id])
		if bad {
			distrustBatch = true
		}
		if stop {
			return true
		}
	}
	if len(sampled) == len(claimed) {
		return false
	}

	var rest []string
	for _, id := range claimed {
		if !sampled[id] {
			if _, still := pend[id]; still {
				rest = append(rest, id)
			}
		}
	}
	if len(rest) == 0 {
		return false
	}
	if !distrustBatch {
		for _, id := range rest {
			st, still := pend[id]
			if !still {
				continue
			}
			delete(pend, id)
			if !fn(st.caller, true) {
				return true
			}
		}
		return false
	}

	// At least one STAT-223 article in this batch failed BODY on p: verify
	// remaining claims one chunk at a time so an early-stop fn exits promptly.
	step := max(1, min(4, p.target()))
	for i := 0; i < len(rest); i += step {
		chunk := rest[i:min(i+step, len(rest))]
		errs := runProbes(chunk, "")
		for _, id := range chunk {
			if _, stop := applyResult(id, errs[id]); stop {
				return true
			}
		}
	}
	return false
}
