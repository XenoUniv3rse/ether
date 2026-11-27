package experience

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/reqmemo"
	"github.com/braedonsaunders/ether/internal/state"
)

// Library is a host-configured, protocol-neutral library.
type Library struct {
	ID   int
	Name string
	Type core.MediaType
	// Virtual is true only when the host asserts this library has no physical
	// locations. False means the host did not assert virtual origin.
	Virtual bool
}

// LibraryPageExclusions is what an indexed library page can push into SQL.
type LibraryPageExclusions struct {
	Keys                           []core.RatingKey
	ExcludeGenres, ExcludeKeywords []string
	Movie, Television              core.Filter
	Availability                   core.Filter
	AvailabilityInSQL              bool
	Err                            error
}

// ServiceDeps contains trusted host dependencies, never provided by a frontend.
type ServiceDeps struct {
	// NextUp returns episodes authorized for the supplied active viewer.
	NextUp func(context.Context, *core.User, int) ([]core.Title, error)
	// LibraryPresentation and BuiltinNative are supplied by the surface wiring.
	LibraryPresentation func(core.Presentations, core.MediaType) (core.MediaType, string, bool)
	BuiltinNative       func(core.MediaType) string

	// BeginPeopleRead supplies fresh, fenced policy facts for the optional People API.
	BeginPeopleRead     func(context.Context, *core.User) (PeopleRead, error)
	NotInterestedSource func(context.Context, *core.User) ([]frontend.NotInterestedEntry, error)
	ByExternalID        func(context.Context, string, string) (*core.Title, error)
	ResolveEpisode      func(context.Context, core.RatingKey, int, int) (core.RatingKey, error)
	// KeysByProviderID resolves one external provider id ("tmdb", "imdb",
	// "tvdb", case-insensitive) to candidate rating keys via indexed lookup,
	// never scanning the catalogue.
	KeysByProviderID func(context.Context, string, string, []core.MediaType) ([]core.RatingKey, error)
	Active           func(context.Context, int64) (*core.User, error)
	Libraries        []Library
	// LiveLibraries, when set, is read on each surface call so configured
	// names stay current. IDs and media types stay stable. Nil uses Libraries.
	LiveLibraries     func() []Library
	PluginLibraries   PluginLibraries
	PluginProbedMedia func(ctx context.Context, v *core.User, title core.Title) (core.MediaInfo, bool)
	Visibility        Visibility
	Get               func(context.Context, *core.User, core.RatingKey) (*core.Title, error)
	// CachedGet applies catalogue visibility without a network refresh.
	CachedGet func(context.Context, core.RatingKey) (*core.Title, error)
	// CachedGetMany reads current cached rows in bounded batches without refresh.
	CachedGetMany func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)
	// ArtworkGet and ArtworkGetMany are CachedGet and CachedGetMany for
	// image grants: they also admit search and list hits held in memory
	// without a row, so those hits keep their poster. Nil uses the cached
	// readers.
	ArtworkGet     func(context.Context, core.RatingKey) (*core.Title, error)
	ArtworkGetMany func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)
	// ArtworkRows is ArtworkGetMany for a request that lists no credited
	// people's portraits (frontend.WithArtworkRows): movies and shows as
	// their rows with the facts given, not read in full. Optional.
	ArtworkRows func(context.Context, []core.RatingKey, core.TitleFacts) (map[core.RatingKey]core.Title, error)
	// VisibilityRows reads current policy facts plus requested facets without credits.
	VisibilityRows         func(context.Context, []core.RatingKey, core.TitleFacts) (map[core.RatingKey]core.Title, error)
	PlaylistVisibilityRows func(context.Context, []core.RatingKey, core.TitleFacts) (map[core.RatingKey]core.Title, error)
	// ParentTitles resolves distinct parents using the same semantics as GetTitle.
	ParentTitles func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)
	// CachedVirtualSeason resolves a viewer's already-computed episode layout.
	CachedVirtualSeason func(context.Context, *core.User, core.RatingKey) (*core.Title, error)
	Children            func(context.Context, *core.User, core.RatingKey) ([]core.Title, error)
	Grandchildren       func(context.Context, *core.User, core.RatingKey) ([]core.Title, error)
	// SeasonEpisodes pages one catalogue season. ok is false when this viewer
	// uses a custom episode group and the caller must use Grandchildren.
	SeasonEpisodes func(context.Context, *core.User, core.RatingKey, int, int, int) (core.Page, bool, error)
	Browse         func(context.Context, core.BrowseQuery) (core.Page, error)
	// PageExclusions are viewer-specific SQL exclusions for an indexed library
	// page: hidden titles, blocked genres, blocked keywords, the policy
	// content-rating cap, and parsed plex.tv share filters. Nil keeps the
	// catalogue scan, which still applies filters the SQL page cannot see.
	PageExclusions func(context.Context, *core.User, core.MediaType) LibraryPageExclusions
	// ViewerRules is the revision of a viewer's own catalogue rules and
	// whether the rules that apply to them (their own or the household's)
	// judge titles on their facts. Optional; ExportPolicy stamps it.
	ViewerRules func(*core.User) (revision int64, judged bool)
	// FacetFilter counts a facet's values over the browsable titles matching
	// a filter in SQL. With PageExclusions, library facet lists use it
	// instead of walking the library. Optional.
	FacetFilter func(context.Context, core.Filter, string) ([]core.FacetValue, error)
	// FacetPage is one page of a facet's values, in byte order, over the
	// browsable titles matching a filter, and how many values there are; ok
	// is false when the catalogue cannot page that facet or filter, and
	// FacetFilter counts it whole. Optional.
	FacetPage func(ctx context.Context, f core.Filter, facet string, offset, limit int) (vals []core.FacetValue, total int, ok bool, err error)
	// StudioByFacetID and PersonByFacetID invert core.FacetID by index: the
	// stored studio, or the credited person's TMDB id, a facet identifier
	// names. ok is false when no title carries it. Optional.
	StudioByFacetID func(context.Context, string) (string, bool, error)
	PersonByFacetID func(context.Context, string) (int64, bool, error)
	// CountBrowsable is the indexed library size. The view list uses it instead
	// of browsing titles.
	CountBrowsable func(context.Context, core.MediaType) (int, error)
	// CreditItems enumerates cached credit-title keys without browsing or hydration.
	CreditItems func(context.Context, core.RatingKey, int) ([]core.RatingKey, error)
	// PeopleIndex lists credited people with a name word starting with every
	// term, most credited first. PersonCredits lists a person's most popular
	// credited titles of the given types. With both, people searches use the
	// name index instead of walking every credited title. Optional.
	PeopleIndex   func(context.Context, []string, int) ([]PersonCandidate, error)
	PersonCredits func(context.Context, int64, []core.MediaType, int) ([]core.RatingKey, error)
	// PersonCreditsMany is PersonCredits for a page of people, with each
	// person's credit rows (name, role, portrait) on those titles. With it
	// (and PeopleRead.VisibleTitles) a people search reads a page of people
	// in a few statements. Optional.
	PersonCreditsMany func(context.Context, []int64, []core.MediaType, int) (map[int64][]core.PersonCredit, error)
	// PersonCreditForms lists the forms (name, portrait) a person's credits
	// take, and PersonFormKeys pages, by key, the titles crediting them in
	// some of those forms. With both (and PeopleRead.PortraitTitles) a person
	// page reads its credited titles in the order its choice prefers them
	// and stops at the first the viewer may see. Optional.
	// RankingDetails reads in full the titles a ranking window
	// (core.WithRankingCandidates) returned partial. With it, Similar ranks
	// its related shelves on the facts their ranking reads and reads only
	// its page in full. Optional.
	RankingDetails    func(context.Context, []core.Title) ([]core.Title, error)
	PersonMetadata    func(context.Context, int64, bool) core.PersonMetadata
	LibraryMembers    func(context.Context, core.LibraryID, []core.RatingKey) (map[core.RatingKey]bool, int64, bool, error)
	PersonCreditForms func(context.Context, int64, []core.MediaType) ([]core.PersonCreditForm, error)
	PersonFormKeys    func(context.Context, int64, []core.MediaType, []core.PersonCreditForm, core.RatingKey, int) ([]core.RatingKey, error)
	// These readers belong to the current people policy proof.
	peopleParentTitles          parentTitleReader
	peopleVisibleTitles         func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)
	peoplePortraitTitles        func(context.Context, []PersonCreditRef) (map[core.RatingKey]core.Title, error)
	peopleRecheckPortraitTitles func(context.Context, []PersonCreditRef) (map[core.RatingKey]core.Title, error)
	AiredEpisodes               func(context.Context, core.RatingKey, int, int) (core.Page, error)
	// AiredEpisodesMany is AiredEpisodes(show, 0, limit) for several shows in
	// a few statements. Optional; without it each show is read alone.
	AiredEpisodesMany func(context.Context, []core.RatingKey, int) (map[core.RatingKey]core.Page, error)
	// AiredEpisodeTallies counts each show's aired episodes and the viewer's
	// plays of them by season (catalog.AiredEpisodeTallies). With it and
	// EpisodesSettledBefore, unplayed badges are counted, not listed.
	// Optional.
	AiredEpisodeTallies func(ctx context.Context, userID int64, shows []core.RatingKey, settled time.Time, judge []core.RatingKey) (map[core.RatingKey]core.AiredEpisodeTally, error)
	// EpisodesSettledBefore is the release time at or before which
	// Visibility.FilterAvailable keeps every episode; zero when it keeps
	// them all. Required with a FilterAvailable for counted badges.
	EpisodesSettledBefore func(context.Context) time.Time
	// StoredEpisodes answers a recursive episode listing from the stored
	// episodes in key order. Optional; without it the listing walks the
	// library.
	StoredEpisodes StoredEpisodeDeps
	// PlayedEpisodes answers a recursive, episodes-only played listing
	// (Filters=IsPlayed, SortBy=DatePlayed) from the viewer's watch rows.
	// Optional; without it the listing walks the library.
	PlayedEpisodes PlayedEpisodeDeps
	// FavoriteEpisodes answers a recursive, episodes-only favorite listing
	// (Filters=IsFavorite, IncludeItemTypes=Episode) from the viewer's favorite
	// rows, in episode key order. Optional; without it the listing reads the
	// favorite keys in process.
	FavoriteEpisodes FavoriteEpisodeDeps
	AddedTimes       func(context.Context, []core.RatingKey) (map[core.RatingKey]time.Time, error)
	// RecentAir is the indexed date-added window. Nil keeps the catalogue scan.
	RecentAir func(context.Context, core.MediaType, int, int) ([]core.Title, bool, error)
	// CountUnplayedAired counts aired episodes with no play. Nil keeps the episode list.
	CountUnplayedAired func(context.Context, int64, []core.RatingKey, string) (map[core.RatingKey]int, error)
	SimilarSource      func(context.Context, core.RatingKey, int) ([]core.Title, error)
	// Related is the same detail recommender Plex uses. Jellyfin and Emby
	// Similar flatten its policy-filtered rows.
	Related               core.Recommender
	FilterRecommendations func(context.Context, *core.User, []core.Title) ([]core.Title, error)
	Collections           core.Collections
	// Franchises answers page-first TMDB franchise listing, set-based
	// visibility counts, metadata and fallback artwork from the indexed
	// catalog_franchises summary table. Optional; tests with fake
	// core.Collections leave it nil.
	Franchises             FranchiseStore
	CollectionMemberCounts func(context.Context, core.Filter, []int64) (map[int64]int, error)
	Playlists              core.Playlists
	// Scheduled lists metadata-only future episodes for one season. Nil means
	// the host has not opted in. Callers must not resolve a release from it.
	Scheduled func(context.Context, core.RatingKey) ([]core.Title, error)
	// Grouped reports shows whose episode order is not the canonical season
	// tree. Future episodes stay off those shows.
	Grouped func(context.Context, *core.User, []core.RatingKey) (map[core.RatingKey]bool, error)
	State   core.State
	// HistoryPlays lists a viewer's completed plugin plays in [from, to),
	// oldest first (listening recaps). Optional.
	HistoryPlays func(ctx context.Context, userID int64, from, to time.Time, limit int) ([]core.HistoryPlay, error)
	// Per-viewer duplicate preferences (optional): a preferred release per
	// recording and the releases the viewer hid in its favour.
	RecordingPreferences   func(ctx context.Context, userID int64, library core.LibraryID, recordings []string) (map[string]core.RatingKey, error)
	SetRecordingPreference func(ctx context.Context, userID int64, library core.LibraryID, recording string, preferred core.RatingKey, hide []core.RatingKey) error
	HiddenRecordingCopies  func(ctx context.Context, userID int64) ([]core.RatingKey, error)
	Search                 Search
	Rows                   *RowsService
	Layout                 core.NeutralHomeLayoutSource
	Protocol               core.Protocol
	// Favorites manages the stable, per-viewer Favorites playlist mapping.
	Favorites      func(context.Context, *core.User, core.RatingKey, bool) (bool, error)
	IsFavorite     func(context.Context, *core.User, core.RatingKey) (bool, error)
	IsFavoriteMany func(context.Context, *core.User, []core.RatingKey) (map[core.RatingKey]bool, error)
	// FavoriteTitleKeys lists up to n of a viewer's favorite titles and
	// whether that is all of them, so a favorites filter seeks those titles
	// instead of walking the library. Optional.
	FavoriteTitleKeys func(context.Context, *core.User, int) ([]core.RatingKey, bool, error)
	// FavoritePage reads one page of a viewer's liked titles of one role:
	// keys in order, when each was liked, the next cursor and the total.
	// Optional; without it FavoriteTitles is unavailable.
	FavoritePage func(ctx context.Context, viewer *core.User, role, cursor string, limit int) (keys []core.RatingKey, likedAt []time.Time, next string, total int, err error)
	// CountWatched counts the titles matching a filter that a viewer has
	// played, from their watch rows. Optional; without it a played filter's
	// total is counted over the whole type.
	CountWatched func(context.Context, core.Filter, int64) (int, error)
	// WatchedTitles reports which keys a viewer has fully watched, matching
	// Browse's watched predicate for shows and seasons. Optional.
	WatchedTitles           func(context.Context, int64, []core.RatingKey) (map[core.RatingKey]bool, error)
	IsNotInterestedMany     func(context.Context, *core.User, []core.RatingKey) (map[core.RatingKey]bool, error)
	IsWatchlistedMany       func(context.Context, int64, []core.RatingKey) (map[core.RatingKey]bool, error)
	FavoriteMembership      func(context.Context, *core.User, core.RatingKey) (frontend.Membership, error)
	IsNotInterested         func(context.Context, *core.User, core.RatingKey) (bool, error)
	SetNotInterestedSource  func(context.Context, *core.User, core.RatingKey, bool) error
	IsWatchlisted           func(context.Context, *core.User, core.RatingKey) (bool, error)
	WatchlistMembership     func(context.Context, *core.User, core.RatingKey) (frontend.Membership, error)
	SetWatchlistSource      func(context.Context, *core.User, core.RatingKey, bool) error
	RemoveContinue          func(context.Context, *core.User, core.RatingKey) error
	CanFavoriteEntity       func(context.Context, *core.User, frontend.FavoriteRef) bool
	SetEntityFavoriteSource func(context.Context, *core.User, frontend.FavoriteRef, bool) error
	EntityFavoritesSource   func(context.Context, *core.User, []frontend.FavoriteRef) (map[frontend.FavoriteRef]bool, error)

	// Enrich must use cached or estimated data; it must never resolve releases.
	Enrich func(context.Context, *core.User, core.Device, *core.Title) (frontend.ItemDetail, error)

	Catalog             core.Catalog
	Recommender         core.Recommender
	VibeSearch          frontend.VibeSearcher
	SearchSources       frontend.SearchSources
	FavoriteShows       func(context.Context, *core.User) (map[core.RatingKey]bool, error)
	FilterAvailable     func(context.Context, []core.Title) []core.Title
	JudgedOnFacts       func(context.Context) func(core.Title) bool
	PrioritizeAvailable func(context.Context, []core.Title) []core.Title
	EpisodeOrder        frontend.EpisodeOrder
	Discovery           *frontend.DiscoveryFeatures
	Dashboard           frontend.Dashboard
	Resources           frontend.ResourceStatistics
	LibraryGUIDs        frontend.LibraryGUIDs
	LibraryGUIDsFunc    func() frontend.LibraryGUIDs
	LibraryMetadata     frontend.LibraryMetadata
	LibraryMetadataFunc func() frontend.LibraryMetadata
	Markers             frontend.MarkerRanges
	LibraryTitles       func() (movies, shows string)
	HomeRevision        func(context.Context, *core.User) (frontend.HomeRevision, error)
	NoProviderWait      func(context.Context) context.Context
	Now                 func() time.Time
	Log                 *slog.Logger
}

type Service struct {
	d             ServiceDeps
	cursors       *episodeCursors
	pluginCursors *pluginCursorCache
	searchGate    *descriptiveGate
	imports       *importJobs
}

func (s *Service) ensureSearchGate() *descriptiveGate {
	if s.searchGate != nil {
		return s.searchGate
	}
	if s.d.Search.gate != nil {
		return s.d.Search.gate
	}
	return fallbackDescriptiveGate
}

// pageUserData reads watch progress, favorites, not-interested and watchlist
// membership for one page in a single query.
type pageUserData interface {
	PageUserData(context.Context, int64, []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error)
}

