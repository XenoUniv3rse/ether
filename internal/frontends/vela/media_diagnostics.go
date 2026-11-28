package vela

import (
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

var playlistURI = regexp.MustCompile(`URI="([^"]*)"`)

// sanitizePlaylist preserves packaging/timeline tags, but drops every URL's
// authority, directory and query. Grants and provider names never enter traces.
func sanitizePlaylist(body string) string {
	lines := strings.Split(body, "\n")
	clean := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "resource"
		}
		return path.Base(u.Path)
	}
	for n, line := range lines {
		if strings.HasPrefix(line, "#") {
			lines[n] = playlistURI.ReplaceAllStringFunc(line, func(value string) string {
				return `URI="` + clean(strings.TrimSuffix(strings.TrimPrefix(value, `URI="`), `"`)) + `"`
			})
		} else if line != "" {
			lines[n] = clean(line)
		}
	}
	return strings.Join(lines, "\n")
}

func (i *instance) serveObservedMedia(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/file") {
		measurement := new(frontend.MediaCapWait)
		r = r.WithContext(frontend.WithMediaCapWait(r.Context(), measurement))
		w = &capWaitWriter{ResponseWriter: w, measurement: measurement, head: r.Method == http.MethodHead}
	}
	tail := strings.TrimPrefix(r.URL.Path, "/sessions/media/")
	id, _, _ := strings.Cut(tail, "/")
	i.mu.Lock()
	s := i.sessions[id]
	i.mu.Unlock()
	if s == nil {
		i.host.Sessions.ServeMedia(w, r)
		return
	}
	done := s.beginMediaRead()
	defer done()
	// Diagnostics are best effort. Media must never queue behind plan work.
	if !s.mu.TryLock() {
		i.host.Sessions.ServeMedia(&mediaActivityWriter{ResponseWriter: w, session: s}, r)
		return
	}
	source, seek := s.source, s.seek
	meterDirect := s.client.Features.AdaptiveHLS && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/file")
	s.mu.Unlock()
	var observed http.ResponseWriter = w
	if meterDirect {
		meter := &directDeliveryWriter{ResponseWriter: w, session: s, source: source, seek: seek}
		defer meter.sample()
		observed = meter
	}
	out := &mediaObservationWriter{ResponseWriter: observed, session: s}
	i.host.Sessions.ServeMedia(out, r)
	record := object{"at": time.Now().UTC(), "resource": path.Base(r.URL.Path), "mimeType": w.Header().Get("Content-Type"), "status": out.status, "bytes": out.bytes, "sourceGeneration": source, "seekGeneration": seek}
	if len(out.playlist) > 0 {
		record["playlist"] = sanitizePlaylist(string(out.playlist))
	}
	if !out.firstByte.IsZero() {
		record["firstByteAt"] = out.firstByte
	}
	if !s.mu.TryLock() {
		return
	}
	defer s.mu.Unlock()
	if source != s.source || seek != s.seek {
		return
	}
	s.observedRecords = append(s.observedRecords, record)
	if len(s.observedRecords) > 32 {
		s.observedRecords = s.observedRecords[len(s.observedRecords)-32:]
	}
}

type mediaActivityWriter struct {
	http.ResponseWriter
	session *playSession
}

func (w *mediaActivityWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *mediaActivityWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }
func (w *mediaActivityWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if n > 0 && w.session != nil {
		w.session.noteActivity()
	}
	return n, err
}

type mediaObservationWriter struct {
	http.ResponseWriter
	session   *playSession
	status    int
	bytes     int64
	firstByte time.Time
	playlist  []byte
}

func (w *mediaObservationWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *mediaObservationWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }
func (w *mediaObservationWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *mediaObservationWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	n, err := w.ResponseWriter.Write(data)
	if n > 0 {
		if w.firstByte.IsZero() {
			w.firstByte = time.Now().UTC()
		}
		if w.session != nil {
			w.session.noteActivity()
		}
	}
	w.bytes += int64(n)
	if strings.Contains(w.Header().Get("Content-Type"), "mpegurl") && len(w.playlist) < 8192 {
		remaining := 8192 - len(w.playlist)
		w.playlist = append(w.playlist, data[:min(n, remaining)]...)
	}
	return n, err
}

func (s *playSession) retainDiagnostics() {
	s.diagnosticHistory = append(s.diagnosticHistory, object{"sourceGeneration": s.source, "seekGeneration": s.seek, "encoder": s.diagnostics, "media": s.observedRecords})
	if len(s.diagnosticHistory) > 4 {
		s.diagnosticHistory = s.diagnosticHistory[len(s.diagnosticHistory)-4:]
	}
	s.diagnostics = nil
	s.observedRecords = nil
}
