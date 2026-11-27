package identity

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// IssuePersonImages signs credited portrait URLs using the same stateless HMAC
// and serve-time revalidation as single portrait grants, with one batched
// credit proof and zero persisted grant rows.
func (s *ImageGrantService) IssuePersonImages(ctx context.Context, credential string, device core.Device, targets []frontend.PersonImageTarget) ([]string, error) {
	if len(targets) > 100 {
		return nil, frontend.ErrImageBatchLimit
	}
	switch s.surface {
	case "silo", "vela", "jellyfin", "emby":
	default:
		return nil, core.ErrUnauthorized
	}
	for _, target := range targets {
		img := target.Image
		if target.Item == 0 || img.Kind != frontend.ImagePerson || img.Path == "" || len(img.Path) > 1024 || len(img.Tag) > 256 || len(img.Subject) > 256 {
			return nil, core.ErrUnauthorized
		}
	}
	viewer, source, err := s.resolveCredential(ctx, credential)
	if err != nil {
		return nil, err
	}
	macKey, encBlock, err := s.grantKeys()
	if err != nil {
		return nil, core.ErrNotAvailable
	}
	reader, ok := s.experience().(frontend.PeopleImageBatchReader)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	allowed, err := reader.AuthorizePersonImages(ctx, viewer, device, targets)
	if err != nil {
		return nil, err
	}
	if len(allowed) != len(targets) {
		return nil, core.ErrUnauthorized
	}
	secrets := make([]string, len(targets))
	sealedAny := false
	for n, target := range targets {
		if !allowed[n] {
			continue
		}
		secret, err := sealImageGrant(macKey, encBlock, string(s.surface), imageClaims{
			Viewer:   viewer.ID,
			Item:     int64(target.Item),
			Kind:     target.Image.Kind,
			Path:     target.Image.Path,
			Tag:      target.Image.Tag,
			Subject:  target.Image.Subject,
			Language: device.Language,
		})
		if err != nil {
			return nil, err
		}
		secrets[n] = secret
		sealedAny = true
	}
	if sealedAny {
		if err := s.sourceStillLive(ctx, viewer.ID, source); err != nil {
			return nil, err
		}
	}
	return secrets, nil
}

var _ frontend.PersonImageGrantBatch = (*ImageGrantService)(nil)
