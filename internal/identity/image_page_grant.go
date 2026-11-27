package identity

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type pageArtKey struct {
	viewer   int64
	item     core.RatingKey
	language string
	rating   string
	sections string
}

type imageClaims struct {
	Viewer   int64
	Item     int64
	Channel  string
	Kind     string
	Path     string
	Tag      string
	Subject  string
	Language string
}

// SetImageKey installs the server image key for stateless grants. An empty key
// keeps the process-local key created on first use.
func (s *ImageGrantService) SetImageKey(key []byte) {
	if s == nil || len(key) == 0 {
		return
	}
	sum := sha256.Sum256(key)
	s.pageMu.Lock()
	s.pageKeyBytes = append([]byte(nil), sum[:]...)
	s.macKeyBytes = nil
	s.encBlock = nil
	s.pageMu.Unlock()
}

// SetImageGeneration invalidates cached artwork identities when the catalogue
// or config generation changes. A nil function disables that cache.
func (s *ImageGrantService) SetImageGeneration(gen func() uint64) {
	if s == nil {
		return
	}
	s.pageMu.Lock()
	s.imageGen = gen
	s.pageMu.Unlock()
}

func (s *ImageGrantService) grantKeys() ([]byte, cipher.Block, error) {
	s.pageMu.Lock()
	defer s.pageMu.Unlock()
	if len(s.pageKeyBytes) != 32 {
		var fresh [32]byte
		if _, err := rand.Read(fresh[:]); err != nil {
			return nil, nil, err
		}
		s.pageKeyBytes = fresh[:]
		s.macKeyBytes = nil
		s.encBlock = nil
	}
	if len(s.macKeyBytes) == 32 && s.encBlock != nil {
		return s.macKeyBytes, s.encBlock, nil
	}
	macH := hmac.New(sha256.New, s.pageKeyBytes)
	_, _ = macH.Write([]byte("ether-image-grant-mac-v2"))
	macKey := macH.Sum(nil)

	encH := hmac.New(sha256.New, s.pageKeyBytes)
	_, _ = encH.Write([]byte("ether-image-grant-enc-v2"))
	encKey := encH.Sum(nil)
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, nil, err
	}
	s.macKeyBytes = macKey
	s.encBlock = block
	return s.macKeyBytes, s.encBlock, nil
}

func validateImageClaims(c imageClaims) bool {
	if c.Viewer <= 0 || c.Item < 0 {
		return false
	}
	if strings.HasPrefix(c.Path, frontend.LibraryLiveThumbPrefix) || strings.HasPrefix(c.Path, frontend.PluginImagePrefix) {
		if c.Item != 0 || c.Channel != "" || c.Subject == "" || c.Kind == frontend.ImagePerson {
			return false
		}
	} else if strings.HasPrefix(c.Path, frontend.ViewerAvatarPrefix) {
		if c.Item != 0 || c.Channel != "" || c.Kind != frontend.ImagePerson || c.Tag == "" || c.Subject == "" {
			return false
		}
	} else if (c.Item == 0) == (c.Channel == "") {
		return false
	}
	if len(c.Channel) > 512 || (c.Channel != "" && c.Kind == frontend.ImagePerson) {
		return false
	}
	if c.Kind == "" || len(c.Kind) > 128 || c.Path == "" || len(c.Path) > 1024 {
		return false
	}
	if len(c.Tag) > 256 || len(c.Subject) > 256 || len(c.Language) > 128 {
		return false
	}
	return true
}

func encodeImageClaims(c imageClaims) []byte {
	size := 2*binary.MaxVarintLen64 + 6*binary.MaxVarintLen64 + len(c.Channel) + len(c.Kind) + len(c.Path) + len(c.Tag) + len(c.Subject) + len(c.Language)
	out := make([]byte, 0, size)
	out = binary.AppendUvarint(out, uint64(c.Viewer))
	out = binary.AppendUvarint(out, uint64(c.Item))
	for _, field := range []string{c.Channel, c.Kind, c.Path, c.Tag, c.Subject, c.Language} {
		out = binary.AppendUvarint(out, uint64(len(field)))
		out = append(out, field...)
	}
	return out
}

