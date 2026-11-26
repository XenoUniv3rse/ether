package adapters

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/archive"
	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/kindstream"
	"github.com/braedonsaunders/ether/internal/libraries"
	"github.com/braedonsaunders/ether/internal/logredact"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/resolver"
)

// maxRecentSourceCandidates bounds category-only recent-release searches
// (empty query with category set) to the newest 100 candidates.
const maxRecentSourceCandidates = 100

// DebridSourceBackend is the subset of debrid.Service used by KindResolver for
// host-mediated debrid cache checks and torrent acquisition.
type DebridSourceBackend interface {
	CheckCached(ctx context.Context, hashes []string) (map[string]bool, bool)
	Acquire(ctx context.Context, rel core.Release) ([]byte, error)
	ListReleaseFiles(ctx context.Context, rel core.Release) ([]core.ReleaseFile, error)
}

type debridCachedByBackend interface {
	CheckCachedBy(ctx context.Context, hashes []string) (map[string][]core.DebridServiceRef, bool)
}

type debridKnownCachedByBackend interface {
	KnownCachedBy(hash string, hintKinds []string) []core.DebridServiceRef
}

type debridServiceRefBackend interface {
	ServiceRef(id, kind string) (core.DebridServiceRef, bool)
}

type debridDownloadQueuer interface {
	QueueDownload(ctx context.Context, rel core.Release) ([]byte, error)
}

type debridConfigBackend interface {
	Config() config.Debrid
}

const (
	maxKindQueueAttempts      = 3
	defaultDebridPrepareRetry = 5 * time.Second
)

type releaseQueueModeKey struct{}

func withReleaseQueueMode(ctx context.Context) context.Context {
	return context.WithValue(ctx, releaseQueueModeKey{}, true)
}

func withoutReleaseQueueMode(ctx context.Context) context.Context {
	return context.WithValue(ctx, releaseQueueModeKey{}, false)
}

func isReleaseQueueMode(ctx context.Context) bool {
	v, _ := ctx.Value(releaseQueueModeKey{}).(bool)
	return v
}

const (
	maxKindCandidateCacheEntries = 512
	defaultKindCandidateCacheTTL = 15 * time.Minute
)

// KindResolverDeps configures a KindResolver.
type KindResolverDeps struct {
	Manager    *plugins.Manager
	Libraries  *libraries.Service
	Items      *pluginitems.Service
	Blobs      *blobcache.Cache
	Catalog    *KindCatalog
	Indexer    core.Indexer
	PluginNZBs PluginNZBStore
	// Sources, when set, accounts plugin-release opens (#792) against the
	// source plugin's grab limits.
	Sources       *StreamSources
	Resolver      *resolver.Service
	Debrid        DebridSourceBackend
	Engine        core.Engine
	UsenetServing func() bool
	Now           func() time.Time
	HTTP          *http.Client
	Authorize     func(ctx context.Context, viewer *core.Viewer, key core.RatingKey) error
	ProbeCache    MediaProbeCache
	ProbeStream   func(ctx context.Context, inputURL string) (*core.MediaInfo, error)
	Logger        *slog.Logger
	// Optional release-open budget overrides (0 uses sensible production defaults).
	NZBFetchTimeout      time.Duration
	EngineOpenTimeout    time.Duration
	CandidateOpenTimeout time.Duration
	// PreparingWait bounds how long an open waits server-side while a
	// plugin's staged resolve answers pending (W348); 0 answers preparing
	// at once. The app sets DefaultKindPreparingWait.
	PreparingWait time.Duration
}

const (
	defaultKindNZBFetchTimeout      = 20 * time.Second
	defaultKindEngineOpenTimeout    = 25 * time.Second
	defaultKindCandidateOpenTimeout = 30 * time.Second
	speculativeSourceBudgetCooloff  = 5 * time.Minute
	kindDetailMissCooloff           = 5 * time.Minute
)

type resolveFlightKey struct {
	ref        core.ItemRef
	part       int
	source     string
	background bool
	variant    string
}

type resolveFlight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	sources []core.PlayableSource
	err     error
	keep    bool
	expires time.Time
}

type cachedKindCandidates struct {
	sources   []core.PlayableSource
	expiresAt time.Time
	storedAt  time.Time
}

// KindResolver implements frontend.KindStreams and frontend.KindPartStreams for
// plugin content kinds and supplies the host-side ResolveSources callback for
// plugins.KindBackends.
type KindResolver struct {
	mgr                  *plugins.Manager
	libs                 *libraries.Service
	items                *pluginitems.Service
	blobs                *blobcache.Cache
	catalog              *KindCatalog
	indexer              core.Indexer
	pluginNZBs           PluginNZBStore
	sources              *StreamSources
	resolver             *resolver.Service
	debrid               DebridSourceBackend
	engine               core.Engine
	usenetServing        func() bool
	now                  func() time.Time
	http                 *http.Client
	authorize            func(ctx context.Context, viewer *core.Viewer, key core.RatingKey) error
	probeCache           MediaProbeCache
	probeStream          func(ctx context.Context, inputURL string) (*core.MediaInfo, error)
	backoff              *kindstream.HostBackoff
	log                  *slog.Logger
	nzbFetchTimeout      time.Duration
	engineOpenTimeout    time.Duration
	candidateOpenTimeout time.Duration
	preparingWait        time.Duration

	unitMu    sync.Mutex
	unitGates map[resolveUnitKey]*resolveUnitGate

	flightMu     sync.Mutex
	flights      map[resolveFlightKey]*resolveFlight
	flightClosed bool
	flightWG     sync.WaitGroup

	candMu          sync.Mutex
	candCache       map[resolveFlightKey]cachedKindCandidates
	detailMissUntil map[resolveFlightKey]time.Time

	specBudgetMu    sync.Mutex
	specBudgetUntil map[string]time.Time

	usenetVerdicts *resolver.UsenetVerdictCache
	grabRejects    kindGrabRejects

	streamMu           sync.Mutex
	streamSearches     map[string]*streamSearchSession
	streamClosed       bool
	streamMaxPerPlugin int
	streamMaxGlobal    int
	streamIdleTTL      time.Duration
	streamFirstPollTTL time.Duration
	streamMaxWait      time.Duration
	streamCeiling      time.Duration
	streamWG           sync.WaitGroup

	sourceFlightMu      sync.Mutex
	sourceFlights       map[string]*sourceWorkFlight
	sourceFlightsClosed bool
	sourceFlightWG      sync.WaitGroup

	releaseFilesMu    sync.Mutex
	releaseFilesCache map[string]cachedReleaseFiles
	sourceHandoffs    map[string]sourceHandoffEntry

	progressMu      sync.Mutex
	resolveProgress map[resolveFlightKey]*resolveProgressEntry
}

var (
	_ frontend.KindStreams     = (*KindResolver)(nil)
	_ frontend.KindPartStreams = (*KindResolver)(nil)
)

func defaultKindHTTPClient() *http.Client {
	if dt, ok := http.DefaultTransport.(*http.Transport); ok && dt != nil {
		tr := dt.Clone()
		tr.ResponseHeaderTimeout = 30 * time.Second
		return &http.Client{Transport: tr}
	}
	return &http.Client{}
}

// NewKindResolver constructs a KindResolver.
func NewKindResolver(deps KindResolverDeps) *KindResolver {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	hc := deps.HTTP
	if hc == nil {
		hc = defaultKindHTTPClient()
	}
	nowFn := deps.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	nzbFetchTimeout := deps.NZBFetchTimeout
	if nzbFetchTimeout <= 0 {
		nzbFetchTimeout = defaultKindNZBFetchTimeout
	}
	engineOpenTimeout := deps.EngineOpenTimeout
	if engineOpenTimeout <= 0 {
		engineOpenTimeout = defaultKindEngineOpenTimeout
	}
	candidateOpenTimeout := deps.CandidateOpenTimeout
	if candidateOpenTimeout <= 0 {
		candidateOpenTimeout = defaultKindCandidateOpenTimeout
	}
	usenetVerdicts := resolver.NewUsenetVerdictCache()
	if deps.Resolver != nil {
		if shared := deps.Resolver.UsenetVerdicts(); shared != nil {
			usenetVerdicts = shared
		}
	}
	r := &KindResolver{
		mgr:                  deps.Manager,
		libs:                 deps.Libraries,
		items:                deps.Items,
		blobs:                deps.Blobs,
		catalog:              deps.Catalog,
		indexer:              deps.Indexer,
		pluginNZBs:           deps.PluginNZBs,
		sources:              deps.Sources,
		resolver:             deps.Resolver,
		debrid:               deps.Debrid,
		engine:               deps.Engine,
		usenetServing:        deps.UsenetServing,
		usenetVerdicts:       usenetVerdicts,
		now:                  nowFn,
		http:                 hc,
		authorize:            deps.Authorize,
		probeCache:           deps.ProbeCache,
		probeStream:          deps.ProbeStream,
		backoff:              kindstream.NewHostBackoff(),
		log:                  log.With("component", "kindresolver"),
		nzbFetchTimeout:      nzbFetchTimeout,
		engineOpenTimeout:    engineOpenTimeout,
		candidateOpenTimeout: candidateOpenTimeout,
		preparingWait:        deps.PreparingWait,
		flights:              make(map[resolveFlightKey]*resolveFlight),
		candCache:            make(map[resolveFlightKey]cachedKindCandidates),
		detailMissUntil:      make(map[resolveFlightKey]time.Time),
		specBudgetUntil:      make(map[string]time.Time),
		streamSearches:       make(map[string]*streamSearchSession),
	}
	if deps.Manager != nil {
		deps.Manager.OnSync(func(_ context.Context, rec plugins.Record) {
			r.InvalidatePlugin(rec.ID)
		})
		deps.Manager.OnRemove(func(_ context.Context, id string) {
			r.InvalidatePlugin(id)
		})
	}
	return r
}

// InvalidatePlugin cancels any in-flight resolves tied to pluginID's previous
// runtime and evicts its in-memory candidate cache so disabling/re-enabling or
// reloading a plugin starts from a clean slate.
func (r *KindResolver) InvalidatePlugin(pluginID string) {
	if r == nil || pluginID == "" {
		return
	}
	prefix := "pk:" + pluginID + ":"
	var cancels []context.CancelFunc
	r.flightMu.Lock()
	for fk, fl := range r.flights {
		if strings.HasPrefix(string(fk.ref), prefix) {
			delete(r.flights, fk)
			if fl.cancel != nil {
				cancels = append(cancels, fl.cancel)
			}
		}
	}
	r.flightMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}

	r.candMu.Lock()
	for fk := range r.candCache {
		if strings.HasPrefix(string(fk.ref), prefix) {
			delete(r.candCache, fk)
		}
	}
	for fk := range r.detailMissUntil {
		if strings.HasPrefix(string(fk.ref), prefix) {
			delete(r.detailMissUntil, fk)
		}
	}
	r.candMu.Unlock()

	r.clearPluginResolveProgress(pluginID)
	r.cancelPluginStreamingSearches(pluginID)
}

// OpenKind implements frontend.KindStreams.
func (r *KindResolver) OpenKind(ctx context.Context, viewer *core.Viewer, key core.RatingKey, accept []string) (*frontend.KindMedia, error) {
	return r.OpenKindExcluding(ctx, viewer, key, accept, nil)
}

// ResolveForMatch performs a fresh plugin /resolve call for a playable item
// (clearing any stale cached candidates first) without opening or downloading
// bytes. Used by the owner console's Match action so books and other plugin
// items reach the resolver and indexer search just like playback/reading does.
func (r *KindResolver) ResolveForMatch(ctx context.Context, lib core.Library, rec pluginitems.KindRecord, item core.KindItem, accept []string) ([]core.PlayableSource, error) {
	if r == nil || r.mgr == nil {
		return nil, core.ErrSourceUnavailable
	}
	if item.Unavailable != nil {
		return nil, core.ErrNotAvailable
	}
	lv, _, ok := rec.Decl.Level(item.Level)
	if !ok || !lv.Playable || !item.Playable {
		return nil, nil
	}
	if core.IndexerPurpose(ctx) == core.IndexerPurposeOther {
		ctx = core.WithIndexerPurpose(ctx, core.IndexerPurposePlayback)
	}
	var partIdx *int
	if len(core.ExtractKindItemPartsFromItem(item)) > 0 {
		zero := 0
		partIdx = &zero
	}
	fk := flightKeyForItem(item.Ref, partIdx)
	r.deleteCandidates(fk)
	sources, err := r.resolveItemSources(ctx, rec, lib, item, partIdx, accept)
	if err == nil && len(sources) > 0 && r.items != nil {
		if _, _, rerr := r.items.GetPartResolution(ctx, item.Ref, partIdx); errors.Is(rerr, core.ErrNotAvailable) {
			_ = r.items.InvalidatePartResolution(ctx, item.Ref, partIdx)
		}
	}
	return sources, err
}

func singlePartIndex(item core.KindItem) (*int, bool) {
	parts := core.ExtractKindItemPartsFromItem(item)
	switch len(parts) {
	case 0:
		return nil, true
	case 1:
		zero := 0
		return &zero, true
	default:
		return nil, false
	}
}

func (r *KindResolver) projectedTitle(ctx context.Context, item core.KindItem, rec pluginitems.KindRecord, lib core.Library) *core.Title {
	var resolve pluginitems.TextResolver
	if r != nil && r.mgr != nil {
		resolve = r.mgr.ResolveTextFunc(ctx, rec.PluginID, "")
	}
	t := pluginitems.ProjectWithText(item, rec.Decl, lib.Present, resolve, lib.CardShape)
	if (t.Type == core.TypeEpisode || t.Type == core.TypeSeason) && item.ParentID != "" {
		cur := item.ParentID
		for depth := range 8 {
			parent, _, err := r.items.GetByRef(ctx, core.NewItemRef(rec.PluginID, rec.KindKey, cur))
			if err != nil {
				break
			}
			// The immediate parent of an episode in a three-level kind is its
			// season container: carry its number and name.
			if depth == 0 && t.Type == core.TypeEpisode && parent.ParentID != "" {
				if t.Season == 0 {
					t.Season = parent.Index
				}
				if t.SeasonTitle == "" {
					t.SeasonTitle = parent.Title
				}
				if t.ParentKey == 0 {
					t.ParentKey = parent.Key
					t.ParentTitle = parent.Title
				}
			}
			if parent.ParentID == "" {
				t.ShowKey = parent.Key
				t.ShowTitle = parent.Title
				break
			}
			cur = parent.ParentID
		}
	}
	return &t
}

func (r *KindResolver) openSingleSource(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, partIdx *int, targetSrc core.PlayableSource, allowQueue bool) (*frontend.KindMedia, error) {
	ksDeps := r.kindstreamDeps(ctx, rec, lib, item, partIdx, nil)
	bs, err := kindstream.Open(ctx, targetSrc, ksDeps)
	if err != nil {
		if allowQueue && isTorrentReleaseSource(targetSrc) && errors.Is(err, core.ErrNotCached) {
			if qbs, _, qerr, settled := r.settleUncachedReleases(ctx, []kindstream.SourceFailure{{Source: targetSrc, Err: err}}, ksDeps); settled {
				if qerr != nil {
					return nil, qerr
				}
				bs, err = qbs, nil
			}
		}
		if err != nil {
			r.recordKindFailure(ctx, item, err)
			return nil, err
		}
	}
	parts := r.itemParts(ctx, item)
	var dur time.Duration
	if partIdx != nil && *partIdx < len(parts) {
		bs = withPartDefaults(bs, parts[*partIdx], item.Title)
		if parts[*partIdx].DurationSec > 0 {
			dur = time.Duration(parts[*partIdx].DurationSec * float64(time.Second))
		}
	}
	if dur == 0 && len(parts) <= 1 {
		dur = extractItemDuration(item, rec.Decl)
	}
	if dur == 0 && targetSrc.DurationSec > 0 {
		dur = time.Duration(targetSrc.DurationSec * float64(time.Second))
	}
	rel := playableSourceRelease(targetSrc, item.Title)
	if rel.Size <= 0 {
		rel.Size = bs.Size()
	}
	if rel.Name == "" {
		rel.Name = firstNonEmpty(bs.Name(), item.Title)
	}
	return &frontend.KindMedia{
		ReaderAt:    bs,
		Closer:      bs,
		Size:        bs.Size(),
		MIME:        bs.MIME(),
		Duration:    dur,
		Name:        bs.Name(),
		Release:     rel,
		MediaSource: unwrapKindMediaSource(bs),
	}, nil
}

func matchPlayableSource(sources []core.PlayableSource, defaultTitle, sourceID string, nowUnix int64) (core.PlayableSource, bool) {
	if sourceID == "" {
		return core.PlayableSource{}, false
	}
	for _, src := range sources {
		if src.ExpiresAt > 0 && nowUnix >= src.ExpiresAt {
			continue
		}
		if src.ID == sourceID {
			return src, true
		}
	}
	for _, src := range sources {
		if src.ExpiresAt > 0 && nowUnix >= src.ExpiresAt {
			continue
		}
		if playableSourceRelease(src, defaultTitle).ID == sourceID {
			return src, true
		}
	}
	return core.PlayableSource{}, false
}

// OpenKindSource opens exactly one plugin kind source by its stable candidate
// release id (a version-slot pick) or PlayableSource.ID. It reuses the
// in-memory and persisted resolution candidate caches so a warm list does not
// re-run the plugin, opens only the matching candidate, and leaves the shared
// default winner untouched. A previously opened NZB can also be reused
// from the vault after the candidate cache expires.
func (r *KindResolver) OpenKindSource(ctx context.Context, viewer *core.Viewer, key core.RatingKey, sourceID string, accept []string) (*frontend.KindMedia, error) {
	if r.waitsPreparing(ctx) {
		return r.waitPreparing(ctx, func(ctx context.Context) (*frontend.KindMedia, error) {
			return r.OpenKindSource(ctx, viewer, key, sourceID, accept)
		})
	}
	if r == nil || r.items == nil || r.libs == nil || !key.IsPluginKey() || sourceID == "" {
		return nil, core.ErrNotFound
	}
	if core.IndexerPurpose(ctx) == core.IndexerPurposeOther {
		ctx = core.WithIndexerPurpose(ctx, core.IndexerPurposePlayback)
	}
	var forcedPart *int
	if parentKey, pIdx, isPart := key.PluginPart(); isPart {
		key = parentKey
		idx := pIdx
		forcedPart = &idx
	}
	item, lib, rec, err := r.loadAuthorized(ctx, viewer, key)
	if err != nil {
		return nil, err
	}
	if item.Unavailable != nil {
		return nil, core.ErrNotAvailable
	}
	lv, _, ok := rec.Decl.Level(item.Level)
	if !ok || !lv.Playable || !item.Playable {
		return nil, core.ErrNotAvailable
	}
	var partIdx *int
	if forcedPart != nil {
		parts := r.itemParts(ctx, item)
		if len(parts) > 0 && *forcedPart >= len(parts) {
			return nil, core.ErrNotFound
		}
		partIdx = forcedPart
	} else {
		partIdx, ok = singlePartIndex(item)
		if !ok {
			// Multi-part items expose a single composite sequence, not per-release versions.
			return nil, core.ErrNotAvailable
		}
	}
	if media, err := r.openSavedKindSource(ctx, rec, lib, item, partIdx, sourceID); media != nil || err != nil {
		return media, err
	}

	sources, err := r.resolveCandidates(ctx, rec, lib, item, partIdx, accept)
	if err != nil {
		return nil, err
	}
	src, matched := matchPlayableSource(sources, item.Title, sourceID, r.now().Unix())
	if !matched {
		return nil, core.ErrNotAvailable
	}
	return r.openSingleSource(ctx, rec, lib, item, partIdx, src, true)
}

