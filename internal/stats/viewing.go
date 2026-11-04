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
	bytes     int64
	failovers int
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
	problem  bool
	written  bool
}

func (v *viewing) totals() viewingPart {
	t := v.ended
	for _, p := range v.live {
		t.bytes += p.bytes
		t.failovers += p.failovers
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
	if ended {
		delete(v.live, p.ID)
		delete(vs.bySession, p.ID)
		v.ended.bytes += part.bytes
		v.ended.failovers += part.failovers
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
	row.BytesServed, row.Failovers = t.bytes, t.failovers
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
