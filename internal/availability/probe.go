package availability

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// probeDue executes one indexed due-items query against
// availability_title_checks_due and searches at most probeBatch gated titles
// per cycle within the configured daily allowances, popular/watchlisted first.
func (s *Service) probeDue(ctx context.Context) error {
	_, cfg := s.discoverySnap()
	if s.d.Search == nil || !cfg.Monitor {
		return nil
	}
	if err := s.seedDueChecks(ctx, cfg); err != nil {
		return err
	}
	if !s.hasGateTable {
		return nil
	}

	now := s.now()
	nowTS := now.Unix()
	rows, err := s.db.QueryContext(ctx, `SELECT rating_key,kind,anchor_ts,window_end_ts,priority,state,next_check FROM availability_title_checks AS checks
 WHERE next_check>0 AND next_check<=? AND state<>'found'
 AND NOT EXISTS (SELECT 1 FROM availability_gate WHERE rating_key=checks.rating_key) ORDER BY priority DESC,next_check ASC LIMIT ?`, nowTS, probeBatch)
	if err != nil {
		return err
	}
	type dueRow struct {
		key       core.RatingKey
		kind      string
		anchor    int64
		windowEnd int64
		priority  float64
		state     string
		next      int64
	}
	var due []dueRow
	for rows.Next() {
		var k int64
		var r dueRow
		if err := rows.Scan(&k, &r.kind, &r.anchor, &r.windowEnd, &r.priority, &r.state, &r.next); err != nil {
			rows.Close()
			return err
		}
		r.key = core.RatingKey(k)
		due = append(due, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(due) == 0 {
		return err
	}

	input := s.discoverySnapshot()
	keys := make([]core.RatingKey, 0, len(due))
	for _, item := range due {
		if item.windowEnd == 0 || nowTS < item.windowEnd {
			keys = append(keys, item.key)
		}
	}
	open := s.openGateKeys(ctx, input.gatePolicy, keys)
	pending := keys[:0]
	for _, key := range keys {
		if !open[key] {
			pending = append(pending, key)
		}
	}
	var titles []core.Title
	if len(pending) > 0 {
		titles, err = s.d.Catalog.CachedTitles(ctx, pending)
		if err != nil {
			return err
		}
	}
	byKey := make(map[core.RatingKey]*core.Title, len(titles))
	for i := range titles {
		byKey[titles[i].Key] = &titles[i]
	}
	feeds := make(map[string]core.ReleaseFeed, len(input.feeds))
	for _, f := range input.feeds {
		feeds[f.ID] = f
	}
	// Check retained observations before spending a provider request on a
	// due title, including the first check after its gate policy changed.
	if err := s.reopenRetainedGates(offRequest(ctx), byKey, feeds, input, open); err != nil {
		return err
	}
	for _, item := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		basePrio := item.priority
		if basePrio >= firstLookPriorityBoost {
			basePrio -= firstLookPriorityBoost
		}
		if item.windowEnd > 0 && nowTS >= item.windowEnd {
			_, _ = s.db.ExecContext(ctx, `UPDATE availability_title_checks SET next_check=0,priority=? WHERE rating_key=?`, basePrio, int64(item.key))
			continue
		}
		if open[item.key] {
			_, _ = s.db.ExecContext(ctx, `UPDATE availability_title_checks SET next_check=0,state='found',reason='',priority=? WHERE rating_key=?`, basePrio, int64(item.key))
			continue
		}
		t := byKey[item.key]
		if t == nil || t.Adult {
			_, _ = s.db.ExecContext(ctx, `UPDATE availability_title_checks SET next_check=0,priority=? WHERE rating_key=?`, basePrio, int64(item.key))
			continue
		}
		if nowTS < item.anchor || !t.IsReleased(now) {
			nextAt := max(item.anchor, t.Released.Unix())
			if item.windowEnd > 0 && nextAt >= item.windowEnd {
				nextAt = 0
			}
			_, _ = s.db.ExecContext(ctx, `UPDATE availability_title_checks SET next_check=? WHERE rating_key=?`, nextAt, int64(item.key))
			continue
		}
		retry := item.state == "retry"
		next, state := int64(0), "none"
		if !retry {
			next, state = s.now().Add(15*time.Minute).Unix(), "retry"
		}
		// Claim the first look before making any request. An interrupted
		// first look leaves its one retry queued.
		res, err := s.db.ExecContext(ctx, `UPDATE availability_title_checks AS checks
 SET checked_at=?,next_check=?,state=? WHERE rating_key=? AND next_check=? AND state=?
 AND NOT EXISTS (SELECT 1 FROM availability_gate WHERE rating_key=checks.rating_key)`,
			nowTS, next, state, int64(item.key), item.next, item.state)
		if err != nil {
			return err
		}
		if claimed, err := res.RowsAffected(); err != nil || claimed == 0 {
			continue
		}
		err = s.probeOneDue(ctx, *t, item.anchor, item.windowEnd, basePrio, retry)
		if err != nil {
			if benign(err) {
				return err
			}
			s.log.Warn("title probe failed", "title", t.Title, "error", err)
		}
	}
	return nil
}

const (
	// probeBatch bounds the titles probed per monitor cycle (a minute).
	probeBatch = 10
	// probeSourceTimeout bounds one source's answer to a probe.
	probeSourceTimeout = 20 * time.Second
)

// probeOneDue asks one eligible source (or the search-only addon fleet) for t.
func (s *Service) probeOneDue(ctx context.Context, t core.Title, anchorTS, windowEndTS int64, basePrio float64, retry bool) error {
	anime := t.IsAnime()
	if t.Type == core.TypeEpisode && t.ShowKey != 0 {
		if show, err := s.d.Catalog.CachedTitle(ctx, t.ShowKey); err == nil && show != nil {
			anime = anime || show.IsAnime()
			t.ShowIDs = show.IDs
			if t.ShowTitle == "" {
				t.ShowTitle = show.Title
			}
			t.ShowOriginalTitle = show.OriginalTitle
			t.ShowYear = show.Year
			if t.Language == "" {
				t.Language = show.Language
			}
		}
	}
	req := core.SearchRequest{
		Type:              t.Type,
		IDs:               t.IDs,
		Title:             t.Title,
		OriginalTitle:     t.OriginalTitle,
		Year:              t.Year,
		AlternativeTitles: t.AlternativeTitles,
		Released:          t.Released,
		Anime:             anime,
	}
	if t.Type == core.TypeEpisode {
		req.IDs = t.ShowIDs
		req.EpisodeSource = t.EpisodeOrder.Source
		req.Released = cmp.Or(t.EpisodeAirDate, t.Released)
		req.Title = cmp.Or(t.ShowTitle, t.Title)
		req.OriginalTitle = t.ShowOriginalTitle
		req.Year = cmp.Or(t.ShowYear, t.Year)
		req.Season = t.Season
		req.Episode = t.Episode
		req.EpisodeTitle = t.Title
		req.AbsoluteEpisode = t.AbsoluteEpisode
	}

	sources := s.d.Feeds.FeedSources()
	_, cfg := s.discoverySnap()
	ids := cfg.IndexerIDs
	policy := s.policy()
	quality := ""
	asked, denied, failed := 0, 0, 0

	var targets, dedicated []core.ReleaseFeed
	for _, f := range sources {
		if !selected(ids, f.ID) {
			continue
		}
		switch {
		case f.AnimeOnly && anime:
			dedicated = append(dedicated, f)
		case !f.AnimeOnly:
			family := 2000
			if req.Type == core.TypeEpisode {
				family = 5000
			}
			if len(f.Categories) == 0 || slices.ContainsFunc(f.Categories, func(cat int) bool { return cat/1000*1000 == family }) {
				targets = append(targets, f)
			}
		}
	}
	// Support search-only installations (Stremio add-ons, debrid scrapers) that
	// have no Newznab/Torznab feed sources registered.
	if len(sources) == 0 && len(ids) == 0 {
		targets = []core.ReleaseFeed{{ID: "probe", Name: "probe", Generation: "probe", Supported: true}}
	}

	// Spread first looks across eligible sources. Fetch at most one
	// targeted search per attempt.
	slot := uint64(max(0, (s.now().Unix()-anchorTS)/3600)) + uint64(t.Key)
	if len(targets) > 1 {
		first := int(slot % uint64(len(targets)))
		targets = targets[first : first+1]
	}
	if len(dedicated) > 0 {
		source := dedicated[slot%uint64(len(dedicated))]
		targets = []core.ReleaseFeed{source}
	}
	for _, f := range targets {
		var skip map[string]bool
		if len(sources) > 0 {
			skip = make(map[string]bool, len(sources))
			for _, other := range sources {
				if other.ID != f.ID {
					skip[other.ID] = true
				}
			}
		}
		bctx, cancel := context.WithTimeout(core.WithUnattendedSearch(core.WithIndexerSkip(s.budgetContext(ctx, titleSearch), skip)), probeSourceTimeout)
		releases, err := s.d.Search(bctx, req)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && len(releases) == 0 {
			if errors.Is(err, core.ErrBudgetExhausted) || errors.Is(err, context.Canceled) {
				denied++
			} else {
				failed++
			}
			continue
		}
		asked++
		kept, verifiedHashes := s.cachedTorrentsWithVerified(ctx, releases)
		for _, r := range kept {
			accepted, reason, e := s.d.Validator.FeedCandidate(ctx, &t, r)
			if e != nil {
				return e
			}
			if reason != "" {
				if quality == "" {
					quality = reason
				}
				continue
			}
			src := feedFor(sources, accepted)
			if src.ID == "" {
				src = core.ReleaseFeed{ID: "probe", Name: accepted.Indexer, Generation: "probe", Protocol: accepted.Protocol, Supported: true}
			}
			if e = s.remember(ctx, src, t, accepted, policy); e != nil {
				return e
			}
			if accepted.InfoHash != "" && verifiedHashes[accepted.InfoHash] {
				_ = s.openGate(ctx, s.currentGatePolicy(), t.Key, src.ID, s.now().Unix())
			}
			if s.isGateOpen(ctx, s.currentGatePolicy(), t.Key) {
				return s.saveTitleCheckDue(ctx, t.Key, "found", "", 0, basePrio)
			}
			return s.saveTitleCheckDue(ctx, t.Key, "none", "", 0, basePrio)
		}
	}
	if asked == 0 && (denied > 0 || failed > 0) {
		nextAt := int64(0)
		state := "none"
		if !retry {
			now := s.now()
			next := now.Add(15 * time.Minute)
			if denied > 0 {
				next = now.UTC().Truncate(24 * time.Hour).Add(24*time.Hour + time.Duration(uint64(t.Key)*2654435761%3600)*time.Second)
			}
			if windowEndTS == 0 || next.Unix() < windowEndTS {
				nextAt, state = next.Unix(), "retry"
			}
		}
		if err := s.saveTitleCheckDue(ctx, t.Key, state, "Search could not be completed", nextAt, basePrio); err != nil {
			return err
		}
		return nil
	}
	if quality != "" {
		return s.saveTitleCheckDue(ctx, t.Key, "quality", quality, 0, basePrio)
	}
	return s.saveTitleCheckDue(ctx, t.Key, "none", "", 0, basePrio)
}

func (s *Service) cachedTorrentsWithVerified(ctx context.Context, rs []core.Release) ([]core.Release, map[string]bool) {
	if s.d.CacheCheck == nil {
		return rs, nil
	}
	var hashes []string
	for _, r := range rs {
		if r.Protocol == core.ProtocolTorrent && r.InfoHash != "" {
			hashes = append(hashes, r.InfoHash)
		}
	}
	if len(hashes) == 0 {
		return rs, nil
	}
	bctx, cancel := context.WithTimeout(s.budgetContext(ctx, verifyCheck), 15*time.Second)
	cached, definitive := s.d.CacheCheck(bctx, hashes)
	cancel()
	if !definitive {
		return rs, nil
	}
	verified := map[string]bool{}
	out := rs[:0]
	for _, r := range rs {
		if r.Protocol == core.ProtocolTorrent && r.InfoHash != "" {
			if !cached[r.InfoHash] {
				continue
			}
			verified[r.InfoHash] = true
		}
		out = append(out, r)
	}
	return out, verified
}

func feedFor(feeds []core.ReleaseFeed, r core.Release) core.ReleaseFeed {
	for _, f := range feeds {
		if f.Name == r.Indexer || f.ID == r.Indexer {
			return f
		}
	}
	return core.ReleaseFeed{}
}

// titleCheck is a title's last probe: when, what it found, and when the next
// scheduled retry runs (zero: nothing pending).
type titleCheck struct {
	checked, next time.Time
	state, reason string
}

func (s *Service) titleCheck(ctx context.Context, key core.RatingKey) (titleCheck, error) {
	var checked, next int64
	var c titleCheck
	err := s.db.QueryRowContext(ctx, `SELECT checked_at,next_check,state,reason FROM availability_title_checks WHERE rating_key=?`, int64(key)).Scan(&checked, &next, &c.state, &c.reason)
	if errors.Is(err, sql.ErrNoRows) {
		return titleCheck{}, nil
	}
	if err != nil {
		return titleCheck{}, err
	}
	c.checked = time.Unix(checked, 0)
	if next > 0 {
		c.next = time.Unix(next, 0)
	}
	return c, nil
}

func (s *Service) saveTitleCheckDue(ctx context.Context, key core.RatingKey, state, reason string, nextAt int64, priority float64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO availability_title_checks(rating_key,checked_at,next_check,state,reason,priority) VALUES(?,?,?,?,?,?)
 ON CONFLICT(rating_key) DO UPDATE SET checked_at=excluded.checked_at,next_check=excluded.next_check,state=excluded.state,reason=excluded.reason,priority=excluded.priority
 WHERE availability_title_checks.state<>'found'
 AND NOT EXISTS (SELECT 1 FROM availability_gate WHERE rating_key=excluded.rating_key)`,
		int64(key), s.now().Unix(), nextAt, state, reason, priority)
	return err
}

func catalogHomeReleaseAdmission(earliest time.Time, offsetDays int) time.Time {
	if earliest.IsZero() || offsetDays == 0 {
		return earliest
	}
	return earliest.Add(time.Duration(offsetDays) * 24 * time.Hour)
}

func (s *Service) acceptDigitalReleases(ctx context.Context, keys []core.RatingKey, now time.Time, offsetDays int, acceptable map[core.RatingKey]bool) {
	h, ok := s.d.Catalog.(interface {
		HomeReleases(context.Context, []core.RatingKey) (map[core.RatingKey]time.Time, error)
	})
	if !ok {
		return
	}
	got, err := h.HomeReleases(ctx, keys)
	if err != nil {
		return
	}
	for k, at := range got {
		if !at.IsZero() && !catalogHomeReleaseAdmission(at, offsetDays).After(now) {
			acceptable[k] = true
		}
	}
}

// WatchlistEvidence shares the release monitor's first look across viewers and
// watchlist providers. Later watchlist passes read the gate locally.
func (s *Service) WatchlistEvidence(ctx context.Context, title *core.Title) (ready, handled bool, err error) {
	cfg := s.cfg().Availability
	kind := hiddenMovies
	if title.Type == core.TypeEpisode {
		kind = hiddenShows
	}
	if !cfg.Monitor || cfg.ModeFor(kind) != config.ModePlayable || s.d.Search == nil || !s.hasGateTable {
		return false, false, nil
	}
	if s.isGateOpen(ctx, s.currentGatePolicy(), title.Key) {
		return true, true, nil
	}
	now := s.now()
	if !title.IsReleased(now) {
		return false, true, nil
	}
	anchor := cmp.Or(title.EpisodeAirDate, title.Released)
	res, err := s.db.ExecContext(ctx, `INSERT INTO availability_title_checks(rating_key,kind,next_check,anchor_ts,priority)
 SELECT ?,?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM availability_gate WHERE rating_key=?)
 ON CONFLICT(rating_key) DO NOTHING`, int64(title.Key), string(title.Type), max(now.Unix(), anchor.Unix()),
		anchor.Unix(), firstLookPriorityBoost+1000+title.Popularity, int64(title.Key))
	if err != nil {
		return false, true, err
	}
	if inserted, err := res.RowsAffected(); err == nil && inserted > 0 {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	return false, true, nil
}
