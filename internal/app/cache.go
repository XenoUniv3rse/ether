package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/segcache"
	"github.com/braedonsaunders/ether/internal/vault"
)

// cacheNodeID identifies the NAS writer. Standalone metadata stays in this
// server's SQLite database; the mount marker prevents unrelated servers from
// adopting the same pool. Cluster members use their registered node identity.
func (a *App) cacheNodeID() string {
	if id := a.clusterNodeID(); id != "" {
		return id
	}
	return "standalone"
}

// adoptMountedCache points a fresh install at the Docker /cache volume before
// the cache opens, so the wizard does not ask for a restart to use it.
func (a *App) adoptMountedCache(cfg config.Config) config.Config {
	if !admin.ShouldAdoptCacheVolume(cfg.SetupComplete, cfg.Cache.Storage, cfg.Cache.Dir, a.opts.DataDir, admin.CacheVolumeMounted()) {
		return cfg
	}
	if err := a.cfg.Update(func(c *config.Config) error {
		if admin.ShouldAdoptCacheVolume(c.SetupComplete, c.Cache.Storage, c.Cache.Dir, a.opts.DataDir, true) {
			c.Cache.Dir = "/cache"
		}
		return nil
	}); err != nil {
		a.log.Warn("cache: could not adopt the /cache volume", "err", err)
		return cfg
	}
	next := a.cfg.Get()
	next.Cache = next.EffectiveCache()
	return next
}

func (a *App) configureCache(c config.Cache) {
	// Keep the active backend and its policy together until the node restarts.
	if a.cache.Directory() != c.Dir || a.cache.Shared() != (c.Storage == "shared") {
		return
	}
	a.cacheConfig.Store(&c)
	a.cache.SetPolicy(segcache.Policy{Mode: c.Mode, Retention: time.Duration(c.RetentionDays) * 24 * time.Hour, ReplayBoost: time.Duration(c.ReplayBoostHours) * time.Hour})
	a.cache.SetBudget(c.BudgetBytes)
	if capper, ok := a.vault.(vault.Capper); ok {
		capper.SetCapBytes(c.LibraryBudgetBytes)
	}
}

func protocolOrigin(protocol string) string {
	switch protocol {
	case core.ProtocolTorrent:
		return "Debrid"
	case core.ProtocolHTTP:
		return "HTTP"
	default:
		return "Usenet"
	}
}

func hasReleaseInfo(info core.ReleaseInfo) bool {
	return info.Height > 0 || info.Source != "" || info.VideoCodec != "" || info.AudioCodec != "" || info.HDR10 || info.DolbyVision || info.Atmos || info.Group != ""
}

