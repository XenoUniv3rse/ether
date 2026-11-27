package livetv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// HLS passthrough on an owned timeline. One producer per channel (and
// variant class) fetches each provider segment once into the timeline,
// whose window every viewer of the channel reads: N viewers make one set of
// provider requests. Segment bytes are never altered.

type ownedServiceState struct {
	mu       sync.Mutex
	channels map[string]*ownedShared
	relays   map[relayOutputKey]*sharedRelay
	slates   slateCache
}

func (s *Service) owned() *ownedServiceState {
	st := &s.direct
	st.mu.Lock()
	if st.channels == nil {
		st.channels = map[string]*ownedShared{}
		st.slates.entries = map[string]*slateEntry{}
		st.slates.encode = make(chan struct{}, 1)
	}
	st.mu.Unlock()
	return st
}

// ownedShared is one channel's shared passthrough timeline.
type ownedShared struct {
	key    string
	tl     *ownedTimeline
	p      *passthroughProducer
	ctx    context.Context
	cancel context.CancelFunc
	refs   int
	done   chan struct{}
	// sessions are the viewers on this timeline (guarded by the registry
	// lock); a hand-off to Steady runs under one of them.
	sessions map[*session]struct{}
}

// ownedLease is a session's hold on a timeline.
type ownedLease struct {
	tl       *ownedTimeline
	relay    *relayProducer // set for relay sessions
	provider func() *providerChange
	once     sync.Once
	release  func()
}

func (l *ownedLease) close() {
	if l != nil {
		l.once.Do(l.release)
	}
}

var errNeedsRelay = errors.New("livetv: provider variants need the relay")

func (s *Service) acquirePassthrough(ss *session) *ownedLease {
	c := ss.info.Channel
	capped := ss.ephemeral || s.sourceConnectionLimit(providerSourceID(c, ss.inputSource)) > 0
	key := c.ID
	if !capped {
		// A capped provider account gets one timeline per channel, whatever
		// the viewers' capabilities: it is one provider connection.
		key += "|" + variantClass(ss.caps)
	}
	if c.Personal != nil {
		key += "|" + strconv.FormatInt(ss.info.UserID, 10)
	}
	st := s.owned()
	slates, reserve := s.slateFor(c), s.stabilityReserve(c)
	st.mu.Lock()
	sh := st.channels[key]
	for sh != nil && sh.ctx.Err() != nil {
		st.mu.Unlock()
		<-sh.done
		st.mu.Lock()
		if st.channels[key] == sh {
			delete(st.channels, key)
		}
		sh = st.channels[key]
	}
	if sh == nil {
		ctx, cancel := context.WithCancel(s.serviceContext())
		log := s.logger().With("channel", c.ID, "owned", "passthrough")
		tl := newOwnedTimeline(c.ID, log, nil, reserve, slates)
		s.enableOwnedTimeshift(tl, c)
		policy := s.latencyPolicy()
		tl.windowSpan, tl.startBehind = policy.PlaylistWindow(), policy.TargetLatency()
		tl.setHint(slateProfileFromInfo(c.ID, ss.info.MediaInfo, "ts"))
		ss.mu.Lock()
		src := ss.inputSource
		raw := ss.inputURL
		hdrs := ss.inputHeaders.Clone()
		ephemeral := ss.ephemeral
		direct := ss.direct
		ss.mu.Unlock()
		var directGen uint64
		cm := ss.sniffedManifestFor(raw)
		if direct != nil {
			dURL, dRawHdrs, dHdrs, dCM, dGen, _, _ := direct.current()
			if dURL != "" && (dURL == raw || ephemeral) {
				raw = dURL
				src.URL = dURL
				src.Headers = dRawHdrs
				hdrs = dHdrs
				cm = dCM
				directGen = dGen
			}
		}
		p := &passthroughProducer{s: s, tl: tl, c: c, caps: ss.caps, log: log,
			src: src, raw: raw, headers: hdrs,
			ephemeral: ephemeral, direct: direct, directGen: directGen, cachedManifest: cm,
			suggest: make(chan sourceChoice, 1)}
		sh = &ownedShared{key: key, tl: tl, p: p, ctx: ctx, cancel: cancel, done: make(chan struct{})}
		st.channels[key] = sh
		p.viewer = func() *session {
			st.mu.Lock()
			defer st.mu.Unlock()
			for v := range sh.sessions {
				return v
			}
			return nil
		}
		go tl.run(ctx)
		go func() {
			defer close(sh.done)
			defer func() {
				if recover() != nil {
					tl.fail(core.ErrNotAvailable)
				}
			}()
			p.run(ctx)
		}()
		log.Info("owned passthrough timeline created", "reserve", tl.reserve, "class", key)
	} else {
		// This tune just proved a source works. A struggling timeline
		// takes it instead of waiting for its own ladder.
		select {
		case sh.p.suggest <- sourceChoice{src: ss.inputSource, raw: ss.inputURL, headers: ss.inputHeaders.Clone()}:
		default:
		}
	}
	sh.refs++
	if sh.sessions == nil {
		sh.sessions = map[*session]struct{}{}
	}
	sh.sessions[ss] = struct{}{}
	st.mu.Unlock()
	return &ownedLease{tl: sh.tl, provider: sh.p.servingProvider, release: func() {
		st.mu.Lock()
		delete(sh.sessions, ss)
		sh.refs--
		last := sh.refs == 0
		if last {
			sh.cancel()
		}
		st.mu.Unlock()
		if last {
			<-sh.done
			st.mu.Lock()
			if st.channels[sh.key] == sh {
				delete(st.channels, sh.key)
			}
			st.mu.Unlock()
		}
	}}
}

