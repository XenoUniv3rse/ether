package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"github.com/braedonsaunders/ether/internal/libraries"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/plugins"
)

// Owner console API for Libraries (spec §11, slice H). Mounted as the
// "libraries" admin extension, so every route sits behind the owner
// API's LAN, token and CSRF boundaries like the cache extension.
// Shares the long-lived pluginitems/blobcache services built by
// buildPlugins when present, falling back to lazy open in unit tests
// that construct App without pluginsRT.

type librariesConsole struct {
	app *App

	once  sync.Once
	items *pluginitems.Service
	blobs *blobcache.Cache
	err   error

	liveMu       sync.Mutex
	liveSessions map[string]consoleLiveSession
}

// libraryHandlers registers the /api/libraries and /api/plugins-setup
// extensions (called from extensionHandlers).
func (a *App) libraryHandlers(m map[string]http.Handler) {
	c := &librariesConsole{app: a}
	m["libraries"] = c.handler()
	m["plugins-setup"] = c.setupHandler()
}

func (c *librariesConsole) services() (*pluginitems.Service, *blobcache.Cache, error) {
	if c.app.pluginsRT != nil && c.app.pluginsRT.pluginitems != nil && c.app.pluginsRT.blobcache != nil {
		return c.app.pluginsRT.pluginitems, c.app.pluginsRT.blobcache, nil
	}
	c.once.Do(func() {
		ctx := context.Background()
		opts := pluginitems.Options{Logger: c.app.log}
		if c.app.pluginsRT != nil && c.app.pluginsRT.mgr != nil {
			opts.Caller = c.app.pluginsRT.mgr
			opts.Health = c.app.pluginsRT.mgr
			opts.Jobs = pluginSyncJobTracker{mgr: c.app.pluginsRT.mgr}
			opts.MaxPluginItemsFor = c.app.pluginsRT.mgr.MaxPluginItems
		}
		if c.items, c.err = pluginitems.New(ctx, c.app.db, opts); c.err != nil {
			return
		}
		c.blobs, c.err = blobcache.Open(filepath.Join(c.app.opts.DataDir, "blobcache"),
			blobcache.Options{Logger: c.app.log, DiskUsage: c.app.opts.diskUsage})
	})
	return c.items, c.blobs, c.err
}

func libJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func libError(w http.ResponseWriter, status int, msg string) {
	libJSON(w, status, map[string]string{"error": msg})
}

func libErrStatus(err error) int {
	switch {
	case errors.Is(err, core.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, libraries.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, libraries.ErrBuiltinLibrary):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func decodeLibBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		libError(w, http.StatusBadRequest, "Invalid request body")
		return false
	}
	return true
}

// kindItemDTO puts Ref and Key back on the wire: core.KindItem hides
// both from plugins (json:"-"), but the owner console needs them.
type kindItemDTO struct {
	core.KindItem
	Ref string `json:"ref"`
	Key int64  `json:"key"`
}

func kindDTOs(items []core.KindItem) []kindItemDTO {
	out := make([]kindItemDTO, len(items))
	for i, it := range items {
		out[i] = kindItemDTO{KindItem: it, Ref: string(it.Ref), Key: int64(it.Key)}
	}
	return out
}

type libraryDTO struct {
	core.Library
	EffectiveTabs  []core.LibraryTab           `json:"effectiveTabs,omitempty"`
	Presentations  *core.ResolvedPresentations `json:"presentations,omitempty"`
	ItemCount      *int                        `json:"itemCount,omitempty"`
	StoredItems    *int                        `json:"storedItems,omitempty"`
	PluginItems    *int                        `json:"pluginItems,omitempty"`
	MaxPluginItems *int                        `json:"maxPluginItems,omitempty"`
	Sync           *pluginitems.SyncState      `json:"sync,omitempty"`
	Kind           *plugins.MediaKindMeta      `json:"kind,omitempty"`
	PluginName     string                      `json:"pluginName,omitempty"`
}

func consoleKindPresentationInput(decl plugins.MediaKindMeta) libraries.KindPresentationInput {
	levels := make([]libraries.KindPresentationLevel, len(decl.Levels))
	for i, lv := range decl.Levels {
		levels[i] = libraries.KindPresentationLevel{
			Key:       lv.Key,
			Parent:    lv.Parent,
			Media:     lv.Media,
			Role:      lv.Role,
			Playable:  lv.Playable,
			Container: lv.Container,
			Members:   lv.Members,
		}
	}
	return libraries.KindPresentationInput{
		Media:   decl.Media,
		Levels:  levels,
		Present: decl.Present,
	}
}

func (c *librariesConsole) resolveLibraryPresentations(ctx context.Context, lib core.Library, kind *plugins.MediaKindMeta) core.ResolvedPresentations {
	if lib.Source == core.LibraryPlugin {
		if kind == nil {
			if items, _, err := c.services(); err == nil && items != nil {
				if rec, err := items.GetKindByLibrary(ctx, lib.ID); err == nil {
					decl := rec.Decl
					kind = &decl
				}
			}
		}
		if kind != nil {
			return libraries.ResolveKindPresentations(consoleKindPresentationInput(*kind), lib.PresentOverrides)
		}
		return libraries.ResolveKindPresentations(libraries.KindPresentationInput{
			Present: lib.Present,
		}, lib.PresentOverrides)
	}
	return libraries.ResolveCataloguePresentations(lib.MediaType, lib.Present, lib.PresentOverrides, lib.Source == core.LibraryBuiltin)
}

func (c *librariesConsole) decorate(ctx context.Context, lib core.Library) libraryDTO {
	dto := libraryDTO{
		Library:       lib,
		EffectiveTabs: c.effectiveTabs(ctx, lib),
	}
	switch lib.Source {
	case core.LibraryPlugin:
		if items, _, err := c.services(); err == nil && items != nil {
			maxItems := items.MaxItemsFor(lib.PluginID)
			dto.MaxPluginItems = &maxItems
			if total, err := items.PluginItemCount(ctx, lib.PluginID); err == nil {
				dto.PluginItems = &total
			}
			if rec, err := items.GetKindByLibrary(ctx, lib.ID); err == nil {
				decl := rec.Decl
				decl.Label = c.catalogueLabel(ctx, lib.PluginID, decl.Label, lib.Name)
				decl.Library.Name = c.catalogueLabel(ctx, lib.PluginID, decl.Library.Name, lib.Name)
				dto.Kind = &decl
				if tally, err := items.LibraryTally(ctx, lib.ID); err == nil {
					stored := 0
					for _, v := range tally {
						stored += v
					}
					dto.StoredItems = &stored
					n := stored
					if len(decl.Levels) > 0 {
						n = tally[decl.Levels[0].Key]
					}
					dto.ItemCount = &n
				}
				if st, err := items.SyncStateOf(ctx, lib.PluginID, lib.KindKey); err == nil {
					dto.Sync = &st
				}
			}
		}
		if c.app.pluginsRT != nil && c.app.pluginsRT.mgr != nil {
			if rec, err := c.app.pluginsRT.mgr.Get(ctx, lib.PluginID); err == nil {
				dto.PluginName = rec.Name
			}
		}
	case core.LibraryBuiltin:
		if core.HasLibraryScopedRules(lib.Rules) && c.app.collections != nil {
			if n, pending, err := c.app.collections.ConsoleCount(ctx, libraryScopeFilter(lib, core.Filter{})); err != nil {
				if c.app.log != nil {
					c.app.log.Warn("libraries: library could not be counted", "id", int64(lib.ID), "source", lib.Source, "err", err)
				}
			} else if !pending || n > 0 {
				dto.ItemCount = &n
			}
		} else if c.app.catalog != nil {
			if st, err := c.app.catalog.Stats(ctx); err == nil {
				n := st.BrowsableMovies
				if lib.MediaType == core.TypeShow {
					n = st.BrowsableShows
				}
				dto.ItemCount = &n
			}
		}
	case core.LibraryCatalog, core.LibraryCollection:
		// The listing never counts a library-sized scope on the request: the
		// cached count refreshes in the background, and the count is omitted
		// until the first one completes. A deleted collection counts as empty.
		if c.app.collections == nil {
			break
		}
		if n, pending, err := c.app.collections.ConsoleCount(ctx, libraryScopeFilter(lib, core.Filter{})); err != nil {
			if c.app.log != nil {
				c.app.log.Warn("libraries: library could not be counted", "id", int64(lib.ID), "source", lib.Source, "err", err)
			}
		} else if !pending || n > 0 {
			dto.ItemCount = &n
		}
	}
	resolved := c.resolveLibraryPresentations(ctx, lib, dto.Kind)
	dto.Presentations = &resolved
	return dto
}

