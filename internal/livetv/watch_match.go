package livetv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/lru"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	// Time-leading index so watch-target matching (A5) probes only programmes
	// overlapping an event's window without scanning the whole guide.
	store.Register("livetv", 26, `CREATE INDEX livetv_programmes_time ON livetv_programmes(start_ms, end_ms);`)
}

// Bounds for indexed watch candidate queries. Every lookup stays bounded
// regardless of lineup or guide size.
const (
	watchPrimaryWindowLimit    = 2000
	watchCarryoverWindowLimit  = 500
	watchCarryoverLookback     = 6 * time.Hour
	watchTargetedChannelCap    = 128
	watchMaxCallsignQueries    = 64
	watchMaxEventQueries       = 64
	watchPerQueryLimit         = 48
	watchCallsignFetchLimit    = 256
	watchSearchTotalChannelCap = 512
	watchMaxPrefChannelIDs     = 64

	// watchConsecutiveOpenFailures is the consecutive failed-open threshold at
	// which a stream input is treated as failing for Sports watch targets even
	// after its short tune cooldown elapses, until an open succeeds or the
	// failure ages out of watchRecentFailureWindow.
	watchConsecutiveOpenFailures = 2
	watchRecentFailureWindow     = 30 * time.Minute
)

type watchChannelState int

const (
	watchChannelHealthy watchChannelState = iota
	watchChannelUnhealthy
	watchChannelPaused
)

// WatchEventQuery describes one event's channel/programme search terms for
// bounded indexed lookup via migration 27's search indexes.
type WatchEventQuery struct {
	// SideA and SideB hold alternative normalized tokens for the two
	// competitors of a head-to-head event (queried as (SideA) AND (SideB)).
	SideA []string
	SideB []string
	// Phrases holds multi-word or single-word event/league queries for
	// single-event or card sports (e.g. ["ufc", "310"], ["las", "vegas", "gp"], ["f1"]).
	Phrases [][]string
}

// WatchQuery specifies bounded inputs for WatchCandidatesQuery.
type WatchQuery struct {
	From               time.Time
	Until              time.Time
	Callsigns          []string
	EventQueries       []WatchEventQuery
	FavoriteChannelIDs []string
}

// WatchGuideEntry is one viewer-visible channel + overlapping guide programme
// (or a matched channel without guide data) returned by a bounded indexed
// lookup for A5/A6 live-event matching.
type WatchGuideEntry struct {
	ArchiveDays   int
	ChannelID     string
	ChannelName   string
	ChannelNumber string
	ChannelLogo   string
	TvgName       string
	GuideID       string
	Group         string
	IsEventAddon  bool
	ChannelOnly   bool
	Favorite      bool
	WatchedAt     time.Time
	Unhealthy     bool
	Programme     Programme
	Live          bool
}

func (s *Service) bumpHealthRevision() {
	if s == nil {
		return
	}
	s.healthRev.Add(1)
}

func (s *Service) noteHealthExpiry(at time.Time) {
	if s == nil || at.IsZero() {
		return
	}
	target := at.UnixNano()
	if target <= 0 {
		return
	}
	for {
		cur := s.nextHealthExpiry.Load()
		if cur > 0 && cur <= target {
			return
		}
		if s.nextHealthExpiry.CompareAndSwap(cur, target) {
			return
		}
	}
}

func (s *Service) healthRevision() uint64 {
	if s == nil {
		return 0
	}
	nowNano := s.o.Now().UnixNano()
	if exp := s.nextHealthExpiry.Load(); exp > 0 && nowNano >= exp {
		if s.nextHealthExpiry.CompareAndSwap(exp, 0) {
			s.healthRev.Add(1)
		}
	}
	return s.healthRev.Load()
}

func sourceHistoryAvailabilityChanged(a, b SourceHistory) bool {
	return a.Failures != b.Failures ||
		!a.RetryAfter.Equal(b.RetryAfter) ||
		a.LastError != b.LastError ||
		!a.LastErrorAt.Equal(b.LastErrorAt) ||
		!a.LastPlayed.Equal(b.LastPlayed)
}

func (s *Service) noteSourceHistoryExpiry(h SourceHistory, now time.Time) {
	if s == nil {
		return
	}
	if now.IsZero() {
		now = s.o.Now()
	}
	var expiry time.Time
	if h.RetryAfter.After(now) {
		expiry = h.RetryAfter
	}
	if h.Failures >= watchConsecutiveOpenFailures {
		lastErr := h.LastErrorAt
		if lastErr.IsZero() {
			lastErr = h.Updated
		}
		if !lastErr.IsZero() && !h.LastPlayed.After(lastErr) {
			if winEnd := lastErr.Add(watchRecentFailureWindow); winEnd.After(now) && winEnd.After(expiry) {
				expiry = winEnd
			}
		}
	}
	if !expiry.IsZero() {
		s.noteHealthExpiry(expiry)
	}
}

func sameAccountHealth(a, b *ProviderAccount) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if !strings.EqualFold(a.Status, b.Status) {
		return false
	}
	if a.ExpiresAt == nil && b.ExpiresAt == nil {
		return true
	}
	if a.ExpiresAt == nil || b.ExpiresAt == nil {
		return false
	}
	return a.ExpiresAt.Equal(*b.ExpiresAt)
}

func accountUsableAt(a *ProviderAccount, now time.Time) bool {
	if a == nil {
		return true
	}
	if a.Status != "" && !strings.EqualFold(a.Status, "active") {
		return false
	}
	if a.ExpiresAt != nil && !now.Before(*a.ExpiresAt) {
		return false
	}
	return true
}

