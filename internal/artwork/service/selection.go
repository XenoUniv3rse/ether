package service

import (
	"context"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/reqmemo"
)

func collectionStamp(c *core.Collection) string {
	if c == nil || c.UpdatedAt.IsZero() {
		return "1"
	}
	return strconv.FormatInt(c.UpdatedAt.Unix(), 10)
}
func (s Source) collection(ctx context.Context, req frontend.ArtworkRequest) (*core.Collection, error) {
	if c, ok := frontend.ArtworkCollection(ctx, req.Item); ok {
		return c, nil
	}
	if s.Collections == nil {
		return nil, core.ErrNotFound
	}
	c, err := s.Collections.Get(ctx, req.Viewer, req.Item)
	if err != nil {
		return nil, err
	}
	if !s.Visibility.CollectionAllowed(req.Viewer, c) {
		return nil, core.ErrNotFound
	}
	if c.Upcoming {
		placeholders, ok := s.Collections.(core.CollectionPlaceholder)
		if !ok || !placeholders.AllowsPlaceholders(ctx, req.Viewer, req.Item, core.ClassifyDevice(req.Device.Product, req.Device.Platform, s.CollectionDevice)) {
			return nil, core.ErrNotFound
		}
		// Preserve the membership lookup and errors. ChildCount is not artwork state.
		if _, err := s.Collections.Items(ctx, req.Viewer, req.Item, 0, 5000); err != nil {
			return nil, err
		}
	}
	return c, nil
}
func (s Source) playlistArtworkKeys(ctx context.Context, viewer int64, key core.RatingKey, offset, limit int) ([]core.RatingKey, error) {
	memo := "artwork:playlist:" + strconv.FormatInt(viewer, 10) + ":" + key.String() + ":" + strconv.Itoa(offset)
	return reqmemo.Do(ctx, memo, func() ([]core.RatingKey, error) {
		if reader, ok := s.Playlists.(interface {
			PlaylistItemKeys(context.Context, int64, core.RatingKey, int, int) ([]core.RatingKey, error)
		}); ok {
			return reader.PlaylistItemKeys(ctx, viewer, key, offset, limit)
		}
		p, err := s.Playlists.Get(ctx, viewer, key)
		if err != nil {
			return nil, err
		}
		start := min(offset, len(p.Items))
		return p.Items[start:min(start+limit, len(p.Items))], nil
	})
}

func (s Source) ArtPath(ctx context.Context, req frontend.ArtworkRequest, element string) (path string, defWidth int, err error) {
	key := req.Item
	switch key.Kind() {
	case core.KindCollection, core.KindSmart:
		if pre, ok := frontend.ArtworkCollection(ctx, key); ok {
			if element == "art" {
				return pre.BackdropPath, 1280, nil
			}
			return pre.PosterPath, 500, nil
		}
		if s.Collections == nil {
			return "", 0, core.ErrNotFound
		}
		c, err := s.collection(ctx, req)
		if err != nil {
			return "", 0, err
		}
		if element == "art" {
			return c.BackdropPath, 1280, nil
		}
		if c.PosterPath == "" {
			// Fall back to the first item's poster the viewer may see:
			// collection items are not filtered by the viewer's policy.
			u := req.Viewer
			if p, err := s.Collections.Items(ctx, u, key, 0, 20); err == nil {
				for i := range p.Items {
					if p.Items[i].PosterPath != "" && s.visible(ctx, u, &p.Items[i]) {
						return p.Items[i].PosterPath, 500, nil
					}
				}
			}
		}
		return c.PosterPath, 500, nil
	case core.KindPlaylist:
		u := req.Viewer
		if s.Playlists == nil || u == nil {
			return "", 0, core.ErrNotFound
		}
		const window = 16
		for offset := 0; ; offset += window {
			keys, err := s.playlistArtworkKeys(ctx, u.ID, key, offset, window)
			if err != nil {
				return "", 0, err
			}
			for _, k := range keys {
				if t, err := s.title(ctx, k); err == nil && s.visible(ctx, u, t) {
					if element == "art" && t.BackdropPath != "" {
						return t.BackdropPath, 1280, nil
					}
					if t.PosterPath != "" {
						return t.PosterPath, 500, nil
					}
				}
			}
			if len(keys) < window {
				break
			}
		}
		return "", 0, core.ErrNotFound
	}
	t, err := s.title(ctx, key)
	if err != nil {
		return "", 0, err
	}
	if t.IsFutureRelease(s.now()) || !s.visible(ctx, req.Viewer, t) {
		return "", 0, core.ErrNotFound
	}
	parent := func() *core.Title {
		pk := t.ShowKey
		if pk == 0 {
			pk = key.Parent()
			if t.Type == core.TypeEpisode {
				pk = pk.Parent()
			}
		}
		if pk == 0 {
			return nil
		}
		p, err := s.title(ctx, pk)
		if err != nil {
			return nil
		}
		return p
	}
	switch element {
	case "art", "background":
		if t.BackdropPath != "" {
			return t.BackdropPath, 1280, nil
		}
		if p := parent(); p != nil && p.BackdropPath != "" {
			return p.BackdropPath, 1280, nil
		}
	case "clearLogo", "logo":
		if t.LogoPath != "" {
			return t.LogoPath, 500, nil
		}
		if p := parent(); p != nil && p.LogoPath != "" {
			return p.LogoPath, 500, nil
		}
	case "banner":
		// A backdrop is not a banner. Titles have no separate banner path, so
		// Jellyfin must not gain an ImageTags/Banner entry for the backdrop.
	default: // thumb, poster
		if t.Type == core.TypeEpisode && t.StillPath != "" {
			return t.StillPath, 0, nil
		}
		if t.PosterPath != "" {
			return t.PosterPath, 500, nil
		}
		if p := parent(); p != nil && p.PosterPath != "" {
			return p.PosterPath, 500, nil
		}
	}
	return "", 0, core.ErrNotFound
}
