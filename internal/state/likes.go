package state

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// SetLikes maps a tri-state like onto the shared 0–10 viewer rating atomically.
func (s *State) SetLikes(ctx context.Context, userID int64, key core.RatingKey, likes *bool) error {
	return s.update(ctx, userID, []core.RatingKey{key}, func(cur *row, now time.Time) (bool, bool, []Event) {
		rating := core.RatingWithLikes(cur.ws.UserRating, likes)
		if rating == cur.ws.UserRating {
			return false, false, nil
		}
		cur.ws.UserRating = rating
		cur.ws.UpdatedAt = now
		return false, false, []Event{{Kind: EventRated, UserID: userID, Key: cur.ws.Key, Rating: rating, At: now}}
	})
}

var _ core.LikesState = (*State)(nil)
