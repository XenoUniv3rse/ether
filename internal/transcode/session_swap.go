package transcode

import (
	"context"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"os"
	"strings"
	"time"
)

// SwapInput implements core.HLSSession: restart on a new input at `at`
// (mid-stream failover). Segments from `at` on are discarded and produced
// again from the new input; numbering stays continuous.
func (s *Session) SwapInput(ctx context.Context, inputURL string, info core.MediaInfo, at time.Duration) error {
	return s.swapInput(ctx, inputURL, info, at, false)
}

func (s *Session) SwapInputBuffered(ctx context.Context, inputURL string, info core.MediaInfo, at time.Duration) error {
	return s.swapInput(ctx, inputURL, info, at, true)
}

func (s *Session) swapInput(ctx context.Context, inputURL string, info core.MediaInfo, at time.Duration, preserve bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if inputURL == "" {
		return errors.New("transcode: SwapInput: empty input URL")
	}
	if err := s.CheckSwap(info); err != nil {
		return err
	}
	// Decode a tiny bounded sample outside the session lock. Only proven
	// embedded side bars matching the existing picture are removed.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("transcode: session closed")
	}
	if s.clientGoneLocked(time.Now()) {
		s.sessionExhausted = true
		s.inputFailed = true
		s.retireSessionAsync()
		s.mu.Unlock()
		return errSessionStopped
	}
	if s.liveRejected[inputURL] {
		s.mu.Unlock()
		return fmt.Errorf("transcode: source exceeds fixed live target; new session required")
	}
	if s.headerRejected[inputURL] {
		s.mu.Unlock()
		return fmt.Errorf("transcode: input rejected after deterministic header failure")
	}
	if s.failedInputs[inputURL] {
		s.mu.Unlock()
		return fmt.Errorf("transcode: input rejected after encoder failure")
	}
	if s.sessionExhausted || (!s.liveInput && s.totalFailures >= maxSessionFailures) {
		s.sessionExhausted = true
		s.inputFailed = true
		s.mu.Unlock()
		return errSessionExhausted
	}
	if len(s.headerRestarts) >= 8 && time.Since(s.headerLastFailure) < time.Minute {
		s.mu.Unlock()
		return fmt.Errorf("transcode: header restart limit reached")
	}
	retryAt := time.Time{}
	if !s.headerLastFailure.IsZero() {
		retryAt = s.headerLastFailure.Add(headerRecoveryDelay(len(s.headerRestarts)))
	}
	if !s.liveInput && !s.lastFailureAt.IsZero() && s.totalFailures > 0 {
		if cand := s.lastFailureAt.Add(s.retryBackoffLocked(s.totalFailures)); cand.After(retryAt) {
			retryAt = cand
		}
	}
	var cancel <-chan struct{}
	if s.encoder != nil {
		cancel = s.encoder.cancel
	}
	before := s.recoveryBufferLocked()
	previousInput := s.input
	stageDecision := s.decision
	if inputURL != previousInput {
		stageDecision = adaptToContract(s.audioMatchBaseDecision(), s.info, info, s.container, s.contract)
	}
	s.mu.Unlock()
	if delay := time.Until(retryAt); !retryAt.IsZero() && delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-cancel:
			timer.Stop()
			return errors.New("transcode: session closed")
		}
		timer.Stop()
	}
	sampleAt := at
	if preserve {
		if before.ReadyUntil > sampleAt && before.ReadyUntil < info.Duration {
			sampleAt = before.ReadyUntil
		}
	}
	crop, err := s.swapPictureCrop(ctx, inputURL, info, sampleAt)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	info = s.m.stageExternalSubtitle(s.dir, info, "", stageDecision)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("transcode: session closed")
	}
	if s.clientGoneLocked(time.Now()) {
		s.sessionExhausted = true
		s.inputFailed = true
		s.retireSessionAsync()
		return errSessionStopped
	}
	if s.failedInputs[inputURL] {
		return fmt.Errorf("transcode: input rejected after encoder failure")
	}
	if s.sessionExhausted || (!s.liveInput && s.totalFailures >= maxSessionFailures) {
		s.sessionExhausted = true
		s.inputFailed = true
		return errSessionExhausted
	}
	if s.recoverySeekAt != before.SeekAt || s.input != previousInput {
		return core.ErrReplacementSuperseded
	}
	// The caller verifies identity alignment, including the buffered boundary.
	// Different trailing credits or container rounding need not discard media.
	preserve = preserve && info.Duration > 0
	if preserve {
		b := s.recoveryBufferLocked()
		// Preserve at least one whole published segment; never seek past EOF.
		preserve = b.ReadyUntil > b.Position && b.ReadyUntil < info.Duration
		if preserve {
			at = b.ReadyUntil
		}
	}
	seg := s.segForOffsetLocked(at)
	if !s.liveInput && s.segFailures[seg] >= maxSegmentFailures {
		s.inputFailed = true
		return errSegmentExhausted
	}
	next := adaptToContract(s.audioMatchBaseDecision(), s.info, info, s.container, s.contract)
	next = s.applyAudioMatchLocked(inputURL, next)
	if inputURL == s.input {
		next.AudioStream = s.decision.AudioStream
		next.SubtitleStream = s.decision.SubtitleStream
	}
	if next.Enhancement.Backend == rtxBackend && next.Enhancement.State == "stopped" && next.Enhancement.Width > 0 {
		next.Enhancement.State = "selected"
	}
	if s.isDASH() {
		next = dashDecision(info, next)
	}
	if s.videoTranscodingOff() && info.Video() != nil && !next.VideoCopy {
		return ErrVideoTranscodingDisabled
	}
	s.stopRunForLocked("input swap")
	for t, done := range s.done {
		for n := range done {
			if n >= seg {
				_ = os.Remove(s.trackSegPath(t, n))
				delete(done, n)
			}
		}
	}
	if next.Kind != s.decision.Kind || next.VideoCopy != s.decision.VideoCopy || next.AudioCopy != s.decision.AudioCopy {
		s.log.Info("failover changes decision", "from", s.decision.Kind, "to", next.Kind, "reason", next.Reason)
	}
	// Keep the ladder's track layout; adapt every rung like the primary.
	if len(s.rungs) > 0 {
		s.rungs[0].decision = next
		for i := 1; i < len(s.rungs); i++ {
			old := s.rungs[i].decision
			nd := adaptDecision(old, s.info, info, s.container)
			if s.isDASH() {
				nd = dashDecision(info, nd)
			}
			nd.VideoCopy = false
			nd.Kind = core.DecisionTranscode
			nd.MaxHeight, nd.BitrateKbps = old.MaxHeight, old.BitrateKbps
			s.rungs[i].decision = nd
		}
	}
	if s.input != inputURL {
		s.sourceLimitKbps, s.sourceLimitPicture = 0, nil
		s.textSubtitleStreams = nil // The durable source identity belongs to the original input.
		s.textSubtitleSource = ""
		s.healTimestamps = false // clock evidence belongs to the sampled input
		s.adaptiveCopyAligned = false
		s.adaptiveCopyDecodeLead = 0
		// SwapInput has no replacement-credential parameter. Never carry an
		// old provider's credentials onto an independently resolved URL.
		s.inputHeaders = nil
	}
	if s.softSubtitleStream != nil && s.input != inputURL {
		old := findStream(s.info, s.softSubtitleInputIndex, "subtitle")
		matched := matchStream(info, old, "subtitle")
		s.softSubtitleInputIndex = -1
		if matched != nil && !matched.External && IsTextSubtitle(matched.Codec) {
			s.softSubtitleInputIndex = matched.Index
		}
	}
	s.inputCrop = crop
	if s.resumeWarm != nil {
		s.resumeWarm.encoder.seal()
		s.resumeWarm = nil
	}
	s.input = inputURL
	s.info = info
	s.decision = next
	s.swapBoundary = seg
	s.swapSeekAt = s.recoverySeekAt
	s.endSeg = -1
	s.failures = 0
	s.failErr = nil
	s.inputFailed = false
	s.headerRecovering = false
	s.headerFallbackPending = ""
	s.retryRecovering = false
	s.retryGen++
	if !preserve {
		s.lastReq = seg
	}
	s.seekRampSeg = seg
	s.log.Info("input swapped", "at", at, "segment", seg)
	return s.startRunForLocked(seg, false, "input swap")
}

