package stats

import (
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// One press of Play is one history row, as on a real PMS. Clients do not
// open media once: Apple TV, Plex HTPC and Android TV send HEAD and range
// probes, a decision and a start, and reopen after a seek, a track change
// or a stalled read. Each of those may prepare its own playback session.
// A viewing coalesces the sessions of one viewer, client and title that
// open within viewingWindow of its last activity into a single row, and a
// row is written only once the viewing has delivered media or failed.
const (
	// viewingWindow is the play-session window: a session that opens this
	// soon after the viewing's last activity continues it.
	viewingWindow = 5 * time.Minute
	// viewingMinBytes is delivered media that proves a viewing rather than a
	// probe. Container probes read headers, cues and a few range bytes; a
	// second of any real stream is larger.
	viewingMinBytes = 1 << 20
	// maxViewings bounds the in-memory viewings. They are this node's live
	// and recently ended plays; the oldest ended ones go first.
	maxViewings = 4096
	// maxEndedParts bounds the ended session ids a viewing remembers so a
	// sample racing a stop cannot reopen it.
	maxEndedParts = 64
)

type viewingKey struct {
	user   int64
	client string
	key    core.RatingKey
}

type viewingPart struct {
	bytes         int64
	failovers     int
	downFailovers int
	// Mid-play buffering of the session (its counters only grow).
	bufferEvents int
	buffering    time.Duration
}

func (p viewingPart) add(o viewingPart) viewingPart {
	p.bytes += o.bytes
	p.failovers += o.failovers
	p.downFailovers += o.downFailovers
	p.bufferEvents += o.bufferEvents
	p.buffering += o.buffering
	return p
}

// viewingStart is the viewing's start experience across its sessions: the
// first play request, the first picture, and why a start failed.
type viewingStart struct {
	requested   bool
	requestedAt time.Time
	picturedAt  time.Time
	failure     string
}

func (s *viewingStart) observe(e *core.PlayExperience) {
	if e == nil {
		return
	}
	s.requested = s.requested || e.Requested
	if !e.RequestedAt.IsZero() && (s.requestedAt.IsZero() || e.RequestedAt.Before(s.requestedAt)) {
		s.requestedAt = e.RequestedAt
	}
	if e.Pictured && !e.PicturedAt.IsZero() && (s.picturedAt.IsZero() || e.PicturedAt.Before(s.picturedAt)) {
		s.picturedAt = e.PicturedAt
	}
	if e.StartFailure != "" {
		s.failure = e.StartFailure
	}
}

// experience is the row's experience: start time from the first request
// to the first picture, a failed start only if no session ever pictured,
// and buffering summed over the sessions.
func (s viewingStart) experience(t viewingPart) *core.PlayExperience {
	if !s.requested {
		return nil
	}
	e := &core.PlayExperience{Requested: true, StartTime: -1, RequestedAt: s.requestedAt, PicturedAt: s.picturedAt, BufferEvents: t.bufferEvents, Buffering: t.buffering}
	if !s.picturedAt.IsZero() {
		e.Pictured = true
		if !s.requestedAt.IsZero() {
			e.StartTime = max(0, s.picturedAt.Sub(s.requestedAt))
		}
	} else {
		e.StartFailure = s.failure
	}
	return e
}

type viewing struct {
	row     string // stats_plays.id: the first session's id
	k       viewingKey
	started time.Time
	last    time.Time
	live    map[string]viewingPart
	// ended folds sessions that stopped; endedIDs keeps their ids (bounded)
	// so a late sample does not reopen the viewing as a new session.
	ended    viewingPart
	endedIDs []string
	watched  bool // a timeline said playing
	start    viewingStart
	problem  bool
	written  bool
}

func (v *viewing) totals() viewingPart {
	t := v.ended
	for _, p := range v.live {
		t = t.add(p)
	}
	return t
}

func (v *viewing) endedPart(id string) bool {
	for _, e := range v.endedIDs {
		if e == id {
			return true
		}
	}
	return false
}

// viewings maps playback sessions onto rows. The caller holds Service.vmu.
type viewings struct {
	byKey     map[viewingKey]*viewing
	bySession map[string]*viewing
}

func newViewings() *viewings {
	return &viewings{byKey: map[viewingKey]*viewing{}, bySession: map[string]*viewing{}}
}

// observe folds one session observation into its viewing. It returns the
// row-level record to write, or ok=false when nothing should be written
// yet (a probe, a prepared source nobody read) or the observation is
// stale (a sample of a session that already stopped).
func (vs *viewings) observe(p core.PlaySession, now time.Time, ended bool, problem string) (playRecord, bool) {
	begin := p.StartedAt
	if begin.IsZero() {
		begin = now
	}
	v := vs.bySession[p.ID]
	if v == nil {
		k := viewingKey{user: p.UserID, client: p.ClientID, key: p.Key}
		if k.client == "" {
			k.client = p.Client.ClientID
		}
		if cur := vs.byKey[k]; cur != nil {
			if cur.endedPart(p.ID) {
				return playRecord{}, false
			}
			if !begin.Before(cur.started.Add(-viewingWindow)) && !begin.After(cur.last.Add(viewingWindow)) {
				v = cur
			}
		}
		if v == nil {
			if cur := vs.byKey[k]; cur != nil {
				vs.forget(cur)
			}
			vs.evict()
			v = &viewing{row: p.ID, k: k, started: begin, last: now, live: map[string]viewingPart{}}
			vs.byKey[k] = v
		}
		// A stop can be the first word of a session that started and ended
		// between samples; it is folded in without being tracked as live.
		if !ended {
			vs.bySession[p.ID] = v
		}
	}
	if begin.Before(v.started) {
		v.started = begin
	}
	if now.After(v.last) {
		v.last = now
	}
	part := v.live[p.ID]
	part.bytes = max(part.bytes, p.BytesServed)
	part.failovers = max(part.failovers, p.Failovers)
	part.downFailovers = max(part.downFailovers, p.DownFailovers)
	if e := p.Experience; e != nil {
		part.bufferEvents = max(part.bufferEvents, e.BufferEvents)
		part.buffering = max(part.buffering, e.Buffering)
	}
	v.start.observe(p.Experience)
	if ended {
		delete(v.live, p.ID)
		delete(vs.bySession, p.ID)
		v.ended = v.ended.add(part)
		v.endedIDs = append(v.endedIDs, p.ID)
		if len(v.endedIDs) > maxEndedParts {
			v.endedIDs = v.endedIDs[len(v.endedIDs)-maxEndedParts:]
		}
	} else {
		v.live[p.ID] = part
	}
	if p.ViewerState == "playing" {
		v.watched = true
	}
	if problem != "" {
		v.problem = true
	}
	t := v.totals()
	if !v.written && !v.problem && !v.watched && t.bytes < viewingMinBytes {
		// A probe or an unread preparation: no history yet. The viewing is
		// kept for the window so the open that plays continues it.
		return playRecord{}, false
	}
	v.written = true
	row := p
	row.ID, row.UserID, row.Key, row.ClientID = v.row, v.k.user, v.k.key, v.k.client
	row.StartedAt = v.started
	row.BytesServed, row.Failovers, row.DownFailovers = t.bytes, t.failovers, t.downFailovers
	row.Experience = v.start.experience(t)
	row.Source, row.Resolution = nil, nil
	return playRecord{p: row, at: now, ended: len(v.live) == 0, problem: problem}, true
}

func (vs *viewings) forget(v *viewing) {
	if vs.byKey[v.k] == v {
		delete(vs.byKey, v.k)
	}
	for id := range v.live {
		if vs.bySession[id] == v {
			delete(vs.bySession, id)
		}
	}
}

// evict keeps the map bounded: ended viewings past the window go first,
// then the least recently active ended one. Live viewings are never
// dropped; the server's stream limits bound them.
func (vs *viewings) evict() {
	if len(vs.byKey) < maxViewings {
		return
	}
	var oldest *viewing
	for _, v := range vs.byKey {
		if len(v.live) > 0 {
			continue
		}
		if oldest == nil || v.last.Before(oldest.last) {
			oldest = v
		}
	}
	if oldest != nil {
		vs.forget(oldest)
	}
}

// prune drops ended viewings whose window has passed.
func (vs *viewings) prune(now time.Time) {
	for _, v := range vs.byKey {
		if len(v.live) == 0 && now.Sub(v.last) > viewingWindow {
			vs.forget(v)
		}
	}
}
