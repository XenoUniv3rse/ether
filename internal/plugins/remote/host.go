package remote

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"github.com/braedonsaunders/ether/internal/httpjson"
	"github.com/braedonsaunders/ether/internal/plugins"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Host is the set of Ether services a remote plugin may call back
// into (SPEC §5.1.5). It mirrors plugins.Host method for method for
// the services remote plugins may use; secret.get is hosted-only and
// deliberately absent (remote plugins receive owner secrets in
// /ether/configure, and may write derived secrets via SecretSet /
// SecretDelete for {secret:<name>} expansion without reading them back).
// Every method is expected to re-check the plugin's live grants and return
// ErrPermissionDenied when a grant is missing or revoked.
type Host interface {
	KVGet(ctx context.Context, pluginID, key string) ([]byte, bool, error)
	KVPut(ctx context.Context, pluginID, key string, value []byte) error
	KVDelete(ctx context.Context, pluginID, key string) error
	KVList(ctx context.Context, pluginID, prefix string) ([]string, error)
	KVUsage(ctx context.Context, pluginID string) (plugins.KVUsage, error)

	SecretSet(ctx context.Context, pluginID, name, value string, ttlSec int64) error
	SecretDelete(ctx context.Context, pluginID, name string) error

	BadgesSet(ctx context.Context, pluginID, setKey string, entries []plugins.BadgeEntry) error
	BadgesClear(ctx context.Context, pluginID, setKey string) error
	BadgesUsage(ctx context.Context, pluginID string) (plugins.BadgeUsage, error)

	CatalogSearch(ctx context.Context, pluginID, query string, limit, offset int) (json.RawMessage, error)
	CatalogGet(ctx context.Context, pluginID, id string) (json.RawMessage, error)

	ViewerGranted(ctx context.Context, pluginID string) (json.RawMessage, error)
	ViewerHistory(ctx context.Context, pluginID string, viewerID int64, limit, offset int) (json.RawMessage, error)
	SetWatchState(ctx context.Context, pluginID string, viewerID int64, titleID string, watched bool) error

	CollectionsUpsert(ctx context.Context, pluginID string, collection json.RawMessage) error

	KindUpsert(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (int, error)
	KindDelete(ctx context.Context, pluginID, kindKey string, externalIDs []string) error
	KindItems(ctx context.Context, pluginID, kindKey, cursor string, limit int) (core.KindPage, error)
	// KindVisible and KindBrowse evaluate a plugin library's Rules and viewer
	// access policy for the requesting frontend-adapter viewer (#615). The
	// viewer is resolved solely from the host-minted X-Ether-Viewer-Token.
	KindVisible(ctx context.Context, pluginID, viewerToken, kindKey string, ids []string) ([]string, error)
	KindBrowse(ctx context.Context, pluginID, viewerToken, kindKey string, q core.KindBrowseRequest) (core.KindPage, error)
	ResolveSources(ctx context.Context, pluginID string, q core.SourceQuery) ([]core.SourceCandidate, error)
	CachePut(ctx context.Context, pluginID, libraryKey, name string, r io.Reader) (string, error)
	CacheOpen(ctx context.Context, pluginID, blobRef string) (io.ReadSeekCloser, error)
	CacheDelete(ctx context.Context, pluginID, blobRef string) error

	// StreamURL mints a scoped stream ticket for one of the plugin's own
	// kind items for viewerID and returns the Ether stream URL (spec §6.5).
	StreamURL(ctx context.Context, pluginID string, viewerID int64, itemRef string) (string, error)

	Settings(ctx context.Context, pluginID string) (map[string]any, error)

	PublishEvent(ctx context.Context, pluginID, key string, payload json.RawMessage, corr events.Correlation) error

	// ScreensInvalidate and ScreensPutRecords are the screens pushes
	// (docs/plugin-screens.md); both need the screens capability.
	ScreensInvalidate(ctx context.Context, pluginID string) (int, error)
	ScreensPutRecords(ctx context.Context, pluginID string, records json.RawMessage) (int, error)

	// AIComplete runs a bounded completion against the owner's configured
	// language model (requires permissions.ai: "complete", or "complete+search"
	// when viewerToken identifies an active frontend-adapter search call; #702).
	AIComplete(ctx context.Context, pluginID, viewerToken string, req plugins.AICompleteRequest) (plugins.AICompleteResponse, error)

	// MediaProbe reads container metadata ({durationSec, container, width,
	// height, sizeBytes}) for a PlayableSource in the same shape /resolve
	// returns, reusing release_probes (requires permissions.media: "probe";
	// refused when viewerToken is set; #712).
	MediaProbe(ctx context.Context, pluginID, viewerToken string, src core.PlayableSource) (core.MediaProbeResult, error)

	NotifyOwner(ctx context.Context, pluginID, text string) error
	Log(ctx context.Context, pluginID, level, msg string, attrs map[string]any) error
}

// Body caps for host calls. KV values get the largest allowance; the
// host's per-plugin quota still applies on top.
const (
	maxKVValueBytes    = 5 << 20
	maxHostBodyBytes   = 2 << 20
	maxAIBodyBytes     = 128 << 10
	maxCollectionBytes = 2 << 20
	maxKindUpsertBytes = 8 << 20
	maxCacheBlobBytes  = 256 << 20
	defaultPageLimit   = 50
	maxPageLimit       = 200
)

// HostHandler serves /ether/plugin-host/* for remote plugins. Every
// request is authenticated with a plugin credential; the resolved
// plugin id is the only identity a call can act as, so a plugin can
// never name another plugin in a request body and be believed.
type HostHandler struct {
	host Host
	auth CredentialResolver
	log  *slog.Logger
	mux  *route.Mux
}

// HostOptions configures a HostHandler.
type HostOptions struct {
	Host   Host
	Auth   CredentialResolver
	Logger *slog.Logger
}

// NewHostHandler builds the plugin-host API. Mount the result at
// HostPath (or use Mount to register it on a mux).
func NewHostHandler(opts HostOptions) *HostHandler {
	h := &HostHandler{host: opts.Host, auth: opts.Auth, log: opts.Logger}
	if h.log == nil {
		h.log = slog.Default()
	}
	mux := &route.Mux{}
	mux.HandleFunc("POST /ether/plugin-host/data/{service}", h.dataCall)
	mux.HandleFunc("GET /ether/plugin-host/kv", h.kvList)
	mux.HandleFunc("GET /ether/plugin-host/kv-usage", h.kvUsage)
	mux.HandleFunc("GET /ether/plugin-host/kv/{key...}", h.kvGet)
	mux.HandleFunc("PUT /ether/plugin-host/kv/{key...}", h.kvPut)
	mux.HandleFunc("DELETE /ether/plugin-host/kv/{key...}", h.kvDelete)
	mux.HandleFunc("PUT /ether/plugin-host/secrets/{name}", h.secretSet)
	mux.HandleFunc("DELETE /ether/plugin-host/secrets/{name}", h.secretDelete)
	mux.HandleFunc("GET /ether/plugin-host/badges/usage", h.badgesUsage)
	mux.HandleFunc("GET /ether/plugin-host/badges-usage", h.badgesUsage)
	mux.HandleFunc("PUT /ether/plugin-host/badges/{setKey}", h.badgesSet)
	mux.HandleFunc("POST /ether/plugin-host/badges/{setKey}", h.badgesPost)
	mux.HandleFunc("POST /ether/plugin-host/badges/{setKey}/set", h.badgesSet)
	mux.HandleFunc("DELETE /ether/plugin-host/badges/{setKey}", h.badgesClear)
	mux.HandleFunc("POST /ether/plugin-host/badges/{setKey}/clear", h.badgesClear)
	mux.HandleFunc("GET /ether/plugin-host/catalog/search", h.catalogSearch)
	mux.HandleFunc("GET /ether/plugin-host/catalog/{id...}", h.catalogGet)
	mux.HandleFunc("GET /ether/plugin-host/viewers/{viewerId}/history", h.viewerHistory)
	mux.HandleFunc("GET /ether/plugin-host/viewers/granted", h.viewerGranted)
	mux.HandleFunc("POST /ether/plugin-host/viewers/watch-state", h.setWatchState)
	mux.HandleFunc("POST /ether/plugin-host/collections/upsert", h.collectionsUpsert)
	mux.HandleFunc("GET /ether/plugin-host/kinds/{kind}/items", h.kindItems)
	mux.HandleFunc("POST /ether/plugin-host/kinds/{kind}/upsert", h.kindUpsert)
	mux.HandleFunc("POST /ether/plugin-host/kinds/{kind}/delete", h.kindDelete)
	mux.HandleFunc("POST /ether/plugin-host/kinds/{kind}/visible", h.kindVisible)
	mux.HandleFunc("POST /ether/plugin-host/kinds/{kind}/browse", h.kindBrowse)
	mux.HandleFunc("POST /ether/plugin-host/sources/resolve", h.resolveSources)
	mux.HandleFunc("PUT /ether/plugin-host/cache/{libraryKey}/{name...}", h.cachePut)
	mux.HandleFunc("POST /ether/plugin-host/cache/{libraryKey}", h.cachePut)
	mux.HandleFunc("GET /ether/plugin-host/cache/{blobRef...}", h.cacheOpen)
	mux.HandleFunc("DELETE /ether/plugin-host/cache/{blobRef...}", h.cacheDelete)
	mux.HandleFunc("POST /ether/plugin-host/frontend/stream-url", h.frontendStreamURL)
	mux.HandleFunc("GET /ether/plugin-host/settings", h.settings)
	mux.HandleFunc("GET /ether/plugin-host/limits", h.runtimeLimits)
	mux.HandleFunc("POST /ether/plugin-host/events/publish", h.eventsPublish)
	mux.HandleFunc("POST /ether/plugin-host/ai/complete", h.aiComplete)
	mux.HandleFunc("POST /ether/plugin-host/media/probe", h.mediaProbe)
	mux.HandleFunc("POST /ether/plugin-host/notify", h.notify)
	mux.HandleFunc("POST /ether/plugin-host/log", h.logLine)
	mux.HandleFunc("POST /ether/plugin-host/screens/invalidate", h.screensInvalidate)
	mux.HandleFunc("POST /ether/plugin-host/screens/records", h.screensPutRecords)
	h.mux = mux
	return h
}

func (h *HostHandler) Routes() []route.Route { return h.mux.Routes() }

// Mount registers the handler's routes on mux.
func (h *HostHandler) Mount(mux *http.ServeMux) {
	mux.Handle(HostPath, h)
}

func (h *HostHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil || h.host == nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "Plugin host API is not configured")
		return
	}
	pluginID, err := h.auth.ResolvePluginCredential(r.Context(), credentialFromRequest(r))
	if err != nil || pluginID == "" {
		writeHostError(w, http.StatusUnauthorized, "unauthorized", "Plugin credential rejected")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), pluginIDContextKey{}, pluginID))
	h.mux.ServeHTTP(w, r)
}

