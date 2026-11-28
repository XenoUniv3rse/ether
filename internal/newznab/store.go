package newznab

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("newznab", 9, `CREATE INDEX newznab_cache_by_indexer_expiry ON newznab_cache(indexer_id,expires_at);`)
	store.Register("newznab", 8, `ALTER TABLE newznab_budget ADD COLUMN feed_hits INTEGER NOT NULL DEFAULT 0;`)
	store.Register("newznab", 7, `
CREATE TABLE newznab_plugin_refs (ref TEXT PRIMARY KEY, descriptor TEXT NOT NULL);
`)
	store.Register("newznab", 6, `
CREATE TABLE newznab_nzb_refusals (
  indexer_id TEXT NOT NULL,
  release_id TEXT NOT NULL,
  retry_at BIGINT NOT NULL,
  PRIMARY KEY (indexer_id, release_id)
);
CREATE INDEX newznab_nzb_refusals_expiry ON newznab_nzb_refusals(retry_at);
`)
	store.Register("newznab", 5, `CREATE TABLE newznab_search_modes (indexer_id TEXT PRIMARY KEY, identity TEXT NOT NULL, mode INTEGER NOT NULL DEFAULT 0, verified INTEGER NOT NULL DEFAULT 0, checked_at INTEGER NOT NULL);`)
	store.Register("newznab", 4, `CREATE TABLE newznab_feed_modes (indexer_id TEXT PRIMARY KEY, identity TEXT NOT NULL, unfiltered INTEGER NOT NULL DEFAULT 0, checked_at INTEGER NOT NULL);`)
	// endpoint fingerprints the URL and API key the caps were fetched
	// from: an indexer edited to point elsewhere must not reuse them.
	store.Register("newznab", 3, `ALTER TABLE newznab_caps ADD COLUMN endpoint TEXT NOT NULL DEFAULT '';`)
	store.Register("newznab", 1, `
CREATE TABLE newznab_budget (
  indexer_id TEXT NOT NULL,
  day TEXT NOT NULL,
  hits INTEGER NOT NULL DEFAULT 0,
  grabs INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (indexer_id, day)
);
CREATE TABLE newznab_cache (
  cache_key TEXT PRIMARY KEY,
  indexer_id TEXT NOT NULL,
  results TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX newznab_cache_expires ON newznab_cache(expires_at);
CREATE TABLE newznab_caps (
  indexer_id TEXT PRIMARY KEY,
  caps TEXT NOT NULL,
  fetched_at INTEGER NOT NULL
);`)
}

// ErrMissingDefinition reports a built-in site indexer whose definition is not
// installed on the server.
var ErrMissingDefinition = fmt.Errorf("indexer definition is not installed: %w", core.ErrSourceUnavailable)

// Remaining reports one indexer's budget state for today (UTC).
// A remaining value of -1 means there is no personal cap; service limits still apply.
type Remaining struct {
	Indexer           string `json:"indexer"`
	Day               string `json:"day"`
	HitsUsed          int    `json:"hitsUsed"`
	HitsLimit         int    `json:"hitsLimit"`
	HitsRemaining     int    `json:"hitsRemaining"`
	GrabsUsed         int    `json:"grabsUsed"`
	GrabsLimit        int    `json:"grabsLimit"`
	GrabsRemaining    int    `json:"grabsRemaining"`
	RetryAt           int64  `json:"retryAt,omitempty"`     // service-reported throttling; automatic retry (Unix seconds)
	PausedUntil       int64  `json:"pausedUntil,omitempty"` // circuit-breaker pause after repeated failures (Unix seconds)
	MissingDefinition bool   `json:"missingDefinition,omitempty"`
	// Enabled and Torrent let the dashboard warn when nothing can be
	// searched, or when debrid has no torrent indexer to search.
	Enabled bool `json:"enabled"`
	Torrent bool `json:"torrent"` // Torznab, a Stremio addon or a torrent built-in site
}

func (c *Client) missingDefinition() bool {
	cfg := c.Config()
	if !cfg.IsBuiltin() {
		return false
	}
	if c.backends == nil {
		return true
	}
	_, err := c.backends(cfg)
	return errors.Is(err, ErrMissingDefinition)
}

