package stats

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"github.com/braedonsaunders/ether/internal/logredact"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/store"
)

const (
	// TimelineCap bounds stored timeline rows per play: the opening steps
	// (resolve, candidates, initial decision) plus the tail leading into
	// stop or failure.
	TimelineCap      = 300
	timelineKeepHead = 60
	timelineKeepTail = TimelineCap - timelineKeepHead

	timelineQueueSize     = 2048
	timelineBatchMax      = 64
	timelineBatchWindow   = 25 * time.Millisecond
	timelineMaxTextRunes  = 512
	timelineOrphanMaxAge  = time.Hour
	maxTrackedLogSessions = 128
	maxBufferedDebugLines = 20
	maxSessionAliases     = 512
)

func init() {
	store.Register("stats", 14, `
CREATE TABLE stats_play_timeline (
	play_id TEXT NOT NULL,
	seq INTEGER NOT NULL,
	at_ms INTEGER NOT NULL,
	entry_type TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT '',
	level TEXT NOT NULL,
	message TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(play_id, seq)
);
CREATE INDEX stats_play_timeline_at ON stats_play_timeline(at_ms);
`)
}

// TimelineEntry is one event or session-scoped log line in a play's timeline.
type TimelineEntry struct {
	Seq      int64  `json:"seq"`
	AtMS     int64  `json:"atMs"`
	OffsetMS int64  `json:"offsetMs"`
	Type     string `json:"type"`
	Kind     string `json:"kind,omitempty"`
	Level    string `json:"level"`
	Message  string `json:"message"`
	Detail   string `json:"detail,omitempty"`
}

// PlayTimeline is the response payload of GET /api/activity/plays/{id}/timeline.
type PlayTimeline struct {
	PlayID    string          `json:"playId"`
	StartedMS int64           `json:"startedMs"`
	Failed    bool            `json:"failed"`
	Error     string          `json:"error"`
	Capped    bool            `json:"capped,omitempty"`
	Entries   []TimelineEntry `json:"entries"`
}

// TimelineWrite is one queued event or log line headed for stats_play_timeline.
type TimelineWrite struct {
	PlayID  string
	At      time.Time
	Type    string // "event" or "log"
	Kind    string
	Level   string // "debug", "info", "warn", "error"
	Message string
	Detail  string
}

type timelineCmd struct {
	write     *TimelineWrite
	aliasFrom string
	aliasInto string
	flush     chan struct{}
}

func clampTimelineText(s string) string {
	s = strings.TrimSpace(logredact.String(s))
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) > timelineMaxTextRunes {
		return string(r[:timelineMaxTextRunes-1]) + "…"
	}
	return s
}

func normalizeTimelineLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return "debug"
	case "warn", "warning":
		return "warn"
	case "error":
		return "error"
	default:
		return "info"
	}
}

// SessionLogSink captures slog records carrying a "session" attribute and
// forwards them asynchronously to stats.Service. Info and above are always
// recorded; Debug records are buffered briefly per session and flushed if and
// while the session enters a failing state.
type SessionLogSink struct {
	mu       sync.Mutex
	sessions map[string]*sessionDebugState
	aliases  map[string]string
	aliasOrd []string
	resolve  func(string) string
	enqueue  func(TimelineWrite)
	pending  []TimelineWrite
}

type sessionDebugState struct {
	failing bool
	updated time.Time
	debug   []TimelineWrite
}

// NewSessionLogSink constructs a sink ready to wrap the application's slog.Handler.
func NewSessionLogSink() *SessionLogSink {
	return &SessionLogSink{
		sessions: make(map[string]*sessionDebugState),
		aliases:  make(map[string]string),
	}
}

// Wrap installs the session-capturing slog.Handler around next.
func (k *SessionLogSink) Wrap(next slog.Handler) slog.Handler {
	if k == nil {
		return next
	}
	return &sessionLogHandler{sink: k, next: next}
}

