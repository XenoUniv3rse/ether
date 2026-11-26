package screens

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/screendoc"
)

// Plugin sources and plugin actions (docs/plugin-screens.md, Plugin sources
// and Actions). Ether calls the plugin, then rewrites every envelope in its
// answer: images become signed grants on the plugin's own hosts, links are
// https only, records get Ether keys, and events and entities are resolved
// and filtered for the viewer. A string that is a bare URL is dropped.

// PluginCaller reaches a plugin's screen routes. viewerScoped calls carry the
// authenticated viewer (the frontend-adapter binding).
type PluginCaller interface {
	CallScreen(ctx context.Context, viewer *core.User, pluginID, path string, body any, viewerScoped bool) (status int, answer []byte, err error)
}

// SetPluginCaller installs the plugin transport.
func (s *Service) SetPluginCaller(pc PluginCaller) {
	if s != nil {
		s.mu.Lock()
		s.caller = pc
		s.mu.Unlock()
	}
}

func (s *Service) pluginCaller() PluginCaller {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.caller
}

const (
	maxPluginAnswerBytes = 256 << 10
	maxPluginRecords     = 500
	defaultMaxAge        = 60 * time.Second
	maxViewerMaxAge      = 60 * time.Second
	minPluginRefresh     = 10 * time.Second
	maxCachedAnswers     = 512
)

var (
	recordTypeRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	recordIDRe   = regexp.MustCompile(`^[A-Za-z0-9._~:-]{1,128}$`)
	bareURLRe    = regexp.MustCompile(`(?i)^\s*((https?:)?//|data:|javascript:)\S*\s*$`)
)

// pluginAnswer is a plugin source's wire answer.
type pluginAnswer struct {
	Value          json.RawMessage `json:"value"`
	Records        []pluginRecord  `json:"records"`
	MaxAgeSec      int             `json:"maxAgeSec"`
	NextRefreshSec int             `json:"nextRefreshSec"`
}

type pluginRecord struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Value json.RawMessage `json:"value"`
}

// answerCache holds raw plugin answers (before per-viewer rewriting), so a
// shared source is asked once per maxAgeSec however many viewers open it.
type answerCache struct {
	mu      sync.Mutex
	entries map[string]cachedAnswer
	flight  map[string]*answerFlight
}

type cachedAnswer struct {
	raw     []byte
	expires time.Time
}

type answerFlight struct {
	done chan struct{}
	raw  []byte
	err  error
}

func newAnswerCache() *answerCache {
	return &answerCache{entries: map[string]cachedAnswer{}, flight: map[string]*answerFlight{}}
}

func (c *answerCache) get(key string, now time.Time) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || now.After(e.expires) {
		return nil, false
	}
	return e.raw, true
}

