package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/extend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/llm"
	"github.com/braedonsaunders/ether/internal/notify"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/adapters"
	"github.com/braedonsaunders/ether/internal/plugins/catalogue"
	"github.com/braedonsaunders/ether/internal/plugins/frontendadapter"
	"github.com/braedonsaunders/ether/internal/plugins/kit"
	"github.com/braedonsaunders/ether/internal/plugins/remote"
	"github.com/braedonsaunders/ether/internal/plugins/studio"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
	"github.com/braedonsaunders/ether/internal/screens"
	"github.com/braedonsaunders/ether/internal/state"
)

// pluginRuntime is the plugin host and its adapters.
type pluginRuntime struct {
	mgr            *plugins.Manager
	cat            *catalogue.Service
	studio         *studio.Studio
	tunnel         *remote.TunnelServer
	pairing        *remote.Pairing
	pluginHost     http.Handler
	subtitleProbe  *adapters.SubtitleProbe
	catalogImports *adapters.CatalogImporter
	pluginitems    *pluginitems.Service
	blobcache      *blobcache.Cache
	kindCatalog    *adapters.KindCatalog
	kindResolver   *adapters.KindResolver
	screens        *screens.Service
	// adapterCtrl keeps plugin-provided frontend adapters registered with
	// the frontend registry; adapterTickets mints the scoped stream tickets
	// adapter responses embed.
	adapterCtrl    *frontendadapter.Controller
	adapterTickets *frontendadapter.TicketIssuer
}

