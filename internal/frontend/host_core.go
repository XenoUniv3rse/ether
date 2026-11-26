package frontend

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Core host ports and values stay owned by core. Protocol adapters reach
// them through these aliases instead of importing the domain package.

// Host service ports.
type Auth = core.Auth
type Catalog = core.Catalog
type Collections = core.Collections
type OwnedCollectionLister = core.OwnedCollectionLister
type DownloadRemuxer = core.DownloadRemuxer
type HomeLayoutSource = core.HomeLayoutSource
type HubItemSource = core.HubItemSource
type ImpressionSink = core.ImpressionSink
type NeutralHomeLayoutSource = core.NeutralHomeLayoutSource
type Playback = core.Playback
type PlaybackPrepareWithin = core.PlaybackPrepareWithin
type Playlists = core.Playlists

type Recommender = core.Recommender
type State = core.State
type Transcoder = core.Transcoder
type TrickplayProvider = core.TrickplayProvider
type VariantResolver = core.VariantResolver

// Viewer access policy ports.
type BitrateAccessPolicy = core.BitrateAccessPolicy

// DownloadLimitPolicy is the optional per-viewer download limit capability.
type DownloadAllowance = core.DownloadAllowance
type DownloadLimitDecision = core.DownloadLimitDecision
type DownloadLimitPolicy = core.DownloadLimitPolicy
type DownloadAccessPolicy = core.DownloadAccessPolicy

// Playback, session and request values.
type AudioLimit = core.AudioLimit
type ClientCapabilityOverride = core.ClientCapabilityOverride
type DASHSession = core.DASHSession
type EncoderStatus = core.EncoderStatus
type HLSLadderSession = core.HLSLadderSession
type HLSRequest = core.HLSRequest
type HLSSession = core.HLSSession
type HTTPSession = core.HTTPSession
type ImageURLFunc = core.ImageURLFunc
type NeutralHomeRow = core.NeutralHomeRow
type PersonCredit = core.PersonCredit
type SearchCandidates = core.SearchCandidates
type UncachedError = core.UncachedError

// Owner row styles (Recommendations page rows).
const (
	RowStyleStandard = core.RowStyleStandard
	RowStyleHero     = core.RowStyleHero
	RowStyleWide     = core.RowStyleWide
)

// Playback protocols and media kinds.
const (
	ProtocolDASH    = core.ProtocolDASH
	ProtocolHLS     = core.ProtocolHLS
	ProtocolTorrent = core.ProtocolTorrent
)

const (
	MediaAny   = core.MediaAny
	MediaMovie = core.MediaMovie
	MediaTV    = core.MediaTV
)

// Prewarm reasons.
const (
	PrewarmBrowse  = core.PrewarmBrowse
	PrewarmTrailer = core.PrewarmTrailer
)

// Release-source states.
const (
	UsenetFailed = core.UsenetFailed
	UsenetNone   = core.UsenetNone
)

// Domain errors, shared by identity for errors.Is checks.
var (
	ErrArchiveUnstreamable = core.ErrArchiveUnstreamable
	ErrArticleCorrupt      = core.ErrArticleCorrupt
	ErrArticleMissing      = core.ErrArticleMissing
	ErrBudgetExhausted     = core.ErrBudgetExhausted
	ErrDebridAuth          = core.ErrDebridAuth
	ErrDownloadLimit       = core.ErrDownloadLimit
	ErrDownloadPaused      = core.ErrDownloadPaused
	ErrDownloadQueued      = core.ErrDownloadQueued
	ErrDownloadRefused     = core.ErrDownloadRefused
	ErrNoSources           = core.ErrNoSources
	ErrNoTorrentSources    = core.ErrNoTorrentSources
	ErrNotCached           = core.ErrNotCached
	ErrPolicyUnconfirmed   = core.ErrPolicyUnconfirmed
	ErrPrepareSlow         = core.ErrPrepareSlow
	ErrPreparing           = core.ErrPreparing
	ErrVersionUnavailable  = core.ErrVersionUnavailable
	ErrJobsBusy            = core.ErrJobsBusy
	ErrReleaseRejected     = core.ErrReleaseRejected
	ErrSourceUnavailable   = core.ErrSourceUnavailable
	ErrSourceChanged       = core.ErrSourceChanged
	ErrSeekUnavailable     = core.ErrSeekUnavailable
	ErrUnreleased          = core.ErrUnreleased
)

