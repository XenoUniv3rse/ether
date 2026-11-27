package livetv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func init() {
	frontend.RegisterLiveTVHelpers(
		func(stage string, err error) frontend.LiveTVTuneDiagnosis {
			d := Diagnose(stage, err)
			return frontend.LiveTVTuneDiagnosis{
				Code:           d.Code,
				Stage:          d.Stage,
				UpstreamStatus: d.UpstreamStatus,
				RetryAfter:     d.RetryAfter,
				Message:        d.Error(),
				LogAttrs:       d.LogAttrs(),
			}
		},
		func(err error) (string, error, bool) {
			var dashErr *VirtualDASHError
			if !errors.As(err, &dashErr) {
				return "", nil, false
			}
			return dashErr.Message, dashErr.Err, true
		},
		func(c frontend.LiveTVChannel) (string, []byte, bool) {
			return ShowcaseLogo(FromFrontendChannel(c))
		},
	)
}

// Frontend returns the frontend.LiveTV adapter for this service, or nil when s is nil.
func (s *Service) Frontend() frontend.LiveTV {
	if s == nil {
		return nil
	}
	return liveTVHost{s: s}
}

// ToFrontendChannel converts a Channel to the frontend boundary value type.
func ToFrontendChannel(c Channel) frontend.LiveTVChannel {
	return frontend.LiveTVChannel{
		ID:            c.ID,
		Number:        c.Number,
		Name:          c.Name,
		Kind:          c.Kind,
		Enabled:       c.Enabled,
		Logo:          c.Logo,
		LogoMatched:   c.LogoMatched,
		Rating:        c.Rating,
		SourceID:      c.SourceID,
		GuideID:       c.GuideID,
		GuideSourceID: c.GuideSourceID,
		Group:         c.Group,
		Personal:      c.Personal != nil,
	}
}

// FromFrontendChannel converts a frontend boundary channel to a Channel.
func FromFrontendChannel(c frontend.LiveTVChannel) Channel {
	ch := Channel{
		ID:            c.ID,
		Number:        c.Number,
		Name:          c.Name,
		Kind:          c.Kind,
		Enabled:       c.Enabled,
		Logo:          c.Logo,
		LogoMatched:   c.LogoMatched,
		Rating:        c.Rating,
		SourceID:      c.SourceID,
		GuideID:       c.GuideID,
		GuideSourceID: c.GuideSourceID,
		Group:         c.Group,
	}
	if c.Personal {
		ch.Personal = &PersonalRecipe{}
	}
	return ch
}

func toFrontendChannels(cs []Channel) []frontend.LiveTVChannel {
	out := make([]frontend.LiveTVChannel, len(cs))
	for i := range cs {
		out[i] = ToFrontendChannel(cs[i])
	}
	return out
}

func toFrontendItem(item Item) frontend.LiveTVItem {
	return frontend.LiveTVItem{
		Key:          item.Key,
		ShowKey:      item.ShowKey,
		Title:        item.Title,
		EpisodeTitle: item.EpisodeTitle,
		ShowTitle:    item.ShowTitle,
		Summary:      item.Summary,
		Rating:       item.Rating,
		Thumb:        item.Thumb,
		Art:          item.Art,
		Genres:       item.Genres,
		GUID:         item.GUID,
		ShowGUID:     item.ShowGUID,
		MediaType:    item.MediaType,
		Premiere:     item.Premiere,
		Live:         item.Live,
		Season:       item.Season,
		Episode:      item.Episode,
		ReleasedMS:   item.ReleasedMS,
	}
}

func toFrontendProgramme(p Programme) frontend.LiveTVProgramme {
	return frontend.LiveTVProgramme{
		ID:        p.ID,
		ChannelID: p.ChannelID,
		Start:     p.Start,
		End:       p.End,
		Lens:      p.Lens,
		Item:      toFrontendItem(p.Item),
	}
}

func toFrontendProgrammes(ps []Programme) []frontend.LiveTVProgramme {
	out := make([]frontend.LiveTVProgramme, len(ps))
	for i := range ps {
		out[i] = toFrontendProgramme(ps[i])
	}
	return out
}

