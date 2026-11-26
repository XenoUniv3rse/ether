package livetv

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func sqliteCompactExpr(expr string) string {
	s := `lower(coalesce(` + expr + `,''))`
	for _, lit := range []string{
		`' '`, `char(9)`, `char(10)`, `char(13)`,
		`'-'`, `'_'`, `'.'`, `','`, `'&'`, `'|'`, `'/'`, `'\\'`,
		`':'`, `';'`, `''''`, `'"'`, `'('`, `')'`, `'['`, `']'`,
		`'+'`, `'!'`, `'?'`, `'*'`, `'#'`, `'@'`, `'~'`,
	} {
		s = `replace(` + s + `,` + lit + `,'')`
	}
	return s
}

var (
	sqliteCompactNameExpr      = sqliteCompactExpr(`json_extract(data,'$.name')`)
	sqliteCompactNewNameExpr   = sqliteCompactExpr(`json_extract(new.data,'$.name')`)
	sqliteCompactNewNumberExpr = sqliteCompactExpr(`json_extract(new.data,'$.number')`)
)

func init() {
	// FTS docids and readiness are disposable engine-local metadata. Keeping
	// readiness in a dump without its FTS tables would skip the next backfill.
	for _, table := range []string{"livetv_search_state", "livetv_search_channel_keys", "livetv_search_programme_keys"} {
		store.RegisterLocalTable(table)
	}
	store.RegisterRestore("livetv_search", func(ctx context.Context, tx *sql.Tx) (func(context.Context, *sql.Tx) error, error) {
		// Keep the migration seed for active: SQLite starts idle; PostgreSQL
		// maintains GIN indexes unconditionally and starts active.
		_, err := tx.ExecContext(ctx, `UPDATE livetv_search_state SET ready=0 WHERE id=1`)
		return nil, err
	}, "livetv_search_channel_keys", "livetv_search_programme_keys", "livetv_search_channels", "livetv_search_channels_compact", "livetv_search_programmes")

	// Index changes happen in the import transaction, without a worker or polling.
	store.Register("livetv", 21, `
 CREATE TABLE livetv_search_state(id INTEGER PRIMARY KEY, active INTEGER NOT NULL, ready INTEGER NOT NULL); INSERT INTO livetv_search_state VALUES(1,0,0);
 CREATE TABLE livetv_search_channel_keys(docid INTEGER PRIMARY KEY,id TEXT NOT NULL UNIQUE);
 CREATE TABLE livetv_search_programme_keys(docid INTEGER PRIMARY KEY,source TEXT NOT NULL,channel TEXT NOT NULL,start BIGINT NOT NULL,UNIQUE(source,channel,start));
 CREATE VIRTUAL TABLE livetv_search_channels USING fts5(id UNINDEXED, body, tokenize='unicode61');
 CREATE TRIGGER livetv_search_channels_insert AFTER INSERT ON livetv_channels WHEN (SELECT active FROM livetv_search_state WHERE id=1)=1 BEGIN
 INSERT INTO livetv_search_channel_keys(id) VALUES(new.id) ON CONFLICT DO NOTHING;
 INSERT OR REPLACE INTO livetv_search_channels(rowid,id,body) VALUES((SELECT docid FROM livetv_search_channel_keys WHERE id=new.id),new.id,json_extract(new.data,'$.name') || ' ' || coalesce(json_extract(new.data,'$.number'),'') || ' ' || coalesce(json_extract(new.data,'$.guideId'),'')); END;
 CREATE TRIGGER livetv_search_channels_delete AFTER DELETE ON livetv_channels WHEN (SELECT active FROM livetv_search_state WHERE id=1)=1 BEGIN DELETE FROM livetv_search_channels WHERE rowid=(SELECT docid FROM livetv_search_channel_keys WHERE id=old.id); DELETE FROM livetv_search_channel_keys WHERE id=old.id; END;
 CREATE TRIGGER livetv_search_channels_update AFTER UPDATE ON livetv_channels WHEN (SELECT active FROM livetv_search_state WHERE id=1)=1 BEGIN
 DELETE FROM livetv_search_channels WHERE rowid=(SELECT docid FROM livetv_search_channel_keys WHERE id=old.id); DELETE FROM livetv_search_channel_keys WHERE id=old.id; INSERT INTO livetv_search_channel_keys(id) VALUES(new.id) ON CONFLICT DO NOTHING;
 INSERT OR REPLACE INTO livetv_search_channels(rowid,id,body) VALUES((SELECT docid FROM livetv_search_channel_keys WHERE id=new.id),new.id,json_extract(new.data,'$.name') || ' ' || coalesce(json_extract(new.data,'$.number'),'') || ' ' || coalesce(json_extract(new.data,'$.guideId'),'')); END;
 CREATE VIRTUAL TABLE livetv_search_programmes USING fts5(source UNINDEXED, channel UNINDEXED, start UNINDEXED, body, tokenize='unicode61');
 CREATE TRIGGER livetv_search_programmes_insert AFTER INSERT ON livetv_programmes WHEN (SELECT active FROM livetv_search_state WHERE id=1)=1 BEGIN
 INSERT INTO livetv_search_programme_keys(source,channel,start) VALUES(new.source_id,new.channel_id,new.start_ms) ON CONFLICT DO NOTHING;
 INSERT OR REPLACE INTO livetv_search_programmes(rowid,source,channel,start,body) VALUES((SELECT docid FROM livetv_search_programme_keys WHERE source=new.source_id AND channel=new.channel_id AND start=new.start_ms),new.source_id,new.channel_id,new.start_ms,coalesce(json_extract(new.data,'$.item.title'),'') || ' ' || coalesce(json_extract(new.data,'$.item.episodeTitle'),'') || ' ' || coalesce(json_extract(new.data,'$.item.summary'),'')); END;
 CREATE TRIGGER livetv_search_programmes_delete AFTER DELETE ON livetv_programmes WHEN (SELECT active FROM livetv_search_state WHERE id=1)=1 BEGIN DELETE FROM livetv_search_programmes WHERE rowid=(SELECT docid FROM livetv_search_programme_keys WHERE source=old.source_id AND channel=old.channel_id AND start=old.start_ms); DELETE FROM livetv_search_programme_keys WHERE source=old.source_id AND channel=old.channel_id AND start=old.start_ms; END;
 CREATE TRIGGER livetv_search_programmes_update AFTER UPDATE ON livetv_programmes WHEN (SELECT active FROM livetv_search_state WHERE id=1)=1 BEGIN
 DELETE FROM livetv_search_programmes WHERE rowid=(SELECT docid FROM livetv_search_programme_keys WHERE source=old.source_id AND channel=old.channel_id AND start=old.start_ms); DELETE FROM livetv_search_programme_keys WHERE source=old.source_id AND channel=old.channel_id AND start=old.start_ms; INSERT INTO livetv_search_programme_keys(source,channel,start) VALUES(new.source_id,new.channel_id,new.start_ms) ON CONFLICT DO NOTHING;
 INSERT OR REPLACE INTO livetv_search_programmes(rowid,source,channel,start,body) VALUES((SELECT docid FROM livetv_search_programme_keys WHERE source=new.source_id AND channel=new.channel_id AND start=new.start_ms),new.source_id,new.channel_id,new.start_ms,coalesce(json_extract(new.data,'$.item.title'),'') || ' ' || coalesce(json_extract(new.data,'$.item.episodeTitle'),'') || ' ' || coalesce(json_extract(new.data,'$.item.summary'),'')); END;
 CREATE INDEX livetv_search_name_fallback ON livetv_channels(lower(coalesce(json_extract(data,'$.name'),'')));
 CREATE INDEX livetv_search_number_fallback ON livetv_channels(lower(coalesce(json_extract(data,'$.number'),'')));
 CREATE INDEX livetv_search_channel_guide ON livetv_channels(coalesce(nullif(json_extract(data,'$.guideSourceId'),''),json_extract(data,'$.sourceId')),json_extract(data,'$.guideId'));
 `)
	store.RegisterDialect("livetv", 21, store.Postgres, `
 CREATE TABLE livetv_search_state(id INTEGER PRIMARY KEY, active INTEGER NOT NULL, ready INTEGER NOT NULL); INSERT INTO livetv_search_state VALUES(1,1,0);
 CREATE INDEX livetv_search_channels ON livetv_channels USING gin(to_tsvector('simple',coalesce(data::jsonb->>'name','') || ' ' || coalesce(data::jsonb->>'number','') || ' ' || coalesce(data::jsonb->>'guideId','')));
 CREATE INDEX livetv_search_programmes ON livetv_programmes USING gin(to_tsvector('simple',coalesce(data::jsonb->'item'->>'title','') || ' ' || coalesce(data::jsonb->'item'->>'episodeTitle','') || ' ' || coalesce(data::jsonb->'item'->>'summary','')));
 CREATE INDEX livetv_search_channel_guide ON livetv_channels(coalesce(nullif(data::jsonb->>'guideSourceId',''),data::jsonb->>'sourceId'),(data::jsonb->>'guideId'));
 `)

	// Migration 27 indexes space- and punctuation-normalised channel names so
	// queries like "red zone" match "NFL RedZone" and "fox5" matches "FOX 5"
	// through indexes on both SQLite and PostgreSQL.
	store.Register("livetv", 27, `
 CREATE VIRTUAL TABLE livetv_search_channels_compact USING fts5(id UNINDEXED, body, tokenize='trigram');
 CREATE INDEX livetv_search_compact_fallback ON livetv_channels(`+sqliteCompactNameExpr+`);
 DROP TRIGGER IF EXISTS livetv_search_channels_insert;
 DROP TRIGGER IF EXISTS livetv_search_channels_delete;
 DROP TRIGGER IF EXISTS livetv_search_channels_update;
 CREATE TRIGGER livetv_search_channels_insert AFTER INSERT ON livetv_channels WHEN (SELECT active FROM livetv_search_state WHERE id=1)=1 BEGIN
  INSERT INTO livetv_search_channel_keys(id) VALUES(new.id) ON CONFLICT DO NOTHING;
  INSERT OR REPLACE INTO livetv_search_channels(rowid,id,body) VALUES((SELECT docid FROM livetv_search_channel_keys WHERE id=new.id),new.id,coalesce(json_extract(new.data,'$.name'),'') || ' ' || coalesce(json_extract(new.data,'$.number'),'') || ' ' || coalesce(json_extract(new.data,'$.guideId'),'') || ' ' || `+sqliteCompactNewNameExpr+`);
  INSERT OR REPLACE INTO livetv_search_channels_compact(rowid,id,body) VALUES((SELECT docid FROM livetv_search_channel_keys WHERE id=new.id),new.id,`+sqliteCompactNewNameExpr+` || ' ' || `+sqliteCompactNewNumberExpr+`);
 END;
 CREATE TRIGGER livetv_search_channels_delete AFTER DELETE ON livetv_channels WHEN (SELECT active FROM livetv_search_state WHERE id=1)=1 BEGIN
  DELETE FROM livetv_search_channels WHERE rowid=(SELECT docid FROM livetv_search_channel_keys WHERE id=old.id);
  DELETE FROM livetv_search_channels_compact WHERE rowid=(SELECT docid FROM livetv_search_channel_keys WHERE id=old.id);
  DELETE FROM livetv_search_channel_keys WHERE id=old.id;
 END;
 CREATE TRIGGER livetv_search_channels_update AFTER UPDATE ON livetv_channels WHEN (SELECT active FROM livetv_search_state WHERE id=1)=1 BEGIN
  DELETE FROM livetv_search_channels WHERE rowid=(SELECT docid FROM livetv_search_channel_keys WHERE id=old.id);
  DELETE FROM livetv_search_channels_compact WHERE rowid=(SELECT docid FROM livetv_search_channel_keys WHERE id=old.id);
  DELETE FROM livetv_search_channel_keys WHERE id=old.id;
  INSERT INTO livetv_search_channel_keys(id) VALUES(new.id) ON CONFLICT DO NOTHING;
  INSERT OR REPLACE INTO livetv_search_channels(rowid,id,body) VALUES((SELECT docid FROM livetv_search_channel_keys WHERE id=new.id),new.id,coalesce(json_extract(new.data,'$.name'),'') || ' ' || coalesce(json_extract(new.data,'$.number'),'') || ' ' || coalesce(json_extract(new.data,'$.guideId'),'') || ' ' || `+sqliteCompactNewNameExpr+`);
  INSERT OR REPLACE INTO livetv_search_channels_compact(rowid,id,body) VALUES((SELECT docid FROM livetv_search_channel_keys WHERE id=new.id),new.id,`+sqliteCompactNewNameExpr+` || ' ' || `+sqliteCompactNewNumberExpr+`);
 END;
 UPDATE livetv_search_state SET ready=0 WHERE id=1;
 `)
	store.RegisterDialect("livetv", 27, store.Postgres, `
 CREATE OR REPLACE FUNCTION livetv_channel_search_text(name text, num text, guide_id text) RETURNS text
 LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT
   coalesce(name,'') || ' ' || coalesce(num,'') || ' ' || coalesce(guide_id,'') ||
   CASE WHEN coalesce(num,'') <> '' THEN ' ' || regexp_replace(lower(num), '[^[:alnum:]]+', '', 'g') ELSE '' END ||
   coalesce((
    SELECT ' ' || string_agg(words[i] || ' ' || array_to_string(words[i:], ''), ' ')
    FROM (
     SELECT array_remove(
      regexp_split_to_array(
       lower(
        regexp_replace(
         regexp_replace(
          regexp_replace(
           regexp_replace(coalesce(name,''), '([A-Z]+)([A-Z][a-z])', '\1 \2', 'g'),
           '([a-z])([A-Z])', '\1 \2', 'g'
          ),
          '([A-Za-z])([0-9])', '\1 \2', 'g'
         ),
         '([0-9])([A-Za-z])', '\1 \2', 'g'
        )
       ),
       '[^[:alnum:]]+'
      ),
      ''
     ) AS words
    ) w,
    LATERAL generate_subscripts(words, 1) AS i
    WHERE i <= 12
   ), '')
 $$;
 DROP INDEX IF EXISTS livetv_search_channels;
 CREATE INDEX livetv_search_channels ON livetv_channels USING gin(to_tsvector('simple', livetv_channel_search_text(data::jsonb->>'name', data::jsonb->>'number', data::jsonb->>'guideId')));
 `)
}

func searchWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func compactSearchText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// channelBoundaryWords splits a channel name into lowercase alphanumeric runs
// at punctuation, whitespace, CamelCase, and letter/digit boundaries.
func channelBoundaryWords(s string) []string {
	runes := []rune(s)
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if cur.Len() > 0 {
			prev := runes[i-1]
			split := (unicode.IsLower(prev) && unicode.IsUpper(r)) ||
				(unicode.IsLetter(prev) && unicode.IsDigit(r)) ||
				(unicode.IsDigit(prev) && unicode.IsLetter(r)) ||
				(unicode.IsUpper(prev) && unicode.IsUpper(r) && i+1 < len(runes) && unicode.IsLower(runes[i+1]))
			if split {
				flush()
			}
		}
		cur.WriteRune(unicode.ToLower(r))
	}
	flush()
	return out
}

// channelSearchBody produces the FTS5 token body for a channel: the raw name,
// number, and guide ID plus CamelCase/digit-split words and word-boundary
// compact suffixes so queries like "red zone", "redz", "nfl red", "fox5",
// "nickjr", and "espn2" hit the word-prefix index directly.
func channelSearchBody(name, number, guideID string) string {
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(number)
	b.WriteByte(' ')
	b.WriteString(guideID)
	if compactNum := compactSearchText(number); compactNum != "" && compactNum != strings.ToLower(number) {
		b.WriteByte(' ')
		b.WriteString(compactNum)
	}
	words := channelBoundaryWords(name)
	if len(words) > 12 {
		words = words[:12]
	}
	for i := range words {
		b.WriteByte(' ')
		b.WriteString(words[i])
		if i+1 < len(words) {
			b.WriteByte(' ')
			for j := i; j < len(words); j++ {
				b.WriteString(words[j])
			}
		}
	}
	return b.String()
}