func (s *Service) sourceAccountHealthy(sourceID string, entry authEntry, now time.Time) bool {
	if entry.pool != nil {
		src := entry.pool
		candidates := append([]ProviderLogin{{ID: "primary", Account: src.Account}}, src.Logins...)
		p := &s.loginPool
		p.mu.Lock()
		var earliestRetry time.Time
		usable := false
		for _, cand := range candidates {
			if cand.Error != "" {
				continue
			}
			if until := p.failed[sourceID+"\x00"+cand.ID]; now.Before(until) {
				if earliestRetry.IsZero() || until.Before(earliestRetry) {
					earliestRetry = until
				}
				continue
			}
			if !accountUsableAt(cand.Account, now) {
				continue
			}
			usable = true
			break
		}
		p.mu.Unlock()
		if !usable {
			if !earliestRetry.IsZero() {
				s.noteHealthExpiry(earliestRetry)
			}
			return false
		}
		return true
	}
	if entry.account != nil {
		if !accountUsableAt(entry.account, now) {
			return false
		}
		if entry.account.ExpiresAt != nil && now.Before(*entry.account.ExpiresAt) {
			s.noteHealthExpiry(*entry.account.ExpiresAt)
		}
	}
	return true
}

func (s *Service) sourceInputHealthy(channelID string, src StreamSource, now time.Time) bool {
	st := s.sourceStatus(channelID, src)
	hist, hasHist := s.sourceHistory(channelID, src)

	var (
		failures   int
		retryAfter time.Time
		lastErrAt  time.Time
		lastPlayed time.Time
	)
	switch {
	case hasHist && hist.Updated.After(st.LastChecked):
		failures = hist.Failures
		retryAfter = hist.RetryAfter
		lastErrAt = hist.LastErrorAt
		if lastErrAt.IsZero() && failures > 0 {
			lastErrAt = hist.Updated
		}
		lastPlayed = hist.LastPlayed
	case !st.LastChecked.IsZero():
		failures = st.Failures
		retryAfter = st.RetryAfter
		if st.LastError != "" || failures > 0 {
			lastErrAt = st.LastChecked
			if hasHist && !hist.LastErrorAt.IsZero() && hist.LastErrorAt.After(lastErrAt) {
				lastErrAt = hist.LastErrorAt
			}
		}
		lastPlayed = st.LastPlayed
	case hasHist:
		failures = hist.Failures
		retryAfter = hist.RetryAfter
		lastErrAt = hist.LastErrorAt
		if lastErrAt.IsZero() && failures > 0 {
			lastErrAt = hist.Updated
		}
		lastPlayed = hist.LastPlayed
	default:
		return true
	}

	var expiry time.Time
	unhealthy := false
	if retryAfter.After(now) {
		unhealthy = true
		expiry = retryAfter
	}
	if failures >= watchConsecutiveOpenFailures && !lastErrAt.IsZero() && !lastPlayed.After(lastErrAt) {
		winEnd := lastErrAt.Add(watchRecentFailureWindow)
		if winEnd.After(now) {
			unhealthy = true
			if winEnd.After(expiry) {
				expiry = winEnd
			}
		}
	}
	if unhealthy {
		if !expiry.IsZero() {
			s.noteHealthExpiry(expiry)
		}
		return false
	}
	return true
}

func (s *Service) channelWatchHealth(ctx context.Context, c Channel, now time.Time) watchChannelState {
	if s.sourcePaused(ctx, c) {
		return watchChannelPaused
	}
	if c.Kind != "iptv" {
		return watchChannelHealthy
	}

	var single [1]StreamSource
	inputs := c.StreamSources
	if len(inputs) == 0 {
		rawURL := strings.TrimSpace(c.StreamURL)
		if !validStreamURL(rawURL) {
			return watchChannelUnhealthy
		}
		id := stableID(c.ID, "primary")
		if c.SourceID != "" {
			id = stableID(c.ID, "playlist")
		}
		single[0] = StreamSource{ID: id, URL: rawURL, Enabled: true}
		inputs = single[:]
	}

	activeCount := 0
	unpausedCount := 0
	healthyCount := 0
	sweptUnavailable := s.sweepState(c) == "unavailable"

	for _, src := range inputs {
		if !src.Enabled || !s.autoDonorActive(src.AutoFrom) {
			continue
		}
		rawURL := strings.TrimSpace(src.URL)
		if !validStreamURL(rawURL) {
			continue
		}
		activeCount++
		effSourceID := src.AuthSourceID
		if effSourceID == "" {
			effSourceID = c.SourceID
		}
		var entry authEntry
		if effSourceID != "" {
			var err error
			entry, err = s.sourceEntry(ctx, effSourceID)
			if errors.Is(err, core.ErrNotAvailable) {
				continue // this input's source is paused
			}
		}
		unpausedCount++
		if sweptUnavailable {
			continue // keep scanning only to see whether all active inputs are paused
		}
		if effSourceID != "" && !s.sourceAccountHealthy(effSourceID, entry, now) {
			continue
		}
		checkSrc := src
		checkSrc.URL = rawURL
		if checkSrc.ID == "" {
			checkSrc.ID = stableID(c.ID, rawURL)
		}
		if s.sourceInputHealthy(c.ID, checkSrc, now) {
			healthyCount++
		}
	}

	if activeCount > 0 && unpausedCount == 0 {
		return watchChannelPaused
	}
	if sweptUnavailable || activeCount == 0 || healthyCount == 0 {
		return watchChannelUnhealthy
	}
	return watchChannelHealthy
}

// GuideRevision returns a compact, cluster-safe revision token covering the
// guide/channel epoch, channel health revision, search index readiness, viewer
// recent-watch state, and the viewer's effective Live TV access policy.
func (s *Service) GuideRevision(ctx context.Context, u *core.User) (string, error) {
	rev, _, err := s.watchGuideRevision(ctx, u)
	return rev, err
}