func (a *App) buildPlugins(ctx context.Context, cfg config.Config) (*pluginRuntime, error) {
	var busyMu sync.Mutex
	var busyAt time.Time
	var busyValue bool
	durableRoot := filepath.Join(a.opts.DataDir, "plugin-blob-storage")
	diskUsage := a.opts.diskUsage
	if diskUsage == nil {
		diskUsage = func() (int64, int64, error) { return admin.DiskUsage(durableRoot) }
	}
	durableStorage, err := blobcache.OpenDurableStore(durableRoot, diskUsage, func(id string) (blobcache.DurableQuota, int) {
		policy := a.cfg.Read().Plugins
		limits := policy.RuntimeOverrides[id]
		return blobcache.DurableQuota{Bytes: limits.DurableBytes, Files: limits.DurableFiles}, policy.RuntimeLimits.DurableFreePercent
	})
	if err != nil {
		return nil, err
	}
	var ffmpeg, ffprobe string
	if a.transcoder != nil {
		ffmpeg, ffprobe = a.transcoder.FFmpegPath(), a.transcoder.FFprobePath()
	}
	mgr, err := plugins.New(plugins.Deps{
		FFmpeg:            ffmpeg,
		FFprobe:           ffprobe,
		DurableStorage:    durableStorage,
		EventBus:          a.EventBus(),
		IsIndexerHost:     a.isConfiguredIndexerHost,
		EventRegistry:     a.EventRegistry(),
		FlowActions:       a.FlowsActions(),
		ReadRuntimeConfig: func() config.Plugins { return a.cfg.Read().Plugins },
		SaveRuntimeConfig: func(id string, limits config.PluginRuntimeLimits) error {
			return a.cfg.Update(func(c *config.Config) error {
				if id == "" {
					c.Plugins.RuntimeLimits = limits
				} else {
					next := map[string]config.PluginRuntimeLimits{}
					for key, value := range c.Plugins.RuntimeOverrides {
						next[key] = value
					}
					next[id] = limits
					c.Plugins.RuntimeOverrides = next
				}
				return nil
			})
		},
		Busy: func() bool {
			busyMu.Lock()
			defer busyMu.Unlock()
			if time.Since(busyAt) < time.Second {
				return busyValue
			}
			busyAt = time.Now()
			busyValue = false
			check := func() bool {
				if a.playback != nil && len(a.playback.Sessions()) > 0 {
					return true
				}
				if a.livetv != nil {
					if usage, err := a.livetv.LiveUsage(context.Background()); err == nil && usage.Sessions > 0 {
						return true
					}
				}
				if a.load != nil {
					s := a.load.Snapshot()
					if s.Current != nil && (s.Current.CPU > 85 || (s.Current.GPU != nil && *s.Current.GPU > 85)) {
						return true
					}
				}
				return false
			}
			busyValue = check()
			return busyValue
		},
		OnAirContent: a.pluginContent,
		ImportStylePack: func(ctx context.Context, id string, files map[string][]byte) error {
			if a.onair == nil {
				return fmt.Errorf("On Air is unavailable")
			}
			_, err := a.onair.ImportStyleFiles(ctx, id, files)
			return err
		},
		DB: a.db, Sealer: a.cfg,
		CheckPackage: kit.CheckInstall,
		Registry:     a.extendRT.registry, Tools: a.extendRT.tools, Notifications: a.notifications,
		Logger:   a.log.With("component", "plugins"),
		IsLeader: func() bool { return a.cluster == nil || a.cluster.IsLeader() },
		Bus:      clusterbus.FromCluster(a.cluster),
		Backends: plugins.Backends{
			CollectionsUpsert: a.pluginCollectionUpsert,
			CollectionsDelete: a.pluginCollectionDelete,
			ResolveBadgeTitles: func(ctx context.Context, refs []plugins.BadgeTitleRef) ([]string, error) {
				if a.catalog == nil || len(refs) == 0 {
					return make([]string, len(refs)), nil
				}
				crefs := make([]catalog.ExternalTitleRef, len(refs))
				for i, r := range refs {
					crefs[i] = catalog.ExternalTitleRef{
						Type: core.MediaType(r.Type),
						TMDB: r.TMDB,
						IMDB: r.IMDB,
					}
				}
				keys, err := a.catalog.ResolveExternalTitles(ctx, crefs)
				if err != nil {
					return nil, err
				}
				out := make([]string, len(keys))
				for i, k := range keys {
					if k != 0 {
						out[i] = k.String()
					}
				}
				return out, nil
			},
			ViewerNames: func(ctx context.Context, ids []int64) (json.RawMessage, error) {
				out := make([]map[string]any, 0, len(ids))
				for _, id := range ids {
					u, err := a.users.ActiveUser(ctx, id)
					if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnauthorized) || (err == nil && u == nil) {
						continue
					}
					if err != nil {
						return nil, err
					}
					name := u.Title
					if name == "" {
						name = u.Username
					}
					out = append(out, map[string]any{"id": u.ID, "name": name})
				}
				return json.Marshal(out)
			},
			CatalogSearch: func(ctx context.Context, query string, limit, offset int) (json.RawMessage, error) {
				if offset < 0 || offset > 2000 {
					return nil, errors.New("catalog search offset out of range")
				}
				if offset > 0 {
					limit += offset
				}
				titles, err := a.catalog.Search(ctx, query, []core.MediaType{core.TypeMovie, core.TypeShow}, limit)
				if err != nil {
					return nil, err
				}
				if offset > 0 {
					if offset >= len(titles) {
						titles = nil
					} else {
						titles = titles[offset:]
					}
				}
				type titleSummary struct {
					ID     int64  `json:"id"`
					Title  string `json:"title"`
					Type   string `json:"type"`
					Year   int    `json:"year,omitempty"`
					TMDBID int64  `json:"tmdbId,omitempty"`
					IMDBID string `json:"imdbId,omitempty"`
				}
				out := make([]titleSummary, 0, len(titles))
				for _, t := range titles {
					out = append(out, titleSummary{ID: int64(t.Key), Title: t.Title, Type: string(t.Type), Year: t.Year, TMDBID: t.IDs.TMDB, IMDBID: t.IDs.IMDB})
				}
				return json.Marshal(map[string]any{"titles": out})
			},
			CatalogGet: func(ctx context.Context, id string) (json.RawMessage, error) {
				title, err := a.pluginCatalogTitle(ctx, id)
				if err != nil {
					return nil, err
				}
				return json.Marshal(map[string]any{"id": title.Key.String(), "title": title.Title, "type": title.Type, "year": title.Year, "tmdbId": title.IDs.TMDB, "imdbId": title.IDs.IMDB, "genres": title.Genres, "posterPath": title.PosterPath, "summary": title.Summary})
			},
			SetWatchState: a.setPluginWatchState,
			ViewerHistory: func(ctx context.Context, viewerID int64, limit, offset int) (json.RawMessage, error) {
				if viewerID <= 0 || limit <= 0 || limit > 200 || offset < 0 || offset > 2000 {
					return nil, errors.New("invalid history page")
				}
				history, err := a.state.History(ctx, viewerID, time.Time{}, limit+offset)
				if err != nil {
					return nil, err
				}
				if offset >= len(history) {
					return json.RawMessage(`[]`), nil
				}
				history = history[offset:]
				keys := make([]core.RatingKey, len(history))
				for i, entry := range history {
					keys[i] = entry.Key
				}
				statuses, err := a.state.GetMany(ctx, viewerID, keys)
				if err != nil {
					return nil, err
				}
				out := make([]map[string]any, 0, limit)
				for _, entry := range history {
					k := entry.Key
					kind := "movie"
					if k.Kind() == core.KindEpisode {
						kind = "episode"
					} else if k.Kind() == core.KindShow {
						kind = "show"
					}
					progress := float64(0)
					status := statuses[k]
					if status.Duration > 0 {
						progress = float64(status.ViewOffset) / float64(status.Duration)
					}
					out = append(out, map[string]any{"titleId": k.String(), "type": kind, "watchedAt": entry.ViewedAt, "progress": progress, "completed": entry.Completed})
				}
				return json.Marshal(out)
			},
			Notice: func(text string) {
				a.log.Warn("plugin notice", "text", text)
				if a.streamRT != nil {
					_ = a.streamRT.Publish(context.Background(), admin.StreamNotice, map[string]any{"text": text})
				}
			},
			AIComplete: func(ctx context.Context, req plugins.AICompleteBackendRequest) (string, error) {
				if a.llm == nil || a.llm.Client() == nil || !a.llm.Client().Enabled() {
					return "", plugins.ErrNotConfigured
				}
				msgs := make([]llm.Message, len(req.Messages))
				for i, m := range req.Messages {
					msgs[i] = llm.Message{Role: m.Role, Content: m.Content}
				}
				out, err := a.llm.Client().Complete(ctx, llm.CompleteOptions{
					Messages:    msgs,
					JSONMode:    req.JSONMode,
					MaxTokens:   req.MaxTokens,
					Temperature: req.Temperature,
				})
				if errors.Is(err, llm.ErrNotConfigured) {
					return "", plugins.ErrNotConfigured
				}
				return out, err
			},
		},
	})
	if err != nil {
		return nil, err
	}
	indexURL, indexKey := cfg.Plugins.Catalogue()
	cat, err := catalogue.New(catalogue.Options{
		DB:        a.db,
		Manager:   mgr,
		IndexURL:  indexURL,
		PublicKey: indexKey,
		Log:       a.log.With("component", "plugins-catalogue"),
	})
	if err != nil {
		return nil, err
	}
	mgr.SetCatalogue(cat)

	// Hosted plugins run sandboxed in-process (SPEC §5.2).
	engine := wasm.NewEngine(a.log.With("component", "plugins-wasm"))
	mgr.RegisterRuntime("hosted", plugins.HostedRuntimeFactory(engine))
	if err := cat.EnforceCachedRevocations(ctx); err != nil {
		_ = engine.Close()
		return nil, err
	}
	a.closers = append(a.closers, func() { _ = engine.Close() })
	grants, err := identity.NewGrants(a.db, a.users, 30*24*time.Hour, nil)
	if err != nil {
		a.log.Warn("plugin credentials unavailable", "err", err)
	}
	auth := remote.CredentialResolverFunc(func(ctx context.Context, credential string) (string, error) {
		if grants == nil {
			return "", remote.ErrUnavailable
		}
		_, device, _, err := grants.ResolveGrantDevice(ctx, "plugin", credential, "")
		if err != nil || device.ID == "" {
			return "", remote.ErrUnauthorized
		}
		rec, err := mgr.Get(ctx, device.ID)
		if err != nil || !rec.Enabled || rec.Manifest.Ether == nil || rec.Manifest.Ether.Runtime.Kind != "remote" {
			return "", remote.ErrUnauthorized
		}
		return device.ID, nil
	})
	log := a.log.With("component", "plugins-remote")
	tunnel := remote.NewTunnelServer(remote.TunnelOptions{Auth: auth, Logger: log})
	factory := &remote.Factory{Tunnel: tunnel, TrustedLAN: remote.GrantsLAN(mgr.GrantsOf), LocalHosts: remote.GrantsLocalHosts(mgr.GrantsOf), CheckHost: mgr.CheckIndexerNetwork, Logger: log}
	mgr.RegisterRuntime("remote", factory.RuntimeFactory())
	mgr.SetSidecarState(factory.SidecarState)
	// A sidecar that (re)connects is checked at once, so the console does
	// not keep showing the failures recorded while it was away.
	tunnel.SetOnConnect(func(pluginID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = mgr.CheckHealth(ctx, pluginID)
	})
	host := remote.NewHostHandler(remote.HostOptions{Host: remote.HostAdapter{HostFor: mgr.HostFor, SettingsWithSecrets: mgr.PluginSettings, ResolveAdapterViewerToken: mgr.ResolveAdapterViewerToken, ResolveAdapterCallToken: mgr.ResolveAdapterCallToken}, Auth: auth, Logger: log})
	pairing := remote.NewPairing(a.db, grants, func(ctx context.Context) (int64, error) {
		owner, err := a.users.Owner(ctx)
		if err != nil {
			return 0, err
		}
		if owner == nil || !owner.IsOwner {
			return 0, core.ErrUnauthorized
		}
		return owner.ID, nil
	}, func(ctx context.Context, id string) bool {
		rec, err := mgr.Get(ctx, id)
		return err == nil && rec.Enabled && rec.Manifest.Ether != nil && rec.Manifest.Ether.Runtime.Kind == "remote"
	}, tunnel)
	pairing.Exists = func(ctx context.Context, id string) bool { _, err := mgr.Get(ctx, id); return err == nil }
	pairing.TunnelURL = func(r *http.Request) string {
		scheme, host := "ws", portalSanitizeHost(r.Host)
		if r.TLS != nil {
			scheme = "wss"
		}
		if portalProxyTrusted(r, identity.ProxyPrefixes(a.cfg.TrustedProxyCIDRs())) {
			if forwarded := portalForwardedHost(r); forwarded != "" {
				host = forwarded
			}
			if proto := portalForwardedProto(r); proto == "https" {
				scheme = "wss"
			} else if proto == "http" {
				scheme = "ws"
			}
		}
		if host == "" {
			return ""
		}
		return scheme + "://" + host + remote.TunnelPath
	}
	a.closers = append(a.closers, func() { _ = tunnel.Close() })
	adapters.Attach(mgr, a.collections, a.personalize, a.livetv)
	catalogImports := adapters.AttachCatalogImporter(a.db, mgr, a.collections, a.log.With("component", "addon-imports"))
	adapters.AttachSubtitles(mgr, a.subtitles)
	adapters.AttachMetadata(mgr, a.catalog)
	adapters.AttachDebrid(mgr, a.debrid)
	adapters.AttachDebridCacheHints(mgr, a.debrid)
	subtitleProbe := adapters.NewSubtitleProbe(mgr, a.catalog)
	a.closers = append(a.closers, subtitleProbe.Close)
	markerCtx, cancelMarkers := context.WithCancel(context.Background())
	markerBridge := adapters.AttachMarkers(markerCtx, mgr, a.markers, a.catalog, adapters.MarkerBridgeOptions{
		Logger: a.log.With("component", "markers"),
		Selected: func(id string) bool {
			for _, source := range a.cfg.Read().Markers.SourceOrder {
				if source == id {
					return true
				}
			}
			return false
		},
		SkipDB: func() (enabled bool, apiKey, baseURL string) {
			c := a.cfg.Read().Markers
			return c.SkipDB, c.SkipDBAPIKey, ""
		},
	})
	a.closers = append(a.closers, func() { cancelMarkers(); markerBridge.Wait() })

	runCtx, cancelRun := context.WithCancel(context.Background())

	itemsSvc, err := pluginitems.New(ctx, a.db, pluginitems.Options{
		Caller:            mgr,
		Health:            mgr,
		Jobs:              pluginSyncJobTracker{mgr: mgr},
		MaxPluginItemsFor: mgr.MaxPluginItems,
		Logger:            a.log.With("component", "pluginitems"),
		IsLeader:          func() bool { return a.cluster == nil || a.cluster.IsLeader() },
	})
	if err != nil {
		cancelRun()
		return nil, err
	}
	a.closers = append(a.closers, itemsSvc.Close)

	blobDir := filepath.Join(a.cache.Directory(), "plugin-media")
	blobs := a.mediaFiles

	kindCatalog := adapters.AttachKindCatalog(runCtx, adapters.KindCatalogDeps{
		TitleInLibrary: func(ctx context.Context, lib core.Library, key core.RatingKey) (bool, error) {
			return a.catalog.TitleInFilter(ctx, libraryAccessFilter(lib), key)
		},
		TitlesInLibrary: func(ctx context.Context, lib core.Library, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
			return a.catalog.TitlesInFilter(ctx, libraryAccessFilter(lib), keys)
		},
		Manager:   mgr,
		Libraries: a.libraries,
		Items:     itemsSvc,
		Blobs:     blobs,
		BlobDir:   blobDir,
		Logger:    a.log,
	})
	if a.state != nil {
		a.state.SetSkipResume(kindCatalog.SkipResume)
	}

	var idx core.Indexer
	var eng core.Engine
	if a.opts.FixtureMedia != nil {
		idx = a.opts.FixtureMedia
		eng = a.opts.FixtureMedia
	} else {
		if a.sources != nil {
			idx = a.sources
		}
		if a.media != nil {
			eng = a.media
		}
	}
	var deb adapters.DebridSourceBackend
	if a.debrid != nil {
		deb = a.debrid
	}
	var usenetServing func() bool
	if a.opts.FixtureMedia == nil {
		usenetServing = func() bool {
			return a.fetcher == nil || a.fetcher.HasServingProvider()
		}
	}
	var probeStream func(context.Context, string) (*core.MediaInfo, error)
	if tc := a.activeTranscoder(); tc != nil {
		probeStream = tc.Probe
	} else if a.sourceProber != nil {
		probeStream = a.sourceProber.Probe
	}
	var pluginNZBs adapters.PluginNZBStore
	if a.indexers != nil && a.opts.FixtureMedia == nil {
		pluginNZBs = a.indexers
	}
	kindResolver := adapters.NewKindResolver(adapters.KindResolverDeps{
		Manager:       mgr,
		Libraries:     a.libraries,
		Items:         itemsSvc,
		Blobs:         blobs,
		Catalog:       kindCatalog,
		Indexer:       idx,
		PluginNZBs:    pluginNZBs,
		Sources:       a.streamSources(),
		Resolver:      a.resolver,
		Debrid:        deb,
		Engine:        eng,
		UsenetServing: usenetServing,
		Authorize: func(ctx context.Context, viewer *core.Viewer, key core.RatingKey) error {
			_, err := a.pluginPlayableTitle(ctx, kindCatalog, viewer, key)
			return err
		},
		ProbeCache:    a.playback,
		ProbeStream:   probeStream,
		Logger:        a.log,
		PreparingWait: adapters.DefaultKindPreparingWait,
	})
	cachePut, cacheOpen, cacheDelete := blobs.HostBackends(kindCatalog.QuotaFor)
	cachePartial := blobs.HostPartialBackend(kindCatalog.QuotaFor)
	adapterTickets := frontendadapter.NewTicketIssuer()
	var admitUsenetCheck func(ctx context.Context, pluginID string) (context.Context, func(), time.Time, error)
	if a.availability != nil {
		admitUsenetCheck = a.availability.AdmitPluginUsenetCheck
	}
	mgr.SetKindBackends(plugins.KindBackends{
		CacheLimit: func(pluginID, libraryKey string) int64 {
			return blobs.Namespace(libraryKey, kindCatalog.QuotaFor(pluginID, libraryKey)).Quota().MaxBytes
		},
		NormalizeStoredReleaseLinks: kindResolver.NormalizeStoredReleaseLinks,

		KindUpsert:                 itemsSvc.KindUpsert,
		KindUpsertStats:            itemsSvc.KindUpsertWithStats,
		KindUpsertUndo:             itemsSvc.KindUpsertWithUndo,
		KindDelete:                 itemsSvc.KindDelete,
		KindItems:                  itemsSvc.KindItems,
		KindItemsByID:              itemsSvc.KindItemsByID,
		KindItemCount:              itemsSvc.PluginItemCount,
		KindVisible:                a.adapterKindVisible(),
		KindBrowse:                 a.adapterKindBrowse(),
		KindExpand:                 a.adapterKindExpand(),
		KindLiveSearch:             a.adapterKindLiveSearch(),
		ResolveSources:             kindResolver.ResolveSources,
		ResolveSourcesResult:       kindResolver.ResolveSourcesWithResult,
		PeekUsenetCheck:            kindResolver.PeekUsenetCheck,
		AdmitBackgroundUsenetCheck: admitUsenetCheck,
		CachePut:                   cachePut,
		CacheLookup: func(pluginID, libraryKey, key string) (string, bool) {
			ref, ok := blobs.Namespace(libraryKey, kindCatalog.QuotaFor(pluginID, libraryKey)).LookupKey(key)
			return string(ref), ok
		},
		CacheOpen:        cacheOpen,
		CacheOpenForStat: blobs.OpenPluginBlobForStat,
		CacheDelete:      cacheDelete,
		CachePartial:     cachePartial,
		StreamURL:        a.adapterStreamURL(adapterTickets),
		StreamPartURL:    a.adapterStreamPartURL(adapterTickets),
		StreamURLOptions: a.adapterStreamURLOptions(adapterTickets),
		MediaProbe:       kindResolver.ProbeMedia,
	})
	mgr.SetKindViewerState(adapterViewerState{app: a})

	if err := mgr.Start(ctx); err != nil {
		cancelRun()
		return nil, err
	}
	a.bindStreamSources(mgr)
	// Installed plugins that serve streams are run-time indexer rows: keep
	// the running search manager in step with installs, removals and role
	// switches. Also refresh published library sections when plugin libraries
	// appear or disappear.
	mgr.OnSync(func(_ context.Context, r plugins.Record) {
		if r.Manifest.IsStremioOnly() || r.Manifest.HasCapability(plugins.CapStreamSource) {
			a.reapplyAddonIndexers()
		}
		if r.Manifest.Ether != nil && len(r.Manifest.Ether.Contributes.MediaKinds) > 0 {
			a.publishLibrarySections()
		}
	})
	mgr.OnRemove(func(context.Context, string) {
		a.reapplyAddonIndexers()
		a.publishLibrarySections()
	})
	screensSvc := a.buildScreensService(mgr)

	if a.resolver != nil {
		a.resolver.SetRankingHooks(adapters.Ranking{Manager: mgr, Registry: a.extendRT.registry})
	}
	unsubscribe := mgr.SubscribeInvalidations()

	studioSvc := studio.New(studio.Deps{
		Manager: mgr,
		Engine:  engine,
		Publish: func(pubCtx context.Context, event string, data any) error {
			if a.streamRT != nil {
				a.streamRT.Publish(pubCtx, event, data)
			}
			return nil
		},
		Logger: a.log.With("component", "plugins-studio"),
		Installed: func(_ context.Context, rec plugins.Record) {
			a.startPluginLiveTV(rec)
		},
	})

	if err := studioSvc.OpenStorage(ctx, a.db); err != nil {
		cancelRun()
		unsubscribe()
		return nil, err
	}
	// Auto-import Live TV channels when a native channels plugin is newly installed.
	mgr.OnInstall(func(_ context.Context, rec plugins.Record) {
		if !rec.Manifest.SpeaksStremio() {
			a.startPluginLiveTV(rec)
		}
	})
	// Removing a plugin removes the drafts it was installed from.
	mgr.OnRemove(func(ctx context.Context, id string) { studioSvc.ForgetPlugin(ctx, id) })

	// Event sources (SPEC §5.7): playback, watch state and notifications.
	publish := func(ev plugins.Event) {
		pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mgr.Publish(pctx, ev)
	}
	stopPlayback := func() {}
	if a.playback != nil {
		stopPlayback = a.playback.Subscribe(func(ev playback.Event) {
			if ev.Session != nil && !ev.Session.PreparedOnly && !ev.Session.Info.Estimated {
				switch ev.Kind {
				case "start", "surface", "failover", "downfailover":
					markerBridge.Schedule(ev.Session.Key, ev.Session.Info.Duration)
				}
			}
			name := plugins.NormalizePlayback(ev.Kind, "")
			if name == "" {
				return
			}
			payload, _ := json.Marshal(map[string]any{"kind": ev.Kind})
			var viewer int64
			if ev.Session != nil {
				viewer = ev.Session.UserID
				payload, _ = json.Marshal(map[string]any{
					"kind": ev.Kind, "ratingKey": int64(ev.Session.Key),
				})
			}
			publish(plugins.Event{Name: name, At: ev.At, ViewerID: viewer, Payload: payload})
		})
	}
	stopState := func() {}
	if a.state != nil {
		stopState = a.state.Subscribe(func(ev state.Event) {
			var name string
			switch ev.Kind {
			case state.EventScrobble:
				// Plugin marks already have watch events with origin-aware
				// delivery. They are not legacy playback completions.
				if strings.HasPrefix(string(ev.Origin.Surface), events.PluginPrefix) {
					return
				}
				name = plugins.EventPlaybackCompleted
			case state.EventCompleted:
				name = plugins.EventPlaybackCompleted
			case state.EventProgress:
				name = plugins.NormalizePlayback("update", ev.State)
			}
			if name == "" {
				return
			}
			payload, _ := json.Marshal(map[string]any{"ratingKey": int64(ev.Key), "completed": ev.Completed})
			publish(plugins.Event{Name: name, At: ev.At, ViewerID: ev.UserID, Payload: payload})
		})
	}
	stopNotify := func() {}
	if a.notifications != nil {
		stopNotify = a.notifications.Subscribe(func(ev notify.Event) {
			if ev.Kind != "title.available" && ev.Kind != "user.joined" {
				return
			}
			payload := json.RawMessage(`{}`)
			if ev.Kind == "title.available" {
				payload, _ = json.Marshal(map[string]any{"title": ev.Title, "message": ev.Message})
			}
			publish(plugins.Event{Name: plugins.NormalizeNotify(ev.Kind), At: ev.At, Payload: payload})
		})
	}

	go mgr.Run(runCtx)
	go cat.Run(runCtx, catalogue.DefaultRefreshInterval)
	a.closers = append(a.closers, func() {
		cancelRun()
		kindResolver.Close()
		stopPlayback()
		stopState()
		stopNotify()
		unsubscribe()
	})
	return &pluginRuntime{
		subtitleProbe:  subtitleProbe,
		catalogImports: catalogImports,
		mgr:            mgr,
		cat:            cat,
		studio:         studioSvc,
		tunnel:         tunnel,
		pairing:        pairing,
		pluginHost:     host,
		pluginitems:    itemsSvc,
		blobcache:      blobs,
		kindCatalog:    kindCatalog,
		kindResolver:   kindResolver,
		screens:        screensSvc,
		adapterTickets: adapterTickets,
	}, nil
}

