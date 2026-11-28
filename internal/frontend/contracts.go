// Package frontend defines the protocol-facing contracts provided by Ether.
package frontend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Protocol identifies a surface independently of client product or platform.
type Protocol = core.Protocol

// PlaybackClientID scopes a client installation to its frontend protocol.
// Playback and support captures use the same key to correlate one device.
func PlaybackClientID(protocol Protocol, deviceID string) string {
	return fmt.Sprintf("%d:%s%s", len(protocol), protocol, deviceID)
}

// ProtocolHTTP is the release protocol for a stream URL read with HTTP
// range requests. Its value stays owned by core.
const ProtocolHTTP = core.ProtocolHTTP

// Placement identifies where rows are rendered.
type Placement string

const (
	PlacementHome    Placement = "home"
	PlacementLibrary Placement = "library"
	PlacementDetail  Placement = "detail"
)

// RowPurpose describes why a row exists, independent of wire-level hub IDs.
type RowPurpose = core.RowPurpose

const (
	RowContinue         RowPurpose = "continue"
	RowNextUp           RowPurpose = "nextUp"
	RowRecentlyReleased RowPurpose = "recentlyReleased"
	RowRecentlyAdded    RowPurpose = "recentlyAdded"
	RowCollection       RowPurpose = "collection"
	RowRecommendation   RowPurpose = "recommendation"
	RowLive             RowPurpose = "live"
	RowWatchlist        RowPurpose = "watchlist"
	RowRecentContexts   RowPurpose = core.PurposeRecentContexts
	RowLiked            RowPurpose = core.PurposeLiked
)

type View struct {
	// Revision changes when the published library definition changes.
	Revision string
	ID       string
	Name     string
	Type     core.MediaType
	// Virtual is true only when the host asserts this library has no physical
	// locations. False means the host did not assert virtual origin.
	Virtual bool
	// LibraryID is the stable integer identity in the libraries store.
	LibraryID core.LibraryID
	// KindKey and PluginID are set for plugin-provided libraries (empty for
	// built-in and filter libraries).
	KindKey  string
	PluginID string
	// Native is the protocol-native presentation string on the bound protocol
	// (for example, MediaBrowser CollectionType or Plex base type).
	Native string
	// Subset is true when the library narrows a broader canonical catalogue
	// with rules, visibility defaults, narrower sources, or a collection.
	Subset bool
	// Plugin library presentation metadata (optional; populated for plugin libraries).
	Media       core.MediaClass `json:"media,omitempty"`
	Card        string          `json:"card,omitempty"`
	DateDisplay string          `json:"dateDisplay,omitempty"`
	Experience  string          `json:"experience,omitempty"` // watch|listen|read|gallery|live… (core.LibraryExperience*)
	Tabs        []ViewTab       `json:"tabs,omitempty"`
	Sorts       []string        `json:"sorts,omitempty"`
	Facets      []ViewFacet     `json:"facets,omitempty"`
	ListLevels  []string        `json:"-"`
	// TabsCustomized distinguishes owner tab lists from protocol defaults.
	TabsCustomized bool `json:"-"`
	// HiddenBrowseTabs retain the query settings for direct links to hidden grids.
	HiddenBrowseTabs []ViewTab `json:"-"`
}

// EffectiveExperience is the library experience apps should present the view
// with: the declared one, else its media class default (listen for music).
func (v View) EffectiveExperience() string {
	return core.EffectiveLibraryExperience(v.Experience, v.Media)
}

// LibraryExperiences lists the experiences Ether assigns (plugins may name others).
var LibraryExperiences = core.LibraryExperiences

// ViewTab describes one sub-tab of a library view.
type ViewTab struct {
	ID                string          `json:"id"`
	Label             string          `json:"label"`
	Kind              string          `json:"kind"`
	Level             string          `json:"level,omitempty"`
	Facet             string          `json:"facet,omitempty"`
	Order             string          `json:"order,omitempty"`
	Sort              string          `json:"sort,omitempty"`
	Direction         string          `json:"direction,omitempty"`
	Card              string          `json:"card,omitempty"`
	Layout            string          `json:"layout,omitempty"`
	Search            bool            `json:"search,omitempty"`
	SearchPlaceholder string          `json:"searchPlaceholder,omitempty"`
	Filters           *core.TabFilter `json:"filters,omitempty"`
}

// LibraryLiveRequest asks a plugin library's live tab for currently live items.
type LibraryLiveRequest struct {
	Tab    string
	Query  string
	Filter string
	Cursor string
	Limit  int
}

// LibraryLiveItem is one live stream item returned by a plugin library live tab.
// ThumbRef is an opaque Ether image reference (LibraryLiveThumbPrefix + hash),
// never a raw upstream URL.
type LibraryLiveItem struct {
	ID       string
	Title    string
	Subtitle string
	Badge    string
	Viewers  *int
	Tags     []string
	ThumbRef string
	Live     bool
}

// LibraryLiveFilter is one optional filter chip returned with a live tab page.
type LibraryLiveFilter struct {
	Key   string
	Label string
}

// LibraryLivePage is one page of live items from a plugin library live tab.
type LibraryLivePage struct {
	Items      []LibraryLiveItem
	Filters    []LibraryLiveFilter
	NextCursor string
}

// LibraryLiveStream is the server-side resolved direct stream for one live item.
type LibraryLiveStream struct {
	URL     string
	Headers map[string]string
	MIME    string
	Title   string
}

// LibraryLive lists and resolves live streams for plugin libraries that declare
// a "live" tab. Nil means plugin library live tabs are not wired.
type LibraryLive interface {
	ListLive(ctx context.Context, viewer *core.Viewer, viewID string, req LibraryLiveRequest) (LibraryLivePage, error)
	ResolveLive(ctx context.Context, viewer *core.Viewer, viewID string, tab string, itemID string) (LibraryLiveStream, error)
}

// LibraryScopeReader exposes a library view's browse-scope filter and indexed
// membership check for frontends that query the catalogue directly for built-in
// sections or filter search/hub results inside one library view.
type LibraryScopeReader interface {
	LibraryBrowseFilter(ctx context.Context, viewer *core.Viewer, viewID string, req core.Filter) (core.Filter, bool, error)
	LibraryMembers(ctx context.Context, libraryID core.LibraryID, keys []core.RatingKey) (map[core.RatingKey]bool, bool, error)
}

// DirectLiveTarget identifies an ephemeral direct-URL live stream tuned through
// the session broker without registering a Live TV channel.
type DirectLiveTarget struct {
	LibraryID core.LibraryID
	Tab       string
	ItemID    string
	Title     string
	URL       string
	Headers   map[string]string
	MIME      string
}

// ViewFacet describes one plugin-declared filter facet on a library view.
type ViewFacet struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Type  string `json:"type"`
	Multi bool   `json:"multi"`
}

// FacetBucket is one discrete filter bucket for a plugin library facet.
type FacetBucket struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// FacetCollection is one facet-backed collection entry with its newest item.
type FacetCollection struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	ItemCount  int         `json:"itemCount"`
	NewestItem *core.Title `json:"-"`
}

// FacetCollectionPage is one page of facet-backed collections.
type FacetCollectionPage struct {
	Items []FacetCollection
	More  bool
}

// LibraryFacetBuckets is an optional Experience capability returning discrete
// filter buckets for a plugin library's declared facet field.
type LibraryFacetBuckets interface {
	LibraryFacetBuckets(ctx context.Context, viewer *core.Viewer, device core.Device, viewID, field string) ([]FacetBucket, error)
}

// LibraryFacetBucketsAtLevel is an optional Experience capability returning
// discrete filter buckets scoped to a specific plugin hierarchy level.
type LibraryFacetBucketsAtLevel interface {
	LibraryFacetBucketsAtLevel(ctx context.Context, viewer *core.Viewer, device core.Device, viewID, level, field string) ([]FacetBucket, error)
}

// LevelFacetReader is an optional Experience capability returning facet values
// scoped to a specific plugin hierarchy level or protocol item type.
type LevelFacetReader interface {
	FacetsAtLevel(ctx context.Context, viewer *core.Viewer, device core.Device, viewID, level, name string) ([]core.FacetValue, error)
}

// LibraryFacetCollections is an optional Experience capability returning a
// single page of facet-backed collections ordered by count (default) or name.
type LibraryFacetCollections interface {
	LibraryFacetCollections(ctx context.Context, viewer *core.Viewer, device core.Device, viewID, field, order string, page PageQuery) (FacetCollectionPage, error)
}

