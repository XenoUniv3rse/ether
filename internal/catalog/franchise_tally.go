package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// catalog_franchises keeps one row per TMDB collection (franchise) present in
// the browsable movie catalogue, with its browsable movie count and name,
// maintained in step by triggers on catalog_titles. catalog_franchise_counts
// and its append-only delta table keep the total number of franchises with at
// least two browsable movies (the threshold Ether lists as a franchise) so a
// paged collection listing reads O(page_size) index entries and an O(1) total
// instead of scanning and grouping every movie in the library.

func franchiseMember(alias string) string {
	return alias + ".type='movie' AND " + alias + ".browsable=1 AND " + alias + ".adult=0 AND " + alias + ".collection_id>0"
}

const franchiseBackfill = `INSERT INTO catalog_franchises(collection_id,name,member_count)
 SELECT t.collection_id, COALESCE(NULLIF(MAX(t.collection_name),''), MAX(cc.name), ''), COUNT(*)
 FROM catalog_titles t
 LEFT JOIN catalog_collections cc ON cc.tmdb_id = t.collection_id
 WHERE ` + `t.type='movie' AND t.browsable=1 AND t.adult=0 AND t.collection_id>0` + `
 GROUP BY t.collection_id`

const franchiseCountsBackfill = `INSERT INTO catalog_franchise_counts(id,n)
 VALUES (1, (SELECT COUNT(*) FROM catalog_franchises WHERE member_count>=2))`

