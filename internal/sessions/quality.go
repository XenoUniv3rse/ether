package sessions

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func (b *Broker) ProactiveFallbackAvailable() bool {
	if b.videoTranscodingOff() {
		return false
	}
	provider, ok := b.deps.Transcoder.(interface{ ProactiveFallbackAvailable() bool })
	return ok && provider.ProactiveFallbackAvailable()
}

// ProactiveFallbackAvailableFor reports whether proactive quality fallback
// (transcoded lower-bitrate rungs) is available for viewer v.
func (b *Broker) ProactiveFallbackAvailableFor(v *core.Viewer) bool {
	if !b.ProactiveFallbackAvailable() {
		return false
	}
	if v != nil {
		var caps core.ClientCaps
		b.stampViewerTranscoding(v, &caps)
		if caps.NoVideoTranscode {
			return false
		}
	}
	return true
}

func (b *Broker) qualityPlan(ctx context.Context, s *brokerSession, viewer *core.Viewer) ([]core.QualityVariant, []core.QualityLimitation, error) {
	if s.public.Protocol != "vela" || s.live || s.restored {
		return nil, nil, nil
	}
	caps := s.caps
	b.stampViewerTranscoding(viewer, &caps)
	if b.videoOff(caps) {
		return nil, nil, nil
	}
	if !b.ProactiveFallbackAvailableFor(viewer) {
		return nil, nil, nil
	}
	if !b.currentMediaPolicy(ctx, viewer, s) {
		return nil, nil, nil
	}
	if s.allowTranscoding != nil && !*s.allowTranscoding {
		return nil, nil, nil
	}
	ps, ok := b.deps.Playback.Session(s.playID)
	if !ok || ps == nil {
		return nil, nil, core.ErrNotAvailable
	}
	if s.sourcePinned && sourceRevision(ps) != s.sourceETag {
		return nil, nil, frontend.ErrSourceGone
	}
	dec := s.decision
	options := s.opts
	// A DirectPlay decision carries no transcode output codecs, so the HLS
	// ladder must be re-decided with DirectPlay disabled. This covers both the
	// original_runtime re-pin and a plain direct-play decision (reason ""),
	// where the empty codecs would otherwise fail profileDeliveryAllowed and
	// drop the HLS rungs.
	if dec.Kind == core.DecisionDirectPlay {
		options.DirectPlay = false
		var err error
		dec, err = b.decideProfile(ps, frontend.PlanRequest{Viewer: viewer, Device: s.public.Device, Protocol: s.public.Protocol, Item: s.public.Item, Caps: s.caps, Intent: frontend.PlayIntent{Options: options, AllowTranscoding: s.allowTranscoding}})
		if err != nil {
			return nil, nil, nil
		}
	}
	dec, allowed := b.hlsOutputDecision(ps, s.caps, options, s.allowTranscoding, dec)
	if !allowed || dec.Container == "mkv" {
		return nil, nil, nil
	}
	variants, limits := b.qualityVariants(ps.Info, dec, s, s.caps, b.qualityBitrateCap(ctx, s, viewer, ps.Info.BitrateKbps))
	return variants, limits, nil
}

func (b *Broker) qualityBitrateCap(ctx context.Context, s *brokerSession, viewer *core.Viewer, demand int) int {
	kbps := s.opts.MaxBitrateKbps
	if s.caps.MaxBitrateKbps > 0 && (kbps == 0 || s.caps.MaxBitrateKbps < kbps) {
		kbps = s.caps.MaxBitrateKbps
	}
	if b.deps.BitrateLimit != nil {
		if limit := b.deps.BitrateLimit(viewer); limit > 0 && (kbps == 0 || limit < kbps) {
			kbps = limit
		}
	}
	if s.caps.Remote && b.deps.RemoteBitrateLimit != nil {
		if limit := b.deps.RemoteBitrateLimit(); limit > 0 && (kbps == 0 || limit < kbps) {
			kbps = limit
		}
	}
	if b.deps.ApplyRuleLimits != nil {
		probe := s.caps
		probe.MaxBitrateKbps = 0
		if limit := b.deps.ApplyRuleLimits(ctx, probe, viewer, nil).MaxBitrateKbps; limit > 0 && (kbps == 0 || limit < kbps) {
			kbps = limit
		}
	}
	if s.caps.Remote && b.deps.RemoteUploadCap != nil {
		if limit, fromBudget := b.deps.RemoteUploadCap(ctx, s.playID, true, true, kbps, demand); fromBudget && limit > 0 && (kbps == 0 || limit < kbps) {
			kbps = limit
		}
	}
	return kbps
}

