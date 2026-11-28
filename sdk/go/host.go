package etherplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultFetchManyConcurrency and MaxFetchManyConcurrency bound the
	// per-call parallelism of Host.FetchMany.
	DefaultFetchManyConcurrency = 8
	MaxFetchManyConcurrency     = 8
	// MaxFetchManyRequests caps the number of requests per FetchMany call.
	MaxFetchManyRequests = 64
	// DefaultMaxFetchPerHost bounds concurrent requests to any single origin.
	DefaultMaxFetchPerHost = 6
)

// HostFetchRequest describes an outbound HTTP call via the host service.
type HostFetchRequest struct {
	TimeoutMS             int64             `json:"timeoutMs,omitempty"`
	Method                string            `json:"method,omitempty"`
	URL                   string            `json:"url"`
	Headers               map[string]string `json:"headers,omitempty"`
	Body                  string            `json:"body,omitempty"`
	BodyBase64            string            `json:"bodyBase64,omitempty"`
	KeepHeadersOnRedirect []string          `json:"keepHeadersOnRedirect,omitempty"`
}

// HostFetchResponse is returned by Host.Fetch.
type HostFetchResponse struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	BodyBase64 string            `json:"bodyBase64,omitempty"`
}

// HostFetchManyOptions configures a bounded concurrent FetchMany batch.
type HostFetchManyOptions struct {
	Concurrency int `json:"concurrency,omitempty"`
}

// HostFetchError describes a per-item error returned by FetchMany.
type HostFetchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// HostFetchManyResult is one ordered result from FetchMany. Either Error is
// non-nil or Status/Headers/Body carry the HTTP response (including non-2xx).
type HostFetchManyResult struct {
	Status     int               `json:"status,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	BodyBase64 string            `json:"bodyBase64,omitempty"`
	Error      *HostFetchError   `json:"error,omitempty"`
}

// FolderHashJob is a pollable background hash. Bytes is the progress so far.
// Terminal states are done, canceled and error. Only done carries Result.
type FolderHashJob struct {
	ID     string      `json:"id"`
	State  string      `json:"state"`
	Bytes  int64       `json:"bytes"`
	Result *FolderHash `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// FolderHash contains digests, never file contents. ModTime is Unix seconds.
type FolderHash struct {
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
	CRC32   string `json:"crc32,omitempty"`
	MD5     string `json:"md5,omitempty"`
	SHA1    string `json:"sha1,omitempty"`
	OSHash  string `json:"oshash,omitempty"`
	PHash   string `json:"phash,omitempty"`
}

// FolderHashOptions limits which digests are computed. Phash requires a background hash job.
type FolderHashOptions struct {
	Algorithms []string `json:"algorithms,omitempty"`
}

// FolderFrameOptions selects a timestamp, image width and optional owned cache library.
type FolderFrameOptions struct {
	AtSec      float64 `json:"atSec"`
	Width      int     `json:"width"`
	LibraryKey string  `json:"libraryKey,omitempty"`
}

// FolderFrame identifies a cached PNG image.
type FolderFrame struct {
	Ref        string `json:"ref"`
	LibraryKey string `json:"libraryKey"`
}

// FolderListOptions selects names strictly after Cursor in ascending name order.
type FolderListOptions struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// FolderPage is a name-sorted page. NextCursor is empty at the end.
type FolderPage struct {
	Entries    []FolderEntry `json:"entries"`
	NextCursor string        `json:"nextCursor"`
	Truncated  bool          `json:"truncated"`
}

// FolderEntry describes one entry returned by Host.FoldersList or Host.FoldersStat.
type FolderEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size,omitempty"`
	Mtime int64  `json:"mtime,omitempty"`
}

// CatalogTitleSummary is a catalog record returned by Host.CatalogSearch or Host.CatalogGet.
type CatalogTitleSummary struct {
	ID     any    `json:"id"`
	Title  string `json:"title"`
	Type   string `json:"type"`
	Year   int    `json:"year,omitempty"`
	IMDBID string `json:"imdbId,omitempty"`
	TMDBID int    `json:"tmdbId,omitempty"`
}

// ViewerWatchEntry is one watch-history entry returned by Host.ViewerHistory.
type ViewerWatchEntry struct {
	TitleID   string `json:"titleId"`
	Title     string `json:"title,omitempty"`
	WatchedAt string `json:"watchedAt,omitempty"`
	Completed bool   `json:"completed,omitempty"`
}

// ViewerWatchEventPayload is the payload of viewer.watch.scrobbled,
// viewer.watch.watched, viewer.watch.unwatched and viewer.watch.progress.
type ViewerWatchEventPayload struct {
	ViewerID   int64  `json:"viewerId,omitempty"`
	Username   string `json:"username,omitempty"`
	TitleKey   string `json:"titleKey"`
	TitleName  string `json:"titleName"`
	MediaType  string `json:"mediaType"`
	Watched    bool   `json:"watched"`
	State      string `json:"state"`  // watched, unwatched, or progress
	Source     string `json:"source"` // playback, manual, plugin, or import
	Surface    string `json:"surface,omitempty"`
	ItemRef    string `json:"itemRef,omitempty"`
	TMDBID     int64  `json:"tmdbId,omitempty"`
	TVDBID     int64  `json:"tvdbId,omitempty"`
	IMDBID     string `json:"imdbId,omitempty"`
	Season     *int   `json:"season,omitempty"`
	Episode    *int   `json:"episode,omitempty"`
	PositionMS *int64 `json:"positionMs,omitempty"`
}

