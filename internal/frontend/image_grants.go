package frontend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrImageBatchLimit = errors.New("frontend: invalid artwork batch")

// ImageGrantRequest asks for one authorized image on one authenticated session.
// Credential is supplied in-process and must never be placed in an image URL.
type ImageGrantRequest struct {
	Credential string
	Device     core.Device
	Item       core.RatingKey
	Channel    string
	Image      Image
}

// ImageGrants issues and validates signed, stateless opaque image capabilities.
// Serve requires a capability, even when the client cannot send account tokens.
// Revoke is retained as a documented no-op for callers; serve-time checks
// enforce viewer status and current item/channel/image policy on every read.
type ImageGrants interface {
	Issue(context.Context, ImageGrantRequest) (string, error)
	Revoke(context.Context, string, string) error
	Serve(http.ResponseWriter, *http.Request, string)
}

// ChannelArtworkGrantBatch prepares channel authorization for a page of
// subsequent channel logo Issue calls on the returned context.
type ChannelArtworkGrantBatch interface {
	PrepareChannelArtwork(context.Context, string, []string) (context.Context, error)
}

// ImageBatchRequest requests primary artwork for a page of items. At most 50
// distinct keys and 256 input keys are accepted; duplicates are removed in
// first-occurrence order. Empty Kinds selects poster. Person and channel
// images are deliberately excluded. Portraits use PersonImageGrantBatch or a
// scoped single grant; channel images use a scoped single grant.
type ImageBatchRequest struct {
	Credential string
	Device     core.Device
	Items      []core.RatingKey
	Kinds      []string
}
type GrantedImage struct {
	Image  Image
	Secret string
}
type ImageBatchItem struct {
	Item          core.RatingKey
	Authoritative bool
	Images        []GrantedImage
}

// ImageGrantBatch is an optional extension for rows and grids. Hidden items
// are omitted. Results preserve first-occurrence item and descriptor order;
// each requested kind contributes at most its first descriptor. An authorized
// item with no images is distinguished from an unavailable artwork list by
// Authoritative. Every secret is independently revalidated when served.
type ImageGrantBatch interface {
	IssueBatch(context.Context, ImageBatchRequest) ([]ImageBatchItem, error)
}

// PublicArtwork lists title artwork an unauthenticated image request may
// be served: artwork of a title visible to every active viewer under
// current policy (sections, content ratings, the owner's content policy
// and inherited show rules). ok is false when the title is missing,
// hidden from any viewer, or not a plain title (collections, playlists
// and extras are never public). reader is a real active viewer the bytes
// may be read as — every viewer passed, so any of them may read — and is
// never a requester identity: there is none. Implementations cache the
// intersection per policy epoch, so a restriction change takes effect
// without waiting for a TTL.
type PublicArtwork interface {
	PublicArtwork(ctx context.Context, device core.Device, item core.RatingKey) (images []Image, reader *core.Viewer, ok bool, err error)
}

// PublicImageServer is the optional image-grant extension that serves
// one PublicArtwork-selected image to a request with no credential. It
// re-checks public visibility and that the image belongs to the item
// before writing bytes, mirroring Serve's re-authorization.
type PublicImageServer interface {
	ServePublic(http.ResponseWriter, *http.Request, core.Device, core.RatingKey, Image)
}

// ImagePageRequest signs artwork for titles the caller has already authorized
// for this page. The signer does not write a grant row.
type ImagePageRequest struct {
	Ranked     map[core.RatingKey]int
	RankedWide bool
	Credential string
	Device     core.Device
	Titles     []core.Title
	Kinds      []string
}

// ImagePageSigner is the list path for image URLs. Serve still rechecks the
// active viewer and current artwork policy on every read.
type ImagePageSigner interface {
	SignPage(context.Context, ImagePageRequest) ([]ImageBatchItem, error)
}

// ImagePageRenewal re-arms images SignPage sealed earlier, for Credential.
// Items are SignPage results, unchanged.
type ImagePageRenewal struct {
	Credential string
	Device     core.Device
	Items      []ImageBatchItem
}

// ImagePageRenewer is an optional listing-cache extension for implementations
// whose page image tokens require periodic renewal.
type ImagePageRenewer interface {
	RenewPage(context.Context, ImagePageRenewal) error
}

// ItemArtwork is an optional cache-only authorization path for issuing grid
// artwork without computing playback versions or tracks.
type ItemArtwork interface {
	ItemArtwork(context.Context, *core.Viewer, core.Device, core.RatingKey) ([]Image, bool, error)
}

// PageArtwork lists artwork for titles the caller already loaded.
type PageArtwork interface {
	ListPage(context.Context, *core.Viewer, core.Device, []core.Title) (map[core.RatingKey][]Image, error)
}