// OpenKindPartSource opens exactly one resolver source (and optional zero-based
// part) for a source-scoped frontend adapter stream ticket (#661). It passes
// KindResolveRequest.Source to /resolve, opens only the matching source via
// openSingleSource with no failover to other sources, and leaves the shared
// default winner and candidate caches untouched. A plugin that ignores
// KindResolveRequest.Source still works: Ether picks the source with the
// matching PlayableSource.ID from the response, or returns ErrNotAvailable if
// it is absent.
func (r *KindResolver) OpenKindPartSource(ctx context.Context, viewer *core.Viewer, key core.RatingKey, partIndex *int, sourceID string, accept []string) (*frontend.KindMedia, error) {
	if r.waitsPreparing(ctx) {
		return r.waitPreparing(ctx, func(ctx context.Context) (*frontend.KindMedia, error) {
			return r.OpenKindPartSource(ctx, viewer, key, partIndex, sourceID, accept)
		})
	}
	if r == nil || r.items == nil || r.libs == nil || !key.IsPluginKey() || sourceID == "" {
		return nil, core.ErrNotFound
	}
	if partIndex != nil && *partIndex < 0 {
		return nil, core.ErrNotFound
	}
	if core.IndexerPurpose(ctx) == core.IndexerPurposeOther {
		ctx = core.WithIndexerPurpose(ctx, core.IndexerPurposePlayback)
	}
	item, lib, rec, err := r.loadAuthorized(ctx, viewer, key)
	if err != nil {
		return nil, err
	}
	if item.Unavailable != nil {
		return nil, core.ErrNotAvailable
	}
	lv, _, ok := rec.Decl.Level(item.Level)
	if !ok {
		return nil, core.ErrNotFound
	}

	targetItem := item
	var effectivePart *int
	if lv.Playable && item.Playable {
		parts := r.itemParts(ctx, item)
		if partIndex != nil {
			if len(parts) == 0 || *partIndex >= len(parts) {
				return nil, core.ErrNotFound
			}
			idx := *partIndex
			effectivePart = &idx
		} else {
			p, ok := singlePartIndex(item)
			if !ok {
				return nil, core.ErrNotAvailable
			}
			effectivePart = p
		}
	} else {
		if partIndex == nil {
			return nil, core.ErrNotAvailable
		}
		leaves, err := r.collectPlayableLeaves(ctx, rec, item)
		if err != nil {
			return nil, err
		}
		if *partIndex >= len(leaves) {
			return nil, core.ErrNotFound
		}
		targetItem = leaves[*partIndex]
		p, ok := singlePartIndex(targetItem)
		if !ok {
			return nil, core.ErrNotAvailable
		}
		effectivePart = p
	}

	fk := flightKeyForItemSource(targetItem.Ref, effectivePart, sourceID)
	if cached, ok := r.cachedCandidates(fk); ok {
		if src, matched := matchPlayableSource(cached, targetItem.Title, sourceID, r.now().Unix()); matched {
			km, oerr := r.openSingleSource(ctx, rec, lib, targetItem, effectivePart, src, true)
			if oerr == nil {
				return km, nil
			}
			if ctx.Err() != nil || errors.Is(oerr, core.ErrPreparing) {
				return nil, oerr
			}
		}
		// Cached source expired or went stale upstream: drop and re-resolve once.
		r.deleteCandidates(fk)
	}

	if media, err := r.openSavedKindSource(ctx, rec, lib, targetItem, effectivePart, sourceID); media != nil || err != nil {
		return media, err
	}

	sources, err := r.resolveItemSourcesForSource(ctx, rec, lib, targetItem, effectivePart, sourceID, accept)
	if err != nil {
		return nil, err
	}
	src, matched := matchPlayableSource(sources, targetItem.Title, sourceID, r.now().Unix())
	if !matched {
		return nil, core.ErrNotAvailable
	}
	km, err := r.openSingleSource(ctx, rec, lib, targetItem, effectivePart, src, true)
	if err != nil {
		if errors.Is(err, core.ErrPreparing) {
			r.storeCandidates(fk, []core.PlayableSource{src})
		}
		return nil, err
	}
	r.storeCandidates(fk, []core.PlayableSource{src})
	return km, nil
}

// KindVersions lists the playable version menu of a plugin kind item in the
// W152 shape: index 0 is the synthetic Auto slot (backed by the viewer's
// remembered choice when matched, else the plugin's first source), followed by
// every distinct /resolve source as an explicit row, Auto's pick included.
// When allowResolve is false or ctx carries LocalResponse, only cached
// candidates are inspected and the plugin's /resolve endpoint is never called.
func (r *KindResolver) KindVersions(ctx context.Context, viewer *core.Viewer, key core.RatingKey, accept []string, allowResolve bool) ([]core.ReleaseVariant, error) {
	if r == nil || r.items == nil || r.libs == nil || viewer == nil || !key.IsPluginKey() {
		return nil, core.ErrNotFound
	}
	var forcedPart *int
	if parentKey, pIdx, isPart := key.PluginPart(); isPart {
		key = parentKey
		idx := pIdx
		forcedPart = &idx
	}
	item, lib, rec, err := r.loadAuthorized(ctx, viewer, key)
	if err != nil {
		return nil, err
	}
	if item.Unavailable != nil {
		return nil, nil
	}
	lv, _, ok := rec.Decl.Level(item.Level)
	if !ok || !lv.Playable || !item.Playable {
		return nil, nil
	}
	var partIdx *int
	if forcedPart != nil {
		parts := r.itemParts(ctx, item)
		if len(parts) > 0 && *forcedPart >= len(parts) {
			return nil, core.ErrNotFound
		}
		partIdx = forcedPart
	} else {
		partIdx, ok = singlePartIndex(item)
		if !ok {
			return nil, nil
		}
	}
	fk := flightKeyForItem(item.Ref, partIdx)
	speculative := core.SpeculativeIndexerWork(ctx)
	var sources []core.PlayableSource
	if allowResolve && !core.LocalResponse(ctx) {
		if speculative {
			if cached, hit, cerr := r.cachedCandidatesFor(ctx, item, partIdx); hit || cerr != nil {
				sources, err = cached, cerr
			} else if r.detailMissActive(fk) {
				return nil, nil
			} else {
				sources, err = r.resolveCandidates(ctx, rec, lib, item, partIdx, accept)
				if ctx.Err() == nil && (err != nil || len(sources) == 0) {
					r.noteDetailMiss(fk)
				}
			}
		} else {
			sources, err = r.resolveCandidates(ctx, rec, lib, item, partIdx, accept)
		}
	} else {
		var hit bool
		sources, hit, err = r.cachedCandidatesFor(ctx, item, partIdx)
		if !hit && err == nil {
			return nil, nil
		}
	}
	if err != nil {
		if !speculative && errors.As(err, new(*core.IndexersPausedError)) {
			return nil, err
		}
		if (speculative && errors.Is(err, core.ErrBudgetExhausted)) ||
			errors.Is(err, core.ErrKindCandidatesRejected) ||
			errors.Is(err, core.ErrNotAvailable) || errors.Is(err, core.ErrPreparing) || errors.Is(err, core.ErrSourceUnavailable) {
			return nil, nil
		}
		return nil, err
	}
	itemDur := extractItemDuration(item, rec.Decl)
	nowUnix := r.now().Unix()
	type entry struct {
		rel     core.Release
		variant core.ReleaseVariant
	}
	entries := make([]entry, 0, len(sources))
	seen := make(map[string]bool, len(sources))
	for _, src := range sources {
		if src.ExpiresAt > 0 && nowUnix >= src.ExpiresAt {
			continue
		}
		rel := playableSourceRelease(src, item.Title)
		if rel.ID == "" || seen[rel.ID] {
			continue
		}
		seen[rel.ID] = true
		entries = append(entries, entry{
			rel:     rel,
			variant: r.kindVariantFor(src, rel, item.Title, itemDur),
		})
	}
	if len(entries) == 0 {
		return nil, nil
	}

	autoIdx := 0
	autoRemembered := false
	if r.resolver != nil {
		title := r.projectedTitle(ctx, item, rec, lib)
		rels := make([]core.Release, len(entries))
		for i, e := range entries {
			rels[i] = e.rel
		}
		if idx, _ := r.resolver.MatchViewerChoice(viewer.ID, title, rels); idx >= 0 {
			autoIdx = idx
			autoRemembered = true
		}
	}

	autoEntry := entries[autoIdx].variant
	autoLabel := "Auto"
	if lbl := strings.TrimSpace(autoEntry.Label); lbl != "" {
		autoLabel = "Auto · " + lbl
	}
	capLen := min(len(entries)+1, core.MaxVariants)
	variants := make([]core.ReleaseVariant, 0, capLen)
	variants = append(variants, core.ReleaseVariant{
		ID:               core.KindAutoVersionID,
		Label:            autoLabel,
		Edition:          autoEntry.Edition,
		Auto:             true,
		Remembered:       autoRemembered,
		Availability:     autoEntry.Availability,
		AvailabilityNote: autoEntry.AvailabilityNote,
		Release:          autoEntry.Release,
		Info:             autoEntry.Info,
	})
	// Every source, Auto's current pick included, stays its own explicit
	// row: picking it forces that release, while Auto keeps failing over
	// (a 4K pick whose articles are gone must not hide the 4K choice).
	for _, e := range entries {
		if len(variants) >= core.MaxVariants {
			break
		}
		variants = append(variants, e.variant)
	}
	if autoIdx >= len(variants)-1 && len(variants) == core.MaxVariants {
		// A remembered pick past the menu bound keeps its explicit row.
		variants[len(variants)-1] = entries[autoIdx].variant
	}
	return variants, nil
}

// kindVariantFor projects one plugin /resolve source into a ReleaseVariant
// whose ID is the source's stable release id, carrying estimated media metadata
// until the source is probed.
func (r *KindResolver) kindVariantFor(src core.PlayableSource, rel core.Release, title string, itemDur time.Duration) core.ReleaseVariant {
	size := rel.Size
	if size <= 0 {
		size = src.Size
	}
	dur := itemDur
	if src.DurationSec > 0 {
		dur = time.Duration(src.DurationSec * float64(time.Second))
	}
	path := src.Path
	if path == "" && src.Release != nil {
		path = firstNonEmpty(src.Release.File, src.Release.FileHint)
	}
	if path == "" && src.Container != nil {
		path = src.Container.Member
	}
	if path == "" {
		if u, err := url.Parse(src.URL); err == nil {
			path = u.Path
		}
	}
	audioContainer, audioCodec := core.AudioFileHint(firstNonEmpty(path, rel.Name))
	mime, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(src.MIME)), ";")
	switch strings.TrimSpace(mime) {
	case "audio/flac", "audio/x-flac":
		audioContainer, audioCodec = "flac", "flac"
	case "audio/mpeg":
		audioContainer, audioCodec = "mp3", "mp3"
	case "audio/opus":
		audioContainer, audioCodec = "ogg", "opus"
	case "audio/alac", "audio/x-alac":
		audioContainer, audioCodec = "mp4", "alac"
	case "audio/ogg":
		audioContainer = "ogg"
	}
	info := core.MediaInfo{
		Container: kindContainerFromMIME(src.MIME, firstNonEmpty(rel.Name, path)),
		Duration:  dur,
		Size:      size,
		Estimated: true,
	}
	if size > 0 && dur > 0 {
		if sec := int64(dur / time.Second); sec > 0 {
			info.BitrateKbps = int((size * 8) / sec / 1000)
		}
	}
	if audioContainer != "" {
		info.Container = audioContainer
	} else if rel.Info.Height > 0 || rel.Info.VideoCodec != "" {
		w := 0
		if rel.Info.Height > 0 {
			w = (rel.Info.Height * 16) / 9
		}
		vs := core.StreamInfo{
			Index:       0,
			Type:        "video",
			Codec:       firstNonEmpty(rel.Info.VideoCodec, "h264"),
			Width:       w,
			Height:      rel.Info.Height,
			DolbyVision: rel.Info.DolbyVision,
		}
		if rel.Info.HDR10 || rel.Info.DolbyVision {
			vs.ColorTransfer = "smpte2084"
		}
		info.Streams = append(info.Streams, vs)
	}
	if audioCodec = firstNonEmpty(audioCodec, rel.Info.AudioCodec); audioCodec != "" {
		info.Streams = append(info.Streams, core.StreamInfo{
			Index:    len(info.Streams),
			Type:     "audio",
			Codec:    audioCodec,
			Channels: kindChannelsFromString(rel.Info.Channels),
		})
	}
	avail := core.VersionFetch
	availNote := ""
	switch strings.ToLower(strings.TrimSpace(src.Pattern)) {
	case "file":
		avail = core.VersionInstant
	case "range", "direct", "download", "container":
		if src.BlobRef != "" {
			avail = core.VersionInstant
		} else {
			avail = core.VersionCached
		}
	case kindstream.PatternRelease:
		if src.Release != nil && src.Release.Candidate != nil && src.Release.Candidate.Cached {
			avail = core.VersionCached
		} else if rel.Protocol == core.ProtocolTorrent {
			avail = core.VersionFetch
			availNote = "May need to fetch before playback"
		}
	}
	return core.ReleaseVariant{
		ID:               rel.ID,
		Label:            firstNonEmpty(src.Label, rel.Name, title),
		Edition:          rel.Info.Edition,
		Availability:     avail,
		AvailabilityNote: availNote,
		Release:          rel,
		Info:             info,
	}
}

func kindContainerFromMIME(mime, name string) string {
	m := strings.ToLower(strings.TrimSpace(mime))
	if semi := strings.IndexByte(m, ';'); semi >= 0 {
		m = strings.TrimSpace(m[:semi])
	}
	switch m {
	case "video/x-matroska", "audio/x-matroska":
		return "mkv"
	case "video/mp4", "audio/mp4":
		return "mp4"
	case "video/webm", "audio/webm":
		return "webm"
	case "video/mp2t":
		return "mpegts"
	case "video/quicktime":
		return "mov"
	}
	switch strings.ToLower(filepath.Ext(strings.TrimSpace(name))) {
	case ".mkv":
		return "mkv"
	case ".mp4", ".m4v", ".m4a":
		return "mp4"
	case ".webm":
		return "webm"
	case ".ts", ".m2ts":
		return "mpegts"
	case ".mov":
		return "mov"
	}
	return ""
}

func kindChannelsFromString(ch string) int {
	switch strings.TrimSpace(ch) {
	case "7.1":
		return 8
	case "6.1":
		return 7
	case "5.1":
		return 6
	case "5.0":
		return 5
	case "2.0":
		return 2
	case "1.0":
		return 1
	default:
		return 2
	}
}

func kindReleaseSource(raw string) core.Source {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "remux":
		return core.SourceRemux
	case "bluray", "blu-ray", "bdrip", "disc":
		return core.SourceBluRay
	case "web-dl", "webdl", "web":
		return core.SourceWEBDL
	case "webrip":
		return core.SourceWEBRip
	case "hdtv":
		return core.SourceHDTV
	case "dvd", "dvdrip":
		return core.SourceDVD
	default:
		return core.SourceUnknown
	}
}

// OpenKindExcluding opens a plugin kind item while excluding already-tried
// candidate release IDs (for mid-stream playback failover). Replacement
// candidates come from the stored candidate list of the same kind resolve,
// falling back to a fresh plugin re-resolve when exhausted or expired, and
// respecting provider backoff.
func (r *KindResolver) OpenKindExcluding(ctx context.Context, viewer *core.Viewer, key core.RatingKey, accept, exclude []string) (*frontend.KindMedia, error) {
	if r.waitsPreparing(ctx) {
		return r.waitPreparing(ctx, func(ctx context.Context) (*frontend.KindMedia, error) {
			return r.OpenKindExcluding(ctx, viewer, key, accept, exclude)
		})
	}
	if r == nil || r.items == nil || r.libs == nil || !key.IsPluginKey() {
		return nil, core.ErrNotFound
	}
	if core.IndexerPurpose(ctx) == core.IndexerPurposeOther {
		ctx = core.WithIndexerPurpose(ctx, core.IndexerPurposePlayback)
	}
	if parentKey, partIdx, isPart := key.PluginPart(); isPart {
		return r.OpenKindPartExcluding(ctx, viewer, parentKey, partIdx, accept, exclude)
	}
	item, lib, rec, err := r.loadAuthorized(ctx, viewer, key)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			if part, parentItem, _, perr := r.items.GetPartByKey(ctx, key); perr == nil {
				return r.openStoredPartForViewer(ctx, viewer, parentItem.Key, part, accept, exclude)
			}
		}
		return nil, err
	}
	if item.Unavailable != nil {
		return nil, core.ErrNotAvailable
	}
	lv, _, ok := rec.Decl.Level(item.Level)
	if !ok {
		return nil, core.ErrNotFound
	}
	if lv.Playable && item.Playable {
		// Honor the viewer's remembered version choice on Auto open without
		// overwriting the shared default winner in plugin_resolutions.
		if len(exclude) == 0 && viewer != nil && r.resolver != nil {
			if partIdx, single := singlePartIndex(item); single {
				title := r.projectedTitle(ctx, item, rec, lib)
				_, hasTitle := r.resolver.ViewerChoiceFor(viewer.ID, item.Key)
				hasShow := false
				if !hasTitle && title.Type == core.TypeEpisode && title.ShowKey.Valid() {
					_, hasShow = r.resolver.ViewerChoiceFor(viewer.ID, title.ShowKey)
				}
				if hasTitle || hasShow {
					if sources, cerr := r.resolveCandidates(ctx, rec, lib, item, partIdx, accept); cerr == nil && len(sources) > 0 {
						nowUnix := r.now().Unix()
						live := make([]core.PlayableSource, 0, len(sources))
						rels := make([]core.Release, 0, len(sources))
						for _, src := range sources {
							if src.ExpiresAt > 0 && nowUnix >= src.ExpiresAt {
								continue
							}
							live = append(live, src)
							rels = append(rels, playableSourceRelease(src, item.Title))
						}
						if idx, _ := r.resolver.MatchViewerChoice(viewer.ID, title, rels); idx >= 0 {
							if media, oerr := r.openSingleSource(ctx, rec, lib, item, partIdx, live[idx], false); oerr == nil {
								return media, nil
							}
						}
					}
				}
			}
		}
		return r.openPlayableItemMediaExcluding(ctx, rec, lib, item, accept, exclude)
	}

	synID := "kind:" + key.String()
	if slices.Contains(exclude, synID) {
		return nil, core.ErrNotAvailable
	}
	leaves, err := r.collectPlayableLeaves(ctx, rec, item)
	if err != nil {
		return nil, err
	}
	if len(leaves) == 0 {
		return nil, core.ErrNotAvailable
	}
	seqLeaves := make([]kindstream.Leaf, len(leaves))
	var totalDur time.Duration
	for i, leaf := range leaves {
		leafItem := leaf
		totalDur += extractItemDuration(leafItem, rec.Decl)
		leafSize, leafMIME, lerr := r.resolveLeafSizeAndMIME(ctx, rec, lib, leafItem, accept)
		if lerr != nil {
			return nil, lerr
		}
		seqLeaves[i] = kindstream.Leaf{
			Name: leafItem.Title,
			MIME: leafMIME,
			Size: leafSize,
			Open: func(lctx context.Context) (kindstream.ByteSource, error) {
				bs, _, _, oerr := r.openPlayableItemByteSourceExcluding(lctx, rec, lib, leafItem, accept, exclude)
				return bs, oerr
			},
		}
	}
	seq := kindstream.NewSequence(seqLeaves)
	return &frontend.KindMedia{
		ReaderAt: seq,
		Closer:   seq,
		Size:     seq.Size(),
		MIME:     seq.MIME(),
		Duration: totalDur,
		Name:     item.Title,
		Release: core.Release{
			ID:       synID,
			Name:     item.Title,
			Protocol: core.ProtocolHTTP,
			Size:     seq.Size(),
		},
	}, nil
}

// OpenKindPart implements frontend.KindPartStreams (#576). It opens one zero-based
// part of a multi-part plugin kind item, passing KindResolveRequest.PartIndex
// to the plugin's /resolve route.
func (r *KindResolver) OpenKindPart(ctx context.Context, viewer *core.Viewer, key core.RatingKey, partIndex int, accept []string) (*frontend.KindMedia, error) {
	return r.OpenKindPartExcluding(ctx, viewer, key, partIndex, accept, nil)
}

