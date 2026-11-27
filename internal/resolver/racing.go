package resolver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
)

type candidate struct {
	sourcePreferred  bool     // source routing deliberately selected this candidate as the leader
	knownCachedHint  bool     // caller already knows this torrent candidate is cached on debrid
	identityWeak     bool     // shared title without evidence identifying this show
	localFirst       bool     // a granted local file the owner plays before any fetched release (Resolver.PreferLocalFiles)
	audioUnverified  bool     // required audio language is unverified in the release name (probe required)
	unverifiedLangs  []string // required audio languages awaiting probe verification
	belowScore       bool
	counted          int
	fallback         bool
	pluginKeys       map[string]int64
	policy           releasepolicy.Result
	rel              core.Release
	trace            int
	verified         bool
	langOK           bool            // declares no audio, or an accepted audio language (acceptedAudio)
	langRank         int             // 1-based viewer-language rank (1 = first choice, 2 = second, ...; 0 = unmatched)
	starved          bool            // below its resolution's bitrate floor (bitrateScore) or a presumed upscale
	bandwidth        int             // fits client cap (+1), unknown (0), or exceeds hard cap / stall limit (-1)
	delivery         int             // soft unverified source-delivery tie-break: fits/unknown (0) or above recent read rate (-1)
	identityConflict bool            // region evidence contradicts the original language
	yearGuessed      bool            // accepted despite a series year that differs from the show's
	cf               int             // custom format score: every matching format, plugin points included
	keys             rankKeys        // the Radarr/Sonarr comparer keys (ranking.go)
	prepared         *candidateCheck // validation overlapped with descriptor discovery
	graceStart       time.Time       // discovery start, set only when preparation overran the grace
	fallbacks        []candidate     // quiet fallbacks inside the same near-duplicate repost group
}

type validation struct {
	index                         int
	rel                           core.Release
	extraContentKeys              []string
	promoted                      *candidate
	fallbacks                     []fallbackValidation
	budgetExhausted               bool
	nzb                           []byte
	err                           error
	healthy, healthChecked        bool
	healthCached                  bool // answered by the verified cache, not a check
	fetchDuration, healthDuration time.Duration
}

const (
	validationBudget = 25 * time.Second
	candidateBudget  = 8 * time.Second
)

var errHealthBudget = fmt.Errorf("source validation limit reached; %w", core.ErrMoreCandidates)

// Close cancels and joins optimistic checks and search refills.
// Owners should close before the DB.
func (s *Service) Close() error {
	s.bgMu.Lock()
	s.bgClosed = true
	s.bgCancel()
	s.bgMu.Unlock()
	s.bgWG.Wait()
	s.SetBus(nil)
	return nil
}

func (s *Service) optimisticCheck(r core.Release, nzb []byte) {
	if s.health == nil {
		return
	}
	s.bgMu.Lock()
	if s.bgClosed {
		s.bgMu.Unlock()
		return
	}
	s.bgWG.Add(1)
	s.bgMu.Unlock()
	go func() {
		defer s.bgWG.Done()
		ctx, cancel := context.WithTimeout(s.bgCtx, 30*time.Second)
		defer cancel()
		// Post-start validation must not compete with the viewer it already
		// released. A no-op budget marks the check background without charging
		// the availability ledger.
		ctx = core.WithBackgroundBudget(ctx, func(context.Context, string, string, int64) error { return nil })
		r, extraKeys := s.learnReleaseContent(ctx, r, nzb)
		ok, _, err := s.checkHealthCachedKeys(ctx, r.ID, s.allVerificationKeys(r, extraKeys...), nzb)
		if ctx.Err() != nil {
			return
		}
		if (!ok && err == nil) || permanentFailure(err) {
			_ = s.MarkBad(ctx, r.ID, "optimistic background health check failed")
		}
	}()
}