func NewService(d ServiceDeps) (*Service, error) {
	if d.Active == nil || d.Get == nil || d.Children == nil || d.State == nil || d.Rows == nil || d.Protocol == "" || d.Visibility.GetTitle == nil || d.Visibility.SectionFor == nil || d.Search.Catalog == nil {
		return nil, errors.New("experience: incomplete service dependencies")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	d.Dashboard, d.Resources = Statistics(d.Dashboard, d.Resources, d.Now)
	d.Libraries = append([]Library(nil), d.Libraries...)
	d.Search.Visibility = d.Visibility
	gate := d.Search.gate
	if gate == nil {
		gate = newDescriptiveGate()
		d.Search.gate = gate
	}
	return &Service{d: d, cursors: &episodeCursors{}, pluginCursors: newPluginCursorCache(), searchGate: gate}, nil
}

func (s *Service) libraries() []Library {
	if s.d.LiveLibraries != nil {
		return s.d.LiveLibraries()
	}
	return s.d.Libraries
}
func (s *Service) viewer(ctx context.Context, v *core.User) (*core.User, error) {
	if v == nil || v.ID <= 0 {
		return nil, core.ErrNotFound
	}
	if memo := frontend.RequestMemoFrom(ctx); memo != nil {
		memo.Mu.Lock()
		if memo.Viewer != nil && memo.Viewer.ID == v.ID {
			active := memo.Viewer
			memo.Mu.Unlock()
			return active, nil
		}
		memo.Mu.Unlock()
	}
	// A surface without a request memo still resolves the viewer once per
	// read request, not once per listed item.
	if s.d.Active == nil {
		return nil, core.ErrNotFound
	}
	active, err := reqmemo.Do(ctx, "experience:active:"+strconv.FormatInt(v.ID, 10), func() (*core.User, error) {
		return s.d.Active(ctx, v.ID)
	})
	if err != nil || active == nil || active.ID != v.ID {
		return nil, core.ErrNotFound
	}
	if memo := frontend.RequestMemoFrom(ctx); memo != nil {
		memo.Mu.Lock()
		if memo.Viewer == nil || memo.Viewer.ID != active.ID {
			memo.Visible = nil
			memo.Aired = nil
		}
		memo.Viewer = active
		memo.Mu.Unlock()
	}
	return active, nil
}
func (s *Service) visible(ctx context.Context, v *core.User, title *core.Title) bool {
	if title == nil || s.d.Visibility.SectionFor == nil {
		return false
	}
	if memo := frontend.RequestMemoFrom(ctx); memo != nil {
		memo.Mu.Lock()
		if seen, ok := memo.Visible[title.Key]; ok {
			memo.Mu.Unlock()
			return seen
		}
		memo.Mu.Unlock()
	}
	seen := s.sectionOrFilterAllowed(ctx, v, title, s.d.Visibility.SectionFor) && withoutSectionCheck(s.d.Visibility).Visible(ctx, v, title)
	if memo := frontend.RequestMemoFrom(ctx); memo != nil {
		memo.Mu.Lock()
		if memo.Visible == nil {
			memo.Visible = map[core.RatingKey]bool{}
		}
		memo.Visible[title.Key] = seen
		memo.Mu.Unlock()
	}
	return seen
}

// visibleCandidates keeps search and related-page authorization in one batch.
func (s *Service) visibleCandidates(ctx context.Context, v *core.User, titles []core.Title) ([]core.Title, error) {
	if len(titles) == 0 {
		return titles, nil
	}
	memo := frontend.RequestMemoFrom(ctx)
	if memo == nil {
		return s.allowTitlesWithParents(ctx, v, titles, s.d.CachedGetMany)
	}
	decisions := make(map[core.RatingKey]bool, len(titles))
	var unknown []core.Title
	memo.Mu.Lock()
	for _, title := range titles {
		if allowed, known := memo.Visible[title.Key]; known {
			decisions[title.Key] = allowed
		} else {
			unknown = append(unknown, title)
			decisions[title.Key] = false
		}
	}
	memo.Mu.Unlock()
	if len(unknown) > 0 {
		allowed, err := s.allowTitlesWithParents(ctx, v, unknown, s.d.CachedGetMany)
		if err != nil {
			return nil, err
		}
		for _, title := range allowed {
			decisions[title.Key] = true
		}
		memo.Mu.Lock()
		if memo.Visible == nil {
			memo.Visible = make(map[core.RatingKey]bool, len(unknown))
		}
		for _, title := range unknown {
			memo.Visible[title.Key] = decisions[title.Key]
		}
		memo.Mu.Unlock()
	}
	out := make([]core.Title, 0, len(titles))
	for _, title := range titles {
		if decisions[title.Key] {
			out = append(out, title)
		}
	}
	return out, nil
}

func (s *Service) playable(ctx context.Context, v *core.User, key core.RatingKey) (*core.Title, error) {
	if key.IsPluginKey() {
		piv, err := s.pluginItem(core.WithNoContainerExpand(ctx), v, key)
		if err != nil {
			return nil, core.ErrNotFound
		}
		return &piv.Title, nil
	}
	t, err := s.d.Get(ctx, v, key)
	if err != nil || t == nil || t.Key != key || s.d.Visibility.SectionFor == nil {
		return nil, core.ErrNotFound
	}
	if !s.sectionOrFilterAllowed(ctx, v, t, s.d.Visibility.SectionFor) || !withoutSectionCheck(s.d.Visibility).Playable(ctx, v, t) {
		return nil, core.ErrNotFound
	}
	return t, nil
}
func (s *Service) item(ctx context.Context, v *core.User, key core.RatingKey) (*core.Title, error) {
	if key.IsPluginKey() {
		piv, err := s.pluginItem(core.WithNoContainerExpand(ctx), v, key)
		if err != nil {
			return nil, core.ErrNotFound
		}
		return &piv.Title, nil
	}
	t, err := s.d.Get(ctx, v, key)
	if err != nil || t == nil || t.Key != key || !s.visible(ctx, v, t) {
		return nil, core.ErrNotFound
	}
	return t, nil
}
func (s *Service) Views(ctx context.Context, viewer *core.Viewer) ([]frontend.View, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	if s.d.PluginLibraries != nil {
		return s.storedViews(ctx, v)
	}
	libs := s.libraries()
	out := make([]frontend.View, 0, len(libs))
	for _, lib := range libs {
		if SectionAllowed(v, lib.ID) {
			out = append(out, frontend.View{ID: strconv.Itoa(lib.ID), LibraryID: core.LibraryID(lib.ID), Name: lib.Name, Type: lib.Type, Virtual: lib.Virtual})
		}
	}
	return out, nil
}

// LibraryCount is the indexed size of one library. It does not browse or
// hydrate titles. A viewer who cannot open the library gets zero.
func (s *Service) LibraryCount(ctx context.Context, viewer *core.Viewer, viewID string) (int, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if s == nil {
		return 0, errors.New("experience: library count unavailable")
	}
	if s.d.PluginLibraries != nil {
		v, err := s.viewer(ctx, viewer)
		if err != nil {
			return 0, err
		}
		id, err := strconv.Atoi(viewID)
		if err != nil {
			return 0, core.ErrNotFound
		}
		lib, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(id))
		if err != nil {
			return 0, core.ErrNotFound
		}
		mt, _, ok := s.publishedLibrary(ctx, lib)
		if !ok {
			return 0, core.ErrNotFound
		}
		if !v.AllowsLibrary(lib.ID) {
			return 0, nil
		}
		switch lib.Source {
		case core.LibraryPlugin:
			return s.d.PluginLibraries.Count(ctx, lib)
		case core.LibraryBuiltin, core.LibraryCatalog, core.LibraryCollection:
			page, err := s.LibraryItems(ctx, v, core.Device{}, viewID, frontend.ItemQuery{Limit: 1})
			if err != nil {
				return 0, err
			}
			return page.Total, nil
		default:
			if s.d.CountBrowsable == nil {
				return 0, errors.New("experience: library count unavailable")
			}
			return s.d.CountBrowsable(ctx, mt)
		}
	}
	if s.d.CountBrowsable == nil {
		return 0, errors.New("experience: library count unavailable")
	}
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return 0, err
	}
	var library Library
	var found bool
	for _, lib := range s.libraries() {
		if strconv.Itoa(lib.ID) == viewID {
			library, found = lib, true
			break
		}
	}
	if !found {
		return 0, core.ErrNotFound
	}
	if !SectionAllowed(v, library.ID) {
		return 0, nil
	}
	return s.d.CountBrowsable(ctx, library.Type)
}

// LibraryItems scans catalogue pages and filters before applying the requested page.
func (s *Service) LibraryItems(ctx context.Context, viewer *core.Viewer, device core.Device, viewID string, q frontend.ItemQuery) (core.Page, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if q.StartAt != 0 && q.AdjacentTo != 0 {
		return core.Page{}, errors.New("experience: incompatible item anchors")
	}
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return core.Page{}, err
	}
	// A request whose IncludeItemTypes names only kinds no accessible library
	// can present can never match: answer empty without listing #405's
	// Person/Music* favorites asks used to scan the whole TV library.
	if !s.requestedTypesServable(ctx, v, q) {
		return core.Page{Items: []core.Title{}, Total: 0, TotalExact: true}, nil
	}
	if q.FilterByProviderIDs {
		return s.providerIDLibraryItems(ctx, v, viewID, q)
	}
	if viewID == "" {
		if q.Sort == "" {
			q.Sort = core.SortPopularity
			q.Desc = true
		}
		if q.AdjacentTo == 0 && q.StartAt == 0 && q.Limit > 0 {
			return s.rootWindow(ctx, v, device, q)
		}
		all := make([]core.Title, 0)
		inner := q
		inner.Offset = 0
		inner.Limit = 0
		inner.AdjacentTo = 0
		inner.StartAt = 0
		inner.Sort = ""
		inner.Desc = false
		if s.d.PluginLibraries != nil {
			views, e := s.storedViews(ctx, v)
			if e != nil {
				return core.Page{}, e
			}
			for _, view := range views {
				page, e := s.LibraryItems(ctx, v, device, view.ID, inner)
				if e != nil {
					return core.Page{}, e
				}
				all = append(all, page.Items...)
			}
		} else {
			for _, lib := range s.libraries() {
				if !SectionAllowed(v, lib.ID) {
					continue
				}
				page, e := s.LibraryItems(ctx, v, device, strconv.Itoa(lib.ID), inner)
				if e != nil {
					return core.Page{}, e
				}
				all = append(all, page.Items...)
			}
		}
		if err = s.sortTitles(ctx, all, q); err != nil {
			return core.Page{}, err
		}
		if q.AdjacentTo != 0 {
			return adjacentTitles(all, q)
		}
		if q.StartAt != 0 {
			return startingTitles(all, q)
		}
		total := len(all)
		start := min(max(q.Offset, 0), total)
		end := total
		if q.Limit > 0 {
			end = min(start+q.Limit, total)
		}
		return core.Page{Items: all[start:end], Total: total}, nil
	}
	id, err := strconv.Atoi(viewID)
	if err != nil {
		return core.Page{}, core.ErrNotFound
	}
	var library *Library
	var stored *core.Library
	if s.d.PluginLibraries != nil {
		lib, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(id))
		if err != nil {
			return core.Page{}, core.ErrNotFound
		}
		stored = &lib
		// A collection library is browsed typed whatever protocol
		// presentations it has yet (a new library is console-only and
		// opts into apps later, §15.3); every other source must be
		// published to the protocol asking.
		mt := filterMediaType(lib)
		if lib.Source != core.LibraryCollection {
			var ok bool
			mt, _, ok = s.publishedLibrary(ctx, lib)
			if !ok {
				return core.Page{}, core.ErrNotFound
			}
		} else if lib.Hidden {
			return core.Page{}, core.ErrNotFound
		}
		if !v.AllowsLibrary(lib.ID) {
			return core.Page{}, core.ErrNotFound
		}
		if lib.Source == core.LibraryPlugin {
			// A catalogue (TMDB) person is credited on no plugin item: plugin
			// people carry their own ids. Answer empty instead of the whole
			// library, which put every audiobook on an actor's page and merged
			// a plugin movie library into their filmography.
			if wantsCataloguePerson(q.Filter) {
				return core.Page{Items: []core.Title{}, Total: 0, TotalExact: true}, nil
			}
			return s.pluginLibraryItems(ctx, v, lib, s.sanitizePluginLibraryQuery(ctx, lib, q))
		}
		if lib.Source == core.LibraryCollection && lib.CollectionID <= 0 {
			return core.Page{Items: []core.Title{}, Total: 0, TotalExact: true}, nil
		}
		if s.d.Browse == nil {
			return core.Page{}, core.ErrNotFound
		}
		q.Filter = s.libraryBrowseFilter(ctx, v, lib, q.Filter)
		library = &Library{ID: int(lib.ID), Name: lib.Name, Type: mt, Virtual: true}
	} else {
		libs := s.libraries()
		for i := range libs {
			if libs[i].ID == id {
				library = &libs[i]
				break
			}
		}
		if library == nil || !SectionAllowed(v, id) || s.d.Browse == nil {
			return core.Page{}, core.ErrNotFound
		}
	}
	if q.CollectionKey != 0 {
		return s.libraryCollectionItems(ctx, v, device, library, stored, q)
	}
	// Recursive show browsing can produce the show, its seasons, and its episodes.
	// Skip the library when the requested types cannot appear in that tree.
	if q.Recursive && library.Type == core.TypeShow && len(q.Types) > 0 {
		possible := false
		for _, typ := range q.Types {
			if typ == core.TypeShow || typ == core.TypeSeason || typ == core.TypeEpisode {
				possible = true
				break
			}
		}
		if !possible {
			if q.StartAt != 0 || q.AdjacentTo != 0 {
				return core.Page{}, core.ErrNotFound
			}
			return core.Page{Items: []core.Title{}, Total: 0, TotalExact: true}, nil
		}
	}
	wantShowChildren := q.Recursive && (len(q.Types) == 0 || containsMediaType(q.Types, core.TypeSeason))
	wantShowEpisodes := q.Recursive && (len(q.Types) == 0 || containsMediaType(q.Types, core.TypeEpisode))
	var permitted []core.Title
	totalMatched := 0
	bounded := !q.Recursive && q.AdjacentTo == 0 && q.StartAt == 0 && q.Limit > 0
	expand := q.Recursive && library.Type == core.TypeShow
	// A plain library probe (the view list asks for one title) can use the
	// catalogue total when this viewer has no extra visibility filter. The
	// browse total already excludes adult and hidden titles.
	plainTotal := bounded && !expand && q.Watched == nil && q.Favorite == nil && len(q.Types) == 0 && q.Season == nil && plainItemFilter(q.Filter) && v.MaxContentRating == "" && (v.IsOwner || len(v.AllowedLibraries) == 0) && s.d.Visibility.FilterAvailable == nil && s.d.Visibility.Policy == nil
	// Recursive episode queries keep at most one bounded batch of visible,
	// statically matching candidates in addition to the existing results.
	var recursiveBatch []core.Title
	flushRecursive := func() error {
		if len(recursiveBatch) == 0 {
			return nil
		}
		chunk := recursiveBatch
		var watched map[core.RatingKey]bool
		if q.Watched != nil {
			var e error
			watched, e = s.titlesWatched(ctx, v, chunk)
			if e != nil {
				return e
			}
		}
		// Compact the owned batch in place. Writes only target earlier slots,
		// and no title pointers escape the batch.
		eligible := chunk[:0]
		for _, title := range chunk {
			if q.Watched == nil || watched[title.Key] == *q.Watched {
				eligible = append(eligible, title)
			}
		}
		var favorites map[core.RatingKey]bool
		if q.Favorite != nil && s.d.IsFavoriteMany != nil && len(eligible) > 0 {
			keys := make([]core.RatingKey, len(eligible))
			for i := range eligible {
				keys[i] = eligible[i].Key
			}
			var e error
			favorites, e = s.d.IsFavoriteMany(ctx, v, keys)
			if e != nil {
				return e
			}
		}
		for _, title := range eligible {
			if q.Favorite != nil {
				var yes bool
				if s.d.IsFavoriteMany != nil {
					yes = favorites[title.Key]
				} else {
					if s.d.IsFavorite == nil {
						return errors.New("experience: favorites unavailable")
					}
					var e error
					yes, e = s.d.IsFavorite(ctx, v, title.Key)
					if e != nil {
						return e
					}
				}
				if yes != *q.Favorite {
					continue
				}
			}
			permitted = append(permitted, title)
		}
		clear(recursiveBatch)
		recursiveBatch = recursiveBatch[:0]
		return nil
	}
	if page, ok, err := s.favoriteEpisodePage(ctx, v, library, 0, q); err != nil {
		return core.Page{}, err
	} else if ok {
		return page, nil
	}
	if page, ok, err := s.favoriteRecursivePage(ctx, v, library, 0, q); err != nil {
		return core.Page{}, err
	} else if ok {
		return page, nil
	}
	if page, ok, err := s.storedEpisodePage(ctx, v, library, q); err != nil {
		return core.Page{}, err
	} else if ok {
		return page, nil
	}
	if page, ok, err := s.playedEpisodePage(ctx, v, library, 0, q); err != nil {
		return core.Page{}, err
	} else if ok {
		return page, nil
	}
	if page, ok, err := s.indexedRecursivePage(ctx, v, library, q); err != nil {
		return core.Page{}, err
	} else if ok {
		return page, nil
	}
	if page, ok, err := s.indexedLibraryPage(ctx, v, library, q); err != nil {
		return core.Page{}, err
	} else if ok {
		return page, nil
	} else if stored != nil && stored.Source == core.LibraryCollection && !expand && !indexedBrowseQuery(library, q) {
		// A sort or request shape SQL cannot page uses the bounded
		// in-process walk; an indexed page already returned above.
		return s.collectionLibraryScan(ctx, v, library, q, stored.CollectionID)
	}
	if expand && wantShowEpisodes && s.d.Grandchildren == nil {
		return core.Page{}, errors.New("experience: recursive library unavailable")
	}
	if expand && wantShowChildren && s.d.Children == nil {
		return core.Page{}, errors.New("experience: recursive library unavailable")
	}
	for offset := 0; ; {
		filter := q.Filter
		if expand {
			filter = scopedShowFilter(q.Filter, nil)
		}
		filter.Type = library.Type
		if v.MaxContentRating != "" && (filter.MaxContentRating == "" || int(contentrating.EnforceLevelOf(v.MaxContentRating)) < int(contentrating.EnforceLevelOf(filter.MaxContentRating))) {
			filter.MaxContentRating = v.MaxContentRating
		}
		bq := core.BrowseQuery{Filter: filter, Sort: q.Sort, Desc: q.Desc, Offset: offset, Limit: 500}
		if q.Sort.IsLastViewed() {
			bq.Sort = core.SortTitle
			bq.SortLastViewed = true
			bq.SortViewerID = v.ID
		}
		page, err := s.d.Browse(ctx, bq)
		if err != nil {
			return core.Page{}, err
		}
		var candidatesPage []core.Title
		pageSectionAllowed := s.sectionOrFilterAllowedBatch(ctx, v, page.Items, s.d.Visibility.SectionFor)
		accept := func(title core.Title, vis Visibility, sectionAllowed map[core.RatingKey]bool) error {
			if !sectionAllowed[title.Key] || !withoutSectionCheck(vis).Visible(ctx, v, &title) {
				return nil
			}
			if !expand {
				candidatesPage = append(candidatesPage, title)
				return nil
			}
			staticQuery := q
			staticQuery.Filter.Type = ""
			staticQuery.Watched, staticQuery.Favorite = nil, nil
			ok, e := s.matchesUserQuery(ctx, v, title, staticQuery)
			if e != nil {
				return e
			}
			if ok {
				recursiveBatch = append(recursiveBatch, title)
				if len(recursiveBatch) == 400 {
					return flushRecursive()
				}
			}
			return nil
		}
		for i := range page.Items {
			parent := &page.Items[i]
			if !s.visible(ctx, v, parent) {
				continue
			}
			var children []core.Title
			if expand && parent.Type == core.TypeShow && wantShowEpisodes {
				var e error
				children, e = s.d.Grandchildren(ctx, v, parent.Key)
				if e != nil {
					return core.Page{}, e
				}
			}
			if expand && parent.Type == core.TypeShow && wantShowChildren {
				seasons, e := s.d.Children(ctx, v, parent.Key)
				if e != nil {
					return core.Page{}, e
				}
				children = append(children, seasons...)
			}
			// Copy only accepted values into the bounded batch; do not duplicate
			// or retain the catalogue's full per-show children slice.
			if e := accept(*parent, s.d.Visibility, pageSectionAllowed); e != nil {
				return core.Page{}, e
			}
			for start := 0; start < len(children); start += 400 {
				chunk := children[start:min(start+400, len(children))]
				vis := s.d.Visibility
				if s.d.ParentTitles != nil {
					keys := make([]core.RatingKey, 0, len(chunk))
					seen := make(map[core.RatingKey]bool)
					for _, child := range chunk {
						if child.ShowKey != 0 && !seen[child.ShowKey] {
							seen[child.ShowKey] = true
							keys = append(keys, child.ShowKey)
						}
					}
					parents, e := s.d.ParentTitles(ctx, keys)
					if e != nil {
						return core.Page{}, e
					}
					vis.GetTitle = func(_ context.Context, key core.RatingKey) (*core.Title, error) {
						title, ok := parents[key]
						if !ok || title.Key != key {
							return nil, core.ErrNotFound
						}
						return &title, nil
					}
				}
				chunkSectionAllowed := s.sectionOrFilterAllowedBatch(ctx, v, chunk, vis.SectionFor)
				for j := range chunk {
					if e := accept(chunk[j], vis, chunkSectionAllowed); e != nil {
						return core.Page{}, e
					}
				}
			}
		}
		for start := 0; start < len(candidatesPage); start += 400 {
			chunk := candidatesPage[start:min(start+400, len(candidatesPage))]
			var watched map[core.RatingKey]bool
			if q.Watched != nil {
				watched, err = s.titlesWatched(ctx, v, chunk)
				if err != nil {
					return core.Page{}, err
				}
			}
			query := q
			query.Watched = nil
			var favorites map[core.RatingKey]bool
			if q.Favorite != nil && s.d.IsFavoriteMany != nil {
				keys := make([]core.RatingKey, len(chunk))
				for i := range chunk {
					keys[i] = chunk[i].Key
				}
				favorites, err = s.d.IsFavoriteMany(ctx, v, keys)
				if err != nil {
					return core.Page{}, err
				}
				query.Favorite = nil
			}
			for _, title := range chunk {
				if q.Watched != nil && watched[title.Key] != *q.Watched {
					continue
				}
				if q.Favorite != nil && s.d.IsFavoriteMany != nil && favorites[title.Key] != *q.Favorite {
					continue
				}
				ok, e := s.matchesUserQuery(ctx, v, title, query)
				if e != nil {
					return core.Page{}, e
				}
				if !ok {
					continue
				}
				if !bounded || (totalMatched >= max(q.Offset, 0) && len(permitted) < q.Limit) {
					permitted = append(permitted, title)
				}
				totalMatched++
			}
		}
		if plainTotal && len(candidatesPage) == len(page.Items) && totalMatched == len(candidatesPage) && len(permitted) >= q.Limit {
			return core.Page{Items: permitted[:q.Limit], Total: page.Total, TotalExact: page.TotalExact}, nil
		}
		offset += len(page.Items)
		if offset >= page.Total {
			break
		}
		if len(page.Items) == 0 {
			return core.Page{}, errors.New("experience: incomplete catalogue page")
		}
		if err = ctx.Err(); err != nil {
			return core.Page{}, err
		}
	}
	if expand {
		if err := flushRecursive(); err != nil {
			return core.Page{}, err
		}
	}
	if bounded {
		return core.Page{Items: permitted, Total: totalMatched, TotalExact: true}, nil
	}
	if expand {
		if err := s.sortTitles(ctx, permitted, q); err != nil {
			return core.Page{}, err
		}
	}
	if q.AdjacentTo != 0 {
		return adjacentTitles(permitted, q)
	}
	if q.StartAt != 0 {
		return startingTitles(permitted, q)
	}
	total := len(permitted)
	start := min(max(q.Offset, 0), total)
	end := total
	if q.Limit > 0 {
		end = min(start+q.Limit, total)
	}
	return core.Page{Items: permitted[start:end], Total: total, TotalExact: true}, nil
}

