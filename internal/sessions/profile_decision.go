package sessions

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// ProfileDiagnostic is a redacted reason emitted only to an explicitly
// installed diagnostic hook. It never includes client profile values.
type ProfileDiagnostic string

const (
	ProfileRequiredConditionUnknown ProfileDiagnostic = "required_condition_unknown"
	ProfileDirectRejected           ProfileDiagnostic = "direct_rejected"
	ProfileDeliveryRejected         ProfileDiagnostic = "delivery_rejected"
	ProfileFallbackTranscode        ProfileDiagnostic = "fallback_transcode"
	ProfileFallbackRemux            ProfileDiagnostic = "fallback_remux"
	ProfileNoDelivery               ProfileDiagnostic = "no_delivery"
)

func (b *Broker) profileDiagnostic(event ProfileDiagnostic) {
	if b.deps.OnProfileDiagnostic != nil {
		b.deps.OnProfileDiagnostic(event)
	}
}

const reasonVideoTranscodingOff = "needs a video transcode, and transcoding is off"

// videoOff is videoTranscodingOff for one request: the server switch, or the
// owner's switch for this viewer carried on the caps.
func (b *Broker) videoOff(caps core.ClientCaps) bool {
	return caps.NoVideoTranscode || b.videoTranscodingOff()
}

// stampViewerTranscoding marks caps for a viewer whose video must not be
// re-encoded, so the resolver, the decision and the ladder all see it.
func (b *Broker) stampViewerTranscoding(v *core.Viewer, caps *core.ClientCaps) {
	if b != nil && v != nil && b.deps.ViewerVideoTranscodingOff != nil && b.deps.ViewerVideoTranscodingOff(v) {
		caps.NoVideoTranscode = true
	}
}

func (b *Broker) videoTranscodingOff() bool {
	if b == nil {
		return false
	}
	if b.deps.VideoTranscodingOff != nil && b.deps.VideoTranscodingOff() {
		return true
	}
	if v, ok := b.deps.Transcoder.(interface{ VideoTranscodingOff() bool }); ok && v.VideoTranscodingOff() {
		return true
	}
	return false
}

func decisionEncodes(d core.Decision, info core.MediaInfo) bool {
	if d.Kind == core.DecisionTranscode {
		return true
	}
	if d.Kind != core.DecisionRemux {
		return false
	}
	return info.Video() != nil && !d.VideoCopy || profileStream(info, "audio", d.AudioStream) != nil && !d.AudioCopy
}

func decisionEncodesVideo(d core.Decision, info core.MediaInfo) bool {
	if info.Video() == nil {
		return false
	}
	if d.Kind == core.DecisionTranscode {
		return true
	}
	if d.Kind != core.DecisionRemux {
		return false
	}
	return !d.VideoCopy
}

func applySubtitleProfiles(caps *core.ClientCaps) {
	if caps.Profiles == nil || caps.Profiles.Subtitles == nil {
		return
	}
	caps.SubtitleCodecs = make([]string, 0)
	for _, p := range caps.Profiles.Subtitles {
		if !strings.EqualFold(p.Method, "embed") {
			continue
		}
		codec := strings.ToLower(p.Format)
		switch codec {
		case "pgs":
			caps.SubtitleCodecs = append(caps.SubtitleCodecs, "pgs", "hdmv_pgs_subtitle")
		case "dvdsub", "vobsub":
			caps.SubtitleCodecs = append(caps.SubtitleCodecs, "vobsub", "dvd_subtitle")
		default:
			if codec != "" {
				caps.SubtitleCodecs = append(caps.SubtitleCodecs, codec)
			}
		}
	}
}

func profileEqualsAny(value, want string) bool {
	for _, v := range strings.Split(want, "|") {
		if strings.EqualFold(value, strings.TrimSpace(v)) {
			return true
		}
	}
	return false
}

func profileName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.HasPrefix(s, "pcm_") {
		return "pcm"
	}
	switch s {
	case "ts", "mpegts":
		return "mpegts"
	case "m4v", "m4a", "m4b", "mov", "fmp4":
		return "mp4"
	case "adts":
		return "aac"
	case "oga":
		return "ogg"
	case "matroska", "mka":
		return "mkv"
	case "weba":
		return "webm"
	case "mp4a":
		return "aac"
	case "ac-3":
		return "ac3"
	case "ec3", "e-ac3":
		return "eac3"
	case "dca":
		return "dts"
	case "mlp":
		return "truehd"
	case "avc", "avc1":
		return "h264"
	case "h265":
		return "hevc"
	}
	return s
}

func profileAudioContainerMatch(containers []string, container, audioCodec string) bool {
	if profileContains(containers, container) {
		return true
	}
	// Ogg/Opus containers are frequently declared interchangeably as "ogg" or "opus".
	c := profileName(container)
	if c == "ogg" && strings.EqualFold(audioCodec, "opus") && profileContains(containers, "opus") {
		return true
	}
	if c == "opus" && profileContains(containers, "ogg") {
		return true
	}
	return false
}
func profileContains(xs []string, name string) bool {
	if len(xs) == 0 {
		return true
	}
	for _, x := range xs {
		if profileName(x) == profileName(name) {
			return true
		}
	}
	return false
}
func profileStream(info core.MediaInfo, kind string, index int) *core.StreamInfo {
	var first, def *core.StreamInfo
	for i := range info.Streams {
		s := &info.Streams[i]
		if s.Type != kind {
			continue
		}
		if s.Index == index {
			return s
		}
		if first == nil {
			first = s
		}
		if s.Default && def == nil {
			def = s
		}
	}
	if def != nil {
		return def
	}
	return first
}

// Unknown required predicates cannot authorize a profile. Optional unknown
// predicates do not constrain playback, as declared by the client.
func profileConditions(cs []core.ClientProfileCondition, info core.MediaInfo, stream *core.StreamInfo) bool {
	match, unknown := evaluateProfileConditions(cs, info, stream)
	return match && !unknown
}

// profileVideoRangeValue is the Jellyfin VideoRangeType a codec condition
// compares. Eight-bit untagged 4:2:0 is SDR once Dolby Vision is known to be
// absent, the same evidence as frontend.KnownSDR. A picture whose range cannot
// be classified stays empty so a required condition fails closed and playback
// can fall back to an encode. As in Jellyfin, Equals and NotEquals DOVI match
// only a picture with no usable base layer (profile 5, or a profile whose
// compatibility is unknown). Profiles 7, 8 and 10 with an HDR10, SDR or HLG
// base layer report their own range type, so `NotEquals DOVI` still direct
// plays their base layer (Streamyfin, Findroid and Jellyfin's own clients rely
// on that).
func profileVideoRangeValue(stream *core.StreamInfo, c core.ClientProfileCondition) string {
	if stream == nil || (stream.Type != "" && stream.Type != "video") {
		return ""
	}
	dv, known := dolbyVisionEvidence(stream)
	if dv {
		value := dolbyVisionRangeType(stream)
		if value == "DOVIInvalid" && dolbyVisionEquality(c) {
			return "DOVI"
		}
		return value
	}
	if !known {
		return ""
	}
	if value, ok := plainVideoRangeType(stream); ok {
		return value
	}
	if dolbyVisionEquality(c) {
		return "non-DOVI"
	}
	return ""
}

func dolbyVisionEquality(c core.ClientProfileCondition) bool {
	if !strings.EqualFold(strings.TrimSpace(c.Value), "DOVI") {
		return false
	}
	switch strings.ToLower(c.Operator) {
	case "equals", "notequals":
		return true
	default:
		return false
	}
}

func dolbyVisionEvidence(stream *core.StreamInfo) (present, known bool) {
	if stream.DolbyVision || (stream.DolbyVisionConfigurationPresent != nil && *stream.DolbyVisionConfigurationPresent) || dolbyVisionCodecTag(stream.CodecTag) {
		return true, true
	}
	if stream.DolbyVisionConfigurationPresent != nil && !*stream.DolbyVisionConfigurationPresent {
		return false, true
	}
	// AVC has no Dolby Vision configuration, so a missing container probe is
	// still enough to classify the picture.
	switch strings.ToLower(stream.Codec) {
	case "h264", "avc":
		return false, true
	default:
		return false, false
	}
}

