package vela

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/language"
	"github.com/braedonsaunders/ether/internal/frontend/kit/quality"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reading"
)

func (i *instance) versionsAvailable() bool {
	return i.host.Editions != nil && i.host.VersionHandles != nil && i.host.Capabilities != nil
}

func (i *instance) negotiatedClientFor(r *http.Request, dev frontend.Device) client {
	if dev.ID != "" {
		k := i.negotiationKey(r, dev.ID)
		i.warmMu.Lock()
		entry, ok := i.warmClients[k]
		i.warmMu.Unlock()
		if ok && time.Since(entry.at) < negotiatedClientTTL {
			return entry.client
		}
	}
	return client{Device: device{ID: dev.ID, Name: dev.DeviceName, Platform: dev.Platform, AppVersion: dev.Version}}
}

func (i *instance) versions(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil {
		fail(w, err, r)
		return
	}
	if !i.versionsAvailable() {
		i.unavailable(w, r)
		return
	}
	k, err := key(r)
	if err != nil {
		problem(w, 404, "not-found", "not_found")
		return
	}
	if k.Kind() == frontend.KindShow || k.Kind() == frontend.KindSeason {
		problem(w, 400, "request", "not_playable")
		return
	}
	dev := requestDevice(r)
	d, err := i.host.Experience.Item(frontend.WithColdDetail(r.Context(), k), frontend.ItemRequest{Viewer: v, Device: dev, Key: k})
	if err != nil {
		fail(w, err, r)
		return
	}
	if d.Title.Type == frontend.TypeShow || d.Title.Type == frontend.TypeSeason {
		problem(w, 400, "request", "not_playable")
		return
	}
	if reading.IsBookTitle(d.Title) {
		problemMessage(w, 400, "request", "not_playable", "This title is a book and opens via reading rather than playback.")
		return
	}
	declared, err := i.clientCapabilities(r, v, i.negotiatedClientFor(r, dev))
	if err != nil {
		fail(w, err, r)
		return
	}
	proj, err := i.titleVersionsProjection(r.Context(), v, dev, d.Title, declared.Client, activeVersion{})
	if err != nil {
		fail(w, err, r)
		return
	}
	out := object{
		"titleId":           ids.RatingKeyGUID(d.Title.Key),
		"selectedVersionId": proj.selectedVersionID,
		"versions":          proj.versions,
	}
	if proj.rememberedChoice != nil {
		out["rememberedChoice"] = proj.rememberedChoice
	}
	if proj.autoSelectionReason != nil {
		out["autoSelectionReason"] = proj.autoSelectionReason
	}
	cache(w, r, out)
}

func (i *instance) clearVersionChoice(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil {
		fail(w, err, r)
		return
	}
	vc, hasChoices := i.host.Editions.(frontend.VersionChoices)
	if !i.versionsAvailable() || !hasChoices {
		i.unavailable(w, r)
		return
	}
	var b struct {
		Scope string `json:"scope"`
	}
	if body(r, &b) != nil {
		problem(w, 400, "request", "invalid_request")
		return
	}
	scope := strings.TrimSpace(b.Scope)
	if scope == "" {
		scope = "all"
	}
	if scope != "all" && scope != "title" && scope != "show" {
		problem(w, 400, "request", "invalid_request")
		return
	}
	k, err := key(r)
	if err != nil {
		problem(w, 404, "not-found", "not_found")
		return
	}
	if k.Kind() == frontend.KindSeason {
		problem(w, 400, "request", "not_playable")
		return
	}
	dev := requestDevice(r)
	d, err := i.host.Experience.Item(frontend.WithColdDetail(r.Context(), k), frontend.ItemRequest{Viewer: v, Device: dev, Key: k})
	if err != nil {
		fail(w, err, r)
		return
	}
	if d.Title.Type == frontend.TypeSeason {
		problem(w, 400, "request", "not_playable")
		return
	}
	cleared, err := vc.ClearViewerChoice(r.Context(), v, &d.Title, scope)
	if err != nil {
		fail(w, err, r)
		return
	}
	out := object{
		"cleared":           cleared,
		"titleId":           ids.RatingKeyGUID(d.Title.Key),
		"selectedVersionId": nil,
		"versions":          []object{},
	}
	if d.Title.Type != frontend.TypeShow && d.Title.Key.Kind() != frontend.KindShow {
		declared, err := i.clientCapabilities(r, v, i.negotiatedClientFor(r, dev))
		if err != nil {
			fail(w, err, r)
			return
		}
		proj, err := i.titleVersionsProjection(r.Context(), v, dev, d.Title, declared.Client, activeVersion{})
		if err != nil {
			fail(w, err, r)
			return
		}
		out["selectedVersionId"] = proj.selectedVersionID
		out["versions"] = proj.versions
		if proj.autoSelectionReason != nil {
			out["autoSelectionReason"] = proj.autoSelectionReason
		}
		if proj.rememberedChoice != nil {
			out["rememberedChoice"] = proj.rememberedChoice
		}
	} else if summary, err := vc.ViewerChoiceFor(r.Context(), v, &d.Title); err == nil && summary != nil {
		out["rememberedChoice"] = object{
			"scope":  summary.Scope,
			"active": false,
			"label":  summary.Label,
		}
	}
	send(w, 200, out)
}

