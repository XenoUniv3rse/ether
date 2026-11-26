package experience

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const playlistTitleBatchSize = 400

// PlaylistsAvailable reports whether viewer playlists are backed by storage.
func (s *Service) PlaylistsAvailable() bool {
	return s != nil && s.d.Playlists != nil
}

// Playlists lists the viewer's own playlists. Membership is the titles this
// viewer is allowed to see.
func (s *Service) Playlists(ctx context.Context, viewer *core.Viewer, q frontend.PageQuery) (frontend.PlaylistPage, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.PlaylistPage{}, err
	}
	if s.d.Playlists == nil {
		return frontend.PlaylistPage{}, nil
	}
	lists, err := s.d.Playlists.List(ctx, v.ID)
	if err != nil {
		return frontend.PlaylistPage{}, err
	}
	owned := make([]core.Playlist, 0, len(lists))
	var allKeys []core.RatingKey
	for _, playlist := range lists {
		if playlist.UserID != v.ID || playlist.Key == 0 {
			continue
		}
		owned = append(owned, playlist)
		allKeys = append(allKeys, playlist.Items...)
	}
	visibleMap, err := s.loadVisiblePlaylistTitles(ctx, v, allKeys)
	if err != nil {
		return frontend.PlaylistPage{}, err
	}
	out := make([]core.Playlist, 0, len(owned))
	for _, playlist := range owned {
		playlist.Items, _ = playlistVisibleEntries(playlist.Items, visibleMap)
		playlist.Audio = audioPlaylist(playlist.Items, visibleMap)
		playlist.ItemIDs = nil
		out = append(out, playlist)
	}
	start, end := playlistPage(len(out), q.Offset, q.Limit)
	return frontend.PlaylistPage{Items: out[start:end], Total: len(out)}, nil
}

func audioPlaylist(keys []core.RatingKey, titles map[core.RatingKey]core.Title) bool {
	for _, key := range keys {
		typ := titles[key].MBItemType
		if typ != "Audio" && typ != "AudioBook" {
			return false
		}
	}
	return len(keys) > 0
}

// ViewerPlaylistContents loads one viewer-owned playlist and its visible
// entries with stable entry identifiers in playlist order.
func (s *Service) ViewerPlaylistContents(ctx context.Context, viewer *core.Viewer, key core.RatingKey) (frontend.PlaylistContents, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.PlaylistContents{}, err
	}
	playlist, err := s.ownedPlaylist(ctx, v, key)
	if err != nil {
		return frontend.PlaylistContents{}, err
	}
	visibleMap, err := s.loadVisiblePlaylistTitles(ctx, v, playlist.Items)
	if err != nil {
		return frontend.PlaylistContents{}, err
	}
	entryIDs := playlistEntryIDs(playlist)
	entries := make([]frontend.PlaylistEntry, 0, len(playlist.Items))
	visibleKeys := make([]core.RatingKey, 0, len(playlist.Items))
	for idx, itemKey := range playlist.Items {
		title, ok := visibleMap[itemKey]
		if !ok {
			continue
		}
		entry := frontend.PlaylistEntry{EntryID: entryIDs[idx], Title: title}
		if idx < len(playlist.ItemAddedAt) {
			entry.AddedAt = playlist.ItemAddedAt[idx]
		}
		entries = append(entries, entry)
		visibleKeys = append(visibleKeys, itemKey)
	}
	header := *playlist
	header.Items = visibleKeys
	header.ItemIDs = nil
	return frontend.PlaylistContents{Playlist: header, Entries: entries}, nil
}

func (s *Service) CreatePlaylist(ctx context.Context, viewer *core.Viewer, name string, items []core.RatingKey) (*core.Playlist, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if s.d.Playlists == nil || name == "" || len(name) > 200 {
		return nil, frontend.ErrBadPlaylist
	}
	titles := make(map[core.RatingKey]core.Title, len(items))
	for _, key := range items {
		title, err := s.item(ctx, v, key)
		if err != nil {
			return nil, err
		}
		titles[key] = *title
	}
	pl, err := s.d.Playlists.Create(ctx, v.ID, name, items)
	if err == nil {
		pl.Audio = audioPlaylist(items, titles)
	}
	return pl, err
}