func plainItemFilter(f core.Filter) bool {
	return f.Group == nil && !f.KnownAddedDate && f.IncludeKeys == nil && len(f.ExcludeKeys) == 0 && f.Type == "" && len(f.Genres) == 0 && f.YearFrom == 0 && f.YearTo == 0 && len(f.Years) == 0 && len(f.ExcludeYears) == 0 && len(f.ReleasedFilters) == 0 && f.MinRating == 0 && f.MaxRuntime == 0 && f.MinRuntime == 0 && len(f.Languages) == 0 && len(f.Countries) == 0 && len(f.ContentRatings) == 0 && f.MaxContentRating == "" && f.Collection == 0 && f.Person == 0 && f.Studio == "" && f.FirstCharacter == "" && f.NameStartsWith == "" && f.NameStartsWithOrGreater == "" && f.NameLessThan == "" && f.Query == ""
}

// filterHasLibraryScope reports whether f carries a library, tab, preview,
// catalogue-source or collection scope that narrows the admitted series.
func filterHasLibraryScope(f core.Filter) bool {
	return len(f.ExtraRules) > 0 || (f.ExtraFallback != "" && f.ExtraFallback != "show") ||
		len(f.TabRules) > 0 || (f.TabFallback != "" && f.TabFallback != "show") ||
		len(f.PreviewRules) > 0 || (f.PreviewFallback != "" && f.PreviewFallback != "show") ||
		(f.RuleVisibility != "" && f.RuleVisibility != "all") || f.ApplyCanonicalInPreview ||
		len(f.CatalogueSources) > 0 || f.CollectionMembers != 0
}

// scopedShowFilter extracts the series-scoping fields of a library/tab filter
// onto a TypeShow browse filter (with the viewer's content-rating cap when v
// is non-nil).
func scopedShowFilter(f core.Filter, v *core.User) core.Filter {
	show := core.Filter{
		Type:                    core.TypeShow,
		RuleVisibility:          f.RuleVisibility,
		ApplyCanonicalInPreview: f.ApplyCanonicalInPreview,
		PreviewRules:            f.PreviewRules,
		PreviewFallback:         f.PreviewFallback,
		ExtraRules:              f.ExtraRules,
		ExtraFallback:           f.ExtraFallback,
		TabRules:                f.TabRules,
		TabFallback:             f.TabFallback,
		CatalogueSources:        f.CatalogueSources,
		CollectionMembers:       f.CollectionMembers,
	}
	if v != nil {
		show.MaxContentRating = v.MaxContentRating
	}
	return show
}

// plainRecursiveFilter is plainItemFilter for a recursive library listing:
// libraryBrowseFilter stamps a custom library's own type on the request
// filter, and the indexed page overwrites Type per child type it reads, so
// that stamp alone must not send the listing into the whole-library walk.
func plainRecursiveFilter(f core.Filter, libraryType core.MediaType) bool {
	if f.Type == libraryType {
		f.Type = ""
	}
	return plainItemFilter(f)
}

func (s *Service) Rows(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
	tv, err := s.viewer(ctx, req.Viewer)
	if err != nil {
		return nil, err
	}
	req.Viewer = tv
	if req.Placement == frontend.PlacementDetail && req.Item.IsPluginKey() {
		return s.pluginDetailRows(ctx, tv, req)
	}
	return s.d.Rows.Rows(ctx, req, s.d.Protocol)
}

// SnapshotRows is the same authorized Home as Rows without issuing impressions.
// A second surface that paints those rows, such as Silo Discover, uses it so
// the bandit is not charged twice for one visit.
func (s *Service) SnapshotRows(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
	tv, err := s.viewer(ctx, req.Viewer)
	if err != nil {
		return nil, err
	}
	req.Viewer = tv
	if req.Placement == frontend.PlacementDetail && req.Item.IsPluginKey() {
		return s.pluginDetailRows(ctx, tv, req)
	}
	return s.d.Rows.SnapshotRows(ctx, req, s.d.Protocol)
}

func pluginPlayRead(piv PluginItemView) (canPlay *bool, canRead *bool) {
	isBook := piv.Title.MediaClass == core.MediaClassBook || piv.Title.MBItemType == "Book"
	playable := piv.Playable && !isBook
	canPlay = &playable
	if isBook {
		readable := piv.Playable && piv.Unavailable == nil
		canRead = &readable
	}
	return canPlay, canRead
}

// StoredItem reads a title already in the catalogue. Resume, Next Up, and
// other lists use it so one stale show cannot refresh from a provider.
func (s *Service) StoredItem(ctx context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
	v, err := s.viewer(ctx, req.Viewer)
	if err != nil {
		return frontend.ItemDetail{}, err
	}
	if req.Key.IsPluginKey() {
		itemCtx := ctx
		if !core.ColdDetail(ctx, req.Key) {
			itemCtx = core.WithNoContainerExpand(ctx)
		}
		piv, err := s.pluginItem(itemCtx, v, req.Key)
		if err != nil {
			return frontend.ItemDetail{}, err
		}
		canPlay, canRead := pluginPlayRead(piv)
		return frontend.ItemDetail{
			Title:               piv.Title,
			Images:              piv.Images,
			ImagesAuthoritative: len(piv.Images) > 0,
			CanPlay:             canPlay,
			CanRead:             canRead,
			Unavailable:         piv.Unavailable,
		}, nil
	}
	if s.d.CachedGet == nil {
		return frontend.ItemDetail{}, core.ErrNotAvailable
	}
	title, err := s.d.CachedGet(ctx, req.Key)
	if err != nil || title == nil || title.Key != req.Key {
		return frontend.ItemDetail{}, core.ErrNotFound
	}
	visibility := s.d.Visibility
	visibility.GetTitle = s.d.CachedGet
	if visibility.SectionFor == nil {
		return frontend.ItemDetail{}, core.ErrNotFound
	}
	if !s.sectionOrFilterAllowed(ctx, v, title, visibility.SectionFor) || !withoutSectionCheck(visibility).Visible(ctx, v, title) {
		return frontend.ItemDetail{}, core.ErrNotFound
	}
	if title.ShowKey != 0 && title.ShowTitle == "" {
		show, e := s.d.CachedGet(ctx, title.ShowKey)
		if e == nil && show != nil && show.Key == title.ShowKey {
			if s.sectionOrFilterAllowed(ctx, v, show, visibility.SectionFor) && withoutSectionCheck(visibility).Visible(ctx, v, show) {
				copy := *title
				copy.ShowTitle = show.Title
				if copy.ShowYear == 0 {
					copy.ShowYear = show.Year
				}
				title = &copy
			}
		}
	}
	return frontend.ItemDetail{Title: *title}, nil
}

func (s *Service) Item(ctx context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
	ctx = s.NoProviderWait(ctx)
	v, err := s.viewer(ctx, req.Viewer)
	if err != nil {
		return frontend.ItemDetail{}, err
	}
	if req.Key.IsPluginKey() {
		piv, err := s.pluginItem(ctx, v, req.Key)
		if err != nil {
			return frontend.ItemDetail{}, err
		}
		canPlay, canRead := pluginPlayRead(piv)
		d := frontend.ItemDetail{
			Title:               piv.Title,
			Images:              piv.Images,
			ImagesAuthoritative: len(piv.Images) > 0,
			Chapters:            append([]core.Chapter(nil), piv.Chapters...),
			CanPlay:             canPlay,
			CanRead:             canRead,
			Unavailable:         piv.Unavailable,
		}
		if s.d.PluginProbedMedia != nil {
			if info, ok := s.d.PluginProbedMedia(ctx, v, piv.Title); ok && !info.Estimated {
				if len(info.Chapters) > 0 {
					d.Chapters = append([]core.Chapter(nil), info.Chapters...)
				}
				if len(info.Streams) > 0 {
					d.Tracks = append([]core.StreamInfo(nil), info.Streams...)
					d.TracksEstimated = false
				}
			}
		}
		d.UserData, err = s.d.State.Get(ctx, v.ID, piv.Title.Key)
		if err != nil {
			return frontend.ItemDetail{}, err
		}
		if s.d.IsFavorite != nil {
			if fav, ferr := s.d.IsFavorite(ctx, v, piv.Title.Key); ferr != nil {
				s.logPluginEnrichError("favorites", piv.Title.Key, ferr)
			} else {
				d.UserData.IsFavorite = fav
			}
		}
		if s.d.IsNotInterested != nil {
			if ni, nerr := s.d.IsNotInterested(ctx, v, piv.Title.Key); nerr != nil {
				s.logPluginEnrichError("not_interested", piv.Title.Key, nerr)
			} else {
				d.UserData.IsNotInterested = ni
			}
		}
		if s.d.IsWatchlisted != nil {
			if wl, werr := s.d.IsWatchlisted(ctx, v, piv.Title.Key); werr != nil {
				s.logPluginEnrichError("watchlist", piv.Title.Key, werr)
			} else {
				d.UserData.InWatchlist = wl
			}
		}
		return d, nil
	}
	if req.Key.Kind() == core.KindCollection || req.Key.Kind() == core.KindSmart {
		if s.d.Collections == nil {
			return frontend.ItemDetail{}, core.ErrNotFound
		}
		c, err := s.d.Collections.Get(ctx, v, req.Key)
		if err != nil || c == nil || c.Key != req.Key || !s.d.Visibility.CollectionAllowed(v, c) {
			return frontend.ItemDetail{}, core.ErrNotFound
		}
		items, err := s.collectionItems(ctx, v, req.Key)
		if err != nil {
			return frontend.ItemDetail{}, err
		}
		copy := *c
		copy.Filter = nil // Smart rules may contain titles or people hidden from this viewer.
		copy.ChildCount = len(items)
		detail := frontend.ItemDetail{Collection: &copy}
		ref := frontend.FavoriteRef{Kind: frontend.FavoriteCollection, ID: req.Key.String()}
		if s.d.EntityFavoritesSource != nil && s.entityAllowed(ctx, v, ref) {
			values, e := s.d.EntityFavoritesSource(ctx, v, []frontend.FavoriteRef{ref})
			if e != nil {
				return frontend.ItemDetail{}, e
			}
			detail.UserData.IsFavorite = values[ref]
		}
		return detail, nil
	}
	if req.Key.Kind() == core.KindPlaylist {
		if s.d.Playlists == nil {
			return frontend.ItemDetail{}, core.ErrNotFound
		}
		p, err := s.d.Playlists.Get(ctx, v.ID, req.Key)
		if err != nil || p == nil || p.UserID != v.ID || p.Key != req.Key {
			return frontend.ItemDetail{}, core.ErrNotFound
		}
		titles, err := s.loadVisiblePlaylistTitles(ctx, v, p.Items)
		if err != nil {
			return frontend.ItemDetail{}, err
		}
		copy := *p
		copy.Items, _ = playlistVisibleEntries(p.Items, titles)
		copy.Audio = audioPlaylist(copy.Items, titles)
		copy.ItemIDs = nil
		return frontend.ItemDetail{Playlist: &copy}, nil
	}
	title, err := s.item(ctx, v, req.Key)
	if err != nil {
		if ep := s.scheduledTitle(ctx, v, req.Key); ep != nil {
			playable := false
			return frontend.ItemDetail{Title: *ep, CanPlay: &playable}, nil
		}
		return frontend.ItemDetail{}, err
	}
	// Older cached child records may omit their series label even though the
	// parent show is known. Fill it only on this neutral detail path, using a
	// cache-only lookup and the same viewer policy as the requested item.
	if title.ShowKey != 0 && title.ShowTitle == "" && s.d.CachedGet != nil {
		show, e := s.d.CachedGet(ctx, title.ShowKey)
		visibility := s.d.Visibility
		visibility.GetTitle = s.d.CachedGet
		if e == nil && show != nil && show.Key == title.ShowKey && visibility.SectionFor != nil {
			if s.sectionOrFilterAllowed(ctx, v, show, visibility.SectionFor) && withoutSectionCheck(visibility).Visible(ctx, v, show) {
				copy := *title
				copy.ShowTitle = show.Title
				if copy.ShowYear == 0 {
					copy.ShowYear = show.Year
				}
				title = &copy
			}
		}
	}
	d := frontend.ItemDetail{Title: *title}
	if s.d.Enrich != nil {
		d, err = s.d.Enrich(ctx, v, req.Device, title)
		if err != nil {
			return frontend.ItemDetail{}, err
		}
		d.Title = *title
		extras := d.Extras[:0:0]
		for _, extra := range d.Extras {
			if extra.ParentKey == title.Key {
				extras = append(extras, extra)
			}
		}
		d.Extras = extras
	}
	d.UserData, err = s.d.State.Get(ctx, v.ID, title.Key)
	if err != nil {
		return frontend.ItemDetail{}, err
	}
	if s.d.IsFavorite != nil {
		d.UserData.IsFavorite, err = s.d.IsFavorite(ctx, v, title.Key)
	}
	if err == nil && s.d.IsNotInterested != nil {
		d.UserData.IsNotInterested, err = s.d.IsNotInterested(ctx, v, title.Key)
	}
	if err == nil && s.d.IsWatchlisted != nil {
		d.UserData.InWatchlist, err = s.d.IsWatchlisted(ctx, v, title.Key)
	}
	return d, err
}