// AdaptiveBitrateLimit reports the active master's peak ceiling in bits per second.
func (s *Session) AdaptiveBitrateLimit() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adaptiveMaxKbps * 1000
}

func (s *Session) CapAdaptiveBitrate(input string, kbps int) (core.Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.capAdaptiveBitrateLocked(input, kbps)
}

func (s *Session) capAdaptiveBitrateLocked(input string, kbps int) (core.Decision, error) {
	if !s.adaptiveHLS || s.closed || s.liveInput || s.input != input || kbps <= 0 {
		return core.Decision{}, core.ErrNotAvailable
	}
	previousCap, previousExclude := s.adaptiveMaxKbps, s.adaptiveExcludeCopy
	if previousCap > 0 {
		kbps = min(kbps, previousCap)
	}
	s.adaptiveMaxKbps = kbps
	// The encoded variants still share the published clock after a swap.
	// Copied packets from a new release need new random-access evidence.
	s.adaptiveExcludeCopy = len(s.rungs) > 0 && s.rungs[0].decision.VideoCopy && !s.adaptiveCopyAligned
	rungs, _ := s.adaptiveRungsLocked()
	if len(rungs) == 0 {
		s.adaptiveMaxKbps, s.adaptiveExcludeCopy = previousCap, previousExclude
		return core.Decision{}, core.ErrNotAvailable
	}
	if s.cur != nil && !s.rungWithinAdaptiveCapLocked(s.cur.track) {
		s.stopRunForLocked("adaptive bitrate cap")
	}
	return s.rungs[s.rungTrack(rungs[0].ID)].decision, nil
}

