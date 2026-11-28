package experience

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const activityScanLimit = 10000

func (s *Service) activityLibrary(v *core.User, viewID string) (*Library, error) {
	if viewID == "" {
		return nil, nil
	}
	id, err := strconv.Atoi(viewID)
	if err != nil || !SectionAllowed(v, id) {
		return nil, core.ErrNotFound
	}
	libs := s.libraries()
	for i := range libs {
		if libs[i].ID == id {
			return &libs[i], nil
		}
	}
	return nil, core.ErrNotFound
}

func activityLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	return min(limit, 500)
}

func (s *Service) LatestAddedGrouped(ctx context.Context, viewer *core.Viewer, device core.Device, viewID string, limit int, group bool) ([]frontend.LatestItem, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	return s.latestAdded(ctx, viewer, device, viewID, limit, group)
}

// LatestAdded returns library roots, with TV grouped as series.
func (s *Service) LatestAdded(ctx context.Context, viewer *core.Viewer, device core.Device, viewID string, limit int) ([]frontend.LatestItem, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	return s.latestAdded(ctx, viewer, device, viewID, limit, true)
}

func (s *Service) resolveActivityLibrary(ctx context.Context, v *core.User, viewID string) (core.Library, core.MediaType, error) {
	id, err := strconv.Atoi(viewID)
	if err != nil {
		return core.Library{}, "", core.ErrNotFound
	}
	lib, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(id))
	if err != nil {
		return core.Library{}, "", core.ErrNotFound
	}
	// Match LibraryItems: collection libraries remain queryable while
	// console-only unless hidden; every other source must be published to the
	// protocol asking.
	mt := filterMediaType(lib)
	if lib.Source != core.LibraryCollection {
		var ok bool
		mt, _, ok = s.publishedLibrary(ctx, lib)
		if !ok {
			return core.Library{}, "", core.ErrNotFound
		}
	} else if lib.Hidden {
		return core.Library{}, "", core.ErrNotFound
	}
	if !v.AllowsLibrary(lib.ID) {
		return core.Library{}, "", core.ErrNotFound
	}
	return lib, mt, nil
}

func isUnscopedBuiltinLibrary(lib core.Library, f core.Filter) bool {
	return (lib.ID == core.LibraryMovies || lib.ID == core.LibraryShows) &&
		lib.Source != core.LibraryCollection &&
		lib.Source != core.LibraryPlugin &&
		!filterHasLibraryScope(f)
}

