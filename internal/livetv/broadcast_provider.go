package livetv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// providerBroadcastPlanner feeds one IPTV channel's continuous output from
// its provider inputs. The output never restarts: every recovery (same-source
// reopen, codec-change rejoin, backup source, hot standby) is a new decoder
// spliced into the same encoder timeline by the broadcast clock.
type providerBroadcastPlanner struct {
	provider              providerTracker
	guideCancel           context.CancelFunc
	guideDone             chan struct{}
	guideFrom, guideUntil time.Time
	guide                 []Programme
	s                     *Service
	channel               Channel
	format                transcode.RawFormat
	runner                transcode.ContinuousRunner
	mu                    sync.Mutex
	lastAired             map[string]time.Duration
	firstAired            bool
	startupFailures       []TuneInputFailure

	ladder         *steadyLadder
	direct         *directStreamState
	manifest       *cachedManifest
	link           *steadyLink // granted input of the current source, reused by same-source reopens
	primed         *steadyLink // the tune's probed input; the first open reads the bytes it probed
	attempt        *steadyAttempt
	stalledAttempt *steadyAttempt
	standby        *steadyStandby
	lastOpened     StreamSource
	pendingStandby bool
	retryAt        time.Time
	lastStress     time.Time
	retry          func() // asks the clock to replace the current input now
	waiting        bool   // failed() scheduled a later retry than now
	openGen        uint64
	linkSeq        atomic.Uint64
}

// steadyLink is one granted provider input: the relay grant (and with it the
// shared tap) plus the provider-account connection it holds.
type steadyLink struct {
	src           StreamSource
	ss            *session
	release       func()
	format        string // probed container: "hls", "mpegts" or empty
	uses          int
	directGen     uint64
	admissionWait time.Duration
	primedDecoder transcode.Decoder // admission already obtained its first picture
	refs          atomic.Int32
	closed        atomic.Bool
	once          sync.Once
}

func newSteadyLink(src StreamSource, ss *session, release func(), format string) *steadyLink {
	l := &steadyLink{src: src, ss: ss, release: release, format: format}
	l.refs.Store(1)
	return l
}

func (l *steadyLink) retain() {
	if l != nil {
		l.refs.Add(1)
	}
}

func (l *steadyLink) releaseRef(s *Service) {
	if l != nil && l.refs.Add(-1) <= 0 {
		l.close(s)
	}
}

func (l *steadyLink) close(s *Service) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.closed.Store(true)
		if l.primedDecoder != nil {
			_ = l.primedDecoder.Close()
			l.primedDecoder = nil
		}
		s.revokeInput(l.ss)
		l.ss.cancel()
		if l.release != nil {
			l.release()
		}
	})
}

type steadyAttempt struct {
	src     StreamSource
	link    *steadyLink
	decoder *providerBroadcastDecoder
}

// A recovery that rejoins the source it just lost gets a short first-picture
// deadline when a backup can take over; a cold or sole input waits longer.
const (
	steadyFirstFrameCold   = 8 * time.Second
	steadyFirstFrameAlone  = 20 * time.Second
	steadyFirstFrameRejoin = 4 * time.Second
	steadyFirstFrameBackup = 8 * time.Second
	steadyStress           = time.Second
	steadyStarvedAfter     = 400 * time.Millisecond
	steadyHoldPicture      = 10 * time.Second
	steadyHoldStalled      = 20 * time.Second
	// steadyDecoderHung is how long a decoder may sit on a buffered reserve
	// without producing a picture before it is replaced.
	steadyDecoderHung   = 3 * time.Second
	steadyStandbyLinger = 30 * time.Second
	steadyClockReset    = 2 * time.Minute
	// Skip at most this much already-aired media on a reopen: a longest
	// reserve plus one provider segment. Longer means the clock moved.
	steadyReplayLimit        = 40 * time.Second
	steadyStandbyID          = "-standby"
	steadyMaxLinkUses        = 4
	steadyOpenResolveTimeout = 5 * time.Second
)

var (
	errSteadyFirstFrame  = errors.New("live input produced no picture in time")
	errSteadyStarved     = errors.New("live input stopped delivering pictures")
	errSteadyChangeStorm = errors.New("live input repeatedly changed without delivering pictures")
)

func (p *providerBroadcastPlanner) plans(now time.Time) (broadcastPlan, *broadcastPlan) {
	p.mu.Lock()
	ready := !now.Before(p.guideFrom) && now.Before(p.guideUntil)
	var tag *broadcastPolicyTag
	if ready {
		for _, programme := range p.guide {
			if !now.Before(programme.Start) && now.Before(programme.End) {
				tag = programme.policyTag
				break
			}
		}
	}
	p.mu.Unlock()
	if !ready {
		// Hold on a neutral signal screen until the background EPG snapshot is
		// ready. A placeholder denial must never poison a published screen segment.
		// Changing identity also retires an active source if its policy expires.
		return broadcastPlan{id: "provider-policy-wait-" + p.channel.ID}, nil
	}
	return broadcastPlan{element: "programme", policy: tag, programme: true, id: "provider-" + p.channel.ID, open: p.open, failed: p.failed}, nil
}

