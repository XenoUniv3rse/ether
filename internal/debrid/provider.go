package debrid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/httpclient"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/retryafter"
)

// Provider errors. Callers classify with errors.Is.
var (
	errDuplicateTorrent = errors.New("debrid: torrent already exists")
	// ErrAuth is a rejected API key or an account that cannot use the API
	// (expired premium). A configuration problem: never a release failure.
	// It is core.ErrDebridAuth so a playback error can name the cause.
	ErrAuth = core.ErrDebridAuth
	// ErrRateLimited is HTTP 429 or the provider's equivalent.
	ErrRateLimited = errors.New("debrid: rate limited")
	// ErrGone is a torrent, file or link the provider no longer has; the
	// payload's stable identifiers can usually re-add it.
	ErrGone = errors.New("debrid: torrent or link no longer available")
	// ErrBlocked is a file the provider holds but refuses to serve, such
	// as Real-Debrid's infringing_file answer to a cached torrent. Re-adding
	// the torrent cannot help, so on this provider it counts as not cached;
	// another provider may still serve it.
	ErrBlocked     = fmt.Errorf("debrid: blocked by the provider: %w", core.ErrNotCached)
	ErrBlockedHash = fmt.Errorf("debrid: the provider blocks this hash: %w", ErrBlocked)
	ErrBlockedFile = fmt.Errorf("debrid: the provider blocks this file: %w", ErrBlocked)
	ErrTransport   = errors.New("debrid: provider transport failure")
	ErrPending     = fmt.Errorf("debrid: provider is still processing the torrent: %w", core.ErrSourceUnavailable)
	// ErrAddRefused is a temporary refusal to add a torrent. The release is
	// skipped on that provider for now and repeated refusals pause adds.
	// Permanent per-release rejections (including infringing_file) use
	// ErrBlocked and must not pause unrelated releases on the account.
	ErrAddRefused = fmt.Errorf("debrid: the provider refused to add the torrent: %w", core.ErrNotCached)
	// ErrDownloadLimit is the account at its active-download limit
	// (Real-Debrid too_many_active_downloads, TorBox ACTIVE_LIMIT): it adds
	// no new torrent for now, cached or not. The provider still answers and
	// serves what it holds, so this is not an outage.
	ErrDownloadLimit = fmt.Errorf("debrid: %w", core.ErrDownloadLimit)
	// ErrTooLarge is a torrent that exceeds the provider's or account plan's
	// maximum torrent size (e.g. TorBox DOWNLOAD_TOO_LARGE, Real-Debrid 29,
	// AllDebrid MAGNET_TOO_LARGE, Debrid-Link torrentTooBig). Another
	// provider may still serve it, and it is not a provider outage.
	ErrTooLarge = fmt.Errorf("debrid: torrent exceeds provider size limit: %w", core.ErrNotCached)
	// errProviderZipped is a torrent that the provider only holds collapsed
	// into a single server-side zip archive instead of its individual files.
	// Another provider may still hold the unzipped files.
	errProviderZipped = fmt.Errorf("debrid: provider holds torrent only as a single zip archive: %w", core.ErrNotCached)
	// ErrNoProvider means no debrid provider is enabled.
	ErrNoProvider = errors.New("debrid: no debrid provider is enabled")
	// ErrUnsupported is an operation a provider does not offer.
	ErrUnsupported = errors.New("debrid: not supported by this provider")
	// errNoVideo is a torrent without a playable file (or without the
	// wanted episode): a property of the release, not of a provider.
	errNoVideo error = releaseNoUsableError{msg: "debrid: no playable video file: " + core.ErrReleaseRejected.Error()}
	// errNoUsableNonVideo is a torrent without a usable non-video file for a
	// plugin release request.
	errNoUsableNonVideo error = releaseNoUsableError{msg: "debrid: no usable non-video file: " + core.ErrReleaseRejected.Error()}
	// errFileMissing is a payload's file absent from its torrent.
	errFileMissing = fmt.Errorf("debrid: file is no longer in the torrent: %w", ErrGone)
)

type releaseNoUsableError struct {
	msg string
}

func (e releaseNoUsableError) Error() string { return e.msg }
func (e releaseNoUsableError) Unwrap() []error {
	return []error{core.ErrReleaseRejected, core.ErrNoUsableFile}
}

// rateLimitError carries the provider's Retry-After hint.
type rateLimitError struct{ retryAfter time.Duration }

func (e *rateLimitError) Error() string { return ErrRateLimited.Error() }
func (e *rateLimitError) Unwrap() error { return ErrRateLimited }

// pendingError is core.ErrNotCached for a torrent that stays on the account
// unfinished (left to download, or an earlier download still running): a
// later Acquire passes TorrentID to follow that download instead of adding
// the torrent a second time.
type pendingError struct {
	torrentID string
	err       error
}

// untrackedPending marks a successful submission by a provider that does not
// return a torrent id usable by Acquire. Availability can still be rechecked.
type untrackedPending struct{ err error }

func (e *untrackedPending) Error() string { return e.err.Error() }
func (e *untrackedPending) Unwrap() error { return e.err }

// queuedPending is core.ErrNotCached for a torrent the provider placed in its
// own queue instead of starting immediately (TorBox returning queued_id with
// no torrent_id when active slots are full). A later check looks the hash up
// on the account until the provider promotes it to an active torrent.
type queuedPending struct {
	queuedID        string
	hash            string
	activeDownloads int
	err             error
}

func (e *queuedPending) Error() string { return e.err.Error() }
func (e *queuedPending) Unwrap() error { return e.err }

func queued(queuedID, hash string, activeDownloads int, err error) error {
	if queuedID == "" {
		return err
	}
	return &queuedPending{
		queuedID:        queuedID,
		hash:            strings.ToLower(strings.TrimSpace(hash)),
		activeDownloads: activeDownloads,
		err:             err,
	}
}

func pendingQueue(err error) (*queuedPending, bool) {
	var qp *queuedPending
	if errors.As(err, &qp) && qp != nil && qp.queuedID != "" {
		return qp, true
	}
	return nil, false
}

func (e *pendingError) Error() string   { return e.err.Error() }
func (e *pendingError) Unwrap() []error { return []error{e.err, ErrPending} }

// pending marks err (core.ErrNotCached) as a download left on the account.
func pending(torrentID string, err error) error {
	if torrentID == "" {
		return err
	}
	return &pendingError{torrentID: torrentID, err: err}
}

// pendingTorrent is the torrent id a pendingError names, or "".
func pendingTorrent(err error) string {
	var pe *pendingError
	if errors.As(err, &pe) {
		return pe.torrentID
	}
	return ""
}

