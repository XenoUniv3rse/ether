package vela

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/mediasafety"
	"github.com/braedonsaunders/ether/internal/frontend/kit/rangefile"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reading"
)

const (
	downloadTicketTTL  = time.Hour
	maxDownloadSources = 64
)

type downloadCapabilityKey struct{}

type downloadGrantRequest struct {
	IdempotencyKey string `json:"idempotencyKey"`
	VersionID      string `json:"versionId"`
	Client         client `json:"client"`
}

type downloadGrant struct {
	TitleID        string    `json:"titleId"`
	VersionID      string    `json:"versionId,omitempty"`
	URL            string    `json:"url"`
	ExpiresAt      time.Time `json:"expiresAt"`
	ETag           string    `json:"etag"`
	SourceID       string    `json:"sourceId"`
	ByteLength     int64     `json:"byteLength,omitempty"`
	MIMEType       string    `json:"mimeType"`
	Filename       string    `json:"filename"`
	Container      string    `json:"container"`
	OriginalVideo  object    `json:"originalVideo,omitempty"`
	AudioTracks    []object  `json:"audioTracks"`
	SubtitleTracks []object  `json:"subtitleTracks"`
	Chapters       []object  `json:"chapters"`
	Markers        []object  `json:"markers,omitempty"`
}

type downloadCapability struct {
	Enabled        bool `json:"enabled"`
	MaxPerDay      *int `json:"maxPerDay,omitempty"`
	RemainingToday *int `json:"remainingToday,omitempty"`
}

// The registry charge includes pending opens and expired tickets with active
// requests. A ticket never evicts another ticket to admit a new open.
type downloadTickets struct {
	mu      sync.Mutex
	entries map[string]*downloadTicket
	inUse   int
	closed  bool
}

type downloadTicket struct {
	source    *frontend.OfflineDownloadSource
	filename  string
	sidecars  map[string]string
	expiresAt time.Time
	timer     *time.Timer
	readers   int
	expired   bool
}

func (i *instance) downloadPolicy(v *frontend.Viewer) (frontend.OfflineDownloadQuota, bool) {
	quota, ok := i.host.ViewerPolicy.(frontend.OfflineDownloadQuota)
	if !ok || i.host.OfflineDownloads == nil || i.host.HostConfig == nil || i.host.Capabilities == nil {
		return nil, false
	}
	cfg := i.host.HostConfig().Downloads
	return quota, frontend.DownloadsAllowed(v, cfg.Enabled, cfg.AllowFriends, i.host.ViewerPolicy)
}

func (i *instance) downloadsFeature(ctx context.Context, v *frontend.Viewer) downloadCapability {
	quota, enabled := i.downloadPolicy(v)
	out := downloadCapability{Enabled: enabled}
	if enabled {
		if limit, remaining, ok := quota.RemainingDailyDownloads(ctx, v); ok {
			out.MaxPerDay, out.RemainingToday = &limit, &remaining
		}
	}
	return out
}