func (c *answerCache) put(key string, raw []byte, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxCachedAnswers {
		oldest, first := "", true
		var at time.Time
		for k, e := range c.entries {
			if first || e.expires.Before(at) {
				oldest, at, first = k, e.expires, false
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[key] = cachedAnswer{raw: raw, expires: expires}
}

// forget drops every cached answer of pluginID.
func (c *answerCache) forget(pluginID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if strings.HasPrefix(k, pluginID+"\x00") {
			delete(c.entries, k)
		}
	}
}

// do runs fetch once for concurrent callers of the same key.
func (c *answerCache) do(ctx context.Context, key string, fetch func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	if f, ok := c.flight[key]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.raw, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &answerFlight{done: make(chan struct{})}
	c.flight[key] = f
	c.mu.Unlock()
	f.raw, f.err = fetch()
	c.mu.Lock()
	delete(c.flight, key)
	c.mu.Unlock()
	close(f.done)
	return f.raw, f.err
}

func (rc *readCtx) routePath(kind string, name string) string {
	base := "/ether/v1/screens/"
	if rc.t.kind == "surface" {
		base = "/ether/v1/surfaces/"
	}
	return base + url.PathEscape(rc.t.localID) + "/" + kind + "/" + url.PathEscape(name)
}

func (rc *readCtx) pluginSource(ctx context.Context, src screendoc.Source) (any, error) {
	caller := rc.s.pluginCaller()
	if caller == nil {
		return nil, errUnsupported
	}
	route, _ := src.Args["route"].(string)
	scope, _ := src.Args["scope"].(string)
	viewerScoped := scope == "viewer"
	args := map[string]string{}
	if m, ok := src.Args["args"].(map[string]any); ok {
		for k, v := range m {
			if sv, ok := v.(string); ok {
				args[k] = rc.b.fill(sv)
			}
		}
	}
	body := map[string]any{"args": args, "page": rc.t.page, "locale": rc.locale(), "tz": rc.loc.String()}
	keyParts := []string{rc.t.plugin.pluginID, rc.t.kind, rc.t.localID, route, rc.locale(), rc.loc.String()}
	argKeys := make([]string, 0, len(args))
	for k := range args {
		argKeys = append(argKeys, k+"="+args[k])
	}
	sort.Strings(argKeys)
	keyParts = append(keyParts, strings.Join(argKeys, "&"))
	if viewerScoped {
		keyParts = append(keyParts, "v="+strconv.FormatInt(rc.viewer.ID, 10))
	}
	key := strings.Join(keyParts, "\x00")
	raw, ok := rc.s.answers.get(key, rc.now)
	if !ok {
		var err error
		raw, err = rc.s.answers.do(ctx, key, func() ([]byte, error) {
			status, answer, err := caller.CallScreen(rc.ctx, rc.viewer, rc.t.plugin.pluginID, rc.routePath("sources", route), body, viewerScoped)
			if err != nil {
				return nil, err
			}
			if status == 404 {
				return nil, core.ErrNotFound
			}
			if status < 200 || status > 299 {
				return nil, fmt.Errorf("screens: plugin source answered %d", status)
			}
			if len(answer) > maxPluginAnswerBytes {
				return nil, errSourceTooLarge
			}
			var probe pluginAnswer
			if err := json.Unmarshal(answer, &probe); err != nil {
				return nil, fmt.Errorf("screens: plugin source answer: %w", err)
			}
			maxAge := defaultMaxAge
			if n, ok := src.Args["maxAgeSec"].(int); ok && n > 0 {
				maxAge = time.Duration(n) * time.Second
			}
			if probe.MaxAgeSec > 0 {
				maxAge = time.Duration(min(max(probe.MaxAgeSec, 5), 3600)) * time.Second
			}
			if viewerScoped && maxAge > maxViewerMaxAge {
				maxAge = maxViewerMaxAge
			}
			rc.s.answers.put(key, answer, rc.now.Add(maxAge))
			return answer, nil
		})
		if err != nil {
			return nil, err
		}
	}
	var ans pluginAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return nil, err
	}
	refresh := time.Duration(0)
	if n, ok := src.Args["refreshSec"].(int); ok && n > 0 {
		refresh = time.Duration(n) * time.Second
	}
	if ans.NextRefreshSec > 0 {
		refresh = time.Duration(ans.NextRefreshSec) * time.Second
	}
	if refresh > 0 {
		refresh = max(refresh, minPluginRefresh)
		rc.mu.Lock()
		if rc.refresh == 0 || refresh < rc.refresh {
			rc.refresh = refresh
		}
		rc.mu.Unlock()
	}
	return rc.rewriteAnswer(ctx, ans.Value, ans.Records)
}

func (rc *readCtx) locale() string {
	if rc.clientLocale != "" {
		return rc.clientLocale
	}
	return "en"
}

// rewriteAnswer turns a plugin's value and records into what a client may
// see.
func (rc *readCtx) rewriteAnswer(ctx context.Context, value json.RawMessage, records []pluginRecord) (any, error) {
	if len(records) > maxPluginRecords {
		return nil, fmt.Errorf("screens: a plugin answer may carry at most %d records", maxPluginRecords)
	}
	env := &envelopes{rc: rc, known: map[string]bool{}, events: map[string]bool{}, entities: map[string]entityRef{}, items: map[string]string{}, channels: map[string]bool{}}
	for _, r := range records {
		if recordTypeRe.MatchString(r.Type) && recordIDRe.MatchString(r.ID) {
			env.known["p:"+r.Type+":"+r.ID] = true
		}
	}
	var v any
	if len(bytes.TrimSpace(value)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(value))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
	}
	// Titles and channels are looked up first, so their envelopes become
	// references to cards the viewer may see (or vanish).
	recVals := make([]any, len(records))
	for i, r := range records {
		dec := json.NewDecoder(bytes.NewReader(r.Value))
		dec.UseNumber()
		_ = dec.Decode(&recVals[i])
	}
	env.prepare(ctx, append([]any{v}, recVals...))
	v = env.walk(v)
	for _, r := range records {
		key := "p:" + r.Type + ":" + r.ID
		if !env.known[key] {
			continue
		}
		var rv any
		dec := json.NewDecoder(bytes.NewReader(r.Value))
		dec.UseNumber()
		if err := dec.Decode(&rv); err != nil {
			continue
		}
		rc.addRecord(key, env.walk(rv))
	}
	env.resolve(ctx)
	return v, nil
}

