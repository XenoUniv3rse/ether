package mediabrowser

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/clientnet"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/rangefile"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

func (i *instance) localTrailers(w http.ResponseWriter, r *http.Request) {
	i.writeExtras(w, r, true)
}

func (i *instance) specialFeatures(w http.ResponseWriter, r *http.Request) {
	i.writeExtras(w, r, false)
}

func (i *instance) writeExtras(w http.ResponseWriter, r *http.Request, trailers bool) {
	detail, ok := i.authorizedItem(w, r)
	if !ok {
		return
	}
	items := []any{}
	for _, extra := range detail.Extras {
		if extra.Key == 0 || isTrailerExtra(extra) != trailers {
			continue
		}
		items = append(items, i.extraItem(r, detail, extra))
	}
	jsonReply(w, items)
}

func isTrailerExtra(extra frontend.Extra) bool {
	return extra.Kind == "trailer" || extra.Kind == "teaser"
}

func (i *instance) extraItem(r *http.Request, parent frontend.ItemDetail, clip frontend.Extra) map[string]any {
	id := ids.RatingKeyGUID(clip.Key)
	parentKey := clip.ParentKey
	if parentKey == 0 {
		parentKey = parent.Title.Key
	}
	name := clip.Title
	if name == "" {
		name = "Trailer"
	}
	kind := "Video"
	if isTrailerExtra(clip) {
		kind = "Trailer"
	}
	item := map[string]any{
		"Id": id, "Name": name, "Type": kind, "MediaType": "Video", "IsFolder": false,
		"ServerId": i.info.ID, "ParentId": ids.RatingKeyGUID(parentKey),
		"UserData":          i.userDataDTO(id, frontend.WatchStatus{}),
		"ImageTags":         map[string]string{},
		"BackdropImageTags": []string{},
	}
	if clip.Duration > 0 {
		item["RunTimeTicks"] = clip.Duration.Nanoseconds() / 100
	}
	if !clip.PublishedAt.IsZero() {
		item["PremiereDate"] = wireTime(clip.PublishedAt)
	}
	height := clip.Height
	if height <= 0 {
		height = 720
	}
	media := frontend.MediaInfo{Container: "mp4", Duration: clip.Duration, Estimated: true, Streams: []frontend.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: height * 16 / 9, Height: height, Default: true},
		{Index: 1, Type: "audio", Codec: "aac", Channels: 2, Language: clip.Language, Default: true},
	}}
	source := i.mediaSourceDTO(id, media)
	playable := i.host.ExtraStreams != nil || (clip.Key.IsPreroll() && i.host.Prerolls != nil)
	source["SupportsDirectPlay"] = playable
	source["SupportsDirectStream"] = playable
	source["SupportsTranscoding"] = false
	source["IsRemote"] = false
	if i.protocol == "emby" {
		i.embyMediaSourceDTO(source, id, media)
		delete(source, "ETag")
	} else {
		item["LocationType"] = "FileSystem"
		switch strings.ToLower(clip.Kind) {
		case "trailer", "teaser":
			item["ExtraType"] = "Trailer"
		case "behindthescenes":
			item["ExtraType"] = "BehindTheScenes"
		case "featurette":
			item["ExtraType"] = "Featurette"
		case "clip":
			item["ExtraType"] = "Clip"
		}
		item["Etag"] = itemEtag(id)
	}
	if i.protocol == "emby" {
		item["Etag"] = embyETag(id)
	}
	i.completeFileSource(r, source, frontend.Title{Key: clip.Key}, "")
	i.mediaSourceDialect(source)
	if i.protocol == "jellyfin" {
		jellyfinMediaSourceShape(source, name)
	}
	item["MediaStreams"] = source["MediaStreams"]
	item["Path"] = source["Path"]
	item["MediaSources"] = []any{source}
	return item
}

