package resolver

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Source routing: among releases of equivalent quality, the source that
// starts playback fastest goes first.
//
// Quality is never traded for speed. Routing only reorders candidates in
// the leader's quality class (sameQuality: height, HDR kind, source class,
// bitrate floor and audio language) whose score is within a margin of the
// leader's. Within that group:
//
//   - "auto" (the default) prefers the source with the lower measured
//     startup time once both have enough samples and differ by a clear
//     margin. Until then, popular titles prefer cached debrid: a cached
//     torrent is one HTTPS range read from a CDN, while Usenet needs an NZB
//     grab, an article health check and archive parsing.
//   - "usenet" and "debrid" always prefer that source.
//
// Only torrents known to be cached on a debrid service count as fast; an
// uncached torrent would cost a provider round trip just to be skipped.
// The best release of the other source is then placed second, so ranked
// validation (which starts the top two checks together) hedges across
// sources, and failover alternates include both.
//
// Startup is measured as NZB/payload fetch plus a health check that really
// ran, for each validation that passed: for Usenet that covers the indexer
// grab, article checks and first articles; for debrid the provider calls
// and the first block. Verified-cache answers are left out.

const (
	PreferAuto   = "auto"
	PreferUsenet = "usenet"
	PreferDebrid = "debrid"

	defaultSourceScoreMargin = 25
	// minStartupSamples per source before measurements decide.
	minStartupSamples = 5
	// minStartupGain is the smallest measured difference that reorders.
	minStartupGain = 250 * time.Millisecond
	startupAlpha   = 0.2

	// A title is popular enough to default to cached debrid when TMDB rates
	// it this popular or with this many votes, or its best torrent has this
	// many seeders (episodes often carry no popularity of their own).
	popularPopularity = 50
	popularVotes      = 1000
	popularSeeders    = 50
)

// SourceSignals is what routing needs from the debrid service.
type SourceSignals interface {
	// KnownCached reports whether a debrid provider is known to hold the
	// torrent (from bulk cache checks or earlier plays), without a request.
	KnownCached(infoHash string) bool
}

// SourceStartup is a source's measured startup time.
type SourceStartup struct {
	Protocol string        `json:"protocol"` // "usenet" or "torrent"
	Average  time.Duration `json:"average"`  // exponentially weighted
	Last     time.Duration `json:"last"`
	Samples  int64         `json:"samples"`
}

type routingState struct {
	mu      sync.Mutex
	signals SourceSignals
	startup map[string]*SourceStartup
}

// SetSourceSignals wires the debrid cache knowledge; nil disables it.
func (s *Service) SetSourceSignals(sig SourceSignals) {
	s.route.mu.Lock()
	s.route.signals = sig
	s.route.mu.Unlock()
}

// SourceStartups returns the measured startup per source.
func (s *Service) SourceStartups() []SourceStartup {
	s.route.mu.Lock()
	defer s.route.mu.Unlock()
	out := []SourceStartup{}
	for _, p := range []string{core.ProtocolUsenet, core.ProtocolTorrent} {
		if st := s.route.startup[p]; st != nil {
			out = append(out, *st)
		}
	}
	return out
}

// UsenetBackingOff reports whether every primary Usenet provider is currently
// in circuit-breaker backoff (for example NNTP 481 simultaneous IP refusals).
func (s *Service) UsenetBackingOff() bool {
	return s.usenetBackingOff()
}

func (s *Service) usenetBackingOff() bool {
	return s != nil && s.usenetServing != nil && !s.usenetServing()
}