// pluginHandlers mounts the owner plugin API under /api/plugins (WS7, WS11).
func (a *App) pluginHandlers(m map[string]http.Handler) {
	if a.pluginsRT != nil && a.pluginsRT.mgr != nil {
		h := a.pluginsRT.mgr.Handler()
		if a.pluginsRT.studio != nil {
			h = a.pluginsRT.studio.WrapPluginHandler(h)
		}
		h = a.wrapPluginLiveEventsHandler(h)
		if a.pluginsRT.pairing != nil {
			wrapped := a.pluginsRT.pairing.WrapOwner(h)
			h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
				if len(parts) >= 2 && (parts[1] == "pair" || parts[1] == "pairings") {
					access, ok := admin.RequestAccess(r.Context())
					if !ok || !access.Owner {
						http.Error(w, "owner authentication required", http.StatusUnauthorized)
						return
					}
				}
				wrapped.ServeHTTP(w, r)
			})
		}
		m["plugins"] = h
		if a.pluginsRT.subtitleProbe != nil {
			tests := a.pluginsRT.subtitleProbe.Handler()
			m["plugin-tests"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				access, ok := admin.RequestAccess(r.Context())
				if !ok || !access.Owner {
					http.Error(w, "owner authentication required", http.StatusUnauthorized)
					return
				}
				tests.ServeHTTP(w, r)
			})
		}
	}
}