// BatchTitles authorizes and returns titles for keys in the requested order
// using batched catalogue, plugin, availability, and parent lookups rather
// than per-item detail enrichment, watch-state, or artwork queries. Keys that
// are missing, non-title entities, or hidden from viewer are omitted.
func (s *Service) BatchTitles(ctx context.Context, viewer *core.Viewer, keys []core.RatingKey) ([]core.Title, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	ctx = s.NoProviderWait(ctx)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	var (
		pluginKeys  []core.RatingKey
		catalogKeys []core.RatingKey
		seen        = make(map[core.RatingKey]bool, len(keys))
	)
	for _, key := range keys {
		if key == 0 || seen[key] {
			continue
		}
		if key.IsPluginKey() {
			seen[key] = true
			pluginKeys = append(pluginKeys, key)
			continue
		}
		switch key.Kind() {
		case core.KindCollection, core.KindSmart, core.KindPlaylist:
			continue
		}
		seen[key] = true
		catalogKeys = append(catalogKeys, key)
	}
	allowed := make(map[core.RatingKey]core.Title, len(pluginKeys)+len(catalogKeys))
	if len(pluginKeys) > 0 && s.d.PluginLibraries != nil {
		views, err := s.d.PluginLibraries.ItemsMany(ctx, pluginKeys)
		if err != nil {
			return nil, err
		}
		pubCache := make(map[core.LibraryID]bool, 4)
		for k, piv := range views {
			if s.pluginItemAllowedCached(ctx, v, piv, pubCache) {
				allowed[k] = piv.Title
			}
		}
	}
	if len(catalogKeys) > 0 {
		var cached map[core.RatingKey]core.Title
		if s.d.CachedGetMany != nil {
			cached, err = s.d.CachedGetMany(ctx, catalogKeys)
			if err != nil {
				return nil, err
			}
		}
		cachedList := make([]core.Title, 0, len(cached))
		var missed []core.RatingKey
		for _, k := range catalogKeys {
			if t, ok := cached[k]; ok && t.Key == k {
				cachedList = append(cachedList, t)
			} else {
				missed = append(missed, k)
			}
		}
		if len(cachedList) > 0 {
			admitted, err := s.allowTitles(ctx, v, cachedList)
			if err != nil {
				return nil, err
			}
			for _, t := range admitted {
				allowed[t.Key] = t
			}
			for _, t := range cachedList {
				if _, ok := allowed[t.Key]; !ok && t.Key.Kind() == core.KindEpisode {
					if ep := s.scheduledTitle(ctx, v, t.Key); ep != nil {
						allowed[t.Key] = *ep
					}
				}
			}
		}
		for _, k := range missed {
			if s.d.Get != nil {
				if t, e := s.item(ctx, v, k); e == nil && t != nil {
					allowed[k] = *t
					continue
				}
			}
			if ep := s.scheduledTitle(ctx, v, k); ep != nil {
				allowed[k] = *ep
			}
		}
	}
	if s.d.CachedGetMany != nil {
		var needShows []core.RatingKey
		seenShow := map[core.RatingKey]bool{}
		for _, t := range allowed {
			if !t.Key.IsPluginKey() && t.ShowKey != 0 && t.ShowTitle == "" && !seenShow[t.ShowKey] {
				seenShow[t.ShowKey] = true
				needShows = append(needShows, t.ShowKey)
			}
		}
		if len(needShows) > 0 {
			if shows, e := s.d.CachedGetMany(ctx, needShows); e == nil && len(shows) > 0 {
				showList := make([]core.Title, 0, len(shows))
				for _, sh := range shows {
					showList = append(showList, sh)
				}
				if okShows, e := s.allowTitles(ctx, v, showList); e == nil && len(okShows) > 0 {
					byKey := make(map[core.RatingKey]core.Title, len(okShows))
					for _, sh := range okShows {
						byKey[sh.Key] = sh
					}
					for k, t := range allowed {
						if t.ShowKey != 0 && t.ShowTitle == "" {
							if sh, ok := byKey[t.ShowKey]; ok {
								t.ShowTitle = sh.Title
								if t.ShowYear == 0 {
									t.ShowYear = sh.Year
								}
								allowed[k] = t
							}
						}
					}
				}
			}
		}
	}
	out := make([]core.Title, 0, len(allowed))
	for _, k := range keys {
		if t, ok := allowed[k]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// AuthorizeArtwork applies the same active-viewer and item policy as Item,
// without running playback or track enrichment for a grid of images.
// allowTitles applies library, policy, availability and parent visibility to a
// page. Availability is one batch, not one query per title.
func (s *Service) allowTitles(ctx context.Context, v *core.User, titles []core.Title) ([]core.Title, error) {
	return s.allowTitlesWithParents(ctx, v, titles, s.d.ParentTitles)
}

type parentTitleReader func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)

func (s *Service) allowTitlesWithParents(ctx context.Context, v *core.User, titles []core.Title, readParents parentTitleReader) ([]core.Title, error) {
	if s.d.Visibility.SectionFor == nil {
		return nil, nil
	}
	var pluginKeys []core.RatingKey
	if s.d.PluginLibraries != nil {
		for i := range titles {
			if titles[i].Key.IsPluginKey() {
				pluginKeys = append(pluginKeys, titles[i].Key)
			}
		}
	}
	var pluginAllowed map[core.RatingKey]core.Title
	if len(pluginKeys) > 0 {
		views, err := s.d.PluginLibraries.ItemsMany(ctx, pluginKeys)
		if err != nil {
			return nil, err
		}
		pluginAllowed = make(map[core.RatingKey]core.Title, len(views))
		for k, piv := range views {
			if s.pluginItemAllowed(ctx, v, piv) {
				pluginAllowed[k] = piv.Title
			}
		}
	}
	sectionAllowed := s.sectionOrFilterAllowedBatchWithParents(ctx, v, titles, s.d.Visibility.SectionFor, readParents)
	candidates := make([]core.Title, 0, len(titles))
	for i := range titles {
		title := titles[i]
		if title.Key.IsPluginKey() {
			continue
		}
		if !sectionAllowed[title.Key] || !TitleAllowed(s.d.Visibility.Policy, v, &title) {
			continue
		}
		candidates = append(candidates, title)
	}
	available := s.d.Visibility.Available(ctx, v, candidates)
	parents := make([]core.RatingKey, 0)
	seenParent := map[core.RatingKey]bool{}
	for _, title := range available {
		if title.ShowKey != 0 && !seenParent[title.ShowKey] {
			seenParent[title.ShowKey] = true
			parents = append(parents, title.ShowKey)
		}
	}
	shows := map[core.RatingKey]core.Title{}
	if len(parents) > 0 {
		var err error
		switch {
		case readParents != nil:
			shows, err = readParents(ctx, parents)
		case s.d.CachedGetMany != nil:
			shows, err = s.d.CachedGetMany(ctx, parents)
		default:
			for _, key := range parents {
				if s.d.Visibility.GetTitle == nil {
					break
				}
				show, e := s.d.Visibility.GetTitle(ctx, key)
				if e == nil && show != nil {
					shows[key] = *show
				}
			}
		}
		if err != nil {
			return nil, err
		}
	}
	tmdbOut := make(map[core.RatingKey]core.Title, len(available))
	for _, title := range available {
		if title.ShowKey == 0 {
			tmdbOut[title.Key] = title
			continue
		}
		show, ok := shows[title.ShowKey]
		if ok && TitleAllowed(s.d.Visibility.Policy, v, &show) {
			tmdbOut[title.Key] = title
		}
	}
	out := make([]core.Title, 0, len(tmdbOut)+len(pluginAllowed))
	for _, title := range titles {
		if title.Key.IsPluginKey() {
			if pt, ok := pluginAllowed[title.Key]; ok {
				out = append(out, pt)
			}
		} else if tt, ok := tmdbOut[title.Key]; ok {
			out = append(out, tt)
		}
	}
	return out, nil
}

// artworkRows reads keys for an artwork request that lists no credited
// people's portraits (frontend.WithArtworkRows) as rows with what v's policy
// reads; a title availability judges on its facts is read in full. Nil,
// nil when the request or the catalogue cannot (read the titles in full).
func (s *Service) artworkRows(ctx context.Context, v *core.User, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
	if s.d.ArtworkRows == nil || frontend.ArtworkRowsFrom(ctx) == nil {
		return nil, nil
	}
	return s.policyRows(ctx, v, keys, 0, s.d.ArtworkRows)
}

// visibilityRows keeps synthetic seasons and non-catalogue keys on their
// existing readers; the bounded row source handles mirrored movies and shows.
func (s *Service) visibilityRows(ctx context.Context, v *core.User, keys []core.RatingKey, facts core.TitleFacts) (map[core.RatingKey]core.Title, error) {
	for _, key := range keys {
		if key.Kind() != core.KindMovie && key.Kind() != core.KindShow {
			return nil, nil
		}
	}
	return s.policyRows(ctx, v, keys, facts, s.d.VisibilityRows)
}

// policyRows loads only policy and requested facts, reading full titles when
// discovery evidence needs them. These partial rows never enter the title cache.
func (s *Service) policyRows(ctx context.Context, v *core.User, keys []core.RatingKey, facts core.TitleFacts, read func(context.Context, []core.RatingKey, core.TitleFacts) (map[core.RatingKey]core.Title, error)) (map[core.RatingKey]core.Title, error) {
	if read == nil {
		return nil, nil
	}
	judge, known := s.d.Visibility.FactJudge(ctx)
	if !known {
		return nil, nil
	}
	rows, err := read(ctx, keys, facts|core.FactHomeRelease|s.d.Visibility.TitleFacts(v))
	if err != nil {
		return nil, err
	}
	if judge == nil {
		return rows, nil
	}
	var full []core.RatingKey
	for key, t := range rows {
		if judge(t) {
			full = append(full, key)
		}
	}
	if len(full) == 0 {
		return rows, nil
	}
	getMany := s.d.ArtworkGetMany
	if getMany == nil {
		getMany = s.d.CachedGetMany
	}
	if getMany == nil {
		return nil, nil
	}
	loaded, err := getMany(ctx, full)
	if err != nil {
		return nil, err
	}
	for _, key := range full {
		if t, ok := loaded[key]; ok {
			rows[key] = t
		} else {
			delete(rows, key)
		}
	}
	return rows, nil
}

// PageArtworkTitles loads one grid and drops titles this viewer cannot see.
func (s *Service) PageArtworkTitles(ctx context.Context, viewer *core.Viewer, keys []core.RatingKey) ([]core.Title, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	var pluginKeys, tmdbKeys []core.RatingKey
	for _, k := range keys {
		if k.IsPluginKey() {
			pluginKeys = append(pluginKeys, k)
		} else {
			tmdbKeys = append(tmdbKeys, k)
		}
	}
	var pluginTitles map[core.RatingKey]core.Title
	if len(pluginKeys) > 0 && s.d.PluginLibraries != nil {
		views, err := s.d.PluginLibraries.ItemsMany(ctx, pluginKeys)
		if err != nil {
			return nil, err
		}
		pluginTitles = make(map[core.RatingKey]core.Title, len(views))
		pubCache := make(map[core.LibraryID]bool, 4)
		for k, piv := range views {
			if s.pluginItemAllowedCached(ctx, v, piv, pubCache) {
				pluginTitles[k] = piv.Title
			}
		}
	}
	var tmdbAllowed map[core.RatingKey]core.Title
	if len(tmdbKeys) > 0 {
		getMany := s.d.ArtworkGetMany
		if getMany == nil {
			getMany = s.d.CachedGetMany
		}
		if getMany == nil {
			return nil, core.ErrNotAvailable
		}
		titles, err := s.artworkRows(ctx, v, tmdbKeys)
		if err != nil {
			return nil, err
		}
		if titles == nil {
			if titles, err = getMany(ctx, tmdbKeys); err != nil {
				return nil, err
			}
		}
		loaded := make([]core.Title, 0, len(titles))
		for _, key := range tmdbKeys {
			title, ok := titles[key]
			if ok && title.Key == key {
				loaded = append(loaded, title)
			}
		}
		allowedList, err := s.allowTitles(ctx, v, loaded)
		if err != nil {
			return nil, err
		}
		tmdbAllowed = make(map[core.RatingKey]core.Title, len(allowedList))
		for _, t := range allowedList {
			tmdbAllowed[t.Key] = t
		}
	}
	out := make([]core.Title, 0, len(keys))
	for _, k := range keys {
		if k.IsPluginKey() {
			if pt, ok := pluginTitles[k]; ok {
				out = append(out, pt)
			}
		} else if tt, ok := tmdbAllowed[k]; ok {
			out = append(out, tt)
		}
	}
	return out, nil
}

func (s *Service) AuthorizeArtwork(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) (*core.Viewer, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeArtwork(ctx, v, device, key); err != nil {
		return nil, err
	}
	return v, nil
}

// AuthorizeArtworkAs is AuthorizeArtwork for a viewer record the caller
// has just read from the identity store (an effective active user): the
// same section, rating, owner-policy and inherited-show checks run, but
// the record is used exactly as given instead of being reloaded by ID.
// Public (unauthenticated) artwork authorization runs it once per active
// user and requires every one to pass.
func (s *Service) AuthorizeArtworkAs(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) error {
	if viewer == nil || viewer.ID <= 0 {
		return core.ErrNotFound
	}
	return s.authorizeArtwork(core.WithCatalogViewer(ctx, viewer), viewer, device, key)
}

func (s *Service) authorizeArtwork(ctx context.Context, v *core.Viewer, device core.Device, key core.RatingKey) error {
	if key.IsPluginKey() {
		piv, err := s.pluginItem(core.WithNoContainerExpand(ctx), v, key)
		if err != nil {
			return err
		}
		frontend.ArtworkRowsFrom(ctx).Put(piv.Title)
		return nil
	}
	if key.Kind() == core.KindPlaylist {
		if reader, ok := s.d.Playlists.(interface {
			Exists(context.Context, int64, core.RatingKey) (bool, error)
		}); ok {
			exists, err := reader.Exists(ctx, v.ID, key)
			if err != nil || !exists {
				return core.ErrNotFound
			}
			return nil
		}
		if _, err := s.ownedPlaylist(ctx, v, key); err != nil {
			return err
		}
		return nil
	}
	if key.Kind() == core.KindSmart && frontend.CollectionCardArtwork(ctx) {
		checker, ok := s.d.Collections.(collectionCardHeaders)
		if !ok {
			return core.ErrNotFound
		}
		card := readCollectionCard(ctx, checker, v, key)
		if card == nil || !s.d.Visibility.CollectionAllowed(v, card) {
			return core.ErrNotFound
		}
		return nil
	}
	if key.Kind() == core.KindSmart && s.d.Collections != nil {
		// Artwork needs only a visible collection with one visible member;
		// loading every member (a genre collection holds thousands) per
		// poster made a Collections page take most of a minute.
		u, err := s.viewer(ctx, v)
		if err != nil {
			return err
		}
		c, err := s.d.Collections.Get(ctx, u, key)
		if err != nil {
			return err
		}
		if !s.d.Visibility.CollectionAllowed(u, c) || (c.OwnerUserID != 0 && c.OwnerUserID != u.ID) {
			return core.ErrNotFound
		}
		ok, err := s.smartCollectionHasVisibleMember(ctx, u, *c)
		if err != nil {
			return err
		}
		if !ok {
			return core.ErrNotFound
		}
	} else if key.Kind() == core.KindCollection || key.Kind() == core.KindSmart {
		if _, err := s.Item(ctx, frontend.ItemRequest{Viewer: v, Device: device, Key: key}); err != nil {
			return err
		}
	} else if s.d.CachedGet != nil {
		get := s.d.ArtworkGet
		if get == nil {
			get = s.d.CachedGet
		}
		var title *core.Title
		rows, err := s.artworkRows(ctx, v, []core.RatingKey{key})
		if err != nil {
			return err
		}
		if t, ok := rows[key]; ok {
			title = &t
		} else {
			title, err = get(ctx, key)
		}
		if errors.Is(err, core.ErrNotFound) && s.d.CachedVirtualSeason != nil {
			title, err = s.d.CachedVirtualSeason(ctx, v, key)
		}
		visibility := s.d.Visibility
		visibility.GetTitle = s.d.CachedGet
		if err != nil {
			return err
		}
		if title == nil || title.Key != key || visibility.SectionFor == nil {
			return core.ErrNotFound
		}
		if !s.sectionOrFilterAllowed(ctx, v, title, visibility.SectionFor) || !withoutSectionCheck(visibility).Visible(ctx, v, title) {
			return core.ErrNotFound
		}
		// The image read that follows lists the same title (ArtworkTitle).
		frontend.ArtworkRowsFrom(ctx).Put(*title)
	} else if _, err := s.item(ctx, v, key); err != nil {
		return err
	}
	return nil
}

// LocalEpisodes lists aired episodes already stored for a show or season.
// The bool is false when the catalogue page was capped, so a caller that
// needs every episode can omit a count instead of inventing one. It does
// not refresh the show or its seasons.
func (s *Service) LocalEpisodes(ctx context.Context, viewer *core.Viewer, key core.RatingKey) ([]core.Title, bool, error) {
	if key.Kind() != core.KindShow && key.Kind() != core.KindSeason {
		return nil, false, core.ErrNotFound
	}
	lists, err := s.localEpisodesMany(ctx, viewer, []core.RatingKey{key})
	if err != nil {
		return nil, false, err
	}
	list := lists[key]
	return list.Items, list.Complete, nil
}

func (s *Service) Children(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey, q frontend.ItemQuery) (core.Page, error) {
	ctx = s.NoProviderWait(ctx)
	ctx = core.WithCatalogViewer(ctx, viewer)
	if q.StartAt != 0 && q.AdjacentTo != 0 {
		return core.Page{}, errors.New("experience: incompatible item anchors")
	}
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return core.Page{}, err
	}
	if key.IsPluginKey() {
		if q.FilterByProviderIDs {
			return emptyProviderIDPage(q)
		}
		return s.pluginChildren(ctx, v, key, q)
	}
	if key.Kind() == core.KindCollection || key.Kind() == core.KindSmart || key.Kind() == core.KindPlaylist {
		detail, err := s.Item(ctx, frontend.ItemRequest{Viewer: v, Device: device, Key: key})
		if err != nil {
			return core.Page{}, err
		}
		var items []core.Title
		if detail.Collection != nil {
			items, err = s.collectionItems(ctx, v, key)
		} else if detail.Playlist != nil {
			visibleMap, loadErr := s.loadVisiblePlaylistTitles(ctx, v, detail.Playlist.Items)
			if loadErr != nil {
				err = loadErr
			} else {
				for _, k := range detail.Playlist.Items {
					if t, ok := visibleMap[k]; ok {
						items = append(items, t)
					}
				}
			}
		}
		if err != nil {
			return core.Page{}, err
		}
		var providerKeySet map[core.RatingKey]bool
		if q.FilterByProviderIDs {
			allowedTypes := intersectRequestedTypes([]core.MediaType{core.TypeMovie, core.TypeShow, core.TypeSeason, core.TypeEpisode}, q)
			matchedKeys, kerr := s.resolveProviderIDKeys(ctx, q.ProviderIDs, allowedTypes)
			if kerr != nil {
				return core.Page{}, kerr
			}
			providerKeySet = make(map[core.RatingKey]bool, len(matchedKeys))
			for _, mk := range matchedKeys {
				providerKeySet[mk] = true
			}
		}
		check := q
		check.FilterByProviderIDs = false
		check.ProviderIDs = nil
		filtered := make([]core.Title, 0, len(items))
		for _, title := range items {
			if q.FilterByProviderIDs && !providerKeySet[title.Key] && !titleMatchesProviderIDs(title, q.ProviderIDs) {
				continue
			}
			ok, e := s.matchesUserQuery(ctx, v, title, check)
			if e != nil {
				return core.Page{}, e
			}
			if ok {
				filtered = append(filtered, title)
			}
		}
		items = filtered
		if err = s.sortTitles(ctx, items, q); err != nil {
			return core.Page{}, err
		}
		if q.AdjacentTo != 0 {
			return adjacentTitles(items, q)
		}
		if q.StartAt != 0 {
			return startingTitles(items, q)
		}
		total := len(items)
		start := min(max(q.Offset, 0), total)
		end := total
		if q.Limit > 0 {
			end = min(start+q.Limit, total)
		}
		return core.Page{Items: items[start:end], Total: total}, nil
	}
	parent, err := s.item(ctx, v, key)
	if err != nil {
		return core.Page{}, err
	}
	if q.FilterByProviderIDs {
		return s.providerIDChildren(ctx, v, key, q)
	}
	if page, handled, err := s.pagedSeason(ctx, v, key, q); handled || err != nil {
		return page, err
	}

	var items []core.Title
	if q.Recursive && key.Kind() == core.KindShow {
		if page, ok, err := s.favoriteEpisodePage(ctx, v, &Library{Type: core.TypeShow}, key, q); err != nil {
			return core.Page{}, err
		} else if ok {
			return page, nil
		}
		if page, ok, err := s.favoriteRecursivePage(ctx, v, &Library{Type: core.TypeShow}, key, q); err != nil {
			return core.Page{}, err
		} else if ok {
			return page, nil
		}
		if page, ok, err := s.playedEpisodePage(ctx, v, &Library{Type: core.TypeShow}, key, q); err != nil {
			return core.Page{}, err
		} else if ok {
			return page, nil
		}
		if s.d.Grandchildren == nil {
			return core.Page{}, errors.New("experience: recursive children unavailable")
		}
		items, err = s.d.Grandchildren(ctx, v, key)
	} else {
		items, err = s.d.Children(ctx, v, key)
	}
	if err != nil {
		return core.Page{}, err
	}
	out := make([]core.Title, 0, len(items))
	for i := range items {
		if s.visible(ctx, v, &items[i]) {
			ok, e := s.matchesUserQuery(ctx, v, items[i], q)
			if e != nil {
				return core.Page{}, e
			}
			if ok {
				out = append(out, items[i])
			}
		}
	}
	out, err = s.appendScheduled(ctx, v, parent, out, q)
	if err != nil {
		return core.Page{}, err
	}
	if err := s.sortTitles(ctx, out, q); err != nil {
		return core.Page{}, err
	}
	if q.AdjacentTo != 0 {
		return adjacentTitles(out, q)
	}
	if q.StartAt != 0 {
		return startingTitles(out, q)
	}
	total := len(out)
	start := min(max(q.Offset, 0), total)
	end := total
	if q.Limit > 0 {
		end = min(start+q.Limit, total)
	}
	return core.Page{Items: out[start:end], Total: total}, nil
}

