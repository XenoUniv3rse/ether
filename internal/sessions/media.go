package sessions

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/mediasafety"
	"github.com/braedonsaunders/ether/internal/streamgrant"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func waitReadRetirement(reads []*core.ReadRetirement) {
	for _, tracker := range reads {
		if tracker != nil {
			tracker.Interrupt()
		}
	}
	for _, tracker := range reads {
		if tracker != nil {
			<-tracker.Seal()
		}
	}
}

func (b *Broker) mintEncoderInput(ctx context.Context, playID string) (core.EncoderInputLease, error) {
	inputs, ok := b.deps.Playback.(core.PlaybackEncoderInputs)
	if !ok {
		return nil, nil
	}
	owned, ok := b.deps.Playback.(core.OwnedSourcePlayback)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	source, ok := owned.SourceRetirement(playID)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	return inputs.ReserveEncoderInput(ctx, playID, source)
}

func (b *Broker) StartStream(ctx context.Context, a frontend.SessionAccess, id string, req frontend.StreamRequest) (string, error) {
	if (req.AudioStream != nil || req.SubtitleStream != nil) && req.Format != frontend.StreamHLS && req.Format != frontend.StreamDASH {
		return "", core.ErrNotAvailable
	}
	if req.PrepareOnly && (a.Protocol != "vela" || req.Format != frontend.StreamHLS) {
		return "", core.ErrNotAvailable
	}
	if req.Position < 0 || req.SegmentDuration < 0 {
		return "", core.ErrNotAvailable
	}
	if path, handled, err := b.forwardNeutralControl(ctx, a, id, "stream", frontend.Timeline{}, req); handled {
		return path, err
	}
	s, _, err := b.access(ctx, a, id)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	select {
	case b.startSlots <- struct{}{}:
		defer func() { <-b.startSlots }()
	default:
		return "", &frontend.SessionBusyError{RetryAfter: time.Second}
	}
	gateKey := ownerGateKey(s.public.ViewerID, s.clientID)
	if s.live && s.multiviewGroup != "" {
		gateKey = tileGateKey(s.public.ViewerID, s.clientID, s.multiviewGroup, s.multiviewTile)
	}
	unlock, err := b.gates.Acquire(ctx, gateKey)
	if err != nil {
		return "", err
	}
	defer unlock()
	s, viewer, err := b.access(ctx, a, id)
	if err != nil {
		return "", err
	}
	if viewer.ID != s.public.ViewerID {
		viewer, err = b.deps.ActiveViewer(ctx, s.public.ViewerID)
	}
	if err != nil || !b.currentMediaPolicy(ctx, viewer, s) {
		return "", core.ErrUnauthorized
	}
	if s.restored {
		if req.AudioStream != nil || req.SubtitleStream != nil {
			return "", core.ErrNotAvailable
		}
		if req.Format != s.format || req.Position != s.streamPosition || req.SegmentDuration != s.segmentDuration {
			return "", core.ErrNotAvailable
		}
		if hs, ok := b.transcodeSession(id); !ok || hs == nil {
			return "", core.ErrNotAvailable
		}
		if err = b.saveNeutralBinding(ctx, id, false); err != nil {
			return "", err
		}
		return "/sessions/media/" + s.public.ID + "/" + s.grant + "/master.m3u8", nil
	}
	if remote, ok := b.deps.Playback.(interface {
		RevalidateSession(context.Context, string) error
	}); ok {
		if err := remote.RevalidateSession(ctx, s.playID); err != nil {
			return "", err
		}
	}
	if s.originalFailure {
		return "", frontend.ErrVersionUnavailable
	}
	if req.ExpectedOriginalRevision != nil {
		revision := ""
		if s.originalReplaced {
			revision = s.sourceETag
		}
		if *req.ExpectedOriginalRevision != revision {
			return "", frontend.ErrSourceGone
		}
	}

	if s.sourcePinned {
		ps, ok := b.deps.Playback.Session(s.playID)
		if !ok || ps == nil || sourceRevision(ps) != s.sourceETag {
			return "", frontend.ErrSourceGone
		}
	}
	if req.Format != frontend.StreamDirect && req.Format != frontend.StreamHLS && req.Format != frontend.StreamDASH && req.Format != frontend.StreamHTTP {
		return "", core.ErrNotAvailable
	}
	if !req.PrepareOnly && (s.format == "" || req.Position != s.streamPosition) {
		if noter, ok := b.deps.Playback.(interface{ NoteStartPosition(string, time.Duration) }); ok {
			noter.NoteStartPosition(s.playID, req.Position)
		}
	}
	path := "/sessions/media/" + s.public.ID + "/" + s.grant + "/"
	if s.live {
		if req.AudioStream != nil || req.SubtitleStream != nil {
			return "", core.ErrNotAvailable
		}
		if req.Format != frontend.StreamHLS {
			return "", core.ErrNotAvailable
		}
		if req.MaxHeight != nil {
			return b.replanLive(ctx, viewer, s, *req.MaxHeight)
		}
		return path + "live/index.m3u8", nil
	}
	if req.Format == frontend.StreamDirect {
		// Infuse Static=true is Jellyfin DirectStream: serve the original
		// file. Remux is the same bytes. A transcode plan still serves the
		// file when the client asked for DirectPlay or DirectStream; Infuse
		// opens /Videos/{id}/stream?Static=true instead of waiting on HLS.
		if !s.opts.DirectPlay && !s.opts.DirectStream {
			return "", core.ErrNotAvailable
		}
		if b.deps.Transcoder != nil {
			b.transcodeStop(id)
		}
		if entry, ok := b.bindings.Get(id); ok {
			entry.Started = false
			b.bindings.Put(entry)
		}
		b.mu.Lock()
		if live := b.sessions[id]; live != nil {
			live.format = req.Format
			live.adaptiveHLS = false
			live.adaptiveMaxBitrate = 0
			live.streamPosition = req.Position
			if req.Position != s.streamPosition {
				live.originalPosition = nil
			}
			live.public.MediaPath = path + "file"
			live.public.PlayMethod = "DirectPlay"
		}
		b.mu.Unlock()
		// The original file is what the client now plays, whatever the
		// plan said; say so even if an encoder ran for this session before.
		ps, _ := b.deps.Playback.Session(s.playID)
		b.recordPlaybackDecision(s.playID, core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: ps == nil || !ps.Info.AudioOnly(), AudioCopy: true})
		if !req.LazyStart && !req.PrepareOnly {
			b.mu.Lock()
			if live := b.sessions[id]; live != nil {
				live.trickplayStarted = true
			}
			b.mu.Unlock()
			b.ensureTrickplay(ctx, s, req.Position)
		}
		// The file URL is an in-memory grant. ServeMedia rechecks the viewer
		// and the current policy on every byte. The durable delivery row is
		// published beside that, not in front of the first byte.
		sealed := *s
		go func() {
			c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			if err := b.bindDeliveryAuthority(c, &sealed, ps); err != nil && b.deps.OnEventError != nil {
				b.deps.OnEventError(err)
			}
		}()
		return path + "file", nil
	}
	if b.deps.Transcoder == nil {
		return "", core.ErrNotAvailable
	}
	adaptive := req.AdaptiveHLS && req.Format == frontend.StreamHLS && (s.public.Protocol == "vela" || s.public.Protocol == "jellyfin" || s.public.Protocol == "emby")
	if req.PrepareOnly && !req.AdaptiveHLS {
		adaptive = s.adaptiveHLS
		req.AdaptiveMaxBitrate = s.adaptiveMaxBitrate
	}
	if adaptive && req.AdaptiveMaxBitrate > 0 && req.AdaptiveMaxBitrate < 1000 {
		return "", &frontend.NoCompatiblePathError{RejectedPaths: []frontend.RejectedPlaybackPath{{Path: "hls", Reasons: []string{"bitrate_limit"}}}}
	}
	qualityChanged := req.MaxHeight != nil && *req.MaxHeight >= 0 && (req.Format == frontend.StreamHLS || req.Format == frontend.StreamDASH)
	tracksChanged := req.AudioStream != nil || req.SubtitleStream != nil || qualityChanged
	reencode := req.ReencodeVideo && s.opts.DirectStream
	if !tracksChanged && !reencode && s.adaptiveHLS == adaptive && s.adaptiveMaxBitrate == req.AdaptiveMaxBitrate && (s.format == req.Format || (s.public.Protocol == "vela" && s.format == frontend.StreamDirect && req.Format == frontend.StreamHLS)) && s.streamPosition == req.Position && s.segmentDuration == req.SegmentDuration {
		if hs, ok := b.transcodeSession(id); ok && hs != nil {
			var mediaPath string
			if req.Format == frontend.StreamDASH {
				if _, ok := hs.(core.DASHSession); ok {
					mediaPath = path + "manifest.mpd"
				}
			} else if req.Format == frontend.StreamHTTP {
				if _, ok := hs.(core.HTTPSession); ok {
					ext := s.httpExt
					if ext == "" {
						ext = httpStreamExt(s.decision.Container, s.decision.AudioCodec)
					}
					mediaPath = path + "stream." + ext
				}
			} else {
				mediaPath = path + "master.m3u8"
			}
			if mediaPath != "" {
				if !req.PrepareOnly && s.format != req.Format {
					b.mu.Lock()
					if live := b.sessions[id]; live != nil {
						live.format = req.Format
						live.public.MediaPath = mediaPath
					}
					b.mu.Unlock()
				}
				if entry, ok := b.bindings.Get(id); ok && entry.Started && !req.PrepareOnly {
					b.recordPlaybackDecision(s.playID, entry.Decision)
				}
				if req.Format != frontend.StreamHTTP && !req.LazyStart && !req.PrepareOnly {
					b.mu.Lock()
					if live := b.sessions[id]; live != nil {
						live.trickplayStarted = true
					}
					b.mu.Unlock()
					b.ensureTrickplay(ctx, s, req.Position)
				}
				return mediaPath, nil
			}
		}
	}
	ps, ok := b.deps.Playback.Session(s.playID)
	if !ok || ps == nil {
		return "", core.ErrNotAvailable
	}

	dec := s.decision
	if tracksChanged || reencode || dec.Reason == "original_runtime" || ((req.Format == frontend.StreamHLS || req.Format == frontend.StreamHTTP) && dec.Kind == core.DecisionDirectPlay) {
		options := s.opts
		if reencode {
			// No copy: the client refused the copied video. Later restarts
			// (seeks) keep encoding.
			options.DirectStream = false
		}
		if qualityChanged {
			cap := *req.MaxHeight
			options.MaxHeight = cap
			source := 0
			if v := ps.Info.Video(); v != nil {
				source = v.Height
			}
			if cap > 0 && source > cap {
				// A copy cannot be smaller: encode to the picked height.
				if options.DirectStream {
					s.qualityCapped = true
				}
				options.DirectStream = false
			} else if s.qualityCapped {
				options.DirectStream = true
				s.qualityCapped = false
			}
		}
		if req.AudioStream != nil {
			if !validObservedTrack(ps.Info, "audio", *req.AudioStream) {
				return "", core.ErrNotAvailable
			}
			options.AudioStream = *req.AudioStream
		}
		if req.SubtitleStream != nil {
			if !validObservedTrack(ps.Info, "subtitle", *req.SubtitleStream) {
				return "", core.ErrNotAvailable
			}
			options.SubtitleStream = *req.SubtitleStream
			// A new selection replaces the burned one: switching from a burned
			// PGS track to a text track or to Off must stop burning it.
			options.BurnSubtitles = false
			if req.BurnImageSubtitles {
				for _, st := range ps.Info.Streams {
					if st.Type == "subtitle" && st.Index == *req.SubtitleStream && transcode.IsImageSubtitle(st.Codec) {
						options.BurnSubtitles = true
					}
				}
			}
		}
		// Track restarts must package the selected source tracks, never silently
		// return the original file's default audio selection.
		options.DirectPlay = false
		d, e := b.decideProfile(ps, frontend.PlanRequest{Viewer: viewer, Device: s.public.Device, Protocol: s.public.Protocol, Item: s.public.Item, Caps: s.caps, Intent: frontend.PlayIntent{Options: options, AllowTranscoding: s.allowTranscoding}})
		if e != nil {
			return "", e
		}
		// The server, not the app, turned off direct play for this restart (and
		// direct stream when the player refused the copy). Activity must not
		// read that as "the app's own quality settings".
		if entry, ok := b.bindings.Get(id); ok {
			if entry.Requested.Options.DirectPlay {
				d.Reason = strings.Replace(d.Reason, "direct play not allowed by client", "server stream restarted", 1)
			}
			if entry.Requested.Options.DirectStream && reencode {
				d.Reason = strings.Replace(d.Reason, "direct stream not allowed by client", "player refused the copied stream", 1)
			}
		}
		dec = d
		s.opts = options
	}
	proto := core.ProtocolHLS
	if req.Format == frontend.StreamDASH {
		proto = core.ProtocolDASH
		if dec.Kind == core.DecisionDirectPlay {
			if ps.Info.Video() != nil && s.caps.EffectiveAlwaysEncodeVideo() && (s.live || !b.videoOff(s.caps)) {
				options := s.opts
				options.DirectPlay = false
				options.DirectStream = false
				options.Protocol = core.ProtocolDASH
				dec = b.deps.Transcoder.Decide(ps.Info, s.caps, options)
			} else {
				dec.Kind, dec.VideoCopy, dec.AudioCopy = core.DecisionRemux, !ps.Info.AudioOnly(), true
			}
		}
	} else if req.Format == frontend.StreamHTTP {
		proto = core.ProtocolHTTP
		var allowed bool
		dec, allowed = b.httpOutputDecision(ps, s.caps, s.opts, s.allowTranscoding, dec)
		if !allowed {
			if !s.live && b.videoOff(s.caps) && decisionEncodesVideo(dec, ps.Info) {
				return "", &frontend.NoCompatiblePathError{
					RejectedPaths: []frontend.RejectedPlaybackPath{{Path: string(req.Format), Reasons: []string{reasonVideoTranscodingOff}}},
					Cause:         core.ErrVideoTranscodingDisabled,
				}
			}
			return "", &frontend.NoCompatiblePathError{RejectedPaths: []frontend.RejectedPlaybackPath{{Path: string(req.Format), Reasons: []string{outputRejection(dec, ps.Info, s.allowTranscoding)}}}}
		}
	} else {
		if len(textSubtitleStreams(string(s.public.Protocol), s.caps, ps.Info)) > 0 && dec.Kind == core.DecisionDirectPlay {
			// Negotiate the real segmented container/codecs, rather than
			// assuming a direct-file codec can be copied into MPEG-TS.
			options := s.opts
			options.DirectPlay = false
			options.SubtitleStream = -1
			options.BurnSubtitles = false
			dec = b.deps.Transcoder.Decide(ps.Info, s.caps, options)
		}
		var allowed bool
		dec, allowed = b.hlsOutputDecision(ps, s.caps, s.opts, s.allowTranscoding, dec)
		if !allowed {
			if !s.live && b.videoOff(s.caps) && decisionEncodesVideo(dec, ps.Info) {
				return "", &frontend.NoCompatiblePathError{
					RejectedPaths: []frontend.RejectedPlaybackPath{{Path: string(req.Format), Reasons: []string{reasonVideoTranscodingOff}}},
					Cause:         core.ErrVideoTranscodingDisabled,
				}
			}
			return "", &frontend.NoCompatiblePathError{RejectedPaths: []frontend.RejectedPlaybackPath{{Path: string(req.Format), Reasons: []string{outputRejection(dec, ps.Info, s.allowTranscoding)}}}}
		}
	}
	if !s.live && b.videoOff(s.caps) && decisionEncodesVideo(dec, ps.Info) {
		return "", &frontend.NoCompatiblePathError{
			RejectedPaths: []frontend.RejectedPlaybackPath{{Path: string(req.Format), Reasons: []string{reasonVideoTranscodingOff}}},
			Cause:         core.ErrVideoTranscodingDisabled,
		}
	}
	if s.allowTranscoding != nil && !*s.allowTranscoding && decisionEncodes(dec, ps.Info) {
		return "", &frontend.NoCompatiblePathError{RejectedPaths: []frontend.RejectedPlaybackPath{{Path: string(req.Format), Reasons: []string{outputRejection(dec, ps.Info, s.allowTranscoding)}}}}
	}
	if !profileDeliveryAllowed(s.caps.Profiles, dec, ps.Info, string(req.Format)) {
		return "", &frontend.NoCompatiblePathError{RejectedPaths: []frontend.RejectedPlaybackPath{{Path: string(req.Format), Reasons: []string{outputRejection(dec, ps.Info, s.allowTranscoding)}}}}
	}
	if dec.Kind == core.DecisionRemux && !s.opts.DirectStream {
		return "", &frontend.NoCompatiblePathError{RejectedPaths: []frontend.RejectedPlaybackPath{{Path: string(req.Format), Reasons: []string{outputRejection(dec, ps.Info, s.allowTranscoding)}}}}
	}
	// A combined multi-episode file starts at the requested episode's
	// segment: the client's position is episode-relative, the source's
	// timeline is not. DASH spans the whole file whatever the offset, and
	// a live channel's position is already in source time.
	startAt := req.Position
	var firstSegmentAt time.Duration
	if req.FullTimeline && proto == core.ProtocolHLS && !s.live {
		// The playlist starts at the title's (or episode's) beginning; the
		// client seeks to its resume point, so encode from there first.
		startAt = 0
		if req.Position > 0 {
			firstSegmentAt = ps.SourcePosition(req.Position)
		}
	}
	if proto != core.ProtocolDASH && !s.live {
		startAt = ps.SourcePosition(startAt)
	}
	request := core.HLSRequest{WaitForEncoder: !req.PrepareOnly, EncoderViewer: viewer.Username, FirstSegmentAt: firstSegmentAt, LazyStart: req.LazyStart, Diagnostics: req.Continuous, Continuous: req.Continuous, RequireIndependentProof: b.policy(s.public.Protocol).DistributedBindings, SessionID: id, ReleaseID: ps.SourceReleaseID(), InputURL: b.deps.Playback.InputURL(s.playID), NoVideoTranscode: !s.live && b.videoOff(s.caps), Info: ps.Info, Decision: dec, StartOffset: startAt, SegmentDuration: req.SegmentDuration, Protocol: proto}
	if req.Continuous && s.public.Protocol == "vela" {
		variants, _, e := b.qualityPlan(ctx, s, viewer)
		if e != nil {
			return "", e
		}
		request.QualityRungs = true
		request.QualityVariants = make([]core.QualityVariant, 0, len(variants))
		for _, variant := range variants {
			request.QualityVariants = append(request.QualityVariants, variant)
			request.QualityRungIDs = append(request.QualityRungIDs, variant.Rung.ID)
		}
	}
	if adaptive && ps.Info.Video() != nil {
		request.AdaptiveHLS = true
		request.AdaptiveEncodeTop = s.public.Protocol == "vela" && !s.live
		// The upload allocation is an admission-time ceiling. Budget the
		// peak of the base rather than the source's average bitrate.
		demand := 0
		if s.caps.Remote && dec.VideoCopy {
			demand = transcode.HLSCopyPeakKbps(ps.Info, req.SegmentDuration)
		}
		request.AdaptiveMaxKbps = stricterLimit(b.qualityBitrateCap(ctx, s, viewer, demand), req.AdaptiveMaxBitrate/1000)
		request.LazyStart, request.Continuous, request.QualityRungs = true, true, true
		switch {
		case s.allowTranscoding != nil && !*s.allowTranscoding || b.videoOff(s.caps):
			request.AdaptiveUnavailable = "transcoding_disabled"
		default:
			base, shared := transcode.AdaptiveAudio(ps.Info, dec, s.caps)
			if !shared || !profileDeliveryAllowed(s.caps.Profiles, base, ps.Info, "hls") {
				request.AdaptiveUnavailable = "shared_audio_unavailable"
				break
			}
			variants, limits := b.qualityVariants(ps.Info, base, s, s.caps, request.AdaptiveMaxKbps, true)
			if len(variants) == 0 {
				request.AdaptiveUnavailable = "no_compatible_rungs"
				for _, limit := range limits {
					if limit.Reason == "encoder_hevc_unavailable" || limit.Reason == "dynamic_range_unavailable" {
						request.AdaptiveUnavailable = limit.Reason
						break
					}
				}
				break
			}
			request.Decision = base
			for _, variant := range variants {
				if n := slices.Index(request.QualityRungIDs, variant.Rung.ID); n >= 0 {
					request.QualityVariants[n] = variant
				} else {
					request.QualityVariants = append(request.QualityVariants, variant)
					request.QualityRungIDs = append(request.QualityRungIDs, variant.Rung.ID)
				}
			}
		}
	}
	if request.Continuous {
		// A continuous playback client declares its decoder set. Optional
		// encoded rungs must also respect its session's transcoding policy.
		request.LadderVideoCodecs = []string{}
		if (s.allowTranscoding == nil || *s.allowTranscoding) && (s.live || !b.videoOff(s.caps)) {
			request.LadderVideoCodecs = append(request.LadderVideoCodecs, s.caps.VideoCodecs...)
		}
	}
	if s.caps.NoVideoTranscode && !s.live && request.LadderVideoCodecs == nil {
		// This viewer's video is never re-encoded: offer the primary rung only.
		request.LadderVideoCodecs = []string{}
	}
	if s.public.Protocol == "jellyfin" && dec.VideoCopy && !s.live && !adaptive {
		// Jellyfin apps play a copied original alone, as Jellyfin serves it
		// (owner decision, 0.3.21). Their Media3 players pick the first
		// variant by an initial bandwidth guess, so they started on the 360p
		// rung and, with the original advertised at its true peak, rarely
		// climbed back to it. Apple players start on the first entry and keep
		// the encoded fallback rungs on the other surfaces (8a3d68cef).
		request.LadderVideoCodecs = []string{}
	}
	request.TextSubtitleStreams = textSubtitleStreams(string(s.public.Protocol), s.caps, ps.Info)
	if len(request.TextSubtitleStreams) > 0 {
		request.TextSubtitleSource = sourceRevision(ps)
	}
	b.transcodeStop(id)
	var hs core.HLSSession
	var lease core.EncoderInputLease
	if remote := b.deps.DistributedHLS; remote != nil && !request.QualityRungs && len(request.TextSubtitleStreams) == 0 && remote.Supports(request) {
		hs, err = remote.StartDistributedHLS(context.WithoutCancel(ctx), viewer.ID, s.playID, sourceRevision(ps), request)
	} else {
		if remote := b.deps.DistributedHLS; remote != nil && remote.Allocated(id) {
			return "", core.ErrNotAvailable
		}
		var leaseErr error
		lease, leaseErr = b.mintEncoderInput(ctx, s.playID)
		if leaseErr != nil {
			return "", leaseErr
		}
		request.InputLease = lease
		hs, err = b.deps.Transcoder.StartHLS(ctx, request)
	}

	if err != nil || hs == nil {
		if lease != nil {
			lease.SealInput()
		}
		if err != nil {
			return "", b.recordEncoderFailure(s.playID, err)
		}
		b.transcodeStop(id)
		return "", core.ErrNotAvailable
	}
	// StartHLS owns a detached lifetime, but a request canceled during startup
	// must not leave an encoder and permit behind. Close also prevents a
	// concurrently starting encoder from surviving shutdown.
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if err := ctx.Err(); err != nil || closed {
		b.transcodeStop(id)
		if err != nil {
			return "", err
		}
		return "", core.ErrNotAvailable
	}
	if req.Format == frontend.StreamDASH {
		if _, ok := hs.(core.DASHSession); !ok {
			b.transcodeStop(id)
			return "", core.ErrNotAvailable
		}
	}
	if req.Format == frontend.StreamHTTP {
		if _, ok := hs.(core.HTTPSession); !ok {
			b.transcodeStop(id)
			return "", core.ErrNotAvailable
		}
	}
	if request.AdaptiveHLS {
		if diagnostics, ok := hs.(interface{ AdaptiveHLSUnavailable() string }); ok && b.deps.Logger != nil {
			if reason := diagnostics.AdaptiveHLSUnavailable(); reason != "" {
				b.deps.Logger.Debug("adaptive HLS unavailable", "session", id, "reason", reason)
			}
		}
		if local, ok := hs.(core.AdaptiveHLSSession); ok {
			if rungs := local.AdaptiveRungs(); rungs != nil && len(rungs) == 0 {
				b.transcodeStop(id)
				return "", &frontend.NoCompatiblePathError{RejectedPaths: []frontend.RejectedPlaybackPath{{Path: string(req.Format), Reasons: []string{"bitrate_limit"}}}}
			}
		}
		if output, ok := hs.(interface{ Decision() core.Decision }); ok {
			dec = output.Decision()
		}
	}
	httpExt := ""
	if req.Format == frontend.StreamHTTP {
		httpExt = httpStreamExt(dec.Container, dec.AudioCodec)
	}
	entry, ok := b.bindings.Get(id)
	if ok {
		entry.Started = true
		entry.Requested.AudioSet = entry.Requested.AudioSet || req.AudioStream != nil
		entry.Decision = dec
		entry.Offset = req.Position
		entry.UpdatedAt = b.deps.Now()
		b.bindings.Put(entry)
	}
	mediaPath := path + "master.m3u8"
	if req.Format == frontend.StreamDASH {
		mediaPath = path + "manifest.mpd"
	} else if req.Format == frontend.StreamHTTP {
		mediaPath = path + "stream." + httpExt
	}
	b.mu.Lock()
	if live := b.sessions[id]; live != nil {
		if !req.PrepareOnly || live.format != frontend.StreamDirect {
			live.format = req.Format
			live.public.MediaPath = mediaPath
		}
		live.httpExt = httpExt
		live.streamPosition = req.Position
		live.segmentDuration = req.SegmentDuration
		live.adaptiveHLS = adaptive
		live.adaptiveMaxBitrate = req.AdaptiveMaxBitrate
		if tracksChanged || reencode {
			live.opts = s.opts
			live.decision = dec
			live.qualityCapped = s.qualityCapped
		}
		if req.AudioStream != nil {
			live.originalAudio = nil
		}
		if req.SubtitleStream != nil {
			live.originalSubtitle = nil
			live.manualSubtitle = true
		}
	}
	b.mu.Unlock()
	if !req.PrepareOnly || s.format != frontend.StreamDirect {
		b.recordPlaybackDecision(s.playID, dec)
	}
	if !req.LazyStart && !req.PrepareOnly {
		b.mu.Lock()
		if live := b.sessions[id]; live != nil {
			live.trickplayStarted = true
		}
		b.mu.Unlock()
		b.ensureTrickplay(ctx, s, req.Position)
	}
	if err := b.bindDeliveryAuthority(ctx, s, ps); err != nil {
		b.transcodeStop(id)
		return "", err
	}
	if err = b.saveNeutralBinding(ctx, id, true); err != nil {
		b.transcodeStop(id)
		return "", err
	}
	return mediaPath, nil
}