type versionsProjection struct {
	autoSelectionReason object
	selectedVersionID   any
	versions            []object
	rememberedChoice    object
	// playingVersionID and playingLabel name the release an active session
	// reads now (after any failover); unset outside a session.
	playingVersionID any
	playingLabel     string
	// cold is true while any slot is still a placeholder (the title's
	// search had not finished when the list was built).
	cold bool
}

// activeVersion is a session's version state: the handle it was admitted
// with ("" for Auto) and the release it reads now, which moves on a failover.
// Release carries at least its ID; Media is the session's media when known.
type activeVersion struct {
	autoReason frontend.AutoSelectionReason
	handle     string
	release    frontend.Release
	media      frontend.MediaInfo
}

// versionFactFields describe a version's release; an Auto row that is
// playing reports them for the release it plays now.
var versionFactFields = []string{"resolution", "dynamicRange", "videoCodec", "audio", "source", "edition", "sizeBytes", "languages"}

// versionPlanFields are the plan fields attachPlaybackVersions owns.
var versionPlanFields = []string{"selectedVersionId", "versions", "rememberedChoice", "playingVersionId", "playingLabel", "autoSelectionReason"}

// attachPlaybackVersions sets the plan's version menu and reports whether it
// is still cold (placeholder slots), so the session can refresh it.
func (i *instance) attachPlaybackVersions(ctx context.Context, v *frontend.Viewer, dev frontend.Device, item frontend.RatingKey, caps frontend.ClientCaps, active activeVersion, plan object) bool {
	if reason := autoSelectionReason(active.autoReason, dev.Language); reason != nil {
		plan["autoSelectionReason"] = reason
	} else {
		delete(plan, "autoSelectionReason")
	}
	if !i.versionsAvailable() || item == 0 || item.Kind() == frontend.KindShow || item.Kind() == frontend.KindSeason {
		return false
	}
	d, err := i.host.Experience.Item(frontend.WithColdDetail(ctx, item), frontend.ItemRequest{Viewer: v, Device: dev, Key: item})
	if err != nil {
		return false
	}
	proj, err := i.titleVersionsProjection(ctx, v, dev, d.Title, caps, active)
	if err != nil {
		return false
	}
	plan["selectedVersionId"] = proj.selectedVersionID
	plan["versions"] = proj.versions
	if proj.rememberedChoice != nil {
		plan["rememberedChoice"] = proj.rememberedChoice
	} else {
		delete(plan, "rememberedChoice")
	}
	if proj.autoSelectionReason != nil {
		plan["autoSelectionReason"] = proj.autoSelectionReason
	} else {
		delete(plan, "autoSelectionReason")
	}
	if proj.playingLabel != "" {
		plan["playingVersionId"] = proj.playingVersionID
		plan["playingLabel"] = proj.playingLabel
	} else {
		delete(plan, "playingVersionId")
		delete(plan, "playingLabel")
	}
	return proj.cold
}

// applyVersionPlan copies the version fields attachPlaybackVersions set (or
// cleared) in from onto the session plan.
func applyVersionPlan(plan, from object) {
	for _, field := range versionPlanFields {
		if val, exists := from[field]; exists {
			plan[field] = val
		} else if field != "selectedVersionId" && field != "versions" {
			delete(plan, field)
		}
	}
}