func toFrontendSessionInfo(info SessionInfo) frontend.LiveTVSessionInfo {
	out := frontend.LiveTVSessionInfo{
		ActiveSourceID:   info.ActiveSourceID,
		Provider:         info.Provider,
		PreviousProvider: info.PreviousProvider,
		ID:               info.ID,
		SubscriptionID:   info.SubscriptionID,
		ActivityID:       info.ActivityID,
		MediaInfo:        info.MediaInfo,
		Channel:          ToFrontendChannel(info.Channel),
		StartedAt:        info.StartedAt,
	}
	if info.Programme != nil {
		prog := toFrontendProgramme(*info.Programme)
		out.Programme = &prog
	}
	return out
}

type liveTVHost struct {
	s *Service
}

func (h liveTVHost) Enabled() bool                  { return h.s.Enabled() }
func (h liveTVHost) LiveGuideWindowAvailable() bool { return h.s != nil }

func (h liveTVHost) Channels(ctx context.Context, v *core.Viewer) ([]frontend.LiveChannel, error) {
	cs, err := h.s.AllowedChannels(ctx, v)
	if err != nil {
		return nil, err
	}
	_, idx := h.s.getOwnerGroupCacheState()
	if idx == nil {
		idx, err = h.s.cachedOwnerGroupIndex(ctx)
		if err != nil {
			return nil, err
		}
	}
	out := make([]frontend.LiveChannel, len(cs))
	for i, c := range cs {
		out[i] = frontend.LiveChannel{
			ArchiveDays: c.ArchiveDays(),
			ID:          c.ID,
			Number:      c.Number,
			Name:        c.Name,
			Kind:        c.Kind,
			Logo:        c.Logo,
			Rating:      c.Rating,
			Group:       c.Group,
			Favorite:    c.Favorite,
			Groups:      idx.memberships(c.SourceID, c.Group, c.ID),
		}
	}
	return out, nil
}