func init() {
	columns := `type, browsable, adult, collection_id, collection_name`
	changed := `(` + franchiseMember("old") + `) IS NOT (` + franchiseMember("new") + `) OR old.collection_id IS NOT new.collection_id OR old.collection_name IS NOT new.collection_name`
	nameVal := func(alias string) string {
		return `COALESCE(NULLIF(` + alias + `.collection_name,''), (SELECT name FROM catalog_collections WHERE tmdb_id=` + alias + `.collection_id), '')`
	}
	upsertSQLite := `INSERT INTO catalog_franchises(collection_id,name,member_count)
 VALUES (new.collection_id,` + nameVal("new") + `,1)
 ON CONFLICT(collection_id) DO UPDATE SET
  member_count=catalog_franchises.member_count+1,
  name=CASE WHEN excluded.name<>'' THEN excluded.name ELSE catalog_franchises.name END;`
	upsertSelectSQLite := `INSERT INTO catalog_franchises(collection_id,name,member_count)
 SELECT new.collection_id,` + nameVal("new") + `,1 WHERE ` + franchiseMember("new") + `
 ON CONFLICT(collection_id) DO UPDATE SET
  member_count=catalog_franchises.member_count+1,
  name=CASE WHEN excluded.name<>'' THEN excluded.name ELSE catalog_franchises.name END;`
	decrementSQLite := `UPDATE catalog_franchises SET member_count=member_count-1 WHERE collection_id=old.collection_id;
 DELETE FROM catalog_franchises WHERE collection_id=old.collection_id AND member_count<=0;`

	store.Register("catalog", 66, `CREATE TABLE catalog_franchises (
 collection_id INTEGER PRIMARY KEY,
 name TEXT NOT NULL DEFAULT '',
 member_count INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX catalog_franchises_count ON catalog_franchises(member_count DESC, collection_id ASC);
CREATE INDEX catalog_franchises_name ON catalog_franchises(LOWER(name), collection_id ASC);
CREATE TABLE catalog_franchise_counts (id INTEGER PRIMARY KEY, n INTEGER NOT NULL);
CREATE TABLE catalog_franchise_counts_delta (id INTEGER NOT NULL, d INTEGER NOT NULL);
`+franchiseBackfill+`;
`+franchiseCountsBackfill+`;
CREATE VIRTUAL TABLE catalog_franchises_fts USING fts5(name, content='catalog_franchises', content_rowid='collection_id', tokenize='trigram');
INSERT INTO catalog_franchises_fts(catalog_franchises_fts) VALUES('rebuild');
CREATE TRIGGER catalog_franchises_fts_insert AFTER INSERT ON catalog_franchises BEGIN
 INSERT INTO catalog_franchises_fts(rowid, name) VALUES (new.collection_id, new.name);
END;
CREATE TRIGGER catalog_franchises_fts_delete AFTER DELETE ON catalog_franchises BEGIN
 INSERT INTO catalog_franchises_fts(catalog_franchises_fts, rowid, name) VALUES ('delete', old.collection_id, old.name);
END;
CREATE TRIGGER catalog_franchises_fts_update AFTER UPDATE OF name ON catalog_franchises WHEN old.name IS NOT new.name BEGIN
 INSERT INTO catalog_franchises_fts(catalog_franchises_fts, rowid, name) VALUES ('delete', old.collection_id, old.name);
 INSERT INTO catalog_franchises_fts(rowid, name) VALUES (new.collection_id, new.name);
END;
CREATE TRIGGER catalog_franchise_counts_insert AFTER INSERT ON catalog_franchises
WHEN new.member_count>=2 BEGIN
 INSERT INTO catalog_franchise_counts_delta(id,d) VALUES (1,1);
END;
CREATE TRIGGER catalog_franchise_counts_delete AFTER DELETE ON catalog_franchises
WHEN old.member_count>=2 BEGIN
 INSERT INTO catalog_franchise_counts_delta(id,d) VALUES (1,-1);
END;
CREATE TRIGGER catalog_franchise_counts_update AFTER UPDATE OF member_count ON catalog_franchises
WHEN (old.member_count>=2) IS NOT (new.member_count>=2) BEGIN
 INSERT INTO catalog_franchise_counts_delta(id,d) VALUES (1, CASE WHEN new.member_count>=2 THEN 1 ELSE -1 END);
END;
CREATE TRIGGER catalog_franchises_insert AFTER INSERT ON catalog_titles
WHEN `+franchiseMember("new")+` BEGIN `+upsertSQLite+` END;
CREATE TRIGGER catalog_franchises_delete AFTER DELETE ON catalog_titles
WHEN `+franchiseMember("old")+` BEGIN `+decrementSQLite+` END;
CREATE TRIGGER catalog_franchises_update AFTER UPDATE OF `+columns+` ON catalog_titles
WHEN `+changed+` BEGIN
 UPDATE catalog_franchises SET member_count=member_count-1 WHERE collection_id=old.collection_id AND `+franchiseMember("old")+`;
 DELETE FROM catalog_franchises WHERE collection_id=old.collection_id AND member_count<=0 AND `+franchiseMember("old")+`;
 `+upsertSelectSQLite+`
END;
CREATE TRIGGER catalog_franchises_collection_name_insert AFTER INSERT ON catalog_collections
WHEN new.name<>'' BEGIN
 UPDATE catalog_franchises SET name=new.name WHERE collection_id=new.tmdb_id AND name='';
END;
CREATE TRIGGER catalog_franchises_collection_name_update AFTER UPDATE OF name ON catalog_collections
WHEN new.name<>'' AND old.name IS NOT new.name BEGIN
 UPDATE catalog_franchises SET name=new.name WHERE collection_id=new.tmdb_id AND name='';
END;`)

	pgUpsert := `INSERT INTO catalog_franchises(collection_id,name,member_count)
 VALUES (NEW.collection_id,` + nameVal("NEW") + `,1)
 ON CONFLICT(collection_id) DO UPDATE SET
  member_count=catalog_franchises.member_count+1,
  name=CASE WHEN EXCLUDED.name<>'' THEN EXCLUDED.name ELSE catalog_franchises.name END;`
	pgDecrement := `UPDATE catalog_franchises SET member_count=member_count-1 WHERE collection_id=OLD.collection_id;
 DELETE FROM catalog_franchises WHERE collection_id=OLD.collection_id AND member_count<=0;`

	store.RegisterDialect("catalog", 66, store.Postgres, `LOCK TABLE catalog_titles IN SHARE ROW EXCLUSIVE MODE;
CREATE TABLE catalog_franchises (
 collection_id BIGINT PRIMARY KEY,
 name TEXT NOT NULL DEFAULT '',
 member_count BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX catalog_franchises_count ON catalog_franchises(member_count DESC, collection_id ASC);
CREATE INDEX catalog_franchises_name ON catalog_franchises(LOWER(name), collection_id ASC);
CREATE TABLE catalog_franchise_counts (id BIGINT PRIMARY KEY, n BIGINT NOT NULL);
CREATE TABLE catalog_franchise_counts_delta (id BIGINT NOT NULL, d BIGINT NOT NULL);
`+franchiseBackfill+`;
`+franchiseCountsBackfill+`;
CREATE FUNCTION catalog_franchise_counts_fn() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='INSERT' AND NEW.member_count>=2 THEN
  INSERT INTO catalog_franchise_counts_delta(id,d) VALUES (1,1);
 ELSIF TG_OP='DELETE' AND OLD.member_count>=2 THEN
  INSERT INTO catalog_franchise_counts_delta(id,d) VALUES (1,-1);
 ELSIF TG_OP='UPDATE' AND ((OLD.member_count>=2) IS DISTINCT FROM (NEW.member_count>=2)) THEN
  INSERT INTO catalog_franchise_counts_delta(id,d) VALUES (1, CASE WHEN NEW.member_count>=2 THEN 1 ELSE -1 END);
 END IF;
 RETURN NULL;
END $$;
CREATE TRIGGER catalog_franchise_counts_trg AFTER INSERT OR DELETE OR UPDATE OF member_count ON catalog_franchises
 FOR EACH ROW EXECUTE FUNCTION catalog_franchise_counts_fn();
CREATE FUNCTION catalog_franchises_fn() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP IN ('UPDATE','DELETE') AND `+franchiseMember("OLD")+` THEN `+pgDecrement+` END IF;
 IF TG_OP IN ('INSERT','UPDATE') AND `+franchiseMember("NEW")+` THEN `+pgUpsert+` END IF;
 RETURN NULL;
END $$;
CREATE TRIGGER catalog_franchises_trg AFTER INSERT OR DELETE ON catalog_titles
 FOR EACH ROW EXECUTE FUNCTION catalog_franchises_fn();
CREATE TRIGGER catalog_franchises_update AFTER UPDATE OF `+columns+` ON catalog_titles
 FOR EACH ROW WHEN (`+pgDistinct(changed)+`) EXECUTE FUNCTION catalog_franchises_fn();
CREATE FUNCTION catalog_franchises_collection_name_fn() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.name<>'' THEN
  UPDATE catalog_franchises SET name=NEW.name WHERE collection_id=NEW.tmdb_id AND name='';
 END IF;
 RETURN NULL;
END $$;
CREATE TRIGGER catalog_franchises_collection_name_trg AFTER INSERT OR UPDATE OF name ON catalog_collections
 FOR EACH ROW EXECUTE FUNCTION catalog_franchises_collection_name_fn();
DO $$
BEGIN
 CREATE EXTENSION IF NOT EXISTS pg_trgm;
 CREATE INDEX IF NOT EXISTS catalog_franchises_trgm ON catalog_franchises USING gin (LOWER(name) gin_trgm_ops);
EXCEPTION WHEN OTHERS THEN
 RAISE NOTICE 'catalog: pg_trgm unavailable, franchise title filters scan catalog_franchises: %', SQLERRM;
END
$$;`)

	store.RegisterRestore("catalog_franchises", func(context.Context, *sql.Tx) (func(context.Context, *sql.Tx) error, error) {
		return func(ctx context.Context, tx *sql.Tx) error { return rebuildFranchiseTally(ctx, tx, false) }, nil
	}, "catalog_franchises", "catalog_franchise_counts", "catalog_franchise_counts_delta")
}

