package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/reqmemo"
	"github.com/braedonsaunders/ether/internal/store"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// settledReleaseAge is how long after theatrical/series release a movie or an
// ended TV show's detailed metadata (credits, keywords, runtime, certification)
// is treated as immutable and kept forever without re-querying TMDB.
const (
	settledReleaseAge  = 90 * 24 * time.Hour
	activeSeasonTTL    = time.Hour
	caughtUpRefreshTTL = 15 * time.Minute
	// refreshMissTTL is how long a failed metadata read is not retried.
	// Library lists call Get per row; without this, one outage becomes one
	// network wait per title.
	refreshMissTTL = time.Minute
)

func showRefreshKey(id int64) string  { return fmt.Sprintf("show:%d", id) }
func movieRefreshKey(id int64) string { return fmt.Sprintf("movie:%d", id) }
func seasonRefreshKey(id int64, season int) string {
	return fmt.Sprintf("season:%d:%d", id, season)
}

func (c *Catalog) refreshMissed(key string) bool {
	c.refreshMissMu.Lock()
	defer c.refreshMissMu.Unlock()
	at, ok := c.refreshMiss[key]
	if !ok {
		return false
	}
	if c.now().Sub(at) >= refreshMissTTL {
		delete(c.refreshMiss, key)
		return false
	}
	return true
}

func (c *Catalog) clearRefreshMiss(key string) {
	c.refreshMissMu.Lock()
	delete(c.refreshMiss, key)
	c.refreshMissMu.Unlock()
}

func (c *Catalog) noteRefreshMiss(key string) {
	c.refreshMissMu.Lock()
	defer c.refreshMissMu.Unlock()
	if c.refreshMiss == nil {
		c.refreshMiss = map[string]time.Time{}
	}
	c.refreshMiss[key] = c.now()
}

func (c *Catalog) fresh(st *storedTitle) bool {
	// detailAt, not fetchedAt: summary upserts from sync, lists and search
	// refresh fetchedAt and would otherwise keep popular titles' details
	// (seasons, credits, release dates) from ever being refetched.
	if st == nil || !st.detail || st.detailAt.IsZero() {
		return false
	}
	now := c.now()
	if now.Sub(st.detailAt) < c.staleAfter {
		return true
	}
	switch st.t.Type {
	case core.TypeMovie:
		if !st.t.Released.IsZero() && now.Sub(st.t.Released) > settledReleaseAge {
			return true
		}
	case core.TypeShow:
		if (strings.EqualFold(st.t.Status, "Ended") || strings.EqualFold(st.t.Status, "Canceled")) &&
			!st.t.Released.IsZero() && now.Sub(st.t.Released) > settledReleaseAge {
			return true
		}
	}
	return false
}

// Get returns a title from the mirror, fetching on demand when missing/stale.
func (c *Catalog) Get(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	t, err := c.get(ctx, key)
	if err == nil {
		err = c.projectSingle(ctx, t)
	}
	if err == nil && !c.visibleTitle(t) {
		return nil, core.ErrNotFound
	}
	// A season none of whose episodes has aired is not listed (seasonsOfShow)
	// and must not open directly either.
	if t != nil {
		c.kickMovieYears([]core.Title{*t})
	}
	return t, err
}

// upcomingMovie permits metadata browsing only; playback still enforces release availability.
func (c *Catalog) upcomingMovie(t *core.Title) bool {
	return t != nil && c.config().ShowUpcomingMovies && t.Type == core.TypeMovie && t.IsFutureRelease(c.now())
}

func (c *Catalog) visibleTitle(t *core.Title) bool {
	return t != nil && !t.Adult && (c.upcomingMovie(t) || (selectedEpisodeReleased(t, c.now()) && (!(c.config().ReleaseMode == config.ModeDigital) || c.homeVisible(t)))) && (t.Type != core.TypeSeason || listableSeason(t, c.now()))
}

// CachedGet applies the same catalogue visibility gates as Get without a
// network refresh. Missing or hidden records fail closed; callers may use
// the normal catalogue path separately to populate the local mirror.
func (c *Catalog) CachedGet(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	key, err := c.canonicalSourceKey(ctx, key)
	if err != nil {
		return nil, err
	}
	stored, err := c.loadByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if err := c.projectSingle(ctx, &stored.t); err != nil {
		return nil, err
	}
	if !c.visibleTitle(&stored.t) {
		return nil, core.ErrNotFound
	}
	c.kickMovieYears([]core.Title{stored.t})
	return &stored.t, nil
}

func (c *Catalog) StoredTitle(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	t, err := c.storedTitle(ctx, key)
	if err != nil {
		return nil, err
	}
	if err := c.projectSingle(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

func (c *Catalog) storedTitle(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	key, err := c.canonicalSourceKey(ctx, key)
	if err != nil {
		return nil, err
	}
	st, err := c.loadByKey(ctx, key)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			if summary, ok := c.summaryTitle(key); ok {
				return &summary, nil
			}
		}
		return nil, err
	}
	return &st.t, nil
}

// RefreshStoredTitle drops the minute cache and reads the stored row again.
// Portrait bytes use it when a credit changed inside the current cache minute.
func (c *Catalog) RefreshStoredTitle(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	c.cacheMu.Lock()
	delete(c.titleCache, key)
	c.cacheMu.Unlock()
	return c.StoredTitle(ctx, key)
}

func (c *Catalog) get(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	if key.IsTitleKey() && (key.Allocated() || c.metadataSourceDefault.Load() != nil || !c.MetadataSources().Get(core.MetadataTMDB).Configured()) {
		t, err := c.storedTitle(ctx, key)
		if err == nil {
			if len(c.metadataSources.Configured()) == 0 || (noReadWait(ctx) && !core.ColdDetail(ctx, t.Key)) {
				return t, nil
			}
			source, err := c.titleDetailsSource(ctx, t)
			if err != nil {
				return nil, err
			}
			if t.Key.Allocated() || source != "" && source != core.MetadataTMDB {
				return c.sourceTitleDetails(ctx, t, source, false)
			}
			key = t.Key
		} else if !errors.Is(err, core.ErrNotFound) {
			return nil, err
		} else if key.Allocated() || !c.MetadataSources().Get(core.MetadataTMDB).Configured() {
			typ := core.TypeShow
			if key.Kind() == core.KindMovie {
				typ = core.TypeMovie
			}
			return c.openSourceTitle(ctx, key, typ)
		}
	}
	if (key.Kind() == core.KindSeason || key.Kind() == core.KindEpisode) && (key.Allocated() || c.canProjectAlternateOrders()) {
		parent, show, order, err := c.orderForParent(ctx, key)
		if err != nil {
			return nil, err
		}
		if parent != nil && show != nil && providerLayout(show, order) {
			if key.Kind() == core.KindSeason && !noReadWait(ctx) {
				if season, err := c.orderSeasonByKey(ctx, show.Key, order, key); err == nil {
					if err := c.ensureOrderSeason(ctx, show, order, season); err != nil {
						return nil, err
					}
				} else if !errors.Is(err, core.ErrNotFound) {
					return nil, err
				}
			}
			return parent, nil
		}
		if key.Allocated() {
			if parent != nil {
				return parent, nil
			}
			return nil, core.ErrNotFound
		}
	}

	if noReadWait(ctx) && (key.Kind() == core.KindShow || key.Kind() == core.KindSeason || key.Kind() == core.KindEpisode) {
		defer c.reportDetailPending(ctx, key)
		if core.ColdDetail(ctx, key) {
			return c.localDetail(ctx, key)
		}
		c.loadSeasonsLater(ctx, key.TMDB(), nil)
		return c.StoredTitle(ctx, key)
	}

	switch key.Kind() {
	case core.KindMovie:
		return c.getMovie(ctx, key.TMDB())
	case core.KindShow:
		return c.getShow(ctx, key.TMDB())
	case core.KindSeason:
		return c.getSeason(ctx, key)
	case core.KindEpisode:
		return c.getEpisode(ctx, key)
	default:
		return nil, fmt.Errorf("%w: key %s", core.ErrNotFound, key)
	}
}

