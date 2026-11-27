package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/availability"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/favorites"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reqmemo"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/plugins/adapters"
	"github.com/braedonsaunders/ether/internal/resolver"
	sessionbroker "github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

type experienceSituationKey struct{}

func situationFor(req frontend.RowRequest) core.HomeSituation {
	h := core.NewHubContext(time.Now().Local(), req.Device.Product, req.Device.Platform, req.Device.DeviceName)
	h.ClientID = req.Device.ID
	h.SectionID, _ = strconv.Atoi(req.ViewID)
	return core.HomeSituation{Device: h.Device, Daypart: h.Daypart, Weekend: h.Weekend, Now: h.Now, LibraryID: h.SectionID, Product: h.Product, Platform: h.Platform, ClientID: h.ClientID, SessionID: h.SessionID}
}

func (a *App) scheduledEpisodes(ctx context.Context, key core.RatingKey) ([]core.Title, error) {
	if a.catalog == nil {
		return nil, core.ErrNotFound
	}
	return a.catalog.ScheduledEpisodes(ctx, key)
}

func (a *App) groupedShows(ctx context.Context, viewer *core.User, shows []core.RatingKey) (map[core.RatingKey]bool, error) {
	if a.episodeorder == nil {
		return map[core.RatingKey]bool{}, nil
	}
	return a.episodeorder.Grouped(ctx, viewer, shows)
}

func (a *App) experienceSearch(vis experience.Visibility, stepAside *experience.StepAside) experience.Search {
	unrestricted := func(viewer *core.User) bool {
		if stepAside != nil && stepAside.Active() {
			return false
		}
		return a.safety != nil && viewer != nil && !a.safety.Restricted(viewer)
	}
	return experience.SearchFor(a.searchSourcesFor(stepAside), vis, unrestricted)
}

func (a *App) searchSourcesFor(stepAside *experience.StepAside) experience.SearchSources {
	src := experience.SearchSources{
		Catalog:      a.catalog.Search,
		PeopleTitles: a.catalog.PeopleTitles,
		Details:      a.catalog.SearchDetails,
	}
	if a.availability != nil && !a.cfg.Read().Spike.Enabled {
		availCat := &availabilityCatalog{Catalog: a.catalog, availability: a.availability}
		src.Catalog = availCat.Search
		src.PeopleTitles = availCat.PeopleTitles
	}
	if stepAside != nil {
		baseCatalog := src.Catalog
		src.Catalog = func(ctx context.Context, q string, types []core.MediaType, n int) ([]core.Title, error) {
			ts, err := baseCatalog(ctx, q, types, n)
			if err != nil {
				return nil, err
			}
			return stepAside.FilterAvailable(ctx, ts), nil
		}
		basePeopleTitles := src.PeopleTitles
		if basePeopleTitles != nil {
			src.PeopleTitles = func(ctx context.Context, viewer *core.User, q string, n int) ([]core.Title, error) {
				ts, err := basePeopleTitles(ctx, viewer, q, n)
				if err != nil {
					return nil, err
				}
				return stepAside.FilterAvailable(core.WithCatalogViewer(ctx, viewer), ts), nil
			}
		}
	}
	if a.state != nil {
		src.Familiar = experience.FamiliarFrom(a.state)
	}
	if a.collections != nil {
		src.Vibe = a.collections
		if stepAside != nil {
			src.Vibe = stepAsideVibe{VibeSearcher: a.collections, stepAside: stepAside}
		}
	}
	return src
}

// peopleIndex adapts the catalogue's people-name index to experience.
func (a *App) peopleIndex(ctx context.Context, terms []string, limit int) ([]experience.PersonCandidate, error) {
	found, err := a.catalog.MatchPeople(ctx, terms, limit)
	if err != nil {
		return nil, err
	}
	out := make([]experience.PersonCandidate, len(found))
	for i, m := range found {
		out[i] = experience.PersonCandidate{TMDB: m.TMDB, Name: m.Name}
	}
	return out, nil
}

// libraryName is the live name of library id from the libraries
// service, or fallback when the service is not wired (hand-built Apps
// in tests) or the row cannot be read.
func (a *App) libraryName(id core.LibraryID, fallback string) string {
	if a.libraries != nil {
		if lib, err := a.libraries.Get(context.Background(), id); err == nil {
			if n := strings.TrimSpace(lib.Name); n != "" {
				return n
			}
		}
	}
	return fallback
}

// libraryTitles is the live Movies / TV Shows name pair.
func (a *App) libraryTitles() (movies, shows string) {
	return a.libraryName(core.LibraryMovies, "Movies"), a.libraryName(core.LibraryShows, "TV Shows")
}

// configuredLibraries reads current catalogue declarations. An empty stored
// list remains empty; only hosts without a library store use legacy defaults.
func (a *App) configuredLibraries() []experience.Library {
	if a.libraries == nil {
		movies, shows := a.libraryTitles()
		return []experience.Library{{ID: 1, Name: movies, Type: core.TypeMovie, Virtual: true}, {ID: 2, Name: shows, Type: core.TypeShow, Virtual: true}}
	}
	out := []experience.Library{}
	libs, err := a.libraries.List(context.Background())
	if err != nil {
		return out
	}
	for _, lib := range libs {
		if lib.Hidden || lib.Source == core.LibraryPlugin {
			continue
		}
		out = append(out, experience.Library{ID: int(lib.ID), Name: lib.Name, Type: lib.MediaType, Virtual: true})
	}
	return out
}

