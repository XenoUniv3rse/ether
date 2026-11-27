package newznab

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/retryafter"
)

// GrabRefusal is a temporary refusal to download a particular release.
type GrabRefusal struct {
	RetryAt time.Time
}

func (e *GrabRefusal) Error() string { return "indexer temporarily refused this NZB grab" }
func (e *GrabRefusal) Unwrap() error { return core.ErrNotAvailable }

var grabWindow = regexp.MustCompile(`(?i)\b(?:within|in|after)\s+(?:(?:the\s+)?(?:last|past)\s+)?(\d+)\s*[- ]?\s*(seconds?|secs?|minutes?|mins?|hours?|hrs?|days?)\b`)

// ClassifyGrabRefusal recognizes duplicate-grab responses and release grab
// limits. Account-wide quota errors and unrelated conflicts remain distinct.
func ClassifyGrabRefusal(err error, now time.Time) *GrabRefusal {
	var refusal *GrabRefusal
	if errors.As(err, &refusal) {
		return refusal
	}
	var api *APIError
	if !errors.As(err, &api) {
		return nil
	}
	desc := strings.ToLower(api.Description)
	limitedRelease := strings.Contains(desc, "grab limit") &&
		(strings.Contains(desc, "this nzb") || strings.Contains(desc, "this release") ||
			(api.Code == http.StatusConflict && !strings.Contains(desc, "indexer grab limit") && !strings.Contains(desc, "daily")))
	if !strings.Contains(desc, "already grabbed") && !limitedRelease {
		return nil
	}

	pause := defaultRateLimitPause
	if parts := grabWindow.FindStringSubmatch(desc); parts != nil {
		if n, err := strconv.Atoi(parts[1]); err == nil && n > 0 {
			unit := time.Second
			switch parts[2][0] {
			case 'm':
				unit = time.Minute
			case 'h':
				unit = time.Hour
			case 'd':
				unit = 24 * time.Hour
			}
			pause = time.Duration(min(n, int(maxRateLimitPause/unit))) * unit
		}
	}
	return &GrabRefusal{RetryAt: now.Add(pause)}
}

func (e *GrabRefusal) setRetryAfter(header string, now time.Time) {
	if d, ok := retryafter.Parse(header, now, maxRateLimitPause); ok {
		e.RetryAt = now.Add(max(time.Second, d))
	}
}