func (h liveTVHost) Guide(ctx context.Context, v *core.Viewer, q frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
	var ps []Programme
	var err error
	if len(q.Channels) > 0 {
		ps, err = h.s.GuideForChannels(ctx, v, q.From, q.Until, q.Channels)
	} else {
		ps, err = h.s.Guide(ctx, v, q.From, q.Until)
	}
	if err != nil {
		return nil, err
	}
	out := make([]frontend.LiveProgramme, 0, len(ps))
	var counts map[string]int
	if q.PerChannelLimit {
		counts = make(map[string]int, len(q.Channels))
	}
	for _, p := range ps {
		if q.PerChannelLimit {
			if counts[p.ChannelID] >= q.Limit {
				continue
			}
			counts[p.ChannelID]++
		}
		out = append(out, frontend.LiveProgramme{ID: p.ID, ChannelID: p.ChannelID, Start: p.Start, End: p.End, Genres: p.Item.Genres, MediaType: p.Item.MediaType, Item: p.Item.Key, Title: p.Item.Title, Summary: p.Item.Summary, Rating: p.Item.Rating, ShowTitle: p.Item.ShowTitle, EpisodeTitle: p.Item.EpisodeTitle, Season: p.Item.Season, Episode: p.Item.Episode})
		if !q.PerChannelLimit && q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

func (h liveTVHost) GuideWindowBatch(ctx context.Context, v *core.Viewer, q frontend.LiveGuideWindowQuery) (frontend.LiveGuideWindow, error) {
	if h.s == nil {
		return frontend.LiveGuideWindow{}, core.ErrNotAvailable
	}
	w, err := h.s.GuideWindowForChannels(ctx, v, q.From, q.Until, q.Channels)
	if err != nil {
		return frontend.LiveGuideWindow{}, err
	}
	fingerprint := sha256.New()
	enc := json.NewEncoder(fingerprint)
	if err := enc.Encode([]any{q.From.UTC(), q.Until.UTC()}); err != nil {
		return frontend.LiveGuideWindow{}, err
	}
	rows := make([]frontend.LiveGuideWindowChannel, len(w.Channels))
	for i, ch := range w.Channels {
		if err := enc.Encode([]any{ch.ChannelID, ch.State}); err != nil {
			return frontend.LiveGuideWindow{}, err
		}
		ps := make([]frontend.LiveProgramme, len(ch.Programmes))
		for j, p := range ch.Programmes {
			if err := enc.Encode([]any{p.ID, p.ChannelID, p.Start.UTC(), p.End.UTC(), p.Item.Key, p.Item.Title, p.Item.Summary, p.Item.Rating, p.Item.ShowTitle, p.Item.EpisodeTitle, p.Item.Season, p.Item.Episode, p.Item.Thumb, p.Item.Art, p.Item.Genres}); err != nil {
				return frontend.LiveGuideWindow{}, err
			}
			ps[j] = frontend.LiveProgramme{
				ArchiveDays:  ch.ArchiveDays,
				ID:           p.ID,
				ChannelID:    p.ChannelID,
				Start:        p.Start,
				End:          p.End,
				Genres:       p.Item.Genres,
				MediaType:    p.Item.MediaType,
				Item:         p.Item.Key,
				Title:        p.Item.Title,
				Summary:      p.Item.Summary,
				Rating:       p.Item.Rating,
				ShowTitle:    p.Item.ShowTitle,
				EpisodeTitle: p.Item.EpisodeTitle,
				Season:       p.Item.Season,
				Episode:      p.Item.Episode,
			}
		}
		rows[i] = frontend.LiveGuideWindowChannel{
			ArchiveDays: ch.ArchiveDays,
			ChannelID:   ch.ChannelID,
			State:       ch.State,
			Programmes:  ps,
		}
	}
	return frontend.LiveGuideWindow{
		From:       w.From,
		Until:      w.Until,
		Revision:   hex.EncodeToString(fingerprint.Sum(nil)),
		ValidUntil: w.ValidUntil,
		Channels:   rows,
	}, nil
}

func (h liveTVHost) ResolveChannelGUID(context.Context, *core.Viewer, string) (frontend.LiveChannel, error) {
	return frontend.LiveChannel{}, core.ErrNotFound
}

func (h liveTVHost) ResolveProgrammeGUID(context.Context, *core.Viewer, string, frontend.LiveGuideQuery) (frontend.LiveProgramme, error) {
	return frontend.LiveProgramme{}, core.ErrNotFound
}

func (h liveTVHost) VisibleTo(ctx context.Context, u *frontend.User) bool {
	return h.s.VisibleTo(ctx, u)
}

func (h liveTVHost) ServeXMLTV(w http.ResponseWriter, r *http.Request, u *frontend.User) {
	h.s.ServeXMLTV(w, r, u)
}

func (h liveTVHost) Stop(ctx context.Context, id string, u *frontend.User) error {
	return h.s.Stop(ctx, id, u)
}

func (h liveTVHost) ServeDASHPart(w http.ResponseWriter, r *http.Request, id string, u *frontend.User, rep, n int, init bool) {
	h.s.ServeDASHPart(w, r, id, u, rep, n, init)
}

func (h liveTVHost) ServePlaylist(w http.ResponseWriter, r *http.Request, id string, u *frontend.User) {
	h.s.ServePlaylist(w, r, id, u)
}

func (h liveTVHost) ServeRelaySegment(w http.ResponseWriter, r *http.Request, id string, n int, u *frontend.User) {
	h.s.ServeRelaySegment(w, r, id, n, u)
}

func (h liveTVHost) ServeResource(w http.ResponseWriter, r *http.Request, id, resource string, u *frontend.User) {
	h.s.ServeResource(w, r, id, resource, u)
}

func (h liveTVHost) AllowedChannels(ctx context.Context, u *frontend.User) ([]frontend.LiveTVChannel, error) {
	cs, err := h.s.AllowedChannels(ctx, u)
	if err != nil {
		return nil, err
	}
	return toFrontendChannels(cs), nil
}

func (h liveTVHost) AllowedChannel(ctx context.Context, u *frontend.User, id string) (frontend.LiveTVChannel, error) {
	c, err := h.s.AllowedChannel(ctx, u, id)
	if err != nil {
		return frontend.LiveTVChannel{}, err
	}
	return ToFrontendChannel(c), nil
}

func (h liveTVHost) Channel(ctx context.Context, id string) (frontend.LiveTVChannel, error) {
	if !h.s.Enabled() {
		return frontend.LiveTVChannel{}, core.ErrNotAvailable
	}
	c, err := h.s.Channel(ctx, id)
	if err != nil {
		return frontend.LiveTVChannel{}, err
	}
	return ToFrontendChannel(c), nil
}

func (h liveTVHost) ChannelForViewer(ctx context.Context, u *frontend.User, id string) (frontend.LiveTVChannel, error) {
	c, err := h.s.ChannelForViewer(ctx, u, id)
	if err != nil {
		return frontend.LiveTVChannel{}, err
	}
	return ToFrontendChannel(c), nil
}

func (h liveTVHost) AllChannels(ctx context.Context) ([]frontend.LiveTVChannel, error) {
	if !h.s.Enabled() {
		return nil, nil
	}
	cs, err := h.s.Channels(ctx)
	if err != nil {
		return nil, err
	}
	return toFrontendChannels(cs), nil
}

func (h liveTVHost) PermittedChannels(ctx context.Context, u *frontend.User, ids []string) (map[string]frontend.LiveTVChannel, error) {
	channels, err := h.s.PermittedChannels(ctx, u, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]frontend.LiveTVChannel, len(channels))
	for id, channel := range channels {
		out[id] = ToFrontendChannel(channel)
	}
	return out, nil
}

func (h liveTVHost) ChannelPermitted(ctx context.Context, u *frontend.User, id string) (bool, error) {
	return h.s.ChannelPermitted(ctx, u, id)
}

func (h liveTVHost) LineupChangedFor(u *frontend.User) time.Time {
	return h.s.LineupChangedFor(u)
}

func (h liveTVHost) GuideForChannels(ctx context.Context, u *frontend.User, from, until time.Time, channelIDs []string) ([]frontend.LiveTVProgramme, error) {
	ps, err := h.s.GuideForChannels(ctx, u, from, until, channelIDs)
	if err != nil {
		return nil, err
	}
	return toFrontendProgrammes(ps), nil
}

func (h liveTVHost) GuideWindow(ctx context.Context, u *frontend.User, from, until time.Time) ([]frontend.LiveTVProgramme, error) {
	ps, err := h.s.Guide(ctx, u, from, until)
	if err != nil {
		return nil, err
	}
	return toFrontendProgrammes(ps), nil
}

func (h liveTVHost) GuideChannelLogos(ctx context.Context, channels []frontend.LiveTVChannel) map[string]string {
	cs := make([]Channel, len(channels))
	for i, c := range channels {
		cs[i] = FromFrontendChannel(c)
	}
	return h.s.GuideChannelLogos(ctx, cs)
}

func (h liveTVHost) GuideChannelLogo(ctx context.Context, c frontend.LiveTVChannel) string {
	return h.s.GuideChannelLogo(ctx, FromFrontendChannel(c))
}

func (h liveTVHost) TuneWithSession(ctx context.Context, u *frontend.User, id string, caps frontend.ClientCaps, sessionID string) (frontend.LiveTVSessionInfo, error) {
	info, err := h.s.TuneWithSession(ctx, u, id, caps, sessionID)
	if err != nil {
		return frontend.LiveTVSessionInfo{}, err
	}
	return toFrontendSessionInfo(info), nil
}

func (h liveTVHost) Sessions(ctx context.Context, u *frontend.User) []frontend.LiveTVSessionInfo {
	infos := h.s.Sessions(ctx, u)
	out := make([]frontend.LiveTVSessionInfo, len(infos))
	for i := range infos {
		out[i] = toFrontendSessionInfo(infos[i])
	}
	return out
}

func (h liveTVHost) Session(ctx context.Context, id string, u *frontend.User) (frontend.LiveTVSessionInfo, error) {
	info, err := h.s.Session(ctx, id, u)
	if err != nil {
		return frontend.LiveTVSessionInfo{}, err
	}
	return toFrontendSessionInfo(info), nil
}

func (h liveTVHost) ServeDASH(w http.ResponseWriter, r *http.Request, id string, u *frontend.User) error {
	return h.s.ServeDASH(w, r, id, u)
}

func (h liveTVHost) ServeHTTP(w http.ResponseWriter, r *http.Request, id string, u *frontend.User) error {
	return h.s.ServeHTTP(w, r, id, u)
}

func (h liveTVHost) ViewingSessions() []*frontend.PlaySession {
	return h.s.ViewingSessions()
}

func (h liveTVHost) NoteClientActivity() {
	h.s.NoteClientActivity()
}

func (h liveTVHost) AuthenticateMediaGrant(r *http.Request, active func(context.Context, int64) *frontend.User) (*frontend.User, bool) {
	return h.s.AuthenticateMediaGrant(r, active)
}

func (h liveTVHost) LogoKey() []byte {
	return nil
}

func (h liveTVHost) FilterDiscovery(ctx context.Context, u *frontend.User, ps []frontend.LiveTVProgramme) ([]frontend.LiveTVProgramme, []frontend.LiveTVChannel, error) {
	var err error
	ctx, _, err = h.s.withPolicy(ctx, u)
	if err != nil {
		return nil, nil, err
	}
	if u == nil {
		ctx = h.s.withOwnerGroupIndex(ctx)
	}
	ids := make([]string, 0, len(ps))
	seen := map[string]bool{}
	for _, p := range ps {
		if !seen[p.ChannelID] {
			seen[p.ChannelID] = true
			ids = append(ids, p.ChannelID)
		}
	}
	visible, err := h.s.searchVisibleGroupIDs(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	cs, err := h.s.searchAllowedChannels(ctx, u, ids)
	if err != nil {
		return nil, nil, err
	}
	allowed := map[string]Channel{}
	for _, c := range cs {
		if visible[c.ID] {
			allowed[c.ID] = c
		}
	}
	out := []frontend.LiveTVProgramme{}
	for _, p := range ps {
		if c, ok := allowed[p.ChannelID]; ok {
			if p.ID == "" {
				out = append(out, p)
				continue
			}
			raw := Programme{ID: p.ID, ChannelID: p.ChannelID, Start: p.Start, End: p.End, Item: Item{Key: p.Item.Key, Rating: p.Item.Rating, Genres: p.Item.Genres, MediaType: p.Item.MediaType}}
			var permitted bool
			if raw.Item.Key != 0 {
				// Discovery must never fetch a missing catalogue title on a
				// client request. Local safety facts are authoritative; missing
				// facts fail closed and can appear after ordinary preparation.
				var lookup func(context.Context, core.RatingKey) (*core.Title, error)
				if cached, ok := h.s.o.Catalog.(interface {
					CachedGet(context.Context, core.RatingKey) (*core.Title, error)
				}); ok {
					lookup = cached.CachedGet
				}
				permitted = h.s.allowedWithLookup(ctx, u, c, &raw, lookup, true)
			} else {
				permitted = h.s.allowedProgramme(ctx, u, c, &raw)
			}
			if p.End.After(h.s.o.Now()) && permitted {
				out = append(out, p)
			}
		}
	}
	return out, toFrontendChannels(cs), nil
}

var _ frontend.LiveGuideWindowReader = liveTVHost{}

func (h liveTVHost) StartCatchUp(ctx context.Context, u *frontend.User, req frontend.CatchUpRequest, caps frontend.ClientCaps, sessionID string) (frontend.CatchUpInfo, error) {
	return h.s.StartCatchUp(ctx, u, req, caps, sessionID)
}
func (h liveTVHost) SeekCatchUp(ctx context.Context, u *frontend.User, id string, at time.Duration) (frontend.CatchUpInfo, error) {
	return h.s.SeekCatchUp(ctx, u, id, at)
}
func (h liveTVHost) TouchLiveSession(ctx context.Context, u *frontend.User, id string) error {
	return h.s.TouchLiveSession(ctx, u, id)
}
func (h liveTVHost) CatchUpLiveWindow(ctx context.Context, id string) (frontend.LiveWindow, error) {
	return h.s.LiveWindow(ctx, id)
}
func (h liveTVHost) ServeCatchUpTS(w http.ResponseWriter, r *http.Request, id string, u *frontend.User) {
	h.s.ServeCatchUpTS(w, r, id, u)
}
