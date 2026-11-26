package liveevents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/livetv"
)

const (
	defaultWatchCacheCap   = 512
	defaultReverseCacheCap = 512
)

// GuideWatchQuery specifies the bounded lookup window, broadcast callsigns,
// per-event channel search terms, and viewer-favourited channel IDs.
type GuideWatchQuery struct {
	From               time.Time
	Until              time.Time
	Callsigns          []string
	EventQueries       []livetv.WatchEventQuery
	FavoriteChannelIDs []string
}

// GuideSource provides bounded indexed Live TV guide and channel access for A5 and A6.
// Production wires NewLiveTVGuideSource(livetvService); tests can supply fakes.
type GuideSource interface {
	GuideRevision(ctx context.Context, viewer *core.User) (string, error)
	WatchCandidates(ctx context.Context, viewer *core.User, q GuideWatchQuery) ([]GuideCandidate, string, error)
	CurrentProgramme(ctx context.Context, viewer *core.User, channelID string, at time.Time) (*GuideCandidate, string, error)
}

// ActiveEventsSource supplies the current set of live / near-live events (and
// a monotonically changing or content-based revision) for A6 reverse matching.
type ActiveEventsSource interface {
	ActiveEvents(ctx context.Context, viewer *core.User, at time.Time) (events []core.LiveEvent, revision string, err error)
}

// LiveTVGuideSource adapts *livetv.Service to GuideSource.
type LiveTVGuideSource struct {
	svc *livetv.Service
}

// NewLiveTVGuideSource wraps a Live TV service for bounded guide matching.
func NewLiveTVGuideSource(svc *livetv.Service) *LiveTVGuideSource {
	if svc == nil {
		return nil
	}
	return &LiveTVGuideSource{svc: svc}
}

func (g *LiveTVGuideSource) GuideRevision(ctx context.Context, viewer *core.User) (string, error) {
	if g == nil || g.svc == nil {
		return "none", nil
	}
	return g.svc.GuideRevision(ctx, viewer)
}

func (g *LiveTVGuideSource) WatchCandidates(ctx context.Context, viewer *core.User, q GuideWatchQuery) ([]GuideCandidate, string, error) {
	if g == nil || g.svc == nil {
		return nil, "none", nil
	}
	entries, rev, err := g.svc.WatchCandidatesQuery(ctx, viewer, livetv.WatchQuery{
		From:               q.From,
		Until:              q.Until,
		Callsigns:          q.Callsigns,
		EventQueries:       q.EventQueries,
		FavoriteChannelIDs: q.FavoriteChannelIDs,
	})
	if err != nil {
		return nil, "", err
	}
	out := make([]GuideCandidate, 0, len(entries))
	for _, e := range entries {
		out = append(out, guideCandidateFromLiveTV(e))
	}
	return out, rev, nil
}

func (g *LiveTVGuideSource) CurrentProgramme(ctx context.Context, viewer *core.User, channelID string, at time.Time) (*GuideCandidate, string, error) {
	if g == nil || g.svc == nil {
		return nil, "", nil
	}
	entry, key, err := g.svc.CurrentProgrammeForWatch(ctx, viewer, channelID, at)
	if err != nil || entry == nil {
		return nil, key, err
	}
	c := guideCandidateFromLiveTV(*entry)
	return &c, key, nil
}

func guideCandidateFromLiveTV(e livetv.WatchGuideEntry) GuideCandidate {
	return GuideCandidate{
		ProgrammeID:   e.Programme.ID,
		ArchiveDays:   e.ArchiveDays,
		ChannelID:     e.ChannelID,
		ChannelName:   e.ChannelName,
		ChannelNumber: e.ChannelNumber,
		ChannelLogo:   e.ChannelLogo,
		TvgName:       e.TvgName,
		GuideID:       e.GuideID,
		Group:         e.Group,
		IsEventAddon:  e.IsEventAddon,
		ChannelOnly:   e.ChannelOnly,
		Favorite:      e.Favorite,
		WatchedAt:     e.WatchedAt,
		Unhealthy:     e.Unhealthy,
		Title:         e.Programme.Item.Title,
		EpisodeTitle:  e.Programme.Item.EpisodeTitle,
		Summary:       e.Programme.Item.Summary,
		Genres:        slices.Clone(e.Programme.Item.Genres),
		Start:         e.Programme.Start,
		End:           e.Programme.End,
		Live:          e.Live,
	}
}