func (i *instance) titleVersionsProjection(ctx context.Context, v *frontend.Viewer, dev frontend.Device, title frontend.Title, caps frontend.ClientCaps, active activeVersion) (versionsProjection, error) {
	activeHandle, activeReleaseID := active.handle, active.release.ID
	if dev.ID != "" {
		caps.ClientID = frontend.PlaybackClientID("vela", dev.ID)
	}
	req := frontend.ResolveRequest{Title: &title, Client: caps, User: v}
	var variants []frontend.ReleaseVariant
	var err error
	if slots, ok := i.host.Editions.(frontend.VersionSlots); ok {
		variants, err = slots.VersionSlots(ctx, req, frontend.MaxVariants, true)
	} else if i.host.Editions != nil {
		variants, err = i.host.Editions.Variants(ctx, req, frontend.MaxVariants)
	}
	if err != nil {
		return versionsProjection{}, err
	}
	scope := frontend.VersionScope{Viewer: v, Device: dev, Protocol: "vela", Caps: caps}
	handles := i.issueVersionHandles(ctx, scope, title.Key, variants)

	items := make([]object, 0, len(variants))
	listed := make([]frontend.ReleaseVariant, 0, len(variants))
	var selectedID any
	autoID := ""
	autoIdx := -1
	anyRemembered := false
	cold := false
	for _, variant := range variants {
		selector := variantSelectorID(variant)
		if selector == "" {
			continue
		}
		handle := handles[selector]
		if handle == "" {
			continue
		}
		if variant.Release.ID == "" || strings.HasPrefix(variant.Release.ID, frontend.VersionSelectorPrefix) {
			cold = true
		}
		item := formatVersionItem(handle, variant)
		if variant.Auto && autoID == "" {
			autoID, autoIdx = handle, len(items)
		}
		items = append(items, item)
		listed = append(listed, variant)
		if variant.Remembered {
			anyRemembered = true
		}
		if activeHandle != "" && handle == activeHandle {
			selectedID = handle
		}
	}
	if selectedID == nil && activeHandle == "" && activeReleaseID != "" {
		// Auto playback (no explicit versionId): Auto slot is selected when
		// present; otherwise match the active release directly.
		if autoID != "" {
			selectedID = autoID
		} else {
			for idx, variant := range listed {
				if variant.Release.ID == activeReleaseID {
					selectedID = items[idx]["id"]
					break
				}
			}
		}
	}
	if selectedID == nil && activeHandle != "" && activeReleaseID != "" {
		// The playing slot folded into another that lists the same release.
		for idx, variant := range listed {
			if variant.Release.ID == activeReleaseID {
				selectedID = items[idx]["id"]
				break
			}
		}
	}
	if selectedID == nil && len(items) > 0 {
		if autoID != "" {
			selectedID = autoID
		} else {
			selectedID = items[0]["id"]
		}
	}

	// The release the session reads now: the explicit (non-Auto) row that
	// lists it, else a label from the session's own release and media. Auto
	// never borrows a release's name as if it were that release; while Auto
	// plays it says what it plays now.
	var playingID any
	var playingItem object
	if activeReleaseID != "" {
		for idx, variant := range listed {
			if !variant.Auto && variant.Release.ID == activeReleaseID {
				playingID, playingItem = items[idx]["id"], items[idx]
				break
			}
		}
		if playingItem == nil {
			playing := frontend.ReleaseVariant{Release: active.release, Info: active.media}
			for _, variant := range listed {
				if variant.Release.ID != activeReleaseID {
					continue
				}
				// Listed only behind Auto: its facts, without Auto naming,
				// refined by what the session itself knows.
				playing = variant
				playing.Auto, playing.Label = false, ""
				if active.release.Name != "" {
					playing.Release = active.release
				}
				if len(active.media.Streams) > 0 && !active.media.Estimated {
					playing.Info = active.media
				}
				break
			}
			playingItem = formatVersionItem("", playing)
		}
	}
	playingLabel, _ := playingItem["label"].(string)
	autoReason := active.autoReason
	if autoIdx >= 0 {
		if activeReleaseID == "" {
			autoReason = listed[autoIdx].AutoSelectionReason
		}
		auto := items[autoIdx]
		pickLabel, _ := auto["label"].(string)
		label := "Auto (best available)"
		switch {
		case pickLabel == "Auto":
			label = pickLabel // cold slot: nothing picked yet
		case !pickListed(listed, autoIdx):
			// Catalogue menus give Auto's pick no row of its own; name it
			// as Auto's pick rather than hide it.
			label = "Auto · " + pickLabel
		}
		if selectedID == autoID && playingLabel != "" {
			// Auto is playing: its facts are the release it plays now,
			// not the pick it started from.
			label = "Auto · now " + playingLabel
			for _, field := range versionFactFields {
				if val, ok := playingItem[field]; ok {
					auto[field] = val
				} else {
					delete(auto, field)
				}
			}
		}
		auto["label"] = label
	}

	var rememberedChoice object
	if vc, ok := i.host.Editions.(frontend.VersionChoices); ok {
		if summary, err := vc.ViewerChoiceFor(ctx, v, &title); err == nil && summary != nil {
			rememberedChoice = object{
				"scope":  summary.Scope,
				"active": anyRemembered,
				"label":  summary.Label,
			}
		}
	}
	return versionsProjection{
		autoSelectionReason: autoSelectionReason(autoReason, dev.Language),
		selectedVersionID:   selectedID,
		versions:            items,
		rememberedChoice:    rememberedChoice,
		playingVersionID:    playingID,
		playingLabel:        playingLabel,
		cold:                cold,
	}, nil
}

