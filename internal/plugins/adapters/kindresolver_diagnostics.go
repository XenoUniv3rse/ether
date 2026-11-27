package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
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

func (r *KindResolver) recordKindFailure(ctx context.Context, item core.KindItem, err error) {
	if r.resolver == nil || !recordKindPlaybackDecision(ctx) {
		return
	}
	switch {
	case errors.Is(err, core.ErrBudgetExhausted):
		r.resolver.RecordKindFailure(ctx, item.Key, item.Title, core.ErrBudgetExhausted)
	case errors.Is(err, core.ErrKindCandidatesRejected):
		r.resolver.RecordKindFailure(ctx, item.Key, item.Title, core.ErrKindCandidatesRejected)
	case errors.Is(err, core.ErrArticleMissing):
		r.resolver.RecordKindFailure(ctx, item.Key, item.Title, core.ErrArticleMissing)
	case errors.Is(err, core.ErrArticleCorrupt):
		r.resolver.RecordKindFailure(ctx, item.Key, item.Title, core.ErrArticleCorrupt)
	case isPermanentReleaseFailure(err):
		r.resolver.RecordKindFailure(ctx, item.Key, item.Title, core.ErrReleaseRejected)
	}
}

func recordKindPlaybackDecision(ctx context.Context) bool {
	return core.IndexerPurpose(ctx) == core.IndexerPurposePlayback &&
		!core.SpeculativeIndexerWork(ctx) && !core.IsBackgroundCheck(ctx) && ctx.Err() == nil
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

func (r *KindResolver) recordKindRejection(ctx context.Context, pluginID string, item core.KindItem, reply json.RawMessage) {
	if !recordKindPlaybackDecision(ctx) {
		return
	}
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
	var rejections []kindRejectionDetail
	for _, candidate := range diagnostics.Debug.Rejected {
		if reason := clean(candidate.Reason); reason != "" {
			title := clean(candidate.Title)
			rejections = append(rejections, kindRejectionDetail{Title: title, Reason: reason})
			candidates = append(candidates, resolver.CandidateTrace{
				Name: title, Reason: reason, Rejected: true,
			})
		}
	}
	if detail == "" && len(rejections) == 0 {
		detail = "plugin_rejection_reason_missing"
	}
	r.log.Warn("kindresolver: plugin rejected all candidates", "plugin", pluginID,
		"key", item.Key, "reason", detail, "candidate_reasons", rejections)
	if r.resolver != nil {
		r.resolver.RecordKindRejection(ctx, item.Key, item.Title, detail, candidates)
	}
}

func (r *KindResolver) emptyKindResolve(ctx context.Context, item core.KindItem, part *int, key resolveFlightKey, state *resolveSearchState, excluded bool) (kindResolveResult, error) {
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
	if err := state.indexerPause(); err != nil {
		return kindResolveResult{}, err
	}
	if key.source == "" && !excluded {
		state.mu.Lock()
		found := state.candidatesFound
		state.mu.Unlock()
		if found {
			return kindResolveResult{}, core.ErrKindCandidatesRejected
		}
		if !key.background && r.items != nil {
			_ = r.items.SaveNegativePartResolution(ctx, item.Ref, part)
		}
	}
	return kindResolveResult{}, core.ErrNotAvailable
}