// demoteBackingOffUsenet places non-Usenet releases ahead of Usenet releases
// within each identity-safety tier while all Usenet providers are backing off,
// keeping Usenet as a last-resort fallback when no other source exists.
func (s *Service) demoteBackingOffUsenet(cands []candidate, trace *Trace) []candidate {
	if !s.usenetBackingOff() || len(cands) < 2 {
		return cands
	}
	hasUsenet, hasOther := false, false
	for _, c := range cands {
		if protocolOf(c.rel) == core.ProtocolUsenet {
			hasUsenet = true
		} else {
			hasOther = true
		}
	}
	if !hasUsenet || !hasOther {
		return cands
	}
	partition := func(in []candidate) []candidate {
		if len(in) < 2 {
			return in
		}
		other := make([]candidate, 0, len(in))
		usenet := make([]candidate, 0, len(in))
		for _, c := range in {
			if protocolOf(c.rel) == core.ProtocolUsenet {
				usenet = append(usenet, c)
			} else {
				other = append(other, c)
			}
		}
		return append(other, usenet...)
	}
	var strongConfirmed, strongUnverified, weak []candidate
	for _, c := range cands {
		switch {
		case c.identityWeak:
			weak = append(weak, c)
		case c.audioUnverified:
			strongUnverified = append(strongUnverified, c)
		default:
			strongConfirmed = append(strongConfirmed, c)
		}
	}
	out := make([]candidate, 0, len(cands))
	out = append(out, partition(strongConfirmed)...)
	out = append(out, partition(strongUnverified)...)
	out = append(out, partition(weak)...)
	if trace != nil && len(out) > 0 && out[0].rel.ID != cands[0].rel.ID && protocolOf(out[0].rel) != core.ProtocolUsenet {
		if i := out[0].trace; i >= 0 && i < len(trace.Candidates) {
			trace.Candidates[i].Route = "usenet provider backing off"
		}
		trace.Routing = "debrid first: usenet provider backing off"
	}
	return out
}

// protocolOf is a release's routing source. An addon's HTTP stream is
// read like a debrid file (HTTPS ranges from a CDN), so it routes with
// debrid; it is never known to be cached, so only an explicit debrid
// preference moves it first.
func protocolOf(rel core.Release) string {
	if rel.Protocol == core.ProtocolTorrent || rel.Protocol == core.ProtocolHTTP {
		return core.ProtocolTorrent
	}
	return core.ProtocolUsenet
}

func sourceName(protocol string) string {
	if protocol == core.ProtocolTorrent {
		return "debrid"
	}
	return "usenet"
}

// ObserveSourceStartup records one completed source open duration into the
// resolver's shared protocol startup statistics (used for auto routing and
// grab-hedge timing).
func (s *Service) ObserveSourceStartup(rel core.Release, took time.Duration) {
	s.noteStartup(rel, took)
}

func (s *Service) noteStartup(rel core.Release, d time.Duration) {
	if d <= 0 {
		return
	}
	p := protocolOf(rel)
	s.route.mu.Lock()
	defer s.route.mu.Unlock()
	if s.route.startup == nil {
		s.route.startup = map[string]*SourceStartup{}
	}
	st := s.route.startup[p]
	if st == nil {
		st = &SourceStartup{Protocol: p, Average: d}
		s.route.startup[p] = st
	} else {
		st.Average = time.Duration(float64(st.Average)*(1-startupAlpha) + float64(d)*startupAlpha)
	}
	st.Last = d
	st.Samples++
}

func (s *Service) startupOf(p string) (time.Duration, bool) {
	s.route.mu.Lock()
	defer s.route.mu.Unlock()
	st := s.route.startup[p]
	if st == nil || st.Samples < minStartupSamples {
		return 0, false
	}
	return st.Average, true
}

func (s *Service) knownCached(rel core.Release) bool {
	if rel.Protocol != core.ProtocolTorrent || rel.InfoHash == "" {
		return false
	}
	s.route.mu.Lock()
	sig := s.route.signals
	s.route.mu.Unlock()
	if len(rel.DebridCached) > 0 {
		if providers, ok := sig.(interface {
			KnownCachedBy(string, []string) []core.DebridServiceRef
		}); ok {
			return len(providers.KnownCachedBy(rel.InfoHash, rel.DebridCached)) > 0
		}
	}
	return sig != nil && sig.KnownCached(rel.InfoHash)
}

func (s *Service) candidateKnownCached(c candidate) bool {
	return c.knownCachedHint || s.knownCached(c.rel)
}