func dolbyVisionCodecTag(tag string) bool {
	switch strings.ToLower(tag) {
	case "dvh1", "dvhe", "dva1", "dvav", "dovi", "dav1":
		return true
	default:
		return false
	}
}

func dolbyVisionRangeType(stream *core.StreamInfo) string {
	profile := stream.DolbyVisionProfile
	if stream.ProbedDolbyVisionProfile != nil && *stream.ProbedDolbyVisionProfile > 0 {
		profile = *stream.ProbedDolbyVisionProfile
	}
	compat := stream.DolbyVisionCompatibilityID
	hdr10Plus := stream.HDR10Plus != nil && *stream.HDR10Plus
	switch profile {
	case 5:
		return "DOVI"
	case 7:
		if hdr10Plus {
			return "DOVIWithELHDR10Plus"
		}
		return "DOVIWithEL"
	case 8:
		switch compat {
		case 1:
			if hdr10Plus {
				return "DOVIWithHDR10Plus"
			}
			return "DOVIWithHDR10"
		case 2:
			return "DOVIWithSDR"
		case 4:
			return "DOVIWithHLG"
		default:
			return "DOVIInvalid"
		}
	case 10:
		switch compat {
		case 0:
			return "DOVI"
		case 1:
			if hdr10Plus {
				return "DOVIWithHDR10Plus"
			}
			return "DOVIWithHDR10"
		case 2:
			return "DOVIWithSDR"
		case 4:
			return "DOVIWithHLG"
		default:
			return "DOVIInvalid"
		}
	default:
		return "DOVI"
	}
}

func plainVideoRangeType(stream *core.StreamInfo) (string, bool) {
	if stream.HDR10Plus != nil && *stream.HDR10Plus {
		return "HDR10Plus", true
	}
	switch strings.ToLower(stream.ColorTransfer) {
	case "smpte2084":
		return "HDR10", true
	case "arib-std-b67":
		return "HLG", true
	}
	if frontend.KnownSDR(*stream) {
		return "SDR", true
	}
	return "", false
}

// stampEncodedVideoRange records the picture an encode or Dolby Vision strip
// actually delivers. An H.264 encode, and any tone-mapped encode, is 8-bit
// SDR. An HEVC encode that keeps HDR keeps the base-layer transfer and drops
// Dolby Vision metadata. Without this, a required VideoRangeType condition
// stays unknown on the output and a playable title is refused.
func stampEncodedVideoRange(s *core.StreamInfo, d core.Decision) {
	if d.VideoCopy {
		if !d.StripDolbyVision {
			return
		}
		no := false
		s.DolbyVision = false
		s.DolbyVisionConfigurationPresent = &no
		s.DolbyVisionProfile = 0
		s.DolbyVisionCompatibilityID = 0
		s.CodecTag = ""
		return
	}
	keepHDR := strings.EqualFold(d.VideoCodec, "hevc") && !d.ToneMap && encodedPictureHDR(s)
	transfer := "bt709"
	if keepHDR {
		if strings.EqualFold(s.ColorTransfer, "arib-std-b67") || s.DolbyVisionCompatibilityID == 4 {
			transfer = "arib-std-b67"
		} else {
			transfer = "smpte2084"
		}
	}
	no := false
	s.DolbyVision = false
	s.DolbyVisionConfigurationPresent = &no
	s.DolbyVisionProfile = 0
	s.DolbyVisionCompatibilityID = 0
	s.CodecTag = ""
	s.HDR10Plus = &no
	s.ColorTransfer = transfer
	if keepHDR {
		if s.BitDepth < 10 {
			s.BitDepth = 10
		}
		depth := s.BitDepth
		s.BitsPerRawSample = &depth
		if strings.EqualFold(d.VideoCodec, "hevc") {
			s.Profile = "Main 10"
			s.PixelFormat = "p010le"
		}
		return
	}
	s.BitDepth = 8
	depth := 8
	s.BitsPerRawSample = &depth
	s.PixelFormat = "yuv420p"
	if strings.EqualFold(d.VideoCodec, "hevc") {
		s.Profile = "Main"
	}
}

// applyProfileRangeCaps lets a device profile's VideoRangeType list drive the
// direct-play decision. Equals and EqualsAny name ranges the client can play.
// A list that only says SDR does not advertise HDR or Dolby Vision.
//
// A profile that never lists the ranges it plays (Fladder declares no codec
// conditions; Wholphin only excludes ranges with NotEquals) accepts every
// range in Jellyfin, which direct-plays HDR10 and Dolby Vision base layers to
// it and leaves display mapping to the client. Treat it as HDR10-capable, so
// its declared conditions (evaluated for direct play) decide instead of a
// forced SDR tone-map encode.
//
// Dolby Vision inference applies only to profiles that express range support
// via VideoRangeType conditions (Wholphin, Jellyfin Android TV, Streamyfin)
// while leaving the DV profile list implicit. It must never override an
// explicit client declaration (Vela), an owner/host override
// (DolbyVisionBaseOnly), or an explicit DV profile list (DVProfilesExplicit).
func applyProfileRangeCaps(caps core.ClientCaps) core.ClientCaps {
	if caps.Profiles == nil {
		return caps
	}
	listed := false
	hasRangeCond := false
	for _, codec := range caps.Profiles.Codecs {
		videoCodec := codec.MediaType == "" || strings.EqualFold(codec.MediaType, "video")
		for _, cond := range codec.Conditions {
			if videoCodec && strings.EqualFold(cond.Property, "VideoRangeType") {
				hasRangeCond = true
			}
			listed = noteProfileRange(&caps, cond) || listed
		}
		if videoCodec && !hasRangeCond {
			for _, cond := range codec.ApplyConditions {
				if strings.EqualFold(cond.Property, "VideoRangeType") {
					hasRangeCond = true
					break
				}
			}
		}
		// ApplyConditions say when a codec profile applies (Wholphin lists
		// the ranges it cannot play there), not which ranges are playable.
	}
	if !listed && !caps.ExplicitNoHDR {
		hasVideo := false
		if caps.Profiles.DirectPlay == nil {
			for _, tp := range caps.Profiles.Transcoding {
				if tp.MediaType == "" || strings.EqualFold(tp.MediaType, "video") {
					hasVideo = true
					break
				}
			}
		} else {
			for _, dp := range caps.Profiles.DirectPlay {
				if dp.MediaType == "" || strings.EqualFold(dp.MediaType, "video") {
					hasVideo = true
					break
				}
			}
		}
		if hasVideo {
			if caps.Protocol != "vela" && !caps.HDR10 && profileAllowsVideoRange(caps.Profiles, "HDR10") {
				caps.HDR10 = true
			}
			// A profile that excludes only unsupported VideoRangeType values
			// (Wholphin, Jellyfin Android TV, Streamyfin) accepts every
			// unexcluded DV range in Jellyfin. Range-less profiles (including
			// Vela's synthetic profile), explicit client declarations, owner
			// overrides, and explicit DV profile lists must not be overridden.
			if hasRangeCond && caps.Protocol != "vela" && !caps.DolbyVision && !caps.DolbyVisionBaseOnly && !caps.DVProfilesExplicit && len(caps.DVProfiles) == 0 {
				var dvProfiles []int
				if profileAllowsVideoRange(caps.Profiles, "DOVI") {
					dvProfiles = append(dvProfiles, 5)
				}
				if profileAllowsVideoRange(caps.Profiles, "DOVIWithHDR10") {
					dvProfiles = append(dvProfiles, 8)
				}
				if len(dvProfiles) > 0 {
					caps.DolbyVision = true
					caps.DVProfiles = dvProfiles
				}
			}
		}
	}
	return caps
}

