package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/livetv"
)

const maxLiveProgrammes = 10000

var errLiveQuery = errors.New("invalid live guide query")

type surfaceLiveTV struct {
	app              *App
	surface          string // the app this lineup is for; the owner's per-app visibility applies
	channelArtwork   func(*core.Viewer, livetv.Channel) frontend.Image
	programmeArtwork func(*core.Viewer, livetv.Programme) []frontend.Image
	artwork          frontend.Artwork
}

func (s surfaceLiveTV) withApp(ctx context.Context) context.Context {
	if s.surface == "" {
		return ctx
	}
	return core.WithLiveApp(ctx, s.surface)
}

func (s surfaceLiveTV) Enabled() bool {
	return s.app != nil && s.app.livetv != nil && s.app.livetv.Enabled()
}

func (s surfaceLiveTV) LiveGuideWindowAvailable() bool {
	return s.app != nil && s.app.livetv != nil
}

func (s surfaceLiveTV) host() frontend.LiveTV {
	if s.app == nil || s.app.livetv == nil {
		return nil
	}
	return s.app.livetv.Frontend()
}

func (s surfaceLiveTV) viewer(ctx context.Context, v *core.Viewer) (*core.User, error) {
	if v == nil || v.ID <= 0 {
		return nil, core.ErrUnauthorized
	}
	viewer, err := s.app.users.ActiveUser(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	return s.app.safety.UserPolicy(viewer), nil
}

func (s surfaceLiveTV) liveChannel(ctx context.Context, viewer *core.Viewer, c livetv.Channel) (frontend.LiveChannel, error) {
	groups, err := s.app.livetv.ChannelGroups(ctx, c.SourceID, c.Group, c.ID)
	if err != nil {
		return frontend.LiveChannel{}, err
	}
	result := frontend.LiveChannel{ArchiveDays: c.ArchiveDays(), ID: c.ID, Number: c.Number, Name: c.Name, Kind: c.Kind, Rating: c.Rating, Favorite: c.Favorite, Group: c.Group, Groups: groups}
	if s.channelArtwork != nil {
		if image := s.channelArtwork(viewer, c); image.Path != "" {
			result.Images = []frontend.Image{image}
		}
	}
	return result, nil
}

func (s surfaceLiveTV) Channels(ctx context.Context, v *core.Viewer) ([]frontend.LiveChannel, error) {
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return nil, err
	}
	return s.channels(ctx, viewer, nil)
}