// ItemArtworkPage authorizes and lists artwork for one grid in a batch.
// Present keys have authoritative listings (including empty listings); missing
// keys must not be granted. SignPage and IssueBatch use it without falling back
// to scalar authorization for missing keys or errors.
type ItemArtworkPage interface {
	ItemArtworkPage(context.Context, *core.Viewer, core.Device, []core.RatingKey) (map[core.RatingKey][]Image, error)
}

type artworkTitleKey struct{}

// WithArtworkTitle carries a title the caller already loaded and authorized.
// Artwork listing must not read that title again.
func WithArtworkTitle(ctx context.Context, title *core.Title) context.Context {
	if title == nil {
		return ctx
	}
	return context.WithValue(ctx, artworkTitleKey{}, title)
}

// ArtworkTitle returns the title stored by WithArtworkTitle when it is the
// requested key, else the one this request read as a row (ArtworkRows).
func ArtworkTitle(ctx context.Context, key core.RatingKey) (*core.Title, bool) {
	title, _ := ctx.Value(artworkTitleKey{}).(*core.Title)
	if title != nil && title.Key == key {
		return title, true
	}
	if rows := ArtworkRowsFrom(ctx); rows != nil {
		if t, ok := rows.Get(key); ok {
			return &t, true
		}
	}
	return nil, false
}

type artworkCollectionKey struct{}

// WithArtworkCollection carries a collection the caller already loaded,
// authorized, and resolved fallback artwork for. Artwork listing must not
// reload the collection or its members.
func WithArtworkCollection(ctx context.Context, collection *core.Collection) context.Context {
	if collection == nil {
		return ctx
	}
	return context.WithValue(ctx, artworkCollectionKey{}, collection)
}

// ArtworkCollection returns the preloaded collection stored by
// WithArtworkCollection when it matches key.
func ArtworkCollection(ctx context.Context, key core.RatingKey) (*core.Collection, bool) {
	col, _ := ctx.Value(artworkCollectionKey{}).(*core.Collection)
	if col != nil && col.Key == key {
		return col, true
	}
	return nil, false
}

// ArtworkRows holds the titles an artwork request read as visibility rows:
// the row, with the facts the viewer's policy reads, and no credits. Only a
// request that never lists credited people's portraits (a grant of a
// title's own images: poster, art, logo...) reads titles this way; its
// authorization and its image read then share them (ArtworkRows).
type ArtworkRows struct {
	mu     sync.Mutex
	titles map[core.RatingKey]core.Title
}

// Put records a title the request judged.
func (r *ArtworkRows) Put(t core.Title) {
	if r == nil || t.Key == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.titles == nil {
		r.titles = map[core.RatingKey]core.Title{}
	}
	r.titles[t.Key] = t
}

// Get returns a title the request recorded.
func (r *ArtworkRows) Get(key core.RatingKey) (core.Title, bool) {
	if r == nil {
		return core.Title{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.titles[key]
	return t, ok
}

type artworkRowsKey struct{}

// WithArtworkRows marks ctx as an artwork request that lists no credited
// people's portraits (ArtworkRows).
func WithArtworkRows(ctx context.Context) context.Context {
	if ArtworkRowsFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, artworkRowsKey{}, &ArtworkRows{})
}

// ArtworkRowsFrom returns WithArtworkRows' holder, or nil.
func ArtworkRowsFrom(ctx context.Context) *ArtworkRows {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(artworkRowsKey{}).(*ArtworkRows)
	return r
}

// ImmutableArtworkPrefix marks grants whose descriptor pins a TMDB image path.
const ImmutableArtworkPrefix = "immutable-artwork/"

// LibraryCollagePrefix marks a generated library-view collage. The remainder
// is the hex identity of the view and the child posters it was built from.
const LibraryCollagePrefix = "library-collage/"

// LibraryCollageSource builds that collage for one library view. viewID is
// the view GUID. A change in the child posters produces a different image.
type LibraryCollageSource interface {
	LibraryCollage(context.Context, *Viewer, Device, string) (Image, error)
}

// LibraryLiveThumbPrefix marks a proxied plugin-library live-item thumbnail.
// The remainder is an opaque hash scoped to the library view in Image.Subject.
const LibraryLiveThumbPrefix = "library-live/"

// LibraryLiveThumbAuthorizer checks that viewer may access a live thumbnail in
// viewID and returns the verified upstream URL for Ether's image proxy to fetch.
type LibraryLiveThumbAuthorizer interface {
	AuthorizeLibraryLiveThumb(ctx context.Context, viewer *Viewer, viewID string, path string) (string, error)
}

// PluginImagePrefix marks an image a plugin named by URL (a team crest, a
// league logo). The remainder is the upstream URL; Image.Subject is the
// plugin ID. The URL travels only inside sealed, encrypted grants, so clients
// never see it, and every read re-checks the plugin and its network
// permission before Ether fetches the bytes.
const PluginImagePrefix = "plugin-image/"

// MaxPluginImageURL bounds a plugin image URL so its grant stays a sane size.
const MaxPluginImageURL = 1000

// PluginImage returns the descriptor for a plugin-named image URL, or false
// when raw is not an absolute http(s) URL Ether may proxy.
func PluginImage(pluginID, raw string) (Image, bool) {
	pluginID = strings.TrimSpace(pluginID)
	raw = strings.TrimSpace(raw)
	if pluginID == "" || len(pluginID) > 256 || raw == "" || len(raw) > MaxPluginImageURL {
		return Image{}, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return Image{}, false
	}
	sum := sha256.Sum256([]byte(raw))
	return Image{Kind: ImageLogo, Path: PluginImagePrefix + raw, Tag: hex.EncodeToString(sum[:16]), Subject: pluginID}, true
}

// PluginImageAuthorizer checks that viewer may still see images named by
// pluginID and returns the upstream URL Ether may fetch for path.
type PluginImageAuthorizer interface {
	AuthorizePluginImage(ctx context.Context, viewer *Viewer, pluginID string, path string) (string, error)
}

// PluginImageMemo remembers, for one response or stream, whether a viewer may
// see a plugin's images from a host, so signing every crest on a page costs
// one access decision per plugin and host rather than one per image.
// Decisions last pluginImageMemoTTL; fetching an image always checks again.
type PluginImageMemo struct {
	mu        sync.Mutex
	decisions map[pluginImageMemoKey]pluginImageDecision
}

type pluginImageMemoKey struct {
	viewer       int64
	plugin, host string
}

type pluginImageDecision struct {
	err error
	at  time.Time
}

const pluginImageMemoTTL = 30 * time.Second

type pluginImageMemoCtx struct{}

// WithPluginImageMemo gives ctx a fresh memo for plugin image decisions.
func WithPluginImageMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, pluginImageMemoCtx{}, &PluginImageMemo{decisions: map[pluginImageMemoKey]pluginImageDecision{}})
}