// providerOrigin reads a provider's media playlist and resources with the
// channel's headers (origin-scoped) and media identity (every hop).
type providerOrigin struct {
	s        *Service
	mediaURL string
	source   string
	headers  http.Header
	public   http.Header
	name     string
}

func (o *providerOrigin) label() string { return o.name }

func (o *providerOrigin) media(ctx context.Context) ([]byte, *url.URL, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	return o.fetch(ctx, o.mediaURL, 0, 0, 4<<20)
}

func (o *providerOrigin) get(ctx context.Context, uri string, n, off int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	b, _, err := o.fetch(ctx, uri, n, off, 64<<20)
	return b, err
}

func (o *providerOrigin) fetch(ctx context.Context, raw string, n, off int64, limit int64) ([]byte, *url.URL, error) {
	in := &http.Request{Header: http.Header{}}
	if n > 0 {
		in.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	}
	res, err := o.s.upstreamWithHeaders(ctx, raw, in, o.headers, o.source, o.public)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return nil, nil, providerResponseError(res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(body)) > limit {
		return nil, nil, fmt.Errorf("livetv: upstream object exceeds %d bytes", limit)
	}
	if n > 0 && res.StatusCode == http.StatusOK {
		// The server ignored the range: cut it out ourselves.
		if off+n > int64(len(body)) {
			return nil, nil, fmt.Errorf("livetv: upstream byte range out of bounds")
		}
		body = body[off : off+n]
	}
	return body, res.Request.URL, nil
}

type passthroughProducer struct {
	s       *Service
	tl      *ownedTimeline
	c       Channel
	caps    core.ClientCaps
	log     *slog.Logger
	src     StreamSource
	raw     string // resolved source URL
	headers http.Header

	ephemeral           bool
	direct              *directStreamState
	directGen           uint64
	cachedManifest      *cachedManifest
	directFailoverTried bool

	suggest      chan sourceChoice
	provider     providerTracker
	providerFeed atomic.Pointer[steadyFeed]
	login        *loginLease // held by the producer, independently of its viewers
	releaseSlot  func()
	inputKey     string
	inputAccount string

	rounds int // failover rounds since the last progress

	ingest     *hlsIngest
	errs       int
	reresolved bool
	healthAt   time.Time
	// stalls counts ladder episodes since the last history report.
	stalls     int64
	reportedAt time.Time
	// viewer returns a session on this timeline; the Steady hand-off fields
	// are guarded by steadyMu (the loop and the hand-off goroutine share
	// them).
	viewer        func() *session
	steadyMu      sync.Mutex
	steady        SteadyFeed
	steadyReady   SteadyFeed
	steadyBusy    bool
	steadyRetryAt time.Time
	steadyClass   string
	churnAt       []time.Time
	seenSplices   int
	seenSkipped   int64
}