// OwnedCollectionMatcher lets search select titles before loading collection members.
type OwnedCollectionMatcher interface {
	ListOwnedMatching(context.Context, *User, MediaType, string, func(string) bool) ([]Collection, error)
}

// CollectionPager is an optional Collections capability returning one page
// of a library type's collections plus the total, without listing them all.
type CollectionPager interface {
	ListPage(ctx context.Context, user *User, typ MediaType, offset, limit int) ([]Collection, int, error)
}

// PluginLibraryRowsReader is an optional Experience capability returning a
// plugin library's rows tab (Recommendations page "library:<id>/<tab>";
// tab "" is the library's first rows tab), arranged by that page.
type PluginLibraryRowsReader interface {
	PluginLibraryRows(ctx context.Context, viewer *core.Viewer, device core.Device, viewID, tab string) ([]Row, error)
}

// RowEntryKind identifies a neutral entry independently of wire formats.
type RowEntryKind string

const (
	RowEntryTitle       RowEntryKind = "title"
	RowEntryCollection  RowEntryKind = "collection"
	RowEntryPlaylist    RowEntryKind = "playlist"
	RowEntryLiveChannel RowEntryKind = "liveChannel"
	RowEntryDirectory   RowEntryKind = "directory"
)

// RowEntry is a tagged union: Kind identifies the one non-nil payload.
// Handle is an opaque, server-issued interaction capability scoped to the
// viewer and device; it is not an account credential.
type RowEntry struct {
	Watchlist   bool
	Badge       string
	Kind        RowEntryKind
	Handle      string
	Title       *core.Title
	Collection  *core.Collection
	Playlist    *core.Playlist
	LiveChannel *RowLiveChannel
	Directory   *RowDirectory
}

// RowLiveChannel contains viewer-visible channel metadata, never source URLs.
type RowLiveChannel struct {
	ID, Number, Name, Logo string
	GuideReference         string
	Current, Next          *RowProgramme
	Images                 []Image
}

// RowProgramme describes one guide entry without an upstream stream URL.
type RowProgramme struct {
	ID, Title, EpisodeTitle string
	Start, End              time.Time
	Season, Episode         int
	Item                    core.RatingKey
}

// RowDirectory identifies a browsable destination in the experience.
type RowDirectory struct {
	ID, Title, Description, Image string
	Kind                          string
}

// RowLayout describes Home Designer presentation selected by experience.
type RowLayout struct {
	RankedTop10    bool
	PreviewOnFocus bool
	// PreviewOnFocusPolicy retains the owner override when a shelf becomes a hero.
	PreviewOnFocusPolicy *bool
	Order                int
	Style                string
	// StylePolicy is the owner's row style (RowStyleStandard, RowStyleHero,
	// RowStyleWide), "" when the owner left the app's default.
	StylePolicy string
	// HideWatched drops the viewer's fully watched titles from the row.
	HideWatched bool
	Paint       RowPaint
	Always      bool
	// Present is the page row's presentation (Recommendations): layout,
	// card, role, personal, library, experience, surface. Wire adapters
	// copy it; unknown values draw as a standard shelf.
	Present core.RowPresentation
}

// RowPaint carries optional presentation colors and artwork references.
type RowPaint struct {
	Background, Foreground, Accent string
	Artwork                        string
}

type Row struct {
	RankedTop10    bool
	PreviewOnFocus bool
	// Situation is the server-owned ranking context retained for deferred receipts.
	Situation core.HomeSituation
	ID, Title string
	// Key is the row's identity across visits when ID is a per-visit
	// snapshot id; "" means ID is already stable.
	Key          string
	Purpose      RowPurpose
	Kind         string // Stable semantic producer kind, independent of protocol.
	Layout       RowLayout
	Style        string
	Promoted     bool
	Rank         int
	ImpressionID string
	// Handle is a server-issued row interaction capability.
	Handle string
	// Entries is the ordered heterogeneous row; Items remains for compatibility.
	Entries    []RowEntry
	Items      []core.Title
	More       bool
	Collection core.RatingKey
}
type RowRequest struct {
	// HomeFallbackOnly requests a bounded, indexed hero fill through the normal
	// policy, owner layout and receipt pipeline. It is internal to Home.
	HomeFallbackOnly bool
	ExcludeItems     []core.RatingKey
	Viewer           *core.Viewer
	Device           core.Device
	Placement        Placement
	ViewID           string
	// Tab is a library's rows tab (page "library:<id>/<tab>"); "" is the
	// library's first rows tab.
	Tab   string
	Item  core.RatingKey
	Limit int
	// Purpose asks for one row. The zero value still returns the whole placement.
	Purpose RowPurpose
}

// RowInteractionKind is a viewer action on an issued row or entry.
type RowInteractionKind string

const (
	RowImpression RowInteractionKind = "impression"
	RowClick      RowInteractionKind = "click"
	RowPlay       RowInteractionKind = "play"
)

type RowInteraction struct {
	Viewer   *core.Viewer
	Device   core.Device
	Protocol Protocol
	Handle   string
	Kind     RowInteractionKind
}

// RowInteractionReporter validates issued handles, membership and device scope
// before recording actions. Clients must never construct handles or contexts.
type RowInteractionReporter interface {
	ReportRowInteraction(context.Context, RowInteraction) error
}

// FavoriteKind namespaces stable, policy-checked entity identifiers.
type FavoriteKind string

const (
	FavoriteTitle      FavoriteKind = "title"
	FavoritePerson     FavoriteKind = "person"
	FavoriteCollection FavoriteKind = "collection"
	FavoriteChannel    FavoriteKind = "channel"
	FavoriteEntity     FavoriteKind = "entity"
)

// FavoriteRef identifies an entity in its own namespace.
type FavoriteRef struct {
	Kind FavoriteKind
	ID   string
}

type ItemRequest struct {
	Viewer *core.Viewer
	Device core.Device
	Key    core.RatingKey
	// IncludeClientYouTube opts into YouTube trailers that can play on the device.
	IncludeClientYouTube bool
}

// DateFilter is the date predicate accepted by the shared catalogue boundary.
type DateFilter = core.DateFilter

// ProviderID is one external provider identifier requested by a client
// (for example Jellyfin/Emby AnyProviderIdEquals=tmdb.872585).
type ProviderID struct {
	Provider string // lowercased provider name: "tmdb", "imdb", "tvdb", ...
	ID       string // trimmed provider-specific id value
}

type ItemQuery struct {
	// AdjacentTo selects the anchor and its immediately neighboring visible items.
	AdjacentTo core.RatingKey
	// StartAt selects an inclusive starting item after authorization, filtering
	// and ordering. Offset and Limit apply to the resulting suffix. It cannot
	// be combined with AdjacentTo; an absent or filtered anchor is not found.
	StartAt       core.RatingKey
	Offset, Limit int
	Sort          core.SortOrder
	Desc          bool
	Types         []core.MediaType
	// UnservedTypes names the client's requested item kinds (Jellyfin/Emby
	// IncludeItemTypes, lowercased) that do not map to a core.MediaType. Empty
	// Types with a non-empty UnservedTypes means the client named only kinds
	// the catalogue cannot hold: the request must answer empty, not list every
	// type (Moonfin's Favorites screen asks for Person and the music kinds,
	// #405). A plugin library whose presentation declares the kind still
	// serves it; callers consult configured libraries before answering empty.
	UnservedTypes []string
	// ExcludedTypes contains core media type names excluded by the client.
	ExcludedTypes []core.MediaType
	// CatalogueOnly limits a search to the catalogue's own media classes
	// (Types): a client whose search filter offers each plugin library kind
	// separately (Vela's Movies and Shows) asked for films or series, not for
	// books, music or other plugin items that borrow that card shape.
	CatalogueOnly bool
	// Level selects a specific declared hierarchy level key on a plugin library.
	Level string
	// Role selects children matching a semantic role (or role-less level key) on a plugin library.
	Role          string
	Recursive     bool
	Season        *int // Optional presented season number.
	CollectionKey core.RatingKey
	Filter        core.Filter
	Watched       *bool
	Favorite      *bool
	// ProviderIDs holds external provider identifiers to match (OR semantics:
	// an item matches if it carries any listed provider id). When
	// FilterByProviderIDs is true, only matching items are returned; an empty
	// or unknown-provider list returns an empty page (Total 0) rather than
	// dropping the filter.
	ProviderIDs         []ProviderID
	FilterByProviderIDs bool
	// NoTotal is for clients that page without a total. A library page
	// answered from the catalogue's indexes then skips counting the matches,
	// which reads every title the filter admits; Page.Total is Offset plus
	// the items returned. Other pages still report their exact total.
	NoTotal bool
	// CheapTotal, when paired with NoTotal, populates Page.Total and sets
	// Page.TotalExact only when an exact count is available cheaply (tallies,
	// cached counts, or bounded index counts); otherwise Page.TotalExact stays
	// false and no full-library count runs.
	CheapTotal bool
	// SuppressExpand disables on-demand background plugin facet/person expansion
	// for internal shelf probes and search fan-out.
	SuppressExpand bool
	// ChildScanLimit bounds work for a stored plugin subtree probe. If the
	// subtree cannot be read within this window, the probe returns no items.
	ChildScanLimit int
	// PluginScanLimit bounds the candidate window when viewer policy requires
	// filtering a plugin library outside SQL.
	PluginScanLimit int
}

