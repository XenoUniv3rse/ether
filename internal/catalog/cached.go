package catalog

import (
	"context"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"time"
)

// CachedTitle returns a mirrored title without contacting TMDB.
func (c *Catalog) CachedTitle(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	key, err := c.canonicalSourceKey(ctx, key)
	if err != nil {
		return nil, err
	}
	st, err := c.loadByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if err := c.projectSingle(ctx, &st.t); err != nil {
		return nil, err
	}
	return &st.t, nil
}

// RecentEpisodes returns only local, released episode identities for artwork
// badges. Callers apply viewer state through core.State instead of a SQL join.
func (c *Catalog) RecentEpisodes(ctx context.Context, show core.RatingKey, since time.Time) ([]core.Title, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT key,episode FROM catalog_titles WHERE show_key=? AND type='episode' AND release_known=1 AND adult=0 AND released_ts BETWEEN ? AND ?`, show, since.Unix(), c.now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Title{}
	for rows.Next() {
		var t core.Title
		t.Type = core.TypeEpisode
		if err := rows.Scan(&t.Key, &t.Episode); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RecentEpisodesMany batches RecentEpisodes across shows so page-level status
// overlays read catalog_titles_episode_aired once per chunk instead of per tile.
func (c *Catalog) RecentEpisodesMany(ctx context.Context, shows []core.RatingKey, since time.Time) (map[core.RatingKey][]core.Title, error) {
	out := make(map[core.RatingKey][]core.Title, len(shows))
	if len(shows) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(shows))
	seen := make(map[core.RatingKey]bool, len(shows))
	for _, k := range shows {
		if k.Kind() == core.KindShow && !seen[k] {
			seen[k] = true
			args = append(args, int64(k))
		}
	}
	if len(args) == 0 {
		return out, nil
	}
	sinceTS := since.Unix()
	nowTS := c.now().Unix()
	const batch = 200
	for chunk := range store.Chunks(args, batch) {
		qArgs := make([]any, 0, 2+len(chunk))
		qArgs = append(qArgs, sinceTS, nowTS)
		qArgs = append(qArgs, chunk...)
		rows, err := c.db.QueryContext(ctx, `SELECT show_key,key,episode FROM catalog_titles WHERE type='episode' AND release_known=1 AND adult=0 AND released_ts BETWEEN ? AND ? AND show_key IN (`+placeholders(len(chunk))+`)`, qArgs...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var showKey int64
			var t core.Title
			t.Type = core.TypeEpisode
			if err := rows.Scan(&showKey, &t.Key, &t.Episode); err != nil {
				rows.Close()
				return nil, err
			}
			sk := core.RatingKey(showKey)
			t.ShowKey = sk
			out[sk] = append(out[sk], t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CachedTitles returns mirrored titles in requested order without refreshing.
// Episode and season lookups also include upcoming rows. Batch consumers such as recommendation
// builders use this to avoid one full hydration round trip per history entry.
func (c *Catalog) CachedTitles(ctx context.Context, keys []core.RatingKey) ([]core.Title, error) {
	return c.titlesByKeysMode(ctx, keys, true)
}

// ShowTrajectories computes season/episode rating trajectories for a batch of
// shows from local mirror data without contacting TMDB.
func (c *Catalog) ShowTrajectories(ctx context.Context, shows []core.RatingKey) (map[core.RatingKey]core.ShowTrajectory, error) {
	out := make(map[core.RatingKey]core.ShowTrajectory, len(shows))
	if len(shows) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(shows))
	seen := map[core.RatingKey]bool{}
	for _, k := range shows {
		if k.Kind() == core.KindShow && !seen[k] {
			seen[k] = true
			args = append(args, int64(k))
		}
	}
	if len(args) == 0 {
		return out, nil
	}
	nowTS := c.now().Unix()
	statusByShow := map[core.RatingKey]string{}
	showRating := map[core.RatingKey]float64{}
	type sPoint struct {
		season int
		rating float64
	}
	byShow := map[core.RatingKey][]sPoint{}
	// Chunk the deduplicated show args (same 200 as hydrateSlice). Each
	// show lands in exactly one chunk, so per-show points stay ordered
	// and are never merged across queries.
	const batch = 200
	for chunk := range store.Chunks(args, batch) {
		part := chunk
		ph := placeholders(len(part))
		srows, err := c.db.QueryContext(ctx, `SELECT key, status, rating FROM catalog_titles WHERE key IN (`+ph+`)`, part...)
		if err != nil {
			return nil, err
		}
		for srows.Next() {
			var k int64
			var st string
			var r float64
			if err := srows.Scan(&k, &st, &r); err != nil {
				srows.Close()
				return nil, err
			}
			statusByShow[core.RatingKey(k)] = st
			showRating[core.RatingKey(k)] = r
		}
		if err := srows.Err(); err != nil {
			srows.Close()
			return nil, err
		}
		srows.Close()

		rows, err := c.db.QueryContext(ctx, `
			SELECT show_key, season, rating FROM catalog_titles
			WHERE type='season' AND season>0 AND rating>0 AND release_known=1 AND released_ts<=? AND show_key IN (`+ph+`)
			ORDER BY show_key ASC, season ASC`, append([]any{nowTS}, part...)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var sk int64
			var sea int
			var r float64
			if err := rows.Scan(&sk, &sea, &r); err != nil {
				rows.Close()
				return nil, err
			}
			byShow[core.RatingKey(sk)] = append(byShow[core.RatingKey(sk)], sPoint{season: sea, rating: r})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()

		// Fallback/augmentation from mirrored episode ratings when season ratings are 0.
		erows, err := c.db.QueryContext(ctx, `
			SELECT show_key, season, AVG(rating) FROM catalog_titles
			WHERE type='episode' AND season>0 AND rating>0 AND release_known=1 AND released_ts<=? AND show_key IN (`+ph+`)
			GROUP BY show_key, season
			ORDER BY show_key ASC, season ASC`, append([]any{nowTS}, part...)...)
		if err == nil {
			epByShow := map[core.RatingKey][]sPoint{}
			scanDiscarded := 0
			for erows.Next() {
				var sk int64
				var sea int
				var r float64
				if err := erows.Scan(&sk, &sea, &r); err != nil {
					scanDiscarded++
					continue
				}
				epByShow[core.RatingKey(sk)] = append(epByShow[core.RatingKey(sk)], sPoint{season: sea, rating: r})
			}
			if err := erows.Err(); err != nil {
				// Episode augmentation is optional: keep the season
				// trajectories already loaded and discard the partial
				// episode points rather than failing the whole batch.
				c.log.Warn("catalog: episode trajectories not fully loaded", "err", err)
			} else if scanDiscarded > 0 {
				// A scan error means some episode points were dropped:
				// discard the partial episode points (like an iteration
				// error) rather than merging an incomplete set.
				c.log.Warn("catalog: episode trajectories not fully loaded", "dropped", scanDiscarded)
			} else {
				for k, pts := range epByShow {
					if len(byShow[k]) < len(pts) {
						byShow[k] = pts
					}
				}
			}
			erows.Close()
		} else {
			c.log.Warn("catalog: episode trajectories not fully loaded", "err", err)
		}
	}

	for k, pts := range byShow {
		if len(pts) == 0 {
			continue
		}
		early := pts[0].rating
		finale := pts[len(pts)-1].rating
		peak := 0.0
		for _, p := range pts {
			if p.rating > peak {
				peak = p.rating
			}
		}
		base := early
		if sr := showRating[k]; sr > 0 && sr < base {
			base = sr
		}
		ended := statusByShow[k] == "Ended" || statusByShow[k] == "Canceled"
		// A show "sticks the landing" when it ended (not canceled on a cliffhanger
		// with a collapsing finale) and its final season holds or beats its early bar.
		sticks := statusByShow[k] == "Ended" && finale >= 7.5 && finale >= base-0.35
		if !ended && len(pts) >= 2 && finale >= 8.0 && finale >= early {
			// Returning multi-season series that is getting better each season.
			sticks = false
		}
		out[k] = core.ShowTrajectory{
			ShowKey:       k,
			EarlyRating:   early,
			FinaleRating:  finale,
			PeakRating:    peak,
			RatedSeasons:  len(pts),
			SticksLanding: sticks,
		}
	}
	return out, nil
}

// StandaloneEpisodes returns acclaimed mirrored episodes from anthology series
// or top-rated standalone chapters that can be watched without a multi-season
// commitment.
func (c *Catalog) StandaloneEpisodes(ctx context.Context, minRating float64, limit int) ([]core.Title, error) {
	if minRating <= 0 {
		minRating = 8.0
	}
	if limit <= 0 {
		limit = 40
	}
	nowTS := c.now().Unix()
	q := `SELECT ` + prefixedCols("e") + ` FROM catalog_titles e
		JOIN catalog_titles s ON s.key = e.show_key
		WHERE e.type='episode' AND e.season>0 AND e.adult=0
		  AND e.release_known=1 AND e.released_ts<=?
		  AND e.rating>=?
		  AND s.browsable=1 AND s.adult=0
		  AND (
		    EXISTS (SELECT 1 FROM catalog_keywords kw WHERE kw.key=s.key AND LOWER(kw.keyword) IN ('anthology','anthology series','stand-alone','standalone','short stories','episodic'))
		    OR (e.season=1 AND e.episode=1 AND e.rating>=8.3)
		    OR (e.rating>=8.8 AND e.vote_count>=50)
		  )
		ORDER BY e.rating DESC, s.popularity DESC, e.key ASC
		LIMIT ?`
	return c.queryTitles(ctx, q, nowTS, minRating, limit)
}

// RecentMovies reads the release-date index, without refreshing any title.
func (c *Catalog) RecentMovies(ctx context.Context, since time.Time) ([]core.Title, error) {
	return c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='movie' AND browsable=1 AND released_ts>? AND released_ts<=? ORDER BY key`, since.Unix(), c.now().Unix())
}

