package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/service"
	"github.com/braedonsaunders/ether/internal/updater"
	"github.com/braedonsaunders/ether/release-notes"
)

// updateState wires the self-update manager into the app lifecycle.
type updateState struct {
	boot updater.StartupResult
	mgr  *updater.Manager
}

func (a *App) updatesDir() string { return filepath.Join(a.opts.DataDir, "updates") }

// confirmUpdateStartup runs before anything a bad release could break
// (config, migrations): it counts starts of a just-installed version and
// restores the previous binary after a crash loop. A rollback returns an
// error wrapping service.ErrRestartRequested so the service manager starts
// the restored binary.
func (a *App) confirmUpdateStartup() error {
	a.updates = &updateState{}
	res, err := updater.ConfirmStartup(updater.StartupOptions{
		Dir:            a.updatesDir(),
		CurrentVersion: a.opts.Version,
		Logger:         a.log,
	})
	if err != nil {
		a.log.Warn("update startup check", "err", err)
	}
	a.updates.boot = res
	if res.RestartRequired {
		return fmt.Errorf("rolled back update %s: %w", res.RolledBack.Version, service.ErrRestartRequested)
	}
	return nil
}

// initUpdates builds the manager. Without a compiled-in release key the
// client cannot be built and checks stay disabled, but the manager still
// confirms a pending self-update.
func (a *App) initUpdates() {
	if a.updates == nil {
		a.updates = &updateState{}
	}
	exe, _ := os.Executable()
	if exe != "" {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
	}
	selfApply, notifyKind, why := updater.SelfApplyMode(updater.Environment{
		GOOS:        runtime.GOOS,
		Target:      exe,
		Dev:         updater.IsDev() || a.opts.Version == "dev",
		InContainer: service.InContainer(),
		Supervised:  !service.Interactive(),
	})
	log := a.log.With("component", "updater")
	installID := ""
	if a.cfg != nil {
		installID = a.cfg.Get().Server.MachineIdentifier
	}
	// A clustered node that installs its own updates takes its turn in a
	// rolling update (rolling.go); notify-only nodes just report.
	var coord updater.Coordinator
	if a.clustered() && selfApply {
		coord = a.newClusterUpdates()
	}
	a.updates.mgr = updater.NewManager(updater.ManagerOptions{
		Coordinator: coord,
		NodeName:    a.cluster.Self().Name,
		NewClient: func(u config.Updates) (updater.Checker, error) {
			return updater.NewClient(updater.Options{
				FeedURL:        u.FeedURL,
				Channel:        u.Channel,
				CurrentVersion: a.opts.Version,
				Logger:         log,
			})
		},
		Config:         func() config.Updates { return a.cfg.Get().Updates },
		CurrentVersion: a.opts.Version,
		Dir:            a.updatesDir(),
		Target:         exe,
		SelfApply:      selfApply,
		NotifyReason:   why,
		ActiveSessions: a.activeSessionCount,
		RequestRestart: a.requestRestart,
		Boot:           a.updates.boot,
		Logger:         log,
		NotifyKind:     notifyKind,
		// The server identity only seeds the local staged-rollout hash.
		InstallID:      installID,
		OnStatusChange: a.publishDashboard,
		OnAvailable: func(version string) error {
			channel := ""
			if a.cfg != nil {
				channel = a.cfg.Get().Updates.Channel
			}
			a.emitUpdateAvailable(a.opts.Version, version, channel, false)
			if a.notifications == nil {
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// Once per version across restarts and cluster nodes; an error
			// is retried at the next check.
			return a.notifications.UpdateAvailable(ctx, a.opts.Version, version)
		},
		// Once per version across restarts and cluster nodes, like OnAvailable.
		OnInstalled: func(u updater.InstalledUpdate) error {
			channel := ""
			auto := false
			if a.cfg != nil {
				uc := a.cfg.Get().Updates
				channel = uc.Channel
				auto = uc.AutoUpdate
			}
			a.emitUpdateInstalled(u.From, u.To, channel, auto)
			if a.notifications == nil {
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return a.notifications.UpdateInstalled(ctx, u.From, u.To)
		},
		OnRolledBack: func(f updater.FailedUpdate) error {
			a.emitUpdateRolledBack(f.Version, f.From, f.Reason)
			if a.notifications == nil {
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return a.notifications.UpdateRolledBack(ctx, f.Version, f.From, f.Reason)
		},
	})
	if a.cfg != nil {
		a.cfg.Subscribe(func(config.Config) {
			a.updates.mgr.Wake()
		})
	}
	if !a.updates.mgr.Enabled() {
		log.Debug("update checks disabled", "reason", a.updates.mgr.Status().DisabledReason)
	}
}

// activeSessionCount is everything an update or maintenance restart would
// interrupt under the shared liveness rule: actively playing local streams
// (excluding paused, starting/abandoned, idle, prepared-only, and stopped).
func (a *App) activeSessionCount() int {
	if a == nil {
		return 0
	}
	n := len((ownerPlayback{Playback: a.playback, app: a}).activeLocalSessions())
	// Standalone encoder-only specialist nodes have no local playback manager;
	// count only transcode sessions with recent media delivery.
	if a.playback == nil && a.brokerPlayback == nil && a.spikeLibrary == nil && a.transcoder != nil {
		now := time.Now()
		for _, hs := range a.transcoder.Sessions() {
			if la := hs.LastMediaAccess(); !la.IsZero() && now.Sub(la) <= core.PlayingEvidenceWindowHLS {
				n++
			}
		}
	}
	return n
}

// runUpdates drives the manager for the lifetime of Run; an installed
// update restarts the server through requestRestart.
func (a *App) runUpdates(ctx context.Context) {
	if a.updates == nil || a.updates.mgr == nil {
		return
	}
	a.updates.mgr.Run(ctx)
}

func (a *App) updateStatus(context.Context) any {
	return a.updateSnapshot()
}

func (a *App) updateSnapshot() updater.ManagerStatus {
	if a.updates == nil || a.updates.mgr == nil {
		return updater.ManagerStatus{Current: a.opts.Version, Capability: updater.CapabilityDisabled, State: updater.StateIdle}
	}
	return a.updates.mgr.Status()
}

type updateNotes struct {
	Version   string                   `json:"version"`
	Markdown  string                   `json:"markdown,omitempty"`
	NotesURL  string                   `json:"notesUrl,omitempty"`
	Unseen    bool                     `json:"unseen,omitempty"`
	Installed *updater.InstalledUpdate `json:"installed,omitempty"`
}

const releaseNotesClient = "maintenance"
const seenReleaseKey = "seenRelease"

func notesVersion(version string) string {
	return strings.TrimPrefix(version, "v")
}

func currentUpdateNotes(st updater.ManagerStatus) updateNotes {
	notes := updateNotes{Version: st.Current}
	if notesVersion(st.Current) == notesVersion(updater.Version) {
		markdown := releasenotes.ForVersion(updater.Version)
		if len(markdown) <= updater.MaxReleaseNotesBytes {
			notes.Markdown = markdown
		}
	}
	if st.Installed != nil && notesVersion(st.Installed.To) == notesVersion(st.Current) {
		notes.Installed = st.Installed
	}
	return notes
}

// updatesHandler serves /ether/api/updates/ (owner auth, LAN and CSRF are
// enforced by the admin handler):
//
//	GET  /                    status
//	GET  /notes?version=      Markdown for this build or the signed available release
//	POST /notes/dismiss      acknowledge the current build by version
//	POST /check               check the feed now
//	POST /apply               {"force": bool} — install as soon as idle (force: now)
//	POST /dismiss-installed   hide the "Updated to X" note
func (a *App) updatesHandler() http.Handler {
	mux := &route.Mux{}
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	writeError := func(w http.ResponseWriter, code int, kind, msg string) {
		writeJSON(w, code, map[string]any{"error": map[string]string{"code": kind, "message": msg}})
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.updateSnapshot())
	})
	prefs := identity.NewPreferences(a.db, "owner-console")
	ownerOnly := func(handler func(http.ResponseWriter, *http.Request, *core.Viewer)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			access, _ := admin.RequestAccess(r.Context())
			if !access.Owner || access.OwnerID <= 0 {
				writeError(w, http.StatusUnauthorized, "unauthorized", "sign in with an owner account")
				return
			}
			handler(w, r, &core.Viewer{ID: access.OwnerID, IsOwner: true})
		}
	}
	mux.HandleFunc("GET /notes", ownerOnly(func(w http.ResponseWriter, r *http.Request, owner *core.Viewer) {
		st := a.updateSnapshot()
		version := notesVersion(r.URL.Query().Get("version"))
		switch {
		case version == "" || version == notesVersion(st.Current):
			notes := currentUpdateNotes(st)
			if notes.Markdown != "" || notes.Installed != nil {
				raw, err := prefs.Get(r.Context(), owner, releaseNotesClient, seenReleaseKey)
				var seen string
				if errors.Is(err, frontend.ErrPreferencesNotFound) {
					err = nil
				} else if err == nil {
					err = json.Unmarshal(raw, &seen)
				}
				if err != nil {
					writeError(w, http.StatusInternalServerError, "notes_failed", "could not read release notes preference")
					return
				}
				notes.Unseen = notesVersion(seen) != notesVersion(st.Current)
			}
			writeJSON(w, http.StatusOK, notes)
		case st.Available != nil && version == notesVersion(st.Available.Version):
			writeJSON(w, http.StatusOK, updateNotes{
				Version: st.Available.Version, Markdown: st.Available.Notes, NotesURL: st.Available.NotesURL,
			})
		default:
			writeError(w, http.StatusNotFound, "release_not_found", "release notes not found for this version")
		}
	}))
	mux.HandleFunc("POST /notes/dismiss", ownerOnly(func(w http.ResponseWriter, r *http.Request, owner *core.Viewer) {
		var body struct {
			Version string `json:"version"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
		current := a.updateSnapshot().Current
		if body.Version == "" || notesVersion(body.Version) != notesVersion(current) {
			writeError(w, http.StatusConflict, "release_changed", "the running version changed")
			return
		}
		raw, _ := json.Marshal(notesVersion(current))
		if err := prefs.Put(r.Context(), owner, releaseNotesClient, seenReleaseKey, raw); err != nil {
			writeError(w, http.StatusInternalServerError, "dismiss_failed", "could not save release notes preference")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /check", func(w http.ResponseWriter, r *http.Request) {
		if a.updates == nil || a.updates.mgr == nil {
			writeError(w, http.StatusServiceUnavailable, "updates_disabled", updater.ErrUpdatesDisabled.Error())
			return
		}
		if err := a.updates.mgr.CheckNow(); err != nil {
			writeError(w, http.StatusServiceUnavailable, "updates_disabled", err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, a.updates.mgr.Status())
	})
	mux.HandleFunc("POST /dismiss-installed", func(w http.ResponseWriter, r *http.Request) {
		if a.updates == nil || a.updates.mgr == nil {
			writeError(w, http.StatusServiceUnavailable, "updates_disabled", updater.ErrUpdatesDisabled.Error())
			return
		}
		if err := a.updates.mgr.DismissInstalled(); err != nil {
			writeError(w, http.StatusInternalServerError, "dismiss_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, a.updates.mgr.Status())
	})
	mux.HandleFunc("POST /apply", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Force bool `json:"force"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
				return
			}
		}
		if a.updates == nil || a.updates.mgr == nil {
			writeError(w, http.StatusServiceUnavailable, "updates_disabled", updater.ErrUpdatesDisabled.Error())
			return
		}
		if err := a.updates.mgr.UpdateNow(body.Force); err != nil {
			code, kind := http.StatusConflict, "not_self_applicable"
			if errors.Is(err, updater.ErrUpdatesDisabled) {
				code, kind = http.StatusServiceUnavailable, "updates_disabled"
			}
			writeError(w, code, kind, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, a.updates.mgr.Status())
	})
	return mux
}
