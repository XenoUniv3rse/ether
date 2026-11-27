package app

import (
	"context"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reqmemo"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/langfeat"
	"github.com/braedonsaunders/ether/internal/markers"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/subtitles"
	"github.com/braedonsaunders/ether/internal/trackprefs"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// subtitleUser is u with the subtitle languages of the viewer's stream
// preferences (Viewers > Defaults or their override), which win over the
// account's own list.
func (a *App) subtitleUser(ctx context.Context, u *core.User) *core.User {
	if u == nil {
		return nil
	}
	copy := *u
	if a.trackprefs != nil {
		rules, err := a.trackprefs.GetRules(ctx, u.ID)
		if err == nil && len(rules.SubtitleLanguages) > 0 {
			copy.SubtitleLanguages = rules.SubtitleLanguages
		}
	}
	return &copy
}

// subtitlePrefs is the viewer's subtitle mode/style, used when listing
// sidecars already on disk. It does not cause a download.
func (a *App) subtitlePrefs(ctx context.Context, u *core.User) subtitles.Preferences {
	if u == nil || a.trackprefs == nil {
		return subtitles.Preferences{}
	}
	rules, err := a.trackprefs.GetRules(ctx, u.ID)
	if err != nil {
		return subtitles.Preferences{}
	}
	return subtitles.Preferences{Mode: rules.SubtitleMode, Style: rules.SubtitleStyle}
}

func (a *App) enrichSubtitles(ctx context.Context, t *core.Title, u *core.User, res *core.Resolution, src core.MediaSource, info *core.MediaInfo) {
	a.subtitles.Enrich(ctx, t, a.subtitleUser(ctx, u), res, src, info, a.subtitlePrefs(ctx, u))
}

var (
	_ frontend.SubtitleSidecarSource  = subtitleSurface{}
	_ frontend.SubtitleSidecarCatalog = subtitleSurface{}
	_ frontend.SubtitleSidecarSearch  = subtitleSurface{}
	_ frontend.SubtitleSidecarUpload  = subtitleSurface{}
)

func (s subtitleSurface) OpenFormat(id, format string) (io.ReadCloser, string, error) {
	return s.app.subtitles.OpenFormat(id, format)
}

func (s subtitleSurface) OpenFormatFrom(id, format, source string) (io.ReadCloser, string, error) {
	return s.app.subtitles.OpenFormatFrom(id, format, source)
}

func (s subtitleSurface) CachedSidecars(ctx context.Context, t *core.Title, u *core.User, embedded []core.StreamInfo) []core.StreamInfo {
	if t == nil {
		return nil
	}
	has, _ := reqmemo.Do(ctx, "subtitles:has:"+strconv.FormatInt(int64(t.Key), 10), func() (bool, error) {
		return s.app.subtitles.HasTitleSelections(t.Key), nil
	})
	if !has {
		return nil
	}
	out := s.app.subtitles.CachedSidecars(t, s.app.subtitleUser(ctx, u), embedded, s.app.subtitlePrefs(ctx, u))
	if !s.app.subtitles.Enabled() {
		out = append(out, s.app.subtitles.ManualSidecars(t, embedded)...)
	}
	return out
}

// subtitleSearchLanguages preserves an explicit client choice; otherwise resolve
// the same scoped viewer rules used for playback before profile/server defaults.
func (s subtitleSurface) subtitleSearchLanguages(ctx context.Context, t *core.Title, u *core.User, lang, device string) ([]string, error) {
	if lang != "" {
		return []string{lang}, nil
	}
	if t == nil {
		return nil, nil
	}
	scope := trackprefs.Scope{Device: device}
	if t.ShowKey != 0 {
		scope.Series = strconv.FormatInt(int64(t.ShowKey), 10)
	} else if t.Key.Kind() == core.KindEpisode {
		scope.Series = strconv.FormatInt(int64(core.ShowKey(t.Key.TMDB())), 10)
	}
	if section, ok := brokerSectionFor(func(core.MediaType) (int, bool) { return 0, false })(t.Type); ok {
		scope.Library = strconv.Itoa(section)
	}
	userID := int64(0)
	if u != nil {
		userID = u.ID
	}
	if s.app.trackprefs != nil {
		return s.app.trackprefs.SubtitleSearchLanguages(ctx, userID, t.Key, scope)
	}
	if u != nil && len(u.SubtitleLanguages) > 0 {
		return u.SubtitleLanguages, nil
	}
	if s.app.cfg != nil {
		return []string{s.app.cfg.Get().Catalog.Language}, nil
	}
	return nil, nil
}

func (s subtitleSurface) SearchSubtitles(ctx context.Context, t *core.Title, u *core.User, lang, release string) ([]frontend.SubtitleSearchOffer, error) {
	device := ""
	if pr := plex.FromContext(ctx); pr != nil {
		device = pr.ClientID
	}
	languages, err := s.subtitleSearchLanguages(ctx, t, u, lang, device)
	if err != nil {
		return nil, err
	}
	asked := lang
	if asked == "" && len(languages) > 0 {
		asked = languages[0]
	}
	var hash string
	var size int64
	if len(languages) > 0 {
		release, hash, size = s.subtitleSearchSource(ctx, t, u, device, release)
	}
	cands, searchErr := s.app.subtitles.SearchOnDemandLanguagesWithHash(ctx, t, languages, release, hash, size)
	if searchErr != nil {
		cands = nil
	}
	out := make([]frontend.SubtitleSearchOffer, 0, len(cands))
	for _, c := range cands {
		if c.Language != "" {
			lang = c.Language
		}
		title := c.Release
		if title == "" {
			title = strings.TrimSuffix(c.FileName, ".srt")
		}
		provider := c.Provider
		if provider == "" {
			provider = "OpenSubtitles"
		}
		out = append(out, frontend.SubtitleSearchOffer{
			ID: c.FileID, Language: lang, Title: title, Provider: provider,
			Score: int(math.Round(c.Score)), HearingImpaired: c.HearingImp, Forced: c.Forced,
			PerfectMatch: c.HashMatch || (release != "" && strings.EqualFold(c.Release, release)),
		})
	}
	if s.app.lang != nil {
		for _, o := range s.app.lang.Offers(t, asked) {
			out = append(out, frontend.SubtitleSearchOffer{
				ID: o.ID, Language: o.Language, Title: o.Title, Provider: o.Provider,
			})
		}
	}
	if searchErr != nil && len(out) == 0 {
		return nil, searchErr
	}
	return out, nil
}

func (s subtitleSurface) DownloadSubtitle(ctx context.Context, t *core.Title, user *core.User, id int64) (core.StreamInfo, error) {
	if s.app.lang != nil && langfeat.IsReserved(id) {
		st, err := s.app.lang.Fulfill(ctx, user, t, id)
		if err == nil || !errors.Is(err, core.ErrNotFound) {
			return st, err
		}
	}
	st, err := s.app.subtitles.DownloadOnDemand(ctx, t, id)
	if err != nil {
		return st, err
	}
	clientID := ""
	if pr := plex.FromContext(ctx); pr != nil {
		clientID = pr.ClientID
	}
	s.app.startSubtitleCorrection(ctx, t, st.ExternalURL, s.app.playSession(user, t, clientID))
	return st, nil
}

func (a *App) playSession(user *core.User, title *core.Title, clientID string) *core.PlaySession {
	if user == nil || title == nil {
		return nil
	}
	return a.playSessionOn("", user.ID, clientID, title.Key)
}

type playSessionFinder interface {
	SessionFor(int64, string, core.RatingKey) (*core.PlaySession, bool)
	SessionForSurfaceClient(string, int64, string, core.RatingKey) (*core.PlaySession, bool)
}

func (a *App) playSessionOn(surface string, userID int64, clientID string, key core.RatingKey) *core.PlaySession {
	if a == nil || a.playback == nil {
		return nil
	}
	return lookupPlaySession(a.playback, surface, userID, clientID, key)
}

// lookupPlaySession finds the live session for this pick. Plex identities
// have an empty surface; Jellyfin/Emby register with surface=protocol and
// a variant, so SessionFor (no surface, no variant) always misses them.
func lookupPlaySession(p playSessionFinder, surface string, userID int64, clientID string, key core.RatingKey) *core.PlaySession {
	if p == nil {
		return nil
	}
	if clientID == "" {
		if lister, ok := p.(interface{ Sessions() []*core.PlaySession }); ok {
			for _, ps := range lister.Sessions() {
				if ps != nil && !ps.PreparedOnly && ps.UserID == userID && ps.Key == key {
					return ps
				}
			}
		}
		return nil
	}
	if sessionSurface(surface) != "" {
		if sess, ok := p.SessionForSurfaceClient(surface, userID, clientID, key); ok && sess != nil && !sess.PreparedOnly {
			return sess
		}
		return nil
	}
	if sess, ok := p.SessionFor(userID, clientID, key); ok && sess != nil && !sess.PreparedOnly {
		return sess
	}
	return nil
}

func (a *App) startSubtitleCorrection(ctx context.Context, t *core.Title, id string, sess *core.PlaySession) {
	if a.subtitles == nil || sess == nil || sess.PreparedOnly {
		return
	}
	if sctx := a.playback.SessionContext(sess.ID); sctx != nil {
		ctx = sctx
	}
	a.subtitles.StartCorrection(ctx, t, id, a.liveReleaseAccess(sess))
}

func (a *App) onPlaybackMarkerEvent(ev playback.Event) {
	if a == nil || a.markers == nil || ev.Session == nil || ev.Session.PreparedOnly {
		return
	}
	switch ev.Kind {
	case "start", "surface", "failover", "downfailover":
	default:
		return
	}
	if ev.Session.Info.Estimated || ev.Session.Info.Duration <= 0 {
		return
	}
	a.markers.Warm(ev.Session.Key, ev.Session.Info)
}

func (a *App) onPlaybackSubtitleEvent(ev playback.Event) {
	if a == nil || a.subtitles == nil || a.playback == nil || ev.Session == nil || ev.Session.PreparedOnly {
		return
	}
	switch ev.Kind {
	case "start", "surface", "failover", "downfailover":
	default:
		return
	}
	live, ok := a.playback.Session(ev.Session.ID)
	if !ok || live == nil || live.PreparedOnly {
		return
	}
	var sidecars []string
	for _, st := range live.Info.Streams {
		if st.Type == "subtitle" && st.External && strings.HasPrefix(st.ExternalURL, subtitles.URLPrefix) {
			sidecars = append(sidecars, st.ExternalURL)
		}
	}
	if len(sidecars) == 0 {
		return
	}
	src := a.playback.SourceKey(live.ID)
	if src == "" && live.Resolution != nil {
		src = live.Resolution.Release.ID
	}
	if src == "" {
		return
	}
	var pending []string
	for _, extURL := range sidecars {
		if !a.subtitles.HasCorrection(extURL, src) {
			pending = append(pending, extURL)
		}
	}
	if len(pending) == 0 {
		return
	}
	var title *core.Title
	if a.catalog != nil && live.Key != 0 {
		title, _ = a.catalog.CachedTitle(context.Background(), live.Key)
	}
	if title == nil && live.Key != 0 {
		title = &core.Title{Key: live.Key}
	}
	sctx := a.playback.SessionContext(live.ID)
	if sctx == nil {
		sctx = context.Background()
	}
	for _, extURL := range pending {
		a.subtitles.StartCorrection(sctx, title, extURL, a.liveReleaseAccess(live))
	}
}

func (a *App) cachedEmbeddedCues(src string, info *core.MediaInfo) func() [][]subtitles.Cue {
	if a == nil || a.transcoder == nil || src == "" || info == nil {
		return nil
	}
	streams := append([]core.StreamInfo(nil), info.Streams...)
	return func() [][]subtitles.Cue {
		ctx := transcode.WithSubtitleSource(context.Background(), src)
		var out [][]subtitles.Cue
		for _, st := range streams {
			if st.Type != "subtitle" || st.External || transcode.IsImageSubtitle(st.Codec) {
				continue
			}
			raw, ok := a.transcoder.CachedSubtitle(ctx, st.Index, st.Codec)
			if !ok || len(raw) == 0 {
				continue
			}
			cues, err := subtitles.ParseCues(string(raw))
			if err == nil && len(cues) >= 3 {
				out = append(out, cues)
			}
		}
		return out
	}
}

func (a *App) liveReleaseAccess(sess *core.PlaySession) subtitles.ReleaseAccess {
	if sess == nil || sess.PreparedOnly || a.playback == nil {
		return subtitles.ReleaseAccess{}
	}
	src := a.playback.SourceKey(sess.ID)
	release := ""
	if sess.Resolution != nil {
		release = sess.Resolution.Release.Name
		if src == "" {
			src = sess.Resolution.Release.ID
		}
	}
	if src == "" {
		return subtitles.ReleaseAccess{}
	}
	id := sess.ID
	info := core.CloneMediaInfo(sess.Info)
	access := subtitles.ReleaseAccess{
		URL:          a.playback.InputURL(id),
		Duration:     info.Duration,
		Info:         &info,
		Release:      release,
		Source:       src,
		EmbeddedCues: a.cachedEmbeddedCues(src, &info),
		Hold: func() (io.ReaderAt, int64, func(), bool) {
			return a.playback.SourceReader(id)
		},
	}
	if a.transcoder != nil {
		access.FFmpeg = a.transcoder.FFmpegPath()
	}
	return access
}

func (s subtitleSurface) UploadSubtitle(_ context.Context, t *core.Title, user *core.User, filename, format, language string, body []byte) (core.StreamInfo, error) {
	if user == nil || !user.IsOwner {
		return core.StreamInfo{}, core.ErrUnauthorized
	}
	return s.app.subtitles.StoreUpload(t, filename, format, language, body)
}

// markerDialogue is markers.Options.Dialogue: cue times from subtitle text
// the system already holds for the played release (cached sidecars), for
// stinger detection. It never fetches video or touches the network.
func (a *App) markerDialogue(_ context.Context, _ core.RatingKey, info core.MediaInfo) ([]markers.DialogueCue, bool) {
	cues, ok := a.subtitles.CuesForInfo(info)
	if !ok {
		return nil, false
	}
	out := make([]markers.DialogueCue, len(cues))
	for i, c := range cues {
		out[i] = markers.DialogueCue{Start: c.Start, End: c.End}
	}
	return out, true
}

// sessionSurface wires the main adapter to its historical session namespace.
func sessionSurface(surface string) string {
	if frontendHostPolicy(core.Protocol(surface)).LegacySessionIdentity {
		return ""
	} // Main adapter namespace wiring.
	return surface
}
