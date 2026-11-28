package vela

import (
	"net/http"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

const (
	directBufferingWindow = 15 * time.Second
	directDeliveryGap     = 5 * time.Second
)

type directDeliveryWindow struct {
	source, seek              uint64
	buffering, previousBuffer time.Time
	observed, attempted       time.Time
	first, last               time.Time
	bytes                     int64
	samples                   int
	clientBps                 float64
	clientAt                  time.Time
	failed                    bool
}

// Called only for accepted observations, with the plan locked.
func (s *playSession) observeDirectDelivery(now time.Time, state string, pos time.Duration, throughput float64) {
	d := &s.directDelivery
	if !s.client.Features.AdaptiveHLS || (state != "buffering" && state != "playing") {
		*d = directDeliveryWindow{}
		return
	}
	elapsed := now.Sub(d.observed)
	if d.source != s.source || d.seek != s.seek || elapsed > 2*directBufferingWindow ||
		pos < s.position-2*time.Second || pos > s.position+elapsed+2*time.Second {
		*d = directDeliveryWindow{source: s.source, seek: s.seek}
	}
	d.observed = now
	if state == "buffering" && d.buffering.IsZero() {
		d.buffering = now
	} else if state == "playing" && !d.buffering.IsZero() {
		d.previousBuffer, d.buffering = d.buffering, time.Time{}
	}
	if throughput > 0 {
		d.clientBps, d.clientAt = throughput, now
	}
}

func (d *directDeliveryWindow) add(start, end time.Time, bytes int64) {
	since := d.buffering
	if end.Sub(d.previousBuffer) <= 2*directBufferingWindow {
		since = d.previousBuffer
	}
	if since.IsZero() || !end.After(since) || end.Before(d.last) {
		return
	}
	if d.first.IsZero() || start.Sub(d.last) > directDeliveryGap || start.Sub(d.first) >= 2*directBufferingWindow {
		d.first, d.bytes, d.samples = start, 0, 0
		if d.first.Before(since) {
			d.first = since
		}
	}
	d.bytes += bytes
	d.samples++
	d.last = end
}

func (d *directDeliveryWindow) starved(now time.Time, mediaBps float64) bool {
	if d.buffering.IsZero() || (now.Sub(d.buffering) < directBufferingWindow && now.Sub(d.previousBuffer) > 2*directBufferingWindow) {
		return false
	}
	var rate float64
	switch {
	case d.clientBps > 0 && now.Sub(d.clientAt) <= directBufferingWindow:
		rate = d.clientBps
	case d.samples >= 3 && d.last.Sub(d.first) >= directBufferingWindow && now.Sub(d.last) <= directDeliveryGap:
		rate = float64(d.bytes) * 8 / d.last.Sub(d.first).Seconds()
	}
	return rate > 0 && rate < 0.9*mediaBps
}

// handoffPath is available while an opted-in original can move to an encoded
// rendition. An on-demand path retains its offer after its previous output retires.
func (s *playSession) handoffPath() object {
	paths, _ := s.plan["paths"].([]object)
	selected := selectedPlaybackPath(s.plan, paths)
	if !s.client.Features.AdaptiveHLS || selected["kind"] != "direct" || selected["transport"] != "file" {
		return nil
	}
	for _, path := range paths {
		if path["id"] == adaptivePathID && path["readiness"] != "unavailable" &&
			(path["adaptive"] == true || (path["readiness"] == "ready" && path["kind"] == "transcode")) {
			return path
		}
	}
	return nil
}

func (s *playSession) updateServerHandoff() {
	switch {
	case s.plan["serverHandoff"] == "completed":
	case s.handoffPath() == nil:
		delete(s.plan, "serverHandoff")
	case s.directDelivery.failed && s.directDelivery.source == s.source && s.directDelivery.seek == s.seek:
		s.plan["serverHandoff"] = "failed"
	default:
		s.plan["serverHandoff"] = "pending"
	}
}

// Adapt via the same generation-advancing handoff as an explicit switch.
func (i *instance) adaptDirectPlayback(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, s *playSession) (adapted, current bool) {
	d := &s.directDelivery
	now := time.Now()
	if d.buffering.IsZero() || now.Sub(d.attempted) < 2*directBufferingWindow || s.commandActive {
		return false, true
	}
	path := s.handoffPath()
	if path == nil {
		return false, true
	}
	mediaBps := float64(s.media.BitrateKbps) * 1000
	if mediaBps <= 0 && s.media.Size > 0 && s.media.Duration > 0 {
		mediaBps = float64(s.media.Size) * 8 / s.media.Duration.Seconds()
	}
	if !d.starved(now, mediaBps) || !s.commandMu.TryLock() {
		return false, true
	}
	defer s.commandMu.Unlock()
	s.commandActive = true
	defer func() { s.commandActive = false }()
	d.attempted = now
	ok, err := i.switchPlaybackPath(w, r, v, s, path, s.position, 0, true)
	if err != nil {
		d.failed = true
		return false, true
	}
	return ok, ok
}

// Meter only successful file bodies. Time between batches includes both
// blocked writes and gaps between a client's sequential range requests.
type directDeliveryWriter struct {
	http.ResponseWriter
	session      *playSession
	source, seek uint64
	start, last  time.Time
	bytes        int64
	media        bool
}

func (w *directDeliveryWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *directDeliveryWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func (w *directDeliveryWriter) WriteHeader(status int) {
	length, _ := strconv.ParseInt(w.Header().Get("Content-Length"), 10, 64)
	w.media = (status == http.StatusOK || status == http.StatusPartialContent) && length >= 64<<10
	w.ResponseWriter.WriteHeader(status)
}

func (w *directDeliveryWriter) Write(p []byte) (int, error) {
	start := time.Now()
	n, err := w.ResponseWriter.Write(p)
	if w.media && n > 0 {
		if w.start.IsZero() {
			w.start = start
		}
		w.last = time.Now()
		w.bytes += int64(n)
		if w.last.Sub(w.start) >= time.Second {
			w.sample()
		}
	}
	return n, err
}

func (w *directDeliveryWriter) sample() {
	if w.bytes == 0 {
		return
	}
	s := w.session
	if !s.mu.TryLock() {
		return
	}
	if s.source == w.source && s.seek == w.seek {
		s.directDelivery.add(w.start, w.last, w.bytes)
	}
	s.mu.Unlock()
	w.start, w.bytes = time.Time{}, 0
}
