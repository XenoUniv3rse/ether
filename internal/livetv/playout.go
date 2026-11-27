package livetv

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/streamgrant"
	"github.com/braedonsaunders/ether/internal/transcode"
)

const (
	segmentDuration = 4 * time.Second

	iptvProbeBudget = 8 * time.Second
	iptvStartFloor  = 8 * time.Second
	iptvStartMax    = 12 * time.Second
)

// iptvSourceStartBudget gives post-probe stream startup its own bounded window
// once probing proved the input answers. Slower routes (such as an outbound
// SOCKS5 proxy) take longer per round-trip to open playlists and first
// segments too, so the window scales from the probe's measured duration rather
// than a proxy-specific constant.
func iptvSourceStartBudget(probeElapsed time.Duration) time.Duration {
	return min(max(iptvStartFloor, probeElapsed+4*time.Second), iptvStartMax)
}

type channelLimiter interface {
	BeginChannel(*core.User, core.ClientCaps, string) error
	EndChannel(string)
}

const directChannelPrefix = "direct:"

// DirectStream describes an ephemeral direct-URL live stream (such as a
// plugin library live tab item) tuned through the HLS passthrough/relay
// pipeline without registering a Live TV channel.
type DirectStream struct {
	ID         string
	Title      string
	URL        string
	Headers    map[string]string
	AllowCheck func(context.Context, *core.User) error
	Resolve    func(context.Context) (DirectStreamResult, error)
}

type session struct {
	archiveInput        bool // staged relay grants have a finite upstream
	catchUp             *catchUpSession
	outputProvider      *providerChange // direct DASH or HTTP input
	broadcastPolicy     broadcastViewerPolicy
	broadcast           *broadcastLease
	ephemeral           bool
	allowCheck          func(context.Context, *core.User) error
	direct              *directStreamState
	sniffedManifest     *cachedManifest
	inputURL            string // selected upstream, immutable once the tune is ready
	inputSource         StreamSource
	inputHeaders        http.Header
	firstMediaDelivered bool
	firstDeliveredAt    time.Time
	firstMediaByte      bool
	bytesServed         int64
	tuneStartedAt       time.Time
	startDeadline       time.Time // bounded post-probe startup window for Direct playout
	lastMediaWarning    time.Time
	consumed            bool
	mediaStarted        bool
	activeReads         atomic.Int32
	startupCancel       context.CancelFunc // immutable after publication; cancels I/O before locking
	view                core.PlaySession   // immutable monitoring snapshot, including pending tunes
	reserved            bool
	ready               bool
	relayInfo           *core.MediaInfo
	steadyRefused       string // why Steady refused this tune; Direct must not escalate back into it
	// owned is the Ether-owned client timeline of a Direct (passthrough or
	// relay) session; ServePlaylist and ServeRelaySegment read it.
	owned *ownedLease
	// sharedInput points to the shared relay's grant owner while this viewer
	// holds its lease. Input readers can resolve it while already holding mu.
	sharedInput atomic.Pointer[session]
	dash        core.DASHSession
	http        core.HTTPSession
	// httpStart is closed when an in-flight ensureHTTP StartHLS finishes.
	// Waiters re-check liveHTTP instead of starting a second encoder.
	httpStart chan struct{}
	// rejoinDone deduplicates background broadcast rejoin for a retained
	// session; rejoin runs under ss.ctx rather than a playlist request ctx.
	rejoinDone chan struct{}
	rejoinErr  error
	// mu guards the fields above and below it. It is never held across
	// Playback.Prepare/Failover, HLS SwapInput, programme lookups, probing
	// or upstream I/O: preparers work on a staged
	// snapshot (prepareMu serializes them) and publish under mu only after
	// validating generation. getSession/ServeSegment/close therefore stay
	// responsive while a tune or playlist preparation is in flight.
	mu sync.Mutex
	// generation is guarded by mu and bumped by close; a preparer that
	// snapshotted an older generation discards its staged result.
	generation int64
	// prepareMu serializes tune/playlist preparation for this session. It
	// is deliberately separate from mu so that slow preparation blocks
	// neither media serving nor close.
	prepareMu sync.Mutex
	// ctx is the session lifetime, cancelled by close; preparation I/O is
	// bound to it (bindContext) so Stop cancels a slow Prepare/probe.
	ctx    context.Context
	cancel context.CancelFunc
	// bound holds the cancels of live bindContext contexts. close cancels
	// them synchronously, so every bound context (a Playback.Failover or
	// Prepare in flight, including its internal retries) is cancelled
	// before Stop returns; context.AfterFunc alone runs in a new goroutine
	// and can lag.
	boundMu     sync.Mutex
	bound       map[uint64]context.CancelFunc
	boundSeq    uint64
	info        SessionInfo
	streamGrant string
	user        core.User
	caps        core.ClientCaps
	last        time.Time
	lastControl time.Time
	closed      bool
}

func (ss *session) setSniffedManifest(cm *cachedManifest) {
	if ss == nil {
		return
	}
	ss.mu.Lock()
	ss.sniffedManifest = cm
	direct := ss.direct
	ss.mu.Unlock()
	if direct != nil && cm != nil && cm.isMaster {
		direct.setManifest(cm)
	}
}

func (ss *session) sniffedManifestFor(raw string) *cachedManifest {
	if ss == nil || raw == "" {
		return nil
	}
	ss.mu.Lock()
	cm := ss.sniffedManifest
	direct := ss.direct
	ss.mu.Unlock()
	if direct != nil {
		if dm := direct.manifestFor(raw); dm != nil {
			return dm
		}
		// Never hand back a stale session copy if the direct stream was
		// invalidated on 403/410.
		if _, _, _, _, _, invalidated, _ := direct.current(); invalidated {
			return nil
		}
	}
	if cm != nil && cm.raw == raw {
		return cm
	}
	return nil
}

func (ss *session) cachedMasterFor(raw string) *cachedManifest {
	cm := ss.sniffedManifestFor(raw)
	if cm != nil && cm.isMaster {
		return cm
	}
	return nil
}

// bindContext derives a context cancelled when parent ends or when the
// session is closed, whichever comes first. A context bound after close
// began is returned already cancelled.
func (ss *session) bindContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if ss.ctx == nil {
		return ctx, cancel
	}
	ss.boundMu.Lock()
	if ss.ctx.Err() != nil {
		ss.boundMu.Unlock()
		cancel()
		return ctx, cancel
	}
	if ss.bound == nil {
		ss.bound = map[uint64]context.CancelFunc{}
	}
	ss.boundSeq++
	id := ss.boundSeq
	ss.bound[id] = cancel
	ss.boundMu.Unlock()
	// Service shutdown cancels ss.ctx without close; that path may lag.
	stop := context.AfterFunc(ss.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
		ss.boundMu.Lock()
		delete(ss.bound, id)
		ss.boundMu.Unlock()
	}
}

// live reports why I/O for ctx must not start: ctx itself ended, or close
// has begun (it cancels ss.ctx before anything else, so any effect of a
// Stop — such as a failover gate it freed — is ordered after this check).
func (ss *session) live(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ss.ctx != nil && ss.ctx.Err() != nil {
		return core.ErrNotFound
	}
	return nil
}

// cancelBound cancels every bound context; ss.ctx must already be
// cancelled so bindContext cannot register another one afterwards.
func (ss *session) cancelBound() {
	ss.boundMu.Lock()
	bound := ss.bound
	ss.bound = nil
	ss.boundMu.Unlock()
	for _, cancel := range bound {
		cancel()
	}
}

