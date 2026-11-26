package screens

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/screendoc"
)

// Data reads (docs/plugin-screens.md, API: Data). One read resolves the
// requested sources of one page or surface in parallel, for one viewer.

// ErrInvalidParams reports a page parameter or context value that fails its
// declaration, an unknown source, or an unknown parameter.
var ErrInvalidParams = errors.New("screens: invalid parameters")

// RecordEntry is one shared record with Ether's revision.
type RecordEntry struct {
	Rev   int64 `json:"rev"`
	Value any   `json:"value"`
}

// SourceError is why one source has no value.
type SourceError struct {
	Code  string `json:"code"`
	Retry bool   `json:"retry,omitempty"`
}

// LiveRef carries the sealed topic a client opens the stream with.
type LiveRef struct {
	Topic string `json:"topic"`
}

// DataResponse is the answer of a data read.
type DataResponse struct {
	Rev     int64                  `json:"rev"`
	Sources map[string]any         `json:"sources"`
	Records map[string]RecordEntry `json:"records"`
	Errors  map[string]SourceError `json:"errors,omitempty"`
	Live    *LiveRef               `json:"live,omitempty"`

	// refresh is the soonest re-read a plugin source asked for.
	refresh time.Duration
}

// DataRequest asks for one page's or surface's sources.
type DataRequest struct {
	Kind    string // "screen" or "surface"
	ID      string // wire id
	Page    string // screens only
	Sources []string
	// Params are the raw p.* values (page params, or surface context).
	Params map[string]string
	TZ     string
	// All reads every source, lazy ones included (an action's answer).
	All bool
	// Images signs image references for the client; nil drops every image,
	// so no upstream URL can ever reach a client.
	Images ImageSigner
	ClientInfo
}

// ImageSigner turns a plugin-named image URL or a channel into a URL the
// client may load; "" drops the image.
type ImageSigner interface {
	Plugin(pluginID, url string) string
	Channel(channelID string) string
}

// LiveDetails is the optional part of the live source the data reads use
// beyond LiveFeedHub: one event's detail and the viewer's spoiler choice.
type LiveDetails interface {
	Detail(ctx context.Context, viewer *core.User, eventID string) (*core.LiveEventDetail, error)
	SpoilerFree(ctx context.Context, viewer *core.User) (bool, error)
}

// Per-source budgets on a data read. A source that misses its budget answers
// a retryable timeout; its late result still lands in the hub's cache.
const (
	builtinSourceBudget = 2500 * time.Millisecond
	pluginSourceBudget  = 4 * time.Second
	maxSourceBytes      = screendoc.MaxDocumentBytes
	maxResponseBytes    = 1 << 20
)

// target is a resolved page or surface ready to read.
type target struct {
	kind         string
	wireID       string
	localID      string
	page         string
	actions      map[string]plugins.ScreenAction
	plugin       *docPlugin
	doc          *screendoc.Compiled
	presentation string
}

func (s *Service) findTarget(ctx context.Context, viewer *core.User, kind, id, page string, c ClientInfo) (target, error) {
	if s.docs == nil {
		return target{}, core.ErrNotFound
	}
	switch kind {
	case "screen":
		t, ok := s.docs.FindScreen(ctx, viewer, c.Frontend, id)
		if !ok {
			return target{}, core.ErrNotFound
		}
		variants, ok := t.plugin.docs.Screens[t.Meta.ID][page]
		if !ok {
			return target{}, core.ErrNotFound
		}
		d := pickVariant(variants, c.Renderer)
		if d == nil {
			return target{}, ErrNoVariant
		}
		pres := screendoc.PresentPage
		if page == "index" {
			pres = screendoc.PresentSection
		}
		return target{kind: kind, wireID: t.WireID, localID: t.Meta.ID, page: page, plugin: t.plugin, doc: d, presentation: pres, actions: t.Meta.Actions}, nil
	case "surface":
		t, ok := s.docs.FindSurface(ctx, viewer, c.Frontend, id)
		if !ok {
			return target{}, core.ErrNotFound
		}
		d := pickVariant(t.plugin.docs.Surfaces[t.Meta.ID], c.Renderer)
		if d == nil {
			return target{}, ErrNoVariant
		}
		return target{kind: kind, wireID: t.WireID, localID: t.Meta.ID, plugin: t.plugin, doc: d, presentation: t.Meta.Presentation}, nil
	}
	return target{}, core.ErrNotFound
}

// bindings are the hole values of one read: "param.x", "context.x",
// "viewer.tz".
type bindings map[string]string

// bind validates raw p.* values against the target's declarations.
func bind(t target, raw map[string]string, tz string) (bindings, error) {
	b := bindings{"viewer.tz": tz}
	contexts := map[string]screendoc.ContextParam{}
	for _, cp := range screendoc.SurfaceContext(t.presentation) {
		contexts[cp.Name] = cp
	}
	for name, v := range raw {
		if p, ok := t.doc.Params[name]; ok {
			cv, err := screendoc.CheckParam(p, v)
			if err != nil {
				return nil, fmt.Errorf("%w: p.%s %v", ErrInvalidParams, name, err)
			}
			b["param."+name] = cv
			continue
		}
		if cp, ok := contexts[name]; ok {
			if cp.Ref != "" && v != "" && !strings.HasPrefix(v, cp.Ref+":") {
				return nil, fmt.Errorf("%w: p.%s must be a %s: record key", ErrInvalidParams, name, cp.Ref)
			}
			if len(v) > 400 {
				return nil, fmt.Errorf("%w: p.%s is too long", ErrInvalidParams, name)
			}
			b["context."+name] = v
			continue
		}
		return nil, fmt.Errorf("%w: unknown parameter p.%s", ErrInvalidParams, name)
	}
	for name, p := range t.doc.Params {
		if _, ok := b["param."+name]; !ok {
			b["param."+name] = p.Default
		}
	}
	return b, nil
}