// newSurfaceExperience builds an explicitly surface-bound service. A shared
// situation is inserted by surfaceExperience.Rows before ranking and receipt issuance.
func (a *App) newSurfaceExperience(protocol core.Protocol, artwork ...frontend.Artwork) (frontend.Experience, error) {
	policy := frontendHostPolicy(protocol)
	if a.spikeLibrary != nil && policy.SpikeExperience {
		return a.spikeLibrary.Experience(), nil
	}
	if a.users == nil || a.catalog == nil || a.safety == nil || a.state == nil || a.personalize == nil || a.favorites == nil || a.playlists == nil || a.db == nil {
		return nil, errors.New("experience: dependencies unavailable")
	}
	liveLibraries := func() []experience.Library {
		libs := a.configuredLibraries()
		if a.libraries == nil {
			return libs
		}
		out := []experience.Library{}
		for _, lib := range libs {
			stored, err := a.libraries.Get(context.Background(), core.LibraryID(lib.ID))
			if err != nil {
				continue
			}
			if _, _, published := presentLibraryForProtocol(stored.Present, protocol, stored.MediaType); published {
				out = append(out, lib)
			}
		}
		return out
	}
	libs := liveLibraries()
	stepAside := a.newStepAsideFor(protocol)
	filter := func(ctx context.Context, ts []core.Title) []core.Title {
		if a.availability == nil {
			return ts
		}
		return a.availability.FilterDiscovery(ctx, ts)
	}
	if stepAside != nil {
		filter = stepAside.WrapAvailable(filter)
	}
	sessions := func() []*core.PlaySession {
		if a.playback == nil {
			return nil
		}
		return a.playback.Sessions()
	}
	// Home, library rows, and card user-data read the mirror. Refreshing a
	// show from a provider stays on the single-title detail and playback path.
	vis := experienceVisibility(a.safety, a.catalog.CachedGet, filter, sessions, libs)
	vis.SectionFor = func(kind core.MediaType) (int, bool) {
		return experience.LibrarySection(liveLibraries(), kind)
	}
	vis.JudgedOnFacts = func(ctx context.Context) func(core.Title) bool {
		if a.availability == nil {
			return nil
		}
		return a.availability.JudgedOnFacts(ctx)
	}
	rowVis := vis
	source := experience.CandidateRowSource{Kind: a.personalize.SemanticRowKind, Load: func(ctx context.Context, req frontend.RowRequest) ([]experience.RankedCandidate, error) {
		return a.productionExperienceRows(ctx, req, liveLibraries(), rowVis, stepAside)
	}, Additional: func(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
		rows, err := a.experienceAdditionalRows(core.WithLiveApp(ctx, string(protocol)), req, a.playlists, rowVis)
		if err != nil || req.Placement != frontend.PlacementHome || req.Purpose != "" && req.Purpose != frontend.RowCollection {
			return rows, err
		}
		switch protocol {
		case "plex", "jellyfin", "emby", "silo", "vela":
			sit, ok := ctx.Value(experienceSituationKey{}).(core.HomeSituation)
			if ok {
				hubs := a.personalize.CollectionCardHubs(ctx, req.Viewer, candidateHubContext(sit), req.Limit)
				cards, _ := rankedCandidates(hubs, nil)
				rows = append(rows, experience.CandidateRows(cards)...)
			}
		}
		return rows, nil
	}}
	var cols core.Collections
	if a.collections != nil {
		cols = a.collections
	}
	var playlistExistsBatch func(context.Context, int64, []core.RatingKey) (map[core.RatingKey]bool, error)
	if batch, ok := a.playlists.(interface {
		ExistsBatch(context.Context, int64, []core.RatingKey) (map[core.RatingKey]bool, error)
	}); ok {
		playlistExistsBatch = batch.ExistsBatch
	}
	var channelPermissions func(context.Context, *core.User, []string) (map[string]bool, error)
	if a.livetv != nil {
		channelPermissions = a.livetv.ChannelPermissions
	}
	entryPolicy := experience.RowEntryPolicy{Visibility: rowVis, Collections: cols, ChannelPermissions: channelPermissions, PlaylistExistsBatch: playlistExistsBatch, Favorite: func(ctx context.Context, u *core.User, subject experience.FavoriteSubject) bool {
		return a.favoriteAllowedStored(ctx, u, favorites.Subject{Kind: favorites.Kind(subject.Kind), ID: subject.ID}, rowVis)
	}, Playlist: func(ctx context.Context, u *core.User, key core.RatingKey) bool {
		if ex, ok := a.playlists.(interface {
			Exists(context.Context, int64, core.RatingKey) (bool, error)
		}); ok {
			ok, err := ex.Exists(ctx, u.ID, key)
			return err == nil && ok
		}
		if hdr, ok := a.playlists.(interface {
			GetHeader(context.Context, int64, core.RatingKey) (*core.Playlist, error)
		}); ok {
			_, err := hdr.GetHeader(ctx, u.ID, key)
			return err == nil
		}
		_, err := a.playlists.Get(ctx, u.ID, key)
		return err == nil
	}}
	interactions := experience.RowInteractionPolicy{Libraries: libs, LiveLibraries: liveLibraries, Visibility: rowVis, Get: a.catalog.CachedGet, AllowEntry: entryPolicy.Subject, Collections: cols, ChannelPermissions: channelPermissions, PlaylistExistsBatch: playlistExistsBatch, CollectionMembers: func(ctx context.Context, u *core.User, key core.RatingKey, entries []core.NeutralEntryRef) bool {
		return rowViewCollectionMembers(ctx, a.collections, u, key, entries)
	}, Impression: a.personalize.RecordNeutralImpression, EntryImpression: a.personalize.RecordNeutralEntryImpression, DetailOpen: a.personalize.RecordDetailOpen}
	receipts, err := experience.NewReceipts(experience.ReceiptOptions{DB: a.db, PolicyLease: a.cfg.AcquirePolicy, Active: a.users.PolicyUser, TTL: 30 * time.Minute, MaxPerDevice: 10000, Prefetch: func(ctx context.Context, keys []core.RatingKey) {
		_, _ = a.catalog.CachedGetMany(ctx, keys)
	}, PrepareBatch: interactions.PrepareBatch, Allowed: func(ctx context.Context, u *core.User, _ core.Device, _ core.Protocol, subject experience.ReceiptSubject) bool {
		return interactions.Allowed(ctx, u, subject)
	}, AllowedAction: func(_ context.Context, _ *core.User, _ core.Device, _ core.Protocol, subject experience.ReceiptSubject, kind frontend.RowInteractionKind) bool {
		return interactions.AllowedAction(subject, kind)
	}, Record: func(ctx context.Context, u *core.User, _ core.Device, _ core.Protocol, subject experience.ReceiptSubject, kind frontend.RowInteractionKind) error {
		return interactions.Record(ctx, u, subject, kind)
	}})
	if err != nil {
		return nil, err
	}
	// watchedTitles is set once the experience service below exists; row
	// layout runs only on requests, after construction.
	var watchedTitles func(context.Context, *core.User, []core.Title) (map[core.RatingKey]bool, error)
	rows, err := experience.NewRowsService(experience.RowServiceDeps{Source: source, Visibility: rowVis, Receipts: receipts, PrefetchTitles: a.catalog.PreloadStored, PrepareRows: entryPolicy.PrepareRows, RankedSource: true, HomeFallback: func(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
		if protocol == "plex" {
			return nil, nil
		}
		return a.experienceHomeFallback(ctx, req, liveLibraries(), rowVis)
	}, Active: func(ctx context.Context, u *core.User) bool {
		v, e := a.users.ActiveUser(ctx, u.ID)
		return e == nil && v != nil
	}, AllowRow: entryPolicy.Row, AllowEntry: entryPolicy.Entry, Layout: func(ctx context.Context, req frontend.RowRequest, rows []frontend.Row) ([]frontend.Row, error) {
		sit, ok := ctx.Value(experienceSituationKey{}).(core.HomeSituation)
		if !ok {
			return nil, errors.New("experience: missing situation")
		}
		var layout core.NeutralHomeLayoutSource = a.personalize
		if req.HomeFallbackOnly {
			layout = neutralLayoutFunc(a.personalize.FinalizeNeutralHomeFill)
		}
		extrasEnabled := a.extras != nil && a.extras.Enabled()
		rows = append([]frontend.Row(nil), rows...)
		for n := range rows {
			if extrasEnabled && (rows[n].Style == "hero" || rows[n].Style == "featured") {
				rows[n].PreviewOnFocus = true
			}
		}
		arranged, err := experience.LayoutRows(ctx, layout, liveLibraries(), req, sit, rows)
		if err == nil {
			arranged = experience.DropWatched(ctx, req.Viewer, arranged, watchedTitles)
		}
		if !extrasEnabled {
			for n := range arranged {
				arranged[n].PreviewOnFocus = false
				arranged[n].Layout.PreviewOnFocus = false
			}
		}
		return arranged, err
	}, Situation: func(ctx context.Context, req frontend.RowRequest) (core.HomeSituation, error) {
		v, ok := ctx.Value(experienceSituationKey{}).(core.HomeSituation)
		if !ok {
			return core.HomeSituation{}, errors.New("experience: missing situation")
		}
		return v, nil
	}})
	if err != nil {
		return nil, err
	}
	rowViews, err := a.newRowViews(protocol, rows, receipts, vis, libs, liveLibraries)
	if err != nil {
		return nil, err
	}
	airedEpisodeTallies := a.catalog.AiredEpisodeTallies
	if stepAside != nil {
		airedEpisodeTallies = func(ctx context.Context, userID int64, shows []core.RatingKey, settled time.Time, judge []core.RatingKey) (map[core.RatingKey]core.AiredEpisodeTally, error) {
			heldEps := stepAside.HeldEpisodesForShows(ctx, shows)
			if len(heldEps) == 0 {
				return a.catalog.AiredEpisodeTallies(ctx, userID, shows, settled, judge)
			}
			if len(judge)+len(heldEps) <= 400 {
				combined := append(append([]core.RatingKey(nil), judge...), heldEps...)
				return a.catalog.AiredEpisodeTallies(ctx, userID, shows, settled, combined)
			}
			out := make(map[core.RatingKey]core.AiredEpisodeTally, len(shows))
			for _, show := range shows {
				if _, dup := out[show]; dup {
					continue
				}
				showHeld := stepAside.HeldEpisodesForShows(ctx, []core.RatingKey{show})
				combined := append(append([]core.RatingKey(nil), judge...), showHeld...)
				part, err := a.catalog.AiredEpisodeTallies(ctx, userID, []core.RatingKey{show}, settled, combined)
				if err != nil {
					return nil, err
				}
				for k, v := range part {
					out[k] = v
				}
			}
			return out, nil
		}
	}
	episodesUnranked := a.catalog.EpisodesUnranked
	if stepAside != nil {
		episodesUnranked = func(ctx context.Context) (bool, error) {
			if stepAside.HasAnyEpisodes() {
				return false, nil
			}
			return a.catalog.EpisodesUnranked(ctx)
		}
	}
	countUnplayedAired := a.catalog.CountUnplayedAired
	if stepAside != nil {
		countUnplayedAired = func(ctx context.Context, userID int64, keys []core.RatingKey, maxRating string) (map[core.RatingKey]int, error) {
			counts, err := a.catalog.CountUnplayedAired(ctx, userID, keys, maxRating)
			if err != nil {
				return nil, err
			}
			return stepAside.AdjustUnplayedCounts(ctx, userID, keys, counts)
		}
	}
	deps := experience.ServiceDeps{BuiltinNative: func(mt core.MediaType) string { return builtinNative(mt, protocol) }, LibraryPresentation: func(p core.Presentations, mt core.MediaType) (core.MediaType, string, bool) {
		return presentLibraryForProtocol(p, protocol, mt)
	}, BeginPeopleRead: a.beginPeopleRead, NotInterestedSource: func(ctx context.Context, u *core.User) ([]frontend.NotInterestedEntry, error) {
		entries, _, err := a.notInterested.Snapshot(ctx, u)
		if err != nil {
			return nil, err
		}
		out := make([]frontend.NotInterestedEntry, 0, len(entries))
		for _, entry := range entries {
			out = append(out, frontend.NotInterestedEntry{Key: entry.Key, AddedAt: entry.AddedAt})
		}
		return out, nil
	}, ByExternalID: a.catalog.ByExternalID, ResolveEpisode: a.catalog.ResolveEpisode, KeysByProviderID: a.catalog.KeysByProviderID, Active: a.users.PolicyUser, Libraries: libs, LiveLibraries: liveLibraries, Visibility: vis, State: a.state, Rows: rows, Protocol: protocol, Browse: a.catalog.Browse, PageExclusions: newLibraryPageExclusionsFor(a, stepAside), ViewerRules: a.catalog.ViewerRulesJudge, FacetFilter: a.catalog.FacetFilter, FacetPage: a.catalog.FacetPage, StudioByFacetID: a.catalog.StudioByFacetID, PersonByFacetID: a.catalog.PersonByFacetID, CountWatched: a.catalog.CountWatched, WatchedTitles: a.catalog.WatchedTitles, CountBrowsable: a.catalog.CountBrowsable, CreditItems: a.catalog.CachedCreditItems, PeopleIndex: a.peopleIndex, PersonCredits: a.catalog.PersonCreditKeys, PersonCreditsMany: a.catalog.PersonCreditsMany, RankingDetails: a.catalog.RankingDetails, PersonMetadata: a.catalog.PersonMetadata, LibraryMembers: a.libraryRecommendationMembers, PersonCreditForms: a.catalog.PersonCreditForms, PersonFormKeys: a.catalog.PersonFormKeys, AiredEpisodes: a.catalog.CachedAiredEpisodes, AiredEpisodesMany: a.catalog.CachedAiredEpisodesMany, AiredEpisodeTallies: airedEpisodeTallies, EpisodesSettledBefore: func(ctx context.Context) time.Time {
		if a.availability == nil {
			return time.Time{}
		}
		return a.availability.EpisodesSettledBefore(ctx)
	}, StoredEpisodes: experience.StoredEpisodeDeps{Page: a.catalog.StoredEpisodesByKey, Count: a.catalog.CountStoredEpisodes, Unranked: episodesUnranked, Customized: func(ctx context.Context, u *core.User) (bool, error) {
		if a.episodeorder == nil {
			return false, nil
		}
		return a.episodeorder.Customized(ctx, u)
	}}, AddedTimes: a.catalog.CachedAddedTimes, PlayedEpisodes: experience.PlayedEpisodeDeps{Page: a.catalog.PlayedEpisodesByLastViewed, Count: a.catalog.CountPlayedEpisodes}, FavoriteEpisodes: experience.FavoriteEpisodeDeps{Page: a.catalog.FavoriteEpisodesByKey, Count: a.catalog.CountFavoriteEpisodes}, RecentAir: func(ctx context.Context, typ core.MediaType, offset, limit int) ([]core.Title, bool, error) {
		f := core.Filter{Type: typ}
		if a.availability != nil {
			var err error
			f, err = a.availability.PagingFilter(ctx, f)
			if err != nil {
				return nil, false, err
			}
		}
		return a.catalog.RecentAirFiltered(ctx, typ, f, offset, limit)
	}, CountUnplayedAired: countUnplayedAired, SimilarSource: a.catalog.Similar, FilterRecommendations: a.personalize.FilterRecommendations, Collections: a.collections, Franchises: a.catalog, CollectionMemberCounts: a.catalog.CollectionMemberCounts, Playlists: a.playlists, Scheduled: a.scheduledEpisodes, Grouped: a.groupedShows, Search: a.experienceSearch(vis, stepAside), CachedGet: a.catalog.CachedGet, CachedGetMany: a.catalog.CachedGetMany, ArtworkGet: a.catalog.ArtworkTitle, ArtworkGetMany: a.catalog.ArtworkTitles, ArtworkRows: a.catalog.ArtworkRows, VisibilityRows: a.catalog.CachedVisibilityRows, ParentTitles: func(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		return a.catalog.ResolveShowParents(ctx, keys)
	}, Get: func(ctx context.Context, u *core.User, k core.RatingKey) (*core.Title, error) {
		if a.episodeorder != nil {
			return a.episodeorder.Get(ctx, u, k)
		}
		return a.catalog.Get(ctx, k)
	}, CachedVirtualSeason: func(ctx context.Context, u *core.User, k core.RatingKey) (*core.Title, error) {
		if a.episodeorder == nil {
			return nil, core.ErrNotFound
		}
		return a.episodeorder.CachedVirtualSeason(ctx, u, k)
	}, Children: func(ctx context.Context, u *core.User, k core.RatingKey) ([]core.Title, error) {
		if a.episodeorder != nil {
			return a.episodeorder.Children(ctx, u, k)
		}
		return a.catalog.Children(ctx, k)
	}, Grandchildren: func(ctx context.Context, u *core.User, k core.RatingKey) ([]core.Title, error) {
		if a.episodeorder != nil {
			return a.episodeorder.Grandchildren(ctx, u, k)
		}
		return a.catalog.Grandchildren(ctx, k)
	}, SeasonEpisodes: func(ctx context.Context, u *core.User, show core.RatingKey, season, offset, limit int) (core.Page, bool, error) {
		if stepAside != nil && stepAside.HasShowEpisodes(core.WithCatalogViewer(ctx, u), show) {
			return core.Page{}, false, nil
		}
		if a.episodeorder != nil {
			pref, err := a.episodeorder.Preference(ctx, u.ID, show)
			if err != nil {
				return core.Page{}, false, err
			}
			if pref.GroupID != "" {
				return core.Page{}, false, nil
			}
			if season == 0 && pref.Specials == "hidden" {
				return core.Page{Items: []core.Title{}}, true, nil
			}
		}
		page, err := a.catalog.SeasonEpisodePage(ctx, show, season, offset, limit)
		return page, true, err
	}, Enrich: func(ctx context.Context, u *core.User, device core.Device, title *core.Title) (frontend.ItemDetail, error) {
		result := frontend.ItemDetail{Title: *title}
		if title.PosterPath != "" {
			result.Images = append(result.Images, frontend.Image{Kind: "poster", Path: title.PosterPath})
		}
		if title.BackdropPath != "" {
			result.Images = append(result.Images, frontend.Image{Kind: "backdrop", Path: title.BackdropPath})
		}
		var overrides []core.ClientCapabilityOverride
		if a.cfg != nil {
			overrides = a.cfg.Read().Clients.Overrides
		}
		// surfaceExperience.Item replaces this list with the shared artwork
		// selection. That selection is the composited poster when overlays are on.
		var (
			resumedInfo     core.MediaInfo
			resumedRel      core.Release
			resumedSelector string
			hasResumed      bool
			markerEstimated = true
		)
		if a.playback != nil {
			caps := core.ClientCaps{PreserveEmptyVideoCodecs: policy.NativePlaybackCapabilities, Protocol: protocol, Product: device.Product, Platform: device.Platform, DeviceName: device.DeviceName, ClientID: device.ID, Version: device.Version}
			if !policy.NativePlaybackCapabilities {
				caps = playbackFloor(caps, overrides)
			}
			if a.cfg != nil {
				caps = resolver.ApplyClientRulesForTitle(a.cfg.Read().Quality, caps, u, title, a.membershipFor(ctx, title))
			}
			resumedInfo, resumedRel, resumedSelector, hasResumed = a.playback.ResumedVersion(ctx, u, title.Key, caps)
			info := resumedInfo
			if !hasResumed || !core.IsAutoVersionSelector(resumedSelector) {
				info = a.playback.Estimate(ctx, title, caps)
				if u != nil && device.ID != "" && core.ColdDetail(ctx, title.Key) &&
					(protocol == "jellyfin" || protocol == "emby" || protocol == "vela" || protocol == "silo") {
					warmCaps := caps
					warmCaps.ClientID = sessionbroker.PlaybackClientID(protocol, device.ID)
					if warm, ok := a.playback.WarmProbeInfo(u, warmCaps, title.Key); ok {
						info = core.CloneMediaInfo(warm)
					}
				}
			}
			markerSession := func(userID int64, clientID string, key core.RatingKey) (*core.PlaySession, bool) {
				if surf := string(protocol); sessionSurface(surf) != "" {
					if sess, ok := a.playback.SessionForSurfaceClient(surf, userID, clientID, key); ok && sess != nil {
						return sess, true
					}
				}
				return a.playback.SessionFor(userID, clientID, key)
			}
			markerInfo := experience.MarkerMedia(u, device, title.Key, info, markerSession)
			markerEstimated = markerInfo.Estimated
			if a.markers != nil {
				result.Markers = experience.ItemMarkers(ctx, a.markers, title.Key, markerInfo)
			} else {
				result.Markers = experience.ItemMarkers(ctx, nil, title.Key, markerInfo)
			}
			if a.subtitles != nil {
				info.Streams = append(info.Streams, (subtitleSurface{app: a}).CachedSidecars(ctx, title, u, info.Streams)...)
			}
			result.Tracks = info.Streams
			result.TracksEstimated = info.Estimated
			if !info.Estimated {
				result.Chapters = info.Chapters
			}
		}
		if a.resolver != nil {
			caps := core.ClientCaps{PreserveEmptyVideoCodecs: policy.NativePlaybackCapabilities, Protocol: protocol, Product: device.Product, Platform: device.Platform, DeviceName: device.DeviceName, ClientID: device.ID, Version: device.Version}
			if !policy.NativePlaybackCapabilities {
				caps = playbackFloor(caps, overrides)
			}
			if a.cfg != nil {
				caps = resolver.ApplyClientRulesForTitle(a.cfg.Read().Quality, caps, u, title, a.membershipFor(ctx, title))
			}
			versions, err := scorecardVariants(ctx, a, u, title, caps)
			if err != nil {
				return frontend.ItemDetail{}, err
			}
			for _, version := range versions {
				if hasResumed {
					matched := (resumedRel.ID != "" && (version.Release.ID == resumedRel.ID || version.ID == resumedRel.ID)) ||
						(!core.IsAutoVersionSelector(resumedSelector) && version.ID == resumedSelector) ||
						(version.Auto && core.IsAutoVersionSelector(resumedSelector))
					if matched {
						merged := core.CloneMediaInfo(resumedInfo)
						merged.Estimated = false
						if len(version.Info.Chapters) > 0 && len(merged.Chapters) == 0 {
							merged.Chapters = version.Info.Chapters
						}
						version.Info = merged
						if merged.Size > 0 && version.Release.Size <= 0 {
							version.Release.Size = merged.Size
						}
					}
				}
				result.Versions = append(result.Versions, frontendVersion(title, version))
				if version.Auto && !version.Info.Estimated && len(version.Info.Chapters) > 0 {
					result.Chapters = version.Info.Chapters
				}
			}
			// When Auto already has real probed tracks (e.g. from the durable
			// content-ID probe cache or vault), surface them as the detail's
			// verified tracks instead of falling back to an estimate.
			if result.TracksEstimated && len(versions) > 0 && !versions[0].Info.Estimated && len(versions[0].Info.Streams) > 0 {
				probed := core.CloneMediaInfo(versions[0].Info)
				if a.subtitles != nil {
					probed.Streams = append(probed.Streams, (subtitleSurface{app: a}).CachedSidecars(ctx, title, u, probed.Streams)...)
				}
				result.Tracks = probed.Streams
				result.TracksEstimated = false
			}
			var markerSource experience.MarkerSource
			if a.markers != nil {
				markerSource = a.markers
			}
			for n := range result.Versions {
				result.Versions[n].Markers = experience.ItemMarkers(ctx, markerSource, title.Key, result.Versions[n].Media)
			}
			if markerEstimated && len(result.Markers) == 0 && len(result.Versions) > 0 && !result.Versions[0].Media.Estimated {
				result.Markers = result.Versions[0].Markers
			}
		}
		if a.extras != nil {
			clips, err := a.extras.List(ctx, title, device.Language)
			if err != nil {
				return frontend.ItemDetail{}, err
			}
			for _, clip := range clips {
				result.Extras = append(result.Extras, frontend.Extra{Key: clip.Key, ParentKey: clip.ParentKey, Title: clip.Title, Kind: clip.Subtype, Source: clip.Site, SourceID: clip.SiteKey, Language: clip.Language, Official: clip.Official, PublishedAt: clip.PublishedAt, Duration: clip.Duration, Height: clip.Size})
			}
		}
		return result, nil
	}, Favorites: func(ctx context.Context, u *core.User, k core.RatingKey, on bool) (bool, error) {
		return a.favorites.Set(ctx, u, favorites.Subject{Kind: favorites.KindTitle, ID: k.String()}, on)
	}, IsFavorite: func(ctx context.Context, u *core.User, k core.RatingKey) (bool, error) {
		return a.favorites.Has(ctx, u, favorites.Subject{Kind: favorites.KindTitle, ID: k.String()})
	}, IsFavoriteMany: func(ctx context.Context, u *core.User, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
		return a.favorites.HasTitles(ctx, u, keys)
	}, HistoryPlays: a.historyPlays(), RecordingPreferences: a.recordingPrefs().get, SetRecordingPreference: a.recordingPrefs().set, HiddenRecordingCopies: a.recordingPrefs().hidden, FavoritePage: func(ctx context.Context, u *core.User, role, cursor string, limit int) ([]core.RatingKey, []time.Time, string, int, error) {
		page, err := a.favorites.Page(ctx, u, favorites.PageQuery{Role: role, Cursor: cursor, Limit: limit})
		if errors.Is(err, favorites.ErrBadCursor) {
			return nil, nil, "", 0, frontend.ErrBadFavoritesCursor
		}
		if err != nil {
			return nil, nil, "", 0, err
		}
		keys := make([]core.RatingKey, 0, len(page.Entries))
		at := make([]time.Time, 0, len(page.Entries))
		for _, e := range page.Entries {
			if k, err := core.ParseRatingKey(e.Subject.ID); err == nil {
				keys, at = append(keys, k), append(at, e.AddedAt)
			}
		}
		return keys, at, page.Next, page.Total, nil
	}, FavoriteTitleKeys: func(ctx context.Context, u *core.User, n int) ([]core.RatingKey, bool, error) {
		return a.favorites.TitleKeys(ctx, u, n)
	}, IsNotInterestedMany: func(ctx context.Context, u *core.User, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
		return a.notInterested.HasMany(ctx, u, keys)
	}, IsWatchlistedMany: func(ctx context.Context, userID int64, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
		if a.watchlist == nil {
			return map[core.RatingKey]bool{}, nil
		}
		return a.watchlist.ContainsMany(ctx, userID, keys)
	}, FavoriteMembership: a.favoriteMembership, IsNotInterested: func(ctx context.Context, u *core.User, k core.RatingKey) (bool, error) {
		return a.notInterested.Has(ctx, u, k)
	}, SetNotInterestedSource: func(ctx context.Context, u *core.User, k core.RatingKey, on bool) error {
		_, err := a.notInterested.Set(ctx, u, k, on)
		return err
	}, IsWatchlisted: func(ctx context.Context, u *core.User, k core.RatingKey) (bool, error) {
		if a.watchlist == nil {
			return false, core.ErrNotAvailable
		}
		return a.watchlist.Contains(ctx, u.ID, k)
	}, WatchlistMembership: a.watchlistMembership, SetWatchlistSource: func(ctx context.Context, u *core.User, k core.RatingKey, on bool) error {
		if a.watchlist == nil {
			return core.ErrNotAvailable
		}
		var err error
		if on {
			err = a.watchlist.Remind(ctx, u, k)
		} else {
			err = a.watchlist.Remove(ctx, u.ID, k)
		}
		if err == nil && a.sessionEvents != nil {
			if e := a.sessionEvents.Publish(ctx, frontend.Event{Type: "UserDataChanged", ViewerID: u.ID, Item: k, OriginProtocol: protocol}); e != nil && a.log != nil {
				a.log.Warn("watchlist event publication failed", "err", e)
			}
		}
		return err
	}, RemoveContinue: func(ctx context.Context, u *core.User, k core.RatingKey) error {
		if err := a.state.HideContinue(ctx, u.ID, k); err != nil {
			return err
		}
		if a.discoveryPersonal != nil && !k.IsPluginKey() {
			if err := a.discoveryPersonal.RemoveContinue(ctx, u.ID, k); err != nil && !errors.Is(err, core.ErrNotFound) {
				return err
			}
		}
		if a.sessionEvents != nil {
			if e := a.sessionEvents.Publish(ctx, frontend.Event{Type: "UserDataChanged", ViewerID: u.ID, Item: k, OriginProtocol: protocol}); e != nil && a.log != nil {
				a.log.Warn("remove continue event publication failed", "err", e)
			}
		}
		return nil
	}, CanFavoriteEntity: func(ctx context.Context, u *core.User, r frontend.FavoriteRef) bool {
		return a.favoriteAllowed(ctx, u, favorites.Subject{Kind: favorites.Kind(r.Kind), ID: r.ID}, vis)
	}, SetEntityFavoriteSource: func(ctx context.Context, u *core.User, r frontend.FavoriteRef, on bool) error {
		_, err := a.favorites.Set(ctx, u, favorites.Subject{Kind: favorites.Kind(r.Kind), ID: r.ID}, on)
		return err
	}, EntityFavoritesSource: func(ctx context.Context, u *core.User, refs []frontend.FavoriteRef) (map[frontend.FavoriteRef]bool, error) {
		out := make(map[frontend.FavoriteRef]bool, len(refs))
		for _, r := range refs {
			yes, err := a.favorites.Has(ctx, u, favorites.Subject{Kind: favorites.Kind(r.Kind), ID: r.ID})
			if err != nil {
				return nil, err
			}
			out[r] = yes
		}
		return out, nil
	}}
	if a.recommend != nil {
		deps.Related = a.recommend
	}
	cat := core.Catalog(a.catalog)
	// The surface's availability filter and search sources carry step aside:
	// Plex paths read FilterAvailableFunc and SearchSources directly, so the
	// bare availability filter here let held titles through every list and
	// search on Plex apps.
	if a.availability != nil {
		cat = &availabilityCatalog{Catalog: a.catalog, availability: a.availability, stepAside: stepAside}
		deps.FilterAvailable = filter
		deps.JudgedOnFacts = a.availability.JudgedOnFacts
	} else if stepAside != nil {
		deps.FilterAvailable = filter
	}
	deps.Catalog = cat
	if a.discoveryPersonal != nil {
		deps.Collections = a.discoveryPersonal.ReviewCollections(a.collections)
	}
	deps.Recommender = a.personalize
	deps.NextUp = func(ctx context.Context, viewer *core.User, limit int) ([]core.Title, error) {
		return a.experienceContinueKind(ctx, viewer, liveLibraries(), rowVis, core.TypeShow, limit)
	}
	deps.VibeSearch = a.collections
	deps.SearchSources = a.searchSourcesFor(stepAside)
	deps.FavoriteShows = a.favoriteShows
	deps.PrioritizeAvailable = func(ctx context.Context, items []core.Title) []core.Title {
		if a.availability == nil {
			return items
		}
		return a.availability.Prioritize(ctx, items)
	}
	deps.EpisodeOrder = a.episodeorder
	if a.discoveryFeatures != nil {
		deps.Discovery = a.discoveryFeatures.Frontend()
	}
	if a.stats != nil {
		deps.Dashboard = a.stats.Dashboard()
	}
	if a.hostResources != nil {
		deps.Resources = a.hostResources
	}
	// Wire the account metadata provider through the shared library interfaces.
	deps.LibraryGUIDsFunc = func() frontend.LibraryGUIDs {
		a.plexMetaMu.Lock()
		defer a.plexMetaMu.Unlock()
		if a.plexGUID == nil {
			return nil
		}
		return a.plexGUID
	}
	deps.LibraryMetadataFunc = func() frontend.LibraryMetadata {
		a.plexMetaMu.Lock()
		defer a.plexMetaMu.Unlock()
		if a.plexMeta == nil {
			return nil
		}
		return a.plexMeta
	}
	deps.Markers = a.markers
	deps.LibraryTitles = a.libraryTitles
	deps.Layout = a.personalize
	if a.pluginsRT != nil && a.pluginsRT.kindCatalog != nil {
		deps.PluginLibraries = a.pluginsRT.kindCatalog
	} else if a.libraries != nil {
		deps.PluginLibraries = adapters.AttachKindCatalog(context.Background(), adapters.KindCatalogDeps{
			TitleInLibrary: func(ctx context.Context, lib core.Library, key core.RatingKey) (bool, error) {
				return a.catalog.TitleInFilter(ctx, libraryAccessFilter(lib), key)
			},
			TitlesInLibrary: func(ctx context.Context, lib core.Library, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
				return a.catalog.TitlesInFilter(ctx, libraryAccessFilter(lib), keys)
			},
			Libraries: a.libraries,
			Logger:    a.log,
		})
	}
	if a.playback != nil {
		pb := a.playback
		var kr *adapters.KindResolver
		if a.pluginsRT != nil {
			kr = a.pluginsRT.kindResolver
		}
		deps.PluginProbedMedia = func(ctx context.Context, u *core.User, title core.Title) (core.MediaInfo, bool) {
			if !title.Key.IsPluginKey() {
				return core.MediaInfo{}, false
			}
			var best core.MediaInfo
			var found bool
			if info, ok := pb.CachedItemProbe(title.Key); ok && !info.Estimated {
				best, found = info, true
				if len(best.Chapters) > 0 {
					return best, true
				}
			}
			if kr != nil && u != nil {
				// Strictly cached candidates only (allowResolve=false); never
				// calls the plugin's /resolve endpoint on a detail read.
				if variants, err := kr.KindVersions(ctx, u, title.Key, kindAccept, false); err == nil && len(variants) > 0 {
					enriched := pb.WithCachedVariantInfo(ctx, u, &title, variants)
					for _, v := range enriched {
						if v.Info.Estimated {
							continue
						}
						if len(v.Info.Chapters) > 0 {
							return core.CloneMediaInfo(v.Info), true
						}
						if !found && len(v.Info.Streams) > 0 {
							best, found = core.CloneMediaInfo(v.Info), true
						}
					}
				}
			}
			return best, found
		}
	}
	deps.Now = parityClock()
	deps.Log = a.log
	if a.db != nil {
		db := a.db
		deps.HomeRevision = func(ctx context.Context, u *core.User) (frontend.HomeRevision, error) {
			pending := a.state.PendingWatchRevision(u.ID)
			revision, err := experience.ReadHomeRevision(ctx, db, u)
			if pending != 0 {
				revision.Viewer += "." + strconv.FormatUint(pending, 36)
			}
			return revision, err
		}
		deps.NoProviderWait = func(ctx context.Context) context.Context {
			return core.WithLocalResponse(catalog.WithReadWaitBudget(ctx, 0))
		}
	}
	service, err := experience.NewService(deps)
	if err != nil {
		return nil, err
	}
	watchedTitles = service.TitlesWatched
	var artworkSource frontend.Artwork
	if len(artwork) > 0 {
		artworkSource = artwork[0]
	}
	detail := detailPersonalization{now: time.Now, ratings: a.ratings}
	if a.personalize != nil {
		detail.micro = a.personalize.MicroReason
	}
	if a.cfg != nil {
		detail.enabled = func() bool { return a.cfg.Read().Personalize.MicroReasons }
	}
	scoped := &surfaceExperience{Service: service, receipts: receipts, protocol: protocol, artwork: artworkSource, pluginLibs: deps.PluginLibraries, active: a.users.ActiveUser, rowViews: rowViews, revealDetail: func(viewer *core.Viewer, device core.Device, key core.RatingKey) {
		if a.playback == nil || viewer == nil {
			return
		}
		a.playback.RevealDetail(&core.User{ID: viewer.ID}, device.ID, key)
	}, detail: detail, generation: func(ctx context.Context) (int64, error) {
		gen, err := viewerpolicy.Read(ctx, a.db)
		if err != nil || stepAside == nil {
			return gen, err
		}
		if heldGen := stepAside.Generation(); heldGen != 0 {
			gen = int64(uint64(gen)*1099511628211 ^ heldGen)
		}
		return gen, nil
	}, catalogGen: func() uint64 {
		var gen uint64
		if a.catalog != nil {
			gen = a.catalog.WriteGeneration()
		}
		if a.libraries != nil {
			gen = gen*1099511628211 ^ a.libraries.Generation()
		}
		if a.pluginsRT != nil && a.pluginsRT.pluginitems != nil {
			gen = gen*1099511628211 ^ a.pluginsRT.pluginitems.Generation()
		}
		return gen
	}, rowWatch: func(ctx context.Context, id int64) (experience.WatchRevision, error) {
		return experience.ReadWatchRevision(ctx, a.db, id, a.state.PendingWatchRevision(id))
	}, rowWatchlist: func(ctx context.Context, id int64) (experience.WatchlistRevision, error) {
		return experience.ReadWatchlistRevision(ctx, a.db, id)
	}, rowGen: func() uint64 {
		if stepAside == nil {
			return 0
		}
		return stepAside.Generation()
	}, posters: func(ctx context.Context, typ core.MediaType, limit int) ([]core.Title, error) {
		if a.catalog == nil {
			return nil, core.ErrNotAvailable
		}
		return a.catalog.LibraryPosters(ctx, typ, limit)
	}, modelReady: func(id int64) bool {
		return a.personalize != nil && a.personalize.ModelReady(id)
	}, publicUsers: a.users.Users, publicEpoch: func() uint64 {
		// The policy epoch: any change to accounts, owner content
		// policy (safety/config), libraries or the catalogue drops cached
		// public artwork verdicts.
		var gen uint64
		if a.users != nil {
			gen = a.users.Generation()
		}
		if a.safety != nil {
			gen = gen*1099511628211 ^ a.safety.Generation()
		}
		if a.cfg != nil {
			gen = gen*1099511628211 ^ a.cfg.Generation()
		}
		if a.catalog != nil {
			gen = gen*1099511628211 ^ a.catalog.WriteGeneration()
		}
		if a.libraries != nil {
			gen = gen*1099511628211 ^ a.libraries.Generation()
		}
		if a.pluginsRT != nil && a.pluginsRT.pluginitems != nil {
			gen = gen*1099511628211 ^ a.pluginsRT.pluginitems.Generation()
		}
		if stepAside != nil {
			if heldGen := stepAside.Generation(); heldGen != 0 {
				gen = gen*1099511628211 ^ heldGen
			}
		}
		return gen
	}}
	if withPlugin, ok := scoped.artwork.(interface {
		WithPluginItem(func(context.Context, *core.User, core.Device, core.RatingKey) (*core.Title, []frontend.Image, error)) frontend.Artwork
	}); ok {
		scoped.artwork = withPlugin.WithPluginItem(scoped.pluginArtworkItem)
	}
	return scoped, nil
}

