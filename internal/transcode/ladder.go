package transcode

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// The ABR ladder: one session, one shared segment Timeline, several
// rungs. The primary rung ("base", track 0) is the decision the session
// was started with; lower rungs are transcodes planned from the same
// decision. One steady encoder runs; a rendition switch briefly retains
// the old encoder until the requested replacement segment is ready. A request on an
// idle rung restarts the encoder at that segment with the rung's
// decision (unused rungs are therefore stopped, not just paused, which
// is the cheapest idle state). Segment files are kept per rung (the
// primary in the session dir, rungs in rung-<id>/ subdirs) because the
// same segment number has different bytes per rung.
//
// DASH uses the same rungs: representations 0 (primary video) and 1
// (audio) keep their historic numbering, extra video representations
// are numbered after them. The DASH muxer still produces one video +
// one audio stream per run; the run's video chunks are regrouped into
// whichever video representation the client asked for.

// rung is one ladder variant.
type rung struct {
	id        string
	decision  core.Decision
	width     int
	height    int
	avgKbps   int // average total bitrate
	peakKbps  int
	codecs    string // RFC 6381, video,audio
	hevcLevel int    // HEVC level_idc set in the adaptive encoder output
}

// ladderSteps are the candidate rungs below the source (id, height,
// total kbps).
var ladderSteps = []struct {
	id     string
	height int
	kbps   int
}{
	{"1080p8m", 1080, 8000},
	{"720p4m", 720, 4000},
	{"480p1500k", 480, 1500},
	{"360p800k", 360, 800},
}

// rungCodecs builds the codecs string of an encode/copy decision.
func rungCodecs(info core.MediaInfo, d core.Decision, outHeight int) string {
	v := info.Video()
	if v == nil {
		return ""
	}
	codec := v.Codec
	profile, depth := v.Profile, v.BitDepth
	if !d.VideoCopy {
		codec = d.VideoCodec
		if codec == "" {
			codec = "h264"
		}
		profile, depth = "", 8
		if codec == "hevc" && keepsHDR(v) && !d.ToneMap {
			depth = 10
		}
	}
	c := estimateCodecs(codec, profile, outHeight, depth)
	if !d.VideoCopy && codec == "h264" && d.H264Level > 0 {
		c = fmt.Sprintf("avc1.6400%02x", d.H264Level)
	}
	if enhancementSelected(d) {
		// Match the native builder's explicit High 4.1/5.1 contract rather
		// than the ordinary encoder's resolution-only level estimate.
		c = "avc1.640029"
		if d.Enhancement.Width*d.Enhancement.Height > 1920*1088 || v.FrameRate > 30 {
			c = "avc1.640033"
		}
	}
	if a := findStream(info, d.AudioStream, "audio"); a != nil {
		ac := a.Codec
		ap := a.Profile
		if !d.AudioCopy {
			ac = d.AudioCodec
			if ac == "" {
				ac = "aac"
			}
			ap = ""
		}
		if s := estimateCodecs(ac, ap, 0, 0); s != "" {
			c += "," + s
		}
	}
	return c
}

// decisionTotalKbps is a decision's total (video+audio) average bitrate:
// what the stream carries, not the source file (which may hold more audio
// tracks, or audio the decision re-encodes).
func decisionTotalKbps(info core.MediaInfo, d core.Decision) int {
	audio := decisionAudioKbps(info, d)
	v := info.Video()
	if d.VideoCopy {
		video := sourceVideoKbps(info, v, nil)
		if video <= 0 && v != nil {
			video = bitrateCeiling(v.Height, v.Codec)
		}
		return video + audio
	}
	if d.BitrateKbps > 0 {
		return d.BitrateKbps
	}
	return videoBitrate(info, d) + audio
}

// decisionAudioKbps is the bitrate of a decision's audio output.
func decisionAudioKbps(info core.MediaInfo, d core.Decision) int {
	a := findStream(info, d.AudioStream, "audio")
	switch {
	case a == nil:
		return 0
	case d.AudioCopy:
		return copiedAudioKbps(a)
	default:
		kbps := audioBitrateKbps(d.AudioCodec, decisionAudioChannels(d, a))
		if d.AudioBitrateKbps > 0 {
			kbps = min(kbps, d.AudioBitrateKbps)
		}
		return kbps
	}
}

// minRungVideoKbps is the floor videoBitrate gives an encode's video.
const minRungVideoKbps = 200

