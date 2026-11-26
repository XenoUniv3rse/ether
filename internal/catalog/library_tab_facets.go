package catalog

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	// One line: the tier migration recreates catalogue indexes from their
	// stored SQL, normalised. Popularity stays out of catalog_titles indexes
	// (titleidx.go): it churns on every TMDB refresh.
	store.Register("catalog", 85, `CREATE INDEX catalog_titles_collection_newest ON catalog_titles(collection_id,released_ts DESC,key) WHERE collection_id>0;`)
}

// LibraryFacetCollections returns a bounded, indexed page of facet buckets
// (genre, studio, network, country, year, decade, or franchise) scoped to the
// library/viewer Filter, ordered by item count (default) or name, with folded
// search pushed into SQL or bounded tally sets, and resolves each bucket's
// newest visible title in one batched lookup.
func (c *Catalog) LibraryFacetCollections(ctx context.Context, q core.CatalogFacetCollectionsQuery) (core.CatalogFacetCollectionsPage, error) {
	facet, ok := core.NormalizeCatalogueTabFacet(core.TabCollections, q.Facet)
	if !ok || facet == "" {
		return core.CatalogFacetCollectionsPage{Items: []core.CatalogFacetCollection{}}, nil
	}
	storageFacet := facet
	if storageFacet == "network" {
		storageFacet = "studio"
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
	rawQuery := strings.TrimSpace(q.Query)
	foldedQuery := foldPersonQueryTerm(rawQuery)

	where, args, err := c.browseWhere(q.Filter)
	if err != nil {
		return core.CatalogFacetCollectionsPage{}, err
	}

	var pageBuckets []core.CatalogFacetCollection
	more := false

	if isExtraCatalogTabFacet(storageFacet) {
		buckets, hasMore, qErr := c.libraryFacetCollectionsExtra(ctx, q.Filter, storageFacet, orderByName, rawQuery, foldedQuery, offset, limit, where, args)
		if qErr != nil {
			return core.CatalogFacetCollectionsPage{}, qErr
		}
		pageBuckets = buckets
		more = hasMore
	} else if storageFacet == "franchise" {
		if buckets, hasMore, usedIndex, fErr := c.libraryFacetFranchises(ctx, q.Filter, orderByName, rawQuery, offset, limit, where, args); fErr != nil {
			return core.CatalogFacetCollectionsPage{}, fErr
		} else if usedIndex {
			pageBuckets = buckets
			more = hasMore
		}
	} else {
		if buckets, hasMore, usedTally, tErr := c.libraryFacetCollectionsFromTally(ctx, q.Filter, storageFacet, orderByName, rawQuery, foldedQuery, offset, limit, where, args); tErr != nil {
			return core.CatalogFacetCollectionsPage{}, tErr
		} else if usedTally {
			pageBuckets = buckets
			more = hasMore
		}
	}

	if pageBuckets == nil {
		buckets, hasMore, qErr := c.libraryFacetCollectionsSQL(ctx, storageFacet, orderByName, rawQuery, foldedQuery, offset, limit, where, args)
		if qErr != nil {
			return core.CatalogFacetCollectionsPage{}, qErr
		}
		pageBuckets = buckets
		more = hasMore
	}

	if len(pageBuckets) == 0 {
		return core.CatalogFacetCollectionsPage{Items: []core.CatalogFacetCollection{}}, nil
	}

	if err := c.hydrateLibraryFacetNewest(ctx, q.Filter, storageFacet, pageBuckets, where, args); err != nil {
		return core.CatalogFacetCollectionsPage{}, err
	}
	return core.CatalogFacetCollectionsPage{Items: pageBuckets, More: more}, nil
}

func (c *Catalog) libraryFacetCollectionsFromTally(
	ctx context.Context,
	f core.Filter,
	storageFacet string,
	orderByName bool,
	rawQuery, foldedQuery string,
	offset, limit int,
	where string,
	args []any,
) ([]core.CatalogFacetCollection, bool, bool, error) {
	if storageFacet == "studio" && rawQuery != "" {
		// Let studioNamesLike drive a narrow vocabulary query rather than reading studioAll.
		return nil, false, false, nil
	}
	tallyName := storageFacet
	switch storageFacet {
	case "country":
		tallyName = "countryAll"
	case "studio":
		if orderByName || offset+limit > 200 {
			tallyName = "studioAll"
		}
	}
	vals, ok, err := c.facetFromTally(ctx, f, tallyName, where, args)
	if err != nil || !ok {
		return nil, false, false, err
	}
	all := make([]core.CatalogFacetCollection, 0, len(vals))
	for _, v := range vals {
		if v.Key == "" || v.Count <= 0 {
			continue
		}
		name := v.Key
		if storageFacet == "decade" {
			name = v.Key + "s"
		}
		if foldedQuery != "" && !strings.Contains(foldPersonQueryTerm(name), foldedQuery) && !strings.Contains(foldPersonQueryTerm(v.Key), foldedQuery) {
			continue
		}
		all = append(all, core.CatalogFacetCollection{
			ID:        v.Key,
			Name:      name,
			ItemCount: v.Count,
		})
	}
	sortCatalogFacetCollections(all, orderByName)
	if offset >= len(all) {
		return []core.CatalogFacetCollection{}, false, true, nil
	}
	end := min(offset+limit, len(all))
	return all[offset:end], end < len(all), true, nil
}

func sortCatalogFacetCollections(items []core.CatalogFacetCollection, orderByName bool) {
	if orderByName {
		slices.SortFunc(items, func(a, b core.CatalogFacetCollection) int {
			if c := cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
				return c
			}
			return cmp.Compare(a.Name, b.Name)
		})
		return
	}
	slices.SortFunc(items, func(a, b core.CatalogFacetCollection) int {
		if c := cmp.Compare(b.ItemCount, a.ItemCount); c != 0 {
			return c
		}
		if c := cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
}

func (c *Catalog) libraryFacetCollectionsSQL(
	ctx context.Context,
	storageFacet string,
	orderByName bool,
	rawQuery, foldedQuery string,
	offset, limit int,
	where string,
	args []any,
) ([]core.CatalogFacetCollection, bool, error) {
	isPG := store.DialectOf(c.db) == store.Postgres
	lowerQuery := strings.ToLower(rawQuery)
	likeArg := "%" + escapeLike(lowerQuery) + "%"
	foldedLikeArg := "%" + escapeLike(foldedQuery) + "%"

	var sqlStr string
	qArgs := make([]any, 0, len(args)+8)

	switch storageFacet {
	case "genre":
		qArgs = append(qArgs, args...)
		extra := ""
		if rawQuery != "" {
			extra = ` AND LOWER(g.genre) LIKE ? ESCAPE '\'`
			qArgs = append(qArgs, likeArg)
		}
		orderClause := `cnt DESC, LOWER(g.genre) ASC, g.genre ASC`
		if orderByName {
			orderClause = `LOWER(g.genre) ASC, g.genre ASC`
		}
		sqlStr = `SELECT g.genre, g.genre, COUNT(*) AS cnt
			FROM catalog_genres g
			JOIN catalog_titles t ON t.key = g.key
			WHERE (` + where + `) AND g.genre <> ''` + extra + `
			GROUP BY g.genre
			ORDER BY ` + orderClause + `
			LIMIT ? OFFSET ?`

	case "studio":
		if rawQuery != "" {
			namesSQL, nameArgs := c.studioNamesLike(likeArg, lowerQuery)
			qArgs = append(qArgs, nameArgs...)
			qArgs = append(qArgs, args...)
			orderClause := `cnt DESC, LOWER(st.studio) ASC, st.studio ASC`
			if orderByName {
				orderClause = `LOWER(st.studio) ASC, st.studio ASC`
			}
			sqlStr = `SELECT st.studio, st.studio, COUNT(*) AS cnt
				FROM catalog_studios st
				JOIN catalog_titles t ON t.key = st.key
				WHERE st.studio IN (` + namesSQL + `) AND (` + where + `) AND st.studio <> ''
				GROUP BY st.studio
				ORDER BY ` + orderClause + `
				LIMIT ? OFFSET ?`
		} else {
			qArgs = append(qArgs, args...)
			orderClause := `cnt DESC, LOWER(st.studio) ASC, st.studio ASC`
			if orderByName {
				orderClause = `LOWER(st.studio) ASC, st.studio ASC`
			}
			sqlStr = `SELECT st.studio, st.studio, COUNT(*) AS cnt
				FROM catalog_studios st
				JOIN catalog_titles t ON t.key = st.key
				WHERE (` + where + `) AND st.studio <> ''
				GROUP BY st.studio
				ORDER BY ` + orderClause + `
				LIMIT ? OFFSET ?`
		}

	case "country":
		qArgs = append(qArgs, args...)
		extra := ""
		if rawQuery != "" {
			extra = ` AND LOWER(co.country) LIKE ? ESCAPE '\'`
			qArgs = append(qArgs, likeArg)
		}
		orderClause := `cnt DESC, LOWER(co.country) ASC, co.country ASC`
		if orderByName {
			orderClause = `LOWER(co.country) ASC, co.country ASC`
		}
		sqlStr = `SELECT co.country, co.country, COUNT(*) AS cnt
			FROM catalog_countries co
			JOIN catalog_titles t ON t.key = co.key
			WHERE (` + where + `) AND co.country <> ''` + extra + `
			GROUP BY co.country
			ORDER BY ` + orderClause + `
			LIMIT ? OFFSET ?`

	case "year":
		qArgs = append(qArgs, args...)
		extra := ""
		if rawQuery != "" {
			extra = ` AND CAST(t.year AS TEXT) LIKE ? ESCAPE '\'`
			qArgs = append(qArgs, likeArg)
		}
		orderClause := `cnt DESC, t.year DESC`
		if orderByName {
			orderClause = `CAST(t.year AS TEXT) ASC`
		}
		sqlStr = `SELECT CAST(t.year AS TEXT), CAST(t.year AS TEXT), COUNT(*) AS cnt
			FROM catalog_titles t
			WHERE (` + where + `) AND t.year > 0` + extra + `
			GROUP BY t.year
			ORDER BY ` + orderClause + `
			LIMIT ? OFFSET ?`

	case "decade":
		qArgs = append(qArgs, args...)
		extra := ""
		if rawQuery != "" {
			extra = ` AND (CAST((t.year/10)*10 AS TEXT) || 's') LIKE ? ESCAPE '\'`
			qArgs = append(qArgs, likeArg)
		}
		orderClause := `cnt DESC, (t.year/10)*10 DESC`
		if orderByName {
			orderClause = `(CAST((t.year/10)*10 AS TEXT) || 's') ASC`
		}
		sqlStr = `SELECT CAST((t.year/10)*10 AS TEXT), CAST((t.year/10)*10 AS TEXT) || 's', COUNT(*) AS cnt
			FROM catalog_titles t
			WHERE (` + where + `) AND t.year > 0` + extra + `
			GROUP BY (t.year/10)*10
			ORDER BY ` + orderClause + `
			LIMIT ? OFFSET ?`

	case "franchise":
		qArgs = append(qArgs, args...)
		nameExpr := `COALESCE(NULLIF(MAX(c.name), ''), MAX(t.collection_name))`
		having := `HAVING ` + nameExpr + ` <> ''`
		if rawQuery != "" {
			if isPG {
				having += ` AND catalog_fold_person_name(` + nameExpr + `) LIKE ? ESCAPE '\'`
				qArgs = append(qArgs, foldedLikeArg)
			} else {
				matchedIDs := c.foldedMatchingCollectionIDs(ctx, foldedQuery)
				if len(matchedIDs) > 0 {
					ph := placeholders(len(matchedIDs))
					having += ` AND (LOWER(` + nameExpr + `) LIKE ? ESCAPE '\' OR t.collection_id IN (` + ph + `))`
					qArgs = append(qArgs, likeArg)
					for _, cid := range matchedIDs {
						qArgs = append(qArgs, cid)
					}
				} else {
					having += ` AND LOWER(` + nameExpr + `) LIKE ? ESCAPE '\'`
					qArgs = append(qArgs, likeArg)
				}
			}
		}
		orderClause := `cnt DESC, LOWER(` + nameExpr + `) ASC, t.collection_id ASC`
		if orderByName {
			orderClause = `LOWER(` + nameExpr + `) ASC, ` + nameExpr + ` ASC, t.collection_id ASC`
		}
		sqlStr = `SELECT CAST(t.collection_id AS TEXT), ` + nameExpr + ` AS cname, COUNT(*) AS cnt
			FROM catalog_titles t
			LEFT JOIN catalog_collections c ON c.tmdb_id = t.collection_id
			WHERE (` + where + `) AND t.collection_id > 0
			GROUP BY t.collection_id
			` + having + `
			ORDER BY ` + orderClause + `
			LIMIT ? OFFSET ?`
	default:
		return []core.CatalogFacetCollection{}, false, nil
	}

	qArgs = append(qArgs, limit+1, offset)
	rows, err := c.db.QueryContext(ctx, sqlStr, qArgs...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var out []core.CatalogFacetCollection
	for rows.Next() {
		var item core.CatalogFacetCollection
		if err := rows.Scan(&item.ID, &item.Name, &item.ItemCount); err != nil {
			return nil, false, err
		}
		item.Name = strings.TrimSpace(item.Name)
		if item.ID != "" && item.Name != "" && item.ItemCount > 0 {
			out = append(out, item)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := false
	if len(out) > limit {
		out = out[:limit]
		more = true
	}
	return out, more, nil
}

// foldedMatchingCollectionIDs scans catalog_collections for non-ASCII names
// whose diacritic-folded form matches foldedQuery on SQLite, where LOWER() is
// ASCII-only. Bounded to 64 IDs.
func (c *Catalog) foldedMatchingCollectionIDs(ctx context.Context, foldedQuery string) []int64 {
	if foldedQuery == "" {
		return nil
	}
	rows, err := c.db.QueryContext(ctx, `SELECT tmdb_id, name FROM catalog_collections`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			break
		}
		if strings.Contains(foldPersonQueryTerm(name), foldedQuery) {
			ids = append(ids, id)
			if len(ids) >= 64 {
				break
			}
		}
	}
	return ids
}

func (c *Catalog) hydrateLibraryFacetNewest(
	ctx context.Context,
	f core.Filter,
	storageFacet string,
	buckets []core.CatalogFacetCollection,
	where string,
	whereArgs []any,
) error {
	if len(buckets) == 0 {
		return nil
	}
	idxByID := make(map[string]int, len(buckets))
	for i, b := range buckets {
		idxByID[b.ID] = i
	}

	keyByBucket := make(map[string]core.RatingKey, len(buckets))
	var branches []string
	var qArgs []any
	if storageFacet == "franchise" {
		for _, bucket := range buckets {
			id, err := strconv.ParseInt(bucket.ID, 10, 64)
			if err != nil || id <= 0 {
				continue
			}
			branches = append(branches, `SELECT CAST(? AS TEXT) AS b, COALESCE((
				SELECT t.key FROM catalog_titles t
				WHERE t.collection_id = ? AND t.collection_id > 0 AND (`+where+`)
				ORDER BY t.released_ts DESC, t.popularity DESC, t.key ASC LIMIT 1
			), 0) AS k`)
			qArgs = append(qArgs, bucket.ID, id)
			qArgs = append(qArgs, whereArgs...)
		}
	} else {
		broadLibrary := f.CollectionMembers == 0 && len(f.CatalogueSources) == 0 && len(f.IncludeKeys) == 0
		for _, b := range buckets {
			switch storageFacet {
			case "year":
				yr, err := strconv.Atoi(b.ID)
				if err != nil || yr <= 0 {
					continue
				}
				upperTS := time.Date(yr+3, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
				branches = append(branches, `SELECT ? AS b, COALESCE((SELECT t.key FROM catalog_titles t WHERE (`+where+`) AND t.year = ? AND t.released_ts <= ? ORDER BY t.released_ts DESC, t.key ASC LIMIT 1), 0) AS k`)
				qArgs = append(qArgs, b.ID)
				qArgs = append(qArgs, whereArgs...)
				qArgs = append(qArgs, yr, upperTS)

			case "decade":
				dec, err := strconv.Atoi(b.ID)
				if err != nil || dec <= 0 {
					continue
				}
				upperTS := time.Date(dec+13, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
				branches = append(branches, `SELECT ? AS b, COALESCE((SELECT t.key FROM catalog_titles t WHERE (`+where+`) AND t.year >= ? AND t.year <= ? AND t.released_ts <= ? ORDER BY t.released_ts DESC, t.key ASC LIMIT 1), 0) AS k`)
				qArgs = append(qArgs, b.ID)
				qArgs = append(qArgs, whereArgs...)
				qArgs = append(qArgs, dec, dec+9, upperTS)

			case "genre":
				if broadLibrary && b.ItemCount <= 256 {
					branches = append(branches, `SELECT ? AS b, COALESCE((SELECT t.key FROM catalog_genres g JOIN catalog_titles t ON t.key = g.key WHERE g.genre = ? AND (`+where+`) ORDER BY t.released_ts DESC, t.key ASC LIMIT 1), 0) AS k`)
					qArgs = append(qArgs, b.ID, b.ID)
					qArgs = append(qArgs, whereArgs...)
				} else {
					branches = append(branches, `SELECT ? AS b, COALESCE((SELECT t.key FROM catalog_titles t WHERE (`+where+`) AND EXISTS (SELECT 1 FROM catalog_genres g WHERE g.key = t.key AND g.genre = ?) ORDER BY t.released_ts DESC, t.key ASC LIMIT 1), 0) AS k`)
					qArgs = append(qArgs, b.ID)
					qArgs = append(qArgs, whereArgs...)
					qArgs = append(qArgs, b.ID)
				}

			case "studio":
				if broadLibrary && b.ItemCount <= 256 {
					branches = append(branches, `SELECT ? AS b, COALESCE((SELECT t.key FROM catalog_studios st JOIN catalog_titles t ON t.key = st.key WHERE st.studio = ? AND (`+where+`) ORDER BY t.released_ts DESC, t.key ASC LIMIT 1), 0) AS k`)
					qArgs = append(qArgs, b.ID, b.ID)
					qArgs = append(qArgs, whereArgs...)
				} else {
					branches = append(branches, `SELECT ? AS b, COALESCE((SELECT t.key FROM catalog_titles t WHERE (`+where+`) AND EXISTS (SELECT 1 FROM catalog_studios st WHERE st.key = t.key AND st.studio = ?) ORDER BY t.released_ts DESC, t.key ASC LIMIT 1), 0) AS k`)
					qArgs = append(qArgs, b.ID)
					qArgs = append(qArgs, whereArgs...)
					qArgs = append(qArgs, b.ID)
				}

			case "country":
				if broadLibrary && b.ItemCount <= 256 {
					branches = append(branches, `SELECT ? AS b, COALESCE((SELECT t.key FROM catalog_countries co JOIN catalog_titles t ON t.key = co.key WHERE co.country = ? AND (`+where+`) ORDER BY t.released_ts DESC, t.key ASC LIMIT 1), 0) AS k`)
					qArgs = append(qArgs, b.ID, b.ID)
					qArgs = append(qArgs, whereArgs...)
				} else {
					branches = append(branches, `SELECT ? AS b, COALESCE((SELECT t.key FROM catalog_titles t WHERE (`+where+`) AND EXISTS (SELECT 1 FROM catalog_countries co WHERE co.key = t.key AND co.country = ?) ORDER BY t.released_ts DESC, t.key ASC LIMIT 1), 0) AS k`)
					qArgs = append(qArgs, b.ID)
					qArgs = append(qArgs, whereArgs...)
					qArgs = append(qArgs, b.ID)
				}
			default:
				c.appendHydrateExtraNewestBranch(storageFacet, b, broadLibrary, where, whereArgs, &branches, &qArgs)
			}
		}
	}
	if len(branches) > 0 {
		rows, err := c.db.QueryContext(ctx, strings.Join(branches, " UNION ALL "), qArgs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var bID string
			var k core.RatingKey
			if err := rows.Scan(&bID, &k); err != nil {
				rows.Close()
				return err
			}
			if k != 0 {
				keyByBucket[bID] = k
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}

	if len(keyByBucket) == 0 {
		return nil
	}
	uniqueKeys := make([]any, 0, len(keyByBucket))
	seenKey := make(map[core.RatingKey]bool, len(keyByBucket))
	for _, k := range keyByBucket {
		if !seenKey[k] {
			seenKey[k] = true
			uniqueKeys = append(uniqueKeys, int64(k))
		}
	}
	titlesByKey := make(map[core.RatingKey]core.Title, len(uniqueKeys))
	tRows, err := c.db.QueryContext(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE key IN (`+placeholders(len(uniqueKeys))+`)`, uniqueKeys...)
	if err != nil {
		return err
	}
	for tRows.Next() {
		st, sErr := scanTitle(tRows)
		if sErr != nil {
			tRows.Close()
			return sErr
		}
		titlesByKey[st.t.Key] = st.t
	}
	if err := tRows.Err(); err != nil {
		tRows.Close()
		return err
	}
	tRows.Close()

	for bID, k := range keyByBucket {
		if idx, ok := idxByID[bID]; ok {
			if t, found := titlesByKey[k]; found {
				cp := t
				buckets[idx].NewestItem = &cp
			}
		}
	}
	return nil
}

// LibraryFacetPeople returns a bounded, indexed page of credited cast or
// directors scoped to the library/viewer Filter, ordered by name (default) or
// item count, with folded search pushed into SQLite FTS5 or PostgreSQL
// folded indexes, and resolves one sample visible credit key per person for
// portrait signing in a single batched query.
func (c *Catalog) LibraryFacetPeople(ctx context.Context, q core.CatalogFacetPeopleQuery) (core.CatalogFacetPeoplePage, error) {
	facet, ok := core.NormalizeCatalogueTabFacet(core.TabPeople, q.Facet)
	if !ok {
		return core.CatalogFacetPeoplePage{Items: []core.CatalogFacetPerson{}}, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := max(q.Offset, 0)
	orderByCount := strings.EqualFold(strings.TrimSpace(q.Order), "count")

	rawQuery := strings.TrimSpace(q.Query)
	var tokens []string
	if rawQuery != "" {
		tokens = personQueryTokens([]string{rawQuery})
		if len(tokens) == 0 {
			return core.CatalogFacetPeoplePage{Items: []core.CatalogFacetPerson{}}, nil
		}
	}

	where, args, err := c.browseWhere(q.Filter)
	if err != nil {
		return core.CatalogFacetPeoplePage{}, err
	}

	var out []core.CatalogFacetPerson
	more := false

	if len(tokens) > 0 {
		out, more, err = c.libraryFacetPeopleSearch(ctx, facet, tokens, orderByCount, offset, limit, where, args)
		if err != nil {
			return core.CatalogFacetPeoplePage{}, err
		}
	} else {
		// Check whether the filtered library is small enough to aggregate
		// directly over matching titles, or large enough to walk the person
		// indexes without a multi-million-row credit aggregation.
		smallLibrary, sErr := c.libraryHasAtMostTitles(ctx, where, args, 5000)
		if sErr != nil {
			return core.CatalogFacetPeoplePage{}, sErr
		}
		if smallLibrary {
			out, more, err = c.libraryFacetPeopleDirect(ctx, facet, orderByCount, offset, limit, where, args)
		} else if !orderByCount {
			out, more, err = c.libraryFacetPeopleLargeByName(ctx, facet, offset, limit, where, args)
		} else {
			out, more, err = c.libraryFacetPeopleLargeByCount(ctx, facet, offset, limit, where, args)
		}
		if err != nil {
			return core.CatalogFacetPeoplePage{}, err
		}
	}

	if len(out) == 0 {
		return core.CatalogFacetPeoplePage{Items: []core.CatalogFacetPerson{}}, nil
	}

	if err := c.hydrateLibraryFacetPeopleSamples(ctx, facet, out, where, args); err != nil {
		return core.CatalogFacetPeoplePage{}, err
	}
	return core.CatalogFacetPeoplePage{Items: out, More: more}, nil
}

func (c *Catalog) libraryHasAtMostTitles(ctx context.Context, where string, args []any, threshold int) (bool, error) {
	qArgs := append(append([]any(nil), args...), threshold+1)
	var cnt int
	err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM catalog_titles t WHERE `+where+` LIMIT ?) s`, qArgs...).Scan(&cnt)
	if err != nil {
		return false, err
	}
	return cnt <= threshold, nil
}

func (c *Catalog) libraryFacetPeopleSearch(
	ctx context.Context,
	kind string,
	tokens []string,
	orderByCount bool,
	offset, limit int,
	where string,
	whereArgs []any,
) ([]core.CatalogFacetPerson, bool, error) {
	var nameCond string
	var nameArgs []any
	if store.DialectOf(c.db) == store.Postgres {
		var conds []string
		for _, tok := range tokens {
			if utf8.RuneCountInString(tok) < 3 {
				conds = append(conds, `(catalog_fold_person_name(p.name) COLLATE "C") LIKE ? ESCAPE '\'`)
				nameArgs = append(nameArgs, escapeLike(tok)+"%")
			} else {
				conds = append(conds, `catalog_fold_person_name(p.name) LIKE ? ESCAPE '\'`)
				nameArgs = append(nameArgs, "%"+escapeLike(tok)+"%")
			}
		}
		nameCond = `p.credits > 0 AND ` + strings.Join(conds, " AND ")
	} else {
		parts := make([]string, len(tokens))
		for i, tok := range tokens {
			parts[i] = `"` + tok + `"*`
		}
		nameCond = `p.credits > 0 AND p.person_tmdb IN (SELECT rowid FROM catalog_person_names_fts WHERE catalog_person_names_fts MATCH ?)`
		nameArgs = append(nameArgs, strings.Join(parts, " "))
	}

	orderClause := `LOWER(p.name) ASC, p.name ASC, p.person_tmdb ASC`
	if orderByCount {
		orderClause = `cnt DESC, LOWER(p.name) ASC, p.person_tmdb ASC`
	}

	qArgs := make([]any, 0, 1+len(nameArgs)+len(whereArgs)+2)
	qArgs = append(qArgs, kind)
	qArgs = append(qArgs, nameArgs...)
	qArgs = append(qArgs, whereArgs...)
	qArgs = append(qArgs, limit+1, offset)

	sqlStr := `SELECT p.person_tmdb, p.name, COUNT(DISTINCT cr.key) AS cnt
		FROM catalog_person_names p
		JOIN catalog_credits cr ON cr.person_tmdb = p.person_tmdb AND cr.kind = ?
		JOIN catalog_titles t ON t.key = cr.key
		WHERE ` + nameCond + ` AND p.name <> '' AND (` + where + `)
		GROUP BY p.person_tmdb, p.name
		ORDER BY ` + orderClause + `
		LIMIT ? OFFSET ?`

	return c.scanFacetPeoplePage(ctx, sqlStr, qArgs, limit)
}

func (c *Catalog) libraryFacetPeopleDirect(
	ctx context.Context,
	kind string,
	orderByCount bool,
	offset, limit int,
	where string,
	whereArgs []any,
) ([]core.CatalogFacetPerson, bool, error) {
	orderClause := `LOWER(p.name) ASC, p.name ASC, p.person_tmdb ASC`
	if orderByCount {
		orderClause = `cnt DESC, LOWER(p.name) ASC, p.person_tmdb ASC`
	}
	qArgs := make([]any, 0, 1+len(whereArgs)+2)
	qArgs = append(qArgs, kind)
	qArgs = append(qArgs, whereArgs...)
	qArgs = append(qArgs, limit+1, offset)

	sqlStr := `SELECT p.person_tmdb, p.name, COUNT(DISTINCT cr.key) AS cnt
		FROM catalog_titles t
		JOIN catalog_credits cr ON cr.key = t.key AND cr.kind = ? AND cr.person_tmdb > 0
		JOIN catalog_person_names p ON p.person_tmdb = cr.person_tmdb
		WHERE (` + where + `) AND p.name <> ''
		GROUP BY p.person_tmdb, p.name
		ORDER BY ` + orderClause + `
		LIMIT ? OFFSET ?`

	return c.scanFacetPeoplePage(ctx, sqlStr, qArgs, limit)
}

func (c *Catalog) libraryFacetPeopleLargeByName(
	ctx context.Context,
	kind string,
	offset, limit int,
	where string,
	whereArgs []any,
) ([]core.CatalogFacetPerson, bool, error) {
	orderExpr := `LOWER(p.name) ASC, p.person_tmdb ASC`
	if store.DialectOf(c.db) == store.Postgres {
		orderExpr = `(translate(p.name, 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz') COLLATE "C") ASC, p.person_tmdb ASC`
	}
	qArgs := make([]any, 0, 1+len(whereArgs)+2)
	qArgs = append(qArgs, kind)
	qArgs = append(qArgs, whereArgs...)
	qArgs = append(qArgs, limit+1, offset)

	sqlStr := `SELECT p.person_tmdb, p.name
		FROM catalog_person_names p
		WHERE p.name <> '' AND p.credits > 0 AND EXISTS (
			SELECT 1 FROM catalog_credits cr
			JOIN catalog_titles t ON t.key = cr.key
			WHERE cr.person_tmdb = p.person_tmdb AND cr.kind = ? AND (` + where + `)
		)
		ORDER BY ` + orderExpr + `
		LIMIT ? OFFSET ?`

	rows, err := c.db.QueryContext(ctx, sqlStr, qArgs...)
	if err != nil {
		return nil, false, err
	}
	var out []core.CatalogFacetPerson
	for rows.Next() {
		var fp core.CatalogFacetPerson
		if err := rows.Scan(&fp.PersonTMDB, &fp.Name); err != nil {
			rows.Close()
			return nil, false, err
		}
		fp.Name = strings.TrimSpace(fp.Name)
		if fp.PersonTMDB > 0 && fp.Name != "" {
			out = append(out, fp)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, false, err
	}
	rows.Close()

	more := false
	if len(out) > limit {
		out = out[:limit]
		more = true
	}
	return out, more, nil
}

func (c *Catalog) libraryFacetPeopleLargeByCount(
	ctx context.Context,
	kind string,
	offset, limit int,
	where string,
	whereArgs []any,
) ([]core.CatalogFacetPerson, bool, error) {
	window := min(max((offset+limit)*6, 300), 1500)
	if offset >= window {
		return []core.CatalogFacetPerson{}, false, nil
	}
	qArgs := make([]any, 0, 1+len(whereArgs)+1)
	qArgs = append(qArgs, kind)
	qArgs = append(qArgs, whereArgs...)
	qArgs = append(qArgs, window)

	sqlStr := `SELECT p.person_tmdb, p.name
		FROM catalog_person_names p
		WHERE p.name <> '' AND p.credits > 0 AND EXISTS (
			SELECT 1 FROM catalog_credits cr
			JOIN catalog_titles t ON t.key = cr.key
			WHERE cr.person_tmdb = p.person_tmdb AND cr.kind = ? AND (` + where + `)
		)
		ORDER BY p.credits DESC, p.person_tmdb ASC
		LIMIT ?`

	rows, err := c.db.QueryContext(ctx, sqlStr, qArgs...)
	if err != nil {
		return nil, false, err
	}
	var cands []core.CatalogFacetPerson
	for rows.Next() {
		var fp core.CatalogFacetPerson
		if err := rows.Scan(&fp.PersonTMDB, &fp.Name); err != nil {
			rows.Close()
			return nil, false, err
		}
		fp.Name = strings.TrimSpace(fp.Name)
		if fp.PersonTMDB > 0 && fp.Name != "" {
			cands = append(cands, fp)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, false, err
	}
	rows.Close()
	if len(cands) == 0 {
		return []core.CatalogFacetPerson{}, false, nil
	}

	if err := c.hydrateLibraryFacetPeopleSamples(ctx, kind, cands, where, whereArgs); err != nil {
		return nil, false, err
	}
	filtered := cands[:0]
	for _, fp := range cands {
		if fp.ItemCount > 0 {
			filtered = append(filtered, fp)
		}
	}
	slices.SortFunc(filtered, func(a, b core.CatalogFacetPerson) int {
		if d := cmp.Compare(b.ItemCount, a.ItemCount); d != 0 {
			return d
		}
		if d := cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); d != 0 {
			return d
		}
		return cmp.Compare(a.PersonTMDB, b.PersonTMDB)
	})
	if offset >= len(filtered) {
		return []core.CatalogFacetPerson{}, false, nil
	}
	end := min(offset+limit, len(filtered))
	return filtered[offset:end], end < len(filtered), nil
}

func (c *Catalog) scanFacetPeoplePage(ctx context.Context, sqlStr string, qArgs []any, limit int) ([]core.CatalogFacetPerson, bool, error) {
	rows, err := c.db.QueryContext(ctx, sqlStr, qArgs...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []core.CatalogFacetPerson
	for rows.Next() {
		var fp core.CatalogFacetPerson
		if err := rows.Scan(&fp.PersonTMDB, &fp.Name, &fp.ItemCount); err != nil {
			return nil, false, err
		}
		fp.Name = strings.TrimSpace(fp.Name)
		if fp.PersonTMDB > 0 && fp.Name != "" && fp.ItemCount > 0 {
			out = append(out, fp)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := false
	if len(out) > limit {
		out = out[:limit]
		more = true
	}
	return out, more, nil
}

// hydrateLibraryFacetPeopleSamples fills SampleKey (preferring a portrait-bearing
// popular credit) and, when ItemCount was 0 (large-library index walk), exact
// ItemCount for the page's people in one batched query driven from
// catalog_credits_person_kind.
func (c *Catalog) hydrateLibraryFacetPeopleSamples(
	ctx context.Context,
	kind string,
	items []core.CatalogFacetPerson,
	where string,
	whereArgs []any,
) error {
	if len(items) == 0 {
		return nil
	}
	// Check if all items already have SampleKey and ItemCount populated.
	allHydrated := true
	for _, it := range items {
		if it.SampleKey == 0 || it.ItemCount == 0 {
			allHydrated = false
			break
		}
	}
	if allHydrated {
		return nil
	}

	const chunkSize = 200
	for start := 0; start < len(items); start += chunkSize {
		chunk := items[start:min(start+chunkSize, len(items))]
		idxByPID := make(map[int64]int, len(chunk))
		pids := make([]any, 0, len(chunk))
		for i, it := range chunk {
			if _, seen := idxByPID[it.PersonTMDB]; !seen {
				idxByPID[it.PersonTMDB] = i
				pids = append(pids, it.PersonTMDB)
			}
		}
		qArgs := make([]any, 0, len(pids)+1+len(whereArgs))
		qArgs = append(qArgs, pids...)
		qArgs = append(qArgs, kind)
		qArgs = append(qArgs, whereArgs...)

		sqlStr := `WITH person_titles AS (
			SELECT cr.person_tmdb,
				cr.key,
				MAX(CASE WHEN cr.thumb <> '' THEN 1 ELSE 0 END) AS has_thumb,
				MAX(t.popularity) AS pop
			FROM catalog_credits cr
			JOIN catalog_titles t ON t.key = cr.key
			WHERE cr.person_tmdb IN (` + placeholders(len(pids)) + `) AND cr.kind = ? AND (` + where + `)
			GROUP BY cr.person_tmdb, cr.key
		),
		ranked AS (
			SELECT person_tmdb,
				key,
				COUNT(*) OVER (PARTITION BY person_tmdb) AS cnt,
				ROW_NUMBER() OVER (PARTITION BY person_tmdb ORDER BY has_thumb DESC, pop DESC, key DESC) AS rn
			FROM person_titles
		)
		SELECT person_tmdb, cnt, key FROM ranked WHERE rn = 1`

		rows, err := c.db.QueryContext(ctx, sqlStr, qArgs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var pid int64
			var cnt int
			var key core.RatingKey
			if err := rows.Scan(&pid, &cnt, &key); err != nil {
				rows.Close()
				return err
			}
			if idx, ok := idxByPID[pid]; ok {
				if chunk[idx].ItemCount == 0 {
					chunk[idx].ItemCount = cnt
				}
				chunk[idx].SampleKey = key
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return nil
}