// channelSearchRank evaluates how well a channel matches the parsed query.
// Lower rank numbers are better matches:
//
//	1: exact compact match on name or number
//	2: compact prefix match on name or number
//	3: compact prefix match at a word/CamelCase/digit boundary in name
//	4: every query word matches a word prefix across name/number/guideId
//	5: compact substring match inside name
func channelSearchRank(name, number, guideID string, plainWords []string, compactQ string) (int, bool) {
	if compactQ == "" || len(plainWords) == 0 {
		return 0, false
	}
	nameCompact := compactSearchText(name)
	numCompact := compactSearchText(number)
	if nameCompact == compactQ || numCompact == compactQ {
		return 1, true
	}
	if strings.HasPrefix(nameCompact, compactQ) || strings.HasPrefix(numCompact, compactQ) {
		return 2, true
	}
	bwords := channelBoundaryWords(name)
	for i := range bwords {
		var suffix strings.Builder
		for j := i; j < len(bwords); j++ {
			suffix.WriteString(bwords[j])
		}
		if strings.HasPrefix(suffix.String(), compactQ) {
			return 3, true
		}
	}
	tokens := append([]string(nil), bwords...)
	tokens = append(tokens, searchWords(name)...)
	tokens = append(tokens, searchWords(number)...)
	tokens = append(tokens, searchWords(guideID)...)
	allWords := true
	for _, w := range plainWords {
		found := false
		for _, tok := range tokens {
			if strings.HasPrefix(tok, w) {
				found = true
				break
			}
		}
		if !found {
			allWords = false
			break
		}
	}
	if allWords {
		return 4, true
	}
	if utf8.RuneCountInString(compactQ) >= 3 && strings.Contains(nameCompact, compactQ) {
		return 5, true
	}
	return 0, false
}