func (s *Service) watchGuideRevision(ctx context.Context, u *core.User) (string, int64, error) {
	if s == nil || s.o.DB == nil {
		return "none", 0, nil
	}
	if u == nil {
		return "denied", 0, nil
	}
	var gen int64
	var rawPol string
	err := s.o.DB.QueryRowContext(ctx,
		`SELECT generation, COALESCE((SELECT data FROM livetv_policies WHERE user_id=?),'') FROM livetv_channel_epoch WHERE id=1`,
		u.ID,
	).Scan(&gen, &rawPol)
	if err != nil {
		return "", 0, err
	}
	healthRev := s.healthRevision()
	sr := 0
	if s.isSearchReady() {
		sr = 1
	}
	rw := s.viewerRecentWatchedGen(u)
	sum := sha256.New()
	_, _ = fmt.Fprintf(sum, "g=%d|h=%d|u=%d|owner=%t|rat=%s|libs=%v|pol=%s|sr=%d|rw=%d|app=%s",
		gen, healthRev, u.ID, u.IsOwner, u.MaxContentRating, u.AllowedLibraries, rawPol, sr, rw, core.LiveApp(ctx))
	return fmt.Sprintf("g%d:h%d:%s", gen, healthRev, hex.EncodeToString(sum.Sum(nil)[:8])), gen, nil
}

// WatchCandidates returns a bounded set of viewer-visible channel+programme
// pairs whose airing window overlaps [from, until), along with the current
// viewer guide revision for caching.
func (s *Service) WatchCandidates(ctx context.Context, u *core.User, from, until time.Time, callsigns []string) ([]WatchGuideEntry, string, error) {
	return s.WatchCandidatesQuery(ctx, u, WatchQuery{
		From:      from,
		Until:     until,
		Callsigns: callsigns,
	})
}

