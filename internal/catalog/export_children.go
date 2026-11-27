package catalog

import (
	"context"
	"slices"

	"github.com/braedonsaunders/ether/internal/config"

	"github.com/braedonsaunders/ether/internal/core"
)

type exportChildrenKey struct{}

type exportChildren struct {
	byParent map[core.RatingKey][]core.Title
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
	memo := exportChildren{byParent: make(map[core.RatingKey][]core.Title), gen: c.writeGen.Load(), minute: c.minuteBucket(), catalog: c}
	args := make([]any, len(shows), len(shows)+1)
	for n, key := range shows {
		args[n] = key
	}
	// Limit before hydration. Large trees use the existing per-parent path.
	const maxRows = 10000
	args = append(args, maxRows+1)
	q := `SELECT ` + titleCols + ` FROM catalog_titles` + c.indexedBy("catalog_titles_show") +
		` WHERE show_key IN (` + placeholders(len(shows)) + `) AND adult=0
		AND (type='season' OR (type='episode' AND key<5000000000000000))
		ORDER BY show_key,season,episode,key LIMIT ?`
	rows, err := c.scanTitleKeys(ctx, q, args...)
	if err != nil || len(rows) > maxRows {
		return ctx, err
	}
	now := c.now()
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
		if err := c.hydrateRows(ctx, lean, factSet(facts)); err != nil {
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
				memo.byParent[row.ShowKey] = append(memo.byParent[row.ShowKey], row)
			}
		case core.TypeEpisode:
			memo.byParent[row.ParentKey] = append(memo.byParent[row.ParentKey], row)
		}
	}
	return context.WithValue(ctx, exportChildrenKey{}, memo), nil
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
