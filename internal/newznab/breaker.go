package newznab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/events"
)

// An indexer that stops answering (hung, down, behind a challenge it cannot
// pass) used to cost every search its full request timeout, once per query.
// The breaker skips it after a few consecutive transport failures, then lets
// one probe request through when the pause ends: failure doubles the pause,
// while success ramps concurrency gradually (1 -> 2 -> 4 -> full) and steps
// trip back-off down across tiers before closing completely. Requests to
// Newznab/Torznab indexers also time out adaptively, at a margin over the
// indexer's own recent p95.
const (
	breakerThreshold = 3
	breakerBasePause = 2 * time.Minute
	breakerMaxPause  = 30 * time.Minute

	breakerRampStart = 2
	breakerRampMax   = 4

	latencySamples    = 64
	minLatencySamples = 10
	adaptiveFloor     = 10 * time.Second
)

// ErrIndexerPaused reports a search skipped because the indexer failed
// repeatedly. It is transient: the indexer is tried again after the pause.
var ErrIndexerPaused = fmt.Errorf("indexer paused after repeated failures: %w", core.ErrSourceUnavailable)

type breakerTicket struct {
	admitted bool
	probe    bool
	epoch    uint64
}

type breaker struct {
	mu        sync.Mutex
	failures  int // consecutive transient failures
	trips     int // consecutive trips, for the pause back-off
	openUntil time.Time
	outageAt  time.Time
	probing   bool // the one half-open trial request is in flight
	rampLimit int  // >0 while ramping after a probe: max concurrent in-flight requests
	rampOK    int  // consecutive successes completed in the current rampLimit tier
	inFlight  int  // admitted requests currently running
	epoch     uint64

	samples [latencySamples]time.Duration
	n, next int
}

// pausedUntil returns the Unix timestamp until which the breaker skips the
// indexer after repeated failures, or 0 when the breaker is closed.
func (b *breaker) pausedUntil(now time.Time) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.openUntil.IsZero() && now.Before(b.openUntil) {
		return b.openUntil.Unix()
	}
	return 0
}

// blocked reports whether a search must skip the indexer without a request.
func (b *breaker) blocked(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.openUntil.IsZero() {
		return now.Before(b.openUntil) || b.probing || b.inFlight > 0
	}
	return b.rampLimit > 0 && b.inFlight >= b.rampLimit
}

// admit claims a request slot. tk.probe reports the initial half-open probe.
func (b *breaker) admit(now time.Time) (breakerTicket, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.openUntil.IsZero() {
		if now.Before(b.openUntil) || b.probing || b.inFlight > 0 {
			return breakerTicket{}, ErrIndexerPaused
		}
		b.probing = true
		b.inFlight++
		return breakerTicket{admitted: true, probe: true, epoch: b.epoch}, nil
	}
	if b.rampLimit > 0 && b.inFlight >= b.rampLimit {
		return breakerTicket{}, ErrIndexerPaused
	}
	b.inFlight++
	return breakerTicket{admitted: true, epoch: b.epoch}, nil
}

// burstLimit bounds parallel fallback/rescue query batches while the breaker
// is probing or ramping so a single search does not exceed the current tier.
func (b *breaker) burstLimit(maxWidth int) int {
	if maxWidth <= 1 {
		return maxWidth
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.openUntil.IsZero() {
		return 1
	}
	if b.rampLimit > 0 {
		return max(1, min(maxWidth, b.rampLimit-b.inFlight))
	}
	return maxWidth
}

// timeout returns the adaptive request timeout, or 0 to keep the ceiling.
// After any failure, or while probing or ramping, requests get the full
// ceiling so a slow sign-in or cold cache is not cut off prematurely.
func (b *breaker) timeout(ceiling time.Duration) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failures > 0 || !b.openUntil.IsZero() || b.rampLimit > 0 || b.n < minLatencySamples {
		return 0
	}
	s := slices.Clone(b.samples[:b.n])
	slices.Sort(s)
	p95 := s[(len(s)*95+99)/100-1]
	t := max(adaptiveFloor, p95+p95/4)
	if t >= ceiling {
		return 0
	}
	return t
}

