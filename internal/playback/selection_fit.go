package playback

import "github.com/braedonsaunders/ether/internal/core"

// sessionFitsSelection reports whether s satisfies the requested variant and
// current quality-rule resolution ceiling and explicit client bitrate ceiling.
// A session already opened under the same or a
// tighter explicit bitrate cap is kept even when no smaller source existed.
func (m *Manager) sessionFitsSelection(s *session, caps core.ClientCaps, prefer string, exact bool) bool {
	if s == nil || s.closed || s.exact != exact {
		return false
	}
	g := s.current()
	if g == nil {
		return false
	}
	if caps.QualityRuleMaxHeight > 0 && g.res != nil {
		g.mu.Lock()
		exceeds := exceedsRuleHeight(caps.QualityRuleMaxHeight, g.res.Release, g.info)
		g.mu.Unlock()
		if exceeds {
			return false
		}
	}
	if m.sessionNeedsVideoTranscode(s, g, caps) {
		return false
	}
	if prefer != "" {
		if g.releaseID() == prefer || (!exact && g.res.MatchesVersion(prefer)) {
			return true
		}
		// A live session already opened for this configured version slot
		// (including a lower-tier step-down when no copy at that tier worked)
		// satisfies repeat requests for the same slot as long as bitrate caps fit.
		if !exact && core.IsExplicitVersionSlotSelector(prefer) && s.prefer == prefer {
			// Fall through to the bitrate cap check below.
		} else {
			return false
		}
	}
	if !caps.DefaultRemoteBitrate && caps.MaxBitrateKbps > 0 &&
		(s.caps.DefaultRemoteBitrate || s.caps.MaxBitrateKbps <= 0 || caps.MaxBitrateKbps < s.caps.MaxBitrateKbps) {
		info := g.infoSnapshot()
		if info.BitrateKbps > 0 {
			if info.BitrateKbps > caps.MaxBitrateKbps {
				return false
			}
		} else {
			dur := info.Duration
			if dur <= 0 && s.title != nil {
				dur = s.title.Runtime
			}
			if !sourceFitsBitrate(g.res.Release.Size, dur, 0, caps.MaxBitrateKbps) {
				return false
			}
		}
	}
	return true
}

// Use measured dimensions when available and the release tier otherwise.
func exceedsRuleHeight(limit int, release core.Release, info core.MediaInfo) bool {
	if limit <= 0 {
		return false
	}
	if video := info.Video(); !info.Estimated && video != nil && video.Height > 0 {
		return warmResolutionHeight(video.Width, video.Height) > limit
	}
	return release.Info.Height > limit
}

// promotePreparedLocked moves a deferred preparation onto id and returns any
// sessions it supersedes on this client surface. Caller holds m.mu.
func (m *Manager) promotePreparedLocked(id, variantID identity, prep *session, replaceOtherTitles bool) []string {
	if prep == nil || prep.closed || m.sessions[prep.id] != prep {
		return nil
	}
	if variantID != id && m.byIdent[variantID] == prep {
		delete(m.byIdent, variantID)
	}
	var replaced []string
	if old := m.byIdent[id]; old != nil && old != prep {
		replaced = append(replaced, old.id)
	}
	// A session's ident never changes once created: it is read without
	// locks across playback. The promoted session answers for id through
	// byIdent, and detach removes that alias with it.
	m.byIdent[id] = prep
	prep.alias, prep.hasAlias = id, true
	prep.mu.Lock()
	prep.preparedOnly = false
	prep.mu.Unlock()
	if !replaceOtherTitles {
		return replaced
	}
	for _, o := range m.sessions {
		if o != prep && o.ident.userID == id.userID && o.ident.clientID == id.clientID && o.ident.surface == id.surface && o.ident.key != id.key {
			replaced = append(replaced, o.id)
		}
	}
	return replaced
}

func (m *Manager) stopReplaced(replaced []string) {
	seen := make(map[string]struct{}, len(replaced))
	for _, old := range replaced {
		if old == "" {
			continue
		}
		if _, dup := seen[old]; dup {
			continue
		}
		seen[old] = struct{}{}
		m.Stop(old)
	}
}
