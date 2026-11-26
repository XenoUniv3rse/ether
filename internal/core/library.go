package core

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// LibraryID identifies one library. 1 = Movies, 2 = TV Shows forever;
// ids are never reused.
type LibraryID int64

const (
	LibraryMovies LibraryID = 1
	LibraryShows  LibraryID = 2
)

// LibrarySource says where a library's items come from.
type LibrarySource string

const (
	LibraryBuiltin    LibrarySource = "builtin"    // the TMDB catalogue, split by MediaType
	LibraryPlugin     LibrarySource = "plugin"     // items owned by one plugin MediaKind instance
	LibraryCatalog    LibrarySource = "catalogue"  // owner-selected catalogue sources
	LibraryCollection LibrarySource = "collection" // exactly the titles of one owner collection
)

// Library card shapes.
const (
	CardPoster    = "poster"
	CardLandscape = "landscape"
	CardSquare    = "square"
)

// ValidCardShape reports whether s is a valid stored Library.CardShape value
// ("" for the library's default, or "poster" | "landscape" | "square").
func ValidCardShape(s string) bool {
	switch s {
	case "", CardPoster, CardLandscape, CardSquare:
		return true
	default:
		return false
	}
}

// Library date display formats.
const (
	DateDisplayYear = "year"
	DateDisplayDate = "date"
)

// ValidDateDisplay reports whether s is a valid library dateDisplay value.
func ValidDateDisplay(s string) bool {
	return s == "" || s == DateDisplayYear || s == DateDisplayDate
}

// Library tab view layouts (used on kind: "view" tabs).
const (
	TabLayoutGrid     = "grid"
	TabLayoutList     = "list"
	TabLayoutTimeline = "timeline"
)

// ValidTabLayout reports whether s is a valid stored LibraryTab.Layout value
// ("" for default "grid", or "grid" | "list" | "timeline").
func ValidTabLayout(s string) bool {
	switch s {
	case "", TabLayoutGrid, TabLayoutList, TabLayoutTimeline:
		return true
	default:
		return false
	}
}

// Library tab kinds.
const (
	TabRecommended = "recommended"
	TabLibrary     = "library"
	TabCollections = "collections"
	TabPeople      = "people"
	TabView        = "view"
	TabLive        = "live"
)

// TabFilter narrows a library or view tab using the same rule model as
// collections (match/rules/exclude) and custom libraries (ordered show/hide
// rule groups). Either representation may be sent; ToPolicy compiles both into
// ordered LibraryExclusion rules for libraryfilter.
type TabFilter struct {
	Match             string             `json:"match,omitempty"`   // "all" (default) or "any" for Rules
	Rules             []CollectionRule   `json:"rules,omitempty"`   // positive conditions
	Exclude           []CollectionRule   `json:"exclude,omitempty"` // negative conditions (any match hides)
	Groups            []LibraryExclusion `json:"groups,omitempty"`  // ordered show/hide groups (custom-library style)
	VisibilityDefault string             `json:"visibilityDefault,omitempty"`
}

// UnmarshalJSON accepts either a TabFilter object or a bare []LibraryExclusion
// array so both Collection Studio rule builders and Library Visibility rule
// editors serialize cleanly.
func (f *TabFilter) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*f = TabFilter{}
		return nil
	}
	if trimmed[0] == '[' {
		var groups []LibraryExclusion
		if err := json.Unmarshal(trimmed, &groups); err != nil {
			return err
		}
		*f = TabFilter{Groups: groups}
		return nil
	}
	type plain TabFilter
	var p plain
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	*f = TabFilter(p)
	return nil
}

// IsEmpty reports whether no filter conditions are configured.
func (f *TabFilter) IsEmpty() bool {
	return f == nil || (len(f.Rules) == 0 && len(f.Exclude) == 0 && len(f.Groups) == 0)
}

// Clone returns a deep copy of f.
func (f *TabFilter) Clone() *TabFilter {
	if f == nil {
		return nil
	}
	out := &TabFilter{
		Match:             f.Match,
		VisibilityDefault: f.VisibilityDefault,
	}
	if len(f.Rules) > 0 {
		out.Rules = cloneCollectionRules(f.Rules)
	}
	if len(f.Exclude) > 0 {
		out.Exclude = cloneCollectionRules(f.Exclude)
	}
	if len(f.Groups) > 0 {
		out.Groups = make([]LibraryExclusion, len(f.Groups))
		for i, g := range f.Groups {
			out.Groups[i] = g
			out.Groups[i].Rules = cloneCollectionRules(g.Rules)
		}
	}
	return out
}