func httpStreamExt(container, audioCodec string) string {
	switch strings.ToLower(strings.TrimSpace(container)) {
	case "mp3":
		return "mp3"
	case "aac", "adts":
		return "aac"
	case "mp4", "m4a", "m4b", "fmp4":
		return "m4a"
	case "ogg", "oga":
		return "ogg"
	case "opus":
		return "opus"
	case "webm", "weba":
		return "webm"
	case "flac":
		return "flac"
	case "wav":
		return "wav"
	case "ts", "mpegts":
		return "ts"
	case "mkv", "matroska", "mka":
		return "mkv"
	}
	switch strings.ToLower(strings.TrimSpace(audioCodec)) {
	case "mp3":
		return "mp3"
	case "opus", "vorbis":
		return "ogg"
	case "flac":
		return "flac"
	case "aac":
		return "m4a"
	}
	return "mkv"
}

// httpOutputDecision is shared by advisory planning and StreamHTTP startup.
func (b *Broker) httpOutputDecision(ps *core.PlaySession, caps core.ClientCaps, opts core.DecideOptions, allowTranscoding *bool, dec core.Decision) (core.Decision, bool) {
	if ps == nil || b.deps.Transcoder == nil {
		return core.Decision{}, false
	}
	if ps.Info.Video() == nil && ps.Info.HasPlayableMedia() {
		if dec.Kind != core.DecisionDirectPlay && profileDeliveryAllowed(caps.Profiles, dec, ps.Info, "http") {
			if allowTranscoding != nil && !*allowTranscoding && decisionEncodes(dec, ps.Info) {
				return dec, false
			}
			if dec.Kind == core.DecisionRemux && !opts.DirectStream {
				return dec, false
			}
			return dec, true
		}
		return b.decideAudioProfile(ps, caps, opts, allowTranscoding, "http")
	}
	opts.DirectPlay = false
	opts.Protocol = core.ProtocolHTTP
	if b.videoOff(caps) {
		caps.NoVideoTranscode = true
	}
	if dec.Kind == core.DecisionDirectPlay || dec.Container != transcode.ContainerMatroska {
		dec = b.deps.Transcoder.Decide(ps.Info, caps, opts)
	}
	if b.videoOff(caps) && decisionEncodesVideo(dec, ps.Info) {
		return dec, false
	}
	if allowTranscoding != nil && !*allowTranscoding && decisionEncodes(dec, ps.Info) {
		return dec, false
	}
	if dec.Kind == core.DecisionRemux && !opts.DirectStream {
		return dec, false
	}
	return dec, profileDeliveryAllowed(caps.Profiles, dec, ps.Info, "http")
}

