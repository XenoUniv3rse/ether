package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/stream"
)

const (
	maxUsenetCheckCacheEntries = resolver.MaxUsenetVerdictEntries
	usenetCheckPositiveTTL     = resolver.UsenetVerdictPositiveTTL
	usenetCheckNegativeTTL     = resolver.UsenetVerdictNegativeTTL
	usenetCheckTimeout         = 15 * time.Second
)

type usenetArticleChecker interface {
	CheckArticles(ctx context.Context, releaseID string, nzbData []byte) (core.UsenetCheckResult, error)
}

func normalizeNZBKey(nzbURL string) string {
	return strings.TrimSpace(nzbURL)
}

func hashNZBBytes(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (r *KindResolver) verdicts() *resolver.UsenetVerdictCache {
	if r == nil {
		return nil
	}
	return r.usenetVerdicts
}

// PeekUsenetCheck returns a live shared verdict cached by keyless NZB URL
// without fetching from an indexer or spending any background check allowance.
func (r *KindResolver) PeekUsenetCheck(q core.SourceQuery) (core.SourceCandidate, bool) {
	if r == nil {
		return core.SourceCandidate{}, false
	}
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
	releaseKey := candidateNZBKey(cand)
	if releaseKey == "" {
		return core.SourceCandidate{}, false
	}
	verdict, ok := r.lookupUsenetCheck(releaseKey, "", "")
	if !ok {
		return core.SourceCandidate{}, false
	}
	cand.UsenetCheck = &verdict
	return r.publicReleaseLinks(cand), true
}

func (r *KindResolver) checkUsenetCandidate(ctx context.Context, cand core.SourceCandidate) (core.UsenetCheckResult, error) {
	cand = r.stripCandidateAuth(cand)
	releaseKey := candidateNZBKey(cand)
	if releaseKey == "" {
		return core.UsenetCheckResult{}, fmt.Errorf("kindresolver: usenet_check requires an nzbUrl")
	}
	if cached, ok := r.lookupUsenetCheck(releaseKey, "", ""); ok {
		noteResolveSourceWork(ctx, "usenet_check:"+releaseKey, true)
		return cached, nil
	}
	if r.usenetServing != nil && !r.usenetServing() {
		return core.UsenetCheckResult{}, core.ErrNotAvailable
	}
	if r.indexer == nil || r.engine == nil {
		return core.UsenetCheckResult{}, core.ErrNotAvailable
	}
	checker, ok := r.engine.(usenetArticleChecker)
	if !ok {
		return core.UsenetCheckResult{}, core.ErrNotAvailable
	}

	// Build a whole-NZB release descriptor (ignoring any fileIndex on the
	// candidate so FetchNZB returns the un-narrowed NZB through its
	// originating indexer, sharing the vault with playback).
	wholeCand := cand
	wholeCand.FileIndex = nil
	rel, err := releaseFromHint(core.ReleaseHint{Candidate: &wholeCand}, "plugin-kind-usenet-check")
	if err != nil {
		return core.UsenetCheckResult{}, err
	}
	rel.Protocol = core.ProtocolUsenet
	rel.Target = nil

	baseCtx := ctx
	if !core.IsBackgroundCheck(baseCtx) {
		if onDemandReleaseOpen(ctx) {
			// The plugin explicitly selected a candidate for an active
			// check. Article reads still use background priority.
			baseCtx = core.WithSpeculativeGrabs(baseCtx)
		}
		baseCtx = core.WithBackgroundBudget(baseCtx, func(context.Context, string, string, int64) error { return nil })
	}

	workKey := "usenet_check:" + releaseKey
	endWork := beginResolveSourceWork(ctx, workKey)
	defer endWork()

	raw, joined, err := r.runDetachedSourceFlight(baseCtx, workKey, usenetCheckTimeout, func(checkCtx context.Context) (any, error) {
		data, err := r.fetchPluginNZB(checkCtx, rel)
		if err != nil {
			return core.UsenetCheckResult{}, err
		}
		nzbHash := hashNZBBytes(data)
		contentID, _, _, _ := nzb.InspectContent(data)
		if cached, ok := r.lookupUsenetCheck(releaseKey, nzbHash, contentID); ok {
			return cached, nil
		}

		res, err := checker.CheckArticles(checkCtx, rel.ID, data)
		if err != nil {
			return core.UsenetCheckResult{}, err
		}
		res.Cached = false
		res.CheckedAt = r.now().Unix()
		return r.storeUsenetCheck(releaseKey, nzbHash, contentID, res), nil
	}, nil)
	if err != nil {
		return core.UsenetCheckResult{}, err
	}
	res, _ := raw.(core.UsenetCheckResult)
	if joined {
		res.Cached = true
	}
	return res, nil
}

func (r *KindResolver) lookupUsenetCheck(releaseKey, nzbHash, contentID string) (core.UsenetCheckResult, bool) {
	return r.verdicts().Lookup(r.now(), releaseKey, nzbHash, contentID)
}

func (r *KindResolver) storeUsenetCheck(releaseKey, nzbHash, contentID string, res core.UsenetCheckResult) core.UsenetCheckResult {
	return r.verdicts().Store(r.now(), releaseKey, nzbHash, contentID, res)
}

func (r *KindResolver) recordUsenetPlaybackFailure(releaseKey, nzbHash, contentID string, err error) {
	reason, articleMissing, ok := classifyUsenetPlaybackFailure(err)
	if !ok || (releaseKey == "" && nzbHash == "" && contentID == "") {
		return
	}
	r.verdicts().RecordPlaybackFailure(r.now(), releaseKey, nzbHash, contentID, reason, articleMissing)
}

func classifyUsenetPlaybackFailure(err error) (reason string, articleMissing bool, ok bool) {
	if err == nil ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, stream.ErrClosed) {
		return "", false, false
	}
	// Provider backoff / transient unavailability must not taint the NZB.
	if errors.Is(err, core.ErrSourceUnavailable) || errors.Is(err, core.ErrNotAvailable) {
		return "", false, false
	}
	switch {
	case errors.Is(err, core.ErrArticleMissing), errors.Is(err, stream.ErrIncomplete):
		return "playback_article_missing", true, true
	case errors.Is(err, core.ErrArticleCorrupt), errors.Is(err, stream.ErrCorrupt):
		return "playback_article_corrupt", false, true
	case errors.Is(err, core.ErrArchiveUnstreamable):
		return "playback_archive_unstreamable", false, true
	case errors.Is(err, core.ErrReleaseRejected), errors.Is(err, core.ErrNoUsableFile), stream.IsRejection(err):
		return "playback_release_rejected", false, true
	default:
		return "", false, false
	}
}