func channelMatchesText(name, number, extra, query string) bool {
	words := searchWords(query)
	if len(words) == 0 {
		return true
	}
	compactQ := strings.Join(words, "")
	_, ok := channelSearchRank(name, number, extra, words, compactQ)
	return ok
}

type policyRowCatalog interface {
	CachedPolicyRows(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)
}

// searchPolicyCache batches and memoizes local catalogue policy lookups for a
// single Live TV search so indexed virtual/personal programmes do not trigger
// per-programme full-title reads (including trailers, credits, and alt titles).
type searchPolicyCache struct {
	s      *Service
	batch  policyRowCatalog
	loaded map[core.RatingKey]bool
	rows   map[core.RatingKey]core.Title
}

func newSearchPolicyCache(s *Service) *searchPolicyCache {
	var batch policyRowCatalog
	if s.o.Catalog != nil {
		batch, _ = s.o.Catalog.(policyRowCatalog)
	}
	return &searchPolicyCache{
		s:      s,
		batch:  batch,
		loaded: map[core.RatingKey]bool{},
		rows:   map[core.RatingKey]core.Title{},
	}
}

func (pc *searchPolicyCache) preload(ctx context.Context, keys []core.RatingKey) error {
	if pc.batch == nil || len(keys) == 0 {
		return nil
	}
	missing := make([]core.RatingKey, 0, len(keys))
	seen := make(map[core.RatingKey]bool, len(keys))
	for _, k := range keys {
		if k == 0 || pc.loaded[k] || seen[k] {
			continue
		}
		seen[k] = true
		missing = append(missing, k)
	}
	if len(missing) == 0 {
		return nil
	}
	got, err := pc.batch.CachedPolicyRows(ctx, missing)
	if err != nil {
		return err
	}
	for _, k := range missing {
		pc.loaded[k] = true
	}
	for k, t := range got {
		pc.rows[k] = t
	}
	return nil
}

func (pc *searchPolicyCache) lookup(c Channel) func(context.Context, core.RatingKey) (*core.Title, error) {
	if pc.s.o.Catalog == nil {
		return nil
	}
	return func(ctx context.Context, key core.RatingKey) (*core.Title, error) {
		if pc.loaded[key] {
			if t, ok := pc.rows[key]; ok {
				return &t, nil
			}
			return nil, core.ErrNotFound
		}
		if pc.batch != nil {
			if err := pc.preload(ctx, []core.RatingKey{key}); err != nil {
				return nil, err
			}
			if t, ok := pc.rows[key]; ok {
				return &t, nil
			}
			return nil, core.ErrNotFound
		}
		fn := pc.s.o.Catalog.Get
		if c.Personal != nil {
			if cached, ok := pc.s.o.Catalog.(interface {
				CachedGet(context.Context, core.RatingKey) (*core.Title, error)
			}); ok {
				fn = cached.CachedGet
			}
		}
		t, err := fn(ctx, key)
		if ctx.Err() == nil {
			pc.loaded[key] = true
			if err == nil && t != nil {
				pc.rows[key] = *t
			}
		}
		return t, err
	}
}

func (s *Service) allowedProgrammeWithLookup(ctx context.Context, u *core.User, c Channel, p *Programme, lookup func(context.Context, core.RatingKey) (*core.Title, error)) bool {
	if p != nil && p.policyTag != nil {
		tag := p.policyTag
		var tagLookup func(context.Context, core.RatingKey) (*core.Title, error)
		if tag.catalog {
			tagLookup = func(context.Context, core.RatingKey) (*core.Title, error) { return tag.title, nil }
		}
		return s.allowedWithLookup(ctx, u, c, p, tagLookup, false)
	}
	return s.allowedWithLookup(ctx, u, c, p, lookup, false)
}