// File is one file of a torrent as a provider lists it.
type File struct {
	ID   string // provider file id ("" when the provider has none)
	Path string
	Size int64
	// MIMEType is optional provider metadata; generic binary types defer to Path.
	MIMEType string
	// Link is the provider-side link for the file (Real-Debrid hoster
	// link, AllDebrid file link, Premiumize, Debrid-Link and Offcloud
	// direct link).
	Link     string
	Zip      *ZipMember // non-nil when Path/Size name a member inside a provider server-side ZIP
	selected bool       // Real-Debrid: already selected in a reused torrent
}

// AcquireRequest asks a provider to make a torrent available.
type AcquireRequest struct {
	InfoHash string
	Magnet   string
	// TorrentID reuses a torrent already on the account ("" adds anew).
	TorrentID string
	// Choose picks the wanted file; providers that select files (Real-Debrid)
	// call it before selection.
	Choose          func([]File) (File, error)
	CachedOnly      bool
	RemoveUncached  bool
	Private         bool
	privateChecked  bool
	selectedFiles   func([]File)
	rememberedFiles []answerFile
	// AllowCompressedZip permits selecting a deflate-compressed member inside
	// a provider's server-side ZIP archive (used for non-video release sources
	// decoded by kindstream and for file-list inspection).
	AllowCompressedZip bool
	// MayRemove, when set, is asked before a torrent this call added is
	// removed again (RemoveUncached); false keeps it on the account because
	// a cluster node is streaming from it.
	MayRemove func(ctx context.Context, torrentID string) bool
}

// mayRemove reports whether a torrent added by this request may be removed.
func (r AcquireRequest) mayRemove(ctx context.Context, torrentID string) bool {
	return r.MayRemove == nil || r.MayRemove(ctx, torrentID)
}

// Acquired is a torrent made available by a provider.
type Acquired struct {
	TorrentID string
	File      File
	// Added reports whether this call added the torrent to the account.
	Added bool
}

// Handle identifies a file for link generation.
type Handle struct {
	TorrentID, FileID, Path, Link, Magnet, InfoHash string
	Size                                            int64
	Private                                         bool
}

// DirectLink is a time-limited HTTPS URL for a file.
type DirectLink struct {
	URL     string
	Size    int64
	Expires time.Time
}

// Account is what the admin test reports.
type Account struct {
	User    string
	Premium bool
	Expires time.Time
}

// Provider is one debrid service. Implementations must never wait for an
// actual torrent download: Acquire returns core.ErrNotCached for a torrent
// the service cannot serve instantly (removing a torrent it just added when
// req.CachedOnly and req.RemoveUncached are set). A torrent that stays on
// the account unfinished is reported with pending(id, err), so the download
// is followed rather than added again.
type Provider interface {
	Kind() string
	// CheckCached reports which hashes are cached; ErrUnsupported when the
	// service has no bulk availability check.
	CheckCached(ctx context.Context, hashes []string) (map[string]bool, error)
	// Find returns the id of a torrent with this hash already on the
	// account ("" when none), so re-adds are avoided; ErrUnsupported when
	// the service keeps no torrent list.
	Find(ctx context.Context, infoHash string) (string, error)
	Acquire(ctx context.Context, req AcquireRequest) (*Acquired, error)
	// Link returns a direct URL for a file; ErrGone when the torrent or
	// file must be re-acquired first.
	Link(ctx context.Context, h Handle) (DirectLink, error)
	Remove(ctx context.Context, torrentID string) error
	Account(ctx context.Context) (Account, error)
}

// ProviderOptions are shared by provider constructors (tests point BaseURL
// at an httptest server and shorten the polls).
type ProviderOptions struct {
	paces        *accountPaces
	HTTPClient   *http.Client
	BaseURL      string
	PollInterval time.Duration // 0 = 500ms
	ReadyPolls   int           // 0 = 6: how long a cached torrent may take to report ready
	Now          func() time.Time
}