// libraryScopeFilter applies sources and visible rules to the request. Draft
// previews use the same source pool without applying the saved rules twice.
func libraryScopeFilter(lib core.Library, req core.Filter) core.Filter {
	f := req
	f.LocalOnly = true
	f.CatalogueSources = lib.Sources
	if lib.MediaType != "" {
		f.Type = lib.MediaType
	}
	if lib.Source == core.LibraryBuiltin || lib.Source == core.LibraryCatalog || lib.Source == core.LibraryCollection {
		if lib.Source == core.LibraryCollection && lib.CollectionID > 0 {
			// Exactly the collection's titles: a SQL semi-join, so the
			// page reads one statement whatever the collection's size.
			f.CollectionMembers = lib.CollectionID
			f.CatalogueSources = nil
		}
		if core.IsBuiltinLibrary(lib.ID) {
			if f.RuleVisibility == "" {
				if extra := core.BuiltinLibraryExtraRules(lib.Rules); len(extra) > 0 {
					f.ExtraRules = extra
				}
			}
		} else if f.RuleVisibility != "" {
			f.ApplyCanonicalInPreview = true
		} else if len(lib.Rules) > 0 || lib.VisibilityDefault != "" {
			f.ExtraRules = lib.Rules
			f.ExtraFallback = lib.VisibilityDefault
		}
	}
	return f
}

// libraryAccessFilter builds the membership filter for direct access, server-wide
// search, and playback. Unlike libraryScopeFilter, it omits a built-in library's
// library-only hide rules so titles hidden from a built-in library remain
// reachable everywhere else.
func libraryAccessFilter(lib core.Library) core.Filter {
	f := libraryScopeFilter(lib, core.Filter{})
	if core.IsBuiltinLibrary(lib.ID) {
		f.ExtraRules = nil
		f.ExtraFallback = ""
	}
	return f
}

// libraryRecommendationMembers implements personalize.Options.LibraryMembers:
// catalogue, collection, and library-scoped built-in libraries scope their
// section rows to their own titles, checked with the same indexed membership
// SQL as the All browse. Unscoped built-in and plugin libraries stay unscoped.
// The revision is the library's UpdatedAt, so editing its sources, rules or
// collection invalidates cached section assemblies.
func (a *App) libraryRecommendationMembers(ctx context.Context, id core.LibraryID, keys []core.RatingKey) (map[core.RatingKey]bool, int64, bool, error) {
	if a.libraries == nil {
		return nil, 0, false, nil
	}
	lib, err := a.libraries.Get(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	if lib.Source != core.LibraryBuiltin && lib.Source != core.LibraryCatalog && lib.Source != core.LibraryCollection {
		return nil, 0, false, nil
	}
	if core.IsBuiltinLibrary(lib.ID) && !core.HasLibraryScopedRules(lib.Rules) {
		return nil, 0, false, nil
	}
	queryKeys := keys
	var remapped bool
	for _, k := range keys {
		if k.Kind() == core.KindSeason || k.Kind() == core.KindEpisode {
			remapped = true
			break
		}
	}
	if remapped {
		seen := make(map[core.RatingKey]struct{}, len(keys))
		queryKeys = make([]core.RatingKey, 0, len(keys))
		for _, k := range keys {
			pk := k
			if k.Kind() == core.KindSeason || k.Kind() == core.KindEpisode {
				pk = core.ShowKey(k.TMDB())
			}
			if _, ok := seen[pk]; !ok {
				seen[pk] = struct{}{}
				queryKeys = append(queryKeys, pk)
			}
		}
	}
	members, err := a.catalog.TitlesInFilter(ctx, libraryScopeFilter(lib, core.Filter{}), queryKeys)
	if err != nil {
		return nil, 0, false, err
	}
	if remapped {
		out := make(map[core.RatingKey]bool, len(keys))
		for _, k := range keys {
			pk := k
			if k.Kind() == core.KindSeason || k.Kind() == core.KindEpisode {
				pk = core.ShowKey(k.TMDB())
			}
			if members[pk] {
				out[k] = true
			}
		}
		members = out
	}
	return members, lib.UpdatedAt.UnixNano(), true, nil
}

func (c *librariesConsole) browseTitles(ctx context.Context, lib core.Library, req core.Filter, q core.BrowseQuery) (core.Page, error) {
	q.Filter = libraryScopeFilter(lib, req)
	if q.Sort == "" {
		q.Sort = core.SortPopularity
		q.Desc = true
	}
	if q.Limit <= 0 {
		q.Limit = 48
	}
	return c.app.collections.BrowseTitles(ctx, q)
}

func (c *librariesConsole) library(w http.ResponseWriter, r *http.Request) (core.Library, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		libError(w, http.StatusBadRequest, "Invalid library id")
		return core.Library{}, false
	}
	lib, err := c.app.libraries.Get(r.Context(), core.LibraryID(id))
	if err != nil {
		libError(w, libErrStatus(err), "Library not found")
		return core.Library{}, false
	}
	return lib, true
}

