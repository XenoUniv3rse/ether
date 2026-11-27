package vela

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type grantQuota struct{ frontend.ViewerPolicy }

func (grantQuota) OfflineDownloadReceipt(context.Context, *frontend.Viewer, string) (frontend.OfflineDownloadBinding, error) {
	return frontend.OfflineDownloadBinding{}, nil
}

func (grantQuota) AdmitOfflineDownload(context.Context, *frontend.Viewer, frontend.RatingKey, string, frontend.OfflineDownloadBinding, int64) (frontend.DownloadLimitDecision, error) {
	return frontend.DownloadLimitDecision{Allowed: true}, nil
}

func (grantQuota) RemainingDailyDownloads(context.Context, *frontend.Viewer) (int, int, bool) {
	return 0, 0, false
}

type grantOpener struct {
	declared *frontend.OriginalPlaybackCapabilities
	media    frontend.MediaInfo
}

const grantSubtitle = "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nExample\n"

type grantSubtitleCatalog struct {
	frontend.SubtitleCatalog
	t *testing.T
}

func (f grantSubtitleCatalog) CachedDownloadSidecars(_ context.Context, title *frontend.Title, viewer *frontend.Viewer, source *frontend.OfflineDownloadSource) []frontend.StreamInfo {
	f.t.Helper()
	if title.Key != core.EpisodeKey(1, 1, 2) || viewer.ID != 1 || source.Release != "selected-release" {
		f.t.Fatal("subtitle lookup was not scoped to the download's title, viewer and source")
	}
	return []frontend.StreamInfo{
		{Index: 4, Type: "subtitle", Codec: "srt", Language: "fra", External: true, ExternalURL: "ether-subtitle:local"},
		{Index: 5, Type: "subtitle", Codec: "srt", External: true, ExternalURL: "https://invalid.example/unavailable"},
	}
}

func (f grantSubtitleCatalog) OpenFormatFrom(id, format, source string) (io.ReadCloser, string, error) {
	f.t.Helper()
	if id != "ether-subtitle:local" || format != "vtt" || source != "selected-release" {
		f.t.Fatal("request opened a subtitle outside the granted representation")
	}
	return io.NopCloser(strings.NewReader(grantSubtitle)), "vtt", nil
}

func (o *grantOpener) OpenOfflineDownload(_ context.Context, _ *frontend.Viewer, _ *frontend.Title, _ frontend.ClientCaps, _ string, declared *frontend.OriginalPlaybackCapabilities) (*frontend.OfflineDownloadSource, error) {
	o.declared = declared
	return &frontend.OfflineDownloadSource{
		ReaderAt: strings.NewReader("video"), Release: "selected-release", Name: "episode.mkv",
		Size: 5, ETag: `"selected-file"`, Media: o.media,
	}, nil
}