// buildRungs plans a session's ladder from its primary decision. Rungs at
// or above the primary's output height or average bitrate are omitted, so
// a client bitrate cap (already applied to the primary decision by
// Decide) also caps the ladder.
func buildRungs(info core.MediaInfo, primary core.Decision, adapt adaptiveSettings) []rung {
	v := info.Video()
	pw, ph := 0, 0
	if v != nil {
		if primary.VideoCopy {
			pw, ph = v.Width, v.Height
		} else {
			pw, ph = decisionOutputSize(v, primary)
		}
	}
	avg := decisionTotalKbps(info, primary)
	base := rung{
		id: "base", decision: primary, width: pw, height: ph,
		avgKbps: avg,
		codecs:  rungCodecs(info, primary, ph),
	}
	base.peakKbps = peakKbps(avg)
	if primary.VideoCopy {
		base.peakKbps = max(base.peakKbps, copyStreamPeakKbps(info, avg))
	}
	rungs := []rung{base}
	// Soft encode rungs under a video-copy primary follow abrLadderUnderCopy
	// (default on for upgrade compatibility with abrLadder). Set it false to
	// advertise only the Direct Stream / remux primary.
	if !adapt.Ladder || v == nil || (primary.VideoCopy && !adapt.LadderUnderCopy) {
		return rungs
	}
	audio := decisionAudioKbps(info, primary) // rungs carry the primary's audio
	below := avg
	// A rung is a genuine step down or nothing. Its class (720p, 480p…)
	// must be visibly below the primary's picture: release heights vary
	// (798, 800, 1036, 1040…), and a "720p" re-encode of a 798-line
	// picture is the same quality at a worse bitrate. Players switch to a
	// rung only when throughput cannot carry the variant above it, so a
	// rung close to that variant's bitrate is just a needless re-encode a
	// healthy link drifts onto.
	for _, step := range ladderSteps {
		if ph > 0 && step.height*100 > ph*rungMaxHeightPercent {
			continue
		}
		w, h := fitRungBox(v, step.height)
		// Advertise what the rung really sends: its video never drops
		// below the encoder floor, so copied lossless or surround audio can
		// make a "lower" rung no cheaper than the one above it.
		total := max(step.kbps, minRungVideoKbps+audio)
		if below > 0 && total*100 > below*rungMaxBitratePercent {
			continue
		}
		d := primary
		// Enhancement belongs only to the base. These are ordinary encodes
		// of the source, including when the base upscales beyond the source.
		d.Enhancement = core.VideoEnhancement{}
		d.Kind = core.DecisionTranscode
		d.VideoCopy = false
		d.MaxHeight = h
		d.BitrateKbps = total
		// Keep HDR only when the primary keeps it in HEVC; otherwise the
		// cheap, universally decoded SDR H.264 encode.
		d.VideoCodec = "h264"
		d.ToneMap = primary.ToneMap || encodedRange(v) != rangeSDR
		if keepsHDR(v) && !primary.ToneMap && (primary.VideoCodec == "hevc" || (primary.VideoCopy && v.Codec == "hevc")) {
			d.VideoCodec = "hevc"
			d.ToneMap = false
		}
		d.Reason = primary.Reason + "; ABR rung " + step.id
		if enhancementSelected(primary) {
			d.Reason = "ordinary ABR rung " + step.id + " beneath RTX Video base"
		}
		rungs = append(rungs, rung{
			id: step.id, decision: d, width: w, height: h,
			avgKbps: total, peakKbps: peakKbps(total),
			codecs: rungCodecs(info, d, h),
		})
		below = total
	}
	return rungs
}

// rungMaxHeightPercent bounds a rung's class height against the
// primary's picture height; a taller class is a near-duplicate of it.
const rungMaxHeightPercent = 85

// rungMaxBitratePercent bounds a rung's average bitrate against the
// variant above it, so a rung only ever answers a real throughput drop.
const rungMaxBitratePercent = 67

// copyStreamPeakKbps is a copied primary's peak segment rate as the
// stream carries it: the source file's peak less the bytes the stream
// does not send (audio tracks it drops or re-encodes smaller), which are
// near-constant. Advertising the file's peak would overstate BANDWIDTH and
// push a healthy link onto a lower rung.
func copyStreamPeakKbps(info core.MediaInfo, streamAvgKbps int) int {
	peak := HLSCopyPeakKbps(info, 0)
	if dropped := info.BitrateKbps - streamAvgKbps; dropped > 0 && streamAvgKbps > 0 {
		peak -= dropped
	}
	return peak
}

