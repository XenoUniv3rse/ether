package catalog

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// pageCursorTTL bounds how long a key-order page end is remembered for the
// next sequential page. A client walking a whole section (Tracearr) pages
// for far longer than one cache minute, and background catalogue writes
// bump the generation throughout; a cursor that dies with either stamp
// sends every later page back to an OFFSET scan over all prior rows.
const pageCursorTTL = 10 * time.Minute

func pageCursorLive(cur cachedBrowse, gen uint64, minute int64) bool {
	if len(cur.page.Items) != 1 {
		return false
	}
	if cur.gen == gen && cur.minute == minute {
		return true
	}
	return !cur.at.IsZero() && time.Since(cur.at) < pageCursorTTL
}

// Key bounds for show, season and episode rating keys.
var (
	showKeyStart    = int64(core.KindShow)*1_000_000_000_000_000 - 1
	showKeyEnd      = int64(core.KindShow+1) * 1_000_000_000_000_000
	episodeKeyStart = int64(core.KindEpisode)*1_000_000_000_000_000 - 1
	episodeKeyEnd   = int64(core.KindEpisode+1) * 1_000_000_000_000_000
	seasonKeyStart  = int64(core.KindSeason)*1_000_000_000_000_000 - 1
	seasonKeyEnd    = int64(core.KindSeason+1) * 1_000_000_000_000_000
)

// Stored episode key pages include the allocated range.
func (c *Catalog) storedEpisodeKeyEnd() int64 {
	if c.canProjectAlternateOrders() {
		return core.AllocatedEpisodeBase + core.AllocatedKeyMaxSeq
	}
	return episodeKeyEnd
}

func (c *Catalog) storedChildCursorShow(ctx context.Context, after core.RatingKey) (int64, error) {
	if after == 0 {
		return showKeyStart + 1, nil
	}
	if !after.Allocated() {
		return int64(core.ShowKey(after.TMDB())), nil
	}
	parents, err := c.ChildAncestry(ctx, []core.RatingKey{after})
	if err != nil {
		return 0, err
	}
	if lineage := parents[after]; lineage.ShowKey != 0 {
		return int64(lineage.ShowKey), nil
	}
	return showKeyStart + 1, nil
}

// storedShowScoped reports whether f narrows the admitted series by library,
// tab or preview rules, a catalogue source or a collection. Scoped key-order
// episode and season reads drive from the admitted series in key order rather
// than probing every stored episode in the catalogue.
func storedShowScoped(f core.Filter) bool {
	return len(f.ExtraRules) > 0 || (f.ExtraFallback != "" && f.ExtraFallback != "show") ||
		len(f.TabRules) > 0 || (f.TabFallback != "" && f.TabFallback != "show") ||
		len(f.PreviewRules) > 0 || (f.PreviewFallback != "" && f.PreviewFallback != "show") ||
		(f.RuleVisibility != "" && f.RuleVisibility != "all") || f.ApplyCanonicalInPreview ||
		f.CollectionMembers != 0 || hasRestrictedCatalogueSources(f.CatalogueSources)
}

func storedBrowseUncacheable(f core.Filter) bool {
	return filterHasViewerState(f) || f.CollectionMembers != 0
}

// splitStoredFilter separates a child browse filter (episode or season) into
// the parent-series filter (t) and any child-level conditions.
func splitStoredFilter(f core.Filter, childKind core.KeyKind) (show core.Filter, childExclude []core.RatingKey) {
	show = core.Filter{
		Topic:                   f.Topic,
		LocalOnly:               f.LocalOnly,
		CatalogCandidates:       f.CatalogCandidates,
		CatalogViewerID:         f.CatalogViewerID,
		CatalogRuleRevision:     f.CatalogRuleRevision,
		KnownAddedDate:          f.KnownAddedDate,
		GatePolicy:              f.GatePolicy,
		ShowGateCutoff:          f.ShowGateCutoff,
		EpisodeGateCutoff:       f.EpisodeGateCutoff,
		GateNow:                 f.GateNow,
		GateRev:                 f.GateRev,
		GateSeedCutoff:          f.GateSeedCutoff,
		HiddenMovieSeq:          f.HiddenMovieSeq,
		HiddenShowSeq:           f.HiddenShowSeq,
		RuleVisibility:          f.RuleVisibility,
		ApplyCanonicalInPreview: f.ApplyCanonicalInPreview,
		PreviewRules:            f.PreviewRules,
		PreviewFallback:         f.PreviewFallback,
		ExtraRules:              f.ExtraRules,
		ExtraFallback:           f.ExtraFallback,
		TabRules:                f.TabRules,
		TabFallback:             f.TabFallback,
		CollectionMembers:       f.CollectionMembers,
		CatalogueSources:        f.CatalogueSources,
		ExcludeGenres:           f.ExcludeGenres,
		ExcludeKeywords:         f.ExcludeKeywords,
		Type:                    core.TypeShow,
		Genres:                  f.Genres,
		Anime:                   f.Anime,
		Kids:                    f.Kids,
		Tags:                    f.Tags,
		Keywords:                f.Keywords,
		Languages:               f.Languages,
		Countries:               f.Countries,
		ExcludeCountries:        f.ExcludeCountries,
		ContentRatings:          f.ContentRatings,
		ExcludeContentRatings:   f.ExcludeContentRatings,
		MaxContentRating:        f.MaxContentRating,
		Collection:              f.Collection,
		Collections:             f.Collections,
		ExcludeCollections:      f.ExcludeCollections,
		MinContentRating:        f.MinContentRating,
		IncludeCollectionNames:  f.IncludeCollectionNames,
		ExcludeCollectionNames:  f.ExcludeCollectionNames,
		SaferPlexRating:         f.SaferPlexRating,
		Person:                  f.Person,
		Credits:                 f.Credits,
		Studio:                  f.Studio,
		StudioFilters:           f.StudioFilters,
	}
	for _, k := range f.ExcludeKeys {
		switch k.Kind() {
		case childKind:
			childExclude = append(childExclude, k)
		default:
			show.ExcludeKeys = append(show.ExcludeKeys, k)
		}
	}
	return show, childExclude
}