// rootWindow answers a bounded page across every library (Swiftfin's and
// Infuse's cross-library "Recently Added"): each library returns only its
// own first offset+limit titles in the requested order, from its indexed
// page, and the windows are merged. The old path listed every title of
// every library and sorted them in memory.
func (s *Service) rootWindow(ctx context.Context, v *core.User, device core.Device, q frontend.ItemQuery) (core.Page, error) {
	inner := q
	inner.Offset = 0
	inner.Limit = max(q.Offset, 0) + q.Limit
	all := make([]core.Title, 0, inner.Limit*2)
	total := 0
	if s.d.PluginLibraries != nil {
		views, err := s.storedViews(ctx, v)
		if err != nil {
			return core.Page{}, err
		}
		for _, view := range views {
			libQuery := inner
			if view.KindKey == "" && len(q.Types) > 0 {
				libQuery.Types = nil
				for _, typ := range q.Types {
					if typ == view.Type || (view.Type == core.TypeShow && (typ == core.TypeSeason || typ == core.TypeEpisode)) {
						libQuery.Types = append(libQuery.Types, typ)
					}
				}
				if len(libQuery.Types) == 0 {
					continue
				}
			}
			page, err := s.LibraryItems(ctx, v, device, view.ID, libQuery)
			if err != nil {
				return core.Page{}, err
			}
			all = append(all, page.Items...)
			total += page.Total
		}
	} else {
		for _, lib := range s.libraries() {
			if !SectionAllowed(v, lib.ID) {
				continue
			}
			// Ask each library only for the requested types it can hold, so its
			// page is one indexed catalogue page.
			libQuery := inner
			if len(q.Types) > 0 {
				libQuery.Types = nil
				for _, typ := range q.Types {
					if typ == lib.Type || (lib.Type == core.TypeShow && (typ == core.TypeSeason || typ == core.TypeEpisode)) {
						libQuery.Types = append(libQuery.Types, typ)
					}
				}
				if len(libQuery.Types) == 0 {
					continue
				}
			}
			page, err := s.LibraryItems(ctx, v, device, strconv.Itoa(lib.ID), libQuery)
			if err != nil {
				return core.Page{}, err
			}
			all = append(all, page.Items...)
			total += page.Total
		}
	}
	if err := s.sortTitles(ctx, all, q); err != nil {
		return core.Page{}, err
	}
	start := min(max(q.Offset, 0), len(all))
	end := min(start+q.Limit, len(all))
	return core.Page{Items: all[start:end], Total: total}, nil
}

// searchWindowMin is the smallest candidate window a search reads.
const searchWindowMin = 100

func (s *Service) Search(ctx context.Context, viewer *core.Viewer, device core.Device, query string, q frontend.ItemQuery) (frontend.SearchPage, error) {
	page, _, err := s.search(ctx, viewer, device, query, q, false)
	return page, err
}

// search is Search; unified leaves descriptive (AI) results to the caller
// (UnifiedSearch) and reports whether the results hold a confident match:
// a title, franchise or person the query names, in the catalogue or a
// plugin library.
func (s *Service) search(ctx context.Context, viewer *core.Viewer, device core.Device, query string, q frontend.ItemQuery, unified bool) (outPage frontend.SearchPage, outConfident bool, outErr error) {
	ctx = core.WithCatalogViewer(core.WithExplicitSearch(ctx), viewer)
	if core.SearchClientFrom(ctx) == "" && device.ID != "" {
		ctx = core.WithSearchClient(ctx, device.ID)
	}
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.SearchPage{}, false, err
	}
	// A newer query from this viewer's client (the next keystroke) cancels
	// this search and drops the remote lookups only it still wanted.
	ctx, leave := core.EnterSearch(ctx, v.ID, core.SearchClientFrom(ctx), query)
	defer leave()
	defer func() {
		if outErr != nil && core.SearchWasSuperseded(ctx) {
			// The viewer typed on: answer empty, never an error a client
			// might retry (the retry would supersede the newer search).
			outPage, outConfident, outErr = frontend.SearchPage{Page: core.Page{Items: []core.Title{}}, Continuation: frontend.ContinuationExhausted}, false, nil
		}
	}()
	if !s.requestedTypesServable(ctx, v, q) {
		return frontend.SearchPage{}, false, nil
	}

	var types []core.MediaType
	show := false
	seenType := make(map[core.MediaType]bool)
	if s.d.PluginLibraries != nil {
		views, err := s.storedViews(ctx, v)
		if err != nil {
			return frontend.SearchPage{}, false, err
		}
		for _, view := range views {
			if view.KindKey != "" {
				continue
			}
			if !seenType[view.Type] {
				seenType[view.Type] = true
				types = append(types, view.Type)
			}
			if view.Type == core.TypeShow {
				show = true
			}
		}
	} else {
		for _, lib := range s.libraries() {
			if SectionAllowed(v, lib.ID) {
				types = append(types, lib.Type)
				if lib.Type == core.TypeShow {
					show = true
				}
			}
		}
	}
	// Season and episode titles are keyword hits inside a show library.
	if show {
		types = append(types, core.TypeSeason, core.TypeEpisode)
	}
	// Apply the requested media types before the catalogue chooses and
	// hydrates its candidate window. Filtering only afterwards wastes that
	// window on other libraries and can crowd out valid results.
	if len(q.Types) > 0 || len(q.UnservedTypes) > 0 || len(q.ExcludedTypes) > 0 {
		filtered := types[:0]
		for _, typ := range types {
			if (len(q.Types) == 0 && len(q.UnservedTypes) == 0 || containsMediaType(q.Types, typ)) && !containsMediaType(q.ExcludedTypes, typ) {
				filtered = append(filtered, typ)
			}
		}
		types = filtered
	}
	offset := max(q.Offset, 0)
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if offset >= 1000 {
		return frontend.SearchPage{Continuation: frontend.ContinuationUnknown}, false, nil
	}
	end := min(offset+limit, 1000)
	// The candidate window grows with the page asked for, so a first page
	// of search results does not load and hydrate a thousand titles.
	window := min(max(2*end, searchWindowMin), 1000)
	// The window is ranked and filtered here, so the catalogue returns it
	// as rows with the facts the filters read; only the page is read in full.
	ctx, pending := core.WithSearchPending(ctx)
	findCtx, cand := ctx, s.searchCandidates(ctx, v, q)
	if cand != nil {
		findCtx = core.WithSearchCandidates(ctx, cand)
	}
	// Title search answers from the catalogue Ether holds: a provider's
	// live search (TMDB) still runs, detached, and answers the next search.
	findCtx = s.NoProviderWait(findCtx)
	// Plugin libraries are searched alongside the catalogue, never after
	// it, and never wait on a plugin's remote catalogue: what a plugin is
	// still fetching arrives on the next ask (SearchPage.Pending).
	var (
		pluginHits []core.Title
		pluginBest float64
		pluginErr  error
		pluginDone = make(chan struct{})
	)
	go func() {
		defer close(pluginDone)
		pluginHits, pluginBest, pluginErr = s.pluginSearchItems(ctx, v, query, q, window)
	}()
	var items []core.Title
	if len(types) > 0 {
		candidates, err := s.d.Search.Find(findCtx, v, query, types, window)
		if err != nil {
			<-pluginDone
			return frontend.SearchPage{}, false, err
		}
		candidates, err = s.visibleCandidates(ctx, v, candidates)
		if err != nil {
			<-pluginDone
			return frontend.SearchPage{}, false, err
		}
		items = make([]core.Title, 0, len(candidates))
		for _, title := range candidates {
			ok, e := s.matchesUserQuery(ctx, v, title, q)
			if e != nil {
				<-pluginDone
				return frontend.SearchPage{}, false, e
			}
			if ok {
				items = append(items, title)
			}
		}
	}
	<-pluginDone
	if pluginErr != nil {
		return frontend.SearchPage{}, false, pluginErr
	}
	confident := pluginBest >= pluginNamedScore || namesTitle(query, items)
	aiMerged := false
	if ds, ok := s.d.Search.Vibe.Source.(core.DescriptiveSearcher); ok && s.d.Protocol != "vela" && !unified {
		if confident || !IsDescriptiveQuery(query, items, ds.DescriptiveSearchAvailable()) || len(types) == 0 {
			if prev, superseded := s.ensureSearchGate().supersede(v, core.SearchClientFrom(ctx), query); superseded {
				if ctrl, ok := s.d.Search.Vibe.Source.(core.DescriptiveSearchController); ok {
					ctrl.CancelDescribeSearch(v, prev)
				}
			}
		} else {
			search := s.d.Search
			search.gate = s.ensureSearchGate()
			_, aiRaw, handled, err := search.resolveDescriptive(ctx, v, query, types, false)
			if err != nil {
				return frontend.SearchPage{}, false, err
			}
			if handled && len(aiRaw) > 0 {
				aiItems := make([]core.Title, 0, len(aiRaw))
				for _, title := range aiRaw {
					if !s.visible(ctx, v, &title) {
						continue
					}
					ok, e := s.matchesUserQuery(ctx, v, title, q)
					if e != nil {
						return frontend.SearchPage{}, false, e
					}
					if ok {
						aiItems = append(aiItems, title)
					}
				}
				items = mergeAIAndKeyword(aiItems, items)
				aiMerged = true
			}
		}
	} else if !unified && s.d.Protocol != "vela" && len(items) == 0 && len(pluginHits) == 0 && len(types) > 0 && !core.IsShortSearch(query) {
		// Vela asks for description matches itself (mode "descriptive", or
		// one list with mode "auto"): its title search never returns them.
		items, err = s.vibeMembers(ctx, v, query, q)
		if err != nil {
			return frontend.SearchPage{}, false, err
		}
	}
	if len(pluginHits) > 0 {
		// Interleave by relevance: a plugin title or cast member match must
		// not land behind every loose catalogue match.
		items = append(items, pluginHits...)
		if !aiMerged {
			sort.SliceStable(items, func(i, j int) bool {
				if items[i].SearchScore != items[j].SearchScore {
					return items[i].SearchScore > items[j].SearchScore
				}
				return items[i].Popularity > items[j].Popularity
			})
		}
	}
	if err = s.sortTitles(ctx, items, q); err != nil {
		return frontend.SearchPage{}, false, err
	}
	start := min(offset, len(items))
	stop := min(end, len(items))
	continuation := frontend.ContinuationUnknown
	if len(items) > end {
		continuation = frontend.ContinuationMore
	}
	page := items[start:stop]
	// A summary search (Stremio) shows the rows as they are.
	if cand != nil && !core.SearchSummaryFacts(ctx) && len(page) > 0 {
		hasPlugin := false
		var tmdbSub []core.Title
		for _, item := range page {
			if item.Key.IsPluginKey() {
				hasPlugin = true
			} else {
				tmdbSub = append(tmdbSub, item)
			}
		}
		if !hasPlugin {
			full, err := s.d.Search.Details(findCtx, page)
			if err != nil {
				return frontend.SearchPage{}, false, err
			}
			page = full
		} else if len(tmdbSub) > 0 {
			fullSub, err := s.d.Search.Details(findCtx, tmdbSub)
			if err != nil {
				return frontend.SearchPage{}, false, err
			}
			byKey := make(map[core.RatingKey]core.Title, len(fullSub))
			for _, t := range fullSub {
				byKey[t.Key] = t
			}
			merged := make([]core.Title, len(page))
			for i, item := range page {
				if full, ok := byKey[item.Key]; ok {
					merged[i] = full
				} else {
					merged[i] = item
				}
			}
			page = merged
		}
	}
	return frontend.SearchPage{Page: core.Page{Items: page, Total: len(items)}, TotalExact: false, Continuation: continuation, Pending: pending.Pending()}, confident, nil
}

