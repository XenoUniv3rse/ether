package livetv

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func (s *Service) catchUpChannels(ctx context.Context, u *core.User, first bool) (context.Context, []Channel, error) {
	ctx, pol, err := s.withPolicy(ctx, u)
	if err != nil || pol.Disabled {
		return ctx, nil, err
	}
	_, snap, err := s.channelSnapshot(ctx)
	if err != nil {
		return ctx, nil, err
	}
	snap.mu.Lock()
	archived := snap.archived
	snap.mu.Unlock()
	var out []Channel
	for _, ch := range archived {
		if s.allowedWithLookup(ctx, u, ch, nil, nil, false) {
			out = append(out, ch)
			if first {
				break
			}
		}
	}
	return ctx, out, nil
}

func (h liveTVHost) CatchUpLibraryVisible(ctx context.Context, u *frontend.Viewer) (bool, error) {
	_, channels, err := h.s.catchUpChannels(ctx, u, true)
	return len(channels) > 0, err
}

func catchUpLibraryItem(p Programme, kind frontend.MediaType) frontend.CatchUpItem {
	out := frontend.CatchUpItem{
		Type: kind, Programme: toFrontendProgramme(p),
		ShowID:   frontend.CatchUpGroupID(frontend.TypeShow, p.Item.Title),
		SeasonID: frontend.CatchUpGroupID(frontend.TypeSeason, p.Item.Title),
	}
	switch kind {
	case frontend.TypeShow:
		out.ID = out.ShowID
	case frontend.TypeSeason:
		out.ID = out.SeasonID
	case frontend.TypeEpisode:
		out.ID = frontend.CatchUpEpisodeID(p.ID)
	}
	return out
}

func (h liveTVHost) CatchUpItem(ctx context.Context, u *frontend.Viewer, id string) (frontend.CatchUpItem, error) {
	s := h.s
	if key, start, ok := frontend.CatchUpGUIDAiring(id); ok {
		_, snapshot, err := s.channelSnapshot(ctx)
		if err != nil {
			return frontend.CatchUpItem{}, err
		}
		snapshot.mu.Lock()
		channel := snapshot.archiveIDs[key]
		snapshot.mu.Unlock()
		if channel == "" {
			return frontend.CatchUpItem{}, core.ErrNotFound
		}
		id = frontend.CatchUpEpisodeID(fmt.Sprintf("%s-%d", channel, start.UnixMilli()))
	}
	_, groupKind := frontend.CatchUpGUIDGroup(id)
	if req, ok := frontend.CatchUpEpisodeRequest(id); ok {
		ctx, _, err := s.withPolicy(ctx, u)
		if err != nil {
			return frontend.CatchUpItem{}, err
		}
		ch, err := s.Channel(ctx, req.ChannelID)
		if err != nil {
			return frontend.CatchUpItem{}, err
		}
		stamp, _ := strconv.ParseInt(strings.TrimPrefix(req.ProgrammeID, ch.ID+"-"), 10, 64)
		now := s.clockNow()
		if ch.ArchiveDays() <= 0 || time.UnixMilli(stamp).Before(now.Add(-time.Duration(ch.ArchiveDays())*24*time.Hour)) ||
			!s.allowedWithLookup(ctx, u, ch, nil, nil, false) {
			return frontend.CatchUpItem{}, core.ErrNotFound
		}
		p, found, err := s.programmeAt(ctx, ch, time.UnixMilli(stamp))
		if err != nil {
			return frontend.CatchUpItem{}, err
		}
		eligible, _ := frontend.ArchiveFlags(ch.ArchiveDays(), p.Start, p.End, now)
		if !found || p.ID != req.ProgrammeID || !eligible || !s.allowedProgramme(ctx, u, ch, &p) {
			return frontend.CatchUpItem{}, core.ErrNotFound
		}
		return catchUpLibraryItem(p, frontend.TypeEpisode), nil
	}
	if groupKind == "" && !strings.HasPrefix(id, "catchup-show-") && !strings.HasPrefix(id, "catchup-season-") {
		return frontend.CatchUpItem{}, core.ErrNotFound
	}
	page, err := h.catchUpItems(ctx, u, frontend.CatchUpQuery{Limit: 1}, id)
	if err != nil {
		return frontend.CatchUpItem{}, err
	}
	if len(page.Items) == 0 {
		return frontend.CatchUpItem{}, core.ErrNotFound
	}
	return page.Items[0], nil
}