// routeSources reorders ranked candidates (best first) by source, as
// described above. It never changes which quality tier leads.
func (s *Service) routeSources(cands []candidate, t *core.Title, trace *Trace) []candidate {
	if len(cands) < 2 {
		return cands
	}
	usenetBackoff := s.usenetBackingOff()
	if usenetBackoff {
		cands = s.demoteBackingOffUsenet(cands, trace)
	}
	rcfg := s.resolverCfg()
	mode := strings.ToLower(strings.TrimSpace(rcfg.SourcePreference))
	if usenetBackoff && mode == PreferUsenet {
		mode = PreferAuto
	}
	explicit := mode == PreferDebrid || mode == PreferUsenet
	preferredProtocol := core.ProtocolUsenet
	if mode == PreferDebrid {
		preferredProtocol = core.ProtocolTorrent
	}
	if len(cands) > 0 && cands[0].keys.addonOrder > 0 && trace != nil && trace.Routing == "" {
		if cands[0].rel.Indexer != "" {
			trace.Routing = "first compatible result from " + cands[0].rel.Indexer + " (add-on order)"
		} else {
			trace.Routing = "first compatible result (add-on order)"
		}
	}
	if explicit && rcfg.SourceAlwaysFirst {
		return s.sourceAlwaysFirst(cands, preferredProtocol, trace)
	}
	if len(cands) > 0 && cands[0].keys.addonOrder > 0 {
		return cands
	}
	// Automatic routing reuses a verified source. An explicit owner change
	// still takes effect for equal-quality releases, even after prior plays.
	if cands[0].verified && (!explicit || protocolOf(cands[0].rel) == preferredProtocol) {
		return cands
	}
	margin := rcfg.SourceScoreMargin
	if margin <= 0 {
		margin = defaultSourceScoreMargin
	}
	lead := cands[0]
	// Verified candidates are promoted ahead of the score order, so equal-tier
	// alternatives need not be adjacent to the leader. Explicit source choices
	// must consider the whole eligible tier instead of stopping at a verified
	// release of another quality.
	var group []candidate
	var indexes []int
	for i, c := range cands {
		eligible := !(c.verified && c.rel.Score < lead.rel.Score) && pluginKeysEqual(c, lead) && c.bandwidth >= lead.bandwidth && (explicit || c.delivery >= lead.delivery) && (!c.verified || explicit) && sameQuality(c, lead) && c.rel.Score >= lead.rel.Score-margin
		if !eligible {
			if !explicit {
				break
			}
			continue
		}
		group = append(group, c)
		indexes = append(indexes, i)
	}
	hasUsenet, hasTorrent := false, false
	for _, c := range group {
		if protocolOf(c.rel) == core.ProtocolTorrent {
			hasTorrent = true
		} else {
			hasUsenet = true
		}
	}
	if hasTorrent && !hasUsenet {
		// Availability still matters when only torrents are eligible. The
		// search cache check may already know a ready equivalent, while a
		// slightly higher-scored descriptor has no hash yet. Promote the
		// ready copy before preparation so unrelated descriptor discovery
		// does not delay its validation. Keep unknown copies as fallbacks.
		for _, i := range indexes {
			if !s.candidateKnownCached(cands[i]) || !equivalentHedge(lead, cands[i], margin) {
				continue
			}
			if i == 0 {
				return cands
			}
			out := make([]candidate, 0, len(cands))
			out = append(out, cands[i])
			out = append(out, cands[:i]...)
			out = append(out, cands[i+1:]...)
			if trace != nil && out[0].trace >= 0 && out[0].trace < len(trace.Candidates) {
				trace.Candidates[out[0].trace].Route = "ready equivalent: cached on debrid"
			}
			return out
		}
		return cands
	}
	if !hasTorrent {
		return cands
	}

	var want, why string
	var hasWant bool
	switch mode {
	case PreferUsenet:
		want, why, hasWant = core.ProtocolUsenet, "owner prefers usenet", true
	case PreferDebrid:
		want, why, hasWant = core.ProtocolTorrent, "owner prefers debrid", true
	default:
		want, why, hasWant = s.autoSource(group, t)
	}
	if !hasWant {
		return cands
	}
	fast := func(c candidate) bool {
		if protocolOf(c.rel) != want {
			return false
		}
		// Under auto, only a known-cached torrent is fast; an owner who
		// prefers debrid accepts the provider round trip.
		return want != core.ProtocolTorrent || mode == PreferDebrid || s.candidateKnownCached(c)
	}
	first := -1
	// An explicit debrid preference still means ready-to-stream first.
	// Unknown availability is a fallback, not a reason to spend the whole
	// validation budget before a known-cached equivalent gets a chance.
	if want == core.ProtocolTorrent {
		for i, c := range group {
			if s.candidateKnownCached(c) {
				first = indexes[i]
				break
			}
		}
	}
	for i, c := range group {
		if first < 0 && fast(c) {
			first = indexes[i]
			break
		}
	}
	if first < 0 {
		return cands
	}
	out := make([]candidate, 0, len(cands))
	out = append(out, cands[first])
	out[0].sourcePreferred = true
	// Hedge: the best of the other source second, so ranked validation
	// checks both sources at once.
	other := -1
	for i, c := range group {
		if indexes[i] != first && protocolOf(c.rel) != want {
			other = indexes[i]
			break
		}
	}
	if other >= 0 {
		out = append(out, cands[other])
	}
	for i, c := range cands {
		if i != first && i != other {
			out = append(out, c)
		}
	}
	if trace != nil {
		note := "faster source: " + why
		if protocolOf(out[0].rel) == core.ProtocolTorrent && s.candidateKnownCached(out[0]) {
			note += " (cached on debrid)"
		}
		if out[0].trace >= 0 && out[0].trace < len(trace.Candidates) {
			trace.Candidates[out[0].trace].Route = note
		}
		if other >= 0 && out[1].trace >= 0 && out[1].trace < len(trace.Candidates) {
			trace.Candidates[out[1].trace].Route = "hedge: checked alongside on " + sourceName(protocolOf(out[1].rel))
		}
		trace.Routing = sourceName(want) + " first: " + why
	}
	if usenetBackoff {
		return s.demoteBackingOffUsenet(out, nil)
	}
	return out
}