func (i *instance) grantDownload(w http.ResponseWriter, r *http.Request) {
	k, keyErr := key(r)
	logRefusal := func(reason string) {
		slog.DebugContext(r.Context(), "Vela download grant refused", "title_key", k, "reason", reason)
	}
	refuse := func(status int, class, code string, after int) {
		logRefusal(code)
		downloadProblem(w, status, class, code, downloadProblemMessage(code, requestDevice(r).Language), after)
	}
	conflict := func(code string) { refuse(http.StatusConflict, "source", code, 0) }
	failed := func(stage string, err error) {
		logRefusal(stage)
		fail(w, err, r)
	}
	v, _, err := i.viewer(r)
	if err != nil {
		failed("authentication_failed", err)
		return
	}
	quota, allowed := i.downloadPolicy(v)
	if !allowed {
		refuse(http.StatusForbidden, "policy", "download_not_allowed", 0)
		return
	}
	var req downloadGrantRequest
	if body(r, &req) != nil {
		refuse(http.StatusBadRequest, "request", "invalid_request", 0)
		return
	}
	if header := strings.TrimSpace(r.Header.Get("Idempotency-Key")); header != "" {
		req.IdempotencyKey = header
	}
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 256 || len(req.VersionID) > 512 {
		refuse(http.StatusBadRequest, "request", "invalid_request", 0)
		return
	}
	if keyErr != nil {
		refuse(http.StatusNotFound, "not-found", "not_found", 0)
		return
	}
	if k.Kind() == frontend.KindShow || k.Kind() == frontend.KindSeason {
		conflict("not_downloadable_kind")
		return
	}
	ctx := frontend.WithLiveApp(r.Context(), "vela")
	dev := requestDevice(r)
	declared := req.Client
	if !declared.OriginalPlayback.Valid() {
		declared = i.negotiatedClientFor(r, dev)
	}
	// Keep device-specific owner rules tied to the authenticated device.
	declared.Device.ID = dev.ID
	capability, err := i.clientCapabilities(r, v, declared)
	if err != nil {
		failed("capability_build_failed", err)
		return
	}
	caps := capability.Client
	if p, ok := i.host.ViewerPolicy.(frontend.PlaybackConfirmationPolicy); ok {
		v, err = p.ConfirmPlayback(ctx, v, caps.Remote, i.serverNow())
		if err != nil || v == nil {
			refuse(http.StatusForbidden, "policy", "cannot_play_now", 0)
			return
		}
	} else if p, ok := i.host.ViewerPolicy.(frontend.PlaybackAccessPolicy); ok && !p.CanPlay(v, caps.Remote, i.serverNow()) {
		refuse(http.StatusForbidden, "policy", "cannot_play_now", 0)
		return
	}
	detail, err := i.host.Experience.Item(frontend.WithColdDetail(ctx, k), frontend.ItemRequest{Viewer: v, Device: dev, Key: k})
	if err != nil {
		switch {
		case errors.Is(err, frontend.ErrUnreleased):
			conflict("title_unavailable")
		case errors.Is(err, frontend.ErrNotFound):
			refuse(http.StatusNotFound, "not-found", "not_found", 0)
		default:
			failed("title_lookup_failed", err)
		}
		return
	}
	switch {
	case detail.Title.Key != k:
		conflict("title_unavailable")
		return
	case detail.Title.Type == frontend.TypeShow || detail.Title.Type == frontend.TypeSeason || reading.IsBookTitle(detail.Title):
		conflict("not_downloadable_kind")
		return
	case detail.Unavailable != nil || detail.Title.IsFutureRelease(i.serverNow()):
		conflict("title_unavailable")
		return
	case detail.CanPlay != nil && !*detail.CanPlay:
		conflict("cannot_play_now")
		return
	case !declared.OriginalPlayback.Valid():
		conflict("capabilities_unknown")
		return
	}
	receipt := hashJSON([]string{k.String(), req.IdempotencyKey})
	if runtime, ok := i.host.Sessions.(frontend.SessionRuntime); ok && runtime.LifecycleGates() != nil {
		unlock, err := runtime.LifecycleGates().Acquire(ctx, "vela-download/"+strconv.FormatInt(v.ID, 10)+"/"+receipt)
		if err != nil {
			failed("receipt_lock_failed", err)
			return
		}
		defer unlock()
	}
	binding, err := quota.OfflineDownloadReceipt(ctx, v, receipt)
	if err != nil {
		failed("receipt_lookup_failed", err)
		return
	}
	scope := frontend.VersionScope{Viewer: v, Device: dev, Protocol: "vela", Caps: caps}
	release := binding.Release
	if binding.ETag != "" {
		if req.VersionID != "" && req.VersionID != binding.VersionID {
			conflict("version_changed")
			return
		}
	} else if req.VersionID != "" {
		target, err := i.resolveDownloadVersion(ctx, scope, k, req.VersionID)
		switch {
		case errors.Is(err, frontend.ErrNotFound), errors.Is(err, frontend.ErrVersionUnavailable):
			conflict("version_not_found")
			return
		case err != nil:
			failed("version_lookup_failed", err)
			return
		case target.Item != k || target.Release == "":
			conflict("version_not_found")
			return
		}
		release = target.Release
		binding.VersionID = req.VersionID
	}
	if !i.downloads.reserve() {
		refuse(http.StatusServiceUnavailable, "capacity", "resource_capacity", 5)
		return
	}
	var source *frontend.OfflineDownloadSource
	issued := false
	defer func() {
		if !issued {
			i.downloads.closeSource(source)
		}
	}()

	if policy, ok := i.host.ViewerPolicy.(frontend.DownloadLimitPolicy); ok {
		caps.MaxHeight = lowerDownloadHeight(caps.MaxHeight, policy.DownloadAllowance(v).MaxHeight)
	}
	caps.MaxHeight = lowerDownloadHeight(caps.MaxHeight, i.host.HostConfig().Downloads.MaxHeight)
	source, err = i.host.OfflineDownloads.OpenOfflineDownload(ctx, v, &detail.Title, caps, release, declared.OriginalPlayback)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, frontend.ErrPreparing):
			failed("source_open_failed", err)
		case errors.Is(err, frontend.ErrUnreleased):
			conflict("title_unavailable")
		case errors.Is(err, frontend.ErrSourceChanged):
			conflict("version_changed")
		case errors.Is(err, frontend.ErrVersionUnavailable), errors.Is(err, frontend.ErrNotAvailable):
			conflict("version_unavailable")
		default:
			failed("source_open_failed", err)
		}
		return
	}
	if source == nil || source.ReaderAt == nil || source.Size <= 0 || source.Release == "" || source.ETag == "" {
		conflict("version_unavailable")
		return
	}
	if binding.ETag != "" && (binding.ETag != source.ETag || binding.Release != source.Release) {
		conflict("version_changed")
		return
	}
	if binding.VersionID == "" {
		if handles, ok := i.host.VersionHandles.(frontend.ListedVersionHandles); ok {
			binding.VersionID = handles.ListedHandle(scope, k, source.Release)
		}
	}
	binding.Release, binding.ETag = source.Release, source.ETag
	token, err := mintReadTicketToken()
	if err != nil {
		failed("ticket_mint_failed", err)
		return
	}
	decision, err := quota.AdmitOfflineDownload(ctx, v, k, receipt, binding, source.Size)
	if errors.Is(err, frontend.ErrSourceChanged) {
		conflict("version_changed")
		return
	}
	if err != nil {
		failed("quota_admission_failed", err)
		return
	}
	if !decision.Allowed {
		wait := decision.ResetAt.Sub(i.serverNow())
		retry := max(1, int((wait+time.Second-1)/time.Second))
		refuse(http.StatusTooManyRequests, "policy", "download_limit", retry)
		return
	}
	url := "/vela/v0/download/" + token
	audio, subs := offlineDownloadTracks(source.Media, declared.SubtitleFormats)
	external, sidecars := i.downloadSidecars(ctx, v, &detail.Title, source, url)
	markers := i.skipMarkers(ctx, k, source.Media)
	expires := i.serverNow().Truncate(time.Second).Add(downloadTicketTTL)
	filename := downloadFilename(source.Name, detail.Title.Title)
	issued = i.downloads.issue(token, &downloadTicket{source: source, expiresAt: expires, filename: filename, sidecars: sidecars}, expires.Sub(i.serverNow()))
	if !issued {
		refuse(http.StatusForbidden, "policy", "feature_unavailable", 0)
		return
	}
	send(w, http.StatusOK, downloadGrant{
		TitleID: ids.RatingKeyGUID(k), VersionID: binding.VersionID, URL: url,
		ExpiresAt: expires, ETag: source.ETag, SourceID: source.ETag, ByteLength: source.Size, MIMEType: source.MIME, Filename: filename,
		Container: source.Media.Container, OriginalVideo: originalVideo(source.Media),
		AudioTracks: audio, SubtitleTracks: append(subs, external...), Chapters: chapters(source.Media), Markers: markers,
	})
}