func (h liveTVHost) CatchUpItems(ctx context.Context, u *frontend.Viewer, q frontend.CatchUpQuery) (frontend.CatchUpPage, error) {
	if q.Sort == frontend.CatchUpSortRecent {
		return h.catchUpItems(ctx, u, q, "")
	}
	// Any other order needs the whole listing; the archive window keeps it
	// to a few thousand rows.
	all := q
	all.Offset, all.Limit, all.Sort = 0, catchUpUnlimited, frontend.CatchUpSortRecent
	page, err := h.catchUpItems(ctx, u, all, "")
	if err != nil {
		return frontend.CatchUpPage{}, err
	}
	sortCatchUpItems(page.Items, q.Sort)
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 1000)
	out := frontend.CatchUpPage{Total: len(page.Items)}
	if q.Offset < len(page.Items) {
		out.Items = page.Items[q.Offset:min(len(page.Items), q.Offset+limit)]
	}
	return out, nil
}

// catchUpUnlimited asks catchUpItems for every matching row.
const catchUpUnlimited = -1

// sortCatchUpItems reorders a newest-first listing. Equal titles keep their
// newest-first order.
func sortCatchUpItems(items []frontend.CatchUpItem, order frontend.CatchUpSort) {
	switch order {
	case frontend.CatchUpSortOldest:
		slices.Reverse(items)
	case frontend.CatchUpSortTitle, frontend.CatchUpSortTitleDesc:
		slices.SortStableFunc(items, func(a, b frontend.CatchUpItem) int {
			c := cmp.Compare(catchUpSortTitle(a), catchUpSortTitle(b))
			if order == frontend.CatchUpSortTitleDesc {
				c = -c
			}
			return c
		})
	}
}

func catchUpSortTitle(item frontend.CatchUpItem) string {
	title := item.Programme.Item.Title
	if item.Type == frontend.TypeEpisode && item.Programme.Item.EpisodeTitle != "" {
		title += " " + item.Programme.Item.EpisodeTitle
	}
	return strings.ToLower(title)
}