func (i *instance) intros(w http.ResponseWriter, r *http.Request) {
	detail, ok := i.authorizedItem(w, r)
	if !ok {
		return
	}
	limit := 2
	if values := route.Query(r.URL.Query(), "Limit"); len(values) == 1 {
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 0 {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		limit = n
	}
	var clips []frontend.Extra
	var preroll frontend.Extra
	prerollOrder := ""
	if i.host.Prerolls != nil && limit > 0 && (detail.Title.Type == "movie" || detail.Title.Type == "episode") {
		clip, order, ok, err := i.host.Prerolls.Intro(i.identityContext(r), detailViewer(r, i), device(r), detail, i.away(r))
		if err != nil {
			if errors.Is(err, frontend.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return
		}
		if ok && clip.Key != 0 {
			preroll, prerollOrder = clip, order
			limit--
		}
	}
	if i.host.Cinema != nil && limit > 0 && detail.Title.Type == "movie" {
		var err error
		clips, err = i.host.Cinema.Trailers(r.Context(), detailViewer(r, i), device(r), detail.Title.Key, limit)
		if err != nil {
			if errors.Is(err, frontend.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	if preroll.Key != 0 {
		if prerollOrder == frontend.PrerollAfterTrailers {
			clips = append(clips, preroll)
		} else {
			clips = append([]frontend.Extra{preroll}, clips...)
		}
	}
	items := make([]any, 0, len(clips))
	for _, clip := range clips {
		if clip.Key == 0 {
			continue
		}
		items = append(items, i.extraItem(r, detail, clip))
	}
	jsonReply(w, i.queryResult(items, len(items), 0))
}

func detailViewer(r *http.Request, i *instance) *frontend.Viewer {
	viewer, _, _ := i.viewer(r)
	return viewer
}

func (i *instance) authorizedItem(w http.ResponseWriter, r *http.Request) (frontend.ItemDetail, bool) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return frontend.ItemDetail{}, false
	}
	if i.host.Experience == nil {
		http.NotFound(w, r)
		return frontend.ItemDetail{}, false
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		scope, scopeErr := i.versionScope(r, viewer)
		if scopeErr != nil || i.host.VersionHandles == nil {
			http.NotFound(w, r)
			return frontend.ItemDetail{}, false
		}
		target, resolveErr := i.host.VersionHandles.Resolve(i.identityContext(r), scope, r.PathValue("id"))
		if resolveErr != nil || target.Item == 0 {
			http.NotFound(w, r)
			return frontend.ItemDetail{}, false
		}
		key = target.Item
	}
	detail, err := i.host.Experience.Item(frontend.WithColdDetail(i.identityContext(r), key), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: key})
	if err != nil {
		http.NotFound(w, r)
		return frontend.ItemDetail{}, false
	}
	return detail, true
}

func (i *instance) extraPlayback(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, key frontend.RatingKey, playback bool) bool {
	if key.IsPreroll() && i.host.Prerolls != nil {
		i.prerollPlayback(w, r, key)
		return true
	}
	if key.Kind() != frontend.KindExtra || i.host.ExtraStreams == nil {
		return false
	}
	clip, ok := i.host.ExtraStreams.Lookup(key)
	if !ok || clip.ParentKey == 0 || i.host.Experience == nil {
		http.NotFound(w, r)
		return true
	}
	if _, err := i.host.Experience.Item(i.identityContext(r), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: clip.ParentKey}); err != nil {
		http.NotFound(w, r)
		return true
	}
	item := i.extraItem(r, frontend.ItemDetail{Title: frontend.Title{Key: clip.ParentKey}}, clip)
	sources, _ := item["MediaSources"].([]any)
	if len(sources) != 1 {
		http.Error(w, "Playback unavailable", http.StatusServiceUnavailable)
		return true
	}
	source := sources[0].(map[string]any)
	if playback {
		container, rawURL, _, err := i.host.ExtraStreams.Open(r.Context(), key)
		if err != nil || !publicExtraURL(rawURL) {
			http.Error(w, "Playback unavailable", http.StatusServiceUnavailable)
			return true
		}
		if container != "" {
			source["Container"] = container
		}
	}
	jsonReply(w, map[string]any{"MediaSources": []any{source}, "PlaySessionId": ""})
	return true
}

func (i *instance) serveExtra(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, key frontend.RatingKey) {
	if key.IsPreroll() && i.host.Prerolls != nil {
		i.servePreroll(w, r, viewer, key)
		return
	}
	if i.host.ExtraStreams == nil {
		http.NotFound(w, r)
		return
	}
	clip, ok := i.host.ExtraStreams.Lookup(key)
	if !ok || clip.ParentKey == 0 || i.host.Experience == nil {
		http.NotFound(w, r)
		return
	}
	if _, err := i.host.Experience.Item(i.identityContext(r), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: clip.ParentKey}); err != nil {
		http.NotFound(w, r)
		return
	}

	if cached, ok := i.host.ExtraStreams.(frontend.CachedExtraStreams); ok {
		file, mime, err := cached.OpenCachedExtra(r.Context(), key)
		if err == nil {
			defer file.Close()
			w.Header().Set("Content-Type", mime)
			if err := rangefile.ServeFile(w, r, "trailer.mp4", "extra:"+key.String()+":"+filepath.Base(file.Name()), time.Time{}, file); err != nil {
				http.NotFound(w, r)
				return
			}
			return
		}
		if !errors.Is(err, frontend.ErrNotAvailable) {
			http.Error(w, "Playback unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	_, rawURL, headers, err := i.host.ExtraStreams.Open(r.Context(), key)
	if err != nil || !publicExtraURL(rawURL) {
		http.Error(w, "Playback unavailable", http.StatusServiceUnavailable)
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		http.Error(w, "Playback unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodHead {
		upstream.Method = http.MethodHead
	}
	if value := r.Header.Get("Range"); rangeHeader(value) {
		upstream.Header.Set("Range", value)
	}
	for name, value := range headers {
		if value == "" || strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n") {
			continue
		}
		upstream.Header.Set(name, value)
	}
	response, err := i.extraClient.Do(upstream)
	if err != nil {
		http.Error(w, "Playback unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		http.Error(w, "Playback unavailable", http.StatusBadGateway)
		return
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := response.Header.Get(name); value != "" && !strings.ContainsAny(value, "\r\n") {
			w.Header().Set(name, value)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "video/mp4")
	}
	w.WriteHeader(response.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, response.Body)
	}
}

func rangeHeader(value string) bool {
	if !strings.HasPrefix(value, "bytes=") || strings.ContainsAny(value, "\r\n") {
		return false
	}
	_, err := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	return err == nil && len(value) < 64
}

func publicExtraURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return false
	}
	host := parsed.Hostname()
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()) {
		return false
	}
	switch strings.ToLower(host) {
	case "localhost", "metadata.google.internal":
		return false
	}
	return true
}

