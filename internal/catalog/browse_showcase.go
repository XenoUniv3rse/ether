package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// PopularGenreCandidates selects the same popularity window as Browse for
// each genre. It returns only the titles accepted by the caller and hydrates
// an overlapping title once across the windows. Returning supported=false
// leaves the caller on Browse when the serving rank is unavailable.
func (c *Catalog) PopularGenreCandidates(ctx context.Context, genres []string, maxRating string, limit int, accept func(core.Title) bool) (byGenre map[string][]core.Title, supported bool, err error) {
	if !c.rankReady.Load() {
		return nil, false, nil
	}
	cfg, now := c.config(), c.now().Unix()
	var windows []string
	var args []any
	for i, genre := range genres {
		q := core.BrowseQuery{
			Filter: core.Filter{Genres: []string{genre}, MaxContentRating: maxRating},
			Sort:   core.SortPopularity, Desc: true, NoTotal: true, Limit: limit,
		}
		q.Filter = c.viewerPaging(ctx, q.Filter)
		where, bind, err := c.browseWhereTier(q.Filter, false, browseAllTiers, cfg, now)
		if err != nil {
			return nil, true, err
		}
		var portions []string
		for _, typ := range []core.MediaType{core.TypeMovie, core.TypeShow} {
			keys, _, ok := c.rankKeysQuery(ctx, q, where, typ)
			if !ok {
				return nil, false, nil
			}
			portions = append(portions, "SELECT page_key, page_rk FROM ("+keys+") AS type_page")
			args = append(args, bind...)
			args = append(args, limit, 0)
		}
		windows = append(windows, fmt.Sprintf(
			"SELECT %d AS genre_index, page_key, page_rk FROM (%s ORDER BY page_rk, page_key LIMIT ?) AS genre_page",
			i, strings.Join(portions, " UNION ALL ")))
		args = append(args, limit)
	}
	if len(windows) == 0 {
		return nil, true, nil
	}
	// Select scalar rating facts before reading the rest of each title.
	// Preserve the popularity window before applying the acclaim filter.
	rows, err := c.db.QueryContext(ctx, `SELECT p.genre_index, t.key, t.type, t.rating, t.vote_count
		FROM (`+strings.Join(windows, " UNION ALL ")+`) AS p
		JOIN catalog_titles t ON t.key=p.page_key ORDER BY p.genre_index, p.page_rk, p.page_key`, args...)
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	windowKeys := make([][]core.RatingKey, len(genres))
	var keys []core.RatingKey
	seen := map[core.RatingKey]bool{}
	for rows.Next() {
		var index int
		var title core.Title
		if err := rows.Scan(&index, &title.Key, &title.Type, &title.Rating, &title.VoteCount); err != nil {
			return nil, true, err
		}
		if accept(title) {
			windowKeys[index] = append(windowKeys[index], title.Key)
			if !seen[title.Key] {
				keys = append(keys, title.Key)
				seen[title.Key] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, true, err
	}
	rows.Close()
	titles, err := c.titlesInKeyOrder(ctx, keys)
	if err != nil {
		return nil, true, err
	}
	c.kickMovieYears(titles)
	hydrated := make(map[core.RatingKey]core.Title, len(titles))
	for _, title := range titles {
		hydrated[title.Key] = title
	}
	byGenre = make(map[string][]core.Title, len(genres))
	for i, genre := range genres {
		for _, key := range windowKeys[i] {
			if title, ok := hydrated[key]; ok {
				byGenre[genre] = append(byGenre[genre], title)
			}
		}
	}
	return byGenre, true, nil
}
