package vela

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

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
			opener := &grantOpener{media: frontend.MediaInfo{Container: "mkv", Streams: []frontend.StreamInfo{
				{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
				{Index: 1, Type: "audio", Codec: "eac3", Channels: 6, Language: "eng", Default: true},
				{Index: 2, Type: "subtitle", Codec: "ass", Language: "eng", Forced: true},
				{Index: 3, Type: "subtitle", Codec: "pgs"},
			}}}
			i := &instance{host: frontend.Host{
				Identity: identityFake{}, ViewerPolicy: grantQuota{}, OfflineDownloads: opener,
				Capabilities: frontendtest.Capabilities{},
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
			body := object{"idempotencyKey": "episode-download"}
			if tc.requestAudio != "" {
				body["client"] = declaration(tc.requestAudio)
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			id := ids.RatingKeyGUID(item)
			r := httptest.NewRequest("POST", "/vela/v0/titles/"+id+"/download", nil)
			r.SetPathValue("id", id)
			r.Header.Set("Authorization", "Bearer access")
			r.Header.Set("X-Vela-Device-Id", "download-device")
			r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, json.RawMessage(raw)))
			if tc.negotiatedAudio != "" {
				i.rememberNegotiatedClient(r, declaration(tc.negotiatedAudio))
			}
			w := httptest.NewRecorder()
			i.grantDownload(w, r)
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
			if !reflect.DeepEqual(grant.AudioTracks, wantAudio) || !reflect.DeepEqual(grant.SubtitleTracks, wantSubs) ||
				grant.Container != "mkv" || grant.Chapters == nil || grant.OriginalVideo["codec"] != "h264" {
				t.Fatalf("unexpected original file metadata: %s", w.Body)
			}
		})
	}
}
