package plugins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/events"
)

// LegacyToCanonicalEvent maps the 12 EPP/1 plugin event names to their
// canonical domain.noun.verb event types on Ether's event bus.
var LegacyToCanonicalEvent = map[string]string{
	EventPlaybackStarted:   "playback.session.started",
	EventPlaybackPaused:    "playback.session.paused",
	EventPlaybackResumed:   "playback.session.resumed",
	EventPlaybackStopped:   "playback.session.stopped",
	EventPlaybackCompleted: "playback.session.completed",
	EventPlaybackError:     "playback.session.failed",
	EventPlaybackFailover:  "playback.source.failover",
	EventTitleAvailable:    "library.title.available",
	EventViewerAdded:       "viewer.account.joined",
	EventViewerAccess:      "viewer.access.changed",
	EventCollectionRebuilt: "library.collection.rebuilt",
	EventServerHealth:      "system.health.changed",
}

// CanonicalToLegacyEvent maps canonical event types back to the 12 legacy
// EPP/1 plugin event names so existing plugin subscriptions keep working
// unchanged through the event bus.
var CanonicalToLegacyEvent = map[string]string{
	"playback.session.started":   EventPlaybackStarted,
	"playback.session.paused":    EventPlaybackPaused,
	"playback.session.resumed":   EventPlaybackResumed,
	"playback.session.stopped":   EventPlaybackStopped,
	"playback.session.completed": EventPlaybackCompleted,
	"playback.session.failed":    EventPlaybackError,
	"playback.source.failover":   EventPlaybackFailover,
	"library.title.available":    EventTitleAvailable,
	"viewer.account.joined":      EventViewerAdded,
	"viewer.access.changed":      EventViewerAccess,
	"library.collection.rebuilt": EventCollectionRebuilt,
	"system.health.changed":      EventServerHealth,
}

// IsSubscribableEvent reports whether name is either one of the 12 legacy
// EPP/1 event names or a valid canonical event type ("domain.noun.verb" or
// "plugin:<pluginId>/<key>").
func IsSubscribableEvent(name string) bool {
	return KnownEvents[name] || events.ValidateType(name) == nil
}

func newEventID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Publish normalizes and fans out one Ether event: it is written to the
// 7-day event log and queued for every enabled plugin that subscribed to
// it (and was not revoked). Viewer identity is stripped per plugin unless
// the plugin's grants allow it. Both legacy EPP/1 names and canonical
// domain.noun.verb names are accepted and bridged to matching subscribers.
func (m *Manager) Publish(ctx context.Context, ev Event) {
	if ev.Name == EventPluginFoldersChanged || !IsSubscribableEvent(ev.Name) {
		return
	}
	names := []string{ev.Name}
	if canonical, ok := LegacyToCanonicalEvent[ev.Name]; ok {
		names = append(names, canonical)
	} else if legacy, ok := CanonicalToLegacyEvent[ev.Name]; ok {
		names = append(names, legacy)
	}
	if ev.ID == "" {
		ev.ID = newEventID()
	}
	if ev.At.IsZero() {
		ev.At = m.now()
	}
	canonicalType := ev.Name
	if c, ok := LegacyToCanonicalEvent[ev.Name]; ok {
		canonicalType = c
	}
	m.fanOutEvent(ctx, canonicalType, names, ev, "")
}

// StartEventBridge subscribes the plugin host to Ether's events.Bus so plugins
// receive both canonical event types and translated legacy 12 event names
// without blocking publishers. Safe to call multiple times; returns a function
// that detaches the bridge subscription.
func (m *Manager) StartEventBridge(ctx context.Context) func() {
	bus := m.flowsEventBus()
	if bus == nil {
		return func() {}
	}
	m.eventBridgeMu.Lock()
	if m.eventBridgeSub != nil {
		m.eventBridgeMu.Unlock()
		return func() {}
	}
	sub := bus.SubscribeFunc(ctx, events.SubscriptionOptions{
		Name:   "plugins.bridge",
		Buffer: events.DefaultSubscriberBuffer,
	}, m.HandleBusEvent)
	m.eventBridgeSub = sub
	m.eventBridgeMu.Unlock()
	return func() {
		m.eventBridgeMu.Lock()
		if m.eventBridgeSub == sub {
			m.eventBridgeSub = nil
		}
		m.eventBridgeMu.Unlock()
		sub.Unsubscribe()
	}
}