func (h liveTVHost) catchUpItems(ctx context.Context, u *core.User, q frontend.CatchUpQuery, exact string) (frontend.CatchUpPage, error) {
	var out frontend.CatchUpPage
	ctx, channels, err := h.s.catchUpChannels(ctx, u, false)
	if err != nil || len(channels) == 0 {
		return out, err
	}
	parent, parentKind := frontend.CatchUpGUIDGroup(q.Parent)
	exactGUID, exactKind := frontend.CatchUpGUIDGroup(exact)
	show := parentKind == frontend.TypeShow || strings.HasPrefix(q.Parent, "catchup-show-")
	season := parentKind == frontend.TypeSeason || strings.HasPrefix(q.Parent, "catchup-season-")
	if q.Parent != "" && q.Parent != frontend.CatchUpLibraryID && !show && !season {
		return out, nil
	}
	kind := frontend.TypeShow
	switch {
	case q.Recursive || season:
		kind = frontend.TypeEpisode
	case show || exactKind == frontend.TypeSeason || strings.HasPrefix(exact, "catchup-season-"):
		kind = frontend.TypeSeason
	}
	limit := q.Limit
	switch {
	case limit == catchUpUnlimited:
		limit = int(^uint(0) >> 1)
	case limit <= 0:
		limit = 100
	default:
		limit = min(limit, 1000)
	}
	now := h.s.clockNow()
	seen := map[string]bool{}
	search := catchUpSearchKey(q.Search)
	_, policy, err := h.s.withPolicy(ctx, u)
	if err != nil {
		return out, err
	}
	maxDays := 0
	ratings := make(map[string]string)
	for _, ch := range channels {
		if ch.GuideID == "" {
			continue
		}
		maxDays = max(maxDays, ch.ArchiveDays())
		if u.MaxContentRating != "" || policy.MaxRating != "" {
			ratings[ch.ID] = h.s.channelContentRating(ctx, ch, nil)
		}
	}
	earliest := now.Add(-time.Duration(maxDays) * 24 * time.Hour)
	// Each batch is a disjoint start-time interval. Process the newest
	// interval first so small pages do not read the entire archive.
	for until := now.Add(time.Millisecond); maxDays > 0 && until.After(earliest); {
		from := until.Add(-12 * time.Hour)
		rows, err := h.s.catchUpWindow(ctx, u, policy, channels, ratings, from, until, now)
		if err != nil {
			return frontend.CatchUpPage{}, err
		}
		for _, p := range rows {
			item := catchUpLibraryItem(p, kind)
			if exact != "" && item.ID != exact && (exactGUID == "" || frontend.CatchUpGroupGUID(item.ID) != exactGUID) ||
				show && item.ShowID != q.Parent && (parent == "" || frontend.CatchUpGroupGUID(item.ShowID) != parent) ||
				season && item.SeasonID != q.Parent && (parent == "" || frontend.CatchUpGroupGUID(item.SeasonID) != parent) {
				continue
			}
			if search != "" && !strings.Contains(catchUpSearchKey(p.Item.Title+" "+p.Item.EpisodeTitle), search) {
				continue
			}
			if kind != frontend.TypeEpisode {
				if seen[item.ID] {
					continue
				}
				seen[item.ID] = true
			}
			if out.Total >= q.Offset && len(out.Items) < limit {
				out.Items = append(out.Items, item)
			}
			out.Total++
			if (exact != "" || kind == frontend.TypeSeason && q.Parent != "") || len(out.Items) == limit && out.Total-q.Offset > limit {
				return out, nil
			}
		}
		until = from
	}
	return out, nil
}

