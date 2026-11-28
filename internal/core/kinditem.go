package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Plugin kind item envelope and source query types (spec §2.2,
// §3.2, §4.5, §7). These types are the frozen cross-slice contract:
// plugins declare kinds with them, pluginitems stores them, and the
// protocol frontends project them. Keep edits additive.

// ItemRef is a plugin item's stable identity:
// "pk:" + pluginID + ":" + kindKey + ":" + externalID, where the
// external id never contains ':' or '/'. Ether assigns it; plugins
// only ever see their own external ids.
type ItemRef string

// NewItemRef formats the canonical ItemRef for (pluginID, kindKey, externalID).
func NewItemRef(pluginID, kindKey, externalID string) ItemRef {
	return ItemRef("pk:" + pluginID + ":" + kindKey + ":" + externalID)
}

// ValidExternalID reports whether id is a legal plugin external id
// (spec §2.2: ^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$, no ':' or '/').
func ValidExternalID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		alnum := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if i == 0 {
			if !alnum {
				return false
			}
			continue
		}
		if !alnum && c != '.' && c != '_' && c != '~' && c != '-' {
			return false
		}
	}
	return true
}

// Parse splits r into (pluginID, kindKey, externalID) and reports
// whether r is a well-formed ItemRef (spec §2.2).
func (r ItemRef) Parse() (pluginID, kindKey, externalID string, ok bool) {
	s, hasPrefix := strings.CutPrefix(string(r), "pk:")
	if !hasPrefix {
		return "", "", "", false
	}
	p1 := strings.IndexByte(s, ':')
	if p1 <= 0 {
		return "", "", "", false
	}
	pluginID = s[:p1]
	rest := s[p1+1:]
	p2 := strings.IndexByte(rest, ':')
	if p2 <= 0 {
		return "", "", "", false
	}
	kindKey = rest[:p2]
	externalID = rest[p2+1:]
	if strings.ContainsRune(pluginID, '/') || strings.ContainsRune(kindKey, '/') || !ValidExternalID(externalID) {
		return "", "", "", false
	}
	return pluginID, kindKey, externalID, true
}

// ParseItemRef validates s and returns it as an ItemRef.
func ParseItemRef(s string) (ItemRef, error) {
	ref := ItemRef(s)
	if _, _, _, ok := ref.Parse(); !ok {
		return "", fmt.Errorf("itemRef %q: invalid format", s)
	}
	return ref, nil
}

// Valid reports whether r is a well-formed ItemRef.
func (r ItemRef) Valid() bool {
	_, _, _, ok := r.Parse()
	return ok
}

// Plugin returns the pluginID component of r, or "" if r is invalid.
func (r ItemRef) Plugin() string {
	p, _, _, _ := r.Parse()
	return p
}

// Kind returns the kindKey component of r, or "" if r is invalid.
func (r ItemRef) Kind() string {
	_, k, _, _ := r.Parse()
	return k
}

// External returns the externalID component of r, or "" if r is invalid.
func (r ItemRef) External() string {
	_, _, e, _ := r.Parse()
	return e
}

func (r ItemRef) String() string { return string(r) }

// ItemUnavailable explains why an item cannot currently be played.
type ItemUnavailable struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// KindItem is the standardised plugin item envelope (spec §3.2).
type KindItem struct {
	Unavailable *ItemUnavailable           `json:"unavailable,omitempty"`
	Ref         ItemRef                    `json:"-" ` // assigned by Ether, never sent to the plugin
	Key         RatingKey                  `json:"-" ` // allocated PluginKey(seq), populated on store reads
	AddedAt     int64                      `json:"-"`  // store insertion unix seconds, populated on store reads
	ID          string                     `json:"id"` // externalID
	Level       string                     `json:"level"`
	ParentID    string                     `json:"parentId,omitempty"`
	Index       int                        `json:"index,omitempty"` // position within parent / container leaf index
	Title       string                     `json:"title"`
	SortTitle   string                     `json:"sortTitle,omitempty"` // Ether derives it when empty
	Summary     string                     `json:"summary,omitempty"`
	Year        int                        `json:"year,omitempty"`
	Released    int64                      `json:"released,omitempty"` // unix seconds
	Rating      float64                    `json:"rating,omitempty"`
	Playable    bool                       `json:"playable,omitempty"`
	ChildCount  int                        `json:"childCount,omitempty"`
	LeafCount   int                        `json:"leafCount,omitempty"`
	Art         []KindArtRef               `json:"art,omitempty"`
	Fields      map[string]json.RawMessage `json:"fields,omitempty"` // validated against the kind declaration
	Hints       json.RawMessage            `json:"hints,omitempty"`  // opaque; stored and echoed to the resolver verbatim
	// Recording identifies the work an item is a copy of, shared by every
	// release that carries it (for music, "isrc:<ISRC>"). Apps group the same
	// recording across albums and compilations by it; empty when unknown.
	Recording string `json:"recording,omitempty"`
	// Members: a list item's ordered members (external ids of other items of
	// the kind), its children. Only on a level declared members: true.
	Members []string `json:"members,omitempty"`
	ETag    string   `json:"etag,omitempty"`
}

