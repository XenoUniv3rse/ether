package transcode

import (
	"fmt"
	"time"
)

// runWatchdog observes actual encoder advancement, not periodic progress
// reports with the same values. A paused encoder and an unrequested session
// are deliberately excluded. Its state belongs only to the watch goroutine.
type runWatchdog struct {
	at       time.Time
	frame    int64
	out      time.Duration
	segments int
}

func (w *runWatchdog) check(s *Session, r *run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	timeout := s.m.opts.stallTimeout
	if timeout <= 0 || s.liveInput || s.closed || s.cur != r || r.finished || r.stopped || r.stalled {
		return
	}
	now := time.Now()
	r.progressMu.Lock()
	progress := r.progress
	r.progressMu.Unlock()
	if w.at.IsZero() || len(s.inflight) == 0 || r.paused || progress.Frame > w.frame || progress.OutTime > w.out || r.produced > w.segments {
		w.at = now
		w.frame = progress.Frame
		w.out = progress.OutTime
		w.segments = r.produced
		return
	}
	if now.Sub(w.at) < timeout {
		return
	}
	select {
	case <-r.proc.Done():
		return
	default:
	}
	r.stalled = true
	err := fmt.Errorf("transcode: encoder made no progress for %s", timeout)
	s.log.Warn("stalled encoder; restarting", "run", r.seq, "segment", r.next(), "timeout", timeout)
	r.proc = failedProc{Process: r.proc, err: err}
	r.proc.Stop()
}

// takeStallRetry caps even short-progress/repeated-stall cycles.
func (s *Session) takeStallRetry(now time.Time) bool {
	kept := s.stallRestarts[:0]
	for _, at := range s.stallRestarts {
		if now.Sub(at) < 2*time.Minute {
			kept = append(kept, at)
		}
	}
	s.stallRestarts = kept
	if len(kept) >= 3 {
		return false
	}
	s.stallRestarts = append(kept, now)
	return true
}