func TestDownloadGrantClientDeclaration(t *testing.T) {
	item := core.EpisodeKey(1, 1, 2)
	declaration := func(audio string) client {
		return client{
			Device:          device{ID: "download-device"},
			SubtitleFormats: []string{"ass"},
			OriginalPlayback: &frontend.OriginalPlaybackCapabilities{
				Version: 1, Range: true, DemuxContainers: []string{"mkv"},
				VideoDecoders: []string{"h264"}, AudioDecoders: []string{audio},
			},
		}
	}
	for _, tc := range []struct {
		name, requestAudio, negotiatedAudio, wantAudio string
	}{
		{name: "request declaration on cold cache", requestAudio: "eac3", wantAudio: "eac3"},
		{name: "cached declaration for existing client", negotiatedAudio: "ac3", wantAudio: "ac3"},
		{name: "request takes precedence", requestAudio: "eac3", negotiatedAudio: "ac3", wantAudio: "eac3"},
		{name: "no declaration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opener := &grantOpener{media: frontend.MediaInfo{Container: "mkv", Duration: time.Minute, Streams: []frontend.StreamInfo{
				{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
				{Index: 1, Type: "audio", Codec: "eac3", Channels: 6, Language: "eng", Default: true},
				{Index: 2, Type: "subtitle", Codec: "ass", Language: "eng", Forced: true},
				{Index: 3, Type: "subtitle", Codec: "pgs"},
			}}}
			now := time.Unix(1_700_000_000, 0)
			i := &instance{mux: http.NewServeMux(), host: frontend.Host{
				Identity: identityFake{}, ViewerPolicy: grantQuota{}, OfflineDownloads: opener,
				Capabilities: frontendtest.Capabilities{},
				Now:          func() time.Time { return now },
				Subtitles:    grantSubtitleCatalog{t: t},
				SkipMarkers: skipMarkersFake{key: item, ranges: []frontend.MarkerRange{
					{Type: "intro", StartMS: 1000, EndMS: 3000},
					{Type: "credits", StartMS: 58000, EndMS: 60000, Final: true},
				}},
				HostConfig: func() frontend.HostConfig {
					var cfg frontend.HostConfig
					cfg.Downloads.Enabled, cfg.Downloads.AllowFriends = true, true
					return cfg
				},
				Experience: frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
					return frontend.ItemDetail{Title: frontend.Title{Key: req.Key, Type: core.TypeEpisode}}, nil
				}},
			}}
			t.Cleanup(i.downloads.close)
			i.routes()
			body := object{"idempotencyKey": "episode-download"}
			if tc.requestAudio != "" {
				body["client"] = declaration(tc.requestAudio)
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			id := ids.RatingKeyGUID(item)
			r := httptest.NewRequest("POST", "/vela/v0/titles/"+id+"/download", bytes.NewReader(raw))
			r.Header.Set("Authorization", "Bearer access")
			r.Header.Set("X-Vela-Device-Id", "download-device")
			if tc.negotiatedAudio != "" {
				i.rememberNegotiatedClient(r, declaration(tc.negotiatedAudio))
			}
			w := httptest.NewRecorder()
			i.Handler().ServeHTTP(w, r)
			if tc.wantAudio == "" {
				var refusal struct {
					Error struct{ Code, Message string }
				}
				if err := json.Unmarshal(w.Body.Bytes(), &refusal); err != nil {
					t.Fatal(err)
				}
				if w.Code != http.StatusConflict || refusal.Error.Code != "capabilities_unknown" ||
					!strings.HasSuffix(refusal.Error.Message, ".") || opener.declared != nil {
					t.Fatalf("wanted capabilities_unknown before opening a source, got %d %s", w.Code, w.Body)
				}
				return
			}
			if w.Code != http.StatusOK || !opener.declared.Valid() || !reflect.DeepEqual(opener.declared.AudioDecoders, []string{tc.wantAudio}) {
				t.Fatalf("wanted grant with %s declaration, got %d %s", tc.wantAudio, w.Code, w.Body)
			}
			var grant downloadGrant
			if err := json.Unmarshal(w.Body.Bytes(), &grant); err != nil {
				t.Fatal(err)
			}
			// These are source indices and delivery facts from the opened file.
			wantAudio := []object{{"id": "1", "streamIndex": float64(1), "kind": "audio", "codec": "eac3", "language": "eng", "name": "", "channels": float64(6), "default": true, "forced": false}}
			wantSubs := []object{{"id": "2", "streamIndex": float64(2), "kind": "subtitle", "codec": "ass", "language": "eng", "name": "", "default": false, "forced": true, "delivery": map[string]any{"kind": "embedded"}}}
			subtitleURL := grant.URL + "/subtitles/4.vtt"
			wantSubs = append(wantSubs, object{
				"id": "4", "streamIndex": float64(4), "kind": "subtitle", "codec": "webvtt", "language": "fra",
				"name": "", "default": false, "forced": false, "url": subtitleURL,
				"delivery": map[string]any{"kind": "external", "format": "webvtt", "url": subtitleURL},
			})
			wantMarkers := []object{
				{"kind": "intro", "startSeconds": float64(1), "endSeconds": float64(3)},
				{"kind": "credits", "startSeconds": float64(58), "endSeconds": float64(60), "final": true},
			}
			if !reflect.DeepEqual(grant.AudioTracks, wantAudio) || !reflect.DeepEqual(grant.SubtitleTracks, wantSubs) ||
				grant.Container != "mkv" || grant.Chapters == nil || grant.OriginalVideo["codec"] != "h264" ||
				grant.SourceID != "\"selected-file\"" || !reflect.DeepEqual(grant.Markers, wantMarkers) {
				t.Fatalf("unexpected original file metadata: %s", w.Body)
			}
			request := func(method, url string, status int) *httptest.ResponseRecorder {
				t.Helper()
				w := httptest.NewRecorder()
				i.Handler().ServeHTTP(w, httptest.NewRequest(method, url, nil))
				if w.Code != status {
					t.Fatalf("%s %s returned %d, want %d", method, url, w.Code, status)
				}
				return w
			}
			if got := request("GET", subtitleURL, http.StatusOK); got.Body.String() != grantSubtitle ||
				got.Header().Get("Content-Type") != "text/vtt; charset=utf-8" {
				t.Fatalf("unexpected sidecar response: %s", got.Body)
			}
			if got := request("HEAD", subtitleURL, http.StatusOK); got.Body.Len() != 0 || got.Header().Get("Content-Length") == "" {
				t.Fatal("HEAD must return sidecar headers without a body")
			}
			request("GET", grant.URL+"/subtitles/5.vtt", http.StatusNotFound)
			request("GET", "/vela/v0/download/unknown/subtitles/4.vtt", http.StatusNotFound)
			now = grant.ExpiresAt
			request("GET", subtitleURL, http.StatusNotFound)
			request("GET", grant.URL, http.StatusNotFound)
		})
	}
}
