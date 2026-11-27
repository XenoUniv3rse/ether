package playback

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

var _ core.KindPlayback = (*Manager)(nil)

// kindIdentity names the synthetic release a plugin kind item opens under when
// no concrete candidate release is supplied.
func kindIdentity(in core.KindInput) string {
	if in.SourceID != "" {
		return "kind:" + in.SourceID
	}
	if in.Title != nil {
		return "kind:" + in.Title.Key.String()
	}
	return "kind:item"
}

// PrepareKind opens a non-catalogue plugin item through the same session,
// decision and stream-limit machinery as a catalogue title (spec §15.1). The
// item's bytes come from KindInput.OpenCandidate (or KindInput.Open);
// everything after that — probe, direct-play/remux/transcode decision,
// HLS/DASH output, mid-stream failover across plugin candidates, stream limits
// and session accounting — is the shared pipeline.
func (m *Manager) PrepareKind(ctx context.Context, user *core.User, in core.KindInput, caps core.ClientCaps) (*core.PlaySession, error) {
	if user == nil {
		return nil, core.ErrUnauthorized
	}
	if in.Title == nil || (in.Open == nil && in.OpenCandidate == nil) {
		return nil, core.ErrNotAvailable
	}
	prefer := in.PreferSourceID
	if isAutoVersionSelector(prefer) {
		prefer = ""
	}
	exact := in.Exact && prefer != ""
	deferSupersession := in.DeferSupersession
	id := identity{userID: user.ID, clientID: caps.ClientID, key: in.Title.Key, surface: m.playbackSurface(string(caps.Protocol))}
	if deferSupersession {
		id.variant, id.exact = prefer, exact
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("playback: shutting down")
	}
	// Reuse a live session for the same viewer, client, item and selection.
	// If the existing session does not fit the requested version or a newer
	// remembered choice, stop it (for non-deferred callers) and open fresh.
	if s := m.byIdent[id]; s != nil && !s.closed {
		fits := m.sessionFitsSelection(s, caps, prefer, exact)
		if fits && prefer == "" {
			if g := s.current(); g != nil && !m.rememberedChoiceAllows(s, g.releaseID()) {
				fits = false
			}
		}
		if fits {
			s.setNoVideoTranscode(caps.NoVideoTranscode)
			m.mu.Unlock()
			s.touch(m.now())
			return s.snapshot(m.now()), nil
		}
		evictID := s.id
		m.mu.Unlock()
		m.Stop(evictID)
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, errors.New("playback: shutting down")
		}
	}
	fk := flightKey{id: id, prefer: prefer, exact: exact, deferSupersession: deferSupersession}
	f := m.flights[fk]
	if f == nil {
		parent := context.WithoutCancel(ctx)
		wctx, cancel := context.WithTimeout(parent, m.opts.PrepareTimeout)
		kindCopy := in
		kindCopy.PreferSourceID = prefer
		kindCopy.Exact = exact
		kindCopy.DeferSupersession = deferSupersession
		f = &flight{sources: newSourceLifetime(user.ID, string(caps.Protocol), caps.ClientID), done: make(chan struct{}), cancel: cancel, kind: &kindCopy}
		m.flights[fk] = f
		u := *user
		go m.runFlight(wctx, f, fk, &u, caps)
	}
	f.waiters++
	f.playRequest = true
	if f.playAt.IsZero() {
		f.playAt = m.now()
	}
	m.mu.Unlock()

	select {
	case <-f.done:
		if f.err != nil {
			return nil, f.err
		}
		ps, _, err := m.snapshotForAdmission(ctx, f.sess, caps, time.Now(), 0)
		return ps, err
	case <-ctx.Done():
		m.mu.Lock()
		if f.waiters--; f.waiters == 0 && !f.detached {
			m.abandonFlightLocked(fk, f)
		}
		m.mu.Unlock()
		return nil, ctx.Err()
	}
}

// SessionForKind finds the live kind session for a viewer, client and item.
func (m *Manager) SessionForKind(userID int64, clientID string, key core.RatingKey) (*core.PlaySession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// The unscoped adapter owns the default lookup; otherwise require a unique
	// match so a kind key on another surface cannot pick an arbitrary source.
	if s := m.byIdent[identity{userID: userID, clientID: clientID, key: key}]; s != nil && s.isKind() {
		return s.snapshot(m.now()), true
	}
	var match *session
	for _, s := range m.byIdent {
		if s != nil && s.isKind() && s.ident.userID == userID && s.ident.clientID == clientID && s.ident.key == key {
			if match != nil && match != s {
				return nil, false
			}
			match = s
		}
	}
	if match != nil {
		return match.snapshot(m.now()), true
	}
	return nil, false
}

