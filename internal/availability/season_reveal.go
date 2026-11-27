package availability

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/store"
)

const (
	seasonRevealCooldown  = 6 * time.Hour
	seasonRevealHourlyMax = 20
	seasonRevealMaxChecks = 12
	// seasonRevealEpisodeSearches bounds per-episode lookups after the season
	// search, for sources that return only packs to a season search.
	seasonRevealEpisodeSearches = 4
	seasonRevealTimeout         = 25 * time.Second
)

func init() {
	store.RegisterAfter("availability_season_reveal", "availability")
	store.Register("availability_season_reveal", 1, `CREATE TABLE availability_season_reveal(season_key INTEGER PRIMARY KEY, checked_at INTEGER NOT NULL);`)
	store.RegisterDialect("availability_season_reveal", 1, store.Postgres, `CREATE TABLE availability_season_reveal(season_key BIGINT PRIMARY KEY, checked_at BIGINT NOT NULL);`)
}

// RevealTitleSeason resolves a show, season, or episode key to its regular
// season and looks for already-discovered evidence off the request path.
func (s *Service) RevealTitleSeason(key core.RatingKey) {
	if s == nil || s.d.Search == nil || key == 0 {
		return
	}
	ctx := core.WithCachedIndexerSearch(core.WithUnattendedSearch(s.bgCtx))
	switch key.Kind() {
	case core.KindSeason:
		s.startSeasonReveal(ctx, key, true)
	case core.KindEpisode, core.KindShow:
		s.visibilityMu.Lock()
		if s.closed || s.bgCtx.Err() != nil {
			s.visibilityMu.Unlock()
			return
		}
		s.bg.Add(1)
		s.visibilityMu.Unlock()
		go func() {
			defer s.bg.Done()
			var seasonKey core.RatingKey
			if key.Kind() == core.KindShow {
				seasonKey = s.latestStartedSeason(s.bgCtx, key)
			} else if title, err := s.d.Catalog.CachedTitle(s.bgCtx, key); err == nil && title != nil {
				seasonKey = releaseSeasonKey(title)
			}
			if seasonKey != 0 {
				s.startSeasonReveal(ctx, seasonKey, false)
			}
		}()
	}
}

// releaseSeasonKey resolves an episode's regular effective season container.
func releaseSeasonKey(t *core.Title) core.RatingKey {
	if t.Type != core.TypeEpisode || t.Season <= 0 {
		return 0
	}
	if t.ParentKey != 0 {
		return t.ParentKey
	}
	return t.Key.Parent()
}

// RevealSeason schedules an asynchronous season-level release check for an
// eligible in-progress regular season, independent of Monitor/Verify toggles.
func (s *Service) RevealSeason(_ context.Context, seasonKey core.RatingKey) {
	if s == nil || s.d.Search == nil {
		return
	}
	if seasonKey.Kind() != core.KindSeason {
		return
	}
	s.startSeasonReveal(s.bgCtx, seasonKey, true)
}

func (s *Service) startSeasonReveal(ctx context.Context, seasonKey core.RatingKey, async bool) {
	now := s.now()
	s.revealMu.Lock()
	if s.revealInFlight == nil {
		s.revealInFlight = map[core.RatingKey]bool{}
	}
	if s.revealChecked == nil {
		s.revealChecked = map[core.RatingKey]time.Time{}
	}
	if s.revealInFlight[seasonKey] {
		s.revealMu.Unlock()
		return
	}
	if last := s.revealChecked[seasonKey]; !last.IsZero() && now.Sub(last) < seasonRevealCooldown {
		s.revealMu.Unlock()
		return
	}
	s.revealInFlight[seasonKey] = true
	s.revealMu.Unlock()

	finish := func() {
		s.revealMu.Lock()
		delete(s.revealInFlight, seasonKey)
		s.revealMu.Unlock()
	}

	if !async {
		defer finish()
		s.runRevealSeason(ctx, seasonKey)
		return
	}

	s.visibilityMu.Lock()
	if s.closed || s.bgCtx.Err() != nil {
		s.visibilityMu.Unlock()
		finish()
		return
	}
	s.bg.Add(1)
	s.visibilityMu.Unlock()

	go func() {
		defer s.bg.Done()
		defer finish()
		s.runRevealSeason(ctx, seasonKey)
	}()
}

