package plugins

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
)

type runtimeCeilings struct {
	memory                  uint64
	deadline                time.Duration
	units                   int64
	poolSize                int
	fetch, member, expanded int64
	members                 int
	maxItems                int
	listItems               int
}

func defaultRuntimeCeilings() runtimeCeilings {
	return runtimeCeilings{
		memory:    2 << 30,
		deadline:  12 * time.Hour,
		units:     1 << 60,
		poolSize:  wasm.DefaultPoolCeiling,
		fetch:     8 << 30,
		member:    8 << 30,
		expanded:  32 << 30,
		members:   65535,
		maxItems:  DefaultMaxPluginItems,
		listItems: MaxListItems,
	}
}
func applyRuntimeCeilings(c runtimeCeilings, l config.PluginRuntimeLimits) runtimeCeilings {
	if l.MemoryBytes > 0 {
		c.memory = min(l.MemoryBytes, uint64(wasm.MaxMemoryBytes))
	}
	if d, err := time.ParseDuration(l.Deadline); err == nil && d > 0 {
		c.deadline = min(d, 365*24*time.Hour)
	}
	if l.UnitBudget > 0 {
		c.units = l.UnitBudget
	}
	if l.PoolSize > 0 {
		c.poolSize = min(l.PoolSize, wasm.MaxPoolSize)
	}
	if l.FetchBytes > 0 {
		c.fetch = min(l.FetchBytes, int64(1<<60))
	}
	if l.ArchiveMemberBytes > 0 {
		c.member = min(l.ArchiveMemberBytes, int64(1<<60))
	}
	if l.ArchiveExpandedBytes > 0 {
		c.expanded = min(l.ArchiveExpandedBytes, int64(1<<60))
	}
	if l.ArchiveMembers > 0 {
		c.members = min(l.ArchiveMembers, 1000000)
	}
	if l.MaxItems > 0 {
		c.maxItems = l.MaxItems
	}
	if l.ListItems > 0 {
		c.listItems = min(l.ListItems, MaxListItems)
	}
	return c
}
func (m *Manager) runtimeConfig() config.Plugins {
	if m != nil && m.deps.ReadRuntimeConfig != nil {
		return m.deps.ReadRuntimeConfig()
	}
	return config.Plugins{}
}
func (m *Manager) runtimeCeilings(id string) runtimeCeilings {
	cfg := m.runtimeConfig()
	c := applyRuntimeCeilings(defaultRuntimeCeilings(), cfg.RuntimeLimits)
	return applyRuntimeCeilings(c, cfg.RuntimeOverrides[id])
}
func (m *Manager) grantedMaxItems(man Manifest, c runtimeCeilings) int {
	n := c.maxItems
	if m != nil && m.runtimeConfig().RuntimeOverrides[man.ID].MaxItems > 0 {
		return n
	}
	if man.Ether != nil && man.Ether.Runtime.Limits != nil && man.Ether.Runtime.Limits.MaxItems > 0 {
		n = min(n, man.Ether.Runtime.Limits.MaxItems)
	}
	return n
}

// MaxPluginItems returns the effective stored-item ceiling across all content
// libraries owned by id (default 2,000,000; owner-configurable globally and per
// plugin with no upper bound).
func (m *Manager) MaxPluginItems(id string) int {
	if m == nil {
		return DefaultMaxPluginItems
	}
	c := m.runtimeCeilings(id)
	if m.store != nil && id != "" {
		if rec, err := m.store.record(context.Background(), id); err == nil {
			return m.grantedMaxItems(rec.Manifest, c)
		}
	}
	return c.maxItems
}
func runtimeLimitsWithCeilings(m Manifest, c runtimeCeilings) wasm.Limits {
	l := wasm.Limits{
		MemoryBytes: min(uint64(32<<20), c.memory),
		Deadline:    c.deadline,
		UnitBudget:  c.units,
		PoolSize:    min(wasm.DefaultPoolSize, c.poolSize),
		Stremio:     m.SpeaksStremio(),
	}
	if m.Ether != nil && m.Ether.Runtime.Limits != nil {
		r := m.Ether.Runtime.Limits
		if r.MemoryBytes > 0 {
			l.MemoryBytes = min(r.MemoryBytes, c.memory)
		}
		if d, err := time.ParseDuration(r.Deadline); err == nil && d > 0 {
			l.Deadline = min(d, c.deadline)
		}
		if r.UnitBudget > 0 {
			l.UnitBudget = min(r.UnitBudget, c.units)
		}
		if r.PoolSize > 0 {
			l.PoolSize = min(r.PoolSize, c.poolSize)
		}
	}
	return l
}
func (m *Manager) grantedPoolSize(man Manifest, c runtimeCeilings) int {
	if m != nil && m.runtimeConfig().RuntimeOverrides[man.ID].PoolSize > 0 {
		return c.poolSize
	}
	n := min(wasm.DefaultPoolSize, c.poolSize)
	if man.Ether != nil && man.Ether.Runtime.Limits != nil && man.Ether.Runtime.Limits.PoolSize > 0 {
		n = min(man.Ether.Runtime.Limits.PoolSize, c.poolSize)
	}
	return n
}