// SetResolver maps external/broker/encoder session IDs to their owning play ID.
func (k *SessionLogSink) SetResolver(fn func(string) string) {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.resolve = fn
	k.mu.Unlock()
}

func (k *SessionLogSink) attach(enqueue func(TimelineWrite)) {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.enqueue = enqueue
	pending := k.pending
	k.pending = nil
	k.mu.Unlock()
	for _, w := range pending {
		enqueue(w)
	}
}

func (k *SessionLogSink) canonicalLocked(id string) string {
	for range 4 {
		next, ok := k.aliases[id]
		if !ok || next == "" || next == id {
			break
		}
		id = next
	}
	return id
}

func (k *SessionLogSink) resolveID(raw string) string {
	if raw == "" {
		return ""
	}
	k.mu.Lock()
	fn := k.resolve
	mapped := k.canonicalLocked(raw)
	k.mu.Unlock()
	if mapped != raw {
		return mapped
	}
	if fn != nil {
		if playID := fn(raw); playID != "" && playID != raw {
			k.mu.Lock()
			playID = k.canonicalLocked(playID)
			k.mu.Unlock()
			return playID
		}
	}
	return raw
}

// AliasSession maps a coalesced playback session ID onto its history row ID.
func (k *SessionLogSink) AliasSession(from, into string) {
	if k == nil || from == "" || into == "" || from == into {
		return
	}
	var flushed []TimelineWrite
	var enqueue func(TimelineWrite)
	k.mu.Lock()
	into = k.canonicalLocked(into)
	if from == into {
		k.mu.Unlock()
		return
	}
	if _, exists := k.aliases[from]; !exists {
		if len(k.aliasOrd) >= maxSessionAliases {
			drop := k.aliasOrd[0]
			k.aliasOrd = k.aliasOrd[1:]
			delete(k.aliases, drop)
		}
		k.aliasOrd = append(k.aliasOrd, from)
	}
	k.aliases[from] = into
	if old, ok := k.sessions[from]; ok {
		dst := k.stateLocked(into, old.updated)
		if old.failing && !dst.failing {
			dst.failing = true
			flushed = append(flushed, dst.debug...)
			dst.debug = nil
		}
		for _, d := range old.debug {
			d.PlayID = into
			if dst.failing {
				flushed = append(flushed, d)
			} else {
				dst.debug = appendBoundedDebug(dst.debug, d)
			}
		}
		delete(k.sessions, from)
	}
	enqueue = k.enqueue
	k.mu.Unlock()
	if enqueue != nil {
		for _, w := range flushed {
			enqueue(w)
		}
	}
}

// MarkFailing enables Debug log capture for sessionID and flushes any buffered
// Debug lines that preceded the failure.
func (k *SessionLogSink) MarkFailing(sessionID string, now time.Time) {
	if k == nil || sessionID == "" {
		return
	}
	playID := k.resolveID(sessionID)
	if now.IsZero() {
		now = time.Now()
	}
	k.mu.Lock()
	st := k.stateLocked(playID, now)
	st.failing = true
	flushed := st.debug
	st.debug = nil
	enqueue := k.enqueue
	if enqueue == nil && len(flushed) > 0 {
		k.pending = appendBoundedPending(k.pending, flushed...)
		flushed = nil
	}
	k.mu.Unlock()
	if enqueue != nil {
		for _, w := range flushed {
			enqueue(w)
		}
	}
}

func (k *SessionLogSink) stateLocked(id string, now time.Time) *sessionDebugState {
	if st, ok := k.sessions[id]; ok {
		st.updated = now
		return st
	}
	if len(k.sessions) >= maxTrackedLogSessions {
		var oldestID string
		var oldestAt time.Time
		for sid, st := range k.sessions {
			if oldestID == "" || st.updated.Before(oldestAt) {
				oldestID, oldestAt = sid, st.updated
			}
		}
		if oldestID != "" {
			delete(k.sessions, oldestID)
		}
	}
	st := &sessionDebugState{updated: now}
	k.sessions[id] = st
	return st
}