// HandleBusEvent delivers one events.Bus envelope to any enabled plugin
// subscribed to its canonical type or mapped legacy EPP/1 event name.
// When no plugin subscribes to either name it does zero database work.
func (m *Manager) HandleBusEvent(ctx context.Context, env events.Envelope) {
	if env.Meta.DryRun || env.Type == "" || env.Type == EventPluginFoldersChanged {
		return
	}
	names := []string{env.Type}
	if legacy, ok := CanonicalToLegacyEvent[env.Type]; ok {
		names = append(names, legacy)
	}
	viewerID := env.Correlation.ViewerID
	if viewerID == 0 {
		viewerID = viewerIDFromPayload(env.Payload)
	}
	at := env.OccurredAt
	if at.IsZero() {
		at = m.now()
	}
	id := env.ID
	if id == "" {
		id = newEventID()
	}
	payload := env.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	m.fanOutEvent(ctx, env.Type, names, Event{
		ID:       id,
		Name:     env.Type,
		At:       at,
		ViewerID: viewerID,
		Payload:  payload,
	}, env.Source)
}

type eventDeliveryTarget struct {
	rec  Record
	name string
}

func (m *Manager) fanOutEvent(ctx context.Context, canonicalType string, names []string, base Event, source string) {
	m.mu.Lock()
	var targets []eventDeliveryTarget
	for _, name := range names {
		for _, r := range m.eventSubs[name] {
			targets = append(targets, eventDeliveryTarget{rec: r, name: name})
		}
	}
	m.mu.Unlock()
	if len(targets) == 0 {
		return
	}

	reg, _ := m.flowsRegistries()
	var schema events.EventSchema
	hasSchema := false
	if reg != nil && canonicalType != "" {
		schema, hasSchema = reg.Get(canonicalType)
	}

	var producer, itemOwner string
	switch canonicalType {
	case events.EventViewerWatchScrobbled, events.EventViewerWatchWatched, events.EventViewerWatchUnwatched, events.EventViewerWatchProgress:
		if id, ok := strings.CutPrefix(source, events.PluginPrefix); ok {
			producer = id
		}
		var item struct {
			Ref core.ItemRef `json:"itemRef"`
		}
		if json.Unmarshal(base.Payload, &item) == nil {
			itemOwner = item.Ref.Plugin()
		}
	}

	logged := false
	for _, target := range targets {
		r := target.rec
		if r.ID == producer || !r.Enabled || r.Manifest.Ether == nil || !r.Manifest.HasCapability(CapEvents) {
			continue
		}
		sub := false
		if evMeta := r.Manifest.Ether.Contributes.Events; evMeta != nil {
			for _, n := range evMeta.Subscribe {
				if n == target.name {
					sub = true
					break
				}
			}
		}
		if !sub {
			continue
		}
		g, err := m.store.grants(ctx, r.ID)
		if err != nil || g.revoked("events") {
			continue
		}
		out := base
		out.Name = target.name
		if !logged {
			m.store.logEvent(ctx, out)
			logged = true
		}
		allowViewer := viewerOK(g, normLevel(r.Manifest.Ether.Permissions.Viewers.Identity), base.ViewerID)
		if !allowViewer {
			out.ViewerID = 0
		}
		out.Payload = sanitizePluginEventPayload(out.Payload, schema, hasSchema, allowViewer)
		if itemOwner != "" && itemOwner != r.ID {
			if fields, err := (events.Envelope{Payload: out.Payload}).PayloadMap(); err == nil {
				delete(fields, "itemRef")
				out.Payload, _ = json.Marshal(fields)
			}
		}
		dropped, err := m.store.enqueue(ctx, r.ID, out)
		if err == nil {
			m.noteOutboxDrops(ctx, r, dropped)
		}
	}
}

