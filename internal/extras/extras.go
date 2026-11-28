// Package extras lists and resolves trailers and other clips attached to
// catalogue titles (Plex "Extras" and Cinema Trailers).
//
// TMDB only gives YouTube/Vimeo keys, which official Plex apps cannot
// play. Server playback of these videos uses a yt-dlp-compatible resolver
// command. Clients with YouTube embeds can opt in to listing YouTube trailers
// and teasers for playback on the device.
package extras

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// SourceCommand marks an Extra whose TMDB video is resolved by the owner's command.
const SourceCommand = "command"

// SourcePlexDiscover marks an Extra whose direct MP4 comes from Plex Discover.
const SourcePlexDiscover = "plex_discover"

// SourceClientYouTube marks a YouTube clip offered for client playback only.
const SourceClientYouTube = "client_youtube"

// MaxExtras caps how many extras one title advertises.
const MaxExtras = 10

// Extra is one clip attached to a title.
type Extra struct {
	Key         core.RatingKey // KindExtra
	ParentKey   core.RatingKey
	ParentTitle string
	Title       string
	Subtype     string // Plex subtype: trailer, teaser, clip, featurette, behindTheScenes
	ExtraType   int    // Plex numeric extra type (1 = trailer)
	Site        string // YouTube, Vimeo (command source)
	SiteKey     string // site-specific video id
	DiscoverKey string // <parentHex>/<extraID> (Plex Discover source)
	Language    string // ISO 639-1
	Official    bool
	PublishedAt time.Time
	Size        int // vertical resolution when known
	Duration    time.Duration
	Source      string // SourcePlexDiscover, SourceCommand or SourceClientYouTube

	identity string // registry identity (collision detection)
}

// IsTrailer reports whether the extra can serve as a Cinema Trailer.
func (e Extra) IsTrailer() bool { return e.Subtype == "trailer" || e.Subtype == "teaser" }

// Resolved is a playable location for an extra.
type Resolved struct {
	URL       string // direct media URL (video when AudioURL is set)
	AudioURL  string // optional separate audio input
	Headers   map[string]string
	Expires   time.Time
	Container string
}

// Input returns the ffmpeg input.
func (r Resolved) Input() string { return r.URL }

// VideoProvider is the TMDB videos surface the service needs
// (*tmdb.Client satisfies it).
type VideoProvider interface {
	MovieVideos(ctx context.Context, id int64, lang string) ([]tmdb.Video, error)
	TVVideos(ctx context.Context, id int64, lang string) ([]tmdb.Video, error)
	SeasonVideos(ctx context.Context, tvID int64, season int, lang string) ([]tmdb.Video, error)
	EpisodeVideos(ctx context.Context, tvID int64, season, episode int, lang string) ([]tmdb.Video, error)
}

// CommandRunner runs the resolver command and returns its stdout. Tests
// substitute a fake; the default is exec without a shell.
type CommandRunner func(ctx context.Context, name string, args []string) ([]byte, error)

// Options are the extras service dependencies.
type Options struct {
	DB            *sql.DB // optional durable list/item metadata cache
	ThemeFFmpeg   func() string
	CacheDir      string
	CacheHTTP     *http.Client // optional injected transport for trailer downloads
	Videos        VideoProvider
	Discover      DiscoverProvider
	Config        func() config.Extras
	Logger        *slog.Logger
	Now           func() time.Time
	CacheTTL      time.Duration // list cache; default 7d
	EmptyCacheTTL time.Duration // verified-empty list cache; default 24h (capped at CacheTTL)
	ColdWait      time.Duration // bounded wait on single-item ColdDetail miss; default 900ms (<0 disables)
	Runner        CommandRunner
	LookPath      func(string) (string, error)
	ToolsDir      string
	Downloader    *Downloader
}

type listEntry struct {
	extras  []Extra
	expires time.Time
}

type listFlight struct {
	done   chan struct{}
	extras []Extra
}