func appendBoundedDebug(buf []TimelineWrite, w TimelineWrite) []TimelineWrite {
	buf = append(buf, w)
	if len(buf) > maxBufferedDebugLines {
		buf = slices.Clone(buf[len(buf)-maxBufferedDebugLines:])
	}
	return buf
}

func appendBoundedPending(buf []TimelineWrite, ws ...TimelineWrite) []TimelineWrite {
	buf = append(buf, ws...)
	if len(buf) > 256 {
		buf = slices.Clone(buf[len(buf)-256:])
	}
	return buf
}

func (k *SessionLogSink) ingestLog(at time.Time, rawSession string, level slog.Level, msg, detail string) {
	playID := k.resolveID(rawSession)
	if playID == "" {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	lvl := "info"
	switch {
	case level >= slog.LevelError:
		lvl = "error"
	case level >= slog.LevelWarn:
		lvl = "warn"
	case level < slog.LevelInfo:
		lvl = "debug"
	}
	entry := TimelineWrite{
		PlayID:  playID,
		At:      at,
		Type:    "log",
		Level:   lvl,
		Message: clampTimelineText(msg),
		Detail:  clampTimelineText(detail),
	}
	if entry.Message == "" {
		return
	}

	var toSend []TimelineWrite
	k.mu.Lock()
	st := k.stateLocked(playID, at)
	if level >= slog.LevelWarn && !st.failing {
		st.failing = true
		toSend = append(toSend, st.debug...)
		st.debug = nil
	}
	if level < slog.LevelInfo && !st.failing {
		st.debug = appendBoundedDebug(st.debug, entry)
		k.mu.Unlock()
		return
	}
	toSend = append(toSend, entry)
	enqueue := k.enqueue
	if enqueue == nil {
		k.pending = appendBoundedPending(k.pending, toSend...)
		k.mu.Unlock()
		return
	}
	k.mu.Unlock()
	for _, w := range toSend {
		enqueue(w)
	}
}

type sessionLogHandler struct {
	sink    *SessionLogSink
	next    slog.Handler
	session string
	group   string
	attrs   []string
}

// Enabled keeps Debug as cheap as before: a Debug record is built only when
// the server logs Debug anyway or the logger is already bound to a session.
// Otherwise every Debug call in the app would be formatted just to be
// checked for a session attribute.
func (h *sessionLogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	if l >= slog.LevelInfo || h.session != "" {
		return true
	}
	return h.next != nil && h.next.Enabled(ctx, l)
}

func (h *sessionLogHandler) Handle(ctx context.Context, r slog.Record) error {
	session := h.session
	var parts []string
	if session == "" {
		r.Attrs(func(a slog.Attr) bool {
			if session == "" && h.group == "" && a.Key == "session" {
				if v := a.Value.Resolve(); v.Kind() == slog.KindString {
					session = v.String()
				}
			}
			return session == ""
		})
	}
	if session != "" {
		if len(h.attrs) > 0 {
			parts = append(parts, h.attrs...)
		}
		r.Attrs(func(a slog.Attr) bool {
			parts = appendFormattedAttr(parts, h.group, a)
			return true
		})
		h.sink.ingestLog(r.Time, session, r.Level, r.Message, strings.Join(parts, " · "))
	}
	if h.next != nil && h.next.Enabled(ctx, r.Level) {
		return h.next.Handle(ctx, r)
	}
	return nil
}

func (h *sessionLogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	session := h.session
	parts := slices.Clone(h.attrs)
	for _, a := range as {
		if session == "" && h.group == "" && a.Key == "session" {
			if v := a.Value.Resolve(); v.Kind() == slog.KindString {
				session = v.String()
			}
		}
		parts = appendFormattedAttr(parts, h.group, a)
	}
	var next slog.Handler
	if h.next != nil {
		next = h.next.WithAttrs(as)
	}
	return &sessionLogHandler{sink: h.sink, next: next, session: session, group: h.group, attrs: parts}
}

func (h *sessionLogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	group := name
	if h.group != "" {
		group = h.group + "." + name
	}
	var next slog.Handler
	if h.next != nil {
		next = h.next.WithGroup(name)
	}
	return &sessionLogHandler{sink: h.sink, next: next, session: h.session, group: group, attrs: slices.Clone(h.attrs)}
}

func appendFormattedAttr(dst []string, group string, a slog.Attr) []string {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		sub := a.Key
		if group != "" && sub != "" {
			sub = group + "." + sub
		} else if group != "" {
			sub = group
		}
		for _, g := range v.Group() {
			dst = appendFormattedAttr(dst, sub, g)
		}
		return dst
	}
	key := a.Key
	if group != "" && key != "" {
		key = group + "." + key
	}
	if key == "" || key == "session" || key == "component" {
		return dst
	}
	if logredact.Sensitive(a.Key) || (group != "" && logredact.Sensitive(group)) {
		return append(dst, key+"="+logredact.Mask)
	}
	var val string
	switch v.Kind() {
	case slog.KindString:
		val = v.String()
	case slog.KindInt64:
		val = strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		val = strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		val = strconv.FormatFloat(v.Float64(), 'f', -1, 64)
	case slog.KindBool:
		val = strconv.FormatBool(v.Bool())
	case slog.KindDuration:
		d := v.Duration()
		if d >= time.Second {
			val = d.Round(100 * time.Millisecond).String()
		} else {
			val = d.Round(time.Millisecond).String()
		}
	case slog.KindTime:
		if !v.Time().IsZero() {
			val = v.Time().UTC().Format(time.RFC3339)
		}
	case slog.KindAny:
		switch x := v.Any().(type) {
		case nil:
			return dst
		case error:
			val = x.Error()
		case fmt.Stringer:
			val = x.String()
		default:
			val = fmt.Sprint(x)
		}
	default:
		val = v.String()
	}
	val = strings.TrimSpace(logredact.String(val))
	if val == "" || val == "<nil>" {
		return dst
	}
	return append(dst, key+"="+val)
}

