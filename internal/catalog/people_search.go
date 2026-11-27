package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Catalog migration 60 upgrades person-name indexing so searching a cast or
// crew member's name (including accented Latin names typed without accents)
// hits an index on both SQLite and PostgreSQL without scanning or sorting the
// catalogue at read time. Existing rows in catalog_person_names are backfilled
// in place during the migration.
func init() {
	store.Register("catalog", 60, `SELECT 1;`)
	store.RegisterSQLiteApply("catalog", 60, func(ctx context.Context, tx *sql.Tx) error {
		for _, stmt := range []string{
			`DROP TABLE IF EXISTS catalog_person_names_fts`,
			`CREATE VIRTUAL TABLE catalog_person_names_fts USING fts5(name, content='catalog_person_names', content_rowid='person_tmdb', tokenize='unicode61 remove_diacritics 2', prefix='1 2 3', detail=none, columnsize=0)`,
			`INSERT INTO catalog_person_names_fts(catalog_person_names_fts) VALUES('rebuild')`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("catalog: rebuild person search index: %w", err)
			}
		}
		return nil
	})
	store.RegisterDialect("catalog", 60, store.Postgres, `
CREATE OR REPLACE FUNCTION catalog_fold_person_name(text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT translate(
  lower($1),
  'àáâãäåāăąçćĉċčďđèéêëēĕėęěğĝġģĥħìíîïĩīĭįıĵķĺļľŀłñńņňòóôõöøōŏőŕŗřśŝşšţťŧùúûüũūŭůűųŵýÿŷźżž',
  'aaaaaaaaacccccddeeeeeeeeegggghhiiiiiiiiiijklllllnnnnooooooooorrrsssstttuuuuuuuuuuwyyyzzz'
 )
$$;
CREATE INDEX IF NOT EXISTS catalog_person_names_folded_exact
 ON catalog_person_names ((catalog_fold_person_name(name) COLLATE "C"), credits DESC, person_tmdb);
DO $$
BEGIN
 CREATE EXTENSION IF NOT EXISTS pg_trgm;
 CREATE INDEX IF NOT EXISTS catalog_person_names_folded_trgm
  ON catalog_person_names USING gin (catalog_fold_person_name(name) gin_trgm_ops);
EXCEPTION WHEN OTHERS THEN
 RAISE NOTICE 'catalog: pg_trgm unavailable for folded person search: %', SQLERRM;
END
$$;`)
}

const (
	// peopleSearchCandidateCap bounds how many person rows one FTS or trigram
	// candidate probe reads before Go-side ranking.
	peopleSearchCandidateCap = 192
	// maxPeopleForTitles bounds how many matched people expand to credits on a
	// single search.
	maxPeopleForTitles = 4
	// primaryPersonCreditCap and secondaryPersonCreditCap bound the credit rows
	// read per matched person so even a prolific career stays well inside the
	// search row budget with zero SQL sorting.
	primaryPersonCreditCap   = 360
	secondaryPersonCreditCap = 120
)

type personMatchTier uint8

const (
	personTierNone personMatchTier = iota
	personTierPrefix
	personTierWord
	personTierNearExact
	personTierExact
)

type scoredPersonMatch struct {
	PersonMatch
	tier personMatchTier
}

func (m scoredPersonMatch) exactOrNear() bool {
	return m.tier >= personTierNearExact
}