func (s *Service) runRevealSeason(ctx context.Context, seasonKey core.RatingKey) {
	if ctx.Err() != nil {
		return
	}
	now := s.now()
	episodes, err := s.seasonEpisodes(ctx, seasonKey)
	if err != nil || len(episodes) == 0 {
		return
	}
	playable := s.playableEpisodeReleases(ctx, episodes)
	releasedCount := 0
	var unreleased []core.Title
	for _, ep := range episodes {
		if ep.Adult || ep.Season <= 0 || ep.Episode <= 0 {
			continue
		}
		if s.episodeReleased(ep, now, playable) {
			releasedCount++
		} else {
			unreleased = append(unreleased, ep)
		}
	}
	if releasedCount == 0 || len(unreleased) == 0 {
		if releasedCount > 0 && len(unreleased) == 0 {
			// Completed seasons cannot gain unreleased episodes until metadata changes;
			// memoize in memory without spending the hourly server-wide search cap.
			s.revealMu.Lock()
			s.revealChecked[seasonKey] = now
			s.revealMu.Unlock()
		}
		return
	}
	if !s.claimSeasonRevealSlot(ctx, seasonKey, now) {
		return
	}

	sample := unreleased[0]
	showKey := sample.ShowKey
	if showKey == 0 {
		showKey = seasonKey.Parent()
	}
	var show *core.Title
	if got, e := s.d.Catalog.CachedTitle(ctx, showKey); e == nil && got != nil {
		show = got
	}
	for i := range unreleased {
		ep := &unreleased[i]
		if ep.ShowKey == 0 {
			ep.ShowKey = showKey
		}
		if show != nil {
			ep.ShowIDs = show.IDs
			if ep.ShowTitle == "" {
				ep.ShowTitle = show.Title
			}
			if ep.ShowOriginalTitle == "" {
				ep.ShowOriginalTitle = show.OriginalTitle
			}
			if ep.ShowYear == 0 {
				ep.ShowYear = show.Year
			}
			if ep.Language == "" {
				ep.Language = show.Language
			}
			if len(ep.AlternativeTitles) == 0 {
				ep.AlternativeTitles = show.AlternativeTitles
			}
		}
	}

	sample = unreleased[0]
	req := core.SearchRequest{
		Type:              core.TypeEpisode,
		EpisodeSource:     sample.EpisodeOrder.Source,
		IDs:               sample.ShowIDs,
		Title:             cmp.Or(sample.ShowTitle, sample.Title),
		OriginalTitle:     sample.ShowOriginalTitle,
		Year:              cmp.Or(sample.ShowYear, sample.Year),
		AlternativeTitles: sample.AlternativeTitles,
		Season:            sample.Season,
	}
	if show != nil {
		req.IDs = show.IDs
		req.Title = show.Title
		req.OriginalTitle = show.OriginalTitle
		req.Year = show.Year
		req.AlternativeTitles = show.AlternativeTitles
	}
	if req.Title == "" && len(req.IDs.IMDB) == 0 && req.IDs.TMDB == 0 && req.IDs.TVDB == 0 {
		return
	}

	_, cfg := s.discoverySnap()
	live := s.budgets()
	rctx := core.WithoutOnDemandStreams(
		core.WithUnattendedSearch(
			core.WithIndexerPurpose(
				core.WithForegroundReserve(ctx, live.ReservePercent),
				core.IndexerPurposeWarmup,
			),
		),
	)
	sctx := rctx
	if len(cfg.IndexerIDs) > 0 && s.d.Feeds != nil {
		sources := s.d.Feeds.FeedSources()
		if len(sources) > 0 {
			skip := make(map[string]bool, len(sources))
			for _, f := range sources {
				if !selected(cfg.IndexerIDs, f.ID) {
					skip[f.ID] = true
				}
			}
			if len(skip) > 0 {
				sctx = core.WithIndexerSkip(rctx, skip)
			}
		}
	}

	qctx, cancel := context.WithTimeout(sctx, seasonRevealTimeout)
	releases, err := s.d.Search(qctx, req)
	cancel()
	if ctx.Err() != nil {
		return
	}
	_ = err // an empty or failed season search still leaves the per-episode lookups below
	for i := range releases {
		if releases[i].Info.Title == "" && releases[i].Name != "" {
			releases[i].Info = releaseparse.Parse(releases[i].Name)
		}
	}

	kept, verifiedHashes := s.cachedTorrentsForReveal(rctx, releases)

	type packState struct {
		checked bool
		known   bool
		match   func(*core.ReleaseTarget) bool
	}
	packs := map[string]packState{}
	inspectPack := func(raw core.Release) (func(*core.ReleaseTarget) bool, bool) {
		if s.d.PackFiles == nil || raw.Protocol != core.ProtocolTorrent || raw.InfoHash == "" {
			return nil, false
		}
		key := strings.ToLower(raw.InfoHash)
		if st, ok := packs[key]; ok && st.checked {
			return st.match, st.known
		}
		pctx, stop := context.WithTimeout(rctx, 15*time.Second)
		match, known := s.d.PackFiles(pctx, raw)
		stop()
		packs[key] = packState{checked: true, known: known, match: match}
		return match, known
	}

	var feeds []core.ReleaseFeed
	if s.d.Feeds != nil {
		feeds = s.d.Feeds.FeedSources()
	}
	policy := s.policy()
	gp := s.currentGatePolicy()
	checksUsed := 0

	type verifiedEpisode struct {
		ep     core.Title
		rel    core.Release
		src    core.ReleaseFeed
		reason string
	}
	var revealed []verifiedEpisode

	evaluate := func(ep core.Title, kept []core.Release, verifiedHashes map[string]bool) *verifiedEpisode {
		if ctx.Err() != nil {
			return nil
		}
		type candidatePair struct {
			raw      core.Release
			accepted core.Release
		}
		var acceptedList []candidatePair
		var chosen *verifiedEpisode

		for _, raw := range kept {
			if !candidateMayMatchEpisode(raw, ep) {
				continue
			}
			accepted, reason, e := s.d.Validator.FeedCandidate(rctx, &ep, raw)
			if e != nil || reason != "" {
				continue
			}
			accepted = ensureEpisodeTarget(accepted, raw, ep)
			hashCached := accepted.Protocol == core.ProtocolTorrent && accepted.InfoHash != "" &&
				(verifiedHashes[accepted.InfoHash] || verifiedHashes[strings.ToLower(accepted.InfoHash)])

			if accepted.Info.FullSeason {
				// A season pack counts before TMDB's date only when its cached torrent
				// file list is verified to contain this specific episode.
				if hashCached && s.d.PackFiles != nil {
					match, known := inspectPack(raw)
					if known && match != nil && match(accepted.Target) {
						src := revealFeedFor(feeds, accepted)
						chosen = &verifiedEpisode{
							ep:     ep,
							rel:    accepted,
							src:    src,
							reason: "Cached season pack verified to contain this episode",
						}
						break
					}
				}
				continue
			}
			if hashCached {
				src := revealFeedFor(feeds, accepted)
				chosen = &verifiedEpisode{
					ep:     ep,
					rel:    accepted,
					src:    src,
					reason: "Cached debrid copy verified for in-progress season",
				}
				break
			}
			if accepted.Protocol != core.ProtocolTorrent {
				acceptedList = append(acceptedList, candidatePair{raw: raw, accepted: accepted})
			}
		}

		if chosen == nil && len(acceptedList) > 0 && checksUsed < seasonRevealMaxChecks {
			sort.SliceStable(acceptedList, func(i, j int) bool {
				return acceptedList[i].accepted.Score > acceptedList[j].accepted.Score
			})
			top := acceptedList[0].accepted
			checksUsed++
			hctx, stop := context.WithTimeout(rctx, 20*time.Second)
			verr := s.d.Validator.VerifyFeedCandidate(hctx, &ep, top)
			stop()
			if ctx.Err() != nil {
				return nil
			}
			if verr == nil {
				src := revealFeedFor(feeds, top)
				chosen = &verifiedEpisode{
					ep:     ep,
					rel:    top,
					src:    src,
					reason: "Source health checked for in-progress season",
				}
			}
		}

		return chosen
	}
	var missed []core.Title
	for _, ep := range unreleased {
		if ctx.Err() != nil {
			return
		}
		if chosen := evaluate(ep, kept, verifiedHashes); chosen != nil {
			revealed = append(revealed, *chosen)
		} else {
			missed = append(missed, ep)
		}
	}
	// Search-only sources (Stremio add-ons) answer a season search with packs
	// only, so a drop released as single episodes needs its own lookup. The
	// earliest hidden episodes go first, a few per run.
	for i, ep := range missed {
		if i >= seasonRevealEpisodeSearches || ctx.Err() != nil {
			break
		}
		ereq := req
		ereq.Episode = ep.Episode
		ereq.EpisodeTitle = ep.Title
		ereq.Released = cmp.Or(ep.EpisodeAirDate, ep.Released)
		ectx, stop := context.WithTimeout(sctx, seasonRevealTimeout)
		found, _ := s.d.Search(ectx, ereq)
		stop()
		if ctx.Err() != nil {
			return
		}
		for j := range found {
			if found[j].Info.Title == "" && found[j].Name != "" {
				found[j].Info = releaseparse.Parse(found[j].Name)
			}
		}
		ekept, ehashes := s.cachedTorrentsForReveal(rctx, found)
		if chosen := evaluate(ep, ekept, ehashes); chosen != nil {
			revealed = append(revealed, *chosen)
		}
	}

	if len(revealed) == 0 {
		return
	}

	revealAt := s.now()
	revealTS := revealAt.Unix()
	keys := make([]core.RatingKey, len(revealed))
	for i, item := range revealed {
		keys[i] = item.ep.Key
		s.rememberVerifiedReveal(ctx, item.src, item.ep, item.rel, policy, item.reason, revealTS)
	}
	if err := s.recordPlayableEpisodes(ctx, keys, revealAt); err != nil {
		s.log.Warn("season reveal could not record playable episodes", "season", seasonKey, "error", err)
		return
	}
	if s.hasGateTable && gp != "" {
		for _, item := range revealed {
			_ = s.openGate(ctx, gp, item.ep.Key, item.src.ID, revealTS)
		}
	}
	s.clearVisibility()
}