func variantSelectorID(v frontend.ReleaseVariant) string {
	if v.ID != "" {
		return v.ID
	}
	return v.Release.ID
}

func (i *instance) issueVersionHandles(ctx context.Context, scope frontend.VersionScope, key frontend.RatingKey, variants []frontend.ReleaseVariant) map[string]string {
	if i.host.VersionHandles == nil || len(variants) == 0 {
		return nil
	}
	ids := make([]string, 0, len(variants))
	seen := make(map[string]bool, len(variants))
	for _, v := range variants {
		sel := variantSelectorID(v)
		if sel == "" || seen[sel] {
			continue
		}
		seen[sel] = true
		ids = append(ids, sel)
	}
	if len(ids) == 0 {
		return nil
	}
	if batch, ok := i.host.VersionHandles.(frontend.VersionHandleBatch); ok {
		if out, err := batch.IssueBatch(ctx, scope, key, ids); err == nil && len(out) > 0 {
			return out
		}
	}
	out := make(map[string]string, len(ids))
	for _, sel := range ids {
		if h, err := i.host.VersionHandles.Issue(ctx, scope, key, sel); err == nil && h != "" {
			out[sel] = h
		}
	}
	return out
}

func versionPreparing(w http.ResponseWriter, err error, locale string) {
	_, msg, secs := frontend.PlaybackPending(err, locale)
	frontend.SetPlaybackRetry(w, "version_preparing", secs)
	send(w, 503, object{"error": object{
		"class":   "source",
		"code":    "version_preparing",
		"message": msg,
		"retry":   object{"allowed": true, "afterSeconds": secs, "maxAttempts": 3},
	}})
}

func formatVersionItem(handle string, v frontend.ReleaseVariant) object {
	placeholder := v.Release.ID == "" || strings.HasPrefix(v.Release.ID, frontend.VersionSelectorPrefix)
	probed := !placeholder && !v.Info.Estimated && len(v.Info.Streams) > 0

	res := versionResolution(v, placeholder, probed)
	dr := versionDynamicRange(v, placeholder, probed)
	vcodec := versionVideoCodec(v, placeholder, probed)
	audio := versionAudio(v, placeholder, probed)
	source := versionSource(v, placeholder)
	edition := strings.TrimSpace(v.Edition)
	if edition == "" && !placeholder {
		edition = strings.TrimSpace(v.Release.Info.Edition)
	}
	var sizeBytes int64
	if !placeholder {
		if probed && v.Info.Size > 0 {
			sizeBytes = v.Info.Size
		} else if v.Release.Size > 0 {
			sizeBytes = v.Release.Size
		}
	}
	langs := versionLanguages(v, placeholder, probed)
	avail := v.Availability
	if avail == "" {
		if placeholder {
			avail = frontend.VersionFetch
		} else if probed {
			avail = frontend.VersionCached
		} else {
			avail = frontend.VersionFetch
		}
	}

	label := versionHumanLabel(v, placeholder, res, dr, source, edition, audio, sizeBytes)
	item := object{
		"id":           handle,
		"label":        label,
		"recommended":  v.Auto && !v.Remembered,
		"availability": string(avail),
	}
	if v.Remembered {
		item["remembered"] = true
	}
	if v.Pinned {
		item["pinned"] = true
	}
	if res != "" {
		item["resolution"] = res
	}
	if dr != "" {
		item["dynamicRange"] = dr
	}
	if vcodec != "" {
		item["videoCodec"] = vcodec
	}
	if audio != nil {
		item["audio"] = audio
	}
	if source != "" {
		item["source"] = source
	}
	if edition != "" {
		item["edition"] = edition
	}
	if sizeBytes > 0 {
		item["sizeBytes"] = sizeBytes
	}
	if len(langs) > 0 {
		item["languages"] = langs
	}
	if note := strings.TrimSpace(v.AvailabilityNote); note != "" {
		item["note"] = note
	}
	return item
}

