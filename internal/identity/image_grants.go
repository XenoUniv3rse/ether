package identity

import (
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/authhdr"
	kitimages "github.com/braedonsaunders/ether/internal/frontend/kit/images"
	"github.com/braedonsaunders/ether/internal/reqmemo"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("frontend_image_grants", 1, `CREATE TABLE frontend_image_grants (hash TEXT PRIMARY KEY, source_hash TEXT NOT NULL REFERENCES identity_grants(hash) ON DELETE CASCADE, viewer_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE, surface TEXT NOT NULL, item INTEGER NOT NULL, device TEXT NOT NULL, image_kind TEXT NOT NULL, image_path TEXT NOT NULL, image_tag TEXT NOT NULL, expires INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0); CREATE INDEX frontend_image_grants_source ON frontend_image_grants(source_hash);`)
	store.RegisterAfter("frontend_image_grants", "auth", "identity_grants")
	store.Register("frontend_image_grants", 2, `ALTER TABLE frontend_image_grants ADD COLUMN channel TEXT NOT NULL DEFAULT '';`)
	store.Register("frontend_image_grants", 3, `ALTER TABLE frontend_image_grants ADD COLUMN image_subject TEXT NOT NULL DEFAULT '';`)
	store.Register("frontend_image_grants", 4, `CREATE INDEX frontend_image_grants_scope_expiry ON frontend_image_grants(viewer_id,surface,expires,hash);`)
	store.Register("frontend_image_grants", 5, `CREATE INDEX frontend_image_grants_expiry ON frontend_image_grants(expires,hash);`)
	// Historical migration step before frontend_image_grants v8 retired the table.
	store.Register("frontend_image_grants", 6, `CREATE INDEX frontend_image_grants_shard_expiry ON frontend_image_grants((viewer_id % 64),expires,hash);`)
}

type ImageSource interface {
	Get(context.Context, string, int) ([]byte, string, error)
}

// ImageGrantOptions supplies adapter-owned credential parsing and public URL policy.
type ImageGrantOptions struct {
	Token             func(*http.Request) string
	PublicContentTags bool
	LibraryLiveThumbs frontend.LibraryLiveThumbAuthorizer
	// PluginImages authorizes plugin-named image URLs (frontend.PluginImagePrefix).
	PluginImages frontend.PluginImageAuthorizer
}

type ImageGrantService struct {
	requestOptions ImageGrantOptions
	db             *sql.DB
	surface        frontend.Protocol
	grants         *Grants
	experience     func() frontend.Experience
	images         ImageSource
	artwork        frontend.Artwork
	now            func() time.Time
	pageMu         sync.Mutex
	pageKeyBytes   []byte
	macKeyBytes    []byte
	encBlock       cipher.Block
	imageGen       func() uint64
	artMu          sync.Mutex
	artGen         uint64
	art            map[pageArtKey][]frontend.Image
}

// Generation is constant because signed image grants carry no per-grant
// revocation state and change only when the image path or tag changes.
func (s *ImageGrantService) Generation() uint64 {
	return 0
}

// NewArtworkImageGrantService serves descriptors through the authorized
// artwork source. The source is consulted again for every fetch, after the
// viewer and Experience policy checks performed by Serve.
func NewArtworkImageGrantService(db *sql.DB, surface frontend.Protocol, grants *Grants, experience func() frontend.Experience, artwork frontend.Artwork, ttl time.Duration, now func() time.Time, options ...ImageGrantOptions) (*ImageGrantService, error) {
	if artwork == nil {
		return nil, errors.New("identity: missing artwork source")
	}
	s, err := NewImageGrantService(db, surface, grants, experience, unavailableImageSource{}, ttl, now, options...)
	if err != nil {
		return nil, err
	}
	s.artwork = artwork
	return s, nil
}

type unavailableImageSource struct{}

func (unavailableImageSource) Get(context.Context, string, int) ([]byte, string, error) {
	return nil, "", core.ErrNotAvailable
}