func (p *passthroughProducer) manifestFor(raw string) *cachedManifest {
	if raw == "" {
		return nil
	}
	if p.direct != nil {
		if dm := p.direct.manifestFor(raw); dm != nil {
			return dm
		}
		if _, _, _, _, _, invalidated, _ := p.direct.current(); invalidated {
			return nil
		}
	}
	if p.cachedManifest != nil && p.cachedManifest.raw == raw && p.cachedManifest.hls {
		return p.cachedManifest
	}
	return nil
}

func (p *passthroughProducer) setCachedMaster(cm *cachedManifest) {
	if cm == nil || !cm.isMaster {
		return
	}
	p.cachedManifest = cm
	if p.direct != nil {
		p.direct.setManifest(cm)
	}
}

func (p *passthroughProducer) invalidateDirect(status int) {
	p.cachedManifest = nil
	if p.direct != nil {
		p.direct.invalidate(p.directGen, status)
	}
}

func (p *passthroughProducer) refreshDirect(ctx context.Context) (StreamSource, string, http.Header, error) {
	if p.direct == nil {
		return p.src, p.raw, p.headers, providerResponseError(http.StatusForbidden)
	}
	dURL, dRawHdrs, dHdrs, dGen, err := p.direct.refresh(ctx, p.directGen)
	if err != nil {
		return p.src, p.raw, p.headers, err
	}
	src := p.src
	src.URL = dURL
	src.Headers = dRawHdrs
	p.directGen = dGen
	p.cachedManifest = p.direct.manifestFor(dURL)
	return src, dURL, dHdrs, nil
}

// open points the producer at a source: it selects the best current variant
// and rebases the ingest. keep preserves the ingest
// position when the source is the same stream (a re-resolution).
func (p *passthroughProducer) open(ctx context.Context, src StreamSource, raw string, headers http.Header, keep bool) error {
	source := providerSourceID(p.c, src)
	entry, err := p.s.sourceEntry(ctx, source)
	if err != nil {
		return err
	}
	key := providerInputKey(raw, headers, src.Headers)
	if p.inputKey != key || p.inputAccount != source {
		p.releaseInput()
	}
	release, err := p.s.holdProviderInput(source, key, entry.limit)
	if err != nil {
		return err
	}
	accepted := false
	var lease *loginLease
	defer func() {
		if !accepted {
			p.s.releaseLogin(lease)
			if release != nil {
				release()
			}
		}
	}()
	lease, err = p.s.leaseLoginFor(ctx, entry, source, raw, headers, key)
	if err != nil {
		return err
	}
	target, upstreamHeaders := raw, headers
	if lease != nil {
		target, upstreamHeaders = lease.raw, lease.headers
	}
	provider := providerForInput(entry, src, lease)
	if last := p.provider.current.Load(); last != nil && last.active.loginKey != provider.loginKey {
		keep = false
	}
	if err = p.openAttempt(ctx, src, target, upstreamHeaders, keep, false); err != nil {
		p.noteLoginFailure(lease, err)
		return err
	}
	p.releaseInput()
	p.login, p.releaseSlot = lease, release
	p.inputKey, p.inputAccount = key, source
	accepted = true
	if lease != nil {
		// The next resolution starts with the original stream coordinates.
		p.raw, p.headers = raw, headers
	}
	p.provider.set(provider)
	return nil
}

