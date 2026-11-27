package experience

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func (s *Service) PatchItemPlayback(ctx context.Context, viewer *core.Viewer, key core.RatingKey, patch frontend.ItemPlaybackPatch) error {
	writer, ok := s.d.State.(interface {
		PatchItemPlayback(context.Context, int64, core.RatingKey, frontend.ItemPlaybackPatch, time.Duration) error
	})
	if !ok {
		return core.ErrNotAvailable
	}
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return err
	}
	title, err := s.playable(ctx, v, key)
	if err != nil {
		return err
	}
	return writer.PatchItemPlayback(frontend.WithHistoryTitle(ctx, title), v.ID, key, patch, title.Runtime)
}

var _ frontend.ItemPlaybackWriter = (*Service)(nil)