type surfaceExperience struct {
	*experience.Service
	receipts     *experience.Receipts
	protocol     core.Protocol
	artwork      frontend.Artwork
	pluginLibs   experience.PluginLibraries
	active       func(context.Context, int64) (*core.User, error)
	rowViews     *experience.RowViewService
	revealDetail func(*core.Viewer, core.Device, core.RatingKey)
	playability  func(context.Context, frontend.ItemRequest) *bool
	detail       detailPersonalization
	generation   func(context.Context) (int64, error)
	catalogGen   func() uint64
	rowGen       func() uint64
	rowWatchlist func(context.Context, int64) (experience.WatchlistRevision, error)
	rowWatch     func(context.Context, int64) (experience.WatchRevision, error)
	posters      func(context.Context, core.MediaType, int) ([]core.Title, error)
	modelReady   func(int64) bool
	rowMu        sync.Mutex
	rowCache     map[string]rowCacheSlot
	collageMu    sync.Mutex
	collage      map[string]collageSlot
	// Public (unauthenticated) title artwork: the active-user snapshot
	// and per-item intersection verdicts, cached per policy epoch.
	publicUsers func(context.Context) ([]core.User, error)
	publicEpoch func() uint64
	publicMu    sync.Mutex
	publicCache publicArtworkCache
}

