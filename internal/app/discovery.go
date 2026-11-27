package app

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/discovery"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/langfeat"
	"github.com/braedonsaunders/ether/internal/overlays"
	"github.com/braedonsaunders/ether/internal/personalize"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/ratings"
	"github.com/braedonsaunders/ether/internal/reqmemo"
	"github.com/braedonsaunders/ether/internal/watchlist"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func (a *App) buildDiscovery() {
	a.ratings = ratings.New(a.db, ratings.Options{Sealer: a.cfg, Certification: a.catalog.ApplyProviderCertification, Unrated: a.catalog.UnratedTitles})
	var sourceResolver core.Resolver
	var sourceEngine core.Engine
	var sourceCache core.CacheProvenanceRecorder
	if a.resolver != nil {
		sourceResolver = a.resolver
	}
	if a.media != nil {
		sourceEngine = a.media
	}
	if a.cache != nil {
		sourceCache = a.cache
	}
	a.watchlist = watchlist.New(a.db, watchlist.Options{
		Logger:   a.log.With("component", "watchlist"),
		Sealer:   a.cfg,
		Catalog:  a.catalog,
		Resolver: sourceResolver,
		Engine:   sourceEngine,
		Cache:    sourceCache,
		Trakt:    a.trakt,
		Users:    a.users.Users,
		User:     a.users.User,
		Rows: func(ctx context.Context, viewer *core.User) []core.Row {
			if a.personalize == nil {
				return nil
			}
			return a.personalize.WatchlistRows(ctx, viewer)
		},
		Allowed:    a.safety.Allowed,
		Notifier:   a.notifications,
		Tokens:     a.discoveryToken,
		OwnerToken: func(context.Context) (string, error) { return a.cfg.Read().Plex.Token, nil },
		PlexLookup: func(ctx context.Context, guid string) (core.RatingKey, error) {
			if a.plexGUID == nil {
				return 0, core.ErrNotFound
			}
			return a.plexGUID.Lookup(ctx, guid)
		},
		ClientIdentifier: plexguid.ClientIdentifier(a.cfg.Get().Plex.ClientIdentifier, a.cfg.Get().Server.MachineIdentifier),
		PrewarmBytes:     a.cfg.Get().Cache.PrefetchBytes,
		ManagedWarming:   true,
		Availability: func(ctx context.Context, title *core.Title) (bool, bool, error) {
			if a.availability == nil {
				return false, false, nil
			}
			return a.availability.WatchlistEvidence(ctx, title)
		},
		OnAdded:        a.cachefill.Wake,
		ResumeSchedule: a.clustered(), // a new leader continues the schedule
		OnAvailable: func(ctx context.Context, userID int64, key core.RatingKey) {
			warmKey := key
			if warmKey.Kind() == core.KindEpisode {
				warmKey = core.ShowKey(warmKey.TMDB())
			}
			_, err := a.db.ExecContext(ctx, `INSERT INTO watchlist_warm_intents(user_id,item_key,added) VALUES(?,?,?) ON CONFLICT(user_id,item_key) DO UPDATE SET added=excluded.added`, userID, warmKey, time.Now().Unix())
			if err != nil {
				a.log.Warn("queue watchlist warm intent", "err", err)
			}
			a.cachefill.Wake()
			a.onWatchlistAvailable(ctx, userID, key)
			a.publishWatchlistChange(ctx, userID, key)
		},
		OnUnavailable: func(ctx context.Context, userID int64, title core.Title, reason string) {
			a.onWatchlistUnavailable(ctx, userID, title, reason)
			a.publishWatchlistChange(ctx, userID, title.Key)
		},
		OnSynced: a.onWatchlistSynced,
		OnFriendsRSSChanged: func(ctx context.Context) {
			if a.collections != nil {
				if err := a.collections.SyncFriendsWatchlist(ctx); err != nil && a.log != nil {
					a.log.Warn("watchlist friends collection sync failed", "err", err)
				}
			}
			if a.personalize != nil {
				a.personalize.InvalidateOwnerRows()
			}
		},
	})
	if a.collections != nil {
		ensureCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := a.collections.EnsureFriendsWatchlist(ensureCtx); err != nil && a.log != nil {
			a.log.Warn("watchlist friends collection startup check failed", "err", err)
		}
		cancel()
	}
	a.discoveryPersonal = personalize.NewDiscovery(a.db, a.catalog, a.state, a.safety.Allowed)
	a.discoveryPersonal.NotInterested = a.notInterestedSignals
	a.discoveryPersonal.AllowedFacts = a.safety.TitleFacts
	a.overlays = overlays.New(a.db, overlays.Options{CacheDir: filepath.Join(a.opts.DataDir, "cache", "overlays"), Images: a.images, Allowed: a.safety.Allowed, Ratings: a.ratings, FactsFor: a.overlayFacts, PreparePage: a.prepareOverlayPage, Variants: a.artworkVariants, PluginBadges: a.overlayPluginBadgeSets, Taste: func(ctx context.Context, u *core.User) []string {
		p, err := a.personalize.Taste(ctx, u)
		if err != nil {
			return nil
		}
		var tags []string
		for _, g := range p.Genres {
			if g.Weight > 0 {
				tags = append(tags, strings.ToLower(g.Name))
			}
		}
		for _, person := range p.People {
			if person.Weight > 0 {
				tags = append(tags, strings.ToLower(person.Name))
			}
		}
		for _, kw := range p.Keywords {
			if kw.Weight > 0 {
				tags = append(tags, strings.ToLower(kw.Name))
			}
		}
		return tags
	}})
	a.discoveryFeatures = &discovery.Features{Watchlist: a.watchlist, Ratings: a.ratings, Overlays: a.overlays, Personal: a.discoveryPersonal, LLM: a.llm, Localize: a.localizedMetadata}
	a.lang = langfeat.New(langfeat.Options{
		Dir:        filepath.Join(a.opts.DataDir, "cache", "langfeat"),
		Gates:      a.languageGates,
		Translate:  a.languageChat,
		Transcribe: a.languageTranscribe,
		Subtitles:  a.subtitles,
		Audio:      a.languageAudio,
		SourceText: a.languageSourceText,
	})
	a.discoveryFeatures.OverviewTranslate = a.lang.Overview
}