// OpenKindPartExcluding opens one zero-based part of a multi-part plugin kind
// item while skipping candidate release IDs listed in exclude.
func (r *KindResolver) OpenKindPartExcluding(ctx context.Context, viewer *core.Viewer, key core.RatingKey, partIndex int, accept, exclude []string) (*frontend.KindMedia, error) {
	if r.waitsPreparing(ctx) {
		return r.waitPreparing(ctx, func(ctx context.Context) (*frontend.KindMedia, error) {
			return r.OpenKindPartExcluding(ctx, viewer, key, partIndex, accept, exclude)
		})
	}
	if r == nil || r.items == nil || r.libs == nil || !key.IsPluginKey() || partIndex < 0 {
		return nil, core.ErrNotFound
	}
	if core.IndexerPurpose(ctx) == core.IndexerPurposeOther {
		ctx = core.WithIndexerPurpose(ctx, core.IndexerPurposePlayback)
	}
	if parentKey, _, isPart := key.PluginPart(); isPart {
		key = parentKey
	}
	item, lib, rec, err := r.loadAuthorized(ctx, viewer, key)
	if err != nil {
		return nil, err
	}
	if item.Unavailable != nil {
		return nil, core.ErrNotAvailable
	}
	lv, _, ok := rec.Decl.Level(item.Level)
	if !ok {
		return nil, core.ErrNotFound
	}
	if lv.Playable && item.Playable {
		parts := r.itemParts(ctx, item)
		if len(parts) == 0 || partIndex >= len(parts) {
			return nil, core.ErrNotFound
		}
		if partIndex < len(parts) && parts[partIndex].FileIndex != nil {
			if media, ok, ferr := r.tryOpenPartFromParentRelease(ctx, rec, lib, item, partIndex, parts[partIndex], accept, exclude); ok {
				return media, ferr
			}
		}
		idx := partIndex
		bs, winning, err := r.openItemByteSourceExcluding(ctx, rec, lib, item, &idx, accept, exclude)
		if err != nil {
			return nil, err
		}
		var dur time.Duration
		if partIndex < len(parts) {
			bs = withPartDefaults(bs, parts[partIndex], item.Title)
			if parts[partIndex].DurationSec > 0 {
				dur = time.Duration(parts[partIndex].DurationSec * float64(time.Second))
			}
		}
		if dur == 0 && len(parts) <= 1 {
			dur = extractItemDuration(item, rec.Decl)
		}
		rel := playableSourceRelease(winning, firstNonEmpty(bs.Name(), item.Title))
		if rel.Size <= 0 {
			rel.Size = bs.Size()
		}
		if rel.Name == "" {
			rel.Name = firstNonEmpty(bs.Name(), item.Title)
		}
		return &frontend.KindMedia{
			ReaderAt:    bs,
			Closer:      bs,
			Size:        bs.Size(),
			MIME:        bs.MIME(),
			Duration:    dur,
			Name:        bs.Name(),
			Release:     rel,
			MediaSource: unwrapKindMediaSource(bs),
		}, nil
	}

	leaves, err := r.collectPlayableLeaves(ctx, rec, item)
	if err != nil {
		return nil, err
	}
	if partIndex >= len(leaves) {
		return nil, core.ErrNotFound
	}
	return r.openPlayableItemMediaExcluding(ctx, rec, lib, leaves[partIndex], accept, exclude)
}

func (r *KindResolver) itemParts(ctx context.Context, item core.KindItem) []core.KindItemPart {
	if r != nil && r.items != nil && item.Ref != "" {
		if stored, err := r.items.ListParts(ctx, item.Ref); err == nil && len(stored) > 0 {
			out := make([]core.KindItemPart, len(stored))
			for i, sp := range stored {
				out[i] = sp.KindItemPart()
			}
			return out
		}
	}
	return core.ExtractKindItemPartsFromItem(item)
}

func (r *KindResolver) openStoredPartForViewer(ctx context.Context, viewer *core.Viewer, parentKey core.RatingKey, part pluginitems.StoredPart, accept, exclude []string) (*frontend.KindMedia, error) {
	parentItem, lib, rec, err := r.loadAuthorized(ctx, viewer, parentKey)
	if err != nil {
		return nil, err
	}
	if parentItem.Unavailable != nil {
		return nil, core.ErrNotAvailable
	}
	kp := part.KindItemPart()
	if kp.FileIndex != nil {
		if media, ok, ferr := r.tryOpenPartFromParentRelease(ctx, rec, lib, parentItem, part.PartIndex, kp, accept, exclude); ok {
			return media, ferr
		}
	}
	return r.OpenKindPartExcluding(ctx, viewer, parentKey, part.PartIndex, accept, exclude)
}

func (r *KindResolver) tryOpenPartFromParentRelease(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, parent core.KindItem, partIndex int, part core.KindItemPart, accept, exclude []string) (*frontend.KindMedia, bool, error) {
	if part.FileIndex == nil || *part.FileIndex < 0 {
		return nil, false, nil
	}
	nowUnix := r.now().Unix()
	var baseSrc *core.PlayableSource
	var vaultedOnly bool
	if cached, ok, expired, err := r.items.GetPartResolutionForOpen(ctx, parent.Ref, nil); err == nil && ok {
		if !expired && strings.EqualFold(strings.TrimSpace(cached.Pattern), kindstream.PatternRelease) && cached.Release != nil && (cached.ExpiresAt == 0 || nowUnix < cached.ExpiresAt) {
			baseSrc = &cached
		} else if expired && r.canReuseVaultedSource(cached) {
			baseSrc, vaultedOnly = &cached, true
		}
	}
	if baseSrc == nil {
		if cands, ok := r.cachedCandidates(resolveFlightKey{ref: parent.Ref, part: -1}); ok {
			for _, cand := range cands {
				if strings.EqualFold(strings.TrimSpace(cand.Pattern), kindstream.PatternRelease) && cand.Release != nil && (cand.ExpiresAt == 0 || nowUnix < cand.ExpiresAt) {
					cp := cand
					baseSrc = &cp
					break
				}
			}
		}
	}
	if baseSrc == nil {
		return nil, false, nil
	}
	relHint := *baseSrc.Release
	fi := *part.FileIndex
	relHint.FileIndex = &fi
	relHint.File = ""
	if relHint.Candidate != nil {
		cc := *relHint.Candidate
		cc.FileIndex = &fi
		relHint.Candidate = &cc
	}
	partSrc := *baseSrc
	partSrc.Release = &relHint
	if part.MIME != "" {
		partSrc.MIME = part.MIME
	}
	if part.Size > 0 {
		partSrc.Size = part.Size
	}
	if part.DurationSec > 0 {
		partSrc.DurationSec = part.DurationSec
	}
	if part.Name != "" {
		partSrc.Label = part.Name
	}
	rel := playableSourceRelease(partSrc, firstNonEmpty(part.Name, parent.Title))
	if slices.Contains(exclude, rel.ID) {
		return nil, !vaultedOnly, core.ErrNotAvailable
	}
	idx := partIndex
	openCtx := ctx
	if vaultedOnly {
		openCtx = newznab.WithVaultedPluginNZB(ctx)
	}
	media, err := r.openSingleSource(openCtx, rec, lib, parent, &idx, partSrc, !vaultedOnly)
	if err != nil {
		if vaultedOnly && ctx.Err() == nil && errors.Is(err, core.ErrSourceUnavailable) {
			return nil, false, nil
		}
		return nil, true, err
	}
	if part.Name != "" {
		media.Name = part.Name
	}
	if media.Duration == 0 && part.DurationSec > 0 {
		media.Duration = time.Duration(part.DurationSec * float64(time.Second))
	}
	return media, true, nil
}

// KindParts implements frontend.KindPartStreams (#576). It returns the declared
// or resolve-time parts of item without opening bytes.
func (r *KindResolver) KindParts(ctx context.Context, viewer *core.Viewer, key core.RatingKey) ([]frontend.KindPart, error) {
	if r == nil || r.items == nil || r.libs == nil || !key.IsPluginKey() {
		return nil, core.ErrNotFound
	}
	item, _, _, err := r.loadAuthorized(ctx, viewer, key)
	if err != nil {
		return nil, err
	}
	if item.Unavailable != nil {
		return nil, core.ErrNotAvailable
	}
	parts := r.itemParts(ctx, item)
	if len(parts) == 0 {
		return nil, nil
	}
	out := make([]frontend.KindPart, len(parts))
	for i, p := range parts {
		idx := i
		size := p.Size
		mime := p.MIME
		if size <= 0 || mime == "" {
			if cachedSrc, ok, cerr := r.items.GetPartResolution(ctx, item.Ref, &idx); cerr == nil && ok {
				if size <= 0 && cachedSrc.Size > 0 {
					size = cachedSrc.Size
				}
				if mime == "" && cachedSrc.MIME != "" {
					mime = cachedSrc.MIME
				}
			}
		}
		out[i] = frontend.KindPart{
			Index:    i,
			Name:     p.Name,
			MIME:     mime,
			Size:     size,
			Duration: time.Duration(p.DurationSec * float64(time.Second)),
		}
	}
	return out, nil
}

func (r *KindResolver) openPlayableItemMedia(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, accept []string) (*frontend.KindMedia, error) {
	return r.openPlayableItemMediaExcluding(ctx, rec, lib, item, accept, nil)
}

func (r *KindResolver) openPlayableItemMediaExcluding(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, accept, exclude []string) (*frontend.KindMedia, error) {
	bs, dur, rel, err := r.openPlayableItemByteSourceExcluding(ctx, rec, lib, item, accept, exclude)
	if err != nil {
		return nil, err
	}
	if rel.Size <= 0 {
		rel.Size = bs.Size()
	}
	if rel.Name == "" {
		rel.Name = firstNonEmpty(bs.Name(), item.Title)
	}
	return &frontend.KindMedia{
		ReaderAt:    bs,
		Closer:      bs,
		Size:        bs.Size(),
		MIME:        bs.MIME(),
		Duration:    dur,
		Name:        bs.Name(),
		Release:     rel,
		MediaSource: unwrapKindMediaSource(bs),
	}, nil
}

func (r *KindResolver) openPlayableItemByteSource(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, accept []string) (kindstream.ByteSource, time.Duration, error) {
	bs, dur, _, err := r.openPlayableItemByteSourceExcluding(ctx, rec, lib, item, accept, nil)
	return bs, dur, err
}

func (r *KindResolver) openPlayableItemByteSourceExcluding(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, accept, exclude []string) (kindstream.ByteSource, time.Duration, core.Release, error) {
	parts := core.ExtractKindItemPartsFromItem(item)
	if len(parts) == 0 {
		bs, winning, err := r.openItemByteSourceExcluding(ctx, rec, lib, item, nil, accept, exclude)
		if err != nil {
			return nil, 0, core.Release{}, err
		}
		return bs, extractItemDuration(item, rec.Decl), playableSourceRelease(winning, item.Title), nil
	}
	if len(parts) == 1 {
		idx := 0
		bs, winning, err := r.openItemByteSourceExcluding(ctx, rec, lib, item, &idx, accept, exclude)
		if err != nil {
			return nil, 0, core.Release{}, err
		}
		bs = withPartDefaults(bs, parts[0], item.Title)
		dur := extractItemDuration(item, rec.Decl)
		if dur == 0 && parts[0].DurationSec > 0 {
			dur = time.Duration(parts[0].DurationSec * float64(time.Second))
		}
		return bs, dur, playableSourceRelease(winning, item.Title), nil
	}

	// Multi-part item (#576): open part 0 up front so any pending/unavailable
	// status surfaces before HTTP headers are written, and assemble a Sequence
	// over all parts so whole-item readers stream every file in order.
	zero := 0
	firstRaw, winning0, err := r.openItemByteSourceExcluding(ctx, rec, lib, item, &zero, accept, exclude)
	if err != nil {
		return nil, 0, core.Release{}, err
	}
	firstBS := withPartDefaults(firstRaw, parts[0], item.Title)

	var firstMu sync.Mutex
	preopened := firstBS
	releasePreopened := func() error {
		firstMu.Lock()
		bs := preopened
		preopened = nil
		firstMu.Unlock()
		if bs != nil {
			return bs.Close()
		}
		return nil
	}

	seqLeaves := make([]kindstream.Leaf, len(parts))
	part0Copy := parts[0]
	seqLeaves[0] = kindstream.Leaf{
		Name: firstNonEmpty(part0Copy.Name, firstBS.Name(), fmt.Sprintf("%s (part 1)", item.Title)),
		MIME: firstNonEmpty(firstBS.MIME(), part0Copy.MIME),
		Size: firstBS.Size(),
		Open: func(lctx context.Context) (kindstream.ByteSource, error) {
			firstMu.Lock()
			bs := preopened
			preopened = nil
			firstMu.Unlock()
			if bs != nil {
				return bs, nil
			}
			idx := 0
			reopened, _, oerr := r.openItemByteSourceExcluding(lctx, rec, lib, item, &idx, accept, exclude)
			if oerr != nil {
				return nil, oerr
			}
			return withPartDefaults(reopened, part0Copy, item.Title), nil
		},
	}

	var sumPartDur time.Duration
	if parts[0].DurationSec > 0 {
		sumPartDur += time.Duration(parts[0].DurationSec * float64(time.Second))
	}

	for i := 1; i < len(parts); i++ {
		partCopy := parts[i]
		partIdx := i
		if partCopy.DurationSec > 0 {
			sumPartDur += time.Duration(partCopy.DurationSec * float64(time.Second))
		}
		partSize, partMIME, perr := r.resolvePartSizeAndMIME(ctx, rec, lib, item, partCopy, accept)
		if perr != nil {
			_ = releasePreopened()
			return nil, 0, core.Release{}, perr
		}
		seqLeaves[i] = kindstream.Leaf{
			Name: firstNonEmpty(partCopy.Name, fmt.Sprintf("%s (part %d)", item.Title, partIdx+1)),
			MIME: partMIME,
			Size: partSize,
			Open: func(lctx context.Context) (kindstream.ByteSource, error) {
				bs, _, oerr := r.openItemByteSourceExcluding(lctx, rec, lib, item, &partIdx, accept, exclude)
				if oerr != nil {
					return nil, oerr
				}
				return withPartDefaults(bs, partCopy, item.Title), nil
			},
		}
	}

	dur := extractItemDuration(item, rec.Decl)
	if dur == 0 {
		dur = sumPartDur
	}
	seq := kindstream.NewSequence(seqLeaves)
	rel := playableSourceRelease(winning0, item.Title)
	rel.Size = seq.Size()
	return &preopenedSequence{
		ByteSource: seq,
		name:       firstNonEmpty(item.Title, seq.Name()),
		cleanup:    releasePreopened,
	}, dur, rel, nil
}

func (r *KindResolver) resolvePartSizeAndMIME(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, part core.KindItemPart, accept []string) (int64, string, error) {
	idx := part.Index
	size := part.Size
	mime := part.MIME
	cachedSrc, ok, cerr := r.items.GetPartResolution(ctx, item.Ref, &idx)
	if cerr != nil {
		return 0, "", cerr
	}
	if ok {
		if size <= 0 && cachedSrc.Size > 0 {
			size = cachedSrc.Size
		}
		if mime == "" && cachedSrc.MIME != "" {
			mime = cachedSrc.MIME
		}
	}
	if size > 0 {
		return size, mime, nil
	}
	bs, err := r.openItemByteSource(ctx, rec, lib, item, &idx, accept)
	if err != nil {
		return 0, "", err
	}
	size = bs.Size()
	if mime == "" && bs.MIME() != "" {
		mime = bs.MIME()
	}
	_ = bs.Close()
	return size, mime, nil
}

func (r *KindResolver) resolveLeafSizeAndMIME(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, leaf core.KindItem, accept []string) (int64, string, error) {
	if parts := core.ExtractKindItemPartsFromItem(leaf); len(parts) > 0 {
		bs, _, err := r.openPlayableItemByteSource(ctx, rec, lib, leaf, accept)
		if err != nil {
			return 0, "", err
		}
		size := bs.Size()
		mime := bs.MIME()
		_ = bs.Close()
		return size, mime, nil
	}
	size, mime := extractItemSizeAndMIME(leaf, rec.Decl)
	if size > 0 {
		return size, mime, nil
	}
	cachedSrc, ok, cerr := r.items.GetResolution(ctx, leaf.Ref)
	if cerr != nil {
		return 0, "", cerr
	}
	if ok && cachedSrc.Size > 0 {
		if cachedSrc.MIME != "" {
			mime = cachedSrc.MIME
		}
		return cachedSrc.Size, mime, nil
	}
	bs, err := r.openItemByteSource(ctx, rec, lib, leaf, nil, accept)
	if err != nil {
		return 0, "", err
	}
	size = bs.Size()
	if bs.MIME() != "" {
		mime = bs.MIME()
	}
	_ = bs.Close()
	return size, mime, nil
}

// KindLeaves implements frontend.KindStreams.
func (r *KindResolver) KindLeaves(ctx context.Context, viewer *core.Viewer, parent core.RatingKey) ([]frontend.KindLeaf, error) {
	if r == nil || r.items == nil || r.libs == nil || !parent.IsPluginKey() {
		return nil, core.ErrNotFound
	}
	parentItem, _, rec, err := r.loadAuthorized(ctx, viewer, parent)
	if err != nil {
		return nil, err
	}
	leaves, err := r.collectPlayableLeaves(ctx, rec, parentItem)
	if err != nil {
		return nil, err
	}
	out := make([]frontend.KindLeaf, len(leaves))
	for i, leaf := range leaves {
		out[i] = frontend.KindLeaf{
			Key:      leaf.Key,
			Index:    leaf.Index,
			Title:    leaf.Title,
			Duration: extractItemDuration(leaf, rec.Decl),
		}
	}
	return out, nil
}

// KeyForRef implements frontend.KindRefKeys. It maps a plugin ItemRef to the
// stored item's RatingKey, refusing hidden libraries and viewers who cannot
// open them.
func (r *KindResolver) KeyForRef(ctx context.Context, viewer *core.Viewer, ref core.ItemRef) (core.RatingKey, error) {
	if r == nil || r.items == nil || r.libs == nil || !ref.Valid() {
		return 0, core.ErrNotFound
	}
	item, libID, err := r.items.GetByRef(ctx, ref)
	if err != nil {
		return 0, core.ErrNotFound
	}
	lib, err := r.libs.Get(ctx, libID)
	if err != nil || lib.Hidden || viewer == nil || viewer.ID <= 0 || !viewer.AllowsLibrary(lib.ID) {
		return 0, core.ErrNotFound
	}
	if r.authorize != nil {
		if err := r.authorize(ctx, viewer, item.Key); err != nil {
			return 0, err
		}
	}
	return item.Key, nil
}

// RefsForKeys implements frontend.KindRefKeys. It returns the ItemRef of each
// key the viewer may see; unknown or unauthorized keys are omitted. The page
// is caller-bounded, so this is a handful of indexed reads, never a walk.
func (r *KindResolver) RefsForKeys(ctx context.Context, viewer *core.Viewer, keys []core.RatingKey) (map[core.RatingKey]core.ItemRef, error) {
	out := make(map[core.RatingKey]core.ItemRef, len(keys))
	if r == nil || r.items == nil || r.libs == nil || viewer == nil || viewer.ID <= 0 {
		return out, nil
	}
	for _, key := range keys {
		if !key.IsPluginKey() {
			continue
		}
		item, libID, err := r.items.GetByKey(ctx, key)
		if err != nil {
			continue
		}
		lib, err := r.libs.Get(ctx, libID)
		if err != nil || lib.Hidden || !viewer.AllowsLibrary(lib.ID) {
			continue
		}
		if r.authorize != nil {
			if err := r.authorize(ctx, viewer, item.Key); err != nil {
				continue
			}
		}
		out[key] = item.Ref
	}
	return out, nil
}

const (
	sourceIndexerSearchTimeout = 30 * time.Second
	sourceReleaseFilesTimeout  = 30 * time.Second
	sourceDebridCheckTimeout   = 15 * time.Second
	sourceDebridAcquireTimeout = 30 * time.Second

	// Viewer / deadline-bounded indexer_search leaves at least
	// max(1.5 s, 35% of remaining time) so the calling plugin can still rank
	// candidates, run a debrid check, and return before its route deadline.
	indexerSearchReserveFloor = 1500 * time.Millisecond
	indexerSearchReservePct   = 35
)

// defaultIndexerWaitBudget computes the default deadline-aware wait budget for
// an indexer_search call under ctx. It returns 0 when ctx has no deadline or
// when a background/granted-limits call still has more remaining time than
// ceiling (so background jobs keep waiting up to the full ceiling unchanged).
func defaultIndexerWaitBudget(ctx context.Context, now time.Time, ceiling time.Duration) time.Duration {
	dl, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	remaining := dl.Sub(now)
	if remaining <= 0 {
		return 0
	}
	callPath := wasm.CallPath(ctx)
	if (wasm.IsBackgroundCall(ctx) || (callPath != "" && wasm.GrantedLimitsPath(callPath))) && remaining > ceiling {
		return 0
	}
	reserve := max(indexerSearchReserveFloor, remaining*indexerSearchReservePct/100)
	wait := remaining - reserve
	// Join smoothly at remaining == 2 s (where remaining - 1.5 s == remaining/4 == 0.5 s)
	// so tight callers still get a bounded wait rather than falling back to 0 (unbounded).
	if floor := remaining / 4; wait < floor {
		wait = floor
	}
	wait = max(time.Millisecond, wait)
	if ceiling > 0 && wait >= ceiling {
		return 0
	}
	return wait
}

