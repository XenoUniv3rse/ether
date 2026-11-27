package etherplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Content-kind contract (Ether v0.3.8): a plugin declares media kinds in
// its manifest and answers /ether/v1/kinds/{kind}/* routes. These types
// mirror the host's wire shapes exactly; the shared fixtures under
// sdk/conformance/kinds pin all three SDKs to the same JSON.

// ItemUnavailable explains why an item cannot currently be played.
type ItemUnavailable struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// KindItem is the standardised plugin item envelope. Ether assigns the
// stored identity; the plugin only ever sees its own external ID.
type KindItem struct {
	Unavailable *ItemUnavailable           `json:"unavailable,omitempty"`
	ID          string                     `json:"id"`
	Level       string                     `json:"level"`
	ParentID    string                     `json:"parentId,omitempty"`
	Index       int                        `json:"index,omitempty"`
	Title       string                     `json:"title"`
	SortTitle   string                     `json:"sortTitle,omitempty"`
	Summary     string                     `json:"summary,omitempty"`
	Year        int                        `json:"year,omitempty"`
	Released    int64                      `json:"released,omitempty"` // unix seconds
	Rating      float64                    `json:"rating,omitempty"`
	Playable    bool                       `json:"playable,omitempty"`
	ChildCount  int                        `json:"childCount,omitempty"`
	LeafCount   int                        `json:"leafCount,omitempty"`
	Art         []KindArtRef               `json:"art,omitempty"`
	Fields      map[string]json.RawMessage `json:"fields,omitempty"`
	Hints       json.RawMessage            `json:"hints,omitempty"` // opaque JSON object, echoed to the resolver
	// Recording: the work this item is a copy of, the same on every release
	// that carries it (music: "isrc:<ISRC>"). Apps group duplicates by it.
	Recording string `json:"recording,omitempty"`
	// Members: a list item's ordered children (external ids of other items of
	// the kind, at most 500). Only on a level declared members: true.
	Members []string `json:"members,omitempty"`
	ETag    string   `json:"etag,omitempty"`
}

// KindItemPart describes one declared or resolve-time file/part on a multi-file
// KindItem (#576). Ether passes the zero-based index back in
// KindResolveRequest.PartIndex when resolving each part, or targets FileIndex
// inside a resolved multi-file release.
type KindItemPart struct {
	Index       int     `json:"index"`
	Name        string  `json:"name,omitempty"`
	MIME        string  `json:"mime,omitempty"`
	Size        int64   `json:"size,omitempty"`
	DurationSec float64 `json:"durationSec,omitempty"`
	FileIndex   *int    `json:"fileIndex,omitempty"`
}

// KindArtRef is one artwork reference on a KindItem.
type KindArtRef struct {
	Role   string `json:"role"`
	URL    string `json:"url"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// KindPage is one page of kind items; an empty NextCursor ends the walk.
type KindPage struct {
	Items      []KindItem `json:"items"`
	NextCursor string     `json:"nextCursor,omitempty"`
	Total      *int       `json:"total,omitempty"`
}

// KindBrowseFacetAlt is one {field, values} alternative inside a cross-field
// KindBrowseFacetGroup (#807).
type KindBrowseFacetAlt struct {
	Field  string   `json:"field"`
	Values []string `json:"values"`
}

// KindBrowseFacetGroup is one OR-group for KindBrowseRequest.AnyGroups (#767, #807).
// A group either names a single {field, values} OR-list or carries a list of
// {field, values} alternatives in Any that are ORed together across fields
// (and ANDed with the rest). Multiple groups — including repeated groups on
// the same field — combine with AND alongside Facets, AllFacets,
// ExcludeFacets, Empty, and ExcludeQuery.
type KindBrowseFacetGroup struct {
	Field  string               `json:"field,omitempty"`
	Values []string             `json:"values,omitempty"`
	Any    []KindBrowseFacetAlt `json:"any,omitempty"`
}

// KindBrowseRequest is Ether's browse/search request for one kind level.
type KindBrowseRequest struct {
	Kind     string `json:"-"` // filled from the route, never on the wire
	ParentID string `json:"parentId,omitempty"`
	Level    string `json:"level,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Sort     string `json:"sort,omitempty"`
	Desc     bool   `json:"desc,omitempty"`
	// Facets requires any of the listed values per field; AllFacets
	// requires every listed value, ExcludeFacets drops items having any
	// listed value, and AnyGroups adds one OR-group per entry — either a
	// single {field, values} list or {any: [{field, values}, ...]} ORed
	// across fields (#767, #807). All four combine with AND.
	Facets        map[string][]string    `json:"facets,omitempty"`
	AllFacets     map[string][]string    `json:"allFacets,omitempty"`
	ExcludeFacets map[string][]string    `json:"excludeFacets,omitempty"`
	AnyGroups     []KindBrowseFacetGroup `json:"anyGroups,omitempty"`
	// Empty tests field presence or absence per field key (#807): true
	// keeps items with no value in the field, false keeps items with at
	// least one. Counts toward neither value bound; unknown fields are
	// rejected.
	Empty map[string]bool `json:"empty,omitempty"`
	Query string          `json:"query,omitempty"`
	// ExcludeQuery drops items whose indexed text matches, using the same
	// text matching as Query (#807).
	ExcludeQuery string `json:"excludeQuery,omitempty"`
	// Total controls whether kinds.browse computes KindPage.Total:
	// "" or "cheap" (default) returns Total only when O(1), "none" omits it,
	// and "exact" computes an exact Total (cached per library generation).
	Total string `json:"total,omitempty"`
}

// KindBrowseQuery is the query passed to kinds.browse from a frontend adapter (#615).
type KindBrowseQuery = KindBrowseRequest