func versionResolution(v frontend.ReleaseVariant, placeholder, probed bool) string {
	if probed {
		if vid := v.Info.Video(); vid != nil && (vid.Width > 0 || vid.Height > 0) {
			return quality.ResolutionLabel(vid.Width, vid.Height)
		}
	}
	if placeholder {
		if v.Auto || v.Release.Info.Height <= 0 {
			return ""
		}
		return quality.ResolutionLabel(0, v.Release.Info.Height)
	}
	if v.Release.Info.Height > 0 {
		return quality.ResolutionLabel(0, v.Release.Info.Height)
	}
	if vid := v.Info.Video(); vid != nil && (vid.Width > 0 || vid.Height > 0) {
		return quality.ResolutionLabel(vid.Width, vid.Height)
	}
	return ""
}

func versionDynamicRange(v frontend.ReleaseVariant, placeholder, probed bool) string {
	if placeholder {
		return ""
	}
	if probed {
		if vid := v.Info.Video(); vid != nil {
			if vid.DolbyVision || vid.DolbyVisionProfile > 0 || (vid.ProbedDolbyVisionProfile != nil && *vid.ProbedDolbyVisionProfile > 0) {
				return "dv"
			}
			transfer := strings.ToLower(strings.TrimSpace(vid.ColorTransfer))
			if transfer == "smpte2084" {
				return "hdr10"
			}
			if transfer == "arib-std-b67" {
				return "hlg"
			}
			return "sdr"
		}
	}
	switch {
	case v.Release.Info.DolbyVision:
		return "dv"
	case v.Release.Info.HDR10:
		return "hdr10"
	default:
		return "sdr"
	}
}

func versionVideoCodec(v frontend.ReleaseVariant, placeholder, probed bool) string {
	if placeholder {
		return ""
	}
	raw := ""
	if probed {
		if vid := v.Info.Video(); vid != nil && vid.Codec != "" {
			raw = vid.Codec
		}
	}
	if raw == "" {
		raw = v.Release.Info.VideoCodec
	}
	return normalizeVideoCodec(raw)
}

