package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// A Stremio add-on opened in Studio becomes a native hosted plugin that
// still answers through the add-on: its code calls ether.addon.get(route)
// and Ether fetches that route from the add-on link it already holds,
// sealed in the plugin record. The link (often carrying a debrid key)
// never reaches the code, the draft or the console.

const addonRuntimeSuffix = "\x00addon"

// ErrNoAddon reports a plugin with no add-on link behind it.
var ErrNoAddon = errors.New("plugins: this plugin is not bound to an add-on")

// AddonFetch reads one Stremio route from the add-on behind plugin id: the
// add-on itself, or the link a Studio-edited version of it carries. It uses
// the same guarded, plain add-on transport as an imported add-on.
func (m *Manager) AddonFetch(ctx context.Context, id, route string) (Response, error) {
	if !strings.HasPrefix(route, "/") || strings.Contains(route, "..") || len(route) > 4096 {
		return Response{}, &ValidationError{[]FieldError{{"route", "must be an add-on path such as /catalog/movie/top.json"}}}
	}
	rec, err := m.store.record(ctx, id)
	if err != nil {
		return Response{}, err
	}
	if !rec.Enabled {
		return Response{}, ErrDisabled
	}
	if rec.Manifest.IsStremioOnly() {
		return m.Call(ctx, id, Request{Method: http.MethodGet, Path: route})
	}
	if rec.SourceURL == "" {
		return Response{}, ErrNoAddon
	}
	rt, err := m.addonRuntime(rec)
	if err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, stremioDeadline(route))
	defer cancel()
	return rt.Call(ctx, Request{Method: http.MethodGet, Path: route})
}

// addonRuntime is the add-on transport for a plugin that wraps one.
func (m *Manager) addonRuntime(r Record) (Runtime, error) {
	key := r.ID + addonRuntimeSuffix
	m.mu.Lock()
	rt, ok := m.runtimes[key]
	f, hasFactory := m.fatories["remote"]
	m.mu.Unlock()
	if ok {
		return rt, nil
	}
	if !hasFactory {
		return nil, ErrNoRuntime
	}
	plain := r.Manifest
	plain.Ether = nil // the add-on side is a plain Stremio add-on
	rt, err := f(Installed{ID: r.ID, Manifest: plain, SourceURL: r.SourceURL, Host: m.HostFor(r.ID)})
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if existing, ok := m.runtimes[key]; ok {
		m.mu.Unlock()
		_ = rt.Close()
		return existing, nil
	}
	m.runtimes[key] = rt
	m.mu.Unlock()
	return rt, nil
}

// AddonGet implements ether.addon.get for hosted plugins.
func (h *host) AddonGet(ctx context.Context, route string) (json.RawMessage, error) {
	resp, err := h.m.AddonFetch(ctx, h.plugin, route)
	if err != nil {
		return nil, err
	}
	return addonResult(resp)
}

func addonResult(resp Response) (json.RawMessage, error) {
	if len(resp.Body) > MaxFetchBytes {
		return nil, fmt.Errorf("plugins: add-on answered with more than %d bytes", MaxFetchBytes)
	}
	return json.Marshal(map[string]any{"status": resp.Status, "body": string(resp.Body)})
}

// AddonResult is the guest-facing answer for an add-on fetch made on a
// draft's behalf (Studio previews).
func AddonResult(resp Response) (json.RawMessage, error) { return addonResult(resp) }

// AddonLink returns the sealed add-on link behind a plugin, for carrying it
// to a Studio-built replacement or copy. Never send it to a client.
func (m *Manager) AddonLink(ctx context.Context, id string) (string, error) {
	rec, err := m.store.record(ctx, id)
	if err != nil {
		return "", err
	}
	if rec.SourceURL == "" {
		return "", ErrNoAddon
	}
	return rec.SourceURL, nil
}

// PackageFiles returns an installed plugin's own files for its current
// version, so Studio can open it for editing.
func (m *Manager) PackageFiles(ctx context.Context, id string) (map[string][]byte, error) {
	rec, err := m.store.record(ctx, id)
	if err != nil {
		return nil, err
	}
	return m.store.files(ctx, id, rec.Version)
}