// A cold IPTV tune waits briefly for its first real picture before the
// output starts on the reconnecting screen: the probe already read the bytes
// this decoder starts from.
//
// Its startup burst only encodes pictures already decoded (the probe's
// bytes, a tap or playlist backlog) a segment or two ahead, so the client
// starts with a cushion; publishing never waits on the input, and the
// rest of a reserve the decoder starts behind the live edge stays on the
// server, where it hides upstream stalls.
//
// The owner profile sets the segment target and desired published lead.
// A cold source without backlog builds that lead opportunistically; neither
// the first playlist nor the frame clock waits for the full cushion.
func (p *providerBroadcastPlanner) coldWait() time.Duration    { return 4 * time.Second }
func (p *providerBroadcastPlanner) holdStalled() time.Duration { return steadyHoldPicture }
func (p *providerBroadcastPlanner) startupLead() time.Duration {
	target := p.s.latencyPolicy().TargetLatency()
	p.mu.Lock()
	hls := p.primed != nil && p.primed.format == "hls"
	if !hls && p.link != nil && p.link.format == "hls" {
		hls = true
	}
	p.mu.Unlock()
	if hls {
		return target
	}
	// MPEG-TS taps pace readers up to tapPaceLead (4 s) ahead of due playout;
	// cap startup burst lead below tapPaceLead so the frame clock never outruns
	// tapPace while keeping the remaining startup burst inside the reserve.
	return min(target, 2500*time.Millisecond)
}
func (p *providerBroadcastPlanner) segmentDuration() time.Duration {
	return p.s.latencyPolicy().SegmentDuration()
}

func (p *providerBroadcastPlanner) reserveTarget() (depth, target time.Duration, ok bool) {
	p.mu.Lock()
	a := p.attempt
	if a == nil {
		a = p.stalledAttempt
	}
	p.mu.Unlock()
	if a == nil || a.link == nil {
		return 0, 0, false
	}
	want := p.s.stabilityReserve(p.channel)
	h, hok := p.s.inputHealth(a.link.ss)
	if !hok {
		return 0, want, false
	}
	if h.Target > 0 && h.Target < want {
		want = h.Target
	}
	return h.ReserveDepth, want, !h.Stalled || h.ReserveDepth > 0
}

func (p *providerBroadcastPlanner) setInputPlayRate(rate float64) {
	p.mu.Lock()
	a := p.attempt
	if a == nil {
		a = p.stalledAttempt
	}
	p.mu.Unlock()
	if a != nil && a.link != nil {
		p.s.setSteadyInputRate(a.link.ss, rate)
	}
}

const steadySegment = 2 * time.Second

func (p *providerBroadcastPlanner) bindRetry(retry func()) {
	p.mu.Lock()
	p.retry = retry
	p.mu.Unlock()
}

func (p *providerBroadcastPlanner) logger() *slog.Logger {
	if p.s == nil || p.s.o.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return p.s.o.Logger.With("channel", p.channel.ID)
}

func sourceIDs(sources []StreamSource) []string {
	ids := make([]string, len(sources))
	for i, src := range sources {
		ids[i] = src.ID
	}
	return ids
}

// keepStalled reports that the current input's transport is still alive or
// recovering by itself: the shared tap holds a live socket across normal
// inter-burst silences and splices reconnects onto the same timeline, and the
// HLS reader keeps reloading its playlist. Never close the decoder on a gap
// shorter than the source's adapted stall threshold while the tap still has a
// live socket; hold the last picture instead. Failed is true only when the
// transport is actually gone and same-source reconnects have failed.
func (p *providerBroadcastPlanner) keepStalled(stalledFor time.Duration) bool {
	p.mu.Lock()
	a := p.attempt
	if a == nil {
		a = p.stalledAttempt
	}
	p.mu.Unlock()
	if a == nil || a.link == nil {
		return false
	}
	h, ok := p.s.inputHealth(a.link.ss)
	if !ok {
		return stalledFor < steadyHoldStalled && p.s.healthGrant(a.link.ss) != nil
	}
	if h.Failed {
		return false
	}
	// The provider delivered after the pictures stopped and the reserve holds
	// media the decoder is not taking: the decoder itself is stuck, and only
	// a new one helps. A quiet provider is never that: a second decoder on
	// the same connection would wait for the same bytes.
	if stalledFor >= steadyDecoderHung && h.DataAge < stalledFor-steadyDecoderHung/2 && h.ReserveDepth >= steadyDecoderHung {
		return false
	}
	return stalledFor < max(steadyHoldStalled, h.StallAfter)
}