func (s *Service) resolution(key core.RatingKey, cands []candidate, winner int, nzb []byte, tr *Trace) *core.Resolution {
	chosen := make([]candidate, 1, 1+s.maxCandidates())
	chosen[0] = cands[winner]
	res := s.resolutionSkipping(key, cands, winner, nzb, func(c candidate) bool {
		if tr.Candidates[c.trace].Rejected {
			return true
		}
		for _, prev := range chosen {
			if repostCandidatesMatch(prev, c) {
				return true
			}
		}
		chosen = append(chosen, c)
		return false
	})
	ct := tr.Candidates[cands[winner].trace]
	res.HealthVerified = ct.HealthChecked && ct.Healthy && !tr.Unverified
	if cands[winner].fallback || cands[winner].belowScore {
		res.QualityFallback = qualityFallbackReason(cands[winner].rel, tr)
		tr.Downgrade = res.QualityFallback
	}
	return res
}

// resolutionSkipping is resolution with the caller deciding which
// candidates cannot serve as failover alternates.
func (s *Service) resolutionSkipping(key core.RatingKey, cands []candidate, winner int, nzb []byte, skip func(candidate) bool) *core.Resolution {
	res := &core.Resolution{Title: key, Release: cands[winner].rel, NZB: nzb, ResolvedAt: s.now()}
	if cands[winner].keys.addonOrder > 0 {
		// Failover for an add-on-ordered source follows the ranked order
		// directly instead of regrouping by edition or cache status.
		for i, c := range cands {
			if i == winner || len(res.Alternates) >= s.maxCandidates() {
				continue
			}
			if skip(c) {
				continue
			}
			res.Alternates = append(res.Alternates, c.rel)
		}
		return res
	}
	// Failover stays on the same cut first: alternates of the winner's
	// edition come before other cuts (ranked order within each group).
	edition := cands[winner].rel.Info.Edition
	for _, same := range []bool{true, false} {
		// Keep a cached equivalent inside the bounded alternate list even
		// when source preference put many Usenet releases ahead of it.
		for _, ready := range []bool{true, false} {
			for i, c := range cands {
				cachedPeer := s.readyPeer([]candidate{cands[winner], c}) == 1
				if i == winner || cachedPeer != ready || len(res.Alternates) >= s.maxCandidates() {
					continue
				}
				if (c.rel.Info.Edition == edition) == same {
					if skip(c) {
						continue
					}
					res.Alternates = append(res.Alternates, c.rel)
				}
			}
		}
	}
	return res
}

func (s *Service) applyFallbackValidations(ctx context.Context, tr *Trace, fbs []fallbackValidation) error {
	var transient error
	for _, fv := range fbs {
		noteValidation(ctx, fv.rel, fv.val)
		if fv.trace >= 0 && fv.trace < len(tr.Candidates) {
			ct := &tr.Candidates[fv.trace]
			ct.Attempted = true
			ct.FetchDuration = fv.val.fetchDuration
			ct.HealthDuration = fv.val.healthDuration
			ct.HealthChecked = fv.val.healthChecked
			ct.Healthy = fv.val.healthy && fv.val.err == nil
			if !ct.Healthy {
				ct.Rejected = true
				ct.err = fv.val.err
				if fv.val.err != nil {
					ct.Reason = candidateErrorReason(fv.val.err)
				} else {
					ct.Reason = "health: unhealthy"
				}
			}
		}
		if fv.val.healthy && fv.val.err == nil {
			continue
		}
		if fv.val.err != nil && !permanentFailure(fv.val.err) {
			transient = errors.Join(transient, fv.val.err)
		} else {
			s.recordFailedValidation(ctx, fv.rel, fv.val)
			s.recordReputation(ctx, fv.rel.Indexer, false)
		}
	}
	return transient
}

