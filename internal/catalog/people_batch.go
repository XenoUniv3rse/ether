package catalog

import (
	"context"
	"encoding/json"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// A people search checks, for each person whose name matches, that the
// viewer can see at least one title crediting them. Per person that was a
// credit query, a full title read (every credit of every title) and a
// re-read of the portrait title: a statement set per matching person,
// growing with the library. These read a page of people in a few
// statements, and only the facts a visibility decision needs.

// peopleBatch bounds the people one credits statement covers.
const peopleBatch = 25

// PersonCreditsMany is PersonCreditKeys for several people at once: each
// person's most popular credited titles of the given types (at most limit
// each), with the person's credit rows on them, most popular first.
func (c *Catalog) PersonCreditsMany(ctx context.Context, persons []int64, types []core.MediaType, limit int) (map[int64][]core.PersonCredit, error) {
	out, _, err := c.personCredits(ctx, persons, types, limit, false)
	return out, err
}

// PersonCreditTitles is PersonCreditsMany with the credited titles'
// visibility rows (CachedVisibilityRows, with facts) read in the same
// statements: a people search's credits and the rows it judges them by.
func (c *Catalog) PersonCreditTitles(ctx context.Context, persons []int64, types []core.MediaType, limit int, facts core.TitleFacts) (map[int64][]core.PersonCredit, map[core.RatingKey]core.Title, error) {
	credits, titles, err := c.personCredits(ctx, persons, types, limit, true)
	if err != nil {
		return nil, nil, err
	}
	items := make([]core.Title, 0, len(titles))
	for _, t := range titles {
		items = append(items, t)
	}
	if err := c.hydrateFacts(ctx, items, facts|c.viewerPolicyFacts(ctx)|c.policyFacts(c.visibilityFor(ctx))); err != nil {
		return nil, nil, err
	}
	rows := make(map[core.RatingKey]core.Title, len(items))
	for i := range items {
		if items[i].Type != core.TypeSeason && c.cachedListVisible(&items[i]) {
			rows[items[i].Key] = items[i]
		}
	}
	return credits, rows, nil
}

// personCredits reads persons' credits in one statement per batch, a row
// per credit; withTitles also reads each credited title's row.
func (c *Catalog) personCredits(ctx context.Context, persons []int64, types []core.MediaType, limit int, withTitles bool) (map[int64][]core.PersonCredit, map[core.RatingKey]core.Title, error) {
	out := map[int64][]core.PersonCredit{}
	titles := map[core.RatingKey]core.Title{}
	if len(persons) == 0 || len(types) == 0 || limit <= 0 {
		return out, titles, nil
	}
	cols := ""
	if withTitles {
		cols = qualifiedTitleCols("t") + ", "
	}
	for part := range store.Chunks(persons, peopleBatch) {
		// One statement: each person's most popular credited titles with
		// that person's credit rows on them, a row per credit.
		var parts []string
		var args []any
		for _, p := range part {
			if p <= 0 {
				continue
			}
			q, a := personCreditKeysSQL(p, types, limit)
			join := ""
			if withTitles {
				join = ` JOIN catalog_titles t ON t.key=k.key`
			}
			parts = append(parts, `SELECT `+cols+`CAST(? AS BIGINT) AS person, k.key AS credit_key, k.popularity AS popularity, cr.kind AS kind, cr.name AS name, cr.role AS role, cr.thumb AS thumb, cr.provider_id AS provider_id, COALESCE(pp.imdb,'') AS imdb, cr.ord AS ord
		FROM (`+q+`) AS k JOIN catalog_credits cr ON cr.person_tmdb=? AND cr.key=k.key LEFT JOIN catalog_people pp ON pp.person_tmdb=cr.person_tmdb`+join)
			args = append(append(append(args, p), a...), p)
		}
		if len(parts) == 0 {
			continue
		}
		rows, err := c.db.QueryContext(ctx, strings.Join(parts, "\nUNION ALL\n")+"\nORDER BY person, popularity DESC, credit_key, kind, ord", args...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var person, key, ord int64
			var pop float64
			var cr core.PersonCredit
			dest := []any{&person, &key, &pop, &cr.Kind, &cr.Person.Name, &cr.Person.Role, &cr.Person.ThumbPath, &cr.Person.ID, &cr.Person.IMDB, &ord}
			if withTitles {
				st, err := scanTitle(extraScan{rows, dest})
				if err != nil {
					rows.Close()
					return nil, nil, err
				}
				titles[st.t.Key] = st.t
				cr.Browsable = st.browsable
			} else if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return nil, nil, err
			}
			cr.Key, cr.Person.TMDB = core.RatingKey(key), person
			out[person] = append(out[person], cr)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	return out, titles, nil
}

// qualifiedTitleCols is titleCols read through alias.
func qualifiedTitleCols(alias string) string {
	cols := strings.Split(titleCols, ",")
	for i := range cols {
		cols[i] = alias + "." + strings.TrimSpace(cols[i])
	}
	return strings.Join(cols, ",")
}

type personKey struct {
	person int64
	key    core.RatingKey
}

// CachedVisibleTitles is CachedCreditTitles for a visibility decision only:
// the stored rows, plus the facts visibility rules and the release gate
// read when they read any, without credits or alternative titles.
func (c *Catalog) CachedVisibleTitles(ctx context.Context, keys []core.RatingKey, cfg config.Catalog, now time.Time) (map[core.RatingKey]core.Title, error) {
	return c.cachedPeopleVisibility(ctx, keys, cfg, now, false)
}

// CachedPeopleParents reads only the facts used for inherited credit policy.
func (c *Catalog) CachedPeopleParents(ctx context.Context, keys []core.RatingKey, cfg config.Catalog, now time.Time) (map[core.RatingKey]core.Title, error) {
	return c.cachedPeopleVisibility(ctx, keys, cfg, now, true)
}

func (c *Catalog) cachedPeopleVisibility(ctx context.Context, keys []core.RatingKey, cfg config.Catalog, now time.Time, parentPolicy bool) (map[core.RatingKey]core.Title, error) {
	filter := c.visibilityFilter()
	digital := cfg.ReleaseMode == config.ModeDigital
	var facts core.TitleFacts
	if parentPolicy || !filter.Passive() || digital {
		facts = core.VisibilityFacts
	}
	out := make(map[core.RatingKey]core.Title, len(keys))
	for part := range store.Chunks(keys, 200) {
		args := make([]any, len(part))
		for i, k := range part {
			args[i] = int64(k)
		}
		items, err := c.readVisibilityRows(ctx, args, facts)
		if err != nil {
			return nil, err
		}
		var seasons []*core.Title
		for i := range items {
			if items[i].Type == core.TypeSeason {
				seasons = append(seasons, &items[i])
			}
		}
		if len(seasons) > 0 {
			if err := applyAiredCounts(ctx, c.db, now, seasons, c.WriteGeneration()); err != nil {
				return nil, err
			}
		}
		for i := range items {
			t := &items[i]
			if parentPolicy && !c.cachedListVisible(t) {
				continue
			}
			upcoming := cfg.ShowUpcomingMovies && t.Type == core.TypeMovie && !t.Released.IsZero() && t.Released.After(now)
			if t.Adult || (!upcoming && !t.IsReleased(now)) || (!c.isTitleOrShowOwnerIncluded(t) && filter.Hidden(t)) || (!upcoming && digital && !homeReleaseVisible(t, now, cfg.HomeReleaseOffsetDays)) || (t.Type == core.TypeSeason && !listableSeason(t, now)) {
				continue
			}
			out[t.Key] = *t
		}
	}
	return out, nil
}

// CachedVisibilityRows is CachedGetMany for a visibility decision: stored
// rows that pass the list gates, with the facts the catalogue's policy for
// ctx's viewer reads (viewerPolicyFacts) and facts, the caller's own, and
// without credits. The partial titles are not cached.
func (c *Catalog) CachedVisibilityRows(ctx context.Context, keys []core.RatingKey, facts core.TitleFacts) (map[core.RatingKey]core.Title, error) {
	return c.cachedVisibilityRows(ctx, keys, facts, false)
}

// CachedPlaylistVisibilityRows projects child episode release and inherited
// policy fields too, for playlists that only render membership counts.
func (c *Catalog) CachedPlaylistVisibilityRows(ctx context.Context, keys []core.RatingKey, facts core.TitleFacts) (map[core.RatingKey]core.Title, error) {
	return c.cachedVisibilityRows(ctx, keys, facts, true)
}

func (c *Catalog) cachedVisibilityRows(ctx context.Context, keys []core.RatingKey, facts core.TitleFacts, children bool) (map[core.RatingKey]core.Title, error) {
	out := make(map[core.RatingKey]core.Title, len(keys))
	// A full snapshot already has every requested fact. Reuse it exactly as
	// CachedGetMany does, so repeated policy/facet checks on a listing do
	// not replace cache hits with fresh SQL reads. Partial rows stay uncached.
	gen, minute := c.writeGen.Load(), c.minuteBucket()
	var missing []core.RatingKey
	for _, key := range keys {
		if title, ok := c.cachedStored(key, gen, minute); ok {
			if children && c.canProjectAlternateOrders() && (title.Type == core.TypeEpisode || title.Type == core.TypeSeason) {
				missing = append(missing, key)
				continue
			}
			if (children || title.Type != core.TypeSeason) && c.cachedListVisible(&title) {
				out[key] = title
			}
			continue
		}
		missing = append(missing, key)
	}
	facts |= c.viewerPolicyFacts(ctx) | c.policyFacts(c.visibilityFor(ctx))
	var items []core.Title
	for part := range store.Chunks(missing, 200) {
		args := make([]any, len(part))
		for i, k := range part {
			args[i] = int64(k)
		}
		batch, err := c.readVisibilityRows(ctx, args, facts)
		if err != nil {
			return nil, err
		}
		items = append(items, batch...)
	}
	if children {
		for _, item := range items {
			if item.Type == core.TypeSeason {
				if err := c.airedCounts(ctx, items); err != nil {
					return nil, err
				}
				break
			}
		}
		if err := c.hydrateShowPolicy(ctx, items); err != nil {
			return nil, err
		}
		if err := c.ProjectEpisodeTitles(ctx, items); err != nil {
			return nil, err
		}
	}
	for i := range items {
		if (children || items[i].Type != core.TypeSeason) && c.cachedListVisible(&items[i]) {
			out[items[i].Key] = items[i]
		}
	}
	return out, nil
}

// CachedPolicyRows is a batched, local-only counterpart to CachedGet for policy
// decisions (such as Live TV search). It reuses cached full titles when present
// and batch-hydrates missing rows with only visibility facts and parent-show
// policy inheritance—never trailers, credits, or alternative titles—and never
// populates the full-title cache with partial rows.
func (c *Catalog) CachedPolicyRows(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
	out := make(map[core.RatingKey]core.Title, len(keys))
	gen, minute := c.writeGen.Load(), c.minuteBucket()
	seen := make(map[core.RatingKey]bool, len(keys))
	var missing []core.RatingKey
	for _, key := range keys {
		if key == 0 || seen[key] {
			continue
		}
		seen[key] = true
		if title, ok := c.cachedStored(key, gen, minute); ok {
			if c.visibleTitle(&title) {
				out[key] = title
			}
			continue
		}
		missing = append(missing, key)
	}
	for part := range store.Chunks(missing, 200) {
		args := make([]any, len(part))
		for i, k := range part {
			args[i] = int64(k)
		}
		rows, err := c.db.QueryContext(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE key IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		var items []core.Title
		hasSeasons := false
		for rows.Next() {
			st, err := scanTitle(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			if st.t.Type == core.TypeSeason {
				hasSeasons = true
			}
			items = append(items, st.t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			continue
		}
		if hasSeasons {
			if err := c.airedCounts(ctx, items); err != nil {
				return nil, err
			}
		}
		if err := c.hydrateFacts(ctx, items, core.VisibilityFacts); err != nil {
			return nil, err
		}
		if err := c.hydrateShowPolicy(ctx, items); err != nil {
			return nil, err
		}
		for i := range items {
			if c.visibleTitle(&items[i]) {
				out[items[i].Key] = items[i]
			}
		}
	}
	return out, nil
}

// hydrateShowPolicy applies only the parent-show fields read by visibility and
// content-rating policy checks (title, ratings, certifications, keywords) for
// seasons and episodes, without reading alternative titles or sibling seasons.
func (c *Catalog) hydrateShowPolicy(ctx context.Context, items []core.Title) error {
	type showPolicy struct {
		title, contentRating, enforcementRating string
		certifications                          map[string]string
		keywords                                []string
	}
	shows := map[core.RatingKey]*showPolicy{}
	var showArgs []any
	for i := range items {
		if items[i].Type != core.TypeSeason && items[i].Type != core.TypeEpisode {
			continue
		}
		if k := items[i].ShowKey; k != 0 && shows[k] == nil {
			shows[k] = &showPolicy{}
			showArgs = append(showArgs, int64(k))
		}
	}
	if len(shows) == 0 {
		return nil
	}
	for chunk := range store.Chunks(showArgs, 200) {
		query := `SELECT t.key,t.title,t.content_rating,t.enforcement_rating,t.certifications,` +
			textFactList(c.db, "catalog_keywords", "keyword", "t.key") +
			` FROM catalog_titles t WHERE t.key IN (` + placeholders(len(chunk)) + `)`
		rows, err := c.db.QueryContext(ctx, query, chunk...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var key core.RatingKey
			var show showPolicy
			var certificates, keywords string
			if err := rows.Scan(&key, &show.title, &show.contentRating, &show.enforcementRating, &certificates, &keywords); err != nil {
				rows.Close()
				return err
			}
			show.certifications = decodeCertifications(certificates)
			if keywords != "[]" {
				if err := json.Unmarshal([]byte(keywords), &show.keywords); err != nil {
					rows.Close()
					return err
				}
				sort.Strings(show.keywords)
			}
			shows[key] = &show
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	for i := range items {
		if items[i].Type != core.TypeSeason && items[i].Type != core.TypeEpisode {
			continue
		}
		show := shows[items[i].ShowKey]
		if show == nil {
			continue
		}
		if len(items[i].Keywords) == 0 && len(show.keywords) > 0 {
			items[i].Keywords = uniqueStrings(append([]string(nil), show.keywords...))
		}
		if items[i].ShowTitle == "" && show.title != "" {
			items[i].ShowTitle = show.title
		}
		if items[i].ContentRating == "" && show.contentRating != "" {
			items[i].ContentRating = show.contentRating
		}
		if len(items[i].ContentRatings) == 0 && len(show.certifications) > 0 {
			items[i].ContentRatings = maps.Clone(show.certifications)
		}
		if items[i].EnforcementRating == "" {
			if enf := contentrating.EffectiveEnforcement(show.enforcementRating, items[i].ContentRating, items[i].Adult, show.keywords); enf != "" {
				items[i].EnforcementRating = enf
			}
		}
	}
	return nil
}
