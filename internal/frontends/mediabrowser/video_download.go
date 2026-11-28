package mediabrowser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/mediasafety"
	"github.com/braedonsaunders/ether/internal/frontend/kit/rangefile"
)

func (i *instance) allowDownloads(viewer *frontend.Viewer) bool {
	policy, ok := i.host.ViewerPolicy.(frontend.DownloadAccessPolicy)
	if viewer == nil {
		return false
	}
	if ok {
		return policy.AllowDownloads(viewer)
	}
	// Plex applies no download restriction when the host omits the policy.
	runtime, ok := i.host.Sessions.(frontend.SessionRuntime)
	return ok && runtime.Playback() != nil
}

func downloadableVideo(title frontend.Title) bool {
	switch mbItemType(title) {
	case "Movie", "Episode", "Video":
		return !mbIsBookItem(title)
	default:
		return false
	}
}

func (i *instance) setVideoCanDownload(viewer *frontend.Viewer, dto map[string]any, title frontend.Title) {
	if downloadableVideo(title) {
		dto["CanDownload"] = i.allowDownloads(viewer)
	}
}

func (i *instance) serveItemFile(w http.ResponseWriter, r *http.Request, attachment bool) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx := i.identityContext(r)
	wireID := r.PathValue("id")
	key, err := ids.ParseRatingKeyGUID(wireID)
	release := ""
	if err != nil && i.host.VersionHandles != nil {
		if r.Method == http.MethodHead {
			if lookup, ok := i.host.VersionHandles.(frontend.StoredVersionLookup); ok {
				scope := frontend.VersionScope{Viewer: viewer, Device: device(r), Protocol: i.protocol}
				if target, e := lookup.LookupStoredVersion(ctx, scope, wireID); e == nil && target.Item != 0 {
					key, release, err = target.Item, target.Release, nil
				}
			}
		} else if scope, e := i.versionScope(r, viewer); e == nil {
			if target, e := i.host.VersionHandles.Resolve(ctx, scope, wireID); e == nil && target.Item != 0 {
				key, release, err = target.Item, target.Release, nil
			}
		}
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	detail, err := i.storedItem(r, viewer, key)
	if err != nil || detail.Title.Key != key {
		http.NotFound(w, r)
		return
	}
	if mbIsBookItem(detail.Title) && key.IsPluginKey() {
		i.serveBookFile(w, r, viewer, detail, attachment)
		return
	}
	if !downloadableVideo(detail.Title) {
		http.NotFound(w, r)
		return
	}
	if !i.allowDownloads(viewer) {
		if _, hasPolicy := i.host.ViewerPolicy.(frontend.DownloadAccessPolicy); hasPolicy {
			http.Error(w, "Forbidden", http.StatusForbidden)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	runtime, ok := i.host.Sessions.(frontend.SessionRuntime)
	if !ok || runtime.Playback() == nil {
		http.NotFound(w, r)
		return
	}
	if detail.Unavailable != nil || (detail.CanPlay != nil && !*detail.CanPlay) {
		http.NotFound(w, r)
		return
	}
	i.serveVideoDownload(w, r, ctx, viewer, detail, release, runtime)
}

func (i *instance) serveVideoDownload(w http.ResponseWriter, r *http.Request, ctx context.Context, viewer *frontend.Viewer, detail frontend.ItemDetail, release string, runtime frontend.SessionRuntime) {
	title := detail.Title
	dev := device(r)
	client := frontend.ClientCaps{Protocol: i.protocol, Remote: true}
	if i.host.Capabilities != nil {
		capability, err := i.host.Capabilities.Build(ctx, frontend.CapabilityRequest{
			Viewer: viewer, Device: caps.DeviceInfo{Protocol: i.protocol, ClientID: dev.ID, Product: dev.Client, DeviceName: dev.DeviceName, Version: dev.Version},
			UserAgent: r.UserAgent(),
		})
		if err != nil {
			writeVideoDownloadError(i.protocol, w, err, r.Header.Get("Accept-Language"))
			return
		}
		client = capability.Client
	}
	if p, ok := i.host.ViewerPolicy.(frontend.PlaybackAccessPolicy); ok && !p.CanPlay(viewer, client.Remote, time.Now()) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	// Keep download source sessions separate from the device's current playback.
	client.ClientID = "download:" + title.Key.String() + ":" + release + ":" + dev.ID
	client.Protocol = i.protocol
	client.NoPlaybackTelemetry = true
	client.DeferSourceCodecValidation = true
	if i.host.HostConfig != nil {
		if height := i.host.HostConfig().Downloads.MaxHeight; height > 0 && (client.MaxHeight == 0 || client.MaxHeight > height) {
			client.MaxHeight = height
		}
	}
	policy, _ := i.host.ViewerPolicy.(frontend.DownloadLimitPolicy)
	if policy != nil {
		if height := policy.DownloadAllowance(viewer).MaxHeight; height > 0 && (client.MaxHeight == 0 || client.MaxHeight > height) {
			client.MaxHeight = height
		}
	}
	if r.Method == http.MethodHead {
		i.headVideoDownload(w, r, viewer, detail, runtime, client, dev.ID, release)
		return
	}
	r = r.WithContext(frontend.WithDownloadRange(r.Context()))
	unlock := func() {}
	if gates := runtime.LifecycleGates(); gates != nil && policy != nil {
		// A parallel retry must not inherit a reservation that is about to be released.
		releaseGate, err := gates.Acquire(ctx, fmt.Sprintf("download/%d/%s", viewer.ID, title.Key))
		if err != nil {
			return
		}
		unlock = releaseGate
		defer unlock()
	}
	reserved, served := false, false
	if policy != nil {
		info := runtime.Playback().Estimate(ctx, &title, client)
		estimate := downloadSize(info)
		if client.MaxBitrateKbps > 0 && info.Duration > 0 && (info.BitrateKbps == 0 || client.MaxBitrateKbps < info.BitrateKbps) {
			estimate = int64(client.MaxBitrateKbps) * 1000 / 8 * int64(info.Duration.Seconds())
		}
		decision := policy.AdmitDownload(ctx, viewer, title.Key, estimate)
		if !decision.Allowed {
			writeDownloadLimit(w, decision)
			return
		}
		reserved = decision.Reserved
		defer func() {
			if reserved && !served {
				policy.ReleaseDownloadReservation(context.WithoutCancel(ctx), viewer, title.Key)
			}
		}()
	}

	// Zero uses the playback runtime's normal preparation deadline, as in Plex.
	source, err := runtime.PrepareSource(ctx, viewer, title.Key, client, release, release != "", 0)
	if err != nil {
		writeVideoDownloadError(i.protocol, w, err, r.Header.Get("Accept-Language"))
		return
	}
	if source == nil || source.ID == "" {
		streamError(i.protocol, w, frontend.ErrNotAvailable, r.Header.Get("Accept-Language"))
		return
	}
	if policy != nil {
		size := mediaSourceSize(source.Info)
		if source.Source != nil && source.Source.Size() > 0 {
			size = source.Source.Size()
		}
		if size > 0 {
			if decision := policy.ReconcileDownloadBytes(ctx, viewer, title.Key, size); !decision.Allowed {
				writeDownloadLimit(w, decision)
				return
			}
		}
	}
	mediasafety.SetHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": videoDownloadFilename(title, source)}))
	served = true
	unlock()
	runtime.ServePartSource(w, r, source.ID)
}

// A size probe can use a matching session or a known file size from item
// metadata. There is no trustworthy Content-Length for an unresolved title.
func (i *instance) headVideoDownload(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, detail frontend.ItemDetail, runtime frontend.SessionRuntime, client frontend.ClientCaps, playbackClient, release string) {
	title := detail.Title
	mediasafety.SetHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	clients := []string{client.ClientID}
	if playbackClient != "" && release != "" {
		clients = append(clients, playbackClient)
	}
	for _, id := range clients {
		part := runtime.PartSource(viewer.ID, id, title.Key, release)
		if !part.Matches {
			if scoped, ok := runtime.Playback().(frontend.SurfaceScopedPlayback); ok {
				part.Session, part.Matches = scoped.SessionForSurface(string(i.protocol), viewer.ID, id, title.Key, release, release != "")
			}
		}
		if part.Matches && part.Session != nil && part.Session.Source != nil {
			if client.MaxHeight > 0 {
				video := part.Session.Info.Video()
				if video != nil && video.Height > client.MaxHeight {
					continue
				}
			}
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": videoDownloadFilename(title, part.Session)}))
			runtime.ServePartSource(w, r.WithContext(frontend.WithDownloadRange(r.Context())), part.Session.ID)
			return
		}
	}
	facts := &frontend.PlaySession{}
	for _, version := range detail.Versions {
		if release != "" && version.ID != release {
			continue
		}
		if mediaSourceSize(version.Media) > 0 {
			facts.Info = version.Media
			break
		}
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": videoDownloadFilename(title, facts)}))
	if size := mediaSourceSize(facts.Info); size > 0 {
		w.Header().Set("Content-Type", "application/octet-stream")
		// HEAD only needs the representation's size and never reads it.
		rangefile.Serve(w, r, "", "", size, time.Time{}, io.NewSectionReader(nil, 0, size))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func downloadSize(info frontend.MediaInfo) int64 {
	if info.Size > 0 {
		return info.Size
	}
	if info.BitrateKbps > 0 && info.Duration > 0 {
		return int64(info.BitrateKbps) * 1000 / 8 * int64(info.Duration.Seconds())
	}
	return 0
}

func videoDownloadFilename(title frontend.Title, source *frontend.PlaySession) string {
	clean := func(name string) string {
		return strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, path.Base(strings.ReplaceAll(name, "\\", "/"))))
	}
	if source.Source != nil {
		if name := clean(source.Source.Name()); path.Ext(name) != "" && name != ".." {
			return name
		}
	}
	name := clean(title.Title)
	if name == "" || name == "." || name == ".." {
		name = "video"
	}
	if ext := clean(source.Info.Container); ext != "" && ext != "." && ext != ".." {
		name += "." + ext
	}
	return name
}

func writeDownloadLimit(w http.ResponseWriter, decision frontend.DownloadLimitDecision) {
	if wait := int(time.Until(decision.ResetAt).Seconds()); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(wait))
	}
	http.Error(w, frontend.DownloadLimitMessage(decision), http.StatusTooManyRequests)
}

func writeVideoDownloadError(protocol frontend.Protocol, w http.ResponseWriter, err error, languages ...string) {
	switch {
	case errors.Is(err, frontend.ErrUnauthorized):
		http.Error(w, "Forbidden", http.StatusForbidden)
	case errors.Is(err, frontend.ErrDownloadBudget), errors.Is(err, frontend.ErrDownloadLimit), errors.Is(err, frontend.ErrDownloadPaused):
		http.Error(w, "Download temporarily unavailable", http.StatusTooManyRequests)
	default:
		streamError(protocol, w, err, languages...)
	}
}
