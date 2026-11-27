package livetv

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// A registry is node-local. Personal channels include viewerID in their key;
// ordinary channels use zero, so all viewers share the same input clock.
type broadcastKey struct {
	channelID string
	viewerID  int64
}
type broadcastRegistry struct {
	mu        sync.Mutex
	outputs   map[broadcastKey]*broadcast
	retiring  map[*broadcast]broadcastKey
	closed    bool
	yielding  atomic.Int32
	onAcquire func()
}
type broadcastLease struct {
	mu        sync.Mutex
	closed    bool
	height    int
	registry  *broadcastRegistry
	broadcast *broadcast
	rendition string
	once      sync.Once
}

func (r *broadcastRegistry) hasActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.outputs) > 0
}

func (r *broadcastRegistry) hasViewers(channelID string) bool {
	if channelID == "" {
		return false
	}
	r.mu.Lock()
	var matches []*broadcast
	for key, b := range r.outputs {
		if key.channelID == channelID && b != nil {
			matches = append(matches, b)
		}
	}
	r.mu.Unlock()
	for _, b := range matches {
		b.mu.Lock()
		active := !b.closed && b.viewers > 0
		b.mu.Unlock()
		if active {
			return true
		}
	}
	return false
}

func (r *broadcastRegistry) watchedChannelIDs() map[string]bool {
	r.mu.Lock()
	type entry struct {
		channelID string
		b         *broadcast
	}
	entries := make([]entry, 0, len(r.outputs))
	for key, b := range r.outputs {
		if key.channelID != "" && b != nil {
			entries = append(entries, entry{channelID: key.channelID, b: b})
		}
	}
	r.mu.Unlock()
	out := map[string]bool{}
	for _, ent := range entries {
		ent.b.mu.Lock()
		active := !ent.b.closed && ent.b.viewers > 0
		ent.b.mu.Unlock()
		if active {
			out[ent.channelID] = true
		}
	}
	return out
}

func (r *broadcastRegistry) acquire(ctx context.Context, key broadcastKey, id string, decision core.Decision, create func() (*broadcast, error), caps ...core.ClientCaps) (*broadcastLease, error) {
	// Provider admission and stream-limit handling yield idle outputs only
	// when capacity is needed. A new rendition alone must not evict warm TV.
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, core.ErrNotFound
	}
	if r.outputs == nil {
		r.outputs = map[broadcastKey]*broadcast{}
	}
	b := r.outputs[key]
	if b != nil {
		b.mu.Lock()
		closed := b.closed
		b.mu.Unlock()
		if closed {
			delete(r.outputs, key)
			b = nil
		}
	}
	running := b != nil
	onAcquire := r.onAcquire
	if b == nil {
		var err error
		b, err = create()
		if err != nil {
			r.mu.Unlock()
			return nil, err
		}
		b.yieldIdle = r.yieldIdle
		r.outputs[key] = b
		b.start()
	}
	b.mu.Lock()
	b.attaching++
	b.mu.Unlock()
	r.mu.Unlock()
	if onAcquire != nil {
		onAcquire()
	}
	_, err := b.addRendition(ctx, id, decision, caps...)
	if err != nil && errors.Is(err, transcode.ErrTooManyTranscodes) && r.yieldIdleForCapacity(ctx, b) {
		_, err = b.addRendition(ctx, id, decision, caps...)
	}
	b.mu.Lock()
	if b.attaching > 0 {
		b.attaching--
	}
	b.mu.Unlock()
	if err != nil {
		if !running || b.providerBacked {
			r.discardCold(b)
		}
		return nil, err
	}
	b.logger.Debug("broadcast viewer attached", "running", running, "rendition", id)
	return &broadcastLease{registry: r, broadcast: b, rendition: id, height: decision.MaxHeight}, nil
}
func (l *broadcastLease) output() (transcode.SegmentIndex, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, core.ErrNotFound
	}
	b := l.broadcast
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, core.ErrNotFound
	}
	if r := b.renditions[l.rendition]; r != nil {
		return r.encoder.Segments(), nil
	}
	var fallback *broadcastRendition
	for _, r := range b.renditions {
		if l.height > 0 && r.height > l.height {
			continue
		}
		if fallback == nil || r.height < fallback.height {
			fallback = r
		}
	}
	if fallback == nil {
		return nil, core.ErrNotAvailable
	}
	fallback.viewers++
	l.rendition = fallback.id
	return fallback.encoder.Segments(), nil
}
func (l *broadcastLease) close() {
	if l != nil {
		l.once.Do(func() {
			l.mu.Lock()
			l.closed = true
			l.broadcast.releaseRendition(l.rendition)
			l.mu.Unlock()
			if l.broadcast.providerBacked {
				l.registry.discardCold(l.broadcast)
			}
		})
	}
}
func (r *broadcastRegistry) close() {
	r.mu.Lock()
	r.closed = true
	outputs := r.outputs
	r.outputs = nil
	r.mu.Unlock()
	for _, b := range outputs {
		b.close()
	}
}
func (r *broadcastRegistry) expire(now time.Time) {
	r.mu.Lock()
	var idle []*broadcast
	for key, b := range r.outputs {
		if b.idle(now) {
			delete(r.outputs, key)
			idle = append(idle, b)
		}
	}
	r.mu.Unlock()
	for _, b := range idle {
		b.close()
	}
}