func (c *Catalog) getMovie(ctx context.Context, id int64) (*core.Title, error) {
	key := core.MovieKey(id)
	st, err := c.loadByKey(ctx, key)
	if err == nil && c.fresh(st) {
		return &st.t, nil
	}
	// A list summary is a read-through fallback, never a foreground mirror.
	summary, fromList := c.listSummary(key)
	if errors.Is(err, core.ErrNotFound) && fromList {
		st = &storedTitle{t: summary}
		err = nil
	}
	if !c.TMDBConfigured() {
		if err != nil {
			return nil, err
		}
		return &st.t, nil
	}
	if c.refreshMissed(movieRefreshKey(id)) {
		if err == nil {
			return &st.t, nil
		}
		return nil, core.ErrNotFound
	}
	var fetched *core.Title
	fetch := func(ctx context.Context) error {
		d, err := c.tmdb().MovieDetails(ctx, id)
		if err != nil {
			c.emitMetadataError(key, core.TypeMovie, err)
			return err
		}
		t := c.movieDetailsToTitle(ctx, d)
		fetched = &t
		var crowd *detailExtra
		if d.Recommendations != nil {
			crowd = c.crowdExtra(ctx, key, movieCrowdTitles(c, ctx, d.Recommendations))
		}
		return c.writeDetail(ctx, t, upsertOpts{browsable: c.meetsThreshold(t), detail: true, releaseDates: d.ReleaseDates}, crowd)
	}
	haveDetails := err == nil && st.detail
	wait := c.listReadWait(haveDetails, fromList)
	hydrateCtx := ctx
	if !haveDetails && noReadWait(ctx) && core.ColdDetail(ctx, key) {
		wait = hydrateWait
		hydrateCtx = WithReadWaitBudget(ctx, hydrateWait)
	}
	if ferr := c.hydrate(hydrateCtx, movieRefreshKey(id), wait, fetch); ferr != nil {
		if err == nil {
			if !haveDetails && core.ColdDetail(ctx, key) {
				core.ReportDetailPending(ctx, key, "details")
			}
			return &st.t, nil // serve stored details while the refresh runs or after it failed
		}
		return nil, ferr
	}
	if fetched != nil && warmOnly(ctx) {
		return fetched, nil
	}
	st2, err := c.loadByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	return &st2.t, nil
}

func (c *Catalog) emitMetadataError(key core.RatingKey, mediaType core.MediaType, err error) {
	if c == nil || c.onMetadataError == nil || err == nil {
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, tmdb.ErrNoKey) || errors.Is(err, core.ErrNotFound) {
		return
	}
	c.onMetadataError(MetadataErrorEvent{
		Provider:  "tmdb",
		TitleKey:  key,
		MediaType: mediaType,
		Error:     err.Error(),
	})
}

// fetchMovieDetails fetches and stores a movie's full details.
func (c *Catalog) fetchMovieDetails(ctx context.Context, id int64) error {
	d, err := c.tmdb().MovieDetails(ctx, id)
	if err != nil {
		c.emitMetadataError(core.MovieKey(id), core.TypeMovie, err)
		return err
	}
	t := c.movieDetailsToTitle(ctx, d)
	var crowd *detailExtra
	if d.Recommendations != nil {
		crowd = c.crowdExtra(ctx, core.MovieKey(id), movieCrowdTitles(c, ctx, d.Recommendations))
	}
	return c.writeDetail(ctx, t, upsertOpts{browsable: c.meetsThreshold(t), detail: true, releaseDates: d.ReleaseDates}, crowd)
}

// fetchShowDetails fetches and stores a series' full details and seasons.
func (c *Catalog) fetchShowDetails(ctx context.Context, id int64) error {
	d, err := c.tmdb().TVDetails(ctx, id)
	if err != nil {
		c.emitMetadataError(core.ShowKey(id), core.TypeShow, err)
		return err
	}
	t := c.tvDetailsToTitle(ctx, d)
	var crowd *detailExtra
	if d.Recommendations != nil {
		crowd = c.crowdExtra(ctx, core.ShowKey(id), tvCrowdTitles(c, ctx, d.Recommendations))
	}
	return c.storeShowDetails(ctx, t, d, crowd)
}

// readWait is how long a read waits for a TMDB refresh: not at all when it
// already has stored details to serve, otherwise at most hydrateWait.
func (c *Catalog) readWait(haveDetails bool) time.Duration {
	if haveDetails {
		return 0
	}
	return hydrateWait
}

// storeShowDetails mirrors a show's details and its season summaries, so a
// season TMDB has added since the last fetch becomes listable.
func (c *Catalog) storeShowDetails(ctx context.Context, t core.Title, d *tmdb.TVDetails, extra ...*detailExtra) error {
	var seasons []core.Title
	for _, s := range d.Seasons {
		if !episodeKeyFits(s.SeasonNumber, 0) {
			continue // its episodes have no rating keys (year-numbered seasons)
		}
		seasons = append(seasons, seasonToTitle(t, s))
	}
	if err := c.dateSeasonSummaries(ctx, t, d, seasons); err != nil {
		return err
	}
	// Upsert show as detail; seasons as non-detail summaries in same call would
	// mark them detail too, so split.
	if err := c.writeDetail(ctx, t, upsertOpts{browsable: c.meetsThreshold(t), detail: true}, extra...); err != nil {
		return err
	}
	if len(seasons) > 0 {
		return c.upsertSeasonRows(ctx, seasons, upsertOpts{airDates: true})
	}
	return nil
}

// showFresh shortens a Returning Series' detail TTL to activeSeasonTTL, as
// seasonFresh does for its latest season: the show details carry the season
// list, so a premiere is listed within the hour rather than after staleAfter.
func (c *Catalog) showFresh(st *storedTitle) bool {
	if !c.fresh(st) {
		return false
	}
	if strings.EqualFold(st.t.Status, "Returning Series") {
		return c.now().Sub(st.detailAt) < min(c.staleAfter, activeSeasonTTL)
	}
	return true
}