func withHardTimeout(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	if limit <= 0 {
		return ctx, func() {}
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= limit {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, limit)
}

func singleIndexerSkipped(name string, skip, only []string) bool {
	target := strings.ToLower(strings.TrimSpace(name))
	for _, s := range skip {
		if k := strings.ToLower(strings.TrimSpace(s)); k != "" && k == target {
			return true
		}
	}
	hasOnly := false
	matchedOnly := false
	for _, s := range only {
		if k := strings.ToLower(strings.TrimSpace(s)); k != "" {
			hasOnly = true
			if k == target {
				matchedOnly = true
			}
		}
	}
	return hasOnly && !matchedOnly
}

type resolveSearchStateKey struct{}

type resolveSearchState struct {
	mu               sync.Mutex
	oneShotPending   bool
	pendingBySearch  map[string]bool
	budgetSkipped    bool
	candidatesFound  bool
	indexerAnswered  bool
	pausedIndexers   map[string]string
	inFlight         int
	sliceSourceCalls int
	sliceWorkKeys    map[string]struct{}
	playbackCtx      context.Context
	indexerSearches  map[string]*resolveIndexerLookup
	lookupLimitHit   bool
}

func withResolveSearchState(ctx context.Context) (context.Context, *resolveSearchState) {
	st := &resolveSearchState{}
	return context.WithValue(ctx, resolveSearchStateKey{}, st), st
}

func resolveSearchStateFrom(ctx context.Context) *resolveSearchState {
	if ctx == nil {
		return nil
	}
	st, _ := ctx.Value(resolveSearchStateKey{}).(*resolveSearchState)
	return st
}

func (s *resolveSearchState) beginSlice() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.oneShotPending = false
	s.sliceSourceCalls = 0
	s.sliceWorkKeys = nil
	s.mu.Unlock()
}

func beginResolveSourceWork(ctx context.Context, workKey string) func() {
	st := resolveSearchStateFrom(ctx)
	if st == nil {
		return func() {}
	}
	st.mu.Lock()
	st.inFlight++
	st.sliceSourceCalls++
	if workKey != "" {
		if st.sliceWorkKeys == nil {
			st.sliceWorkKeys = make(map[string]struct{})
		}
		st.sliceWorkKeys[workKey] = struct{}{}
	}
	st.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			st.mu.Lock()
			if st.inFlight > 0 {
				st.inFlight--
			}
			st.mu.Unlock()
		})
	}
}

func noteResolveSourceWork(ctx context.Context, workKey string, cached bool) {
	st := resolveSearchStateFrom(ctx)
	if st == nil {
		return
	}
	st.mu.Lock()
	st.sliceSourceCalls++
	if !cached && workKey != "" {
		if st.sliceWorkKeys == nil {
			st.sliceWorkKeys = make(map[string]struct{})
		}
		st.sliceWorkKeys[workKey] = struct{}{}
	}
	st.mu.Unlock()
}

func (s *resolveSearchState) timeoutProgressSnapshot() (hasPending, hasActivity bool, workKeys []string) {
	if s == nil {
		return false, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hasPending = s.oneShotPending
	if !hasPending {
		for _, pending := range s.pendingBySearch {
			if pending {
				hasPending = true
				break
			}
		}
	}
	hasActivity = s.inFlight > 0 || s.sliceSourceCalls > 0
	if len(s.sliceWorkKeys) > 0 {
		workKeys = make([]string, 0, len(s.sliceWorkKeys))
		for wk := range s.sliceWorkKeys {
			workKeys = append(workKeys, wk)
		}
	}
	return hasPending, hasActivity, workKeys
}

func noteResolveSearchResult(ctx context.Context, res core.SourceResolveResult) {
	st := resolveSearchStateFrom(ctx)
	if st == nil {
		return
	}
	hasPending := len(res.Pending) > 0
	st.mu.Lock()
	if res.SearchID != "" {
		if st.pendingBySearch == nil {
			st.pendingBySearch = make(map[string]bool)
		}
		st.pendingBySearch[res.SearchID] = hasPending
	} else if hasPending {
		st.oneShotPending = true
	}
	st.candidatesFound = st.candidatesFound || len(res.Candidates) > 0
	for _, status := range res.Indexers {
		switch {
		case status.Error == "budget_exhausted":
			st.budgetSkipped = true
		case status.Status == core.IndexerStatusAnswered:
			st.indexerAnswered = true
		case status.Status == core.IndexerStatusSkipped && status.Reason == core.IndexerSkipReasonPaused:
			if st.pausedIndexers == nil {
				st.pausedIndexers = make(map[string]string)
			}
			st.pausedIndexers[status.IndexerID] = status.ConfiguredName
		}
	}
	st.mu.Unlock()
}

func (s *resolveSearchState) indexerPause() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.indexerAnswered || len(s.pausedIndexers) == 0 {
		return nil
	}
	names := slices.Collect(maps.Values(s.pausedIndexers))
	slices.Sort(names)
	return &core.IndexersPausedError{Names: slices.Compact(names)}
}

func noteResolveSearchCanceled(ctx context.Context, searchID string) {
	st := resolveSearchStateFrom(ctx)
	if st == nil || searchID == "" {
		return
	}
	st.mu.Lock()
	delete(st.pendingBySearch, searchID)
	st.mu.Unlock()
}

func noteResolveBudgetSkip(ctx context.Context) {
	st := resolveSearchStateFrom(ctx)
	if st == nil {
		return
	}
	st.mu.Lock()
	st.budgetSkipped = true
	st.mu.Unlock()
}

func (s *resolveSearchState) hasPending() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.oneShotPending {
		return true
	}
	for _, pending := range s.pendingBySearch {
		if pending {
			return true
		}
	}
	return false
}

func (s *resolveSearchState) hadBudgetSkip() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.budgetSkipped
}

func (r *KindResolver) resolveIndexerSearch(ctx context.Context, pluginID string, q core.SourceQuery) (core.SourceResolveResult, error) {
	if q.Stream {
		return r.startStreamingIndexerSearch(ctx, pluginID, q)
	}
	query, category, err := r.validateIndexerSearchQuery(ctx, q)
	if err != nil {
		return core.SourceResolveResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return core.SourceResolveResult{}, err
	}
	if r.indexer == nil {
		return core.SourceResolveResult{Candidates: []core.SourceCandidate{}}, nil
	}
	if r.speculativeSourceBudgetPaused(ctx) {
		noteResolveBudgetSkip(ctx)
		var statuses []core.IndexerSearchStatus
		if dir, ok := r.indexer.(core.IndexerDirectory); ok {
			statuses = dir.EnabledIndexerStatuses(core.IndexerStatusSkipped)
		}
		return core.SourceResolveResult{Candidates: []core.SourceCandidate{}, Indexers: statuses}, nil
	}

	ceiling := r.streamCeilingDuration()
	searchReq := core.SearchRequest{
		Title:    query,
		Category: category,
		Lookup:   q.Lookup,
	}
	opts := core.IndexerSearchOptions{
		FirstResults:      q.FirstResults,
		MinResults:        q.MinResults,
		SkipIndexers:      q.SkipIndexers,
		OnlyIndexers:      q.OnlyIndexers,
		BackgroundTimeout: ceiling,
	}
	if q.SettleMS != nil {
		opts.Settle = min(time.Duration(*q.SettleMS)*time.Millisecond, ceiling)
		opts.SettleSet = true
	}
	if q.TimeoutMS > 0 {
		opts.WaitTimeout = min(time.Duration(q.TimeoutMS)*time.Millisecond, ceiling)
	}
	if waitBudget := defaultIndexerWaitBudget(ctx, r.now(), ceiling); waitBudget > 0 {
		if opts.WaitTimeout == 0 || opts.WaitTimeout > waitBudget {
			opts.WaitTimeout = waitBudget
		}
	}

	var (
		rels     []core.Release
		statuses []core.IndexerSearchStatus
		pending  []core.IndexerRef
	)
	if csi, ok := r.indexer.(core.ControlledSearchIndexer); ok {
		searchCtx := ctx
		if opts.WaitTimeout == 0 {
			var cancel context.CancelFunc
			searchCtx, cancel = withHardTimeout(ctx, ceiling)
			defer cancel()
		}
		var sr core.IndexerSearchResult
		if st := resolveSearchStateFrom(ctx); st != nil && st.playbackCtx != nil {
			return r.continueIndexerSearch(searchCtx, st, csi, q, searchReq, opts)
		}
		sr, err = csi.SearchWithOptions(searchCtx, searchReq, opts)
		rels = sr.Releases
		statuses = sr.Indexers
		if len(sr.Pending) > 0 {
			pending = sr.Pending
		}
		if err != nil {
			if searchCtx.Err() != nil {
				return core.SourceResolveResult{}, searchCtx.Err()
			}
			if errors.Is(err, core.ErrBudgetExhausted) {
				noteResolveBudgetSkip(ctx)
				r.noteSpeculativeSourceBudgetExhausted(ctx)
			}
			// One broken indexer (a missing site definition, an outage)
			// fails the whole fan-out when nobody else had a match. For a
			// plugin that is simply "no releases": its search must not
			// throw because of an indexer it never chose.
			r.log.Debug("plugin indexer search found nothing; some indexers failed")
			return core.SourceResolveResult{Candidates: []core.SourceCandidate{}, Indexers: statuses, Pending: pending}, nil
		}
	} else {
		name := r.indexer.Name()
		if singleIndexerSkipped(name, opts.SkipIndexers, opts.OnlyIndexers) {
			return core.SourceResolveResult{
				Candidates: []core.SourceCandidate{},
				Indexers: []core.IndexerSearchStatus{{
					IndexerID: name,
					Indexer:   name,
					Status:    core.IndexerStatusSkipped,
					MS:        0,
				}},
			}, nil
		}
		limit := ceiling
		if opts.WaitTimeout > 0 {
			limit = opts.WaitTimeout
		}
		searchCtx, cancel := withHardTimeout(ctx, limit)
		defer cancel()
		started := time.Now()
		rels, err = r.indexer.Search(searchCtx, searchReq)
		ms := max(int64(0), time.Since(started).Milliseconds())
		if err != nil {
			if opts.WaitTimeout > 0 && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return core.SourceResolveResult{
					Candidates: []core.SourceCandidate{},
					Indexers: []core.IndexerSearchStatus{{
						IndexerID: name,
						Indexer:   name,
						Status:    core.IndexerStatusTimedOut,
						MS:        ms,
					}},
				}, nil
			}
			if searchCtx.Err() != nil {
				return core.SourceResolveResult{}, searchCtx.Err()
			}
			if errors.Is(err, core.ErrBudgetExhausted) {
				noteResolveBudgetSkip(ctx)
				r.noteSpeculativeSourceBudgetExhausted(ctx)
			}
			r.log.Debug("plugin indexer search found nothing; some indexers failed")
			return core.SourceResolveResult{
				Candidates: []core.SourceCandidate{},
				Indexers: []core.IndexerSearchStatus{{
					IndexerID: name,
					Indexer:   name,
					Status:    core.IndexerStatusError,
					MS:        ms,
					Error:     "search_failed",
				}},
			}, nil
		}
		statuses = []core.IndexerSearchStatus{{
			IndexerID: name,
			Indexer:   name,
			Status:    core.IndexerStatusAnswered,
			Results:   len(rels),
			MS:        ms,
		}}
	}

	return r.finishIndexerSearch(ctx, query, q.Profile, rels, statuses, pending)
}

func (r *KindResolver) finishIndexerSearch(ctx context.Context, query, profile string, rels []core.Release, statuses []core.IndexerSearchStatus, pending []core.IndexerRef) (core.SourceResolveResult, error) {
	if query == "" {
		rels = append([]core.Release(nil), rels...)
		sort.SliceStable(rels, func(i, j int) bool {
			pi, pj := rels[i].PubDate, rels[j].PubDate
			if pi.IsZero() != pj.IsZero() {
				return !pi.IsZero()
			}
			return pi.After(pj)
		})
		if len(rels) > maxRecentSourceCandidates {
			rels = rels[:maxRecentSourceCandidates]
		}
	}
	scored, err := r.scoreIndexerReleases(ctx, query, profile, rels)
	if err != nil {
		return core.SourceResolveResult{}, err
	}
	return core.SourceResolveResult{
		Candidates: scored,
		Indexers:   statuses,
		Pending:    pending,
	}, nil
}

// ResolveSourcesWithResult implements plugins.KindBackends.ResolveSourcesResult
// for plugins granted permissions.sources ("search" or "acquire"). For
// indexer_search / streaming / indexers queries it returns the full response
// envelope alongside candidates (#733).
func (r *KindResolver) ResolveSourcesWithResult(ctx context.Context, pluginID string, q core.SourceQuery) (core.SourceResolveResult, error) {
	if r == nil {
		return core.SourceResolveResult{}, core.ErrNotAvailable
	}
	if wasm.IsBackgroundCall(ctx) {
		ctx = core.WithUnattendedSearch(core.WithIndexerPurpose(ctx, core.IndexerPurposeWarmup))
	}
	switch q.Kind {
	case "indexer_search":
		workKey := fmt.Sprintf("indexer_search:%s|%s|%t", strings.TrimSpace(q.Query), strings.TrimSpace(q.Category), q.Stream)
		endWork := beginResolveSourceWork(ctx, workKey)
		defer endWork()
		res, err := r.resolveIndexerSearch(ctx, pluginID, q)
		if err == nil {
			noteResolveSearchResult(ctx, res)
		}
		return res, err
	case "indexer_search_results":
		endWork := beginResolveSourceWork(ctx, "")
		defer endWork()
		res, err := r.pollStreamingIndexerSearch(ctx, pluginID, q)
		if err == nil {
			noteResolveSearchResult(ctx, res)
			if len(res.Candidates) > 0 {
				first := res.Candidates[0]
				noteResolveSourceWork(ctx, fmt.Sprintf("indexer_search_results:%s:%s:%s:%s:%d", res.SearchID, first.NZBURL, first.InfoHash, first.Title, len(res.Candidates)), false)
			}
		}
		return res, err
	case "indexer_search_cancel":
		noteResolveSearchCanceled(ctx, q.SearchID)
		return r.cancelStreamingIndexerSearch(ctx, pluginID, q)
	case "indexers":
		return r.resolveIndexersList(), nil
	}
	cands, err := r.ResolveSources(ctx, pluginID, q)
	noteKindSourceFailure(ctx, err)
	if err != nil {
		return core.SourceResolveResult{}, err
	}
	if cands == nil {
		cands = []core.SourceCandidate{}
	}
	return core.SourceResolveResult{Candidates: cands}, nil
}