func (c *librariesConsole) handler() http.Handler {
	mux := &route.Mux{}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		libs, err := c.app.libraries.List(r.Context())
		if err != nil {
			libError(w, libErrStatus(err), "Could not list libraries")
			return
		}
		out := make([]libraryDTO, len(libs))
		for i, lib := range libs {
			out[i] = c.decorate(r.Context(), lib)
		}
		libJSON(w, http.StatusOK, map[string]any{"libraries": out})
	})
	mux.HandleFunc("POST /preview", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Sources           []string                `json:"sources"`
			MediaType         core.MediaType          `json:"mediaType"`
			Rules             []core.LibraryExclusion `json:"rules"`
			VisibilityDefault string                  `json:"visibilityDefault"`
		}
		if !decodeLibBody(w, r, &body) {
			return
		}
		lib := core.Library{Source: core.LibraryCatalog, MediaType: body.MediaType, Sources: body.Sources}
		if err := c.app.validateCatalogueSources(r.Context(), lib, body.Sources); err != nil {
			libError(w, 400, err.Error())
			return
		}
		if _, err := libraryfilter.CompilePolicy(body.Rules, body.VisibilityDefault); err != nil {
			libError(w, 400, err.Error())
			return
		}
		sf := libraryScopeFilter(lib, core.Filter{RuleVisibility: "all"})
		result, err := c.app.catalog.PreviewVisibilityScoped(r.Context(), body.Rules, body.VisibilityDefault, &sf)
		if err != nil {
			libError(w, 500, "Could not preview catalogue")
			return
		}
		libJSON(w, 200, result)
	})
	mux.HandleFunc("POST /{$}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Sources           []string                `json:"sources"`
			MetadataSource    *string                 `json:"metadataSource"`
			Name              string                  `json:"name"`
			Rules             []core.LibraryExclusion `json:"rules"`
			VisibilityDefault string                  `json:"visibilityDefault"`
			// A collection library instead of a filter library: the id of
			// the owner collection whose titles it shows, and the type it
			// is browsed as.
			CollectionID int64               `json:"collectionId"`
			MediaType    core.MediaType      `json:"mediaType"`
			CardShape    *string             `json:"cardShape"`
			Present      *core.Presentations `json:"present"`
			Tabs         *[]core.LibraryTab  `json:"tabs"`
		}
		if !decodeLibBody(w, r, &body) {
			return
		}
		// Console-only by default (§15.3): the owner opts in per
		// protocol afterwards from the library's Settings tab.
		var present core.Presentations
		if body.Present != nil {
			present = *body.Present
		}
		media := core.MediaType(core.NormalizeRuleMediaType(string(body.MediaType)))
		var source string
		if body.MetadataSource == nil {
			source = string(c.app.defaultMetadataSource(media))
		} else {
			source = *body.MetadataSource
			if !c.app.validLibraryMetadataSource(media, source) {
				libJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_metadata_source"})
				return
			}
		}
		option := libraries.WithMetadataSource(source)
		var lib core.Library
		var err error
		if body.CollectionID > 0 {
			lib, err = c.app.libraries.CreateCollection(r.Context(), body.Name, body.CollectionID, body.MediaType, present, option)
		} else {
			lib, err = c.app.libraries.Create(r.Context(), body.Name, body.MediaType, body.Rules, body.VisibilityDefault, present, option)
		}
		if err != nil {
			libError(w, libErrStatus(err), "Could not create the library")
			return
		}
		if len(body.Sources) > 0 && lib.Source != core.LibraryCollection {
			if err = c.app.validateCatalogueSources(r.Context(), lib, body.Sources); err != nil {
				_ = c.app.libraries.Delete(r.Context(), lib.ID)
				libError(w, 400, err.Error())
				return
			}
			if err = c.app.libraries.SetSources(r.Context(), lib.ID, body.Sources); err != nil {
				libError(w, 500, "Could not save sources")
				return
			}
			lib, _ = c.app.libraries.Get(r.Context(), lib.ID)
		}
		if body.CardShape != nil {
			if err = c.app.libraries.SetCardShape(r.Context(), lib.ID, *body.CardShape); err != nil {
				_ = c.app.libraries.Delete(r.Context(), lib.ID)
				libError(w, libErrStatus(err), "Could not set card shape")
				return
			}
			lib, _ = c.app.libraries.Get(r.Context(), lib.ID)
		}
		if body.Tabs != nil && len(*body.Tabs) > 0 {
			if _, err = c.app.libraries.SetTabs(r.Context(), lib.ID, *body.Tabs, nil); err != nil {
				_ = c.app.libraries.Delete(r.Context(), lib.ID)
				libError(w, libErrStatus(err), err.Error())
				return
			}
			lib, _ = c.app.libraries.Get(r.Context(), lib.ID)
		}
		c.app.publishLibrarySections()
		libJSON(w, http.StatusCreated, c.decorate(r.Context(), lib))
	})
	mux.HandleFunc("GET /{id}", func(w http.ResponseWriter, r *http.Request) {
		lib, ok := c.library(w, r)
		if !ok {
			return
		}
		libJSON(w, http.StatusOK, c.decorate(r.Context(), lib))
	})
	mux.HandleFunc("PATCH /{id}", func(w http.ResponseWriter, r *http.Request) {
		lib, ok := c.library(w, r)
		if !ok {
			return
		}
		var body struct {
			CollectionID      *int64                    `json:"collectionId"`
			Sources           *[]string                 `json:"sources"`
			MetadataSource    *string                   `json:"metadataSource"`
			Name              *string                   `json:"name"`
			Hidden            *bool                     `json:"hidden"`
			Sort              *int                      `json:"sort"`
			CardShape         *string                   `json:"cardShape"`
			Present           *core.Presentations       `json:"present"`
			PresentApps       *libraries.AppSwitchPatch `json:"presentApps"`
			Rules             *[]core.LibraryExclusion  `json:"rules"`
			VisibilityDefault *string                   `json:"visibilityDefault"`
			QualityProfile    *string                   `json:"qualityProfile"`
			Tabs              *[]core.LibraryTab        `json:"tabs"`
		}
		if !decodeLibBody(w, r, &body) {
			return
		}
		if body.MetadataSource != nil && (lib.Source == core.LibraryPlugin ||
			!c.app.validLibraryMetadataSource(lib.MediaType, *body.MetadataSource)) {
			libJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_metadata_source"})
			return
		}
		ctx := r.Context()
		svc := c.app.libraries
		sectionsChanged := false
		if body.CollectionID != nil {
			if err := svc.SetCollection(ctx, lib.ID, *body.CollectionID); err != nil {
				libError(w, libErrStatus(err), "Could not update collection")
				return
			}
		}
		if body.Sources != nil {
			if err := c.app.validateCatalogueSources(ctx, lib, *body.Sources); err != nil {
				libError(w, 400, err.Error())
				return
			}
			if err := svc.SetSources(ctx, lib.ID, *body.Sources); err != nil {
				libError(w, 500, "Could not save sources")
				return
			}
		}
		if body.Name != nil {
			if err := svc.Rename(ctx, lib.ID, *body.Name); err != nil {
				libError(w, libErrStatus(err), "Could not rename the library")
				return
			}
			sectionsChanged = true
		}
		if body.Hidden != nil {
			if err := svc.SetHidden(ctx, lib.ID, *body.Hidden); err != nil {
				libError(w, libErrStatus(err), "Could not update the library")
				return
			}
			sectionsChanged = true
		}
		if body.Sort != nil {
			if err := svc.SetSort(ctx, lib.ID, *body.Sort); err != nil {
				libError(w, libErrStatus(err), "Could not reorder the library")
				return
			}
			sectionsChanged = true
		}
		if body.CardShape != nil {
			if err := svc.SetCardShape(ctx, lib.ID, *body.CardShape); err != nil {
				libError(w, libErrStatus(err), "Could not update card shape")
				return
			}
		}
		if body.PresentApps != nil {
			resolved := c.resolveLibraryPresentations(ctx, lib, nil)
			eff, ov, err := libraries.ApplyAppSwitches(resolved, lib.PresentOverrides, *body.PresentApps)
			if err != nil {
				libError(w, libErrStatus(err), strings.TrimPrefix(err.Error(), libraries.ErrInvalid.Error()+": "))
				return
			}
			if err := svc.SetPresentAndOverrides(ctx, lib.ID, eff, ov); err != nil {
				libError(w, libErrStatus(err), "Could not update where the library appears")
				return
			}
			sectionsChanged = true
		} else if body.Present != nil {
			if err := libraries.ValidatePresentations(*body.Present); err != nil {
				libError(w, libErrStatus(err), "Could not update where the library appears")
				return
			}
			resolved := c.resolveLibraryPresentations(ctx, lib, nil)
			eff, ov, err := libraries.OverridesFromLegacyPresent(resolved, lib.PresentOverrides, *body.Present)
			if err != nil {
				libError(w, libErrStatus(err), strings.TrimPrefix(err.Error(), libraries.ErrInvalid.Error()+": "))
				return
			}
			if lib.Source != core.LibraryPlugin {
				eff = *body.Present
			}
			if err := svc.SetPresentAndOverrides(ctx, lib.ID, eff, ov); err != nil {
				libError(w, libErrStatus(err), "Could not update where the library appears")
				return
			}
			sectionsChanged = true
		}
		if body.QualityProfile != nil {
			name := strings.TrimSpace(*body.QualityProfile)
			if name != "" {
				_, valid := core.FindProfile(c.app.cfg.Read().Quality.Profiles, name)
				if lib.Source == core.LibraryPlugin || !valid {
					libError(w, http.StatusBadRequest, "Could not update the quality profile")
					return
				}
			}

			if err := svc.SetQualityProfile(ctx, lib.ID, *body.QualityProfile); err != nil {
				libError(w, libErrStatus(err), "Could not update the quality profile")
				return
			}
		}
		if body.Rules != nil || body.VisibilityDefault != nil {
			rules := lib.Rules
			if body.Rules != nil {
				rules = *body.Rules
			}
			visDefault := lib.VisibilityDefault
			if body.VisibilityDefault != nil {
				visDefault = *body.VisibilityDefault
			}
			if err := svc.SetRules(ctx, lib.ID, rules, visDefault); err != nil {
				libError(w, libErrStatus(err), "Could not save visibility rules")
				return
			}
			if lib.ID == core.LibraryMovies || lib.ID == core.LibraryShows {
				if err := c.app.refreshLibraryPolicies(ctx); err != nil {
					libError(w, http.StatusBadRequest, err.Error())
					return
				}
			}
		}
		if body.Tabs != nil {
			if len(*body.Tabs) == 0 {
				if err := svc.ResetTabs(ctx, lib.ID); err != nil {
					libError(w, libErrStatus(err), "Could not reset library tabs")
					return
				}
			} else {
				schema := c.pluginTabSchema(ctx, lib)
				if _, err := svc.SetTabs(ctx, lib.ID, *body.Tabs, schema); err != nil {
					libError(w, libErrStatus(err), err.Error())
					return
				}
			}
		}
		if body.MetadataSource != nil {
			if err := svc.SetMetadataSource(ctx, lib.ID, *body.MetadataSource); err != nil {
				libJSON(w, libErrStatus(err), map[string]string{"code": "metadata_source_write_failed"})
				return
			}
		}
		if sectionsChanged {
			c.app.publishLibrarySections()
		}
		fresh, err := svc.Get(ctx, lib.ID)
		if err != nil {
			libError(w, libErrStatus(err), "Library not found")
			return
		}
		libJSON(w, http.StatusOK, c.decorate(ctx, fresh))
	})
	mux.HandleFunc("DELETE /{id}", func(w http.ResponseWriter, r *http.Request) {
		lib, ok := c.library(w, r)
		if !ok {
			return
		}
		if err := c.app.libraries.Delete(r.Context(), lib.ID); err != nil {
			libError(w, libErrStatus(err), "Could not delete the library")
			return
		}
		c.app.cleanupLibraryQualityRules(lib.ID)
		if lib.Source == core.LibraryPlugin && c.app.pluginsRT != nil && c.app.pluginsRT.pluginitems != nil {
			c.app.pluginsRT.pluginitems.StopKindSync(lib.PluginID, lib.KindKey)
		}
		if err := c.app.refreshLibraryPolicies(r.Context()); err != nil {
			libError(w, http.StatusInternalServerError, "Could not save visibility rules")
			return
		}
		c.app.publishLibrarySections()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /{id}/tabs", c.handleTabsGet)
	mux.HandleFunc("PUT /{id}/tabs", c.handleTabsPut)
	mux.HandleFunc("DELETE /{id}/tabs", c.handleTabsReset)
	mux.HandleFunc("POST /{id}/tabs/preview", c.handleTabsPreview)
	mux.HandleFunc("GET /{id}/live", c.handleLiveList)
	mux.HandleFunc("GET /{id}/live/thumb/{hash}", c.handleLiveThumb)
	mux.HandleFunc("POST /{id}/live/play", c.handleLivePlay)
	mux.HandleFunc("GET /{id}/live/sessions/{session}/index.m3u8", c.handleLiveSessionPlaylist)
	mux.HandleFunc("GET /{id}/live/sessions/{session}/relay/{segment}", c.handleLiveSessionRelay)
	mux.HandleFunc("GET /{id}/live/sessions/{session}/resource/{resource}", c.handleLiveSessionResource)
	mux.HandleFunc("DELETE /{id}/live/sessions/{session}", c.handleLiveSessionStop)
	mux.HandleFunc("GET /{id}/items", c.handleItems)
	mux.HandleFunc("GET /collection-sources", c.handleCollectionSources)
	mux.HandleFunc("GET /catalogue-sources", c.handleCatalogueSources)
	mux.HandleFunc("POST /catalogue-sources/{source}/sync", c.handleCatalogueSourceSync)
	mux.HandleFunc("GET /{id}/facets/{field...}", c.handleFacets)
	mux.HandleFunc("GET /{id}/item/{ref}/match", c.handleMatchGet)
	mux.HandleFunc("PUT /{id}/item/{ref}/match", c.handleMatchPin)
	mux.HandleFunc("DELETE /{id}/item/{ref}/match", c.handleMatchClear)
	mux.HandleFunc("POST /{id}/item/{ref}/match", c.handleMatchRun)
	mux.HandleFunc("GET /{id}/item/{ref}", c.handleItem)
	mux.HandleFunc("POST /{id}/sync", c.handleSync)
	mux.HandleFunc("GET /{id}/cache", c.handleCacheGet)
	mux.HandleFunc("PUT /{id}/cache", c.handleCachePut)
	mux.HandleFunc("POST /{id}/cache/sweep", c.handleCacheSweep)
	mux.HandleFunc("POST /{id}/cache/purge", c.handleCachePurge)
	mux.HandleFunc("GET /{id}/setup", func(w http.ResponseWriter, r *http.Request) {
		lib, ok := c.library(w, r)
		if !ok {
			return
		}
		c.writeSetup(w, r, lib)
	})
	return mux
}

func parseFacets(r *http.Request) map[string][]string {
	out := map[string][]string{}
	for k, vs := range r.URL.Query() {
		if f, ok := strings.CutPrefix(k, "facet."); ok && f != "" {
			for _, v := range vs {
				out[f] = append(out[f], strings.Split(v, ",")...)
			}
		}
	}
	return out
}

func (c *librariesConsole) handleItems(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	p := r.URL.Query()
	limit, _ := strconv.Atoi(p.Get("limit"))
	if lib.Source == core.LibraryPlugin {
		items, _, err := c.services()
		if err != nil || items == nil {
			libError(w, http.StatusServiceUnavailable, "Plugin items are unavailable")
			return
		}
		policy, err := items.LibraryPolicy(r.Context(), lib)
		if err != nil {
			libError(w, 400, "Invalid filter")
			return
		}
		hidden := false
		if mode := p.Get("ruleVisibility"); mode != "" {
			var specs []core.LibraryExclusion
			if (mode != "hidden" && mode != "shown") || json.Unmarshal([]byte(p.Get("rules")), &specs) != nil {
				libError(w, 400, "Invalid filter")
				return
			}
			policy, err = items.CompilePolicy(r.Context(), lib.ID, specs, p.Get("ruleFallback"))
			if err != nil {
				libError(w, 400, "Invalid filter")
				return
			}
			hidden = mode == "hidden"
		}
		facets := parseFacets(r)
		if q := strings.TrimSpace(p.Get("q")); q != "" {
			if p.Get("ruleVisibility") == "" {
				policy = policy.ForSearch()
			}
			page, err := items.Search(r.Context(), pluginitems.SearchQuery{
				Policy: policy, Hidden: hidden, LibraryID: lib.ID, Query: q, Level: p.Get("level"), Facets: facets, Limit: limit,
			})
			if err != nil {
				libError(w, http.StatusInternalServerError, "Search failed")
				return
			}
			libJSON(w, http.StatusOK, map[string]any{
				"items": kindDTOs(page.Items), "total": page.Total, "nextCursor": page.NextCursor,
			})
			return
		}
		page, err := items.Browse(r.Context(), pluginitems.BrowseQuery{
			Policy: policy, Hidden: hidden,
			LibraryID: lib.ID,
			ParentRef: core.ItemRef(p.Get("parent")),
			Level:     p.Get("level"),
			Sort:      p.Get("sort"),
			Desc:      p.Get("desc") == "1",
			Facets:    facets,
			Cursor:    p.Get("cursor"),
			Limit:     limit,
		})
		if err != nil {
			libError(w, http.StatusBadRequest, "Could not browse this library")
			return
		}
		out := map[string]any{
			"items": kindDTOs(page.Items), "total": page.Total, "nextCursor": page.NextCursor,
		}
		if p.Get("ruleVisibility") == "" && p.Get("parent") == "" && len(facets) > 0 && c.app.pluginsRT != nil && c.app.pluginsRT.kindCatalog != nil {
			if c.app.pluginsRT.kindCatalog.MaybeExpand(r.Context(), lib, frontend.ItemQuery{Filter: core.Filter{Facets: facets}}) {
				out["expanding"] = true
			}
		}
		libJSON(w, http.StatusOK, out)
		return
	}
	// Builtin/filter: TMDB titles through the catalogue, scoped.
	var reqFilter core.Filter
	if raw := p.Get("filter"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &reqFilter); err != nil {
			libError(w, http.StatusBadRequest, "Invalid filter")
			return
		}
	}
	if q := strings.TrimSpace(p.Get("q")); q != "" {
		reqFilter.Query = q
	}
	offset, _ := strconv.Atoi(p.Get("offset"))
	sort := core.SortOrder(p.Get("sort"))
	page, err := c.browseTitles(r.Context(), lib, reqFilter, core.BrowseQuery{
		Sort: sort, Desc: p.Get("desc") != "0", Offset: offset, Limit: limit,
	})
	if err != nil {
		libError(w, http.StatusInternalServerError, "Could not browse this library")
		return
	}
	if page.Items == nil {
		page.Items = []core.Title{}
	}
	libJSON(w, http.StatusOK, map[string]any{"items": page.Items, "total": page.Total})
}