func (c *Client) indexerID() string {
	cfg := c.Config()
	if cfg.ID != "" {
		return cfg.ID
	}
	return c.Name()
}

func (c *Client) today() string {
	return c.now().UTC().Format("2006-01-02")
}

type memBudget struct {
	day                   string
	hits, grabs, feedHits int
}

// counts returns today's (hits, grabs).
func (c *Client) counts(ctx context.Context) (int, int, error) {
	day := c.today()
	if c.db == nil {
		c.budgetMu.Lock()
		defer c.budgetMu.Unlock()
		if c.mem.day != day {
			return 0, 0, nil
		}
		return c.mem.hits, c.mem.grabs, nil
	}
	var hits, grabs int
	err := c.db.QueryRowContext(ctx,
		`SELECT hits, grabs FROM newznab_budget WHERE indexer_id=? AND day=?`, c.indexerID(), day).
		Scan(&hits, &grabs)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	return hits, grabs, err
}

// reserve checks and increments one budget counter ("hit" or "grab").
func (c *Client) reserve(ctx context.Context, kind string) error {
	if until, err := c.rateLimitUntil(ctx); err != nil {
		return err
	} else if c.now().Before(until) {
		return fmt.Errorf("indexer %q paused until %s: %w: %w", c.Name(), until.UTC().Format(time.RFC3339), errLocalBudget, core.ErrBudgetExhausted)
	}
	cfg := c.Config()
	limit := c.reportedLimit(kind)
	if kind == "grab" {
		limit = lowerLimit(limit, cfg.DailyGrabs)
	}
	totalLimit := cfg.DailyLimit
	feed := kind == "hit" && core.IndexerPurpose(ctx) == core.IndexerPurposeFeed
	background := backgroundSearch(ctx)
	if wasBackground, caps := ctx.Value(capsRequestKey{}).(bool); caps {
		background = wasBackground
	}
	if err := c.speculativeRoom(ctx, kind); err != nil {
		return err
	}
	if background {
		// Every background producer shares a ceiling on total consumption,
		// including foreground work already spent today.
		all := lowerLimit(totalLimit, c.reportedLimit("hit"))
		if all == 0 {
			all = config.AssumedDailyLimit
		}
		pct := 100 - max(20, core.ForegroundReserve(ctx))
		totalLimit = min(all-2, all*pct/100)
		if limit > 0 {
			limit = limit * pct / 100
			if limit == 0 {
				return fmt.Errorf("%w: %w: %w", errLocalBudget, errSpeculativeShare, core.ErrBudgetExhausted)
			}
		}
		if totalLimit <= 0 {
			return fmt.Errorf("%w: %w: %w", errLocalBudget, errSpeculativeShare, core.ErrBudgetExhausted)
		}
	}
	if err := core.SpendBackground(ctx, kind, c.indexerID(), 1); err != nil {
		if errors.Is(err, core.ErrBudgetExhausted) {
			return fmt.Errorf("%w: %w: %w", errLocalBudget, errSpeculativeShare, err)
		}
		return err
	}
	budgetError := func() error {
		if background {
			return fmt.Errorf("indexer %q %s budget: %w: %w: %w", c.Name(), kind, errLocalBudget, errSpeculativeShare, core.ErrBudgetExhausted)
		}
		return fmt.Errorf("indexer %q %s budget: %w: %w", c.Name(), kind, errLocalBudget, core.ErrBudgetExhausted)
	}
	day := c.today()
	if c.db == nil {
		c.budgetMu.Lock()
		if c.mem.day != day {
			c.mem = memBudget{day: day}
		}
		used := c.mem.hits
		if kind == "grab" {
			used = c.mem.grabs
		}
		if (limit > 0 && used >= limit) || (totalLimit > 0 && c.mem.hits+c.mem.grabs >= totalLimit) ||
			(feed && c.mem.feedHits >= maxDailyFeedHits) {
			c.budgetMu.Unlock()
			if !background {
				effective := lowerLimit(limit, totalLimit)
				c.emitBudgetExhausted(day, kind, effective, effective)
			}
			return budgetError()
		}
		if kind == "hit" {
			c.mem.hits++
			used = c.mem.hits
		} else {
			c.mem.grabs++
			used = c.mem.grabs
		}
		if feed {
			c.mem.feedHits++
		}
		total := c.mem.hits + c.mem.grabs
		c.budgetMu.Unlock()
		c.recordUsage(ctx, kind)
		if !background {
			c.emitBudgetWarning(day, kind, used, limit)
			c.emitBudgetWarning(day, "hit", total, totalLimit)
		}
		return nil
	}
	col := "hits"
	if kind == "grab" {
		col = "grabs"
	}
	if _, err := c.db.ExecContext(ctx,
		`INSERT INTO newznab_budget(indexer_id, day) VALUES (?,?) ON CONFLICT(indexer_id, day) DO NOTHING`,
		c.indexerID(), day); err != nil {
		return err
	}
	// One conditional increment checks and reserves atomically, so nodes
	// sharing the database can never overspend the budget together.
	q := `UPDATE newznab_budget SET ` + col + `=` + col + `+1`
	if feed {
		q += `, feed_hits=feed_hits+1`
	}
	q += ` WHERE indexer_id=? AND day=?`
	args := []any{c.indexerID(), day}
	if limit > 0 {
		q += ` AND ` + col + `<?`
		args = append(args, limit)
	}
	if totalLimit > 0 {
		q += ` AND hits+grabs<?`
		args = append(args, totalLimit)
	}
	if feed {
		q += ` AND feed_hits<?`
		args = append(args, maxDailyFeedHits)
	}
	res, err := c.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if !background {
			c.emitBudgetExhausted(day, kind, lowerLimit(limit, totalLimit), lowerLimit(limit, totalLimit))
		}
		return budgetError()
	}
	c.recordUsage(ctx, kind)
	if !background && (limit > 0 || totalLimit > 0) && c.events != nil {
		var used, total int
		if err := c.db.QueryRowContext(ctx, `SELECT `+col+`, hits+grabs FROM newznab_budget WHERE indexer_id=? AND day=?`, c.indexerID(), day).Scan(&used, &total); err == nil {
			c.emitBudgetWarning(day, kind, used, limit)
			c.emitBudgetWarning(day, "hit", total, totalLimit)
		}
	}
	return nil
}

