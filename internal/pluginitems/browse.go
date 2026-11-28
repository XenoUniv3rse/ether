package pluginitems

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"github.com/braedonsaunders/ether/internal/store"
)

// BrowseQuery specifies a keyset-paged browse over a plugin library and level
// (spec §2.3, §3.4).
type BrowseQuery struct {
	// NoTotal avoids count queries for clients using cursor continuation.
	NoTotal bool
	// CheapTotal, when combined with NoTotal, still populates Total (and sets
	// BrowsePage.TotalExact) when the exact count is available cheaply from
	// plugin_item_tally or a single-parent child count, without scanning the
	// whole library.
	CheapTotal bool
	Policy     *libraryfilter.Filter
	TabPolicy  *libraryfilter.Filter
	ViewerID   int64
	Hidden     bool
	LibraryID  core.LibraryID
	ParentRef  core.ItemRef
	Level      string
	Levels     []string
	AnyLevel   bool
	PersonID   int64
	Sort       string              // title (default), added, released, year, rating, index, lastViewed
	Desc       bool                // false = default direction for Sort; true = reverse
	Facets     map[string][]string // field key -> values, any of (OR within key, AND across keys)
	// AllFacets requires every listed value per field; ExcludeFacets
	// drops items having any listed value; AnyGroups adds one OR-group
	// per entry (a single {field, values} list or {any: [...]} across
	// fields, #767, #807); Empty tests field presence/absence per key;
	// ExcludeQuery drops items matching the same text search as Search.
	AllFacets     map[string][]string
	ExcludeFacets map[string][]string
	AnyGroups     []core.KindBrowseFacetGroup
	Empty         map[string]bool
	ExcludeQuery  string
	// Watched filters on the viewer's watch state in SQL (ViewerID):
	// true keeps items with a state_watch view_count > 0, false keeps
	// items without one. Nil applies no watched predicate.
	Watched *bool
	// MaxContentRating caps items at this content-rating enforcement
	// level in SQL (persisted rating_level). Unrated items never pass a
	// cap, matching contentrating.EnforceExceeds. Empty is uncapped.
	MaxContentRating string
	// ExcludeKeys drops these plugin items by RatingKey. Callers bound
	// the list; non-plugin keys are ignored.
	ExcludeKeys []core.RatingKey
	// ReleaseFrom filters known release dates by an inclusive Unix timestamp.
	ReleaseFrom *int64
	Cursor      string // opaque keyset cursor
	Limit       int
}

// BrowsePage is a keyset page of plugin items.
type BrowsePage struct {
	Items      []core.KindItem `json:"items"`
	Total      int             `json:"total"`
	TotalExact bool            `json:"-"`
	NextCursor string          `json:"nextCursor,omitempty"`
}

// FacetCount is a single facet value and item count.
type FacetCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// FacetBucket is one discrete facet bucket (id, label, count) for browse UIs.
type FacetBucket struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

type browseCursor struct {
	Sort       string  `json:"s"`
	Desc       bool    `json:"d,omitempty"`
	Phase      int     `json:"p,omitempty"`
	StartPhase bool    `json:"sp,omitempty"`
	StrVal     string  `json:"sv,omitempty"`
	IntVal     int64   `json:"iv,omitempty"`
	FloatVal   float64 `json:"fv,omitempty"`
	ItemRef    string  `json:"r"`
}