func (a *App) publishWatchlistChange(ctx context.Context, userID int64, key core.RatingKey) {
	if a.sessionEvents == nil {
		return
	}
	event := frontend.Event{Type: "LibraryChanged", ViewerID: userID, Item: key}
	if userID > 0 {
		if viewer, err := a.users.User(ctx, userID); err == nil && viewer != nil {
			event.RowIDs = a.watchlist.HomeHubIDs(ctx, viewer)
		}
	}
	if err := a.sessionEvents.Publish(ctx, event); err != nil && a.log != nil {
		a.log.Warn("watchlist change publication failed")
	}
}

type overlayPageFactsKey struct {
	app    *App
	userID int64
}

type overlayShowStatus struct {
	newEpisode bool
	newSeason  bool
}

type overlayPageFacts struct {
	showChecked    map[core.RatingKey]bool
	showStatus     map[core.RatingKey]overlayShowStatus
	qualityChecked map[core.RatingKey]bool
	qualityInfo    map[core.RatingKey]core.MediaInfo
}

func (a *App) prepareOverlayPage(ctx context.Context, u *core.User, titles []core.Title, status, quality bool) context.Context {
	if !reqmemo.Active(ctx) {
		ctx = reqmemo.With(ctx)
	}
	if u == nil || len(titles) == 0 {
		return ctx
	}
	key := overlayPageFactsKey{app: a, userID: u.ID}
	prev, _ := ctx.Value(key).(overlayPageFacts)
	next := overlayPageFacts{
		showChecked:    make(map[core.RatingKey]bool, len(prev.showChecked)),
		showStatus:     make(map[core.RatingKey]overlayShowStatus, len(prev.showStatus)),
		qualityChecked: make(map[core.RatingKey]bool, len(prev.qualityChecked)),
		qualityInfo:    make(map[core.RatingKey]core.MediaInfo, len(prev.qualityInfo)),
	}
	for k, v := range prev.showChecked {
		next.showChecked[k] = v
	}
	for k, v := range prev.showStatus {
		next.showStatus[k] = v
	}
	for k, v := range prev.qualityChecked {
		next.qualityChecked[k] = v
	}
	for k, v := range prev.qualityInfo {
		next.qualityInfo[k] = v
	}

	if status && a.catalog != nil && a.state != nil {
		var shows []core.RatingKey
		seenShow := make(map[core.RatingKey]bool)
		for i := range titles {
			if titles[i].Type == core.TypeShow && titles[i].Key > 0 && !next.showChecked[titles[i].Key] && !seenShow[titles[i].Key] {
				seenShow[titles[i].Key] = true
				shows = append(shows, titles[i].Key)
			}
		}
		if len(shows) > 0 {
			epByShow, err := a.catalog.RecentEpisodesMany(ctx, shows, time.Now().Add(-30*24*time.Hour))
			if err == nil {
				var epKeys []core.RatingKey
				seenEp := make(map[core.RatingKey]bool)
				for _, eps := range epByShow {
					for _, ep := range eps {
						if ep.Key > 0 && !seenEp[ep.Key] {
							seenEp[ep.Key] = true
							epKeys = append(epKeys, ep.Key)
						}
					}
				}
				var watched map[core.RatingKey]core.WatchStatus
				if len(epKeys) > 0 {
					watched, err = a.state.GetMany(ctx, u.ID, epKeys)
				}
				if err == nil {
					for _, sk := range shows {
						next.showChecked[sk] = true
						var st overlayShowStatus
						for _, ep := range epByShow[sk] {
							if watched[ep.Key].ViewCount == 0 {
								st.newEpisode = true
								if ep.Episode == 1 {
									st.newSeason = true
								}
							}
						}
						if st.newEpisode || st.newSeason {
							next.showStatus[sk] = st
						}
					}
				}
			}
		}
	}

	if quality {
		var qKeys []core.RatingKey
		seenQ := make(map[core.RatingKey]bool)
		for i := range titles {
			k := titles[i].Key
			if k > 0 && !next.qualityChecked[k] && !seenQ[k] {
				seenQ[k] = true
				qKeys = append(qKeys, k)
			}
		}
		if len(qKeys) > 0 {
			if a.playback != nil {
				if obs, err := a.playback.ObservedMediaInfoMany(ctx, u.ID, qKeys); err == nil {
					for _, k := range qKeys {
						next.qualityChecked[k] = true
						if o := obs[k]; o != nil {
							next.qualityInfo[k] = o.Info
						}
					}
				}
			} else if a.brokerPlayback != nil {
				wanted := make(map[core.RatingKey]bool, len(qKeys))
				for _, k := range qKeys {
					wanted[k] = true
					next.qualityChecked[k] = true
				}
				for _, session := range a.brokerPlayback.Sessions() {
					if session != nil && session.UserID == u.ID && wanted[session.Key] && !session.Info.Estimated {
						if _, exists := next.qualityInfo[session.Key]; !exists {
							next.qualityInfo[session.Key] = session.Info
						}
					}
				}
			}
		}
	}

	return context.WithValue(ctx, key, next)
}

