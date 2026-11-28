package newznab

import (
	"context"
	"time"
)

// Fresh titles can gain releases quickly. Stable titles use the owner's
// full cache duration; recent empty answers expire sooner still.
const (
	cacheFreshAge  = 30 * 24 * time.Hour
	cacheOldAge    = 365 * 24 * time.Hour
	freshResultTTL = 90 * time.Minute
	emptyFreshAge  = 7 * 24 * time.Hour
	emptyStableTTL = 30 * time.Minute
)

type cacheAgeKey struct{}

// withCacheAge carries the searched title's release time to cachePut.
func withCacheAge(ctx context.Context, released time.Time) context.Context {
	if released.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, cacheAgeKey{}, released)
}

func cacheAge(ctx context.Context) time.Time {
	t, _ := ctx.Value(cacheAgeKey{}).(time.Time)
	return t
}

// searchCacheTTL is how long to keep a search's results, from the client's
// base lifetime and the title's release time (zero: unknown, base rules).
func searchCacheTTL(base time.Duration, released, now time.Time, empty bool) time.Duration {
	if base <= 0 {
		return base
	}
	if released.IsZero() || released.After(now) {
		if empty {
			return min(base, emptySearchCacheTTL)
		}
		return base
	}
	age := now.Sub(released)
	if empty {
		if age < emptyFreshAge {
			return min(base, emptySearchCacheTTL)
		}
		return min(base, emptyStableTTL)
	}
	fresh := min(base, freshResultTTL)
	if age <= cacheFreshAge {
		return fresh
	}
	if age >= cacheOldAge {
		return base
	}
	frac := float64(age-cacheFreshAge) / float64(cacheOldAge-cacheFreshAge)
	return fresh + time.Duration(frac*float64(base-fresh))
}