type pluginArtworkBatchKey struct{}

type pluginArtworkBatch struct {
	viewerID int64
	items    map[core.RatingKey]experience.PluginItemView
}

func (s *surfaceExperience) pluginArtworkItem(ctx context.Context, viewer *core.User, device core.Device, key core.RatingKey) (*core.Title, []frontend.Image, error) {
	if s == nil || viewer == nil || !key.IsPluginKey() || s.pluginLibs == nil {
		return nil, nil, core.ErrNotFound
	}
	if batch, ok := ctx.Value(pluginArtworkBatchKey{}).(pluginArtworkBatch); ok && batch.viewerID == viewer.ID {
		if piv, found := batch.items[key]; found {
			t := piv.Title
			return &t, piv.Images, nil
		}
	}
	if _, err := s.Service.AuthorizeArtwork(ctx, viewer, device, key); err != nil {
		return nil, nil, err
	}
	piv, err := s.pluginLibs.Item(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	t := piv.Title
	return &t, piv.Images, nil
}

type collageSlot struct {
	gen   uint64
	img   frontend.Image
	ready bool
	busy  bool
}

// PolicyGeneration is the viewer-policy epoch. Personal-view caches drop
// when it changes.
func (s *surfaceExperience) PolicyGeneration(ctx context.Context) (int64, error) {
	if s.generation == nil {
		return 0, nil
	}
	return s.generation(ctx)
}

// stepAsideActive reports whether this surface has step-aside held titles that
// must not be bypassed by an unrestricted (owner) fast-path. Only the Plex
// surface builds a step-aside policy, so rowGen is zero everywhere else.
func (s *surfaceExperience) stepAsideActive() bool {
	return s != nil && s.rowGen != nil && s.rowGen() != 0
}

// wrapUnrestricted disables an unrestricted fast-path while step-aside holds
// titles, so owners still run filtered selection instead of bypassing it. It
// mirrors the per-protocol Plex experience wrapper the feature shipped with,
// now that every protocol mounts from the shared surface.
func (s *surfaceExperience) wrapUnrestricted(fn func(*core.User) bool) func(*core.User) bool {
	if !s.stepAsideActive() {
		return fn
	}
	return func(u *core.User) bool {
		if s.stepAsideActive() {
			return false
		}
		if fn == nil {
			return false
		}
		return fn(u)
	}
}

func (s *surfaceExperience) NewRecent(o frontend.RecentOptions) frontend.Recent {
	o.Unrestricted = s.wrapUnrestricted(o.Unrestricted)
	return s.Service.NewRecent(o)
}

func (s *surfaceExperience) NewContinue(o frontend.ContinueOptions) frontend.Continue {
	o.Unrestricted = s.wrapUnrestricted(o.Unrestricted)
	return s.Service.NewContinue(o)
}

func (s *surfaceExperience) NewScopedCollection(o frontend.ScopedCollectionOptions) frontend.ScopedCollection {
	o.Unrestricted = s.wrapUnrestricted(o.Unrestricted)
	return s.Service.NewScopedCollection(o)
}

func (s *surfaceExperience) NewDiscoverySelection(o frontend.DiscoverySelectionOptions) frontend.DiscoverySelection {
	o.Unrestricted = s.wrapUnrestricted(o.Unrestricted)
	return s.Service.NewDiscoverySelection(o)
}

func (s *surfaceExperience) NewHomeSelection(o frontend.HomeSelectionOptions) frontend.HomeSelection {
	if s.stepAsideActive() {
		o.Restricted = true
	}
	return s.Service.NewHomeSelection(o)
}

func (s *surfaceExperience) NewPostPlaySelection(o frontend.PostPlaySelectionOptions) frontend.PostPlaySelection {
	if s.stepAsideActive() {
		o.Unrestricted = false
	}
	return s.Service.NewPostPlaySelection(o)
}

func (s *surfaceExperience) ListRowViews(ctx context.Context, req frontend.RowViewRequest) ([]frontend.RowView, error) {
	if req.Protocol != s.protocol || s.rowViews == nil {
		return nil, core.ErrUnauthorized
	}
	return s.rowViews.ListRowViews(ctx, req)
}

// BeginBootstrapRead is used only by Vela's outer before/after revision fence.
func (s *surfaceExperience) BeginBootstrapRead(ctx context.Context, viewer *frontend.Viewer, revision frontend.HomeRevision) context.Context {
	return experience.WithBootstrapProof(ctx, viewer.ID, revision.Policy)
}

func (s *surfaceExperience) ResolveRowViews(ctx context.Context, reqs []frontend.RowViewPageRequest) ([]frontend.RowViewPage, error) {
	if s.rowViews == nil {
		return nil, core.ErrUnauthorized
	}
	return s.rowViews.ResolveRowViews(ctx, reqs)
}

func (s *surfaceExperience) ValidateRowViews(ctx context.Context, reqs []frontend.RowViewPageRequest) error {
	if s.rowViews == nil || (len(reqs) > 0 && reqs[0].Protocol != s.protocol) {
		return core.ErrUnauthorized
	}
	return s.rowViews.ValidateRowViews(ctx, reqs)
}

func (s *surfaceExperience) ResolveRowView(ctx context.Context, req frontend.RowViewPageRequest) (frontend.RowViewPage, error) {
	if req.Protocol != s.protocol || s.rowViews == nil {
		return frontend.RowViewPage{}, core.ErrUnauthorized
	}
	return s.rowViews.ResolveRowView(ctx, req)
}

func (s *surfaceExperience) LibraryCollage(ctx context.Context, viewer *frontend.Viewer, device frontend.Device, viewID string) (frontend.Image, error) {
	if s == nil || s.artwork == nil || viewer == nil || viewID == "" || s.posters == nil {
		return frontend.Image{}, core.ErrNotAvailable
	}
	builder, ok := s.artwork.(interface {
		LibraryCollage(context.Context, *core.Viewer, core.Device, string, []core.Title) (frontend.Image, error)
	})
	if !ok {
		return frontend.Image{}, core.ErrNotAvailable
	}
	typ, ok := s.libraryMediaType(ctx, viewer, viewID)
	if !ok {
		return frontend.Image{}, core.ErrNotFound
	}
	var gen uint64
	if s.catalogGen != nil {
		gen = s.catalogGen()
	}
	if img, fresh := s.collageImage(viewID, gen); fresh {
		return img, nil
	} else if img.Path != "" {
		s.startCollage(viewID, gen, typ, *viewer, device, builder)
		return img, nil
	}
	s.startCollage(viewID, gen, typ, *viewer, device, builder)
	return frontend.Image{}, core.ErrNotFound
}

func (s *surfaceExperience) libraryMediaType(ctx context.Context, viewer *frontend.Viewer, viewID string) (core.MediaType, bool) {
	if s == nil || s.Service == nil {
		return "", false
	}
	n, err := ids.ParseGUID(ids.View, viewID)
	if err != nil {
		return "", false
	}
	want := strconv.FormatInt(n, 10)
	views, err := s.Views(ctx, viewer)
	if err != nil {
		return "", false
	}
	for _, view := range views {
		if view.ID == want {
			if view.PluginID != "" {
				return "", false
			}
			return view.Type, true
		}
	}
	return "", false
}

func (s *surfaceExperience) collageImage(viewID string, gen uint64) (frontend.Image, bool) {
	s.collageMu.Lock()
	defer s.collageMu.Unlock()
	slot, ok := s.collage[viewID]
	if !ok || !slot.ready {
		return frontend.Image{}, false
	}
	return slot.img, slot.gen == gen
}

func (s *surfaceExperience) startCollage(viewID string, gen uint64, typ core.MediaType, viewer core.Viewer, device frontend.Device, builder interface {
	LibraryCollage(context.Context, *core.Viewer, core.Device, string, []core.Title) (frontend.Image, error)
}) {
	s.collageMu.Lock()
	if s.collage == nil {
		s.collage = map[string]collageSlot{}
	}
	slot := s.collage[viewID]
	if slot.busy || (slot.ready && slot.gen == gen) {
		s.collageMu.Unlock()
		return
	}
	slot.busy = true
	s.collage[viewID] = slot
	s.collageMu.Unlock()
	go func() {
		defer func() {
			s.collageMu.Lock()
			slot := s.collage[viewID]
			slot.busy = false
			s.collage[viewID] = slot
			s.collageMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		titles, err := s.posters(ctx, typ, 16)
		if err != nil {
			return
		}
		img, err := builder.LibraryCollage(ctx, &viewer, device, viewID, titles)
		if err != nil || img.Path == "" {
			return
		}
		img.Subject = viewID
		s.collageMu.Lock()
		s.collage[viewID] = collageSlot{gen: gen, img: img, ready: true}
		s.collageMu.Unlock()
	}()
}

func (s *surfaceExperience) ItemArtworkPage(ctx context.Context, viewer *core.Viewer, device core.Device, keys []core.RatingKey) (map[core.RatingKey][]frontend.Image, error) {
	out := make(map[core.RatingKey][]frontend.Image, len(keys))
	var titleKeys []core.RatingKey
	var franchiseKeys []core.RatingKey
	var smartKeys []core.RatingKey
	seenDirect := map[core.RatingKey]bool{}
	for _, k := range keys {
		switch k.Kind() {
		case core.KindCollection:
			if seenDirect[k] {
				continue
			}
			seenDirect[k] = true
			franchiseKeys = append(franchiseKeys, k)
		case core.KindSmart:
			if seenDirect[k] {
				continue
			}
			seenDirect[k] = true
			smartKeys = append(smartKeys, k)
		case core.KindPlaylist:
			if seenDirect[k] {
				continue
			}
			seenDirect[k] = true
			imgs, ok, err := s.ItemArtwork(ctx, viewer, device, k)
			if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnauthorized) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if ok {
				out[k] = imgs
			}
		default:
			titleKeys = append(titleKeys, k)
		}
	}
	var collections []core.Collection
	if len(smartKeys) > 0 && s.artwork != nil {
		cols, err := s.PageArtworkSmartCollections(ctx, viewer, smartKeys)
		if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnauthorized) {
			cols = nil
			err = nil
		}
		if err != nil {
			return nil, err
		}
		for _, k := range smartKeys {
			col, ok := cols[k]
			if !ok {
				continue
			}
			if col.Key == k {
				collections = append(collections, col)
				continue
			}
			// A promoted ephemeral collection can still be addressed
			// by its original key. Keep that key on the artwork request.
			imgs, _, err := s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: viewer, Device: device, Item: k})
			if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnauthorized) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out[k] = imgs
		}
	}
	if len(franchiseKeys) > 0 && s.artwork != nil {
		cols, handled, err := s.PageArtworkCollections(ctx, viewer, franchiseKeys)
		if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnauthorized) {
			handled = true
			cols = nil
			err = nil
		}
		if err != nil {
			return nil, err
		}
		if handled {
			for _, k := range franchiseKeys {
				if col, ok := cols[k]; ok {
					collections = append(collections, col)
				}
			}
		} else {
			for _, k := range franchiseKeys {
				imgs, ok, err := s.ItemArtwork(ctx, viewer, device, k)
				if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnauthorized) {
					continue
				}
				if err != nil {
					return nil, err
				}
				if ok {
					out[k] = imgs
				}
			}
		}
	}
	if len(collections) > 0 {
		if batch, ok := s.artwork.(interface {
			ListCollectionPage(context.Context, *core.Viewer, core.Device, []core.Collection) (map[core.RatingKey][]frontend.Image, error)
		}); ok {
			listed, err := batch.ListCollectionPage(ctx, viewer, device, collections)
			if err != nil {
				return nil, err
			}
			for key, images := range listed {
				out[key] = images
			}
		} else {
			for i := range collections {
				col := &collections[i]
				listCtx := ctx
				if col.Key.Kind() == core.KindCollection || (!col.Upcoming && col.PosterPath != "") {
					listCtx = frontend.WithArtworkCollection(ctx, col)
				}
				imgs, _, err := s.artwork.List(listCtx, frontend.ArtworkRequest{Viewer: viewer, Device: device, Item: col.Key})
				if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnauthorized) {
					continue
				}
				if err != nil {
					return nil, err
				}
				out[col.Key] = imgs
			}
		}
	}
	if len(titleKeys) == 0 {
		return out, nil
	}
	ctx = core.WithCatalogViewer(ctx, viewer)
	titles, err := s.PageArtworkTitles(ctx, viewer, titleKeys)
	if err != nil {
		return nil, err
	}
	var pluginKeys []core.RatingKey
	for _, t := range titles {
		if t.Key.IsPluginKey() {
			pluginKeys = append(pluginKeys, t.Key)
		}
	}
	if s.artwork == nil {
		if len(pluginKeys) > 0 && s.pluginLibs != nil {
			pivs, err := s.pluginLibs.ItemsMany(ctx, pluginKeys)
			if err != nil {
				return nil, err
			}
			for _, k := range pluginKeys {
				if piv, ok := pivs[k]; ok {
					out[k] = piv.Images
				}
			}
		}
		return out, nil
	}
	if len(pluginKeys) > 0 && s.pluginLibs != nil && viewer != nil {
		pivs, err := s.pluginLibs.ItemsMany(ctx, pluginKeys)
		if err != nil {
			return nil, err
		}
		ctx = context.WithValue(ctx, pluginArtworkBatchKey{}, pluginArtworkBatch{viewerID: viewer.ID, items: pivs})
	}
	if len(titles) == 0 {
		return out, nil
	}
	if page, ok := s.artwork.(frontend.PageArtwork); ok {
		listed, err := page.ListPage(ctx, viewer, device, titles)
		if err != nil {
			return nil, err
		}
		for k, imgs := range listed {
			out[k] = imgs
		}
		return out, nil
	}
	for i := range titles {
		images, _, err := s.artwork.List(frontend.WithArtworkTitle(ctx, &titles[i]), frontend.ArtworkRequest{Viewer: viewer, Device: device, Item: titles[i].Key})
		if err != nil {
			return nil, err
		}
		out[titles[i].Key] = images
	}
	return out, nil
}