// collectionSourceDTO is one collection the New library picker can bind a
// library to: its public id, name and the media type its members are.
type collectionSourceDTO struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	ItemCount int    `json:"itemCount"`
	BuiltIn   bool   `json:"builtIn,omitempty"`
}

// handleCollectionSources lists the owner's collections for the "From a
// collection" step. It reads the stored specifications and their counts,
// never the members, so the list is one indexed read whatever their size.
func (c *librariesConsole) handleCollectionSources(w http.ResponseWriter, r *http.Request) {
	if c.app.collections == nil {
		libJSON(w, http.StatusOK, map[string]any{"collections": []collectionSourceDTO{}})
		return
	}
	specs, err := c.app.collections.ListSpecs(r.Context())
	if err != nil {
		libError(w, libErrStatus(err), "Could not list collections")
		return
	}
	out := make([]collectionSourceDTO, 0, len(specs))
	for _, sp := range specs {
		id, err := strconv.ParseInt(sp.ID, 10, 64)
		if err != nil || id <= 0 {
			// A built-in collection is addressed by slug and cannot back a
			// typed library; leave it out of the picker.
			continue
		}
		mt := sp.MediaType
		out = append(out, collectionSourceDTO{ID: id, Name: sp.Name, MediaType: mt, ItemCount: sp.ItemCount, BuiltIn: sp.BuiltIn})
	}
	libJSON(w, http.StatusOK, map[string]any{"collections": out})
}

