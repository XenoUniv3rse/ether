package catalog

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/reqmemo"
	"github.com/braedonsaunders/ether/internal/store"
)

type requestRowFacts struct {
	mu    sync.Mutex
	byKey map[core.RatingKey]core.Title // zero key records a successful miss
}

// LoadRowFacts is a bounded read at row assembly, never a visibility predicate.
// Warm canonical titles reuse the catalogue cache. Each cold chunk reads release,
// home-release and mandatory safety facts together with indexed key lookups.
// Missing remote summaries remain in memory until background mirroring succeeds.
func (c *Catalog) LoadRowFacts(ctx context.Context, titles []core.Title) []core.Title {
	if len(titles) == 0 {
		return titles
	}
	facts := map[core.RatingKey]core.Title{}
	var need []core.Title
	seen := map[core.RatingKey]bool{}
	gen, minute := c.writeGen.Load(), c.minuteBucket()
	memo, _ := reqmemo.Do(ctx, fmt.Sprintf("catalog:row-facts:%p:%d:%d", c, gen, minute), func() (*requestRowFacts, error) {
		return &requestRowFacts{byKey: map[core.RatingKey]core.Title{}}, nil
	})
	memo.mu.Lock()
	var missing []core.Title
	for _, t := range titles {
		if t.Key.IsPluginKey() || t.Type != core.TypeMovie && t.Type != core.TypeShow || seen[t.Key] {
			continue
		}
		seen[t.Key] = true
		if cached, ok := c.cachedStored(t.Key, gen, minute); ok {
			facts[t.Key] = cached
		} else if fact, ok := memo.byKey[t.Key]; !ok {
			need = append(need, t)
		} else if fact.Key != 0 {
			facts[t.Key] = fact
		} else {
			missing = append(missing, t)
		}
	}
	enfCol := `CASE WHEN t.enforcement_rating<>'' THEN t.enforcement_rating WHEN t.content_rating='' AND (` + adultKeywordsExistsSQL("t.key") + `) THEN '` + contentrating.UncertifiedAdult + `' ELSE '' END`
	for chunk := range store.Chunks(need, 200) {
		args := keyArgs(chunk)
		rows, err := c.db.QueryContext(ctx, `SELECT t.key,t.released_ts,t.year,t.adult,t.content_rating,`+enfCol+`,COALESCE(h.checked,0),COALESCE(h.earliest_ts,0),COALESCE(h.fallback_admitted,0) FROM catalog_titles t LEFT JOIN catalog_home_release h ON h.key=t.key WHERE t.key IN (`+placeholders(len(args))+`)`, args...)
		if err != nil {
			memo.mu.Unlock()
			return nil
		}
		for rows.Next() {
			var t core.Title
			var released, home int64
			var adult, checked, fallback int
			if err := rows.Scan(&t.Key, &released, &t.Year, &adult, &t.ContentRating, &t.EnforcementRating, &checked, &home, &fallback); err != nil {
				rows.Close()
				memo.mu.Unlock()
				return nil
			}
			if released != 0 {
				t.Released = time.Unix(released, 0).UTC()
			}
			if home != 0 {
				t.HomeReleased = time.Unix(home, 0).UTC()
			}
			t.Adult = adult != 0
			t.HomeReleaseChecked = checked != 0
			t.HomeReleaseFallbackAdmitted = fallback != 0
			facts[t.Key] = t
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			memo.mu.Unlock()
			return nil
		}
	}
	for _, t := range need {
		fact, ok := facts[t.Key]
		memo.byKey[t.Key] = fact
		if !ok {
			missing = append(missing, t)
		}
	}
	memo.mu.Unlock()
	c.RememberListSummaries(ctx, missing)
	out := make([]core.Title, 0, len(titles))
	for _, t := range titles {
		if f, ok := facts[t.Key]; ok {
			t.Released = f.Released
			t.Year = f.Year
			t.Adult = f.Adult
			t.ContentRating = f.ContentRating
			t.EnforcementRating = contentrating.EffectiveEnforcement(f.EnforcementRating, f.ContentRating, f.Adult, f.Keywords)
			t.HomeReleased = f.HomeReleased
			t.HomeReleaseChecked = f.HomeReleaseChecked
			t.HomeReleaseFallbackAdmitted = f.HomeReleaseFallbackAdmitted
		} else if !t.Key.IsPluginKey() && (t.Type == core.TypeMovie || t.Type == core.TypeShow) {
			if _, ok := c.listSummary(t.Key); !ok {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}
