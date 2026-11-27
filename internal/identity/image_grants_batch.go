package identity

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const maxImageBatchItems = 50
const maxImageBatchInput = 256

// IssueBatch returns at most one primary descriptor of each requested kind per
// visible item using the unified stateless HMAC scheme, writing zero rows.
func (s *ImageGrantService) IssueBatch(ctx context.Context, req frontend.ImageBatchRequest) ([]frontend.ImageBatchItem, error) {
	if len(req.Items) > maxImageBatchInput || len(req.Kinds) > 6 {
		return nil, frontend.ErrImageBatchLimit
	}
	keys := make([]core.RatingKey, 0, len(req.Items))
	seen := make(map[core.RatingKey]bool, len(req.Items))
	for _, key := range req.Items {
		if key == 0 {
			return nil, frontend.ErrImageBatchLimit
		}
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	if len(keys) > maxImageBatchItems {
		return nil, frontend.ErrImageBatchLimit
	}
	kinds := req.Kinds
	if len(kinds) == 0 {
		kinds = []string{frontend.ImagePoster}
	}
	wanted := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		switch kind {
		case frontend.ImagePoster, frontend.ImageBackdrop, frontend.ImageLogo, frontend.ImageThumb, frontend.ImageBanner, frontend.ImageStill:
			wanted[kind] = true
		default:
			return nil, frontend.ErrImageBatchLimit
		}
	}
	viewer, source, err := s.resolveCredential(ctx, req.Credential)
	if err != nil {
		return nil, err
	}
	macKey, encBlock, err := s.grantKeys()
	if err != nil {
		return nil, core.ErrNotAvailable
	}
	experience := s.experience()
	if experience == nil {
		return nil, core.ErrNotAvailable
	}
	// A batch grants a title's own images, never a credited person's
	// portrait, and seals at most one image per wanted kind.
	ctx = frontend.WithPrimaryArtworkOnly(frontend.WithArtworkRows(ctx))
	page, hasPage := experience.(frontend.ItemArtworkPage)
	var listed map[core.RatingKey][]frontend.Image
	if hasPage && len(keys) > 0 {
		listed, err = page.ItemArtworkPage(frontend.WithArtworkKinds(ctx, wanted), viewer, req.Device, keys)
		if err != nil {
			return nil, err
		}
	}
	out := make([]frontend.ImageBatchItem, 0, len(keys))
	sealedAny := false
	for _, key := range keys {
		var images []frontend.Image
		var complete bool
		if hasPage {
			var present bool
			images, present = listed[key]
			if !present {
				continue
			}
			complete = true
		} else if scoped, ok := experience.(frontend.ItemArtwork); ok {
			images, complete, err = scoped.ItemArtwork(frontend.WithArtworkKinds(ctx, wanted), viewer, req.Device, key)
		} else {
			var detail frontend.ItemDetail
			detail, err = experience.Item(ctx, frontend.ItemRequest{Viewer: viewer, Device: req.Device, Key: key})
			images, complete = detail.Images, detail.ImagesAuthoritative
		}
		if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnauthorized) {
			continue
		}
		if errors.Is(err, core.ErrNotAvailable) {
			out = append(out, frontend.ImageBatchItem{Item: key})
			continue
		}
		if err != nil {
			return nil, err
		}
		item, err := sealTitleBatchItem(macKey, encBlock, string(s.surface), viewer.ID, req.Device.Language, key, images, complete, wanted, false)
		if err != nil {
			return nil, err
		}
		if len(item.Images) > 0 {
			sealedAny = true
		}
		out = append(out, item)
	}
	if sealedAny {
		if err := s.sourceStillLive(ctx, viewer.ID, source); err != nil {
			return nil, err
		}
	}
	return out, nil
}

var _ frontend.ImageGrantBatch = (*ImageGrantService)(nil)
