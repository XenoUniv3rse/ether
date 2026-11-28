// Package plugins is Ether's plugin host core (SPEC §5.1, §5.4, §5.5, §5.7,
// §5.13, WS7): manifest parsing and validation, install and lifecycle,
// permissions and grants, settings and sealed secrets, KV, health, the pull
// scheduler, the event outbox and log, the journal (shadow and undo), update
// and rollback, removal, cluster placement, and the owner API.
//
// Runtimes are separate workstreams and plug in through Runtime and
// RuntimeFactory: WS8a registers the hosted (wazero) runtime, WS8b the
// remote (URL and tunnel) runtimes. Capability adapters (WS9) call plugins
// through Manager.Call and read state through Manager's accessors; they
// never touch the tables directly. The conformance kit (WS11) reuses
// ParseManifest, Manifest.Validate and LintCopy.
package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
	"github.com/braedonsaunders/ether/internal/stremio"
)

// Protocol is the Ether Plugin Protocol major this host speaks (EPP/1).
const Protocol = 1

// Kind decides where the owner meets a plugin (SPEC §5.1.1).
type Kind string

const (
	KindSource              Kind = "source"
	KindIntegration         Kind = "integration"
	KindNotificationChannel Kind = "notification_channel"
	KindLiveTVSource        Kind = "livetv_source"
	KindExtension           Kind = "extension"
	// KindContent plugins declare media kinds; the owner meets each one
	// as a Library (spec §2.1).
	KindContent Kind = "content"
)

// Capability is one thing a plugin can do (SPEC §5.1.3).
type Capability string

const (
	CapStreamSource    Capability = "stream_source"
	CapDebridProvider  Capability = "debrid_provider"
	CapListSource      Capability = "list_source"
	CapMetadata        Capability = "metadata"
	CapPluginDirectory Capability = "plugin_directory"
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
	CapOnAirContent    Capability = "onair_content"
	CapLiveTVPack      Capability = "livetv_pack"
	CapPosterBadges    Capability = "poster_badges"
	// Content-kind capabilities (spec §2.1). Catalog provider, kind
	// resolver and matcher need kind content; a frontend adapter may
	// also be an extension over the built-in libraries.
	CapCatalogProvider Capability = "catalog_provider"
	CapKindResolver    Capability = "kind_resolver"
	CapFrontendAdapter Capability = "frontend_adapter"
	CapMatcher         Capability = "matcher"
	CapLiveEvents      Capability = "live_events"
	// CapScreens lets a plugin contribute screens and surfaces that Vela
	// draws from documents in the package (docs/plugin-screens.md).
	CapScreens Capability = "screens"
)

// Reserved capabilities are known names a manifest may not claim in v1.
var ReservedCapabilities = map[Capability]bool{
	"artwork": true, "ratings": true,
	"stats_export": true, "sign_in": true,
}

// Manifest is a plugin's manifest.json: the Stremio manifest fields, typed
// by the one Stremio model (internal/stremio), plus Ether's fields under
// Ether (SPEC §5.1.1). A manifest with a nil Ether is a plain Stremio
// add-on. Every Stremio rule (resources, ids, catalogs, extras) is the
// library's, reached through Stremio().
type Manifest struct {
	ID            string                `json:"id"`
	Version       string                `json:"version"`
	Name          string                `json:"name"`
	Description   string                `json:"description,omitempty"`
	Logo          string                `json:"logo,omitempty"`
	Background    string                `json:"background,omitempty"`
	Types         []string              `json:"types,omitempty"`
	AddonCatalogs []StremioCat          `json:"addonCatalogs,omitempty"`
	Catalogs      []StremioCat          `json:"catalogs,omitempty"`
	Resources     stremio.Resources     `json:"resources,omitempty"`
	IDPrefixes    []string              `json:"idPrefixes,omitempty"`
	BehaviorHints stremio.BehaviorHints `json:"behaviorHints,omitzero"`
	Ether         *EtherBlock           `json:"ether,omitempty"`
	// rolesOff are capabilities the owner switched off on the installed
	// record (Record.RolesOff); EffectiveEther leaves them out.
	rolesOff []Capability
}

// Stremio is the manifest's Stremio part.
func (m Manifest) Stremio() stremio.Manifest {
	return stremio.Manifest{
		ID: m.ID, Version: m.Version, Name: m.Name, Description: m.Description, Logo: m.Logo,
		Background: m.Background, Types: m.Types, IDPrefixes: m.IDPrefixes, Resources: m.Resources,
		Catalogs: m.Catalogs, AddonCatalogs: m.AddonCatalogs, BehaviorHints: m.BehaviorHints,
	}
}

// HasResource reports whether the manifest declares the named resource.
func (m Manifest) HasResource(name string) bool { return m.Stremio().HasResource(name) }

// Serves reports whether the named resource answers content type typ.
func (m Manifest) Serves(name, typ string) bool { return m.Stremio().Serves(name, typ) }

// CatalogsOfType returns the catalogs of one content type.
func (m Manifest) CatalogsOfType(typ string) []StremioCat { return m.Stremio().CatalogsOfType(typ) }

// StremioCat is one Stremio catalog declaration.
type StremioCat = stremio.Catalog

// StremioExtra is one catalog extra declaration.
type StremioExtra = stremio.Extra

// EtherBlock is the `ether` object of a manifest.
type EtherBlock struct {
	Protocol int `json:"protocol"`
	// Stremio marks a native plugin that answers Stremio add-on routes,
	// such as an add-on opened and edited in Studio: Ether talks to it
	// exactly as to the add-on it came from.
	Stremio      bool         `json:"stremio,omitempty"`
	Kind         Kind         `json:"kind"`
	Capabilities []Capability `json:"capabilities,omitempty"`
	// Summary is plain text or a "$t:key" reference into Strings.
	Summary string      `json:"summary,omitempty"`
	Author  Author      `json:"author,omitempty"`
	License string      `json:"license,omitempty"`
	Repo    string      `json:"repo,omitempty"`
	Icon    string      `json:"icon,omitempty"`
	Runtime RuntimeDecl `json:"runtime"`
	// Sidecar tells the console how to run a remote plugin's program: a
	// Docker image, a download+command pair, or neither (docs only). Only
	// meaningful when Runtime.Kind is "remote".
	Sidecar *SidecarDecl `json:"sidecar,omitempty"`
	// Cluster is "leader" (default) or "every_node".
	Cluster     string      `json:"cluster,omitempty"`
	Health      HealthDecl  `json:"health,omitempty"`
	Permissions Permissions `json:"permissions,omitempty"`
	// Settings is a settings schema (SPEC §5.5), {} or {"$ref":"file"}.
	Settings json.RawMessage `json:"settings,omitempty"`
	// Strings maps a locale to a file path (string) or an inline
	// key-to-text object. "en" is required when any $t: key is used.
	Strings     map[string]json.RawMessage `json:"strings,omitempty"`
	Schedule    []ScheduleEntry            `json:"schedule,omitempty"`
	Contributes Contributes                `json:"contributes,omitempty"`
}

// Author names a plugin's author.
type Author struct {
	Name string `json:"name,omitempty"`
	URL  string `json:"url,omitempty"`
}

// RuntimeKind is the runtime that serves the plugin. A plain Stremio
// add-on (no ether block) is always reached over HTTP: "remote".
func (m Manifest) RuntimeKind() string {
	if m.Ether == nil || m.Ether.Runtime.Kind == "" {
		return "remote"
	}
	return m.Ether.Runtime.Kind
}

// RuntimeDecl declares how a plugin runs: hosted (Entry JS or Module WASM)
// or remote (the manifest URL or a tunnel is the endpoint).
type RuntimeDecl struct {
	Kind   string         `json:"kind"` // "hosted" or "remote"
	Entry  string         `json:"entry,omitempty"`
	Module string         `json:"module,omitempty"`
	Limits *RuntimeLimits `json:"limits,omitempty"`
}

// DefaultMaxPluginItems is the default per-plugin stored item ceiling across
// its content libraries; owners can raise or lower it globally or per plugin
// without an upper bound.
const DefaultMaxPluginItems = 2_000_000

// RuntimeLimits requests per-call hosted resources and data/catalogue ceilings.
// Ether grants defaults for omitted fields and clamps requests to
// owner-controlled ceilings.
type RuntimeLimits struct {
	FetchBytes           int64  `json:"fetchBytes,omitempty"`
	ArchiveMemberBytes   int64  `json:"archiveMemberBytes,omitempty"`
	ArchiveExpandedBytes int64  `json:"archiveExpandedBytes,omitempty"`
	ArchiveMembers       int    `json:"archiveMembers,omitempty"`
	MaxItems             int    `json:"maxItems,omitempty"`    // default 2,000,000 stored items across the plugin's libraries; owner-controlled
	MemoryBytes          uint64 `json:"memoryBytes,omitempty"` // default 32 MiB, ceiling owner-controlled, up to 4 GiB
	// ReadBytes is the granted per-call byte ceiling for cache.read,
	// archive.open and archive.read, derived from MemoryBytes:
	// clamp(memoryBytes/4, 8 MiB, 512 MiB).
	ReadBytes int64 `json:"readBytes,omitempty"`
	// Deadline and UnitBudget are the granted ceilings in manifests/detail
	// and the active route's effective call limits when read via runtime.limits.
	Deadline   string `json:"deadline,omitempty"`   // Go duration; default 12h ceiling, clamped per route on active calls
	UnitBudget int64  `json:"unitBudget,omitempty"` // default effectively unlimited ceiling; viewer routes clamp to 10,000 units/s of Deadline
	// PoolSize is the hosted WASM instance pool size (default 2; default owner
	// ceiling 4, raisable up to 16). With poolSize >= 3 at least one instance is
	// reserved exclusively for playback resolves and background jobs hold at
	// most poolSize-2; with poolSize == 2 background jobs and frontend adapters
	// share at most one instance so resolves and health checks keep one free.
	PoolSize int `json:"poolSize,omitempty"`
}