// WatchCandidatesQuery returns a bounded set of viewer-visible channel and
// overlapping programme candidates using migration 27's channel search index,
// programme FTS/GIN index, livetv_search_channel_guide, and
// livetv_programmes_time without scanning the full channel lineup.
func (s *Service) WatchCandidatesQuery(ctx context.Context, u *core.User, q WatchQuery) ([]WatchGuideEntry, string, error) {
	if s == nil || s.o.DB == nil {
		return nil, "none", nil
	}
	if u == nil {
		return nil, "denied", nil
	}
	if !q.Until.After(q.From) {
		rev, err := s.GuideRevision(ctx, u)
		return nil, rev, err
	}

	searchReady, err := s.prepareWatchSearchIndex(ctx)
	if err != nil {
		return nil, "", err
	}

	rev, gen, err := s.watchGuideRevision(ctx, u)
	if err != nil {
		return nil, "", err
	}

	ctx, pol, err := s.withPolicy(ctx, u)
	if err != nil {
		return nil, "", err
	}
	if !u.IsOwner && pol.Disabled {
		return nil, rev, nil
	}

	pg := store.DialectOf(s.o.DB) == store.Postgres
	favSet := make(map[string]bool, len(pol.Favorites)+len(q.FavoriteChannelIDs))
	for _, id := range pol.Favorites {
		if id != "" {
			favSet[id] = true
		}
	}
	for _, id := range q.FavoriteChannelIDs {
		if id != "" {
			favSet[id] = true
		}
	}
	recentWatched, _ := s.viewerRecentWatched(u)

	// Step 1: Indexed channel lookup via migration 27's channel search indexes
	// (no full channel snapshot scan).
	targetedIDs := make([]string, 0, 64)
	seenTargeted := make(map[string]bool, 64)
	addTargetedID := func(id string) bool {
		if id == "" || seenTargeted[id] {
			return false
		}
		if len(targetedIDs) >= watchSearchTotalChannelCap {
			return false
		}
		seenTargeted[id] = true
		targetedIDs = append(targetedIDs, id)
		return true
	}

	// Always include the viewer's favourited and recently watched channels so
	// noisy networks with many local feeds never crowd them out of FTS limits.
	prefCount := 0
	for id := range favSet {
		if prefCount >= watchMaxPrefChannelIDs {
			break
		}
		if addTargetedID(id) {
			prefCount++
		}
	}
	for id := range recentWatched {
		if prefCount >= watchMaxPrefChannelIDs {
			break
		}
		if addTargetedID(id) {
			prefCount++
		}
	}

	searchCtx := ctx
	if searchReady {
		searchCtx = context.WithValue(ctx, watchSearchEpochKey{}, gen)
	}

	// 1a. Broadcaster callsign indexed lookup.
	callsignPhrases := expandWatchCallsignQueries(q.Callsigns)
	if len(callsignPhrases) > watchMaxCallsignQueries {
		callsignPhrases = callsignPhrases[:watchMaxCallsignQueries]
	}
	callsignPerQueryLimit := watchPerQueryLimit
	if len(callsignPhrases) <= 4 {
		callsignPerQueryLimit = watchCallsignFetchLimit
	}
	for _, phrase := range callsignPhrases {
		if len(targetedIDs) >= watchSearchTotalChannelCap {
			break
		}
		ids, err := s.searchWatchChannelsByPhrase(searchCtx, pg, searchReady, phrase, callsignPerQueryLimit)
		if err != nil {
			return nil, "", err
		}
		for _, id := range ids {
			addTargetedID(id)
		}
	}

	// 1b. Event-channel indexed lookup (per-game / single-event channel names).
	eventQueries := q.EventQueries
	if len(eventQueries) > watchMaxEventQueries {
		eventQueries = eventQueries[:watchMaxEventQueries]
	}
	for _, eq := range eventQueries {
		if len(targetedIDs) >= watchSearchTotalChannelCap {
			break
		}
		if len(eq.SideA) > 0 && len(eq.SideB) > 0 {
			ids, err := s.searchWatchChannelsHeadToHead(searchCtx, pg, searchReady, eq.SideA, eq.SideB, watchPerQueryLimit)
			if err != nil {
				return nil, "", err
			}
			for _, id := range ids {
				addTargetedID(id)
			}
		}
		for _, phrase := range eq.Phrases {
			if len(targetedIDs) >= watchSearchTotalChannelCap {
				break
			}
			ids, err := s.searchWatchChannelsByPhrase(searchCtx, pg, searchReady, phrase, watchPerQueryLimit)
			if err != nil {
				return nil, "", err
			}
			for _, id := range ids {
				addTargetedID(id)
			}
		}
	}

	// 1c. Stremio event channels (guideId = "event:...").
	if len(targetedIDs) < watchSearchTotalChannelCap {
		ids, err := s.searchWatchChannelsByPhrase(searchCtx, pg, searchReady, []string{"event"}, watchPerQueryLimit)
		if err != nil {
			return nil, "", err
		}
		for _, id := range ids {
			addTargetedID(id)
		}
	}

	// Hydrate targeted channel records via bounded PK lookup + visibility, policy, and health.
	now := s.o.Now()
	app := core.LiveApp(ctx)
	allowedChannels := make(map[string]Channel)
	unhealthyByID := make(map[string]bool)
	hydratedChecked := make(map[string]bool)
	hydrateAllowed := func(ids []string) error {
		var missing []string
		for _, id := range ids {
			if id == "" || hydratedChecked[id] {
				continue
			}
			missing = append(missing, id)
		}
		if len(missing) == 0 {
			return nil
		}
		missing = uniqueIDs(missing)
		for _, id := range missing {
			hydratedChecked[id] = true
		}
		visible, err := s.searchVisibleGroupIDs(ctx, missing)
		if err != nil {
			return err
		}
		records, err := s.channelsByIDs(ctx, missing)
		if err != nil {
			return err
		}
		for _, id := range missing {
			if !visible[id] {
				continue
			}
			c, ok := records[id]
			if !ok || c.Kind == "virtual" {
				continue
			}
			if app != "" && slices.Contains(c.HiddenApps, app) {
				continue
			}
			if !s.audiencePolicyAllowed(ctx, u, c) {
				continue
			}
			health := s.channelWatchHealth(ctx, c, now)
			if health == watchChannelPaused {
				continue
			}
			if health == watchChannelUnhealthy {
				unhealthyByID[id] = true
			}
			allowedChannels[id] = c
		}
		return nil
	}
	if err := hydrateAllowed(targetedIDs); err != nil {
		return nil, "", err
	}

	// Prioritize healthy targeted channels over unhealthy ones so >watchTargetedChannelCap
	// failing feeds cannot crowd out healthy channels.
	var healthyTargeted, unhealthyTargeted []Channel
	for _, id := range targetedIDs {
		c, ok := allowedChannels[id]
		if !ok {
			continue
		}
		if unhealthyByID[id] {
			unhealthyTargeted = append(unhealthyTargeted, c)
		} else {
			healthyTargeted = append(healthyTargeted, c)
		}
	}
	targetedCap := watchTargetedChannelCap
	if len(q.EventQueries) > 0 || len(q.Callsigns) > 4 {
		targetedCap = watchSearchTotalChannelCap
	}
	targetedAllowed := make([]Channel, 0, min(len(healthyTargeted)+len(unhealthyTargeted), targetedCap))
	if len(healthyTargeted) > targetedCap {
		targetedAllowed = append(targetedAllowed, healthyTargeted[:targetedCap]...)
	} else {
		targetedAllowed = append(targetedAllowed, healthyTargeted...)
		if rem := targetedCap - len(targetedAllowed); rem > 0 && len(unhealthyTargeted) > 0 {
			targetedAllowed = append(targetedAllowed, unhealthyTargeted[:min(rem, len(unhealthyTargeted))]...)
		}
	}
	targetedGuide := make(map[guideKey][]Channel)
	for _, c := range targetedAllowed {
		if c.GuideID != "" {
			gk := guideKey{source: guideSource(c), guide: c.GuideID}
			targetedGuide[gk] = append(targetedGuide[gk], c)
		}
	}

	type chanProgKey struct {
		channelID string
		startMS   int64
	}
	seenProg := make(map[chanProgKey]struct{})
	channelsWithProg := make(map[string]bool)
	var out []WatchGuideEntry
	policyCache := newSearchPolicyCache(s)

	appendChannelProgramme := func(c Channel, p Programme) {
		dk := chanProgKey{channelID: c.ID, startMS: p.Start.UnixMilli()}
		if _, dup := seenProg[dk]; dup {
			return
		}
		seenProg[dk] = struct{}{}
		// Record that this channel has real guide data in [from, until) even
		// if policy hides the programme, so broadcaster-only fallback never
		// overrides known EPG schedules or policy-blocked programmes.
		channelsWithProg[c.ID] = true
		cp := p
		cp.ChannelID = c.ID
		if cp.ID == "" {
			cp.ID = fmt.Sprintf("%s-%d", c.ID, cp.Start.UnixMilli())
		}
		if !s.allowedWithLookupOpts(ctx, u, c, &cp, policyCache.lookup(c), false, true) {
			return
		}
		out = append(out, WatchGuideEntry{
			ChannelID:     c.ID,
			ChannelName:   c.Name,
			ChannelNumber: c.Number,
			ChannelLogo:   c.Logo,
			TvgName:       c.TvgName,
			GuideID:       c.GuideID,
			Group:         c.Group,
			IsEventAddon:  len(cp.EventStreams) > 0 || isStremioEventChannel(c),
			Favorite:      c.Favorite || favSet[c.ID],
			WatchedAt:     recentWatched[c.ID],
			Unhealthy:     unhealthyByID[c.ID],
			Programme:     cp,
			ArchiveDays:   c.ArchiveDays(),
			Live:          !now.Before(cp.Start) && now.Before(cp.End),
		})
	}

	// Step 2a: Read the guide only for targeted channels over the event window.
	targetedProgs, err := s.programmesForKeys(ctx, targetedGuide, q.From, q.Until)
	if err != nil {
		return nil, "", err
	}

	// Step 2b: Indexed programme search for event queries in [from, until)
	// (finds EPG matches even on very large guides whose time slice exceeds cap).
	type rawChanProg struct {
		channelID string
		prog      Programme
	}
	var discoveredProgs []rawChanProg
	var discoveredChanIDs []string
	collectJoinedProg := func(id string, raw []byte) {
		var p Programme
		if err := json.Unmarshal(raw, &p); err != nil {
			return
		}
		discoveredProgs = append(discoveredProgs, rawChanProg{channelID: id, prog: p})
		if _, known := allowedChannels[id]; !known {
			discoveredChanIDs = append(discoveredChanIDs, id)
		}
	}

	if searchReady {
		fromMS := q.From.UnixMilli()
		untilMS := q.Until.UnixMilli()
		for _, eq := range eventQueries {
			if len(eq.SideA) > 0 && len(eq.SideB) > 0 {
				if err := s.searchWatchProgrammesHeadToHead(ctx, pg, eq.SideA, eq.SideB, fromMS, untilMS, watchPerQueryLimit, collectJoinedProg); err != nil {
					return nil, "", err
				}
			}
			for _, phrase := range eq.Phrases {
				if len(phrase) < 2 {
					continue
				}
				if err := s.searchWatchProgrammesByPhrase(ctx, pg, phrase, fromMS, untilMS, watchPerQueryLimit, collectJoinedProg); err != nil {
					return nil, "", err
				}
			}
		}
	}

	// An unqualified window has no search terms to narrow it. While indexes
	// are preparing, the time index also covers programmes yet to be indexed.
	if !searchReady || len(eventQueries) == 0 && len(q.Callsigns) == 0 {
		fromMS := q.From.UnixMilli()
		untilMS := q.Until.UnixMilli()
		if err := s.scanWatchTimeSliceJoined(ctx, pg, fromMS, untilMS, fromMS, true, watchPrimaryWindowLimit, collectJoinedProg); err != nil {
			return nil, "", err
		}
		carryFromMS := q.From.Add(-watchCarryoverLookback).UnixMilli()
		if err := s.scanWatchTimeSliceJoined(ctx, pg, carryFromMS, fromMS, fromMS, false, watchCarryoverWindowLimit, collectJoinedProg); err != nil {
			return nil, "", err
		}
	}

	if len(discoveredChanIDs) > 0 {
		if err := hydrateAllowed(discoveredChanIDs); err != nil {
			return nil, "", err
		}
	}
	// Candidate programme links need local visibility facts only. Load them
	// once by key before enforcing policy, including candidates found via FTS.
	var keys []core.RatingKey
	for _, ps := range targetedProgs {
		for _, p := range ps {
			if p.Item.Key != 0 {
				keys = append(keys, p.Item.Key)
			}
		}
	}
	for _, dp := range discoveredProgs {
		if _, ok := allowedChannels[dp.channelID]; ok && dp.prog.Item.Key != 0 {
			keys = append(keys, dp.prog.Item.Key)
		}
	}
	if err := policyCache.preload(ctx, keys); err != nil {
		return nil, "", err
	}
	for gk, ps := range targetedProgs {
		for _, p := range ps {
			for _, c := range targetedGuide[gk] {
				appendChannelProgramme(c, p)
			}
		}
	}
	for _, dp := range discoveredProgs {
		if c, ok := allowedChannels[dp.channelID]; ok {
			appendChannelProgramme(c, dp.prog)
		}
	}

	// Step 3: Guide-free entries for targeted channels without overlapping EPG
	// (covers both IPTV event channels and broadcaster-name fallback).
	for _, c := range targetedAllowed {
		if channelsWithProg[c.ID] {
			continue
		}
		out = append(out, WatchGuideEntry{
			ChannelID:     c.ID,
			ChannelName:   c.Name,
			ChannelNumber: c.Number,
			ChannelLogo:   c.Logo,
			TvgName:       c.TvgName,
			GuideID:       c.GuideID,
			Group:         c.Group,
			IsEventAddon:  isStremioEventChannel(c),
			ChannelOnly:   true,
			Favorite:      c.Favorite || favSet[c.ID],
			WatchedAt:     recentWatched[c.ID],
			Unhealthy:     unhealthyByID[c.ID],
		})
	}

	return out, rev, nil
}