func (m *Manager) noteOutboxDrops(ctx context.Context, r Record, dropped int) {
	if dropped <= 0 {
		return
	}
	m.healthMu.Lock()
	if h, err := m.store.health(ctx, r.ID); err == nil {
		h.Drops += int64(dropped)
		_ = m.store.putHealth(ctx, r.ID, h)
	}
	m.healthMu.Unlock()
	m.emitBusEvent(events.EventPluginQuotaExceeded, r.ID, events.PluginQuotaExceededPayload{
		PluginID:  r.ID,
		Name:      r.Name,
		QuotaKind: "events_outbox",
		Used:      int64(OutboxCap),
		Limit:     int64(OutboxCap),
		Drops:     int64(dropped),
	}, false)
}

func sanitizePluginEventPayload(raw json.RawMessage, schema events.EventSchema, hasSchema, allowViewer bool) json.RawMessage {
	payload := raw
	if hasSchema && len(payload) > 0 {
		if m, err := (events.Envelope{Payload: payload}).PayloadMap(); err == nil {
			redacted := schema.Redact(m, events.RedactOptions{StripPII: !allowViewer})
			if b, err := json.Marshal(redacted); err == nil {
				payload = b
			}
		}
	}
	if !allowViewer {
		payload = stripViewer(payload)
	}
	return payload
}

