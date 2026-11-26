package catalog

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("catalog", 67, `
		CREATE INDEX catalog_genres_folded_key ON catalog_genres(LOWER(genre), key);
		CREATE INDEX catalog_studios_folded_key ON catalog_studios(LOWER(studio), key);`)
}

func (c *Catalog) collectionCardQuery(ctx context.Context, f core.Filter, id int64, facet, value string, anyPersonal *core.FilterGroup, releaseMode string, lookahead int) (from, where, position string, args []any, err error) {
	f = c.viewerPaging(ctx, normalizeConsoleFilter(f))
	f.CatalogCandidates = false
	upcoming := releaseMode == "mixed" || releaseMode == "upcoming"
	where, args, err = c.browseWhereMode(f, upcoming)
	if err != nil {
		return "", "", "", nil, err
	}
	if upcoming {
		rules, ruleArgs := c.browseViewerRules(f)
		if len(rules) > 0 {
			where += " AND " + strings.Join(rules, " AND ")
			args = append(args, ruleArgs...)
		}
		now := c.now()
		where += " AND t.adult=0 AND t.release_known=1 AND t.released_ts<=?"
		args = append(args, now.AddDate(0, 0, lookahead).Unix())
		if releaseMode == "upcoming" {
			where += " AND t.released_ts>?"
			args = append(args, now.Unix())
		}
	}
	from, position = "collections_members m", "m.pos"
	seek, bind := "m.spec_id=? AND m.shared=1", []any{id}
	switch facet {
	case "genre":
		from, position, seek, bind = "catalog_genres m", "m.key", "LOWER(m.genre)=?", []any{value}
	case "studio":
		from, position, seek, bind = "catalog_studios m", "m.key", "LOWER(m.studio)=?", []any{value}
	case "":
		if anyPersonal != nil {
			expr, values, err := c.compileBrowseGroup(anyPersonal, "t", 0, f.Type)
			if err != nil {
				return "", "", "", nil, err
			}
			seek = "m.spec_id=? AND (m.shared=1 OR (" + expr + "))"
			bind = append(bind, values...)
		}
	}
	// Keep SQLite on the narrow member/facet index instead of a catalogue-wide index.
	return from + " CROSS JOIN catalog_titles t", "t.key=m.key AND (" + seek + ") AND " + where, position, append(bind, args...), nil
}

// CollectionCardSample reads one authorized artwork record without loading members.
func (c *Catalog) CollectionCardSample(ctx context.Context, f core.Filter, id int64, facet, value string, anyPersonal *core.FilterGroup, releaseMode string, lookahead int) (core.Title, error) {
	from, where, _, args, err := c.collectionCardQuery(ctx, f, id, facet, value, anyPersonal, releaseMode, lookahead)
	if err != nil {
		return core.Title{}, err
	}
	query := "SELECT t.key,t.type,t.poster,t.backdrop FROM " + from + " WHERE " + where + " LIMIT 1"
	var sample core.Title
	err = c.db.QueryRowContext(ctx, query, args...).Scan(&sample.Key, &sample.Type, &sample.PosterPath, &sample.BackdropPath)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Title{}, nil
	}
	return sample, err
}

// CollectionCardMembers seeks one page in persisted member order (facet key
// order for local facet collections). Only the selected page is hydrated.
func (c *Catalog) CollectionCardMembers(ctx context.Context, f core.Filter, id int64, facet, value string, anyPersonal *core.FilterGroup, releaseMode string, lookahead int, after int64, limit int) ([]core.CollectionCardMember, error) {
	from, where, position, args, err := c.collectionCardQuery(ctx, f, id, facet, value, anyPersonal, releaseMode, lookahead)
	if err != nil {
		return nil, err
	}
	selectClause := "SELECT "
	if facet != "" {
		// Facet rows can contain multiple spellings of the same value.
		selectClause = "SELECT DISTINCT "
	}
	query := selectClause + position + ",t.key FROM " + from + " WHERE " + where +
		" AND " + position + ">? ORDER BY " + position + " LIMIT ?"
	rows, err := c.db.QueryContext(ctx, query, append(args, after, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []core.CollectionCardMember
	for rows.Next() {
		var member core.CollectionCardMember
		if err := rows.Scan(&member.Position, &member.Key); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}