func (s surfaceLiveTV) ReadChannelSelection(ctx context.Context, v *core.Viewer, ids []string) ([]frontend.LiveChannel, error) {
	if v == nil || v.ID <= 0 {
		return nil, core.ErrUnauthorized
	}
	viewer, err := s.app.users.PolicyUser(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	return s.channels(ctx, s.app.safety.UserPolicy(viewer), ids)
}

func (s surfaceLiveTV) ChannelSelectionRevision(ctx context.Context) (frontend.LiveChannelRevision, error) {
	var rev frontend.LiveChannelRevision
	err := s.app.db.QueryRowContext(ctx, `SELECT v.generation, c.generation
		FROM viewer_policy_epoch v CROSS JOIN livetv_channel_epoch c WHERE v.id=1 AND c.id=1`).Scan(&rev.Policy, &rev.Lineup)
	return rev, err
}

func (s surfaceLiveTV) channels(ctx context.Context, viewer *core.Viewer, ids []string) ([]frontend.LiveChannel, error) {
	ctx = s.withApp(ctx)
	if s.app.livetv == nil {
		return nil, core.ErrNotAvailable
	}
	ctx = s.app.livetv.WithOwnerGroupIndex(ctx)
	var channels []livetv.Channel
	var err error
	if ids == nil {
		channels, err = s.app.livetv.AllowedChannels(ctx, viewer)
	} else {
		channels, err = s.app.livetv.AllowedChannelsForIDs(ctx, viewer, ids)
	}
	if err == nil {
		s.app.livetv.NoteClientActivity()
	}
	if err != nil {
		return nil, err
	}
	out := make([]frontend.LiveChannel, 0, len(channels))
	for _, c := range channels {
		row, err := s.liveChannel(ctx, viewer, c)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

func (s surfaceLiveTV) Groups(ctx context.Context, u *frontend.User) ([]frontend.LiveGroup, error) {
	ctx = s.withApp(ctx)
	viewer, err := s.viewer(ctx, u)
	if err != nil {
		return nil, err
	}
	if h, ok := s.host().(frontend.LiveTVGroups); ok {
		return h.Groups(ctx, viewer)
	}
	return []frontend.LiveGroup{}, nil
}

func (s surfaceLiveTV) GroupChannels(ctx context.Context, u *frontend.User, id string) ([]frontend.LiveTVChannel, error) {
	ctx = s.withApp(ctx)
	viewer, err := s.viewer(ctx, u)
	if err != nil {
		return nil, err
	}
	if h, ok := s.host().(frontend.LiveTVGroups); ok {
		return h.GroupChannels(ctx, viewer, id)
	}
	return nil, core.ErrNotFound
}

func (s surfaceLiveTV) SearchLiveTV(ctx context.Context, v *core.Viewer, query string, limit int) (frontend.LiveSearchResults, error) {
	ctx = s.withApp(ctx)
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return frontend.LiveSearchResults{}, err
	}
	if h, ok := s.host().(frontend.LiveTVSearch); ok {
		return h.SearchLiveTV(ctx, viewer, query, limit)
	}
	return frontend.LiveSearchResults{}, nil
}

func (s surfaceLiveTV) Guide(ctx context.Context, v *core.Viewer, q frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
	page, err := s.guidePage(ctx, v, frontend.LiveGuidePageQuery{LiveGuideQuery: q}, false)
	return page.Items, err
}

func (s surfaceLiveTV) GuidePage(ctx context.Context, v *core.Viewer, pageQuery frontend.LiveGuidePageQuery) (frontend.LiveGuidePage, error) {
	return s.guidePage(ctx, v, pageQuery, true)
}

func (s surfaceLiveTV) guidePage(ctx context.Context, v *core.Viewer, pageQuery frontend.LiveGuidePageQuery, paged bool) (frontend.LiveGuidePage, error) {
	q := pageQuery.LiveGuideQuery
	ctx = s.withApp(ctx)
	maxSpan := 7 * 24 * time.Hour
	if s.surface == "xtream" && len(q.Channels) == 1 {
		maxSpan = 32 * 24 * time.Hour
	}
	if !q.Until.After(q.From) || q.Until.Sub(q.From) > maxSpan || q.Limit < 0 || q.Limit > maxLiveProgrammes || pageQuery.Offset < 0 {
		return frontend.LiveGuidePage{}, errLiveQuery
	}
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return frontend.LiveGuidePage{}, err
	}
	if s.app.livetv == nil {
		return frontend.LiveGuidePage{}, core.ErrNotAvailable
	}
	s.app.livetv.NoteClientActivity()
	var permitted []livetv.Channel
	if len(q.Channels) > 0 {
		permitted, err = s.app.livetv.AllowedChannelsForIDs(ctx, viewer, q.Channels)
	} else {
		permitted, err = s.app.livetv.AllowedChannels(ctx, viewer)
	}
	if err != nil {
		return frontend.LiveGuidePage{}, err
	}
	selected := make(map[string]bool, len(q.Channels))
	for _, id := range q.Channels {
		selected[id] = true
	}
	allowed := make(map[string]int, len(permitted))
	for _, c := range permitted {
		if len(selected) == 0 || selected[c.ID] {
			allowed[c.ID] = c.ArchiveDays()
		}
	}
	// TODO: bound upstream guide materialization after policy filtering; retain
	// the existing Plex scheduling semantics while applying a response cap here.
	var programmes []livetv.Programme
	if len(q.Channels) > 0 {
		programmes, err = s.app.livetv.GuideForChannels(ctx, viewer, q.From, q.Until, q.Channels)
	} else {
		programmes, err = s.app.livetv.Guide(ctx, viewer, q.From, q.Until)
	}
	if err != nil {
		return frontend.LiveGuidePage{}, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = maxLiveProgrammes
	}
	// The source materializes the selected time window from its guide snapshot.
	// Fingerprint that authorized order without counting the media library or
	// hydrating titles outside this response page.
	fingerprint := sha256.New()
	encoder := json.NewEncoder(fingerprint)
	if paged {
		if err := encoder.Encode([]any{q.From.UTC(), q.Until.UTC(), q.Channels}); err != nil {
			return frontend.LiveGuidePage{}, err
		}
	}
	selectedPage := make([]livetv.Programme, 0, min(len(programmes), limit+1))
	index := 0
	for _, p := range programmes {
		if _, ok := allowed[p.ChannelID]; !ok {
			continue
		}
		if paged {
			if err := encoder.Encode([]any{p.ID, p.ChannelID, p.Start.UTC(), p.End.UTC(), p.Item.Key, p.Item.Title, p.Item.Summary, p.Item.Rating, p.Item.Thumb, p.Item.Art, p.Item.Genres}); err != nil {
				return frontend.LiveGuidePage{}, err
			}
		}
		if index >= pageQuery.Offset && len(selectedPage) <= limit {
			selectedPage = append(selectedPage, p)
		}
		index++
		if !paged && len(selectedPage) == limit {
			break
		}
	}
	snapshot := hex.EncodeToString(fingerprint.Sum(nil))
	if pageQuery.Snapshot != "" && pageQuery.Snapshot != snapshot {
		return frontend.LiveGuidePage{}, frontend.ErrLiveGuideStale
	}
	more := len(selectedPage) > limit
	selectedPage = selectedPage[:min(len(selectedPage), limit)]
	out := make([]frontend.LiveProgramme, 0, len(selectedPage))
	imageCache := make(map[core.RatingKey][]frontend.Image)
	for _, p := range selectedPage {
		item := frontend.LiveProgramme{ArchiveDays: allowed[p.ChannelID], ID: p.ID, ChannelID: p.ChannelID, Start: p.Start, End: p.End, Item: p.Item.Key, Title: p.Item.Title, Summary: p.Item.Summary, Rating: p.Item.Rating, ShowTitle: p.Item.ShowTitle, EpisodeTitle: p.Item.EpisodeTitle, Season: p.Item.Season, Episode: p.Item.Episode, Genres: p.Item.Genres}
		if s.programmeArtwork != nil {
			item.Images = s.programmeArtwork(viewer, p)
		}
		if p.Item.Key != 0 && s.app.catalog != nil {
			if title, e := s.app.catalog.Get(ctx, p.Item.Key); e == nil {
				if item.ShowTitle == "" {
					item.ShowTitle = title.ShowTitle
				}
				if item.EpisodeTitle == "" && title.Type == core.TypeEpisode {
					item.EpisodeTitle = title.Title
				}
				if item.Season == 0 {
					item.Season = title.Season
				}
				if item.Episode == 0 {
					item.Episode = title.Episode
				}
				if len(item.Genres) == 0 {
					item.Genres = title.Genres
				}
			}
			if s.artwork != nil && len(item.Images) == 0 {
				images, found := imageCache[p.Item.Key]
				if !found {
					var complete bool
					images, complete, _ = s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: viewer, Device: q.Device, Item: p.Item.Key})
					if !complete {
						images = nil
					}
					imageCache[p.Item.Key] = images
				}
				item.Images = images
			}
		}
		out = append(out, item)
	}
	return frontend.LiveGuidePage{Items: out, Snapshot: snapshot, More: more}, nil
}