// ResolveSources implements plugins.KindBackends.ResolveSources for plugins
// granted permissions.sources ("search" or "acquire"). Capability and
// permission checks are enforced upstream in plugins/host_kinds.go.
func (r *KindResolver) ResolveSources(ctx context.Context, pluginID string, q core.SourceQuery) ([]core.SourceCandidate, error) {
	if r == nil {
		return nil, core.ErrNotAvailable
	}
	switch q.Kind {
	case "indexer_search", "indexer_search_results", "indexer_search_cancel", "indexers":
		res, err := r.ResolveSourcesWithResult(ctx, pluginID, q)
		if err != nil {
			return nil, err
		}
		return res.Candidates, nil

	case "debrid_check":
		if r.debrid == nil {
			return []core.SourceCandidate{}, nil
		}
		var hashes []string
		seen := make(map[string]bool)
		addHash := func(h string) {
			h = strings.ToLower(strings.TrimSpace(h))
			if h != "" && !seen[h] {
				seen[h] = true
				hashes = append(hashes, h)
			}
		}
		for _, h := range q.InfoHashes {
			addHash(h)
		}
		addHash(q.InfoHash)
		if len(hashes) == 0 {
			return []core.SourceCandidate{}, nil
		}
		sortedHashes := slices.Clone(hashes)
		slices.Sort(sortedHashes)
		workKey := "debrid_check:" + strings.Join(sortedHashes, ",")
		reorder := func(in []core.SourceCandidate) []core.SourceCandidate {
			byHash := make(map[string]core.SourceCandidate, len(in))
			for _, c := range in {
				byHash[c.InfoHash] = c
			}
			out := make([]core.SourceCandidate, len(hashes))
			for i, h := range hashes {
				if c, ok := byHash[h]; ok {
					out[i] = c
				} else {
					out[i] = core.SourceCandidate{InfoHash: h}
				}
			}
			return out
		}
		if handed, ok := r.takeSourceHandoff(workKey); ok {
			noteResolveSourceWork(ctx, workKey, true)
			return reorder(handed), nil
		}
		endWork := beginResolveSourceWork(ctx, workKey)
		defer endWork()
		raw, _, err := r.runDetachedSourceFlight(ctx, workKey, sourceDebridCheckTimeout, func(workCtx context.Context) (any, error) {
			out := make([]core.SourceCandidate, len(sortedHashes))
			if byBackend, ok := r.debrid.(debridCachedByBackend); ok {
				by, _ := byBackend.CheckCachedBy(workCtx, sortedHashes)
				for i, h := range sortedHashes {
					services := by[h]
					out[i] = core.SourceCandidate{
						InfoHash: h,
						Cached:   len(services) > 0,
						CachedBy: services,
					}
				}
				return out, nil
			}
			cached, _ := r.debrid.CheckCached(workCtx, sortedHashes)
			for i, h := range sortedHashes {
				out[i] = core.SourceCandidate{
					InfoHash: h,
					Cached:   cached[h],
				}
			}
			return out, nil
		}, func(res any) {
			if cands, ok := res.([]core.SourceCandidate); ok {
				r.storeSourceHandoff(workKey, cands)
			}
		})
		if err != nil {
			return nil, err
		}
		cands, _ := raw.([]core.SourceCandidate)
		return reorder(cloneSourceCandidates(cands)), nil

	case "nzb_request":
		return r.requestPluginNZB(ctx, q)

	case "release_files":
		cand := core.SourceCandidate{
			InfoHash:   strings.ToLower(strings.TrimSpace(q.InfoHash)),
			Magnet:     strings.TrimSpace(q.Magnet),
			TorrentURL: strings.TrimSpace(q.TorrentURL),
			TorrentRef: strings.TrimSpace(q.TorrentRef),
			NZBURL:     strings.TrimSpace(q.NZBURL),
			NZBRef:     strings.TrimSpace(q.NZBRef),
		}
		if q.Candidate != nil {
			cand = *q.Candidate
			if q.NZBURL != "" {
				cand.NZBURL = strings.TrimSpace(q.NZBURL)
			}
			if q.NZBRef != "" {
				cand.NZBRef = strings.TrimSpace(q.NZBRef)
			}
			cand.InfoHash = strings.ToLower(strings.TrimSpace(cand.InfoHash))
			cand.Magnet = strings.TrimSpace(cand.Magnet)
			cand.TorrentURL = strings.TrimSpace(cand.TorrentURL)
			cand.NZBURL = strings.TrimSpace(cand.NZBURL)
			if q.InfoHash != "" {
				cand.InfoHash = strings.ToLower(strings.TrimSpace(q.InfoHash))
			}
			if q.Magnet != "" {
				cand.Magnet = strings.TrimSpace(q.Magnet)
			}
			if q.TorrentRef != "" {
				cand.TorrentRef = strings.TrimSpace(q.TorrentRef)
			}
			if q.TorrentURL != "" {
				cand.TorrentURL = strings.TrimSpace(q.TorrentURL)
			}
		}
		if cand.PluginRelease != nil && cand.InfoHash == "" && cand.Magnet == "" && cand.TorrentURL == "" && cand.TorrentRef == "" && cand.NZBURL == "" && cand.NZBRef == "" {
			cand.FileIndex = nil
			cand.Files = nil
			return r.pluginReleaseFiles(ctx, cand)
		}
		// The listing answers for the whole release; any file choice the
		// candidate already carries does not narrow it.
		cand = r.stripCandidateAuth(cand)
		fileCount := max(q.FileCount, cand.FileCount, len(cand.Files))
		listCand := cand
		listCand.FileIndex = nil
		listCand.Files = nil
		rel, err := releaseFromHint(core.ReleaseHint{Candidate: &listCand, FileCount: fileCount}, "")
		if err != nil {
			return nil, err
		}
		if rel.Protocol == core.ProtocolUsenet {
			if r.indexer == nil {
				return []core.SourceCandidate{r.publicLegacyCandidate(ctx, cand)}, nil
			}
			releaseKey := candidateNZBKey(cand)
			workKey := "release_files:" + releaseKey
			if files, ok := r.lookupUsenetReleaseFiles(releaseKey); ok {
				noteResolveSourceWork(ctx, workKey, true)
				cand.Files = files
				return []core.SourceCandidate{r.publicLegacyCandidate(ctx, cand)}, nil
			}
			endWork := beginResolveSourceWork(ctx, workKey)
			defer endWork()
			raw, _, err := r.runDetachedSourceFlight(ctx, workKey, sourceReleaseFilesTimeout, func(workCtx context.Context) (any, error) {
				// Fetching through the originating indexer is the same call
				// playback makes, so the NZB lands in the vault and playback
				// does not fetch it again.
				data, err := r.fetchPluginNZB(workCtx, rel)
				if err != nil {
					return nil, err
				}
				files, err := r.listNZBFiles(workCtx, rel.ID, data)
				if err != nil {
					return nil, err
				}
				archiveOK := true
				if lister, ok := r.engine.(interface {
					ListArchiveMembers(context.Context, []byte) (map[int][]core.ReleaseMember, error)
				}); ok && lister != nil {
					byVol, lerr := lister.ListArchiveMembers(workCtx, data)
					if lerr != nil {
						archiveOK = false
					} else if len(byVol) > 0 {
						for idx, members := range byVol {
							if idx >= 0 && idx < len(files) {
								files[idx].Members = members
							}
						}
					}
				}
				if archiveOK && workCtx.Err() == nil && len(files) > 0 {
					r.storeUsenetReleaseFiles(releaseKey, files)
				}
				return files, nil
			}, nil)
			if err != nil {
				return nil, err
			}
			files, _ := raw.([]core.ReleaseFile)
			cand.Files = cloneReleaseFiles(files)
			return []core.SourceCandidate{r.publicLegacyCandidate(ctx, cand)}, nil
		}
		if rel.Protocol != core.ProtocolTorrent {
			return []core.SourceCandidate{r.publicLegacyCandidate(ctx, cand)}, nil
		}
		workKey := "release_files:torrent:" + rel.ID
		if handed, ok := r.takeSourceHandoff(workKey); ok {
			noteResolveSourceWork(ctx, workKey, true)
			return handed, nil
		}
		endWork := beginResolveSourceWork(ctx, workKey)
		defer endWork()
		raw, _, err := r.runDetachedSourceFlight(ctx, workKey, sourceReleaseFilesTimeout, func(workCtx context.Context) (any, error) {
			outCand := cand
			outRel := rel
			if outRel.InfoHash == "" && outRel.MagnetURL == "" && (outRel.NZBURL != "" || outRel.TorrentRef != "") {
				resolved, err := r.resolveTorrent(workCtx, outRel)
				if err != nil {
					return nil, err
				}
				outRel = resolved
				if outRel.InfoHash != "" {
					outCand.InfoHash = outRel.InfoHash
				}
				if outRel.MagnetURL != "" {
					outCand.Magnet = outRel.MagnetURL
				}
			}
			if r.debrid != nil {
				files, err := r.debrid.ListReleaseFiles(workCtx, outRel)
				if err == nil {
					outCand.Cached = true
					if knownLookup, ok := r.debrid.(debridKnownCachedByBackend); ok && outCand.InfoHash != "" {
						outCand.CachedBy = knownLookup.KnownCachedBy(outCand.InfoHash, nil)
					}
					outCand.Files = files
					outCand.FileCount = len(files)
					return []core.SourceCandidate{r.publicReleaseLinks(outCand)}, nil
				}
				if !errors.Is(err, core.ErrNotCached) && !errors.Is(err, debrid.ErrNoProvider) {
					return nil, err
				}
				outCand.Cached = false
				outCand.CachedBy = nil
			}
			if len(outRel.TorrentFiles) > 0 {
				outCand.Files = slices.Clone(outRel.TorrentFiles)
				outCand.FileCount = len(outCand.Files)
			}
			return []core.SourceCandidate{r.publicReleaseLinks(outCand)}, nil
		}, func(res any) {
			if cands, ok := res.([]core.SourceCandidate); ok {
				r.storeSourceHandoff(workKey, cands)
			}
		})
		if err != nil {
			return nil, err
		}
		cands, _ := raw.([]core.SourceCandidate)
		return cloneSourceCandidates(cands), nil

	case "usenet_check":
		cand := core.SourceCandidate{
			NZBURL: strings.TrimSpace(q.NZBURL),
			NZBRef: strings.TrimSpace(q.NZBRef),
		}
		if q.Candidate != nil {
			cand = *q.Candidate
			if q.NZBRef != "" {
				cand.NZBRef = strings.TrimSpace(q.NZBRef)
			}
			cand.NZBURL = strings.TrimSpace(cand.NZBURL)
			if strings.TrimSpace(q.NZBURL) != "" {
				cand.NZBURL = strings.TrimSpace(q.NZBURL)
			}
		}
		cand = r.stripCandidateAuth(cand)
		res, err := r.checkUsenetCandidate(ctx, cand)
		if err != nil {
			return nil, err
		}
		cand.UsenetCheck = &res
		return []core.SourceCandidate{r.publicLegacyCandidate(ctx, cand)}, nil

	case "debrid_acquire":
		if r.debrid == nil {
			return nil, core.ErrNotAvailable
		}
		cand := core.SourceCandidate{
			InfoHash:   strings.ToLower(strings.TrimSpace(q.InfoHash)),
			Magnet:     strings.TrimSpace(q.Magnet),
			TorrentURL: strings.TrimSpace(q.TorrentURL),
			TorrentRef: strings.TrimSpace(q.TorrentRef),
		}
		if q.Candidate != nil {
			cand = *q.Candidate
			cand.InfoHash = strings.ToLower(strings.TrimSpace(cand.InfoHash))
			cand.Magnet = strings.TrimSpace(cand.Magnet)
			cand.TorrentURL = strings.TrimSpace(cand.TorrentURL)
			if q.InfoHash != "" {
				cand.InfoHash = strings.ToLower(strings.TrimSpace(q.InfoHash))
			}
			if q.Magnet != "" {
				cand.Magnet = strings.TrimSpace(q.Magnet)
			}
			if q.TorrentRef != "" {
				cand.TorrentRef = strings.TrimSpace(q.TorrentRef)
			}
			if q.TorrentURL != "" {
				cand.TorrentURL = strings.TrimSpace(q.TorrentURL)
			}
		}
		cand = r.stripCandidateAuth(cand)
		fileCount := max(q.FileCount, cand.FileCount, len(cand.Files))
		rel, err := releaseFromHint(core.ReleaseHint{Candidate: &cand, FileCount: fileCount}, "plugin-kind-acquire")
		if err != nil {
			return nil, err
		}
		rel.Protocol = core.ProtocolTorrent
		workKey := "debrid_acquire:" + rel.ID
		if handed, ok := r.takeSourceHandoff(workKey); ok {
			noteResolveSourceWork(ctx, workKey, true)
			return handed, nil
		}
		endWork := beginResolveSourceWork(ctx, workKey)
		defer endWork()
		raw, _, err := r.runDetachedSourceFlight(ctx, workKey, sourceDebridAcquireTimeout, func(workCtx context.Context) (any, error) {
			outRel := rel
			if outRel.InfoHash == "" && outRel.MagnetURL == "" && (outRel.NZBURL != "" || outRel.TorrentRef != "") {
				resolved, err := r.resolveTorrent(workCtx, outRel)
				if err != nil {
					return nil, err
				}
				outRel = resolved
			}
			payloadBytes, err := r.debrid.Acquire(workCtx, outRel)
			if err != nil {
				return nil, err
			}
			var cachedBy []core.DebridServiceRef
			if p, err := debrid.DecodePayload(payloadBytes); err == nil && p.Provider != "" {
				if refLookup, ok := r.debrid.(debridServiceRefBackend); ok {
					if ref, ok := refLookup.ServiceRef(p.Provider, p.Kind); ok {
						cachedBy = []core.DebridServiceRef{ref}
					}
				} else {
					cachedBy = []core.DebridServiceRef{{ID: p.Provider, Name: p.Provider}}
				}
			}
			return []core.SourceCandidate{r.publicReleaseLinks(core.SourceCandidate{
				InfoHash:   outRel.InfoHash,
				Magnet:     outRel.MagnetURL,
				TorrentURL: cand.TorrentURL,
				TorrentRef: cand.TorrentRef,
				Cached:     true,
				CachedBy:   cachedBy,
			})}, nil
		}, func(res any) {
			if cands, ok := res.([]core.SourceCandidate); ok {
				r.storeSourceHandoff(workKey, cands)
			}
		})
		if err != nil {
			return nil, err
		}
		cands, _ := raw.([]core.SourceCandidate)
		return cloneSourceCandidates(cands), nil

	default:
		return nil, fmt.Errorf("kindresolver: unknown source query kind %q", q.Kind)
	}
}

func (r *KindResolver) loadAuthorized(ctx context.Context, viewer *core.Viewer, key core.RatingKey) (core.KindItem, core.Library, pluginitems.KindRecord, error) {
	if viewer == nil || viewer.ID <= 0 {
		return core.KindItem{}, core.Library{}, pluginitems.KindRecord{}, core.ErrNotFound
	}
	if r.authorize != nil {
		if err := r.authorize(ctx, viewer, key); err != nil {
			return core.KindItem{}, core.Library{}, pluginitems.KindRecord{}, err
		}
	}
	item, libID, err := r.items.GetByKey(ctx, key)
	if err != nil {
		return core.KindItem{}, core.Library{}, pluginitems.KindRecord{}, core.ErrNotFound
	}
	lib, err := r.libs.Get(ctx, libID)
	if err != nil || lib.Hidden || !viewer.AllowsLibrary(lib.ID) {
		return core.KindItem{}, core.Library{}, pluginitems.KindRecord{}, core.ErrNotFound
	}
	rec, err := r.items.GetKindByLibrary(ctx, libID)
	if err != nil {
		return core.KindItem{}, core.Library{}, pluginitems.KindRecord{}, core.ErrNotFound
	}
	if r.catalog != nil && !r.catalog.IsPluginLibraryActive(ctx, lib) {
		return core.KindItem{}, core.Library{}, pluginitems.KindRecord{}, core.ErrNotFound
	} else if r.catalog == nil && r.mgr != nil {
		pr, err := r.mgr.Get(ctx, rec.PluginID)
		if err != nil || !pr.Enabled {
			return core.KindItem{}, core.Library{}, pluginitems.KindRecord{}, core.ErrNotFound
		}
	}
	r.items.PinItemAsync(item.Ref)
	if r.authorize == nil && viewer.MaxContentRating != "" {
		proj := pluginitems.Project(item, rec.Decl, lib.Present, lib.CardShape)
		cr := proj.ContentRating
		if cr == "" {
			cr = extractContentRating(item.Fields)
		}
		if cr != "" && contentrating.Exceeds(cr, viewer.MaxContentRating) {
			return core.KindItem{}, core.Library{}, pluginitems.KindRecord{}, core.ErrNotFound
		}
	}
	return item, lib, rec, nil
}

func (r *KindResolver) collectPlayableLeaves(ctx context.Context, rec pluginitems.KindRecord, parent core.KindItem) ([]core.KindItem, error) {
	if parent.Unavailable != nil {
		return nil, core.ErrNotAvailable
	}
	lv, _, ok := rec.Decl.Level(parent.Level)
	if ok && lv.Playable && parent.Playable {
		return []core.KindItem{parent}, nil
	}
	kindMedia := ResolveKindMediaClass(rec.Decl, rec.Decl.Present)
	parentMedia := rec.Decl.EffectiveLevelMedia(parent.Level, kindMedia)
	var leaves []core.KindItem
	queue := []core.ItemRef{parent.Ref}
	const maxLeaves = 2000
	for len(queue) > 0 && len(leaves) < maxLeaves {
		cur := queue[0]
		queue = queue[1:]
		cursor := ""
		for {
			page, err := r.items.Children(ctx, cur, cursor, 200)
			if err != nil {
				return nil, err
			}
			for _, child := range page.Items {
				if child.Unavailable != nil {
					continue
				}
				// Stay within the container's own media class when expanding
				// playable leaves (e.g. artist Play/Shuffle stays on audio
				// album/track levels and excludes video concerts).
				if rec.Decl.EffectiveLevelMedia(child.Level, kindMedia) != parentMedia {
					continue
				}
				clv, _, cok := rec.Decl.Level(child.Level)
				if cok && clv.Playable && child.Playable {
					leaves = append(leaves, child)
					if len(leaves) >= maxLeaves {
						break
					}
				} else {
					queue = append(queue, child.Ref)
				}
			}
			if page.NextCursor == "" || len(page.Items) == 0 || len(leaves) >= maxLeaves {
				break
			}
			cursor = page.NextCursor
		}
	}
	return leaves, nil
}

// skippedSources collects candidates that failed before a later one opened
// during one item open, so the resolver can decline to save a fallback as
// the item's resolution when an earlier candidate failed because the
// plugin's answer was wrong (#641).
type skippedSources struct {
	failures        []kindstream.SourceFailure
	attempts        []kindstream.SourceFailure
	anyPluginAnswer bool
}

func (s *skippedSources) resetAttempts() {
	if s == nil {
		return
	}
	s.attempts = nil
}

func (s *skippedSources) pluginAnswerFailed() bool {
	if s.anyPluginAnswer {
		return true
	}
	for _, f := range s.failures {
		if f.PluginAnswer() {
			return true
		}
	}
	return false
}

func (r *KindResolver) kindstreamDeps(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, partIdx *int, skipped *skippedSources) kindstream.Deps {
	var quota blobcache.Quota
	if r.catalog != nil && lib.Key != "" {
		quota = r.catalog.QuotaFor(rec.PluginID, lib.Key)
	} else {
		quota = blobcache.Quota{
			MaxBytes: rec.Decl.Cache.MaxBytes,
			TTL:      parseKindDuration(rec.Decl.Cache.TTL, 0),
		}
	}
	var ns *blobcache.Namespace
	if r.blobs != nil && lib.Key != "" {
		r.blobs.BindPluginLibrary(rec.PluginID, lib.Key)
		ns = r.blobs.Namespace(lib.Key, quota)
	}
	var allowHost func(string) bool
	var secretFn func(context.Context, string) (string, error)
	if r.mgr != nil {
		// The byte source outlives the prepare request (the client fetches
		// the media later), so the allowlist check must not use the
		// request's cancellable context.
		allowCtx := context.WithoutCancel(ctx)
		allowHost = func(host string) bool {
			hctx, cancel := context.WithTimeout(allowCtx, 5*time.Second)
			defer cancel()
			return r.mgr.AllowNetworkHost(hctx, rec.PluginID, host)
		}
		host := r.mgr.HostFor(rec.PluginID)
		secretFn = func(sctx context.Context, name string) (string, error) {
			return host.SecretGet(sctx, name)
		}
	}
	defaultMIME := kindDefaultMIMEForLevel(rec.Decl, item.Level)
	defaultCategory := kindDefaultCategoryForLevel(rec.Decl, item.Level)
	var sourceRouter kindstream.SourceRouter
	if r.resolver != nil {
		sourceRouter = r.resolver
	}
	var itemDur time.Duration
	if partIdx != nil && *partIdx >= 0 {
		if parts := core.ExtractKindItemParts(item.Hints); *partIdx < len(parts) && parts[*partIdx].DurationSec > 0 {
			itemDur = time.Duration(parts[*partIdx].DurationSec * float64(time.Second))
		}
	}
	if itemDur <= 0 {
		itemDur = extractItemDuration(item, rec.Decl)
	}
	var routeTitle *core.Title
	if itemDur > 0 || item.Title != "" {
		routeTitle = &core.Title{
			Title:   item.Title,
			Runtime: itemDur,
		}
	}
	return kindstream.Deps{
		OpenFile: func(ctx context.Context, src core.PlayableSource) (kindstream.ByteSource, error) {
			s := &StreamSources{mgr: r.mgr}
			resolved, _, err := s.validateLocalFile(ctx, rec.PluginID, src.Path)
			if err != nil {
				return nil, err
			}
			f, err := s.openGrantedLocalFile(ctx, rec.PluginID, resolved)
			if err != nil {
				return nil, err
			}
			info, err := f.Stat()
			if err != nil {
				f.Close()
				return nil, err
			}
			if !info.Mode().IsRegular() {
				f.Close()
				return nil, core.ErrSourceUnavailable
			}
			return &kindLocalFile{File: f, size: info.Size(), mime: src.MIME}, nil
		},
		HTTP:  r.http,
		Cache: ns,
		OpenRelease: func(octx context.Context, hint core.ReleaseHint) (core.MediaSource, error) {
			return r.openReleaseWithCategory(octx, hint, defaultCategory)
		},
		OpenReleaseSource: func(octx context.Context, src core.PlayableSource) (core.MediaSource, error) {
			return r.openReleaseForSourceWithCategory(octx, src, defaultMIME, defaultCategory, secretFn)
		},
		DefaultReleaseMIME:   defaultMIME,
		CandidateOpenTimeout: r.candidateOpenTimeout,
		Secret:               secretFn,
		AllowHost:            allowHost,
		MaxDownloadBytes:     quota.EffectiveMaxBytes(),
		ItemIndex:            effectiveItemIndex(item.Index, partIdx),
		Backoff:              r.backoff,
		SourceRouter:         sourceRouter,
		SourceRelease: func(src core.PlayableSource) core.Release {
			return playableSourceRelease(src, item.Title)
		},
		Title: routeTitle,
		ProbeMedia: func(pctx context.Context, src core.PlayableSource, bs kindstream.ByteSource) (*core.MediaInfo, error) {
			return r.probeOpenByteSource(pctx, rec.PluginID, src, defaultMIME, bs)
		},
		OnSourceFailed: func(attempt, total int, src core.PlayableSource, took time.Duration, err error) {
			if skipped != nil {
				skipped.attempts = append(skipped.attempts, kindstream.SourceFailure{Source: src, Err: err})
				if errors.Is(err, kindstream.ErrPluginAnswer) {
					skipped.anyPluginAnswer = true
				}
			}
			if r.log == nil {
				return
			}
			r.log.Warn("kind source open attempt failed",
				"plugin", rec.PluginID,
				"library", lib.Key,
				"item", item.ID,
				"attempt", attempt,
				"candidates", total,
				"source", logredact.Source(src.ID),
				"pattern", logredact.Source(src.Pattern),
				"took", took.Round(time.Millisecond),
				"err", err,
			)
		},
		OnSourceSkipped: func(failures []kindstream.SourceFailure, winning core.PlayableSource) {
			if skipped != nil {
				skipped.failures = append(skipped.failures, failures...)
			}
			for _, f := range failures {
				if r.log == nil {
					break
				}
				r.log.Warn("kind source failed to open; serving a later source",
					"plugin", rec.PluginID,
					"library", lib.Key,
					"item", item.ID,
					"source", logredact.Source(f.Source.ID),
					"pattern", logredact.Source(f.Source.Pattern),
					"winning", logredact.Source(winning.ID),
					"err", f.Err,
				)
			}
		},
	}
}

func (r *KindResolver) openItemByteSource(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, partIdx *int, accept []string) (kindstream.ByteSource, error) {
	bs, _, err := r.openItemByteSourceExcluding(ctx, rec, lib, item, partIdx, accept, nil)
	return bs, err
}

func flightKeyForItem(ref core.ItemRef, partIdx *int) resolveFlightKey {
	return flightKeyForItemSource(ref, partIdx, "")
}

func flightKeyForItemSource(ref core.ItemRef, partIdx *int, source string) resolveFlightKey {
	fk := resolveFlightKey{ref: ref, part: -1, source: source}
	if partIdx != nil {
		fk.part = *partIdx
	}
	return fk
}