func normalizeVideoCodec(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "hevc", "h265", "h.265", "x265":
		return "hevc"
	case "h264", "h.264", "x264", "avc", "avc1":
		return "h264"
	case "av1", "av01":
		return "av1"
	case "vp9":
		return "vp9"
	case "vc1", "vc-1":
		return "vc1"
	case "mpeg2", "mpeg2video":
		return "mpeg2"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func versionAudio(v frontend.ReleaseVariant, placeholder, probed bool) object {
	if placeholder {
		return nil
	}
	if probed {
		var primary *frontend.StreamInfo
		for idx := range v.Info.Streams {
			st := &v.Info.Streams[idx]
			if st.Type != "audio" || st.Codec == "" {
				continue
			}
			if st.Default || primary == nil {
				primary = st
				if st.Default {
					break
				}
			}
		}
		if primary != nil {
			codec := normalizeAudioCodec(primary.Codec, primary.Profile)
			if codec != "" {
				out := object{"codec": codec}
				if ch := formatChannelLayout(primary.ChannelLayout, primary.Channels); ch != "" {
					out["channels"] = ch
				}
				probedAtmos := strings.Contains(strings.ToLower(primary.Title), "atmos") || strings.Contains(strings.ToLower(primary.Profile), "atmos")
				sameFamily := audioCodecMatches(codec, normalizeAudioCodec(v.Release.Info.AudioCodec, ""))
				if probedAtmos || (sameFamily && v.Release.Info.Atmos) {
					out["atmos"] = true
				}
				return out
			}
		}
	}
	codec := normalizeAudioCodec(v.Release.Info.AudioCodec, "")
	if codec == "" {
		return nil
	}
	out := object{"codec": codec}
	if ch := strings.TrimSpace(v.Release.Info.Channels); ch != "" {
		out["channels"] = ch
	}
	if v.Release.Info.Atmos {
		out["atmos"] = true
	}
	return out
}

func normalizeAudioCodec(raw, profile string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	p := strings.ToLower(strings.TrimSpace(profile))
	switch s {
	case "eac3", "ec-3", "ec3", "ddp", "dd+", "dolby digital plus":
		return "eac3"
	case "ac3", "ac-3", "dd", "dolby digital":
		return "ac3"
	case "truehd", "mlp":
		return "truehd"
	case "dts-hd", "dts-hd ma", "dtshd", "dca-hd":
		return "dtshd"
	case "dts", "dca":
		if strings.Contains(p, "hd") || strings.Contains(p, "ma") || strings.Contains(p, "hra") {
			return "dtshd"
		}
		return "dts"
	case "aac":
		return "aac"
	case "flac":
		return "flac"
	case "opus":
		return "opus"
	case "mp3":
		return "mp3"
	case "pcm", "lpcm", "pcm_s16le", "pcm_s24le":
		return "pcm"
	default:
		if strings.HasPrefix(s, "pcm_") {
			return "pcm"
		}
		return s
	}
}

func audioCodecMatches(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	return (a == "dtshd" && b == "dts") || (a == "dts" && b == "dtshd")
}

func formatChannelLayout(layout string, channels int) string {
	s := strings.ToLower(strings.TrimSpace(layout))
	switch {
	case strings.HasPrefix(s, "7.1"):
		return "7.1"
	case strings.HasPrefix(s, "6.1"):
		return "6.1"
	case strings.HasPrefix(s, "5.1"):
		return "5.1"
	case strings.HasPrefix(s, "5.0"):
		return "5.0"
	case strings.HasPrefix(s, "3.0"):
		return "3.0"
	case strings.HasPrefix(s, "2.0") || s == "stereo":
		return "2.0"
	case strings.HasPrefix(s, "1.0") || s == "mono":
		return "1.0"
	}
	switch channels {
	case 8:
		return "7.1"
	case 7:
		return "6.1"
	case 6:
		return "5.1"
	case 5:
		return "5.0"
	case 3:
		return "3.0"
	case 2:
		return "2.0"
	case 1:
		return "1.0"
	default:
		return ""
	}
}

func versionSource(v frontend.ReleaseVariant, placeholder bool) string {
	if placeholder {
		return ""
	}
	if label := frontend.SourceLabel(v.Release.Info.Source); label != "" {
		return label
	}
	if v.Release.Protocol == "local" || strings.HasPrefix(v.Release.ID, "local:") || strings.HasPrefix(v.Release.ID, "lab:") {
		return "Local"
	}
	return ""
}

func versionLanguages(v frontend.ReleaseVariant, placeholder, probed bool) []string {
	if placeholder {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.EqualFold(raw, "und") || strings.EqualFold(raw, "unknown") {
			return
		}
		base := language.Base(raw)
		if base == "" || base == "und" || seen[base] {
			return
		}
		seen[base] = true
		out = append(out, base)
	}
	if probed {
		for _, st := range v.Info.Streams {
			if st.Type == "audio" {
				add(st.Language)
			}
		}
	}
	if len(out) == 0 {
		for _, lang := range v.Release.Info.Languages {
			add(lang)
		}
	}
	return out
}

func versionHumanLabel(v frontend.ReleaseVariant, placeholder bool, res, dr, source, edition string, audio object, sizeBytes int64) string {
	if placeholder {
		if v.Auto {
			return "Auto"
		}
		if clean := strings.TrimSpace(v.Label); clean != "" {
			return clean
		}
		if res != "" {
			return res
		}
		return "Standard"
	}
	var videoParts []string
	if res != "" {
		videoParts = append(videoParts, res)
	}
	switch dr {
	case "dv":
		videoParts = append(videoParts, "Dolby Vision")
	case "hdr10+":
		videoParts = append(videoParts, "HDR10+")
	case "hdr10":
		videoParts = append(videoParts, "HDR10")
	case "hlg":
		videoParts = append(videoParts, "HLG")
	}
	if source != "" {
		videoParts = append(videoParts, source)
	}
	var segments []string
	if len(videoParts) > 0 {
		segments = append(segments, strings.Join(videoParts, " "))
	}
	if edition != "" {
		segments = append(segments, edition)
	}
	if audioPhrase := versionAudioLabel(audio); audioPhrase != "" {
		segments = append(segments, audioPhrase)
	}
	if len(segments) == 0 {
		if clean := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v.Label), "Auto · ")); clean != "" && !strings.EqualFold(clean, "Auto") {
			segments = append(segments, clean)
		}
	}
	if sizeBytes > 0 {
		if sizePhrase := formatVersionSize(sizeBytes); sizePhrase != "" {
			segments = append(segments, sizePhrase)
		}
	}
	if len(segments) == 0 {
		if v.Auto {
			return "Auto"
		}
		return "Standard"
	}
	return strings.Join(segments, " · ")
}

