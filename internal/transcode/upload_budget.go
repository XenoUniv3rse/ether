package transcode

import (
	"os"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/sockerr"
)

// RemoteDeliveryEvent reports one completed media-segment delivery write phase
// so the upload budget estimator can passively measure link upload throughput
// and TCP send congestion from real remote streams.
type RemoteDeliveryEvent struct {
	SessionID        string
	RemoteAddr       string
	Bytes            int64
	Duration         time.Duration
	SegDuration      time.Duration
	MediaBitrateKbps int
	TCPBacklogBytes  int
	TCPStall         sockerr.TCPStallKind
	BufferFilling    bool
}

// SetRemoteDeliveryObserver installs an optional callback invoked after
// completed non-loopback media segment deliveries.
func (m *Manager) SetRemoteDeliveryObserver(fn func(RemoteDeliveryEvent)) {
	if m == nil {
		return
	}
	if fn == nil {
		m.remoteDelivery.Store(nil)
		return
	}
	m.remoteDelivery.Store(&fn)
}

func (m *Manager) notifyRemoteDelivery(ev RemoteDeliveryEvent) {
	if m == nil {
		return
	}
	if ptr := m.remoteDelivery.Load(); ptr != nil && *ptr != nil {
		(*ptr)(ev)
	}
}

// AdaptiveBudgetInfo returns the ladder rung bitrates (kbps, descending), the
// currently applied budget cap (0 = uncapped), and whether sessionID supports
// mid-stream adaptive budget stepping.
func (m *Manager) AdaptiveBudgetInfo(sessionID string) (rungsKbps []int, currentCapKbps int, adaptive bool) {
	if m == nil || sessionID == "" {
		return nil, 0, false
	}
	m.mu.Lock()
	s := m.sessions[sessionID]
	m.mu.Unlock()
	if s == nil {
		return nil, 0, false
	}
	return s.AdaptiveBudgetInfo()
}

// SetSessionBudgetCap applies (or clears when capKbps <= 0) a dynamic shared
// upload budget cap on a running adaptive HLS session.
func (m *Manager) SetSessionBudgetCap(sessionID string, capKbps int) (applied bool, rungID string) {
	if m == nil || sessionID == "" {
		return false, ""
	}
	m.mu.Lock()
	s := m.sessions[sessionID]
	m.mu.Unlock()
	if s == nil {
		return false, ""
	}
	return s.SetBudgetCapKbps(capKbps)
}

// SessionBudgetStatus returns the active budget cap (kbps), whether a budget
// cap is active, and the active stepped rung id (if any) for sessionID.
func (m *Manager) SessionBudgetStatus(sessionID string) (capKbps int, active bool, rungID string) {
	if m == nil || sessionID == "" {
		return 0, false, ""
	}
	m.mu.Lock()
	s := m.sessions[sessionID]
	m.mu.Unlock()
	if s == nil {
		return 0, false, ""
	}
	return s.BudgetStatus()
}

