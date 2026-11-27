package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/logredact"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/textclip"
)

func noteKindSourceFailure(ctx context.Context, err error) {
	state := resolveSearchStateFrom(ctx)
	if state == nil || err == nil {
		return
	}
	if errors.Is(err, core.ErrBudgetExhausted) {
		noteResolveBudgetSkip(ctx)
		return
	}
	if !isPermanentReleaseFailure(err) {
		return
	}
	state.mu.Lock()
	state.candidatesFound = true
	state.mu.Unlock()
}

// kindDecisionError keeps History details off the playback error shown to clients.
type kindDecisionError struct {
	cause      error
	reason     string
	detail     string
	candidates []resolver.CandidateTrace
}

func (e *kindDecisionError) Error() string { return e.cause.Error() }
func (e *kindDecisionError) Unwrap() error { return e.cause }

func (s *resolveSearchState) searchError(cause error, reason string) error {
	var names []string
	if s != nil {
		s.mu.Lock()
		for _, outcome := range s.searches {
			names = append(names, outcome.unanswered...)
		}
		s.mu.Unlock()
	}
	slices.Sort(names)
	return &kindDecisionError{cause: cause, reason: reason, detail: strings.Join(slices.Compact(names), ", ")}
}

func (r *KindResolver) recordKindDecision(ctx context.Context, pluginID string, item core.KindItem, winner core.PlayableSource, sources []core.PlayableSource, failover bool, err error) {
	if r.resolver == nil || core.IndexerPurpose(ctx) != core.IndexerPurposePlayback ||
		core.SpeculativeIndexerWork(ctx) || core.IsBackgroundCheck(ctx) {
		return
	}
	tr := resolver.Trace{PluginKind: true, TitleKey: item.Key, TitleName: item.Title, Failover: failover}
	var decision *kindDecisionError
	switch {
	case errors.As(err, &decision):
		tr.Err, tr.Candidates = decision.reason, decision.candidates
		if decision.detail != "" {
			tr.Err += " (" + decision.detail + ")"
		}
	case err != nil:
		tr.Err = kindFailureReason(err)
	}
	if len(sources) > 0 || err == nil {
		candidate := func(src core.PlayableSource) resolver.CandidateTrace {
			rel := playableSourceRelease(src, item.Title)
			// Some source IDs incorporate a download URL. History only needs
			// an opaque ID to join the winner to its candidate.
			id := fmt.Sprintf("kind:history:%x", sha256.Sum256([]byte(rel.ID)))
			return resolver.CandidateTrace{
				ReleaseID: id, Name: logredact.Source(rel.Name), Size: rel.Size,
			}
		}
		if err == nil {
			chosen := candidate(winner)
			tr.Winner = chosen.ReleaseID
			tr.Candidates = append(tr.Candidates, chosen)
		}
		seen := map[string]bool{tr.Winner: true}
		for _, src := range sources {
			c := candidate(src)
			if !seen[c.ReleaseID] {
				tr.Candidates = append(tr.Candidates, c)
				seen[c.ReleaseID] = true
			}
		}
	}
	tr.TitleName = logredact.Source(tr.TitleName)
	if r.mgr == nil {
		r.resolver.RecordKindDecision(ctx, tr, nil)
		return
	}
	r.resolver.RecordKindDecision(ctx, tr, func(ctx context.Context, value string) string {
		return r.mgr.RedactDiagnostic(ctx, pluginID, value)
	})
}

func kindFailureReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, core.ErrPrepareSlow):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return context.Canceled.Error()
	}
	var paused *core.IndexersPausedError
	if errors.As(err, &paused) {
		return "indexer paused after repeated failures"
	}
	for _, reason := range []error{
		core.ErrBudgetExhausted, core.ErrKindCandidatesRejected, core.ErrArticleMissing,
		core.ErrArticleCorrupt, core.ErrNotAvailable, core.ErrDownloadQueued, core.ErrPreparing,
		core.ErrJobsBusy,
	} {
		if errors.Is(err, reason) {
			return reason.Error()
		}
	}
	if isPermanentReleaseFailure(err) {
		return core.ErrReleaseRejected.Error()
	}
	return core.ErrSourceUnavailable.Error()
}

type kindRejectionDetail struct {
	Title  string `json:"t"`
	Reason string `json:"why"`
}