func (s surfaceLiveTV) GuideWindowBatch(ctx context.Context, v *core.Viewer, q frontend.LiveGuideWindowQuery) (frontend.LiveGuideWindow, error) {
	ctx = s.withApp(ctx)
	maxWindow := time.Duration(frontend.MaxLiveGuideWindowMinutes) * time.Minute
	if !q.Until.After(q.From) || q.Until.Sub(q.From) > maxWindow || len(q.Channels) == 0 || len(q.Channels) > frontend.MaxLiveGuideWindowChannels {
		return frontend.LiveGuideWindow{}, errLiveQuery
	}
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return frontend.LiveGuideWindow{}, err
	}
	if s.app == nil || s.app.livetv == nil {
		return frontend.LiveGuideWindow{}, core.ErrNotAvailable
	}
	s.app.livetv.NoteClientActivity()
	window, err := s.app.livetv.GuideWindowForChannels(ctx, viewer, q.From, q.Until, q.Channels)
	if err != nil {
		return frontend.LiveGuideWindow{}, err
	}

	var cachedTitles map[core.RatingKey]core.Title
	if s.app.catalog != nil {
		seenKeys := make(map[core.RatingKey]bool)
		var keys []core.RatingKey
		for _, ch := range window.Channels {
			for _, p := range ch.Programmes {
				if p.Item.Key != 0 && !seenKeys[p.Item.Key] {
					seenKeys[p.Item.Key] = true
					keys = append(keys, p.Item.Key)
				}
			}
		}
		if len(keys) > 0 {
			cachedTitles = make(map[core.RatingKey]core.Title, len(keys))
			for start := 0; start < len(keys); start += 400 {
				batch := keys[start:min(start+400, len(keys))]
				found, e := s.app.catalog.CachedGetMany(ctx, batch)
				if e != nil {
					return frontend.LiveGuideWindow{}, e
				}
				for k, t := range found {
					cachedTitles[k] = t
				}
			}
		}
	}

	fingerprint := sha256.New()
	encoder := json.NewEncoder(fingerprint)
	if err := encoder.Encode([]any{q.From.UTC(), q.Until.UTC()}); err != nil {
		return frontend.LiveGuideWindow{}, err
	}

	imageCache := make(map[core.RatingKey][]frontend.Image)
	rows := make([]frontend.LiveGuideWindowChannel, len(window.Channels))
	for i, ch := range window.Channels {
		if err := encoder.Encode([]any{ch.ChannelID, ch.State}); err != nil {
			return frontend.LiveGuideWindow{}, err
		}
		items := make([]frontend.LiveProgramme, len(ch.Programmes))
		for j, p := range ch.Programmes {
			item := frontend.LiveProgramme{
				ArchiveDays: ch.ArchiveDays, ID: p.ID,
				ChannelID:    p.ChannelID,
				Start:        p.Start,
				End:          p.End,
				Item:         p.Item.Key,
				Title:        p.Item.Title,
				Summary:      p.Item.Summary,
				Rating:       p.Item.Rating,
				ShowTitle:    p.Item.ShowTitle,
				EpisodeTitle: p.Item.EpisodeTitle,
				Season:       p.Item.Season,
				Episode:      p.Item.Episode,
				Genres:       p.Item.Genres,
			}
			if s.programmeArtwork != nil {
				item.Images = s.programmeArtwork(viewer, p)
			}
			if p.Item.Key != 0 && s.app.catalog != nil {
				title, hasTitle := cachedTitles[p.Item.Key]
				if hasTitle {
					if item.ShowTitle == "" {
						item.ShowTitle = title.ShowTitle
					}
					if item.EpisodeTitle == "" && title.Type == core.TypeEpisode {
						item.EpisodeTitle = title.Title
					}
					if item.Season == 0 {
						item.Season = title.Season
					}
					if item.Episode == 0 {
						item.Episode = title.Episode
					}
					if len(item.Genres) == 0 {
						item.Genres = title.Genres
					}
				}
				if s.artwork != nil && len(item.Images) == 0 {
					images, found := imageCache[p.Item.Key]
					if !found {
						artCtx := ctx
						if hasTitle {
							artCtx = frontend.WithArtworkTitle(ctx, &title)
						}
						var complete bool
						images, complete, _ = s.artwork.List(artCtx, frontend.ArtworkRequest{Viewer: viewer, Device: q.Device, Item: p.Item.Key})
						if !complete {
							images = nil
						}
						imageCache[p.Item.Key] = images
					}
					item.Images = images
				}
			}
			if err := encoder.Encode([]any{item.ID, item.ChannelID, item.Start.UTC(), item.End.UTC(), item.Item, item.Title, item.Summary, item.Rating, item.ShowTitle, item.EpisodeTitle, item.Season, item.Episode, item.Genres, p.Item.Thumb, p.Item.Art}); err != nil {
				return frontend.LiveGuideWindow{}, err
			}
			items[j] = item
		}
		rows[i] = frontend.LiveGuideWindowChannel{ArchiveDays: ch.ArchiveDays,
			ChannelID:  ch.ChannelID,
			State:      ch.State,
			Programmes: items,
		}
	}

	return frontend.LiveGuideWindow{
		From:       window.From,
		Until:      window.Until,
		Revision:   hex.EncodeToString(fingerprint.Sum(nil)),
		ValidUntil: window.ValidUntil,
		Channels:   rows,
	}, nil
}