func (p *providerBroadcastPlanner) commitReplacement() {
	p.mu.Lock()
	prev := p.lastOpened
	if p.stalledAttempt != nil && prev.ID == "" {
		prev = p.stalledAttempt.src
	}
	var cur StreamSource
	if p.attempt != nil {
		cur = p.attempt.src
	}
	standby := p.pendingStandby
	p.pendingStandby = false
	p.stalledAttempt = nil
	if cur.ID != "" {
		p.lastOpened = cur
	}
	p.mu.Unlock()
	if prev.ID != "" && cur.ID != "" && prev.ID != cur.ID {
		reason := "input failover"
		if standby {
			reason = "standby promoted"
			p.logger().Info("live input switched to standby", append([]any{"input", cur.ID}, p.s.duplicateFailoverLogAttrs(p.channel, prev, cur)...)...)
		} else {
			p.logger().Info("live input switched source", append([]any{"from", prev.ID, "to", cur.ID}, p.s.duplicateFailoverLogAttrs(p.channel, prev, cur)...)...)
		}
		p.s.emitSourceFailover(p.channel, prev.ID, cur.ID, reason, "broadcast")
	}
}

func (p *providerBroadcastPlanner) abortReplacement() {
	p.mu.Lock()
	p.openGen++
	p.pendingStandby = false
	prev := p.stalledAttempt
	var drop *steadyLink
	if prev != nil {
		p.stalledAttempt = nil
		p.attempt = prev
		if prev.src.ID != "" {
			p.lastOpened = prev.src
		}
		if p.link != prev.link {
			drop = p.link
			if prev.link != nil && !prev.link.closed.Load() {
				prev.link.retain()
				p.link = prev.link
			} else {
				p.link = nil
			}
		}
		p.ladder.delivered(prev.src.ID)
	}
	p.mu.Unlock()
	if prev != nil && p.s != nil {
		p.s.recordSourceHealth(p.channel.ID, prev.src, nil, true)
	}
	if drop != nil {
		drop.releaseRef(p.s)
	}
}

// failed records why the current input ended and returns when the channel
// opens its next one. It never parks the channel behind a source cooldown.
func (p *providerBroadcastPlanner) failed(err error) time.Time {
	now := p.s.o.Now()
	candidates := p.s.orderedSourceCandidates(p.channel)
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.attempt
	p.attempt = nil
	if a == nil {
		// open failed before any decoder started; it already chose the retry.
		if p.retryAt.IsZero() {
			return now.Add(steadySpacing)
		}
		return p.retryAt
	}
	if p.stalledAttempt == nil || p.stalledAttempt.link == nil || p.stalledAttempt.link.closed.Load() {
		p.stalledAttempt = a
	}
	aired := a.decoder.delivered()
	changed := errors.Is(err, transcode.ErrInputChanged)
	if !p.firstAired && !changed {
		p.noteStartupFailure(a.src.ID, err)
	}
	// A transport-stream tap already reconnected its URL itself before its
	// readers saw the end, so the ladder skips straight to the next source.
	tapped := a.link.format == "mpegts"
	failover := !p.firstAired && !changed
	if h, ok := p.s.inputHealth(a.link.ss); ok {
		tapped = h.Kind == "ts"
		failover = failover || h.Failed
	}
	failover = failover || tapped && steadyFailover(err) || errors.Is(err, errSteadyChangeStorm)
	at := p.ladder.failed(a.src.ID, err, aired, failover, sourceIDs(candidates), now)
	if p.ladder.state(a.src.ID).parked.After(now) && changed {
		err, changed, failover = errSteadyChangeStorm, false, true
	}
	if !changed {
		p.s.recordSourceHealth(p.channel.ID, a.src, err, false)
	}
	if failover && p.ladder.next == a.src.ID {
		// The tap gave up on this source and its readers ended: what it
		// buffered has aired. Rejoin at the live edge rather than re-decode it.
		p.ladder.liveEdge = true
	}
	if sb := p.standby; sb != nil && !changed && sb.src.ID != a.src.ID && sb.ready() {
		// Hot standby: the backup is already decoding at its live edge.
		p.ladder.next, p.ladder.liveEdge = sb.src.ID, false
		at = now
	}
	var nextSrc StreamSource
	for _, cand := range candidates {
		if cand.ID == p.ladder.next {
			nextSrc = cand
			break
		}
	}
	if nextSrc.ID == "" && p.standby != nil && p.standby.src.ID == p.ladder.next {
		nextSrc = p.standby.src
	}
	p.waiting = at.Sub(now) > 250*time.Millisecond
	p.logger().Info("live input lost", append([]any{"input", a.src.ID, "reason", err, "aired", aired, "failover", failover, "next", p.ladder.next, "retryIn", at.Sub(now)}, p.s.duplicateFailoverLogAttrs(p.channel, a.src, nextSrc)...)...)
	return at
}