func (c *Catalog) getShow(ctx context.Context, id int64) (*core.Title, error) {
	key := core.ShowKey(id)
	st, err := c.loadByKey(ctx, key)
	if err == nil && c.showFresh(st) {
		return &st.t, nil
	}
	// A list summary is a read-through fallback, never a foreground mirror.
	summary, fromList := c.listSummary(key)
	if errors.Is(err, core.ErrNotFound) && fromList {
		st = &storedTitle{t: summary}
		err = nil
	}
	if !c.TMDBConfigured() {
		if err != nil {
			return nil, err
		}
		return &st.t, nil
	}
	if c.refreshMissed(showRefreshKey(id)) {
		if err == nil {
			return &st.t, nil
		}
		return nil, core.ErrNotFound
	}
	var fetched *core.Title
	fetch := func(ctx context.Context) error {
		d, err := c.tmdb().TVDetails(ctx, id)
		if err != nil {
			c.emitMetadataError(key, core.TypeShow, err)
			return err
		}
		t := c.tvDetailsToTitle(ctx, d)
		fetched = &t
		var crowd *detailExtra
		if d.Recommendations != nil {
			crowd = c.crowdExtra(ctx, key, tvCrowdTitles(c, ctx, d.Recommendations))
		}
		return c.storeShowDetails(ctx, t, d, crowd)
	}
	if ferr := c.hydrate(ctx, showRefreshKey(id), c.listReadWait(err == nil && st.detail, fromList), fetch); ferr != nil {
		if err == nil {
			return &st.t, nil
		}
		return nil, ferr
	}
	if fetched != nil && warmOnly(ctx) {
		return fetched, nil
	}
	st2, err := c.loadByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	return &st2.t, nil
}

// ensureShow returns the show title, fetching if needed.
func (c *Catalog) ensureShow(ctx context.Context, showID int64) (*core.Title, error) {
	return c.getShow(ctx, showID)
}

func (c *Catalog) getSeason(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	_, err := c.fetchSeason(ctx, key.TMDB(), key.Season())
	if err != nil {
		// Fall back to stored season summary.
		if st, lerr := c.loadByKey(ctx, key); lerr == nil {
			return &st.t, nil
		}
		return nil, err
	}
	st, err := c.loadByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	return &st.t, nil
}

// seasonFresh uses a shorter TTL (1h) for the latest season of an active
// Returning Series so shows airing multiple times a week (e.g. Sun/Wed/Thu)
// pick up same-day TMDB episode/air-date additions without waiting 24h.
func (c *Catalog) seasonFresh(st *storedTitle, show *core.Title) bool {
	if !c.fresh(st) {
		return false
	}
	if show != nil && strings.EqualFold(show.Status, "Returning Series") && st.t.Season >= show.ChildCount {
		ttl := min(c.staleAfter, activeSeasonTTL)
		return c.now().Sub(st.detailAt) < ttl
	}
	return true
}

// fetchSeason ensures season+episodes are in the mirror, fetching TMDB when
// missing or stale. Returns the season title.
func (c *Catalog) fetchSeason(ctx context.Context, showID int64, season int) (*core.Title, error) {
	return c.fetchSeasonOpts(ctx, showID, season, false, false)
}

// fetchSeasonOpts refreshes a season; force waits for TMDB, and
// knownMirrored says the caller already holds one of its episodes.
func (c *Catalog) fetchSeasonOpts(ctx context.Context, showID int64, season int, force, knownMirrored bool) (*core.Title, error) {
	key := core.SeasonKey(showID, season)
	show, err := c.ensureShow(ctx, showID)
	if err != nil {
		return nil, err
	}
	// If season row is fresh-detailed and episodes exist, skip network.
	// Mirrored, not aired: a fresh season whose episodes are all still
	// to air must not refetch on every read.
	stored, lerr := c.loadByKey(ctx, key)
	mirrored := knownMirrored
	if lerr == nil && !mirrored {
		mirrored, _ = c.seasonMirrored(ctx, showID, season, max(1, stored.t.ChildCount))
	}
	if !force && lerr == nil && mirrored && c.seasonFresh(stored, show) {
		return &stored.t, nil
	}
	if !c.TMDBConfigured() {
		if lerr != nil {
			return nil, lerr
		}
		return &stored.t, nil
	}
	if c.refreshMissed(seasonRefreshKey(showID, season)) {
		if lerr != nil {
			return nil, core.ErrNotFound
		}
		return &stored.t, nil
	}
	fetch := func(ctx context.Context) error {
		sd, err := c.tmdb().SeasonDetails(ctx, showID, season)
		if err != nil {
			return err
		}
		return c.storeSeasonDetails(ctx, show, sd, false)
	}
	// A forced refresh is synchronous; a mirrored season is served while it
	// refreshes; a season with no episodes stored waits briefly.
	wait := c.readWait(lerr == nil && stored.detail && mirrored)
	if deferred, _ := ctx.Value(deferAiredCountsKey{}).(bool); deferred {
		wait = hydrateBudget
	}
	if force {
		wait = hydrateBudget
	}
	if err := c.hydrate(ctx, seasonRefreshKey(showID, season), wait, fetch); err != nil {
		if lerr == nil {
			return &stored.t, nil
		}
		return nil, err
	}
	st, err := c.loadByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	return &st.t, nil
}

// storeSeasonDetails mirrors episode metadata from the season payload.
func (c *Catalog) storeSeasonDetails(ctx context.Context, show *core.Title, sd *tmdb.SeasonDetails, explicit bool) error {
	showID, season := show.IDs.TMDB, sd.SeasonNumber
	opts := upsertOpts{airDates: true, replaceMetadata: explicit}
	seasonName := sd.Name
	if seasonName == "" {
		seasonName = "Season " + itoa(season)
	}
	seasonT := seasonToTitle(*show, tmdbSeasonSummary(sd))
	eps := make([]core.Title, 0, len(sd.Episodes))
	for _, e := range sd.Episodes {
		if !episodeKeyFits(e.SeasonNumber, e.EpisodeNumber) {
			// Its rating key would decode as another title's episode.
			continue
		}
		eps = append(eps, episodeToTitle(*show, seasonName, e))
	}
	dateSeasonFromAiredEpisodes(&seasonT, eps, c.now())
	seasonT.ChildCount = len(eps)
	// The season payload supplies episode names, artwork and credits.
	// Keep the ordinary read's credit write behind initial publication.
	rows := eps
	credited := false
	if !explicit {
		rows = make([]core.Title, len(eps))
		for i, e := range eps {
			credited = credited || hasCredits(&e)
			e.Cast, e.Directors, e.Writers, e.Crew = nil, nil, nil, nil
			rows[i] = e
		}
	}
	if err := c.upsertSeasonRows(ctx, rows, opts); err != nil {
		return err
	}
	// Publish freshness only after every episode chunk committed.
	if explicit {
		keys := make([]core.RatingKey, 0, len(rows))
		for _, row := range rows {
			keys = append(keys, row.Key)
		}
		err := c.upsertItems(ctx, []upsertItem{{t: seasonT, o: upsertOpts{detail: true, replaceMetadata: true}}}, func(ctx context.Context, tx *sql.Tx) error {
			return c.removeMissingChildren(ctx, tx, show.Key, &season, keys)
		})
		if err == nil {
			c.clearRefreshMiss(seasonRefreshKey(showID, season))
		}
		return err
	}
	if err := c.upsert(ctx, seasonT, upsertOpts{detail: true}); err != nil {
		return err
	}
	if credited {
		_ = c.hydrate(ctx, seasonRefreshKey(showID, season)+":credits", 0, func(ctx context.Context) error {
			return c.upsertSeasonRows(ctx, eps, opts)
		})
	}
	return nil
}