// DownloadQueuedError wraps ErrDownloadQueued with optional provider queue context.
type DownloadQueuedError = core.DownloadQueuedError

// DownloadQueuedDetail returns the provider queue reason when err carries one, else "".
func DownloadQueuedDetail(err error) string {
	return core.DownloadQueuedDetail(err)
}

// PreparingError carries a suggested retry delay for ErrPreparing.
type PreparingError = core.PreparingError

// PreparingRetrySeconds returns the HTTP Retry-After value in whole seconds
// for an ErrPreparing error.
func PreparingRetrySeconds(err error, fallback int) int {
	return core.PreparingRetrySeconds(err, fallback)
}

// PreparingMessage returns the viewer-facing message for an ErrPreparing error.
func PreparingMessage(err error) string {
	return core.PreparingMessage(err)
}

// PreparingErrorBody returns the JSON media error envelope for preparing.
func PreparingErrorBody(err error) []byte {
	return core.PreparingErrorBody(err)
}

// JobsBusyError carries the refusal message and retry delay for ErrJobsBusy.
type JobsBusyError = core.JobsBusyError

// JobsBusyRetrySeconds returns the HTTP Retry-After value in whole seconds
// for an ErrJobsBusy error.
func JobsBusyRetrySeconds(err error, fallback int) int {
	return core.JobsBusyRetrySeconds(err, fallback)
}

// JobsBusyMessage returns the viewer-facing message for an ErrJobsBusy error.
func JobsBusyMessage(err error) string {
	return core.JobsBusyMessage(err)
}

// JobsBusyErrorBody returns the JSON media error envelope for jobs_busy.
func JobsBusyErrorBody(err error) []byte {
	return core.JobsBusyErrorBody(err)
}

// FilterOutSurroundAudio returns codecs without surround audio formats.
func FilterOutSurroundAudio(codecs []string) []string {
	return core.FilterOutSurroundAudio(codecs)
}

// NewHubContext derives the request context for a hubs call.
func NewHubContext(now time.Time, product, platform, device string) HubContext {
	return core.NewHubContext(now, product, platform, device)
}

// SectionView restricts a Home hub to one library section's media type.
func SectionView(h Hub, t MediaType) (Hub, bool) { return core.SectionView(h, t) }

// WithBrowseScope identifies a tokenless client's browse scope.
func WithBrowseScope(ctx context.Context, scope string) context.Context {
	return core.WithBrowseScope(ctx, scope)
}

// WithPlexSaferRating marks a request for the safer age-rating mapping.
func WithPlexSaferRating(ctx context.Context) context.Context {
	return core.WithPlexSaferRating(ctx)
}

// WithSearchCandidates attaches the request-scoped search candidate set.
func WithSearchCandidates(ctx context.Context, c *SearchCandidates) context.Context {
	return core.WithSearchCandidates(ctx, c)
}

// WithSearchListFacts marks a search rendered as list items.
func WithSearchListFacts(ctx context.Context) context.Context {
	return core.WithSearchListFacts(ctx)
}

// WithSearchClient attaches the client identifier for search-as-you-type single-flight.
func WithSearchClient(ctx context.Context, clientID string) context.Context {
	return core.WithSearchClient(ctx, clientID)
}

