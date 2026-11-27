package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

// The Collection Studio API.

// CollectionStudio backs /ether/api/collections*. Implemented by
// internal/collections.
//
// Errors are mapped by behaviour, so implementations need not import this
// package: core.ErrNotFound → 404; an error with InvalidFields()
// map[string]string → 400 with per-field messages; NotConfigured() bool
// (the owner has not enabled the LLM that Generate drafts with) → 501;
// Conflict() bool → 409; Upstream() bool → 502.
type CollectionStudio interface {
	ListSpecs(ctx context.Context) ([]core.CollectionSpec, error)
	GetSpec(ctx context.Context, id string) (*core.CollectionSpec, error)
	CreateSpec(ctx context.Context, spec core.CollectionSpec) (*core.CollectionSpec, error)
	UpdateSpec(ctx context.Context, id string, spec core.CollectionSpec) (*core.CollectionSpec, error)
	DeleteSpec(ctx context.Context, id string) error
	Preview(ctx context.Context, spec core.CollectionSpec, n int) (*core.CollectionPreview, error)
	Rebuild(ctx context.Context, id string) (*core.CollectionSpec, error)
	Templates() []core.CollectionTemplate
	// Artwork returns the collection's poster at width w.
	Artwork(ctx context.Context, id string, w int) ([]byte, string, time.Time, error)
}

// AddonCatalogImporter backs /ether/api/collections/addon-imports* (bulk
// Stremio catalogue import and manifest-following sync). Implemented by
// internal/plugins/adapters.CatalogImporter.
type AddonCatalogImporter interface {
	List(ctx context.Context) (*core.AddonCatalogImportList, error)
	Preview(ctx context.Context, req core.AddonCatalogPreviewRequest) (*core.AddonCatalogPreview, error)
	Apply(ctx context.Context, req core.AddonCatalogImportRequest) (*core.AddonCatalogImportResult, error)
	SyncAddon(ctx context.Context, addonID string) (*core.AddonCatalogImportResult, error)
	DeleteSubscription(ctx context.Context, addonID string, deleteCollections bool) error
}

// collectionRoutes registers the Collection Studio and catalogue helper
// endpoints. Artwork and the image proxy are public: browsers load them
// with <img> tags, which cannot send the owner token.
func (h *Handler) collectionRoutes(mux *route.Mux, api func(string, http.HandlerFunc)) {
	api("GET /api/collections", h.handleListCollections)
	api("POST /api/collections", h.handleCreateCollection)
	api("POST /api/collections/preview", h.handlePreviewCollection)
	api("GET /api/collections/templates", h.handleCollectionTemplates)
	api("GET /api/collections/template-bundles", h.handleCollectionTemplateBundles)
	api("POST /api/collections/template-bundles/{id}/preview", h.handlePreviewTemplateBundle)
	api("POST /api/collections/template-bundles/{id}/apply", h.handleApplyTemplateBundle)
	api("GET /api/collections/addon-imports", h.handleListAddonImports)
	api("POST /api/collections/addon-imports/preview", h.handlePreviewAddonImport)
	api("POST /api/collections/addon-imports", h.handleApplyAddonImport)
	api("POST /api/collections/addon-imports/{id}/sync", h.handleSyncAddonImport)
	api("DELETE /api/collections/addon-imports/{id}", h.handleDeleteAddonImport)
	api("GET /api/collections/{id}", h.handleGetCollection)
	api("PUT /api/collections/{id}", h.handleUpdateCollection)
	api("DELETE /api/collections/{id}", h.handleDeleteCollection)
	api("POST /api/collections/{id}/rebuild", h.handleRebuildCollection)
	mux.HandleFunc("GET /api/collections/{id}/artwork", h.handleCollectionArtwork)

	api("POST /api/catalog/visibility/preview", h.handleVisibilityPreview)
	api("POST /api/catalog/tier-migration", h.handleTierMigration)
	api("GET /api/catalog/browse", h.handleCatalogBrowse)
	api("GET /api/catalog/interpret", h.handleCatalogInterpretCapability)
	api("POST /api/catalog/interpret", h.handleCatalogInterpret)
	api("GET /api/catalog/search", h.handleCatalogSearch)
	api("GET /api/catalog/facets", h.handleCatalogFacets)
	api("GET /api/catalog/lookup", h.handleCatalogLookup)
	api("GET /api/catalog/external", h.handleCatalogExternal)
	api("GET /api/catalog/title/{key}", h.handleCatalogTitle)
	api("POST /api/catalog/title/{key}/refresh", h.handleCatalogTitleManagement)
	api("GET /api/catalog/title/{key}/matches", h.handleCatalogTitleManagement)
	api("PUT /api/catalog/title/{key}/match", h.handleCatalogTitleManagement)
	api("PUT /api/catalog/title/{key}/episode-order", h.handleTitleEpisodeOrder)
	api("GET /api/catalog/title/{key}/tags", h.handleCatalogTags)
	api("PUT /api/catalog/title/{key}/tags", h.handleCatalogTags)
	api("GET /api/catalog/title/{key}/library", h.handleCatalogTitleLibrary)
	api("PUT /api/catalog/title/{key}/library", h.handleCatalogTitleLibrary)
	api("DELETE /api/catalog/title/{key}/library", h.handleCatalogTitleLibrary)
	api("GET /api/catalog/title/{key}/schedule", h.handleCatalogTitleSchedule)
	api("GET /api/catalog/title/{key}/season/{season}", h.handleCatalogTitleSeason)
	api("GET /api/catalog/title/{key}/releases", h.handleCatalogTitleReleases)
	mux.HandleFunc("GET /api/image", h.handleImage)
	mux.HandleFunc("GET /api/image/generated", h.handleGeneratedImage)
}