func (o ProviderOptions) withDefaults(base string) ProviderOptions {
	if o.HTTPClient == nil {
		o.HTTPClient = egress.Client(egress.CapDebrid, 20*time.Second)
	}
	if o.BaseURL == "" {
		o.BaseURL = base
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	if o.PollInterval <= 0 {
		o.PollInterval = 500 * time.Millisecond
	}
	if o.ReadyPolls <= 0 {
		o.ReadyPolls = 6
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// NewProvider builds the provider for a config entry.
func NewProvider(cfg config.DebridProvider, opts ProviderOptions) (Provider, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("debrid provider %q has no API key: %w", displayName(cfg), ErrAuth)
	}
	switch cfg.Kind {
	case config.DebridRealDebrid:
		return newRealDebrid(cfg.APIKey, opts), nil
	case config.DebridTorBox:
		return newTorBox(cfg.APIKey, opts), nil
	case config.DebridAllDebrid:
		return newAllDebrid(cfg.APIKey, opts), nil
	case config.DebridPremiumize:
		return newPremiumize(cfg.APIKey, opts), nil
	case config.DebridDebridLink:
		return newDebridLink(cfg.APIKey, opts), nil
	case config.DebridOffcloud:
		return newOffcloud(cfg.APIKey, opts), nil
	}
	return nil, fmt.Errorf("debrid: unknown provider kind %q", cfg.Kind)
}

// displayName names an account in logs and errors: the owner's name for it,
// else its service ("Real-Debrid"), never the internal id.
func displayName(cfg config.DebridProvider) string {
	if cfg.Name != "" {
		return cfg.Name
	}
	if label, ok := kindLabels[cfg.Kind]; ok {
		return label
	}
	if cfg.Kind != "" {
		return cfg.Kind
	}
	return cfg.ID
}

var kindLabels = map[string]string{
	config.DebridRealDebrid: "Real-Debrid",
	config.DebridTorBox:     "TorBox",
	config.DebridAllDebrid:  "AllDebrid",
	config.DebridPremiumize: "Premiumize",
	config.DebridDebridLink: "Debrid-Link",
	config.DebridOffcloud:   "Offcloud",
}

// ---------------------------------------------------------------------------
// HTTP plumbing shared by providers.

// apiRequest is one provider API call.
type apiRequest struct {
	retryDelay *time.Duration
	pace       *accountPacer
	retry      bool
	paced      bool
	method     string
	path       string
	query      url.Values
	form       url.Values // urlencoded body
	body       io.Reader  // raw body (with contentType)
	ctype      string
}

type diagnosticKey struct{}
type providerDiagnostic struct {
	log                     *slog.Logger
	provider, release, hash string
}

var diagnosticURL = regexp.MustCompile(`(?i)\b(?:https?|magnet)[:][^\s"']+`)
var diagnosticCredential = regexp.MustCompile(`(?i)(bearer\s+|(?:api[_-]?key|access[_-]?token|token|password)\s*[=:]\s*)[^\s,;"']+`)

func cleanDiagnostic(s, key string) string {
	if key != "" {
		s = strings.ReplaceAll(s, key, "[redacted]")
		s = strings.ReplaceAll(s, url.QueryEscape(key), "[redacted]")
	}
	s = diagnosticURL.ReplaceAllString(s, "[redacted-url]")
	s = diagnosticCredential.ReplaceAllString(s, "${1}[redacted]")
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}

// logProviderResponse records a failed provider response only when a playback
// acquisition supplies release context. Never log a request or raw body.
func logProviderResponse(ctx context.Context, key string, status int, code, msg string) {
	d, ok := ctx.Value(diagnosticKey{}).(providerDiagnostic)
	if !ok {
		return
	}
	clean := func(s string) string { return cleanDiagnostic(s, key) }
	if code == "" {
		code = fmt.Sprintf("HTTP %d", status)
	}
	if msg == "" {
		msg = code
	}
	d.log.Warn("debrid provider rejected request", "provider", clean(d.provider), "code", clean(code), "message", clean(msg), "release", clean(d.release), "infohash", clean(d.hash))
}

// A rate-limited response is returned by call before provider decoding so its
// Retry-After survives. Extract its diagnostic fields without logging the body.
func logProviderHTTPError(ctx context.Context, key string, status int, body []byte) {
	var v struct {
		ErrorCode   json.Number     `json:"error_code"`
		Error       json.RawMessage `json:"error"`
		Message     string          `json:"message"`
		Detail      string          `json:"detail"`
		Description string          `json:"error_description"`
	}
	_ = json.Unmarshal(body, &v)
	code, msg := v.ErrorCode.String(), v.Detail
	if msg == "" {
		msg = v.Message
	}
	if msg == "" {
		msg = v.Description
	}
	var simple string
	if json.Unmarshal(v.Error, &simple) == nil {
		if code == "" {
			code = simple
		} else if msg == "" {
			msg = simple
		}
	} else {
		var nested struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(v.Error, &nested)
		if code == "" {
			code = nested.Code
		}
		if msg == "" {
			msg = nested.Message
		}
	}
	logProviderResponse(ctx, key, status, code, msg)
}

// call performs an API request and returns the status and body. Errors
// never include the request URL: several providers take the key as a
// query parameter.
func call(ctx context.Context, hc *http.Client, base string, r apiRequest, auth func(*http.Request)) (int, []byte, error) {
	if err := core.SpendBackground(ctx, "debrid", base, 1); err != nil {
		return 0, nil, err
	}
	u := base + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	var body io.Reader
	ctype := r.ctype
	switch {
	case r.form != nil:
		body = strings.NewReader(r.form.Encode())
		ctype = "application/x-www-form-urlencoded"
	case r.body != nil:
		body = r.body
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u, body)
	if err != nil {
		return 0, nil, errors.New("debrid: invalid request")
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", httpclient.UserAgent())
	if auth != nil {
		auth(req)
	}
	if r.paced && req.Method != http.MethodGet && req.Method != http.MethodHead {
		req.GetBody = nil
	}
	pace := r.pace
	if pace != nil {
		class := endpointClass(r)
		pace.noteAttempt(ctx, class, r.retry)
		replayCtx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		var connectionSeen atomic.Bool
		req = req.WithContext(httptrace.WithClientTrace(replayCtx, &httptrace.ClientTrace{
			GotConn: func(httptrace.GotConnInfo) {
				if !connectionSeen.Swap(true) {
					return
				}
				// net/http can internally replay on a reused connection.
				// Mutations must return control to the provider adapter.
				if r.paced {
					if req.Method != http.MethodGet && req.Method != http.MethodHead {
						cancel(fmt.Errorf("%w: API mutation replay interrupted", ErrTransport))
						return
					}
					_, release, err := pace.reserve(replayCtx, false)
					if err != nil {
						cancel(err)
						return
					}
					release()
				}
				pace.noteAttempt(ctx, class, true)
			},
		}))
	}
	// Provider API endpoints are fixed. Redirects would bypass pacing and
	// could forward authentication to an unintended endpoint.
	client := *hc
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if cause := context.Cause(req.Context()); cause != nil && ctx.Err() == nil {
			return 0, nil, cause
		}
		return 0, nil, fmt.Errorf("%w: %w", ErrTransport, sanitize(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests && pace != nil {
		pace.noteLimit(true)
	}
	if r.retryDelay != nil {
		*r.retryDelay, _ = retryafter.Parse(resp.Header.Get("Retry-After"), time.Now(), maxBackoff)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("%w: %w", ErrTransport, sanitize(err))
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return resp.StatusCode, data, &rateLimitError{retryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	}
	return resp.StatusCode, data, nil
}

// sanitize strips the URL from transport errors.
func sanitize(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return fmt.Errorf("debrid: request timed out: %w", context.DeadlineExceeded)
		}
		return fmt.Errorf("debrid: request failed: %w", ue.Err)
	}
	return err
}

// retryAfter parses a Retry-After header, capped at maxBackoff: a huge
// value must neither overflow the duration nor pause a provider or CDN
// host for longer than this server's own longest back-off.
func retryAfter(v string) time.Duration {
	d, _ := retryafter.Parse(v, time.Now(), maxBackoff)
	return d
}

// statusError maps an unexpected HTTP status to a classified error.
func statusError(kind string, status int, msg string) error {
	if msg == "" {
		msg = http.StatusText(status)
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%s: %s: %w", kind, msg, ErrAuth)
	case status == http.StatusNotFound || status == http.StatusGone:
		return fmt.Errorf("%s: %s: %w", kind, msg, ErrGone)
	case status == http.StatusUnavailableForLegalReasons:
		return fmt.Errorf("%s: %w", kind, ErrBlocked)
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("%s: %s: %w", kind, msg, ErrRateLimited)
	}
	return fmt.Errorf("%s: HTTP %d: %s: %w", kind, status, msg, ErrTransport)
}

func decodeJSON(kind string, data []byte, out any) error {
	if err := json.Unmarshal(bytes.TrimSpace(data), out); err != nil {
		return fmt.Errorf("%s: unexpected response", kind)
	}
	return nil
}

// sleep waits d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// listCache remembers a provider's torrent list (hash -> id) briefly so
// reuse lookups cost at most one list call per minute.
type listCache struct {
	mu     sync.Mutex
	at     time.Time
	byHash map[string]string
	flight *listFlight // used only by TorBox and Debrid-Link
}

const listTTL = time.Minute

// known returns a fresh local listing without waiting behind an account refresh.
func (c *listCache) known(now time.Time, hash string) string {
	if !c.mu.TryLock() {
		return ""
	}
	defer c.mu.Unlock()
	if now.Sub(c.at) > listTTL {
		return ""
	}
	return c.byHash[strings.ToLower(hash)]
}

func (c *listCache) find(ctx context.Context, now time.Time, hash string, load func(context.Context) (map[string]string, error)) (string, error) {
	hash = strings.ToLower(hash)
	c.mu.Lock()
	if c.byHash != nil && now.Sub(c.at) <= listTTL {
		id := c.byHash[hash]
		c.mu.Unlock()
		return id, nil
	}
	if f := c.flight; f != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-f.done:
			if f.err != nil {
				return "", f.err
			}
			c.mu.Lock()
			id := c.byHash[hash]
			c.mu.Unlock()
			return id, nil
		}
	}
	f := &listFlight{done: make(chan struct{})}
	c.flight = f
	c.mu.Unlock()
	items, err := load(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.byHash, c.at = items, now
	}
	f.err = err
	c.flight = nil
	close(f.done)
	return items[hash], err
}

// listFlight publishes one shared refresh without holding mu during I/O.
type listFlight struct {
	done chan struct{}
	err  error
}

// shared starts a bounded refresh or joins the one already running. Detached
// refreshes retain admission policy but outlive a speculative caller's cancel,
// so foreground duplicate recovery can use the same account listing.
func (c *listCache) shared(ctx context.Context, now time.Time, load func(context.Context) (map[string]string, error)) *listFlight {
	if c.flight != nil {
		return c.flight
	}
	f := &listFlight{done: make(chan struct{})}
	c.flight = f
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), providerOpTimeout)
	go func() {
		defer cancel()
		m, err := load(ctx)
		c.mu.Lock()
		defer c.mu.Unlock()
		if err == nil {
			c.byHash, c.at = m, now
		}
		f.err = err
		c.flight = nil
		close(f.done)
	}()
	return f
}

