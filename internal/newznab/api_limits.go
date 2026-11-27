package newznab

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	// The daily API and grab limits an indexer reports about the account
	// (<newznab:apilimits apimax="…" grabmax="…"/>), remembered across
	// restarts so background work sizes itself before the first response.
	store.RegisterAfter("newznab_limits", "newznab")
	store.Register("newznab_limits", 2, `ALTER TABLE newznab_api_limits ADD COLUMN endpoint TEXT NOT NULL DEFAULT '';`)
	store.Register("newznab_limits", 1, `CREATE TABLE newznab_api_limits (indexer_id TEXT PRIMARY KEY, api_max INTEGER NOT NULL, grab_max INTEGER NOT NULL DEFAULT 0);`)
}

var (
	apiMaxAttr  = regexp.MustCompile(`(?i)apimax\W{0,4}(\d{1,9})`)
	grabMaxAttr = regexp.MustCompile(`(?i)grabmax\W{0,4}(\d{1,9})`)

	// errLocalBudget marks a request Ether refused locally (daily limit,
	// background share, or active Retry-After pause) before contacting the
	// indexer, distinguishing it from a remote HTTP 429 response.
	errLocalBudget = errors.New("newznab: held back by local budget")
	// errSpeculativeShare marks a request held back specifically by the
	// warm-up or viewer detail share while playback budget remains.
	errSpeculativeShare = errors.New("newznab: discovery share reached")
)

// reportedLimits reads the account's daily API request and NZB grab
// limits from a response that carries newznab:apilimits (nZEDb,
// newznab-tmux and the indexers built on them, in XML or JSON). Zero means
// not reported.
func reportedLimits(body []byte) (api, grabs int) {
	head := body[:min(len(body), 8192)]
	i := bytes.Index(bytes.ToLower(head), []byte("apilimits"))
	if i < 0 {
		return 0, 0
	}
	tag := head[i:min(len(head), i+400)]
	if m := apiMaxAttr.FindSubmatch(tag); m != nil {
		api, _ = strconv.Atoi(string(m[1]))
	}
	if m := grabMaxAttr.FindSubmatch(tag); m != nil {
		grabs, _ = strconv.Atoi(string(m[1]))
	}
	return api, grabs
}

type apiLimitState struct {
	mu        sync.Mutex
	endpoint  string
	api, grab int
}

// noteAPILimit remembers limits the indexer reported in body.
func (c *Client) noteAPILimit(ctx context.Context, body []byte) {
	api, grab := reportedLimits(body)
	if api <= 0 && grab <= 0 {
		return
	}
	c.loadLimits()
	c.apiLimit.mu.Lock()
	if api <= 0 {
		api = c.apiLimit.api
	}
	if grab <= 0 {
		grab = c.apiLimit.grab
	}
	changed := c.apiLimit.api != api || c.apiLimit.grab != grab
	c.apiLimit.api, c.apiLimit.grab = api, grab
	ep := c.apiLimit.endpoint
	defer c.apiLimit.mu.Unlock()
	if !changed || c.db == nil {
		return
	}
	if _, err := c.db.ExecContext(context.WithoutCancel(ctx), `INSERT INTO newznab_api_limits(indexer_id,api_max,grab_max,endpoint) VALUES(?,?,?,?) ON CONFLICT(indexer_id) DO UPDATE SET api_max=excluded.api_max,grab_max=excluded.grab_max,endpoint=excluded.endpoint`, c.indexerID(), api, grab, ep); err != nil {
		c.log.Debug("could not remember indexer API limit", "indexer", c.Name(), "err", err)
	}
}

func (c *Client) loadLimits() {
	c.apiLimit.mu.Lock()
	defer c.apiLimit.mu.Unlock()
	ep := c.capsEndpoint()
	if c.apiLimit.endpoint == ep {
		return
	}
	c.apiLimit.endpoint, c.apiLimit.api, c.apiLimit.grab = ep, 0, 0
	if c.db != nil {
		_ = c.db.QueryRowContext(context.Background(), `SELECT api_max,grab_max FROM newznab_api_limits WHERE indexer_id=? AND endpoint=?`, c.indexerID(), ep).Scan(&c.apiLimit.api, &c.apiLimit.grab)
	}
}

