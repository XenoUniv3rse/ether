package core

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// Transcoder (internal/transcode)

type HLSResume struct {
	FirstSegment int
	Generation   uint64
}

// HLSRamp freezes private HLS planning across config changes and replacement.
// Version 0 is legacy; version 1 with Segments 0 explicitly selects steady HLS.
// All fields are values so admission/request copies retain an immutable snapshot.
type HLSRamp struct {
	Version  int
	Segments int
	Duration time.Duration
}

// ArchiveRangeOpener opens the sequential loopback input at a session-relative
// offset. Release closes its upstream connection once the encoder is done.
type ArchiveRangeOpener func(context.Context, time.Duration) (ArchiveRangeInput, error)

type ArchiveRangeInput struct {
	URL     string
	Trim    time.Duration
	Release func()
}

type HLSRequest struct {
	// WaitForEncoder gives interactive starts a bounded wait at the local encoder.
	// EncoderViewer identifies a viewer in owner-only capacity diagnostics.
	WaitForEncoder bool   `json:"-"`
	EncoderViewer  string `json:"-"`

	// PMSDASH selects the single-video PMS manifest and five-second grid for Plex TV apps.
	PMSDASH bool

	// MediaGeneration is a trusted presentation namespace retained across encoder
	// recovery. A new playback start must use a fresh value; empty mints one.
	MediaGeneration string `json:",omitempty"`

	// AdaptiveHLS lets the player choose variants on the authored timeline.
	AdaptiveHLS     bool
	AdaptiveMaxKbps int // peak delivery cap, including audio and mux overhead

	// QualityRungs explicitly requests the lazy, policy-filtered file-session ladder.
	QualityRungs    bool
	QualityRungIDs  []string
	QualityVariants []QualityVariant `json:"-"`

	// LazyStart defers the first encoder until the client selects a rendition.
	LazyStart bool
	// LadderVideoCodecs restricts optional encoded rungs when non-nil.
	// An empty list permits only the primary decision.
	LadderVideoCodecs []string
	Diagnostics       bool
	// Continuous disables ramps and governor changes that introduce discontinuities.
	Continuous bool
	// FirstSegmentAt is the title time a full-timeline (StartOffset zero)
	// client seeks to first, such as a Jellyfin resume point. The encoder
	// starts at that segment instead of at zero. Zero starts at the beginning.
	FirstSegmentAt time.Duration `json:",omitempty"`
	Ramp           HLSRamp       `json:",omitzero"`
	Resume         *HLSResume    `json:",omitempty"`
	// RecoveryStart is trusted composition only; nil preserves legacy behavior.
	RecoveryStart *RecoveryStart `json:",omitempty"`

	// ManifestSessionID is a trusted host-assigned public session identifier used
	// only for DASH representation URL rendering. Empty retains SessionID. It
	// never changes encoder registry identity, source scope or retirement ownership.
	ManifestSessionID string
	// MultiviewGroup scopes live tile transcode accounting so all tiles in one
	// multiview group share a single transcode slot against MaxTranscodes.
	MultiviewGroup string `json:",omitempty"`

	// SlateText is trusted server-generated copy for a looping silent card.
	// It is never decoded from client requests. Info.Duration bounds the slot.
	SlateText []string `json:"-"`
	// SoftSubtitleStream, when set, asks the encoder to expose this embedded
	// text stream as ASS alongside its ordinary media output.
	SoftSubtitleStream *int
	// TextSubtitleStreams tees embedded text tracks from the HLS video input.
	TextSubtitleStreams []int `json:",omitempty"`
	// TextSubtitleSource is the host-verified file revision for durable cue windows.
	TextSubtitleSource string `json:",omitempty"`
	// RequireIndependentProof emits HLS independence tags only when the
	// session can guarantee every advertised segment is independently decodable.
	// Zero preserves the legacy playlist contract for existing callers.
	RequireIndependentProof bool
	SessionID               string
	// ReleaseID identifies the initial source in manifest diagnostics.
	ReleaseID string
	InputURL  string // ffmpeg input; Ether serves sources on loopback HTTP with range support
	// LiveInput permits playlist demuxers for owner-configured live sources.
	// Ordinary resolved media must leave this false, even if duration is unknown.
	LiveInput bool
	// ArchiveInput reads a finite provider range from a sequential HTTP
	// response. ArchiveTrim discards the provider's rounded leading time.
	ArchiveInput     bool               `json:"-"`
	ArchiveTrim      time.Duration      `json:"-"`
	OpenArchiveRange ArchiveRangeOpener `json:"-"`
	// NoVideoTranscode enforces the owner's per-viewer "no video transcoding"
	// policy on this session (never re-encode video; LiveInput is unaffected).
	NoVideoTranscode bool `json:",omitempty"`
	// LiveFromBacklog starts a live HLS input at the first segment its
	// playlist lists (Live TV's relay trims that playlist to the channel's
	// stability reserve), so the backlog primes the reserve instead of the
	// demuxer's default three segments from the live edge.
	LiveFromBacklog bool
	// LiveEndOnEOF publishes the final live segments and ends the playlist
	// on a clean encoder exit, letting an owned relay splice the next input.
	LiveEndOnEOF bool `json:"-"`
	// CopyTimestamps preserves source timestamps for HTTP Matroska output.
	CopyTimestamps bool
	// InputHeaders are optional HTTP headers ffmpeg must send when reading
	// InputURL (e.g. for resolved trailer URLs). Transcoders that cannot
	// send headers ignore them; most resolved URLs need none (additive).
	InputHeaders map[string]string
	// InputLease is trusted host composition only, never decoded from wire
	// requests. A caller retains an unclaimed lease if admission fails early.
	InputLease  EncoderInputLease `json:"-"`
	Info        MediaInfo
	Decision    Decision
	StartOffset time.Duration
	// ResumeHint warms one speculative Matroska copy seek without moving the playlist origin.
	ResumeHint time.Duration `json:",omitempty"`
	// SegmentDuration defaults to 4s when zero (5s for DASH).
	SegmentDuration time.Duration
	// Protocol is ProtocolHLS (default when empty), ProtocolDASH, or
	// ProtocolHTTP (Plex for Mac / Desktop's chunked Matroska stream;
	// the constant lives with the other "http" protocol name in debrid.go).
	// A DASH session also implements DASHSession; an HTTP session also
	// implements HTTPSession. Transcoder.Session returns either as an
	// HLSSession, so type-assert to reach the protocol methods.
	Protocol string
}