// WatchMatcher implements A5 ("Where to watch") and A6 (reverse channel->event
// linking), caching forward results per event + guide/lineup/preference
// revision and reverse results per programme + events revision within bounded
// memory.
type WatchMatcher struct {
	guide            GuideSource
	activeEvents     ActiveEventsSource
	favoriteEntities func(ctx context.Context, viewer *core.User) ([]string, error)
	favoriteChannels func(ctx context.Context, viewer *core.User) ([]string, error)
	now              func() time.Time

	mu           sync.Mutex
	watchCap     int
	watchCache   map[string][]core.WatchTarget
	watchOrder   []string
	reverseCap   int
	reverseCache map[string]*core.LiveEventRef
	reverseOrder []string
}

// WatchMatcherOptions configures a WatchMatcher.
type WatchMatcherOptions struct {
	Guide            GuideSource
	ActiveEvents     ActiveEventsSource
	FavoriteEntities func(ctx context.Context, viewer *core.User) ([]string, error)
	FavoriteChannels func(ctx context.Context, viewer *core.User) ([]string, error)
	Now              func() time.Time
	CacheCap         int
}

// NewWatchMatcher constructs a bounded, cached A5/A6 matcher.
func NewWatchMatcher(opts WatchMatcherOptions) *WatchMatcher {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	capVal := opts.CacheCap
	if capVal <= 0 {
		capVal = defaultWatchCacheCap
	}
	return &WatchMatcher{
		guide:            opts.Guide,
		activeEvents:     opts.ActiveEvents,
		favoriteEntities: opts.FavoriteEntities,
		favoriteChannels: opts.FavoriteChannels,
		now:              nowFn,
		watchCap:         capVal,
		watchCache:       make(map[string][]core.WatchTarget),
		reverseCap:       capVal,
		reverseCache:     make(map[string]*core.LiveEventRef),
	}
}

const (
	watchBatchMaxSpan         = 6 * time.Hour
	watchBatchPrimaryCapGuard = 1800
	watchBatchMaxEvents       = 32
)

// eventWatchWindow returns the guide overlap window for ev.
// Multi-day live tournaments (such as PGA or F1) have ev.Start days earlier,
// so in-progress/delayed events outside [start-30m, start+4h] anchor around now.
func eventWatchWindow(ev core.LiveEvent, now time.Time) (time.Time, time.Time, bool) {
	before := time.Duration(WatchWindowBefore) * time.Second
	after := time.Duration(WatchWindowAfter) * time.Second
	start := parseEventTime(ev.Start)
	if ev.Status.State == core.LiveEventStateIn || ev.Status.State == core.LiveEventStateDelayed {
		if start.IsZero() || now.After(start.Add(after)) || now.Before(start.Add(-before)) {
			return now.Add(-before), now.Add(after), true
		}
	}
	if start.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	return start.Add(-before), start.Add(after), true
}

func (m *WatchMatcher) viewerFavoriteChannels(ctx context.Context, viewer *core.User) ([]string, map[string]bool, string) {
	if m == nil || m.favoriteChannels == nil || viewer == nil {
		return nil, nil, ""
	}
	ids, err := m.favoriteChannels(ctx, viewer)
	if err != nil || len(ids) == 0 {
		return nil, nil, ""
	}
	clean := make([]string, 0, len(ids))
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || set[id] {
			continue
		}
		set[id] = true
		clean = append(clean, id)
	}
	if len(clean) == 0 {
		return nil, nil, ""
	}
	slices.Sort(clean)
	h := sha256.New()
	for _, id := range clean {
		_, _ = io.WriteString(h, id)
		_, _ = io.WriteString(h, "\x00")
	}
	return clean, set, "fc:" + hex.EncodeToString(h.Sum(nil)[:8])
}