// fitRungBox fits v's display picture into the 16:9 box of a ladder class
// (1280x720 for 720p), never upscaling, so wide scope pictures shrink by
// width as release naming classes them. The size is what the encoder
// makes of the returned height (displayOutputSize), so the advertised
// RESOLUTION matches the rung's bytes.
func fitRungBox(v *core.StreamInfo, boxH int) (int, int) {
	w, h := displaySize(v)
	if w > 0 && h > 0 && w*9 > h*16 { // wider than 16:9: width bound
		boxH = even(h * boxH * 16 / (w * 9))
	}
	return outputSize(w, h, boxH)
}

func peakKbps(avg int) int {
	if avg <= 0 {
		return 0
	}
	p := avg * 12 / 10
	if p < avg+100 {
		p = avg + 100
	}
	return p
}

// rungTrack is the session track of a rung ID (-1 unknown). For DASH the
// track layout differs (see dashTrackForRung).
func (s *Session) rungTrack(id string) int {
	for i, r := range s.rungs {
		if r.id == id {
			if s.isDASH() {
				return dashTrackForRung(i)
			}
			return i
		}
	}
	return -1
}

// dashTrackForRung maps a rung index to its DASH session track: rung 0 is
// track 0 (video), track 1 is audio, further rungs follow.
func dashTrackForRung(rungIdx int) int {
	if rungIdx <= 0 {
		return 0
	}
	return rungIdx + 1
}

// rungForDashTrack is the inverse of dashTrackForRung for video tracks.
func rungForDashTrack(track int) int {
	if track <= 0 {
		return 0
	}
	return track - 1
}

// audioTrackLocked is the DASH audio track index (-1 when there is none
// or the session is HLS).
func (s *Session) audioTrackLocked() int {
	if !s.isDASH() || s.dash == nil {
		return -1
	}
	for i, r := range s.dash.reps {
		if r.Kind == "audio" {
			return i
		}
	}
	return -1
}

// Rungs implements core.HLSLadderSession.
func (s *Session) Rungs() []core.HLSRung {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rungsLocked()
}

func (s *Session) rungsLocked() []core.HLSRung {
	fr := 0.0
	if v := s.info.Video(); v != nil {
		fr = v.FrameRate
	}

	out := make([]core.HLSRung, 0, len(s.rungs))
	for t, r := range s.rungs {
		peak := s.rungPeakKbpsLocked(t)
		video, videoPeak, codec, dr := qualityMetadata(s.info, r)

		frameRate := fr
		if s.adaptiveHLS && r.decision.H264FrameRate > 0 {
			frameRate = r.decision.H264FrameRate
		}
		out = append(out, core.HLSRung{
			VideoBitrate: video, PeakVideoBitrate: videoPeak, Codec: codec, DynamicRange: dr,
			ID: r.id, Bandwidth: peak * 1000, AverageBandwidth: r.avgKbps * 1000,
			Width: r.width, Height: r.height, Codecs: r.codecs, OutputCodecs: s.outputCodecs[t], OutputAudioChannels: s.outputAudioChannels[t], FrameRate: frameRate,
		})
	}
	return out
}

// AdaptiveRungs returns only variants that share a switchable clock and
// decoder family. The explicit quality menu may include other choices.
func (s *Session) AdaptiveRungs() []core.HLSRung {
	s.mu.Lock()
	defer s.mu.Unlock()
	rungs, _ := s.adaptiveRungsLocked()
	return rungs
}

// AdaptiveHLSUnavailable identifies the gate that prevented a switching set.
func (s *Session) AdaptiveHLSUnavailable() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, reason := s.adaptiveRungsLocked()
	return reason
}