// Continuation distinguishes a known next page from exhaustion and a
// truncated source that cannot determine whether more matches exist.
type Continuation string

const (
	ContinuationMore      Continuation = "more"
	ContinuationExhausted Continuation = "exhausted"
	ContinuationUnknown   Continuation = "unknown"
)

type SearchPage struct {
	Descriptive  *DescriptiveSearchStatus
	Page         core.Page
	TotalExact   bool
	Continuation Continuation
	// Pending reports that a slower source (a plugin's remote catalogue) was
	// still answering: the page holds what Ether already had, and the same
	// search shortly returns more. Only searches run with
	// core.WithSearchPending answer early.
	Pending bool
}

// Image identifies an image authorized for an item. Path is an opaque,
// server-owned source reference: callers must neither interpret it as a URL nor
// construct one themselves. Tag changes when the selected artwork changes.
// Older adapters may still return a TMDB path while migrating to opaque refs.
type Image struct {
	Kind, Path, Tag string
	// Subject identifies an entity within the authorized item, such as the
	// tmdb:<id> person whose credit owns a portrait. It is part of the grant.
	Subject string
	// Width and Height describe the cached source pixels; zero means unknown.
	Width, Height int
	// BlurHash is a cached preview of the source; empty means unavailable.
	BlurHash string
	// ThumbHash is padded base64 of the cached source's standard ThumbHash;
	// empty means the background computation has not completed.
	ThumbHash string
}

// SameImage compares stable authorization identity, excluding optional metadata.
func SameImage(a, b Image) bool {
	return a.Kind == b.Kind && a.Path == b.Path && a.Tag == b.Tag && a.Subject == b.Subject
}

const (
	ImagePoster   = "poster"
	ImageBackdrop = "backdrop"
	ImageLogo     = "logo"
	ImageThumb    = "thumb"
	ImageBanner   = "banner"
	ImageStill    = "still"
	ImagePerson   = "person"
	ImageChannel  = "channel"
)

type Version struct {
	ID, Name string
	// Edition is the release's named cut when the catalogue recorded one.
	Edition string
	// AddedAt follows date added. Movies and episodes use their own release
	// or air date. Series and seasons use the latest aired episode, then
	// the premiere. It is not a catalogue insertion, observation, or refresh.
	AddedAt time.Time
	Media   core.MediaInfo
	// Markers are ranges for this version's probed timeline. Nil means the
	// version has no separate timeline yet; a detail-level marker set is not
	// a substitute when several files are present.
	Markers []Marker
	// Source facts are absent unless they are known for this version. FileName
	// is a basename only; DateModified is never a catalogue fallback date.
	FileName     string     `json:"fileName,omitempty"`
	PartCount    *int       `json:"partCount,omitempty"`
	ETag         string     `json:"etag,omitempty"`
	DateModified *time.Time `json:"dateModified,omitempty"`
	PublishedAt  *time.Time `json:"publishedAt,omitempty"`
}
type Marker struct {
	Type       string
	Start, End time.Duration
}

// SessionMarkers describes only a complete set bound to the current source.
// Known=false means the source has not established a complete marker set.
type SessionMarkers struct {
	Known      bool
	SourceETag string
	Markers    []Marker
}

// ExtraStreams resolves a playable trailer or clip the extras service registered.
// Open returns a direct media URL. Callers authorize the parent before Open.
type ExtraStreams interface {
	Lookup(core.RatingKey) (Extra, bool)
	Open(context.Context, core.RatingKey) (container, rawURL string, headers map[string]string, err error)
	Enabled() bool
	List(ctx context.Context, parent *core.Title, lang string) ([]ExtraClip, error)
	Clip(key core.RatingKey) (ExtraClip, bool)
	Resolve(ctx context.Context, e ExtraClip) (ResolvedExtra, error)
	PrewarmBytes() int64
}

// KindMedia is an opened byte-addressable stream for a plugin kind item.
type KindMedia struct {
	io.ReaderAt
	io.Closer
	Size        int64
	MIME        string
	Duration    time.Duration
	Name        string
	Release     core.Release
	MediaSource core.MediaSource
}

// KindLeaf is one playable leaf entry within a plugin kind container item.
type KindLeaf struct {
	Index    int
	Title    string
	Key      core.RatingKey
	Duration time.Duration
	Size     int64
	MIME     string
}

// KindStreams opens byte-addressable media and lists playable leaves for
// plugin-provided content kinds (spec §6.1). Callers authorize viewer access
// through viewer policy; nil on Host means plugin kind media is unavailable.
type KindStreams interface {
	OpenKind(ctx context.Context, viewer *core.Viewer, key core.RatingKey, accept []string) (*KindMedia, error)
	KindLeaves(ctx context.Context, viewer *core.Viewer, parent core.RatingKey) ([]KindLeaf, error)
}

// KindPart is one addressable file/part of a multi-part plugin kind item (#576).
type KindPart struct {
	Index    int
	Name     string
	MIME     string
	Size     int64
	Duration time.Duration
}

// KindPartStreams opens individual parts of a multi-part plugin kind item and
// lists declared parts without opening bytes (#576). Frontends and kind-media
// type-assert for it when serving per-part URLs.
type KindPartStreams interface {
	OpenKindPart(ctx context.Context, viewer *core.Viewer, key core.RatingKey, partIndex int, accept []string) (*KindMedia, error)
	KindParts(ctx context.Context, viewer *core.Viewer, key core.RatingKey) ([]KindPart, error)
}

// KindRefKeys maps between plugin RatingKeys and their ItemRefs for a viewer
// who may see them. Protocols that address plugin items by their ItemRef
// verbatim (the Stremio "pk:" identity, spec §2.2) use it to build catalog
// ids and to dispatch meta/stream requests back to the stored item. It is
// optional: frontends type-assert for it and 404 when the host does not
// implement it.
type KindRefKeys interface {
	KeyForRef(ctx context.Context, viewer *core.Viewer, ref core.ItemRef) (core.RatingKey, error)
	RefsForKeys(ctx context.Context, viewer *core.Viewer, keys []core.RatingKey) (map[core.RatingKey]core.ItemRef, error)
}

// CinemaIntros returns playable trailers of other visible, unwatched movies
// to play before feature. An empty slice means cinema intros are off or none match.
type CinemaIntros interface {
	Trailers(context.Context, *core.Viewer, core.Device, core.RatingKey, int) ([]Extra, error)
}

// Pre-roll placements relative to Cinema Trailers.
const (
	PrerollBeforeTrailers = core.PrerollBeforeTrailers
	PrerollAfterTrailers  = core.PrerollAfterTrailers
)

// Pre-roll request, clip and play values stay owned by core. Adapters
// exchange them through these aliases.
type PrerollRequest = core.PrerollRequest
type PrerollClip = core.PrerollClip
type PrerollPlay = core.PrerollPlay

// Prerolls are the owner's pre-rolls (#298), served as the first intro of
// a fresh movie or episode play. Intro reports the pre-roll before an item
// the front end already authorized (with its placement,
// PrerollBeforeTrailers or PrerollAfterTrailers), or false. Lookup and
// Open serve a ready pre-roll; it has no parent, and any signed-in viewer
// may play it. Started records a play once per viewer, pre-roll and
// feature.
type Prerolls interface {
	Intro(ctx context.Context, viewer *Viewer, device Device, item ItemDetail, away bool) (Extra, string, bool, error)
	Lookup(ctx context.Context, key RatingKey) (Extra, bool)
	Open(ctx context.Context, key RatingKey) (io.ReadSeekCloser, time.Time, error)
	Started(ctx context.Context, viewer *Viewer, device Device, key RatingKey)
	Active(ctx context.Context) bool
	Pick(ctx context.Context, req PrerollRequest) (PrerollClip, bool)
	Clip(ctx context.Context, key RatingKey) (PrerollClip, bool)
	Thumb(ctx context.Context, key RatingKey) (string, bool)
	RecordPlay(ctx context.Context, play PrerollPlay)
}