// background returns only fresh local state; a cold snapshot is fetched once
// alongside the caller. Holding mu never entails waiting on network I/O.
func (c *listCache) background(ctx context.Context, now time.Time, hash string, load func(context.Context) (map[string]string, error)) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byHash != nil && now.Sub(c.at) <= listTTL {
		return c.byHash[strings.ToLower(hash)]
	}
	c.shared(ctx, now, load)
	return ""
}

// reuse waits only for an already-running refresh, and returns a positive
// fresh hint. A negative snapshot never substitutes for duplicate recovery.
func (c *listCache) reuse(ctx context.Context, now time.Time, hash string) (string, error) {
	c.mu.Lock()
	f := c.flight
	c.mu.Unlock()
	if f != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-f.done:
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byHash != nil && now.Sub(c.at) <= listTTL {
		return c.byHash[strings.ToLower(hash)], nil
	}
	return "", nil
}

func (c *listCache) findShared(ctx context.Context, now time.Time, hash string, load func(context.Context) (map[string]string, error)) (string, error) {
	c.mu.Lock()
	if c.byHash != nil && now.Sub(c.at) <= listTTL {
		id := c.byHash[strings.ToLower(hash)]
		c.mu.Unlock()
		return id, nil
	}
	f := c.shared(ctx, now, load)
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-f.done:
	}
	if f.err != nil {
		return "", f.err
	}
	return c.reuse(ctx, now, hash)
}

// remember records an id the provider just returned.
func (c *listCache) remember(hash, id string) {
	c.mu.Lock()
	if c.byHash != nil {
		c.byHash[strings.ToLower(hash)] = id
	}
	c.mu.Unlock()
}

// forget drops a removed torrent.
func (c *listCache) forget(id string) {
	c.mu.Lock()
	for h, v := range c.byHash {
		if v == id {
			delete(c.byHash, h)
		}
	}
	c.mu.Unlock()
}

// recentLinks holds the direct URL a provider listed while acquiring a
// torrent, so the Link call that immediately follows needs no second
// listing. Providers whose listed URL is already the direct link (and that
// document no expiry for it) must not trust a stored URL later: a removed
// torrent would hand out a dead link on every refresh. An entry is used
// once, within recentLinkTTL.
type recentLinks struct {
	mu sync.Mutex
	m  map[string]recentLink
}

type recentLink struct {
	url string
	at  time.Time
}

const recentLinkTTL = time.Minute

func (r *recentLinks) put(torrentID, fileID, u string, now time.Time) {
	if u == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]recentLink{}
	}
	for k, v := range r.m {
		if now.Sub(v.at) > recentLinkTTL {
			delete(r.m, k)
		}
	}
	r.m[torrentID+"\x00"+fileID] = recentLink{url: u, at: now}
}

func (r *recentLinks) take(torrentID, fileID string, now time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := torrentID + "\x00" + fileID
	v, ok := r.m[k]
	if !ok {
		return ""
	}
	delete(r.m, k)
	if now.Sub(v.at) > recentLinkTTL {
		return ""
	}
	return v.url
}

func (r *recentLinks) forget(torrentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.m {
		if strings.HasPrefix(k, torrentID+"\x00") {
			delete(r.m, k)
		}
	}
}

// ---------------------------------------------------------------------------
// File selection.

var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".ts": true,
	".m2ts": true, ".webm": true, ".wmv": true, ".mpg": true, ".mpeg": true,
}

func isVideo(p string) bool {
	return videoExts[strings.ToLower(path.Ext(p))]
}

// playableVideo is shared by automatic selection and video-only selectors.
// A filename must name a supported video, and a specific provider MIME type
// must agree. Providers commonly report generic binary data for valid videos.
func playableVideo(f File) bool {
	if !isVideo(f.Path) || isExtra(f.Path) || f.Size <= 0 {
		return false
	}
	if strings.TrimSpace(f.MIMEType) == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(f.MIMEType)
	return err == nil && (strings.HasPrefix(mediaType, "video/") ||
		mediaType == "application/octet-stream" || mediaType == "binary/octet-stream")
}

// isExtra reports samples, trailers and featurettes.
func isExtra(p string) bool {
	lower := strings.ToLower(strings.ReplaceAll(p, "\\", "/"))
	for _, tok := range strings.FieldsFunc(lower, func(r rune) bool {
		return r == '/' || r == '.' || r == '-' || r == '_' || r == ' ' || r == '[' || r == ']' || r == '(' || r == ')'
	}) {
		switch tok {
		case "sample", "samples", "trailer", "trailers", "featurette", "featurettes", "extras":
			return true
		}
	}
	return false
}