func (s *Service) latestAdded(ctx context.Context, viewer *core.Viewer, _ core.Device, viewID string, limit int, group bool) ([]frontend.LatestItem, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	limit = activityLimit(limit)
	if s.d.PluginLibraries != nil {
		if viewID != "" {
			lib, mt, err := s.resolveActivityLibrary(ctx, v, viewID)
			if err != nil {
				return nil, err
			}
			items, err := s.latestFromLibrary(ctx, v, lib, mt, limit, group)
			if err != nil {
				return nil, err
			}
			return sortLatestItems(items, limit), nil
		}
		libs, err := s.d.PluginLibraries.Libraries(ctx)
		if err != nil {
			return nil, err
		}
		var result []frontend.LatestItem
		for _, lib := range libs {
			mt, _, ok := s.publishedLibrary(ctx, lib)
			if !ok || !v.AllowsLibrary(lib.ID) {
				continue
			}
			found, err := s.latestFromLibrary(ctx, v, lib, mt, limit, group)
			if errors.Is(err, core.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			result = append(result, found...)
		}
		return sortLatestItems(dedupeLatestItems(result), limit), nil
	}
	lib, err := s.activityLibrary(v, viewID)
	if err != nil {
		return nil, err
	}
	if s.d.Browse == nil || s.d.AddedTimes == nil {
		return nil, core.ErrNotAvailable
	}
	var result []frontend.LatestItem
	libraries := s.libraries()
	if lib != nil {
		libraries = []Library{*lib}
	}
	for _, library := range libraries {
		if !SectionAllowed(v, library.ID) {
			continue
		}
		if group && s.d.RecentAir != nil && (library.Type == core.TypeMovie || library.Type == core.TypeShow) {
			found, err := s.latestFromRecent(ctx, v, library, limit)
			if err != nil {
				return nil, err
			}
			result = append(result, found...)
			continue
		}
		found, err := s.latestFromBrowseScan(ctx, v, core.Filter{Type: library.Type, KnownAddedDate: true}, limit, false, library.Type == core.TypeShow && !group)
		if err != nil {
			return nil, err
		}
		result = append(result, found...)
	}
	return sortLatestItems(dedupeLatestItems(result), limit), nil
}

func (s *Service) latestFromLibrary(ctx context.Context, v *core.User, lib core.Library, mt core.MediaType, limit int, group bool) ([]frontend.LatestItem, error) {
	if lib.Source == core.LibraryPlugin {
		return s.latestFromPluginLibrary(ctx, v, lib, limit)
	}
	if lib.Source == core.LibraryCollection && lib.CollectionID <= 0 {
		return nil, nil
	}
	if mt != core.TypeMovie && mt != core.TypeShow {
		return nil, nil
	}
	if s.d.Browse == nil {
		return nil, core.ErrNotAvailable
	}
	baseFilter := s.libraryBrowseFilter(ctx, v, lib, core.Filter{KnownAddedDate: true})
	library := &Library{ID: int(lib.ID), Name: lib.Name, Type: mt, Virtual: true}
	unscoped := isUnscopedBuiltinLibrary(lib, baseFilter)
	baseFilter.Type = mt
	if unscoped {
		if group && s.d.RecentAir != nil {
			return s.latestFromRecent(ctx, v, *library, limit)
		}
		if !group && mt == core.TypeShow {
			if s.d.AddedTimes == nil {
				return nil, core.ErrNotAvailable
			}
			return s.latestFromBrowseScan(ctx, v, baseFilter, limit, false, true)
		}
	}
	if !group && mt == core.TypeShow {
		return s.latestScopedEpisodes(ctx, v, baseFilter, limit)
	}
	req := frontend.ItemQuery{
		Filter:  baseFilter,
		Sort:    core.SortAdded,
		Desc:    true,
		Limit:   limit,
		NoTotal: true,
	}
	if playedOnly, filterPlayed := frontend.LatestPlayedFilter(ctx); filterPlayed {
		req.Watched = &playedOnly
	}
	if page, ok, err := s.indexedLibraryPage(ctx, v, library, req); err != nil {
		return nil, err
	} else if ok {
		return s.latestItemsWithAdded(ctx, page.Items)
	}
	return s.latestFromBrowseScan(ctx, v, baseFilter, limit, true, false)
}

func (s *Service) latestScopedEpisodes(ctx context.Context, v *core.User, baseFilter core.Filter, limit int) ([]frontend.LatestItem, error) {
	epFilter := baseFilter
	epFilter.Type = core.TypeEpisode
	epFilter.KnownAddedDate = true
	req := frontend.ItemQuery{
		Filter:  baseFilter,
		Sort:    core.SortAdded,
		Desc:    true,
		Limit:   limit,
		NoTotal: true,
	}
	req.Filter.Type = ""
	if playedOnly, filterPlayed := frontend.LatestPlayedFilter(ctx); filterPlayed {
		req.Watched = &playedOnly
	}
	if s.d.PageExclusions != nil {
		ex := s.d.PageExclusions(ctx, v, core.TypeEpisode)
		if ex.Err != nil {
			return nil, ex.Err
		}
		filter, fits, none := viewerBrowseFilter(epFilter, v, ex, core.TypeEpisode)
		if none {
			return nil, nil
		}
		if fits {
			child := filter
			child.ChildVisibility = nil
			filter.ChildVisibility = &child
			var err error
			filter, fits, none, err = s.userStateFilter(ctx, v, filter, req)
			if err != nil {
				return nil, err
			}
			if none {
				return nil, nil
			}
			if fits {
				page, err := s.d.Browse(ctx, core.BrowseQuery{Filter: filter, Sort: core.SortAdded, Desc: true, Limit: limit, NoTotal: true})
				if err != nil {
					return nil, err
				}
				if s.pageStateHolds(ctx, v, page.Items, req) {
					check := req
					check.Watched = nil
					if s.d.ParentTitles != nil {
						kept, err := s.visibleEpisodes(ctx, v, page.Items, check, ex.AvailabilityInSQL, true)
						if err != nil {
							return nil, err
						}
						if len(kept) == len(page.Items) || len(page.Items) < limit {
							return s.latestItemsWithAdded(ctx, kept)
						}
					} else if s.indexedPageHolds(ctx, v, page.Items, check, ex.AvailabilityInSQL) {
						return s.latestItemsWithAdded(ctx, page.Items)
					}
				}
			}
		}
	}
	return s.latestFromBrowseScan(ctx, v, epFilter, limit, true, false)
}

func (s *Service) latestItemsWithAdded(ctx context.Context, items []core.Title) ([]frontend.LatestItem, error) {
	if len(items) == 0 {
		return nil, nil
	}
	var missing []core.RatingKey
	for i := range items {
		if items[i].AddedAt.IsZero() {
			missing = append(missing, items[i].Key)
		}
	}
	var times map[core.RatingKey]time.Time
	if len(missing) > 0 {
		if s.d.AddedTimes == nil {
			return nil, core.ErrNotAvailable
		}
		var err error
		times, err = s.d.AddedTimes(ctx, missing)
		if err != nil {
			return nil, err
		}
	}
	out := make([]frontend.LatestItem, 0, len(items))
	for i := range items {
		at := items[i].AddedAt
		if at.IsZero() {
			at = times[items[i].Key]
		}
		if at.IsZero() {
			continue
		}
		value := at
		out = append(out, frontend.LatestItem{Title: items[i], AddedAt: &value})
	}
	return out, nil
}

func (s *Service) latestFromBrowseScan(ctx context.Context, v *core.User, filter core.Filter, limit int, skipSectionCheck, useAiredEpisodes bool) ([]frontend.LatestItem, error) {
	vis := s.d.Visibility
	if skipSectionCheck {
		vis = withoutSectionCheck(vis)
	}
	playedOnly, filterPlayed := frontend.LatestPlayedFilter(ctx)
	var result []frontend.LatestItem
	for offset := 0; offset < activityScanLimit; {
		fetch := min(max(limit*2, 50), 500)
		var page core.Page
		var err error
		if useAiredEpisodes {
			if s.d.AiredEpisodes == nil {
				return nil, core.ErrNotAvailable
			}
			page, err = s.d.AiredEpisodes(ctx, 0, offset, fetch)
		} else {
			page, err = s.d.Browse(ctx, core.BrowseQuery{Filter: filter, Sort: core.SortAdded, Desc: true, Offset: offset, Limit: fetch, NoTotal: true})
		}
		if err != nil {
			return nil, err
		}
		var missingTimes []core.RatingKey
		for i := range page.Items {
			if page.Items[i].AddedAt.IsZero() {
				missingTimes = append(missingTimes, page.Items[i].Key)
			}
		}
		var times map[core.RatingKey]time.Time
		if len(missingTimes) > 0 {
			if s.d.AddedTimes == nil {
				return nil, core.ErrNotAvailable
			}
			times, err = s.d.AddedTimes(ctx, missingTimes)
			if err != nil {
				return nil, err
			}
		}
		var watched map[core.RatingKey]bool
		if filterPlayed {
			if s.d.State == nil {
				return nil, core.ErrNotAvailable
			}
			watched, err = s.titlesWatched(ctx, v, page.Items)
			if err != nil {
				return nil, err
			}
		}
		for i := range page.Items {
			at := page.Items[i].AddedAt
			if at.IsZero() {
				at = times[page.Items[i].Key]
			}
			if at.IsZero() {
				continue
			}
			var allowed bool
			if skipSectionCheck {
				allowed = vis.Visible(ctx, v, &page.Items[i])
			} else {
				allowed = s.visible(ctx, v, &page.Items[i])
			}
			if !allowed {
				continue
			}
			if filterPlayed && watched[page.Items[i].Key] != playedOnly {
				continue
			}
			value := at
			result = append(result, frontend.LatestItem{Title: page.Items[i], AddedAt: &value})
			if len(result) == limit {
				break
			}
		}
		offset += len(page.Items)
		if len(result) == limit {
			break
		}
		if useAiredEpisodes {
			if offset >= page.Total {
				break
			}
			if len(page.Items) == 0 {
				return nil, errors.New("experience: incomplete latest catalogue page")
			}
		} else if len(page.Items) < fetch || (page.TotalExact && offset >= page.Total) {
			break
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if offset >= activityScanLimit {
			return nil, errors.New("experience: latest catalogue scan limit reached")
		}
	}
	return result, nil
}

func dedupeLatestItems(items []frontend.LatestItem) []frontend.LatestItem {
	if len(items) < 2 {
		return items
	}
	seen := make(map[core.RatingKey]int, len(items))
	out := make([]frontend.LatestItem, 0, len(items))
	for _, item := range items {
		if idx, ok := seen[item.Title.Key]; ok {
			if item.AddedAt != nil && (out[idx].AddedAt == nil || item.AddedAt.After(*out[idx].AddedAt)) {
				out[idx] = item
			}
			continue
		}
		seen[item.Title.Key] = len(out)
		out = append(out, item)
	}
	return out
}

func sortLatestItems(result []frontend.LatestItem, limit int) []frontend.LatestItem {
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].AddedAt == nil {
			return false
		}
		if result[j].AddedAt == nil {
			return true
		}
		return result[i].AddedAt.After(*result[j].AddedAt)
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

// latestFromPluginLibrary returns the newest root items in one published plugin
// library in O(limit) SQL via the plugin item store's added_at index.
func (s *Service) latestFromPluginLibrary(ctx context.Context, v *core.User, lib core.Library, limit int) ([]frontend.LatestItem, error) {
	q := frontend.ItemQuery{
		Sort:    core.SortAdded,
		Desc:    true,
		Limit:   limit,
		NoTotal: true,
	}
	if playedOnly, filterPlayed := frontend.LatestPlayedFilter(ctx); filterPlayed {
		q.Watched = &playedOnly
	}
	page, err := s.pluginLibraryItems(ctx, v, lib, q)
	if err != nil {
		return nil, err
	}
	out := make([]frontend.LatestItem, 0, len(page.Items))
	for i := range page.Items {
		item := page.Items[i]
		var addedAt *time.Time
		if item.PluginAddedAt > 0 {
			t := time.Unix(item.PluginAddedAt, 0).UTC()
			addedAt = &t
		} else if !item.AddedAt.IsZero() {
			t := item.AddedAt
			addedAt = &t
		}
		out = append(out, frontend.LatestItem{Title: item, AddedAt: addedAt})
	}
	return out, nil
}

// UnplayedEpisodeCounts is the aired-episode badge for a list of shows and
// seasons. It does not load those episodes.
func (s *Service) UnplayedEpisodeCounts(ctx context.Context, viewer *core.Viewer, keys []core.RatingKey) (map[core.RatingKey]int, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if s == nil || s.d.CountUnplayedAired == nil {
		return nil, core.ErrNotAvailable
	}
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	return s.d.CountUnplayedAired(ctx, v.ID, keys, v.MaxContentRating)
}

// latestFromRecent fills one library from the indexed air window. It stops
// after a few windows so a played filter cannot walk the catalogue.
func (s *Service) latestFromRecent(ctx context.Context, v *core.User, library Library, limit int) ([]frontend.LatestItem, error) {
	window := min(max(limit, 16), 64)
	var result []frontend.LatestItem
	for offset := 0; offset < 256 && len(result) < limit; {
		titles, done, err := s.d.RecentAir(ctx, library.Type, offset, window)
		if err != nil {
			return nil, err
		}
		if len(titles) == 0 {
			break
		}
		var watched map[core.RatingKey]bool
		playedOnly, filterPlayed := frontend.LatestPlayedFilter(ctx)
		if filterPlayed {
			if s.d.State == nil {
				return nil, core.ErrNotAvailable
			}
			watched, err = s.titlesWatched(ctx, v, titles)
			if err != nil {
				return nil, err
			}
		}
		visible, err := s.allowTitles(ctx, v, titles)
		if err != nil {
			return nil, err
		}
		for _, title := range visible {
			if title.AddedAt.IsZero() || (filterPlayed && watched[title.Key] != playedOnly) {
				continue
			}
			value := title.AddedAt
			result = append(result, frontend.LatestItem{Title: title, AddedAt: &value})
			if len(result) == limit {
				break
			}
		}
		if done {
			break
		}
		offset += window
	}
	return result, nil
}

// Resume reads only in-progress state, without next-up or Home Designer data.
func (s *Service) Resume(ctx context.Context, viewer *core.Viewer, _ core.Device, viewID string, types []core.MediaType, offset, limit int, requireExact bool) (frontend.ActivityPage, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	var page frontend.ActivityPage
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return page, err
	}
	lib, err := s.activityLibrary(v, viewID)
	if err != nil {
		return page, err
	}
	if s.d.State == nil || s.d.CachedGet == nil {
		return page, core.ErrNotAvailable
	}
	if offset < 0 || offset > activityScanLimit {
		return page, errors.New("experience: invalid resume offset")
	}
	limit = activityLimit(limit)
	visibility := s.d.Visibility
	visibility.GetTitle = s.d.CachedGet
	target := offset + limit + 1
	fetch := min(max(target*2, 50), activityScanLimit+1)
	processed := 0
	var items []core.Title
	for {
		states, err := s.d.State.InProgress(ctx, v.ID, fetch)
		if err != nil {
			return page, err
		}
		if len(states) < processed {
			return page, errors.New("experience: resume state changed during scan")
		}
		// The batch's titles and their series in two reads, not one read
		// (and one hydration) per in-progress title.
		cached := s.prefetchActivity(ctx, states[processed:])
		visibility.GetTitle = func(ctx context.Context, key core.RatingKey) (*core.Title, error) {
			if t, ok := cached[key]; ok {
				return &t, nil
			}
			return s.d.CachedGet(ctx, key)
		}
		var libMembers map[core.RatingKey]bool
		var libScoped bool
		if lib != nil && s.d.LibraryMembers != nil && len(states[processed:]) > 0 {
			batchKeys := make([]core.RatingKey, 0, len(states[processed:]))
			for _, st := range states[processed:] {
				batchKeys = append(batchKeys, st.Key)
			}
			if m, _, ok, err := s.d.LibraryMembers(ctx, core.LibraryID(lib.ID), batchKeys); err == nil && ok {
				libMembers, libScoped = m, true
			}
		}
		for _, status := range states[processed:] {
			if err = ctx.Err(); err != nil {
				return page, err
			}
			if libScoped && !libMembers[status.Key] {
				continue
			}
			title, err := visibility.GetTitle(ctx, status.Key)
			if errors.Is(err, core.ErrNotFound) {
				continue
			}
			if err != nil {
				return page, err
			}
			if title == nil || title.Key != status.Key || visibility.SectionFor == nil {
				continue
			}
			section, ok := visibility.SectionFor(title.Type)
			if !ok || !SectionAllowed(v, section) || (lib != nil && section != lib.ID) || !visibility.Visible(ctx, v, title) {
				continue
			}
			if len(types) > 0 {
				matches := false
				for _, typ := range types {
					if typ == title.Type {
						matches = true
						break
					}
				}
				if !matches {
					continue
				}
			}
			items = append(items, *title)
			if !requireExact && len(items) == target {
				page.Items, page.Total, page.More = items[offset:offset+limit], len(items), true
				return page, nil
			}
		}
		processed = len(states)
		if len(states) < fetch {
			page.Total, page.TotalExact = len(items), true
			start := min(offset, len(items))
			page.Items = items[start:min(start+limit, len(items))]
			page.More = start+len(page.Items) < len(items)
			return page, nil
		}
		if fetch > activityScanLimit {
			return page, errors.New("experience: resume scan limit reached")
		}
		fetch = min(fetch*2, activityScanLimit+1)
	}
}

var _ frontend.LibraryActivity = (*Service)(nil)
var _ frontend.LibraryActivityGrouping = (*Service)(nil)

// prefetchActivity reads the titles of states, and the series of their
// episodes, in batches; what it misses is read one at a time by the caller.
func (s *Service) prefetchActivity(ctx context.Context, states []core.WatchStatus) map[core.RatingKey]core.Title {
	if s.d.CachedGetMany == nil || len(states) == 0 {
		return nil
	}
	keys := make([]core.RatingKey, 0, len(states))
	for _, st := range states {
		keys = append(keys, st.Key)
	}
	titles, err := s.d.CachedGetMany(ctx, keys)
	if err != nil {
		return nil
	}
	if titles == nil {
		titles = make(map[core.RatingKey]core.Title, len(keys))
	}
	// Remember negative results too. Deleted or policy-hidden watch rows
	// must not turn a successful batch into one lookup per missing title.
	for _, key := range keys {
		if _, ok := titles[key]; !ok {
			titles[key] = core.Title{}
		}
	}
	var shows []core.RatingKey
	seen := map[core.RatingKey]bool{}
	for _, t := range titles {
		if t.ShowKey != 0 && !seen[t.ShowKey] {
			if _, ok := titles[t.ShowKey]; !ok {
				seen[t.ShowKey] = true
				shows = append(shows, t.ShowKey)
			}
		}
	}
	if len(shows) > 0 {
		if parents, err := s.d.CachedGetMany(ctx, shows); err == nil {
			for k, t := range parents {
				titles[k] = t
			}
		}
	}
	return titles
}

// NextUp is a watch-state read, independent of discovery home rows.
func (s *Service) NextUp(ctx context.Context, viewer *core.Viewer, _ core.Device, limit int) ([]core.Title, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	if s.d.NextUp == nil {
		return nil, core.ErrNotAvailable
	}
	titles, err := s.d.NextUp(ctx, v, activityLimit(limit))
	if err != nil {
		return nil, err
	}
	// The trusted activity source returns viewer-authorized episodes. Keep
	// that request's verdict for the immediately following user-data batch.
	if memo := frontend.RequestMemoFrom(ctx); memo != nil {
		memo.Mu.Lock()
		if memo.Viewer != nil && memo.Viewer.ID == v.ID {
			if memo.Visible == nil {
				memo.Visible = map[core.RatingKey]bool{}
			}
			for _, title := range titles {
				memo.Visible[title.Key] = true
			}
		}
		memo.Mu.Unlock()
	}
	return titles, nil
}