func (p *passthroughProducer) openAttempt(ctx context.Context, src StreamSource, raw string, headers http.Header, keep, refreshed bool) error {
	o := &providerOrigin{s: p.s, mediaURL: raw, source: raw, headers: headers, public: redirectMediaHeaders(src.Headers), name: "provider:" + src.ID}
	var (
		body []byte
		base *url.URL
		err  error
	)
	if cm := p.manifestFor(raw); cm != nil && cm.hls {
		if !cm.isMaster {
			p.cachedManifest = nil
		}
		body, base = cm.body, cm.base
	} else {
		p.cachedManifest = nil
		body, base, err = o.media(ctx)
		if err != nil {
			if status, ok := expiredProviderStatus(err); ok {
				p.invalidateDirect(status)
				if !refreshed && p.direct != nil && p.direct.canResolve() {
					p.directFailoverTried = true
					nextSrc, nextRaw, nextHdrs, rerr := p.refreshDirect(ctx)
					if rerr != nil {
						return rerr
					}
					return p.openAttempt(ctx, nextSrc, nextRaw, nextHdrs, keep, true)
				}
			}
			return err
		}
		if isMasterPlaylist(body) {
			cm := newCachedManifest(raw, body, base, true)
			p.setCachedMaster(cm)
			body, base = cm.body, cm.base
		}
	}
	if isMasterPlaylist(body) {
		m := parseMasterPlaylist(body, base)
		v, ok := pickVariant(m, p.caps)
		// The relay can convert a codec the viewer cannot play. Do not
		// discard its audible program in favor of compatible silent video.
		inputCaps := p.caps
		inputCaps.VideoCodecs = nil
		best, _ := pickVariant(m, inputCaps)
		needsConversion := variantHasAudio(best) && !variantHasAudio(v)
		overCeiling := (p.caps.MaxHeight > 0 && v.height > p.caps.MaxHeight) ||
			(p.caps.MaxBitrateKbps > 0 && v.bandwidth > p.caps.MaxBitrateKbps*1000)
		if !ok || needsConversion || overCeiling || (v.audio != "" && m.demuxedAudio[v.audio]) {
			p.log.Info("owned passthrough requires remux", "height", v.height, "bandwidth", v.bandwidth, "audio_group", v.audio)
			return errNeedsRelay
		}
		o.mediaURL = v.uri
		p.log.Info("owned passthrough variant selected", "bandwidth", v.bandwidth, "height", v.height, "codecs", v.codecs, "variants", len(m.variants))
	} else if base != nil {
		o.mediaURL = base.String()
	}
	seed, seedBase := body, base
	if isMasterPlaylist(body) {
		seed, seedBase = nil, nil
	}
	reserve := max(p.tl.reserve-p.tl.depth(), 0)
	switch {
	case p.ingest == nil:
		p.ingest = newHLSIngest(p.tl, o, p.log, reserve, 3)
	case keep:
		p.ingest.origin = o
	default:
		p.ingest.reserve = reserve
		p.ingest.rebase(o)
		p.tl.splice(false)
	}
	p.ingest.seed, p.ingest.seedBase = seed, seedBase
	p.src, p.raw, p.headers = src, raw, headers
	if refreshed && p.direct != nil {
		p.provider.set(providerForInput(authEntry{}, src, nil))
	}
	return nil
}

func (p *passthroughProducer) noteLoginFailure(lease *loginLease, err error) {
	var te *TuneError
	if lease != nil && errors.As(err, &te) &&
		(te.UpstreamStatus == http.StatusUnauthorized || te.UpstreamStatus == http.StatusForbidden) {
		p.s.failedLogin(lease)
	}
}

func (p *passthroughProducer) servingProvider() *providerChange {
	return steadyProvider(p.providerFeed.Load(), p.provider.current.Load())
}

func (p *passthroughProducer) releaseInput() {
	p.s.releaseLogin(p.login)
	p.login = nil
	if p.releaseSlot != nil {
		p.releaseSlot()
		p.releaseSlot = nil
	}
	p.inputKey, p.inputAccount = "", ""
}