// AttachLogSink connects a SessionLogSink to this Service's async timeline writer.
func (s *Service) AttachLogSink(sink *SessionLogSink) {
	if s == nil || sink == nil {
		return
	}
	s.timelineMu.Lock()
	s.logSink = sink
	s.timelineMu.Unlock()
	sink.attach(s.enqueueTimelineWrite)
}

// ObserveTimeline records structured playback lifecycle events asynchronously.
func (s *Service) ObserveTimeline(ev playback.TimelineEvent) {
	if s == nil || ev.SessionID == "" {
		return
	}
	at := ev.At
	if at.IsZero() {
		at = s.opts.Now()
	}
	s.timelineMu.Lock()
	sink := s.logSink
	s.timelineMu.Unlock()
	playID := ev.SessionID
	if sink != nil {
		playID = sink.resolveID(ev.SessionID)
		if ev.Failing || ev.Level == "error" || ev.Kind == "error" || ev.Kind == "failover" || ev.Kind == "stall" {
			sink.MarkFailing(playID, at)
		}
	}
	msg := clampTimelineText(ev.Message)
	if msg == "" {
		return
	}
	s.enqueueTimelineWrite(TimelineWrite{
		PlayID:  playID,
		At:      at,
		Type:    "event",
		Kind:    strings.TrimSpace(ev.Kind),
		Level:   normalizeTimelineLevel(ev.Level),
		Message: msg,
		Detail:  clampTimelineText(ev.Detail),
	})
}

func (s *Service) aliasTimelineSession(from, into string) {
	if from == "" || into == "" || from == into {
		return
	}
	s.timelineMu.Lock()
	sink := s.logSink
	s.timelineMu.Unlock()
	if sink != nil {
		sink.AliasSession(from, into)
	}
	s.ensureTimelineWorker()
	select {
	case s.timelineCh <- timelineCmd{aliasFrom: from, aliasInto: into}:
	default:
	}
}

