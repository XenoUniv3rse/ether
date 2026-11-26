package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/favorites"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/liveevents"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/screens"
)

// Document screens and surfaces (docs/plugin-screens.md): the app wires the
// document registry to plugin lifecycle and policy, and serves frontends.

func (a *App) buildDocRegistry(mgr *plugins.Manager) *screens.DocRegistry {
	a.screenPublish = screens.NewPublishStore(a.db)
	reg := screens.NewDocRegistry(screens.DocRegistryOptions{
		IsPublished: func(ctx context.Context, pluginID, kind, id, frontendID string, def bool) bool {
			return a.screenFrontendPublished(frontendID) && a.screenPublish.Published(ctx, pluginID, kind, id, frontendID, def)
		},
		IsViewerAllowed: func(ctx context.Context, viewer *core.User, pluginID string) bool {
			return a.screenPluginViewerAllowed(ctx, mgr, viewer, pluginID)
		},
		OverlayOrder: func(ctx context.Context, frontendID string) []string {
			return a.screenPublish.OverlayOrder(ctx, frontendID)
		},
		RatingAllowed: func(viewer *core.User, rating string) bool {
			if viewer == nil || viewer.MaxContentRating == "" {
				return true
			}
			return !contentrating.EnforceExceeds(rating, viewer.MaxContentRating)
		},
	})
	sync := func(ctx context.Context, r plugins.Record) {
		if !plugins.ScreensUseDocuments(r.Manifest) {
			reg.Remove(r.ID)
			return
		}
		files, err := mgr.PackageFiles(ctx, r.ID)
		if err != nil {
			a.log.Warn("plugin screens: package files unavailable", "plugin", r.ID, "err", err)
			reg.Remove(r.ID)
			return
		}
		// Texts are read whenever a document is served, long after this
		// sync's context has ended.
		id := r.ID
		text := func(locale, key string) (string, bool) {
			return mgr.ResolveText(context.Background(), id, key, locale)
		}
		if err := reg.Sync(r, files, text); err != nil {
			a.log.Warn("plugin screens: documents rejected", "plugin", r.ID, "err", err)
			return
		}
		a.offerHomeRowSurfaces(ctx, reg, r.ID)
	}
	mgr.OnSync(sync)
	mgr.OnRemove(func(_ context.Context, id string) { reg.Remove(id) })
	return reg
}

// Detail and SpoilerFree make the hub adapter the data reads' LiveDetails.
func (h liveEventsScreenHub) Detail(ctx context.Context, viewer *core.User, eventID string) (*core.LiveEventDetail, error) {
	if h.hub == nil {
		return nil, core.ErrNotAvailable
	}
	d, err := h.hub.GetDetail(ctx, viewer, eventID)
	if err != nil {
		return nil, mapScreenHubError(err)
	}
	return d, nil
}

func (h liveEventsScreenHub) SpoilerFree(ctx context.Context, viewer *core.User) (bool, error) {
	if h.hub == nil || viewer == nil {
		return false, nil
	}
	return h.hub.SpoilerFree(ctx, viewer.ID)
}

// screenLiveSubscriber subscribes the hub for a stream's feeds and events.
type screenLiveSubscriber struct{ hub *liveevents.Hub }

func (s screenLiveSubscriber) Subscribe(ctx context.Context, viewer *core.User, feeds, eventIDs []string) (<-chan struct{}, func(), error) {
	sub, err := s.hub.Subscribe(ctx, liveevents.SubscribeRequest{Feeds: feeds, EventIDs: eventIDs, Viewer: viewer, FilterSpoilers: true, InvalidateOnly: true})
	if err != nil {
		return nil, nil, mapLiveEventError(err)
	}
	out := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case _, ok := <-sub.Updates():
				if !ok {
					close(out)
					return
				}
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	var once sync.Once
	return out, func() { once.Do(func() { close(done); sub.Close() }) }, nil
}

func (s surfaceScreens) docs() *screens.DocRegistry {
	if s.app == nil || s.app.pluginsRT == nil || s.app.pluginsRT.screens == nil {
		return nil
	}
	return s.app.pluginsRT.screens.Documents()
}

func (s surfaceScreens) client(c frontend.ScreenClient) screens.ClientInfo {
	return screens.ClientInfo{Frontend: s.surface, Renderer: c.Renderer, Locale: c.Locale}
}

func mapScreensError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, screens.ErrInvalidParams):
		return frontend.ErrScreenParams
	case errors.Is(err, screens.ErrNoVariant):
		return frontend.ErrScreenNoVariant
	case errors.Is(err, screens.ErrInvalidTopic):
		return frontend.ErrScreenTopic
	case errors.Is(err, screens.ErrTooManyTopics):
		return frontend.ErrScreenTooManyTopics
	case errors.Is(err, core.ErrNotFound):
		return frontend.ErrNotFound
	}
	return err
}

