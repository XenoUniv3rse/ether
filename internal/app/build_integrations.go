package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/cachefill"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/episodeorder"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/spike"
	"github.com/braedonsaunders/ether/internal/transcode"
	"github.com/braedonsaunders/ether/internal/trickplay"
)

func (a *App) buildIntegrations(ctx context.Context, cfg config.Config) error {
	var err error
	var sharedShelf *cachefill.SharedStore
	if a.cache.Shared() {
		sharedShelf = cachefill.NewSharedStore(a.db, a.cacheNodeID())
	}
	a.cachefill, err = cachefill.New(a.cache.Directory(), cachefill.Deps{
		Shared:            sharedShelf,
		Allowed:           a.safety.Allowed,
		WatchlistPosition: a.cacheWatchlistPosition,
		WatchlistIntents:  a.cacheWatchlistIntents, WatchlistDone: a.cacheWatchlistDone,
		Catalog: a.catalog, Resolver: a.resolver, Engine: a.media, Cache: a.cache,
		State: a.state, Household: a.cacheHousehold,
		Config: a.runningCacheConfig, Owner: a.users.Owner,
		Active: func() bool {
			if len(a.playback.Sessions()) > 0 {
				return true
			}
			if a.cache.Shared() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				return len(a.playback.RemoteSessions(ctx)) > 0
			}
			return false
		},
		FreeBytes: func() (int64, error) { return admin.DiskFree(a.cache.Directory()) },
		Remember:  a.playback.Remember, Logger: a.log,
	})
	if err != nil {
		return fmt.Errorf("managed cache: %w", err)
	}
	a.buildInsights()
	if err := a.notifications.InitializePush(ctx); err != nil {
		a.log.Warn("notifications: push identity unavailable", "err", err)
	}
	a.buildDiscovery()
	a.episodeorder = episodeorder.New(a.db, a.tmdb, a.catalog)
	a.catalog.SetMetadataEpisodeGroup(a.episodeorder.MetadataGroup)
	a.buildUploadBudget()
	if err := a.buildLiveTV(); err != nil {
		return fmt.Errorf("live TV: %w", err)
	}
	a.buildCalendar()
	a.cfg.Subscribe(a.applyConfig)

	if err := a.buildAvailability(); err != nil {
		return err
	}
	a.buildExtras()
	if err := a.buildPrerolls(); err != nil {
		return fmt.Errorf("pre-rolls: %w", err)
	}
	if err := a.buildExtensibility(ctx, cfg); err != nil {
		return err
	}
	if a.transcoder != nil {
		if a.subtitles != nil {
			a.subtitles.SetFFmpeg(a.transcoder.FFmpegPath())
		}
		a.trickplay = trickplay.New(transcode.NewThumbnails(transcode.ThumbOptions{FFmpeg: a.transcoder.FFmpegPath(), Logger: a.log}))
		a.closers = append(a.closers, func() { _ = a.trickplay.Close() })
	}
	a.frontendClients = newFrontendClients(a.db, a.log.With("component", "frontend-clients"), nil)
	a.closers = append(a.closers, a.frontendClients.Close)
	if cfg.Spike.Enabled {
		a.spikeLibrary, err = spike.New(ctx, a.cfg.Read, spike.Options{Logger: a.log.With("component", "spike")})
		if err != nil {
			return fmt.Errorf("local library: %w", err)
		}
		if mgr, ok := a.spikeLibrary.Playback().(*playback.Manager); ok && mgr != nil {
			mgr.Subscribe(func(playback.Event) {
				a.notifySessionsChanged()
			})
		}
		a.closers = append(a.closers, func() { _ = a.spikeLibrary.Close() })
	}
	if a.transcoder != nil && !cfg.Spike.Enabled {
		if download, e := a.transcoder.NewDownloadRemuxer(cfg.Downloads); e == nil {
			a.downloadRemuxer = download
		} else {
			a.log.Warn("offline downloads unavailable", "err", e)
		}
	}
	a.frontends, err = newFrontendRuntime(a, cfg)
	if err != nil {
		return fmt.Errorf("front-end registry: %w", err)
	}
	a.closers = append(a.closers, a.frontends.close)
	a.cfg.Subscribe(a.frontends.reconcile)
	consoleMux := http.NewServeMux()
	consoleMux.Handle("/ether/", a.perf.wrap("console", lanOnly(a.buildAdmin(), a.cfg.TrustedProxyCIDRs, a.cfg.AllowRemoteConsole)))
	a.mountExtensibility(consoleMux)
	consoleRedirect := lanOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ether/", http.StatusFound)
	}), a.cfg.TrustedProxyCIDRs, a.cfg.AllowRemoteConsole)
	consoleMux.Handle("/ether", consoleRedirect)
	consoleMux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			consoleRedirect.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	a.consoleHandler = consoleMux

	mux := http.NewServeMux()
	a.mountAdapterMedia(mux)
	gateConsole := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !a.consoleOnMain() {
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	mux.Handle("/ether/", gateConsole(consoleMux))
	mux.Handle("/ether", gateConsole(consoleMux))
	mux.Handle("/.well-known/oauth-protected-resource", gateConsole(consoleMux))
	mux.Handle("/.well-known/oauth-protected-resource/ether/mcp", gateConsole(consoleMux))
	mux.Handle("/.well-known/oauth-authorization-server", gateConsole(consoleMux))
	frontendHandler := a.frontends.plexHandler()
	mux.Handle("/", browserRootRedirectWhen(a.consoleOnMain, frontendHandler))
	a.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Roku sends an empty show ID with an explicit episode anchor. Let the
		// MediaBrowser router handle that registered path before ServeMux cleans it.
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "//") &&
			(strings.EqualFold(r.URL.Path, "/jellyfin/shows//episodes") || strings.EqualFold(r.URL.Path, "/emby/shows//episodes")) {
			if h := a.frontends.control.listeners.sharedHandler(r.URL.Path); h != nil {
				h.ServeHTTP(w, r)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
	return nil
}

// consoleOnMain reports whether /ether and its console APIs are currently
// served on Ether's main (Plex) port. Always true unless a dedicated console
// listener is bound and DisableConsoleOnMainPort is enabled.
func (a *App) consoleOnMain() bool {
	if f := a.front.Load(); f != nil {
		return f.consoleOnMain.Load()
	}
	return true
}

func (a *App) consoleListenerStatus() admin.ConsoleListenerStatus {
	if f := a.front.Load(); f != nil {
		return f.consoleStatus()
	}
	cfg := a.cfg.Get()
	return admin.ConsoleListenerStatus{
		ConfiguredPort: cfg.Server.ConsolePort,
		ActivePort:     0,
		MainPort:       cfg.Server.Port,
		OnMainPort:     true,
	}
}

// browserRootRedirect sends a person who types the server's address into a
// browser to the console instead of Plex's 401. Only a bare GET or HEAD of
// "/" that asks for HTML and carries nothing of Plex's (no X-Plex-* header,
// no query) is redirected; every client request reaches Plex unchanged.
func browserRootRedirect(next http.Handler) http.Handler {
	return browserRootRedirectWhen(nil, next)
}

func browserRootRedirectWhen(allow func() bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (allow == nil || allow()) && isBrowserRootRequest(r) {
			http.Redirect(w, r, "/ether/", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isBrowserRootRequest(r *http.Request) bool {
	if r.URL.Path != "/" || r.URL.RawQuery != "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	for k := range r.Header {
		if len(k) >= len("X-Plex-") && strings.EqualFold(k[:len("X-Plex-")], "X-Plex-") {
			return false
		}
	}
	for _, part := range strings.Split(strings.Join(r.Header.Values("Accept"), ","), ",") {
		mt, _, _ := strings.Cut(part, ";")
		if strings.EqualFold(strings.TrimSpace(mt), "text/html") {
			return true
		}
	}
	return false
}