// chooseNonVideoFile selects a non-video file (or single-volume wrapper
// archive for downstream extraction) from a torrent file list when a plugin
// release target carries AllowNonVideo with a member selector or media MIME.
func chooseNonVideoFile(files []File, target *core.ReleaseTarget) (File, error) {
	var nonEmpty []File
	var names []string
	for _, f := range files {
		if f.Size <= 0 {
			continue
		}
		nonEmpty = append(nonEmpty, f)
		names = append(names, f.Path)
	}
	if len(nonEmpty) == 0 {
		return File{}, fmt.Errorf("debrid: no non-empty files (%s): %w", torrentFileSummary(files), errNoUsableNonVideo)
	}

	singleArchive := func() (File, bool) {
		type archiveSetKey struct {
			kind nzb.Kind
			name string
		}
		setCounts := make(map[archiveSetKey]int)
		infos := make([]nzb.FileInfo, len(nonEmpty))
		for i, f := range nonEmpty {
			info := nzb.Classify(path.Base(strings.ReplaceAll(f.Path, "\\", "/")))
			infos[i] = info
			if info.Kind.IsArchive() {
				setCounts[archiveSetKey{kind: info.Kind, name: info.SetName}]++
			}
		}
		best := -1
		for i, f := range nonEmpty {
			info := infos[i]
			if !info.Kind.IsArchive() || info.Kind == nzb.KindSplit {
				continue
			}
			// Debrid serves one file per Acquire; skip multi-volume archive
			// sets that cannot be opened from a single HTTP volume.
			if setCounts[archiveSetKey{kind: info.Kind, name: info.SetName}] != 1 || info.Volume > 0 {
				continue
			}
			if best < 0 || f.Size > nonEmpty[best].Size || (f.Size == nonEmpty[best].Size && f.Path < nonEmpty[best].Path) {
				best = i
			}
		}
		if best >= 0 {
			return nonEmpty[best], true
		}
		return File{}, false
	}

	if want := strings.TrimSpace(target.Member); want != "" {
		if idx, ok, ambig := nzb.MatchPathIndex(names, want); ok {
			return nonEmpty[idx], nil
		} else if ambig {
			return File{}, fmt.Errorf("debrid: member %q matched multiple files: %w", want, core.ErrWrongContent)
		}
		if arc, ok := singleArchive(); ok {
			return arc, nil
		}
		return File{}, fmt.Errorf("debrid: no file or archive matching %q (%s): %w", want, torrentFileSummary(files), errNoUsableNonVideo)
	}

	if target.MemberIndex != nil {
		if arc, ok := singleArchive(); ok {
			return arc, nil
		}
		return File{}, fmt.Errorf("debrid: no archive for memberIndex %d (%s): %w", *target.MemberIndex, torrentFileSummary(files), errNoUsableNonVideo)
	}

	if th := coreTrackToNZB(target.Track); th != nil {
		if idx, ok, ambig := nzb.MatchTrackIndex(names, *th, target.MIME); ok {
			return nonEmpty[idx], nil
		} else if ambig {
			return File{}, fmt.Errorf("debrid: track hint (disc=%d number=%d title=%q) matched multiple files: %w", th.Disc, th.Number, th.Title, core.ErrWrongContent)
		}
		if arc, ok := singleArchive(); ok {
			return arc, nil
		}
		return File{}, fmt.Errorf("debrid: no file or archive matching track hint (disc=%d number=%d title=%q) (%s): %w", th.Disc, th.Number, th.Title, torrentFileSummary(files), errNoUsableNonVideo)
	}

	best := -1
	bestRank := 0
	for i, f := range nonEmpty {
		rank := nzb.NonVideoMatchRank(f.Path, target.MIME)
		if rank == 0 {
			continue
		}
		if rank > bestRank ||
			(rank == bestRank && (best < 0 || f.Size > nonEmpty[best].Size || (f.Size == nonEmpty[best].Size && f.Path < nonEmpty[best].Path))) {
			best = i
			bestRank = rank
		}
	}
	if best >= 0 {
		return nonEmpty[best], nil
	}
	if arc, ok := singleArchive(); ok {
		return arc, nil
	}
	return File{}, fmt.Errorf("debrid: no usable %s file (%s): %w", target.MIME, torrentFileSummary(files), errNoUsableNonVideo)
}