// KindItemPart describes one declared or resolved file/part of a multi-file
// KindItem (spec §3.2, §4.1, #576).
type KindItemPart struct {
	Index       int     `json:"index"`
	Name        string  `json:"name,omitempty"`
	MIME        string  `json:"mime,omitempty"`
	Size        int64   `json:"size,omitempty"`
	DurationSec float64 `json:"durationSec,omitempty"`
	FileIndex   *int    `json:"fileIndex,omitempty"`
}

// ExtractKindItemPartsFromItem inspects a KindItem for declared parts,
// checking hints.parts first and falling back to a declared "parts" array
// field when present (#576).
func ExtractKindItemPartsFromItem(item KindItem) []KindItemPart {
	if parts := ExtractKindItemParts(item.Hints); len(parts) > 0 {
		return parts
	}
	if raw, ok := item.Fields["parts"]; ok && len(raw) > 0 {
		if wrapped, err := json.Marshal(struct {
			Parts json.RawMessage `json:"parts"`
		}{Parts: raw}); err == nil {
			return ExtractKindItemParts(wrapped)
		}
	}
	return nil
}

// ExtractKindItemParts inspects a KindItem's Hints JSON for a non-empty
// "parts" array and returns generic per-part metadata in declaration order.
// Returns nil when the item does not declare parts.
func ExtractKindItemParts(hints json.RawMessage) []KindItemPart {
	if len(hints) == 0 {
		return nil
	}
	var env struct {
		Parts []json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(hints, &env); err != nil || len(env.Parts) == 0 {
		return nil
	}
	out := make([]KindItemPart, 0, len(env.Parts))
	for i, raw := range env.Parts {
		part := KindItemPart{Index: i}
		var str string
		if err := json.Unmarshal(raw, &str); err == nil {
			part.Name = strings.TrimSpace(str)
			out = append(out, part)
			continue
		}
		var obj struct {
			Name        string  `json:"name"`
			File        string  `json:"file"`
			Filename    string  `json:"filename"`
			Path        string  `json:"path"`
			ROM         string  `json:"rom"`
			Label       string  `json:"label"`
			Title       string  `json:"title"`
			ID          string  `json:"id"`
			MIME        string  `json:"mime"`
			MIMEType    string  `json:"mimeType"`
			ContentType string  `json:"contentType"`
			Size        int64   `json:"size"`
			SizeBytes   int64   `json:"sizeBytes"`
			ByteSize    int64   `json:"byteSize"`
			DurationSec float64 `json:"durationSec"`
			Duration    float64 `json:"duration"`
			FileIndex   *int    `json:"fileIndex"`
		}
		if err := json.Unmarshal(raw, &obj); err == nil {
			for _, cand := range []string{obj.Name, obj.File, obj.Filename, obj.Path, obj.ROM, obj.Label, obj.Title, obj.ID} {
				if s := strings.TrimSpace(cand); s != "" {
					part.Name = s
					break
				}
			}
			for _, cand := range []string{obj.MIME, obj.MIMEType, obj.ContentType} {
				if s := strings.TrimSpace(cand); s != "" {
					part.MIME = s
					break
				}
			}
			for _, sz := range []int64{obj.Size, obj.SizeBytes, obj.ByteSize} {
				if sz > 0 {
					part.Size = sz
					break
				}
			}
			for _, dur := range []float64{obj.DurationSec, obj.Duration} {
				if dur > 0 {
					part.DurationSec = dur
					break
				}
			}
			if obj.FileIndex != nil && *obj.FileIndex >= 0 {
				v := *obj.FileIndex
				part.FileIndex = &v
			}
		}
		out = append(out, part)
	}
	return out
}

// ExtractKindItemChaptersFromItem inspects a KindItem for declared chapters,
// checking hints.chapters first and falling back to a declared "chapters" array
// field when present.
func ExtractKindItemChaptersFromItem(item KindItem) []Chapter {
	if chs := ExtractKindItemChapters(item.Hints); len(chs) > 0 {
		return chs
	}
	if raw, ok := item.Fields["chapters"]; ok && len(raw) > 0 {
		if wrapped, err := json.Marshal(struct {
			Chapters json.RawMessage `json:"chapters"`
		}{Chapters: raw}); err == nil {
			return ExtractKindItemChapters(wrapped)
		}
	}
	return nil
}

// ExtractKindItemChapters inspects a KindItem's Hints JSON for a non-empty
// "chapters" array and returns normalized Chapter markers in declaration order.
func ExtractKindItemChapters(hints json.RawMessage) []Chapter {
	if len(hints) == 0 {
		return nil
	}
	var env struct {
		Chapters []json.RawMessage `json:"chapters"`
	}
	if err := json.Unmarshal(hints, &env); err != nil || len(env.Chapters) == 0 {
		return nil
	}
	out := make([]Chapter, 0, len(env.Chapters))
	var cursor float64
	for i, raw := range env.Chapters {
		var obj struct {
			Title        string   `json:"title"`
			Name         string   `json:"name"`
			Label        string   `json:"label"`
			StartSec     *float64 `json:"startSec"`
			StartSeconds *float64 `json:"startSeconds"`
			Start        *float64 `json:"start"`
			EndSec       *float64 `json:"endSec"`
			EndSeconds   *float64 `json:"endSeconds"`
			End          *float64 `json:"end"`
			DurationSec  *float64 `json:"durationSec"`
			Duration     *float64 `json:"duration"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}
		title := ""
		for _, cand := range []string{obj.Title, obj.Name, obj.Label} {
			if s := strings.TrimSpace(cand); s != "" {
				title = s
				break
			}
		}
		var startSec float64
		hasStart := false
		for _, cand := range []*float64{obj.StartSec, obj.StartSeconds, obj.Start} {
			if cand != nil && *cand >= 0 {
				startSec = *cand
				hasStart = true
				break
			}
		}
		if !hasStart {
			startSec = cursor
		}
		var endSec float64
		for _, cand := range []*float64{obj.EndSec, obj.EndSeconds, obj.End} {
			if cand != nil && *cand > startSec {
				endSec = *cand
				break
			}
		}
		if endSec <= startSec {
			for _, cand := range []*float64{obj.DurationSec, obj.Duration} {
				if cand != nil && *cand > 0 {
					endSec = startSec + *cand
					break
				}
			}
		}
		if endSec <= startSec && !hasStart && title == "" {
			continue
		}
		if title == "" {
			title = fmt.Sprintf("Chapter %d", i+1)
		}
		if endSec > cursor {
			cursor = endSec
		}
		out = append(out, Chapter{
			ID:    int64(i),
			Title: title,
			Start: time.Duration(startSec * float64(time.Second)),
			End:   time.Duration(endSec * float64(time.Second)),
		})
	}
	return out
}

// KindArtRef is one artwork reference on a KindItem.
type KindArtRef struct {
	Role   string `json:"role"`
	URL    string `json:"url"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// KindPage is one page of kind items.
type KindPage struct {
	Items      []KindItem `json:"items"`
	NextCursor string     `json:"nextCursor,omitempty"` // opaque; absent = last page
	Total      *int       `json:"total,omitempty"`      // provider claim; Ether never trusts it for its own totals
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

// Alternatives returns the {field, values} alternatives carried by g. A
// single-field {field, values} group returns a slice of one; when Any is
// present, its alternatives are returned (after the top-level field/values if
// both were supplied).
func (g KindBrowseFacetGroup) Alternatives() []KindBrowseFacetAlt {
	hasSingle := strings.TrimSpace(g.Field) != "" && len(g.Values) > 0
	if len(g.Any) == 0 {
		if !hasSingle {
			return nil
		}
		return []KindBrowseFacetAlt{{Field: g.Field, Values: g.Values}}
	}
	if !hasSingle {
		return g.Any
	}
	out := make([]KindBrowseFacetAlt, 0, len(g.Any)+1)
	out = append(out, KindBrowseFacetAlt{Field: g.Field, Values: g.Values})
	out = append(out, g.Any...)
	return out
}

// KindBrowseRequest is the request body sent to a catalog_provider
// browse or search route (spec §3.1, §3.2) and passed to the
// viewer-scoped kinds.browse host service (#615).
type KindBrowseRequest struct {
	ParentID string `json:"parentId,omitempty"`
	Level    string `json:"level,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Sort     string `json:"sort,omitempty"`
	Desc     bool   `json:"desc,omitempty"`
	// Facets requires any of the listed values per field (OR within a
	// field, AND across fields). AllFacets requires every listed value
	// per field, ExcludeFacets drops items having any listed value, and
	// AnyGroups adds one OR-group per entry — either a single {field,
	// values} list or {any: [{field, values}, ...]} ORed across fields
	// (#767, #807). All four combine with AND and accept the same
	// per-type values (including lo..hi ranges and people names).
	Facets        map[string][]string    `json:"facets,omitempty"`
	AllFacets     map[string][]string    `json:"allFacets,omitempty"`
	ExcludeFacets map[string][]string    `json:"excludeFacets,omitempty"`
	AnyGroups     []KindBrowseFacetGroup `json:"anyGroups,omitempty"`
	// Empty tests field presence or absence per field key (#807): true
	// keeps items with no value in the field, false keeps items with at
	// least one. Counts toward neither facet value bound; unknown fields
	// are rejected.
	Empty map[string]bool `json:"empty,omitempty"`
	Query string          `json:"query,omitempty"`
	// ExcludeQuery drops items whose indexed text matches, using the same
	// text matching as Query (#807).
	ExcludeQuery string `json:"excludeQuery,omitempty"`
	// Total controls whether kinds.browse computes KindPage.Total (#682):
	//   - "" or "cheap" (default): populate Total only when O(1) (item or
	//     field tallies, a cached exact count, or a single-page result) and
	//     omit Total otherwise instead of running a full COUNT(*).
	//   - "none": never compute or return Total.
	//   - "exact": compute an exact Total (cached per library generation).
	Total string `json:"total,omitempty"`
}

// KindExpandRequest is the request body sent to POST /ether/v1/kinds/{kind}/expand
// to expand a declared browse facet bucket, people credit, or container on demand.
type KindExpandRequest struct {
	Facet    string `json:"facet,omitempty"`
	Value    string `json:"value,omitempty"`
	ValueID  int64  `json:"valueId,omitempty"`
	ParentID string `json:"parentId,omitempty"`
	Level    string `json:"level,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
}

// KindExpandResponse is the response from POST /ether/v1/kinds/{kind}/expand.
// Plugins return either {items, cursor} directly or {job: "/ether/job/..."} for long work.
type KindExpandResponse struct {
	Items      []KindItem `json:"items,omitempty"`
	Cursor     string     `json:"cursor,omitempty"`
	NextCursor string     `json:"nextCursor,omitempty"`
	Job        string     `json:"job,omitempty"`
}

// EffectiveCursor returns Cursor when non-empty, falling back to NextCursor.
func (r KindExpandResponse) EffectiveCursor() string {
	if r.Cursor != "" {
		return r.Cursor
	}
	return r.NextCursor
}

// KindChangePage is one delta page returned by POST /ether/v1/kinds/{kind}/changes
// during pull sync (spec §3.5).
type KindChangePage struct {
	// Checkpoint marks an upsert-only page whose next cursor can resume in a later sync.
	Checkpoint bool       `json:"checkpoint,omitempty"`
	Upserts    []KindItem `json:"upserts,omitempty"`
	Deletes    []string   `json:"deletes,omitempty"` // externalIDs
	NextCursor string     `json:"nextCursor,omitempty"`
	Complete   bool       `json:"complete"`
}

// KindMatchRequest is the request body sent to POST /ether/v1/kinds/{kind}/match (spec §7).
type KindMatchRequest struct {
	Item       KindItem          `json:"item"`
	Candidates []SourceCandidate `json:"candidates"`
	Settings   map[string]any    `json:"settings,omitempty"`
}

// KindMatchScore is one candidate score returned by a matcher (spec §7).
type KindMatchScore struct {
	CandidateRef string   `json:"candidateRef"`
	Score        float64  `json:"score"`
	Reasons      []string `json:"reasons,omitempty"`
}

// KindMatchResponse is the response from POST /ether/v1/kinds/{kind}/match (spec §7).
type KindMatchResponse struct {
	Pending   bool             `json:"pending,omitempty"`
	RetryInMS int64            `json:"retryInMs,omitempty"`
	Job       string           `json:"job,omitempty"`
	Matches   []KindMatchScore `json:"matches"`
}

// SourceQuery asks Ether to find sources for a kind resolver
// (spec §4.5).
type SourceQuery struct {
	Kind  string `json:"kind"` // indexer_search|indexer_search_results|indexer_search_cancel|indexers|debrid|debrid_check|debrid_acquire|release_files|usenet_check|nzb_request
	Query string `json:"query,omitempty"`
	// Category accepts comma-separated Newznab IDs or standard family names.
	// When set with an empty Query, indexer_search requests recent releases.
	Category string `json:"category,omitempty"`
	// Lookup, on an indexer_search with a Category, adds a typed lookup
	// (e.g. {type: "album", fields: {artist, title}}) for plugin sources that
	// declared it. Every other indexer searches Query alone.
	Lookup *SourceLookup `json:"lookup,omitempty"`
	// Profile names the quality profile to score video release names against
	// on indexer_search; empty uses the owner's default profile.
	Profile    string   `json:"profile,omitempty"`
	InfoHashes []string `json:"infoHashes,omitempty"`
	InfoHash   string   `json:"infoHash,omitempty"`
	Magnet     string   `json:"magnet,omitempty"`
	// TorrentURL carries a torrent download or details page link for torrent
	// indexers that do not emit an info hash or magnet in search results.
	TorrentURL string `json:"torrentUrl,omitempty"`
	TorrentRef string `json:"torrentRef,omitempty"`
	// NZBURL carries a Usenet NZB link for usenet_check when the plugin does
	// not pass a full Candidate from indexer_search.
	NZBURL      string       `json:"nzbUrl,omitempty"`
	NZBRef      string       `json:"nzbRef,omitempty"`
	NZBIdentity *NZBIdentity `json:"nzbIdentity,omitempty"`
	// Candidate carries a candidate previously returned by indexer_search
	// back for release_files or usenet_check, so an NZB or torrent link is
	// resolved through its originating indexer.
	Candidate *SourceCandidate `json:"candidate,omitempty"`
	// FileCount optionally hints how many files a torrent contains when Ether
	// has not seen its file list yet (e.g. a magnet for a large collection),
	// so zip-only debrid caches are checked via cached-only add.
	FileCount int `json:"fileCount,omitempty"`
	// Stream, when true on indexer_search, starts the indexer fan-out and
	// returns immediately with a searchId handle to poll via
	// indexer_search_results or stop via indexer_search_cancel.
	Stream bool `json:"stream,omitempty"`
	// SearchID identifies a streaming search started by indexer_search with
	// stream:true; required on indexer_search_results and indexer_search_cancel.
	SearchID string `json:"searchId,omitempty"`
	// WaitMS sets how long indexer_search_results waits (in milliseconds,
	// capped at 10000 ms) for at least one new arrival or search completion
	// before returning. Zero returns immediately with arrivals buffered so far.
	WaitMS int `json:"waitMs,omitempty"`
	// FirstResults, when true on indexer_search, returns early as soon as the
	// fastest indexer(s) return results plus the SettleMS window (default
	// 250 ms; 0 returns immediately), while slower indexers keep running in the
	// background to populate Ether's search cache for later calls. Default
	// false waits for every searched indexer.
	FirstResults bool `json:"firstResults,omitempty"`
	// SettleMS sets the settle window in milliseconds after the first results
	// arrive under FirstResults (default 250 ms when nil; 0 returns
	// immediately), or after MinResults is met when FirstResults is false.
	// Clamped to the effective call timeout.
	SettleMS *int `json:"settleMs,omitempty"`
	// MinResults sets a minimum deduplicated candidate count for early return
	// on indexer_search. Once at least MinResults unique results have arrived,
	// Ether returns early while remaining indexers keep filling Ether's search
	// cache. Combined with FirstResults, returns as soon as either MinResults
	// is reached or the settle window elapses.
	MinResults int `json:"minResults,omitempty"`
	// SkipIndexers excludes matching enabled indexers from an indexer_search
	// fan-out by indexerId or indexer name (case-insensitive, trimmed).
	SkipIndexers []string `json:"skipIndexers,omitempty"`
	// OnlyIndexers, when non-empty, restricts an indexer_search fan-out to
	// matching enabled indexers by indexerId or indexer name (case-insensitive,
	// trimmed). Entries also matching SkipIndexers are excluded.
	OnlyIndexers []string `json:"onlyIndexers,omitempty"`
	// TimeoutMS sets a per-call wait limit in milliseconds for indexer_search,
	// capped by Ether's 30 s indexer-search ceiling and any shorter route
	// deadline. When TimeoutMS is shorter than the ceiling and expires while
	// slow indexers keep filling the search cache, those indexers are reported
	// as pending (and listed in top-level pending); indexers that hit the 30 s
	// ceiling are reported as timed_out. Omit or 0 keeps the 30 s default.
	TimeoutMS int `json:"timeoutMs,omitempty"`
}

// Status values for IndexerSearchStatus.Status.
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

// IndexerRef identifies one configured indexer by ID and display name,
// returned in the top-level pending list on indexer_search and
// indexer_search_results responses.
type IndexerRef struct {
	IndexerID string `json:"indexerId"`
	Indexer   string `json:"indexer"`
}

// IndexerSearchStatus reports either one enabled indexer's outcome on an
// indexer_search / indexer_search_results / indexer_search_cancel query
// (Status, Results, MS, Reason, Error) or one enabled indexer's summary on an
// indexers listing query (Kind, MedianMS, LastMS, LastError).
type IndexerSearchStatus struct {
	ConfiguredName string `json:"-"` // owner-entered display name for playback diagnostics
	IndexerID      string `json:"indexerId"`
	Indexer        string `json:"indexer"`
	Status         string `json:"status,omitempty"` // answered|pending|timed_out|skipped|error
	Results        int    `json:"results,omitempty"`
	MS             int64  `json:"ms"`
	Reason         string `json:"reason,omitempty"` // populated on automatic skips (for example paused_backoff)
	Error          string `json:"error,omitempty"`
	// Kind, MedianMS, LastMS and LastError are populated on kind:"indexers"
	// responses (never carrying API keys, URLs with secrets, or cookies).
	Kind      string `json:"kind,omitempty"` // newznab|torznab|builtin|stremio|zilean|addon
	MedianMS  *int64 `json:"medianMs,omitempty"`
	LastMS    *int64 `json:"lastMs,omitempty"`
	LastError string `json:"lastError,omitempty"`
}

// MarshalJSON preserves ms:0 on search status entries (Status != "") while
// omitting status/ms/results/reason/error on kind:"indexers" listing entries
// (Status == "").
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

// SourceResolveResult is the full wire response returned by sources.resolve.
// The debrid query returns policy. Other queries return candidates and may
// include per-indexer outcomes, streaming state, or the cache policy.
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

// MarshalJSON emits the debrid policy by itself for the debrid query.
// Candidate queries always emit candidates and preserve non-nil empty search
// slices as [].
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
// returned by a release_files source query. Hand Path back as
// ReleaseHint.FileHint (or ContainerRef.Member) to open that exact member.
type ReleaseMember struct {
	Index  int    `json:"index"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Stored bool   `json:"stored,omitempty"`
}

// UsenetCheckResult reports the sampled article availability verdict for one
// Usenet NZB release, as returned by a usenet_check source query.
type UsenetCheckResult struct {
	Available bool `json:"available"`
	Checked   int  `json:"checked"`
	Missing   int  `json:"missing"`
	// Cached is true when this verdict came from Ether's bounded per-NZB
	// cache (including a recorded playback failure) rather than a fresh
	// NNTP check on this call.
	Cached bool `json:"cached,omitempty"`
	// PlaybackFailed is true when a previous real playback attempt against
	// this NZB failed with an article or release error (such as missing or
	// corrupt articles), overriding or pre-empting any sampled check.
	PlaybackFailed bool `json:"playbackFailed,omitempty"`
	// Reason carries a short diagnostic code when Available is false
	// (e.g. "articles_missing", "playback_article_missing",
	// "playback_article_corrupt", "playback_release_rejected").
	Reason string `json:"reason,omitempty"`
	// CheckedAt is the Unix timestamp (seconds) when this verdict or
	// playback failure was recorded.
	CheckedAt int64 `json:"checkedAt,omitempty"`
}

// ReleaseFile is one file inside a candidate release, as returned by a
// release_files source query. Index is the position Ether's own file
// selection counts: the debrid provider's file-list order for torrents,
// NZB document order for Usenet. Hand it back as ReleaseHint.FileIndex,
// or match Path with ReleaseHint.File. When the file is the first volume
// of a readable RAR, 7z, or ZIP archive on Usenet, Members lists the
// non-directory files inside that archive.
type ReleaseFile struct {
	Index int    `json:"index"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	// SizeApproximate is true when Size is a segment-derived decoded estimate
	// rather than an exact byte count (e.g. a Usenet NZB whose PAR2/yEnc
	// headers could not be read). Exact sizes leave it false/omitted.
	SizeApproximate bool            `json:"sizeApproximate,omitempty"`
	Members         []ReleaseMember `json:"members,omitempty"`
}

// DebridServiceRef identifies one owner-configured debrid service (without
// exposing credentials) that holds a cached torrent or served an acquisition.
type DebridServiceRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// SourceCandidate is one Ether-found release for a resolver to rank,
// carrying the release-hint fields plus the facts matching needs.
// Torrent candidates carry their infoHash, magnet, and/or torrentUrl and never
// populate nzbUrl; Usenet candidates populate nzbUrl. Same-origin download
// links returned by indexer_search are stripped of indexer API keys and
// secrets (Ether re-attaches them at fetch time on the originating indexer's
// origin), so plugins can safely cache candidates without storing credentials.
type SourceCandidate struct {
	InfoHash   string `json:"infoHash,omitempty"`
	Magnet     string `json:"magnet,omitempty"`
	TorrentURL string `json:"torrentUrl,omitempty"`
	TorrentRef string `json:"torrentRef,omitempty"`
	NZBURL     string `json:"nzbUrl,omitempty"`
	NZBRef     string `json:"nzbRef,omitempty"`
	// FileIndex selects a zero-based entry in the torrent provider file list; nil uses video selection.
	FileIndex *int   `json:"fileIndex,omitempty"`
	Title     string `json:"title,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Seeders   int    `json:"seeders,omitempty"`
	Cached    bool   `json:"cached,omitempty"`
	// CachedBy lists the owner-configured debrid services holding this
	// torrent in the order Ether would acquire from them (or, on
	// debrid_acquire, the service that served it). Omitted when uncached.
	CachedBy []DebridServiceRef `json:"cachedBy,omitempty"`
	// Indexer and IndexerID name the indexer an indexer_search result came
	// from, so a plugin that hands the candidate back can be fetched
	// through that indexer (an NZB needs its originating indexer).
	Indexer   string `json:"indexer,omitempty"`
	IndexerID string `json:"indexerId,omitempty"`
	// Category is the standard Newznab/Torznab category ID reported for this
	// release when known, preserving media type for follow-up downloads.
	Category int `json:"category,omitempty"`
	// Score, Resolution, Source and Codec are populated on indexer_search
	// candidates whose release name identifies video, scored against the
	// requested (or owner default) quality profile; non-video candidates
	// leave them empty.
	Score      *int   `json:"score,omitempty"`
	Resolution int    `json:"resolution,omitempty"`
	Source     string `json:"source,omitempty"`
	Codec      string `json:"codec,omitempty"`
	// Files is populated by release_files and nzb_request: the files inside
	// this candidate release.
	Files []ReleaseFile `json:"files,omitempty"`
	// FileCount is the torrent's known file count when reported by an indexer
	// or release_files query, allowing zip-only debrid caches to be probed by
	// cached-only add even when Files is omitted.
	FileCount int `json:"fileCount,omitempty"`
	// UsenetCheck is populated only by a usenet_check query: the sampled
	// article availability verdict and any recorded playback failure for
	// this NZB release.
	UsenetCheck *UsenetCheckResult `json:"usenetCheck,omitempty"`
	// PluginRelease names a release a stream_source plugin answered for a
	// category search it declared (ether.contributes.source.search). It has
	// no info hash, magnet or NZB: release_files asks that plugin for the
	// files and a release source opens it through the plugin's open route.
	PluginRelease *PluginReleaseRef `json:"pluginRelease,omitempty"`
}

// Deprecated link fields use these non-HTTP schemes for Ether references.
const (
	PluginNZBRefPrefix     = "ether-nzb:"
	PluginTorrentRefPrefix = "ether-torrent:"
)

// NZBIdentity identifies a native indexer release when no search reference is available.
type NZBIdentity struct {
	IndexerID string `json:"indexerId"`
	GUID      string `json:"guid"`
}

// PluginReleaseRef identifies one release of a stream_source plugin: the
// plugin's id and the lazy stream id it answered the search with.
type PluginReleaseRef struct {
	Plugin string `json:"plugin"`
	ID     string `json:"id"`
}

// SourceLookup is a typed lookup on a category search, such as an album by
// artist and title. Type is a key the answering plugin source declared in
// ether.contributes.source.search.lookups; Fields are its named values.
// Indexers that do not declare the lookup search the free-text query only.
type SourceLookup struct {
	Type   string            `json:"type"`
	Fields map[string]string `json:"fields,omitempty"`
}

// PluginLibraryKey is the library key a plugin kind owns
// (spec §1.1): "pk-" + slug(pluginID) + "-" + slug(kindKey), matching
// internal/libraries.UpsertPluginLibrary. The key is an opaque unique
// slug; ownership is always read back from the stored plugin/kind
// columns, never parsed out of the key.
func PluginLibraryKey(pluginID, kindKey string) string {
	return "pk-" + libraryKeySlug(pluginID) + "-" + libraryKeySlug(kindKey)
}

func libraryKeySlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	return b.String()
}

// PlayableSource is one candidate way to acquire bytes for a KindItem or one of
// its parts (spec §4.1). Multiple sources are tried in order.
type PlayableSource struct {
	Path    string `json:"path,omitempty"`
	ID      string `json:"id"`
	Label   string `json:"label,omitempty"`
	Pattern string `json:"pattern"` // "range" | "download" | "direct" | "container" | "release" | "file"
	MIME    string `json:"mime"`
	// Tier optionally marks interchangeable sources of the same quality tier.
	// Sources sharing a non-nil Tier may be reordered by Ether's source routing
	// (owner preference, measured startup, and stall demotion) and may overtake
	// each other after the ranked open grace period. Sources in different tiers
	// keep the plugin's tier order; when Tier is nil, the plugin's exact order
	// is kept.
	Tier *int  `json:"tier,omitempty"`
	Size int64 `json:"size,omitempty"`
	// SizeApproximate marks Size as an approximate hint (for example from a
	// Usenet release_files estimate) rather than an exact byte count:
	// kindstream applies a tolerance band instead of failing on exact byte
	// inequality, while exact sizes (false/omitted) remain strict.
	SizeApproximate bool    `json:"sizeApproximate,omitempty"`
	DurationSec     float64 `json:"durationSec,omitempty"`
	// DurationToleranceSec opts into open-time container duration verification:
	// when both DurationSec and DurationToleranceSec are positive, Ether reads
	// the container duration (reusing the durable release_probes cache when
	// available) and refuses the source as wrong content (core.ErrWrongContent
	// / kindstream.ErrPluginAnswer) when the observed duration differs from
	// DurationSec by more than DurationToleranceSec seconds, so the next
	// candidate is tried. Omitted or zero leaves DurationSec as estimated
	// metadata only.
	DurationToleranceSec float64           `json:"durationToleranceSec,omitempty"`
	URL                  string            `json:"url,omitempty"`
	Headers              map[string]string `json:"headers,omitempty"`
	// KeepHeadersOnRedirect lists optional host or wildcard (*.example.org)
	// patterns (within permissions.network / granted hosts) that may keep
	// credentials and caller headers across redirects when the target is not
	// already the same origin or a strict same-site subdomain. Never applies
	// across HTTPS -> HTTP downgrades.
	KeepHeadersOnRedirect []string `json:"keepHeadersOnRedirect,omitempty"`
	BlobRef               string   `json:"blobRef,omitempty"`
	SHA256                string   `json:"sha256,omitempty"`
	// SHA1 and CRC32 are the digests content catalogues actually publish
	// (hex; CRC32 is the IEEE polynomial as 8 hex digits). Any declared
	// digest — and Size, when set — is checked against the bytes actually
	// served, including a release's selected file or container member.
	SHA1      string         `json:"sha1,omitempty"`
	CRC32     string         `json:"crc32,omitempty"`
	MaxBytes  int64          `json:"maxBytes,omitempty"`
	ExpiresAt int64          `json:"expiresAt,omitempty"`
	Container *ContainerRef  `json:"container,omitempty"`
	Release   *ReleaseHint   `json:"release,omitempty"`
	Parts     []KindItemPart `json:"parts,omitempty"`
}

// ContainerRef describes an archive member inside one or more volume streams
// (spec §4.1, §5.3).
type ContainerRef struct {
	Format         string      `json:"format,omitempty"` // "" (sniff) | "rar" | "7z" | "zip"
	Volumes        []VolumeRef `json:"volumes"`
	Member         string      `json:"member,omitempty"`
	MemberIndex    *int        `json:"memberIndex,omitempty"`
	PasswordSecret string      `json:"passwordSecret,omitempty"`
}

// VolumeRef locates one archive volume by URL or blobcache reference (spec §4.1).
type VolumeRef struct {
	URL                   string            `json:"url,omitempty"`
	Headers               map[string]string `json:"headers,omitempty"`
	KeepHeadersOnRedirect []string          `json:"keepHeadersOnRedirect,omitempty"`
	BlobRef               string            `json:"blobRef,omitempty"`
	Size                  int64             `json:"size"`
}

// TrackHint identifies one track inside a multi-track audio release or archive
// when exact filenames cannot be known before opening (for example scene RARs).
// Ether matches by disc/track number first, then normalised title tokens,
// failing on ambiguity. Exact fileHint semantics remain unchanged.
type TrackHint struct {
	Disc   int    `json:"disc,omitempty"`
	Number int    `json:"number,omitempty"`
	Title  string `json:"title,omitempty"`
}

// ReleaseHint hands an NZB, magnet, infohash, torrent link, or pre-built
// Candidate to Ether's existing Usenet / debrid / torrent acquisition pipeline
// (spec §4.1, §4.3).
type ReleaseHint struct {
	// FileIndex overrides candidate.fileIndex; out-of-range indices fail without fallback.
	FileIndex  *int             `json:"fileIndex,omitempty"`
	NZBURL     string           `json:"nzbUrl,omitempty"`
	NZBRef     string           `json:"nzbRef,omitempty"`
	Magnet     string           `json:"magnet,omitempty"`
	InfoHash   string           `json:"infoHash,omitempty"`
	TorrentURL string           `json:"torrentUrl,omitempty"`
	TorrentRef string           `json:"torrentRef,omitempty"`
	Candidate  *SourceCandidate `json:"candidate,omitempty"`
	// File selects an exact torrent path or unique basename, including non-video files.
	File           string     `json:"file,omitempty"`
	FileHint       string     `json:"fileHint,omitempty"`
	Track          *TrackHint `json:"track,omitempty"`
	PasswordSecret string     `json:"passwordSecret,omitempty"`
	// CachedOnly forces debrid acquisition to require an already-cached copy
	// even when the owner turned "Cached torrents only" off. When omitted or
	// false, torrent releases follow the owner's debrid setting (queuing
	// uncached downloads when cached-only is off).
	CachedOnly bool `json:"cachedOnly,omitempty"`
	// FileCount optionally hints how many files a torrent contains when Ether
	// has not seen its file list yet (for example a magnet for one entry in a
	// large collection). Providers whose cache check cannot see zip-only
	// high-file-count torrents (such as TorBox at 100+ files) use this to try
	// a cached-only add instead of trusting a checkcached miss.
	FileCount int `json:"fileCount,omitempty"`
}

// KindResolveRequest is the payload sent to a plugin's kind_resolve handler
// (spec §4.1).
type KindResolveRequest struct {
	LibraryKey string         `json:"libraryKey"`
	Item       KindItem       `json:"item"`
	PartIndex  *int           `json:"partIndex,omitempty"`
	Source     string         `json:"source,omitempty"`
	Client     map[string]any `json:"client,omitempty"`
}

// KindResolveResponse is the response returned by a plugin's kind_resolve
// handler (spec §4.1).
type KindResolveResponse struct {
	// With sources, Pending is a fallback if cache policy blocks every source.
	Pending   bool             `json:"pending,omitempty"`
	RetryInMS int64            `json:"retryInMs,omitempty"`
	Job       string           `json:"job,omitempty"`
	Sources   []PlayableSource `json:"sources"`
	Parts     []KindItemPart   `json:"parts,omitempty"`
	TTLSec    int              `json:"ttlSec,omitempty"`
}

// MediaProbeResult is the container metadata summary returned by the plugin
// host's media.probe service.
type MediaProbeResult struct {
	DurationSec float64 `json:"durationSec"`
	Container   string  `json:"container"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	SizeBytes   int64   `json:"sizeBytes"`
}