func (s *Service) catchUpWindow(ctx context.Context, u *core.User, policy Policy, channels []Channel, ratings map[string]string, from, until, now time.Time) ([]Programme, error) {
	var out []Programme
	byID := make(map[string]Channel, len(channels))
	for batch := range slices.Chunk(channels, 100) {
		values := make([]string, 0, len(batch))
		args := make([]any, 0, 4*len(batch)+2)
		for _, ch := range batch {
			earliest := now.Add(-time.Duration(ch.ArchiveDays()) * 24 * time.Hour)
			if ch.GuideID == "" || !earliest.Before(until) {
				continue
			}
			values = append(values, "(CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS BIGINT))")
			args = append(args, ch.ID, guideSource(ch), ch.GuideID, max(from.UnixMilli(), earliest.UnixMilli()))
			byID[ch.ID] = ch
		}
		if len(values) == 0 {
			continue
		}
		args = append(args, until.UnixMilli(), now.UnixMilli())
		rows, err := s.o.DB.QueryContext(ctx, `WITH archive(id, source, guide, earliest) AS (VALUES `+strings.Join(values, ",")+`)
			SELECT archive.id, p.data FROM archive JOIN livetv_programmes p
			ON p.source_id=archive.source AND p.channel_id=archive.guide AND p.start_ms>=archive.earliest
			WHERE p.start_ms<? AND p.end_ms<=? AND p.end_ms>p.start_ms`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var raw []byte
			var p Programme
			if err = rows.Scan(&id, &raw); err == nil {
				err = json.Unmarshal(raw, &p)
			}
			if err != nil {
				break
			}
			ch := byID[id]
			eligible, _ := frontend.ArchiveFlags(ch.ArchiveDays(), p.Start, p.End, now)
			if !eligible || p.Item.Title == "" {
				continue
			}
			p.ChannelID = id
			p.ID = fmt.Sprintf("%s-%d", id, p.Start.UnixMilli())
			out = append(out, p)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	lookup, err := s.catchUpTitleFacts(ctx, out)
	if err != nil {
		return nil, err
	}
	out = slices.DeleteFunc(out, func(p Programme) bool {
		ch := byID[p.ChannelID]
		rating := resolveChannelRating(ch, &p, "", ratings[ch.ID], false)
		return !s.allowedProgrammeContent(ctx, u, ch, &p, policy, rating, lookup, s.o.Catalog != nil)
	})
	slices.SortFunc(out, func(a, b Programme) int {
		if n := b.Start.Compare(a.Start); n != 0 {
			return n
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out, nil
}

// Imported airings carry their own policy facts. A linked airing also needs
// its local title and parent facts, loaded in batches for the current window.
func (s *Service) catchUpTitleFacts(ctx context.Context, programmes []Programme) (func(context.Context, core.RatingKey) (*core.Title, error), error) {
	catalog, ok := s.o.Catalog.(interface {
		CachedGetMany(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)
	})
	if !ok {
		return nil, nil
	}
	var keys []core.RatingKey
	seen := make(map[core.RatingKey]bool)
	for _, p := range programmes {
		if p.Item.Key != 0 && !seen[p.Item.Key] {
			keys = append(keys, p.Item.Key)
			seen[p.Item.Key] = true
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	titles := make(map[core.RatingKey]core.Title, len(keys))
	fill := func(keys []core.RatingKey) error {
		for batch := range slices.Chunk(keys, 200) {
			found, err := catalog.CachedGetMany(ctx, batch)
			if err != nil {
				return err
			}
			for key, title := range found {
				titles[key] = title
			}
		}
		return nil
	}
	if err := fill(keys); err != nil {
		return nil, err
	}
	var parents []core.RatingKey
	for _, title := range titles {
		if title.ShowKey != 0 && !seen[title.ShowKey] {
			parents = append(parents, title.ShowKey)
			seen[title.ShowKey] = true
		}
	}
	if err := fill(parents); err != nil {
		return nil, err
	}
	return func(_ context.Context, key core.RatingKey) (*core.Title, error) {
		title, ok := titles[key]
		if !ok {
			return nil, core.ErrNotFound
		}
		return &title, nil
	}, nil
}

func (h liveTVHost) FindCatchUp(ctx context.Context, viewer *frontend.Viewer, req CatchUpRequest, caps frontend.ClientCaps, clientSession string) (CatchUpInfo, bool) {
	if viewer == nil || clientSession == "" {
		return CatchUpInfo{}, false
	}
	h.s.mu.Lock()
	var candidate CatchUpInfo
	for _, ss := range h.s.sessions {
		ss.mu.Lock()
		if !ss.closed && ss.catchUp != nil && ss.info.UserID == viewer.ID &&
			ss.info.ClientSessionID == clientSession && ss.caps.ClientID == caps.ClientID && ss.caps.Protocol == caps.Protocol {
			info := ss.catchUp.info
			if info.ChannelID == req.ChannelID && info.ProgrammeID == req.ProgrammeID && info.DASH == req.DASH && !info.StartOver {
				candidate = info
			}
		}
		ss.mu.Unlock()
		if candidate.ID != "" {
			break
		}
	}
	h.s.mu.Unlock()
	if candidate.ID == "" {
		return CatchUpInfo{}, false
	}
	_, err := h.s.getSession(ctx, candidate.ID, viewer)
	return candidate, err == nil
}

func (h liveTVHost) LibraryCatchUpSession(access frontend.SessionAccess, id string) (CatchUpInfo, bool) {
	if access.Viewer == nil {
		return CatchUpInfo{}, false
	}
	h.s.mu.Lock()
	ss := h.s.sessions[id]
	h.s.mu.Unlock()
	if ss == nil {
		return CatchUpInfo{}, false
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.closed || ss.catchUp == nil || ss.info.UserID != access.Viewer.ID || ss.caps.Protocol != access.Protocol ||
		(access.Device.ID != "" && ss.caps.ClientID != access.Device.ID) {
		return CatchUpInfo{}, false
	}
	return ss.catchUp.info, true
}

// catchUpSearchKey folds a title or query to lower-case letters and digits,
// so "world news tonight", "World-News Tonight" and "worldnews" all match.
func catchUpSearchKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