// ChooseFile selects an explicitly named file (including non-video files when
// target.AllowNonVideo is set) or picks the main playable video: the target
// episode or largest video.
func ChooseFile(files []File, target *core.ReleaseTarget) (File, error) {
	exactMatch := func(f File) bool {
		want := strings.ReplaceAll(target.ExactFile, "\\", "/")
		if want == "" {
			return true
		}
		name := strings.TrimPrefix(strings.ReplaceAll(f.Path, "\\", "/"), "/")
		want = strings.TrimPrefix(want, "/")
		if name == want {
			return true
		}
		// Providers list the same torrent with and without its root folder
		// (Real-Debrid "/01.flac", TorBox "Album/01.flac"), so a path that is
		// the other's suffix on a folder boundary is the same file.
		if strings.HasSuffix(name, "/"+want) || strings.HasSuffix(want, "/"+name) {
			return true
		}
		return !strings.Contains(want, "/") && path.Base(name) == want
	}
	if target != nil && target.AllowNonVideo && (target.FileIndex != nil || target.ExactFile != "") {
		if target.FileIndex != nil {
			i := *target.FileIndex
			if i < 0 || i >= len(files) {
				return File{}, fmt.Errorf("debrid: fileIndex %d outside file list of %d (%s): %w", i, len(files), torrentFileSummary(files), core.ErrWrongContent)
			}
			f := files[i]
			if !exactMatch(f) {
				return File{}, fmt.Errorf("debrid: selected file %q does not match target %q (%s): %w", f.Path, target.ExactFile, torrentFileSummary(files), core.ErrWrongContent)
			}
			if f.Size <= 0 {
				return File{}, fmt.Errorf("debrid: selected file %q is empty: %w", f.Path, core.ErrReleaseRejected)
			}
			return f, nil
		}
		var chosen File
		count := 0
		for _, f := range files {
			if exactMatch(f) {
				chosen = f
				count++
			}
		}
		if count == 0 {
			cleanWant := strings.TrimPrefix(strings.ReplaceAll(target.ExactFile, "\\", "/"), "/")
			if strings.Contains(cleanWant, "/") {
				suffix := "/" + cleanWant
				for _, f := range files {
					cleanName := strings.TrimPrefix(strings.ReplaceAll(f.Path, "\\", "/"), "/")
					if strings.HasSuffix(cleanName, suffix) {
						chosen = f
						count++
					}
				}
			}
		}
		if count != 1 {
			return File{}, fmt.Errorf("debrid: file %q matched %d files (%s): %w", target.ExactFile, count, torrentFileSummary(files), core.ErrWrongContent)
		}
		if chosen.Size <= 0 {
			return File{}, fmt.Errorf("debrid: selected file %q is empty: %w", chosen.Path, core.ErrReleaseRejected)
		}
		return chosen, nil
	}

	if target != nil && target.AllowNonVideo &&
		(target.Member != "" || target.MemberIndex != nil || coreTrackToNZB(target.Track) != nil || nzb.IsNonVideoMIME(target.MIME)) {
		return chooseNonVideoFile(files, target)
	}

	featureCount := 0
	for _, f := range files {
		if playableVideo(f) {
			featureCount++
		}
	}
	if featureCount == 0 {
		return File{}, fmt.Errorf("%w (%s)", errNoVideo, torrentFileSummary(files))
	}
	movieMatches := func(f File) bool { return featureCount <= 1 || movieFileMatches(f.Path, target) }

	if target != nil && (target.FileIndex != nil || target.ExactFile != "") {
		if target.FileIndex != nil {
			i := *target.FileIndex
			if i < 0 || i >= len(files) {
				return File{}, fmt.Errorf("debrid: fileIndex %d outside file list of %d (%s): %w", i, len(files), torrentFileSummary(files), core.ErrWrongContent)
			}
			f := files[i]
			if !playableVideo(f) {
				return File{}, fmt.Errorf("debrid: selected file %q is not a main video: %w", f.Path, errNoVideo)
			}
			if !exactMatch(f) || !movieMatches(f) {
				return File{}, fmt.Errorf("debrid: selected file %q does not match target (%s): %w", f.Path, torrentFileSummary(files), core.ErrWrongContent)
			}
			return f, nil
		}
		var chosen File
		count := 0
		for _, f := range files {
			if exactMatch(f) && !playableVideo(f) {
				return File{}, fmt.Errorf("debrid: selected file %q is not a main video: %w", f.Path, errNoVideo)
			}
			if playableVideo(f) && exactMatch(f) && movieMatches(f) {
				chosen = f
				count++
			}
		}
		if count != 1 {
			return File{}, fmt.Errorf("debrid: file %q matched %d files (%s): %w", target.ExactFile, count, torrentFileSummary(files), core.ErrWrongContent)
		}
		return chosen, nil
	}

	if target != nil && target.Season == 0 && target.AddonSpecialEpisode > 0 {
		copy := *target
		copy.Episode = target.AddonSpecialEpisode
		target = &copy
	}
	if f, ok := sourceNamedFile(files, target); ok {
		return f, nil
	}
	var videos []File
	identityMismatch := false
	for _, f := range files {
		if target != nil && (!movieMatches(f) || !fileCourMatchesTarget(f.Path, target) || !fileIdentityMatches(f, target)) {
			identityMismatch = identityMismatch || playableVideo(f)
			continue
		}
		if playableVideo(f) {
			videos = append(videos, f)
		}
	}
	if len(videos) == 0 {
		if identityMismatch {
			// The torrent may be correct for a different series. A mismatch
			// must never permanently blacklist it for that title as well.
			return File{}, fmt.Errorf("debrid: different series or episode title: %w", core.ErrWrongContent)
		}
		return File{}, fmt.Errorf("%w (%s)", errNoVideo, torrentFileSummary(files))
	}
	sort.SliceStable(videos, func(i, j int) bool { return videos[i].Size > videos[j].Size })
	if target == nil || target.Episode <= 0 {
		if target != nil && len(videos) > 1 && len(target.WantedLanguages) > 0 {
			mc := make([]releaseparse.MovieCandidate, len(videos))
			for i, v := range videos {
				mc[i] = releaseparse.MovieCandidate{Path: v.Path, Size: v.Size}
			}
			order := releaseparse.OrderMovieCandidates(mc, target.MovieTitles, target.OriginalLanguage, target.WantedLanguages)
			return videos[order[0]], nil
		}
		return videos[0], nil
	}
	if target.Season == 0 {
		for _, f := range videos {
			base := path.Base(strings.ReplaceAll(f.Path, "\\", "/"))
			info := releaseparse.Parse(base)
			if releaseparse.EpisodeMatches(info, 0, target.Episode) ||
				(len(info.Seasons) == 0 && len(info.Episodes) == 0 && len(info.AbsoluteEpisodes) == 0 && releaseparse.SpecialTitleEvidence(base, target.EpisodeTitle, target.ShowTitles...)) {
				return f, nil
			}
		}
		return File{}, fmt.Errorf("debrid: no verified special S00E%02d: %w", target.Episode, core.ErrWrongContent)
	}
	for _, cand := range targetEpisodeCandidates(target) {
		candCour := target.Cour
		if cand.Season != target.Season || cand.Episode != target.Episode {
			candCour = 0
		}
		sizeGate := episodeSizeGateForSeason(videos, cand.Season, candCour)
		// seasonCounted is a file in the target's cour numbered within the
		// season ("S04.Part.2/S04E17"), used only when no file carries the
		// cour-relative number.
		var seasonCounted *File
		// Pass 1: explicit season + episode match on base name, or on full path
		// when the base name carries no episode number of its own.
		for _, f := range videos {
			if !sizeGate.Accept(f.Path, f.Size) {
				continue
			}
			normPath := strings.ReplaceAll(f.Path, "\\", "/")
			baseInfo := releaseparse.Parse(path.Base(normPath))
			fullInfo := releaseparse.Parse(strings.ReplaceAll(normPath, "/", " "))
			// The file's own season, else the one its nearest season
			// folder names: in a range pack "Show.S01-S05/S02/..." the
			// whole path reads as seasons 1 to 5.
			fileSeasons := baseInfo.Seasons
			if len(fileSeasons) == 0 {
				fileSeasons = releaseparse.FolderSeasons(normPath)
			}
			fileCour, courRelative := baseInfo.Cour, baseInfo.CourRelative
			if fileCour == 0 {
				fileCour, courRelative = fullInfo.Cour, fullInfo.CourRelative
			}
			if candCour > 0 && target.CourEpisode > 0 && fileCour > 0 {
				seasonOK := len(fileSeasons) == 0 || slices.Contains(fileSeasons, cand.Season)
				if fileCour == candCour && seasonOK &&
					(slices.Contains(baseInfo.Episodes, target.CourEpisode) ||
						releaseparse.EpisodeMatchesAbsolute(baseInfo, target.CourEpisode)) {
					return f, nil
				}
				if seasonCounted == nil && fileCour == candCour && seasonOK && !baseInfo.CourRelative &&
					slices.Contains(baseInfo.Episodes, cand.Episode) {
					seasonCounted = &f
				}
				continue
			}
			if !releaseparse.SeasonNumberingUnderCour(fileCour, courRelative, target.Cour, target.Anime) {
				continue
			}
			// Absolute anime numbers are authoritative file identities. Never let
			// a parent pack's episode range or the single-file fallback override one.
			if len(baseInfo.AbsoluteEpisodes) > 0 {
				if (len(fileSeasons) == 0 || len(fileSeasons) == 1 && fileSeasons[0] == cand.Season) && releaseparse.EpisodeMatchesAbsolute(baseInfo, target.AbsoluteEpisode) {
					return f, nil
				}
				// In an explicitly numbered season folder, a bare file number is
				// local to that season (the folder supplies the missing Sxx).
				if len(fileSeasons) == 1 && fileSeasons[0] == cand.Season && releaseparse.EpisodeMatchesAbsolute(baseInfo, cand.Episode) {
					return f, nil
				}
				continue
			}
			baseInfo.Seasons = fileSeasons
			if len(fileSeasons) > 0 {
				fullInfo.Seasons = fileSeasons
			}
			if releaseparse.EpisodeMatches(baseInfo, cand.Season, cand.Episode) ||
				(len(baseInfo.Episodes) == 0 && (len(videos) == 1 || len(fullInfo.Episodes) == 1) && releaseparse.EpisodeMatches(fullInfo, cand.Season, cand.Episode)) ||
				(cand.Season > 0 && len(baseInfo.Episodes) == 0 && len(baseInfo.AbsoluteEpisodes) == 0 && releaseparse.EpisodeMatchesAirDateString(baseInfo, target.AirDate)) {
				return f, nil
			}
		}
		if seasonCounted != nil {
			return *seasonCounted, nil
		}
		// Pass 2: classic pack numbering ("Season 2/05 - Title.mkv", "205 - Title.avi",
		// or miniseries "Part 2 - Title.mkv") when no standard SxxExx matched.
		if len(videos) > 1 {
			for _, f := range videos {
				if sizeGate.Accept(f.Path, f.Size) && classicPackEpisodeMatches(f.Path, cand.Season, cand.Episode) {
					return f, nil
				}
			}
		}
	}
	// A single-episode torrent whose file name carries no episode number.
	// A file that names a different episode (a partial pack) never stands
	// in for the wanted one.
	if len(videos) == 1 {
		normPath := strings.ReplaceAll(videos[0].Path, "\\", "/")
		baseInfo := releaseparse.Parse(path.Base(normPath))
		info := releaseparse.Parse(strings.ReplaceAll(normPath, "/", " "))
		seasons := baseInfo.Seasons
		if len(seasons) == 0 {
			seasons = releaseparse.FolderSeasons(normPath)
		}
		// Nor does one that sits in (or names) another season.
		if len(baseInfo.AbsoluteEpisodes) == 0 && len(baseInfo.Episodes) == 0 && len(info.Episodes) == 0 && targetAllowsSeason(target, seasons) && !namesOtherClassicEpisodeForTarget(videos[0].Path, target) {
			return videos[0], nil
		}
	}
	return File{}, fmt.Errorf("debrid: no file for S%02dE%02d in torrent (%s): %w", target.Season, target.Episode, torrentFileSummary(files), core.ErrWrongContent)
}