func (s *Service) scanWatchTimeSliceJoined(
	ctx context.Context,
	pg bool,
	startMinMS, startMaxMS, endMinMS int64,
	asc bool,
	limit int,
	emit func(channelID string, raw []byte),
) error {
	order := "ASC"
	if !asc {
		order = "DESC"
	}
	query := `SELECT c.id, p.data
FROM (
	SELECT source_id, channel_id, data
	FROM livetv_programmes
	WHERE start_ms>=? AND start_ms<? AND end_ms>?
	ORDER BY start_ms ` + order + `
	LIMIT ?
) p
JOIN livetv_channels c
	ON coalesce(nullif(json_extract(c.data,'$.guideSourceId'),''),json_extract(c.data,'$.sourceId'))=+p.source_id
	AND json_extract(c.data,'$.guideId')=+p.channel_id`
	if pg {
		query = `SELECT c.id, p.data
FROM (
	SELECT source_id, channel_id, data
	FROM livetv_programmes
	WHERE start_ms>=? AND start_ms<? AND end_ms>?
	ORDER BY start_ms ` + order + `
	LIMIT ?
) p
JOIN livetv_channels c
	ON coalesce(nullif(c.data::jsonb->>'guideSourceId',''),c.data::jsonb->>'sourceId')=p.source_id
	AND (c.data::jsonb->>'guideId')=p.channel_id`
	}
	rows, err := s.o.DB.QueryContext(ctx, query, startMinMS, startMaxMS, endMinMS, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		emit(id, raw)
	}
	return rows.Err()
}

type watchSearchEpochKey struct{}

type watchChannelSearchKey struct {
	epoch int64
	query string
	terms string
	limit int
}

type watchChannelSearchCache struct {
	mu      sync.Mutex
	results *lru.Cache[watchChannelSearchKey, []string]
}