func (s *surfaceExperience) ItemArtwork(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) ([]frontend.Image, bool, error) {
	active, err := s.Service.AuthorizeArtwork(ctx, viewer, device, key)
	if err != nil {
		return nil, false, err
	}
	if s.artwork == nil {
		if key.IsPluginKey() && s.pluginLibs != nil {
			piv, err := s.pluginLibs.Item(ctx, key)
			if err != nil {
				return nil, false, err
			}
			return piv.Images, true, nil
		}
		return nil, false, core.ErrNotAvailable
	}
	return s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: active, Device: device, Item: key})
}

func (s *surfaceExperience) RelatedArtwork(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) (frontend.RelatedArtwork, error) {
	service, err := experience.NewRelatedArtworkService(experience.RelatedArtworkDeps{Authorize: s.Service.AuthorizeArtwork, StoredItem: s.Service.StoredItem, List: func(ctx context.Context, active *core.Viewer, device core.Device, key core.RatingKey) ([]frontend.Image, bool, error) {
		if s.artwork == nil {
			if key.IsPluginKey() && s.pluginLibs != nil {
				piv, err := s.pluginLibs.Item(ctx, key)
				if err != nil {
					return nil, false, err
				}
				return piv.Images, true, nil
			}
			return nil, false, core.ErrNotAvailable
		}
		return s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: active, Device: device, Item: key})
	}})
	if err != nil {
		return frontend.RelatedArtwork{}, err
	}
	return service.RelatedArtwork(ctx, viewer, device, key)
}

