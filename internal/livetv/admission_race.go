package livetv

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

const (
	steadyProbeBudget       = 7 * time.Second
	steadyWaveProbeBudget   = 3500 * time.Millisecond
	steadyMinRemainingProbe = 2500 * time.Millisecond
)

// raceSteadyInputs gives each input a useful probe window rather than dividing
// the deadline by the channel's input count. Only three probes can be alive.
// Capped accounts are deliberately serial here; reservation and login leasing
// remain the authority for capacity already held by other channels/nodes.
//
// Probing and post-probe first-picture startup use separate bounded budgets:
// a dead source still fails inside the probe window so backups can take over,
// while an input that answered the probe gets its own startup window scaled
// from the probe's measured latency. Provider tune-gap waits happen before a
// candidate's probe timer starts so provider pacing never eats startup time.
func (s *Service) raceSteadyInputs(ctx context.Context, c Channel, candidates []StreamSource) (*steadyAdmission, []TuneInputFailure, bool, bool, error) {
	return s.raceSteadyInputsSeed(ctx, c, candidates, nil, nil)
}

func (s *Service) raceSteadyInputsSeed(ctx context.Context, c Channel, candidates []StreamSource, direct *directStreamState, manifest *cachedManifest) (*steadyAdmission, []TuneInputFailure, bool, bool, error) {
	type result struct {
		src         StreamSource
		account     string
		link        *steadyLink
		info        *core.MediaInfo
		startBudget time.Duration
		err         error
	}
	life, cancel := context.WithCancel(ctx)
	defer cancel()

	var deadlineMu sync.Mutex
	probeDeadline := time.Now().Add(steadyProbeBudget)
	if dl, ok := ctx.Deadline(); ok && dl.Before(probeDeadline) {
		probeDeadline = dl
	}
	remainingProbe := func() time.Duration {
		deadlineMu.Lock()
		defer deadlineMu.Unlock()
		return max(time.Until(probeDeadline), 0)
	}
	restoreProbe := func(floor time.Duration) {
		deadlineMu.Lock()
		if target := time.Now().Add(floor); target.After(probeDeadline) {
			probeDeadline = target
			if dl, ok := ctx.Deadline(); ok && dl.Before(probeDeadline) {
				probeDeadline = dl
			}
		}
		deadlineMu.Unlock()
	}

	results := make(chan result, 3)
	pending := append([]StreamSource(nil), candidates...)
	active := map[string]int{}
	running := 0
	var winner *steadyAdmission
	var failures []TuneInputFailure
	var last error = core.ErrNotAvailable
	silent, radio := false, false
	for len(pending) > 0 || running > 0 {
		for winner == nil && life.Err() == nil && running < 3 {
			if remainingProbe() < steadyMinRemainingProbe {
				if len(candidates) > 0 && len(failures) == 0 && errors.Is(last, core.ErrNotAvailable) {
					last = context.DeadlineExceeded
				}
				pending = nil
				break
			}
			index := -1
			account := ""
			var accountLimit int
			for j, src := range pending {
				account = src.AuthSourceID
				if account == "" {
					account = c.SourceID
				}
				entry, err := s.sourceEntry(life, account)
				if err == nil && entry.limit > 0 && active[account] > 0 {
					continue
				}
				if err == nil {
					accountLimit = entry.limit
				}
				index = j
				break
			}
			if index < 0 {
				break
			}
			src := pending[index]
			pending = append(pending[:index], pending[index+1:]...)
			serial := len(candidates) > 3
			if !serial && account != "" && accountLimit > 0 {
				for _, next := range pending {
					nextAccount := next.AuthSourceID
					if nextAccount == "" {
						nextAccount = c.SourceID
					}
					if nextAccount == account {
						serial = true
						break
					}
				}
			}
			active[account]++
			running++
			go func(src StreamSource, account string, serial bool) {
				started := time.Now()
				var link *steadyLink
				var sourceOpenElapsed, probeElapsed, pictureElapsed, startBudget time.Duration
				var info *core.MediaInfo
				defer func() {
					if recover() != nil {
						link.close(s)
						results <- result{src: src, account: account, err: core.ErrNotAvailable}
					}
				}()
				err := func() error {
					beforeOpen := remainingProbe()
					openBudget := beforeOpen
					// Up to three inputs keep the full probe window. Larger
					// lists get two waves with 3.5 seconds per candidate.
					if len(candidates) > 3 {
						openBudget = min(openBudget, steadyWaveProbeBudget)
					}
					openCtx, stopOpen := context.WithTimeout(life, openBudget)
					opened := time.Now()
					var err error
					link, err = s.linkSteadySourceWithState(openCtx, c, src, "broadcast-"+c.ID+"-admit-"+randomID(), s.latencyPolicy().TargetLatency()+s.latencyPolicy().SegmentDuration(), direct, manifest, life)
					sourceOpenElapsed += time.Since(opened)
					stopOpen()
					if err != nil {
						return err
					}
					restoreProbe(min(beforeOpen, remainingProbe()+link.admissionWait))
					s.setInputAdmission(link.ss, true)
					rem := remainingProbe()
					paceStarted := time.Now()
					paceWait, err := s.paceSessionInput(life, link.ss)
					sourceOpenElapsed += time.Since(paceStarted)
					if err != nil {
						return err
					}
					if paceWait > 0 {
						restoreProbe(rem)
					}
					probeBudget := remainingProbe()
					if len(candidates) > 3 {
						probeBudget = min(probeBudget, steadyWaveProbeBudget)
					}
					sniffBudget := probeBudget
					if serial {
						// When another candidate on the same capped account is
						// waiting behind this one, bound initial connect/sniff
						// so a silent primary leaves probe window for backup.
						sniffBudget = min(sniffBudget, steadyWaveProbeBudget)
					}
					probeCtx, stopProbe := context.WithTimeout(life, probeBudget)
					probeStarted := time.Now()
					info, err = s.probeSteadyLink(probeCtx, sniffBudget, c, link)
					probeElapsed += time.Since(probeStarted)
					stopProbe()
					if err != nil {
						return err
					}
					startBudget = iptvSourceStartBudget(probeElapsed)
					startCtx, stopStart := context.WithTimeout(life, startBudget)
					pictureStarted := time.Now()
					err = s.primeAdmissionPicture(startCtx, c, link, info)
					pictureElapsed += time.Since(pictureStarted)
					stopStart()
					return err
				}()
				s.logger().Debug("live admission input finished", "channel", c.ID, "input", src.ID, "elapsed", time.Since(started), "source_open_elapsed", sourceOpenElapsed, "probe_elapsed", probeElapsed, "first_picture_elapsed", pictureElapsed, "success", err == nil)
				results <- result{src: src, account: account, link: link, info: info, startBudget: startBudget, err: err}
			}(src, account, serial)
		}
		if running == 0 {
			break
		}
		// Always drain the bounded workers before returning: no orphaned probes,
		// grants, login leases or reservations survive a losing attempt.
		r := <-results
		running--
		active[r.account]--
		if winner != nil || ctx.Err() != nil {
			r.link.close(s)
			pending = nil
			continue
		}
		if r.err == nil && r.info != nil && r.info.Video() != nil && liveInputHasAudio(r.info) {
			s.setInputAdmission(r.link.ss, false)
			r.link.format = steadyInputFormat(r.info)
			winner = &steadyAdmission{shape: steadyShapeFor(r.info), link: r.link, info: r.info, startBudget: r.startBudget}
			s.recordSourceHealth(c.ID, r.src, nil, false)
			cancel()
			pending = nil
			continue
		}
		r.link.close(s)
		if r.err == nil && r.info != nil && len(r.info.Streams) > 0 {
			if r.info.Video() == nil {
				radio = true
			} else {
				silent = true
			}
			continue
		}
		if r.err == nil {
			r.err = core.ErrNotAvailable
		}
		last = r.err
		failures = append(failures, inputFailure(r.src.ID, r.err))
		if !errors.Is(r.err, ErrProviderBusy) && !errors.Is(r.err, ErrProviderPaced) && life.Err() == nil {
			s.recordSourceHealth(c.ID, r.src, r.err, false)
		}
	}
	if winner == nil && ctx.Err() != nil {
		last = ctx.Err()
	}
	return winner, failures, silent, radio, last
}