// profileAllowsVideoRange reports whether video codec profiles in p leave
// rangeType unexcluded. Profiles with ApplyConditions (Wholphin / Jellyfin
// Android TV) apply only when their VideoRangeType ApplyConditions match
// rangeType, and exclude it when their Conditions reject rangeType.
func profileAllowsVideoRange(p *core.ClientCapabilityProfiles, rangeType string) bool {
	if p == nil {
		return true
	}
	for _, cp := range p.Codecs {
		if cp.MediaType != "" && !strings.EqualFold(cp.MediaType, "video") {
			continue
		}
		if !profileContains(cp.Codecs, "hevc") {
			continue
		}
		if len(cp.ApplyConditions) > 0 {
			hasRangeApply := false
			applies := true
			for _, cond := range cp.ApplyConditions {
				if !strings.EqualFold(cond.Property, "VideoRangeType") {
					continue
				}
				hasRangeApply = true
				ok, known := profileConditionHolds(rangeType, cond)
				if known && !ok {
					applies = false
					break
				}
			}
			if !hasRangeApply || !applies {
				continue
			}
		} else if rangeType != "SDR" {
			for _, cond := range cp.Conditions {
				prop, op := strings.ToLower(cond.Property), strings.ToLower(cond.Operator)
				if prop == "videobitdepth" && (op == "lessthanequal" || op == "equals") {
					if depth, err := strconv.Atoi(cond.Value); err == nil && depth < 10 {
						return false
					}
				}
				if prop == "videoprofile" && (op == "equals" || op == "equalsany") && !strings.Contains(strings.ToLower(cond.Value), "main 10") && !strings.Contains(strings.ToLower(cond.Value), "main10") {
					return false
				}
			}
		}
		for _, cond := range cp.Conditions {
			if !strings.EqualFold(cond.Property, "VideoRangeType") {
				continue
			}
			ok, known := profileConditionHolds(rangeType, cond)
			if known && !ok {
				return false
			}
		}
	}
	return true
}

// noteProfileRange reports whether cond positively lists playable ranges.
func noteProfileRange(caps *core.ClientCaps, cond core.ClientProfileCondition) bool {
	if !strings.EqualFold(cond.Property, "VideoRangeType") {
		return false
	}
	switch strings.ToLower(cond.Operator) {
	case "equals", "equalsany":
	default:
		return false
	}
	allowHDR := !caps.ExplicitNoHDR
	allowDV := allowHDR && !caps.DolbyVisionBaseOnly && !(caps.DVProfilesExplicit && len(caps.DVProfiles) == 0)
	for _, part := range strings.Split(cond.Value, "|") {
		switch strings.ToUpper(strings.TrimSpace(part)) {
		case "HDR10", "HLG", "HDR10PLUS":
			if allowHDR {
				caps.HDR10 = true
			}
		case "DOVI", "DOVIWITHSDR", "DOVIWITHEL":
			if allowDV {
				caps.DolbyVision = true
			}
		case "DOVIWITHHDR10", "DOVIWITHHLG", "DOVIWITHHDR10PLUS", "DOVIWITHELHDR10PLUS":
			if allowHDR {
				caps.HDR10 = true
			}
			if allowDV {
				caps.DolbyVision = true
			}
		}
	}
	return true
}

func encodedPictureHDR(s *core.StreamInfo) bool {
	dv := s.DolbyVision || (s.DolbyVisionConfigurationPresent != nil && *s.DolbyVisionConfigurationPresent) || dolbyVisionCodecTag(s.CodecTag)
	switch strings.ToLower(s.ColorTransfer) {
	case "smpte2084", "arib-std-b67":
		return true
	}
	if !dv {
		return false
	}
	if s.DolbyVisionProfile == 5 || (s.ProbedDolbyVisionProfile != nil && *s.ProbedDolbyVisionProfile == 5) {
		return false
	}
	if s.DolbyVisionCompatibilityID == 2 {
		return false
	}
	return s.DolbyVisionProfile == 7 || s.DolbyVisionProfile == 8 || s.DolbyVisionProfile == 10
}

// unknown reports an unsupported or unavailable required value separately
// from a false condition, which can make an ApplyConditions profile inapplicable.
func evaluateProfileConditions(cs []core.ClientProfileCondition, info core.MediaInfo, stream *core.StreamInfo) (bool, bool) {
	for _, c := range cs {
		value, zeroKnown := profileMeasurement(c, info, stream)
		// A missing measurement is Jellyfin's null: a required condition
		// fails closed and an optional one does not constrain playback.
		// A reported zero stays a real zero (reference frames, stream counts).
		if value == "" || (value == "0" && !zeroKnown) {
			if c.Required {
				return false, true
			}
			continue
		}
		// Jellyfin treats HDR10Plus as HDR10 when that comparison succeeds,
		// then still compares the HDR10Plus token itself.
		if strings.EqualFold(c.Property, "videorangetype") && strings.EqualFold(value, "HDR10Plus") {
			if ok, known := profileConditionHolds("HDR10", c); known && ok {
				continue
			}
		}
		ok, known := profileConditionHolds(value, c)
		if !known && c.Required {
			return false, true
		}
		if known && !ok {
			return false, false
		}
	}
	return true, false
}

func profileConditionHolds(value string, c core.ClientProfileCondition) (ok, known bool) {
	want := strings.TrimSpace(c.Value)
	switch strings.ToLower(c.Operator) {
	case "equals":
		return strings.EqualFold(value, want), true
	case "notequals":
		// Jellyfin Android TV rejects an unsupported range by NotEquals
		// against a pipe-separated list, and only applies that profile when
		// the picture is already in the list. A single token stays an exact
		// comparison, which is what NotEquals DOVI clients send.
		if strings.EqualFold(c.Property, "videorangetype") && strings.Contains(want, "|") {
			return !profileEqualsAny(value, want), true
		}
		return !strings.EqualFold(value, want), true
	case "equalsany":
		return profileEqualsAny(value, want), true
	case "lessthanequal", "greaterthanequal":
		a, e1 := strconv.ParseFloat(value, 64)
		b, e2 := strconv.ParseFloat(want, 64)
		if e1 != nil || e2 != nil {
			return false, false
		}
		if strings.EqualFold(c.Operator, "lessthanequal") {
			return a <= b, true
		}
		return a >= b, true
	default:
		return false, false
	}
}

// profileMeasurement reads one Jellyfin ProfileConditionValue.
// zeroKnown is set when "0" is a real measurement rather than "not reported".
// PacketLength, VideoTimestamp, Has64BitOffsets and VideoRotation stay empty:
// the probe does not carry them, so they follow Jellyfin's null rule.
func profileMeasurement(c core.ClientProfileCondition, info core.MediaInfo, stream *core.StreamInfo) (value string, zeroKnown bool) {
	switch strings.ToLower(strings.TrimSpace(c.Property)) {
	case "videorangetype":
		return profileVideoRangeValue(stream, c), false
	case "videowidth", "width":
		if stream != nil {
			return strconv.Itoa(stream.Width), false
		}
	case "videoheight", "height":
		if stream != nil {
			return strconv.Itoa(stream.Height), false
		}
	case "videoframerate", "framerate":
		if stream != nil && stream.FrameRate > 0 {
			return strconv.FormatFloat(stream.FrameRate, 'f', -1, 64), false
		}
	case "videobitdepth":
		if stream != nil {
			return strconv.Itoa(stream.BitDepth), false
		}
	case "videolevel":
		if stream != nil {
			return strconv.Itoa(stream.Level), false
		}
	case "videoprofile", "audioprofile":
		if stream != nil {
			return stream.Profile, false
		}
	case "audiochannels":
		if stream != nil {
			return strconv.Itoa(stream.Channels), false
		}
	case "audiosamplerate":
		if stream != nil {
			return strconv.Itoa(stream.SampleRate), false
		}
	case "audiobitdepth":
		if stream != nil && stream.Type == "audio" && stream.BitsPerRawSample != nil {
			return strconv.Itoa(*stream.BitsPerRawSample), false
		}
	case "videobitrate", "audiobitrate":
		if stream != nil {
			return strconv.Itoa(stream.BitrateKbps * 1000), false
		}
	case "containerbitrate":
		return strconv.Itoa(info.BitrateKbps * 1000), false
	case "videocodectag":
		if stream != nil {
			return stream.CodecTag, false
		}
	case "refframes":
		if stream != nil && stream.ReferenceFrames != nil {
			return strconv.Itoa(*stream.ReferenceFrames), true
		}
	case "isanamorphic":
		return anamorphicValue(stream)
	case "isinterlaced":
		return interlacedValue(stream)
	case "isavc":
		return avcValue(stream)
	case "issecondaryaudio":
		return secondaryAudioValue(info, stream)
	case "numaudiostreams":
		return strconv.Itoa(countStreams(info, "audio")), true
	case "numvideostreams":
		return strconv.Itoa(countStreams(info, "video")), true
	case "numstreams":
		return strconv.Itoa(len(info.Streams)), true
	case "videotimestamp", "packetlength", "has64bitoffsets", "videorotation":
		return "", false
	}
	return "", false
}