func normalizeLiveGUID(raw string, kind ids.Kind) (string, error) {
	n, err := ids.ParseGUID(kind, raw)
	if err != nil {
		return "", core.ErrNotFound
	}
	return ids.GUID(kind, n)
}

func (s surfaceLiveTV) ResolveChannelGUID(ctx context.Context, v *core.Viewer, guid string) (frontend.LiveChannel, error) {
	ctx = s.withApp(ctx)
	normalized, err := normalizeLiveGUID(guid, ids.Channel)
	if err != nil {
		return frontend.LiveChannel{}, err
	}
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return frontend.LiveChannel{}, err
	}
	if s.app.livetv == nil {
		return frontend.LiveChannel{}, core.ErrNotAvailable
	}
	ctx = s.app.livetv.WithOwnerGroupIndex(ctx)
	channels, err := s.app.livetv.Channels(ctx)
	if err != nil {
		return frontend.LiveChannel{}, err
	}
	var matched []livetv.Channel
	for _, c := range channels {
		if !c.Enabled {
			continue
		}
		id, _ := ids.StringGUID(ids.Channel, c.ID)
		if strings.EqualFold(id, normalized) {
			matched = append(matched, c)
		}
	}
	var cached func(context.Context, core.RatingKey) (*core.Title, error)
	if s.app.catalog != nil {
		cached = s.app.catalog.CachedGet
	}
	var found *frontend.LiveChannel
	for _, c := range matched {
		if err := s.app.livetv.AllowedChannelCached(ctx, viewer, c.ID, cached); err != nil {
			if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnauthorized) {
				continue
			}
			return frontend.LiveChannel{}, err
		}
		if found != nil {
			return frontend.LiveChannel{}, core.ErrUnauthorized
		}
		row, err := s.liveChannel(ctx, viewer, c)
		if err != nil {
			return frontend.LiveChannel{}, err
		}
		found = &row
	}
	if found == nil {
		return frontend.LiveChannel{}, core.ErrNotFound
	}
	return *found, nil
}