func applyFavoriteChannels(cands []GuideCandidate, favSet map[string]bool) {
	if len(favSet) == 0 {
		return
	}
	for i := range cands {
		if favSet[cands[i].ChannelID] {
			cands[i].Favorite = true
		}
	}
}

// WatchTargets resolves playable watch targets for ev (spec A5).
// External reference links (detail.Links) are intentionally excluded; only
// Live TV channels, event add-ons, and playable plugin streams are returned.
// Results are cached per event fingerprint, guide/lineup revision, and viewer
// channel-favourite revision.
func (m *WatchMatcher) WatchTargets(ctx context.Context, ev core.LiveEvent, viewer *core.User) ([]core.WatchTarget, error) {
	if m == nil {
		return nil, nil
	}
	guideRev := "none"
	if m.guide != nil {
		rev, err := m.guide.GuideRevision(ctx, viewer)
		if err != nil {
			return nil, err
		}
		guideRev = rev
	}
	favIDs, favSet, favRev := m.viewerFavoriteChannels(ctx, viewer)
	now := m.now()
	ecw := eventChannelWindowOK(ev, now)

	cacheKey := watchCacheKey(ev, guideRev, favRev, ecw)
	if cached, ok := m.getWatchCached(cacheKey, now); ok {
		return cached, nil
	}

	var candidates []GuideCandidate
	if m.guide != nil && viewer != nil {
		if winFrom, winUntil, ok := eventWatchWindow(ev, now); ok {
			q := GuideWatchQuery{
				From:               winFrom,
				Until:              winUntil,
				Callsigns:          collectEventCallsigns(ev),
				FavoriteChannelIDs: favIDs,
			}
			if eq, ok := collectEventWatchQuery(ev, now); ok {
				q.EventQueries = []livetv.WatchEventQuery{eq}
			}
			cands, rev, err := m.guide.WatchCandidates(ctx, viewer, q)
			if err != nil {
				return nil, err
			}
			candidates = cands
			applyFavoriteChannels(candidates, favSet)
			if rev != "" && rev != guideRev {
				cacheKey = watchCacheKey(ev, rev, favRev, ecw)
			}
		}
	}

	targets := matchEventToCandidatesAt(ev, candidates, now)
	m.putWatchCached(cacheKey, targets)
	return cloneWatchTargets(targets), nil
}