// Remaining returns today's budget state.
func (c *Client) Remaining(ctx context.Context) (Remaining, error) {
	hits, grabs, err := c.counts(ctx)
	cfg := c.Config()
	now := c.now()
	r := Remaining{
		Indexer: c.Name(), Day: c.today(),
		HitsUsed: hits, HitsLimit: c.DailyAPILimit(), HitsRemaining: -1,
		GrabsUsed: grabs, GrabsLimit: lowerLimit(cfg.DailyGrabs, c.reportedLimit("grab")), GrabsRemaining: -1,
		PausedUntil:       c.br.pausedUntil(now),
		MissingDefinition: c.missingDefinition(),
		Enabled:           cfg.Enabled, Torrent: cfg.IsTorrent(),
	}
	if limit := c.reportedLimit("hit"); limit > 0 {
		r.HitsRemaining = max(0, limit-hits)
	}
	if r.GrabsLimit > 0 {
		r.GrabsRemaining = max(0, r.GrabsLimit-grabs)
	}
	if cfg.DailyLimit > 0 {
		left := max(0, cfg.DailyLimit-hits-grabs)
		if r.HitsRemaining < 0 || r.HitsRemaining > left {
			r.HitsRemaining = left
		}
		if r.GrabsRemaining < 0 || r.GrabsRemaining > left {
			r.GrabsRemaining = left
		}
	}
	if until, rateErr := c.rateLimitUntil(ctx); rateErr != nil && err == nil {
		err = rateErr
	} else if now.Before(until) {
		r.RetryAt = until.Unix()
	}
	return r, err
}

// Exhausted reports whether either daily budget is spent.
func (c *Client) Exhausted(ctx context.Context) bool {
	r, err := c.Remaining(ctx)
	return err == nil && (r.HitsRemaining == 0 || r.GrabsRemaining == 0 || r.RetryAt > c.now().Unix())
}