// searchCandidates is the candidate window a search asks the catalogue for
// (core.SearchCandidates): every fact the viewer's policy and q's filters
// read beyond a title's row, and the titles availability judges in full.
// Nil, for full results, when a filter reads credits, the sort is by the
// date added (a full title derives it from its release dates), or the
// catalogue or availability cannot say.
func (s *Service) searchCandidates(ctx context.Context, v *core.User, q frontend.ItemQuery) *core.SearchCandidates {
	if s.d.Search.Details == nil || q.Filter.Person != 0 || len(q.Filter.Credits) > 0 || q.Sort == core.SortAdded {
		return nil
	}
	detail, ok := s.d.Visibility.FactJudge(ctx)
	if !ok {
		return nil
	}
	facts := s.d.Visibility.TitleFacts(v)
	if len(q.Filter.Genres) > 0 || len(q.Filter.ExcludeGenres) > 0 {
		facts |= core.FactGenres
	}
	if len(q.Filter.Countries) > 0 || len(q.Filter.ExcludeCountries) > 0 {
		facts |= core.FactCountries
	}
	if q.Filter.Studio != "" || len(q.Filter.StudioFilters) > 0 {
		facts |= core.FactStudios
	}
	return &core.SearchCandidates{Facts: facts, Detail: detail}
}
func (s *Service) UserData(ctx context.Context, viewer *core.Viewer, keys []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	allowed := make([]core.RatingKey, 0, len(keys))
	var pluginKeys, tmdbKeys []core.RatingKey
	// A list may already have authorized these same keys in this request.
	// Reuse only verdicts belonging to this active viewer.
	tmdbAllowed := map[core.RatingKey]bool{}
	known := map[core.RatingKey]bool{}
	if memo := frontend.RequestMemoFrom(ctx); memo != nil {
		memo.Mu.Lock()
		if memo.Viewer != nil && memo.Viewer.ID == v.ID {
			for _, key := range keys {
				if !key.IsPluginKey() {
					if yes, ok := memo.Visible[key]; ok {
						known[key] = true
						tmdbAllowed[key] = yes
					}
				}
			}
		}
		memo.Mu.Unlock()
	}
	for _, k := range keys {
		if k.IsPluginKey() {
			pluginKeys = append(pluginKeys, k)
		} else if !known[k] {
			tmdbKeys = append(tmdbKeys, k)
		}
	}
	var pluginAllowed map[core.RatingKey]bool
	if len(pluginKeys) > 0 && s.d.PluginLibraries != nil {
		views, err := s.d.PluginLibraries.ItemsMany(ctx, pluginKeys)
		if err != nil {
			return nil, err
		}
		pluginAllowed = make(map[core.RatingKey]bool, len(views))
		pubCache := make(map[core.LibraryID]bool, 4)
		for k, piv := range views {
			if s.pluginItemAllowedCached(ctx, v, piv, pubCache) {
				pluginAllowed[k] = true
			}
		}
	}
	if len(tmdbKeys) > 0 {
		if s.d.CachedGetMany != nil {
			// Watch status needs current authorization, not cast, crew or trailers.
			// Keep the full read fallback for sources without visibility rows.
			titles, err := s.visibilityRows(ctx, v, tmdbKeys, 0)
			if err == nil && titles == nil {
				titles, err = s.d.CachedGetMany(ctx, tmdbKeys)
			}
			if err != nil {
				return nil, err
			}
			loaded := make([]core.Title, 0, len(tmdbKeys))
			for _, key := range tmdbKeys {
				title, ok := titles[key]
				if ok && title.Key == key {
					loaded = append(loaded, title)
				}
			}
			permitted, err := s.allowTitles(ctx, v, loaded)
			if err != nil {
				return nil, err
			}
			for _, title := range permitted {
				tmdbAllowed[title.Key] = true
			}
		} else {
			for _, key := range tmdbKeys {
				if _, err := s.item(ctx, v, key); err == nil {
					tmdbAllowed[key] = true
				}
			}
		}
	}
	for _, key := range keys {
		if key.IsPluginKey() {
			if pluginAllowed[key] {
				allowed = append(allowed, key)
			}
		} else if tmdbAllowed[key] {
			allowed = append(allowed, key)
		}
	}
	if facts, ok := s.d.State.(pageUserData); ok {
		data, err := facts.PageUserData(ctx, v.ID, allowed)
		if err != nil {
			return nil, err
		}
		for _, key := range allowed {
			status := data[key]
			status.Key = key
			data[key] = status
		}
		return data, nil
	}
	data, err := s.d.State.GetMany(ctx, v.ID, allowed)
	if err != nil {
		return nil, err
	}
	filtered := make(map[core.RatingKey]core.WatchStatus, len(allowed))
	for _, key := range allowed {
		status := data[key]
		status.Key = key
		filtered[key] = status
	}
	data = filtered
	if err := s.fillFavorites(ctx, v, allowed, data); err != nil {
		return nil, err
	}
	if err := s.fillNotInterested(ctx, v, allowed, data); err != nil {
		return nil, err
	}
	if err := s.fillWatchlist(ctx, v, allowed, data); err != nil {
		return nil, err
	}
	return data, nil
}

func (s *Service) logPluginEnrichError(field string, key core.RatingKey, err error) {
	log := s.d.Log
	if log == nil {
		log = slog.Default()
	}
	log.Warn("plugin item enrichment omitted", "field", field, "key", key, "err", err)
}

func splitPluginKeys(keys []core.RatingKey) (catalog, plugin []core.RatingKey) {
	for _, key := range keys {
		if key.IsPluginKey() {
			plugin = append(plugin, key)
		} else {
			catalog = append(catalog, key)
		}
	}
	return catalog, plugin
}

func (s *Service) fillFavorites(ctx context.Context, v *core.User, keys []core.RatingKey, data map[core.RatingKey]core.WatchStatus) error {
	catalogKeys, pluginKeys := splitPluginKeys(keys)
	if s.d.IsFavoriteMany != nil {
		if len(catalogKeys) > 0 {
			flags, err := s.d.IsFavoriteMany(ctx, v, catalogKeys)
			if err != nil {
				return err
			}
			for _, key := range catalogKeys {
				status := data[key]
				status.Key = key
				status.IsFavorite = flags[key]
				data[key] = status
			}
		}
		if len(pluginKeys) > 0 {
			flags, err := s.d.IsFavoriteMany(ctx, v, pluginKeys)
			if err != nil {
				s.logPluginEnrichError("favorites", pluginKeys[0], err)
			}
			for _, key := range pluginKeys {
				status := data[key]
				status.Key = key
				if err == nil {
					status.IsFavorite = flags[key]
				}
				data[key] = status
			}
		}
		return nil
	}
	if s.d.IsFavorite == nil {
		return nil
	}
	for _, key := range catalogKeys {
		favorite, err := s.d.IsFavorite(ctx, v, key)
		if err != nil {
			return err
		}
		status := data[key]
		status.Key = key
		status.IsFavorite = favorite
		data[key] = status
	}
	for _, key := range pluginKeys {
		status := data[key]
		status.Key = key
		if favorite, err := s.d.IsFavorite(ctx, v, key); err != nil {
			s.logPluginEnrichError("favorites", key, err)
		} else {
			status.IsFavorite = favorite
		}
		data[key] = status
	}
	return nil
}

func (s *Service) fillNotInterested(ctx context.Context, v *core.User, keys []core.RatingKey, data map[core.RatingKey]core.WatchStatus) error {
	catalogKeys, pluginKeys := splitPluginKeys(keys)
	if s.d.IsNotInterestedMany != nil {
		if len(catalogKeys) > 0 {
			flags, err := s.d.IsNotInterestedMany(ctx, v, catalogKeys)
			if err != nil {
				return err
			}
			for _, key := range catalogKeys {
				status := data[key]
				status.IsNotInterested = flags[key]
				data[key] = status
			}
		}
		if len(pluginKeys) > 0 {
			flags, err := s.d.IsNotInterestedMany(ctx, v, pluginKeys)
			if err != nil {
				s.logPluginEnrichError("not_interested", pluginKeys[0], err)
			} else {
				for _, key := range pluginKeys {
					status := data[key]
					status.IsNotInterested = flags[key]
					data[key] = status
				}
			}
		}
		return nil
	}
	if s.d.IsNotInterested == nil {
		return nil
	}
	for _, key := range catalogKeys {
		value, err := s.d.IsNotInterested(ctx, v, key)
		if err != nil {
			return err
		}
		status := data[key]
		status.IsNotInterested = value
		data[key] = status
	}
	for _, key := range pluginKeys {
		if value, err := s.d.IsNotInterested(ctx, v, key); err != nil {
			s.logPluginEnrichError("not_interested", key, err)
		} else {
			status := data[key]
			status.IsNotInterested = value
			data[key] = status
		}
	}
	return nil
}

func (s *Service) fillWatchlist(ctx context.Context, v *core.User, keys []core.RatingKey, data map[core.RatingKey]core.WatchStatus) error {
	catalogKeys, pluginKeys := splitPluginKeys(keys)
	if s.d.IsWatchlistedMany != nil {
		if len(catalogKeys) > 0 {
			flags, err := s.d.IsWatchlistedMany(ctx, v.ID, catalogKeys)
			if err != nil {
				return err
			}
			for _, key := range catalogKeys {
				status := data[key]
				status.InWatchlist = flags[key]
				data[key] = status
			}
		}
		if len(pluginKeys) > 0 {
			flags, err := s.d.IsWatchlistedMany(ctx, v.ID, pluginKeys)
			if err != nil {
				s.logPluginEnrichError("watchlist", pluginKeys[0], err)
			} else {
				for _, key := range pluginKeys {
					status := data[key]
					status.InWatchlist = flags[key]
					data[key] = status
				}
			}
		}
		return nil
	}
	if s.d.IsWatchlisted == nil {
		return nil
	}
	for _, key := range catalogKeys {
		value, err := s.d.IsWatchlisted(ctx, v, key)
		if err != nil {
			return err
		}
		status := data[key]
		status.InWatchlist = value
		data[key] = status
	}
	for _, key := range pluginKeys {
		if value, err := s.d.IsWatchlisted(ctx, v, key); err != nil {
			s.logPluginEnrichError("watchlist", key, err)
		} else {
			status := data[key]
			status.InWatchlist = value
			data[key] = status
		}
	}
	return nil
}

var _ frontend.Experience = (*Service)(nil)