// Streaming protocols of HLSRequest.Protocol.
const (
	ProtocolHLS  = "hls"
	ProtocolDASH = "dash"
)

// DASHSession is a running ffmpeg remux or transcode producing MPEG-DASH:
// a static, full-length MPD with separate video and audio representations
// of fMP4 segments addressed by number ($Number$, from 0). Its HLSSession
// methods map onto the first representation; ServePlaylist writes the MPD.
type DASHSession interface {
	HLSSession
	// ServeManifest writes the MPD.
	ServeManifest(w http.ResponseWriter, r *http.Request)
	// ServeInit writes the initialization segment of a representation.
	ServeInit(w http.ResponseWriter, r *http.Request, rep int)
	// ServeMedia writes media segment n of a representation, waiting for
	// ffmpeg and restarting it at the right offset when a seek jumps ahead.
	ServeMedia(w http.ResponseWriter, r *http.Request, rep, n int)
}

// HTTPSession is a running ffmpeg remux or transcode writing one
// streamable Matroska to the client (Plex for Mac protocol=http).
// ServeHTTP writes the chunked body; a later start with offset is a
// new StartHLS of the same session id.
type HTTPSession interface {
	HLSSession
	ServeHTTP(w http.ResponseWriter, r *http.Request)
}

// HLSSession is one running ffmpeg remux or transcode producing HLS (or
// DASH; see DASHSession).
type HLSSession interface {
	ID() string
	// ServePlaylist writes the media playlist (index.m3u8).
	ServePlaylist(w http.ResponseWriter, r *http.Request)
	// ServeSegment writes segment n, waiting for ffmpeg to produce it and
	// restarting ffmpeg at the right offset when a seek jumps ahead.
	ServeSegment(w http.ResponseWriter, r *http.Request, n int)
	// SwapInput restarts the session on a new input at the given position
	// (mid-stream failover). Segment numbering continues.
	SwapInput(ctx context.Context, inputURL string, info MediaInfo, at time.Duration) error
	LastAccess() time.Time
	Close() error
}