// done records a request's outcome. failed reports a transport failure;
// anything else (results, an API error) means the indexer answered and is
// up. sample is the duration of a successful request that counts toward the
// latency profile (0 to skip). It returns the pause when the breaker opens.
func (b *breaker) done(now time.Time, tk breakerTicket, failed bool, sample time.Duration) (opened time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if tk.admitted && b.inFlight > 0 {
		b.inFlight--
	}
	if tk.admitted && tk.epoch != b.epoch {
		return 0
	}
	if tk.probe {
		b.probing = false
	}
	if !failed {
		b.failures = 0
		if sample > 0 {
			b.samples[b.next] = sample
			b.next = (b.next + 1) % latencySamples
			b.n = min(b.n+1, latencySamples)
		}
		switch {
		case tk.probe:
			b.openUntil = time.Time{}
			b.rampLimit = breakerRampStart
			b.rampOK = 0
			b.trips = max(1, b.trips/2)
		case b.rampLimit > 0:
			b.rampOK++
			if b.rampOK >= b.rampLimit {
				if next := b.rampLimit * 2; next <= breakerRampMax {
					b.rampLimit = next
					b.rampOK = 0
					b.trips = max(1, b.trips/2)
				} else {
					b.rampLimit, b.rampOK, b.trips = 0, 0, 0
				}
			}
		default:
			b.openUntil = time.Time{}
			b.trips = 0
		}
		return 0
	}
	b.failures++
	if !tk.probe && b.rampLimit == 0 && (b.failures < breakerThreshold || now.Before(b.openUntil)) {
		return 0
	}
	pause := min(breakerMaxPause, breakerBasePause<<min(b.trips, 8))
	b.trips++
	b.rampLimit, b.rampOK, b.probing = 0, 0, false
	b.openUntil = now.Add(pause)
	if b.outageAt.IsZero() {
		b.outageAt = now
	}
	b.epoch++
	return pause
}

func (b *breaker) outageSnapshot(now time.Time) (failures int, started time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures, b.outageAt
}

func (b *breaker) clearOutage(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.outageAt.IsZero() {
		return 0
	}
	d := max(time.Duration(0), now.Sub(b.outageAt))
	b.outageAt = time.Time{}
	return d
}

// neutral releases an admitted slot whose request ended without a verdict (the
// caller went away or a local budget refused it).
func (b *breaker) neutral(tk breakerTicket) {
	if !tk.admitted {
		return
	}
	b.mu.Lock()
	if b.inFlight > 0 {
		b.inFlight--
	}
	if tk.probe && tk.epoch == b.epoch {
		b.probing = false
	}
	b.mu.Unlock()
}

// transientFailure reports an error that says the indexer did not answer:
// a timeout, a connection failure, an HTTP 5xx or an unpassable challenge.
// API errors, budget refusals and auth failures are answers.
func transientFailure(err error) bool {
	switch {
	case err == nil, errors.Is(err, core.ErrBudgetExhausted), errors.Is(err, core.ErrUnauthorized), errors.Is(err, context.Canceled):
		return false
	case errors.Is(err, core.ErrSourceUnavailable), errors.Is(err, context.DeadlineExceeded), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code >= 500
	}
	var ne net.Error
	return errors.As(err, &ne)
}

type searchCeilingKey struct{}

// withSearchCeiling tags a context whose deadline is the indexer search
// ceiling (for example SearchWithOptions's detached background timeout), so
// DeadlineExceeded on that context counts as an indexer timeout rather than
// a caller leaving early.
func withSearchCeiling(ctx context.Context) context.Context {
	return context.WithValue(ctx, searchCeilingKey{}, true)
}

func isSearchCeiling(ctx context.Context) bool {
	v, _ := ctx.Value(searchCeilingKey{}).(bool)
	return v
}

// callerLeft reports whether ctx ended because the caller cancelled or set a
// shorter deadline than our indexer search ceiling. A DeadlineExceeded at the
// search ceiling (or after defaultHTTPTimeout) means the indexer itself timed
// out and must count against the circuit breaker.
func callerLeft(ctx context.Context, elapsed time.Duration) bool {
	err := ctx.Err()
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) && (isSearchCeiling(ctx) || elapsed >= defaultHTTPTimeout) {
		return false
	}
	return true
}

func backgroundSearch(ctx context.Context) bool {
	return core.SpeculativeIndexerWork(ctx) || core.IsBackgroundCheck(ctx) ||
		core.IsBackgroundAnalysis(ctx) || core.IndexerPurpose(ctx) == core.IndexerPurposeFeed
}

func (c *Client) searchBreaker(ctx context.Context) *breaker {
	if backgroundSearch(ctx) {
		return &c.backgroundBR
	}
	return &c.br
}

