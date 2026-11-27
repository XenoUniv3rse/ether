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
	keepHDR := codec == "hevc" && sourceRange != rangeSDR && sourceRange != rangeDV && slices.Contains(caps.HDRFormats, sourceRange) && !caps.ExplicitNoHDR
	steps := []struct{ height, video int }{{2160, 40000}, {1080, 20000}, {1080, 12000}, {1080, 8000}, {720, 6000}, {720, 4000}, {480, 1500}, {360, 800}}
	for _, step := range steps {
		// Use source naming classes (width for scope movies), never duplicate a
		// taller class by clamping it to the source dimensions.
		if v.Height < step.height && v.Width < step.height*16/9 {
			continue
		}
		w, h := fitRungBox(v, step.height)
		ranges := []string{rangeSDR}
		if keepHDR {
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
			case codec == "hevc" && caps.VideoLevels[codec] > 0:
				// HEVC output levels have no enforced encoder contract yet. Required
				// decoder bounds must not be satisfied using a guessed RFC6381 token.
				reason = "decoder_level_unverified"
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
			if maxKbps > 0 && peak > maxKbps {
				reason = "bitrate_limit"
			}
			if reason != "" {
				limits = append(limits, core.QualityLimitation{Height: step.height, Reason: reason})
				continue
			}
			id := fmt.Sprintf("%dp%dk-%s-%s", step.height, step.video, codec, dr)
			d.Reason = "explicit Vela quality " + id
			out = append(out, core.QualityVariant{Decision: d, Rung: core.HLSRung{ID: id, Width: w, Height: h, VideoBitrate: step.video * 1000, PeakVideoBitrate: qualityPeakVideoKbps(peak, audio) * 1000, Codec: codec, DynamicRange: dr, AverageBandwidth: d.BitrateKbps * 1000, Bandwidth: peak * 1000}})
		}
	}
	if sourceRange != rangeSDR && !keepHDR {
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

// Use the same segmentable audio decision as the base. A copy of an
// unsupported audio codec can change the mux budget during session startup.
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
