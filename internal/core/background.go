package core

import (
	"context"
	"errors"
)

// ErrForegroundOnly reports a speculative check that could not decide a
// source without work only a foreground attempt may do (adding a torrent to
// a debrid account, say). Like ErrBudgetExhausted it is never evidence of
// source absence: a foreground caller tries the source itself.
var ErrForegroundOnly = errors.New("background check cannot decide this source")

// BackgroundBudget is an optional admission hook for speculative work. Ordinary
// playback has no hook. Implementations must be concurrency safe; denied work
// returns ErrBudgetExhausted and is never evidence of source absence. A
// negative amount (from SettleBackground) returns admitted units the work
// did not use; it is never refused.
type BackgroundBudget func(ctx context.Context, resource, source string, amount int64) error

type backgroundBudgetKey struct{}

func WithBackgroundBudget(ctx context.Context, budget BackgroundBudget) context.Context {
	return context.WithValue(ctx, backgroundBudgetKey{}, budget)
}
func IsBackgroundCheck(ctx context.Context) bool { return ctx.Value(backgroundBudgetKey{}) != nil }
func SpendBackground(ctx context.Context, resource, source string, amount int64) error {
	if b, ok := ctx.Value(backgroundBudgetKey{}).(BackgroundBudget); ok {
		return b(ctx, resource, source, amount)
	}
	return nil
}

// SettleBackground settles a reservation SpendBackground admitted for
// amounts only known afterwards (NNTP BODY bytes, reserved at the
// provider's maximum article size): the part of reserved that used did not
// consume goes back to the allowance. It runs even when ctx is cancelled.
func SettleBackground(ctx context.Context, resource, source string, reserved, used int64) {
	unused := reserved - max(used, 0)
	if unused <= 0 {
		return
	}
	if b, ok := ctx.Value(backgroundBudgetKey{}).(BackgroundBudget); ok {
		_ = b(context.WithoutCancel(ctx), resource, source, -unused)
	}
}

// ReleaseFeed describes an indexer's recent-release discovery capability.
type ReleaseFeed struct {
	Categories []int  `json:"categories,omitempty"` // local filter when a feed ignores category queries
	ID         string `json:"id"`
	Name       string `json:"name"`
	Generation string `json:"-"`
	Protocol   string `json:"protocol"`
	Supported  bool   `json:"supported"`
	Reason     string `json:"reason,omitempty"`
	// DailyLimit is the indexer's own cap. Zero means unlimited.
	DailyLimit int `json:"dailyLimit,omitempty"`
	// PageMax is the page size reported by caps. Zero means unknown.
	PageMax int `json:"pageMax,omitempty"`
	// SinglePage marks a feed that lists only its latest page (a built-in
	// site's latest releases); it cannot page back to close a gap.
	SinglePage bool `json:"singlePage,omitempty"`
	// DailyFeedLimit bounds the shared quota for query-less pages.
	DailyFeedLimit int  `json:"-"`
	AnimeOnly      bool `json:"-"`
}
type foregroundReserveKey struct{}

func WithForegroundReserve(ctx context.Context, percent int) context.Context {
	return context.WithValue(ctx, foregroundReserveKey{}, percent)
}
func ForegroundReserve(ctx context.Context) int {
	n, _ := ctx.Value(foregroundReserveKey{}).(int)
	return min(100, max(0, n))
}

// InheritBackground carries admission policy into a shared worker whose lifetime
// belongs to the engine, without tying that worker to one waiter's cancellation.
func InheritBackground(dst, src context.Context) context.Context {
	if b, ok := src.Value(backgroundBudgetKey{}).(BackgroundBudget); ok {
		dst = WithBackgroundBudget(dst, b)
		dst = WithForegroundReserve(dst, ForegroundReserve(src))
	}
	return dst
}

type indexerSkipKey struct{}

