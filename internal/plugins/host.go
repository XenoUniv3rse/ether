package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

// host implements Host for one plugin. Grants are re-read on every call,
// so a revoke takes effect mid-call. Writes are journalled with their
// inverse; in shadow mode they are recorded without applying.
type host struct {
	m      *Manager
	plugin string
}

type readOnlyToolContextKey struct{}

func (h *host) requireWritableToolContext(ctx context.Context) error {
	if readOnly, _ := ctx.Value(readOnlyToolContextKey{}).(bool); readOnly {
		h.noteDenied("read-only tool")
		return fmt.Errorf("%w: read-only tool cannot write", ErrPermissionDenied)
	}
	return nil
}

func (h *host) record(ctx context.Context) (Record, Grants, error) {
	return h.m.store.hostAccess(ctx, h.plugin)
}

func (h *host) deny(r Record, g Grants, key, level string, allowed func(string) bool) error {
	if g.revoked(key) || !allowed(level) {
		h.noteDenied(key)
		return fmt.Errorf("%w: %s", ErrPermissionDenied, key)
	}
	return nil
}

func (h *host) noteDenied(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.m.healthMu.Lock()
	defer h.m.healthMu.Unlock()
	if hh, err := h.m.store.health(ctx, h.plugin); err == nil && hh.Status == "ok" {
		hh.Status = "attention"
		hh.Reason = "A permission it needs was taken back: " + key
		_ = h.m.store.putHealth(ctx, h.plugin, hh)
	}
}

func viewerOK(g Grants, level string, viewerID int64) bool {
	switch level {
	case "all":
		return true
	case "granted_viewers":
		return g.viewerGranted(viewerID)
	}
	return false
}

func (h *host) KVGet(ctx context.Context, key string) ([]byte, bool, error) {
	if _, _, err := h.record(ctx); err != nil {
		return nil, false, err
	}
	value, ok, err := h.m.store.kvGet(ctx, h.plugin, key)
	if !ok || err != nil {
		return value, ok, err
	}
	if normalize := h.m.kindBackends().NormalizeStoredReleaseLinks; normalize != nil {
		value, err = normalize(ctx, value)
	}
	return value, ok && err == nil, err
}

func (h *host) KVPut(ctx context.Context, key string, value []byte) error {
	r, _, err := h.record(ctx)
	if err != nil {
		return err
	}
	err = h.m.store.kvPut(ctx, h.plugin, key, value)
	if errors.Is(err, ErrQuotaExceeded) {
		usage, _ := h.m.store.kvUsage(ctx, h.plugin)
		h.m.emitBusEvent(events.EventPluginQuotaExceeded, h.plugin, events.PluginQuotaExceededPayload{
			PluginID:  h.plugin,
			Name:      r.Name,
			QuotaKind: "kv",
			Used:      usage.Bytes,
			Limit:     KVQuotaBytes,
		}, false)
	}
	return err
}

func (h *host) KVDelete(ctx context.Context, key string) error {
	if _, _, err := h.record(ctx); err != nil {
		return err
	}
	return h.m.store.kvDelete(ctx, h.plugin, key)
}

func (h *host) KVList(ctx context.Context, prefix string) ([]string, error) {
	if _, _, err := h.record(ctx); err != nil {
		return nil, err
	}
	return h.m.store.kvList(ctx, h.plugin, prefix)
}

func (h *host) KVUsage(ctx context.Context) (KVUsage, error) {
	if _, _, err := h.record(ctx); err != nil {
		return KVUsage{}, err
	}
	return h.m.store.kvUsage(ctx, h.plugin)
}

// Settings returns the plugin's stored non-secret settings.
func (h *host) Settings(ctx context.Context) (map[string]any, error) {
	r, _, err := h.record(ctx)
	if err != nil {
		return nil, err
	}
	row, err := h.m.store.requestSettings(ctx, h.plugin)
	if err != nil {
		return nil, err
	}
	return NonSecretSettings(row.Settings, row.Schema, settingsSecrets(r)), nil
}