func rebuildFranchiseTally(ctx context.Context, db execer, rebuildFTS bool) error {
	for _, q := range []string{
		`DELETE FROM catalog_franchises`,
		`DELETE FROM catalog_franchise_counts_delta`,
		`DELETE FROM catalog_franchise_counts`,
		franchiseBackfill,
		`DELETE FROM catalog_franchise_counts_delta`,
		`DELETE FROM catalog_franchise_counts`,
		franchiseCountsBackfill,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if rebuildFTS {
		if _, err := db.ExecContext(ctx, `INSERT INTO catalog_franchises_fts(catalog_franchises_fts) VALUES('rebuild')`); err != nil {
			return err
		}
	}
	return nil
}

// FranchiseTotal returns the number of franchises with at least two browsable
// movies in O(1) from catalog_franchise_counts and its delta table.
func (c *Catalog) FranchiseTotal(ctx context.Context) (int, error) {
	return franchiseTotalDB(ctx, c.db)
}

func franchiseTotalDB(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT CAST(COALESCE(SUM(n),0) AS BIGINT) FROM (
		SELECT n FROM catalog_franchise_counts WHERE id=1
		UNION ALL
		SELECT d FROM catalog_franchise_counts_delta WHERE id=1
	) t`).Scan(&n)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		n = 0
	}
	return n, nil
}

func scanFranchiseRows(rows *sql.Rows) ([]core.Collection, error) {
	var out []core.Collection
	for rows.Next() {
		var id int64
		var name, poster, backdrop, summary string
		var n int
		if err := rows.Scan(&id, &name, &n, &poster, &backdrop, &summary); err != nil {
			return nil, err
		}
		if name == "" {
			name = fmt.Sprintf("Collection %d", id)
		}
		out = append(out, core.Collection{
			Key:          core.CollectionKey(id),
			Title:        name,
			Summary:      summary,
			Kind:         "franchise",
			Type:         core.TypeMovie,
			PosterPath:   poster,
			BackdropPath: backdrop,
			ChildCount:   n,
		})
	}
	return out, rows.Err()
}

// FranchisesPage returns one page of franchises with at least two browsable
// movies, ordered by member_count DESC, collection_id ASC.
func (c *Catalog) FranchisesPage(ctx context.Context, offset, limit int) ([]core.Collection, error) {
	return franchisesPageDB(ctx, c.db, offset, limit)
}

func franchisesPageDB(ctx context.Context, db *sql.DB, offset, limit int) ([]core.Collection, error) {
	if limit <= 0 {
		return nil, nil
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := db.QueryContext(ctx, `SELECT f.collection_id,
		COALESCE(NULLIF(f.name,''), cc.name, ''),
		f.member_count,
		COALESCE(cc.poster,''), COALESCE(cc.backdrop,''), COALESCE(cc.summary,'')
		FROM catalog_franchises f
		LEFT JOIN catalog_collections cc ON cc.tmdb_id = f.collection_id
		WHERE f.member_count>=2
		ORDER BY f.member_count DESC, f.collection_id ASC
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFranchiseRows(rows)
}

// FranchisesCursor returns the next keyset page of franchises with at least
// two browsable movies in (member_count DESC, collection_id ASC) order.
// Passing afterID == 0 starts from the beginning.
func (c *Catalog) FranchisesCursor(ctx context.Context, afterCount, afterID, limit int) ([]core.Collection, error) {
	return franchisesCursorDB(ctx, c.db, 2, afterCount, afterID, limit)
}

func franchisesCursorDB(ctx context.Context, db *sql.DB, minCount, afterCount, afterID, limit int) ([]core.Collection, error) {
	if limit <= 0 {
		return nil, nil
	}
	if afterID <= 0 {
		rows, err := db.QueryContext(ctx, `SELECT f.collection_id,
			COALESCE(NULLIF(f.name,''), cc.name, ''),
			f.member_count,
			COALESCE(cc.poster,''), COALESCE(cc.backdrop,''), COALESCE(cc.summary,'')
			FROM catalog_franchises f
			LEFT JOIN catalog_collections cc ON cc.tmdb_id = f.collection_id
			WHERE f.member_count>=?
			ORDER BY f.member_count DESC, f.collection_id ASC
			LIMIT ?`, minCount, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanFranchiseRows(rows)
	}
	// Two index seeks keep keyset paging O(limit) at any depth on both SQLite
	// and PostgreSQL even when many franchises share the same member_count.
	rows, err := db.QueryContext(ctx, `SELECT f.collection_id,
		COALESCE(NULLIF(f.name,''), cc.name, ''),
		f.member_count,
		COALESCE(cc.poster,''), COALESCE(cc.backdrop,''), COALESCE(cc.summary,'')
		FROM catalog_franchises f
		LEFT JOIN catalog_collections cc ON cc.tmdb_id = f.collection_id
		WHERE f.member_count=? AND f.collection_id>?
		ORDER BY f.member_count DESC, f.collection_id ASC
		LIMIT ?`, afterCount, afterID, limit)
	if err != nil {
		return nil, err
	}
	out, err := scanFranchiseRows(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(out) >= limit || afterCount <= minCount {
		return out, nil
	}
	rows, err = db.QueryContext(ctx, `SELECT f.collection_id,
		COALESCE(NULLIF(f.name,''), cc.name, ''),
		f.member_count,
		COALESCE(cc.poster,''), COALESCE(cc.backdrop,''), COALESCE(cc.summary,'')
		FROM catalog_franchises f
		LEFT JOIN catalog_collections cc ON cc.tmdb_id = f.collection_id
		WHERE f.member_count>=? AND f.member_count<?
		ORDER BY f.member_count DESC, f.collection_id ASC
		LIMIT ?`, minCount, afterCount, limit-len(out))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	more, err := scanFranchiseRows(rows)
	if err != nil {
		return nil, err
	}
	return append(out, more...), nil
}

// FranchisesSearch returns the next keyset page of franchises whose name
// contains query (case-insensitively), ordered by member_count DESC,
// collection_id ASC. On SQLite substrings of three or more characters use the
// trigram FTS index; on PostgreSQL pg_trgm accelerates LOWER(name) LIKE.
func (c *Catalog) FranchisesSearch(ctx context.Context, query string, afterCount, afterID, limit int) ([]core.Collection, error) {
	if limit <= 0 {
		return nil, nil
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return c.FranchisesCursor(ctx, afterCount, afterID, limit)
	}
	like := "%" + escapeLike(q) + "%"
	var conds []string
	var args []any
	conds = append(conds, "f.member_count>=2")
	if store.DialectOf(c.db) == store.SQLite && utf8.RuneCountInString(q) >= 3 && !strings.ContainsAny(q, `%_\`) {
		conds = append(conds, "f.collection_id IN (SELECT rowid FROM catalog_franchises_fts WHERE name LIKE ?)")
		args = append(args, "%"+q+"%")
	}
	conds = append(conds, "LOWER(COALESCE(NULLIF(f.name,''), cc.name, '')) LIKE ? ESCAPE '\\'")
	args = append(args, like)
	if afterID > 0 {
		conds = append(conds, "f.member_count<=? AND (f.member_count<? OR f.collection_id>?)")
		args = append(args, afterCount, afterCount, afterID)
	}
	args = append(args, limit)
	rows, err := c.db.QueryContext(ctx, `SELECT f.collection_id,
		COALESCE(NULLIF(f.name,''), cc.name, ''),
		f.member_count,
		COALESCE(cc.poster,''), COALESCE(cc.backdrop,''), COALESCE(cc.summary,'')
		FROM catalog_franchises f
		LEFT JOIN catalog_collections cc ON cc.tmdb_id = f.collection_id
		WHERE `+strings.Join(conds, " AND ")+`
		ORDER BY f.member_count DESC, f.collection_id ASC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFranchiseRows(rows)
}

// HasActiveViewerRules reports whether viewerID has an active catalogue
// visibility rule set that can hide movies.
func (c *Catalog) HasActiveViewerRules(viewerID int64) bool {
	if c == nil || viewerID == 0 {
		return false
	}
	policy := c.viewerFilter(viewerID, false)
	return policy != nil && !policy.Passive()
}

// franchiseMemberWhere builds the SQL predicate for a browsable, released,
// non-adult movie that is visible under f (matching Catalog.Browse's viewer
// rules, digital release mode, safety, rating caps, availability gates and
// library scope, excluding unreleased upcoming titles).
func (c *Catalog) franchiseMemberWhere(f core.Filter) (string, []any, error) {
	f.Type = core.TypeMovie
	f.LocalOnly = true
	where, args, err := c.browseWhere(f)
	if err != nil {
		return "", nil, err
	}
	nowTS := c.now().Unix()
	extra := []string{"t.browsable=1", "t.released_ts<=?"}
	args = append(args, nowTS)
	if f.MaxContentRating != "" && contentrating.EnforceLevelOf(f.MaxContentRating) != contentrating.Unrated {
		extra = append(extra, "t.enforcement_rating<>'XXX'")
	}
	return where + " AND " + strings.Join(extra, " AND "), args, nil
}

// FranchiseVisibleCounts returns, per collection_id in collectionIDs, how many
// movies in that franchise satisfy f. Every candidate batch runs as one
// set-based query against catalog_titles_collection without hydrating credits.
func (c *Catalog) FranchiseVisibleCounts(ctx context.Context, f core.Filter, collectionIDs []int) (map[int]int, error) {
	if len(collectionIDs) == 0 {
		return nil, nil
	}
	where, baseArgs, err := c.franchiseMemberWhere(f)
	if err != nil {
		return nil, err
	}
	out := make(map[int]int, len(collectionIDs))
	for part := range store.Chunks(collectionIDs, 400) {
		args := make([]any, 0, len(part)+len(baseArgs))
		for _, id := range part {
			args = append(args, id)
		}
		args = append(args, baseArgs...)
		rows, err := c.db.QueryContext(ctx, `SELECT t.collection_id, COUNT(*)
			FROM catalog_titles AS t
			WHERE t.collection_id IN (`+placeholders(len(part))+`) AND `+where+`
			GROUP BY t.collection_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, n int
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return nil, err
			}
			if n > 0 {
				out[id] = n
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// FranchiseVisibleKeys returns the visible movie keys per collection_id for
// collectionIDs under f, used to union membership across multiple filter
// libraries without double-counting overlapping titles.
func (c *Catalog) FranchiseVisibleKeys(ctx context.Context, f core.Filter, collectionIDs []int) (map[int][]core.RatingKey, error) {
	if len(collectionIDs) == 0 {
		return nil, nil
	}
	where, baseArgs, err := c.franchiseMemberWhere(f)
	if err != nil {
		return nil, err
	}
	out := make(map[int][]core.RatingKey, len(collectionIDs))
	for part := range store.Chunks(collectionIDs, 400) {
		args := make([]any, 0, len(part)+len(baseArgs))
		for _, id := range part {
			args = append(args, id)
		}
		args = append(args, baseArgs...)
		rows, err := c.db.QueryContext(ctx, `SELECT t.collection_id, t.key
			FROM catalog_titles AS t
			WHERE t.collection_id IN (`+placeholders(len(part))+`) AND `+where, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int
			var key core.RatingKey
			if err := rows.Scan(&id, &key); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = append(out[id], key)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// smallFilterFranchiseCap bounds the direct grouped franchise query for
// narrow filtered libraries so a sparse library with few or zero matching
// franchises never walks catalog_franchises looking for matches.
const smallFilterFranchiseCap = 5000

// FranchisesInSmallFilter answers a filtered library's franchise page directly
// when the library is a collection library or has at most smallFilterFranchiseCap
// matching movies with collection_id>0. usedDirect is false when the filter is
// broad and the caller should walk catalog_franchises in cursor batches.
func (c *Catalog) FranchisesInSmallFilter(ctx context.Context, f core.Filter, query string, offset, limit int) (items []core.Collection, hasMore, usedDirect bool, err error) {
	return c.FranchisesInSmallFilterAfter(ctx, f, query, offset, limit, 0, 0)
}

// FranchisesInSmallFilterAfter seeks in the same raw tally order used by
// FranchisesCursor while counting members in the requested library.
func (c *Catalog) FranchisesInSmallFilterAfter(ctx context.Context, f core.Filter, query string, offset, limit, afterCount, afterID int) (items []core.Collection, hasMore, usedDirect bool, err error) {
	if limit <= 0 {
		return nil, false, true, nil
	}
	if offset < 0 {
		offset = 0
	}
	where, baseArgs, err := c.franchiseMemberWhere(f)
	if err != nil {
		return nil, false, false, err
	}
	if f.CollectionMembers == 0 {
		probeArgs := append([]any(nil), baseArgs...)
		probeArgs = append(probeArgs, smallFilterFranchiseCap+1)
		var count int
		err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
			SELECT 1 FROM catalog_titles AS t
			WHERE t.collection_id>0 AND `+where+`
			LIMIT ?
		) q`, probeArgs...).Scan(&count)
		if err != nil {
			return nil, false, false, err
		}
		if count > smallFilterFranchiseCap {
			return nil, false, false, nil
		}
		if count == 0 {
			return nil, false, true, nil
		}
	}
	q := strings.ToLower(strings.TrimSpace(query))
	args := append([]any(nil), baseArgs...)
	extra := ""
	if q != "" {
		extra = ` AND LOWER(COALESCE(NULLIF(f.name,''), cc.name, '')) LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(q)+"%")
	}
	if afterID != 0 {
		extra += ` AND (f.member_count < ? OR (f.member_count = ? AND f.collection_id > ?))`
		args = append(args, afterCount, afterCount, afterID)
	}

	args = append(args, limit+1, offset)
	rows, err := c.db.QueryContext(ctx, `SELECT f.collection_id,
		COALESCE(NULLIF(f.name,''), MAX(cc.name), ''),
		COUNT(*),
		COALESCE(MAX(cc.poster),''), COALESCE(MAX(cc.backdrop),''), COALESCE(MAX(cc.summary),'')
		FROM catalog_titles AS t
		JOIN catalog_franchises AS f ON f.collection_id=t.collection_id AND f.member_count>=2
		LEFT JOIN catalog_collections AS cc ON cc.tmdb_id=f.collection_id
		WHERE t.collection_id>0 AND `+where+extra+`
		GROUP BY f.collection_id, f.name, f.member_count
		ORDER BY f.member_count DESC, f.collection_id ASC
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, false, false, err
	}
	defer rows.Close()
	out, err := scanFranchiseRows(rows)
	if err != nil {
		return nil, false, false, err
	}
	if len(out) > limit {
		hasMore = true
		out = out[:limit]
	}
	return out, hasMore, true, nil
}

// FranchiseSummaryBatch loads metadata for a batch of TMDB collection IDs from
// catalog_franchises and catalog_collections in one query.
func (c *Catalog) FranchiseSummaryBatch(ctx context.Context, collectionIDs []int) (map[int]core.Collection, error) {
	if len(collectionIDs) == 0 {
		return nil, nil
	}
	out := make(map[int]core.Collection, len(collectionIDs))
	for part := range store.Chunks(collectionIDs, 400) {
		args := make([]any, len(part))
		for i, id := range part {
			args[i] = id
		}
		rows, err := c.db.QueryContext(ctx, `SELECT f.collection_id,
			COALESCE(NULLIF(f.name,''), cc.name, ''),
			f.member_count,
			COALESCE(cc.poster,''), COALESCE(cc.backdrop,''), COALESCE(cc.summary,'')
			FROM catalog_franchises f
			LEFT JOIN catalog_collections cc ON cc.tmdb_id = f.collection_id
			WHERE f.collection_id IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		batch, err := scanFranchiseRows(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		for _, col := range batch {
			out[int(col.Key.TMDB())] = col
		}
	}
	return out, nil
}

// FranchiseFallbackArt returns the first visible movie's poster and backdrop
// paths (ordered by release date) for franchises lacking their own artwork.
func (c *Catalog) FranchiseFallbackArt(ctx context.Context, f core.Filter, collectionIDs []int) (map[int][2]string, error) {
	if len(collectionIDs) == 0 {
		return nil, nil
	}
	where, baseArgs, err := c.franchiseMemberWhere(f)
	if err != nil {
		return nil, err
	}
	out := make(map[int][2]string, len(collectionIDs))
	for part := range store.Chunks(collectionIDs, 400) {
		args := make([]any, 0, len(part)+len(baseArgs))
		for _, id := range part {
			args = append(args, id)
		}
		args = append(args, baseArgs...)
		rows, err := c.db.QueryContext(ctx, `SELECT t.collection_id, t.poster_path, t.backdrop_path
			FROM catalog_titles AS t
			WHERE t.collection_id IN (`+placeholders(len(part))+`)
			  AND (t.poster_path<>'' OR t.backdrop_path<>'')
			  AND `+where+`
			ORDER BY t.collection_id ASC, t.released_ts ASC, t.key ASC`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int
			var poster, backdrop string
			if err := rows.Scan(&id, &poster, &backdrop); err != nil {
				rows.Close()
				return nil, err
			}
			cur := out[id]
			if cur[0] == "" && poster != "" {
				cur[0] = poster
			}
			if cur[1] == "" && backdrop != "" {
				cur[1] = backdrop
			}
			out[id] = cur
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// libraryFacetFranchises answers broad franchise facet-collection tabs from
// catalog_franchises in bounded cursor batches instead of grouping every
// franchise movie in catalog_titles.
func (c *Catalog) libraryFacetFranchises(
	ctx context.Context,
	f core.Filter,
	orderByName bool,
	rawQuery string,
	offset, limit int,
	where string,
	args []any,
) ([]core.CatalogFacetCollection, bool, bool, error) {
	if rawQuery != "" || orderByName || f.CollectionMembers != 0 || (f.Type != "" && f.Type != core.TypeMovie) {
		return nil, false, false, nil
	}
	hasFilter := len(f.ExtraRules) > 0 || f.ExtraFallback != "" || len(f.CatalogueSources) > 0
	target := offset + limit + 1
	batchSize := min(max(limit*2, 128), 400)
	afterCount, afterID := 0, 0
	matched, checked, batches := 0, 0, 0
	out := make([]core.CatalogFacetCollection, 0, limit+1)
	for {
		batch, err := franchisesCursorDB(ctx, c.db, 1, afterCount, afterID, batchSize)
		if err != nil {
			return nil, false, false, err
		}
		if len(batch) == 0 {
			break
		}
		batches++
		checked += len(batch)
		ids := make([]int, len(batch))
		for i, col := range batch {
			ids[i] = int(col.Key.TMDB())
		}
		last := batch[len(batch)-1]
		afterCount, afterID = last.ChildCount, int(last.Key.TMDB())

		qArgs := make([]any, 0, len(ids)+len(args))
		for _, id := range ids {
			qArgs = append(qArgs, id)
		}
		qArgs = append(qArgs, args...)
		rows, err := c.db.QueryContext(ctx, `SELECT t.collection_id, COUNT(*)
			FROM catalog_titles t
			WHERE t.collection_id IN (`+placeholders(len(ids))+`) AND (`+where+`)
			GROUP BY t.collection_id`, qArgs...)
		if err != nil {
			return nil, false, false, err
		}
		counts := make(map[int]int, len(ids))
		for rows.Next() {
			var cid, cnt int
			if err := rows.Scan(&cid, &cnt); err != nil {
				rows.Close()
				return nil, false, false, err
			}
			if cnt > 0 {
				counts[cid] = cnt
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, false, false, err
		}
		for _, col := range batch {
			cid := int(col.Key.TMDB())
			cnt := counts[cid]
			name := strings.TrimSpace(col.Title)
			if cnt <= 0 || name == "" {
				continue
			}
			if matched >= offset {
				out = append(out, core.CatalogFacetCollection{
					ID:        fmt.Sprintf("%d", cid),
					Name:      name,
					ItemCount: cnt,
				})
			}
			matched++
			if matched >= target {
				break
			}
		}
		if matched >= target || len(batch) < batchSize {
			break
		}
		if hasFilter && batches == 2 && matched*4 < checked {
			// Ultra-sparse filtered library: fall back to direct SQL grouping
			// rather than walking the rest of catalog_franchises.
			return nil, false, false, nil
		}
	}
	more := false
	if len(out) > limit {
		more = true
		out = out[:limit]
	}
	return out, more, true, nil
}

// FranchiseFilteredPage uses the tally's order index to seek across franchises,
// proving each qualifying bucket with an indexed member EXISTS. It counts
// members only for the rows it returns.
func (c *Catalog) FranchiseFilteredPage(ctx context.Context, f core.Filter, offset, limit, afterCount, afterID int, exactTotal bool) ([]core.Collection, int, error) {
	memberWhere, args, err := c.franchiseMemberWhere(f)
	if err != nil {
		return nil, 0, err
	}
	where := ` WHERE f.member_count>=2 AND EXISTS (
		SELECT 1 FROM catalog_titles t WHERE t.collection_id=f.collection_id AND (` + memberWhere + `)
		LIMIT 1 OFFSET 0
	)`
	var total int
	if exactTotal {
		if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_franchises f`+where, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}
	if limit <= 0 || exactTotal && offset >= total {
		return nil, total, nil
	}
	if afterID > 0 {
		where += ` AND (f.member_count < ? OR (f.member_count = ? AND f.collection_id > ?))`
		args = append(args, afterCount, afterCount, afterID)
	}
	rows, err := c.db.QueryContext(ctx, `SELECT f.collection_id,COALESCE(NULLIF(f.name,''),cc.name,''),
		f.member_count,COALESCE(cc.poster,''),COALESCE(cc.backdrop,''),COALESCE(cc.summary,'')
		FROM catalog_franchises f LEFT JOIN catalog_collections cc ON cc.tmdb_id=f.collection_id`+where+
		` ORDER BY f.member_count DESC,f.collection_id ASC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	page, err := scanFranchiseRows(rows)
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	ids := make([]int, 0, len(page))
	for _, col := range page {
		ids = append(ids, int(col.Key.TMDB()))
	}
	counts, err := c.FranchiseVisibleCounts(ctx, f, ids)
	if err != nil {
		return nil, 0, err
	}
	for i := range page {
		page[i].ChildCount = counts[ids[i]]
	}
	if !exactTotal {
		total = offset + len(page)
	}
	return page, total, nil
}