func cloneCollectionRules(in []CollectionRule) []CollectionRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]CollectionRule, len(in))
	for i, c := range in {
		if len(c.Value) > 0 {
			v := make(json.RawMessage, len(c.Value))
			copy(v, c.Value)
			c.Value = v
		}
		if len(c.Labels) > 0 {
			c.Labels = append([]string(nil), c.Labels...)
		}
		out[i] = c
	}
	return out
}

// ToPolicy converts f into ordered LibraryExclusion rules and a fallback
// ("show" or "hide") suitable for libraryfilter.CompilePolicy and
// pluginitems.CompilePolicy.
func (f *TabFilter) ToPolicy(mediaType MediaType) ([]LibraryExclusion, string) {
	if f.IsEmpty() {
		return nil, ""
	}
	mt := NormalizeRuleMediaType(string(mediaType))
	if mt == "" {
		mt = "mixed"
	}
	if len(f.Groups) > 0 {
		groups := make([]LibraryExclusion, len(f.Groups))
		hasShow := false
		for i, g := range f.Groups {
			if g.MediaType == "" {
				g.MediaType = mt
			}
			if g.Action == "show" {
				hasShow = true
			}
			groups[i] = g
		}
		fallback := f.VisibilityDefault
		if fallback != "show" && fallback != "hide" {
			if hasShow {
				fallback = "hide"
			} else {
				fallback = "show"
			}
		}
		return NormalizeLibraryRules(groups), fallback
	}
	var out []LibraryExclusion
	if len(f.Exclude) > 0 {
		out = append(out, LibraryExclusion{
			Action:    "hide",
			MediaType: mt,
			Match:     "any",
			Rules:     f.Exclude,
		})
	}
	if len(f.Rules) > 0 {
		match := f.Match
		if match != "any" {
			match = "all"
		}
		out = append(out, LibraryExclusion{
			Action:    "show",
			MediaType: mt,
			Match:     match,
			Rules:     f.Rules,
		})
		return NormalizeLibraryRules(out), "hide"
	}
	return NormalizeLibraryRules(out), "show"
}

// LibraryTab configures one tab inside a library in Vela.
type LibraryTab struct {
	Key               string     `json:"key"`
	Label             string     `json:"label"`
	Kind              string     `json:"kind"`                        // recommended|library|collections|people|view|live
	Level             string     `json:"level,omitempty"`             // optional plugin hierarchy level key (library, view, collections or people)
	Facet             string     `json:"facet,omitempty"`             // collections or people facet
	Order             string     `json:"order,omitempty"`             // count|name (collections or people)
	Sort              string     `json:"sort,omitempty"`              // default sort (library, view, facet collections)
	Direction         string     `json:"direction,omitempty"`         // asc|desc
	Card              string     `json:"card,omitempty"`              // landscape (default)|poster|square (live or recommended tabs)
	Layout            string     `json:"layout,omitempty"`            // grid (default)|list|timeline (view tabs only)
	Search            *bool      `json:"search,omitempty"`            // defaults true for library/collections/people/view/live, false for recommended
	SearchPlaceholder string     `json:"searchPlaceholder,omitempty"` // optional custom search placeholder
	Filters           *TabFilter `json:"filters,omitempty"`           // saved filter rules (view or library)
	Hidden            bool       `json:"hidden,omitempty"`            // hidden from Vela while kept in editor
}

// EffectiveLayout returns the tab's layout ("grid" by default for view tabs,
// empty for other tab kinds).
func (t LibraryTab) EffectiveLayout() string {
	if t.Kind != TabView {
		return ""
	}
	switch t.Layout {
	case TabLayoutGrid, TabLayoutList, TabLayoutTimeline:
		return t.Layout
	default:
		return TabLayoutGrid
	}
}

// EffectiveCard returns the tab's card shape ("landscape" by default for live
// tabs, explicit card when set on recommended tabs, empty otherwise).
func (t LibraryTab) EffectiveCard() string {
	switch t.Kind {
	case TabLive:
		switch t.Card {
		case CardPoster, CardLandscape, CardSquare:
			return t.Card
		default:
			return CardLandscape
		}
	case TabRecommended:
		switch t.Card {
		case CardPoster, CardLandscape, CardSquare:
			return t.Card
		}
	}
	return ""
}