// WatchTargetsBatch resolves playable watch targets for many events with one
// GuideRevision check and bounded batched indexed guide/channel queries for
// cache-missed events.
func (m *WatchMatcher) WatchTargetsBatch(ctx context.Context, events []core.LiveEvent, viewer *core.User) (map[string][]core.WatchTarget, error) {
	out := make(map[string][]core.WatchTarget, len(events))
	if m == nil || len(events) == 0 {
		return out, nil
	}

	guideRev := "none"
	if m.guide != nil {
		rev, err := m.guide.GuideRevision(ctx, viewer)
		if err != nil {
			return nil, err
		}
		guideRev = rev
	}
	favIDs, favSet, favRev := m.viewerFavoriteChannels(ctx, viewer)

	type pendingEvent struct {
		ev       core.LiveEvent
		cacheKey string
		winFrom  time.Time
		winUntil time.Time
		hasWin   bool
		ecw      bool
	}

	now := m.now()
	seenID := make(map[string]struct{}, len(events))
	var missed []pendingEvent
	for _, ev := range events {
		if _, dup := seenID[ev.ID]; dup {
			continue
		}
		seenID[ev.ID] = struct{}{}
		ecw := eventChannelWindowOK(ev, now)
		ck := watchCacheKey(ev, guideRev, favRev, ecw)
		if cached, ok := m.getWatchCached(ck, now); ok {
			out[ev.ID] = cached
			continue
		}
		wf, wu, ok := eventWatchWindow(ev, now)
		missed = append(missed, pendingEvent{
			ev:       ev,
			cacheKey: ck,
			winFrom:  wf,
			winUntil: wu,
			hasWin:   ok,
			ecw:      ecw,
		})
	}
	if len(missed) == 0 {
		return out, nil
	}

	if m.guide == nil || viewer == nil {
		for _, p := range missed {
			targets := matchEventToCandidatesAt(p.ev, nil, now)
			m.putWatchCached(p.cacheKey, targets)
			out[p.ev.ID] = cloneWatchTargets(targets)
		}
		return out, nil
	}

	// Partition windowed missed events into time-compact buckets (<= 6h span
	// and <= watchBatchMaxEvents) so even week-long screens execute a tiny
	// bounded number of indexed queries without crowding out any event.
	var unwindowed []pendingEvent
	var windowed []pendingEvent
	for _, p := range missed {
		if !p.hasWin {
			unwindowed = append(unwindowed, p)
			continue
		}
		windowed = append(windowed, p)
	}
	for _, p := range unwindowed {
		targets := matchEventToCandidatesAt(p.ev, nil, now)
		m.putWatchCached(p.cacheKey, targets)
		out[p.ev.ID] = cloneWatchTargets(targets)
	}
	if len(windowed) == 0 {
		return out, nil
	}

	sort.SliceStable(windowed, func(i, j int) bool {
		return windowed[i].winFrom.Before(windowed[j].winFrom)
	})

	var buckets [][]pendingEvent
	for _, p := range windowed {
		if len(buckets) == 0 {
			buckets = append(buckets, []pendingEvent{p})
			continue
		}
		last := buckets[len(buckets)-1]
		minFrom := last[0].winFrom
		maxUntil := last[0].winUntil
		for _, item := range last[1:] {
			if item.winFrom.Before(minFrom) {
				minFrom = item.winFrom
			}
			if item.winUntil.After(maxUntil) {
				maxUntil = item.winUntil
			}
		}
		candMax := maxUntil
		if p.winUntil.After(candMax) {
			candMax = p.winUntil
		}
		if len(last) < watchBatchMaxEvents && candMax.Sub(minFrom) <= watchBatchMaxSpan {
			buckets[len(buckets)-1] = append(last, p)
		} else {
			buckets = append(buckets, []pendingEvent{p})
		}
	}

	for _, bucket := range buckets {
		minFrom, maxUntil := bucket[0].winFrom, bucket[0].winUntil
		var unionCallsigns []string
		seenCS := make(map[string]struct{})
		var unionEventQueries []livetv.WatchEventQuery
		for _, p := range bucket {
			if p.winFrom.Before(minFrom) {
				minFrom = p.winFrom
			}
			if p.winUntil.After(maxUntil) {
				maxUntil = p.winUntil
			}
			for _, cs := range collectEventCallsigns(p.ev) {
				low := strings.ToLower(strings.TrimSpace(cs))
				if low == "" {
					continue
				}
				if _, ok := seenCS[low]; !ok {
					seenCS[low] = struct{}{}
					unionCallsigns = append(unionCallsigns, cs)
				}
			}
			if eq, ok := collectEventWatchQuery(p.ev, now); ok {
				unionEventQueries = append(unionEventQueries, eq)
			}
		}

		cands, rev, err := m.guide.WatchCandidates(ctx, viewer, GuideWatchQuery{
			From:               minFrom,
			Until:              maxUntil,
			Callsigns:          unionCallsigns,
			EventQueries:       unionEventQueries,
			FavoriteChannelIDs: favIDs,
		})
		if err != nil {
			return nil, err
		}
		applyFavoriteChannels(cands, favSet)
		batchRev := guideRev
		if rev != "" {
			batchRev = rev
		}

		// Only reuse the shared slice when it did not approach the primary
		// programme cap (ensures huge EPGs never drop any event's window).
		progCount := 0
		for _, c := range cands {
			if !c.ChannelOnly {
				progCount++
			}
		}
		batchUsable := progCount < watchBatchPrimaryCapGuard

		for _, p := range bucket {
			var evCands []GuideCandidate
			evRev := batchRev
			if batchUsable {
				evCands = filterCandidatesForWindow(cands, p.winFrom, p.winUntil)
			} else {
				q := GuideWatchQuery{
					From:               p.winFrom,
					Until:              p.winUntil,
					Callsigns:          collectEventCallsigns(p.ev),
					FavoriteChannelIDs: favIDs,
				}
				if eq, ok := collectEventWatchQuery(p.ev, now); ok {
					q.EventQueries = []livetv.WatchEventQuery{eq}
				}
				var qerr error
				evCands, evRev, qerr = m.guide.WatchCandidates(ctx, viewer, q)
				if qerr != nil {
					return nil, qerr
				}
				applyFavoriteChannels(evCands, favSet)
			}
			ck := p.cacheKey
			if evRev != "" && evRev != guideRev {
				ck = watchCacheKey(p.ev, evRev, favRev, p.ecw)
			}
			targets := matchEventToCandidatesAt(p.ev, evCands, now)
			m.putWatchCached(ck, targets)
			out[p.ev.ID] = cloneWatchTargets(targets)
		}
	}
	return out, nil
}