// Host provides access to Ether host services (KV, secrets, HTTP, catalog,
// viewers, collections, folders, owner notifications, and logging).
type Host interface {
	KVGet(ctx context.Context, key string) (string, bool, error)
	KVPut(ctx context.Context, key, value string) error
	KVDelete(ctx context.Context, key string) error
	KVList(ctx context.Context, prefix string) ([]string, error)
	SecretGet(ctx context.Context, name string) (string, error)
	Fetch(ctx context.Context, req HostFetchRequest) (HostFetchResponse, error)
	CatalogSearch(ctx context.Context, query string, limit, offset int) ([]CatalogTitleSummary, error)
	CatalogGet(ctx context.Context, id string) (*CatalogTitleSummary, error)
	ViewerHistory(ctx context.Context, viewerID int64, limit, offset int) ([]ViewerWatchEntry, error)
	// titleID accepts a catalogue movie/episode ID or an owned playable item ref.
	SetWatchState(ctx context.Context, viewerID int64, titleID string, watched bool) error
	UpsertCollection(ctx context.Context, collection map[string]any) error
	FoldersRoots(ctx context.Context) (FolderRoots, error)
	FoldersList(ctx context.Context, root, path string, options ...FolderListOptions) (FolderPage, error)
	FoldersStat(ctx context.Context, root, path string) (FolderEntry, error)
	FoldersHash(ctx context.Context, root, path string, options ...FolderHashOptions) (FolderHash, error)
	FoldersHashStart(ctx context.Context, root, path string, options ...FolderHashOptions) (FolderHashJob, error)
	FoldersFrame(ctx context.Context, root, path string, options FolderFrameOptions) (FolderFrame, error)
	FoldersHashPoll(ctx context.Context, id string) (FolderHashJob, error)
	FoldersHashCancel(ctx context.Context, id string) (FolderHashJob, error)
	NotifyOwner(ctx context.Context, text string) error
	Log(ctx context.Context, level, message string, attrs map[string]any) error
	// Content-kind host services. KindUpsert/KindDelete need a push-mode
	// kind; ResolveSources needs permissions.sources.
	KindUpsert(ctx context.Context, kindKey string, items []KindItem) (accepted int, err error)
	KindDelete(ctx context.Context, kindKey string, externalIDs []string) error
	KindItems(ctx context.Context, kindKey, cursor string, limit int) (KindPage, error)
	ResolveSources(ctx context.Context, q SourceQuery) ([]SourceCandidate, error)
	CachePut(ctx context.Context, libraryKey, name string, data []byte) (blobRef string, err error)
	CacheOpen(ctx context.Context, blobRef string) ([]byte, error)
	// StreamURL mints a short-lived, scoped ticket for one of this plugin's
	// own kind items and returns the Ether stream URL to embed in a
	// frontend adapter response (spec §6.5). viewerID is the viewer the
	// adapter authenticated.
	StreamURL(ctx context.Context, viewerID int64, itemRef string) (string, error)
}

// KVUsage reports a plugin's current KV bytes (including the 256-byte charge
// per key), key count, and configured quotas (#591).
type KVUsage struct {
	Bytes      int64 `json:"bytes"`
	Keys       int64 `json:"keys"`
	QuotaBytes int64 `json:"quotaBytes"`
	QuotaKeys  int64 `json:"quotaKeys"`
}

// KVUsageHost reports current KV usage and quotas (#591).
// RemoteHostClient and MockHost implement it.
type KVUsageHost interface {
	KVUsage(ctx context.Context) (KVUsage, error)
}

// FetchManyHost executes up to MaxFetchManyRequests outbound HTTP calls
// concurrently with bounded per-call and per-origin concurrency, returning
// ordered results with per-item error isolation (#703).
// RemoteHostClient and MockHost implement it.
type FetchManyHost interface {
	FetchMany(ctx context.Context, reqs []HostFetchRequest, opts ...HostFetchManyOptions) ([]HostFetchManyResult, error)
}

// SecretWriteHost stores and removes sealed derived secrets declared in
// permissions.secrets (#660). Owner-configured settings secrets cannot be
// overwritten. RemoteHostClient and MockHost implement it.
type SecretWriteHost interface {
	SecretSet(ctx context.Context, name, value string, ttlSec int64) error
	SecretDelete(ctx context.Context, name string) error
}

// BadgesHost pushes, clears, and inspects plugin-owned poster badges (#594).
// RemoteHostClient and MockHost implement it.
type BadgesHost interface {
	BadgesSet(ctx context.Context, setKey string, entries []BadgeEntry) error
	BadgesClear(ctx context.Context, setKey string) error
	BadgesUsage(ctx context.Context) (BadgeUsage, error)
}

// EventsHost emits declared plugin events onto Ether's event bus.
// RemoteHostClient and MockHost implement it; Context.PublishEvent uses it automatically.
type EventsHost interface {
	PublishEvent(ctx context.Context, key string, payload any, corr *EventCorrelation) error
}

// AIMessage is one chat turn sent to AIHost.AIComplete.
type AIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// AICompleteRequest configures one completion on the owner's configured
// language model (#669).
type AICompleteRequest struct {
	Messages []AIMessage `json:"messages"`
	// Schema is an optional JSON Schema object (raw JSON or map[string]any
	// marshalled to JSON). When set, Ether extracts JSON from the model reply
	// and validates it against Schema before returning. External $ref URLs are
	// rejected.
	Schema json.RawMessage `json:"schema,omitempty"`
	// MaxTokens bounds output tokens (default 1024, max 4096).
	MaxTokens int `json:"maxTokens,omitempty"`
	// Temperature optionally adjusts sampling in [0, 2]; ignored safely by
	// providers/models that disallow explicit temperature.
	Temperature *float64 `json:"temperature,omitempty"`
	// TimeoutMS optionally tightens the call timeout (clamped to 30000 ms on
	// background/action routes, 20000 ms on interactive search routes, and the
	// enclosing call deadline).
	TimeoutMS int64 `json:"timeoutMs,omitempty"`
	// SearchQuery identifies the viewer's search text when calling ai.complete
	// on an interactive search route under permissions.ai: "complete+search".
	// Required on adapter search routes without a URL query param (e.g. GraphQL)
	// and used for normalized per-query caching.
	SearchQuery string `json:"searchQuery,omitempty"`
}

