package debrid

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/flight"
	"github.com/braedonsaunders/ether/internal/nzb"
)

// TorrentResolver fills a torrent release's info hash and magnet (by
// downloading its .torrent when the indexer gave neither). Implemented by
// *newznab.Manager.
type TorrentResolver interface {
	ResolveTorrent(ctx context.Context, r core.Release) (core.Release, error)
}

// Indexer wraps the aggregate indexer: torrent results are kept only while
// a debrid provider is enabled (annotated with cache availability where a
// provider offers a bulk check), and FetchNZB returns a debrid payload for
// torrents while Usenet releases go to the inner indexer unchanged.
//
// A Stremio addon's own HTTP stream links (core.ProtocolHTTP) need no
// debrid account. Without one, a torrent the addon also linked plays
// through that link instead; with cached-only on, links the addon labels
// only as uncached are left out (the label is a hint, never proof: the
// probe in FetchNZB decides). Usenet results are left out while no Usenet
// provider is enabled.
type Indexer struct {
	inner   core.Indexer
	svc     *Service
	flights flight.Group[string, []byte]
	// usenetReady reports whether a Usenet provider is enabled; nil means
	// unknown (no debrid-only source check).
	usenetReady func() bool
	packNamer   PackNamer
}

var _ core.Indexer = (*Indexer)(nil)

// NewIndexer wraps inner.
func NewIndexer(inner core.Indexer, svc *Service) *Indexer { return &Indexer{inner: inner, svc: svc} }

// SetUsenetReady wires the "is a Usenet provider enabled" check used by
// MissingSources. Call before the indexer is used.
// SetPackNamer sets what reads real file names out of an obfuscated Usenet
// season pack (PAR2) so its wanted episode can be found. Without one, only
// packs with readable file names resolve.
func (x *Indexer) SetPackNamer(n PackNamer) { x.packNamer = n }

func (x *Indexer) SetUsenetReady(fn func() bool) { x.usenetReady = fn }

// sourceInventory is what MissingSources needs from the inner indexer
// (implemented by *newznab.Manager).
type sourceInventory interface {
	MissingSources() error
	HasTorrentIndexer() bool
}

// MissingSources reports core.ErrNoSources when no indexer is enabled, and
// core.ErrNoTorrentSources when debrid is the only way to play (no Usenet
// provider) but no enabled indexer returns torrents.
func (x *Indexer) MissingSources() error {
	inv, ok := x.inner.(sourceInventory)
	if !ok {
		return nil
	}
	if err := inv.MissingSources(); err != nil {
		return err
	}
	direct := false
	if sources, ok := x.inner.(interface{ HasDirectSources() bool }); ok {
		direct = sources.HasDirectSources()
	}
	if !direct && x.usenetReady != nil && !x.usenetReady() && x.svc != nil && x.svc.Enabled() && !inv.HasTorrentIndexer() {
		return core.ErrNoTorrentSources
	}
	return nil
}

// Inner returns the wrapped indexer.
func (x *Indexer) Inner() core.Indexer { return x.inner }

// ResolveTorrent implements TorrentResolver by forwarding to the wrapped
// indexer (for example VODIndexer -> StreamSources -> newznab.Manager ->
// cardigann.Runner), filling a torrent release's info hash, magnet and
// optional .torrent metainfo file list.
func (x *Indexer) ResolveTorrent(ctx context.Context, r core.Release) (core.Release, error) {
	if tr, ok := x.inner.(TorrentResolver); ok {
		return tr.ResolveTorrent(ctx, r)
	}
	if r.InfoHash != "" {
		return r, nil
	}
	return r, fmt.Errorf("debrid: torrent %q has no info hash: %w", r.Name, core.ErrReleaseRejected)
}

// StripLinkAuth forwards credential stripping to the wrapped indexer so
// plugin-facing source candidates do not carry indexer API keys or secrets.
func (x *Indexer) StripLinkAuth(r core.Release) core.Release {
	if s, ok := x.inner.(interface {
		StripLinkAuth(core.Release) core.Release
	}); ok {
		return s.StripLinkAuth(r)
	}
	return r
}

// Name implements core.Indexer.
func (x *Indexer) Name() string { return x.inner.Name() }