type entityRef struct {
	feed, kind, id string
}

// envelopes rewrites one plugin answer.
type envelopes struct {
	rc       *readCtx
	known    map[string]bool
	events   map[string]bool
	entities map[string]entityRef
	// items maps "<kind>\x00<external id>" to an item: record key;
	// channels holds channel ids with a channel: record.
	items    map[string]string
	channels map[string]bool
}

// prepare finds the $item and $channel envelopes in values and adds the
// cards the viewer may see as records.
func (e *envelopes) prepare(ctx context.Context, values []any) {
	rc := e.rc
	byKind := map[string][]string{}
	var chans []string
	var scan func(any)
	scan = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			if len(n) == 1 {
				if inner, ok := n["$item"].(map[string]any); ok {
					k, _ := inner["kind"].(string)
					id, _ := inner["id"].(string)
					if recordTypeRe.MatchString(k) && recordIDRe.MatchString(id) {
						byKind[k] = append(byKind[k], id)
					}
					return
				}
				if id, ok := n["$channel"].(string); ok && recordIDRe.MatchString(id) {
					chans = append(chans, id)
					return
				}
			}
			for _, c := range n {
				scan(c)
			}
		case []any:
			for _, c := range n {
				scan(c)
			}
		}
	}
	for _, v := range values {
		scan(v)
	}
	if f := rc.s.itemFinder(); f != nil {
		for kind, ids := range byKind {
			if !rc.ownsKind(kind) {
				continue
			}
			keys, err := f.KindKeys(ctx, rc.t.plugin.pluginID, kind, ids)
			if err != nil {
				continue
			}
			list := make([]core.RatingKey, 0, len(keys))
			for _, k := range keys {
				list = append(list, k)
			}
			rc.titleRecords(ctx, list)
			rc.mu.Lock()
			for ext, k := range keys {
				if _, ok := rc.records["item:"+k.String()]; ok {
					e.items[kind+"\x00"+ext] = "item:" + k.String()
				}
			}
			rc.mu.Unlock()
		}
	}
	if cards := rc.cards(); cards != nil && len(chans) > 0 {
		order, got := cards.ChannelCards(ctx, chans, "")
		for _, id := range order {
			if card, ok := got[id]; ok {
				rc.addRecord("channel:"+id, card)
				e.channels[id] = true
			}
		}
	}
}