func NewImageGrantService(db *sql.DB, surface frontend.Protocol, grants *Grants, experience func() frontend.Experience, images ImageSource, _ time.Duration, now func() time.Time, options ...ImageGrantOptions) (*ImageGrantService, error) {
	if db == nil || surface == "" || grants == nil || experience == nil || images == nil {
		return nil, errors.New("identity: invalid image grant dependencies")
	}
	if now == nil {
		now = time.Now
	}
	var requestOptions ImageGrantOptions
	if len(options) > 0 {
		requestOptions = options[0]
	}
	return &ImageGrantService{requestOptions: requestOptions, db: db, surface: surface, grants: grants, experience: experience, images: images, now: now}, nil
}

func (s *ImageGrantService) authorized(ctx context.Context, viewer *core.Viewer, device core.Device, item core.RatingKey, channel string, image frontend.Image) error {
	base, _, _, rankErr := frontend.UnrankArtwork(image)
	if rankErr != nil {
		return rankErr
	}
	image = base
	if strings.HasPrefix(image.Path, frontend.ViewerAvatarPrefix) {
		if item != 0 || channel != "" {
			return core.ErrUnauthorized
		}
		current, ok := frontend.ViewerAvatarImage(viewer)
		if !ok || !frontend.SameImage(current, image) {
			return core.ErrUnauthorized
		}
		return nil
	}
	if strings.HasPrefix(image.Path, frontend.LibraryLiveThumbPrefix) {
		if s.requestOptions.LibraryLiveThumbs == nil {
			return core.ErrNotAvailable
		}
		if _, err := s.requestOptions.LibraryLiveThumbs.AuthorizeLibraryLiveThumb(core.WithLiveApp(ctx, string(s.surface)), viewer, image.Subject, image.Path); err != nil {
			return err
		}
		return nil
	}
	if strings.HasPrefix(image.Path, frontend.PluginImagePrefix) {
		if item != 0 || channel != "" || s.requestOptions.PluginImages == nil {
			return core.ErrNotAvailable
		}
		if _, err := s.requestOptions.PluginImages.AuthorizePluginImage(core.WithLiveApp(ctx, string(s.surface)), viewer, image.Subject, image.Path); err != nil {
			return err
		}
		return nil
	}
	if strings.HasPrefix(image.Path, frontend.LibraryCollagePrefix) {
		experience := s.experience()
		if experience == nil {
			return core.ErrNotAvailable
		}
		source, ok := experience.(frontend.LibraryCollageSource)
		if !ok {
			return core.ErrNotAvailable
		}
		got, err := source.LibraryCollage(ctx, viewer, device, image.Subject)
		if err != nil {
			return err
		}
		if frontend.SameImage(got, image) {
			return nil
		}
		return core.ErrUnauthorized
	}
	if channel != "" {
		if s.artwork == nil {
			return core.ErrNotAvailable
		}
		images, complete, err := s.artwork.List(frontend.WithoutArtworkMetadata(core.WithLiveApp(ctx, string(s.surface))), channelImageRequest(viewer, device, channel, image))
		if err != nil {
			return err
		}
		if !complete {
			return core.ErrNotAvailable
		}
		for _, candidate := range images {
			if frontend.SameImage(candidate, image) {
				return nil
			}
		}
		return core.ErrUnauthorized
	}
	experience := s.experience()
	if experience == nil {
		return core.ErrNotAvailable
	}
	if (s.surface == "silo" || s.surface == "vela") && image.Kind == frontend.ImagePerson {
		people, ok := experience.(frontend.PeopleImageReader)
		if !ok {
			return core.ErrNotAvailable
		}
		return people.AuthorizePersonImage(ctx, viewer, device, item, image, nil)
	}

	authCtx := frontend.WithoutArtworkMetadata(ctx)
	if image.Subject == frontend.CollectionCardArtworkSubject {
		authCtx = frontend.WithCollectionCardArtwork(authCtx)
	}
	if image.Kind != "" {
		authCtx = frontend.WithArtworkKinds(authCtx, map[string]bool{image.Kind: true})
	}
	var images []frontend.Image
	var complete bool
	var err error
	if scoped, ok := experience.(frontend.ItemArtwork); ok {
		images, complete, err = scoped.ItemArtwork(authCtx, viewer, device, item)
	} else {
		var detail frontend.ItemDetail
		detail, err = experience.Item(authCtx, frontend.ItemRequest{Viewer: viewer, Device: device, Key: item})
		images, complete = detail.Images, detail.ImagesAuthoritative
	}
	if err != nil {
		return err
	}
	if !complete {
		return core.ErrNotAvailable
	}
	for _, candidate := range images {
		if frontend.SameImage(candidate, image) {
			return nil
		}
	}
	return core.ErrUnauthorized
}