// sourceNamedFile accepts a unique basename hint only after the normal identity
// checks. Numbered files still go through the regular selector so the hint
// cannot override episode, absolute, season, or cour numbering.
func sourceNamedFile(files []File, target *core.ReleaseTarget) (File, bool) {
	if target == nil || target.Episode <= 0 || target.FileName == "" {
		return File{}, false
	}
	basename := func(p string) string {
		p = path.Base(strings.ReplaceAll(p, "\\", "/"))
		if isVideo(p) {
			p = strings.TrimSuffix(p, path.Ext(p))
		}
		return p
	}
	var match File
	count := 0
	for _, f := range files {
		if playableVideo(f) && strings.EqualFold(basename(f.Path), basename(target.FileName)) {
			match = f
			count++
		}
	}
	if count != 1 || !fileCourMatchesTarget(match.Path, target) || !fileIdentityMatches(match, target) {
		return File{}, false
	}
	var peers []File
	for _, f := range files {
		if playableVideo(f) && fileCourMatchesTarget(f.Path, target) && fileIdentityMatches(f, target) {
			peers = append(peers, f)
		}
	}
	if !episodeSizeGate(peers, target).Accept(match.Path, match.Size) {
		return File{}, false
	}
	info := releaseparse.Parse(path.Base(strings.ReplaceAll(match.Path, "\\", "/")))
	if len(info.Seasons) > 0 && !targetAllowsSeason(target, info.Seasons) {
		return File{}, false
	}
	if len(info.Episodes) > 0 || len(info.AbsoluteEpisodes) > 0 || len(info.Seasons) > 0 || namesOtherClassicEpisodeForTarget(match.Path, target) {
		copyTarget := *target
		copyTarget.FileName = "" // avoid applying the hint again
		_, err := ChooseFile([]File{match}, &copyTarget)
		return match, err == nil
	}
	// Even a title-only file must not contradict an explicitly named season
	// folder. A collection folder with several seasons supplies no such proof.
	if folder := releaseparse.FolderSeasons(match.Path); len(folder) == 1 && !targetAllowsSeason(target, folder) {
		return File{}, false
	}
	return match, true
}

// torrentFileSummary bounds diagnostics regardless of the torrent's size.
func torrentFileSummary(files []File) string {
	names := make([]string, 0, min(5, len(files)))
	for _, f := range files[:min(5, len(files))] {
		name := path.Base(strings.ReplaceAll(f.Path, "\\", "/"))
		runes := []rune(name)
		if len(runes) > 160 {
			name = string(runes[:160]) + "…"
		}
		names = append(names, fmt.Sprintf("%q", name))
	}
	return fmt.Sprintf("%d files: %s", len(files), strings.Join(names, ", "))
}