func (s *surfaceExperience) Item(ctx context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
	detail, err := s.Service.Item(ctx, req)
	if err != nil {
		return detail, err
	}
	if req.Key.IsPluginKey() || detail.Title.Key.IsPluginKey() {
		if s.pluginLibs != nil {
			if piv, pErr := s.pluginLibs.Item(ctx, detail.Title.Key); pErr == nil {
				isBook := piv.Title.MediaClass == core.MediaClassBook || piv.Title.MBItemType == "Book"
				playable := piv.Playable && !isBook
				detail.CanPlay = &playable
				if isBook {
					readable := piv.Playable && piv.Unavailable == nil
					detail.CanRead = &readable
				}
			}
		}
		if s.artwork == nil || req.Viewer == nil {
			return detail, nil
		}
		viewer, uErr := s.active(ctx, req.Viewer.ID)
		if uErr != nil || viewer == nil {
			detail.Images = nil
			detail.ImagesAuthoritative = false
			return detail, nil
		}
		images, complete, aErr := s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: viewer, Device: req.Device, Item: detail.Title.Key})
		if aErr == nil {
			detail.Images, detail.ImagesAuthoritative = images, complete
		}
		return detail, nil
	}
	if s.revealDetail != nil && core.ColdDetail(ctx, req.Key) {
		s.revealDetail(req.Viewer, req.Device, detail.Title.Key)
	}
	if detail.Title.Key != 0 && s.playability != nil {
		if detail.Title.Type == core.TypeShow || detail.Title.Type == core.TypeSeason {
			playable := false
			detail.CanPlay = &playable
		} else {
			detail.CanPlay = s.playability(ctx, req)
		}
	}
	if s.artwork == nil {
		return detail, nil
	}
	// The service has already authorized the item. Resolve the active user
	// again before passing it to the artwork policy; do not trust caller fields.
	detail.Images = nil
	detail.ImagesAuthoritative = false
	if req.Viewer == nil {
		return detail, nil
	}
	viewer, err := s.active(ctx, req.Viewer.ID)
	if err != nil || viewer == nil {
		return detail, nil
	}
	images, complete, err := s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: viewer, Device: req.Device, Item: req.Key})
	if err == nil {
		detail.Images, detail.ImagesAuthoritative = images, complete
	}
	return detail, nil
}

