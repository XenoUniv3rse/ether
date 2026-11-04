package debrid

import (
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
)

// RegisterPluginProvider joins a native provider to the same cache, file
// selection, failover, limiter and breaker pipeline as configured providers.
// A nil provider removes it. Plugin credentials remain in the plugin vault.
func (s *Service) RegisterPluginProvider(pluginID, name string, p Provider) {
	id := "plugin:" + pluginID + "/debrid"
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.plugins == nil {
		s.plugins = map[string]*entry{}
	}
	delete(s.plugins, id)
	next := make([]*entry, 0, len(s.entries)+1)
	for _, e := range s.entries {
		if e.cfg.ID != id || !strings.HasPrefix(e.cfg.Kind, "plugin:") {
			next = append(next, e)
		}
	}
	if p != nil {
		e := &entry{cfg: config.DebridProvider{ID: id, Name: name, Kind: "plugin:" + pluginID, Enabled: true, Priority: 1000}, p: p, sem: newLimiter(s.slotsFor(providerParallel))}
		s.plugins[id] = e
		next = append(next, e)
	}
	sort.SliceStable(next, func(i, j int) bool { return next[i].cfg.Priority < next[j].cfg.Priority })
	s.entries = next
	// Discard in-memory hints when a provider is replaced. Persistent cache
	// availability remains subject to the normal TTL and acquire validation.
	s.knownMu.Lock()
	for key := range s.known {
		if strings.HasPrefix(key, id+"|") {
			delete(s.known, key)
		}
	}
	s.knownMu.Unlock()
}

// Plugin links are untrusted HTTP input, even though their cache and link
// lifecycle uses the native debrid pipeline. Guard every dial and redirect.
func (p Payload) IsPluginProvider() bool { return strings.HasPrefix(p.Kind, "plugin:") }
