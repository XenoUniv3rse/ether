package plex

import "github.com/braedonsaunders/ether/internal/frontend"

// SelectAutomaticSubtitle applies the same identity-based selection used by
// PUT /library/parts. The caller fences source publication and manual intent;
// this opt-in rule choice must not persist a manual preference.
func (s *Server) SelectAutomaticSubtitle(access frontend.SessionAccess, key frontend.RatingKey, track frontend.StreamInfo) error {
	if s.Closed() || access.Viewer == nil || !track.External || track.Type != "subtitle" {
		return frontend.ErrNotAvailable
	}
	s.selections.update(access.Viewer.ID, int64(key), func(sel *selection) {
		sel.subtitle, sel.subtitleSet, sel.subtitleStream = -1, true, &track
	})
	s.playIDs.pickSubtitle(playIdentityKey{userID: access.Viewer.ID, clientID: access.Device.ID, key: key}, &track, s.now())
	return nil
}

// subtitleSelectionEcho recognizes only a positive track this client was shown
// selected. Absence of a track is never evidence that an Off request is an echo.
func (s *Server) subtitleSelectionEcho(pr *Request, key frontend.RatingKey, track *frontend.StreamInfo) bool {
	if track == nil || s.playIDs == nil {
		return false
	}
	shown, _ := s.playIDs.shownSubtitles(s.playIdentityFor(pr, key), s.now())
	for _, selected := range shown {
		if sameShownSubtitle(selected, *track) {
			return true
		}
	}
	return false
}

func sameShownSubtitle(a, b frontend.StreamInfo) bool {
	return a.Index == b.Index && a.External == b.External && a.ExternalURL == b.ExternalURL &&
		a.Language == b.Language && a.Codec == b.Codec && a.Forced == b.Forced && a.Title == b.Title
}