func (s surfaceLiveTV) ResolveProgrammeGUID(ctx context.Context, v *core.Viewer, guid string, q frontend.LiveGuideQuery) (frontend.LiveProgramme, error) {
	normalized, err := normalizeLiveGUID(guid, ids.Programme)
	if err != nil {
		return frontend.LiveProgramme{}, err
	}
	rows, err := s.Guide(ctx, v, q)
	if err != nil {
		return frontend.LiveProgramme{}, err
	}
	var found *frontend.LiveProgramme
	for _, row := range rows {
		id, _ := ids.StringGUID(ids.Programme, row.ID)
		if strings.EqualFold(id, normalized) {
			if found != nil {
				return frontend.LiveProgramme{}, core.ErrUnauthorized
			}
			copy := row
			found = &copy
		}
	}
	if found == nil {
		return frontend.LiveProgramme{}, core.ErrNotFound
	}
	return *found, nil
}

func (s surfaceLiveTV) VisibleTo(ctx context.Context, u *frontend.User) bool {
	h := s.host()
	return h != nil && h.VisibleTo(s.withApp(ctx), u)
}
func (s surfaceLiveTV) ServeXMLTV(w http.ResponseWriter, r *http.Request, u *frontend.User) {
	if h := s.host(); h != nil {
		if s.surface != "" {
			r = r.WithContext(s.withApp(r.Context()))
		}
		h.ServeXMLTV(w, r, u)
	}
}
func (s surfaceLiveTV) Stop(ctx context.Context, id string, u *frontend.User) error {
	h := s.host()
	if h == nil {
		return core.ErrNotAvailable
	}
	return h.Stop(ctx, id, u)
}
func (s surfaceLiveTV) ServeDASHPart(w http.ResponseWriter, r *http.Request, id string, u *frontend.User, rep, n int, init bool) {
	if h := s.host(); h != nil {
		h.ServeDASHPart(w, r, id, u, rep, n, init)
	}
}
func (s surfaceLiveTV) ServePlaylist(w http.ResponseWriter, r *http.Request, id string, u *frontend.User) {
	if h := s.host(); h != nil {
		h.ServePlaylist(w, r.WithContext(s.withApp(r.Context())), id, u)
	}
}
func (s surfaceLiveTV) ServeRelaySegment(w http.ResponseWriter, r *http.Request, id string, n int, u *frontend.User) {
	if h := s.host(); h != nil {
		h.ServeRelaySegment(w, r.WithContext(s.withApp(r.Context())), id, n, u)
	}
}
func (s surfaceLiveTV) ServeResource(w http.ResponseWriter, r *http.Request, id, resource string, u *frontend.User) {
	if h := s.host(); h != nil {
		h.ServeResource(w, r, id, resource, u)
	}
}
func (s surfaceLiveTV) AllowedChannels(ctx context.Context, u *frontend.User) ([]frontend.LiveTVChannel, error) {
	h := s.host()
	if h == nil {
		return nil, core.ErrNotAvailable
	}
	return h.AllowedChannels(s.withApp(ctx), u)
}
func (s surfaceLiveTV) AllowedChannel(ctx context.Context, u *frontend.User, id string) (frontend.LiveTVChannel, error) {
	h := s.host()
	if h == nil {
		return frontend.LiveTVChannel{}, core.ErrNotAvailable
	}
	return h.AllowedChannel(s.withApp(ctx), u, id)
}
func (s surfaceLiveTV) Channel(ctx context.Context, id string) (frontend.LiveTVChannel, error) {
	h := s.host()
	if h == nil {
		return frontend.LiveTVChannel{}, core.ErrNotAvailable
	}
	return h.Channel(ctx, id)
}
func (s surfaceLiveTV) ChannelForViewer(ctx context.Context, u *frontend.User, id string) (frontend.LiveTVChannel, error) {
	h := s.host()
	if h == nil {
		return frontend.LiveTVChannel{}, core.ErrNotAvailable
	}
	return h.ChannelForViewer(ctx, u, id)
}
func (s surfaceLiveTV) AllChannels(ctx context.Context) ([]frontend.LiveTVChannel, error) {
	h := s.host()
	if h == nil {
		return nil, core.ErrNotAvailable
	}
	return h.AllChannels(ctx)
}
func (s surfaceLiveTV) ChannelPermitted(ctx context.Context, u *frontend.User, id string) (bool, error) {
	h := s.host()
	if h == nil {
		return false, core.ErrNotAvailable
	}
	return h.ChannelPermitted(s.withApp(ctx), u, id)
}
func (s surfaceLiveTV) LineupChangedFor(u *frontend.User) time.Time {
	if h := s.host(); h != nil {
		return h.LineupChangedFor(u)
	}
	return time.Time{}
}
func (s surfaceLiveTV) GuideForChannels(ctx context.Context, u *frontend.User, from, until time.Time, channelIDs []string) ([]frontend.LiveTVProgramme, error) {
	h := s.host()
	if h == nil {
		return nil, core.ErrNotAvailable
	}
	return h.GuideForChannels(s.withApp(ctx), u, from, until, channelIDs)
}
func (s surfaceLiveTV) GuideWindow(ctx context.Context, u *frontend.User, from, until time.Time) ([]frontend.LiveTVProgramme, error) {
	h := s.host()
	if h == nil {
		return nil, core.ErrNotAvailable
	}
	return h.GuideWindow(s.withApp(ctx), u, from, until)
}
func (s surfaceLiveTV) GuideChannelLogo(ctx context.Context, c frontend.LiveTVChannel) string {
	if h := s.host(); h != nil {
		return h.GuideChannelLogo(ctx, c)
	}
	return ""
}
func (s surfaceLiveTV) TuneWithSession(ctx context.Context, u *frontend.User, id string, caps frontend.ClientCaps, sessionID string) (frontend.LiveTVSessionInfo, error) {
	h := s.host()
	if h == nil {
		return frontend.LiveTVSessionInfo{}, core.ErrNotAvailable
	}
	return h.TuneWithSession(s.withApp(ctx), u, id, liveTVClientCaps(caps), sessionID)
}
func (s surfaceLiveTV) Sessions(ctx context.Context, u *frontend.User) []frontend.LiveTVSessionInfo {
	if h := s.host(); h != nil {
		return h.Sessions(ctx, u)
	}
	return nil
}
func (s surfaceLiveTV) Session(ctx context.Context, id string, u *frontend.User) (frontend.LiveTVSessionInfo, error) {
	h := s.host()
	if h == nil {
		return frontend.LiveTVSessionInfo{}, core.ErrNotAvailable
	}
	return h.Session(ctx, id, u)
}
func (s surfaceLiveTV) ServeDASH(w http.ResponseWriter, r *http.Request, id string, u *frontend.User) error {
	h := s.host()
	if h == nil {
		return core.ErrNotAvailable
	}
	return h.ServeDASH(w, r, id, u)
}
func (s surfaceLiveTV) ServeHTTP(w http.ResponseWriter, r *http.Request, id string, u *frontend.User) error {
	h := s.host()
	if h == nil {
		return core.ErrNotAvailable
	}
	return h.ServeHTTP(w, r, id, u)
}
func (s surfaceLiveTV) ViewingSessions() []*frontend.PlaySession {
	if h := s.host(); h != nil {
		return h.ViewingSessions()
	}
	return nil
}
func (s surfaceLiveTV) NoteClientActivity() {
	if h := s.host(); h != nil {
		h.NoteClientActivity()
	}
}
func (s surfaceLiveTV) AuthenticateMediaGrant(r *http.Request, active func(context.Context, int64) *frontend.User) (*frontend.User, bool) {
	if h := s.host(); h != nil {
		return h.AuthenticateMediaGrant(r, active)
	}
	return nil, false
}
func (s surfaceLiveTV) LogoKey() []byte {
	if s.app != nil && s.app.cfg != nil {
		return s.app.cfg.DerivedKey("ether-live-logo-v1")
	}
	return nil
}