func (r *KindResolver) openItemByteSourceExcluding(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, partIdx *int, accept, exclude []string) (kindstream.ByteSource, core.PlayableSource, error) {
	if item.Unavailable != nil {
		return nil, core.PlayableSource{}, core.ErrNotAvailable
	}
	skipped := &skippedSources{}
	ksDeps := r.kindstreamDeps(ctx, rec, lib, item, partIdx, skipped)
	fk := flightKeyForItem(item.Ref, partIdx)

	saveAndReturn := func(bs kindstream.ByteSource, winning core.PlayableSource) (kindstream.ByteSource, core.PlayableSource, error) {
		if winning.Size <= 0 && bs.Size() > 0 {
			winning.Size = bs.Size()
		}
		if winning.MIME == "" && bs.MIME() != "" {
			winning.MIME = bs.MIME()
		}
		// A fallback that opened only because an earlier candidate failed
		// on the plugin's own answer is not saved as the item's
		// resolution, and the broken candidate list is dropped so the next
		// play calls /resolve again instead of replaying cached candidates
		// (#641). A source whose declared digest is still verifying is not
		// saved either (#642).
		pluginBad := skipped.pluginAnswerFailed()
		if pluginBad {
			r.deleteCandidates(fk)
			if r.items != nil {
				_ = r.items.InvalidatePartResolution(ctx, item.Ref, partIdx)
			}
		}
		save := !pluginBad
		if dr, ok := bs.(kindstream.DigestReporter); ok {
			if declared, verified := dr.DigestState(); declared && !verified {
				save = false
			}
		}
		if save && r.items != nil {
			_ = r.items.SavePartResolution(ctx, item.Ref, partIdx, winning)
		}
		return bs, winning, nil
	}

	// 1. Check persisted positive/negative resolution cache first so repeat
	// opens reuse a fresh source, or a previously opened NZB that the vault
	// still has. Respect explicit exclusions and provider backoff.
	var backingOffCached *core.PlayableSource
	var cachedBudgetExhausted bool
	cachedSrc, ok, expired, err := r.items.GetPartResolutionForOpen(ctx, item.Ref, partIdx)
	if err != nil {
		return nil, core.PlayableSource{}, err
	}
	if ok && (!expired || r.canReuseVaultedSource(cachedSrc)) {
		cachedID := playableSourceRelease(cachedSrc, item.Title).ID
		if slices.Contains(exclude, cachedID) {
			_ = r.items.InvalidatePartResolution(ctx, item.Ref, partIdx)
		} else if r.isSourceBackingOff(cachedSrc) {
			if !expired {
				c := cachedSrc
				backingOffCached = &c
			}
		} else {
			openCtx, openCancel := withHardTimeout(ctx, r.candidateOpenTimeout)
			if expired {
				openCtx = newznab.WithVaultedPluginNZB(openCtx)
			}
			cachedStart := time.Now()
			bs, oerr := kindstream.Open(openCtx, cachedSrc, ksDeps)
			openCancel()
			if oerr == nil {
				if cachedSrc.Size <= 0 && bs.Size() > 0 {
					cachedSrc.Size = bs.Size()
					if cachedSrc.MIME == "" {
						cachedSrc.MIME = bs.MIME()
					}
					if !expired {
						_ = r.items.SavePartResolution(ctx, item.Ref, partIdx, cachedSrc)
					}
				}
				return bs, cachedSrc, nil
			}
			if ctx.Err() != nil {
				return nil, core.PlayableSource{}, ctx.Err()
			}
			if r.log != nil {
				r.log.Warn("cached kind resolution failed to open; falling back to re-resolve",
					"plugin", rec.PluginID,
					"library", lib.Key,
					"item", item.ID,
					"source", logredact.Source(cachedSrc.ID),
					"pattern", logredact.Source(cachedSrc.Pattern),
					"took", time.Since(cachedStart).Round(time.Millisecond),
					"err", oerr,
				)
			}
			cachedBudgetExhausted = errors.Is(oerr, core.ErrBudgetExhausted)
			if !expired {
				// Cached source went stale upstream: invalidate and fall through.
				_ = r.items.InvalidatePartResolution(ctx, item.Ref, partIdx)
				var grabRefusal *newznab.GrabRefusal
				if !errors.As(oerr, &grabRefusal) {
					r.deleteCandidates(fk)
				}
			}
		}
	}

	// 2. On failover or provider backoff, try remaining candidates from the
	// stored candidate list of the original kind resolve before re-calling
	// the plugin runtime.
	if len(exclude) > 0 || backingOffCached != nil {
		if stored, hasStored, cerr := r.cachedCandidatesFor(ctx, item, partIdx); cerr == nil && hasStored {
			cands := r.filterAndOrderCandidates(stored, item.Title, exclude)
			hasReady := slices.ContainsFunc(cands, func(s core.PlayableSource) bool { return !r.isSourceBackingOff(s) })
			if len(cands) > 0 && (backingOffCached == nil || hasReady) {
				bs, winning, oerr := kindstream.OpenSources(ctx, cands, ksDeps)
				if oerr == nil {
					return saveAndReturn(bs, winning)
				}
				cachedBudgetExhausted = cachedBudgetExhausted || errors.Is(oerr, core.ErrBudgetExhausted)
			}
		}
	}

	// 3. Resolve (or re-resolve excluding failed releases) via the plugin.
	// On a normal (non-failover) open, reuse warm cached candidates first so
	// listing versions and then pressing Play only calls /resolve once.
	var sources []core.PlayableSource
	if len(exclude) > 0 {
		sources, err = r.resolveItemSourcesExcluding(ctx, rec, lib, item, partIdx, accept, exclude)
	} else {
		sources, err = r.resolveCandidates(ctx, rec, lib, item, partIdx, accept)
	}
	if err != nil {
		if backingOffCached != nil {
			bs, oerr := kindstream.Open(ctx, *backingOffCached, ksDeps)
			if oerr == nil {
				return saveAndReturn(bs, *backingOffCached)
			}
			cachedBudgetExhausted = cachedBudgetExhausted || errors.Is(oerr, core.ErrBudgetExhausted)
		}
		if cachedBudgetExhausted && errors.Is(err, core.ErrNotAvailable) {
			_ = r.items.InvalidatePartResolution(ctx, item.Ref, partIdx)
			r.recordKindFailure(ctx, item, core.ErrBudgetExhausted)
			return nil, core.PlayableSource{}, core.ErrBudgetExhausted
		}
		return nil, core.PlayableSource{}, err
	}

	cands := r.filterAndOrderCandidates(sources, item.Title, exclude)
	if len(cands) == 0 {
		if backingOffCached != nil {
			bs, oerr := kindstream.Open(ctx, *backingOffCached, ksDeps)
			if oerr == nil {
				return saveAndReturn(bs, *backingOffCached)
			}
			cachedBudgetExhausted = cachedBudgetExhausted || errors.Is(oerr, core.ErrBudgetExhausted)
		}
		if cachedBudgetExhausted {
			r.recordKindFailure(ctx, item, core.ErrBudgetExhausted)
			return nil, core.PlayableSource{}, core.ErrBudgetExhausted
		}
		return nil, core.PlayableSource{}, core.ErrNotAvailable
	}
	openStart := time.Now()
	skipped.resetAttempts()
	bs, winning, err := kindstream.OpenSources(ctx, cands, ksDeps)
	if err != nil {
		if qbs, qwin, qerr, settled := r.settleUncachedReleases(ctx, skipped.attempts, ksDeps); settled {
			if qerr == nil {
				return saveAndReturn(qbs, qwin)
			}
			err = qerr
		}
		if skipped.pluginAnswerFailed() {
			r.deleteCandidates(fk)
			if r.items != nil {
				_ = r.items.InvalidatePartResolution(ctx, item.Ref, partIdx)
			}
		}
		if r.log != nil {
			if errors.Is(err, core.ErrPreparing) {
				r.log.Info("kindresolver: release download queued; item preparing",
					"plugin", rec.PluginID,
					"library", lib.Key,
					"item", item.ID,
					"candidates", len(cands),
					"took", time.Since(openStart).Round(time.Millisecond),
					"err", err,
				)
			} else if errors.Is(err, core.ErrArchiveUnstreamable) {
				r.log.Warn("this archive format or compression method cannot be streamed",
					"plugin", rec.PluginID,
					"library", lib.Key,
					"item", item.ID,
					"candidates", len(cands),
					"took", time.Since(openStart).Round(time.Millisecond),
					"err", err,
				)
			} else {
				r.log.Warn("kindresolver: open kind sources failed",
					"plugin", rec.PluginID,
					"library", lib.Key,
					"item", item.ID,
					"candidates", len(cands),
					"took", time.Since(openStart).Round(time.Millisecond),
					"err", err,
				)
			}
		}
		r.recordKindFailure(ctx, item, err)
		return nil, core.PlayableSource{}, err
	}
	return saveAndReturn(bs, winning)
}

func (r *KindResolver) debridCachedOnly() bool {
	if r == nil || r.debrid == nil {
		return true
	}
	if cfg, ok := r.debrid.(debridConfigBackend); ok {
		return cfg.Config().CachedOnly
	}
	if _, canQueue := r.debrid.(debridDownloadQueuer); canQueue {
		return false
	}
	return true
}

func (r *KindResolver) allowUncachedReleaseQueue(ctx context.Context) bool {
	if r.debridCachedOnly() {
		return false
	}
	if core.IsBackgroundCheck(ctx) || core.IsSpeculative(ctx) || core.IsUnattendedSearch(ctx) {
		return false
	}
	switch core.IndexerPurpose(ctx) {
	case core.IndexerPurposeWarmup, core.IndexerPurposeAvailability, core.IndexerPurposeDetail, core.IndexerPurposeFeed:
		return false
	}
	return true
}

func preparingQueuedError(err error) error {
	return &core.PreparingError{
		RetryAfter: defaultDebridPrepareRetry,
		Err:        &core.UncachedError{Err: err, CachedOnly: false},
	}
}

func isTorrentReleaseSource(src core.PlayableSource) bool {
	pattern := strings.ToLower(strings.TrimSpace(src.Pattern))
	if pattern == "" && src.Release != nil && src.Container == nil {
		pattern = kindstream.PatternRelease
	}
	if pattern != kindstream.PatternRelease || src.Release == nil {
		return false
	}
	rel := src.Release
	if strings.TrimSpace(rel.InfoHash) != "" || strings.TrimSpace(rel.Magnet) != "" || strings.TrimSpace(rel.TorrentURL) != "" || rel.TorrentRef != "" {
		return true
	}
	if rel.Candidate != nil {
		return strings.TrimSpace(rel.Candidate.InfoHash) != "" ||
			strings.TrimSpace(rel.Candidate.Magnet) != "" ||
			strings.TrimSpace(rel.Candidate.TorrentURL) != "" || rel.Candidate.TorrentRef != ""
	}
	return false
}

func isPermanentReleaseFailure(err error) bool {
	for _, target := range []error{
		core.ErrReleaseRejected,
		core.ErrArchiveUnstreamable,
		core.ErrNoUsableFile,
		core.ErrArticleMissing,
		core.ErrArticleCorrupt,
		core.ErrWrongContent,
		kindstream.ErrPluginAnswer,
		archive.ErrMissingVolume,
		archive.ErrCorrupt,
		archive.ErrCompressed,
		archive.ErrEncrypted,
		archive.ErrSolid,
		archive.ErrUnsupported,
		nzb.ErrInvalid,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// settleUncachedReleases applies Ether's uncached-torrent policy after every
// candidate failed its cached-first open pass: if a candidate is already being
// followed as a queued download it answers preparing (ErrPreparing wrapping
// ErrDownloadQueued); otherwise, when every failure was either an uncached
// torrent or a permanent non-torrent failure, it honours the owner's "Cached
// torrents only" setting and per-release cachedOnly opt-out, queueing at most
// one uncached torrent download (trying up to maxKindQueueAttempts on refusal,
// stopping immediately on active-download limits or a ready/queued torrent).
func (r *KindResolver) settleUncachedReleases(ctx context.Context, failures []kindstream.SourceFailure, ksDeps kindstream.Deps) (kindstream.ByteSource, core.PlayableSource, error, bool) {
	if len(failures) == 0 || ctx.Err() != nil {
		return nil, core.PlayableSource{}, nil, false
	}
	var (
		uncached          []int
		queuedErr         error
		otherNonPermanent bool
	)
	for i, f := range failures {
		if isTorrentReleaseSource(f.Source) && errors.Is(f.Err, core.ErrNotCached) {
			if errors.Is(f.Err, core.ErrDownloadQueued) && !f.Source.Release.CachedOnly && queuedErr == nil {
				queuedErr = f.Err
			}
			uncached = append(uncached, i)
			continue
		}
		if !isPermanentReleaseFailure(f.Err) {
			otherNonPermanent = true
		}
	}
	if queuedErr != nil {
		return nil, core.PlayableSource{}, preparingQueuedError(queuedErr), true
	}
	if len(uncached) == 0 || otherNonPermanent {
		return nil, core.PlayableSource{}, nil, false
	}

	var (
		errs      []error
		queueable []int
	)
	for _, i := range uncached {
		f := failures[i]
		ferr := f.Err
		if f.Source.Release.CachedOnly && errors.Is(ferr, core.ErrDownloadQueued) {
			ferr = core.ErrNotCached
		}
		errs = append(errs, ferr)
		if errors.Is(f.Err, core.ErrPrivateUncached) || f.Source.Release.CachedOnly {
			continue
		}
		queueable = append(queueable, i)
	}

	ownerCachedOnly := r.debridCachedOnly()
	out := &core.UncachedError{
		Err:        errors.Join(errs...),
		CachedOnly: ownerCachedOnly && len(queueable) > 0,
	}
	if len(queueable) == 0 || !r.allowUncachedReleaseQueue(ctx) {
		return nil, core.PlayableSource{}, out, true
	}
	if _, canQueue := r.debrid.(debridDownloadQueuer); !canQueue {
		return nil, core.PlayableSource{}, out, true
	}

	qDeps := ksDeps
	qDeps.OnSourceFailed = nil
	qDeps.OnSourceSkipped = nil
	var refused []error
	for n, i := range queueable {
		if n == maxKindQueueAttempts {
			break
		}
		cand := failures[i].Source
		candCtx := withReleaseQueueMode(ctx)
		var candCancel context.CancelFunc
		if ksDeps.CandidateOpenTimeout > 0 {
			candCtx, candCancel = context.WithTimeout(candCtx, ksDeps.CandidateOpenTimeout)
		}
		bs, qerr := kindstream.Open(candCtx, cand, qDeps)
		if candCancel != nil {
			candCancel()
		}
		if qerr == nil {
			return bs, cand, nil, true
		}
		if ctx.Err() != nil {
			return nil, core.PlayableSource{}, ctx.Err(), true
		}
		if errors.Is(qerr, core.ErrDownloadQueued) {
			return nil, core.PlayableSource{}, preparingQueuedError(qerr), true
		}
		refused = append(refused, qerr)
		if errors.Is(qerr, core.ErrDownloadLimit) {
			break
		}
	}
	out.Err = errors.Join(refused...)
	if !errors.Is(out.Err, core.ErrDownloadRefused) {
		out.Err = fmt.Errorf("%w: %w", core.ErrDownloadRefused, out.Err)
	}
	return nil, core.PlayableSource{}, out, true
}

func (r *KindResolver) storeCandidates(fk resolveFlightKey, sources []core.PlayableSource) {
	if r == nil || len(sources) == 0 {
		return
	}
	now := r.now()
	exp := now.Add(defaultKindCandidateCacheTTL)
	for _, s := range sources {
		if s.ExpiresAt > 0 {
			sExp := time.Unix(s.ExpiresAt, 0)
			if sExp.After(now) && sExp.Before(exp) {
				exp = sExp
			}
		}
	}
	cloned := slices.Clone(sources)
	r.candMu.Lock()
	defer r.candMu.Unlock()
	for k, v := range r.candCache {
		if !now.Before(v.expiresAt) {
			delete(r.candCache, k)
		}
	}
	if _, exists := r.candCache[fk]; !exists && len(r.candCache) >= maxKindCandidateCacheEntries {
		var oldestKey resolveFlightKey
		var oldestAt time.Time
		first := true
		for k, v := range r.candCache {
			if first || v.storedAt.Before(oldestAt) {
				oldestKey = k
				oldestAt = v.storedAt
				first = false
			}
		}
		if !first {
			delete(r.candCache, oldestKey)
		}
	}
	r.candCache[fk] = cachedKindCandidates{
		sources:   cloned,
		expiresAt: exp,
		storedAt:  now,
	}
}

func (r *KindResolver) cachedCandidates(fk resolveFlightKey) ([]core.PlayableSource, bool) {
	if r == nil {
		return nil, false
	}
	now := r.now()
	r.candMu.Lock()
	defer r.candMu.Unlock()
	entry, ok := r.candCache[fk]
	if !ok {
		return nil, false
	}
	if !now.Before(entry.expiresAt) {
		delete(r.candCache, fk)
		return nil, false
	}
	return slices.Clone(entry.sources), true
}

func (r *KindResolver) deleteCandidates(fk resolveFlightKey) {
	if r == nil {
		return
	}
	r.candMu.Lock()
	delete(r.candCache, fk)
	delete(r.detailMissUntil, fk)
	r.candMu.Unlock()
}

func (r *KindResolver) detailMissActive(fk resolveFlightKey) bool {
	if r == nil {
		return false
	}
	now := r.now()
	r.candMu.Lock()
	defer r.candMu.Unlock()
	until, ok := r.detailMissUntil[fk]
	if !ok {
		return false
	}
	if !now.Before(until) {
		delete(r.detailMissUntil, fk)
		return false
	}
	return true
}

func (r *KindResolver) noteDetailMiss(fk resolveFlightKey) {
	if r == nil {
		return
	}
	now := r.now()
	r.candMu.Lock()
	defer r.candMu.Unlock()
	if r.detailMissUntil == nil {
		r.detailMissUntil = make(map[resolveFlightKey]time.Time)
	}
	for k, u := range r.detailMissUntil {
		if !now.Before(u) {
			delete(r.detailMissUntil, k)
		}
	}
	if len(r.detailMissUntil) >= maxKindCandidateCacheEntries {
		clear(r.detailMissUntil)
	}
	r.detailMissUntil[fk] = now.Add(kindDetailMissCooloff)
}

func (r *KindResolver) speculativeSourceBudgetPaused(ctx context.Context) bool {
	if r == nil || !core.SpeculativeIndexerWork(ctx) {
		return false
	}
	purpose := core.IndexerPurpose(ctx)
	if purpose == "" {
		return false
	}
	now := r.now()
	r.specBudgetMu.Lock()
	until := r.specBudgetUntil[purpose]
	r.specBudgetMu.Unlock()
	return now.Before(until)
}

func (r *KindResolver) noteSpeculativeSourceBudgetExhausted(ctx context.Context) {
	if r == nil || !core.SpeculativeIndexerWork(ctx) {
		return
	}
	purpose := core.IndexerPurpose(ctx)
	if purpose == "" {
		return
	}
	r.specBudgetMu.Lock()
	if r.specBudgetUntil == nil {
		r.specBudgetUntil = make(map[string]time.Time)
	}
	r.specBudgetUntil[purpose] = r.now().Add(speculativeSourceBudgetCooloff)
	r.specBudgetMu.Unlock()
}

// cachedCandidatesFor checks the in-memory candidate cache first and falls
// back to the persisted plugin_resolutions.candidates column. Expired sources
// are filtered against r.now() so tests with fake clocks expire cleanly.
func (r *KindResolver) cachedCandidatesFor(ctx context.Context, item core.KindItem, partIdx *int) ([]core.PlayableSource, bool, error) {
	fk := flightKeyForItem(item.Ref, partIdx)
	nowUnix := r.now().Unix()
	filterLive := func(srcs []core.PlayableSource) []core.PlayableSource {
		live := make([]core.PlayableSource, 0, len(srcs))
		for _, s := range srcs {
			if s.ExpiresAt > 0 && nowUnix >= s.ExpiresAt {
				continue
			}
			// A Usenet release that already failed in playback is not
			// offered again from the cached candidate list (#665): the
			// check and real playback must agree.
			if r.usenetPlaybackFailed(s) {
				continue
			}
			live = append(live, s)
		}
		return live
	}
	if cached, ok := r.cachedCandidates(fk); ok {
		if live := filterLive(cached); len(live) > 0 {
			return live, true, nil
		}
		r.deleteCandidates(fk)
	}
	if r.items != nil {
		stored, ok, err := r.items.GetPartCandidates(ctx, item.Ref, partIdx)
		if err != nil {
			return nil, ok, err
		}
		if ok {
			if live := filterLive(stored); len(live) > 0 {
				r.storeCandidates(fk, live)
				return live, true, nil
			}
		}
	}
	return nil, false, nil
}

// resolveCandidates returns the cached candidate list when fresh (or an active
// negative cache error), calling the plugin's /resolve only on a cache miss.
func (r *KindResolver) resolveCandidates(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, partIdx *int, accept []string) ([]core.PlayableSource, error) {
	if sources, ok, err := r.cachedCandidatesFor(ctx, item, partIdx); ok {
		return sources, err
	}
	return r.resolveItemSources(ctx, rec, lib, item, partIdx, accept)
}

func (r *KindResolver) isUsenetBackingOff() bool {
	if r == nil {
		return false
	}
	if r.usenetServing != nil && !r.usenetServing() {
		return true
	}
	return r.resolver != nil && r.resolver.UsenetBackingOff()
}

func (r *KindResolver) isSourceBackingOff(src core.PlayableSource) bool {
	if r == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(src.Pattern), kindstream.PatternRelease) && src.Release != nil {
		rel, err := releaseFromHint(*src.Release, "")
		if err == nil && rel.Protocol == core.ProtocolUsenet && rel.InfoHash == "" && rel.MagnetURL == "" {
			return r.isUsenetBackingOff()
		}
		return false
	}
	if src.URL != "" && r.backoff != nil {
		return r.backoff.IsBackingOff(src.URL)
	}
	return false
}

func (r *KindResolver) filterAndOrderCandidates(sources []core.PlayableSource, defaultTitle string, exclude []string) []core.PlayableSource {
	if len(sources) == 0 {
		return nil
	}
	nowUnix := r.now().Unix()
	ready := make([]core.PlayableSource, 0, len(sources))
	backingOff := make([]core.PlayableSource, 0)
	for _, src := range sources {
		if src.ExpiresAt > 0 && nowUnix >= src.ExpiresAt {
			continue
		}
		id := playableSourceRelease(src, defaultTitle).ID
		if len(exclude) > 0 && slices.Contains(exclude, id) {
			continue
		}
		if r.isSourceBackingOff(src) {
			backingOff = append(backingOff, src)
		} else {
			ready = append(ready, src)
		}
	}
	return append(ready, backingOff...)
}

func (r *KindResolver) resolveItemSourcesExcluding(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, partIdx *int, accept, exclude []string) ([]core.PlayableSource, error) {
	return r.resolveSourcesFlight(ctx, rec, lib, item, partIdx, "", accept, exclude)
}

func (r *KindResolver) resolveItemSourcesForSource(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, partIdx *int, sourceID string, accept []string) ([]core.PlayableSource, error) {
	return r.resolveSourcesFlight(ctx, rec, lib, item, partIdx, sourceID, accept, nil)
}

func (r *KindResolver) resolveItemSources(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, partIdx *int, accept []string) ([]core.PlayableSource, error) {
	return r.resolveSourcesFlight(ctx, rec, lib, item, partIdx, "", accept, nil)
}

func parsePreparingReply(body []byte) error {
	var pending plugins.PendingReply
	if len(body) > 0 {
		_ = json.Unmarshal(body, &pending)
	}
	return core.PreparingFromPending(pending.RetryInMS)
}

func (r *KindResolver) callPluginResolve(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, partIdx *int, source string, accept, exclude []string) (_ []core.PlayableSource, err error) {
	defer func() {
		r.recordKindFailure(ctx, item, err)
		err = logredact.SourceError(err)
	}()
	pr, err := r.mgr.Get(ctx, rec.PluginID)
	if err != nil || !pr.Enabled || !pr.Manifest.HasCapability(plugins.CapKindResolver) {
		return nil, core.ErrSourceUnavailable
	}
	var clientMap map[string]any
	if len(accept) > 0 || len(exclude) > 0 {
		clientMap = make(map[string]any, 2)
		if len(accept) > 0 {
			clientMap["accept"] = accept
		}
		if len(exclude) > 0 {
			clientMap["exclude"] = slices.DeleteFunc(slices.Clone(exclude), func(id string) bool {
				return strings.HasPrefix(id, "kind:http://") || strings.HasPrefix(id, "kind:https://")
			})
		}
	}
	body, err := json.Marshal(core.KindResolveRequest{
		LibraryKey: lib.Key,
		Item:       item,
		PartIndex:  partIdx,
		Source:     source,
		Client:     clientMap,
	})
	if err != nil {
		return nil, err
	}
	body, err = r.NormalizeStoredReleaseLinks(ctx, body)
	if err != nil {
		return nil, err
	}
	fk := flightKeyForItemSource(item.Ref, partIdx, source)
	fk.background = core.SpeculativeIndexerWork(ctx) || core.IsBackgroundCheck(ctx)
	searchState := resolveSearchStateFrom(ctx)
	if searchState == nil {
		ctx, searchState = withResolveSearchState(ctx)
	}
	ctx = plugins.WithResolveSliceHook(ctx, searchState.beginSlice)
	unit := resolveUnitKey{pluginID: rec.PluginID, library: lib.Key, parent: item.ParentID}
	if !item.Playable {
		unit.parent = ""
	}
	releaseUnit, err := r.acquireResolveUnit(ctx, unit)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		// A sibling's resolve of the same unit outlasted this call's
		// budget: the unit is still resolving, not unavailable.
		return nil, &core.PreparingError{RetryAfter: time.Second}
	}
	resp, err := r.mgr.Call(ctx, rec.PluginID, plugins.Request{
		Method: "POST",
		Path:   "/ether/v1/kinds/" + rec.KindKey + "/resolve",
		Body:   body,
	})
	releaseUnit()
	if err != nil {
		if errors.Is(err, core.ErrJobsBusy) {
			return nil, err
		}
		if errors.Is(err, plugins.ErrBusy) {
			return nil, fmt.Errorf("%w: %w", core.ErrSourceUnavailable, err)
		}
		var we *wasm.Error
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &core.PreparingError{RetryAfter: 2 * time.Second}
		}
		if errors.As(err, &we) && we.Status == wasm.StatusTimeout && r.allowTimeoutPreparing(fk, searchState) {
			return nil, &core.PreparingError{RetryAfter: 2 * time.Second}
		}
		return nil, fmt.Errorf("%w: %v", core.ErrSourceUnavailable, err)
	}
	if resp.Status == http.StatusAccepted {
		if searchState.indexerLookupLimitHit() {
			return nil, core.ErrSourceUnavailable
		}
		if searchState.hadBudgetSkip() && !searchState.hasPending() {
			r.clearResolveProgress(fk)
			return nil, core.ErrBudgetExhausted
		}
		return nil, parsePreparingReply(resp.Body)
	}
	if resp.Status == http.StatusNotFound {
		return r.emptyKindResolve(ctx, item, partIdx, fk, searchState, len(exclude) > 0)
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("%w: resolver status %d", core.ErrSourceUnavailable, resp.Status)
	}
	var resolved core.KindResolveResponse
	if err := json.Unmarshal(resp.Body, &resolved); err != nil {
		return nil, fmt.Errorf("%w: invalid resolver payload: %v", core.ErrSourceUnavailable, err)
	}
	if resolved.Pending {
		if searchState.indexerLookupLimitHit() {
			return nil, core.ErrSourceUnavailable
		}
		return nil, core.PreparingFromPending(resolved.RetryInMS)
	}
	resolvedParts := resolved.Parts
	if len(resolvedParts) == 0 && len(resolved.Sources) > 0 {
		for _, src := range resolved.Sources {
			if len(src.Parts) > 0 {
				resolvedParts = src.Parts
				break
			}
		}
	}
	if len(resolvedParts) > 0 && partIdx == nil && r.items != nil {
		_, _ = r.items.SaveParts(ctx, item.Ref, resolvedParts, "resolve")
	}
	for _, src := range resolved.Sources {
		if strings.EqualFold(strings.TrimSpace(src.Pattern), "file") {
			s := &StreamSources{mgr: r.mgr}
			if _, _, err := s.validateLocalFile(ctx, rec.PluginID, src.Path); err != nil {
				return nil, err
			}
		}
	}
	if len(resolved.Sources) == 0 {
		return r.emptyKindResolve(ctx, item, partIdx, fk, searchState, len(exclude) > 0)
	}
	if err := r.validateNoRawSecretsInHeaders(ctx, pr.ID, pr.Manifest.Ether.Permissions.Secrets, resolved.Sources); err != nil {
		return nil, err
	}
	if resolved.TTLSec > 0 {
		exp := r.now().Add(time.Duration(resolved.TTLSec) * time.Second).Unix()
		for i := range resolved.Sources {
			if resolved.Sources[i].ExpiresAt == 0 {
				resolved.Sources[i].ExpiresAt = exp
			}
		}
	}
	r.clearResolveProgress(fk)
	return resolved.Sources, nil
}