// startOwnedHLS ties startup to the request and the returned encoder to the
// session. Once startup completes, request cancellation must not end playback.
func (s *Service) startOwnedHLS(request context.Context, ss *session, req core.HLSRequest) (core.HLSSession, context.CancelFunc, error) {
	base := ss.ctx
	if base == nil {
		base = s.serviceContext()
	}
	if base == nil {
		base = context.Background()
	}
	lifetime, cancel := context.WithCancel(base)
	stop := context.AfterFunc(request, cancel)
	hls, err := s.transcoder().StartHLS(lifetime, req)
	stop()
	if errors.Is(err, context.Canceled) && request.Err() != nil {
		err = request.Err()
	}
	if err == nil && request.Err() != nil {
		err = request.Err()
	}
	if err == nil && lifetime.Err() != nil {
		err = lifetime.Err()
	}
	if err != nil {
		cancel()
		if hls != nil {
			hls.Close()
		}
		return nil, nil, err
	}
	return hls, cancel, nil
}

func (s *Service) clockNow() time.Time {
	if s != nil && s.o.Now != nil {
		return s.o.Now()
	}
	return time.Now()
}

func (s *Service) observeSession(p *core.PlaySession, ended bool, problem string) {
	if s == nil || s.o.ObserveSession == nil || p == nil || p.ID == "" {
		return
	}
	s.o.ObserveSession(p, ended, problem)
}

func (s *Service) sessionSnapshotLocked(ss *session, now time.Time) core.PlaySession {
	return s.sessionSnapshotLockedWithUsage(ss, now, nil)
}

func (s *Service) sessionSnapshotLockedWithUsage(ss *session, now time.Time, usage map[providerLoginKey]int) core.PlaySession {
	p := ss.view
	if archive := ss.catchUp; archive != nil {
		p.Decision = archive.info.Decision.Kind
		p.DecisionVideoCopy = archive.info.Decision.VideoCopy
		p.DecisionAudioCopy = archive.info.Decision.AudioCopy
		p.Info.Duration = archive.info.End.Sub(archive.info.Start)
	}
	if p.LiveChannel != nil {
		ch := *p.LiveChannel
		ch.Provider, ch.PreviousProvider = ss.servingProviderLocked().snapshot(usage)
		p.LiveChannel = &ch
	}
	p.Liveness = s.sessionLivenessLocked(ss, now).State
	p.LastActive = ss.last
	p.BytesServed = ss.bytesServed
	if ss.firstMediaDelivered {
		p.ViewerState = "playing"
		startedAt := ss.info.StartedAt
		deliveredAt := ss.firstDeliveredAt
		if deliveredAt.IsZero() {
			deliveredAt = now
		}
		var startTime time.Duration
		if !startedAt.IsZero() && !deliveredAt.IsZero() {
			startTime = max(0, deliveredAt.Sub(startedAt))
		}
		p.Experience = &core.PlayExperience{
			Requested:   true,
			RequestedAt: startedAt,
			Pictured:    true,
			PicturedAt:  deliveredAt,
			StartTime:   startTime,
		}
	}
	return p
}

func (ss *session) close(s *Service)        { ss.closeWithReason(s, false) }
func (ss *session) closeExpired(s *Service) { ss.closeWithReason(s, true) }

func (ss *session) closeWithReason(s *Service, expired bool) {
	defer s.onChannelSessionClosed(ss.info.Channel.ID)
	defer s.endPersonal(ss.info.Channel, ss.info.UserID)
	if ss.startupCancel != nil {
		ss.startupCancel()
	}
	if ss.cancel != nil {
		ss.cancel()
		ss.cancelBound()
	}
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return
	}
	ss.closed = true
	ss.generation++
	broadcast := ss.broadcast
	owned := ss.owned
	dash := ss.dash
	archive := ss.catchUp
	httpStream := ss.http
	reserved := ss.reserved
	ready, requested, delivered := ss.ready, ss.consumed, ss.firstMediaDelivered
	bytesServed := ss.bytesServed
	stopInfo := ss.info
	stopSrc := ss.inputSource
	now := s.clockNow()
	end := now
	if expired && !ss.last.IsZero() && ss.last.Before(end) {
		end = ss.last
	}
	snap := s.sessionSnapshotLocked(ss, end)
	ss.publishOwnedLocked(nil)
	ss.broadcast = nil
	ss.dash = nil
	ss.http = nil
	mode := sessionPlayoutModeLocked(ss)
	ss.catchUp = nil
	snap.LastActive = end
	ss.mu.Unlock()
	if broadcast != nil {
		broadcast.close()
	}
	s.revokeInput(ss)
	archive.close(s)
	owned.close()
	if dash != nil {
		dash.Close()
	}
	if httpStream != nil {
		httpStream.Close()
	}
	if reserved {
		if limiter, ok := s.o.Playback.(channelLimiter); ok {
			limiter.EndChannel(ss.info.ID)
		}
	}
	age := now.Sub(stopInfo.StartedAt)
	if ready {
		s.emitChannelStopped(stopInfo, stopSrc, age.Milliseconds(), bytesServed, mode)
	}
	if delivered {
		s.observeSession(&snap, true, "")
	}
	s.sessionLog(ss).Info("live session resources released", "ready", ready, "media_requested", requested, "media_delivered", delivered, "age", age, "viewer_reservation_released", reserved)
}

func (s *Service) Tune(ctx context.Context, u *core.User, channelID string, caps core.ClientCaps) (SessionInfo, error) {
	return s.TuneWithSession(ctx, u, channelID, caps, caps.ClientID)
}