// open starts the decoder of the input the ladder chose. Synchronous failures
// (resolution, grant, spawn) advance the ladder within this call, so the
// channel moves to a backup without waiting for another clock tick. When
// replacing a stalled live input in parallel, the old input's link keeps its
// decoder reference open until the replacement delivers a picture (unless a
// single-connection account's cap leaves no room for a second connection).
func (p *providerBroadcastPlanner) open(ctx context.Context) (transcode.Decoder, func(), error) {
	c := p.channel
	p.mu.Lock()
	gen := p.openGen
	p.attempt = nil
	p.pendingStandby = false
	p.retryAt = time.Time{}
	p.waiting = false
	stalled := p.stalledAttempt
	p.mu.Unlock()
	var last error = core.ErrNotAvailable
	var heldDisplaced *steadyLink
	busy := map[string]bool{}
	for range 2*(MaxChannelInputs+1) + 2 {
		now := p.s.o.Now()
		var candidates []StreamSource
		for _, src := range p.s.orderedSourceCandidates(c) {
			if !busy[src.ID] {
				candidates = append(candidates, src)
			}
		}
		p.mu.Lock()
		if p.openGen != gen {
			p.mu.Unlock()
			return nil, nil, context.Canceled
		}
		id, liveEdge, ok, retry := p.ladder.choose(sourceIDs(candidates), now)
		if !ok && (len(candidates) == 0 || stalled != nil && busy[stalled.src.ID]) {
			var dropStandby *steadyStandby
			if p.standby != nil && len(busy) > 0 {
				dropStandby = p.standby
				p.standby = nil
			} else if (heldDisplaced == nil || heldDisplaced.closed.Load()) && p.stalledAttempt != nil && p.stalledAttempt.link != nil && !p.stalledAttempt.link.closed.Load() {
				heldDisplaced = p.stalledAttempt.link
			}
			p.mu.Unlock()
			if dropStandby != nil {
				dropStandby.close(p.s)
				clear(busy)
				continue
			}
			if heldDisplaced != nil && !heldDisplaced.closed.Load() && len(busy) > 0 {
				// Single-connection account cannot race a second connection in
				// parallel; release the stalled connection's slot and retry.
				heldDisplaced.close(p.s)
				heldDisplaced = nil
				clear(busy)
				continue
			}
			p.mu.Lock()
			if p.openGen == gen {
				p.retryAt = now.Add(steadySpacing)
			}
			p.mu.Unlock()
			return nil, nil, fmt.Errorf("broadcast provider: %w", last)
		}
		if !ok {
			p.retryAt = retry
			p.mu.Unlock()
			return nil, nil, fmt.Errorf("broadcast provider: every input is resting: %w", last)
		}
		var src StreamSource
		for _, candidate := range candidates {
			if candidate.ID == id {
				src = candidate
			}
		}
		// Closing a link can close an upstream connection synchronously;
		// never do that under p.mu, which the frame clock also takes.
		var retired []*steadyLink
		if sb := p.standby; sb != nil && sb.src.ID == id && !sb.failed.Load() {
			ready := sb.ready()
			racing := p.stalledAttempt != nil
			prev := p.lastOpened
			if !racing {
				p.lastOpened = src
			}
			p.pendingStandby = racing && ready
			p.standby = nil
			sbLink := sb.link
			if p.link != nil && p.link != sbLink {
				retired = append(retired, p.link)
			}
			p.link = sbLink
			sbLink.retain()
			var firstFrame time.Duration
			if !ready {
				firstFrame = p.firstFrameDeadlineLocked(src.ID, len(candidates) > 1)
			}
			p.ladder.opened(id, now)
			decoder := p.wrap(ctx, src, sbLink, sb.adopt(), firstFrame)
			p.attempt = &steadyAttempt{src: src, link: sbLink, decoder: decoder}
			p.mu.Unlock()
			for _, l := range retired {
				l.releaseRef(p.s)
			}
			if !racing {
				if ready {
					p.logger().Info("live input switched to standby", append([]any{"input", id}, p.s.duplicateFailoverLogAttrs(c, prev, src)...)...)
					if prev.ID != "" && prev.ID != src.ID {
						p.s.emitSourceFailover(c, prev.ID, src.ID, "standby promoted", "broadcast")
					}
				} else if prev.ID != "" && prev.ID != src.ID {
					p.logger().Info("live input switched source", append([]any{"from", prev.ID, "to", src.ID}, p.s.duplicateFailoverLogAttrs(c, prev, src)...)...)
					p.s.emitSourceFailover(c, prev.ID, src.ID, "input failover", "broadcast")
				}
			}
			return decoder, func() { sbLink.releaseRef(p.s) }, nil
		}
		link := p.link
		if link != nil && link.closed.Load() {
			link = nil
			p.link = nil
		}
		if primed := p.primed; primed != nil {
			p.primed = nil
			if primed.src.ID == id && link == nil {
				link = primed
			} else {
				retired = append(retired, primed)
			}
		}
		if link != nil && (link.src.ID != id || (p.direct == nil && link.src.URL != src.URL) || (p.direct != nil && p.direct.needsRefresh(link.directGen)) || link.uses >= steadyMaxLinkUses && !p.s.linkTransportAlive(link)) {
			p.rememberManifestLocked(link)
			retired = append(retired, link)
			if heldDisplaced == nil {
				heldDisplaced = link
			}
			link = nil
		}
		p.link = link
		if link != nil {
			link.retain()
		}
		direct := p.direct
		cached := p.manifestForLocked(src.URL)
		p.mu.Unlock()
		for _, l := range retired {
			l.releaseRef(p.s)
		}
		if link == nil {
			var err error
			resID := "broadcast-" + c.ID + "-" + strconv.FormatUint(p.linkSeq.Add(1), 10)
			link, err = p.s.linkSteadySourceWithState(ctx, c, src, resID, 0, direct, cached, nil)
			if errors.Is(err, ErrProviderBusy) || errors.Is(err, ErrProviderPaced) {
				busy[id] = true
				// Reopening the stalled input can share its slot. Defer that
				// reopen so another input can win, or free the slot and retry
				// the replacement if its provider is at capacity.
				if errors.Is(err, ErrProviderBusy) && stalled != nil && stalled.link != nil &&
					!stalled.link.closed.Load() && providerSourceID(c, src) == providerSourceID(c, stalled.src) {
					busy[stalled.src.ID] = true
				}
				last = err
				continue
			}
			if err != nil {
				last = err
				p.mu.Lock()
				stale := p.openGen != gen
				p.mu.Unlock()
				if stale {
					return nil, nil, context.Canceled
				}
				p.syncFailure(src, err, now)
				continue
			}
			p.mu.Lock()
			if p.openGen != gen {
				p.mu.Unlock()
				link.releaseRef(p.s)
				return nil, nil, context.Canceled
			}
			p.link = link
			link.retain()
			p.mu.Unlock()
		}
		decoder, err := p.startDecoder(ctx, link, liveEdge)
		if err != nil {
			last = err
			var releasePlanner bool
			p.mu.Lock()
			stale := p.openGen != gen
			if !stale && p.link == link {
				p.link = nil
				releasePlanner = true
			}
			p.mu.Unlock()
			link.releaseRef(p.s)
			if releasePlanner {
				link.releaseRef(p.s)
			}
			if stale {
				return nil, nil, context.Canceled
			}
			p.syncFailure(src, err, now)
			continue
		}
		wrapped := p.wrap(ctx, src, link, decoder, p.firstFrameDeadline(src.ID, len(candidates) > 1))
		p.mu.Lock()
		if p.openGen != gen {
			p.mu.Unlock()
			_ = wrapped.Close()
			link.releaseRef(p.s)
			return nil, nil, context.Canceled
		}
		racing := p.stalledAttempt != nil
		prev := p.lastOpened
		if !racing {
			p.lastOpened = src
		}
		link.uses++
		p.ladder.opened(id, now)
		p.attempt = &steadyAttempt{src: src, link: link, decoder: wrapped}
		p.mu.Unlock()
		if !racing && prev.ID != "" && prev.ID != src.ID {
			p.logger().Info("live input switched source", append([]any{"from", prev.ID, "to", src.ID}, p.s.duplicateFailoverLogAttrs(c, prev, src)...)...)
			p.s.emitSourceFailover(c, prev.ID, src.ID, "input failover", "broadcast")
		}
		p.logger().Info("live input opening", "input", id, "liveEdge", liveEdge, "format", link.format, "uses", link.uses)
		return wrapped, func() { link.releaseRef(p.s) }, nil
	}
	return nil, nil, fmt.Errorf("broadcast provider: %w", last)
}