var (
	_ frontend.LiveTV                = surfaceLiveTV{}
	_ frontend.LiveTVGroups          = surfaceLiveTV{}
	_ frontend.LiveTVSearch          = surfaceLiveTV{}
	_ frontend.LiveGuidePager        = surfaceLiveTV{}
	_ frontend.LiveGuideWindowReader = surfaceLiveTV{}
)

func (s surfaceLiveTV) FilterDiscovery(ctx context.Context, u *frontend.User, ps []frontend.LiveTVProgramme) ([]frontend.LiveTVProgramme, []frontend.LiveTVChannel, error) {
	viewer, err := s.viewer(ctx, u)
	if err != nil {
		return nil, nil, err
	}
	if h, ok := s.host().(frontend.LiveTVDiscoveryPolicy); ok {
		return h.FilterDiscovery(s.withApp(ctx), viewer, ps)
	}
	return nil, nil, frontend.ErrNotAvailable
}

func (s surfaceLiveTV) StartCatchUp(ctx context.Context, v *frontend.User, req frontend.CatchUpRequest, caps frontend.ClientCaps, sessionID string) (frontend.CatchUpInfo, error) {
	if !s.Enabled() {
		return frontend.CatchUpInfo{}, core.ErrNotAvailable
	}
	if sessionID == "" {
		sessionID = caps.ClientID
	}
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return frontend.CatchUpInfo{}, err
	}
	return s.app.livetv.StartCatchUp(s.withApp(ctx), viewer, req, liveTVClientCaps(caps), sessionID)
}