func countStreams(info core.MediaInfo, kind string) int {
	n := 0
	for _, s := range info.Streams {
		if s.Type == kind {
			n++
		}
	}
	return n
}

func anamorphicValue(stream *core.StreamInfo) (string, bool) {
	if stream == nil || stream.Type == "audio" || stream.Type == "subtitle" {
		return "", false
	}
	sar := strings.TrimSpace(stream.SampleAspectRatio)
	if sar == "" || strings.EqualFold(sar, "N/A") || sar == "0:1" {
		return "", false
	}
	sar = strings.ReplaceAll(sar, "/", ":")
	n, d, ok := strings.Cut(sar, ":")
	if !ok {
		return "", false
	}
	num, e1 := strconv.ParseFloat(n, 64)
	den, e2 := strconv.ParseFloat(d, 64)
	if e1 != nil || e2 != nil || den == 0 {
		return "", false
	}
	if num/den > 0.99 && num/den < 1.01 {
		return "false", false
	}
	return "true", false
}

func interlacedValue(stream *core.StreamInfo) (string, bool) {
	if stream == nil || (stream.Type != "" && stream.Type != "video") {
		return "", false
	}
	if stream.ProbedInterlaced != nil {
		return strconv.FormatBool(*stream.ProbedInterlaced), false
	}
	switch strings.ToLower(stream.FieldOrder) {
	case "progressive":
		return "false", false
	case "tt", "bb", "tb", "bt":
		return "true", false
	}
	if stream.Interlaced {
		return "true", false
	}
	return "", false
}

func avcValue(stream *core.StreamInfo) (string, bool) {
	if stream == nil || (stream.Type != "" && stream.Type != "video") {
		return "", false
	}
	switch strings.ToLower(stream.CodecTag) {
	case "avc1", "avc3":
		return "true", false
	}
	if stream.NALLengthSize != nil {
		return strconv.FormatBool(*stream.NALLengthSize > 0), false
	}
	return "", false
}

func secondaryAudioValue(info core.MediaInfo, stream *core.StreamInfo) (string, bool) {
	if stream == nil || stream.Type != "audio" {
		return "", false
	}
	first := -1
	for _, s := range info.Streams {
		if s.Type == "audio" {
			first = s.Index
			break
		}
	}
	if first < 0 {
		return "", false
	}
	return strconv.FormatBool(stream.Index != first), false
}

func profileRestrictions(p *core.ClientCapabilityProfiles, info core.MediaInfo, video, audio string, audioIndex int) bool {
	audioOnly := info.Video() == nil && audio != ""
	for _, c := range p.Containers {
		if audioOnly {
			if (c.MediaType == "" || strings.EqualFold(c.MediaType, "audio")) && profileAudioContainerMatch(c.Containers, info.Container, audio) && !profileConditions(c.Conditions, info, profileStream(info, "audio", audioIndex)) {
				return false
			}
			continue
		}
		if (c.MediaType == "" || strings.EqualFold(c.MediaType, "video")) && profileContains(c.Containers, info.Container) && !profileConditions(c.Conditions, info, info.Video()) {
			return false
		}
	}
	for _, c := range p.Codecs {
		var stream *core.StreamInfo
		var codec string
		switch strings.ToLower(c.MediaType) {
		case "video":
			if audioOnly {
				continue
			}
			stream = info.Video()
			codec = video
		case "audio", "videoaudio":
			if audioOnly && strings.EqualFold(c.MediaType, "videoaudio") {
				continue
			}
			stream = profileStream(info, "audio", audioIndex)
			codec = audio
		default:
			for _, x := range append(append([]core.ClientProfileCondition(nil), c.Conditions...), c.ApplyConditions...) {
				if x.Required {
					return false
				}
			}
			continue
		}
		if !profileContains(c.Codecs, codec) || !profileAudioContainerMatch(c.Containers, info.Container, codec) {
			continue
		}
		applies, unknown := evaluateProfileConditions(c.ApplyConditions, info, stream)
		if unknown {
			return false
		}
		if !applies {
			continue
		}
		if !profileConditions(c.Conditions, info, stream) {
			return false
		}
	}
	return true
}

func profileUnknownRequired(p *core.ClientCapabilityProfiles, info core.MediaInfo, d core.Decision) bool {
	video, audio := info.Video(), profileStream(info, "audio", d.AudioStream)
	audioOnly := video == nil && audio != nil
	for _, c := range p.Containers {
		if audioOnly {
			if (c.MediaType == "" || strings.EqualFold(c.MediaType, "audio")) && profileAudioContainerMatch(c.Containers, info.Container, audio.Codec) {
				_, unknown := evaluateProfileConditions(c.Conditions, info, audio)
				if unknown {
					return true
				}
			}
			continue
		}
		if (c.MediaType == "" || strings.EqualFold(c.MediaType, "video")) && profileContains(c.Containers, info.Container) {
			_, unknown := evaluateProfileConditions(c.Conditions, info, video)
			if unknown {
				return true
			}
		}
	}
	for _, c := range p.Codecs {
		var stream *core.StreamInfo
		switch strings.ToLower(c.MediaType) {
		case "video":
			if audioOnly {
				continue
			}
			stream = video
		case "audio", "videoaudio":
			if audioOnly && strings.EqualFold(c.MediaType, "videoaudio") {
				continue
			}
			stream = audio
		default:
			continue
		}
		if stream == nil || !profileContains(c.Codecs, stream.Codec) || !profileAudioContainerMatch(c.Containers, info.Container, stream.Codec) {
			continue
		}
		applies, unknown := evaluateProfileConditions(c.ApplyConditions, info, stream)
		if unknown {
			return true
		}
		if applies {
			_, unknown = evaluateProfileConditions(c.Conditions, info, stream)
			if unknown {
				return true
			}
		}
	}
	return false
}

func profileDirectAllowed(p *core.ClientCapabilityProfiles, info core.MediaInfo, d core.Decision) bool {
	if p == nil {
		return true
	}
	video, audio := "", ""
	if s := info.Video(); s != nil {
		video = s.Codec
	}
	if s := profileStream(info, "audio", d.AudioStream); s != nil {
		audio = s.Codec
	}
	audioOnly := video == "" && audio != ""
	if p.MaxStaticBitrate > 0 && info.BitrateKbps > 0 && int64(info.BitrateKbps)*1000 > int64(p.MaxStaticBitrate) {
		return false
	}
	if audioOnly && p.MaxStaticMusicBitrate > 0 && info.BitrateKbps > 0 && int64(info.BitrateKbps)*1000 > int64(p.MaxStaticMusicBitrate) {
		return false
	}
	if !profileRestrictions(p, info, video, audio, d.AudioStream) {
		return false
	}
	// A profile that omits DirectPlayProfiles (e.g. Infuse, which posts only
	// TranscodingProfiles and SubtitleProfiles with EnableDirectPlay=true) does
	// not restrict direct play by container/codec pair; static bitrate and
	// codec/container conditions above still apply, while an explicit empty
	// DirectPlayProfiles slice still rejects direct play.
	if p.DirectPlay == nil {
		return true
	}
	for _, candidate := range p.DirectPlay {
		if audioOnly {
			// Music and audiobooks: an audio-only source matches the
			// client's audio (or untyped) direct-play profiles; video
			// codec lists say nothing about it.
			if (candidate.MediaType == "" || strings.EqualFold(candidate.MediaType, "audio")) && profileAudioContainerMatch(candidate.Containers, info.Container, audio) && profileContains(candidate.AudioCodecs, audio) {
				return true
			}
			continue
		}
		if (candidate.MediaType == "" || strings.EqualFold(candidate.MediaType, "video")) && profileContains(candidate.Containers, info.Container) && profileContains(candidate.VideoCodecs, video) && profileContains(candidate.AudioCodecs, audio) {
			return true
		}
	}
	return false
}