func encodeBrowseCursor(c browseCursor) string {
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeBrowseCursor(raw string) (browseCursor, error) {
	var c browseCursor
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return c, fmt.Errorf("pluginitems: invalid cursor: %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("pluginitems: invalid cursor payload: %w", err)
	}
	if c.ItemRef == "" && !c.StartPhase {
		return c, fmt.Errorf("pluginitems: cursor missing item_ref")
	}
	return c, nil
}

// normalizeSortField returns the canonical column name, value kind ("str", "int", "float", "last_viewed"),
// and default descending flag for a browse sort name.
func normalizeSortField(sortName string) (col string, kind string, defaultDesc bool) {
	switch strings.ToLower(strings.TrimSpace(sortName)) {
	case "", "title", "sort_title":
		return "sort_title", "str", false
	case "added", "added_at", "date_added":
		return "added_at", "int", true
	case "released", "originally_available_at", "release_date":
		return "released", "int", true
	case "year":
		return "year", "int", true
	case "rating":
		return "rating", "float", true
	case "index", "idx":
		return "idx", "int", false
	case "lastviewed", "last_viewed", "played":
		return "last_viewed_at", "last_viewed", true
	default:
		return "sort_title", "str", false
	}
}

func parseBrowseSort(q BrowseQuery) (col, kind string, desc, defaultDesc bool) {
	col, kind, defaultDesc = normalizeSortField(q.Sort)
	desc = defaultDesc
	if q.Desc {
		desc = !defaultDesc
	}
	lower := strings.ToLower(strings.TrimSpace(q.Sort))
	if strings.HasSuffix(lower, ":desc") {
		col, kind, defaultDesc = normalizeSortField(strings.TrimSuffix(lower, ":desc"))
		desc = true
	} else if strings.HasSuffix(lower, ":asc") {
		col, kind, defaultDesc = normalizeSortField(strings.TrimSuffix(lower, ":asc"))
		desc = false
	}
	return col, kind, desc, defaultDesc
}

// browseDriver describes a positive plugin_item_fields subquery that drives a
// Browse query (both the page statement and its paired COUNT(*)) from the most
// selective positive facet, with every remaining facet / exclusion probed via
// correlated EXISTS / NOT EXISTS on (item_ref, field, ...).
type browseDriver struct {
	cteSQL     string
	cteArgs    []any
	existsSQL  string // correlated EXISTS predicate on alias `i`, for last_viewed Phase 0 and legacy callers
	existsArgs []any
}

type browseQueryPlan struct {
	driver    *browseDriver
	matchSort string
	baseWhere string
	baseArgs  []any
	viewerID  int64
	empty     bool
}

func (p browseQueryPlan) pageSQLAndArgs(isPG bool, cols, pageWhere string, pageArgs []any, orderBy string, limitPlusOne int) (string, []any) {
	if p.driver == nil {
		args := make([]any, 0, len(pageArgs)+1)
		args = append(args, pageArgs...)
		args = append(args, limitPlusOne)
		if p.matchSort != "" {
			return p.matchFirstPageSQL("WITH ", cols, pageWhere, orderBy), args
		}
		return `SELECT ` + cols + `
FROM plugin_items i
WHERE ` + pageWhere + `
ORDER BY ` + orderBy + `
LIMIT ?`, args
	}
	args := make([]any, 0, len(p.driver.cteArgs)+len(pageArgs)+1)
	args = append(args, p.driver.cteArgs...)
	args = append(args, pageArgs...)
	args = append(args, limitPlusOne)
	if isPG {
		// Materializing the narrow driver CTE and joining through a LIMIT 1
		// LATERAL probe fences PostgreSQL against planner cardinality skew
		// (e.g. walking the sort index or hash-joining broad facets).
		return `WITH _drv AS MATERIALIZED (
	` + p.driver.cteSQL + `
)
SELECT ` + cols + `
FROM _drv _d
CROSS JOIN LATERAL (
	SELECT ` + cols + `
	FROM plugin_items i
	WHERE i.item_ref = _d.item_ref AND ` + pageWhere + `
	LIMIT 1
) i
ORDER BY ` + orderBy + `
LIMIT ?`, args
	}
	return `WITH _drv(item_ref) AS (
	` + p.driver.cteSQL + `
)
SELECT ` + cols + `
FROM _drv _d
CROSS JOIN plugin_items i
WHERE i.item_ref = _d.item_ref AND ` + pageWhere + `
ORDER BY ` + orderBy + `
LIMIT ?`, args
}

// Fence the filters from ORDER BY/LIMIT so PostgreSQL can plan a hash
// (anti-)join independently of the requested order. Keep only refs and the
// sort key in the materialized result, then read the payload for the page.
func (p browseQueryPlan) matchFirstPageSQL(prefix, cols, where, orderBy string) string {
	return prefix + `_matches AS MATERIALIZED (
	SELECT i.item_ref, i.` + p.matchSort + `
	FROM plugin_items i WHERE ` + where + `
), _page AS MATERIALIZED (
	SELECT i.item_ref FROM _matches i ORDER BY ` + orderBy + ` LIMIT ?
)
SELECT ` + cols + `
FROM _page p CROSS JOIN plugin_items i WHERE i.item_ref = p.item_ref
ORDER BY ` + orderBy
}

func (p browseQueryPlan) countSQLAndArgs(isPG bool) (string, []any) {
	if p.driver == nil {
		return `SELECT COUNT(*) FROM plugin_items i WHERE ` + p.baseWhere, p.baseArgs
	}
	args := make([]any, 0, len(p.driver.cteArgs)+len(p.baseArgs))
	args = append(args, p.driver.cteArgs...)
	args = append(args, p.baseArgs...)
	if isPG {
		return `WITH _drv AS MATERIALIZED (
	` + p.driver.cteSQL + `
)
SELECT COUNT(*)
FROM _drv _d
CROSS JOIN LATERAL (
	SELECT 1
	FROM plugin_items i
	WHERE i.item_ref = _d.item_ref AND ` + p.baseWhere + `
	LIMIT 1
) _m`, args
	}
	return `WITH _drv(item_ref) AS (
	` + p.driver.cteSQL + `
)
SELECT COUNT(*)
FROM _drv _d
CROSS JOIN plugin_items i
WHERE i.item_ref = _d.item_ref AND ` + p.baseWhere, args
}

func (p browseQueryPlan) fullWhereAndArgs() (string, []any) {
	if p.driver == nil || p.driver.existsSQL == "" {
		return p.baseWhere, p.baseArgs
	}
	args := make([]any, 0, len(p.baseArgs)+len(p.driver.existsArgs))
	args = append(args, p.baseArgs...)
	args = append(args, p.driver.existsArgs...)
	return p.baseWhere + p.driver.existsSQL, args
}

func (s *Service) buildBrowsePredicates(
	ctx context.Context,
	q BrowseQuery,
	parentRef, level, col, kind string,
	desc, defaultDesc bool,
	limit int,
	tally browseTallyInfo,
) (baseWhere string, baseArgs []any, viewerID int64, empty bool, err error) {
	plan, err := s.buildBrowsePlan(ctx, q, parentRef, level, col, kind, desc, defaultDesc, limit, tally)
	if err != nil || plan.empty {
		return "", nil, plan.viewerID, plan.empty, err
	}
	w, a := plan.fullWhereAndArgs()
	return w, a, plan.viewerID, false, nil
}

func (s *Service) buildBrowsePlan(
	ctx context.Context,
	q BrowseQuery,
	parentRef, level, col, kind string,
	desc, defaultDesc bool,
	limit int,
	tally browseTallyInfo,
) (browseQueryPlan, error) {
	driveFacetFromSort := shouldDriveFacetFromSortIndex(tally, col, kind, desc, defaultDesc, level, q.Levels, q.AnyLevel, limit)

	var (
		filterWhere     strings.Builder
		filterArgs      []any
		driver          *browseDriver
		driveFromFields bool
	)

	if err := s.appendBrowseEmptyPlan(ctx, q.LibraryID, &filterWhere, &filterArgs, q.Empty, tally.emptyMatchFirst); err != nil {
		return browseQueryPlan{}, err
	}

	isPG := store.DialectOf(s.db) == store.Postgres
	if driveFacetFromSort {
		// Probe the covering PK (item_ref, field, v_text, v_int) (or
		// plugin_item_fields_pkey_idx on PostgreSQL) via correlated EXISTS so
		// SQLite/PostgreSQL walks the sort index and stops after limit+1
		// matches instead of sorting all matching rows (#682).
		appendCorrelatedTextClause(&filterWhere, &filterArgs, tally.positiveTextField, tally.positiveTextValues, false, isPG)
	} else {
		hasAnyFacet := q.PersonID > 0 || hasActiveFacets(q.Facets) || hasActiveFacets(q.AllFacets) || hasActiveFacets(q.ExcludeFacets) || hasActiveAnyGroups(q.AnyGroups)
		if hasAnyFacet {
			if q.PersonID > 0 && !core.IsPluginPersonID(q.PersonID) {
				return browseQueryPlan{empty: true}, nil
			}
			rec, err := s.GetKindByLibrary(ctx, q.LibraryID)
			if err != nil {
				return browseQueryPlan{}, err
			}
			types := make(map[string]string, len(rec.Decl.Fields))
			for _, field := range rec.Decl.Fields {
				types[field.Key] = field.Type
			}
			var peopleFields []string
			if q.PersonID > 0 {
				peopleFields = declaredPeopleFields(rec.Decl)
			}

			clauses, impossible := collectBrowseFieldClauses(q, types, peopleFields)
			if impossible {
				return browseQueryPlan{empty: true}, nil
			}

			driverIdx := -1
			if parentRef == "" {
				var tallyEmpty bool
				driverIdx, tallyEmpty = pickBrowseDriver(clauses, tally)
				if tallyEmpty {
					return browseQueryPlan{empty: true}, nil
				}
			}
			if driverIdx >= 0 {
				dc := clauses[driverIdx]
				cteSQL, cteArgs := buildClauseDriverSQL(dc, isPG)
				var exBuf strings.Builder
				var exArgs []any
				appendClauseExistsSQL(&exBuf, &exArgs, dc, isPG)
				driver = &browseDriver{
					cteSQL:     cteSQL,
					cteArgs:    cteArgs,
					existsSQL:  exBuf.String(),
					existsArgs: exArgs,
				}
				driveFromFields = true
			}
			for idx, c := range clauses {
				if idx == driverIdx {
					continue
				}
				appendBrowseClauseSQL(&filterWhere, &filterArgs, c, types, isPG)
			}
		}
		if !q.Hidden && (q.TabPolicy.HasPositiveViewerFilter() || q.Policy.HasPositiveViewerFilter()) {
			driveFromFields = true
		}
	}

	viewerID := q.ViewerID
	if viewerID <= 0 {
		viewerID = core.CatalogViewerID(ctx)
	}

	// Base filter predicate.
	var bw strings.Builder
	baseArgs := []any{int64(q.LibraryID)}
	if driveFromFields && parentRef == "" && !isPG {
		bw.WriteString(`i.library_id + 0 = ?`)
	} else {
		bw.WriteString(`i.library_id = ?`)
	}
	// When driving a non-sort_title sort from its (library_id, col, item_ref)
	// index on SQLite, keep equality predicates on level/parent_ref from pulling
	// the planner onto plugin_items_browse / plugin_items_level_sort + TEMP B-TREE.
	// On a single-level library where every item already belongs to the requested
	// level, omit the redundant level/parent_ref check so SQLite can scan the
	// sort index as a covering index until EXISTS matches.
	omitSingleLevel := tally.singleLevel && parentRef == "" && col != "sort_title"
	sqliteSortIndexHint := driveFacetFromSort && col != "sort_title" && !isPG
	if parentRef != "" {
		bw.WriteString(` AND i.parent_ref = ?`)
		baseArgs = append(baseArgs, parentRef)
		if len(q.Levels) > 0 {
			placeholders := make([]string, len(q.Levels))
			for idx, lv := range q.Levels {
				placeholders[idx] = "?"
				baseArgs = append(baseArgs, strings.TrimSpace(lv))
			}
			bw.WriteString(` AND i.level IN (` + strings.Join(placeholders, ",") + `)`)
		} else if level != "" {
			bw.WriteString(` AND i.level = ?`)
			baseArgs = append(baseArgs, level)
		}
	} else if !omitSingleLevel {
		if len(q.Levels) > 0 {
			placeholders := make([]string, len(q.Levels))
			for idx, lv := range q.Levels {
				placeholders[idx] = "?"
				baseArgs = append(baseArgs, strings.TrimSpace(lv))
			}
			if sqliteSortIndexHint {
				bw.WriteString(` AND +i.level IN (` + strings.Join(placeholders, ",") + `)`)
			} else {
				bw.WriteString(` AND i.level IN (` + strings.Join(placeholders, ",") + `)`)
			}
		} else if level != "" {
			if sqliteSortIndexHint {
				bw.WriteString(` AND +i.level = ?`)
			} else {
				bw.WriteString(` AND i.level = ?`)
			}
			baseArgs = append(baseArgs, level)
		} else if !q.AnyLevel && q.PersonID <= 0 {
			// Default to root items when neither Level nor ParentRef is specified.
			if sqliteSortIndexHint {
				bw.WriteString(` AND +i.parent_ref = ''`)
			} else {
				bw.WriteString(` AND i.parent_ref = ''`)
			}
		}
	}
	if filterWhere.Len() > 0 {
		bw.WriteString(filterWhere.String())
		baseArgs = append(baseArgs, filterArgs...)
	}
	if q.ReleaseFrom != nil {
		bw.WriteString(` AND i.released > 0 AND i.released >= ?`)
		baseArgs = append(baseArgs, *q.ReleaseFrom)
	}
	if q.MaxContentRating != "" {
		if lvl := contentrating.EnforceLevelOf(q.MaxContentRating); lvl == contentrating.Unrated {
			bw.WriteString(` AND 0=1`)
		} else {
			bw.WriteString(` AND i.rating_level > 0 AND i.rating_level <= ?`)
			baseArgs = append(baseArgs, int(lvl))
		}
	}
	if len(q.ExcludeKeys) > 0 {
		var placeholders []string
		for _, k := range q.ExcludeKeys {
			seq := k.PluginSeq()
			if seq <= 0 {
				continue
			}
			placeholders = append(placeholders, "?")
			baseArgs = append(baseArgs, seq)
		}
		if len(placeholders) > 0 {
			bw.WriteString(` AND i.seq NOT IN (` + strings.Join(placeholders, ",") + `)`)
		}
	}
	if q.Watched != nil {
		switch {
		case viewerID <= 0 && *q.Watched:
			bw.WriteString(` AND 0=1`)
		case viewerID > 0 && *q.Watched:
			bw.WriteString(fmt.Sprintf(` AND EXISTS (SELECT 1 FROM state_watch w WHERE w.user_id = ? AND w.rating_key = %d + i.seq AND w.view_count > 0)`, core.PluginKeyOffset))
			baseArgs = append(baseArgs, viewerID)
		case viewerID > 0:
			bw.WriteString(fmt.Sprintf(` AND NOT EXISTS (SELECT 1 FROM state_watch w WHERE w.user_id = ? AND w.rating_key = %d + i.seq AND w.view_count > 0)`, core.PluginKeyOffset))
			baseArgs = append(baseArgs, viewerID)
		}
	}
	appendBrowseExcludeQuery(&bw, &baseArgs, q.ExcludeQuery, isPG)
	if err := s.appendVisibilityForViewer(ctx, &bw, &baseArgs, q.LibraryID, q.Policy, q.Hidden, viewerID); err != nil {
		return browseQueryPlan{}, err
	}
	if err := s.appendVisibilityForViewer(ctx, &bw, &baseArgs, q.LibraryID, q.TabPolicy, false, viewerID); err != nil {
		return browseQueryPlan{}, err
	}
	var matchSort string
	if len(tally.emptyMatchFirst) > 0 && driver == nil {
		matchSort = col
		if kind == "last_viewed" {
			matchSort = "sort_title"
		}
	}
	return browseQueryPlan{
		matchSort: matchSort,
		driver:    driver,
		baseWhere: bw.String(),
		baseArgs:  baseArgs,
		viewerID:  viewerID,
	}, nil
}

// Browse returns a keyset-paged slice of items in a library (spec §2.3, §3.4).
// Unfiltered totals are read from plugin_item_tally in O(1); filtered or
// child-scoped totals execute an exact COUNT(*) with the same WHERE clauses,
// so a filtered total is never a capped count presented as the total.
func (s *Service) Browse(ctx context.Context, q BrowseQuery) (BrowsePage, error) {
	if s == nil || s.db == nil {
		return BrowsePage{}, fmt.Errorf("pluginitems: nil service")
	}
	if q.LibraryID <= 0 {
		return BrowsePage{}, fmt.Errorf("pluginitems: library_id is required")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultBrowseLimit
	}
	if limit > MaxBrowseLimit {
		limit = MaxBrowseLimit
	}

	col, kind, desc, defaultDesc := parseBrowseSort(q)

	level := strings.TrimSpace(q.Level)
	parentRef := strings.TrimSpace(string(q.ParentRef))
	if err := checkFacetValueBound(q.AnyGroups, q.Facets, q.AllFacets, q.ExcludeFacets); err != nil {
		return BrowsePage{}, err
	}
	var parsedCursor browseCursor
	hasCursor := strings.TrimSpace(q.Cursor) != ""
	if hasCursor && kind != "last_viewed" {
		var err error
		parsedCursor, err = decodeBrowseCursor(q.Cursor)
		if err != nil {
			return BrowsePage{}, err
		}
	}
	hasFacets := hasActiveFacets(q.Facets) || hasActiveFacets(q.AllFacets) || hasActiveFacets(q.ExcludeFacets) || hasActiveAnyGroups(q.AnyGroups) || len(q.Empty) > 0 || hasActiveExcludeQuery(q.ExcludeQuery) || !q.Policy.Passive() || !q.TabPolicy.Passive() || q.Hidden || q.PersonID > 0 || len(q.Levels) > 0 || q.AnyLevel || q.Watched != nil || q.MaxContentRating != "" || len(q.ExcludeKeys) > 0 || q.ReleaseFrom != nil

	// Probe O(1) item/field tallies before building the page query so single
	// tallied facets answer totals without COUNT(*), empty buckets return
	// immediately, and large facets can drive from the sort index (#682).
	tally, err := s.browseTallyProbe(ctx, q, parentRef, level)
	if err != nil {
		return BrowsePage{}, err
	}
	if (tally.totalExact && tally.total == 0) || tally.knownEmpty {
		if q.NoTotal && !q.CheapTotal {
			return BrowsePage{Items: []core.KindItem{}, Total: 0, TotalExact: false}, nil
		}
		return BrowsePage{Items: []core.KindItem{}, Total: 0, TotalExact: true}, nil
	}
	if q.ReleaseFrom != nil {
		tally.totalExact = false
	}

	plan, err := s.buildBrowsePlan(ctx, q, parentRef, level, col, kind, desc, defaultDesc, limit, tally)
	if err != nil {
		return BrowsePage{}, err
	}
	if plan.empty {
		if q.NoTotal && !q.CheapTotal {
			return BrowsePage{Items: []core.KindItem{}, Total: 0, TotalExact: false}, nil
		}
		return BrowsePage{Items: []core.KindItem{}, Total: 0, TotalExact: true}, nil
	}

	if kind == "last_viewed" {
		return s.browseLastViewed(ctx, q, plan, hasFacets, parentRef, level, tally, desc, limit)
	}

	pageWhere := plan.baseWhere
	pageArgs := append([]any(nil), plan.baseArgs...)

	if hasCursor {
		cur := parsedCursor
		var curVal any
		switch kind {
		case "str":
			curVal = cur.StrVal
		case "int":
			curVal = cur.IntVal
		case "float":
			curVal = cur.FloatVal
		}
		if !desc {
			// Range-friendly keyset predicate: i.<col> >= ? AND (i.<col> > ? OR i.item_ref > ?)
			pageWhere += fmt.Sprintf(` AND (i.%s >= ? AND (i.%s > ? OR i.item_ref > ?))`, col, col)
			pageArgs = append(pageArgs, curVal, curVal, cur.ItemRef)
		} else {
			// Range-friendly DESC predicate with ASC item_ref tie-breaker:
			// i.<col> <= ? AND (i.<col> < ? OR i.item_ref > ?)
			pageWhere += fmt.Sprintf(` AND (i.%s <= ? AND (i.%s < ? OR i.item_ref > ?))`, col, col)
			pageArgs = append(pageArgs, curVal, curVal, cur.ItemRef)
		}
	}

	dir := "ASC"
	if desc {
		dir = "DESC"
	}
	orderBy := fmt.Sprintf("i.%s %s, i.item_ref ASC", col, dir)
	isPG := store.DialectOf(s.db) == store.Postgres
	querySQL, queryArgs := plan.pageSQLAndArgs(isPG, prefixedItemCols("i."), pageWhere, pageArgs, orderBy, limit+1)

	items, err := s.queryItems(ctx, querySQL, queryArgs, false)
	if err != nil {
		return BrowsePage{}, err
	}

	var nextCursor string
	if len(items) > limit {
		last := items[limit-1]
		items = items[:limit]
		cur := browseCursor{
			Sort:    col,
			Desc:    desc,
			ItemRef: string(last.Ref),
		}
		switch col {
		case "sort_title":
			cur.StrVal = last.SortTitle
		case "added_at":
			cur.IntVal = last.AddedAt
		case "released":
			cur.IntVal = last.Released
		case "year":
			cur.IntVal = int64(last.Year)
		case "idx":
			cur.IntVal = int64(last.Index)
		case "rating":
			cur.FloatVal = last.Rating
		}
		nextCursor = encodeBrowseCursor(cur)
	}

	if len(items) > 0 {
		if err := s.hydrateArtSlice(ctx, items); err != nil {
			return BrowsePage{}, err
		}
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

// largeFacetMinMatches is the minimum tallied match count before Browse drives
// a single positive text facet from the sort index rather than fetching and
// sorting every matching item row (#682). Package var for unit tests.
var largeFacetMinMatches = 1000

type browseTextTallyKey struct {
	field string
	val   string
}

type browsePersonTallyKey struct {
	field string
	pid   int64
}

type browseTallyInfo struct {
	emptyMatchFirst    map[string]bool
	totalExact         bool
	total              int
	knownEmpty         bool
	lowerBound         int
	libTotal           int
	singleLevel        bool
	positiveTextField  string
	positiveTextValues []string

	// Positive facet cardinality maps populated when libTally.active &&
	// parentRef == "" (including multi-facet and filtered queries) so
	// buildBrowsePlan picks the narrowest positive facet as driver in O(1).
	talliesActive bool
	textCounts    map[browseTextTallyKey]int
	personCounts  map[browsePersonTallyKey]int
}

func isTalliedTextType(ftype string) bool {
	switch ftype {
	case "string", "enum", "bool", "strings":
		return true
	default:
		return false
	}
}

func isSingleValuedTextType(ftype string) bool {
	switch ftype {
	case "string", "enum", "bool":
		return true
	default:
		return false
	}
}

func shouldDriveFacetFromSortIndex(tally browseTallyInfo, col, kind string, desc, defaultDesc bool, level string, levels []string, anyLevel bool, limit int) bool {
	if tally.positiveTextField == "" || kind == "last_viewed" {
		return false
	}
	var m int64
	switch {
	case tally.totalExact:
		m = int64(tally.total)
	case tally.lowerBound > 0:
		m = int64(tally.lowerBound)
	default:
		return false
	}
	if desc != defaultDesc {
		return false
	}
	if col == "sort_title" && level == "" && (anyLevel || len(levels) > 0) {
		// plugin_items_browse covers parent_ref='' and plugin_items_level_sort
		// covers level=?; AnyLevel / multi-level sort_title lacks item_ref order.
		return false
	}
	if m < int64(largeFacetMinMatches) {
		return false
	}
	n := int64(tally.libTotal)
	if n <= 0 {
		return false
	}
	return m*m >= n*int64(limit+1)*8
}

func dedupCleanValues(vals []string) []string {
	var out []string
	var seen map[string]struct{}
	for _, raw := range vals {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if len(out) == 0 {
			out = append(out, v)
			continue
		}
		if len(out) == 1 {
			if out[0] == v {
				continue
			}
			out = append(out, v)
			continue
		}
		if seen == nil {
			seen = make(map[string]struct{}, len(vals))
			for _, existing := range out {
				seen[existing] = struct{}{}
			}
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

type activeBrowseFacet struct {
	mode facetMode
	key  string
	vals []string
}

func collectActiveBrowseFacets(q BrowseQuery) (active []activeBrowseFacet, hasDisjunction bool) {
	collect := func(m map[string][]string, mode facetMode) {
		for _, k := range sortedFacetKeys(m) {
			vals := dedupCleanValues(m[k])
			if len(vals) == 0 {
				continue
			}
			active = append(active, activeBrowseFacet{mode: mode, key: k, vals: vals})
		}
	}
	collect(q.Facets, facetAny)
	collect(q.AllFacets, facetAll)
	collect(q.ExcludeFacets, facetNone)
	for _, g := range q.AnyGroups {
		alts := activeAnyGroupAlts(g)
		switch len(alts) {
		case 0:
		case 1:
			active = append(active, activeBrowseFacet{mode: facetAny, key: alts[0].Field, vals: dedupCleanValues(alts[0].Values)})
		default:
			hasDisjunction = true
		}
	}
	return active, hasDisjunction
}

func (s *Service) queryFieldTallyCounts(ctx context.Context, libraryID core.LibraryID, tallyLevels []string, fieldVals map[string][]string) (map[browseTextTallyKey]int, error) {
	if len(fieldVals) == 0 {
		return nil, nil
	}
	out := make(map[browseTextTallyKey]int)
	fields := make([]string, 0, len(fieldVals))
	for fk := range fieldVals {
		fields = append(fields, fk)
	}
	sort.Strings(fields)
	isPG := store.DialectOf(s.db) == store.Postgres
	for _, fk := range fields {
		vals := dedupCleanValues(fieldVals[fk])
		if len(vals) == 0 {
			continue
		}
		var sqlBuf strings.Builder
		args := make([]any, 0, 3+len(tallyLevels)+2*len(vals))
		if len(tallyLevels) == 1 {
			sqlBuf.WriteString(`SELECT v_text, cnt FROM plugin_item_field_tally WHERE library_id = ? AND level = ? AND field = ?`)
			args = append(args, int64(libraryID), tallyLevels[0], fk)
		} else {
			lvPh := make([]string, len(tallyLevels))
			args = append(args, int64(libraryID))
			for i, lv := range tallyLevels {
				lvPh[i] = "?"
				args = append(args, lv)
			}
			args = append(args, fk)
			sqlBuf.WriteString(`SELECT v_text, SUM(cnt) FROM plugin_item_field_tally WHERE library_id = ? AND level IN (` + strings.Join(lvPh, ",") + `) AND field = ?`)
		}
		if isPG {
			appendPGMD5In(&sqlBuf, &args, "v_text", vals)
		} else {
			vPh := make([]string, len(vals))
			for i, v := range vals {
				vPh[i] = "?"
				args = append(args, v)
			}
			sqlBuf.WriteString(` AND v_text IN (` + strings.Join(vPh, ",") + `)`)
		}
		if len(tallyLevels) > 1 {
			sqlBuf.WriteString(` GROUP BY v_text`)
		}
		rows, err := s.db.QueryContext(ctx, sqlBuf.String(), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var v string
			var cnt int
			if err := rows.Scan(&v, &cnt); err != nil {
				rows.Close()
				return nil, err
			}
			out[browseTextTallyKey{field: fk, val: v}] = cnt
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func (s *Service) queryPersonTallyCounts(ctx context.Context, libraryID core.LibraryID, tallyLevels []string, fieldPIDs map[string][]int64) (map[browsePersonTallyKey]int, error) {
	if len(fieldPIDs) == 0 {
		return nil, nil
	}
	out := make(map[browsePersonTallyKey]int)
	fields := make([]string, 0, len(fieldPIDs))
	for fk := range fieldPIDs {
		fields = append(fields, fk)
	}
	sort.Strings(fields)
	for _, fk := range fields {
		pids := fieldPIDs[fk]
		if len(pids) == 0 {
			continue
		}
		var sqlBuf strings.Builder
		args := make([]any, 0, 3+len(tallyLevels)+len(pids))
		if len(tallyLevels) == 1 {
			sqlBuf.WriteString(`SELECT v_int, cnt FROM plugin_item_person_tally WHERE library_id = ? AND level = ? AND field = ?`)
			args = append(args, int64(libraryID), tallyLevels[0], fk)
		} else {
			lvPh := make([]string, len(tallyLevels))
			args = append(args, int64(libraryID))
			for i, lv := range tallyLevels {
				lvPh[i] = "?"
				args = append(args, lv)
			}
			args = append(args, fk)
			sqlBuf.WriteString(`SELECT v_int, SUM(cnt) FROM plugin_item_person_tally WHERE library_id = ? AND level IN (` + strings.Join(lvPh, ",") + `) AND field = ?`)
		}
		pPh := make([]string, len(pids))
		for i, pid := range pids {
			pPh[i] = "?"
			args = append(args, pid)
		}
		sqlBuf.WriteString(` AND v_int IN (` + strings.Join(pPh, ",") + `)`)
		if len(tallyLevels) > 1 {
			sqlBuf.WriteString(` GROUP BY v_int`)
		}
		rows, err := s.db.QueryContext(ctx, sqlBuf.String(), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var pid int64
			var cnt int
			if err := rows.Scan(&pid, &cnt); err != nil {
				rows.Close()
				return nil, err
			}
			out[browsePersonTallyKey{field: fk, pid: pid}] = cnt
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func (s *Service) browseTallyProbe(ctx context.Context, q BrowseQuery, parentRef, level string) (browseTallyInfo, error) {
	if len(q.Empty) > 0 {
		return s.probeBrowsePresence(ctx, q, parentRef, level)
	}
	if hasActiveExcludeQuery(q.ExcludeQuery) {
		return browseTallyInfo{}, nil
	}
	noExtraFilters := q.Policy.Passive() && q.TabPolicy.Passive() && !q.Hidden && q.PersonID <= 0 && q.Watched == nil && q.MaxContentRating == "" && len(q.ExcludeKeys) == 0
	if parentRef != "" {
		if !noExtraFilters || hasActiveFacets(q.Facets) || hasActiveFacets(q.AllFacets) || hasActiveFacets(q.ExcludeFacets) || hasActiveAnyGroups(q.AnyGroups) {
			return browseTallyInfo{}, nil
		}
		var pLevels []string
		switch {
		case len(q.Levels) > 0:
			pLevels = dedupCleanValues(q.Levels)
			if len(pLevels) == 0 {
				return browseTallyInfo{}, nil
			}
		case level != "":
			pLevels = []string{level}
		}
		var n int
		var err error
		if len(pLevels) == 0 {
			err = s.db.QueryRowContext(ctx,
				`SELECT COALESCE(SUM(child_count), 0) FROM plugin_item_parent_tally WHERE library_id = ? AND parent_ref = ?`,
				int64(q.LibraryID), parentRef).Scan(&n)
		} else if len(pLevels) == 1 {
			err = s.db.QueryRowContext(ctx,
				`SELECT COALESCE(child_count, 0) FROM plugin_item_parent_tally WHERE library_id = ? AND parent_ref = ? AND child_level = ?`,
				int64(q.LibraryID), parentRef, pLevels[0]).Scan(&n)
			if errors.Is(err, sql.ErrNoRows) {
				n, err = 0, nil
			}
		} else {
			placeholders := make([]string, len(pLevels))
			args := make([]any, 0, len(pLevels)+2)
			args = append(args, int64(q.LibraryID), parentRef)
			for i, lv := range pLevels {
				placeholders[i] = "?"
				args = append(args, lv)
			}
			err = s.db.QueryRowContext(ctx,
				`SELECT COALESCE(SUM(child_count), 0) FROM plugin_item_parent_tally WHERE library_id = ? AND parent_ref = ? AND child_level IN (`+strings.Join(placeholders, ",")+`)`,
				args...).Scan(&n)
		}
		if err != nil {
			return browseTallyInfo{}, err
		}
		if n == 0 {
			// Guard against raw-SQL test fixtures that seeded plugin_items without parent tallies.
			var hasAny bool
			var probeErr error
			if len(pLevels) == 1 {
				probeErr = s.db.QueryRowContext(ctx,
					`SELECT EXISTS(SELECT 1 FROM plugin_items WHERE library_id = ? AND parent_ref = ? AND level = ?)`,
					int64(q.LibraryID), parentRef, pLevels[0]).Scan(&hasAny)
			} else {
				probeErr = s.db.QueryRowContext(ctx,
					`SELECT EXISTS(SELECT 1 FROM plugin_items WHERE library_id = ? AND parent_ref = ?)`,
					int64(q.LibraryID), parentRef).Scan(&hasAny)
			}
			if probeErr != nil {
				return browseTallyInfo{}, probeErr
			}
			if hasAny {
				return browseTallyInfo{}, nil
			}
		}
		return browseTallyInfo{total: n, totalExact: true}, nil
	}

	var (
		levels        []string
		anyLevel      bool
		defaultedRoot bool
		kindRec       KindRecord
		hasKind       bool
	)
	switch {
	case len(q.Levels) > 0:
		levels = dedupCleanValues(q.Levels)
		if len(levels) == 0 {
			return browseTallyInfo{}, nil
		}
	case level != "":
		levels = []string{level}
	case q.AnyLevel || q.PersonID > 0:
		anyLevel = true
	default:
		rec, err := s.GetKindByLibrary(ctx, q.LibraryID)
		if err != nil || len(rec.Decl.Levels) == 0 {
			return browseTallyInfo{}, nil
		}
		kindRec = rec
		hasKind = true
		defaultedRoot = true
		levels = []string{rec.Decl.Levels[0].Key}
	}

	libTally, err := s.libraryTally(ctx, q.LibraryID)
	if err != nil {
		return browseTallyInfo{}, err
	}
	if !libTally.active {
		return browseTallyInfo{}, nil
	}
	libTotal := libTally.libTotal

	levelTotal := libTotal
	if !anyLevel {
		levelTotal = 0
		for _, lv := range levels {
			levelTotal += libTally.levelCounts[lv]
		}
	}
	singleLevel := libTotal > 0 && levelTotal == libTotal && (!defaultedRoot || len(kindRec.Decl.Levels) == 1)

	active, hasDisjunction := collectActiveBrowseFacets(q)
	noExtraFilters = noExtraFilters && !hasDisjunction
	if len(active) == 0 && q.PersonID <= 0 {
		if noExtraFilters {
			return browseTallyInfo{totalExact: true, total: levelTotal, libTotal: libTotal, singleLevel: singleLevel}, nil
		}
		return browseTallyInfo{libTotal: libTotal, singleLevel: singleLevel}, nil
	}

	if !hasKind {
		rec, err := s.GetKindByLibrary(ctx, q.LibraryID)
		if err != nil {
			return browseTallyInfo{libTotal: libTotal, singleLevel: singleLevel}, nil
		}
		kindRec = rec
	}
	fieldTypes := make(map[string]string, len(kindRec.Decl.Fields))
	for _, fd := range kindRec.Decl.Fields {
		fieldTypes[fd.Key] = fd.Type
	}

	tallyLevels := levels
	if anyLevel {
		tallyLevels = []string{""}
	}

	// Single-facet exact-total + #682 sort-index fast path when no extra
	// filters apply.
	if noExtraFilters && len(active) == 1 {
		f := active[0]
		if !envelopeFacet(f.key) && isTalliedTextType(fieldTypes[f.key]) {
			singleValued := isSingleValuedTextType(fieldTypes[f.key])
			if singleValued && f.mode == facetAll && len(f.vals) > 1 {
				return browseTallyInfo{totalExact: true, total: 0, knownEmpty: true, libTotal: libTotal, singleLevel: singleLevel}, nil
			}
			if singleValued || len(f.vals) == 1 || f.mode == facetAny {
				counts, err := s.queryFieldTallyCounts(ctx, q.LibraryID, tallyLevels, map[string][]string{f.key: f.vals})
				if err != nil {
					return browseTallyInfo{}, err
				}
				var sumCnt, maxCnt int
				for _, v := range f.vals {
					c := counts[browseTextTallyKey{field: f.key, val: v}]
					sumCnt += c
					if c > maxCnt {
						maxCnt = c
					}
				}
				multiOrMultiVal := !singleValued && len(f.vals) > 1 && f.mode == facetAny
				if multiOrMultiVal {
					if sumCnt == 0 {
						return browseTallyInfo{totalExact: true, total: 0, knownEmpty: true, libTotal: libTotal, singleLevel: singleLevel}, nil
					}
					return browseTallyInfo{
						lowerBound:         maxCnt,
						libTotal:           libTotal,
						singleLevel:        singleLevel,
						positiveTextField:  f.key,
						positiveTextValues: f.vals,
						talliesActive:      true,
						textCounts:         counts,
					}, nil
				}
				if f.mode == facetNone {
					return browseTallyInfo{
						totalExact:    true,
						total:         max(0, levelTotal-sumCnt),
						libTotal:      libTotal,
						singleLevel:   singleLevel,
						talliesActive: true,
						textCounts:    counts,
					}, nil
				}
				return browseTallyInfo{
					totalExact:         true,
					total:              sumCnt,
					knownEmpty:         sumCnt == 0,
					libTotal:           libTotal,
					singleLevel:        singleLevel,
					positiveTextField:  f.key,
					positiveTextValues: f.vals,
					talliesActive:      true,
					textCounts:         counts,
				}, nil
			}
		}
	}

	// Multi-facet, multi-value AllFacets, people, or extra-filtered queries:
	// fetch O(1) positive facet tallies so buildBrowsePlan chooses the
	// narrowest positive facet as the driver and probes the rest via EXISTS.
	textFields := map[string][]string{}
	peopleFields := map[string][]int64{}
	for _, a := range active {
		if a.mode == facetNone || envelopeFacet(a.key) {
			continue
		}
		ft := fieldTypes[a.key]
		if isSingleValuedTextType(ft) && a.mode == facetAll && len(a.vals) > 1 {
			return browseTallyInfo{knownEmpty: true, totalExact: true, total: 0, libTotal: libTotal, singleLevel: singleLevel}, nil
		}
		if isTalliedTextType(ft) {
			textFields[a.key] = append(textFields[a.key], a.vals...)
			continue
		}
		if ft == "people" {
			pids := parsePeopleFacetIDs(a.vals)
			if len(pids) == 0 {
				return browseTallyInfo{knownEmpty: true, totalExact: true, total: 0, libTotal: libTotal, singleLevel: singleLevel}, nil
			}
			peopleFields[a.key] = appendUniqueInt64s(peopleFields[a.key], pids)
		}
	}
	if q.PersonID > 0 {
		if !core.IsPluginPersonID(q.PersonID) {
			return browseTallyInfo{knownEmpty: true, totalExact: true, total: 0, libTotal: libTotal, singleLevel: singleLevel}, nil
		}
		pFields := declaredPeopleFields(kindRec.Decl)
		if len(pFields) == 0 {
			return browseTallyInfo{knownEmpty: true, totalExact: true, total: 0, libTotal: libTotal, singleLevel: singleLevel}, nil
		}
		for _, fk := range pFields {
			peopleFields[fk] = appendUniqueInt64s(peopleFields[fk], []int64{q.PersonID})
		}
	}

	textCounts, err := s.queryFieldTallyCounts(ctx, q.LibraryID, tallyLevels, textFields)
	if err != nil {
		return browseTallyInfo{}, err
	}
	personCounts, err := s.queryPersonTallyCounts(ctx, q.LibraryID, tallyLevels, peopleFields)
	if err != nil {
		return browseTallyInfo{}, err
	}

	// If any positive tallied text clause has zero items in this library/level,
	// the intersection is guaranteed empty without scanning plugin_items.
	for _, a := range active {
		if a.mode == facetNone || envelopeFacet(a.key) || !isTalliedTextType(fieldTypes[a.key]) {
			continue
		}
		if a.mode == facetAll {
			for _, v := range a.vals {
				if textCounts[browseTextTallyKey{field: a.key, val: v}] == 0 {
					return browseTallyInfo{knownEmpty: true, totalExact: true, total: 0, libTotal: libTotal, singleLevel: singleLevel}, nil
				}
			}
			continue
		}
		sumCnt := 0
		for _, v := range a.vals {
			sumCnt += textCounts[browseTextTallyKey{field: a.key, val: v}]
		}
		if sumCnt == 0 {
			return browseTallyInfo{knownEmpty: true, totalExact: true, total: 0, libTotal: libTotal, singleLevel: singleLevel}, nil
		}
	}

	return browseTallyInfo{
		libTotal:      libTotal,
		singleLevel:   singleLevel,
		talliesActive: true,
		textCounts:    textCounts,
		personCounts:  personCounts,
	}, nil
}

func (s *Service) resolveBrowseTotal(
	ctx context.Context,
	q BrowseQuery,
	plan browseQueryPlan,
	hasFacets bool,
	parentRef string,
	tally browseTallyInfo,
	singlePage bool,
	itemCount int,
) (int, bool, error) {
	if q.NoTotal && !q.CheapTotal {
		return itemCount, false, nil
	}
	if tally.totalExact {
		return tally.total, true, nil
	}
	if singlePage {
		return itemCount, true, nil
	}

	isPG := store.DialectOf(s.db) == store.Postgres
	countSQL, countArgs := plan.countSQLAndArgs(isPG)

	cacheable := q.Watched == nil && !strings.Contains(plan.baseWhere, "state_watch")
	var (
		ckey   string
		gen    uint64
		minute int64
	)
	if cacheable {
		gen = s.writeGen.Load()
		minute = s.countMinuteBucket()
		ckey = browseCountCacheKey("browse", countSQL, countArgs)
		if cached, ok := s.countCacheGet(ckey, gen, minute); ok {
			return cached, true, nil
		}
	}

	if q.NoTotal && q.CheapTotal {
		// Allow an exact child count for a single parent; avoid scanning a full
		// filtered plugin library.
		if parentRef == "" || hasFacets {
			return itemCount, false, nil
		}
	}

	var total int
	if err := s.db.QueryRowContext(ctx, countSQL, countArgs...).Scan(&total); err != nil {
		return 0, false, err
	}
	if cacheable {
		s.countCachePut(ckey, total, gen, minute)
	}
	return total, true, nil
}

const smallPluginLibraryMax = 5000

func (s *Service) libraryHasAtMostItems(ctx context.Context, itemWhere string, itemArgs []any, maxItems int) (bool, error) {
	qArgs := append(append([]any(nil), itemArgs...), maxItems+1)
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (SELECT 1 FROM plugin_items i WHERE `+itemWhere+` LIMIT ?) s`,
		qArgs...).Scan(&n); err != nil {
		return false, err
	}
	return n <= maxItems, nil
}

// FacetPage is one field's filter values in display order. Truncated means
// the field has more distinct values than the limit returned.
type FacetPage struct {
	Values    []FacetCount `json:"values"`
	Truncated bool         `json:"truncated,omitempty"`
}

// Facets lists a field's values. The query keeps the most common values when
// a field has more than limit; the page is then ordered for reading: an enum
// in its declared order, text values alphabetically, numbers by count.
func (s *Service) Facets(ctx context.Context, libraryID core.LibraryID, level, fieldKey string, limit int) (FacetPage, error) {
	if s == nil || s.db == nil {
		return FacetPage{}, fmt.Errorf("pluginitems: nil service")
	}
	fieldKey = strings.TrimSpace(fieldKey)
	if libraryID <= 0 || fieldKey == "" {
		return FacetPage{}, fmt.Errorf("pluginitems: library_id and field_key are required")
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	fetch := limit + 1

	var (
		query string
		args  []any
	)
	if envelopeFacet(fieldKey) {
		query = `SELECT CAST(i.` + fieldKey + ` AS TEXT), COUNT(*) AS cnt FROM plugin_items i WHERE i.library_id = ? AND i.` + fieldKey + ` > 0`
		args = []any{int64(libraryID)}
		if strings.TrimSpace(level) != "" {
			query += ` AND i.level = ?`
			args = append(args, strings.TrimSpace(level))
		}
		query += ` GROUP BY i.` + fieldKey + ` ORDER BY cnt DESC, i.` + fieldKey + ` ASC LIMIT ?`
		args = append(args, fetch)
	} else {
		query = `
			SELECT v_text, cnt
			FROM plugin_item_field_tally
			WHERE library_id = ? AND level = ? AND field = ? AND cnt > 0
			ORDER BY cnt DESC, v_text ASC
			LIMIT ?`
		args = []any{int64(libraryID), strings.TrimSpace(level), fieldKey, fetch}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return FacetPage{}, err
	}
	defer rows.Close()

	var out []FacetCount
	for rows.Next() {
		var fc FacetCount
		if err := rows.Scan(&fc.Value, &fc.Count); err != nil {
			return FacetPage{}, err
		}
		out = append(out, fc)
	}
	if err := rows.Err(); err != nil {
		return FacetPage{}, err
	}
	page := FacetPage{Values: out}
	if len(out) > limit {
		page.Values, page.Truncated = out[:limit], true
	}
	if !envelopeFacet(fieldKey) {
		if rec, err := s.GetKindByLibrary(ctx, libraryID); err == nil {
			for _, f := range rec.Decl.Fields {
				if f.Key == fieldKey {
					orderFacetValues(page.Values, f.Type, f.Enum)
					break
				}
			}
		}
	}
	return page, nil
}

// FacetBuckets returns discrete filter buckets for a declared or envelope facet
// field on libraryID, respecting library visibility policy:
//   - date (and envelope released): UTC years ("2024"), newest year first
//   - <date>-decade (and envelope decade): UTC decades ("1990" / "1990s"), newest first
//   - duration: discrete duration ranges ("Under 10 min", ...), omitting empty
//   - int (and envelope year): distinct values when <= 50, else clean ranges
//   - float (and envelope rating): distinct values when <= 50, else clean ranges
//   - enum: declared order
//   - string, strings, bool, people: alphabetical order
func (s *Service) FacetBuckets(ctx context.Context, libraryID core.LibraryID, level string, policy *libraryfilter.Filter, fieldKey string) ([]FacetBucket, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("pluginitems: nil service")
	}
	fieldKey = strings.TrimSpace(fieldKey)
	if libraryID <= 0 || fieldKey == "" {
		return nil, fmt.Errorf("pluginitems: library_id and field_key are required")
	}
	rec, err := s.GetKindByLibrary(ctx, libraryID)
	if err != nil {
		return nil, err
	}

	itemWhere, fieldDrivenWhere, itemArgs, err := s.buildRootItemPredicates(ctx, rec, level, policy)
	if err != nil {
		return nil, err
	}
	targetLevel := effectiveRootLevel(rec, level)
	var libTally libraryTallyState
	if policy.Passive() {
		libTally, err = s.libraryTally(ctx, libraryID)
		if err != nil {
			return nil, err
		}
	}
	var smallLib bool
	if policy.Passive() && libTally.active {
		smallLib = libTally.countFor(targetLevel) <= smallPluginLibraryMax
	} else {
		smallLib, err = s.libraryHasAtMostItems(ctx, itemWhere, itemArgs, smallPluginLibraryMax)
		if err != nil {
			return nil, err
		}
	}

	fieldsByKey := make(map[string]struct {
		typ  string
		enum []string
	}, len(rec.Decl.Fields))
	for _, fd := range rec.Decl.Fields {
		fieldsByKey[fd.Key] = struct {
			typ  string
			enum []string
		}{typ: fd.Type, enum: fd.Enum}
	}

	if fieldKey == "decade" {
		if _, hasDeclared := fieldsByKey["decade"]; !hasDeclared {
			years, err := s.scanEnvelopeYearCounts(ctx, libraryID, itemWhere, itemArgs, smallLib)
			if err != nil {
				return nil, err
			}
			return bucketYearDecades(years), nil
		}
	}

	if base, ok := libraryfilter.SplitDateDecadeFacet(fieldKey); ok {
		if fd, exists := fieldsByKey[base]; exists && fd.typ == "date" {
			return s.scanDateFieldDecadeBuckets(ctx, base, itemWhere, fieldDrivenWhere, itemArgs, smallLib)
		}
	}

	switch fieldKey {
	case "year":
		vals, err := s.scanEnvelopeYearCounts(ctx, libraryID, itemWhere, itemArgs, smallLib)
		if err != nil {
			return nil, err
		}
		return bucketIntValues(vals, true), nil
	case "released":
		return s.scanEnvelopeReleasedBuckets(ctx, libraryID, itemWhere, itemArgs, smallLib)
	case "rating":
		vals, err := s.scanEnvelopeRatingCounts(ctx, libraryID, itemWhere, itemArgs, smallLib)
		if err != nil {
			return nil, err
		}
		return bucketFloatValues(vals), nil
	}

	fd, found := fieldsByKey[fieldKey]
	if !found {
		return []FacetBucket{}, nil
	}
	fieldType := fd.typ
	enumVals := fd.enum

	args := append(append([]any(nil), itemArgs...), fieldKey)
	switch fieldType {
	case "date":
		return s.scanDateFieldYearBuckets(ctx, fieldKey, itemWhere, fieldDrivenWhere, itemArgs, smallLib)
	case "duration":
		if smallLib {
			vals, err := s.scanIntFacetCounts(ctx, `SELECT f.v_int, COUNT(*) FROM plugin_item_fields f JOIN plugin_items i ON i.item_ref = f.item_ref WHERE `+itemWhere+` AND f.field = ? AND (f.v_text = '' OR f.v_int <> 0) AND f.v_int >= 0 GROUP BY f.v_int`, args)
			if err != nil {
				return nil, err
			}
			return bucketDurationValues(vals), nil
		}
		return s.scanDurationBucketsLarge(ctx, fieldKey, fieldDrivenWhere, itemArgs)
	case "int":
		vals, err := s.scanFieldIntCounts(ctx, fieldKey, itemWhere, fieldDrivenWhere, itemArgs, smallLib)
		if err != nil {
			return nil, err
		}
		return bucketIntValues(vals, false), nil
	case "float":
		vals, err := s.scanFieldFloatCounts(ctx, fieldKey, itemWhere, fieldDrivenWhere, itemArgs, smallLib)
		if err != nil {
			return nil, err
		}
		return bucketFloatValues(vals), nil
	case "people":
		if policy.Passive() && libTally.active {
			if libTally.countFor(targetLevel) == 0 {
				return []FacetBucket{}, nil
			}
			orderBy := `cnt DESC, label_lower ASC, label ASC, v_int ASC`
			if store.DialectOf(s.db) == store.Postgres {
				orderBy = `cnt DESC, left(label_lower, 256) ASC, label_lower ASC, label ASC, v_int ASC`
			}
			qSQL := `SELECT label, cnt FROM plugin_item_person_tally WHERE library_id = ? AND level = ? AND field = ? ORDER BY ` + orderBy
			qArgs := []any{int64(libraryID), targetLevel, fieldKey}
			if !smallLib {
				qSQL += ` LIMIT 400`
			}
			rows, err := s.db.QueryContext(ctx, qSQL, qArgs...)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var out []FacetBucket
			for rows.Next() {
				var name string
				var cnt int
				if err := rows.Scan(&name, &cnt); err != nil {
					return nil, err
				}
				name = strings.TrimSpace(name)
				if name != "" && cnt > 0 {
					out = append(out, FacetBucket{ID: name, Label: name, Count: cnt})
				}
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
			sort.SliceStable(out, func(i, j int) bool { return facetNameLess(out[i].Label, out[j].Label) })
			if out == nil {
				return []FacetBucket{}, nil
			}
			return out, nil
		}
		if !smallLib {
			return s.scanPeopleBucketsLarge(ctx, fieldKey, fieldDrivenWhere, itemArgs)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT MIN(f.v_text), COUNT(*) FROM plugin_item_fields f JOIN plugin_items i ON i.item_ref = f.item_ref WHERE `+itemWhere+` AND f.field = ? AND f.v_int <> 0 AND f.v_int > 0 AND f.v_text <> '' GROUP BY f.v_int`, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []FacetBucket
		for rows.Next() {
			var name string
			var cnt int
			if err := rows.Scan(&name, &cnt); err != nil {
				return nil, err
			}
			name = strings.TrimSpace(name)
			if name != "" && cnt > 0 {
				out = append(out, FacetBucket{ID: name, Label: name, Count: cnt})
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		sort.SliceStable(out, func(i, j int) bool { return facetNameLess(out[i].Label, out[j].Label) })
		if out == nil {
			return []FacetBucket{}, nil
		}
		return out, nil
	default:
		if policy.Passive() && libTally.active {
			if libTally.countFor(targetLevel) == 0 {
				return []FacetBucket{}, nil
			}
			orderBy := `cnt DESC, v_lower ASC, v_text ASC`
			if store.DialectOf(s.db) == store.Postgres {
				orderBy = `cnt DESC, left(v_lower, 256) ASC, v_lower ASC, v_text ASC`
			}
			qSQL := `SELECT v_text, cnt FROM plugin_item_field_tally WHERE library_id = ? AND level = ? AND field = ? ORDER BY ` + orderBy
			qArgs := []any{int64(libraryID), targetLevel, fieldKey}
			if !smallLib {
				qSQL += ` LIMIT 400`
			}
			rows, err := s.db.QueryContext(ctx, qSQL, qArgs...)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var raw []FacetCount
			for rows.Next() {
				var v string
				var cnt int
				if err := rows.Scan(&v, &cnt); err != nil {
					return nil, err
				}
				if v != "" && cnt > 0 {
					raw = append(raw, FacetCount{Value: v, Count: cnt})
				}
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
			orderFacetValues(raw, fieldType, enumVals)
			out := make([]FacetBucket, 0, len(raw))
			for _, fc := range raw {
				out = append(out, FacetBucket{ID: fc.Value, Label: fc.Value, Count: fc.Count})
			}
			return out, nil
		}
		if !smallLib {
			raw, err := s.scanFieldTextCountsLarge(ctx, fieldKey, fieldDrivenWhere, itemArgs, 2001)
			if err != nil {
				return nil, err
			}
			orderFacetValues(raw, fieldType, enumVals)
			out := make([]FacetBucket, 0, len(raw))
			for _, fc := range raw {
				out = append(out, FacetBucket{ID: fc.Value, Label: fc.Value, Count: fc.Count})
			}
			return out, nil
		}
		rows, err := s.db.QueryContext(ctx, `SELECT f.v_text, COUNT(*) FROM plugin_item_fields f JOIN plugin_items i ON i.item_ref = f.item_ref WHERE `+itemWhere+` AND f.field = ? AND f.v_text <> '' GROUP BY f.v_text`, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var raw []FacetCount
		for rows.Next() {
			var fc FacetCount
			if err := rows.Scan(&fc.Value, &fc.Count); err != nil {
				return nil, err
			}
			if fc.Value != "" && fc.Count > 0 {
				raw = append(raw, fc)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		orderFacetValues(raw, fieldType, enumVals)
		out := make([]FacetBucket, 0, len(raw))
		for _, fc := range raw {
			out = append(out, FacetBucket{ID: fc.Value, Label: fc.Value, Count: fc.Count})
		}
		return out, nil
	}
}

func (s *Service) scanEnvelopeYearCounts(ctx context.Context, libraryID core.LibraryID, itemWhere string, itemArgs []any, smallLib bool) ([]intFacetPair, error) {
	if smallLib {
		return s.scanIntFacetCounts(ctx, `SELECT i.year, COUNT(*) FROM plugin_items i WHERE `+itemWhere+` AND i.year > 0 GROUP BY i.year ORDER BY i.year ASC`, itemArgs)
	}
	qArgs := make([]any, 0, 2+len(itemArgs))
	qArgs = append(qArgs, int64(libraryID), int64(libraryID))
	qArgs = append(qArgs, itemArgs...)
	return s.scanIntFacetCounts(ctx, `
WITH RECURSIVE distinct_years(y, n) AS (
	SELECT (SELECT i.year FROM plugin_items i WHERE i.library_id = ? AND i.year > 0 ORDER BY i.year DESC LIMIT 1), 1
	UNION ALL
	SELECT (SELECT i.year FROM plugin_items i WHERE i.library_id = ? AND i.year > 0 AND i.year < distinct_years.y ORDER BY i.year DESC LIMIT 1), distinct_years.n + 1
	FROM distinct_years
	WHERE distinct_years.y IS NOT NULL AND distinct_years.n < 300
)
SELECT y, (
	SELECT COUNT(*) FROM (
		SELECT 1 FROM plugin_items i
		WHERE `+itemWhere+` AND i.year = distinct_years.y
		LIMIT 25001
	) s
)
FROM distinct_years
WHERE y IS NOT NULL
ORDER BY y ASC`, qArgs)
}

type intSpanDef struct {
	id    string
	label string
	lo    int64
	hi    int64
}

func (s *Service) scanEnvelopeReleasedBuckets(ctx context.Context, libraryID core.LibraryID, itemWhere string, itemArgs []any, smallLib bool) ([]FacetBucket, error) {
	if smallLib {
		vals, err := s.scanIntFacetCounts(ctx, `SELECT i.released, COUNT(*) FROM plugin_items i WHERE `+itemWhere+` AND i.released <> 0 GROUP BY i.released`, itemArgs)
		if err != nil {
			return nil, err
		}
		return bucketDateYears(vals), nil
	}
	var minTS, maxTS sql.NullInt64
	_ = s.db.QueryRowContext(ctx,
		`SELECT
			(SELECT i.released FROM plugin_items i WHERE i.library_id = ? AND i.released > 0 ORDER BY i.released ASC LIMIT 1),
			(SELECT i.released FROM plugin_items i WHERE i.library_id = ? AND i.released > 0 ORDER BY i.released DESC LIMIT 1)`,
		int64(libraryID), int64(libraryID)).Scan(&minTS, &maxTS)
	if !minTS.Valid || !maxTS.Valid {
		return []FacetBucket{}, nil
	}
	minY := max(time.Unix(minTS.Int64, 0).UTC().Year(), 1900)
	maxY := min(time.Unix(maxTS.Int64, 0).UTC().Year(), 2100)
	if minY > maxY {
		return []FacetBucket{}, nil
	}
	if maxY-minY > 200 {
		minY = maxY - 200
	}
	arms := make([]string, 0, maxY-minY+1)
	qArgs := make([]any, 0, 3*(maxY-minY+1)+len(itemArgs))
	for y := maxY; y >= minY; y-- {
		lo := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
		hi := time.Date(y+1, 1, 1, 0, 0, 0, 0, time.UTC).Unix() - 1
		if len(arms) == 0 {
			arms = append(arms, `SELECT CAST(? AS INTEGER) AS y, CAST(? AS BIGINT) AS lo, CAST(? AS BIGINT) AS hi`)
		} else {
			arms = append(arms, `SELECT ?, ?, ?`)
		}
		qArgs = append(qArgs, y, lo, hi)
	}
	qArgs = append(qArgs, itemArgs...)
	rows, err := s.db.QueryContext(ctx, `
WITH spans(y, lo, hi) AS (
	`+strings.Join(arms, "\n\tUNION ALL\n\t")+`
)
SELECT y, (
	SELECT COUNT(*) FROM (
		SELECT 1 FROM plugin_items i
		WHERE `+itemWhere+` AND i.released BETWEEN spans.lo AND spans.hi
		LIMIT 25001
	) s
)
FROM spans
ORDER BY y DESC`, qArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FacetBucket
	for rows.Next() {
		var y, cnt int
		if err := rows.Scan(&y, &cnt); err != nil {
			return nil, err
		}
		if cnt > 0 {
			ys := strconv.Itoa(y)
			out = append(out, FacetBucket{ID: ys, Label: ys, Count: cnt})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		return []FacetBucket{}, nil
	}
	return out, nil
}

func (s *Service) scanEnvelopeRatingCounts(ctx context.Context, libraryID core.LibraryID, itemWhere string, itemArgs []any, smallLib bool) ([]floatFacetPair, error) {
	if smallLib {
		return s.scanFloatFacetCounts(ctx, `SELECT i.rating, COUNT(*) FROM plugin_items i WHERE `+itemWhere+` AND i.rating > 0 GROUP BY i.rating ORDER BY i.rating ASC`, itemArgs)
	}
	qArgs := make([]any, 0, 2+len(itemArgs))
	qArgs = append(qArgs, int64(libraryID), int64(libraryID))
	qArgs = append(qArgs, itemArgs...)
	return s.scanFloatFacetCounts(ctx, `
WITH RECURSIVE distinct_ratings(r, n) AS (
	SELECT (SELECT i.rating FROM plugin_items i WHERE i.library_id = ? AND i.rating > 0 ORDER BY i.rating DESC LIMIT 1), 1
	UNION ALL
	SELECT (SELECT i.rating FROM plugin_items i WHERE i.library_id = ? AND i.rating > 0 AND i.rating < distinct_ratings.r ORDER BY i.rating DESC LIMIT 1), distinct_ratings.n + 1
	FROM distinct_ratings
	WHERE distinct_ratings.r IS NOT NULL AND distinct_ratings.n < 200
)
SELECT r, (
	SELECT COUNT(*) FROM (
		SELECT 1 FROM plugin_items i
		WHERE `+itemWhere+` AND i.rating = distinct_ratings.r
		LIMIT 2001
	) s
)
FROM distinct_ratings
WHERE r IS NOT NULL
ORDER BY r ASC`, qArgs)
}

func (s *Service) scanFieldIntSpanBuckets(ctx context.Context, fieldKey, fieldDrivenWhere string, itemArgs []any, defs []intSpanDef, includeZero bool) ([]FacetBucket, error) {
	if len(defs) == 0 {
		return []FacetBucket{}, nil
	}
	arms := make([]string, len(defs))
	qArgs := make([]any, 0, 3*len(defs)+2+2*len(itemArgs))
	for i, d := range defs {
		if i == 0 {
			arms[i] = `SELECT CAST(? AS INTEGER) AS idx, CAST(? AS BIGINT) AS lo, CAST(? AS BIGINT) AS hi`
		} else {
			arms[i] = `SELECT ?, ?, ?`
		}
		qArgs = append(qArgs, i, d.lo, d.hi)
	}
	qArgs = append(qArgs, fieldKey)
	qArgs = append(qArgs, itemArgs...)

	// Drive the range scan with f.v_int <> 0 so SQLite treats the partial
	// plugin_item_fields_lookup_int index as covering across every span arm
	// instead of falling back to plugin_item_fields_fold. When legitimate zero
	// values belong to a span (e.g. duration's under-10m bucket), add an exact
	// v_int = 0 probe carrying the full partial predicate.
	zeroTerm := ""
	if includeZero {
		qArgs = append(qArgs, fieldKey, int64(0))
		qArgs = append(qArgs, itemArgs...)
		zeroTerm = ` + CASE WHEN 0 BETWEEN spans.lo AND spans.hi THEN (
	SELECT COUNT(*) FROM (
		SELECT 1 FROM plugin_item_fields f
		JOIN plugin_items i ON i.item_ref = f.item_ref
		WHERE f.field = ? AND (f.v_text = '' OR f.v_int <> 0) AND f.v_int = ? AND ` + fieldDrivenWhere + `
		LIMIT 25001
	) s0
) ELSE 0 END`
	}

	rows, err := s.db.QueryContext(ctx, `
WITH spans(idx, lo, hi) AS (
	`+strings.Join(arms, "\n\tUNION ALL\n\t")+`
)
SELECT idx, (
	SELECT COUNT(*) FROM (
		SELECT 1 FROM plugin_item_fields f
		JOIN plugin_items i ON i.item_ref = f.item_ref
		WHERE f.field = ? AND f.v_int <> 0 AND f.v_int BETWEEN spans.lo AND spans.hi AND `+fieldDrivenWhere+`
		LIMIT 25001
	) s
)`+zeroTerm+`
FROM spans
ORDER BY idx ASC`, qArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FacetBucket
	for rows.Next() {
		var idx, cnt int
		if err := rows.Scan(&idx, &cnt); err != nil {
			return nil, err
		}
		if cnt > 25001 {
			cnt = 25001
		}
		if cnt > 0 && idx >= 0 && idx < len(defs) {
			out = append(out, FacetBucket{ID: defs[idx].id, Label: defs[idx].label, Count: cnt})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		return []FacetBucket{}, nil
	}
	return out, nil
}

func (s *Service) scanDateFieldYearBuckets(ctx context.Context, fieldKey, itemWhere, fieldDrivenWhere string, itemArgs []any, smallLib bool) ([]FacetBucket, error) {
	if smallLib {
		args := append(append([]any(nil), itemArgs...), fieldKey)
		vals, err := s.scanIntFacetCounts(ctx, `SELECT f.v_int, COUNT(*) FROM plugin_item_fields f JOIN plugin_items i ON i.item_ref = f.item_ref WHERE `+itemWhere+` AND f.field = ? AND f.v_int <> 0 GROUP BY f.v_int`, args)
		if err != nil {
			return nil, err
		}
		return bucketDateYears(vals), nil
	}
	minY, maxY, ok := s.dateFieldYearSpan(ctx, fieldKey)
	if !ok {
		return []FacetBucket{}, nil
	}
	defs := make([]intSpanDef, 0, maxY-minY+1)
	for y := maxY; y >= minY; y-- {
		ys := strconv.Itoa(y)
		defs = append(defs, intSpanDef{
			id:    ys,
			label: ys,
			lo:    time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC).Unix(),
			hi:    time.Date(y+1, 1, 1, 0, 0, 0, 0, time.UTC).Unix() - 1,
		})
	}
	return s.scanFieldIntSpanBuckets(ctx, fieldKey, fieldDrivenWhere, itemArgs, defs, false)
}

func (s *Service) scanDateFieldDecadeBuckets(ctx context.Context, fieldKey, itemWhere, fieldDrivenWhere string, itemArgs []any, smallLib bool) ([]FacetBucket, error) {
	if smallLib {
		args := append(append([]any(nil), itemArgs...), fieldKey)
		vals, err := s.scanIntFacetCounts(ctx, `SELECT f.v_int, COUNT(*) FROM plugin_item_fields f JOIN plugin_items i ON i.item_ref = f.item_ref WHERE `+itemWhere+` AND f.field = ? AND f.v_int <> 0 GROUP BY f.v_int`, args)
		if err != nil {
			return nil, err
		}
		return bucketDateDecades(vals), nil
	}
	minY, maxY, ok := s.dateFieldYearSpan(ctx, fieldKey)
	if !ok {
		return []FacetBucket{}, nil
	}
	minD := (minY / 10) * 10
	maxD := (maxY / 10) * 10
	defs := make([]intSpanDef, 0, (maxD-minD)/10+1)
	for d := maxD; d >= minD; d -= 10 {
		ds := strconv.Itoa(d)
		defs = append(defs, intSpanDef{
			id:    ds,
			label: ds + "s",
			lo:    time.Date(d, 1, 1, 0, 0, 0, 0, time.UTC).Unix(),
			hi:    time.Date(d+10, 1, 1, 0, 0, 0, 0, time.UTC).Unix() - 1,
		})
	}
	return s.scanFieldIntSpanBuckets(ctx, fieldKey, fieldDrivenWhere, itemArgs, defs, false)
}

func (s *Service) dateFieldYearSpan(ctx context.Context, fieldKey string) (int, int, bool) {
	var minTS, maxTS sql.NullInt64
	_ = s.db.QueryRowContext(ctx,
		`SELECT
			(SELECT f.v_int FROM plugin_item_fields f WHERE f.field = ? AND f.v_int <> 0 ORDER BY f.v_int ASC LIMIT 1),
			(SELECT f.v_int FROM plugin_item_fields f WHERE f.field = ? AND f.v_int <> 0 ORDER BY f.v_int DESC LIMIT 1)`,
		fieldKey, fieldKey).Scan(&minTS, &maxTS)
	if !minTS.Valid || !maxTS.Valid {
		return 0, 0, false
	}
	minY := max(time.Unix(minTS.Int64, 0).UTC().Year(), 1900)
	maxY := min(time.Unix(maxTS.Int64, 0).UTC().Year(), 2100)
	if minY > maxY {
		return 0, 0, false
	}
	if maxY-minY > 200 {
		minY = maxY - 200
	}
	return minY, maxY, true
}

func (s *Service) scanDurationBucketsLarge(ctx context.Context, fieldKey, fieldDrivenWhere string, itemArgs []any) ([]FacetBucket, error) {
	defs := make([]intSpanDef, len(libraryfilter.DurationBucketDefs))
	for i, d := range libraryfilter.DurationBucketDefs {
		defs[i] = intSpanDef{id: d.ID, label: d.Label, lo: d.Min, hi: d.Max}
	}
	return s.scanFieldIntSpanBuckets(ctx, fieldKey, fieldDrivenWhere, itemArgs, defs, true)
}

func (s *Service) scanFieldIntCounts(ctx context.Context, fieldKey, itemWhere, fieldDrivenWhere string, itemArgs []any, smallLib bool) ([]intFacetPair, error) {
	if smallLib {
		args := append(append([]any(nil), itemArgs...), fieldKey)
		return s.scanIntFacetCounts(ctx, `SELECT f.v_int, COUNT(*) FROM plugin_item_fields f JOIN plugin_items i ON i.item_ref = f.item_ref WHERE `+itemWhere+` AND f.field = ? AND (f.v_text = '' OR f.v_int <> 0) GROUP BY f.v_int ORDER BY f.v_int ASC`, args)
	}
	qArgs := make([]any, 0, 3+len(itemArgs))
	qArgs = append(qArgs, fieldKey, fieldKey, fieldKey)
	qArgs = append(qArgs, itemArgs...)
	return s.scanIntFacetCounts(ctx, `
WITH RECURSIVE distinct_ints(v, n) AS (
	SELECT (SELECT f.v_int FROM plugin_item_fields f WHERE f.field = ? AND (f.v_text = '' OR f.v_int <> 0) ORDER BY f.v_int ASC LIMIT 1), 1
	UNION ALL
	SELECT (SELECT f.v_int FROM plugin_item_fields f WHERE f.field = ? AND (f.v_text = '' OR f.v_int <> 0) AND f.v_int > distinct_ints.v ORDER BY f.v_int ASC LIMIT 1), distinct_ints.n + 1
	FROM distinct_ints
	WHERE distinct_ints.v IS NOT NULL AND distinct_ints.n < 400
)
SELECT v, (
	SELECT COUNT(*) FROM (
		SELECT 1 FROM plugin_item_fields f
		JOIN plugin_items i ON i.item_ref = f.item_ref
		WHERE f.field = ? AND (f.v_text = '' OR f.v_int <> 0) AND f.v_int = distinct_ints.v AND `+fieldDrivenWhere+`
		LIMIT 2001
	) s
)
FROM distinct_ints
WHERE v IS NOT NULL
ORDER BY v ASC`, qArgs)
}

func (s *Service) scanFieldFloatCounts(ctx context.Context, fieldKey, itemWhere, fieldDrivenWhere string, itemArgs []any, smallLib bool) ([]floatFacetPair, error) {
	if smallLib {
		args := append(append([]any(nil), itemArgs...), fieldKey)
		return s.scanFloatFacetCounts(ctx, `SELECT f.v_real, COUNT(*) FROM plugin_item_fields f JOIN plugin_items i ON i.item_ref = f.item_ref WHERE `+itemWhere+` AND f.field = ? AND f.v_text = '' GROUP BY f.v_real ORDER BY f.v_real ASC`, args)
	}
	qArgs := make([]any, 0, 3+len(itemArgs))
	qArgs = append(qArgs, fieldKey, fieldKey, fieldKey)
	qArgs = append(qArgs, itemArgs...)
	return s.scanFloatFacetCounts(ctx, `
WITH RECURSIVE distinct_reals(v, n) AS (
	SELECT (SELECT f.v_real FROM plugin_item_fields f WHERE f.field = ? AND f.v_text = '' ORDER BY f.v_real ASC LIMIT 1), 1
	UNION ALL
	SELECT (SELECT f.v_real FROM plugin_item_fields f WHERE f.field = ? AND f.v_text = '' AND f.v_real > distinct_reals.v ORDER BY f.v_real ASC LIMIT 1), distinct_reals.n + 1
	FROM distinct_reals
	WHERE distinct_reals.v IS NOT NULL AND distinct_reals.n < 400
)
SELECT v, (
	SELECT COUNT(*) FROM (
		SELECT 1 FROM plugin_item_fields f
		JOIN plugin_items i ON i.item_ref = f.item_ref
		WHERE f.field = ? AND f.v_text = '' AND f.v_real = distinct_reals.v AND `+fieldDrivenWhere+`
		LIMIT 2001
	) s
)
FROM distinct_reals
WHERE v IS NOT NULL
ORDER BY v ASC`, qArgs)
}

func (s *Service) scanFieldTextCountsLarge(ctx context.Context, fieldKey, fieldDrivenWhere string, itemArgs []any, countCap int) ([]FacetCount, error) {
	qArgs := make([]any, 0, 4+len(itemArgs))
	qArgs = append(qArgs, fieldKey, fieldKey, fieldKey)
	qArgs = append(qArgs, itemArgs...)
	qArgs = append(qArgs, countCap)
	querySQL := `
WITH RECURSIVE distinct_vals(val, n) AS (
	SELECT (SELECT f.v_text FROM plugin_item_fields f WHERE f.field = ? AND f.v_text > '' ORDER BY f.v_text ASC LIMIT 1), 1
	UNION ALL
	SELECT (SELECT f.v_text FROM plugin_item_fields f WHERE f.field = ? AND f.v_text > distinct_vals.val ORDER BY f.v_text ASC LIMIT 1), distinct_vals.n + 1
	FROM distinct_vals
	WHERE distinct_vals.val IS NOT NULL AND distinct_vals.n < 400
)
SELECT val, (
	SELECT COUNT(*) FROM (
		SELECT 1 FROM plugin_item_fields f
		JOIN plugin_items i ON i.item_ref = f.item_ref
		WHERE f.field = ? AND f.v_text = distinct_vals.val AND ` + fieldDrivenWhere + `
		LIMIT ?
	) s
)
FROM distinct_vals
WHERE val IS NOT NULL`
	if store.DialectOf(s.db) == store.Postgres {
		querySQL = `
WITH RECURSIVE distinct_vals(val, n) AS (
	SELECT (SELECT f.v_text FROM plugin_item_fields f WHERE f.field = ? AND left(f.v_text, 256) > '' ORDER BY left(f.v_text, 256) ASC, f.v_text ASC LIMIT 1), 1
	UNION ALL
	SELECT (
		SELECT f.v_text
		FROM plugin_item_fields f
		WHERE f.field = ?
		  AND left(f.v_text, 256) >= left(distinct_vals.val, 256)
		  AND (left(f.v_text, 256) > left(distinct_vals.val, 256) OR (left(f.v_text, 256) = left(distinct_vals.val, 256) AND f.v_text > distinct_vals.val))
		ORDER BY left(f.v_text, 256) ASC, f.v_text ASC
		LIMIT 1
	), distinct_vals.n + 1
	FROM distinct_vals
	WHERE distinct_vals.val IS NOT NULL AND distinct_vals.n < 400
)
SELECT val, (
	SELECT COUNT(*) FROM (
		SELECT 1 FROM plugin_item_fields f
		JOIN plugin_items i ON i.item_ref = f.item_ref
		WHERE f.field = ? AND left(f.v_text, 256) = left(distinct_vals.val, 256) AND f.v_text = distinct_vals.val AND ` + fieldDrivenWhere + `
		LIMIT ?
	) s
)
FROM distinct_vals
WHERE val IS NOT NULL`
	}
	rows, err := s.db.QueryContext(ctx, querySQL, qArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FacetCount
	for rows.Next() {
		var fc FacetCount
		if err := rows.Scan(&fc.Value, &fc.Count); err != nil {
			return nil, err
		}
		if fc.Value != "" && fc.Count > 0 {
			out = append(out, fc)
		}
	}
	return out, rows.Err()
}

type personBucketCandidate struct {
	pid   int64
	label string
	count int
}

func (s *Service) scanPeopleCandidatesLarge(ctx context.Context, fieldKey, fieldDrivenWhere string, itemArgs []any, countCap int) ([]personBucketCandidate, error) {
	minPID := core.PluginPersonIDBase
	maxPID := core.PluginPersonIDBase + core.PluginPersonIDSpan
	qArgs := make([]any, 0, 9+2*len(itemArgs))
	qArgs = append(qArgs, fieldKey, minPID, maxPID, fieldKey, maxPID)
	qArgs = append(qArgs, fieldKey)
	qArgs = append(qArgs, itemArgs...)
	qArgs = append(qArgs, countCap)
	qArgs = append(qArgs, fieldKey)
	qArgs = append(qArgs, itemArgs...)
	qArgs = append(qArgs, countCap)
	rows, err := s.db.QueryContext(ctx, `
WITH RECURSIVE distinct_pids(pid, n) AS (
	SELECT (SELECT f.v_int FROM plugin_item_fields f WHERE f.field = ? AND f.v_int <> 0 AND f.v_int > ? AND f.v_int < ? ORDER BY f.v_int ASC LIMIT 1), 1
	UNION ALL
	SELECT (SELECT f.v_int FROM plugin_item_fields f WHERE f.field = ? AND f.v_int <> 0 AND f.v_int > distinct_pids.pid AND f.v_int < ? ORDER BY f.v_int ASC LIMIT 1), distinct_pids.n + 1
	FROM distinct_pids
	WHERE distinct_pids.pid IS NOT NULL AND distinct_pids.n < 400
)
SELECT pid,
	COALESCE((
		SELECT MIN(sub.v_text) FROM (
			SELECT f.v_text FROM plugin_item_fields f
			JOIN plugin_items i ON i.item_ref = f.item_ref
			WHERE f.field = ? AND f.v_int <> 0 AND f.v_int = distinct_pids.pid AND f.v_text <> '' AND `+fieldDrivenWhere+`
			LIMIT ?
		) sub
	), ''),
	(
		SELECT COUNT(*) FROM (
			SELECT 1 FROM plugin_item_fields f
			JOIN plugin_items i ON i.item_ref = f.item_ref
			WHERE f.field = ? AND f.v_int <> 0 AND f.v_int = distinct_pids.pid AND f.v_text <> '' AND `+fieldDrivenWhere+`
			LIMIT ?
		) sub2
	)
FROM distinct_pids
WHERE pid IS NOT NULL`, qArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []personBucketCandidate
	for rows.Next() {
		var pid int64
		var label string
		var cnt int
		if err := rows.Scan(&pid, &label, &cnt); err != nil {
			return nil, err
		}
		clean := strings.TrimSpace(label)
		if clean != "" && cnt > 0 {
			out = append(out, personBucketCandidate{pid: pid, label: clean, count: cnt})
		}
	}
	return out, rows.Err()
}

func (s *Service) scanPeopleBucketsLarge(ctx context.Context, fieldKey, fieldDrivenWhere string, itemArgs []any) ([]FacetBucket, error) {
	cands, err := s.scanPeopleCandidatesLarge(ctx, fieldKey, fieldDrivenWhere, itemArgs, 2001)
	if err != nil {
		return nil, err
	}
	out := make([]FacetBucket, 0, len(cands))
	for _, c := range cands {
		out = append(out, FacetBucket{ID: c.label, Label: c.label, Count: c.count})
	}
	sort.SliceStable(out, func(i, j int) bool { return facetNameLess(out[i].Label, out[j].Label) })
	return out, nil
}

type intFacetPair struct {
	val   int64
	count int
}

type floatFacetPair struct {
	val   float64
	count int
}

func (s *Service) scanIntFacetCounts(ctx context.Context, query string, args []any) ([]intFacetPair, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []intFacetPair
	for rows.Next() {
		var p intFacetPair
		if err := rows.Scan(&p.val, &p.count); err != nil {
			return nil, err
		}
		if p.count > 0 {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

func (s *Service) scanFloatFacetCounts(ctx context.Context, query string, args []any) ([]floatFacetPair, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []floatFacetPair
	for rows.Next() {
		var p floatFacetPair
		if err := rows.Scan(&p.val, &p.count); err != nil {
			return nil, err
		}
		if p.count > 0 {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

func bucketDateYears(vals []intFacetPair) []FacetBucket {
	if len(vals) == 0 {
		return []FacetBucket{}
	}
	byYear := make(map[int]int)
	for _, p := range vals {
		y := time.Unix(p.val, 0).UTC().Year()
		if y >= 1000 && y <= 9999 {
			byYear[y] += p.count
		}
	}
	years := make([]int, 0, len(byYear))
	for y := range byYear {
		years = append(years, y)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(years)))
	out := make([]FacetBucket, 0, len(years))
	for _, y := range years {
		s := strconv.Itoa(y)
		out = append(out, FacetBucket{ID: s, Label: s, Count: byYear[y]})
	}
	return out
}

func bucketDateDecades(vals []intFacetPair) []FacetBucket {
	if len(vals) == 0 {
		return []FacetBucket{}
	}
	byDecade := make(map[int]int)
	for _, p := range vals {
		y := time.Unix(p.val, 0).UTC().Year()
		if y >= 1000 && y <= 9999 {
			d := (y / 10) * 10
			byDecade[d] += p.count
		}
	}
	decades := make([]int, 0, len(byDecade))
	for d := range byDecade {
		decades = append(decades, d)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(decades)))
	out := make([]FacetBucket, 0, len(decades))
	for _, d := range decades {
		ds := strconv.Itoa(d)
		out = append(out, FacetBucket{ID: ds, Label: ds + "s", Count: byDecade[d]})
	}
	return out
}

func bucketYearDecades(vals []intFacetPair) []FacetBucket {
	if len(vals) == 0 {
		return []FacetBucket{}
	}
	byDecade := make(map[int]int)
	for _, p := range vals {
		y := int(p.val)
		if y >= 1000 && y <= 9999 {
			d := (y / 10) * 10
			byDecade[d] += p.count
		}
	}
	decades := make([]int, 0, len(byDecade))
	for d := range byDecade {
		decades = append(decades, d)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(decades)))
	out := make([]FacetBucket, 0, len(decades))
	for _, d := range decades {
		ds := strconv.Itoa(d)
		out = append(out, FacetBucket{ID: ds, Label: ds + "s", Count: byDecade[d]})
	}
	return out
}

func bucketDurationValues(vals []intFacetPair) []FacetBucket {
	if len(vals) == 0 {
		return []FacetBucket{}
	}
	counts := make([]int, len(libraryfilter.DurationBucketDefs))
	for _, p := range vals {
		if p.val < 0 {
			continue
		}
		for i, def := range libraryfilter.DurationBucketDefs {
			if p.val >= def.Min && p.val <= def.Max {
				counts[i] += p.count
				break
			}
		}
	}
	out := make([]FacetBucket, 0, len(libraryfilter.DurationBucketDefs))
	for i, def := range libraryfilter.DurationBucketDefs {
		if counts[i] > 0 {
			out = append(out, FacetBucket{ID: def.ID, Label: def.Label, Count: counts[i]})
		}
	}
	return out
}

func bucketIntValues(vals []intFacetPair, descDistinct bool) []FacetBucket {
	if len(vals) == 0 {
		return []FacetBucket{}
	}
	if len(vals) <= 50 {
		out := make([]FacetBucket, len(vals))
		for i, p := range vals {
			idx := i
			if descDistinct {
				idx = len(vals) - 1 - i
			}
			s := strconv.FormatInt(p.val, 10)
			out[idx] = FacetBucket{ID: s, Label: s, Count: p.count}
		}
		return out
	}
	minVal, maxVal := vals[0].val, vals[len(vals)-1].val
	step := niceIntBucketStep(minVal, maxVal, 10)
	firstLo := floorDivInt64(minVal, step) * step
	type rangeAccum struct {
		lo, hi int64
		count  int
	}
	byIdx := make(map[int64]*rangeAccum)
	var order []int64
	for _, p := range vals {
		bIdx := floorDivInt64(p.val-firstLo, step)
		acc := byIdx[bIdx]
		if acc == nil {
			lo := firstLo + bIdx*step
			acc = &rangeAccum{lo: lo, hi: lo + step - 1}
			byIdx[bIdx] = acc
			order = append(order, bIdx)
		}
		acc.count += p.count
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	out := make([]FacetBucket, 0, len(order))
	for _, bIdx := range order {
		acc := byIdx[bIdx]
		if acc.count <= 0 {
			continue
		}
		id := fmt.Sprintf("%d..%d", acc.lo, acc.hi)
		label := fmt.Sprintf("%d-%d", acc.lo, acc.hi)
		if acc.lo == acc.hi {
			label = strconv.FormatInt(acc.lo, 10)
		} else if acc.lo < 0 {
			label = fmt.Sprintf("%d to %d", acc.lo, acc.hi)
		}
		out = append(out, FacetBucket{ID: id, Label: label, Count: acc.count})
	}
	return out
}

func bucketFloatValues(vals []floatFacetPair) []FacetBucket {
	if len(vals) == 0 {
		return []FacetBucket{}
	}
	if len(vals) <= 50 {
		out := make([]FacetBucket, len(vals))
		for i, p := range vals {
			s := strconv.FormatFloat(p.val, 'f', -1, 64)
			out[i] = FacetBucket{ID: s, Label: s, Count: p.count}
		}
		return out
	}
	minInt := int64(math.Floor(vals[0].val))
	maxInt := int64(math.Ceil(vals[len(vals)-1].val))
	step := niceIntBucketStep(minInt, maxInt, 10)
	firstLo := floorDivInt64(minInt, step) * step
	type rangeAccum struct {
		lo, hi int64
		count  int
	}
	byIdx := make(map[int64]*rangeAccum)
	var order []int64
	for _, p := range vals {
		iv := int64(math.Floor(p.val))
		bIdx := floorDivInt64(iv-firstLo, step)
		acc := byIdx[bIdx]
		if acc == nil {
			lo := firstLo + bIdx*step
			acc = &rangeAccum{lo: lo, hi: lo + step}
			byIdx[bIdx] = acc
			order = append(order, bIdx)
		}
		acc.count += p.count
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	out := make([]FacetBucket, 0, len(order))
	for _, bIdx := range order {
		acc := byIdx[bIdx]
		if acc.count <= 0 {
			continue
		}
		id := fmt.Sprintf("%d..%d", acc.lo, acc.hi)
		label := fmt.Sprintf("%d-%d", acc.lo, acc.hi)
		if acc.lo < 0 {
			label = fmt.Sprintf("%d to %d", acc.lo, acc.hi)
		}
		out = append(out, FacetBucket{ID: id, Label: label, Count: acc.count})
	}
	return out
}

func niceIntBucketStep(minVal, maxVal int64, targetBuckets int64) int64 {
	if targetBuckets <= 0 {
		targetBuckets = 10
	}
	span := float64(maxVal-minVal) + 1
	if span <= float64(targetBuckets) {
		return 1
	}
	raw := span / float64(targetBuckets)
	exp := math.Pow(10, math.Floor(math.Log10(raw)))
	if exp < 1 {
		exp = 1
	}
	for _, mult := range []float64{1, 2, 5, 10} {
		cand := mult * exp
		if cand >= raw {
			if cand > float64(math.MaxInt64/2) {
				return math.MaxInt64 / 2
			}
			step := int64(cand)
			if step < 1 {
				return 1
			}
			return step
		}
	}
	return 1
}

func floorDivInt64(a, b int64) int64 {
	q := a / b
	r := a % b
	if r != 0 && ((r < 0) != (b < 0)) {
		q--
	}
	return q
}

// orderFacetValues orders a facet page for reading. Numbers keep the most
// common first; labels read alphabetically; an enum follows its declaration.
func orderFacetValues(values []FacetCount, fieldType string, enum []string) {
	switch fieldType {
	case "enum":
		rank := make(map[string]int, len(enum))
		for i, v := range enum {
			rank[v] = i
		}
		sort.SliceStable(values, func(i, j int) bool {
			ri, iok := rank[values[i].Value]
			rj, jok := rank[values[j].Value]
			if iok != jok {
				return iok
			}
			if iok {
				return ri < rj
			}
			return facetNameLess(values[i].Value, values[j].Value)
		})
	case "string", "strings", "bool", "people":
		sort.SliceStable(values, func(i, j int) bool { return facetNameLess(values[i].Value, values[j].Value) })
	}
}

func facetNameLess(a, b string) bool {
	if la, lb := strings.ToLower(a), strings.ToLower(b); la != lb {
		return la < lb
	}
	return a < b
}

func envelopeFacet(key string) bool {
	return key == "year" || key == "released" || key == "rating"
}

func hasActiveFacets(facets map[string][]string) bool {
	for k, vals := range facets {
		if strings.TrimSpace(k) == "" {
			continue
		}
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return true
			}
		}
	}
	return false
}

func hasActiveAnyGroups(groups []core.KindBrowseFacetGroup) bool {
	for _, g := range groups {
		if len(activeAnyGroupAlts(g)) > 0 {
			return true
		}
	}
	return false
}

// facetMode selects how one facet map's values combine within a field:
// any of them (OR), all of them (one predicate per value), or none of
// them (negated). Fields always combine with AND.
type facetMode int

const (
	facetAny facetMode = iota
	facetAll
	facetNone
)

func checkFacetValueBound(anyGroups []core.KindBrowseFacetGroup, maps ...map[string][]string) error {
	if len(anyGroups) > MaxBrowseAnyGroups {
		return fmt.Errorf("pluginitems: facet anyGroups exceed %d per query", MaxBrowseAnyGroups)
	}
	total := 0
	for _, facets := range maps {
		for _, vals := range facets {
			total += len(vals)
			if total > MaxBrowseFacetValues {
				return fmt.Errorf("pluginitems: facet values exceed %d per query", MaxBrowseFacetValues)
			}
		}
	}
	alts := 0
	for _, g := range anyGroups {
		total += len(g.Values)
		alts += len(g.Any)
		if alts > MaxBrowseFacetValues {
			return fmt.Errorf("pluginitems: facet anyGroups alternatives exceed %d per query", MaxBrowseFacetValues)
		}
		for _, alt := range g.Any {
			total += len(alt.Values)
		}
		if total > MaxBrowseFacetValues {
			return fmt.Errorf("pluginitems: facet values exceed %d per query", MaxBrowseFacetValues)
		}
	}
	return nil
}

func cleanFacetValues(vals []string) []string {
	var clean []string
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			clean = append(clean, v)
		}
	}
	return clean
}

func sortedFacetKeys(facets map[string][]string) []string {
	keys := make([]string, 0, len(facets))
	for k := range facets {
		if strings.TrimSpace(k) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// appendLibraryFacetMode emits libraryfilter facet predicates for one
// mode: any uses the map directly, all repeats the predicate per value,
// and none negates each field's predicate. When isPG is true, text facets
// include md5(f.v_text) so PostgreSQL seeks plugin_item_fields_pkey_idx
// directly while f.v_text IN (...) keeps equality exact.
func appendLibraryFacetMode(where *strings.Builder, args *[]any, facets map[string][]string, types map[string]string, mode facetMode, isPG bool) {
	if isPG {
		emitPGText := func(key string, cleanVals []string, negate bool) {
			op := "EXISTS"
			if negate {
				op = "NOT EXISTS"
			}
			where.WriteString(` AND ` + op + ` (SELECT 1 FROM plugin_item_fields f WHERE f.item_ref = i.item_ref AND f.field = ?`)
			*args = append(*args, key)
			appendPGMD5In(where, args, "f.v_text", cleanVals)
			where.WriteString(`)`)
		}
		for _, key := range sortedFacetKeys(facets) {
			cleanVals := cleanFacetValues(facets[key])
			if len(cleanVals) == 0 {
				continue
			}
			ftype := types[key]
			base, isDateDecade := libraryfilter.SplitDateDecadeFacet(key)
			isTextFacet := !(key == "decade" && types["decade"] == "") &&
				!(isDateDecade && types[base] == "date") &&
				!envelopeFacet(key) &&
				ftype != "int" && ftype != "date" && ftype != "duration" && ftype != "float" && ftype != "people" &&
				!(ftype == "" && browseAllIntRanges(cleanVals))
			if isTextFacet {
				switch mode {
				case facetAny:
					emitPGText(key, cleanVals, false)
				case facetAll:
					for _, v := range cleanVals {
						emitPGText(key, []string{v}, false)
					}
				case facetNone:
					emitPGText(key, cleanVals, true)
				}
				continue
			}
			appendLibraryFacetMode(where, args, map[string][]string{key: cleanVals}, types, mode, false)
		}
		return
	}
	switch mode {
	case facetAny:
		libraryfilter.AppendPluginFacetClauses(where, args, facets, types)
	case facetAll:
		for _, key := range sortedFacetKeys(facets) {
			for _, v := range cleanFacetValues(facets[key]) {
				libraryfilter.AppendPluginFacetClauses(where, args, map[string][]string{key: {v}}, types)
			}
		}
	case facetNone:
		for _, key := range sortedFacetKeys(facets) {
			cleanVals := cleanFacetValues(facets[key])
			if len(cleanVals) == 0 {
				continue
			}
			var tmp strings.Builder
			var tmpArgs []any
			libraryfilter.AppendPluginFacetClauses(&tmp, &tmpArgs, map[string][]string{key: cleanVals}, types)
			clause := strings.TrimPrefix(tmp.String(), " AND ")
			if clause == "" || clause == "0=1" {
				continue
			}
			*args = append(*args, tmpArgs...)
			where.WriteString(` AND NOT (` + clause + `)`)
		}
	}
}

// appendFacets resolves field types only when a browse/search uses facets.
func (s *Service) appendFacets(ctx context.Context, lib core.LibraryID, where *strings.Builder, args *[]any, facets, allFacets, excludeFacets map[string][]string, anyGroups []core.KindBrowseFacetGroup) error {
	if err := checkFacetValueBound(anyGroups, facets, allFacets, excludeFacets); err != nil {
		return err
	}
	if !hasActiveFacets(facets) && !hasActiveFacets(allFacets) && !hasActiveFacets(excludeFacets) && !hasActiveAnyGroups(anyGroups) {
		return nil
	}
	rec, err := s.GetKindByLibrary(ctx, lib)
	if err != nil {
		return err
	}
	types := map[string]string{}
	for _, field := range rec.Decl.Fields {
		types[field.Key] = field.Type
	}
	isPG := store.DialectOf(s.db) == store.Postgres
	appendLibraryFacetMode(where, args, facets, types, facetAny, isPG)
	appendLibraryFacetMode(where, args, allFacets, types, facetAll, isPG)
	appendLibraryFacetMode(where, args, excludeFacets, types, facetNone, isPG)
	for _, g := range anyGroups {
		appendAnyGroupClause(where, args, types, g, isPG)
	}
	return nil
}

type browseClauseKind uint8

const (
	clauseText browseClauseKind = iota
	clauseInt
	clauseFloat
	clauseDateDecade
	clausePeople
	clausePersonID
	clauseEnvelope
	clauseDisjunction
)

type browseFacetClause struct {
	kind         browseClauseKind
	key          string
	ftype        string
	vals         []string
	pids         []int64
	peopleFields []string
	disjuncts    []fieldDisjunct
	alternatives []browseFacetClause
	defaultPart  string
	negate       bool
	singleValued bool
	isRange      bool
	estCount     int
}

func appendUniqueInt64s(dst, src []int64) []int64 {
	if len(src) == 0 {
		return dst
	}
	var seen map[int64]struct{}
	if len(dst) > 0 {
		seen = make(map[int64]struct{}, len(dst)+len(src))
		for _, v := range dst {
			seen[v] = struct{}{}
		}
	}
	for _, v := range src {
		if seen != nil {
			if _, dup := seen[v]; dup {
				continue
			}
			seen[v] = struct{}{}
		} else if len(src) > 1 {
			seen = make(map[int64]struct{}, len(src))
			seen[v] = struct{}{}
		}
		dst = append(dst, v)
	}
	return dst
}

func parsePeopleFacetIDs(vals []string) []int64 {
	var pids []int64
	for _, v := range vals {
		var pid int64
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && core.IsPluginPersonID(n) {
			pid = n
		} else if p := core.PluginPersonID(v); p > 0 {
			pid = p
		}
		if pid > 0 {
			pids = appendUniqueInt64s(pids, []int64{pid})
		}
	}
	return pids
}

func collectBrowseFieldClauses(q BrowseQuery, types map[string]string, peopleFields []string) ([]browseFacetClause, bool) {
	var clauses []browseFacetClause
	if q.PersonID > 0 {
		if !core.IsPluginPersonID(q.PersonID) || len(peopleFields) == 0 {
			return nil, true
		}
		clauses = append(clauses, browseFacetClause{
			kind:         clausePersonID,
			pids:         []int64{q.PersonID},
			peopleFields: peopleFields,
			estCount:     -1,
		})
	}

	addOne := func(key string, vals []string, negate bool) bool {
		c, skip, impossible := buildBrowseFacetClause(types, key, vals, negate)
		if impossible {
			return false
		}
		if !skip {
			clauses = append(clauses, c)
		}
		return true
	}

	for _, key := range sortedFacetKeys(q.Facets) {
		vals := dedupCleanValues(q.Facets[key])
		if len(vals) == 0 {
			continue
		}
		if !addOne(key, vals, false) {
			return nil, true
		}
	}
	for _, key := range sortedFacetKeys(q.AllFacets) {
		vals := dedupCleanValues(q.AllFacets[key])
		if len(vals) == 0 {
			continue
		}
		if isSingleValuedTextType(types[key]) && len(vals) > 1 {
			return nil, true
		}
		for _, v := range vals {
			if !addOne(key, []string{v}, false) {
				return nil, true
			}
		}
	}
	for _, g := range q.AnyGroups {
		alts := activeAnyGroupAlts(g)
		switch len(alts) {
		case 0:
			continue
		case 1:
			if !addOne(alts[0].Field, dedupCleanValues(alts[0].Values), false) {
				return nil, true
			}
		default:
			group := browseFacetClause{kind: clauseDisjunction, estCount: -1}
			for _, alt := range alts {
				c, skip, impossible := buildBrowseFacetClause(types, alt.Field, dedupCleanValues(alt.Values), false)
				if !skip && !impossible {
					group.alternatives = append(group.alternatives, c)
				}
			}
			if len(group.alternatives) == 0 {
				return nil, true
			}
			clauses = append(clauses, group)
		}
	}
	for _, key := range sortedFacetKeys(q.ExcludeFacets) {
		vals := dedupCleanValues(q.ExcludeFacets[key])
		if len(vals) == 0 {
			continue
		}
		if !addOne(key, vals, true) {
			return nil, true
		}
	}
	return clauses, false
}

func buildBrowseFacetClause(types map[string]string, key string, vals []string, negate bool) (browseFacetClause, bool, bool) {
	if (key == "decade" && types["decade"] == "") || envelopeFacet(key) {
		return browseFacetClause{
			kind:     clauseEnvelope,
			key:      key,
			ftype:    types[key],
			vals:     vals,
			negate:   negate,
			estCount: -1,
		}, false, false
	}
	if base, ok := libraryfilter.SplitDateDecadeFacet(key); ok && types[base] == "date" {
		disjuncts := buildDateDecadeDisjuncts(vals)
		if len(disjuncts) == 0 {
			return browseFacetClause{}, true, !negate
		}
		return browseFacetClause{
			kind:        clauseDateDecade,
			key:         base,
			ftype:       "date",
			vals:        vals,
			disjuncts:   disjuncts,
			defaultPart: `(f.v_text = '' OR f.v_int <> 0)`,
			negate:      negate,
			isRange:     true,
			estCount:    -1,
		}, false, false
	}
	ftype := types[key]
	switch ftype {
	case "int", "date", "duration":
		disjuncts, isRange := buildIntFieldDisjuncts(ftype, vals)
		if len(disjuncts) == 0 {
			return browseFacetClause{}, true, !negate
		}
		return browseFacetClause{
			kind:        clauseInt,
			key:         key,
			ftype:       ftype,
			vals:        vals,
			disjuncts:   disjuncts,
			defaultPart: `(f.v_text = '' OR f.v_int <> 0)`,
			negate:      negate,
			isRange:     isRange,
			estCount:    -1,
		}, false, false
	case "float":
		disjuncts, isRange := buildFloatFieldDisjuncts(vals)
		if len(disjuncts) == 0 {
			return browseFacetClause{}, true, !negate
		}
		return browseFacetClause{
			kind:        clauseFloat,
			key:         key,
			ftype:       ftype,
			vals:        vals,
			disjuncts:   disjuncts,
			defaultPart: `f.v_text = ''`,
			negate:      negate,
			isRange:     isRange,
			estCount:    -1,
		}, false, false
	case "people":
		pids := parsePeopleFacetIDs(vals)
		if len(pids) == 0 {
			return browseFacetClause{}, true, !negate
		}
		return browseFacetClause{
			kind:     clausePeople,
			key:      key,
			ftype:    ftype,
			vals:     vals,
			pids:     pids,
			negate:   negate,
			estCount: -1,
		}, false, false
	default:
		if ftype == "" && browseAllIntRanges(vals) {
			disjuncts, isRange := buildIntFieldDisjuncts("int", vals)
			if len(disjuncts) == 0 {
				return browseFacetClause{}, true, !negate
			}
			return browseFacetClause{
				kind:        clauseInt,
				key:         key,
				ftype:       "int",
				vals:        vals,
				disjuncts:   disjuncts,
				defaultPart: `(f.v_text = '' OR f.v_int <> 0)`,
				negate:      negate,
				isRange:     isRange,
				estCount:    -1,
			}, false, false
		}
		return browseFacetClause{
			kind:         clauseText,
			key:          key,
			ftype:        ftype,
			vals:         vals,
			negate:       negate,
			singleValued: isSingleValuedTextType(ftype),
			estCount:     -1,
		}, false, false
	}
}

func clauseValCount(c browseFacetClause) int {
	switch c.kind {
	case clausePeople:
		return len(c.pids)
	case clausePersonID:
		return len(c.peopleFields)
	default:
		return len(c.vals)
	}
}

// pickBrowseDriver chooses the narrowest positive plugin_item_fields clause
// to drive both the page and count queries, using O(1) stored tallies when
// available and falling back to shape heuristics (single-value exact equality
// before multi-value or range filters) when tallies are absent or untallied.
func pickBrowseDriver(clauses []browseFacetClause, tally browseTallyInfo) (bestIdx int, empty bool) {
	bestIdx = -1
	for i := range clauses {
		c := &clauses[i]
		// A disjunction has no estimate for the union of its alternatives.
		if c.negate || c.kind == clauseEnvelope || c.kind == clauseDisjunction {
			continue
		}
		if tally.talliesActive {
			switch c.kind {
			case clauseText:
				if isTalliedTextType(c.ftype) {
					sum := 0
					for _, v := range c.vals {
						sum += tally.textCounts[browseTextTallyKey{field: c.key, val: v}]
					}
					if sum == 0 {
						return -1, true
					}
					c.estCount = sum
				}
			case clausePeople:
				if tally.personCounts != nil {
					sum := 0
					for _, pid := range c.pids {
						sum += tally.personCounts[browsePersonTallyKey{field: c.key, pid: pid}]
					}
					c.estCount = sum
				}
			case clausePersonID:
				if tally.personCounts != nil {
					sum := 0
					for _, fk := range c.peopleFields {
						sum += tally.personCounts[browsePersonTallyKey{field: fk, pid: c.pids[0]}]
					}
					c.estCount = sum
				}
			}
		}
		if bestIdx < 0 || betterBrowseDriver(*c, clauses[bestIdx], tally.libTotal) {
			bestIdx = i
		}
	}
	return bestIdx, false
}

// betterBrowseDriver reports whether candidate a is strictly preferable to b
// as the driving positive facet.
func betterBrowseDriver(a, b browseFacetClause, libTotal int) bool {
	const narrowTallyCeiling = 20_000
	isBroadTallied := func(est int) bool {
		return est > narrowTallyCeiling && (libTotal <= 0 || est*10 > libTotal)
	}
	isSingleExact := func(c browseFacetClause) bool {
		return !c.isRange && clauseValCount(c) == 1
	}

	aTallied := a.estCount >= 0
	bTallied := b.estCount >= 0
	switch {
	case aTallied && bTallied:
		if a.estCount != b.estCount {
			return a.estCount < b.estCount
		}
		if aSingle, bSingle := isSingleExact(a), isSingleExact(b); aSingle != bSingle {
			return aSingle
		}
		if a.singleValued != b.singleValued {
			return a.singleValued
		}
		return false
	case aTallied && !bTallied:
		// A known-narrow tallied facet always beats an untallied filter. If the
		// tallied facet is very broad (>20k and >10% of the library), prefer an
		// untallied single-value exact equality (e.g. episodes = 42), while
		// still beating untallied ranges.
		if isBroadTallied(a.estCount) && isSingleExact(b) {
			return false
		}
		return true
	case !aTallied && bTallied:
		return isBroadTallied(b.estCount) && isSingleExact(a)
	default:
		if a.isRange != b.isRange {
			return !a.isRange
		}
		aN, bN := clauseValCount(a), clauseValCount(b)
		if aN != bN {
			return aN < bN
		}
		if a.singleValued != b.singleValued {
			return a.singleValued
		}
		return false
	}
}

func buildClauseDriverSQL(c browseFacetClause, isPG bool) (string, []any) {
	var sqlBuf strings.Builder
	var args []any
	switch c.kind {
	case clauseText:
		args = append(args, c.key)
		if isPG {
			sqlBuf.WriteString(`SELECT DISTINCT f.item_ref FROM plugin_item_fields f WHERE f.field = ?`)
			appendPGPrefixIn(&sqlBuf, &args, "f.v_text", c.vals)
		} else if len(c.vals) == 1 {
			args = append(args, c.vals[0])
			sqlBuf.WriteString(`SELECT DISTINCT f.item_ref FROM plugin_item_fields f WHERE f.field = ? AND f.v_text = ?`)
		} else {
			ph := make([]string, len(c.vals))
			for i, v := range c.vals {
				ph[i] = "?"
				args = append(args, v)
			}
			sqlBuf.WriteString(`SELECT DISTINCT f.item_ref FROM plugin_item_fields f WHERE f.field = ? AND f.v_text IN (` + strings.Join(ph, ", ") + `)`)
		}
	case clausePeople:
		args = append(args, c.key)
		if len(c.pids) == 1 {
			args = append(args, c.pids[0])
			sqlBuf.WriteString(`SELECT DISTINCT f.item_ref FROM plugin_item_fields f WHERE f.field = ? AND f.v_int <> 0 AND f.v_int = ?`)
		} else {
			ph := make([]string, len(c.pids))
			for i, pid := range c.pids {
				ph[i] = "?"
				args = append(args, pid)
			}
			sqlBuf.WriteString(`SELECT DISTINCT f.item_ref FROM plugin_item_fields f WHERE f.field = ? AND f.v_int <> 0 AND f.v_int IN (` + strings.Join(ph, ", ") + `)`)
		}
	case clausePersonID:
		if len(c.peopleFields) == 1 {
			args = append(args, c.peopleFields[0], c.pids[0])
			sqlBuf.WriteString(`SELECT DISTINCT f.item_ref FROM plugin_item_fields f WHERE f.field = ? AND f.v_int <> 0 AND f.v_int = ?`)
		} else {
			ph := make([]string, len(c.peopleFields))
			for i, fk := range c.peopleFields {
				ph[i] = "?"
				args = append(args, fk)
			}
			args = append(args, c.pids[0])
			sqlBuf.WriteString(`SELECT DISTINCT f.item_ref FROM plugin_item_fields f WHERE f.field IN (` + strings.Join(ph, ", ") + `) AND f.v_int <> 0 AND f.v_int = ?`)
		}
	case clauseInt, clauseFloat, clauseDateDecade:
		predFor := func(d fieldDisjunct) string {
			if d.partialPred != "" {
				return d.partialPred
			}
			return c.defaultPart
		}
		if len(c.disjuncts) == 1 {
			args = append(args, c.key)
			args = append(args, c.disjuncts[0].args...)
			sqlBuf.WriteString(`SELECT DISTINCT f.item_ref FROM plugin_item_fields f WHERE f.field = ? AND ` + predFor(c.disjuncts[0]) + ` AND ` + c.disjuncts[0].sql)
		} else {
			for i, d := range c.disjuncts {
				if i > 0 {
					sqlBuf.WriteString(` UNION `)
				}
				args = append(args, c.key)
				args = append(args, d.args...)
				sqlBuf.WriteString(`SELECT f.item_ref FROM plugin_item_fields f WHERE f.field = ? AND ` + predFor(d) + ` AND ` + d.sql)
			}
		}
	}
	return sqlBuf.String(), args
}

// appendCorrelatedTextClause emits a correlated EXISTS or NOT EXISTS probe on
// plugin_item_fields(item_ref, field, ...) for text facets.
//   - On PostgreSQL, the LIMIT 1 subquery fence keeps the planner from
//     decorrelating the probe into a Parallel Seq Scan / Hash Semi Join under
//     cardinality skew; single-value probes use md5(v_text) to seek the 3rd
//     column of plugin_item_fields_pkey_idx, while multi-value IN lists match
//     (item_ref, field) directly to avoid hashing every value per item.
//   - On SQLite, multi-value IN lists prefix unary `+` onto f.v_text so SQLite
//     probes the covering PK (item_ref, field) once per item instead of probing
//     plugin_item_fields_lookup len(vals) times per item.
func appendCorrelatedTextClause(where *strings.Builder, args *[]any, fieldKey string, vals []string, negate, isPG bool) {
	if negate {
		where.WriteString(` AND NOT EXISTS (`)
	} else {
		where.WriteString(` AND EXISTS (`)
	}
	*args = append(*args, fieldKey)
	if isPG {
		where.WriteString(`SELECT 1 FROM (SELECT 1 FROM plugin_item_fields f WHERE f.item_ref = i.item_ref AND f.field = ?`)
		if len(vals) == 1 {
			appendPGMD5In(where, args, "f.v_text", vals)
		} else {
			ph := make([]string, len(vals))
			for i, v := range vals {
				ph[i] = "?"
				*args = append(*args, v)
			}
			where.WriteString(` AND f.v_text IN (` + strings.Join(ph, ", ") + `)`)
		}
		where.WriteString(` LIMIT 1) _s)`)
		return
	}
	if len(vals) == 1 {
		*args = append(*args, vals[0])
		where.WriteString(`SELECT 1 FROM plugin_item_fields f WHERE f.item_ref = i.item_ref AND f.field = ? AND f.v_text = ?)`)
		return
	}
	ph := make([]string, len(vals))
	for i, v := range vals {
		ph[i] = "?"
		*args = append(*args, v)
	}
	where.WriteString(`SELECT 1 FROM plugin_item_fields f WHERE f.item_ref = i.item_ref AND f.field = ? AND +f.v_text IN (` + strings.Join(ph, ", ") + `))`)
}

func appendBrowseClauseSQL(where *strings.Builder, args *[]any, c browseFacetClause, types map[string]string, isPG bool) {
	switch c.kind {
	case clauseEnvelope:
		mode := facetAny
		if c.negate {
			mode = facetNone
		}
		appendLibraryFacetMode(where, args, map[string][]string{c.key: c.vals}, types, mode, isPG)
	case clauseDisjunction:
		where.WriteString(" AND (")
		for i, alt := range c.alternatives {
			if i > 0 {
				where.WriteString(" OR ")
			}
			var predicate strings.Builder
			appendBrowseClauseSQL(&predicate, args, alt, types, isPG)
			where.WriteString("(" + strings.TrimPrefix(predicate.String(), " AND ") + ")")
		}
		where.WriteString(")")
	default:
		appendClauseExistsSQL(where, args, c, isPG)
	}
}

func appendClauseExistsSQL(where *strings.Builder, args *[]any, c browseFacetClause, isPG bool) {
	if c.kind == clauseText {
		appendCorrelatedTextClause(where, args, c.key, c.vals, c.negate, isPG)
		return
	}
	if c.negate {
		where.WriteString(` AND NOT EXISTS (`)
	} else {
		where.WriteString(` AND EXISTS (`)
	}
	if isPG {
		where.WriteString(`SELECT 1 FROM (SELECT 1 FROM plugin_item_fields f WHERE f.item_ref = i.item_ref AND `)
	} else {
		where.WriteString(`SELECT 1 FROM plugin_item_fields f WHERE f.item_ref = i.item_ref AND `)
	}

	switch c.kind {
	case clausePeople:
		*args = append(*args, c.key)
		if len(c.pids) == 1 {
			*args = append(*args, c.pids[0])
			where.WriteString(`f.field = ? AND f.v_int <> 0 AND f.v_int = ?`)
		} else {
			ph := make([]string, len(c.pids))
			for i, pid := range c.pids {
				ph[i] = "?"
				*args = append(*args, pid)
			}
			if isPG {
				where.WriteString(`f.field = ? AND f.v_int <> 0 AND f.v_int IN (` + strings.Join(ph, ", ") + `)`)
			} else {
				// Every pid is positive (> 0), so +f.v_int IN (...) implies
				// v_int <> 0 while steering SQLite onto the covering PK
				// (item_ref, field) instead of probing lookup_int N times.
				where.WriteString(`f.field = ? AND +f.v_int IN (` + strings.Join(ph, ", ") + `)`)
			}
		}
	case clausePersonID:
		if len(c.peopleFields) == 1 {
			*args = append(*args, c.peopleFields[0], c.pids[0])
			where.WriteString(`f.field = ? AND f.v_int <> 0 AND f.v_int = ?`)
		} else {
			ph := make([]string, len(c.peopleFields))
			for i, fk := range c.peopleFields {
				ph[i] = "?"
				*args = append(*args, fk)
			}
			*args = append(*args, c.pids[0])
			where.WriteString(`f.field IN (` + strings.Join(ph, ", ") + `) AND f.v_int <> 0 AND f.v_int = ?`)
		}
	case clauseInt, clauseFloat, clauseDateDecade:
		*args = append(*args, c.key)
		predFor := func(d fieldDisjunct) string {
			if d.partialPred != "" {
				return d.partialPred
			}
			return c.defaultPart
		}
		where.WriteString(`f.field = ? AND (`)
		for i, d := range c.disjuncts {
			if i > 0 {
				where.WriteString(` OR `)
			}
			where.WriteString(`(` + predFor(d) + ` AND ` + d.sql + `)`)
			*args = append(*args, d.args...)
		}
		where.WriteString(`)`)
	}

	if isPG {
		where.WriteString(` LIMIT 1) _s)`)
	} else {
		where.WriteString(`)`)
	}
}

type fieldDisjunct struct {
	sql         string
	args        []any
	partialPred string
}

func intRangePartialPred(lo, hi int64) string {
	if lo > 0 || hi < 0 {
		return `f.v_int <> 0`
	}
	return `(f.v_text = '' OR f.v_int <> 0)`
}

func buildDateDecadeDisjuncts(vals []string) []fieldDisjunct {
	var disjuncts []fieldDisjunct
	for _, v := range vals {
		if lo, hi, ok := libraryfilter.ParseDateDecadeBucket(v); ok {
			disjuncts = append(disjuncts, fieldDisjunct{
				sql:         `f.v_int BETWEEN ? AND ?`,
				args:        []any{lo, hi},
				partialPred: intRangePartialPred(lo, hi),
			})
		}
	}
	return disjuncts
}

func buildFloatFieldDisjuncts(vals []string) ([]fieldDisjunct, bool) {
	var disjuncts []fieldDisjunct
	var exactPh []string
	var exactArgs []any
	hasRange := false
	for _, v := range vals {
		if r, ok := libraryfilter.ParseFloatRange(v); ok {
			q, rargs := r.SQL(`f.v_real`)
			disjuncts = append(disjuncts, fieldDisjunct{
				sql:  q,
				args: rargs,
			})
			hasRange = true
			continue
		}
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			continue
		}
		exactPh = append(exactPh, "?")
		exactArgs = append(exactArgs, n)
	}
	if len(exactPh) > 0 {
		disjuncts = append(disjuncts, fieldDisjunct{
			sql:  `f.v_real IN (` + strings.Join(exactPh, ", ") + `)`,
			args: exactArgs,
		})
	}
	return disjuncts, hasRange
}

func browseAllIntRanges(vals []string) bool {
	if len(vals) == 0 {
		return false
	}
	for _, v := range vals {
		if _, ok := libraryfilter.ParseIntRange(v); !ok {
			return false
		}
	}
	return true
}

func buildIntFieldDisjuncts(ftype string, vals []string) ([]fieldDisjunct, bool) {
	var disjuncts []fieldDisjunct
	var exactPh []string
	var exactArgs []any
	exactHasZero := false
	hasRange := false
	for _, v := range vals {
		if ftype == "duration" {
			if lo, hi, ok := libraryfilter.ParseDurationBucket(v); ok {
				if hi == math.MaxInt64 {
					disjuncts = append(disjuncts, fieldDisjunct{
						sql:         `f.v_int >= ?`,
						args:        []any{lo},
						partialPred: intRangePartialPred(lo, hi),
					})
				} else {
					disjuncts = append(disjuncts, fieldDisjunct{
						sql:         `f.v_int BETWEEN ? AND ?`,
						args:        []any{lo, hi},
						partialPred: intRangePartialPred(lo, hi),
					})
				}
				hasRange = true
				continue
			}
		}
		if ftype == "date" {
			if lo, hi, ok := libraryfilter.ParseDateYearBucket(v); ok {
				disjuncts = append(disjuncts, fieldDisjunct{
					sql:         `f.v_int BETWEEN ? AND ?`,
					args:        []any{lo, hi},
					partialPred: intRangePartialPred(lo, hi),
				})
				hasRange = true
				continue
			}
		}
		if r, ok := libraryfilter.ParseIntRange(v); ok {
			q, rargs := r.SQL(`f.v_int`)
			pred := `(f.v_text = '' OR f.v_int <> 0)`
			if r.ExcludesZero() {
				pred = `f.v_int <> 0`
			}
			disjuncts = append(disjuncts, fieldDisjunct{
				sql:         q,
				args:        rargs,
				partialPred: pred,
			})
			hasRange = true
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		if n == 0 {
			exactHasZero = true
		}
		exactPh = append(exactPh, "?")
		exactArgs = append(exactArgs, n)
	}
	if len(exactPh) > 0 {
		pred := `f.v_int <> 0`
		if exactHasZero {
			pred = `(f.v_text = '' OR f.v_int <> 0)`
		}
		disjuncts = append(disjuncts, fieldDisjunct{
			sql:         `f.v_int IN (` + strings.Join(exactPh, ", ") + `)`,
			args:        exactArgs,
			partialPred: pred,
		})
	}
	return disjuncts, hasRange
}

func (s *Service) appendPersonFilter(ctx context.Context, lib core.LibraryID, where *strings.Builder, args *[]any, personID int64) (bool, error) {
	if !core.IsPluginPersonID(personID) {
		return false, nil
	}
	rec, err := s.GetKindByLibrary(ctx, lib)
	if err != nil {
		return false, err
	}
	pFields := declaredPeopleFields(rec.Decl)
	if len(pFields) == 0 {
		return false, nil
	}
	placeholders := make([]string, len(pFields))
	for i, fk := range pFields {
		placeholders[i] = "?"
		*args = append(*args, fk)
	}
	*args = append(*args, personID)
	where.WriteString(` AND EXISTS (SELECT 1 FROM plugin_item_fields pf WHERE pf.item_ref = i.item_ref AND pf.field IN (` + strings.Join(placeholders, ",") + `) AND pf.v_int <> 0 AND pf.v_int = ?)`)
	return true, nil
}

// FacetCollectionsQuery requests a single page of facet-backed collections
// ordered by item count (default) or bucket name, hydrating artwork only for
// the newest item of each bucket on the page in one batched query.
type FacetCollectionsQuery struct {
	LibraryID core.LibraryID
	Level     string
	Policy    *libraryfilter.Filter
	FieldKey  string
	Order     string // "count" (default) | "name"
	Query     string
	Offset    int
	Limit     int
}

// FacetCollection is one facet-backed collection bucket with its newest item.
type FacetCollection struct {
	ID         string         `json:"id"`
	Label      string         `json:"label"`
	Count      int            `json:"count"`
	NewestItem *core.KindItem `json:"-"`
}

// FacetCollectionsPage is a single page of facet collections.
type FacetCollectionsPage struct {
	Items []FacetCollection
	More  bool
}

// FacetCollections returns one page of facet buckets ordered most-items-first
// (default) or alphabetically by name (Order: "name"), resolving the newest
// visible item + artwork across the whole page in one batched lookup.
func (s *Service) FacetCollections(ctx context.Context, q FacetCollectionsQuery) (FacetCollectionsPage, error) {
	if s == nil || s.db == nil {
		return FacetCollectionsPage{}, fmt.Errorf("pluginitems: nil service")
	}
	fieldKey := strings.TrimSpace(q.FieldKey)
	if q.LibraryID <= 0 || fieldKey == "" {
		return FacetCollectionsPage{}, fmt.Errorf("pluginitems: library_id and field_key are required")
	}
	trimmedQuery := strings.TrimSpace(q.Query)
	terms := foldQueryTerms(trimmedQuery)
	if trimmedQuery != "" && len(terms) == 0 {
		return FacetCollectionsPage{Items: []FacetCollection{}}, nil
	}
	rec, err := s.GetKindByLibrary(ctx, q.LibraryID)
	if err != nil {
		return FacetCollectionsPage{}, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := max(q.Offset, 0)
	orderByName := strings.EqualFold(strings.TrimSpace(q.Order), "name")

	itemWhere, fieldDrivenWhere, itemArgs, err := s.buildRootItemPredicates(ctx, rec, q.Level, q.Policy)
	if err != nil {
		return FacetCollectionsPage{}, err
	}
	targetLevel := effectiveRootLevel(rec, q.Level)
	var libTally libraryTallyState
	if q.Policy.Passive() {
		libTally, err = s.libraryTally(ctx, q.LibraryID)
		if err != nil {
			return FacetCollectionsPage{}, err
		}
	}
	var smallLib bool
	if q.Policy.Passive() && libTally.active {
		smallLib = libTally.countFor(targetLevel) <= smallPluginLibraryMax
	} else {
		smallLib, err = s.libraryHasAtMostItems(ctx, itemWhere, itemArgs, smallPluginLibraryMax)
		if err != nil {
			return FacetCollectionsPage{}, err
		}
	}

	fieldTypes := make(map[string]string, len(rec.Decl.Fields))
	var fieldType string
	for _, fd := range rec.Decl.Fields {
		fieldTypes[fd.Key] = fd.Type
		if fd.Key == fieldKey {
			fieldType = fd.Type
		}
	}
	isEnv := envelopeFacet(fieldKey) || (fieldKey == "decade" && fieldType == "")
	isDateDecade := false
	if base, ok := libraryfilter.SplitDateDecadeFacet(fieldKey); ok && fieldTypes[base] == "date" {
		isDateDecade = true
	}
	if !isEnv && !isDateDecade && fieldType == "" {
		return FacetCollectionsPage{Items: []FacetCollection{}}, nil
	}

	var pageBuckets []FacetBucket
	var pagePersonIDs []int64
	var pageRepRefs []string
	more := false

	isPG := store.DialectOf(s.db) == store.Postgres
	switch {
	case !isEnv && !isDateDecade && fieldType == "people":
		if q.Policy.Passive() && libTally.active {
			if libTally.countFor(targetLevel) == 0 {
				break
			}
			var tallyWhere strings.Builder
			tallyWhere.WriteString(`library_id = ? AND level = ? AND field = ?`)
			tallyArgs := []any{int64(q.LibraryID), targetLevel, fieldKey}
			appendFoldedWordPrefixCol(&tallyWhere, &tallyArgs, "label_fold", terms)
			orderClause := `cnt DESC, label_lower ASC, label ASC, v_int ASC`
			if isPG {
				orderClause = `cnt DESC, left(label_lower, 256) ASC, label_lower ASC, label ASC, v_int ASC`
			}
			if orderByName {
				orderClause = `label_lower ASC, label ASC, v_int ASC`
				if isPG {
					orderClause = `left(label_lower, 256) ASC, label_lower ASC, label ASC, v_int ASC`
				}
			}
			tallyArgs = append(tallyArgs, limit+1, offset)
			rows, err := s.db.QueryContext(ctx,
				`SELECT v_int, label, cnt, rep_ref
				 FROM plugin_item_person_tally
				 WHERE `+tallyWhere.String()+`
				 ORDER BY `+orderClause+`
				 LIMIT ? OFFSET ?`, tallyArgs...)
			if err != nil {
				return FacetCollectionsPage{}, err
			}
			for rows.Next() {
				var pid int64
				var label, repRef string
				var cnt int
				if err := rows.Scan(&pid, &label, &cnt, &repRef); err != nil {
					rows.Close()
					return FacetCollectionsPage{}, err
				}
				label = strings.TrimSpace(label)
				if label != "" && cnt > 0 {
					pageBuckets = append(pageBuckets, FacetBucket{ID: label, Label: label, Count: cnt})
					pagePersonIDs = append(pagePersonIDs, pid)
					pageRepRefs = append(pageRepRefs, repRef)
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return FacetCollectionsPage{}, err
			}
			rows.Close()
			if len(pageBuckets) > limit {
				pageBuckets = pageBuckets[:limit]
				pagePersonIDs = pagePersonIDs[:limit]
				pageRepRefs = pageRepRefs[:limit]
				more = true
			}
			break
		}
		if !smallLib && len(terms) == 0 {
			cands, err := s.scanPeopleCandidatesLarge(ctx, fieldKey, fieldDrivenWhere, itemArgs, 2001)
			if err != nil {
				return FacetCollectionsPage{}, err
			}
			if orderByName {
				sort.SliceStable(cands, func(i, j int) bool {
					if !facetNameLess(cands[i].label, cands[j].label) && !facetNameLess(cands[j].label, cands[i].label) {
						return cands[i].pid < cands[j].pid
					}
					return facetNameLess(cands[i].label, cands[j].label)
				})
			} else {
				sort.SliceStable(cands, func(i, j int) bool {
					if cands[i].count != cands[j].count {
						return cands[i].count > cands[j].count
					}
					return facetNameLess(cands[i].label, cands[j].label)
				})
			}
			if offset < len(cands) {
				end := min(offset+limit, len(cands))
				for _, c := range cands[offset:end] {
					pageBuckets = append(pageBuckets, FacetBucket{ID: c.label, Label: c.label, Count: c.count})
					pagePersonIDs = append(pagePersonIDs, c.pid)
				}
				more = end < len(cands)
			}
			break
		}
		orderClause := `cnt DESC, LOWER(MIN(f.v_text)) ASC, MIN(f.v_text) ASC, f.v_int ASC`
		if orderByName {
			orderClause = `LOWER(MIN(f.v_text)) ASC, MIN(f.v_text) ASC, f.v_int ASC`
		}
		var rows *sql.Rows
		if len(terms) > 0 {
			var subWhere strings.Builder
			var subArgs []any
			if isPG {
				subWhere.WriteString(`f.field = ? AND f.v_int > 0 AND f.v_text <> ''`)
			} else {
				subWhere.WriteString(`f.field = ? AND f.v_int + 0 > 0 AND f.v_text <> ''`)
			}
			subArgs = append(subArgs, fieldKey)
			appendFoldedWordPrefixDialect(&subWhere, &subArgs, isPG, terms)
			subArgs = append(subArgs, 50000)
			qArgs := append(subArgs, itemArgs...)
			qArgs = append(qArgs, limit+1, offset)
			rows, err = s.db.QueryContext(ctx,
				`SELECT f.v_int, MIN(f.v_text) AS label, COUNT(*) AS cnt
				 FROM (SELECT item_ref, v_int, v_text FROM plugin_item_fields f WHERE `+subWhere.String()+` LIMIT ?) f
				 JOIN plugin_items i ON i.item_ref = f.item_ref
				 WHERE `+fieldDrivenWhere+`
				 GROUP BY f.v_int
				 ORDER BY `+orderClause+`
				 LIMIT ? OFFSET ?`, qArgs...)
		} else {
			qArgs := append(append([]any(nil), itemArgs...), fieldKey, limit+1, offset)
			rows, err = s.db.QueryContext(ctx,
				`SELECT f.v_int, MIN(f.v_text) AS label, COUNT(*) AS cnt
				 FROM plugin_item_fields f
				 JOIN plugin_items i ON i.item_ref = f.item_ref
				 WHERE `+itemWhere+` AND f.field = ? AND f.v_int <> 0 AND f.v_int > 0 AND f.v_text <> ''
				 GROUP BY f.v_int
				 ORDER BY `+orderClause+`
				 LIMIT ? OFFSET ?`, qArgs...)
		}
		if err != nil {
			return FacetCollectionsPage{}, err
		}
		for rows.Next() {
			var pid int64
			var label string
			var cnt int
			if err := rows.Scan(&pid, &label, &cnt); err != nil {
				rows.Close()
				return FacetCollectionsPage{}, err
			}
			label = strings.TrimSpace(label)
			if label != "" && cnt > 0 {
				pageBuckets = append(pageBuckets, FacetBucket{ID: label, Label: label, Count: cnt})
				pagePersonIDs = append(pagePersonIDs, pid)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return FacetCollectionsPage{}, err
		}
		rows.Close()
		if len(pageBuckets) > limit {
			pageBuckets = pageBuckets[:limit]
			pagePersonIDs = pagePersonIDs[:limit]
			more = true
		}

	case !isEnv && !isDateDecade && (fieldType == "string" || fieldType == "strings" || fieldType == "enum" || fieldType == "bool"):
		if q.Policy.Passive() && libTally.active {
			if libTally.countFor(targetLevel) == 0 {
				break
			}
			var tallyWhere strings.Builder
			tallyWhere.WriteString(`library_id = ? AND level = ? AND field = ?`)
			tallyArgs := []any{int64(q.LibraryID), targetLevel, fieldKey}
			appendFoldedWordPrefixCol(&tallyWhere, &tallyArgs, "v_fold", terms)
			orderClause := `cnt DESC, v_lower ASC, v_text ASC`
			if isPG {
				orderClause = `cnt DESC, left(v_lower, 256) ASC, v_lower ASC, v_text ASC`
			}
			if orderByName {
				orderClause = `v_lower ASC, v_text ASC`
				if isPG {
					orderClause = `left(v_lower, 256) ASC, v_lower ASC, v_text ASC`
				}
			}
			tallyArgs = append(tallyArgs, limit+1, offset)
			rows, err := s.db.QueryContext(ctx,
				`SELECT v_text, cnt, rep_ref
				 FROM plugin_item_field_tally
				 WHERE `+tallyWhere.String()+`
				 ORDER BY `+orderClause+`
				 LIMIT ? OFFSET ?`, tallyArgs...)
			if err != nil {
				return FacetCollectionsPage{}, err
			}
			for rows.Next() {
				var val, repRef string
				var cnt int
				if err := rows.Scan(&val, &cnt, &repRef); err != nil {
					rows.Close()
					return FacetCollectionsPage{}, err
				}
				if val != "" && cnt > 0 {
					pageBuckets = append(pageBuckets, FacetBucket{ID: val, Label: val, Count: cnt})
					pageRepRefs = append(pageRepRefs, repRef)
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return FacetCollectionsPage{}, err
			}
			rows.Close()
			if len(pageBuckets) > limit {
				pageBuckets = pageBuckets[:limit]
				pageRepRefs = pageRepRefs[:limit]
				more = true
			}
			break
		}
		if !smallLib && len(terms) == 0 {
			raw, err := s.scanFieldTextCountsLarge(ctx, fieldKey, fieldDrivenWhere, itemArgs, 2001)
			if err != nil {
				return FacetCollectionsPage{}, err
			}
			if orderByName {
				sort.SliceStable(raw, func(i, j int) bool {
					return facetNameLess(raw[i].Value, raw[j].Value)
				})
			} else {
				sort.SliceStable(raw, func(i, j int) bool {
					if raw[i].Count != raw[j].Count {
						return raw[i].Count > raw[j].Count
					}
					return facetNameLess(raw[i].Value, raw[j].Value)
				})
			}
			if offset < len(raw) {
				end := min(offset+limit, len(raw))
				var refineIdxs []int
				for _, fc := range raw[offset:end] {
					idx := len(pageBuckets)
					pageBuckets = append(pageBuckets, FacetBucket{ID: fc.Value, Label: fc.Value, Count: fc.Count})
					if fc.Count >= 2001 {
						refineIdxs = append(refineIdxs, idx)
					}
				}
				if len(refineIdxs) > 0 {
					arms := make([]string, len(refineIdxs))
					rArgs := make([]any, 0, 2*len(refineIdxs)+1+len(itemArgs))
					for i, bIdx := range refineIdxs {
						if i == 0 {
							arms[i] = `SELECT CAST(? AS INTEGER) AS idx, CAST(? AS TEXT) AS val`
						} else {
							arms[i] = `SELECT ?, ?`
						}
						rArgs = append(rArgs, bIdx, pageBuckets[bIdx].ID)
					}
					rArgs = append(rArgs, fieldKey)
					rArgs = append(rArgs, itemArgs...)
					refineTextCond := `f.v_text = refine.val`
					if isPG {
						refineTextCond = `left(f.v_text, 256) = left(refine.val, 256) AND f.v_text = refine.val`
					}
					if rRows, rErr := s.db.QueryContext(ctx, `
WITH refine(idx, val) AS (
	`+strings.Join(arms, "\n\tUNION ALL\n\t")+`
)
SELECT idx, (
	SELECT COUNT(*) FROM (
		SELECT 1 FROM plugin_item_fields f
		JOIN plugin_items i ON i.item_ref = f.item_ref
		WHERE f.field = ? AND `+refineTextCond+` AND `+fieldDrivenWhere+`
		LIMIT 25001
	) s
) FROM refine`, rArgs...); rErr == nil {
						for rRows.Next() {
							var bIdx, cnt int
							if rRows.Scan(&bIdx, &cnt) == nil && bIdx >= 0 && bIdx < len(pageBuckets) {
								pageBuckets[bIdx].Count = cnt
							}
						}
						rRows.Close()
					}
				}
				more = end < len(raw)
			}
			break
		}
		orderClause := `cnt DESC, LOWER(f.v_text) ASC, f.v_text ASC`
		if orderByName {
			orderClause = `LOWER(f.v_text) ASC, f.v_text ASC`
		}
		var rows *sql.Rows
		if len(terms) > 0 {
			var subWhere strings.Builder
			var subArgs []any
			subWhere.WriteString(`f.field = ? AND f.v_text <> ''`)
			subArgs = append(subArgs, fieldKey)
			appendFoldedWordPrefixDialect(&subWhere, &subArgs, isPG, terms)
			subArgs = append(subArgs, 50000)
			qArgs := append(subArgs, itemArgs...)
			qArgs = append(qArgs, limit+1, offset)
			rows, err = s.db.QueryContext(ctx,
				`SELECT f.v_text, COUNT(*) AS cnt
				 FROM (SELECT item_ref, v_text FROM plugin_item_fields f WHERE `+subWhere.String()+` LIMIT ?) f
				 JOIN plugin_items i ON i.item_ref = f.item_ref
				 WHERE `+fieldDrivenWhere+`
				 GROUP BY f.v_text
				 ORDER BY `+orderClause+`
				 LIMIT ? OFFSET ?`, qArgs...)
		} else {
			qArgs := append(append([]any(nil), itemArgs...), fieldKey, limit+1, offset)
			rows, err = s.db.QueryContext(ctx,
				`SELECT f.v_text, COUNT(*) AS cnt
				 FROM plugin_item_fields f
				 JOIN plugin_items i ON i.item_ref = f.item_ref
				 WHERE `+itemWhere+` AND f.field = ? AND f.v_text <> ''
				 GROUP BY f.v_text
				 ORDER BY `+orderClause+`
				 LIMIT ? OFFSET ?`, qArgs...)
		}
		if err != nil {
			return FacetCollectionsPage{}, err
		}
		for rows.Next() {
			var val string
			var cnt int
			if err := rows.Scan(&val, &cnt); err != nil {
				rows.Close()
				return FacetCollectionsPage{}, err
			}
			if val != "" && cnt > 0 {
				pageBuckets = append(pageBuckets, FacetBucket{ID: val, Label: val, Count: cnt})
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return FacetCollectionsPage{}, err
		}
		rows.Close()
		if len(pageBuckets) > limit {
			pageBuckets = pageBuckets[:limit]
			more = true
		}

	case !isEnv && !isDateDecade && fieldType == "int":
		vals, err := s.scanFieldIntCounts(ctx, fieldKey, itemWhere, fieldDrivenWhere, itemArgs, smallLib)
		if err != nil {
			return FacetCollectionsPage{}, err
		}
		all := make([]FacetBucket, 0, len(vals))
		for _, p := range vals {
			lbl := strconv.FormatInt(p.val, 10)
			if len(terms) > 0 && !foldedWordsMatch(lbl, terms) {
				continue
			}
			all = append(all, FacetBucket{ID: lbl, Label: lbl, Count: p.count})
		}
		if orderByName {
			sort.SliceStable(all, func(i, j int) bool {
				vi, _ := strconv.ParseInt(all[i].ID, 10, 64)
				vj, _ := strconv.ParseInt(all[j].ID, 10, 64)
				return vi < vj
			})
		} else {
			sort.SliceStable(all, func(i, j int) bool {
				if all[i].Count != all[j].Count {
					return all[i].Count > all[j].Count
				}
				vi, _ := strconv.ParseInt(all[i].ID, 10, 64)
				vj, _ := strconv.ParseInt(all[j].ID, 10, 64)
				return vi < vj
			})
		}
		if offset < len(all) {
			end := min(offset+limit, len(all))
			pageBuckets = all[offset:end]
			more = end < len(all)
		}

	case !isEnv && !isDateDecade && fieldType == "float":
		vals, err := s.scanFieldFloatCounts(ctx, fieldKey, itemWhere, fieldDrivenWhere, itemArgs, smallLib)
		if err != nil {
			return FacetCollectionsPage{}, err
		}
		all := make([]FacetBucket, 0, len(vals))
		for _, p := range vals {
			lbl := strconv.FormatFloat(p.val, 'f', -1, 64)
			if len(terms) > 0 && !foldedWordsMatch(lbl, terms) {
				continue
			}
			all = append(all, FacetBucket{ID: lbl, Label: lbl, Count: p.count})
		}
		if orderByName {
			sort.SliceStable(all, func(i, j int) bool {
				vi, _ := strconv.ParseFloat(all[i].ID, 64)
				vj, _ := strconv.ParseFloat(all[j].ID, 64)
				return vi < vj
			})
		} else {
			sort.SliceStable(all, func(i, j int) bool {
				if all[i].Count != all[j].Count {
					return all[i].Count > all[j].Count
				}
				vi, _ := strconv.ParseFloat(all[i].ID, 64)
				vj, _ := strconv.ParseFloat(all[j].ID, 64)
				return vi < vj
			})
		}
		if offset < len(all) {
			end := min(offset+limit, len(all))
			pageBuckets = all[offset:end]
			more = end < len(all)
		}

	default:
		all, err := s.FacetBuckets(ctx, q.LibraryID, q.Level, q.Policy, fieldKey)
		if err != nil {
			return FacetCollectionsPage{}, err
		}
		if len(terms) > 0 {
			filtered := all[:0]
			for _, b := range all {
				if foldedWordsMatch(b.Label, terms) || foldedWordsMatch(b.ID, terms) {
					filtered = append(filtered, b)
				}
			}
			all = filtered
		}
		if orderByName {
			sort.SliceStable(all, func(i, j int) bool {
				return facetNameLess(all[i].Label, all[j].Label)
			})
		} else {
			sort.SliceStable(all, func(i, j int) bool {
				if all[i].Count != all[j].Count {
					return all[i].Count > all[j].Count
				}
				return facetNameLess(all[i].Label, all[j].Label)
			})
		}
		if offset < len(all) {
			end := min(offset+limit, len(all))
			pageBuckets = all[offset:end]
			more = end < len(all)
		}
	}

	if len(pageBuckets) == 0 {
		return FacetCollectionsPage{Items: []FacetCollection{}}, nil
	}

	var newest []*core.KindItem
	if len(pageRepRefs) == len(pageBuckets) {
		byRef, err := s.resolveItemsByRefs(ctx, pageRepRefs, true)
		if err != nil {
			return FacetCollectionsPage{}, err
		}
		newest = make([]*core.KindItem, len(pageBuckets))
		var missingIdxs []int
		for i, ref := range pageRepRefs {
			if it, ok := byRef[ref]; ok {
				cp := it
				newest[i] = &cp
			} else {
				missingIdxs = append(missingIdxs, i)
			}
		}
		if len(missingIdxs) > 0 {
			missBuckets := make([]FacetBucket, len(missingIdxs))
			var missPIDs []int64
			for j, idx := range missingIdxs {
				missBuckets[j] = pageBuckets[idx]
				if len(pagePersonIDs) == len(pageBuckets) {
					missPIDs = append(missPIDs, pagePersonIDs[idx])
				}
			}
			var filled []*core.KindItem
			if !isEnv && !isDateDecade && fieldType == "people" {
				filled, err = s.resolveNewestByPersonIDs(ctx, q.LibraryID, fieldKey, missPIDs, missBuckets, fieldDrivenWhere, itemArgs, libTally, targetLevel, q.Policy.Passive())
			} else {
				filled, err = s.resolveNewestByTextBuckets(ctx, q.LibraryID, fieldKey, missBuckets, fieldDrivenWhere, itemArgs, libTally, targetLevel, q.Policy.Passive())
			}
			if err != nil {
				return FacetCollectionsPage{}, err
			}
			for j, idx := range missingIdxs {
				if j < len(filled) && filled[j] != nil {
					newest[idx] = filled[j]
				}
			}
		}
	} else {
		switch {
		case !isEnv && !isDateDecade && fieldType == "people":
			newest, err = s.resolveNewestByPersonIDs(ctx, q.LibraryID, fieldKey, pagePersonIDs, pageBuckets, fieldDrivenWhere, itemArgs, libTally, targetLevel, q.Policy.Passive())
		case !isEnv && !isDateDecade && (fieldType == "string" || fieldType == "strings" || fieldType == "enum" || fieldType == "bool"):
			newest, err = s.resolveNewestByTextBuckets(ctx, q.LibraryID, fieldKey, pageBuckets, fieldDrivenWhere, itemArgs, libTally, targetLevel, q.Policy.Passive())
		case !isEnv && !isDateDecade && fieldType == "int":
			newest, err = s.resolveNewestByIntBuckets(ctx, fieldKey, pageBuckets, fieldDrivenWhere, itemArgs)
		case !isEnv && !isDateDecade && fieldType == "float":
			newest, err = s.resolveNewestByFloatBuckets(ctx, fieldKey, pageBuckets, fieldDrivenWhere, itemArgs)
		default:
			newest, err = s.resolveNewestByPredicateBuckets(ctx, fieldKey, fieldTypes, pageBuckets, itemWhere, itemArgs)
		}
		if err != nil {
			return FacetCollectionsPage{}, err
		}
	}

	out := make([]FacetCollection, len(pageBuckets))
	for i, b := range pageBuckets {
		out[i] = FacetCollection{
			ID:    b.ID,
			Label: b.Label,
			Count: b.Count,
		}
		if i < len(newest) && newest[i] != nil {
			out[i].NewestItem = newest[i]
		}
	}
	return FacetCollectionsPage{Items: out, More: more}, nil
}
