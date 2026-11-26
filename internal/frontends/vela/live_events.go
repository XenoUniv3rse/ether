package vela

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

const liveStreamHeartbeatSec = 25 * time.Second

func (i *instance) liveEventsAvailable() bool {
	if i.host.LiveEvents == nil {
		return false
	}
	ctx := i.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return i.host.LiveEvents.Available(ctx)
}

// GET /vela/v0/live/stream?topic=&since=&alerts=1
//
// The live stream of plugin screens and surfaces (docs/plugin-screens.md):
// up to four sealed topics from data reads, replaying everything newer than
// since, then records and source frames as they change; alerts=1 adds the
// viewer's alerts; a heartbeat every 25 s re-checks access.
func (i *instance) liveEventsStream(w http.ResponseWriter, r *http.Request) {
	v, token, err := i.viewer(r)
	if err != nil {
		fail(w, err, r)
		return
	}
	flush, ok := w.(http.Flusher)
	if !ok {
		i.unavailable(w, r)
		return
	}
	q := r.URL.Query()
	for name := range q {
		switch name {
		case "topic", "since", "alerts":
		default:
			problem(w, 400, "request", "invalid_request")
			return
		}
	}
	alertsParam := strings.TrimSpace(q.Get("alerts"))
	wantsAlerts := alertsParam == "1" || strings.EqualFold(alertsParam, "true")
	topics := q["topic"]
	if len(topics) > frontend.MaxScreenStreamTopics {
		problem(w, 400, "request", "too_many_topics")
		return
	}
	if len(topics) == 0 && !wantsAlerts {
		problem(w, 400, "request", "invalid_request")
		return
	}
	var since int64
	if raw := q.Get("since"); raw != "" {
		n, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || n < 0 {
			problem(w, 400, "request", "invalid_request")
			return
		}
		since = n
	}
	images := i.newLiveImages(r, token)
	images.viewer = v
	images.channelCards = &streamChannelCards{}
	var screenFrames <-chan frontend.ScreenFrame
	if len(topics) > 0 {
		ps, ok := i.pluginScreens()
		if !ok {
			problem(w, 404, "not-found", "not_found")
			return
		}
		if i.previewExpired(w, r, ps) {
			return
		}
		ss, err := ps.ScreenStream(r.Context(), v, topics, since, images)
		if err != nil {
			screenFail(w, r, err)
			return
		}
		defer ss.Close()
		screenFrames = ss.Frames()
	}
	var alertsC <-chan frontend.LiveEventAlert
	if wantsAlerts && i.host.LiveEvents != nil {
		stream, err := i.host.LiveEvents.Subscribe(r.Context(), v, frontend.LiveEventStreamRequest{Alerts: true})
		if err != nil {
			fail(w, err, r)
			return
		}
		defer stream.Close()
		alertsC = stream.Alerts()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")

	rc := http.NewResponseController(w)
	writeTimeout := i.streamWriteTimeout()
	writeRaw := func(eventName string, raw json.RawMessage) error {
		_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, raw); err != nil {
			return err
		}
		return flushSSE(rc, flush, writeTimeout)
	}
	writeEvent := func(eventName string, payload any) error {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		return writeRaw(eventName, b)
	}
	// An immediate heartbeat confirms the stream to the client.
	if err := writeEvent("heartbeat", object{}); err != nil {
		return
	}

	var heartbeatC <-chan time.Time
	if i.liveHeartbeatCh != nil {
		heartbeatC = i.liveHeartbeatCh
	} else {
		ticker := time.NewTicker(liveStreamHeartbeatSec)
		defer ticker.Stop()
		heartbeatC = ticker.C
	}
	alertSurfaces := map[string]string{}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-i.done():
			return
		case alert, ok := <-alertsC:
			if !ok {
				alertsC = nil
				continue
			}
			if err := writeEvent("alert", i.alertFrame(r, v, alert, alertSurfaces)); err != nil {
				return
			}
		case f, ok := <-screenFrames:
			if !ok {
				// The topics' pages went away for this viewer.
				return
			}
			signed, err := images.signRaw(f.Data, "")
			if err != nil {
				continue
			}
			if err := writeRaw(f.Event, signed); err != nil {
				return
			}
		case <-heartbeatC:
			// Revoked grants must not keep an authenticated stream alive.
			if _, _, err := i.viewer(r); err != nil {
				return
			}
			if err := writeEvent("heartbeat", object{}); err != nil {
				return
			}
		}
	}
}

// alertFrame is the alert as a document client gets it: the toast surface
// to draw it with and the event it is about as a record key.
func (i *instance) alertFrame(r *http.Request, v *frontend.Viewer, alert frontend.LiveEventAlert, surfaces map[string]string) object {
	pluginID := ""
	if p, _, ok := strings.Cut(alert.EventID, ":"); ok {
		pluginID = p
	}
	out := object{"id": alert.ID, "kind": alert.Kind, "title": alert.Title, "body": alert.Body, "at": alert.At}
	if alert.EventID != "" {
		out["ref"] = "event:" + alert.EventID
	}
	if pluginID == "" {
		return out
	}
	out["plugin"] = pluginID
	surface, seen := surfaces[pluginID]
	if !seen {
		if ps, ok := i.pluginScreens(); ok {
			c := screenClient(r)
			if c.Renderer == 0 {
				c.Renderer = 1 << 20
			}
			surface = ps.ToastSurface(r.Context(), v, c, pluginID)
		}
		surfaces[pluginID] = surface
	}
	if surface != "" {
		out["surface"] = surface
	}
	return out
}