func encodedAudioChannels(d core.Decision, info core.MediaInfo) int {
	if d.AudioChannels > 0 {
		return d.AudioChannels
	}
	ch := 2
	if source := profileStream(info, "audio", d.AudioStream); source != nil && source.Channels > 0 {
		ch = source.Channels
	}
	switch strings.ToLower(strings.TrimSpace(d.AudioCodec)) {
	case "ac3", "eac3":
		return min(ch, 6)
	default:
		return min(ch, 2)
	}
}

func profileDeliveryAllowed(p *core.ClientCapabilityProfiles, d core.Decision, info core.MediaInfo, delivery string) bool {
	if p == nil {
		return true
	}
	output := info
	output.Container = d.Container
	// Segmented output maps one video and the selected audio, not every
	// track in the input file. Retain their input indices for the encoder,
	// but evaluate output predicates (notably IsSecondaryAudio) against
	// this actual stream set. A second language becomes the only audio.
	output.Streams = nil
	if video := info.Video(); video != nil {
		output.Streams = append(output.Streams, *video)
	}
	if audio := profileStream(info, "audio", d.AudioStream); audio != nil {
		output.Streams = append(output.Streams, *audio)
	}
	for i := range output.Streams {
		s := &output.Streams[i]
		if s.Type == "video" {
			s.Codec = d.VideoCodec
			if !d.VideoCopy {
				s.Width, s.Height = transcode.OutputGeometry(s, d.MaxHeight)
				s.SampleAspectRatio = "1:1" // encodes are square-pixel
				s.Level = d.H264Level
				s.Profile = ""
				s.BitDepth = 0
				if d.VideoCodec == "h264" {
					s.FrameRate = 0
					if d.H264Level > 0 {
						s.Profile, s.BitDepth, s.FrameRate = "High", 8, d.H264FrameRate
					}
				}
				s.BitrateKbps = 0
				stampEncodedVideoRange(s, d)
			} else if d.StripDolbyVision {
				stampEncodedVideoRange(s, d)
			}
			if d.Container == transcode.ContainerFMP4 {
				if tag := fmp4VideoTag(info.Video(), d); tag != "" {
					s.CodecTag = tag
				}
			}
		} else if s.Type == "audio" && s.Index == d.AudioStream {
			s.Codec = d.AudioCodec
			if !d.AudioCopy {
				s.Channels = encodedAudioChannels(d, info)
				s.SampleRate = d.AudioSampleRate
				s.BitrateKbps = 0
				s.Profile = ""
			}
		}
	}
	audioOnly := info.Video() == nil && profileStream(info, "audio", d.AudioStream) != nil
	for _, c := range p.Transcoding {
		candDelivery := strings.ToLower(strings.TrimSpace(c.Delivery))
		if candDelivery == "" && audioOnly {
			candDelivery = "http"
		}
		if !strings.EqualFold(candDelivery, delivery) {
			continue
		}
		if audioOnly {
			if (c.MediaType != "" && !strings.EqualFold(c.MediaType, "audio")) || !profileAudioContainerMatch(c.Containers, d.Container, d.AudioCodec) || !profileContains(c.AudioCodecs, d.AudioCodec) {
				continue
			}
		} else {
			if (c.MediaType != "" && !strings.EqualFold(c.MediaType, "video")) || !profileContains(c.Containers, d.Container) || !profileContains(c.VideoCodecs, d.VideoCodec) || !profileContains(c.AudioCodecs, d.AudioCodec) {
				continue
			}
		}
		if c.MaxAudioChannels != "" && (d.AudioCodec != "" || profileStream(info, "audio", d.AudioStream) != nil) {
			limit, err := strconv.Atoi(c.MaxAudioChannels)
			channels := d.AudioChannels
			if channels == 0 {
				if d.AudioCopy {
					if source := profileStream(info, "audio", d.AudioStream); source != nil {
						channels = source.Channels
					}
				} else {
					channels = encodedAudioChannels(d, info)
				}
			}
			if err != nil || limit <= 0 || channels <= 0 || channels > limit {
				continue
			}
		}
		// Transcoding profile constraints describe the delivered stream.
		// A 2160p input can satisfy a 1080p profile via a video encode.
		condStream := output.Video()
		if audioOnly {
			condStream = profileStream(output, "audio", d.AudioStream)
		}
		if profileConditions(c.Conditions, output, condStream) && profileRestrictions(p, output, d.VideoCodec, d.AudioCodec, d.AudioStream) {
			return true
		}
	}
	return false
}

// fmp4VideoTag is the sample entry fMP4 segments carry for the output video,
// as the transcoder's arguments write it: HEVC is tagged hvc1 (dvh1 for a
// copied Dolby Vision profile 5 source) and H.264 avc1, whatever the source
// container used. A required VideoCodecTag condition, such as AVPlayer's
// hvc1/dvh1, is evaluated against this rather than the source's tag, which
// Matroska does not have.
func fmp4VideoTag(source *core.StreamInfo, d core.Decision) string {
	switch d.VideoCodec {
	case "hevc":
		if d.VideoCopy && source != nil && source.DolbyVision && source.DolbyVisionProfile == 5 {
			return "dvh1"
		}
		return "hvc1"
	case "h264":
		return "avc1"
	}
	return ""
}

// constrainedH264 describes only facts guaranteed by the selected encoder
// contract. Unknown geometry or rate stays unknown, so required predicates
// still fail closed.
func constrainedH264(d core.Decision, info core.MediaInfo, caps core.ClientCaps, encoderVerified bool) (core.Decision, bool) {
	if d.VideoCopy || d.VideoCodec != "h264" {
		return d, true
	}
	ordinary := d.Enhancement.Backend == ""
	if !ordinary {
		switch d.Enhancement.State {
		case "off", "ineligible", "unavailable", "fallback":
			ordinary = true
		}
	}
	if !encoderVerified || !ordinary {
		// Do not assert an output profile or level for an unverified hardware
		// encoder. Required profile predicates fail closed in the caller.
		return d, caps.VideoLevels["h264"] <= 0 && len(caps.VideoProfiles["h264"]) == 0
	}
	if profiles := caps.VideoProfiles["h264"]; len(profiles) > 0 && !profileContains(profiles, "High") {
		return d, false
	}
	v := info.Video()
	if v == nil {
		return d, caps.VideoLevels["h264"] <= 0
	}
	fps := v.FrameRate
	if v.ReferenceFrameRate != nil && *v.ReferenceFrameRate > fps {
		fps = *v.ReferenceFrameRate
	}
	if v.AverageFrameRate != nil && *v.AverageFrameRate > fps {
		fps = *v.AverageFrameRate
	}
	w, h := transcode.OutputGeometry(v, d.MaxHeight)
	if w == 0 || h == 0 || fps <= 0 || d.BitrateKbps <= 0 {
		return d, caps.VideoLevels["h264"] <= 0 && len(caps.VideoProfiles["h264"]) == 0
	}
	if d.BitrateKbps > 200000 {
		return d, false
	}
	// The encoder's bounded profile uses at most four decoded pictures and a
	// peak no greater than 150% of the total decision bitrate.
	level := transcode.H264HighLevel(w, h, fps, d.BitrateKbps*3/2, 4)
	if level == 0 || (caps.VideoLevels["h264"] > 0 && level > caps.VideoLevels["h264"]) {
		return d, false
	}
	d.H264Level = level
	d.H264FrameRate = fps
	return d, true
}

// h264EncoderVerified matches the manager's actual encoder-selection rule.
// QSV, VAAPI and VideoToolbox remain unknown until their SPS is verified.
func (b *Broker) h264EncoderVerified() bool {
	if b.deps.Transcoder == nil {
		return false
	}
	hw := b.deps.Transcoder.HWAccel()
	if hw.Name == "" || hw.Name == transcode.AccelNone {
		return true
	}
	if !slices.Contains(hw.Encoders, "h264_"+hw.Name) {
		return true
	}
	return hw.Name == transcode.AccelNVENC
}

