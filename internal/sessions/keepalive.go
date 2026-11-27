package sessions

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// KeepAlive is activity only: it does not report progress, persist a timeline,
// prepare a missing source, or cancel a client's pending stop.
func (b *Broker) KeepAlive(ctx context.Context, a frontend.SessionAccess, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Trailer grants are broker sessions without a playback-manager source.
	// Check their bound viewer and device without extending the grant lifetime.
	if handled, err := b.previewControl(ctx, a, id, false); handled {
		return err
	}
	activity, ok := b.deps.Playback.(interface{ KeepAlive(string) bool })
	if !ok {
		return core.ErrNotAvailable
	}
	// A liveness request must never restore a missing binding.
	s, v, err := b.accessSessionBinding(ctx, a, id, true, false)
	if err != nil {
		return err
	}
	if !b.currentMediaPolicy(ctx, v, s) {
		return core.ErrUnauthorized
	}
	// Live sessions have their own backend lifecycle, not a playback source.
	if s.live {
		if s.multiviewGroup != "" {
			b.mu.Lock()
			b.touchMultiviewLocked(s.multiviewGroup, b.deps.Now())
			b.mu.Unlock()
		}
		return core.ErrNotAvailable
	}
	if !activity.KeepAlive(s.playID) {
		return core.ErrNotFound
	}
	return nil
}