// collectionView is a spec plus browser-loadable artwork.
type collectionView struct {
	core.CollectionSpec
	ArtworkURL string `json:"artworkUrl,omitempty"`
}

func viewOf(s core.CollectionSpec) collectionView {
	s.Rules, s.Warnings = encodableRules(s.Rules, s.Warnings)
	s.Exclude, s.Warnings = encodableRules(s.Exclude, s.Warnings)
	if len(s.Sources) > 0 {
		srcs := make([]core.CollectionSource, len(s.Sources))
		copy(srcs, s.Sources)
		for i := range srcs {
			// A legacy add-on catalog awaiting migration never shows its link.
			if srcs[i].URL != "" && strings.HasPrefix(srcs[i].Kind, "stremio") {
				srcs[i].URL = redact(srcs[i].URL)
			}
		}
		s.Sources = srcs
	}
	v := collectionView{CollectionSpec: s}
	if s.ID != "" {
		stamp := ""
		if s.LastBuiltAt != nil {
			stamp = strconv.FormatInt(s.LastBuiltAt.Unix(), 10)
		}
		if s.UpdatedAt != nil {
			stamp += "-" + strconv.FormatInt(s.UpdatedAt.Unix(), 10)
		}
		v.ArtworkURL = "/ether/api/collections/" + s.ID + "/artwork?v=" + stamp
	}
	return v
}

// encodableRules drops rules whose stored value is not valid JSON, so one
// damaged rule cannot fail the whole collection list. Each dropped rule is
// reported as a warning on its collection.
func encodableRules(rules []core.CollectionRule, warnings []string) ([]core.CollectionRule, []string) {
	out := rules[:0:0]
	for _, r := range rules {
		if len(r.Value) == 0 || !json.Valid(r.Value) {
			msg := fmt.Sprintf("Rule %q has a damaged value and was skipped. Edit the collection to set it again.", r.Field)
			has := false
			for _, w := range warnings {
				if w == msg {
					has = true
					break
				}
			}
			if !has {
				warnings = append(warnings, msg)
			}
			continue
		}
		out = append(out, r)
	}
	return out, warnings
}

// decodeSpec reads a CollectionSpec, tolerating fields the server owns
// (id, itemCount, artworkUrl…) that the UI sends back unchanged.
func decodeSpec(r *http.Request) (core.CollectionSpec, error) {
	var s core.CollectionSpec
	if r.Body == nil {
		return s, errBadJSON
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes+1))
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("%w: %s", errBadJSON, err)
	}
	return s, nil
}

