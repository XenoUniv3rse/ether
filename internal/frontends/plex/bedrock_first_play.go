package plex

import (
	"context"
	"net/url"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

const (
	// playIntentProbeWait bounds how long explicit play intent waits on the
	// shared foreground resolve+probe flight so the pre-play queue/metadata
	// row carries real probed tracks and the viewer's selected audio/subtitle
	// track before direct play begins (#618, #670, #758). The playback
	// manager waits only briefly while the source is still being searched,
	// and up to this bound for the probe of a source it found: a queue or
	// metadata request never waits out a slow search.
	playIntentProbeWait = 4 * time.Second
	// playIntentVersionWait bounds the extra wait for header probes on other
	// versions after Auto's probe, on queue and pre-play metadata reads.
	playIntentVersionWait = 1500 * time.Millisecond
	// playIntentActiveQueueWindow bounds how long a client's active PlayQueue
	// classifies a lean pre-play /library/metadata/<id> fetch as play intent
	// for the selected or upcoming queue item.
	playIntentActiveQueueWindow = 30 * time.Minute
	// playIntentSelectedJoinWindow bounds how long a full metadata fetch for
	// the currently selected queue item joins an in-flight play probe right
	// after POST /playQueues without turning later detail-page browsing into a
	// play probe.
	playIntentSelectedJoinWindow = 30 * time.Second
)

type activePrepareObserver interface {
	HasActivePrepare(*frontend.User, frontend.RatingKey, frontend.ClientCaps) bool
}

// probePlayIntent runs or joins the shared foreground resolve+probe flight
// (and concurrent free-source non-Auto version header probes) for an explicit
// play-intent request (POST /playQueues, play-queue re-centering/takeover, or
// a pre-play /library/metadata fetch for an item in the client's active queue).
// It applies to every Plex playback client (excluding library tools) so
// direct-play clients that keep pre-play PlayQueue/metadata tracks (desktop,
// HTPC, Web, Apple TV, Roku, mobile, smart TVs, etc.) receive real probed
// streams and the viewer's rule-selected audio/subtitle tracks on first play
// (#618, #670, #715, #728, #758).
//
// It returns true when handled via PlayMetadataProber (or exact probed tracks
// are already available), and false when the playback backend does not
// implement PlayMetadataProber so callers can fall back to warmDetail.
func (s *Server) probePlayIntent(ctx context.Context, pr *Request, t *frontend.Title) bool {
	if pr == nil || pr.User == nil || t == nil || !isPlayable(t.Type) || t.Key.IsPluginKey() || s.playback() == nil || libraryToolClient(pr) {
		return false
	}
	_, _, exact := s.mediaInfoWithSource(ctx, pr, t)
	prober, ok := s.playback().(frontend.PlayMetadataProber)
	if !exact && !ok {
		return false
	}
	var autoProbe func(context.Context) error
	if !exact {
		caps := s.clientCapsForKey(pr, t.Key)
		autoProbe = func(c context.Context) error {
			return prober.ProbePlayMetadata(c, pr.User, t.Key, caps)
		}
	}
	s.runPlayIntentProbes(ctx, pr, t, exact, playIntentProbeWait, playIntentVersionWait, autoProbe)
	return true
}

// joinActivePrepare waits bounded on an already-running foreground Prepare
// flight for this client and title so a metadata read arriving during playback
// startup receives the probed streams rather than an estimate (#758). It never
// starts a probe or runs version discovery.
func (s *Server) joinActivePrepare(ctx context.Context, pr *Request, t *frontend.Title) bool {
	if pr == nil || pr.User == nil || t == nil || !isPlayable(t.Type) || t.Key.IsPluginKey() || s.playback() == nil || libraryToolClient(pr) {
		return false
	}
	obs, ok := s.playback().(activePrepareObserver)
	if !ok {
		return false
	}
	prober, ok := s.playback().(frontend.PlayMetadataProber)
	if !ok {
		return false
	}
	caps := s.clientCapsForKey(pr, t.Key)
	if !obs.HasActivePrepare(pr.User, t.Key, caps) {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, playIntentProbeWait)
	defer cancel()
	_ = prober.ProbePlayMetadata(probeCtx, pr.User, t.Key, caps)
	return true
}

// playIntentFirstPlay handles a player's pre-play or startup-time
// /library/metadata/<id> fetch:
//   - when a foreground Prepare flight is already running for this client and
//     title, waits bounded for its probe to land;
//   - otherwise, for non-Plezy clients (Plezy has its own detail/play
//     classifier in plezyFirstPlay), probes for the selected item of an active
//     PlayQueue (joining any in-flight play probe started by POST /playQueues).
//
// Ordinary detail browsing returns false immediately without starting a
// foreground probe.
func (s *Server) playIntentFirstPlay(ctx, reqCtx context.Context, pr *Request, t *frontend.Title, q url.Values) bool {
	if pr == nil || pr.User == nil || t == nil || libraryToolClient(pr) || t.Key.IsPluginKey() {
		return false
	}
	if _, _, exact := s.mediaInfoWithSource(ctx, pr, t); exact {
		return false
	}
	if reqCtx == nil {
		reqCtx = ctx
	}
	if s.joinActivePrepare(reqCtx, pr, t) {
		return true
	}
	if plezyClient(pr) || s.queues == nil {
		return false
	}
	detailPage := metadataOptionOn(q.Get("includeRelated")) || metadataOptionOn(q.Get("includeExtras"))
	maxAge := playIntentActiveQueueWindow
	if detailPage {
		maxAge = playIntentSelectedJoinWindow
	}
	role := s.queues.ActiveQueueRole(pr.User.ID, pr.ClientID, t.Key, maxAge)
	// Metadata reads for upcoming queue entries can be client prefetches.
	// They use ordinary detail warming until playback is requested.
	if role != 1 {
		return false
	}
	return s.probePlayIntent(reqCtx, pr, t)
}
