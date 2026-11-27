package newznab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// maxDailyFeedHits bounds the single-page recent-release poll per source.
const maxDailyFeedHits = 48

// FeedSources reports capabilities without making a request. Built-in sites
// remain searchable but are not treated as paginated feeds without that contract.
func (m *Manager) FeedSources() []core.ReleaseFeed {
	out := []core.ReleaseFeed{}
	for _, c := range m.Clients() {
		cfg := c.Config()
		if !cfg.Enabled {
			continue
		}
		raw, _ := json.Marshal(cfg)
		sum := sha256.Sum256(raw)
		c.capsMu.Lock()
		page := 0
		caps := c.caps
		if caps != nil {
			page = caps.LimitMax
		}
		c.capsMu.Unlock()
		anime := animeOnly(cfg, caps)
		categories := supportedCategories(recentCategories(cfg), caps)
		f := core.ReleaseFeed{
			ID: c.indexerID(), Name: c.Name(), Generation: hex.EncodeToString(sum[:]), Protocol: cfg.Protocol,
			Categories: categories, Supported: hasRecentFeed(cfg) && !anime && len(categories) > 0,
			DailyLimit: c.DailyAPILimit(), PageMax: page, DailyFeedLimit: maxDailyFeedHits,
			SinglePage: true, AnimeOnly: anime,
		}
		if !f.Supported {
			f.Reason = "This source has no recent-release listing; Ether searches it for a title when needed."
		}
		out = append(out, f)
	}
	return out
}

// Recent fetches one page, charging the same account-wide limits as Search.
// Caps disambiguate category support before a Newznab feed is read.
func (m *Manager) Recent(ctx context.Context, id string, offset, limit int) ([]core.Release, error) {
	if offset > 0 {
		return nil, nil // one recent page per scheduled poll
	}
	for _, c := range m.Clients() {
		if c.indexerID() != id {
			continue
		}
		cfg := c.Config()
		if !cfg.Enabled || !hasRecentFeed(cfg) || namedAnimeSource(cfg) {
			return nil, fmt.Errorf("recent-release feed is not enabled")
		}
		ctx = core.WithIndexerPurpose(ctx, core.IndexerPurposeFeed)
		if cfg.UsesBackend() {
			return c.backendRecent(ctx)
		}
		return c.recent(ctx, limit)
	}
	return nil, fmt.Errorf("release-feed source no longer configured")
}

// hasRecentFeed reports whether an indexer lists its recent releases: a
// Newznab/Torznab API, or a built-in site's latest-releases page. Stremio
// addons and plugin sources only answer title searches.
func hasRecentFeed(cfg config.Indexer) bool {
	return !cfg.UsesBackend() || cfg.Kind == config.IndexerKindCardigann
}

func recentCategories(cfg config.Indexer) []int {
	movies, tv := cfg.MovieCategories, cfg.TVCategories
	if len(movies) == 0 {
		movies = []int{2000}
	}
	if len(tv) == 0 {
		tv = []int{5000}
	}
	return append(append([]int{}, movies...), tv...)
}

func (c *Client) recent(ctx context.Context, limit int) ([]core.Release, error) {
	cfg := c.Config()
	caps, err := c.Caps(ctx)
	if ctx.Err() != nil || errors.Is(err, core.ErrBudgetExhausted) {
		return nil, err
	}
	cats := supportedCategories(recentCategories(cfg), caps)
	if animeOnly(cfg, caps) || len(cats) == 0 {
		return nil, nil
	}
	q := url.Values{"t": {"search"}, "extended": {"1"}, "limit": {strconv.Itoa(min(100, max(1, limit)))}, "cat": {joinCats(cats)}}
	rs, err := c.doSearch(ctx, q, c.clientIdentity(), config.IdentityMediaAny)
	if err == nil && len(rs) > 0 {
		c.cachePut(ctx, c.indexerID()+"|feed|"+searchIdentity(cfg), rs)
	}
	return rs, err
}

// Category listings reuse a bounded set of paid-for releases. Categories
// with no cached results can still be searched by title.
func (c *Client) cachedCategoryListing(ctx context.Context, cats []int) ([]core.Release, error) {
	if c.db == nil {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, `SELECT results FROM newznab_cache WHERE indexer_id=? AND expires_at>? ORDER BY expires_at DESC LIMIT 16`, c.indexerID(), c.now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Release
	seen := map[string]bool{}
	identity := searchIdentity(c.Config())
	for rows.Next() {
		var raw string
		var doc cachedResults
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(raw), &doc) != nil || doc.Identity != identity {
			continue
		}
		rs, ok := c.decodeCached(raw)
		if !ok {
			continue
		}
		for _, r := range rs {
			inCategory := slices.ContainsFunc(cats, func(cat int) bool {
				return r.Category == cat || (cat%1000 == 0 && r.Category/1000 == cat/1000)
			})
			if inCategory && !seen[r.ID] {
				out = append(out, r)
				seen[r.ID] = true
			}
		}
	}
	slices.SortStableFunc(out, func(a, b core.Release) int { return b.PubDate.Compare(a.PubDate) })
	return out[:min(100, len(out))], rows.Err()
}

// CachedReleases reuses a bounded set of already-paid-for search/feed results.
// It performs no requests, capability discovery, cache refresh, or downloads.
func (m *Manager) CachedReleases(ctx context.Context) ([]core.Release, error) {
	if m.db == nil {
		return nil, nil
	}
	clients := map[string]*Client{}
	for _, c := range m.Clients() {
		if c.Config().Enabled {
			clients[c.indexerID()] = c
		}
	}
	rows, err := m.db.QueryContext(ctx, `SELECT indexer_id,results FROM newznab_cache WHERE expires_at>? ORDER BY expires_at DESC LIMIT 100`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Release
	seen := map[string]bool{}
	for rows.Next() {
		var id, raw string
		if err = rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		c := clients[id]
		if c == nil {
			continue
		}
		// Bulk reuse must establish the same configuration binding as cacheKey.
		// Older cache rows remain usable by exact Search, but cannot be bulk adopted.
		var doc cachedResults
		if json.Unmarshal([]byte(raw), &doc) != nil || doc.Identity != searchIdentity(c.Config()) {
			continue
		}
		rs, ok := c.decodeCached(raw)
		if !ok {
			continue
		}
		for _, r := range rs {
			if !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, r)
				if len(out) >= 5000 {
					return out, nil
				}
			}
		}
	}
	return out, rows.Err()
}