// validateCandidates races bounded groups when enabled. Ranked mode still starts
// two checks together, preferring rank order during a short grace. A healthy
// equivalent can then bypass a stalled preferred source without losing quality.
// Losing checks are cancelled and joined, except an already-running fallback
// retained by ready-source validation under the service shutdown lifecycle.
func (s *Service) validateCandidates(ctx context.Context, cands []candidate, tr *Trace, race int) (int, []byte, error) {
	if !core.IsSpeculative(ctx) && !channelAvailabilityCheck(ctx) {
		if peer := s.readyPeer(cands); peer >= 0 {
			return s.validateReady(ctx, cands, tr, peer)
		}
	}
	if len(cands) > 0 && cands[0].fallback {
		// Even an owner-enabled health race must preserve fallback tier order.
		race = 1
	}
	if race <= 0 {
		return s.validateRanked(ctx, cands, tr)
	}
	ctx, cancelBudget := context.WithTimeout(ctx, validationBudget)
	defer cancelBudget()
	attempts := &atomic.Int64{}
	ctx = context.WithValue(ctx, healthAttemptsKey{}, attempts)
	width := min(race, 4)
	limit := min(len(cands), maxHealthAttempts)
	var transient error
	budgetHit := false
	for base := 0; base < limit; {
		if core.IsSpeculative(ctx) && base >= speculativeHealthAttempts {
			budgetHit = true
			break
		}
		if pastPlayDeadlineWithUnverified(ctx) {
			break
		}
		if attempts.Load() >= int64(maxHealthAttempts) {
			budgetHit = true
			break
		}
		if cands[base].rel.OpensOnDemand() {
			if !core.OnDemandAllowedAt(ctx, cands[base].rel, base) {
				return -1, nil, errors.Join(transient, errOnDemandForeground)
			}
		}
		count := min(width, limit-base)
		if cands[base].verified || cands[base].rel.OpensOnDemand() || cands[base].keys.addonOrder > 0 {
			// Reusing a known working release must not start speculative
			// downloads and health checks for other releases alongside it,
			// an on-demand open makes its add-on fetch the release, and
			// add-on-ordered sources validate strictly in add-on order.
			count = 1
		} else {
			// A race stays inside one policy tier and priority band. A healthy
			// fallback must not be selected while a primary in this window is
			// still valid. It never includes an on-demand link or an add-on-ordered
			// release.
			same := 1
			for same < count && !cands[base+same].rel.OpensOnDemand() && cands[base+same].keys.addonOrder == 0 && cands[base+same].keys.sourceBand == cands[base].keys.sourceBand && samePreferenceTier(cands[base], cands[base+same]) && releasepolicy.Equivalent(cands[base].policy, cands[base+same].policy) && pluginKeysEqual(cands[base], cands[base+same]) {
				same++
			}
			count = same
		}
		workCtx, cancel := context.WithCancel(ctx)
		results := make(chan validation, count)
		var wg sync.WaitGroup
		for j := 0; j < count; j++ {
			i := base + j
			s.launchValidation(&wg, workCtx, cands[i], i, base+count >= limit, results)
		}
		winner := -1
		var nzb []byte
		for j := 0; j < count; j++ {
			v := <-results
			if v.budgetExhausted {
				budgetHit = true
			}
			origLeader := cands[v.index]
			if len(v.fallbacks) > 0 {
				if fterr := s.applyFallbackValidations(ctx, tr, v.fallbacks); fterr != nil && v.promoted == nil {
					transient = errors.Join(transient, fterr)
				}
			}
			if v.promoted != nil {
				// Mark the failed group leader in trace before promoting the fallback.
				lct := &tr.Candidates[origLeader.trace]
				lct.Attempted = true
				lct.Rejected = true
				if lct.Reason == "" {
					lct.Reason = "health: unhealthy"
				}
				cands[v.index] = *v.promoted
			} else if v.rel.ID != "" {
				cands[v.index].rel = v.rel
			}
			if v.promoted == nil {
				noteValidation(ctx, cands[v.index].rel, v)
			}
			if errors.Is(v.err, errHealthBudget) && !v.healthChecked && len(v.nzb) == 0 && v.promoted == nil && len(v.fallbacks) == 0 {
				budgetHit = true
				continue
			}
			ct := &tr.Candidates[cands[v.index].trace]
			ct.Attempted = true
			ct.FetchDuration = v.fetchDuration
			ct.HealthDuration = v.healthDuration
			ct.HealthChecked = v.healthChecked
			ct.Healthy = v.healthy && v.err == nil
			if ctx.Err() != nil {
				cancel()
				wg.Wait()
				drainValidations(ctx, cands, results)
				return -1, nil, ctx.Err()
			}
			if ct.Healthy {
				if v.healthChecked && !v.healthCached {
					// A real check always counts, even when a coarse clock
					// (Windows) measured it as taking no time.
					s.markVerifiedKeys(ctx, s.allVerificationKeys(cands[v.index].rel, v.extraContentKeys...))
					s.noteStartup(cands[v.index].rel, max(v.fetchDuration+v.healthDuration, time.Microsecond))
				}
				s.recordReputation(ctx, cands[v.index].rel.Indexer, true)
				winner = v.index
				nzb = v.nzb
				break
			}
			ct.Rejected = true
			ct.err = v.err
			if v.err != nil {
				ct.Reason = candidateErrorReason(v.err)
			} else {
				ct.Reason = "health: unhealthy"
			}
			if v.err != nil && !permanentFailure(v.err) {
				if !errors.Is(v.err, errHealthBudget) {
					transient = errors.Join(transient, v.err)
				}
			} else {
				s.recordFailedValidation(ctx, origLeader.rel, v)
				s.recordReputation(ctx, origLeader.rel.Indexer, false)
			}
		}
		cancel()
		wg.Wait()
		if winner >= 0 {
			return winner, nzb, nil
		}
		base += count
	}
	if transient == nil && (budgetHit || len(cands) > limit || (core.IsSpeculative(ctx) && len(cands) > speculativeHealthAttempts)) {
		return -1, nil, errHealthBudget
	}
	return -1, nil, transient
}

