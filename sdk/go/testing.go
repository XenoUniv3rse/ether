package etherplugin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var (
	mockBadgeSetKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	mockBadgeIMDBRe   = regexp.MustCompile(`^tt[0-9]{1,12}$`)
)

func normalizeMockBadgeLabel(raw string) (string, error) {
	if len(raw) < 1 || len(raw) > MaxBadgeLabelLen {
		return "", fmt.Errorf("badge label must be 1 to %d characters", MaxBadgeLabelLen)
	}
	buf := make([]byte, len(raw))
	nonSpace := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c >= 'a' && c <= 'z' {
			c = c - 'a' + 'A'
		}
		switch {
		case c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '+', c == '&', c == '\'', c == '.', c == '/', c == '-':
			nonSpace = true
		case c == ' ':
		default:
			return "", fmt.Errorf("badge label must contain only [A-Z0-9 +&'./-]")
		}
		buf[i] = c
	}
	if !nonSpace {
		return "", fmt.Errorf("badge label must not be blank")
	}
	return string(buf), nil
}

func resolveMockBadgeTitleKey(ref BadgeTitleRef, catalog map[string]CatalogTitleSummary) (string, error) {
	typ := strings.TrimSpace(ref.Type)
	if typ != "movie" && typ != "show" {
		return "", fmt.Errorf("badge title.type must be movie or show")
	}
	if ref.TMDB < 0 {
		return "", fmt.Errorf("badge title.tmdb must be positive")
	}
	imdb := strings.ToLower(strings.TrimSpace(ref.IMDB))
	if imdb != "" && !mockBadgeIMDBRe.MatchString(imdb) {
		return "", fmt.Errorf("badge title.imdb must match tt<digits>")
	}
	if ref.TMDB == 0 && imdb == "" {
		return "", fmt.Errorf("badge title requires tmdb or imdb")
	}
	if ref.TMDB > 0 {
		for _, item := range catalog {
			if item.Type == typ && int64(item.TMDBID) == ref.TMDB {
				return fmt.Sprint(item.ID), nil
			}
		}
	}
	if imdb != "" {
		for _, item := range catalog {
			if item.Type == typ && strings.EqualFold(item.IMDBID, imdb) {
				return fmt.Sprint(item.ID), nil
			}
		}
	}
	if ref.TMDB > 0 {
		return fmt.Sprintf("tmdb:%s:%d", typ, ref.TMDB), nil
	}
	return fmt.Sprintf("imdb:%s:%s", typ, imdb), nil
}

func countDistinctMockBadgeTitles(sets map[string]map[string][]string) int64 {
	seen := map[string]struct{}{}
	for _, setMap := range sets {
		for k := range setMap {
			seen[k] = struct{}{}
		}
	}
	return int64(len(seen))
}

// RecordedLog is one log message recorded by MockHost.
type RecordedLog struct {
	Level   string
	Message string
	Attrs   map[string]any
}

// RecordedPublishedEvent is one plugin event recorded by MockHost.PublishEvent.
type RecordedPublishedEvent struct {
	Key         string
	Payload     map[string]any
	Correlation *EventCorrelation
}

// MockHost is an in-memory Host implementation for unit tests.
type MockHost struct {
	mu                sync.RWMutex
	KV                map[string]string
	Badges            map[string]map[string][]string
	BadgeQuotaTitles  int64
	Secrets           map[string]string
	Catalog           map[string]CatalogTitleSummary
	History           map[int64][]ViewerWatchEntry
	WatchState        map[string]bool
	Collections       []map[string]any
	Kinds             map[string][]KindItem      // kindKey -> pushed items, in upsert order
	KindHiddenIDs     map[string]map[string]bool // kindKey -> externalID -> hidden by mock library Rules (#615)
	KindPolicyFilter  func(kindKey string, item KindItem) bool
	Sources           []SourceCandidate     // returned by ResolveSources
	SourceIndexers    []IndexerSearchStatus // returned alongside Sources by ResolveSourcesWithResult (#733)
	SourceHandler     func(ctx context.Context, q SourceQuery) (SourceResolveResult, error)
	Blobs             map[string][]byte // cache blob ref -> bytes
	FolderRoots       []string
	Folders           map[string][]FolderEntry
	PublishedEvents   []RecordedPublishedEvent
	Notifications     []string
	Logs              []RecordedLog
	FetchHandler      func(ctx context.Context, req HostFetchRequest) (HostFetchResponse, error)
	AIHandler         func(ctx context.Context, req AICompleteRequest) (AICompleteResponse, error)
	MediaProbeHandler func(ctx context.Context, src PlayableSource) (MediaProbeResult, error)
	// LastStreamURLViewer/Item/Part/Source/Absolute record the most recent
	// StreamURL, StreamPartURL, or StreamURLWithOptions call.
	LastStreamURLViewer   int64
	LastStreamURLItem     string
	LastStreamURLPart     *int
	LastStreamURLSource   string
	LastStreamURLAbsolute bool
	LastStreamURLWait     *StreamWaitPolicy
	// StreamOrigin is prepended to mock stream URLs when Absolute is true
	// (defaults to "http://ether.test").
	StreamOrigin string
}