type pluginIDContextKey struct{}

func pluginIDOf(r *http.Request) string {
	id, _ := r.Context().Value(pluginIDContextKey{}).(string)
	return id
}

// --- KV ---

func (h *HostHandler) kvGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	value, ok, err := h.host.KVGet(r.Context(), pluginIDOf(r), key)
	if !h.finishErr(w, err) {
		return
	}
	if !ok {
		writeHostError(w, http.StatusNotFound, "not_found", "No value for this key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "value": string(value)})
}

func (h *HostHandler) kvPut(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var body struct {
		Value *string `json:"value"`
	}
	// Accept either {"value": "..."} or a raw text body: the SDKs send
	// JSON, hand-rolled sidecars often send the bare value.
	raw, err := readBody(w, r, maxKVValueBytes)
	if err != nil {
		h.finishErr(w, err)
		return
	}
	value := raw
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &body); err != nil || body.Value == nil {
			writeHostError(w, http.StatusBadRequest, "invalid", `Body must be {"value": "..."}`)
			return
		}
		value = []byte(*body.Value)
	}
	if !h.finishErr(w, h.host.KVPut(r.Context(), pluginIDOf(r), key, value)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *HostHandler) kvDelete(w http.ResponseWriter, r *http.Request) {
	if !h.finishErr(w, h.host.KVDelete(r.Context(), pluginIDOf(r), r.PathValue("key"))) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *HostHandler) kvList(w http.ResponseWriter, r *http.Request) {
	keys, err := h.host.KVList(r.Context(), pluginIDOf(r), r.URL.Query().Get("prefix"))
	if !h.finishErr(w, err) {
		return
	}
	if keys == nil {
		keys = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (h *HostHandler) kvUsage(w http.ResponseWriter, r *http.Request) {
	usage, err := h.host.KVUsage(r.Context(), pluginIDOf(r))
	if !h.finishErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

// --- Derived secrets (#660) ---

func (h *HostHandler) secretSet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeHostError(w, http.StatusBadRequest, "invalid", "Secret name is required")
		return
	}
	var body struct {
		Value  string `json:"value"`
		TTLSec int64  `json:"ttlSec"`
	}
	if !h.decode(w, r, maxHostBodyBytes, &body) {
		return
	}
	if !h.finishErr(w, h.host.SecretSet(r.Context(), pluginIDOf(r), name, body.Value, body.TTLSec)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HostHandler) secretDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeHostError(w, http.StatusBadRequest, "invalid", "Secret name is required")
		return
	}
	if !h.finishErr(w, h.host.SecretDelete(r.Context(), pluginIDOf(r), name)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Poster badges (#594) ---

func decodeBadgeEntries(raw []byte) ([]plugins.BadgeEntry, string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		var entries []plugins.BadgeEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, "", false
		}
		return entries, "", true
	}
	var body struct {
		SetKey  string               `json:"setKey"`
		Entries []plugins.BadgeEntry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, "", false
	}
	return body.Entries, body.SetKey, true
}

func (h *HostHandler) badgesSet(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(w, r, maxHostBodyBytes)
	if err != nil {
		h.finishErr(w, err)
		return
	}
	entries, _, ok := decodeBadgeEntries(raw)
	if !ok {
		writeHostError(w, http.StatusBadRequest, "invalid", `Body must be {"entries": [...]}`)
		return
	}
	if !h.finishErr(w, h.host.BadgesSet(r.Context(), pluginIDOf(r), r.PathValue("setKey"), entries)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *HostHandler) badgesPost(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(w, r, maxHostBodyBytes)
	if err != nil {
		h.finishErr(w, err)
		return
	}
	pathKey := r.PathValue("setKey")
	entries, bodySetKey, ok := decodeBadgeEntries(raw)
	if !ok {
		writeHostError(w, http.StatusBadRequest, "invalid", `Body must be {"entries": [...]}`)
		return
	}
	if pathKey == "clear" && bodySetKey != "" {
		if !h.finishErr(w, h.host.BadgesClear(r.Context(), pluginIDOf(r), bodySetKey)) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if pathKey == "set" && bodySetKey != "" {
		pathKey = bodySetKey
	}
	if !h.finishErr(w, h.host.BadgesSet(r.Context(), pluginIDOf(r), pathKey, entries)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *HostHandler) badgesClear(w http.ResponseWriter, r *http.Request) {
	if !h.finishErr(w, h.host.BadgesClear(r.Context(), pluginIDOf(r), r.PathValue("setKey"))) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *HostHandler) badgesUsage(w http.ResponseWriter, r *http.Request) {
	usage, err := h.host.BadgesUsage(r.Context(), pluginIDOf(r))
	if !h.finishErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

// --- Catalog ---

func (h *HostHandler) catalogSearch(w http.ResponseWriter, r *http.Request) {
	limit, offset := pageParams(r)
	out, err := h.host.CatalogSearch(r.Context(), pluginIDOf(r), r.URL.Query().Get("q"), limit, offset)
	h.writeRaw(w, out, err)
}

func (h *HostHandler) catalogGet(w http.ResponseWriter, r *http.Request) {
	out, err := h.host.CatalogGet(r.Context(), pluginIDOf(r), r.PathValue("id"))
	h.writeRaw(w, out, err)
}

// --- Viewers ---

func (h *HostHandler) viewerGranted(w http.ResponseWriter, r *http.Request) {
	out, err := h.host.ViewerGranted(r.Context(), pluginIDOf(r))
	h.writeRaw(w, out, err)
}

func (h *HostHandler) viewerHistory(w http.ResponseWriter, r *http.Request) {
	viewerID, err := strconv.ParseInt(r.PathValue("viewerId"), 10, 64)
	if err != nil || viewerID <= 0 {
		writeHostError(w, http.StatusBadRequest, "invalid", "viewerId must be a positive number")
		return
	}
	limit, offset := pageParams(r)
	out, err := h.host.ViewerHistory(r.Context(), pluginIDOf(r), viewerID, limit, offset)
	h.writeRaw(w, out, err)
}

func (h *HostHandler) setWatchState(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ViewerID int64  `json:"viewerId"`
		TitleID  string `json:"titleId"`
		Watched  *bool  `json:"watched"`
	}
	if !h.decode(w, r, maxHostBodyBytes, &body) {
		return
	}
	if body.ViewerID <= 0 || body.TitleID == "" || body.Watched == nil {
		writeHostError(w, http.StatusBadRequest, "invalid", "viewerId, titleId and watched are required")
		return
	}
	if !h.finishErr(w, h.host.SetWatchState(r.Context(), pluginIDOf(r), body.ViewerID, body.TitleID, *body.Watched)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- Collections, kinds, sources, cache, notify, log ---

func (h *HostHandler) collectionsUpsert(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(w, r, maxCollectionBytes)
	if err != nil {
		h.finishErr(w, err)
		return
	}
	if !json.Valid(raw) {
		writeHostError(w, http.StatusBadRequest, "invalid", "Body must be a collection document as JSON")
		return
	}
	if !h.finishErr(w, h.host.CollectionsUpsert(r.Context(), pluginIDOf(r), raw)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *HostHandler) kindItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := defaultPageLimit
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	page, err := h.host.KindItems(r.Context(), pluginIDOf(r), r.PathValue("kind"), q.Get("cursor"), limit)
	if !h.finishErr(w, err) {
		return
	}
	if page.Items == nil {
		page.Items = []core.KindItem{}
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *HostHandler) kindUpsert(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(w, r, maxKindUpsertBytes)
	if err != nil {
		h.finishErr(w, err)
		return
	}
	var items []core.KindItem
	var envelope struct {
		Items []core.KindItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Items != nil {
		items = envelope.Items
	} else if err := json.Unmarshal(raw, &items); err != nil {
		writeHostError(w, http.StatusBadRequest, "invalid", `Body must be {"items": [...]} or a JSON array of KindItem`)
		return
	}
	result := plugins.KindUpsertResult{}
	if wh, ok := h.host.(interface {
		KindUpsertWithResult(context.Context, string, string, []core.KindItem) (plugins.KindUpsertResult, error)
	}); ok {
		result, err = wh.KindUpsertWithResult(r.Context(), pluginIDOf(r), r.PathValue("kind"), items)
	} else {
		var accepted int
		accepted, err = h.host.KindUpsert(r.Context(), pluginIDOf(r), r.PathValue("kind"), items)
		result.Accepted = accepted
	}
	if !h.finishErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "accepted": result.Accepted, "added": result.Added,
		"updated": result.Updated, "unchanged": result.Unchanged,
	})
}

func (h *HostHandler) kindDelete(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(w, r, maxHostBodyBytes)
	if err != nil {
		h.finishErr(w, err)
		return
	}
	var ids []string
	var envelope struct {
		IDs         []string `json:"ids"`
		ExternalIDs []string `json:"externalIds"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && (envelope.IDs != nil || envelope.ExternalIDs != nil) {
		ids = envelope.IDs
		if len(ids) == 0 {
			ids = envelope.ExternalIDs
		}
	} else if err := json.Unmarshal(raw, &ids); err != nil {
		writeHostError(w, http.StatusBadRequest, "invalid", `Body must be {"ids": [...]} or a JSON array of item ids`)
		return
	}
	if !h.finishErr(w, h.host.KindDelete(r.Context(), pluginIDOf(r), r.PathValue("kind"), ids)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// kindVisible returns the subset of external IDs in {kind} allowed by the
// library's Rules and viewer access policy (#615). The viewer identity comes
// exclusively from the host-minted X-Ether-Viewer-Token header on active
// frontend adapter requests, never from guest-supplied viewer IDs.
func (h *HostHandler) kindVisible(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(w, r, maxHostBodyBytes)
	if err != nil {
		h.finishErr(w, err)
		return
	}
	var ids []string
	var envelope struct {
		IDs []string `json:"ids"`
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		ids = nil
	} else if err := json.Unmarshal(raw, &envelope); err == nil && envelope.IDs != nil {
		ids = envelope.IDs
	} else if err := json.Unmarshal(raw, &ids); err != nil {
		writeHostError(w, http.StatusBadRequest, "invalid", `Body must be {"ids": [...]} or a JSON array of item ids`)
		return
	}
	viewerToken := strings.TrimSpace(r.Header.Get(HeaderViewerToken))
	visible, err := h.host.KindVisible(r.Context(), pluginIDOf(r), viewerToken, r.PathValue("kind"), ids)
	if !h.finishErr(w, err) {
		return
	}
	if visible == nil {
		visible = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ids": visible})
}

// kindBrowse browses or searches {kind} using the same LibraryPolicy path as
// Ether's console and protocol projectors for the requesting frontend-adapter
// viewer (#615). The viewer identity comes exclusively from X-Ether-Viewer-Token.
func (h *HostHandler) kindBrowse(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(w, r, maxHostBodyBytes)
	if err != nil {
		h.finishErr(w, err)
		return
	}
	var q core.KindBrowseRequest
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &q); err != nil {
			writeHostError(w, http.StatusBadRequest, "invalid", "Body must be a JSON browse query object")
			return
		}
	}
	viewerToken := strings.TrimSpace(r.Header.Get(HeaderViewerToken))
	page, err := h.host.KindBrowse(r.Context(), pluginIDOf(r), viewerToken, r.PathValue("kind"), q)
	if !h.finishErr(w, err) {
		return
	}
	if page.Items == nil {
		page.Items = []core.KindItem{}
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *HostHandler) resolveSources(w http.ResponseWriter, r *http.Request) {
	var q core.SourceQuery
	if !h.decode(w, r, maxHostBodyBytes, &q) {
		return
	}
	var (
		res core.SourceResolveResult
		err error
	)
	if rh, ok := h.host.(interface {
		ResolveSourcesWithResult(context.Context, string, core.SourceQuery) (core.SourceResolveResult, error)
	}); ok {
		res, err = rh.ResolveSourcesWithResult(r.Context(), pluginIDOf(r), q)
	} else {
		res.Candidates, err = h.host.ResolveSources(r.Context(), pluginIDOf(r), q)
	}
	if !h.finishErr(w, err) {
		return
	}
	if res.Candidates == nil {
		res.Candidates = []core.SourceCandidate{}
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *HostHandler) cachePut(w http.ResponseWriter, r *http.Request) {
	libraryKey := r.PathValue("libraryKey")
	name := r.PathValue("name")
	if name == "" {
		name = r.URL.Query().Get("name")
	}
	body := io.LimitReader(r.Body, maxCacheBlobBytes+1)
	ref, err := h.host.CachePut(r.Context(), pluginIDOf(r), libraryKey, name, body)
	if !h.finishErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "blob": ref})
}

func (h *HostHandler) cacheOpen(w http.ResponseWriter, r *http.Request) {
	rc, err := h.host.CacheOpen(r.Context(), pluginIDOf(r), r.PathValue("blobRef"))
	if !h.finishErr(w, err) {
		return
	}
	defer rc.Close()
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", time.Time{}, rc)
}

func (h *HostHandler) cacheDelete(w http.ResponseWriter, r *http.Request) {
	if !h.finishErr(w, h.host.CacheDelete(r.Context(), pluginIDOf(r), r.PathValue("blobRef"))) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type streamViewerTokenKey struct{}

func withStreamViewerToken(ctx context.Context, token string) context.Context {
	token = strings.TrimSpace(token)
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, streamViewerTokenKey{}, token)
}

func streamViewerToken(ctx context.Context) string {
	tok, _ := ctx.Value(streamViewerTokenKey{}).(string)
	return tok
}

// frontendStreamURL mints a scoped stream ticket for one of the plugin's
// own kind items and returns the Ether stream URL (spec §6.5). The viewer
// id is the one the adapter proxied the request for; when X-Ether-Viewer-Token
// is forwarded, Ether also binds the active adapter route and request origin
// for absolute stream URLs (#772).
func (h *HostHandler) frontendStreamURL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ViewerID  int64  `json:"viewerId"`
		ItemRef   string `json:"itemRef"`
		PartIndex *int   `json:"partIndex,omitempty"`
		Source    string `json:"source,omitempty"`
		Absolute  bool   `json:"absolute,omitempty"`
	}
	if !h.decode(w, r, maxHostBodyBytes, &body) {
		return
	}
	if body.PartIndex != nil && *body.PartIndex < 0 {
		writeHostError(w, http.StatusBadRequest, "bad_arguments", "partIndex must be non-negative")
		return
	}
	if body.Source != "" && strings.TrimSpace(body.Source) == "" {
		writeHostError(w, http.StatusBadRequest, "bad_arguments", "source must not be blank")
		return
	}
	ctx := withStreamViewerToken(r.Context(), r.Header.Get(HeaderViewerToken))
	var (
		u   string
		err error
	)
	if body.Source != "" || body.Absolute {
		oh, ok := h.host.(interface {
			StreamURLWithOptions(ctx context.Context, pluginID string, viewerID int64, itemRef string, opts plugins.StreamURLOptions) (string, error)
		})
		if !ok {
			writeHostError(w, http.StatusServiceUnavailable, "unavailable", "This host service does not support stream URL options")
			return
		}
		u, err = oh.StreamURLWithOptions(ctx, pluginIDOf(r), body.ViewerID, body.ItemRef, plugins.StreamURLOptions{
			PartIndex: body.PartIndex,
			Source:    body.Source,
			Absolute:  body.Absolute,
		})
	} else if body.PartIndex != nil {
		if oh, ok := h.host.(interface {
			StreamURLWithOptions(ctx context.Context, pluginID string, viewerID int64, itemRef string, opts plugins.StreamURLOptions) (string, error)
		}); ok {
			u, err = oh.StreamURLWithOptions(ctx, pluginIDOf(r), body.ViewerID, body.ItemRef, plugins.StreamURLOptions{PartIndex: body.PartIndex})
		} else if ph, ok := h.host.(interface {
			StreamPartURL(ctx context.Context, pluginID string, viewerID int64, itemRef string, partIndex *int) (string, error)
		}); ok {
			u, err = ph.StreamPartURL(ctx, pluginIDOf(r), body.ViewerID, body.ItemRef, body.PartIndex)
		} else {
			u, err = h.host.StreamURL(ctx, pluginIDOf(r), body.ViewerID, body.ItemRef)
			if err == nil {
				sep := "?"
				if strings.Contains(u, "?") {
					sep = "&"
				}
				u = u + sep + "part=" + strconv.Itoa(*body.PartIndex)
			}
		}
	} else {
		u, err = h.host.StreamURL(ctx, pluginIDOf(r), body.ViewerID, body.ItemRef)
	}
	if !h.finishErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": u})
}

func (h *HostHandler) settings(w http.ResponseWriter, r *http.Request) {
	v, err := h.host.Settings(r.Context(), pluginIDOf(r))
	if !h.finishErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": v})
}

func (h *HostHandler) runtimeLimits(w http.ResponseWriter, r *http.Request) {
	service, ok := h.host.(interface {
		CallDataHost(context.Context, string, string, json.RawMessage) (json.RawMessage, error)
	})
	if !ok {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "Data services unavailable")
		return
	}
	out, err := service.CallDataHost(r.Context(), pluginIDOf(r), "runtime.limits", json.RawMessage("[]"))
	h.writeRaw(w, out, err)
}

func (h *HostHandler) eventsPublish(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key         string             `json:"key"`
		Payload     json.RawMessage    `json:"payload"`
		Correlation events.Correlation `json:"correlation"`
	}
	if !h.decode(w, r, maxHostBodyBytes, &body) {
		return
	}
	if strings.TrimSpace(body.Key) == "" {
		writeHostError(w, http.StatusBadRequest, "invalid", "key is required")
		return
	}
	if len(body.Payload) == 0 || string(body.Payload) == "null" {
		body.Payload = json.RawMessage(`{}`)
	}
	if !h.finishErr(w, h.host.PublishEvent(r.Context(), pluginIDOf(r), body.Key, body.Payload, body.Correlation)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *HostHandler) screensInvalidate(w http.ResponseWriter, r *http.Request) {
	n, err := h.host.ScreensInvalidate(r.Context(), pluginIDOf(r))
	if !h.finishErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"streams": n})
}

func (h *HostHandler) screensPutRecords(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Records json.RawMessage `json:"records"`
	}
	if !h.decode(w, r, maxHostBodyBytes, &body) {
		return
	}
	n, err := h.host.ScreensPutRecords(r.Context(), pluginIDOf(r), body.Records)
	if !h.finishErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"streams": n})
}

func (h *HostHandler) aiComplete(w http.ResponseWriter, r *http.Request) {
	viewerToken := strings.TrimSpace(r.Header.Get(HeaderViewerToken))
	var req plugins.AICompleteRequest
	if !h.decode(w, r, maxAIBodyBytes, &req) {
		return
	}
	resp, err := h.host.AIComplete(r.Context(), pluginIDOf(r), viewerToken, req)
	if !h.finishErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *HostHandler) mediaProbe(w http.ResponseWriter, r *http.Request) {
	viewerToken := strings.TrimSpace(r.Header.Get(HeaderViewerToken))
	var src core.PlayableSource
	if !h.decode(w, r, maxHostBodyBytes, &src) {
		return
	}
	resp, err := h.host.MediaProbe(r.Context(), pluginIDOf(r), viewerToken, src)
	if !h.finishErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *HostHandler) notify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !h.decode(w, r, maxHostBodyBytes, &body) {
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeHostError(w, http.StatusBadRequest, "invalid", "text is required")
		return
	}
	if !h.finishErr(w, h.host.NotifyOwner(r.Context(), pluginIDOf(r), body.Text)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *HostHandler) logLine(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Level   string         `json:"level"`
		Message string         `json:"message"`
		Attrs   map[string]any `json:"attrs"`
	}
	if !h.decode(w, r, maxHostBodyBytes, &body) {
		return
	}
	if body.Level == "" {
		body.Level = "info"
	}
	if !h.finishErr(w, h.host.Log(r.Context(), pluginIDOf(r), body.Level, body.Message, body.Attrs)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- helpers ---

func (h *HostHandler) writeRaw(w http.ResponseWriter, out json.RawMessage, err error) {
	if !h.finishErr(w, err) {
		return
	}
	if len(out) == 0 {
		out = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (h *HostHandler) decode(w http.ResponseWriter, r *http.Request, max int64, into any) bool {
	raw, err := readBody(w, r, max)
	if err != nil {
		h.finishErr(w, err)
		return false
	}
	if err := json.Unmarshal(raw, into); err != nil {
		writeHostError(w, http.StatusBadRequest, "invalid", "Body is not valid JSON")
		return false
	}
	return true
}

// finishErr maps a host error onto the EPP error shape and reports
// whether the handler may continue. Host errors are mapped, never
// echoed raw: a storage error must not leak SQL or paths to a
// plugin.
func (h *HostHandler) finishErr(w http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	var blockedLocal *plugins.BlockedLocalHostError
	if errors.As(err, &blockedLocal) {
		writeHostError(w, http.StatusForbidden, "permission_denied", blockedLocal.PluginMessage())
		return false
	}
	var blockedOp *plugins.BlockedOperatorHostError
	if errors.As(err, &blockedOp) {
		writeHostError(w, http.StatusForbidden, "permission_denied", blockedOp.PluginMessage())
		return false
	}
	switch {
	case errors.Is(err, ErrPermissionDenied):
		writeHostError(w, http.StatusForbidden, "permission_denied", "This plugin does not have the grant this call needs")
	case errors.Is(err, ErrNotFound):
		writeHostError(w, http.StatusNotFound, "not_found", "Not found")
	case errors.Is(err, plugins.ErrQuotaExceeded):
		msg := strings.TrimPrefix(err.Error(), "remote: response body too large: ")
		writeHostError(w, http.StatusRequestEntityTooLarge, "quota_exceeded", msg)
	case errors.Is(err, ErrTooLarge):
		writeHostError(w, http.StatusRequestEntityTooLarge, "too_large", "Request body is too large")
	case errors.Is(err, ErrInvalid):
		writeHostError(w, http.StatusBadRequest, "invalid", cleanMessage(err))
	case errors.Is(err, ErrRateLimited):
		var rl *plugins.RateLimitError
		if errors.As(err, &rl) {
			sec := rl.RetryAfterSeconds(time.Now())
			if sec > 0 {
				w.Header().Set("Retry-After", strconv.FormatInt(sec, 10))
			}
			errObj := map[string]any{
				"code":    "rate_limited",
				"message": rl.Error(),
			}
			if sec > 0 {
				errObj["retryAfterSec"] = sec
			}
			if !rl.RetryAt.IsZero() {
				errObj["retryAt"] = rl.RetryAt.UTC().Format(time.RFC3339)
			}
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": errObj})
			return false
		}
		writeHostError(w, http.StatusTooManyRequests, "rate_limited", strings.TrimPrefix(err.Error(), "remote: rate limit exceeded: "))
	case errors.Is(err, ErrNotConfigured):
		writeHostError(w, http.StatusNotImplemented, "not_configured", "Language model is not configured")
	case errors.Is(err, ErrInvalidOutput):
		writeHostError(w, http.StatusBadGateway, "invalid_output", "Language model returned invalid or oversized output")
	case errors.Is(err, ErrUnavailable):
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "This host service is not available")
	case errors.Is(err, ErrUnauthorized):
		writeHostError(w, http.StatusUnauthorized, "unauthorized", "Plugin credential rejected")
	case errors.Is(err, context.DeadlineExceeded):
		writeHostError(w, http.StatusGatewayTimeout, "deadline", "The call ran past its deadline")
	default:
		h.log.Warn("plugin host call failed", "err", err)
		writeHostError(w, http.StatusInternalServerError, "error", "The host call failed")
	}
	return false
}

// cleanMessage strips the sentinel prefix from an ErrInvalid so the
// plugin sees the specific reason ("path escapes...") only.
func cleanMessage(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "remote: invalid request: ")
	msg = strings.TrimPrefix(msg, "remote: invalid request")
	if msg == "" {
		return "The request is not valid"
	}
	return msg
}

// pageParams reads limit/offset, or page/pageSize, clamped.
func pageParams(r *http.Request) (limit, offset int) {
	q := r.URL.Query()
	limit = defaultPageLimit
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if v, err := paging.ParseOffset(q.Get("offset"), limit); err == nil && v >= 0 {
		offset = v
	}
	if q.Get("page") != "" {
		pageSize := limit
		if v, err := strconv.Atoi(q.Get("pageSize")); err == nil && v > 0 {
			pageSize = v
		}
		if v, err := strconv.Atoi(q.Get("page")); err == nil && v >= 1 {
			limit = pageSize
			offset = paging.PageOffset(v, pageSize)
		}
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	return limit, offset
}

func readBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > max {
		return nil, ErrTooLarge
	}
	return raw, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	httpjson.Write(w, status, v)
}

// writeHostError emits the EPP error shape (SPEC §5.1.2).
func writeHostError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// dataCall exposes only bounded blob/archive services, authenticated like all
// other plugin-host calls. HostAdapter performs the live grant checks.
func (h *HostHandler) dataCall(w http.ResponseWriter, r *http.Request) {
	var args json.RawMessage
	if !h.decode(w, r, maxHostBodyBytes, &args) {
		return
	}
	service, ok := h.host.(interface {
		CallDataHost(context.Context, string, string, json.RawMessage) (json.RawMessage, error)
	})
	if !ok {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "Data services unavailable")
		return
	}
	name := r.PathValue("service")
	ctx := r.Context()
	if name == "cache.stat" && strings.TrimSpace(r.Header.Get(HeaderViewerToken)) != "" {
		ctx = plugins.WithCallContext(ctx, "/ether/v1/frontend/", "")
	}
	out, err := service.CallDataHost(ctx, pluginIDOf(r), name, args)
	if !h.finishErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}