// Service lists, registers and resolves extras. Safe for concurrent use.
type Service struct {
	db               *sql.DB
	themeCtx         context.Context
	themeCancel      context.CancelFunc
	themeFFmpeg      func() string
	themeWG          sync.WaitGroup
	themeMu          sync.Mutex
	themeEvictMu     sync.Mutex
	themeWait        time.Duration
	themeFlights     map[core.RatingKey]*themeFlight
	themeRetry       map[core.RatingKey]time.Time
	themeBotUntil    time.Time
	themeBotStep     time.Duration
	cacheDir         string
	cacheHTTP        *http.Client
	cacheMu          sync.Mutex
	cacheDownloadMu  sync.Mutex
	cacheFlightsMu   sync.Mutex
	cacheFlights     map[string]*trailerFill
	downloadTrailers bool
	videos           VideoProvider
	discover         DiscoverProvider
	cfgFn            func() config.Extras
	log              *slog.Logger
	now              func() time.Time
	ttl              time.Duration
	emptyTTL         time.Duration
	coldWait         time.Duration
	runner           CommandRunner
	look             func(string) (string, error)
	downloader       *Downloader

	listWG      sync.WaitGroup
	listClosed  bool
	listFlights map[string]*listFlight
	mu          sync.Mutex
	registry    map[core.RatingKey]Extra
	lists       map[string]listEntry
	resolved    map[string]Resolved
}

const (
	defaultListTTL      = 7 * 24 * time.Hour
	defaultEmptyListTTL = 24 * time.Hour
	defaultColdWait     = 900 * time.Millisecond
	listFetchTimeout    = 5 * time.Second
	maxBgListFlights    = 2
	maxColdListFlights  = 8

	maxListEntries     = 2048
	maxRegistryEntries = 8192
	maxResolvedEntries = 2048
)