// NewMockHost creates an initialized MockHost.
func NewMockHost() *MockHost {
	return &MockHost{
		KV:               map[string]string{},
		Badges:           map[string]map[string][]string{},
		BadgeQuotaTitles: PosterBadgeQuotaTitles,
		Secrets:          map[string]string{},
		Catalog:          map[string]CatalogTitleSummary{},
		History:          map[int64][]ViewerWatchEntry{},
		WatchState:       map[string]bool{},
		Folders:          map[string][]FolderEntry{},
		Collections:      []map[string]any{},
		Kinds:            map[string][]KindItem{},
		KindHiddenIDs:    map[string]map[string]bool{},
		Blobs:            map[string][]byte{},
	}
}

// KVGet reads a key from the in-memory KV store.
func (m *MockHost) KVGet(_ context.Context, key string) (string, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.KV[key]
	return v, ok, nil
}

// KVPut writes a key-value pair to the in-memory KV store.
func (m *MockHost) KVPut(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.KV[key] = value
	return nil
}

// KVDelete deletes a key from the in-memory KV store.
func (m *MockHost) KVDelete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.KV, key)
	return nil
}

// KVList lists keys matching prefix in sorted order.
func (m *MockHost) KVList(_ context.Context, prefix string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var keys []string
	for k := range m.KV {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// KVUsage returns in-memory KV usage matching the host's 256-byte key charge.
func (m *MockHost) KVUsage(_ context.Context) (KVUsage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var used int64
	for _, v := range m.KV {
		used += int64(len(v))
	}
	keys := int64(len(m.KV))
	return KVUsage{
		Bytes:      used + keys*256,
		Keys:       keys,
		QuotaBytes: 50 << 20,
		QuotaKeys:  10000,
	}, nil
}

func (m *MockHost) badgeQuotaLocked() int64 {
	if m.BadgeQuotaTitles > 0 {
		return m.BadgeQuotaTitles
	}
	return PosterBadgeQuotaTitles
}

// BadgesSet stores or clears (empty Labels) poster badges in memory, enforcing
// the same label and per-plugin title bounds as Ether (#594).
func (m *MockHost) BadgesSet(_ context.Context, setKey string, entries []BadgeEntry) error {
	if !mockBadgeSetKeyRe.MatchString(setKey) {
		return fmt.Errorf("badge set key must match ^[a-z][a-z0-9_]{0,31}$")
	}
	if len(entries) > MaxBadgeEntriesPerCall {
		return fmt.Errorf("badges.set accepts at most %d entries per call", MaxBadgeEntriesPerCall)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Badges == nil {
		m.Badges = map[string]map[string][]string{}
	}
	nextSet := map[string][]string{}
	for k, v := range m.Badges[setKey] {
		nextSet[k] = append([]string{}, v...)
	}
	hasUpsert := false
	for _, e := range entries {
		if len(e.Labels) > MaxBadgeLabelsPerTitle {
			return fmt.Errorf("badge entry may have at most %d labels per title", MaxBadgeLabelsPerTitle)
		}
		titleKey, err := resolveMockBadgeTitleKey(e.Title, m.Catalog)
		if err != nil {
			return err
		}
		if len(e.Labels) == 0 {
			delete(nextSet, titleKey)
			continue
		}
		var clean []string
		seen := make(map[string]bool, len(e.Labels))
		for _, raw := range e.Labels {
			norm, err := normalizeMockBadgeLabel(raw)
			if err != nil {
				return err
			}
			if !seen[norm] {
				seen[norm] = true
				clean = append(clean, norm)
			}
		}
		nextSet[titleKey] = clean
		hasUpsert = true
	}
	preview := make(map[string]map[string][]string, len(m.Badges)+1)
	for k, v := range m.Badges {
		preview[k] = v
	}
	if len(nextSet) == 0 {
		delete(preview, setKey)
	} else {
		preview[setKey] = nextSet
	}
	quota := m.badgeQuotaLocked()
	if hasUpsert {
		if titles := countDistinctMockBadgeTitles(preview); titles > quota {
			return fmt.Errorf("poster badge quota exceeded (%d > %d titles)", titles, quota)
		}
	}
	if len(nextSet) == 0 {
		delete(m.Badges, setKey)
	} else {
		m.Badges[setKey] = nextSet
	}
	return nil
}

// BadgesClear removes every badge in setKey from the in-memory store (#594).
func (m *MockHost) BadgesClear(_ context.Context, setKey string) error {
	if !mockBadgeSetKeyRe.MatchString(setKey) {
		return fmt.Errorf("badge set key must match ^[a-z][a-z0-9_]{0,31}$")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Badges, setKey)
	return nil
}

// BadgesUsage returns the distinct badged title count and configured quota (#594).
func (m *MockHost) BadgesUsage(_ context.Context) (BadgeUsage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return BadgeUsage{
		Titles:      countDistinctMockBadgeTitles(m.Badges),
		QuotaTitles: m.badgeQuotaLocked(),
	}, nil
}

// SecretGet reads a secret from the in-memory secrets map.
func (m *MockHost) SecretGet(_ context.Context, name string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Secrets[name], nil
}

// SecretSet stores (or deletes when empty) a derived secret in the in-memory secrets map.
func (m *MockHost) SecretSet(_ context.Context, name, value string, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if value == "" {
		delete(m.Secrets, name)
		return nil
	}
	if m.Secrets == nil {
		m.Secrets = map[string]string{}
	}
	m.Secrets[name] = value
	return nil
}

// SecretDelete removes a derived secret from the in-memory secrets map.
func (m *MockHost) SecretDelete(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Secrets, name)
	return nil
}

// Fetch invokes FetchHandler if set, or returns a 200 "{}" response.
func (m *MockHost) Fetch(ctx context.Context, req HostFetchRequest) (HostFetchResponse, error) {
	if m.FetchHandler != nil {
		return m.FetchHandler(ctx, req)
	}
	return HostFetchResponse{
		Status:  http.StatusOK,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    "{}",
	}, nil
}

// FetchMany executes reqs concurrently through Fetch with bounded concurrency
// and per-item error isolation, returning results in input order.
func (m *MockHost) FetchMany(ctx context.Context, reqs []HostFetchRequest, opts ...HostFetchManyOptions) ([]HostFetchManyResult, error) {
	var opt HostFetchManyOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	return runSDKFetchMany(ctx, reqs, opt, m.Fetch)
}

// CatalogSearch searches the in-memory catalog by case-insensitive substring.
func (m *MockHost) CatalogSearch(_ context.Context, query string, limit, offset int) ([]CatalogTitleSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	needle := strings.ToLower(query)
	var matches []CatalogTitleSummary
	for _, item := range m.Catalog {
		if strings.Contains(strings.ToLower(item.Title), needle) {
			matches = append(matches, item)
		}
	}
	if offset >= len(matches) {
		return nil, nil
	}
	end := len(matches)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return matches[offset:end], nil
}

// CatalogGet returns a title by ID from the in-memory catalog.
func (m *MockHost) CatalogGet(_ context.Context, id string) (*CatalogTitleSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if item, ok := m.Catalog[id]; ok {
		cp := item
		return &cp, nil
	}
	return nil, nil
}

// ViewerHistory returns watch history for viewerID.
func (m *MockHost) ViewerHistory(_ context.Context, viewerID int64, limit, offset int) ([]ViewerWatchEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := m.History[viewerID]
	if offset >= len(list) {
		return nil, nil
	}
	end := len(list)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return list[offset:end], nil
}

// SetWatchState records watch state for viewerID:titleID.
func (m *MockHost) SetWatchState(_ context.Context, viewerID int64, titleID string, watched bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.WatchState[fmt.Sprintf("%d:%s", viewerID, titleID)] = watched
	return nil
}

// UpsertCollection appends the collection to Collections.
func (m *MockHost) UpsertCollection(_ context.Context, collection map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Collections = append(m.Collections, collection)
	return nil
}

// FoldersRoots returns the configured mock grants.
func (m *MockHost) FoldersRoots(context.Context) (FolderRoots, error) {
	return append([]string{}, m.FolderRoots...), nil
}

// FoldersList returns folder entries for root/path.
func (m *MockHost) FoldersList(_ context.Context, root, path string, options ...FolderListOptions) (FolderPage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := root
	if path != "" {
		key = root + "/" + path
	}
	opts := FolderListOptions{}
	if len(options) > 0 {
		opts = options[0]
	}
	limit := opts.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	page := FolderPage{Entries: make([]FolderEntry, 0)}
	for _, entry := range m.Folders[key] {
		if entry.Name > opts.Cursor {
			page.Entries = append(page.Entries, entry)
		}
	}
	sort.Slice(page.Entries, func(i, j int) bool { return page.Entries[i].Name < page.Entries[j].Name })
	if len(page.Entries) > limit {
		page.Entries = page.Entries[:limit]
		page.NextCursor = page.Entries[limit-1].Name
		page.Truncated = true
	}
	return page, nil
}

// FoldersStat returns a FolderEntry for root and path.
func (m *MockHost) FoldersStat(_ context.Context, root, path string) (FolderEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, f := range m.Folders[root] {
		if f.Path == path || f.Name == path {
			return f, nil
		}
	}
	return FolderEntry{Name: path, Path: path}, nil
}

// PublishEvent records one published plugin event on MockHost.
func (m *MockHost) PublishEvent(_ context.Context, key string, payload any, corr *EventCorrelation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var doc map[string]any
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("event payload must be a JSON object: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}
	var corrCopy *EventCorrelation
	if corr != nil {
		cp := *corr
		corrCopy = &cp
	}
	m.PublishedEvents = append(m.PublishedEvents, RecordedPublishedEvent{
		Key:         key,
		Payload:     doc,
		Correlation: corrCopy,
	})
	return nil
}

// NotifyOwner appends text to Notifications.
func (m *MockHost) NotifyOwner(_ context.Context, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Notifications = append(m.Notifications, text)
	return nil
}

// AIComplete invokes AIHandler if set, or returns a *HostError with Code "not_configured".
func (m *MockHost) AIComplete(ctx context.Context, req AICompleteRequest) (AICompleteResponse, error) {
	if m.AIHandler != nil {
		return m.AIHandler(ctx, req)
	}
	return AICompleteResponse{}, &HostError{
		Status:  http.StatusNotImplemented,
		Code:    "not_configured",
		Message: "Language model is not configured",
	}
}

// MediaProbe invokes MediaProbeHandler if set, or returns a *HostError with Code "unavailable".
func (m *MockHost) MediaProbe(ctx context.Context, src PlayableSource) (MediaProbeResult, error) {
	if m.MediaProbeHandler != nil {
		return m.MediaProbeHandler(ctx, src)
	}
	return MediaProbeResult{}, &HostError{
		Status:  http.StatusServiceUnavailable,
		Code:    "unavailable",
		Message: "media.probe is not configured in mock host",
	}
}

// Log appends a RecordedLog entry to Logs.
func (m *MockHost) Log(_ context.Context, level, message string, attrs map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Logs = append(m.Logs, RecordedLog{Level: level, Message: message, Attrs: attrs})
	return nil
}

// KindUpsert records pushed items under their kind, replacing same-id items.
func (m *MockHost) KindUpsert(_ context.Context, kindKey string, items []KindItem) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Kinds == nil {
		m.Kinds = map[string][]KindItem{}
	}
	for _, it := range items {
		replaced := false
		for i, old := range m.Kinds[kindKey] {
			if old.ID == it.ID {
				m.Kinds[kindKey][i] = it
				replaced = true
			}
		}
		if !replaced {
			m.Kinds[kindKey] = append(m.Kinds[kindKey], it)
		}
	}
	return len(items), nil
}

// KindDelete drops recorded items by external id.
func (m *MockHost) KindDelete(_ context.Context, kindKey string, externalIDs []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	drop := map[string]bool{}
	for _, id := range externalIDs {
		drop[id] = true
	}
	kept := m.Kinds[kindKey][:0]
	for _, it := range m.Kinds[kindKey] {
		if !drop[it.ID] {
			kept = append(kept, it)
		}
	}
	m.Kinds[kindKey] = kept
	return nil
}

// KindItems pages recorded items; the cursor is the last item id seen.
func (m *MockHost) KindItems(_ context.Context, kindKey, cursor string, limit int) (KindPage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if limit <= 0 {
		limit = 200
	}
	var out []KindItem
	after := cursor == ""
	for _, it := range m.Kinds[kindKey] {
		if !after {
			after = it.ID == cursor
			continue
		}
		out = append(out, it)
		if len(out) == limit {
			return KindPage{Items: out, NextCursor: it.ID}, nil
		}
	}
	if out == nil {
		out = []KindItem{}
	}
	return KindPage{Items: out}, nil
}

func (m *MockHost) mockKindItemAllowed(kindKey string, item KindItem) bool {
	if hidden := m.KindHiddenIDs[kindKey]; hidden != nil && hidden[item.ID] {
		return false
	}
	if m.KindPolicyFilter != nil && !m.KindPolicyFilter(kindKey, item) {
		return false
	}
	return true
}

// KindVisible filters ids against MockHost.Kinds, KindHiddenIDs, and
// KindPolicyFilter (#615). Requires a viewer token on ctx (WithViewerToken).
func (m *MockHost) KindVisible(ctx context.Context, kindKey string, ids []string) ([]string, error) {
	if _, ok := ViewerTokenFromContext(ctx); !ok {
		return nil, fmt.Errorf("kinds.visible: no authenticated frontend adapter viewer token on context")
	}
	if len(ids) > MaxKindVisibleIDs {
		return nil, fmt.Errorf("kinds.visible: batch larger than %d ids", MaxKindVisibleIDs)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	byID := make(map[string]KindItem, len(m.Kinds[kindKey]))
	for _, it := range m.Kinds[kindKey] {
		byID[it.ID] = it
	}
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		it, ok := byID[id]
		if !ok || !m.mockKindItemAllowed(kindKey, it) {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// KindBrowse filters and pages MockHost.Kinds with KindHiddenIDs and
// KindPolicyFilter applied (#615). Requires a viewer token on ctx.
func (m *MockHost) KindBrowse(ctx context.Context, kindKey string, query KindBrowseQuery) (KindPage, error) {
	if _, ok := ViewerTokenFromContext(ctx); !ok {
		return KindPage{}, fmt.Errorf("kinds.browse: no authenticated frontend adapter viewer token on context")
	}
	switch query.Total {
	case "", "cheap", "none", "exact":
	default:
		return KindPage{}, fmt.Errorf("kinds.browse: invalid total mode %q", query.Total)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	limit := query.Limit
	if limit <= 0 {
		limit = 200
	}
	needle := strings.ToLower(strings.TrimSpace(query.Query))
	excludeNeedle := strings.ToLower(strings.TrimSpace(query.ExcludeQuery))
	var matched []KindItem
	for _, it := range m.Kinds[kindKey] {
		if !m.mockKindItemAllowed(kindKey, it) {
			continue
		}
		if query.ParentID != "" && it.ParentID != query.ParentID {
			continue
		}
		if query.Level != "" && it.Level != query.Level {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(it.Title+" "+it.Summary), needle) {
			continue
		}
		if excludeNeedle != "" && strings.Contains(strings.ToLower(it.Title+" "+it.Summary), excludeNeedle) {
			continue
		}
		if len(query.Facets) > 0 && !mockItemMatchesFacets(it, query.Facets) {
			continue
		}
		if len(query.AllFacets) > 0 && !mockItemMatchesAllFacets(it, query.AllFacets) {
			continue
		}
		if len(query.ExcludeFacets) > 0 && mockItemMatchesAnyFacet(it, query.ExcludeFacets) {
			continue
		}
		if len(query.AnyGroups) > 0 && !mockItemMatchesAnyGroups(it, query.AnyGroups) {
			continue
		}
		if len(query.Empty) > 0 && !mockItemMatchesEmpty(it, query.Empty) {
			continue
		}
		matched = append(matched, it)
	}
	if query.Desc {
		for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
			matched[i], matched[j] = matched[j], matched[i]
		}
	}
	var totalPtr *int
	if query.Total != "none" {
		n := len(matched)
		totalPtr = &n
	}
	var out []KindItem
	after := query.Cursor == ""
	for i, it := range matched {
		if !after {
			after = it.ID == query.Cursor
			continue
		}
		out = append(out, it)
		if len(out) == limit {
			next := ""
			if i+1 < len(matched) {
				next = it.ID
			}
			return KindPage{Items: out, NextCursor: next, Total: totalPtr}, nil
		}
	}
	if out == nil {
		out = []KindItem{}
	}
	return KindPage{Items: out, Total: totalPtr}, nil
}

func mockItemMatchesFacets(item KindItem, facets map[string][]string) bool {
	for k, want := range facets {
		if len(want) == 0 {
			continue
		}
		raw, ok := item.Fields[k]
		if !ok || len(raw) == 0 {
			return false
		}
		var vals []string
		var single string
		if json.Unmarshal(raw, &vals) != nil {
			if json.Unmarshal(raw, &single) == nil && single != "" {
				vals = []string{single}
			}
		}
		if len(vals) == 0 {
			return false
		}
		found := false
		for _, w := range want {
			for _, h := range vals {
				if strings.EqualFold(w, h) {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func mockItemMatchesAnyFacet(item KindItem, facets map[string][]string) bool {
	for k, want := range facets {
		if len(want) == 0 {
			continue
		}
		if mockItemMatchesFacets(item, map[string][]string{k: want}) {
			return true
		}
	}
	return false
}

func mockItemMatchesAllFacets(item KindItem, facets map[string][]string) bool {
	for k, want := range facets {
		for _, w := range want {
			if w == "" {
				continue
			}
			if !mockItemMatchesFacets(item, map[string][]string{k: {w}}) {
				return false
			}
		}
	}
	return true
}

func mockItemMatchesAnyGroups(item KindItem, groups []KindBrowseFacetGroup) bool {
	for _, g := range groups {
		var alts []KindBrowseFacetAlt
		if fk := strings.TrimSpace(g.Field); fk != "" && len(g.Values) > 0 {
			alts = append(alts, KindBrowseFacetAlt{Field: fk, Values: g.Values})
		}
		alts = append(alts, g.Any...)
		var active []KindBrowseFacetAlt
		for _, alt := range alts {
			fk := strings.TrimSpace(alt.Field)
			if fk == "" {
				continue
			}
			var want []string
			for _, v := range alt.Values {
				if s := strings.TrimSpace(v); s != "" {
					want = append(want, s)
				}
			}
			if len(want) > 0 {
				active = append(active, KindBrowseFacetAlt{Field: fk, Values: want})
			}
		}
		if len(active) == 0 {
			continue
		}
		groupHit := false
		for _, alt := range active {
			if mockItemMatchesFacets(item, map[string][]string{alt.Field: alt.Values}) {
				groupHit = true
				break
			}
		}
		if !groupHit {
			return false
		}
	}
	return true
}

func mockItemMatchesEmpty(item KindItem, empty map[string]bool) bool {
	for k, wantEmpty := range empty {
		hasVal := false
		if raw, ok := item.Fields[k]; ok && len(raw) > 0 {
			var arr []any
			var s string
			switch {
			case json.Unmarshal(raw, &arr) == nil:
				for _, el := range arr {
					if str, ok := el.(string); ok {
						if str != "" {
							hasVal = true
							break
						}
					} else if el != nil {
						hasVal = true
						break
					}
				}
			case json.Unmarshal(raw, &s) == nil:
				hasVal = s != ""
			default:
				hasVal = string(raw) != "null"
			}
		} else {
			switch k {
			case "released":
				hasVal = item.Released != 0
			case "year", "decade":
				hasVal = item.Year > 0
			case "rating":
				hasVal = item.Rating > 0
			case "summary":
				hasVal = item.Summary != ""
			case "recording":
				hasVal = item.Recording != ""
			case "parentId":
				hasVal = item.ParentID != ""
			case "index":
				hasVal = item.Index > 0
			}
		}
		if wantEmpty && hasVal {
			return false
		}
		if !wantEmpty && !hasVal {
			return false
		}
	}
	return true
}

// ResolveSources returns the canned Sources (or calls SourceHandler if set).
func (m *MockHost) ResolveSources(ctx context.Context, q SourceQuery) ([]SourceCandidate, error) {
	res, err := m.ResolveSourcesWithResult(ctx, q)
	if err != nil {
		return nil, err
	}
	return res.Candidates, nil
}

// ResolveSourcesWithResult returns the canned Sources and SourceIndexers (or
// delegates to SourceHandler when set) (#733).
func (m *MockHost) ResolveSourcesWithResult(ctx context.Context, q SourceQuery) (SourceResolveResult, error) {
	m.mu.RLock()
	handler := m.SourceHandler
	cands := append([]SourceCandidate{}, m.Sources...)
	idx := append([]IndexerSearchStatus(nil), m.SourceIndexers...)
	m.mu.RUnlock()
	if handler != nil {
		return handler(ctx, q)
	}
	return SourceResolveResult{Candidates: cands, Indexers: idx}, nil
}

// CachePut stores bytes under a sha256 blob ref, like the real cache.
func (m *MockHost) CachePut(_ context.Context, _, _ string, data []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Blobs == nil {
		m.Blobs = map[string][]byte{}
	}
	ref := fmt.Sprintf("blob:%x", sha256.Sum256(data))
	m.Blobs[ref] = append([]byte{}, data...)
	return ref, nil
}

// CacheOpen reads stored blob bytes back.
func (m *MockHost) CacheOpen(_ context.Context, blobRef string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.Blobs[blobRef]
	if !ok {
		return nil, fmt.Errorf("blob %q not found", blobRef)
	}
	return append([]byte{}, b...), nil
}

// CacheDelete removes a stored blob from memory; deleting an absent ref succeeds.
func (m *MockHost) CacheDelete(_ context.Context, blobRef string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Blobs, blobRef)
	return nil
}

// StreamURL mints a deterministic stand-in URL and records the last call, so
// a plugin's frontend adapter can be tested without a real ticket issuer.
func (m *MockHost) StreamURL(_ context.Context, viewerID int64, itemRef string) (string, error) {
	return m.StreamURLWithOptions(context.Background(), viewerID, itemRef, StreamURLOptions{})
}

// StreamPartURL mints a deterministic part-scoped stand-in URL and records the
// last call (#576).
func (m *MockHost) StreamPartURL(_ context.Context, viewerID int64, itemRef string, partIndex int) (string, error) {
	if partIndex < 0 {
		return "", fmt.Errorf("stream url: partIndex must be non-negative")
	}
	return m.StreamURLWithOptions(context.Background(), viewerID, itemRef, StreamURLOptions{PartIndex: &partIndex})
}

// StreamURLWithOptions mints a deterministic stand-in URL and records the
// viewer, item, optional part index, and optional source id (#576, #661).
func (m *MockHost) StreamURLWithOptions(_ context.Context, viewerID int64, itemRef string, opts StreamURLOptions) (string, error) {
	if opts.Wait != nil && opts.Wait.MaxSeconds <= 0 {
		return "", fmt.Errorf("stream url: wait.maxSeconds must be a positive integer")
	}
	if opts.PartIndex != nil && *opts.PartIndex < 0 {
		return "", fmt.Errorf("stream url: partIndex must be non-negative")
	}
	if opts.Source != "" && strings.TrimSpace(opts.Source) == "" {
		return "", fmt.Errorf("stream url: source must not be blank")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LastStreamURLViewer = viewerID
	m.LastStreamURLItem = itemRef
	if opts.PartIndex != nil {
		cp := *opts.PartIndex
		m.LastStreamURLPart = &cp
	} else {
		m.LastStreamURLPart = nil
	}
	m.LastStreamURLSource = opts.Source
	m.LastStreamURLAbsolute = opts.Absolute
	m.LastStreamURLWait = nil
	if opts.Wait != nil {
		wait := *opts.Wait
		m.LastStreamURLWait = &wait
	}
	u := "/ether/v1/kind-media/mock-" + strconv.FormatInt(viewerID, 10)
	if opts.PartIndex != nil {
		u += "?part=" + strconv.Itoa(*opts.PartIndex)
	}
	if opts.Absolute {
		origin := strings.TrimRight(strings.TrimSpace(m.StreamOrigin), "/")
		if origin == "" {
			origin = "http://ether.test"
		}
		return origin + u, nil
	}
	return u, nil
}

// AssertValidViewSpec validates a ViewSpecDocument for structural rules and
// rejects any `*ClassName` fields.
func AssertValidViewSpec(doc ViewSpecDocument) error {
	if doc.SpecVersion != 1 {
		return fmt.Errorf("ViewSpecDocument specVersion must be 1 (got %d)", doc.SpecVersion)
	}
	switch doc.Layout {
	case "list", "detail", "bare":
	default:
		return fmt.Errorf("ViewSpecDocument layout must be list, detail, or bare (got %q)", doc.Layout)
	}
	if doc.Header == nil {
		return fmt.Errorf("ViewSpecDocument header must not be nil")
	}
	if doc.Body == nil {
		return fmt.Errorf("ViewSpecDocument body must not be nil")
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	return checkNoClassName(raw, "spec")
}

func checkNoClassName(v any, path string) error {
	switch t := v.(type) {
	case []any:
		for i, item := range t {
			if err := checkNoClassName(item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case map[string]any:
		for k, val := range t {
			if k == "className" || strings.HasSuffix(k, "ClassName") {
				return fmt.Errorf("ViewSpec forbids %s at %s.%s", k, path, k)
			}
			if err := checkNoClassName(val, path+"."+k); err != nil {
				return err
			}
		}
	}
	return nil
}

// Fixtures provides sample EPP/1 records for plugin unit tests.
var Fixtures = struct {
	PickToken     func(itemID string) string
	Movie         func() MetaItem
	CatalogItem   func() CatalogItem
	PlaybackEvent func(name string) EtherEvent
	Candidate     func() CandidateFacts
}{
	PickToken: func(itemID string) string {
		if itemID == "" {
			itemID = "ether:movie:1"
		}
		return "pick-v1:" + itemID + ":sig-test"
	},
	Movie: func() MetaItem {
		return MetaItem{
			CatalogItem: CatalogItem{
				ID:          "tt0133093",
				Type:        "movie",
				Name:        "The Matrix",
				ReleaseInfo: "1999",
				Description: "A computer hacker learns about the true nature of reality.",
				Genres:      []string{"Action", "Sci-Fi"},
				IMDBRating:  "8.7",
			},
		}
	},
	CatalogItem: func() CatalogItem {
		return CatalogItem{
			ID:          "tt0133093",
			Type:        "movie",
			Name:        "The Matrix",
			ReleaseInfo: "1999",
		}
	},
	PlaybackEvent: func(name string) EtherEvent {
		if name == "" {
			name = "playback.completed"
		}
		return EtherEvent{
			ID:       "evt_01",
			Name:     name,
			At:       "2026-11-02T12:00:00Z",
			ViewerID: 1,
			Payload: map[string]any{
				"titleId":  "tt0133093",
				"title":    "The Matrix",
				"progress": 0.95,
			},
		}
	},
	Candidate: func() CandidateFacts {
		return CandidateFacts{
			ReleaseID:  "rel_01",
			Name:       "The.Matrix.1999.2160p.UHD.BluRay.x265",
			Resolution: 2160,
			HDR:        []string{"DV", "HDR10"},
			VideoCodec: "hevc",
			Cached:     true,
			Verified:   true,
		}
	},
}

// TestHarness wraps a Plugin and MockHost for ergonomic unit tests.
type TestHarness struct {
	Plugin *Plugin
	Host   *MockHost
}

// NewTestHarness constructs a TestHarness for p.
func NewTestHarness(p *Plugin, host *MockHost) *TestHarness {
	if host == nil {
		if mh, ok := p.host.(*MockHost); ok {
			host = mh
		} else {
			host = NewMockHost()
		}
	}
	p.host = host
	return &TestHarness{Plugin: p, Host: host}
}

func encodeExtraSegment(extra map[string]string) string {
	if len(extra) == 0 {
		return ""
	}
	vals := url.Values{}
	for k, v := range extra {
		vals.Set(k, v)
	}
	return "/" + vals.Encode()
}

// Request dispatches a RawRequest against the plugin.
func (h *TestHarness) Request(req RawRequest) RawResponse {
	return h.Plugin.Dispatch(context.Background(), req)
}

// Manifest fetches and decodes GET /manifest.json.
func (h *TestHarness) Manifest() (Manifest, error) {
	res := h.Request(RawRequest{Method: http.MethodGet, Path: "/manifest.json"})
	var m Manifest
	err := json.Unmarshal(res.Body, &m)
	return m, err
}

// Health calls GET /ether/health.
func (h *TestHarness) Health() (int, HealthResponse, error) {
	res := h.Request(RawRequest{Method: http.MethodGet, Path: "/ether/health"})
	var out HealthResponse
	err := json.Unmarshal(res.Body, &out)
	return res.Status, out, err
}

// Configure calls POST /ether/configure.
func (h *TestHarness) Configure(settings map[string]any) (int, ConfigureResponse, error) {
	b, _ := json.Marshal(settings)
	res := h.Request(RawRequest{Method: http.MethodPost, Path: "/ether/configure", Body: b})
	var out ConfigureResponse
	err := json.Unmarshal(res.Body, &out)
	return res.Status, out, err
}

// Test calls POST /ether/test.
func (h *TestHarness) Test() (int, TestResponse, error) {
	res := h.Request(RawRequest{Method: http.MethodPost, Path: "/ether/test"})
	var out TestResponse
	err := json.Unmarshal(res.Body, &out)
	return res.Status, out, err
}

// Options calls GET /ether/options/{field}.
func (h *TestHarness) Options(field string) (int, []FieldOption, error) {
	res := h.Request(RawRequest{Method: http.MethodGet, Path: "/ether/options/" + url.PathEscape(field)})
	var out []FieldOption
	err := json.Unmarshal(res.Body, &out)
	return res.Status, out, err
}

// Catalog calls GET /catalog/{type}/{id}[/{extra}].json.
func (h *TestHarness) Catalog(typ, id string, extra map[string]string) (int, CatalogResponse, error) {
	suffix := encodeExtraSegment(extra)
	path := fmt.Sprintf("/catalog/%s/%s%s.json", url.PathEscape(typ), url.PathEscape(id), suffix)
	res := h.Request(RawRequest{Method: http.MethodGet, Path: path})
	var out CatalogResponse
	err := json.Unmarshal(res.Body, &out)
	return res.Status, out, err
}

// Meta calls GET /meta/{type}/{id}.json.
func (h *TestHarness) Meta(typ, id string) (int, MetaResponse, error) {
	path := fmt.Sprintf("/meta/%s/%s.json", url.PathEscape(typ), url.PathEscape(id))
	res := h.Request(RawRequest{Method: http.MethodGet, Path: path})
	var out MetaResponse
	err := json.Unmarshal(res.Body, &out)
	return res.Status, out, err
}

// Stream calls GET /stream/{type}/{id}[/{extra}].json.
func (h *TestHarness) Stream(typ, id string, extra map[string]string) (int, StreamResponse, error) {
	suffix := encodeExtraSegment(extra)
	path := fmt.Sprintf("/stream/%s/%s%s.json", url.PathEscape(typ), url.PathEscape(id), suffix)
	res := h.Request(RawRequest{Method: http.MethodGet, Path: path})
	var out StreamResponse
	err := json.Unmarshal(res.Body, &out)
	return res.Status, out, err
}

// Subtitles calls GET /subtitles/{type}/{id}[/{extra}].json with optional pickToken.
func (h *TestHarness) Subtitles(typ, id, pickToken string, extra map[string]string) (int, SubtitlesResponse, RawResponse) {
	suffix := encodeExtraSegment(extra)
	path := fmt.Sprintf("/subtitles/%s/%s%s.json", url.PathEscape(typ), url.PathEscape(id), suffix)
	hdrs := map[string]string{}
	if pickToken != "" {
		hdrs["X-Ether-Pick"] = pickToken
	}
	res := h.Request(RawRequest{Method: http.MethodGet, Path: path, Headers: hdrs})
	var out SubtitlesResponse
	_ = json.Unmarshal(res.Body, &out)
	return res.Status, out, res
}

// Markers calls GET /ether/markers/{type}/{id}.json.
func (h *TestHarness) Markers(typ, id string) (int, MarkersResponse, error) {
	path := fmt.Sprintf("/ether/markers/%s/%s.json", url.PathEscape(typ), url.PathEscape(id))
	res := h.Request(RawRequest{Method: http.MethodGet, Path: path})
	var out MarkersResponse
	err := json.Unmarshal(res.Body, &out)
	return res.Status, out, err
}

// Event calls POST /ether/event.
func (h *TestHarness) Event(events []EtherEvent) (int, EventResponse, error) {
	b, _ := json.Marshal(map[string]any{"events": events})
	res := h.Request(RawRequest{Method: http.MethodPost, Path: "/ether/event", Body: b})
	var out EventResponse
	err := json.Unmarshal(res.Body, &out)
	return res.Status, out, err
}

// View calls GET /ether/view/{page} and validates the returned ViewSpecDocument.
func (h *TestHarness) View(page string, query map[string]string) (int, ViewResponse, error) {
	clean := strings.TrimLeft(page, "/")
	if clean == "" {
		clean = "home"
	}
	res := h.Request(RawRequest{
		Method: http.MethodGet,
		Path:   "/ether/view/" + url.PathEscape(clean),
		Query:  query,
	})
	var out ViewResponse
	if err := json.Unmarshal(res.Body, &out); err != nil {
		return res.Status, out, err
	}
	if res.Status == http.StatusOK {
		if err := AssertValidViewSpec(out.Spec); err != nil {
			return res.Status, out, err
		}
	}
	return res.Status, out, nil
}

// Action calls POST /ether/action/{action}.
func (h *TestHarness) Action(action string, input map[string]any) (int, ActionResponse, error) {
	b, _ := json.Marshal(input)
	res := h.Request(RawRequest{
		Method: http.MethodPost,
		Path:   "/ether/action/" + url.PathEscape(action),
		Body:   b,
	})
	var out ActionResponse
	err := json.Unmarshal(res.Body, &out)
	return res.Status, out, err
}

// FlowAction calls a plugin flow action route with X-Ether-Idempotency-Key.
func (h *TestHarness) FlowAction(key string, req FlowActionRequest) (int, FlowActionResponse, error) {
	route := "/ether/v1/flow-actions/" + url.PathEscape(key)
	if m := h.Plugin.Manifest(); m.Ether != nil && m.Ether.Contributes != nil {
		for _, fa := range m.Ether.Contributes.FlowActions {
			if fa.Key == key && fa.Route != "" {
				route = fa.Route
				break
			}
		}
	}
	b, _ := json.Marshal(req)
	hdrs := map[string]string{}
	if req.IdempotencyKey != "" {
		hdrs[HeaderIdempotencyKey] = req.IdempotencyKey
	}
	res := h.Request(RawRequest{
		Method:  http.MethodPost,
		Path:    route,
		Headers: hdrs,
		Body:    b,
	})
	var out FlowActionResponse
	err := json.Unmarshal(res.Body, &out)
	return res.Status, out, err
}

// Tool calls POST /ether/tool/{name}.
func (h *TestHarness) Tool(name string, input map[string]any) (int, ToolResponse, error) {
	b, _ := json.Marshal(input)
	res := h.Request(RawRequest{
		Method: http.MethodPost,
		Path:   "/ether/tool/" + url.PathEscape(name),
		Body:   b,
	})
	var out ToolResponse
	err := json.Unmarshal(res.Body, &out)
	return res.Status, out, err
}

// File calls GET /ether/file/{path}.
func (h *TestHarness) File(path, rangeHeader string) RawResponse {
	hdrs := map[string]string{}
	if rangeHeader != "" {
		hdrs["Range"] = rangeHeader
	}
	return h.Request(RawRequest{
		Method:  http.MethodGet,
		Path:    "/ether/file/" + strings.TrimLeft(path, "/"),
		Headers: hdrs,
	})
}

func (*MockHost) FoldersHash(context.Context, string, string, ...FolderHashOptions) (FolderHash, error) {
	return FolderHash{}, fmt.Errorf("folders.hash requires the hosted runtime")
}

func (*MockHost) FoldersHashStart(context.Context, string, string, ...FolderHashOptions) (FolderHashJob, error) {
	return FolderHashJob{}, fmt.Errorf("folders.hash requires the hosted runtime")
}

func (*MockHost) FoldersFrame(context.Context, string, string, FolderFrameOptions) (FolderFrame, error) {
	return FolderFrame{}, fmt.Errorf("folders.frame requires the hosted runtime")
}

func (*MockHost) FoldersHashPoll(context.Context, string) (FolderHashJob, error) {
	return FolderHashJob{}, fmt.Errorf("folders.hash requires the hosted runtime")
}

func (*MockHost) FoldersHashCancel(context.Context, string) (FolderHashJob, error) {
	return FolderHashJob{}, fmt.Errorf("folders.hash requires the hosted runtime")
}
