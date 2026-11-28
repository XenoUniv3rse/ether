package vela

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) proactiveFallbackAvailable() bool {
	q, ok := i.host.Sessions.(frontend.QualitySessions)
	return ok && q.ProactiveFallbackAvailable()
}

func (i *instance) proactiveFallbackAvailableFor(v *frontend.Viewer) bool {
	if v != nil {
		if q, ok := i.host.Sessions.(interface{ ProactiveFallbackAvailableFor(*frontend.Viewer) bool }); ok {
			return q.ProactiveFallbackAvailableFor(v)
		}
	}
	return i.proactiveFallbackAvailable()
}

func (i *instance) addQualityPaths(ctx context.Context, a frontend.SessionAccess, id string, s *playSession) {
	if _, live := s.plan["live"]; live {
		return
	}
	q, ok := i.host.Sessions.(frontend.QualitySessions)
	if !ok {
		return
	}
	if provider, ok := i.host.Sessions.(interface {
		QualityLimitations(context.Context, frontend.SessionAccess, string) ([]frontend.QualityLimitation, error)
	}); ok {
		if limits, err := provider.QualityLimitations(ctx, a, id); err == nil {
			filtered := make([]frontend.QualityLimitation, 0, len(limits))
			for _, l := range limits {
				if l.Height > 0 {
					filtered = append(filtered, l)
				}
			}
			if len(filtered) > 0 {
				s.plan["qualityLimitations"] = filtered
			}
		}
	}
	rungs, err := q.QualityRungs(ctx, a, id)
	if err != nil || len(rungs) == 0 {
		return
	}
	paths := s.plan["paths"].([]object)
	s.qualityRungs = map[string]string{}
	for _, r := range rungs {
		pathID := "quality-" + r.ID
		s.qualityRungs[pathID] = r.ID
		paths = append(paths, object{"id": pathID, "kind": "transcode", "transport": "hls", "mimeType": "application/vnd.apple.mpegurl", "readiness": "on-demand", "sourceId": paths[0]["sourceId"], "timelineId": paths[0]["timelineId"], "quality": qualityTarget(r), "qualities": []object{}, "boundaries": []object{}})
	}
	s.plan["paths"] = paths
	s.plan["fallback"] = object{"state": "idle"}
}

const adaptivePathID = "adaptive-hls"

func adaptivePath(plan object, id string) object {
	paths := plan["paths"].([]object)
	for _, path := range paths {
		if path["id"] == id {
			return path
		}
	}
	path := object{"id": id, "kind": "remux", "transport": "hls", "mimeType": "application/vnd.apple.mpegurl",
		"readiness": "on-demand", "sourceId": paths[0]["sourceId"], "timelineId": paths[0]["timelineId"], "qualities": []object{}, "boundaries": []object{}}
	plan["paths"] = append(paths, path)
	return path
}

func (i *instance) offerAdaptive(ctx context.Context, a frontend.SessionAccess, s *playSession, direct, hls bool) {
	if !direct || !s.client.Features.AdaptiveHLS || !(s.client.HLS || s.client.MSE) || s.media.Video() == nil || s.channelID != "" {
		return
	}
	path := adaptivePath(s.plan, adaptivePathID)
	if !hls {
		path["readiness"] = "unavailable"
		setAdaptiveUnavailable(path, "hls_unavailable", a.Device.Language)
		return
	}
	url, err := i.host.Sessions.StartStream(ctx, a, s.id, frontend.StreamRequest{
		Format: frontend.StreamHLS, Position: s.position, LazyStart: true, Continuous: true, AdaptiveHLS: true, PrepareOnly: true,
	})
	if err != nil {
		path["readiness"] = "unavailable"
		setAdaptiveUnavailable(path, "hls_unavailable", a.Device.Language)
		return
	}
	path["url"], path["readiness"] = url, "ready"
	path["timelineOriginSeconds"] = s.position.Seconds()
}

func pathBitrate(path object) int {
	cap, _ := path["maxBitrate"].(int)
	return cap
}

func (s *playSession) restartAdaptiveCap(path object) int {
	if s.plan["serverHandoff"] == "completed" && path["id"] == adaptivePathID {
		return 0
	}
	return pathBitrate(path)
}