// This extension is mounted behind the owner API's authentication and CSRF guard.
func (a *App) cacheHandler() http.Handler {
	a.cache.SetBlockCheck(a.resolver.LocalReleaseBlocked)
	mux := &route.Mux{}
	a.registerWarmingControls(mux)
	a.registerVaultAdmin(mux)
	mux.HandleFunc("POST /clear-all", func(w http.ResponseWriter, r *http.Request) {
		if len(a.playback.Sessions()) > 0 || len(a.playback.RemoteSessions(r.Context())) > 0 {
			cacheConflict(w)
			return
		}
		var freed int64
		complete := true
		pins := a.cache.Pins()
		for _, release := range a.cache.Releases() {
			before := a.cache.ReleaseBytes(release.ReleaseID)
			// With nothing playing, Clear cache removes everything,
			// including the resume protection kept for unfinished titles;
			// the next play caches them again.
			for _, reason := range pins[release.ReleaseID] {
				if reason == playback.PinReasonInProgress {
					_ = a.cache.Unpin(release.ReleaseID, reason)
				}
			}
			if err := a.cache.ClearUnpinned(release.ReleaseID); err != nil {
				complete = false
				continue
			}
			freed += max(int64(0), before-a.cache.ReleaseBytes(release.ReleaseID))
		}
		if a.mediaFiles != nil {
			n, err := a.mediaFiles.ClearFiles()
			freed += n
			if err != nil {
				complete = false
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"freedBytes": freed, "complete": complete})
	})
	mux.HandleFunc("POST /clear-file", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			LibraryKey string        `json:"libraryKey"`
			Ref        blobcache.Ref `json:"ref"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || body.LibraryKey == "" || strings.ContainsAny(body.LibraryKey, "/\\") || path.Clean(body.LibraryKey) != body.LibraryKey || strings.Trim(body.LibraryKey, ".") == "" {
			http.Error(w, "Invalid cached file", http.StatusBadRequest)
			return
		}
		if _, err := body.Ref.Hash(); err != nil {
			http.Error(w, "Invalid file reference", http.StatusBadRequest)
			return
		}
		if a.mediaFiles == nil {
			http.NotFound(w, r)
			return
		}
		freed, err := a.mediaFiles.EvictFile(body.LibraryKey, body.Ref)
		switch {
		case errors.Is(err, blobcache.ErrNotFound):
			http.NotFound(w, r)
		case errors.Is(err, blobcache.ErrFileInUse):
			http.Error(w, "File is in use", http.StatusConflict)
		case err != nil:
			http.Error(w, "Could not clear file", http.StatusInternalServerError)
		default:
			libJSON(w, http.StatusOK, map[string]int64{"freedBytes": freed})
		}
	})
	mux.HandleFunc("POST /clear", func(w http.ResponseWriter, r *http.Request) { a.clearReleaseCache(w, r, false) })
	mux.HandleFunc("POST /clear-block", func(w http.ResponseWriter, r *http.Request) { a.clearReleaseCache(w, r, true) })
	mux.HandleFunc("GET /releases/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.PathValue("id"))
		detail, ok := a.cache.ReleaseDetail(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(detail)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		used, budget := a.cache.Usage()
		releases := a.cache.Releases()
		preload := a.cachefill.Status()

		preloadKeys := make(map[string]core.RatingKey, len(preload.Items))
		for _, it := range preload.Items {
			if it.ReleaseID != "" && it.Key != 0 {
				preloadKeys[it.ReleaseID] = it.Key
			}
		}

		live := map[string]*core.PlaySession{}
		for _, session := range a.playback.Sessions() {
			if session.Resolution == nil || session.Resolution.Release.ID == "" {
				continue
			}
			live[session.Resolution.Release.ID] = session
		}

		for i := range releases {
			reason := releases[i].Reason
			if reason == nil {
				continue
			}
			id := releases[i].ReleaseID
			if session, ok := live[id]; ok {
				switch session.ViewerState {
				case "playing", "paused":
					reason.AddCause(core.CacheCause{Code: session.ViewerState, FirstSeen: session.StartedAt})
				}
				if reason.Origin == "" && session.Origin != "" {
					reason.Origin = session.Origin
				}
				if session.Resolution != nil {
					if reason.RatingKey == 0 && session.Resolution.Title != 0 {
						reason.RatingKey = session.Resolution.Title
					}
					if reason.Indexer == "" && session.Resolution.Release.Indexer != "" {
						reason.Indexer = session.Resolution.Release.Indexer
					}
					if reason.Source == "" && session.Resolution.Release.Name != "" {
						reason.Source = session.Resolution.Release.Name
					}
					if reason.Info == nil && hasReleaseInfo(session.Resolution.Release.Info) {
						info := session.Resolution.Release.Info
						reason.Info = &info
					}
				}
			}
			if reason.RatingKey == 0 {
				if key, ok := preloadKeys[id]; ok {
					reason.RatingKey = key
				}
			}
			if a.resolver != nil {
				if rel, titleKey, ok := a.resolver.LookupReleaseMeta(id); ok {
					if reason.RatingKey == 0 && titleKey != 0 {
						reason.RatingKey = titleKey
					}
					if reason.Indexer == "" && rel.Indexer != "" {
						reason.Indexer = rel.Indexer
					}
					if reason.Source == "" && rel.Name != "" {
						reason.Source = rel.Name
					}
					if reason.Origin == "" {
						reason.Origin = protocolOrigin(rel.Protocol)
					}
					if reason.Info == nil && hasReleaseInfo(rel.Info) {
						info := rel.Info
						reason.Info = &info
					}
				}
			}
		}
		protected := a.protectedReleases()
		var pinnedBytes int64
		for _, item := range releases {
			if len(item.Pins) > 0 || protected[item.ReleaseID] {
				pinnedBytes += item.Bytes
			}
		}
		if a.mediaFiles != nil {
			pinnedBytes += a.mediaFiles.ProtectedBytes()
		}
		var files []map[string]any
		if r.URL.Query().Get("files") == "1" && a.mediaFiles != nil {
			names := map[string]string{}
			if a.libraries != nil {
				libs, _ := a.libraries.List(r.Context())
				for _, lib := range libs {
					names[lib.Key] = lib.Name
				}
			}
			for _, f := range a.mediaFiles.Files() {
				library := names[f.LibraryKey]
				if library == "" {
					library = f.LibraryKey
				}
				name, _, _ := strings.Cut(f.Name, "?")
				name, _, _ = strings.Cut(name, "#")
				name = path.Base(strings.ReplaceAll(name, "\\", "/"))
				if name == "." || name == "/" || name == "" {
					name = string(f.Ref)
				}
				files = append(files, map[string]any{"libraryKey": f.LibraryKey, "library": library, "ref": f.Ref, "name": name, "size": f.Size, "lastOpen": f.LastOpen, "inUse": f.InUse})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"files": files, "policy": a.runningCacheConfig(), "shared": a.cache.Shared(), "clustered": a.clustered(), "storageError": a.cache.StorageError(), "restartRequired": a.cacheRestartRequired(), "used": used, "budget": budget, "directory": a.cache.Directory(), "protectedBytes": pinnedBytes, "releases": releases, "metrics": a.cache.Metrics(), "prefetch": a.prefetch.Stats(), "preload": preload})
	})
	mux.HandleFunc("POST /warm", func(w http.ResponseWriter, r *http.Request) {
		cfg := a.runningCacheConfig()
		if cfg.DisableSpeculation || cfg.Mode == "off" || !cfg.KeepInProgressWarm {
			http.Error(w, "Enable resume warming first", http.StatusConflict)
			return
		}
		a.prefetch.WarmNow()
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /pin", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ReleaseID string `json:"releaseId"`
			Pinned    bool   `json:"pinned"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if dec.Decode(&body) != nil || body.ReleaseID == "" {
			http.Error(w, "Invalid release", http.StatusBadRequest)
			return
		}
		found := false
		for _, item := range a.cache.Releases() {
			if item.ReleaseID == body.ReleaseID {
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "Release no longer cached", http.StatusNotFound)
			return
		}
		var err error
		if body.Pinned {
			err = a.cache.Pin(body.ReleaseID, "owner")
			if err == nil {
				a.cache.RecordProvenance(core.CacheProvenance{ReleaseID: body.ReleaseID, Cause: core.CacheCauseOwnerPin})
			}
		} else {
			err = a.cache.Unpin(body.ReleaseID, "owner")
		}
		if err != nil {
			http.Error(w, "Could not save pin", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /preload", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Paused bool `json:"paused"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body) != nil {
			http.Error(w, "Invalid request", 400)
			return
		}
		if err := a.cachefill.Pause(body.Paused); err != nil {
			http.Error(w, "Could not save preload pause", 500)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	return mux
}

// Cache learning defaults to the owner and active Plex Home members, never
// silently treating every remote friend as part of the household.
func (a *App) cacheHousehold(ctx context.Context, scope string) ([]core.User, error) {
	active, err := a.users.Users(ctx)
	if err != nil || scope == "all" {
		return active, err
	}
	all, err := a.users.AllUsers(ctx)
	if err != nil {
		return nil, err
	}
	home := map[int64]bool{}
	for _, u := range all {
		if u.Home || u.IsOwner {
			home[u.ID] = true
		}
	}
	out := make([]core.User, 0)
	for _, u := range active {
		if home[u.ID] {
			out = append(out, u)
		}
	}
	return out, nil
}

func (a *App) runningCacheConfig() config.Cache {
	if p := a.cacheConfig.Load(); p != nil {
		return *p
	}
	return a.cfg.Get().EffectiveCache()
}
func (a *App) cacheRestartRequired() bool {
	p := a.cfg.Get().EffectiveCache()
	return p.Dir != a.cache.Directory() || (p.Storage == "shared") != a.cache.Shared()
}

// clearReleaseCache is mounted only under the authenticated owner extension.
// It clears a release's cached bytes; with block it also rejects and blocks the
// release so it is never chosen for playback again.
func (a *App) clearReleaseCache(w http.ResponseWriter, r *http.Request, block bool) {
	var body struct {
		ReleaseID string `json:"releaseId"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || strings.TrimSpace(body.ReleaseID) == "" {
		http.Error(w, "Invalid release", 400)
		return
	}
	sessions := append(a.playback.Sessions(), a.playback.RemoteSessions(r.Context())...)
	for _, s := range sessions {
		if s.Resolution != nil && s.Resolution.Release.ID == body.ReleaseID {
			cacheConflict(w)
			return
		}
	}
	var found *segcache.ReleaseUsage
	for _, item := range a.cache.Releases() {
		if item.ReleaseID == body.ReleaseID {
			copy := item
			found = &copy
			break
		}
	}
	if found == nil {
		http.Error(w, "Release no longer cached", 404)
		return
	}
	for _, pin := range found.Pins {
		if pin == "playback:live" {
			cacheConflict(w)
			return
		}
	}
	if block {
		if err := a.playback.RejectReleaseID(r.Context(), body.ReleaseID); err != nil {
			http.Error(w, "Could not persist release rejection", 500)
			return
		}
		if _, err := a.resolver.Block(r.Context(), resolver.BlockRequest{ReleaseID: body.ReleaseID, Name: found.Title}, "Cleared and blocked from Cache by owner"); err != nil {
			http.Error(w, "Could not persist release block", 500)
			return
		}
	}
	// Owner pins can be explicitly discarded; background and playback pins must
	// retain their lifetime ownership, including pins on other nodes.
	err := a.cache.Unpin(body.ReleaseID, "owner")
	if err == nil {
		err = a.cache.ClearUnpinned(body.ReleaseID)
	}
	message := clearMessage(block, a.cache.Shared(), err == nil)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"blocked": block, "cleared": err == nil, "message": message})
}

func clearMessage(block, shared, cleared bool) string {
	switch {
	case block && !cleared:
		return "Release blocked for future playback. Cached bytes remain protected or could not be removed; stop background warming and retry clearing."
	case block && shared:
		return "Release blocked for future playback. Current shared cache bytes cleared; writes already running on another node may still finish."
	case block:
		return "Release blocked for future playback and cached bytes cleared on this cache."
	case !cleared:
		return "Cached bytes remain protected or could not be removed; stop background warming and retry clearing."
	case shared:
		return "Current shared cache bytes cleared; writes already running on another node may still finish. The release can still be played and cached again."
	default:
		return "Cached bytes cleared on this cache. The release can still be played and cached again."
	}
}

func cacheConflict(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "playback_active", "message": "Stop active or paused playback before clearing this release."}})
}