func filterCandidatesForWindow(all []GuideCandidate, from, until time.Time) []GuideCandidate {
	if len(all) == 0 {
		return nil
	}
	out := make([]GuideCandidate, 0, len(all))
	for _, c := range all {
		if c.ChannelOnly {
			out = append(out, c)
			continue
		}
		if c.Start.IsZero() && c.End.IsZero() {
			// Fake/test candidates without timestamps pass through.
			out = append(out, c)
			continue
		}
		if c.Start.Before(until) && c.End.After(from) {
			out = append(out, c)
		}
	}
	return out
}

// LiveEventForChannel resolves the live event matching the programme currently
// airing on channelID for viewer (A6 reverse of A5), cached per programme.
// Returns (programmeKey, ref, err); ref is nil when no event matches.
func (m *WatchMatcher) LiveEventForChannel(ctx context.Context, viewer *core.User, channelID string) (string, *core.LiveEventRef, error) {
	if m == nil || m.guide == nil || m.activeEvents == nil || viewer == nil || channelID == "" {
		return "", nil, nil
	}
	now := m.now()
	cand, progKey, err := m.guide.CurrentProgramme(ctx, viewer, channelID, now)
	if err != nil || cand == nil || progKey == "" || cand.Unhealthy {
		return progKey, nil, err
	}

	events, eventsRev, err := m.activeEvents.ActiveEvents(ctx, viewer, now)
	if err != nil {
		return progKey, nil, err
	}
	if len(events) == 0 {
		return progKey, nil, nil
	}

	revKey := progKey + "|" + eventsRev
	var favKeys []string
	if cand.ChannelOnly && m.favoriteEntities != nil {
		keys, ferr := m.favoriteEntities(ctx, viewer)
		if ferr == nil && len(keys) > 0 {
			favKeys = slices.Clone(keys)
			slices.Sort(favKeys)
			h := sha256.New()
			for _, k := range favKeys {
				_, _ = io.WriteString(h, k)
				_, _ = io.WriteString(h, "\x00")
			}
			revKey += "|fav:" + hex.EncodeToString(h.Sum(nil)[:8])
		}
	}
	if ref, ok := m.getReverseCached(revKey); ok {
		return progKey, ref, nil
	}

	var ref *core.LiveEventRef
	if cand.ChannelOnly {
		ref = matchChannelOnlyToEvents(*cand, events, favKeys, now)
	} else {
		ref = matchProgrammeToEvents(*cand, events, now)
	}
	m.putReverseCached(revKey, ref)
	return progKey, cloneLiveEventRef(ref), nil
}

func (m *WatchMatcher) getWatchCached(key string, now time.Time) ([]core.WatchTarget, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.watchCache[key]
	if !ok || watchProgrammeFlagsChanged(v, now) {
		return nil, false
	}
	return cloneWatchTargets(v), true
}