// EffectiveSearch reports whether in-tab search is enabled on t.
func (t LibraryTab) EffectiveSearch() bool {
	if t.Kind == TabRecommended {
		return false
	}
	if t.Search != nil {
		return *t.Search
	}
	return true
}

// Clone returns a deep copy of t.
func (t LibraryTab) Clone() LibraryTab {
	out := t
	if t.Search != nil {
		b := *t.Search
		out.Search = &b
	}
	if t.Filters != nil {
		out.Filters = t.Filters.Clone()
	}
	return out
}

// DefaultCatalogueTabs returns the default tabs when a catalogue or collection
// library has no owner-configured tabs.
func DefaultCatalogueTabs() []LibraryTab {
	searchOff := false
	searchOn := true
	return []LibraryTab{
		{Key: "recommended", Label: "Recommendations", Kind: TabRecommended, Search: &searchOff},
		{Key: "library", Label: "Library", Kind: TabLibrary, Search: &searchOn},
		{Key: "collections", Label: "Collections", Kind: TabCollections, Search: &searchOn},
	}
}

// NormalizeCatalogueTabFacet normalizes a catalogue library facet name to its
// canonical singular key, returning ("", false) when unrecognized. For
// collections, an empty facet means owner collections.
func NormalizeCatalogueTabFacet(kind, facet string) (string, bool) {
	f := strings.ToLower(strings.TrimSpace(facet))
	switch kind {
	case TabCollections:
		switch f {
		case "":
			return "", true
		case "genre", "genres":
			return "genre", true
		case "studio", "studios":
			return "studio", true
		case "network", "networks":
			return "network", true
		case "country", "countries":
			return "country", true
		case "language", "languages":
			return "language", true
		case "contentrating", "content_rating", "content-rating", "contentratings", "content_ratings", "content-ratings", "certification", "certifications":
			return "contentRating", true
		case "year", "years":
			return "year", true
		case "decade", "decades":
			return "decade", true
		case "franchise", "franchises":
			return "franchise", true
		case "tag", "tags":
			return "tag", true
		case "keyword", "keywords":
			return "keyword", true
		case "director", "directors":
			return "director", true
		case "writer", "writers":
			return "writer", true
		case "resolution", "resolutions":
			return "resolution", true
		case "hdr":
			return "hdr", true
		case "audiolanguage", "audio_language", "audio-language", "audiolanguages", "audio_languages", "audio-languages":
			return "audioLanguage", true
		case "rating", "ratings", "ratingband", "rating_band", "rating-band", "ratingbands", "rating_bands", "rating-bands":
			return "rating", true
		case "runtime", "runtimes", "duration", "runtimeband", "runtime_band", "runtime-band", "runtimebands", "runtime_bands", "runtime-bands":
			return "runtime", true
		case "addedmonth", "added_month", "added-month", "addedmonths", "added_months", "added-months":
			return "addedMonth", true
		default:
			return "", false
		}
	case TabPeople:
		switch f {
		case "cast":
			return "cast", true
		case "director", "directors":
			return "director", true
		case "writer", "writers":
			return "writer", true
		default:
			return "", false
		}
	default:
		return "", f == ""
	}
}

// ParseCatalogueRatingBand maps a canonical rating band ID onto (min, max, maxExclusive).
func ParseCatalogueRatingBand(id string) (minR, maxR float64, maxExclusive, ok bool) {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "9-10":
		return 9.0, 10.0, false, true
	case "8-8.9":
		return 8.0, 9.0, true, true
	case "7-7.9":
		return 7.0, 8.0, true, true
	case "6-6.9":
		return 6.0, 7.0, true, true
	case "under-6":
		return 0, 6.0, true, true
	default:
		return 0, 0, false, false
	}
}

// ParseCatalogueRuntimeBand maps a canonical runtime band ID onto inclusive seconds (minSec, maxSec).
func ParseCatalogueRuntimeBand(id string) (minSec, maxSec int64, ok bool) {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "under-30m":
		return 1, 1799, true
	case "30-60m":
		return 1800, 3599, true
	case "60-90m":
		return 3600, 5399, true
	case "90-120m":
		return 5400, 7199, true
	case "120-180m":
		return 7200, 10799, true
	case "180m-plus":
		return 10800, 0, true
	default:
		return 0, 0, false
	}
}