// fileCourMatchesTarget applies to every selection path, including classic
// numbering and the unnamed single-file fallback.
func fileCourMatchesTarget(filePath string, target *core.ReleaseTarget) bool {
	if target == nil || target.Episode <= 0 {
		return true
	}
	norm := strings.ReplaceAll(filePath, "\\", "/")
	base := releaseparse.Parse(path.Base(norm))
	full := releaseparse.Parse(strings.ReplaceAll(norm, "/", " "))
	cour, courRelative := base.Cour, base.CourRelative
	if cour == 0 {
		cour, courRelative = full.Cour, full.CourRelative
	}
	if cour == 0 {
		return true
	}
	if cour > 1 && (target.Cour != cour || target.CourEpisode <= 0) &&
		!releaseparse.SeasonNumberingUnderCour(cour, courRelative, target.Cour, target.Anime) ||
		cour == 1 && target.Cour > 1 {
		return false
	}
	folder := releaseparse.FolderSeasons(norm)
	return (len(base.Seasons) == 0 || targetAllowsSeason(target, base.Seasons)) &&
		(len(folder) == 0 || targetAllowsSeason(target, folder))
}

func chosenFileMatchesTarget(filePath string, target *core.ReleaseTarget) bool {
	if !fileCourMatchesTarget(filePath, target) {
		return false
	}
	if target == nil || target.Episode <= 0 || filePath == "" {
		return true
	}
	if target.Season != 0 {
		for _, cand := range targetEpisodeCandidates(target) {
			if classicPackEpisodeMatches(filePath, cand.Season, cand.Episode) {
				return true
			}
		}
	}
	_, err := ChooseFile([]File{{Path: filePath, Size: 1}}, target)
	return err == nil
}

var (
	rePartEpisode = regexp.MustCompile(`(?i)\b(?:Part|Pt)\.?\s*(\d{1,2})\b`)
)

func classicPackEpisodeMatches(filePath string, wantSeason, wantEp int) bool {
	return releaseparse.ClassicPackEpisodeMatches(filePath, wantSeason, wantEp)
}

func namesOtherClassicEpisode(filePath string, wantSeason, wantEp int) bool {
	base := path.Base(strings.ReplaceAll(filePath, "\\", "/"))
	if wantSeason == 1 {
		if m := rePartEpisode.FindStringSubmatch(base); m != nil {
			if ep, err := strconv.Atoi(m[1]); err == nil && ep > 0 && ep != wantEp {
				return true
			}
		}
	}
	return false
}

// sameFile chooses the file a payload already names (link refresh after
// re-adding a torrent, or on another provider): same base name and size,
// else the only file of that size.
func sameFile(p Payload) func([]File) (File, error) {
	return func(files []File) (File, error) {
		name := strings.ToLower(p.Name())
		var bySize []File
		for _, f := range files {
			if f.Size != p.Size {
				continue
			}
			if strings.ToLower(path.Base(strings.ReplaceAll(f.Path, "\\", "/"))) == name {
				return f, nil
			}
			bySize = append(bySize, f)
		}
		if len(bySize) == 1 {
			return bySize[0], nil
		}
		return File{}, fmt.Errorf("%s: %w", p.Name(), errFileMissing)
	}
}

func fileIdentityMatches(f File, target *core.ReleaseTarget) bool {
	if target == nil {
		return true
	}
	if len(target.MovieTitles) > 0 {
		return true // movie identity is arbitrated only with the complete multi-file list
	}
	idMatch, idConflict := releaseparse.CompareTitleIDs(target.ReportedIDs, target.ExpectedIDs)
	if idConflict {
		return false
	}
	if target.Season == 0 && target.Episode > 0 {
		if !releaseparse.SpecialMatches(f.Path, target.EpisodeTitle, target.SpecialTitles, target.ShowTitles) {
			return false
		}
	}
	full := strings.NewReplacer("/", " ", "\\", " ").Replace(f.Path)
	info := releaseparse.Parse(full)
	year := releaseparse.EpisodeSeriesYear(full, info.Year)
	return releaseparse.SeriesYearMatches(year, target.ShowYear) && (target.Season == 0 || year != 0 || idMatch || releaseparse.EpisodeTitleMatches(path.Base(strings.ReplaceAll(f.Path, "\\", "/")), target.EpisodeTitle))
}

func targetEpisodeCandidates(target *core.ReleaseTarget) []core.EpisodeNumber {
	if target == nil || target.Episode <= 0 {
		return nil
	}
	var out []core.EpisodeNumber
	add := func(s, e int) {
		if s <= 0 || e <= 0 {
			return
		}
		n := core.EpisodeNumber{Season: s, Episode: e}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	add(target.Season, target.Episode)
	for _, a := range target.Aliases {
		add(a.Season, a.Episode)
	}
	if target.PackID == "" {
		add(target.CanonicalSeason, target.CanonicalEpisode)
	}
	return out
}

func targetAllowsSeason(target *core.ReleaseTarget, seasons []int) bool {
	if target == nil || len(seasons) == 0 {
		return true
	}
	for _, cand := range targetEpisodeCandidates(target) {
		if slices.Contains(seasons, cand.Season) {
			return true
		}
	}
	return false
}

func namesOtherClassicEpisodeForTarget(filePath string, target *core.ReleaseTarget) bool {
	if target == nil {
		return false
	}
	cands := targetEpisodeCandidates(target)
	if len(cands) == 0 {
		return namesOtherClassicEpisode(filePath, target.Season, target.Episode)
	}
	other := false
	for _, cand := range cands {
		if namesOtherClassicEpisode(filePath, cand.Season, cand.Episode) {
			other = true
		} else if cand.Season == 1 {
			return false
		}
	}
	return other
}

func episodeSizeGate(videos []File, target *core.ReleaseTarget) releaseparse.PackEpisodeSizeGate {
	if target == nil || target.Season <= 0 || target.Episode <= 0 {
		return releaseparse.PackEpisodeSizeGate{}
	}
	return episodeSizeGateForSeason(videos, target.Season, target.Cour)
}

func episodeSizeGateForSeason(videos []File, season, cour int) releaseparse.PackEpisodeSizeGate {
	if season <= 0 {
		return releaseparse.PackEpisodeSizeGate{}
	}
	sized := make([]releaseparse.PackSizedFile, len(videos))
	for i, f := range videos {
		sized[i] = releaseparse.PackSizedFile{Path: f.Path, Size: f.Size}
	}
	return releaseparse.NewPackEpisodeSizeGate(sized, season, cour)
}

func movieFileMatches(name string, target *core.ReleaseTarget) bool {
	return target == nil || len(target.MovieTitles) == 0 || releaseparse.MovieFileMatches(name, target.MovieTitles, target.MovieYear)
}