func watchProgrammeFlagsChanged(targets []core.WatchTarget, now time.Time) bool {
	for _, target := range targets {
		if p := target.Programme; p != nil {
			catchUp, startOver := frontend.ArchiveFlags(p.ArchiveDays, p.Start, p.End, now)
			if p.CatchUp != catchUp || p.StartOver != startOver || !catchUp && !p.End.After(now) {
				return true
			}
		}
		if watchProgrammeFlagsChanged(target.Alternates, now) {
			return true
		}
	}
	return false
}

func (m *WatchMatcher) putWatchCached(key string, targets []core.WatchTarget) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.watchCache[key]; !exists {
		if len(m.watchOrder) >= m.watchCap {
			evict := m.watchOrder[0]
			m.watchOrder = m.watchOrder[1:]
			delete(m.watchCache, evict)
		}
		m.watchOrder = append(m.watchOrder, key)
	}
	m.watchCache[key] = cloneWatchTargets(targets)
}

func cloneWatchTargets(in []core.WatchTarget) []core.WatchTarget {
	if in == nil {
		return nil
	}
	out := make([]core.WatchTarget, len(in))
	for i, t := range in {
		out[i] = t.Clone()
	}
	return out
}

func (m *WatchMatcher) getReverseCached(key string) (*core.LiveEventRef, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.reverseCache[key]
	if !ok {
		return nil, false
	}
	return cloneLiveEventRef(v), true
}

func (m *WatchMatcher) putReverseCached(key string, ref *core.LiveEventRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.reverseCache[key]; !exists {
		if len(m.reverseOrder) >= m.reverseCap {
			evict := m.reverseOrder[0]
			m.reverseOrder = m.reverseOrder[1:]
			delete(m.reverseCache, evict)
		}
		m.reverseOrder = append(m.reverseOrder, key)
	}
	m.reverseCache[key] = cloneLiveEventRef(ref)
}

func cloneLiveEventRef(ref *core.LiveEventRef) *core.LiveEventRef {
	if ref == nil {
		return nil
	}
	cp := *ref
	return &cp
}

func parseEventTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	return time.Time{}
}

func collectEventCallsigns(ev core.LiveEvent) []string {
	var out []string
	for _, b := range ev.Broadcasts {
		if b.Kind != "radio" && b.Name != "" {
			out = append(out, b.Name)
		}
	}
	if ev.MatchHints != nil {
		out = append(out, ev.MatchHints.Callsigns...)
	}
	return out
}

