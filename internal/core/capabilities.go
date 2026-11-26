package core

// These named extension contracts describe optional cross-component capabilities.
// Consumers may select a capability without inflating the base interfaces or
// hiding method signatures in call sites. Implementations retain their base
// contracts; capability absence has an explicit fallback at each consumer.

import (
	"context"
	"time"
)

// ActiveUserSource checks current server membership for delegated stream access.
type ActiveUserSource interface {
	ActiveUser(context.Context, int64) (*User, error)
}

// ArtworkAccessPolicy exposes the optional Restricted capability.
type ArtworkAccessPolicy interface{ Restricted(*User) bool }

// AvailabilitySource exposes the optional Available capability.
type AvailabilitySource interface {
	Available(context.Context, RatingKey) (bool, bool)
}

// BitrateAccessPolicy exposes the optional BitrateLimit capability.
type BitrateAccessPolicy interface{ BitrateLimit(*User) int }

// CachePlayRecorder exposes the optional RecordPlay capability.
type CachePlayRecorder interface{ RecordPlay(string, string, int64) }

// CachedTitleSource exposes the optional CachedTitles capability.
type CachedTitleSource interface {
	CachedTitles(context.Context, []RatingKey) ([]Title, error)
}

// ChannelPreparer exposes the optional PrepareChannel capability.
type ChannelPreparer interface {
	PrepareChannel(context.Context, *User, RatingKey, ClientCaps, string) (*PlaySession, error)
}

// CollectionArtworkSource exposes the optional CollectionArtwork capability.
type CollectionArtworkSource interface {
	CollectionArtwork(context.Context, *User, RatingKey, string, int) ([]byte, string, bool, error)
}

// CollectionLayoutSource exposes the optional LayoutRow capability.
type CollectionLayoutSource interface {
	LayoutRow(context.Context, *User, HubContext, string) (*CollectionRow, error)
}

// CollectionSpecSource exposes the optional ListSpecs capability.
type CollectionSpecSource interface {
	ListSpecs(context.Context) ([]CollectionSpec, error)
}

// ContextReaderAt reads source bytes with request cancellation.
type ContextReaderAt interface {
	ReadAtContext(context.Context, []byte, int64) (int, error)
}

// DamageReporter exposes the optional SetDamageHandler capability.
type DamageReporter interface{ SetDamageHandler(func(int64, int64) bool) }

// DirectPlayRetrier exposes the optional RetryDirectPlay capability.
type DirectPlayRetrier interface{ RetryDirectPlay() bool }

// DownloadAllowance is what one viewer may download: the owner's server-wide
// defaults with that viewer's overrides applied. LimitUnit is "" for no
// limit, "titles" or "gb"; LimitPeriod is "day" or "month".
type DownloadAllowance struct {
	LimitUnit   string
	LimitAmount int
	LimitPeriod string
	MaxHeight   int  // 0 = the server's download resolution cap only
	NoTranscode bool // downloads come only from a release needing no video re-encode
}

// DownloadLimitDecision reports whether a viewer's download is admitted and,
// when refused, which limit was reached and when it resets (UTC).
// Reserved is true when AdmitDownload inserted a fresh usage row for this
// attempt so the caller should release it if the download fails or is canceled
// before completing.
type DownloadLimitDecision struct {
	Allowed  bool
	Reserved bool
	Unit     string
	Period   string
	ResetAt  time.Time
}

// DownloadLimitPolicy exposes the optional per-viewer download limits.
// AdmitDownload reserves estimatedBytes against the viewer's usage; callers
// reconcile with ReconcileDownloadBytes once the download size is known, update
// to the finished size with RecordDownloadBytes, and call
// ReleaseDownloadReservation if a newly reserved download fails or is canceled
// before completing.
type DownloadLimitPolicy interface {
	DownloadAllowance(*User) DownloadAllowance
	AdmitDownload(ctx context.Context, u *User, key RatingKey, estimatedBytes int64) DownloadLimitDecision
	ReconcileDownloadBytes(ctx context.Context, u *User, key RatingKey, actualBytes int64) DownloadLimitDecision
	RecordDownloadBytes(ctx context.Context, u *User, key RatingKey, bytes int64)
	ReleaseDownloadReservation(ctx context.Context, u *User, key RatingKey)
}

// OfflineDownloadBinding identifies bytes already admitted for an offline grant.
type OfflineDownloadBinding struct {
	Release, VersionID, ETag string
}

// DownloadAccessPolicy exposes the optional AllowDownloads capability.
type DownloadAccessPolicy interface{ AllowDownloads(*User) bool }

// DurablePrefetcher exposes the optional DurablePrefetch capability.
type DurablePrefetcher interface {
	DurablePrefetch(context.Context, string, []byte, int64, int64) error
}

// HomeCandidateSource exposes the optional HomeCandidatesFor capability.
type HomeCandidateSource interface {
	HomeCandidatesFor(context.Context, *User, HubContext, int) ([]Hub, error)
}