// SearchLiveTV searches identities once, regardless of group overlap. Only a
// bounded candidate window is decoded and checked by the normal viewer policy.
func (h liveTVHost) SearchLiveTV(ctx context.Context, viewer *core.Viewer, query string, limit int) (frontend.LiveSearchResults, error) {
	out := frontend.LiveSearchResults{Channels: []frontend.LiveTVChannel{}, Programmes: []frontend.LiveTVProgramme{}}
	plainWords := searchWords(query)
	if len(plainWords) == 0 || len(query) > 1024 || !h.s.Enabled() {
		return out, nil
	}
	if len(plainWords) > 6 {
		plainWords = plainWords[:6]
	}
	compactQ := strings.Join(plainWords, "")
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 100)
	pg := store.DialectOf(h.s.o.DB) == store.Postgres
	if err := h.s.demandSearchIndex(ctx); err != nil {
		return out, err
	}
	h.s.searchMu.Lock()
	out.Partial = !h.s.searchReady
	h.s.searchMu.Unlock()
	words := make([]string, len(plainWords))
	for i, w := range plainWords {
		if pg {
			words[i] = w + ":*"
		} else {
			words[i] = "\"" + w + "\"*"
		}
	}
	var match string
	if pg {
		match = strings.Join(words, " & ")
		if len(plainWords) > 1 && compactQ != "" {
			match = "(" + match + ") | " + compactQ + ":*"
		}
	} else {
		match = strings.Join(words, " AND ")
		if len(plainWords) > 1 && compactQ != "" {
			match = "(" + match + ") OR \"" + compactQ + "\"*"
		}
	}
	// The unary + strips p's column affinity so SQLite probes
	// livetv_search_channel_guide for each match. Without it every matched
	// programme scans the whole channel table (25 s for "office" on a large
	// lineup).
	cq := `SELECT id FROM livetv_search_channels WHERE livetv_search_channels MATCH ? ORDER BY rank LIMIT 400`
	pq := `SELECT c.id,p.data FROM livetv_search_programmes f JOIN livetv_programmes p ON p.source_id=f.source AND p.channel_id=f.channel AND p.start_ms=f.start JOIN livetv_channels c ON coalesce(nullif(json_extract(c.data,'$.guideSourceId'),''),json_extract(c.data,'$.sourceId'))=+p.source_id AND json_extract(c.data,'$.guideId')=+p.channel_id WHERE livetv_search_programmes MATCH ? AND p.end_ms>? AND p.start_ms<? ORDER BY p.start_ms,c.id LIMIT 400`
	if pg {
		cq = `SELECT id FROM livetv_channels WHERE to_tsvector('simple', livetv_channel_search_text(data::jsonb->>'name', data::jsonb->>'number', data::jsonb->>'guideId')) @@ to_tsquery('simple',?) ORDER BY id LIMIT 400`
		pq = `SELECT c.id,p.data FROM livetv_programmes p JOIN livetv_channels c ON coalesce(nullif(c.data::jsonb->>'guideSourceId',''),c.data::jsonb->>'sourceId')=p.source_id AND c.data::jsonb->>'guideId'=p.channel_id WHERE to_tsvector('simple',coalesce(p.data::jsonb->'item'->>'title','') || ' ' || coalesce(p.data::jsonb->'item'->>'episodeTitle','') || ' ' || coalesce(p.data::jsonb->'item'->>'summary','')) @@ to_tsquery('simple',?) AND p.end_ms>? AND p.start_ms<? ORDER BY p.start_ms,c.id LIMIT 400`
	}
	channelCandIDs := []string{}
	seenCand := map[string]bool{}
	collectChannelIDs := func(q string, args ...any) error {
		rows, err := h.s.o.DB.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			if !seenCand[id] {
				seenCand[id] = true
				channelCandIDs = append(channelCandIDs, id)
			}
		}
		re := rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		return re
	}
	if err := collectChannelIDs(cq, match); err != nil {
		return out, err
	}
	if !pg {
		if utf8.RuneCountInString(compactQ) >= 3 {
			compactSQL := `SELECT id FROM livetv_search_channels_compact WHERE livetv_search_channels_compact MATCH ? ORDER BY rank LIMIT 400`
			if err := collectChannelIDs(compactSQL, `"`+compactQ+`"`); err != nil {
				return out, err
			}
		}
		if utf8.RuneCountInString(compactQ) < 3 || out.Partial {
			compactFallback := `SELECT id FROM livetv_channels WHERE ` + sqliteCompactNameExpr + `>=? AND ` + sqliteCompactNameExpr + `<? LIMIT 400`
			if err := collectChannelIDs(compactFallback, compactQ, compactQ+"\uffff"); err != nil {
				return out, err
			}
		}
		if out.Partial {
			prefix := plainWords[0]
			fallback := `SELECT id FROM livetv_channels WHERE lower(coalesce(json_extract(data,'$.name'),''))>=? AND lower(coalesce(json_extract(data,'$.name'),''))<? UNION SELECT id FROM livetv_channels WHERE lower(coalesce(json_extract(data,'$.number'),''))>=? AND lower(coalesce(json_extract(data,'$.number'),''))<? LIMIT 400`
			if err := collectChannelIDs(fallback, prefix, prefix+"\uffff", prefix, prefix+"\uffff"); err != nil {
				return out, err
			}
		}
	}
	now := h.s.o.Now()
	rows, err := h.s.o.DB.QueryContext(ctx, pq, match, now.UnixMilli(), now.Add(7*24*time.Hour).UnixMilli())
	if err != nil {
		return out, err
	}
	ps := []Programme{}
	progChannelIDs := []string{}
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			break
		}
		var p Programme
		if err = json.Unmarshal(raw, &p); err != nil {
			break
		}
		p.ChannelID = id
		p.ID = fmt.Sprintf("%s-%d", id, p.Start.UnixMilli())
		ps = append(ps, p)
		progChannelIDs = append(progChannelIDs, id)
	}
	re := rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if re != nil {
		return out, re
	}
	viewID := int64(0)
	if viewer != nil {
		viewID = viewer.ID
	}
	vq := `SELECT v.channel_id,v.data FROM livetv_search_virtual_fts f JOIN livetv_search_virtual v ON v.rowid=f.rowid WHERE livetv_search_virtual_fts MATCH ? AND v.end_ms>? AND v.start_ms<? AND (v.viewer_id=0 OR v.viewer_id=?) ORDER BY v.start_ms LIMIT 400`
	if pg {
		vq = `SELECT channel_id,data FROM livetv_search_virtual WHERE to_tsvector('simple',body) @@ to_tsquery('simple',?) AND end_ms>? AND start_ms<? AND (viewer_id=0 OR viewer_id=?) ORDER BY start_ms LIMIT 400`
	}
	vr, e := h.s.o.DB.QueryContext(ctx, vq, match, now.UnixMilli(), now.Add(7*24*time.Hour).UnixMilli(), viewID)
	if e != nil {
		return out, e
	}
	for vr.Next() {
		var id string
		var raw []byte
		if e = vr.Scan(&id, &raw); e != nil {
			break
		}
		var p Programme
		if e = json.Unmarshal(raw, &p); e != nil {
			break
		}
		p.ChannelID = id
		ps = append(ps, p)
		progChannelIDs = append(progChannelIDs, id)
	}
	re = vr.Err()
	vr.Close()
	if e != nil {
		return out, e
	}
	if re != nil {
		return out, re
	}
	ctx, _, err = h.s.withPolicy(ctx, viewer)
	if err != nil {
		return out, err
	}
	allIDs := make([]string, 0, len(channelCandIDs)+len(progChannelIDs))
	allIDs = append(allIDs, channelCandIDs...)
	allIDs = append(allIDs, progChannelIDs...)
	unique := uniqueIDs(allIDs)
	visible, err := h.s.searchVisibleGroupIDs(ctx, unique)
	if err != nil {
		return out, err
	}
	policyCache := newSearchPolicyCache(h.s)
	candidates, err := h.s.searchAllowedChannelsWithPolicy(ctx, viewer, unique, policyCache)
	if err != nil {
		return out, err
	}
	allowed := map[string]Channel{}
	for _, c := range candidates {
		if visible[c.ID] {
			allowed[c.ID] = c
		}
	}
	type rankedChannel struct {
		c    Channel
		rank int
	}
	matched := make([]rankedChannel, 0, len(channelCandIDs))
	for _, id := range channelCandIDs {
		c, ok := allowed[id]
		if !ok {
			continue
		}
		rank, ok := channelSearchRank(c.Name, c.Number, c.GuideID, plainWords, compactQ)
		if !ok {
			continue
		}
		matched = append(matched, rankedChannel{c: c, rank: rank})
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].rank < matched[j].rank })
	for i := 0; i < len(matched) && i < limit; i++ {
		out.Channels = append(out.Channels, ToFrontendChannel(matched[i].c))
	}
	channelIDs := map[string]bool{}
	seen := map[string]bool{}
	for i := 0; i < len(ps) && len(out.Programmes) < limit; {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		batchSize := max(limit-len(out.Programmes), 32)
		batch := make([]Programme, 0, batchSize)
		preloadKeys := make([]core.RatingKey, 0, batchSize)
		for i < len(ps) && len(batch) < batchSize {
			p := ps[i]
			i++
			if _, ok := allowed[p.ChannelID]; !ok || seen[p.ID] {
				continue
			}
			seen[p.ID] = true
			batch = append(batch, p)
			if p.policyTag == nil && p.Item.Key != 0 {
				preloadKeys = append(preloadKeys, p.Item.Key)
			}
		}
		if len(preloadKeys) > 0 {
			if err := policyCache.preload(ctx, preloadKeys); err != nil {
				return out, err
			}
		}
		for j := range batch {
			if len(out.Programmes) >= limit {
				break
			}
			if err := ctx.Err(); err != nil {
				return out, err
			}
			p := batch[j]
			c := allowed[p.ChannelID]
			if h.s.allowedProgrammeWithLookup(ctx, viewer, c, &p, policyCache.lookup(c)) {
				out.Programmes = append(out.Programmes, toFrontendProgramme(p))
				if !channelIDs[c.ID] {
					channelIDs[c.ID] = true
					out.ProgrammeChannels = append(out.ProgrammeChannels, ToFrontendChannel(c))
				}
			}
		}
	}
	return out, nil
}

