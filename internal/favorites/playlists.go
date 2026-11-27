package favorites

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/reqmemo"
)

// Playlists projects the marked Favorites playlist from the authoritative set.
// The underlying playlist contains no duplicated favorite item list.
type Playlists struct {
	Base      core.Playlists
	Favorites *Service
	Marked    core.MarkedPlaylists
}

func NewPlaylists(base core.Playlists, favorites *Service) (*Playlists, error) {
	marked, ok := base.(core.MarkedPlaylists)
	if base == nil || favorites == nil || !ok {
		return nil, errors.New("favorites: missing marked playlist support")
	}
	return &Playlists{base, favorites, marked}, nil
}
func (p *Playlists) favorite(ctx context.Context, uid int64) (*core.Playlist, error) {
	return p.Marked.MarkedPlaylist(ctx, uid, "favorites")
}
func (p *Playlists) project(ctx context.Context, uid int64, pl *core.Playlist) (*core.Playlist, error) {
	entries, err := p.Favorites.List(ctx, &core.User{ID: uid}, KindTitle, 0)
	if err != nil {
		return nil, err
	}
	cp := *pl
	cp.Items = make([]core.RatingKey, 0, len(entries))
	cp.ItemAddedAt = make([]time.Time, 0, len(entries))
	updated, err := p.Favorites.UpdatedAt(ctx, uid)
	if err != nil {
		return nil, err
	}
	if updated.After(cp.UpdatedAt) {
		cp.UpdatedAt = updated
	}
	for _, e := range entries {
		k, err := core.ParseRatingKey(e.Subject.ID)
		if err != nil {
			return nil, err
		}
		cp.Items = append(cp.Items, k)
		cp.ItemAddedAt = append(cp.ItemAddedAt, e.AddedAt)
	}
	return &cp, nil
}
func (p *Playlists) List(ctx context.Context, uid int64) ([]core.Playlist, error) {
	items, err := p.Base.List(ctx, uid)
	if err != nil {
		return nil, err
	}
	fav, err := p.favorite(ctx, uid)
	if errors.Is(err, core.ErrNotFound) {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].Key == fav.Key {
			projected, e := p.project(ctx, uid, &items[i])
			if e != nil {
				return nil, e
			}
			items[i] = *projected
		}
	}
	return items, nil
}

// ListHeaders lists the viewer's playlists without their items, with the
// favorites playlist's date as List projects it; Get reads one with its
// items.
func (p *Playlists) ListHeaders(ctx context.Context, uid int64) ([]core.Playlist, error) {
	base, ok := p.Base.(interface {
		ListHeaders(context.Context, int64) ([]core.Playlist, error)
	})
	if !ok {
		return p.List(ctx, uid)
	}
	items, err := base.ListHeaders(ctx, uid)
	if err != nil {
		return nil, err
	}
	fav, err := p.favorite(ctx, uid)
	if errors.Is(err, core.ErrNotFound) {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].Key == fav.Key {
			updated, err := p.Favorites.UpdatedAt(ctx, uid)
			if err != nil {
				return nil, err
			}
			if updated.After(items[i].UpdatedAt) {
				items[i].UpdatedAt = updated
			}
		}
	}
	return items, nil
}

// GetHeader returns one playlist header without loading its items, with the
// favorites playlist's UpdatedAt projected as in ListHeaders.
func (p *Playlists) GetHeader(ctx context.Context, uid int64, key core.RatingKey) (*core.Playlist, error) {
	base, ok := p.Base.(interface {
		GetHeader(context.Context, int64, core.RatingKey) (*core.Playlist, error)
	})
	if !ok {
		pl, err := p.Get(ctx, uid, key)
		if err != nil || pl == nil {
			return pl, err
		}
		cp := *pl
		cp.Items = nil
		return &cp, nil
	}
	pl, err := base.GetHeader(ctx, uid, key)
	if err != nil {
		return nil, err
	}
	fav, e := p.favorite(ctx, uid)
	if e != nil && !errors.Is(e, core.ErrNotFound) {
		return nil, e
	}
	if e == nil && fav.Key == key {
		updated, err := p.Favorites.UpdatedAt(ctx, uid)
		if err != nil {
			return nil, err
		}
		if updated.After(pl.UpdatedAt) {
			pl.UpdatedAt = updated
		}
	}
	return pl, nil
}

