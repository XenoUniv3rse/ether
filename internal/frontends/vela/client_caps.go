package vela

import (
	"net/http"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
)

// clientBitrateKbps converts Vela's client.maxBitrate (bits/s, total) to kbps.
// Values <= 0 mean unlimited; small positive values clamp to 1 kbps rather than
// truncating to unlimited.
func clientBitrateKbps(bps int) int {
	if bps <= 0 {
		return 0
	}
	return max(1, bps/1000)
}

// Detail and Play build the same declaration with current network and owner rules.
func (i *instance) clientCapabilities(r *http.Request, v *frontend.Viewer, c client) (frontend.CapabilityResult, error) {
	audioCodecs := strings.Join(c.AudioCodecs, ",")
	maxBitrate := max(c.MaxBitrate, 0)
	maxHeight := max(c.MaxHeight, 0)
	profile := caps.DeviceProfile{
		MaxStreamingBitrate: maxBitrate,
		DirectPlayProfiles: []caps.DirectPlayProfile{
			{Type: "Video", Container: strings.Join(c.Containers, ","), VideoCodec: strings.Join(c.VideoCodecs, ","), AudioCodec: audioCodecs},
		},
	}
	if containers := velaAudioContainers(c.Containers, c.AudioCodecs); len(containers) > 0 {
		// Music and audiobooks: the declared audio codecs imply the
		// audio-only containers this client plays as files.
		profile.DirectPlayProfiles = append(profile.DirectPlayProfiles, caps.DirectPlayProfile{Type: "Audio", Container: strings.Join(containers, ","), AudioCodec: audioCodecs})
	}
	audioTranscodeCodecs := audioCodecs
	if audioTranscodeCodecs == "" {
		audioTranscodeCodecs = "aac,mp3,opus"
	}
	profile.TranscodingProfiles = append(profile.TranscodingProfiles, caps.TranscodingProfile{
		Type:       "Audio",
		Protocol:   "http",
		Container:  strings.Join(velaAudioTranscodeContainers(c.Containers, c.AudioCodecs), ","),
		AudioCodec: audioTranscodeCodecs,
	})
	if c.HLS || c.MSE {
		container := "mp4"
		profile.TranscodingProfiles = append(profile.TranscodingProfiles,
			caps.TranscodingProfile{Type: "Video", Protocol: "hls", Container: container, VideoCodec: strings.Join(c.VideoCodecs, ","), AudioCodec: audioCodecs},
			caps.TranscodingProfile{Type: "Audio", Protocol: "hls", Container: "mp4,ts", AudioCodec: audioTranscodeCodecs},
		)
	}
	for _, format := range c.SubtitleFormats {
		if (c.HLS || c.MSE) && format == "webvtt" {
			profile.SubtitleProfiles = append(profile.SubtitleProfiles, caps.SubtitleProfile{Format: "ether-vtt-window", Method: "External"})
		}
		profile.SubtitleProfiles = append(profile.SubtitleProfiles, caps.SubtitleProfile{Format: format, Method: "External"})
	}
	hdr, dv := false, false
	for _, format := range c.HDR {
		switch format {
		case "hdr10", "hlg":
			hdr = true
		case "dv":
			dv = true
		}
	}
	result, err := i.host.Capabilities.Build(i.identityContext(r), frontend.CapabilityRequest{Viewer: v, Device: caps.DeviceInfo{Protocol: "vela", Product: "Vela", Platform: c.Device.Platform, Version: c.Device.AppVersion, ClientID: c.Device.ID, DeviceName: c.Device.Name, HDR10: hdr, DolbyVision: dv, VideoCodecs: c.VideoCodecs, AudioCodecs: c.AudioCodecs, Containers: c.Containers, SubtitleCodecs: c.SubtitleFormats, MaxHeight: maxHeight, MaxBitrateKbps: clientBitrateKbps(c.MaxBitrate)}, Profile: profile, UserAgent: r.UserAgent()})
	result.Client.AwaitAdmissionIndex = result.Client.AwaitAdmissionIndex || (c.Features.AdaptiveHLS && (c.HLS || c.MSE))
	velaDolbyVisionProfiles(&result.Client, c)
	result.Quality.AllowDolbyVision = result.Quality.AllowDolbyVision && result.Client.DolbyVision
	// Preserve exact declarations, after owner rules have denied any ranges.
	for _, format := range c.HDR {
		if ((format == "hdr10" || format == "hlg") && result.Client.HDR10) || (format == "dv" && result.Client.DolbyVision) {
			result.Client.HDRFormats = append(result.Client.HDRFormats, format)
		}
	}
	if (c.HLS || c.MSE) && !velaSupportsTS(c) {
		result.Client.HLSContainers = []string{"fmp4"}
	}
	return result, err
}

