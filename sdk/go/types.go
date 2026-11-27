package etherplugin

import (
	"encoding/json"
	"fmt"
)

// PluginKind identifies the top-level plugin classification in manifest.ether.kind.
type PluginKind string

const (
	KindSource              PluginKind = "source"
	KindIntegration         PluginKind = "integration"
	KindNotificationChannel PluginKind = "notification_channel"
	KindLiveTVSource        PluginKind = "livetv_source"
	KindExtension           PluginKind = "extension"
	KindContent             PluginKind = "content"
)

// Capability names a capability declared in manifest.ether.capabilities.
type Capability string

const (
	CapStreamSource    Capability = "stream_source"
	CapDebridProvider  Capability = "debrid_provider"
	CapListSource      Capability = "list_source"
	CapLiveTV          Capability = "livetv_channels"
	CapSubtitles       Capability = "subtitles"
	CapMarkers         Capability = "markers"
	CapNotify          Capability = "notify"
	CapEvents          Capability = "events"
	CapFlowActions     Capability = "flow_actions"
	CapRankingStep     Capability = "ranking_step"
	CapRuleCondition   Capability = "rule_condition"
	CapTools           Capability = "tools"
	CapUI              Capability = "ui"
	CapCatalogProvider Capability = "catalog_provider"
	CapKindResolver    Capability = "kind_resolver"
	CapFrontendAdapter Capability = "frontend_adapter"
	CapMatcher         Capability = "matcher"
	CapPosterBadges    Capability = "poster_badges"
	CapLiveEvents      Capability = "live_events"
)

// Manifest is the EPP/1 manifest document served at GET /manifest.json.
type Manifest struct {
	ID            string               `json:"id"`
	Version       string               `json:"version"`
	Name          string               `json:"name"`
	Description   string               `json:"description,omitempty"`
	Logo          string               `json:"logo,omitempty"`
	Background    string               `json:"background,omitempty"`
	Types         []string             `json:"types,omitempty"`
	Catalogs      []StremioCatalogDecl `json:"catalogs,omitempty"`
	Resources     []any                `json:"resources,omitempty"`
	IDPrefixes    []string             `json:"idPrefixes,omitempty"`
	BehaviorHints *BehaviorHints       `json:"behaviorHints,omitempty"`
	Ether         *EtherManifestBlock  `json:"ether,omitempty"`
}

// BehaviorHints carries Stremio-compatible behavior hints.
type BehaviorHints struct {
	Adult                 bool `json:"adult,omitempty"`
	P2P                   bool `json:"p2p,omitempty"`
	Configurable          bool `json:"configurable,omitempty"`
	ConfigurationRequired bool `json:"configurationRequired,omitempty"`
}

// StremioCatalogDecl declares a Stremio catalog in the manifest.
type StremioCatalogDecl struct {
	Type  string              `json:"type"`
	ID    string              `json:"id"`
	Name  string              `json:"name,omitempty"`
	Extra []StremioExtraField `json:"extra,omitempty"`
}

// StremioExtraField declares a supported extra parameter on a catalog.
type StremioExtraField struct {
	Name         string   `json:"name"`
	IsRequired   bool     `json:"isRequired,omitempty"`
	Options      []string `json:"options,omitempty"`
	OptionsLimit int      `json:"optionsLimit,omitempty"`
}

// EtherManifestBlock holds the EPP/1 `ether` manifest block.
type EtherManifestBlock struct {
	Protocol     int             `json:"protocol"`
	Kind         PluginKind      `json:"kind"`
	Capabilities []Capability    `json:"capabilities,omitempty"`
	Summary      string          `json:"summary"`
	Author       *PluginAuthor   `json:"author,omitempty"`
	License      string          `json:"license,omitempty"`
	Repo         string          `json:"repo,omitempty"`
	Icon         string          `json:"icon,omitempty"`
	Runtime      RuntimeDecl     `json:"runtime"`
	Cluster      string          `json:"cluster,omitempty"`
	Health       *HealthDecl     `json:"health,omitempty"`
	Permissions  *Permissions    `json:"permissions,omitempty"`
	Settings     *SettingsSchema `json:"settings,omitempty"`
	Strings      map[string]any  `json:"strings,omitempty"`
	Schedule     []ScheduleEntry `json:"schedule,omitempty"`
	Contributes  *Contributes    `json:"contributes,omitempty"`
}

// PluginAuthor identifies the plugin author.
type PluginAuthor struct {
	Name string `json:"name,omitempty"`
	URL  string `json:"url,omitempty"`
}

// RuntimeDecl declares how the plugin is executed.
type RuntimeDecl struct {
	Kind   string         `json:"kind"`
	Entry  string         `json:"entry,omitempty"`
	Module string         `json:"module,omitempty"`
	Limits *RuntimeLimits `json:"limits,omitempty"`
}

// RuntimeLimits requests hosted/data resources within owner-controlled ceilings.
// Defaults: 32 MiB heap, 12h, effectively unlimited background work units.
type RuntimeLimits struct {
	FetchBytes           int64  `json:"fetchBytes,omitempty"`
	ArchiveMemberBytes   int64  `json:"archiveMemberBytes,omitempty"`
	ArchiveExpandedBytes int64  `json:"archiveExpandedBytes,omitempty"`
	ArchiveMembers       int    `json:"archiveMembers,omitempty"`
	MaxItems             int    `json:"maxItems,omitempty"`
	MemoryBytes          uint64 `json:"memoryBytes,omitempty"`
	ReadBytes            int64  `json:"readBytes,omitempty"`
	Deadline             string `json:"deadline,omitempty"`
	UnitBudget           int64  `json:"unitBudget,omitempty"`
	// PoolSize requests the hosted WASM instance pool size (default 2; default
	// owner ceiling 4, raisable up to 16).
	PoolSize int `json:"poolSize,omitempty"`
}

// HealthDecl configures periodic health checks.
type HealthDecl struct {
	Interval string `json:"interval,omitempty"`
}

// ScheduleEntry declares a scheduled background job.
type ScheduleEntry struct {
	Every string `json:"every"`
	Call  string `json:"call"`
}

