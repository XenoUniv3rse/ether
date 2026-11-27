package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/animemap"
	"github.com/braedonsaunders/ether/internal/cardigann"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/scenenumbering"
	"github.com/braedonsaunders/ether/internal/segcache"
	"github.com/braedonsaunders/ether/internal/stream"
	"github.com/braedonsaunders/ether/internal/stremio"
	"github.com/braedonsaunders/ether/internal/sysload"
	"github.com/braedonsaunders/ether/internal/usenet"
	"github.com/braedonsaunders/ether/internal/vault"
)

func (a *App) buildSources(ctx context.Context, cfg config.Config) error {
	log := a.log
	dataDir := a.opts.DataDir
	var err error
	// Usenet, cache, stream engine. The dialer reads the live proxy policy.
	a.applyOutbound(cfg)
	a.fetcher = usenet.New(cfg.Usenet.Providers, usenet.Options{Logger: log.With("component", "usenet"), MinIdle: cfg.Usenet.MinIdle, Balanced: cfg.Usenet.Balanced(), DialContext: egress.Dial, OnStateChange: a.publishDashboard, Events: a.eventBus})
	a.closers = append(a.closers, func() { a.fetcher.Close() })
	if cfg.Cache.Storage == "shared" {
		if cfg.SharedCache == nil {
			return errors.New("configure the shared cache policy in Server → Cache before starting a NAS-backed node")
		}
		a.cache, err = segcache.OpenShared(cfg.Cache.Dir, cfg.Cache.BudgetBytes, a.db, a.cacheNodeID(), log)
	} else {
		a.cache, err = segcache.Open(cfg.Cache.Dir, cfg.Cache.BudgetBytes, log)
	}
	if err != nil {
		return fmt.Errorf("segment cache: %w", err)
	}
	a.closers = append(a.closers, func() { a.cache.Close() })
	// Keep part of the cache's volume free whatever the budget says.
	cacheDir := a.cache.Directory()
	diskUsage := a.opts.diskUsage
	if diskUsage == nil {
		diskUsage = func() (int64, int64, error) { return admin.DiskUsage(cacheDir) }
	}
	a.cache.SetDiskUsage(diskUsage)
	a.cache.SetEventBus(a.eventBus)
	a.configureCache(cfg.Cache)
	a.engine = stream.New(a.fetcher, a.cache, a.vaultStreamOptions(cfg), log.With("component", "stream"))
	a.closers = append(a.closers, func() { a.engine.Close() })

	shared := cfg.Cache.Storage == "shared"
	local, err := vault.Open(ctx, vault.Options{Dir: filepath.Join(cacheDir, "vault"), DB: a.db, Sealer: a.cfg, NodeID: a.cacheNodeID(), Shared: shared, CapBytes: cfg.Cache.LibraryBudgetBytes, Logger: log.With("component", "vault"), Events: a.eventBus})
	if err != nil {
		return fmt.Errorf("open vault: %w", err)
	}
	a.vault = a.clusterVault(ctx, local, shared)
	legacyDir := filepath.Join(dataDir, "nzb")
	var migrated atomic.Bool
	if m, ok := local.(vault.Migrator); ok {
		migrationCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		done := make(chan struct{})
		go func() {
			defer close(done)
			// Pass after pass until the old directory is empty: files that
			// failed (a Windows sharing violation, a busy database) stay
			// readable and are retried with a growing pause.
			for wait := time.Minute; ; wait = min(wait*2, time.Hour) {
				err := m.MigrateDir(migrationCtx, legacyDir)
				if err == nil {
					migrated.Store(true)
					return
				}
				if migrationCtx.Err() != nil {
					return
				}
				log.Warn("NZB migration incomplete; retrying", "err", err, "retryIn", wait)
				select {
				case <-migrationCtx.Done():
					return
				case <-time.After(wait):
				}
			}
		}()
		a.closers = append(a.closers, func() { cancel(); <-done })
	}

	// Indexers and resolver.
	indexerHosts := newznab.NewHostLimiter(0)
	siteOpts := cardigann.Options{DB: a.db, Sealer: a.cfg, Logger: log.With("component", "cardigann"), Sites: cfg.IndexerSites, HostLimiter: indexerHosts, ClientIdentity: cfg.ClientIdentity}
	if a.clustered() {
		siteOpts.LoginLock = a.clusterLoginLock // one login per site across nodes
	}
	a.sites = cardigann.New(siteOpts)
	var animeCacheDir string
	if dataDir != "" {
		animeCacheDir = filepath.Join(dataDir, "cache", "animemap")
	}
	animeMap := animemap.New(animemap.Options{CacheDir: animeCacheDir, Logger: log.With("component", "animemap")})
	specials := stremio.NewSpecialMapper(egress.Client(egress.CapStremio, 3*time.Second), "https://v3-cinemeta.strem.io")
	a.addons = stremio.NewServiceWithOptions(stremio.Options{
		Anime:    animeMap,
		Specials: specials,
		ClientIdentity: func() config.ClientIdentity {
			if a.cfg == nil {
				return config.ClientIdentity{}
			}
			return a.cfg.Get().ClientIdentity
		},
	})
	a.indexers = newznab.NewManager(a.db, a.addonIndexers(context.Background(), cfg.Indexers), newznab.Options{
		PluginReferences: a.cfg,
		Logger:           log.With("component", "newznab"),
		Events:           a.eventBus,
		SearchCacheTTL:   cfg.Resolver.SearchCacheTTL.D(),
		Vault:            a.vault,
		LegacyNZBDir: func() string {
			if migrated.Load() {
				return ""
			}
			return legacyDir
		},
		PublicIndexer: func(ctx context.Context, ix config.Indexer) bool {
			if !ix.IsBuiltin() {
				return false
			}
			d, err := a.sites.Definition(ctx, ix.Definition)
			return err == nil && d.Type == "public"
		},
		ClientIdentity: cfg.ClientIdentity,
		HostLimiter:    indexerHosts,
		Backend: func(ix config.Indexer) (newznab.Backend, error) {
			if ix.IsStremio() {
				return a.addons.Backend(ix)
			}
			// An installed plugin's streams (made at run time, never saved).
			if ix.IsAddon() {
				if ss := a.streamSources(); ss != nil {
					return ss.Backend(ix)
				}
				return nil, fmt.Errorf("indexer %q: add-ons are not available yet", ix.Name)
			}
			return a.sites.Backend(ix)
		},
	})
	a.indexers.ApplyConfig(a.withAddonIndexers(cfg))
	a.closers = append(a.closers, a.indexers.Close)
	a.indexers.Subscribe(func(ev newznab.Event) {
		if !ev.Success || ev.BudgetExhausted || (a.streamRT != nil && a.streamRT.Subscribers() > 0) {
			a.publishDashboard()
		}
	})
	a.debrid = debrid.NewService(cfg.Debrid, debrid.Options{ProviderOptions: a.opts.debridProviderOptions, IsIndexerHost: a.isConfiguredIndexerHost, AcquireStreamProvider: func(ctx context.Context, id string) (func(), error) {
		if a.livetv == nil {
			return nil, core.ErrSourceUnavailable
		}
		return a.livetv.AcquireProvider(ctx, id)
	}, DB: a.db, Logger: log.With("component", "debrid"), OnChange: a.publishDashboard, Events: a.eventBus})
	a.debrid.SetVault(a.vault)
	ss := a.initStreamSources(a.indexers)
	ss.SetAnimeIDs(a.addons.AnimeIDs())
	ss.SetSpecialEpisodes(specials)
	usenetReady := func() bool {
		if a.fetcher != nil {
			return a.fetcher.HasUsableProvider()
		}
		for _, p := range a.cfg.Get().Usenet.Providers {
			if p.Enabled {
				return true
			}
		}
		return false
	}
	ss.SetUsenetReady(usenetReady)
	eng := ss.WrapEngine(a.engine)
	a.media = debrid.NewEngine(a.debrid, debrid.EngineOptions{Inner: eng, Health: eng, Cache: a.cache, Logger: log.With("component", "debrid")})
	a.closers = append(a.closers, func() { a.media.Close() })
	a.sources = debrid.NewIndexer(&livetv.VODIndexer{Inner: ss, Service: func() *livetv.Service { return a.livetv }}, a.debrid)
	a.sources.SetPackNamer(a.engine)
	a.sources.SetUsenetReady(func() bool {
		return usenetReady() || a.hasEnabledPluginSources()
	})
	indexer := core.Indexer(parityIndexer(a.sources, dataDir))
	health := core.HealthChecker(parityHealth(a.media, dataDir))
	animeCour := resolver.AnimeCour(a.addons)
	animeIDs := resolver.AnimeIDs(animeMap)
	releaseTitleIDs := a.releaseTitleIDs
	verifyTitleIDs := a.verifyTitleIDs
	usenetServing := func() bool {
		return a.fetcher == nil || a.fetcher.HasServingProvider()
	}
	if a.opts.FixtureMedia != nil {
		indexer, health = a.opts.FixtureMedia, a.opts.FixtureMedia
		animeCour = nil
		animeIDs = nil
		releaseTitleIDs = nil
		verifyTitleIDs = nil
		usenetServing = nil
	}
	sceneNumbers := scenenumbering.New(scenenumbering.Options{})
	a.libProfiles = newLibraryProfiles(a)
	a.resolver = resolver.New(indexer, health, a.db, cfg, resolver.Options{Vault: a.vault, Events: a.eventBus, Verifier: a.vaultVerifier(), EpisodeNumbering: func(ctx context.Context, req core.SearchRequest, cachedOnly bool) (core.ReleaseNumbering, error) {
		if a.episodeorder == nil {
			return core.ReleaseNumbering{}, nil
		}
		show := core.ShowKey(req.IDs.TMDB)
		pref, err := a.episodeorder.ReleasePreference(ctx, show)
		if err != nil {
			return core.ReleaseNumbering{}, err
		}
		switch pref.Mode {
		case "tmdb":
			return core.ReleaseNumbering{}, nil
		case "group":
			season, episode, err := a.episodeorder.SearchNumbering(ctx, show, req.Season, req.Episode, cachedOnly)
			out := core.ReleaseNumbering{Explicit: true}
			if season > 0 && episode > 0 {
				out.Aliases = []core.EpisodeNumber{{Season: season, Episode: episode}}
				out.PackTargets = out.Aliases
			}
			return out, err
		default:
			if cachedOnly {
				return sceneNumbers.LookupCached(req, func() (core.EpisodeLayout, error) { return a.catalog.ReleaseEpisodeLayout(ctx, show) })
			}
			return sceneNumbers.Lookup(ctx, req, func() (core.EpisodeLayout, error) { return a.catalog.ReleaseEpisodeLayout(ctx, show) })
		}
	}, EpisodeIdentity: a.catalog.EpisodeIdentity, SpecialTitles: a.cachedSpecialTitles, Now: parityClock(), SearchObserved: a.observeSearch, Discovered: a.discoveredReleases, Observed: a.observeRelease, Invalidated: a.invalidateRelease, SeriesYearConflict: a.seriesYearConflict, ReleaseTitleIDs: releaseTitleIDs, VerifyTitleIDs: verifyTitleIDs, SeriesNamesake: a.seriesNamesake, AnimeCour: animeCour, AnimeIDs: animeIDs, LibraryProfiles: a.libProfiles, UsenetServing: usenetServing, Logger: log.With("component", "resolver")})
	a.resolver.SetSourceSignals(a.debrid)
	a.closers = append(a.closers, func() { a.resolver.Close() })

	a.load = sysload.New(sysload.Options{Logger: log.With("component", "sysload")})

	return nil
}