// Search implements core.Indexer.
func (x *Indexer) Search(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	return x.SearchWith(ctx, req, nil)
}

// EnabledIndexerStatuses forwards enabled indexer status listing to the
// wrapped indexer.
func (x *Indexer) EnabledIndexerStatuses(status string) []core.IndexerSearchStatus {
	if dir, ok := x.inner.(core.IndexerDirectory); ok {
		return dir.EnabledIndexerStatuses(status)
	}
	return nil
}

// EnabledIndexers forwards enabled indexer listing to the wrapped indexer.
func (x *Indexer) EnabledIndexers() []core.IndexerSearchStatus {
	if dir, ok := x.inner.(core.IndexerDirectory); ok {
		return dir.EnabledIndexers()
	}
	return []core.IndexerSearchStatus{}
}

// SearchWithOptions forwards controlled fan-out options to the wrapped indexer
// and filters results through playable + debrid cache availability. Early
// return and bounded-wait calls avoid adding a blocking post-search CheckCached
// round-trip on top of the caller's wait budget.
func (x *Indexer) SearchWithOptions(ctx context.Context, req core.SearchRequest, opts core.IndexerSearchOptions) (core.IndexerSearchResult, error) {
	csi, ok := x.inner.(core.ControlledSearchIndexer)
	if !ok {
		started := time.Now()
		rels, err := x.Search(ctx, req)
		if err != nil {
			return core.IndexerSearchResult{}, err
		}
		name := x.Name()
		return core.IndexerSearchResult{
			Releases: rels,
			Indexers: []core.IndexerSearchStatus{{
				IndexerID: name,
				Indexer:   name,
				Status:    core.IndexerStatusAnswered,
				Results:   len(rels),
				MS:        max(int64(0), time.Since(started).Milliseconds()),
			}},
		}, nil
	}
	if publish := opts.ContinueSearch; publish != nil {
		opts.ContinueSearch = func(snapshot func(context.Context) core.IndexerSearchResult) {
			publish(func(pollCtx context.Context) core.IndexerSearchResult {
				return x.filterControlledSearch(pollCtx, snapshot(pollCtx), true)
			})
		}
	}
	res, err := csi.SearchWithOptions(ctx, req, opts)
	if err != nil {
		return res, err
	}
	bounded := opts.FirstResults || opts.MinResults > 0 || opts.WaitTimeout > 0 || len(res.Pending) > 0
	return x.filterControlledSearch(ctx, res, bounded), nil
}

func (x *Indexer) filterControlledSearch(ctx context.Context, res core.IndexerSearchResult, bounded bool) core.IndexerSearchResult {
	enabled := x.svc != nil && x.svc.Enabled()
	if enabled {
		x.svc.ApplyHints(ctx, res.Releases)
	}
	var torrents []string
	out := res.Releases[:0:0]
	for _, r := range res.Releases {
		r, ok := x.playable(r)
		if !ok {
			continue
		}
		if r.Protocol == core.ProtocolTorrent && r.InfoHash != "" {
			torrents = append(torrents, r.InfoHash)
		}
		out = append(out, r)
	}
	if len(torrents) == 0 || !enabled || bounded {
		if enabled && len(torrents) > 0 {
			sort.SliceStable(out, func(i, j int) bool {
				ci := out[i].Protocol == core.ProtocolTorrent && x.svc.KnownCached(out[i].InfoHash)
				cj := out[j].Protocol == core.ProtocolTorrent && x.svc.KnownCached(out[j].InfoHash)
				return ci && !cj
			})
		}
		res.Releases = out
		return res
	}
	cached := x.checkAvailability(ctx, torrents)
	kept := out[:0]
	dropUncached := x.svc.Config().CachedOnly
	explain := x.explainUncached(out, cached)
	for _, r := range out {
		if r.Protocol == core.ProtocolTorrent && (dropUncached || r.Private) && x.confirmedUncached(ctx, r, cached) {
			if explain == 0 {
				continue
			}
			explain--
		}
		kept = append(kept, r)
	}
	x.rankAvailability(kept, cached)
	res.Releases = kept
	return res
}