// TuneWithSession preserves the client's playback-session identity independently
// of the server's stream, subscription and activity identities.
func (s *Service) TuneWithSession(ctx context.Context, u *core.User, channelID string, caps core.ClientCaps, clientSessionID string) (result SessionInfo, resultErr error) {
	if !s.Enabled() {
		return SessionInfo{}, core.ErrNotAvailable
	}
	started := time.Now()
	tuneStarted := s.clockNow()
	attemptID := liveSessionID()
	var admitted bool
	var tunedCh Channel
	var tunedProg *Programme
	var tunedSess *session
	log := s.logger().With("session", attemptID, "channel", channelID, "client", caps.ClientID, "product", caps.Product)
	log.Info("live tune started")
	defer func() {
		attrs := []any{"elapsed", time.Since(started)}
		if resultErr != nil {
			detail := Diagnose("tune", resultErr)
			attrs = append(attrs, detail.LogAttrs()...)
			if detail.Code == "cancelled" {
				log.Debug("live tune cancelled", attrs...)
			} else {
				log.Info("live tune failed", attrs...)
				if admitted && u != nil && tunedCh.ID != "" && !(tunedSess != nil && errors.Is(resultErr, core.ErrNotFound) && tunedSess.isClosed()) {
					var snap core.PlaySession
					if tunedSess != nil {
						tunedSess.mu.Lock()
						snap = s.sessionSnapshotLocked(tunedSess, s.clockNow())
						tunedSess.mu.Unlock()
					} else {
						title := tunedCh.Name
						if tunedProg != nil && tunedProg.Item.Title != "" {
							title = tunedProg.Item.Title
						}
						snap = core.PlaySession{
							ID: attemptID, PlaybackGroup: attemptID,
							UserID: u.ID, Username: u.Username,
							ClientID: caps.ClientID, Client: caps,
							TitleName: title, StartedAt: tuneStarted,
							Decision: core.DecisionRemux, DecisionVideoCopy: true, DecisionAudioCopy: true,
							DecisionReason: "Live channel playout",
							Origin:         "Live TV",
							LiveChannel:    &core.LiveChannel{ID: tunedCh.ID, Name: tunedCh.Name, Number: tunedCh.Number},
						}
					}
					msg := detail.Error()
					snap.ViewerState = ""
					snap.Experience = &core.PlayExperience{
						Requested:    true,
						RequestedAt:  snap.StartedAt,
						StartFailure: msg,
					}
					s.observeSession(&snap, true, msg)
				}
			}
		} else {
			log.Info("live tune ready", append(attrs, "source", result.Channel.SourceID, "analyzed", result.MediaInfo != nil)...)
		}
	}()

	if clientSessionID == "" {
		clientSessionID = caps.ClientID
	}
	if u == nil || (s.o.CanPlay != nil && !s.o.CanPlay(u, caps.Remote, s.o.Now())) {
		return SessionInfo{}, core.ErrUnauthorized
	}
	if s.o.ConfirmAdmission != nil {
		confirmed, err := s.o.ConfirmAdmission(ctx, u, caps.Remote, s.o.Now())
		if err != nil {
			return SessionInfo{}, err
		}
		u = confirmed
	}
	if caps.Remote && s.o.RemoteUploadCap != nil {
		if limit, fromBudget := s.o.RemoteUploadCap(ctx, attemptID, true, true, caps.MaxBitrateKbps, 0); fromBudget && limit > 0 && (caps.MaxBitrateKbps == 0 || limit < caps.MaxBitrateKbps) {
			caps.MaxBitrateKbps = limit
		}
	}
	ctx, policy, err := s.withPolicy(ctx, u)
	if err != nil {
		return SessionInfo{}, err
	}
	c, err := s.Channel(ctx, channelID)
	if err != nil {
		return SessionInfo{}, err
	}
	if c.Kind == "virtual" {
		// Cold guide preparation and source startup share the first-playlist
		// budget. A tune immediately after restart may need more than the
		// five seconds allowed to an ordinary guide request.
		var cancelStartup context.CancelFunc
		ctx, cancelStartup = context.WithTimeout(ctx, 20*time.Second)
		defer cancelStartup()
		ctx = context.WithValue(ctx, virtualTuneGuideKey{}, true)
	}
	c = s.personalChannel(ctx, c, u)
	ps, err := s.programmes(ctx, c, s.o.Now(), s.o.Now().Add(time.Second))
	if err != nil {
		return SessionInfo{}, err
	}
	var p *Programme
	if len(ps) > 0 {
		p = &ps[0]
	}
	if !s.allowed(ctx, u, c, p) {
		return SessionInfo{}, core.ErrUnauthorized
	}
	admitted = true
	tunedCh = c
	tunedProg = p
	// Apply the viewer override only to this tune's channel copy. A channel
	// already requiring conversion remains converted for every viewer.
	if c.Kind == "iptv" && policy.Transcode {
		c.Transcode = true
	}
	if c.Kind == "virtual" && p == nil {
		return SessionInfo{}, core.ErrNotAvailable
	}
	var eventProgs map[string]Programme
	if c.Kind == "iptv" {
		var ep *Programme
		c, ep, eventProgs, err = s.expandChannelForTune(ctx, c)
		if err != nil {
			return SessionInfo{}, err
		}
		if ep != nil {
			p = ep
		}
		tunedCh = c
		tunedProg = p
	}
	// A single client device watches one live channel at a time, except tiles
	// belonging to the same multiview group. A non-group tune supersedes the
	// entire group; re-tuning a tile is a handover that keeps the old tile
	// running until the replacement is ready.
	callerHandover := core.LiveHandover(ctx)
	handoverIDs := s.supersedeBeforeTune(u.ID, caps, attemptID, callerHandover)
	var playbackSessions []*core.PlaySession
	if s.o.Playback != nil {
		playbackSessions = s.o.Playback.Sessions()
	}
	s.mu.Lock()
	count := s.activeStreamCountLocked(u.ID, caps.MultiviewGroup, caps.ClientID, playbackSessions, handoverIDs)
	limit := u.StreamLimit
	if s.o.StreamLimit != nil {
		limit = s.o.StreamLimit(u)
	}
	if limit > 0 && count >= limit {
		s.mu.Unlock()
		return SessionInfo{}, core.ErrStreamLimit
	}
	// A provider that allows N connections can carry N different channels;
	// viewers of a channel already playing share its connection.
	if c.Kind == "iptv" && c.SourceID != "" {
		if max := s.sourceConnectionLimit(c.SourceID); max > 0 {
			playing := map[string]bool{}
			for _, other := range s.sessions {
				if !other.archiveInput && other.info.Channel.SourceID == c.SourceID {
					playing[other.info.Channel.ID] = true
				}
			}
			if active := len(playing) + s.inputRelay.taps.externalCount(c.SourceID); !playing[c.ID] && active >= max {
				s.mu.Unlock()
				s.emitProviderLimitReached(c.SourceID, active, max, c.ID)
				return SessionInfo{}, s.providerConnectionCapError(c.SourceID, max, active, ErrProviderBusy)
			}
		}
	}
	s.nextSubscription++
	ss := &session{
		tuneStartedAt: started,
		info: SessionInfo{
			ID: attemptID, SubscriptionID: s.nextSubscription,
			ActivityID: liveSessionID(), ClientSessionID: clientSessionID,
			UserID: u.ID, Channel: c, Programme: p, StartedAt: s.o.Now(),
		},
		streamGrant: randomID(), user: *u, caps: caps, last: s.o.Now(),
	}
	base := s.serviceContext()
	if base == nil {
		base = context.Background()
	}
	ss.ctx, ss.cancel = context.WithCancel(base)
	// Probing and post-probe stream startup enforce their own bounded budgets
	// so a dead input still fails fast while a slower route that answered its
	// probe has room to open. startupCancel aborts whichever stage is running.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ss.startupCancel = cancel
	ss.view = core.PlaySession{ID: ss.info.ID, PlaybackGroup: ss.info.ID, UserID: u.ID, Username: u.Username, ClientID: caps.ClientID, Client: caps, TitleName: c.Name, StartedAt: ss.info.StartedAt, Decision: core.DecisionRemux, DecisionVideoCopy: true, DecisionAudioCopy: true, DecisionReason: "Live channel playout", Origin: "Live TV"}
	ss.view.LiveChannel = &core.LiveChannel{ID: c.ID, Name: c.Name, Number: c.Number}
	if p != nil {
		ss.view.Key = p.Item.Key
		ss.view.TitleName = p.Item.Title
		ss.view.Info.Duration = p.End.Sub(p.Start)
	}
	tunedSess = ss
	s.sessions[ss.info.ID] = ss
	s.noteWatchedLocked(c.ID)
	if u != nil && u.ID > 0 {
		s.noteViewerWatchedLocked(u.ID, c.ID)
	}
	s.noteSourceUsedLocked(c.SourceID)
	s.mu.Unlock()
	if c.SourceID != "" && s.hasOnDemand.Load() {
		s.wakeSourceRefresh()
	}
	s.notifySessionsChanged()
	s.beginPersonal(c, u)
	if limiter, ok := s.o.Playback.(channelLimiter); ok {
		if joined, ok := s.o.Playback.(interface {
			JoinChannel(*core.User, core.ClientCaps, string, string) error
		}); ok && s.useBroadcast(c) {
			group := "broadcast-" + c.ID
			if c.Personal != nil {
				group += "-viewer-" + strconv.FormatInt(u.ID, 10)
			}
			err = joined.JoinChannel(u, caps, ss.info.ID, group)
			if errors.Is(err, core.ErrStreamLimit) && s.broadcasts.yieldIdle(nil) {
				deadline := time.NewTimer(2 * time.Second)
				tick := time.NewTicker(25 * time.Millisecond)
				defer deadline.Stop()
				defer tick.Stop()
			retryJoin:
				for {
					select {
					case <-ctx.Done():
						break retryJoin
					case <-deadline.C:
						break retryJoin
					case <-tick.C:
						err = joined.JoinChannel(u, caps, ss.info.ID, group)
						if !errors.Is(err, core.ErrStreamLimit) {
							break retryJoin
						}
					}
				}
			}
		} else {
			err = limiter.BeginChannel(u, caps, ss.info.ID)
		}
		if err != nil {
			s.Stop(ctx, ss.info.ID, u)
			return SessionInfo{}, err
		}
		ss.mu.Lock()
		if ss.closed {
			ss.mu.Unlock()
			limiter.EndChannel(ss.info.ID)
			return SessionInfo{}, core.ErrNotFound
		}
		ss.reserved = true
		ss.mu.Unlock()
	}

	// Input and output startup perform their I/O without ss.mu;
	// close can therefore cancel it through the session context instead of
	// waiting behind up to ~10s of upstream work.
	if s.useBroadcast(c) {
		err = s.startBroadcast(ctx, ss)
		if errors.Is(err, errSteadyRefused) {
			// A refused tune is an ordinary direct session: it holds its own
			// viewer reservation instead of a share of the channel output.
			err = s.reserveDirectViewer(u, caps, ss)
			if err == nil {
				err = s.startIPTVSession(ctx, ss)
			}
			if err == nil {
				err = s.awaitOwnedStart(ctx, ss)
			}
		}
		if err != nil {
			s.Stop(ctx, ss.info.ID, u)
			return SessionInfo{}, s.wrapProviderConnectionCap(c.SourceID, err)
		}
	} else {
		err = s.startIPTVSession(ctx, ss)
		if err == nil {
			err = s.awaitOwnedStart(ctx, ss)
		}
		if err != nil {
			s.Stop(ctx, ss.info.ID, u)
			return SessionInfo{}, s.wrapProviderConnectionCap(c.SourceID, err)
		}
	}
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return SessionInfo{}, core.ErrNotFound
	}
	ss.ready = true
	if matched, ok := eventProgs[ss.inputSource.ID]; ok {
		ss.info.Programme = &matched
		ss.view.TitleName = matched.Item.Title
		if d := matched.End.Sub(matched.Start); d > 0 {
			ss.view.Info.Duration = d
		}
	}
	info := ss.info
	ss.servingProviderLocked().attachSession(&info, u.IsOwner, nil)
	tunedSrc := ss.inputSource
	tunedUser := ss.user
	tunedCaps := ss.caps
	mode := sessionPlayoutModeLocked(ss)
	if ss.relayInfo != nil {
		info.MediaInfo = ss.relayInfo
	}
	ss.mu.Unlock()
	if !callerHandover {
		s.finishHandover(handoverIDs, attemptID)
	}
	s.emitChannelTuned(info, tunedSrc, tunedUser, tunedCaps, mode)
	info.Channel.StreamURL = ""
	info.Channel.redactArchiveSource()
	info.Channel.StreamSources = nil
	return info, nil
}