// SubtitleOffer is one subtitle search result. ID is the only value a later
// Download accepts, and only for the title Search returned it for.
type SubtitleOffer struct {
	ID, Language, Name, Provider, Format, Author     string
	Downloads                                        int
	Rating                                           float64
	HearingImpaired, Forced, HashMatch, AITranslated bool
}

// SubtitleCatalog searches and downloads sidecars. Download fetches only an
// offer Search returned for that title. It does not prefetch.
type SubtitleCatalog interface {
	Enabled() bool
	Search(context.Context, *core.Viewer, core.Device, core.RatingKey, string) ([]SubtitleOffer, error)
	Download(context.Context, *core.Viewer, core.Device, core.RatingKey, string) (int, error)
	OpenFormat(id, format string) (io.ReadCloser, string, error)
	OpenFormatFrom(id, format, source string) (io.ReadCloser, string, error)
	CachedSidecars(ctx context.Context, t *Title, u *User, embedded []StreamInfo) []StreamInfo
	SearchSubtitles(ctx context.Context, t *Title, u *User, lang, release string) ([]SubtitleSearchOffer, error)
	DownloadSubtitle(ctx context.Context, t *Title, u *User, id int64) (StreamInfo, error)
	UploadSubtitle(ctx context.Context, t *Title, u *User, filename, format, language string, body []byte) (StreamInfo, error)
	DeleteSubtitle(ctx context.Context, key RatingKey, u *User, stream StreamInfo) error
}

// SubtitleSidecarSource resolves cached OpenSubtitles and synchronized sidecars.
type SubtitleSidecarSource interface {
	OpenFormat(string, string) (io.ReadCloser, string, error)
}

// SubtitleSidecarCatalog is optionally implemented by Host.Subtitles: the sidecars
// already cached for a title and viewer, without network access, skipping
// streams embedded already lists (by ExternalURL) or covers (by language).
type SubtitleSidecarCatalog interface {
	CachedSidecars(ctx context.Context, t *Title, u *User, embedded []StreamInfo) []StreamInfo
}

// SubtitleSearchOffer is one subtitle search result.
type SubtitleSearchOffer struct {
	ID              int64  // provider file id, passed back to DownloadSubtitle
	Language        string // ISO 639-1
	Title           string // release or file name
	Provider        string // "OpenSubtitles"
	Score           int
	HearingImpaired bool
	Forced          bool
	PerfectMatch    bool // matched the playing file's hash or release
}

// SubtitleSidecarSearch is optionally implemented by Host.Subtitles: on-demand
// subtitle search and pick returning a sidecar stream.
type SubtitleSidecarSearch interface {
	SearchSubtitles(ctx context.Context, t *Title, u *User, lang, release string) ([]SubtitleSearchOffer, error)
	// DownloadSubtitle fetches an offer returned by SearchSubtitles for t
	// and returns its sidecar stream; ErrNotFound for unknown offers.
	DownloadSubtitle(ctx context.Context, t *Title, u *User, id int64) (StreamInfo, error)
}

// SubtitleSidecarUpload is optionally implemented by Host.Subtitles:
// viewer-uploaded sidecar subtitles.
type SubtitleSidecarUpload interface {
	UploadSubtitle(ctx context.Context, t *Title, u *User, filename, format, language string, body []byte) (StreamInfo, error)
}

// Extra describes a trailer or clip. Key is the item
// identity; Source and SourceID describe the upstream reference, not a resolved
// URL or credentials. Plex media and part identifiers remain adapter-owned.
type Extra struct {
	Key, ParentKey                          core.RatingKey
	Title, Kind, Source, SourceID, Language string
	Official                                bool
	PublishedAt                             time.Time
	Duration                                time.Duration
	Height                                  int
	ClientOnly                              bool // playback requires the viewer's device
}
type ItemDetail struct {
	Collection *core.Collection
	Playlist   *core.Playlist
	Title      core.Title
	UserData   core.WatchStatus
	// Unavailable is the plugin-declared reason playback is blocked.
	Unavailable *core.ItemUnavailable
	// CanPlay reflects current playback authorization when known. Nil means the
	// verified peer or policy result is unavailable. It does not guarantee a
	// source, codec, stream slot or transcode capacity will be available.
	CanPlay *bool
	// CanRead reflects current reading authorization for book items when known.
	// Nil on non-book items.
	CanRead *bool
	Tracks  []core.StreamInfo
	// TracksEstimated marks a set containing synthesized playback estimates.
	// These streams must not be presented as facts about a resolved source.
	TracksEstimated bool
	Images          []Image
	// ImagesAuthoritative reports whether Images describes the currently selected
	// authorized artwork. Background discovery may add alternatives later.
	ImagesAuthoritative bool
	Versions            []Version
	// Chapters carries probed or declared chapter markers for the item (for
	// example embedded audiobook chapters or part-derived chapters).
	Chapters []core.Chapter
	Markers  []Marker
	Extras   []Extra
}

// ResolvedEventSubject is viewer-authorized current data for an event entity.
// PersonFacetID shares the namespace used by person facets.
type ResolvedEventSubject struct {
	Ref           FavoriteRef
	PersonFacetID string
	Favorite      bool
	UserData      *core.WatchStatus
}

// ExternalRequest identifies an IMDb title with an optional canonical episode.
// Season and Episode must either both be set or both omitted.
type ExternalRequest struct {
	Source          string
	ID              string
	Type            core.MediaType
	Season, Episode *int
}

// CollectionQuery requests viewer-scoped collection pagination. A nonpositive
// Limit defaults to 50; larger requests are capped at 500.
type CollectionQuery struct {
	// ExistsOnly stops after the first visible collection; Total is then 0 or 1.
	ExistsOnly bool
	// NoTotal skips computing an exact total when the caller pages by cursor
	// or limit+1 (e.g. Vela's continuation token).
	NoTotal bool
	// After seeks from the last visible collection for a cursor client.
	// Offset remains available when the collection source cannot seek.
	After         core.RatingKey
	ViewID        string
	Query         string
	Types         []core.MediaType
	Offset, Limit int
}
type CollectionPage struct {
	Items []core.Collection
	Total int
}

// PageQuery defaults nonpositive Limit to 50 and caps it at 500.
type PageQuery struct {
	Offset, Limit int
	Query         string
	Level         string
}

// PlaylistPage is one viewer's own playlists. Item keys are titles that
// viewer is allowed to see, in playlist order.
type PlaylistPage struct {
	Items []core.Playlist
	Total int
}

// ErrBadPlaylist is an invalid playlist name, index, or empty change.
var ErrBadPlaylist = errors.New("invalid playlist request")

// MaxPlaylistEntries is the maximum number of entries permitted in a viewer
// playlist created or extended through the ViewerPlaylists contract.
const MaxPlaylistEntries = 5000

var (
	// ErrPlaylistReadOnly marks mutations on read-only playlists (such as smart
	// playlists or protected system playlists). It wraps ErrUnauthorized so
	// existing surfaces that reject protected mutations as unauthorized keep
	// their behavior while newer surfaces can report a read-only conflict.
	ErrPlaylistReadOnly = fmt.Errorf("%w: playlist read only", ErrUnauthorized)
	// ErrPlaylistTooLarge rejects playlist creation or additions that would
	// exceed MaxPlaylistEntries.
	ErrPlaylistTooLarge = errors.New("playlist exceeds maximum entry count")
	// ErrPlaylistUnsupportedKind rejects playlist entries whose title kind is
	// neither playable nor expandable to playable episodes.
	ErrPlaylistUnsupportedKind = errors.New("unsupported playlist entry kind")
)

// PlaylistEntry pairs a stable entry identifier with a visible playlist title.
type PlaylistEntry struct {
	EntryID string
	Title   core.Title
	// AddedAt is when the entry was added; zero when unknown.
	AddedAt time.Time
}

// PlaylistContents is a viewer's playlist metadata paired with its visible
// entries in playlist order.
type PlaylistContents struct {
	Playlist core.Playlist
	Entries  []PlaylistEntry
}

// PlayContexts remembers what a viewer plays *from* (an album, an artist, a
// playlist, their liked songs) for Recently played, keeping the newest few.
type PlayContexts interface {
	RecordPlayContext(ctx context.Context, viewer *core.Viewer, ref PlayContextRef) error
	RecentPlayContexts(ctx context.Context, viewer *core.Viewer, limit int) ([]RecentPlayContext, error)
}

