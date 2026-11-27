package vela

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/mediasafety"
	"github.com/braedonsaunders/ether/internal/frontend/kit/rangefile"
)

func (i *instance) downloadSidecars(ctx context.Context, viewer *frontend.Viewer, title *frontend.Title, source *frontend.OfflineDownloadSource, baseURL string) ([]object, map[string]string) {
	catalog, ok := i.host.Subtitles.(frontend.OfflineDownloadSidecars)
	if !ok {
		return nil, nil
	}
	var tracks []object
	authorized := make(map[string]string)
	for _, st := range catalog.CachedDownloadSidecars(ctx, title, viewer, source) {
		if st.Type != "subtitle" || !st.External || !strings.HasPrefix(st.ExternalURL, "ether-subtitle:") || !frontend.IsTextSubtitle(st.Codec) {
			continue
		}
		track := sourceTrack(st)
		name := track["id"].(string) + ".vtt"
		url := baseURL + "/subtitles/" + name
		track["codec"], track["url"] = "webvtt", url
		track["delivery"] = object{"kind": "external", "format": "webvtt", "url": url}
		tracks = append(tracks, track)
		authorized[name] = st.ExternalURL
	}
	return tracks, authorized
}

func (i *instance) downloadSubtitle(w http.ResponseWriter, r *http.Request) {
	ticket := i.downloads.acquire(r.PathValue("ticket"), i.serverNow())
	if ticket == nil {
		http.NotFound(w, r)
		return
	}
	defer i.downloads.release(ticket)
	name := r.PathValue("track")
	sidecar := ticket.sidecars[name]
	if sidecar == "" || i.host.Subtitles == nil {
		http.NotFound(w, r)
		return
	}
	reader, format, err := i.host.Subtitles.OpenFormatFrom(sidecar, "vtt", ticket.source.Release)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer reader.Close()
	const maxSubtitle = 32 << 20
	raw, err := io.ReadAll(io.LimitReader(reader, maxSubtitle+1))
	if err != nil || format != "vtt" || len(raw) > maxSubtitle {
		http.NotFound(w, r)
		return
	}
	mediasafety.SetHeaders(w)
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	rangefile.Serve(w, r, name, "", int64(len(raw)), time.Time{}, bytes.NewReader(raw))
}