func (p *passthroughProducer) run(ctx context.Context) {
	defer p.releaseInput()
	err := p.open(ctx, p.src, p.raw, p.headers, false)
	if errors.Is(err, errNeedsRelay) {
		p.tl.fail(errNeedsRelay)
		return
	}
	if err != nil && ctx.Err() == nil {
		p.log.Warn("owned passthrough could not open its source", Diagnose("source_open", err).LogAttrs()...)
		if !p.failover(ctx) {
			p.tl.fail(Diagnose("source_open", err))
		}
	}
	warmed := false
	backoff := 250 * time.Millisecond
	defer p.reportHistory()
	defer p.closeSteady()
	for ctx.Err() == nil {
		if p.takeSteady() {
			continue
		}
		if p.onSteady() {
			// Steady owns recovery for the whole channel now; the
			// timeline's deadline and slate still guard it.
			p.ingest.step(ctx)
			sleepCtx(ctx, 250*time.Millisecond)
			continue
		}
		if time.Since(p.reportedAt) >= 30*time.Second {
			p.reportHistory()
		}
		if p.ingest == nil || p.inputKey == "" {
			if !sleepCtx(ctx, 2*time.Second) || !p.failover(ctx) {
				continue
			}
		}
		if p.login != nil && p.login.lost.Load() {
			p.releaseInput()
			if !p.failover(ctx) {
				sleepCtx(ctx, p.roundDelay())
			}
			continue
		}
		progress, err := p.ingest.step(ctx)
		if ctx.Err() != nil {
			return
		}
		p.watchChurn(ctx)
		now := time.Now()
		if progress {
			p.errs, p.reresolved, p.rounds, p.directFailoverTried, backoff = 0, false, 0, false, 250*time.Millisecond
			if now.Sub(p.healthAt) > time.Minute {
				p.healthAt = now
				p.s.recordSourceHealth(p.c.ID, p.src, nil, true)
			}
			if !warmed && p.tl.started() {
				warmed = true
				p.tl.warmSlate()
			}
		}
		if err != nil {
			p.errs++
		}
		td := p.tl.targetDuration()
		if td <= 0 {
			td = max(p.ingest.target, 2*time.Second)
		}
		stale := p.ingest.stale(now)
		var segErr *segmentError
		isSegErr := errors.As(err, &segErr)
		if status, ok := expiredProviderStatus(err); ok && (!isSegErr || status == http.StatusForbidden) {
			p.invalidateDirect(status)
		}
		if !isSegErr {
			p.noteLoginFailure(p.login, err)
		}
		hard := !isSegErr && isHardProviderError(err) || errors.Is(err, errOriginEnded) || errors.Is(err, errMasterSource)
		if err != nil || stale >= td/2 {
			select {
			case choice := <-p.suggest:
				if choice.src.ID != p.src.ID || choice.raw != p.raw {
					prev := p.src
					if oerr := p.open(ctx, choice.src, choice.raw, choice.headers, false); oerr == nil {
						p.log.Warn("owned passthrough took a viewer's freshly tuned source", append([]any{"input", choice.src.ID}, p.s.duplicateFailoverLogAttrs(p.c, prev, choice.src)...)...)
						p.errs, p.reresolved = 0, false
						continue
					}
				}
			default:
			}
		}
		switch {
		case !p.tl.started() && (hard || p.errs >= 2 || stale >= td/2):
			// Nothing published yet: a viewer is waiting on a cold tune
			// that just resolved this source. Go straight to the next one.
			p.s.recordSourceHealth(p.c.ID, p.src, firstErr(err, errOriginStalled), false)
			p.failover(ctx)
			sleepCtx(ctx, p.roundDelay())
			p.reresolved, p.errs = false, 0
			continue
		case !p.reresolved && (hard || p.errs >= 3 || stale >= td):
			// Second rung: re-resolve the same source (fresh tokens,
			// redirects and plugin/Xtream resolution).
			p.reresolved = true
			p.stalls++
			p.churn(ctx, "stall")
			p.log.Warn("owned passthrough re-resolving source", "stale", stale, "errors", p.errs, "err", errString(err))
			if rerr := p.reresolve(ctx); rerr != nil {
				p.log.Warn("owned passthrough re-resolution failed", Diagnose("source_resolve", rerr).LogAttrs()...)
			}
			continue
		case p.reresolved && (hard || p.errs >= 6 || stale >= 2*td):
			// Third rung: the next source in the channel's health order.
			// Every source failing fast must not become a request storm:
			// rounds without progress back off up to ten seconds while
			// slate holds the playlist.
			p.s.recordSourceHealth(p.c.ID, p.src, firstErr(err, errOriginStalled), false)
			p.failover(ctx)
			sleepCtx(ctx, p.roundDelay())
			p.reresolved, p.errs = false, 0
			continue
		}
		wait := 500 * time.Millisecond
		if err != nil {
			wait = backoff
			backoff = min(backoff*2, 2*time.Second)
		} else if progress {
			// The next segment is due one segment duration after this one
			// was listed; poll just before then.
			wait = max(p.ingest.lastDur*8/10, 250*time.Millisecond)
		}
		sleepCtx(ctx, wait)
	}
}