// SidecarDecl says how a remote plugin's program comes to be: a Docker image,
// a download with a start command, or documentation only. Every field is
// optional; an empty block is valid but the console shows only the pairing
// code.
type SidecarDecl struct {
	// Docker is an image reference (registry/name:tag or @sha256).
	Docker string `json:"docker,omitempty"`
	// Download is an https URL to the program.
	Download string `json:"download,omitempty"`
	// Command starts the program after download (single line, <= 200 chars).
	Command string `json:"command,omitempty"`
	// Docs is an https URL with setup instructions.
	Docs string `json:"docs,omitempty"`
}

// HealthDecl declares the health-check interval (minimum 15s).
type HealthDecl struct {
	Interval string `json:"interval,omitempty"`
}

// ScheduleEntry is one pull job Ether runs (minimum interval 15m).
type ScheduleEntry struct {
	Every string `json:"every"`
	Call  string `json:"call"`
}

// Permissions is what a plugin asks for; anything not declared is denied
// (SPEC §5.4). Empty means none.
type Permissions struct {
	Network     []string    `json:"network,omitempty" yaml:"network,omitempty"`
	Catalog     string      `json:"catalog,omitempty" yaml:"catalog,omitempty"` // ""|none|read
	Viewers     ViewerPerms `json:"viewers,omitempty" yaml:"viewers,omitempty"`
	Collections string      `json:"collections,omitempty" yaml:"collections,omitempty"` // ""|none|write
	Folders     string      `json:"folders,omitempty" yaml:"folders,omitempty"`         // ""|none|read
	// Sources lets a kind resolver ask Ether to find bytes through the
	// owner's indexers and debrid accounts (spec §4.5).
	Sources string `json:"sources,omitempty" yaml:"sources,omitempty"` // ""|none|search|acquire
	// AI lets a plugin send bounded completion requests to the owner's
	// configured language model (actions/jobs/sync only, never viewer paths).
	AI string `json:"ai,omitempty" yaml:"ai,omitempty"` // ""|none|complete|complete+search
	// Storage lets a plugin retain durable blobs within its per-plugin quota.
	Storage StoragePermission `json:"storage,omitempty" yaml:"storage,omitempty"` // ""|none|durable
	// Media lets a plugin probe container metadata via media.probe from jobs,
	// actions, and background sync (never viewer or client request paths).
	Media   string   `json:"media,omitempty" yaml:"media,omitempty"` // ""|none|probe
	Secrets []string `json:"secrets,omitempty" yaml:"secrets,omitempty"`
}

// ViewerPerms holds the three viewer permission levels.
type ViewerPerms struct {
	Identity        string `json:"identity,omitempty" yaml:"identity,omitempty"`                   // none|granted_viewers|all
	WatchHistory    string `json:"watch_history,omitempty" yaml:"watch_history,omitempty"`         // none|granted_viewers|all
	WriteWatchState string `json:"write_watch_state,omitempty" yaml:"write_watch_state,omitempty"` // none|granted_viewers
}

// Contributes carries per-capability metadata and UI contributions.
type PackStyle struct {
	ID   string `json:"id"`
	File string `json:"file"`
}

type Contributes struct {
	Looks          []PackStyle          `json:"looks,omitempty"`
	OnAirFeeds     []OnAirFeed          `json:"onairFeeds,omitempty"`
	OnAirSegments  []OnAirSegment       `json:"onairSegments,omitempty"`
	Directories    []DirectoryMeta      `json:"directories,omitempty"`
	Metadata       *MetadataMeta        `json:"metadata,omitempty"`
	Source         *SourceMeta          `json:"source,omitempty"`
	Channels       *ChannelsMeta        `json:"channels,omitempty"`
	Subtitles      *SubtitlesMeta       `json:"subtitles,omitempty"`
	Markers        *MarkersMeta         `json:"markers,omitempty"`
	Notification   *NotificationMeta    `json:"notification,omitempty"`
	Events         *EventsMeta          `json:"events,omitempty"`
	FlowActions    []FlowActionMeta     `json:"flowActions,omitempty"`
	Integration    *IntegrationMeta     `json:"integration,omitempty"`
	ListSources    []ListSourceMeta     `json:"listSources,omitempty"`
	RankingSteps   []RankingStepMeta    `json:"rankingSteps,omitempty"`
	RuleConditions []RuleConditionMeta  `json:"ruleConditions,omitempty"`
	PosterBadges   []PosterBadgeSetMeta `json:"posterBadges,omitempty"`
	Tools          []ToolMeta           `json:"tools,omitempty"`
	Pages          []PageMeta           `json:"pages,omitempty"`
	Slots          []SlotMeta           `json:"slots,omitempty"`
	Commands       []CommandMeta        `json:"commands,omitempty"`
	// MediaKinds declares the content kinds this plugin serves; each entry
	// that declares a library block becomes its own library (spec §2.1).
	MediaKinds []MediaKindMeta `json:"mediaKinds,omitempty"`
	// FrontendAdapters declares protocol surfaces this plugin serves
	// through Ether's generic adapter frontend (spec §6.5).
	FrontendAdapters []FrontendAdapterMeta `json:"frontendAdapters,omitempty"`
	// LiveFeeds declares live event feeds this plugin serves under
	// /ether/v1/live/{feed}/* when it declares the live_events capability.
	LiveFeeds []LiveFeedMeta `json:"liveFeeds,omitempty"`
	// Setup declares guided setup steps for any plugin kind (§8).
	Setup *SetupMeta `json:"setup,omitempty"`
	// Screens declares generic top-level screens rendered natively by frontends
	// such as Vela (spec A3).
	Screens []ScreenMeta `json:"screens,omitempty"`
	// Surfaces are documents drawn in host slots: player overlays, toasts,
	// Home rows and title panels (docs/plugin-screens.md, Surfaces).
	Surfaces []SurfaceMeta `json:"surfaces,omitempty"`
}

// MinLiveFeedIntervalSec is the lowest polling interval (in seconds) a plugin
// live feed may declare.
const MinLiveFeedIntervalSec = 10

// LiveFeedMeta declares one live events feed contributed by a plugin.
type LiveFeedMeta struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	MinIntervalSec int      `json:"minIntervalSec"`
	EntityKinds    []string `json:"entityKinds,omitempty"`
}

// LiveFeedByID returns the declared live feed with this id, if any.
func (m Manifest) LiveFeedByID(id string) (LiveFeedMeta, bool) {
	if e := m.EffectiveEther(); e != nil {
		for _, lf := range e.Contributes.LiveFeeds {
			if lf.ID == id {
				return lf, true
			}
		}
	}
	return LiveFeedMeta{}, false
}

// PosterBadgeSetMeta declares one poster overlay badge set contributed by a plugin (#594).
type PosterBadgeSetMeta struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// SourceMeta is mandatory for stream_source.
type SourceMeta struct {
	Class     string          `json:"class"` // debrid|usenet|indexer|direct
	Abilities []string        `json:"abilities,omitempty"`
	Limits    json.RawMessage `json:"limits,omitempty"`
	// Search opts a stream_source plugin in to category searches (#792).
	// Without it the plugin is asked by title id only and category
	// searches refuse it.
	Search *SourceSearchMeta `json:"search,omitempty"`
}

// SourceSearchMeta declares the category searches a stream_source plugin
// answers on GET /ether/search: free text plus a Newznab category, and
// optionally typed lookups. Ether routes a category search to the plugin
// only when one of the requested categories is declared; a declared top
// category (3000) covers its subcategories (3010, 3040).
type SourceSearchMeta struct {
	Categories []int    `json:"categories"`
	Lookups    []string `json:"lookups,omitempty"`
}

// MaxSourceSearchCategories and MaxSourceSearchLookups bound a declaration.
const (
	MaxSourceSearchCategories = 16
	MaxSourceSearchLookups    = 8
)

// AnswersCategories reports whether the declaration covers any of cats.
func (s *SourceSearchMeta) AnswersCategories(cats []int) bool {
	return len(s.MatchCategories(cats)) > 0
}

