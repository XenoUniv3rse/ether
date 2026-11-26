package frontend

import "github.com/braedonsaunders/ether/internal/core"

// Protocol adapters use these aliases to exchange host values without importing
// domain packages directly. Their implementations remain owned by core.
type Viewer = core.Viewer
type Device = core.Device
type Title = core.Title
type Collection = core.Collection
type Person = core.Person
type RatingKey = core.RatingKey

// ItemRef is a plugin item's stable identity (spec §2.2). Protocols that
// address plugin items by their ItemRef verbatim (Stremio's "pk:" id) exchange
// it through the host seam rather than importing core.
type ItemRef = core.ItemRef
type Playlist = core.Playlist
type MediaType = core.MediaType
type MediaClass = core.MediaClass
type MediaRole = core.MediaRole
type TitleParent = core.TitleParent
type MediaCredit = core.MediaCredit
type TitleReading = core.TitleReading
type TitleFact = core.TitleFact

// Lyrics are a title's words, timed or plain.
type Lyrics = core.Lyrics
type LyricLine = core.LyricLine
type LyricWord = core.LyricWord

const (
	MediaClassVideo     = core.MediaClassVideo
	MediaClassMusic     = core.MediaClassMusic
	MediaClassAudiobook = core.MediaClassAudiobook
	MediaClassBook      = core.MediaClassBook
	MediaClassPodcast   = core.MediaClassPodcast
	MediaClassPhoto     = core.MediaClassPhoto
)

const (
	MediaRoleItem       = core.MediaRoleItem
	MediaRoleArtist     = core.MediaRoleArtist
	MediaRoleAlbum      = core.MediaRoleAlbum
	MediaRoleTrack      = core.MediaRoleTrack
	MediaRoleSeries     = core.MediaRoleSeries
	MediaRoleBook       = core.MediaRoleBook
	MediaRoleAudiobook  = core.MediaRoleAudiobook
	MediaRolePart       = core.MediaRolePart
	MediaRoleShow       = core.MediaRoleShow
	MediaRoleEpisode    = core.MediaRoleEpisode
	MediaRoleCollection = core.MediaRoleCollection
	MediaRolePlaylist   = core.MediaRolePlaylist
)

type Page = core.Page
type WatchStatus = core.WatchStatus
type ReadingProgress = core.ReadingProgress
type StreamInfo = core.StreamInfo
type MediaInfo = core.MediaInfo

// Chapter is a probed or declared chapter marker.
type Chapter = core.Chapter
type DecideOptions = core.DecideOptions
type ClientCaps = core.ClientCaps
type PlaybackSourceHistory = core.PlaybackSourceHistory
type Decision = core.Decision
type FacetValue = core.FacetValue
type User = core.User

// LibraryID names one library (built-ins 1 = Movies, 2 = TV Shows).
type LibraryID = core.LibraryID

// The built-in libraries.
const (
	LibraryMovies = core.LibraryMovies
	LibraryShows  = core.LibraryShows
)

// Home hub values stay owned by core. Adapters render hubs through these
// aliases.
type Hub = core.Hub
type HubContext = core.HubContext
type HomeSituation = core.HomeSituation

// Playback session and decision values stay owned by core. Adapters
// observe and render them through these aliases.
type PlaySession = core.PlaySession
type DecisionKind = core.DecisionKind
type DecisionCandidate = core.DecisionCandidate
type DecisionTrace = core.DecisionTrace
type ReleaseVariant = core.ReleaseVariant
type VersionAvailability = core.VersionAvailability
type VersionProfile = core.VersionProfile
type ViewerChoiceSummary = core.ViewerChoiceSummary

const (
	VersionInstant     = core.VersionInstant
	VersionCached      = core.VersionCached
	VersionFetch       = core.VersionFetch
	VersionUnavailable = core.VersionUnavailable
)

const (
	DecisionDirectPlay = core.DecisionDirectPlay
	DecisionRemux      = core.DecisionRemux
	DecisionTranscode  = core.DecisionTranscode
)