func (s surfaceLiveTV) SeekCatchUp(ctx context.Context, v *frontend.User, id string, at time.Duration) (frontend.CatchUpInfo, error) {
	if !s.Enabled() {
		return frontend.CatchUpInfo{}, core.ErrNotAvailable
	}
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return frontend.CatchUpInfo{}, err
	}
	return s.app.livetv.SeekCatchUp(s.withApp(ctx), viewer, id, at)
}

func (s surfaceLiveTV) TouchLiveSession(ctx context.Context, v *frontend.User, id string) error {
	if !s.Enabled() {
		return core.ErrNotAvailable
	}
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return err
	}
	return s.app.livetv.TouchLiveSession(s.withApp(ctx), viewer, id)
}

func (s surfaceLiveTV) CatchUpLiveWindow(ctx context.Context, id string) (frontend.LiveWindow, error) {
	if !s.Enabled() {
		return frontend.LiveWindow{}, core.ErrNotAvailable
	}
	return (sessionLive{service: s.app.livetv}).LiveWindow(ctx, id)
}

func (s surfaceLiveTV) ServeCatchUpTS(w http.ResponseWriter, r *http.Request, id string, v *frontend.User) {
	if !s.Enabled() {
		http.NotFound(w, r)
		return
	}
	viewer, err := s.viewer(r.Context(), v)
	if err != nil {
		http.Error(w, "Playback unavailable", http.StatusForbidden)
		return
	}
	s.app.livetv.ServeCatchUpTS(w, r.WithContext(s.withApp(r.Context())), id, viewer)
}

var _ frontend.LiveCatchUp = surfaceLiveTV{}

func (s surfaceLiveTV) LiveCatchUpMaxDays(ctx context.Context, v *frontend.Viewer) (int, error) {
	if !s.Enabled() {
		return 0, nil
	}
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return 0, err
	}
	return s.app.livetv.CatchUpMaxDays(s.withApp(ctx), viewer)
}

func (s surfaceLiveTV) LiveTimeshiftMinutes(ctx context.Context, id string) int {
	if !s.Enabled() {
		return 0
	}
	return s.app.livetv.LiveTimeshiftMinutes(ctx, id)
}