// MatchCategories returns the requested categories the declaration covers,
// in request order.
func (s *SourceSearchMeta) MatchCategories(cats []int) []int {
	if s == nil {
		return nil
	}
	var out []int
	for _, c := range cats {
		for _, d := range s.Categories {
			if c == d || (d%1000 == 0 && c/1000 == d/1000) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// AnswersLookup reports whether the declaration lists the lookup type.
func (s *SourceSearchMeta) AnswersLookup(typ string) bool {
	if s == nil || typ == "" {
		return false
	}
	for _, l := range s.Lookups {
		if l == typ {
			return true
		}
	}
	return false
}

// SourceSearch returns the plugin's category-search declaration, or nil.
func (m Manifest) SourceSearch() *SourceSearchMeta {
	if m.Ether == nil || m.Ether.Stremio || !m.HasCapability(CapStreamSource) || m.Ether.Contributes.Source == nil {
		return nil
	}
	return m.Ether.Contributes.Source.Search
}

// ChannelsMeta is mandatory for livetv_channels.
type ChannelsMeta struct {
	Refresh string `json:"refresh,omitempty"`
	Test    bool   `json:"test"`
}

// SubtitlesMeta is mandatory for subtitles. Languages is "any" or a list.
type SubtitlesMeta struct {
	Languages json.RawMessage `json:"languages,omitempty"`
	Formats   []string        `json:"formats,omitempty"`
}

// MarkersMeta is mandatory for markers.
type MarkersMeta struct {
	Kinds []string `json:"kinds"` // intro|credits|recap|preview
}

// NotificationMeta is mandatory for notify.
type NotificationMeta struct {
	Fields    json.RawMessage `json:"fields,omitempty"`
	SendTest  bool            `json:"sendTest"`
	Formats   []string        `json:"formats,omitempty"`
	MaxLength int             `json:"maxLength,omitempty"`
}

// EventsMeta is mandatory for events. Subscribe lists built-in or plugin event
// names delivered to POST /ether/event; Publish declares custom event schemas
// the plugin may emit onto Ether's event bus as "plugin:<pluginId>/<key>".
type EventsMeta struct {
	Subscribe []string             `json:"subscribe,omitempty"`
	Publish   []PublishedEventMeta `json:"publish,omitempty"`
}

// EventEnumOption describes one allowed option for an enum event field or flow
// action parameter.
type EventEnumOption struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
}

// EventFieldMeta describes one typed field in a published plugin event payload
// or a flow action output object.
type EventFieldMeta struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Label       string            `json:"label"`
	Description string            `json:"description,omitempty"`
	Unit        string            `json:"unit,omitempty"`
	Enum        []EventEnumOption `json:"enum,omitempty"`
	PII         bool              `json:"pii,omitempty"`
	Secret      bool              `json:"secret,omitempty"`
	Filterable  bool              `json:"filterable,omitempty"`
	Example     any               `json:"example,omitempty"`
}

// PublishedEventMeta declares one custom event type a plugin emits through
// Host.PublishEvent ("plugin:<pluginId>/<key>").
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
	Type        string            `json:"type"`
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

// FlowActionMeta declares one Flow action contributed by a plugin
// ("plugin:<pluginId>/<key>"). Ether invokes Route with HTTP POST and an
// X-Ether-Idempotency-Key header.
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

// IntegrationMeta is mandatory for the integration kind.
type IntegrationMeta struct {
	Category string   `json:"category"`
	Logo     string   `json:"logo,omitempty"`
	Adds     []string `json:"adds"`    // 1..4 plain lines or $t: keys
	Connect  string   `json:"connect"` // fields|device_code|none
	Test     bool     `json:"test,omitempty"`
}

// ListSourceMeta is one contributed list source.
type ListSourceMeta struct {
	Key     string          `json:"key"`
	Label   string          `json:"label"`
	Hint    string          `json:"hint,omitempty"`
	Catalog string          `json:"catalog"`
	Params  json.RawMessage `json:"params,omitempty"`
	// MaxItems is how many titles one sync of the list reads (≤ 2000;
	// DefaultListItems when omitted). The install review shows it and the
	// owner can lower or raise it in the plugin's limits.
	MaxItems int    `json:"maxItems,omitempty"`
	Refresh  string `json:"refresh,omitempty"` // ≥ 1h
}

// RankingStepMeta is one contributed ranking step (hosted only).
type RankingStepMeta struct {
	Key    string          `json:"key"`
	Label  string          `json:"label"`
	Hint   string          `json:"hint,omitempty"`
	Editor json.RawMessage `json:"editor,omitempty"`
	Budget json.RawMessage `json:"budget,omitempty"`
}

// RuleConditionMeta is one contributed rule condition (hosted only).
type RuleConditionMeta struct {
	Key       string          `json:"key"`
	Label     string          `json:"label"`
	Operators []string        `json:"operators,omitempty"`
	Editor    json.RawMessage `json:"editor,omitempty"`
}

// ToolMeta is one contributed agent tool.
type ToolMeta struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	ReadOnly    bool            `json:"readOnly,omitempty"`
	Destructive bool            `json:"destructive,omitempty"`
	OpenWorld   bool            `json:"openWorld,omitempty"`
}

// PageMeta is one contributed console page (ViewSpec, SPEC §5.6).
type PageMeta struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Route string `json:"route"` // plugin route returning {spec,data}
	Group string `json:"group,omitempty"`
	Icon  string `json:"icon,omitempty"`
}

// SlotMeta is one contributed console slot.
type SlotMeta struct {
	Slot   string `json:"slot"` // title.panel|viewer.tab|dashboard.panel|actionMenu.*
	Key    string `json:"key"`
	Label  string `json:"label"`
	Route  string `json:"route,omitempty"`
	Action string `json:"action,omitempty"`
}

// CommandMeta is one contributed ⌘K command.
type CommandMeta struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Action string `json:"action"` // plugin action name
}

// MediaKindMeta declares one content kind (spec §2.1).
type MediaKindMeta struct {
	Key     string             `json:"key"` // ^[a-z][a-z0-9-]{0,31}$, unique in the plugin
	Label   string             `json:"label"`
	Media   string             `json:"media,omitempty"` // video|music|audiobook|book|podcast|photo
	Library KindLibraryDecl    `json:"library,omitzero"`
	Levels  []KindLevel        `json:"levels"`           // 1..5, first is the root
	Fields  []KindField        `json:"fields,omitempty"` //
	Art     []KindArt          `json:"art,omitempty"`    // roles beyond poster/backdrop/thumb/logo
	Present core.Presentations `json:"present,omitzero"`
	Browse  KindBrowseDecl     `json:"browse"`
	Sync    KindSyncDecl       `json:"sync"`
	Cache   KindCacheDecl      `json:"cache,omitempty"`
	Match   KindMatchDecl      `json:"match,omitempty"`
	// Related: the plugin serves POST /ether/v1/kinds/{kind}/related for its
	// items ({item, purpose, limit} -> {label?, items: KindItem[]}): "similar"
	// for a detail row (an artist's "Fans also like"), "continuation" for what
	// plays after a queue ends (radio). Items are stored like live search
	// results. Asked when a viewer opens the item or nears a queue's end.
	Related *KindRelatedDecl `json:"related,omitempty"`
	// Import: the plugin turns playlist links into track lists (POST
	// /ether/v1/kinds/{kind}/import, for the sites named in links) and finds
	// its own items for described tracks (POST .../identify), so viewers can
	// import playlists from a link or an M3U/CSV file.
	Import *KindImportDecl `json:"import,omitempty"`
	// Lyrics: the plugin serves POST /ether/v1/kinds/{kind}/lyrics for its
	// playable items ({"item": KindItem} → syncedLyrics (LRC), plainLyrics,
	// instrumental, source; 404 when it has none). Ether asks once per item
	// when a viewer opens the lyrics, never ahead of use.
	Lyrics bool `json:"lyrics,omitempty"`
}

// MaxKindLibraryTabs is the maximum number of declared sub-tabs per kind library.
const MaxKindLibraryTabs = 50

// KindLibraryDecl is the optional library created for a kind on install.
type KindLibraryDecl struct {
	Name        string             `json:"name"`                  // default name; the owner may rename it after
	Icon        string             `json:"icon,omitempty"`        // package-relative SVG or PNG shown in Integrations and Libraries
	Card        string             `json:"card,omitempty"`        // poster (default)|landscape|square
	DateDisplay string             `json:"dateDisplay,omitempty"` // year (default)|date
	Experience  string             `json:"experience,omitempty"`  // watch|listen|read|gallery|live or another id; defaults by media class
	Tabs        []KindLibraryTab   `json:"tabs,omitempty"`        // up to 50; defaults to Recommended + Library
	Shelves     []KindLibraryShelf `json:"shelves,omitempty"`     // up to 40; default Recommended shelves
}

// KindLibraryTab declares one sub-tab shown inside a plugin library.
type KindLibraryTab struct {
	Key               string             `json:"key"`
	Label             string             `json:"label"`
	Kind              string             `json:"kind"`                        // recommended|library|collections|people|view|live
	Level             string             `json:"level,omitempty"`             // optional declared hierarchy level (library, view, collections or people)
	Facet             string             `json:"facet,omitempty"`             // declared browse.facets entry (collections or people)
	Sort              string             `json:"sort,omitempty"`              // one of browse.sorts or lastViewed (facet collections or view)
	Direction         string             `json:"direction,omitempty"`         // asc|desc (facet collections or view)
	Order             string             `json:"order,omitempty"`             // count|name (facet collections or people)
	Card              string             `json:"card,omitempty"`              // landscape|poster|square (live or recommended tabs; defaults to landscape on live)
	Layout            string             `json:"layout,omitempty"`            // grid (default)|list|timeline (view tabs only)
	Search            *bool              `json:"search,omitempty"`            // collections, people, view or live (default true)
	SearchPlaceholder string             `json:"searchPlaceholder,omitempty"` // localizable placeholder, <= 60 chars
	Filters           *core.TabFilter    `json:"filters,omitempty"`           // saved filter rules (view tabs only)
	Shelves           []KindLibraryShelf `json:"shelves,omitempty"`           // recommended tabs only; overrides library.shelves for that tab
}

// EffectiveSearch reports whether tab search is enabled. Search defaults to
// true on collections, people, view and live tabs and is never enabled on
// other manifest tab kinds.
func (t KindLibraryTab) EffectiveSearch() bool {
	if t.Kind != "collections" && t.Kind != "people" && t.Kind != "view" && t.Kind != "live" {
		return false
	}
	if t.Search != nil {
		return *t.Search
	}
	return true
}

// EffectiveLayout returns the tab's layout ("grid" by default for view tabs,
// empty for other tab kinds).
func (t KindLibraryTab) EffectiveLayout() string {
	if t.Kind != "view" {
		return ""
	}
	switch t.Layout {
	case core.TabLayoutGrid, core.TabLayoutList, core.TabLayoutTimeline:
		return t.Layout
	default:
		return core.TabLayoutGrid
	}
}

