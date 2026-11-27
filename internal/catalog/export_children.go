package catalog

import (
	"context"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type exportChildrenKey struct{}

type exportChildren struct {
	byParent map[core.RatingKey][]core.Title
	seasons  map[core.RatingKey]core.Title
	ids      map[core.RatingKey]int32
	future   map[core.RatingKey][]core.Title
	gen      uint64
	minute   int64
	catalog  *Catalog
}

// PrepareExportChildren loads the canonical season and aired episode rows for
// an export page in one lookup. Children still chooses the viewer's episode
// order and applies its usual policy. Oversized or alternate-order trees
// continue through the per-parent reader.
func (c *Catalog) PrepareExportChildren(ctx context.Context, shows []core.RatingKey, projection *core.ListRows) (context.Context, error) {
	if c.canProjectAlternateOrders() || len(shows) == 0 {
		return ctx, nil
	}
	// A library default can still select a provider order on a mirror
	// without stored layouts. Check all show defaults in one batch.
	orders, err := c.queryShowOrders(ctx, shows)
	if err != nil {
		return ctx, err
	}
	canonical := make([]core.RatingKey, 0, len(shows))
	for _, show := range shows {
		order, ok := orders[show]
		if ok && !providerLayout(&order.title, order.order) {
			canonical = append(canonical, show)
		}
	}
	shows = canonical
	if len(shows) == 0 {
		return ctx, nil
	}
	memo := exportChildren{seasons: make(map[core.RatingKey]core.Title), byParent: make(map[core.RatingKey][]core.Title), gen: c.writeGen.Load(), minute: c.minuteBucket(), catalog: c}
	args := make([]any, len(shows), len(shows)+1)
	for n, key := range shows {
		args[n] = key
	}
	// Limit before hydration. Large trees use the existing per-parent path.
	const maxRows = 10000
	args = append(args, maxRows+1)
	q := `SELECT ` + prefixedCols("t") + `,COALESCE(x.id,0),` + episodeAirStampSQL + ` FROM catalog_titles AS t` + c.indexedBy("catalog_titles_show") +
		` LEFT JOIN frontend_compact_ids x ON x.surface='xtream' AND x.kind='item' AND x.ref=CAST(t.key AS TEXT)
		LEFT JOIN catalog_episode_provider_air p ON p.key=t.key
		LEFT JOIN catalog_episode_release e ON e.key=t.key
		WHERE t.show_key IN (` + placeholders(len(shows)) + `) AND t.adult=0
		AND (t.type='season' OR (t.type='episode' AND t.key<5000000000000000))
		ORDER BY t.show_key,t.season,t.episode,t.key LIMIT ?`
	rows, ids, err := c.readExportChildren(ctx, q, args...)
	if err != nil || len(rows) > maxRows {
		return ctx, err
	}
	now := c.now()
	if c.config().ShowUpcomingEpisodes {
		if err := c.prepareExportSchedule(ctx, &memo, rows, now, orders); err != nil {
			return ctx, err
		}
	}
	rows = slices.DeleteFunc(rows, func(t core.Title) bool {
		return !t.IsReleased(now) && !(t.Type == core.TypeSeason && t.Released.IsZero())
	})
	var lean, full []core.Title
	var leanAt, fullAt []int
	for n, row := range rows {
		if projection == nil || row.Type == core.TypeSeason || (projection.Detail != nil && projection.Detail(row)) {
			full, fullAt = append(full, row), append(fullAt, n)
		} else {
			lean, leanAt = append(lean, row), append(leanAt, n)
		}
	}
	if err := c.hydrateSlice(ctx, full); err != nil {
		return ctx, err
	}
	if len(lean) != 0 {
		facts := projection.Facts | c.policyFacts(c.visibilityFor(ctx)) | c.viewerPolicyFacts(ctx)
		if err := c.hydrateRows(ctx, lean, factSet(facts)|factEpisodeAirLoaded, false); err != nil {
			return ctx, err
		}
	}
	digital := c.config().ReleaseMode == config.ModeDigital
	visible := func(title core.Title) bool {
		return title.IsReleased(c.now()) && (!digital || c.homeVisible(&title))
	}
	for n, idx := range fullAt {
		rows[idx] = full[n]
	}
	for n, idx := range leanAt {
		rows[idx] = lean[n]
	}
	rows = slices.DeleteFunc(rows, func(t core.Title) bool { return !visible(t) })
	full = slices.DeleteFunc(full, func(t core.Title) bool { return !visible(t) })
	c.rememberTitles(memo.gen, memo.minute, full)
	if err := c.ProjectEpisodeTitles(ctx, rows); err != nil {
		return ctx, err
	}
	for _, show := range shows {
		memo.byParent[show] = nil
	}
	for _, row := range rows {
		switch row.Type {
		case core.TypeSeason:
			memo.byParent[row.Key] = nil
			if listableSeason(&row, now) {
				memo.seasons[row.Key] = row
				memo.byParent[row.ShowKey] = append(memo.byParent[row.ShowKey], row)
			}
		case core.TypeEpisode:
			memo.byParent[row.ParentKey] = append(memo.byParent[row.ParentKey], row)
		}
	}
	memo.ids = ids
	return context.WithValue(ctx, exportChildrenKey{}, memo), nil
}

// prepareExportSchedule retains the future episode slice already collected
// by the canonical child query. The normal scheduled reader caps each season
// at 1000 episodes in episode order.
func (c *Catalog) prepareExportSchedule(ctx context.Context, memo *exportChildren, rows []core.Title, now time.Time, shows map[core.RatingKey]showOrder) error {
	memo.future = make(map[core.RatingKey][]core.Title)
	var future []core.Title
	counts := make(map[core.RatingKey]int)
	for _, title := range rows {
		if title.Type != core.TypeEpisode || title.Season <= 0 || !title.IsFutureRelease(now) || shows[title.ShowKey].title.Adult {
			continue
		}
		parent := title.Key.Parent()
		if counts[parent] < 1000 {
			counts[parent]++
			future = append(future, title)
		}
	}
	if err := c.hydrateSlice(ctx, future); err != nil {
		return err
	}
	if err := c.ProjectEpisodeTitles(ctx, future); err != nil {
		return err
	}
	for _, title := range future {
		memo.future[title.ParentKey] = append(memo.future[title.ParentKey], title)
	}
	return nil
}

func (c *Catalog) exportSchedule(ctx context.Context, season core.RatingKey) ([]core.Title, bool) {
	memo, ok := ctx.Value(exportChildrenKey{}).(exportChildren)
	if !ok || memo.catalog != c || c.writeGen.Load() != memo.gen || c.minuteBucket() != memo.minute || memo.future == nil {
		return nil, false
	}
	if _, ok := memo.seasons[season]; !ok {
		return nil, false
	}
	items := memo.future[season]
	out := make([]core.Title, len(items))
	for i, item := range items {
		out[i] = cloneTitle(item)
	}
	return out, true
}

func (c *Catalog) readExportChildren(ctx context.Context, query string, args ...any) ([]core.Title, map[core.RatingKey]int32, error) {
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var titles []core.Title
	ids := make(map[core.RatingKey]int32)
	for rows.Next() {
		var id int32
		var air int64
		title, err := scanTitle(extraScan{rows, []any{&id, &air}})
		if err != nil {
			return nil, nil, err
		}
		if title.t.Type == core.TypeEpisode {
			title.t.EpisodeAirDate = time.Time{}
			if air != 0 {
				title.t.EpisodeAirDate = time.Unix(air, 0).UTC()
			}
			if id != 0 {
				ids[title.t.Key] = id
			}
		}
		titles = append(titles, title.t)
	}
	return titles, ids, rows.Err()
}

// ExportCompactIDs returns the stable protocol IDs joined to this batch.
func (c *Catalog) ExportCompactIDs(ctx context.Context) map[core.RatingKey]int32 {
	memo, _ := ctx.Value(exportChildrenKey{}).(exportChildren)
	return memo.ids
}

// ExportChildTitles returns the policy rows loaded for this export batch.
func (c *Catalog) ExportChildTitles(ctx context.Context, roots []core.Title) []core.Title {
	memo, ok := ctx.Value(exportChildrenKey{}).(exportChildren)
	if !ok || memo.catalog != c || memo.gen != c.writeGen.Load() || memo.minute != c.minuteBucket() {
		return nil
	}
	titles := make([]core.Title, 0, len(roots))
	for _, root := range roots {
		if _, prepared := memo.byParent[root.Key]; prepared {
			titles = append(titles, root)
		}
	}
	for _, children := range memo.byParent {
		titles = append(titles, children...)
	}
	return titles
}

func (c *Catalog) preparedExportSeason(ctx context.Context, key core.RatingKey) (*core.Title, bool) {
	memo, ok := ctx.Value(exportChildrenKey{}).(exportChildren)
	if !ok || memo.catalog != c || c.writeGen.Load() != memo.gen || c.minuteBucket() != memo.minute {
		return nil, false
	}
	season, ok := memo.seasons[key]
	if !ok {
		return nil, false
	}
	season = cloneTitle(season)
	return &season, true
}

func (c *Catalog) exportChildrenFor(ctx context.Context, parent core.RatingKey) ([]core.Title, bool) {
	memo, ok := ctx.Value(exportChildrenKey{}).(exportChildren)
	if !ok || memo.catalog != c || c.writeGen.Load() != memo.gen || c.minuteBucket() != memo.minute {
		return nil, false
	}
	items, found := memo.byParent[parent]
	out := make([]core.Title, len(items))
	for j, item := range items {
		out[j] = cloneTitle(item)
	}
	return out, found
}
