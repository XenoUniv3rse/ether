package pluginitems

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/store"
)

// ErrInvalidBrowseField is returned when a browse or search filter references
// an unknown or blank field (such as in BrowseQuery.Empty, #807).
var ErrInvalidBrowseField = errors.New("pluginitems: unknown browse field")

// activeAnyGroupAlts normalizes one KindBrowseFacetGroup into its non-empty
// {field, values} alternatives (#767, #807).
func activeAnyGroupAlts(g core.KindBrowseFacetGroup) []core.KindBrowseFacetAlt {
	raw := g.Alternatives()
	if len(raw) == 0 {
		return nil
	}
	var out []core.KindBrowseFacetAlt
	for _, alt := range raw {
		fk := strings.TrimSpace(alt.Field)
		if fk == "" {
			continue
		}
		vals := cleanFacetValues(alt.Values)
		if len(vals) == 0 {
			continue
		}
		out = append(out, core.KindBrowseFacetAlt{Field: fk, Values: vals})
	}
	return out
}

// hasActiveExcludeQuery reports whether raw carries at least one searchable
// token so excludeQuery will emit a predicate (#807).
func hasActiveExcludeQuery(raw string) bool {
	return len(tokenizeSearchQuery(raw)) > 0
}

// buildSQLiteFTSMatch formats tokenized query terms into a SQLite FTS5 MATCH
// expression matching searchSQLite's exact prefix rules.
func buildSQLiteFTSMatch(tokens []string) string {
	parts := make([]string, 0, len(tokens))
	for i, tok := range tokens {
		part := `"` + strings.ReplaceAll(tok, `"`, `""`) + `"`
		if searchTokenPrefix(tok, i, len(tokens)) {
			part += "*"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " AND ")
}

// buildPostgresTSQuery formats tokenized query terms into a PostgreSQL
// to_tsquery('simple', ...) expression matching searchPostgres's exact rules.
func buildPostgresTSQuery(tokens []string) string {
	parts := make([]string, 0, len(tokens))
	for i, tok := range tokens {
		if searchTokenPrefix(tok, i, len(tokens)) {
			tok += ":*"
		}
		parts = append(parts, tok)
	}
	return strings.Join(parts, " & ")
}

// appendAnyGroupClause emits a correlated EXISTS (or direct envelope column)
// predicate for one anyGroups entry. Multiple alternatives are ORed inside
// parentheses and ANDed with the rest of the query (#807).
func appendAnyGroupClause(where *strings.Builder, args *[]any, types map[string]string, g core.KindBrowseFacetGroup, isPG bool) {
	alts := activeAnyGroupAlts(g)
	if len(alts) == 0 {
		return
	}
	if len(alts) == 1 {
		appendLibraryFacetMode(where, args, map[string][]string{alts[0].Field: alts[0].Values}, types, facetAny, isPG)
		return
	}
	parts := make([]string, 0, len(alts))
	var groupArgs []any
	for _, alt := range alts {
		var tmp strings.Builder
		var tmpArgs []any
		appendLibraryFacetMode(&tmp, &tmpArgs, map[string][]string{alt.Field: alt.Values}, types, facetAny, isPG)
		clause := strings.TrimPrefix(tmp.String(), " AND ")
		if clause == "" {
			continue
		}
		parts = append(parts, "("+clause+")")
		groupArgs = append(groupArgs, tmpArgs...)
	}
	if len(parts) == 0 {
		return
	}
	*args = append(*args, groupArgs...)
	if len(parts) == 1 {
		where.WriteString(" AND " + parts[0])
		return
	}
	where.WriteString(" AND (" + strings.Join(parts, " OR ") + ")")
}

func validateBrowseEmptyFields(decl plugins.MediaKindMeta, empty map[string]bool) ([]string, error) {
	keys := make([]string, 0, len(empty))
	for k := range empty {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !plugins.ValidKindEmptyField(decl, k) {
			return nil, fmt.Errorf("%w %q", ErrInvalidBrowseField, k)
		}
	}
	return keys, nil
}

// appendBrowseEmptyFields emits correlated EXISTS / NOT EXISTS predicates on
// plugin_item_fields for declared fields and direct column checks on
// plugin_items for envelope columns (#807). Keys are sorted for deterministic
// SQL and count-cache keys. Unknown fields are rejected.
func (s *Service) appendBrowseEmptyFields(ctx context.Context, lib core.LibraryID, where *strings.Builder, args *[]any, empty map[string]bool) error {
	return s.appendBrowseEmptyPlan(ctx, lib, where, args, empty, nil)
}

type browsePresenceField struct {
	key      string
	textOnly bool
}

func browsePresenceFields(decl plugins.MediaKindMeta) map[string]browsePresenceField {
	fields := make(map[string]browsePresenceField, len(decl.Fields))
	for _, f := range decl.Fields {
		fields[f.Key] = browsePresenceField{key: f.Key, textOnly: f.Type == "string" || f.Type == "strings" || f.Type == "enum"}
	}
	for _, f := range decl.Fields {
		if f.Type == "date" {
			alias := f.Key + "-decade"
			if _, declared := fields[alias]; !declared {
				fields[alias] = fields[f.Key]
			}
		}
	}
	return fields
}

func (s *Service) appendBrowseEmptyPlan(ctx context.Context, lib core.LibraryID, where *strings.Builder, args *[]any, empty, matchFirst map[string]bool) error {
	if len(empty) == 0 {
		return nil
	}
	rec, err := s.GetKindByLibrary(ctx, lib)
	if err != nil {
		return err
	}
	keys, err := validateBrowseEmptyFields(rec.Decl, empty)
	if err != nil {
		return err
	}
	fields := browsePresenceFields(rec.Decl)
	isPG := store.DialectOf(s.db) == store.Postgres
	for _, k := range keys {
		wantEmpty := empty[k]
		if field, indexed := fields[k]; indexed {
			predicate := "f.field = ?"
			if field.textOnly {
				predicate += " AND f.v_text <> ''"
			}
			if !isPG && matchFirst[k] {
				// SQLite has no hash anti-join. Build its membership B-tree
				// once using the field-led index instead of probing each item.
				op := "IN"
				if wantEmpty {
					op = "NOT IN"
				}
				where.WriteString(" AND i.item_ref " + op + " (SELECT f.item_ref FROM plugin_item_fields f WHERE " + predicate + ")")
			} else {
				op := "EXISTS"
				if wantEmpty {
					op = "NOT EXISTS"
				}
				where.WriteString(" AND " + op + " (SELECT 1 FROM plugin_item_fields f WHERE f.item_ref = i.item_ref AND " + predicate + ")")
			}
			*args = append(*args, field.key)
			continue
		}
		// Envelope columns on plugin_items are NOT NULL with 0 / '' defaults.
		switch k {
		case "released":
			if wantEmpty {
				where.WriteString(` AND i.released = 0`)
			} else {
				where.WriteString(` AND i.released <> 0`)
			}
		case "year", "decade":
			if wantEmpty {
				where.WriteString(` AND i.year <= 0`)
			} else {
				where.WriteString(` AND i.year > 0`)
			}
		case "rating":
			if wantEmpty {
				where.WriteString(` AND i.rating <= 0`)
			} else {
				where.WriteString(` AND i.rating > 0`)
			}
		case "summary":
			if wantEmpty {
				where.WriteString(` AND i.summary = ''`)
			} else {
				where.WriteString(` AND i.summary <> ''`)
			}
		case "recording":
			if wantEmpty {
				where.WriteString(` AND i.recording = ''`)
			} else {
				where.WriteString(` AND i.recording <> ''`)
			}
		case "parentId":
			if wantEmpty {
				where.WriteString(` AND i.parent_ref = ''`)
			} else {
				where.WriteString(` AND i.parent_ref <> ''`)
			}
		case "index":
			if wantEmpty {
				where.WriteString(` AND i.idx <= 0`)
			} else {
				where.WriteString(` AND i.idx > 0`)
			}
		}
	}
	return nil
}

// appendBrowseExcludeQuery drops items whose indexed text matches excludeQuery
// using the same tokenization and prefix matching as Search (#807). On
// PostgreSQL it evaluates directly against i.search_tsv; on SQLite it probes
// plugin_item_fts via a correlated NOT EXISTS on ef.rowid = i.seq.
func appendBrowseExcludeQuery(where *strings.Builder, args *[]any, excludeQuery string, isPG bool) {
	tokens := tokenizeSearchQuery(excludeQuery)
	if len(tokens) == 0 {
		return
	}
	if isPG {
		where.WriteString(` AND NOT (i.search_tsv @@ to_tsquery('simple', ?))`)
		*args = append(*args, buildPostgresTSQuery(tokens))
		return
	}
	where.WriteString(` AND NOT EXISTS (SELECT 1 FROM plugin_item_fts ef WHERE ef.rowid = i.seq AND ef.plugin_item_fts MATCH ?)`)
	*args = append(*args, buildSQLiteFTSMatch(tokens))
}