// AICompleteResponse is the completion returned by AIHost.AIComplete.
type AICompleteResponse struct {
	// Text is the assistant reply (with reasoning blocks stripped). When Schema
	// was set, Text holds compact canonical JSON.
	Text string `json:"text"`
	// JSON holds the validated JSON value when Schema was set.
	JSON json.RawMessage `json:"json,omitempty"`
}

// HostError is a structured error returned by Ether's plugin-host API,
// preserving the EPP error code (for example "not_configured", "rate_limited",
// "permission_denied", or "invalid_output") alongside the human message and
// optional retry window when rate limited.
type HostError struct {
	Status        int
	Code          string
	Message       string
	RetryAfterSec int64
	RetryAt       string
}

func (e *HostError) Error() string { return e.Message }

// AIHost runs bounded completions on the owner's configured language model
// (requires permissions.ai: "complete", or "complete+search" for opted-in
// interactive search routes; every other viewer/client path stays refused).
// RemoteHostClient and MockHost implement it; Context.AIComplete uses it
// automatically.
type AIHost interface {
	AIComplete(ctx context.Context, req AICompleteRequest) (AICompleteResponse, error)
}

// MediaHost probes container metadata for a PlayableSource in the same shape
// /resolve returns, reusing Ether's durable release_probes cache (requires
// permissions.media: "probe"; jobs, actions, and background routes only).
// RemoteHostClient and MockHost implement it; Context.MediaProbe uses it
// automatically.
type MediaHost interface {
	MediaProbe(ctx context.Context, src PlayableSource) (MediaProbeResult, error)
}

// PartStreamHost mints a short-lived, scoped ticket for a specific zero-based
// part of a multi-file kind item (#576). RemoteHostClient and MockHost
// implement it; Context.StreamPartURL uses it automatically.
type PartStreamHost interface {
	StreamPartURL(ctx context.Context, viewerID int64, itemRef string, partIndex int) (string, error)
}

// StreamURLOptionsHost mints a short-lived, scoped ticket for a specific
// zero-based part and/or resolver PlayableSource.ID of a kind item (#576, #661).
// RemoteHostClient and MockHost implement it; Context.StreamURLWithOptions
// uses it automatically.
type StreamURLOptionsHost interface {
	StreamURLWithOptions(ctx context.Context, viewerID int64, itemRef string, opts StreamURLOptions) (string, error)
}

// ResolveSourcesResultHost runs sources.resolve and returns both the candidate
// list and per-indexer search statuses for indexer_search queries (#733).
// RemoteHostClient and MockHost implement it; Context.ResolveSourcesWithResult
// uses it automatically.
type ResolveSourcesResultHost interface {
	ResolveSourcesWithResult(ctx context.Context, q SourceQuery) (SourceResolveResult, error)
}

// CacheDeleteHost deletes cached blobs from this plugin's own library cache
// namespaces. RemoteHostClient and MockHost implement it.
type CacheDeleteHost interface {
	CacheDelete(ctx context.Context, blobRef string) error
}

const (
	// MaxKindVisibleIDs is the per-call batch cap for kinds.visible (#615).
	MaxKindVisibleIDs = 500
	// HeaderViewerToken carries the host-minted, request-scoped adapter viewer
	// proof header on frontend adapter calls (#615).
	HeaderViewerToken = "X-Ether-Viewer-Token"
	// HeaderOrigin carries the client-visible origin
	// ("scheme://host[:port][/prefix]") injected by Ether on authenticated
	// frontend adapter calls (#772).
	HeaderOrigin = "X-Ether-Origin"
)

type viewerTokenContextKey struct{}

// WithViewerToken attaches the host-minted X-Ether-Viewer-Token from an
// incoming frontend adapter request onto ctx so KindVisible and KindBrowse
// authenticate as that request's viewer without passing a viewer ID (#615).
func WithViewerToken(ctx context.Context, token string) context.Context {
	token = strings.TrimSpace(token)
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, viewerTokenContextKey{}, token)
}

// ViewerTokenFromContext returns the adapter viewer token attached by
// WithViewerToken (or automatically during FrontendAdapter dispatch).
func ViewerTokenFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	tok, ok := ctx.Value(viewerTokenContextKey{}).(string)
	return tok, ok && tok != ""
}

// KindPolicyHost evaluates a plugin library's Rules and viewer access policy
// for the requesting frontend-adapter viewer (#615). RemoteHostClient and
// MockHost implement it. The viewer identity comes solely from the request's
// authenticated X-Ether-Viewer-Token on ctx, never from caller-supplied IDs.
type KindPolicyHost interface {
	KindVisible(ctx context.Context, kindKey string, ids []string) ([]string, error)
	KindBrowse(ctx context.Context, kindKey string, query KindBrowseQuery) (KindPage, error)
}

// RemoteHostOptions configures a RemoteHostClient for sidecar plugins.
type RemoteHostOptions struct {
	BaseURL    string
	Credential string
	HTTPClient *http.Client
}

// RemoteHostClient implements Host by calling `/ether/plugin-host/*` on the
// Ether server authenticated with `X-Ether-Plugin-Credential`.
type RemoteHostClient struct {
	baseURL    string
	credential string
	client     *http.Client
}

