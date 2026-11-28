package plugins

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/blobcache"
)

const DurableStoragePermissionLine = "Stores saved data until deleted"

// BeginAdapterUpload reserves request-scoped staging for a declared adapter
// upload route and returns the effective limit for the HTTP body reader.
// The proxy calls it after authenticating the viewer.
func (m *Manager) BeginAdapterUpload(ctx context.Context, id, key, subpath string, size int64) (*blobcache.Upload, int64, error) {
	if m.deps.DurableStorage == nil {
		return nil, 0, ErrUnavailable
	}
	r, err := m.store.record(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	if !r.Enabled || !r.Manifest.HasCapability(CapFrontendAdapter) {
		return nil, 0, ErrPermissionDenied
	}
	if e := r.Manifest.EffectiveEther(); e != nil {
		for _, adapter := range e.Contributes.FrontendAdapters {
			if adapter.Key != key {
				continue
			}
			limit := min(adapter.UploadLimit(subpath), adapterUploadCeiling(m.runtimeConfig().RuntimeOverrides[id].UploadBytes))
			if limit == 0 {
				break
			}
			if size > limit {
				return nil, 0, blobcache.ErrTooLarge
			}
			if size <= 0 {
				size = limit
			}
			upload, err := m.deps.DurableStorage.BeginUpload(id, size)
			return upload, limit, err
		}
	}
	return nil, 0, ErrPermissionDenied
}

func adapterUploadCeiling(configured int64) int64 {
	if configured <= 0 {
		return DefaultAdapterUploadBytes
	}
	return min(configured, MaxAdapterUploadBytes)
}

// Remote host callbacks can arrive without the viewer token. Track active
// interactive calls by plugin so a sidecar cannot accidentally hash a large
// blob while answering one of those calls.
func (m *Manager) trackRemoteClientCall(man Manifest, path string) func() {
	if man.RuntimeKind() != "remote" || !(isClientRequestPath(path) || isFrontendAdapterPath(path)) {
		return func() {}
	}
	m.remoteClientMu.Lock()
	if m.activeRemoteClients == nil {
		m.activeRemoteClients = make(map[string]int)
	}
	m.activeRemoteClients[man.ID]++
	m.remoteClientMu.Unlock()
	return sync.OnceFunc(func() {
		m.remoteClientMu.Lock()
		if m.activeRemoteClients[man.ID] == 1 {
			delete(m.activeRemoteClients, man.ID)
		} else {
			m.activeRemoteClients[man.ID]--
		}
		m.remoteClientMu.Unlock()
	})
}

func (m *Manager) remoteClientActive(pluginID string) bool {
	m.remoteClientMu.Lock()
	defer m.remoteClientMu.Unlock()
	return m.activeRemoteClients[pluginID] > 0
}

// CacheStat retrieves the digests of an owned blob. Only small cold reads are
// admitted on viewer paths, and cached or staged answers require no hashing.
func (h *host) CacheStat(ctx context.Context, ref string) (blobcache.Stat, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	src, err := h.cacheOpen(ctx, ref, true)
	if err != nil {
		if errors.Is(err, blobcache.ErrNotFound) {
			err = ErrNotFound
		}
		return blobcache.Stat{}, err
	}
	defer src.Close()
	maxBytes := blobcache.MaxStatBytes
	path := callPathFromContext(ctx)
	_, viewer := ViewerFromContext(ctx)
	if viewer || isClientRequestPath(path) ||
		(path != "" && !h.allowsLongFetchToCache(ctx)) ||
		(path == "" && h.m.remoteClientActive(h.plugin)) {
		maxBytes = blobcache.InteractiveStatBytes
	}
	stat, err := blobcache.StatOwned(ctx, blobcache.Ref(ref), src, maxBytes)
	return stat, storageError(err)
}

func (h *host) durableWriteCheck(ctx context.Context) error {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return err
	}
	return h.durableReadCheck(ctx)
}

func (h *host) durableReadCheck(ctx context.Context) error {
	r, g, err := h.record(ctx)
	if err != nil {
		return err
	}
	return h.deny(r, g, "storage", storagePermission(r.Manifest), allowsDurableStorage)
}

func storagePermission(m Manifest) string {
	if e := m.EffectiveEther(); e != nil {
		return normLevel(string(e.Permissions.Storage))
	}
	return "none"
}

func allowsDurableStorage(level string) bool { return level == "durable" }

func (m *Manager) grantedDurableQuota(man Manifest) *blobcache.DurableQuota {
	s := m.deps.DurableStorage
	if s == nil || !allowsDurableStorage(storagePermission(man)) {
		return nil
	}
	quota := s.Quota(man.ID)
	return &quota
}