// ServeLiveSessionMedia keeps a start-over handoff on the scoped Vela media route.
func (s surfaceLiveTV) ServeLiveSessionMedia(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, id, tail string) {
	if !s.Enabled() {
		http.NotFound(w, r)
		return
	}
	viewer, err := s.viewer(r.Context(), v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	(sessionLive{service: s.app.livetv}).Serve(w, r.WithContext(s.withApp(r.Context())), viewer, id, tail, "")
}

func (s surfaceLiveTV) catchUpLibrary(ctx context.Context, v *frontend.Viewer) (frontend.CatchUpLibrary, *core.User, error) {
	h, ok := s.host().(frontend.CatchUpLibrary)
	if !ok {
		return nil, nil, nil
	}
	viewer, err := s.viewer(ctx, v)
	return h, viewer, err
}

func (s surfaceLiveTV) CatchUpLibraryVisible(ctx context.Context, v *frontend.Viewer) (bool, error) {
	h, viewer, err := s.catchUpLibrary(ctx, v)
	if err != nil || h == nil {
		return false, err
	}
	return h.CatchUpLibraryVisible(s.withApp(ctx), viewer)
}

func (s surfaceLiveTV) CatchUpItems(ctx context.Context, v *frontend.Viewer, q frontend.CatchUpQuery) (frontend.CatchUpPage, error) {
	h, viewer, err := s.catchUpLibrary(ctx, v)
	if err != nil || h == nil {
		return frontend.CatchUpPage{}, err
	}
	page, err := h.CatchUpItems(s.withApp(ctx), viewer, q)
	for j := range page.Items {
		s.catchUpArtwork(viewer, &page.Items[j])
	}
	return page, err
}

func (s surfaceLiveTV) CatchUpItem(ctx context.Context, v *frontend.Viewer, id string) (frontend.CatchUpItem, error) {
	h, viewer, err := s.catchUpLibrary(ctx, v)
	if err != nil {
		return frontend.CatchUpItem{}, err
	}
	if h == nil {
		return frontend.CatchUpItem{}, core.ErrNotFound
	}
	item, err := h.CatchUpItem(s.withApp(ctx), viewer, id)
	if err == nil {
		s.catchUpArtwork(viewer, &item)
	}
	return item, err
}

func (s surfaceLiveTV) catchUpArtwork(viewer *core.Viewer, item *frontend.CatchUpItem) {
	p := &item.Programme
	if s.programmeArtwork != nil {
		item.Artwork = s.programmeArtwork(viewer, livetv.Programme{
			ID: p.ID, ChannelID: p.ChannelID, Start: p.Start, End: p.End,
			Item: livetv.Item{Thumb: p.Item.Thumb, Art: p.Item.Art},
		})
	}
	p.Item.Thumb, p.Item.Art = "", ""
}

func (s surfaceLiveTV) CatchUpArtwork(ctx context.Context, v *frontend.Viewer, id, kind string) ([]byte, string, error) {
	if s.artwork == nil {
		return nil, "", core.ErrNotFound
	}
	item, err := s.CatchUpItem(ctx, v, id)
	if err != nil {
		return nil, "", err
	}
	for _, image := range item.Artwork {
		if image.Kind == kind {
			return s.artwork.Read(s.withApp(ctx), frontend.ArtworkRequest{
				Viewer: v, Channel: item.Programme.ChannelID, Programme: item.Programme.ID,
			}, image, frontend.ArtworkTransform{})
		}
	}
	return nil, "", core.ErrNotFound
}

func (s surfaceLiveTV) FindCatchUp(ctx context.Context, v *frontend.Viewer, req frontend.CatchUpRequest, caps frontend.ClientCaps, clientSession string) (frontend.CatchUpInfo, bool) {
	h, ok := s.host().(frontend.CatchUpSessionFinder)
	if !ok {
		return frontend.CatchUpInfo{}, false
	}
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return frontend.CatchUpInfo{}, false
	}
	return h.FindCatchUp(s.withApp(ctx), viewer, req, liveTVClientCaps(caps), clientSession)
}

func (s surfaceLiveTV) LibraryCatchUpSession(access frontend.SessionAccess, id string) (frontend.CatchUpInfo, bool) {
	h, ok := s.host().(frontend.CatchUpSessionReader)
	if !ok {
		return frontend.CatchUpInfo{}, false
	}
	return h.LibraryCatchUpSession(access, id)
}