// PlayContextRef names a play context: a title (album, artist, show), a
// playlist, or a liked list by role ("favorites" with Role "track").
type PlayContextRef struct {
	Kind string // "title", "playlist", "favorites"
	Key  core.RatingKey
	Role string
}

// RecentPlayContext is one play context resolved for the viewer.
type RecentPlayContext struct {
	Kind     string
	Title    *Title
	Playlist *core.Playlist
	Role     string
	PlayedAt time.Time
}

// TitleContinuations say what plays after a playable title when a queue
// ends (radio), from the title's source. Asked as the queue nears its end.
type TitleContinuations interface {
	PluginContinuation(ctx context.Context, viewer *core.Viewer, key core.RatingKey, limit int) ([]Title, error)
	ContinuationAvailable() bool
}

// TitleLyrics reads a playable title's lyrics from its source (a kind
// plugin that declares them). Asked when a viewer opens the lyrics.
type TitleLyrics interface {
	TitleLyrics(ctx context.Context, viewer *core.Viewer, key RatingKey) (Lyrics, error)
	// LyricsAvailable reports whether any source can answer at all.
	LyricsAvailable(ctx context.Context) bool
}

// LyricAvailability describes what is already known about a title's lyrics.
type LyricAvailability uint8

const (
	LyricsAbsent LyricAvailability = iota
	LyricsPossible
	LyricsPlain
	LyricsTimed
)

// TitleLyricsHint reports availability from the projected title and in-memory
// answers. It performs no I/O and is safe to call for each title in a page.
type TitleLyricsHint interface {
	TitleLyricsAvailability(t Title) LyricAvailability
}

// FavoriteTitles lists a viewer's liked titles of one role (tracks for Liked
// Songs, albums, artists they follow, movies…) one page at a time, in the
// viewer's order. A page reads only that viewer's likes of that role.
type FavoriteTitles interface {
	FavoriteTitles(ctx context.Context, viewer *core.Viewer, q FavoriteTitlesQuery) (FavoriteTitlesPage, error)
}

// FavoriteTitlesQuery names the role ("track", "album", "artist", "movie" …),
// where to continue and how many to return.
type FavoriteTitlesQuery struct {
	Role   string
	Cursor string
	Limit  int
}

// FavoriteTitlesPage is one page; Total counts the viewer's likes of the role.
type FavoriteTitlesPage struct {
	Items []LikedTitle
	Next  string
	Total int
}

// LikedTitle is one liked title and when it was liked.
type LikedTitle struct {
	Title   Title
	LikedAt time.Time
}

// ErrBadFavoritesCursor rejects a cursor this server did not issue.
var ErrBadFavoritesCursor = errors.New("bad favorites cursor")

// TitlePlaylistMembership reports one viewer playlist and every visible entry
// identifier within it that references the queried title.
type TitlePlaylistMembership struct {
	Playlist core.Playlist
	EntryIDs []string
}

// PlaylistAvailability reports whether viewer playlists are backed by storage.
type PlaylistAvailability interface {
	PlaylistsAvailable() bool
}

// ViewerPlaylists exposes viewer-owned playlists with stable entry identities,
// show/season expansion, rename, and per-title membership lookup. All reads
// and mutations enforce viewer visibility policy before counting or indexing.
// ViewerPlaylistEditor edits a viewer-owned playlist's name and description
// (nil leaves a field as it is).
type ViewerPlaylistEditor interface {
	EditViewerPlaylist(ctx context.Context, viewer *core.Viewer, key core.RatingKey, name, summary *string) (*core.Playlist, error)
}

type ViewerPlaylists interface {
	PlaylistLibrary
	ViewerPlaylistContents(ctx context.Context, viewer *core.Viewer, key core.RatingKey) (PlaylistContents, error)
	CreateViewerPlaylist(ctx context.Context, viewer *core.Viewer, name string, items []core.RatingKey) (*core.Playlist, error)
	RenameViewerPlaylist(ctx context.Context, viewer *core.Viewer, key core.RatingKey, name string) (*core.Playlist, error)
	AddViewerPlaylistEntries(ctx context.Context, viewer *core.Viewer, key core.RatingKey, items []core.RatingKey) (int, error)
	RemoveViewerPlaylistEntry(ctx context.Context, viewer *core.Viewer, key core.RatingKey, entryID string) error
	MoveViewerPlaylistEntry(ctx context.Context, viewer *core.Viewer, key core.RatingKey, entryID, afterEntryID string) error
	TitlePlaylists(ctx context.Context, viewer *core.Viewer, titleKey core.RatingKey) ([]TitlePlaylistMembership, error)
}

// PlaylistLibrary is the viewer's playlists. Remove and move indexes are
// positions in the visible item list, never raw storage positions that
// include titles hidden from this viewer.
type PlaylistLibrary interface {
	Playlists(context.Context, *core.Viewer, PageQuery) (PlaylistPage, error)
	CreatePlaylist(context.Context, *core.Viewer, string, []core.RatingKey) (*core.Playlist, error)
	AddPlaylistItems(context.Context, *core.Viewer, core.RatingKey, []core.RatingKey) error
	RemovePlaylistItem(context.Context, *core.Viewer, core.RatingKey, int) error
	MovePlaylistItem(context.Context, *core.Viewer, core.RatingKey, int, int) error
	DeletePlaylist(context.Context, *core.Viewer, core.RatingKey) error
}

// PlaylistEdit updates only explicitly supplied fields. Its storage stays
// owned by core.
type PlaylistEdit = core.PlaylistEdit

// PlaylistMutation addresses members by stable item identity. Its storage
// stays owned by core.
type PlaylistMutation = core.PlaylistMutation

// PlaylistEditor is the optional playlist field-edit capability a playlist
// store may expose.
type PlaylistEditor interface {
	EditPlaylist(context.Context, int64, RatingKey, PlaylistEdit) (*Playlist, error)
}

// PlaylistMutator is the optional identity-addressed playlist mutation
// capability a playlist store may expose.
type PlaylistMutator interface {
	MutatePlaylist(context.Context, int64, RatingKey, PlaylistMutation) (*Playlist, int, error)
}

// SmartPlaylists persists a section query for evaluation at read time.
type SmartPlaylists interface {
	CreateSmartPlaylist(ctx context.Context, userID int64, title, uri string) (*Playlist, error)
}

type NotInterestedEntry struct {
	Key     core.RatingKey
	AddedAt time.Time
}
type NotInterestedPage struct {
	Items []NotInterestedEntry
	Total int
}

// Experience applies viewer policy before returning any data. Browse operations
// must not resolve or open a release. Policy is applied before pagination and limits.
type Experience interface {
	ExperienceServices
	LibraryGUIDs() LibraryGUIDs
	LibraryMetadata() LibraryMetadata
	MarkerRanges
	// PublishedViews lists protocol-published libraries without viewer policy.
	// Callers must apply the real viewer's restrictions before serving them.
	PublishedViews(context.Context) ([]View, error)
	Views(context.Context, *core.Viewer) ([]View, error)
	Collections(context.Context, *core.Viewer, core.Device, CollectionQuery) (CollectionPage, error)
	NotInterested(context.Context, *core.Viewer, core.Device, PageQuery) (NotInterestedPage, error)
	NotInterestedItem(context.Context, *core.Viewer, core.Device, core.RatingKey) (NotInterestedEntry, error)
	// ResolveExternal resolves an IMDb ID without opening a release, and returns
	// a key only after enforcing current viewer and inherited-show policy.
	ResolveExternal(context.Context, *core.Viewer, core.Device, ExternalRequest) (core.RatingKey, error)
	Rows(context.Context, RowRequest) ([]Row, error)
	Item(context.Context, ItemRequest) (ItemDetail, error)
	Children(context.Context, *core.Viewer, core.Device, core.RatingKey, ItemQuery) (core.Page, error)
	LibraryItems(context.Context, *core.Viewer, core.Device, string, ItemQuery) (core.Page, error)
	Similar(context.Context, *core.Viewer, core.Device, core.RatingKey, ItemQuery) (SearchPage, error)
	Ancestors(context.Context, *core.Viewer, core.Device, core.RatingKey) ([]core.Title, error)
	Facets(context.Context, *core.Viewer, core.Device, string, string) ([]core.FacetValue, error)
	ResolveFacet(context.Context, *core.Viewer, core.Device, string, string, string) (core.FacetValue, error)
	// PersonFacetIDs returns IDs only for credited people of an authorized title.
	// Episode-only credits may not appear in library-wide Facets or ResolveFacet.
	// Requests are bounded to 256 positive TMDB IDs; missing credits are omitted.
	PersonFacetIDs(context.Context, *core.Viewer, core.Device, core.RatingKey, []int64) (map[int64]string, error)
	// NamedFacetIDs resolves exact genre or studio names on one visible title.
	// A request contains at most 256 nonempty names; missing names are omitted.
	NamedFacetIDs(context.Context, *core.Viewer, core.Device, core.RatingKey, string, []string) (map[string]string, error)
	Search(context.Context, *core.Viewer, core.Device, string, ItemQuery) (SearchPage, error)
	UserData(context.Context, *core.Viewer, []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error)
	SetPlayed(context.Context, *core.Viewer, core.RatingKey) error
	SetUnplayed(context.Context, *core.Viewer, core.RatingKey) error
	SetFavorite(context.Context, *core.Viewer, core.RatingKey, bool) error
	SetNotInterested(context.Context, *core.Viewer, core.RatingKey, bool) error
	SetWatchlist(context.Context, *core.Viewer, core.RatingKey, bool) error
	SetEntityFavorite(context.Context, *core.Viewer, FavoriteRef, bool) error
	EntityFavorites(context.Context, *core.Viewer, []FavoriteRef) (map[FavoriteRef]bool, error)
	ResolveEventSubject(context.Context, *core.Viewer, core.Device, FavoriteRef) (ResolvedEventSubject, error)
	SetRating(context.Context, *core.Viewer, core.RatingKey, float64) error
	SetLikes(context.Context, *core.Viewer, core.RatingKey, *bool) error
	RemoveFromContinue(context.Context, *core.Viewer, core.RatingKey) error
}