func (s *Service) enqueueTimelineWrite(w TimelineWrite) {
	if w.PlayID == "" || w.Message == "" {
		return
	}
	if w.At.IsZero() {
		w.At = s.opts.Now()
	}
	s.ensureTimelineWorker()
	select {
	case s.timelineCh <- timelineCmd{write: &w}:
	default:
		// Never block the playback or logging hot path if the queue is full.
	}
}

func (s *Service) ensureTimelineWorker() {
	s.timelineOnce.Do(func() {
		s.timelineCh = make(chan timelineCmd, timelineQueueSize)
		s.timelineStop = make(chan struct{})
		s.timelineDone = make(chan struct{})
		go s.runTimelineWriter()
	})
}

// FlushTimeline waits for queued timeline entries to be committed.
func (s *Service) FlushTimeline(ctx context.Context) error {
	s.timelineMu.Lock()
	started := s.timelineDone != nil
	s.timelineMu.Unlock()
	if !started && s.timelineCh == nil {
		return nil
	}
	if s.timelineCh == nil {
		return nil
	}
	done := make(chan struct{})
	select {
	case s.timelineCh <- timelineCmd{flush: done}:
	case <-s.timelineDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-s.timelineDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) closeTimeline(ctx context.Context) error {
	if s.timelineStop == nil {
		return nil
	}
	_ = s.FlushTimeline(ctx)
	s.timelineStopOnce.Do(func() { close(s.timelineStop) })
	select {
	case <-s.timelineDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) runTimelineWriter() {
	defer close(s.timelineDone)
	seqByPlay := make(map[string]int64)
	aliases := make(map[string]string)
	var held []TimelineWrite

	canonical := func(id string) string {
		for range 4 {
			next, ok := aliases[id]
			if !ok || next == "" || next == id {
				break
			}
			id = next
		}
		return id
	}

	var batch []TimelineWrite
	var flushes []chan struct{}
	flushNow := func() {
		if len(held) > 0 && !s.opts.Availability.Down() {
			batch = append(held, batch...)
			held = nil
		}
		if len(batch) > 0 {
			if s.opts.Availability.Down() {
				held = append(held, batch...)
				if len(held) > 512 {
					held = slices.Clone(held[len(held)-512:])
				}
			} else {
				wctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				if err := s.writeTimelineBatch(wctx, batch, seqByPlay, canonical); err != nil {
					s.opts.Logger.Debug("write playback timeline", "err", err)
				}
				cancel()
			}
			batch = batch[:0]
		}
		for _, ch := range flushes {
			close(ch)
		}
		flushes = flushes[:0]
	}

	applyAlias := func(from, into string) {
		if from == "" || into == "" || from == into {
			return
		}
		into = canonical(into)
		if from == into {
			return
		}
		aliases[from] = into
		if len(aliases) > maxSessionAliases {
			for k := range aliases {
				if k != from {
					delete(aliases, k)
					break
				}
			}
		}
		for i := range batch {
			if batch[i].PlayID == from {
				batch[i].PlayID = into
			}
		}
		for i := range held {
			if held[i].PlayID == from {
				held[i].PlayID = into
			}
		}
		if !s.opts.Availability.Down() {
			wctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = s.rekeyTimelineRows(wctx, from, into, seqByPlay)
			cancel()
		}
	}

	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timerC = nil
		}
	}
	armTimer := func() {
		if timerC != nil {
			return
		}
		if timer == nil {
			timer = time.NewTimer(timelineBatchWindow)
		} else {
			timer.Reset(timelineBatchWindow)
		}
		timerC = timer.C
	}

	for {
		select {
		case cmd := <-s.timelineCh:
			if cmd.aliasFrom != "" {
				applyAlias(cmd.aliasFrom, cmd.aliasInto)
			}
			if cmd.write != nil {
				w := *cmd.write
				w.PlayID = canonical(w.PlayID)
				batch = append(batch, w)
				if len(batch) >= timelineBatchMax {
					stopTimer()
					flushNow()
				} else {
					armTimer()
				}
			}
			if cmd.flush != nil {
				flushes = append(flushes, cmd.flush)
				stopTimer()
				flushNow()
			}
		case <-timerC:
			timerC = nil
			flushNow()
		case <-s.timelineStop:
			stopTimer()
			for {
				select {
				case cmd := <-s.timelineCh:
					if cmd.aliasFrom != "" {
						applyAlias(cmd.aliasFrom, cmd.aliasInto)
					}
					if cmd.write != nil {
						w := *cmd.write
						w.PlayID = canonical(w.PlayID)
						batch = append(batch, w)
					}
					if cmd.flush != nil {
						flushes = append(flushes, cmd.flush)
					}
				default:
					flushNow()
					return
				}
			}
		}
	}
}