func (i *instance) resolveDownloadVersion(ctx context.Context, scope frontend.VersionScope, key frontend.RatingKey, version string) (frontend.VersionTarget, error) {
	handles, ok := i.host.VersionHandles.(frontend.OfflineVersionHandles)
	if !ok {
		return frontend.VersionTarget{}, frontend.ErrVersionUnavailable
	}
	return handles.ResolveOfflineVersion(ctx, scope, key, version)
}

func lowerDownloadHeight(current, limit int) int {
	if limit > 0 && (current == 0 || limit < current) {
		return limit
	}
	return current
}

func downloadFilename(name, title string) string {
	clean := func(s string) string {
		return strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, path.Base(strings.ReplaceAll(s, "\\", "/"))))
	}
	if name = clean(name); name != "" && name != "." && name != ".." && name != "/" {
		return name
	}
	if title = clean(title); title != "" && title != "." && title != ".." && title != "/" {
		return title
	}
	return "video"
}

// Error codes are localizable by the client. No source diagnostics or
// provider identifiers are included in the download response.
func downloadProblem(w http.ResponseWriter, status int, class, code, message string, after int) {
	if after <= 0 {
		problemMessage(w, status, class, code, message)
		return
	}
	w.Header().Set("Retry-After", strconv.Itoa(after))
	retry := object{"allowed": true, "afterSeconds": after}
	send(w, status, object{"error": object{"class": class, "code": code, "message": message, "retry": retry}})
}