// Permissions declares the host access a plugin requests.
type Permissions struct {
	Network     []string           `json:"network,omitempty"`
	Catalog     string             `json:"catalog,omitempty"`
	Viewers     *ViewerPermissions `json:"viewers,omitempty"`
	Collections string             `json:"collections,omitempty"`
	Folders     string             `json:"folders,omitempty"`
	Secrets     []string           `json:"secrets,omitempty"`
	// Sources lets a kind resolver ask Ether to find bytes:
	// ""|none|search|search+check|acquire|acquire+check. The "+check" variants
	// also allow background usenet_check calls in jobs and schedules within the
	// owner's per-plugin daily allowance.
	Sources string `json:"sources,omitempty"`
	// AI lets a plugin call AIComplete on the owner's configured language model
	// (""|none|complete|complete+search; "complete" allows actions, jobs, sync,
	// and other non-viewer routes, while "complete+search" also allows bounded
	// 20 s cached calls on interactive search routes).
	AI string `json:"ai,omitempty"`
	// Media lets a plugin call MediaProbe (media.probe) from jobs, actions,
	// and background sync (""|none|probe; refused on viewer/client paths).
	Media   string `json:"media,omitempty"`
	Storage string `json:"storage,omitempty"` // ""|none|durable
}

// ViewerPermissions scopes viewer identity and watch-history access.
type ViewerPermissions struct {
	Identity        string `json:"identity,omitempty"`
	WatchHistory    string `json:"watch_history,omitempty"`
	WriteWatchState string `json:"write_watch_state,omitempty"`
}

// SettingsSchema describes a plugin's configuration fields.
type SettingsSchema struct {
	Ref    string          `json:"$ref,omitempty"`
	Fields []SettingField  `json:"fields,omitempty"`
	Groups []SettingsGroup `json:"groups,omitempty"`
}

// SettingsGroup groups related settings fields.
type SettingsGroup struct {
	Key      string         `json:"key,omitempty"`
	Label    string         `json:"label,omitempty"`
	Hint     string         `json:"hint,omitempty"`
	Advanced bool           `json:"advanced,omitempty"`
	Fields   []SettingField `json:"fields"`
}

// SettingField describes one leaf or group setting field.
type SettingField struct {
	Key         string         `json:"key"`
	Kind        string         `json:"kind"`
	Label       string         `json:"label,omitempty"`
	Hint        string         `json:"hint,omitempty"`
	Required    bool           `json:"required,omitempty"`
	Default     any            `json:"default,omitempty"`
	Options     []FieldOption  `json:"options,omitempty"`
	Fields      []SettingField `json:"fields,omitempty"`
	Min         *float64       `json:"min,omitempty"`
	Max         *float64       `json:"max,omitempty"`
	Placeholder string         `json:"placeholder,omitempty"`
}

// FieldOption is one selectable option for a select or multiSelect field.
type FieldOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Hint  string `json:"hint,omitempty"`
}

// Contributes declares the descriptors a plugin contributes to Ether.
type Contributes struct {
	Source           *SourceMeta           `json:"source,omitempty"`
	Channels         *ChannelsMeta         `json:"channels,omitempty"`
	Subtitles        *SubtitlesMeta        `json:"subtitles,omitempty"`
	Markers          *MarkersMeta          `json:"markers,omitempty"`
	Notification     *NotificationMeta     `json:"notification,omitempty"`
	Events           *EventsMeta           `json:"events,omitempty"`
	FlowActions      []FlowActionMeta      `json:"flowActions,omitempty"`
	Integration      *IntegrationMeta      `json:"integration,omitempty"`
	ListSources      []ListSourceMeta      `json:"listSources,omitempty"`
	RankingSteps     []RankingStepMeta     `json:"rankingSteps,omitempty"`
	RuleConditions   []RuleConditionMeta   `json:"ruleConditions,omitempty"`
	PosterBadges     []PosterBadgeSetMeta  `json:"posterBadges,omitempty"`
	Tools            []ToolMeta            `json:"tools,omitempty"`
	Pages            []PageMeta            `json:"pages,omitempty"`
	Slots            []SlotMeta            `json:"slots,omitempty"`
	Commands         []CommandMeta         `json:"commands,omitempty"`
	MediaKinds       []MediaKindMeta       `json:"mediaKinds,omitempty"`
	FrontendAdapters []FrontendAdapterMeta `json:"frontendAdapters,omitempty"`
	LiveFeeds        []LiveFeedMeta        `json:"liveFeeds,omitempty"`
	Setup            *SetupMeta            `json:"setup,omitempty"`
	Screens          []ScreenMeta          `json:"screens,omitempty"`
	Surfaces         []SurfaceMeta         `json:"surfaces,omitempty"`
}

// ScreenMeta declares one generic top-level screen contributed by a plugin.
type ScreenMeta struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Icon      string   `json:"icon,omitempty"`
	Frontends []string `json:"frontends,omitempty"`
	// Pages are the screen's documents by page id ("index" first); each is
	// a package file or a list of renderer variants (docs/plugin-screens.md).
	Pages         map[string]any          `json:"pages,omitempty"`
	Placement     string                  `json:"placement,omitempty"` // navigation|hidden
	ContentRating string                  `json:"contentRating,omitempty"`
	Default       *bool                   `json:"default,omitempty"`
	Actions       map[string]ScreenAction `json:"actions,omitempty"`
}

// ScreenAction declares a plugin-handled action documents call.
type ScreenAction struct {
	Args    map[string]ScreenActionArg `json:"args,omitempty"`
	Confirm string                     `json:"confirm,omitempty"`
	Rate    string                     `json:"rate,omitempty"` // normal|low
}

// ScreenActionArg is one typed action argument.
type ScreenActionArg struct {
	Type     string   `json:"type"` // string|int|bool|date|ref|strings
	Max      int      `json:"max,omitempty"`
	Min      int      `json:"min,omitempty"`
	Enum     []string `json:"enum,omitempty"`
	Required bool     `json:"required,omitempty"`
}

// SurfaceMeta declares a document drawn in a host slot.
type SurfaceMeta struct {
	ID            string          `json:"id"`
	Presentation  string          `json:"presentation"` // playerOverlay|toast|homeRow|titlePanel
	Title         string          `json:"title"`
	Anchor        string          `json:"anchor,omitempty"`
	Visibility    string          `json:"visibility,omitempty"`
	Setting       *SurfaceSetting `json:"setting,omitempty"`
	When          *SurfaceWhen    `json:"when,omitempty"`
	TTL           int             `json:"ttl,omitempty"`
	ContentRating string          `json:"contentRating,omitempty"`
	Default       *bool           `json:"default,omitempty"`
	Document      any             `json:"document"`
	Frontends     []string        `json:"frontends,omitempty"`
}

// SurfaceSetting is a player overlay's player-menu switch.
type SurfaceSetting struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Default bool   `json:"default"`
}