type Transcoder interface {
	Probe(ctx context.Context, inputURL string) (*MediaInfo, error)
	Decide(info MediaInfo, caps ClientCaps, opts DecideOptions) Decision
	// StartHLS starts an HLS session, a DASH session (a DASHSession)
	// when req.Protocol is ProtocolDASH, or an HTTP Matroska session
	// (an HTTPSession) when req.Protocol is ProtocolHTTP.
	StartHLS(ctx context.Context, req HLSRequest) (HLSSession, error)
	Session(id string) (HLSSession, bool)
	Stop(id string)
	HWAccel() HWAccel
}

// SubtitleExtractor is optionally implemented by transcoders that can write
// an embedded text subtitle stream as WebVTT, so a track picked mid-playback
// can be served from the transcode session without a restart. Image
// subtitles (PGS/VobSub) return an error.
type SubtitleExtractor interface {
	ExtractSubtitle(ctx context.Context, inputURL string, streamIndex int, codec string, w io.Writer) error
}

// DecideOptions are per-request choices from the Plex decision request.
type DecideOptions struct {
	// ToneMap limits apply only to HDR-to-SDR video conversion, never direct
	// playback or copy-only remux. Zero preserves the existing output budget.
	ToneMapMaxHeight, ToneMapMaxBitrateKbps int
	// DisableVideoTranscode prevents optional enhancement from promoting direct playback.
	DisableVideoTranscode bool

	DirectPlay     bool     // client allows direct play
	DirectStream   bool     // client allows remux
	MaxBitrateKbps int      // client-requested cap (maxVideoBitrate)
	MaxHeight      int      // from videoResolution
	MaxWidth       int      // explicit output width ceiling
	AudioStream    int      // selected stream index, -1 default
	SubtitleStream int      // -1 none
	AudioFilters   []string // trusted owner/viewer audio processing; forces audio encoding
	BurnSubtitles  bool
	// BurnTextSubtitles lets BurnSubtitles burn a text track too. Set for
	// a client that draws no soft text track in this playback (Plex Web
	// asking to burn an ASS track). Additive.
	BurnTextSubtitles bool
	// Protocol is the conversion protocol the client asked for (hls, dash,
	// http). Empty keeps the HLS target. Additive.
	Protocol string
}

// ---------------------------------------------------------------------------
// Playback orchestration (internal/playback)

// PlaySession binds a user, a title and a client to a resolved, open source.
type PlaySession struct {
	// OriginalRevision is the revision a replanned native file URL must name.
	OriginalRevision string `json:"-"`

	Encoder       *EncoderStatus `json:"encoder,omitempty"`
	PlaybackGroup string         `json:"playbackGroup,omitempty"`
	// PreparedOnly marks a source reserved for planning but not opened by a
	// frontend. Cluster snapshots carry it for owner Activity.
	PreparedOnly bool `json:"preparedOnly,omitempty"`
	ID           string
	UserID       int64
	ClientID     string
	Key          RatingKey
	Resolution   *Resolution
	Source       MediaSource // swaps on failover; read through Playback.Source
	Info         MediaInfo
	StartedAt    time.Time

	// Status for the admin dashboard and /status/sessions, filled on the
	// snapshots Playback returns.
	TitleName        string       // "Movie (2024)" or "Show - S01E02 - Episode"
	LiveChannel      *LiveChannel `json:"liveChannel,omitempty"` // selected channel, including when programme metadata is available
	Username         string       // user display name
	Client           ClientCaps   // the client's product, device and limits
	DecisionReason   string       `json:"decisionReason,omitempty"`
	QualityFallback  string       `json:"qualityFallback,omitempty"`
	Decision         DecisionKind // directplay once bytes go straight to the client; "" until known
	DeliveryDecision *Decision    `json:"deliveryDecision,omitempty"` // negotiated output and track selection
	// DecisionVideoCopy and DecisionAudioCopy say whether the current
	// decision copies each stream (additive): a remux with
	// DecisionAudioCopy false is transcoding only the audio.
	DecisionVideoCopy bool  `json:"decisionVideoCopy,omitempty"`
	DecisionAudioCopy bool  `json:"decisionAudioCopy,omitempty"`
	BytesServed       int64 // source bytes delivered (direct play and ffmpeg input)
	SpeedBps          int64 // recent delivery rate, bytes per second
	// BufferedBytes is contiguous media downloaded ahead of the reader; zero
	// when the source cannot report it. BufferedUntil is the absolute media
	// time that read-ahead reaches, zero when unknown.
	BufferedBytes int64
	BufferedUntil time.Duration
	LastActive    time.Time
	// Liveness is the session's evaluated state under the shared liveness rule
	// (playing, paused, starting, idle, stopped).
	Liveness SessionLivenessState `json:"liveness,omitempty"`
	// ViewerState is the last client timeline state: playing, paused, or stopped.
	ViewerState string `json:"viewerState,omitempty"`
	// ViewOffset is the client's last reported playhead in milliseconds.
	// Nil means no timeline has arrived; source bytes do not imply progress.
	ViewOffset *int64 `json:"viewOffset,omitempty"`
	Failovers  int    // mid-stream release swaps so far
	// Origin names where the current release streams from: "Usenet" or the
	// debrid service, such as "Real-Debrid" (additive).
	Origin string `json:"origin,omitempty"`
	// DownFailovers counts speed-driven swaps to a lower-bitrate release
	// (a subset of Failovers) (additive).
	DownFailovers int `json:"downFailovers,omitempty"`
	// RequiresRemux is set when a direct-play failover could not splice or
	// find an exact-size replacement: the next decision/start for this
	// session must be a zero-encode remux (additive).
	RequiresRemux bool `json:"requiresRemux,omitempty"`
	// Node is the cluster node serving a session reported by another node
	// (empty for this node's own sessions and on a standalone server).
	Node string `json:"node,omitempty"`
	// Experience is this play's viewer-experience accounting: start time
	// and mid-play buffering (additive). Nil for a source no play request
	// or client timeline has reached.
	Experience *PlayExperience `json:"experience,omitempty"`
	// SegmentStart is where the requested episode begins inside a combined
	// multi-episode file ("S01E01-E02" played for its second episode):
	// server-started streams add it to the client's episode-relative
	// position. Zero for a file holding only the requested episode, and
	// recomputed on every snapshot so a failover to another release
	// re-derives it (additive).
	SegmentStart time.Duration `json:"segmentStart,omitempty"`
}