func (s *Session) adaptiveRungsLocked() ([]core.HLSRung, string) {
	if !s.adaptiveHLS {
		return nil, "adaptive_not_started"
	}
	out := make([]core.HLSRung, 0, len(s.rungs))
	if s.closed || len(s.rungs) == 0 {
		return out, "adaptive_not_started"
	}
	rungs := s.rungsLocked()
	base := rungs[0]
	v := s.info.Video()
	reason := s.adaptiveUnavailable
	if reason == "" {
		switch {
		case s.container != ContainerFMP4 && s.container != ContainerMPEGTS:
			reason = "unsupported_container"
		case v == nil || v.VariableFrameRate || base.FrameRate <= 0 || s.plan == nil || s.plan.Total() <= 0 || s.rtxTimeline || s.copyFallback || s.sourceLimitKbps > 0:
			reason = "unsupported_timeline"
		case s.rungs[0].decision.VideoCopy && !s.adaptiveCopyAligned:
			reason = "copy_alignment_unverified"
		}
	}
	videoOff := s.videoTranscodingOff()
	capped := false
	for t, r := range rungs {
		d := s.rungs[t].decision
		if r.Bandwidth <= 0 || (videoOff && d.EncodesVideo(s.info)) {
			continue
		}
		if t > 0 {
			b := s.rungs[0].decision
			if r.Bandwidth >= base.Bandwidth || r.Codec != base.Codec || r.DynamicRange != base.DynamicRange ||
				r.FrameRate != base.FrameRate || d.Container != b.Container || d.AudioCodec != b.AudioCodec ||
				d.AudioStream != b.AudioStream || d.AudioCopy != b.AudioCopy || d.AudioChannels != b.AudioChannels ||
				d.AudioSampleRate != b.AudioSampleRate || d.AudioBitrateKbps != b.AudioBitrateKbps ||
				!slices.Equal(d.AudioFilters, b.AudioFilters) || d.SubtitleStream != b.SubtitleStream || d.BurnSubtitles != b.BurnSubtitles {
				continue
			}
		}
		if !s.rungWithinAdaptiveCapLocked(t) {
			capped = true
			continue
		}
		if d.VideoCopy && v != nil && v.CodecString != "" && !d.StripDolbyVision {
			_, audio, hasAudio := strings.Cut(r.Codecs, ",")
			r.Codecs = v.CodecString
			if hasAudio {
				r.Codecs += "," + audio
			}
		}
		if r.OutputCodecs != "" {
			r.Codecs = r.OutputCodecs
		}
		out = append(out, r)
		if reason != "" {
			break
		}
	}
	switch {
	case len(out) > 1:
		return out, ""
	case capped && (len(out) == 0 || reason == ""):
		return out, "bitrate_limit"
	case reason != "":
		return out, reason
	default:
		return out, "no_compatible_rungs"
	}
}

func (s *Session) rungWithinAdaptiveCapLocked(t int) bool {
	return !s.adaptiveHLS || s.adaptiveMaxKbps <= 0 || s.rungPeakKbpsLocked(t) <= s.adaptiveMaxKbps
}

// adaptiveCopyIndex requires a known opening cut and an index reaching the
// tail. A short probe of a long title is evidence only for the ordinary
// remux path. A sparse or inaccurate index cannot define an ABR clock.
func adaptiveCopyIndex(info core.MediaInfo, base, next, segment time.Duration) bool {
	if next <= base || next-base > 3*segment || info.Duration <= next {
		return false
	}
	keys := info.Keyframes
	i := sort.Search(len(keys), func(i int) bool { return keys[i].Time >= next-time.Millisecond })
	if i >= len(keys) || keys[i].Time > next+time.Millisecond {
		return false
	}
	previous := keys[i].Time
	for _, k := range keys[i+1:] {
		if k.Time <= previous || k.Time-previous > 3*segment {
			return false
		}
		previous = k.Time
	}
	return previous < info.Duration && info.Duration-previous <= 3*segment
}

// For adaptive HEVC outputs, choose a Main-tier level that fits the coded
// picture, sample rate and video peak. The encoder applies the same level to
// its parameter sets. Limits are from ITU-T H.265 Annex A.
func adaptiveHEVCLevel(r rung, fps float64) int {
	levels := [...]struct {
		id, picture, kbps int
		samples           float64
	}{
		{60, 122880, 1500, 3686400},
		{63, 245760, 3000, 7372800},
		{90, 552960, 6000, 16588800},
		{93, 983040, 10000, 33177600},
		{120, 2228224, 12000, 66846720},
		{123, 2228224, 20000, 133693440},
		{150, 8912896, 25000, 267386880},
		{153, 8912896, 40000, 534773760},
		{156, 8912896, 60000, 1069547520},
		{180, 35651584, 60000, 1069547520},
		{183, 35651584, 120000, 2139095040},
		{186, 35651584, 240000, 4278190080},
	}
	picture := r.width * r.height
	side := max(r.width, r.height)
	for _, l := range levels {
		if picture <= l.picture && side*side <= 8*l.picture && float64(picture)*fps <= l.samples && r.peakKbps <= l.kbps {
			return l.id
		}
	}
	return 0
}

func setAdaptiveHEVCLevel(r *rung, v *core.StreamInfo) {
	if r.decision.VideoCopy || r.decision.VideoCodec != "hevc" || v.FrameRate <= 0 {
		return
	}
	level := adaptiveHEVCLevel(*r, v.FrameRate)
	if level == 0 {
		return
	}
	r.hevcLevel = level
	_, audio, hasAudio := strings.Cut(r.codecs, ",")
	var video string
	if keepsHDR(v) && !r.decision.ToneMap {
		video = fmt.Sprintf("hvc1.2.4.L%d.90", level)
	} else {
		video = fmt.Sprintf("hvc1.1.6.L%d.90", level)
	}
	r.codecs = video
	if hasAudio {
		r.codecs += "," + audio
	}
}