// SurfaceWhen limits where a surface applies.
type SurfaceWhen struct {
	Event *struct {
		Feed string `json:"feed"`
	} `json:"event,omitempty"`
	Kind      string `json:"kind,omitempty"`
	TitleKind string `json:"titleKind,omitempty"`
}

// MinLiveFeedIntervalSec is the minimum polling interval a live feed may declare.
const MinLiveFeedIntervalSec = 10

// LiveFeedMeta declares one live events feed contributed by a plugin.
type LiveFeedMeta struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	MinIntervalSec int      `json:"minIntervalSec"`
	EntityKinds    []string `json:"entityKinds,omitempty"`
}

// Poster badge bounds (#594).
const (
	MaxPosterBadgeSets     = 4
	MaxBadgeEntriesPerCall = 500
	MaxBadgeLabelsPerTitle = 3
	MaxBadgeLabelLen       = 16
	PosterBadgeQuotaTitles = 200_000
)

// PosterBadgeSetMeta declares one poster overlay badge set contributed by a plugin (#594).
type PosterBadgeSetMeta struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// BadgeTitleRef identifies a canonical movie or show by TMDB and/or IMDb id.
type BadgeTitleRef struct {
	Type string `json:"type"`
	TMDB int64  `json:"tmdb,omitempty"`
	IMDB string `json:"imdb,omitempty"`
}

// BadgeEntry pushes or clears (empty Labels) poster badges for one title.
type BadgeEntry struct {
	Title  BadgeTitleRef `json:"title"`
	Labels []string      `json:"labels"`
}

// BadgeUsage reports a plugin's badged title count and per-plugin title quota.
type BadgeUsage struct {
	Titles      int64 `json:"titles"`
	QuotaTitles int64 `json:"quotaTitles"`
}

// Presentations declares how a library is seen per protocol. Each app block
// describes how that app renders the library (never whether it is published,
// though Default may suggest starting off for an app). When an app block is
// omitted, Ether infers a fitting presentation from the kind's media class,
// level roles, and hierarchy shape.
type Presentations struct {
	AdapterOnly bool `json:"adapterOnly,omitempty"`
	Vela        *struct {
		Type    string `json:"type,omitempty"`    // movies|shows
		Default *bool  `json:"default,omitempty"` // optional default on/off suggestion
	} `json:"vela,omitempty"`
	Plex *struct {
		Base    string `json:"base,omitempty"`    // movie|show|artist|photo
		Default *bool  `json:"default,omitempty"` // optional default on/off suggestion
	} `json:"plex,omitempty"`
	MediaBrowser *struct {
		CollectionType string `json:"collectionType,omitempty"` // movies|tvshows|music|audiobooks|books|photos|homevideos
		// ItemTypes maps each level key to its Jellyfin/Emby item type:
		// Movie|Series|Season|Episode|Video|MusicArtist|MusicAlbum|Audio|
		// AudioBook|Book|PhotoAlbum|Photo. Unmapped levels are inferred from
		// the kind's media class, level roles, and hierarchy shape.
		ItemTypes map[string]string `json:"itemTypes,omitempty"`
		Default   *bool             `json:"default,omitempty"` // optional default on/off suggestion
	} `json:"mediaBrowser,omitempty"`
	Stremio *struct {
		Type    string `json:"type,omitempty"`
		Default *bool  `json:"default,omitempty"`
	} `json:"stremio,omitempty"`
	Xtream *struct {
		Category string `json:"category,omitempty"`
		Default  *bool  `json:"default,omitempty"`
	} `json:"xtream,omitempty"`
	Silo *struct {
		Type    string `json:"type,omitempty"`
		Default *bool  `json:"default,omitempty"`
	} `json:"silo,omitempty"`
}

// MediaKindMeta declares one content kind; each kind becomes its own library.
type MediaKindMeta struct {
	Key     string          `json:"key"`
	Label   string          `json:"label"`
	Media   string          `json:"media,omitempty"` // video|music|audiobook|book|podcast|photo
	Library KindLibraryDecl `json:"library"`
	Levels  []KindLevel     `json:"levels"`
	Fields  []KindField     `json:"fields,omitempty"`
	Art     []KindArt       `json:"art,omitempty"`
	Present Presentations   `json:"present,omitempty"`
	Browse  KindBrowseDecl  `json:"browse"`
	Sync    KindSyncDecl    `json:"sync"`
	Cache   *KindCacheDecl  `json:"cache,omitempty"`
	Match   *KindMatchDecl  `json:"match,omitempty"`
	// Lyrics: the provider implements LyricsProvider (POST /ether/v1/kinds/{kind}/lyrics).
	Lyrics bool `json:"lyrics,omitempty"`
	// Related: the provider implements RelatedProvider (POST /ether/v1/kinds/{kind}/related).
	Related *KindRelatedDecl `json:"related,omitempty"`
}

// KindRelatedDecl names which relation purposes a kind answers.
type KindRelatedDecl struct {
	Similar      bool `json:"similar,omitempty"`
	Continuation bool `json:"continuation,omitempty"`
}

// MaxKindLibraryTabs is the maximum number of declared sub-tabs per kind library.
const MaxKindLibraryTabs = 50

// KindLibraryDecl is the library created for a kind on install.
type KindLibraryDecl struct {
	Name        string             `json:"name"`
	Icon        string             `json:"icon,omitempty"`
	Card        string             `json:"card,omitempty"`        // poster (default)|landscape|square
	DateDisplay string             `json:"dateDisplay,omitempty"` // year (default)|date
	Experience  string             `json:"experience,omitempty"`  // watch|listen|read|gallery|live or another id; defaults by media class
	Tabs        []KindLibraryTab   `json:"tabs,omitempty"`        // up to 50; defaults to Recommended + Library
	Shelves     []KindLibraryShelf `json:"shelves,omitempty"`     // up to 40; default Recommended shelves
}

// KindLibraryTabFilter defines saved filter rules on a "view" tab.
type KindLibraryTabFilter struct {
	Match   string               `json:"match,omitempty"`   // "all" (default) or "any"
	Rules   []KindLibraryTabRule `json:"rules,omitempty"`   // matching conditions
	Exclude []KindLibraryTabRule `json:"exclude,omitempty"` // excluded conditions
}

// KindLibraryTabRule is one filter condition inside a "view" tab's filters.
// Field may be "browse:<indexedField>", "viewer" (with value such as
// {"history":true}, {"watched":true}, {"unwatched":true}, {"inProgress":true},
// {"favorite":true}, {"watchlist":true}, {"lastViewedAfter":unix},
// {"lastViewedBefore":unix}), "year", "rating", or a mapped indexed field.
type KindLibraryTabRule struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value,omitempty"`
}