const (
	// MaxDerivedSecretBytes caps a single plugin-written derived secret value.
	MaxDerivedSecretBytes = 16 << 10
	// MaxDerivedSecrets caps the number of derived secrets a plugin may store.
	MaxDerivedSecrets = 32
	// MaxDerivedSecretTTLSec caps an explicit derived-secret TTL (1 year).
	MaxDerivedSecretTTLSec int64 = 365 * 24 * 3600
)

func (h *host) checkSecretAccess(ctx context.Context, name string) (Record, error) {
	r, g, err := h.record(ctx)
	if err != nil {
		return Record{}, err
	}
	declared := false
	for _, s := range r.Manifest.Ether.Permissions.Secrets {
		if s == name {
			declared = true
			break
		}
	}
	if err := h.deny(r, g, "secrets", "", func(string) bool { return declared }); err != nil {
		return Record{}, err
	}
	return r, nil
}

func (h *host) SecretGet(ctx context.Context, name string) (string, error) {
	if _, err := h.checkSecretAccess(ctx, name); err != nil {
		return "", err
	}
	row, err := h.m.store.requestSettings(ctx, h.plugin)
	if err != nil {
		return "", err
	}
	if stored, ok := row.Secrets[name]; ok && stored != "" {
		return h.m.store.openSecret(h.plugin, name, stored)
	}
	if plain, ok, err := h.m.store.derivedSecret(ctx, h.plugin, name); err != nil {
		return "", err
	} else if ok {
		return plain, nil
	}
	return "", fmt.Errorf("%w: secret %s is not saved", ErrUnavailable, name)
}

func (h *host) SecretSet(ctx context.Context, name, value string, ttlSec int64) error {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return err
	}
	if _, err := h.checkSecretAccess(ctx, name); err != nil {
		return err
	}
	if ttlSec < 0 || ttlSec > MaxDerivedSecretTTLSec {
		return &ValidationError{Fields: []FieldError{{
			Field:   "ttlSec",
			Message: fmt.Sprintf("must be between 0 and %d seconds", MaxDerivedSecretTTLSec),
		}}}
	}
	row, err := h.m.store.settings(ctx, h.plugin)
	if err != nil {
		return err
	}
	if secretFieldNames(row.Schema)[name] || row.Secrets[name] != "" {
		return fmt.Errorf("%w: secret %s is owner-configured and cannot be modified by the plugin", ErrPermissionDenied, name)
	}
	if value == "" {
		return h.m.store.deleteDerivedSecret(ctx, h.plugin, name)
	}
	if len(value) > MaxDerivedSecretBytes {
		return fmt.Errorf("%w: secret %s exceeds %d bytes", ErrQuotaExceeded, name, MaxDerivedSecretBytes)
	}
	if strings.ContainsRune(value, 0) {
		return &ValidationError{Fields: []FieldError{{Field: "value", Message: "must not contain NUL bytes"}}}
	}
	exists, err := h.m.store.hasDerivedSecret(ctx, h.plugin, name)
	if err != nil {
		return err
	}
	if !exists {
		count, err := h.m.store.derivedSecretCount(ctx, h.plugin)
		if err != nil {
			return err
		}
		if count >= MaxDerivedSecrets {
			return fmt.Errorf("%w: derived secret limit (%d) reached", ErrQuotaExceeded, MaxDerivedSecrets)
		}
	}
	var expiresAt int64
	if ttlSec > 0 {
		expiresAt = h.m.now().Unix() + ttlSec
	}
	return h.m.store.putDerivedSecret(ctx, h.plugin, name, value, expiresAt)
}

func (h *host) SecretDelete(ctx context.Context, name string) error {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return err
	}
	if _, err := h.checkSecretAccess(ctx, name); err != nil {
		return err
	}
	row, err := h.m.store.settings(ctx, h.plugin)
	if err != nil {
		return err
	}
	if secretFieldNames(row.Schema)[name] || row.Secrets[name] != "" {
		return fmt.Errorf("%w: secret %s is owner-configured and cannot be modified by the plugin", ErrPermissionDenied, name)
	}
	return h.m.store.deleteDerivedSecret(ctx, h.plugin, name)
}

var errIndexerNetwork = fmt.Errorf("%w: configured indexers must use the indexer host API (sources.resolve with kind nzb_request)", ErrPermissionDenied)

