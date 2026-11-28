package adapters

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
)

type cacheHintCaller interface {
	Call(context.Context, string, plugins.Request) (plugins.Response, error)
	PoolStats(string) (wasm.PoolStats, bool)
}

// DebridCacheHints asks enabled plugins with debrid_cache_hints which
// torrents a debrid provider holds (POST /ether/debrid/hints).
type DebridCacheHints struct {
	mgr    cacheHintCaller
	mu     sync.Mutex
	active map[string]bool
}

// AttachDebridCacheHints keeps the set of enabled cache-hint plugins and
// hands it to the debrid service. Disable/remove takes a plugin out at once.
func AttachDebridCacheHints(mgr *plugins.Manager, svc *debrid.Service) *DebridCacheHints {
	if mgr == nil || svc == nil {
		return nil
	}
	h := &DebridCacheHints{mgr: mgr, active: map[string]bool{}}
	mgr.OnSync(func(_ context.Context, r plugins.Record) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if r.Enabled && r.Manifest.HasCapability(plugins.CapDebridCacheHints) {
			h.active[r.ID] = true
		} else {
			delete(h.active, r.ID)
		}
	})
	svc.SetCacheHinter(h)
	return h
}

// Enabled reports whether any enabled plugin declares the capability, so
// preparation can skip the hook before reading anything.
func (h *DebridCacheHints) Enabled() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.active) > 0
}

type cacheHintWire struct {
	InfoHash      string   `json:"infoHash"`
	Private       bool     `json:"private"`
	ProviderKinds []string `json:"providerKinds"`
}

// CacheHints asks every enabled hint plugin at once and hands on each answer
// that arrives before ctx ends. A plugin whose instances are all busy is
// skipped rather than waited for; errors and error statuses are ignored.
func (h *DebridCacheHints) CacheHints(ctx context.Context, cands []debrid.CacheHintCandidate, found func(map[string][]string)) {
	if h == nil || len(cands) == 0 {
		return
	}
	h.mu.Lock()
	ids := make([]string, 0, len(h.active))
	for id := range h.active {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	sort.Strings(ids)
	wire := make([]cacheHintWire, len(cands))
	for i, c := range cands {
		wire[i] = cacheHintWire{InfoHash: c.InfoHash, Private: c.Private, ProviderKinds: c.ProviderKinds}
	}
	body, err := json.Marshal(map[string]any{"candidates": wire})
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		if st, ok := h.mgr.PoolStats(id); ok && poolBusy(st) {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			resp, err := h.mgr.Call(ctx, id, plugins.Request{Method: "POST", Path: "/ether/debrid/hints", Body: body})
			if err != nil || ctx.Err() != nil || resp.Status < 200 || resp.Status >= 300 {
				return
			}
			var out struct {
				Cached map[string][]string `json:"cached"`
			}
			if json.Unmarshal(resp.Body, &out) != nil || len(out.Cached) == 0 {
				return
			}
			answer := make(map[string][]string, len(out.Cached))
			for hash, kinds := range out.Cached {
				hash = strings.ToLower(hash)
				answer[hash] = append(answer[hash], kinds...)
			}
			found(answer)
		}(id)
	}
	wg.Wait()
}

// poolBusy reports a pool with no free or buildable instance, or a queue.
func poolBusy(st wasm.PoolStats) bool {
	return st.Size > 0 && (st.Waiting > 0 || (st.Idle == 0 && st.InUse+st.Building >= st.Size))
}