// RecentShows only reads the local premiere-date index.
func (c *Catalog) RecentShows(ctx context.Context, since time.Time) ([]core.Title, error) {
	return c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='show' AND browsable=1 AND released_ts>? AND released_ts<=? ORDER BY key`, since.Unix(), c.now().Unix())
}

// RecentEpisodes is the bounded evidence window used before episode paging.
func (c *Catalog) RecentEvidenceEpisodes(ctx context.Context, since time.Time) ([]core.Title, error) {
	return c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='episode' AND adult=0 AND release_known=1 AND released_ts>? AND released_ts<=? ORDER BY released_ts, key`, since.Unix(), c.now().Unix())
}

// CachedDiscoveryEpisodes returns regular episode metadata without hydration or
// remote lookups. Unknown/future dates are retained so callers can distinguish
// an unmirrored premiere from a known but unreleased episode.
func (c *Catalog) CachedDiscoveryEpisodes(ctx context.Context, shows []core.RatingKey) (map[core.RatingKey][]core.Title, error) {
	out := map[core.RatingKey][]core.Title{}
	for start := 0; start < len(shows); start += 500 {
		batch := shows[start:min(start+500, len(shows))]
		args := make([]any, len(batch))
		for i, k := range batch {
			args[i] = k
		}
		rows, err := c.db.QueryContext(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='episode' AND season>0 AND show_key IN (`+placeholders(len(batch))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			st, err := scanTitle(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[st.t.ShowKey] = append(out[st.t.ShowKey], st.t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CachedGetMany applies CachedGet's visibility gates to bounded local reads.
// Missing or hidden titles are omitted. It never refreshes or admits fallbacks.
func (c *Catalog) cachedListVisible(t *core.Title) bool {
	return !t.Adult && (c.upcomingMovie(t) || (selectedEpisodeReleased(t, c.now()) && (!(c.config().ReleaseMode == config.ModeDigital) || c.homeVisible(t)))) && (t.Type != core.TypeSeason || listableSeason(t, c.now()))
}

func (c *Catalog) cachedStoredEntry(key core.RatingKey, gen uint64, minute int64) (storedTitle, bool) {
	if !cacheStoredTitle(key.Kind()) {
		return storedTitle{}, false
	}
	c.cacheMu.RLock()
	ent, ok := c.titleCache[key]
	c.cacheMu.RUnlock()
	if !ok || ent.gen != gen || ent.minute != minute {
		return storedTitle{}, false
	}
	cp := ent.st
	cp.t = cloneTitle(ent.st.t)
	return cp, true
}

func (c *Catalog) cachedStored(key core.RatingKey, gen uint64, minute int64) (core.Title, bool) {
	st, ok := c.cachedStoredEntry(key, gen, minute)
	if !ok {
		return core.Title{}, false
	}
	return st.t, true
}

func (c *Catalog) rememberTitles(gen uint64, minute int64, items []core.Title) {
	if c.writeGen.Load() != gen {
		return
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if c.titleCache == nil {
		c.titleCache = map[core.RatingKey]cachedStoredTitle{}
	}
	for i := range items {
		c.putSnapshotLocked(items[i].Key, gen, minute, storedTitle{t: cloneTitle(items[i])})
	}
}

func (c *Catalog) rememberStored(gen uint64, minute int64, items []storedTitle) {
	if c.writeGen.Load() != gen || len(items) == 0 {
		return
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if c.titleCache == nil {
		c.titleCache = map[core.RatingKey]cachedStoredTitle{}
	}
	for i := range items {
		cp := items[i]
		cp.t = cloneTitle(items[i].t)
		c.putSnapshotLocked(cp.t.Key, gen, minute, cp)
	}
}

const titleCacheMax = 8192

// putSnapshotLocked records the first title seen for this generation and
// minute. The caller holds cacheMu. A later hydrate must not replace it:
// a background browse can read a credit row another writer just changed.
func (c *Catalog) putSnapshotLocked(key core.RatingKey, gen uint64, minute int64, st storedTitle) {
	if !cacheStoredTitle(key.Kind()) {
		return
	}
	if c.titleCache == nil {
		c.titleCache = map[core.RatingKey]cachedStoredTitle{}
	}
	if ent, ok := c.titleCache[key]; ok && ent.gen == gen && ent.minute == minute {
		if !ent.st.metaKnown && st.metaKnown {
			ent.st.browsable = st.browsable
			ent.st.fetchedAt = st.fetchedAt
			ent.st.detail = st.detail
			ent.st.detailAt = st.detailAt
			ent.st.metaKnown = true
			c.titleCache[key] = ent
		}
		return
	}
	if len(c.titleCache) >= titleCacheMax {
		// Drop stale generations/minutes first; if the current snapshot alone
		// fills the bound, retain the newer half so a PreloadStored or list
		// hydration crossing titleCacheMax never wipes entries just preloaded
		// for the active request.
		live := 0
		for _, ent := range c.titleCache {
			if ent.gen == gen && ent.minute == minute {
				live++
			}
		}
		var cutoff uint64
		if live >= titleCacheMax && c.titleSeq > titleCacheMax/2 {
			cutoff = c.titleSeq - titleCacheMax/2
		}
		keep := make(map[core.RatingKey]cachedStoredTitle, min(live+256, titleCacheMax))
		for k, ent := range c.titleCache {
			if ent.gen == gen && ent.minute == minute && ent.seq > cutoff {
				keep[k] = ent
			}
		}
		c.titleCache = keep
	}
	c.titleSeq++
	c.titleCache[key] = cachedStoredTitle{st: st, gen: gen, minute: minute, seq: c.titleSeq}
}

func (c *Catalog) CachedGetMany(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
	out, err := c.cachedGetMany(ctx, keys, true)
	if err != nil {
		return nil, err
	}
	items := make([]core.Title, 0, len(out))
	for _, item := range out {
		items = append(items, item)
	}
	if err := c.ProjectEpisodeTitles(ctx, items); err != nil {
		return nil, err
	}
	for _, item := range items {
		if c.cachedListVisible(&item) {
			out[item.Key] = item
		} else {
			delete(out, item.Key)
		}
	}
	return out, nil
}

// cachedGetMany reads local titles. reuse serves the minute snapshot, which a
// foreign credit write does not invalidate. People credit proofs pass reuse
// false so they hydrate the current credit rows and leave that snapshot alone.
func (c *Catalog) cachedGetMany(ctx context.Context, keys []core.RatingKey, reuse bool) (map[core.RatingKey]core.Title, error) {
	out := make(map[core.RatingKey]core.Title, len(keys))
	projected := reuse && c.canProjectAlternateOrders()
	gen := c.writeGen.Load()
	minute := c.minuteBucket()
	missing := make([]core.RatingKey, 0, len(keys))
	for _, key := range keys {
		if reuse {
			if title, ok := c.cachedStored(key, gen, minute); ok {
				if c.cachedListVisible(&title) || projected && (title.Type == core.TypeEpisode || title.Type == core.TypeSeason) {
					out[key] = title
				}
				continue
			}
		}
		missing = append(missing, key)
	}
	if len(missing) > 0 {
		part := missing
		args := make([]any, len(part))
		for i, key := range part {
			args[i] = int64(key)
		}
		ph, bound := c.hydrationKeys(args)
		rows, err := c.db.QueryContext(ctx, `SELECT `+titleCols+`,detail_at FROM catalog_titles WHERE key IN (`+ph+`)`, bound...)
		if err != nil {
			return nil, err
		}
		var stored []*storedTitle
		var items []core.Title
		for rows.Next() {
			var detailAt int64
			st, e := scanTitle(extraScan{rows, []any{&detailAt}})
			if e != nil {
				rows.Close()
				return nil, e
			}
			if detailAt != 0 {
				st.detailAt = time.Unix(detailAt, 0).UTC()
			}
			st.metaKnown = true
			stored = append(stored, st)
			items = append(items, st.t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if err := c.hydrateSlice(ctx, items); err != nil {
			return nil, err
		}
		visible := make([]storedTitle, 0, len(items))
		for i := range items {
			if c.cachedListVisible(&items[i]) || projected && (items[i].Type == core.TypeEpisode || items[i].Type == core.TypeSeason) {
				out[items[i].Key] = items[i]
				st := *stored[i]
				st.t = items[i]
				visible = append(visible, st)
			}
		}
		if reuse {
			c.rememberStored(gen, minute, visible)
		}
	}
	return out, nil
}

// ResolveShowParents batches fresh mirrored shows and falls back to Get for
// stale, missing, or non-show keys. Individual Get failures are omitted, as
// inherited visibility treats them as inaccessible.
func (c *Catalog) ResolveShowParents(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
	out := make(map[core.RatingKey]core.Title, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	gen := c.writeGen.Load()
	minute := c.minuteBucket()
	fallback := func(part []core.RatingKey) {
		for _, key := range part {
			title, err := c.Get(ctx, key)
			if err == nil && title != nil && title.Key == key {
				out[key] = *title
			}
		}
	}
	localShow := func(key core.RatingKey, st *storedTitle) bool {
		return key.Kind() == core.KindShow && st != nil && (c.fresh(st) || !c.TMDBConfigured() || (noReadWait(ctx) && !core.ColdDetail(ctx, key)))
	}
	var local []core.Title
	missing := make([]core.RatingKey, 0, len(keys))
	seen := make(map[core.RatingKey]bool, len(keys))
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		if st, ok := c.cachedStoredEntry(key, gen, minute); ok && st.metaKnown {
			if localShow(key, &st) {
				if !c.fresh(&st) && c.TMDBConfigured() && !key.Allocated() && st.t.IDs.TMDB > 0 {
					c.loadSeasonsLater(ctx, st.t.IDs.TMDB, nil)
					c.reportDetailPending(ctx, key)
				}
				if c.visibleTitle(&st.t) {
					local = append(local, st.t)
				}
				continue
			}
		}
		missing = append(missing, key)
	}
	for start := 0; start < len(missing); start += 200 {
		part := missing[start:min(start+200, len(missing))]
		args := make([]any, len(part))
		for i, k := range part {
			args[i] = int64(k)
		}
		rows, err := c.db.QueryContext(ctx, `SELECT `+titleCols+`,detail_at FROM catalog_titles WHERE key IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			fallback(part)
			continue
		}
		stored := map[core.RatingKey]*storedTitle{}
		for rows.Next() {
			var detail int64
			st, e := scanTitle(extraScan{rows, []any{&detail}})
			if e != nil {
				err = e
				break
			}
			if detail != 0 {
				st.detailAt = time.Unix(detail, 0).UTC()
			}
			st.metaKnown = true
			stored[st.t.Key] = st
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			fallback(part)
			continue
		}
		var warm []core.Title
		var warmMeta []*storedTitle
		for _, key := range part {
			st := stored[key]
			if localShow(key, st) {
				if !c.fresh(st) && c.TMDBConfigured() && !key.Allocated() && st.t.IDs.TMDB > 0 {
					c.loadSeasonsLater(ctx, st.t.IDs.TMDB, nil)
					c.reportDetailPending(ctx, key)
				}
				warm = append(warm, st.t)
				warmMeta = append(warmMeta, st)
			}
		}
		if err := c.hydrateSlice(ctx, warm); err != nil {
			fallback(part)
			continue
		}
		remember := make([]storedTitle, 0, len(warm))
		for i := range warm {
			st := *warmMeta[i]
			st.t = warm[i]
			remember = append(remember, st)
			if c.visibleTitle(&warm[i]) {
				local = append(local, warm[i])
			}
		}
		if deferred, _ := ctx.Value(deferAiredCountsKey{}).(bool); !deferred {
			c.rememberStored(gen, minute, remember)
		}
		for _, key := range part {
			st := stored[key]
			if localShow(key, st) {
				continue
			}
			title, e := c.Get(ctx, key)
			if e == nil && title != nil && title.Key == key {
				out[key] = *title
			}
		}
	}
	if err := c.ProjectEpisodeTitles(ctx, local); err != nil {
		return nil, err
	}
	for _, title := range local {
		out[title.Key] = title
	}
	return out, nil
}

// CachedSeasons returns listable mirrored seasons for multiple shows in one
// batched read (season rows + aired counts), without hydrating credits,
// keywords or air-added timestamps. Callers that only inspect season number,
// premiere date and episode counts (e.g. New Seasons) avoid per-show Children calls.
func (c *Catalog) CachedSeasons(ctx context.Context, shows []core.RatingKey) (map[core.RatingKey][]core.Title, error) {
	out := make(map[core.RatingKey][]core.Title, len(shows))
	if len(shows) == 0 {
		return out, nil
	}
	unique := make([]core.RatingKey, 0, len(shows))
	seen := make(map[core.RatingKey]bool, len(shows))
	for _, k := range shows {
		if k.Kind() == core.KindShow && !seen[k] {
			seen[k] = true
			unique = append(unique, k)
		}
	}
	if len(unique) == 0 {
		return out, nil
	}
	var all []core.Title
	for start := 0; start < len(unique); start += 200 {
		part := unique[start:min(start+200, len(unique))]
		args := make([]any, len(part))
		for i, k := range part {
			args[i] = int64(k)
		}
		rows, err := c.db.QueryContext(ctx, `SELECT key,show_key,season,released_ts,child_count,adult FROM catalog_titles`+c.indexedBy("catalog_titles_show")+` WHERE type='season' AND show_key IN (`+placeholders(len(part))+`) ORDER BY show_key ASC, season ASC`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key, showKey, released int64
			var season, child, adult int
			if err := rows.Scan(&key, &showKey, &season, &released, &child, &adult); err != nil {
				rows.Close()
				return nil, err
			}
			if adult != 0 {
				continue
			}
			t := core.Title{
				Key:        core.RatingKey(key),
				Type:       core.TypeSeason,
				ShowKey:    core.RatingKey(showKey),
				Season:     season,
				ChildCount: child,
			}
			if released != 0 {
				t.Released = time.Unix(released, 0).UTC()
			}
			all = append(all, t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	if len(all) == 0 {
		return out, nil
	}
	if err := c.airedCounts(ctx, all); err != nil {
		return nil, err
	}
	now := c.now()
	for _, s := range all {
		if listableSeason(&s, now) {
			out[s.ShowKey] = append(out[s.ShowKey], s)
		}
	}
	return out, nil
}

// DigitalEvidenceMovies excludes settled home releases at the database boundary.
// Unknown and future home dates remain gated, regardless of cinema age.
func (c *Catalog) DigitalEvidenceMovies(ctx context.Context, since time.Time) ([]core.Title, error) {
	return c.queryTitlesUnfiltered(ctx, `SELECT `+prefixedCols("t")+` FROM catalog_titles t WHERE t.type='movie' AND t.browsable=1 AND t.release_known=1 AND NOT EXISTS (SELECT 1 FROM catalog_home_release h WHERE h.key=t.key AND h.checked=1 AND h.earliest_ts<>0 AND h.earliest_ts<=?) ORDER BY t.key`, since.Unix())
}

// PlaylistTitleRows reads membership, policy and duration facts from the local
// mirror. Playlist consumers hydrate display metadata only for their page.
func (c *Catalog) PlaylistTitleRows(ctx context.Context, keys []core.RatingKey, facts core.TitleFacts) (map[core.RatingKey]core.Title, error) {
	out := make(map[core.RatingKey]core.Title, len(keys))
	keys = uniqueRatingKeys(keys)
	if len(keys) == 0 {
		return out, nil
	}
	facts |= c.viewerPolicyFacts(ctx)
	if core.CatalogViewerRestricted(ctx) {
		facts |= core.FactKeywords
	}
	args := make([]any, len(keys))
	episodeAir := false
	for i, key := range keys {
		args[i] = int64(key)
		episodeAir = episodeAir || key.Kind() == core.KindEpisode
	}
	// Fold the policy facts into each membership row. Full display facts
	// are still loaded only for the page Plex sends.
	var items []core.Title
	for part := range store.Chunks(args, 400) {
		rows, err := c.readVisibilityRowFacts(ctx, part, facts, episodeAir)
		if err != nil {
			return nil, err
		}
		items = append(items, rows...)
	}
	var seasons []core.Title
	for _, item := range items {
		if item.Type == core.TypeSeason {
			seasons = append(seasons, item)
		}
	}
	if len(seasons) > 0 {
		if err := c.airedCounts(ctx, seasons); err != nil {
			return nil, err
		}
		i := 0
		for n := range items {
			if items[n].Type == core.TypeSeason {
				items[n] = seasons[i]
				i++
			}
		}
	}
	if err := c.hydrateShows(ctx, items); err != nil {
		return nil, err
	}
	if err := c.ProjectEpisodeTitles(ctx, items); err != nil {
		return nil, err
	}
	for _, item := range items {
		if c.visibleTitle(&item) {
			out[item.Key] = item
		}
	}
	return out, nil
}