// ProbedChapterCount identifies source chapter facts, including a probed
// file with zero chapters. Estimates and live programmes have no file count.
func (p *PlaySession) ProbedChapterCount() *int {
	if p.LiveChannel != nil || p.Info.Estimated || p.Info.Video() == nil || (!p.Info.ChaptersProbed && len(p.Info.Chapters) == 0) {
		return nil
	}
	count := len(p.Info.Chapters)
	return &count
}

// LiveChannel identifies a live session's selected channel independently of its programme.
type LiveChannel struct {
	ID               string                `json:"id,omitempty"`
	Name             string                `json:"name,omitempty"`
	Number           string                `json:"number,omitempty"`
	Provider         *LiveProvider         `json:"provider,omitempty"`
	PreviousProvider *LiveProviderIdentity `json:"previousProvider,omitempty"`
}

// LiveProviderIdentity contains only display-safe metadata from the serving input.
type LiveProviderIdentity struct {
	SourceName  string `json:"sourceName"`
	LoginID     string `json:"loginId,omitempty"`
	LoginLabel  string `json:"loginLabel,omitempty"`
	LoginNumber int    `json:"loginNumber,omitempty"`
}

type LiveProvider struct {
	LiveProviderIdentity
	LoginSlot  int  `json:"loginSlot,omitempty"`
	LoginLimit int  `json:"loginLimit,omitempty"`
	LocalInUse *int `json:"localInUse,omitempty"`
}

// SourceReleaseID identifies the selected release when one is available.
func (s *PlaySession) SourceReleaseID() string {
	if s == nil || s.Resolution == nil {
		return ""
	}
	return s.Resolution.Release.ID
}

// SourcePosition translates a client's episode-relative position into the
// source file's timeline, past any earlier episodes a combined file holds
// before the requested one (SegmentStart). Server-started streams start at
// the translated position; client-reported positions stay episode-relative.
func (s *PlaySession) SourcePosition(client time.Duration) time.Duration {
	if s == nil {
		return client
	}
	return client + s.SegmentStart
}