// NewRemoteHostClient creates a RemoteHostClient.
func NewRemoteHostClient(opts RemoteHostOptions) *RemoteHostClient {
	base := opts.BaseURL
	if base == "" {
		base = os.Getenv("ETHER_HOST_URL")
	}
	if base == "" {
		base = "http://127.0.0.1:32450"
	}
	base = strings.TrimRight(base, "/")

	cred := opts.Credential
	if cred == "" {
		cred = os.Getenv("ETHER_PLUGIN_CREDENTIAL")
	}

	hc := opts.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	return &RemoteHostClient{
		baseURL:    base,
		credential: cred,
		client:     hc,
	}
}

// SettingsGet reads the current non-secret settings from Ether.
func (c *RemoteHostClient) SettingsGet(ctx context.Context) (map[string]any, error) {
	var result struct {
		Settings map[string]any `json:"settings"`
	}
	_, err := c.callJSON(ctx, http.MethodGet, "/ether/plugin-host/settings", nil, &result, false)
	return result.Settings, err
}

func (c *RemoteHostClient) callJSON(
	ctx context.Context,
	method, path string,
	in any,
	out any,
	allow404 bool,
) (bool, error) {
	return c.callJSONWithHeaders(ctx, method, path, nil, in, out, allow404)
}

func (c *RemoteHostClient) callJSONWithHeaders(
	ctx context.Context,
	method, path string,
	extraHeaders map[string]string,
	in any,
	out any,
	allow404 bool,
) (bool, error) {
	var bodyReader io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return false, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-Ether-Plugin-Credential", c.credential)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extraHeaders {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close() //nolint:errcheck

	raw, err := io.ReadAll(io.LimitReader(resp.Body, (MaxDataReadBytes+2)/3*4+(4<<20)))
	if err != nil {
		return false, err
	}
	if allow404 && resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errDoc struct {
			Error struct {
				Code          string `json:"code"`
				Message       string `json:"message"`
				RetryAfterSec int64  `json:"retryAfterSec"`
				RetryAt       string `json:"retryAt"`
			} `json:"error"`
		}
		retryAfterSec := int64(0)
		if hdr := strings.TrimSpace(resp.Header.Get("Retry-After")); hdr != "" {
			if n, perr := strconv.ParseInt(hdr, 10, 64); perr == nil && n > 0 {
				retryAfterSec = n
			}
		}
		if json.Unmarshal(raw, &errDoc) == nil && (errDoc.Error.Message != "" || errDoc.Error.Code != "") {
			msg := errDoc.Error.Message
			if msg == "" {
				msg = fmt.Sprintf("host call %s %s failed with status %d", method, path, resp.StatusCode)
			}
			if errDoc.Error.RetryAfterSec > 0 {
				retryAfterSec = errDoc.Error.RetryAfterSec
			}
			return false, &HostError{
				Status:        resp.StatusCode,
				Code:          errDoc.Error.Code,
				Message:       msg,
				RetryAfterSec: retryAfterSec,
				RetryAt:       errDoc.Error.RetryAt,
			}
		}
		return false, &HostError{
			Status:        resp.StatusCode,
			Message:       fmt.Sprintf("host call %s %s failed with status %d", method, path, resp.StatusCode),
			RetryAfterSec: retryAfterSec,
		}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return false, err
		}
	}
	return true, nil
}

// KVGet reads a key from the plugin's KV namespace.
func (c *RemoteHostClient) KVGet(ctx context.Context, key string) (string, bool, error) {
	var res struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	found, err := c.callJSON(ctx, http.MethodGet, "/ether/plugin-host/kv/"+url.PathEscape(key), nil, &res, true)
	if err != nil || !found {
		return "", false, err
	}
	return res.Value, true, nil
}

// KVPut writes a key-value pair to the plugin's KV namespace.
func (c *RemoteHostClient) KVPut(ctx context.Context, key, value string) error {
	_, err := c.callJSON(
		ctx,
		http.MethodPut,
		"/ether/plugin-host/kv/"+url.PathEscape(key),
		map[string]string{"value": value},
		nil,
		false,
	)
	return err
}

// KVDelete deletes a key from the plugin's KV namespace.
func (c *RemoteHostClient) KVDelete(ctx context.Context, key string) error {
	_, err := c.callJSON(ctx, http.MethodDelete, "/ether/plugin-host/kv/"+url.PathEscape(key), nil, nil, false)
	return err
}

// KVList lists keys matching the given prefix in the plugin's KV namespace.
func (c *RemoteHostClient) KVList(ctx context.Context, prefix string) ([]string, error) {
	path := "/ether/plugin-host/kv"
	if prefix != "" {
		path += "?prefix=" + url.QueryEscape(prefix)
	}
	var res struct {
		Keys []string `json:"keys"`
	}
	if _, err := c.callJSON(ctx, http.MethodGet, path, nil, &res, false); err != nil {
		return nil, err
	}
	return res.Keys, nil
}

// KVUsage reads the plugin's current KV byte/key usage and quotas.
func (c *RemoteHostClient) KVUsage(ctx context.Context) (KVUsage, error) {
	var res KVUsage
	if _, err := c.callJSON(ctx, http.MethodGet, "/ether/plugin-host/kv-usage", nil, &res, false); err != nil {
		return KVUsage{}, err
	}
	return res, nil
}