func (b *Broker) qualityVariants(info core.MediaInfo, dec core.Decision, s *brokerSession, caps core.ClientCaps, kbps int) ([]core.QualityVariant, []core.QualityLimitation) {
	width, height := s.opts.MaxWidth, stricterLimit(s.opts.MaxHeight, caps.MaxHeight)
	support := transcode.QualitySupport{}
	if provider, ok := b.deps.Transcoder.(interface {
		QualitySupport() transcode.QualitySupport
	}); ok {
		support = provider.QualitySupport()
	}
	variants, limits := transcode.VelaQualityVariants(info, dec, caps, width, height, kbps, support)
	out := variants[:0]
	for _, variant := range variants {
		if variant.Rung.Height <= 0 {
			continue
		}
		d, ok := constrainedH264(variant.Decision, info, caps, b.h264EncoderVerified())
		if !ok || !profileDeliveryAllowed(caps.Profiles, d, info, "hls") {
			limits = append(limits, core.QualityLimitation{Height: variant.Rung.Height, Reason: "decoder_profile_limit"})
			continue
		}
		variant.Decision = d
		out = append(out, variant)
	}
	filteredLimits := limits[:0]
	for _, l := range limits {
		if l.Height > 0 {
			filteredLimits = append(filteredLimits, l)
		}
	}
	if len(filteredLimits) == 0 {
		filteredLimits = nil
	}
	return out, filteredLimits
}

func (b *Broker) qualityRungs(ctx context.Context, s *brokerSession, viewer *core.Viewer) ([]core.HLSRung, error) {
	variants, _, err := b.qualityPlan(ctx, s, viewer)
	out := make([]core.HLSRung, 0, len(variants))
	for _, variant := range variants {
		out = append(out, variant.Rung)
	}
	return out, err
}

// QualityLimitations reports stable reason codes without starting media work.
func (b *Broker) QualityLimitations(ctx context.Context, a frontend.SessionAccess, id string) ([]core.QualityLimitation, error) {
	s, v, err := b.access(ctx, a, id)
	if err != nil {
		return nil, err
	}
	_, limits, err := b.qualityPlan(ctx, s, v)
	return limits, err
}

func (b *Broker) QualityRungs(ctx context.Context, a frontend.SessionAccess, id string) ([]core.HLSRung, error) {
	s, v, err := b.access(ctx, a, id)
	if err != nil {
		return nil, err
	}
	return b.qualityRungs(ctx, s, v)
}

func (b *Broker) PrepareQuality(ctx context.Context, a frontend.SessionAccess, id, rung string, position time.Duration) (string, time.Duration, error) {
	rungs, err := b.QualityRungs(ctx, a, id)
	if err != nil {
		return "", 0, err
	}
	allowed := false
	for _, r := range rungs {
		if r.ID == rung {
			allowed = true
		}
	}
	if !allowed {
		return "", 0, core.ErrNotAvailable
	}
	s, _, err := b.access(ctx, a, id)
	if err != nil {
		return "", 0, err
	}
	origin := position
	if hs, ok := b.transcodeSession(id); ok && hs != nil {
		origin = s.streamPosition
	}
	url, err := b.StartStream(ctx, a, id, frontend.StreamRequest{Format: frontend.StreamHLS, Position: origin, LazyStart: true, Continuous: true, PrepareOnly: true})
	if err != nil {
		return "", 0, err
	}
	hs, ok := b.transcodeSession(id)
	if !ok {
		return "", 0, core.ErrNotAvailable
	}
	preparer, ok := hs.(interface {
		PrepareRung(context.Context, string, time.Duration) error
	})
	if !ok {
		return "", 0, core.ErrNotAvailable
	}
	if err = preparer.PrepareRung(ctx, rung, position); err != nil {
		return "", 0, err
	}
	if clock, ok := hs.(interface{ TimelineOrigin() time.Duration }); ok {
		origin = clock.TimelineOrigin()
	}
	return url, origin, nil
}
