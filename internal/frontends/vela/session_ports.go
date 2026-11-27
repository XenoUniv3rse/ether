package vela

import "net/http"

// port releases the UI/diagnostics lock before entering the host. Commands
// serialize separately, so a slow backend never holds up media byte serving.
// The caller holds mu, including on return.
func (s *playSession) port(w http.ResponseWriter, call func()) bool {
	return s.portCall(w, call, false)
}
func (s *playSession) stopPort(w http.ResponseWriter, call func()) bool {
	return s.portCall(w, call, true)
}
func (s *playSession) portCall(w http.ResponseWriter, call func(), stopping bool) bool {
	source, seek, ended := s.source, s.seek, s.ended
	s.mu.Unlock()
	func() {
		defer s.mu.Lock()
		call()
	}()
	if stale(w, s, source, seek) {
		return false
	}
	if !stopping && s.ended != ended {
		problem(w, 409, "conflict", "session_ended")
		return false
	}
	return true
}

func copyPlanValue(v any) any {
	switch v := v.(type) {
	case object:
		out := object{}
		for k, value := range v {
			out[k] = copyPlanValue(value)
		}
		return out
	case []object:
		out := make([]object, len(v))
		for n, value := range v {
			out[n] = copyPlanValue(value).(object)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for n, value := range v {
			out[n] = copyPlanValue(value)
		}
		return out
	default:
		return v
	}
}

// outputSnapshot contains only immutable inputs and a private plan, never a
// copied mutex or the live SSE signal/event ring.
func (s *playSession) outputSnapshot() *playSession {
	var archive *catchUpPlayback
	if s.archive != nil {
		copy := *s.archive
		archive = &copy
	}
	return &playSession{archive: archive, position: s.position, plan: copyPlanValue(s.plan).(object), source: s.source, seek: s.seek,
		qualityRungs: s.qualityRungs, media: s.media, subtitleFormats: s.subtitleFormats,
		canBurn: s.canBurn, preparation: s.preparation, diagnostics: s.diagnostics}
}
func (s *playSession) applyOutput(copy *playSession) {
	// Output refresh changes evidence on existing paths. Keep those maps in
	// place: command code and plan readers may retain a path reference across
	// refreshes, and must see the newly accepted evidence on that same path.
	if paths, ok := copy.plan["paths"].([]object); ok {
		current, _ := s.plan["paths"].([]object)
		samePaths := len(current) == len(paths)
		if samePaths {
			for n := range paths {
				if current[n]["id"] != paths[n]["id"] {
					samePaths = false
					break
				}
			}
		}
		if samePaths {
			for n, path := range paths {
				clear(current[n])
				for key, value := range path {
					current[n][key] = value
				}
			}
		} else {
			s.plan["paths"] = paths
		}
	}
	for _, key := range []string{"audioTracks", "subtitleTracks", "selectedAudioTrackId", "preparation", "live"} {
		if value, ok := copy.plan[key]; ok {
			s.plan[key] = value
		}
	}
	if copy.archive != nil {
		s.archive = copy.archive
		for _, key := range []string{"catchUp", "durationSeconds"} {
			if value, ok := copy.plan[key]; ok {
				s.plan[key] = value
			}
		}
	}
	s.diagnostics = copy.diagnostics
}
func (s *playSession) portSnapshot(w http.ResponseWriter, call func(*playSession)) bool {
	copy := s.outputSnapshot()
	cursor := s.cursor
	if !s.port(w, func() { call(copy) }) {
		return false
	}
	// Background refreshes yield to newer events; a serialized command
	// applies the output facts produced by its own host operation.
	if cursor == s.cursor || s.commandActive {
		s.applyOutput(copy)
	}
	return true
}