// SearchWith exposes arrivals from both sources while cache discovery continues.
// The resolver targets and validates speculative torrents before selecting one.
func (x *Indexer) SearchWith(ctx context.Context, req core.SearchRequest, arrived func([]core.Release)) ([]core.Release, error) {
	started := time.Now()
	early := x.earlyCacheChecks(ctx, arrived)
	if early != nil {
		defer func() { early.cancel(); early.wg.Wait() }()
	}
	var results []core.Release
	var err error
	emit := func(batch []core.Release) {
		x.svc.ApplyHints(ctx, batch)
		var eligible []core.Release
		for _, r := range batch {
			if r, ok := x.playable(r); ok {
				eligible = append(eligible, r)
			}
		}
		if early != nil {
			early.add(eligible)
		}
		if arrived != nil && len(eligible) > 0 {
			arrived(eligible)
		}
	}
	if streaming, ok := x.inner.(core.StreamingIndexer); ok && (early != nil || arrived != nil) {
		results, err = streaming.SearchWith(ctx, req, emit)
	} else {
		results, err = x.inner.Search(ctx, req)
	}
	searchDuration := time.Since(started)
	var cacheDuration time.Duration
	var hashCount int
	defer func() {
		x.svc.log.Info("source search completed", "indexers", searchDuration,
			"cacheDiscovery", cacheDuration, "torrentHashes", hashCount,
			"results", len(results), "took", time.Since(started), "success", err == nil)
	}()
	if err != nil {
		return results, err
	}
	enabled := x.svc.Enabled()
	if enabled {
		x.svc.ApplyHints(ctx, results)
	}
	var torrents []string
	out := results[:0:0]
	for _, r := range results {
		r, ok := x.playable(r)
		if !ok {
			continue
		}
		if r.Protocol != core.ProtocolTorrent {
			out = append(out, r)
			continue
		}
		if r.InfoHash != "" {
			torrents = append(torrents, r.InfoHash)
		}
		out = append(out, r)
	}
	if len(torrents) == 0 || core.HasIndexerStatusObserver(ctx) {
		return out, nil
	}
	// One bulk availability call per provider that offers it: torrents
	// known to be uncached everywhere are dropped (they would only burn
	// health-check attempts), and known-cached torrents move ahead of
	// unknown ones of equal rank.
	cacheStart := time.Now()
	var cached map[string]bool
	var expired bool
	if early != nil {
		// Most hashes were checked while slower indexers were still
		// searching; only stragglers are checked now.
		cached, expired = early.finish(ctx, torrents)
	} else {
		cached = x.checkAvailability(ctx, torrents)
	}
	cacheDuration, hashCount = time.Since(cacheStart), len(torrents)
	kept := out[:0]
	anyCached := false
	// With cached-only off an uncached torrent is still playable later
	// (playback sends it to download), so it stays, ranked after cached ones.
	dropUncached := x.svc.Config().CachedOnly
	explain := x.explainUncached(out, cached)
	var explained []string // ids kept only to explain a not-cached failure
	var unknown []core.Release
	for _, r := range out {
		if r.Protocol == core.ProtocolTorrent {
			hit, checked := cached[r.InfoHash]
			// Late unknowns must not suppress the season query or claim a
			// release ID before it can return a confirmed match.
			if expired && r.InfoHash != "" && !checked && (dropUncached || r.Private) {
				unknown = append(unknown, r)
				continue
			}
			if (dropUncached || r.Private) && x.confirmedUncached(ctx, r, cached) {
				if explain == 0 {
					continue
				}
				explain--
				explained = append(explained, r.ID)
			}
			anyCached = anyCached || hit || x.svc.KnownCached(r.InfoHash)
		}
		kept = append(kept, r)
	}
	// When a foreground episode search returned only uncached single-episode
	// torrents (preventing the indexer's own empty-result fallback from querying
	// season packs), search torrent indexers for season packs (Episode = 0) and
	// keep cached packs so older shows whose episodes live only in cached season
	// packs still resolve. Speculative work keeps to the primary query.
	hasPack := false
	for _, r := range kept {
		if r.Info.FullSeason {
			hasPack = true
			break
		}
	}
	if enabled && !anyCached && !hasPack && len(torrents) > 0 && !core.SpeculativeIndexerWork(ctx) && req.Type == core.TypeEpisode && req.Season > 0 && req.Episode > 0 && ctx.Err() == nil {
		packCached, packs := x.searchSeasonPacks(ctx, req)
		for h, ok := range packCached {
			if ok {
				if cached == nil {
					cached = map[string]bool{}
				}
				cached[h] = true
			}
		}
		byID := make(map[string]int, len(kept))
		for i, r := range kept {
			byID[r.ID] = i
		}
		for _, r := range packs {
			if i, seen := byID[r.ID]; seen {
				if packCached[r.InfoHash] && !cached[kept[i].InfoHash] {
					kept[i] = r
				}
				continue
			}
			byID[r.ID] = len(kept)
			kept = append(kept, r)
		}
		if len(explained) > 0 && slices.ContainsFunc(packs, func(r core.Release) bool { return packCached[r.InfoHash] }) {
			// A cached pack plays: nothing is left to explain.
			kept = slices.DeleteFunc(kept, func(r core.Release) bool {
				return !cached[r.InfoHash] && slices.Contains(explained, r.ID)
			})
		}
	}
	if len(unknown) > 0 {
		seenID := make(map[string]bool, len(kept))
		for _, r := range kept {
			seenID[r.ID] = true
		}
		for _, r := range unknown {
			if !seenID[r.ID] {
				kept = append(kept, r)
				seenID[r.ID] = true
			}
		}
	}
	x.rankAvailability(kept, cached)
	return kept, nil
}