// openKindCandidate opens the next non-excluded candidate of a plugin kind
// session and probes it into a generation.
func (m *Manager) openKindCandidate(ctx context.Context, s *session, exclude []string, fallback func(int64) core.MediaInfo) (*generation, *core.Resolution, error) {
	if !s.isKind() {
		return nil, nil, fmt.Errorf("playback: session %s is not a kind session", s.id)
	}
	var (
		src core.MediaSource
		rel core.Release
		err error
	)
	if s.kindOpenCandidate != nil {
		src, rel, err = s.kindOpenCandidate(ctx, exclude)
	} else {
		defaultID := kindIdentity(core.KindInput{SourceID: s.kindSourceID, Title: s.title})
		if slices.Contains(exclude, defaultID) {
			return nil, nil, core.ErrNotAvailable
		}
		src, err = s.kindOpen(ctx)
		if err == nil && src != nil {
			rel = core.Release{
				ID:       defaultID,
				Name:     displayTitle(s.title),
				Protocol: core.ProtocolHTTP,
				Size:     src.Size(),
				PubDate:  m.now(),
			}
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("playback: open kind source: %w", err)
	}
	if src == nil {
		return nil, nil, errors.New("playback: kind source returned no media")
	}
	if src.Size() <= 0 {
		_ = src.Close()
		return nil, nil, errors.New("playback: kind source is empty")
	}
	if rel.ID == "" {
		if id := src.ReleaseID(); id != "" {
			rel.ID = id
		} else {
			rel.ID = kindIdentity(core.KindInput{SourceID: s.kindSourceID, Title: s.title})
		}
	}
	if slices.Contains(exclude, rel.ID) {
		_ = src.Close()
		return nil, nil, core.ErrNotAvailable
	}
	if rel.Name == "" {
		rel.Name = displayTitle(s.title)
	}
	if rel.Protocol == "" {
		rel.Protocol = core.ProtocolHTTP
	}
	if rel.Size <= 0 {
		rel.Size = src.Size()
	}
	if rel.PubDate.IsZero() {
		rel.PubDate = m.now()
	}
	finish := func(closeErr error) { _ = src.Close() }
	res := &core.Resolution{
		Title:          s.ident.key,
		ResolvedAt:     m.now(),
		HealthVerified: true,
		Release:        rel,
	}
	if m.titleReleaseRejected(ctx, s.ident.key, rel) {
		_ = src.Close()
		return nil, res, core.ErrReleaseRejected
	}
	g, err := m.probeGeneration(ctx, s, res, src, finish, fallback)
	if err != nil {
		return nil, res, err
	}
	return g, res, nil
}

// openKindInitial opens a plugin item's initial candidate and probes it into a
// generation.
func (m *Manager) openKindInitial(ctx context.Context, s *session) (*generation, error) {
	if !s.isKind() {
		return nil, fmt.Errorf("playback: session %s is not a kind session", s.id)
	}
	m.recordTimeline(s, TimelineEvent{
		Kind:    "resolve",
		Level:   "info",
		Message: "Opening plugin stream",
		Detail:  s.kindSourceID,
	})
	fallback := func(size int64) core.MediaInfo {
		info := EstimateInfo(s.title, s.caps, m.selectProfile(s.caps, &s.user, s.ident.key))
		info.Size = size
		return info
	}
	g, res, err := m.openKindCandidate(ctx, s, nil, fallback)
	if err != nil {
		if ctx.Err() == nil {
			if errors.Is(err, core.ErrPreparing) {
				m.recordTimeline(s, TimelineEvent{
					Kind:    "candidate",
					Level:   "info",
					Message: "Plugin stream is still preparing",
					Detail:  s.kindSourceID,
				})
			} else if errors.Is(err, core.ErrJobsBusy) {
				m.recordTimeline(s, TimelineEvent{
					Kind:    "candidate",
					Level:   "info",
					Message: "Plugin preparation jobs are all busy",
					Detail:  core.JobsBusyMessage(err),
				})
			} else {
				m.recordTimeline(s, TimelineEvent{
					Kind:    "candidate",
					Level:   "warn",
					Failing: true,
					Message: "Plugin stream failed to open",
					Detail:  OwnerError(err),
				})
			}
		}
		return nil, err
	}
	if g != nil && res != nil {
		m.recordTimeline(s, TimelineEvent{
			Kind:    "candidate",
			Level:   "info",
			Message: "Opened plugin stream " + displayTitle(s.title),
			Detail:  timelineOpenedDetail(g.infoSnapshot(), res.Release, g.src.Size()),
		})
	}
	return g, nil
}