// ReadingProgressWriter is an optional interface implemented by Experience
// hosts that persist book reading progress alongside video/audio watch state.
type ReadingProgressWriter interface {
	SetReadingProgress(ctx context.Context, viewer *core.Viewer, key core.RatingKey, progress core.ReadingProgress) error
}

// TitleBatchReader is an optional interface implemented by Experience hosts
// that batch-authorize and return titles for a list of rating keys without
// per-item detail enrichment, user-data, or artwork queries.
type TitleBatchReader interface {
	BatchTitles(ctx context.Context, viewer *core.Viewer, keys []core.RatingKey) ([]core.Title, error)
}

type PlayIntent struct {
	// OriginalPlayback declares an installed demux/decoder runtime independently
	// of native media element capabilities. Originals are pinned for byte reads.
	OriginalPlayback *OriginalPlaybackCapabilities

	Estimate bool
	Options  core.DecideOptions
	// ReadChapters resolves deferred container chapters for an explicit detail request.
	ReadChapters bool
	// ExactVersion treats PlanRequest.Version as a scoped opaque handle and
	// refuses any substitute release, including during later source failover.
	ExactVersion bool
	// AutoVersion marks an ExactVersion handle that Auto chose rather than
	// the viewer: it opens that release, then fails over like Auto.
	AutoVersion bool
	// Set flags distinguish omitted choices from stream index zero and
	// an explicit subtitle-off choice (SubtitleStream == -1).
	AudioSet    bool
	SubtitleSet bool
	// AudioOrdinal and SubtitleOrdinal select a zero-based position among the
	// opened, probed source's audio or subtitle streams, in source order (the
	// order a client's track inventory lists them). SubtitleOrdinal -1
	// explicitly disables subtitles. Nil preserves normal preferences. A
	// position the source cannot honour is no choice: viewer rules pick.
	AudioOrdinal    *int
	SubtitleOrdinal *int
	Position        time.Duration
	ClientSessionID string
	PlaybackID      string
	// AllowTranscoding is nil for the existing default; false forbids video
	// or audio transcoding while retaining any allowed direct-stream remux.
	AllowTranscoding *bool
	// PreferRemux asks for a copy-only remux when source changes would
	// otherwise disrupt the client. The broker still decides whether it applies.
	PreferRemux bool
	// Queued opens the next item of the device's play queue ahead of time
	// (gapless audio). It plays beside the device's current session and
	// replaces it only when it is first observed playing; preloading its
	// media does not. It never counts as a second stream.
	Queued bool
	// ViewerPlay marks a start a viewer pressed Play for: Vela's playback
	// request and Silo's playback start, nothing else. Jellyfin/Emby never
	// set it: some clients send IsPlayback true for queue prefetches, so a
	// PlaybackInfo is no proof of a Play. The broker then makes its source
	// preparation core.ExplicitPlay, unless the plan is queued or a detail
	// plan. Lazy stream fetches, version switches, failover and every other
	// plan leave it unset.
	ViewerPlay bool
	// DeclaredCaps marks Caps built from this request's complete device
	// declaration (a Jellyfin/Emby PlaybackInfo DeviceProfile, a Silo
	// capability body). The broker keeps the latest such caps per device so
	// a later detail page can warm the title with exactly what Play sends
	// (DetailWarmer); generic caps are never remembered.
	DeclaredCaps bool
	Hints        *PlayHints
}

// PlayHints carries Plex-specific selections that cannot be represented by
// neutral stream indexes. Only the Plex adapter should populate it.
type PlayHints struct {
	ManualSubtitle    bool // explicit request or stored manual choice, before preferences
	Collection        string
	MediaIndex        int
	PartReleaseID     string
	AudioSelection    int
	SubtitleSelection int
	SubtitleSet       bool
	SubtitleStream    *core.StreamInfo
}

type StreamFormat string

// ErrTranscodeCapacity classifies an exhausted shared encoder budget.
var ErrTranscodeCapacity = errors.New("transcode capacity exhausted")

// TranscodeCapacityError carries a suggested retry delay. Capacity is checked
// atomically at stream start and is not reserved by an earlier Plan.
type TranscodeCapacityError struct {
	RetryAfter time.Duration
	Cause      error
}

func (e *TranscodeCapacityError) Error() string        { return ErrTranscodeCapacity.Error() }
func (e *TranscodeCapacityError) Unwrap() error        { return e.Cause }
func (e *TranscodeCapacityError) Is(target error) bool { return target == ErrTranscodeCapacity }

const (
	StreamDirect StreamFormat = "direct"
	StreamHLS    StreamFormat = "hls"
	StreamDASH   StreamFormat = "dash"
	StreamHTTP   StreamFormat = "http"
)

// StreamRequest describes output, not an upstream URL or credential. The
// broker owns the input, decision and stream grant.
type StreamRequest struct {
	// AdaptiveHLS opts into rendition selection by the HLS player.
	AdaptiveHLS bool
	// AdaptiveMaxBitrate is the client-requested peak in bits per second.
	// Vela clamps estimates below the lowest policy-eligible rendition.
	AdaptiveMaxBitrate int

	// ExpectedOriginalRevision is set by a stable media URL resolving to a
	// direct-file grant. Empty requires the first representation.
	ExpectedOriginalRevision *string
	// PrepareOnly keeps original-file delivery authorized while preparing HLS.
	PrepareOnly bool

	// Track changes are source stream indices, validated before output restart.
	AudioStream, SubtitleStream *int
	// BurnImageSubtitles burns a selected image subtitle (PGS, VobSub, DVB)
	// into segmented output even when the client could draw it from the
	// original file: HLS cannot carry image subtitles.
	BurnImageSubtitles bool
	// ReencodeVideo replaces a copied video stream with an encoded one: the
	// client could not play the copy (a transcode rung of a replan ladder).
	ReencodeVideo bool
	// MaxHeight is a viewer's quality pick for a segmented stream: the video
	// is encoded no taller than it. A pick of 0 lifts an earlier cap. Nil
	// leaves the session's quality as it is.
	MaxHeight *int
	// LazyStart leaves the encoder idle until a rendition is requested.
	LazyStart bool
	// Continuous uses one uniform segment clock, without adaptive discontinuities.
	Continuous bool
	// FullTimeline publishes an HLS playlist that spans the whole title from
	// zero, as Jellyfin does, even when Position is a resume point. The client
	// seeks its player to Position; the encoder starts at the segment it asks
	// for. Position still records where the viewer starts.
	FullTimeline    bool
	Format          StreamFormat
	Position        time.Duration
	SegmentDuration time.Duration
}
type PlanRequest struct {
	// Version is a resolver-issued variant ID in legacy mode, or a scoped
	// VersionHandles handle when ExactVersion is set. Empty selects automatically.
	Version string
	// ResolvedRelease is the release id just returned by VersionHandles.Resolve
	// for Version, in this process. Plan checks that the handle row still names
	// that release and does not list offers again. Callers that did not resolve
	// leave it empty, and Plan validates the offer itself.
	ResolvedRelease string
	// ExpectedSourceETag binds playback to a previously offered resolved source.
	// It is not an authorization credential. A missing or changed source fails
	// closed instead of resolving a different release.
	ExpectedSourceETag string
	// DetailSource is an opaque, scoped handle returned by DetailPlan.
	DetailSource string
	Viewer       *core.Viewer
	Device       core.Device
	Protocol     Protocol
	Item         core.RatingKey
	// Channel is the stable Host.LiveTV channel ID. Exactly one of Item,
	// Channel, or DirectLive must be supplied.
	Channel    string
	DirectLive *DirectLiveTarget
	Multiview  *MultiviewTarget
	Caps       core.ClientCaps
	Intent     PlayIntent
}
type AutoSelectionReason = core.AutoSelectionReason