// admissionDecoder transfers exactly one already decoded frame, then resumes
// the same decoder without another upstream open or keyframe wait.
type admissionDecoder struct {
	transcode.Decoder
	first *transcode.RawFrame
}

func (d *admissionDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	if err := ctx.Err(); err != nil {
		return transcode.RawFrame{}, err
	}
	if d.first != nil {
		f := *d.first
		d.first = nil
		return f, nil
	}
	return d.Decoder.Next(ctx)
}

func (d *admissionDecoder) StarvingErrors() (time.Duration, bool) {
	if source, ok := d.Decoder.(starving); ok {
		return source.StarvingErrors()
	}
	return 0, false
}

func (d *admissionDecoder) Diagnostics() string {
	if source, ok := d.Decoder.(interface{ Diagnostics() string }); ok {
		return source.Diagnostics()
	}
	return ""
}

func (s *Service) setInputAdmission(ss *session, admitting bool) {
	s.inputRelay.mu.Lock()
	g := s.inputRelay.bySess[ss]
	s.inputRelay.mu.Unlock()
	if g != nil {
		g.admitting.Store(admitting)
	}
}

// primeAdmissionPicture runs only while a viewer is tuning. The winning
// decoder outlives the attempt context; losers are closed before admission
// returns. Its first frame is retained rather than opening the provider again.
func (s *Service) primeAdmissionPicture(ctx context.Context, c Channel, link *steadyLink, info *core.MediaInfo) error {
	if info == nil || info.Video() == nil || !liveInputHasAudio(info) {
		return nil
	}
	if code, _ := steadyRefusal(info); code != "" {
		return nil
	}
	runner, ok := s.transcoder().(transcode.ContinuousRunner)
	if !ok {
		return nil
	}
	input, err := s.relayInput(link.ss)
	if err != nil {
		return err
	}
	reserve := s.stabilityReserve(c) > 0
	d, err := runner.StartDecoder(link.ss.ctx, transcode.DecoderRequest{SessionID: link.ss.info.ID, InputURL: input, Format: steadyShapeFor(info).format, Live: true, PrimeLive: !reserve, ReserveStart: reserve, InputFormat: steadyInputFormat(info)})
	if err != nil {
		return err
	}
	first, err := d.Next(ctx)
	if err != nil {
		_ = d.Close()
		return err
	}
	link.primedDecoder = &admissionDecoder{Decoder: d, first: &first}
	return nil
}