// checkAvailability retains positive answers from partial batches. Only a
// definitive batch can establish that a torrent is absent everywhere.
func (x *Indexer) checkAvailability(ctx context.Context, hashes []string) map[string]bool {
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cached, definitive := x.svc.CheckCached(cctx, hashes)
	if !definitive {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			core.NoteSearchIncomplete(ctx)
		}
		for h, hit := range cached {
			if !hit {
				delete(cached, h)
			}
		}
	}
	return cached
}

func (x *Indexer) confirmedUncached(ctx context.Context, r core.Release, cached map[string]bool) bool {
	hit, checked := cached[r.InfoHash]
	return checked && !hit && !x.svc.KnownCached(r.InfoHash) && !x.svc.mayBeZipOnlyCached(ctx, r)
}

func (x *Indexer) rankAvailability(releases []core.Release, cached map[string]bool) {
	rank := func(r core.Release) int {
		if r.Protocol == core.ProtocolTorrent {
			if cached[r.InfoHash] {
				return 0
			}
			if x.svc.KnownCached(r.InfoHash) {
				return 1
			}
			if _, checked := cached[r.InfoHash]; checked {
				return 3
			}
		}
		return 2
	}
	sort.SliceStable(releases, func(i, j int) bool { return rank(releases[i]) < rank(releases[j]) })
}

// keepUncached is how many torrents no provider holds a cached-only search
// keeps when none is cached (the rest are dropped so they do not use up the
// resolver's check budget). Their availability answer is remembered, so each fails as not
// cached at once without an API call, and when nothing else plays the
// player and Releases › History say "not cached" instead of "no release
// found".
const keepUncached = 3

// explainUncached is how many uncached torrents a cached-only search keeps:
// keepUncached when no torrent in it is cached (they are then the reason
// the title cannot play), else none, as before.
func (x *Indexer) explainUncached(rels []core.Release, cached map[string]bool) int {
	for _, r := range rels {
		if r.Protocol == core.ProtocolTorrent && r.InfoHash != "" && (cached[r.InfoHash] || x.svc.KnownCached(r.InfoHash)) {
			return 0
		}
	}
	return keepUncached
}

// QueueDownload sends an uncached torrent release to the owner's debrid
// service to download (cached-only off); see Service.QueueDownload.
func (x *Indexer) QueueDownload(ctx context.Context, r core.Release) ([]byte, error) {
	if r.Protocol != core.ProtocolTorrent || !x.svc.Enabled() {
		return nil, fmt.Errorf("release %q: %w", r.Name, core.ErrNotCached)
	}
	return x.svc.QueueDownload(ctx, r)
}

// UsenetReady reports whether a Usenet provider is set up, so a failure
// can say what Usenet contributed.
func (x *Indexer) UsenetReady() bool { return x.usenetReady == nil || x.usenetReady() }

