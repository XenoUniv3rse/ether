package wasm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"runtime"
	"sync"
	"time"
)

// Candidate is one release on the hot path. Facts is the CandidateFacts
// JSON (SPEC 5.8); ReleaseID keys the memo and must be stable per release.
type Candidate struct {
	ReleaseID string
	Facts     json.RawMessage
}

// memo caches hot-path results by (plugin, version, config hash, release).
// Bounded with FIFO eviction; values are int64 ranks or bool conditions
// encoded as int64.
type memo struct {
	mu    sync.Mutex
	cap   int
	items map[string]int64
	order []string
}

func newMemo(cap int) *memo { return &memo{cap: cap, items: map[string]int64{}} }

func (m *memo) get(k string) (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[k]
	return v, ok
}

func (m *memo) put(k string, v int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[k]; ok {
		m.items[k] = v
		return
	}
	for len(m.items) >= m.cap && len(m.order) > 0 {
		old := m.order[0]
		m.order = m.order[1:]
		delete(m.items, old)
	}
	m.items[k] = v
	m.order = append(m.order, k)
}

func hashKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// warmHot fills the hot pool at load: no hot-path call ever spends its
// budget building an instance (see pool).
func (p *Plugin) warmHot() {
	if !p.caps.RankKey && !p.caps.CondMatch {
		return
	}
	p.hot.fill()
}

func (p *Plugin) hotCallState(ctx context.Context, start time.Time) *callState {
	return &callState{ctx: ctx, host: p.host, lim: &p.lim, start: start, last: start,
		hot: true, logger: p.eng.logger, pluginID: p.id}
}

// hotInstance waits for an idle hot instance. Waiting is not plugin time,
// but it is the resolve's time, so it is bounded by the given budget.
func (p *Plugin) hotInstance(ctx context.Context, wait time.Duration) (instance, error) {
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	return p.hot.get(waitCtx)
}

// fatal reports whether a call error leaves the instance unusable: it
// trapped or was killed at its deadline.
func fatal(err *Error) bool { return err != nil && (err.Trap || err.Status == StatusTimeout) }

// each runs candidates one at a time on an instance without a batch entry
// point (raw modules), under the rules of the JavaScript batch loop (see
// hotPrelude): the budgets bound the plugin's own time, read from the
// monotonic clock and, when that says a budget is spent, from the thread's
// CPU clock; the batch stops starting candidates once its budget is spent,
// and a candidate over budget by the wall clock runs once more and is slow
// only if that run's CPU time is over budget too. A nil result is a slow
// or failed candidate. The loop stops at a fatal error, since the instance
// is dead; the first error of any kind is returned with the partial
// results.
func each[T any](lim *Limits, n int, one func(int) (T, *Error)) ([]*T, *Error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	out := make([]*T, n)
	var first *Error
	wall0 := time.Now()
	cpu0, haveCPU := threadCPU()
	spent := func(w time.Time, c time.Duration, budget time.Duration) bool {
		if time.Since(w) <= budget {
			return false
		}
		now, ok := threadCPU()
		return !haveCPU || !ok || now-c > budget
	}
	for i := 0; i < n; i++ {
		if spent(wall0, cpu0, lim.RankBatch) {
			break
		}
		t0 := time.Now()
		v, err := one(i)
		if err == nil && time.Since(t0) > lim.RankPerCandidate {
			c0, _ := threadCPU()
			t0 = time.Now()
			v, err = one(i)
			if err == nil && spent(t0, c0, lim.RankPerCandidate) {
				continue
			}
		}
		if err != nil {
			if first == nil {
				first = err
			}
			if fatal(err) {
				return out, err
			}
			continue
		}
		out[i] = &v
	}
	return out, first
}

// RankBatch scores candidates with the plugin's rank_key. Scores align
// with cands; a candidate that exceeds its per-candidate budget, or that
// the batch did not reach before its budget was spent, scores the neutral
// 0, counts as slow and is not memoized, so a later batch for the same
// releases picks up where this one stopped. Errors are returned only for
// instance-level failures (trap, kill at the hard deadline); per-candidate
// slowness never fails the resolve.
func (p *Plugin) RankBatch(ctx context.Context, cands []Candidate, config json.RawMessage) (scores []int64, out Outcome, err error) {
	start := time.Now()
	out.Status = StatusOK
	defer func() {
		out.Duration = time.Since(start)
		p.stats.record(out)
	}()
	scores = make([]int64, len(cands))
	if !p.caps.RankKey {
		out.Status = StatusError
		return scores, out, &Error{Status: StatusError, Message: "plugin does not export rank_key"}
	}
	if len(config) == 0 {
		config = json.RawMessage("{}")
	}
	cfgHash := hashKey(string(config))
	pending := make([]int, 0, len(cands))
	for i, c := range cands {
		if v, ok := p.memo.get(hashKey(p.id, p.version, "rank", cfgHash, c.ReleaseID)); ok {
			scores[i] = v
			out.MemoHits++
		} else {
			pending = append(pending, i)
		}
	}
	if len(pending) == 0 {
		return scores, out, nil
	}
	in, gerr := p.hotInstance(ctx, p.lim.RankBatch)
	if gerr != nil {
		out.SlowCandidates = len(pending)
		out.Status = StatusTimeout
		return scores, out, nil // neutral for the rest; health sees the counters
	}
	cs := p.hotCallState(ctx, start)
	facts := make([][]byte, len(pending))
	for j, i := range pending {
		facts[j] = cands[i].Facts
	}
	var got []*int64
	var callErr *Error
	if br, ok := in.(batchRanker); ok {
		got, callErr = br.rankBatch(ctx, cs, facts, config, &p.lim)
	} else {
		killCtx, cancel := context.WithTimeout(ctx, p.lim.hotKill())
		got, callErr = each(&p.lim, len(facts), func(j int) (int64, *Error) { return in.rankOne(killCtx, cs, facts[j], config) })
		cancel()
		if !fatal(callErr) {
			callErr = nil // a failed candidate is neutral, like a slow one
		}
	}
	p.hot.put(in, !fatal(callErr))
	out.Units = cs.finishUnits()
	if callErr != nil {
		out.Status, out.Trap = callErr.Status, callErr.Trap
	}
	for j, i := range pending {
		if j >= len(got) || got[j] == nil {
			out.SlowCandidates++
			continue // neutral 0, not memoized: timing, not a value
		}
		scores[i] = *got[j]
		p.memo.put(hashKey(p.id, p.version, "rank", cfgHash, cands[i].ReleaseID), *got[j])
	}
	if callErr != nil && got == nil {
		return scores, out, callErr
	}
	return scores, out, nil
}