// hlsOutputDecision is shared by advisory planning and stream startup, so a
// detail response cannot advertise HLS from an unsupported delivery profile.
// Capacity remains an atomic StartHLS check, not a planning reservation.
func (b *Broker) hlsOutputDecision(ps *core.PlaySession, caps core.ClientCaps, opts core.DecideOptions, allowTranscoding *bool, dec core.Decision) (core.Decision, bool) {
	if ps == nil || b.deps.Transcoder == nil {
		return core.Decision{}, false
	}
	if ps.Info.Video() == nil && ps.Info.HasPlayableMedia() {
		if dec.Kind != core.DecisionDirectPlay &&
			(dec.Container == transcode.ContainerFMP4 || dec.Container == transcode.ContainerMPEGTS || dec.Container == transcode.ContainerMatroska) &&
			profileDeliveryAllowed(caps.Profiles, dec, ps.Info, "hls") {
			if allowTranscoding != nil && !*allowTranscoding && decisionEncodes(dec, ps.Info) {
				return dec, false
			}
			if dec.Kind == core.DecisionRemux && !opts.DirectStream {
				return dec, false
			}
			return dec, true
		}
		return b.decideAudioProfile(ps, caps, opts, allowTranscoding, "hls")
	}
	if b.videoOff(caps) {
		caps.NoVideoTranscode = true
	}
	dec = HLSDecision(dec, opts.DirectPlay, func() core.Decision {
		retry := opts
		retry.DirectPlay = false
		return b.deps.Transcoder.Decide(ps.Info, caps, retry)
	}, func(d core.Decision) core.Decision {
		if ps.Info.Video() != nil && caps.EffectiveAlwaysEncodeVideo() {
			retry := opts
			retry.DirectPlay = false
			retry.DirectStream = false
			return b.deps.Transcoder.Decide(ps.Info, caps, retry)
		}
		return d
	})
	if b.videoOff(caps) && decisionEncodesVideo(dec, ps.Info) {
		return dec, false
	}
	if allowTranscoding != nil && !*allowTranscoding && decisionEncodes(dec, ps.Info) {
		return dec, false
	}
	if dec.Kind == core.DecisionRemux && !opts.DirectStream {
		return dec, false
	}
	return dec, profileDeliveryAllowed(caps.Profiles, dec, ps.Info, "hls")
}

