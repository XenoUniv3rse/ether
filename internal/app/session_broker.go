package app

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/extras"
	"github.com/braedonsaunders/ether/internal/frontend"
	playlimits "github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

func (a *App) buildSessionEvents(context.Context, config.Config) error {
	a.sessionCoordination = sessions.NewCoordination()
	a.sessionEvents = sessions.NewEventBus(a.clusterNode(), a.users.ActiveUser)
	a.closers = append(a.closers, a.sessionEvents.Close)
	return nil
}

func (a *App) newSessionBroker(events *sessions.EventBus) (*sessions.Broker, error) {
	tc := a.activeTranscoder()
	var live sessions.LiveBackend
	if a.livetv != nil {
		live = sessionLive{service: a.livetv, app: a, cached: a.catalog.CachedGet}
	}
	visibility := experienceVisibility(a.safety, a.catalog.Get, nil, nil, serverLibraries(a.cfg.Read()))
	// Section IDs are stable even when Spike does not expose a TV library.
	// Missing a presented library must not exempt a known title from the
	// viewer's section restriction. Preserve the existing fallback for other
	// kinds, including virtual entries.
	visibility.SectionFor = brokerSectionFor(visibility.SectionFor)

	playback := core.Playback(a.playback)
	if a.brokerPlayback != nil {
		playback = a.brokerPlayback
	}
	if a.spikeLibrary != nil {
		// Spike is the library, not a side path. The broker has to see the
		// same sessions and direct-play fallback transcoder the adapter uses.
		playback = a.spikeLibrary.Playback()
		if tc == nil {
			tc = a.spikeLibrary.Transcoder()
		}
	}
	return sessions.NewBroker(sessions.BrokerDeps{SurfacePolicy: sessionPolicy,
		ExtraLookup: func(key core.RatingKey) (frontend.Extra, bool) {
			if a.extras == nil || !a.extras.Enabled() {
				return frontend.Extra{}, false
			}
			clip, ok := a.extras.Lookup(key)
			return frontendExtra(clip), ok
		},
		ExtraSource: func(ctx context.Context, viewer *core.Viewer, title *core.Title, key core.RatingKey, lang string) (sessions.PreviewSource, error) {
			if a.extras == nil {
				return sessions.PreviewSource{}, core.ErrNotAvailable
			}
			clip, ok := a.extras.Lookup(key)
			if !ok || clip.ParentKey != title.Key || !clip.IsTrailer() {
				return sessions.PreviewSource{}, core.ErrNotFound
			}
			return a.cachedTrailerSource(ctx, clip)
		},
		PreviewAvailable: func() bool {
			return a.extras != nil && a.extras.Enabled() && a.cfg.Read().Extras.TrailerCacheMaxBytes >= 0
		},
		PreviewSource: func(ctx context.Context, viewer *core.Viewer, title *core.Title, lang string) (sessions.PreviewSource, error) {
			if a.extras == nil {
				return sessions.PreviewSource{}, core.ErrNotAvailable
			}
			clips, err := a.extras.List(ctx, title, lang)
			if err != nil {
				return sessions.PreviewSource{}, err
			}
			var lastErr error
			for _, clip := range clips {
				if !clip.IsTrailer() {
					continue
				}
				src, err := a.cachedTrailerSource(ctx, clip)
				if err == nil || errors.Is(err, sessions.ErrPreviewPreparing) || ctx.Err() != nil {
					return src, err
				}
				lastErr = err
			}
			if lastErr != nil {
				return sessions.PreviewSource{}, lastErr
			}
			return sessions.PreviewSource{}, core.ErrNotFound
		},
		OnActualPlaying:     a.automaticSubtitleWork,
		DistributedHLS:      a.distributedHLS(),
		Trickplay:           a.trickplay.Borrow(),
		Coordination:        a.sessionCoordination,
		OnProfileDiagnostic: scorecardProfileDiagnostic(a),
		DB:                  a.db, Cluster: a.clusterNode(), Live: live,
		Logger:         a.log.With("component", "plex"),
		Downloads:      a.downloadRemuxer,
		DecisionTraces: a.resolver,
		Variants: func(ctx context.Context, v *core.Viewer, t *core.Title, caps core.ClientCaps) ([]core.ReleaseVariant, error) {
			if a.resolver == nil {
				return nil, core.ErrNotAvailable
			}
			// Handle authorization must see the same stable logical selectors
			// that detail pages issued. Concrete sources are checked when the
			// selected slot is resolved, never by reranking this listing.
			return scorecardVariants(core.WithLocalResponse(ctx), a, v, t, caps)
		},
		SelectScopedTracks: func(ctx context.Context, viewer *core.Viewer, device core.Device, title *core.Title, info core.MediaInfo, opts core.DecideOptions, audioSet, subtitleSet bool) (core.DecideOptions, error) {
			if a.trackprefs == nil {
				return opts, nil
			}
			scope := trackprefs.Scope{Device: device.ID}
			if title.ShowKey != 0 {
				scope.Series = strconv.FormatInt(int64(title.ShowKey), 10)
			} else if title.Key.Kind() == core.KindEpisode || title.Key.Kind() == core.KindSeason || title.Key.Kind() == core.KindShow {
				scope.Series = strconv.FormatInt(int64(core.ShowKey(title.Key.TMDB())), 10)
			}
			if section, ok := visibility.SectionFor(title.Type); ok {
				scope.Library = strconv.Itoa(section)
			}
			rules, _, err := a.trackprefs.ScopedRules(ctx, viewer.ID, scope)
			if err != nil {
				return opts, err
			}
			opts.MaxBitrateKbps = trackprefs.LimitBitrate(opts.MaxBitrateKbps, rules.MaxBitrateKbps)
			opts.MaxHeight = trackprefs.LimitBitrate(opts.MaxHeight, rules.MaxHeight)
			prefs := experience.TrackPreferences{Source: scopedTrackSource{Service: a.trackprefs, scope: scope}, GetTitle: a.catalog.Get}
			return selectBrokerTracks(ctx, prefs, viewer, title, info, opts, audioSet, subtitleSet, func(err error) { a.log.Warn("track preferences unavailable", "err", err) })
		},
		Lookup: func(ctx context.Context, v *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
			return a.admissionSessionTitle(ctx, v, key, visibility, false)
		},
		CurrentMediaLookup: func(ctx context.Context, v *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
			return a.librarySessionTitle(ctx, v, key, visibility, true, true)
		},
		KindSource:   a.kindSourceFunc(),
		KindVersions: a.pluginKindVersions,
		KindRememberedRelease: func(userID int64, key core.RatingKey) string {
			if a.resolver == nil {
				return ""
			}
			if vc, ok := a.resolver.ViewerChoiceFor(userID, key); ok && vc.Scope == "title" && vc.Release.ID != core.KindAutoVersionID {
				return vc.Release.ID
			}
			return ""
		},
		MediaLookup: func(ctx context.Context, v *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
			return a.admissionSessionTitle(ctx, v, key, visibility, true)
		},
		Playback: playback, Transcoder: tc, Progress: a.state, SourceHistory: a.state,
		ActiveViewer: func(ctx context.Context, id int64) (*core.Viewer, error) {
			v, err := a.users.ActiveUser(ctx, id)
			if err != nil || v == nil {
				return v, err
			}
			return a.safety.UserPolicy(v), nil
		},
		CanPlay: a.safety.CanPlay, FreshPlay: a.safety.ConfirmAdmission, CurrentPlay: a.safety.ConfirmPlayback, PreviewPlay: a.safety.PreviewPlayback, ReceiptCurrent: a.users.ReceiptCurrent, BitrateLimit: func(*core.Viewer) int { return 0 }, ViewerVideoTranscodingOff: a.safety.VideoTranscodingDisabled,
		ApplyRuleLimits: func(ctx context.Context, caps core.ClientCaps, viewer *core.Viewer, title *core.Title) core.ClientCaps {
			q := a.cfg.Read().Quality
			if title != nil {
				return resolver.ApplyClientRulesForTitle(q, caps, viewer, title, a.membershipFor(ctx, title))
			}
			return resolver.ApplyClientRulesForMedia(q, caps, viewer, core.MediaAny)
		},
		StreamLimit:         func(v *core.Viewer) int { return playlimits.UserStreamLimit(a.cfg.Read(), v) },
		RemoteUploadCap:     a.remoteUploadCap,
		RecordUploadCap:     a.recordUploadCap,
		VideoTranscodingOff: func() bool { return a.cfg.Read().Transcode.VideoTranscodingOff() },
		OnTimeline: func(v *core.Viewer, key core.RatingKey, pos, dur time.Duration, playState string) {
			if playState == "playing" && a.personalize != nil {
				a.personalize.NotePlayback(v, key)
			}
			if a.prefetch != nil {
				a.prefetch.OnTimeline(v, key, pos, dur, playState)
			}
		},
		Events:           events,
		DomainEvents:     a.eventBus,
		OpenSubtitle:     a.subtitles.OpenFormat,
		OpenSubtitleFrom: a.subtitles.OpenFormatFrom,
		ExtractSubtitle: func(ctx context.Context, input string, index int, codec string, w io.Writer) error {
			if a.transcoder == nil {
				return core.ErrNotAvailable
			}
			return a.transcoder.ExtractSubtitle(ctx, input, index, codec, w)
		},
		OnEventError: func(err error) { a.log.Warn("session lifecycle operation failed", "err", err) },
	})
}