// compatibilityNote explains how a release plays on the client: the
// transcodes it needs and whether it fits the client's bitrate cap. The
// streaming formats (formats_builtin.go) score the same facts.
func compatibilityNote(r core.Release, c core.ClientCaps, t *core.Title) string {
	var notes []string
	if r.Info.VideoCodec != "" && len(c.VideoCodecs) > 0 && !c.SupportsVideo(r.Info.VideoCodec) {
		notes = append(notes, "video transcode required")
	}
	if r.Info.AudioCodec != "" && len(c.AudioCodecs) > 0 {
		if c.SupportsAudio(r.Info.AudioCodec) {
			notes = append(notes, "audio copies")
		} else {
			notes = append(notes, "audio transcode required")
		}
	}
	if c.MaxBitrateKbps > 0 && r.Size > 0 && t != nil && t.Runtime > 0 {
		kbps := float64(r.Size) * 8 / t.Runtime.Seconds() / 1000
		// NZB sizes include archive overhead; reserve 10% for instantaneous peaks.
		if kbps*1.1 <= float64(c.MaxBitrateKbps) {
			notes = append(notes, fmt.Sprintf("estimated %.0f kbps fits client cap", kbps))
		} else {
			notes = append(notes, fmt.Sprintf("estimated %.0f kbps exceeds client cap", kbps))
		}
	}
	return strings.Join(notes, "; ")
}

func permanentFailure(err error) bool {
	return errors.Is(err, core.ErrArticleMissing) || errors.Is(err, core.ErrArticleCorrupt) || errors.Is(err, core.ErrReleaseRejected)
}

func qualityFallbackReason(rel core.Release, tr *Trace) string {
	for _, c := range tr.Candidates {
		if c.ReleaseID != rel.ID {
			continue
		}
		if c.MinimumFormatScore != nil {
			score := 0
			for _, match := range c.CustomFormats {
				if core.CountsTowardMinimum(match.ID) {
					score += match.Score
				}
			}
			return fmt.Sprintf("Best available — below this profile's minimum (score %d of %d)", score, *c.MinimumFormatScore)
		}
		if !c.Rejected && strings.HasPrefix(c.Reason, "Best available") {
			return c.Reason
		}
	}
	height := fallbackHeight(rel.Info)
	label := "unknown resolution"
	if height > 0 {
		label = fmt.Sprintf("%dp", height)
		if source := core.SourceLabel(rel.Info.Source); source != "" {
			label += " " + source
		}
	}
	if len(tr.Candidates) > 0 && tr.Limits != nil && height > 0 && height < tr.Limits.ProfileMinHeight {
		return fmt.Sprintf("Played in %s: no acceptable %dp or better release was found.", label, tr.Limits.ProfileMinHeight)
	}
	return fmt.Sprintf("Played in %s: none of this profile's allowed qualities were found.", label)
}
