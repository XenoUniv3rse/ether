package resolver

import (
	"context"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

var _ core.PreferenceFallbackObserver = (*Service)(nil)

// RecordPreferenceFallback also covers opens that reuse Auto without Resolve.
// Use the same bounded off-request-path queue as ordinary resolve outcomes.
func (s *Service) RecordPreferenceFallback(_ context.Context, req core.ResolveRequest, played, reason string, retry time.Duration) {
	if req.Title == nil {
		return
	}
	tr := Trace{At: s.now(), TitleKey: req.Title.Key, TitleName: consoleTitleName(req.Title), Winner: played, Chooser: chooserAuto,
		Downgrade: fmt.Sprintf("Picked version %s is using Auto: %s; retry after %s", req.Prefer, reason, retry)}
	// Error is surfaced by the history list; Downgrade retains the explanation
	// in the detailed decision, including successful automatic playback.
	tr.Err = tr.Downgrade
	s.addTrace(tr)
	s.queueHistory(tr, nil)
}

// PreferredVersionRecovered never searches, ranks the library, or reads storage.
// A newly discovered matching candidate warrants retrying the pick; admission
// still applies profile, identity, owner policy, and health checks normally.
func (s *Service) PreferredVersionRecovered(title *core.Title, prefer, played string, since time.Time) bool {
	if title == nil {
		return false
	}
	s.mu.RLock()
	e, ok := s.searchCache[searchCacheKey(searchRequestFor(title))]
	s.mu.RUnlock()
	if !ok || !e.refreshed.After(since) || !s.now().Before(e.expires) {
		return false
	}
	slot, logical := parseVersionSlot(prefer)
	if !logical {
		for _, r := range e.results {
			if r.ID == prefer && !s.isBadRelease(r) && !s.recentlyFailedRelease(r) {
				return true
			}
		}
		return false
	}
	ranked := make([]candidate, 0, len(e.results))
	auto := core.Release{ID: played}
	for _, r := range e.results {
		if r.ID == played {
			auto = r
		}
		if s.isBadRelease(r) || s.recentlyFailedRelease(r) {
			continue
		}
		ranked = append(ranked, candidate{rel: r})
	}
	if auto.ID == "" && len(ranked) > 0 {
		auto = ranked[0].rel
	}
	if slot.Ordinal < 0 || slot.Ordinal >= core.MaxVariants {
		return false
	}
	slots := []versionSlot{{Kind: "auto"}}
	if slot.Kind == "auto" {
		return auto.ID != ""
	}
	// Earlier alternatives consume their candidates before the picked ordinal.
	for n := 0; n <= slot.Ordinal; n++ {
		v := slot
		v.Ordinal = n
		slots = append(slots, v)
	}
	picks := slotChoices(slots, ranked, auto)
	return picks[len(picks)-1].ID != ""
}