func decodeImageClaims(plain []byte) (imageClaims, bool) {
	var c imageClaims
	viewer, n := binary.Uvarint(plain)
	if n <= 0 || viewer == 0 || viewer > 1<<62 {
		return imageClaims{}, false
	}
	c.Viewer = int64(viewer)
	pos := n

	item, n := binary.Uvarint(plain[pos:])
	if n <= 0 || item > 1<<62 {
		return imageClaims{}, false
	}
	c.Item = int64(item)
	pos += n

	fields := []*string{&c.Channel, &c.Kind, &c.Path, &c.Tag, &c.Subject, &c.Language}
	for _, dst := range fields {
		length, m := binary.Uvarint(plain[pos:])
		if m <= 0 || length > 1024 || pos+m+int(length) > len(plain) {
			return imageClaims{}, false
		}
		pos += m
		*dst = string(plain[pos : pos+int(length)])
		pos += int(length)
	}
	if pos != len(plain) || !validateImageClaims(c) {
		return imageClaims{}, false
	}
	return c, true
}

func computeImageMAC(macKey []byte, surface string, plain []byte) [32]byte {
	mac := hmac.New(sha256.New, macKey)
	var lenBuf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(lenBuf[:], uint64(len(surface)))
	_, _ = mac.Write(lenBuf[:n])
	_, _ = mac.Write([]byte(surface))
	_, _ = mac.Write(plain)
	var sum [32]byte
	copy(sum[:], mac.Sum(nil))
	return sum
}

func sealImageGrant(macKey []byte, encBlock cipher.Block, surface string, c imageClaims) (string, error) {
	if !validateImageClaims(c) {
		return "", core.ErrUnauthorized
	}
	plain := encodeImageClaims(c)
	tag := computeImageMAC(macKey, surface, plain)
	out := make([]byte, 32+len(plain))
	copy(out[:32], tag[:])
	cipher.NewCTR(encBlock, tag[:16]).XORKeyStream(out[32:], plain)
	return base64.RawURLEncoding.EncodeToString(out), nil
}