// autoSource picks the preferred protocol for "auto": measured startup
// first, then popularity. ok == false means no preference.
func (s *Service) autoSource(group []candidate, t *core.Title) (string, string, bool) {
	u, uok := s.startupOf(core.ProtocolUsenet)
	d, dok := s.startupOf(core.ProtocolTorrent)
	if uok && dok {
		switch {
		case u-d >= minStartupGain:
			return core.ProtocolTorrent, fmt.Sprintf("measured debrid %s vs usenet %s", roundMs(d), roundMs(u)), true
		case d-u >= minStartupGain:
			return core.ProtocolUsenet, fmt.Sprintf("measured usenet %s vs debrid %s", roundMs(u), roundMs(d)), true
		}
		// Too close to call: fall through to popularity.
	}
	if popular(group, t) {
		return core.ProtocolTorrent, "popular title", true
	}
	return "", "", false
}

func popular(group []candidate, t *core.Title) bool {
	if t != nil && (t.Popularity >= popularPopularity || t.VoteCount >= popularVotes) {
		return true
	}
	for _, c := range group {
		if c.rel.Protocol == core.ProtocolTorrent && c.rel.Seeders >= popularSeeders {
			return true
		}
	}
	return false
}

func roundMs(d time.Duration) string {
	return d.Round(10 * time.Millisecond).String()
}