// playable adapts a search result to what this server can stream, or
// reports false: torrents need an enabled debrid provider (or else play
// through the addon link they came with), Usenet needs a Usenet provider
// (or likewise its addon link),
// and with cached-only on an HTTP link the addon labels only as uncached
// is left out.
func (x *Indexer) playable(r core.Release) (core.Release, bool) {
	switch r.Protocol {
	case core.ProtocolTorrent:
		if x.svc.Enabled() {
			return r, true
		}
		if r.StreamURL == "" {
			return r, false
		}
		r.Protocol = core.ProtocolHTTP
		fallthrough
	case core.ProtocolHTTP:
		if r.StreamURL == "" {
			return r, false
		}
		if x.svc.Config().CachedOnly && len(r.DebridUncached) > 0 && len(r.DebridCached) == 0 {
			return r, false
		}
		return r, true
	case core.ProtocolUsenet:
		if x.usenetReady == nil || x.usenetReady() {
			return r, true
		}
		// An add-on Usenet stream that also gave its own play link plays
		// through it, as a torrent does without a debrid provider.
		if r.StreamURL == "" {
			return r, false
		}
		r.Protocol = core.ProtocolHTTP
		return r, true
	}
	return r, true
}

// searchSeasonPacks queries torrent indexers in inner for season-level
// releases (Episode = 0) and bulk-checks their debrid cache availability.
// Usenet indexers are skipped because non-torrent results are unused here.
func (x *Indexer) searchSeasonPacks(ctx context.Context, req core.SearchRequest) (map[string]bool, []core.Release) {
	ctx = core.WithTorrentOnlySearch(ctx)
	packReq := req
	packReq.Episode = 0
	early := x.earlyCacheChecks(ctx, nil)
	if early != nil {
		defer func() { early.cancel(); early.wg.Wait() }()
	}
	var results []core.Release
	var err error
	if streaming, ok := x.inner.(core.StreamingIndexer); ok && early != nil {
		results, err = streaming.SearchWith(ctx, packReq, early.add)
	} else {
		results, err = x.inner.Search(ctx, packReq)
	}
	if err != nil || len(results) == 0 {
		return nil, nil
	}
	x.svc.ApplyHints(ctx, results)
	var hashes []string
	var packs []core.Release
	for _, r := range results {
		if r.Protocol != core.ProtocolTorrent {
			continue
		}
		if r.InfoHash != "" {
			hashes = append(hashes, r.InfoHash)
		}
		packs = append(packs, r)
	}
	if len(packs) == 0 {
		return nil, nil
	}
	var cached map[string]bool
	if len(hashes) > 0 {
		if early != nil {
			cached, _ = early.finish(ctx, hashes)
		} else {
			cached = x.checkAvailability(ctx, hashes)
		}
	}
	kept := packs[:0]
	for _, r := range packs {
		if x.confirmedUncached(ctx, r, cached) {
			continue
		}
		kept = append(kept, r)
	}
	return cached, kept
}

// SearchRescue implements second-pass rescue search through the inner
// indexer, annotating discovered torrent releases with debrid cache status.
func (x *Indexer) SearchRescue(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	rs, ok := x.inner.(core.RescueIndexer)
	if !ok {
		return nil, nil
	}
	early := x.earlyCacheChecks(ctx, nil)
	if early != nil {
		defer func() { early.cancel(); early.wg.Wait() }()
	}
	var results []core.Release
	var err error
	if streaming, ok := x.inner.(core.StreamingRescueIndexer); ok && early != nil {
		results, err = streaming.SearchRescueWith(ctx, req, early.add)
	} else {
		results, err = rs.SearchRescue(ctx, req)
	}
	if err != nil || len(results) == 0 {
		return results, err
	}
	enabled := x.svc.Enabled()
	if enabled {
		x.svc.ApplyHints(ctx, results)
	}
	var torrents []string
	out := results[:0:0]
	for _, r := range results {
		r, ok := x.playable(r)
		if !ok {
			continue
		}
		if r.Protocol != core.ProtocolTorrent {
			out = append(out, r)
			continue
		}
		if r.InfoHash != "" {
			torrents = append(torrents, r.InfoHash)
		}
		out = append(out, r)
	}
	if len(torrents) == 0 {
		return out, nil
	}
	var cached map[string]bool
	if early != nil {
		cached, _ = early.finish(ctx, torrents)
	} else {
		cached = x.checkAvailability(ctx, torrents)
	}
	kept := out[:0]
	dropUncached := x.svc.Config().CachedOnly
	explain := x.explainUncached(out, cached)
	for _, r := range out {
		if r.Protocol == core.ProtocolTorrent && (dropUncached || r.Private) && x.confirmedUncached(ctx, r, cached) {
			if explain == 0 {
				continue
			}
			explain--
		}
		kept = append(kept, r)
	}
	x.rankAvailability(kept, cached)
	return kept, nil
}