// velaDolbyVisionProfiles fills a missing profile policy with the client's
// declared variants. An empty declaration disables DV even on a known device.
func velaDolbyVisionProfiles(cc *frontend.ClientCaps, c client) {
	if !slices.Contains(c.HDR, "dv") {
		cc.DolbyVision = false
		cc.DolbyVisionBaseOnly = true
		return
	}
	if cc.DVProfilesExplicit && len(cc.DVProfiles) == 0 {
		cc.DolbyVision = false
		return
	}
	if !cc.DolbyVision || cc.DolbyVisionBaseOnly || c.DolbyVisionProfiles == nil {
		return
	}
	if len(*c.DolbyVisionProfiles) == 0 {
		cc.DVProfiles = nil
		cc.DVProfilesExplicit = true
		cc.DVProfilesForced = false
		cc.DolbyVision = false
		return
	}
	if cc.DVProfilesForced || len(cc.DVProfiles) > 0 {
		return
	}
	for _, variant := range *c.DolbyVisionProfiles {
		var profile int
		switch variant {
		case "5":
			profile = 5
		case "7":
			profile = 7
		case "8.1", "8.4":
			profile = 8
		default:
			continue
		}
		if !slices.Contains(cc.DVProfiles, profile) {
			cc.DVProfiles = append(cc.DVProfiles, profile)
		}
		if !slices.Contains(cc.DVProfileVariants, variant) {
			cc.DVProfileVariants = append(cc.DVProfileVariants, variant)
		}
	}
	cc.DVProfilesExplicit = len(cc.DVProfiles) == 0
	cc.DolbyVision = !cc.DVProfilesExplicit
}

// velaSupportsTS reports whether a Vela client can play MPEG-TS HLS segments.
// Clients that explicitly declare TS or run native Apple AVPlayer HLS play TS;
// browsers and MSE clients declaring only MP4/WebM require fMP4 HLS segments.
func velaSupportsTS(c client) bool {
	for _, container := range c.Containers {
		switch strings.ToLower(strings.TrimSpace(container)) {
		case "ts", "mpegts":
			return true
		}
	}
	if c.HLS {
		switch strings.ToLower(strings.TrimSpace(c.Device.Platform)) {
		case "tvos", "ios", "ipados", "macos", "visionos", "appletv":
			return true
		}
		if len(c.Containers) == 0 && !c.MSE {
			return true
		}
	}
	return false
}

// velaAudioContainers lists the audio-only file containers implied by a Vela
// client's declared audio codecs, plus any audio containers it declared.
func velaAudioContainers(containers, audioCodecs []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(names ...string) {
		for _, n := range names {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	for _, c := range containers {
		switch strings.ToLower(strings.TrimSpace(c)) {
		case "flac", "mp3", "aac", "m4a", "m4b", "wav":
			add(strings.ToLower(strings.TrimSpace(c)))
		case "adts":
			add("aac", "adts")
		case "mp4":
			add("m4a", "m4b", "mp4")
		case "ogg", "oga":
			add("ogg", "oga")
		case "opus":
			add("opus", "ogg")
		case "webm", "weba":
			add("webm", "weba")
		}
	}
	for _, a := range audioCodecs {
		low := strings.ToLower(strings.TrimSpace(a))
		switch {
		case low == "flac":
			add("flac")
		case low == "mp3":
			add("mp3")
		case low == "aac":
			add("aac", "m4a", "m4b", "mp4")
		case low == "alac":
			add("m4a", "m4b", "mp4")
		case low == "opus":
			add("ogg", "opus", "webm")
		case low == "vorbis":
			add("ogg", "webm")
		case low == "pcm" || strings.HasPrefix(low, "pcm_"):
			add("wav")
		}
	}
	return out
}

func velaAudioTranscodeContainers(containers, audioCodecs []string) []string {
	supported := velaAudioContainers(containers, audioCodecs)
	if len(supported) == 0 {
		return []string{"mp4", "aac", "mp3", "ogg", "webm"}
	}
	has := make(map[string]bool, len(supported))
	for _, s := range supported {
		has[s] = true
	}
	var out []string
	for _, c := range []string{"mp4", "aac", "mp3", "ogg", "webm"} {
		if has[c] {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return []string{"mp4", "aac", "mp3", "ogg", "webm"}
	}
	return out
}