// ftsSearchWords splits text on non-alphanumeric Unicode boundaries (matching
// SQLite FTS5 unicode61 and PostgreSQL simple tokenization).
func ftsSearchWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(foldText(s)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func isNoiseCompetitorSearchWord(tok string) bool {
	switch tok {
	case "the", "new", "los", "san", "st", "fc", "sc", "cf", "ac", "bc",
		"city", "club", "real", "de", "la", "el", "at", "vs", "v", "of", "and":
		return true
	default:
		return false
	}
}

// collectEventWatchQuery extracts bounded FTS5/GIN search terms for ev so
// migration 27's channel and programme indexes can locate matching IPTV event
// channels and EPG programmes without scanning the full lineup.
func collectEventWatchQuery(ev core.LiveEvent, now time.Time) (livetv.WatchEventQuery, bool) {
	switch ev.Status.State {
	case core.LiveEventStatePost, core.LiveEventStateCanceled, core.LiveEventStatePostponed:
		return livetv.WatchEventQuery{}, false
	}
	prof := buildEventMatchProfile(ev, now)
	var eq livetv.WatchEventQuery

	collectSide := func(c core.LiveEventCompetitor, ct competitorTerms) []string {
		var out []string
		add := func(tok string) {
			if len(tok) < 2 || len(out) >= 6 || slices.Contains(out, tok) {
				return
			}
			out = append(out, tok)
		}
		// Prefer Short, Abbr, and distinctive trailing word of Name first.
		for _, w := range ftsSearchWords(c.Short) {
			if !isNoiseCompetitorSearchWord(w) {
				add(w)
			}
		}
		for _, w := range ftsSearchWords(c.Abbr) {
			add(w)
		}
		if nameWords := ftsSearchWords(c.Name); len(nameWords) > 0 {
			last := nameWords[len(nameWords)-1]
			if !isNoiseCompetitorSearchWord(last) {
				add(last)
			}
			for _, w := range nameWords {
				if len(w) >= 4 && !isNoiseCompetitorSearchWord(w) {
					add(w)
				}
			}
		}
		for _, p := range ct.phrases {
			for _, tok := range p {
				for _, w := range ftsSearchWords(tok) {
					if len(w) >= 3 && !isNoiseCompetitorSearchWord(w) {
						add(w)
					}
				}
			}
		}
		// Fallback if all tokens were filtered as noise (e.g. "Real" alone).
		if len(out) == 0 {
			for _, w := range ftsSearchWords(c.Name) {
				add(w)
			}
		}
		return out
	}

	if len(ev.Competitors) == 2 && len(prof.competitors) == 2 {
		eq.SideA = collectSide(ev.Competitors[0], prof.competitors[0])
		eq.SideB = collectSide(ev.Competitors[1], prof.competitors[1])
	}

	addPhrase := func(p []string, maxTotal int) {
		var clean []string
		for _, tok := range p {
			clean = append(clean, ftsSearchWords(tok)...)
		}
		if len(clean) == 0 || (len(clean) == 1 && len(clean[0]) < 2) {
			return
		}
		if len(clean) > 4 {
			clean = clean[:4]
		}
		for _, existing := range eq.Phrases {
			if slices.Equal(existing, clean) {
				return
			}
		}
		if len(eq.Phrases) < maxTotal {
			eq.Phrases = append(eq.Phrases, clean)
		}
	}

	if prof.singleEvent || prof.hasCardTitle {
		// Prioritize short distinctive title phrases (e.g. "ufc 310", "las vegas gp"),
		// keeping dedicated headroom so league phrases (e.g. "f1", "ufc") are never
		// crowded out by multi-word sponsor/title permutations.
		sortedTitles := slices.Clone(prof.eventTitlePhrases)
		sort.SliceStable(sortedTitles, func(i, j int) bool {
			return len(sortedTitles[i]) < len(sortedTitles[j])
		})
		for _, p := range sortedTitles {
			if len(p) >= 2 && len(p) <= 4 {
				addPhrase(p, 4)
			}
		}
		if prof.eventChanWindow {
			for _, lp := range prof.leaguePhrases {
				addPhrase(lp, 8)
			}
		}
	}

	if len(eq.SideA) == 0 && len(eq.SideB) == 0 && len(eq.Phrases) == 0 {
		return livetv.WatchEventQuery{}, false
	}
	return eq, true
}

func watchCacheKey(ev core.LiveEvent, guideRev, favRev string, eventChanWindow bool) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "id=%s|rev=%s|fav=%s|ecw=%t|start=%s|state=%s|title=%s|cat=%s|lg=%s:%s",
		ev.ID, guideRev, favRev, eventChanWindow, ev.Start, ev.Status.State, ev.Title, ev.Category, ev.League.Name, ev.League.Abbr)
	for _, c := range ev.Competitors {
		_, _ = fmt.Fprintf(h, "|c=%s:%s:%s", c.Name, c.Short, c.Abbr)
	}
	for _, b := range ev.Broadcasts {
		_, _ = fmt.Fprintf(h, "|b=%s:%s:%s", b.Name, b.Kind, b.URL)
	}
	if ev.MatchHints != nil {
		for _, t := range ev.MatchHints.Terms {
			_, _ = fmt.Fprintf(h, "|t=%s", t)
		}
		for _, cs := range ev.MatchHints.Callsigns {
			_, _ = fmt.Fprintf(h, "|cs=%s", cs)
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