func (c *Catalog) childFilterConds(alias string, f core.Filter, childExclude []core.RatingKey) ([]string, []any) {
	var conds []string
	var args []any
	if alias == "e" {
		if ec, ea := episodeGateSQL(alias, f, c.now().Unix()); ec != "" {
			conds = append(conds, ec)
			args = append(args, ea...)
		}
	}
	if alias == "e" && f.HiddenEpisodeSeq != 0 {
		clause := "NOT EXISTS (SELECT 1 FROM availability_hidden ah WHERE ah.kind='episode' AND ah.from_seq<=? AND ah.to_seq>? AND ah.rating_key=" + alias + ".key)"
		args = append(args, f.HiddenEpisodeSeq, f.HiddenEpisodeSeq)
		if len(f.AvailabilityKeepKeys) > 0 {
			clause = "(" + clause + " OR " + alias + ".key IN (" + placeholders(len(f.AvailabilityKeepKeys)) + "))"
			for _, key := range f.AvailabilityKeepKeys {
				args = append(args, key)
			}
		}
		conds = append(conds, clause)
	}
	if len(childExclude) > 0 {
		conds = append(conds, alias+".key NOT IN ("+placeholders(len(childExclude))+")")
		for _, k := range childExclude {
			args = append(args, k)
		}
	}
	if f.UpdatedAfter != 0 {
		conds = append(conds, alias+".fetched_at>=?")
		args = append(args, f.UpdatedAfter)
	}
	if f.UpdatedBefore != 0 {
		conds = append(conds, alias+".fetched_at<=? AND "+alias+".fetched_at>0")
		args = append(args, f.UpdatedBefore)
	}
	if f.AddedAfter != 0 {
		conds = append(conds, alias+".release_known=1 AND "+alias+".released_ts>=?")
		args = append(args, f.AddedAfter)
	}
	if f.AddedBefore != 0 {
		conds = append(conds, alias+".release_known=1 AND "+alias+".released_ts<=?")
		args = append(args, f.AddedBefore)
	}
	if f.YearFrom > 0 {
		conds = append(conds, alias+".year>=?")
		args = append(args, f.YearFrom)
	}
	if f.YearTo > 0 {
		conds = append(conds, alias+".year<=? AND "+alias+".year>0")
		args = append(args, f.YearTo)
	}
	if f.MinRating > 0 {
		conds = append(conds, alias+".rating>=?")
		args = append(args, f.MinRating)
	}
	if f.MaxRuntime > 0 {
		conds = append(conds, alias+".runtime_sec>0 AND "+alias+".runtime_sec<=?")
		args = append(args, int64(f.MaxRuntime.Seconds()))
	}
	if f.MinRuntime > 0 {
		conds = append(conds, alias+".runtime_sec>=?")
		args = append(args, int64(f.MinRuntime.Seconds()))
	}
	if cond, condArgs := c.childSortNameCond(alias, sortNameRangeOf(f)); cond != "" {
		conds = append(conds, cond)
		args = append(args, condArgs...)
	}
	if len(f.Years) > 0 {
		conds = append(conds, alias+".year IN ("+placeholders(len(f.Years))+")")
		for _, y := range f.Years {
			args = append(args, y)
		}
	}
	if len(f.ExcludeYears) > 0 {
		conds = append(conds, alias+".year>0 AND "+alias+".year NOT IN ("+placeholders(len(f.ExcludeYears))+")")
		for _, y := range f.ExcludeYears {
			args = append(args, y)
		}
	}
	if f.MaxRating > 0 {
		conds = append(conds, alias+".rating>0 AND "+alias+".rating<=?")
		args = append(args, f.MaxRating)
	}
	if len(f.ExcludeRatings) > 0 {
		conds = append(conds, alias+".rating>0 AND "+alias+".rating NOT IN ("+placeholders(len(f.ExcludeRatings))+")")
		for _, r := range f.ExcludeRatings {
			args = append(args, r)
		}
	}
	if len(f.ExcludeRuntimes) > 0 {
		conds = append(conds, alias+".runtime_sec>0 AND "+alias+".runtime_sec NOT IN ("+placeholders(len(f.ExcludeRuntimes))+")")
		for _, d := range f.ExcludeRuntimes {
			args = append(args, int64(d.Seconds()))
		}
	}
	if f.Query != "" {
		match, matchArgs := c.titleMatch(alias+".", f.Query)
		conds = append(conds, match)
		args = append(args, matchArgs...)
	}
	for _, tf := range f.TitleFilters {
		val := strings.ToLower(tf.Value)
		switch tf.Op {
		case "==":
			conds = append(conds, "LOWER("+alias+".title)=?")
			args = append(args, val)
		case "!==":
			conds = append(conds, "LOWER("+alias+".title)<>?")
			args = append(args, val)
		case "!=":
			conds = append(conds, "LOWER("+alias+".title) NOT LIKE ? ESCAPE '\\'")
			args = append(args, "%"+escapeLike(val)+"%")
		case "<=":
			conds = append(conds, "LOWER("+alias+".title) LIKE ? ESCAPE '\\'")
			args = append(args, escapeLike(val)+"%")
		case ">=":
			conds = append(conds, "LOWER("+alias+".title) LIKE ? ESCAPE '\\'")
			args = append(args, "%"+escapeLike(val))
		default:
			match, matchArgs := c.titleMatch(alias+".", tf.Value)
			conds = append(conds, match)
			args = append(args, matchArgs...)
		}
	}
	for _, rf := range f.ReleasedFilters {
		const day = 86400
		switch rf.Op {
		case "<<":
			conds = append(conds, alias+".released_ts<?")
			args = append(args, rf.Unix)
		case "<=":
			end := rf.Unix
			if rf.SpanDay {
				end += day - 1
			}
			conds = append(conds, alias+".released_ts<=?")
			args = append(args, end)
		case ">>":
			conds = append(conds, alias+".released_ts>?")
			args = append(args, rf.Unix)
		case ">=":
			conds = append(conds, alias+".released_ts>=?")
			args = append(args, rf.Unix)
		case "!=", "!==":
			if rf.SpanDay {
				conds = append(conds, "("+alias+".released_ts<? OR "+alias+".released_ts>=?)")
				args = append(args, rf.Unix, rf.Unix+day)
			} else {
				conds = append(conds, alias+".released_ts<>?")
				args = append(args, rf.Unix)
			}
		default:
			if rf.SpanDay {
				conds = append(conds, alias+".released_ts>=? AND "+alias+".released_ts<?")
				args = append(args, rf.Unix, rf.Unix+day)
			} else {
				conds = append(conds, alias+".released_ts=?")
				args = append(args, rf.Unix)
			}
		}
	}
	return conds, args
}

