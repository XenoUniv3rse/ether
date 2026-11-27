package plugins

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
)

// KindUpsertResult is what one kinds.upsert batch did (#646): Accepted
// items were taken, of which Added were new, Updated rewrote a stored
// item, and Unchanged matched the stored item and were skipped.
type KindUpsertResult struct {
	Accepted  int
	Added     int
	Updated   int
	Unchanged int
}

// KindUpsertUndo is the upsert result plus the exact change set committed for
// the undo journal (#766): AddedIDs were newly inserted, UpdatedIDs had a
// different content hash from the stored row, and Previous carries pre-write
// copies of UpdatedIDs only when requested (omitted for unchanged items and for
// kinds declaring sync.undo: "ids").
type KindUpsertUndo struct {
	Result     KindUpsertResult
	AddedIDs   []string
	UpdatedIDs []string
	Previous   []core.KindItem
}

// KindBackends are the Ether services the kind host calls end in: the
// plugin-item store, the source resolver and the blob cache. Any field
// may be nil, in which case the matching host call returns
// ErrUnavailable. App wiring fills them (pluginitems, the indexer and
// debrid services, blobcache); the plugin id is always the caller's,
// so a plugin can only ever reach its own kinds and cache.
type KindBackends struct {
	KindUpsert func(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (accepted int, err error)
	// KindUpsertStats, when set, is preferred over KindUpsert and its
	// counts ride the host reply.
	KindUpsertStats func(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (KindUpsertResult, error)
	// KindUpsertUndo, when set, is preferred over KindUpsertStats and
	// KindUpsert: it reports the committed added/updated IDs from the store's
	// content-hash check and reads previous copies only for modified items when
	// snapshotPrevious is true (#766).
	KindUpsertUndo func(ctx context.Context, pluginID, kindKey string, items []core.KindItem, snapshotPrevious bool) (KindUpsertUndo, error)
	KindDelete     func(ctx context.Context, pluginID, kindKey string, externalIDs []string) error
	KindItems      func(ctx context.Context, pluginID, kindKey, cursor string, limit int) (core.KindPage, error)
	KindItemsByID  func(ctx context.Context, pluginID, kindKey string, externalIDs []string) ([]core.KindItem, error)
	KindItemCount  func(ctx context.Context, pluginID string) (int, error)
	// KindVisible and KindBrowse evaluate a plugin library's Rules and viewer
	// access policy for the requesting frontend-adapter viewer (#615).
	KindVisible func(ctx context.Context, pluginID string, viewerID int64, kindKey string, ids []string) ([]string, error)
	KindBrowse  func(ctx context.Context, pluginID string, viewerID int64, kindKey string, q core.KindBrowseRequest) (core.KindPage, error)
	// KindExpand and KindLiveSearch open a container and search for a
	// frontend-adapter viewer the way an Ether app does (kinds.expand /
	// kinds.search): Ether fetches from the plugin and stores the items.
	KindExpand     func(ctx context.Context, pluginID string, viewerID int64, kindKey, parentID string) (pending bool, err error)
	KindLiveSearch func(ctx context.Context, pluginID string, viewerID int64, kindKey, query string, limit int) (found int, err error)
	ResolveSources func(ctx context.Context, pluginID string, q core.SourceQuery) ([]core.SourceCandidate, error)
	// ResolveSourcesResult, when set, is preferred over ResolveSources and
	// carries per-indexer search status alongside candidates (#733).
	ResolveSourcesResult func(ctx context.Context, pluginID string, q core.SourceQuery) (core.SourceResolveResult, error)

	NormalizeStoredReleaseLinks func(context.Context, []byte) ([]byte, error)

	// PeekUsenetCheck returns a cached shared Usenet verdict by keyless NZB URL
	// before admitting a background check against the plugin's daily allowance.
	PeekUsenetCheck func(q core.SourceQuery) (core.SourceCandidate, bool)
	// AdmitBackgroundUsenetCheck reserves one background usenet_check candidate
	// check against the plugin's daily allowance in availability_budget.
	AdmitBackgroundUsenetCheck func(ctx context.Context, pluginID string) (outCtx context.Context, refund func(), resetAt time.Time, err error)
	CachePut                   func(ctx context.Context, pluginID, libraryKey, name string, r io.Reader) (blobRef string, err error)
	CacheLookup                func(pluginID, libraryKey, key string) (blobRef string, ok bool)
	CacheOpen                  func(ctx context.Context, pluginID, blobRef string) (io.ReadSeekCloser, error)
	CacheDelete                func(ctx context.Context, pluginID, blobRef string) error
	CachePartial               func(ctx context.Context, pluginID, libraryKey, key string) (*blobcache.Partial, error)
	// StreamURL mints a scoped stream ticket for one of the plugin's own
	// kind items for one viewer and returns the Ether stream URL (spec
	// §6.5). The backend re-checks item ownership and library visibility.
	StreamURL        func(ctx context.Context, pluginID string, viewerID int64, itemRef string) (string, error)
	StreamPartURL    func(ctx context.Context, pluginID string, viewerID int64, itemRef string, partIndex *int) (string, error)
	StreamURLOptions func(ctx context.Context, pluginID string, viewerID int64, itemRef string, opts StreamURLOptions) (string, error)
	// MediaProbe reads container metadata for a PlayableSource (#712).
	MediaProbe func(ctx context.Context, pluginID string, src core.PlayableSource) (core.MediaProbeResult, error)
}

// SetKindBackends wires the kind host services into the Manager's
// Backends. Called once at startup by app wiring or in tests.
func (m *Manager) SetKindBackends(b KindBackends) {
	m.mu.Lock()
	m.deps.Backends.KindUpsert = b.KindUpsert
	m.deps.Backends.KindUpsertStats = b.KindUpsertStats
	m.deps.Backends.KindUpsertUndo = b.KindUpsertUndo
	m.deps.Backends.KindDelete = b.KindDelete
	m.deps.Backends.KindItems = b.KindItems
	m.deps.Backends.KindItemsByID = b.KindItemsByID
	m.deps.Backends.KindItemCount = b.KindItemCount
	m.deps.Backends.KindVisible = b.KindVisible
	m.deps.Backends.KindBrowse = b.KindBrowse
	m.deps.Backends.KindExpand = b.KindExpand
	m.deps.Backends.KindLiveSearch = b.KindLiveSearch
	m.deps.Backends.ResolveSources = b.ResolveSources
	m.deps.Backends.ResolveSourcesResult = b.ResolveSourcesResult
	m.deps.Backends.NormalizeStoredReleaseLinks = b.NormalizeStoredReleaseLinks
	m.deps.Backends.PeekUsenetCheck = b.PeekUsenetCheck
	m.deps.Backends.AdmitBackgroundUsenetCheck = b.AdmitBackgroundUsenetCheck
	m.deps.Backends.CachePut = b.CachePut
	m.deps.Backends.CacheLookup = b.CacheLookup
	m.deps.Backends.CacheOpen = b.CacheOpen
	m.deps.Backends.CacheDelete = b.CacheDelete
	m.deps.Backends.CachePartial = b.CachePartial
	m.deps.Backends.StreamURL = b.StreamURL
	m.deps.Backends.StreamPartURL = b.StreamPartURL
	m.deps.Backends.StreamURLOptions = b.StreamURLOptions
	m.deps.Backends.MediaProbe = b.MediaProbe
	m.mu.Unlock()
}

func (m *Manager) kindBackends() KindBackends {
	m.mu.Lock()
	defer m.mu.Unlock()
	return KindBackends{
		NormalizeStoredReleaseLinks: m.deps.Backends.NormalizeStoredReleaseLinks,

		KindUpsert:                 m.deps.Backends.KindUpsert,
		KindUpsertStats:            m.deps.Backends.KindUpsertStats,
		KindUpsertUndo:             m.deps.Backends.KindUpsertUndo,
		KindDelete:                 m.deps.Backends.KindDelete,
		KindItems:                  m.deps.Backends.KindItems,
		KindItemsByID:              m.deps.Backends.KindItemsByID,
		KindItemCount:              m.deps.Backends.KindItemCount,
		KindVisible:                m.deps.Backends.KindVisible,
		KindBrowse:                 m.deps.Backends.KindBrowse,
		KindExpand:                 m.deps.Backends.KindExpand,
		KindLiveSearch:             m.deps.Backends.KindLiveSearch,
		ResolveSources:             m.deps.Backends.ResolveSources,
		ResolveSourcesResult:       m.deps.Backends.ResolveSourcesResult,
		PeekUsenetCheck:            m.deps.Backends.PeekUsenetCheck,
		AdmitBackgroundUsenetCheck: m.deps.Backends.AdmitBackgroundUsenetCheck,
		CachePut:                   m.deps.Backends.CachePut,
		CacheLookup:                m.deps.Backends.CacheLookup,
		CacheOpen:                  m.deps.Backends.CacheOpen,
		CacheDelete:                m.deps.Backends.CacheDelete,
		CachePartial:               m.deps.Backends.CachePartial,
		StreamURL:                  m.deps.Backends.StreamURL,
		StreamPartURL:              m.deps.Backends.StreamPartURL,
		StreamURLOptions:           m.deps.Backends.StreamURLOptions,
		MediaProbe:                 m.deps.Backends.MediaProbe,
	}
}

// Per-call caps for kind host services (spec §3.3 sizes, host side).
const (
	MaxKindUpsertItems       = 1000
	MaxKindVisibleIDs        = 500
	MaxKindHintsBytes        = 16 << 10
	MaxKindArtRoles          = 16
	MaxSourceIndexerFilters  = 256
	MaxKindBrowseFacetValues = 256
	MaxKindBrowseAnyGroups   = 32
	// HeaderAdapterViewerToken carries a host-minted, request-scoped token on
	// frontend adapter calls so remote plugins can prove the authenticated
	// viewer on viewer-scoped host callbacks without supplying viewer IDs.
	HeaderAdapterViewerToken = "X-Ether-Viewer-Token"
	// HeaderAdapterOrigin carries the client-visible origin
	// ("scheme://host[:port][/prefix]") derived by Ether on an authenticated
	// frontend adapter request (#772).
	HeaderAdapterOrigin = "X-Ether-Origin"
)

var externalIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`)

// ValidExternalID reports whether id is a legal plugin external id
// (spec §2.2: no ':' or '/').
func ValidExternalID(id string) bool { return externalIDRe.MatchString(id) }

// CheckKindItem validates one item envelope against its kind
// declaration: identity, level, the string caps and the hints shape.
// Field-value typing against KindField is the item store's deeper
// check (spec §3.3); this one is shared by the host and the kit so a
// plugin is told the same thing everywhere. It returns a plain
// reason, or "" when the item passes.
// MaxKindItemMembers bounds one list item's members.
const MaxKindItemMembers = 500

// MaxKindItemRecording bounds an item's recording identity.
const MaxKindItemRecording = 200

func CheckKindItem(decl MediaKindMeta, it core.KindItem) string {
	switch {
	case !ValidExternalID(it.ID):
		return "id must start with a letter or digit and use letters, digits, dot, dash, underscore or tilde, at most 128 characters"
	case it.ParentID != "" && !ValidExternalID(it.ParentID):
		return "parentId must be a valid external id"
	case strings.TrimSpace(it.Title) == "":
		return "title is required"
	case utf8.RuneCountInString(it.Title) > 500:
		return "title is longer than 500 characters"
	case utf8.RuneCountInString(it.Summary) > 20000:
		return "summary is longer than 20000 characters"
	case len(it.Art) > MaxKindArtRoles:
		return "art lists more than 16 roles"
	case len(it.Hints) > MaxKindHintsBytes:
		return "hints are larger than 16 KiB"
	}
	lv, at, ok := decl.Level(it.Level)
	if !ok {
		return "level " + it.Level + " is not declared for this kind"
	}
	if at == 0 && it.ParentID != "" {
		return "a root-level item cannot name a parent"
	}
	if lv.Members && it.ParentID != "" {
		return "a list item (" + it.Level + ") cannot name a parent"
	}
	if at > 0 && it.ParentID == "" && !lv.Members {
		return "a " + it.Level + " item needs a parentId"
	}
	if len(it.Members) > 0 && !lv.Members {
		return "only a list level (members: true) carries members"
	}
	if len(it.Members) > MaxKindItemMembers {
		return "a list carries at most 500 members"
	}
	for _, m := range it.Members {
		if !ValidExternalID(m) || m == it.ID {
			return "members must be valid external ids of other items"
		}
	}
	if len(it.Recording) > MaxKindItemRecording || strings.ContainsAny(it.Recording, "\x00\n\r") {
		return "recording must be at most 200 characters on one line"
	}
	if it.Playable && !lv.Playable {
		return "level " + it.Level + " is not declared playable"
	}
	if len(it.Hints) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(it.Hints, &obj); err != nil || obj == nil {
			return "hints must be a JSON object"
		}
	}
	seenArt := map[string]bool{}
	for _, a := range it.Art {
		if a.Role == "" || a.URL == "" || seenArt[a.Role] {
			return "each art entry needs a distinct role and a url"
		}
		seenArt[a.Role] = true
	}
	for key := range it.Fields {
		if !kindFieldRe.MatchString(key) {
			return "field names must match the declared field pattern"
		}
	}
	return ""
}

// kindCheck resolves the caller's record and one declared kind,
// requiring one of caps. Roles the owner switched off are absent from
// the effective manifest, so they deny like a missing capability.
func (h *host) kindCheck(ctx context.Context, kindKey string, caps ...Capability) (Record, MediaKindMeta, error) {
	r, _, err := h.record(ctx)
	if err != nil {
		return Record{}, MediaKindMeta{}, err
	}
	man := r.Manifest
	man.Ether = man.EffectiveEther()
	allowed := false
	for _, c := range caps {
		if man.HasCapability(c) {
			allowed = true
		}
	}
	if !allowed {
		h.noteDenied(string(caps[0]))
		return Record{}, MediaKindMeta{}, fmt.Errorf("%w: %s", ErrPermissionDenied, caps[0])
	}
	decl, ok := man.MediaKindByKey(kindKey)
	if !ok {
		return Record{}, MediaKindMeta{}, fmt.Errorf("%w: kind %q is not declared", ErrUnavailable, kindKey)
	}
	return r, decl, nil
}

type kindInverse struct {
	Op       string          `json:"op"`
	Kind     string          `json:"kind"`
	Items    []string        `json:"items,omitempty"`    // external ids created on upsert (or targeted on delete/shadow)
	Touched  []string        `json:"touched,omitempty"`  // existing external ids modified under sync.undo: "ids"
	Previous []core.KindItem `json:"previous,omitempty"` // previous items before upsert or delete
}

func uniqueIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	unique := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	return unique
}

// snapshotKindItems looks up any existing items for kindKey whose IDs
// are in ids so a live KindUpsert or KindDelete can record a real
// inverse in the journal.
func (h *host) snapshotKindItems(ctx context.Context, kindKey string, ids []string) (existing []core.KindItem, missing []string) {
	unique := uniqueIDs(ids)
	if len(unique) == 0 {
		return nil, nil
	}
	readFn := h.m.kindBackends().KindItemsByID
	if readFn == nil {
		return nil, unique
	}
	items, err := readFn(ctx, h.plugin, kindKey, unique)
	if err != nil {
		return nil, unique
	}
	found := make(map[string]core.KindItem, len(items))
	for _, it := range items {
		if _, dup := found[it.ID]; !dup {
			found[it.ID] = it
		}
	}
	for _, id := range unique {
		if prev, ok := found[id]; ok {
			existing = append(existing, prev)
		} else {
			missing = append(missing, id)
		}
	}
	return existing, missing
}

func (h *host) KindUpsert(ctx context.Context, kindKey string, items []core.KindItem) (int, error) {
	result, err := h.KindUpsertWithResult(ctx, kindKey, items)
	return result.Accepted, err
}

// KindUpsertWithResult is KindUpsert reporting the added/updated/
// unchanged split when the backend provides it (#646).
func (h *host) KindUpsertWithResult(ctx context.Context, kindKey string, items []core.KindItem) (KindUpsertResult, error) {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return KindUpsertResult{}, err
	}
	r, decl, err := h.kindCheck(ctx, kindKey, CapCatalogProvider)
	if err != nil {
		return KindUpsertResult{}, err
	}
	if decl.Sync.Mode != KindSyncPush {
		return KindUpsertResult{}, fmt.Errorf("%w: kind %s is pull-synced by Ether and takes no pushes", ErrPermissionDenied, kindKey)
	}
	if len(items) > MaxKindUpsertItems {
		return KindUpsertResult{}, fmt.Errorf("plugins: kind upsert batch larger than %d items", MaxKindUpsertItems)
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if msg := CheckKindItem(decl, it); msg != "" {
			return KindUpsertResult{}, fmt.Errorf("plugins: kind item %q: %s", it.ID, msg)
		}
		ids = append(ids, it.ID)
	}
	if r.Shadow {
		return KindUpsertResult{Accepted: len(items)}, h.journal(ctx, r, "kinds.upsert", kindKey, kindInverse{Op: "kinds.upsert", Kind: kindKey, Items: ids})
	}
	backends := h.m.kindBackends()
	if backends.KindUpsert == nil && backends.KindUpsertStats == nil && backends.KindUpsertUndo == nil {
		return KindUpsertResult{}, ErrUnavailable
	}
	idsOnly := decl.Sync.Undo == KindSyncUndoIDs
	if backends.KindUpsertUndo != nil {
		u, err := backends.KindUpsertUndo(ctx, h.plugin, kindKey, items, !idsOnly)
		if err != nil {
			return KindUpsertResult{}, err
		}
		inv := kindInverse{
			Op:    "kinds.upsert",
			Kind:  kindKey,
			Items: u.AddedIDs,
		}
		if idsOnly {
			inv.Touched = u.UpdatedIDs
		} else {
			inv.Previous = u.Previous
		}
		if len(inv.Items) == 0 && len(inv.Touched) == 0 && len(inv.Previous) == 0 {
			return u.Result, nil
		}
		if err := h.journal(ctx, r, "kinds.upsert", kindKey, inv); err != nil {
			return KindUpsertResult{}, err
		}
		return u.Result, nil
	}
	var prev []core.KindItem
	var created []string
	if idsOnly {
		created = uniqueIDs(ids)
	} else {
		prev, created = h.snapshotKindItems(ctx, kindKey, ids)
	}
	var result KindUpsertResult
	if backends.KindUpsertStats != nil {
		result, err = backends.KindUpsertStats(ctx, h.plugin, kindKey, items)
	} else {
		var accepted int
		accepted, err = backends.KindUpsert(ctx, h.plugin, kindKey, items)
		result = KindUpsertResult{Accepted: accepted}
	}
	if err != nil {
		return KindUpsertResult{}, err
	}
	if err := h.journal(ctx, r, "kinds.upsert", kindKey, kindInverse{
		Op:       "kinds.upsert",
		Kind:     kindKey,
		Items:    created,
		Previous: prev,
	}); err != nil {
		return KindUpsertResult{}, err
	}
	return result, nil
}

func (h *host) KindDelete(ctx context.Context, kindKey string, externalIDs []string) error {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return err
	}
	r, decl, err := h.kindCheck(ctx, kindKey, CapCatalogProvider)
	if err != nil {
		return err
	}
	if decl.Sync.Mode != KindSyncPush {
		return fmt.Errorf("%w: kind %s is pull-synced by Ether and takes no pushes", ErrPermissionDenied, kindKey)
	}
	if len(externalIDs) > MaxKindUpsertItems {
		return fmt.Errorf("plugins: kind delete batch larger than %d items", MaxKindUpsertItems)
	}
	for _, id := range externalIDs {
		if !ValidExternalID(id) {
			return fmt.Errorf("plugins: kind item id %q is not a valid external id", id)
		}
	}
	if r.Shadow {
		return h.journal(ctx, r, "kinds.delete", kindKey, kindInverse{Op: "kinds.delete", Kind: kindKey, Items: externalIDs})
	}
	backend := h.m.kindBackends().KindDelete
	if backend == nil {
		return ErrUnavailable
	}
	var prev []core.KindItem
	if decl.Sync.Undo != KindSyncUndoIDs {
		prev, _ = h.snapshotKindItems(ctx, kindKey, externalIDs)
	}
	if err := backend(ctx, h.plugin, kindKey, externalIDs); err != nil {
		return err
	}
	return h.journal(ctx, r, "kinds.delete", kindKey, kindInverse{
		Op:       "kinds.delete",
		Kind:     kindKey,
		Items:    externalIDs,
		Previous: prev,
	})
}

func (h *host) KindItems(ctx context.Context, kindKey, cursor string, limit int) (core.KindPage, error) {
	if _, _, err := h.kindCheck(ctx, kindKey, CapCatalogProvider); err != nil {
		return core.KindPage{}, err
	}
	backends := h.m.kindBackends()
	backend := backends.KindItems
	if backend == nil {
		return core.KindPage{}, ErrUnavailable
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > MaxKindUpsertItems {
		limit = MaxKindUpsertItems
	}
	page, err := backend(ctx, h.plugin, kindKey, cursor, limit)
	if err == nil {
		err = normalizeKindPageHints(ctx, &page, backends.NormalizeStoredReleaseLinks)
	}
	return page, err
}

// KindVisible evaluates the library's Rules and viewer access policy for a
// bounded batch (max MaxKindVisibleIDs) of external IDs in kindKey (#615).
// Only frontend adapter requests carrying an authenticated viewer may call it;
// the viewer comes solely from the host context, never from guest input.
func (h *host) KindVisible(ctx context.Context, kindKey string, ids []string) ([]string, error) {
	if _, _, err := h.kindCheck(ctx, kindKey, CapFrontendAdapter); err != nil {
		return nil, err
	}
	viewerID, ok := ViewerFromContext(ctx)
	if !ok || viewerID <= 0 {
		return nil, fmt.Errorf("%w: kinds.visible requires an authenticated frontend adapter viewer", ErrPermissionDenied)
	}
	if len(ids) > MaxKindVisibleIDs {
		return nil, &ValidationError{Fields: []FieldError{{
			Field:   "ids",
			Message: fmt.Sprintf("kinds.visible accepts at most %d ids per call", MaxKindVisibleIDs),
		}}}
	}
	if len(ids) == 0 {
		return []string{}, nil
	}
	backend := h.m.kindBackends().KindVisible
	if backend == nil {
		return nil, ErrUnavailable
	}
	visible, err := backend(ctx, h.plugin, viewerID, kindKey, ids)
	if err != nil {
		return nil, err
	}
	if visible == nil {
		visible = []string{}
	}
	return visible, nil
}

// ValidKindEmptyField reports whether field may appear as a key in
// KindBrowseRequest.Empty for decl (#807): any declared field on decl, any
// optional item envelope column (released, year, rating, summary, recording,
// parentId, index, or the virtual decade alias), or a <dateField>-decade facet
// whose base is a declared date field.
func ValidKindEmptyField(decl MediaKindMeta, field string) bool {
	if strings.TrimSpace(field) == "" || field != strings.TrimSpace(field) {
		return false
	}
	for _, f := range decl.Fields {
		if f.Key == field {
			return true
		}
	}
	switch field {
	case "released", "year", "rating", "summary", "recording", "parentId", "index", "decade":
		return true
	}
	if base, ok := libraryfilter.SplitDateDecadeFacet(field); ok {
		for _, f := range decl.Fields {
			if f.Key == base && f.Type == "date" {
				return true
			}
		}
	}
	return false
}

// KindBrowse browses or searches kindKey using the same LibraryPolicy path
// as Ether's console and protocol projectors for the requesting
// frontend-adapter viewer (#615). Only frontend adapter requests carrying an
// authenticated viewer may call it; the viewer comes solely from the host
// context, never from guest input.
func (h *host) KindBrowse(ctx context.Context, kindKey string, q core.KindBrowseRequest) (core.KindPage, error) {
	_, decl, err := h.kindCheck(ctx, kindKey, CapFrontendAdapter)
	if err != nil {
		return core.KindPage{}, err
	}
	viewerID, ok := ViewerFromContext(ctx)
	if !ok || viewerID <= 0 {
		return core.KindPage{}, fmt.Errorf("%w: kinds.browse requires an authenticated frontend adapter viewer", ErrPermissionDenied)
	}
	if q.ParentID != "" && !ValidExternalID(q.ParentID) {
		return core.KindPage{}, &ValidationError{Fields: []FieldError{{
			Field:   "parentId",
			Message: "parentId must be a valid external id",
		}}}
	}
	switch q.Total {
	case "", "cheap", "none", "exact":
	default:
		return core.KindPage{}, &ValidationError{Fields: []FieldError{{
			Field:   "total",
			Message: `total must be "cheap", "none", or "exact"`,
		}}}
	}
	if len(q.AnyGroups) > MaxKindBrowseAnyGroups {
		return core.KindPage{}, &ValidationError{Fields: []FieldError{{
			Field:   "anyGroups",
			Message: fmt.Sprintf("anyGroups accepts at most %d groups per query", MaxKindBrowseAnyGroups),
		}}}
	}
	facetValues := 0
	for _, m := range []map[string][]string{q.Facets, q.AllFacets, q.ExcludeFacets} {
		for _, vals := range m {
			facetValues += len(vals)
		}
	}
	anyAlts := 0
	for _, g := range q.AnyGroups {
		facetValues += len(g.Values)
		anyAlts += len(g.Any)
		for _, alt := range g.Any {
			facetValues += len(alt.Values)
		}
	}
	if anyAlts > MaxKindBrowseFacetValues {
		return core.KindPage{}, &ValidationError{Fields: []FieldError{{
			Field:   "anyGroups",
			Message: fmt.Sprintf("anyGroups alternatives exceed %d per query", MaxKindBrowseFacetValues),
		}}}
	}
	if facetValues > MaxKindBrowseFacetValues {
		field := "facets"
		if len(q.AnyGroups) > 0 && len(q.Facets) == 0 && len(q.AllFacets) == 0 && len(q.ExcludeFacets) == 0 {
			field = "anyGroups"
		}
		return core.KindPage{}, &ValidationError{Fields: []FieldError{{
			Field:   field,
			Message: fmt.Sprintf("facet values exceed %d per query", MaxKindBrowseFacetValues),
		}}}
	}
	if len(q.Empty) > 0 {
		keys := make([]string, 0, len(q.Empty))
		for k := range q.Empty {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !ValidKindEmptyField(decl, k) {
				f := "empty"
				if k != "" {
					f = "empty." + k
				}
				return core.KindPage{}, &ValidationError{Fields: []FieldError{{
					Field:   f,
					Message: fmt.Sprintf("unknown field %q", k),
				}}}
			}
		}
	}
	if q.Limit <= 0 {
		q.Limit = 200
	}
	if q.Limit > MaxKindUpsertItems {
		q.Limit = MaxKindUpsertItems
	}
	backends := h.m.kindBackends()
	backend := backends.KindBrowse
	if backend == nil {
		return core.KindPage{}, ErrUnavailable
	}
	page, err := backend(ctx, h.plugin, viewerID, kindKey, q)
	if err != nil {
		return core.KindPage{}, err
	}
	if page.Items == nil {
		page.Items = []core.KindItem{}
	}
	err = normalizeKindPageHints(ctx, &page, backends.NormalizeStoredReleaseLinks)
	return page, err
}

func normalizeKindPageHints(ctx context.Context, page *core.KindPage, normalize func(context.Context, []byte) ([]byte, error)) error {
	if normalize == nil || len(page.Items) == 0 {
		return nil
	}
	hints := make([]json.RawMessage, len(page.Items))
	for i, item := range page.Items {
		hints[i] = item.Hints
	}
	raw, err := json.Marshal(hints)
	if err != nil {
		return err
	}
	normalized, err := normalize(ctx, raw)
	if err != nil || bytes.Equal(raw, normalized) {
		return err
	}
	if err := json.Unmarshal(normalized, &hints); err != nil {
		return err
	}
	page.Items = slices.Clone(page.Items)
	for i := range page.Items {
		if len(page.Items[i].Hints) != 0 {
			page.Items[i].Hints = hints[i]
		}
	}
	return nil
}

// DefaultUsenetCheckPerMin bounds per-plugin on-demand usenet_check calls
// per minute so a resolver cannot flood Usenet indexers or NNTP providers.
const DefaultUsenetCheckPerMin = 20

type backgroundCallKey struct{}

// Bounds of an indexer_search typed lookup.
const (
	MaxSourceLookupFields     = 8
	MaxSourceLookupFieldBytes = 256
)

var lookupFieldRe = regexp.MustCompile(`^[a-z][a-zA-Z0-9_]{0,31}$`)

// ValidateSourceLookup checks an indexer_search lookup: it rides on a
// category search, names a simple type, and carries a few short fields.
func ValidateSourceLookup(q core.SourceQuery) *FieldError {
	l := q.Lookup
	if l == nil {
		return nil
	}
	switch {
	case strings.TrimSpace(q.Category) == "":
		return &FieldError{Field: "lookup", Message: "lookup needs a category: plugin sources answer typed lookups on category searches"}
	case !keyRe.MatchString(l.Type):
		return &FieldError{Field: "lookup.type", Message: "lookup.type must be a simple key such as album or book"}
	case len(l.Fields) > MaxSourceLookupFields:
		return &FieldError{Field: "lookup.fields", Message: fmt.Sprintf("at most %d lookup fields allowed", MaxSourceLookupFields)}
	}
	for k, v := range l.Fields {
		if !lookupFieldRe.MatchString(k) {
			return &FieldError{Field: "lookup.fields", Message: fmt.Sprintf("lookup field %q must be a simple camelCase name", k)}
		}
		if len(v) > MaxSourceLookupFieldBytes {
			return &FieldError{Field: "lookup.fields." + k, Message: fmt.Sprintf("must be at most %d bytes", MaxSourceLookupFieldBytes)}
		}
	}
	return nil
}

// SourcesPermissionAllowsSearch reports whether the declared permissions.sources
// level permits indexer search, release inspection, debrid cache checks, and
// on-demand Usenet checks.
func SourcesPermissionAllowsSearch(level string) bool {
	switch normLevel(level) {
	case "search", "search+check", "acquire", "acquire+check":
		return true
	default:
		return false
	}
}

// SourcesPermissionAllowsAcquire reports whether the declared permissions.sources
// level permits debrid_acquire.
func SourcesPermissionAllowsAcquire(level string) bool {
	switch normLevel(level) {
	case "acquire", "acquire+check":
		return true
	default:
		return false
	}
}

// SourcesPermissionAllowsBackgroundCheck reports whether the declared
// permissions.sources level permits background usenet_check calls within the
// per-plugin daily allowance.
func SourcesPermissionAllowsBackgroundCheck(level string) bool {
	switch normLevel(level) {
	case "search+check", "acquire+check":
		return true
	default:
		return false
	}
}

// WithBackgroundCall marks a plugin invocation context as background work
// (such as a scheduled job or catalog sync/expand). Background usenet_check
// calls require permissions.sources "search+check" or "acquire+check" and
// count against the per-plugin daily allowance.
func WithBackgroundCall(ctx context.Context) context.Context {
	return context.WithValue(wasm.WithBackgroundCall(ctx), backgroundCallKey{}, true)
}

func isBackgroundCall(ctx context.Context) bool {
	if bg, _ := ctx.Value(backgroundCallKey{}).(bool); bg {
		return true
	}
	return wasm.IsBackgroundCall(ctx)
}

func (h *host) ResolveSources(ctx context.Context, q core.SourceQuery) ([]core.SourceCandidate, error) {
	res, err := h.ResolveSourcesWithResult(ctx, q)
	if err != nil {
		return nil, err
	}
	return res.Candidates, nil
}

// ResolveSourcesWithResult runs ResolveSources and preserves per-indexer
// search status alongside candidates when the backend supports it (#733).
func (h *host) ResolveSourcesWithResult(ctx context.Context, q core.SourceQuery) (core.SourceResolveResult, error) {
	r, g, err := h.record(ctx)
	if err != nil {
		return core.SourceResolveResult{}, err
	}
	man := r.Manifest
	man.Ether = man.EffectiveEther()
	if !man.HasCapability(CapKindResolver) {
		h.noteDenied(string(CapKindResolver))
		return core.SourceResolveResult{}, fmt.Errorf("%w: %s", ErrPermissionDenied, CapKindResolver)
	}
	need := "search"
	switch q.Kind {
	case "indexer_search":
		if strings.TrimSpace(q.Query) == "" && strings.TrimSpace(q.Category) == "" {
			return core.SourceResolveResult{}, &ValidationError{Fields: []FieldError{{
				Field:   "query",
				Message: "indexer_search needs a query or category",
			}}}
		}
		if q.SettleMS != nil && *q.SettleMS < 0 {
			return core.SourceResolveResult{}, &ValidationError{Fields: []FieldError{{
				Field:   "settleMs",
				Message: "settleMs must be non-negative",
			}}}
		}
		if q.MinResults < 0 {
			return core.SourceResolveResult{}, &ValidationError{Fields: []FieldError{{
				Field:   "minResults",
				Message: "minResults must be non-negative",
			}}}
		}
		if q.TimeoutMS < 0 {
			return core.SourceResolveResult{}, &ValidationError{Fields: []FieldError{{
				Field:   "timeoutMs",
				Message: "timeoutMs must be non-negative",
			}}}
		}
		if len(q.SkipIndexers) > MaxSourceIndexerFilters {
			return core.SourceResolveResult{}, &ValidationError{Fields: []FieldError{{
				Field:   "skipIndexers",
				Message: fmt.Sprintf("at most %d skipIndexers entries allowed", MaxSourceIndexerFilters),
			}}}
		}
		if len(q.OnlyIndexers) > MaxSourceIndexerFilters {
			return core.SourceResolveResult{}, &ValidationError{Fields: []FieldError{{
				Field:   "onlyIndexers",
				Message: fmt.Sprintf("at most %d onlyIndexers entries allowed", MaxSourceIndexerFilters),
			}}}
		}
		if fe := ValidateSourceLookup(q); fe != nil {
			return core.SourceResolveResult{}, &ValidationError{Fields: []FieldError{*fe}}
		}
	case "indexer_search_results":
		if strings.TrimSpace(q.SearchID) == "" {
			return core.SourceResolveResult{}, &ValidationError{Fields: []FieldError{{
				Field:   "searchId",
				Message: "indexer_search_results needs a searchId",
			}}}
		}
		if q.WaitMS < 0 {
			return core.SourceResolveResult{}, &ValidationError{Fields: []FieldError{{
				Field:   "waitMs",
				Message: "waitMs must be non-negative",
			}}}
		}
	case "indexer_search_cancel":
		if strings.TrimSpace(q.SearchID) == "" {
			return core.SourceResolveResult{}, &ValidationError{Fields: []FieldError{{
				Field:   "searchId",
				Message: "indexer_search_cancel needs a searchId",
			}}}
		}
	case "indexers":
		// Lists the owner's enabled indexers (gated by the same search permission).
	case "debrid":
		// Reads the owner policy under the same permission as cache checks.
	case "debrid_check":
		if len(q.InfoHashes) == 0 && q.InfoHash == "" {
			return core.SourceResolveResult{}, &ValidationError{Fields: []FieldError{{
				Field:   "infoHashes",
				Message: "debrid_check needs an info hash",
			}}}
		}
	case "release_files":
		if q.InfoHash == "" && q.Magnet == "" && q.TorrentURL == "" && q.TorrentRef == "" && q.NZBURL == "" && q.NZBRef == "" && q.Candidate == nil {
			return core.SourceResolveResult{}, &ValidationError{Fields: []FieldError{{
				Field:   "infoHash",
				Message: "release_files needs a release reference, info hash, magnet or candidate",
			}}}
		}
	case "nzb_request":
		if strings.TrimSpace(q.NZBRef) == "" && (q.NZBIdentity == nil ||
			strings.TrimSpace(q.NZBIdentity.IndexerID) == "" || strings.TrimSpace(q.NZBIdentity.GUID) == "") {
			return core.SourceResolveResult{}, &ValidationError{Fields: []FieldError{{
				Field: "nzbRef", Message: "nzb_request needs an nzbRef or indexer release identity",
			}}}
		}
	case "usenet_check":
		nzbURL := strings.TrimSpace(q.NZBURL)
		if nzbURL == "" && q.Candidate != nil {
			nzbURL = strings.TrimSpace(q.Candidate.NZBURL)
		}
		if nzbURL == "" && q.NZBRef == "" && (q.Candidate == nil || q.Candidate.NZBRef == "") {
			return core.SourceResolveResult{}, &ValidationError{Fields: []FieldError{{
				Field:   "nzbUrl",
				Message: "usenet_check needs an nzbRef, candidate or legacy nzbUrl",
			}}}
		}
	case "debrid_acquire":
		need = "acquire"
		if q.InfoHash == "" && q.Magnet == "" && q.TorrentURL == "" && q.TorrentRef == "" && q.Candidate == nil {
			return core.SourceResolveResult{}, &ValidationError{Fields: []FieldError{{
				Field:   "infoHash",
				Message: "debrid_acquire needs an info hash, magnet, torrentUrl or candidate",
			}}}
		}
	default:
		return core.SourceResolveResult{}, &ValidationError{Fields: []FieldError{{
			Field:   "kind",
			Message: fmt.Sprintf("unknown source query kind %q", q.Kind),
		}}}
	}
	level := normLevel(r.Manifest.Ether.Permissions.Sources)
	if err := h.deny(r, g, "sources", level, func(l string) bool {
		return SourcesPermissionAllowsAcquire(l) || (need == "search" && SourcesPermissionAllowsSearch(l))
	}); err != nil {
		return core.SourceResolveResult{}, err
	}
	nzbQuery := q.Kind == "usenet_check" || q.Kind == "nzb_request"
	bgUsenetCheck := nzbQuery && isBackgroundCall(ctx)
	if bgUsenetCheck {
		if err := h.deny(r, g, "sources.check", level, SourcesPermissionAllowsBackgroundCheck); err != nil {
			return core.SourceResolveResult{}, err
		}
	}
	backends := h.m.kindBackends()
	if backends.ResolveSourcesResult == nil && backends.ResolveSources == nil {
		return core.SourceResolveResult{}, ErrUnavailable
	}
	callCtx := ctx
	var refundCheck func()
	var resetAt time.Time
	if nzbQuery {
		if bgUsenetCheck {
			// Reuse a live verdict from the shared cache before spending the
			// plugin's daily background allowance.
			if q.Kind == "usenet_check" && backends.PeekUsenetCheck != nil {
				if cand, ok := backends.PeekUsenetCheck(q); ok {
					return core.SourceResolveResult{Candidates: []core.SourceCandidate{pluginSourceCandidate(cand)}}, nil
				}
			}
			if backends.AdmitBackgroundUsenetCheck == nil {
				return core.SourceResolveResult{}, ErrUnavailable
			}
			var admitErr error
			callCtx, refundCheck, resetAt, admitErr = backends.AdmitBackgroundUsenetCheck(ctx, h.plugin)
			if admitErr != nil {
				if errors.Is(admitErr, core.ErrBudgetExhausted) {
					return core.SourceResolveResult{}, h.m.rateLimitError(
						"background usenet_check daily allowance exhausted",
						resetAt,
					)
				}
				return core.SourceResolveResult{}, pluginSourceError(admitErr)
			}
		} else {
			// On-demand viewer calls never count against the daily background
			// allowance; only the per-minute burst limit applies.
			limit := h.m.usenetCheckLimit
			if limit <= 0 {
				limit = DefaultUsenetCheckPerMin
			}
			if ok, retryAt := h.m.rateOKRetryAt(h.m.usenetCheckTimes, h.plugin, limit, time.Minute); !ok {
				return core.SourceResolveResult{}, h.m.rateLimitError(
					fmt.Sprintf("usenet_check is limited to %d per minute", limit),
					retryAt,
				)
			}
		}
	}
	var (
		res  core.SourceResolveResult
		rerr error
	)
	if backends.ResolveSourcesResult != nil {
		res, rerr = backends.ResolveSourcesResult(callCtx, h.plugin, q)
	} else {
		var cands []core.SourceCandidate
		cands, rerr = backends.ResolveSources(callCtx, h.plugin, q)
		res = core.SourceResolveResult{Candidates: cands}
	}
	if rerr != nil {
		if bgUsenetCheck {
			if errors.Is(rerr, core.ErrNotAvailable) && refundCheck != nil {
				refundCheck()
			}
			if errors.Is(rerr, core.ErrBudgetExhausted) {
				if refundCheck != nil {
					refundCheck()
				}
				return core.SourceResolveResult{}, h.m.rateLimitError(
					"background usenet_check daily allowance exhausted",
					resetAt,
				)
			}
		}
		return core.SourceResolveResult{}, pluginSourceError(rerr)
	}
	res.Candidates = slices.Clone(res.Candidates)
	for i := range res.Candidates {
		res.Candidates[i] = pluginSourceCandidate(res.Candidates[i])
	}
	return res, nil
}

// Keep indexer-owned locators on the host, including inside magnet parameters.
// The same response boundary serves hosted, HTTP, and tunnel plugins.
func pluginSourceCandidate(c core.SourceCandidate) core.SourceCandidate {
	if !strings.HasPrefix(c.NZBURL, core.PluginNZBRefPrefix) {
		c.NZBURL = ""
	}
	if !strings.HasPrefix(c.TorrentURL, core.PluginTorrentRefPrefix) {
		c.TorrentURL = ""
	}
	if c.Magnet != "" {
		hash := newznab.InfoHashFromMagnet(c.Magnet)
		c.Magnet = newznab.BareMagnet(hash)
	}
	return c
}

// Backend errors can contain an upstream response or request URL. Return
// fixed classifications and allow structured errors produced by the host.
func pluginSourceError(err error) error {
	var invalid *ValidationError
	var limit *RateLimitError
	if errors.As(err, &invalid) {
		return invalid
	}
	if errors.As(err, &limit) {
		return limit
	}
	for _, public := range []error{
		context.DeadlineExceeded, context.Canceled, ErrNotFound, ErrPermissionDenied,
		core.ErrBudgetExhausted, core.ErrReleaseRejected, core.ErrSourceUnavailable,
		core.ErrNotCached, core.ErrNotAvailable,
	} {
		if errors.Is(err, public) {
			return public
		}
	}
	return ErrUnavailable
}

func (h *host) CachePut(ctx context.Context, libraryKey, name string, r io.Reader) (string, error) {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return "", err
	}
	decl, err := h.cacheWriteCheck(ctx, libraryKey, name)
	if err != nil {
		return "", err
	}
	backend := h.m.kindBackends().CachePut
	if backend == nil {
		return "", ErrUnavailable
	}
	// A single blob can never exceed the whole per-library quota.
	limited := io.LimitReader(r, decl.Cache.EffectiveMaxBytes()+1)
	return backend(ctx, h.plugin, libraryKey, name, limited)
}

// viewerContextKey carries the authenticated viewer of a proxied request
// into a plugin call, so a host service can act as that viewer without the
// plugin passing (or being trusted with) the identity.
type viewerContextKey struct{}

// WithViewer attaches a viewer id to a plugin call context.
func WithViewer(ctx context.Context, viewerID int64) context.Context {
	return context.WithValue(ctx, viewerContextKey{}, viewerID)
}

// ViewerFromContext returns the viewer attached by WithViewer, if any.
func ViewerFromContext(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(viewerContextKey{}).(int64)
	return id, ok
}

type adapterOriginContextKey struct{}

// WithAdapterOrigin attaches the host-derived client-visible origin
// ("scheme://host[:port][/prefix]") for an active frontend adapter request (#772).
func WithAdapterOrigin(ctx context.Context, origin string) context.Context {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if origin == "" {
		return ctx
	}
	return context.WithValue(ctx, adapterOriginContextKey{}, origin)
}

// AdapterOriginFromContext returns the origin attached by WithAdapterOrigin, if any.
func AdapterOriginFromContext(ctx context.Context) (string, bool) {
	origin, ok := ctx.Value(adapterOriginContextKey{}).(string)
	return origin, ok && origin != ""
}

// AdapterKeyFromPath extracts the adapter key from a plugin-side frontend
// adapter route ("/ether/v1/frontend/<key>/...").
func AdapterKeyFromPath(path string) string {
	rest, ok := strings.CutPrefix(path, "/ether/v1/frontend/")
	if !ok || rest == "" {
		return ""
	}
	key, _, _ := strings.Cut(rest, "/")
	return key
}

// AdapterKeyFromContext returns the adapter key of the active frontend adapter
// call path attached to ctx, if any.
func AdapterKeyFromContext(ctx context.Context) (string, bool) {
	key := AdapterKeyFromPath(callPathFromContext(ctx))
	return key, key != ""
}

// AdapterCallInfo identifies the active /ether/v1/frontend/* call that minted
// an X-Ether-Viewer-Token so remote host callbacks enforce the same viewer,
// origin, and route policies as hosted plugins.
type AdapterCallInfo struct {
	ViewerID    int64
	Path        string
	SearchQuery string
	Origin      string
}

type adapterViewerGrant struct {
	pluginID    string
	viewerID    int64
	path        string
	searchQuery string
	origin      string
	expires     time.Time
}

func isFrontendAdapterPath(path string) bool {
	return isFrontendAdapterRoute(path) || isViewerScreenPath(path)
}

// isFrontendAdapterRoute is strictly a frontend adapter's own traffic.
func isFrontendAdapterRoute(path string) bool {
	return strings.HasPrefix(path, "/ether/v1/frontend/") && len(path) > len("/ether/v1/frontend/")
}

// isViewerScreenPath is a screen's viewer-scoped source or action call: like
// an adapter call, it carries the authenticated viewer so the plugin can use
// the viewer-bound host services (kinds.visible, kinds.state, its KV).
func isViewerScreenPath(path string) bool {
	return strings.HasPrefix(path, "/ether/v1/screens/") && len(path) > len("/ether/v1/screens/")
}

// bindAdapterViewer attaches the authenticated viewer from X-Ether-Viewer to
// ctx and mints a request-scoped X-Ether-Viewer-Token for remote callbacks,
// strictly on /ether/v1/frontend/* adapter calls. Non-adapter calls never
// receive a viewer context or token, and any pre-existing token header is
// stripped before binding.
func (m *Manager) bindAdapterViewer(ctx context.Context, pluginID string, req *Request, deadline time.Duration) (context.Context, func()) {
	if req != nil && req.Headers != nil {
		for k := range req.Headers {
			if strings.EqualFold(k, HeaderAdapterViewerToken) {
				delete(req.Headers, k)
			}
		}
	}
	if req == nil || !isFrontendAdapterPath(req.Path) || req.Headers == nil {
		return ctx, func() {}
	}
	raw := req.Headers["X-Ether-Viewer"]
	if raw == "" {
		return ctx, func() {}
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return ctx, func() {}
	}
	ctx = WithViewer(ctx, id)
	origin := strings.TrimRight(strings.TrimSpace(req.Headers[HeaderAdapterOrigin]), "/")
	if origin != "" {
		ctx = WithAdapterOrigin(ctx, origin)
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ctx, func() {}
	}
	token := hex.EncodeToString(b[:])
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	ttl := deadline
	if ttl < time.Minute {
		ttl = time.Minute
	}
	m.adapterViewerMu.Lock()
	if m.adapterViewerTokens == nil {
		m.adapterViewerTokens = make(map[string]adapterViewerGrant)
	}
	for k, v := range m.adapterViewerTokens {
		if !now.Before(v.expires) {
			delete(m.adapterViewerTokens, k)
		}
	}
	m.adapterViewerTokens[token] = adapterViewerGrant{
		pluginID:    pluginID,
		viewerID:    id,
		path:        req.Path,
		searchQuery: extractCallSearchQuery(*req),
		origin:      origin,
		expires:     now.Add(ttl),
	}
	m.adapterViewerMu.Unlock()
	req.Headers[HeaderAdapterViewerToken] = token
	return ctx, func() { m.revokeAdapterViewerToken(token) }
}

func (m *Manager) revokeAdapterViewerToken(token string) {
	if token == "" {
		return
	}
	m.adapterViewerMu.Lock()
	delete(m.adapterViewerTokens, token)
	m.adapterViewerMu.Unlock()
}

// ResolveAdapterCallToken verifies that token belongs to an active
// /ether/v1/frontend/* call for pluginID and returns its authenticated viewer
// and active route context.
func (m *Manager) ResolveAdapterCallToken(pluginID, token string) (AdapterCallInfo, bool) {
	token = strings.TrimSpace(token)
	if pluginID == "" || token == "" {
		return AdapterCallInfo{}, false
	}
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	m.adapterViewerMu.Lock()
	defer m.adapterViewerMu.Unlock()
	grant, ok := m.adapterViewerTokens[token]
	if !ok {
		return AdapterCallInfo{}, false
	}
	if !now.Before(grant.expires) {
		delete(m.adapterViewerTokens, token)
		return AdapterCallInfo{}, false
	}
	if grant.pluginID != pluginID || grant.viewerID <= 0 {
		return AdapterCallInfo{}, false
	}
	return AdapterCallInfo{
		ViewerID:    grant.viewerID,
		Path:        grant.path,
		SearchQuery: grant.searchQuery,
		Origin:      grant.origin,
	}, true
}

// ResolveAdapterViewerToken verifies that token belongs to an active
// /ether/v1/frontend/* call for pluginID and returns its authenticated viewer.
func (m *Manager) ResolveAdapterViewerToken(pluginID, token string) (int64, bool) {
	info, ok := m.ResolveAdapterCallToken(pluginID, token)
	if !ok {
		return 0, false
	}
	return info.ViewerID, true
}

const maxStreamSourceIDLen = 1024

func validateStreamSourceID(source string) error {
	if source == "" {
		return nil
	}
	if strings.TrimSpace(source) == "" {
		return &ValidationError{Fields: []FieldError{{Field: "source", Message: "stream url source must not be blank"}}}
	}
	if len(source) > maxStreamSourceIDLen {
		return &ValidationError{Fields: []FieldError{{Field: "source", Message: "stream url source exceeds maximum length"}}}
	}
	for _, r := range source {
		if r < 0x20 || r == 0x7f {
			return &ValidationError{Fields: []FieldError{{Field: "source", Message: "stream url source must not contain control characters"}}}
		}
	}
	return nil
}

// StreamURL mints a scoped stream ticket for one of the plugin's own kind
// items and returns the Ether stream URL (spec §6.5). The viewer is the one
// the proxied request authenticated, carried on the call context as
// X-Ether-Viewer. The backend re-checks that the item belongs to this plugin
// and that the viewer may see its library.
func (h *host) StreamURL(ctx context.Context, itemRef string) (string, error) {
	return h.StreamURLWithOptions(ctx, itemRef, StreamURLOptions{})
}

func (h *host) StreamPartURL(ctx context.Context, itemRef string, partIndex *int) (string, error) {
	return h.StreamURLWithOptions(ctx, itemRef, StreamURLOptions{PartIndex: partIndex})
}

func (h *host) StreamURLWithOptions(ctx context.Context, itemRef string, opts StreamURLOptions) (string, error) {
	rec, _, err := h.record(ctx)
	if err != nil {
		return "", err
	}
	man := rec.Manifest
	man.Ether = man.EffectiveEther()
	if man.Ether == nil || !man.HasCapability(CapFrontendAdapter) {
		h.noteDenied(string(CapFrontendAdapter))
		return "", fmt.Errorf("%w: %s", ErrPermissionDenied, CapFrontendAdapter)
	}
	viewerID, ok := ViewerFromContext(ctx)
	if !ok || viewerID <= 0 {
		return "", fmt.Errorf("%w: stream url needs an authenticated viewer", ErrPermissionDenied)
	}
	if strings.TrimSpace(itemRef) == "" {
		return "", fmt.Errorf("plugins: stream url needs an item ref")
	}
	if opts.PartIndex != nil && *opts.PartIndex < 0 {
		return "", fmt.Errorf("plugins: stream url partIndex must be non-negative")
	}
	if err := validateStreamSourceID(opts.Source); err != nil {
		return "", err
	}
	backends := h.m.kindBackends()
	if backends.StreamURLOptions != nil {
		return backends.StreamURLOptions(ctx, h.plugin, viewerID, itemRef, opts)
	}
	if opts.Source != "" || opts.Absolute {
		return "", ErrUnavailable
	}
	if backends.StreamPartURL != nil {
		return backends.StreamPartURL(ctx, h.plugin, viewerID, itemRef, opts.PartIndex)
	}
	if backends.StreamURL == nil {
		return "", ErrUnavailable
	}
	u, err := backends.StreamURL(ctx, h.plugin, viewerID, itemRef)
	if err != nil || opts.PartIndex == nil {
		return u, err
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "part=" + strconv.Itoa(*opts.PartIndex), nil
}

func validateBlobRef(blobRef string) error {
	hexPart, ok := strings.CutPrefix(blobRef, "blob:")
	if !ok || len(hexPart) != 64 {
		return &ValidationError{Fields: []FieldError{{Field: "blobRef", Message: "cache reference must be blob:<sha256>"}}}
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return &ValidationError{Fields: []FieldError{{Field: "blobRef", Message: "cache reference must be blob:<sha256>"}}}
	}
	return nil
}

func validateDurableBlobRef(ref string) error {
	if _, err := blobcache.ParseDurableRef(blobcache.Ref(ref)); err != nil {
		return &ValidationError{Fields: []FieldError{{Field: "blobRef", Message: "durable reference must be durable:<sha256>"}}}
	}
	return nil
}

func (h *host) CacheOpen(ctx context.Context, blobRef string) (io.ReadSeekCloser, error) {
	if strings.HasPrefix(blobRef, blobcache.DurableRefPrefix) {
		if err := validateDurableBlobRef(blobRef); err != nil {
			return nil, err
		}
		return h.openDurableBlob(ctx, blobRef)
	}
	r, err := h.callRecord(ctx)
	if err != nil {
		return nil, err
	}
	if !r.Enabled {
		return nil, ErrDisabled
	}
	if err := validateBlobRef(blobRef); err != nil {
		return nil, err
	}
	if s := h.m.deps.DurableStorage; s != nil {
		if src, err := s.Open(h.plugin, blobcache.Ref(blobRef)); !errors.Is(err, blobcache.ErrNotFound) {
			return src, storageError(err)
		}
	}
	backend := h.m.kindBackends().CacheOpen
	if backend == nil {
		return nil, ErrUnavailable
	}
	return backend(ctx, h.plugin, blobRef)
}

// CacheDelete removes one cached blob from this plugin's own library cache
// namespaces, freeing its quota immediately. Deleting a ref that is absent or
// already evicted succeeds.
func (h *host) CacheDelete(ctx context.Context, blobRef string) error {
	if strings.HasPrefix(blobRef, blobcache.DurableRefPrefix) {
		if err := validateDurableBlobRef(blobRef); err != nil {
			return err
		}
		return h.deleteDurableBlob(ctx, blobRef)
	}
	if err := h.requireWritableToolContext(ctx); err != nil {
		return err
	}
	rec, _, err := h.record(ctx)
	if err != nil {
		return err
	}
	man := rec.Manifest
	man.Ether = man.EffectiveEther()
	if man.Ether == nil || len(man.Ether.Contributes.MediaKinds) == 0 || !(man.HasCapability(CapCatalogProvider) || man.HasCapability(CapKindResolver)) {
		h.noteDenied("cache")
		return fmt.Errorf("%w: cache", ErrPermissionDenied)
	}
	if err := validateBlobRef(blobRef); err != nil {
		return err
	}
	backend := h.m.kindBackends().CacheDelete
	if backend == nil {
		return ErrUnavailable
	}
	return backend(ctx, h.plugin, blobRef)
}

func (h *host) cacheWriteCheck(ctx context.Context, libraryKey, name string) (MediaKindMeta, error) {
	rec, _, err := h.record(ctx)
	if err != nil {
		return MediaKindMeta{}, err
	}
	man := rec.Manifest
	man.Ether = man.EffectiveEther()
	// The library key must name one of this plugin's own kinds; the
	// blob cache namespace follows from it, so a plugin can never
	// write into another library's cache.
	var decl MediaKindMeta
	found := false
	if man.Ether != nil {
		for _, k := range man.Ether.Contributes.MediaKinds {
			if core.PluginLibraryKey(h.plugin, k.Key) == libraryKey {
				decl, found = k, true
			}
		}
	}
	if !found || !(man.HasCapability(CapCatalogProvider) || man.HasCapability(CapKindResolver)) {
		h.noteDenied("cache")
		return MediaKindMeta{}, fmt.Errorf("%w: cache", ErrPermissionDenied)
	}
	if strings.TrimSpace(name) == "" {
		return MediaKindMeta{}, fmt.Errorf("plugins: cache put needs a name")
	}
	return decl, nil
}

func (h *host) archiveExtractCheck(ctx context.Context, explicitLibraryKey string) (string, error) {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return "", err
	}
	if explicit := strings.TrimSpace(explicitLibraryKey); explicit != "" {
		if _, err := h.cacheWriteCheck(ctx, explicit, "_"); err != nil {
			return "", err
		}
		return explicit, nil
	}
	rec, _, err := h.record(ctx)
	if err != nil {
		return "", err
	}
	man := rec.Manifest
	man.Ether = man.EffectiveEther()
	if man.Ether == nil || len(man.Ether.Contributes.MediaKinds) == 0 || !(man.HasCapability(CapCatalogProvider) || man.HasCapability(CapKindResolver)) {
		h.noteDenied("cache")
		return "", fmt.Errorf("%w: cache", ErrPermissionDenied)
	}
	return core.PluginLibraryKey(h.plugin, man.Ether.Contributes.MediaKinds[0].Key), nil
}

func (h *host) resolveExtractTarget(ctx context.Context, explicitLibraryKey, sourceLibraryKey, name string) (string, int64, error) {
	if explicit := strings.TrimSpace(explicitLibraryKey); explicit != "" {
		decl, err := h.cacheWriteCheck(ctx, explicit, name)
		if err != nil {
			return "", 0, err
		}
		return explicit, decl.Cache.EffectiveMaxBytes(), nil
	}
	if src := strings.TrimSpace(sourceLibraryKey); src != "" {
		if decl, err := h.cacheWriteCheck(ctx, src, name); err == nil {
			return src, decl.Cache.EffectiveMaxBytes(), nil
		}
	}
	libKey, err := h.archiveExtractCheck(ctx, "")
	if err != nil {
		return "", 0, err
	}
	decl, err := h.cacheWriteCheck(ctx, libKey, name)
	if err != nil {
		return "", 0, err
	}
	return libKey, decl.Cache.EffectiveMaxBytes(), nil
}