// AdaptiveBudgetInfo reports ladder rung bitrates (descending) and whether
// this session can step down/up mid-stream for the shared upload budget.
func (s *Session) AdaptiveBudgetInfo() (rungsKbps []int, currentCapKbps int, adaptive bool) {
	if s == nil {
		return nil, 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.supportsBudgetSteppingLocked() {
		return nil, 0, false
	}
	rungs := make([]int, 0, len(s.rungs))
	for _, r := range s.rungs {
		if r.avgKbps > 0 {
			rungs = append(rungs, r.avgKbps)
		}
	}
	return rungs, s.budgetCapKbps, true
}

// BudgetStatus reports the session's active shared upload budget cap.
func (s *Session) BudgetStatus() (capKbps int, active bool, rungID string) {
	if s == nil {
		return 0, false, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.budgetCapKbps <= 0 {
		return 0, false, ""
	}
	if s.budgetRung > 0 && s.budgetRung < len(s.rungs) {
		rungID = s.rungs[s.budgetRung].id
	}
	return s.budgetCapKbps, true, rungID
}

func (s *Session) supportsBudgetSteppingLocked() bool {
	if s.closed || s.adaptiveHLS || s.liveInput || s.proto != core.ProtocolHLS || len(s.rungs) == 0 {
		return false
	}
	if s.videoTranscodingOff() {
		return false
	}
	switch s.container {
	case ContainerMPEGTS:
		// MPEG-TS carries SPS/PPS inline so stepping between rungs or clamping
		// bitrate mid-stream is decoder-safe.
		return len(s.rungs) > 1 || s.rungs[0].decision.EncodesVideo(s.info)
	case ContainerFMP4:
		// For fMP4, init.mp4 fixes the codec/resolution; only already-encoded
		// tracks can smoothly vary their bitrate without invalidating init.mp4.
		activeTrack := s.wantTrack
		if activeTrack < 0 || activeTrack >= len(s.rungs) {
			activeTrack = 0
		}
		return s.rungs[activeTrack].decision.EncodesVideo(s.info)
	default:
		return false
	}
}

// SetBudgetCapKbps steps a running adaptive HLS session down when capKbps > 0
// tightens its allowance and back up when capKbps widens or clears (0).
// Unserved future segments are discarded and the current encoder is stopped so
// the next segment request seamlessly encodes at the new bitrate/rung.
func (s *Session) SetBudgetCapKbps(capKbps int) (applied bool, rungID string) {
	if s == nil {
		return false, ""
	}
	if capKbps < 0 {
		capKbps = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.supportsBudgetSteppingLocked() {
		return false, ""
	}

	baseAvg := s.rungs[0].avgKbps
	newCap := capKbps
	if newCap > 0 && baseAvg > 0 && newCap >= baseAvg {
		newCap = 0
	}

	targetRung := 0
	if newCap > 0 && s.container == ContainerMPEGTS && len(s.rungs) > 1 {
		targetRung = len(s.rungs) - 1
		for i, r := range s.rungs {
			if r.avgKbps > 0 && r.avgKbps <= newCap {
				targetRung = i
				break
			}
		}
	}

	if s.budgetCapKbps == newCap && s.budgetRung == targetRung {
		return false, ""
	}

	s.budgetCapKbps = newCap
	s.budgetRung = targetRung
	if targetRung > 0 && targetRung < len(s.rungs) {
		rungID = s.rungs[targetRung].id
	} else if len(s.rungs) > 0 {
		rungID = s.rungs[0].id
	}

	// Discard not-yet-served ahead segments so the updated bitrate/rung takes
	// effect on the very next segment fetched by the player.
	cutAfter := s.lastReq
	if cutAfter < 0 {
		cutAfter = 0
	}
	for t := range s.done {
		if t != 0 && t != s.wantTrack {
			continue
		}
		for produced := range s.done[t] {
			if produced > cutAfter {
				delete(s.done[t], produced)
				_ = os.Remove(s.trackSegPath(t, produced))
			}
		}
	}
	if s.cur != nil && !s.cur.finished && !s.cur.stopped {
		s.stopRunForLocked("upload budget")
		s.resumeAt = cutAfter + 1
	}
	return true, rungID
}

// applyBudgetDecisionLocked overlays the active shared upload budget cap onto
// track t's decision while preserving container decoder invariants.
func (s *Session) applyBudgetDecisionLocked(t int, d core.Decision) core.Decision {
	if s.budgetCapKbps <= 0 || s.isDASH() {
		return d
	}
	if s.container == ContainerMPEGTS && s.budgetRung > t && s.budgetRung < len(s.rungs) {
		d = s.rungs[s.budgetRung].decision
	}
	if d.EncodesVideo(s.info) && (d.BitrateKbps == 0 || s.budgetCapKbps < d.BitrateKbps) {
		capVal := s.budgetCapKbps
		if capVal < 300 {
			capVal = 300
		}
		d.BitrateKbps = capVal
	}
	return d
}