func (s *Service) mutate(ctx context.Context, viewer *core.User, key core.RatingKey, fn func(*core.User) error) error {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return err
	}
	if _, err = s.playable(ctx, v, key); err != nil {
		return err
	}
	return fn(v)
}
func (s *Service) setPlayed(ctx context.Context, v *core.Viewer, k core.RatingKey, played bool) error {
	ctx = state.WithOrigin(ctx, state.Origin{Surface: s.d.Protocol})
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return err
	}
	if k.IsPluginKey() {
		noExpandCtx := core.WithNoContainerExpand(ctx)
		piv, err := s.pluginItem(noExpandCtx, viewer, k)
		if err != nil {
			return err
		}
		keys := []core.RatingKey{k}
		if !piv.Playable {
			keys = nil
			cursor := ""
			for {
				page, err := s.d.PluginLibraries.Children(noExpandCtx, k, true, cursor, 500)
				if err != nil {
					return err
				}
				if len(page.Items) > 0 {
					childKeys := make([]core.RatingKey, len(page.Items))
					for i := range page.Items {
						childKeys[i] = page.Items[i].Key
					}
					views, err := s.d.PluginLibraries.ItemsMany(ctx, childKeys)
					if err != nil {
						return err
					}
					for _, ck := range childKeys {
						cv, ok := views[ck]
						if !ok || !s.pluginItemAllowed(ctx, viewer, cv) {
							return core.ErrNotFound
						}
						if cv.Playable {
							keys = append(keys, ck)
						}
					}
				}
				if page.NextCursor == "" || len(page.Items) == 0 || page.NextCursor == cursor {
					break
				}
				cursor = page.NextCursor
			}
		}
		if len(keys) == 0 {
			return nil
		}
		if len(keys) == 1 {
			if played {
				return s.d.State.Scrobble(ctx, viewer.ID, keys[0])
			}
			return s.d.State.Unscrobble(ctx, viewer.ID, keys[0])
		}
		batch, ok := s.d.State.(core.BatchWatchState)
		if !ok {
			return errors.New("experience: atomic batch watch updates unavailable")
		}
		if played {
			return batch.ScrobbleMany(ctx, viewer.ID, keys)
		}
		return batch.UnscrobbleMany(ctx, viewer.ID, keys)
	}
	if _, err = s.playable(ctx, viewer, k); err != nil {
		return err
	}
	keys := []core.RatingKey{k}
	if k.Kind() == core.KindShow || k.Kind() == core.KindSeason {
		var children []core.Title
		if k.Kind() == core.KindShow {
			if s.d.Grandchildren == nil {
				return errors.New("experience: episode expansion unavailable")
			}
			children, err = s.d.Grandchildren(ctx, viewer, k)
		} else {
			children, err = s.d.Children(ctx, viewer, k)
		}
		if err != nil {
			return err
		}
		keys = nil
		for _, child := range children {
			if child.Type != core.TypeEpisode {
				continue
			}
			if _, err = s.playable(ctx, viewer, child.Key); err != nil {
				return err
			}
			keys = append(keys, child.Key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	if len(keys) == 1 {
		if played {
			return s.d.State.Scrobble(ctx, viewer.ID, keys[0])
		}
		return s.d.State.Unscrobble(ctx, viewer.ID, keys[0])
	}
	batch, ok := s.d.State.(core.BatchWatchState)
	if !ok {
		return errors.New("experience: atomic batch watch updates unavailable")
	}
	if played {
		return batch.ScrobbleMany(ctx, viewer.ID, keys)
	}
	return batch.UnscrobbleMany(ctx, viewer.ID, keys)
}
func (s *Service) SetPlayed(ctx context.Context, v *core.Viewer, k core.RatingKey) error {
	return s.setPlayed(ctx, v, k, true)
}
func (s *Service) SetUnplayed(ctx context.Context, v *core.Viewer, k core.RatingKey) error {
	return s.setPlayed(ctx, v, k, false)
}

func (s *Service) SetOfflineProgress(ctx context.Context, viewer *core.Viewer, key core.RatingKey, sample frontend.OfflineProgress) error {
	writer, ok := s.d.State.(frontend.OfflineProgressState)
	if !ok {
		return core.ErrNotAvailable
	}
	return s.mutate(ctx, viewer, key, func(u *core.User) error {
		_, err := writer.ApplyOfflineProgress(ctx, u.ID, key, sample)
		return err
	})
}

var _ frontend.ReadingProgressWriter = (*Service)(nil)

func (s *Service) SetReadingProgress(ctx context.Context, v *core.Viewer, k core.RatingKey, progress frontend.ReadingProgress) error {
	ctx = state.WithOrigin(ctx, state.Origin{Surface: s.d.Protocol})
	return s.mutate(ctx, v, k, func(u *core.User) error {
		rs, ok := s.d.State.(core.ReadingState)
		if !ok || rs == nil {
			return core.ErrNotAvailable
		}
		return rs.SetReadingProgress(ctx, u.ID, k, progress)
	})
}
func (s *Service) SetNotInterested(ctx context.Context, v *core.Viewer, k core.RatingKey, on bool) error {
	return s.mutate(ctx, v, k, func(u *core.User) error {
		if s.d.SetNotInterestedSource == nil {
			return core.ErrNotAvailable
		}
		return s.d.SetNotInterestedSource(ctx, u, k, on)
	})
}
func (s *Service) SetFavorite(ctx context.Context, v *core.Viewer, k core.RatingKey, on bool) error {
	return s.mutate(ctx, v, k, func(u *core.User) error {
		if s.d.Favorites == nil {
			return core.ErrNotFound
		}
		_, err := s.d.Favorites(ctx, u, k, on)
		return err
	})
}
func (s *Service) SetWatchlist(ctx context.Context, v *core.Viewer, k core.RatingKey, on bool) error {
	return s.mutate(ctx, v, k, func(u *core.User) error {
		if s.d.SetWatchlistSource == nil {
			return core.ErrNotAvailable
		}
		return s.d.SetWatchlistSource(ctx, u, k, on)
	})
}
func (s *Service) SetRating(ctx context.Context, v *core.Viewer, k core.RatingKey, rating float64) error {
	ctx = state.WithOrigin(ctx, state.Origin{Surface: s.d.Protocol})
	return s.mutate(ctx, v, k, func(u *core.User) error { return s.d.State.Rate(ctx, u.ID, k, rating) })
}
func (s *Service) RemoveFromContinue(ctx context.Context, v *core.Viewer, k core.RatingKey) error {
	ctx = state.WithOrigin(ctx, state.Origin{Surface: s.d.Protocol})
	return s.mutate(ctx, v, k, func(u *core.User) error {
		if s.d.RemoveContinue != nil {
			return s.d.RemoveContinue(ctx, u, k)
		}
		if hider, ok := s.d.State.(interface {
			HideContinue(context.Context, int64, core.RatingKey) error
		}); ok {
			return hider.HideContinue(ctx, u.ID, k)
		}
		return core.ErrNotFound
	})
}

func (s *Service) LibraryGUIDs() frontend.LibraryGUIDs {
	if s == nil {
		return nil
	}
	if s.d.LibraryGUIDsFunc != nil {
		return s.d.LibraryGUIDsFunc()
	}
	return s.d.LibraryGUIDs
}
func (s *Service) LibraryMetadata() frontend.LibraryMetadata {
	if s == nil {
		return nil
	}
	if s.d.LibraryMetadataFunc != nil {
		return s.d.LibraryMetadataFunc()
	}
	return s.d.LibraryMetadata
}
func (s *Service) Markers(ctx context.Context, key core.RatingKey, info core.MediaInfo) ([]frontend.MarkerRange, error) {
	var src frontend.MarkerRanges
	if s != nil {
		src = s.d.Markers
	}
	return PlaybackMarkerRanges(ctx, src, key, info)
}

func (s *Service) sortTitles(ctx context.Context, items []core.Title, q frontend.ItemQuery) error {
	if q.Sort == "" {
		return nil
	}
	if q.Sort == core.SortAdded && s.d.AddedTimes != nil {
		keys := make([]core.RatingKey, len(items))
		for i := range items {
			keys[i] = items[i].Key
		}
		dates, err := s.d.AddedTimes(ctx, keys)
		if err != nil {
			return err
		}
		for i := range items {
			if date, known := dates[items[i].Key]; known {
				items[i].AddedAt = date
			}
		}
	}
	switch q.Sort {
	case core.SortTitle, core.SortReleased, core.SortAdded, core.SortYear, core.SortRating, core.SortPopularity, core.SortDuration, core.SortIndex:
	case core.SortPlayed, core.SortLastViewed, core.SortUpdated:
		// Played and updated orders come from catalogue/watch rows that
		// projected titles do not carry; preserve the listing's order.
		return nil
	default:
		return errors.New("experience: unsupported sort")
	}
	now := time.Now()
	addedTime := func(t core.Title) time.Time {
		if t.Key.IsPluginKey() && t.PluginAddedAt > 0 {
			return time.Unix(t.PluginAddedAt, 0).UTC()
		}
		return t.DateAdded(now)
	}
	// A title missing the sorted value goes last in either direction; a
	// sort never drops it or floats it to the top of an ascending list.
	missing := func(t core.Title) bool {
		switch q.Sort {
		case core.SortReleased:
			return t.Released.IsZero()
		case core.SortAdded:
			return addedTime(t).IsZero()
		case core.SortYear:
			return t.Year == 0
		case core.SortRating:
			return t.Rating == 0
		case core.SortPopularity:
			return t.Popularity == 0
		case core.SortDuration:
			return t.Runtime <= 0
		}
		return false
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if am, bm := missing(a), missing(b); am != bm {
			return bm
		}
		cmp := 0
		switch q.Sort {
		case core.SortTitle:
			cmp = strings.Compare(core.EffectiveSortTitle(a.Title, a.SortTitle), core.EffectiveSortTitle(b.Title, b.SortTitle))
		case core.SortAdded:
			ad, bd := addedTime(a), addedTime(b)
			if ad.Before(bd) {
				cmp = -1
			} else if ad.After(bd) {
				cmp = 1
			}
		case core.SortReleased:
			if a.Released.Before(b.Released) {
				cmp = -1
			} else if a.Released.After(b.Released) {
				cmp = 1
			}
		case core.SortYear:
			if a.Year < b.Year {
				cmp = -1
			} else if a.Year > b.Year {
				cmp = 1
			}
		case core.SortRating:
			if a.Rating < b.Rating {
				cmp = -1
			} else if a.Rating > b.Rating {
				cmp = 1
			}
		case core.SortPopularity:
			if a.Popularity < b.Popularity {
				cmp = -1
			} else if a.Popularity > b.Popularity {
				cmp = 1
			}
		case core.SortDuration:
			if a.Runtime < b.Runtime {
				cmp = -1
			} else if a.Runtime > b.Runtime {
				cmp = 1
			}
		case core.SortIndex:
			if a.ParentKey != b.ParentKey && (a.ParentTitle != "" || b.ParentTitle != "") {
				cmp = strings.Compare(strings.ToLower(a.ParentTitle), strings.ToLower(b.ParentTitle))
			}
			if cmp == 0 && a.ParentKey != b.ParentKey {
				if a.ParentKey < b.ParentKey {
					cmp = -1
				} else {
					cmp = 1
				}
			}
			if cmp == 0 {
				da, db := a.Disc, b.Disc
				if da <= 0 {
					da = 1
				}
				if db <= 0 {
					db = 1
				}
				if da < db {
					cmp = -1
				} else if da > db {
					cmp = 1
				}
			}
			if cmp == 0 {
				if a.ItemIndex < b.ItemIndex {
					cmp = -1
				} else if a.ItemIndex > b.ItemIndex {
					cmp = 1
				}
			}
			if cmp == 0 {
				cmp = strings.Compare(core.EffectiveSortTitle(a.Title, a.SortTitle), core.EffectiveSortTitle(b.Title, b.SortTitle))
			}
		}
		if cmp == 0 {
			return a.Key < b.Key
		}
		if q.Desc {
			return cmp > 0
		}
		return cmp < 0
	})
	return nil
}

// Similar applies policy before pagination and marks truncated candidate sets unknown.
func (s *Service) Similar(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey, q frontend.ItemQuery) (frontend.SearchPage, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.SearchPage{}, err
	}
	if _, err = s.item(ctx, v, key); err != nil {
		return frontend.SearchPage{}, err
	}
	if key.IsPluginKey() {
		return frontend.SearchPage{Page: core.Page{Items: []core.Title{}}, TotalExact: true, Continuation: frontend.ContinuationExhausted}, nil
	}
	rank := s.similarWindow(ctx, v, q)
	if rank != nil {
		ctx = core.WithRankingCandidates(ctx, rank)
	}
	candidates, err := s.similarCandidates(ctx, v, key)
	if err != nil {
		return frontend.SearchPage{}, err
	}
	if s.d.FilterRecommendations != nil {
		candidates, err = s.d.FilterRecommendations(ctx, v, candidates)
		if err != nil {
			return frontend.SearchPage{}, err
		}
	}
	candidates, err = s.visibleCandidates(ctx, v, candidates)
	if err != nil {
		return frontend.SearchPage{}, err
	}
	items := make([]core.Title, 0, len(candidates))
	for _, title := range candidates {
		ok, err := s.matchesUserQuery(ctx, v, title, q)
		if err != nil {
			return frontend.SearchPage{}, err
		}
		if ok {
			items = append(items, title)
		}
	}
	if err = s.sortTitles(ctx, items, q); err != nil {
		return frontend.SearchPage{}, err
	}
	offset := min(max(q.Offset, 0), len(items))
	end := len(items)
	if q.Limit > 0 {
		end = min(offset+q.Limit, end)
	}
	// Existing catalogue Similar may use only one upstream page even when
	// fewer than requested are returned; exhaustion is not knowable here.
	continuation := frontend.ContinuationUnknown
	exact := false
	if end < len(items) {
		continuation = frontend.ContinuationMore
	}
	page := items[offset:end]
	if rank != nil {
		if page, err = s.d.RankingDetails(ctx, page); err != nil {
			return frontend.SearchPage{}, err
		}
	}
	return frontend.SearchPage{Page: core.Page{Items: page, Total: len(items)}, TotalExact: exact, Continuation: continuation}, nil
}

// similarWindow is the ranking window Similar reads its related shelves
// with (core.WithRankingCandidates): the facts the viewer's policy reads,
// and the genres and keywords a shelf's taste ranking reads with its
// credits. Nil, for full titles, when q filters on facts, or the catalogue
// or availability cannot say.
func (s *Service) similarWindow(ctx context.Context, v *core.User, q frontend.ItemQuery) *core.SearchCandidates {
	if s.d.RankingDetails == nil || !reflect.ValueOf(q.Filter).IsZero() {
		return nil
	}
	detail, ok := s.d.Visibility.FactJudge(ctx)
	if !ok {
		return nil
	}
	return &core.SearchCandidates{Facts: s.d.Visibility.TitleFacts(v) | core.FactGenres | core.FactKeywords, Detail: detail}
}

// similarCandidates uses the detail recommender when the host wired one, so
// Jellyfin and Emby see the same policy-filtered related titles as Plex.
func (s *Service) similarCandidates(ctx context.Context, viewer *core.User, key core.RatingKey) ([]core.Title, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if s.d.Related != nil {
		hubs, err := (DetailRelated{Source: s.d.Related, Visibility: s.d.Visibility, Get: s.d.CachedGet}).Rows(ctx, viewer, key, 1000)
		if err != nil {
			return nil, err
		}
		items := make([]core.Title, 0)
		seen := map[core.RatingKey]bool{key: true}
		for _, hub := range hubs {
			for _, item := range hub.Items {
				if seen[item.Key] {
					continue
				}
				seen[item.Key] = true
				items = append(items, item)
			}
		}
		return items, nil
	}
	if s.d.SimilarSource == nil {
		return nil, core.ErrNotFound
	}
	return s.d.SimilarSource(ctx, key, 1000)
}

// Ancestors returns only authorized parents, failing closed if a parent is hidden.
func (s *Service) Ancestors(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) ([]core.Title, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	if key.IsPluginKey() {
		return s.pluginAncestors(ctx, v, key)
	}
	t, err := s.item(ctx, v, key)
	if err != nil {
		return nil, err
	}
	var keys []core.RatingKey
	if t.ShowKey != 0 {
		keys = append(keys, t.ShowKey)
		if t.Type == core.TypeEpisode {
			parent := t.ParentKey
			if parent == 0 {
				parent = t.Key.Parent()
			}
			if parent != 0 {
				keys = append(keys, parent)
			}
		}
	}
	out := make([]core.Title, 0, len(keys))
	for _, k := range keys {
		parent, err := s.item(ctx, v, k)
		if err != nil {
			return nil, err
		}
		out = append(out, *parent)
	}
	return out, nil
}

// Facets are computed only from titles the viewer may see.
func (s *Service) Facets(ctx context.Context, viewer *core.Viewer, device core.Device, viewID, name string) ([]core.FacetValue, error) {
	return s.FacetsAtLevel(ctx, viewer, device, viewID, "", name)
}

// FacetsAtLevel computes facet values for viewID, scoped to level (or protocol
// item type) when viewID names a plugin library.
func (s *Service) FacetsAtLevel(ctx context.Context, viewer *core.Viewer, device core.Device, viewID, level, name string) ([]core.FacetValue, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	skipIndexed := false
	if s.d.PluginLibraries != nil && viewID != "" {
		id, err := strconv.Atoi(viewID)
		if err != nil {
			return nil, core.ErrNotFound
		}
		lib, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(id))
		if err != nil {
			return nil, core.ErrNotFound
		}
		switch lib.Source {
		case core.LibraryPlugin, core.LibraryCollection:
			if lib.Source == core.LibraryPlugin {
				return s.pluginLibraryFacetsAtLevel(ctx, viewer, lib, level, name)
			}
			// A collection library's facet values are counted over the same
			// SQL semi-join as its page, then the walk stays the last word.
		case core.LibraryCatalog:
			if out, ok, err := s.filterLibraryFacets(ctx, viewer, lib, name); err != nil || ok {
				return out, err
			}
			skipIndexed = true
		}
	}
	if !skipIndexed {
		if out, ok, err := s.indexedFacets(ctx, viewer, viewID, name); err != nil || ok {
			return out, err
		}
	}
	page, err := s.LibraryItems(ctx, viewer, device, viewID, frontend.ItemQuery{})
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	labels := map[string]string{}
	for _, t := range page.Items {
		var values []string
		switch name {
		case "person":
			for _, group := range [][]core.Person{t.Cast, t.Directors, t.Writers, t.Crew} {
				for _, person := range group {
					if person.TMDB > 0 && person.Name != "" {
						key := strconv.FormatInt(person.TMDB, 10)
						values = append(values, key)
						if old := labels[key]; old == "" || person.Name < old {
							labels[key] = person.Name
						}
					}
				}
			}
		case "genre":
			values = t.Genres
		case "year":
			if t.Year > 0 {
				values = []string{strconv.Itoa(t.Year)}
			}
		case "decade":
			if t.Year > 0 {
				values = []string{strconv.Itoa(t.Year / 10 * 10)}
			}
		case "studio":
			values = t.Studios
		case "country":
			values = t.Countries
		case "contentRating":
			if t.ContentRating != "" {
				values = []string{t.ContentRating}
			}
		default:
			return nil, errors.New("experience: unsupported facet")
		}
		seen := map[string]bool{}
		for _, v := range values {
			if v != "" && !seen[v] {
				counts[v]++
				seen[v] = true
			}
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]core.FacetValue, 0, len(keys))
	seenIDs := make(map[string]string, len(keys))
	for _, k := range keys {
		id := facetID(name, k)
		if old, ok := seenIDs[id]; ok && old != k {
			return nil, errors.New("experience: facet identifier collision")
		}
		seenIDs[id] = k
		title := k
		if label := labels[k]; label != "" {
			title = label
		}
		out = append(out, core.FacetValue{ID: id, Key: k, Title: title, Count: counts[k]})
	}
	return out, nil
}

func (s *Service) collectionItems(ctx context.Context, v *core.User, key core.RatingKey) ([]core.Title, error) {
	upcoming := false
	if s.d.Collections != nil {
		if collection, err := s.d.Collections.Get(ctx, v, key); err == nil && collection != nil && collection.Key == key && collection.Upcoming {
			upcoming = true
		}
	}
	return s.collectionItemsFor(ctx, v, key, upcoming)
}

func (s *Service) collectionItemsFor(ctx context.Context, v *core.User, key core.RatingKey, upcoming bool) ([]core.Title, error) {
	var out []core.Title
	seen := map[core.RatingKey]bool{}
	for offset := 0; ; {
		page, err := s.d.Collections.Items(ctx, v, key, offset, 500)
		if err != nil {
			return nil, err
		}
		batch := page.Items
		if upcoming {
			batch = s.d.Visibility.UpcomingItems(ctx, v, batch, time.Now)
		} else {
			batch, err = s.allowTitles(ctx, v, batch)
			if err != nil {
				return nil, err
			}
		}
		for i := range batch {
			if !seen[batch[i].Key] {
				seen[batch[i].Key] = true
				out = append(out, batch[i])
			}
		}
		offset += len(page.Items)
		if offset >= page.Total {
			return out, nil
		}
		if len(page.Items) == 0 {
			return nil, errors.New("experience: incomplete collection page")
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
	}
}

func (s *Service) entityAllowed(ctx context.Context, v *core.User, ref frontend.FavoriteRef) bool {
	if ref.ID == "" {
		return false
	}
	switch ref.Kind {
	case frontend.FavoriteTitle:
		key, err := core.ParseRatingKey(ref.ID)
		if err != nil {
			return false
		}
		_, err = s.item(ctx, v, key)
		return err == nil
	case frontend.FavoritePerson, frontend.FavoriteCollection, frontend.FavoriteChannel:
		return s.d.CanFavoriteEntity != nil && s.d.CanFavoriteEntity(ctx, v, ref)
	default:
		return false
	}
}
func (s *Service) SetEntityFavorite(ctx context.Context, viewer *core.Viewer, ref frontend.FavoriteRef, value bool) error {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return err
	}
	if !s.entityAllowed(ctx, v, ref) {
		return core.ErrNotFound
	}
	if s.d.SetEntityFavoriteSource == nil {
		return core.ErrNotFound
	}
	return s.d.SetEntityFavoriteSource(ctx, v, ref, value)
}
func (s *Service) EntityFavorites(ctx context.Context, viewer *core.Viewer, refs []frontend.FavoriteRef) (map[frontend.FavoriteRef]bool, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	allowed := make([]frontend.FavoriteRef, 0, len(refs))
	for _, ref := range refs {
		if s.entityAllowed(ctx, v, ref) {
			allowed = append(allowed, ref)
		}
	}
	if s.d.EntityFavoritesSource == nil {
		return nil, core.ErrNotFound
	}
	values, err := s.d.EntityFavoritesSource(ctx, v, allowed)
	if err != nil {
		return nil, err
	}
	out := make(map[frontend.FavoriteRef]bool, len(allowed))
	for _, ref := range allowed {
		out[ref] = values[ref]
	}
	return out, nil
}