// EffectiveCard returns the tab's card shape ("landscape" by default for live
// tabs, explicit card when set on recommended tabs, empty otherwise).
func (t KindLibraryTab) EffectiveCard() string {
	switch t.Kind {
	case "live":
		switch t.Card {
		case "poster", "landscape", "square":
			return t.Card
		default:
			return "landscape"
		}
	case "recommended":
		switch t.Card {
		case "poster", "landscape", "square":
			return t.Card
		}
	}
	return ""
}

// IsZero reports whether every field of the library declaration is empty.
func (d KindLibraryDecl) IsZero() bool {
	return d.Name == "" && d.Icon == "" && d.Card == "" && d.DateDisplay == "" && d.Experience == "" && len(d.Tabs) == 0 && len(d.Shelves) == 0
}

// EffectiveCard returns the library's card aspect ("poster" by default).
func (d KindLibraryDecl) EffectiveCard() string {
	switch d.Card {
	case "landscape", "square":
		return d.Card
	default:
		return "poster"
	}
}

// EffectiveDateDisplay returns how dates are shown on cards and detail headers
// ("year" by default, or "date" for full YYYY-MM-DD display).
func (d KindLibraryDecl) EffectiveDateDisplay() string {
	if d.DateDisplay == "date" {
		return "date"
	}
	return "year"
}

// KindLiveRequest is the request body sent to POST /ether/v1/kinds/{kind}/live.
type KindLiveRequest struct {
	Tab    string `json:"tab,omitempty"`
	Q      string `json:"q,omitempty"`
	Filter string `json:"filter,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// KindLiveItem is one currently live stream returned by a plugin live tab.
type KindLiveItem struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Subtitle string   `json:"subtitle,omitempty"`
	Badge    string   `json:"badge,omitempty"`
	Viewers  *int     `json:"viewers,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Thumb    string   `json:"thumb,omitempty"`
	Live     *bool    `json:"live,omitempty"`
}

// IsLive reports whether the item is live (defaults to true when omitted).
func (i KindLiveItem) IsLive() bool {
	return i.Live == nil || *i.Live
}

