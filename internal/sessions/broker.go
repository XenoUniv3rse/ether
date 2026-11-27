package sessions

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/trickplay"
)

// BrokerDeps are the explicit dependencies shared by all protocol surfaces.
type BrokerDeps struct {
	PreviewAvailable func() bool
	ExtraLookup      func(core.RatingKey) (frontend.Extra, bool)
	ExtraSource      func(context.Context, *core.Viewer, *core.Title, core.RatingKey, string) (PreviewSource, error)
	PreviewSource    func(context.Context, *core.Viewer, *core.Title, string) (PreviewSource, error)
	SurfacePolicy    func(frontend.Protocol) core.SessionPolicy
	// OnActualPlaying runs in a bounded worker after authenticated playing telemetry.
	OnActualPlaying func(context.Context, *Broker, PlayingSubtitleSession)
	// DistributedHLS is opt-in; unsupported plans stay local before reservation.
	DistributedHLS DistributedHLS
	Coordination   *Coordination
	// MaxPendingStarts bounds active or waiting neutral stream starts.
	// Zero selects the default; the broker rejects values above 4096.
	MaxPendingStarts int
	DB               *sql.DB
	Cluster          clusterbus.Node
	SessionHeartbeat time.Duration
	Logger           *slog.Logger
	Downloads        core.DownloadRemuxer
	DecisionTraces   core.DecisionTraceSource
	Live             LiveBackend
	Variants         func(context.Context, *core.Viewer, *core.Title, core.ClientCaps) ([]core.ReleaseVariant, error)
	SelectTracks     func(context.Context, *core.Viewer, *core.Title, core.MediaInfo, core.DecideOptions, bool, bool) (core.DecideOptions, error)
	// SelectScopedTracks receives the authenticated device as well. If present,
	// it replaces SelectTracks and must preserve the unscoped behavior.
	SelectScopedTracks func(context.Context, *core.Viewer, core.Device, *core.Title, core.MediaInfo, core.DecideOptions, bool, bool) (core.DecideOptions, error)
	Lookup             func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Title, error)
	// KindSource, when set, supplies non-catalogue plugin kind items: the
	// projected title and an opener for its bytes. A plugin key in Plan is
	// prepared through KindPlayback so it shares the decision, HLS and
	// stream-limit machinery with catalogue titles (spec §15.1).
	KindSource func(context.Context, *core.Viewer, core.RatingKey) (*core.KindInput, error)
	// KindVersions, when set with KindSource, lists the version menu of a
	// plugin kind item (Auto slot + one entry per /resolve source) so plugin
	// keys get the same version surface as catalogue titles.
	KindVersions func(context.Context, *core.Viewer, core.RatingKey) ([]core.ReleaseVariant, error)
	// KindRememberedRelease returns the release ID of the viewer's remembered
	// version for a plugin kind item ("" for none). A plain start (no
	// version requested) prefers it, as catalogue titles do in the resolver.
	KindRememberedRelease func(userID int64, key core.RatingKey) string
	// CurrentMediaLookup checks already admitted playback using local facts.
	// When nil, MediaLookup retains its historical behavior. MediaLookup checks
	// admission policy, including lazy issuance. Neither lookup may resolve a
	// release or refresh remote metadata. If MediaLookup is nil, Lookup must
	// satisfy that same local-only requirement.
	CurrentMediaLookup func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Title, error)
	MediaLookup        func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Title, error)
	Playback           core.Playback
	Transcoder         core.Transcoder
	Progress           ProgressWriter
	SourceHistory      SourceHistoryWriter
	ActiveViewer       func(context.Context, int64) (*core.Viewer, error)
	CanPlay            func(*core.Viewer, bool, time.Time) bool
	// CurrentPlay rechecks a running item without imposing bedtime.
	CurrentPlay func(context.Context, *core.Viewer, bool, time.Time) (*core.Viewer, error)
	// FreshPlay re-reads fenced policy before admission. ErrPolicyUnconfirmed
	// keeps the cached decision for an unrestricted viewer.
	FreshPlay func(context.Context, *core.Viewer, bool, time.Time) (*core.Viewer, error)
	// PreviewPlay reads policy without fencing when no playback is admitted.
	PreviewPlay func(context.Context, *core.Viewer, bool, time.Time, bool) (*core.Viewer, error)
	// ReceiptCurrent rechecks the acceptance Plex stored for this request.
	ReceiptCurrent func(context.Context, auth.Acceptance) error
	BitrateLimit   func(*core.Viewer) int
	// ApplyRuleLimits applies quality-rule limits (MaxHeight, MaxBitrateKbps,
	// StereoAudio) for viewer and title (title == nil for pre-title / Live TV).
	ApplyRuleLimits     func(ctx context.Context, caps core.ClientCaps, viewer *core.Viewer, title *core.Title) core.ClientCaps
	StreamLimit         func(*core.Viewer) int
	RemoteBitrateLimit  func() int
	RemoteUploadCap     func(ctx context.Context, sessionID string, remote bool, allowTranscoding bool, existingCapKbps int, demandKbps int) (capKbps int, fromBudget bool)
	RecordUploadCap     func(sessionID string, capKbps int, fromBudget bool)
	VideoTranscodingOff func() bool
	// ViewerVideoTranscodingOff is the per-viewer form of VideoTranscodingOff.
	ViewerVideoTranscodingOff func(*core.Viewer) bool
	OnTimeline                func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string)
	Events                    *EventBus
	DomainEvents              *events.Bus
	OnEventError              func(error)
	// OnProfileDiagnostic is an optional fixture hook. Values are closed enums
	// only; the callback receives no media, identity or client-supplied data.
	OnProfileDiagnostic func(ProfileDiagnostic)
	// SourceMarkers returns a complete marker set for the exact source revision.
	SourceMarkers    func(context.Context, *core.Viewer, core.RatingKey, string, core.MediaInfo) ([]frontend.Marker, bool, error)
	OpenSubtitle     func(string, string) (io.ReadCloser, string, error)
	OpenSubtitleFrom func(string, string, string) (io.ReadCloser, string, error)
	ExtractSubtitle  func(context.Context, string, int, string, io.Writer) error
	// Trickplay is borrowed from the runtime owner (internal/trickplay/owner.go).
	// It is the same provider Plex uses. The broker releases its own leases;
	// only the runtime owner may Close the provider.
	Trickplay core.TrickplayProvider
	Now       func() time.Time
}

// versionAdmission holds the limits and delivery options before source-specific admission.
type versionAdmission struct {
	caps core.ClientCaps
	opts core.DecideOptions
}

type brokerPlan struct {
	versionAdmission versionAdmission
	request          frontend.PlanRequest
	source           *core.PlaySession
	decision         core.Decision
	created          time.Time
	// requested is when the play request that made this plan arrived.
	requested     time.Time
	etag          string
	live          bool
	exactRelease  string
	versionHandle string
	opening       bool
	pinned        bool
	// autoRelease: exactRelease is the client's Auto entry, which may fail
	// over after admission like any automatic session.
	autoRelease bool
	// external is a Plex admission: the adapter already prepared the source.
	external      bool
	budgetCapKbps int
	baseMaxHeight int
	clientCap     clientCapPolicy
}

// clientCapPolicy tracks Vela's own streaming cap (client.maxBitrate, together
// with maxWidth/maxHeight) alongside owner caps so only the client's cap can
// be lifted for an explicit original choice or last-resort fallback.
type clientCapPolicy struct {
	clientKbps  int
	ownerKbps   int
	ownerHeight int
}
type brokerSession struct {
	versionAdmission versionAdmission
	// queued is a prefetched next item (PlayIntent.Queued) not yet observed
	// playing: it supersedes nothing until activateQueued.
	queued bool
	// outgoing is the item a queued successor took over from, still playing
	// out (a crossfade) for at most OutgoingHandover: its reads keep serving,
	// it no longer owns the device, and its reports write nothing.
	outgoing bool
	// qualityCapped records that a quality pick turned off copying, so a
	// later Auto pick turns it back on.
	qualityCapped          bool
	observedSourceRevision string
	audio                  *audioRendition
	audioGeneration        uint64

	manualSubtitle        bool
	autoSubtitleIdentity  *int
	autoSubtitleQueued    bool
	autoSubtitleEpoch     uint64
	autoSubtitleCancel    context.CancelFunc
	distributed, restored bool
	distributedSaved      time.Time
	trickplay             *trickplay.Holder
	trickplayStarted      bool
	public                frontend.Session
	grant                 string
	playID                string
	clientID              string
	playbackID            string
	caps                  core.ClientCaps
	opts                  core.DecideOptions
	decision              core.Decision
	allowTranscoding      *bool
	format                frontend.StreamFormat
	httpExt               string
	streamPosition        time.Duration
	segmentDuration       time.Duration
	live                  bool
	directLive            *frontend.DirectLiveTarget
	multiviewGroup        string
	multiviewTile         int
	baseMaxHeight         int
	clientCap             clientCapPolicy
	knownVideoHeight      int
	sequence              uint64
	historyEpoch          int64
	historySequence       int64
	versionHandle         string
	exactRelease          string
	autoRelease           bool
	sourcePinned          bool
	sourceETag            string

	originalFailure, originalReplaced bool
	originalAwaitingProgress          bool
	originalSubstituted               bool
	originalPosition                  *time.Duration
	originalResumeAt                  time.Duration
	originalAudio, originalSubtitle   *int

	// transferBarrier is this session's admission ticket. A later open of the
	// same title, part and device reuses it. Stop drops it once no live
	// session still holds it; an unverified retirement leaves it until the
	// next admission evicts that idle ticket instead of refusing the viewer.
	// lazyHolds protect a source until its backend retirement succeeds.
	transferBarrier *capability.Header
	lazyHolds       []capability.Header
	// servingReads is one tracker per in-flight media request. Stop and
	// supersede wait until those producers release; the signal does not
	// itself free a source or capability claim.
	servingReads []*core.ReadRetirement
	// deliveryToken is this session's admission order. A later session can
	// replace the durable authority; an older token cannot.
	deliveryToken      int64
	deliveryGeneration int64
	deliverySession    string
}

type retiredSession struct {
	viewerID int64
	protocol frontend.Protocol
	deviceID string
	item     core.RatingKey
	until    time.Time
	// grant and replacedAt are set only when a newer playback on the same
	// device superseded the session; see replacedMedia.
	grant      string
	replacedAt time.Time
}

// Broker coordinates playback admission for every protocol, including Plex.
type Broker struct {
	// subtitleWarm holds the session tracks whose extraction warmSubtitle
	// started and has not finished.
	subtitleWarm     sync.Map
	timelineReserved int
	timelineEffects  []timelineEffect
	timelineRunning  bool
	timelineCancel   context.CancelFunc
	timelineDone     chan struct{}

	previews            map[string]previewSession
	previewPending      map[int64]bool
	previewPreparations map[int64]*previewPreparation
	previewWG           sync.WaitGroup
	autoSubtitleWG      sync.WaitGroup
	autoSubtitleRunning int
	sampleLimit         int
	deps                BrokerDeps
	startSlots          chan struct{}
	shutdownTimeout     time.Duration
	mu                  sync.Mutex
	plans               map[string]brokerPlan
	sessions            map[string]*brokerSession
	multiviewGroups     map[string]*multiviewGroup
	nonGroupEpoch       map[string]uint64
	adapterActivities   map[string]*PlaybackActivity
	adapterActivityNext int
	retired             map[string]retiredSession
	lazySessions        map[string]lazySession
	lazyCleanup         map[string]lazyCleanupEntry
	lazyCleanupRunning  bool
	lazyCleanupWait     time.Duration
	lazyCleanupReserved int
	lazyHeartbeatCount  int
	lazyTransfers       map[string]map[*lazyTransfer]context.CancelFunc
	lazyCleanupCancel   context.CancelFunc
	lazyCleanupDone     chan struct{}
	devices             map[deviceKey]frontend.SessionCapabilities
	playCapsByDevice    map[deviceKey]rememberedPlayCaps
	owners              *PlaybackOwners
	gates               *Lifecycle
	bindings            *BindingTable[frontend.PlayIntent]
	closed              bool
	pendingPlans        int
	pendingByViewer     map[int64]int
	sourceLeases        map[string]*sourceLease
	planSweepStop       chan struct{}
	planSweepDone       chan struct{}
	planSweepEvery      time.Duration
	detailPlans         map[string]*detailFlight
	cluster             *brokerCluster
	adapterStreams      *StreamTable
	historyQueue        chan sourceHistorySample
	historyCancel       context.CancelFunc
	historyDone         chan struct{}
	holdMu              sync.Mutex
	holds               map[string]*capabilityHold
}