// KindLibraryTab declares one sub-tab shown inside a plugin library.
type KindLibraryTab struct {
	Key               string                `json:"key"`
	Label             string                `json:"label"`
	Kind              string                `json:"kind"`                        // recommended|library|collections|people|view|live
	Level             string                `json:"level,omitempty"`             // optional declared hierarchy level (library, view, collections or people)
	Facet             string                `json:"facet,omitempty"`             // declared browse.facets entry (optional on collections; required people-type facet on people)
	Order             string                `json:"order,omitempty"`             // count|name (default count on collections, name on people)
	Card              string                `json:"card,omitempty"`              // landscape|poster|square (live or recommended tabs; defaults to landscape on live)
	Layout            string                `json:"layout,omitempty"`            // grid (default)|list|timeline (view tabs only)
	Search            *bool                 `json:"search,omitempty"`            // collections/people/view/live only; default true
	SearchPlaceholder string                `json:"searchPlaceholder,omitempty"` // localizable placeholder (<=60 chars; collections/people/view/live when search is enabled)
	Sort              string                `json:"sort,omitempty"`              // one of browse.sorts or lastViewed (facet collections or view)
	Direction         string                `json:"direction,omitempty"`         // asc|desc (facet collections or view)
	Filters           *KindLibraryTabFilter `json:"filters,omitempty"`           // saved filter rules (view tabs only)
	Shelves           []KindLibraryShelf    `json:"shelves,omitempty"`           // recommended tabs only; overrides library.shelves for that tab
}

// MaxKindLibraryShelves is the maximum number of declared Recommended shelves per kind library.
const MaxKindLibraryShelves = 40

// KindLibraryShelf declares one default Recommended shelf for a plugin library.
// Each shelf gets the stable row ID "plugin:<viewID>:shelf:<key>" (or
// "plugin:<viewID>:tab:<tabId>:shelf:<key>" on per-tab shelves).
// Filters accepts a map[string][]string (facet map), *KindLibraryTabFilter,
// KindLibraryTabRule, or []KindLibraryTabRule.
type KindLibraryShelf struct {
	Key       string   `json:"key"`
	Label     string   `json:"label"`
	Level     string   `json:"level,omitempty"`     // optional declared hierarchy level key
	Sort      string   `json:"sort,omitempty"`      // released (default)|title|added|year|rating|index|lastViewed
	Direction string   `json:"direction,omitempty"` // desc (default)|asc
	Facet     string   `json:"facet,omitempty"`     // single-facet shorthand (declared and indexed)
	Value     string   `json:"value,omitempty"`     // single value for facet
	Values    []string `json:"values,omitempty"`    // OR values for facet
	Filters   any      `json:"filters,omitempty"`   // map[string][]string, *KindLibraryTabFilter, KindLibraryTabRule or []KindLibraryTabRule
	Purpose   string   `json:"purpose,omitempty"`   // recentContexts (Jump back in) | liked (Liked songs)
	Layout    string   `json:"layout,omitempty"`    // default look: standard|hero|wide|tiles|tracks
}

// KindLevel is one hierarchy level of a kind.
type KindLevel struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Parent    string `json:"parent,omitempty"` // optional earlier level key; defaults to the previous level
	Media     string `json:"media,omitempty"`  // optional media class override: video|music|audiobook|book|podcast|photo
	Role      string `json:"role,omitempty"`   // free-text semantic role (e.g. artist, album, track, concert)
	Playable  bool   `json:"playable,omitempty"`
	Container bool   `json:"container,omitempty"`
	// Members: a list level (a playlist): a root of its own whose items name
	// other items of the kind as their ordered children (KindItem.Members).
	Members bool `json:"members,omitempty"`
}

// KindField is one declared item field.
type KindField struct {
	Key     string   `json:"key"`
	Type    string   `json:"type"` // string|int|float|bool|date|duration|enum|strings|people
	Label   string   `json:"label"`
	Indexed bool     `json:"indexed,omitempty"`
	Enum    []string `json:"enum,omitempty"`
	Values  []string `json:"values,omitempty"`
}

// KindArt declares one artwork role beyond poster/backdrop/thumb/logo.
type KindArt struct {
	Role  string `json:"role"`
	Label string `json:"label"`
}

// DefaultKindExpandTTLHours is the default freshness window (7 days) for
// on-demand facet and person slice expansions when ttlHours is omitted or zero.
const DefaultKindExpandTTLHours = 168

// KindBrowseExpand configures on-demand expansion for declared browse facets
// (when a viewer opens a facet bucket or plugin person) and/or container levels
// (when a viewer opens a container item or its children).
type KindBrowseExpand struct {
	Facets   []string `json:"facets,omitempty"`
	Levels   []string `json:"levels,omitempty"`
	TTLHours int      `json:"ttlHours,omitempty"`
}

// KindBrowseDecl is how a kind library is browsed.
type KindBrowseDecl struct {
	Sorts        []string `json:"sorts,omitempty"`
	Facets       []string `json:"facets,omitempty"`
	Search       bool     `json:"search"`
	SearchFields []string `json:"searchFields,omitempty"`
	// LiveSearch: client searches also call the plugin's /search route
	// with a short budget; results are upserted and merged (§15.4).
	LiveSearch bool              `json:"liveSearch,omitempty"`
	Expand     *KindBrowseExpand `json:"expand,omitempty"`
}

// Kind sync modes: pull (Ether walks browse/changes) or push (KindUpsert).
const (
	KindSyncPull = "pull"
	KindSyncPush = "push"
)

// Kind sync undo modes for push-synced kinds.
const (
	KindSyncUndoSnapshots = "snapshots"
	KindSyncUndoIDs       = "ids"
)

// KindSyncDecl is how a kind's items reach Ether's store.
type KindSyncDecl struct {
	Mode    string `json:"mode"` // pull|push
	Refresh string `json:"refresh,omitempty"`
	Undo    string `json:"undo,omitempty"` // snapshots|ids, push only
}

// KindCacheDecl is the per-library blob cache budget; 0 means server default.
type KindCacheDecl struct {
	MaxBytes int64  `json:"maxBytes,omitempty"`
	TTL      string `json:"ttl,omitempty"`
}

// KindMatchDecl holds matcher defaults for a kind.
type KindMatchDecl struct {
	ThresholdDefault float64 `json:"thresholdDefault,omitempty"`
}

