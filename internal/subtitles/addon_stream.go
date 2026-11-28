package subtitles

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

const addonSubtitlePrefix = "addon-stream-"

var addonSubtitleClient = (&safefetch.Guard{}).Client(egress.Client(egress.CapStremio, 5*time.Second))

func appendAddonSubtitles(info *core.MediaInfo, tracks []core.AddonSubtitle) {
	next := 0
	for _, stream := range info.Streams {
		next = max(next, stream.Index+1)
	}
	for _, track := range tracks {
		if !config.IsSealed(track.Reference) {
			continue
		}
		id := addonSubtitlePrefix + base64.RawURLEncoding.EncodeToString([]byte(track.Reference))
		info.Streams = append(info.Streams, core.StreamInfo{
			Index: next, Type: "subtitle", Codec: "srt", Language: track.Language,
			Forced: track.Forced, External: true, ExternalURL: URLPrefix + id,
		})
		next++
	}
}

// Listing a source's sidecars does not fetch their files. Resolve the sealed
// link when the player requests the track or the encoder needs to stage it.
func (s *Service) cacheAddonSubtitle(id string) (string, error) {
	if s.addonSubtitleSealer == nil || len(id) > 8192 {
		return "", core.ErrNotFound
	}
	ref, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, addonSubtitlePrefix))
	if err != nil || !config.IsSealed(string(ref)) {
		return "", core.ErrNotFound
	}
	plain, _, err := s.addonSubtitleSealer.OpenField(string(ref), core.AddonSubtitleSealContext)
	if err != nil {
		return "", core.ErrNotFound
	}
	parts := strings.SplitN(plain, "\n", 3)
	if len(parts) != 3 || parts[0] == "" {
		return "", core.ErrNotFound
	}
	origin, lang, link := parts[0], parts[1], parts[2]
	digest := sha256.Sum256([]byte(plain))
	cacheID := addonSubtitlePrefix + hex.EncodeToString(digest[:])
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.fetchAndCache(ctx, cacheID, Candidate{Language: lang, DownloadURL: link, addonOrigin: origin}, 0)
}

func fetchAddonSubtitle(ctx context.Context, origin, link string) ([]byte, error) {
	u, err := url.Parse(link)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, core.ErrNotFound
	}
	release, err := acquireDownloadHost(ctx, strings.ToLower(u.Host))
	if err != nil {
		return nil, err
	}
	defer release()
	req, err := http.NewRequestWithContext(safefetch.WithTrustedOriginFor(ctx, origin, link), http.MethodGet, link, nil)
	if err != nil {
		return nil, core.ErrNotFound
	}
	resp, err := addonSubtitleClient.Do(req)
	if err != nil {
		return nil, core.ErrNotAvailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, core.ErrNotAvailable
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSubtitleBytes+1))
	if err != nil || len(data) > maxSubtitleBytes {
		return nil, core.ErrNotAvailable
	}
	return data, nil
}