func (c *librariesConsole) handleFacets(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	field := r.PathValue("field")
	p := r.URL.Query()
	limit, _ := strconv.Atoi(p.Get("limit"))
	if lib.Source == core.LibraryPlugin {
		items, _, err := c.services()
		if err != nil || items == nil {
			libError(w, http.StatusServiceUnavailable, "Plugin items are unavailable")
			return
		}
		q := p.Get("q")
		offset, _ := strconv.Atoi(p.Get("offset"))
		order := p.Get("order")
		includeDeclared := p.Get("declared") == "1" || strings.EqualFold(p.Get("declared"), "true")
		if q != "" || offset > 0 || order != "" || includeDeclared {
			if limit <= 0 {
				limit = 40
			}
			page, err := items.FieldValues(r.Context(), pluginitems.FieldValuesQuery{
				LibraryID:       lib.ID,
				Level:           p.Get("level"),
				FieldKey:        field,
				Query:           q,
				Offset:          offset,
				Limit:           limit,
				Order:           order,
				IncludeDeclared: includeDeclared,
			})
			if err != nil {
				libError(w, http.StatusInternalServerError, "Could not list filter values")
				return
			}
			if page.Values == nil {
				page.Values = []pluginitems.FacetCount{}
			}
			libJSON(w, http.StatusOK, map[string]any{
				"field":     field,
				"values":    page.Values,
				"truncated": page.More,
				"more":      page.More,
			})
			return
		}
		if limit <= 0 {
			limit = 500 // the store's ceiling; the response says when a field has more
		}
		page, err := items.Facets(r.Context(), lib.ID, p.Get("level"), field, limit)
		if err != nil {
			libError(w, http.StatusInternalServerError, "Could not list filter values")
			return
		}
		if page.Values == nil {
			page.Values = []pluginitems.FacetCount{}
		}
		libJSON(w, http.StatusOK, map[string]any{"field": field, "values": page.Values, "truncated": page.Truncated})
		return
	}
	t := lib.MediaType
	values, err := c.app.collections.Facets(r.Context(), t, field)
	if err != nil {
		libError(w, http.StatusBadRequest, "Unknown filter")
		return
	}
	if values == nil {
		values = []core.FacetValue{}
	}
	libJSON(w, http.StatusOK, map[string]any{"field": field, "values": values})
}

func (c *librariesConsole) itemRef(w http.ResponseWriter, r *http.Request, lib core.Library) (core.ItemRef, *pluginitems.Service, bool) {
	if lib.Source != core.LibraryPlugin {
		libError(w, http.StatusNotFound, "Only plugin libraries have plugin items")
		return "", nil, false
	}
	ref, err := core.ParseItemRef(r.PathValue("ref"))
	if err != nil {
		libError(w, http.StatusBadRequest, "Invalid item reference")
		return "", nil, false
	}
	items, _, err := c.services()
	if err != nil || items == nil {
		libError(w, http.StatusServiceUnavailable, "Plugin items are unavailable")
		return "", nil, false
	}
	if _, libID, err := items.GetByRef(r.Context(), ref); err != nil {
		libError(w, http.StatusNotFound, "Item not found")
		return "", nil, false
	} else if libID != lib.ID {
		libError(w, http.StatusNotFound, "Item not found")
		return "", nil, false
	}
	return ref, items, true
}

func (c *librariesConsole) handleItem(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ref, items, ok := c.itemRef(w, r, lib)
	if !ok {
		return
	}
	ctx := r.Context()
	item, _, _ := items.GetByRef(ctx, ref)
	children, _ := items.Children(ctx, ref, r.URL.Query().Get("cursor"), 48)
	matches, _ := items.ListMatches(ctx, ref)
	if matches == nil {
		matches = []pluginitems.MatchRecord{}
	}
	out := map[string]any{
		"item":     kindItemDTO{KindItem: item, Ref: string(item.Ref), Key: int64(item.Key)},
		"children": kindDTOs(children.Items),
		"matches":  matches,
	}
	if rec, err := items.GetKindByLibrary(ctx, lib.ID); err == nil {
		out["kind"] = rec.Decl
	}
	if src, found, err := items.GetResolution(ctx, ref); item.Unavailable == nil && err == nil && found {
		out["resolution"] = src
	}
	libJSON(w, http.StatusOK, out)
}

func (c *librariesConsole) handleMatchGet(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ref, items, ok := c.itemRef(w, r, lib)
	if !ok {
		return
	}
	matches, err := items.ListMatches(r.Context(), ref)
	if err != nil {
		libError(w, http.StatusInternalServerError, "Could not list matches")
		return
	}
	if matches == nil {
		matches = []pluginitems.MatchRecord{}
	}
	libJSON(w, http.StatusOK, map[string]any{"matches": matches})
}

func (c *librariesConsole) handleMatchPin(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ref, items, ok := c.itemRef(w, r, lib)
	if !ok {
		return
	}
	var body struct {
		CandidateRef string   `json:"candidateRef"`
		Reasons      []string `json:"reasons"`
	}
	if !decodeLibBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.CandidateRef) == "" {
		libError(w, http.StatusBadRequest, "Pick a match first")
		return
	}
	if err := items.PinMatch(r.Context(), ref, body.CandidateRef, body.Reasons); err != nil {
		libError(w, http.StatusInternalServerError, "Could not save the match")
		return
	}
	c.handleMatchGet(w, r)
}

func (c *librariesConsole) handleMatchClear(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ref, items, ok := c.itemRef(w, r, lib)
	if !ok {
		return
	}
	if err := items.ClearManualMatch(r.Context(), ref); err != nil {
		libError(w, http.StatusInternalServerError, "Could not clear the match")
		return
	}
	c.handleMatchGet(w, r)
}

var matchResolveAccept = []string{"range", "download", "container", "release", "file"}