func viewerIDFromPayload(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var doc struct {
		ViewerID int64 `json:"viewerId"`
		Viewer   *struct {
			ID int64 `json:"id"`
		} `json:"viewer"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return 0
	}
	if doc.ViewerID > 0 {
		return doc.ViewerID
	}
	if doc.Viewer != nil && doc.Viewer.ID > 0 {
		return doc.Viewer.ID
	}
	return 0
}

// indexEventSubscriptions is updated together with the plugin's descriptors.
func (m *Manager) indexEventSubscriptions(r Record) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, subscribers := range m.eventSubs {
		delete(subscribers, r.ID)
	}
	if !r.Enabled || r.Manifest.Ether == nil || !r.Manifest.HasCapability(CapEvents) || r.Manifest.Ether.Contributes.Events == nil {
		return
	}
	for _, name := range r.Manifest.Ether.Contributes.Events.Subscribe {
		if !IsSubscribableEvent(name) {
			continue
		}
		if m.eventSubs[name] == nil {
			m.eventSubs[name] = make(map[string]Record)
		}
		m.eventSubs[name][r.ID] = r
	}
}

func stripViewer(payload json.RawMessage) json.RawMessage {
	var doc any
	if json.Unmarshal(payload, &doc) != nil {
		return json.RawMessage(`{}`)
	}
	changed := false
	var strip func(any)
	strip = func(v any) {
		switch value := v.(type) {
		case map[string]any:
			for k, child := range value {
				lk := strings.ToLower(k)
				if strings.Contains(lk, "viewer") || strings.Contains(lk, "user") {
					delete(value, k)
					changed = true
				} else {
					strip(child)
				}
			}
		case []any:
			for _, child := range value {
				strip(child)
			}
		}
	}
	strip(doc)
	if !changed {
		return payload
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return payload
	}
	return out
}

// NormalizePlayback maps a playback.Manager event kind to §5.7 names.
// state is the raw timeline state for updates (playing, paused, stopped).
// It returns "" for kinds with no normalized event.
func NormalizePlayback(kind, state string) string {
	switch kind {
	case "start", "started":
		return EventPlaybackStarted
	case "stop", "stopped":
		return EventPlaybackStopped
	case "completed", "complete":
		return EventPlaybackCompleted
	case "error":
		return EventPlaybackError
	case "failover", "downfailover":
		return EventPlaybackFailover
	case "update", "progress":
		switch state {
		case "paused":
			return EventPlaybackPaused
		case "playing":
			return EventPlaybackResumed
		}
	}
	return ""
}

// NormalizeNotify maps a notify event kind to §5.7 names.
func NormalizeNotify(kind string) string {
	switch kind {
	case "title.available":
		return EventTitleAvailable
	case "user.joined":
		return EventViewerAdded
	case "playback.error":
		return EventPlaybackError
	case "playback.failover":
		return EventPlaybackFailover
	case "playback.start":
		return EventPlaybackStarted
	case "playback.stop":
		return EventPlaybackStopped
	}
	return ""
}

// DeliverOutbox sends queued events to one plugin in batches of 50 and
// deletes the ones it acks. A missing ack keeps events for redelivery
// (at least once).
func (m *Manager) DeliverOutbox(ctx context.Context, pluginID string) error {
	for {
		evs, ids, err := m.store.outboxBatch(ctx, pluginID, 50)
		if err != nil || len(evs) == 0 {
			return err
		}
		body, _ := json.Marshal(map[string]any{"events": evs})
		resp, err := m.Call(ctx, pluginID, Request{Method: "POST", Path: "/ether/event", Body: body})
		if err != nil {
			return err
		}
		if resp.Status < 200 || resp.Status >= 300 {
			return fmt.Errorf("event delivery failed with status %d", resp.Status)
		}
		var out struct {
			Ack []string `json:"ack"`
		}
		_ = json.Unmarshal(resp.Body, &out)
		acked := map[string]bool{}
		for _, id := range out.Ack {
			acked[id] = true
		}
		var done []int64
		for i, ev := range evs {
			if acked[ev.ID] {
				done = append(done, ids[i])
			}
		}
		if len(done) == 0 {
			return nil // nothing acked; try again later
		}
		if err := m.store.ackOutbox(ctx, done); err != nil {
			return err
		}
		if len(done) < len(evs) {
			return nil
		}
	}
}

// Run drives health checks, scheduled jobs and outbox delivery until ctx
// ends. Leader plugins run jobs and event delivery only on the leader;
// every_node plugins run everywhere. Health checks run on every node.
type scheduleFlight struct {
	cancel context.CancelFunc
}

func (m *Manager) waitSchedule() {
	m.scheduleWG.Wait()
}

func (m *Manager) scheduleBusyLocked(pluginID, callPath string) bool {
	key := pluginID + "|" + callPath
	if m.scheduleInFlight[key] != nil {
		return true
	}
	if j := m.scheduleJobs[key]; j != nil {
		if j.snapshot().State == "running" {
			return true
		}
		delete(m.scheduleJobs, key)
	}
	if strings.HasPrefix(callPath, "/ether/job/") {
		if old, ok := m.jobs.Load(key); ok && old.(*actionJob).snapshot().State == "running" {
			return true
		}
	}
	return false
}

func (m *Manager) runScheduledCall(ctx context.Context, flight *scheduleFlight, pluginID, callPath, key string) {
	defer m.scheduleWG.Done()
	defer flight.cancel()
	startedAt := m.now()
	resp, err := m.Call(WithBackgroundCall(ctx), pluginID, Request{Method: "POST", Path: callPath})
	var started *actionJob
	var startErr error
	if err == nil && ctx.Err() == nil && resp.Status >= 200 && resp.Status < 300 && len(resp.Body) > 0 {
		started, startErr = m.startResolverJob(pluginID, resp.Body, nil, "")
		if started != nil && ctx.Err() != nil {
			started.cancel()
			started = nil
		}
	}
	var cancelStarted *actionJob
	m.mu.Lock()
	if m.scheduleInFlight[key] == flight {
		delete(m.scheduleInFlight, key)
		if started != nil {
			if m.scheduleJobs == nil {
				m.scheduleJobs = map[string]*actionJob{}
			}
			m.scheduleJobs[key] = started
		}
	} else if started != nil {
		cancelStarted = started
	}
	m.mu.Unlock()
	if cancelStarted != nil {
		cancelStarted.cancel()
	}
	if startErr != nil {
		m.log.Warn("plugin schedule: start job", "plugin", pluginID, "call", callPath, "err", startErr)
	}
	var jobErr string
	switch {
	case err != nil && ctx.Err() == nil:
		jobErr = m.store.redactKnown(context.Background(), pluginID, err.Error())
	case startErr != nil:
		jobErr = m.store.redactKnown(context.Background(), pluginID, startErr.Error())
	case err == nil && resp.Status >= 400:
		redact := func(s string) string {
			return m.store.redactKnown(context.Background(), pluginID, s)
		}
		jobErr = formatJobHTTPError(resp.Status, resp.Body, redact).Error()
		m.log.Warn("plugin schedule: job failed", "plugin", pluginID, "call", callPath, "err", jobErr)
	}
	if jobErr != "" {
		durMS := max(m.now().Sub(startedAt).Milliseconds(), int64(0))
		m.emitBusEvent(events.EventPluginJobFailed, pluginID, events.PluginJobFailedPayload{
			PluginID:   pluginID,
			Name:       m.pluginDisplayName(context.Background(), pluginID),
			Job:        callPath,
			Kind:       "scheduled",
			Error:      jobErr,
			DurationMS: durMS,
		}, false)
	}
}

func (m *Manager) Run(ctx context.Context) {
	stopBridge := m.StartEventBridge(ctx)
	defer stopBridge()
	defer m.scheduleWG.Wait()
	inspectorTick := time.NewTicker(250 * time.Millisecond)
	defer inspectorTick.Stop()
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.flushAdapterCalls(flushCtx)
	}()
	defer m.cancelActionJobs("")
	defer m.cancelRemoteHashes("")
	defer func() {
		m.hashMu.Lock()
		defer m.hashMu.Unlock()
		for _, job := range m.hashJobs {
			job.cancel()
		}
	}()

	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	lastHealth := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-inspectorTick.C:
			m.flushAdapterCalls(ctx)
		case <-tick.C:
			m.runOnce(ctx, lastHealth)
		}
	}
}

// runOnce is one scheduler pass; tests drive it directly.
// stremioManifestRefresh is how often an installed Stremio add-on's
// manifest is re-read.
const stremioManifestRefresh = time.Hour

func (m *Manager) runOnce(ctx context.Context, lastHealth map[string]time.Time) {
	recs, err := m.store.list(ctx)
	if err != nil {
		return
	}
	now := m.now()
	m.refreshOnAir(ctx, recs, now)
	for _, r := range recs {
		if r.Enabled && r.Manifest.IsStremioOnly() && r.SourceKind == "url" && m.isLeader() {
			// Stremio add-ons change catalogs and resources without a
			// version bump; re-read the manifest hourly (cluster leader).
			key := "stremio-manifest|" + r.ID
			if last, seen := lastHealth[key]; !seen || now.Sub(last) >= stremioManifestRefresh {
				lastHealth[key] = now
				if seen {
					go func(id string) {
						rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
						defer cancel()
						_, _, _ = m.CheckUpdate(rctx, id)
					}(r.ID)
				}
			}
		}
		if !r.Enabled || r.Manifest.Ether == nil || m.sidecarWaiting(r) {
			continue
		}
		interval := time.Minute
		if d, err := time.ParseDuration(r.Manifest.Ether.Health.Interval); err == nil && d >= 15*time.Second {
			interval = d
		}
		if _, hasRuntime := m.factoryFor(r); hasRuntime && now.Sub(lastHealth[r.ID]) >= interval {
			lastHealth[r.ID] = now
			h, _ := m.store.health(ctx, r.ID)
			if h.NextRetry.IsZero() || !now.Before(h.NextRetry) || h.Status != "error" {
				_, _ = m.CheckHealth(ctx, r.ID)
			}
		}
		if !m.mayRunJobs(r) {
			continue
		}
		for _, job := range r.Manifest.Ether.Schedule {
			if job.Call == "/ether/feeds" {
				continue
			}
			every, err := time.ParseDuration(job.Every)
			if err != nil {
				continue
			}
			key := r.ID + "|" + job.Call
			m.mu.Lock()
			next, seen := m.nextRun[key]
			if !seen {
				last, err := m.store.scheduleLastRun(ctx, r.ID, job.Call)
				if err != nil {
					m.mu.Unlock()
					m.log.Warn("plugin schedule: read last run", "plugin", r.ID, "call", job.Call, "err", err)
					continue
				}
				if !last.IsZero() {
					next = last.Add(every)
					m.nextRun[key] = next
				}
			}
			if now.Before(next) {
				m.mu.Unlock()
				continue
			}
			if ctx.Err() != nil {
				m.mu.Unlock()
				return
			}
			if m.scheduleBusyLocked(r.ID, job.Call) {
				m.mu.Unlock()
				continue
			}
			// Record dispatch before calling: failures (or a restart during a
			// long job) must not produce a first-run storm on the next boot.
			dispatched := m.now()
			if err := m.store.markScheduleRun(ctx, r.ID, job.Call, dispatched); err != nil {
				m.mu.Unlock()
				m.log.Warn("plugin schedule: save last run", "plugin", r.ID, "call", job.Call, "err", err)
				continue
			}
			m.nextRun[key] = dispatched.Add(every)
			callCtx, cancel := context.WithCancel(ctx)
			flight := &scheduleFlight{cancel: cancel}
			if m.scheduleInFlight == nil {
				m.scheduleInFlight = map[string]*scheduleFlight{}
			}
			m.scheduleInFlight[key] = flight
			m.scheduleWG.Add(1)
			m.mu.Unlock()
			go m.runScheduledCall(callCtx, flight, r.ID, job.Call, key)
		}
		_ = m.DeliverOutbox(ctx, r.ID)
		// Capability refreshes ride the schedule: list sources and
		// channels declare their own refresh in contributes.
		m.maybeRefresh(ctx, r, now)
	}
}

func (m *Manager) factoryFor(r Record) (RuntimeFactory, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.fatories[r.Manifest.RuntimeKind()]
	return f, ok
}

// mayRunJobs applies cluster placement: leader plugins only on the
// leader, every_node everywhere.
func (m *Manager) mayRunJobs(r Record) bool {
	if r.Manifest.Ether.Cluster == "every_node" {
		return true
	}
	return m.isLeader()
}

func (m *Manager) maybeRefresh(ctx context.Context, r Record, now time.Time) {
	type refresh struct {
		key   string
		every time.Duration
		path  string
	}
	var jobs []refresh
	c := r.Manifest.Ether.Contributes
	if c.Channels != nil && c.Channels.Refresh != "" {
		if d, err := time.ParseDuration(c.Channels.Refresh); err == nil {
			jobs = append(jobs, refresh{"channels", d, "/ether/channels.json"})
		}
	}
	for _, ls := range c.ListSources {
		refreshStr := ls.Refresh
		if refreshStr == "" {
			refreshStr = "1h"
		}
		if d, err := time.ParseDuration(refreshStr); err == nil {
			jobs = append(jobs, refresh{"list:" + ls.Key, d, "/catalog/movie/" + ls.Catalog + ".json"})
		}
	}
	for _, j := range jobs {
		key := r.ID + "|refresh:" + j.key
		m.mu.Lock()
		next, seen := m.nextRun[key]
		if !seen {
			m.nextRun[key] = now.Add(j.every)
			m.mu.Unlock()
			continue
		}
		if now.Before(next) {
			m.mu.Unlock()
			continue
		}
		m.nextRun[key] = now.Add(j.every)
		m.mu.Unlock()
		_, _ = m.Call(ctx, r.ID, Request{Method: "GET", Path: j.path})
	}
}