func containsMediaType(types []core.MediaType, want core.MediaType) bool {
	for _, typ := range types {
		if typ == want {
			return true
		}
	}
	return false
}

func (s *Service) matchesUserQuery(ctx context.Context, v *core.User, t core.Title, q frontend.ItemQuery) (bool, error) {
	if containsMediaType(q.ExcludedTypes, t.Type) {
		return false, nil
	}
	// A declared MediaBrowser type (MusicAlbum, Audio, Book, …) is a name a
	// client can include or exclude an item by, alongside its core type.
	if t.MBItemType != "" {
		for _, excluded := range q.ExcludedTypes {
			if strings.EqualFold(string(excluded), t.MBItemType) {
				return false, nil
			}
		}
	}
	if len(q.Types) > 0 || len(q.UnservedTypes) > 0 {
		found := containsMediaType(q.Types, t.Type)
		// A plugin level answers the type its declared role or MediaBrowser
		// type names (Plex type=8/9/10 ask artist/album/track), the same
		// mapping the plugin store applied when it chose levels; a music
		// artist projects as core "show", so the core type alone emptied
		// every typed plugin listing that is re-checked here (#777).
		for _, want := range q.Types {
			if t.MediaRole != "" && strings.EqualFold(string(want), string(t.MediaRole)) || t.MBItemType != "" && strings.EqualFold(string(want), t.MBItemType) {
				found = true
			}
		}
		for _, kind := range q.UnservedTypes {
			if strings.EqualFold(kind, string(t.Type)) || strings.EqualFold(kind, t.MBItemType) {
				found = true
			}
		}
		if !found {
			return false, nil
		}
	}
	if !matchesFilter(t, q.Filter) {
		return false, nil
	}
	if q.Level != "" && !strings.EqualFold(t.KindLevel, q.Level) {
		return false, nil
	}
	if q.Role != "" && !strings.EqualFold(string(t.MediaRole), q.Role) && !strings.EqualFold(t.KindLevel, q.Role) {
		return false, nil
	}
	if q.Season != nil && t.Season != *q.Season {
		return false, nil
	}

	if q.Watched != nil {
		watched, err := s.titlesWatched(ctx, v, []core.Title{t})
		if err != nil {
			return false, err
		}
		if watched[t.Key] != *q.Watched {
			return false, nil
		}
	}
	if q.Favorite != nil {
		if s.d.IsFavorite == nil {
			return false, errors.New("experience: favorites unavailable")
		}
		favorite, err := s.d.IsFavorite(ctx, v, t.Key)
		if err != nil {
			if t.Key.IsPluginKey() {
				s.logPluginEnrichError("favorites", t.Key, err)
				favorite = false
			} else {
				return false, err
			}
		}
		if favorite != *q.Favorite {
			return false, nil
		}
	}
	return true, nil
}

func matchesFilter(t core.Title, f core.Filter) bool {
	enf := contentrating.EffectiveEnforcement(t.EnforcementRating, t.ContentRating, t.Adult, t.Keywords)
	if f.MaxContentRating != "" && contentrating.EnforceExceeds(enf, f.MaxContentRating) {
		return false
	}
	if enf == contentrating.UncertifiedAdult && (len(f.ContentRatings) > 0 || len(f.ExcludeContentRatings) > 0 || f.MinContentRating != "") {
		return false
	}
	if f.MinContentRating != "" {
		minLvl := contentrating.EnforceLevelOf(f.MinContentRating)
		haveLvl := contentrating.EnforceLevelOf(enf)
		if haveLvl == contentrating.Unrated {
			haveLvl = contentrating.EnforceLevelOf(t.ContentRating)
		}
		if minLvl == contentrating.Unrated || haveLvl == contentrating.Unrated || haveLvl < minLvl {
			return false
		}
	}
	for _, key := range f.ExcludeKeys {
		if t.Key == key {
			return false
		}
	}
	if f.Type != "" && t.Type != f.Type {
		return false
	}
	if f.YearFrom > 0 && t.Year < f.YearFrom {
		return false
	}
	if f.YearTo > 0 && t.Year > f.YearTo {
		return false
	}
	if len(f.Years) > 0 && !slices.Contains(f.Years, t.Year) || slices.Contains(f.ExcludeYears, t.Year) {
		return false
	}
	if !releasedMatches(t, f.ReleasedFilters) {
		return false
	}
	if f.MinRating > 0 && t.Rating < f.MinRating {
		return false
	}
	if f.MaxRating > 0 && t.Rating > f.MaxRating {
		return false
	}
	if len(f.ExcludeRatings) > 0 && slices.Contains(f.ExcludeRatings, t.Rating) {
		return false
	}
	if f.MinRuntime > 0 && t.Runtime < f.MinRuntime {
		return false
	}
	if f.MaxRuntime > 0 && t.Runtime > f.MaxRuntime {
		return false
	}
	if len(f.ExcludeRuntimes) > 0 && slices.Contains(f.ExcludeRuntimes, t.Runtime) {
		return false
	}
	intersects := func(a, b []string) bool {
		if len(b) == 0 {
			return true
		}
		for _, x := range a {
			for _, y := range b {
				if strings.EqualFold(x, y) {
					return true
				}
			}
		}
		return false
	}
	hasExcluded := func(a, b []string) bool {
		return len(b) > 0 && intersects(a, b)
	}
	if !intersects(t.Genres, f.Genres) || !intersects([]string{t.Language}, f.Languages) || !intersects(t.Countries, f.Countries) || (len(f.ContentRatings) > 0 && !matchesFilterRatings(t.ContentRating, enf, f.ContentRatings)) {
		return false
	}
	if hasExcluded(t.Genres, f.ExcludeGenres) || hasExcluded(t.Countries, f.ExcludeCountries) || (len(f.ExcludeContentRatings) > 0 && matchesFilterRatings(t.ContentRating, enf, f.ExcludeContentRatings)) {
		return false
	}
	if f.Collection != 0 && len(f.Collections) == 0 && t.CollectionID != f.Collection {
		return false
	}
	if len(f.Collections) > 0 && !slices.Contains(f.Collections, t.CollectionID) {
		return false
	}
	if len(f.ExcludeCollections) > 0 && slices.Contains(f.ExcludeCollections, t.CollectionID) {
		return false
	}
	if f.Studio != "" && len(f.StudioFilters) == 0 && !intersects(t.Studios, []string{f.Studio}) {
		return false
	}
	if !studioFiltersMatch(t.Studios, f.StudioFilters) {
		return false
	}
	if f.Query != "" && !titleContainsQuery(t, f.Query) {
		return false
	}
	if !titleFiltersMatch(t, f.TitleFilters) {
		return false
	}
	if f.Person != 0 && len(f.Credits) == 0 {
		found := false
		for _, group := range [][]core.Person{t.Cast, t.Directors, t.Writers, t.Crew} {
			for _, p := range group {
				if p.TMDB == f.Person {
					found = true
				}
			}
		}
		if !found {
			return false
		}
	}
	for _, cf := range f.Credits {
		if len(cf.People) == 0 && len(cf.Names) == 0 {
			continue
		}
		var groups [][]core.Person
		switch cf.Kind {
		case "cast":
			groups = [][]core.Person{t.Cast}
		case "director":
			groups = [][]core.Person{t.Directors}
		case "writer":
			groups = [][]core.Person{t.Writers}
		case "crew":
			groups = [][]core.Person{t.Crew}
		default:
			groups = [][]core.Person{t.Cast, t.Directors, t.Writers, t.Crew}
		}
		matched := false
		for _, group := range groups {
			for _, p := range group {
				if slices.Contains(cf.People, p.TMDB) {
					matched = true
					break
				}
				for _, name := range cf.Names {
					if strings.EqualFold(strings.TrimSpace(p.Name), strings.TrimSpace(name)) {
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
			if matched {
				break
			}
		}
		if matched == cf.Exclude {
			return false
		}
	}
	return true
}

func matchesFilterRatings(rating, enf string, values []string) bool {
	have := strings.TrimSpace(rating)
	haveLevel := contentrating.EnforceLevelOf(enf)
	if haveLevel == contentrating.Unrated {
		haveLevel = contentrating.EnforceLevelOf(have)
	}
	nonUSOrUnratedDisplay := have == "" || contentrating.IsNotRated(have) || contentrating.Parse(have).Country != "US"
	for _, want := range values {
		w := strings.TrimSpace(want)
		if w == "" || strings.EqualFold(w, "None") || strings.EqualFold(w, "NR") {
			if enf == "" && haveLevel == contentrating.Unrated && (have == "" || contentrating.IsNotRated(have)) {
				return true
			}
			continue
		}
		if strings.EqualFold(have, w) {
			return true
		}
		if nonUSOrUnratedDisplay && contentrating.Parse(w).Country == "US" {
			wantLevel := contentrating.EnforceLevelOf(w)
			if wantLevel == contentrating.Unrated {
				continue
			}
			if haveLevel == wantLevel {
				return true
			}
			if strings.EqualFold(w, "TV-MA") && haveLevel == contentrating.Adult {
				return true
			}
		}
	}
	return false
}

func titleContainsQuery(t core.Title, q string) bool {
	needle := strings.ToLower(q)
	if strings.Contains(strings.ToLower(t.Title), needle) || strings.Contains(strings.ToLower(t.OriginalTitle), needle) {
		return true
	}
	for _, alt := range t.AlternativeTitles {
		if strings.Contains(strings.ToLower(alt), needle) {
			return true
		}
	}
	return false
}

func studioFiltersMatch(studios []string, filters []core.StringFilter) bool {
	for _, sf := range filters {
		val := strings.ToLower(sf.Value)
		switch sf.Op {
		case "==":
			ok := false
			for _, s := range studios {
				if strings.EqualFold(s, sf.Value) {
					ok = true
					break
				}
			}
			if !ok {
				return false
			}
		case "!==":
			for _, s := range studios {
				if strings.EqualFold(s, sf.Value) {
					return false
				}
			}
		case "!=":
			for _, s := range studios {
				if strings.Contains(strings.ToLower(s), val) {
					return false
				}
			}
		case "<=":
			ok := false
			for _, s := range studios {
				if strings.HasPrefix(strings.ToLower(s), val) {
					ok = true
					break
				}
			}
			if !ok {
				return false
			}
		case ">=":
			ok := false
			for _, s := range studios {
				if strings.HasSuffix(strings.ToLower(s), val) {
					ok = true
					break
				}
			}
			if !ok {
				return false
			}
		default:
			ok := false
			for _, s := range studios {
				if strings.Contains(strings.ToLower(s), val) {
					ok = true
					break
				}
			}
			if !ok {
				return false
			}
		}
	}
	return true
}

func titleFiltersMatch(t core.Title, filters []core.StringFilter) bool {
	titleLower := strings.ToLower(t.Title)
	for _, tf := range filters {
		val := strings.ToLower(tf.Value)
		switch tf.Op {
		case "==":
			if !strings.EqualFold(t.Title, tf.Value) {
				return false
			}
		case "!==":
			if strings.EqualFold(t.Title, tf.Value) {
				return false
			}
		case "!=":
			if strings.Contains(titleLower, val) {
				return false
			}
		case "<=":
			if !strings.HasPrefix(titleLower, val) {
				return false
			}
		case ">=":
			if !strings.HasSuffix(titleLower, val) {
				return false
			}
		default:
			if !titleContainsQuery(t, tf.Value) {
				return false
			}
		}
	}
	return true
}

// releasedMatches is catalog browseWhere's release-date predicates, for
// pages the catalogue's SQL did not read. An unknown date matches none.
func releasedMatches(t core.Title, filters []core.DateFilter) bool {
	if len(filters) == 0 {
		return true
	}
	if t.Released.IsZero() {
		return false
	}
	const day = 86400
	ts := t.Released.Unix()
	for _, rf := range filters {
		var ok bool
		switch rf.Op {
		case "<<":
			ok = ts < rf.Unix
		case "<=":
			end := rf.Unix
			if rf.SpanDay {
				end += day - 1
			}
			ok = ts <= end
		case ">>":
			ok = ts > rf.Unix
		case ">=":
			ok = ts >= rf.Unix
		case "!=", "!==":
			ok = ts != rf.Unix
			if rf.SpanDay {
				ok = ts < rf.Unix || ts >= rf.Unix+day
			}
		default:
			ok = ts == rf.Unix
			if rf.SpanDay {
				ok = ts >= rf.Unix && ts < rf.Unix+day
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// SetLikes delegates to the state's atomic shared-rating operation.
func (s *Service) SetLikes(ctx context.Context, viewer *core.Viewer, key core.RatingKey, likes *bool) error {
	ctx = core.WithCatalogViewer(ctx, viewer)
	ctx = state.WithOrigin(ctx, state.Origin{Surface: s.d.Protocol})
	return s.mutate(ctx, viewer, key, func(v *core.User) error {
		state, ok := s.d.State.(core.LikesState)
		if !ok {
			return errors.New("experience: atomic likes unavailable")
		}
		return state.SetLikes(ctx, v.ID, key, likes)
	})
}

func facetID(kind, value string) string { return core.FacetID(kind, value) }

// ResolveFacet only resolves identifiers among the viewer's current visible facets.
func (s *Service) ResolveFacet(ctx context.Context, viewer *core.Viewer, device core.Device, viewID, kind, id string) (core.FacetValue, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	isBuiltinView := true
	if s.d.PluginLibraries != nil && viewID != "" {
		if lid, err := strconv.Atoi(viewID); err == nil {
			// Ordinary catalogue libraries share the indexed studio IDs;
			// resolving one must not enumerate the entire vocabulary.
			if lib, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(lid)); err == nil && lib.Source != core.LibraryBuiltin && !(kind == "studio" && lib.Source == core.LibraryCatalog) {
				isBuiltinView = false
			}
		}
	}
	if kind == "person" {
		tmdb, ok, err := s.personFacetTMDB(ctx, id)
		if err != nil {
			return core.FacetValue{}, err
		}
		if ok {
			if found, ok, err := s.resolvePersonFacet(ctx, viewer, viewID, id, tmdb); err != nil || ok {
				return found, err
			}
		} else if isBuiltinView && s.d.PersonByFacetID != nil {
			// The person vocabulary names every credited person: an id it
			// does not know is no one's.
			return core.FacetValue{}, core.ErrNotFound
		}
	}
	if isBuiltinView {
		if kind == "studio" && s.d.StudioByFacetID != nil {
			// The studio vocabulary carries each name's identifier: one indexed
			// read, where the list of every studio of the library was hashed
			// to find it. Which titles the viewer may see under it is the
			// filtered page's business.
			return s.resolveStudio(ctx, viewer, viewID, id)
		}
		if name := sqlFacets[kind]; name != "" && s.d.FacetFilter != nil {
			// A genre, studio or country id names a value; which titles the
			// viewer may see under it is the filtered page's business. The id
			// resolves against the library-wide values, one cached count for
			// every viewer, not a count of this viewer's whole library.
			if found, ok, err := s.resolveGlobalFacet(ctx, viewer, viewID, kind, name, id); err != nil || ok {
				return found, err
			}
		}
	}
	facets, err := s.Facets(ctx, viewer, device, viewID, kind)
	if err != nil {
		return core.FacetValue{}, err
	}
	var found *core.FacetValue
	for i := range facets {
		if facets[i].ID == id {
			if found != nil {
				return core.FacetValue{}, errors.New("experience: facet identifier collision")
			}
			found = &facets[i]
		}
	}
	if found == nil {
		return core.FacetValue{}, core.ErrNotFound
	}
	return *found, nil
}

func (s *Service) IssueRowReceipts(ctx context.Context, req frontend.RowRequest, rows []frontend.Row) ([]frontend.Row, error) {
	v, err := s.viewer(ctx, req.Viewer)
	if err != nil {
		return nil, err
	}
	req.Viewer = v
	return s.d.Rows.IssueRowReceipts(ctx, req, s.d.Protocol, rows)
}

// wantsCataloguePerson reports a filter that asks for a catalogue (TMDB)
// person's works: Filter.Person (Jellyfin, Emby, Vela) or a required credit
// that names only catalogue people (Plex's /library/people/{id}/media). No
// plugin item carries such a credit, and a plugin library drops filters it
// has not declared, so it would otherwise list everything.
func wantsCataloguePerson(f core.Filter) bool {
	if f.Person != 0 && !core.IsPluginPersonID(f.Person) {
		return true
	}
	for _, c := range f.Credits {
		if c.Exclude || len(c.People) == 0 || len(c.Names) > 0 {
			continue
		}
		plugin := false
		for _, id := range c.People {
			if core.IsPluginPersonID(id) {
				plugin = true
				break
			}
		}
		if !plugin {
			return true
		}
	}
	return false
}
