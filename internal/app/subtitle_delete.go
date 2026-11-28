package app

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

func (s subtitleSurface) DeleteSubtitle(_ context.Context, key core.RatingKey, user *core.User, stream core.StreamInfo) error {
	if user == nil || !user.IsOwner {
		return core.ErrUnauthorized
	}
	return s.app.subtitles.DeleteUpload(key, stream)
}