// KindLiveFilter is one optional filter chip returned alongside live items.
type KindLiveFilter struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// KindLivePage is the response returned by POST /ether/v1/kinds/{kind}/live.
type KindLivePage struct {
	Items      []KindLiveItem   `json:"items"`
	Filters    []KindLiveFilter `json:"filters,omitempty"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

// KindLiveResolveRequest is the request body sent to POST /ether/v1/kinds/{kind}/live/resolve.
type KindLiveResolveRequest struct {
	ID string `json:"id"`
}

// KindLiveResolveResponse is the direct playable stream returned by
// POST /ether/v1/kinds/{kind}/live/resolve.
type KindLiveResolveResponse struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	MIME    string            `json:"mime,omitempty"`
	Title   string            `json:"title,omitempty"`
}

// EffectiveTabs returns the declared tabs, or the default Recommended and Library tabs.
func (d KindLibraryDecl) EffectiveTabs() []KindLibraryTab {
	if len(d.Tabs) > 0 {
		out := make([]KindLibraryTab, len(d.Tabs))
		copy(out, d.Tabs)
		return out
	}
	return []KindLibraryTab{
		{Key: "recommended", Label: "Recommendations", Kind: "recommended"},
		{Key: "library", Label: "Library", Kind: "library"},
	}
}

// MaxKindLibraryShelves is the maximum number of declared Recommended shelves per kind library.
const MaxKindLibraryShelves = 40

// KindLibraryShelfFilters configures optional filtering for a declared
// Recommended shelf. In JSON it accepts four equivalent forms:
//
//  1. A legacy facet map: {"topic":["science","nature"]}
//  2. A TabFilter object: {"match":"all","rules":[...],"exclude":[...]}
//  3. A single rule object: {"field":"viewer","value":{"history":true}}
//  4. An array of rules: [{"field":"viewer","value":{"history":true}}]
//
// Omitted "op" on a "viewer" rule defaults to "is".
type KindLibraryShelfFilters struct {
	Facets map[string][]string
	Tab    *core.TabFilter
}

// IsEmpty reports whether neither facet filters nor rule filters are set.
func (f *KindLibraryShelfFilters) IsEmpty() bool {
	if f == nil {
		return true
	}
	return len(f.Facets) == 0 && f.Tab.IsEmpty()
}

// Clone returns a deep copy of f, or nil when f is nil.
func (f *KindLibraryShelfFilters) Clone() *KindLibraryShelfFilters {
	if f == nil {
		return nil
	}
	out := &KindLibraryShelfFilters{Tab: f.Tab.Clone()}
	if len(f.Facets) > 0 {
		out.Facets = make(map[string][]string, len(f.Facets))
		for k, vals := range f.Facets {
			out.Facets[k] = append([]string(nil), vals...)
		}
	}
	return out
}

// MarshalJSON encodes f in its canonical form (TabFilter when rule-based,
// otherwise the facet map).
func (f KindLibraryShelfFilters) MarshalJSON() ([]byte, error) {
	if f.Tab != nil {
		return json.Marshal(f.Tab)
	}
	if len(f.Facets) > 0 {
		return json.Marshal(f.Facets)
	}
	return []byte("{}"), nil
}

// UnmarshalJSON decodes a facet map, TabFilter, single rule, or rule array.
func (f *KindLibraryShelfFilters) UnmarshalJSON(data []byte) error {
	f.Facets = nil
	f.Tab = nil
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '[' {
		var rules []core.CollectionRule
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rules); err != nil {
			return err
		}
		normalizeShelfRuleOps(rules)
		f.Tab = &core.TabFilter{Match: "all", Rules: rules}
		return nil
	}
	if trimmed[0] != '{' {
		return fmt.Errorf("shelf filters must be a JSON object or array")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	if isSingleRuleObject(raw) {
		var rule core.CollectionRule
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rule); err != nil {
			return err
		}
		rules := []core.CollectionRule{rule}
		normalizeShelfRuleOps(rules)
		f.Tab = &core.TabFilter{Match: "all", Rules: rules}
		return nil
	}
	if isTabFilterObject(raw) {
		var tf core.TabFilter
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&tf); err != nil {
			return err
		}
		normalizeShelfTabFilter(&tf)
		f.Tab = &tf
		return nil
	}
	var facets map[string][]string
	if err := json.Unmarshal(trimmed, &facets); err != nil {
		return err
	}
	f.Facets = facets
	return nil
}

func isSingleRuleObject(raw map[string]json.RawMessage) bool {
	v, ok := raw["field"]
	if !ok {
		return false
	}
	trimmed := bytes.TrimSpace(v)
	return len(trimmed) > 0 && trimmed[0] == '"'
}

func isTabFilterObject(raw map[string]json.RawMessage) bool {
	for _, key := range []string{"match", "visibilityDefault"} {
		if v, ok := raw[key]; ok {
			t := bytes.TrimSpace(v)
			if len(t) > 0 && t[0] == '"' {
				return true
			}
		}
	}
	for _, key := range []string{"rules", "exclude", "groups"} {
		if v, ok := raw[key]; ok {
			t := bytes.TrimSpace(v)
			if len(t) == 0 || t[0] != '[' {
				continue
			}
			// Check whether the array holds objects (rules/groups) rather than strings (a facet literally named "rules").
			rest := bytes.TrimSpace(t[1:])
			if len(rest) > 0 && (rest[0] == '{' || rest[0] == ']') {
				return true
			}
		}
	}
	return false
}

func normalizeShelfTabFilter(tf *core.TabFilter) {
	if tf == nil {
		return
	}
	normalizeShelfRuleOps(tf.Rules)
	normalizeShelfRuleOps(tf.Exclude)
	for i := range tf.Groups {
		normalizeShelfRuleOps(tf.Groups[i].Rules)
	}
}

func normalizeShelfRuleOps(rules []core.CollectionRule) {
	for i := range rules {
		if rules[i].Field == "viewer" && rules[i].Op == "" {
			rules[i].Op = "is"
		}
	}
}

// KindLibraryShelf declares one default Recommended shelf for a plugin library.
// Each shelf gets the stable row ID "plugin:<viewID>:shelf:<key>" so the
// library's Recommended row editor can reorder, hide, pin, or cap it.
type KindLibraryShelf struct {
	Key       string                   `json:"key"`
	Label     string                   `json:"label"`
	Level     string                   `json:"level,omitempty"`     // optional declared hierarchy level key
	Sort      string                   `json:"sort,omitempty"`      // released (default)|title|added|year|rating|index|lastViewed
	Direction string                   `json:"direction,omitempty"` // desc (default)|asc
	Facet     string                   `json:"facet,omitempty"`     // single-facet shorthand (declared and indexed)
	Value     string                   `json:"value,omitempty"`     // single value for facet
	Values    []string                 `json:"values,omitempty"`    // OR values for facet
	Filters   *KindLibraryShelfFilters `json:"filters,omitempty"`   // facet map, TabFilter, or rule/rules list
	// Purpose tells apps what the shelf is for, so they can draw it their
	// own way and keep its titles out of other rows: recentContexts ("Jump
	// back in") or liked (Liked songs).
	Purpose string `json:"purpose,omitempty"`
	// Layout is the shelf's default look (standard, hero, wide, tiles or
	// tracks); an owner's look on a Recommendations page wins.
	Layout string `json:"layout,omitempty"`
}

// EffectiveSort returns the shelf's sort field ("released" by default).
func (s KindLibraryShelf) EffectiveSort() string {
	if s.Sort != "" {
		return s.Sort
	}
	return "released"
}

// EffectiveDirection returns the shelf's sort direction ("desc" by default).
func (s KindLibraryShelf) EffectiveDirection() string {
	if s.Direction == "asc" {
		return "asc"
	}
	return "desc"
}

// EffectiveTabFilter returns the shelf's rule-based TabFilter when Filters
// was authored as rules or a TabFilter, or nil otherwise.
func (s KindLibraryShelf) EffectiveTabFilter() *core.TabFilter {
	if s.Filters == nil || s.Filters.Tab.IsEmpty() {
		return nil
	}
	return s.Filters.Tab.Clone()
}

// EffectiveFilters returns the shelf's facet filter map (AND across facets,
// OR within each facet), or nil when the shelf has no facet map filter.
func (s KindLibraryShelf) EffectiveFilters() map[string][]string {
	if s.Facet != "" {
		if len(s.Values) > 0 {
			vals := make([]string, 0, len(s.Values))
			for _, v := range s.Values {
				if v = strings.TrimSpace(v); v != "" {
					vals = append(vals, v)
				}
			}
			if len(vals) == 0 {
				return nil
			}
			return map[string][]string{s.Facet: vals}
		}
		if v := strings.TrimSpace(s.Value); v != "" {
			return map[string][]string{s.Facet: {v}}
		}
		return nil
	}
	if s.Filters == nil || len(s.Filters.Facets) == 0 {
		return nil
	}
	out := make(map[string][]string, len(s.Filters.Facets))
	for k, raw := range s.Filters.Facets {
		var vals []string
		for _, v := range raw {
			if v = strings.TrimSpace(v); v != "" {
				vals = append(vals, v)
			}
		}
		if len(vals) > 0 {
			out[k] = vals
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// KindLevel is one hierarchy level of a kind.
type KindLevel struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Parent    string `json:"parent,omitempty"`    // optional earlier level key; defaults to the previous level
	Media     string `json:"media,omitempty"`     // optional media class override: video|music|audiobook|book|podcast|photo
	Role      string `json:"role,omitempty"`      // free-text semantic role (e.g. artist, album, track, concert)
	Playable  bool   `json:"playable,omitempty"`  // leaves the resolver can open; at least one level
	Container bool   `json:"container,omitempty"` // item is a container other leaves are extracted from
	// Members: a list level (a playlist, a chart). Its items are roots of
	// their own (no parent) and carry `members`, an ordered list of other
	// items of the kind (by external id) that are their children. It is
	// never playable and no level names it as parent; a browse of it
	// (level=<key>, no parentId) may return the members and their parents in
	// the same page so they are stored together.
	Members bool `json:"members,omitempty"`
}

// KindField is one declared item field.
type KindField struct {
	Key     string   `json:"key"`  // ^[a-z][a-zA-Z0-9_]{0,39}$
	Type    string   `json:"type"` // string|int|float|bool|date|duration|enum|strings|people
	Label   string   `json:"label"`
	Indexed bool     `json:"indexed,omitempty"` // gets a typed column in the item store
	Enum    []string `json:"enum,omitempty"`
	// Values optionally declares a known vocabulary for a "strings" field so
	// the rules editor can offer those values before any item uses them.
	Values []string `json:"values,omitempty"`
	// Fact controls whether this field is surfaced in TitleMedia.facts.
	// Nil follows the default rule (non-structural fields appear as facts);
	// explicit true/false overrides it.
	Fact *bool `json:"fact,omitempty"`
}

// KindArt declares one extra artwork role.
type KindArt struct {
	Role  string `json:"role"`
	Label string `json:"label"`
}

// Builtin kind art roles; declared roles must not repeat them.
var BuiltinKindArtRoles = map[string]bool{"poster": true, "backdrop": true, "thumb": true, "logo": true}

// DefaultKindExpandTTLHours is the default freshness window (7 days) for an
// expanded facet bucket or people credit before opening it again triggers a
// fresh background expansion.
const DefaultKindExpandTTLHours = 168

// KindBrowseExpand declares on-demand expansion for specific browse facets
// (collection buckets or people fields) and/or container levels opened by a viewer.
type KindBrowseExpand struct {
	Facets   []string `json:"facets,omitempty"`   // subset of browse.facets
	Levels   []string `json:"levels,omitempty"`   // container levels (container: true) expanded on open
	TTLHours int      `json:"ttlHours,omitempty"` // default 168 (7 days)
}

// EffectiveTTLHours returns the configured freshness window in hours,
// defaulting to DefaultKindExpandTTLHours (168).
func (e *KindBrowseExpand) EffectiveTTLHours() int {
	if e == nil || e.TTLHours <= 0 {
		return DefaultKindExpandTTLHours
	}
	return e.TTLHours
}

// ExpandsLevel reports whether level is declared for container-on-open
// expansion in browse.expand.levels.
func (e *KindBrowseExpand) ExpandsLevel(level string) bool {
	if e == nil || level == "" {
		return false
	}
	for _, lv := range e.Levels {
		if lv == level {
			return true
		}
	}
	return false
}

// KindImportDecl declares a kind's playlist import support.
type KindImportDecl struct {
	// Links names the sites whose playlist links the plugin reads (for apps
	// to show, e.g. "deezer"); empty when it reads none.
	Links []string `json:"links,omitempty"`
	// Identify: the plugin answers /identify, which file imports need.
	Identify bool `json:"identify,omitempty"`
}

// KindRelatedDecl names which relation purposes a kind's plugin answers.
type KindRelatedDecl struct {
	Similar      bool `json:"similar,omitempty"`
	Continuation bool `json:"continuation,omitempty"`
}

// Answers reports whether purpose is declared.
func (d *KindRelatedDecl) Answers(purpose string) bool {
	if d == nil {
		return false
	}
	switch purpose {
	case "similar":
		return d.Similar
	case "continuation":
		return d.Continuation
	}
	return false
}

// KindBrowseDecl is how a kind library is browsed.
type KindBrowseDecl struct {
	Sorts        []string          `json:"sorts,omitempty"`        // subset of title|added|released|year|rating|index
	Facets       []string          `json:"facets,omitempty"`       // field keys; each must be indexed
	Search       bool              `json:"search"`                 // full-text over the title plus SearchFields
	SearchFields []string          `json:"searchFields,omitempty"` // declared text fields included in search
	Expand       *KindBrowseExpand `json:"expand,omitempty"`       // on-demand background facet/person expansion
	// LiveSearch: client searches also call the plugin's /search route
	// with a short budget; results are upserted and merged (§15.4).
	LiveSearch bool `json:"liveSearch,omitempty"`
}

// Kind sync modes (spec §3.5 as narrowed by §15.4: no ondemand mode).
const (
	KindSyncPull = "pull"
	KindSyncPush = "push"
)

// Kind sync undo modes for push-synced kinds (#766).
//   - "snapshots" (default when omitted): journal previous copies of modified
//     items so undo restores them; unchanged items are never snapshotted.
//   - "ids": journal only created and touched item IDs without reading item
//     snapshots; undo deletes only items the batch created.
const (
	KindSyncUndoSnapshots = "snapshots"
	KindSyncUndoIDs       = "ids"
)

// KindSyncDecl is how a kind's items reach Ether's store. Mode is empty
// only for a resolver-only kind, which owns no item sync; a catalog
// provider must pick pull or push.
type KindSyncDecl struct {
	Mode    string `json:"mode"`              // pull|push
	Refresh string `json:"refresh,omitempty"` // at least 15m, pull only
	Undo    string `json:"undo,omitempty"`    // snapshots|ids, push only
}

// KindCacheDecl is the per-library blob cache budget (§5.4).
type KindCacheDecl struct {
	MaxBytes int64  `json:"maxBytes,omitempty"` // 0 inherits the media pool budget.
	TTL      string `json:"ttl,omitempty"`
}

// KindMatchDecl holds matcher defaults for a kind (spec §7).
type KindMatchDecl struct {
	// ThresholdDefault is the score (0..1) at or above which an
	// automatic match is accepted; 0 leaves the server default.
	ThresholdDefault float64 `json:"thresholdDefault,omitempty"`
}

// FrontendAdapterMeta declares one plugin-provided protocol surface
// (spec §6.5).
type FrontendAdapterMeta struct {
	Key          string   `json:"key"`
	Label        string   `json:"label"`
	ProtocolHint string   `json:"protocolHint"` // opds|subsonic|custom
	PathPrefix   string   `json:"pathPrefix"`
	DefaultPort  int      `json:"defaultPort,omitempty"`
	Clients      []string `json:"clients,omitempty"`
	Auth         string   `json:"auth,omitempty"` // ether_token (default)|basic|none_lan|query_credentials
	// Libraries lists the plugin's media-kind keys this adapter publishes.
	// Ether resolves each key to the library the kind created and admits a
	// viewer only to the libraries they may see; a viewer who sees none of
	// them is answered 404. Empty means the adapter is not library-scoped.
	Libraries []string `json:"libraries,omitempty"`
	// SearchPaths optionally lists adapter subpaths (such as "/search",
	// "/rest/search3", or "/graphql") that handle interactive viewer searches
	// and may call ai.complete when permissions.ai is "complete+search" (#702).
	// When omitted, conventional search subpaths (ending in /search, /search2,
	// or /search3, with optional .json/.xml/.view suffix) are recognized.
	SearchPaths []string `json:"searchPaths,omitempty"`
	// UploadPaths opt in to disk staging for selected adapter subpaths.
	UploadPaths []FrontendUploadPath `json:"uploadPaths,omitempty"`
	// Icon is the adapter's own mark on the Apps page: a path to an SVG or
	// PNG inside the package. Without one the plugin's icon stands in.
	Icon string `json:"icon,omitempty"`
}

// FrontendUploadPath declares a glob and the maximum wire body size in bytes.
type FrontendUploadPath struct {
	Path     string `json:"path"`
	MaxBytes int64  `json:"maxBytes"`
}

// FrontendUploadHeader identifies the metadata-only body on sidecar transports.
const FrontendUploadHeader = "X-Ether-Frontend-Upload"

type FrontendUpload = wasm.FrontendUpload
type FrontendUploadBlob = wasm.FrontendUploadBlob

// SetupMeta declares guided setup for any plugin kind (spec §8).
type SetupMeta struct {
	Steps []SetupStep `json:"steps"`
}

// SetupStep is one setup wizard step. An empty Test means saving the
// fields is the whole step; otherwise Ether posts the step's saved
// field values to Test, conventionally /ether/v1/setup/<key>/test.
type SetupStep struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Hint     string   `json:"hint,omitempty"`
	Fields   []string `json:"fields"` // settings field keys this step collects
	Test     string   `json:"test"`   // plugin route, or "" for no test
	Required bool     `json:"required"`
}

// Request is one HTTP-shaped call from Ether to a plugin (SPEC §5.1.2).
// Hosted plugins receive the same object in-process.
type Request struct {
	Op       string            `json:"op,omitempty"`
	ID       string            `json:"id,omitempty"`
	PluginID string            `json:"pluginId"`
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Query    map[string]string `json:"query,omitempty"`
	// RepeatedQuery holds every value of query keys that appeared more than
	// once on a frontend adapter request (Subsonic sends id=1&id=2). Hosted
	// runtimes deliver those keys as arrays; the remote wire keeps Query's
	// first value. Host-side only.
	RepeatedQuery map[string][]string `json:"-"`
	Headers       map[string]string   `json:"headers,omitempty"`
	Body          json.RawMessage     `json:"body,omitempty"`
	// BodyBase64 carries the exact request bytes, base64-encoded, when the
	// caller needs them byte-for-byte (frontend adapter routes). Body holds
	// the parsed JSON value when the bytes are valid JSON; a non-JSON body
	// arrives only here, never as an invalid RawMessage.
	BodyBase64  string          `json:"bodyBase64,omitempty"`
	ContentType string          `json:"contentType,omitempty"`
	Upload      *FrontendUpload `json:"upload,omitempty"`
	// Locale is the console locale for calls that return copy.
	Locale string `json:"locale,omitempty"`
	// Foreground marks an /ether/job/ call that a viewer is waiting on
	// (playback resolution). It never yields to background throttling.
	// Host-side only; never sent to the plugin.
	Foreground bool `json:"-"`
	// Deadline carries the effective per-call deadline computed by the manager
	// (including live overrides) for hosted runtimes. Host-side only.
	Deadline time.Duration `json:"-"`
	// AISearch marks a search route whose plugin has active permissions.ai:
	// "complete+search" so hosted runtimes allow Deadline up to the AI search
	// route bound. Host-side only.
	AISearch bool `json:"-"`
}

// FrontendBlob asks the host to serve bytes directly. A blob:<sha256> ref
// requires LibraryKey for an owned, viewer-visible kind library and can select
// an archive member. A durable:<sha256> ref serves the plugin's entire saved
// blob with durable storage permission.
type FrontendBlob struct {
	LibraryKey  string `json:"libraryKey,omitempty"`
	Ref         string `json:"ref"`
	MemberIndex *int   `json:"memberIndex,omitempty"`
}

// FrontendMedia asks Ether to serve one of the plugin's own playable kind
// items to the authenticated adapter viewer, at the adapter URL itself, with
// Range support. Ether re-checks ownership and library visibility exactly as
// frontend.streamURL does; the bytes never pass through the plugin. It suits
// protocols whose clients do not follow redirects or re-request ranges long
// after a stream ticket would have expired.
type FrontendMedia struct {
	ItemRef   string `json:"itemRef"`
	PartIndex *int   `json:"partIndex,omitempty"`
	Source    string `json:"source,omitempty"`
}

// FrontendArt asks Ether to serve the stored artwork of one of the plugin's
// own kind items (role poster, thumb, backdrop or logo; empty picks the
// first). Ether fetches and caches the image itself, so a plugin cannot use it
// to proxy arbitrary URLs.
type FrontendArt struct {
	ItemRef string `json:"itemRef"`
	Role    string `json:"role,omitempty"`
}

// Response is a plugin's answer to a Request.
type Response struct {
	Blob    *FrontendBlob     `json:"blob,omitempty"`
	Media   *FrontendMedia    `json:"media,omitempty"`
	Art     *FrontendArt      `json:"art,omitempty"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`

	pendingFallback func() error
}

// Runtime is how the host reaches one installed plugin. WS8a (hosted) and
// WS8b (remote) implement it; tests use a fake. Call must honour ctx
// deadlines. Close releases the instance; the host may open a fresh one
// after a failure (restart with backoff).
type Runtime interface {
	Call(ctx context.Context, req Request) (Response, error)
	Close() error
}

// Installed is the host's record of one installed plugin, handed to a
// RuntimeFactory when an instance is opened.
type Installed struct {
	RuntimeLimits *RuntimeLimits
	Busy          func() bool
	ID            string
	Manifest      Manifest
	// SourceURL is where the manifest came from (remote endpoint base).
	SourceURL string
	// Files returns an artifact file (entry, module, strings, schema) for
	// the installed version, or false. Hosted runtimes load code through it.
	Files func(path string) ([]byte, bool)
	// Host is the per-plugin host-services handle (SPEC §5.1.5).
	Host Host
}

// RuntimeFactory opens a Runtime for one installed plugin. The factory is
// chosen by manifest ether.runtime.kind ("hosted" or "remote").
type RuntimeFactory func(inst Installed) (Runtime, error)

// KVUsage reports a plugin's current KV bytes (including the 256-byte charge
// per key), key count, and configured quotas (#591).
type KVUsage struct {
	Bytes      int64 `json:"bytes"`
	Keys       int64 `json:"keys"`
	QuotaBytes int64 `json:"quotaBytes"`
	QuotaKeys  int64 `json:"quotaKeys"`
}

// Poster badge bounds (#594).
const (
	MaxPosterBadgeSets     = 4
	MaxBadgeEntriesPerCall = 500
	MaxBadgeLabelsPerTitle = 3
	MaxBadgeLabelLen       = 16
	PosterBadgeQuotaTitles = 200_000
)

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

// Host is the set of Ether services a plugin may call (SPEC §5.1.5).
// Every method re-checks the plugin's live grants, so a revoke takes
// effect mid-call. Writes are journalled; in shadow mode they are
// recorded without applying. The hosted runtime exposes Host as imports;
// remote plugins reach it over /ether/plugin-host/* (WS8b).
type Host interface {
	KVGet(ctx context.Context, key string) ([]byte, bool, error)
	KVPut(ctx context.Context, key string, value []byte) error
	KVDelete(ctx context.Context, key string) error
	KVList(ctx context.Context, prefix string) ([]string, error)
	KVUsage(ctx context.Context) (KVUsage, error)

	// BadgesSet, BadgesClear and BadgesUsage need poster_badges (#594).
	BadgesSet(ctx context.Context, setKey string, entries []BadgeEntry) error
	BadgesClear(ctx context.Context, setKey string) error
	BadgesUsage(ctx context.Context) (BadgeUsage, error)

	// SecretGet returns a sealed setting the manifest declared in
	// permissions.secrets (owner-configured first, then plugin-written derived
	// secrets). Hosted plugins only.
	SecretGet(ctx context.Context, name string) (string, error)
	// SecretSet stores a sealed derived secret under a name declared in
	// permissions.secrets that is not an owner-configured settings secret.
	// Passing an empty value deletes the derived secret. ttlSec <= 0 means no
	// expiry; positive values expire after ttlSec seconds.
	SecretSet(ctx context.Context, name, value string, ttlSec int64) error
	// SecretDelete removes a plugin-written derived secret declared in
	// permissions.secrets. Owner-configured settings secrets cannot be deleted.
	SecretDelete(ctx context.Context, name string) error

	// HTTPFetch fetches through egress and safefetch; only manifest
	// network hosts are allowed. Observed hosts feed the privacy label.
	HTTPFetch(ctx context.Context, req FetchRequest) (FetchResponse, error)

	// CatalogSearch and CatalogGet need catalog: read. Results are JSON
	// title summaries produced by the injected catalog backend.
	CatalogSearch(ctx context.Context, query string, limit, offset int) (json.RawMessage, error)
	CatalogGet(ctx context.Context, id string) (json.RawMessage, error)

	// ViewerHistory needs viewers.watch_history for that viewer.
	ViewerGranted(ctx context.Context) (json.RawMessage, error)
	ViewerHistory(ctx context.Context, viewerID int64, limit, offset int) (json.RawMessage, error)
	// SetWatchState needs viewers.write_watch_state for the requested viewer.
	// titleID is an owned playable item ref or a catalogue movie/episode ID.
	SetWatchState(ctx context.Context, viewerID int64, titleID string, watched bool) error
	// CollectionsUpsert needs collections: write; journalled.
	CollectionsUpsert(ctx context.Context, collection json.RawMessage) error

	// KindUpsert and KindDelete need catalog_provider and a push-mode
	// kind (spec §3.6): the sync engine is the only writer for pull
	// kinds. Writes are journalled in shadow mode without applying.
	KindUpsert(ctx context.Context, kindKey string, items []core.KindItem) (accepted int, err error)
	KindDelete(ctx context.Context, kindKey string, externalIDs []string) error
	// KindItems reads the plugin's own library back, keyset-paged.
	KindItems(ctx context.Context, kindKey, cursor string, limit int) (core.KindPage, error)
	// KindVisible returns the subset of external IDs (at most MaxKindVisibleIDs)
	// in kindKey that the library's Rules and viewer access policy allow for the
	// requesting frontend-adapter viewer (#615). Only viewer-authenticated
	// frontend adapter requests may call it; the viewer comes from the adapter
	// request context, never from guest input.
	KindVisible(ctx context.Context, kindKey string, ids []string) ([]string, error)
	// KindBrowse browses or searches kindKey using the same LibraryPolicy path
	// as Ether's console and protocol projectors for the requesting
	// frontend-adapter viewer (#615). Only viewer-authenticated frontend adapter
	// requests may call it; the viewer comes from the adapter request context,
	// never from guest input.
	KindBrowse(ctx context.Context, kindKey string, q core.KindBrowseRequest) (core.KindPage, error)
	// ResolveSources needs kind_resolver and permissions.sources
	// (search to look, acquire to open debrid content, §4.5).
	ResolveSources(ctx context.Context, q core.SourceQuery) ([]core.SourceCandidate, error)
	// CachePut stores one blob in a kind library's cache namespace and
	// returns its "blob:<sha256>" reference; CacheOpen reads one back;
	// CacheDelete removes one from this plugin's own namespaces.
	CachePut(ctx context.Context, libraryKey, name string, r io.Reader) (blobRef string, err error)
	CacheOpen(ctx context.Context, blobRef string) (io.ReadSeekCloser, error)
	CacheDelete(ctx context.Context, blobRef string) error

	// StreamURL mints a short-lived, scoped ticket for one of this
	// plugin's own kind items and returns the Ether stream URL a frontend
	// adapter embeds instead of a plugin URL (spec §6.5). The viewer is
	// the one the proxied request authenticated (X-Ether-Viewer); items
	// from another plugin, or a library the viewer cannot see, are
	// refused. Bytes are then served by Ether, never by the plugin.
	StreamURL(ctx context.Context, itemRef string) (string, error)
	StreamPartURL(ctx context.Context, itemRef string, partIndex *int) (string, error)
	StreamURLWithOptions(ctx context.Context, itemRef string, opts StreamURLOptions) (string, error)

	// FoldersRoots lists the live owner-picked absolute paths on non-client routes.
	FoldersRoots(ctx context.Context) ([]string, error)

	// FoldersList and FoldersStat need folders: read and an owner-picked
	// root. They never return file contents.
	FoldersList(ctx context.Context, root, path string, options ...FolderListOptions) (FolderPage, error)
	FoldersStat(ctx context.Context, root, path string) (FolderEntry, error)
	FoldersHash(ctx context.Context, root, path string, options ...FolderHashOptions) (FolderHash, error)
	FoldersHashStart(ctx context.Context, root, path string, options ...FolderHashOptions) (FolderHashJob, error)
	FoldersFrame(ctx context.Context, root, path string, options FolderFrameOptions) (FolderFrame, error)
	FoldersHashPoll(ctx context.Context, id string) (FolderHashJob, error)
	FoldersHashCancel(ctx context.Context, id string) (FolderHashJob, error)

	// Settings returns the plugin's current non-secret settings. Plugins
	// read them per request instead of keeping them in instance memory,
	// because hosted plugins run in a pool of instances that restart.
	// Secrets stay behind SecretGet.
	Settings(ctx context.Context) (map[string]any, error)

	// PublishEvent emits one manifest-declared plugin event onto Ether's event
	// bus as "plugin:<pluginId>/<key>". Requires the events capability and a
	// matching entry in contributes.events.publish; payload is schema-validated
	// and rate-limited.
	PublishEvent(ctx context.Context, key string, payload json.RawMessage, corr events.Correlation) error

	// ScreensInvalidate makes every open screen and surface of this plugin
	// read its sources again now (screens capability). It returns how many
	// open streams heard it; nobody watching means nothing happens.
	ScreensInvalidate(ctx context.Context) (int, error)
	// ScreensPutRecords pushes new values for the plugin's own records
	// ([{type, id, value}], at most 100) to open screens already showing
	// them (screens capability).
	ScreensPutRecords(ctx context.Context, records json.RawMessage) (int, error)

	// AIComplete runs a bounded text or JSON-Schema-validated completion against
	// the owner's configured language model (Settings -> Integrations -> Language
	// model). Requires permissions.ai: "complete" (or "complete+search" to also
	// allow bounded, cached calls on Stremio catalog search, kinds search, and
	// declared frontend-adapter search routes; #702). Every other viewer/client
	// request path stays refused. Returns ErrNotConfigured when no model is
	// configured.
	AIComplete(ctx context.Context, req AICompleteRequest) (AICompleteResponse, error)

	// MediaProbe reads container metadata ({durationSec, container, width,
	// height, sizeBytes}) for a PlayableSource in the same shape /resolve
	// returns, reusing Ether's durable release_probes cache. Requires
	// permissions.media: "probe", is restricted to jobs, actions, and
	// background routes (never viewer or client request paths), is rate-limited
	// per plugin, and is bounded at 10 s per probe.
	MediaProbe(ctx context.Context, src core.PlayableSource) (core.MediaProbeResult, error)

	// NotifyOwner posts a console notice (rate-limited).
	NotifyOwner(ctx context.Context, text string) error
	// Log appends to the plugin's log ring (redacted, rate-limited).
	Log(ctx context.Context, level, msg string, attrs map[string]any) error
}

// AIMessage is one chat turn sent to Host.AIComplete.
type AIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// AICompleteRequest configures one completion on the owner's configured
// language model (#669).
type AICompleteRequest struct {
	Messages []AIMessage `json:"messages"`
	// Schema is an optional JSON Schema object. When set, the host extracts
	// JSON from the reply, validates it against Schema, and populates JSON
	// (returning ErrInvalidOutput if validation fails). External $ref URLs
	// are rejected.
	Schema json.RawMessage `json:"schema,omitempty"`
	// MaxTokens bounds output tokens (default DefaultAIMaxTokens, max MaxAIMaxTokens).
	MaxTokens int `json:"maxTokens,omitempty"`
	// Temperature optionally adjusts sampling in [0, 2]. Providers or models
	// that disallow explicit temperature ignore it safely.
	Temperature *float64 `json:"temperature,omitempty"`
	// TimeoutMS optionally tightens the call timeout (clamped to MaxAITimeout
	// on background/action paths, MaxAISearchTimeout on interactive search
	// routes, and the enclosing call deadline).
	TimeoutMS int64 `json:"timeoutMs,omitempty"`
	// SearchQuery identifies the viewer's search query when calling ai.complete
	// on an interactive search route under permissions.ai: "complete+search"
	// (#702). Required on frontend-adapter search routes that do not carry a
	// query parameter in the URL (such as GraphQL POST endpoints) and used for
	// normalized per-query caching so repeated searches do not re-ask the model.
	SearchQuery string `json:"searchQuery,omitempty"`
}

// AICompleteResponse is the completion returned by Host.AIComplete.
type AICompleteResponse struct {
	// Text is the assistant reply (with reasoning blocks stripped). When
	// Schema was provided, Text holds the compact canonical JSON of JSON.
	Text string `json:"text"`
	// JSON holds the validated JSON value when Schema was provided.
	JSON json.RawMessage `json:"json,omitempty"`
}

// AICompleteBackendRequest is passed from the plugin host to Backends.AIComplete
// after permissions, route policy, input bounds, and rate limits have been checked.
type AICompleteBackendRequest struct {
	Messages    []AIMessage
	JSONMode    bool
	MaxTokens   int
	Temperature *float64
}

// StreamURLOptions scopes a frontend adapter stream ticket to a specific
// zero-based item part and/or resolver PlayableSource.ID (#576, #661) and
// optionally asks Ether to return an absolute URL using the client-visible
// request origin or the adapter's first published address (#772).
type StreamURLOptions struct {
	Fallback  bool              `json:"fallback,omitempty"` // Try remaining sources when the named source cannot open.
	PartIndex *int              `json:"partIndex,omitempty"`
	Source    string            `json:"source,omitempty"`
	Absolute  bool              `json:"absolute,omitempty"`
	Wait      *StreamWaitPolicy `json:"wait,omitempty"`
}

// StreamWaitPolicy opts a download ticket into holding the media response
// during preparation. MaxSeconds must be positive. Ether caps it at 1800.
type StreamWaitPolicy struct {
	MaxSeconds int `json:"maxSeconds"`
}

// FetchRequest is one plugin outbound HTTP call.
type FetchRequest struct {
	TimeoutMS int64             `json:"timeoutMs,omitempty"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      []byte            `json:"body,omitempty"`
	// KeepHeadersOnRedirect lists optional host or wildcard (*.example.org)
	// patterns (within permissions.network / granted hosts) that may keep
	// credentials and caller headers across redirects when the target is not
	// already the same origin or a strict same-site subdomain. Never applies
	// across HTTPS -> HTTP downgrades.
	KeepHeadersOnRedirect []string `json:"keepHeadersOnRedirect,omitempty"`
}

