package newznab

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

type warmSearchKey struct{}

// Warm-ups can reuse every source's answer, even when Play returned before
// the rest of the fleet finished. A cold warm-up may use at most two sources.
func (m *Manager) reuseWarmSearch(ctx context.Context, req core.SearchRequest, clients []*Client) context.Context {
	if core.CachedIndexerSearch(ctx) || !core.IsUnattendedSearch(ctx) || usagePurpose(ctx) != core.IndexerPurposeWarmup {
		return ctx
	}
	var pending []*Client
	for _, c := range clients {
		cfg := c.Config()
		if !cfg.Enabled || core.IndexerSkipped(ctx, c.indexerID()) || (core.TorrentOnlySearch(ctx) && !cfg.IsTorrent()) {
			continue
		}
		c.capsMu.Lock()
		caps := c.caps
		c.capsMu.Unlock()
		if (animeOnly(cfg, caps) && !isAnimeSearch(req)) ||
			(!cfg.UsesBackend() && len(routedCategories(cfg, caps, req)) == 0) {
			continue
		}
		key := cacheKey(c, req)
		_, ready := c.cacheGet(ctx, key)
		_, recentMiss := c.cacheGet(ctx, key+primaryMissKey)
		if ready || recentMiss {
			return context.WithValue(ctx, warmSearchKey{}, map[*Client]bool{})
		}
		pending = append(pending, c)
	}
	allowed := map[*Client]bool{}
	for _, c := range pending[:min(2, len(pending))] {
		allowed[c] = true
	}
	return context.WithValue(ctx, warmSearchKey{}, allowed)
}

func cacheOnlySearch(ctx context.Context, c *Client) bool {
	if core.CachedIndexerSearch(ctx) {
		return true
	}
	allowed, ok := ctx.Value(warmSearchKey{}).(map[*Client]bool)
	return ok && core.IsUnattendedSearch(ctx) && !allowed[c]
}