func (c *librariesConsole) handleMatchRun(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ref, items, ok := c.itemRef(w, r, lib)
	if !ok {
		return
	}
	if c.app.pluginsRT == nil || c.app.pluginsRT.mgr == nil {
		libError(w, http.StatusServiceUnavailable, "Matching needs the plugin")
		return
	}
	ctx := r.Context()
	item, _, err := items.GetByRef(ctx, ref)
	if err != nil {
		libError(w, http.StatusNotFound, "Item not found")
		return
	}
	if item.Unavailable != nil {
		msg := strings.TrimSpace(item.Unavailable.Detail)
		if msg == "" {
			msg = "This item is currently unavailable"
		}
		libError(w, http.StatusConflict, msg)
		return
	}
	pr, prErr := c.app.pluginsRT.mgr.Get(ctx, lib.PluginID)
	if prErr != nil || !pr.Enabled {
		libError(w, http.StatusServiceUnavailable, "Matching needs the plugin")
		return
	}
	hasResolver := pr.Manifest.HasCapability(plugins.CapKindResolver)
	hasMatcher := pr.Manifest.HasCapability(plugins.CapMatcher)
	if !hasResolver && !hasMatcher {
		libError(w, http.StatusBadGateway, "The plugin could not match this item")
		return
	}
	rec, recErr := items.GetKindByLibrary(ctx, lib.ID)
	threshold := pluginitems.DefaultMatchThreshold
	if recErr == nil && rec.Decl.Match.ThresholdDefault > 0 {
		threshold = rec.Decl.Match.ThresholdDefault
	}

	var sources []core.PlayableSource
	if hasResolver && recErr == nil && c.app.pluginsRT.kindResolver != nil {
		resolved, rerr := c.app.pluginsRT.kindResolver.ResolveForMatch(ctx, lib, rec, item, matchResolveAccept)
		if rerr != nil {
			switch {
			case errors.Is(rerr, core.ErrPreparing):
				w.Header().Set("Retry-After", strconv.Itoa(core.PreparingRetrySeconds(rerr, 5)))
				libError(w, http.StatusServiceUnavailable, "Matching is still preparing; try again shortly")
				return
			case errors.Is(rerr, core.ErrJobsBusy):
				w.Header().Set("Retry-After", strconv.Itoa(core.JobsBusyRetrySeconds(rerr, 5)))
				libError(w, http.StatusServiceUnavailable, "The plugin is busy preparing other items; try again shortly")
				return
			case errors.Is(rerr, core.ErrNotAvailable):
				// Resolver found no releases/sources right now.
			default:
				if !hasMatcher {
					libError(w, http.StatusBadGateway, "The plugin could not match this item")
					return
				}
			}
		} else {
			sources = resolved
		}
	}

	candidates := sourceCandidatesFromPlayable(sources)
	if hasMatcher {
		_, err := items.MatchItem(ctx, c.app.pluginsRT.mgr, lib.PluginID, lib.KindKey, item, candidates, nil, threshold)
		if err != nil {
			switch {
			case errors.Is(err, core.ErrPreparing):
				w.Header().Set("Retry-After", strconv.Itoa(core.PreparingRetrySeconds(err, 5)))
				libError(w, http.StatusServiceUnavailable, "Matching is still preparing; try again shortly")
				return
			case errors.Is(err, core.ErrJobsBusy):
				w.Header().Set("Retry-After", strconv.Itoa(core.JobsBusyRetrySeconds(err, 5)))
				libError(w, http.StatusServiceUnavailable, "The plugin is busy preparing other items; try again shortly")
				return
			}
			if len(sources) > 0 {
				scores := scoresFromResolvedSources(sources)
				if serr := items.SaveMatches(ctx, ref, lib.PluginID, scores, threshold); serr != nil {
					libError(w, http.StatusInternalServerError, "Could not save matches")
					return
				}
				c.handleMatchGet(w, r)
				return
			}
			libError(w, http.StatusBadGateway, "The plugin could not match this item")
			return
		}
	} else if len(sources) > 0 {
		scores := scoresFromResolvedSources(sources)
		if err := items.SaveMatches(ctx, ref, lib.PluginID, scores, threshold); err != nil {
			libError(w, http.StatusInternalServerError, "Could not save matches")
			return
		}
	}
	c.handleMatchGet(w, r)
}

func sourceCandidatesFromPlayable(sources []core.PlayableSource) []core.SourceCandidate {
	out := make([]core.SourceCandidate, 0, len(sources))
	for _, src := range sources {
		out = append(out, sourceCandidateFromPlayable(src))
	}
	return out
}

func sourceCandidateFromPlayable(src core.PlayableSource) core.SourceCandidate {
	var c core.SourceCandidate
	if src.Release != nil {
		if src.Release.Candidate != nil {
			c = *src.Release.Candidate
		}
		if c.InfoHash == "" {
			c.InfoHash = strings.TrimSpace(src.Release.InfoHash)
		}
		if c.Magnet == "" {
			c.Magnet = strings.TrimSpace(src.Release.Magnet)
		}
		if c.TorrentRef == "" {
			c.TorrentRef = strings.TrimSpace(src.Release.TorrentRef)
		}
		if c.NZBRef == "" {
			c.NZBRef = strings.TrimSpace(src.Release.NZBRef)
		}
		if c.TorrentURL == "" {
			c.TorrentURL = strings.TrimSpace(src.Release.TorrentURL)
		}
		if c.NZBURL == "" {
			c.NZBURL = strings.TrimSpace(src.Release.NZBURL)
		}
		if c.FileIndex == nil && src.Release.FileIndex != nil {
			idx := *src.Release.FileIndex
			c.FileIndex = &idx
		}
		if c.Title == "" {
			c.Title = firstNonEmptyMatchStr(src.Release.File, src.Release.FileHint, src.Label, src.ID)
		}
	} else {
		c.Title = firstNonEmptyMatchStr(src.Label, src.Path, src.URL, src.ID)
	}
	if c.Size <= 0 && src.Size > 0 {
		c.Size = src.Size
	}
	return c
}

func scoresFromResolvedSources(sources []core.PlayableSource) []core.KindMatchScore {
	scores := make([]core.KindMatchScore, 0, len(sources))
	for idx, src := range sources {
		cand := sourceCandidateFromPlayable(src)
		ref := candidateRefForPlayable(src, cand)
		if ref == "" {
			continue
		}
		score := 1.0 - 0.05*float64(idx)
		if score < 0.5 {
			score = 0.5
		}
		var reasons []string
		if label := firstNonEmptyMatchStr(src.Label, cand.Title, src.Pattern); label != "" {
			reasons = []string{label}
		}
		scores = append(scores, core.KindMatchScore{
			CandidateRef: ref,
			Score:        score,
			Reasons:      reasons,
		})
	}
	return scores
}

func candidateRefForPlayable(src core.PlayableSource, c core.SourceCandidate) string {
	switch {
	case c.InfoHash != "":
		return "infohash:" + strings.ToLower(c.InfoHash)
	case c.Magnet != "":
		return "magnet:" + c.Magnet
	case c.TorrentRef != "":
		return "torrentref:" + c.TorrentRef
	case c.NZBRef != "":
		return "nzbref:" + c.NZBRef
	case c.TorrentURL != "":
		return "torrent:" + c.TorrentURL
	case c.NZBURL != "":
		return "nzb:" + c.NZBURL
	case strings.TrimSpace(src.ID) != "":
		return strings.TrimSpace(src.ID)
	case c.Title != "":
		return c.Title
	default:
		return strings.TrimSpace(src.URL)
	}
}

