package transcode

import (
	"fmt"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// QualitySupport is a snapshot of the local encoder and filter inventory.
// Planning must never probe hardware or start a process.
type QualitySupport struct{ HEVC, ToneMap, DolbyVision5 bool }

func (m *Manager) QualitySupport() QualitySupport {
	_, hw := videoEncoder("hevc", m.hw, m.feat)
	return QualitySupport{HEVC: hw, ToneMap: len(toneMapPaths(m.feat, m.hw)) > 0 || m.feat.vaapiToneMap != nil, DolbyVision5: m.ToneMapsDolbyVisionProfile5()}
}

// VelaQualityVariants is an explicit menu, independent of the throughput-drop
// heuristics in buildRungs. Same-resolution bitrate choices are intentional.
// All bitrates here are video targets; audio is added exactly once.
func VelaQualityVariants(info core.MediaInfo, base core.Decision, caps core.ClientCaps, maxWidth, maxHeight, maxKbps int, support QualitySupport) ([]core.QualityVariant, []core.QualityLimitation) {
	return qualityVariants(info, base, caps, maxWidth, maxHeight, maxKbps, support, false)
}

// AdaptiveQualitySupport includes software HEVC for the lower-resolution ladder.
func (m *Manager) AdaptiveQualitySupport(info core.MediaInfo, base core.Decision) QualitySupport {
	support := m.QualitySupport()
	base.VideoCodec = "hevc"
	encoder, _ := (runSpec{Info: info, Decision: base, Accel: m.hw, Features: m.feat}).videoEncoder()
	support.HEVC = encoder != "libx264"
	return support
}

// AdaptiveQualityVariants stays within the decoder family and range of the
// negotiated HLS output. The explicit quality menu can offer other formats.
func AdaptiveQualityVariants(info core.MediaInfo, base core.Decision, caps core.ClientCaps, maxWidth, maxHeight, maxKbps int, support QualitySupport) ([]core.QualityVariant, []core.QualityLimitation) {
	return qualityVariants(info, base, caps, maxWidth, maxHeight, maxKbps, support, true)
}

// AdaptiveAudio gives each muxed variant the same AAC-LC output, including
// an encoded sample clock on the video-copy rung.
func AdaptiveAudio(info core.MediaInfo, base core.Decision, caps core.ClientCaps) (core.Decision, bool) {
	a := pickAudio(info, base.AudioStream)
	if a == nil {
		return base, true
	}
	if !caps.SupportsAudio("aac") {
		return base, false
	}
	base.AudioStream = a.Index
	base.AudioCodec, base.AudioCopy = "aac", false
	base.AudioChannels, base.AudioSampleRate, base.AudioBitrateKbps = min(2, max(1, a.Channels)), 48000, 0
	applyAudioLimits(&base, a, caps, false, "aac")
	if base.AudioSampleRate != 48000 {
		return base, false
	}
	base.Kind = core.DecisionTranscode
	return base, true
}

func qualityVariants(info core.MediaInfo, base core.Decision, caps core.ClientCaps, maxWidth, maxHeight, maxKbps int, support QualitySupport, adaptive bool) ([]core.QualityVariant, []core.QualityLimitation) {
	v := info.Video()
	if v == nil || v.Width <= 0 || v.Height <= 0 {
		return nil, nil
	}
	var out []core.QualityVariant
	var limits []core.QualityLimitation
	codec := "h264"
	if support.HEVC && slices.Contains(caps.VideoCodecs, "hevc") {
		codec = "hevc"
	}
	sourceRange := encodedRange(v)
	dr := rangeSDR
	if adaptive {
		_, _, codec, dr = qualityMetadata(info, rung{decision: base})
		if codec != "h264" && codec != "hevc" {
			return nil, nil
		}
		if codec == "hevc" && !support.HEVC {
			return nil, []core.QualityLimitation{{Height: v.Height, Reason: "encoder_hevc_unavailable"}}
		}
		if dr == rangeDV || (codec != "hevc" && dr != rangeSDR) {
			return nil, []core.QualityLimitation{{Height: v.Height, Reason: "dynamic_range_unavailable"}}
		}
	}
	keepHDR := codec == "hevc" && sourceRange != rangeSDR && sourceRange != rangeDV && slices.Contains(caps.HDRFormats, sourceRange) && !caps.ExplicitNoHDR
	type qualityStep struct{ height, video int }
	sourceSize := false
	steps := []qualityStep{{2160, 40000}, {1080, 20000}, {1080, 12000}, {1080, 8000}, {720, 6000}, {720, 4000}, {480, 1500}, {360, 800}}
	var cappedVideo int
	if adaptive && maxKbps > 0 {
		average := min(maxKbps*10/12, maxKbps-100)
		cappedVideo = average - decisionAudioKbps(info, base)
	}
	// The copy clock may be unavailable even on a fast link. Offer a
	// source-size encode under the peak budget before the standard steps.
	if adaptive && base.VideoCopy {
		w, sourceHeight := displaySize(v)
		h := minPositive(sourceHeight, maxHeight, caps.VideoMaxHeights[codec])
		if maxWidth > 0 && maxWidth < w {
			h = min(h, even(sourceHeight*maxWidth/w))
		}
		target := bitrateCeiling(h, codec)
		if maxKbps > 0 {
			target = min(target, cappedVideo)
		}
		if h > 480 && target >= bitrateCeiling(h, codec)/4 {
			steps = append([]qualityStep{{h, target}}, steps...)
			sourceSize = true
		}
	}
	for index, step := range steps {
		if adaptive && maxKbps > 0 && step.height == 360 && cappedVideo >= minRungVideoKbps {
			step.video = min(step.video, cappedVideo)
		}
		if adaptive && step.height > 720 && step.video > bitrateCeiling(step.height, codec) {
			continue
		}
		// Use source naming classes (width for scope movies), never duplicate a
		// taller class by clamping it to the source dimensions.
		if v.Height < step.height && v.Width < step.height*16/9 {
			continue
		}
		w, h := fitRungBox(v, step.height)
		if sourceSize && index == 0 {
			w, h = displayOutputSize(v, step.height)
		}
		ranges := []string{rangeSDR}
		if adaptive {
			ranges = []string{dr}
		} else if keepHDR {
			ranges = []string{sourceRange}
			if step.height == 2160 {
				ranges = append(ranges, rangeSDR)
			}
		}
		for _, dr := range ranges {
			reason := ""
			switch {
			case !slices.Contains(caps.VideoCodecs, codec):
				reason = "decoder_unavailable"
			case (maxWidth > 0 && w > maxWidth) || (maxHeight > 0 && h > maxHeight):
				reason = "resolution_limit"
			case caps.VideoMaxHeights[codec] > 0 && h > caps.VideoMaxHeights[codec]:
				reason = "decoder_resolution_limit"
			case codec == "hevc" && !qualityHEVCProfileAllowed(caps.VideoProfiles[codec], dr):
				reason = "decoder_profile_limit"
			case v.DolbyVision && v.DolbyVisionProfile == 5 && !support.DolbyVision5:
				reason = "encoder_dv5_tonemap_unavailable"
			case sourceRange != rangeSDR && dr == rangeSDR && !support.ToneMap:
				reason = "encoder_tonemap_unavailable"
			}
			d := base
			d.Kind, d.VideoCopy, d.VideoCodec = core.DecisionTranscode, false, codec
			d.Enhancement = core.VideoEnhancement{}
			d.MaxHeight = h
			d.H264Level, d.H264FrameRate = 0, 0
			d.ToneMap = sourceRange != rangeSDR && dr == rangeSDR
			d.StripDolbyVision = v.DolbyVision
			audio := decisionAudioKbps(info, d)
			d.BitrateKbps = step.video + audio
			peak := peakKbps(d.BitrateKbps)
			if limit := caps.VideoLevels[codec]; reason == "" && codec == "hevc" && limit > 0 {
				// Only the adaptive encoder enforces an advertised HEVC level.
				level := adaptiveHEVCLevel(rung{width: w, height: h, peakKbps: peak}, v.FrameRate)
				if !adaptive || v.FrameRate <= 0 || level == 0 || level > limit {
					reason = "decoder_level_unverified"
				}
			}
			if maxKbps > 0 && peak > maxKbps {
				reason = "bitrate_limit"
			}
			if reason != "" {
				limits = append(limits, core.QualityLimitation{Height: step.height, Reason: reason})
				continue
			}
			id := fmt.Sprintf("%dp%dk-%s-%s", step.height, step.video, codec, dr)
			if slices.ContainsFunc(out, func(q core.QualityVariant) bool { return q.Rung.ID == id }) {
				continue
			}
			d.Reason = "explicit Vela quality " + id
			out = append(out, core.QualityVariant{Decision: d, Rung: core.HLSRung{ID: id, Width: w, Height: h, VideoBitrate: step.video * 1000, PeakVideoBitrate: qualityPeakVideoKbps(peak, audio) * 1000, Codec: codec, DynamicRange: dr, AverageBandwidth: d.BitrateKbps * 1000, Bandwidth: peak * 1000}})
		}
	}
	if !adaptive && sourceRange != rangeSDR && !keepHDR {
		reason := "display_hdr_undeclared"
		if !support.HEVC {
			reason = "encoder_hevc_hdr_unavailable"
		} else if !slices.Contains(caps.VideoCodecs, "hevc") {
			reason = "decoder_hevc_unavailable"
		}
		limits = append(limits, core.QualityLimitation{Height: v.Height, Reason: reason})
	}
	return out, limits
}

// Apply the session muxer's audio rules before comparing a quality variant
// with the base. Some manual variants still carry a copied audio decision.
func adaptiveQualityVariant(info core.MediaInfo, container string, q core.QualityVariant) core.QualityVariant {
	d := q.Decision
	d.Container = container
	if d.AudioStream < 0 {
		if a := pickAudio(info, -1); a != nil {
			d.AudioStream = a.Index
		}
	}
	d = segmentableAudio(info, d)
	if d.AudioCopy && d.AudioCodec == "" {
		if a := findStream(info, d.AudioStream, "audio"); a != nil {
			d.AudioCodec = a.Codec
		}
	}
	if q.Rung.VideoBitrate > 0 {
		d.BitrateKbps = q.Rung.VideoBitrate/1000 + decisionAudioKbps(info, d)
		q.Rung.AverageBandwidth = d.BitrateKbps * 1000
		q.Rung.Bandwidth = peakKbps(d.BitrateKbps) * 1000
	}
	q.Decision = d
	return q
}

func qualityRung(info core.MediaInfo, q core.QualityVariant) rung {
	return rung{id: q.Rung.ID, decision: q.Decision, width: q.Rung.Width, height: q.Rung.Height, avgKbps: q.Rung.AverageBandwidth / 1000, peakKbps: q.Rung.Bandwidth / 1000, codecs: rungCodecs(info, q.Decision, q.Rung.Height)}
}

func qualityMetadata(info core.MediaInfo, r rung) (video, peak int, codec, dr string) {
	codec, dr = r.decision.VideoCodec, rangeSDR
	if v := info.Video(); v != nil {
		if r.decision.VideoCopy {
			codec = v.Codec
			dr = dynamicRange(v)
			if r.decision.StripDolbyVision {
				dr = encodedRange(v)
			}
		} else if codec == "hevc" && !r.decision.ToneMap {
			dr = encodedRange(v)
		}
	}
	video = videoBitrate(info, r.decision) * 1000
	if r.decision.VideoCopy {
		video = max(0, r.avgKbps-decisionAudioKbps(info, r.decision)) * 1000
	}
	// Match the payload budget boundHLSRate reserves for maxrate plus the
	// VBV reservoir over the HLS peak window, excluding audio/mux overhead.
	peak = max(video, qualityPeakVideoKbps(r.peakKbps, decisionAudioKbps(info, r.decision))*1000)
	if r.decision.VideoCopy {
		peak = max(video, (r.peakKbps-decisionAudioKbps(info, r.decision))*1000)
	}
	return
}

func qualityHEVCProfileAllowed(profiles []string, dr string) bool {
	if len(profiles) == 0 {
		return true
	}
	want := "main"
	if dr != rangeSDR {
		want = "main10"
	}
	for _, profile := range profiles {
		if strings.ReplaceAll(strings.ToLower(profile), " ", "") == want {
			return true
		}
	}
	return false
}

func qualityPeakVideoKbps(totalPeak, audio int) int {
	return max(0, ((totalPeak-16)*10+10)/11-audio)
}