func candidateMayMatchEpisode(r core.Release, ep core.Title) bool {
	if r.Name == "" && r.Info.Title == "" && len(r.Info.Seasons) == 0 && len(r.Info.Episodes) == 0 {
		return true
	}
	if releaseparse.EpisodeMatches(r.Info, ep.Season, ep.Episode) || releaseparse.EpisodeMatchesAirDate(r.Info, cmp.Or(ep.EpisodeAirDate, ep.Released)) {
		return true
	}
	if r.Info.FullSeason {
		if len(r.Info.Seasons) == 0 {
			return true
		}
		for _, n := range r.Info.Seasons {
			if n == ep.Season {
				return true
			}
		}
	}
	return false
}

func ensureEpisodeTarget(accepted, raw core.Release, ep core.Title) core.Release {
	baseID := cmp.Or(raw.ID, accepted.ID, strings.ToLower(accepted.InfoHash), accepted.Name)
	if accepted.Info.FullSeason {
		if accepted.Target == nil {
			target := core.ReleaseTarget{
				Season:       ep.Season,
				Episode:      ep.Episode,
				EpisodeTitle: ep.Title,
				PackID:       baseID,
			}
			accepted.Target = &target
			if baseID != "" && (accepted.ID == "" || accepted.ID == raw.ID) {
				accepted.ID = core.TargetReleaseID(baseID, target)
			}
		} else if accepted.ID == "" && baseID != "" {
			accepted.ID = core.TargetReleaseID(baseID, *accepted.Target)
		}
	}
	if accepted.ID == "" {
		accepted.ID = baseID
	}
	return accepted
}