func (i *instance) prepareFallback(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, s *playSession, id string, maxBitrate int) bool {
	if s.client.Features.AdaptiveHLS && s.channelID == "" && s.media.Video() != nil {
		return i.prepareAdaptiveFallback(w, r, v, s, id, maxBitrate)
	}
	q, ok := i.host.Sessions.(frontend.QualitySessions)
	if !ok || len(s.qualityRungs) == 0 {
		i.unavailable(w, r)
		return false
	}
	available := false
	if !s.port(w, func() { available = i.proactiveFallbackAvailableFor(v) }) {
		return false
	}
	if !available {
		i.unavailable(w, r)
		return false
	}
	chosen, ok := i.selectQualityRung(w, r, v, s, id, maxBitrate)
	if !ok {
		return false
	}
	var err error
	pathID := "quality-" + chosen.ID
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	s.plan["fallback"] = object{"state": "preparing", "pathId": pathID}
	s.publish("plan")
	position := s.position
	var url string
	var origin time.Duration
	if !s.port(w, func() { url, origin, err = q.PrepareQuality(ctx, i.access(r, v), id, chosen.ID, position) }) {
		return false
	}
	if err != nil {
		s.plan["fallback"] = object{"state": "unavailable", "pathId": pathID}
		s.publish("plan")
		fail(w, err, r)
		return false
	}
	for _, p := range s.plan["paths"].([]object) {
		if p["id"] == pathID {
			p["url"] = strings.TrimSuffix(url, "master.m3u8") + chosen.ID + "/index.m3u8"
			p["readiness"] = "ready"
			p["timelineOriginSeconds"] = origin.Seconds()
		}
	}
	s.plan["fallback"] = object{"state": "ready", "pathId": pathID}
	return s.portSnapshot(w, func(copy *playSession) { i.refreshOutput(ctx, id, copy) })
}

func (i *instance) prepareAdaptiveFallback(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, s *playSession, id string, maxBitrate int) bool {
	pos := s.position
	origin := pos.Seconds()
	var url string
	var err error
	selected := selectedPlaybackPath(s.plan, s.plan["paths"].([]object))
	if selected["transport"] == "hls" {
		// A capped view of an existing master does not retire its active
		// encoder or rewrite the selected path while preparing a switch.
		current, _ := selected["url"].(string)
		master, _, _ := strings.Cut(current, "?")
		if !strings.HasSuffix(master, "/master.m3u8") {
			problem(w, 422, "decoder", "quality_unavailable")
			return false
		}
		var fits bool
		if !s.port(w, func() {
			if hs, ok := i.host.Sessions.TranscodeFor(r.Context(), id); ok {
				if adaptive, ok := hs.(frontend.AdaptiveHLSSession); ok {
					fits = len(frontend.AdaptiveRungsAtOrBelow(adaptive.AdaptiveRungs(), maxBitrate)) > 0
				}
			}
		}) {
			return false
		}
		if !fits {
			problem(w, 422, "decoder", "quality_unavailable")
			return false
		}
		origin, _ = selected["timelineOriginSeconds"].(float64)
		pathID := adaptivePathID
		if selected["id"] == adaptivePathID {
			pathID = "adaptive-fallback"
		}
		path := adaptivePath(s.plan, pathID)
		path["url"] = master + "?maxBitrate=" + strconv.Itoa(maxBitrate)
		path["readiness"], path["maxBitrate"], path["timelineOriginSeconds"] = "ready", maxBitrate, origin
		s.plan["fallback"] = object{"state": "ready", "pathId": pathID}
		return s.portSnapshot(w, func(copy *playSession) { i.refreshOutput(r.Context(), id, copy) })
	}
	if !s.port(w, func() {
		url, err = i.host.Sessions.StartStream(r.Context(), i.access(r, v), id, frontend.StreamRequest{
			Format: frontend.StreamHLS, Position: pos, LazyStart: true, Continuous: true,
			AdaptiveHLS: true, AdaptiveMaxBitrate: maxBitrate, PrepareOnly: true,
		})
	}) {
		return false
	}
	if err != nil {
		i.failFor(w, v, err, r)
		return false
	}
	path := adaptivePath(s.plan, adaptivePathID)
	path["url"], path["readiness"], path["maxBitrate"], path["timelineOriginSeconds"] = url, "ready", maxBitrate, origin
	s.plan["fallback"] = object{"state": "ready", "pathId": adaptivePathID}
	return s.portSnapshot(w, func(copy *playSession) { i.refreshOutput(r.Context(), id, copy) })
}