// BadgesSet pushes poster badges for one declared badge set (#594).
func (c *RemoteHostClient) BadgesSet(ctx context.Context, setKey string, entries []BadgeEntry) error {
	if entries == nil {
		entries = []BadgeEntry{}
	}
	_, err := c.callJSON(
		ctx,
		http.MethodPut,
		"/ether/plugin-host/badges/"+url.PathEscape(setKey),
		map[string]any{"entries": entries},
		nil,
		false,
	)
	return err
}

// BadgesClear removes every badge in one declared set (#594).
func (c *RemoteHostClient) BadgesClear(ctx context.Context, setKey string) error {
	_, err := c.callJSON(ctx, http.MethodDelete, "/ether/plugin-host/badges/"+url.PathEscape(setKey), nil, nil, false)
	return err
}

// BadgesUsage reads the plugin's current badged title count and quota (#594).
func (c *RemoteHostClient) BadgesUsage(ctx context.Context) (BadgeUsage, error) {
	var res BadgeUsage
	if _, err := c.callJSON(ctx, http.MethodGet, "/ether/plugin-host/badges-usage", nil, &res, false); err != nil {
		return BadgeUsage{}, err
	}
	return res, nil
}

// SecretGet is only available in hosted mode; remote plugins receive secrets via /ether/configure.
func (c *RemoteHostClient) SecretGet(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("secret.get is only available in hosted plugins; remote plugins receive secrets via /ether/configure")
}

// SecretSet stores a sealed derived secret under a name declared in
// permissions.secrets (#660). Passing an empty value deletes it.
func (c *RemoteHostClient) SecretSet(ctx context.Context, name, value string, ttlSec int64) error {
	payload := map[string]any{"value": value}
	if ttlSec != 0 {
		payload["ttlSec"] = ttlSec
	}
	_, err := c.callJSON(ctx, http.MethodPut, "/ether/plugin-host/secrets/"+url.PathEscape(name), payload, nil, false)
	return err
}

// SecretDelete removes a plugin-written derived secret declared in
// permissions.secrets (#660).
func (c *RemoteHostClient) SecretDelete(ctx context.Context, name string) error {
	_, err := c.callJSON(ctx, http.MethodDelete, "/ether/plugin-host/secrets/"+url.PathEscape(name), nil, nil, false)
	return err
}

// Fetch performs an outbound HTTP request.
func (c *RemoteHostClient) Fetch(ctx context.Context, r HostFetchRequest) (HostFetchResponse, error) {
	if r.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(min(r.TimeoutMS, int64(86400000)))*time.Millisecond)
		defer cancel()
	}
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	var bodyReader io.Reader
	if r.Body != "" {
		bodyReader = strings.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.URL, bodyReader)
	if err != nil {
		return HostFetchResponse{}, err
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return HostFetchResponse{}, err
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return HostFetchResponse{}, err
	}
	hdrs := map[string]string{}
	for k, vals := range resp.Header {
		if len(vals) > 0 {
			hdrs[strings.ToLower(k)] = vals[0]
		}
	}
	return HostFetchResponse{
		Status:  resp.StatusCode,
		Headers: hdrs,
		Body:    string(raw),
	}, nil
}

// FetchMany runs up to MaxFetchManyRequests outbound HTTP calls concurrently
// (bounded to MaxFetchManyConcurrency per batch and DefaultMaxFetchPerHost per
// origin), returning ordered results with per-item error isolation.
func (c *RemoteHostClient) FetchMany(ctx context.Context, reqs []HostFetchRequest, opts ...HostFetchManyOptions) ([]HostFetchManyResult, error) {
	var opt HostFetchManyOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	return runSDKFetchMany(ctx, reqs, opt, c.Fetch)
}

func sdkFetchOriginKey(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

func runSDKFetchMany(
	ctx context.Context,
	reqs []HostFetchRequest,
	opts HostFetchManyOptions,
	doOne func(context.Context, HostFetchRequest) (HostFetchResponse, error),
) ([]HostFetchManyResult, error) {
	if len(reqs) > MaxFetchManyRequests {
		return nil, fmt.Errorf("http.fetchMany accepts at most %d requests per call", MaxFetchManyRequests)
	}
	if opts.Concurrency < 0 {
		return nil, fmt.Errorf("http.fetchMany concurrency must be a non-negative integer")
	}
	if len(reqs) == 0 {
		return []HostFetchManyResult{}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultFetchManyConcurrency
	}
	if concurrency > MaxFetchManyConcurrency {
		concurrency = MaxFetchManyConcurrency
	}
	if concurrency > len(reqs) {
		concurrency = len(reqs)
	}

	var keys []string
	buckets := make(map[string][]int)
	for i, req := range reqs {
		k := sdkFetchOriginKey(req.URL)
		if _, ok := buckets[k]; !ok {
			keys = append(keys, k)
		}
		buckets[k] = append(buckets[k], i)
	}
	jobs := make(chan int, len(reqs))
	emitted := 0
	for emitted < len(reqs) {
		for _, k := range keys {
			q := buckets[k]
			if len(q) == 0 {
				continue
			}
			jobs <- q[0]
			buckets[k] = q[1:]
			emitted++
		}
	}
	close(jobs)

	var gateMu sync.Mutex
	gates := make(map[string]chan struct{})
	acquireOrigin := func(c context.Context, rawURL string) (func(), error) {
		k := sdkFetchOriginKey(rawURL)
		if k == "" {
			return func() {}, nil
		}
		gateMu.Lock()
		sem, ok := gates[k]
		if !ok {
			sem = make(chan struct{}, DefaultMaxFetchPerHost)
			gates[k] = sem
		}
		gateMu.Unlock()
		select {
		case sem <- struct{}{}:
			return func() { <-sem }, nil
		case <-c.Done():
			return nil, c.Err()
		}
	}

	results := make([]HostFetchManyResult, len(reqs))
	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					return
				}
				release, aerr := acquireOrigin(ctx, reqs[idx].URL)
				if aerr != nil {
					return
				}
				resp, err := doOne(ctx, reqs[idx])
				release()
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					code := "host_error"
					if errors.Is(err, context.DeadlineExceeded) {
						code = "timeout"
					}
					results[idx] = HostFetchManyResult{Error: &HostFetchError{Code: code, Message: err.Error()}}
					continue
				}
				results[idx] = HostFetchManyResult{
					Status:     resp.Status,
					Headers:    resp.Headers,
					Body:       resp.Body,
					BodyBase64: resp.BodyBase64,
				}
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// CatalogSearch searches the Ether catalog.
func (c *RemoteHostClient) CatalogSearch(
	ctx context.Context,
	query string,
	limit, offset int,
) ([]CatalogTitleSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("limit", strconv.Itoa(limit))
	q.Set("offset", strconv.Itoa(offset))
	var res struct {
		Titles []CatalogTitleSummary `json:"titles"`
	}
	if _, err := c.callJSON(ctx, http.MethodGet, "/ether/plugin-host/catalog/search?"+q.Encode(), nil, &res, false); err != nil {
		return nil, err
	}
	return res.Titles, nil
}