func (m *Manager) grantedRuntimeLimits(man Manifest) *RuntimeLimits {
	if man.RuntimeKind() != "hosted" {
		return nil
	}
	c := m.runtimeCeilings(man.ID)
	l := runtimeLimitsWithCeilings(man, c)
	r := &RuntimeLimits{
		MemoryBytes: l.MemoryBytes,
		ReadBytes:   wasm.DataReadBytes(l.MemoryBytes),
		Deadline:    l.Deadline.String(),
		UnitBudget:  l.UnitBudget,
		PoolSize:    m.grantedPoolSize(man, c),
	}
	r.FetchBytes = c.fetch
	r.ArchiveMemberBytes = c.member
	r.ArchiveExpandedBytes = c.expanded
	r.ArchiveMembers = c.members
	r.MaxItems = m.grantedMaxItems(man, c)
	if req := man.Ether.Runtime.Limits; req != nil {
		if req.FetchBytes > 0 {
			r.FetchBytes = min(r.FetchBytes, req.FetchBytes)
		}
		if req.ArchiveMemberBytes > 0 {
			r.ArchiveMemberBytes = min(r.ArchiveMemberBytes, req.ArchiveMemberBytes)
		}
		if req.ArchiveExpandedBytes > 0 {
			r.ArchiveExpandedBytes = min(r.ArchiveExpandedBytes, req.ArchiveExpandedBytes)
		}
		if req.ArchiveMembers > 0 {
			r.ArchiveMembers = min(r.ArchiveMembers, req.ArchiveMembers)
		}
	}
	return r
}
func (m *Manager) handleRuntimeLimits(w http.ResponseWriter, r *http.Request) {
	plugin := r.URL.Query().Get("plugin")
	if r.Method == http.MethodPut {
		var req struct {
			Plugin string                     `json:"plugin"`
			Limits config.PluginRuntimeLimits `json:"limits"`
		}
		if err := decode(r, &req); err != nil {
			m.fail(w, err)
			return
		}
		if req.Limits.MemoryBytes > wasm.MaxMemoryBytes || req.Limits.UnitBudget < 0 || req.Limits.PoolSize < 0 || req.Limits.PoolSize > wasm.MaxPoolSize || req.Limits.FetchBytes < 0 || req.Limits.ArchiveMemberBytes < 0 || req.Limits.ArchiveExpandedBytes < 0 || req.Limits.ArchiveMembers < 0 || req.Limits.ArchiveMembers > 1000000 || req.Limits.MaxItems < 0 || req.Limits.ListItems < 0 || req.Limits.ListItems > MaxListItems {
			writeErr(w, http.StatusBadRequest, "validation", fmt.Sprintf("Memory must be at most 4 GiB, instances at most %d, titles per list at most %d, and resource limits must not be negative.", wasm.MaxPoolSize, MaxListItems), nil)
			return
		}
		if req.Limits.UploadBytes < 0 || req.Limits.UploadBytes > MaxAdapterUploadBytes || (req.Plugin == "" && req.Limits.UploadBytes != 0) {
			m.fail(w, &ValidationError{Fields: []FieldError{{Field: "uploadBytes", Message: "expected a per-plugin upload limit from 0 to 512 MiB"}}})
			return
		}
		plugin = req.Plugin
		if req.Limits.DurableBytes < 0 || req.Limits.DurableFiles < 0 ||
			(plugin == "" && (req.Limits.DurableBytes != 0 || req.Limits.DurableFiles != 0)) ||
			req.Limits.DurableFreePercent < 0 || req.Limits.DurableFreePercent > 100 ||
			(plugin != "" && req.Limits.DurableFreePercent != 0) {
			writeErr(w, http.StatusBadRequest, "durable_quota_invalid", "Invalid durable storage settings.", nil)
			return
		}
		if req.Limits.Deadline != "" {
			if d, err := time.ParseDuration(req.Limits.Deadline); err != nil || d <= 0 || d > 365*24*time.Hour {
				writeErr(w, http.StatusBadRequest, "validation", "Time limit must be positive and at most one year.", nil)
				return
			}
		}
		if req.Plugin != "" {
			if _, err := m.Get(r.Context(), req.Plugin); err != nil {
				m.fail(w, err)
				return
			}
		}
		if m.deps.SaveRuntimeConfig == nil {
			m.fail(w, fmt.Errorf("runtime settings are unavailable"))
			return
		}
		m.storageMu.Lock()
		cfg := m.runtimeConfig()
		before := cfg.RuntimeLimits
		if plugin != "" {
			before = cfg.RuntimeOverrides[plugin]
		}
		after := req.Limits
		if after.DurableBytes > before.DurableBytes || after.DurableFiles > before.DurableFiles {
			capacity, _ := m.deps.DurableStorage.Capacity(cfg.RuntimeLimits.DurableFreePercent)
			if (after.DurableBytes > before.DurableBytes && after.DurableBytes > capacity.Bytes) ||
				(after.DurableFiles > before.DurableFiles && after.DurableFiles > capacity.Files) {
				m.storageMu.Unlock()
				writeErr(w, http.StatusBadRequest, "durable_quota_ceiling", "Durable quota exceeds the current volume ceiling.", nil)
				return
			}
		}
		err := m.deps.SaveRuntimeConfig(plugin, after)
		m.storageMu.Unlock()
		if err != nil {
			m.fail(w, err)
			return
		}
		// Durable quotas are consulted live and do not require stopping plugin jobs.
		if runtimeLimitsWithoutDurable(before) != runtimeLimitsWithoutDurable(after) {
			recs, err := m.List(r.Context())
			if err != nil {
				m.fail(w, err)
				return
			}
			for _, rec := range recs {
				if plugin == "" || plugin == rec.ID {
					m.closeRuntime(rec.ID)
				}
			}
		}
		m.broadcast(r.Context())
	}
	cfg := m.runtimeConfig()
	out := struct {
		config.Plugins
		DurableStorage *durableLimitsStatus `json:"durableStorage,omitempty"`
	}{Plugins: cfg}
	if plugin != "" && m.deps.DurableStorage != nil {
		rec, err := m.Get(r.Context(), plugin)
		if err != nil {
			m.fail(w, err)
			return
		}
		if allowsDurableStorage(storagePermission(rec.Manifest)) {
			store := m.deps.DurableStorage
			capacity, diskErr := store.Capacity(cfg.RuntimeLimits.DurableFreePercent)
			usage, usageErr := store.Usage(plugin)
			out.DurableStorage = &durableLimitsStatus{
				Capacity: capacity, DiskAvailable: diskErr == nil,
			}
			if usageErr == nil {
				out.DurableStorage.Usage = &usage
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func runtimeLimitsWithoutDurable(l config.PluginRuntimeLimits) config.PluginRuntimeLimits {
	l.DurableBytes, l.DurableFiles, l.DurableFreePercent = 0, 0, 0
	return l
}

type durableLimitsStatus struct {
	Capacity      blobcache.DurableCapacity `json:"capacity"`
	DiskAvailable bool                      `json:"diskAvailable"`
	Usage         *blobcache.DurableUsage   `json:"usage,omitempty"`
}

type callRecordKey struct{}

type cachedCallRecord struct {
	m      *Manager
	plugin string
	rec    Record
}

func withCallRecord(ctx context.Context, h Host) context.Context {
	ph, ok := h.(*host)
	if !ok {
		return ctx
	}
	rec, err := ph.m.store.record(ctx, ph.plugin)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, callRecordKey{}, cachedCallRecord{m: ph.m, plugin: ph.plugin, rec: rec})
}

func (h *host) callRecord(ctx context.Context) (Record, error) {
	if cached, ok := ctx.Value(callRecordKey{}).(cachedCallRecord); ok && cached.m == h.m && cached.plugin == h.plugin {
		return cached.rec, nil
	}
	return h.m.store.record(ctx, h.plugin)
}

// SourceDataLimits returns the same owner grants for a kind source download or
// extraction as a plugin-initiated fetchToCache or archive.extract operation.
func (m *Manager) SourceDataLimits(ctx context.Context, pluginID string) RuntimeLimits {
	return (&host{m: m, plugin: pluginID}).DataLimits(ctx)
}

// DataLimits exposes owner grants to shared hosted/remote data services.
func (h *host) DataLimits(ctx context.Context) RuntimeLimits {
	rec, err := h.callRecord(ctx)
	if err == nil {
		if l := h.m.grantedRuntimeLimits(rec.Manifest); l != nil {
			return *l
		}
	}
	c := h.m.runtimeCeilings(h.plugin)
	maxItems := c.maxItems
	if err == nil {
		maxItems = h.m.grantedMaxItems(rec.Manifest, c)
	}
	return RuntimeLimits{
		FetchBytes:           c.fetch,
		ArchiveMemberBytes:   c.member,
		ArchiveExpandedBytes: c.expanded,
		ArchiveMembers:       c.members,
		MaxItems:             maxItems,
		MemoryBytes:          c.memory,
		ReadBytes:            wasm.DataReadBytes(c.memory),
		Deadline:             c.deadline.String(),
	}
}
