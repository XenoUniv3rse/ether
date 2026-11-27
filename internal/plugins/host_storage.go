package plugins

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"

	"github.com/braedonsaunders/ether/internal/blobcache"
)

const DurableStoragePermissionLine = "Stores up to 1 GiB of data until deleted"

// BeginAdapterUpload reserves request-scoped staging for a declared adapter
// upload route. The proxy calls it after authenticating the viewer.
func (m *Manager) BeginAdapterUpload(ctx context.Context, id, key, subpath string, size int64) (*blobcache.Upload, error) {
	if m.deps.DurableStorage == nil {
		return nil, ErrUnavailable
	}
	r, err := m.store.record(ctx, id)
	if err != nil {
		return nil, err
	}
	if !r.Enabled || !r.Manifest.HasCapability(CapFrontendAdapter) {
		return nil, ErrPermissionDenied
	}
	if e := r.Manifest.EffectiveEther(); e != nil {
		for _, adapter := range e.Contributes.FrontendAdapters {
			if adapter.Key == key {
				if limit := adapter.UploadLimit(subpath); limit > 0 && size <= limit {
					return m.deps.DurableStorage.BeginUpload(id, size)
				}
				break
			}
		}
	}
	return nil, ErrPermissionDenied
}

func (h *host) durableWriteCheck(ctx context.Context) error {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return err
	}
	r, g, err := h.record(ctx)
	if err != nil {
		return err
	}
	return h.deny(r, g, "storage", storagePermission(r.Manifest), allowsDurableStorage)
}

func storagePermission(m Manifest) string {
	if e := m.EffectiveEther(); e != nil {
		return normLevel(e.Permissions.Storage)
	}
	return "none"
}

func allowsDurableStorage(level string) bool { return level == "durable" }

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
	case errors.Is(err, blobcache.ErrTooLarge):
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
	r, grants, err := h.record(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.deny(r, grants, "storage", storagePermission(r.Manifest), allowsDurableStorage); err != nil {
		return nil, err
	}
	s := h.m.deps.DurableStorage
	if s == nil {
		return nil, ErrUnavailable
	}
	src, err := s.Open(h.plugin, blobcache.Ref(ref))
	return src, storageError(err)
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