// CatalogGet fetches one title summary from the Ether catalog.
func (c *RemoteHostClient) CatalogGet(ctx context.Context, id string) (*CatalogTitleSummary, error) {
	var res CatalogTitleSummary
	found, err := c.callJSON(ctx, http.MethodGet, "/ether/plugin-host/catalog/"+url.PathEscape(id), nil, &res, true)
	if err != nil || !found {
		return nil, err
	}
	return &res, nil
}

// ViewerHistory reads watch history for a granted viewer.
func (c *RemoteHostClient) ViewerHistory(
	ctx context.Context,
	viewerID int64,
	limit, offset int,
) ([]ViewerWatchEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	q.Set("offset", strconv.Itoa(offset))
	path := fmt.Sprintf("/ether/plugin-host/viewers/%d/history?%s", viewerID, q.Encode())
	var out []ViewerWatchEntry
	if _, err := c.callJSON(ctx, http.MethodGet, path, nil, &out, false); err != nil {
		return nil, err
	}
	return out, nil
}

// SetWatchState updates the watch state for a granted viewer.
func (c *RemoteHostClient) SetWatchState(
	ctx context.Context,
	viewerID int64,
	titleID string,
	watched bool,
) error {
	_, err := c.callJSON(
		ctx,
		http.MethodPost,
		"/ether/plugin-host/viewers/watch-state",
		map[string]any{
			"viewerId": viewerID,
			"titleId":  titleID,
			"watched":  watched,
		},
		nil,
		false,
	)
	return err
}

// UpsertCollection creates or updates a plugin-managed collection.
func (c *RemoteHostClient) UpsertCollection(ctx context.Context, collection map[string]any) error {
	_, err := c.callJSON(ctx, http.MethodPost, "/ether/plugin-host/collections/upsert", collection, nil, false)
	return err
}

// FoldersList is only supported in hosted mode; sidecars access local folders directly.
func (c *RemoteHostClient) FoldersRoots(context.Context) (FolderRoots, error) {
	return nil, fmt.Errorf("folders.roots requires the hosted runtime")
}

func (c *RemoteHostClient) FoldersList(_ context.Context, _, _ string, _ ...FolderListOptions) (FolderPage, error) {
	return FolderPage{}, fmt.Errorf("folders.list is for hosted plugins; sidecar plugins read their local fileRoot directly")
}

// FoldersStat is only supported in hosted mode; sidecars access local folders directly.
func (c *RemoteHostClient) FoldersStat(_ context.Context, _, _ string) (FolderEntry, error) {
	return FolderEntry{}, fmt.Errorf("folders.stat is for hosted plugins; sidecar plugins read their local fileRoot directly")
}

// HeaderIdempotencyKey carries the Flow step idempotency key on plugin flow action calls.
const HeaderIdempotencyKey = "X-Ether-Idempotency-Key"

// PublishEvent emits a declared plugin event ("plugin:<id>/<key>") onto Ether's
// event bus through POST /ether/plugin-host/events/publish.
func (c *RemoteHostClient) PublishEvent(ctx context.Context, key string, payload any, corr *EventCorrelation) error {
	if payload == nil {
		payload = map[string]any{}
	}
	body := map[string]any{
		"key":     key,
		"payload": payload,
	}
	if corr != nil {
		body["correlation"] = corr
	}
	_, err := c.callJSON(ctx, http.MethodPost, "/ether/plugin-host/events/publish", body, nil, false)
	return err
}

// AIComplete runs a bounded text or JSON-Schema-validated completion against
// the owner's configured language model through POST /ether/plugin-host/ai/complete.
// Requires permissions.ai: "complete" (or "complete+search" on interactive search
// routes, forwarding X-Ether-Viewer-Token when present on ctx).
// When no model is configured, returns a *HostError with Code == "not_configured".
func (c *RemoteHostClient) AIComplete(ctx context.Context, req AICompleteRequest) (AICompleteResponse, error) {
	var res AICompleteResponse
	var headers map[string]string
	if tok, ok := ViewerTokenFromContext(ctx); ok && tok != "" {
		headers = map[string]string{HeaderViewerToken: tok}
	}
	_, err := c.callJSONWithHeaders(ctx, http.MethodPost, "/ether/plugin-host/ai/complete", headers, req, &res, false)
	return res, err
}