// fill substitutes holes in a source argument.
func (b bindings) fill(s string) string {
	if !strings.Contains(s, "{") {
		return s
	}
	for k, v := range b {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

func (b bindings) text(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(b.fill(v))
}

// readCtx is the shared state of one data read.
type readCtx struct {
	ctx    context.Context
	s      *Service
	viewer *core.User
	t      target
	b      bindings
	loc    *time.Location
	now    time.Time
	favs   *favoriteSet
	images ImageSigner
	// clientLocale is the request's locale, handed to plugin calls.
	clientLocale string

	mu      sync.Mutex
	records map[string]any
	events  map[string]*eventFetch
	// refresh is the soonest re-read a plugin source asked for while
	// someone watches (0: none).
	refresh time.Duration
}

type eventFetch struct {
	done   chan struct{}
	events []core.LiveEvent
	err    error
}

// img signs a plugin-named image of this read's plugin.
func (rc *readCtx) img(raw string) string {
	if raw == "" || rc.images == nil {
		return ""
	}
	return rc.images.Plugin(rc.t.plugin.pluginID, raw)
}

func (rc *readCtx) signEvent(v *EventView) {
	v.League.Logo = rc.img(v.League.Logo)
	for i := range v.Sides {
		v.Sides[i].Logo = rc.img(v.Sides[i].Logo)
	}
	for i := range v.Watch.Targets {
		v.Watch.Targets[i].Logo = rc.channelLogo(v.Watch.Targets[i])
	}
	if v.Watch.Primary != nil {
		p := *v.Watch.Primary
		p.Logo = rc.channelLogo(p)
		v.Watch.Primary = &p
	}
}

func (rc *readCtx) channelLogo(t WatchTargetView) string {
	if t.Logo == "" || t.ChannelID == "" || rc.images == nil {
		return ""
	}
	return rc.images.Channel(t.ChannelID)
}

func (rc *readCtx) signLeagues(ls []LeagueView) []LeagueView {
	for i := range ls {
		ls[i].Logo = rc.img(ls[i].Logo)
	}
	return ls
}

func (rc *readCtx) addRecord(key string, value any) {
	rc.mu.Lock()
	// A detail source may have an already enriched event for this key.
	if _, staged := value.(eventRecord); !staged {
		rc.records[key] = value
	} else if _, exists := rc.records[key]; !exists {
		rc.records[key] = value
	}
	rc.mu.Unlock()
}

// Authorize checks that viewer may use the page or surface req names, with
// valid parameters, without reading any source.
func (s *Service) Authorize(ctx context.Context, viewer *core.User, req DataRequest) error {
	t, err := s.findTarget(ctx, viewer, req.Kind, req.ID, req.Page, req.ClientInfo)
	if err != nil {
		return err
	}
	_, err = bind(t, req.Params, req.TZ)
	return err
}

// Data resolves a data read.
func (s *Service) Data(ctx context.Context, viewer *core.User, req DataRequest) (DataResponse, error) {
	t, err := s.findTarget(ctx, viewer, req.Kind, req.ID, req.Page, req.ClientInfo)
	if err != nil {
		return DataResponse{}, err
	}
	loc := time.UTC
	if req.TZ != "" {
		if l, err := time.LoadLocation(req.TZ); err == nil && req.TZ != "Local" {
			loc = l
		} else {
			return DataResponse{}, fmt.Errorf("%w: tz must be an IANA time zone", ErrInvalidParams)
		}
	}
	b, err := bind(t, req.Params, loc.String())
	if err != nil {
		return DataResponse{}, err
	}
	ids := req.Sources
	if req.All {
		ids = t.doc.SourceIDs()
	} else if len(ids) == 0 {
		for _, id := range t.doc.SourceIDs() {
			if !t.doc.Sources[id].Lazy {
				ids = append(ids, id)
			}
		}
	}
	for _, id := range ids {
		if _, ok := t.doc.Sources[id]; !ok {
			return DataResponse{}, fmt.Errorf("%w: unknown source %s", ErrInvalidParams, id)
		}
	}
	favs, err := s.loadFavorites(ctx, viewer)
	if err != nil {
		return DataResponse{}, err
	}
	rc := &readCtx{ctx: ctx, s: s, viewer: viewer, t: t, b: b, loc: loc, now: s.now(), favs: favs, images: req.Images, clientLocale: req.Locale,
		records: map[string]any{}, events: map[string]*eventFetch{}}
	out := DataResponse{Sources: map[string]any{}, Records: map[string]RecordEntry{}}
	type result struct {
		id    string
		value any
		err   error
	}
	results := make(chan result, len(ids))
	for _, id := range ids {
		go func(src screendoc.Source) {
			budget := builtinSourceBudget
			if src.From == screendoc.FromPlugin {
				budget = pluginSourceBudget
			}
			sctx, cancel := context.WithTimeout(ctx, budget)
			defer cancel()
			done := make(chan result, 1)
			go func() {
				v, err := rc.resolve(sctx, src)
				done <- result{src.ID, v, err}
			}()
			select {
			case r := <-done:
				// A source that failed because its budget ran out (an
				// interrupted query, a cancelled plugin call) is a timeout,
				// not an outage: the hub keeps the late answer for the retry.
				if r.err != nil && sctx.Err() != nil && ctx.Err() == nil {
					r.err = context.DeadlineExceeded
				}
				results <- r
			case <-sctx.Done():
				results <- result{src.ID, nil, context.DeadlineExceeded}
			}
		}(t.doc.Sources[id])
	}
	for range ids {
		r := <-results
		if r.err != nil {
			out.Errors = setErr(out.Errors, r.id, r.err)
			if out.Errors[r.id].Code == "unavailable" {
				slog.Warn("screen source unavailable", "plugin", t.plugin.pluginID, "source", r.id, "from", t.doc.Sources[r.id].From, "err", r.err)
			}
			continue
		}
		raw, _ := json.Marshal(r.value)
		if len(raw) > maxSourceBytes {
			out.Errors = setErr(out.Errors, r.id, errSourceTooLarge)
			continue
		}
		out.Sources[r.id] = r.value
	}
	rc.mu.Lock()
	recs := maps.Clone(rc.records)
	out.refresh = rc.refresh
	rc.mu.Unlock()
	rc.resolveEventRecords(ctx, recs)
	out.Rev = s.revs.stamp(recs, out.Records)
	for id, v := range out.Sources {
		out.Rev = max(out.Rev, s.revs.stampSource(t.plugin.pluginID+"\x00"+t.wireID+"\x00"+t.page+"\x00"+id+"\x00"+canonicalBindings(b, t.doc.Sources[id].Deps), v))
	}
	if raw, _ := json.Marshal(out); len(raw) > maxResponseBytes {
		return DataResponse{}, errResponseTooLarge
	}
	if topic, ok := s.sealTopic(viewer, req, t, b); ok {
		out.Live = &LiveRef{Topic: topic}
	}
	return out, nil
}

var (
	errSourceTooLarge   = errors.New("screens: source answer too large")
	errResponseTooLarge = errors.New("screens: data response too large")
	errUnsupported      = errors.New("screens: source type not available")
)

func setErr(m map[string]SourceError, id string, err error) map[string]SourceError {
	if m == nil {
		m = map[string]SourceError{}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		m[id] = SourceError{Code: "timeout", Retry: true}
	case errors.Is(err, core.ErrNotFound):
		m[id] = SourceError{Code: "not_found"}
	case errors.Is(err, errSourceTooLarge):
		m[id] = SourceError{Code: "too_large"}
	case errors.Is(err, errUnsupported):
		m[id] = SourceError{Code: "unsupported"}
	case errors.Is(err, ErrInvalidParams):
		m[id] = SourceError{Code: "invalid"}
	default:
		m[id] = SourceError{Code: "unavailable", Retry: true}
	}
	return m
}

func canonicalBindings(b bindings, deps []string) string {
	parts := make([]string, 0, len(deps)+1)
	for _, d := range deps {
		parts = append(parts, d+"="+b[d])
	}
	parts = append(parts, "tz="+b["viewer.tz"])
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

func (rc *readCtx) resolve(ctx context.Context, src screendoc.Source) (any, error) {
	switch src.From {
	case screendoc.FromLiveEvents:
		return rc.liveEvents(ctx, src)
	case screendoc.FromLiveSchedule:
		return rc.liveSchedule(ctx, src)
	case screendoc.FromLiveEvent:
		return rc.liveEvent(ctx, src)
	case screendoc.FromLiveTables:
		return rc.liveTables(ctx, src)
	case screendoc.FromLiveEntities:
		return rc.liveEntities(ctx, src)
	case screendoc.FromLiveLeagues:
		return rc.liveLeagues(ctx, src)
	case screendoc.FromWatch:
		return rc.watch(ctx, src)
	case screendoc.FromViewer:
		return rc.viewerSource(ctx)
	case screendoc.FromPlugin:
		return rc.pluginSource(ctx, src)
	case screendoc.FromItems:
		return rc.itemsSource(ctx, src)
	case screendoc.FromChannels:
		return rc.channelsSource(ctx, src)
	}
	if rc.s.extra != nil {
		return rc.s.extra.Resolve(ctx, rc.viewer, ExtraSource{PluginID: rc.t.plugin.pluginID, From: src.From, Args: src.Args, Fill: rc.b.fill, Record: rc.addRecord})
	}
	return nil, errUnsupported
}

// ExtraSource is a source type resolved outside this package (plugin, items,
// liveItems, channels): its arguments, a hole filler, and a way to add
// records to the response.
type ExtraSource struct {
	PluginID string
	From     string
	Args     map[string]any
	Fill     func(string) string
	Record   func(key string, value any)
}

// ExtraSources resolves the source types the app owns.
type ExtraSources interface {
	Resolve(ctx context.Context, viewer *core.User, src ExtraSource) (any, error)
}

func recordRef(key string) map[string]string { return map[string]string{"$record": key} }

func (rc *readCtx) entityKind(feed string) string {
	for _, f := range rc.t.plugin.contrib.LiveFeeds {
		if f.ID == feed && len(f.EntityKinds) > 0 && strings.TrimSpace(f.EntityKinds[0]) != "" {
			return strings.TrimSpace(f.EntityKinds[0])
		}
	}
	return "team"
}

// dayStart is the start of the viewer's local day containing t.
func (rc *readCtx) dayStart(t time.Time) time.Time {
	l := t.In(rc.loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, rc.loc)
}

// fetchEvents reads one feed's events between from and to. It asks the hub
// one whole UTC day at a time, so every viewer in every time zone (and the
// hub's own polling) shares the same cached queries; the caller filters to
// the local window. Days are fetched in parallel and shared within the read.
func (rc *readCtx) fetchEvents(ctx context.Context, feed, league string, from, to time.Time) ([]core.LiveEvent, error) {
	hub := rc.s.hubDeps()
	if hub == nil {
		return nil, core.ErrNotAvailable
	}
	// One or two days: one query per UTC day, shared with every time zone
	// and the hub's own polling. Longer windows (a week, the schedule
	// strip): one query for the whole UTC-aligned range, so a cold read is
	// one plugin call per league rather than one per league per day.
	type span struct{ from, to time.Time }
	var spans []span
	start, end := startOfDayUTC(from), startOfDayUTC(to.Add(-time.Nanosecond)).Add(24*time.Hour)
	if end.Sub(start) <= 48*time.Hour {
		for d := start; d.Before(end); d = d.Add(24 * time.Hour) {
			spans = append(spans, span{d, d.Add(24 * time.Hour)})
		}
	} else {
		if end.Sub(start) > 15*24*time.Hour {
			end = start.Add(15 * 24 * time.Hour)
		}
		spans = append(spans, span{start, end})
	}
	fetches := make([]*eventFetch, len(spans))
	for i, sp := range spans {
		d, dEnd := sp.from, sp.to
		key := feed + "\x00" + league + "\x00" + d.Format("2006-01-02") + "\x00" + dEnd.Format("2006-01-02")
		rc.mu.Lock()
		ef, ok := rc.events[key]
		if !ok {
			ef = &eventFetch{done: make(chan struct{})}
			rc.events[key] = ef
		}
		rc.mu.Unlock()
		if !ok {
			go func(ef *eventFetch, d, dEnd time.Time) {
				ef.events, ef.err = hub.Events(rc.ctx, rc.viewer, EventQuery{PluginID: rc.t.plugin.pluginID, Feed: feed, From: d, To: dEnd, League: league})
				close(ef.done)
			}(ef, d, dEnd)
		}
		fetches[i] = ef
	}
	seen := map[string]bool{}
	var out []core.LiveEvent
	var firstErr error
	for _, ef := range fetches {
		select {
		case <-ef.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if ef.err != nil {
			if firstErr == nil {
				firstErr = ef.err
			}
			continue
		}
		for _, ev := range ef.events {
			if !seen[ev.ID] {
				seen[ev.ID] = true
				out = append(out, ev)
			}
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// window gives the local window for a query.
func (rc *readCtx) window(window string, states []string) (time.Time, time.Time) {
	day := rc.dayStart(rc.now)
	switch window {
	case "week":
		includePast := len(states) == 0 || containsString(states, core.LiveEventStatePost)
		if includePast {
			return day.AddDate(0, 0, -1), day.AddDate(0, 0, 7)
		}
		return day, day.AddDate(0, 0, 7)
	case "recent":
		return rc.now.Add(-72 * time.Hour), rc.now.Add(time.Minute)
	default:
		return day, day.AddDate(0, 0, 1)
	}
}

type eventQueryArgs struct {
	states    []string
	league    string
	favorites bool
	window    string
	category  string
	date      string
	ids       []string
}

func (rc *readCtx) queryArgs(args map[string]any) eventQueryArgs {
	q := eventQueryArgs{}
	m, _ := args["query"].(map[string]any)
	if m == nil {
		return q
	}
	if list, ok := m["state"].([]any); ok {
		for _, s := range list {
			if v, ok := s.(string); ok {
				q.states = append(q.states, v)
			}
		}
	}
	q.league = rc.b.text(m, "league")
	q.window = rc.b.text(m, "window")
	q.category = rc.b.text(m, "category")
	q.date = rc.b.text(m, "date")
	switch v := m["favorites"].(type) {
	case bool:
		q.favorites = v
	case string:
		q.favorites = rc.b.fill(v) == "true"
	}
	if list, ok := m["ids"].([]any); ok {
		for _, s := range list {
			if v, ok := s.(string); ok {
				if id := strings.TrimPrefix(rc.b.fill(v), "event:"); id != "" {
					q.ids = append(q.ids, id)
				}
			}
		}
	}
	return q
}

// keep reports whether ev passes a query within [from, to).
func (rc *readCtx) keep(ev core.LiveEvent, feed, kind string, q eventQueryArgs, from, to time.Time, liveAcrossMidnight bool) bool {
	if q.league != "" && !strings.EqualFold(ev.League.ID, q.league) {
		return false
	}
	if len(q.states) > 0 && !containsString(q.states, ev.Status.State) {
		return false
	}
	if q.category != "" && !strings.EqualFold(ev.Category, q.category) {
		return false
	}
	if q.favorites && !rc.favs.matchesEvent(rc.t.plugin.pluginID, feed, kind, ev) {
		return false
	}
	if len(q.ids) > 0 && !containsString(q.ids, ev.ID) {
		return false
	}
	if liveAcrossMidnight && ev.Status.State == core.LiveEventStateIn {
		return true
	}
	start := parseEventStart(ev.Start)
	if start.IsZero() {
		return true
	}
	return !start.Before(from) && start.Before(to)
}

type eventRecord struct {
	event core.LiveEvent
	kind  string
}

// eventRecords stages the displayed events. Overlapping page sources (hero,
// live list, schedule) share one record and one watch lookup by event ID.
func (rc *readCtx) eventRecords(events []core.LiveEvent, kind string) []map[string]string {
	refs := make([]map[string]string, 0, len(events))
	for _, ev := range events {
		key := "event:" + ev.ID
		rc.addRecord(key, eventRecord{event: ev, kind: kind})
		refs = append(refs, recordRef(key))
	}
	return refs
}

func (rc *readCtx) resolveEventRecords(ctx context.Context, records map[string]any) {
	var keys []string
	for key, value := range records {
		if _, ok := value.(eventRecord); ok {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return
	}
	sort.Strings(keys)
	events := make([]core.LiveEvent, len(keys))
	for i, key := range keys {
		events[i] = records[key].(eventRecord).event
	}
	ctx, cancel := context.WithTimeout(ctx, builtinSourceBudget)
	defer cancel()
	rc.enrichWatch(ctx, events)
	for i, key := range keys {
		v := buildEventView(events[i], rc.favs, records[key].(eventRecord).kind)
		rc.signEvent(&v)
		records[key] = v
	}
}

func (rc *readCtx) enrichWatch(ctx context.Context, events []core.LiveEvent) {
	_, watch := rc.s.deps()
	if watch == nil || len(events) == 0 {
		return
	}
	if batch, ok := watch.(BatchWatchTargetResolver); ok {
		got, err := batch.WatchTargetsBatch(ctx, rc.viewer, events)
		if err != nil {
			return
		}
		for i := range events {
			if w, ok := got[events[i].ID]; ok {
				events[i].Watch = w
			}
		}
		return
	}
	for i := range events {
		if w, err := watch.WatchTargets(ctx, rc.viewer, events[i]); err == nil {
			events[i].Watch = w
		}
	}
}

func (rc *readCtx) liveEvents(ctx context.Context, src screendoc.Source) (any, error) {
	feed, _ := src.Args["feed"].(string)
	kind := rc.entityKind(feed)
	q := rc.queryArgs(src.Args)
	if q.favorites && !rc.favs.hasAnyForFeed(rc.t.plugin.pluginID, feed) {
		return map[string]any{"events": []any{}}, nil
	}
	window := q.window
	if window == "" {
		window = "today"
	}
	from, to := rc.window(window, q.states)
	ticker := window == "ticker"
	if ticker {
		// In progress, finals that started under 30 h ago, and events
		// starting within 6 h: what a live ticker runs through.
		from, to = rc.now.Add(-30*time.Hour), rc.now.Add(6*time.Hour)
	}
	if q.date != "" {
		d, err := time.ParseInLocation("2006-01-02", q.date, rc.loc)
		if err != nil {
			return nil, fmt.Errorf("%w: date", ErrInvalidParams)
		}
		from, to = d, d.AddDate(0, 0, 1)
	}
	raw, err := rc.fetchEvents(ctx, feed, q.league, from, to)
	if err != nil {
		return nil, err
	}
	exclude := strings.TrimPrefix(rc.b.text(src.Args, "exclude"), "event:")
	var events []core.LiveEvent
	for _, ev := range raw {
		if ev.ID == exclude {
			continue
		}
		if !rc.keep(ev, feed, kind, q, from, to, q.date == "" && (window == "today" || window == "week" || ticker)) {
			continue
		}
		if ticker && !tickerMember(ev, rc.now) {
			continue
		}
		events = append(events, ev)
	}
	sortKey, _ := src.Args["sort"].(string)
	fav := func(ev core.LiveEvent) bool { return rc.favs.matchesEvent(rc.t.plugin.pluginID, feed, kind, ev) }
	switch {
	case sortKey == "featured" || ticker:
		sort.SliceStable(events, func(i, j int) bool { return featuredBefore(events[i], events[j], fav) })
	case window == "recent" || (len(q.states) == 1 && q.states[0] == core.LiveEventStatePost):
		sort.SliceStable(events, func(i, j int) bool {
			a, b := parseEventStart(events[i].Start), parseEventStart(events[j].Start)
			if !a.Equal(b) {
				return a.After(b)
			}
			return events[i].ID < events[j].ID
		})
	case sortKey == "start":
		sort.SliceStable(events, func(i, j int) bool {
			a, b := parseEventStart(events[i].Start), parseEventStart(events[j].Start)
			if !a.Equal(b) {
				return a.Before(b)
			}
			return events[i].ID < events[j].ID
		})
	default:
		sort.SliceStable(events, func(i, j int) bool { return rowBefore(events[i], events[j], fav) })
	}
	limit := 100
	if n, ok := src.Args["limit"].(int); ok && n > 0 {
		limit = n
	}
	if len(events) > limit {
		events = events[:limit]
	}
	return map[string]any{"events": rc.eventRecords(events, kind)}, nil
}

// tickerMember keeps what a ticker shows: events in progress, finals that
// started under 30 h ago, and upcoming events starting within 6 h.
func tickerMember(ev core.LiveEvent, now time.Time) bool {
	start := parseEventStart(ev.Start)
	switch ev.Status.State {
	case core.LiveEventStateIn:
		return true
	case core.LiveEventStatePost:
		return !start.IsZero() && now.Sub(start) < 30*time.Hour
	case core.LiveEventStatePre, core.LiveEventStateDelayed:
		return !start.IsZero() && start.Sub(now) <= 6*time.Hour && start.After(now.Add(-6*time.Hour))
	}
	return false
}

// featuredBefore is the generic featured order: a favourite in progress,
// then events in progress by the plugin's prominence (close games first when
// scores are shown), then upcoming soonest, then finals most recent.
func featuredBefore(a, b core.LiveEvent, fav func(core.LiveEvent) bool) bool {
	ta, tb := featuredTier(a, fav), featuredTier(b, fav)
	if ta != tb {
		return ta < tb
	}
	if a.Prominence != b.Prominence {
		return a.Prominence > b.Prominence
	}
	if ra, rb := bestCompetitorRank(a), bestCompetitorRank(b); ra != rb {
		return ra < rb
	}
	sa, sb := parseEventStart(a.Start), parseEventStart(b.Start)
	if a.Status.State == core.LiveEventStatePost && b.Status.State == core.LiveEventStatePost {
		if !sa.Equal(sb) {
			return sa.After(sb)
		}
	} else if !sa.Equal(sb) {
		return sa.Before(sb)
	}
	return a.ID < b.ID
}

func featuredTier(ev core.LiveEvent, fav func(core.LiveEvent) bool) int {
	f := fav(ev)
	switch ev.Status.State {
	case core.LiveEventStateIn:
		if f {
			return 0
		}
		if !ev.Spoiler && ev.Status.Close {
			return 1
		}
		return 2
	case core.LiveEventStatePre, core.LiveEventStateDelayed:
		if f {
			return 3
		}
		return 4
	case core.LiveEventStatePost:
		if f {
			return 5
		}
		return 6
	}
	if f {
		return 7
	}
	return 8
}

func rowBefore(a, b core.LiveEvent, fav func(core.LiveEvent) bool) bool {
	sa, sb := stateOrder(a.Status.State), stateOrder(b.Status.State)
	if sa != sb {
		return sa < sb
	}
	if fa, fb := fav(a), fav(b); fa != fb {
		return fa
	}
	if a.Status.State == core.LiveEventStateIn && !a.Spoiler && !b.Spoiler && a.Status.Close != b.Status.Close {
		return a.Status.Close
	}
	ta, tb := parseEventStart(a.Start), parseEventStart(b.Start)
	if a.Status.State == core.LiveEventStatePost {
		if !ta.Equal(tb) {
			return ta.After(tb)
		}
	} else if !ta.Equal(tb) {
		return ta.Before(tb)
	}
	return a.ID < b.ID
}

// ScheduleDayView is one day of the schedule strip.
type ScheduleDayView struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

func (rc *readCtx) liveSchedule(ctx context.Context, src screendoc.Source) (any, error) {
	feed, _ := src.Args["feed"].(string)
	kind := rc.entityKind(feed)
	league := rc.b.text(src.Args, "league")
	day := rc.dayStart(rc.now)
	if d := rc.b.text(src.Args, "date"); d != "" {
		parsed, err := time.ParseInLocation("2006-01-02", d, rc.loc)
		if err != nil {
			return nil, fmt.Errorf("%w: date", ErrInvalidParams)
		}
		day = parsed
	}
	// One hub query covers the whole strip, so every day's count is real.
	stripFrom, stripTo := day.AddDate(0, 0, -2), day.AddDate(0, 0, 5)
	raw, err := rc.fetchEvents(ctx, feed, league, stripFrom, stripTo)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	var events []core.LiveEvent
	q := eventQueryArgs{league: league}
	for _, ev := range raw {
		if !rc.keep(ev, feed, kind, q, stripFrom, stripTo, false) {
			continue
		}
		start := parseEventStart(ev.Start)
		if start.IsZero() {
			continue
		}
		local := start.In(rc.loc).Format("2006-01-02")
		counts[local]++
		if local == day.Format("2006-01-02") {
			events = append(events, ev)
		}
	}
	sort.SliceStable(events, func(i, j int) bool {
		a, b := parseEventStart(events[i].Start), parseEventStart(events[j].Start)
		if !a.Equal(b) {
			return a.Before(b)
		}
		return events[i].ID < events[j].ID
	})
	days := make([]ScheduleDayView, 0, 7)
	for off := -2; off <= 4; off++ {
		d := day.AddDate(0, 0, off).Format("2006-01-02")
		days = append(days, ScheduleDayView{Date: d, Count: counts[d]})
	}
	return map[string]any{
		"date":     day.Format("2006-01-02"),
		"previous": day.AddDate(0, 0, -1).Format("2006-01-02"),
		"next":     day.AddDate(0, 0, 1).Format("2006-01-02"),
		"days":     days,
		"events":   rc.eventRecords(events, kind),
	}, nil
}

func (rc *readCtx) liveEvent(ctx context.Context, src screendoc.Source) (any, error) {
	feed, _ := src.Args["feed"].(string)
	id := strings.TrimPrefix(rc.b.text(src.Args, "id"), "event:")
	if id == "" {
		return nil, core.ErrNotFound
	}
	if p, f, _ := splitEventID(id); p != rc.t.plugin.pluginID || f != feed {
		return nil, core.ErrNotFound
	}
	live, ok := rc.s.hubDeps().(LiveDetails)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	d, err := live.Detail(ctx, rc.viewer, id)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, core.ErrNotFound
	}
	kind := rc.entityKind(feed)
	events := []core.LiveEvent{d.LiveEvent}
	if len(d.Watch) == 0 {
		rc.enrichWatch(ctx, events)
	}
	ev := buildEventView(events[0], rc.favs, kind)
	rc.signEvent(&ev)
	rc.addRecord("event:"+id, ev)
	rc.addRecord("detail:"+id, buildDetailView(*d, rc.favs, kind))
	return map[string]any{"event": recordRef("event:" + id), "detail": recordRef("detail:" + id)}, nil
}

func splitEventID(id string) (string, string, string) {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) != 3 {
		return "", "", ""
	}
	return parts[0], parts[1], parts[2]
}

func (rc *readCtx) liveTables(ctx context.Context, src screendoc.Source) (any, error) {
	feed, _ := src.Args["feed"].(string)
	league := rc.b.text(src.Args, "league")
	hub := rc.s.hubDeps()
	if hub == nil {
		return nil, core.ErrNotAvailable
	}
	tables, err := hub.Standings(ctx, rc.viewer, rc.t.plugin.pluginID, feed, league)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return nil, err
	}
	kind := rc.entityKind(feed)
	out := make([]TableView, 0, len(tables))
	for _, t := range tables {
		out = append(out, tableView(t, rc.favs, rc.t.plugin.pluginID, feed, kind))
	}
	res := map[string]any{"tables": out}
	if league != "" {
		if lg, ok := rc.leagueByID(ctx, feed, league); ok {
			res["league"] = map[string]string{"id": lg.ID, "name": lg.Name, "label": lg.Label}
		} else {
			res["league"] = map[string]string{"id": league, "name": strings.ToUpper(league), "label": strings.ToUpper(league)}
		}
	}
	return res, nil
}

// todayLeagues lists the leagues on the feed's slate today, in first-seen
// order, from one shared hub query.
func (rc *readCtx) todayLeagues(ctx context.Context, feed string) ([]LeagueView, []core.LiveEvent, error) {
	from, to := rc.window("today", nil)
	raw, err := rc.fetchEvents(ctx, feed, "", from, to)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	var out []LeagueView
	for _, ev := range raw {
		id := strings.TrimSpace(ev.League.ID)
		if id == "" || seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		out = append(out, league(ev.League))
	}
	return out, raw, nil
}

func (rc *readCtx) leagueByID(ctx context.Context, feed, id string) (LeagueView, bool) {
	leagues, _, err := rc.todayLeagues(ctx, feed)
	if err != nil {
		return LeagueView{}, false
	}
	for _, l := range leagues {
		if strings.EqualFold(l.ID, id) {
			return l, true
		}
	}
	return LeagueView{}, false
}

func (rc *readCtx) liveLeagues(ctx context.Context, src screendoc.Source) (any, error) {
	feed, _ := src.Args["feed"].(string)
	leagues, _, err := rc.todayLeagues(ctx, feed)
	if err != nil {
		return nil, err
	}
	if leagues == nil {
		leagues = []LeagueView{}
	}
	return map[string]any{"leagues": rc.signLeagues(leagues), "selected": ""}, nil
}

func (rc *readCtx) liveEntities(ctx context.Context, src screendoc.Source) (any, error) {
	feed, _ := src.Args["feed"].(string)
	kind := rc.b.text(src.Args, "kind")
	if kind == "" {
		kind = rc.entityKind(feed)
	}
	hub := rc.s.hubDeps()
	if hub == nil {
		return nil, core.ErrNotAvailable
	}
	pluginID := rc.t.plugin.pluginID
	leagues, today, err := rc.todayLeagues(ctx, feed)
	if err != nil {
		leagues = nil
	}
	// Favourites the viewer follows in this feed, fetched by id (bounded).
	favIDs := rc.favs.localIDs(pluginID, feed, kind)
	if len(favIDs) > 100 {
		favIDs = favIDs[:100]
	}
	var favRows []core.LiveEventEntity
	if len(favIDs) > 0 {
		favRows, _ = hub.Entities(ctx, rc.viewer, EntityQuery{PluginID: pluginID, Feed: feed, Kind: kind, IDs: favIDs})
	}
	known := map[string]bool{}
	for _, l := range leagues {
		known[strings.ToLower(l.ID)] = true
	}
	for _, e := range favRows {
		if id := strings.TrimSpace(e.League); id != "" && !known[strings.ToLower(id)] {
			known[strings.ToLower(id)] = true
			leagues = append(leagues, LeagueView{ID: id, Name: strings.ToUpper(id), Label: strings.ToUpper(id)})
		}
	}
	opening := rc.b.text(src.Args, "league")
	if opening == "" {
		opening = rc.b.text(src.Args, "prefer")
	}
	if opening == "" {
		for _, e := range favRows {
			if e.League != "" {
				opening = e.League
				break
			}
		}
	}
	if opening == "" {
		for _, ev := range today {
			if len(ev.Competitors) > 0 && ev.League.ID != "" {
				opening = ev.League.ID
				break
			}
		}
	}
	if opening == "" && len(leagues) > 0 {
		opening = leagues[0].ID
	}
	var rows []core.LiveEventEntity
	if opening != "" {
		rows, err = hub.Entities(ctx, rc.viewer, EntityQuery{PluginID: pluginID, Feed: feed, Kind: kind, League: opening, Query: rc.b.text(src.Args, "q")})
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			return nil, err
		}
	}
	seen := map[string]bool{}
	var merged []core.LiveEventEntity
	for _, e := range append(favRows, rows...) {
		if e.ID == "" || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		if e.Kind == "" {
			e.Kind = kind
		}
		merged = append(merged, e)
	}
	views := make([]EntityView, 0, len(merged))
	for _, e := range merged {
		views = append(views, entityView(e, pluginID, feed, rc.favs))
	}
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].Favourite != views[j].Favourite {
			return views[i].Favourite
		}
		return strings.ToLower(views[i].Name) < strings.ToLower(views[j].Name)
	})
	refs := make([]map[string]string, 0, len(views))
	for _, v := range views {
		v.Logo = rc.img(v.Logo)
		rc.addRecord("entity:"+v.Key, v)
		refs = append(refs, recordRef("entity:"+v.Key))
	}
	if leagues == nil {
		leagues = []LeagueView{}
	}
	return map[string]any{"entities": refs, "leagues": rc.signLeagues(leagues), "opening": opening}, nil
}

func (rc *readCtx) watch(ctx context.Context, src screendoc.Source) (any, error) {
	id := strings.TrimPrefix(rc.b.text(src.Args, "event"), "event:")
	if p, _, _ := splitEventID(id); p != rc.t.plugin.pluginID {
		return nil, core.ErrNotFound
	}
	hub := rc.s.hubDeps()
	if hub == nil {
		return nil, core.ErrNotAvailable
	}
	_, feed, _ := splitEventID(id)
	events, err := hub.Events(ctx, rc.viewer, EventQuery{PluginID: rc.t.plugin.pluginID, Feed: feed, IDs: []string{id}})
	if err != nil {
		return nil, err
	}
	for _, ev := range events {
		if ev.ID == id {
			list := []core.LiveEvent{ev}
			rc.enrichWatch(ctx, list)
			w := watchView(list[0])
			for i := range w.Targets {
				w.Targets[i].Logo = rc.channelLogo(w.Targets[i])
			}
			if w.Primary != nil {
				p := *w.Primary
				p.Logo = rc.channelLogo(p)
				w.Primary = &p
			}
			return w, nil
		}
	}
	return nil, core.ErrNotFound
}

func (rc *readCtx) viewerSource(ctx context.Context) (any, error) {
	out := map[string]any{"spoilerFree": false, "canSetSpoilerFree": rc.viewer != nil && rc.viewer.ID > 0, "locale": rc.locale(), "favouriteCount": 0}
	if live, ok := rc.s.hubDeps().(LiveDetails); ok {
		if sf, err := live.SpoilerFree(ctx, rc.viewer); err == nil {
			out["spoilerFree"] = sf
		}
	}
	count := 0
	if rc.favs != nil {
		for key := range rc.favs.full {
			for _, f := range rc.t.plugin.feeds {
				if strings.HasPrefix(key, rc.t.plugin.pluginID+":"+f+":") {
					count++
					break
				}
			}
		}
	}
	out["favouriteCount"] = count
	return out, nil
}

// revStore assigns record and source revisions: a revision changes only when
// the content does, and always grows (max(last+1, now in ms)).
type revStore struct {
	mu    sync.Mutex
	last  int64
	byKey map[string]revEntry
	now   func() time.Time
}

type revEntry struct {
	hash string
	rev  int64
	used int64
}

const maxRevEntries = 50000

func newRevStore(now func() time.Time) *revStore {
	return &revStore{byKey: map[string]revEntry{}, now: now}
}

// signedImageRe matches a signed image URL. Grants are sealed per request,
// so the same picture signs differently each time; content revisions ignore
// the grant, or every read would look like a change.
var signedImageRe = regexp.MustCompile(`"/vela/v0/images/[^"]*"`)

func (r *revStore) revFor(key string, value any) int64 {
	raw, _ := json.Marshal(value)
	raw = signedImageRe.ReplaceAll(raw, []byte(`"img"`))
	sum := sha256.Sum256(raw)
	h := hex.EncodeToString(sum[:12])
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UnixMilli()
	if e, ok := r.byKey[key]; ok && e.hash == h {
		e.used = now
		r.byKey[key] = e
		return e.rev
	}
	rev := max(r.last+1, now)
	r.last = rev
	if len(r.byKey) >= maxRevEntries {
		r.evictLocked()
	}
	r.byKey[key] = revEntry{hash: h, rev: rev, used: now}
	return rev
}

func (r *revStore) evictLocked() {
	// Drop the least recently used half; a dropped key gets a fresh, higher
	// revision next time, which clients accept.
	type kv struct {
		k string
		u int64
	}
	all := make([]kv, 0, len(r.byKey))
	for k, e := range r.byKey {
		all = append(all, kv{k, e.used})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].u < all[j].u })
	for _, e := range all[:len(all)/2] {
		delete(r.byKey, e.k)
	}
}

func (r *revStore) stamp(values map[string]any, out map[string]RecordEntry) int64 {
	var hi int64
	for k, v := range values {
		rev := r.revFor("r\x00"+k, v)
		out[k] = RecordEntry{Rev: rev, Value: v}
		hi = max(hi, rev)
	}
	return hi
}

func (r *revStore) stampSource(key string, v any) int64 {
	return r.revFor("s\x00"+key, v)
}
