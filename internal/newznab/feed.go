package newznab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

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
		if c.caps != nil {
			page = c.caps.LimitMax
		}
		c.capsMu.Unlock()
		f := core.ReleaseFeed{ID: c.indexerID(), Name: c.Name(), Generation: hex.EncodeToString(sum[:]), Protocol: cfg.Protocol, Categories: recentCategories(cfg), Supported: hasRecentFeed(cfg), DailyLimit: c.DailyAPILimit(), PageMax: page}
		if cfg.Kind == config.IndexerKindCardigann {
			// A site lists its latest releases on one page; it cannot page back.
			f.PageMax, f.SinglePage = 0, true
		}
		if !f.Supported {
			f.Reason = "This source has no recent-release listing; Ether searches it for a title when needed."
		}
		out = append(out, f)
	}
	return out
}

// Recent fetches one page, charging the same account-wide limits as Search.
// It never invokes title searches, capability discovery, or implicit retries.
func (m *Manager) Recent(ctx context.Context, id string, offset, limit int) ([]core.Release, error) {
	for _, c := range m.Clients() {
		if c.indexerID() != id {
			continue
		}
		cfg := c.Config()
		if !cfg.Enabled || !hasRecentFeed(cfg) {
			return nil, fmt.Errorf("recent-release feed is not enabled")
		}
		ctx = core.WithIndexerPurpose(ctx, core.IndexerPurposeFeed)
		if cfg.UsesBackend() {
			if offset > 0 {
				return nil, nil // one page: the feed ends there
			}
			return c.backendRecent(ctx)
		}
		return c.recent(ctx, offset, limit)
	}
	return nil, fmt.Errorf("release-feed source no longer configured")
}

// hasRecentFeed reports whether an indexer lists its recent releases: a
// Newznab/Torznab API, or a built-in site's latest-releases page. Stremio
// addons and plugin sources only answer title searches.
func hasRecentFeed(cfg config.Indexer) bool {
	return !cfg.UsesBackend() || cfg.Kind == config.IndexerKindCardigann
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