// guardSearch runs one search request through the breaker and per-host
// concurrency limiter. Local host-queue wait is excluded from both the
// adaptive timeout and p95 latency sampling. adaptive applies the adaptive
// timeout under ceiling (Newznab/Torznab HTTP requests; a backend's own
// sessions and challenge solving keep their timeouts).
func (c *Client) guardSearch(ctx context.Context, hostURL string, adaptive bool, ceiling time.Duration, do func(context.Context) ([]core.Release, error)) ([]core.Release, error) {
	if err := c.searchPaused(ctx); err != nil {
		return nil, err
	}
	br := c.searchBreaker(ctx)
	tk, err := br.admit(c.now())
	if err != nil {
		return nil, fmt.Errorf("indexer %q: %w", c.Name(), err)
	}
	settled := false
	defer func() {
		if !settled {
			br.neutral(tk)
		}
	}()
	hctx, releaseHost, err := c.acquireHost(ctx, hostURL)
	if err != nil {
		settled = true
		br.neutral(tk)
		return nil, err
	}
	defer releaseHost()
	rctx := hctx
	var limit time.Duration
	if adaptive {
		if limit = br.timeout(ceiling); limit > 0 {
			var cancel context.CancelFunc
			rctx, cancel = context.WithTimeout(hctx, limit)
			defer cancel()
		}
	}
	started := time.Now()
	res, err := do(rctx)
	elapsed := time.Since(started)
	releaseHost()
	if err != nil && (errors.Is(err, core.ErrBudgetExhausted) || callerLeft(ctx, elapsed)) {
		// The caller went away, or a budget or rate-limit pause (which
		// has its own back-off) refused the request: no verdict.
		settled = true
		br.neutral(tk)
		return res, err
	}
	if err != nil && limit > 0 && rctx.Err() != nil {
		err = fmt.Errorf("indexer %q: no answer within %s (its usual time is far shorter): %w", c.Name(), limit.Round(time.Second), context.DeadlineExceeded)
	}
	settled = true
	c.settleBreaker(ctx, br, tk, err, elapsed, adaptive)
	return res, err
}

func (c *Client) settleBreaker(ctx context.Context, br *breaker, tk breakerTicket, err error, elapsed time.Duration, adaptive bool) {
	failed := transientFailure(err)
	var sample time.Duration
	if err == nil && adaptive {
		sample = elapsed
	}
	now := c.now()
	if err != nil {
		c.emitRequestFailed(searchEventKind(ctx), err, elapsed)
	}
	pause := br.done(now, tk, failed, sample)
	if br != &c.br {
		return
	}
	if pause > 0 {
		c.log.Warn("indexer paused after repeated failures; will try again automatically", "indexer", c.Name(), "pause", pause, "err", err)
		failures, _ := c.br.outageSnapshot(now)
		c.publishDomainEvent(events.EventIndexerServiceOutage, events.IndexerServiceOutagePayload{
			IndexerID:           c.indexerID(),
			IndexerName:         c.Name(),
			Kind:                c.indexerKind(),
			ConsecutiveFailures: failures,
			Error:               classifyIndexerError(err),
		})
	} else if tk.probe && !failed {
		c.log.Info("indexer is answering again", "indexer", c.Name())
		outageDur := c.br.clearOutage(now)
		c.publishDomainEvent(events.EventIndexerServiceRecovered, events.IndexerServiceRecoveredPayload{
			IndexerID:        c.indexerID(),
			IndexerName:      c.Name(),
			Kind:             c.indexerKind(),
			OutageDurationMS: outageDur.Milliseconds(),
		})
	}
}

// recordSearchTimeout feeds a search-ceiling timeout that happened before
// guardSearch (for example during a cold t=caps request against a hung server)
// into the circuit breaker so repeated hangs still open backoff.
func (c *Client) recordSearchTimeout(ctx context.Context, br *breaker, err error, elapsed time.Duration) {
	if errors.Is(err, core.ErrBudgetExhausted) {
		return
	}
	tk, admitErr := br.admit(c.now())
	if admitErr != nil {
		return
	}
	c.settleBreaker(ctx, br, tk, err, elapsed, false)
}

// searchPaused reports whether the indexer is paused by the breaker, so a
// search can skip it before any caps or budget work.
func (c *Client) searchPaused(ctx context.Context) error {
	now := c.now()
	if c.br.blocked(now) || (backgroundSearch(ctx) && c.backgroundBR.blocked(now)) {
		return fmt.Errorf("indexer %q: %w", c.Name(), ErrIndexerPaused)
	}
	return nil
}