// PlayExperience measures what the viewer saw (additive).
type PlayExperience struct {
	// Requested is set once a play request (decision, start, part or
	// stream open) or a client timeline reached the source.
	Requested bool `json:"requested"`
	// Pictured is set once the client showed a picture; StartTime is the
	// time from the play request to it, negative when unknown.
	Pictured  bool          `json:"pictured"`
	StartTime time.Duration `json:"startTime"`
	// RequestedAt and PicturedAt are the instants behind StartTime (zero
	// when unknown), so a history row spanning several sessions can
	// measure from its first request to its first picture.
	RequestedAt time.Time `json:"requestedAt,omitzero"`
	PicturedAt  time.Time `json:"picturedAt,omitzero"`
	// StartFailure names why a start never reached a picture even though
	// the client reported one (a stalled start the server ended).
	StartFailure string `json:"startFailure,omitempty"`
	// BufferEvents counts mid-play stalls after the first picture; Buffering
	// is their total length (startup excluded).
	BufferEvents int           `json:"bufferEvents"`
	Buffering    time.Duration `json:"buffering"`
}

// EncoderStatus describes the currently running output process, not the
// original playback negotiation. Times are media timestamps or wall-clock
// instants as indicated by their field names.
type EncoderStatus struct {
	RecoveryReason string         `json:"recoveryReason,omitempty"`
	PlayerPosition *time.Duration `json:"playerPosition,omitempty"`
	Runway         *time.Duration `json:"runway,omitempty"`
	Decision       Decision       `json:"decision"`
	// Rung is the ABR rung the active run encodes ("" or "base" for the
	// session's primary decision).
	Rung               string        `json:"rung,omitempty"`
	Processor          string        `json:"processor"`
	Filters            string        `json:"filters"`
	Phase              string        `json:"phase"`
	Reason             string        `json:"reason,omitempty"`
	StartedAt          time.Time     `json:"startedAt"`
	UpdatedAt          time.Time     `json:"updatedAt"`
	Speed              float64       `json:"speed"`
	OutputTime         time.Duration `json:"outputTime"`
	ReadyUntil         time.Duration `json:"readyUntil"`
	MinOffsetAvailable time.Duration `json:"minOffsetAvailable,omitempty"`
	HWDecoder          string        `json:"hwDecoder,omitempty"`
	HWEncoder          string        `json:"hwEncoder,omitempty"`
}

type Playback interface {
	// Estimate is the virtual media info shown before a title is resolved.
	Estimate(ctx context.Context, t *Title, caps ClientCaps) MediaInfo
	// Prepare resolves and opens a source for the user and client, enforcing
	// stream limits. Reuses an existing session for the same user/client/key.
	Prepare(ctx context.Context, user *User, key RatingKey, caps ClientCaps) (*PlaySession, error)
	Session(id string) (*PlaySession, bool)
	// SessionFor finds the live session of a client for a title.
	SessionFor(userID int64, clientID string, key RatingKey) (*PlaySession, bool)
	// ServeRange serves the session's current source with HTTP range
	// support, failing over to the next release when articles are missing.
	ServeRange(w http.ResponseWriter, r *http.Request, sessionID string)
	// InputURL is a loopback URL ffmpeg can read the session's source from.
	InputURL(sessionID string) string
	// Failover swaps the session to the next candidate release, keeping the
	// timestamp. Returns the new media info.
	Failover(ctx context.Context, sessionID string, at time.Duration) (*PlaySession, error)
	Stop(sessionID string)
	Sessions() []*PlaySession
}

// DirectDeliverer is implemented by a Playback that can name the provider URL
// a direct play of one of its sessions may be redirected to (additive). A
// runtime without it — a remote cluster playback, a fixture — always relays.
type DirectDeliverer interface {
	// DirectDelivery reports the provider URL this session's file can be
	// fetched from directly, and records the read as a direct play of a live
	// session, the way a relayed range read would.
	DirectDelivery(sessionID string) (link DirectSourceLink, ok bool)
}

// SurfaceScopedPlayback separates client identifiers by their authenticated
// protocol surface, even when different clients supply identical IDs.
type SurfaceScopedPlayback interface {
	PrepareSurface(ctx context.Context, user *User, key RatingKey, caps ClientCaps, releaseID string, exact bool) (*PlaySession, error)
	SessionForSurface(surface string, userID int64, clientID string, key RatingKey, releaseID string, exact bool) (*PlaySession, bool)
	MarkSurfaceOpened(id string) bool
	MarkSurfacePrepared(id string) bool
}