// MediaProbe probes container metadata for a PlayableSource through
// POST /ether/plugin-host/media/probe. Requires permissions.media: "probe" and
// may only be called from jobs, actions, and background routes (never viewer
// requests; forwarding X-Ether-Viewer-Token is refused by the host).
func (c *RemoteHostClient) MediaProbe(ctx context.Context, src PlayableSource) (MediaProbeResult, error) {
	var res MediaProbeResult
	var headers map[string]string
	if tok, ok := ViewerTokenFromContext(ctx); ok && tok != "" {
		headers = map[string]string{HeaderViewerToken: tok}
	}
	_, err := c.callJSONWithHeaders(ctx, http.MethodPost, "/ether/plugin-host/media/probe", headers, src, &res, false)
	return res, err
}

// NotifyOwner sends an owner notification through Ether.
func (c *RemoteHostClient) NotifyOwner(ctx context.Context, text string) error {
	_, err := c.callJSON(
		ctx,
		http.MethodPost,
		"/ether/plugin-host/notify",
		map[string]string{"text": text},
		nil,
		false,
	)
	return err
}

// Log sends a structured log entry to Ether.
func (c *RemoteHostClient) Log(ctx context.Context, level, message string, attrs map[string]any) error {
	payload := map[string]any{
		"level":   level,
		"message": message,
	}
	if attrs != nil {
		payload["attrs"] = attrs
	}
	_, err := c.callJSON(ctx, http.MethodPost, "/ether/plugin-host/log", payload, nil, false)
	return err
}

// KindUpsert pushes items into a push-mode kind library.
func (c *RemoteHostClient) KindUpsert(ctx context.Context, kindKey string, items []KindItem) (int, error) {
	var out struct {
		Accepted int `json:"accepted"`
	}
	_, err := c.callJSON(ctx, http.MethodPost, "/ether/plugin-host/kinds/"+url.PathEscape(kindKey)+"/upsert",
		map[string]any{"items": items}, &out, false)
	return out.Accepted, err
}

// KindDelete removes items from a push-mode kind library.
func (c *RemoteHostClient) KindDelete(ctx context.Context, kindKey string, externalIDs []string) error {
	_, err := c.callJSON(ctx, http.MethodPost, "/ether/plugin-host/kinds/"+url.PathEscape(kindKey)+"/delete",
		map[string]any{"externalIds": externalIDs}, nil, false)
	return err
}

// KindItems reads the plugin's own kind library back, keyset-paged.
func (c *RemoteHostClient) KindItems(ctx context.Context, kindKey, cursor string, limit int) (KindPage, error) {
	path := "/ether/plugin-host/kinds/" + url.PathEscape(kindKey) + "/items?cursor=" + url.QueryEscape(cursor)
	if limit > 0 {
		path += "&limit=" + strconv.Itoa(limit)
	}
	var page KindPage
	_, err := c.callJSON(ctx, http.MethodGet, path, nil, &page, false)
	return page, err
}

// KindVisible evaluates the library's Rules and viewer access policy for a
// bounded batch (at most MaxKindVisibleIDs) of external IDs in kindKey (#615).
// ctx must carry the incoming frontend adapter's X-Ether-Viewer-Token (via
// WithViewerToken, attached automatically by FrontendAdapter dispatch).
func (c *RemoteHostClient) KindVisible(ctx context.Context, kindKey string, ids []string) ([]string, error) {
	tok, ok := ViewerTokenFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("kinds.visible: no authenticated frontend adapter viewer token on context")
	}
	if len(ids) > MaxKindVisibleIDs {
		return nil, fmt.Errorf("kinds.visible: batch larger than %d ids", MaxKindVisibleIDs)
	}
	var out struct {
		IDs []string `json:"ids"`
	}
	_, err := c.callJSONWithHeaders(
		ctx,
		http.MethodPost,
		"/ether/plugin-host/kinds/"+url.PathEscape(kindKey)+"/visible",
		map[string]string{HeaderViewerToken: tok},
		map[string]any{"ids": ids},
		&out,
		false,
	)
	if err != nil {
		return nil, err
	}
	if out.IDs == nil {
		out.IDs = []string{}
	}
	return out.IDs, nil
}

// KindBrowse browses or searches kindKey with the library's Rules and viewer
// access policy applied for the requesting frontend-adapter viewer (#615).
// ctx must carry the incoming frontend adapter's X-Ether-Viewer-Token (via
// WithViewerToken, attached automatically by FrontendAdapter dispatch).
func (c *RemoteHostClient) KindBrowse(ctx context.Context, kindKey string, query KindBrowseQuery) (KindPage, error) {
	tok, ok := ViewerTokenFromContext(ctx)
	if !ok {
		return KindPage{}, fmt.Errorf("kinds.browse: no authenticated frontend adapter viewer token on context")
	}
	var page KindPage
	_, err := c.callJSONWithHeaders(
		ctx,
		http.MethodPost,
		"/ether/plugin-host/kinds/"+url.PathEscape(kindKey)+"/browse",
		map[string]string{HeaderViewerToken: tok},
		query,
		&page,
		false,
	)
	if err != nil {
		return KindPage{}, err
	}
	if page.Items == nil {
		page.Items = []KindItem{}
	}
	return page, nil
}

// ResolveSources asks Ether to find sources through its indexers/debrid.
func (c *RemoteHostClient) ResolveSources(ctx context.Context, q SourceQuery) ([]SourceCandidate, error) {
	res, err := c.ResolveSourcesWithResult(ctx, q)
	if err != nil {
		return nil, err
	}
	return res.Candidates, nil
}