// WithSearchPending makes the search in ctx answer without waiting on
// remote lookups; the returned collector reports whether one was still
// running (core.WithSearchPending).
func WithSearchPending(ctx context.Context) (context.Context, *core.SearchPending) {
	return core.WithSearchPending(ctx)
}

// EnterSearch makes ctx the latest search of a viewer's client: a newer
// query from the same client cancels it (core.EnterSearch).
func EnterSearch(ctx context.Context, viewerID int64, client, query string) (context.Context, func()) {
	return core.EnterSearch(ctx, viewerID, client, query)
}

// EnterSearchGeneration is EnterSearch for a client that numbers its
// queries, so an older keystroke arriving late never supersedes a newer one.
func EnterSearchGeneration(ctx context.Context, viewerID int64, client, query string, generation int64) (context.Context, func()) {
	return core.EnterSearchGeneration(ctx, viewerID, client, query, generation)
}

// SearchWasSuperseded reports a search cancelled by a newer one from the
// same client (core.SearchWasSuperseded); it answers empty, not an error.
func SearchWasSuperseded(ctx context.Context) bool {
	return core.SearchWasSuperseded(ctx)
}

// WithExplicitSearch marks a request as a viewer or owner typing a search,
// so browsing-only catalogue rules do not filter out explicit search hits.
func WithExplicitSearch(ctx context.Context) context.Context {
	return core.WithExplicitSearch(ctx)
}

// ErrBedtime carries the next playback admission time.
type ErrBedtime = core.ErrBedtime

// WithLocalResponse requests an answer from local snapshots and caches,
// without searches or source opens.
func WithLocalResponse(ctx context.Context) context.Context {
	return core.WithLocalResponse(ctx)
}

// WithoutLocalResponse clears LocalResponse on ctx while preserving deadlines,
// cancellation and other request values.
func WithoutLocalResponse(ctx context.Context) context.Context {
	return core.WithoutLocalResponse(ctx)
}

// IsAutoVersionSelector reports whether selector denotes the Auto slot (empty
// or an encoded {"k":"auto"} configured selector).
func IsAutoVersionSelector(selector string) bool {
	return core.IsAutoVersionSelector(selector)
}

// IsExplicitVersionSlotSelector reports whether selector denotes a non-Auto
// configured version slot (as opposed to Auto or a legacy concrete release ID).
func IsExplicitVersionSlotSelector(selector string) bool {
	return core.IsExplicitVersionSlotSelector(selector)
}

const IndexerPurposePlayback = core.IndexerPurposePlayback

// WithIndexerPurpose tags ctx with the indexer usage purpose (for example
// IndexerPurposePlayback so explicit Play is never gated by discovery shares).
func WithIndexerPurpose(ctx context.Context, purpose string) context.Context {
	return core.WithIndexerPurpose(ctx, purpose)
}

// WithColdDetail identifies the single title page allowed a minimum cold fill.
func WithColdDetail(ctx context.Context, key RatingKey) context.Context {
	return core.WithColdDetail(ctx, key)
}

// ColdDetail reports whether key is the exact page allowed a cold fill.
func ColdDetail(ctx context.Context, key RatingKey) bool {
	return core.ColdDetail(ctx, key)
}

// WithNoContainerExpand suppresses plugin container-on-open expansion for
// list, artwork, Home, and background reads.
func WithNoContainerExpand(ctx context.Context) context.Context {
	return core.WithNoContainerExpand(ctx)
}

// NoContainerExpand reports whether plugin container-on-open expansion is
// suppressed on ctx.
func NoContainerExpand(ctx context.Context) bool {
	return core.NoContainerExpand(ctx)
}

// DetailPending is the exact-page enrichment snapshot.
type DetailPending = core.DetailPending

func WithDetailPending(ctx context.Context, key RatingKey) (context.Context, *DetailPending) {
	return core.WithDetailPending(ctx, key)
}

var IsWatchlistHub = core.IsWatchlistHub
var WatchlistHubAvailable = core.WatchlistHubAvailable
