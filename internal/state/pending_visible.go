package state

import "github.com/braedonsaunders/ether/internal/core"

// HasPendingWatch reports whether reads overlay queued watch writes.
func (s *State) HasPendingWatch(uid int64) bool {
	return s.PendingWatchRevision(uid) != 0
}

// HasPendingEpisodeWatch reports whether an episode's persisted watch state
// may be overridden by a queued update in this process.
func (s *State) HasPendingEpisodeWatch(uid int64) bool {
	if s.pending == nil {
		return false
	}
	s.pending.mu.Lock()
	defer s.pending.mu.Unlock()
	for key := range s.pending.entries {
		if key.user == uid && key.key.Kind() == core.KindEpisode {
			return true
		}
	}
	return false
}

// PendingWatchRevision reads a viewer's latest queued edit from the bounded
// SQLite queue. Zero means their watch state is fully persisted.
func (s *State) PendingWatchRevision(uid int64) uint64 {
	if s.pending == nil {
		return 0
	}
	s.pending.mu.Lock()
	defer s.pending.mu.Unlock()
	var seq uint64
	for key, write := range s.pending.entries {
		if key.user == uid {
			seq = max(seq, write.seq)
		}
	}
	return seq
}