func (h *Handler) handleListCollections(w http.ResponseWriter, r *http.Request) {
	specs, err := h.deps.Collections.ListSpecs(r.Context())
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	// Owner collections first (newest edits first), then built-ins.
	sort.SliceStable(specs, func(i, j int) bool {
		if specs[i].BuiltIn != specs[j].BuiltIn {
			return !specs[i].BuiltIn
		}
		return false
	})
	// Each collection is encoded on its own, so one that cannot be encoded
	// is left out with a logged warning instead of failing the whole list.
	out := make([]json.RawMessage, 0, len(specs))
	for _, s := range specs {
		b, err := json.Marshal(viewOf(s))
		if err != nil {
			h.log.Warn("admin: collection left out of the list; it could not be encoded", "id", s.ID, "name", s.Name, "err", err)
			continue
		}
		out = append(out, b)
	}
	writeJSON(w, http.StatusOK, map[string]any{"collections": out})
}

func (h *Handler) handleGetCollection(w http.ResponseWriter, r *http.Request) {
	s, err := h.deps.Collections.GetSpec(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, viewOf(*s))
}

func (h *Handler) invalidateOwnerRows() {
	if inv, ok := h.deps.Personalization.(interface{ InvalidateOwnerRows() }); ok {
		inv.InvalidateOwnerRows()
	}
}

