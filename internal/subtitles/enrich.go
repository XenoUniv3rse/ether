package subtitles

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// Enrich attaches the release's declared sidecars and cached provider tracks
// during playback preparation. The preparing generation owns the source
// during local hashing, so it cannot be disposed during failover.
func (s *Service) Enrich(ctx context.Context, title *core.Title, user *core.User, res *core.Resolution, src core.MediaSource, info *core.MediaInfo, prefs ...Preferences) {
	if s == nil || info == nil || res == nil {
		return
	}
	if s.addonSubtitleSealer != nil && len(res.Release.AddonSubtitles) > 0 {
		appendAddonSubtitles(info, res.Release.AddonSubtitles)
	}
	if !s.Enabled() {
		return
	}
	release := res.Release.Name
	hash := ""
	if src != nil && strings.HasPrefix(res.Release.ID, "local:") {
		var err error
		hash, err = MovieHash(src, src.Size())
		if err != nil {
			s.log.Debug("local subtitle hash unavailable", "error", err)
		} else if title != nil {
			s.RememberVideoHash(title.Key, release, hash, src.Size())
			if revision := core.SourceRevision(&core.PlaySession{Key: title.Key, Source: src}); revision != "" {
				s.RememberVideoHash(title.Key, revision, hash, src.Size())
			}
		}
	}
	sidecars, err := s.SidecarsWithHash(ctx, title, user, info.Streams, release, hash, prefs...)
	if err != nil {
		s.log.Debug("subtitle enrichment unavailable", "error", err)
	}
	info.Streams = append(info.Streams, sidecars...)
}