// storedEpisodesWhere selects stored, aired, non-adult episodes (e) whose
// series (t) passes the browse filter f.
func (c *Catalog) storedEpisodesWhere(f core.Filter) (string, []any, error) {
	show, childExclude := splitStoredFilter(f, core.KindEpisode)
	where, args, err := c.browseWhere(show)
	if err != nil {
		return "", nil, err
	}
	conds := []string{
		"e.type='episode'",
		"e.adult=0",
		"e.release_known=1",
		"e.released_ts<=?",
	}
	outArgs := []any{c.now().Unix()}
	cConds, cArgs := c.childFilterConds("e", f, childExclude)
	conds = append(conds, cConds...)
	outArgs = append(outArgs, cArgs...)
	visibility, visibilityArgs := c.childVisibilityConds(f.ChildVisibility)
	conds = append(conds, visibility...)
	outArgs = append(outArgs, visibilityArgs...)
	conds = append(conds, where)
	outArgs = append(outArgs, args...)
	if f.Group != nil {
		clause, a, err := c.compileBrowseGroup(f.Group, "e", core.KindEpisode, core.TypeShow)
		if err != nil {
			return "", nil, err
		}
		conds = append(conds, "("+clause+")")
		outArgs = append(outArgs, a...)
	}
	return strings.Join(conds, " AND "), outArgs, nil
}

// storedEpisodesProbeWhere selects stored, aired, non-adult episodes (e) and
// probes each episode's series (t) by primary key in a correlated EXISTS with
// OFFSET 0 so PostgreSQL and SQLite walk an episode index in order without
// hashing or scanning catalog_titles t.
func (c *Catalog) storedEpisodesProbeWhere(f core.Filter) (string, []any, error) {
	show, childExclude := splitStoredFilter(f, core.KindEpisode)
	where, args, err := c.browseWhere(show)
	if err != nil {
		return "", nil, err
	}
	conds := []string{
		"e.type='episode'",
		"e.adult=0",
		"e.release_known=1",
		"e.released_ts<=?",
		"e.show_key<>0",
	}
	outArgs := []any{c.now().Unix()}
	cConds, cArgs := c.childFilterConds("e", f, childExclude)
	conds = append(conds, cConds...)
	outArgs = append(outArgs, cArgs...)
	visibility, visibilityArgs := c.childVisibilityConds(f.ChildVisibility)
	showCond := "t.key=e.show_key AND " + where
	probeArgs := append([]any{}, args...)
	if len(visibility) > 0 {
		showCond += " AND " + strings.Join(visibility, " AND ")
		probeArgs = append(probeArgs, visibilityArgs...)
	}
	if f.Group != nil {
		clause, a, err := c.compileBrowseGroup(f.Group, "e", core.KindEpisode, core.TypeShow)
		if err != nil {
			return "", nil, err
		}
		showCond += " AND (" + clause + ")"
		probeArgs = append(probeArgs, a...)
	}
	conds = append(conds, "EXISTS(SELECT 1 FROM catalog_titles t WHERE "+showCond+" LIMIT 1 OFFSET 0)")
	outArgs = append(outArgs, probeArgs...)
	return strings.Join(conds, " AND "), outArgs, nil
}

// StoredEpisodesByKey returns up to limit stored, aired, non-adult episodes
// after the key after, in key order (series, season, episode), of the
// series the browse filter show admits. It reads the key range and probes
// each episode's series (or drives from admitted series when scoped), so a
// page costs the same however large the library; it refreshes nothing.
func (c *Catalog) StoredEpisodesByKey(ctx context.Context, show core.Filter, after core.RatingKey, limit int) ([]core.Title, error) {
	if err := c.ensureCatalogueSources(ctx, show); err != nil {
		return nil, err
	}
	show = c.viewerPaging(ctx, show)
	lo := max(int64(after), episodeKeyStart)
	if storedShowScoped(show) {
		where, args, err := c.storedEpisodesWhere(show)
		if err != nil {
			return nil, err
		}
		showLo, err := c.storedChildCursorShow(ctx, after)
		if err != nil {
			return nil, err
		}
		q := `SELECT ` + prefixedCols("e") + ` FROM catalog_titles t CROSS JOIN catalog_titles e WHERE t.key>=? AND t.key<? AND e.show_key=t.key AND e.key>? AND e.key<? AND (t.key>? OR e.key>?) AND ` + where + ` ORDER BY t.key ASC, e.key ASC LIMIT ?`
		return c.queryPagedTitles(ctx, q, append(append([]any{showLo, showKeyEnd, episodeKeyStart, c.storedEpisodeKeyEnd(), showLo, lo}, args...), limit)...)
	}
	where, args, err := c.storedEpisodesProbeWhere(show)
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + prefixedCols("e") + ` FROM catalog_titles e WHERE e.key>? AND e.key<? AND ` + where + ` ORDER BY e.key LIMIT ?`
	return c.queryPagedTitles(ctx, q, append(append([]any{lo, c.storedEpisodeKeyEnd()}, args...), limit)...)
}