func versionAudioLabel(audio object) string {
	if audio == nil {
		return ""
	}
	codec, _ := audio["codec"].(string)
	display := map[string]string{
		"truehd": "TrueHD",
		"eac3":   "DD+",
		"ac3":    "DD",
		"dtshd":  "DTS-HD",
		"dts":    "DTS",
		"aac":    "AAC",
		"flac":   "FLAC",
		"opus":   "Opus",
		"mp3":    "MP3",
		"pcm":    "PCM",
	}[codec]
	if display == "" && codec != "" {
		display = strings.ToUpper(codec)
	}
	var parts []string
	if display != "" {
		parts = append(parts, display)
	}
	if atmos, _ := audio["atmos"].(bool); atmos {
		parts = append(parts, "Atmos")
	}
	if ch, _ := audio["channels"].(string); ch != "" {
		parts = append(parts, ch)
	}
	return strings.Join(parts, " ")
}

func formatVersionSize(n int64) string {
	const (
		mb = 1 << 20
		gb = 1 << 30
	)
	switch {
	case n >= 100*gb:
		return fmt.Sprintf("%.0f GB", float64(n)/float64(gb))
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.0f MB", float64(n)/float64(mb))
	default:
		return ""
	}
}

// pickListed reports whether the release behind the Auto row at autoIdx is
// also listed as its own explicit row.
func pickListed(listed []frontend.ReleaseVariant, autoIdx int) bool {
	id := listed[autoIdx].Release.ID
	if id == "" || strings.HasPrefix(id, frontend.VersionSelectorPrefix) {
		return false
	}
	for idx, variant := range listed {
		if idx != autoIdx && !variant.Auto && variant.Release.ID == id {
			return true
		}
	}
	return false
}

func planActiveVersion(plan frontend.PlayPlan) activeVersion {
	return activeVersion{autoReason: plan.AutoSelectionReason, handle: plan.VersionHandle, release: frontend.Release{ID: plan.ReleaseID}, media: plan.Media}
}

// pollPlayingRelease reads the release the session's playback reads now and,
// when an in-session failover moved it (articles gone mid-stream), rebuilds
// the version menu so selectedVersionId, playingVersionId and playingLabel
// name the release actually playing. The read is live session state only;
// the menu is rebuilt only on a change. Reports whether the plan changed.
func (i *instance) pollPlayingRelease(ctx context.Context, viewer *frontend.Viewer, device frontend.Device, id string, s *playSession) bool {
	reader, ok := i.host.Sessions.(frontend.PlayingReleaseReader)
	if !ok || !i.versionsAvailable() {
		return false
	}
	s.mu.Lock()
	skip := !s.ended.IsZero() || s.commandActive || s.item == 0 || s.titleID == "" || s.channelID != ""
	before, source, item, caps := s.active, s.source, s.item, s.declaredCaps
	s.mu.Unlock()
	if skip {
		return false
	}
	rel, media, err := reader.PlayingRelease(ctx, frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "vela"}, id)
	if err != nil || rel.ID == "" || rel.ID == before.release.ID {
		return false
	}
	next := activeVersion{handle: before.handle, release: rel, media: media}
	if before.autoReason != "" {
		next.autoReason = frontend.AutoSourceSelection
	}
	versionPlan := object{}
	cold := i.attachPlaybackVersions(ctx, viewer, device, item, caps, next, versionPlan)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ended.IsZero() || s.commandActive || s.source != source || s.active.release.ID != before.release.ID {
		return false // a command or another refresh moved the session meanwhile
	}
	s.active = next
	if _, ok := versionPlan["versions"]; !ok {
		return false
	}
	applyVersionPlan(s.plan, versionPlan)
	s.versionsCold = cold
	s.publish("plan")
	return true
}