// FrontendAdapterMeta declares one plugin-provided protocol surface.
type FrontendAdapterMeta struct {
	Key          string   `json:"key"`
	Label        string   `json:"label"`
	ProtocolHint string   `json:"protocolHint"` // opds|subsonic|custom
	PathPrefix   string   `json:"pathPrefix"`
	DefaultPort  int      `json:"defaultPort,omitempty"`
	Clients      []string `json:"clients,omitempty"`
	Auth         string   `json:"auth,omitempty"` // ether_token (default)|basic|none_lan|query_credentials
	Libraries    []string `json:"libraries,omitempty"`
	// SearchPaths optionally lists adapter subpaths (such as "/search",
	// "/rest/search3", or "/graphql") allowed to call ai.complete when
	// permissions.ai is "complete+search".
	SearchPaths []string             `json:"searchPaths,omitempty"`
	UploadPaths []FrontendUploadPath `json:"uploadPaths,omitempty"`
	// Icon is the adapter's own mark on the Apps page: a path to an SVG or
	// PNG inside the package. Without one the plugin's icon stands in.
	Icon string `json:"icon,omitempty"`
}

type FrontendUploadPath struct {
	Path     string `json:"path"`
	MaxBytes int64  `json:"maxBytes"`
}

type FrontendUpload struct {
	ContentType string               `json:"contentType,omitempty"`
	Blob        *FrontendUploadBlob  `json:"blob,omitempty"`
	Files       []FrontendUploadBlob `json:"files,omitempty"`
	Fields      map[string][]string  `json:"fields,omitempty"`
}

type FrontendUploadBlob struct {
	Ref         string `json:"ref"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	SHA1        string `json:"sha1"`
	MD5         string `json:"md5"`
	CRC32       string `json:"crc32"`
	ContentType string `json:"contentType,omitempty"`
	Field       string `json:"field,omitempty"`
	Filename    string `json:"filename,omitempty"`
}

// SetupMeta declares guided setup for any plugin kind.
type SetupMeta struct {
	Steps []SetupStep `json:"steps"`
}

// SetupStep is one setup wizard step; an empty Test means saving is the step.
type SetupStep struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Hint     string   `json:"hint,omitempty"`
	Fields   []string `json:"fields"`
	Test     string   `json:"test"`
	Required bool     `json:"required"`
}

// SourceMeta describes a stream source contribution.
type SourceMeta struct {
	Class     string         `json:"class"`
	Abilities []string       `json:"abilities,omitempty"`
	Limits    map[string]any `json:"limits,omitempty"`
	// Search opts a stream_source plugin in to category searches: Ether
	// asks GET /ether/search (Routes.SourceSearch) for the declared Newznab
	// categories, such as 3000 (audio) or 7000 (books).
	Search *SourceSearchMeta `json:"search,omitempty"`
}

// SourceSearchMeta declares the category searches a source plugin answers.
// A top category (3000) covers its subcategories (3010, 3040). Lookups name
// typed lookups (e.g. "album") whose fields Ether passes when a caller
// sends one.
type SourceSearchMeta struct {
	Categories []int    `json:"categories"`
	Lookups    []string `json:"lookups,omitempty"`
}

// ChannelsMeta describes a Live TV channel contribution.
type ChannelsMeta struct {
	Refresh string `json:"refresh,omitempty"`
	Test    bool   `json:"test"`
}

// SubtitlesMeta describes a subtitle provider contribution.
type SubtitlesMeta struct {
	Languages any      `json:"languages,omitempty"`
	Formats   []string `json:"formats,omitempty"`
}

// MarkersMeta describes a playback markers contribution.
type MarkersMeta struct {
	Kinds []string `json:"kinds"`
}

// NotificationMeta describes a notification channel contribution.
type NotificationMeta struct {
	Fields    any      `json:"fields,omitempty"`
	SendTest  bool     `json:"sendTest"`
	Formats   []string `json:"formats,omitempty"`
	MaxLength int      `json:"maxLength,omitempty"`
}

// EventsMeta declares subscribed event names and custom published event schemas.
type EventsMeta struct {
	Subscribe []string             `json:"subscribe,omitempty"`
	Publish   []PublishedEventMeta `json:"publish,omitempty"`
}

// EventFieldType classifies a typed field in a published event payload or
// flow action parameter/output schema.
type EventFieldType string

const (
	EventFieldString     EventFieldType = "string"
	EventFieldInt        EventFieldType = "int"
	EventFieldFloat      EventFieldType = "float"
	EventFieldBool       EventFieldType = "bool"
	EventFieldDurationMS EventFieldType = "duration_ms"
	EventFieldTimestamp  EventFieldType = "timestamp"
	EventFieldEnum       EventFieldType = "enum"
	EventFieldStringList EventFieldType = "string_list"
	EventFieldObject     EventFieldType = "object"
)

// EventEnumOption describes one allowed option for an enum field or parameter.
type EventEnumOption struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
}

// EventFieldMeta describes one typed field in a published plugin event payload
// or flow action output object.
type EventFieldMeta struct {
	Name        string            `json:"name"`
	Type        EventFieldType    `json:"type"`
	Label       string            `json:"label"`
	Description string            `json:"description,omitempty"`
	Unit        string            `json:"unit,omitempty"`
	Enum        []EventEnumOption `json:"enum,omitempty"`
	PII         bool              `json:"pii,omitempty"`
	Secret      bool              `json:"secret,omitempty"`
	Filterable  bool              `json:"filterable,omitempty"`
	Example     any               `json:"example,omitempty"`
}

// PublishedEventMeta declares one custom event emitted via PublishEvent
// ("plugin:<pluginId>/<key>").
type PublishedEventMeta struct {
	Key         string           `json:"key"`
	Label       string           `json:"label"`
	Description string           `json:"description,omitempty"`
	Version     int              `json:"version,omitempty"`
	Fields      []EventFieldMeta `json:"fields,omitempty"`
}

// FlowActionParamMeta describes one configurable parameter of a plugin flow action.
type FlowActionParamMeta struct {
	Key         string            `json:"key"`
	Type        EventFieldType    `json:"type"`
	Label       string            `json:"label"`
	Description string            `json:"description,omitempty"`
	Required    bool              `json:"required,omitempty"`
	Secret      bool              `json:"secret,omitempty"`
	Template    bool              `json:"template,omitempty"`
	Default     any               `json:"default,omitempty"`
	Enum        []EventEnumOption `json:"enum,omitempty"`
	Unit        string            `json:"unit,omitempty"`
	Picker      string            `json:"picker,omitempty"`
}

// FlowActionMeta declares one custom Flow action contributed by a plugin.
type FlowActionMeta struct {
	Key         string                `json:"key"`
	Label       string                `json:"label"`
	Description string                `json:"description,omitempty"`
	Icon        string                `json:"icon,omitempty"`
	Route       string                `json:"route"`
	Idempotent  bool                  `json:"idempotent,omitempty"`
	SideEffect  *bool                 `json:"sideEffect,omitempty"`
	Timeout     string                `json:"timeout,omitempty"`
	Params      []FlowActionParamMeta `json:"params,omitempty"`
	Outputs     []EventFieldMeta      `json:"outputs,omitempty"`
}

// EventCorrelation links a published plugin event to related Ether entities.
type EventCorrelation struct {
	Key       string `json:"key,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	PlayID    string `json:"playId,omitempty"`
	TitleID   string `json:"titleId,omitempty"`
	ViewerID  int64  `json:"viewerId,omitempty"`
	ChannelID string `json:"channelId,omitempty"`
	SourceID  string `json:"sourceId,omitempty"`
	PluginID  string `json:"pluginId,omitempty"`
	DeviceID  string `json:"deviceId,omitempty"`
	LibraryID string `json:"libraryId,omitempty"`
}