func (i *instance) downloadFile(w http.ResponseWriter, r *http.Request) {
	ticket := i.downloads.acquire(r.PathValue("ticket"), i.serverNow())
	if ticket == nil {
		http.NotFound(w, r)
		return
	}
	defer i.downloads.release(ticket)
	source := ticket.source
	mediasafety.SetHeaders(w)
	w.Header().Set("ETag", source.ETag)
	w.Header().Set("Content-Type", source.MIME)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": ticket.filename}))
	reader := io.NewSectionReader(reading.ContextReaderAt(r.Context(), source.ReaderAt), 0, source.Size)
	rangefile.Serve(w, r, ticket.filename, "", source.Size, time.Time{}, reader)
}

func (d *downloadTickets) reserve() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.inUse >= maxDownloadSources {
		return false
	}
	d.inUse++
	return true
}

func (d *downloadTickets) issue(token string, entry *downloadTicket, ttl time.Duration) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	if d.entries == nil {
		d.entries = make(map[string]*downloadTicket)
	}
	d.entries[token] = entry
	entry.timer = time.AfterFunc(ttl, func() { d.expire(token) })
	return true
}

func (d *downloadTickets) acquire(token string, now time.Time) *downloadTicket {
	d.mu.Lock()
	defer d.mu.Unlock()
	entry := d.entries[token]
	if d.closed || entry == nil || !now.Before(entry.expiresAt) {
		return nil
	}
	entry.readers++
	return entry
}

func (d *downloadTickets) release(entry *downloadTicket) {
	d.mu.Lock()
	entry.readers--
	closeNow := entry.expired && entry.readers == 0
	d.mu.Unlock()
	if closeNow {
		d.closeSource(entry.source)
	}
}

func (d *downloadTickets) expire(token string) {
	d.mu.Lock()
	entry := d.entries[token]
	delete(d.entries, token)
	closeNow := false
	if entry != nil {
		entry.timer.Stop()
		entry.expired = true
		closeNow = entry.readers == 0
	}
	d.mu.Unlock()
	if closeNow {
		d.closeSource(entry.source)
	}
}

func (d *downloadTickets) close() {
	d.mu.Lock()
	d.closed = true
	tokens := make([]string, 0, len(d.entries))
	for token := range d.entries {
		tokens = append(tokens, token)
	}
	d.mu.Unlock()
	for _, token := range tokens {
		d.expire(token)
	}
}

func (d *downloadTickets) closeSource(source *frontend.OfflineDownloadSource) {
	if source != nil && source.Closer != nil {
		_ = source.Close()
	}
	d.mu.Lock()
	d.inUse--
	d.mu.Unlock()
}