func (h *Handler) handleCreateCollection(w http.ResponseWriter, r *http.Request) {
	spec, err := decodeSpec(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	s, err := h.deps.Collections.CreateSpec(r.Context(), spec)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	h.invalidateOwnerRows()
	writeJSON(w, http.StatusCreated, viewOf(*s))
}

func (h *Handler) handleUpdateCollection(w http.ResponseWriter, r *http.Request) {
	spec, err := decodeSpec(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	id := r.PathValue("id")
	s, err := h.deps.Collections.UpdateSpec(r.Context(), id, spec)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	h.invalidateOwnerRows()
	writeJSON(w, http.StatusOK, viewOf(*s))
}

func (h *Handler) handleDeleteCollection(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Collections.DeleteSpec(r.Context(), r.PathValue("id")); err != nil {
		h.writeServiceError(w, err)
		return
	}
	h.invalidateOwnerRows()
	w.WriteHeader(http.StatusNoContent)
}

// previewResponse is the contract shape: TitleCards, not titles.
type previewResponse struct {
	Total    int         `json:"total"`
	Items    []TitleCard `json:"items"`
	Warnings []string    `json:"warnings"`
	TookMs   int64       `json:"tookMs"`
}

func (h *Handler) handlePreviewCollection(w http.ResponseWriter, r *http.Request) {
	spec, err := decodeSpec(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 200 {
		n = 60
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	started := time.Now()
	p, err := h.deps.Collections.Preview(ctx, spec, n)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	warnings := p.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	took := p.TookMs
	if took == 0 {
		took = time.Since(started).Milliseconds()
	}
	writeJSON(w, http.StatusOK, previewResponse{Total: p.Total, Items: cardsFor(p.Items), Warnings: warnings, TookMs: took})
}

func (h *Handler) handleRebuildCollection(w http.ResponseWriter, r *http.Request) {
	s, err := h.deps.Collections.Rebuild(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	h.invalidateOwnerRows()
	writeJSON(w, http.StatusOK, map[string]any{"itemCount": s.ItemCount, "lastBuiltAt": s.LastBuiltAt, "warnings": s.Warnings})
}

func (h *Handler) handleCollectionTemplates(w http.ResponseWriter, r *http.Request) {
	ts := h.deps.Collections.Templates()
	if ts == nil {
		ts = []core.CollectionTemplate{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": ts})
}

func (h *Handler) handleCollectionArtwork(w http.ResponseWriter, r *http.Request) {
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	resolve := h.deps.Collections.Artwork
	if r.URL.Query().Get("card") == "1" {
		source, ok := h.deps.Collections.(interface {
			CustomCardArtwork(context.Context, string, int) ([]byte, string, time.Time, error)
		})
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "no artwork for this collection")
			return
		}
		resolve = source.CustomCardArtwork
	}
	data, ct, mod, err := resolve(r.Context(), r.PathValue("id"), width)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) || isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", "no artwork for this collection")
			return
		}
		h.writeServiceError(w, err)
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", ct)
	// Images (SVG logos, owner-supplied custom art) must never run script on
	// the owner UI origin.
	hdr.Set("Content-Security-Policy", imageCSP)
	// The UI versions the URL (?v=) by build time, so a day of caching is safe.
	hdr.Set("Cache-Control", "public, max-age=86400")
	if !mod.IsZero() {
		hdr.Set("Last-Modified", mod.UTC().Format(http.TimeFormat))
		if ims, err := http.ParseTime(r.Header.Get("If-Modified-Since")); err == nil && !mod.Truncate(time.Second).After(ims) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	hdr.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

// writeServiceError maps component errors to API errors by behaviour.
func (h *Handler) writeServiceError(w http.ResponseWriter, err error) {
	var invalid interface{ InvalidFields() map[string]string }
	var notConfigured interface{ NotConfigured() bool }
	var conflict interface{ Conflict() bool }
	var upstream interface{ Upstream() bool }
	switch {
	case errors.Is(err, core.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.As(err, &invalid):
		fields := invalid.InvalidFields()
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fe := make([]FieldError, 0, len(keys))
		for _, k := range keys {
			fe = append(fe, FieldError{Field: k, Message: fields[k]})
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"code": "validation", "message": "the collection is invalid", "fields": fe},
		})
	case errors.As(err, &notConfigured) && notConfigured.NotConfigured():
		writeError(w, http.StatusNotImplemented, "not_configured", err.Error())
	case errors.As(err, &conflict) && conflict.Conflict():
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.As(err, &upstream) && upstream.Upstream():
		writeError(w, http.StatusBadGateway, "upstream", err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "timeout", "that took too long; try again")
	default:
		h.log.Warn("admin: collections API", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (h *Handler) handleListAddonImports(w http.ResponseWriter, r *http.Request) {
	if h.deps.AddonCatalogImports == nil {
		writeJSON(w, http.StatusOK, core.AddonCatalogImportList{
			Imports:         []core.AddonCatalogSubscription{},
			AvailableAddons: []core.AddonCatalogAvailable{},
		})
		return
	}
	res, err := h.deps.AddonCatalogImports.List(r.Context())
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) handlePreviewAddonImport(w http.ResponseWriter, r *http.Request) {
	if h.deps.AddonCatalogImports == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "Stremio add-on imports are not available")
		return
	}
	var req core.AddonCatalogPreviewRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	res, err := h.deps.AddonCatalogImports.Preview(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) handleApplyAddonImport(w http.ResponseWriter, r *http.Request) {
	if h.deps.AddonCatalogImports == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "Stremio add-on imports are not available")
		return
	}
	var req core.AddonCatalogImportRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	res, err := h.deps.AddonCatalogImports.Apply(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	h.invalidateOwnerRows()
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) handleSyncAddonImport(w http.ResponseWriter, r *http.Request) {
	if h.deps.AddonCatalogImports == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "Stremio add-on imports are not available")
		return
	}
	res, err := h.deps.AddonCatalogImports.SyncAddon(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	h.invalidateOwnerRows()
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) handleDeleteAddonImport(w http.ResponseWriter, r *http.Request) {
	if h.deps.AddonCatalogImports == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "Stremio add-on imports are not available")
		return
	}
	deleteCollections := r.URL.Query().Get("deleteCollections") == "true" || r.URL.Query().Get("deleteCollections") == "1"
	if err := h.deps.AddonCatalogImports.DeleteSubscription(r.Context(), r.PathValue("id"), deleteCollections); err != nil {
		h.writeServiceError(w, err)
		return
	}
	if deleteCollections {
		h.invalidateOwnerRows()
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTierMigration pauses or resumes the library-tier migration:
// {"paused": true|false}. Progress is in the catalogue status.
func (h *Handler) handleTierMigration(w http.ResponseWriter, r *http.Request) {
	if h.deps.PauseTierMigration == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "the tier migration is not available")
		return
	}
	var body struct {
		Paused *bool `json:"paused"`
	}
	if err := decodeJSON(r, &body); err != nil || body.Paused == nil {
		writeError(w, http.StatusBadRequest, "bad_request", `send {"paused": true} or {"paused": false}`)
		return
	}
	if err := h.deps.PauseTierMigration(r.Context(), *body.Paused); err != nil {
		writeError(w, http.StatusConflict, "tier_migration", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"paused": *body.Paused})
}