// FetchNZB implements core.Indexer. For a torrent it returns the debrid
// payload of the release's main video file (or the wanted episode of a
// season pack); for anything else, the inner indexer's NZB.
func (x *Indexer) FetchNZB(ctx context.Context, r core.Release) ([]byte, error) {
	if r.Protocol == core.ProtocolHTTP {
		return x.svc.DirectPayload(ctx, r)
	}
	if r.Protocol != core.ProtocolTorrent {
		if x.usenetReady != nil && !x.usenetReady() {
			return nil, fmt.Errorf("usenet release %q: %w", r.Name, core.ErrNotAvailable)
		}
		data, err := x.inner.FetchNZB(ctx, r)
		if err != nil || r.Target == nil {
			return data, err
		}
		if r.Info.FullSeason {
			// A Usenet season pack plays as its wanted episode's files only.
			return x.episodeNZB(ctx, r, data)
		}
		if r.Target.AllowNonVideo && (r.Target.FileIndex != nil || r.Target.ExactFile != "" ||
			r.Target.Member != "" || r.Target.MemberIndex != nil || nzb.IsNonVideoMIME(r.Target.MIME)) {
			// A plugin targeted a non-video file, archive member, or media MIME:
			// narrow to an explicit file when named and carry non-video metadata
			// into the stream engine.
			out, err := EpisodeNZB(data, r.Target)
			if errors.Is(err, core.ErrWrongContent) && r.Target.ExactFile != "" && x.packNamer != nil && NeedsExactFileNaming(data, r.Target) {
				named, nerr := x.packNamer.NameFiles(ctx, r.ID, data)
				if nerr != nil {
					return nil, nerr
				}
				return EpisodeNZB(named, r.Target)
			}
			return out, err
		}
		if len(r.Target.MovieTitles) > 0 || len(r.Target.WantedLanguages) > 0 {
			if out, merr := EpisodeNZB(data, r.Target); merr == nil {
				return out, nil
			}
		}
		return data, nil
	}
	if !x.svc.Enabled() {
		return nil, fmt.Errorf("torrent release %q: %w", r.Name, ErrNoProvider)
	}
	return x.fetchTorrent(ctx, r)
}

func (x *Indexer) fetchTorrentOnce(ctx context.Context, r core.Release) (data []byte, err error) {
	started := time.Now()
	var descriptor, provider time.Duration
	reused := false
	defer func() {
		x.svc.log.Info("debrid payload prepared", "release", r.ID, "reused", reused,
			"descriptor", descriptor, "provider", provider, "took", time.Since(started), "success", err == nil)
	}()
	if p, ok := x.svc.reusablePayload(ctx, r); ok {
		reused = true
		return p.Encode(), nil
	}
	if r.InfoHash == "" {
		tr, ok := x.inner.(TorrentResolver)
		if !ok {
			return nil, fmt.Errorf("debrid: torrent %q has no info hash: %w", r.Name, core.ErrReleaseRejected)
		}
		phase := time.Now()
		resolved, err := tr.ResolveTorrent(ctx, r)
		descriptor = time.Since(phase)
		if err != nil {
			return nil, err
		}
		if resolved.FileCount < r.FileCount {
			resolved.FileCount = r.FileCount
		}
		r = resolved
	}
	phase := time.Now()
	data, err = x.svc.Acquire(ctx, r)
	provider = time.Since(phase)
	return data, err
}

