package frontendadapter

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/rangefile"
	"github.com/braedonsaunders/ether/internal/kindstream"
	"github.com/braedonsaunders/ether/internal/plugins"
)

// KindMediaPath is the Ether-owned endpoint a plugin embeds in an adapter
// response (spec §6.5). The path segment after it is the opaque ticket.
const KindMediaPath = "/ether/v1/kind-media/"

// MediaDeps are the seams the kind-media endpoint needs. The app fills them
// from pluginitems + the kind resolver; the endpoint never touches tables.
type MediaDeps struct {
	// Tickets redeems the scoped, short-lived grant.
	Tickets *TicketIssuer
	// Viewer loads the current policy user by id, so the library and rating
	// checks use live restrictions rather than a stale ticket snapshot.
	Viewer func(ctx context.Context, id int64) (*core.Viewer, error)
	// Open opens one plugin item ref for the granted viewer, honouring the
	// request's accepted MIME types. It runs the same authorization path
	// kind playback uses.
	Open func(ctx context.Context, viewer *core.Viewer, itemRef string, accept []string) (*frontend.KindMedia, error)
	// OpenPart opens one zero-based part of a multi-part plugin item ref for
	// the granted viewer (#576). Used when the ticket is part-scoped or when
	// the request names ?part=<n>.
	OpenPart func(ctx context.Context, viewer *core.Viewer, itemRef string, partIndex int, accept []string) (*frontend.KindMedia, error)
	// OpenSource opens a named resolver source (and optional zero-based part)
	// for a source-scoped ticket. The grant controls whether other sources can play.
	OpenSource func(ctx context.Context, viewer *core.Viewer, itemRef string, partIndex *int, source string, fallback bool, accept []string) (*frontend.KindMedia, error)
	// TrackWait adds an active preparation wait to plugin diagnostics.
	TrackWait func(pluginID string, budget time.Duration, cancel context.CancelFunc) func()
	// Log records each refused request with its cause (optional).
	Log *slog.Logger
}

// MediaHandler serves /ether/v1/kind-media/{ticket}: it redeems the ticket,
// loads the viewer, opens the item (or one part of it) through KindStreams
// and streams the bytes with Range support. The ticket is the only
// credential the request needs.
type MediaHandler struct {
	deps MediaDeps
}

// NewMediaHandler builds the endpoint handler. It returns nil when the
// dependencies are incomplete, so app wiring can leave the route unmounted.
func NewMediaHandler(deps MediaDeps) *MediaHandler {
	if deps.Tickets == nil || deps.Viewer == nil || deps.Open == nil {
		return nil
	}
	return &MediaHandler{deps: deps}
}

func (h *MediaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, KindMediaPath)
	if token == "" || strings.Contains(token, "/") {
		h.refuse(w, http.StatusNotFound, "ticket_invalid", "The media ticket is missing or malformed.", nil, "")
		return
	}
	grant, err := h.deps.Tickets.Redeem(token)
	if err != nil {
		h.refuse(w, http.StatusNotFound, "ticket_invalid", "The media ticket is unknown, expired or out of scope.", err, "")
		return
	}
	h.ServeGrant(w, r, grant)
}

// ServeGrant serves the item a grant authorizes, exactly as a redeemed ticket
// would. Callers that authenticated the viewer themselves (a frontend adapter
// answering with a FrontendMedia descriptor) use it after re-checking that the
// item belongs to the plugin and that its library is visible to the viewer.
func (h *MediaHandler) ServeGrant(w http.ResponseWriter, r *http.Request, grant StreamGrant) {
	var err error
	var partIdx *int
	if rawPart := strings.TrimSpace(r.URL.Query().Get("part")); rawPart != "" {
		n, perr := strconv.Atoi(rawPart)
		if perr != nil || n < 0 {
			h.refuse(w, http.StatusBadRequest, "invalid_part", "The requested part index must be a non-negative integer.", perr, grant.ItemID)
			return
		}
		if grant.PartIndex != nil && *grant.PartIndex != n {
			h.refuse(w, http.StatusNotFound, "ticket_invalid", "The media ticket is unknown, expired or out of scope.", ErrTicketInvalid, grant.ItemID)
			return
		}
		partIdx = &n
	} else if grant.PartIndex != nil {
		n := *grant.PartIndex
		partIdx = &n
	}

	viewer, err := h.deps.Viewer(r.Context(), grant.ViewerID)
	if err != nil || viewer == nil {
		h.refuse(w, http.StatusForbidden, "viewer_invalid", "The viewer this ticket was issued to can no longer play.", err, grant.ItemID)
		return
	}
	accept := parseAccept(r.Header.Get("Accept"))
	var open func(context.Context) (*frontend.KindMedia, error)
	switch {
	case grant.Source != "":
		if h.deps.OpenSource == nil {
			h.refuse(w, http.StatusConflict, "no_source", "The item has no playable source.", core.ErrNotAvailable, grant.ItemID)
			return
		}
		open = func(ctx context.Context) (*frontend.KindMedia, error) {
			return h.deps.OpenSource(ctx, viewer, grant.ItemID, partIdx, grant.Source, grant.Fallback, accept)
		}
	case partIdx != nil:
		if h.deps.OpenPart == nil {
			h.refuse(w, http.StatusNotFound, "item_not_found", "The requested item part is not available.", core.ErrNotFound, grant.ItemID)
			return
		}
		open = func(ctx context.Context) (*frontend.KindMedia, error) {
			return h.deps.OpenPart(ctx, viewer, grant.ItemID, *partIdx, accept)
		}
	default:
		open = func(ctx context.Context) (*frontend.KindMedia, error) {
			return h.deps.Open(ctx, viewer, grant.ItemID, accept)
		}
	}
	ctx := r.Context()
	if grant.Wait > 0 && h.deps.Tickets.waits.acquire(grant.Adapter) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(plugins.WithFrontendPreparationHold(ctx))
		defer cancel() // The returned stream may still use this context.
		openOnce := open
		open = func(ctx context.Context) (*frontend.KindMedia, error) {
			defer h.deps.Tickets.waits.release(grant.Adapter)
			return h.waitForMedia(ctx, cancel, grant.Adapter, min(grant.Wait, maxDownloadWait), openOnce)
		}
	}
	media, err := open(ctx)
	if r.Context().Err() != nil {
		if media != nil {
			_ = media.Closer.Close()
		}
		return
	}
	if err != nil || media == nil {
		if frontend.WritePlaybackPending(w, err, r.Header.Get("Accept-Language")) {
			return
		}
		if errors.Is(err, core.ErrJobsBusy) {
			w.Header().Set("Retry-After", strconv.Itoa(core.JobsBusyRetrySeconds(err, 5)))
		} else if errors.Is(err, plugins.ErrBusy) {
			w.Header().Set("Retry-After", "5")
		}
		status, code, message := openFailure(err)
		h.refuse(w, status, code, message, err, grant.ItemID)
		return
	}
	defer media.Closer.Close()
	h.serve(w, r, media)
}