// CountStoredEpisodes counts StoredEpisodesByKey's whole set. Like a browse
// total, a large count is served from the last one while it recounts.
func (c *Catalog) CountStoredEpisodes(ctx context.Context, show core.Filter) (int, error) {
	if err := c.ensureCatalogueSources(ctx, show); err != nil {
		return 0, err
	}
	show = c.viewerPaging(ctx, show)
	if g := show.Group; g != nil {
		if watchedOnly(*g) {
			plain := show
			plain.Group = nil
			return c.CountPlayedEpisodes(ctx, plain, g.UserID, 0)
		}
		if unwatchedOnly(*g) {
			plain := show
			plain.Group = nil
			all, err := c.CountStoredEpisodes(ctx, plain)
			if err != nil {
				return 0, err
			}
			played, err := c.CountPlayedEpisodes(ctx, plain, g.UserID, 0)
			if err != nil {
				return 0, err
			}
			return max(all-played, 0), nil
		}
	}
	scoped := storedShowScoped(show)
	var where string
	var args []any
	var err error
	if scoped {
		where, args, err = c.storedEpisodesWhere(show)
	} else {
		where, args, err = c.storedEpisodesProbeWhere(show)
	}
	if err != nil {
		return 0, err
	}
	uncacheable := storedBrowseUncacheable(show)
	ckey := "stored-episodes:" + browseFilterKey(c.policyKeyFilter(show)) + c.consoleGateKey(show)
	gen, minute := c.writeGen.Load(), c.minuteBucket()
	c.cacheMu.RLock()
	ent, ok := c.countCache[ckey]
	c.cacheMu.RUnlock()
	if uncacheable {
		ok = false
	}
	count := func(ctx context.Context) (int, error) {
		var total int
		var bySeries bool
		var err error
		total, bySeries, err = c.countStoredEpisodesBySeries(ctx, show)
		if err != nil {
			return 0, err
		}
		if !bySeries {
			if scoped {
				if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles t CROSS JOIN catalog_titles e WHERE t.key>? AND t.key<? AND e.show_key=t.key AND e.key>? AND e.key<? AND `+where,
					append([]any{showKeyStart, showKeyEnd, episodeKeyStart, c.storedEpisodeKeyEnd()}, args...)...).Scan(&total); err != nil {
					return 0, err
				}
			} else {
				if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles e WHERE e.key>? AND e.key<? AND `+where,
					append([]any{episodeKeyStart, c.storedEpisodeKeyEnd()}, args...)...).Scan(&total); err != nil {
					return 0, err
				}
			}
		}
		if !uncacheable {
			c.cacheMu.Lock()
			if len(c.countCache) >= 1024 {
				c.countCache = map[string]cachedCount{}
			}
			c.countCache[ckey] = cachedCount{total: total, gen: gen, minute: minute, counted: time.Now()}
			c.cacheMu.Unlock()
		}
		return total, nil
	}
	switch {
	case ok && ent.gen == gen && ent.minute == minute:
		return ent.total, nil
	case ok && ent.total >= staleCountMin:
		// A busy catalogue bumps the write generation constantly; serving
		// the last large total while a recount runs behind the read keeps
		// a page's total from recounting the whole section (as
		// CountStoredSeasons does).
		if time.Since(ent.counted) >= recountEvery {
			_ = c.hydrate(ctx, "count:"+ckey, 0, func(ctx context.Context) error {
				_, err := count(ctx)
				return err
			})
		}
		return ent.total, nil
	}
	return count(ctx)
}

