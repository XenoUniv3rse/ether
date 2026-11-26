package images

import (
	"context"
	"sync/atomic"
)

type VariantCache interface {
	Variant(context.Context, []byte, string, string, func(context.Context) ([]byte, string, error)) ([]byte, string, error)
}

type cachePolicyKey struct{}
type cachePolicy struct{ noStore atomic.Bool }

// TrackCachePolicy gives a renderer a request-local way to pass an upstream
// no-store directive through intermediate image transforms.
func TrackCachePolicy(ctx context.Context) context.Context {
	if _, ok := ctx.Value(cachePolicyKey{}).(*cachePolicy); ok {
		return ctx
	}
	return context.WithValue(ctx, cachePolicyKey{}, &cachePolicy{})
}

func MarkNoStore(ctx context.Context) {
	if policy, ok := ctx.Value(cachePolicyKey{}).(*cachePolicy); ok {
		policy.noStore.Store(true)
	}
}

func NoStore(ctx context.Context) bool {
	policy, _ := ctx.Value(cachePolicyKey{}).(*cachePolicy)
	return policy != nil && policy.noStore.Load()
}