// PlaybackRevisionPin keeps an opened generation from silently failing over
// while a client consumes metadata bound to its source revision.
type PlaybackRevisionPin interface {
	PinSourceRevision(ctx context.Context, id, revision string) error
	UnpinSourceRevision(id string)
}

// SourceBinding says why an exact session is held on its release, which
// decides whether that release may still be replaced when it fails.
type SourceBinding uint8

const (
	// SourceBindingUnspecified keeps the exact selection locked.
	SourceBindingUnspecified SourceBinding = iota
	// SourceBindingExplicit is a version the viewer picked. It is never
	// replaced. A provably unreadable release ends playback with
	// ErrVersionUnreadable instead.
	SourceBindingExplicit
	// SourceBindingRemembered resumes the release this viewer last played.
	// It is replaced only when it is proven unreadable.
	SourceBindingRemembered
	// SourceBindingAuto is the release a client was handed as its Auto or
	// default entry. It fails over like any automatic session.
	SourceBindingAuto
)

// ErrVersionUnreadable ends playback of a picked version whose release is
// provably unreadable (corrupt or missing data on every provider).
var ErrVersionUnreadable = fmt.Errorf("this version can't be played: the file is incomplete on your provider: %w", ErrReleaseRejected)

// ErrOriginalLost means all eligible debrid accounts confirmed that the
// same file cannot be recovered. Expired links and transient failures do not
// establish loss.
var ErrOriginalLost = fmt.Errorf("provider no longer has the original: %w", ErrSourceUnavailable)

// OriginalRecovery replaces the current original after confirmed loss. Admit
// scores a probed candidate between zero (incompatible) and one (best fit).
// Direct range requests must name the new revision after replacement.
type OriginalRecovery interface {
	ReplaceLostOriginal(ctx context.Context, id, revision string, position time.Duration, explicit bool, admit func(*PlaySession) float64) (*PlaySession, time.Duration, error)
}

type originalRangeKey struct{}

type OriginalRange struct {
	Revision string
	Lost     func()
}

func WithOriginalRange(ctx context.Context, request OriginalRange) context.Context {
	return context.WithValue(ctx, originalRangeKey{}, request)
}

func OriginalRangeFrom(ctx context.Context) OriginalRange {
	request, _ := ctx.Value(originalRangeKey{}).(OriginalRange)
	return request
}

// SourceBinder records why an exact playback session holds its release.
type SourceBinder interface {
	BindSource(id string, binding SourceBinding)
}

// PlaybackSourceObservation contains scalar facts from one opened source
// generation. Empty Revision means its identity cannot be established safely;
// nil HDR and empty codec/zero height remain unknown.
type PlaybackSourceObservation struct {
	Revision, ReleaseID string
	VideoCodec          string
	Height              int
	HDR                 *bool
}

// PlaybackSourceHistory contains persisted source facts without raw release
// identifiers. A digest can only be matched against authorized candidates.
type PlaybackSourceHistory struct {
	Revision, ReleaseDigest, VideoCodec string
	Height                              int
	HDR                                 *bool
}

// PlaybackSourceSnapshot copies immutable facts validated at source open/probe
// for the current generation. It performs no filesystem I/O and is not fresh
// authorization or a claim that a local path has remained unchanged.
type PlaybackSourceSnapshot interface {
	CurrentSource(context.Context, string) (PlaybackSourceObservation, error)
}

// PlaybackSelectionFence validates an observed selection against the exact
// current source. fn must be short and must not read media or call a provider.
// The source fence is acquired before any manual-intent database writer in fn.
type PlaybackSelectionFence interface {
	WithSourceSelection(context.Context, string, string, func(MediaInfo) error) error
}

type sourceDecisionKey struct{}

// SourceDecision evaluates a candidate before its source identity is published.
// The estimate may lack full track evidence, so the chooser checks again on open.
type SourceDecision struct {
	Options DecideOptions
	Decide  func(MediaInfo, *Resolution) Decision
}

// WithSourceDecision carries the playback request's output permissions
// into source selection, before the source revision is published.
func WithSourceDecision(ctx context.Context, selection SourceDecision) context.Context {
	return context.WithValue(ctx, sourceDecisionKey{}, selection)
}

func PlaybackSourceDecision(ctx context.Context) (SourceDecision, bool) {
	selection, ok := ctx.Value(sourceDecisionKey{}).(SourceDecision)
	return selection, ok
}