func (s *Service) nextTimelineSeq(ctx context.Context, tx *sql.Tx, seqByPlay map[string]int64, playID string) (int64, error) {
	if cur, ok := seqByPlay[playID]; ok {
		return cur, nil
	}
	var maxSeq int64
	if err := tx.QueryRowContext(ctx, `SELECT CAST(COALESCE(MAX(seq),0) AS BIGINT) FROM stats_play_timeline WHERE play_id=?`, playID).Scan(&maxSeq); err != nil {
		return 0, err
	}
	if len(seqByPlay) >= 1024 {
		for k := range seqByPlay {
			delete(seqByPlay, k)
			break
		}
	}
	seqByPlay[playID] = maxSeq
	return maxSeq, nil
}

func (s *Service) writeTimelineBatch(ctx context.Context, batch []TimelineWrite, seqByPlay map[string]int64, canonical func(string) string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	touched := make(map[string]int64)
	for _, w := range batch {
		playID := canonical(w.PlayID)
		if playID == "" {
			continue
		}
		cur, err := s.nextTimelineSeq(ctx, tx, seqByPlay, playID)
		if err != nil {
			return err
		}
		cur++
		seqByPlay[playID] = cur
		touched[playID] = cur
		entryType := w.Type
		if entryType != "event" && entryType != "log" {
			entryType = "event"
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stats_play_timeline(play_id,seq,at_ms,entry_type,kind,level,message,detail) VALUES(?,?,?,?,?,?,?,?)`,
			playID, cur, w.At.UnixMilli(), entryType, w.Kind, normalizeTimelineLevel(w.Level), clampTimelineText(w.Message), clampTimelineText(w.Detail),
		); err != nil {
			delete(seqByPlay, playID)
			return err
		}
	}
	for playID, maxSeq := range touched {
		if maxSeq > TimelineCap {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM stats_play_timeline WHERE play_id=? AND seq>? AND seq<=?`,
				playID, timelineKeepHead, maxSeq-timelineKeepTail,
			); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Service) rekeyTimelineRows(ctx context.Context, from, into string, seqByPlay map[string]int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `SELECT at_ms,entry_type,kind,level,message,detail FROM stats_play_timeline WHERE play_id=? ORDER BY seq ASC`, from)
	if err != nil {
		return err
	}
	type row struct {
		atMS                        int64
		entryType, kind, level, msg string
		detail                      string
	}
	var moved []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.atMS, &r.entryType, &r.kind, &r.level, &r.msg, &r.detail); err != nil {
			rows.Close()
			return err
		}
		moved = append(moved, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(moved) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM stats_play_timeline WHERE play_id=?`, from); err != nil {
		return err
	}
	delete(seqByPlay, from)

	cur, err := s.nextTimelineSeq(ctx, tx, seqByPlay, into)
	if err != nil {
		return err
	}
	for _, r := range moved {
		cur++
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stats_play_timeline(play_id,seq,at_ms,entry_type,kind,level,message,detail) VALUES(?,?,?,?,?,?,?,?)`,
			into, cur, r.atMS, r.entryType, r.kind, r.level, r.msg, r.detail,
		); err != nil {
			delete(seqByPlay, into)
			return err
		}
	}
	seqByPlay[into] = cur
	if cur > TimelineCap {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM stats_play_timeline WHERE play_id=? AND seq>? AND seq<=?`,
			into, timelineKeepHead, cur-timelineKeepTail,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PlayTimeline returns the recorded timeline events and session logs for a play
// or live session; nil when the play does not exist. Plays recorded before
// timeline capture return an empty Entries slice rather than scanning log files.
func (s *Service) PlayTimeline(ctx context.Context, id string) (*PlayTimeline, error) {
	if id == "" {
		return nil, nil
	}
	flushCtx, cancel := context.WithTimeout(ctx, time.Second)
	_ = s.FlushTimeline(flushCtx)
	cancel()

	now := s.opts.Now().UTC()
	live, isLive := s.liveSession(id)
	s.vmu.Lock()
	lv, hasViewing := s.viewings.lookup(id, live, now)
	s.vmu.Unlock()

	rowID := id
	if hasViewing && lv.row != "" {
		rowID = lv.row
	}

	var (
		startedSec, endedSec                    int64
		errText                                 string
		measured, played, pictured, startFailed int
		foundRow                                bool
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT started,ended,error,measured,played,pictured,start_failed FROM stats_plays WHERE id=?`,
		rowID,
	).Scan(&startedSec, &endedSec, &errText, &measured, &played, &pictured, &startFailed)
	switch {
	case err == nil:
		foundRow = true
	case errors.Is(err, sql.ErrNoRows):
		if !isLive || live == nil {
			return nil, nil
		}
	default:
		return nil, err
	}

	out := &PlayTimeline{
		PlayID:  id,
		Entries: []TimelineEntry{},
	}
	if foundRow {
		out.StartedMS = startedSec * 1000
		out.Error = clampTimelineText(errText)
		measuredFailed := measured == 1 && played == 1 && endedSec > 0 && (pictured == 0 || startFailed != 0)
		out.Failed = !isLive && (out.Error != "" || measuredFailed)
	} else if isLive && live != nil {
		start := live.StartedAt
		if hasViewing && !lv.started.IsZero() && (start.IsZero() || lv.started.Before(start)) {
			start = lv.started
		}
		if start.IsZero() {
			start = now
		}
		out.StartedMS = start.UnixMilli()
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT seq,at_ms,entry_type,kind,level,message,detail FROM stats_play_timeline WHERE play_id=? ORDER BY at_ms ASC, seq ASC`,
		rowID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var maxSeq int64
	for rows.Next() {
		var e TimelineEntry
		if err := rows.Scan(&e.Seq, &e.AtMS, &e.Type, &e.Kind, &e.Level, &e.Message, &e.Detail); err != nil {
			return nil, err
		}
		e.Message = clampTimelineText(e.Message)
		e.Detail = clampTimelineText(e.Detail)
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
		out.Entries = append(out.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	baseMS := out.StartedMS
	if len(out.Entries) > 0 && (baseMS <= 0 || out.Entries[0].AtMS < baseMS) {
		baseMS = out.Entries[0].AtMS
		if out.StartedMS <= 0 {
			out.StartedMS = baseMS
		}
	}
	for i := range out.Entries {
		out.Entries[i].OffsetMS = max(0, out.Entries[i].AtMS-baseMS)
	}
	out.Capped = maxSeq > TimelineCap
	return out, nil
}

// ActivityHandler serves owner-authenticated activity endpoints under /api/activity/.
func (s *Service) ActivityHandler() http.Handler {
	m := &route.Mux{}
	m.HandleFunc("GET /plays/{id}/timeline", s.servePlayTimeline)
	return m
}

func (s *Service) servePlayTimeline(w http.ResponseWriter, r *http.Request) {
	tl, err := s.PlayTimeline(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusInternalServerError, "Timeline unavailable")
		return
	}
	if tl == nil {
		fail(w, http.StatusNotFound, "Play not found")
		return
	}
	reply(w, http.StatusOK, tl)
}