// TuneDirectStream tunes an ephemeral direct-URL live stream (such as a plugin
// library live tab item) through Ether's HLS passthrough and relay pipeline
// without requiring Live TV channels to exist or registering one in Live TV.
func (s *Service) TuneDirectStream(ctx context.Context, u *core.User, ds DirectStream, caps core.ClientCaps, clientSessionID string) (result SessionInfo, resultErr error) {
	if !validHTTP(ds.URL) {
		return SessionInfo{}, core.ErrNotAvailable
	}
	started := time.Now()
	tuneStarted := s.clockNow()
	attemptID := liveSessionID()
	title := strings.TrimSpace(ds.Title)
	if title == "" {
		title = "Live stream"
	}
	var admitted bool
	var tunedCh Channel
	var tunedSess *session
	log := s.logger().With("session", attemptID, "direct_live", ds.ID, "client", caps.ClientID, "product", caps.Product)
	log.Info("direct live tune started")
	defer func() {
		attrs := []any{"elapsed", time.Since(started)}
		if resultErr != nil {
			detail := Diagnose("tune", resultErr)
			attrs = append(attrs, detail.LogAttrs()...)
			if detail.Code == "cancelled" {
				log.Debug("direct live tune cancelled", attrs...)
			} else {
				log.Info("direct live tune failed", attrs...)
				if admitted && u != nil && tunedCh.ID != "" && !(tunedSess != nil && errors.Is(resultErr, core.ErrNotFound) && tunedSess.isClosed()) {
					var snap core.PlaySession
					if tunedSess != nil {
						tunedSess.mu.Lock()
						snap = s.sessionSnapshotLocked(tunedSess, s.clockNow())
						tunedSess.mu.Unlock()
					} else {
						snap = core.PlaySession{
							ID: attemptID, PlaybackGroup: attemptID,
							UserID: u.ID, Username: u.Username,
							ClientID: caps.ClientID, Client: caps,
							TitleName: title, StartedAt: tuneStarted,
							Decision: core.DecisionRemux, DecisionVideoCopy: true, DecisionAudioCopy: true,
							DecisionReason: "Plugin live stream",
							Origin:         "Live TV",
							LiveChannel:    &core.LiveChannel{ID: tunedCh.ID, Name: tunedCh.Name},
						}
					}
					msg := detail.Error()
					snap.ViewerState = ""
					snap.Experience = &core.PlayExperience{
						Requested:    true,
						RequestedAt:  snap.StartedAt,
						StartFailure: msg,
					}
					s.observeSession(&snap, true, msg)
				}
			}
		} else {
			log.Info("direct live tune ready", append(attrs, "analyzed", result.MediaInfo != nil)...)
		}
	}()

	if clientSessionID == "" {
		clientSessionID = caps.ClientID
	}
	if u == nil || (s.o.CanPlay != nil && !s.o.CanPlay(u, caps.Remote, s.o.Now())) {
		return SessionInfo{}, core.ErrUnauthorized
	}
	if s.o.ConfirmAdmission != nil {
		confirmed, err := s.o.ConfirmAdmission(ctx, u, caps.Remote, s.o.Now())
		if err != nil {
			return SessionInfo{}, err
		}
		u = confirmed
	}
	if ds.AllowCheck != nil {
		if err := ds.AllowCheck(ctx, u); err != nil {
			return SessionInfo{}, err
		}
	}
	if caps.Remote && s.o.RemoteUploadCap != nil {
		if limit, fromBudget := s.o.RemoteUploadCap(ctx, attemptID, true, true, caps.MaxBitrateKbps, 0); fromBudget && limit > 0 && (caps.MaxBitrateKbps == 0 || limit < caps.MaxBitrateKbps) {
			caps.MaxBitrateKbps = limit
		}
	}

	c := Channel{
		ID:      directChannelPrefix + attemptID,
		Name:    title,
		Kind:    "iptv",
		Enabled: true,
		StreamSources: []StreamSource{{
			ID:      "direct",
			Name:    title,
			URL:     ds.URL,
			Headers: CleanInputHeaders(ds.Headers),
			Enabled: true,
		}},
	}
	admitted = true
	tunedCh = c

	callerHandover := core.LiveHandover(ctx)
	handoverIDs := s.supersedeBeforeTune(u.ID, caps, attemptID, callerHandover)
	var playbackSessions []*core.PlaySession
	if s.o.Playback != nil {
		playbackSessions = s.o.Playback.Sessions()
	}
	s.mu.Lock()
	count := s.activeStreamCountLocked(u.ID, caps.MultiviewGroup, caps.ClientID, playbackSessions, handoverIDs)
	limit := u.StreamLimit
	if s.o.StreamLimit != nil {
		limit = s.o.StreamLimit(u)
	}
	if limit > 0 && count >= limit {
		s.mu.Unlock()
		return SessionInfo{}, core.ErrStreamLimit
	}
	s.nextSubscription++
	base := s.serviceContext()
	if base == nil {
		base = context.Background()
	}
	ss := &session{
		tuneStartedAt: started,
		ephemeral:     true,
		allowCheck:    ds.AllowCheck,
		direct:        newDirectStreamState(base, ds.URL, ds.Headers, ds.Resolve),
		info: SessionInfo{
			ID: attemptID, SubscriptionID: s.nextSubscription,
			ActivityID: liveSessionID(), ClientSessionID: clientSessionID,
			UserID: u.ID, Channel: c, StartedAt: s.o.Now(),
		},
		streamGrant: randomID(), user: *u, caps: caps, last: s.o.Now(),
	}
	ss.ctx, ss.cancel = context.WithCancel(base)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ss.startupCancel = cancel
	ss.view = core.PlaySession{
		ID: ss.info.ID, PlaybackGroup: ss.info.ID,
		UserID: u.ID, Username: u.Username,
		ClientID: caps.ClientID, Client: caps,
		TitleName: title, StartedAt: ss.info.StartedAt,
		Decision: core.DecisionRemux, DecisionVideoCopy: true, DecisionAudioCopy: true,
		DecisionReason: "Plugin live stream", Origin: "Live TV",
		LiveChannel: &core.LiveChannel{ID: c.ID, Name: c.Name},
	}
	tunedSess = ss
	s.sessions[ss.info.ID] = ss
	s.mu.Unlock()
	s.notifySessionsChanged()

	if limiter, ok := s.o.Playback.(channelLimiter); ok {
		if err := limiter.BeginChannel(u, caps, ss.info.ID); err != nil {
			s.Stop(ctx, ss.info.ID, u)
			return SessionInfo{}, err
		}
		ss.mu.Lock()
		if ss.closed {
			ss.mu.Unlock()
			limiter.EndChannel(ss.info.ID)
			return SessionInfo{}, core.ErrNotFound
		}
		ss.reserved = true
		ss.mu.Unlock()
	}

	err := s.startIPTVSession(ctx, ss)
	if err == nil {
		err = s.awaitOwnedStart(ctx, ss)
	}
	if err != nil {
		s.Stop(ctx, ss.info.ID, u)
		return SessionInfo{}, err
	}
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return SessionInfo{}, core.ErrNotFound
	}
	ss.ready = true
	info := ss.info
	ss.servingProviderLocked().attachSession(&info, u.IsOwner, nil)
	tunedSrc := ss.inputSource
	tunedUser := ss.user
	tunedCaps := ss.caps
	mode := sessionPlayoutModeLocked(ss)
	if ss.relayInfo != nil {
		info.MediaInfo = ss.relayInfo
	}
	ss.mu.Unlock()
	if !callerHandover {
		s.finishHandover(handoverIDs, attemptID)
	}
	s.emitChannelTuned(info, tunedSrc, tunedUser, tunedCaps, mode)
	info.Channel.StreamURL = ""
	info.Channel.redactArchiveSource()
	info.Channel.StreamSources = nil
	return info, nil
}