// CheckIndexerNetwork rejects a plugin network request to a configured indexer.
func (m *Manager) CheckIndexerNetwork(hostName string) error {
	if m.deps.IsIndexerHost != nil && m.deps.IsIndexerHost(hostName) {
		return errIndexerNetwork
	}
	return nil
}

// AllowNetworkHost reports whether pluginID is enabled, still holds its
// network grant, and declared hostName in permissions.network.
func (m *Manager) AllowNetworkHost(ctx context.Context, pluginID, hostName string) bool {
	if m.CheckIndexerNetwork(hostName) != nil {
		return false
	}
	h := &host{m: m, plugin: pluginID}
	r, g, err := h.record(ctx)
	if err != nil || r.Manifest.Ether == nil {
		return false
	}
	if g.revoked("network") {
		h.noteDenied("network")
		return false
	}
	if hostAllowed(r.Manifest.Ether.Permissions.Network, hostName) ||
		g.allowsLocalHostName(hostName) ||
		(DeclaresOperatorHosts(r.Manifest) && g.allowsOperatorHostName(hostName)) {
		return true
	}
	if DeclaresOperatorHosts(r.Manifest) {
		m.noteBlockedOperatorHost(ctx, pluginID, hostName)
		return false
	}
	if IsLocalNetworkHost(hostName) {
		m.noteBlockedLocalHost(ctx, pluginID, hostName)
		return false
	}
	h.noteDenied("network")
	return false
}

// hostAllowed matches a host against the manifest network list.
func hostAllowed(patterns []string, host string) bool {
	host = strings.ToLower(host)
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == OperatorNetworkEntry {
			continue
		}
		if strings.HasPrefix(p, "*.") {
			if strings.HasSuffix(host, p[1:]) && host != p[2:] {
				return true
			}
			continue
		}
		if p == host {
			return true
		}
	}
	return false
}

// ErrFetchFailed is returned when a plugin's outbound request fails. Its
// message names only the host and a category: request URLs can carry
// plugin secrets (API keys in the query), and net/http errors include the
// full URL, so neither the URL nor the underlying error text is exposed.
var ErrFetchFailed = errors.New("plugins: request failed")

func fetchError(host string, err error) error {
	var blocked *BlockedLocalHostError
	if errors.As(err, &blocked) {
		return blocked
	}
	var blockedOp *BlockedOperatorHostError
	if errors.As(err, &blockedOp) {
		return blockedOp
	}
	if errors.Is(err, errIndexerNetwork) {
		return errIndexerNetwork
	}
	reason := "network error"
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		reason = "timed out"
	case errors.Is(err, context.Canceled):
		reason = "canceled"
	case errors.Is(err, ErrPermissionDenied):
		// A redirect left the plugin's network grant.
		return fmt.Errorf("%w: a redirect from %s left the granted hosts", ErrPermissionDenied, host)
	case errors.Is(err, safefetch.ErrBlocked):
		if isNonPublicBlockError(err) {
			return &BlockedLocalHostError{Host: host}
		}
		return fmt.Errorf("%w: %s is not allowed (%w)", ErrFetchFailed, host, safefetch.ErrBlocked)
	}
	if reason == "timed out" {
		return fmt.Errorf("%w: %s: %w", ErrFetchFailed, host, context.DeadlineExceeded)
	}
	return fmt.Errorf("%w: %s: %s", ErrFetchFailed, host, reason)
}

type fetchOptions struct {
	stripCrossOriginHeaders []string
	stripAllCallerHeaders   bool
	applyHostLimit          bool
	dedicatedConnection     bool
}

func validateKeepHeadersOnRedirect(man Manifest, g Grants, raw []string) ([]safefetch.RedirectHeaderPattern, error) {
	patterns, err := safefetch.ParseRedirectHeaderPatterns(raw)
	if err != nil {
		return nil, &ValidationError{Fields: []FieldError{{
			Field:   "keepHeadersOnRedirect",
			Message: err.Error(),
		}}}
	}
	var network []string
	if man.Ether != nil {
		network = man.Ether.Permissions.Network
	}
	for _, p := range patterns {
		if !networkCoversRedirectPattern(network, g, man, p) {
			return nil, fmt.Errorf("%w: keepHeadersOnRedirect %q is not in granted network hosts", ErrPermissionDenied, p.Raw)
		}
	}
	return patterns, nil
}