// refuse answers a failed kind-media request in the API's error shape, with
// the code in X-Ether-Media-Error, and logs the cause host-side. Upstream
// error text stays in the log: it can name internal hosts or paths.
func (h *MediaHandler) refuse(w http.ResponseWriter, status int, code, message string, err error, item string) {
	if h.deps.Log != nil {
		args := []any{"code", code, "status", status, "message", message}
		if item != "" {
			args = append(args, "item", item)
		}
		if err != nil {
			args = append(args, "err", err)
		}
		h.deps.Log.Info("kind media refused", args...)
	}
	w.Header().Set("X-Ether-Media-Error", code)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// openFailure classifies why an authorized item could not be opened.
func openFailure(err error) (int, string, string) {
	switch {
	case errors.Is(err, core.ErrJobsBusy):
		return http.StatusServiceUnavailable, "jobs_busy", core.JobsBusyMessage(err)
	case err == nil, errors.Is(err, core.ErrNotAvailable):
		return http.StatusConflict, "no_source", "The item has no playable source."
	case errors.Is(err, core.ErrNotFound):
		return http.StatusNotFound, "item_not_found", "The item is not in a library this viewer can open."
	case errors.Is(err, core.ErrArchiveUnstreamable):
		return http.StatusUnprocessableEntity, "archive_unstreamable", "This archive format or compression method cannot be streamed."
	case errors.Is(err, core.ErrReleaseRejected), errors.Is(err, core.ErrNoUsableFile), errors.Is(err, core.ErrArticleMissing), errors.Is(err, core.ErrArticleCorrupt):
		return http.StatusUnprocessableEntity, "source_unplayable", "The item's source is incomplete or cannot be played."
	case errors.Is(err, core.ErrSourceUnavailable), errors.Is(err, plugins.ErrBusy):
		return http.StatusServiceUnavailable, "source_unavailable", "The item's source is temporarily unavailable."
	default:
		return http.StatusBadGateway, "open_failed", "The item's source could not be opened."
	}
}

// serve streams the media with Range support. ServeContent needs an
// io.ReadSeeker, so the ReaderAt is wrapped in a SectionReader over the
// whole object.
func (h *MediaHandler) serve(w http.ResponseWriter, r *http.Request, media *frontend.KindMedia) {
	mimeType := strings.TrimSpace(media.MIME)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	if media.Size <= 0 {
		// Unknown size: stream once, no Range.
		w.Header().Set("Content-Type", mimeType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, io.NewSectionReader(media.ReaderAt, 0, 1<<62))
		return
	}
	name := media.Name
	if name == "" {
		name = "media"
	}
	w.Header().Set("Content-Type", mimeType)
	// One read-ahead buffer per response: ServeContent copies 32 KiB at a
	// time, and a range source would otherwise make one upstream request
	// per copy (#666).
	ra := kindstream.ReadAhead(r.Context(), media.ReaderAt, media.Size)
	rangefile.Serve(w, r, name, cmp.Or(media.Release.ID, name), media.Size, media.Release.PubDate, io.NewSectionReader(ra, 0, media.Size))
}

// parseAccept splits an Accept header into media types, lowercased and
// trimmed; a plugin's resolver uses it to pick a format.
func parseAccept(header string) []string {
	if strings.TrimSpace(header) == "" {
		return nil
	}
	parts := strings.Split(header, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if mt := strings.TrimSpace(strings.SplitN(p, ";", 2)[0]); mt != "" {
			out = append(out, mt)
		}
	}
	return out
}