// FetchResponse is the answer to a FetchRequest (body capped at 10 MiB).
type FetchResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

// FetchManyOptions configures a bounded concurrent http.fetchMany batch call.
// Concurrency defaults to DefaultFetchManyConcurrency (8) and clamps to
// MaxFetchManyConcurrency (8); there is no owner setting.
type FetchManyOptions struct {
	Concurrency int `json:"concurrency,omitempty"`
}

// FetchManyError describes one failed request inside http.fetchMany. Code
// matches the hosted fault code vocabulary ("timeout", "forbidden",
// "invalid_input", "host_error", "bad_arguments").
type FetchManyError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// FetchManyResult is one ordered answer from http.fetchMany. Either Error is
// non-nil or Response carries the received HTTP response (including non-2xx
// statuses, matching http.fetch).
type FetchManyResult struct {
	Response FetchResponse   `json:"-"`
	Error    *FetchManyError `json:"error,omitempty"`
}

// FolderHashJob is a pollable background hash. Bytes is the progress so far.
// Terminal states are done, canceled and error. Only done carries Result.
type FolderHashJob struct {
	ID     string      `json:"id"`
	State  string      `json:"state"`
	Bytes  int64       `json:"bytes"`
	Result *FolderHash `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// FolderHash contains digests, never file contents. ModTime is Unix seconds.
type FolderHash struct {
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
	CRC32   string `json:"crc32,omitempty"`
	MD5     string `json:"md5,omitempty"`
	SHA1    string `json:"sha1,omitempty"`
	OSHash  string `json:"oshash,omitempty"`
	PHash   string `json:"phash,omitempty"`
}

// FolderHashOptions limits which digests are computed. Phash requires a background hash job.
type FolderHashOptions struct {
	Algorithms []string `json:"algorithms,omitempty"`
}

// FolderFrameOptions selects a timestamp, image width and optional owned cache library.
type FolderFrameOptions struct {
	AtSec      float64 `json:"atSec"`
	Width      int     `json:"width"`
	LibraryKey string  `json:"libraryKey,omitempty"`
}

// FolderFrame identifies a cached PNG image.
type FolderFrame struct {
	Ref        string `json:"ref"`
	LibraryKey string `json:"libraryKey"`
}

// FolderListOptions selects names strictly after Cursor in ascending name order.
type FolderListOptions struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// FolderPage is a name-sorted page. NextCursor is empty at the end.
type FolderPage struct {
	Entries    []FolderEntry `json:"entries"`
	NextCursor string        `json:"nextCursor"`
	Truncated  bool          `json:"truncated"`
}

// FolderEntry is one entry from FoldersList or FoldersStat.
type FolderEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size,omitempty"`
	Mtime int64  `json:"mtime,omitempty"` // unix seconds
}