// EpisodesUnranked reports that no stored episode has a popularity, so a
// popularity order of episodes is their key order. TMDB gives episodes no
// popularity; this checks the index's two ends rather than assuming it.
func (c *Catalog) EpisodesUnranked(ctx context.Context) (bool, error) {
	// Separate MIN and MAX subqueries, so each is one index seek. After the
	// title index swap the index is the partial catalog_titles_child_pop;
	// SQLite uses a partial index only when the query repeats its WHERE
	// term (type='episode' alone does not imply the IN list). Without it
	// each subquery read every episode: 3 to 12 s on a 540k-title library.
	var ranked bool
	err := c.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT MIN(popularity) FROM catalog_titles WHERE type='episode' AND browsable=0 AND `+childPopPartial+`),0)<>0
		OR COALESCE((SELECT MAX(popularity) FROM catalog_titles WHERE type='episode' AND browsable=0 AND `+childPopPartial+`),0)<>0
		OR COALESCE((SELECT MIN(popularity) FROM catalog_titles WHERE type='episode' AND browsable=1 AND `+childPopPartial+`),0)<>0
		OR COALESCE((SELECT MAX(popularity) FROM catalog_titles WHERE type='episode' AND browsable=1 AND `+childPopPartial+`),0)<>0`).Scan(&ranked)
	return !ranked, err
}

// BrowseStoredEpisodes pages stored, aired episodes in the library without
// refreshing shows or seasons from TMDB. Sequential key-ordered pages advance
// via a cached key cursor so deep offsets stay O(limit).
func (c *Catalog) BrowseStoredEpisodes(ctx context.Context, q core.BrowseQuery) (core.Page, error) {
	if err := c.ensureCatalogueSources(ctx, q.Filter); err != nil {
		return core.Page{}, err
	}
	q.Filter = c.viewerPaging(ctx, q.Filter)
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	offset := max(q.Offset, 0)
	gen, minBucket := c.writeGen.Load(), c.minuteBucket()
	uncacheable := storedBrowseUncacheable(q.Filter)
	ckey := "ep:" + browseFilterKey(c.policyKeyFilter(q.Filter)) + c.consoleGateKey(q.Filter)
	bkey := fmt.Sprintf("%s|%s|%v|%v|%d|%d|%v", ckey, q.Sort, q.Desc, q.SecondarySorts, offset, limit, q.NoTotal)
	if q.ListDisplayFacts {
		ctx = context.WithValue(ctx, listDisplayFactsKey{}, true)
		bkey += "|list-display"
	}
	cursorPrefix := fmt.Sprintf("cursor:%s|%s|%v|%v|", ckey, q.Sort, q.Desc, q.SecondarySorts)

	c.cacheMu.RLock()
	if ent, ok := c.browseCache[bkey]; !uncacheable && !q.SortLastViewed && ok && ent.gen == gen && ent.minute == minBucket {
		items := make([]core.Title, len(ent.page.Items))
		for i := range ent.page.Items {
			items[i] = cloneTitle(ent.page.Items[i])
		}
		c.cacheMu.RUnlock()
		return core.Page{Items: items, Total: ent.page.Total}, nil
	}
	var afterKey core.RatingKey
	if offset > 0 && !uncacheable && !q.SortLastViewed {
		if cur, ok := c.browseCache[cursorPrefix+strconv.Itoa(offset)]; ok && pageCursorLive(cur, gen, minBucket) {
			afterKey = cur.page.Items[0].Key
		}
	}
	c.cacheMu.RUnlock()

	var total int
	var err error
	if q.NoTotal {
		total = offset + limit
	} else {
		total, err = c.CountStoredEpisodes(ctx, q.Filter)
		if err != nil {
			return core.Page{}, err
		}
		if total == 0 || offset >= total {
			return core.Page{Items: []core.Title{}, Total: total}, nil
		}
	}

	if q.SortLastViewed {
		where, args, err := c.storedEpisodesWhere(q.Filter)
		if err != nil {
			return core.Page{}, err
		}
		items, err := c.browseHistoryPage(ctx, q, where, args, "e", offset, limit)
		if err != nil {
			return core.Page{}, err
		}
		if q.NoTotal {
			total = offset + len(items)
		}
		return core.Page{Items: items, Total: total}, nil
	}
	keyOrder := !q.SortLastViewed && len(q.SecondarySorts) == 0 && (q.Sort == "" || (q.Sort == core.SortPopularity && !q.Desc))
	if !q.SortLastViewed && !keyOrder && len(q.SecondarySorts) == 0 && q.Sort == core.SortPopularity {
		if unranked, err := c.EpisodesUnranked(ctx); err == nil && unranked {
			keyOrder = true
		}
	}

	smallScoped := storedShowScoped(q.Filter) && ((!q.NoTotal && total <= 5000) || q.Filter.CollectionMembers != 0 || hasRestrictedCatalogueSources(q.Filter.CatalogueSources))
	var items []core.Title
	switch {
	case q.Filter.Group != nil && watchedOnly(*q.Filter.Group):
		plain := q.Filter
		plain.Group = nil
		where, args, werr := c.playedEpisodesWhere(ctx, plain, 0)
		if werr != nil {
			return core.Page{}, werr
		}
		if q.Sort == core.SortUpdated && plain.UpdatedAfter == 0 {
			where = "e.fetched_at>=? AND " + where
			args = append([]any{int64(0)}, args...)
		}
		order := "e.key ASC"
		if !keyOrder {
			order = storedChildOrder("e", q) + ", e.key ASC"
		}
		sqlQ := `SELECT ` + prefixedCols("e") + ` FROM state_watch w CROSS JOIN catalog_titles e JOIN catalog_titles t ON t.key=e.show_key WHERE w.user_id=? AND w.view_count>0 AND e.key=w.rating_key AND ` + where + ` ORDER BY ` + order + ` LIMIT ? OFFSET ?`
		a := append([]any{q.Filter.Group.UserID}, args...)
		a = append(a, limit, offset)
		items, err = c.queryPagedTitles(ctx, sqlQ, a...)
	case keyOrder && (offset == 0 || afterKey > 0):
		items, err = c.StoredEpisodesByKey(ctx, q.Filter, afterKey, limit)
	case keyOrder && storedShowScoped(q.Filter):
		where, args, werr := c.storedEpisodesWhere(q.Filter)
		if werr != nil {
			return core.Page{}, werr
		}
		sqlQ := `SELECT ` + prefixedCols("e") + ` FROM catalog_titles t CROSS JOIN catalog_titles e WHERE t.key>? AND t.key<? AND e.show_key=t.key AND e.key>? AND e.key<? AND ` + where + ` ORDER BY t.key ASC, e.key ASC LIMIT ? OFFSET ?`
		items, err = c.queryPagedTitles(ctx, sqlQ, append(append([]any{showKeyStart, showKeyEnd, episodeKeyStart, c.storedEpisodeKeyEnd()}, args...), limit, offset)...)
	case keyOrder:
		where, args, werr := c.storedEpisodesProbeWhere(q.Filter)
		if werr != nil {
			return core.Page{}, werr
		}
		sqlQ := `SELECT ` + prefixedCols("e") + ` FROM catalog_titles e WHERE e.key>? AND e.key<? AND ` + where + ` ORDER BY e.key ASC LIMIT ? OFFSET ?`
		items, err = c.queryPagedTitles(ctx, sqlQ, append(append([]any{episodeKeyStart, c.storedEpisodeKeyEnd()}, args...), limit, offset)...)
	case !smallScoped && len(q.SecondarySorts) == 0 && q.Desc && (q.Sort == core.SortReleased || q.Sort == core.SortAdded || q.Sort == core.SortUpdated):
		where, args, werr := c.storedEpisodesProbeWhere(q.Filter)
		if werr != nil {
			return core.Page{}, werr
		}
		if q.Sort == core.SortUpdated && q.Filter.UpdatedAfter == 0 {
			where = "e.fetched_at>=? AND " + where
			args = append([]any{int64(0)}, args...)
		}
		order := storedChildOrder("e", q)
		sqlQ := `SELECT ` + prefixedCols("e") + ` FROM catalog_titles e WHERE ` + where + ` ORDER BY ` + order + `, e.key ASC LIMIT ? OFFSET ?`
		items, err = c.queryPagedTitles(ctx, sqlQ, append(append([]any{}, args...), limit, offset)...)
	default:
		where, args, werr := c.storedEpisodesWhere(q.Filter)
		if werr != nil {
			return core.Page{}, werr
		}
		if q.Sort == core.SortUpdated && q.Filter.UpdatedAfter == 0 {
			where = "e.fetched_at>=? AND " + where
			args = append([]any{int64(0)}, args...)
		}
		order := storedChildOrder("e", q)
		from := ` FROM catalog_titles e JOIN catalog_titles t ON t.key=e.show_key WHERE `
		qargs := args
		if smallScoped {
			from = ` FROM catalog_titles t CROSS JOIN catalog_titles e WHERE t.key>? AND t.key<? AND e.show_key=t.key AND e.key>? AND e.key<? AND `
			qargs = append([]any{showKeyStart, showKeyEnd, episodeKeyStart, c.storedEpisodeKeyEnd()}, args...)
		}
		sqlQ := `SELECT ` + prefixedCols("e") + from + where + ` ORDER BY ` + order + `, e.key ASC LIMIT ? OFFSET ?`
		items, err = c.queryPagedTitles(ctx, sqlQ, append(append([]any{}, qargs...), limit, offset)...)
	}
	if err != nil {
		return core.Page{}, err
	}
	if q.Sort == core.SortAdded {
		if err := applyAirAddedTimes(ctx, c.db, items, c.now()); err != nil {
			return core.Page{}, err
		}
	}
	if items == nil {
		items = []core.Title{}
	}
	if q.NoTotal {
		total = offset + len(items)
	}
	if !uncacheable && !q.SortLastViewed {
		c.cacheMu.Lock()
		if len(c.browseCache) >= 512 {
			c.browseCache = map[string]cachedBrowse{}
		}
		if c.writeGen.Load() == gen {
			cachedItems := make([]core.Title, len(items))
			for i := range items {
				cachedItems[i] = cloneTitle(items[i])
			}
			c.browseCache[bkey] = cachedBrowse{page: core.Page{Items: cachedItems, Total: total}, gen: gen, minute: minBucket}
		}
		// The cursor is stored even when a write landed during the read:
		// it only names where this sequential walk reached, and losing it
		// to background writes would break the chain on a busy catalogue.
		if keyOrder && len(items) > 0 {
			last := items[len(items)-1].Key
			c.browseCache[cursorPrefix+strconv.Itoa(offset+len(items))] = cachedBrowse{
				page: core.Page{Items: []core.Title{{Key: last}}}, gen: gen, minute: minBucket, at: time.Now(),
			}
		}
		c.cacheMu.Unlock()
	}
	return core.Page{Items: items, Total: total}, nil
}

func storedChildOrder(alias string, q core.BrowseQuery) string {
	col := alias + ".key"
	switch q.Sort {
	case core.SortTitle:
		col = alias + ".sort_title"
	case core.SortReleased, core.SortAdded:
		col = alias + ".released_ts"
	case core.SortYear:
		col = alias + ".year"
	case core.SortRating:
		col = alias + ".rating"
	case core.SortDuration:
		col = alias + ".runtime_sec"
	case core.SortUpdated:
		col = alias + ".fetched_at"
	case core.SortPopularity:
		col = alias + ".popularity"
	}
	dir := "ASC"
	if q.Desc {
		dir = "DESC"
	}
	return col + " " + dir + browseSecondaryOrder(alias, q.SecondarySorts)
}

// storedSeasonsWhere selects stored, non-adult seasons (s) whose series (t)
// passes the browse filter f. Regular seasons require a known past air date
// and positive stored child count; Specials (season 0) may derive their
// premiere from aired special episodes during hydration.
func (c *Catalog) storedSeasonsWhere(f core.Filter, requireKnownRelease bool) (string, []any, error) {
	show, childExclude := splitStoredFilter(f, core.KindSeason)
	where, args, err := c.browseWhere(show)
	if err != nil {
		return "", nil, err
	}
	nowTS := c.now().Unix()
	var conds []string
	var outArgs []any
	if requireKnownRelease {
		conds = []string{
			"s.type='season'",
			"s.adult=0",
			"s.release_known=1",
			"s.released_ts<=?",
		}
		outArgs = []any{nowTS}
	} else {
		conds = []string{
			"s.type='season'",
			"s.adult=0",
			"((s.release_known=1 AND s.released_ts<=?) OR s.season=0)",
		}
		outArgs = []any{nowTS}
	}
	cConds, cArgs := c.childFilterConds("s", f, childExclude)
	conds = append(conds, cConds...)
	outArgs = append(outArgs, cArgs...)
	conds = append(conds, where)
	outArgs = append(outArgs, args...)
	if f.Group != nil {
		clause, a, err := c.compileBrowseGroup(f.Group, "s", core.KindSeason, core.TypeShow)
		if err != nil {
			return "", nil, err
		}
		conds = append(conds, "("+clause+")")
		outArgs = append(outArgs, a...)
	}
	return strings.Join(conds, " AND "), outArgs, nil
}

// StoredSeasonsByKey returns up to limit stored, listable seasons after the
// key after, in key order (series, season), of the series the browse filter
// show admits. It never refreshes shows or seasons from TMDB.
func (c *Catalog) StoredSeasonsByKey(ctx context.Context, show core.Filter, after core.RatingKey, limit int) ([]core.Title, error) {
	if err := c.ensureCatalogueSources(ctx, show); err != nil {
		return nil, err
	}
	show = c.viewerPaging(ctx, show)
	where, args, err := c.storedSeasonsWhere(show, false)
	if err != nil {
		return nil, err
	}
	lo := max(int64(after), seasonKeyStart)
	scoped := storedShowScoped(show)
	q := `SELECT ` + prefixedCols("s") + ` FROM catalog_titles s JOIN catalog_titles t ON t.key=s.show_key WHERE s.key>? AND s.key<? AND ` + where + ` ORDER BY s.key ASC LIMIT ?`
	if scoped {
		q = `SELECT ` + prefixedCols("s") + ` FROM catalog_titles t CROSS JOIN catalog_titles s WHERE t.key>=? AND t.key<? AND s.show_key=t.key AND s.key>? AND s.key<? AND (t.key>? OR s.key>?) AND ` + where + ` ORDER BY t.key ASC, s.key ASC LIMIT ?`
	}
	now := c.now()
	var out []core.Title
	for len(out) < limit {
		need := limit - len(out)
		var qargs []any
		if scoped {
			showLo, err := c.storedChildCursorShow(ctx, after)
			if err != nil {
				return nil, err
			}
			qargs = append(append([]any{showLo, showKeyEnd, seasonKeyStart, seasonKeyEnd, showLo, lo}, args...), need)
		} else {
			qargs = append(append([]any{lo, seasonKeyEnd}, args...), need)
		}
		batch, err := c.queryTitles(ctx, q, qargs...)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for i := range batch {
			if listableSeason(&batch[i], now) {
				out = append(out, batch[i])
			}
		}
		nextLo := int64(batch[len(batch)-1].Key)
		if len(batch) < need || nextLo == lo {
			break
		}
		lo = nextLo
		after = core.RatingKey(nextLo)
	}
	if out == nil {
		out = []core.Title{}
	}
	return out, nil
}

// CountStoredSeasons counts the stored, listable seasons in the library for
// the browse filter show.
func (c *Catalog) CountStoredSeasons(ctx context.Context, show core.Filter) (int, error) {
	if err := c.ensureCatalogueSources(ctx, show); err != nil {
		return 0, err
	}
	show = c.viewerPaging(ctx, show)
	if g := show.Group; g != nil {
		if watchedOnly(*g) {
			plain := show
			plain.Group = nil
			return c.countPlayedSeasons(ctx, plain, g.UserID)
		}
		if unwatchedOnly(*g) {
			plain := show
			plain.Group = nil
			all, err := c.CountStoredSeasons(ctx, plain)
			if err != nil {
				return 0, err
			}
			played, err := c.countPlayedSeasons(ctx, plain, g.UserID)
			if err != nil {
				return 0, err
			}
			return max(all-played, 0), nil
		}
	}
	where, args, err := c.storedSeasonsWhere(show, false)
	if err != nil {
		return 0, err
	}
	scoped := storedShowScoped(show)
	uncacheable := show.Group != nil || storedBrowseUncacheable(show)
	ckey := "stored-seasons:" + browseFilterKey(c.policyKeyFilter(show)) + c.consoleGateKey(show)
	gen, minute := c.writeGen.Load(), c.minuteBucket()
	c.cacheMu.RLock()
	ent, ok := c.countCache[ckey]
	c.cacheMu.RUnlock()
	if uncacheable {
		ok = false
	}
	count := func(ctx context.Context) (int, error) {
		var total int
		if scoped {
			if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles t CROSS JOIN catalog_titles s WHERE t.key>? AND t.key<? AND s.show_key=t.key AND s.key>? AND s.key<? AND `+where,
				append([]any{showKeyStart, showKeyEnd, seasonKeyStart, seasonKeyEnd}, args...)...).Scan(&total); err != nil {
				return 0, err
			}
		} else {
			if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles s JOIN catalog_titles t ON t.key=s.show_key WHERE s.key>? AND s.key<? AND `+where,
				append([]any{seasonKeyStart, seasonKeyEnd}, args...)...).Scan(&total); err != nil {
				return 0, err
			}
		}
		// For small libraries, refine the SQL count through listableSeason so
		// seasons whose mirrored episodes are all undated are excluded just as
		// in Children(show) and StoredSeasonsByKey.
		if total > 0 && total <= 500 {
			if exact, err := c.StoredSeasonsByKey(ctx, show, 0, total+1); err == nil {
				total = len(exact)
			}
		}
		if !uncacheable {
			c.cacheMu.Lock()
			if len(c.countCache) >= 1024 {
				c.countCache = map[string]cachedCount{}
			}
			c.countCache[ckey] = cachedCount{total: total, gen: gen, minute: minute, counted: time.Now()}
			c.cacheMu.Unlock()
		}
		return total, nil
	}
	switch {
	case ok && ent.gen == gen && ent.minute == minute:
		return ent.total, nil
	case ok && ent.total >= staleCountMin:
		if time.Since(ent.counted) >= recountEvery {
			_ = c.hydrate(ctx, "count:"+ckey, 0, func(ctx context.Context) error {
				_, err := count(ctx)
				return err
			})
		}
		return ent.total, nil
	}
	return count(ctx)
}

func (c *Catalog) countPlayedSeasons(ctx context.Context, show core.Filter, userID int64) (int, error) {
	where, args, err := c.storedSeasonsWhere(show, false)
	if err != nil {
		return 0, err
	}
	wSQL, wArgs := c.watchedSeasonKeysSQL(userID)
	a := append(append([]any{}, wArgs...), args...)
	var total int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles s JOIN catalog_titles t ON t.key=s.show_key WHERE s.key IN (`+wSQL+`) AND `+where, a...).Scan(&total); err != nil {
		return 0, err
	}
	if total > 0 && total <= 500 {
		rows, err := c.queryTitles(ctx, `SELECT `+prefixedCols("s")+` FROM catalog_titles s JOIN catalog_titles t ON t.key=s.show_key WHERE s.key IN (`+wSQL+`) AND `+where+` ORDER BY s.key ASC`, a...)
		if err == nil {
			now := c.now()
			kept := 0
			for i := range rows {
				if listableSeason(&rows[i], now) {
					kept++
				}
			}
			total = kept
		}
	}
	return total, nil
}

