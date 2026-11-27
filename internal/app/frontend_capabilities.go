package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func init() {
	store.Register("frontend_client_families", 1, `CREATE TABLE frontend_client_families (surface TEXT NOT NULL, family TEXT NOT NULL, policy TEXT NOT NULL, last_seen INTEGER NOT NULL, PRIMARY KEY(surface,family));`)
}

type surfaceCapabilities struct {
	policy  core.FrontendHostPolicy
	app     *App
	surface frontend.Protocol
}

func (s surfaceCapabilities) Build(ctx context.Context, req frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
	// Protocol is assigned by the host; a client cannot opt into another rule set.
	req.Device.Protocol = s.surface
	ip, ok := frontend.ClientIP(ctx)
	if !ok || ip.IsUnspecified() || ip.IsMulticast() {
		return frontend.CapabilityResult{}, core.ErrUnauthorized
	}
	req.Device.Remote = !identity.LAN(ip)
	declared, err := (caps.Builder{}).Build(ctx, req.Device, req.Profile, req.ProtocolInfo, req.UserAgent)
	if err != nil {
		return frontend.CapabilityResult{}, err
	}
	profileDeclared := declared
	cfg := s.app.cfg.Read()
	overrides := cfg.Clients.Overrides
	// Frontends with their own capability builder already apply this baseline.
	// Otherwise fill only missing codec lists; an explicit declaration wins.
	if !s.policy.NativePlaybackCapabilities {
		declared = playbackFloor(declared, overrides)
	} else {
		if transcode.PlaybackDefaultsFor(declared.Product, declared.Platform, declared.Device, declared.DeviceName, "", declared.Version, overrides).AlwaysEncodeVideo {
			declared.AlwaysEncodeVideo = true
		}
	}
	policy := transcode.ResolvePolicy(declared, overrides)
	if declared.SubtitleCodecs == nil {
		declared.SubtitleCodecs = append([]string(nil), policy.SubtitleCodecs...)
	}
	if s.surface == "vela" {
		// Preserve an empty policy and a DV-off rule before Vela fills missing
		// profiles from the client declaration.
		declared.DVProfiles = policy.DolbyVisionProfiles
		declared.DVProfilesExplicit = declared.DVProfiles != nil && len(declared.DVProfiles) == 0
		if transcode.PlaybackDefaultsFor(declared.Product, declared.Platform, declared.Device, declared.DeviceName, "", declared.Version, overrides).DolbyVisionOff {
			declared.DolbyVision = false
			declared.DolbyVisionBaseOnly = true
		}
	} else if len(declared.DVProfiles) == 0 {
		declared.DVProfiles = append([]int(nil), policy.DolbyVisionProfiles...)
	}
	if policy.AlwaysEncodeVideo {
		declared.AlwaysEncodeVideo = true
	}
	declared = transcode.ApplyForcedOverrides(declared, "", overrides)
	declared.PreserveEmptyVideoCodecs = s.policy.NativePlaybackCapabilities
	qualityCaps := declared
	declared = resolver.ApplyClientRules(cfg.Quality, declared, req.Viewer)
	declared.ClientIP = ip.String()
	declared.InferBuffering = s.policy.InferBuffering
	declared.NoPlaybackTelemetry = !s.policy.PlaybackTelemetry
	result := frontend.CapabilityResult{Client: declared, Quality: resolver.SelectProfile(cfg.Quality, declared, req.Viewer)}
	if (s.surface == "jellyfin" || s.surface == "silo") && (req.Profile.MaxStreamingBitrate > 0 || len(req.Profile.DirectPlayProfiles) > 0 || len(req.Device.VideoCodecs) > 0) {
		s.app.log.Info("playback capability selection", "product", declared.Product,
			"maxHeight", declared.MaxHeight, "videoCodecs", declared.VideoCodecs,
			"profileMaxHeight", profileDeclared.MaxHeight, "profileHeightDeclared", profileDeclared.ExplicitMaxHeight,
			"profileHDR10", profileDeclared.HDR10, "profileDolbyVision", profileDeclared.DolbyVision,
			"hdr10", declared.HDR10, "dolbyVision", declared.DolbyVision,
			"maxBitrateKbps", declared.MaxBitrateKbps,
			"qualityRule", result.Quality.Name, "qualityMaxHeight", result.Quality.MaxHeight,
			"qualityHDR", result.Quality.AllowHDR,
			"qualityLayers", resolver.ExplainQuality(resolver.QualityQuestion{Quality: cfg.Quality, Caps: qualityCaps, User: req.Viewer}).Layers)
	}
	if req.Viewer != nil && !s.policy.NativeClientFamilies {
		s.app.recordFrontendFamily(s.surface, declared)
	}
	return result, nil
}

func (s surfaceCapabilities) ApplyClientRules(q frontend.HostQualityConfig, caps core.ClientCaps, viewer *core.Viewer, media string) core.ClientCaps {
	return resolver.ApplyClientRulesForMedia(q, caps, viewer, media)
}