// episodeKeyFits reports whether an episode's numbers fit its rating key
// (core.EpisodeKey: season < 1000, episode < 10000).
func episodeKeyFits(season, episode int) bool {
	return season >= 0 && season < 1000 && episode >= 0 && episode < 10_000
}

func tmdbSeasonSummary(sd *tmdb.SeasonDetails) tmdb.SeasonSummary {
	return tmdb.SeasonSummary{ID: sd.ID, Name: sd.Name, Overview: sd.Overview, SeasonNumber: sd.SeasonNumber,
		EpisodeCount: len(sd.Episodes), AirDate: sd.AirDate, PosterPath: sd.PosterPath, VoteAverage: sd.VoteAverage}
}

func (c *Catalog) getEpisode(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	stored, lerr := c.loadByKey(ctx, key)
	if lerr == nil && c.fresh(stored) {
		return &stored.t, nil
	}
	// A stored episode proves its season is mirrored, so the season check
	// needs no episode count.
	if _, err := c.fetchSeasonOpts(ctx, key.TMDB(), key.Season(), false, lerr == nil); err != nil {
		if st, lerr := c.loadByKey(ctx, key); lerr == nil {
			return &st.t, nil
		}
		return nil, err
	}
	st, err := c.loadByKey(ctx, key)
	if err != nil {
		return nil, core.ErrNotFound
	}
	// Best-effort episode external ids (its own IMDb id is useful to indexers).
	// A season refresh that just failed will fail these too; do not fan that
	// wait out across every episode in a list.
	refreshKey := "episode-ids:" + strconv.FormatUint(uint64(key), 10)
	if c.TMDBConfigured() && st.t.IDs.IMDB == "" && !c.refreshMissed(seasonRefreshKey(key.TMDB(), key.Season())) && !c.refreshing(seasonRefreshKey(key.TMDB(), key.Season())) && !c.refreshMissed(refreshKey) {
		stored := st.t
		fetch := func(ctx context.Context) error {
			ext, err := c.tmdb().EpisodeExternalIDs(ctx, key.TMDB(), key.Season(), key.Episode())
			if err != nil {
				return err
			}
			if ext == nil || (ext.IMDBID == "" && ext.TVDBID == 0) {
				c.noteRefreshMiss(refreshKey)
				return nil
			}
			t := stored
			t.IDs.IMDB = ext.IMDBID
			if ext.TVDBID != 0 {
				t.IDs.TVDB = ext.TVDBID
			}
			// Nil facets so the upsert keeps the stored rows. Keywords
			// especially: the loaded episode carries its show's (inherited
			// on read), which must not be saved as the episode's own.
			t.Cast, t.Directors, t.Writers, t.Crew = nil, nil, nil, nil
			t.Genres, t.Studios, t.Countries, t.Keywords, t.AlternativeTitles = nil, nil, nil, nil, nil
			return c.upsert(ctx, t, upsertOpts{detail: true})
		}
		if c.hydrate(ctx, refreshKey, hydrateWait, fetch) == nil {
			if st2, err := c.loadByKey(ctx, key); err == nil {
				return &st2.t, nil
			}
		}
	}
	return &st.t, nil
}

// seasonsOfShow lists a show's seasons that have aired: their own air date
// has passed and at least one episode has aired (queryTitles drops
// unreleased rows and applies aired counts).
func (c *Catalog) seasonsOfShow(ctx context.Context, showID int64) ([]core.Title, error) {
	deferred, _ := ctx.Value(deferAiredCountsKey{}).(bool)
	items, err := reqmemo.Do(ctx, fmt.Sprintf("catalog:seasons:%p:%d:%d:%t", c, showID, c.writeGen.Load(), deferred), func() ([]core.Title, error) {
		return c.readSeasonsOfShow(ctx, showID)
	})
	out := make([]core.Title, len(items))
	for i := range items {
		out[i] = cloneTitle(items[i])
	}
	return out, err
}

func (c *Catalog) readSeasonsOfShow(ctx context.Context, showID int64) ([]core.Title, error) {
	// show_key, not show_tmdb: only show_key is indexed, and a show's
	// pages read its seasons and episodes many times.
	all, err := c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles`+c.indexedBy("catalog_titles_show")+` WHERE type='season' AND show_key=? ORDER BY season ASC`, int64(core.ShowKey(showID)))
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for i := range all {
		if listableSeason(&all[i], c.now()) {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// episodesOfSeason lists a season's aired episodes; queryTitles drops those
// with unknown or future air dates.
func (c *Catalog) episodesOfSeason(ctx context.Context, showID int64, season int) ([]core.Title, error) {
	return c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles`+c.indexedBy("catalog_titles_show")+` WHERE type='episode' AND key<5000000000000000 AND show_key=? AND season=? ORDER BY episode ASC`, int64(core.ShowKey(showID)), season)
}

// airedSeasonEpisodeWhere matches the rows queryTitles keeps for a season:
// not adult, and an air date that is already known and not in the future.
const airedSeasonEpisodeWhere = `type='episode' AND key<5000000000000000 AND show_key=? AND season=? AND adult=0 AND ` + knownReleaseSQL + ` AND released_ts<=?`

// SeasonEpisodePage returns one window of a single catalogue season. The
// count and the page both use catalog_titles_show (show_key, season, episode),
// so a season of thousands of episodes does not load the rest of the show.
// limit is 1..1000. The first page checks season freshness, serving stored
// episodes while stale metadata refreshes. Later pages do not fetch.
func (c *Catalog) SeasonEpisodePage(ctx context.Context, show core.RatingKey, season, offset, limit int) (core.Page, error) {
	if show.Kind() != core.KindShow || season < 0 || offset < 0 || limit < 1 || limit > 1000 {
		return core.Page{}, fmt.Errorf("catalog: season page out of range")
	}
	_, parent, order, err := c.orderForParent(ctx, show)
	if err != nil {
		return core.Page{}, err
	}
	if parent != nil && providerLayout(parent, order) {
		if offset == 0 {
			seasons, err := c.ensureOrderManifest(ctx, parent, order)
			if err != nil {
				return core.Page{}, err
			}
			if selected, ok := orderedSeason(seasons, season); ok {
				if err := c.ensureOrderSeason(ctx, parent, order, selected); err != nil {
					return core.Page{}, err
				}
			}
		}
		return c.orderedEpisodePage(ctx, parent, order, season, offset, limit, false)
	}
	if season >= 1000 {
		return core.Page{}, core.ErrNotFound
	}

	if noReadWait(ctx) {
		key := core.SeasonKey(show.TMDB(), season)
		defer c.reportDetailPending(ctx, key)
		if core.ColdDetail(ctx, key) {
			_, _ = c.localDetail(ctx, key)
		} else {
			c.loadSeasonsLater(ctx, show.TMDB(), nil)
		}
		return c.seasonEpisodeQuery(ctx, show.TMDB(), season, offset, limit)
	}
	page, err := c.seasonEpisodeQuery(ctx, show.TMDB(), season, offset, limit)
	if err != nil || offset > 0 {
		return page, err
	}
	_, ferr := c.fetchSeason(ctx, show.TMDB(), season)
	if page.Total > 0 {
		// Existing rows must not suppress refresh: TMDB may have added an
		// episode or supplied its missing air date since the last visit.
		// Keep serving this page even if the background refresh fails.
		return page, nil
	}
	if ferr != nil {
		if errors.Is(ferr, core.ErrNotFound) {
			return core.Page{Items: []core.Title{}}, nil
		}
		return core.Page{}, ferr
	}
	return c.seasonEpisodeQuery(ctx, show.TMDB(), season, offset, limit)
}