// ServeMedia validates a session-scoped capability on every request.
func (b *Broker) ServeMedia(w http.ResponseWriter, r *http.Request) {
	b.serveMedia("", nil, w, r)
}

// ServeMediaFor binds a mounted handler to its protocol surface.
func (b *Broker) ServeMediaFor(protocol frontend.Protocol, w http.ResponseWriter, r *http.Request) {
	b.serveMedia(protocol, nil, w, r)
}

// ServeMediaAccess additionally binds the grant to the authenticated device.
func (b *Broker) ServeMediaAccess(w http.ResponseWriter, r *http.Request, a frontend.SessionAccess) {
	b.serveMedia(a.Protocol, &a, w, r)
}

// ServeSessionMedia serves one session resource for a device that already
// proved it owns the session. The grant stays on the internal path; callers
// must not copy it into a client URL.
func (b *Broker) ServeSessionMedia(w http.ResponseWriter, r *http.Request, a frontend.SessionAccess, sessionID, resource string) {
	if !sessionResource(sessionID, resource) {
		http.NotFound(w, r)
		return
	}
	if err := b.restoreNeutralBindingMode(r.Context(), sessionID, &a, "", string(a.Protocol), true); err != nil && err != core.ErrNotFound {
		http.NotFound(w, r)
		return
	}
	b.mu.Lock()
	live := b.sessions[sessionID]
	grant := ""
	tuned := false
	replacementRevision := ""
	if live != nil {
		grant = live.grant
		tuned = live.live
		if live.originalReplaced && resource == "file" && a.Viewer != nil && a.Viewer.ID == live.public.ViewerID && a.Protocol == live.public.Protocol && a.Device.ID != "" && a.Device.ID == live.public.Device.ID {
			replacementRevision = live.sourceETag
		}
	}
	b.mu.Unlock()
	if grant == "" {
		http.NotFound(w, r)
		return
	}
	if replacementRevision != "" && r.URL.Query().Get("sourceRevision") != replacementRevision {
		w.Header().Set("X-Ether-Playback-Error", "source_replaced")
		w.WriteHeader(http.StatusConflict)
		return
	}
	resourcePath := resource
	liveBase := ""
	// Silo plays the session master. A live tune's playlist and segments stay
	// under Silo's scoped /segment/<name> prefix so the client never sees the
	// media grant and every URI satisfies PlaybackMediaAuthorization.Scope.
	if tuned && a.Protocol == "silo" {
		liveBase = "/api/v2/playback/transcode/" + sessionID + "/segment/"
		switch {
		case resource == "master.m3u8":
			resourcePath = "live/index.m3u8"
		case strings.HasPrefix(resource, "segment/"):
			resourcePath = "live/" + siloLiveSegmentPath(strings.TrimPrefix(resource, "segment/"))
		}
	}
	forward := r.Clone(r.Context())
	u := *r.URL
	u.Path = "/sessions/media/" + sessionID + "/" + grant + "/" + resourcePath
	u.RawPath = ""
	forward.URL = &u
	if liveBase != "" {
		b.serveMediaAt(a.Protocol, &a, w, forward, liveBase)
		return
	}
	b.ServeMediaAccess(w, forward, a)
}