// ResolveSourcesWithResult asks Ether to find sources and additionally returns
// per-indexer search statuses for indexer_search queries (#733).
func (c *RemoteHostClient) ResolveSourcesWithResult(ctx context.Context, q SourceQuery) (SourceResolveResult, error) {
	var out SourceResolveResult
	_, err := c.callJSON(ctx, http.MethodPost, "/ether/plugin-host/sources/resolve", q, &out, false)
	if err != nil {
		return SourceResolveResult{}, err
	}
	if out.Candidates == nil {
		out.Candidates = []SourceCandidate{}
	}
	return out, nil
}

// CachePut stores one blob in a kind library's cache; the ref is content-addressed.
func (c *RemoteHostClient) CachePut(ctx context.Context, libraryKey, name string, data []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/ether/plugin-host/cache/"+url.PathEscape(libraryKey)+"?name="+url.QueryEscape(name),
		bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	if c.credential != "" {
		req.Header.Set("X-Ether-Plugin-Credential", c.credential)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("plugin-host cache put: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Blob string `json:"blob"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	return out.Blob, nil
}

// CacheOpen reads one cached blob back.
func (c *RemoteHostClient) CacheOpen(ctx context.Context, blobRef string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/ether/plugin-host/cache/"+strings.TrimPrefix(blobRef, "/"), nil)
	if err != nil {
		return nil, err
	}
	if c.credential != "" {
		req.Header.Set("X-Ether-Plugin-Credential", c.credential)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("plugin-host cache open: status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// CacheDelete removes one cached blob from this plugin's own library cache
// namespaces, freeing its quota immediately. Deleting an absent or already
// evicted ref succeeds.
func (c *RemoteHostClient) CacheDelete(ctx context.Context, blobRef string) error {
	_, err := c.callJSON(ctx, http.MethodDelete, "/ether/plugin-host/cache/"+url.PathEscape(blobRef), nil, nil, false)
	return err
}

// StreamURL mints a scoped stream ticket and returns the Ether stream URL
// for one of this plugin's own kind items (spec §6.5).
func (c *RemoteHostClient) StreamURL(ctx context.Context, viewerID int64, itemRef string) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	_, err := c.callJSON(ctx, http.MethodPost, "/ether/plugin-host/frontend/stream-url",
		map[string]any{"viewerId": viewerID, "itemRef": itemRef}, &out, false)
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

// StreamPartURL mints a scoped stream ticket for one zero-based part of a
// multi-file kind item (#576).
func (c *RemoteHostClient) StreamPartURL(ctx context.Context, viewerID int64, itemRef string, partIndex int) (string, error) {
	if partIndex < 0 {
		return "", fmt.Errorf("stream url: partIndex must be non-negative")
	}
	return c.StreamURLWithOptions(ctx, viewerID, itemRef, StreamURLOptions{PartIndex: &partIndex})
}

// StreamURLWithOptions mints a scoped stream ticket for one of this plugin's
// own kind items, optionally pinned to a zero-based part and/or resolver
// PlayableSource.ID (#576, #661) and optionally resolved to an absolute URL
// using the active request origin or the adapter's first published address (#772).
func (c *RemoteHostClient) StreamURLWithOptions(ctx context.Context, viewerID int64, itemRef string, opts StreamURLOptions) (string, error) {
	if opts.Wait != nil && opts.Wait.MaxSeconds <= 0 {
		return "", fmt.Errorf("stream url: wait.maxSeconds must be a positive integer")
	}
	if opts.PartIndex != nil && *opts.PartIndex < 0 {
		return "", fmt.Errorf("stream url: partIndex must be non-negative")
	}
	if opts.Fallback && opts.Source == "" {
		return "", fmt.Errorf("stream url: fallback requires a source")
	}
	if opts.Source != "" && strings.TrimSpace(opts.Source) == "" {
		return "", fmt.Errorf("stream url: source must not be blank")
	}
	payload := map[string]any{"viewerId": viewerID, "itemRef": itemRef}
	if opts.PartIndex != nil {
		payload["partIndex"] = *opts.PartIndex
	}
	if opts.Source != "" {
		payload["source"] = opts.Source
		if opts.Fallback {
			payload["fallback"] = true
		}
	}
	if opts.Absolute {
		payload["absolute"] = true
	}
	if opts.Wait != nil {
		payload["wait"] = opts.Wait
	}
	var headers map[string]string
	if tok, ok := ViewerTokenFromContext(ctx); ok && tok != "" {
		headers = map[string]string{HeaderViewerToken: tok}
	}
	var out struct {
		URL string `json:"url"`
	}
	_, err := c.callJSONWithHeaders(ctx, http.MethodPost, "/ether/plugin-host/frontend/stream-url", headers, payload, &out, false)
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

func (*RemoteHostClient) FoldersHash(context.Context, string, string, ...FolderHashOptions) (FolderHash, error) {
	return FolderHash{}, fmt.Errorf("folders.hash requires the hosted runtime")
}

func (*RemoteHostClient) FoldersHashStart(context.Context, string, string, ...FolderHashOptions) (FolderHashJob, error) {
	return FolderHashJob{}, fmt.Errorf("folders.hash requires the hosted runtime")
}

func (*RemoteHostClient) FoldersFrame(context.Context, string, string, FolderFrameOptions) (FolderFrame, error) {
	return FolderFrame{}, fmt.Errorf("folders.frame requires the hosted runtime")
}

func (*RemoteHostClient) FoldersHashPoll(context.Context, string) (FolderHashJob, error) {
	return FolderHashJob{}, fmt.Errorf("folders.hash requires the hosted runtime")
}

func (*RemoteHostClient) FoldersHashCancel(context.Context, string) (FolderHashJob, error) {
	return FolderHashJob{}, fmt.Errorf("folders.hash requires the hosted runtime")
}