func (r *KindResolver) wrapUsenetPlaybackSource(ms core.MediaSource, releaseKey, nzbHash, contentID string) core.MediaSource {
	if ms == nil || (releaseKey == "" && nzbHash == "" && contentID == "") {
		return ms
	}
	record := func(err error) {
		r.recordUsenetPlaybackFailure(releaseKey, nzbHash, contentID, err)
	}
	return wrapObservedMediaSource(ms, record)
}

func wrapObservedMediaSource(ms core.MediaSource, record func(error)) core.MediaSource {
	base := &usenetObservedSource{
		MediaSource: ms,
		record:      record,
	}
	if avs, ok := ms.(core.ArchiveVolumeSource); ok {
		arch := &usenetObservedArchiveSource{
			usenetObservedSource: base,
			avs:                  avs,
		}
		if dr, ok := ms.(core.SourceDeliveryReporter); ok {
			return &usenetObservedArchiveDeliverySource{
				usenetObservedArchiveSource: arch,
				dr:                          dr,
			}
		}
		return arch
	}
	if dr, ok := ms.(core.SourceDeliveryReporter); ok {
		return &usenetObservedDeliverySource{
			usenetObservedSource: base,
			dr:                   dr,
		}
	}
	if cr, ok := ms.(core.ConcurrentRangeReaderAt); ok {
		return &usenetObservedConcurrentSource{
			usenetObservedSource: base,
			cr:                   cr,
		}
	}
	return base
}

type usenetObservedSource struct {
	core.MediaSource
	record func(error)
}

func (s *usenetObservedSource) ReadAt(p []byte, off int64) (int, error) {
	n, err := s.MediaSource.ReadAt(p, off)
	if err != nil {
		s.record(err)
	}
	return n, err
}

func (s *usenetObservedSource) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	var (
		n   int
		err error
	)
	if cr, ok := s.MediaSource.(core.ContextReaderAt); ok {
		n, err = cr.ReadAtContext(ctx, p, off)
	} else {
		n, err = s.MediaSource.ReadAt(p, off)
	}
	if err != nil {
		s.record(err)
	}
	return n, err
}

func (s *usenetObservedSource) ReadMetadataAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	var (
		n   int
		err error
	)
	if mr, ok := s.MediaSource.(core.MetadataReaderAt); ok {
		n, err = mr.ReadMetadataAtContext(ctx, p, off)
	} else {
		return s.ReadAtContext(ctx, p, off)
	}
	if err != nil {
		s.record(err)
	}
	return n, err
}

func (s *usenetObservedSource) ReadAtPrefetchContext(ctx context.Context, p []byte, off int64) (int, error) {
	var (
		n   int
		err error
	)
	if pr, ok := s.MediaSource.(core.PrefetchReaderAt); ok {
		n, err = pr.ReadAtPrefetchContext(ctx, p, off)
	} else {
		return s.ReadAtContext(ctx, p, off)
	}
	if err != nil {
		s.record(err)
	}
	return n, err
}