func siloLiveSegmentPath(seg string) string {
	if seg == "index.m3u8" {
		return "index.m3u8"
	}
	if rest, ok := strings.CutPrefix(seg, "resource-"); ok && rest != "" && !strings.Contains(rest, "/") {
		return "resource/" + rest
	}
	if rest, ok := strings.CutPrefix(seg, "relay-"); ok && rest != "" && !strings.Contains(rest, "/") {
		return "relay/" + rest
	}
	if rest, ok := strings.CutPrefix(seg, "program-"); ok && !strings.Contains(rest, "/") {
		if start, file, ok := strings.Cut(rest, "-"); ok && start != "" && file != "" {
			return "program/" + start + "/" + file
		}
	}
	return seg
}

func sessionResource(sessionID, resource string) bool {
	if sessionID == "" || resource == "" || strings.Contains(sessionID, "/") || strings.HasPrefix(resource, "/") || strings.HasSuffix(resource, "/") {
		return false
	}
	for _, part := range strings.Split(resource, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func (b *Broker) currentMediaPolicy(ctx context.Context, viewer *core.Viewer, s *brokerSession) bool {
	confirmed, err := b.confirmCurrent(ctx, viewer, s.caps)
	if err != nil || confirmed.ID != s.public.ViewerID {
		return false
	}
	return b.currentItemPolicy(ctx, confirmed, s)
}

func (b *Broker) currentItemPolicy(ctx context.Context, viewer *core.Viewer, s *brokerSession) bool {
	if s.live {
		if b.deps.Live == nil {
			return false
		}
		liveCtx := core.WithLiveApp(ctx, string(s.public.Protocol))
		if s.directLive != nil {
			db, ok := b.deps.Live.(DirectLiveBackend)
			return ok && db.DirectAllowed(liveCtx, viewer, s.directLive) == nil
		}
		return b.deps.Live.Allowed(liveCtx, viewer, s.public.Channel) == nil
	}
	lookup := b.deps.CurrentMediaLookup
	if lookup == nil {
		lookup = b.deps.MediaLookup
	}
	if lookup == nil {
		lookup = b.deps.Lookup
	}
	if lookup == nil {
		if s.public.Item.IsPluginKey() && b.deps.KindSource != nil {
			in, err := b.deps.KindSource(core.WithLiveApp(ctx, string(s.public.Protocol)), viewer, s.public.Item)
			return err == nil && in != nil && in.Title != nil && in.Title.Key == s.public.Item && (in.Title.Released.IsZero() || !in.Title.Released.After(b.deps.Now()))
		}
		return false
	}
	title, err := lookup(core.WithLiveApp(ctx, string(s.public.Protocol)), viewer, s.public.Device, s.public.Item)
	return err == nil && title != nil && title.Key == s.public.Item && (title.Released.IsZero() || !title.Released.After(b.deps.Now()))
}

func (b *Broker) serveMedia(protocol frontend.Protocol, access *frontend.SessionAccess, w http.ResponseWriter, r *http.Request) {
	b.serveMediaAt(protocol, access, w, r, "")
}

// serveMediaAt allows a broker-owned wrapper to keep live playlist resources
// on its own authorized path. Empty base preserves the existing media URLs.
func (b *Broker) serveMediaAt(protocol frontend.Protocol, access *frontend.SessionAccess, w http.ResponseWriter, r *http.Request, liveBase string) {
	var lost func()
	defer func() {
		if lost != nil {
			lost()
		}
	}()
	mediasafety.SetHeaders(w)
	if b.servePreview(protocol, access, w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/sessions/media/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	_ = b.restoreNeutralBindingMode(r.Context(), parts[0], access, parts[1], string(protocol), true)
	b.mu.Lock()
	live, ok := b.sessions[parts[0]]
	var tracker *core.ReadRetirement
	var s *brokerSession
	var interruptMu sync.Mutex
	interruptible := false
	if ok {
		tracker = &core.ReadRetirement{}
		live.servingReads = append(live.servingReads, tracker)
		copied := *live
		s = &copied
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		// Retirement cuts this response short: cancel its reads and fail a
		// write blocked on a client that stopped reading. The response
		// controller is valid only while this handler runs.
		controller := http.NewResponseController(w)
		interruptible = true
		tracker.OnInterrupt(func() {
			cancel()
			interruptMu.Lock()
			defer interruptMu.Unlock()
			if interruptible {
				_ = controller.SetWriteDeadline(time.Now())
			}
		})
		r = r.WithContext(core.WithReadRetirement(ctx, tracker))
	}
	b.mu.Unlock()
	if !ok {
		if access == nil {
			if d, yes := b.deps.DistributedHLS.(interface {
				ForwardMedia(http.ResponseWriter, *http.Request, string, string) bool
			}); yes && d.ForwardMedia(w, r, parts[0], string(protocol)) {
				return
			}
		}
		if b.replacedMedia(parts[0], parts[1]) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "Session replaced", http.StatusConflict)
			return
		}
		b.logMasterFailure(r, parts[0], "session unavailable")
		http.NotFound(w, r)
		return
	}
	defer func() {
		interruptMu.Lock()
		interruptible = false
		interruptMu.Unlock()
		tracker.OnInterrupt(nil)
		<-tracker.Seal()
		b.mu.Lock()
		if cur := b.sessions[parts[0]]; cur != nil {
			kept := cur.servingReads[:0]
			for _, item := range cur.servingReads {
				if item != tracker {
					kept = append(kept, item)
				}
			}
			cur.servingReads = kept
		}
		b.mu.Unlock()
	}()
	if protocol != "" && s.public.Protocol != protocol {
		http.NotFound(w, r)
		return
	}
	if access != nil && (access.Owner || access.Viewer == nil || access.Viewer.ID != s.public.ViewerID || access.Protocol == "" || access.Device.ID == "" || access.Device.ID != s.public.Device.ID) {
		http.NotFound(w, r)
		return
	}
	viewer, ok := streamgrant.AuthenticateGrant(r.Context(), r.Method, parts[1], streamgrant.GrantBinding{Token: s.grant, ViewerID: s.public.ViewerID, Started: true}, func(ctx context.Context, id int64) *core.Viewer {
		v, e := b.deps.ActiveViewer(ctx, id)
		if e != nil {
			return nil
		}
		return v
	})
	if !ok || viewer.ID != s.public.ViewerID {
		b.logMasterFailure(r, s.public.ID, "media grant denied")
		http.NotFound(w, r)
		return
	}
	// Grants do not freeze library, rating, release, or channel permissions.
	// Check the current policy before handing any bytes to a backend.
	if !b.currentMediaPolicy(r.Context(), viewer, s) {
		b.logMasterFailure(r, s.public.ID, "current media policy denied")
		http.NotFound(w, r)
		return
	}
	if !b.neutralMediaCurrent(r.Context(), s) {
		http.Error(w, "session coordinator unavailable", 503)
		return
	}
	if b.neutralCurrent(r.Context(), s) {
		if err := b.saveNeutralBinding(r.Context(), s.public.ID, false); err != nil {
			http.Error(w, "session state unavailable", 503)
			return
		}
	}
	if remote, ok := b.deps.Playback.(interface{ Touch(string) }); ok {
		remote.Touch(s.playID)
	}
	if s.sourcePinned {
		ps, ok := b.deps.Playback.Session(s.playID)
		if !ok || ps == nil || sourceRevision(ps) != s.sourceETag {
			serveSourceGone(b.policy(protocol), w, r)
			return
		}
	}
	tail := parts[2:]
	if s.originalFailure && len(tail) == 1 && tail[0] == "file" {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Ether-Playback-Error", "version_unavailable")
		w.WriteHeader(http.StatusConflict)
		return
	}
	if serveAudioRendition(w, r, s, tail) {
		return
	}
	if s.live && len(tail) >= 2 && tail[0] == "live" && b.deps.Live != nil {
		if s.multiviewGroup != "" {
			b.mu.Lock()
			b.touchMultiviewLocked(s.multiviewGroup, b.deps.Now())
			b.mu.Unlock()
		}
		if liveBase == "" {
			liveBase = "/sessions/media/" + s.public.ID + "/" + s.grant + "/live/"
		}
		b.deps.Live.Serve(w, r, viewer, s.playID, strings.Join(tail[1:], "/"), liveBase)
		return
	}
	if r.Method == http.MethodGet && !s.live && b.deps.Trickplay != nil && !s.trickplayStarted {
		b.mu.Lock()
		start := false
		if cur := b.sessions[s.public.ID]; cur != nil && !b.closed && cur.playID == s.playID && !cur.trickplayStarted {
			cur.trickplayStarted = true
			start = true
		}
		b.mu.Unlock()
		if start {
			sealed := *s
			go func() {
				c, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
				defer cancel()
				b.ensureTrickplay(c, &sealed, sealed.streamPosition)
			}()
		}
	}
	beginMediaRead := func() func() {
		if r.Method == http.MethodHead || s.live || s.playID == "" {
			return func() {}
		}
		if reader, ok := b.deps.Playback.(interface{ BeginMediaRead(string) func() }); ok {
			return reader.BeginMediaRead(s.playID)
		}
		return func() {}
	}
	if len(tail) == 2 && tail[0] == "subtitles" {
		b.serveSubtitle(w, r, s, tail[1])
		return
	}
	if len(tail) == 1 && tail[0] == "file" && (s.format == frontend.StreamDirect || (s.public.Protocol == "vela" && (s.opts.DirectPlay || s.opts.DirectStream))) {
		if s.decision.Reason == "original_runtime" {
			policy, _ := core.RangeResponseFrom(r.Context())
			policy.StrictRanges = true
			r = r.WithContext(core.WithRangeResponse(r.Context(), policy))
		}
		ctx := r.Context()
		if _, ok := b.deps.Playback.(core.OriginalRecovery); ok && !s.live && !s.restored {
			revision := ""
			if s.sourcePinned || s.originalReplaced {
				revision = s.sourceETag
			}
			request := core.OriginalRange{Revision: revision, Lost: func() {
				lost = func() { b.recoverOriginal(context.WithoutCancel(ctx), s, viewer, false) }
			}}
			r = r.WithContext(core.WithOriginalRange(ctx, request))
		}
		b.deps.Playback.ServeRange(w, r, s.playID)
		return
	}
	if b.deps.Transcoder == nil {
		http.NotFound(w, r)
		return
	}
	hs, ok := b.transcodeSession(s.public.ID)
	if !ok || hs == nil {
		b.logMasterFailure(r, s.public.ID, "encoder session unavailable")
		http.NotFound(w, r)
		return
	}
	if s.format == frontend.StreamHTTP {
		if httpSess, ok := hs.(core.HTTPSession); ok && len(tail) == 1 && (tail[0] == "stream" || strings.HasPrefix(tail[0], "stream.")) {
			// Byte offsets from a failed source cannot address a fresh
			// progressive remux. A client can start it with a plain GET.
			if r.Header.Get("Range") != "" {
				if ps, ok := b.deps.Playback.Session(s.playID); ok && ps != nil && ps.Failovers > 0 {
					w.Header().Set("Cache-Control", "no-store")
					w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					return
				}
			}
			httpSess.ServeHTTP(w, r)
			return
		}
	}
	if s.format == frontend.StreamHLS || (s.public.Protocol == "vela" && s.format == frontend.StreamDirect) {
		if len(tail) == 1 && tail[0] == "master.m3u8" {
			b.serveMaster(w, r, s, hs)
			return
		}
		if serveTextRendition(w, r, hs, tail) {
			return
		}
		// Keep the initial single-variant URLs working for already-open clients.
		if len(tail) == 1 {
			if tail[0] == "index.m3u8" {
				hs.ServePlaylist(w, r)
				return
			}
			if n, ok := transcode.ParseSegmentName(tail[0]); ok {
				defer beginMediaRead()()
				hs.ServeSegment(w, r, n)
				return
			}
		}
		if len(tail) == 2 {
			rung, name := tail[0], tail[1]
			// Remote sessions expose the ladder interface even when ABR is
			// disabled. An empty ladder still uses the base rendition emitted
			// by serveMaster, through the ordinary playlist/segment operations.
			if ls, ok := hs.(core.HLSLadderSession); ok && len(ls.Rungs()) > 0 {
				for _, candidate := range ls.Rungs() {
					if candidate.ID == rung && validRung(rung) {
						if name == "index.m3u8" {
							ls.ServeRungPlaylist(w, r, rung)
							return
						}
						if n, ok := transcode.ParseSegmentName(name); ok {
							defer beginMediaRead()()
							ls.ServeRungSegment(w, r, rung, n)
							return
						}
					}
				}
			} else if rung == "base" || rung == "segment" {
				if name == "index.m3u8" {
					hs.ServePlaylist(w, r)
					return
				}
				if n, ok := transcode.ParseSegmentName(name); ok {
					defer beginMediaRead()()
					hs.ServeSegment(w, r, n)
					return
				}
			}
		}
	}
	if s.format == frontend.StreamDASH {
		ds, ok := hs.(core.DASHSession)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if len(tail) == 1 && tail[0] == "manifest.mpd" {
			ds.ServeManifest(w, r)
			return
		}
		if len(tail) == 4 && tail[0] == "session" && tail[1] == s.public.ID {
			rep, err := strconv.Atoi(tail[2])
			if err == nil && rep >= 0 {
				if tail[3] == "header" {
					defer beginMediaRead()()
					ds.ServeInit(w, r, rep)
					return
				}
				if n, ok := transcode.ParseSegmentName(tail[3]); ok && n >= 0 {
					defer beginMediaRead()()
					ds.ServeMedia(w, r, rep, n)
					return
				}
			}
		}
	}
	http.NotFound(w, r)
}

func validRung(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (b *Broker) serveMaster(w http.ResponseWriter, r *http.Request, s *brokerSession, hs core.HLSSession) {
	ps, ok := b.deps.Playback.Session(s.playID)
	if !ok || ps == nil {
		b.logMasterFailure(r, s.public.ID, "playback source unavailable")
		http.NotFound(w, r)
		return
	}
	var body strings.Builder
	body.WriteString("#EXTM3U\n")
	var rungs []core.HLSRung
	if adaptive, ok := hs.(core.AdaptiveHLSSession); ok && s.adaptiveHLS {
		rungs = adaptive.AdaptiveRungs()

		if rungs != nil && len(rungs) == 0 {
			http.Error(w, "no playable rendition", http.StatusServiceUnavailable)
			return
		}
	} else if ls, ok := hs.(core.HLSLadderSession); ok && s.public.Protocol != "vela" {
		if all := ls.Rungs(); len(all) > 1 {
			rungs = all
		}
	}
	if rawCap := r.URL.Query().Get("maxBitrate"); s.public.Protocol == "vela" && rawCap != "" {
		cap, err := strconv.Atoi(rawCap)
		if err != nil || cap <= 0 || !s.adaptiveHLS || rungs == nil {
			http.NotFound(w, r)
			return
		}
		filtered := make([]core.HLSRung, 0, len(rungs))
		for _, rung := range rungs {
			if rung.Bandwidth <= cap {
				filtered = append(filtered, rung)
			}
		}
		if len(filtered) == 0 {
			http.Error(w, "no playable rendition", http.StatusServiceUnavailable)
			return
		}
		rungs = filtered
	}
	if len(rungs) == 1 && rungs[0].ID == "base" && rungs[0].DynamicRange == "dv" {
		// Single-variant Dolby Vision needs the source profile and optional
		// supplemental codec declaration emitted by the canonical writer.
		rungs = nil
	}
	if len(rungs) > 0 {
		for _, rung := range rungs {
			if !validRung(rung.ID) || rung.Bandwidth <= 0 {
				continue
			}
			fmt.Fprintf(&body, "#EXT-X-STREAM-INF:BANDWIDTH=%d", rung.Bandwidth)
			if rung.AverageBandwidth > 0 {
				fmt.Fprintf(&body, ",AVERAGE-BANDWIDTH=%d", rung.AverageBandwidth)
			}
			if rung.Width > 0 && rung.Height > 0 {
				fmt.Fprintf(&body, ",RESOLUTION=%dx%d", rung.Width, rung.Height)
			}
			if rung.Codecs != "" && !strings.ContainsAny(rung.Codecs, "\"\r\n") {
				fmt.Fprintf(&body, ",CODECS=%q", rung.Codecs)
			}
			if rung.FrameRate > 0 {
				fmt.Fprintf(&body, ",FRAME-RATE=%.3f", rung.FrameRate)
			}
			if s.adaptiveHLS {
				videoRange := "SDR"
				switch rung.DynamicRange {
				case "hdr10", "dv":
					videoRange = "PQ"
				case "hlg":
					videoRange = "HLG"
				}
				fmt.Fprintf(&body, ",VIDEO-RANGE=%s", videoRange)
			}
			fmt.Fprintf(&body, "\n%s/index.m3u8\n", rung.ID)
		}
	} else {
		entry, ok := b.bindings.Get(s.public.ID)
		if !ok {
			b.logMasterFailure(r, s.public.ID, "encoder binding unavailable")
			http.NotFound(w, r)
			return
		}
		dec := entry.Decision
		bw := ps.Info.BitrateKbps
		if (!dec.VideoCopy || !dec.AudioCopy || ps.Info.Video() == nil) && dec.BitrateKbps > 0 {
			bw = dec.BitrateKbps
		}
		if bw <= 0 {
			if ps.Info.Video() == nil {
				bw = 192
			} else {
				bw = 8000
			}
		}
		fmt.Fprintf(&body, "#EXT-X-STREAM-INF:BANDWIDTH=%d%s%s\nbase/index.m3u8\n", bw*1000, transcode.HLSVariantGeometry(ps.Info, dec), transcode.HLSVariantAttributes(ps.Info, dec))
	}
	if body.String() == "#EXTM3U\n" {
		http.Error(w, "no playable rendition", http.StatusServiceUnavailable)
		return
	}
	master := body.String()
	if tags, group := textRenditionTags(s, hs, ps.Info); tags != "" {
		lines := strings.Split(strings.TrimPrefix(master, "#EXTM3U\n"), "\n")
		for n, line := range lines {
			if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
				lines[n] = line + group
			}
		}
		master = "#EXTM3U\n" + tags + strings.Join(lines, "\n")
	}
	if b.policy(s.public.Protocol).MasterMetadata {
		independent, ok := hs.(core.HLSIndependentSegments)
		version := 7 // conservative if the selected media format is unavailable
		if binding, present := b.bindings.Get(s.public.ID); present && binding.Decision.Container == transcode.ContainerMPEGTS {
			version = 3
		}
		master = embyMasterMetadata(master, version, ok && independent.IndependentSegments())
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(master)))
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, master)
	}
}