var errOriginStalled = errors.New("livetv: live source stalled")

// roundDelay paces failover rounds that brought no media: the first after
// progress is immediate, then 1 s, 2 s ... up to 10 s, while the reserve and
// slate hold the playlist.
func (p *passthroughProducer) roundDelay() time.Duration {
	d := min(time.Duration(p.rounds)*time.Second, 10*time.Second)
	p.rounds++
	return d
}

type sourceChoice struct {
	src     StreamSource
	raw     string
	headers http.Header
}

// reportHistory hands what the passthrough received since the last report
// to the source's history: media, skipped segments, stall episodes. Owned
// passthrough reads the provider directly, not through the input relay that
// records the other paths.
func (p *passthroughProducer) reportHistory() {
	p.reportedAt = time.Now()
	if p.ingest == nil {
		return
	}
	ev := SourceEvidence{Media: p.ingest.media, DamageEvents: p.ingest.skipped, Stalls: p.stalls}
	p.ingest.media, p.ingest.skipped, p.stalls = 0, 0, 0
	if ev == (SourceEvidence{}) {
		return
	}
	p.s.noteSourceEvidence(p.c.ID, p.src, ev)
}

func (p *passthroughProducer) reresolve(ctx context.Context) error {
	rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if p.direct != nil {
		dURL, dRawHdrs, dHdrs, dCM, dGen, dInvalid, _ := p.direct.current()
		if !dInvalid && dCM != nil && dCM.isMaster && dURL != "" {
			src := p.src
			src.URL = dURL
			src.Headers = dRawHdrs
			p.directGen = dGen
			p.cachedManifest = dCM
			return p.open(rctx, src, dURL, dHdrs, true)
		}
		src, raw, headers, err := p.refreshDirect(rctx)
		if err != nil {
			return err
		}
		return p.open(rctx, src, raw, headers, true)
	}
	raw, headers, err := p.s.ResolveStreamSource(rctx, p.c, p.src)
	if err != nil {
		return err
	}
	return p.open(rctx, p.src, raw, headers, true)
}

// failover tries every other enabled source in health order, then the
// current one again. It reports whether a source opened.
func (p *passthroughProducer) failover(ctx context.Context) bool {
	if p.ephemeral {
		if p.direct != nil && p.direct.canResolve() && !p.directFailoverTried {
			p.directFailoverTried = true
			rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			src, raw, headers, err := p.refreshDirect(rctx)
			if err == nil {
				err = p.open(rctx, src, raw, headers, p.tl.started())
			}
			cancel()
			if err == nil {
				return true
			}
			if !errors.Is(err, context.Canceled) {
				p.log.Warn("owned passthrough direct re-resolve failed", Diagnose("source_failover", err).LogAttrs()...)
			}
		}
		return false
	}
	failed := p.src
	candidates := append(p.s.orderedSourceCandidates(p.c, failed.ID), failed)
	for _, src := range candidates {
		if ctx.Err() != nil {
			return false
		}
		rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		raw, headers, err := p.s.ResolveStreamSource(rctx, p.c, src)
		if err == nil {
			err = p.open(rctx, src, raw, headers, src.ID == failed.ID)
		}
		cancel()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				p.s.recordSourceHealth(p.c.ID, src, err, false)
			}
			p.log.Warn("owned passthrough source unavailable", append(Diagnose("source_failover", err).LogAttrs(), "input", src.ID)...)
			continue
		}
		if src.ID != failed.ID {
			p.s.emitSourceFailover(p.c, failed.ID, src.ID, "source failed", "passthrough")
		}
		p.log.Warn("owned passthrough switched source", append([]any{"from", failed.ID, "to", src.ID}, p.s.duplicateFailoverLogAttrs(p.c, failed, src)...)...)
		return true
	}
	return false
}

func isHardProviderError(err error) bool {
	var te *TuneError
	if !errors.As(err, &te) {
		return false
	}
	switch te.UpstreamStatus {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return true
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return Diagnose("media_fetch", err).Code
}