// Event is one normalized Ether event (SPEC §5.7). Payload is JSON with no
// secrets; viewer identity appears only for plugins granted it (the host
// strips it per plugin at delivery).
type Event struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	At       time.Time       `json:"at"`
	ViewerID int64           `json:"viewerId,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// EventPluginFoldersChanged is private to the hosted plugin whose folder grant changed.
const EventPluginFoldersChanged = "plugin.folders.changed"

type FolderRootsChangedPayload struct {
	Roots []string `json:"roots"`
}

// Normalized event names (SPEC §5.7).
const (
	EventPlaybackStarted   = "playback.started"
	EventPlaybackPaused    = "playback.paused"
	EventPlaybackResumed   = "playback.resumed"
	EventPlaybackStopped   = "playback.stopped"
	EventPlaybackCompleted = "playback.completed"
	EventPlaybackError     = "playback.error"
	EventPlaybackFailover  = "playback.failover"
	EventTitleAvailable    = "title.available"
	EventViewerAdded       = "viewer.added"
	EventViewerAccess      = "viewer.access_changed"
	EventCollectionRebuilt = "collection.rebuilt"
	EventServerHealth      = "server.health_changed"
)

// KnownEvents is the closed v1 event set.
var KnownEvents = map[string]bool{
	EventPlaybackStarted: true, EventPlaybackPaused: true, EventPlaybackResumed: true,
	EventPlaybackStopped: true, EventPlaybackCompleted: true, EventPlaybackError: true,
	EventPlaybackFailover: true, EventTitleAvailable: true, EventViewerAdded: true,
	EventViewerAccess: true, EventCollectionRebuilt: true, EventServerHealth: true,
}

// CandidateFacts is the hot-path input for ranking steps and rule
// conditions (SPEC §5.8), JSON-encoded. It never carries viewer data.
type CandidateFacts struct {
	ReleaseID     string     `json:"releaseId"`
	Name          string     `json:"name"`
	Size          int64      `json:"size,omitempty"`
	Protocol      string     `json:"protocol,omitempty"` // usenet|torrent|direct
	SourceClass   string     `json:"sourceClass,omitempty"`
	Indexer       string     `json:"indexer,omitempty"`
	Resolution    string     `json:"resolution,omitempty"`
	HDR           []string   `json:"hdr,omitempty"`
	VideoCodec    string     `json:"videoCodec,omitempty"`
	AudioCodecs   []string   `json:"audioCodecs,omitempty"`
	AudioChannels string     `json:"audioChannels,omitempty"`
	Languages     []string   `json:"languages,omitempty"`
	Subtitles     []string   `json:"subtitles,omitempty"`
	ReleaseType   string     `json:"releaseType,omitempty"`
	Group         string     `json:"group,omitempty"`
	Cached        bool       `json:"cached,omitempty"`
	Verified      bool       `json:"verified,omitempty"`
	ClientCaps    ClientCaps `json:"clientCaps,omitempty"`
}

// ClientCaps is what the playing client can show.
type ClientCaps struct {
	MaxHeight int      `json:"maxHeight,omitempty"`
	HDR       []string `json:"hdr,omitempty"`
	Codecs    []string `json:"codecs,omitempty"`
}

// CatalogueArtifact is a resolved version from the signed plugin catalogue (WS12).
type CatalogueArtifact struct {
	ID           string
	Version      string
	URL          string
	SHA256       string
	Icon         string
	Kind         Kind
	Permissions  Permissions
	Capabilities []Capability
}

// Catalogue is the signed catalogue hook used by the owner API, install
// and update flows (WS12).
type Catalogue interface {
	http.Handler
	// Resolve looks up a plugin in the verified catalogue by plugin ID (or
	// artifact URL) and optional version (empty means latest). It fails when
	// the plugin is not in the catalogue or is revoked.
	Resolve(ctx context.Context, idOrURL, version string) (CatalogueArtifact, error)
}

// Sentinel errors. Adapters and the API map them to plain reasons.
var (
	ErrNotFound = errors.New("plugins: not found")

	ErrPermissionDenied = errors.New("plugins: permission denied")
	ErrQuotaExceeded    = errors.New("plugins: storage quota exceeded")
	ErrRateLimited      = errors.New("plugins: rate limit exceeded")
	ErrDisabled         = errors.New("plugins: plugin is disabled")
	ErrNoRuntime        = errors.New("plugins: no runtime registered for this plugin")
	ErrUnavailable      = errors.New("plugins: host service unavailable")
	ErrNotConfigured    = errors.New("plugins: language model is not configured")
	ErrInvalidOutput    = errors.New("plugins: invalid language model output")
	ErrBusy             = errors.New("plugins: plugin is busy")
	// ErrUnsupportedBody reports a request body Ether could not encode for
	// the plugin at all: the guest never ran, so it is never a plugin crash.
	ErrUnsupportedBody = errors.New("plugins: request body cannot be delivered to the plugin")
	ErrJobsBusy        = core.ErrJobsBusy

	ErrDigestMismatch = errors.New("plugins: artifact digest does not match")
	ErrRevoked        = errors.New("plugins: plugin was revoked")
)

// RateLimitError reports a rate-limited host call together with when the
// plugin may retry (for example, UTC midnight for a daily allowance). It wraps
// ErrRateLimited so callers checking errors.Is(err, ErrRateLimited) match.
type RateLimitError struct {
	Message    string
	RetryAt    time.Time
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e == nil {
		return ErrRateLimited.Error()
	}
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = ErrRateLimited.Error()
	}
	if !e.RetryAt.IsZero() {
		return fmt.Sprintf("%s (retry after %s)", msg, e.RetryAt.UTC().Format(time.RFC3339))
	}
	if sec := e.RetryAfterSeconds(time.Now()); sec > 0 {
		return fmt.Sprintf("%s (retry in %ds)", msg, sec)
	}
	return msg
}

func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// RetryAfterSeconds returns the positive whole-second delay until the limit
// resets (at least 1 second when a retry window is set).
func (e *RateLimitError) RetryAfterSeconds(now time.Time) int64 {
	if e == nil {
		return 0
	}
	d := e.RetryAfter
	if d <= 0 && !e.RetryAt.IsZero() {
		d = e.RetryAt.Sub(now)
	}
	if d <= 0 {
		if !e.RetryAt.IsZero() {
			return 1
		}
		return 0
	}
	sec := int64((d + time.Second - 1) / time.Second)
	if sec < 1 {
		return 1
	}
	return sec
}

// FieldError is one validation problem, keyed by a dotted field path.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError carries every field problem found at once.
type ValidationError struct{ Fields []FieldError }

func (e *ValidationError) Error() string {
	if len(e.Fields) == 0 {
		return "plugins: invalid"
	}
	return fmt.Sprintf("plugins: invalid: %s: %s", e.Fields[0].Field, e.Fields[0].Message)
}

// MetadataMeta declares a provider of canonical catalogue records. Resolution
// happens during explicit materialization, never as part of ordinary browsing.
// Ether-native providers implement POST /ether/metadata/resolve using
// core.MetadataRequest and a {"title": core.Title} response.
type MetadataMeta struct {
	Types []string `json:"types"`
}

// DirectoryMeta declares a protocol-neutral plugin discovery source.
// Entries are suggestions, never installation authorization or verification.
type DirectoryMeta struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// OnAirFeed declares a bounded household feed. MaxAge is a Go duration.
type OnAirFeed struct {
	ID     string            `json:"id"`
	Fields map[string]string `json:"fields"`
	Sample map[string]any    `json:"sample,omitempty"`
	MaxAge string            `json:"maxAge"`
}

// OnAirSegment is a fixed-duration declarative scene, never arbitrary video.
type OnAirSegment struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Duration int      `json:"duration"`
	File     string   `json:"file"`
	Feeds    []string `json:"feeds"`
	Rating   string   `json:"rating,omitempty"`
}