// CreateViewerPlaylist creates a viewer-owned playlist, expanding visible
// shows and seasons to their visible episodes in order and rejecting
// unsupported kinds or playlists exceeding MaxPlaylistEntries.
func (s *Service) CreateViewerPlaylist(ctx context.Context, viewer *core.Viewer, name string, items []core.RatingKey) (*core.Playlist, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	if s.d.Playlists == nil {
		return nil, core.ErrNotAvailable
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return nil, frontend.ErrBadPlaylist
	}
	expanded, err := s.expandPlaylistKeys(ctx, v, items, frontend.MaxPlaylistEntries)
	if err != nil {
		return nil, err
	}
	return s.d.Playlists.Create(ctx, v.ID, name, expanded)
}

// RenameViewerPlaylist updates a viewer-owned mutable playlist's name and
// returns the updated playlist with visible item keys.
func (s *Service) RenameViewerPlaylist(ctx context.Context, viewer *core.Viewer, key core.RatingKey, name string) (*core.Playlist, error) {
	return s.EditViewerPlaylist(ctx, viewer, key, &name, nil)
}

var _ frontend.ViewerPlaylistEditor = (*Service)(nil)

// MaxPlaylistSummary bounds a playlist's description.
const MaxPlaylistSummary = 1000

// EditViewerPlaylist changes a viewer-owned mutable playlist's name and/or
// description and returns it with visible item keys.
func (s *Service) EditViewerPlaylist(ctx context.Context, viewer *core.Viewer, key core.RatingKey, name, summary *string) (*core.Playlist, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, playlist, err := s.mutablePlaylist(ctx, viewer, key)
	if err != nil {
		return nil, err
	}
	edit := core.PlaylistEdit{}
	if name != nil {
		trimmed := strings.TrimSpace(*name)
		if trimmed == "" || len(trimmed) > 200 {
			return nil, frontend.ErrBadPlaylist
		}
		edit.Title = &trimmed
	}
	if summary != nil {
		trimmed := strings.TrimSpace(*summary)
		if len(trimmed) > MaxPlaylistSummary {
			return nil, frontend.ErrBadPlaylist
		}
		edit.Summary = &trimmed
	}
	if edit.Title == nil && edit.Summary == nil {
		return nil, frontend.ErrBadPlaylist
	}
	editor, ok := s.d.Playlists.(core.PlaylistEditor)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	updated, err := editor.EditPlaylist(ctx, v.ID, playlist.Key, edit)
	if err != nil {
		return nil, err
	}
	visible, _, err := s.visiblePlaylistIndexes(ctx, v, updated.Items)
	if err != nil {
		return nil, err
	}
	cp := *updated
	cp.Items = visible
	cp.ItemIDs = nil
	return &cp, nil
}

func (s *Service) AddPlaylistItems(ctx context.Context, viewer *core.Viewer, key core.RatingKey, items []core.RatingKey) error {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, playlist, err := s.mutablePlaylist(ctx, viewer, key)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return frontend.ErrBadPlaylist
	}
	if err := s.playlistTitles(ctx, v, items); err != nil {
		return err
	}
	return s.d.Playlists.Add(ctx, v.ID, playlist.Key, items)
}

// AddViewerPlaylistEntries expands and appends items to a viewer-owned mutable
// playlist, enforcing MaxPlaylistEntries and returning the updated visible
// entry count.
func (s *Service) AddViewerPlaylistEntries(ctx context.Context, viewer *core.Viewer, key core.RatingKey, items []core.RatingKey) (int, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, playlist, err := s.mutablePlaylist(ctx, viewer, key)
	if err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, frontend.ErrBadPlaylist
	}
	remaining := frontend.MaxPlaylistEntries - len(playlist.Items)
	if remaining < 0 {
		remaining = 0
	}
	expanded, err := s.expandPlaylistKeys(ctx, v, items, remaining)
	if err != nil {
		return 0, err
	}
	if len(expanded) > 0 {
		if err := s.d.Playlists.Add(ctx, v.ID, playlist.Key, expanded); err != nil {
			return 0, err
		}
	}
	updated, err := s.ownedPlaylist(ctx, v, playlist.Key)
	if err != nil {
		return 0, err
	}
	visible, _, err := s.visiblePlaylistIndexes(ctx, v, updated.Items)
	if err != nil {
		return 0, err
	}
	return len(visible), nil
}