// ParseCatalogueAddedMonth parses a canonical "YYYY-MM" bucket ID into inclusive UTC Unix bounds.
func ParseCatalogueAddedMonth(id string) (fromTS, toTS int64, ok bool) {
	id = strings.TrimSpace(id)
	if len(id) != 7 || id[4] != '-' {
		return 0, 0, false
	}
	t, err := time.Parse("2006-01", id)
	if err != nil {
		return 0, 0, false
	}
	y, m, _ := t.UTC().Date()
	if y < 1970 || y > 9999 {
		return 0, 0, false
	}
	start := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0).Add(-time.Second)
	return start.Unix(), end.Unix(), true
}

// Library is the one library model for builtins, plugin kinds and
// owner catalogue libraries alike (spec §1.1).
type Library struct {
	ID      LibraryID     `json:"id"`
	Key     string        `json:"key"`  // slug, unique, immutable: "movies", "shows", "pk-<plugin>-<kind>", owner slug
	Name    string        `json:"name"` // owner-editable display name
	Source  LibrarySource `json:"source"`
	Sources []string      `json:"sources"`
	// Empty selects the first configured provider that supports MediaType.
	MetadataSource string `json:"metadataSource,omitempty"`
	// Builtin only: TypeMovie or TypeShow.
	MediaType MediaType `json:"mediaType,omitempty"`
	// Plugin only.
	PluginID string `json:"pluginId,omitempty"`
	KindKey  string `json:"kindKey,omitempty"` // MediaKind.Key within that plugin
	// Collection only: the owner collection whose titles this library
	// shows (an MDBList list, a Trakt list, a smart rule, a hand-made
	// list…). MediaType pins movie|show. A deleted
	// collection leaves the library empty, never an error.
	CollectionID int64 `json:"collectionId,omitempty"`
	// Visibility rules for this library. Built-in libraries 1 and 2
	// govern canonical movie/show visibility across every protocol; catalogue
	// libraries may carry additional rules on top of their canonical library.
	RulesBroken       bool               `json:"rulesBroken,omitempty"`
	Rules             []LibraryExclusion `json:"rules,omitempty"`
	VisibilityDefault string             `json:"visibilityDefault,omitempty"`
	// Owner-defined Vela tabs. Empty when TabsCustomized is false, in which
	// case effective tabs come from the plugin declaration or DefaultCatalogueTabs.
	Tabs           []LibraryTab `json:"tabs,omitempty"`
	TabsCustomized bool         `json:"tabsCustomized,omitempty"`
	HideLibraryTab bool         `json:"hideLibraryTab,omitempty"`
	// CardShape overrides how item thumbnails in this library are shaped
	// ("poster", "landscape", or "square"). Empty means default: the plugin's
	// declared library.card for plugin libraries, or "poster" for catalogue
	// libraries.
	CardShape string `json:"cardShape,omitempty"`
	// Presentation: how protocols expose this library (§6.2). For builtins
	// this is synthesised, not stored.
	Present Presentations `json:"present,omitempty"`
	// PresentOverrides records sticky owner on/off switches per app.
	// Nil fields follow the manifest or inferred default; non-nil choices
	// survive plugin updates and restarts.
	PresentOverrides PresentationOverrides `json:"presentOverrides,omitempty"`
	// QualityProfile is the owner-set quality profile name titles in this
	// library rank with. Empty means "Default": today's Movies/Shows
	// home/away defaults apply. Set on any library, including catalogue
	// libraries (an "Anime" library can rank with an anime profile).
	QualityProfile string    `json:"qualityProfile,omitempty"`
	Sort           int       `json:"sort"`   // owner order in every client
	Hidden         bool      `json:"hidden"` // owner hid it from all clients (still in console)
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// Library experiences name how an app presents a library: watch (films and
// TV), listen (a listening app: music, audiobooks, podcasts), read (books and
// comics), gallery (photos and home videos) and live (a guide, multiview,
// sports). The set is open: a plugin may name another id, and an app that
// does not know an id presents the library as watch.
const (
	LibraryExperienceWatch   = "watch"
	LibraryExperienceListen  = "listen"
	LibraryExperienceRead    = "read"
	LibraryExperienceGallery = "gallery"
	LibraryExperienceLive    = "live"
)

// LibraryExperiences lists the experiences Ether itself assigns.
var LibraryExperiences = []string{LibraryExperienceWatch, LibraryExperienceListen, LibraryExperienceRead, LibraryExperienceGallery, LibraryExperienceLive}

var libraryExperienceID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidLibraryExperience reports whether s is a well-formed experience id
// (lowercase letters, digits and dashes, up to 32 characters).
func ValidLibraryExperience(s string) bool {
	return libraryExperienceID.MatchString(s)
}

// EffectiveLibraryExperience is the experience a library asks for: the
// declared one, else its media class's: listen for music, audiobooks and
// podcasts, read for books, gallery for photos, watch for the rest.
func EffectiveLibraryExperience(declared string, class MediaClass) string {
	if ValidLibraryExperience(declared) {
		return declared
	}
	switch class {
	case MediaClassMusic, MediaClassAudiobook, MediaClassPodcast:
		return LibraryExperienceListen
	case MediaClassBook:
		return LibraryExperienceRead
	case MediaClassPhoto:
		return LibraryExperienceGallery
	}
	return LibraryExperienceWatch
}

// EffectiveCardShape resolves the library's card shape: the owner's CardShape
// choice first, then pluginDefault (for plugin libraries), falling back to
// "poster".
func (l Library) EffectiveCardShape(pluginDefault string) string {
	switch l.CardShape {
	case CardPoster, CardLandscape, CardSquare:
		return l.CardShape
	}
	if l.Source == LibraryPlugin {
		switch pluginDefault {
		case CardPoster, CardLandscape, CardSquare:
			return pluginDefault
		}
	}
	return CardPoster
}

// MediaClass names the kind of media a content kind holds.
type MediaClass string

const (
	MediaClassVideo     MediaClass = "video"
	MediaClassMusic     MediaClass = "music"
	MediaClassAudiobook MediaClass = "audiobook"
	MediaClassBook      MediaClass = "book"
	MediaClassPodcast   MediaClass = "podcast"
	MediaClassPhoto     MediaClass = "photo"
)

// ValidMediaClass reports whether s is a supported content kind media class.
func ValidMediaClass(s string) bool {
	switch MediaClass(s) {
	case MediaClassVideo, MediaClassMusic, MediaClassAudiobook, MediaClassBook, MediaClassPodcast, MediaClassPhoto:
		return true
	default:
		return false
	}
}

// MediaRole names what one hierarchy level of a content kind represents.
type MediaRole string

const (
	MediaRoleItem       MediaRole = "item"
	MediaRoleArtist     MediaRole = "artist"
	MediaRoleAlbum      MediaRole = "album"
	MediaRoleTrack      MediaRole = "track"
	MediaRoleSeries     MediaRole = "series"
	MediaRoleBook       MediaRole = "book"
	MediaRoleAudiobook  MediaRole = "audiobook"
	MediaRolePart       MediaRole = "part"
	MediaRoleShow       MediaRole = "show"
	MediaRoleEpisode    MediaRole = "episode"
	MediaRoleCollection MediaRole = "collection"
	// MediaRolePlaylist is a list level (members: true): an ordered list of
	// other items, such as an editorial playlist.
	MediaRolePlaylist MediaRole = "playlist"
)

// ValidMediaRole reports whether s is a supported content kind level role.
func ValidMediaRole(s string) bool {
	switch MediaRole(s) {
	case MediaRoleItem, MediaRoleArtist, MediaRoleAlbum, MediaRoleTrack,
		MediaRoleSeries, MediaRoleBook, MediaRoleAudiobook, MediaRolePart,
		MediaRoleShow, MediaRoleEpisode, MediaRoleCollection, MediaRolePlaylist:
		return true
	default:
		return false
	}
}

// VelaPresentation describes how Vela sees a library.
type VelaPresentation struct {
	Type    string `json:"type,omitempty"`    // movies|shows
	Default *bool  `json:"default,omitempty"` // optional manifest default hint (true when omitted)
}

// PlexPresentation describes how Plex sees a library.
type PlexPresentation struct {
	Base    string `json:"base,omitempty"`    // movie|show|artist|photo
	Default *bool  `json:"default,omitempty"` // optional manifest default hint (true when omitted)
}

// MediaBrowserPresentation describes how Jellyfin and Emby see a library.
type MediaBrowserPresentation struct {
	CollectionType string            `json:"collectionType,omitempty"` // movies|tvshows|music|audiobooks|books|photos|homevideos
	ItemTypes      map[string]string `json:"itemTypes,omitempty"`      // level -> Movie|Series|Season|Episode|Video|MusicArtist|MusicAlbum|Audio|AudioBook|Book|PhotoAlbum|Photo
	Default        *bool             `json:"default,omitempty"`        // optional manifest default hint (true when omitted)
}

// StremioPresentation describes how Stremio sees a library.
type StremioPresentation struct {
	Type    string `json:"type,omitempty"`    // movie|series|channel|tv
	Default *bool  `json:"default,omitempty"` // optional manifest default hint (true when omitted)
}

// XtreamPresentation describes how Xtream sees a library.
type XtreamPresentation struct {
	Category string `json:"category,omitempty"` // movie|series|live
	Default  *bool  `json:"default,omitempty"`  // optional manifest default hint (true when omitted)
}

// SiloPresentation describes how Silo sees a library.
type SiloPresentation struct {
	Type    string `json:"type,omitempty"`    // movies|tvshows (mirrors MediaBrowser family)
	Default *bool  `json:"default,omitempty"` // optional manifest default hint (true when omitted)
}

// Presentations declares how a library is presented per protocol (§6.2).
// In a plugin manifest, each app block describes how that app sees the library
// (and may optionally suggest default: false to start off). On a stored
// core.Library, non-nil app blocks represent the currently published apps with
// their resolved per-protocol shapes.
type Presentations struct {
	AdapterOnly  bool                      `json:"adapterOnly,omitempty"` // only via FrontendAdapter (§6.5)
	Vela         *VelaPresentation         `json:"vela,omitempty"`
	Plex         *PlexPresentation         `json:"plex,omitempty"`
	MediaBrowser *MediaBrowserPresentation `json:"mediaBrowser,omitempty"`
	Stremio      *StremioPresentation      `json:"stremio,omitempty"`
	Xtream       *XtreamPresentation       `json:"xtream,omitempty"`
	Silo         *SiloPresentation         `json:"silo,omitempty"`
}

// PresentationOverrides records explicit per-app owner publish switches.
// Nil fields follow the manifest or inferred default; non-nil choices are
// sticky across plugin updates and server restarts.
type PresentationOverrides struct {
	AdapterOnly  *bool `json:"adapterOnly,omitempty"`
	Vela         *bool `json:"vela,omitempty"`
	Plex         *bool `json:"plex,omitempty"`
	MediaBrowser *bool `json:"mediaBrowser,omitempty"`
	Stremio      *bool `json:"stremio,omitempty"`
	Xtream       *bool `json:"xtream,omitempty"`
	Silo         *bool `json:"silo,omitempty"`
}

// Get returns the owner override for app ("vela"|"plex"|"mediaBrowser"|
// "stremio"|"xtream"|"silo"|"adapterOnly"), or nil if not overridden.
func (o PresentationOverrides) Get(app string) *bool {
	switch app {
	case "adapterOnly":
		return o.AdapterOnly
	case "vela":
		return o.Vela
	case "plex":
		return o.Plex
	case "mediaBrowser":
		return o.MediaBrowser
	case "stremio":
		return o.Stremio
	case "xtream":
		return o.Xtream
	case "silo":
		return o.Silo
	default:
		return nil
	}
}

// Set updates the owner override for app.
func (o *PresentationOverrides) Set(app string, val *bool) {
	if o == nil {
		return
	}
	var cp *bool
	if val != nil {
		v := *val
		cp = &v
	}
	switch app {
	case "adapterOnly":
		o.AdapterOnly = cp
	case "vela":
		o.Vela = cp
	case "plex":
		o.Plex = cp
	case "mediaBrowser":
		o.MediaBrowser = cp
	case "stremio":
		o.Stremio = cp
	case "xtream":
		o.Xtream = cp
	case "silo":
		o.Silo = cp
	}
}

// IsEmpty reports whether no owner overrides are stored.
func (o PresentationOverrides) IsEmpty() bool {
	return o.AdapterOnly == nil && o.Vela == nil && o.Plex == nil &&
		o.MediaBrowser == nil && o.Stremio == nil && o.Xtream == nil && o.Silo == nil
}

// PresentationSource says where an app's presentation shape came from.
type PresentationSource string

const (
	PresentationSourcePlugin      PresentationSource = "plugin"
	PresentationSourceInferred    PresentationSource = "inferred"
	PresentationSourceUnavailable PresentationSource = "unavailable"
)

// ResolvedAppPresentation describes one app row for a library in the owner
// console: whether the app can show the library, whether it is currently
// published, where the presentation came from, and a plain-language reason
// when unavailable.
type ResolvedAppPresentation struct {
	App        string             `json:"app"` // vela|plex|mediaBrowser|stremio|xtream|silo
	Available  bool               `json:"available"`
	Enabled    bool               `json:"enabled"`
	DefaultOn  bool               `json:"defaultOn"`
	Overridden bool               `json:"overridden,omitempty"`
	Source     PresentationSource `json:"source"`              // plugin|inferred|unavailable
	HintValue  string             `json:"hintValue,omitempty"` // e.g. movies, show, artist, music, books, photo
	Reason     string             `json:"reason,omitempty"`    // plain-language reason when unavailable
}

// ResolvedPresentations is the full server-computed presentation picture for a
// library, separating HOW each app sees the library (Available) from WHETHER
// each app currently publishes it (Effective / Apps).
type ResolvedPresentations struct {
	MediaClass  MediaClass                `json:"mediaClass"`
	LevelRoles  map[string]MediaRole      `json:"levelRoles,omitempty"`
	AdapterOnly bool                      `json:"adapterOnly,omitempty"`
	Effective   Presentations             `json:"effective"`
	Available   Presentations             `json:"available"`
	Apps        []ResolvedAppPresentation `json:"apps"`
}

// CanonicalLibraryForTitle returns the one canonical library of a TMDB
// title (§1.2): TypeMovie → 1; TypeShow/Season/Episode → 2. Any other
// type has no canonical library and returns 0.
func CanonicalLibraryForTitle(t MediaType) LibraryID {
	switch t {
	case TypeMovie:
		return LibraryMovies
	case TypeShow, TypeSeason, TypeEpisode:
		return LibraryShows
	default:
		return 0
	}
}

// AllowsLibrary reports whether u may open library id (§1.4): the owner
// and a viewer with no restriction see every library; otherwise the
// library must be listed. A nil user sees nothing.
func (u *User) AllowsLibrary(id LibraryID) bool {
	if u == nil {
		return false
	}
	if u.IsOwner || len(u.AllowedLibraries) == 0 {
		return true
	}
	for _, allowed := range u.AllowedLibraries {
		if allowed == id {
			return true
		}
	}
	return false
}

// CatalogFacetCollectionsQuery requests a paged facet listing over catalogue
// titles matching Filter.
type CatalogFacetCollectionsQuery struct {
	Filter Filter
	Facet  string // genre|studio|network|country|language|contentRating|year|decade|franchise|tag|keyword|director|writer|resolution|hdr|audioLanguage|rating|runtime|addedMonth
	Order  string // count (default) | name
	Query  string // optional folded search string
	Offset int
	Limit  int
}

// CatalogFacetCollection is one facet bucket in a catalogue library's
// collections tab, along with its newest visible title for artwork.
type CatalogFacetCollection struct {
	ID         string
	Name       string
	ItemCount  int
	NewestItem *Title
}

// CatalogFacetCollectionsPage is one page of catalogue facet collections.
type CatalogFacetCollectionsPage struct {
	Items []CatalogFacetCollection
	More  bool
}

// CatalogFacetPeopleQuery requests a paged cast, director, or writer listing
// over catalogue titles matching Filter.
type CatalogFacetPeopleQuery struct {
	Filter Filter
	Facet  string // cast | director | writer
	Order  string // name (default) | count
	Query  string // optional folded search string
	Offset int
	Limit  int
}

// CatalogFacetPerson is one person entry in a catalogue library's people tab.
type CatalogFacetPerson struct {
	PersonTMDB int64
	Name       string
	ItemCount  int
	SampleKey  RatingKey
}

// CatalogFacetPeoplePage is one page of catalogue facet people.
type CatalogFacetPeoplePage struct {
	Items []CatalogFacetPerson
	More  bool
}