func firstNonEmptyMatchStr(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func (c *librariesConsole) handleSync(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	if lib.Source != core.LibraryPlugin {
		if len(lib.Sources) > 0 && c.app.catalog != nil {
			for _, id := range lib.Sources {
				if _, err := c.app.db.ExecContext(r.Context(), `UPDATE libraries_catalogue_state SET refreshed=0 WHERE source_id=?`, id); err != nil {
					libError(w, 500, "Could not refresh source")
					return
				}
			}
			if err := c.app.catalog.BrowseSourceLibrary(withForceCatalogueSync(r.Context()), lib.Sources); err != nil {
				libError(w, 502, err.Error())
				return
			}
			libJSON(w, 200, map[string]int{"upserted": 0, "deleted": 0, "skipped": 0})
			return
		}
		libError(w, http.StatusConflict, "Only plugin libraries sync from a plugin")
		return
	}
	items, _, err := c.services()
	if err != nil || items == nil {
		libError(w, http.StatusServiceUnavailable, "Plugin items are unavailable")
		return
	}
	var body struct {
		Force        bool `json:"force"`
		FullWalk     bool `json:"fullWalk"`
		KeepExisting bool `json:"keepExisting"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeLibBody(w, r, &body) {
		return
	}
	if body.KeepExisting {
		st, err := items.KeepExistingItems(r.Context(), lib.PluginID, lib.KindKey)
		if err != nil {
			libError(w, http.StatusInternalServerError, "Could not update sync state")
			return
		}
		libJSON(w, http.StatusOK, map[string]any{
			"kept":    true,
			"running": false,
			"sync":    st,
		})
		return
	}
	done, alreadyRunning := items.StartManualSync(lib.PluginID, lib.KindKey, pluginitems.SyncOptions{
		Force:    body.Force,
		FullWalk: body.FullWalk,
		Manual:   true,
	})
	if alreadyRunning {
		st, _ := items.SyncStateOf(r.Context(), lib.PluginID, lib.KindKey)
		libJSON(w, http.StatusAccepted, map[string]any{
			"started":        false,
			"alreadyRunning": true,
			"running":        true,
			"sync":           st,
		})
		return
	}
	// Wait briefly so fast syncs (and immediate delete-guard aborts) return
	// their final outcome directly while long syncs return 202 Accepted and
	// report live progress via sync state and plugin Activity jobs.
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case run := <-done:
		st, _ := items.SyncStateOf(r.Context(), lib.PluginID, lib.KindKey)
		if run.Err != nil {
			var dg *pluginitems.DeleteGuardError
			if errors.As(run.Err, &dg) {
				libJSON(w, http.StatusConflict, map[string]any{
					"error":       dg.PlainMessage(),
					"code":        "delete_guard",
					"deleteGuard": dg.State(),
					"sync":        st,
				})
				return
			}
			msg := strings.TrimSpace(run.Err.Error())
			if msg == "" {
				msg = "Sync failed"
			}
			libJSON(w, http.StatusBadGateway, map[string]any{
				"error": msg,
				"sync":  st,
			})
			return
		}
		libJSON(w, http.StatusOK, map[string]any{
			"upserted":  run.Result.Upserted,
			"deleted":   run.Result.Deleted,
			"skipped":   run.Result.Skipped,
			"protected": run.Result.Protected,
			"running":   false,
			"sync":      st,
		})
	case <-timer.C:
		st, _ := items.SyncStateOf(r.Context(), lib.PluginID, lib.KindKey)
		libJSON(w, http.StatusAccepted, map[string]any{
			"started": true,
			"running": true,
			"sync":    st,
		})
	case <-r.Context().Done():
		return
	}
}

// pluginTextToken is a plugin "$t:key" text reference inside a label.
var pluginTextToken = regexp.MustCompile(`\$t:[A-Za-z0-9_.\-]+`)

type pluginSyncJobTracker struct {
	mgr *plugins.Manager
}

func (t pluginSyncJobTracker) BeginSyncJob(pluginID, kindKey, routePath, label string, cancel context.CancelFunc) pluginitems.SyncJobHandle {
	if t.mgr == nil {
		return nil
	}
	if strings.Contains(label, "$t:") {
		if resolve := t.mgr.ResolveTextFunc(context.Background(), pluginID, ""); resolve != nil {
			label = pluginTextToken.ReplaceAllStringFunc(label, func(tok string) string {
				if text, ok := resolve(strings.TrimPrefix(tok, "$t:")); ok {
					return text
				}
				return tok
			})
		}
	}
	h := t.mgr.BeginHostJob(pluginID, "sync:"+kindKey, routePath, label, kindKey, cancel)
	if h == nil {
		return nil
	}
	return pluginSyncJobHandle{h: h}
}

func (t pluginSyncJobTracker) UpdateSyncJob(pluginID, kindKey, state, code, outcome, errMsg string) {
	if t.mgr == nil {
		return
	}
	t.mgr.UpdateHostJob(pluginID, "sync:"+kindKey, state, code, outcome, errMsg)
}

type pluginSyncJobHandle struct {
	h *plugins.HostJobHandle
}

func (j pluginSyncJobHandle) SetProgress(progress string) {
	j.h.SetProgress(progress)
}

func (j pluginSyncJobHandle) Finish(state, code, outcome, errMsg string) {
	j.h.Finish(state, code, outcome, errMsg)
}

// --- per-library blob cache (spec §11 Cache row, §15.5) ---

func (c *librariesConsole) namespace(w http.ResponseWriter, r *http.Request, lib core.Library) (*blobcache.Namespace, bool) {
	if lib.Source != core.LibraryPlugin {
		libError(w, http.StatusNotFound, "Only plugin libraries have a file cache")
		return nil, false
	}
	items, blobs, err := c.services()
	if err != nil || blobs == nil {
		libError(w, http.StatusServiceUnavailable, "The file cache is unavailable")
		return nil, false
	}
	quota := blobcache.Quota{}
	if items != nil {
		if rec, err := items.GetKindByLibrary(r.Context(), lib.ID); err == nil {
			quota.MaxBytes = rec.Decl.Cache.MaxBytes
			if ttl, err := time.ParseDuration(rec.Decl.Cache.TTL); err == nil {
				quota.TTL = ttl
			}
		}
	}
	blobs.BindPluginLibrary(lib.PluginID, lib.Key)
	return blobs.Namespace(lib.Key, quota), true
}

func cacheDTO(lib core.Library, ns *blobcache.Namespace) map[string]any {
	used, blobs := ns.Stats()
	q := ns.Quota()
	return map[string]any{
		"libraryId": int64(lib.ID), "used": used, "blobs": blobs,
		"maxBytes": q.EffectiveMaxBytes(), "ttlSeconds": int64(q.TTL.Seconds()),
	}
}

func (c *librariesConsole) handleCacheGet(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ns, ok := c.namespace(w, r, lib)
	if !ok {
		return
	}
	_, blobs, _ := c.services()
	out := cacheDTO(lib, ns)
	totalUsed, totalBlobs := blobs.TotalStats()
	out["sharedUsed"] = totalUsed
	out["sharedBlobs"] = totalBlobs
	out["sharedCap"] = blobs.EffectiveSharedCap()
	libJSON(w, http.StatusOK, out)
}

func (c *librariesConsole) handleCachePut(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ns, ok := c.namespace(w, r, lib)
	if !ok {
		return
	}
	var body struct {
		MaxBytes   int64 `json:"maxBytes"`
		TTLSeconds int64 `json:"ttlSeconds"`
	}
	if !decodeLibBody(w, r, &body) {
		return
	}
	if body.MaxBytes < 0 || body.TTLSeconds < 0 {
		libError(w, http.StatusBadRequest, "Cache limits cannot be negative")
		return
	}
	// Owner quota overrides are session-lived; the kind's declared cap (or
	// the 2 GiB default) applies again at restart.
	ns.SetQuota(blobcache.Quota{MaxBytes: body.MaxBytes, TTL: time.Duration(body.TTLSeconds) * time.Second})
	ns.Sweep(0)
	libJSON(w, http.StatusOK, cacheDTO(lib, ns))
}

func (c *librariesConsole) handleCacheSweep(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ns, ok := c.namespace(w, r, lib)
	if !ok {
		return
	}
	ns.Sweep(0)
	libJSON(w, http.StatusOK, cacheDTO(lib, ns))
}

func (c *librariesConsole) handleCachePurge(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ns, ok := c.namespace(w, r, lib)
	if !ok {
		return
	}
	ns.Purge()
	libJSON(w, http.StatusOK, cacheDTO(lib, ns))
}

// --- guided setup (spec §8): library-scoped view + plugin-scoped API ---

type setupStepDTO struct {
	plugins.SetupStep
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func (c *librariesConsole) setupFor(ctx context.Context, pluginID string) (map[string]any, error) {
	mgr := c.app.pluginsRT.mgr
	rec, err := mgr.Get(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	state, err := mgr.SetupState(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	byStep := map[string]plugins.SetupStepState{}
	for _, st := range state {
		byStep[st.Step] = st
	}
	// The setup view shows the plugin's own "$t:" text resolved, never the
	// raw keys.
	resolveText := func(v string) string {
		key, ok := strings.CutPrefix(strings.TrimSpace(v), "$t:")
		if !ok {
			return v
		}
		if got, ok := mgr.ResolveText(ctx, pluginID, key, "en"); ok && strings.TrimSpace(got) != "" {
			return got
		}
		return key
	}
	steps := []setupStepDTO{}
	blockers := []string{}
	if e := rec.Manifest.EffectiveEther(); e != nil && e.Contributes.Setup != nil {
		for _, s := range e.Contributes.Setup.Steps {
			st := byStep[s.Key]
			status := st.Status
			if status == "" {
				status = plugins.SetupPending
			}
			s.Label, s.Hint = resolveText(s.Label), resolveText(s.Hint)
			steps = append(steps, setupStepDTO{SetupStep: s, Status: status, Detail: st.Detail})
			if s.Required && status != plugins.SetupOK && status != plugins.SetupSkipped {
				blockers = append(blockers, s.Key)
			}
		}
	}
	settings, schema, err := mgr.GetSettings(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	kinds := []plugins.MediaKindMeta{}
	libraries := []libraryDTO{}
	if e := rec.Manifest.EffectiveEther(); e != nil {
		kinds = append(kinds, e.Contributes.MediaKinds...)
	}
	if libs, err := c.app.libraries.List(ctx); err == nil {
		for _, lib := range libs {
			if lib.Source == core.LibraryPlugin && lib.PluginID == pluginID {
				libraries = append(libraries, c.decorate(ctx, lib))
			}
		}
	}
	if len(schema) > 0 {
		var doc any
		if json.Unmarshal(schema, &doc) == nil {
			doc = resolveSchemaText(doc, resolveText)
			if b, err := json.Marshal(doc); err == nil {
				schema = b
			}
		}
	}
	return map[string]any{
		"pluginId": pluginID, "pluginName": rec.Name, "steps": steps,
		"blockers": blockers, "settings": settings, "schema": json.RawMessage(schema),
		"kinds": kinds, "libraries": libraries,
	}, nil
}

func (c *librariesConsole) writeSetup(w http.ResponseWriter, r *http.Request, lib core.Library) {
	if lib.Source != core.LibraryPlugin || c.app.pluginsRT == nil || c.app.pluginsRT.mgr == nil {
		libError(w, http.StatusNotFound, "This library has no plugin setup")
		return
	}
	out, err := c.setupFor(r.Context(), lib.PluginID)
	if err != nil {
		libError(w, http.StatusNotFound, "Plugin not found")
		return
	}
	libJSON(w, http.StatusOK, out)
}

func (c *librariesConsole) setupHandler() http.Handler {
	mux := &route.Mux{}
	guard := func(w http.ResponseWriter) bool {
		if c.app.pluginsRT == nil || c.app.pluginsRT.mgr == nil {
			libError(w, http.StatusServiceUnavailable, "Plugins are unavailable")
			return false
		}
		return true
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if c.app.pluginsRT == nil || c.app.pluginsRT.mgr == nil {
			libJSON(w, http.StatusOK, map[string]any{"plugins": []any{}})
			return
		}
		recs, err := c.app.pluginsRT.mgr.List(r.Context())
		if err != nil {
			libError(w, http.StatusInternalServerError, "Could not list plugins")
			return
		}
		var libs []core.Library
		if c.app.libraries != nil {
			libs, _ = c.app.libraries.List(r.Context())
		}
		out := []map[string]any{}
		for _, rec := range recs {
			e := rec.Manifest.EffectiveEther()
			if e == nil || len(e.Contributes.MediaKinds) == 0 {
				continue
			}
			pluginLibs := []libraryDTO{}
			for _, lib := range libs {
				if lib.Source == core.LibraryPlugin && lib.PluginID == rec.ID {
					pluginLibs = append(pluginLibs, c.decorate(r.Context(), lib))
				}
			}
			out = append(out, map[string]any{
				"pluginId":   rec.ID,
				"pluginName": rec.Name,
				"summary":    rec.Manifest.Description,
				"kinds":      e.Contributes.MediaKinds,
				"libraries":  pluginLibs,
			})
		}
		libJSON(w, http.StatusOK, map[string]any{"plugins": out})
	})
	mux.HandleFunc("POST /{plugin}/ensure", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w) {
			return
		}
		ctx := r.Context()
		pluginID := r.PathValue("plugin")
		rec, err := c.app.pluginsRT.mgr.Get(ctx, pluginID)
		if err != nil {
			libError(w, http.StatusNotFound, "Plugin not found")
			return
		}
		e := rec.Manifest.EffectiveEther()
		if e == nil || len(e.Contributes.MediaKinds) == 0 {
			libError(w, http.StatusBadRequest, "This plugin does not contribute a library kind")
			return
		}
		items, _, err := c.services()
		if err != nil {
			libError(w, http.StatusInternalServerError, "Plugin items are unavailable")
			return
		}
		selectedKind := r.URL.Query().Get("kind")
		if selectedKind != "" {
			found := false
			for _, decl := range e.Contributes.MediaKinds {
				if decl.Key == selectedKind && decl.HasLibrary() {
					found = true
				}
			}
			if !found {
				libError(w, http.StatusNotFound, "Library kind not found")
				return
			}
		}
		for _, decl := range e.Contributes.MediaKinds {
			if selectedKind != "" && decl.Key != selectedKind {
				continue
			}
			if !decl.HasLibrary() {
				continue
			}
			name := strings.TrimSpace(decl.Library.Name)
			if name == "" {
				name = strings.TrimSpace(decl.Label)
			}
			if name == "" {
				name = rec.Name
			}
			resolved := libraries.ResolveKindPresentations(consoleKindPresentationInput(decl), core.PresentationOverrides{})
			lib, err := c.app.libraries.RestorePluginLibrary(ctx, pluginID, decl.Key, libraries.PluginLibraryDecl{
				Name:    name,
				Present: resolved.Effective,
			})
			if err != nil {
				libError(w, libErrStatus(err), "Could not create plugin library")
				return
			}
			if _, err := items.UpsertKind(ctx, pluginID, lib.ID, decl, false); err != nil {
				libError(w, http.StatusBadRequest, "Could not initialize plugin library kind")
				return
			}
		}
		if c.app.pluginsRT.kindCatalog != nil {
			c.app.pluginsRT.kindCatalog.RefreshPlugin(ctx, rec)
		}
		c.app.publishLibrarySections()
		out, err := c.setupFor(ctx, pluginID)
		if err != nil {
			libError(w, http.StatusNotFound, "Plugin not found")
			return
		}
		libJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /{plugin}", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w) {
			return
		}
		out, err := c.setupFor(r.Context(), r.PathValue("plugin"))
		if err != nil {
			libError(w, http.StatusNotFound, "Plugin not found")
			return
		}
		libJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("PUT /{plugin}/settings", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w) {
			return
		}
		var values map[string]any
		if !decodeLibBody(w, r, &values) {
			return
		}
		fieldErrors, err := c.app.pluginsRT.mgr.Configure(r.Context(), r.PathValue("plugin"), values)
		if err != nil {
			libError(w, http.StatusBadRequest, "Could not save these settings")
			return
		}
		libJSON(w, http.StatusOK, map[string]any{"fieldErrors": fieldErrors})
	})
	mux.HandleFunc("POST /{plugin}/{step}/test", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w) {
			return
		}
		res, err := c.app.pluginsRT.mgr.TestSetupStep(r.Context(), r.PathValue("plugin"), r.PathValue("step"))
		if err != nil {
			libError(w, http.StatusNotFound, "Setup step not found")
			return
		}
		libJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /{plugin}/{step}/skip", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w) {
			return
		}
		if err := c.app.pluginsRT.mgr.SkipSetupStep(r.Context(), r.PathValue("plugin"), r.PathValue("step")); err != nil {
			libError(w, http.StatusNotFound, "Setup step not found")
			return
		}
		libJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	return mux
}

// resolveSchemaText resolves "$t:" references in a plugin settings schema's
// human text (labels, hints, help, placeholders, option labels).
func resolveSchemaText(v any, resolve func(string) string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			switch k {
			case "label", "hint", "help", "placeholder", "title", "description":
				if str, ok := child.(string); ok {
					t[k] = resolve(str)
					continue
				}
			}
			t[k] = resolveSchemaText(child, resolve)
		}
		return t
	case []any:
		for i := range t {
			t[i] = resolveSchemaText(t[i], resolve)
		}
		return t
	default:
		return v
	}
}