// First use asks for preparation and returns immediately; never walk a large
// lineup in a search request. Backfill yields between small write transactions.
func (s *Service) demandSearchIndex(ctx context.Context) error {
	s.searchDemanded.Store(true)
	s.searchMu.Lock()
	defer s.searchMu.Unlock()
	if s.searchClosed {
		return core.ErrNotAvailable
	}
	if s.searchReady {
		return nil
	}
	if store.DialectOf(s.o.DB) == store.Postgres {
		// PostgreSQL maintains GIN indexes unconditionally.
		s.searchReady = true
		return nil
	}
	var ready int
	if err := s.o.DB.QueryRowContext(ctx, `SELECT ready FROM livetv_search_state WHERE id=1`).Scan(&ready); err != nil {
		return err
	}
	if !s.searchBuilding {
		s.searchBuilding = true
		done := make(chan struct{})
		s.searchDone = done
		s.wg.Add(1)
		s.ctxMu.Lock()
		workerCtx := s.ctx
		s.ctxMu.Unlock()
		go func() {
			defer s.wg.Done()
			err := s.buildSearchIndex(workerCtx, ready == 0)
			s.searchMu.Lock()
			s.searchBuilding = false
			s.searchReady = err == nil
			if s.searchDone == done {
				s.searchDone = nil
			}
			close(done)
			s.searchMu.Unlock()
			if err != nil && !errors.Is(err, context.Canceled) {
				s.o.Logger.Warn("live search index preparation failed", "error", err)
			}
		}()
	}
	return nil
}

func (s *Service) isSearchReady() bool {
	if s == nil {
		return false
	}
	if s.o.DB != nil && store.DialectOf(s.o.DB) == store.Postgres {
		return true
	}
	s.searchMu.Lock()
	ready := s.searchReady
	s.searchMu.Unlock()
	return ready
}