func (i *instance) selectQualityRung(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, s *playSession, id string, maxBitrate int) (frontend.HLSRung, bool) {
	if maxBitrate <= 0 {
		problem(w, 400, "request", "invalid_bitrate")
		return frontend.HLSRung{}, false
	}
	q, ok := i.host.Sessions.(frontend.QualitySessions)
	if !ok || len(s.qualityRungs) == 0 {
		i.unavailable(w, r)
		return frontend.HLSRung{}, false
	}
	var rungs []frontend.HLSRung
	var err error
	if !s.port(w, func() { rungs, err = q.QualityRungs(r.Context(), i.access(r, v), id) }) {
		return frontend.HLSRung{}, false
	}
	if err != nil {
		fail(w, err, r)
		return frontend.HLSRung{}, false
	}
	var chosen frontend.HLSRung
	for _, rung := range rungs {
		if s.qualityRungs["quality-"+rung.ID] == rung.ID && rung.Bandwidth <= maxBitrate && rung.Bandwidth > chosen.Bandwidth {
			chosen = rung
		}
	}
	if chosen.ID == "" {
		problem(w, 422, "decoder", "quality_unavailable")
		return frontend.HLSRung{}, false
	}
	return chosen, true
}

func (s *playSession) rungFor(path object) string {
	id, _ := path["id"].(string)
	return s.qualityRungs[id]
}
func hlsDirectory(url string) string {
	if strings.HasSuffix(url, "master.m3u8") {
		return strings.TrimSuffix(url, "master.m3u8")
	}
	base := strings.TrimSuffix(url, "/index.m3u8")
	if n := strings.LastIndex(base, "/"); n >= 0 {
		return base[:n+1]
	}
	return ""
}

// Restarts retire prepared bytes and codec facts from the previous clock.
func (s *playSession) invalidatePrepared(selected object) {
	for _, path := range s.plan["paths"].([]object) {
		if path["id"] == selected["id"] || path["transport"] != "hls" {
			continue
		}
		resetPreparedPath(path)
	}
	s.plan["fallback"] = object{"state": "idle"}
}
func resetPreparedPath(path object) {
	if path["readiness"] == "unavailable" {
		return
	}
	delete(path, "url")
	delete(path, "maxBitrate")
	delete(path, "initialRenditionId")
	delete(path, "adaptive")
	delete(path, "adaptiveUnavailable")
	if path["id"] == adaptivePathID {
		path["adaptive"] = true
	}
	path["readiness"] = "on-demand"
	path["qualities"] = []object{}
}

func (i *instance) qualityAllowed(ctx context.Context, a frontend.SessionAccess, id, rung string) bool {
	service, ok := i.host.Sessions.(frontend.QualitySessions)
	if !ok {
		return false
	}
	rungs, err := service.QualityRungs(ctx, a, id)
	if err != nil {
		return false
	}
	for _, r := range rungs {
		if r.ID == rung {
			return true
		}
	}
	return false
}

func qualityTarget(r frontend.HLSRung) object {
	q := object{"width": r.Width, "height": r.Height, "bitrate": r.Bandwidth, "videoCodec": "", "codecStatus": "pending"}
	if r.VideoBitrate > 0 {
		q["videoBitrate"] = r.VideoBitrate
	}
	if r.PeakVideoBitrate > 0 {
		q["peakVideoBitrate"] = r.PeakVideoBitrate
	}
	if r.AverageBandwidth > 0 {
		q["averageBitrate"] = r.AverageBandwidth
	}
	if r.Codec != "" {
		q["codec"] = r.Codec
	}
	if r.DynamicRange != "" {
		q["dynamicRange"] = r.DynamicRange
	}
	return q
}