type rowCacheRevision struct {
	policy    int64
	stepAside uint64
	watchlist experience.WatchlistRevision
	watch     experience.WatchRevision
}

type rowCacheSlot struct {
	gen   rowCacheRevision
	at    time.Time
	rows  []frontend.Row
	ready bool
	busy  bool
}

const homeRowCacheTTL = 15 * time.Second

func (s *surfaceExperience) SnapshotRows(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
	return s.cachedRows(ctx, req, false)
}

func (s *surfaceExperience) Rows(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
	return s.cachedRows(ctx, req, true)
}

func (s *surfaceExperience) cachedRows(ctx context.Context, req frontend.RowRequest, issue bool) ([]frontend.Row, error) {
	// Issued rows mint receipts. A cached success would skip that transaction,
	// so a later bind failure could not roll back and the client would keep
	// handles whose receipts were never written. Snapshots are safe to reuse.
	for attempt := 0; ; attempt++ {
		rows, err := s.readRows(ctx, req, issue)
		if !errors.Is(err, viewerpolicy.ErrChanged) {
			return rows, err
		}
		// Vela owns the complete visit, including heroes and row views.
		if s.protocol == "vela" || attempt == 2 || ctx.Err() != nil {
			return nil, frontend.ErrRowViewStale
		}
		ctx = frontend.WithFreshRequestMemo(reqmemo.With(ctx))
	}
}

func (s *surfaceExperience) readRows(ctx context.Context, req frontend.RowRequest, issue bool) ([]frontend.Row, error) {
	if issue || req.HomeFallbackOnly || (req.Placement != frontend.PlacementHome && req.Placement != frontend.PlacementLibrary) {
		return s.loadRows(ctx, req, issue)
	}
	key, stepAside := s.rowCacheKey(req)
	gen := rowCacheRevision{stepAside: stepAside}
	if s.generation != nil {
		policy, err := s.generation(ctx)
		if err != nil {
			return nil, err
		}
		gen.policy = policy
	}
	if s.rowWatch != nil && req.Viewer != nil {
		watch, err := s.rowWatch(ctx, req.Viewer.ID)
		if err != nil {
			return nil, err
		}
		gen.watch = watch
	}
	if s.rowWatchlist != nil && req.Viewer != nil && req.Placement == frontend.PlacementHome {
		rev, err := s.rowWatchlist(ctx, req.Viewer.ID)
		if err != nil {
			return nil, err
		}
		gen.watchlist = rev
	}

	s.rowMu.Lock()
	slot := s.rowCache[key]
	fresh := slot.ready && slot.gen == gen && time.Since(slot.at) < homeRowCacheTTL
	if fresh || (slot.ready && slot.gen == gen && slot.busy) {
		rows := cloneHomeRows(slot.rows)
		s.rowMu.Unlock()
		return rows, nil
	}
	stale := slot.ready && slot.gen == gen
	if stale {
		slot.busy = true
		s.rowCache[key] = slot
		rows := cloneHomeRows(slot.rows)
		s.rowMu.Unlock()
		go s.refreshRows(key, gen, req, issue)
		return rows, nil
	}
	s.rowMu.Unlock()
	rows, err := s.loadRows(ctx, req, issue)
	if err != nil {
		return nil, err
	}
	if err := s.checkRowCachePolicy(ctx, gen); err != nil {
		return nil, err
	}
	s.storeRows(key, gen, rows)
	return rows, nil
}

