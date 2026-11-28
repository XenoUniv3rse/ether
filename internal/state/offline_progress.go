package state

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// ApplyOfflineProgress compares and writes in the same transaction, then
// publishes through the usual progress and completion journal.
func (s *State) ApplyOfflineProgress(ctx context.Context, user int64, key core.RatingKey, sample frontend.OfflineProgress) (frontend.TimelinePersistenceResult, error) {
	if sample.ObservedAt.IsZero() || sample.Position < 0 {
		return frontend.TimelinePersistenceResult{}, core.ErrNotAvailable
	}
	if sample.ReceivedAt.IsZero() {
		sample.ReceivedAt = s.now()
	}
	at := sample.ObservedAt
	if at.After(sample.ReceivedAt) {
		at = sample.ReceivedAt
	}
	var result frontend.TimelinePersistenceResult
	err := s.progressAt(ctx, user, key, progressWrite{
		offset: sample.Position, duration: sample.Duration, state: "stopped",
		at: at, origin: originFrom(ctx), offline: &sample, offlineResult: &result,
	})
	return result, err
}

// ResolveItemPlayback preserves an explicit position until the item is marked
// watched or the position reaches the playback completion threshold.
func ResolveItemPlayback(p frontend.ItemPlaybackPatch, duration time.Duration) (position *time.Duration, count *int, watched bool) {
	position, count = p.Position, p.PlayCount
	watched = p.Played != nil && *p.Played
	if p.Played != nil && !*p.Played {
		zero := 0
		count = &zero
	}
	if position != nil {
		watched = watched || ReachedWatchedThreshold(*position, duration)
	}
	if watched {
		zero := time.Duration(0)
		position = &zero
	}
	return
}

// PatchItemPlayback orders observations against both committed and held
// timeline writes before applying any part of their playback snapshot.
func (s *State) PatchItemPlayback(ctx context.Context, user int64, key core.RatingKey, patch frontend.ItemPlaybackPatch, duration time.Duration) error {
	at := s.now()
	if patch.LastPlayedAt != nil && patch.LastPlayedAt.Before(at) {
		at = *patch.LastPlayedAt
	}
	return s.updateAt(ctx, user, []core.RatingKey{key}, at, func(cur *row, now time.Time) (bool, bool, []Event) {
		if s.staleOfflineProgress(cur, now, user, key) {
			cur.skipSave = true
			return false, false, nil
		}
		dur := cur.ws.Duration
		if dur <= 0 {
			dur = duration
		}
		position, count, watched := ResolveItemPlayback(patch, dur)
		before, previousCount := cur.ws.ViewOffset, cur.ws.ViewCount
		if count != nil {
			cur.ws.ViewCount = *count
		}
		if watched {
			cur.ws.ViewCount = max(1, cur.ws.ViewCount)
		}
		if position != nil {
			cur.ws.ViewOffset = *position
			cur.ws.Duration = dur
		}
		if patch.LastPlayedAt != nil || patch.Position != nil || watched {
			cur.ws.LastViewedAt = now
		}
		if position != nil || count != nil {
			cur.counted = watched || (position == nil && cur.ws.ViewCount > 0)
		}
		cur.watchAt = now
		if now.After(cur.ws.UpdatedAt) {
			cur.ws.UpdatedAt = now
		}
		completed := cur.ws.ViewCount > previousCount
		ev := Event{Kind: EventProgress, UserID: user, Key: key, Offset: cur.ws.ViewOffset, Duration: dur,
			State: "stopped", At: now, Offline: true, ResumeSet: before != cur.ws.ViewOffset, Resume: cur.ws.ViewOffset}
		if completed {
			ev.Kind, ev.Completed, ev.Fraction = EventCompleted, true, 1
		} else if previousCount > 0 && cur.ws.ViewCount == 0 {
			ev.Kind = EventUnscrobble
		}
		return completed, completed, []Event{ev}
	})
}

func (s *State) staleOfflineProgress(cur *row, at time.Time, user int64, key core.RatingKey) bool {
	return !at.After(cur.ws.LastViewedAt) || !at.After(cur.watchAt) ||
		(s.pending != nil && !at.After(s.pending.latestAt(progressKey{user, key})))
}