func IsLastViewedSort(raw string) bool { return core.IsLastViewedSort(raw) }
func TitleSortKey(s string) string     { return core.TitleSortKey(s) }
func FoldSortTitle(s string) string    { return core.FoldSortTitle(s) }
func EffectiveSortTitle(title, explicit string) string {
	return core.EffectiveSortTitle(title, explicit)
}

func IsSurroundAudioCodec(c string) bool { return core.IsSurroundAudioCodec(c) }

// Release and variant-resolution values stay owned by core. Adapters
// describe and label versions through these aliases.
type Source = core.Source
type Release = core.Release
type ReleaseInfo = core.ReleaseInfo
type ResolveRequest = core.ResolveRequest

const MaxVariants = core.MaxVariants

const VersionSelectorPrefix = core.VersionSelectorPrefix

const KindAutoVersionID = core.KindAutoVersionID

func ResolutionTier(w, h int) int { return core.ResolutionTier(w, h) }

func SourceLabel(s Source) string { return core.SourceLabel(s) }

// Library browse and history values stay owned by core. Adapters build
// queries and read results through these aliases.
type BrowseQuery = core.BrowseQuery
type CreditFilter = core.CreditFilter
type Filter = core.Filter
type FilterGroup = core.FilterGroup
type StringFilter = core.StringFilter
type SortOrder = core.SortOrder
type SortSpec = core.SortSpec
type HistoryEntry = core.HistoryEntry
type HistoryQuery = core.HistoryQuery
type HistoryCount = core.HistoryCount
type WatchPage = core.WatchPage
type TitleFacts = core.TitleFacts

const (
	SortAdded      = core.SortAdded
	SortDuration   = core.SortDuration
	SortPopularity = core.SortPopularity
	SortPlayed     = core.SortPlayed
	SortLastViewed = core.SortLastViewed
	SortRating     = core.SortRating
	SortReleased   = core.SortReleased
	SortTitle      = core.SortTitle
	SortIndex      = core.SortIndex
	SortUpdated    = core.SortUpdated
	SortYear       = core.SortYear
)

// Catalogue key kinds and constructors stay owned by core. Adapters compare
// and rebuild parent keys through these aliases.
const (
	KindShow    = core.KindShow
	KindSeason  = core.KindSeason
	KindEpisode = core.KindEpisode
	KindExtra   = core.KindExtra
)

func ShowKey(tmdb int64) RatingKey { return core.ShowKey(tmdb) }

func PluginKey(seq int64) RatingKey { return core.PluginKey(seq) }

func SeasonKey(show int64, season int) RatingKey {
	return core.SeasonKey(show, season)
}

// Protocol integration contracts remain implemented by the host.
type Presentations = core.Presentations
type VelaPresentation = core.VelaPresentation
type MediaBrowserPresentation = core.MediaBrowserPresentation
type StremioPresentation = core.StremioPresentation
type XtreamPresentation = core.XtreamPresentation
type SiloPresentation = core.SiloPresentation
type SessionPolicy = core.SessionPolicy
type FrontendHostPolicy = core.FrontendHostPolicy
type DeliveryPolicy = core.DeliveryPolicy
type RangeResponse = core.RangeResponse
type RangeRepresentation = core.RangeRepresentation

// Direct delivery crosses the same seam: a playback that can name the provider URL
// a session's file may be fetched from, and the link it names. A playback without
// the capability (a remote cluster, a fixture) simply relays.
type DirectSourceLink = core.DirectSourceLink
type DirectDeliverer = core.DirectDeliverer

var WithRangeResponse = core.WithRangeResponse
var IsGrantCredential = core.IsGrantCredential
var IsAPIKeyCredential = core.IsAPIKeyCredential

// HLS output evidence is shared with adapters without exposing the encoder.
type HLSRung = core.HLSRung
type QualityLimitation = core.QualityLimitation
type HLSDiagnosticSession = core.HLSDiagnosticSession
type HLSPreparationEvent = core.HLSPreparationEvent