// CondMatch evaluates one rule condition. Budget exhaustion and errors
// read as false with the reason in the Outcome, matching RankBatch's
// neutral-on-trouble rule.
func (p *Plugin) CondMatch(ctx context.Context, cand Candidate, operator string, value json.RawMessage) (match bool, out Outcome, err error) {
	start := time.Now()
	out.Status = StatusOK
	defer func() {
		out.Duration = time.Since(start)
		p.stats.record(out)
	}()
	if !p.caps.CondMatch {
		out.Status = StatusError
		return false, out, &Error{Status: StatusError, Message: "plugin does not export cond_match"}
	}
	if len(value) == 0 {
		value = json.RawMessage("null")
	}
	key := hashKey(p.id, p.version, "cond", hashKey(operator, string(value)), cand.ReleaseID)
	if v, ok := p.memo.get(key); ok {
		out.MemoHits = 1
		return v != 0, out, nil
	}
	in, gerr := p.hotInstance(ctx, p.lim.RankPerCandidate)
	if gerr != nil {
		out.Status = StatusTimeout
		out.SlowCandidates = 1
		return false, out, nil
	}
	cs := p.hotCallState(ctx, start)
	got, callErr := p.condRun(ctx, cs, in, [][]byte{cand.Facts}, operator, value)
	p.hot.put(in, !fatal(callErr))
	out.Units = cs.finishUnits()
	if callErr != nil {
		out.Status, out.Trap = callErr.Status, callErr.Trap
		out.SlowCandidates = 1
		return false, out, callErr
	}
	if got[0] == nil {
		out.SlowCandidates = 1
		return false, out, nil
	}
	v := int64(0)
	if *got[0] {
		v = 1
	}
	p.memo.put(key, v)
	return *got[0], out, nil
}

// condRun evaluates a condition for each candidate under the hot-path
// budgets, in one crossing where the instance supports it.
func (p *Plugin) condRun(ctx context.Context, cs *callState, in instance, facts [][]byte, operator string, value json.RawMessage) ([]*bool, *Error) {
	if bm, ok := in.(batchMatcher); ok {
		return bm.condBatch(ctx, cs, facts, operator, value, &p.lim)
	}
	killCtx, cancel := context.WithTimeout(ctx, p.lim.hotKill())
	defer cancel()
	return each(&p.lim, len(facts), func(i int) (bool, *Error) { return in.condOne(killCtx, cs, facts[i], operator, value) })
}

// CondBatch checks a batch without memoization, suitable for conformance
// testing. It applies the same per-candidate and batch limits as ranking.
func (p *Plugin) CondBatch(ctx context.Context, cands []Candidate, operator string, value json.RawMessage) ([]bool, Outcome, error) {
	out := Outcome{Status: StatusOK}
	results := make([]bool, len(cands))
	if !p.caps.CondMatch {
		return results, out, &Error{Status: StatusError, Message: "plugin does not export cond_match"}
	}
	if len(value) == 0 {
		value = json.RawMessage("null")
	}
	start := time.Now()
	in, err := p.hotInstance(ctx, p.lim.RankBatch)
	if err != nil {
		out.SlowCandidates = len(cands)
		return results, out, err
	}
	cs := p.hotCallState(ctx, start)
	facts := make([][]byte, len(cands))
	for i := range cands {
		facts[i] = cands[i].Facts
	}
	got, callErr := p.condRun(ctx, cs, in, facts, operator, value)
	p.hot.put(in, !fatal(callErr))
	out.Units = cs.finishUnits()
	if callErr != nil {
		out.Status = callErr.Status
		err = callErr
	}
	for i := range cands {
		if i >= len(got) || got[i] == nil {
			out.SlowCandidates++
			continue
		}
		results[i] = *got[i]
	}
	return results, out, err
}
