// Package stremio presents Ether's viewer-specific Stremio addon.
package stremio

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reqmemo"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"github.com/braedonsaunders/ether/internal/frontend/kit/support"
)

// writeJSON encodes before writing the status, so an unencodable value is a
// 500, never an empty 200.
func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	status := http.StatusOK
	if err != nil {
		slog.Error("encode response", "component", "stremio", "err", err)
		b, status = []byte(`{"error":{"code":"encode_failed","message":"the server could not encode this response"}}`), http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

const MountPath = "/stremio"

// AddonBasePath adds the mount to a public path prefix unless it already
// ends in the mount.
func AddonBasePath(prefix string) string {
	prefix = strings.TrimRight(prefix, "/")
	if strings.HasSuffix(prefix, MountPath) {
		return prefix
	}
	return prefix + MountPath
}

// Frontend is mounted on the main listener under /stremio.
type Frontend struct{}

func (Frontend) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: "stremio", Name: "Stremio", Version: "1.0.0", Clients: []string{"Stremio"}, SettingsSchema: frontend.AccountSchema("addon")}
}

func (Frontend) Mount(_ context.Context, host frontend.Host) (frontend.Instance, error) {
	if host.Identity == nil || host.Experience == nil || host.Sessions == nil {
		return nil, errors.New("stremio requires identity, experience and sessions")
	}
	i := &instance{host: host}
	if err := i.registerRoutes(); err != nil {
		return nil, err
	}
	return i, nil
}

// RegisteredRoutes enumerates the same routes mounted by the frontend.
func RegisteredRoutes() ([]route.Route, error) {
	i := &instance{}
	if err := i.registerRoutes(); err != nil {
		return nil, err
	}
	return i.router.Routes(), nil
}

func (i *instance) registerRoutes() error {
	i.router = &route.Router{}
	if err := i.router.Handle(http.MethodGet, "/{credential}/manifest.json", http.HandlerFunc(i.manifest)); err != nil {
		return err
	}
	for _, pattern := range []string{"/{credential}/catalog/{type}/{id}", "/{credential}/catalog/{type}/{id}/{extra}"} {
		if err := i.router.Handle(http.MethodGet, pattern, http.HandlerFunc(i.catalog)); err != nil {
			return err
		}
	}
	if err := i.router.Handle(http.MethodGet, "/{credential}/meta/{type}/{id}", http.HandlerFunc(i.meta)); err != nil {
		return err
	}
	if err := i.router.Handle(http.MethodGet, "/image/{secret}", http.HandlerFunc(i.image)); err != nil {
		return err
	}
	if err := i.router.Handle(http.MethodGet, "/{credential}/stream/{type}/{id}", http.HandlerFunc(i.stream)); err != nil {
		return err
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if err := i.router.Handle(method, "/sessions/lazy/{rest...}", http.HandlerFunc(i.serveLazy)); err != nil {
			return err
		}
	}
	return nil
}

type instance struct {
	host   frontend.Host
	router *route.Router
	pages  catalogPages
}

// catalogPages keeps one encoded catalog body per viewer and page freshness.
// A catalogue, policy, or grant change drops it. The body is the response
// already authorized for that viewer.
type catalogPages struct {
	mu      sync.Mutex
	entries map[string]catalogPage
}

type catalogPage struct {
	fresh frontend.PageFreshness
	body  []byte
}