func revealFeedFor(feeds []core.ReleaseFeed, r core.Release) core.ReleaseFeed {
	src := feedFor(feeds, r)
	if src.ID == "" {
		src = core.ReleaseFeed{
			ID:         "probe",
			Name:       cmp.Or(r.Indexer, "probe"),
			Generation: "probe",
			Protocol:   r.Protocol,
			Supported:  true,
		}
	}
	return src
}

func (s *Service) rememberVerifiedReveal(ctx context.Context, src core.ReleaseFeed, ep core.Title, r core.Release, policy, reason string, nowTS int64) {
	if r.ID == "" {
		return
	}
	if err := s.remember(ctx, src, ep, r, policy); err != nil {
		return
	}
	_, err := s.db.ExecContext(ctx, `UPDATE availability_releases SET state='verified',reason=?,verified_at=?,checked_id=?,
 first_verified=CASE WHEN first_verified=0 THEN ? ELSE first_verified END,
 policy=?,next_check=? WHERE id=? AND rating_key=? AND source_id=? AND generation=?`,
		reason, nowTS, r.ID, nowTS, policy, nowTS+int64((6*time.Hour).Seconds()), r.ID, ep.Key, src.ID, src.Generation)
	if err == nil {
		s.publishConsoleEvidence(ctx, ep.Key, r, src.ID, policy, 1)
	}
}