// PolicyHomeCandidateSource filters a viewer's candidate pool before ranking
// and row limits. The caller supplies a fail-closed visibility predicate.
type PolicyHomeCandidateSource interface {
	HomeCandidatesWithPolicy(context.Context, *User, HubContext, int, func(context.Context, *User, *Title) bool) ([]Hub, error)
	SectionCandidatesWithPolicy(context.Context, *User, MediaType, HubContext, int, func(context.Context, *User, *Title) bool) ([]Hub, error)
}

// HubItemSource exposes the optional HubItemsFor capability for resolving a
// specific hub by identifier even after a model or window transition.
type HubItemSource interface {
	HubItemsFor(context.Context, *User, HubContext, string, int) (Hub, bool)
}

// HomeLayoutSource exposes the optional FinalizeHome capability.
type HomeLayoutSource interface {
	FinalizeHome(context.Context, *User, HubContext, string, []Hub) ([]Hub, error)
}

// MetadataReaderAt reads container metadata without advancing playback or read-ahead.
type MetadataReaderAt interface {
	ReadMetadataAtContext(context.Context, []byte, int64) (int, error)
}

// PlaybackAccessPolicy exposes the optional CanPlay capability.
type PlaybackAccessPolicy interface {
	CanPlay(*User, bool, time.Time) bool
}

// PlaybackDurationSetter exposes the optional SetPlaybackDuration capability:
// playback reports a probed title's running time so the source can read ahead
// of the viewer by time (minutes of media) rather than by bytes.
type PlaybackDurationSetter interface{ SetPlaybackDuration(time.Duration) }

// PlaybackPosition exposes the optional RequestedPosition capability.
type PlaybackPosition interface{ RequestedPosition() time.Duration }

// BufferedSource reports contiguous bytes downloaded ahead of the reader.
type BufferedSource interface{ BufferedAhead() int64 }

// SourceDelivery accounts decoded article-fetch completions owned by this source,
// including demand, near read-ahead and disk play-ahead. Engine cache hits and
// joined engine flights do not count. Fetcher-level coalescing can share wire work;
// these bytes must not be summed across sources as unique provider traffic.
// Difference Bytes across snapshots and divide by elapsed wall time for delivery.
// Busy excludes idle gaps, includes queue wait, and is diagnostic only.
type SourceDelivery struct {
	Bytes   int64         // decoded bytes fetched from the network
	Fetches int64         // completed network fetches
	Busy    time.Duration // wall time with at least one fetch outstanding
	// Diagnostics of the moment.
	InFlight      int     // fetches outstanding now, including source/pool queue wait
	Parallel      int     // shared speculative fetch budget (not sockets)
	PipelineDepth int     // normal (non-burst) read-ahead BODY depth ceiling
	PerFetchBps   float64 // recent fetch completion rate including queue wait
}

// SourceDeliveryReporter exposes the optional SourceDelivery capability.
type SourceDeliveryReporter interface{ SourceDelivery() SourceDelivery }

// PlaybackPreparer warms a title without reserving playback or changing history.
type PlaybackPreparer interface {
	PreResolve(context.Context, *User, RatingKey, ClientCaps) bool
}

// PrefetchReaderAt reads speculative bytes at background priority.
type PrefetchReaderAt interface {
	ReadAtPrefetchContext(context.Context, []byte, int64) (int, error)
}

// ProcessCPUTime exposes the optional CPUTime capability.
type ProcessCPUTime interface {
	CPUTime() (time.Duration, time.Duration)
}

// ProcessIdentity exposes the optional PID capability.
type ProcessIdentity interface{ PID() int }

// ProcessPeakMemory exposes the optional PeakRSSBytes capability.
type ProcessPeakMemory interface{ PeakRSSBytes() (uint64, bool) }

// ReleaseSizeSource exposes the optional ReleaseSizes capability.
type ReleaseSizeSource interface{ ReleaseSizes() map[string]int64 }

// RescueIndexer exposes the optional SearchRescue capability.
type RescueIndexer interface {
	SearchRescue(context.Context, SearchRequest) ([]Release, error)
}

// SectionCandidateSource exposes the optional SectionCandidatesFor capability.
type SectionCandidateSource interface {
	SectionCandidatesFor(context.Context, *User, MediaType, HubContext, int) ([]Hub, error)
}

// SegmentCacheLimit exposes the optional UsageLimit capability: the most the
// segment cache may hold right now, the budget or the free-space floor's cap
// when lower. -1 means unbounded and 0 means media bytes are not retained.
type SegmentCacheLimit interface{ UsageLimit() int64 }

// SegmentPresence exposes the optional Has capability.
type SegmentPresence interface{ Has(string, int) bool }

// StreamingIndexer exposes the optional SearchWith capability.
type StreamingIndexer interface {
	SearchWith(context.Context, SearchRequest, func([]Release)) ([]Release, error)
}

