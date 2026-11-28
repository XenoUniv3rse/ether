package debrid

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// CacheHintCandidate is one torrent a cache-hint source is asked about: its
// info hash (lower case), whether any copy of it comes from a private
// tracker, and the enabled provider kinds with no answer of their own.
type CacheHintCandidate struct {
	InfoHash      string
	Private       bool
	ProviderKinds []string
}

// CacheHinter asks the sources that know which torrents a debrid service
// holds (plugins with the debrid_cache_hints capability). Enabled reports,
// without any I/O, whether any source would be asked. CacheHints calls found
// with each source's answer, info hash -> provider kinds holding it, as the
// answer arrives, and returns once every source answered or ctx ended.
type CacheHinter interface {
	Enabled() bool
	CacheHints(ctx context.Context, cands []CacheHintCandidate, found func(map[string][]string))
}

const (
	// cacheHintDeadline bounds the whole ask, from the first read to the
	// last hint written; Play never waits longer.
	cacheHintDeadline = 500 * time.Millisecond
	// maxCacheHintTorrents is the shortlist preparation looks at.
	maxCacheHintTorrents = 12
)

// SetCacheHinter attaches the cache-hint sources; nil detaches them.
func (s *Service) SetCacheHinter(h CacheHinter) {
	if h == nil {
		s.hinter.Store(nil)
		return
	}
	s.hinter.Store(&h)
}

func (s *Service) cacheHinter() CacheHinter {
	if h := s.hinter.Load(); h != nil {
		return *h
	}
	return nil
}

// explicitPlay reports whether ctx is a viewer's Play: marked by the start
// paths that serve one (core.WithExplicitPlay), never inferred from the
// playback purpose that downloads and failovers carry too, and not
// speculative, unattended, background or analysis work.
func explicitPlay(ctx context.Context) bool {
	return core.ExplicitPlay(ctx) &&
		!core.SpeculativeIndexerWork(ctx) && !core.IsBackgroundCheck(ctx) && !core.IsBackgroundAnalysis(ctx)
}

// askCacheHints asks the cache-hint sources about a prepared shortlist on an
// explicit Play whose lead is not known cached, for the providers that have
// no answer of their own (neither in discovery memory nor remembered by the
// account). One cacheHintDeadline covers those reads, the lead lookup
// included, the ask and the writes: a read still blocked at the deadline
// ends the round, each positive for a pair it asked about goes through
// ApplyHints as it arrives, and nothing is written after the deadline.
// Misses, errors and late answers write nothing.
func (x *Indexer) askCacheHints(ctx context.Context, out []core.Release) {
	if len(out) == 0 || ctx.Err() != nil || !explicitPlay(ctx) {
		return
	}
	h := x.svc.cacheHinter()
	if h == nil || !h.Enabled() {
		return
	}
	var lead string
	if r := out[0]; r.Protocol == core.ProtocolTorrent && r.InfoHash != "" {
		lead = strings.ToLower(r.InfoHash)
	}
	var all []CacheHintCandidate
	seen := map[string]int{} // hash -> all index
	for _, r := range out {
		if r.Protocol != core.ProtocolTorrent || r.InfoHash == "" {
			continue
		}
		hash := strings.ToLower(r.InfoHash)
		if i, ok := seen[hash]; ok {
			all[i].Private = all[i].Private || r.Private
			continue
		}
		if len(all) == maxCacheHintTorrents {
			continue
		}
		seen[hash] = len(all)
		all = append(all, CacheHintCandidate{InfoHash: hash, Private: r.Private})
	}
	if len(all) == 0 {
		return
	}
	hctx, cancel := context.WithTimeout(ctx, cacheHintDeadline)
	defer cancel()
	read := make(chan []CacheHintCandidate, 1)
	go func() { read <- x.svc.cacheHintCandidates(hctx, lead, all) }()
	var cands []CacheHintCandidate
	select {
	case cands = <-read:
	case <-hctx.Done():
		return
	}
	if len(cands) == 0 || hctx.Err() != nil {
		return
	}
	index := make(map[string]int, len(cands)) // hash -> cands index
	for i, c := range cands {
		index[c.InfoHash] = i
	}
	var mu sync.Mutex
	open := true
	found := func(answer map[string][]string) {
		mu.Lock()
		if !open || hctx.Err() != nil {
			mu.Unlock()
			return
		}
		var hinted []core.Release
		for hash, kinds := range answer {
			i, ok := index[strings.ToLower(hash)]
			if !ok {
				continue
			}
			var asked []string
			for _, k := range kinds {
				if slices.Contains(cands[i].ProviderKinds, k) && !slices.Contains(asked, k) {
					asked = append(asked, k)
				}
			}
			if len(asked) > 0 {
				hinted = append(hinted, core.Release{Protocol: core.ProtocolTorrent, InfoHash: cands[i].InfoHash, Private: cands[i].Private, DebridCached: asked})
			}
		}
		mu.Unlock()
		if len(hinted) > 0 {
			x.svc.ApplyHints(hctx, hinted) // writes nothing once hctx ended
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.CacheHints(hctx, slices.Clone(cands), found)
	}()
	select {
	case <-done:
	case <-hctx.Done():
	}
	mu.Lock()
	open = false
	mu.Unlock()
}

// cacheHintCandidates does the reads before an ask: nil when the lead is
// known cached, in discovery memory or remembered cached by an account,
// otherwise the shortlist entries with at least one provider kind whose
// account has no answer of its own. It reads each account's remembered
// answers for the whole shortlist at once. It owns all.
func (s *Service) cacheHintCandidates(ctx context.Context, lead string, all []CacheHintCandidate) []CacheHintCandidate {
	if lead != "" && s.KnownCached(lead) {
		return nil
	}
	hashes := make([]string, len(all))
	for i, c := range all {
		hashes[i] = c.InfoHash
	}
	for _, e := range s.active() {
		kind := s.kindOf(e)
		known := s.knownAvailability(ctx, e, hashes)
		remembered := e.rememberedAnswers(ctx, hashes)
		if lead != "" && remembered[lead].verdict == answerCached {
			return nil
		}
		for i, c := range all {
			if _, ok := known[c.InfoHash]; ok || slices.Contains(c.ProviderKinds, kind) || remembered[c.InfoHash].verdict != "" {
				continue
			}
			all[i].ProviderKinds = append(all[i].ProviderKinds, kind)
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	var cands []CacheHintCandidate
	for _, c := range all {
		if len(c.ProviderKinds) > 0 {
			cands = append(cands, c)
		}
	}
	return cands
}

// kindOf reads e's configured kind under the lock UpdateConfig holds while
// it replaces the configuration.
func (s *Service) kindOf(e *entry) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return e.cfg.Kind
}