func (s surfaceScreens) ScreenNavigation(ctx context.Context, viewer *frontend.Viewer, c frontend.ScreenClient) (json.RawMessage, error) {
	docs := s.docs()
	if docs == nil || viewer == nil {
		return json.RawMessage("[]"), nil
	}
	return json.Marshal(docs.Navigation(ctx, viewer, s.client(c)))
}

func (s surfaceScreens) ScreenSurfaces(ctx context.Context, viewer *frontend.Viewer, c frontend.ScreenClient) (json.RawMessage, error) {
	docs := s.docs()
	if docs == nil || viewer == nil || c.Renderer <= 0 {
		return json.RawMessage("[]"), nil
	}
	settings, err := s.settings().Load(ctx, viewer.ID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(docs.SurfaceEntries(ctx, viewer, s.client(c), func(id string) (bool, bool) {
		v, ok := settings[id]
		return v, ok
	}))
}

func (s surfaceScreens) settings() screens.SurfaceSettings {
	return screens.SurfaceSettings{DB: s.app.db}
}

func (s surfaceScreens) ScreenDocument(ctx context.Context, viewer *frontend.Viewer, kind, id, page string, c frontend.ScreenClient) ([]byte, string, error) {
	docs := s.docs()
	if docs == nil {
		return nil, "", frontend.ErrNotFound
	}
	body, tag, err := docs.Document(ctx, viewer, screens.DocumentRequest{Kind: kind, ID: id, Page: page, ClientInfo: s.client(c)})
	return body, tag, mapScreensError(err)
}

func (s surfaceScreens) ScreenData(ctx context.Context, viewer *frontend.Viewer, req frontend.ScreenDataRequest) (json.RawMessage, error) {
	if s.app == nil || s.app.pluginsRT == nil || s.app.pluginsRT.screens == nil {
		return nil, frontend.ErrNotFound
	}
	res, err := s.app.pluginsRT.screens.Data(core.WithLiveApp(ctx, s.surface), viewer, screens.DataRequest{Kind: req.Kind, ID: req.ID, Page: req.Page, Sources: req.Sources, Params: req.Params, TZ: req.TZ, Images: req.Images, ClientInfo: s.client(req.Client)})
	if err != nil {
		return nil, mapScreensError(err)
	}
	return json.Marshal(res)
}

func (s surfaceScreens) SetSurfaceSetting(ctx context.Context, viewer *frontend.Viewer, id string, enabled bool) error {
	docs := s.docs()
	if docs == nil || viewer == nil {
		return frontend.ErrNotFound
	}
	t, ok := docs.FindSurface(ctx, viewer, s.surface, id)
	if !ok || t.Meta.Setting == nil {
		return frontend.ErrNotFound
	}
	if err := s.settings().Set(ctx, viewer.ID, t.WireID, enabled); err != nil {
		return err
	}
	// Bootstrap carries the remembered switch; refresh it everywhere.
	docs.Bump()
	return nil
}

func (s surfaceScreens) ScreenAsset(ctx context.Context, pluginID, version, p string) ([]byte, string, error) {
	docs := s.docs()
	if docs == nil || s.app.pluginsRT.mgr == nil || !docs.Asset(pluginID, version, p) {
		return nil, "", frontend.ErrNotFound
	}
	files, err := s.app.pluginsRT.mgr.PackageFiles(ctx, pluginID)
	if err != nil {
		return nil, "", frontend.ErrNotFound
	}
	b, ok := files[p]
	if !ok {
		return nil, "", frontend.ErrNotFound
	}
	ct := mime.TypeByExtension(path.Ext(p))
	if ct == "" {
		ct = "application/octet-stream"
	}
	return b, ct, nil
}

func (s surfaceScreens) ToastSurface(ctx context.Context, viewer *frontend.Viewer, c frontend.ScreenClient, pluginID string) string {
	docs := s.docs()
	if docs == nil || c.Renderer <= 0 {
		return ""
	}
	for _, t := range docs.Surfaces(ctx, viewer, s.surface) {
		if t.PluginID == pluginID && t.Meta.Presentation == "toast" {
			return t.WireID
		}
	}
	return ""
}

type screenStreamAdapter struct {
	frames chan frontend.ScreenFrame
	stop   func()
}

func (a *screenStreamAdapter) Frames() <-chan frontend.ScreenFrame { return a.frames }
func (a *screenStreamAdapter) Close()                              { a.stop() }

func (s surfaceScreens) ScreenStream(ctx context.Context, viewer *frontend.Viewer, topics []string, since int64, images frontend.ScreenImages) (frontend.ScreenStream, error) {
	if s.app == nil || s.app.pluginsRT == nil || s.app.pluginsRT.screens == nil {
		return nil, frontend.ErrNotFound
	}
	in, stop, err := s.app.pluginsRT.screens.Stream(core.WithLiveApp(ctx, s.surface), viewer, topics, since, images)
	if err != nil {
		return nil, mapScreensError(err)
	}
	out := make(chan frontend.ScreenFrame, 8)
	go func() {
		defer close(out)
		for f := range in {
			select {
			case out <- frontend.ScreenFrame{Event: f.Event, Data: f.Data}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &screenStreamAdapter{frames: out, stop: stop}, nil
}

// Built-in actions (ether.*) need no declaration; their answer carries the
// page's sources and records read again after the change, so every card
// redraws from the answer.
const screenActionBudget = 8 * time.Second

func (s surfaceScreens) ScreenAction(ctx context.Context, viewer *frontend.Viewer, req frontend.ScreenActionRequest) (json.RawMessage, error) {
	if s.app == nil || s.app.pluginsRT == nil || s.app.pluginsRT.screens == nil || viewer == nil {
		return nil, frontend.ErrNotFound
	}
	svc := s.app.pluginsRT.screens
	ctx, cancel := context.WithTimeout(core.WithLiveApp(ctx, s.surface), screenActionBudget)
	defer cancel()
	// The page must be one the viewer may use, whatever the action.
	params := req.Params
	if req.Kind == "surface" {
		params = req.Context
	}
	read := screens.DataRequest{Kind: req.Kind, ID: req.ID, Page: req.Page, Params: params, TZ: req.TZ, All: true, Images: req.Images, ClientInfo: s.client(req.Client)}
	if err := svc.Authorize(ctx, viewer, read); err != nil {
		return nil, mapScreensError(err)
	}
	str := func(name string) string {
		var v string
		if raw, ok := req.Args[name]; ok {
			_ = json.Unmarshal(raw, &v)
		}
		return strings.TrimSpace(v)
	}
	boolean := func(name string) (bool, bool) {
		var v bool
		raw, ok := req.Args[name]
		if !ok || json.Unmarshal(raw, &v) != nil {
			var sv string
			if ok && json.Unmarshal(raw, &sv) == nil && (sv == "true" || sv == "false") {
				return sv == "true", true
			}
			return false, false
		}
		return v, true
	}
	switch req.Action {
	case "ether.favorite":
		ref := str("ref")
		on, ok := boolean("on")
		if !ok || ref == "" {
			return nil, frontend.ErrScreenParams
		}
		key, isEntity := strings.CutPrefix(ref, "entity:")
		if !isEntity {
			return nil, frontend.ErrScreenParams
		}
		if _, ok := favorites.ParseEntityKey(key); !ok {
			return nil, frontend.ErrScreenParams
		}
		if err := s.SetFavoriteEntity(ctx, viewer, key, on); err != nil {
			return nil, err
		}
	case "ether.reveal":
		id := strings.TrimPrefix(str("event"), "event:")
		if id == "" {
			return nil, frontend.ErrScreenParams
		}
		if err := (surfaceLiveEvents{app: s.app}).Reveal(ctx, viewer, id); err != nil {
			return nil, err
		}
	case "ether.spoilerFree":
		on, ok := boolean("on")
		if !ok {
			return nil, frontend.ErrScreenParams
		}
		if err := (surfaceLiveEvents{app: s.app}).SetSpoilerFree(ctx, viewer, on); err != nil {
			return nil, err
		}
	default:
		return s.pluginAction(ctx, viewer, req)
	}
	res, err := svc.Data(ctx, viewer, read)
	if err != nil {
		return nil, mapScreensError(err)
	}
	if t, ok := s.docs().FindScreen(ctx, viewer, s.surface, req.ID); ok && req.Kind == "screen" {
		svc.Touched(t.PluginID, viewer.ID)
	} else if t, ok := s.docs().FindSurface(ctx, viewer, s.surface, req.ID); ok && req.Kind == "surface" {
		svc.Touched(t.PluginID, viewer.ID)
	}
	return json.Marshal(map[string]any{"records": res.Records, "sources": res.Sources})
}

// pluginAction runs a plugin-declared action.
func (s surfaceScreens) pluginAction(ctx context.Context, viewer *frontend.Viewer, req frontend.ScreenActionRequest) (json.RawMessage, error) {
	ans, err := s.app.pluginsRT.screens.PluginAction(ctx, viewer, screens.ActionRequest{
		Kind: req.Kind, ID: req.ID, Page: req.Page, Action: req.Action, Params: req.Params, Context: req.Context,
		Args: req.Args, TZ: req.TZ, Images: req.Images, ClientInfo: s.client(req.Client),
	})
	if errors.Is(err, screens.ErrActionRate) {
		return nil, frontend.ErrScreenRateLimited
	}
	if err != nil {
		return nil, mapScreensError(err)
	}
	return json.Marshal(ans)
}

// screenPluginCaller reaches a plugin's /ether/v1/screens|surfaces routes.
// Viewer-scoped calls carry the viewer exactly as a frontend adapter call
// does, so the plugin can use its viewer-bound host services.
type screenPluginCaller struct{ mgr *plugins.Manager }

func (c screenPluginCaller) CallScreen(ctx context.Context, viewer *core.User, pluginID, path string, body any, viewerScoped bool) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req := plugins.Request{PluginID: pluginID, Method: "POST", Path: path, Body: raw, Headers: map[string]string{"Content-Type": "application/json"}}
	if viewerScoped && viewer != nil && viewer.ID > 0 {
		req.Headers["X-Ether-Viewer"] = strconv.FormatInt(viewer.ID, 10)
		req.Headers["X-Ether-Viewer-Name"] = viewerDisplayName(viewer)
		if viewer.MaxContentRating != "" {
			req.Headers["X-Ether-Viewer-Max-Rating"] = viewer.MaxContentRating
		}
	}
	resp, err := c.mgr.Call(ctx, pluginID, req)
	if err != nil {
		return 0, nil, err
	}
	return resp.Status, resp.Body, nil
}

func viewerDisplayName(v *core.User) string {
	if strings.TrimSpace(v.Title) != "" {
		return v.Title
	}
	return v.Username
}

// screenPushes receives screens.invalidate and screens.putRecords.
type screenPushes struct{ svc *screens.Service }

func (p screenPushes) Invalidate(pluginID string) int { return p.svc.Invalidate(pluginID) }

func (p screenPushes) PutRecords(ctx context.Context, pluginID string, raw json.RawMessage) (int, error) {
	var in []struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return 0, fmt.Errorf("%w: records must be a list of {type, id, value}", plugins.ErrPermissionDenied)
	}
	recs := make([]screens.PutRecord, 0, len(in))
	for _, r := range in {
		var v any
		if len(r.Value) > 0 {
			if err := json.Unmarshal(r.Value, &v); err != nil {
				return 0, err
			}
		}
		recs = append(recs, screens.PutRecord{Type: r.Type, ID: r.ID, Value: v})
	}
	return p.svc.PutRecords(ctx, pluginID, recs)
}

func (s surfaceScreens) ScreenPreviewValid(ctx context.Context) bool {
	return s.docs().IsPreview(ctx)
}

var _ frontend.PluginScreens = surfaceScreens{}

// screenItemFinder answers items sources and $item envelopes from the
// plugin's own library (indexed shelf query, bounded; library visibility is
// enforced again when the frontend projects the cards).
type screenItemFinder struct{ app *App }

func (f screenItemFinder) KindShelf(ctx context.Context, viewer *core.User, pluginID string, q screens.ItemsQuery) ([]core.RatingKey, error) {
	if f.app == nil || f.app.pluginsRT == nil || f.app.pluginsRT.pluginitems == nil || viewer == nil {
		return nil, core.ErrNotAvailable
	}
	items := f.app.pluginsRT.pluginitems
	rec, err := items.GetKind(ctx, pluginID, q.Kind)
	if err != nil {
		return nil, core.ErrNotFound
	}
	bq := pluginitems.BrowseQuery{
		LibraryID:        rec.LibraryID,
		Level:            q.Level,
		Sort:             q.Sort,
		Desc:             q.Direction != "asc",
		NoTotal:          true,
		ViewerID:         viewer.ID,
		MaxContentRating: viewer.MaxContentRating,
		Limit:            min(max(q.Limit, 1), 50),
	}
	if q.Facet != "" && len(q.Values) > 0 {
		bq.Facets = map[string][]string{q.Facet: q.Values}
	}
	page, err := items.Browse(ctx, bq)
	if err != nil {
		return nil, err
	}
	keys := make([]core.RatingKey, 0, len(page.Items))
	for _, it := range page.Items {
		if it.Key != 0 {
			keys = append(keys, it.Key)
		}
	}
	return keys, nil
}

func (f screenItemFinder) KindKeys(ctx context.Context, pluginID, kind string, ids []string) (map[string]core.RatingKey, error) {
	if f.app == nil || f.app.pluginsRT == nil || f.app.pluginsRT.pluginitems == nil {
		return nil, core.ErrNotAvailable
	}
	if len(ids) > 200 {
		ids = ids[:200]
	}
	items, err := f.app.pluginsRT.pluginitems.KindItemsByID(ctx, pluginID, kind, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]core.RatingKey, len(items))
	for _, it := range items {
		if it.Key != 0 {
			out[it.ID] = it.Key
		}
	}
	return out, nil
}