// validateNoRawSecretsInHeaders rejects resolver headers whose value equals or
// embeds a stored plugin secret, enforcing the {secret:name} placeholder rule.
func (r *KindResolver) validateNoRawSecretsInHeaders(ctx context.Context, pluginID string, secretNames []string, sources []core.PlayableSource) error {
	if r.mgr == nil || len(secretNames) == 0 {
		return nil
	}
	hasHeaders := false
	for _, src := range sources {
		if len(src.Headers) > 0 {
			hasHeaders = true
			break
		}
	}
	if !hasHeaders {
		return nil
	}
	host := r.mgr.HostFor(pluginID)
	var rawSecrets []string
	for _, name := range secretNames {
		if val, err := host.SecretGet(ctx, name); err == nil && val != "" {
			rawSecrets = append(rawSecrets, val)
		}
	}
	if len(rawSecrets) == 0 {
		return nil
	}
	for _, src := range sources {
		for hk, hv := range src.Headers {
			for _, raw := range rawSecrets {
				if hv == raw || strings.Contains(hv, raw) {
					return fmt.Errorf("%w: resolver header %q contains raw secret value; use {secret:name} placeholder", core.ErrSourceUnavailable, hk)
				}
			}
		}
	}
	return nil
}

func releaseFromHint(hint core.ReleaseHint, defaultTitle string) (core.Release, error) {
	var rel core.Release
	fileIndex := hint.FileIndex
	if fileIndex == nil && hint.Candidate != nil {
		fileIndex = hint.Candidate.FileIndex
	}
	if fileIndex != nil && *fileIndex < 0 {
		return core.Release{}, fmt.Errorf("release fileIndex must be non-negative")
	}
	if fileIndex != nil || strings.TrimSpace(hint.File) != "" {
		rel.Target = &core.ReleaseTarget{
			FileIndex:     fileIndex,
			ExactFile:     strings.TrimSpace(hint.File),
			AllowNonVideo: true,
		}
	}
	var torrentURL string
	if hint.Candidate != nil {
		rel.InfoHash = strings.ToLower(strings.TrimSpace(hint.Candidate.InfoHash))
		rel.MagnetURL = strings.TrimSpace(hint.Candidate.Magnet)
		torrentURL = strings.TrimSpace(hint.Candidate.TorrentURL)
		rel.TorrentRef = hint.Candidate.TorrentRef
		rel.NZBURL = strings.TrimSpace(hint.Candidate.NZBURL)
		rel.NZBRef = strings.TrimSpace(hint.Candidate.NZBRef)
		rel.Name = hint.Candidate.Title
		rel.Size = hint.Candidate.Size
		rel.Seeders = hint.Candidate.Seeders
		rel.Indexer = hint.Candidate.Indexer
		rel.IndexerID = hint.Candidate.IndexerID
		rel.Category = hint.Candidate.Category
		rel.FileCount = max(hint.Candidate.FileCount, len(hint.Candidate.Files))
	}
	if hint.FileCount > rel.FileCount {
		rel.FileCount = hint.FileCount
	}
	if h := strings.ToLower(strings.TrimSpace(hint.InfoHash)); h != "" {
		rel.InfoHash = h
	}
	if m := strings.TrimSpace(hint.Magnet); m != "" {
		rel.MagnetURL = m
	}
	if hint.TorrentRef != "" {
		rel.TorrentRef = hint.TorrentRef
	}
	if t := strings.TrimSpace(hint.TorrentURL); t != "" {
		torrentURL = t
	}
	if ref := strings.TrimSpace(hint.NZBRef); ref != "" {
		rel.NZBRef = ref
	}
	if u := strings.TrimSpace(hint.NZBURL); u != "" {
		rel.NZBURL = u
	}
	if strings.HasPrefix(strings.ToLower(torrentURL), "magnet:") {
		if rel.MagnetURL == "" {
			rel.MagnetURL = torrentURL
		}
		torrentURL = ""
	}
	if rel.InfoHash == "" && rel.MagnetURL != "" {
		if h := newznab.InfoHashFromMagnet(rel.MagnetURL); h != "" {
			rel.InfoHash = h
		}
	}
	switch {
	case rel.InfoHash != "" || rel.MagnetURL != "" || torrentURL != "" || rel.TorrentRef != "":
		rel.Protocol = core.ProtocolTorrent
		if torrentURL != "" {
			// Core's torrent resolvers (newznab/cardigann) read the torrent
			// download/page link from Release.NZBURL while Protocol is Torrent.
			rel.NZBURL = torrentURL
		} else {
			rel.NZBURL = ""
		}
	case rel.NZBURL != "" || rel.NZBRef != "":
		rel.Protocol = core.ProtocolUsenet
	case pluginReleaseOf(&hint) != nil:
		ref := hint.Candidate.PluginRelease
		rel.Protocol = "direct"
		rel.GUID = fmt.Sprintf("plugin:%s:lazy:%s", ref.Plugin, ref.ID)
	}
	assignKindReleaseID(&rel)
	if rel.Name == "" {
		rel.Name = firstNonEmpty(hint.FileHint, defaultTitle, rel.InfoHash, "kind-release")
	}
	return rel, nil
}

func assignKindReleaseID(rel *core.Release) {
	identity := firstNonEmpty(rel.InfoHash, rel.NZBURL, rel.MagnetURL, rel.GUID)
	if ref := firstNonEmpty(rel.NZBRef, rel.TorrentRef); identity == "" && ref != "" {
		identity = fmt.Sprintf("%x", sha256.Sum256([]byte(ref)))
	}
	rel.ID = "kind:" + identity
	if rel.Target != nil {
		target := *rel.Target
		// Keep the existing file selection hash independent of the pack alias.
		target.PackID = ""
		selection, _ := json.Marshal(&target)
		target.PackID = rel.ID
		rel.Target = &target
		rel.ID += fmt.Sprintf(":file:%x", sha256.Sum256(selection))
	}
}

func kindDefaultCategoryForLevel(decl plugins.MediaKindMeta, level string) int {
	if lv, _, ok := decl.Level(level); ok {
		switch strings.ToLower(strings.TrimSpace(lv.Media)) {
		case "video":
			return 2000
		case "music":
			return 3000
		case "audiobook":
			return 3030
		case "book":
			return 7000
		case "podcast", "photo":
			return 0
		}
	}
	return kindDefaultCategory(decl)
}

func kindDefaultCategory(decl plugins.MediaKindMeta) int {
	switch strings.ToLower(strings.TrimSpace(string(decl.Media))) {
	case "music":
		return 3000
	case "audiobook":
		return 3030
	case "book":
		return 7000
	}
	if decl.Present.MediaBrowser != nil {
		switch strings.ToLower(strings.TrimSpace(decl.Present.MediaBrowser.CollectionType)) {
		case "movies":
			return 2000
		case "tvshows":
			return 5000
		case "music":
			return 3000
		case "audiobooks":
			return 3030
		case "books":
			return 7000
		}
	}
	if decl.Present.Plex != nil {
		switch strings.ToLower(strings.TrimSpace(decl.Present.Plex.Base)) {
		case "movie":
			return 2000
		case "show":
			return 5000
		case "artist":
			return 3000
		}
	}
	return 0
}

func kindDefaultMIMEForLevel(decl plugins.MediaKindMeta, level string) string {
	if lv, _, ok := decl.Level(level); ok {
		switch strings.ToLower(strings.TrimSpace(lv.Media)) {
		case "video":
			return ""
		case "music", "audiobook", "podcast":
			return "audio/*"
		case "book":
			return "application/epub+zip"
		case "photo":
			return "image/*"
		}
	}
	return kindDefaultMIME(decl)
}

func kindDefaultMIME(decl plugins.MediaKindMeta) string {
	// The kind-level media class is the source of truth; per-app
	// presentations are only consulted when a manifest does not declare one.
	switch strings.ToLower(strings.TrimSpace(string(decl.Media))) {
	case "music", "audiobook", "podcast":
		return "audio/*"
	case "book":
		return "application/epub+zip"
	case "photo":
		return "image/*"
	case "video":
		return ""
	}
	if decl.Present.MediaBrowser != nil {
		switch strings.ToLower(strings.TrimSpace(decl.Present.MediaBrowser.CollectionType)) {
		case "music", "audiobooks":
			return "audio/*"
		case "books":
			return "application/epub+zip"
		case "photos":
			return "image/*"
		}
	}
	if decl.Present.Plex != nil {
		switch strings.ToLower(strings.TrimSpace(decl.Present.Plex.Base)) {
		case "artist":
			return "audio/*"
		case "photo":
			return "image/*"
		}
	}
	return ""
}

func releaseFromSource(src core.PlayableSource, defaultTitle, fallbackMIME string) (core.Release, error) {
	src = kindstream.NormalizeNZBSource(src)
	if src.Release == nil {
		return core.Release{}, core.ErrSourceUnavailable
	}
	rel, err := releaseFromHint(*src.Release, defaultTitle)
	if err != nil {
		return core.Release{}, err
	}
	effMIME := nzb.NormalizeMIME(src.MIME)
	if effMIME == "" {
		effMIME = nzb.NormalizeMIME(fallbackMIME)
	}
	var member string
	var memberIdx *int
	var track *core.TrackHint
	if src.Container != nil {
		if strings.TrimSpace(src.Release.File) == "" && strings.TrimSpace(src.Release.FileHint) != "" {
			// release.fileHint names the archive file inside the release while
			// src.Container selects the member inside that archive (#709).
			member = strings.TrimSpace(src.Release.FileHint)
		} else {
			member = strings.TrimSpace(src.Container.Member)
			memberIdx = src.Container.MemberIndex
		}
	} else {
		member = strings.TrimSpace(src.Release.FileHint)
	}
	if src.Release.Track != nil && (src.Release.Track.Number > 0 || strings.TrimSpace(src.Release.Track.Title) != "") {
		t := *src.Release.Track
		t.Title = strings.TrimSpace(t.Title)
		track = &t
	}
	if !strings.HasPrefix(effMIME, "video/") {
		if rel.Target != nil {
			if effMIME != "" {
				rel.Target.MIME = effMIME
			}
			if member != "" {
				rel.Target.Member = member
			}
			if memberIdx != nil {
				rel.Target.MemberIndex = memberIdx
			}
			if track != nil {
				rel.Target.Track = track
			}
		} else if nzb.IsNonVideoMIME(effMIME) || member != "" || memberIdx != nil || track != nil {
			rel.Target = &core.ReleaseTarget{
				AllowNonVideo: true,
				Member:        member,
				MemberIndex:   memberIdx,
				Track:         track,
				MIME:          effMIME,
			}
		}
	}
	if rel.Target != nil {
		assignKindReleaseID(&rel)
	}
	return rel, nil
}

func playableSourceRelease(src core.PlayableSource, defaultTitle string) core.Release {
	if strings.EqualFold(strings.TrimSpace(src.Pattern), kindstream.PatternRelease) && src.Release != nil {
		if rel, err := releaseFromSource(src, defaultTitle, ""); err == nil {
			if rel.Size <= 0 && src.Size > 0 {
				rel.Size = src.Size
			}
			rel.Info = releaseparse.Parse(firstNonEmpty(rel.Name, src.Release.FileHint, src.Label))
			if cand := src.Release.Candidate; cand != nil {
				if cand.Resolution > 0 {
					rel.Info.Height = cand.Resolution
				}
				if cand.Codec != "" && rel.Info.VideoCodec == "" {
					rel.Info.VideoCodec = strings.ToLower(strings.TrimSpace(cand.Codec))
				}
				if cand.Source != "" && rel.Info.Source == core.SourceUnknown {
					rel.Info.Source = kindReleaseSource(cand.Source)
				}
			}
			return rel
		}
	}
	containerSeed, _ := json.Marshal(src.Container)
	seed := fmt.Sprintf("%s|%s|%s|%s",
		strings.ToLower(strings.TrimSpace(src.Pattern)),
		strings.TrimSpace(src.URL),
		strings.TrimSpace(src.Path),
		string(containerSeed),
	)
	if blob := strings.TrimSpace(src.BlobRef); blob != "" {
		seed += "|blob:" + blob
	} else if strings.TrimSpace(src.URL) == "" && strings.TrimSpace(src.Path) == "" && src.Container == nil && strings.TrimSpace(src.ID) != "" {
		seed += "|id:" + strings.TrimSpace(src.ID)
	}
	sum := sha256.Sum256([]byte(seed))
	name := firstNonEmpty(src.Label, defaultTitle, "kind-stream")
	return core.Release{
		ID:       "kind:src:" + hex.EncodeToString(sum[:8]),
		Name:     name,
		Protocol: core.ProtocolHTTP,
		Size:     src.Size,
		Info:     releaseparse.Parse(firstNonEmpty(src.Label, src.Path, src.URL)),
	}
}