func networkCoversRedirectPattern(network []string, g Grants, man Manifest, p safefetch.RedirectHeaderPattern) bool {
	if p.Wildcard {
		return hostPatternCoveredByNetwork(network, p.Host)
	}
	u := p.SyntheticURL()
	if u == nil {
		return false
	}
	return networkAllowedURL(network, u) ||
		g.allowsLocalURL(u) ||
		(DeclaresOperatorHosts(man) && g.allowsOperatorURL(u))
}

func hostPatternCoveredByNetwork(network []string, wildcardSuffix string) bool {
	for _, raw := range network {
		p := strings.ToLower(strings.TrimSpace(raw))
		if !strings.HasPrefix(p, "*.") {
			continue
		}
		netSuffix := strings.TrimSuffix(p[2:], ".")
		if netSuffix == "" {
			continue
		}
		if wildcardSuffix == netSuffix || strings.HasSuffix(wildcardSuffix, "."+netSuffix) {
			return true
		}
	}
	return false
}

func (h *host) openFetch(ctx context.Context, req FetchRequest) (*http.Response, error) {
	return h.openFetchWithOptions(ctx, req, fetchOptions{})
}

func (h *host) openFetchWithOptions(ctx context.Context, req FetchRequest, opts fetchOptions) (*http.Response, error) {
	r, g, err := h.record(ctx)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(req.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("%w: bad url", ErrPermissionDenied)
	}
	if err := h.m.CheckIndexerNetwork(u.Hostname()); err != nil {
		return nil, err
	}
	if g.revoked("network") {
		h.noteDenied("network")
		return nil, fmt.Errorf("%w: network", ErrPermissionDenied)
	}
	if !networkAllowedURL(r.Manifest.Ether.Permissions.Network, u) &&
		!g.allowsLocalURL(u) &&
		!(DeclaresOperatorHosts(r.Manifest) && g.allowsOperatorURL(u)) {
		if DeclaresOperatorHosts(r.Manifest) {
			blocked := urlHostPortDisplay(u)
			h.m.noteBlockedOperatorHost(ctx, h.plugin, blocked)
			return nil, &BlockedOperatorHostError{Host: blocked}
		}
		if IsLocalNetworkHost(u.Hostname()) {
			blocked := urlHostPortDisplay(u)
			h.m.noteBlockedLocalHost(ctx, h.plugin, blocked)
			return nil, &BlockedLocalHostError{Host: blocked}
		}
		h.noteDenied("network")
		return nil, fmt.Errorf("%w: network", ErrPermissionDenied)
	}
	redirectPatterns, err := validateKeepHeadersOnRedirect(r.Manifest, g, req.KeepHeadersOnRedirect)
	if err != nil {
		if errors.Is(err, ErrPermissionDenied) {
			h.noteDenied("network")
		}
		return nil, err
	}
	if trusted := trustedFetchHosts(r.Manifest, g); len(trusted) > 0 {
		ctx = safefetch.WithTrustedHosts(ctx, trusted)
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	hr, err := http.NewRequestWithContext(ctx, method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	hr.Close = opts.dedicatedConnection
	for k, v := range req.Headers {
		hr.Header.Set(k, v)
	}
	// Recheck the current grant before every redirect, as well as the SSRF
	// guard's own redirect policy. A redirect is another network request.
	// Credentials and caller-supplied headers follow redirects only within the
	// same origin, to a strict same-site subdomain (no scheme downgrade or port
	// change, verified via the Public Suffix List), or to a host explicitly
	// listed in KeepHeadersOnRedirect (still within live network grants and
	// never across HTTPS -> HTTP downgrades). Confinement is sticky across hops.
	// When ctx already has a deadline, let Request.Context govern both headers
	// and body reads so timeouts always surface context.DeadlineExceeded rather
	// than net/http's non-unwrapping Client.Timeout error.
	client := *h.m.client
	if _, ok := ctx.Deadline(); ok {
		client.Timeout = 0
	}
	lastURL := u
	redirectHeaders := safefetch.NewRedirectHeaderState(u, safefetch.RedirectHeaderOptions{
		Patterns:              redirectPatterns,
		InitialHeaders:        hr.Header,
		StripAllCallerHeaders: opts.stripAllCallerHeaders,
		StripHeaders:          opts.stripCrossOriginHeaders,
	})
	previousRedirect := client.CheckRedirect
	release := func() {}
	acquire := func(ctx context.Context, target *url.URL) error {
		if !opts.applyHostLimit || h.m.fetchHosts == nil {
			return nil
		}
		var err error
		release, err = h.m.fetchHosts.Acquire(ctx, target.String())
		if err != nil {
			release = func() {}
			return fetchError(target.Hostname(), err)
		}
		return nil
	}
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		lastURL = next.URL
		if err := h.m.CheckIndexerNetwork(next.URL.Hostname()); err != nil {
			return err
		}
		redirectHeaders.Apply(next)
		next.Close = opts.dedicatedConnection
		current, grants, err := h.record(next.Context())
		if err != nil {
			return err
		}
		if grants.revoked("network") {
			h.noteDenied("network")
			return fmt.Errorf("%w: network", ErrPermissionDenied)
		}
		if !networkAllowedURL(current.Manifest.Ether.Permissions.Network, next.URL) &&
			!grants.allowsLocalURL(next.URL) &&
			!(DeclaresOperatorHosts(current.Manifest) && grants.allowsOperatorURL(next.URL)) {
			if DeclaresOperatorHosts(current.Manifest) {
				blocked := urlHostPortDisplay(next.URL)
				h.m.noteBlockedOperatorHost(next.Context(), h.plugin, blocked)
				return &BlockedOperatorHostError{Host: blocked}
			}
			if IsLocalNetworkHost(next.URL.Hostname()) {
				blocked := urlHostPortDisplay(next.URL)
				h.m.noteBlockedLocalHost(next.Context(), h.plugin, blocked)
				return &BlockedLocalHostError{Host: blocked}
			}
			h.noteDenied("network")
			return fmt.Errorf("%w: network", ErrPermissionDenied)
		}
		*next = *next.WithContext(safefetch.WithTrustedHosts(next.Context(), trustedFetchHosts(current.Manifest, grants)))
		if previousRedirect != nil {
			if err := previousRedirect(next, via); err != nil {
				return err
			}
		} else if len(via) >= 10 {
			return errors.New("plugins: stopped after 10 redirects")
		}
		// Release before acquiring another origin so reciprocal redirects
		// cannot deadlock at the shared concurrency cap.
		if opts.applyHostLimit && fetchHostKey(via[len(via)-1].URL.String()) != fetchHostKey(next.URL.String()) {
			release()
			return acquire(next.Context(), next.URL)
		}
		return nil
	}
	if err := acquire(ctx, u); err != nil {
		return nil, err
	}
	resp, err := client.Do(hr)
	if err != nil {
		release()
		if isNonPublicBlockError(err) {
			blocked := urlHostPortDisplay(lastURL)
			h.m.noteBlockedLocalHost(ctx, h.plugin, blocked)
			return nil, fetchError(blocked, err)
		}
		return nil, fetchError(u.Hostname(), err)
	}
	if opts.applyHostLimit {
		resp.Body = &releaseOnCloseReadCloser{ReadCloser: resp.Body, release: release}
	}
	return resp, nil
}

const (
	defaultHostHTTPFetchTimeout = 30 * time.Second
	maxHostHTTPFetchTimeout     = 60 * time.Second
)

func (h *host) HTTPFetch(ctx context.Context, req FetchRequest) (FetchResponse, error) {
	return h.fetchOne(ctx, req, nil, func(r FetchRequest, respBytes int64) {
		h.noteFetchTraffic(ctx, r, respBytes)
	})
}

func (h *host) catalogCheck(ctx context.Context) (Record, error) {
	r, g, err := h.record(ctx)
	if err != nil {
		return Record{}, err
	}
	return r, h.deny(r, g, "catalog", normLevel(r.Manifest.Ether.Permissions.Catalog), func(l string) bool { return l == "read" })
}

func (h *host) CatalogSearch(ctx context.Context, query string, limit, offset int) (json.RawMessage, error) {
	if _, err := h.catalogCheck(ctx); err != nil {
		return nil, err
	}
	if h.m.deps.Backends.CatalogSearch == nil {
		return nil, ErrUnavailable
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	return h.m.deps.Backends.CatalogSearch(ctx, query, limit, offset)
}

func (h *host) CatalogGet(ctx context.Context, id string) (json.RawMessage, error) {
	if _, err := h.catalogCheck(ctx); err != nil {
		return nil, err
	}
	if h.m.deps.Backends.CatalogGet == nil {
		return nil, ErrUnavailable
	}
	return h.m.deps.Backends.CatalogGet(ctx, id)
}

func (h *host) ViewerGranted(ctx context.Context) (json.RawMessage, error) {
	r, g, err := h.record(ctx)
	if err != nil {
		return nil, err
	}
	p := r.Manifest.Ether.Permissions.Viewers
	allowed := (!g.revoked("viewers.watch_history") && normLevel(p.WatchHistory) != "none") || (!g.revoked("viewers.identity") && normLevel(p.Identity) != "none")
	if !allowed || len(g.Viewers) == 0 {
		return json.RawMessage(`[]`), nil
	}
	if h.m.deps.Backends.ViewerNames == nil {
		return nil, ErrUnavailable
	}
	return h.m.deps.Backends.ViewerNames(ctx, g.Viewers)
}

func (h *host) ViewerHistory(ctx context.Context, viewerID int64, limit, offset int) (json.RawMessage, error) {
	r, g, err := h.record(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.deny(r, g, "viewers.watch_history", normLevel(r.Manifest.Ether.Permissions.Viewers.WatchHistory),
		func(l string) bool { return viewerOK(g, l, viewerID) }); err != nil {
		return nil, err
	}
	if h.m.deps.Backends.ViewerHistory == nil {
		return nil, ErrUnavailable
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	return h.m.deps.Backends.ViewerHistory(ctx, viewerID, limit, offset)
}

// journalWrite records a write (and, unless shadow, has already applied
// it; callers pass the inverse they obtained).
func (h *host) journal(ctx context.Context, r Record, op, detail string, inverse any) error {
	raw, err := json.Marshal(inverse)
	if err != nil {
		return err
	}
	_, err = h.m.store.journal(ctx, JournalEntry{Plugin: h.plugin, Op: op, Detail: detail, Inverse: raw, Shadow: r.Shadow})
	return err
}

type watchInverse struct {
	Op       string `json:"op"`
	ViewerID int64  `json:"viewerId"`
	TitleID  string `json:"titleId"`
	Watched  bool   `json:"watched"`
}

type collectionInverse struct {
	Op       string          `json:"op"`
	Previous json.RawMessage `json:"previous,omitempty"` // empty means delete
	Doc      json.RawMessage `json:"doc,omitempty"`
}

func (h *host) SetWatchState(ctx context.Context, viewerID int64, titleID string, watched bool) error {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return err
	}
	r, g, err := h.record(ctx)
	if err != nil {
		return err
	}
	if err := h.deny(r, g, "viewers.write_watch_state", normLevel(r.Manifest.Ether.Permissions.Viewers.WriteWatchState),
		func(l string) bool { return viewerOK(g, l, viewerID) }); err != nil {
		return err
	}
	if r.Shadow {
		return h.journal(ctx, r, "viewers.setWatchState", titleID, watchInverse{Op: "viewers.setWatchState", ViewerID: viewerID, TitleID: titleID, Watched: !watched})
	}
	if h.m.deps.Backends.SetWatchState == nil {
		return ErrUnavailable
	}
	prev, err := h.m.deps.Backends.SetWatchState(ctx, h.plugin, viewerID, titleID, watched)
	if err != nil {
		return err
	}
	return h.journal(ctx, r, "viewers.setWatchState", titleID, watchInverse{Op: "viewers.setWatchState", ViewerID: viewerID, TitleID: titleID, Watched: prev})
}

func (h *host) CollectionsUpsert(ctx context.Context, doc json.RawMessage) error {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return err
	}
	r, g, err := h.record(ctx)
	if err != nil {
		return err
	}
	if err := h.deny(r, g, "collections", normLevel(r.Manifest.Ether.Permissions.Collections),
		func(l string) bool { return l == "write" }); err != nil {
		return err
	}
	if r.Shadow {
		return h.journal(ctx, r, "collections.upsert", "", collectionInverse{Op: "collections.upsert", Doc: doc})
	}
	if h.m.deps.Backends.CollectionsUpsert == nil {
		return ErrUnavailable
	}
	prev, err := h.m.deps.Backends.CollectionsUpsert(collectionContext(ctx, h.plugin), doc)
	if err != nil {
		return err
	}
	return h.journal(ctx, r, "collections.upsert", "", collectionInverse{Op: "collections.upsert", Previous: prev, Doc: doc})
}

// UndoAll replays a plugin's live journal inverses, newest first, and
// returns how many entries were undone.
func (m *Manager) UndoAll(ctx context.Context, pluginID string) (int, error) {
	return m.UndoSince(ctx, pluginID, 0)
}

// UndoSince reverses the plugin's changes back to and including entry
// since (0 means all of them within the retained live journal window),
// newest first, so an older change is never reversed underneath a later one.
func (m *Manager) UndoSince(ctx context.Context, pluginID string, since int64) (int, error) {
	entries, err := m.store.journalList(ctx, pluginID, JournalLiveMaxEntries, true)
	if err != nil {
		return 0, err
	}
	undone := 0
	for _, e := range entries { // journalList is newest first
		if e.ID < since {
			break
		}
		if err := m.applyInverse(ctx, e); err != nil {
			return undone, err
		}
		if err := m.store.markUndone(ctx, e.ID); err != nil {
			return undone, err
		}
		undone++
	}
	return undone, nil
}

func (m *Manager) applyInverse(ctx context.Context, e JournalEntry) error {
	switch e.Op {
	case "viewers.setWatchState":
		var inv watchInverse
		if err := json.Unmarshal(e.Inverse, &inv); err != nil {
			return err
		}
		if m.deps.Backends.SetWatchState == nil {
			return ErrUnavailable
		}
		_, err := m.deps.Backends.SetWatchState(ctx, e.Plugin, inv.ViewerID, inv.TitleID, inv.Watched)
		return err
	case "collections.upsert":
		var inv collectionInverse
		if err := json.Unmarshal(e.Inverse, &inv); err != nil {
			return err
		}
		if len(inv.Previous) > 0 {
			if m.deps.Backends.CollectionsUpsert == nil {
				return ErrUnavailable
			}
			_, err := m.deps.Backends.CollectionsUpsert(collectionContext(ctx, e.Plugin), inv.Previous)
			return err
		}
		if m.deps.Backends.CollectionsDelete == nil {
			return ErrUnavailable
		}
		return m.deps.Backends.CollectionsDelete(collectionContext(ctx, e.Plugin), inv.Doc)
	case "kinds.upsert":
		var inv kindInverse
		if err := json.Unmarshal(e.Inverse, &inv); err != nil {
			return err
		}
		b := m.kindBackends()
		if len(inv.Previous) > 0 {
			if b.KindUpsert == nil {
				return ErrUnavailable
			}
			if _, err := b.KindUpsert(ctx, e.Plugin, inv.Kind, inv.Previous); err != nil {
				return err
			}
		}
		if len(inv.Items) > 0 {
			if b.KindDelete == nil {
				return ErrUnavailable
			}
			if err := b.KindDelete(ctx, e.Plugin, inv.Kind, inv.Items); err != nil {
				return err
			}
		}
		return nil
	case "kinds.delete":
		var inv kindInverse
		if err := json.Unmarshal(e.Inverse, &inv); err != nil {
			return err
		}
		if len(inv.Previous) > 0 {
			b := m.kindBackends()
			if b.KindUpsert == nil {
				return ErrUnavailable
			}
			_, err := b.KindUpsert(ctx, e.Plugin, inv.Kind, inv.Previous)
			return err
		}
		return nil
	case "badges.set", "badges.clear":
		return nil
	default:
		return fmt.Errorf("plugins: no inverse for journal op %q", e.Op)
	}
}

func (h *host) folderCheck(ctx context.Context, root, path string) (string, error) {
	r, g, err := h.record(ctx)
	if err != nil {
		return "", err
	}
	if err := h.deny(r, g, "folders", normLevel(r.Manifest.Ether.Permissions.Folders),
		func(l string) bool { return l == "read" }); err != nil {
		return "", err
	}
	granted := false
	for _, f := range g.Folders {
		if f == root {
			granted = true
		}
	}
	if !granted {
		h.noteDenied("folders")
		return "", fmt.Errorf("%w: folder was not picked for this plugin", ErrPermissionDenied)
	}
	cleanRoot := filepath.Clean(root)
	joined := filepath.Clean(filepath.Join(cleanRoot, path))
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: path leaves the picked folder", ErrPermissionDenied)
	}
	return joined, nil
}

func (h *host) FoldersStat(ctx context.Context, root, path string) (FolderEntry, error) {
	_, err := h.folderCheck(ctx, root, path)
	if err != nil {
		return FolderEntry{}, err
	}
	fs, err := os.OpenRoot(root)
	if err != nil {
		return FolderEntry{}, err
	}
	defer fs.Close()
	name := path
	if name == "" {
		name = "."
	}
	info, err := fs.Stat(name)
	if err != nil {
		return FolderEntry{}, err
	}
	return FolderEntry{Name: info.Name(), Path: path, Dir: info.IsDir(), Size: info.Size(), Mtime: info.ModTime().Unix()}, nil
}

func (m *Manager) rateOK(bucket map[string][]time.Time, key string, limit int, window time.Duration) bool {
	ok, _ := m.rateOKRetryAt(bucket, key, limit, window)
	return ok
}

func (m *Manager) rateOKRetryAt(bucket map[string][]time.Time, key string, limit int, window time.Duration) (bool, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	kept := bucket[key][:0:0]
	for _, t := range bucket[key] {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	if len(kept) >= limit {
		bucket[key] = kept
		retryAt := now.Add(window)
		if len(kept) > 0 {
			retryAt = kept[0].Add(window)
		}
		return false, retryAt.UTC()
	}
	bucket[key] = append(kept, now)
	return true, time.Time{}
}

func (m *Manager) rateLimitError(message string, retryAt time.Time) error {
	now := m.now()
	var after time.Duration
	if !retryAt.IsZero() {
		retryAt = retryAt.UTC()
		after = retryAt.Sub(now)
		if after < time.Second {
			after = time.Second
		}
	}
	return &RateLimitError{
		Message:    message,
		RetryAt:    retryAt,
		RetryAfter: after,
	}
}

func (h *host) NotifyOwner(ctx context.Context, text string) error {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return err
	}
	if _, _, err := h.record(ctx); err != nil {
		return err
	}
	if !h.m.rateOK(h.m.notifyTimes, h.plugin, 5, time.Hour) {
		return fmt.Errorf("plugins: owner notices are limited to five an hour")
	}
	text = h.m.store.redactKnown(ctx, h.plugin, text)
	if h.m.deps.Backends.Notice != nil {
		h.m.deps.Backends.Notice(truncate(text, 500))
	}
	h.m.store.addLog(ctx, h.plugin, "notice: "+text)
	return nil
}

func (h *host) Log(ctx context.Context, level, msg string, attrs map[string]any) error {
	if _, _, err := h.record(ctx); err != nil {
		return err
	}
	if !h.m.rateOK(h.m.logTimes, h.plugin, 600, time.Minute) {
		return nil // drop, never fail the caller for logging
	}
	line := level + ": " + msg
	if len(attrs) > 0 {
		if raw, err := json.Marshal(attrs); err == nil {
			line += " " + redactJSON(raw)
		}
	}
	h.m.store.addLog(ctx, h.plugin, line)
	return nil
}

func (h *host) noteFetchTraffic(ctx context.Context, req FetchRequest, size int64) {
	u, err := url.Parse(req.URL)
	if err != nil {
		return
	}
	h.m.healthMu.Lock()
	defer h.m.healthMu.Unlock()
	if hh, err := h.m.store.health(ctx, h.plugin); err == nil {
		hh.Observed[u.Hostname()]++
		hh.EgressBytes += size + int64(len(req.Body))
		_ = h.m.store.putHealth(ctx, h.plugin, hh)
	}
}