// FlowActionRequest is passed to a FlowAction route handler.
type FlowActionRequest struct {
	Key            string         `json:"key,omitempty"`
	FlowID         string         `json:"flowId"`
	RunID          string         `json:"runId"`
	NodeID         string         `json:"nodeId"`
	IdempotencyKey string         `json:"idempotencyKey"`
	DryRun         bool           `json:"dryRun,omitempty"`
	Params         map[string]any `json:"params,omitempty"`
}

// FlowActionResponse is returned by a FlowAction route handler.
type FlowActionResponse struct {
	Output map[string]any `json:"output,omitempty"`
	Branch string         `json:"branch,omitempty"`
}

// IntegrationMeta describes an external service integration.
type IntegrationMeta struct {
	Category string   `json:"category"`
	Logo     string   `json:"logo,omitempty"`
	Adds     []string `json:"adds"`
	Connect  string   `json:"connect"`
	Test     bool     `json:"test,omitempty"`
}

// ListSourceMeta describes a catalog list source.
type ListSourceMeta struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Hint     string `json:"hint,omitempty"`
	Catalog  string `json:"catalog"`
	Params   any    `json:"params,omitempty"`
	MaxItems int    `json:"maxItems,omitempty"`
	Refresh  string `json:"refresh,omitempty"`
}

// RankingStepMeta describes a custom ranking step.
type RankingStepMeta struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Hint   string `json:"hint,omitempty"`
	Editor any    `json:"editor,omitempty"`
	Budget any    `json:"budget,omitempty"`
}

// RuleConditionMeta describes a custom rule condition.
type RuleConditionMeta struct {
	Key       string   `json:"key"`
	Label     string   `json:"label"`
	Operators []string `json:"operators,omitempty"`
	Editor    any      `json:"editor,omitempty"`
}

// ToolMeta describes an agent tool contributed by a plugin.
type ToolMeta struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	ReadOnly    bool           `json:"readOnly,omitempty"`
	Destructive bool           `json:"destructive,omitempty"`
	OpenWorld   bool           `json:"openWorld,omitempty"`
}

// PageMeta describes a ViewSpec page contributed by a plugin.
type PageMeta struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Route string `json:"route"`
	Group string `json:"group,omitempty"`
	Icon  string `json:"icon,omitempty"`
}

// SlotMeta describes a UI slot contributed by a plugin.
type SlotMeta struct {
	Slot  string `json:"slot"`
	Key   string `json:"key"`
	Label string `json:"label"`
	Route string `json:"route"`
}

// CommandMeta describes a command contributed by a plugin.
type CommandMeta struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Action string `json:"action"`
}

// --- Route request and response types ---

// CatalogItem is one item in a catalog response.
type CatalogItem struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	Name        string   `json:"name"`
	Poster      string   `json:"poster,omitempty"`
	PosterShape string   `json:"posterShape,omitempty"`
	Background  string   `json:"background,omitempty"`
	Logo        string   `json:"logo,omitempty"`
	Description string   `json:"description,omitempty"`
	ReleaseInfo string   `json:"releaseInfo,omitempty"`
	IMDBRating  string   `json:"imdbRating,omitempty"`
	Genres      []string `json:"genres,omitempty"`
}

// CatalogRequest is passed to a Catalog route handler.
type CatalogRequest struct {
	Type   string            `json:"type"`
	ID     string            `json:"id"`
	Extra  map[string]string `json:"extra"`
	Skip   int               `json:"skip"`
	Search string            `json:"search,omitempty"`
	Genre  string            `json:"genre,omitempty"`
}

// CatalogResponse is returned by a Catalog route handler.
type CatalogResponse struct {
	Metas []CatalogItem `json:"metas"`
}

// MetaVideo describes one episode/video inside a MetaItem.
type MetaVideo struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Released  string `json:"released,omitempty"`
	Season    int    `json:"season,omitempty"`
	Episode   int    `json:"episode,omitempty"`
	Overview  string `json:"overview,omitempty"`
	Thumbnail string `json:"thumbnail,omitempty"`
}

// MetaItem is a full metadata record returned by a Meta route handler.
type MetaItem struct {
	CatalogItem
	Runtime  string      `json:"runtime,omitempty"`
	Cast     []string    `json:"cast,omitempty"`
	Director []string    `json:"director,omitempty"`
	Videos   []MetaVideo `json:"videos,omitempty"`
}

// MetaRequest is passed to a Meta route handler.
type MetaRequest struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// MetaResponse is returned by a Meta route handler.
type MetaResponse struct {
	Meta MetaItem `json:"meta"`
}

// StreamEtherExt holds Ether-specific extensions on a StreamItem.
type StreamEtherExt struct {
	ID          string   `json:"id,omitempty"`
	ReleaseName string   `json:"releaseName,omitempty"`
	Size        int64    `json:"size,omitempty"`
	Languages   []string `json:"languages,omitempty"`
	Lazy        bool     `json:"lazy,omitempty"`
	File        string   `json:"file,omitempty"`
}