func (s *surfaceExperience) checkRowCachePolicy(ctx context.Context, gen rowCacheRevision) error {
	if s.generation == nil {
		return nil
	}
	current, err := s.generation(ctx)
	if err != nil {
		return err
	}
	if current != gen.policy {
		return viewerpolicy.ErrChanged
	}
	return nil
}

func (s *surfaceExperience) refreshRows(key string, gen rowCacheRevision, req frontend.RowRequest, issue bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer func() {
		s.rowMu.Lock()
		slot := s.rowCache[key]
		slot.busy = false
		s.rowCache[key] = slot
		s.rowMu.Unlock()
	}()
	rows, err := s.loadRows(ctx, req, issue)
	if err != nil || s.checkRowCachePolicy(ctx, gen) != nil {
		return
	}
	s.storeRows(key, gen, rows)
}

func (s *surfaceExperience) storeRows(key string, gen rowCacheRevision, rows []frontend.Row) {
	s.rowMu.Lock()
	defer s.rowMu.Unlock()
	if s.rowCache == nil {
		s.rowCache = map[string]rowCacheSlot{}
	}
	s.rowCache[key] = rowCacheSlot{gen: gen, at: time.Now(), rows: cloneHomeRows(rows), ready: true}
}

func (s *surfaceExperience) rowCacheKey(req frontend.RowRequest) (string, uint64) {
	viewer := int64(0)
	ready := false
	if req.Viewer != nil {
		viewer = req.Viewer.ID
		if s.modelReady != nil {
			ready = s.modelReady(viewer)
		}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	key := fmt.Sprintf("%s\n%d\n%s\n%s\n%s\n%s\n%s\n%d\n%t", s.protocol, viewer, req.Device.ID, req.Device.Language, req.Placement, req.ViewID, req.Purpose, limit, ready)
	// Home stays cached for the TTL. Catalogue write generation is not part of
	// the key: a background title refresh bumps it on every provider hydrate
	// and was forcing the next Home to rebuild. Step-aside revision is rare and
	// invalidates immediately when a server finishes syncing or toggles.
	var gen uint64
	if s.rowGen != nil {
		gen = s.rowGen()
	}
	return key, gen
}

func cloneHomeRows(in []frontend.Row) []frontend.Row {
	out := make([]frontend.Row, len(in))
	for i, row := range in {
		out[i] = row
		out[i].Entries = cloneRowEntries(row.Entries)
		if row.Items != nil {
			out[i].Items = append([]core.Title(nil), row.Items...)
		}
	}
	return out
}

func cloneRowEntries(in []frontend.RowEntry) []frontend.RowEntry {
	if in == nil {
		return nil
	}
	out := make([]frontend.RowEntry, len(in))
	for i, entry := range in {
		out[i] = entry
		if entry.Title != nil {
			title := *entry.Title
			out[i].Title = &title
		}
	}
	return out
}

func (s *surfaceExperience) loadRows(ctx context.Context, req frontend.RowRequest, issue bool) ([]frontend.Row, error) {
	ctx = availability.WithDiscoveryMemo(livetv.WithChannelMemo(context.WithValue(ctx, experienceSituationKey{}, situationFor(req))))
	var rows []frontend.Row
	var err error
	if issue {
		rows, err = s.Service.Rows(ctx, req)
	} else {
		rows, err = s.Service.SnapshotRows(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	var active *core.User
	if req.Viewer != nil {
		active, _ = s.active(ctx, req.Viewer.ID)
	}
	if active != nil {
		if batch, ok := s.artwork.(interface {
			PrepareChannelArtwork(context.Context, *core.Viewer, []string) (context.Context, error)
		}); ok {
			var channels []string
			for _, row := range rows {
				for _, entry := range row.Entries {
					if entry.LiveChannel != nil {
						channels = append(channels, entry.LiveChannel.ID)
					}
				}
			}
			if prepared, err := batch.PrepareChannelArtwork(ctx, active, channels); err == nil {
				ctx = prepared
			}
		}
	}
	for _, row := range rows {
		s.warmArtwork(row.Items, req.Device)
		for _, entry := range row.Entries {
			if entry.Title != nil {
				s.warmArtwork([]core.Title{*entry.Title}, req.Device)
			}
			if channel := entry.LiveChannel; channel != nil {
				// An upstream logo may carry provider credentials. Only return
				// a policy-scoped descriptor to non-Plex front ends.
				channel.Logo = ""
				if active != nil && s.artwork != nil {
					images, complete, e := s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: active, Device: req.Device, Channel: channel.ID})
					if e == nil && complete {
						channel.Images = images
					}
				}
			}
		}
	}
	return rows, nil
}

func (s *surfaceExperience) warmArtwork(items []core.Title, device core.Device) {
	warmer, ok := s.artwork.(interface{ Warm(core.Title, core.Device) })
	if !ok {
		return
	}
	for _, item := range items {
		if item.Key.IsPluginKey() {
			continue
		}
		warmer.Warm(item, device)
	}
}

func (s *surfaceExperience) Children(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey, query frontend.ItemQuery) (core.Page, error) {
	page, err := s.Service.Children(ctx, viewer, device, key, query)
	if err == nil {
		s.warmArtwork(page.Items, device)
		target := key
		if query.Season != nil && key.Kind() == core.KindShow {
			target = core.SeasonKey(key.TMDB(), *query.Season)
		}
		if s.revealDetail != nil && !target.IsPluginKey() && core.ColdDetail(ctx, target) {
			s.revealDetail(viewer, device, target)
		}
	}
	return page, err
}

func (s *surfaceExperience) LibraryItems(ctx context.Context, viewer *core.Viewer, device core.Device, viewID string, query frontend.ItemQuery) (core.Page, error) {
	page, err := s.Service.LibraryItems(ctx, viewer, device, viewID, query)
	if err == nil {
		s.warmArtwork(page.Items, device)
	}
	return page, err
}

func (s *surfaceExperience) Search(ctx context.Context, viewer *core.Viewer, device core.Device, query string, options frontend.ItemQuery) (frontend.SearchPage, error) {
	if s.protocol == "stremio" {
		ctx = core.WithSearchSummaryFacts(ctx)
	}
	page, err := s.Service.Search(ctx, viewer, device, query, options)
	if err == nil {
		s.warmArtwork(page.Page.Items, device)
	}
	return page, err
}

func (s *surfaceExperience) UnifiedSearch(ctx context.Context, viewer *core.Viewer, device core.Device, query string, options frontend.ItemQuery) (frontend.UnifiedSearchPage, error) {
	page, err := s.Service.UnifiedSearch(ctx, viewer, device, query, options)
	if err == nil {
		s.warmArtwork(page.Page.Items, device)
	}
	return page, err
}

func (s *surfaceExperience) Similar(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey, query frontend.ItemQuery) (frontend.SearchPage, error) {
	page, err := s.Service.Similar(ctx, viewer, device, key, query)
	if err == nil {
		s.warmArtwork(page.Page.Items, device)
	}
	return page, err
}
func (s *surfaceExperience) ReportRowInteraction(ctx context.Context, i frontend.RowInteraction) error {
	if i.Protocol != s.protocol {
		return experience.ErrReceipt
	}
	return s.receipts.Report(ctx, i)
}

// stepAsideVibe drops titles a step-aside Plex Media Server holds from a
// collection search, which an unrestricted viewer otherwise sees unfiltered.
type stepAsideVibe struct {
	core.VibeSearcher
	stepAside *experience.StepAside
}

func (v stepAsideVibe) VibeCollection(ctx context.Context, user *core.User, query string) (*core.Collection, []core.Title, bool, error) {
	c, items, ok, err := v.VibeSearcher.VibeCollection(ctx, user, query)
	if err != nil || !ok {
		return c, items, ok, err
	}
	return c, v.stepAside.FilterAvailable(core.WithCatalogViewer(ctx, user), items), ok, nil
}

type neutralLayoutFunc func(context.Context, *core.User, core.HomeSituation, string, []core.NeutralHomeRow) ([]core.NeutralHomeRow, error)

func (f neutralLayoutFunc) FinalizeNeutralHome(ctx context.Context, user *core.User, situation core.HomeSituation, section string, rows []core.NeutralHomeRow) ([]core.NeutralHomeRow, error) {
	return f(ctx, user, situation, section, rows)
}

func (v stepAsideVibe) DescriptiveSearchAvailable() bool {
	source, ok := v.VibeSearcher.(core.DescriptiveSearcher)
	return ok && source.DescriptiveSearchAvailable()
}

func (v stepAsideVibe) DescribeSearch(ctx context.Context, user *core.User, query string, wait bool) (core.DescriptiveResult, error) {
	source, ok := v.VibeSearcher.(core.DescriptiveSearcher)
	if !ok {
		return core.DescriptiveResult{State: "unavailable"}, nil
	}
	result, err := source.DescribeSearch(ctx, user, query, wait)
	if err == nil {
		result.Items = v.stepAside.FilterAvailable(core.WithCatalogViewer(ctx, user), result.Items)
	}
	return result, err
}

func (v stepAsideVibe) DescribeCached(user *core.User, query string) bool {
	ctrl, ok := v.VibeSearcher.(core.DescriptiveSearchController)
	return ok && ctrl.DescribeCached(user, query)
}

func (v stepAsideVibe) CancelDescribeSearch(user *core.User, query string) {
	if ctrl, ok := v.VibeSearcher.(core.DescriptiveSearchController); ok {
		ctrl.CancelDescribeSearch(user, query)
	}
}
