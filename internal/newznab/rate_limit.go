package newznab

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/retryafter"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("newznab", 2, `
CREATE TABLE newznab_rate_limit (
  indexer_id TEXT PRIMARY KEY,
  retry_at INTEGER NOT NULL
);`)
}

type rateLimitState struct {
	mu     sync.Mutex
	loaded bool
	until  time.Time
}

// There is no universal Newznab account-quota discovery endpoint. Let the
// service enforce its actual allowance; after a refusal, honor Retry-After.
// When no reset time is supplied, make one new attempt after a modest pause,
// rather than assuming a daily reset time in the server's timezone.
const defaultRateLimitPause = 15 * time.Minute

// maxRateLimitPause bounds a service-requested pause: daily allowances
// reset within a day, and a bogus Retry-After (a proxy, a misconfigured
// service) must not silently disable an indexer for months.
const maxRateLimitPause = 24 * time.Hour

func retryTime(header string, now time.Time) time.Time {
	if d, ok := retryafter.Parse(header, now, maxRateLimitPause); ok {
		return now.Add(max(time.Second, d))
	}
	return now.Add(defaultRateLimitPause)
}

func (c *Client) rateLimitUntil(ctx context.Context) (time.Time, error) {
	c.rate.mu.Lock()
	defer c.rate.mu.Unlock()
	// Without a pause in force, re-read the shared row: another node of a
	// cluster may have been throttled since this one last looked.
	if c.db != nil && (!c.rate.loaded || !c.now().Before(c.rate.until)) {
		var at int64
		err := c.db.QueryRowContext(ctx, `SELECT retry_at FROM newznab_rate_limit WHERE indexer_id=?`, c.indexerID()).Scan(&at)
		if err != nil && err != sql.ErrNoRows {
			return time.Time{}, err
		}
		if err == nil {
			stored := time.Unix(at, 0)
			if limit := c.now().Add(maxRateLimitPause); stored.After(limit) {
				// A pause stored before the cap (or by a node with a wrong
				// clock): bound it here and in the shared row, which the
				// upsert would otherwise keep at its larger value.
				stored = limit
				if _, uerr := c.db.ExecContext(ctx, `UPDATE newznab_rate_limit SET retry_at=? WHERE indexer_id=? AND retry_at>?`, stored.Unix(), c.indexerID(), stored.Unix()); uerr != nil {
					c.log.Warn("could not bound stored indexer pause", "indexer", c.Name(), "err", uerr)
				}
			}
			if stored.After(c.rate.until) {
				c.rate.until = stored
			}
		}
	}
	c.rate.loaded = true
	return c.rate.until, nil
}

func (c *Client) recordRateLimit(ctx context.Context, retryAfter string, cause error) error {
	now := c.now()
	until := retryTime(retryAfter, now)
	if _, ok := retryafter.Parse(retryAfter, now, maxRateLimitPause); !ok {
		var ae *APIError
		prowlarr := strings.HasPrefix(c.indexerID(), "prowlarr-") ||
			strings.Contains(c.Config().URL, "/api/v1/indexer/") ||
			strings.HasPrefix(strings.ToLower(c.Name()), "prowlarr / ")
		if errors.As(cause, &ae) && quotaDescription(ae.Description) {
			// Prowlarr limits use a sliding hour or day. Without its
			// Retry-After the age of the oldest charged request is unknown.
			if m := quotaHours.FindStringSubmatch(ae.Description); m != nil {
				hours, _ := strconv.Atoi(m[1])
				until = now.Add(time.Duration(min(hours, 24)) * time.Hour)
			} else if prowlarr {
				until = now.Add(24 * time.Hour)
			} else {
				until = nextBudgetDay(now)
			}
		} else if prowlarr && errors.As(cause, &ae) && ae.Code == http.StatusTooManyRequests {
			until = now.Add(24 * time.Hour)
		}
	}
	ctx = context.WithoutCancel(ctx)
	c.rate.mu.Lock()
	defer c.rate.mu.Unlock()
	if c.rate.until.After(until) {
		until = c.rate.until
	}
	c.rate.loaded, c.rate.until = true, until
	if c.db != nil {
		_, err := c.db.ExecContext(ctx, `INSERT INTO newznab_rate_limit(indexer_id, retry_at) VALUES (?,?)
ON CONFLICT(indexer_id) DO UPDATE SET retry_at=CASE WHEN excluded.retry_at > newznab_rate_limit.retry_at THEN excluded.retry_at ELSE newznab_rate_limit.retry_at END`, c.indexerID(), until.Unix())
		if err != nil {
			c.log.Warn("could not persist indexer pause", "indexer", c.Name(), "err", err)
		}
	}
	c.log.Info("indexer paused by service; will retry automatically", "indexer", c.Name(), "retryAt", until.UTC())
	c.emitRateLimited(until, cause)
	return cause
}

var quotaHours = regexp.MustCompile(`(?i)\b(?:last|past) ([1-9][0-9]?) hour`)

func nextBudgetDay(now time.Time) time.Time {
	return now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
}

// Retry-After is authoritative. Otherwise read only a small quota error body
// to distinguish a daily limit from ordinary HTTP throttling.
func rateLimitResponse(resp *http.Response, now time.Time) error {
	if _, ok := retryafter.Parse(resp.Header.Get("Retry-After"), now, maxRateLimitPause); !ok {
		if reader, err := decodeBody(resp); err == nil {
			if body, err := io.ReadAll(io.LimitReader(reader, 4096)); err == nil {
				if _, err := parseFeed(body, ""); err != nil {
					var ae *APIError
					if errors.As(err, &ae) && quotaDescription(ae.Description) {
						return ae
					}
				}
			}
		}
	}
	return apiError(429, "too many requests")
}