func (c *Catalog) seasonEpisodeQuery(ctx context.Context, showID int64, season, offset, limit int) (core.Page, error) {
	showKey := int64(core.ShowKey(showID))
	now := c.now().Unix()
	var total int
	from := `catalog_titles` + c.indexedBy("catalog_titles_show")
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+from+` WHERE `+airedSeasonEpisodeWhere, showKey, season, now).Scan(&total); err != nil {
		return core.Page{}, err
	}
	if offset >= total {
		return core.Page{Items: []core.Title{}, Total: total}, nil
	}
	items, err := c.queryTitles(ctx, `SELECT `+titleCols+` FROM `+from+` WHERE `+airedSeasonEpisodeWhere+` ORDER BY episode ASC LIMIT ? OFFSET ?`, showKey, season, now, limit, offset)
	if err != nil {
		return core.Page{}, err
	}
	if items == nil {
		items = []core.Title{}
	}
	return core.Page{Items: items, Total: total}, nil
}

// seasonMirrored checks whether the mirror has the advertised number of
// episodes, including episodes which have yet to air.
func (c *Catalog) seasonMirrored(ctx context.Context, showID int64, season, count int) (bool, error) {
	var exists bool
	err := c.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM catalog_titles`+c.indexedBy("catalog_titles_show")+` WHERE type='episode' AND key<5000000000000000 AND show_key=? AND season=? LIMIT 1 OFFSET ?)`, int64(core.ShowKey(showID)), season, count-1).Scan(&exists)
	return exists, err
}

// Children: seasons of a show, episodes of a season.
func (c *Catalog) Children(ctx context.Context, key core.RatingKey) ([]core.Title, error) {
	if _, err := c.Get(ctx, key); err != nil {
		return nil, err
	}
	parent, show, order, err := c.orderForParent(ctx, key)
	if err != nil {
		return nil, err
	}
	if show != nil && providerLayout(show, order) {
		switch key.Kind() {
		case core.KindShow:
			seasons, err := c.ensureOrderManifest(ctx, show, order)
			if err != nil {
				return nil, err
			}
			out := make([]core.Title, 0, len(seasons))
			for _, season := range seasons {
				t := orderedSeasonTitle(show, order, season)
				if (t.Released.IsZero() || t.IsReleased(c.now())) && season.EpisodeCount != 0 {
					out = append(out, t)
				}
			}
			if _, found := orderedSeason(seasons, 0); !found && order.Source != core.MetadataTMDB && show.IDs.TMDB > 0 && !show.Key.Allocated() {
				if special, err := c.loadByKey(ctx, core.SeasonKey(show.IDs.TMDB, 0)); err == nil && c.visibleTitle(&special.t) {
					t := special.t
					t.ParentKey, t.EpisodeOrder = show.Key, order
					out = append([]core.Title{t}, out...)
				} else if err != nil && !errors.Is(err, core.ErrNotFound) {
					return nil, err
				}
			}
			if err := c.hydrateShows(ctx, out); err != nil {
				return nil, err
			}
			return out, nil
		case core.KindSeason:
			seasons, err := c.ensureOrderManifest(ctx, show, order)
			if err != nil {
				return nil, err
			}
			for _, selected := range seasons {
				if selected.key == key {
					if err := c.ensureOrderSeason(ctx, show, order, selected); err != nil {
						return nil, err
					}
					page, err := c.orderedEpisodePage(ctx, show, order, selected.Season, 0, max(selected.EpisodeCount+256, 1024), false)
					return page.Items, err
				}
			}
			if parent != nil && parent.Season == 0 && order.Source != core.MetadataTMDB && !key.Allocated() {
				page, err := c.orderedEpisodePage(ctx, show, order, 0, 0, math.MaxInt32, false)
				return page.Items, err
			}
			return []core.Title{}, nil
		}
	}

	switch key.Kind() {
	case core.KindShow:
		if !noReadWait(ctx) {
			if _, err := c.ensureShow(ctx, key.TMDB()); err != nil {
				return nil, err
			}
		}
		out, err := c.seasonsOfShow(ctx, key.TMDB())
		if out == nil && err == nil {
			out = []core.Title{}
		}
		return out, err
	case core.KindSeason:
		if !noReadWait(ctx) {
			if _, err := c.fetchSeason(ctx, key.TMDB(), key.Season()); err != nil {
				return nil, err
			}
		}
		out, err := c.episodesOfSeason(ctx, key.TMDB(), key.Season())
		if out == nil && err == nil {
			out = []core.Title{}
		}
		return out, err
	case core.KindMovie, core.KindEpisode:
		return []core.Title{}, nil
	default:
		return nil, fmt.Errorf("%w: key %s", core.ErrNotFound, key)
	}
}

// Grandchildren: all episodes of a show.
func (c *Catalog) Grandchildren(ctx context.Context, key core.RatingKey) ([]core.Title, error) {
	if key.Kind() != core.KindShow {
		if key.Kind() == core.KindSeason || key.Kind() == core.KindMovie || key.Kind() == core.KindEpisode {
			return []core.Title{}, nil
		}
		return nil, fmt.Errorf("%w: key %s", core.ErrNotFound, key)
	}
	_, show, order, err := c.orderForParent(ctx, key)
	if err != nil {
		return nil, err
	}
	if show != nil && providerLayout(show, order) {
		seasons, err := c.ensureOrderManifest(ctx, show, order)
		if err != nil {
			return nil, err
		}
		for _, season := range seasons {
			if d := metadataDate(season.AirDate); !d.IsZero() && d.After(c.now()) {
				continue
			}
			if err := c.ensureOrderSeason(ctx, show, order, season); err != nil {
				return nil, err
			}
		}
		page, err := c.orderedEpisodePage(ctx, show, order, -1, 0, math.MaxInt32, false)
		return page.Items, err
	}

	gen := c.writeGen.Load()
	minute := c.minuteBucket()
	c.cacheMu.RLock()
	if ent, ok := c.grandCache[key]; ok && ent.gen == gen && ent.minute == minute {
		items := make([]core.Title, len(ent.page.Items))
		for i := range ent.page.Items {
			items[i] = cloneTitle(ent.page.Items[i])
		}
		c.cacheMu.RUnlock()
		return items, nil
	}
	c.cacheMu.RUnlock()
	seasons, err := c.Children(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(seasons) == 0 {
		if !c.TMDBConfigured() {
			var seasonRows int
			if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles`+c.indexedBy("catalog_titles_show")+` WHERE type='season' AND show_key=?`, int64(key)).Scan(&seasonRows); err == nil && seasonRows == 0 {
				out, err := c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles`+c.indexedBy("catalog_titles_show")+` WHERE type='episode' AND show_key=? ORDER BY season ASC, episode ASC`, int64(key))
				if out == nil && err == nil {
					out = []core.Title{}
				}
				return out, err
			}
		}
		return []core.Title{}, nil
	}
	if err := c.loadSeasons(ctx, key.TMDB(), seasons); err != nil {
		return nil, err
	}
	// One membership bind keeps statement count fixed even for a series
	// with more seasons than SQLite's ordinary placeholder window.
	numbers := make([]core.RatingKey, len(seasons))
	for i, s := range seasons {
		numbers[i] = core.RatingKey(s.Season)
	}
	membership, args := c.keyMembership("season", false, numbers)
	args = append([]any{int64(key)}, args...)
	out, err := c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles`+c.indexedBy("catalog_titles_show")+` WHERE type='episode' AND key<5000000000000000 AND show_key=? AND `+membership+` ORDER BY season ASC, episode ASC`, args...)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []core.Title{}
	}
	if c.writeGen.Load() == gen {
		stored := make([]core.Title, len(out))
		for i := range out {
			stored[i] = cloneTitle(out[i])
		}
		c.cacheMu.Lock()
		if len(c.grandCache) >= 256 {
			c.grandCache = map[core.RatingKey]cachedBrowse{}
		}
		c.grandCache[key] = cachedBrowse{page: core.Page{Items: stored, Total: len(stored)}, gen: gen, minute: minute}
		c.cacheMu.Unlock()
	}
	return out, nil
}