// StreamingRescueIndexer exposes the optional SearchRescueWith capability.
type StreamingRescueIndexer interface {
	SearchRescueWith(context.Context, SearchRequest, func([]Release)) ([]Release, error)
}

// IndexerSearchOptions configures fan-out filtering, early return, and wait
// budgets for ControlledSearchIndexer.SearchWithOptions. The zero value matches
// Indexer.Search: query every enabled indexer and wait for all of them.
type IndexerSearchOptions struct {
	// FirstResults returns early once the fastest indexer(s) produce at least
	// one result plus Settle (default 250 ms when SettleSet is false; 0
	// returns immediately), while remaining indexers keep running in the
	// background to populate Ether's search cache.
	FirstResults bool
	// Settle sets the settle window after the first results arrive (under
	// FirstResults) or after MinResults is met (when FirstResults is false and
	// SettleSet is true).
	Settle    time.Duration
	SettleSet bool
	// MinResults sets a minimum deduplicated result count for early return.
	// When combined with FirstResults, returns as soon as either MinResults is
	// reached or the settle window after the first results elapses.
	MinResults int
	// SkipIndexers excludes matching enabled indexers by id or name
	// (case-insensitive, trimmed).
	SkipIndexers []string
	// OnlyIndexers, when non-empty, restricts the search to matching enabled
	// indexers by id or name (case-insensitive, trimmed). SkipIndexers still
	// removes entries from the OnlyIndexers set.
	OnlyIndexers []string
	// WaitTimeout bounds how long the caller waits before returning whatever
	// arrived so far. When shorter than BackgroundTimeout, unfinished indexers
	// are reported as pending while continuing to fill Ether's search cache;
	// when at or above BackgroundTimeout, unfinished indexers hit the ceiling
	// and are reported as timed_out. Zero waits for ctx.
	WaitTimeout time.Duration
	// BackgroundTimeout caps how long detached background indexers may continue
	// running after an early return or WaitTimeout. Zero uses the indexer
	// manager's default ceiling (30 s).
	BackgroundTimeout time.Duration
	// ContinueSearch receives a snapshot function when pending indexers are
	// left running. Snapshots include results from the same fan-out as they
	// arrive. ContinuationContext, when set, bounds that work's lifetime.
	ContinueSearch      func(snapshot func(context.Context) IndexerSearchResult)
	ContinuationContext context.Context
}

// IndexerSearchResult carries merged, deduplicated releases alongside
// per-indexer outcomes (and any still-running pending indexers) for every
// enabled indexer in priority order.
type IndexerSearchResult struct {
	Releases []Release
	Indexers []IndexerSearchStatus
	Pending  []IndexerRef
}

// ControlledSearchIndexer exposes controlled indexer_search fan-out options
// (firstResults, settle, minResults, skip/only filters, wait timeout) and
// per-indexer outcomes. Implemented by *newznab.Manager and forwarded by
// indexer wrapper layers.
type ControlledSearchIndexer interface {
	SearchWithOptions(ctx context.Context, req SearchRequest, opts IndexerSearchOptions) (IndexerSearchResult, error)
}

// IndexerDirectory lists the owner's currently enabled indexers (without
// secrets, API keys, URLs, or cookies) for status envelopes and the
// sources.resolve({kind: "indexers"}) listing.
type IndexerDirectory interface {
	EnabledIndexerStatuses(status string) []IndexerSearchStatus
	EnabledIndexers() []IndexerSearchStatus
}

// TrickplayCanceler exposes the optional CancelTrickplay capability.
type TrickplayCanceler interface{ CancelTrickplay(string) }

// UserPolicySource exposes the optional UserPolicy capability.
type UserPolicySource interface{ UserPolicy(*User) *User }

// ViewerInvalidator exposes the optional Invalidate capability.
type ViewerInvalidator interface{ Invalidate(userID int64) }

// SourceDeliveryObserver receives uncached source throughput and stall evidence.
// It is neither client bandwidth nor a claim about an entire provider network.
type SourceDeliveryObserver interface {
	ObserveSourceDelivery(Release, float64)
	ObserveSourceStall(Release, float64)
}

// ConcurrentRangeReaderAt keeps active demuxer range requests independent.
// A container reader can overlap header, cue and media ranges while a prior
// HTTP response unwinds. Request cancellation still retires its urgent work;
// unrelated seeks must only cancel speculative read-ahead.
type ConcurrentRangeReaderAt interface {
	ReadAtConcurrentContext(context.Context, []byte, int64) (int, error)
}

// AnalysisReaderAt is an explicitly admitted background analysis read. It keeps
// prefetch priority while allowing provider-account pacing and viewer yielding.
type AnalysisReaderAt interface {
	ReadAtAnalysisContext(context.Context, []byte, int64) (int, error)
}

// AnalysisSourceStats reports only analysis provider requests, bytes and errors.
type AnalysisSourceStats interface{ AnalysisStats() (int64, int64, int64) }
