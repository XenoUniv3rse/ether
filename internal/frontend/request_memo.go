package frontend

import (
	"context"
	"sync"

	"github.com/braedonsaunders/ether/internal/core"
)

type requestMemoKey struct{}

// AiredMemo is one show's capped local episode page. Season rows on the same
// request share it instead of reading the show again for every season.
type AiredMemo struct {
	Items []core.Title
	Total int
	Err   error
}

// RequestMemo keeps one viewer snapshot and one visibility decision per title
// for the rest of a single request.
type RequestMemo struct {
	Mu      sync.Mutex
	Viewer  *core.User
	Visible map[core.RatingKey]bool
	Aired   map[core.RatingKey]AiredMemo
}

// WithRequestMemo installs the per-request viewer and visibility memo.
func WithRequestMemo(ctx context.Context) context.Context {
	if ctx == nil || ctx.Value(requestMemoKey{}) != nil {
		return ctx
	}
	return WithFreshRequestMemo(ctx)
}

// WithFreshRequestMemo starts an independent read within the same request.
func WithFreshRequestMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestMemoKey{}, &RequestMemo{})
}

// WithCatalogViewer records the catalogue viewer for this request. The
// context key stays owned by core.
func WithCatalogViewer(ctx context.Context, u *User) context.Context {
	return core.WithCatalogViewer(ctx, u)
}

// WithoutCatalogViewer drops the catalogue viewer, for reads that are the
// same for every viewer and share one cache entry. The context key stays
// owned by core.
func WithoutCatalogViewer(ctx context.Context) context.Context {
	return core.WithoutCatalogViewer(ctx)
}

// WithSearchMemo lets one search request read each name's people once for
// both its title and its person results. The context key stays owned by core.
func WithSearchMemo(ctx context.Context) context.Context {
	return core.WithSearchMemo(ctx)
}

// WithLiveApp records the live app name for this request. The context key
// stays owned by core.
func WithLiveApp(ctx context.Context, app string) context.Context {
	return core.WithLiveApp(ctx, app)
}

// RequestMemoFrom returns the memo installed by WithRequestMemo.
func RequestMemoFrom(ctx context.Context) *RequestMemo {
	if ctx == nil {
		return nil
	}
	memo, _ := ctx.Value(requestMemoKey{}).(*RequestMemo)
	return memo
}