// seasonLoads bounds a show's concurrent season fetches.
const seasonLoads = 6

// loadSeasons brings a show's listed seasons into the mirror for a whole-show
// read. A season is fetched unless a season fetch has mirrored it: its season
// record is detailed and it has episodes. A mirrored season is refetched when
// stale only if it can still change: it is the latest season, it has episodes
// yet to air, or the show's refreshed summary counts more episodes than the
// mirror holds (a former latest season's final episodes). Otherwise it stays
// as mirrored until its own page or episodes are opened (fetchSeason), so a
// long-running show does not refetch every past season once its details go
// stale. Fetches run concurrently.
func (c *Catalog) loadSeasons(ctx context.Context, showID int64, seasons []core.Title) error {
	if noReadWait(ctx) {
		// One bounded, deduplicated job owns cold season discovery. A detail
		// response must not re-read show/count metadata once per season, or
		// wait for the writes made by those refreshes.
		c.loadSeasonsLater(ctx, showID, seasons)
		return nil
	}
	// Apply the same active-season TTL as direct season reads. Using fresh
	// alone here kept watchlist Grandchildren reads stale for the general TTL.
	var show *core.Title
	if st, err := c.loadByKey(ctx, core.ShowKey(showID)); err == nil {
		show = &st.t
	}

	type mirrored struct{ episodes, pending int }
	have := map[int]mirrored{}
	rows, err := c.db.QueryContext(ctx, `SELECT season,COUNT(*),COUNT(CASE WHEN release_known=0 OR released_ts>? THEN 1 END)
		FROM catalog_titles`+c.indexedBy("catalog_titles_show")+` WHERE type='episode' AND show_key=? GROUP BY season`, c.now().Unix(), int64(core.ShowKey(showID)))
	if err != nil {
		return err
	}
	for rows.Next() {
		var season int
		var m mirrored
		if err := rows.Scan(&season, &m.episodes, &m.pending); err != nil {
			rows.Close()
			return err
		}
		have[season] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	type record struct {
		detailed, fresh bool
		episodes        int // the latest season summary's or fetch's count
	}
	records := map[int]record{}
	rows, err = c.db.QueryContext(ctx, `SELECT season,detail,detail_at,child_count FROM catalog_titles`+c.indexedBy("catalog_titles_show")+` WHERE type='season' AND show_key=?`, int64(core.ShowKey(showID)))
	if err != nil {
		return err
	}
	for rows.Next() {
		var season, detail, child int
		var detailAt int64
		if err := rows.Scan(&season, &detail, &detailAt, &child); err != nil {
			rows.Close()
			return err
		}
		st := storedTitle{detail: detail != 0}
		st.t.Season = season
		if detailAt != 0 {
			st.detailAt = time.Unix(detailAt, 0).UTC()
		}
		records[season] = record{detailed: st.detail && detailAt != 0, fresh: c.seasonFresh(&st, show), episodes: child}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	latest := 0
	for _, s := range seasons {
		latest = max(latest, s.Season)
	}
	var load []int
	for _, s := range seasons {
		m, ok := have[s.Season]
		r := records[s.Season]
		switch {
		case m.episodes < r.episodes:
		case r.detailed && ctx.Value(deferAiredCountsKey{}) == true:
			continue
		case !ok || !r.detailed:
		case r.fresh:
			continue
		case s.Season != latest && m.pending == 0 && r.episodes <= m.episodes:
			continue
		}
		load = append(load, s.Season)
	}
	if len(load) == 0 {
		return nil
	}
	// Each fetch loads the show; make sure it is mirrored once, first.
	if _, err := c.ensureShow(ctx, showID); err != nil {
		return err
	}
	errs := make([]error, len(load))
	sem := make(chan struct{}, seasonLoads)
	var wg sync.WaitGroup
	for i, season := range load {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			errs[i] = ctx.Err()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			_, errs[i] = c.fetchSeason(ctx, showID, season)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// NextStoredEpisode is the next regular episode already in the catalogue.
// It does not refresh the show or season from a provider. Home, library
// rows, and model builds use it so one stale show cannot stall the request.
// A special still jumps to the first regular episode, matching NextEpisode.
func (c *Catalog) NextStoredEpisode(ctx context.Context, episode core.RatingKey) (*core.Title, error) {
	next, err := c.NextStoredEpisodes(ctx, []core.RatingKey{episode})
	if err != nil {
		return nil, err
	}
	if t := next[episode]; t != nil {
		return t, nil
	}
	return nil, core.ErrNotFound
}

// NextStoredEpisodes is NextStoredEpisode for several episodes; episodes with
// no stored next episode are absent from the result. Each show's window is
// read on its own; the episodes returned are hydrated together. Continue
// Watching asks this for each of up to fifty shows, and hydrating per show
// (or, before, every episode of each window) read titles' facts and their
// shows' seasons fifty times per Home request.
func (c *Catalog) NextStoredEpisodes(ctx context.Context, episodes []core.RatingKey) (map[core.RatingKey]*core.Title, error) {
	return c.nextStoredEpisodes(ctx, episodes, nil)
}

// NextUnwatchedStoredEpisodes skips completed successors with indexed state
// probes, before selecting each show's bounded window. Home must not walk a
// long watched season one episode (and one status query) at a time.
func (c *Catalog) NextUnwatchedStoredEpisodes(ctx context.Context, uid int64, episodes []core.RatingKey) (map[core.RatingKey]*core.Title, error) {
	return c.nextStoredEpisodes(ctx, episodes, &uid)
}

func (c *Catalog) nextStoredEpisodes(ctx context.Context, episodes []core.RatingKey, uid *int64) (map[core.RatingKey]*core.Title, error) {
	out := map[core.RatingKey]*core.Title{}
	if c == nil || c.db == nil {
		return out, nil
	}
	type window struct {
		from      core.RatingKey
		query     string
		args      []any
		candidate core.RatingKey
	}
	now := c.now()
	// Keep the original eight-successor window. Return one eligible key per
	// show and hydrate those keys together. Only a rejected candidate needs
	// another indexed read of its window.
	var order []*window
	for _, episode := range episodes {
		if episode.Kind() != core.KindEpisode {
			continue
		}
		order = append(order, &window{from: episode})
	}
	const showsPerStatement = 50
	for start := 0; start < len(order); start += showsPerStatement {
		part := order[start:min(start+showsPerStatement, len(order))]
		var byKey map[core.RatingKey]core.Title
		var choices map[core.RatingKey]showOrder
		if c.canProjectAlternateOrders() {
			keys := make([]core.RatingKey, len(part))
			for i, w := range part {
				keys[i] = w.from
			}
			filter, bind := c.keyMembership("key", false, keys)
			anchors, err := c.scanTitleKeys(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE `+filter, bind...)
			if err != nil {
				return nil, err
			}
			showKeys := make([]core.RatingKey, 0, len(anchors))
			byKey = make(map[core.RatingKey]core.Title, len(anchors))
			for _, t := range anchors {
				showKeys = append(showKeys, t.ShowKey)
				byKey[t.Key] = t
			}
			choices, err = c.readShowOrders(ctx, showKeys)
			if err != nil {
				return nil, err
			}
		}

		for _, w := range part {
			anchor := byKey[w.from]
			choice, known := choices[anchor.ShowKey]
			if known && providerLayout(&choice.title, choice.order) {
				cond := ` WHERE e.show_key=? AND e.provider=? AND e.order_kind=? AND e.season>0 AND e.air_date<>'' AND e.air_date<=? AND t.adult=0
 AND (a.episode_key IS NOT NULL AND (e.season>a.season OR (e.season=a.season AND e.episode>a.episode))
 OR (a.episode_key IS NULL AND (?=0 OR e.air_date>?)))`
				bind := []any{w.from, anchor.ShowKey, choice.order.Source, choice.order.Kind, now.UTC().Format(time.DateOnly), anchor.Season, anchor.Released.UTC().Format(time.DateOnly)}
				if uid != nil {
					cond += ` AND NOT EXISTS(SELECT 1 FROM state_watch w WHERE w.user_id=? AND w.rating_key=t.key AND w.view_count>0 AND w.view_offset_ns<=0)`
					bind = append(bind, *uid)
				}
				w.query = `FROM catalog_order_episodes e JOIN catalog_titles t ON t.key=e.episode_key
 LEFT JOIN catalog_order_episodes a ON a.show_key=e.show_key AND a.provider=e.provider AND a.order_kind=e.order_kind AND a.episode_key=?` +
					cond + ` ORDER BY e.season,e.episode LIMIT 1 OFFSET ?`
				w.args = bind
				continue
			}
			show := anchor.ShowKey
			season, episode := anchor.Season, anchor.Episode
			if show == 0 && !w.from.Allocated() {
				show, season, episode = core.ShowKey(w.from.TMDB()), w.from.Season(), w.from.Episode()
			}
			if show == 0 {
				continue
			}
			cond := `season>0 AND key<5000000000000000 AND (season,episode)>(?,?)`
			wargs := []any{show, season, episode}
			if season <= 0 {
				cond, wargs = `season>0 AND key<5000000000000000`, []any{show}
			}
			if uid != nil {
				cond += ` AND NOT EXISTS(SELECT 1 FROM state_watch watched WHERE watched.user_id=? AND watched.rating_key=catalog_titles.key AND watched.view_count>0 AND watched.view_offset_ns<=0 LIMIT 1 OFFSET 0)`
				wargs = append(wargs, *uid)
			}
			w.query = `FROM (SELECT key,season,episode,adult,released_ts FROM catalog_titles` +
				c.indexedBy("catalog_titles_show") + ` WHERE type='episode' AND show_key=? AND ` + cond +
				` ORDER BY season,episode LIMIT 8) AS t
 WHERE t.adult=0 AND t.released_ts<>0 AND t.released_ts<=? ORDER BY t.season,t.episode LIMIT 1 OFFSET ?`
			w.args = append(wargs, now.Unix())
		}
	}
	open := order[:0]
	for _, w := range order {
		if w.query != "" {
			open = append(open, w)
		}
	}
	for offset := 0; len(open) > 0 && offset < 8; offset++ {
		keys := make([]any, 0, len(open))
		for start := 0; start < len(open); start += showsPerStatement {
			part := open[start:min(start+showsPerStatement, len(open))]
			branches := make([]string, 0, len(part))
			var args []any
			for i, w := range part {
				w.candidate = 0
				branches = append(branches, `SELECT * FROM (SELECT t.key,`+strconv.Itoa(i)+` AS w `+w.query+`) AS candidate`)
				args = append(args, w.args...)
				args = append(args, offset)
			}
			rows, err := c.db.QueryContext(ctx, strings.Join(branches, ` UNION ALL `), args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var key core.RatingKey
				var i int
				if err := rows.Scan(&key, &i); err != nil {
					rows.Close()
					return nil, err
				}
				part[i].candidate = key
				keys = append(keys, int64(key))
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
		}
		got := map[core.RatingKey]core.Title{}
		const batch = 200
		for chunk := range store.Chunks(keys, batch) {
			part := chunk
			rows, err := c.queryPagedTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE key IN (`+placeholders(len(part))+`)`, part...)
			if err != nil {
				return nil, err
			}
			for _, t := range rows {
				got[t.Key] = t
			}
		}
		retry := open[:0]
		for _, w := range open {
			if w.candidate == 0 {
				continue
			}
			if t, ok := got[w.candidate]; ok && c.visibleTitle(&t) {
				t := t
				out[w.from] = &t
				continue
			}
			retry = append(retry, w)
		}
		open = retry
	}
	return out, nil
}

// ContinueEpisodes returns the next episode in the stored effective order.
func (c *Catalog) ContinueEpisodes(ctx context.Context, episodes []core.RatingKey) (map[core.RatingKey]*core.Title, error) {
	return c.NextStoredEpisodes(ctx, episodes)
}

// scanTitleKeys reads title rows without hydrating them.
func (c *Catalog) scanTitleKeys(ctx context.Context, q string, args ...any) ([]core.Title, error) {
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Title
	for rows.Next() {
		st, err := scanTitle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st.t)
	}
	return out, rows.Err()
}

// NextEpisode returns the next regular episode (specials, season 0, are skipped
// as a target; calling it on a special jumps to S1E1-order sequence).
func (c *Catalog) NextEpisode(ctx context.Context, episode core.RatingKey) (*core.Title, error) {
	if episode.Kind() != core.KindEpisode {
		return nil, core.ErrNotFound
	}
	anchor, parent, order, err := c.orderForParent(ctx, episode)
	if err != nil {
		return nil, err
	}
	if anchor != nil && parent != nil && providerLayout(parent, order) {
		if err := c.projectSingle(ctx, anchor); err != nil {
			return nil, err
		}
		seasons, err := c.ensureOrderManifest(ctx, parent, order)
		if err != nil {
			return nil, err
		}
		if current, ok := orderedSeason(seasons, anchor.Season); ok {
			if err := c.ensureOrderSeason(ctx, parent, order, current); err != nil {
				return nil, err
			}
		}
		if next, err := c.NextStoredEpisode(ctx, episode); err == nil || !errors.Is(err, core.ErrNotFound) {
			return next, err
		}
		for _, season := range seasons {
			if season.Season <= anchor.Season || season.Season <= 0 {
				continue
			}
			date := metadataDate(season.AirDate)
			if !date.IsZero() && date.After(c.now()) {
				break
			}
			if err := c.ensureOrderSeason(ctx, parent, order, season); err != nil {
				return nil, err
			}
			break
		}
		return c.NextStoredEpisode(ctx, episode)
	}
	if episode.Allocated() {
		return c.NextStoredEpisode(ctx, episode)
	}

	showID, curSeason, curEp := episode.TMDB(), episode.Season(), episode.Episode()
	show, err := c.ensureShow(ctx, showID)
	if err != nil {
		return nil, err
	}
	// Same season, higher episode number (only for regular seasons).
	if curSeason > 0 {
		if _, err := c.fetchSeason(ctx, showID, curSeason); err == nil {
			eps, err := c.episodesOfSeason(ctx, showID, curSeason)
			if err != nil {
				return nil, err
			}
			for _, e := range eps {
				if e.Episode > curEp {
					ep := e
					return &ep, nil
				}
			}
			// Caught up to the end of the mirrored episodes on a Returning
			// Series: if the season has not been re-checked in the last 15
			// minutes, poll TMDB in case tonight's episode was just added.
			if show != nil && strings.EqualFold(show.Status, "Returning Series") {
				sKey := core.SeasonKey(showID, curSeason)
				if st, lerr := c.loadByKey(ctx, sKey); lerr == nil && c.now().Sub(st.detailAt) >= min(c.staleAfter, caughtUpRefreshTTL) {
					if _, ferr := c.fetchSeasonOpts(ctx, showID, curSeason, true, false); ferr == nil {
						if eps2, eerr := c.episodesOfSeason(ctx, showID, curSeason); eerr == nil {
							for _, e := range eps2 {
								if e.Episode > curEp {
									ep := e
									return &ep, nil
								}
							}
						}
					}
				}
			}
		}
	}
	seasons, err := c.seasonsOfShow(ctx, showID)
	if err != nil {
		return nil, err
	}
	for _, s := range seasons {
		if s.Season <= curSeason || s.Season == 0 {
			continue
		}
		if _, err := c.fetchSeason(ctx, showID, s.Season); err != nil {
			if errors.Is(err, core.ErrNotFound) {
				continue
			}
			return nil, err
		}
		eps, err := c.episodesOfSeason(ctx, showID, s.Season)
		if err != nil {
			return nil, err
		}
		for _, e := range eps {
			if e.Episode > 0 {
				ep := e
				return &ep, nil
			}
		}
	}
	return nil, core.ErrNotFound
}

// RefreshTitle updates canonical details from TMDB without a stale fallback.
func (c *Catalog) RefreshTitle(ctx context.Context, key core.RatingKey, persist bool) (*core.Title, error) {
	return c.refreshTitle(ctx, key, persist, false)
}

// RefreshTitleTree refreshes a single canonical title and its episode metadata.
func (c *Catalog) RefreshTitleTree(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	return c.refreshTitle(ctx, key, true, true)
}

func (c *Catalog) refreshTitle(ctx context.Context, key core.RatingKey, persist, tree bool) (*core.Title, error) {
	id := key.TMDB()
	if tree {
		t, err := c.storedTitle(ctx, key)
		if err != nil {
			return nil, err
		}
		source, err := c.titleDetailsSource(ctx, t)
		if err != nil {
			return nil, err
		}
		alternate := false
		if t.Type == core.TypeShow {
			order, err := c.effectiveEpisodeOrder(ctx, t)
			if err != nil {
				return nil, err
			}
			alternate = providerLayout(t, order.Order)
		}
		if t.Key.Allocated() || alternate || source != "" && source != core.MetadataTMDB {
			return c.refreshSourceTree(ctx, t, source)
		}
		key = t.Key
		if t.IDs.TMDB > 0 {
			id = t.IDs.TMDB
		}
	}

	client := c.tmdb()
	if client == nil || !c.TMDBConfigured() {
		return nil, core.ErrUnsupported
	}
	var t core.Title
	switch key.Kind() {
	case core.KindMovie:
		var d *tmdb.MovieDetails
		var err error
		if fresh, ok := client.(interface {
			MovieDetailsFresh(context.Context, int64) (*tmdb.MovieDetails, error)
		}); ok {
			d, err = fresh.MovieDetailsFresh(ctx, id)
		} else {
			return nil, fmt.Errorf("catalog: fresh movie details unavailable")
		}
		if err != nil {
			return nil, err
		}
		if d == nil || d.ID != id {
			return nil, fmt.Errorf("catalog: invalid movie details for %s", key)
		}
		t = c.movieDetailsToTitle(ctx, d)
		if persist {
			if err := c.upsert(ctx, t, upsertOpts{browsable: c.meetsThreshold(t), detail: true, releaseDates: d.ReleaseDates, replaceMetadata: tree}); err != nil {
				return nil, err
			}
		}
	case core.KindShow:
		var d *tmdb.TVDetails
		var err error
		if fresh, ok := client.(interface {
			TVDetailsFresh(context.Context, int64) (*tmdb.TVDetails, error)
		}); ok {
			d, err = fresh.TVDetailsFresh(ctx, id)
		} else {
			return nil, fmt.Errorf("catalog: fresh show details unavailable")
		}
		if err != nil {
			return nil, err
		}
		if d == nil || d.ID != id {
			return nil, fmt.Errorf("catalog: invalid show details for %s", key)
		}
		t = c.tvDetailsToTitle(ctx, d)
		if persist {
			if tree {
				if err := c.refreshShowTree(ctx, client, t, d); err != nil {
					return nil, err
				}
			} else if err := c.storeShowDetails(ctx, t, d); err != nil {
				return nil, err
			}
		}
	default:
		return nil, core.ErrNotFound
	}
	if tree {
		if key.Kind() == core.KindMovie {
			c.clearRefreshMiss(movieRefreshKey(id))
		} else {
			c.clearRefreshMiss(showRefreshKey(id))
		}
	}
	if !persist {
		return &t, nil
	}
	st, err := c.loadByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	return &st.t, nil
}

var _ core.AuthoritativeTitleRefresher = (*Catalog)(nil)

// List titles serve loaded summaries immediately while details hydrate behind
// the request. Other direct lookups retain their existing bounded fetch budget.
func (c *Catalog) listReadWait(details, fromList bool) time.Duration {
	if fromList {
		return 0
	}
	return c.readWait(details)
}