func newExtraHTTPClient(proxy func(*http.Request) (*url.URL, error)) *http.Client {
	if proxy == nil {
		proxy = http.ProxyFromEnvironment
	}
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			Proxy:                 proxy,
			ResponseHeaderTimeout: 20 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !publicExtraURL(req.URL.String()) {
				return errors.New("refused trailer redirect")
			}
			return nil
		},
	}
}

// away reports whether the client is outside the local network.
func (i *instance) away(r *http.Request) bool {
	if i.host.ClientIP == nil {
		return false
	}
	ip, ok := i.host.ClientIP(r)
	if !ok {
		return true
	}
	return !clientnet.LAN(ip)
}

// prerollPlayback answers PlaybackInfo for a pre-roll: a direct-play MP4.
func (i *instance) prerollPlayback(w http.ResponseWriter, r *http.Request, key frontend.RatingKey) {
	clip, ok := i.host.Prerolls.Lookup(r.Context(), key)
	if !ok {
		http.NotFound(w, r)
		return
	}
	item := i.extraItem(r, frontend.ItemDetail{Title: frontend.Title{Key: clip.ParentKey}}, clip)
	sources, _ := item["MediaSources"].([]any)
	if len(sources) != 1 {
		http.Error(w, "Playback unavailable", http.StatusServiceUnavailable)
		return
	}
	source := sources[0].(map[string]any)
	jsonReply(w, map[string]any{"MediaSources": []any{source}, "PlaySessionId": ""})
}

// servePreroll streams the converted pre-roll with byte ranges and
// records the play.
func (i *instance) servePreroll(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, key frontend.RatingKey) {
	file, modified, err := i.host.Prerolls.Open(r.Context(), key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	if r.Method != http.MethodHead {
		i.host.Prerolls.Started(context.WithoutCancel(r.Context()), viewer, device(r), key)
	}
	w.Header().Set("Content-Type", "video/mp4")
	rangefile.ServeSeeker(w, r, "", "preroll:"+key.String(), modified, file)
}

// writeExtraDetail answers an item request for a trailer or pre-roll. Jellyfin
// Web reads each intro's item before playing it; the detail route answered 404
// for these keys, which made the player give up on the feature too ("Unable to
// find a valid media source").
func (i *instance) writeExtraDetail(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, key frontend.RatingKey) bool {
	var clip frontend.Extra
	var ok bool
	switch {
	case key.IsPreroll() && i.host.Prerolls != nil:
		clip, ok = i.host.Prerolls.Lookup(r.Context(), key)
	case key.Kind() == frontend.KindExtra && i.host.ExtraStreams != nil:
		clip, ok = i.host.ExtraStreams.Lookup(key)
	default:
		return false
	}
	if !ok {
		http.NotFound(w, r)
		return true
	}
	if clip.ParentKey != 0 && i.host.Experience != nil {
		if _, err := i.host.Experience.Item(i.identityContext(r), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: clip.ParentKey}); err != nil {
			http.NotFound(w, r)
			return true
		}
	}
	jsonReply(w, i.extraItem(r, frontend.ItemDetail{Title: frontend.Title{Key: clip.ParentKey}}, clip))
	return true
}