// Cache only indexed channel IDs. Viewer policy, health and programme
// visibility are evaluated separately on each candidate lookup.
func (s *Service) watchChannelIDs(ctx context.Context, query, terms string, limit int) ([]string, error) {
	epoch, cacheable := ctx.Value(watchSearchEpochKey{}).(int64)
	if !cacheable {
		return s.collectWatchChannelIDs(ctx, query, terms, limit)
	}
	key := watchChannelSearchKey{epoch: epoch, query: query, terms: terms, limit: limit}
	s.watchSearch.mu.Lock()
	ids, ok := s.watchSearch.results.Get(key)
	s.watchSearch.mu.Unlock()
	if ok {
		return ids, nil
	}
	ids, err := s.collectWatchChannelIDs(ctx, query, terms, limit)
	if err != nil {
		return nil, err
	}
	s.watchSearch.mu.Lock()
	s.watchSearch.results.Add(key, ids)
	s.watchSearch.mu.Unlock()
	return ids, nil
}

func (s *Service) collectWatchChannelIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.o.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Service) searchWatchChannelsByPhrase(ctx context.Context, pg, searchReady bool, phrase []string, limit int) ([]string, error) {
	words := sanitizeWatchTokens(phrase)
	if len(words) == 0 {
		return nil, nil
	}
	var out []string
	seen := make(map[string]bool)
	appendIDs := func(ids []string) {
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}

	if pg {
		expr := strings.Join(words, " & ")
		ids, err := s.watchChannelIDs(ctx,
			`SELECT id FROM livetv_channels WHERE to_tsvector('simple', livetv_channel_search_text(data::jsonb->>'name', data::jsonb->>'number', data::jsonb->>'guideId')) @@ to_tsquery('simple',?) ORDER BY id LIMIT ?`,
			expr, limit,
		)
		if err != nil {
			return nil, err
		}
		appendIDs(ids)
		return out, nil
	}

	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = `"` + w + `"`
	}
	ftsExpr := strings.Join(quoted, " AND ")
	ids, err := s.watchChannelIDs(ctx,
		`SELECT id FROM livetv_search_channels WHERE livetv_search_channels MATCH ? ORDER BY rank LIMIT ?`,
		ftsExpr, limit,
	)
	if err != nil {
		return nil, err
	}
	appendIDs(ids)

	// Probe migration 27's compact trigram index for glued/superscript/digit names
	// (e.g. "US| FOXᴴᴰ", "US| ESPNᴴᴰ", "ESPN2", "TSN1", "FOXSports1", "beINSports1").
	compact := strings.Join(words, "")
	compactLen := utf8.RuneCountInString(compact)
	if compactLen >= 3 && (len(out) < limit || len(words) <= 3) {
		compactLimit := limit
		if len(out) >= limit && compactLimit > 32 {
			compactLimit = 32
		}
		cids, err := s.watchChannelIDs(ctx,
			`SELECT id FROM livetv_search_channels_compact WHERE livetv_search_channels_compact MATCH ? ORDER BY rank LIMIT ?`,
			`"`+compact+`"`, compactLimit,
		)
		if err != nil {
			return nil, err
		}
		appendIDs(cids)
	}

	// Indexed prefix fallbacks while SQLite backfill is still running on a cold DB.
	if !searchReady && len(out) < limit {
		if compact != "" {
			cids, err := s.collectWatchChannelIDs(ctx,
				`SELECT id FROM livetv_channels WHERE `+sqliteCompactNameExpr+`>=? AND `+sqliteCompactNameExpr+`<? LIMIT ?`,
				compact, compact+"\uffff", limit,
			)
			if err != nil {
				return nil, err
			}
			appendIDs(cids)
		}
		prefix := words[0]
		nids, err := s.collectWatchChannelIDs(ctx,
			`SELECT id FROM livetv_channels WHERE lower(coalesce(json_extract(data,'$.name'),''))>=? AND lower(coalesce(json_extract(data,'$.name'),''))<? LIMIT ?`,
			prefix, prefix+"\uffff", limit,
		)
		if err != nil {
			return nil, err
		}
		appendIDs(nids)
	}
	return out, nil
}

func (s *Service) searchWatchChannelsHeadToHead(ctx context.Context, pg, searchReady bool, sideA, sideB []string, limit int) ([]string, error) {
	a := sanitizeWatchTokens(sideA)
	b := sanitizeWatchTokens(sideB)
	if len(a) == 0 || len(b) == 0 {
		return nil, nil
	}
	if len(a) > 6 {
		a = a[:6]
	}
	if len(b) > 6 {
		b = b[:6]
	}
	if pg {
		expr := "(" + strings.Join(a, " | ") + ") & (" + strings.Join(b, " | ") + ")"
		return s.watchChannelIDs(ctx,
			`SELECT id FROM livetv_channels WHERE to_tsvector('simple', livetv_channel_search_text(data::jsonb->>'name', data::jsonb->>'number', data::jsonb->>'guideId')) @@ to_tsquery('simple',?) ORDER BY id LIMIT ?`,
			expr, limit,
		)
	}
	qa := make([]string, len(a))
	for i, w := range a {
		qa[i] = `"` + w + `"`
	}
	qb := make([]string, len(b))
	for i, w := range b {
		qb[i] = `"` + w + `"`
	}
	expr := "(" + strings.Join(qa, " OR ") + ") AND (" + strings.Join(qb, " OR ") + ")"
	ids, err := s.watchChannelIDs(ctx,
		`SELECT id FROM livetv_search_channels WHERE livetv_search_channels MATCH ? ORDER BY rank LIMIT ?`,
		expr, limit,
	)
	if err != nil || searchReady || len(ids) > 0 {
		return ids, err
	}
	// Cold-start fallback: bounded prefix lookup on first token of sideA.
	prefix := a[0]
	return s.collectWatchChannelIDs(ctx,
		`SELECT id FROM livetv_channels WHERE lower(coalesce(json_extract(data,'$.name'),''))>=? AND lower(coalesce(json_extract(data,'$.name'),''))<? LIMIT ?`,
		prefix, prefix+"\uffff", limit,
	)
}