func (p *providerBroadcastPlanner) syncFailure(src StreamSource, err error, now time.Time) {
	p.mu.Lock()
	if !p.firstAired {
		p.noteStartupFailure(src.ID, err)
	}
	if p.lastOpened.ID == "" {
		p.lastOpened = src
	}
	p.mu.Unlock()
	p.s.recordSourceHealth(p.channel.ID, src, err, false)
	candidates := sourceIDs(p.s.orderedSourceCandidates(p.channel))
	p.mu.Lock()
	p.ladder.opened(src.ID, now)
	p.retryAt = p.ladder.failed(src.ID, err, false, true, candidates, now)
	p.mu.Unlock()
}

func (p *providerBroadcastPlanner) firstFrameDeadline(id string, backup bool) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.firstFrameDeadlineLocked(id, backup)
}

func (p *providerBroadcastPlanner) firstFrameDeadlineLocked(id string, backup bool) time.Duration {
	switch {
	case !p.firstAired:
		if backup {
			return 2 * time.Second
		}
		return 4 * time.Second
	case !backup:
		return steadyFirstFrameAlone
	case id == p.ladder.source:
		return steadyFirstFrameRejoin
	}
	return steadyFirstFrameBackup
}

func (p *providerBroadcastPlanner) startDecoder(ctx context.Context, link *steadyLink, liveEdge bool) (transcode.Decoder, error) {
	if link.primedDecoder != nil {
		d := link.primedDecoder
		link.primedDecoder = nil
		return d, nil
	}
	input, err := p.s.relayInput(link.ss)
	if liveEdge {
		input, err = p.s.relayInputLive(link.ss)
	}
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	_, aired := p.lastAired[link.src.ID]
	p.mu.Unlock()
	reserve := p.s.stabilityReserve(p.channel) > 0
	return p.runner.StartDecoder(ctx, transcode.DecoderRequest{SessionID: link.ss.info.ID, InputURL: input, Format: p.format, Live: true, PrimeLive: !aired && !reserve, ReserveStart: reserve && !liveEdge, InputFormat: link.format})
}

