package core

import (
	"context"
	"sync"
)

type searchMemoKey struct{}

// SearchMemo shares one search request's person-name matches between the
// paths that read them: a search's title results merge the titles of the
// people it names, and its person results list those same people.
type SearchMemo struct {
	mu sync.Mutex
	m  map[string]any
}

// WithSearchMemo installs a memo for the rest of one search request.
func WithSearchMemo(ctx context.Context) context.Context {
	if ctx == nil || ctx.Value(searchMemoKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, searchMemoKey{}, &SearchMemo{m: map[string]any{}})
}

// SearchMemoFrom returns the request's memo, or nil outside a search request.
func SearchMemoFrom(ctx context.Context) *SearchMemo {
	if ctx == nil {
		return nil
	}
	m, _ := ctx.Value(searchMemoKey{}).(*SearchMemo)
	return m
}

func (m *SearchMemo) Load(key string) (any, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.m[key]
	return v, ok
}

func (m *SearchMemo) Store(key string, v any) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.m[key] = v
	m.mu.Unlock()
}

// LoadOrStore returns the shared value for key, publishing candidate when absent.
func (m *SearchMemo) LoadOrStore(key string, candidate any) any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if value, ok := m.m[key]; ok {
		return value
	}
	m.m[key] = candidate
	return candidate
}

type searchPeopleLimitKey struct{}

// WithSearchPeopleLimit records the largest person-name window used by a search.
func WithSearchPeopleLimit(ctx context.Context, limit int) context.Context {
	return context.WithValue(ctx, searchPeopleLimitKey{}, limit)
}

func SearchPeopleLimit(ctx context.Context) int {
	limit, _ := ctx.Value(searchPeopleLimitKey{}).(int)
	return limit
}