func openImageGrant(macKey []byte, encBlock cipher.Block, surface, secret string) (imageClaims, error) {
	if len(secret) == 0 || len(secret) > 4096 {
		return imageClaims{}, core.ErrUnauthorized
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(secret)
	if err != nil || len(raw) <= 32 || base64.RawURLEncoding.EncodeToString(raw) != secret {
		return imageClaims{}, core.ErrUnauthorized
	}
	gotMAC := raw[:32]
	plain := make([]byte, len(raw)-32)
	cipher.NewCTR(encBlock, gotMAC[:16]).XORKeyStream(plain, raw[32:])
	wantMAC := computeImageMAC(macKey, surface, plain)
	if !hmac.Equal(gotMAC, wantMAC[:]) {
		return imageClaims{}, core.ErrUnauthorized
	}
	claims, ok := decodeImageClaims(plain)
	if !ok {
		return imageClaims{}, core.ErrUnauthorized
	}
	return claims, nil
}

func (s *ImageGrantService) sealClaims(c imageClaims) (string, error) {
	macKey, encBlock, err := s.grantKeys()
	if err != nil {
		return "", core.ErrNotAvailable
	}
	return sealImageGrant(macKey, encBlock, string(s.surface), c)
}

func (s *ImageGrantService) openClaims(secret string) (imageClaims, error) {
	macKey, encBlock, err := s.grantKeys()
	if err != nil {
		return imageClaims{}, core.ErrNotAvailable
	}
	return openImageGrant(macKey, encBlock, string(s.surface), secret)
}

// SignPage seals artwork for a page of titles using the single deterministic
// stateless image grant scheme, writing zero rows. Serve rechecks viewer
// activation and current artwork policy before returning bytes.
func (s *ImageGrantService) SignPage(ctx context.Context, req frontend.ImagePageRequest) ([]frontend.ImageBatchItem, error) {
	if len(req.Titles) > 10000 || len(req.Kinds) > 6 {
		return nil, frontend.ErrImageBatchLimit
	}
	kinds := req.Kinds
	if len(kinds) == 0 {
		kinds = []string{frontend.ImagePoster}
	}
	wanted := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		switch kind {
		case frontend.ImagePoster, frontend.ImageBackdrop, frontend.ImageLogo, frontend.ImageThumb, frontend.ImageBanner, frontend.ImageStill:
			wanted[kind] = true
		default:
			return nil, frontend.ErrImageBatchLimit
		}
	}
	viewer, _, err := s.resolveCredential(ctx, req.Credential)
	if err != nil {
		return nil, err
	}
	macKey, encBlock, err := s.grantKeys()
	if err != nil {
		return nil, core.ErrNotAvailable
	}
	ctx = frontend.WithArtworkRows(ctx)
	if !req.IncludeVariants {
		ctx = frontend.WithPrimaryArtworkOnly(ctx)
	}
	experience := s.experience()
	if page, ok := experience.(frontend.ItemArtworkPage); ok {
		keys := make([]core.RatingKey, 0, len(req.Titles))
		seen := map[core.RatingKey]bool{}
		for _, title := range req.Titles {
			if title.Key == 0 || seen[title.Key] {
				continue
			}
			seen[title.Key] = true
			keys = append(keys, title.Key)
		}
		listed, err := page.ItemArtworkPage(frontend.WithArtworkKinds(ctx, wanted), viewer, req.Device, keys)
		if err != nil {
			return nil, err
		}
		out := make([]frontend.ImageBatchItem, 0, len(keys))
		for _, key := range keys {
			images, ok := listed[key]
			if !ok {
				continue
			}
			if len(req.Ranked) > 0 {
				images = rankedPageImages(images, req.Ranked[key], req.RankedWide)
			}
			item, err := sealTitleBatchItem(macKey, encBlock, string(s.surface), viewer.ID, req.Device.Language, key, images, true, wanted, req.IncludeVariants)
			if err != nil {
				return nil, err
			}
			out = append(out, item)
		}
		return out, nil
	}
	out := make([]frontend.ImageBatchItem, 0, len(req.Titles))
	seen := map[core.RatingKey]bool{}
	for _, title := range req.Titles {
		if title.Key == 0 {
			return nil, frontend.ErrImageBatchLimit
		}
		if seen[title.Key] {
			continue
		}
		seen[title.Key] = true
		images, complete, err := s.pageImages(ctx, viewer, req.Device, title.Key)
		if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnauthorized) {
			continue
		}
		if errors.Is(err, core.ErrNotAvailable) {
			out = append(out, frontend.ImageBatchItem{Item: title.Key})
			continue
		}
		if err != nil {
			return nil, err
		}
		item, err := sealTitleBatchItem(macKey, encBlock, string(s.surface), viewer.ID, req.Device.Language, title.Key, rankedPageImages(images, req.Ranked[title.Key], req.RankedWide), complete, wanted, req.IncludeVariants)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func sealTitleBatchItem(macKey []byte, encBlock cipher.Block, surface string, viewer int64, language string, key core.RatingKey, images []frontend.Image, complete bool, wanted map[string]bool, includeVariants bool) (frontend.ImageBatchItem, error) {
	item := frontend.ImageBatchItem{Item: key, Authoritative: complete}
	if !complete {
		return item, nil
	}
	chosen := map[string]bool{}
	for _, image := range images {
		if !wanted[image.Kind] || (!includeVariants && chosen[image.Kind]) || image.Path == "" {
			continue
		}
		claims := imageClaims{
			Viewer:   viewer,
			Item:     int64(key),
			Kind:     image.Kind,
			Path:     image.Path,
			Tag:      image.Tag,
			Subject:  image.Subject,
			Language: language,
		}
		if !validateImageClaims(claims) {
			return frontend.ImageBatchItem{}, core.ErrNotAvailable
		}
		secret, err := sealImageGrant(macKey, encBlock, surface, claims)
		if err != nil {
			return frontend.ImageBatchItem{}, err
		}
		chosen[image.Kind] = true
		item.Images = append(item.Images, frontend.GrantedImage{Image: image, Secret: secret})
	}
	return item, nil
}

func (s *ImageGrantService) pageImages(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) ([]frontend.Image, bool, error) {
	s.pageMu.Lock()
	genFn := s.imageGen
	s.pageMu.Unlock()
	gen := uint64(0)
	if genFn != nil {
		gen = genFn()
	}
	cacheKey := pageArtKey{viewer: viewer.ID, item: key, language: device.Language, rating: viewer.MaxContentRating, sections: sectionsKey(viewer.AllowedLibraries)}
	if genFn != nil {
		s.artMu.Lock()
		if s.art != nil && s.artGen == gen {
			if images, ok := s.art[cacheKey]; ok {
				s.artMu.Unlock()
				return images, true, nil
			}
		}
		s.artMu.Unlock()
	}
	experience := s.experience()
	if experience == nil {
		return nil, false, core.ErrNotAvailable
	}
	var images []frontend.Image
	var complete bool
	var err error
	if scoped, ok := experience.(frontend.ItemArtwork); ok {
		images, complete, err = scoped.ItemArtwork(ctx, viewer, device, key)
	} else {
		var detail frontend.ItemDetail
		detail, err = experience.Item(ctx, frontend.ItemRequest{Viewer: viewer, Device: device, Key: key})
		images, complete = detail.Images, detail.ImagesAuthoritative
	}
	if err != nil || !complete || genFn == nil {
		return images, complete, err
	}
	s.artMu.Lock()
	if s.art == nil || s.artGen != gen {
		s.art = map[pageArtKey][]frontend.Image{}
		s.artGen = gen
	}
	s.art[cacheKey] = images
	s.artMu.Unlock()
	return images, complete, nil
}

func sectionsKey(sections []core.LibraryID) string {
	if len(sections) == 0 {
		return ""
	}
	b := make([]byte, 0, len(sections)*4)
	var buf [4]byte
	for _, section := range sections {
		binary.BigEndian.PutUint32(buf[:], uint32(section))
		b = append(b, buf[:]...)
	}
	return string(b)
}

func urlImageKinds(r *http.Request) []string {
	if r == nil || r.URL == nil {
		return nil
	}
	const marker = "/Images/"
	path := r.URL.Path
	i := strings.LastIndex(path, marker)
	if i < 0 {
		return nil
	}
	rest := path[i+len(marker):]
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		rest = rest[:slash]
	}
	switch strings.ToLower(rest) {
	case "primary":
		return []string{frontend.ImagePoster, "primary", frontend.ImageStill}
	case "thumb":
		return []string{frontend.ImageThumb, frontend.ImageStill}
	case "backdrop":
		return []string{frontend.ImageBackdrop}
	case "logo":
		return []string{frontend.ImageLogo}
	case "banner":
		return []string{frontend.ImageBanner}
	default:
		return nil
	}
}