func (s *Service) cachedTorrentsForReveal(ctx context.Context, rs []core.Release) ([]core.Release, map[string]bool) {
	if s.d.CacheCheck == nil {
		return rs, nil
	}
	var hashes []string
	seen := map[string]bool{}
	for _, r := range rs {
		if r.Protocol == core.ProtocolTorrent && r.InfoHash != "" {
			key := strings.ToLower(r.InfoHash)
			if !seen[key] {
				seen[key] = true
				hashes = append(hashes, r.InfoHash)
			}
		}
	}
	if len(hashes) == 0 {
		return rs, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	cached, definitive := s.d.CacheCheck(cctx, hashes)
	cancel()
	if !definitive {
		return rs, nil
	}
	verified := map[string]bool{}
	out := make([]core.Release, 0, len(rs))
	for _, r := range rs {
		if r.Protocol == core.ProtocolTorrent && r.InfoHash != "" {
			lower := strings.ToLower(r.InfoHash)
			if !cached[r.InfoHash] && !cached[lower] {
				continue
			}
			verified[r.InfoHash] = true
			verified[lower] = true
		}
		out = append(out, r)
	}
	return out, verified
}

func (s *Service) claimSeasonRevealSlot(ctx context.Context, seasonKey core.RatingKey, now time.Time) bool {
	nowTS := now.Unix()
	cooldownCutoff := now.Add(-seasonRevealCooldown).Unix()
	hourCutoff := now.Add(-time.Hour).Unix()

	if s.hasSeasonRevealTable {
		var checkedAt int64
		err := s.db.QueryRowContext(ctx, `SELECT checked_at FROM availability_season_reveal WHERE season_key=?`, int64(seasonKey)).Scan(&checkedAt)
		if err == nil && checkedAt > cooldownCutoff {
			s.revealMu.Lock()
			s.revealChecked[seasonKey] = time.Unix(checkedAt, 0)
			s.revealMu.Unlock()
			return false
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false
		}
		var recentRuns int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM availability_season_reveal WHERE checked_at>?`, hourCutoff).Scan(&recentRuns); err == nil && recentRuns >= seasonRevealHourlyMax {
			return false
		}
	}

	s.revealMu.Lock()
	if last := s.revealChecked[seasonKey]; !last.IsZero() && now.Sub(last) < seasonRevealCooldown {
		s.revealMu.Unlock()
		return false
	}
	hourAgo := now.Add(-time.Hour)
	kept := s.revealRuns[:0]
	for _, at := range s.revealRuns {
		if at.After(hourAgo) {
			kept = append(kept, at)
		}
	}
	s.revealRuns = kept
	if len(s.revealRuns) >= seasonRevealHourlyMax {
		s.revealMu.Unlock()
		return false
	}
	s.revealChecked[seasonKey] = now
	s.revealRuns = append(s.revealRuns, now)
	s.revealMu.Unlock()

	if s.hasSeasonRevealTable {
		_, _ = s.db.ExecContext(ctx, `INSERT INTO availability_season_reveal(season_key,checked_at) VALUES(?,?) ON CONFLICT(season_key) DO UPDATE SET checked_at=excluded.checked_at`, int64(seasonKey), nowTS)
	}
	return true
}

func (s *Service) seasonEpisodes(ctx context.Context, seasonKey core.RatingKey) ([]core.Title, error) {
	if all, ok := s.d.Catalog.(interface {
		SeasonEpisodesAll(context.Context, core.RatingKey) ([]core.Title, error)
	}); ok {
		return all.SeasonEpisodesAll(ctx, seasonKey)
	}
	return nil, nil
}

func (s *Service) playableEpisodeReleases(ctx context.Context, episodes []core.Title) map[core.RatingKey]time.Time {
	if src, ok := s.d.Catalog.(interface {
		PlayableEpisodeReleases(context.Context, []core.RatingKey) (map[core.RatingKey]time.Time, error)
	}); ok {
		keys := make([]core.RatingKey, len(episodes))
		for i, ep := range episodes {
			keys[i] = ep.Key
		}
		if got, err := src.PlayableEpisodeReleases(ctx, keys); err == nil {
			return got
		}
	}
	return nil
}

func (s *Service) episodeReleased(ep core.Title, now time.Time, playable map[core.RatingKey]time.Time) bool {
	if ep.IsReleased(now) {
		return true
	}
	if at, ok := playable[ep.Key]; ok && !at.IsZero() && !at.After(now) {
		return true
	}
	return false
}

func (s *Service) latestStartedSeason(ctx context.Context, showKey core.RatingKey) core.RatingKey {
	if latest, ok := s.d.Catalog.(interface {
		LatestStartedSeason(context.Context, core.RatingKey) (core.RatingKey, error)
	}); ok {
		sk, err := latest.LatestStartedSeason(ctx, showKey)
		if err == nil {
			return sk
		}
	}
	return 0
}

// isSeasonInProgress reports whether seasonKey is a regular season with at
// least one released episode and at least one unreleased episode at now.
func (s *Service) isSeasonInProgress(ctx context.Context, seasonKey core.RatingKey, now time.Time, candidates []core.Title, memo map[core.RatingKey]bool) bool {
	if seasonKey.Kind() != core.KindSeason {
		return false
	}
	if v, ok := memo[seasonKey]; ok {
		return v
	}
	if eps, err := s.seasonEpisodes(ctx, seasonKey); err == nil && len(eps) > 0 {
		playable := s.playableEpisodeReleases(ctx, eps)
		hasReleased, hasUnreleased := false, false
		for _, ep := range eps {
			if ep.Adult || ep.Season <= 0 || ep.Episode <= 0 {
				continue
			}
			if s.episodeReleased(ep, now, playable) {
				hasReleased = true
			} else {
				hasUnreleased = true
			}
		}
		inProgress := hasReleased && hasUnreleased
		memo[seasonKey] = inProgress
		return inProgress
	}

	hasReleased, hasUnreleased := false, false
	for _, t := range candidates {
		if releaseSeasonKey(&t) == seasonKey && t.Episode > 0 && !t.Adult {
			if t.IsReleased(now) {
				hasReleased = true
			} else {
				hasUnreleased = true
			}
		}
	}
	// A packed TMDB season still has an addressable premiere when only the
	// premiere is mirrored. Allocated seasons need stored episode mappings.
	if !hasReleased && !seasonKey.Allocated() && seasonKey.Season() > 0 {
		if ep1, err := s.d.Catalog.CachedTitle(ctx, core.EpisodeKey(seasonKey.TMDB(), seasonKey.Season(), 1)); err == nil && ep1 != nil && ep1.IsReleased(now) {
			hasReleased = true
		}
	}
	inProgress := hasReleased && hasUnreleased
	memo[seasonKey] = inProgress
	return inProgress
}