func (s *Service) searchWatchProgrammesHeadToHead(ctx context.Context, pg bool, sideA, sideB []string, fromMS, untilMS int64, limit int, emit func(string, []byte)) error {
	a := sanitizeWatchTokens(sideA)
	b := sanitizeWatchTokens(sideB)
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	if len(a) > 6 {
		a = a[:6]
	}
	if len(b) > 6 {
		b = b[:6]
	}
	var expr, query string
	if pg {
		expr = "(" + strings.Join(a, " | ") + ") & (" + strings.Join(b, " | ") + ")"
		query = `SELECT c.id, p.data FROM livetv_programmes p JOIN livetv_channels c ON coalesce(nullif(c.data::jsonb->>'guideSourceId',''),c.data::jsonb->>'sourceId')=p.source_id AND (c.data::jsonb->>'guideId')=p.channel_id WHERE to_tsvector('simple',coalesce(p.data::jsonb->'item'->>'title','') || ' ' || coalesce(p.data::jsonb->'item'->>'episodeTitle','') || ' ' || coalesce(p.data::jsonb->'item'->>'summary','')) @@ to_tsquery('simple',?) AND p.end_ms>? AND p.start_ms<? ORDER BY p.start_ms, c.id LIMIT ?`
	} else {
		qa := make([]string, len(a))
		for i, w := range a {
			qa[i] = `"` + w + `"`
		}
		qb := make([]string, len(b))
		for i, w := range b {
			qb[i] = `"` + w + `"`
		}
		expr = "(" + strings.Join(qa, " OR ") + ") AND (" + strings.Join(qb, " OR ") + ")"
		query = `SELECT c.id, p.data FROM livetv_search_programmes f JOIN livetv_programmes p ON p.source_id=f.source AND p.channel_id=f.channel AND p.start_ms=f.start JOIN livetv_channels c ON coalesce(nullif(json_extract(c.data,'$.guideSourceId'),''),json_extract(c.data,'$.sourceId'))=+p.source_id AND json_extract(c.data,'$.guideId')=+p.channel_id WHERE livetv_search_programmes MATCH ? AND p.end_ms>? AND p.start_ms<? ORDER BY p.start_ms, c.id LIMIT ?`
	}
	rows, err := s.o.DB.QueryContext(ctx, query, expr, fromMS, untilMS, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		emit(id, raw)
	}
	return rows.Err()
}

func (s *Service) searchWatchProgrammesByPhrase(ctx context.Context, pg bool, phrase []string, fromMS, untilMS int64, limit int, emit func(string, []byte)) error {
	words := sanitizeWatchTokens(phrase)
	if len(words) == 0 {
		return nil
	}
	var expr, query string
	if pg {
		expr = strings.Join(words, " & ")
		query = `SELECT c.id, p.data FROM livetv_programmes p JOIN livetv_channels c ON coalesce(nullif(c.data::jsonb->>'guideSourceId',''),c.data::jsonb->>'sourceId')=p.source_id AND (c.data::jsonb->>'guideId')=p.channel_id WHERE to_tsvector('simple',coalesce(p.data::jsonb->'item'->>'title','') || ' ' || coalesce(p.data::jsonb->'item'->>'episodeTitle','') || ' ' || coalesce(p.data::jsonb->'item'->>'summary','')) @@ to_tsquery('simple',?) AND p.end_ms>? AND p.start_ms<? ORDER BY p.start_ms, c.id LIMIT ?`
	} else {
		q := make([]string, len(words))
		for i, w := range words {
			q[i] = `"` + w + `"`
		}
		expr = strings.Join(q, " AND ")
		query = `SELECT c.id, p.data FROM livetv_search_programmes f JOIN livetv_programmes p ON p.source_id=f.source AND p.channel_id=f.channel AND p.start_ms=f.start JOIN livetv_channels c ON coalesce(nullif(json_extract(c.data,'$.guideSourceId'),''),json_extract(c.data,'$.sourceId'))=+p.source_id AND json_extract(c.data,'$.guideId')=+p.channel_id WHERE livetv_search_programmes MATCH ? AND p.end_ms>? AND p.start_ms<? ORDER BY p.start_ms, c.id LIMIT ?`
	}
	rows, err := s.o.DB.QueryContext(ctx, query, expr, fromMS, untilMS, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		emit(id, raw)
	}
	return rows.Err()
}

// CurrentProgrammeForWatch returns the programme currently airing on channelID
// for viewer u at time at, along with a stable programme key suitable for
// caching reverse live-event matches (A6) and detecting programme rollovers.
// Uses bounded PK lookup instead of scanning the full lineup.
// Returns (nil, "", nil) when no programme is on air or channel is unavailable.
func (s *Service) CurrentProgrammeForWatch(ctx context.Context, u *core.User, channelID string, at time.Time) (*WatchGuideEntry, string, error) {
	if s == nil || s.o.DB == nil || u == nil || strings.TrimSpace(channelID) == "" {
		return nil, "", nil
	}
	if at.IsZero() {
		at = s.o.Now()
	}
	ctx, pol, err := s.withPolicy(ctx, u)
	if err != nil {
		return nil, "", err
	}
	if !u.IsOwner && pol.Disabled {
		return nil, "", nil
	}
	recs, err := s.channelsByIDs(ctx, []string{channelID})
	if err != nil {
		return nil, "", err
	}
	c, ok := recs[channelID]
	if !ok {
		return nil, "", nil
	}
	if app := core.LiveApp(ctx); app != "" && slices.Contains(c.HiddenApps, app) {
		return nil, "", nil
	}
	if !s.audienceAllowed(ctx, u, c) || s.channelWatchHealth(ctx, c, at) != watchChannelHealthy {
		return nil, "", nil
	}
	recentWatched, _ := s.viewerRecentWatched(u)
	isFav := c.Favorite || slices.Contains(pol.Favorites, c.ID)
	p, found, err := s.programmeAt(ctx, c, at)
	if err != nil {
		return nil, "", err
	}
	if !found {
		// Guide-free fallback (A6): channel is visible to the viewer but has
		// no current EPG programme; expose its metadata so the matcher can
		// link it to an unambiguously matching live event.
		entry := &WatchGuideEntry{
			ChannelID:     c.ID,
			ChannelName:   c.Name,
			ChannelNumber: c.Number,
			ChannelLogo:   c.Logo,
			TvgName:       c.TvgName,
			GuideID:       c.GuideID,
			Group:         c.Group,
			IsEventAddon:  isStremioEventChannel(c),
			ChannelOnly:   true,
			Favorite:      isFav,
			WatchedAt:     recentWatched[c.ID],
			Live:          true,
		}
		progKey := fmt.Sprintf("%s|channel-only|%s|%s|%s", c.ID, c.Name, c.TvgName, c.Group)
		return entry, progKey, nil
	}
	if !s.allowedProgramme(ctx, u, c, &p) {
		return nil, "", nil
	}
	entry := &WatchGuideEntry{
		ChannelID:     c.ID,
		ChannelName:   c.Name,
		ChannelNumber: c.Number,
		ChannelLogo:   c.Logo,
		TvgName:       c.TvgName,
		GuideID:       c.GuideID,
		Group:         c.Group,
		IsEventAddon:  len(p.EventStreams) > 0 || isStremioEventChannel(c),
		Favorite:      isFav,
		WatchedAt:     recentWatched[c.ID],
		Programme:     p,
		ArchiveDays:   c.ArchiveDays(),
		Live:          true,
	}
	progKey := fmt.Sprintf("%s|%d|%d|%s", c.ID, p.Start.UnixMilli(), p.End.UnixMilli(), p.Item.Title)
	return entry, progKey, nil
}

