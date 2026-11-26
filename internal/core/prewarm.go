package core

import (
	"context"
	"errors"
	"time"
)

// ErrPrepareSlow indicates that preparation continues after the caller's wait.
var ErrPrepareSlow = errors.New("playback preparation is still running")

// PlaybackPrepareWithin joins a shared source preparation. A positive wait
// permits ErrPrepareSlow while preparation continues; a non-positive wait
// waits for completion or context cancellation under the service deadline.
type PlaybackPrepareWithin interface {
	PrepareWithin(context.Context, time.Duration, *User, RatingKey, ClientCaps, string, bool) (*PlaySession, error)
}

// Reasons for PrewarmOptions.Reason.
const (
	PrewarmDetail      = "detail"      // a detail page was opened
	PrewarmTrailer     = "trailer"     // a trailer or Cinema Trailer for the title is playing
	PrewarmPostPlay    = "postplay"    // the viewer is near the end of something and this is up next
	PrewarmBrowse      = "browse"      // a home/section row is on screen; open the debrid link only
	PrewarmNextEpisode = "nextepisode" // the viewer is nearing the end of an episode; warm the next one
)

// PrewarmOptions tune a speculative warm-up of a title before the viewer
// presses Play.
type PrewarmOptions struct {
	// StartOffset warms a bounded indexed window around a known linear-channel
	// join point. Trusted host hint; never accepted from frontend JSON.
	StartOffset time.Duration `json:"-"`
	// Reason labels the trigger for logs and budgets (Prewarm* constants).
	Reason string `json:"reason"`
	// PrefetchBytes is how much of the opening of the file to pre-buffer at
	// background priority once the release is resolved, opened and probed.
	// Zero uses the implementation's default for the reason.
	PrefetchBytes int64 `json:"prefetchBytes,omitempty"`
	// DuringPlayback allows the warm-up while the viewer already has an
	// active session (a trailer, or the end of a film, is playing).
	DuringPlayback bool `json:"duringPlayback,omitempty"`
	// PlayIntent marks a detail warm-up from a device whose real caps are
	// known. Held for a moment, it may open the one top-ranked add-on
	// stream that its add-on reports debrid-cached (core.WithOnDemandWarm).
	// Up Next and next-episode warm-ups imply it. Never set for browsing.
	PlayIntent bool `json:"-"`
	// MetadataOnly prepares tracks for an opened detail page without
	// prefetching the playback window.
	MetadataOnly bool `json:"-"`
}

// Prewarmer is implemented by playback managers that can resolve, open,
// probe and pre-buffer a title ahead of Play, so the later Prepare for the
// same user, client and title is near-instant. A warm-up never reserves a
// stream or writes resume/history records. It reports whether a warm-up was
// scheduled (false when skipped by budgets or already warm).
type Prewarmer interface {
	Prewarm(ctx context.Context, user *User, key RatingKey, caps ClientCaps, opts PrewarmOptions) bool
}

// WarmProber is optionally implemented by playback managers that complete a
// speculative probe (Prewarm/PreResolve) before Play. WarmProbeInfo returns
// the probed media info a warm-up already produced for this user/client/key,
// so metadata and play-queue renders can list the file's real audio and
// subtitle tracks instead of the single invented audio track of the
// estimate. ok=false when no warm probe has completed (or it has expired).
// It never starts a probe or blocks on one in flight.
//
// It complements ResumedTracks, which only covers a viewer's later opens:
// this covers the first open, once the detail-page warm-up has landed.
type WarmProber interface {
	WarmProbeInfo(user *User, caps ClientCaps, key RatingKey) (MediaInfo, bool)
}

// WarmProbeWaiter is optionally implemented by playback managers that can wait
// up to a bounded budget for an already-scheduled warm probe (PreResolve) to
// finish, so the first detail-page open can render the file's real audio and
// subtitle streams instead of an estimate. It never cancels the background
// warm-up when the budget expires.
type WarmProbeWaiter interface {
	WaitWarmProbe(ctx context.Context, user *User, caps ClientCaps, key RatingKey, budget time.Duration) (MediaInfo, bool)
}

// MetadataProber obtains exact pre-play source facts without admitting a live
// session, subject to owner warm-up settings and a bounded wait. An error
// means facts are not ready; detail pages should render their normal fallback.
type MetadataProber interface {
	ProbeMetadata(context.Context, *User, RatingKey, ClientCaps) error
}

// PlayMetadataProber joins or starts the shared resolve+probe flight for a
// client's play-time metadata re-ask (such as Plezy's first play) and waits
// until the probe lands or the caller's deadline/context expires. Unlike
// MetadataProber (detail browsing), this carries explicit play intent: it
// runs under playback resolution rules, bypasses speculative warm-up gates
// and negative memos, and publishes into the warm cache so the following
// stream request adopts the same resolved source and probe.
type PlayMetadataProber interface {
	ProbePlayMetadata(context.Context, *User, RatingKey, ClientCaps) error
}

// VersionHeaderProber runs bounded header probes on unprobed offered versions
// that can be opened without indexer searches or NZB grabs (already-cached
// debrid torrents, direct HTTP streams, or vaulted Usenet NZBs) so play-time
// metadata reads can return real container tracks for non-Auto picks too.
type VersionHeaderProber interface {
	ProbeVersionHeaders(context.Context, *User, RatingKey, ClientCaps) error
}

type versionProbeSearchWaitKey struct{}

// WithVersionProbeSearchWait attaches a signal channel closed when a concurrent
// play-intent probe (Auto) has finished. ProbeVersionHeaders uses it to observe
// an indexer search started by that concurrent probe instead of exiting before
// searchCached begins, while still returning promptly if Auto finishes without
// running a search.
func WithVersionProbeSearchWait(ctx context.Context, done <-chan struct{}) context.Context {
	if done == nil {
		return ctx
	}
	return context.WithValue(ctx, versionProbeSearchWaitKey{}, done)
}

// VersionProbeSearchWait returns the concurrent Auto-probe completion signal
// attached by WithVersionProbeSearchWait, or nil when none was set.
func VersionProbeSearchWait(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	ch, _ := ctx.Value(versionProbeSearchWaitKey{}).(<-chan struct{})
	return ch
}