func (e *envelopes) walk(v any) any {
	switch n := v.(type) {
	case map[string]any:
		if len(n) == 1 {
			for k, inner := range n {
				if strings.HasPrefix(k, "$") {
					return e.envelope(k, inner)
				}
			}
		}
		out := make(map[string]any, len(n))
		for k, child := range n {
			if strings.HasPrefix(k, "$") {
				continue
			}
			out[k] = e.walk(child)
		}
		return out
	case []any:
		out := make([]any, 0, len(n))
		for _, child := range n {
			w := e.walk(child)
			if w == nil {
				if _, wasEnvelope := child.(map[string]any); wasEnvelope && isEnvelope(child) {
					// A dropped envelope closes its gap in a list.
					continue
				}
			}
			out = append(out, w)
		}
		return out
	case string:
		if bareURLRe.MatchString(n) || utf8.RuneCountInString(n) > screendoc.MaxStringLength {
			return nil
		}
		return n
	}
	return v
}

func isEnvelope(v any) bool {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return false
	}
	for k := range m {
		return strings.HasPrefix(k, "$")
	}
	return false
}

func (e *envelopes) envelope(kind string, inner any) any {
	rc := e.rc
	pluginID := rc.t.plugin.pluginID
	switch kind {
	case "$image":
		s, _ := inner.(string)
		if p, ok := strings.CutPrefix(s, "asset:"); ok {
			p = strings.TrimPrefix(p, "/")
			if p == "" || strings.Contains(p, "..") {
				return nil
			}
			return "/vela/v0/screen-assets/" + url.PathEscape(pluginID) + "/" + url.PathEscape(rc.t.plugin.version) + "/" + p
		}
		if img := rc.img(s); img != "" {
			return img
		}
		return nil
	case "$link":
		s, _ := inner.(string)
		u, err := url.Parse(strings.TrimSpace(s))
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(s) > 2048 {
			return nil
		}
		return map[string]string{"href": u.String(), "host": u.Hostname()}
	case "$record":
		m, _ := inner.(map[string]any)
		t, _ := m["type"].(string)
		id, _ := m["id"].(string)
		key := "p:" + t + ":" + id
		if !e.known[key] {
			return nil
		}
		return recordRef(key)
	case "$event":
		m, _ := inner.(map[string]any)
		feed, _ := m["feed"].(string)
		id, _ := m["id"].(string)
		if !containsString(rc.t.plugin.feeds, feed) || !recordIDRe.MatchString(id) {
			return nil
		}
		full := pluginID + ":" + feed + ":" + id
		e.events[full] = true
		return recordRef("event:" + full)
	case "$entity":
		m, _ := inner.(map[string]any)
		feed, _ := m["feed"].(string)
		k, _ := m["kind"].(string)
		id, _ := m["id"].(string)
		if !containsString(rc.t.plugin.feeds, feed) || !recordTypeRe.MatchString(k) || !recordIDRe.MatchString(id) {
			return nil
		}
		key := pluginID + ":" + feed + ":" + k + ":" + id
		e.entities[key] = entityRef{feed: feed, kind: k, id: id}
		return recordRef("entity:" + key)
	case "$item":
		m, _ := inner.(map[string]any)
		k, _ := m["kind"].(string)
		id, _ := m["id"].(string)
		if key, ok := e.items[k+"\x00"+id]; ok {
			return recordRef(key)
		}
		return nil
	case "$channel":
		id, _ := inner.(string)
		if e.channels[id] {
			return recordRef("channel:" + id)
		}
		return nil
	}
	return nil
}

func (rc *readCtx) ownsKind(kind string) bool {
	for _, k := range rc.t.plugin.contrib.MediaKinds {
		if k.Key == kind {
			return true
		}
	}
	return false
}