func (s *Service) getSession(ctx context.Context, id string, u *core.User) (*session, error) {
	s.mu.Lock()
	ss := s.sessions[id]
	s.mu.Unlock()
	if ss == nil {
		return nil, core.ErrNotFound
	}
	ctx, cancel := ss.bindContext(ctx)
	defer cancel()
	if u == nil || ss.info.UserID != u.ID || s.o.CanPlay != nil && !s.o.CanPlay(u, ss.caps.Remote, s.o.Now()) {
		return nil, core.ErrUnauthorized
	}
	ss.mu.Lock()
	shared := ss.broadcast != nil
	ephemeral := ss.ephemeral
	allowCheck := ss.allowCheck
	ss.mu.Unlock()
	if ephemeral {
		if allowCheck != nil {
			if err := allowCheck(ctx, u); err != nil {
				return nil, err
			}
		}
	} else if shared {
		ctx, err := s.broadcastPolicyContext(ctx, ss, u)
		if err != nil {
			return nil, err
		}
		if !s.broadcastTagsAllowed(ctx, ss, nil) {
			return nil, core.ErrUnauthorized
		}
	} else {
		c, err := s.Channel(ctx, ss.info.Channel.ID)
		if err != nil {
			return nil, err
		}
		c = s.personalChannel(ctx, c, u)
		ps, err := s.programmes(ctx, c, s.o.Now(), s.o.Now().Add(time.Second))
		if err != nil {
			return nil, err
		}
		var p *Programme
		if len(ps) > 0 {
			p = &ps[0]
		}
		if !s.allowed(ctx, u, c, p) {
			return nil, core.ErrUnauthorized
		}
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.closed {
		return nil, core.ErrNotFound
	}
	if !ss.ready {
		return nil, core.ErrNotAvailable
	}
	ss.user = *u
	return ss, nil
}

// Media consumption renews the lease and records playback evidence. Playlist
// polls do neither.
func (s *Service) touchMedia(ss *session) {
	ss.mu.Lock()
	first := !ss.consumed
	ss.consumed = true
	ss.mediaStarted = true
	ss.last = s.o.Now()
	// A shared passthrough timeline may have left the source this tune
	// selected; its producer records the health of what actually plays.
	shared := ss.owned != nil && ss.owned.relay == nil
	src := ss.inputSource
	ss.mu.Unlock()
	if first {
		s.sessionLog(ss).Info("live media first requested", "since_tune", s.o.Now().Sub(ss.info.StartedAt))
	}
	if !shared {
		s.recordSourceHealth(ss.info.Channel.ID, src, nil, true)
	}
}
func (s *Service) Session(ctx context.Context, id string, u *core.User) (SessionInfo, error) {
	ss, err := s.getSession(ctx, id, u)
	if err != nil {
		return SessionInfo{}, err
	}
	// Snapshot under mu, then run the programme lookup (DB/upstream)
	// without the lock so getSession/ServeSegment/close stay responsive
	// while it is in flight.
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return SessionInfo{}, core.ErrNotFound
	}
	gen := ss.generation
	info := ss.info
	if ss.relayInfo != nil {
		info.MediaInfo = ss.relayInfo
	}
	provider := ss.servingProviderLocked()
	channel := ss.info.Channel
	ephemeral := ss.ephemeral
	ss.mu.Unlock()
	owner := u != nil && u.IsOwner
	var usage map[providerLoginKey]int
	if owner && provider != nil {
		usage = s.localLoginUsage()
	}
	provider.attachSession(&info, owner, usage)
	info.Channel.StreamURL = ""
	info.Channel.redactArchiveSource()
	info.Channel.StreamSources = nil
	if info.Channel.Archive != nil {
		info.Channel.Archive = &ChannelArchive{Days: info.Channel.Archive.Days, Mode: info.Channel.Archive.Mode}
	}
	if ephemeral {
		return info, nil
	}
	lookupCtx, cancel := ss.bindContext(ctx)
	defer cancel()
	ps, err := s.programmes(lookupCtx, channel, s.o.Now(), s.o.Now().Add(time.Second))
	// Discard the snapshot if the session died while the lookup ran.
	ss.mu.Lock()
	stale := ss.closed || ss.generation != gen
	ss.mu.Unlock()
	if stale {
		return SessionInfo{}, core.ErrNotFound
	}
	if err == nil && len(ps) > 0 {
		info.Programme = &ps[0]
	}
	return info, err
}
func (s *Service) Sessions(ctx context.Context, u *core.User) []SessionInfo {
	s.mu.Lock()
	var ids []string
	for id := range s.sessions {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	out := []SessionInfo{}
	for _, id := range ids {
		if info, err := s.Session(ctx, id, u); err == nil {
			out = append(out, info)
		}
	}
	return out
}
func (s *Service) Stop(ctx context.Context, id string, u *core.User) error {
	s.mu.Lock()
	ss := s.sessions[id]
	if ss == nil {
		s.mu.Unlock()
		return core.ErrNotFound
	}
	if u == nil || ss.info.UserID != u.ID && !u.IsOwner {
		s.mu.Unlock()
		return core.ErrUnauthorized
	}
	delete(s.sessions, id)
	s.mu.Unlock()
	ss.close(s)
	s.notifySessionsChanged()
	return nil
}

// startIPTVSession runs startIPTV on a staged copy of ss, so probing,
// upstream fetches and relay startup never hold ss.mu. On success the
// staged selection is published under ss.mu (including the input-relay
// grant, which is re-keyed from the stage to ss); if the session closed or
// changed generation meanwhile, the staged relay/grant are discarded.
func (s *Service) startIPTVSession(ctx context.Context, ss *session) error {
	ss.prepareMu.Lock()
	defer ss.prepareMu.Unlock()
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return core.ErrNotFound
	}
	gen := ss.generation
	stage := &session{info: ss.info, user: ss.user, caps: ss.caps, ctx: ss.ctx, steadyRefused: ss.steadyRefused, ephemeral: ss.ephemeral, direct: ss.direct}
	ss.mu.Unlock()

	ctx, cancel := ss.bindContext(ctx)
	defer cancel()
	if err := ss.live(ctx); err != nil {
		return err
	}
	if err := s.startIPTV(ctx, stage); err != nil {
		return err
	}
	ss.mu.Lock()
	if ss.closed || ss.generation != gen {
		if stage.broadcast != nil {
			stage.broadcast.close()
		}
		ss.mu.Unlock()
		stage.owned.close()
		s.revokeInput(stage)
		return core.ErrNotFound
	}
	ss.inputURL, ss.inputSource, ss.inputHeaders = stage.inputURL, stage.inputSource, stage.inputHeaders
	ss.sniffedManifest = stage.sniffedManifest
	ss.info.MediaInfo, ss.info.ActiveSourceID = stage.info.MediaInfo, stage.info.ActiveSourceID
	ss.relayInfo = stage.relayInfo
	ss.publishOwnedLocked(stage.owned)
	ss.startDeadline = stage.startDeadline
	ss.broadcast = stage.broadcast
	s.transferInput(stage, ss)
	if ss.owned != nil && ss.owned.relay != nil && !ss.owned.relay.shared {
		ss.owned.relay.bind(ss)
	}
	ss.mu.Unlock()
	return nil
}