// sideSubtitleStream reports the text subtitle stream selected for the client
// to render beside the video when decideProfile cleared Decision.SubtitleStream
// to -1 so the video encoder does not burn it in.
func sideSubtitleStream(ps *core.PlaySession, req frontend.PlanRequest, dec core.Decision) *int {
	if ps == nil || dec.SubtitleStream >= 0 || req.Intent.Options.SubtitleStream < 0 {
		return nil
	}
	want := req.Intent.Options.SubtitleStream
	if slices.Contains(progressiveTextTracks(string(req.Protocol), req.Caps, ps.Info), want) ||
		(hlsOnlyTextClient(string(req.Protocol), req.Caps) && slices.Contains(hlsTextTracks(string(req.Protocol), req.Caps, ps.Info), want)) {
		return &want
	}
	return nil
}

func (b *Broker) decideProfile(ps *core.PlaySession, req frontend.PlanRequest) (core.Decision, error) {
	if ps == nil {
		return core.Decision{}, core.ErrNotFound
	}
	if req.Channel == "" && b.videoOff(req.Caps) {
		req.Caps.NoVideoTranscode = true
	}
	requiresRemux := ps.RequiresRemux
	if rr, ok := b.deps.Playback.(core.RemuxRequirer); ok && rr.RequiresRemux(ps.ID) {
		requiresRemux = true
	}
	if !requiresRemux && req.Protocol == "vela" && req.Channel == "" && req.Intent.Options.DirectPlay && !selectedImageSubtitle(ps.Info, req.Intent.Options.SubtitleStream) && len(b.originalBudgetRejection(ps.Info, req)) == 0 && (req.Intent.OriginalPlayback.Supports(ps.Info, req.Intent.Options.AudioStream) || (b.AudioFallbackAvailable() && req.Intent.OriginalPlayback.SupportsAudio("aac") && (req.Intent.AllowTranscoding == nil || *req.Intent.AllowTranscoding) && req.Intent.OriginalPlayback.SupportsVideo(ps.Info))) {
		d := core.Decision{Kind: core.DecisionDirectPlay, Reason: "original_runtime", Container: ps.Info.Container, VideoCopy: !ps.Info.AudioOnly(), AudioCopy: true, AudioStream: req.Intent.Options.AudioStream, SubtitleStream: req.Intent.Options.SubtitleStream}
		if audio := profileStream(ps.Info, "audio", d.AudioStream); audio != nil {
			d.AudioStream = audio.Index
		}
		for _, s := range ps.Info.Streams {
			if s.Type == "video" {
				d.VideoCodec = s.Codec
			}
			if s.Type == "audio" && (s.Index == d.AudioStream || d.AudioStream < 0) {
				d.AudioStream = s.Index
				d.AudioCodec = s.Codec
			}
		}
		return d, nil
	}

	rejected := []frontend.RejectedPlaybackPath{{Path: "original", Reasons: b.originalRejection(ps.Info, req)}}

	// The progressive client renders text outside the video. Keep the user's
	// selection in the plan, but never ask the video decision to burn it in.
	if ps != nil {
		tracks := progressiveTextTracks(string(req.Protocol), req.Caps, ps.Info)
		if len(tracks) > 0 {
			// Progressive cues need the playback demuxer. Negotiate its
			// real output now, including the streaming audio constraints.
			req.Intent.Options.DirectPlay = false
		}
		// A native HLS client shows its selected text track as a rendition of
		// the HLS stream. Only HLS can carry it, so the original file is not
		// direct-played, and the video decision stays subtitle-free like the
		// progressive client's below.
		// A client that also renders embedded tracks from the original file
		// (HLSTextRenditions) keeps direct play and its selection.
		for _, index := range hlsTextTracks(string(req.Protocol), req.Caps, ps.Info) {
			if hlsOnlyTextClient(string(req.Protocol), req.Caps) && req.Intent.Options.SubtitleStream == index {
				req.Intent.Options.DirectPlay = false
				req.Intent.Options.SubtitleStream = -1
				req.Intent.Options.BurnSubtitles = false
				break
			}
		}
		for _, index := range tracks {
			if req.Intent.Options.SubtitleStream == index {
				req.Intent.Options.SubtitleStream = -1
				req.Intent.Options.BurnSubtitles = false
				break
			}
		}
	}

	req.Caps = applyProfileRangeCaps(req.Caps)
	reject := func(path, reason string) {
		for _, prior := range rejected {
			if prior.Path == path && slices.Contains(prior.Reasons, reason) {
				return
			}
		}
		rejected = append(rejected, frontend.RejectedPlaybackPath{Path: path, Reasons: []string{reason}})
	}
	videoOff := req.Channel == "" && b.videoOff(req.Caps)
	r := DecisionRequest{Session: ps, Caps: req.Caps, Options: req.Intent.Options, PreferRemux: req.Intent.PreferRemux, PreserveSourceOnFailure: true, NoVideoTranscode: videoOff}
	d, err := Decide(b.deps.Playback, b.deps.Transcoder, r)
	if err == nil && d.Reason == "remux for failover safety: health unverified" && b.deps.Logger != nil {
		b.deps.Logger.Debug(d.Reason, "session", ps.ID)
	}
	// A profile that lists no direct-play and no transcoding formats declares
	// nothing about playback (Findroid's "Direct play all"); the ordinary
	// capability decision stands.
	if err != nil || req.Caps.Profiles == nil || (req.Caps.Profiles.DirectPlay == nil && req.Caps.Profiles.Transcoding == nil) {
		if err == nil && videoOff && decisionEncodesVideo(d, ps.Info) {
			return core.Decision{}, &frontend.NoCompatiblePathError{
				RejectedPaths: []frontend.RejectedPlaybackPath{{Path: "video", Reasons: []string{reasonVideoTranscodingOff}}},
				Cause:         core.ErrVideoTranscodingDisabled,
			}
		}
		return d, err
	}
	if d.Kind == core.DecisionDirectPlay && profileDirectAllowed(req.Caps.Profiles, ps.Info, d) {
		return d, nil
	}
	if d.Kind == core.DecisionDirectPlay {
		b.profileDiagnostic(ProfileDirectRejected)
		reject("direct", "source does not match the declared direct playback profile")
		if b.deps.OnProfileDiagnostic != nil && profileUnknownRequired(req.Caps.Profiles, ps.Info, d) {
			b.profileDiagnostic(ProfileRequiredConditionUnknown)
		}
	}
	rejectedForVideoOff := false
	allowed := func(d core.Decision) bool {
		if videoOff && decisionEncodesVideo(d, ps.Info) {
			rejectedForVideoOff = true
			return false
		}
		return req.Intent.AllowTranscoding == nil || *req.Intent.AllowTranscoding || !decisionEncodes(d, ps.Info)
	}
	if ps != nil && ps.Info.Video() == nil && ps.Info.HasPlayableMedia() {
		if d.Kind != core.DecisionDirectPlay && allowed(d) {
			delivery := "hls"
			if req.Intent.Options.Protocol == core.ProtocolHTTP {
				delivery = "http"
			}
			if profileDeliveryAllowed(req.Caps.Profiles, d, ps.Info, delivery) {
				return d, nil
			}
		}
		if decision, ok := b.decideAudioProfile(ps, req.Caps, req.Intent.Options, req.Intent.AllowTranscoding, ""); ok {
			if decision.Kind == core.DecisionTranscode {
				b.profileDiagnostic(ProfileFallbackTranscode)
			} else {
				b.profileDiagnostic(ProfileFallbackRemux)
			}
			return decision, nil
		}
		b.profileDiagnostic(ProfileNoDelivery)
		return core.Decision{}, &frontend.NoCompatiblePathError{RejectedPaths: rejected}
	}
	if output, ok := constrainedH264(d, ps.Info, req.Caps, b.h264EncoderVerified()); d.Kind != core.DecisionDirectPlay && ok && allowed(output) && (profileDeliveryAllowed(req.Caps.Profiles, output, ps.Info, "hls") || profileDeliveryAllowed(req.Caps.Profiles, output, ps.Info, "dash")) {
		return output, nil
	}
	if d.Kind != core.DecisionDirectPlay {
		b.profileDiagnostic(ProfileDeliveryRejected)
		if videoOff && decisionEncodesVideo(d, ps.Info) {
			rejectedForVideoOff = true
			reject("delivery", reasonVideoTranscodingOff)
		} else {
			reject("delivery", outputRejection(d, ps.Info, req.Intent.AllowTranscoding))
		}
	}
	var encodeFallback *core.Decision
	for _, candidate := range req.Caps.Profiles.Transcoding {
		reject(candidate.Delivery, "no output matches declared containers ["+strings.Join(candidate.Containers, ",")+"] video ["+strings.Join(candidate.VideoCodecs, ",")+"] audio ["+strings.Join(candidate.AudioCodecs, ",")+"] and profile constraints")
		if !strings.EqualFold(candidate.Delivery, "hls") && !strings.EqualFold(candidate.Delivery, "dash") {
			continue
		}
		tryOutput := func(videoCodecs, audioCodecs []string) (core.Decision, bool) {
			caps := req.Caps
			caps.VideoCodecs = videoCodecs
			caps.AudioCodecs = audioCodecs
			caps.HLSContainers = candidate.Containers
			caps.TranscodeVideoCodecs = nil
			caps.TranscodeAudioCodecs = nil
			// MaxAudioChannels constrains this delivery, not direct-file
			// decoding. Feed it into the audio planner so supported surround
			// codecs are downmixed instead of repeatedly proposing copy.
			if candidate.MaxAudioChannels != "" {
				limit, err := strconv.Atoi(candidate.MaxAudioChannels)
				if err != nil || limit <= 0 {
					return core.Decision{}, false
				}
				caps.AudioLimits = append(slices.Clone(caps.AudioLimits), core.AudioLimit{Codec: "*", Name: "audio.channels", Type: "upperBound", Value: limit})
			}
			opts := req.Intent.Options
			opts.DirectPlay = false
			for _, condition := range candidate.Conditions {
				limit, err := strconv.Atoi(condition.Value)
				if err != nil || limit <= 0 || !strings.EqualFold(condition.Operator, "LessThanEqual") {
					continue
				}
				switch strings.ToLower(condition.Property) {
				case "height", "videoheight":
					opts.MaxHeight = stricterLimit(opts.MaxHeight, limit)
				case "width", "videowidth":
					opts.MaxWidth = stricterLimit(opts.MaxWidth, limit)
				}
			}
			// Retry only bounded output heights; never raise a client or request
			// cap. This allows a lower resolution when the required H.264 level
			// or dimensions cannot support the original size.
			tryAttempt := func(attempt core.DecideOptions) (core.Decision, bool, bool) {
				decision, err := Decide(b.deps.Playback, b.deps.Transcoder, DecisionRequest{Session: ps, Caps: caps, Options: attempt, PreferRemux: req.Intent.PreferRemux, PreserveSourceOnFailure: true, NoVideoTranscode: videoOff})
				if err == nil && !allowed(decision) {
					if videoOff && decisionEncodesVideo(decision, ps.Info) {
						reject(candidate.Delivery, reasonVideoTranscodingOff)
					} else {
						reject(candidate.Delivery, "transcode not allowed")
					}
				}
				if err != nil || decision.Kind == core.DecisionDirectPlay || !allowed(decision) {
					return decision, false, false
				}
				decision, ok := constrainedH264(decision, ps.Info, caps, b.h264EncoderVerified())
				if ok && profileDeliveryAllowed(req.Caps.Profiles, decision, ps.Info, candidate.Delivery) {
					if req.Intent.Options.DirectPlay {
						decision.Reason = strings.ReplaceAll(decision.Reason, "direct play not allowed by client", "direct play rejected by declared output profile")
					}
					return decision, true, true
				}
				return decision, false, true
			}
			for _, height := range []int{0, 2160, 1440, 1080, 720, 576, 480, 360, 240, 144} {
				attempt := opts
				if height > 0 && (attempt.MaxHeight == 0 || height < attempt.MaxHeight) {
					attempt.MaxHeight = height
				}
				decision, matched, valid := tryAttempt(attempt)
				if matched {
					return decision, true
				}
				if !videoOff && valid && decision.VideoCopy && attempt.DirectStream {
					attempt.DirectStream = false
					opts.DirectStream = false
					decision, matched, valid = tryAttempt(attempt)
					if matched {
						return decision, true
					}
				}
				if !valid {
					continue
				}
				if decision.VideoCopy || (decision.VideoCodec != "h264" && decision.VideoCodec != "hevc") {
					break
				}
			}
			return core.Decision{}, false
		}
		try := func(videoCodecs []string) (core.Decision, bool) {
			if decision, ok := tryOutput(videoCodecs, candidate.AudioCodecs); ok {
				return decision, true
			}
			// A profile can allow several audio codecs with different
			// decoder constraints. E-AC-3 is the default surround target,
			// but rejecting that output must not discard AC-3 or AAC.
			if len(candidate.AudioCodecs) > 1 {
				for _, codec := range []string{"ac3", "eac3", "aac", "mp3", "opus", "flac"} {
					if profileContains(candidate.AudioCodecs, codec) {
						if decision, ok := tryOutput(videoCodecs, []string{codec}); ok {
							return decision, true
						}
					}
				}
			}
			return core.Decision{}, false
		}
		if decision, ok := try(candidate.VideoCodecs); ok {
			if decision.VideoCopy {
				if decision.Kind == core.DecisionTranscode {
					b.profileDiagnostic(ProfileFallbackTranscode)
				} else {
					b.profileDiagnostic(ProfileFallbackRemux)
				}
				return decision, nil
			}
			if encodeFallback == nil {
				d := decision
				encodeFallback = &d
			}
			continue
		}
		// A combined codec list may repeatedly select source copy even when
		// its required profile predicates reject that source. Try individually
		// declared alternatives, retaining all audio and container constraints.
		// The transcoder currently emits H.264 or HEVC. Trying these fixed
		// outputs bounds work independently of untrusted profile list length.
		fallbackCodecs := []string{"h264", "hevc"}
		if v := ps.Info.Video(); v != nil && encodedPictureHDR(v) && req.Caps.HDR10 {
			fallbackCodecs = []string{"hevc", "h264"}
		}
		for _, codec := range fallbackCodecs {
			if !profileContains(candidate.VideoCodecs, codec) {
				continue
			}
			if len(req.Caps.VideoCodecs) > 0 && !req.Caps.SupportsVideo(codec) {
				continue
			}
			if decision, ok := try([]string{codec}); ok {
				if decision.VideoCopy {
					if decision.Kind == core.DecisionTranscode {
						b.profileDiagnostic(ProfileFallbackTranscode)
					} else {
						b.profileDiagnostic(ProfileFallbackRemux)
					}
					return decision, nil
				}
				if encodeFallback == nil {
					d := decision
					encodeFallback = &d
				}
				break
			}
		}
	}
	if encodeFallback != nil {
		if encodeFallback.Kind == core.DecisionTranscode {
			b.profileDiagnostic(ProfileFallbackTranscode)
		} else {
			b.profileDiagnostic(ProfileFallbackRemux)
		}
		return *encodeFallback, nil
	}
	b.profileDiagnostic(ProfileNoDelivery)
	outErr := &frontend.NoCompatiblePathError{RejectedPaths: rejected}
	if rejectedForVideoOff {
		outErr.Cause = core.ErrVideoTranscodingDisabled
	}
	return core.Decision{}, outErr
}