// earlyChecks runs debrid cache checks per indexer as results arrive.
type earlyChecks struct {
	x        *Indexer
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	wg       sync.WaitGroup
	asked    map[string]bool
	cached   map[string]bool
	byHash   map[string][]core.Release
	onCached func([]core.Release)
	pending  []string
	running  bool
	expired  bool // the deadline interrupted an availability batch
}

func (x *Indexer) earlyCacheChecks(ctx context.Context, onCached func([]core.Release)) *earlyChecks {
	if !x.svc.Enabled() {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	return &earlyChecks{
		x:        x,
		ctx:      cctx,
		cancel:   cancel,
		asked:    map[string]bool{},
		cached:   map[string]bool{},
		byHash:   map[string][]core.Release{},
		onCached: onCached,
	}
}

// add starts a cache check for the torrent hashes in one indexer's results.
func (e *earlyChecks) add(rs []core.Release) {
	e.mu.Lock()
	for _, r := range rs {
		if r.Protocol == core.ProtocolTorrent && r.InfoHash != "" {
			if r.ID != "" {
				e.byHash[r.InfoHash] = append(e.byHash[r.InfoHash], r)
			}
			if !e.asked[r.InfoHash] {
				e.asked[r.InfoHash] = true
				e.pending = append(e.pending, r.InfoHash)
			}
		}
	}
	if len(e.pending) == 0 || e.running || e.ctx.Err() != nil {
		e.mu.Unlock()
		return
	}
	e.running = true
	e.wg.Add(1)
	e.mu.Unlock()
	go func() {
		defer e.wg.Done()
		for {
			// Streaming feeds emit individual items. Coalesce those into bulk
			// provider calls, with one bounded consumer rather than one HTTP
			// request goroutine for every torrent in a large feed.
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-e.ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
			e.mu.Lock()
			if e.ctx.Err() != nil || len(e.pending) == 0 {
				e.running = false
				e.mu.Unlock()
				return
			}
			hashes := e.pending
			e.pending = nil
			e.mu.Unlock()
			cached, definitive := e.x.svc.CheckCached(e.ctx, hashes)
			e.mu.Lock()
			var newlyCached []core.Release
			for h, ok := range cached {
				if ok || definitive {
					e.cached[h] = ok
				}
				if ok {
					newlyCached = append(newlyCached, e.byHash[h]...)
				}
			}
			e.expired = e.expired || (!definitive && errors.Is(e.ctx.Err(), context.DeadlineExceeded))
			done := len(e.pending) == 0
			if done {
				e.running = false
			}
			e.mu.Unlock()
			if e.onCached != nil && len(newlyCached) > 0 {
				e.onCached(newlyCached)
			}
			if done {
				return
			}
		}
	}()
}

// finish returns verdicts for completed checks. Hashes without a verdict
// stay unknown, including hashes that arrived after the discovery window.
func (e *earlyChecks) finish(ctx context.Context, hashes []string) (map[string]bool, bool) {
	defer e.cancel()
	e.add(hashOnly(hashes))
	e.wg.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	expired := e.expired || (len(e.pending) > 0 && errors.Is(e.ctx.Err(), context.DeadlineExceeded))
	if expired {
		core.NoteSearchIncomplete(ctx)
	}
	return e.cached, expired
}

func hashOnly(hashes []string) []core.Release {
	out := make([]core.Release, len(hashes))
	for i, h := range hashes {
		out[i] = core.Release{Protocol: core.ProtocolTorrent, InfoHash: h}
	}
	return out
}

// RecentSourceThroughput uses only account measurements already collected by
// active streams. An unmeasured eligible account leaves selection unrestricted.
func (x *Indexer) RecentSourceThroughput(r core.Release) float64 {
	if x.svc == nil || r.Protocol != core.ProtocolTorrent {
		return 0
	}
	var best float64
	for _, e := range x.svc.active() {
		if len(r.DebridCached) > 0 && !slices.Contains(r.DebridCached, e.cfg.Kind) {
			continue
		}
		e.mu.Lock()
		l := e.stats.Latency
		e.mu.Unlock()
		if l.ThroughputBps <= 0 || x.svc.now().Sub(l.ThroughputAt) > 2*time.Minute {
			return 0
		}
		best = max(best, l.ThroughputBps)
	}
	return best
}