func writeStreamError(w http.ResponseWriter, err error) {
	var bedtime *core.ErrBedtime
	if errors.As(err, &bedtime) {
		http.Error(w, bedtime.Error(), http.StatusForbidden)
		return
	}
	code := http.StatusBadGateway
	switch err {
	case core.ErrNotFound:
		code = 404
	case core.ErrUnauthorized:
		code = 403
	case core.ErrStreamLimit:
		code = 429
	case core.ErrNotAvailable:
		w.Header().Set("Retry-After", "1")
		code = 503
	}
	http.Error(w, http.StatusText(code), code)
}

// ServePlaylist writes the shared broadcast view or the owner's unchanged
// IPTV passthrough/relay view when continuous output is disabled.
func (s *Service) ServePlaylist(w http.ResponseWriter, r *http.Request, id string, u *core.User) {
	ss, err := s.getSession(r.Context(), id, u)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	ss.mu.Lock()
	ss.consumed = true
	shared := ss.broadcast != nil
	archive := ss.catchUp
	ss.mu.Unlock()
	if archive != nil {
		s.serveCatchUp(w, r, ss, archive, nil)
		return
	}
	if shared {
		s.serveBroadcastPlaylist(w, r, ss)
		return
	}
	s.serveOwnedPlaylist(w, r, ss)
}
func withAuth(raw, grant string) string { return streamgrant.GrantURL(raw, grant) }

// Failover is owned by the shared input planner, which retries without changing
// the viewer's output clock.
func (s *Service) Failover(ctx context.Context, id string, u *core.User) error {
	ss, err := s.getSession(ctx, id, u)
	if err != nil {
		return err
	}
	ss.mu.Lock()
	lease := ss.broadcast
	ss.mu.Unlock()
	if lease == nil {
		return core.ErrNotAvailable
	}
	lease.broadcast.requestRetry()
	return nil
}

// startIPTVSource recognizes HLS without assuming URL suffixes (many owners
// use signed extensionless URLs). Other HTTP streams are remuxed through
// ffmpeg, whose existing input layer reconnects on network errors with bounded
// delay. Probing runs under sniffBudget/probeBudget; once the input proves
// alive, relay/timeline startup gets its own bounded startDeadline.
func (s *Service) startIPTVSource(ctx context.Context, ss *session, sniffBudget, probeBudget time.Duration, deferSilent bool) error {
	if ss.closed {
		return core.ErrNotFound
	}
	probeStarted := time.Now()
	probeCtx, stopProbe := context.WithTimeout(ctx, probeBudget)
	defer stopProbe()
	sniffCtx, stopSniff := context.WithTimeout(probeCtx, sniffBudget)
	defer stopSniff()
	armStartBudget := func() (context.Context, context.CancelFunc) {
		ss.startDeadline = time.Now().Add(iptvSourceStartBudget(time.Since(probeStarted)))
		return context.WithDeadline(ctx, ss.startDeadline)
	}
	c := ss.info.Channel
	c.StreamURL = ss.inputURL
	analyze := len(ss.caps.VideoCodecs) > 0 || len(ss.caps.AudioCodecs) > 0 || ss.caps.ProbeLiveWithoutCodecs || (ss.caps.MultiviewGroup != "" && ss.caps.MaxHeight > 0) || broadcastUsesFMP4(ss.caps)
	probe := analyze || c.Transcode || ss.caps.ProbeLiveInput
	// Codec analysis overlaps the playlist prefix read. The first tune of a
	// channel still waits on the probe; a retune inside liveProbeTTL does not.
	probeCh, probeCancel := s.startEarlyProbe(probeCtx, ss, probe)
	if probeCancel != nil {
		defer probeCancel()
	}
	// The sniff joins the input's provider connection, which the probe and
	// ffmpeg then read from too.
	prefix, ctype, err := s.sniffInput(sniffCtx, ss, 7)
	if err != nil {
		return Diagnose("input_sniff", err)
	}
	isHLS := strings.HasPrefix(strings.TrimSpace(string(prefix)), "#EXTM3U") || strings.Contains(strings.ToLower(ctype), "mpegurl")
	var info *core.MediaInfo
	if isHLS && !c.Transcode {
		if s.transcoder() != nil && probe {
			probeWaitCtx := probeCtx
			var waitCancel context.CancelFunc
			if ss.caps.BestEffortLiveStart {
				// This caller has a < 1s startup budget. Wait briefly for a
				// cached or fast probe, then proceed with HLS passthrough if
				// live ffprobe is still sampling segments.
				probeWaitCtx, waitCancel = context.WithTimeout(probeCtx, 350*time.Millisecond)
				defer waitCancel()
			}
			info, err = s.takeEarlyProbe(probeWaitCtx, ss, probeCh)
			if analyze && !ss.caps.BestEffortLiveStart {
				if err != nil {
					return Diagnose("probe", err)
				}
				if info == nil || info.Video() == nil {
					return &TuneError{Code: "no_video", Stage: "probe", cause: core.ErrNotAvailable}
				}
			}
			if err == nil && info != nil && info.Video() != nil {
				copy := *info
				copy.Duration = 0
				info = &copy
				ss.info.MediaInfo = info
				if deferSilent && !liveInputHasAudio(info) {
					return errSilentInput
				}
				if analyze {
					// A playlist is a transport, not evidence that its codecs are
					// playable. MPEG-2/MP2 HLS needs conversion on Apple TV too,
					// and clients that only play fMP4 cannot consume MPEG-TS HLS.
					dec := s.transcoder().Decide(*info, ss.caps, core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1, SubtitleStream: -1})
					needsFMP4 := broadcastUsesFMP4(ss.caps) && !strings.EqualFold(info.Container, "mp4") && !strings.EqualFold(info.Container, "fmp4")
					if !dec.VideoCopy || !dec.AudioCopy || info.Video().Codec == "hevc" || needsFMP4 {
						startCtx, stopStart := armStartBudget()
						defer stopStart()
						return s.startIPTVRelay(startCtx, ss, *info)
					}
				}
			}
		}
		_, stopStart := armStartBudget()
		stopStart()
		ss.owned = s.acquirePassthrough(ss)
		s.sessionLog(ss).Info("live playback path selected", "path", "hls_passthrough", "analyzed", ss.info.MediaInfo != nil)
		return nil
	}
	if s.transcoder() == nil {
		return core.ErrNotAvailable
	}
	info, err = s.takeEarlyProbe(probeCtx, ss, probeCh)
	if err != nil {
		return Diagnose("probe", err)
	}
	if info == nil {
		return &TuneError{Code: "no_video", Stage: "probe", cause: core.ErrNotAvailable}
	}
	copy := *info
	copy.Duration = 0
	ss.info.MediaInfo = &copy
	if deferSilent && !liveInputHasAudio(info) {
		return errSilentInput
	}
	startCtx, stopStart := armStartBudget()
	defer stopStart()
	return s.startIPTVRelay(startCtx, ss, copy)
}

type probeResult struct {
	info *core.MediaInfo
	err  error
}