// resolve fetches the events and entities an answer referenced and adds them
// as records, filtered for the viewer like every other read.
func (e *envelopes) resolve(ctx context.Context) {
	rc := e.rc
	hub := rc.s.hubDeps()
	if hub == nil {
		return
	}
	byFeed := map[string][]string{}
	for full := range e.events {
		_, feed, _ := splitEventID(full)
		byFeed[feed] = append(byFeed[feed], full)
	}
	for feed, ids := range byFeed {
		sort.Strings(ids)
		events, err := hub.Events(ctx, rc.viewer, EventQuery{PluginID: rc.t.plugin.pluginID, Feed: feed, IDs: ids})
		if err != nil {
			continue
		}
		var keep []core.LiveEvent
		for _, ev := range events {
			if e.events[ev.ID] {
				keep = append(keep, ev)
			}
		}
		rc.eventRecords(keep, rc.entityKind(feed))
	}
	type group struct{ feed, kind string }
	groups := map[group][]string{}
	for _, r := range e.entities {
		g := group{r.feed, r.kind}
		groups[g] = append(groups[g], r.id)
	}
	for g, ids := range groups {
		rows, err := hub.Entities(ctx, rc.viewer, EntityQuery{PluginID: rc.t.plugin.pluginID, Feed: g.feed, Kind: g.kind, IDs: ids})
		if err != nil {
			continue
		}
		for _, row := range rows {
			if row.Kind == "" {
				row.Kind = g.kind
			}
			v := entityView(row, rc.t.plugin.pluginID, g.feed, rc.favs)
			v.Logo = rc.img(v.Logo)
			rc.addRecord("entity:"+v.Key, v)
		}
	}
}

// ActionRequest is one plugin action call.
type ActionRequest struct {
	Kind, ID, Page, Action string
	Params, Context        map[string]string
	Args                   map[string]json.RawMessage
	TZ                     string
	Images                 ImageSigner
	ClientInfo
}

// ActionAnswer is what a client receives from an action.
type ActionAnswer struct {
	Records  map[string]RecordEntry `json:"records,omitempty"`
	Sources  map[string]any         `json:"sources,omitempty"`
	Toast    string                 `json:"toast,omitempty"`
	Navigate map[string]any         `json:"navigate,omitempty"`
}

var (
	// ErrActionRate reports a viewer calling actions too often.
	ErrActionRate = errors.New("screens: action rate limited")
)