func (s *usenetObservedSource) ReadAtAnalysisContext(ctx context.Context, p []byte, off int64) (int, error) {
	var (
		n   int
		err error
	)
	if ar, ok := s.MediaSource.(core.AnalysisReaderAt); ok {
		n, err = ar.ReadAtAnalysisContext(ctx, p, off)
	} else {
		return s.ReadAtPrefetchContext(ctx, p, off)
	}
	if err != nil {
		s.record(err)
	}
	return n, err
}

func (s *usenetObservedSource) BufferedAhead() int64 {
	if bs, ok := s.MediaSource.(core.BufferedSource); ok {
		return bs.BufferedAhead()
	}
	return 0
}

func (s *usenetObservedSource) SetPlaybackDuration(d time.Duration) {
	if ps, ok := s.MediaSource.(core.PlaybackDurationSetter); ok {
		ps.SetPlaybackDuration(d)
	}
}

func (s *usenetObservedSource) SetDamageHandler(handler func(int64, int64) bool) {
	if dr, ok := s.MediaSource.(core.DamageReporter); ok {
		dr.SetDamageHandler(handler)
	}
}

func (s *usenetObservedSource) SetRepairHandler(handler func(core.RepairEvent) core.RepairWindow) {
	if rr, ok := s.MediaSource.(core.RepairReporter); ok {
		rr.SetRepairHandler(handler)
	}
}

func (s *usenetObservedSource) MediaCapWaitKnown() bool {
	if mc, ok := s.MediaSource.(core.MediaCapWaitSource); ok {
		return mc.MediaCapWaitKnown()
	}
	return false
}

func (s *usenetObservedSource) Origin() string {
	if os, ok := s.MediaSource.(core.OriginSource); ok {
		return os.Origin()
	}
	return ""
}

func (s *usenetObservedSource) MIME() string {
	if ms, ok := s.MediaSource.(core.MIMESource); ok {
		return ms.MIME()
	}
	return ""
}

func (s *usenetObservedSource) ResolvedArchiveMember() bool {
	if rm, ok := s.MediaSource.(core.ResolvedMemberSource); ok {
		return rm.ResolvedArchiveMember()
	}
	return false
}

type usenetObservedDeliverySource struct {
	*usenetObservedSource
	dr core.SourceDeliveryReporter
}

func (s *usenetObservedDeliverySource) SourceDelivery() core.SourceDelivery {
	return s.dr.SourceDelivery()
}

type usenetObservedConcurrentSource struct {
	*usenetObservedSource
	cr core.ConcurrentRangeReaderAt
}

func (s *usenetObservedConcurrentSource) ReadAtConcurrentContext(ctx context.Context, p []byte, off int64) (int, error) {
	n, err := s.cr.ReadAtConcurrentContext(ctx, p, off)
	if err != nil {
		s.record(err)
	}
	return n, err
}

type usenetObservedArchiveDeliverySource struct {
	*usenetObservedArchiveSource
	dr core.SourceDeliveryReporter
}

func (s *usenetObservedArchiveDeliverySource) SourceDelivery() core.SourceDelivery {
	return s.dr.SourceDelivery()
}

type usenetObservedArchiveSource struct {
	*usenetObservedSource
	avs core.ArchiveVolumeSource
}

func (s *usenetObservedArchiveSource) ArchiveVolumes() ([]core.MediaSource, string, string) {
	vols, format, member := s.avs.ArchiveVolumes()
	out := make([]core.MediaSource, len(vols))
	for i, vol := range vols {
		out[i] = wrapObservedMediaSource(vol, s.record)
	}
	return out, format, member
}

// usenetPlaybackFailed reports whether src names a Usenet NZB whose cached
// verdict records a permanent playback failure.
func (r *KindResolver) usenetPlaybackFailed(src core.PlayableSource) bool {
	if r == nil || src.Release == nil {
		return false
	}
	nzbURL := src.Release.NZBURL
	ref := src.Release.NZBRef
	var indexer, indexerID string
	if src.Release.Candidate != nil {
		if ref == "" {
			ref = src.Release.Candidate.NZBRef
		}
		if nzbURL == "" {
			nzbURL = src.Release.Candidate.NZBURL
		}
		indexer = src.Release.Candidate.Indexer
		indexerID = src.Release.Candidate.IndexerID
	}
	if nzbURL != "" {
		nzbURL = r.stripReleaseAuth(core.Release{
			NZBURL:    nzbURL,
			Indexer:   indexer,
			IndexerID: indexerID,
			Protocol:  core.ProtocolUsenet,
		}).NZBURL
	}
	key := candidateNZBKey(core.SourceCandidate{NZBURL: nzbURL, NZBRef: ref})
	if key == "" {
		return false
	}
	return r.verdicts().PlaybackFailed(r.now(), key, "", "")
}