// startEarlyProbe begins codec analysis before the prefix read when this
// tune will need it. Direct HLS without a codec decision does not probe.
func (s *Service) startEarlyProbe(ctx context.Context, ss *session, need bool) (<-chan probeResult, context.CancelFunc) {
	if !need || s.transcoder() == nil {
		return nil, nil
	}
	ch := make(chan probeResult, 1)
	probeCtx, cancel := context.WithCancel(ctx)
	go func() {
		info, err := s.probeSessionInput(probeCtx, ss)
		ch <- probeResult{info, err}
	}()
	return ch, cancel
}

func (s *Service) takeEarlyProbe(ctx context.Context, ss *session, ch <-chan probeResult) (*core.MediaInfo, error) {
	if ch == nil {
		return s.probeSessionInput(ctx, ss)
	}
	select {
	case r := <-ch:
		return r.info, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// waitOwnedRelayStart waits for a relay timeline's first playlist. If the
// outer budget expires while at least one segment is already pending, it lets
// the timeline's bounded startup depth wait finish rather than failing a
// live stream that is about to publish.
func waitOwnedRelayStart(parent, deadlineCtx context.Context, lease *ownedLease) error {
	err := lease.tl.waitStarted(deadlineCtx)
	if errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil && lease.tl.hasPending() {
		gctx, cancel := context.WithTimeout(parent, lease.tl.startWaitRemaining()+2*ownedTick)
		defer cancel()
		err = lease.tl.waitStarted(gctx)
	}
	return err
}

// awaitOwnedStart holds the tune until a Direct session's owned timeline has
// its first playlist. An Apple player hangs a live item for good when the
// first playlist response takes a target duration, so the wait for the
// first segments belongs to the tune, never to that response. Silo's
// passthrough start keeps its sub-second budget (700 ms); relay sessions
// wait for their startup depth so the first playlist request returns ready.
func (s *Service) awaitOwnedStart(ctx context.Context, ss *session) error {
	ss.mu.Lock()
	lease := ss.owned
	deadline := ss.startDeadline
	ss.mu.Unlock()
	if lease == nil {
		return nil
	}
	parentCtx := ctx
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	var err error
	if lease.relay != nil {
		err = waitOwnedRelayStart(parentCtx, ctx, lease)
	} else {
		wait := ctx
		if ss.caps.BestEffortLiveStart {
			var cancel context.CancelFunc
			wait, cancel = context.WithTimeout(ctx, 700*time.Millisecond)
			defer cancel()
		}
		err = lease.tl.waitStarted(wait)
		if errors.Is(err, errNeedsRelay) {
			if lease, err = s.passthroughToRelay(ctx, ss); err == nil {
				err = waitOwnedRelayStart(parentCtx, ctx, lease)
			}
		} else if err != nil && wait.Err() != nil && ctx.Err() == nil {
			return nil // The playlist request waits for the rest
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// The tune budget passed without one segment from the source.
		return &TuneError{Code: "timeout", Stage: "owned_start", cause: err}
	}
	return err
}

// serveOwnedPlaylist writes a Direct session's owned live playlist. The
// first request waits for the first segments (bounded), as players reject an
// empty live playlist.
func (s *Service) serveOwnedPlaylist(w http.ResponseWriter, r *http.Request, ss *session) {
	ss.mu.Lock()
	lease := ss.owned
	ss.mu.Unlock()
	if lease == nil {
		writeStreamError(w, core.ErrNotFound)
		return
	}
	// Include the source target for passthrough inputs with longer segments.
	// AVPlayer can hang the item permanently when its first playlist takes
	// a whole target duration. Keep the wait below that, including startup.
	lease.tl.mu.Lock()
	target := max(lease.tl.target, lease.tl.targetFloor, s.latencyPolicy().SegmentDuration())
	lease.tl.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), target/2)
	defer cancel()
	var err error
	if lease.relay != nil {
		err = waitOwnedRelayStart(r.Context(), ctx, lease)
	} else {
		err = lease.tl.waitStarted(ctx)
		if errors.Is(err, errNeedsRelay) {
			rctx, rcancel := context.WithTimeout(r.Context(), iptvStartFloor)
			defer rcancel()
			lease, err = s.passthroughToRelay(rctx, ss)
			if err == nil {
				err = waitOwnedRelayStart(r.Context(), rctx, lease)
			}
		}
	}
	if err != nil {
		if r.Context().Err() == nil {
			s.sessionLog(ss).Warn("owned live playlist unavailable", Diagnose("playlist", err).LogAttrs()...)
		}
		http.Error(w, "IPTV playout unavailable", http.StatusBadGateway)
		return
	}
	prefix := "/livetv/sessions/" + ss.info.ID + "/0/"
	body, window := lease.tl.renderSnapshot(func(seq int64, ext string) string {
		return withAuth(prefix+"relay/"+strconv.FormatInt(seq, 10)+"."+ext, ss.streamGrant)
	}, func(name string) string {
		return withAuth(prefix+"resource/"+name, ss.streamGrant)
	}, ss.caps.Protocol == "vela")
	if ss.caps.Protocol == "vela" {
		writeLiveAnchor(w, window)
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

// passthroughToRelay moves a passthrough session whose provider variants
// cannot ride one owned timeline (demuxed audio renditions) onto the relay,
// before its first playlist is served.
func (s *Service) passthroughToRelay(ctx context.Context, ss *session) (*ownedLease, error) {
	ss.prepareMu.Lock()
	defer ss.prepareMu.Unlock()
	ss.mu.Lock()
	lease, info := ss.owned, ss.info.MediaInfo
	ss.mu.Unlock()
	if lease != nil && lease.relay != nil {
		return lease, nil
	}
	if s.transcoder() == nil {
		return nil, core.ErrNotAvailable
	}
	if info == nil {
		probed, err := s.probeSessionInput(ctx, ss)
		if err != nil {
			return nil, err
		}
		if probed == nil || probed.Video() == nil {
			return nil, &TuneError{Code: "no_video", Stage: "probe", cause: core.ErrNotAvailable}
		}
		copy := *probed
		copy.Duration = 0
		info = &copy
	}
	dec, cut, _ := s.relayPlan(ss, *info)
	next, err := s.startOwnedRelay(ctx, ss, *info, relayCaps(ss.caps), dec, cut)
	if err != nil {
		return nil, Diagnose("relay_start", err)
	}
	output := liveRelayInfo(*info, dec)
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		next.close()
		return nil, core.ErrNotFound
	}
	old := ss.owned
	ss.relayInfo, ss.info.MediaInfo = &output, info
	ss.publishOwnedLocked(next)
	ss.mu.Unlock()
	old.close()
	s.sessionLog(ss).Info("live playback path selected", "path", "relay", "reason", "provider variants need remux")
	return next, nil
}

// ServeRelaySegment serves segment n of the session's owned timeline (or of
// its shared broadcast).
func (s *Service) ServeRelaySegment(w http.ResponseWriter, r *http.Request, id string, n int, u *core.User) {
	ss, err := s.getSession(r.Context(), id, u)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	ss.mu.Lock()
	shared := ss.broadcast != nil
	fmp4 := broadcastUsesFMP4(ss.caps)
	lease := ss.owned
	archive := ss.catchUp
	ss.mu.Unlock()
	if archive != nil {
		s.serveCatchUp(w, r, ss, archive, &n)
		return
	}
	if shared {
		if n == transcode.InitSegment {
			s.serveBroadcastInit(w, r, ss)
			return
		}
		if n < 0 {
			writeStreamError(w, core.ErrNotFound)
			return
		}
		if fmp4 || strings.HasSuffix(strings.ToLower(r.URL.Path), ".m4s") {
			s.serveBroadcastFMP4(w, r, ss, int64(n))
			return
		}
		s.serveBroadcastTS(w, r, ss, int64(n))
		return
	}
	if lease == nil || n < 0 {
		writeStreamError(w, core.ErrNotFound)
		return
	}
	seg, err := lease.tl.segment(r.Context(), int64(n))
	if err != nil {
		writeStreamError(w, err)
		return
	}
	if r.Method != http.MethodHead {
		s.touchMedia(ss)
	}
	ctype := "video/mp2t"
	if seg.ext == "m4s" {
		ctype = "video/mp4"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(seg.data)))
	w.Header().Set("Cache-Control", "private, max-age=60")
	armWriteDeadline(w)
	observed := s.observeMedia(w, ss)
	defer observed.done()
	observed.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = observed.Write(seg.data)
	}
}