func extractItemsSegment(path string) (string, bool) {
	const marker = "/items/"
	i := strings.Index(strings.ToLower(path), marker)
	if i < 0 {
		return "", false
	}
	rest := path[i+len(marker):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return "", false
	}
	return rest[:slash], true
}

func extractUsersImageSegment(path string) (string, bool) {
	lower := strings.ToLower(path)
	const marker = "/users/"
	i := strings.Index(lower, marker)
	if i < 0 {
		return "", false
	}
	rest := path[i+len(marker):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return "", false
	}
	if !strings.HasPrefix(lower[i+len(marker)+slash:], "/images/") {
		return "", false
	}
	return rest[:slash], true
}

func isUserImagePath(path string) bool {
	lower := strings.ToLower(path)
	return lower == "/userimage" || strings.HasSuffix(lower, "/userimage")
}

func claimMatchesURL(claims imageClaims, r *http.Request) bool {
	if r == nil || r.URL == nil {
		return true
	}
	isViewerAvatar := strings.HasPrefix(claims.Path, frontend.ViewerAvatarPrefix)
	if seg, ok := extractUsersImageSegment(r.URL.Path); ok {
		if !isViewerAvatar {
			return false
		}
		uid, err := ids.ParseGUID(ids.User, seg)
		return err == nil && uid == claims.Viewer
	}
	if isUserImagePath(r.URL.Path) {
		if !isViewerAvatar {
			return false
		}
		for k, vs := range r.URL.Query() {
			if strings.EqualFold(k, "userid") {
				if len(vs) != 1 {
					return false
				}
				uid, err := ids.ParseGUID(ids.User, vs[0])
				if err != nil || uid != claims.Viewer {
					return false
				}
			}
		}
		return true
	}
	if isViewerAvatar {
		if _, ok := extractItemsSegment(r.URL.Path); ok {
			return false
		}
		return true
	}
	if seg, ok := extractItemsSegment(r.URL.Path); ok {
		if claims.Channel != "" {
			if seg == claims.Channel {
				return true
			}
			canonical := strings.ToLower(strings.ReplaceAll(seg, "-", ""))
			if want, err := ids.StringGUID(ids.Channel, claims.Channel); err == nil && canonical == want {
				return true
			}
			if claims.Subject != "" {
				if want, err := ids.StringGUID(ids.Programme, claims.Subject); err == nil && canonical == want {
					return true
				}
			}
			return false
		}
		if _, err := ids.ParseGUID(ids.Channel, seg); err == nil {
			return false
		}
	}
	if claims.Channel == "" && claims.Kind != frontend.ImagePerson {
		if want := urlImageKinds(r); len(want) > 0 {
			matched := false
			for _, kind := range want {
				if strings.EqualFold(kind, claims.Kind) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	}
	return true
}

func rankedPageImages(images []frontend.Image, rank int, wide bool) []frontend.Image {
	if rank < 1 || rank > 10 {
		return images
	}
	out := make([]frontend.Image, 0, len(images)+1)
	hasPoster := false
	for _, img := range images {
		if img.Kind == frontend.ImagePoster {
			hasPoster = true
			break
		}
	}
	for _, img := range images {
		if hasPoster && img.Kind == frontend.ImageThumb {
			continue
		}
		out = append(out, frontend.RankedArtwork(img, rank, wide))
		if img.Kind == frontend.ImagePoster {
			out = append(out, frontend.RankedCardThumb(img, rank))
		}
	}
	return out
}

var _ frontend.ImagePageSigner = (*ImageGrantService)(nil)
