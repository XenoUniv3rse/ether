package sessions

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const deferredFilePrefix = "d1_"
const deferredFileTokenLimit = 4096

type deferredFileClaims struct {
	Viewer    int64
	Reference string
	Request   json.RawMessage
	Expires   int64
}

// EnableDeferredLazyFile binds the shared key used to carry a read-only file
// offer from item detail to playback. Set it before publishing the surface.
func EnableDeferredLazyFile(streams frontend.LazyStreams, versions frontend.VersionHandles, key []byte) error {
	s, ok := streams.(*lazyStreams)
	listed, supportsListed := versions.(frontend.ListedVersionHandles)
	if !ok || !supportsListed || len(key) != 32 {
		return core.ErrNotAvailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	s.deferredFile, err = cipher.NewGCMWithRandomNonce(block)
	s.deferredVersions = listed
	return err
}

func (s *lazyStreams) sealDeferredFile(viewer int64, reference string, data []byte, expires time.Time) (string, error) {
	raw, err := json.Marshal(deferredFileClaims{Viewer: viewer, Reference: reference, Request: data, Expires: expires.UnixMilli()})
	if err != nil || base64.RawURLEncoding.EncodedLen(len(raw)+s.deferredFile.Overhead())+len(deferredFilePrefix) > deferredFileTokenLimit {
		return "", core.ErrNotAvailable
	}
	token := deferredFilePrefix + base64.RawURLEncoding.EncodeToString(s.deferredFile.Seal(nil, nil, raw, []byte(s.protocol)))
	return "/sessions/lazy/" + token + "/file", nil
}

func (s *lazyStreams) admitDeferredFile(ctx context.Context, token string) (int64, string, string, int64, error) {
	encoded, ok := strings.CutPrefix(token, deferredFilePrefix)
	if !ok || s.deferredFile == nil || len(token) > deferredFileTokenLimit {
		return 0, "", "", 0, core.ErrNotFound
	}
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return 0, "", "", 0, core.ErrNotFound
	}
	raw, err := s.deferredFile.Open(nil, nil, sealed, []byte(s.protocol))
	if err != nil {
		return 0, "", "", 0, core.ErrNotFound
	}
	var claims deferredFileClaims
	var req frontend.PlanRequest
	now := s.broker.deps.Now()
	if json.Unmarshal(raw, &claims) != nil || claims.Viewer <= 0 || claims.Expires <= now.UnixMilli() ||
		json.Unmarshal(claims.Request, &req) != nil || req.Protocol != s.protocol || req.Device.ID == "" || req.Item == 0 || req.Channel != "" {
		return 0, "", "", 0, core.ErrNotFound
	}
	if err := s.grants.ValidateGrantReference(ctx, string(s.protocol), claims.Reference, claims.Viewer); err != nil {
		return 0, "", "", 0, err
	}
	if req.Version != "" && req.Intent.ExactVersion {
		viewer, err := s.broker.deps.ActiveViewer(ctx, claims.Viewer)
		if err != nil || viewer == nil {
			return 0, "", "", 0, core.ErrUnauthorized
		}
		scope := frontend.VersionScope{Viewer: viewer, Device: req.Device, Protocol: s.protocol, Caps: req.Caps}
		if _, err := s.deferredVersions.ResolveListed(ctx, scope, req.Item, req.Version); err != nil {
			return 0, "", "", 0, err
		}
	}
	if err := s.persistCapability(ctx, token, claims.Viewer, claims.Reference, req, claims.Request, now, time.UnixMilli(claims.Expires), false, false, true); err != nil {
		return 0, "", "", 0, err
	}
	return claims.Viewer, claims.Reference, string(claims.Request), claims.Expires, nil
}