// decideAudioProfile negotiates an audio-only transcoding/remux decision against
// declared Type:"Audio" (or untyped) TranscodingProfiles. wantDelivery restricts
// matching to "http" or "hls" when non-empty; empty evaluates candidates in
// declaration order across both progressive HTTP and HLS.
func (b *Broker) decideAudioProfile(ps *core.PlaySession, baseCaps core.ClientCaps, baseOpts core.DecideOptions, allowTranscoding *bool, wantDelivery string) (core.Decision, bool) {
	if ps == nil || b.deps.Transcoder == nil || ps.Info.Video() != nil || !ps.Info.HasPlayableMedia() {
		return core.Decision{}, false
	}
	allowed := func(d core.Decision) bool {
		if d.Kind == core.DecisionDirectPlay {
			return false
		}
		if allowTranscoding != nil && !*allowTranscoding && decisionEncodes(d, ps.Info) {
			return false
		}
		if d.Kind == core.DecisionRemux && !baseOpts.DirectStream {
			return false
		}
		return true
	}
	if baseCaps.Profiles == nil || baseCaps.Profiles.Transcoding == nil {
		opts := baseOpts
		opts.DirectPlay = false
		if wantDelivery == "http" {
			opts.Protocol = core.ProtocolHTTP
		} else if wantDelivery == "hls" {
			opts.Protocol = core.ProtocolHLS
		}
		d, err := Decide(b.deps.Playback, b.deps.Transcoder, DecisionRequest{Session: ps, Caps: baseCaps, Options: opts, PreserveSourceOnFailure: true})
		if err != nil || !allowed(d) {
			return core.Decision{}, false
		}
		return d, true
	}
	for _, candidate := range baseCaps.Profiles.Transcoding {
		if candidate.MediaType != "" && !strings.EqualFold(candidate.MediaType, "audio") {
			continue
		}
		delivery := strings.ToLower(strings.TrimSpace(candidate.Delivery))
		if delivery == "" {
			delivery = "http"
		}
		if delivery != "http" && delivery != "hls" {
			continue
		}
		if wantDelivery != "" && !strings.EqualFold(delivery, wantDelivery) {
			continue
		}
		tryCodecs := func(audioCodecs []string) (core.Decision, bool) {
			caps := baseCaps
			caps.AudioCodecs = audioCodecs
			caps.TranscodeAudioCodecs = audioCodecs
			caps.HLSContainers = candidate.Containers
			if candidate.MaxAudioChannels != "" {
				limit, err := strconv.Atoi(candidate.MaxAudioChannels)
				if err != nil || limit <= 0 {
					return core.Decision{}, false
				}
				caps.AudioLimits = append(slices.Clone(caps.AudioLimits), core.AudioLimit{Codec: "*", Name: "audio.channels", Type: "upperBound", Value: limit})
			}
			opts := baseOpts
			opts.DirectPlay = false
			if delivery == "http" {
				opts.Protocol = core.ProtocolHTTP
			} else {
				opts.Protocol = core.ProtocolHLS
			}
			if baseCaps.Profiles.MusicStreamingTranscodingBitrate > 0 {
				kbps := baseCaps.Profiles.MusicStreamingTranscodingBitrate / 1000
				if kbps > 0 && (opts.MaxBitrateKbps == 0 || kbps < opts.MaxBitrateKbps) {
					opts.MaxBitrateKbps = kbps
				}
			}
			d, err := Decide(b.deps.Playback, b.deps.Transcoder, DecisionRequest{Session: ps, Caps: caps, Options: opts, PreserveSourceOnFailure: true})
			if err != nil || !allowed(d) {
				return core.Decision{}, false
			}
			if profileDeliveryAllowed(baseCaps.Profiles, d, ps.Info, delivery) {
				if baseOpts.DirectPlay {
					d.Reason = strings.ReplaceAll(d.Reason, "direct play not allowed by client, ", "")
					d.Reason = strings.ReplaceAll(d.Reason, "direct play not allowed by client", "direct play rejected by declared output profile")
				}
				return d, true
			}
			return core.Decision{}, false
		}
		if d, ok := tryCodecs(candidate.AudioCodecs); ok {
			return d, true
		}
		for _, codec := range candidate.AudioCodecs {
			if d, ok := tryCodecs([]string{codec}); ok {
				return d, true
			}
		}
	}
	return core.Decision{}, false
}