const (
	AutoConnectionEstimate = core.AutoConnectionEstimate
	AutoSourceDelivery     = core.AutoSourceDelivery
	AutoResumeSource       = core.AutoResumeSource
	AutoSourceSelection    = core.AutoSourceSelection
)

type PlayPlan struct {
	AutoSelectionReason        AutoSelectionReason
	SourceChosenAt, DecisionAt time.Time
	ID                         string
	Estimated                  bool
	Media                      core.MediaInfo
	Decision                   core.Decision
	// SelectedSubtitle holds the text subtitle stream selected for the client
	// to render beside the video when Decision.SubtitleStream was cleared to
	// -1 so the video encoder never burns it in. Nil means
	// Decision.SubtitleStream is already the authoritative selection.
	SelectedSubtitle *int
	// Alternatives are evaluated against this same source and client policy.
	// DirectPlay, DirectStream, Transcoding are evaluated against this same source.
	DirectPlay, DirectStream, Transcoding bool
	// VideoTranscodingOff reports that video re-encoding is disabled for movies
	// and shows (audio/subtitle conversion and remuxing may still run).
	VideoTranscodingOff bool
	// HLS reports whether this resolved source can be delivered as HLS under
	// the declared client profile. Encoder capacity is checked at stream start.
	HLS bool
	// HLSContainer is the authoritative segment container from the resolved HLS
	// output decision (mpegts, fmp4, or mkv). Empty means unknown or ineligible;
	// it is not the source container or an encoder-capacity reservation.
	HLSContainer string
	// HTTP reports whether this resolved source can be delivered as a
	// progressive HTTP transcode stream under the declared client profile.
	HTTP bool
	// HTTPContainer is the authoritative container from the resolved HTTP
	// progressive transcode decision (for example mp4, aac, mp3, ogg, mkv).
	HTTPContainer string
	// ETag identifies the resolved immutable source when available; it is
	// empty when the backend cannot prove a stable source identity.
	ETag string
	// DetailSource is a scoped, expiring handle for the resolved source.
	DetailSource string
	// VersionHandle is the opaque VersionHandles ID for the admitted version
	// when one was resolved or matched for this plan.
	VersionHandle string
	// ReleaseID is the internal resolved release identity used by frontends
	// to correlate the admitted source with offered VersionSlots (never
	// exposed directly on external APIs).
	ReleaseID string
	// OriginalOverClientCap is the direct-play decision for this source that
	// only the client's own streaming cap (Vela client.maxBitrate) ruled out;
	// owner caps still admit it. Frontends offer it as an explicit, unselected
	// choice. Nil when the original is selected, within the cap, or not
	// playable for another reason.
	OriginalOverClientCap *core.Decision
}

// SubtitleStream reports the subtitle stream selected for this play plan,
// preserving a side-rendered text track when Decision.SubtitleStream was
// cleared to keep the video decision burn-free.
func (p PlayPlan) SubtitleStream() int {
	if p.SelectedSubtitle != nil {
		return *p.SelectedSubtitle
	}
	return p.Decision.SubtitleStream
}

// DetailPlanner resolves a playable single item within a bounded admission
// budget. It returns source facts without creating a frontend session. The
// caller must separately obtain a scoped lazy stream capability.
type DetailPlanner interface {
	DetailPlan(ctx context.Context, request PlanRequest) (PlayPlan, error)
	ResolveDetailSource(ctx context.Context, scope VersionScope, handle string) (DetailSourceTarget, error)
	MatchDetailSession(ctx context.Context, scope VersionScope, handle, sessionID string) error
}

// StaleDetailResolver identifies an expired detail-source handle that belonged
// to the same authenticated viewer, device, and surface for an accessible item.
type StaleDetailResolver interface {
	ResolveStaleDetailParent(ctx context.Context, scope VersionScope, handle string) (RatingKey, error)
}

// SubtitleURLRequest describes one subtitle stream URL to authorize in a batch.
type SubtitleURLRequest struct {
	Index  int
	Format string
}

// SubtitleURLBatch authorizes multiple subtitle stream URLs for one session
// with a single viewer and policy check. Entries that cannot be delivered are
// returned as empty strings in the corresponding slice position.
type SubtitleURLBatch interface {
	SubtitleURLs(ctx context.Context, access SessionAccess, sessionID string, reqs []SubtitleURLRequest) ([]string, error)
}

type DetailSourceTarget struct {
	Item RatingKey
	ETag string
}
type OpenRequest struct {
	Protocol Protocol
	PlanID   string
	Viewer   *core.Viewer
	Device   core.Device
}
type Session struct {
	Purpose       string // preview grants never report library progress
	ID, MediaPath string
	Protocol      Protocol
	ViewerID      int64
	// ClientSessionID is a non-secret protocol installation/session identifier.
	ClientSessionID string
	Device          core.Device
	Item            core.RatingKey
	Channel         string
	Multiview       *MultiviewTarget
	Media           *core.MediaInfo
	StartedAt       time.Time
	Capabilities    *SessionCapabilities
	// PlayMethod is the broker's live delivery decision for this session:
	// "DirectPlay", "DirectStream" (remux) or "Transcode". Empty when the
	// session is idle, live, or restored on another cluster node.
	PlayMethod string
}

// SessionCapabilities are declared by an authenticated device on one surface.
// URL fields are metadata and must never be fetched by the broker.
type SessionCapabilities struct {
	PlayableMediaTypes           []string
	SupportedCommands            []string
	SupportsMediaControl         *bool
	SupportsPersistentIdentifier *bool
	SupportsRemoteControl        *bool
	DeviceProfile                json.RawMessage
	AppStoreURL                  string
	IconURL                      string
}

// ValidClientThroughput checks a measured client download rate in bits/s.
func ValidClientThroughput(bps float64) bool { return core.ValidClientThroughput(bps) }

type Timeline struct {
	// ThroughputBps is optional client download telemetry in bits/s.
	ThroughputBps float64 `json:"throughputBps,omitempty"`
	// Observed track indices are presence-aware client telemetry, not proof of
	// manual intent. In particular Jellyfin Web emits ordinary timeupdate events.
	AudioStreamIndex, SubtitleStreamIndex *int

	// PlaybackID may be empty for clients that do not send one. The broker
	// preserves its no-ID supersession semantics.
	PlaybackID      string
	ClientSessionID string
	// Sequence is optional; zero preserves the legacy unordered behavior.
	Sequence           uint64
	Position, Duration time.Duration
	State              string
}

type CommandKind string

const (
	CommandStop        CommandKind = "stop"
	CommandPause       CommandKind = "pause"
	CommandUnpause     CommandKind = "unpause"
	CommandSeek        CommandKind = "seek"
	CommandSetAudio    CommandKind = "setAudio"
	CommandSetSubtitle CommandKind = "setSubtitle"
	CommandMessage     CommandKind = "message"
)

type Command struct {
	Kind                CommandKind
	Position            time.Duration
	StreamIndex         int
	Message             string
	ControllerSessionID string
}
type Event struct {
	Type     string
	ViewerID int64
	// ActorViewerID is set from the authenticated actor for SessionCommand;
	// zero denotes a system-originated stop without an authenticated actor.
	ActorViewerID int64
	Item          core.RatingKey
	SessionID     string
	// SubjectKind and SubjectID identify non-title user data such as a
	// favorite person, collection or channel. Item remains the title key.
	SubjectKind     string
	SubjectID       string
	OriginProtocol  Protocol
	OriginSessionID string
	OriginClientID  string
	Command         *Command
	TargetProtocol  Protocol
	TargetDeviceID  string
	Session         *Session
	Timeline        *Timeline

	RowIDs []string // shelves whose contents changed
}

// TrickplayOffer is sprite geometry and coverage state for one generated width.
// Width and Height are one thumbnail in pixels. Columns and Rows are the
// grid inside each sheet. Interval is the time between thumbnails.
type TrickplayOffer struct {
	Width, Height int
	Columns, Rows int
	Count         int
	Interval      time.Duration
	Bandwidth     int
	CoverageUntil time.Duration
	Revision      uint64
	Generating    bool
}