// hlsPeakWindowLocked is the shortest RFC8216 peak measurement window.
func (s *Session) hlsPeakWindowLocked() time.Duration {
	target := s.segDur
	if s.plan != nil {
		target = s.plan.TargetDuration()
	}
	if target <= 0 {
		target = DefaultSegmentDuration
	}
	return ((target + time.Second - 1) / time.Second * time.Second) / 2
}

func (s *Session) rungPeakKbpsLocked(t int) int {
	r := s.rungs[t]
	peak := r.peakKbps
	if !s.isDASH() && t == 0 && s.plan != nil && s.plan.rampSegs > 0 && s.rampTranscodesLocked() {
		peak = max(peak, encodedHLSPeakKbps(s.info, fastStartDecision(r.decision, s.info, s.adapt), rateControl{}, s.hlsPeakWindowLocked()))
	}
	return peak
}

// ServeRungPlaylist implements core.HLSLadderSession.
func (s *Session) ServeRungPlaylist(w http.ResponseWriter, r *http.Request, rungID string) {
	if s.isDASH() {
		s.ServeManifest(w, r)
		return
	}
	s.mu.Lock()
	t := s.rungTrack(rungID)
	if t < 0 || s.closed || !s.rungWithinAdaptiveCapLocked(t) {
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	s.mu.Unlock()
	if err := s.awaitLiveTarget(r.Context(), t); err != nil {
		http.Error(w, "live playlist unavailable", http.StatusServiceUnavailable)
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	body := s.renderPlaylistLocked(t)
	s.notePlaylistLocked(t, body)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// ServeRungSegment implements core.HLSLadderSession.
func (s *Session) ServeRungSegment(w http.ResponseWriter, r *http.Request, rungID string, n int) {
	if !s.acceptMediaGeneration(w, r) {
		return
	}
	s.mu.Lock()
	t := s.rungTrack(rungID)
	s.mu.Unlock()
	if t < 0 {
		http.NotFound(w, r)
		return
	}
	if n == InitSegment {
		s.serveInit(w, r, t)
		return
	}
	s.serveSegment(w, r, t, n)
}

// dashRepsLadder plans a DASH session's representations: primary video
// (track 0), audio (track 1), then one video representation per extra
// rung, all $Number$-aligned on the session's segment duration.
func dashRepsLadder(info core.MediaInfo, primary core.Decision, rungs []rung) []dashRep {
	reps := dashReps(info, primary)
	if enhancementSelected(primary) && len(rungs) > 0 {
		for i := range reps {
			if reps[i].Kind == "video" {
				reps[i].Codecs = strings.SplitN(rungs[0].codecs, ",", 2)[0]
			}
		}
	}
	for i := 1; i < len(rungs); i++ {
		sub := dashReps(info, rungs[i].decision)
		for _, r := range sub {
			if r.Kind != "video" {
				continue
			}
			r.ID = dashTrackForRung(i)
			reps = append(reps, r)
		}
	}
	return reps
}

var _ core.HLSLadderSession = (*Session)(nil)
var _ core.GoodputSession = (*Session)(nil)

// PrepareRung requests the existing ladder's segment; it creates no parallel encoder.
func (s *Session) PrepareRung(ctx context.Context, id string, position time.Duration) error {
	s.mu.Lock()
	t := s.rungTrack(id)
	n := 0
	if s.plan != nil {
		n = s.plan.IndexAt(position - s.base)
	}
	videoOff := s.videoTranscodingOff() && t >= 0 && t < len(s.rungs) && s.rungs[t].decision.EncodesVideo(s.info)
	s.mu.Unlock()
	if t < 0 {
		return core.ErrNotAvailable
	}
	if videoOff {
		return ErrVideoTranscodingDisabled
	}
	w := &preparationSink{header: make(http.Header)}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/prepare", nil)
	if err != nil {
		return err
	}
	// A one-byte GET activates the lazy rung and waits for real publication.
	r.Header.Set("Range", "bytes=0-0")
	s.serveSegment(w, r, t, n)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if w.status >= 400 || !w.wrote {
		return core.ErrNotAvailable
	}
	return nil
}

type preparationSink struct {
	header http.Header
	status int
	wrote  bool
}

func (w *preparationSink) Header() http.Header         { return w.header }
func (w *preparationSink) WriteHeader(n int)           { w.status = n; w.wrote = true }
func (w *preparationSink) Write(p []byte) (int, error) { w.wrote = true; return io.Discard.Write(p) }

func (m *Manager) ProactiveFallbackAvailable() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.closed && !m.cfg.VideoTranscodingOff()
}