// PluginImageMemoFrom returns ctx's memo, or nil.
func PluginImageMemoFrom(ctx context.Context) *PluginImageMemo {
	m, _ := ctx.Value(pluginImageMemoCtx{}).(*PluginImageMemo)
	return m
}

// Decide returns the remembered decision for (viewer, plugin, host) or runs
// decide and remembers it. A nil memo always decides.
func (m *PluginImageMemo) Decide(viewer int64, plugin, host string, decide func() error) error {
	if m == nil {
		return decide()
	}
	key := pluginImageMemoKey{viewer: viewer, plugin: plugin, host: host}
	m.mu.Lock()
	d, ok := m.decisions[key]
	m.mu.Unlock()
	if ok && time.Since(d.at) < pluginImageMemoTTL {
		return d.err
	}
	err := decide()
	m.mu.Lock()
	m.decisions[key] = pluginImageDecision{err: err, at: time.Now()}
	m.mu.Unlock()
	return err
}

// ChannelLogoIssuer issues a grant for one channel's own logo (the channel
// artwork descriptor), so a payload that names a channel never carries the
// channel's raw logo URL.
type ChannelLogoIssuer interface {
	IssueChannelLogo(ctx context.Context, credential string, device core.Device, channelID string) (string, error)
}

// ViewerAvatarPrefix marks a viewer's Plex account or Plex Home profile avatar.
// The remainder is an opaque hash of the avatar URL; the raw upstream URL never
// appears in wire artwork descriptors or signed grants.
const ViewerAvatarPrefix = "viewer-avatar/"

// ViewerAvatarImage returns the shared artwork descriptor for a Plex-linked
// viewer's avatar, or false when the viewer has no Plex link or valid avatar URL.
func ViewerAvatarImage(v *Viewer) (Image, bool) {
	if v == nil || v.ID <= 0 || v.PlexID <= 0 {
		return Image{}, false
	}
	raw := strings.TrimSpace(v.Thumb)
	if raw == "" {
		return Image{}, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return Image{}, false
	}
	sum := sha256.Sum256([]byte("viewer-avatar-v1\x00" + raw))
	digest := hex.EncodeToString(sum[:16])
	return Image{
		Kind:    ImagePerson,
		Path:    ViewerAvatarPrefix + digest,
		Tag:     digest,
		Subject: "viewer:" + strconv.FormatInt(v.ID, 10),
	}, true
}

// ProgrammeImageGrant scopes an airing image to its channel and time. Images
// attached by the title artwork service keep their existing title scope.
func ProgrammeImageGrant(p LiveProgramme, image Image, credential string, device Device) ImageGrantRequest {
	req := ImageGrantRequest{Credential: credential, Device: device, Item: p.Item, Image: image}
	if image.Subject == p.ID {
		req.Item, req.Channel = 0, p.ChannelID
	}
	return req
}
