package vela

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func (i *instance) extrasAvailable() bool {
	if i.host.ExtraStreams != nil {
		if i.trailerPlaybackMode() == trailerPlaybackClient {
			client, ok := i.host.ExtraStreams.(interface{ ClientYouTubeAvailable() bool })
			if ok && client.ClientYouTubeAvailable() {
				return true
			}
		}
		return i.host.ExtraStreams.Enabled()
	}
	return i.extraPlaybackAvailable() || i.previewAvailable()
}

func (i *instance) extraPlaybackAvailable() bool {
	p, ok := i.host.Sessions.(frontend.ExtraPlaybacks)
	return ok && i.host.Capabilities != nil && p.ExtraPlaybackAvailable()
}

func (i *instance) extraPlayback(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, token, extraID, idempotency string, c client) {
	if !i.extraPlaybackAvailable() {
		i.unavailable(w, r)
		return
	}
	item, err := ids.ParseRatingKeyGUID(extraID)
	if err != nil || !boundedPreviewClient(c) || idempotency == "" || len(idempotency) > 256 {
		problem(w, 400, "request", "invalid_target")
		return
	}
	if !i.allowPreview(v.ID) {
		w.Header().Set("Retry-After", "1")
		send(w, http.StatusAccepted, frontend.TrailerPreviewResult{State: "preparing", RetryAfterSeconds: 1})
		return
	}
	scope, err := i.grantScope(r.Context(), token, requestDevice(r))
	if err != nil {
		fail(w, err, r)
		return
	}
	profile := caps.DeviceProfile{MaxStreamingBitrate: max(c.MaxBitrate, 0), DirectPlayProfiles: []caps.DirectPlayProfile{{Type: "Video", Container: strings.Join(c.Containers, ","), VideoCodec: strings.Join(c.VideoCodecs, ","), AudioCodec: strings.Join(c.AudioCodecs, ",")}}}
	declared, err := i.host.Capabilities.Build(i.identityContext(r), frontend.CapabilityRequest{Viewer: v, Device: caps.DeviceInfo{Protocol: "vela", Product: "Vela", Platform: c.Device.Platform, Version: c.Device.AppVersion, ClientID: requestDevice(r).ID, DeviceName: c.Device.Name, VideoCodecs: c.VideoCodecs, AudioCodecs: c.AudioCodecs, Containers: c.Containers, MaxHeight: max(c.MaxHeight, 0), MaxBitrateKbps: clientBitrateKbps(c.MaxBitrate)}, Profile: profile, UserAgent: r.UserAgent()})
	if err != nil {
		fail(w, err, r)
		return
	}
	lang := ""
	if len(v.AudioLanguages) > 0 {
		lang = v.AudioLanguages[0]
	}
	result, err := i.host.Sessions.(frontend.ExtraPlaybacks).ExtraPlayback(r.Context(), frontend.TrailerPreviewRequest{Viewer: v, Device: requestDevice(r), Protocol: "vela", Item: item, IdempotencyKey: idempotency, Language: lang, Caps: declared.Client})
	if err != nil {
		fail(w, err, r)
		return
	}
	if result.State == "preparing" {
		w.Header().Set("Retry-After", "1")
		send(w, http.StatusAccepted, result)
		return
	}
	g := result.Grant
	if result.State != "ready" || g == nil {
		problem(w, 422, "source", "trailer_unavailable")
		return
	}
	if g.Purpose != "playback" || g.SessionID == "" || !strings.HasPrefix(g.URL, "/sessions/media/") || !g.ExpiresAt.After(time.Now()) || g.SourceGeneration <= 0 || g.SeekGeneration <= 0 {
		problem(w, 503, "transient", "invalid_extra_grant")
		return
	}
	if r.Context().Err() != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancel()
		_ = i.host.Sessions.Stop(ctx, i.access(r, v), g.SessionID)
		return
	}
	path := object{"id": "extra-file", "kind": "direct", "transport": "file", "mimeType": g.MIMEType, "url": g.URL, "readiness": "ready", "range": true, "qualities": []object{}, "boundaries": []object{}}
	plan := object{"sessionId": g.SessionID, "extraId": extraID, "purpose": "playback", "revision": uint64(1), "sourceGeneration": uint64(g.SourceGeneration), "seekGeneration": uint64(g.SeekGeneration), "state": "ready", "positionSeconds": 0, "paths": []object{path}, "selectedPathId": "extra-file", "audioTracks": []object{}, "subtitleTracks": []object{}, "selectedSubtitleTrackId": nil, "chapters": []object{}, "fallback": object{"state": "unsupported"}}
	i.mu.Lock()
	for id, old := range i.sessions {
		old.mu.Lock()
		_, extra := old.plan["extraId"]
		expired := extra && (!old.previewExpires.After(time.Now()) || !old.ended.IsZero())
		if extra && old.viewer == v.ID {
			old.ended = time.Now()
			old.plan["state"] = "ended"
		}
		old.mu.Unlock()
		if expired {
			delete(i.sessions, id)
		}
	}
	if i.sessions == nil {
		i.sessions = map[string]*playSession{}
	}
	i.sessions[g.SessionID] = &playSession{scope: scope, viewer: v.ID, device: requestDevice(r).ID, plan: plan, source: uint64(g.SourceGeneration), seek: uint64(g.SeekGeneration), revision: 1, cursor: 1, signal: make(chan struct{}), previewExpires: g.ExpiresAt}
	i.mu.Unlock()
	send(w, 200, plan)
}
