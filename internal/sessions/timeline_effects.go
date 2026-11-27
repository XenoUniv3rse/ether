package sessions

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type timelineEffect struct {
	session  brokerSession
	viewer   core.Viewer
	timeline frontend.Timeline
	notify   func()
}

// Queue only accepted telemetry. One lazy worker and at most 256 pending
// accepted reports bound work while a hook is slow. The worker never owns
// the broker lifecycle gate or a session UI lock.
func (b *Broker) enqueueTimelineEffect(s *brokerSession, v *core.Viewer, t frontend.Timeline, notify func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		b.timelineReserved--
		return
	}
	effect := timelineEffect{session: *s, viewer: *v, timeline: t, notify: notify}
	b.timelineEffects = append(b.timelineEffects, effect)
	if b.timelineRunning {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.timelineCancel, b.timelineDone, b.timelineRunning = cancel, make(chan struct{}), true
	done := b.timelineDone
	go b.runTimelineEffects(ctx, done)
}
func (b *Broker) runTimelineEffects(ctx context.Context, done chan struct{}) {
	defer close(done)
	for {
		b.mu.Lock()
		if b.closed || ctx.Err() != nil || len(b.timelineEffects) == 0 {
			b.timelineReserved -= len(b.timelineEffects)
			b.timelineEffects = nil
			b.timelineRunning = false
			b.mu.Unlock()
			return
		}
		effect := b.timelineEffects[0]
		b.timelineEffects[0] = timelineEffect{}
		b.timelineEffects = b.timelineEffects[1:]
		b.mu.Unlock()
		if effect.notify != nil {
			effect.notify()
		}
		b.mu.Lock()
		current := b.sessions[effect.session.public.ID]
		valid := current != nil && current.playID == effect.session.playID && current.streamPosition == effect.session.streamPosition && (effect.timeline.Sequence == 0 || current.sequence <= effect.timeline.Sequence)
		b.mu.Unlock()
		if !valid {
			b.mu.Lock()
			b.timelineReserved--
			b.mu.Unlock()
			continue
		}
		workCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		b.applyTimelineEffects(workCtx, &effect.session, &effect.viewer, effect.timeline)
		b.ensureTrickplay(workCtx, &effect.session, effect.timeline.Position)
		if effect.timeline.State == "playing" {
			b.enqueueSubtitleWork(&effect.session)
		}
		cancel()
		b.mu.Lock()
		b.timelineReserved--
		b.mu.Unlock()
	}
}
func (b *Broker) applyTimelineEffects(ctx context.Context, s *brokerSession, v *core.Viewer, t frontend.Timeline) {
	NotifyTranscodeActivity(b.deps.Transcoder, s.public.ID, t.State)
	if s.public.Protocol == "vela" && (t.State == "playing" || t.State == "buffering") && frontend.ValidClientThroughput(t.ThroughputBps) {
		if observer, ok := b.deps.Playback.(interface{ ObserveClientThroughput(string, float64) }); ok {
			observer.ObserveClientThroughput(s.playID, t.ThroughputBps)
		}
	}
	if progress, ok := b.deps.Playback.(core.PlaybackTimeline); ok {
		progress.PlaybackProgress(s.public.ViewerID, s.clientID, s.public.Item, t.Position, t.State)
	}
	if b.deps.OnTimeline != nil {
		b.deps.OnTimeline(v, s.public.Item, t.Position, t.Duration, t.State)
	}
	b.publish(ctx, sessionEvent("SessionUpdated", s.public, &t))
}
