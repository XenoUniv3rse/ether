package playback

import (
	"context"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/rangefile"
)

// OpenOriginalDownload uses the ordinary source resolver and probe, but never
// registers a session or reserves stream capacity. The returned handle owns the
// generation until its caller finishes serving the download ticket.
func (m *Manager) OpenOriginalDownload(ctx context.Context, viewer *core.Viewer, title *core.Title, caps core.ClientCaps, release string, kind *core.KindInput, declaration *frontend.OriginalPlaybackCapabilities) (*frontend.OfflineDownloadSource, error) {
	if viewer == nil || title == nil || title.IsFutureRelease(m.now()) || !declaration.Valid() {
		return nil, frontend.ErrVersionUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, m.opts.PrepareTimeout)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	ctx = withDeferredGenerationFills(ctx)

	caps.NoPlaybackTelemetry, caps.DeferSourceCodecValidation = true, true
	if caps.DefaultRemoteBitrate {
		caps.MaxBitrateKbps, caps.DefaultRemoteBitrate = 0, false
	}
	caps.VideoCodecs = slices.Clone(declaration.VideoDecoders)
	caps.AudioCodecs = slices.Clone(declaration.AudioDecoders)
	caps.Containers = slices.Clone(declaration.DemuxContainers)
	id := identity{userID: viewer.ID, clientID: caps.ClientID, key: title.Key, surface: m.playbackSurface(string(caps.Protocol))}
	profile := m.selectProfile(caps, viewer, title.Key)
	s := newSession(randomID(16), id, viewer, caps, title, profileKey(profile), m.now())
	defer s.sources.seal()
	defer s.stopCancel()
	s.prefer, s.exact = release, release != "" && !core.IsAutoVersionSelector(release)
	s.offlineOriginal = func(info core.MediaInfo) bool {
		video := info.Video()
		bitrate := info.BitrateKbps
		if bitrate <= 0 && info.Duration > 0 && info.Size > 0 {
			bitrate = int(float64(info.Size) * 8 / info.Duration.Seconds() / 1000)
		}
		return declaration.Supports(info, -1) && video != nil &&
			(caps.MaxHeight <= 0 || video.Height > 0 && video.Height <= caps.MaxHeight) &&
			(caps.MaxBitrateKbps <= 0 || bitrate > 0 && bitrate <= caps.MaxBitrateKbps)
	}

	var g *generation
	var err error
	if kind != nil {
		s.kindOpen, s.kindOpenCandidate, s.kindSourceID = kind.Open, kind.OpenCandidate, kind.SourceID
		fallback := func(size int64) core.MediaInfo { return core.MediaInfo{Size: size, Estimated: true} }
		var excluded []string
		for attempt := 0; attempt < core.MaxVariants && ctx.Err() == nil; attempt++ {
			var res *core.Resolution
			g, res, err = m.openKindCandidate(ctx, s, excluded, fallback)
			if err == nil || s.exact || res == nil {
				break
			}
			excluded = append(excluded, res.Release.ID)
		}
	} else {
		g, err = m.openInitial(ctx, s, profile)
	}
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, frontend.ErrVersionUnavailable
	}
	// The probe may have used the loopback token. The HTTP download owns a
	// direct reader and never needs the playback route.
	m.mu.Lock()
	delete(m.tokens, s.id+"/"+g.token)
	m.mu.Unlock()
	g.markIndexReady()
	size := g.src.Size()
	revision := core.SourceRevision(&core.PlaySession{Key: title.Key, Source: g.src})
	if ctx.Err() != nil {
		m.dropGeneration(g)
		return nil, ctx.Err()
	}
	if size <= 0 || revision == "" || (kind != nil && s.exact && !g.res.MatchesVersion(release)) {
		m.dropGeneration(g)
		return nil, frontend.ErrVersionUnavailable
	}
	name := g.src.Name()
	info := g.infoSnapshot()
	// The grant needs probe facts. Keep seek indexes on the generation.
	info.Keyframes, info.CopyTimeline = nil, nil
	// Warm playback probes may carry another session's subtitle selections.
	info.Streams = slices.DeleteFunc(info.Streams, func(st core.StreamInfo) bool { return st.External })
	releaseName := ""
	if g.res != nil {
		releaseName = g.res.Release.Name
	}
	media := &frontend.OfflineDownloadSource{
		ReaderAt: g.src, Closer: offlineGeneration{m, g}, ReleaseName: releaseName,
		Release: g.releaseID(), Name: name, Size: size, Media: info,
		MIME: contentTypeForInfo(info, name, sourceMIME(g.src)),
		ETag: rangefile.StrongETag(g.releaseID()+"\x00"+revision, size, time.Time{}),
	}
	return media, nil
}

type offlineGeneration struct {
	manager    *Manager
	generation *generation
}

func (o offlineGeneration) Close() error {
	o.manager.dropGeneration(o.generation)
	return nil
}