// reportedLimit is the daily API (kind "hit") or grab limit the indexer
// last reported, or 0.
func (c *Client) reportedLimit(kind string) int {
	c.loadLimits()
	c.apiLimit.mu.Lock()
	defer c.apiLimit.mu.Unlock()
	if kind == "grab" {
		return c.apiLimit.grab
	}
	return c.apiLimit.api
}

// lowerLimit picks the stricter of two limits; zero means unspecified.
func lowerLimit(a, b int) int {
	if a == 0 || b == 0 {
		return max(a, b)
	}
	return min(a, b)
}

// DailyAPILimit is the lower of the owner's cap and the reported API limit.
func (c *Client) DailyAPILimit() int {
	return lowerLimit(c.Config().DailyLimit, c.reportedLimit("hit"))
}

func (c *Client) dailyLimitOrAssumed(kind string) int {
	n := c.DailyAPILimit()
	if kind == "grab" {
		cfg := c.Config()
		n = lowerLimit(cfg.DailyLimit, lowerLimit(cfg.DailyGrabs, c.reportedLimit(kind)))
	}
	if n <= 0 {
		n = config.AssumedDailyLimit
	}
	return n
}

// Unattended warm-ups may spend 10% of each indexer's daily limit.
// Viewer-triggered detail-page discovery has a separate 15% allowance:
// neither class can exhaust the other's share. Availability searches have
// their own allowance (internal/availability); their grabs stay in the
// warm-up share. Playback is never refused by either share.
const (
	warmupShare        = 10
	detailShare        = 15
	warmupHitPurposes  = "'warmup'"
	warmupGrabPurposes = "'warmup','availability'"
	detailPurposes     = "'detail'"
)

// usagePurpose is the purpose a request is recorded and budgeted under: ctx's
// tag, except that a warm-up a viewer promoted to Play is playback from then
// on, while unpromoted speculative work never escapes into playback.
func usagePurpose(ctx context.Context) string {
	p := core.IndexerPurpose(ctx)
	// A detached refill with no viewer waiting must never spend the
	// viewer's detail allowance, even if its caller retained that tag.
	if p == core.IndexerPurposeDetail && core.IsUnattendedSearch(ctx) {
		return core.IndexerPurposeWarmup
	}
	if p == core.IndexerPurposeWarmup && !core.IsUnattendedSearch(ctx) {
		return core.IndexerPurposePlayback
	}
	if p == core.IndexerPurposePlayback && core.IsSpeculative(ctx) {
		return core.IndexerPurposeWarmup
	}
	return p
}

// speculativeRoom checks only the request's own purpose set, using one
// query against newznab_usage's (indexer_id, day, purpose) primary key.
// Fallbacks and rescues inherit their originating class, so they cannot
// escape its allowance. Playback and owner actions bypass these shares.
func (c *Client) speculativeRoom(ctx context.Context, kind string) error {
	purposes, share, label := warmupHitPurposes, warmupShare, "background"
	col := "hits"
	if kind == "grab" {
		purposes, col = warmupGrabPurposes, "grabs"
	}
	switch usagePurpose(ctx) {
	case core.IndexerPurposeDetail:
		purposes, share, label = detailPurposes, detailShare, "detail"
	case core.IndexerPurposeWarmup:
	case core.IndexerPurposeAvailability:
		if kind != "grab" {
			return nil
		}
	default:
		return nil
	}
	db := c.db
	if db == nil {
		return nil
	}
	var used int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(`+col+`),0) FROM newznab_usage WHERE indexer_id=? AND day=? AND purpose IN (`+purposes+`)`, c.indexerID(), c.today()).Scan(&used); err != nil {
		return err
	}
	if used >= max(1, c.dailyLimitOrAssumed(kind)*share/100) {
		return fmt.Errorf("indexer %q %s %s share: %w: %w: %w", c.Name(), label, kind, errLocalBudget, errSpeculativeShare, core.ErrBudgetExhausted)
	}
	return nil
}
