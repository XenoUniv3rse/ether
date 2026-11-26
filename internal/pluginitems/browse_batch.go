package pluginitems

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

type browseBatchEntry struct {
	idx         int
	q           BrowseQuery
	limit       int
	col         string
	kind        string
	desc        bool
	defaultDesc bool
	level       string
	parentRef   string
	tally       browseTallyInfo
	plan        browseQueryPlan
	empty       bool
}

// BrowseBatch executes a batch of BrowseQuery shelves for the same library
// while sharing kind lookup, plugin_item_tally, batched plugin_item_field_tally
// probes, batched UNION ALL item_ref selection, deduplicated plugin_items row
// fetches, and a single batched plugin_item_art hydration. Queries that do not
// qualify for batching (cursor pagination, total counts, last_viewed, hidden,
// or mixed libraries) fall back transparently to Browse.
func (s *Service) BrowseBatch(ctx context.Context, queries []BrowseQuery) ([]BrowsePage, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("pluginitems: nil service")
	}
	if len(queries) == 0 {
		return nil, nil
	}
	if len(queries) == 1 {
		p, err := s.Browse(ctx, queries[0])
		if err != nil {
			return nil, err
		}
		return []BrowsePage{p}, nil
	}

	out := make([]BrowsePage, len(queries))
	libID := queries[0].LibraryID
	if libID <= 0 {
		return nil, fmt.Errorf("pluginitems: library_id is required")
	}

	var (
		eligible  []browseBatchEntry
		fallbacks []int
	)
	for i, q := range queries {
		if q.LibraryID != libID {
			fallbacks = append(fallbacks, i)
			continue
		}
		col, kind, desc, defaultDesc := parseBrowseSort(q)
		if !q.NoTotal || q.CheapTotal || strings.TrimSpace(q.Cursor) != "" || q.Hidden || kind == "last_viewed" {
			fallbacks = append(fallbacks, i)
			continue
		}
		if err := checkFacetValueBound(q.AnyGroups, q.Facets, q.AllFacets, q.ExcludeFacets); err != nil {
			return nil, err
		}
		limit := q.Limit
		if limit <= 0 {
			limit = DefaultBrowseLimit
		}
		if limit > MaxBrowseLimit {
			limit = MaxBrowseLimit
		}
		eligible = append(eligible, browseBatchEntry{
			idx:         i,
			q:           q,
			limit:       limit,
			col:         col,
			kind:        kind,
			desc:        desc,
			defaultDesc: defaultDesc,
			level:       strings.TrimSpace(q.Level),
			parentRef:   strings.TrimSpace(string(q.ParentRef)),
		})
	}

	if len(eligible) > 0 {
		if err := s.resolveBrowseBatchTallies(ctx, libID, eligible); err != nil {
			return nil, err
		}

		var active []int
		for i := range eligible {
			e := &eligible[i]
			if len(e.q.Empty) > 0 {
				var err error
				e.tally, err = s.probeBrowsePresence(ctx, e.q, e.parentRef, e.level)
				if err != nil {
					return nil, err
				}
			}
			if (e.tally.totalExact && e.tally.total == 0) || e.tally.knownEmpty {
				e.empty = true
				out[e.idx] = BrowsePage{Items: []core.KindItem{}}
				continue
			}
			plan, err := s.buildBrowsePlan(ctx, e.q, e.parentRef, e.level, e.col, e.kind, e.desc, e.defaultDesc, e.limit, e.tally)
			if err != nil {
				return nil, err
			}
			if plan.empty {
				e.empty = true
				out[e.idx] = BrowsePage{Items: []core.KindItem{}}
				continue
			}
			e.plan = plan
			active = append(active, i)
		}

		if len(active) > 0 {
			refsByEntry := make([][]core.ItemRef, len(eligible))
			uniqueRefs := make([]core.ItemRef, 0, len(active)*8)
			seenRef := make(map[core.ItemRef]struct{}, len(active)*8)
			isPG := store.DialectOf(s.db) == store.Postgres

			const maxUnionArms = 40
			for start := 0; start < len(active); start += maxUnionArms {
				chunk := active[start:min(start+maxUnionArms, len(active))]
				arms := make([]string, len(chunk))
				var cteDefs []string
				var cteArgs []any
				var armArgs []any
				for armIdx, ei := range chunk {
					e := &eligible[ei]
					dir := "ASC"
					if e.desc {
						dir = "DESC"
					}
					var subSQL string
					if e.plan.matchSort != "" {
						subSQL = e.plan.matchFirstPageSQL("WITH ", "i.item_ref, i."+e.col+" AS sort_col", e.plan.baseWhere,
							"i."+e.col+" "+dir+", i.item_ref ASC")
					} else if e.plan.driver == nil {
						subSQL = fmt.Sprintf(
							`SELECT i.item_ref, i.%s AS sort_col FROM plugin_items i WHERE %s ORDER BY i.%s %s, i.item_ref ASC LIMIT ?`,
							e.col, e.plan.baseWhere, e.col, dir,
						)
					} else {
						drvName := fmt.Sprintf("_drv_%d", armIdx)
						if isPG {
							cteDefs = append(cteDefs, fmt.Sprintf("%s AS MATERIALIZED (\n\t%s\n)", drvName, e.plan.driver.cteSQL))
							subSQL = fmt.Sprintf(
								`SELECT i.item_ref, i.%s AS sort_col FROM %s _d CROSS JOIN LATERAL (SELECT i.item_ref, i.%s FROM plugin_items i WHERE i.item_ref = _d.item_ref AND %s LIMIT 1) i ORDER BY i.%s %s, i.item_ref ASC LIMIT ?`,
								e.col, drvName, e.col, e.plan.baseWhere, e.col, dir,
							)
						} else {
							cteDefs = append(cteDefs, fmt.Sprintf("%s(item_ref) AS (\n\t%s\n)", drvName, e.plan.driver.cteSQL))
							subSQL = fmt.Sprintf(
								`SELECT i.item_ref, i.%s AS sort_col FROM %s _d CROSS JOIN plugin_items i WHERE i.item_ref = _d.item_ref AND %s ORDER BY i.%s %s, i.item_ref ASC LIMIT ?`,
								e.col, drvName, e.plan.baseWhere, e.col, dir,
							)
						}
						cteArgs = append(cteArgs, e.plan.driver.cteArgs...)
					}
					arms[armIdx] = fmt.Sprintf(
						`SELECT * FROM (SELECT CAST(? AS INTEGER) AS shelf_idx, sub.item_ref, ROW_NUMBER() OVER (ORDER BY sub.sort_col %s, sub.item_ref ASC) AS pos FROM (%s) sub) arm%d`,
						dir, subSQL, armIdx,
					)
					armArgs = append(armArgs, ei)
					armArgs = append(armArgs, e.plan.baseArgs...)
					armArgs = append(armArgs, e.limit+1)
				}
				cteDefs = append(cteDefs, "batch AS (\n\t"+strings.Join(arms, "\n\tUNION ALL\n\t")+"\n)")
				querySQL := "WITH " + strings.Join(cteDefs, ",\n") + `
SELECT shelf_idx, item_ref, pos
FROM batch
ORDER BY shelf_idx ASC, pos ASC`
				args := make([]any, 0, len(cteArgs)+len(armArgs))
				args = append(args, cteArgs...)
				args = append(args, armArgs...)

				rows, err := s.db.QueryContext(ctx, querySQL, args...)
				if err != nil {
					return nil, err
				}
				for rows.Next() {
					var ei int
					var refStr string
					var pos int
					if err := rows.Scan(&ei, &refStr, &pos); err != nil {
						rows.Close()
						return nil, err
					}
					if ei < 0 || ei >= len(eligible) {
						continue
					}
					ref := core.ItemRef(refStr)
					refsByEntry[ei] = append(refsByEntry[ei], ref)
					if _, ok := seenRef[ref]; !ok {
						seenRef[ref] = struct{}{}
						uniqueRefs = append(uniqueRefs, ref)
					}
				}
				if err := rows.Err(); err != nil {
					rows.Close()
					return nil, err
				}
				rows.Close()
			}

			itemsByRef, err := s.fetchItemsByRefsWithArt(ctx, uniqueRefs)
			if err != nil {
				return nil, err
			}

			for _, ei := range active {
				e := &eligible[ei]
				refs := refsByEntry[ei]
				if len(refs) == 0 {
					out[e.idx] = BrowsePage{Items: []core.KindItem{}}
					continue
				}
				var nextCursor string
				if len(refs) > e.limit {
					if last, ok := itemsByRef[refs[e.limit-1]]; ok {
						cur := browseCursor{
							Sort:    e.col,
							Desc:    e.desc,
							ItemRef: string(last.Ref),
						}
						switch e.col {
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
					refs = refs[:e.limit]
				}
				items := make([]core.KindItem, 0, len(refs))
				for _, ref := range refs {
					if it, ok := itemsByRef[ref]; ok {
						items = append(items, it)
					}
				}
				out[e.idx] = BrowsePage{
					Items:      items,
					Total:      len(items),
					TotalExact: false,
					NextCursor: nextCursor,
				}
			}
		}
	}

	for _, i := range fallbacks {
		p, err := s.Browse(ctx, queries[i])
		if err != nil {
			return nil, err
		}
		out[i] = p
	}
	return out, nil
}

// resolveBrowseBatchTallies computes browseTallyInfo for every eligible batch
// entry using one memoized plugin_item_tally read and batched PK-prefix queries
// on plugin_item_field_tally / plugin_item_person_tally grouped by (level, field).
func (s *Service) resolveBrowseBatchTallies(ctx context.Context, libraryID core.LibraryID, entries []browseBatchEntry) error {
	libTally, err := s.libraryTally(ctx, libraryID)
	if err != nil {
		return err
	}
	if !libTally.active {
		return nil
	}
	libTotal := libTally.libTotal

	kindRec, kindErr := s.GetKindByLibrary(ctx, libraryID)
	fieldTypes := map[string]string{}
	var declaredPeople []string
	if kindErr == nil {
		for _, fd := range kindRec.Decl.Fields {
			fieldTypes[fd.Key] = fd.Type
		}
		declaredPeople = declaredPeopleFields(kindRec.Decl)
	}

	type pendingSingleProbe struct {
		entryIdx     int
		levels       []string // {""} for anyLevel
		field        string
		vals         []string
		mode         facetMode
		singleValued bool
		levelTotal   int
		singleLevel  bool
	}
	type pendingMultiProbe struct {
		entryIdx    int
		levels      []string // {""} for anyLevel
		active      []activeBrowseFacet
		personID    int64
		singleLevel bool
	}
	type groupKey struct {
		level string
		field string
	}
	textGroups := map[groupKey]map[string]struct{}{}
	personGroups := map[groupKey]map[int64]struct{}{}
	addTextGroup := func(tallyLevels []string, field string, vals []string) {
		for _, lv := range tallyLevels {
			gk := groupKey{level: lv, field: field}
			set := textGroups[gk]
			if set == nil {
				set = map[string]struct{}{}
				textGroups[gk] = set
			}
			for _, v := range vals {
				set[v] = struct{}{}
			}
		}
	}
	addPersonGroup := func(tallyLevels []string, field string, pids []int64) {
		for _, lv := range tallyLevels {
			gk := groupKey{level: lv, field: field}
			set := personGroups[gk]
			if set == nil {
				set = map[int64]struct{}{}
				personGroups[gk] = set
			}
			for _, pid := range pids {
				set[pid] = struct{}{}
			}
		}
	}

	var (
		pendingSingle []pendingSingleProbe
		pendingMulti  []pendingMultiProbe
	)

	for i := range entries {
		e := &entries[i]
		q := e.q
		if e.parentRef != "" || len(q.Empty) > 0 || hasActiveExcludeQuery(q.ExcludeQuery) {
			continue
		}
		noExtraFilters := q.Policy.Passive() && q.TabPolicy.Passive() && !q.Hidden && q.PersonID <= 0 && q.Watched == nil && q.MaxContentRating == "" && len(q.ExcludeKeys) == 0

		var (
			levels        []string
			anyLevel      bool
			defaultedRoot bool
		)
		switch {
		case len(q.Levels) > 0:
			levels = dedupCleanValues(q.Levels)
			if len(levels) == 0 {
				continue
			}
		case e.level != "":
			levels = []string{e.level}
		case q.AnyLevel || q.PersonID > 0:
			anyLevel = true
		default:
			if kindErr != nil || len(kindRec.Decl.Levels) == 0 {
				continue
			}
			defaultedRoot = true
			levels = []string{kindRec.Decl.Levels[0].Key}
		}

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
				e.tally = browseTallyInfo{totalExact: true, total: levelTotal, libTotal: libTotal, singleLevel: singleLevel}
			} else {
				e.tally = browseTallyInfo{libTotal: libTotal, singleLevel: singleLevel}
			}
			continue
		}
		if kindErr != nil {
			e.tally = browseTallyInfo{libTotal: libTotal, singleLevel: singleLevel}
			continue
		}

		tallyLevels := levels
		if anyLevel {
			tallyLevels = []string{""}
		}

		if noExtraFilters && len(active) == 1 {
			f := active[0]
			if !envelopeFacet(f.key) && isTalliedTextType(fieldTypes[f.key]) {
				singleValued := isSingleValuedTextType(fieldTypes[f.key])
				if singleValued && f.mode == facetAll && len(f.vals) > 1 {
					e.tally = browseTallyInfo{totalExact: true, total: 0, knownEmpty: true, libTotal: libTotal, singleLevel: singleLevel}
					continue
				}
				if singleValued || len(f.vals) == 1 || f.mode == facetAny {
					addTextGroup(tallyLevels, f.key, f.vals)
					pendingSingle = append(pendingSingle, pendingSingleProbe{
						entryIdx:     i,
						levels:       tallyLevels,
						field:        f.key,
						vals:         f.vals,
						mode:         f.mode,
						singleValued: singleValued,
						levelTotal:   levelTotal,
						singleLevel:  singleLevel,
					})
					continue
				}
			}
		}

		contradiction := false
		for _, a := range active {
			if a.mode == facetNone || envelopeFacet(a.key) {
				continue
			}
			ft := fieldTypes[a.key]
			if isSingleValuedTextType(ft) && a.mode == facetAll && len(a.vals) > 1 {
				contradiction = true
				break
			}
			if isTalliedTextType(ft) {
				addTextGroup(tallyLevels, a.key, a.vals)
				continue
			}
			if ft == "people" {
				pids := parsePeopleFacetIDs(a.vals)
				if len(pids) == 0 {
					contradiction = true
					break
				}
				addPersonGroup(tallyLevels, a.key, pids)
			}
		}
		if !contradiction && q.PersonID > 0 {
			if !core.IsPluginPersonID(q.PersonID) || len(declaredPeople) == 0 {
				contradiction = true
			} else {
				for _, fk := range declaredPeople {
					addPersonGroup(tallyLevels, fk, []int64{q.PersonID})
				}
			}
		}
		if contradiction {
			e.tally = browseTallyInfo{knownEmpty: true, totalExact: true, total: 0, libTotal: libTotal, singleLevel: singleLevel}
			continue
		}
		pendingMulti = append(pendingMulti, pendingMultiProbe{
			entryIdx:    i,
			levels:      tallyLevels,
			active:      active,
			personID:    q.PersonID,
			singleLevel: singleLevel,
		})
	}

	if len(pendingSingle) == 0 && len(pendingMulti) == 0 {
		return nil
	}

	// Execute one batched PK-prefix query per (level, field) group on field tallies.
	type textRowKey struct {
		level string
		field string
		val   string
	}
	textCounts := make(map[textRowKey]int, (len(pendingSingle)+len(pendingMulti))*2)
	if len(textGroups) > 0 {
		groupKeys := make([]groupKey, 0, len(textGroups))
		for gk := range textGroups {
			groupKeys = append(groupKeys, gk)
		}
		slices.SortFunc(groupKeys, func(a, b groupKey) int {
			if c := strings.Compare(a.level, b.level); c != 0 {
				return c
			}
			return strings.Compare(a.field, b.field)
		})

		isPG := store.DialectOf(s.db) == store.Postgres
		const chunkSize = 250
		for _, gk := range groupKeys {
			vals := make([]string, 0, len(textGroups[gk]))
			for v := range textGroups[gk] {
				vals = append(vals, v)
			}
			slices.Sort(vals)
			for start := 0; start < len(vals); start += chunkSize {
				chunk := vals[start:min(start+chunkSize, len(vals))]
				var qSQL strings.Builder
				args := make([]any, 0, 3+2*len(chunk))
				qSQL.WriteString(`SELECT v_text, cnt FROM plugin_item_field_tally WHERE library_id = ? AND level = ? AND field = ?`)
				args = append(args, int64(libraryID), gk.level, gk.field)
				if isPG {
					appendPGMD5In(&qSQL, &args, "v_text", chunk)
				} else {
					ph := make([]string, len(chunk))
					for i, v := range chunk {
						ph[i] = "?"
						args = append(args, v)
					}
					qSQL.WriteString(` AND v_text IN (` + strings.Join(ph, ",") + `)`)
				}
				rows, err := s.db.QueryContext(ctx, qSQL.String(), args...)
				if err != nil {
					return err
				}
				for rows.Next() {
					var v string
					var cnt int
					if err := rows.Scan(&v, &cnt); err != nil {
						rows.Close()
						return err
					}
					textCounts[textRowKey{level: gk.level, field: gk.field, val: v}] = cnt
				}
				if err := rows.Err(); err != nil {
					rows.Close()
					return err
				}
				rows.Close()
			}
		}
	}

	// Execute one batched PK-prefix query per (level, field) group on person tallies.
	type personRowKey struct {
		level string
		field string
		pid   int64
	}
	personCounts := make(map[personRowKey]int, len(personGroups)*2)
	if len(personGroups) > 0 {
		groupKeys := make([]groupKey, 0, len(personGroups))
		for gk := range personGroups {
			groupKeys = append(groupKeys, gk)
		}
		slices.SortFunc(groupKeys, func(a, b groupKey) int {
			if c := strings.Compare(a.level, b.level); c != 0 {
				return c
			}
			return strings.Compare(a.field, b.field)
		})
		const chunkSize = 250
		for _, gk := range groupKeys {
			pids := make([]int64, 0, len(personGroups[gk]))
			for pid := range personGroups[gk] {
				pids = append(pids, pid)
			}
			slices.Sort(pids)
			for start := 0; start < len(pids); start += chunkSize {
				chunk := pids[start:min(start+chunkSize, len(pids))]
				ph := make([]string, len(chunk))
				args := make([]any, 0, 3+len(chunk))
				args = append(args, int64(libraryID), gk.level, gk.field)
				for i, pid := range chunk {
					ph[i] = "?"
					args = append(args, pid)
				}
				rows, err := s.db.QueryContext(ctx,
					`SELECT v_int, cnt FROM plugin_item_person_tally WHERE library_id = ? AND level = ? AND field = ? AND v_int IN (`+strings.Join(ph, ",")+`)`,
					args...)
				if err != nil {
					return err
				}
				for rows.Next() {
					var pid int64
					var cnt int
					if err := rows.Scan(&pid, &cnt); err != nil {
						rows.Close()
						return err
					}
					personCounts[personRowKey{level: gk.level, field: gk.field, pid: pid}] = cnt
				}
				if err := rows.Err(); err != nil {
					rows.Close()
					return err
				}
				rows.Close()
			}
		}
	}

	for _, p := range pendingSingle {
		multiOrMultiVal := !p.singleValued && len(p.vals) > 1 && p.mode == facetAny
		perEntryText := make(map[browseTextTallyKey]int, len(p.vals))
		var sumCnt, maxCnt int
		for _, v := range p.vals {
			valCnt := 0
			for _, lv := range p.levels {
				valCnt += textCounts[textRowKey{level: lv, field: p.field, val: v}]
			}
			perEntryText[browseTextTallyKey{field: p.field, val: v}] = valCnt
			sumCnt += valCnt
			if valCnt > maxCnt {
				maxCnt = valCnt
			}
		}
		if multiOrMultiVal {
			if sumCnt == 0 {
				entries[p.entryIdx].tally = browseTallyInfo{totalExact: true, total: 0, knownEmpty: true, libTotal: libTotal, singleLevel: p.singleLevel}
			} else {
				entries[p.entryIdx].tally = browseTallyInfo{
					lowerBound:         maxCnt,
					libTotal:           libTotal,
					singleLevel:        p.singleLevel,
					positiveTextField:  p.field,
					positiveTextValues: p.vals,
					talliesActive:      true,
					textCounts:         perEntryText,
				}
			}
			continue
		}
		if p.mode == facetNone {
			entries[p.entryIdx].tally = browseTallyInfo{
				totalExact:    true,
				total:         max(0, p.levelTotal-sumCnt),
				libTotal:      libTotal,
				singleLevel:   p.singleLevel,
				talliesActive: true,
				textCounts:    perEntryText,
			}
			continue
		}
		entries[p.entryIdx].tally = browseTallyInfo{
			totalExact:         true,
			total:              sumCnt,
			knownEmpty:         sumCnt == 0,
			libTotal:           libTotal,
			singleLevel:        p.singleLevel,
			positiveTextField:  p.field,
			positiveTextValues: p.vals,
			talliesActive:      true,
			textCounts:         perEntryText,
		}
	}

	for _, p := range pendingMulti {
		perEntryText := map[browseTextTallyKey]int{}
		perEntryPerson := map[browsePersonTallyKey]int{}
		knownEmpty := false
		for _, a := range p.active {
			if a.mode == facetNone || envelopeFacet(a.key) {
				continue
			}
			ft := fieldTypes[a.key]
			if isTalliedTextType(ft) {
				sumCnt := 0
				for _, v := range a.vals {
					valCnt := 0
					for _, lv := range p.levels {
						valCnt += textCounts[textRowKey{level: lv, field: a.key, val: v}]
					}
					perEntryText[browseTextTallyKey{field: a.key, val: v}] = valCnt
					if a.mode == facetAll && valCnt == 0 {
						knownEmpty = true
					}
					sumCnt += valCnt
				}
				if sumCnt == 0 {
					knownEmpty = true
				}
				continue
			}
			if ft == "people" {
				for _, pid := range parsePeopleFacetIDs(a.vals) {
					valCnt := 0
					for _, lv := range p.levels {
						valCnt += personCounts[personRowKey{level: lv, field: a.key, pid: pid}]
					}
					perEntryPerson[browsePersonTallyKey{field: a.key, pid: pid}] = valCnt
				}
			}
		}
		if p.personID > 0 {
			for _, fk := range declaredPeople {
				valCnt := 0
				for _, lv := range p.levels {
					valCnt += personCounts[personRowKey{level: lv, field: fk, pid: p.personID}]
				}
				perEntryPerson[browsePersonTallyKey{field: fk, pid: p.personID}] = valCnt
			}
		}
		if knownEmpty {
			entries[p.entryIdx].tally = browseTallyInfo{knownEmpty: true, totalExact: true, total: 0, libTotal: libTotal, singleLevel: p.singleLevel}
			continue
		}
		entries[p.entryIdx].tally = browseTallyInfo{
			libTotal:      libTotal,
			singleLevel:   p.singleLevel,
			talliesActive: true,
			textCounts:    perEntryText,
			personCounts:  perEntryPerson,
		}
	}
	return nil
}

func (s *Service) fetchItemsByRefsWithArt(ctx context.Context, refs []core.ItemRef) (map[core.ItemRef]core.KindItem, error) {
	if len(refs) == 0 {
		return map[core.ItemRef]core.KindItem{}, nil
	}
	items := make([]core.KindItem, 0, len(refs))
	const chunkSize = 250
	for start := 0; start < len(refs); start += chunkSize {
		chunk := refs[start:min(start+chunkSize, len(refs))]
		ph := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, r := range chunk {
			ph[i] = "?"
			args[i] = string(r)
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT `+itemCols+` FROM plugin_items WHERE item_ref IN (`+strings.Join(ph, ",")+`)`,
			args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			it, _, _, err := scanItemRow(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			items = append(items, it)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	if len(items) > 0 {
		if err := s.hydrateArtSlice(ctx, items); err != nil {
			return nil, err
		}
	}
	byRef := make(map[core.ItemRef]core.KindItem, len(items))
	for _, it := range items {
		byRef[it.Ref] = it
	}
	return byRef, nil
}
