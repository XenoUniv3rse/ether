package core

import "context"

// LikesState atomically applies a viewer's tri-state like to the shared rating.
// Nil clears the rating; true and false preserve existing compatible ratings.
type LikesState interface {
	SetLikes(context.Context, int64, RatingKey, *bool) error
}

// RatingWithLikes applies a tri-state like to the shared 0–10 rating.
func RatingWithLikes(rating float64, likes *bool) float64 {
	switch {
	case likes == nil:
		return 0
	case *likes && rating < 7:
		return 10
	case !*likes && (rating == 0 || rating > 3):
		return 2
	default:
		return rating
	}
}