func (h *host) cachePutDurable(ctx context.Context, name string, body []byte, fromBlob string) (string, error) {
	h.m.storageMu.RLock()
	defer h.m.storageMu.RUnlock()
	if err := h.durableWriteCheck(ctx); err != nil {
		return "", err
	}
	s := h.m.deps.DurableStorage
	if s == nil {
		return "", ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if fromBlob != "" {
		ref, err := s.Promote(h.plugin, blobcache.Ref(fromBlob))
		return ref.String(), storageError(err)
	}
	upload, err := s.BeginUpload(h.plugin, int64(len(body)))
	if err != nil {
		return "", storageError(err)
	}
	defer upload.Close()
	ref, _, err := upload.Stage(ctx, name, "", bytes.NewReader(body))
	if err != nil {
		return "", storageError(err)
	}
	out, err := s.Promote(h.plugin, ref)
	return out.String(), storageError(err)
}

func storageError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, blobcache.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, blobcache.ErrQuotaFull), errors.Is(err, blobcache.ErrTooLarge):
		return ErrQuotaExceeded
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return ErrUnavailable
	}
}

type cachePutOptions struct {
	Durable  bool   `json:"durable"`
	FromBlob string `json:"fromBlob"`
}

func callCachePut(ctx context.Context, h Host, args []json.RawMessage) (map[string]string, error) {
	bad := func() error {
		return &ValidationError{Fields: []FieldError{{Field: "cache.put", Message: "expected (libraryKey, name, data[, {durable, fromBlob}])"}}}
	}
	var library, name string
	var opts cachePutOptions
	if len(args) < 3 || len(args) > 4 ||
		json.Unmarshal(args[0], &library) != nil || json.Unmarshal(args[1], &name) != nil ||
		(len(args) == 4 && json.Unmarshal(args[3], &opts) != nil) {
		return nil, bad()
	}
	if (opts.Durable && library != "") || (opts.FromBlob != "" && (!opts.Durable || string(args[2]) != "null")) {
		return nil, bad()
	}
	var body []byte
	if opts.FromBlob != "" {
		if err := validateBlobRef(opts.FromBlob); err != nil {
			return nil, err
		}
	} else {
		var plain string
		if json.Unmarshal(args[2], &plain) == nil {
			body = []byte(plain)
		} else {
			var data struct {
				Body       string `json:"body"`
				BodyBase64 string `json:"bodyBase64"`
			}
			if json.Unmarshal(args[2], &data) != nil {
				return nil, bad()
			}
			body = []byte(data.Body)
			if data.BodyBase64 != "" {
				var err error
				body, err = base64.StdEncoding.DecodeString(data.BodyBase64)
				if err != nil {
					return nil, bad()
				}
			}
		}
	}
	var ref string
	var err error
	if opts.Durable {
		service, ok := h.(interface {
			cachePutDurable(context.Context, string, []byte, string) (string, error)
		})
		if !ok {
			return nil, ErrUnavailable
		}
		ref, err = service.cachePutDurable(ctx, name, body, opts.FromBlob)
	} else {
		ref, err = h.CachePut(ctx, library, name, bytes.NewReader(body))
	}
	return map[string]string{"blob": ref}, err
}

func (h *host) openDurableBlob(ctx context.Context, ref string) (io.ReadSeekCloser, error) {
	if err := h.durableReadCheck(ctx); err != nil {
		return nil, err
	}
	s := h.m.deps.DurableStorage
	if s == nil {
		return nil, ErrUnavailable
	}
	src, err := s.Open(h.plugin, blobcache.Ref(ref))
	return src, storageError(err)
}

// CacheUsage reports saved blob usage for this plugin's namespace.
func (h *host) CacheUsage(ctx context.Context) (blobcache.DurableUsage, error) {
	if err := h.durableReadCheck(ctx); err != nil {
		return blobcache.DurableUsage{}, err
	}
	s := h.m.deps.DurableStorage
	if s == nil {
		return blobcache.DurableUsage{}, ErrUnavailable
	}
	out, err := s.Usage(h.plugin)
	return out, storageError(err)
}

func (h *host) deleteDurableBlob(ctx context.Context, ref string) error {
	h.m.storageMu.RLock()
	defer h.m.storageMu.RUnlock()
	if err := h.durableWriteCheck(ctx); err != nil {
		return err
	}
	s := h.m.deps.DurableStorage
	if s == nil {
		return ErrUnavailable
	}
	_, err := s.Delete(h.plugin, blobcache.Ref(ref))
	return storageError(err)
}