func (p *providerBroadcastPlanner) wrap(ctx context.Context, src StreamSource, link *steadyLink, d transcode.Decoder, firstFrame time.Duration) *providerBroadcastDecoder {
	c := p.channel
	life, cancel := context.WithCancel(ctx)
	var reopen func() (transcode.Decoder, error)
	var provider servingProvider
	if link != nil {
		reopen = func() (transcode.Decoder, error) { return p.startDecoder(life, link, true) }
		if g := p.s.sessionGrant(link.ss); g != nil {
			provider = g.provider
		}
	}
	return &providerBroadcastDecoder{reopen: reopen, life: life, cancel: cancel, Decoder: d, planner: p, provider: provider, sourceID: src.ID, firstFrame: firstFrame, started: time.Now(), frame: time.Second * time.Duration(p.format.FPSDen) / time.Duration(p.format.FPSNum), ready: func() {}, healthy: func() {
		p.s.recordSourceHealth(c.ID, src, nil, true)
		p.mu.Lock()
		p.ladder.delivered(src.ID)
		p.mu.Unlock()
	}}
}

// linkTransportAlive reports that a link's provider connection is open or
// reconnecting by itself. Such a link is reused for every new decoder: a new
// link would open a second provider connection beside it, which a capped
// account (and many providers that do not say so) refuses or answers by
// cutting the first.
func (s *Service) linkTransportAlive(l *steadyLink) bool {
	if l == nil || l.closed.Load() {
		return false
	}
	h, ok := s.inputHealth(l.ss)
	return ok && !h.Failed
}

func (p *providerBroadcastPlanner) rememberManifestLocked(link *steadyLink) {
	if link == nil || link.ss == nil {
		return
	}
	link.ss.mu.Lock()
	raw := link.ss.inputURL
	link.ss.mu.Unlock()
	if cm := link.ss.cachedMasterFor(raw); cm != nil {
		p.manifest = cm
		return
	}
	p.s.inputRelay.mu.Lock()
	g := p.s.inputRelay.bySess[link.ss]
	p.s.inputRelay.mu.Unlock()
	if g != nil {
		if cm := g.peekManifest(g.rootURL()); cm != nil && cm.isMaster {
			p.manifest = cm
		}
	}
}

func (p *providerBroadcastPlanner) manifestForLocked(raw string) *cachedManifest {
	if p.direct != nil {
		return p.direct.manifestFor(raw)
	}
	if p.manifest != nil && p.manifest.isMaster && p.manifest.raw == raw {
		return p.manifest
	}
	return nil
}

// linkSteadySource reserves the provider account's connection, resolves the
// input and grants it to the loopback relay.
func (s *Service) linkSteadySource(ctx context.Context, c Channel, src StreamSource, reservation string, lead time.Duration) (*steadyLink, error) {
	return s.linkSteadySourceWithState(ctx, c, src, reservation, lead, nil, nil, nil)
}

func (s *Service) linkSteadySourceWithState(ctx context.Context, c Channel, src StreamSource, reservation string, lead time.Duration, direct *directStreamState, cached *cachedManifest, admission context.Context) (*steadyLink, error) {
	account := providerSourceID(c, src)
	release, err := s.trackBroadcastProvider(account, c.ID, reservation)
	if err != nil {
		return nil, err
	}
	var ss *session
	linked := false
	defer func() {
		if !linked {
			if ss != nil {
				s.revokeInput(ss)
				ss.cancel()
			}
			release()
		}
	}()
	var resolved string
	var headers http.Header
	var directGen uint64
	var manifest *cachedManifest
	var ephemeral bool
	if direct != nil {
		ephemeral = true
		dURL, dRawHdrs, dHdrs, dCM, dGen, dInvalid, _ := direct.current()
		if dInvalid || dURL == "" {
			attempt, cancelAttempt := context.WithTimeout(ctx, steadyOpenResolveTimeout)
			var rerr error
			dURL, dRawHdrs, dHdrs, dGen, rerr = direct.refresh(attempt, dGen)
			cancelAttempt()
			if rerr != nil {
				return nil, rerr
			}
			dCM = direct.manifestFor(dURL)
		}
		resolved, headers, directGen, manifest = dURL, dHdrs, dGen, dCM
		src.URL = resolved
		src.Headers = dRawHdrs
	} else {
		attempt, cancelAttempt := context.WithTimeout(ctx, steadyOpenResolveTimeout)
		var rerr error
		resolved, headers, rerr = s.ResolveStreamSource(attempt, c, src)
		cancelAttempt()
		if rerr != nil {
			return nil, rerr
		}
		if cached != nil && cached.isMaster && cached.raw == resolved {
			manifest = cached
		}
	}
	inputCtx, cancel := context.WithCancel(s.serviceContext())
	ss = &session{ctx: inputCtx, cancel: cancel, info: SessionInfo{ID: reservation, Channel: c}, ephemeral: ephemeral, direct: direct, sniffedManifest: manifest, inputSource: src, inputURL: resolved, inputHeaders: headers}
	grant := src
	grant.URL = resolved
	// The tune's first input also feeds the startup burst: it joins that much
	// deeper so the stability reserve is whole once the burst is out.
	var waited time.Duration
	if admission != nil {
		start := time.Now()
		err = s.withProviderCapacity(admission, c, src, func(admitCtx context.Context) error {
			return s.grantInputForContext(admitCtx, ss, grant, headers, true, lead)
		})
		waited = time.Since(start)
	} else {
		err = s.grantInputForContext(ctx, ss, grant, headers, true, lead)
	}
	if err != nil {
		return nil, err
	}
	sl := newSteadyLink(src, ss, release, s.cachedInputFormat(ss))
	sl.directGen = directGen
	sl.admissionWait = waited
	linked = true
	return sl, nil
}