// publicExtensionHandlers are console API subtrees that authorise each
// request themselves: the add-on settings relay checks its own token.
func (a *App) publicExtensionHandlers() map[string]http.Handler {
	if a.pluginsRT == nil || a.pluginsRT.mgr == nil {
		return nil
	}
	return map[string]http.Handler{"addon-relay": a.pluginsRT.mgr.RelayHandler()}
}

// mountPlugins mounts the plugin tunnel and plugin host API (WS8b).
func (a *App) mountPlugins(mux *http.ServeMux) {
	if a.pluginsRT == nil {
		return
	}
	if a.pluginsRT.tunnel != nil {
		mux.Handle("GET "+remote.TunnelPath, a.pluginsRT.tunnel.Handler())
	}
	if a.pluginsRT.pluginHost != nil {
		mux.Handle(remote.HostPath, a.perf.wrap("plugin-host", a.pluginsRT.pluginHost))
	}
	if a.pluginsRT.pairing != nil {
		mux.Handle(remote.PairExchangePath, lanOnly(http.HandlerFunc(a.pluginsRT.pairing.Exchange), a.cfg.TrustedProxyCIDRs, nil))
	}
}

// pluginTextResolver resolves plugin strings for the registry (WS7).
func (a *App) pluginTextResolver() extend.TextResolver {
	if a.pluginsRT == nil || a.pluginsRT.mgr == nil {
		return nil
	}
	return a.pluginsRT.mgr.TextResolver()
}

