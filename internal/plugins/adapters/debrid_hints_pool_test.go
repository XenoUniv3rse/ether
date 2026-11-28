package adapters

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
)

type poolCaller struct {
	mu     sync.Mutex
	called []string
	pools  map[string]wasm.PoolStats
}

func (p *poolCaller) Call(_ context.Context, id string, _ plugins.Request) (plugins.Response, error) {
	p.mu.Lock()
	p.called = append(p.called, id)
	p.mu.Unlock()
	return plugins.Response{Status: 200, Body: []byte(`{"cached":{}}`)}, nil
}

func (p *poolCaller) PoolStats(id string) (wasm.PoolStats, bool) {
	s, ok := p.pools[id]
	return s, ok
}

// A plugin whose instances are all busy is skipped rather than waited for;
// one with a free or buildable instance, or no pool at all, is asked.
func TestDebridCacheHintsSkipsBusyPools(t *testing.T) {
	caller := &poolCaller{pools: map[string]wasm.PoolStats{
		"busy":    {Size: 2, InUse: 2},
		"queued":  {Size: 2, InUse: 1, Idle: 1, Waiting: 1},
		"idle":    {Size: 2, InUse: 1, Idle: 1},
		"growing": {Size: 2, InUse: 1},
	}}
	h := &DebridCacheHints{mgr: caller, active: map[string]bool{"busy": true, "queued": true, "idle": true, "growing": true, "remote": true}}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	h.CacheHints(ctx, []debrid.CacheHintCandidate{{InfoHash: "abc", ProviderKinds: []string{"realdebrid"}}}, func(map[string][]string) {})
	slices.Sort(caller.called)
	if want := []string{"growing", "idle", "remote"}; !slices.Equal(caller.called, want) {
		t.Fatalf("called %v, want %v", caller.called, want)
	}
}
