package pluginitems

import (
	"context"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// browseLastViewed returns a keyset-paged slice of plugin items ordered by the
// requesting viewer's watch recency (state_watch.last_viewed_at > 0), followed
// by matching undated/unviewed items in sort_title order. Phase 0 drives from
// state_watch_last_viewed into plugin_items(seq), keeping queries bounded to
// the viewer's watch rows rather than walking the library.
func (s *Service) browseLastViewed(
	ctx context.Context,
	q BrowseQuery,
	plan browseQueryPlan,
	hasFacets bool,
	parentRef string,
	level string,
	tally browseTallyInfo,
	desc bool,
	limit int,
) (BrowsePage, error) {
	viewerID := plan.viewerID
	var cur browseCursor
	hasCursor := strings.TrimSpace(q.Cursor) != ""
	if hasCursor {
		var err error
		cur, err = decodeBrowseCursor(q.Cursor)
		if err != nil {
			return BrowsePage{}, err
		}
	}

	keyBase := core.PluginKeyOffset
	keyEnd := core.PluginKeyOffset + core.PluginKeyMaxSeq

	// Rolled watch history CTE: drives strictly from state_watch for this
	// viewer, joins plugin_items by seq for this library, and rolls each
	// watched item's last_viewed_at up to its ancestor containers (up to the
	// 5-level hierarchy) taking MAX(last_viewed_at) per item_ref.
	rolledCTE := fmt.Sprintf(`WITH viewer_watch AS (
	SELECT p0.item_ref AS r0,
	       p0.parent_ref AS r1,
	       COALESCE(p1.parent_ref, '') AS r2,
	       COALESCE(p2.parent_ref, '') AS r3,
	       COALESCE(p3.parent_ref, '') AS r4,
	       w.last_viewed_at AS last_viewed_at
	FROM state_watch w
	JOIN plugin_items p0 ON p0.seq = w.rating_key - %d
	LEFT JOIN plugin_items p1 ON p0.parent_ref <> '' AND p1.item_ref = p0.parent_ref
	LEFT JOIN plugin_items p2 ON p1.parent_ref IS NOT NULL AND p1.parent_ref <> '' AND p2.item_ref = p1.parent_ref
	LEFT JOIN plugin_items p3 ON p2.parent_ref IS NOT NULL AND p2.parent_ref <> '' AND p3.item_ref = p2.parent_ref
	WHERE w.user_id = ? AND w.last_viewed_at > 0 AND w.rating_key > %d AND w.rating_key < %d AND p0.library_id = ?
),
rolled AS (
	SELECT item_ref, MAX(last_viewed_at) AS last_viewed_at
	FROM (
		SELECT r0 AS item_ref, last_viewed_at FROM viewer_watch
		UNION ALL
		SELECT r1 AS item_ref, last_viewed_at FROM viewer_watch WHERE r1 <> ''
		UNION ALL
		SELECT r2 AS item_ref, last_viewed_at FROM viewer_watch WHERE r2 <> ''
		UNION ALL
		SELECT r3 AS item_ref, last_viewed_at FROM viewer_watch WHERE r3 <> ''
		UNION ALL
		SELECT r4 AS item_ref, last_viewed_at FROM viewer_watch WHERE r4 <> ''
	) u
	GROUP BY item_ref
)
`, keyBase, keyBase, keyEnd)

	var items []core.KindItem
	var viewedAts []int64
	phase0Count := 0

	// Phase 0: dated watch history from state_watch (including ancestor
	// rollups), bounded to this viewer and library. Because Phase 0 drives
	// from rolled watch rows, apply the full item predicate (including the
	// driver facet as correlated EXISTS when present).
	if viewerID > 0 && (!hasCursor || cur.Phase == 0) {
		fullWhere, fullArgs := plan.fullWhereAndArgs()
		var p0Where strings.Builder
		p0Args := make([]any, 0, len(fullArgs)+6)
		p0Args = append(p0Args, viewerID, int64(q.LibraryID))
		fmt.Fprintf(&p0Where, `i.item_ref = r.item_ref AND (%s)`, fullWhere)
		p0Args = append(p0Args, fullArgs...)

		if hasCursor && !cur.StartPhase {
			if desc {
				p0Where.WriteString(` AND (r.last_viewed_at <= ? AND (r.last_viewed_at < ? OR i.item_ref > ?))`)
			} else {
				p0Where.WriteString(` AND (r.last_viewed_at >= ? AND (r.last_viewed_at > ? OR i.item_ref > ?))`)
			}
			p0Args = append(p0Args, cur.IntVal, cur.IntVal, cur.ItemRef)
		}

		dir := "ASC"
		if desc {
			dir = "DESC"
		}
		p0Args = append(p0Args, limit+1)

		querySQL := rolledCTE + `SELECT ` + prefixedItemCols("i.") + `, r.last_viewed_at
FROM rolled r CROSS JOIN plugin_items i
WHERE ` + p0Where.String() + `
ORDER BY r.last_viewed_at ` + dir + `, i.item_ref ASC
LIMIT ?`

		rows, err := s.db.QueryContext(ctx, querySQL, p0Args...)
		if err != nil {
			return BrowsePage{}, err
		}
		for rows.Next() {
			var viewedAt int64
			it, _, _, scanErr := scanItemRowExtra(rows, &viewedAt)
			if scanErr != nil {
				rows.Close()
				return BrowsePage{}, scanErr
			}
			items = append(items, it)
			viewedAts = append(viewedAts, viewedAt)
		}
		if err := rows.Close(); err != nil {
			return BrowsePage{}, err
		}
		if err := rows.Err(); err != nil {
			return BrowsePage{}, err
		}

		if len(items) > limit {
			last := items[limit-1]
			lastViewed := viewedAts[limit-1]
			items = items[:limit]
			nextCursor := encodeBrowseCursor(browseCursor{
				Sort:    "last_viewed_at",
				Desc:    desc,
				Phase:   0,
				IntVal:  lastViewed,
				ItemRef: string(last.Ref),
			})
			if len(items) > 0 {
				if err := s.hydrateArtSlice(ctx, items); err != nil {
					return BrowsePage{}, err
				}
			}
			total, totalExact, err := s.resolveBrowseTotal(ctx, q, plan, hasFacets, parentRef, tally, false, len(items))
			if err != nil {
				return BrowsePage{}, err
			}
			return BrowsePage{Items: items, Total: total, TotalExact: totalExact, NextCursor: nextCursor}, nil
		}
		phase0Count = len(items)
	}

	// Phase 1: matching items without a dated watch row (or rolled-up child
	// history), in sort_title order. When TabPolicy/Policy requires viewer
	// history, baseWhere already restricts i.item_ref to watched/rolled items
	// so unviewed items never leak through.
	// When Phase 0 filled exactly limit items, need == 1 probes whether Phase 1
	// has any remaining rows without over-fetching.
	need := limit + 1 - len(items)
	if need > 0 {
		isPG := store.DialectOf(s.db) == store.Postgres
		cols := prefixedItemCols("i.")
		p1Where := plan.baseWhere
		p1ItemArgs := append([]any(nil), plan.baseArgs...)
		if viewerID > 0 {
			p1Where += ` AND NOT EXISTS (SELECT 1 FROM rolled _r WHERE _r.item_ref = i.item_ref)`
		}
		if hasCursor && cur.Phase == 1 && !cur.StartPhase {
			p1Where += ` AND (i.sort_title >= ? AND (i.sort_title > ? OR i.item_ref > ?))`
			p1ItemArgs = append(p1ItemArgs, cur.StrVal, cur.StrVal, cur.ItemRef)
		}

		var querySQL string
		var p1Args []any
		switch {
		case viewerID > 0 && plan.driver != nil:
			p1Args = make([]any, 0, 2+len(plan.driver.cteArgs)+len(p1ItemArgs)+1)
			p1Args = append(p1Args, viewerID, int64(q.LibraryID))
			p1Args = append(p1Args, plan.driver.cteArgs...)
			p1Args = append(p1Args, p1ItemArgs...)
			p1Args = append(p1Args, need)
			baseCTE := strings.TrimRight(rolledCTE, "\n")
			if isPG {
				querySQL = baseCTE + `,
_drv AS MATERIALIZED (
	` + plan.driver.cteSQL + `
)
SELECT ` + cols + `
FROM _drv _d
CROSS JOIN LATERAL (
	SELECT ` + cols + `
	FROM plugin_items i
	WHERE i.item_ref = _d.item_ref AND ` + p1Where + `
	LIMIT 1
) i
ORDER BY i.sort_title ASC, i.item_ref ASC
LIMIT ?`
			} else {
				querySQL = baseCTE + `,
_drv(item_ref) AS (
	` + plan.driver.cteSQL + `
)
SELECT ` + cols + `
FROM _drv _d
CROSS JOIN plugin_items i
WHERE i.item_ref = _d.item_ref AND ` + p1Where + `
ORDER BY i.sort_title ASC, i.item_ref ASC
LIMIT ?`
			}
		case viewerID > 0:
			p1Args = make([]any, 0, 2+len(p1ItemArgs)+1)
			p1Args = append(p1Args, viewerID, int64(q.LibraryID))
			p1Args = append(p1Args, p1ItemArgs...)
			p1Args = append(p1Args, need)
			if plan.matchSort != "" {
				querySQL = plan.matchFirstPageSQL(strings.TrimRight(rolledCTE, "\n")+",\n", cols, p1Where, "i.sort_title ASC, i.item_ref ASC")
				break
			}
			querySQL = rolledCTE + `SELECT ` + cols + `
FROM plugin_items i
WHERE ` + p1Where + `
ORDER BY i.sort_title ASC, i.item_ref ASC
LIMIT ?`
		default:
			querySQL, p1Args = plan.pageSQLAndArgs(isPG, cols, p1Where, p1ItemArgs, "i.sort_title ASC, i.item_ref ASC", need)
		}

		tail, err := s.queryItems(ctx, querySQL, p1Args, false)
		if err != nil {
			return BrowsePage{}, err
		}
		items = append(items, tail...)
	}

	var nextCursor string
	if len(items) > limit {
		items = items[:limit]
		if phase0Count == limit {
			nextCursor = encodeBrowseCursor(browseCursor{
				Sort:       "last_viewed_at",
				Desc:       desc,
				Phase:      1,
				StartPhase: true,
			})
		} else {
			last := items[limit-1]
			nextCursor = encodeBrowseCursor(browseCursor{
				Sort:    "last_viewed_at",
				Desc:    desc,
				Phase:   1,
				StrVal:  last.SortTitle,
				ItemRef: string(last.Ref),
			})
		}
	}

	if len(items) > 0 {
		if err := s.hydrateArtSlice(ctx, items); err != nil {
			return BrowsePage{}, err
		}
	}
	if items == nil {
		items = []core.KindItem{}
	}
	total, totalExact, err := s.resolveBrowseTotal(ctx, q, plan, hasFacets, parentRef, tally, !hasCursor && nextCursor == "", len(items))
	if err != nil {
		return BrowsePage{}, err
	}
	return BrowsePage{
		Items:      items,
		Total:      total,
		TotalExact: totalExact,
		NextCursor: nextCursor,
	}, nil
}