// ReduceOutputBitrate keeps the delivered picture and segment clock. A
// different source is still preferable when the original input is too slow.
func (s *Session) ReduceOutputBitrate(ctx context.Context, input string, limitKbps int) (core.Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return core.Decision{}, err
	}
	if s.closed || s.inputFailed || s.sessionExhausted || s.input != input || s.liveInput ||
		s.videoTranscodingOff() || s.headerRecovering || s.retryRecovering ||
		(s.container != ContainerFMP4 && s.container != ContainerMPEGTS && s.container != ContainerMatroska) ||
		(s.sourceLimitKbps > 0 && limitKbps >= s.sourceLimitKbps) {
		return core.Decision{}, core.ErrNotAvailable
	}
	if s.adaptiveHLS {
		if s.adaptiveMaxKbps > 0 && limitKbps >= s.adaptiveMaxKbps {
			return core.Decision{}, core.ErrNotAvailable
		}
		return s.capAdaptiveBitrateLocked(input, limitKbps)
	}
	// Bypass the contract's copy branch: this path requires an encoder
	// capable of reproducing the delivered format.
	picture := s.sourceLimitPicture
	if picture == nil {
		picture = s.contract
	}
	if picture == nil {
		var ok bool
		picture, ok = contractOf(s.info, s.decision)
		if !ok {
			return core.Decision{}, core.ErrNotAvailable
		}
	}
	contract := *picture
	contract.copied = false
	if err := contract.check(s.info, s.m.feat); err != nil {
		return core.Decision{}, err
	}
	d := sourceLimitedDecision(s.decision, s.info, &contract)
	window := s.hlsPeakWindowLocked()
	rate := boundedHLSRate(s.info, d, rateControl{}, limitKbps, window)
	if rate.TargetKbps < 300 || (!s.decision.VideoCopy && videoBitrate(s.info, d) <= rate.TargetKbps) {
		return core.Decision{}, core.ErrNotAvailable
	}
	b := s.recoveryBufferLocked()
	at := b.Position
	if b.ReadyUntil > at && b.ReadyUntil < s.info.Duration {
		at = b.ReadyUntil
	}
	seg := s.segForOffsetLocked(at)
	if s.total > 0 && seg >= s.total || s.segFailures[seg] >= maxSegmentFailures {
		return core.Decision{}, core.ErrNotAvailable
	}
	s.stopRunForLocked("source bitrate cap")
	for t, done := range s.done {
		for n := range done {
			if n >= seg {
				_ = os.Remove(s.trackSegPath(t, n))
				delete(done, n)
			}
		}
	}
	prevLimit, prevPicture := s.sourceLimitKbps, s.sourceLimitPicture
	s.sourceLimitKbps, s.sourceLimitPicture = limitKbps, &contract
	if s.container == ContainerMPEGTS {
		if s.copyDiscontinuities == nil {
			s.copyDiscontinuities = make(map[int]bool)
		}
		s.copyDiscontinuities[seg] = true
		s.playlist = nil
	}
	if s.resumeWarm != nil {
		s.resumeWarm.encoder.seal()
		s.resumeWarm = nil
	}
	s.endSeg = -1
	d.BitrateKbps = rate.TargetKbps + decisionAudioKbps(s.info, d)
	if err := s.startRunForLocked(seg, false, "source bitrate cap"); err != nil {
		s.sourceLimitKbps, s.sourceLimitPicture = prevLimit, prevPicture
		return core.Decision{}, err
	}
	return d, nil
}