// New builds the extras service.
func New(o Options) *Service {
	if o.Config == nil {
		o.Config = func() config.Extras { return config.Extras{} }
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.CacheTTL <= 0 {
		o.CacheTTL = defaultListTTL
	}
	emptyTTL := o.EmptyCacheTTL
	if emptyTTL <= 0 {
		emptyTTL = min(defaultEmptyListTTL, o.CacheTTL)
	}
	coldWait := o.ColdWait
	if coldWait == 0 {
		coldWait = defaultColdWait
	} else if coldWait < 0 {
		coldWait = 0
	}
	downloadTrailers := o.Runner == nil
	if o.Runner == nil {
		o.Runner = execRunner
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	dl := o.Downloader
	if dl == nil && o.ToolsDir != "" {
		dl = NewDownloader(DownloaderOptions{
			ToolsDir: o.ToolsDir,
			Config:   o.Config,
			LookPath: o.LookPath,
			Now:      o.Now,
		})
	}
	themeCtx, themeCancel := context.WithCancel(context.Background())
	return &Service{
		db: o.DB, themeCtx: themeCtx, themeCancel: themeCancel, downloadTrailers: downloadTrailers,
		themeFFmpeg: o.ThemeFFmpeg, themeWait: themeFirstOpenWait, cacheDir: o.CacheDir, cacheHTTP: o.CacheHTTP,
		videos: o.Videos, discover: o.Discover, cfgFn: o.Config,
		log: o.Logger.With("component", "extras"), now: o.Now,
		ttl: o.CacheTTL, emptyTTL: emptyTTL, coldWait: coldWait,
		runner: o.Runner, look: o.LookPath,
		downloader: dl,
		registry:   map[core.RatingKey]Extra{}, lists: map[string]listEntry{},
		resolved: map[string]Resolved{},
	}
}

// Downloader returns the managed yt-dlp downloader shared with this service.
func (s *Service) Downloader() *Downloader { return s.downloader }

func (s *Service) cfg() config.Extras { return s.cfgFn() }

// DefaultResolverCommand is used when the owner enabled trailers but set
// no resolver command. It is resolved on PATH at use time and never
// written into the saved config.
const DefaultResolverCommand = "yt-dlp"

// effectiveCommand returns the command that would run for this config:
// the configured one, the verified managed yt-dlp binary in Ether's data dir,
// or the yt-dlp default when none is set. The second result reports whether
// it was found and verified.
func (s *Service) effectiveCommand(c config.Extras) (string, bool) {
	cmd := strings.TrimSpace(c.ResolverCommand)
	if cmd == "" {
		if s.downloader != nil {
			if path, _, ok := s.downloader.VerifiedManagedBinary(); ok {
				return path, true
			}
		}
		cmd = DefaultResolverCommand
	}
	_, err := s.look(cmd)
	return cmd, err == nil
}

// DiscoverAvailable reports whether the Plex Discover extras source can run.
func (s *Service) DiscoverAvailable() bool {
	return s.discover != nil && s.discover.Available()
}

// CommandAvailable reports whether the resolver command source can run.
func (s *Service) CommandAvailable() bool {
	_, ok := s.effectiveCommand(s.cfg())
	return ok
}

// Enabled reports whether any playable extras can be advertised.
func (s *Service) Enabled() bool {
	return s.cfg().Enabled && (s.DiscoverAvailable() || s.videoSourceAvailable() && s.CommandAvailable())
}

// ClientYouTubeAvailable reports whether client-playable YouTube trailers can be listed.
func (s *Service) ClientYouTubeAvailable() bool {
	return s.cfg().Enabled && s.videoSourceAvailable()
}

func (s *Service) videoSourceAvailable() bool {
	if keyed, ok := s.videos.(interface{ HasKey() bool }); ok {
		return keyed.HasKey()
	}
	return s.videos != nil
}

// DisabledReason explains why extras are off, for a startup log line.
// Empty when enabled.
func (s *Service) DisabledReason() string {
	c := s.cfg()
	switch {
	case !c.Enabled:
		return "extras disabled in config (extras.enabled is false)"
	case s.Enabled():
		return ""
	case s.CommandAvailable():
		return "extras enabled but no trailer metadata source is configured"
	case strings.TrimSpace(c.ResolverCommand) == "":
		return "extras enabled but trailers need yt-dlp installed (or set extras.resolverCommand to a yt-dlp-compatible tool)"
	default:
		return fmt.Sprintf("extras resolver command %q not found or not executable", c.ResolverCommand)
	}
}

// PrewarmBytes is the configured trailer pre-buffer size (32–64 MiB).
func (s *Service) PrewarmBytes() int64 { return s.cfg().PrewarmBytesOr() }

// ISO639 reduces a BCP-47 tag to its ISO 639-1 language.
func ISO639(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexAny(lang, "-_"); i > 0 {
		lang = lang[:i]
	}
	return lang
}

// ListOption controls which playback sources a caller can use.
type ListOption uint8

// IncludeClientYouTube includes YouTube trailers and teasers when no resolver is available.
const IncludeClientYouTube ListOption = 1

// List returns a title's extras, best first. By default it lists server-playable
// sources. Extras must be enabled even for callers that opt into client playback.
func (s *Service) List(ctx context.Context, parent *core.Title, lang string, opts ...ListOption) ([]Extra, error) {
	if parent == nil || !s.cfg().Enabled {
		return nil, nil
	}
	iso := ISO639(lang)
	discoverOn := s.DiscoverAvailable()
	videosOn := trailerTMDBID(parent) > 0 && s.videoSourceAvailable()
	cmdOn := videosOn && s.CommandAvailable()
	includeClientYouTube := slices.Contains(opts, IncludeClientYouTube)
	clientOnly := videosOn && !cmdOn && includeClientYouTube
	if !discoverOn && !cmdOn && !clientOnly {
		return nil, nil
	}
	cacheKey := fmt.Sprintf("%s|%s|%t|%t", parent.Key.String(), iso, discoverOn, cmdOn)
	if includeClientYouTube {
		cacheKey += fmt.Sprintf("|client-youtube=%t", clientOnly)
	}

	s.mu.Lock()
	if e, ok := s.lists[cacheKey]; ok && s.now().Before(e.expires) {
		out := s.cloneAndRegisterLocked(e.extras)
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()

	if items, expires, ok := s.loadListDB(ctx, cacheKey); ok {
		s.mu.Lock()
		s.pruneLocked()
		out := s.cloneAndRegisterLocked(items)
		s.putListLocked(cacheKey, out, expires)
		s.mu.Unlock()
		return out, nil
	}

	if core.LocalResponse(ctx) {
		cold := core.ColdDetail(ctx, parent.Key)
		limit := maxBgListFlights
		if cold {
			limit = maxColdListFlights
		}
		s.mu.Lock()
		if e, ok := s.lists[cacheKey]; ok && s.now().Before(e.expires) {
			out := s.cloneAndRegisterLocked(e.extras)
			s.mu.Unlock()
			return out, nil
		}
		flight := s.listFlights[cacheKey]
		if flight == nil && !s.listClosed && len(s.listFlights) < limit {
			if s.listFlights == nil {
				s.listFlights = map[string]*listFlight{}
			}
			flight = &listFlight{done: make(chan struct{})}
			s.listFlights[cacheKey] = flight
			s.listWG.Add(1)
			// Video lookup reads only this immutable identity snapshot.
			owned := *parent
			go func() {
				defer s.listWG.Done()
				bg, cancel := context.WithTimeout(s.themeCtx, listFetchTimeout)
				defer cancel()
				out := s.fetchAndStoreList(bg, &owned, lang, iso, discoverOn, cmdOn, clientOnly, cacheKey)
				flight.extras = out
				s.mu.Lock()
				delete(s.listFlights, cacheKey)
				s.mu.Unlock()
				close(flight.done)
			}()
		}
		s.mu.Unlock()
		if flight == nil {
			return nil, nil
		}
		if !cold || s.coldWait <= 0 {
			core.ReportDetailPending(ctx, parent.Key, "extras")
			return nil, nil
		}
		timer := time.NewTimer(s.coldWait)
		defer timer.Stop()
		select {
		case <-flight.done:
			s.mu.Lock()
			out := s.cloneAndRegisterLocked(flight.extras)
			s.mu.Unlock()
			return out, nil
		case <-timer.C:
			core.ReportDetailPending(ctx, parent.Key, "extras")
			return nil, nil
		case <-ctx.Done():
			return nil, nil
		}
	}

	return s.fetchAndStoreList(ctx, parent, lang, iso, discoverOn, cmdOn, clientOnly, cacheKey), nil
}

func (s *Service) fetchAndStoreList(ctx context.Context, parent *core.Title, lang, iso string, discoverOn, cmdOn, clientOnly bool, cacheKey string) []Extra {
	var out []Extra
	sourcesTried := 0
	sourcesFailed := 0
	if discoverOn {
		sourcesTried++
		ds, err := s.discover.List(ctx, parent)
		if err != nil {
			sourcesFailed++
			s.log.Debug("plex discover extras", "key", parent.Key, "err", err)
		}
		out = append(out, ds...)
	}
	if cmdOn || clientOnly {
		sourcesTried++
		vs, err := s.fetchVideos(ctx, parent, lang)
		if err != nil {
			sourcesFailed++
			s.log.Debug("tmdb videos", "key", parent.Key, "err", err)
		}
		out = append(out, videosToExtras(parent, vs, clientOnly)...)
	}
	out = dedupeExtras(out)
	sortExtras(out, iso)
	if len(out) > MaxExtras {
		out = out[:MaxExtras]
	}
	var (
		save      bool
		saveNow   time.Time
		saveUntil time.Time
		toSave    []Extra
	)
	s.mu.Lock()
	s.pruneLocked()
	for i := range out {
		out[i] = s.registerLocked(out[i])
	}
	// Do not cache an empty result when an active source failed, or a
	// transient error would hide trailers for the whole TTL.
	if sourcesFailed == 0 || (len(out) > 0 && sourcesFailed < sourcesTried) {
		saveNow = s.now()
		ttl := s.ttl
		if len(out) == 0 && s.emptyTTL > 0 && s.emptyTTL < ttl {
			ttl = s.emptyTTL
		}
		saveUntil = saveNow.Add(ttl)
		s.putListLocked(cacheKey, out, saveUntil)
		toSave = append([]Extra(nil), out...)
		save = true
	}
	s.mu.Unlock()
	if save {
		s.saveListDB(ctx, cacheKey, parent.Key, toSave, saveNow, saveUntil)
	}
	return out
}

func (s *Service) cloneAndRegisterLocked(in []Extra) []Extra {
	out := append([]Extra(nil), in...)
	for i := range out {
		out[i] = s.registerLocked(out[i])
	}
	return out
}

func (s *Service) putListLocked(cacheKey string, items []Extra, expires time.Time) {
	if len(s.lists) >= maxListEntries {
		for k := range s.lists {
			delete(s.lists, k)
			break
		}
	}
	s.lists[cacheKey] = listEntry{extras: append([]Extra(nil), items...), expires: expires}
}

func trailerTMDBID(parent *core.Title) int64 {
	if parent.Type == core.TypeEpisode || parent.Type == core.TypeSeason {
		if parent.EpisodeOrder.Source != "" && (parent.EpisodeOrder.Source != core.MetadataTMDB || parent.EpisodeOrder.Kind != core.OrderAired) ||
			parent.Key.Allocated() || parent.Key.Season() != parent.Season || parent.Key.Episode() != parent.Episode {
			return 0
		}
		return parent.ShowIDs.TMDB
	}
	return parent.IDs.TMDB
}

func (s *Service) fetchVideos(ctx context.Context, parent *core.Title, lang string) ([]tmdb.Video, error) {
	tmdbID := trailerTMDBID(parent)
	if tmdbID <= 0 {
		return nil, nil
	}
	switch parent.Type {
	case core.TypeMovie:
		return s.videos.MovieVideos(ctx, tmdbID, lang)
	case core.TypeShow:
		return s.videos.TVVideos(ctx, tmdbID, lang)
	case core.TypeSeason:
		return s.videos.SeasonVideos(ctx, tmdbID, parent.Season, lang)
	case core.TypeEpisode:
		return s.videos.EpisodeVideos(ctx, tmdbID, parent.Season, parent.Episode, lang)
	}
	return nil, nil
}

// Lookup returns a registered extra by rating key, falling back to the
// durable extras item store so keys remain playable across restarts.
func (s *Service) Lookup(key core.RatingKey) (Extra, bool) {
	s.mu.Lock()
	if e, ok := s.registry[key]; ok {
		s.mu.Unlock()
		return e, true
	}
	s.mu.Unlock()
	if e, ok := s.lookupItemDB(key); ok {
		s.mu.Lock()
		if existing, found := s.registry[key]; found {
			s.mu.Unlock()
			return existing, true
		}
		if len(s.registry) >= maxRegistryEntries {
			for k := range s.registry {
				delete(s.registry, k)
				break
			}
		}
		s.registry[key] = e
		s.mu.Unlock()
		return e, true
	}
	return Extra{}, false
}

// registerLocked assigns the deterministic KindExtra key for an extra.
// The payload is a hash of the extra's identity, so the same extra keeps
// the same key across listings and restarts; hash collisions salt.
func (s *Service) registerLocked(e Extra) Extra {
	for salt := 0; ; salt++ {
		id := e.identity
		if salt > 0 {
			id = fmt.Sprintf("%s#%d", id, salt)
		}
		h := fnv.New64a()
		_, _ = h.Write([]byte(id))
		key := core.ExtraKey(int64(h.Sum64()%999_999_999_999_999) + 1)
		if key.IsPreroll() {
			continue // reserved for owner pre-rolls
		}
		if prev, ok := s.registry[key]; ok && prev.identity != e.identity {
			continue
		}
		e.Key = key
		// Client-only clips have a stable wire ID but no server playback record.
		if e.Source == SourceClientYouTube {
			return e
		}
		if len(s.registry) >= maxRegistryEntries {
			for k := range s.registry {
				delete(s.registry, k)
				break
			}
		}
		s.registry[key] = e
		return e
	}
}

// pruneLocked removes expired cache records. The hard caps below also bound
// entries whose keys are never revisited and therefore never expire lazily.
func (s *Service) pruneLocked() {
	now := s.now()
	for k, v := range s.lists {
		if !now.Before(v.expires) {
			delete(s.lists, k)
		}
	}
	for k, v := range s.resolved {
		if !v.Expires.IsZero() && !now.Before(v.Expires) {
			delete(s.resolved, k)
		}
	}
}

// Resolve turns an extra into a playable input. Command results are cached
// until the returned URL expires; Plex Discover signed URLs are resolved fresh
// per play.
func (s *Service) Resolve(ctx context.Context, e Extra) (Resolved, error) {
	switch e.Source {
	case SourcePlexDiscover:
		return s.resolveDiscover(ctx, e)
	case SourceCommand:
		return s.resolveCommand(ctx, e)
	default:
		return Resolved{}, fmt.Errorf("extras: unknown source %q", e.Source)
	}
}

func (s *Service) resolveDiscover(ctx context.Context, e Extra) (Resolved, error) {
	if !s.cfg().Enabled {
		return Resolved{}, fmt.Errorf("extras disabled")
	}
	if !s.DiscoverAvailable() {
		return Resolved{}, fmt.Errorf("extras: plex discover unavailable")
	}
	return s.discover.Resolve(ctx, e)
}

func (s *Service) resolveCommand(ctx context.Context, e Extra) (Resolved, error) {
	cfg := s.cfg()
	if !cfg.Enabled {
		return Resolved{}, fmt.Errorf("extras disabled")
	}
	cmd, ok := s.effectiveCommand(cfg)
	if !ok {
		return Resolved{}, fmt.Errorf("extras: resolver command unavailable")
	}
	cfg.ResolverCommand = cmd
	// A new command or argument template may choose a different format or
	// credentials. Do not reuse a URL produced by the previous settings.
	identity, _ := json.Marshal(struct {
		Command string
		Args    []string
	}{cfg.ResolverCommand, cfg.ResolverArgs})
	cacheKey := string(identity) + ":" + strings.ToLower(e.Site) + ":" + e.SiteKey
	s.mu.Lock()
	if r, ok := s.resolved[cacheKey]; ok && (r.Expires.IsZero() || s.now().Before(r.Expires)) {
		s.mu.Unlock()
		return r, nil
	}
	s.mu.Unlock()
	r, err := runResolver(ctx, cfg, e, s.runner, s.now)
	if err != nil {
		return Resolved{}, err
	}
	s.mu.Lock()
	s.pruneLocked()
	s.resolved[cacheKey] = r
	if len(s.resolved) > maxResolvedEntries {
		for k := range s.resolved {
			if k != cacheKey {
				delete(s.resolved, k)
				break
			}
		}
	}
	s.mu.Unlock()
	return r, nil
}

// subtypeFor maps a TMDB video type to the Plex subtype and numeric
// extra type. Unknown types are treated as clips.
func subtypeFor(tmdbType string) (string, int) {
	switch tmdbType {
	case tmdb.VideoTrailer:
		return "trailer", 1
	case tmdb.VideoTeaser:
		return "teaser", 1
	case tmdb.VideoBehindTheScenes, tmdb.VideoBloopers:
		return "behindTheScenes", 3
	case tmdb.VideoFeaturette:
		return "featurette", 6
	default:
		return "clip", 4
	}
}

func videosToExtras(parent *core.Title, vs []tmdb.Video, clientOnly bool) []Extra {
	var out []Extra
	for _, v := range vs {
		if !ValidateSiteKey(v.Site, v.Key) {
			continue
		}
		sub, num := subtypeFor(v.Type)
		source := SourceCommand
		site := v.Site
		if clientOnly {
			if !strings.EqualFold(strings.TrimSpace(site), "youtube") || (sub != "trailer" && sub != "teaser") {
				continue
			}
			source = SourceClientYouTube
			site = "YouTube"
		}
		title := strings.TrimSpace(v.Name)
		if title == "" {
			title = parent.Title + " " + v.Type
		}
		out = append(out, Extra{
			ParentKey: parent.Key, ParentTitle: parent.Title, Title: title,
			Subtype: sub, ExtraType: num, Site: site, SiteKey: v.Key,
			Language: ISO639(v.ISO639), Official: v.Official,
			PublishedAt: v.Published(), Size: v.Size, Source: source,
			identity: fmt.Sprintf("%d|c|%s|%s", int64(parent.Key), strings.ToLower(site), v.Key),
		})
	}
	return out
}

func dedupeExtras(in []Extra) []Extra {
	seen := map[string]int{}
	out := in[:0:0]
	for _, e := range in {
		k := e.identity
		if i, ok := seen[k]; ok {
			// Keep the better copy (official, then larger).
			if (e.Official && !out[i].Official) || e.Size > out[i].Size {
				out[i] = e
			}
			continue
		}
		seen[k] = len(out)
		out = append(out, e)
	}
	return out
}

func typeRank(sub string) int {
	switch sub {
	case "trailer":
		return 0
	case "teaser":
		return 1
	case "clip", "sceneOrSample":
		return 2
	case "featurette":
		return 3
	case "behindTheScenes":
		return 4
	}
	return 5
}

func sourceRank(src string) int {
	switch src {
	case SourcePlexDiscover:
		return 0
	case SourceCommand, SourceClientYouTube:
		return 1
	}
	return 2
}

func langRank(e Extra, iso string) int {
	switch {
	case iso != "" && e.Language == iso:
		return 0
	case e.Language == "en":
		return 1
	case e.Language == "":
		return 2
	}
	return 3
}

// sortExtras orders by type, official first, direct source before command,
// viewer language, resolution, then newest.
func sortExtras(es []Extra, iso string) {
	sort.SliceStable(es, func(i, j int) bool {
		a, b := es[i], es[j]
		if ra, rb := typeRank(a.Subtype), typeRank(b.Subtype); ra != rb {
			return ra < rb
		}
		oa, ob := a.Official, b.Official
		if oa != ob {
			return oa
		}
		if sa, sb := sourceRank(a.Source), sourceRank(b.Source); sa != sb {
			return sa < sb
		}
		if la, lb := langRank(a, iso), langRank(b, iso); la != lb {
			return la < lb
		}
		if a.Size != b.Size {
			return a.Size > b.Size
		}
		if !a.PublishedAt.Equal(b.PublishedAt) {
			return a.PublishedAt.After(b.PublishedAt)
		}
		return a.Title < b.Title
	})
}
