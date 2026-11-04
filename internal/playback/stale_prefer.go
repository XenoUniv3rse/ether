package playback

import (
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// A Plex client keeps the variant Part id of the version it was shown and
// plays it on every open, long after the release behind it failed its
// health check (the resolver then rejects it for a cooldown, so it is "not
// among candidates"). Each open used to ask the resolver for that release
// again, fall back to a fresh normal ranking that skips the warm entry, and
// so play a different release from the one the opening prefetch warmed.
//
// When a non-exact pick falls back, the Manager forgets it for
// stalePreferTTL: later opens of that version are the automatic choice
// (which the warm-up and resume already hold). After the TTL one open asks
// for the picked release again, so a version that recovers is played.
const (
	stalePreferTTL  = 10 * time.Minute
	maxStalePrefers = 1024
)

type stalePreferKey struct {
	key     core.RatingKey
	release string
}

// preferenceStale reports a picked release that recently fell back. The
// caller holds m.mu.
func (m *Manager) preferenceStaleLocked(key core.RatingKey, release string, now time.Time) bool {
	at, ok := m.stalePrefers[stalePreferKey{key, release}]
	if !ok {
		return false
	}
	if now.Sub(at) >= stalePreferTTL {
		delete(m.stalePrefers, stalePreferKey{key, release})
		return false
	}
	return true
}

func (m *Manager) notePreferenceStale(key core.RatingKey, release, played string) {
	now := m.now()
	m.mu.Lock()
	if m.stalePrefers == nil {
		m.stalePrefers = map[stalePreferKey]time.Time{}
	}
	if len(m.stalePrefers) >= maxStalePrefers {
		var oldest stalePreferKey
		var oldestAt time.Time
		for k, at := range m.stalePrefers {
			if now.Sub(at) >= stalePreferTTL {
				delete(m.stalePrefers, k)
			} else if oldestAt.IsZero() || at.Before(oldestAt) {
				oldest, oldestAt = k, at
			}
		}
		if len(m.stalePrefers) >= maxStalePrefers {
			delete(m.stalePrefers, oldest)
		}
	}
	m.stalePrefers[stalePreferKey{key, release}] = now
	m.mu.Unlock()
	m.log.Info("picked version unavailable; playing the automatic choice until it recovers", "key", key, "prefer", release, "release", played, "retry_after", stalePreferTTL)
}

// forgetStalePrefersLocked drops a title's stale picks (InvalidateTitle:
// the owner pinned, blocked or re-searched it). The caller holds m.mu.
func (m *Manager) forgetStalePrefersLocked(key core.RatingKey) {
	for k := range m.stalePrefers {
		if titleIncludes(key, k.key) {
			delete(m.stalePrefers, k)
		}
	}
}
