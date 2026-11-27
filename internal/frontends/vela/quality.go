package vela

import (
	"context"
	"net/http"
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

func (i *instance) prepareFallback(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, s *playSession, id string, maxBitrate int) bool {
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
	if len(s.qualityRungs) == 0 {
		return
	}
	for _, path := range s.plan["paths"].([]object) {
		if path["id"] == selected["id"] || path["transport"] != "hls" {
			continue
		}
		delete(path, "url")
		delete(path, "initialRenditionId")
		delete(path, "adaptive")
		path["readiness"] = "on-demand"
		path["qualities"] = []object{}
	}
	s.plan["fallback"] = object{"state": "idle"}
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