// sourceAlwaysFirst is the owner's "always" source order: every release of
// the preferred source (for debrid, every known-cached one) goes ahead of
// the rest, whatever its quality or connection fit, and the rest stay as
// fallbacks in rank order. Never-play fallback tiers and the audio-language
// check still come first, so a preferred source is never picked in a
// language nobody asked for.
func (s *Service) sourceAlwaysFirst(cands []candidate, want string, trace *Trace) []candidate {
	preferred := func(c candidate) bool {
		if protocolOf(c.rel) != want {
			return false
		}
		return want != core.ProtocolTorrent || s.candidateKnownCached(c)
	}
	out := slices.Clone(cands)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !samePreferenceTier(a, b) {
			return compareCandidates(a, b)
		}
		if a.policy.Fallback != b.policy.Fallback {
			return a.policy.Fallback < b.policy.Fallback
		}
		if a.langOK != b.langOK {
			return a.langOK
		}
		if a.keys.sourceBand != b.keys.sourceBand {
			return a.keys.sourceBand < b.keys.sourceBand
		}
		if a.keys.addonOrder > 0 && b.keys.addonOrder > 0 {
			if a.keys.addonOrder != b.keys.addonOrder {
				return a.keys.addonOrder < b.keys.addonOrder
			}
			return !a.keys.pack && b.keys.pack
		}

		return preferred(a) && !preferred(b)
	})
	// A remembered success may break a score tie, but cannot jump ahead
	// of a higher-scored lead solely through source preference.
	if len(out) > 0 && out[0].verified && out[0].rel.Score < cands[0].rel.Score {
		return cands
	}
	if trace != nil && len(out) > 0 && preferred(out[0]) && out[0].rel.ID != cands[0].rel.ID {
		if i := out[0].trace; i >= 0 && i < len(trace.Candidates) {
			trace.Candidates[i].Route = "owner puts " + sourceName(want) + " first"
		}
	}
	return out
}

// PlayableRouteCandidate describes one candidate inside an interchangeable
// plugin tier for RoutePlayableTier.
type PlayableRouteCandidate struct {
	// Release supplies protocol, ID, infohash, size, and seeders for routing,
	// debrid-cache lookup, and delivery/stall evidence.
	Release core.Release
	// KnownCached marks a torrent candidate already known to be cached on
	// debrid (for example from SourceCandidate.Cached / CachedBy), in addition
	// to the resolver's live debrid cache signal.
	KnownCached bool
}

// RoutePlayableTier returns the reordered indices [0..len(items)-1] for
// interchangeable sources within one plugin tier, applying Ether's delivery /
// stall demotion (delivery.go) and source routing (routing.go: auto/usenet/
// debrid, measured startup, known-cached debrid, and Usenet backoff).
func (s *Service) RoutePlayableTier(items []PlayableRouteCandidate, title *core.Title) []int {
	if len(items) < 2 {
		out := make([]int, len(items))
		for i := range items {
			out[i] = i
		}
		return out
	}
	cands := make([]candidate, len(items))
	for i, item := range items {
		bw, deliv, _ := s.deliveryFitReason(item.Release, core.ClientCaps{}, title)
		if bw == 0 && deliv == 0 && (item.Release.Size <= 0 || title == nil || title.Runtime <= 0) && s.hasRecentStall(item.Release) {
			bw, deliv = -1, -1
		}
		// Within one plugin tier the plugin has already declared all items
		// interchangeable; zero movie/TV-specific quality/score fields so
		// routeSources treats every item in the tier as same-quality while
		// still honoring Protocol, ID, InfoHash, Size, Seeders, and delivery.
		rel := item.Release
		rel.Info = core.ReleaseInfo{}
		rel.Score = 0
		cands[i] = candidate{
			rel:             rel,
			trace:           i,
			bandwidth:       bw,
			delivery:        deliv,
			knownCachedHint: item.KnownCached && rel.Protocol == core.ProtocolTorrent,
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].bandwidth != cands[j].bandwidth {
			return cands[i].bandwidth > cands[j].bandwidth
		}
		return cands[i].delivery > cands[j].delivery
	})
	routed := s.routeSources(cands, title, nil)
	order := make([]int, len(routed))
	for i, c := range routed {
		order[i] = c.trace
	}
	return order
}