// PluginAction validates and runs a plugin-declared action, then rewrites
// its answer like a source answer and re-reads the sources it names.
func (s *Service) PluginAction(ctx context.Context, viewer *core.User, req ActionRequest) (ActionAnswer, error) {
	params := req.Params
	if req.Kind == "surface" {
		params = req.Context
	}
	t, err := s.findTarget(ctx, viewer, req.Kind, req.ID, req.Page, req.ClientInfo)
	if err != nil {
		return ActionAnswer{}, err
	}
	decl, ok := t.actions[req.Action]
	if !ok {
		return ActionAnswer{}, core.ErrNotFound
	}
	b, err := bind(t, params, req.TZ)
	if err != nil {
		return ActionAnswer{}, err
	}
	args := map[string]any{}
	for name, raw := range req.Args {
		spec, ok := decl.Args[name]
		if !ok {
			return ActionAnswer{}, fmt.Errorf("%w: unknown argument %s", ErrInvalidParams, name)
		}
		v, err := checkActionArg(spec, raw)
		if err != nil {
			return ActionAnswer{}, fmt.Errorf("%w: %s %v", ErrInvalidParams, name, err)
		}
		args[name] = v
	}
	for name, spec := range decl.Args {
		if _, ok := args[name]; !ok && spec.Required {
			return ActionAnswer{}, fmt.Errorf("%w: %s is required", ErrInvalidParams, name)
		}
	}
	limit := 10
	if decl.Rate == "low" {
		limit = 2
	}
	if !s.actionRate.allow(fmt.Sprintf("%d\x00%s", viewer.ID, t.plugin.pluginID), limit, s.now()) {
		return ActionAnswer{}, ErrActionRate
	}
	caller := s.pluginCaller()
	if caller == nil {
		return ActionAnswer{}, core.ErrNotAvailable
	}
	loc := time.UTC
	if l, err := time.LoadLocation(req.TZ); err == nil && req.TZ != "" && req.TZ != "Local" {
		loc = l
	}
	favs, err := s.loadFavorites(ctx, viewer)
	if err != nil {
		return ActionAnswer{}, err
	}
	rc := &readCtx{ctx: ctx, s: s, viewer: viewer, t: t, b: b, loc: loc, now: s.now(), favs: favs, images: req.Images,
		clientLocale: req.Locale, records: map[string]any{}, events: map[string]*eventFetch{}}
	pageParams := map[string]string{}
	for k, v := range b {
		if name, ok := strings.CutPrefix(k, "param."); ok {
			pageParams[name] = v
		}
	}
	status, raw, err := caller.CallScreen(ctx, viewer, t.plugin.pluginID, rc.routePath("actions", req.Action),
		map[string]any{"args": args, "page": t.page, "params": pageParams, "locale": rc.locale(), "tz": loc.String()}, true)
	if err != nil {
		return ActionAnswer{}, err
	}
	if status == 404 {
		return ActionAnswer{}, core.ErrNotFound
	}
	if status < 200 || status > 299 || len(raw) > maxPluginAnswerBytes {
		return ActionAnswer{}, fmt.Errorf("screens: plugin action answered %d", status)
	}
	var ans struct {
		Records  []pluginRecord             `json:"records"`
		Sources  map[string]json.RawMessage `json:"sources"`
		Refresh  []string                   `json:"refresh"`
		Toast    string                     `json:"toast"`
		Navigate map[string]any             `json:"navigate"`
	}
	if err := json.Unmarshal(raw, &ans); err != nil {
		return ActionAnswer{}, fmt.Errorf("screens: plugin action answer: %w", err)
	}
	s.answers.forget(t.plugin.pluginID)
	out := ActionAnswer{Records: map[string]RecordEntry{}, Sources: map[string]any{}}
	// Records the plugin returns stand alone.
	if _, err := rc.rewriteAnswer(ctx, nil, ans.Records); err != nil {
		return ActionAnswer{}, err
	}
	for id, v := range ans.Sources {
		if _, ok := t.doc.Sources[id]; !ok {
			continue
		}
		rv, err := rc.rewriteAnswer(ctx, v, nil)
		if err == nil {
			out.Sources[id] = rv
		}
	}
	var refresh []string
	for _, id := range ans.Refresh {
		if _, ok := t.doc.Sources[id]; ok && len(refresh) < screendoc.MaxSources {
			refresh = append(refresh, id)
		}
	}
	if len(refresh) > 0 {
		res, err := s.Data(ctx, viewer, DataRequest{Kind: req.Kind, ID: req.ID, Page: req.Page, Sources: refresh, Params: params, TZ: req.TZ, Images: req.Images, ClientInfo: req.ClientInfo})
		if err == nil {
			for k, v := range res.Sources {
				out.Sources[k] = v
			}
			for k, v := range res.Records {
				out.Records[k] = v
			}
		}
	}
	rc.mu.Lock()
	recs := rc.records
	rc.mu.Unlock()
	s.revs.stamp(recs, out.Records)
	if toast := strings.TrimSpace(ans.Toast); toast != "" && !bareURLRe.MatchString(toast) {
		if utf8.RuneCountInString(toast) > 200 {
			toast = string([]rune(toast)[:200])
		}
		out.Toast = toast
	}
	if nav := validNavigate(ans.Navigate); nav != nil {
		out.Navigate = nav
	}
	s.Touched(t.plugin.pluginID, viewer.ID)
	return out, nil
}