// trackBroadcastProvider identifies idle outputs eligible for preemption.
// The input grant owns the capacity reservation.
func (s *Service) trackBroadcastProvider(account, channel, id string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broadcastProviders == nil {
		s.broadcastProviders = map[string]map[string]string{}
	}
	active := s.broadcastProviders[account]
	if active == nil {
		active = map[string]string{}
		s.broadcastProviders[account] = active
	}
	if _, exists := active[id]; exists {
		return nil, ErrProviderBusy
	}
	active[id] = channel
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			delete(active, id)
			if len(active) == 0 {
				delete(s.broadcastProviders, account)
			}
			s.mu.Unlock()
		})
	}, nil
}

// providerBroadcastDecoder adapts one provider decoder to the channel clock:
// it never replays media this source already aired, bounds the wait for the
// first picture, and reports delivery and stress to the planner.
type providerBroadcastDecoder struct {
	provider servingProvider
	transcode.Decoder
	planner    *providerBroadcastPlanner
	sourceID   string
	once       sync.Once
	ready      func()
	healthy    func()
	firstFrame time.Duration
	started    time.Time
	frame      time.Duration
	passed     bool
	skipped    int
	decodedAt  atomic.Int64
	aired      atomic.Int64
	healthOnce sync.Once
	// Stream-change splicing (broadcast_splice.go): reopen starts a decoder
	// on the same input at its live edge; life bounds the frame pumps.
	reopen        func() (transcode.Decoder, error)
	life          context.Context
	cancel        context.CancelFunc
	cur, cand     *decoderPump
	lastCandidate time.Time
	changeAired   int64 // aired ticks at the previous distinct change
}

func (d *providerBroadcastDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	if !d.passed && d.firstFrame > 0 {
		remaining := d.firstFrame - time.Since(d.started)
		if remaining <= 0 {
			return transcode.RawFrame{}, errSteadyFirstFrame
		}
		bounded, cancel := context.WithTimeoutCause(ctx, remaining, errSteadyFirstFrame)
		defer cancel()
		frame, err := d.next(bounded)
		if err != nil && ctx.Err() == nil && errors.Is(context.Cause(bounded), errSteadyFirstFrame) {
			return frame, errSteadyFirstFrame
		}
		return frame, err
	}
	return d.next(ctx)
}

func (d *providerBroadcastDecoder) next(ctx context.Context) (transcode.RawFrame, error) {
	for {
		frame, err := d.pull(ctx)
		if err != nil {
			return frame, err
		}
		if !frame.SourcePTSValid {
			return transcode.RawFrame{}, fmt.Errorf("live decoder did not report a source timestamp")
		}
		d.decodedAt.Store(time.Now().UnixNano())
		if !d.passed {
			// A reopened input may start inside media this source already aired
			// (a provider playlist that stopped advancing, a reserve backlog).
			// Within one decoder the media is continuous, including provider
			// discontinuities, so the floor applies only until it is passed.
			// A source whose clock restarted far below the floor is new media.
			d.planner.mu.Lock()
			floor, seen := d.planner.lastAired[d.sourceID]
			d.planner.mu.Unlock()
			if seen && frame.SourcePTS <= floor && floor-frame.SourcePTS < steadyClockReset && time.Duration(d.skipped)*d.frame < steadyReplayLimit {
				d.skipped++
				continue
			}
			d.passed = true
			details := ""
			if source, ok := d.Decoder.(interface{ Diagnostics() string }); ok {
				details = source.Diagnostics()
			}
			d.planner.logger().Debug("live input first picture", "input", d.sourceID, "waited", time.Since(d.started), "skippedAired", d.skipped, "pts", frame.SourcePTS, "floor", floor, "decoder", details)
		}
		d.once.Do(d.ready)
		return frame, nil
	}
}

func (d *providerBroadcastDecoder) Aired(frame transcode.RawFrame) {
	d.planner.mu.Lock()
	d.planner.firstAired = true
	d.planner.startupFailures = nil
	if !frame.SourcePTSValid {
		d.planner.mu.Unlock()
		return
	}
	if d.planner.lastAired == nil {
		d.planner.lastAired = map[string]time.Duration{}
	}
	d.planner.lastAired[d.sourceID] = frame.SourcePTS
	if d.provider.sourceID != "" {
		d.planner.provider.set(d.provider)
	}
	d.planner.mu.Unlock()
	if n := d.aired.Add(1); d.healthy != nil && time.Duration(n)*d.frame >= steadyDelivered {
		d.healthOnce.Do(d.healthy)
	}
}