// StreamItem is one playable candidate returned by a Stream route handler.
type StreamItem struct {
	Name        string          `json:"name,omitempty"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	URL         string          `json:"url,omitempty"`
	InfoHash    string          `json:"infoHash,omitempty"`
	FileIdx     *int            `json:"fileIdx,omitempty"`
	YtID        string          `json:"ytId,omitempty"`
	ExternalURL string          `json:"externalUrl,omitempty"`
	Ether       *StreamEtherExt `json:"ether,omitempty"`
}

// StreamRequest is passed to a Stream route handler.
type StreamRequest struct {
	Type  string            `json:"type"`
	ID    string            `json:"id"`
	Extra map[string]string `json:"extra"`
}

// StreamResponse is returned by a Stream route handler.
type StreamResponse struct {
	Streams []StreamItem `json:"streams"`
}

// OpenStreamRequest is passed to an OpenStream route handler
// (GET /ether/open/{streamId}). For a release answered on a category search,
// File or FileIndex name the file to open (from ReleaseFiles); both empty
// lets the plugin choose.
type OpenStreamRequest struct {
	StreamID  string `json:"streamId"`
	File      string `json:"file,omitempty"`
	FileIndex *int   `json:"fileIndex,omitempty"`
}

// SourceLookup is a typed lookup on a category search, such as
// {Type: "album", Fields: {"artist": "...", "title": "..."}}.
type SourceLookup struct {
	Type   string            `json:"type"`
	Fields map[string]string `json:"fields,omitempty"`
}

// SourceSearchRequest is passed to a SourceSearch route handler
// (GET /ether/search). Categories are the requested ones the plugin
// declared; Lookup is set only for a lookup type the plugin declared.
// Purpose says why Ether searches (playback, detail, warmup, ...) when known.
type SourceSearchRequest struct {
	Query      string        `json:"query"`
	Categories []int         `json:"categories"`
	Lookup     *SourceLookup `json:"lookup,omitempty"`
	Purpose    string        `json:"purpose,omitempty"`
}

// SourceSearchResponse answers a category search in the stream route's
// shape. Return releases as lazy streams (Ether.Lazy with Ether.ID and
// Ether.ReleaseName) that ReleaseFiles lists and OpenStream opens, or as
// torrents (InfoHash) Ether opens through debrid.
type SourceSearchResponse = StreamResponse

// ReleaseFilesRequest is passed to a ReleaseFiles route handler
// (GET /ether/files/{id}) for a lazy release a search answered.
type ReleaseFilesRequest struct {
	ID string `json:"id"`
}

// PluginReleaseFile is one file inside a plugin release.
type PluginReleaseFile struct {
	Path string `json:"path"`
	Size int64  `json:"size,omitempty"`
}

// ReleaseFilesResponse lists a release's files in the order FileIndex counts.
type ReleaseFilesResponse struct {
	Files []PluginReleaseFile `json:"files"`
}

// OpenStreamResponse is returned by an OpenStream route handler.
type OpenStreamResponse struct {
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	ExpiresAt string            `json:"expiresAt,omitempty"`
	File      string            `json:"file,omitempty"`
}

// SubtitleItem is one subtitle track returned by a Subtitles route handler.
type SubtitleItem struct {
	ID              string `json:"id"`
	URL             string `json:"url,omitempty"`
	Name            string `json:"name,omitempty"`
	Lang            string `json:"lang"`
	Label           string `json:"label,omitempty"`
	Format          string `json:"format,omitempty"`
	HearingImpaired bool   `json:"hearingImpaired,omitempty"`
}

// SubtitlesRequest is passed to a Subtitles route handler.
type SubtitlesRequest struct {
	Type      string            `json:"type"`
	ID        string            `json:"id"`
	Extra     map[string]string `json:"extra"`
	PickToken string            `json:"pickToken"`
	VideoHash string            `json:"videoHash,omitempty"`
	VideoSize int64             `json:"videoSize,omitempty"`
	Filename  string            `json:"filename,omitempty"`
}

// SubtitlesResponse is returned by a Subtitles route handler.
type SubtitlesResponse struct {
	Subtitles []SubtitleItem `json:"subtitles"`
}

// SubtitleDownloadRequest identifies a viewer-picked subtitle.
type SubtitleDownloadRequest struct {
	ID     string `json:"id"`
	Lang   string `json:"lang"`
	Format string `json:"format"`
}

// SubtitleDownloadResponse carries subtitle text or base64-encoded bytes.
type SubtitleDownloadResponse struct {
	Format   string `json:"format"`
	Encoding string `json:"encoding"`
	Data     string `json:"data"`
}

// MarkerItem is one intro/credits/recap/preview marker.
type MarkerItem struct {
	Kind  string  `json:"kind"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Label string  `json:"label,omitempty"`
}

// MarkersRequest is passed to a Markers route handler.
type MarkersRequest struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// MarkersResponse is returned by a Markers route handler.
type MarkersResponse struct {
	Markers []MarkerItem `json:"markers"`
}

// ChannelInput is one stream input for a Live TV channel.
type ChannelInput struct {
	URL      string            `json:"url"`
	Name     string            `json:"name,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Priority int               `json:"priority,omitempty"`
}

// ChannelItem is one Live TV channel.
type ChannelItem struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Number  int            `json:"number,omitempty"`
	Group   string         `json:"group,omitempty"`
	Logo    string         `json:"logo,omitempty"`
	GuideID string         `json:"guideId,omitempty"`
	Inputs  []ChannelInput `json:"inputs"`
}

// ChannelsResponse is returned by a Channels route handler.
type ChannelsResponse struct {
	Channels []ChannelItem `json:"channels"`
}

// GuideRequest is passed to a Guide route handler.
type GuideRequest struct {
	ChannelID string `json:"channelId"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
}

// ProgrammeItem is one EPG programme entry.
type ProgrammeItem struct {
	ID          string   `json:"id,omitempty"`
	ChannelID   string   `json:"channelId,omitempty"`
	Title       string   `json:"title"`
	Subtitle    string   `json:"subtitle,omitempty"`
	Description string   `json:"description,omitempty"`
	Start       string   `json:"start"`
	Stop        string   `json:"stop"`
	Icon        string   `json:"icon,omitempty"`
	Categories  []string `json:"categories,omitempty"`
}

// GuideResponse is returned by a Guide route handler.
type GuideResponse struct {
	Programmes []ProgrammeItem `json:"programmes"`
}

// ConfigureResponse is returned by a Configure route handler.
type ConfigureResponse struct {
	Errors map[string]string `json:"errors"`
}

// TestResponse is returned by a Test route handler.
type TestResponse struct {
	OK      bool     `json:"ok"`
	Message string   `json:"message,omitempty"`
	Details []string `json:"details,omitempty"`
}

