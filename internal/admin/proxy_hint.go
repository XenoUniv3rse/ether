package admin

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/identity"
)

// proxyHintLookups resolves names for the reverse-proxy hint. Tests replace it.
var proxyHintLookups = struct {
	addr func(ctx context.Context, addr string) ([]string, error)
	host func(ctx context.Context, host string) ([]netip.Addr, error)
}{
	// Both fall back to Docker's embedded DNS, which a container sharing a
	// VPN container's network (network_mode: service:x) cannot otherwise see.
	addr: identity.LookupProxyAddr,
	host: identity.LookupProxyHost,
}

// untrustedProxy is one reverse proxy Ether sees but does not trust yet.
type untrustedProxy struct {
	Address string `json:"address"`
	// Name is a host or container name that resolves back to Address
	// (Docker's DNS names a container this way), so trusting it survives
	// the container getting a new address. Empty when there is none.
	Name     string    `json:"name,omitempty"`
	Requests int64     `json:"requests"`
	LastSeen time.Time `json:"lastSeen"`
}

// handleProxyHint lists home-network peers that forward public client
// addresses without being trusted, so the console can offer to trust them.
// Nothing is trusted here: the owner adds the entry and saves.
func (h *Handler) handleProxyHint(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	access, _ := RequestAccess(r.Context())
	if !access.Owner {
		writeError(w, http.StatusForbidden, "forbidden", "owner required")
		return
	}
	sightings := identity.UntrustedForwarders(h.proxyPrefixes()...)
	if len(sightings) > 3 {
		sightings = sightings[:3]
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	out := make([]untrustedProxy, 0, len(sightings))
	for _, s := range sightings {
		out = append(out, untrustedProxy{
			Address:  s.Peer.String(),
			Name:     h.proxyHintNames.name(ctx, s.Peer),
			Requests: s.Requests,
			LastSeen: s.LastSeen.UTC(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"proxies": out})
}

// Cache even negative hints so opening or refreshing the console doesn't
// repeatedly query DNS. Hint lookups only run when an owner asks for them.
type proxyHintCache struct {
	mu      sync.Mutex
	entries map[netip.Addr]*proxyHintEntry
}

type proxyHintEntry struct {
	expires time.Time
	ready   chan struct{}
	name    string
}

func (c *proxyHintCache) name(ctx context.Context, peer netip.Addr) string {
	c.mu.Lock()
	now := time.Now()
	entry := c.entries[peer]
	first := entry == nil
	if first || !now.Before(entry.expires) {
		if c.entries == nil {
			c.entries = make(map[netip.Addr]*proxyHintEntry)
		}
		if first && len(c.entries) >= 16 {
			var oldest netip.Addr
			for addr, cached := range c.entries {
				if !oldest.IsValid() || cached.expires.Before(c.entries[oldest].expires) {
					oldest = addr
				}
			}
			delete(c.entries, oldest)
		}
		entry = &proxyHintEntry{expires: now.Add(30 * time.Second), ready: make(chan struct{})}
		c.entries[peer] = entry
		go func(e *proxyHintEntry) {
			e.name = proxyName(context.Background(), peer)
			close(e.ready)
		}(entry)
	}
	c.mu.Unlock()
	if !first {
		// Refresh in the background. Until forward confirmation comes
		// back, the hint can still offer the observed IP address.
		select {
		case <-entry.ready:
			return entry.name
		default:
			return ""
		}
	}
	select {
	case <-entry.ready:
		return entry.name
	case <-ctx.Done():
		return ""
	}
}

// proxyName returns the first label of addr's reverse DNS name when that
// label resolves back to addr. Docker answers "container.network." for a
// container's address; the bare container name is what the owner knows.
func proxyName(ctx context.Context, addr netip.Addr) string {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	names, err := proxyHintLookups.addr(ctx, addr.String())
	if err != nil {
		return ""
	}
	for _, full := range names {
		label, _, _ := strings.Cut(strings.TrimSuffix(full, "."), ".")
		if strings.ContainsAny(label, ":/") || !identity.HasProxyHostNames([]string{label}) {
			continue
		}
		addrs, err := proxyHintLookups.host(ctx, label)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if a.Unmap().WithZone("") == addr {
				return label
			}
		}
	}
	return ""
}