func (a *App) recordFrontendFamily(surface frontend.Protocol, caps core.ClientCaps) {
	// Store only the static family policy, not per-device declarations or viewer IDs.
	product := strings.TrimSpace(caps.Product)
	device := caps.Device
	if device == "" {
		device = caps.DeviceName
	}
	if product == "" || len(product) > 128 || len(caps.Platform) > 128 || len(device) > 128 {
		return
	}
	policy := transcode.ClientPolicyFor(core.ClientCaps{Protocol: surface, Product: product, Platform: caps.Platform, Device: device})
	if policy.Family == "generic" || policy.Family == "Plex Web" {
		policy.Family = product
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return
	}
	key := string(surface) + "\x00" + policy.Family
	a.familyMu.Lock()
	defer a.familyMu.Unlock()
	if a.familyClosed {
		return
	}
	if a.seenFamilies[key] == string(data) || a.familyPending[key] {
		return
	}
	if a.familySlots == nil {
		a.familySlots = make(chan struct{}, 8)
		a.seenFamilies = make(map[string]string)
		a.familyPending = make(map[string]bool)
	}
	select {
	case a.familySlots <- struct{}{}:
	default:
		return
	}
	a.familyPending[key] = true
	a.familyWrites.Add(1)
	go func() {
		defer a.familyWrites.Done()
		defer func() { <-a.familySlots }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := a.saveFrontendFamily(ctx, surface, policy.Family, data)
		a.familyMu.Lock()
		delete(a.familyPending, key)
		if err == nil && len(a.seenFamilies) < 4096 {
			a.seenFamilies[key] = string(data)
		}
		a.familyMu.Unlock()
	}()
}

func (a *App) saveFrontendFamily(ctx context.Context, surface frontend.Protocol, family string, data []byte) error {
	// Bound the number of client-provided family names stored per surface.
	var count int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_client_families WHERE surface=?`, string(surface)).Scan(&count); err != nil {
		return err
	}
	if count >= 256 {
		_, err := a.db.ExecContext(ctx, `UPDATE frontend_client_families SET policy=?,last_seen=? WHERE surface=? AND family=?`, string(data), time.Now().Unix(), string(surface), family)
		return err
	}
	_, err := a.db.ExecContext(ctx, `INSERT INTO frontend_client_families(surface,family,policy,last_seen) VALUES(?,?,?,?) ON CONFLICT(surface,family) DO UPDATE SET policy=excluded.policy,last_seen=excluded.last_seen`, string(surface), family, string(data), time.Now().Unix())
	return err
}

// playbackFloor copies a recognized client's registry codecs onto empty
// lists. The generic preset is not a declaration, so an unknown client stays
// unrestricted. HDR flags stay with the profile: the preset does not certify
// a display, except for native-player apps (see the quirk below).
func playbackFloor(declared core.ClientCaps, overrides []core.ClientCapabilityOverride) core.ClientCaps {
	device := declared.Device
	if device == "" {
		device = declared.DeviceName
	}
	defaults := transcode.PlaybackDefaultsFor(declared.Product, declared.Platform, declared.Device, declared.DeviceName, "", declared.Version, overrides)
	if defaults.AlwaysEncodeVideo {
		declared.AlwaysEncodeVideo = true
	}
	if defaults.ID == "" || defaults.ID == "generic" {
		return declared
	}
	if len(declared.VideoCodecs) == 0 {
		declared.VideoCodecs = append([]string(nil), defaults.Video...)
	}
	if len(declared.AudioCodecs) == 0 {
		declared.AudioCodecs = append([]string(nil), defaults.Audio...)
	}
	if len(declared.Containers) == 0 {
		declared.Containers = append([]string(nil), defaults.Containers...)
	}
	if declared.MaxHeight == 0 && !declared.ExplicitMaxHeight && defaults.MaxHeight > 0 {
		declared.MaxHeight = defaults.MaxHeight
		if defaults.HeightLocked {
			declared.MaxHeightSource = "App compatibility override"
		} else {
			declared.MaxHeightSource = fmt.Sprintf("registry preset %q", defaults.ID)
		}
	}
	// An app that plays through its own native player (registry quirk
	// native-player-dynamic-range: Stremio desktop/Android, Nuvio) renders
	// HDR itself and tone-maps for an SDR display, so the record's range
	// flags are the player's, not a display claim. An explicit refusal wins.
	if !declared.ExplicitNoHDR && transcode.HasQuirk(declared.Product, declared.Platform, device, "", declared.Version, "native-player-dynamic-range") {
		declared.HDR10 = declared.HDR10 || defaults.HDR10
		declared.DolbyVision = declared.DolbyVision || (defaults.DolbyVision && !defaults.DolbyVisionOff)
	}
	return declared
}

var _ frontend.Capabilities = surfaceCapabilities{}