func (a *App) overlayFacts(ctx context.Context, u *core.User, t *core.Title, status, quality bool) overlays.Facts {
	f := overlays.Facts{}
	var (
		pf    overlayPageFacts
		hasPF bool
	)
	if u != nil {
		pf, hasPF = ctx.Value(overlayPageFactsKey{app: a, userID: u.ID}).(overlayPageFacts)
	}
	if status {
		if expiry, err := a.collections.LastChance(ctx, u, t.Key); err == nil {
			f.LastChance = expiry
		}
	}
	if status && t.Type == core.TypeShow && u != nil {
		if hasPF && pf.showChecked[t.Key] {
			st := pf.showStatus[t.Key]
			f.NewEpisode = st.newEpisode
			f.NewSeason = st.newSeason
		} else {
			episodes, err := a.catalog.RecentEpisodes(ctx, t.Key, time.Now().Add(-30*24*time.Hour))
			if err == nil {
				keys := make([]core.RatingKey, len(episodes))
				for i, ep := range episodes {
					keys[i] = ep.Key
				}
				if watched, err := a.state.GetMany(ctx, u.ID, keys); err == nil {
					for _, ep := range episodes {
						if watched[ep.Key].ViewCount == 0 {
							f.NewEpisode = true
							if ep.Episode == 1 {
								f.NewSeason = true
							}
						}
					}
				}
			}
		}
	}
	// Only exact facts belonging to the selected viewer can decorate their
	// poster. Historical probes survive a restart and are bound to the current
	// resume release by the playback package; no resolving/probing happens here.
	if quality && u != nil {
		if hasPF && pf.qualityChecked[t.Key] {
			if info, ok := pf.qualityInfo[t.Key]; ok {
				applyOverlayQuality(&f, info)
			}
		} else if a.playback != nil {
			if observed, err := a.playback.ObservedMediaInfo(ctx, u.ID, t.Key); err == nil && observed != nil {
				applyOverlayQuality(&f, observed.Info)
			}
		} else if a.brokerPlayback != nil {
			// Remote preparation snapshots contain exact owner-probed metadata for an
			// immutable pinned revision. Do not resolve/probe/network during artwork.
			for _, session := range a.brokerPlayback.Sessions() {
				if session != nil && session.UserID == u.ID && session.Key == t.Key && !session.Info.Estimated {
					applyOverlayQuality(&f, session.Info)
					break
				}
			}
		}
	}
	return f
}
func (a *App) discoveryHandlers(m map[string]http.Handler) {
	base := a.watchlist.Router()
	m["discovery"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/translate" {
			a.translateOverview(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
	m["discovery/ratings"] = a.ratings.Router(a.catalog)
	m["discovery/overlays"] = a.overlays.Router(a.catalog, func(r *http.Request, id int64) (*core.User, error) {
		u, err := a.users.User(r.Context(), id)
		if err != nil {
			return nil, err
		}
		return a.safety.UserPolicy(u), nil
	})
}

func (a *App) discoveryToken(ctx context.Context, id int64) (string, error) {
	u, err := a.users.User(ctx, id)
	if err != nil {
		return "", err
	}
	return discoveryViewerToken(ctx, u, a.users.HomeMember(id), a.cfg.Read().Plex.Token, a.plextv.HomeUserToken)
}

func discoveryViewerToken(ctx context.Context, u *core.User, home bool, ownerToken string, switchHome func(context.Context, string, int64, string) (string, error)) (string, error) {
	if u.IsOwner {
		return ownerToken, nil
	}
	if home && u.PlexID > 0 {
		return switchHome(ctx, ownerToken, u.PlexID, "")
	}
	return "", core.ErrUnauthorized
}

func (a *App) localizedMetadata(ctx context.Context, t *core.Title, lang string) (*core.Title, error) {
	if !a.catalog.TMDBConfigured() || !hasTMDBArtwork(t) {
		return t, nil
	}
	return a.tmdb.Localized(ctx, t, lang)
}

func hasTMDBArtwork(t *core.Title) bool {
	if t == nil {
		return false
	}
	switch t.Type {
	case core.TypeMovie, core.TypeShow:
		return t.IDs.TMDB > 0
	case core.TypeSeason, core.TypeEpisode:
		if t.EpisodeOrder.Source != "" && (t.EpisodeOrder.Source != core.MetadataTMDB || t.EpisodeOrder.Kind != core.OrderAired) {
			return false
		}
		return !t.Key.Allocated() && t.ShowIDs.TMDB > 0 && t.Season == t.Key.Season() && t.Episode == t.Key.Episode()
	}
	return false
}

func (a *App) artworkVariants(ctx context.Context, t *core.Title, lang string) (overlays.Artwork, error) {
	var images *core.Artwork
	switch {
	case a.catalog.TMDBConfigured() && hasTMDBArtwork(t):
		var err error
		images, err = a.tmdb.ArtworkVariants(ctx, t, lang)
		if err != nil {
			return overlays.Artwork{}, err
		}
	case t != nil && t.IDs.TVDB > 0:
		source := a.catalog.MetadataSources().Get(core.MetadataTVDB)
		if source == nil || !source.Configured() || !source.Capabilities().Images {
			return overlays.Artwork{}, nil
		}
		var err error
		got, err := source.Images(ctx, core.ProviderRef{Source: source.ID(), ID: strconv.FormatInt(t.IDs.TVDB, 10), Type: t.Type})
		if errors.Is(err, core.ErrUnsupported) || errors.Is(err, core.ErrNotFound) {
			return overlays.Artwork{}, nil
		}
		if err != nil {
			return overlays.Artwork{}, err
		}
		images = &got
	default:
		return overlays.Artwork{}, nil
	}
	out := overlays.Artwork{}
	for _, img := range images.Posters {
		out.Posters = append(out.Posters, overlays.Variant{Path: img.FilePath, Language: img.ISO639})
	}
	for _, img := range images.Backdrops {
		out.Backdrops = append(out.Backdrops, overlays.Variant{Path: img.FilePath, Language: img.ISO639})
	}
	return out, nil
}

// Keep one release's facts together: never combine 4K from one stream/release
// with Dolby Vision or Atmos from an unrelated one.
func applyOverlayQuality(f *overlays.Facts, info core.MediaInfo) {
	if info.Estimated {
		return
	}
	video := info.Video()
	if video == nil || video.Height <= 0 {
		return
	}
	f.Width = video.Width
	f.Height = video.Height
	f.HDR = video.ColorTransfer == "smpte2084" || video.ColorTransfer == "arib-std-b67"
	f.DolbyVision = video.DolbyVision
	f.Atmos = false
	for _, stream := range info.Streams {
		if stream.Type == "audio" && (strings.EqualFold(stream.Codec, "truehd") || strings.EqualFold(stream.Codec, "eac3")) {
			for _, word := range strings.FieldsFunc(strings.ToLower(stream.Profile), func(r rune) bool { return !unicode.IsLetter(r) }) {
				if word == "atmos" {
					f.Atmos = true
				}
			}
		}
	}
}

func (a *App) overlayPluginBadgeSets(ctx context.Context) []overlays.PluginBadgeSet {
	if a == nil || a.pluginsRT == nil || a.pluginsRT.mgr == nil {
		return nil
	}
	active := a.pluginsRT.mgr.ActivePosterBadgeSets(ctx)
	if len(active) == 0 {
		return nil
	}
	out := make([]overlays.PluginBadgeSet, len(active))
	for i, ps := range active {
		out[i] = overlays.PluginBadgeSet{
			PluginID:    ps.PluginID,
			PluginName:  ps.PluginName,
			Key:         ps.Key,
			LayerKey:    ps.LayerKey,
			Label:       ps.Label,
			Description: ps.Description,
		}
	}
	return out
}