func (s *Service) RemovePlaylistItem(ctx context.Context, viewer *core.Viewer, key core.RatingKey, index int) error {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, playlist, raw, err := s.visiblePlaylist(ctx, viewer, key)
	if err != nil {
		return err
	}
	if index < 0 || index >= len(raw) {
		return frontend.ErrBadPlaylist
	}
	return s.d.Playlists.Remove(ctx, v.ID, playlist.Key, raw[index])
}

// RemoveViewerPlaylistEntry removes one visible playlist entry identified by
// its stable entryID.
func (s *Service) RemoveViewerPlaylistEntry(ctx context.Context, viewer *core.Viewer, key core.RatingKey, entryID string) error {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, playlist, err := s.mutablePlaylist(ctx, viewer, key)
	if err != nil {
		return err
	}
	if entryID == "" {
		return core.ErrNotFound
	}
	visibleMap, err := s.loadVisiblePlaylistTitles(ctx, v, playlist.Items)
	if err != nil {
		return err
	}
	entryIDs := playlistEntryIDs(playlist)
	for idx, itemKey := range playlist.Items {
		if _, ok := visibleMap[itemKey]; !ok {
			continue
		}
		if entryIDs[idx] == entryID {
			return s.d.Playlists.Remove(ctx, v.ID, playlist.Key, idx)
		}
	}
	return core.ErrNotFound
}

func (s *Service) MovePlaylistItem(ctx context.Context, viewer *core.Viewer, key core.RatingKey, from, to int) error {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, playlist, raw, err := s.visiblePlaylist(ctx, viewer, key)
	if err != nil {
		return err
	}
	if from < 0 || to < 0 || from >= len(raw) || to >= len(raw) {
		return frontend.ErrBadPlaylist
	}
	if from == to {
		return nil
	}
	return s.d.Playlists.Move(ctx, v.ID, playlist.Key, raw[from], raw[to])
}

// MoveViewerPlaylistEntry moves a visible playlist entry to the front when
// afterEntryID is empty, or immediately after the visible entry afterEntryID.
func (s *Service) MoveViewerPlaylistEntry(ctx context.Context, viewer *core.Viewer, key core.RatingKey, entryID, afterEntryID string) error {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, playlist, err := s.mutablePlaylist(ctx, viewer, key)
	if err != nil {
		return err
	}
	if entryID == "" {
		return core.ErrNotFound
	}
	visibleMap, err := s.loadVisiblePlaylistTitles(ctx, v, playlist.Items)
	if err != nil {
		return err
	}
	entryIDs := playlistEntryIDs(playlist)
	raw := make([]int, 0, len(playlist.Items))
	visFrom, visAfter := -1, -1
	for idx, itemKey := range playlist.Items {
		if _, ok := visibleMap[itemKey]; !ok {
			continue
		}
		visIdx := len(raw)
		raw = append(raw, idx)
		if entryIDs[idx] == entryID {
			visFrom = visIdx
		}
		if afterEntryID != "" && entryIDs[idx] == afterEntryID {
			visAfter = visIdx
		}
	}
	if visFrom < 0 {
		return core.ErrNotFound
	}
	if afterEntryID == "" {
		if visFrom == 0 {
			return nil
		}
		return s.d.Playlists.Move(ctx, v.ID, playlist.Key, raw[visFrom], raw[0])
	}
	if visAfter < 0 {
		return core.ErrNotFound
	}
	if visFrom == visAfter || visFrom == visAfter+1 {
		return nil
	}
	from, rawAfter := raw[visFrom], raw[visAfter]
	to := rawAfter
	if from > rawAfter {
		to = rawAfter + 1
	}
	return s.d.Playlists.Move(ctx, v.ID, playlist.Key, from, to)
}