func (c *Client) searchExhausted(ctx context.Context) bool {
	r, err := c.Remaining(ctx)
	if err != nil {
		return false
	}
	return r.HitsRemaining == 0 || r.RetryAt > c.now().Unix()
}

// --- result cache ---

type memCacheEntry struct {
	results []core.Release
	expires time.Time
}

func (c *Client) ttl() time.Duration {
	c.cfgMu.RLock()
	defer c.cfgMu.RUnlock()
	return c.cacheTTL
}

func (c *Client) cacheGet(ctx context.Context, key string) ([]core.Release, bool) {
	if c.ttl() <= 0 {
		return nil, false
	}
	now := c.now()
	if c.db == nil {
		c.cacheMu.RLock()
		defer c.cacheMu.RUnlock()
		e, ok := c.memCache[key]
		if !ok || now.After(e.expires) {
			return nil, false
		}
		return c.bindReleases(e.results), true
	}
	var raw string
	var exp int64
	err := c.db.QueryRowContext(ctx, `SELECT results, expires_at FROM newznab_cache WHERE cache_key=? AND indexer_id=?`, key, c.indexerID()).Scan(&raw, &exp)
	if err != nil || now.Unix() > exp {
		return nil, false
	}
	return c.decodeCached(raw)
}

// emptySearchCacheTTL bounds how long a 0-result indexer search is cached, so
// an episode checked at 8 PM before its upload is re-searched once it drops.
const emptySearchCacheTTL = 3 * time.Minute

func (c *Client) cachePut(ctx context.Context, key string, results []core.Release) {
	results = c.bindReleases(results)
	// The owner's "Remember searches for" is a hard cap: age may only shorten it.
	ttl := min(searchCacheTTL(c.ttl(), cacheAge(ctx), c.now(), len(results) == 0), c.ttl())
	if ttl <= 0 {
		return
	}
	exp := c.now().Add(ttl)
	if c.db == nil {
		c.cacheMu.Lock()
		defer c.cacheMu.Unlock()
		if c.memCache == nil {
			c.memCache = map[string]memCacheEntry{}
		}
		c.memCache[key] = memCacheEntry{results: append([]core.Release(nil), results...), expires: exp}
		return
	}
	raw, ok := c.encodeCached(results)
	if !ok {
		return
	}
	if _, err := c.db.ExecContext(ctx,
		`INSERT INTO newznab_cache(cache_key, indexer_id, results, expires_at) VALUES (?,?,?,?)
		 ON CONFLICT(cache_key) DO UPDATE SET results=excluded.results, expires_at=excluded.expires_at`,
		key, c.indexerID(), raw, exp.Unix()); err != nil {
		c.log.Debug("newznab cache save failed", "indexer", c.Name(), "err", err)
	}
}

// --- caps cache ---

func (c *Client) capsFromDB(ctx context.Context, endpoint string) (*Caps, bool) {
	if c.db == nil {
		return nil, false
	}
	var raw string
	var at int64
	if err := c.db.QueryRowContext(ctx, `SELECT caps, fetched_at FROM newznab_caps WHERE indexer_id=? AND endpoint=?`, c.indexerID(), endpoint).Scan(&raw, &at); err != nil {
		return nil, false
	}
	if c.now().Sub(time.Unix(at, 0)) > 24*time.Hour {
		return nil, false
	}
	var caps Caps
	if json.Unmarshal([]byte(raw), &caps) != nil {
		return nil, false
	}
	return &caps, true
}

func (c *Client) capsToDB(ctx context.Context, caps *Caps, endpoint string) {
	if c.db == nil || caps == nil {
		return
	}
	raw, err := json.Marshal(caps)
	if err != nil {
		return
	}
	if _, err := c.db.ExecContext(ctx,
		`INSERT INTO newznab_caps(indexer_id, caps, fetched_at, endpoint) VALUES (?,?,?,?)
		 ON CONFLICT(indexer_id) DO UPDATE SET caps=excluded.caps, fetched_at=excluded.fetched_at, endpoint=excluded.endpoint`,
		c.indexerID(), string(raw), c.now().Unix(), endpoint); err != nil {
		c.log.Debug("newznab caps save failed", "indexer", c.Name(), "err", err)
	}
}