func (r *KindResolver) openRelease(ctx context.Context, hint core.ReleaseHint) (core.MediaSource, error) {
	return r.openReleaseWithCategory(ctx, hint, 0)
}

func (r *KindResolver) openReleaseWithCategory(ctx context.Context, hint core.ReleaseHint, fallbackCategory int) (core.MediaSource, error) {
	if r.engine == nil {
		return nil, core.ErrSourceUnavailable
	}
	if hint.CachedOnly && isReleaseQueueMode(ctx) {
		ctx = withoutReleaseQueueMode(ctx)
	}
	rel, err := releaseFromHint(hint, "")
	if err != nil {
		return nil, err
	}
	if ref := pluginReleaseOf(&hint); ref != nil && rel.Protocol == "direct" {
		return r.openPluginRelease(ctx, *ref, rel, "")
	}
	if rel.Category == 0 && fallbackCategory != 0 {
		rel.Category = fallbackCategory
	}
	return r.openPreparedRelease(ctx, rel)
}

func (r *KindResolver) openReleaseForSource(ctx context.Context, src core.PlayableSource, fallbackMIME string, secretFn func(context.Context, string) (string, error)) (core.MediaSource, error) {
	return r.openReleaseForSourceWithCategory(ctx, src, fallbackMIME, 0, secretFn)
}

func (r *KindResolver) openReleaseForSourceWithCategory(ctx context.Context, src core.PlayableSource, fallbackMIME string, fallbackCategory int, secretFn func(context.Context, string) (string, error)) (core.MediaSource, error) {
	if r.engine == nil {
		return nil, core.ErrSourceUnavailable
	}
	if src.Release != nil && src.Release.CachedOnly && isReleaseQueueMode(ctx) {
		ctx = withoutReleaseQueueMode(ctx)
	}
	rel, err := releaseFromSource(src, "", fallbackMIME)
	if err != nil {
		return nil, err
	}
	if ref := pluginReleaseOf(src.Release); ref != nil && rel.Protocol == "direct" {
		return r.openPluginRelease(ctx, *ref, rel, firstNonEmpty(src.MIME, fallbackMIME))
	}
	if rel.Category == 0 && fallbackCategory != 0 {
		rel.Category = fallbackCategory
	}
	if rel.Protocol == core.ProtocolUsenet && rel.Target != nil && rel.Target.AllowNonVideo && secretFn != nil {
		pwSecret := ""
		if src.Container != nil && strings.TrimSpace(src.Container.PasswordSecret) != "" {
			pwSecret = strings.TrimSpace(src.Container.PasswordSecret)
		} else if src.Release != nil {
			pwSecret = strings.TrimSpace(src.Release.PasswordSecret)
		}
		if pwSecret != "" {
			pw, err := secretFn(ctx, pwSecret)
			if err != nil {
				return nil, err
			}
			rel.Target.ArchivePassword = pw
		}
	}
	return r.openPreparedRelease(ctx, rel)
}

func (r *KindResolver) listNZBFiles(ctx context.Context, releaseID string, data []byte) ([]core.ReleaseFile, error) {
	if lister, ok := r.engine.(interface {
		ListFiles(context.Context, string, []byte) ([]core.ReleaseFile, error)
	}); ok {
		files, err := lister.ListFiles(ctx, releaseID, data)
		if err == nil {
			return files, nil
		}
		if !errors.Is(err, core.ErrNotAvailable) {
			return nil, err
		}
	}
	doc, err := nzb.Parse(data)
	if err != nil {
		return nil, err
	}
	files := make([]core.ReleaseFile, len(doc.Files))
	for i, f := range doc.Files {
		files[i] = core.ReleaseFile{
			Index:           i,
			Path:            f.StableName(),
			Size:            f.EstimatedDecodedBytes(),
			SizeApproximate: true,
		}
	}
	return files, nil
}

func (r *KindResolver) resolveTorrent(ctx context.Context, rel core.Release) (core.Release, error) {
	var resolved core.Release
	var err error
	if r.pluginNZBs != nil {
		resolved, err = r.pluginNZBs.ResolvePluginTorrent(ctx, rel)
	} else {
		tr, ok := r.indexer.(debrid.TorrentResolver)
		if !ok {
			return rel, fmt.Errorf("kindresolver: torrent %q has no info hash: %w", rel.Name, core.ErrReleaseRejected)
		}
		resolved, err = tr.ResolveTorrent(ctx, rel)
	}
	if err != nil {
		return rel, err
	}
	resolved.Protocol = core.ProtocolTorrent
	if resolved.FileCount < rel.FileCount {
		resolved.FileCount = rel.FileCount
	}
	if resolved.InfoHash != "" {
		assignKindReleaseID(&resolved)
	}
	return resolved, nil
}

type linkAuthStripper interface {
	StripLinkAuth(core.Release) core.Release
}

// stripReleaseAuth removes indexer-owned credentials from rel.NZBURL and
// recomputes the release and pack IDs so logs, vault aliases, and Usenet-check
// caches stay key-less even when a plugin hands back a legacy keyed link.
func (r *KindResolver) stripReleaseAuth(rel core.Release) core.Release {
	if r == nil || r.indexer == nil || strings.TrimSpace(rel.NZBURL) == "" {
		return rel
	}
	s, ok := r.indexer.(linkAuthStripper)
	if !ok {
		return rel
	}
	stripped := s.StripLinkAuth(rel)
	if stripped.NZBURL != rel.NZBURL {
		assignKindReleaseID(&stripped)
	}
	return stripped
}

// stripCandidateAuth removes indexer-owned credentials from cand's NZBURL and
// TorrentURL so plugins never receive or persist indexer API keys.
func (r *KindResolver) stripCandidateAuth(cand core.SourceCandidate) core.SourceCandidate {
	if r == nil || r.indexer == nil {
		return cand
	}
	s, ok := r.indexer.(linkAuthStripper)
	if !ok {
		return cand
	}
	if strings.TrimSpace(cand.NZBURL) != "" {
		cand.NZBURL = s.StripLinkAuth(core.Release{
			NZBURL:    cand.NZBURL,
			Indexer:   cand.Indexer,
			IndexerID: cand.IndexerID,
			Protocol:  core.ProtocolUsenet,
		}).NZBURL
	}
	if strings.TrimSpace(cand.TorrentURL) != "" {
		cand.TorrentURL = s.StripLinkAuth(core.Release{
			NZBURL:    cand.TorrentURL,
			Indexer:   cand.Indexer,
			IndexerID: cand.IndexerID,
			Protocol:  core.ProtocolTorrent,
		}).NZBURL
	}
	return cand
}

func (r *KindResolver) openPreparedRelease(ctx context.Context, rel core.Release) (_ core.MediaSource, err error) {
	defer func() { err = logredact.SourceError(err) }()
	start := time.Now()
	rel = r.stripReleaseAuth(rel)
	if rel.Protocol == core.ProtocolTorrent || rel.InfoHash != "" || rel.MagnetURL != "" {
		if r.debrid == nil {
			return nil, core.ErrSourceUnavailable
		}
		rel.Protocol = core.ProtocolTorrent
		queueMode := isReleaseQueueMode(ctx)
		if r.log != nil {
			r.log.Info("kindresolver: opening release", "release", logredact.Source(rel.ID), "name", logredact.Source(rel.Name), "protocol", rel.Protocol, "queue", queueMode)
		}
		acqCtx, acqCancel := withHardTimeout(ctx, r.engineOpenTimeout)
		defer acqCancel()
		if rel.InfoHash == "" && rel.MagnetURL == "" && (rel.NZBURL != "" || rel.TorrentRef != "") {
			resolved, err := r.resolveTorrent(acqCtx, rel)
			if err != nil {
				if r.log != nil {
					r.log.Warn("kindresolver: release open failed", "release", logredact.Source(rel.ID), "name", logredact.Source(rel.Name), "protocol", rel.Protocol, "stage", "resolve_torrent", "took", time.Since(start).Round(time.Millisecond), "err", logredact.SourceError(err))
				}
				return nil, err
			}
			rel = resolved
		}
		var (
			payload []byte
			err     error
		)
		if queueMode {
			q, canQueue := r.debrid.(debridDownloadQueuer)
			if !canQueue || !r.allowUncachedReleaseQueue(ctx) {
				return nil, fmt.Errorf("%q: %w", rel.Name, core.ErrNotCached)
			}
			payload, err = q.QueueDownload(acqCtx, rel)
			if err != nil {
				if r.log != nil {
					if errors.Is(err, core.ErrDownloadQueued) {
						r.log.Info("kindresolver: release download queued", "release", logredact.Source(rel.ID), "name", logredact.Source(rel.Name), "protocol", rel.Protocol, "took", time.Since(start).Round(time.Millisecond), "err", logredact.SourceError(err))
					} else {
						r.log.Warn("kindresolver: release open failed", "release", logredact.Source(rel.ID), "name", logredact.Source(rel.Name), "protocol", rel.Protocol, "stage", "debrid_queue", "took", time.Since(start).Round(time.Millisecond), "err", logredact.SourceError(err))
					}
				}
				return nil, err
			}
		} else {
			payload, err = r.debrid.Acquire(acqCtx, rel)
			if err != nil {
				if r.log != nil {
					if errors.Is(err, core.ErrDownloadQueued) {
						r.log.Info("kindresolver: queued release still downloading", "release", logredact.Source(rel.ID), "name", logredact.Source(rel.Name), "protocol", rel.Protocol, "took", time.Since(start).Round(time.Millisecond), "err", logredact.SourceError(err))
					} else {
						r.log.Warn("kindresolver: release open failed", "release", logredact.Source(rel.ID), "name", logredact.Source(rel.Name), "protocol", rel.Protocol, "stage", "debrid_acquire", "took", time.Since(start).Round(time.Millisecond), "err", logredact.SourceError(err))
					}
				}
				return nil, err
			}
		}
		openStart := time.Now()
		openCtx, openCancel := withHardTimeout(ctx, r.engineOpenTimeout)
		defer openCancel()
		ms, err := r.engine.Open(openCtx, rel.ID, payload)
		if err != nil {
			if r.log != nil {
				r.log.Warn("kindresolver: release open failed", "release", logredact.Source(rel.ID), "name", logredact.Source(rel.Name), "protocol", rel.Protocol, "stage", "engine_open", "took", time.Since(start).Round(time.Millisecond), "err", logredact.SourceError(err))
			}
			return nil, err
		}
		if r.log != nil {
			r.log.Info("kindresolver: opened release", "release", logredact.Source(rel.ID), "name", logredact.Source(rel.Name), "protocol", rel.Protocol, "open_took", time.Since(openStart).Round(time.Millisecond), "took", time.Since(start).Round(time.Millisecond), "size", ms.Size())
		}
		return ms, nil
	}
	if rel.NZBURL != "" || rel.NZBRef != "" {
		if r.indexer == nil && r.pluginNZBs == nil {
			return nil, core.ErrSourceUnavailable
		}
		rel.Protocol = core.ProtocolUsenet
		releaseKey := candidateNZBKey(core.SourceCandidate{NZBURL: rel.NZBURL, NZBRef: rel.NZBRef})
		rejectKey := kindGrabRejectKey(rel)
		if r.pluginNZBs == nil {
			if refusal := r.grabRejects.lookup(rejectKey, r.now()); refusal != nil {
				return nil, refusal
			}
		}
		if r.log != nil {
			r.log.Info("kindresolver: opening release", "release", logredact.Source(rel.ID), "name", logredact.Source(rel.Name), "protocol", rel.Protocol)
		}
		fetchStart := time.Now()
		fetchCtx, fetchCancel := withHardTimeout(ctx, r.nzbFetchTimeout)
		nzbData, err := r.fetchPluginNZB(fetchCtx, rel)
		fetchCancel()
		fetchTook := time.Since(fetchStart)
		if err != nil {
			now := r.now()
			if refusal := newznab.ClassifyGrabRefusal(err, now); refusal != nil {
				if r.pluginNZBs == nil {
					r.grabRejects.record(rejectKey, now, refusal.RetryAt)
				}
				return nil, refusal
			}
			if r.log != nil {
				r.log.Warn("kindresolver: release open failed", "release", logredact.Source(rel.ID), "name", logredact.Source(rel.Name), "protocol", rel.Protocol, "stage", "fetch_nzb", "took", fetchTook.Round(time.Millisecond), "err", logredact.SourceError(err))
			}
			return nil, err
		}
		var nzbHash, contentID string
		if rel.Target == nil {
			nzbHash = hashNZBBytes(nzbData)
			contentID, _, _, _ = nzb.InspectContent(nzbData)
		}
		if rel.Target != nil && rel.Target.AllowNonVideo {
			prepared, perr := ensureNonVideoNZB(nzbData, rel.Target)
			if errors.Is(perr, core.ErrWrongContent) && rel.Target.ExactFile != "" && debrid.NeedsExactFileNaming(nzbData, rel.Target) {
				if namer, ok := r.engine.(debrid.PackNamer); ok {
					nameCtx, nameCancel := withHardTimeout(ctx, r.engineOpenTimeout)
					named, nerr := namer.NameFiles(nameCtx, rel.ID, nzbData)
					nameCancel()
					if nerr == nil {
						prepared, perr = ensureNonVideoNZB(named, rel.Target)
					} else if !errors.Is(nerr, core.ErrNotAvailable) {
						perr = nerr
					}
				}
			}
			if perr != nil {
				if r.log != nil {
					r.log.Warn("kindresolver: release open failed", "release", logredact.Source(rel.ID), "name", logredact.Source(rel.Name), "protocol", rel.Protocol, "stage", "prepare_nzb", "took", time.Since(start).Round(time.Millisecond), "err", logredact.SourceError(perr))
				}
				return nil, perr
			}
			nzbData = prepared
		}
		openStart := time.Now()
		openCtx, openCancel := withHardTimeout(ctx, r.engineOpenTimeout)
		ms, err := r.engine.Open(openCtx, rel.ID, nzbData)
		openCancel()
		openTook := time.Since(openStart)
		if err != nil {
			r.recordUsenetPlaybackFailure(releaseKey, nzbHash, contentID, err)
			if r.log != nil {
				r.log.Warn("kindresolver: release open failed", "release", logredact.Source(rel.ID), "name", logredact.Source(rel.Name), "protocol", rel.Protocol, "stage", "engine_open", "fetch_took", fetchTook.Round(time.Millisecond), "open_took", openTook.Round(time.Millisecond), "took", time.Since(start).Round(time.Millisecond), "err", logredact.SourceError(err))
			}
			return nil, err
		}
		if r.log != nil {
			r.log.Info("kindresolver: opened release", "release", logredact.Source(rel.ID), "name", logredact.Source(rel.Name), "protocol", rel.Protocol, "fetch_took", fetchTook.Round(time.Millisecond), "open_took", openTook.Round(time.Millisecond), "took", time.Since(start).Round(time.Millisecond), "size", ms.Size())
		}
		return r.wrapUsenetPlaybackSource(ms, releaseKey, nzbHash, contentID), nil
	}
	return nil, core.ErrSourceUnavailable
}

// ensureNonVideoNZB attaches ether-nonvideo metadata (and narrows explicit file
// targets) when the underlying core.Indexer returned a raw NZB directly instead
// of passing through debrid.Indexer.FetchNZB.
func ensureNonVideoNZB(data []byte, target *core.ReleaseTarget) ([]byte, error) {
	if target == nil || !target.AllowNonVideo {
		return data, nil
	}
	if bytes.Contains(data, []byte(nzb.MetaNonVideo)) {
		return data, nil
	}
	if !bytes.Contains(bytes.ToLower(data), []byte("<nzb")) {
		return data, nil
	}
	return debrid.EpisodeNZB(data, target)
}

func extractItemDuration(item core.KindItem, decl plugins.MediaKindMeta) time.Duration {
	if len(item.Fields) == 0 {
		return 0
	}
	parseNum := func(raw json.RawMessage) int64 {
		var n float64
		if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
			return int64(n)
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if f, perr := strconv.ParseFloat(strings.TrimSpace(s), 64); perr == nil && f > 0 {
				return int64(f)
			}
		}
		return 0
	}
	for _, fd := range decl.Fields {
		if fd.Type == "duration" {
			if raw, ok := item.Fields[fd.Key]; ok {
				if sec := parseNum(raw); sec > 0 {
					return time.Duration(sec) * time.Second
				}
			}
		}
	}
	for _, key := range []string{"duration", "durationSec", "runtime"} {
		if raw, ok := item.Fields[key]; ok {
			if sec := parseNum(raw); sec > 0 {
				return time.Duration(sec) * time.Second
			}
		}
	}
	return 0
}

func extractItemSizeAndMIME(item core.KindItem, decl plugins.MediaKindMeta) (int64, string) {
	if len(item.Fields) == 0 {
		return 0, ""
	}
	var size int64
	var mime string
	parseNum := func(raw json.RawMessage) int64 {
		var n float64
		if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
			return int64(n)
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if v, perr := strconv.ParseInt(strings.TrimSpace(s), 10, 64); perr == nil && v > 0 {
				return v
			}
		}
		return 0
	}
	for _, fd := range decl.Fields {
		if fd.Type == "bytes" {
			if raw, ok := item.Fields[fd.Key]; ok {
				if v := parseNum(raw); v > 0 {
					size = v
					break
				}
			}
		}
	}
	if size == 0 {
		for _, key := range []string{"size", "sizeBytes", "byteSize"} {
			if raw, ok := item.Fields[key]; ok {
				if v := parseNum(raw); v > 0 {
					size = v
					break
				}
			}
		}
	}
	for _, key := range []string{"mime", "mimeType", "contentType"} {
		if raw, ok := item.Fields[key]; ok {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
				mime = strings.TrimSpace(s)
				break
			}
		}
	}
	return size, mime
}

type kindLocalFile struct {
	*os.File
	size int64
	mime string
}

func (f *kindLocalFile) Size() int64  { return f.size }
func (f *kindLocalFile) Name() string { return filepath.Base(f.File.Name()) }
func (f *kindLocalFile) MIME() string {
	if strings.TrimSpace(f.mime) == "" {
		return "application/octet-stream"
	}
	return f.mime
}

func effectiveItemIndex(itemIdx int, partIdx *int) int {
	if partIdx != nil && *partIdx >= 0 && itemIdx == 0 {
		return *partIdx
	}
	return itemIdx
}

type partDefaultByteSource struct {
	kindstream.ByteSource
	name string
	mime string
}

func (p *partDefaultByteSource) Name() string {
	if p.name != "" {
		return p.name
	}
	return p.ByteSource.Name()
}

func (p *partDefaultByteSource) MIME() string {
	if p.mime != "" {
		return p.mime
	}
	return p.ByteSource.MIME()
}

func (p *partDefaultByteSource) ReadAtContext(ctx context.Context, buf []byte, off int64) (int, error) {
	if cr, ok := p.ByteSource.(core.ContextReaderAt); ok {
		return cr.ReadAtContext(ctx, buf, off)
	}
	return p.ByteSource.ReadAt(buf, off)
}

func (p *partDefaultByteSource) UnwrapByteSource() kindstream.ByteSource {
	return p.ByteSource
}

func unwrapKindMediaSource(bs kindstream.ByteSource) core.MediaSource {
	if p, ok := bs.(*partDefaultByteSource); ok {
		bs = p.ByteSource
	}
	return kindstream.UnwrapMediaSource(bs)
}

func withPartDefaults(bs kindstream.ByteSource, part core.KindItemPart, itemTitle string) kindstream.ByteSource {
	name := bs.Name()
	if name == "" {
		name = firstNonEmpty(part.Name, itemTitle)
	}
	mime := bs.MIME()
	if (mime == "" || mime == "application/octet-stream") && part.MIME != "" {
		mime = part.MIME
	}
	if name == bs.Name() && mime == bs.MIME() {
		return bs
	}
	return &partDefaultByteSource{ByteSource: bs, name: name, mime: mime}
}

type preopenedSequence struct {
	kindstream.ByteSource
	name    string
	cleanup func() error
}

func (p *preopenedSequence) Name() string {
	if p.name != "" {
		return p.name
	}
	return p.ByteSource.Name()
}

func (p *preopenedSequence) ReadAtContext(ctx context.Context, buf []byte, off int64) (int, error) {
	if cr, ok := p.ByteSource.(core.ContextReaderAt); ok {
		return cr.ReadAtContext(ctx, buf, off)
	}
	return p.ByteSource.ReadAt(buf, off)
}

func (p *preopenedSequence) Close() error {
	err1 := p.ByteSource.Close()
	var err2 error
	if p.cleanup != nil {
		err2 = p.cleanup()
	}
	if err1 != nil {
		return err1
	}
	return err2
}
