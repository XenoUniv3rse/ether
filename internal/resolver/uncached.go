package resolver

import (
	"context"
	"errors"
	"fmt"

	"github.com/braedonsaunders/ether/internal/core"
)

// downloadQueuer is an indexer that can send an uncached torrent release to
// the owner's debrid service to download (debrid.Indexer).
type downloadQueuer interface {
	QueueDownload(ctx context.Context, rel core.Release) ([]byte, error)
}

// usenetInventory reports whether a Usenet provider is set up
// (debrid.Indexer).
type usenetInventory interface {
	UsenetReady() bool
}

// maxQueueAttempts bounds how many releases one resolve offers for
// download when the debrid service refuses the best ones.
const maxQueueAttempts = 3

// uncachedOutcome settles a resolve whose checked releases all failed and
// at least one torrent failed only because no debrid account holds it.
// Every other failure must be a Usenet release that is confirmed broken;
// anything else (a timeout, a provider outage) is not this case and ok is
// false.
//
// With cached-only on (or for a warm-up, which never starts downloads) the
// answer is a *core.UncachedError. With it off, the best uncached release in
// ranked order is sent to download through the indexer (QueueDownload): the
// answer then wraps core.ErrDownloadQueued, or core.ErrDownloadRefused /
// core.ErrDownloadLimit when the service would not take it; a release the
// provider turns out to hold plays at once (index >= 0). The trace error
// says the same thing the player is told, for Quality › History.
func (s *Service) uncachedOutcome(ctx context.Context, ranked []candidate, results []core.Release, tr *Trace) (index int, payload []byte, err error, ok bool) {
	var uncached []int
	usenetTried := false
	for i, c := range ranked {
		ct := &tr.Candidates[c.trace]
		if !ct.Attempted || !ct.Rejected {
			continue
		}
		switch {
		case ct.err != nil && errors.Is(ct.err, core.ErrNotCached):
			if protocolOf(c.rel) == core.ProtocolTorrent {
				uncached = append(uncached, i)
			}
		case protocolOf(c.rel) == core.ProtocolUsenet && (ct.err == nil || permanentFailure(ct.err)):
			usenetTried = true // unhealthy or missing articles
		default:
			return -1, nil, nil, false
		}
	}
	if len(uncached) == 0 || ctx.Err() != nil {
		return -1, nil, nil, false
	}
	usenet := ""
	if inv, ok := s.indexer.(usenetInventory); ok && inv.UsenetReady() {
		usenet = core.UsenetNone
		for _, r := range results {
			if protocolOf(r) == core.ProtocolUsenet {
				usenet = core.UsenetFailed
				break
			}
		}
		if usenetTried {
			usenet = core.UsenetFailed
		}
	}
	var errs []error
	for _, i := range uncached {
		errs = append(errs, tr.Candidates[ranked[i].trace].err)
	}
	out := &core.UncachedError{Err: errors.Join(errs...), CachedOnly: s.cachedOnly(), Usenet: usenet}

	q, canQueue := s.indexer.(downloadQueuer)
	if out.CachedOnly || !canQueue || core.IsBackgroundCheck(ctx) || core.IsSpeculative(ctx) || core.IndexerPurpose(ctx) == core.IndexerPurposeWarmup {
		tr.Err = uncachedSummary(out, nil)
		return -1, nil, out, true
	}
	var refused []error
	for n, i := range uncached {
		if n == maxQueueAttempts {
			break
		}
		rel := ranked[i].rel
		b, qerr := q.QueueDownload(ctx, rel)
		ct := &tr.Candidates[ranked[i].trace]
		if qerr == nil {
			ct.Rejected, ct.Healthy, ct.err = false, true, nil
			ct.Reason = "cached on the debrid service when sent to download"
			return i, b, nil, true
		}
		if ctx.Err() != nil {
			return -1, nil, ctx.Err(), true
		}
		ct.Reason = qerr.Error()
		if errors.Is(qerr, core.ErrDownloadQueued) {
			out.Err = qerr
			tr.Err = uncachedSummary(out, qerr)
			return -1, nil, out, true
		}
		refused = append(refused, qerr)
		if errors.Is(qerr, core.ErrDownloadLimit) {
			// The account takes no new download at all: another release
			// would only get the same answer.
			break
		}
	}
	out.Err = errors.Join(refused...)
	if !errors.Is(out.Err, core.ErrDownloadRefused) {
		// Nothing was refused by a provider (a transient add failure):
		// still a refusal from the viewer's point of view.
		out.Err = fmt.Errorf("%w: %w", core.ErrDownloadRefused, out.Err)
	}
	tr.Err = uncachedSummary(out, out.Err)
	return -1, nil, out, true
}

// uncachedSummary is the history line for an uncached outcome.
func uncachedSummary(e *core.UncachedError, queued error) string {
	var msg string
	switch {
	case queued != nil && errors.Is(queued, core.ErrDownloadQueued):
		msg = "not cached on the debrid service: " + queued.Error()
	case queued != nil && errors.Is(queued, core.ErrDownloadLimit):
		msg = "not cached, and the debrid account is at its active-download limit: nothing was sent to download"
	case queued != nil:
		msg = "not cached, and the debrid service did not take it to download: " + queued.Error()
	case e.CachedOnly:
		msg = "not cached by any debrid provider (Cached torrents only is on)"
	default:
		msg = "not cached by any debrid provider"
	}
	switch e.Usenet {
	case core.UsenetNone:
		msg += "; Usenet found no release"
	case core.UsenetFailed:
		msg += "; the Usenet releases found failed"
	}
	return msg
}