var _ frontend.Sessions = (*Broker)(nil)

// embyMasterMetadata adds the compatibility version supported by the emitted
// master tags. Independence is advertised only from a session-wide guarantee.
func embyMasterMetadata(master string, version int, independent bool) string {
	metadata := fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:%d\n", version)
	if independent {
		metadata += "#EXT-X-INDEPENDENT-SEGMENTS\n"
	}
	return metadata + strings.TrimPrefix(master, "#EXTM3U\n")
}

// Log only failed master requests, without the URL or its bearer grant. A
// frontend's status-only access log cannot distinguish these failure paths.
func (b *Broker) logMasterFailure(r *http.Request, sessionID, reason string) {
	if b.deps.Logger != nil && strings.HasSuffix(r.URL.Path, "/master.m3u8") {
		b.deps.Logger.Warn("session master unavailable", "session", sessionID, "reason", reason)
	}
}

// PlayingRelease reports the release the session's playback reads now,
// after any failover. It reads the live playback session; it never resolves.
func (b *Broker) PlayingRelease(ctx context.Context, a frontend.SessionAccess, id string) (core.Release, core.MediaInfo, error) {
	s, _, err := b.accessSession(ctx, a, id, false)
	if err != nil {
		return core.Release{}, core.MediaInfo{}, err
	}
	if s.live || s.playID == "" || b.deps.Playback == nil {
		return core.Release{}, core.MediaInfo{}, core.ErrNotAvailable
	}
	ps, ok := b.deps.Playback.Session(s.playID)
	if !ok || ps == nil || ps.Resolution == nil || ps.Resolution.Release.ID == "" {
		return core.Release{}, core.MediaInfo{}, core.ErrNotAvailable
	}
	return ps.Resolution.Release, publicMedia(ps.Info), nil
}

func (b *Broker) StreamDecision(ctx context.Context, a frontend.SessionAccess, id string) (core.Decision, error) {
	if _, _, err := b.access(ctx, a, id); err != nil {
		return core.Decision{}, err
	}
	entry, ok := b.bindings.Get(id)
	if !ok || !entry.Started {
		return core.Decision{}, core.ErrNotAvailable
	}
	return entry.Decision, nil
}