// startPluginLiveTV adds a Live TV source for a newly installed channels
// plugin, so its lineup lands in the guide without a second setup step.
// It runs once per install; an owner who later removes the source keeps
// that choice.
func (a *App) startPluginLiveTV(rec plugins.Record) {
	if a.livetv == nil || !rec.Manifest.HasCapability(plugins.CapLiveTV) {
		return
	}
	kind := extend.PluginID(rec.ID, "channels")
	go func() {
		a.pluginLiveTVMu.Lock()
		defer a.pluginLiveTVMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// The source kind registers when the install syncs; wait for it.
		for attempt := 0; attempt < 10; attempt++ {
			sources, err := a.livetv.Sources(ctx)
			if err == nil {
				for _, s := range sources {
					if s.Kind == kind {
						return
					}
				}
			}
			if _, err = a.livetv.ImportSource(ctx, livetv.Source{Name: rec.Name, Kind: kind}); err == nil {
				return
			}
			a.log.Debug("plugin live tv source not ready", "plugin", rec.ID, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
}

// pluginPlayableTitle is the viewer check for a plugin library item: the
// library must be visible to the viewer and active, and the item must pass
// the viewer's rating and safety policy. Resolving a source and re-checking
// a playing session both use it.
func (a *App) pluginPlayableTitle(ctx context.Context, kindCatalog *adapters.KindCatalog, viewer *core.Viewer, key core.RatingKey) (*core.Title, error) {
	if viewer == nil || viewer.ID <= 0 || a.users == nil || kindCatalog == nil {
		return nil, core.ErrNotFound
	}
	u, err := a.users.PolicyUser(ctx, viewer.ID)
	if err != nil || u == nil {
		return nil, core.ErrNotFound
	}
	piv, err := kindCatalog.Item(ctx, key)
	if err != nil {
		return nil, core.ErrNotFound
	}
	if piv.Library.Hidden || !u.AllowsLibrary(piv.Library.ID) || !kindCatalog.IsPluginLibraryActive(ctx, piv.Library) {
		return nil, core.ErrNotFound
	}
	if app := core.LiveApp(ctx); app != "" {
		if _, _, ok := presentLibraryForProtocol(piv.Library.Present, core.Protocol(app), kindCatalog.RootMediaType(ctx, piv.Library)); !ok {
			return nil, core.ErrNotFound
		}
	}
	if u.MaxContentRating != "" && piv.Title.ContentRating != "" && contentrating.Exceeds(piv.Title.ContentRating, u.MaxContentRating) {
		return nil, core.ErrNotFound
	}
	if !experience.TitleAllowed(a.safety, u, &piv.Title) {
		return nil, core.ErrNotFound
	}
	t := piv.Title
	return &t, nil
}

// isConfiguredIndexerHost reserves plugin egress to configured API hosts.
func (a *App) isConfiguredIndexerHost(host string) bool {
	if a.cfg == nil {
		return false
	}
	if matchesIndexerHost(a.cfg.Read().Prowlarr.URL, host) {
		return true
	}
	if a.indexers == nil {
		return false
	}
	if a.indexers.IsIndexerHost(host) {
		return true
	}
	if a.sites == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	for _, client := range a.indexers.Clients() {
		cfg := client.Config()
		if !cfg.IsBuiltin() {
			continue
		}
		site, err := a.sites.Definition(ctx, cfg.Definition)
		if err != nil {
			continue
		}
		for _, link := range site.Links {
			if matchesIndexerHost(link, host) {
				return true
			}
		}
	}
	return false
}

func matchesIndexerHost(rawURL, host string) bool {
	u, err := url.Parse(rawURL)
	return err == nil && u.Hostname() != "" && strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), strings.TrimSuffix(host, "."))
}
