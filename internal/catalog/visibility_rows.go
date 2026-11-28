package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// readVisibilityRows returns one row per requested title with just the
// required policy facts. This keeps repeated title keys and fact tags out of
// the driver result and permits an indexed lookup for each selected facet.
func (c *Catalog) readVisibilityRows(ctx context.Context, args []any, facts core.TitleFacts) ([]core.Title, error) {
	return c.readVisibilityRowFacts(ctx, args, facts, facts != 0)
}

func (c *Catalog) readVisibilityRowFacts(ctx context.Context, args []any, facts core.TitleFacts, episodeAir bool) ([]core.Title, error) {
	columns := []string{qualifiedTitleCols("t")}
	fields := [...]struct {
		fact          core.TitleFacts
		table, column string
	}{
		{core.FactGenres, "catalog_genres", "genre"},
		{core.FactStudios, "catalog_studios", "studio"},
		{core.FactCountries, "catalog_countries", "country"},
		{core.FactKeywords, "catalog_keywords", "keyword"},
	}
	for _, field := range fields {
		if facts&field.fact != 0 {
			columns = append(columns, textFactList(c.db, field.table, field.column, "t.key"))
		}
	}
	joins := ""
	if episodeAir {
		joins += ` LEFT JOIN catalog_episode_provider_air p ON p.key=t.key
			LEFT JOIN catalog_episode_release e ON e.key=t.key`
		columns = append(columns, `CASE WHEN t.type='episode'
			THEN COALESCE(p.air_ts,CASE WHEN e.earliest_ts=t.released_ts THEN 0 ELSE t.released_ts END)
			ELSE 0 END`)
	}
	if facts&core.FactHomeRelease != 0 {
		joins += " LEFT JOIN catalog_home_release h ON h.key=t.key"
		columns = append(columns, "COALESCE(h.checked,0)", "COALESCE(h.earliest_ts,0)", "COALESCE(h.fallback_admitted,0)")
	}
	rows, err := c.db.QueryContext(ctx, "SELECT "+strings.Join(columns, ",")+" FROM catalog_titles t"+joins+
		" WHERE t.key IN ("+placeholders(len(args))+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Title
	for rows.Next() {
		var raw [len(fields)]string
		var air, checked, earliest, admitted int64
		dest := make([]any, 0, len(fields)+4)
		for i, field := range fields {
			if facts&field.fact != 0 {
				dest = append(dest, &raw[i])
			}
		}
		if episodeAir {
			dest = append(dest, &air)
		}
		if facts&core.FactHomeRelease != 0 {
			dest = append(dest, &checked, &earliest, &admitted)
		}
		st, err := scanTitle(extraScan{rows, dest})
		if err != nil {
			return nil, err
		}
		t := &st.t
		for i, field := range []*[]string{&t.Genres, &t.Studios, &t.Countries, &t.Keywords} {
			if raw[i] == "" || raw[i] == "[]" {
				continue
			}
			if err := json.Unmarshal([]byte(raw[i]), field); err != nil {
				return nil, err
			}
			if i == 0 || i == 3 {
				*field = uniqueStrings(*field)
			}
			sort.Strings(*field)
		}
		if t.Type == core.TypeEpisode && episodeAir {
			t.EpisodeAirDate = time.Time{}
			if air != 0 {
				t.EpisodeAirDate = time.Unix(air, 0).UTC()
			}
		}
		t.HomeReleaseChecked, t.HomeReleaseFallbackAdmitted = checked != 0, admitted != 0
		if earliest != 0 {
			t.HomeReleased = time.Unix(earliest, 0).UTC()
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func textFactList(db *sql.DB, table, column, key string) string {
	aggregate := "json_group_array(" + column + ")"
	if store.DialectOf(db) == store.Postgres {
		aggregate = "CAST(json_agg(" + column + ") AS TEXT)"
	}
	// The two engines differ on an aggregate over zero rows.
	return "COALESCE((SELECT " + aggregate + " FROM " + table + " WHERE " + table + ".key=" + key + "), '[]')"
}