// TranscodeFor is unused by shared broadcasts: decoder retries are owned by
// their planner and never swap a viewer's encoder input.
func (s *Service) TranscodeFor(string) (core.HLSSession, bool) { return nil, false }

// sessionLivenessLocked evaluates ss under the shared core liveness rule.
// Caller holds ss.mu.
func (s *Service) sessionLivenessLocked(ss *session, now time.Time) core.SessionLiveness {
	started := ss.info.StartedAt
	if started.IsZero() {
		started = ss.last
	}
	in := core.SessionLivenessInput{
		Closed:         ss.closed,
		StartedAt:      started,
		OpenedAt:       started,
		IsLive:         true,
		ActiveReads:    ss.activeReads.Load(),
		AbandonedAfter: 20 * time.Second,
		IdleTimeout:    30 * time.Second,
	}
	if ss.mediaStarted {
		in.LastMediaAt = ss.last
		in.MediaBytes = max(int64(1), ss.bytesServed)
	}
	liveness := core.EvaluateSessionLiveness(in, now)
	// Player observations renew the lease independently of media reads.
	if !ss.closed && !ss.lastControl.IsZero() && now.Sub(ss.lastControl) <= in.IdleTimeout {
		liveness.Expired = false
	}
	return liveness
}

// ViewingSessions reports one trusted reservation unit per live viewer. It is
// used by ordinary playback's limit accounting, including HLS passthrough
// viewers that do not own an ffmpeg/source session.
func (s *Service) ViewingSessions() []*core.PlaySession {
	s.mu.Lock()
	list := make([]*session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		list = append(list, ss)
	}
	s.mu.Unlock()
	now := s.o.Now()
	out := make([]*core.PlaySession, 0, len(list))
	var usage map[providerLoginKey]int
	if len(list) > 0 {
		usage = s.localLoginUsage()
	}
	for _, ss := range list {
		var p core.PlaySession
		if ss.mu.TryLock() {
			p = s.sessionSnapshotLockedWithUsage(ss, now, usage)
			ss.mu.Unlock()
		} else {
			// Startup I/O holds ss.mu: never block monitoring.
			p = ss.view
			p.Liveness = core.SessionLivenessStarting
		}
		out = append(out, &p)
	}
	return out
}

func shouldSupersedeLiveSession(old *session, userID int64, caps core.ClientCaps) bool {
	if old.info.UserID != userID || old.caps.ClientID != caps.ClientID {
		return false
	}
	// A non-multiview tune on the same device supersedes every prior session on
	// that device, including all tiles of an active multiview group.
	if caps.MultiviewGroup == "" {
		return true
	}
	// Sibling tiles in the same multiview group coexist; re-tuning the same
	// tile replaces only that tile, and starting a different group replaces
	// any prior sessions on the device.
	if old.caps.MultiviewGroup != caps.MultiviewGroup {
		return true
	}
	return old.caps.MultiviewTile == caps.MultiviewTile
}

func isHandoverLiveSession(old *session, userID int64, caps core.ClientCaps, callerHandover bool) bool {
	if old.info.UserID != userID || caps.ClientID == "" || old.caps.ClientID != caps.ClientID {
		return false
	}
	if caps.MultiviewGroup != "" && old.caps.MultiviewGroup == caps.MultiviewGroup && old.caps.MultiviewTile == caps.MultiviewTile {
		return true
	}
	return callerHandover && caps.MultiviewGroup == "" && old.caps.MultiviewGroup == ""
}

func (s *Service) supersedeBeforeTune(userID int64, caps core.ClientCaps, attemptID string, callerHandover bool) map[string]bool {
	if caps.ClientID == "" {
		return nil
	}
	var superseded []*session
	var handoverIDs map[string]bool
	s.mu.Lock()
	for id, old := range s.sessions {
		if !shouldSupersedeLiveSession(old, userID, caps) {
			continue
		}
		if isHandoverLiveSession(old, userID, caps, callerHandover) {
			if handoverIDs == nil {
				handoverIDs = map[string]bool{}
			}
			handoverIDs[id] = true
			continue
		}
		delete(s.sessions, id)
		superseded = append(superseded, old)
	}
	s.mu.Unlock()
	for _, old := range superseded {
		s.sessionLog(old).Info("live session superseded", "replacement_session", attemptID)
		old.close(s)
	}
	if len(superseded) > 0 {
		s.notifySessionsChanged()
	}
	return handoverIDs
}

func (s *Service) finishHandover(handoverIDs map[string]bool, attemptID string) {
	if len(handoverIDs) == 0 {
		return
	}
	var replaced []*session
	s.mu.Lock()
	for id := range handoverIDs {
		if old, ok := s.sessions[id]; ok {
			delete(s.sessions, id)
			replaced = append(replaced, old)
		}
	}
	s.mu.Unlock()
	for _, old := range replaced {
		s.sessionLog(old).Info("live session superseded", "replacement_session", attemptID)
		old.close(s)
	}
	if len(replaced) > 0 {
		s.notifySessionsChanged()
	}
}

func (s *Service) activeStreamCountLocked(userID int64, incomingGroup, clientID string, playbackSessions []*core.PlaySession, handoverIDs map[string]bool) int {
	count := 0
	mvGroups := map[string]bool{}
	for _, ss := range s.sessions {
		if ss.info.UserID != userID || handoverIDs[ss.info.ID] {
			continue
		}
		if g := ss.caps.MultiviewGroup; g != "" {
			if incomingGroup != "" && g == incomingGroup {
				continue
			}
			if !mvGroups[g] {
				mvGroups[g] = true
				count++
			}
			continue
		}
		count++
	}
	for _, ps := range playbackSessions {
		if core.SessionCountsForLimit(ps) && ps.UserID == userID && ps.PlaybackGroup == "" {
			if clientID != "" && ps.ClientID == clientID {
				continue
			}
			count++
		}
	}
	return count
}

func (s *Service) sourceConnectionsInUse(sourceID string) int {
	if sourceID == "" {
		return 0
	}
	playing := map[string]bool{}
	s.mu.Lock()
	for _, other := range s.sessions {
		if !other.archiveInput && other.info.Channel.SourceID == sourceID {
			playing[other.info.Channel.ID] = true
		}
	}
	for chID := range s.broadcastProviders[sourceID] {
		playing[chID] = true
	}
	s.mu.Unlock()
	return max(len(playing), s.inputRelay.taps.open(sourceID)) + s.inputRelay.taps.externalCount(sourceID)
}

func (s *Service) providerConnectionCapError(sourceID string, maxConns, inUse int, cause error) error {
	if cause == nil {
		cause = ErrProviderBusy
	}
	name := ""
	if sourceID != "" {
		if e, err := s.sourceEntry(s.serviceContext(), sourceID); err == nil {
			name = strings.TrimSpace(e.name)
			if maxConns <= 0 {
				maxConns = e.limit
			}
		}
	}
	if maxConns > 0 && inUse < maxConns {
		inUse = maxConns
	}
	return &core.ProviderConnectionCapError{
		Provider: name,
		Max:      maxConns,
		InUse:    inUse,
		Cause:    cause,
	}
}

func (s *Service) wrapProviderConnectionCap(sourceID string, err error) error {
	if err == nil || !errors.Is(err, ErrProviderBusy) {
		return err
	}
	var existing *core.ProviderConnectionCapError
	if errors.As(err, &existing) {
		return err
	}
	maxConns := s.sourceConnectionLimit(sourceID)
	inUse := s.sourceConnectionsInUse(sourceID)
	return s.providerConnectionCapError(sourceID, maxConns, inUse, err)
}