// prepareWatchSearchIndex activates migration 27's channel search index and
// waits briefly for small lineups to finish indexing without blocking on
// tens-of-thousands-of-channel backfills.
func (s *Service) prepareWatchSearchIndex(ctx context.Context) (bool, error) {
	if err := s.demandSearchIndex(ctx); err != nil {
		return false, err
	}
	s.searchMu.Lock()
	ready := s.searchReady
	done := s.searchDone
	s.searchMu.Unlock()
	if ready || done == nil {
		return ready, nil
	}
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return s.isSearchReady(), nil
}
func (s *Service) buildSearchIndex(ctx context.Context, backfill bool) error {
	if _, err := s.o.DB.ExecContext(ctx, `UPDATE livetv_search_state SET active=1 WHERE id=1`); err != nil {
		return err
	}
	tables := []string{}
	if backfill && store.DialectOf(s.o.DB) != store.Postgres {
		tables = []string{"livetv_channels", "livetv_programmes"}
	}
	for _, table := range tables {
		var last int64
		for {
			var end int64
			err := s.o.DB.QueryRowContext(ctx, `SELECT coalesce(max(rowid),0) FROM (SELECT rowid FROM `+table+` WHERE rowid>? ORDER BY rowid LIMIT 500)`, last).Scan(&end)
			if err != nil {
				return err
			}
			if end == 0 {
				break
			}
			keyQuery := `INSERT INTO livetv_search_channel_keys(id) SELECT id FROM livetv_channels WHERE rowid>? AND rowid<=? ON CONFLICT DO NOTHING`
			if table == "livetv_programmes" {
				keyQuery = `INSERT INTO livetv_search_programme_keys(source,channel,start) SELECT source_id,channel_id,start_ms FROM livetv_programmes WHERE rowid>? AND rowid<=? ON CONFLICT DO NOTHING`
			}
			batch, err := s.o.DB.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			if _, err = batch.ExecContext(ctx, keyQuery, last, end); err != nil {
				batch.Rollback()
				return err
			}
			var batchCount int
			if table == "livetv_channels" {
				type chanIndexRow struct {
					docid   int64
					id      string
					name    string
					number  string
					guideID string
				}
				rs, qerr := batch.QueryContext(ctx, `SELECT k.docid,c.id,coalesce(json_extract(c.data,'$.name'),''),coalesce(json_extract(c.data,'$.number'),''),coalesce(json_extract(c.data,'$.guideId'),'') FROM livetv_channels c JOIN livetv_search_channel_keys k ON k.id=c.id WHERE c.rowid>? AND c.rowid<=?`, last, end)
				if qerr != nil {
					batch.Rollback()
					return qerr
				}
				buf := make([]chanIndexRow, 0, 500)
				for rs.Next() {
					var r chanIndexRow
					if qerr = rs.Scan(&r.docid, &r.id, &r.name, &r.number, &r.guideID); qerr != nil {
						break
					}
					buf = append(buf, r)
				}
				re := rs.Err()
				rs.Close()
				if qerr != nil {
					batch.Rollback()
					return qerr
				}
				if re != nil {
					batch.Rollback()
					return re
				}
				batchCount = len(buf)
				mainStmt, serr := batch.PrepareContext(ctx, `INSERT OR REPLACE INTO livetv_search_channels(rowid,id,body) VALUES(?,?,?)`)
				if serr != nil {
					batch.Rollback()
					return serr
				}
				compactStmt, cerr := batch.PrepareContext(ctx, `INSERT OR REPLACE INTO livetv_search_channels_compact(rowid,id,body) VALUES(?,?,?)`)
				if cerr != nil {
					mainStmt.Close()
					batch.Rollback()
					return cerr
				}
				for _, r := range buf {
					if _, err = mainStmt.ExecContext(ctx, r.docid, r.id, channelSearchBody(r.name, r.number, r.guideID)); err != nil {
						break
					}
					if _, err = compactStmt.ExecContext(ctx, r.docid, r.id, compactSearchText(r.name)+" "+compactSearchText(r.number)); err != nil {
						break
					}
				}
				mainStmt.Close()
				compactStmt.Close()
				if err != nil {
					batch.Rollback()
					return err
				}
			} else {
				query := `INSERT OR REPLACE INTO livetv_search_programmes(rowid,source,channel,start,body) SELECT (SELECT docid FROM livetv_search_programme_keys k WHERE k.source=livetv_programmes.source_id AND k.channel=livetv_programmes.channel_id AND k.start=livetv_programmes.start_ms),source_id,channel_id,start_ms,coalesce(json_extract(data,'$.item.title'),'') || ' ' || coalesce(json_extract(data,'$.item.episodeTitle'),'') || ' ' || coalesce(json_extract(data,'$.item.summary'),'') FROM livetv_programmes WHERE rowid>? AND rowid<=?`
				res, qerr := batch.ExecContext(ctx, query, last, end)
				if qerr != nil {
					batch.Rollback()
					return qerr
				}
				if n, rerr := res.RowsAffected(); rerr == nil {
					batchCount = int(n)
				}
			}
			if err = batch.Commit(); err != nil {
				return err
			}
			last = end
			if batchCount < 500 {
				continue
			}
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	if err := s.backfillPersonalSearch(ctx); err != nil {
		return err
	}
	s.virtualGuide.mu.RLock()
	snapshots := make(map[string]*virtualGuideSnapshot, len(s.virtualGuide.entries))
	for id, entry := range s.virtualGuide.entries {
		snapshots[id] = entry
	}
	s.virtualGuide.mu.RUnlock()
	for id, entry := range snapshots {
		if err := s.indexVirtualSearchSnapshot(ctx, id, entry); err != nil {
			return err
		}
	}
	_, err := s.o.DB.ExecContext(ctx, `UPDATE livetv_search_state SET ready=1 WHERE id=1`)
	return err
}

func (s *Service) ownerSearchRoute(w http.ResponseWriter, r *http.Request) {
	result, err := (liveTVHost{s: s}).SearchLiveTV(r.Context(), &core.User{IsOwner: true}, r.URL.Query().Get("q"), 8)
	channels := []map[string]any{}
	programmes := []map[string]any{}
	for _, c := range result.Channels {
		channels = append(channels, map[string]any{"id": c.ID, "name": c.Name, "number": c.Number})
	}
	for _, p := range result.Programmes {
		programmes = append(programmes, map[string]any{"id": p.ID, "channelId": p.ChannelID, "title": p.Item.Title, "episodeTitle": p.Item.EpisodeTitle, "start": p.Start, "end": p.End})
	}
	ownerResult(w, map[string]any{"channels": channels, "programmes": programmes}, err)
}

// Search consumes only existing schedules. In particular a name hit must not
// prepare an unwatched virtual channel as a side effect of policy checking.
func (s *Service) searchAllowedChannels(ctx context.Context, viewer *core.Viewer, ids []string) ([]Channel, error) {
	return s.searchAllowedChannelsWithPolicy(ctx, viewer, ids, newSearchPolicyCache(s))
}

func (s *Service) searchAllowedChannelsWithPolicy(ctx context.Context, viewer *core.Viewer, ids []string, policyCache *searchPolicyCache) ([]Channel, error) {
	ctx, policy, err := s.withPolicy(ctx, viewer)
	if err != nil {
		return nil, err
	}
	records, err := s.channelsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	cs := []Channel{}
	iptv := []Channel{}
	for _, id := range ids {
		if c, ok := records[id]; ok {
			cs = append(cs, c)
			if c.Kind != "virtual" {
				iptv = append(iptv, c)
			}
		}
	}
	now := s.o.Now()
	current, err := s.programmesAt(ctx, iptv, now)
	if err != nil {
		return nil, err
	}
	airing := map[string]Programme{}
	for i, p := range current {
		airing[iptv[i].ID] = p
	}
	audienceOK := make([]Channel, 0, len(cs))
	personalIDs := []string{}
	for _, c := range cs {
		if !s.audienceAllowed(ctx, viewer, c) {
			continue
		}
		audienceOK = append(audienceOK, c)
		if c.Personal != nil && viewer != nil {
			personalIDs = append(personalIDs, c.ID)
		}
	}
	personalAiring := map[string]Programme{}
	if len(personalIDs) > 0 && viewer != nil {
		nowMS := now.UnixMilli()
		for chunk := range store.Chunks(personalIDs, 200) {
			ph := strings.Repeat("?,", len(chunk))
			ph = ph[:len(ph)-1]
			args := make([]any, 0, len(chunk)+3)
			args = append(args, viewer.ID)
			for _, id := range chunk {
				args = append(args, id)
			}
			args = append(args, nowMS, nowMS)
			rows, qerr := s.o.DB.QueryContext(ctx, `SELECT channel_id, data FROM livetv_search_virtual WHERE viewer_id=? AND channel_id IN (`+ph+`) AND start_ms<=? AND end_ms>? ORDER BY channel_id, start_ms DESC`, args...)
			if qerr != nil {
				return nil, qerr
			}
			for rows.Next() {
				var cid string
				var raw []byte
				if qerr = rows.Scan(&cid, &raw); qerr != nil {
					break
				}
				if _, seen := personalAiring[cid]; seen {
					continue
				}
				var cur Programme
				if qerr = json.Unmarshal(raw, &cur); qerr != nil {
					break
				}
				personalAiring[cid] = cur
			}
			re := rows.Err()
			rows.Close()
			if qerr != nil {
				return nil, qerr
			}
			if re != nil {
				return nil, re
			}
		}
	}
	type channelCandidate struct {
		c Channel
		p *Programme
	}
	pending := make([]channelCandidate, 0, len(audienceOK))
	var preloadKeys []core.RatingKey
	for _, c := range audienceOK {
		var p *Programme
		if c.Personal != nil && viewer != nil {
			if cur, ok := personalAiring[c.ID]; ok {
				copy := cur
				p = &copy
			}
		} else if c.Kind == "virtual" {
			s.virtualGuide.mu.RLock()
			entry := s.virtualGuide.entries[c.ID]
			s.virtualGuide.mu.RUnlock()
			if entry != nil && entry.guide != nil {
				if ps := entry.guide.slice(c.ID, now, now.Add(time.Second)); len(ps) > 0 {
					p = &ps[0]
				}
			}
		} else if a, ok := airing[c.ID]; ok {
			copy := a
			p = &copy
		}
		if p == nil && c.Kind == "virtual" && viewer != nil && (viewer.MaxContentRating != "" || policy.MaxRating != "") {
			continue
		}
		if p != nil && p.policyTag == nil && p.Item.Key != 0 {
			preloadKeys = append(preloadKeys, p.Item.Key)
		}
		pending = append(pending, channelCandidate{c: c, p: p})
	}
	if len(preloadKeys) > 0 && policyCache != nil {
		if err := policyCache.preload(ctx, preloadKeys); err != nil {
			return nil, err
		}
	}
	out := []Channel{}
	for _, cand := range pending {
		c := cand.c
		var ok bool
		if policyCache != nil {
			ok = s.allowedProgrammeWithLookup(ctx, viewer, c, cand.p, policyCache.lookup(c))
		} else {
			ok = s.allowedProgramme(ctx, viewer, c, cand.p)
		}
		if ok {
			c.StreamURL = ""
			c.redactArchiveSource()
			c.StreamSources = nil
			out = append(out, c)
		}
	}
	return out, nil
}