func sourceLimitedDecision(d core.Decision, info core.MediaInfo, picture *videoContract) core.Decision {
	d.Kind, d.VideoCopy, d.VideoCodec = core.DecisionTranscode, false, picture.codec
	d.MaxHeight = picture.height
	d.StripDolbyVision, d.ToneMap = false, picture.tonemaps(info.Video())
	d.Enhancement = core.VideoEnhancement{}
	return d
}

func (s *Session) segForOffsetLocked(at time.Duration) int {
	seg := 0
	if at > s.base {
		if s.plan != nil {
			seg = s.plan.IndexAt(at - s.base)
		} else if s.segDur > 0 {
			seg = int((at - s.base) / s.segDur)
		}
	}
	if s.total > 0 && seg >= s.total {
		seg = s.total - 1
	}
	return seg
}

// SkipDamagedSegment implements core.DamagedSegmentSkipper: after failover
// cannot recover a corrupt mid-stream segment on another release, alias up to
// maxDamagedSkipsIncident consecutive segments with valid empty placeholders
// and seek ffmpeg past the damaged range, bounded by exponential backoff,
// per-session hard caps, and client liveness.
func (s *Session) SkipDamagedSegment(ctx context.Context, inputURL string, info core.MediaInfo, at time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if inputURL == "" {
		return errors.New("transcode: SkipDamagedSegment: empty input URL")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("transcode: session closed")
	}
	if s.clientGoneLocked(time.Now()) {
		s.sessionExhausted = true
		s.inputFailed = true
		s.retireSessionAsync()
		s.mu.Unlock()
		return errSessionStopped
	}
	if s.liveInput || s.isDASH() {
		s.mu.Unlock()
		return errors.New("transcode: damaged segment skip unsupported for live/DASH")
	}
	if !looksLikeCorruptInput(s.lastFailStderr, s.failErr) {
		s.mu.Unlock()
		return errors.New("transcode: failure is not a localized corrupt segment")
	}
	seg := s.segForOffsetLocked(at)
	if seg <= 0 || (s.total > 0 && seg+1 >= s.total) || len(s.done) == 0 || !s.done[0][seg-1] {
		s.mu.Unlock()
		return errors.New("transcode: cannot skip damaged segment without prior published output")
	}
	if s.sessionExhausted || s.totalFailures >= maxSessionFailures || s.damagedSkips >= maxDamagedSkipsSession {
		s.sessionExhausted = true
		s.inputFailed = true
		s.mu.Unlock()
		return errSessionExhausted
	}
	var cancel <-chan struct{}
	if s.encoder != nil {
		cancel = s.encoder.cancel
	}
	probeTimeout := 30 * time.Second
	if s.m != nil && s.m.opts.segmentTimeout > 0 && s.m.opts.segmentTimeout < probeTimeout {
		probeTimeout = s.m.opts.segmentTimeout
	}
	s.mu.Unlock()

	for step := 0; step < maxDamagedSkipsIncident; step++ {
		target := seg + step
		next := target + 1

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return errors.New("transcode: session closed")
		}
		if s.clientGoneLocked(time.Now()) {
			s.sessionExhausted = true
			s.inputFailed = true
			s.retireSessionAsync()
			s.mu.Unlock()
			return errSessionStopped
		}
		if s.sessionExhausted || s.totalFailures >= maxSessionFailures || s.damagedSkips >= maxDamagedSkipsSession {
			s.sessionExhausted = true
			s.inputFailed = true
			s.mu.Unlock()
			return errSessionExhausted
		}
		if s.total > 0 && next >= s.total {
			s.mu.Unlock()
			return fmt.Errorf("transcode: damaged segment range at %d reached end of title", seg)
		}
		if s.segFailures[next] >= maxSegmentFailures {
			s.inputFailed = true
			s.mu.Unlock()
			return errSegmentExhausted
		}
		var delay time.Duration
		if !s.lastFailureAt.IsZero() && s.totalFailures > 0 {
			delay = time.Until(s.lastFailureAt.Add(s.retryBackoffLocked(s.totalFailures)))
		}
		s.mu.Unlock()

		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-cancel:
				timer.Stop()
				return errors.New("transcode: session closed")
			}
			timer.Stop()
		}

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return errors.New("transcode: session closed")
		}
		if s.clientGoneLocked(time.Now()) {
			s.sessionExhausted = true
			s.inputFailed = true
			s.retireSessionAsync()
			s.mu.Unlock()
			return errSessionStopped
		}
		if s.sessionExhausted || s.totalFailures >= maxSessionFailures || s.damagedSkips >= maxDamagedSkipsSession {
			s.sessionExhausted = true
			s.inputFailed = true
			s.mu.Unlock()
			return errSessionExhausted
		}
		if !s.publishBoundaryAliasLocked(target) {
			s.inputFailed = true
			s.mu.Unlock()
			return fmt.Errorf("transcode: could not alias damaged segment %d", target)
		}
		if s.skippedSegs == nil {
			s.skippedSegs = make(map[int]bool)
		}
		s.skippedSegs[target] = true
		s.damagedSkips++
		s.damagedStreak++

		if s.input != inputURL {
			nextDecision := adaptToContract(s.audioMatchBaseDecision(), s.info, info, s.container, s.contract)
			nextDecision = s.applyAudioMatchLocked(inputURL, nextDecision)
			if s.videoTranscodingOff() && info.Video() != nil && !nextDecision.VideoCopy {
				s.inputFailed = true
				s.mu.Unlock()
				return ErrVideoTranscodingDisabled
			}
			if len(s.rungs) > 0 {
				s.rungs[0].decision = nextDecision
				for i := 1; i < len(s.rungs); i++ {
					old := s.rungs[i].decision
					nd := adaptDecision(old, s.info, info, s.container)
					nd.VideoCopy = false
					nd.Kind = core.DecisionTranscode
					nd.MaxHeight, nd.BitrateKbps = old.MaxHeight, old.BitrateKbps
					s.rungs[i].decision = nd
				}
			}
			s.input = inputURL
			s.info = info
			s.decision = nextDecision
			if s.pictureSamples != nil {
				s.inputCrop = s.pictureSamples[inputURL]
			}
		}
		s.stopRunForLocked("damaged segment skip")
		for t, done := range s.done {
			for n := range done {
				if n >= next {
					_ = os.Remove(s.trackSegPath(t, n))
					delete(done, n)
				}
			}
		}
		s.wantTrack = 0
		s.endSeg = -1
		s.resumeAt = -1
		s.failures = 0
		s.failErr = nil
		s.inputFailed = false
		s.headerRecovering = false
		s.headerFallbackPending = ""
		s.retryRecovering = false
		s.retryGen++
		s.skipProbing = true
		s.seekRampSeg = next
		startErr := s.startRunForLocked(next, true, "damaged segment skip")
		probeRun := s.cur
		s.broadcastLocked()
		if startErr != nil {
			s.skipProbing = false
			s.inputFailed = true
			s.mu.Unlock()
			return startErr
		}
		s.mu.Unlock()

		probeTimer := time.NewTimer(probeTimeout)
		stepSucceeded := false
		stepCorrupt := false
		var waitErr error
		for {
			s.mu.Lock()
			if s.closed {
				s.skipProbing = false
				s.mu.Unlock()
				waitErr = errors.New("transcode: session closed")
				break
			}
			if s.cur != probeRun {
				s.skipProbing = false
				s.mu.Unlock()
				waitErr = core.ErrReplacementSuperseded
				break
			}
			if probeRun.produced > 0 {
				s.skipProbing = false
				s.damagedStreak = 0
				delete(s.failedInputs, inputURL)
				s.broadcastLocked()
				s.log.Warn("skipped damaged segment range", "from", seg, "to", target, "resumedAt", next, "damagedSkips", s.damagedSkips)
				s.mu.Unlock()
				stepSucceeded = true
				break
			}
			if probeRun.finished {
				s.skipProbing = false
				stepCorrupt = looksLikeCorruptInput(s.lastFailStderr, s.failErr)
				waitErr = s.failErr
				s.mu.Unlock()
				break
			}
			changed := s.changed
			s.mu.Unlock()
			select {
			case <-changed:
			case <-probeTimer.C:
				s.mu.Lock()
				s.skipProbing = false
				s.inputFailed = true
				s.stopRunForLocked("damaged skip timeout")
				s.broadcastLocked()
				s.mu.Unlock()
				waitErr = errors.New("transcode: damaged segment skip timed out")
				goto doneWait
			case <-ctx.Done():
				s.mu.Lock()
				s.skipProbing = false
				s.inputFailed = true
				s.stopRunForLocked("damaged skip canceled")
				s.broadcastLocked()
				s.mu.Unlock()
				waitErr = ctx.Err()
				goto doneWait
			case <-cancel:
				s.mu.Lock()
				s.skipProbing = false
				s.mu.Unlock()
				waitErr = errors.New("transcode: session closed")
				goto doneWait
			}
		}
	doneWait:
		probeTimer.Stop()
		if stepSucceeded {
			return nil
		}
		if !stepCorrupt {
			s.mu.Lock()
			s.inputFailed = true
			if waitErr == nil {
				waitErr = errors.New("transcode: damaged segment skip failed")
			}
			s.broadcastLocked()
			s.mu.Unlock()
			return waitErr
		}
	}

	s.mu.Lock()
	s.skipProbing = false
	s.inputFailed = true
	s.sessionExhausted = true
	s.broadcastLocked()
	s.mu.Unlock()
	return fmt.Errorf("transcode: damaged segment range at %d exceeded skip limit: %w", seg, core.ErrEncoderExhausted)
}