func (s *Service) DeletePlaylist(ctx context.Context, viewer *core.Viewer, key core.RatingKey) error {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, playlist, err := s.mutablePlaylist(ctx, viewer, key)
	if err != nil {
		return err
	}
	return s.d.Playlists.Delete(ctx, v.ID, playlist.Key)
}

// TitlePlaylists lists every playlist of the viewer together with all entry
// identifiers holding titleKey (empty when absent). Smart playlists are
// included and flagged.
func (s *Service) TitlePlaylists(ctx context.Context, viewer *core.Viewer, titleKey core.RatingKey) ([]frontend.TitlePlaylistMembership, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	if s.d.Playlists == nil {
		return nil, core.ErrNotAvailable
	}
	if !titleKey.IsPluginKey() {
		switch titleKey.Kind() {
		case core.KindCollection, core.KindSmart, core.KindPlaylist:
			return nil, core.ErrNotFound
		}
	}
	if _, err := s.item(ctx, v, titleKey); err != nil {
		return nil, err
	}
	lists, err := s.d.Playlists.List(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	// A show or season is added as its episodes, so membership is the
	// entries of those episodes; a movie or episode matches itself.
	members := map[core.RatingKey]bool{titleKey: true}
	if !titleKey.IsPluginKey() {
		switch titleKey.Kind() {
		case core.KindShow, core.KindSeason:
			expanded, err := s.expandPlaylistKeys(ctx, v, []core.RatingKey{titleKey}, frontend.MaxPlaylistEntries)
			if err != nil && !errors.Is(err, frontend.ErrPlaylistTooLarge) {
				return nil, err
			}
			for _, k := range expanded {
				members[k] = true
			}
		}
	}
	out := make([]frontend.TitlePlaylistMembership, 0, len(lists))
	for _, pl := range lists {
		if pl.UserID != v.ID || pl.Key == 0 {
			continue
		}
		entryIDs := playlistEntryIDs(&pl)
		matched := []string{}
		for idx, itemKey := range pl.Items {
			if members[itemKey] {
				matched = append(matched, entryIDs[idx])
			}
		}
		pl.Items = nil
		pl.ItemIDs = nil
		out = append(out, frontend.TitlePlaylistMembership{
			Playlist: pl,
			EntryIDs: matched,
		})
	}
	return out, nil
}

func (s *Service) mutablePlaylist(ctx context.Context, viewer *core.Viewer, key core.RatingKey) (*core.User, *core.Playlist, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, nil, err
	}
	playlist, err := s.ownedPlaylist(ctx, v, key)
	if err != nil {
		return nil, nil, err
	}
	if s.playlistProtected(ctx, v, playlist) {
		return nil, nil, frontend.ErrPlaylistReadOnly
	}
	return v, playlist, nil
}

func (s *Service) visiblePlaylist(ctx context.Context, viewer *core.Viewer, key core.RatingKey) (*core.User, *core.Playlist, []int, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, playlist, err := s.mutablePlaylist(ctx, viewer, key)
	if err != nil {
		return nil, nil, nil, err
	}
	_, raw, err := s.visiblePlaylistIndexes(ctx, v, playlist.Items)
	if err != nil {
		return nil, nil, nil, err
	}
	return v, playlist, raw, nil
}

func (s *Service) ownedPlaylist(ctx context.Context, v *core.User, key core.RatingKey) (*core.Playlist, error) {
	if s.d.Playlists == nil || key.Kind() != core.KindPlaylist {
		return nil, core.ErrNotFound
	}
	playlist, err := s.d.Playlists.Get(ctx, v.ID, key)
	if err != nil || playlist == nil || playlist.UserID != v.ID || playlist.Key != key {
		return nil, core.ErrNotFound
	}
	return playlist, nil
}

func (s *Service) playlistProtected(ctx context.Context, v *core.User, playlist *core.Playlist) bool {
	if playlist == nil || playlist.Smart {
		return true
	}
	marked, ok := s.d.Playlists.(core.MarkedPlaylists)
	if !ok {
		return false
	}
	favorite, err := marked.MarkedPlaylist(ctx, v.ID, "favorites")
	return err == nil && favorite != nil && favorite.Key == playlist.Key
}