// TrickplaySheetResult is one rendered or revalidated trickplay sheet.
type TrickplaySheetResult struct {
	Body        []byte
	Offer       TrickplayOffer
	Fallback    bool
	NotModified bool
}

// TrickplayCacheKey binds preview work to the selected item, release and
// opened source member. The identity stays owned by core.
func TrickplayCacheKey(s *PlaySession) string { return core.TrickplayCacheKey(s) }

// TrickplayCanceler is the optional capability that cancels preview work
// for a trickplay cache key.
type TrickplayCanceler interface{ CancelTrickplay(string) }

// TrickplayTiles serves preview sheets for an item the viewer may play.
// The broker owns release identity and generation. A false offer means no
// frame exists yet, which clients render as an empty trickplay map.
type TrickplayTiles interface {
	TrickplayOffer(context.Context, SessionAccess, core.RatingKey) (TrickplayOffer, bool)
	TrickplaySheet(context.Context, SessionAccess, core.RatingKey, int) ([]byte, error)
}

// SessionTrickplay serves read-only trickplay metadata and JPEG sheets bound
// strictly to one owning playback session without starting generation.
type SessionTrickplay interface {
	SessionTrickplayOffer(ctx context.Context, access SessionAccess, sessionID string) (TrickplayOffer, error)
	SessionTrickplaySheet(ctx context.Context, access SessionAccess, sessionID string, width, index int, unchanged func(TrickplayOffer) bool) (TrickplaySheetResult, error)
}

// SessionAccess binds client operations to an authenticated viewer and surface.
// Owner requests owner scope; it is not proof of authorization. The broker
// verifies the authenticated viewer is an active owner.
type SessionAccess struct {
	Viewer   *core.Viewer
	Device   core.Device
	Protocol Protocol
	Owner    bool
	// VisibleOnly restricts Sessions() to actively playing or explicitly
	// paused sessions (for activity/device lists), hiding freshly opened
	// Starting sessions before their first media byte.
	VisibleOnly bool
}

// ScopedMedia serves a stream only to the authenticated device that owns it.
// The scoped URL's grant and current playback policy are also checked.
type ScopedMedia interface {
	ServeMediaAccess(http.ResponseWriter, *http.Request, SessionAccess)
}

// Owner requests owner scope; the subscriber must verify active owner status.
type EventScope struct {
	Viewer *core.Viewer
	Owner  bool
	// Protocol suppresses this surface's own user-data invalidations.
	Protocol Protocol
	// SessionID selects the authenticated client session for targeted commands.
	SessionID string
	// Device binds a socket before a playback session exists and remains valid
	// when this authenticated device's current session changes.
	Device core.Device
}

// Sessions owns playback across protocols. Open revalidates the broker-owned
// plan, protocol, viewer, device and admission; estimated plans cannot be opened.
// MediaPath is scoped and unguessable and never contains account credentials.
type Sessions interface {
	Plan(context.Context, PlanRequest) (PlayPlan, error)
	Open(context.Context, OpenRequest) (Session, error)
	// StartStream revalidates access to the session and returns its scoped URL.
	StartStream(context.Context, SessionAccess, string, StreamRequest) (string, error)
	// SubtitleURL returns a scoped URL for a subtitle stream index and format.
	SubtitleURL(context.Context, SessionAccess, string, int, string) (string, error)
	// Markers returns source-bound ranges for the authorized playback session.
	Markers(context.Context, SessionAccess, string) (SessionMarkers, error)
	// ServeMedia authorizes the scoped grant and serves direct, HLS or DASH data.
	ServeMedia(http.ResponseWriter, *http.Request)
	// KeepAlive renews activity for an existing authorized session without changing playback or cancelling a stop.
	KeepAlive(context.Context, SessionAccess, string) error
	Report(context.Context, SessionAccess, string, Timeline) error
	Stop(context.Context, SessionAccess, string) error
	Command(context.Context, *core.Viewer, string, Command) error
	UpdateCapabilities(context.Context, SessionAccess, SessionCapabilities) error
	Capabilities(context.Context, SessionAccess) (SessionCapabilities, error)
	Sessions(context.Context, SessionAccess) ([]Session, error)
	Recover(context.Context, string, string, time.Duration) error
	TranscodeFor(context.Context, string) (core.HLSSession, bool)
	TrickplayAvailable(string) bool
	TrickplayBIF(string) ([]byte, bool)
	TrickplayFrame(string, time.Duration) ([]byte, bool)
	CancelTrickplay(string)
	// Events includes events from all cluster nodes, filters by viewer unless owner access is authorized, and ends on cancellation.
	Events(context.Context, EventScope) (<-chan Event, error)
}

// ItemProgress records a Jellyfin or Emby timeline when the client play
// session is not one the broker opened. Infuse generates PlaySessionId and
// still reports the item and its file-shaped MediaSourceId.
type ItemProgress interface {
	ReportItemProgress(context.Context, SessionAccess, core.RatingKey, Timeline) error
}

// PlaybackSourceKeyer reports the resolved source identity for a playback session.
type PlaybackSourceKeyer interface {
	SourceKey(playID string) string
}

// PlaybackSourceReader opens a session's current source for direct reads;
// release ends the hold. A subtitle extraction uses it to read an indexed
// text track line by line instead of the whole file.
type PlaybackSourceReader interface {
	SourceReader(playID string) (src io.ReaderAt, size int64, release func(), ok bool)
}

// SoftSubtitleServer serves soft ASS subtitles directly from an active HLS remux session.
type SoftSubtitleServer interface {
	ServeSoftSubtitle(http.ResponseWriter, *http.Request, int) bool
}

// RowReceiptIssuer issues row-only receipts for server-owned, delivered snapshots.
// It rechecks current policy without composing Home again.
type RowReceiptIssuer interface {
	IssueRowReceipts(context.Context, RowRequest, []Row) ([]Row, error)
}

// FilterFacetReader supplies bounded indexed filter values without a title walk.
type FilterFacetReader interface {
	FilterFacets(context.Context, *Viewer, Device, string, string) ([]FacetValue, error)
}

// StreamDecisionReader returns the admitted decision after an authorized restart.
type StreamDecisionReader interface {
	StreamDecision(context.Context, SessionAccess, string) (core.Decision, error)
}

// OriginalRangeReader associates a direct byte-range read with its broker session.
type OriginalRangeReader interface {
	OriginalRange(ctx context.Context, access SessionAccess, sessionID, releaseID, revision string) core.OriginalRange
}

func WithOriginalRange(ctx context.Context, request core.OriginalRange) context.Context {
	return core.WithOriginalRange(ctx, request)
}

// OriginalPlaybackReader exposes a range-triggered original replacement or a
// terminal loss to session-aware clients.
type OriginalPlaybackReader interface {
	OriginalPlaybackState(context.Context, SessionAccess, string) (OriginalPlaybackState, error)
}

type OriginalPlaybackState struct {
	Unavailable                      bool
	Revision, URL, ReleaseID, Origin string
	Media                            core.MediaInfo
	Decision                         core.Decision
	Position                         time.Duration
}

// PlayingReleaseReader reports the release a session is reading now, after
// any in-session failover. It reads live session state only; it never
// resolves or searches.
type PlayingReleaseReader interface {
	PlayingRelease(context.Context, SessionAccess, string) (core.Release, core.MediaInfo, error)
}

// QualitySessions offers lazy variants of the pinned file using the broker ladder.
// Preparation returns only after a real media segment has been published.
type QualitySessions interface {
	ProactiveFallbackAvailable() bool
	QualityRungs(context.Context, SessionAccess, string) ([]HLSRung, error)
	PrepareQuality(context.Context, SessionAccess, string, string, time.Duration) (string, time.Duration, error)
}

// VideoTranscodingPolicy reports the owner's per-viewer "No video
// transcoding" switch, so a frontend can offer only rungs and releases that
// play without re-encoding video for that viewer.
type VideoTranscodingPolicy interface {
	VideoTranscodingDisabled(*User) bool
}

// PosterGenerationSource reports the poster presentation generation, which
// advances each time the owner saves poster settings so apps refetch posters.
type PosterGenerationSource interface {
	PosterGeneration() uint64
}

type NeutralImpression = core.NeutralImpression

// DeliveredRowRecorder is used by server adapters for native clients that
// cannot report row impressions. Only entries actually sent belong here.
type DeliveredRowRecorder interface {
	RecordDeliveredRows(*Viewer, []NeutralImpression)
}
