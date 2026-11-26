package catalog

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// GuideArtwork matches a local movie or show by exact name and optional year.
// Count namesakes even when they have no artwork, so an undated airing cannot
// borrow an image from a different edition. The title index bounds each probe.
func (c *Catalog) GuideArtwork(ctx context.Context, title string, year int, kind core.MediaType) (poster, backdrop string, err error) {
	kinds := []core.MediaType{kind}
	if kind == "" {
		kinds = []core.MediaType{core.TypeMovie, core.TypeShow}
	}
	title = strings.TrimSpace(title)
	found := false
	for _, k := range kinds {
		query := "SELECT poster, backdrop, adult FROM catalog_titles WHERE type=? AND LOWER(title)=LOWER(?)"
		args := []any{k, title}
		if year > 0 {
			query += " AND year=?"
			args = append(args, year)
		}
		rows, err := c.db.QueryContext(ctx, query+" LIMIT 2", args...)
		if err != nil {
			return "", "", err
		}
		for rows.Next() {
			var adult int
			if err := rows.Scan(&poster, &backdrop, &adult); err != nil {
				rows.Close()
				return "", "", err
			}
			if found || adult != 0 {
				rows.Close()
				return "", "", nil
			}
			found = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return "", "", err
		}
	}
	return poster, backdrop, nil
}