// isStremioEventChannel identifies 0.3.18 Stremio event channels synthesized by
// internal/plugins/adapters/livetv_events.go.
func isStremioEventChannel(c Channel) bool {
	if strings.HasPrefix(c.GuideID, "event:") {
		return true
	}
	if isEventPluginURL(c.StreamURL) {
		return true
	}
	for _, src := range c.StreamSources {
		if isEventPluginURL(src.URL) {
			return true
		}
	}
	return false
}

func isEventPluginURL(u string) bool {
	return strings.HasPrefix(u, "plugin://") && strings.Contains(u, "/event/")
}

func sanitizeWatchTokens(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, r := range raw {
		for _, w := range searchWords(r) {
			if w == "" || seen[w] {
				continue
			}
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

func hasDigitRune(s string) bool {
	for _, r := range s {
		if unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// expandWatchCallsignQueries turns raw broadcast callsigns into normalized
// word phrases (plus aliases such as FS1 <-> FOX Sports 1, TSN 1 <-> TSN1,
// ESPN 2 <-> ESPN2, beIN Sports -> beIN Sports 1) suitable for migration 27's
// FTS5/GIN channel search index.
func expandWatchCallsignQueries(callsigns []string) [][]string {
	if len(callsigns) == 0 {
		return nil
	}
	var out [][]string
	seen := make(map[string]bool)
	add := func(words []string) {
		words = sanitizeWatchTokens(words)
		if len(words) == 0 {
			return
		}
		if len(words) == 1 && len(words[0]) < 2 {
			return
		}
		key := strings.Join(words, " ")
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, words)
	}
	isDropSuffix := func(tok string) bool {
		switch tok {
		case "hd", "fhd", "uhd", "sd", "4k", "8k", "hevc", "raw",
			"east", "west", "tv", "network", "channel", "chnl", "ch",
			"backup", "bkup", "bkp", "bak", "alt", "alternate", "1080p", "720p", "2160p":
			return true
		default:
			return false
		}
	}
	for _, raw := range callsigns {
		words := searchWords(raw)
		if len(words) == 0 {
			continue
		}
		var base []string
		for _, w := range words {
			if isDropSuffix(w) {
				continue
			}
			if w == "net" && len(base) > 0 {
				continue
			}
			base = append(base, w)
		}
		if len(base) == 0 {
			base = words
		}
		add(base)
		key := strings.Join(base, " ")
		switch key {
		case "fs1":
			add([]string{"fox", "sports", "1"})
		case "fox sports 1":
			add([]string{"fs1"})
		case "fs2":
			add([]string{"fox", "sports", "2"})
		case "fox sports 2":
			add([]string{"fs2"})
		case "espn2":
			add([]string{"espn", "2"})
		case "espn 2":
			add([]string{"espn2"})
		case "nbatv":
			add([]string{"nba"})
		case "cbssn":
			add([]string{"cbs", "sports"})
		case "cbs sports":
			add([]string{"cbssn"})
		case "tru":
			add([]string{"trutv"})
		case "trutv":
			add([]string{"tru"})
		case "nfln":
			add([]string{"nfl"})
		case "mlbn":
			add([]string{"mlb"})
		case "nhln":
			add([]string{"nhl"})
		case "bein sports":
			add([]string{"bein", "sports", "1"})
		case "bein sports 1":
			add([]string{"bein", "sports"})
		case "sn1", "sportsnet 1", "sportsnet one":
			add([]string{"sn1"})
			add([]string{"sportsnet", "1"})
			add([]string{"sportsnet", "one"})
		case "sn360":
			add([]string{"sportsnet", "360"})
		case "sky sports pl":
			add([]string{"sky", "sports", "premier", "league"})
		case "sky sports me":
			add([]string{"sky", "sports", "main", "event"})
		}
		// General "<word> <digit>" <-> "<word><digit>" (e.g. TSN 1 <-> TSN1, DAZN 1 <-> DAZN1).
		if len(base) == 2 && len(base[0]) >= 3 && isAllASCIIDigits(base[1]) && !hasDigitRune(base[0]) {
			add([]string{base[0] + base[1]})
		} else if len(base) == 1 {
			if alpha, num, ok := splitAlphaTrailingDigits(base[0]); ok && len(alpha) >= 3 {
				add([]string{alpha, num})
			}
		}
	}
	return out
}

func isAllASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func splitAlphaTrailingDigits(s string) (string, string, bool) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == 0 || i == len(s) {
		return "", "", false
	}
	for j := 0; j < i; j++ {
		if s[j] < 'a' || s[j] > 'z' {
			return "", "", false
		}
	}
	return s[:i], s[i:], true
}