type kindRejectionDiagnostics struct {
	Reason string `json:"reason"`
	Debug  struct {
		Reason   string                `json:"reason"`
		Rejected []kindRejectionDetail `json:"rejected"`
	} `json:"debug"`
}

func (r *KindResolver) kindRejection(ctx context.Context, pluginID string, reply json.RawMessage) error {
	// Some resolvers include owner-configured indexer names in their prose.
	// Preserve the reasons and release titles without carrying those labels.
	var indexerNames []string
	if dir, ok := r.indexer.(core.IndexerDirectory); ok {
		for _, idx := range dir.EnabledIndexers() {
			for _, name := range []string{idx.ConfiguredName, idx.Indexer, idx.IndexerID} {
				if name != "" {
					indexerNames = append(indexerNames, regexp.QuoteMeta(name))
				}
			}
		}
	}
	var indexers *regexp.Regexp
	if len(indexerNames) > 0 {
		indexers, _ = regexp.Compile(`(?i)\b(?:` + strings.Join(indexerNames, "|") + `)\b`)
	}
	clean := func(value string) string {
		value = logredact.Source(value)
		if indexers != nil {
			value = indexers.ReplaceAllString(value, "[indexer]")
		}
		return textclip.Bytes(strings.Join(strings.Fields(value), " "), 512)
	}

	var diagnostics, reported kindRejectionDiagnostics
	if json.Unmarshal(reply, &reported) == nil {
		reported.Debug.Rejected = reported.Debug.Rejected[:min(len(reported.Debug.Rejected), 50)]
		raw, _ := json.Marshal(reported)
		if err := json.Unmarshal([]byte(r.mgr.RedactDiagnostic(ctx, pluginID, string(raw))), &diagnostics); err != nil {
			diagnostics = kindRejectionDiagnostics{}
		}
	}
	detail := clean(diagnostics.Reason)
	if detail == "" {
		detail = clean(diagnostics.Debug.Reason)
	}
	var candidates []resolver.CandidateTrace
	for _, candidate := range diagnostics.Debug.Rejected {
		if reason := clean(candidate.Reason); reason != "" {
			title := clean(candidate.Title)
			candidates = append(candidates, resolver.CandidateTrace{
				Name: title, Reason: reason, Rejected: true,
			})
		}
	}
	if detail == "" && len(candidates) == 0 {
		detail = "plugin_rejection_reason_missing"
	}
	return &kindDecisionError{
		cause: core.ErrKindCandidatesRejected, reason: core.ErrKindCandidatesRejected.Error(),
		detail: detail, candidates: candidates,
	}
}

func (r *KindResolver) emptyKindResolve(ctx context.Context, pluginID string, item core.KindItem, part *int, key resolveFlightKey, state *resolveSearchState, excluded bool, reply json.RawMessage) (kindResolveResult, error) {
	if err := ctx.Err(); err != nil {
		return kindResolveResult{}, err
	}
	if state.indexerLookupLimitHit() {
		return kindResolveResult{}, core.ErrSourceUnavailable
	}
	if state.hasPending() {
		if state.playbackCtx != nil {
			return kindResolveResult{}, core.PreparingFromPending(250)
		}
		return kindResolveResult{}, core.PreparingFromPending(2000)
	}
	r.clearResolveProgress(key)
	if state.hadBudgetSkip() {
		return kindResolveResult{}, core.ErrBudgetExhausted
	}
	state.mu.Lock()
	found, incomplete, timedOut := state.candidatesFound, len(state.searches) > 0 || state.inFlight > 0, false
	for _, outcome := range state.searches {
		timedOut = timedOut || outcome.timedOut
	}
	state.mu.Unlock()
	if incomplete {
		reason := "media source temporarily unavailable (indexer search incomplete)"
		if timedOut {
			reason = "timed out"
		} else if err := state.indexerPause(); err != nil {
			return kindResolveResult{}, err
		}
		return kindResolveResult{}, state.searchError(core.PreparingFromPending(2000), reason)
	}
	if key.source == "" && !excluded {
		if found {
			return kindResolveResult{}, r.kindRejection(ctx, pluginID, reply)
		}
		if !key.background && r.items != nil {
			_ = r.items.SaveNegativePartResolution(ctx, item.Ref, part)
		}
	}
	return kindResolveResult{}, core.ErrNotAvailable
}
