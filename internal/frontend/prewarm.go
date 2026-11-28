package frontend

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// PrewarmOptions tune a speculative warm-up of a title before the viewer
// presses Play. The options stay owned by core.
type PrewarmOptions = core.PrewarmOptions

// PrewarmDetail is the warm-up reason for an opened detail page.
const PrewarmDetail = core.PrewarmDetail

// Prewarmer is implemented by playback managers that can resolve, open,
// probe and pre-buffer a title ahead of Play. It reports whether a
// warm-up was scheduled.
type Prewarmer interface {
	Prewarm(ctx context.Context, user *User, key RatingKey, caps ClientCaps, opts PrewarmOptions) bool
}

// WarmProber returns the probed media info a completed warm-up already
// produced for this user, client and title.
type WarmProber interface {
	WarmProbeInfo(user *User, caps ClientCaps, key RatingKey) (MediaInfo, bool)
}

// WarmProbeWaiter waits up to a bounded budget for an already-scheduled
// warm probe to finish.
type WarmProbeWaiter interface {
	WaitWarmProbe(ctx context.Context, user *User, caps ClientCaps, key RatingKey, budget time.Duration) (MediaInfo, bool)
}

// MetadataProber obtains exact pre-play source facts without admitting a
// live session. An error means facts are not ready.
type MetadataProber interface {
	ProbeMetadata(context.Context, *User, RatingKey, ClientCaps) error
}

// PlayMetadataProber joins or starts the shared resolve+probe flight for a
// client's play-time metadata re-ask and waits bounded by ctx.
type PlayMetadataProber interface {
	ProbePlayMetadata(context.Context, *User, RatingKey, ClientCaps) error
}

// VersionHeaderProber runs bounded header probes on unprobed free-source
// offered versions (cached debrid / vaulted Usenet) ahead of play.
type VersionHeaderProber interface {
	ProbeVersionHeaders(context.Context, *User, RatingKey, ClientCaps) error
}

// WithVersionProbeSearchWait attaches a concurrent Auto-probe completion signal
// to ctx for ProbeVersionHeaders.
func WithVersionProbeSearchWait(ctx context.Context, done <-chan struct{}) context.Context {
	return core.WithVersionProbeSearchWait(ctx, done)
}

// DetailRevealer schedules a background season-drop check when a viewer opens
// a show, season, or episode detail page, subject to detail-burst gating.
type DetailRevealer interface {
	RevealDetail(user *User, clientID string, key RatingKey)
}

// WithNextEpisodePreparation identifies a pre-play metadata request for the
// immediate next episode of the queue. Playback checks the preceding session.
func WithNextEpisodePreparation(ctx context.Context, previous RatingKey) context.Context {
	return core.WithNextEpisodePreparation(ctx, previous, nil)
}