func (r *broadcastRegistry) invalidate(channelID string, all bool) {
	r.mu.Lock()
	var stale []*broadcast
	for key, b := range r.outputs {
		if all || key.channelID == channelID {
			delete(r.outputs, key)
			stale = append(stale, b)
		}
	}
	r.mu.Unlock()
	for _, b := range stale {
		b.close()
	}
}

// timelineOrigin is the timestamp of the first finished segment, retained for
// the rendition's lifetime even after that segment leaves the DVR window.
// Seq*Duration cannot recover this with variable opening GOPs.
func (l *broadcastLease) timelineOrigin(ctx context.Context) (time.Time, error) {
	l.mu.Lock()
	id := l.rendition
	closed := l.closed
	l.mu.Unlock()
	if closed {
		return time.Time{}, core.ErrNotFound
	}
	b := l.broadcast
	b.mu.Lock()
	r := b.renditions[id]
	if r == nil {
		b.mu.Unlock()
		return time.Time{}, core.ErrNotAvailable
	}
	done := r.originDone
	b.mu.Unlock()
	if done == nil {
		return time.Time{}, core.ErrNotAvailable
	}
	select {
	case <-ctx.Done():
		return time.Time{}, ctx.Err()
	case <-done:
	}
	b.mu.Lock()
	origin, err := r.origin, r.originErr
	b.mu.Unlock()
	return origin, err
}

// yieldIdle closes every output nobody is watching except the caller, so the
// stream slots they hold during their idle grace go to a channel someone is
// tuning. It reports whether slots are being freed, including closes still in
// flight from an earlier call, so the caller retries soon rather than in 15 s.
func (r *broadcastRegistry) yieldIdle(except *broadcast) bool {
	now := time.Now()
	r.mu.Lock()
	var idle []*broadcast
	for key, b := range r.outputs {
		b.mu.Lock()
		// Claim the idle output under the same lock used to attach viewers.
		if b != except && b.viewers == 0 && b.attaching == 0 && b.hadViewer && !b.closed && now.Sub(b.created) >= 2*time.Second {
			b.closed = true
			b.cancel()
			delete(r.outputs, key)
			if r.retiring == nil {
				r.retiring = map[*broadcast]broadcastKey{}
			}
			r.retiring[b] = key
			idle = append(idle, b)
		}
		b.mu.Unlock()
	}
	r.mu.Unlock()
	for _, b := range idle {
		r.yielding.Add(1)
		b.logger.Debug("broadcast yielded its stream to a tuned channel")
		// close waits for the output's goroutines; never block the caller's
		// frame clock on it.
		go func() {
			defer r.yielding.Add(-1)
			b.close()
			r.mu.Lock()
			delete(r.retiring, b)
			r.mu.Unlock()
		}()
	}
	return len(idle) > 0 || r.yielding.Load() > 0
}

// yieldIdleForCapacity synchronously reclaims idle broadcasts (ignoring the
// 2 s warmup floor) and idle renditions on active broadcasts so their
// transcode slots are freed immediately before retrying a capacity-limited tune.
func (r *broadcastRegistry) yieldIdleForCapacity(ctx context.Context, except *broadcast) bool {
	r.mu.Lock()
	var idle []*broadcast
	var active []*broadcast
	for key, b := range r.outputs {
		if b == except {
			continue
		}
		b.mu.Lock()
		if b.viewers == 0 && b.attaching == 0 && b.hadViewer && !b.closed {
			b.closed = true
			b.cancel()
			delete(r.outputs, key)
			idle = append(idle, b)
		} else if !b.closed && b.hasIdleRenditionLocked() {
			active = append(active, b)
		}
		b.mu.Unlock()
	}
	if len(idle) > 0 {
		r.yielding.Add(int32(len(idle)))
	}
	r.mu.Unlock()
	reclaimed := len(idle) > 0
	for _, b := range idle {
		b.logger.Debug("broadcast synchronously yielded its encoder slot for capacity")
		b.wg.Wait()
		r.yielding.Add(-1)
	}
	for _, b := range active {
		if b.dropIdleRenditions() {
			reclaimed = true
		}
	}
	if reclaimed {
		return true
	}
	if r.yielding.Load() <= 0 {
		return false
	}
	deadline := time.NewTimer(2 * time.Second)
	tick := time.NewTicker(25 * time.Millisecond)
	defer deadline.Stop()
	defer tick.Stop()
	for r.yielding.Load() > 0 {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return true
		case <-tick.C:
		}
	}
	return true
}

// A failed cold tune keeps no grace-period encoder running for an absent viewer.
func (r *broadcastRegistry) discardCold(b *broadcast) {
	r.mu.Lock()
	b.startMu.Lock()
	b.mu.Lock()
	unused := b.viewers == 0 && b.attaching == 0
	if unused {
		b.closed = true
		b.cancel()
	}
	b.mu.Unlock()
	if unused {
		for key, output := range r.outputs {
			if output == b {
				delete(r.outputs, key)
			}
		}
	}
	b.startMu.Unlock()
	r.mu.Unlock()
	if unused {
		b.wg.Wait()
	}
}