func (s *Service) playlistTitles(ctx context.Context, v *core.User, items []core.RatingKey) error {
	for _, key := range items {
		if _, err := s.item(ctx, v, key); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) expandPlaylistKeys(ctx context.Context, v *core.User, keys []core.RatingKey, maxEntries int) ([]core.RatingKey, error) {
	expanded := make([]core.RatingKey, 0, len(keys))
	appendKey := func(k core.RatingKey) error {
		if len(expanded) >= maxEntries {
			return frontend.ErrPlaylistTooLarge
		}
		expanded = append(expanded, k)
		return nil
	}
	for _, key := range keys {
		if key == 0 {
			return nil, frontend.ErrPlaylistUnsupportedKind
		}
		if key.IsPluginKey() {
			noExpandCtx := core.WithNoContainerExpand(ctx)
			piv, err := s.pluginItem(noExpandCtx, v, key)
			if err != nil {
				return nil, err
			}
			if piv.Playable {
				if err := appendKey(key); err != nil {
					return nil, err
				}
				continue
			}
			if piv.Title.Type != core.TypeShow && piv.Title.Type != core.TypeSeason {
				return nil, frontend.ErrPlaylistUnsupportedKind
			}
			remaining := maxEntries - len(expanded)
			page, err := s.pluginChildren(noExpandCtx, v, key, frontend.ItemQuery{
				Recursive: piv.Title.Type == core.TypeShow,
				Limit:     remaining + 1,
			})
			if err != nil {
				return nil, err
			}
			childKeys := make([]core.RatingKey, 0, len(page.Items))
			for _, child := range page.Items {
				childKeys = append(childKeys, child.Key)
			}
			if len(childKeys) == 0 {
				continue
			}
			views, err := s.d.PluginLibraries.ItemsMany(ctx, childKeys)
			if err != nil {
				return nil, err
			}
			for _, ck := range childKeys {
				childPiv, ok := views[ck]
				if !ok || !childPiv.Playable || !s.pluginItemAllowed(ctx, v, childPiv) {
					continue
				}
				if err := appendKey(ck); err != nil {
					return nil, err
				}
			}
			continue
		}
		switch key.Kind() {
		case core.KindMovie, core.KindEpisode:
			if _, err := s.item(ctx, v, key); err != nil {
				return nil, err
			}
			if err := appendKey(key); err != nil {
				return nil, err
			}
		case core.KindShow:
			if _, err := s.item(ctx, v, key); err != nil {
				return nil, err
			}
			var eps []core.Title
			if s.d.Grandchildren != nil {
				var err error
				eps, err = s.d.Grandchildren(ctx, v, key)
				if err != nil {
					return nil, err
				}
			} else if s.d.Children != nil {
				seasons, err := s.d.Children(ctx, v, key)
				if err != nil {
					return nil, err
				}
				for _, season := range seasons {
					children, err := s.d.Children(ctx, v, season.Key)
					if err != nil {
						return nil, err
					}
					eps = append(eps, children...)
				}
			}
			allowed, err := s.allowTitles(ctx, v, eps)
			if err != nil {
				return nil, err
			}
			for _, ep := range allowed {
				if err := appendKey(ep.Key); err != nil {
					return nil, err
				}
			}
		case core.KindSeason:
			if _, err := s.item(ctx, v, key); err != nil {
				return nil, err
			}
			if s.d.Children == nil {
				continue
			}
			eps, err := s.d.Children(ctx, v, key)
			if err != nil {
				return nil, err
			}
			allowed, err := s.allowTitles(ctx, v, eps)
			if err != nil {
				return nil, err
			}
			for _, ep := range allowed {
				if err := appendKey(ep.Key); err != nil {
					return nil, err
				}
			}
		default:
			return nil, frontend.ErrPlaylistUnsupportedKind
		}
	}
	return expanded, nil
}

func (s *Service) visiblePlaylistIndexes(ctx context.Context, v *core.User, items []core.RatingKey) ([]core.RatingKey, []int, error) {
	if len(items) == 0 {
		return []core.RatingKey{}, []int{}, nil
	}
	visibleMap, err := s.loadVisiblePlaylistTitles(ctx, v, items)
	if err != nil {
		return nil, nil, err
	}
	visible, raw := playlistVisibleEntries(items, visibleMap)
	return visible, raw, nil
}

func playlistVisibleEntries(items []core.RatingKey, visibleMap map[core.RatingKey]core.Title) ([]core.RatingKey, []int) {
	visible := make([]core.RatingKey, 0, len(items))
	raw := make([]int, 0, len(items))
	for i, key := range items {
		if _, ok := visibleMap[key]; ok {
			visible = append(visible, key)
			raw = append(raw, i)
		}
	}
	return visible, raw
}

// loadVisiblePlaylistTitles hydrates and policy-filters all unique keys in
// bounded batches without per-entry queries.
func (s *Service) loadVisiblePlaylistTitles(ctx context.Context, v *core.User, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
	out := map[core.RatingKey]core.Title{}
	if len(keys) == 0 {
		return out, nil
	}
	seen := make(map[core.RatingKey]bool, len(keys))
	var pluginKeys, catalogKeys []core.RatingKey
	for _, k := range keys {
		if k == 0 || seen[k] {
			continue
		}
		seen[k] = true
		if k.IsPluginKey() {
			pluginKeys = append(pluginKeys, k)
		} else {
			catalogKeys = append(catalogKeys, k)
		}
	}
	if len(pluginKeys) > 0 && s.d.PluginLibraries != nil {
		for start := 0; start < len(pluginKeys); start += playlistTitleBatchSize {
			chunk := pluginKeys[start:min(start+playlistTitleBatchSize, len(pluginKeys))]
			views, err := s.d.PluginLibraries.ItemsMany(ctx, chunk)
			if err != nil {
				return nil, err
			}
			for k, piv := range views {
				if s.pluginItemAllowed(ctx, v, piv) {
					out[k] = piv.Title
				}
			}
		}
	}
	if len(catalogKeys) > 0 {
		if s.d.CachedGetMany != nil {
			for start := 0; start < len(catalogKeys); start += playlistTitleBatchSize {
				chunk := catalogKeys[start:min(start+playlistTitleBatchSize, len(catalogKeys))]
				titles, err := s.d.CachedGetMany(ctx, chunk)
				if err != nil {
					return nil, err
				}
				loaded := make([]core.Title, 0, len(titles))
				for _, k := range chunk {
					if t, ok := titles[k]; ok && t.Key == k {
						loaded = append(loaded, t)
					}
				}
				allowed, err := s.allowTitles(ctx, v, loaded)
				if err != nil {
					return nil, err
				}
				for _, t := range allowed {
					out[t.Key] = t
				}
			}
		} else {
			for _, k := range catalogKeys {
				t, err := s.item(ctx, v, k)
				if err == nil && t != nil {
					out[k] = *t
					continue
				}
				if !errors.Is(err, core.ErrNotFound) {
					return nil, err
				}
			}
		}
	}
	return out, nil
}

func playlistEntryIDs(pl *core.Playlist) []string {
	out := make([]string, len(pl.Items))
	hasDBIDs := len(pl.ItemIDs) == len(pl.Items)
	seen := map[core.RatingKey]int{}
	for idx, key := range pl.Items {
		if hasDBIDs && pl.ItemIDs[idx] > 0 {
			out[idx] = strconv.FormatInt(pl.ItemIDs[idx], 10)
			continue
		}
		seen[key]++
		out[idx] = "k-" + strconv.FormatInt(int64(key), 10) + "-" + strconv.Itoa(seen[key])
	}
	return out
}

func playlistPage(n, offset, limit int) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset > n {
		offset = n
	}
	end := offset + limit
	if end > n {
		end = n
	}
	return offset, end
}

var _ frontend.ViewerPlaylists = (*Service)(nil)
var _ frontend.PlaylistAvailability = (*Service)(nil)