// HealthResponse is returned by a Health route handler.
type HealthResponse struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// ViewSpecDocument is a declarative ViewSpec v1 page specification.
type ViewSpecDocument struct {
	SpecVersion int              `json:"specVersion"`
	Layout      string           `json:"layout"`
	Route       string           `json:"route,omitempty"`
	Header      []map[string]any `json:"header"`
	Body        []map[string]any `json:"body"`
}

// ViewRequest is passed to a View route handler.
type ViewRequest struct {
	Page  string            `json:"page"`
	Query map[string]string `json:"query"`
}

// ViewResponse is returned by a View route handler.
type ViewResponse struct {
	Spec ViewSpecDocument `json:"spec"`
	Data map[string]any   `json:"data"`
}

// ActionProposal describes a confirmation proposal returned by an Action handler.
type ActionProposal struct {
	Category    string         `json:"category"`
	Description string         `json:"description"`
	Input       map[string]any `json:"input,omitempty"`
}

// ActionRequest is passed to an Action route handler.
type ActionRequest struct {
	Action string         `json:"action"`
	Input  map[string]any `json:"input"`
}

// ActionResponse is returned by an Action route handler.
type ActionResponse struct {
	Toast    string          `json:"toast,omitempty"`
	Refresh  bool            `json:"refresh,omitempty"`
	Open     string          `json:"open,omitempty"`
	Proposal *ActionProposal `json:"proposal,omitempty"`
}

// EventPluginFoldersChanged is delivered to the hosted plugin whose folder grant changed.
const EventPluginFoldersChanged = "plugin.folders.changed"

// FolderRoots is the absolute owner-picked root paths returned by FoldersRoots.
type FolderRoots = []string

// FolderRootsChangedPayload contains the absolute paths the owner grants after the change.
type FolderRootsChangedPayload struct {
	Roots FolderRoots `json:"roots"`
}

// EtherEvent is one event delivered in a batch to POST /ether/event.
type EtherEvent struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	At       string         `json:"at"`
	ViewerID int64          `json:"viewerId,omitempty"`
	Payload  map[string]any `json:"payload,omitempty"`
}

// EventResponse is returned by an Event route handler.
type EventResponse struct {
	Ack []string `json:"ack"`
}

// ToolRequest is passed to a Tool route handler.
type ToolRequest struct {
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// ToolResponse is returned by a Tool route handler.
type ToolResponse map[string]any

// JobRequest is passed to a Job route handler.
type JobRequest struct {
	Name         string          `json:"name"`
	Continuation json.RawMessage `json:"continuation,omitempty"`
}

// JobResponse returns continueInMs, continuation and progress to continue;
// done:true completes, failed:string stops with an error. Ether caps jobs at
// the owner time ceiling (12h by default) and 1000000 calls; store checkpoints in KV for restart recovery.
type JobResponse map[string]any

// NotifyRequest is passed to a Notify route handler.
type NotifyRequest struct {
	Title    string         `json:"title,omitempty"`
	Message  string         `json:"message"`
	Event    string         `json:"event,omitempty"`
	Settings map[string]any `json:"settings,omitempty"`
}

// NotifyResponse is returned by a Notify route handler.
type NotifyResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// FileRequest is passed to a File route handler.
type FileRequest struct {
	Path     string `json:"path"`
	Range    string `json:"range,omitempty"`
	HeadOnly bool   `json:"headOnly,omitempty"`
}

// FileRangeResponse is returned by a File route handler.
type FileRangeResponse struct {
	Status       int               `json:"status,omitempty"`
	ContentType  string            `json:"contentType,omitempty"`
	ContentRange string            `json:"contentRange,omitempty"`
	TotalSize    *int64            `json:"totalSize,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Body         []byte            `json:"-"`
}

// ClientCaps describes playback client capabilities passed to hot-path functions.
type ClientCaps struct {
	MaxHeight int      `json:"maxHeight,omitempty"`
	HDR       []string `json:"hdr,omitempty"`
	Codecs    []string `json:"codecs,omitempty"`
}

// CandidateFacts is the release candidate structure passed to RankKey and CondMatch.
type CandidateFacts struct {
	ReleaseID     string      `json:"releaseId"`
	Name          string      `json:"name"`
	Size          int64       `json:"size,omitempty"`
	Protocol      string      `json:"protocol,omitempty"`
	SourceClass   string      `json:"sourceClass,omitempty"`
	Indexer       string      `json:"indexer,omitempty"`
	Resolution    any         `json:"resolution,omitempty"`
	HDR           []string    `json:"hdr,omitempty"`
	VideoCodec    string      `json:"videoCodec,omitempty"`
	AudioCodecs   []string    `json:"audioCodecs,omitempty"`
	AudioChannels string      `json:"audioChannels,omitempty"`
	Languages     []string    `json:"languages,omitempty"`
	Subtitles     []string    `json:"subtitles,omitempty"`
	ReleaseType   string      `json:"releaseType,omitempty"`
	Group         string      `json:"group,omitempty"`
	Cached        bool        `json:"cached,omitempty"`
	Verified      bool        `json:"verified,omitempty"`
	ClientCaps    *ClientCaps `json:"clientCaps,omitempty"`
}

// RawRequest is the low-level transport-agnostic request passed to Plugin.Dispatch.
type RawRequest struct {
	PluginID string            `json:"pluginId,omitempty"`
	Method   string            `json:"method,omitempty"`
	Path     string            `json:"path"`
	Query    map[string]string `json:"query,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Body     json.RawMessage   `json:"body,omitempty"`
	Upload   *FrontendUpload   `json:"upload,omitempty"`
	Locale   string            `json:"locale,omitempty"`
}

// FrontendBlob selects host-served bytes for a frontend adapter response.
// Set LibraryKey for blob:<sha256> refs and optionally select an archive
// member with MemberIndex. Leave both unset for durable:<sha256> refs.
type FrontendBlob struct {
	LibraryKey  string `json:"libraryKey,omitempty"`
	Ref         string `json:"ref"`
	MemberIndex *int   `json:"memberIndex,omitempty"`
}

// RawResponse is the low-level transport-agnostic response returned by Plugin.Dispatch.
type RawResponse struct {
	Blob       *FrontendBlob     `json:"blob,omitempty"`
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       json.RawMessage   `json:"body,omitempty"`
	BinaryBody []byte            `json:"-"`
}

// RouteError represents a structured HTTP error returned by a plugin handler.
type RouteError struct {
	Status  int
	Code    string
	Message string
}

func (e *RouteError) Error() string {
	return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
}

// NewRouteError constructs a structured RouteError.
func NewRouteError(status int, code, message string) *RouteError {
	return &RouteError{Status: status, Code: code, Message: message}
}