// GetHeaders returns headers for the requested playlists in a batch without
// loading playlist items, projecting the favorites playlist's UpdatedAt once.
func (p *Playlists) GetHeaders(ctx context.Context, uid int64, keys []core.RatingKey) (map[core.RatingKey]core.Playlist, error) {
	base, ok := p.Base.(interface {
		GetHeaders(context.Context, int64, []core.RatingKey) (map[core.RatingKey]core.Playlist, error)
	})
	if !ok {
		out := make(map[core.RatingKey]core.Playlist, len(keys))
		for _, k := range keys {
			pl, err := p.GetHeader(ctx, uid, k)
			if errors.Is(err, core.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if pl != nil {
				out[pl.Key] = *pl
			}
		}
		return out, nil
	}
	out, err := base.GetHeaders(ctx, uid, keys)
	if err != nil || len(out) == 0 {
		return out, err
	}
	fav, e := p.favorite(ctx, uid)
	if e != nil && !errors.Is(e, core.ErrNotFound) {
		return nil, e
	}
	if e == nil {
		if pl, exists := out[fav.Key]; exists {
			updated, err := p.Favorites.UpdatedAt(ctx, uid)
			if err != nil {
				return nil, err
			}
			if updated.After(pl.UpdatedAt) {
				pl.UpdatedAt = updated
				out[fav.Key] = pl
			}
		}
	}
	return out, nil
}

type playlistOwnershipKey struct{}

type playlistOwnership struct {
	viewer int64
	owned  map[core.RatingKey]bool
}

// PreparePlaylistArtwork shares ownership checks across the artwork page.
func (p *Playlists) PreparePlaylistArtwork(ctx context.Context, uid int64, keys []core.RatingKey) (context.Context, error) {
	owned, err := p.ExistsBatch(ctx, uid, keys)
	if err != nil {
		return ctx, err
	}
	for _, key := range keys {
		if !owned[key] {
			owned[key] = false
		}
	}
	return context.WithValue(ctx, playlistOwnershipKey{}, playlistOwnership{viewer: uid, owned: owned}), nil
}

// Exists reports whether the viewer owns a playlist with key without reading
// its items or the favorites marker.
func (p *Playlists) Exists(ctx context.Context, uid int64, key core.RatingKey) (bool, error) {
	if page, ok := ctx.Value(playlistOwnershipKey{}).(playlistOwnership); ok && page.viewer == uid {
		if found, checked := page.owned[key]; checked {
			return found, nil
		}
	}
	if base, ok := p.Base.(interface {
		GetHeader(context.Context, int64, core.RatingKey) (*core.Playlist, error)
	}); ok {
		pl, err := base.GetHeader(ctx, uid, key)
		if errors.Is(err, core.ErrNotFound) {
			return false, nil
		}
		return pl != nil, err
	}
	pl, err := p.Base.Get(ctx, uid, key)
	if errors.Is(err, core.ErrNotFound) {
		return false, nil
	}
	return pl != nil, err
}

// ExistsBatch checks ownership of multiple playlists in one base header query
// without loading items or the favorites marker.
func (p *Playlists) ExistsBatch(ctx context.Context, uid int64, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
	out := make(map[core.RatingKey]bool, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	if base, ok := p.Base.(interface {
		GetHeaders(context.Context, int64, []core.RatingKey) (map[core.RatingKey]core.Playlist, error)
	}); ok {
		hdrs, err := base.GetHeaders(ctx, uid, keys)
		if err != nil {
			return nil, err
		}
		for k := range hdrs {
			out[k] = true
		}
		return out, nil
	}
	for _, k := range keys {
		ok, err := p.Exists(ctx, uid, k)
		if err != nil {
			return nil, err
		}
		if ok {
			out[k] = true
		}
	}
	return out, nil
}

// PlaylistItemKeys keeps artwork selection on the ordered member window.
func (p *Playlists) PlaylistItemKeys(ctx context.Context, uid int64, key core.RatingKey, offset, limit int) ([]core.RatingKey, error) {
	reader, paged := p.Base.(interface {
		PlaylistItemKeys(context.Context, int64, core.RatingKey, int, int) ([]core.RatingKey, error)
	})
	if paged {
		fav, err := reqmemo.Do(ctx, "favorites:artwork-marker:"+strconv.FormatInt(uid, 10), func() (*core.Playlist, error) {
			return p.favorite(ctx, uid)
		})
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			return nil, err
		}
		if fav == nil || fav.Key != key {
			return reader.PlaylistItemKeys(ctx, uid, key, offset, limit)
		}
	}
	pl, err := reqmemo.Do(ctx, "favorites:artwork-members:"+strconv.FormatInt(uid, 10)+":"+key.String(), func() (*core.Playlist, error) {
		return p.Get(ctx, uid, key)
	})
	if err != nil {
		return nil, err
	}
	start := min(offset, len(pl.Items))
	return pl.Items[start:min(start+limit, len(pl.Items))], nil
}

func (p *Playlists) Get(ctx context.Context, uid int64, key core.RatingKey) (*core.Playlist, error) {
	pl, err := p.Base.Get(ctx, uid, key)
	if err != nil {
		return nil, err
	}
	fav, e := p.favorite(ctx, uid)
	if e != nil && !errors.Is(e, core.ErrNotFound) {
		return nil, e
	}
	if e == nil && fav.Key == key {
		return p.project(ctx, uid, pl)
	}
	return pl, nil
}
func (p *Playlists) Create(ctx context.Context, uid int64, title string, items []core.RatingKey) (*core.Playlist, error) {
	return p.Base.Create(ctx, uid, title, items)
}
func (p *Playlists) CreateSmartPlaylist(ctx context.Context, uid int64, title, uri string) (*core.Playlist, error) {
	base, ok := p.Base.(core.SmartPlaylists)
	if !ok {
		return nil, errors.New("smart playlists unavailable")
	}
	return base.CreateSmartPlaylist(ctx, uid, title, uri)
}
func (p *Playlists) Add(ctx context.Context, uid int64, key core.RatingKey, items []core.RatingKey) error {
	fav, err := p.favorite(ctx, uid)
	if errors.Is(err, core.ErrNotFound) {
		return p.Base.Add(ctx, uid, key, items)
	}
	if err != nil {
		return err
	}
	if fav.Key != key {
		return p.Base.Add(ctx, uid, key, items)
	}
	subjects := make([]Subject, len(items))
	for i, k := range items {
		subjects[i] = Subject{Kind: KindTitle, ID: k.String()}
	}
	_, err = p.Favorites.setMany(ctx, &core.User{ID: uid}, subjects, true, key)
	return err
}
func (p *Playlists) Remove(ctx context.Context, uid int64, key core.RatingKey, index int) error {
	fav, err := p.favorite(ctx, uid)
	if errors.Is(err, core.ErrNotFound) {
		return p.Base.Remove(ctx, uid, key, index)
	}
	if err != nil {
		return err
	}
	if fav.Key != key {
		return p.Base.Remove(ctx, uid, key, index)
	}
	return p.Favorites.removeTitleAt(ctx, &core.User{ID: uid}, key, index)
}
func (p *Playlists) Move(ctx context.Context, uid int64, key core.RatingKey, from, to int) error {
	fav, err := p.favorite(ctx, uid)
	if errors.Is(err, core.ErrNotFound) {
		return p.Base.Move(ctx, uid, key, from, to)
	}
	if err != nil {
		return err
	}
	if fav.Key == key {
		return p.Favorites.MoveTitle(ctx, &core.User{ID: uid}, key, from, to)
	}
	return p.Base.Move(ctx, uid, key, from, to)
}
func (p *Playlists) Delete(ctx context.Context, uid int64, key core.RatingKey) error {
	fav, err := p.favorite(ctx, uid)
	if errors.Is(err, core.ErrNotFound) {
		return p.Base.Delete(ctx, uid, key)
	}
	if err != nil {
		return err
	}
	if fav.Key != key {
		return p.Base.Delete(ctx, uid, key)
	}
	return p.Favorites.setMirror(ctx, &core.User{ID: uid}, false, key)
}
func (p *Playlists) EditPlaylist(ctx context.Context, uid int64, key core.RatingKey, edit core.PlaylistEdit) (*core.Playlist, error) {
	editor, ok := p.Base.(core.PlaylistEditor)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	pl, err := editor.EditPlaylist(ctx, uid, key, edit)
	if err != nil {
		return nil, err
	}
	fav, e := p.favorite(ctx, uid)
	if e != nil && !errors.Is(e, core.ErrNotFound) {
		return nil, e
	}
	if e == nil && fav.Key == key {
		return p.project(ctx, uid, pl)
	}
	return pl, nil
}

var _ core.Playlists = (*Playlists)(nil)
var _ core.PlaylistEditor = (*Playlists)(nil)