// BrowseStoredSeasons pages stored, listable seasons in the library without
// refreshing shows or seasons from TMDB.
func (c *Catalog) BrowseStoredSeasons(ctx context.Context, q core.BrowseQuery) (core.Page, error) {
	if err := c.ensureCatalogueSources(ctx, q.Filter); err != nil {
		return core.Page{}, err
	}
	q.Filter = c.viewerPaging(ctx, q.Filter)
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	offset := max(q.Offset, 0)
	gen, minBucket := c.writeGen.Load(), c.minuteBucket()
	cacheable := q.Filter.Group == nil && !storedBrowseUncacheable(q.Filter)
	ckey := "sn:" + browseFilterKey(c.policyKeyFilter(q.Filter)) + c.consoleGateKey(q.Filter)
	bkey := fmt.Sprintf("%s|%s|%v|%v|%d|%d|%v", ckey, q.Sort, q.Desc, q.SecondarySorts, offset, limit, q.NoTotal)
	cursorPrefix := fmt.Sprintf("cursor:%s|%s|%v|%v|", ckey, q.Sort, q.Desc, q.SecondarySorts)

	c.cacheMu.RLock()
	if ent, ok := c.browseCache[bkey]; cacheable && !q.SortLastViewed && ok && ent.gen == gen && ent.minute == minBucket {
		items := make([]core.Title, len(ent.page.Items))
		for i := range ent.page.Items {
			items[i] = cloneTitle(ent.page.Items[i])
		}
		c.cacheMu.RUnlock()
		return core.Page{Items: items, Total: ent.page.Total}, nil
	}
	var afterKey core.RatingKey
	if offset > 0 && cacheable && !q.SortLastViewed {
		if cur, ok := c.browseCache[cursorPrefix+strconv.Itoa(offset)]; ok && pageCursorLive(cur, gen, minBucket) {
			afterKey = cur.page.Items[0].Key
		}
	}
	c.cacheMu.RUnlock()

	var total int
	var err error
	if q.NoTotal {
		total = offset + limit
	} else {
		total, err = c.CountStoredSeasons(ctx, q.Filter)
		if err != nil {
			return core.Page{}, err
		}
		if !q.SortLastViewed && (total == 0 || offset >= total) {
			return core.Page{Items: []core.Title{}, Total: total}, nil
		}
	}

	if q.SortLastViewed {
		where, args, err := c.storedSeasonsWhere(q.Filter, false)
		if err != nil {
			return core.Page{}, err
		}
		// Mirror listableSeason: a known released season with aired children,
		// or an undated specials season with an aired special. If no child
		// records exist, retain the released season's stored child count.
		where += ` AND ((s.release_known=1 AND s.released_ts<=?) OR (s.season=0 AND s.released_ts=0 AND EXISTS (SELECT 1 FROM catalog_titles x WHERE x.type='episode' AND x.show_key=s.show_key AND x.season=s.season AND x.adult=0 AND x.release_known=1 AND x.released_ts<=?))) AND (EXISTS (SELECT 1 FROM catalog_titles x WHERE x.type='episode' AND x.show_key=s.show_key AND x.season=s.season AND x.adult=0 AND x.release_known=1 AND x.released_ts<=?) OR (s.child_count>0 AND NOT EXISTS (SELECT 1 FROM catalog_titles x WHERE x.type='episode' AND x.show_key=s.show_key AND x.season=s.season)))`
		now := c.now().Unix()
		args = append(args, now, now, now)
		if !q.NoTotal {
			if err = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles s JOIN catalog_titles t ON t.key=s.show_key WHERE `+where, args...).Scan(&total); err != nil {
				return core.Page{}, err
			}
		}
		items, err := c.browseHistoryPage(ctx, q, where, args, "s", offset, limit)
		if err != nil {
			return core.Page{}, err
		}
		if q.NoTotal {
			total = offset + len(items)
		}
		return core.Page{Items: items, Total: total}, nil
	}

	keyOrder := !q.SortLastViewed && len(q.SecondarySorts) == 0 && (q.Sort == "" || (q.Sort == core.SortPopularity && !q.Desc))
	now := c.now()
	var items []core.Title
	switch {
	case q.Filter.Group != nil && watchedOnly(*q.Filter.Group):
		plain := q.Filter
		plain.Group = nil
		requireKnown := q.Sort == core.SortReleased || q.Sort == core.SortAdded
		where, args, werr := c.storedSeasonsWhere(plain, requireKnown)
		if werr != nil {
			return core.Page{}, werr
		}
		if q.Sort == core.SortUpdated && plain.UpdatedAfter == 0 {
			where = "s.fetched_at>=? AND " + where
			args = append([]any{int64(0)}, args...)
		}
		order := "s.key ASC"
		if !keyOrder {
			order = storedChildOrder("s", q) + ", s.key ASC"
		}
		wSQL, wArgs := c.watchedSeasonKeysSQL(q.Filter.Group.UserID)
		sqlQ := `SELECT ` + prefixedCols("s") + ` FROM catalog_titles s JOIN catalog_titles t ON t.key=s.show_key WHERE s.key IN (` + wSQL + `) AND ` + where + ` ORDER BY ` + order + ` LIMIT ? OFFSET ?`
		a := append(append([]any{}, wArgs...), args...)
		a = append(a, limit, offset)
		raw, qerr := c.queryTitles(ctx, sqlQ, a...)
		if qerr != nil {
			return core.Page{}, qerr
		}
		for i := range raw {
			if listableSeason(&raw[i], now) {
				items = append(items, raw[i])
			}
		}
	case keyOrder && (offset == 0 || afterKey > 0):
		items, err = c.StoredSeasonsByKey(ctx, q.Filter, afterKey, limit)
	case keyOrder && total <= 500:
		all, aerr := c.StoredSeasonsByKey(ctx, q.Filter, 0, offset+limit)
		if aerr != nil {
			return core.Page{}, aerr
		}
		if offset < len(all) {
			items = all[offset:min(offset+limit, len(all))]
		}
	case keyOrder && storedShowScoped(q.Filter):
		where, args, werr := c.storedSeasonsWhere(q.Filter, false)
		if werr != nil {
			return core.Page{}, werr
		}
		sqlQ := `SELECT ` + prefixedCols("s") + ` FROM catalog_titles t CROSS JOIN catalog_titles s WHERE t.key>? AND t.key<? AND s.show_key=t.key AND s.key>? AND s.key<? AND ` + where + ` ORDER BY t.key ASC, s.key ASC LIMIT ? OFFSET ?`
		raw, qerr := c.queryTitles(ctx, sqlQ, append(append([]any{showKeyStart, showKeyEnd, seasonKeyStart, seasonKeyEnd}, args...), limit, offset)...)
		if qerr != nil {
			return core.Page{}, qerr
		}
		for i := range raw {
			if listableSeason(&raw[i], now) {
				items = append(items, raw[i])
			}
		}
	case keyOrder:
		where, args, werr := c.storedSeasonsWhere(q.Filter, false)
		if werr != nil {
			return core.Page{}, werr
		}
		sqlQ := `SELECT ` + prefixedCols("s") + ` FROM catalog_titles s JOIN catalog_titles t ON t.key=s.show_key WHERE s.key>? AND s.key<? AND ` + where + ` ORDER BY s.key ASC LIMIT ? OFFSET ?`
		raw, qerr := c.queryTitles(ctx, sqlQ, append(append([]any{seasonKeyStart, seasonKeyEnd}, args...), limit, offset)...)
		if qerr != nil {
			return core.Page{}, qerr
		}
		for i := range raw {
			if listableSeason(&raw[i], now) {
				items = append(items, raw[i])
			}
		}
	default:
		requireKnown := q.Sort == core.SortReleased || q.Sort == core.SortAdded
		where, args, werr := c.storedSeasonsWhere(q.Filter, requireKnown)
		if werr != nil {
			return core.Page{}, werr
		}
		if q.Sort == core.SortUpdated && q.Filter.UpdatedAfter == 0 {
			where = "s.fetched_at>=? AND " + where
			args = append([]any{int64(0)}, args...)
		}
		order := storedChildOrder("s", q)
		sqlQ := `SELECT ` + prefixedCols("s") + ` FROM catalog_titles s JOIN catalog_titles t ON t.key=s.show_key WHERE ` + where + ` ORDER BY ` + order + `, s.key ASC LIMIT ? OFFSET ?`
		raw, qerr := c.queryTitles(ctx, sqlQ, append(append([]any{}, args...), limit, offset)...)
		if qerr != nil {
			return core.Page{}, qerr
		}
		for i := range raw {
			if listableSeason(&raw[i], now) {
				items = append(items, raw[i])
			}
		}
	}
	if err != nil {
		return core.Page{}, err
	}
	if items == nil {
		items = []core.Title{}
	}
	if q.NoTotal {
		total = offset + len(items)
	}
	if cacheable && !q.SortLastViewed {
		c.cacheMu.Lock()
		if len(c.browseCache) >= 512 {
			c.browseCache = map[string]cachedBrowse{}
		}
		if c.writeGen.Load() == gen {
			cachedItems := make([]core.Title, len(items))
			for i := range items {
				cachedItems[i] = cloneTitle(items[i])
			}
			c.browseCache[bkey] = cachedBrowse{page: core.Page{Items: cachedItems, Total: total}, gen: gen, minute: minBucket}
		}
		if keyOrder && len(items) > 0 {
			last := items[len(items)-1].Key
			c.browseCache[cursorPrefix+strconv.Itoa(offset+len(items))] = cachedBrowse{
				page: core.Page{Items: []core.Title{{Key: last}}}, gen: gen, minute: minBucket, at: time.Now(),
			}
		}
		c.cacheMu.Unlock()
	}
	return core.Page{Items: items, Total: total}, nil
}