// delivered reports that this input aired enough media to count as healthy.
func (d *providerBroadcastDecoder) delivered() bool {
	return d != nil && time.Duration(d.aired.Load())*d.frame >= steadyDelivered
}

func (d *providerBroadcastDecoder) sinceDecoded(now time.Time) time.Duration {
	at := d.decodedAt.Load()
	if at == 0 {
		return 0
	}
	return now.Sub(time.Unix(0, at))
}

// stressed reports a live input that stopped producing pictures.
func (d *providerBroadcastDecoder) stressed(now time.Time) bool {
	return d.sinceDecoded(now) > steadyStress
}

// starved reports an input that delivered pictures and then none for
// steadyStarved: a ready standby replaces it rather than holding a picture.
func (d *providerBroadcastDecoder) starved(now time.Time) bool {
	return d.sinceDecoded(now) > steadyStarvedAfter
}

func (d *providerBroadcastDecoder) Diagnostics() string {
	d.planner.mu.Lock()
	floor := d.planner.lastAired[d.sourceID]
	d.planner.mu.Unlock()
	details := ""
	if source, ok := d.Decoder.(interface{ Diagnostics() string }); ok {
		details = source.Diagnostics()
	}
	return fmt.Sprintf("lastAiredSourcePTS=%s %s", floor, details)
}

func (p *providerBroadcastPlanner) broadcastLogger() *slog.Logger {
	return p.s.o.Logger.With("channel", p.channel.ID)
}

// IPTV outputs retain EPG policy at exact frame-clock boundaries as well.
// Fetching/hydrating the bounded upcoming window is background work, as is
// the hot-standby watch.
func (p *providerBroadcastPlanner) startPlanner(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	p.guideCancel = cancel
	p.guideDone = make(chan struct{})
	if p.ladder == nil {
		p.ladder = newSteadyLadder()
	}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			now := p.s.o.Now()
			until := now.Add(2 * time.Minute)
			ps, err := p.s.programmes(ctx, p.channel, now, until)
			if err == nil {
				for i := range ps {
					ps[i].policyTag = p.s.programmePolicy(ctx, ps[i])
				}
				p.mu.Lock()
				p.guideFrom, p.guideUntil, p.guide = now, until, ps
				p.mu.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	go func() { defer workers.Done(); p.watchStandby(ctx) }()
	go func() { workers.Wait(); close(p.guideDone) }()
}

func (p *providerBroadcastPlanner) closePlanner() {
	p.guideCancel()
	<-p.guideDone
	p.mu.Lock()
	p.openGen++
	a := p.attempt
	if a == nil {
		a = p.stalledAttempt
	}
	if a != nil {
		// Report how this output's input ended to the source history.
		outcome := "ok"
		if !a.decoder.delivered() {
			outcome = "failed"
		}
		defer p.s.noteSourceMode(p.channel.ID, a.src, "steady", outcome)
	}
	var stalledLink *steadyLink
	if p.stalledAttempt != nil && p.stalledAttempt.link != p.link {
		stalledLink = p.stalledAttempt.link
	}
	link, primed, standby := p.link, p.primed, p.standby
	p.link, p.primed, p.standby, p.stalledAttempt = nil, nil, nil, nil
	p.mu.Unlock()
	standby.close(p.s)
	primed.close(p.s)
	stalledLink.close(p.s)
	link.close(p.s)
}

// A reconnecting screen is useful after playback starts, but cannot make a
// cold tune successful. Only an actual provider picture satisfies readiness.
func (p *providerBroadcastPlanner) awaitFirstPicture(ctx context.Context) error {
	candidates := len(p.s.orderedSourceCandidates(p.channel))
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		p.mu.Lock()
		ready := p.firstAired
		failures := append([]TuneInputFailure(nil), p.startupFailures...)
		p.mu.Unlock()
		if ready {
			return nil
		}
		if ctx.Err() != nil || candidates > 0 && len(failures) >= candidates {
			reason := ctx.Err()
			if reason == nil {
				reason = core.ErrNotAvailable
			}
			detail := *Diagnose("first_picture", reason)
			if len(failures) > 0 && !errors.Is(reason, context.Canceled) {
				last := failures[len(failures)-1]
				detail.Code, detail.Stage, detail.UpstreamStatus = last.Code, last.Stage, last.UpstreamStatus
			}
			detail.Inputs = failures
			return &detail
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
}

// Caller holds mu. Retain one safe reason per input, only before any media airs.
func (p *providerBroadcastPlanner) noteStartupFailure(id string, err error) {
	failure := inputFailure(id, err)
	for i := range p.startupFailures {
		if p.startupFailures[i].Input == id {
			p.startupFailures[i] = failure
			return
		}
	}
	if len(p.startupFailures) < MaxChannelInputs+1 {
		p.startupFailures = append(p.startupFailures, failure)
	}
}