// CheckSwap reports whether SwapInput can deliver info's video in the
// format the client already opened. Playback calls it before committing
// a replacement so an incompatible candidate is skipped, not spliced.
func (s *Session) CheckSwap(info core.MediaInfo) error {
	if s.videoTranscodingOff() && !info.Estimated && info.Video() != nil && !s.SwapWouldCopyVideo(info) {
		return ErrVideoTranscodingDisabled
	}
	if s.contract == nil {
		return nil
	}
	return s.contract.check(info, s.m.feat)
}

// SwapWouldCopyVideo is a preference only; CheckSwap remains the admission
// check. It evaluates the probed stream against the delivered contract.
func (s *Session) SwapWouldCopyVideo(info core.MediaInfo) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if info.Estimated || info.Video() == nil || s.closed {
		return false
	}
	d := adaptToContract(s.audioMatchBaseDecision(), s.info, info, s.container, s.contract)
	return d.VideoCopy && !d.BurnSubtitles
}

// adaptDecision keeps a failover's output compatible with what the client
// is already playing: same codecs, container and size. Streams are matched
// by language; anything the new release cannot copy is transcoded to the
// codec the client already accepted.
func adaptDecision(old core.Decision, oldInfo, newInfo core.MediaInfo, container string) core.Decision {
	d := old
	d.Container = container
	var notes []string

	// Audio: same language (and codec when possible) as before.
	oldA := findStream(oldInfo, old.AudioStream, "audio")
	newA := matchStream(newInfo, oldA, "audio")
	d.AudioStream = -1
	if newA != nil {
		d.AudioStream = newA.Index
	}
	// Subtitles.
	oldS := findStream(oldInfo, old.SubtitleStream, "subtitle")
	newS := matchStream(newInfo, oldS, "subtitle")
	d.SubtitleStream = -1
	if oldS != nil && newS != nil {
		d.SubtitleStream = newS.Index
	}
	if d.BurnSubtitles && newS == nil {
		d.BurnSubtitles = false
		notes = append(notes, "subtitle track missing in new release")
	}
	if newS != nil && IsImageSubtitle(newS.Codec) && oldS != nil {
		d.BurnSubtitles = true
	}

	oldV, newV := oldInfo.Video(), newInfo.Video()
	if newV != nil && oldV != nil {
		oldRange := dynamicRange(oldV)
		if old.StripDolbyVision {
			oldRange = rangeHDR10 // the decoder holds HDR10, not source DV headers
		}
		if d.VideoCopy {
			sameRange := oldRange == dynamicRange(newV) ||
				(dynamicRange(newV) == rangeDV && rangeOfDVBase(newV) == oldRange)
			if newV.Codec != oldV.Codec || newV.Height > oldV.Height || !sameRange || d.BurnSubtitles {
				d.Kind = core.DecisionTranscode
				d.VideoCopy = false
				d.VideoCodec = oldV.Codec
				if d.VideoCodec != "h264" && d.VideoCodec != "hevc" {
					d.VideoCodec = "h264"
				}
				d.MaxHeight = oldV.Height
				if d.BitrateKbps <= 0 {
					d.BitrateKbps = bitrateCeiling(d.MaxHeight, d.VideoCodec)
				}
				d.ToneMap = encodedRange(newV) != rangeSDR && !isHDR(oldV)
				notes = append(notes, fmt.Sprintf("video %s %dp re-encoded to match the stream", newV.Codec, newV.Height))
			}
		} else {
			keptHDR := isHDR(oldV) && !old.ToneMap
			d.ToneMap = encodedRange(newV) != rangeSDR && !keptHDR
		}
		d.StripDolbyVision = d.VideoCopy && oldRange == rangeHDR10 && newV.DolbyVision && rangeOfDVBase(newV) == rangeHDR10
	}
	var audioNote string
	d, audioNote = preserveAudio(d, oldA, newA)
	if audioNote != "" {
		notes = append(notes, audioNote)
	}
	if !d.AudioCopy && newA != nil {
		switch d.AudioCodec {
		case "aac", "ac3", "eac3", "mp3":
		case "opus":
			if container != ContainerFMP4 {
				d.AudioCodec = "aac"
			}
		default: // not encodable for HLS (flac, truehd, dts...)
			d.AudioCodec = "aac"
		}
	}
	if d.Kind == core.DecisionDirectPlay {
		d.Kind = core.DecisionRemux
	}
	if len(notes) > 0 {
		d.Reason = old.Reason + "; failover: " + strings.Join(notes, ", ")
	}
	return d
}

func rangeOfDVBase(v *core.StreamInfo) string {
	b, ok := dvBaseLayer(v)
	if !ok {
		return ""
	}
	return b
}

// matchStream finds the stream in info that best matches prev across failover.
func matchStream(info core.MediaInfo, prev *core.StreamInfo, typ string) *core.StreamInfo {
	return matchReplacementStream(info, prev, typ)
}
