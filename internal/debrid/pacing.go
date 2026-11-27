package debrid

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/lru"
)

const blockedHashTTL = 7 * 24 * time.Hour

type accountPaces struct {
	mu       sync.Mutex
	accounts map[[32]byte]*accountPacer
	onPause  func(*accountPacer, time.Time)
	share    func(int) int
	answers  *answerStore
}

func (r *accountPaces) forAccount(kind, key string, now func() time.Time) *accountPacer {
	id := sha256.Sum256([]byte(kind + "\x00" + key))
	identity := hex.EncodeToString(id[:])
	if now == nil {
		now = time.Now
	}
	if r == nil {
		return &accountPacer{identity: identity, kind: kind, now: now}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.accounts == nil {
		r.accounts = make(map[[32]byte]*accountPacer)
	}
	if r.accounts[id] == nil {
		r.accounts[id] = &accountPacer{identity: identity, kind: kind, now: now, answers: r.answers, onPause: r.onPause, share: r.share}
	}
	return r.accounts[id]
}

type accountPacer struct {
	mu                              sync.Mutex
	adds, other                     []time.Time
	waiting, foreground, background int
	until, lastLimited              time.Time
	strikes                         int
	generation                      uint64
	identity, kind                  string
	now                             func() time.Time
	answers                         *answerStore
	learned                         *lru.Cache[answerKey, learnedAnswer]
	acquiring                       map[string]chan struct{}
	api                             APICallStats
	onPause                         func(*accountPacer, time.Time)
	share                           func(int) int
}

type pacedProvider interface {
	accountPace() *accountPacer
}

func lowPriority(ctx context.Context) bool {
	previous, _ := core.NextEpisodePreparation(ctx)
	return previous != 0 || core.IsSpeculative(ctx) || core.SpeculativeIndexerWork(ctx) || core.IsBackgroundCheck(ctx) || isAnalysisRead(ctx)
}

func mayTrialAdd(ctx context.Context) bool {
	if !lowPriority(ctx) {
		return true
	}
	_, playing := core.NextEpisodePreparation(ctx)
	return playing != nil && !core.IsBackgroundCheck(ctx) && !isAnalysisRead(ctx) && playing()
}

func (p *accountPacer) reserve(ctx context.Context, add bool) (uint64, func(), error) {
	p.mu.Lock()
	background := lowPriority(ctx)
	if background {
		defer p.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		if p.waiting > 0 || p.foreground > 0 || p.background > 0 || time.Now().Before(p.until) || !p.window(time.Now(), add, true).IsZero() {
			return 0, nil, core.ErrBudgetExhausted
		}
		p.background++
		return p.generation, func() {
			p.mu.Lock()
			p.background--
			p.mu.Unlock()
		}, nil
	}
	p.waiting++
	defer func() {
		p.waiting--
		p.mu.Unlock()
	}()
	for {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		now := time.Now()
		next := p.until
		if !now.Before(next) {
			next = p.window(now, add, false)
			if next.IsZero() {
				p.foreground++
				return p.generation, func() {
					p.mu.Lock()
					p.foreground--
					p.mu.Unlock()
				}, nil
			}
		}
		if deadline, ok := core.DebridPlaybackDeadline(ctx); ok && !next.Before(deadline) {
			return 0, nil, p.throttleError()
		}
		p.mu.Unlock()
		err := sleep(ctx, time.Until(next))
		p.mu.Lock()
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, nil, p.throttleError()
		}
		if err != nil {
			return 0, nil, err
		}
	}
}

// window reserves a request or returns the earliest free slot. The total
// budget is 132/min against Real-Debrid's documented 250/min. Background
// calls can use at most a third of either budget, leaving the rest for Play.
func (p *accountPacer) window(now time.Time, add, background bool) time.Time {
	times, limit := &p.other, 120
	if add {
		times, limit = &p.adds, 12
	}
	if p.share != nil {
		limit = p.share(limit)
	}
	if background {
		limit /= 3
		if limit == 0 {
			return now.Add(time.Minute)
		}
	}
	for len(*times) > 0 && !now.Before((*times)[0].Add(time.Minute)) {
		*times = (*times)[1:]
	}
	if n := len(*times); n >= limit {
		return (*times)[n-limit].Add(time.Minute)
	}
	*times = append(*times, now)
	return time.Time{}
}

func (p *accountPacer) throttle(generation uint64, delay time.Duration) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if generation == p.generation {
		p.generation++
		// Success on a read says nothing about a throttled add endpoint.
		// Decay the escalation only after a sustained quiet period.
		if now.Sub(p.lastLimited) > 10*time.Minute {
			p.strikes = 0
		}
		p.lastLimited = now
		p.strikes = min(p.strikes+1, 5)
		if delay <= 0 {
			delay = min(2*time.Minute, 8*time.Second<<(p.strikes-1))
		}
	}
	// An older in-flight request may carry a longer explicit Retry-After.
	if next := now.Add(delay); delay > 0 && next.After(p.until) {
		p.until = next
		return next
	}
	return time.Time{}
}

func (p *accountPacer) applyPause(until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if until.After(p.until) {
		p.until = until
		p.generation++
	}
}

func (p *accountPacer) retryAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.until
}

func (p *accountPacer) cooldownFor(ctx context.Context) error {
	until := p.retryAt()
	if deadline, ok := core.DebridPlaybackDeadline(ctx); ok && time.Now().Before(until) && !until.Before(deadline) {
		return p.throttleError()
	}
	return nil
}

func (p *accountPacer) throttleError() error {
	label := kindLabels[p.kind]
	if label == "" {
		label = "The debrid provider"
	}
	return &core.DebridThrottleError{Provider: label}
}

func pacedCall[T any](ctx context.Context, p *accountPacer, r apiRequest, add bool, request func(context.Context, apiRequest) (T, error)) (T, error) {
	var zero T
	r.pace, r.paced = p, true
	if _, bounded := core.DebridPlaybackDeadline(ctx); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, providerOpTimeout)
		defer cancel()
	}
	for {
		if add && !mayTrialAdd(ctx) {
			return zero, errBackgroundUndecided
		}
		generation, release, err := p.reserve(ctx, add)
		if err != nil {
			return zero, err
		}
		var delay time.Duration
		r.retryDelay = &delay
		value, err := request(ctx, r)
		if !errors.Is(err, ErrRateLimited) {
			release()
			return value, err
		}
		var httpLimit *rateLimitError
		if !errors.As(err, &httpLimit) {
			p.noteLimit(false)
		}
		until := p.throttle(generation, delay)
		release()
		if !until.IsZero() && p.onPause != nil {
			p.onPause(p, until)
		}
		if core.IndexerPurpose(ctx) == core.IndexerPurposeTest {
			return value, err
		}
		if lowPriority(ctx) {
			return zero, core.ErrBudgetExhausted
		}
		if r.retry {
			return zero, p.throttleError()
		}
		r.retry = true
		// A provider refusal has no admitted mutation. The next actual
		// attempt must acquire a fresh token after the shared cooldown.
	}
}