// brokerSectionFor uses the canonical authorization IDs for known catalog
// types, even when a frontend does not expose the corresponding library.
func brokerSectionFor(other func(core.MediaType) (int, bool)) func(core.MediaType) (int, bool) {
	return func(kind core.MediaType) (int, bool) {
		switch kind {
		case core.TypeMovie:
			return 1, true
		case core.TypeShow, core.TypeSeason, core.TypeEpisode:
			return 2, true
		default:
			return other(kind)
		}
	}
}

// sessionTitle resolves a playable title. Spike-library keys live outside
// the TMDB catalog; both the live and cached lookups have to find them or
// broker publish reports the title missing.
func (a *App) sessionTitle(ctx context.Context, viewer *core.Viewer, key core.RatingKey, visibility experience.Visibility, cached bool) (*core.Title, error) {
	if a.spikeLibrary != nil && key.Kind() == core.KindLocal {
		title, err := a.spikeLibrary.Catalog().Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if title == nil || title.Key != key || !visibility.Playable(ctx, viewer, title) {
			return nil, core.ErrNotFound
		}
		return title, nil
	}
	if cached {
		return cachedSessionTitle(ctx, viewer, key, a.catalog.CachedGet, visibility)
	}
	title, err := a.catalog.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if title == nil || !visibility.Playable(ctx, viewer, title) {
		return nil, core.ErrNotFound
	}
	return title, nil
}

