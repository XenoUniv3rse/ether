package frontendadapter

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plugins"
)

const (
	maxDownloadWait  = 30 * time.Minute
	maxHeldPerPlugin = 4
	maxHeldGlobally  = 32
)

// DownloadWait converts the ticket policy to a bounded duration.
func DownloadWait(policy *plugins.StreamWaitPolicy) time.Duration {
	if policy == nil || policy.MaxSeconds <= 0 {
		return 0
	}
	return time.Duration(min(policy.MaxSeconds, int(maxDownloadWait/time.Second))) * time.Second
}

// Shared by every media handler using this issuer, across listener mounts.
type mediaWaitSlots struct {
	mu      sync.Mutex
	total   int
	plugins map[string]int
}

func (s *mediaWaitSlots) acquire(pluginID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total >= maxHeldGlobally || s.plugins[pluginID] >= maxHeldPerPlugin {
		return false
	}
	if s.plugins == nil {
		s.plugins = make(map[string]int)
	}
	s.total++
	s.plugins[pluginID]++
	return true
}

func (s *mediaWaitSlots) release(pluginID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total--
	s.plugins[pluginID]--
	if s.plugins[pluginID] == 0 {
		delete(s.plugins, pluginID)
	}
}

func (h *MediaHandler) waitForMedia(ctx context.Context, cancel context.CancelFunc, pluginID string, budget time.Duration, open func(context.Context) (*frontend.KindMedia, error)) (media *frontend.KindMedia, err error) {
	started := time.Now()
	if h.deps.Log != nil {
		defer func() {
			h.deps.Log.Info("kind media preparation wait ended", "plugin", pluginID, "elapsed", time.Since(started), "ready", media != nil && err == nil)
		}()
	}

	// Disarm timeout and Activity cancellation before handing off the
	// stream. The client context continues to govern delivery.
	var endWait sync.Once
	abort := func() { endWait.Do(cancel) }
	expiry := time.AfterFunc(budget, abort)
	defer expiry.Stop()
	if h.deps.TrackWait != nil {
		defer h.deps.TrackWait(pluginID, budget, abort)()
	}

	lastPending := core.ErrPrepareSlow
	for {
		media, err = open(ctx)
		pending := errors.Is(err, core.ErrPreparing) || errors.Is(err, core.ErrPrepareSlow) || errors.Is(err, core.ErrDownloadQueued)
		if !pending || ctx.Err() != nil {
			if !expiry.Stop() {
				abort()
			}
			endWait.Do(func() {})
			if ctx.Err() != nil {
				if media != nil {
					_ = media.Closer.Close()
				}
				return nil, lastPending
			}
			return media, err
		}

		lastPending = err
		timer := time.NewTimer(mediaRetryDelay(err))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, lastPending
		case <-timer.C:
		}
	}
}

func mediaRetryDelay(err error) time.Duration {
	// A waiter timeout can rejoin the active flight; other pending replies
	// may need another resolve or source open.
	if errors.Is(err, core.ErrPrepareSlow) {
		return 100 * time.Millisecond
	}
	var preparing *core.PreparingError
	if errors.As(err, &preparing) && preparing.RetryAfter > 0 {
		return min(max(preparing.RetryAfter, 100*time.Millisecond), 2*time.Second)
	}
	return time.Second
}