// foldPersonQueryTerm lowercases s and strips Latin diacritics in the same way
// as catalog_fold_person_name and FTS5 unicode61 remove_diacritics 2.
func foldPersonQueryTerm(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		switch r {
		case 'ł':
			b.WriteByte('l')
		case 'ø':
			b.WriteByte('o')
		case 'đ':
			b.WriteByte('d')
		case 'ß':
			b.WriteString("ss")
		case 'æ':
			b.WriteString("ae")
		case 'œ':
			b.WriteString("oe")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func personQueryTokens(terms []string) []string {
	var out []string
	for _, term := range terms {
		folded := foldPersonQueryTerm(term)
		for _, word := range strings.FieldsFunc(folded, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.Is(unicode.Co, r)
		}) {
			if word != "" {
				out = append(out, word)
			}
		}
	}
	return out
}

// matchPeople executes bounded indexed person-name probes (exact + prefix) and
// orders the candidate set in Go by (credits DESC, person_tmdb ASC) so SQLite
// never builds a temp B-tree and PostgreSQL never sorts all trigram hits.
func (c *Catalog) matchPeople(ctx context.Context, terms []string, limit int) ([]PersonMatch, error) {
	tokens := personQueryTokens(terms)
	if len(tokens) == 0 {
		return nil, nil
	}
	fetchLimit := min(max(limit*4, 64), peopleSearchCandidateCap)
	// One search request reads the same names for its title results and
	// its person results; a wider read already made answers a narrower one.
	memo := core.SearchMemoFrom(ctx)
	key := "people\x00" + strings.Join(tokens, "\x00")
	if v, ok := memo.Load(key); ok {
		if hit := v.(personMatchesMemo); hit.fetch >= fetchLimit && hit.limit >= limit {
			return slices.Clone(hit.sorted[:min(limit, len(hit.sorted))]), nil
		}
	}
	out, err := c.readPeopleMatches(ctx, tokens, fetchLimit, limit)
	if err != nil {
		return nil, err
	}
	memo.Store(key, personMatchesMemo{fetch: fetchLimit, limit: limit, sorted: out})
	return slices.Clone(out[:min(limit, len(out))]), nil
}

// personMatchesMemo is one request's candidate read for a name, sorted by
// credits, and how wide the read was.
type personMatchesMemo struct {
	fetch, limit int
	sorted       []PersonMatch
}

// readPeopleMatches ranks the union of bounded index probes. Only the
// requested number of distinct people crosses the driver boundary.
func (c *Catalog) readPeopleMatches(ctx context.Context, tokens []string, fetchLimit, limit int) ([]PersonMatch, error) {
	var probes []string
	var args []any
	add := func(q string, bound ...any) {
		probes = append(probes, fmt.Sprintf("SELECT person_tmdb,name,credits,%d AS probe FROM (%s) AS matches", len(probes), q))
		args = append(args, bound...)
	}
	joinedName := strings.Join(tokens, " ")
	prefix := ""
	if store.DialectOf(c.db) == store.Postgres {
		add(`SELECT person_tmdb,name,credits FROM catalog_person_names
			WHERE (catalog_fold_person_name(name) COLLATE "C") = (catalog_fold_person_name(?) COLLATE "C") AND credits>0
			ORDER BY credits DESC,person_tmdb LIMIT ?`, joinedName, min(fetchLimit, 64))
	} else {
		prefix = store.SQLiteOnly
		add(`SELECT person_tmdb,name,credits FROM catalog_person_names WHERE LOWER(name)=? AND credits>0 LIMIT ?`, joinedName, 16)
		exactParts := make([]string, len(tokens))
		for i, token := range tokens {
			exactParts[i] = `"` + token + `"`
		}
		add(`SELECT p.person_tmdb,p.name,p.credits FROM catalog_person_names_fts f JOIN catalog_person_names p ON p.person_tmdb=f.rowid
			WHERE catalog_person_names_fts MATCH ? AND p.credits>0 LIMIT ?`,
			strings.Join(exactParts, " "), min(fetchLimit, 64))
	}
	query, bound := c.matchPeopleSQL(tokens, fetchLimit)
	add(query, bound...)
	args = append(args, limit)
	rows, err := c.db.QueryContext(ctx, prefix+`WITH probes AS (`+strings.Join(probes, " UNION ALL ")+`),
		ranked AS (SELECT person_tmdb,name,credits,ROW_NUMBER() OVER (PARTITION BY person_tmdb ORDER BY credits DESC,probe) AS position FROM probes)
		SELECT person_tmdb,name,credits FROM ranked WHERE position=1 ORDER BY credits DESC,person_tmdb LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PersonMatch, 0, limit)
	for rows.Next() {
		var person PersonMatch
		if err := rows.Scan(&person.TMDB, &person.Name, &person.Credits); err != nil {
			return nil, err
		}
		out = append(out, person)
	}
	return out, rows.Err()
}

// hasExactPersonMatch reports whether sq names a credited person by exact or
// near-exact full name, allowing searchTitlesOpts to answer immediately
// without waiting on TMDB's live title search.
func (c *Catalog) hasExactPersonMatch(ctx context.Context, sq searchQuery) bool {
	if len(sq.words) == 0 {
		return false
	}
	matches, err := c.peopleSearchMatches(ctx, sq, true)
	if err == nil && len(matches) > 0 {
		return true
	}
	if sq.yearless != nil {
		matches, err = c.peopleSearchMatches(ctx, *sq.yearless, true)
		return err == nil && len(matches) > 0
	}
	return false
}

func (c *Catalog) peopleSearchMatches(ctx context.Context, sq searchQuery, exactOnly bool) ([]scoredPersonMatch, error) {
	if len(sq.words) == 0 {
		return nil, nil
	}
	// Require at least 3 letters across the query before expanding person
	// credits into title search results.
	if len(sq.runes) < 3 {
		return nil, nil
	}
	cands, err := c.matchPeople(ctx, sq.words, 64)
	if err != nil {
		return nil, err
	}
	scored := scorePersonCandidates(sq, cands, exactOnly)
	hasStrong := slices.ContainsFunc(scored, scoredPersonMatch.exactOrNear)
	// When a 2+ word full-name query had a 1-letter typo (e.g. "quentin
	// tarentino" or "brad pittt"), probe 3-character prefixes of the first and
	// last words so edit-distance-1 full names still resolve via the prefix
	// index with a single bounded query.
	if !hasStrong && len(sq.words) >= 2 {
		first, last := []rune(sq.words[0]), []rune(sq.words[len(sq.words)-1])
		if len(first) >= 3 && len(last) >= 3 {
			shortTerms := []string{string(first[:3]), string(last[:3])}
			more, merr := c.matchPeople(ctx, shortTerms, 64)
			if merr == nil && len(more) > 0 {
				seen := make(map[int64]bool, len(cands)+len(more))
				for _, m := range cands {
					seen[m.TMDB] = true
				}
				for _, m := range more {
					if !seen[m.TMDB] {
						cands = append(cands, m)
					}
				}
				scored = scorePersonCandidates(sq, cands, exactOnly)
			}
		}
	}
	return scored, nil
}

func scorePersonCandidates(sq searchQuery, cands []PersonMatch, exactOnly bool) []scoredPersonMatch {
	var scratch []int
	out := make([]scoredPersonMatch, 0, len(cands))
	for _, m := range cands {
		tier := scorePersonName(sq, m.Name, &scratch)
		if tier == personTierNone || (exactOnly && tier < personTierNearExact) {
			continue
		}
		out = append(out, scoredPersonMatch{PersonMatch: m, tier: tier})
	}
	slices.SortFunc(out, func(a, b scoredPersonMatch) int {
		if c := cmp.Compare(b.tier, a.tier); c != 0 {
			return c
		}
		if c := cmp.Compare(b.Credits, a.Credits); c != 0 {
			return c
		}
		return cmp.Compare(a.TMDB, b.TMDB)
	})
	return out
}

func isPersonSuffix(w string) bool {
	switch w {
	case "jr", "sr", "ii", "iii", "iv":
		return true
	}
	return false
}

func stripPersonAffixes(words []string) []string {
	if len(words) <= 2 {
		if len(words) == 2 && isPersonSuffix(words[1]) {
			return words[:1]
		}
		return words
	}
	out := make([]string, 0, len(words))
	for i, w := range words {
		if i == len(words)-1 && isPersonSuffix(w) {
			continue
		}
		// Drop single-letter middle initials ("Samuel L. Jackson" -> "samuel jackson").
		if i > 0 && i < len(words)-1 && utf8.RuneCountInString(w) == 1 {
			continue
		}
		out = append(out, w)
	}
	return out
}

// scorePersonName classifies how closely sq matches a credited person's name.
func scorePersonName(sq searchQuery, name string, scratch *[]int) personMatchTier {
	if len(sq.words) == 0 || name == "" {
		return personTierNone
	}
	variants := [][]string{searchWords(name)}
	if split := personQueryTokens([]string{name}); len(split) > 0 && !slices.Equal(split, variants[0]) {
		variants = append(variants, split)
	}
	best := personTierNone
	for _, pWords := range variants {
		if len(pWords) == 0 {
			continue
		}
		pJoin := strings.Join(pWords, "")
		// 1. Exact full name ("brad pitt", "stellan skarsgard", "zendaya", "jk simmons").
		if slices.Equal(pWords, sq.words) || (pJoin == sq.join && len(sq.runes) >= 4) {
			return personTierExact
		}
		// 2. Near-exact full name:
		//    - Dropped suffix or middle initial ("robert downey" -> "Robert Downey Jr.",
		//      "samuel jackson" -> "Samuel L. Jackson")
		//    - Multi-word full name with last word nearly complete ("brad pit" -> "Brad Pitt")
		//    - 1-edit typo on a multi-word full name ("brad pittt", "quentin tarentino")
		if len(sq.words) >= 2 {
			stripped := stripPersonAffixes(pWords)
			if len(stripped) >= 2 && slices.Equal(stripped, sq.words) {
				best = max(best, personTierNearExact)
				continue
			}
			if len(sq.words) == len(pWords) {
				allLead := true
				for i := 0; i < len(sq.words)-1; i++ {
					if sq.words[i] != pWords[i] {
						allLead = false
						break
					}
				}
				lastQ, lastP := sq.words[len(sq.words)-1], pWords[len(pWords)-1]
				if allLead && strings.HasPrefix(lastP, lastQ) && len(lastQ) >= max(3, len(lastP)-2) {
					best = max(best, personTierNearExact)
					continue
				}
			}
			if len(sq.runes) >= 6 {
				pr := []rune(pJoin)
				if e := editDistance(sq.runes, pr, 1, scratch); e == 1 {
					best = max(best, personTierNearExact)
					continue
				}
			}
		}
		// 3. Partial whole-word match ("tarantino" -> "Quentin Tarantino").
		if allWordsMatch(sq.words, pWords, false) {
			best = max(best, personTierWord)
			continue
		}
		// 4. Partial word-prefix match ("tarant" -> "Quentin Tarantino").
		if allWordsMatch(sq.words, pWords, true) {
			best = max(best, personTierPrefix)
		}
	}
	return best
}

func allWordsMatch(qWords, pWords []string, allowPrefix bool) bool {
	if len(qWords) == 0 || len(qWords) > len(pWords) || len(pWords) > 32 {
		return false
	}
	var used uint32
	for _, qw := range qWords {
		found := false
		for j, pw := range pWords {
			if used&(1<<j) != 0 {
				continue
			}
			if pw == qw || (allowPrefix && strings.HasPrefix(pw, qw)) {
				used |= 1 << j
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// creditRoleWeight weights a credit by billing importance so starring roles
// and directed features rank ahead of cameos, talk-show "Self" appearances,
// and minor crew credits.
func creditRoleWeight(kind string, ord int64, role string) float64 {
	switch kind {
	case "director":
		return 1.00
	case "cast":
		if isMinorSelfOrCameoRole(role) {
			return 0.15
		}
		switch {
		case ord <= 2:
			return 1.00 - float64(max(ord, int64(0)))*0.03
		case ord <= 7:
			return 0.82 - float64(ord-3)*0.04
		case ord <= 15:
			return 0.48
		default:
			return 0.20
		}
	case "writer":
		return 0.86
	case "crew":
		return 0.70
	default:
		return 0.40
	}
}

func isMinorSelfOrCameoRole(role string) bool {
	if role == "" {
		return false
	}
	r := strings.ToLower(strings.TrimSpace(role))
	if r == "self" || r == "himself" || r == "herself" || r == "themselves" {
		return true
	}
	for _, prefix := range []string{"self -", "self (", "himself -", "himself (", "herself -", "herself ("} {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return strings.Contains(r, "archive footage") || strings.Contains(r, "uncredited")
}

// PeopleTitles returns up to limit catalogue titles credited to people whose
// names match query, applying catalogue visibility and viewer context rules.
// Exact and near-exact full-name matches score right below exact title matches
// (0.935..0.956); partial surname/prefix matches score below title matches
// (< 0.50) so title search is never pushed down.
func (c *Catalog) PeopleTitles(ctx context.Context, viewer *core.User, query string, limit int) ([]core.Title, error) {
	query = strings.TrimSpace(query)
	if query == "" || limit <= 0 {
		return nil, nil
	}
	if viewer != nil && core.CatalogViewerID(ctx) == 0 && !core.CatalogViewerRestricted(ctx) {
		ctx = core.WithCatalogViewer(ctx, viewer)
	}
	types := core.SearchTypesFrom(ctx)
	if len(types) == 0 {
		types = []core.MediaType{core.TypeMovie, core.TypeShow}
	}
	var creditTypes []core.MediaType
	for _, t := range types {
		switch t {
		case core.TypeMovie, core.TypeShow, core.TypeEpisode:
			if !slices.Contains(creditTypes, t) {
				creditTypes = append(creditTypes, t)
			}
		}
	}
	if len(creditTypes) == 0 {
		return nil, nil
	}

	sq := parseSearchQuery(query)
	exactOnly := core.ExactPeopleOnly(ctx)
	yearFilter := 0
	matches, err := c.peopleSearchMatches(ctx, sq, exactOnly)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 && sq.yearless != nil {
		matches, err = c.peopleSearchMatches(ctx, *sq.yearless, exactOnly)
		if err != nil {
			return nil, err
		}
		yearFilter = sq.year
	}
	if len(matches) == 0 {
		return nil, nil
	}
	// Matches are sorted strongest first. A name typed in full expands up to
	// maxPeopleForTitles people sharing it; a partial name ("star" inside
	// "Starr") only adds its best match's titles under the title results,
	// so a short word never reads several whole filmographies.
	if !matches[0].exactOrNear() {
		matches = matches[:1]
	} else if n := slices.IndexFunc(matches, func(m scoredPersonMatch) bool { return !m.exactOrNear() }); n > 0 {
		matches = matches[:n]
	}
	if len(matches) > maxPeopleForTitles {
		matches = matches[:maxPeopleForTitles]
	}

	personRankByID := make(map[int64]int, len(matches))
	personMatchByID := make(map[int64]scoredPersonMatch, len(matches))
	typePlaceholders := make([]string, len(creditTypes))
	typeArgs := make([]any, len(creditTypes))
	for i, mt := range creditTypes {
		typePlaceholders[i] = "?"
		typeArgs[i] = string(mt)
	}
	inTypes := strings.Join(typePlaceholders, ",")
	cols := qualifiedTitleCols("t")
	gateWhere := ""
	var gateArgs []any
	if gc, ga := c.searchGateConds(ctx, "t"); gc != "" {
		gateWhere = " AND " + gc
		gateArgs = ga
	}

	// Read each matched person's credited titles in a single UNION ALL
	// statement without SQL ORDER BY (zero temp B-tree / Sort): each arm seeks
	// catalog_credits_person -> catalog_titles PK.
	var parts []string
	var args []any
	for i, m := range matches {
		personRankByID[m.TMDB] = i
		personMatchByID[m.TMDB] = m
		capRows := secondaryPersonCreditCap
		if i == 0 && m.exactOrNear() {
			capRows = primaryPersonCreditCap
		}
		parts = append(parts, `SELECT * FROM (SELECT `+cols+`, cr.person_tmdb, cr.kind, cr.ord, cr.role
			FROM catalog_credits cr
			JOIN catalog_titles t ON t.key=cr.key
			WHERE cr.person_tmdb=? AND t.adult=0 AND t.type IN (`+inTypes+`)`+gateWhere+`
			LIMIT ?) AS person_`+strconv.Itoa(i))
		args = append(args, m.TMDB)
		args = append(args, typeArgs...)
		args = append(args, gateArgs...)
		args = append(args, capRows)
	}

	rows, err := c.db.QueryContext(ctx, strings.Join(parts, "\nUNION ALL\n"), args...)
	if err != nil {
		return nil, err
	}
	type candidateEntry struct {
		title      core.Title
		tier       personMatchTier
		personRank int
		leadRank   float64
	}
	byKey := make(map[core.RatingKey]*candidateEntry, 64)
	now := c.now()
	for rows.Next() {
		var personID, ord int64
		var kind, role string
		dest := []any{&personID, &kind, &ord, &role}
		st, err := scanTitle(extraScan{rows, dest})
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		t := st.t
		if t.Adult || (!t.IsReleased(now) && !c.upcomingMovie(&t)) || t.Key == 0 {
			continue
		}
		pm, ok := personMatchByID[personID]
		if !ok {
			continue
		}
		pr := personRankByID[personID]
		w := creditRoleWeight(kind, ord, role)
		// Combine billing prominence with popularity so iconic lead and
		// directed works surface ahead of cameos or minor credits.
		popNorm := popularityPrior(t.Popularity, t.VoteCount) / 0.10
		leadRank := w * (0.30 + 0.70*popNorm)

		prev, exists := byKey[t.Key]
		if !exists {
			byKey[t.Key] = &candidateEntry{
				title:      t,
				tier:       pm.tier,
				personRank: pr,
				leadRank:   leadRank,
			}
			continue
		}
		if pm.tier > prev.tier || (pm.tier == prev.tier && (pr < prev.personRank || (pr == prev.personRank && leadRank > prev.leadRank))) {
			prev.tier = pm.tier
			prev.personRank = pr
			prev.leadRank = leadRank
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if len(byKey) == 0 {
		return nil, nil
	}

	// Hydrate visibility-relevant facts on movie/show candidates before
	// checking viewer/library visibility rules.
	cand := core.SearchCandidatesFrom(ctx)
	if cand == nil && core.SearchSummaryFacts(ctx) {
		cand = &core.SearchCandidates{Facts: core.VisibilityFacts}
	}
	windowFacts := c.policyFacts(c.visibilityFor(ctx))
	entries := make([]*candidateEntry, 0, len(byKey))
	var factRows []core.Title
	var factIdx []int
	for _, e := range byKey {
		idx := len(entries)
		entries = append(entries, e)
		if windowFacts != 0 && (e.title.Type == core.TypeMovie || e.title.Type == core.TypeShow) {
			factRows = append(factRows, e.title)
			factIdx = append(factIdx, idx)
		}
	}
	if len(factRows) > 0 {
		if err := c.hydrateFacts(ctx, factRows, windowFacts); err != nil {
			return nil, err
		}
		for j, idx := range factIdx {
			entries[idx].title = factRows[j]
		}
	}

	visFilter := c.visibilityFor(ctx)
	visible := entries[:0]
	for _, e := range entries {
		if c.visibleFor(ctx, visFilter, &e.title) {
			visible = append(visible, e)
		}
	}
	entries = visible
	if len(entries) == 0 {
		return nil, nil
	}

	if yearFilter != 0 {
		var yrMatched []*candidateEntry
		for _, e := range entries {
			if e.title.Year == yearFilter {
				yrMatched = append(yrMatched, e)
			}
		}
		if len(yrMatched) > 0 {
			entries = yrMatched
		}
	}

	// Optional viewer affinity tie-break kept strictly inside the person tier
	// band so it never crosses above exact title matches or pushes down titles.
	var affinity map[core.RatingKey]int
	if fn := core.SearchAffinity(ctx); fn != nil {
		akeys := make([]core.RatingKey, 0, len(entries))
		for _, e := range entries {
			if e.title.Type == core.TypeMovie || e.title.Type == core.TypeShow {
				akeys = append(akeys, e.title.Key)
			}
		}
		affinity = fn(ctx, akeys)
	}

	for _, e := range entries {
		normLead := min(max(e.leadRank, 0), 1.0)
		var score float64
		switch e.tier {
		case personTierExact:
			// Right after exact/article title matches (TitleMatchScore = 0.96).
			score = 0.946 + 0.009*normLead - float64(e.personRank)*0.0015
		case personTierNearExact:
			score = 0.935 + 0.006*normLead - float64(e.personRank)*0.0010
		case personTierWord:
			// Below any title match (>= 0.50) so title search is never pushed down.
			score = 0.420 + 0.065*normLead - float64(e.personRank)*0.0050
		default:
			score = 0.320 + 0.065*normLead - float64(e.personRank)*0.0050
		}
		switch affinity[e.title.Key] {
		case core.AffinityViewer:
			score += 0.0006
		case core.AffinityServer:
			score += 0.0003
		}
		if e.tier >= personTierNearExact {
			score = min(score, 0.9558)
		} else {
			score = min(score, 0.490)
		}
		if !c.inLibrary(e.title.Key) {
			score = outsideScore(score)
		}
		e.title.SearchScore = score
	}

	slices.SortFunc(entries, func(a, b *candidateEntry) int {
		if c := cmp.Compare(b.title.SearchScore, a.title.SearchScore); c != 0 {
			return c
		}
		if c := cmp.Compare(b.title.Popularity, a.title.Popularity); c != 0 {
			return c
		}
		return cmp.Compare(a.title.Key, b.title.Key)
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}

	items := make([]core.Title, len(entries))
	partial := make(map[core.RatingKey]bool, len(entries))
	for i, e := range entries {
		items[i] = e.title
		if e.title.Type == core.TypeMovie || e.title.Type == core.TypeShow {
			partial[e.title.Key] = true
		}
	}

	if cand != nil {
		if err := c.finishCandidates(ctx, cand, items, partial, windowFacts); err != nil {
			return nil, err
		}
	} else {
		var keys []core.RatingKey
		var epIdx []int
		var eps []core.Title
		for i, t := range items {
			switch t.Type {
			case core.TypeMovie, core.TypeShow:
				keys = append(keys, t.Key)
			case core.TypeEpisode:
				epIdx = append(epIdx, i)
				eps = append(eps, t)
			}
		}
		if len(keys) > 0 {
			if core.SearchListFacts(ctx) {
				loaded, err := c.searchListRows(ctx, keys)
				if err != nil {
					return nil, err
				}
				for i, t := range items {
					if full, ok := loaded[t.Key]; ok {
						full.SearchScore = t.SearchScore
						items[i] = full
					}
				}
			} else {
				c.PreloadStored(ctx, keys)
				for i, t := range items {
					if t.Type != core.TypeMovie && t.Type != core.TypeShow {
						continue
					}
					if st, err := c.loadByKey(ctx, t.Key); err == nil {
						full := st.t
						full.SearchScore = t.SearchScore
						items[i] = full
					} else if !errors.Is(err, core.ErrNotFound) {
						return nil, err
					}
				}
			}
		}
		if len(eps) > 0 {
			if err := c.hydrateSlice(ctx, eps); err != nil {
				return nil, err
			}
			for j, idx := range epIdx {
				eps[j].SearchScore = items[idx].SearchScore
				items[idx] = eps[j]
			}
		}
	}
	return items, nil
}