// KindExpandRequest asks a plugin to expand one declared facet bucket,
// plugin person slice, or container's children via POST /ether/v1/kinds/{kind}/expand.
type KindExpandRequest struct {
	Kind     string `json:"-"`
	Facet    string `json:"facet,omitempty"`
	Value    string `json:"value,omitempty"`
	ValueID  int64  `json:"valueId,omitempty"`
	ParentID string `json:"parentId,omitempty"`
	Level    string `json:"level,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
}

// KindExpandResponse is returned by POST /ether/v1/kinds/{kind}/expand.
// Either Items (with optional Cursor/NextCursor for further pages) or Job
// ("/ether/job/...") is populated.
type KindExpandResponse struct {
	Items      []KindItem `json:"items,omitempty"`
	Cursor     string     `json:"cursor,omitempty"`
	NextCursor string     `json:"nextCursor,omitempty"`
	Job        string     `json:"job,omitempty"`
}

// KindChangePage is one delta page for pull sync.
type KindChangePage struct {
	Upserts    []KindItem `json:"upserts,omitempty"`
	Deletes    []string   `json:"deletes,omitempty"`
	NextCursor string     `json:"nextCursor,omitempty"`
	Complete   bool       `json:"complete"`
}

// KindChangesRequest is Ether's delta-feed request.
type KindChangesRequest struct {
	Kind   string `json:"-"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// SourceQuery asks Ether to find sources (Host.ResolveSources).
type SourceQuery struct {
	Kind  string `json:"kind"` // indexer_search|indexer_search_results|indexer_search_cancel|indexers|debrid|debrid_check|debrid_acquire|release_files|usenet_check|nzb_request
	Query string `json:"query,omitempty"`
	// Category accepts comma-separated Newznab IDs or standard family names;
	// when set with an empty Query, indexer_search requests recent releases.
	// Source plugins that declared the category answer it too.
	Category string `json:"category,omitempty"`
	// Lookup (indexer_search with Category) adds a typed lookup, such as an
	// album by artist and title, for source plugins that declared its type.
	// Other indexers search Query alone.
	Lookup *SourceLookup `json:"lookup,omitempty"`
	// Profile names the quality profile to score video release names against
	// on indexer_search; empty uses the owner's default profile.
	Profile string `json:"profile,omitempty"`
	// Stream (indexer_search only), when true, starts the fan-out and returns
	// immediately with a SearchID handle to poll via indexer_search_results or
	// stop via indexer_search_cancel (#733).
	Stream bool `json:"stream,omitempty"`
	// SearchID identifies a streaming search started by indexer_search with
	// Stream: true; required on indexer_search_results and indexer_search_cancel.
	SearchID string `json:"searchId,omitempty"`
	// WaitMS (indexer_search_results only) sets how long in milliseconds to
	// wait for at least one new arrival or search completion (capped at
	// 10000 ms; 0 returns immediately).
	WaitMS int `json:"waitMs,omitempty"`
	// FirstResults (indexer_search only), when true, returns as soon as the
	// fastest indexers produce results plus SettleMS (default 250 ms), while
	// slower indexers keep populating Ether's search cache in the background (#733).
	FirstResults bool `json:"firstResults,omitempty"`
	// SettleMS (indexer_search only) sets the post-first-result grace window in
	// milliseconds (nil uses 250 ms when FirstResults is true; 0 returns
	// immediately). Also applies when MinResults is met if explicitly set.
	// Capped at 30000 ms.
	SettleMS *int `json:"settleMs,omitempty"`
	// MinResults (indexer_search only) is the minimum deduplicated candidate
	// count required before returning early.
	MinResults int `json:"minResults,omitempty"`
	// SkipIndexers (indexer_search only) excludes matching indexer IDs or
	// display names (case-insensitive). Precedence over OnlyIndexers.
	SkipIndexers []string `json:"skipIndexers,omitempty"`
	// OnlyIndexers (indexer_search only) restricts the search to matching
	// indexer IDs or display names (case-insensitive).
	OnlyIndexers []string `json:"onlyIndexers,omitempty"`
	// TimeoutMS (indexer_search only) caps how long the call waits in
	// milliseconds, bounded by Ether's 30000 ms ceiling (and any shorter parent
	// request deadline). When shorter than the ceiling, slow indexers still
	// running in the background are reported as pending (and listed in Pending);
	// indexers that hit the ceiling are reported as timed_out.
	TimeoutMS  int      `json:"timeoutMs,omitempty"`
	InfoHashes []string `json:"infoHashes,omitempty"`
	InfoHash   string   `json:"infoHash,omitempty"`
	Magnet     string   `json:"magnet,omitempty"`
	// Deprecated: use TorrentRef for an Ether torrent descriptor reference.
	TorrentURL string `json:"torrentUrl,omitempty"`
	// Deprecated: use NZBRef. NZBURL also accepts legacy download links.
	NZBURL      string       `json:"nzbUrl,omitempty"`
	NZBRef      string       `json:"nzbRef,omitempty"`
	TorrentRef  string       `json:"torrentRef,omitempty"`
	NZBIdentity *NZBIdentity `json:"nzbIdentity,omitempty"`
	// Candidate carries an indexer_search candidate back for release_files
	// or usenet_check.
	Candidate *SourceCandidate `json:"candidate,omitempty"`
	// FileCount optionally hints how many files a torrent contains when passing
	// a magnet or infoHash without a file list (e.g. large collection torrents
	// held as zip-only caches on TorBox).
	FileCount int `json:"fileCount,omitempty"`
}

// Per-indexer search outcome status values reported in IndexerSearchStatus.Status.
const (
	IndexerStatusAnswered = "answered"
	IndexerStatusPending  = "pending"
	IndexerStatusTimedOut = "timed_out"
	IndexerStatusSkipped  = "skipped"
	IndexerStatusError    = "error"
)

// Reason values for IndexerSearchStatus.Reason when Status is IndexerStatusSkipped.
const (
	// IndexerSkipReasonPaused reports an indexer skipped because its circuit
	// breaker paused it in backoff after repeated failures or timeouts.
	IndexerSkipReasonPaused = "paused_backoff"
)

// IndexerRef identifies one enabled indexer by ID and display name in the
// top-level Pending list on early-returning and streaming search responses.
type IndexerRef struct {
	IndexerID string `json:"indexerId"`
	Indexer   string `json:"indexer"`
}

// IndexerSearchStatus reports either one enabled indexer's outcome during an
// indexer_search / indexer_search_results / indexer_search_cancel call (Status,
// Results, MS, Reason, Error) or an enabled indexer summary on kind:"indexers"
// (Kind, MedianMS, LastMS, LastError) (#733).
type IndexerSearchStatus struct {
	IndexerID string `json:"indexerId"`
	Indexer   string `json:"indexer"`
	Status    string `json:"status,omitempty"` // answered|pending|timed_out|skipped|error
	Results   int    `json:"results,omitempty"`
	MS        int64  `json:"ms"`
	Reason    string `json:"reason,omitempty"` // populated on automatic skips (for example paused_backoff)
	Error     string `json:"error,omitempty"`
	Kind      string `json:"kind,omitempty"` // newznab|torznab|builtin|stremio|zilean|addon
	MedianMS  *int64 `json:"medianMs,omitempty"`
	LastMS    *int64 `json:"lastMs,omitempty"`
	LastError string `json:"lastError,omitempty"`
}

// MarshalJSON preserves ms:0 on search status entries (Status != "") while
// omitting status/results/ms/reason/error on kind:"indexers" entries (Status == "").
func (s IndexerSearchStatus) MarshalJSON() ([]byte, error) {
	if s.Status == "" {
		type listing struct {
			IndexerID string `json:"indexerId"`
			Indexer   string `json:"indexer"`
			Kind      string `json:"kind,omitempty"`
			MedianMS  *int64 `json:"medianMs,omitempty"`
			LastMS    *int64 `json:"lastMs,omitempty"`
			LastError string `json:"lastError,omitempty"`
		}
		return json.Marshal(listing{
			IndexerID: s.IndexerID,
			Indexer:   s.Indexer,
			Kind:      s.Kind,
			MedianMS:  s.MedianMS,
			LastMS:    s.LastMS,
			LastError: s.LastError,
		})
	}
	type searchStatus struct {
		IndexerID string `json:"indexerId"`
		Indexer   string `json:"indexer"`
		Status    string `json:"status"`
		Results   int    `json:"results,omitempty"`
		MS        int64  `json:"ms"`
		Reason    string `json:"reason,omitempty"`
		Error     string `json:"error,omitempty"`
		Kind      string `json:"kind,omitempty"`
		MedianMS  *int64 `json:"medianMs,omitempty"`
		LastMS    *int64 `json:"lastMs,omitempty"`
		LastError string `json:"lastError,omitempty"`
	}
	return json.Marshal(searchStatus{
		IndexerID: s.IndexerID,
		Indexer:   s.Indexer,
		Status:    s.Status,
		Results:   s.Results,
		MS:        s.MS,
		Reason:    s.Reason,
		Error:     s.Error,
		Kind:      s.Kind,
		MedianMS:  s.MedianMS,
		LastMS:    s.LastMS,
		LastError: s.LastError,
	})
}

// SourceResolveResult is the full response envelope from sources.resolve (#733).
type SourceResolveResult struct {
	// Enabled is the owner's debrid availability (kind "debrid").
	Enabled *bool `json:"enabled,omitempty"`
	// CachedOnly is the owner policy (kinds "debrid" and "debrid_check").
	CachedOnly *bool                 `json:"cachedOnly,omitempty"`
	Candidates []SourceCandidate     `json:"candidates,omitempty"`
	Indexers   []IndexerSearchStatus `json:"indexers,omitempty"`
	Pending    []IndexerRef          `json:"pending,omitempty"`
	SearchID   string                `json:"searchId,omitempty"`
	Done       *bool                 `json:"done,omitempty"`
}

// MarshalJSON emits the debrid policy or the candidate response envelope.
func (r SourceResolveResult) MarshalJSON() ([]byte, error) {
	if r.Enabled != nil {
		return json.Marshal(struct {
			Enabled    bool  `json:"enabled"`
			CachedOnly *bool `json:"cachedOnly"`
		}{Enabled: *r.Enabled, CachedOnly: r.CachedOnly})
	}
	cands := r.Candidates
	if cands == nil {
		cands = []SourceCandidate{}
	}
	wire := struct {
		Candidates []SourceCandidate      `json:"candidates"`
		CachedOnly *bool                  `json:"cachedOnly,omitempty"`
		Indexers   *[]IndexerSearchStatus `json:"indexers,omitempty"`
		Pending    *[]IndexerRef          `json:"pending,omitempty"`
		SearchID   string                 `json:"searchId,omitempty"`
		Done       *bool                  `json:"done,omitempty"`
	}{
		Candidates: cands,
		CachedOnly: r.CachedOnly,
		SearchID:   r.SearchID,
		Done:       r.Done,
	}
	if r.Indexers != nil {
		s := r.Indexers
		wire.Indexers = &s
	}
	if r.Pending != nil {
		s := r.Pending
		wire.Pending = &s
	}
	return json.Marshal(wire)
}

// ReleaseMember is one non-directory file inside an archive (RAR, 7z, ZIP)
// returned by a release_files source query.
type ReleaseMember struct {
	Index  int    `json:"index"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Stored bool   `json:"stored,omitempty"`
}

// UsenetCheckResult reports the sampled article availability verdict for one
// Usenet NZB release (usenet_check).
type UsenetCheckResult struct {
	Available bool `json:"available"`
	Checked   int  `json:"checked"`
	Missing   int  `json:"missing"`
	// Cached is true when this verdict came from Ether's bounded per-NZB
	// cache (including a recorded playback failure) rather than a fresh
	// NNTP check on this call.
	Cached bool `json:"cached,omitempty"`
	// PlaybackFailed is true when a prior real playback attempt against this
	// NZB failed with an article or release error.
	PlaybackFailed bool `json:"playbackFailed,omitempty"`
	// Reason carries a short diagnostic code when Available is false.
	Reason string `json:"reason,omitempty"`
	// CheckedAt is the Unix timestamp (seconds) when this verdict or
	// playback failure was recorded.
	CheckedAt int64 `json:"checkedAt,omitempty"`
}

// ReleaseFile is one file inside a candidate release (release_files).
type ReleaseFile struct {
	Index int    `json:"index"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	// SizeApproximate is true when Size is a segment-derived decoded estimate
	// rather than an exact byte length (e.g. an NZB without readable PAR2/yEnc
	// headers). Exact sizes leave it false/omitted.
	SizeApproximate bool            `json:"sizeApproximate,omitempty"`
	Members         []ReleaseMember `json:"members,omitempty"`
}

// DebridServiceRef identifies one owner-configured debrid service (without
// exposing credentials) that holds a cached torrent or served an acquisition.
type DebridServiceRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// NZBIdentity identifies a release by its native configured indexer and GUID.
type NZBIdentity struct {
	IndexerID string `json:"indexerId"`
	GUID      string `json:"guid"`
}

// SourceCandidate is one Ether-found release for a resolver to rank.
// NZBRef and TorrentRef are opaque references returned by indexer search.
type SourceCandidate struct {
	InfoHash string `json:"infoHash,omitempty"`
	Magnet   string `json:"magnet,omitempty"`
	// Deprecated: TorrentURL carries an opaque ether-torrent: reference. Use TorrentRef.
	TorrentURL string `json:"torrentUrl,omitempty"`
	// Deprecated: NZBURL carries an opaque ether-nzb: reference. Use NZBRef.
	NZBURL     string `json:"nzbUrl,omitempty"`
	NZBRef     string `json:"nzbRef,omitempty"`
	TorrentRef string `json:"torrentRef,omitempty"`
	// FileIndex selects a zero-based entry in the torrent provider file list; nil uses video selection.
	FileIndex *int   `json:"fileIndex,omitempty"`
	Title     string `json:"title,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Seeders   int    `json:"seeders,omitempty"`
	Cached    bool   `json:"cached,omitempty"`
	// CachedBy lists the configured debrid services holding this torrent in
	// acquisition order (or the service that served debrid_acquire); omitted when uncached.
	CachedBy  []DebridServiceRef `json:"cachedBy,omitempty"`
	Indexer   string             `json:"indexer,omitempty"`
	IndexerID string             `json:"indexerId,omitempty"`
	// Score, Resolution, Source and Codec are populated on indexer_search
	// candidates whose release name identifies video; non-video candidates
	// leave them empty.
	Score      *int   `json:"score,omitempty"`
	Resolution int    `json:"resolution,omitempty"`
	Source     string `json:"source,omitempty"`
	Codec      string `json:"codec,omitempty"`
	// Files is populated by release_files or nzb_request.
	Files []ReleaseFile `json:"files,omitempty"`
	// FileCount is the torrent's known file count when reported by an indexer
	// or release_files query.
	FileCount int `json:"fileCount,omitempty"`
	// UsenetCheck is populated only by a usenet_check query.
	UsenetCheck *UsenetCheckResult `json:"usenetCheck,omitempty"`
	// PluginRelease names a release a source plugin answered for a declared
	// category search. It has no infoHash or nzbUrl: hand the candidate back
	// to release_files to list its files and in a release source to open it.
	PluginRelease *PluginReleaseRef `json:"pluginRelease,omitempty"`

	// Ref names this candidate in match scores. Ether derives it from the
	// infohash/NZB when empty; matchers echo it verbatim.
	Ref string `json:"-"`
}

// CandidateRef returns the stable ref matchers report for a candidate.
func (c SourceCandidate) CandidateRef() string {
	switch {
	case c.Ref != "":
		return c.Ref
	case c.InfoHash != "":
		return "infohash:" + strings.ToLower(c.InfoHash)
	case c.Magnet != "":
		return "magnet:" + c.Magnet
	case c.TorrentRef != "":
		return "torrentref:" + c.TorrentRef
	case c.NZBRef != "":
		return "nzbref:" + c.NZBRef
	case c.TorrentURL != "":
		return "torrent:" + c.TorrentURL
	case c.NZBURL != "":
		return "nzb:" + c.NZBURL
	case c.PluginRelease != nil:
		return "plugin:" + c.PluginRelease.Plugin + ":" + c.PluginRelease.ID
	default:
		return c.Title
	}
}

// PluginReleaseRef identifies a source plugin's release: the plugin and the
// lazy stream id it answered the search with.
type PluginReleaseRef struct {
	Plugin string `json:"plugin"`
	ID     string `json:"id"`
}

// KindMatchRequest asks a matcher to score candidates for one item.
type KindMatchRequest struct {
	Kind       string            `json:"-"`
	Item       KindItem          `json:"item"`
	Candidates []SourceCandidate `json:"candidates"`
	Settings   map[string]any    `json:"settings,omitempty"`
}

// KindMatchScore is one candidate score (0..1) with plain-language reasons.
type KindMatchScore struct {
	CandidateRef string   `json:"candidateRef"`
	Score        float64  `json:"score"`
	Reasons      []string `json:"reasons,omitempty"`
}

// KindMatchResponse is a matcher's answer, best first.
type KindMatchResponse struct {
	Pending   bool             `json:"pending,omitempty"`
	RetryInMS int64            `json:"retryInMs,omitempty"`
	Job       string           `json:"job,omitempty"`
	Matches   []KindMatchScore `json:"matches"`
}

// PlayableSource is one candidate way to acquire an item's bytes. URL may
// carry {itemIndex}, {pageNumber}, {byteStart}, {byteEnd} and {byteRange}
// placeholders that Ether substitutes.
type PlayableSource struct {
	Path    string `json:"path,omitempty"`
	ID      string `json:"id"`
	Label   string `json:"label,omitempty"`
	Pattern string `json:"pattern"` // range|download|direct|container|release|file
	MIME    string `json:"mime"`
	// Tier optionally marks interchangeable sources of the same quality tier.
	// Sources sharing a non-nil Tier may be reordered by Ether's source routing
	// (auto/usenet/debrid, measured startup, and stall demotion) and may
	// overtake each other after the ranked open grace period. Distinct tiers
	// keep the plugin's tier order; omitting Tier keeps exact plugin order.
	Tier *int  `json:"tier,omitempty"`
	Size int64 `json:"size,omitempty"`
	// SizeApproximate marks Size as an approximate hint (for example from a
	// Usenet release_files estimate) rather than an exact byte count.
	SizeApproximate bool    `json:"sizeApproximate,omitempty"`
	DurationSec     float64 `json:"durationSec,omitempty"`
	// DurationToleranceSec opts into open-time container duration verification
	// when DurationSec > 0: Ether reads the container duration (reusing the
	// durable release_probes cache) and refuses the source as wrong content
	// when the observed duration differs from DurationSec by more than this
	// tolerance, trying the next candidate and flagging the plugin answer.
	DurationToleranceSec  float64           `json:"durationToleranceSec,omitempty"`
	URL                   string            `json:"url,omitempty"`
	Headers               map[string]string `json:"headers,omitempty"`
	KeepHeadersOnRedirect []string          `json:"keepHeadersOnRedirect,omitempty"`
	BlobRef               string            `json:"blobRef,omitempty"`
	SHA256                string            `json:"sha256,omitempty"`
	// SHA1 and CRC32 are hex digests checked against the served bytes,
	// including a release's selected file or container member; Size too.
	SHA1      string         `json:"sha1,omitempty"`
	CRC32     string         `json:"crc32,omitempty"`
	MaxBytes  int64          `json:"maxBytes,omitempty"`
	ExpiresAt int64          `json:"expiresAt,omitempty"`
	Container *ContainerRef  `json:"container,omitempty"`
	Release   *ReleaseHint   `json:"release,omitempty"`
	Parts     []KindItemPart `json:"parts,omitempty"`
}

// MediaProbeResult is the container metadata summary returned by media.probe.
type MediaProbeResult struct {
	DurationSec float64 `json:"durationSec"`
	Container   string  `json:"container"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	SizeBytes   int64   `json:"sizeBytes"`
}

// ContainerRef describes an archive member inside ordered volumes.
type ContainerRef struct {
	Format         string      `json:"format,omitempty"` // "" (sniff)|rar|7z|zip
	Volumes        []VolumeRef `json:"volumes"`
	Member         string      `json:"member,omitempty"`
	MemberIndex    *int        `json:"memberIndex,omitempty"`
	PasswordSecret string      `json:"passwordSecret,omitempty"`
}

// VolumeRef locates one archive volume by URL or blob cache reference.
type VolumeRef struct {
	URL                   string            `json:"url,omitempty"`
	Headers               map[string]string `json:"headers,omitempty"`
	KeepHeadersOnRedirect []string          `json:"keepHeadersOnRedirect,omitempty"`
	BlobRef               string            `json:"blobRef,omitempty"`
	Size                  int64             `json:"size"`
}

// TrackHint identifies one track inside a multi-track audio release or archive
// when exact inner filenames are unknown before opening.
type TrackHint struct {
	Disc   int    `json:"disc,omitempty"`
	Number int    `json:"number,omitempty"`
	Title  string `json:"title,omitempty"`
}

// ReleaseHint hands a release to Ether's Usenet/debrid/torrent pipeline.
type ReleaseHint struct {
	// FileIndex overrides candidate.fileIndex; out-of-range indices fail without fallback.
	FileIndex  *int             `json:"fileIndex,omitempty"`
	NZBURL     string           `json:"nzbUrl,omitempty"`
	NZBRef     string           `json:"nzbRef,omitempty"`
	TorrentRef string           `json:"torrentRef,omitempty"`
	Magnet     string           `json:"magnet,omitempty"`
	InfoHash   string           `json:"infoHash,omitempty"`
	TorrentURL string           `json:"torrentUrl,omitempty"`
	Candidate  *SourceCandidate `json:"candidate,omitempty"`
	// File selects an exact torrent path or unique basename, including non-video files.
	File           string     `json:"file,omitempty"`
	FileHint       string     `json:"fileHint,omitempty"`
	Track          *TrackHint `json:"track,omitempty"`
	PasswordSecret string     `json:"passwordSecret,omitempty"`
	// CachedOnly requires an already-cached debrid copy even when the owner turned
	// "Cached torrents only" off. Omit or leave false to follow the owner's debrid setting.
	CachedOnly bool `json:"cachedOnly,omitempty"`
	// FileCount optionally hints how many files a torrent contains when Ether has
	// not seen its file list yet (e.g. a magnet for one entry of a large collection),
	// so providers whose cache check cannot see zip-only 100+-file torrents (TorBox)
	// try a cached-only add instead of trusting a checkcached miss.
	FileCount int `json:"fileCount,omitempty"`
}

// KindResolveRequest asks a resolver for playable sources for one item.
type KindResolveRequest struct {
	Kind       string         `json:"-"`
	LibraryKey string         `json:"libraryKey"`
	Item       KindItem       `json:"item"`
	PartIndex  *int           `json:"partIndex,omitempty"`
	Source     string         `json:"source,omitempty"`
	Client     map[string]any `json:"client,omitempty"`
}

// StreamURLOptions scopes a frontend adapter stream ticket to a specific
// zero-based item part and/or resolver PlayableSource.ID (#576, #661) and
// optionally asks Ether to return an absolute URL using the client-visible
// request origin or the adapter's first published address (#772).
type StreamURLOptions struct {
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

// KindResolveResponse is a resolver's ordered sources.
type KindResolveResponse struct {
	// With sources, Pending is a fallback if cache policy blocks every source.
	Pending   bool             `json:"pending,omitempty"`
	RetryInMS int64            `json:"retryInMs,omitempty"`
	Job       string           `json:"job,omitempty"`
	Sources   []PlayableSource `json:"sources"`
	Parts     []KindItemPart   `json:"parts,omitempty"`
	TTLSec    int              `json:"ttlSec,omitempty"`
}

// KindLiveRequest asks a kind live provider for currently live items on a
// declared "live" library tab (POST /ether/v1/kinds/{kind}/live).
type KindLiveRequest struct {
	Kind   string `json:"-"`
	Tab    string `json:"tab,omitempty"`
	Q      string `json:"q,omitempty"`
	Filter string `json:"filter,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// KindLiveItem is one live stream item returned by POST /ether/v1/kinds/{kind}/live.
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

// KindLiveFilter is one optional filter option returned by a live tab.
type KindLiveFilter struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// KindLivePage is returned by POST /ether/v1/kinds/{kind}/live.
type KindLivePage struct {
	Items      []KindLiveItem   `json:"items"`
	Filters    []KindLiveFilter `json:"filters,omitempty"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

// KindLiveResolveRequest asks a kind live provider to resolve one live item ID
// into a playable stream URL (POST /ether/v1/kinds/{kind}/live/resolve).
type KindLiveResolveRequest struct {
	Kind string `json:"-"`
	ID   string `json:"id"`
}

// KindLiveResolveResponse is returned by POST /ether/v1/kinds/{kind}/live/resolve.
type KindLiveResolveResponse struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	MIME    string            `json:"mime,omitempty"`
	Title   string            `json:"title,omitempty"`
}

// SetupTestRequest is Ether's setup-step test call: the step key plus the
// saved (non-secret) values of the step's fields.
type SetupTestRequest struct {
	Step   string         `json:"step"`
	Fields map[string]any `json:"fields,omitempty"`
}

// SetupTestResult is a setup test's answer.
type SetupTestResult struct {
	OK     bool              `json:"ok"`
	Detail string            `json:"detail,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
}

// Reserved plugin error codes Ether understands on kind routes.
var (
	ErrKindNotFound     = &RouteError{Status: http.StatusNotFound, Code: "not_found", Message: "Item not found."}
	ErrKindNotAvailable = &RouteError{Status: http.StatusOK, Code: "not_available", Message: "No source is available right now."}
)

// CatalogProvider serves browse and detail for one kind. Implement
// ChangesProvider too for delta pull sync; without it Ether re-walks.
type CatalogProvider interface {
	Browse(ctx *Context, req KindBrowseRequest) (KindPage, error)
	Detail(ctx *Context, kind, id string) (KindItem, error)
}

// ChangesProvider is the optional delta feed for pull sync.
type ChangesProvider interface {
	Changes(ctx *Context, req KindChangesRequest) (KindChangePage, error)
}

// SearchProvider is the optional provider-side search route.
type SearchProvider interface {
	Search(ctx *Context, req KindBrowseRequest) (KindPage, error)
}

// ExpandProvider is the optional on-demand facet/person slice expander for
// kinds that declare browse.expand.
type ExpandProvider interface {
	Expand(ctx *Context, req KindExpandRequest) (KindExpandResponse, error)
}

// KindLyricsRequest asks for one stored playable item's lyrics.
type KindLyricsRequest struct {
	Kind string   `json:"kind"`
	Item KindItem `json:"item"`
}

// KindLyrics is a lyrics answer: timed LRC lines ("[mm:ss.xx] words"), plain
// text, or instrumental. Source is shown for attribution.
type KindLyrics struct {
	SyncedLyrics string `json:"syncedLyrics,omitempty"`
	PlainLyrics  string `json:"plainLyrics,omitempty"`
	Instrumental bool   `json:"instrumental,omitempty"`
	Source       string `json:"source,omitempty"`
	// Lines are structured timed lines; when present Ether uses them instead
	// of SyncedLyrics (word timing, translation and romanization lines).
	Lines []KindLyricsLine `json:"lines,omitempty"`
}

// KindLyricsLine is one timed line (milliseconds), optionally with per-word
// timing, a translation and a romanization.
type KindLyricsLine struct {
	StartMs      int64            `json:"startMs"`
	EndMs        int64            `json:"endMs,omitempty"`
	Text         string           `json:"text"`
	Words        []KindLyricsWord `json:"words,omitempty"`
	Translation  string           `json:"translation,omitempty"`
	Romanization string           `json:"romanization,omitempty"`
}

// KindLyricsWord is one sung word of a line (milliseconds).
type KindLyricsWord struct {
	StartMs int64  `json:"startMs"`
	EndMs   int64  `json:"endMs,omitempty"`
	Text    string `json:"text"`
}

// LyricsProvider serves lyrics for kinds that declare lyrics: true. Return
// found=false when there are none (Ether answers 404 and remembers the miss).
type LyricsProvider interface {
	Lyrics(ctx *Context, req KindLyricsRequest) (lyrics KindLyrics, found bool, err error)
}

// ImportTrack is one track of a playlist being imported.
type ImportTrack struct {
	Title       string  `json:"title"`
	Artist      string  `json:"artist"`
	Album       string  `json:"album,omitempty"`
	ISRC        string  `json:"isrc,omitempty"`
	DurationSec float64 `json:"durationSec,omitempty"`
}

// KindImportRequest asks a kind to read a playlist link (POST
// /ether/v1/kinds/{kind}/import); answer 404 for a link it does not read.
type KindImportRequest struct {
	URL string `json:"url"`
}

// KindImport is a link's playlist: name and tracks in order.
type KindImport struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Tracks      []ImportTrack `json:"tracks"`
	Total       int           `json:"total,omitempty"`     // the source playlist's track count, when known
	Truncated   bool          `json:"truncated,omitempty"` // the source shared fewer tracks than it has
}

// KindIdentifyRequest asks a kind to find its items for tracks (POST
// /ether/v1/kinds/{kind}/identify).
type KindIdentifyRequest struct {
	Tracks []ImportTrack `json:"tracks"`
}

// KindIdentifyMatch answers one requested track by index: the matched item's
// id and how (isrc|title), candidates when several fit, or neither.
type KindIdentifyMatch struct {
	Index      int      `json:"index"`
	ID         string   `json:"id,omitempty"`
	By         string   `json:"by,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
}

// KindIdentify carries the items the matches need (stored like live-search
// results) and the matches; tracks left out are asked again.
type KindIdentify struct {
	Items   []KindItem          `json:"items"`
	Matches []KindIdentifyMatch `json:"matches"`
}

// KindRelatedRequest asks for items related to one stored item: "similar"
// (a detail row such as an artist's "Fans also like") or "continuation" (what
// plays after it when a queue ends).
type KindRelatedRequest struct {
	Kind    string   `json:"kind"`
	Item    KindItem `json:"item"`
	Purpose string   `json:"purpose"`
	Limit   int      `json:"limit"`
}

// KindRelated is a relation answer: items to store (with any parents they
// need), Order naming the ones to show (default: same level for similar,
// playable for continuation), and an optional row label.
type KindRelated struct {
	Label string     `json:"label,omitempty"`
	Items []KindItem `json:"items"`
	Order []string   `json:"order,omitempty"`
}

// RelatedProvider serves relations for kinds that declare related.
type RelatedProvider interface {
	Related(ctx *Context, req KindRelatedRequest) (KindRelated, error)
}

// KindResolver turns a playable item into ordered playable sources.
type KindResolver interface {
	Resolve(ctx *Context, req KindResolveRequest) (KindResolveResponse, error)
}

// KindMatcher scores source candidates for one item.
type KindMatcher interface {
	Match(ctx *Context, req KindMatchRequest) (KindMatchResponse, error)
}

// KindLiveProvider serves a kind's declared "live" library tab: listing
// currently live streams and resolving an item ID into a playable stream URL.
type KindLiveProvider interface {
	Live(ctx *Context, req KindLiveRequest) (KindLivePage, error)
	ResolveLive(ctx *Context, req KindLiveResolveRequest) (KindLiveResolveResponse, error)
}

// FrontendAdapter is a plugin-provided protocol surface (OPDS, Subsonic,
// custom). Ether authenticates the viewer first and forwards the request
// under /ether/v1/frontend/{key}; the handler sees X-Ether-Viewer headers
// and never a credential.
type FrontendAdapter interface {
	Handler() http.Handler
}

// SetupTester answers setup-step test calls.
type SetupTester interface {
	TestSetup(ctx *Context, req SetupTestRequest) (SetupTestResult, error)
}

type kindHandlers struct {
	catalog   map[string]CatalogProvider
	resolve   map[string]KindResolver
	match     map[string]KindMatcher
	live      map[string]KindLiveProvider
	liveFeeds map[string]LiveFeedProvider
	frontend  map[string]FrontendAdapter
	setup     SetupTester
	// screen sources and actions by "<screen>/<name>" ("*" matches any screen).
	screenSources map[string]ScreenSourceHandler
	screenActions map[string]ScreenActionHandler
}

func (p *Plugin) kinds() *kindHandlers {
	if p.kindHandlers == nil {
		p.kindHandlers = &kindHandlers{
			catalog: map[string]CatalogProvider{}, resolve: map[string]KindResolver{},
			match: map[string]KindMatcher{}, live: map[string]KindLiveProvider{},
			liveFeeds: map[string]LiveFeedProvider{}, frontend: map[string]FrontendAdapter{},
		}
	}
	return p.kindHandlers
}

// OnCatalog registers the catalogue provider for one declared kind.
func (p *Plugin) OnCatalog(kind string, c CatalogProvider) { p.kinds().catalog[kind] = c }

// OnResolve registers the resolver for one declared kind.
func (p *Plugin) OnResolve(kind string, r KindResolver) { p.kinds().resolve[kind] = r }

// OnMatch registers the matcher for one declared kind.
func (p *Plugin) OnMatch(kind string, m KindMatcher) { p.kinds().match[kind] = m }

// OnLive registers the live-tab provider for one declared kind.
func (p *Plugin) OnLive(kind string, l KindLiveProvider) { p.kinds().live[kind] = l }

// OnFrontend registers one declared frontend adapter.
func (p *Plugin) OnFrontend(key string, a FrontendAdapter) { p.kinds().frontend[key] = a }

// OnSetupTest registers the handler for /ether/v1/setup/{step}/test.
func (p *Plugin) OnSetupTest(t SetupTester) { p.kinds().setup = t }

// StreamURL mints a short-lived, scoped ticket for one of this plugin's own
// kind items and returns the Ether stream URL to embed in a frontend adapter
// response (spec §6.5). The viewer is taken from the request's X-Ether-Viewer
// header, which Ether injects after authenticating; the plugin never sees a
// credential. Bytes are served by Ether from the ticket, never by the plugin.
func (c *Context) StreamURL(itemRef string) (string, error) {
	if c.Ether == nil {
		return "", errors.New("stream url: no host service available")
	}
	viewerID, err := strconv.ParseInt(strings.TrimSpace(c.Headers["X-Ether-Viewer"]), 10, 64)
	if err != nil || viewerID <= 0 {
		return "", errors.New("stream url: no authenticated viewer on this request")
	}
	return c.Ether.StreamURL(c.Context, viewerID, itemRef)
}

// StreamPartURL mints a short-lived, scoped ticket for one zero-based part of
// a multi-file kind item (#576) and returns the Ether stream URL to embed in a
// frontend adapter response.
func (c *Context) StreamPartURL(itemRef string, partIndex int) (string, error) {
	if partIndex < 0 {
		return "", errors.New("stream url: partIndex must be non-negative")
	}
	return c.StreamURLWithOptions(itemRef, StreamURLOptions{PartIndex: &partIndex})
}

// StreamURLWithOptions mints a short-lived, scoped ticket for one of this
// plugin's own kind items, optionally pinned to a zero-based part and/or a
// specific resolver PlayableSource.ID (#576, #661) and optionally resolved to
// an absolute URL (#772).
func (c *Context) StreamURLWithOptions(itemRef string, opts StreamURLOptions) (string, error) {
	if opts.Wait != nil && opts.Wait.MaxSeconds <= 0 {
		return "", fmt.Errorf("stream url: wait.maxSeconds must be a positive integer")
	}
	if c.Ether == nil {
		return "", errors.New("stream url: no host service available")
	}
	if opts.PartIndex != nil && *opts.PartIndex < 0 {
		return "", errors.New("stream url: partIndex must be non-negative")
	}
	if opts.Source != "" && strings.TrimSpace(opts.Source) == "" {
		return "", errors.New("stream url: source must not be blank")
	}
	viewerID, err := strconv.ParseInt(strings.TrimSpace(c.Headers["X-Ether-Viewer"]), 10, 64)
	if err != nil || viewerID <= 0 {
		return "", errors.New("stream url: no authenticated viewer on this request")
	}
	ctx := c.Context
	if tok := c.viewerToken(); tok != "" {
		ctx = WithViewerToken(ctx, tok)
	}
	if oh, ok := c.Ether.(StreamURLOptionsHost); ok {
		return oh.StreamURLWithOptions(ctx, viewerID, itemRef, opts)
	}
	if opts.Source != "" {
		return "", errors.New("stream url: host does not support source-scoped stream tickets")
	}
	if opts.Absolute {
		return "", errors.New("stream url: host does not support absolute stream URLs")
	}
	if opts.Wait != nil {
		return "", errors.New("stream url: host does not support preparation waits")
	}
	if opts.PartIndex != nil {
		if ph, ok := c.Ether.(PartStreamHost); ok {
			return ph.StreamPartURL(ctx, viewerID, itemRef, *opts.PartIndex)
		}
		u, err := c.Ether.StreamURL(ctx, viewerID, itemRef)
		if err != nil {
			return "", err
		}
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		return u + sep + "part=" + strconv.Itoa(*opts.PartIndex), nil
	}
	return c.Ether.StreamURL(ctx, viewerID, itemRef)
}

// RequestNZB opens an indexer search reference and returns a file list and vault reference.
func (c *Context) RequestNZB(ref string) (SourceCandidate, error) {
	res, err := c.ResolveSourcesWithResult(SourceQuery{Kind: "nzb_request", NZBRef: ref})
	if err != nil {
		return SourceCandidate{}, err
	}
	if len(res.Candidates) != 1 || res.Candidates[0].NZBRef == "" {
		return SourceCandidate{}, errors.New("nzb_request returned no vault reference")
	}
	return res.Candidates[0], nil
}

// ResolveSourcesWithResult runs sources.resolve and returns both the candidate
// list and per-indexer search statuses for indexer_search queries (#733).
// Falls back to Host.ResolveSources when the host does not implement
// ResolveSourcesResultHost.
func (c *Context) ResolveSourcesWithResult(q SourceQuery) (SourceResolveResult, error) {
	if c.Ether == nil {
		return SourceResolveResult{}, errors.New("resolve sources: no host service available")
	}
	if rh, ok := c.Ether.(ResolveSourcesResultHost); ok {
		return rh.ResolveSourcesWithResult(c.Context, q)
	}
	cands, err := c.Ether.ResolveSources(c.Context, q)
	if err != nil {
		return SourceResolveResult{}, err
	}
	if cands == nil {
		cands = []SourceCandidate{}
	}
	return SourceResolveResult{Candidates: cands}, nil
}

func (c *Context) viewerToken() string {
	if tok := strings.TrimSpace(headerValue(c.Headers, HeaderViewerToken)); tok != "" {
		return tok
	}
	if tok, ok := ViewerTokenFromContext(c.Context); ok {
		return tok
	}
	return ""
}

// KindVisible evaluates the library's Rules and viewer access policy for a
// bounded batch (at most MaxKindVisibleIDs) of external IDs in kindKey (#615).
// Only frontend adapter requests carrying Ether's authenticated viewer token
// may call it.
func (c *Context) KindVisible(kindKey string, ids []string) ([]string, error) {
	if c.Ether == nil {
		return nil, errors.New("kinds.visible: no host service available")
	}
	kh, ok := c.Ether.(KindPolicyHost)
	if !ok {
		return nil, errors.New("kinds.visible: host client does not implement KindPolicyHost")
	}
	tok := c.viewerToken()
	if tok == "" {
		return nil, errors.New("kinds.visible: no authenticated frontend adapter viewer on this request")
	}
	return kh.KindVisible(WithViewerToken(c.Context, tok), kindKey, ids)
}

// KindBrowse browses or searches kindKey with the library's Rules and viewer
// access policy applied for the requesting frontend-adapter viewer (#615).
// Only frontend adapter requests carrying Ether's authenticated viewer token
// may call it.
func (c *Context) KindBrowse(kindKey string, query KindBrowseQuery) (KindPage, error) {
	if c.Ether == nil {
		return KindPage{}, errors.New("kinds.browse: no host service available")
	}
	kh, ok := c.Ether.(KindPolicyHost)
	if !ok {
		return KindPage{}, errors.New("kinds.browse: host client does not implement KindPolicyHost")
	}
	tok := c.viewerToken()
	if tok == "" {
		return KindPage{}, errors.New("kinds.browse: no authenticated frontend adapter viewer on this request")
	}
	return kh.KindBrowse(WithViewerToken(c.Context, tok), kindKey, query)
}

// dispatchKinds handles the /ether/v1/kinds, /ether/v1/setup and
// /ether/v1/frontend route families. It reports false when the path is
// none of them so Dispatch can fall through to its 404.
func (p *Plugin) dispatchKinds(pctx *Context, method, path string, req RawRequest) (RawResponse, bool) {
	if !strings.HasPrefix(path, "/ether/v1/") {
		return RawResponse{}, false
	}
	rest := strings.TrimPrefix(path, "/ether/v1/")
	parts := strings.Split(rest, "/")
	// Frontend adapters are protocol surfaces (WebDAV, Subsonic, OPDS, …) and
	// see every method token. The kind and setup routes stay JSON GET/POST.
	if parts[0] == "frontend" {
		if len(parts) >= 2 {
			key, _ := url.PathUnescape(parts[1])
			return p.dispatchFrontend(pctx, method, key, "/"+strings.Join(parts[2:], "/"), req), true
		}
		return RawResponse{}, false
	}
	if method != http.MethodPost && method != http.MethodGet {
		return errorJSON(http.StatusMethodNotAllowed, "method_not_allowed", "Kind routes use GET or POST."), true
	}
	switch {
	case len(parts) >= 2 && parts[0] == "kinds":
		kind, _ := url.PathUnescape(parts[1])
		return p.dispatchKind(pctx, kind, parts[2:], req), true
	case len(parts) >= 3 && parts[0] == "live":
		feed, _ := url.PathUnescape(parts[1])
		return p.dispatchLiveFeed(pctx, method, feed, parts[2:]), true
	case len(parts) == 4 && (parts[0] == "screens" || parts[0] == "surfaces") && (parts[2] == "sources" || parts[2] == "actions"):
		screen, _ := url.PathUnescape(parts[1])
		name, _ := url.PathUnescape(parts[3])
		return p.dispatchScreen(pctx, method, screen, parts[2], name, req), true
	case len(parts) == 3 && parts[0] == "setup" && parts[2] == "test":
		if p.kindHandlers == nil || p.kindHandlers.setup == nil {
			return errorJSON(http.StatusNotFound, "not_found", "Setup test handler is not registered."), true
		}
		var in SetupTestRequest
		if len(req.Body) > 0 {
			if err := json.Unmarshal(req.Body, &in); err != nil {
				return errorJSON(http.StatusBadRequest, "invalid_json", "Request body must be valid JSON."), true
			}
		}
		if in.Step == "" {
			in.Step, _ = url.PathUnescape(parts[1])
		}
		res, err := p.kindHandlers.setup.TestSetup(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err), true
		}
		return okJSON(res, http.StatusOK), true
	}
	return RawResponse{}, false
}

func (p *Plugin) dispatchKind(pctx *Context, kind string, tail []string, req RawRequest) RawResponse {
	h := p.kindHandlers
	decode := func(v any) (RawResponse, bool) {
		if len(req.Body) == 0 {
			return RawResponse{}, true
		}
		if err := json.Unmarshal(req.Body, v); err != nil {
			return errorJSON(http.StatusBadRequest, "invalid_json", "Request body must be valid JSON."), false
		}
		return RawResponse{}, true
	}
	switch {
	case len(tail) == 1 && (tail[0] == "browse" || tail[0] == "search"):
		prov := lookup(h, func(k *kindHandlers) CatalogProvider { return k.catalog[kind] })
		if prov == nil {
			return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No catalogue provider is registered for kind %q.", kind))
		}
		var in KindBrowseRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		if in.Limit <= 0 {
			in.Limit = 200
		}
		if tail[0] == "search" {
			sp, isSearch := prov.(SearchProvider)
			if !isSearch {
				return errorJSON(http.StatusNotImplemented, "not_implemented", "This provider does not search.")
			}
			page, err := sp.Search(pctx, in)
			return kindPageResponse(page, err, in.Limit)
		}
		page, err := prov.Browse(pctx, in)
		return kindPageResponse(page, err, in.Limit)
	case len(tail) == 1 && tail[0] == "expand":
		prov := lookup(h, func(k *kindHandlers) CatalogProvider { return k.catalog[kind] })
		ep, isExpand := prov.(ExpandProvider)
		if prov == nil || !isExpand {
			return errorJSON(http.StatusNotImplemented, "not_implemented", "This provider does not expand facets.")
		}
		var in KindExpandRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		res, err := ep.Expand(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		if res.Job == "" && res.Items == nil {
			res.Items = []KindItem{}
		}
		return okJSON(res, http.StatusOK)
	case len(tail) == 1 && tail[0] == "related":
		prov := lookup(h, func(k *kindHandlers) CatalogProvider { return k.catalog[kind] })
		rp, isRelated := prov.(RelatedProvider)
		if prov == nil || !isRelated {
			return errorJSON(http.StatusNotImplemented, "not_implemented", "This provider has no related items.")
		}
		var in KindRelatedRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		res, err := rp.Related(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		if res.Items == nil {
			res.Items = []KindItem{}
		}
		return okJSON(res, http.StatusOK)
	case len(tail) == 1 && tail[0] == "lyrics":
		prov := lookup(h, func(k *kindHandlers) CatalogProvider { return k.catalog[kind] })
		lp, isLyrics := prov.(LyricsProvider)
		if prov == nil || !isLyrics {
			return errorJSON(http.StatusNotImplemented, "not_implemented", "This provider has no lyrics.")
		}
		var in KindLyricsRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		res, found, err := lp.Lyrics(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		if !found {
			return errorJSON(http.StatusNotFound, "not_found", "No lyrics for this item.")
		}
		return okJSON(res, http.StatusOK)
	case len(tail) == 2 && tail[0] == "items":
		prov := lookup(h, func(k *kindHandlers) CatalogProvider { return k.catalog[kind] })
		if prov == nil {
			return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No catalogue provider is registered for kind %q.", kind))
		}
		id, _ := url.PathUnescape(tail[1])
		item, err := prov.Detail(pctx, kind, id)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		return okJSON(item, http.StatusOK)
	case len(tail) == 1 && tail[0] == "changes":
		prov := lookup(h, func(k *kindHandlers) CatalogProvider { return k.catalog[kind] })
		cp, isChanges := prov.(ChangesProvider)
		if prov == nil || !isChanges {
			// 404/501 tells Ether to fall back to a re-walk diff.
			return errorJSON(http.StatusNotImplemented, "not_implemented", "This provider has no changes feed.")
		}
		var in KindChangesRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		page, err := cp.Changes(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		if page.Upserts == nil {
			page.Upserts = []KindItem{}
		}
		return okJSON(page, http.StatusOK)
	case len(tail) == 1 && tail[0] == "resolve":
		var r KindResolver
		if h != nil {
			r = h.resolve[kind]
		}
		if r == nil {
			return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No resolver is registered for kind %q.", kind))
		}
		var in KindResolveRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		res, err := r.Resolve(pctx, in)
		if err != nil {
			if errors.Is(err, ErrKindNotAvailable) {
				return okJSON(KindResolveResponse{Sources: []PlayableSource{}}, http.StatusOK)
			}
			return toRouteErrorResponse(err)
		}
		if res.Sources == nil {
			res.Sources = []PlayableSource{}
		}
		return okJSON(res, http.StatusOK)
	case len(tail) == 1 && tail[0] == "live":
		var lp KindLiveProvider
		if h != nil {
			lp = h.live[kind]
		}
		if lp == nil {
			return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No live provider is registered for kind %q.", kind))
		}
		var in KindLiveRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		if in.Limit <= 0 {
			in.Limit = 50
		}
		page, err := lp.Live(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		if page.Items == nil {
			page.Items = []KindLiveItem{}
		}
		if in.Limit > 0 && len(page.Items) > in.Limit {
			page.Items = page.Items[:in.Limit]
		}
		return okJSON(page, http.StatusOK)
	case len(tail) == 2 && tail[0] == "live" && tail[1] == "resolve":
		var lp KindLiveProvider
		if h != nil {
			lp = h.live[kind]
		}
		if lp == nil {
			return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No live provider is registered for kind %q.", kind))
		}
		var in KindLiveResolveRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		res, err := lp.ResolveLive(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		return okJSON(res, http.StatusOK)
	case len(tail) == 1 && tail[0] == "match":
		var m KindMatcher
		if h != nil {
			m = h.match[kind]
		}
		if m == nil {
			return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No matcher is registered for kind %q.", kind))
		}
		var in KindMatchRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		res, err := m.Match(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		if res.Matches == nil {
			res.Matches = []KindMatchScore{}
		}
		return okJSON(res, http.StatusOK)
	}
	return errorJSON(http.StatusNotFound, "not_found", "No kind route matched.")
}

func lookup(h *kindHandlers, get func(*kindHandlers) CatalogProvider) CatalogProvider {
	if h == nil {
		return nil
	}
	return get(h)
}

// kindPageResponse enforces the page contract: never more than limit
// items (Ether treats an oversized page as a contract error).
func kindPageResponse(page KindPage, err error, limit int) RawResponse {
	if err != nil {
		return toRouteErrorResponse(err)
	}
	if limit > 0 && len(page.Items) > limit {
		page.Items = page.Items[:limit]
	}
	if page.Items == nil {
		page.Items = []KindItem{}
	}
	return okJSON(page, http.StatusOK)
}

type frontendContextKey struct{}

type frontendUploadKey struct{}

const frontendUploadHeader = "X-Ether-Frontend-Upload"

// FrontendUploadFromContext returns upload metadata for a declared upload route.
func FrontendUploadFromContext(ctx context.Context) (*FrontendUpload, bool) {
	u, ok := ctx.Value(frontendUploadKey{}).(*FrontendUpload)
	return u, ok
}

// FrontendContext returns the Ether plugin Context attached to a
// FrontendAdapter http.Request during dispatch, so ServeHTTP handlers can
// call pctx.KindVisible, pctx.KindBrowse, pctx.StreamURL, etc. directly (#615).
func FrontendContext(ctx context.Context) (*Context, bool) {
	if ctx == nil {
		return nil, false
	}
	pctx, ok := ctx.Value(frontendContextKey{}).(*Context)
	return pctx, ok && pctx != nil
}

const maxFrontendBody = 10 << 20

func (p *Plugin) dispatchFrontend(pctx *Context, method, key, subPath string, req RawRequest) RawResponse {
	var a FrontendAdapter
	if p.kindHandlers != nil {
		a = p.kindHandlers.frontend[key]
	}
	if a == nil || a.Handler() == nil {
		return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No frontend adapter is registered for %q.", key))
	}
	if subPath == "/" || subPath == "" {
		subPath = "/"
	}
	// Protocol adapters (Subsonic, OPDS, WebDAV) carry their arguments in the
	// query string; the server relays them, so rebuild the request URL with
	// the original query. The server flattens repeated keys to the first
	// value, so a query map holds one value per key.
	_, query := parsePathAndQuery(req.Path, req.Query)
	target := "http://ether.invalid" + subPath
	if len(query) > 0 {
		target += "?" + encodeQuery(query)
	}
	reqCtx := pctx.Context
	if tok := headerValue(req.Headers, HeaderViewerToken); tok != "" {
		reqCtx = WithViewerToken(reqCtx, tok)
		pctx.Context = reqCtx
	}
	reqCtx = context.WithValue(reqCtx, frontendContextKey{}, pctx)
	upload := req.Upload
	if headerValue(req.Headers, frontendUploadHeader) == "1" {
		upload = new(FrontendUpload)
		if err := json.Unmarshal(req.Body, upload); err != nil {
			return errorJSON(http.StatusBadRequest, "invalid_upload", "Invalid frontend upload metadata.")
		}
	}
	bodyBytes := req.Body
	if upload != nil {
		reqCtx = context.WithValue(reqCtx, frontendUploadKey{}, upload)
		bodyBytes = nil
	}
	httpReq := httptest.NewRequestWithContext(reqCtx, method, target, bytes.NewReader(bodyBytes))
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	if upload != nil {
		httpReq.Header.Del(frontendUploadHeader)
		httpReq.Header.Del("Content-Type")
		if upload.ContentType != "" {
			httpReq.Header.Set("Content-Type", upload.ContentType)
		}
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httpReq)
	res := rec.Result()
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxFrontendBody+1))
	if err != nil {
		return errorJSON(http.StatusBadGateway, "adapter_error", err.Error())
	}
	if len(body) > maxFrontendBody {
		return errorJSON(http.StatusBadGateway, "adapter_error", "Frontend adapter response is larger than 10 MiB; serve media through Ether stream URLs.")
	}
	hdrs := map[string]string{}
	for k := range res.Header {
		hdrs[k] = res.Header.Get(k)
	}
	// A HEAD reply carries headers only, exactly as a real server would.
	if method == http.MethodHead {
		body = nil
	}
	return RawResponse{Status: res.StatusCode, Headers: hdrs, BinaryBody: body}
}

// encodeQuery renders a query map back into a raw query string, preserving
// key order is unnecessary; values are escaped so adapters see what the
// viewer sent.
func encodeQuery(query map[string]string) string {
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(query[k]))
	}
	return strings.Join(parts, "&")
}