// WithIndexerSkip marks the indexers (by feed source id, core.ReleaseFeed.ID)
// a search should leave out: a background probe skips indexers whose
// release feed has already shown it everything new.
func WithIndexerSkip(ctx context.Context, ids map[string]bool) context.Context {
	if len(ids) == 0 {
		return ctx
	}
	return context.WithValue(ctx, indexerSkipKey{}, ids)
}

// IndexerSkipped reports whether ctx's search leaves out the indexer id.
func IndexerSkipped(ctx context.Context, id string) bool {
	ids, _ := ctx.Value(indexerSkipKey{}).(map[string]bool)
	return ids[id]
}

type indexerStatusObserverKey struct{}

// WithIndexerStatusObserver attaches a callback notified as each enabled
// indexer finishes (or is skipped) during a streaming fan-out (SearchWith).
// Callbacks may run concurrently from multiple indexer goroutines.
func WithIndexerStatusObserver(ctx context.Context, fn func(IndexerSearchStatus)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, indexerStatusObserverKey{}, fn)
}

// HasIndexerStatusObserver reports whether ctx carries an indexer status
// observer attached by WithIndexerStatusObserver.
func HasIndexerStatusObserver(ctx context.Context) bool {
	fn, _ := ctx.Value(indexerStatusObserverKey{}).(func(IndexerSearchStatus))
	return fn != nil
}

// ObserveIndexerStatus reports a single indexer's search status to any
// observer attached to ctx.
func ObserveIndexerStatus(ctx context.Context, st IndexerSearchStatus) {
	if fn, ok := ctx.Value(indexerStatusObserverKey{}).(func(IndexerSearchStatus)); ok && fn != nil {
		fn(st)
	}
}

type torrentOnlySearchKey struct{}

// WithTorrentOnlySearch confines a fleet search to indexers that return
// torrents (Torznab, Stremio addons, torrent built-in sites), leaving Usenet
// Newznab indexers untouched when the caller only inspects torrent results.
func WithTorrentOnlySearch(ctx context.Context) context.Context {
	return context.WithValue(ctx, torrentOnlySearchKey{}, true)
}

// TorrentOnlySearch reports whether ctx's search wants only torrent indexers.
func TorrentOnlySearch(ctx context.Context) bool {
	v, _ := ctx.Value(torrentOnlySearchKey{}).(bool)
	return v
}

type unattendedSearchKey struct{}

// WithUnattendedSearch marks a search nobody is waiting on (a watchlist
// check, a preload, a next-episode prefetch): it is speculative indexer work
// (SpeculativeIndexerWork), which asks only each indexer's primary query and
// never fans out into fallback or rescue queries as Play does.
func WithUnattendedSearch(ctx context.Context) context.Context {
	return context.WithValue(ctx, unattendedSearchKey{}, true)
}

// IsUnattendedSearch reports whether nobody is waiting on ctx's search:
// marked unattended, or speculative work (a warm-up) not yet promoted to
// Play.
func IsUnattendedSearch(ctx context.Context) bool {
	if v, _ := ctx.Value(unattendedSearchKey{}).(bool); v {
		return true
	}
	return IsSpeculative(ctx)
}

// Background analysis is an internal producer role, never a request identity.
// Copy only this boolean into detached source-index work; no caller values.
type backgroundAnalysisKey struct{}

func WithBackgroundAnalysis(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundAnalysisKey{}, true)
}
func IsBackgroundAnalysis(ctx context.Context) bool {
	v, _ := ctx.Value(backgroundAnalysisKey{}).(bool)
	return v
}

type speculativeGrabsKey struct{}

// WithSpeculativeGrabs allows downloads for a job that the owner enabled,
// such as availability health verification or managed caching.
func WithSpeculativeGrabs(ctx context.Context) context.Context {
	return context.WithValue(ctx, speculativeGrabsKey{}, true)
}

func SpeculativeGrabsAllowed(ctx context.Context) bool {
	ok, _ := ctx.Value(speculativeGrabsKey{}).(bool)
	return ok
}