func (i *instance) Handler() http.Handler {
	next := support.Wrap(i.host.Support, "stremio", func(r *http.Request) (*frontend.Viewer, frontend.Device) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) < 2 || parts[0] == "image" || parts[0] == "sessions" {
			return nil, frontend.Device{}
		}
		v, err := i.host.Identity.Resolve(r.Context(), parts[0])
		if err != nil || v == nil {
			return nil, frontend.Device{}
		}
		return v, deviceFor(parts[0])
	}, func(path string) string {
		parts := strings.Split(path, "/")
		if len(parts) > 1 {
			parts[1] = "*"
		}
		return strings.Join(parts, "/")
	}, i.router)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			ctx := r.Context()
			if !reqmemo.Active(ctx) {
				ctx = reqmemo.With(ctx)
			}
			if frontend.RequestMemoFrom(ctx) == nil {
				ctx = frontend.WithRequestMemo(ctx)
			}
			r = r.WithContext(ctx)
		}
		if strings.HasPrefix(r.URL.Path, MountPath+"/") {
			r = r.Clone(r.Context())
			u := *r.URL
			u.Path = strings.TrimPrefix(u.Path, MountPath)
			r.URL = &u
		}
		next.ServeHTTP(w, r)
	})
}
func (*instance) Listeners() []frontend.ListenerSpec {
	return []frontend.ListenerSpec{{Shared: true, PathPrefix: MountPath}}
}
func (*instance) Discovery() []frontend.Announcer { return nil }
func (*instance) Status(context.Context) frontend.Status {
	return frontend.Status{TargetVersion: "1.0", Conformance: "partial"}
}
func (*instance) Close(context.Context) error { return nil }
func (i *instance) Routes() []route.Route     { return i.router.Routes() }

func (i *instance) observeClient(r *http.Request, credential string, viewer *frontend.Viewer) {
	if i.host.ClientSeen != nil {
		i.host.ClientSeen(deviceFor(credential).ID)
	}
	support.Seen(r, i.host.Support, "stremio", viewer, deviceFor(credential))
}

func (i *instance) manifest(w http.ResponseWriter, r *http.Request) {
	credential := r.PathValue("credential")
	viewer, err := i.host.Identity.Resolve(r.Context(), credential)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	i.observeClient(r, credential, viewer)
	views, err := i.host.Experience.Views(r.Context(), viewer)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	types := map[string]bool{}
	catalogs := []any{}
	for _, view := range views {
		types[stremioLibraryType(view)] = true
	}
	for _, typ := range []string{"movie", "series"} {
		if !types[typ] {
			continue
		}
		alias, name := "ether-movies", "Ether Movies"
		if typ == "series" {
			alias, name = "ether-series", "Ether Series"
		}
		catalogs = append(catalogs, map[string]any{"type": typ, "id": alias, "name": name, "extra": []any{map[string]any{"name": "search"}, map[string]any{"name": "skip"}, map[string]any{"name": "genre"}}})
		for _, row := range []struct{ id, name string }{{"continue", "Continue Watching"}, {"recent", "Recently Released"}, {"recommendations", "Recommendations"}} {
			catalogs = append(catalogs, map[string]any{"type": typ, "id": "ether-" + row.id, "name": row.name, "extra": []any{map[string]any{"name": "skip"}}})
		}
	}
	// One catalog per published library whose presentation is a valid
	// Stremio type (spec §6.4). The library's catalog id is ether-lib-<id>;
	// plugin items are addressed as pk: ItemRefs.
	catalogs = append(catalogs, i.libraryCatalogs(r, viewer)...)
	writeJSON(w, map[string]any{
		"id": "org.ether.viewer", "version": "1.0.0", "name": "Ether",
		"types": []string{"movie", "series", "channel", "tv"}, "resources": []string{"catalog", "meta", "stream"},
		"idPrefixes": []string{"tmdb:", "tt", "pk:", catalogKeyPrefix},
		"catalogs":   catalogs,
	})
}

// libraryCatalogs lists one ether-lib-<id> catalog for every library the
// viewer may open whose Stremio presentation type is one Stremio accepts,
// built-in and plugin alike.
func (i *instance) libraryCatalogs(r *http.Request, viewer *frontend.Viewer) []any {
	if i.host.Experience == nil {
		return nil
	}
	views, err := i.host.Experience.Views(r.Context(), viewer)
	if err != nil {
		return nil
	}
	out := make([]any, 0, len(views))
	for _, view := range views {
		typ := stremioLibraryType(view)
		if typ == "" {
			continue
		}
		out = append(out, map[string]any{
			"type": typ, "id": libraryCatalogID(view.ID), "name": view.Name,
			"extra": []any{map[string]any{"name": "skip"}},
		})
	}
	return out
}