// cachedSessionTitle checks both the item and inherited show using only the
// local mirror. Missing facts fail closed; it never refreshes remote metadata.
func cachedSessionTitle(ctx context.Context, viewer *core.Viewer, key core.RatingKey, cached func(context.Context, core.RatingKey) (*core.Title, error), visibility experience.Visibility) (*core.Title, error) {
	title, err := cached(ctx, key)
	if err != nil {
		return nil, err
	}
	visibility.GetTitle = cached
	if title == nil || title.Key != key || !visibility.Playable(ctx, viewer, title) {
		return nil, core.ErrNotFound
	}
	return title, nil
}

// subscribeSessionState publishes protocol-neutral committed state invalidations.
func subscribeSessionState(watch *state.State, events *sessions.EventBus, onError func(error)) func() {
	return watch.Subscribe(func(ev state.Event) {
		if ev.Kind == state.EventProgress && ev.Completed {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := events.Publish(ctx, frontend.Event{Type: "UserDataChanged", ViewerID: ev.UserID, Item: ev.Key, OriginProtocol: ev.Origin.Surface, OriginSessionID: ev.Origin.SessionID, OriginClientID: ev.Origin.ClientID}); err != nil && onError != nil {
			onError(err)
		}
		if ev.Kind == state.EventScrobble || ev.Kind == state.EventUnscrobble || ev.Kind == state.EventCompleted {
			if err := events.Publish(ctx, frontend.Event{Type: "WatchStateChanged", ViewerID: ev.UserID, Item: ev.Key, OriginProtocol: ev.Origin.Surface, OriginSessionID: ev.Origin.SessionID, OriginClientID: ev.Origin.ClientID}); err != nil && onError != nil {
				onError(err)
			}
		}
	})
}

// Both full playback and focus previews resolve and retain exactly these bytes.
func (a *App) cachedTrailerSource(ctx context.Context, clip extras.Extra) (sessions.PreviewSource, error) {
	cached, err := a.extras.CacheTrailer(ctx, clip)
	if errors.Is(err, extras.ErrCachePreparing) {
		return sessions.PreviewSource{}, sessions.ErrPreviewPreparing
	}
	if err != nil {
		return sessions.PreviewSource{}, err
	}
	return sessions.PreviewSource{MIMEType: cached.MIMEType, SourceGeneration: cached.SourceGeneration, Open: func() (*os.File, error) { return a.extras.OpenCachedTrailer(cached) }}, nil
}

// admissionSessionTitle separates current publication from retained playback.
// Both full planning and lazy issuance pass here. Only local library and
// catalogue facts are used for membership, including inherited show membership.
func (a *App) admissionSessionTitle(ctx context.Context, v *core.Viewer, key core.RatingKey, visibility experience.Visibility, cached bool) (*core.Title, error) {
	return a.librarySessionTitle(ctx, v, key, visibility, cached, false)
}

func (a *App) librarySessionTitle(ctx context.Context, v *core.Viewer, key core.RatingKey, visibility experience.Visibility, cached, retained bool) (*core.Title, error) {
	// Plugin library items are not catalogue titles: check them against the
	// plugin library the viewer may open, as their source resolve does.
	if key.IsPluginKey() {
		if a.pluginsRT == nil || a.pluginsRT.kindCatalog == nil {
			return nil, core.ErrNotFound
		}
		return a.pluginPlayableTitle(ctx, a.pluginsRT.kindCatalog, v, key)
	}
	if a.libraries == nil || (a.spikeLibrary != nil && key.Kind() == core.KindLocal) {
		return a.sessionTitle(ctx, v, key, visibility, cached)
	}
	visibility.SectionFor = nil
	if a.safety != nil {
		visibility.Policy = sessionSafetyPolicy{a}
	}
	title, err := a.sessionTitle(ctx, v, key, visibility, cached)
	if err != nil {
		return nil, err
	}
	var libs []core.Library
	if retained {
		libs, err = a.libraries.ListRetained(ctx)
	} else {
		libs, err = a.libraries.ListCurrent(ctx)
	}
	if err != nil {
		return nil, err
	}
	for _, lib := range libs {
		if lib.Hidden || lib.Source == core.LibraryPlugin || lib.RulesBroken || !frontend.SectionAllowed(v, int(lib.ID)) {
			continue
		}
		if lib.MediaType != "" && string(lib.MediaType) != titleMedia(title) {
			continue
		}
		published := false
		for protocol := range libraryPresentations {
			if app := core.LiveApp(ctx); app != "" && app != string(protocol) {
				continue
			}
			if _, _, ok := presentLibraryForProtocol(lib.Present, protocol, lib.MediaType); ok {
				published = true
				break
			}
		}
		if !published {
			continue
		}
		ok, err := a.catalog.TitleInFilter(core.WithCatalogViewer(ctx, v), libraryAccessFilter(lib), libraryProfileKey(title))
		if err != nil {
			return nil, err
		}
		if ok {
			return title, nil
		}
	}
	return nil, core.ErrNotFound
}

// Library publication is evaluated separately from mandatory viewer safety.
type sessionSafetyPolicy struct{ app *App }

func (p sessionSafetyPolicy) Allowed(v *core.User, t *core.Title) bool {
	return p.app.safety.PolicyAllowed(v, t)
}