func (s *ImageGrantService) resolveCredential(ctx context.Context, credential string) (*core.Viewer, string, error) {
	source, ok := grantHash(credential)
	if !ok {
		return nil, "", core.ErrUnauthorized
	}
	memoKey := "image-grant:" + string(s.surface) + ":" + source + ":" + strconv.FormatUint(s.grants.Generation(), 10)
	viewer, err := reqmemo.Do(ctx, memoKey, func() (*core.Viewer, error) {
		// Artwork is on read paths, including portrait signing on people detail.
		return s.grants.resolve(ctx, string(s.surface), credential, true)
	})
	if err != nil || viewer == nil {
		if err == nil {
			err = core.ErrUnauthorized
		}
		return nil, "", err
	}
	return viewer, source, nil
}

func (s *ImageGrantService) sourceStillLive(ctx context.Context, viewerID int64, source string) error {
	var live int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=? AND g.viewer_id=? AND g.surface=? AND g.revoked=0 AND g.expires>? AND u.active=1`, source, viewerID, string(s.surface), s.now().UnixMilli()).Scan(&live)
	if err != nil {
		return core.ErrUnauthorized
	}
	return nil
}

func (s *ImageGrantService) Issue(ctx context.Context, req frontend.ImageGrantRequest) (string, error) {
	viewer, _, err := s.resolveCredential(ctx, req.Credential)
	if err != nil {
		return "", err
	}
	claims := imageClaims{
		Viewer:   viewer.ID,
		Item:     int64(req.Item),
		Channel:  req.Channel,
		Kind:     req.Image.Kind,
		Path:     req.Image.Path,
		Tag:      req.Image.Tag,
		Subject:  req.Image.Subject,
		Language: req.Device.Language,
	}
	if !validateImageClaims(claims) {
		return "", core.ErrUnauthorized
	}
	if req.Image.Kind != frontend.ImagePerson {
		ctx = frontend.WithArtworkRows(ctx)
	}
	if err := s.authorized(ctx, viewer, req.Device, req.Item, req.Channel, req.Image); err != nil {
		return "", err
	}
	return s.sealClaims(claims)
}

// IssueChannelLogo seals the channel's own artwork descriptor for the
// credential's viewer. The descriptor comes from the current, policy-filtered
// channel listing, so a channel the viewer may not see yields no grant.
func (s *ImageGrantService) IssueChannelLogo(ctx context.Context, credential string, device core.Device, channelID string) (string, error) {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" || s.artwork == nil {
		return "", core.ErrNotAvailable
	}
	viewer, _, err := s.resolveCredential(ctx, credential)
	if err != nil {
		return "", err
	}
	images, complete, err := s.artwork.List(frontend.WithoutArtworkMetadata(core.WithLiveApp(ctx, string(s.surface))), frontend.ArtworkRequest{Viewer: viewer, Device: device, Channel: channelID})
	if err != nil {
		return "", err
	}
	if !complete {
		return "", core.ErrNotAvailable
	}
	for _, img := range images {
		if img.Kind != frontend.ImageChannel {
			continue
		}
		claims := imageClaims{Viewer: viewer.ID, Channel: channelID, Kind: img.Kind, Path: img.Path, Tag: img.Tag, Subject: img.Subject, Language: device.Language}
		if !validateImageClaims(claims) {
			return "", core.ErrUnauthorized
		}
		return s.sealClaims(claims)
	}
	return "", core.ErrNotFound
}

// Revoke is a documented no-op: stateless image grants store no per-grant
// rows and re-authorize active viewer membership and current item or channel
// visibility on every Serve.
func (s *ImageGrantService) Revoke(context.Context, string, string) error {
	return nil
}

// ServePublic serves one title image to a request carrying no
// credential at all (frontend.PublicImageServer, #405). There is no
// grant to validate; instead the image must currently be public — the
// title visible to every active viewer (frontend.PublicArtwork) — and
// img must be one of that title's listed public images, re-checked here
// exactly as Serve re-authorizes a grant before writing bytes. Person
// and channel images are never public.
func (s *ImageGrantService) ServePublic(w http.ResponseWriter, r *http.Request, device core.Device, item core.RatingKey, img frontend.Image) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", 405)
		return
	}
	switch img.Kind {
	case frontend.ImagePoster, frontend.ImageBackdrop, frontend.ImageLogo, frontend.ImageThumb, frontend.ImageBanner, frontend.ImageStill:
	default:
		http.NotFound(w, r)
		return
	}
	public, ok := s.experience().(frontend.PublicArtwork)
	if !ok {
		http.NotFound(w, r)
		return
	}
	base, _, _, rankErr := frontend.UnrankArtwork(img)
	if rankErr != nil {
		http.NotFound(w, r)
		return
	}
	r = artworkRowsRequest(r, img)
	pubCtx := frontend.WithoutArtworkMetadata(r.Context())
	if base.Kind != "" {
		pubCtx = frontend.WithArtworkKinds(pubCtx, map[string]bool{base.Kind: true})
	}
	images, reader, visible, err := public.PublicArtwork(pubCtx, device, item)
	if err != nil || !visible || reader == nil {
		http.NotFound(w, r)
		return
	}
	for _, candidate := range images {
		if frontend.SameImage(candidate, base) {
			s.writeGrantedImage(w, r, reader, device, item, "", img)
			return
		}
	}
	http.NotFound(w, r)
}

func (s *ImageGrantService) requestCredential(r *http.Request) string {
	if r == nil {
		return ""
	}
	if s.requestOptions.Token != nil {
		if token := s.requestOptions.Token(r); token != "" {
			return token
		}
	}
	if token, ok := authhdr.Bearer(r.Header.Get("Authorization")); ok {
		return token
	}
	return ""
}

func (s *ImageGrantService) Serve(w http.ResponseWriter, r *http.Request, secret string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", 405)
		return
	}
	claims, err := s.openClaims(secret)
	if err != nil || !claimMatchesURL(claims, r) {
		http.NotFound(w, r)
		return
	}
	if cred := s.requestCredential(r); cred != "" {
		reqViewer, err := s.grants.Resolve(r.Context(), string(s.surface), cred)
		if err != nil || reqViewer == nil || reqViewer.ID != claims.Viewer {
			http.NotFound(w, r)
			return
		}
	}
	var active int
	if err := s.db.QueryRowContext(r.Context(), `SELECT active FROM auth_users WHERE id=?`, claims.Viewer).Scan(&active); err != nil || active != 1 {
		http.NotFound(w, r)
		return
	}
	viewer, err := s.grants.viewers.ActiveUser(r.Context(), claims.Viewer)
	if err != nil || viewer == nil {
		http.NotFound(w, r)
		return
	}
	device := core.Device{Language: claims.Language}
	img := frontend.Image{Kind: claims.Kind, Path: claims.Path, Tag: claims.Tag, Subject: claims.Subject}
	r = artworkRowsRequest(r, img)
	if err = s.authorized(r.Context(), viewer, device, core.RatingKey(claims.Item), claims.Channel, img); err != nil {
		http.NotFound(w, r)
		return
	}
	s.writeGrantedImage(w, r, viewer, device, core.RatingKey(claims.Item), claims.Channel, img)
}

// artworkRowsRequest lets an image read authorize and list its title from
// visibility rows (frontend.WithArtworkRows), shared by its authorization
// and its read. A credited person's portrait is listed from the title's
// credits, so its title is read in full.
func artworkRowsRequest(r *http.Request, img frontend.Image) *http.Request {
	if img.Kind == frontend.ImagePerson {
		return r
	}
	return r.WithContext(frontend.WithArtworkRows(r.Context()))
}

func imageETag(img frontend.Image, width, height int, format string, blur bool) string {
	identity := img.Tag
	if identity == "" {
		identity = img.Path
	}
	if identity == "" {
		return ""
	}
	blurText := "0"
	if blur {
		blurText = "1"
	}
	sum := sha256.Sum256([]byte(identity + "\x00" + strconv.Itoa(width) + "\x00" + strconv.Itoa(height) + "\x00" + format + "\x00" + blurText))
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

func (s *ImageGrantService) writeGrantedImage(w http.ResponseWriter, r *http.Request, viewer *core.Viewer, device core.Device, item core.RatingKey, channel string, img frontend.Image) {
	width := grantedImageWidth(r, s.surface)
	mediaBrowser := s.surface == "jellyfin" || s.surface == "emby"
	quality := 0
	if mediaBrowser {
		quality, _ = strconv.Atoi(r.URL.Query().Get("quality"))
		if quality < 0 || quality > 100 {
			quality = 0
		}
	}
	height, _ := strconv.Atoi(r.URL.Query().Get("h"))
	if mediaBrowser && height == 0 {
		for _, name := range []string{"fillHeight", "maxHeight"} {
			if n, _ := strconv.Atoi(r.URL.Query().Get(name)); n > 0 {
				height = n
				break
			}
		}
	}
	if mediaBrowser && width == 0 && height == 0 && img.Kind != frontend.ImageBackdrop && img.Kind != frontend.ImageThumb && img.Kind != frontend.ImageLogo {
		width = 500
	}
	format := r.URL.Query().Get("format")
	if mediaBrowser {
		// The shared renderer encodes JPEG and PNG. Other Jellyfin formats
		// (Webp, Gif, Bmp, Svg), and unknown values, use the source format.
		switch strings.ToLower(format) {
		case "jpg", "jpeg":
			format = "jpeg"
		case "png":
			format = "png"
		default:
			format = ""
		}
	}
	if s.surface == "silo" && channel != "" && format == "" {
		// Channel artwork is often a generated SVG tile, which the Silo apps
		// cannot draw: every live card was blank. Rasterize it for Silo.
		format = "png"
	}
	blur := r.URL.Query().Get("blur") == "1"
	if s.artwork != nil {
		if height < 0 || height > 4000 {
			http.Error(w, "invalid image height", http.StatusBadRequest)
			return
		}
		if format != "" && format != "jpeg" && format != "png" {
			http.Error(w, "invalid image format", http.StatusBadRequest)
			return
		}
		if blurQuery := r.URL.Query().Get("blur"); blurQuery != "" && blurQuery != "0" && blurQuery != "1" {
			http.Error(w, "invalid image blur", http.StatusBadRequest)
			return
		}
	}
	// Direct sources retain their immutable descriptor contract. Shared
	// artwork descriptors authorize a selection and can resolve to new pixels
	// after a provider refresh. Collage URLs include their source identity.
	immutable := (s.artwork == nil && !strings.HasPrefix(img.Path, frontend.LibraryLiveThumbPrefix) &&
		!strings.HasPrefix(img.Path, frontend.PluginImagePrefix)) ||
		strings.HasPrefix(img.Path, frontend.ImmutableArtworkPrefix) || strings.HasPrefix(img.Path, frontend.LibraryCollagePrefix)
	var etag string
	if immutable {
		etag = imageETag(img, width, height, format+strconv.Itoa(quality), blur)
		if etag != "" {
			w.Header().Set("Cache-Control", kitimages.Immutable)
			if kitimages.NotModified(w, r, etag) {
				return
			}
			w.Header().Del("Cache-Control")
			w.Header().Del("ETag")
		}
	}
	ctx := kitimages.TrackCachePolicy(r.Context())
	var data []byte
	var contentType string
	var err error
	if s.artwork != nil {
		q := r.URL.Query()
		siloPluginResize := s.surface == "silo" && item.IsPluginKey() && (q.Get("w") != "" || q.Get("maxWidth") != "" || q.Get("fillWidth") != "" || q.Get("h") != "")
		data, contentType, err = s.artwork.Read(core.WithLiveApp(ctx, string(s.surface)), grantedArtworkRequest(viewer, device, item, channel, img), img, frontend.ArtworkTransform{Width: width, Height: height, Format: format, Blur: blur, Quality: quality, Fit: mediaBrowser || s.surface == "vela" || siloPluginResize})
	} else {
		data, contentType, err = s.images.Get(r.Context(), img.Path, width)
	}
	if err == nil && s.artwork == nil && s.surface == "vela" && !strings.HasPrefix(contentType, "image/svg") {
		data, contentType, err = fitGrantedImage(data, contentType, width, height)
	}
	if err != nil {
		// A missing image is a 404, never a gateway error: clients treat
		// 502 as a server fault and retry in a loop (#614). Only a
		// failure that is not "the image does not exist" stays 502.
		if errors.Is(err, core.ErrNotFound) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, artwork.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "image unavailable", 502)
		return
	}
	// A portrait or viewer-avatar fetch may cross I/O after its first
	// authorization. Check current membership and fresh policy again before
	// publishing bytes.
	if strings.HasPrefix(img.Path, frontend.ViewerAvatarPrefix) {
		var stillActive int
		if err := s.db.QueryRowContext(r.Context(), `SELECT active FROM auth_users WHERE id=?`, viewer.ID).Scan(&stillActive); err != nil || stillActive != 1 {
			http.NotFound(w, r)
			return
		}
		freshViewer, err := s.grants.viewers.ActiveUser(r.Context(), viewer.ID)
		if err != nil || freshViewer == nil {
			http.NotFound(w, r)
			return
		}
		if current, ok := frontend.ViewerAvatarImage(freshViewer); !ok || !frontend.SameImage(current, img) {
			http.NotFound(w, r)
			return
		}
	} else if (s.surface == "silo" || s.surface == "vela") && img.Kind == frontend.ImagePerson {
		if err := s.authorizePersonGrant(r.Context(), viewer, device, item, img); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
	if kitimages.NoStore(ctx) {
		h.Set("Cache-Control", "private, no-store")
	} else {
		cacheControl := kitimages.Revalidate
		if etag != "" {
			cacheControl = kitimages.Immutable
		} else {
			etag = kitimages.ETag(data, contentType)
		}
		h.Set("Cache-Control", cacheControl)
		if kitimages.NotModified(w, r, etag) {
			return
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

var _ frontend.ImageGrants = (*ImageGrantService)(nil)
var _ frontend.PublicImageServer = (*ImageGrantService)(nil)
var _ frontend.ChannelLogoIssuer = (*ImageGrantService)(nil)

func grantedImageWidth(r *http.Request, surface frontend.Protocol) int {
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	// MediaBrowser and Silo name their width constraints maxWidth/fillWidth.
	if (surface == "silo" || surface == "jellyfin" || surface == "emby") && width == 0 {
		for _, name := range []string{"maxWidth", "fillWidth"} {
			if n, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil && n > 0 {
				width = min(n, 4000)
				if surface == "silo" {
					width = min(width, 3840)
				}
				break
			}
		}
	}
	if width < 0 || width > 4000 {
		width = 500
	}
	if width == 0 && r.URL.Query().Get("h") == "" && surface != "jellyfin" && surface != "emby" && surface != "vela" {
		width = 500
	}
	return width
}

// Programme descriptors seal the channel and the exact airing identity in
// Subject so both issue and reads re-enumerate that airing under current
// channel/programme policy, without a library scan.
func channelImageRequest(viewer *core.Viewer, device core.Device, channel string, image frontend.Image) frontend.ArtworkRequest {
	req := frontend.ArtworkRequest{Viewer: viewer, Device: device, Channel: channel}
	if image.Kind == frontend.ImageStill || image.Kind == frontend.ImageBackdrop {
		req.Programme = image.Subject
	}
	return req
}
func grantedArtworkRequest(viewer *core.Viewer, device core.Device, item core.RatingKey, channel string, image frontend.Image) frontend.ArtworkRequest {
	if channel != "" {
		return channelImageRequest(viewer, device, channel, image)
	}
	return frontend.ArtworkRequest{Viewer: viewer, Device: device, Item: item}
}