func NewBroker(d BrokerDeps) (*Broker, error) {
	if d.Lookup == nil || d.Playback == nil || d.ActiveViewer == nil || d.Events == nil || d.CanPlay == nil || d.BitrateLimit == nil || d.Progress == nil || d.OnTimeline == nil {
		return nil, errors.New("sessions: missing required dependency")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	coord := d.Coordination
	if coord == nil {
		coord = NewCoordination()
	}
	if coord.Owners == nil || coord.Gates == nil {
		return nil, errors.New("sessions: invalid coordination")
	}
	limit := d.MaxPendingStarts
	if limit <= 0 {
		limit = 128
	}
	if limit > 4096 {
		return nil, errors.New("sessions: max pending starts exceeds 4096")
	}
	b := &Broker{sampleLimit: 100000, deps: d, startSlots: make(chan struct{}, limit), shutdownTimeout: 10 * time.Second, lazyCleanupWait: 5 * time.Second, planSweepEvery: 5 * time.Second, plans: make(map[string]brokerPlan), pendingByViewer: make(map[int64]int), sourceLeases: make(map[string]*sourceLease), sessions: make(map[string]*brokerSession), adapterActivities: make(map[string]*PlaybackActivity), retired: make(map[string]retiredSession), devices: make(map[deviceKey]frontend.SessionCapabilities), playCapsByDevice: make(map[deviceKey]rememberedPlayCaps), owners: coord.Owners, gates: coord.Gates, bindings: NewBindingTable[frontend.PlayIntent](), adapterStreams: NewStreamTable()}
	if d, ok := b.deps.DistributedHLS.(interface {
		BindDelivery(func(http.ResponseWriter, *http.Request, string)) error
	}); ok {
		if err := d.BindDelivery(func(w http.ResponseWriter, r *http.Request, p string) { b.ServeMediaFor(frontend.Protocol(p), w, r) }); err != nil {
			return nil, err
		}
	}
	if d.DB != nil && d.Cluster != nil {
		var err error
		b.cluster, err = startBrokerCluster(b, d.DB, d.Cluster)
		if err != nil {
			return nil, err
		}
	}

	if d.SourceHistory != nil {
		ctx, cancel := context.WithCancel(context.Background())
		b.historyCancel = cancel
		b.historyQueue = make(chan sourceHistorySample, 256)
		b.historyDone = make(chan struct{})
		go func() { defer close(b.historyDone); b.runSourceHistory(ctx) }()
	}

	if remote, ok := b.deps.DistributedHLS.(neutralControl); ok {
		if err := remote.BindNeutralControl(b.neutralControl); err != nil {
			return nil, err
		}
	}
	return b, nil
}
func (b *Broker) viewer(ctx context.Context, v *core.Viewer) (*core.Viewer, error) {
	if v == nil || v.ID <= 0 {
		return nil, core.ErrUnauthorized
	}
	active, err := b.deps.ActiveViewer(ctx, v.ID)
	if err != nil || active == nil || active.ID != v.ID {
		return nil, core.ErrUnauthorized
	}
	return active, nil
}
func (b *Broker) permitted(v *core.Viewer, caps core.ClientCaps) bool {
	return b.deps.CanPlay == nil || b.deps.CanPlay(v, caps.Remote, b.deps.Now())
}

// confirm applies a fenced playback policy when one is installed. The returned
// viewer is the one later lookups must use.
func (b *Broker) confirm(ctx context.Context, v *core.Viewer, caps core.ClientCaps) (*core.Viewer, error) {
	return b.confirmPolicy(ctx, v, caps, b.deps.FreshPlay)
}

func (b *Broker) confirmPreview(ctx context.Context, v *core.Viewer, caps core.ClientCaps, admission bool) (*core.Viewer, error) {
	if b.deps.PreviewPlay == nil {
		if admission {
			return b.confirm(ctx, v, caps)
		}
		return b.confirmCurrent(ctx, v, caps)
	}
	return b.confirmPolicy(ctx, v, caps, func(ctx context.Context, u *core.Viewer, remote bool, at time.Time) (*core.Viewer, error) {
		return b.deps.PreviewPlay(ctx, u, remote, at, admission)
	})
}

// Current item checks deliberately exclude bedtime: an admitted item can finish.
func (b *Broker) confirmCurrent(ctx context.Context, v *core.Viewer, caps core.ClientCaps) (*core.Viewer, error) {
	fresh := b.deps.CurrentPlay
	if fresh == nil {
		fresh = b.deps.FreshPlay
	}
	return b.confirmPolicy(ctx, v, caps, fresh)
}
func (b *Broker) confirmPolicy(ctx context.Context, v *core.Viewer, caps core.ClientCaps, freshPlay func(context.Context, *core.Viewer, bool, time.Time) (*core.Viewer, error)) (*core.Viewer, error) {
	if v == nil || v.ID <= 0 {
		return nil, core.ErrUnauthorized
	}
	at := time.Now()
	if b.deps.Now != nil {
		at = b.deps.Now()
	}
	if freshPlay != nil {
		fresh, err := freshPlay(ctx, v, caps.Remote, at)
		if err == nil && fresh != nil && fresh.ID == v.ID {
			return fresh, nil
		}
		if err != nil && !errors.Is(err, core.ErrPolicyUnconfirmed) {
			if errors.Is(err, core.ErrUnauthorized) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, core.ErrUnauthorized
		}
	}
	if !b.permitted(v, caps) {
		return nil, core.ErrUnauthorized
	}
	return v, nil
}

// ConfirmAdmission is the fenced policy check a Plex plan uses.
// planPlexAdmission already calls confirm with the request caps. This is
// the same check when the caller has only the remote bit.
func (b *Broker) ConfirmAdmission(ctx context.Context, v *core.Viewer, remote bool) (*core.Viewer, error) {
	caps := core.ClientCaps{Remote: remote}
	confirmed, err := b.confirm(ctx, v, caps)
	if err != nil {
		b.emitAdmissionDenied(v, 0, "", caps, core.Device{}, "plex", false, err)
	}
	return confirmed, err
}

func (b *Broker) publish(ctx context.Context, event frontend.Event) {
	if err := b.deps.Events.Publish(ctx, event); err != nil && b.deps.OnEventError != nil {
		b.deps.OnEventError(err)
	}
}
func clientKey(protocol frontend.Protocol, device core.Device) string {
	return PlaybackClientID(protocol, device.ID)
}

// PlaybackClientID is the broker's device identity on the playback surface.
func PlaybackClientID(protocol frontend.Protocol, deviceID string) string {
	return fmt.Sprintf("%d:%s%s", len(protocol), protocol, deviceID)
}
func copyRequest(r frontend.PlanRequest) frontend.PlanRequest {
	if r.Intent.OriginalPlayback != nil {
		original := *r.Intent.OriginalPlayback
		original.DemuxContainers = slices.Clone(original.DemuxContainers)
		original.VideoDecoders = slices.Clone(original.VideoDecoders)
		original.AudioDecoders = slices.Clone(original.AudioDecoders)
		r.Intent.OriginalPlayback = &original
	}

	if r.Intent.AudioOrdinal != nil {
		ordinal := *r.Intent.AudioOrdinal
		r.Intent.AudioOrdinal = &ordinal
	}
	if r.Intent.SubtitleOrdinal != nil {
		index := *r.Intent.SubtitleOrdinal
		r.Intent.SubtitleOrdinal = &index
	}
	if r.Intent.AllowTranscoding != nil {
		allowed := *r.Intent.AllowTranscoding
		r.Intent.AllowTranscoding = &allowed
	}
	if r.DirectLive != nil {
		d := *r.DirectLive
		d.Headers = maps.Clone(d.Headers)
		r.DirectLive = &d
	}
	if r.Multiview != nil {
		mv := *r.Multiview
		r.Multiview = &mv
	}
	r.Caps.VideoCodecs = slices.Clone(r.Caps.VideoCodecs)
	r.Caps.AudioCodecs = slices.Clone(r.Caps.AudioCodecs)
	r.Caps.TranscodeAudioCodecs = slices.Clone(r.Caps.TranscodeAudioCodecs)
	r.Caps.Containers = slices.Clone(r.Caps.Containers)
	r.Caps.HLSContainers = slices.Clone(r.Caps.HLSContainers)
	r.Caps.SubtitleCodecs = slices.Clone(r.Caps.SubtitleCodecs)
	r.Caps.HDRFormats = slices.Clone(r.Caps.HDRFormats)
	r.Caps.DVProfiles = slices.Clone(r.Caps.DVProfiles)
	r.Caps.DVProfileVariants = slices.Clone(r.Caps.DVProfileVariants)
	r.Caps.VideoLevels = maps.Clone(r.Caps.VideoLevels)
	r.Caps.VideoMaxHeights = maps.Clone(r.Caps.VideoMaxHeights)
	r.Caps.VideoProfiles = maps.Clone(r.Caps.VideoProfiles)
	for k, v := range r.Caps.VideoProfiles {
		r.Caps.VideoProfiles[k] = slices.Clone(v)
	}
	r.Caps.Profiles = cloneClientProfiles(r.Caps.Profiles)
	r.Intent.Options.AudioFilters = slices.Clone(r.Intent.Options.AudioFilters)
	if r.Intent.Hints != nil {
		p := *r.Intent.Hints
		if p.SubtitleStream != nil {
			s := copyStream(*p.SubtitleStream)
			p.SubtitleStream = &s
		}
		r.Intent.Hints = &p
	}
	return r
}
func copyStream(stream core.StreamInfo) core.StreamInfo {
	for _, field := range []**bool{&stream.DefaultDisposition, &stream.ForcedDisposition, &stream.HearingImpaired, &stream.Original, &stream.VisualImpaired, &stream.Commentary, &stream.Dub, &stream.HDR10Plus, &stream.DolbyVisionBLPresent, &stream.DolbyVisionELPresent, &stream.DolbyVisionRPUPresent, &stream.ProbedInterlaced, &stream.DolbyVisionConfigurationPresent, &stream.DolbyVisionBLCompatibilityIDPresent} {
		if *field != nil {
			value := **field
			*field = &value
		}
	}
	for _, field := range []**int{&stream.NALLengthSize, &stream.ReferenceFrames, &stream.BitsPerRawSample, &stream.ProbedLevel, &stream.ProbedDolbyVisionProfile, &stream.ProbedDolbyVisionLevel} {
		if *field != nil {
			value := **field
			*field = &value
		}
	}
	for _, field := range []**float64{&stream.AverageFrameRate, &stream.ReferenceFrameRate} {
		if *field != nil {
			value := **field
			*field = &value
		}
	}
	return stream
}
func publicMedia(info core.MediaInfo) core.MediaInfo {
	info.Streams = slices.Clone(info.Streams)
	for i := range info.Streams {
		info.Streams[i] = copyStream(info.Streams[i])
		info.Streams[i].ExternalURL = ""
	}
	info.Chapters = slices.Clone(info.Chapters)
	info.Keyframes = slices.Clone(info.Keyframes)
	return info
}
func (b *Broker) Plan(ctx context.Context, req frontend.PlanRequest) (planned frontend.PlayPlan, planErr error) {
	requested := b.deps.Now()
	req = copyRequest(req)
	v, err := b.viewer(ctx, req.Viewer)
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	targets := 0
	if req.Item != 0 {
		targets++
	}
	if req.Channel != "" {
		targets++
	}
	if req.DirectLive != nil {
		targets++
	}
	if req.Protocol == "" || req.Device.ID == "" || targets != 1 {
		return frontend.PlayPlan{}, core.ErrUnauthorized
	}
	if req.Protocol == "silo" && req.Intent.ClientSessionID != "" && !canonicalUUID(req.Intent.ClientSessionID) {
		return frontend.PlayPlan{}, core.ErrUnauthorized
	}
	if b.policy(req.Protocol).ExternalAdmission {
		if admit, ok := frontend.PlanAdmissionFrom(ctx); ok {
			return b.planPlexAdmission(ctx, req, admit)
		}
	}
	// Plex-specific version and stream identifiers remain on the existing Plex
	// path until its adapter can translate them into neutral selections.
	if req.Intent.Hints != nil {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	confirm := b.confirm
	if req.Intent.Estimate {
		confirm = func(ctx context.Context, u *core.Viewer, caps core.ClientCaps) (*core.Viewer, error) {
			return b.confirmPreview(ctx, u, caps, true)
		}
	}
	confirmed, err := confirm(ctx, v, req.Caps)
	if err != nil {
		if !req.Intent.Estimate {
			b.emitAdmissionDenied(v, req.Item, "", req.Caps, req.Device, string(req.Protocol), false, err)
		}
		return frontend.PlayPlan{}, err
	}
	v = confirmed
	viewerCopy := *v
	req.Viewer = &viewerCopy
	if req.Intent.DeclaredCaps && req.Item != 0 {
		b.rememberPlayCaps(v, req.Protocol, req.Device, req.Item, req.Caps)
	}
	// clientCap tracks Vela's own streaming cap (client.maxBitrate, together
	// with maxWidth/maxHeight) alongside every owner cap so only the client's
	// streaming cap can be lifted for an explicit original choice or fallback.
	var clientCap clientCapPolicy
	if req.Protocol == "vela" {
		if req.Item != 0 {
			clientCap.clientKbps = req.Intent.Options.MaxBitrateKbps
			if clientCap.clientKbps > 0 {
				if c := req.Caps.MaxBitrateKbps; c > 0 && c < clientCap.clientKbps {
					clientCap.ownerKbps = c
				}
				if req.Caps.MaxHeight > 0 && req.Caps.MaxHeightSource != "" {
					clientCap.ownerHeight = req.Caps.MaxHeight
				}
			}
		}
		// Never let an owner quality rule with a higher ceiling raise Caps
		// above Vela's declared maxHeight or maxBitrate (including live starts,
		// which read Caps directly).
		if h := req.Intent.Options.MaxHeight; h > 0 && (req.Caps.MaxHeight == 0 || h < req.Caps.MaxHeight) {
			req.Caps.MaxHeight = h
		}
		if k := req.Intent.Options.MaxBitrateKbps; k > 0 && (req.Caps.MaxBitrateKbps == 0 || k < req.Caps.MaxBitrateKbps) {
			req.Caps.MaxBitrateKbps = k
		}
	}
	req.Caps = b.playCaps(v, req.Protocol, req.Device, req.Caps)
	req.Caps = b.applyRuleLimits(ctx, req.Caps, &req.Intent.Options, v, nil)
	if b.deps.BitrateLimit != nil {
		limit := b.deps.BitrateLimit(v)
		if limit > 0 && (req.Intent.Options.MaxBitrateKbps == 0 || limit < req.Intent.Options.MaxBitrateKbps) {
			req.Intent.Options.MaxBitrateKbps = limit
		}
		clientCap.ownerKbps = stricterLimit(clientCap.ownerKbps, limit)
	}
	if req.Caps.Remote && b.deps.RemoteBitrateLimit != nil {
		clientCap.ownerKbps = stricterLimit(clientCap.ownerKbps, b.deps.RemoteBitrateLimit())
	}
	budgetCapKbps := 0
	if req.Caps.Remote && !req.Intent.Estimate && b.deps.RemoteUploadCap != nil {
		allowTC := (req.Channel != "" || req.DirectLive != nil || !b.videoOff(req.Caps)) &&
			(req.Intent.AllowTranscoding == nil || *req.Intent.AllowTranscoding)
		existing := req.Caps.MaxBitrateKbps
		if req.Intent.Options.MaxBitrateKbps > 0 && (existing == 0 || req.Intent.Options.MaxBitrateKbps < existing) {
			existing = req.Intent.Options.MaxBitrateKbps
		}
		if capKbps, fromBudget := b.deps.RemoteUploadCap(ctx, req.Intent.ClientSessionID, req.Caps.Remote, allowTC, existing, 0); fromBudget && capKbps > 0 {
			budgetCapKbps = capKbps
			clientCap.ownerKbps = stricterLimit(clientCap.ownerKbps, capKbps)
			if req.Caps.MaxBitrateKbps == 0 || capKbps < req.Caps.MaxBitrateKbps {
				req.Caps.MaxBitrateKbps = capKbps
			}
			if req.Intent.Options.MaxBitrateKbps == 0 || capKbps < req.Intent.Options.MaxBitrateKbps {
				req.Intent.Options.MaxBitrateKbps = capKbps
			}
		}
	}
	if req.Multiview != nil && req.Channel == "" {
		return frontend.PlayPlan{}, frontend.ErrInvalidMultiview
	}
	if req.Channel != "" || req.DirectLive != nil {
		if req.Version != "" || req.Intent.ExactVersion || req.ExpectedSourceETag != "" || req.DetailSource != "" {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		return b.planLive(ctx, req, v)
	}
	if req.Item.IsPluginKey() && b.deps.KindSource != nil {
		// Detail sources and source ETags are catalogue-only; a rejected
		// release ETag has no plugin analogue. Version selection (the /resolve
		// per-source menu) is honoured in planKind.
		if req.ExpectedSourceETag != "" || req.DetailSource != "" {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		return b.planKind(core.WithLiveApp(ctx, string(req.Protocol)), req, v, clientCap)
	}
	title, err := b.deps.Lookup(core.WithLiveApp(ctx, string(req.Protocol)), v, req.Device, req.Item)
	if err != nil {
		if errors.Is(err, core.ErrUnauthorized) {
			b.emitAdmissionDenied(v, req.Item, "", req.Caps, req.Device, string(req.Protocol), true, err)
		}
		return frontend.PlayPlan{}, err
	}
	if title == nil || title.Key != req.Item {
		return frontend.PlayPlan{}, core.ErrNotFound
	}
	if !title.Released.IsZero() && title.Released.After(b.deps.Now()) {
		b.emitAdmissionDenied(v, req.Item, title.Title, req.Caps, req.Device, string(req.Protocol), false, core.ErrUnreleased)
		return frontend.PlayPlan{}, core.ErrUnreleased
	}
	b.applyTitleRuleLimits(ctx, &req, &clientCap, v, title)
	var detail *detailSourceRecord
	if req.DetailSource != "" {
		if req.Version != "" || req.Intent.ExactVersion {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		record, err := b.resolveDetailSource(ctx, frontend.VersionScope{Viewer: v, Device: req.Device, Protocol: req.Protocol, Caps: req.Caps}, req.DetailSource)
		if err != nil {
			return frontend.PlayPlan{}, err
		}
		if record.item != req.Item || (req.ExpectedSourceETag != "" && req.ExpectedSourceETag != record.revision) {
			return frontend.PlayPlan{}, frontend.ErrSourceGone
		}
		req.ExpectedSourceETag = record.revision
		detail = &record
	}
	if req.Intent.Estimate && (req.Intent.AudioOrdinal != nil || req.Intent.SubtitleOrdinal != nil) {
		return frontend.PlayPlan{}, frontend.ErrTrackSourceUnavailable
	}
	if req.Intent.Estimate && req.Intent.ExactVersion {
		info := b.deps.Playback.Estimate(ctx, title, req.Caps)
		dec := core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: !info.AudioOnly(), AudioCopy: true, Container: info.Container}
		if b.deps.Transcoder != nil {
			dec = b.deps.Transcoder.Decide(info, req.Caps, req.Intent.Options)
		}
		return frontend.PlayPlan{Estimated: true, Media: publicMedia(info), Decision: dec}, nil
	}
	versionHandle := ""
	var chosen *core.ReleaseVariant
	if req.Intent.ExactVersion && req.ResolvedRelease != "" {
		if req.Version == "" || b.deps.DB == nil || req.ResolvedRelease == "" || len(req.ResolvedRelease) > 512 {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		h := &versionHandles{db: b.deps.DB, broker: b, protocol: req.Protocol}
		item, release, err := h.lookupRow(ctx, frontend.VersionScope{Viewer: v, Device: req.Device, Protocol: req.Protocol, Caps: req.Caps}, req.Version)
		if err != nil {
			return frontend.PlayPlan{}, err
		}
		if item != req.Item || release != req.ResolvedRelease {
			return frontend.PlayPlan{}, core.ErrNotFound
		}
		versionHandle, req.Version = normalizeVersionHandle(req.Version), release
		chosen = &core.ReleaseVariant{ID: release}
	} else if req.Intent.ExactVersion {
		if req.Version == "" || b.deps.DB == nil {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		h := &versionHandles{db: b.deps.DB, broker: b, protocol: req.Protocol}
		scope := frontend.VersionScope{Viewer: v, Device: req.Device, Protocol: req.Protocol, Caps: req.Caps}
		item, release, err := h.resolveRecord(ctx, scope, req.Version)
		if errors.Is(err, core.ErrNotFound) {
			// An id names one release. When this viewer is no longer offered
			// it (the release went away), Vela rejects with ErrVersionUnavailable
			// rather than silently substituting Auto; legacy surfaces fall back.
			if parent, _, lerr := h.lookupRow(ctx, scope, req.Version); lerr == nil && parent == req.Item {
				if req.Protocol == "vela" {
					return frontend.PlayPlan{}, core.ErrVersionUnavailable
				}
				item, release, err = parent, "", nil
				req.Intent.ExactVersion = false
			}
		}
		if err != nil {
			return frontend.PlayPlan{}, err
		}
		if item != req.Item {
			return frontend.PlayPlan{}, core.ErrNotFound
		}
		versionHandle, req.Version = normalizeVersionHandle(req.Version), release
	}
	if chosen == nil && req.Version != "" {
		if b.deps.Variants == nil {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		variants, err := b.deps.Variants(ctx, v, title, req.Caps)
		if err != nil {
			return frontend.PlayPlan{}, err
		}
		for j := range variants {
			if variants[j].ID == req.Version || variants[j].Release.ID == req.Version {
				chosen = &variants[j]
				break
			}
		}
		if chosen == nil && versionHandle != "" && len(variants) > 0 {
			chosen = &core.ReleaseVariant{ID: req.Version, Auto: true, Info: variants[0].Info}
		}
		if chosen != nil && chosen.Availability == core.VersionUnavailable && req.Protocol == "vela" && !core.IsExplicitVersionSlotSelector(chosen.ID) {
			return frontend.PlayPlan{}, core.ErrVersionUnavailable
		}
		if chosen == nil {
			return frontend.PlayPlan{}, core.ErrNotFound
		}
	}
	if req.Intent.Estimate {
		var info core.MediaInfo
		if chosen != nil {
			info = chosen.Info
		} else {
			info = b.deps.Playback.Estimate(ctx, title, req.Caps)
		}
		dec := core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: !info.AudioOnly(), AudioCopy: true, Container: info.Container}
		if b.deps.Transcoder != nil {
			dec = b.deps.Transcoder.Decide(info, req.Caps, req.Intent.Options)
		}
		return frontend.PlayPlan{Estimated: true, Media: publicMedia(info), Decision: dec}, nil
	}
	if !b.reservePlan(v.ID) {
		return frontend.PlayPlan{}, core.ErrStreamLimit
	}
	defer b.releasePlanReservation(v.ID)
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", v.ID, req.Caps.ClientID))
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	defer unlock()
	playback, ok := b.deps.Playback.(core.SurfaceScopedPlayback)
	if !ok {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	release := ""
	prepareExact := req.Intent.ExactVersion
	slotSelector := false
	if chosen != nil {
		if req.Protocol == "vela" && chosen.Auto {
			prepareExact = false
			req.Intent.ExactVersion = false
		} else {
			release = chosen.ID
		}
	}
	// A configured version slot (tier/alternative/distinct) is a logical
	// selector, not a single concrete release: let playback fail over across
	// matching copies and step down to lower tiers instead of failing closed
	// on the first dead release or jumping back up to Auto.
	if core.IsExplicitVersionSlotSelector(release) {
		slotSelector = true
		prepareExact = false
	}
	rememberedFallback := false
	if remembered, rememberErr := b.rememberedDeliveryRelease(ctx, v.ID, string(req.Protocol), req.Device.ID, req.Item, req.Intent.ClientSessionID); rememberErr != nil {
		return frontend.PlayPlan{}, rememberErr
	} else if remembered != "" && release == "" && !prepareExact {
		release, prepareExact, rememberedFallback = remembered, true, true
	}
	previous, existed := playback.SessionForSurface(string(req.Protocol), v.ID, req.Caps.ClientID, req.Item, release, prepareExact)
	if detail != nil {
		if cached := b.localDetailSource(v.ID, req.Device.ID, req.Protocol, req.Item, *detail); cached != nil {
			previous, existed = cached, true
		}
	}
	useCachedDetail := detail != nil && existed && previous != nil && sourceRevision(previous) == detail.revision && previous.Resolution != nil && previous.Resolution.Release.ID == detail.release
	if detail != nil && !useCachedDetail {
		release, prepareExact, rememberedFallback = detail.release, true, false
		previous, existed = playback.SessionForSurface(string(req.Protocol), v.ID, req.Caps.ClientID, req.Item, release, prepareExact)
	}
	if req.ExpectedSourceETag != "" && detail == nil && (!existed || previous == nil || sourceRevision(previous) != req.ExpectedSourceETag) {
		return frontend.PlayPlan{}, frontend.ErrSourceGone
	}
	if !req.Intent.Queued && !req.Intent.AudioSet && !req.Intent.SubtitleSet &&
		req.Intent.AudioOrdinal == nil && req.Intent.SubtitleOrdinal == nil && detail == nil &&
		req.ExpectedSourceETag == "" && (req.Intent.AllowTranscoding == nil || *req.Intent.AllowTranscoding) {
		ctx = b.withSourceDecision(ctx, req)
	}
	var ps *core.PlaySession
	// autoRelease: the handle names Auto's release, so the session may play
	// another release in its place (a fallback now, a failover later).
	autoRelease := ((chosen != nil && chosen.Auto) || req.Intent.AutoVersion) && !slotSelector && detail == nil
	prepStarted := time.Now()
	if useCachedDetail {
		if liveness, ok := b.deps.Playback.(interface{ SessionActive(string) bool }); ok && !liveness.SessionActive(previous.ID) {
			ps, err = playback.PrepareSurface(ctx, v, req.Item, req.Caps, release, prepareExact)
		} else {
			ps = previous
		}
	} else {
		ps, err = playback.PrepareSurface(ctx, v, req.Item, req.Caps, release, prepareExact)
		// If the remembered release (or a non-detail MediaSourceId handle when
		// no ExpectedSourceETag was pinned) is no longer available or changed,
		// fall back cleanly to the next healthy candidate instead of failing
		// replay with "not available yet: playback: source revision changed".
		if err != nil && detail == nil && req.ExpectedSourceETag == "" && !slotSelector && (rememberedFallback || autoRelease || errors.Is(err, core.ErrSourceChanged)) {
			if fallbackPS, fallbackErr := playback.PrepareSurface(ctx, v, req.Item, req.Caps, "", false); fallbackErr == nil && fallbackPS != nil {
				ps, err = fallbackPS, nil
				prepareExact = false
				req.Intent.ExactVersion = false
				if ps.Resolution != nil && ps.Resolution.Release.ID != "" {
					req.Version = ps.Resolution.Release.ID
					release = ps.Resolution.Release.ID
				}
			} else if errors.Is(fallbackErr, core.ErrVideoTranscodingDisabled) || errors.As(fallbackErr, new(*core.RequiredAudioLanguageError)) {
				err = fallbackErr
			}
		}
	}
	if os.Getenv("ETHER_TRACE_PLAYBACK") != "" {
		slog.Info("playback trace: broker.Plan PrepareSurface",
			"item", req.Item,
			"protocol", req.Protocol,
			"release_req", release,
			"exact", prepareExact,
			"cached_detail", useCachedDetail,
			"existed", existed,
			"prepare_ms", time.Since(prepStarted).Milliseconds(),
			"err", err,
		)
	}
	if err != nil {
		if detail != nil && (errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrSourceChanged)) {
			return frontend.PlayPlan{}, frontend.ErrSourceGone
		}
		return frontend.PlayPlan{}, err
	}
	if ps == nil {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	// An exact start is the viewer's pick only when they chose a version.
	// The Auto entry a client was handed (Silo's default file, Jellyfin's
	// transcode-avoiding Auto switch) and a remembered resume open their
	// release exactly, but must still fail over when it cannot be read.
	if prepareExact && detail == nil {
		binding := core.SourceBindingExplicit
		if rememberedFallback {
			binding = core.SourceBindingRemembered
		} else if autoRelease {
			binding = core.SourceBindingAuto
		}
		if binder, ok := b.deps.Playback.(core.SourceBinder); ok {
			binder.BindSource(ps.ID, binding)
		}
	}
	// Keep the selected source identifiable when delivery negotiation fails.
	// In particular, an owner pin must explain why it cannot be delivered
	// without concealing which release was kept instead of substituting one.
	defer func() {
		var noPath *frontend.NoCompatiblePathError
		if errors.As(planErr, &noPath) && ps.Resolution != nil && ps.Resolution.Release.ID != "" {
			planErr = fmt.Errorf("release %s: %w", ps.Resolution.Release.ID, planErr)
		}
	}()
	if req.Intent.ExactVersion && !slotSelector && !ps.Resolution.MatchesVersion(req.Version) {
		if !existed || previous == nil || previous.ID != ps.ID {
			b.deps.Playback.Stop(ps.ID)
		}
		if req.Protocol == "vela" {
			return frontend.PlayPlan{}, core.ErrVersionUnavailable
		}
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	if detail != nil && (ps.Resolution == nil || ps.Resolution.Release.ID != detail.release) {
		if !existed || previous == nil || previous.ID != ps.ID {
			b.deps.Playback.Stop(ps.ID)
		}
		return frontend.PlayPlan{}, frontend.ErrSourceGone
	}
	existed = existed && previous != nil && previous.ID == ps.ID
	b.acquirePlanSource(ps.ID, !existed)
	stored := false
	pinned := false
	defer func() {
		if !stored {
			b.releasePlanSource(ps.ID, pinned)
		}
	}()
	admission := versionAdmission{caps: req.Caps, opts: req.Intent.Options}
	var pinErr error
	ps, pinned, pinErr = b.pinRequestedTracks(ctx, &req, ps)
	if pinErr != nil {
		return frontend.PlayPlan{}, pinErr
	}
	if err := bindSourceTracks(&req.Intent, ps.Info); err != nil {
		return frontend.PlayPlan{}, err
	}
	if b.deps.SelectScopedTracks != nil || b.deps.SelectTracks != nil {
		var selected core.DecideOptions
		var err error
		if b.deps.SelectScopedTracks != nil {
			selected, err = b.deps.SelectScopedTracks(ctx, v, req.Device, title, ps.Info, req.Intent.Options, req.Intent.AudioSet, req.Intent.SubtitleSet)
		} else {
			selected, err = b.deps.SelectTracks(ctx, v, title, ps.Info, req.Intent.Options, req.Intent.AudioSet, req.Intent.SubtitleSet)
		}
		if err != nil {
			return frontend.PlayPlan{}, err
		}
		req.Intent.Options = selected
	}
	sourceChosenAt := b.deps.Now()
	dec, originalChoice, err := b.admitWithClientCap(ps, &req, clientCap, b.admitPlanDecision)
	decisionAt := b.deps.Now()
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	ps, pinned, pinErr = b.pinOriginalRuntime(ctx, &req, ps, dec, pinned)
	if pinErr != nil {
		return frontend.PlayPlan{}, pinErr
	}
	id := rand.Text()
	etag := sourceRevision(ps)
	if req.Intent.SubtitleSet || (req.Intent.Hints != nil && req.Intent.Hints.ManualSubtitle) {
		if err := b.RecordAuthorizedSubtitleIntent(ctx, v.ID, req.Item, true); err != nil {
			return frontend.PlayPlan{}, err
		}
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	exactRelease := ""
	// The selected logical slot was validated above. Pin its admitted
	// concrete release for Open and all later source-revision checks.
	if (req.Intent.ExactVersion || versionHandle != "") && ps.Resolution != nil && ps.Resolution.Release.ID != "" {
		exactRelease = ps.Resolution.Release.ID
	}
	if detail != nil {
		exactRelease = detail.release
	}
	b.plans[id] = brokerPlan{versionAdmission: admission, request: req, source: ps, decision: dec, created: b.deps.Now(), requested: requested, etag: etag, exactRelease: exactRelease, autoRelease: autoRelease, versionHandle: versionHandle, pinned: pinned, budgetCapKbps: budgetCapKbps, clientCap: clientCap}
	if budgetCapKbps > 0 && b.deps.RecordUploadCap != nil && ps != nil && ps.ID != "" {
		constrained := ps.Info.BitrateKbps <= 0 || ps.Info.BitrateKbps > budgetCapKbps || dec.Kind == core.DecisionTranscode
		b.deps.RecordUploadCap(ps.ID, budgetCapKbps, constrained)
	}
	stored = true
	b.mu.Unlock()
	direct, remux, transcoding := b.planAlternatives(ps, req)
	hls, hlsContainer := b.hlsPlanOutput(ps, req, dec)
	httpOK, httpContainer := b.httpPlanOutput(ps, req, dec)
	relID := ""
	if ps.Resolution != nil {
		relID = ps.Resolution.Release.ID
	}
	return frontend.PlayPlan{SourceChosenAt: sourceChosenAt, DecisionAt: decisionAt, ID: id, Media: publicMedia(ps.Info), Decision: dec, SelectedSubtitle: sideSubtitleStream(ps, req, dec), DirectPlay: direct, DirectStream: remux, Transcoding: transcoding, VideoTranscodingOff: req.Channel == "" && b.videoOff(req.Caps), HLS: hls, HLSContainer: hlsContainer, HTTP: httpOK, HTTPContainer: httpContainer, ETag: etag, VersionHandle: versionHandle, ReleaseID: relID, OriginalOverClientCap: originalChoice}, nil
}
func (b *Broker) Open(ctx context.Context, req frontend.OpenRequest) (frontend.Session, error) {
	opened := b.deps.Now()
	v, err := b.viewer(ctx, req.Viewer)
	if err != nil {
		return frontend.Session{}, err
	}
	b.mu.Lock()
	p, ok := b.plans[req.PlanID]
	if b.closed {
		ok = false
	}
	if ok && (p.opening || p.request.Viewer.ID != v.ID || p.request.Protocol != req.Protocol || req.Device.ID == "" || p.request.Device.ID != req.Device.ID) {
		ok = false
	}
	var openEpoch uint64
	if ok {
		p.opening = true
		b.plans[req.PlanID] = p
		openEpoch = b.nonGroupEpoch[ownerGateKey(v.ID, p.request.Caps.ClientID)]
	}
	b.mu.Unlock()
	if !ok {
		return frontend.Session{}, core.ErrUnauthorized
	}
	gateKey := ownerGateKey(v.ID, p.request.Caps.ClientID)
	if p.live && p.request.Multiview != nil && p.request.Multiview.GroupID != "" {
		gateKey = tileGateKey(v.ID, p.request.Caps.ClientID, p.request.Multiview.GroupID, p.request.Multiview.Tile)
	}
	gateCtx, cancelGate := context.WithTimeout(ctx, 10*time.Second)
	unlock, err := b.gates.Acquire(gateCtx, gateKey)
	cancelGate()
	if err != nil {
		// Keep the scoped reference for the bounded sweeper; it must acquire
		// the owner gate before releasing a potentially shared source.
		b.mu.Lock()
		if current, exists := b.plans[req.PlanID]; exists && current.opening {
			current.opening = false
			current.created = b.deps.Now().Add(-planLifetime - time.Nanosecond)
			b.plans[req.PlanID] = current
		}
		b.mu.Unlock()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return frontend.Session{}, &frontend.SessionBusyError{RetryAfter: time.Second}
		}
		return frontend.Session{}, err
	}
	defer unlock()
	b.mu.Lock()
	current, exists := b.plans[req.PlanID]
	if exists && current.opening {
		delete(b.plans, req.PlanID)
	}
	b.mu.Unlock()
	if !exists || !current.opening {
		return frontend.Session{}, core.ErrNotAvailable
	}
	transferred := false
	defer func() {
		if !transferred && p.source != nil {
			b.releasePlanSource(p.source.ID, p.pinned)
		}
	}()
	if b.deps.Now().Sub(p.created) > planLifetime {
		return frontend.Session{}, core.ErrUnauthorized
	}
	v, err = b.viewer(ctx, req.Viewer)
	if err != nil {
		return frontend.Session{}, core.ErrUnauthorized
	}
	v, err = b.confirm(ctx, v, p.request.Caps)
	if err != nil {
		return frontend.Session{}, err
	}
	if limit := b.deps.BitrateLimit(v); limit > 0 && (p.request.Caps.MaxBitrateKbps == 0 || p.request.Caps.MaxBitrateKbps > limit) {
		return frontend.Session{}, core.ErrUnauthorized
	}
	if p.request.Caps.Remote && b.deps.RemoteBitrateLimit != nil {
		if limit := b.deps.RemoteBitrateLimit(); limit > 0 && (p.request.Caps.MaxBitrateKbps == 0 || p.request.Caps.MaxBitrateKbps > limit) {
			return frontend.Session{}, core.ErrUnauthorized
		}
	}
	if b.deps.ApplyRuleLimits != nil {
		probe := p.request.Caps
		probe.MaxBitrateKbps = 0
		if limit := b.deps.ApplyRuleLimits(ctx, probe, v, nil).MaxBitrateKbps; limit > 0 && (p.request.Caps.MaxBitrateKbps == 0 || p.request.Caps.MaxBitrateKbps > limit) {
			return frontend.Session{}, core.ErrUnauthorized
		}
	}
	if p.live {
		return b.openLive(ctx, req, p, v, openEpoch)
	}
	var title *core.Title
	if p.request.Item.IsPluginKey() && b.deps.KindSource != nil {
		if in, kerr := b.deps.KindSource(core.WithLiveApp(ctx, string(p.request.Protocol)), v, p.request.Item); kerr == nil && in != nil {
			title = in.Title
		}
	} else {
		var lerr error
		title, lerr = b.deps.Lookup(core.WithLiveApp(ctx, string(p.request.Protocol)), v, req.Device, p.request.Item)
		if lerr != nil {
			return frontend.Session{}, lerr
		}
	}
	if title == nil || title.Key != p.request.Item || (!title.Released.IsZero() && title.Released.After(b.deps.Now())) {
		return frontend.Session{}, core.ErrNotFound
	}
	if remote, ok := b.deps.Playback.(interface {
		RevalidateSession(context.Context, string) error
	}); ok {
		if err := remote.RevalidateSession(ctx, p.source.ID); err != nil {
			return frontend.Session{}, err
		}
	}
	ps, ok := b.deps.Playback.Session(p.source.ID)
	if !ok || ps == nil || ps.UserID != v.ID || ps.ClientID != p.request.Caps.ClientID || ps.Key != p.request.Item {
		return frontend.Session{}, core.ErrNotAvailable
	}
	if p.exactRelease != "" && (ps.Resolution == nil || ps.Resolution.Release.ID != p.exactRelease) {
		return frontend.Session{}, core.ErrNotAvailable
	}
	// A plan's tag must not describe a source that failed over before Open.
	// Ask the frontend to re-plan instead of handing it a stale revision.
	if p.etag != "" && sourceRevision(ps) != p.etag {
		if p.pinned {
			return frontend.Session{}, frontend.ErrSourceGone
		}
		return frontend.Session{}, core.ErrNotAvailable
	}
	if reused, ok := b.reuseDirectFileSession(v, req, p, ps); ok {
		// The deferred release drops the plan's own source reference; the
		// kept session holds its own.
		return reused, nil
	}
	if remote, ok := b.deps.Playback.(interface {
		OpenRemoteSource(context.Context, string) error
	}); ok {
		if err := remote.OpenRemoteSource(ctx, p.source.ID); err != nil {
			return frontend.Session{}, err
		}
	}
	id, err := sessionID(req.Protocol)
	if err != nil {
		return frontend.Session{}, err
	}
	if binding, ok := b.deps.Playback.(interface {
		BindClientSession(context.Context, string, string) error
	}); ok {
		if err := binding.BindClientSession(ctx, id, ps.ID); err != nil {
			return frontend.Session{}, err
		}
	} else if b.policy(req.Protocol).DistributedBindings {
		if binding, ok := b.deps.DistributedHLS.(interface {
			BindLocalClientSession(context.Context, string, *core.PlaySession) error
		}); ok {
			if err := binding.BindLocalClientSession(ctx, id, ps); err != nil {
				return frontend.Session{}, err
			}
		}
	}
	if b.policy(req.Protocol).ExternalAdmission && p.external {
		if !validPlexTranscodeID(p.request.Intent.ClientSessionID) {
			return frontend.Session{}, core.ErrNotAvailable
		}
		id = p.request.Intent.ClientSessionID
		if reused, ok, reuseErr := b.reusePlexSession(ctx, v, req, p, ps, id); ok || reuseErr != nil {
			if ok {
				transferred = true
			}
			return reused, reuseErr
		}
	}
	grant := rand.Text()
	playbackID := p.request.Intent.PlaybackID
	if playbackID == "" && !b.policy(req.Protocol).ExternalAdmission {
		playbackID = id
	}
	session := frontend.Session{ID: id, MediaPath: "/sessions/media/" + id + "/" + grant + "/file", Protocol: req.Protocol, ViewerID: v.ID, Device: p.request.Device, Item: p.request.Item, StartedAt: b.deps.Now(), PlayMethod: playMethod(p.decision.Kind)}
	if req.Protocol == "silo" {
		session.ClientSessionID = p.request.Intent.ClientSessionID
	}
	if err := b.enrichCapabilities(ctx, &session); err != nil {
		return frontend.Session{}, err
	}
	deliverySession := p.request.Intent.ClientSessionID
	if deliverySession == "" {
		deliverySession = id
	}
	s := &brokerSession{versionAdmission: p.versionAdmission, autoSubtitleIdentity: new(int), manualSubtitle: p.request.Intent.SubtitleSet || (p.request.Intent.Hints != nil && p.request.Intent.Hints.ManualSubtitle), public: copySession(session), grant: grant, playID: ps.ID, clientID: p.request.Caps.ClientID, playbackID: playbackID, caps: p.request.Caps, opts: p.request.Intent.Options, decision: p.decision, allowTranscoding: p.request.Intent.AllowTranscoding, versionHandle: p.versionHandle, exactRelease: p.exactRelease, autoRelease: p.autoRelease, sourcePinned: p.pinned, sourceETag: p.etag, clientCap: p.clientCap, deliverySession: deliverySession}
	barrier, barrierErr := b.admitSessionBarrier(ctx, v.ID, string(req.Protocol), p.request.Device.ID, p.request.Item, ps.ID, id)
	if barrierErr != nil {
		if errors.Is(barrierErr, errTransferCommitUnknown) {
			// The durable row may exist. Keep the prepared source; do not Stop it.
			transferred = true
		}
		return frontend.Session{}, barrierErr
	}
	s.transferBarrier = barrier
	if b.cluster != nil {
		if err := b.cluster.store.put(ctx, session, ps.ID, *v, b.deps.Now()); err != nil {
			return frontend.Session{}, err
		}
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		if b.cluster != nil {
			_ = b.cluster.store.remove(ctx, session.ID)
		}
		if s.transferBarrier != nil {
			transferred = true
		}
		return frontend.Session{}, core.ErrNotAvailable
	}
	if !p.external {
		if scoped, ok := b.deps.Playback.(core.SurfaceScopedPlayback); !ok || !scoped.MarkSurfaceOpened(ps.ID) {
			b.mu.Unlock()
			if b.cluster != nil {
				_ = b.cluster.store.remove(ctx, session.ID)
			}
			return frontend.Session{}, core.ErrNotAvailable
		}
	} else if scoped, ok := b.deps.Playback.(core.SurfaceScopedPlayback); ok && !scoped.MarkSurfaceOpened(ps.ID) {
		b.mu.Unlock()
		if b.cluster != nil {
			_ = b.cluster.store.remove(ctx, session.ID)
		}
		if s.transferBarrier != nil {
			transferred = true
		}
		return frontend.Session{}, core.ErrNotAvailable
	}
	b.bindings.Put(&Binding[frontend.PlayIntent]{ID: id, UserID: v.ID, ClientID: s.clientID, Key: session.Item, PlayID: ps.ID, PlaybackID: s.playbackID, Requested: p.request.Intent, Decision: p.decision, Info: ps.Info, Caps: s.caps, Opts: s.opts, UpdatedAt: b.deps.Now()})
	// A queued next item opens beside the playing session; it takes over the
	// device (ownership, supersession) only once it is observed playing.
	s.queued = p.request.Intent.Queued && !p.live && p.request.Multiview == nil
	var superseded []*brokerSession
	if s.queued {
		superseded = b.collectStaleQueuedLocked(s)
	} else {
		b.owners.BindFor("broker", v.ID, s.clientID, session.Item, s.playbackID, ps.ID, true, b.deps.Now())
		superseded = b.collectSupersededLocked(s)
	}
	b.sessions[id] = s
	if !s.queued {
		b.bumpNonGroupEpochLocked(v.ID, s.clientID)
	}
	b.transferPlanSourceLocked(ps.ID)
	transferred = true
	b.mu.Unlock()
	if noter, ok := b.deps.Playback.(interface{ NotePlayRequest(string, time.Time) }); ok {
		// The play request is the plan's (Jellyfin PlaybackInfo plans and
		// opens in one request); a plan made earlier counts from this open.
		at := p.requested
		if at.IsZero() {
			at = opened
		}
		noter.NotePlayRequest(ps.ID, at)
	}
	if noter, ok := b.deps.Playback.(interface{ NoteStartPosition(string, time.Duration) }); ok {
		noter.NoteStartPosition(ps.ID, p.request.Intent.Position)
	}
	if b.deps.SourceHistory != nil && !b.policy(session.Protocol).ExternalAdmission {
		// The epoch and the opened-file facts are durable bookkeeping. The
		// session URL does not wait for that write. Start after the session
		// is visible so the writer can find the row it updates.
		go b.publishOpenedSource(s)
	}
	b.finishSuperseded(ctx, superseded, ps.ID)
	if b.cluster != nil {
		b.cluster.ownershipChanged(ctx)
	}
	b.publish(ctx, sessionEvent("SessionStarted", session, nil))
	return session, nil
}
func (b *Broker) access(ctx context.Context, a frontend.SessionAccess, id string) (*brokerSession, *core.Viewer, error) {
	return b.accessSession(ctx, a, id, true)
}

func (b *Broker) accessSession(ctx context.Context, a frontend.SessionAccess, id string, enforcePlaybackPolicy bool) (*brokerSession, *core.Viewer, error) {
	return b.accessSessionBinding(ctx, a, id, enforcePlaybackPolicy, true)
}

func (b *Broker) accessSessionBinding(ctx context.Context, a frontend.SessionAccess, id string, enforcePlaybackPolicy, restore bool) (*brokerSession, *core.Viewer, error) {
	viewer, err := b.viewer(ctx, a.Viewer)
	if err != nil {
		return nil, nil, err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, nil, core.ErrNotAvailable
	}
	s, ok := b.sessions[id]
	if ok {
		copy := *s
		s = &copy
	}
	b.mu.Unlock()
	if !ok {
		if !restore {
			return nil, nil, core.ErrNotFound
		}
		if err := b.restoreNeutralBinding(ctx, id, &a, "", string(a.Protocol)); err != nil {
			return nil, nil, err
		}
		b.mu.Lock()
		s = b.sessions[id]
		if s != nil {
			copy := *s
			s = &copy
		}
		b.mu.Unlock()
		if s == nil {
			return nil, nil, core.ErrNotFound
		}
	}
	if !b.neutralCurrent(ctx, s) {
		return nil, nil, core.ErrNotAvailable
	}
	owner := a.Owner && viewer.IsOwner && !viewer.Delegated
	if !owner && (s.public.ViewerID != viewer.ID || s.public.Protocol != a.Protocol || a.Device.ID == "" || s.public.Device.ID != a.Device.ID) {
		return nil, nil, core.ErrUnauthorized
	}
	if a.Owner && !owner {
		return nil, nil, core.ErrUnauthorized
	}
	if enforcePlaybackPolicy && !owner {
		confirmed, err := b.confirmCurrent(ctx, viewer, s.caps)
		if err != nil {
			return nil, nil, err
		}
		viewer = confirmed
	}
	return s, viewer, nil
}
func (b *Broker) Events(ctx context.Context, scope frontend.EventScope) (<-chan frontend.Event, error) {
	return b.deps.Events.Subscribe(ctx, scope)
}
func (b *Broker) Recover(ctx context.Context, id, url string, at time.Duration) error {
	return Recover(ctx, b.bindings, b.deps.Playback, id, url, at)
}

// HasBinding reports whether this broker owns an encoder ID.
func (b *Broker) HasBinding(id string) bool { _, ok := b.bindings.Get(id); return ok }

// PlayIDFor maps a public or encoder session ID to its underlying playback session ID.
func (b *Broker) PlayIDFor(id string) string {
	if b == nil || id == "" {
		return ""
	}
	if binding, ok := b.bindings.Get(id); ok {
		return binding.PlayID
	}
	return ""
}
func (b *Broker) TranscodeFor(_ context.Context, id string) (core.HLSSession, bool) {
	// Neutral frontends use the public session ID, which is also the encoder
	// ID. Do not interpret it as a playback source ID: those are distinct.
	if b.HasBinding(id) {
		return b.transcodeSession(id)
	}
	for _, encoderID := range b.bindings.ByPlayID(id) {
		if hs, ok := b.transcodeSession(encoderID); ok && hs != nil {
			return hs, true
		}
	}
	return nil, false
}
func (b *Broker) Sessions(ctx context.Context, a frontend.SessionAccess) ([]frontend.Session, error) {
	v, err := b.viewer(ctx, a.Viewer)
	if err != nil {
		return nil, err
	}
	owner := a.Owner && v.IsOwner && !v.Delegated
	if a.Owner && !owner {
		return nil, core.ErrUnauthorized
	}
	if err = b.restoreNeutralScope(ctx, a); err != nil {
		return nil, err
	}
	b.mu.Lock()
	livenessState, checkState := b.deps.Playback.(interface {
		SessionLiveness(string) core.SessionLivenessState
	})
	visChecker, checkVisible := b.deps.Playback.(interface{ SessionVisible(string) bool })
	liveness, checkLiveness := b.deps.Playback.(interface{ SessionActive(string) bool })
	var out []frontend.Session
	isolated := map[string]bool{}
	b.prunePreviewsLocked()
	for _, s := range b.previews {
		if owner || s.public.ViewerID == v.ID {
			out = append(out, s.public)
			isolated[s.public.ID] = true
		}
	}
	for _, s := range b.sessions {
		if !s.live && s.playID != "" {
			if a.VisibleOnly {
				if checkVisible {
					if !visChecker.SessionVisible(s.playID) {
						continue
					}
				} else if checkLiveness && !liveness.SessionActive(s.playID) {
					continue
				}
			} else if checkState {
				// Keep freshly opened Starting sessions reachable for follow-up
				// stream/subtitle/control lookups right after PlaybackInfo, while
				// excluding Idle and Stopped sessions.
				st := livenessState.SessionLiveness(s.playID)
				if st != core.SessionLivenessStarting && st != core.SessionLivenessPlaying && st != core.SessionLivenessPaused {
					continue
				}
			} else if checkVisible {
				if !visChecker.SessionVisible(s.playID) {
					continue
				}
			} else if checkLiveness && !liveness.SessionActive(s.playID) {
				continue
			}
		}
		if owner || s.public.ViewerID == v.ID {
			pub := s.public
			if !s.live && s.format == "" {
				// A session whose stream has not been started yet is not
				// serving /file; keep alias callers on StartStream.
				pub.MediaPath = ""
			}
			out = append(out, pub)
		}
	}
	b.mu.Unlock()
	if b.cluster != nil {
		remote, err := b.cluster.remote(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range remote {
			if owner || r.session.ViewerID == v.ID {
				out = append(out, r.session)
			}
		}
	}
	visible := out[:0]
	for i := range out {
		if !isolated[out[i].ID] && out[i].Purpose != "preview" && b.cluster != nil && !b.policy(out[i].Protocol).ExternalAdmission {
			active, err := b.cluster.store.current(ctx, out[i].ID)
			if err != nil {
				return nil, err
			}
			if !active {
				continue
			}
		}
		if err := b.enrichCapabilities(ctx, &out[i]); err != nil {
			return nil, err
		}
		visible = append(visible, out[i])
	}
	return visible, nil
}
func (b *Broker) Report(ctx context.Context, a frontend.SessionAccess, id string, t frontend.Timeline) error {
	if handled, err := b.previewControl(ctx, a, id, t.State == "stopped"); handled {
		return err
	}
	if _, handled, err := b.forwardNeutralControl(ctx, a, id, "report", t, frontend.StreamRequest{}); handled {
		return err
	}
	s, _, err := b.accessSession(ctx, a, id, t.State != "stopped")
	if errors.Is(err, core.ErrNotFound) {
		item, retired, retiredErr := b.retiredAccess(ctx, a, id)
		if retiredErr != nil {
			return retiredErr
		}
		if retired {
			// Static playback opens a second session and retires the one
			// PlaybackInfo minted. Infuse keeps reporting that first id.
			// A bare stop still does not rewind a resume point.
			if item != 0 && b.policy(a.Protocol).DistributedBindings && (t.Position > 0 || t.Duration > 0) {
				if err := b.recordItemTimeline(ctx, a, id, item, t); err != nil {
					return err
				}
			}
			return nil
		}
	}
	if errors.Is(err, core.ErrNotFound) && b.cluster != nil {
		return b.cluster.forwardReport(ctx, a, id, t)
	}
	if err != nil {
		return err
	}
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", s.public.ViewerID, s.clientID))
	if err != nil {
		return err
	}
	defer unlock()
	// A queued next item observed playing takes over the device now, under
	// the same owner gate an open holds.
	if s.queued && (t.State == "playing" || t.State == "paused" || t.State == "buffering") {
		b.activateQueued(ctx, id)
	}
	s, v, err := b.accessSession(ctx, a, id, t.State != "stopped")
	if err != nil {
		return err
	}
	if v.ID != s.public.ViewerID {
		v, err = b.deps.ActiveViewer(ctx, s.public.ViewerID)
		if err != nil || v == nil {
			return core.ErrUnauthorized
		}
	}
	// A revoked session may still stop and release resources, but cannot write
	// further progress after its title or channel becomes inaccessible.
	if !b.currentMediaPolicy(ctx, v, s) {
		if t.State == "stopped" {
			return b.stopLocked(ctx, s)
		}
		return core.ErrUnauthorized
	}
	if s.outgoing {
		// Playing out after its successor took over: already recorded as
		// played; its stop ends it, other reports change nothing.
		if t.State == "stopped" {
			b.retireOutgoing(ctx, s.public.ID, "")
		}
		return nil
	}
	if b.cluster != nil && !b.policy(s.public.Protocol).ExternalAdmission {
		active, err := b.cluster.store.active(ctx, s.public.ID)
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
	}
	report, sequenced := ctx.Value(sampleReportKey{}).(*sampleReport)
	accepted, rollbackSequence := true, func() {}
	if !sequenced {
		accepted, rollbackSequence, err = b.acceptSequence(ctx, s, t.Sequence)
	}
	if err != nil {
		return err
	}
	if !accepted {
		return nil
	}
	if err := b.enrichCapabilities(ctx, &s.public); err != nil && b.deps.OnEventError != nil {
		b.deps.OnEventError(err)
	}
	if (s.originalFailure || s.originalAwaitingProgress) && t.State == "stopped" {
		return b.stopLocked(ctx, s)
	}
	if !s.live {
		reportedID := t.PlaybackID
		if reportedID == s.public.ID {
			reportedID = s.playbackID
		}
		if b.owners.StaleFor("broker", s.public.ViewerID, s.clientID, s.public.Item, reportedID) {
			rollbackSequence()
			if b.owners.StaleFor("broker", s.public.ViewerID, s.clientID, s.public.Item, s.playbackID) {
				return b.stopLocked(ctx, s)
			}
			return nil
		}
	}
	if s.live {
		if s.public.Protocol == "vela" && (t.State == "playing" || t.State == "paused" || t.State == "buffering") {
			if live, ok := b.deps.Live.(interface {
				TouchLiveSession(context.Context, *core.Viewer, string) error
			}); ok {
				if err := live.TouchLiveSession(core.WithLiveApp(ctx, string(s.public.Protocol)), v, s.playID); err != nil {
					rollbackSequence()
					return err
				}
			}
		}
		b.publish(ctx, sessionEvent("SessionUpdated", s.public, &t))
		if t.State == "stopped" {
			return b.stopLocked(ctx, s)
		}
		return nil
	}
	if err := b.observeSubtitleSelection(ctx, s, t); err != nil && !errors.Is(err, core.ErrSourceChanged) {
		rollbackSequence()
		return err
	}
	if source, ok := b.deps.Playback.Session(s.playID); ok && source != nil && source.Info.Duration > 0 {
		if t.Duration <= 0 || !source.Info.Estimated {
			t.Duration = source.Info.Duration
		}
	}
	// Keep active broker ownership alive when Plex's shared v39 history
	// expires idle entries. Never refresh a superseded binding.
	if !b.owners.StaleFor("broker", s.public.ViewerID, s.clientID, s.public.Item, s.playbackID) {
		b.owners.BindFor("broker", s.public.ViewerID, s.clientID, s.public.Item, s.playbackID, s.playID, false, b.deps.Now())
	}
	originSession := s.public.ID
	if b.policy(s.public.Protocol).ExternalAdmission && s.playbackID != "" {
		originSession = s.playbackID
	}
	ctx = state.WithOrigin(ctx, state.Origin{Surface: s.public.Protocol, SessionID: originSession, ClientID: s.public.Device.ID})
	var notify func()
	reserved := false
	if s.public.Protocol == "vela" {
		b.mu.Lock()
		if b.closed || b.timelineReserved >= 256 {
			b.mu.Unlock()
			rollbackSequence()
			return errors.New("sessions: timeline effects capacity reached")
		}
		b.timelineReserved++
		b.mu.Unlock()
		reserved = true
		defer func() {
			if reserved {
				b.mu.Lock()
				b.timelineReserved--
				b.mu.Unlock()
			}
		}()
	}
	if sequenced {
		err = b.deps.Progress.(receiptProgress).ProgressWithReceipt(ctx, s.public.ViewerID, s.public.Item, t.Position, t.Duration, t.State, b.sampleReceipt(ctx, s, report))
	} else if progress, ok := b.deps.Progress.(interface {
		AcceptProgress(context.Context, int64, core.RatingKey, time.Duration, time.Duration, string) (func(), error)
	}); ok && s.public.Protocol == "vela" && s.public.Item.Kind() != core.KindExtra {
		notify, err = progress.AcceptProgress(ctx, s.public.ViewerID, s.public.Item, t.Position, t.Duration, t.State)
	} else {
		err = RecordProgress(ctx, b.deps.Progress, s.public.ViewerID, s.public.Item, t.Position, t.Duration, t.State)
	}
	if err != nil {
		rollbackSequence()
		if sequenced && errors.Is(err, errSampleNotApplied) && report.outcome == frontend.ReportReplayed && report.sample.Stop {
			return b.stopLocked(ctx, s)
		}
		return err
	}
	if sequenced {
		report.outcome = frontend.ReportApplied
	}
	if t.State == "playing" || t.State == "paused" || t.State == "buffering" {
		b.mu.Lock()
		if live := b.sessions[id]; live != nil {
			position := t.Position
			live.originalPosition = &position
			if t.State == "playing" {
				live.originalAwaitingProgress = false
			}
			if t.AudioStreamIndex != nil {
				index := *t.AudioStreamIndex
				live.originalAudio = &index
			}
			if t.SubtitleStreamIndex != nil {
				index := *t.SubtitleStreamIndex
				live.originalSubtitle = &index
			}
		}
		b.mu.Unlock()
	}

	if s.format != "" {
		b.recordSourceHistory(ctx, s)
	}
	if toucher, ok := b.deps.Playback.(interface{ Touch(string) }); ok && (t.State == "playing" || t.State == "paused") {
		toucher.Touch(s.playID)
	}
	if s.public.Protocol != "vela" {
		b.applyTimelineEffects(ctx, s, v, t)
	} else {
		b.enqueueTimelineEffect(s, v, t, notify)
		reserved = false
	}
	if t.State == "stopped" {
		return b.stopLocked(ctx, s)
	}
	if err = b.saveNeutralBinding(ctx, id, true); err != nil {
		return err
	}
	if s.public.Protocol != "vela" {
		b.ensureTrickplay(ctx, s, t.Position)
		if t.State == "playing" {
			b.enqueueSubtitleWork(s)
		}
	}
	return nil
}

// recordItemTimeline writes a Jellyfin or Emby watch update without stopping
// the playback that replaced this session.
func (b *Broker) recordItemTimeline(ctx context.Context, a frontend.SessionAccess, sessionID string, item core.RatingKey, t frontend.Timeline) error {
	viewer, err := b.viewer(ctx, a.Viewer)
	if err != nil || viewer == nil {
		return core.ErrUnauthorized
	}
	origin := t.PlaybackID
	if origin == "" {
		origin = sessionID
	}
	ctx = state.WithOrigin(ctx, state.Origin{Surface: a.Protocol, SessionID: origin, ClientID: a.Device.ID})
	if err := RecordProgress(ctx, b.deps.Progress, viewer.ID, item, t.Position, t.Duration, t.State); err != nil {
		return err
	}
	if progress, ok := b.deps.Playback.(core.PlaybackTimeline); ok {
		progress.PlaybackProgress(viewer.ID, clientKey(a.Protocol, a.Device), item, t.Position, t.State)
	}
	if b.deps.OnTimeline != nil {
		b.deps.OnTimeline(viewer, item, t.Position, t.Duration, t.State)
	}
	return nil
}

// ReportItemProgress records a Jellyfin or Emby timeline that names an item
// instead of a broker session.
// keepItemSessionsActive is what Report does for a session's own timeline:
// the open sessions of this viewer, device and title stay active, and their
// output learns the player is playing or paused. Jellyfin Web reports
// progress without a PlaySessionId, so its reports arrive here; without this
// a player that buffered ahead (or paused) for longer than the idle timeout
// had its stream closed as idle, and its next segment request got a 404.
func (b *Broker) keepItemSessionsActive(viewerID int64, a frontend.SessionAccess, item core.RatingKey, state string) {
	b.mu.Lock()
	var active []*brokerSession
	for _, s := range b.sessions {
		if s.public.ViewerID == viewerID && s.public.Protocol == a.Protocol && s.public.Device.ID == a.Device.ID && s.public.Item == item {
			active = append(active, s)
		}
	}
	b.mu.Unlock()
	toucher, _ := b.deps.Playback.(interface{ Touch(string) })
	for _, s := range active {
		if toucher != nil {
			toucher.Touch(s.playID)
		}
		NotifyTranscodeActivity(b.deps.Transcoder, s.public.ID, state)
	}
}

func (b *Broker) ReportItemProgress(ctx context.Context, a frontend.SessionAccess, item core.RatingKey, t frontend.Timeline) error {
	if !b.policy(a.Protocol).DistributedBindings {
		return core.ErrUnauthorized
	}
	viewer, err := b.viewer(ctx, a.Viewer)
	if err != nil || a.Device.ID == "" || item == 0 {
		return core.ErrUnauthorized
	}
	lookup := b.deps.MediaLookup
	if lookup == nil {
		lookup = b.deps.Lookup
	}
	if lookup == nil {
		return core.ErrNotAvailable
	}
	title, err := lookup(core.WithLiveApp(ctx, string(a.Protocol)), viewer, a.Device, item)
	if err != nil || title == nil || title.Key != item {
		if err != nil {
			return err
		}
		return core.ErrNotFound
	}
	// No session names this report, so its duration is the client's. One far
	// shorter than the title's runtime is a trailer, theme or preview the
	// client attributed to the title: it neither writes the title's progress
	// nor stops the title's own playback.
	if frontend.ForeignMediaDuration(title.Runtime, t.Duration) {
		return nil
	}
	// Emby and Jellyfin can send item-only reports without a runtime.
	// Apply the resume and completion thresholds on the first report too.
	if t.Duration <= 0 {
		t.Duration = title.Runtime
	}
	ctx = state.WithOrigin(ctx, state.Origin{Surface: a.Protocol, SessionID: t.PlaybackID, ClientID: a.Device.ID})
	if err := RecordProgress(ctx, b.deps.Progress, viewer.ID, item, t.Position, t.Duration, t.State); err != nil {
		return err
	}
	if progress, ok := b.deps.Playback.(core.PlaybackTimeline); ok {
		progress.PlaybackProgress(viewer.ID, clientKey(a.Protocol, a.Device), item, t.Position, t.State)
	}
	if b.deps.OnTimeline != nil {
		b.deps.OnTimeline(viewer, item, t.Position, t.Duration, t.State)
	}
	if t.State == "playing" || t.State == "paused" {
		b.keepItemSessionsActive(viewer.ID, a, item, t.State)
	}
	if t.State == "stopped" {
		clientID := clientKey(a.Protocol, a.Device)
		unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", viewer.ID, clientID))
		if err != nil {
			return err
		}
		defer unlock()
		b.mu.Lock()
		var toStop []*brokerSession
		for _, s := range b.sessions {
			if s.public.ViewerID == viewer.ID && s.public.Protocol == a.Protocol && s.public.Device.ID == a.Device.ID && s.public.Item == item {
				toStop = append(toStop, s)
			}
		}
		b.mu.Unlock()
		for _, s := range toStop {
			if err := b.stopLocked(ctx, s); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReportProgress records a Plex timeline that has no playback source.
// It does not prepare or stop one.
func (b *Broker) ReportProgress(ctx context.Context, a frontend.SessionAccess, item core.RatingKey, t frontend.Timeline) error {
	viewer, err := b.viewer(ctx, a.Viewer)
	if err != nil || !b.policy(a.Protocol).ExternalAdmission || a.Device.ID == "" || item == 0 {
		return core.ErrUnauthorized
	}
	ctx = state.WithOrigin(ctx, state.Origin{Surface: a.Protocol, SessionID: t.PlaybackID, ClientID: a.Device.ID})
	if err := RecordProgress(ctx, b.deps.Progress, viewer.ID, item, t.Position, t.Duration, t.State); err != nil {
		return err
	}
	if progress, ok := b.deps.Playback.(core.PlaybackTimeline); ok {
		progress.PlaybackProgress(viewer.ID, a.Device.ID, item, t.Position, t.State)
	}
	if b.deps.OnTimeline != nil {
		b.deps.OnTimeline(viewer, item, t.Position, t.Duration, t.State)
	}
	return nil
}
func (b *Broker) Stop(ctx context.Context, a frontend.SessionAccess, id string) error {
	if handled, err := b.previewControl(ctx, a, id, true); handled {
		return err
	}
	if _, handled, err := b.forwardNeutralControl(ctx, a, id, "stop", frontend.Timeline{}, frontend.StreamRequest{}); handled {
		return err
	}
	s, _, err := b.accessSession(ctx, a, id, false)
	if errors.Is(err, core.ErrNotFound) {
		if b.cluster == nil {
			return nil
		}
		v, err := b.viewer(ctx, a.Viewer)
		if err != nil {
			return err
		}
		owner := a.Owner && v.IsOwner && !v.Delegated
		if a.Owner && !owner {
			return core.ErrUnauthorized
		}
		remote, err := b.cluster.remote(ctx)
		if err != nil {
			return err
		}
		for _, r := range remote {
			if r.session.ID != id {
				continue
			}
			if !owner && (r.session.ViewerID != v.ID || r.session.Protocol != a.Protocol || a.Device.ID == "" || r.session.Device.ID != a.Device.ID) {
				return core.ErrUnauthorized
			}
			payload, err := json.Marshal(remoteStop{Node: r.node, ID: id, ActorID: v.ID, DeviceID: a.Device.ID, Protocol: a.Protocol, Owner: owner})
			if err != nil {
				return err
			}
			return b.cluster.node.Publish(ctx, brokerStopTopic, payload)
		}
		return nil
	}
	if err != nil {
		return err
	}
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", s.public.ViewerID, s.clientID))
	if err != nil {
		return err
	}
	defer unlock()
	s, viewer, err := b.accessSession(ctx, a, id, false)
	if err != nil {
		return err
	}
	if s.outgoing {
		b.retireOutgoing(ctx, s.public.ID, "")
		return nil
	}
	if viewer.ID != s.public.ViewerID {
		viewer, err = b.deps.ActiveViewer(ctx, s.public.ViewerID)
	}
	if err == nil && viewer != nil && !s.live && s.format != "" && !b.owners.StaleFor("broker", s.public.ViewerID, s.clientID, s.public.Item, s.playbackID) && b.currentMediaPolicy(ctx, viewer, s) {
		b.recordSourceHistory(ctx, s)
	}
	return b.stopLocked(ctx, s)
}
func (b *Broker) stopLocked(ctx context.Context, s *brokerSession) error {
	if err := b.retireAudio(ctx, s); err != nil {
		return err
	}
	if s.autoSubtitleCancel != nil {
		s.autoSubtitleCancel()
	}
	if s.distributed {
		if storage, ok := b.deps.DistributedHLS.(neutralBindingStore); ok {
			if err := storage.DeleteNeutralBinding(ctx, s.public.ID); err != nil {
				return err
			}
		}
	}
	b.mu.Lock()
	current, ok := b.sessions[s.public.ID]
	var hold *trickplay.Holder
	if ok {
		hold = current.trickplay
	}
	stopSource := false
	var reads []*core.ReadRetirement
	if ok {
		s = current
		reads = slices.Clone(s.servingReads)
		delete(b.sessions, s.public.ID)
		b.rememberRetiredLocked(s)
		if !s.live {
			stopSource = b.releaseSessionSourceLocked(s.playID)
		}
	}
	transfers := b.lazyTransfers[s.public.ID]
	delete(b.lazyTransfers, s.public.ID)
	b.mu.Unlock()
	waitReadRetirement(reads)
	if hold != nil {
		// Plex applies its own idle rule after the stop, so other sources of
		// the same release keep their thumbnails.
		hold.Close(!b.policy(s.public.Protocol).ExternalAdmission)
	}
	for _, cancel := range transfers {
		cancel()
	}
	if !ok {
		return nil
	}
	if s.sourcePinned {
		if pins, ok := b.deps.Playback.(core.PlaybackRevisionPin); ok {
			pins.UnpinSourceRevision(s.playID)
		}
	}
	if b.cluster != nil {
		if err := b.cluster.store.remove(ctx, s.public.ID); err != nil && b.deps.OnEventError != nil {
			b.deps.OnEventError(err)
		}
		b.cluster.announceRetired(ctx, s)
	}
	b.cleanupSampleReceipt(ctx, s)

	b.bindings.Remove(s.public.ID)
	if s.live {
		err := b.deps.Live.Stop(ctx, &core.Viewer{ID: s.public.ViewerID}, s.playID)
		// The broker already authenticated and removed this exact session.
		// A live idle reaper may have joined its backend before the next switch;
		// absence is successful retirement, not a failure to start the new item.
		if errors.Is(err, core.ErrNotFound) {
			err = nil
		}
		b.settleRetirement(context.WithoutCancel(ctx), s, err == nil)
		b.publish(ctx, sessionEvent("SessionStopped", s.public, nil))
		return err
	}
	// A newer binding owns this client and title. Stop the encoder for this
	// session, and leave the source with that binding. This is the broker
	// switch's stop rule; retirement must not drop it.
	if stopSource && b.owners.StaleFor("broker", s.public.ViewerID, s.clientID, s.public.Item, s.playbackID) {
		stopSource = false
	}
	if err := b.retireBackends(ctx, s, stopSource); err != nil && b.deps.OnEventError != nil {
		b.deps.OnEventError(err)
	}
	b.publish(ctx, sessionEvent("SessionStopped", s.public, nil))
	return nil
}

// CleanupStoppedPlayback retires local broker sessions whose underlying
// playback session has already ended or expired, releasing their encoders,
// lazy bindings, and grants cleanly without issuing a cluster stop command.
func (b *Broker) CleanupStoppedPlayback(ctx context.Context, playID string) bool {
	if b == nil || playID == "" {
		return false
	}
	b.mu.Lock()
	var entries []brokerSession
	for _, s := range b.sessions {
		if !s.live && s.playID == playID {
			entries = append(entries, *s)
		}
	}
	b.mu.Unlock()
	if len(entries) == 0 {
		return false
	}
	for i := range entries {
		s := &entries[i]
		unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", s.public.ViewerID, s.clientID))
		if err == nil {
			_ = b.stopLocked(ctx, s)
			unlock()
		}
	}
	return true
}

// CleanupStoppedLive retires local broker live sessions (including multiview
// tiles) whose underlying live session has already expired or stopped, freeing
// broker state, cluster ownership, and frontend watchers independently of any
// multiview group TTL.
func (b *Broker) CleanupStoppedLive(ctx context.Context, playID string) bool {
	if b == nil || playID == "" {
		return false
	}
	b.mu.Lock()
	var entries []brokerSession
	for _, s := range b.sessions {
		if s.live && s.playID == playID {
			entries = append(entries, *s)
		}
	}
	b.mu.Unlock()
	if len(entries) == 0 {
		return false
	}
	for i := range entries {
		s := &entries[i]
		gateKey := ownerGateKey(s.public.ViewerID, s.clientID)
		if s.multiviewGroup != "" {
			gateKey = tileGateKey(s.public.ViewerID, s.clientID, s.multiviewGroup, s.multiviewTile)
		}
		unlock, err := b.gates.Acquire(ctx, gateKey)
		if err == nil {
			_ = b.stopLocked(ctx, s)
			unlock()
		}
	}
	return true
}

// StopPlayback stops every local broker session fed by a source ID.
func (b *Broker) StopPlayback(ctx context.Context, playID string) bool {
	b.mu.Lock()
	var entries []brokerSession
	for _, s := range b.sessions {
		if s.playID == playID {
			entries = append(entries, *s)
		}
	}
	b.mu.Unlock()
	stopped := true
	for i := range entries {
		s := &entries[i]
		unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", s.public.ViewerID, s.clientID))
		if err == nil {
			cmd := frontend.Command{Kind: frontend.CommandStop}
			b.publish(ctx, frontend.Event{Type: "SessionCommand", ViewerID: s.public.ViewerID, SessionID: s.public.ID, TargetProtocol: s.public.Protocol, TargetDeviceID: s.public.Device.ID, Command: &cmd})
			_ = b.stopLocked(ctx, s)
			unlock()
		} else {
			stopped = false
		}
	}
	if len(entries) == 0 && b.cluster != nil {
		remote, err := b.cluster.remote(ctx)
		if err != nil {
			if b.deps.OnEventError != nil {
				b.deps.OnEventError(err)
			}
			return false
		}
		for _, r := range remote {
			if r.sourceID != "" && r.sourceID == playID {
				payload, err := json.Marshal(remoteStop{Node: r.node, SourceID: playID})
				if err == nil {
					err = b.cluster.node.Publish(ctx, brokerStopTopic, payload)
				}
				if err != nil && b.deps.OnEventError != nil {
					b.deps.OnEventError(err)
				}
				return err == nil
			}
		}
	}
	return len(entries) > 0 && stopped
}

// Close stops local broker sessions and prevents new plans or opens.
func (b *Broker) Close(ctx context.Context) {
	b.mu.Lock()
	if b.closed && len(b.sessions) == 0 && len(b.plans) == 0 {
		b.mu.Unlock()
		return
	}
	firstClose := !b.closed
	b.closed = true
	b.multiviewGroups = nil
	b.nonGroupEpoch = nil
	for _, s := range b.previews {
		s.cancel()
	}
	b.previews = nil
	for _, job := range b.previewPreparations {
		job.cancel()
	}
	b.previewPreparations = nil
	if b.historyCancel != nil {
		b.historyCancel()
	}
	if b.timelineCancel != nil {
		b.timelineCancel()
	}
	timelineDone := b.timelineDone
	var transferCancels []context.CancelFunc
	for _, transfers := range b.lazyTransfers {
		for _, cancel := range transfers {
			transferCancels = append(transferCancels, cancel)
		}
	}
	b.lazyTransfers = nil
	lazyCancel, lazyDone := b.lazyCleanupCancel, b.lazyCleanupDone
	var ids, planIDs []string
	seen := make(map[string]bool)
	for _, s := range b.sessions {
		if s.autoSubtitleCancel != nil {
			s.autoSubtitleCancel()
		}
		if !seen[s.playID] {
			ids = append(ids, s.playID)
			seen[s.playID] = true
		}
	}
	for id := range b.plans {
		planIDs = append(planIDs, id)
	}
	b.mu.Unlock()
	for _, cancel := range transferCancels {
		cancel()
	}
	b.autoSubtitleWG.Wait()
	b.previewWG.Wait()
	// Shutdown must still release sessions when its caller has already been
	// canceled; bound the cleanup so an unresponsive gate cannot block forever.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.shutdownTimeout)
	defer cancel()
	if timelineDone != nil {
		select {
		case <-timelineDone:
		case <-cleanupCtx.Done():
		}
	}
	if b.historyDone != nil {
		drainCtx, drainCancel := context.WithTimeout(cleanupCtx, 150*time.Millisecond)
		select {
		case <-b.historyDone:
		case <-drainCtx.Done():
			if b.deps.OnEventError != nil {
				b.deps.OnEventError(errors.New("sessions: source history writer did not stop after cancellation"))
			}
		}
		drainCancel()
	}
	if lazyCancel != nil {
		lazyCancel()
	}
	if lazyDone != nil {
		select {
		case <-lazyDone:
		case <-cleanupCtx.Done():
			if b.deps.OnEventError != nil {
				b.deps.OnEventError(fmt.Errorf("sessions: lazy cleanup worker did not stop during shutdown"))
			}
		}
	}
	// The bounded plan sweeper stays alive until outstanding owner gates and
	// in-flight opens release their references, even after this budget expires.
	for _, id := range ids {
		if !b.StopPlayback(cleanupCtx, id) && b.deps.OnEventError != nil {
			b.deps.OnEventError(fmt.Errorf("sessions: could not stop playback %s during shutdown", id))
		}
	}
	for _, id := range planIDs {
		b.dropPlan(cleanupCtx, id, true)
	}
	if firstClose && b.cluster != nil {
		b.cluster.close()
	}
}

// admitPlanDecision decides delivery for a resolved source and applies the
// request's admission rules (transcoding policy, delivery modes, and Vela's
// HLS/HTTP output check).
func (b *Broker) admitPlanDecision(ps *core.PlaySession, req *frontend.PlanRequest) (core.Decision, error) {
	dec, err := b.decideTracks(ps, req)
	if err != nil {
		return dec, err
	}
	if req.Channel == "" && b.videoOff(req.Caps) && decisionEncodesVideo(dec, ps.Info) {
		return core.Decision{}, &frontend.NoCompatiblePathError{
			RejectedPaths: []frontend.RejectedPlaybackPath{{Path: "video", Reasons: []string{reasonVideoTranscodingOff}}},
			Cause:         core.ErrVideoTranscodingDisabled,
		}
	}
	if req.Intent.AllowTranscoding != nil && !*req.Intent.AllowTranscoding && decisionEncodes(dec, ps.Info) {
		return core.Decision{}, &frontend.NoCompatiblePathError{RejectedPaths: []frontend.RejectedPlaybackPath{{Path: "transcode", Reasons: []string{"transcode not allowed"}}}}
	}
	if (dec.Kind == core.DecisionDirectPlay && !req.Intent.Options.DirectPlay) || (dec.Kind == core.DecisionRemux && !req.Intent.Options.DirectStream) {
		return core.Decision{}, &frontend.NoCompatiblePathError{RejectedPaths: []frontend.RejectedPlaybackPath{{Path: string(dec.Kind), Reasons: []string{"delivery mode not allowed by client"}}}}
	}
	if req.Protocol == "vela" && dec.Kind != core.DecisionDirectPlay {
		hlsAllowed, _ := b.hlsPlanOutput(ps, *req, dec)
		httpAllowed := false
		if ps.Info.Video() == nil && ps.Info.HasPlayableMedia() {
			httpAllowed, _ = b.httpPlanOutput(ps, *req, dec)
		}
		if !hlsAllowed && !httpAllowed {
			outErr := &frontend.NoCompatiblePathError{RejectedPaths: []frontend.RejectedPlaybackPath{{Path: "original", Reasons: b.originalRejection(ps.Info, *req)}, {Path: "hls", Reasons: []string{outputRejection(dec, ps.Info, req.Intent.AllowTranscoding)}}}}
			if req.Channel == "" && b.videoOff(req.Caps) {
				if hlsDec, ok := b.hlsOutputDecision(ps, req.Caps, req.Intent.Options, req.Intent.AllowTranscoding, dec); !ok && decisionEncodesVideo(hlsDec, ps.Info) {
					outErr.RejectedPaths[1].Reasons = []string{reasonVideoTranscodingOff}
					outErr.Cause = core.ErrVideoTranscodingDisabled
				}
			}
			return core.Decision{}, outErr
		}
	}
	return dec, nil
}

// admitWithClientCap runs admit under the request's caps. When Vela supplied a
// client streaming cap (client.maxBitrate, together with maxWidth/maxHeight):
//   - If nothing within the cap can play (or only an unconverted copy over the
//     cap), it falls back to the original with only the client's cap lifted
//     (owner bitrate and height caps still apply).
//   - If the client's cap converted an otherwise direct-playable original, it
//     returns that original decision as an unselected explicit choice without
//     starting any work.
func (b *Broker) admitWithClientCap(ps *core.PlaySession, req *frontend.PlanRequest, cap clientCapPolicy, admit func(*core.PlaySession, *frontend.PlanRequest) (core.Decision, error)) (core.Decision, *core.Decision, error) {
	dec, err := admit(ps, req)
	if cap.clientKbps > 0 && (err != nil || (dec.Kind != core.DecisionDirectPlay && !decisionEncodes(dec, ps.Info))) {
		lifted := clientCapLifted(*req, cap.ownerKbps, cap.ownerHeight)
		if liftedDec, liftedErr := admit(ps, &lifted); liftedErr == nil && !decisionEncodes(liftedDec, ps.Info) && (err != nil || liftedDec.Kind == core.DecisionDirectPlay) {
			dec, err, *req = liftedDec, nil, lifted
		}
	}
	if err != nil {
		return core.Decision{}, nil, err
	}
	var originalChoice *core.Decision
	if cap.clientKbps > 0 && dec.Kind != core.DecisionDirectPlay {
		if d, e := b.decideProfile(ps, clientCapLifted(*req, cap.ownerKbps, cap.ownerHeight)); e == nil && d.Kind == core.DecisionDirectPlay {
			originalChoice = &d
		}
	}
	return dec, originalChoice, nil
}

// clientCapLifted is req without the client's own streaming cap (bitrate and
// width/height box); ownerKbps (viewer limit, away cap, upload budget) and
// ownerHeight (quality rule, app compatibility override) stay.
func clientCapLifted(req frontend.PlanRequest, ownerKbps, ownerHeight int) frontend.PlanRequest {
	req.Intent.Options.MaxBitrateKbps = ownerKbps
	req.Caps.MaxBitrateKbps = ownerKbps
	req.Intent.Options.MaxWidth = 0
	req.Intent.Options.MaxHeight = ownerHeight
	req.Caps.MaxHeight = ownerHeight
	return req
}

func stricterLimit(a, b int) int {
	if a <= 0 || (b > 0 && b < a) {
		return b
	}
	return a
}

// Keep the household limits even when the viewer explicitly chooses Original.
// Evaluate the title rules before intersecting them with the Vela preset, so
// a stricter client preset cannot hide a household limit on the same axis.
func (b *Broker) applyTitleRuleLimits(ctx context.Context, req *frontend.PlanRequest, policy *clientCapPolicy, viewer *core.Viewer, title *core.Title) {
	if policy.clientKbps == 0 || b.deps.ApplyRuleLimits == nil {
		req.Caps = b.applyRuleLimits(ctx, req.Caps, &req.Intent.Options, viewer, title)
		return
	}
	owner := clientCapLifted(*req, policy.ownerKbps, policy.ownerHeight)
	caps := b.applyRuleLimits(ctx, owner.Caps, nil, viewer, title)
	policy.ownerKbps, policy.ownerHeight = caps.MaxBitrateKbps, caps.MaxHeight
	caps.MaxBitrateKbps = stricterLimit(req.Caps.MaxBitrateKbps, caps.MaxBitrateKbps)
	caps.MaxHeight = stricterLimit(req.Caps.MaxHeight, caps.MaxHeight)
	req.Caps = caps
	req.Intent.Options.MaxBitrateKbps = stricterLimit(req.Intent.Options.MaxBitrateKbps, caps.MaxBitrateKbps)
}

func (b *Broker) applyRuleLimits(ctx context.Context, caps core.ClientCaps, opts *core.DecideOptions, v *core.Viewer, title *core.Title) core.ClientCaps {
	if b == nil || b.deps.ApplyRuleLimits == nil {
		return caps
	}
	updated := b.deps.ApplyRuleLimits(ctx, caps, v, title)
	if opts != nil && updated.MaxBitrateKbps > 0 && (opts.MaxBitrateKbps == 0 || updated.MaxBitrateKbps < opts.MaxBitrateKbps) {
		opts.MaxBitrateKbps = updated.MaxBitrateKbps
	}
	return updated
}