// Original-file decoders cannot apply a server-side image subtitle selection.
// An explicit image track must use the normal burn-in decision instead.
func selectedImageSubtitle(info core.MediaInfo, index int) bool {
	for _, track := range info.Streams {
		if track.Type == "subtitle" && track.Index == index {
			return transcode.IsImageSubtitle(track.Codec)
		}
	}
	return false
}

func (b *Broker) originalBudgetRejection(info core.MediaInfo, req frontend.PlanRequest) []string {
	var reasons []string
	if req.Caps.EffectiveAlwaysEncodeVideo() {
		reasons = append(reasons, "owner policy requires video encoding")
	}
	if v := info.Video(); v != nil {
		if reason := transcode.DirectPlayDolbyVisionRejection(v, req.Caps, req.Intent.Options); reason != "" {
			reasons = append(reasons, reason)
		}
		if req.Intent.Options.MaxHeight > 0 && v.Height > req.Intent.Options.MaxHeight {
			reasons = append(reasons, fmt.Sprintf("%dp exceeds client %dp limit", v.Height, req.Intent.Options.MaxHeight))
		}
		if req.Intent.Options.MaxWidth > 0 && v.Width > req.Intent.Options.MaxWidth {
			reasons = append(reasons, fmt.Sprintf("%dpx width exceeds client %dpx limit", v.Width, req.Intent.Options.MaxWidth))
		}
		if h := req.Caps.VideoMaxHeights[v.Codec]; h > 0 && v.Height > h {
			reasons = append(reasons, fmt.Sprintf("%dp exceeds %dp %s decoder limit", v.Height, h, v.Codec))
		}
		if req.Caps.MaxHeightSource == "App compatibility override" && req.Caps.MaxHeight > 0 && v.Height > req.Caps.MaxHeight {
			reasons = append(reasons, fmt.Sprintf("%dp exceeds %dp app compatibility limit", v.Height, req.Caps.MaxHeight))
		}
	}
	bitrateCap := req.Intent.Options.MaxBitrateKbps
	if req.Caps.Remote {
		if req.Caps.MaxBitrateKbps > 0 && (bitrateCap == 0 || req.Caps.MaxBitrateKbps < bitrateCap) {
			bitrateCap = req.Caps.MaxBitrateKbps
		}
		if b != nil && b.deps.RemoteBitrateLimit != nil {
			if limit := b.deps.RemoteBitrateLimit(); limit > 0 && (bitrateCap == 0 || limit < bitrateCap) {
				bitrateCap = limit
			}
		}
	}
	sourceKbps := info.BitrateKbps
	if sourceKbps <= 0 && info.Size > 0 && info.Duration > 0 {
		sourceKbps = int(float64(info.Size*8) / info.Duration.Seconds() / 1000)
	}
	if bitrateCap > 0 && sourceKbps > bitrateCap {
		reasons = append(reasons, fmt.Sprintf("bitrate %d kbps exceeds %d kbps limit", sourceKbps, bitrateCap))
	}
	return reasons
}

// Diagnostics use only resolved source facts and declarations, never source URLs.
func (b *Broker) originalRejection(info core.MediaInfo, req frontend.PlanRequest) []string {
	var reasons []string
	if !req.Intent.Options.DirectPlay {
		reasons = append(reasons, "original playback not allowed")
	}
	if req.Channel != "" {
		reasons = append(reasons, "live originals not supported")
	}
	if selectedImageSubtitle(info, req.Intent.Options.SubtitleStream) {
		reasons = append(reasons, "selected image subtitle requires conversion")
	}
	reasons = append(reasons, b.originalBudgetRejection(info, req)...)
	c := req.Intent.OriginalPlayback
	if !c.Valid() {
		reasons = append(reasons, "original decoder capabilities not declared")
	} else if !c.Supports(info, req.Intent.Options.AudioStream) {
		reasons = append(reasons, "source container or selected codecs not supported by original decoders")
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "original playback unavailable for this protocol")
	}
	return reasons
}
func outputRejection(d core.Decision, info core.MediaInfo, allow *bool) string {
	if allow != nil && !*allow && decisionEncodes(d, info) {
		return "transcode not allowed"
	}
	return "output video [" + d.VideoCodec + "] audio [" + d.AudioCodec + "] in [" + d.Container + "] does not match declared delivery profile"
}