// validNavigate keeps a navigate answer in Vela's NavigateAction shape:
// {action: "navigate", args: {page?, screen?, route?, id?, ...params}} of
// short text values only.
func validNavigate(n map[string]any) map[string]any {
	if n == nil || n["action"] != "navigate" {
		return nil
	}
	args, _ := n["args"].(map[string]any)
	if len(args) > 12 {
		return nil
	}
	clean := map[string]any{}
	for k, v := range args {
		sv, ok := v.(string)
		if !ok || len(k) > 32 || utf8.RuneCountInString(sv) > 256 || bareURLRe.MatchString(sv) {
			return nil
		}
		if k == "route" {
			switch sv {
			case "title", "live", "guide", "search", "home", "library":
			default:
				return nil
			}
		}
		clean[k] = sv
	}
	return map[string]any{"action": "navigate", "args": clean}
}

func checkActionArg(spec plugins.ScreenActionArg, raw json.RawMessage) (any, error) {
	// Documents fill templates into text, so a value may arrive as a string
	// ("true", "12", "a,b"); coerce it to the declared type.
	var text string
	isText := json.Unmarshal(raw, &text) == nil
	if isText {
		switch spec.Type {
		case "bool":
			switch strings.TrimSpace(text) {
			case "true", "1":
				return true, nil
			case "false", "0":
				return false, nil
			}
			return nil, errors.New("must be true or false")
		case "int":
			n, err := strconv.Atoi(strings.TrimSpace(text))
			if err != nil {
				return nil, errors.New("must be a whole number")
			}
			raw, _ = json.Marshal(n)
		case "strings":
			var list []string
			for _, part := range strings.Split(text, ",") {
				if p := strings.TrimSpace(part); p != "" {
					list = append(list, p)
				}
			}
			if list == nil {
				list = []string{}
			}
			raw, _ = json.Marshal(list)
		}
	}
	switch spec.Type {
	case "bool":
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, errors.New("must be true or false")
		}
		return b, nil
	case "int":
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, errors.New("must be a whole number")
		}
		if (spec.Min != 0 || spec.Max != 0) && (n < spec.Min || n > spec.Max) {
			return nil, fmt.Errorf("must be from %d to %d", spec.Min, spec.Max)
		}
		return n, nil
	case "strings":
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil || len(list) > 50 {
			return nil, errors.New("must be a list of at most 50 texts")
		}
		for _, s := range list {
			if err := checkText(spec, s); err != nil {
				return nil, err
			}
		}
		return list, nil
	default:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, errors.New("must be text")
		}
		switch spec.Type {
		case "date":
			if _, err := time.Parse("2006-01-02", s); err != nil {
				return nil, errors.New("must be a date, YYYY-MM-DD")
			}
		case "ref":
			if _, err := screendoc.CheckParam(screendoc.Param{Type: "ref"}, s); err != nil {
				return nil, err
			}
		default:
			if err := checkText(spec, s); err != nil {
				return nil, err
			}
		}
		return s, nil
	}
}

func checkText(spec plugins.ScreenActionArg, s string) error {
	max := spec.Max
	if max == 0 {
		max = 512
	}
	if utf8.RuneCountInString(s) > max {
		return fmt.Errorf("is longer than %d characters", max)
	}
	if len(spec.Enum) > 0 && !containsString(spec.Enum, s) {
		return fmt.Errorf("must be one of %s", strings.Join(spec.Enum, ", "))
	}
	return nil
}

// rateLimiter allows n calls per key per 10 s window.
type rateLimiter struct {
	mu      sync.Mutex
	windows map[string]rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

func (l *rateLimiter) allow(key string, n int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.windows == nil {
		l.windows = map[string]rateWindow{}
	}
	w := l.windows[key]
	if now.Sub(w.start) >= 10*time.Second {
		if len(l.windows) > 10000 {
			for k, old := range l.windows {
				if now.Sub(old.start) >= 10*time.Second {
					delete(l.windows, k)
				}
			}
		}
		w = rateWindow{start: now}
	}
	if w.count >= n {
		return false
	}
	w.count++
	l.windows[key] = w
	return true
}
