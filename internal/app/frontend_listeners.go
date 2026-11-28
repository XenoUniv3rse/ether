package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/braedonsaunders/ether/internal/faillog"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sockerr"
)

// frontendListeners owns listeners and announcers for independently mounted
// protocol instances. A failed bind is retained as status, not a process error.
type frontendListeners struct {
	log               *slog.Logger
	perf              *perfObserver
	failures401       map[frontend.Protocol]*faillog.Tracker
	observed          *observedAddresses
	trustedProxies    func() []netip.Prefix
	mu                sync.Mutex
	opMu              sync.Mutex
	registry          *frontend.Registry
	running           map[frontend.Protocol]*runningFrontend
	failures          map[frontend.Protocol]string
	discovery         *sharedDiscovery
	discoveryEndpoint func(context.Context, frontend.Protocol, netip.AddrPort) (string, error)
}
type runningFrontend struct {
	servers         []*http.Server
	listeners       []net.Listener
	announcers      []frontend.Announcer
	warnings        []string
	busyPort        int
	actualPort      int
	busyPortHolder  string
	prefixes        []string
	sharedPrefixes  []string
	sharedDiscovery bool
}

func newFrontendListeners(r *frontend.Registry) *frontendListeners {
	return &frontendListeners{registry: r, running: make(map[frontend.Protocol]*runningFrontend), failures: make(map[frontend.Protocol]string), observed: newObservedAddresses()}
}
func (m *frontendListeners) start(ctx context.Context, id frontend.Protocol) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	_, exists := m.running[id]
	m.mu.Unlock()
	if exists {
		return frontend.ErrAlreadyMounted
	}
	inst, err := m.registry.Mount(ctx, id)
	if err != nil {
		m.mu.Lock()
		m.failures[id] = err.Error()
		m.mu.Unlock()
		return err
	}
	run := &runningFrontend{}
	rollback := func() {
		for _, ln := range run.listeners {
			_ = ln.Close()
		}
		for _, a := range run.announcers {
			_ = a.Close()
		}
		_ = m.registry.Unmount(context.Background(), id)
	}
	for _, spec := range inst.Listeners() {
		if spec.Shared {
			if inst.Handler() == nil {
				err = errors.New("frontend returned nil handler")
				break
			}
			if id != "plex" {
				prefix := strings.TrimSuffix(spec.PathPrefix, "/")
				if prefix == "" || !strings.HasPrefix(prefix, "/") || prefix == "/ether" || strings.HasPrefix(prefix, "/ether/") {
					err = errors.New("invalid shared listener path")
					break
				}
				m.mu.Lock()
				for _, running := range m.running {
					for _, existing := range running.sharedPrefixes {
						if existing == prefix {
							err = errors.New("shared listener path already in use")
						}
					}
				}
				m.mu.Unlock()
				if err != nil {
					break
				}
				run.sharedPrefixes = append(run.sharedPrefixes, prefix)
			}
			continue
		}
		if spec.TLS {
			err = errors.New("frontend TLS listener requires a certificate provider")
			break
		}
		var ln net.Listener
		ln, err = listenFrontend(id, spec.Address)
		if err != nil {
			break
		}
		run.listeners = append(run.listeners, ln)
		run.prefixes = append(run.prefixes, spec.PathPrefix)
		_, requestedPort, requestedErr := net.SplitHostPort(spec.Address)
		_, actualPort, actualErr := net.SplitHostPort(ln.Addr().String())
		if requestedErr == nil && actualErr == nil && requestedPort != "0" && requestedPort != actualPort {
			reqNum, reqParseErr := strconv.Atoi(requestedPort)
			actNum, actParseErr := strconv.Atoi(actualPort)
			if reqParseErr == nil && actParseErr == nil {
				run.busyPort = reqNum
				run.actualPort = actNum
				run.busyPortHolder = lookupPortHolder(reqNum)
			}
		}
		mountPrefix := strings.TrimSuffix(spec.PathPrefix, "/")
		if mountPrefix == "/" {
			mountPrefix = ""
		}
		handler := m.observe(id, mountPrefix, inst.Handler())
		if handler == nil {
			err = errors.New("frontend returned nil handler")
			break
		}
		if spec.PathPrefix != "" && spec.PathPrefix != "/" {
			prefix := strings.TrimSuffix(spec.PathPrefix, "/")
			if !strings.HasPrefix(prefix, "/") {
				err = errors.New("invalid path prefix")
				break
			}
			next := handler
			handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
					http.NotFound(w, r)
					return
				}
				next.ServeHTTP(w, r)
			})
		}
		if id != "silo" {
			// A Silo client pointed at this port must not read its
			// probe 404 as "update the server" (see siloProbeMiss).
			next := handler
			handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if siloProbeMiss(w, r) {
					return
				}
				next.ServeHTTP(w, r)
			})
		}
		run.servers = append(run.servers, &http.Server{Handler: handler, ConnContext: sockerr.WithConn, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second})
	}
	if err == nil {
		for _, a := range inst.Discovery() {
			if a == nil {
				err = errors.New("nil discovery announcer")
				break
			}
			if err = a.Start(ctx); err != nil {
				_ = a.Close()
				break
			}
			run.announcers = append(run.announcers, a)
		}
	}
	if err != nil {
		rollback()
		m.mu.Lock()
		m.failures[id] = err.Error()
		m.mu.Unlock()
		return fmt.Errorf("start %s: %w", id, err)
	}
	responder, hasDiscovery := inst.(frontend.DiscoveryResponder)
	run.sharedDiscovery = hasDiscovery && m.discovery != nil && m.discoveryEndpoint != nil
	m.mu.Lock()
	delete(m.failures, id)
	m.running[id] = run
	m.mu.Unlock()
	if run.sharedDiscovery {
		_ = m.discovery.add(id, discoveryEntry{responder: responder, endpoint: m.discoveryEndpoint})
	}
	if warm, ok := inst.(interface{ PreparePersonalHomes(context.Context) }); ok {
		warm.PreparePersonalHomes(ctx)
	}
	for i, srv := range run.servers {
		ln := run.listeners[i]
		go func() {
			err := srv.Serve(ln)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				m.mu.Lock()
				m.failures[id] = err.Error()
				m.mu.Unlock()
			}
		}()
	}
	return nil
}

// sharedHandler chooses the longest mounted path, leaving the owner console
// and Plex's root handler to the app's main mux.
func (m *frontendListeners) sharedHandler(path string) http.Handler {
	m.mu.Lock()
	var chosen frontend.Protocol
	var matchedPrefix string
	for id, running := range m.running {
		for _, prefix := range running.sharedPrefixes {
			if (path == prefix || strings.HasPrefix(path, prefix+"/")) && len(prefix) > len(matchedPrefix) {
				chosen, matchedPrefix = id, prefix
			}
		}
	}
	m.mu.Unlock()
	if matchedPrefix == "" {
		return nil
	}
	if instance, ok := m.registry.Instance(chosen); ok {
		return m.observe(chosen, matchedPrefix, instance.Handler())
	}
	return nil
}

func (m *frontendListeners) sharedHandlerWithPrefix(path, mountPrefix string) http.Handler {
	m.mu.Lock()
	var chosen frontend.Protocol
	longest := 0
	for id, running := range m.running {
		for _, prefix := range running.sharedPrefixes {
			if (path == prefix || strings.HasPrefix(path, prefix+"/")) && len(prefix) > longest {
				chosen, longest = id, len(prefix)
			}
		}
	}
	m.mu.Unlock()
	if longest == 0 {
		return nil
	}
	if instance, ok := m.registry.Instance(chosen); ok {
		return m.observe(chosen, mountPrefix, instance.Handler())
	}
	return nil
}
func (m *frontendListeners) stop(ctx context.Context, id frontend.Protocol) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	run, ok := m.running[id]
	if !ok {
		delete(m.failures, id)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	if run.sharedDiscovery && m.discovery != nil {
		m.discovery.remove(id)
	}
	var errs []error
	// A protocol may have long-lived streams. A toggle must not wait forever
	// for them, even when the caller supplied a context with no deadline.
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, srv := range run.servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			errs = append(errs, err)
			_ = srv.Close()
		}
	}
	for _, a := range run.announcers {
		if err := a.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := m.registry.Unmount(ctx, id); err != nil {
		errs = append(errs, err)
	} else {
		m.mu.Lock()
		delete(m.running, id)
		m.mu.Unlock()
	}
	return errors.Join(errs...)
}
func (m *frontendListeners) status(ctx context.Context, id frontend.Protocol) frontend.Status {
	m.mu.Lock()
	run := m.running[id]
	failure := m.failures[id]
	m.mu.Unlock()
	var st frontend.Status
	if inst, ok := m.registry.Instance(id); ok {
		st = inst.Status(ctx)
		st.Mounted = run != nil
	}
	if run != nil {
		providedURLs := len(st.ConnectURLs) > 0
		for i, ln := range run.listeners {
			st.Addresses = append(st.Addresses, ln.Addr().String())
			if !providedURLs {
				addresses, _ := net.InterfaceAddrs()
				st.ConnectURLs = append(st.ConnectURLs, connectionURLs(ln.Addr().String(), run.prefixes[i], addresses)...)
			}
		}
		st.Warnings = append(st.Warnings, run.warnings...)
		if run.busyPort > 0 {
			st.BusyPort = run.busyPort
			st.ActualPort = run.actualPort
			st.BusyPortHolder = run.busyPortHolder
			st.Warnings = append(st.Warnings, busyPortWarning(strconv.Itoa(run.busyPort), st.ConnectURLs))
		}
		if run.sharedDiscovery && m.discovery != nil {
			if err := m.discovery.error(); err != nil {
				msg := discoveryBusyWarning(st.ConnectURLs)
				replaced := false
				for i, warning := range st.Warnings {
					if strings.Contains(warning, "using discovery") || strings.HasPrefix(warning, "Discovery unavailable:") {
						st.Warnings[i] = msg
						replaced = true
					}
				}
				if !replaced {
					st.Warnings = append(st.Warnings, msg)
				}
			}
		}
	}
	if failure != "" {
		st.Error = failure
	}
	if m.observed != nil {
		st.ObservedAddresses = m.observed.list(id)
	}
	return st
}
func (m *frontendListeners) close(ctx context.Context) error {
	m.mu.Lock()
	ids := make([]frontend.Protocol, 0, len(m.running))
	for id := range m.running {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	var errs []error
	for _, id := range ids {
		if err := m.stop(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	if m.discovery != nil {
		m.discovery.close()
	}
	return errors.Join(errs...)
}

// listenFrontend falls back only when a protocol's default port is busy; a
// custom port remains explicit. Plex retains its historical 32450-32459 range.
func listenFrontend(id frontend.Protocol, address string) (net.Listener, error) {
	return listenFrontendWith(id, address, net.Listen)
}

func listenFrontendWith(id frontend.Protocol, address string, listen func(string, string) (net.Listener, error)) (net.Listener, error) {
	ln, err := listen("tcp", address)
	if err == nil || !sockerr.IsAddrInUse(err) {
		return ln, err
	}
	host, portText, splitErr := net.SplitHostPort(address)
	if splitErr != nil {
		return nil, err
	}
	port, parseErr := strconv.Atoi(portText)
	defaultPort, ok := frontend.DefaultPort(id)
	if parseErr != nil || !ok || port != defaultPort {
		return nil, err
	}
	first := defaultPort + 1
	if id == "plex" {
		first = 32450
	}
	for candidate := first; candidate < first+10; candidate++ {
		reserved := false
		for _, other := range []frontend.Protocol{"plex", "jellyfin", "emby", "silo", "vela"} {
			if p, ok := frontend.DefaultPort(other); ok && candidate == p {
				reserved = true
				break
			}
		}
		if reserved {
			continue
		}
		ln, nextErr := listen("tcp", net.JoinHostPort(host, strconv.Itoa(candidate)))
		if nextErr == nil {
			return ln, nil
		}
		if !sockerr.IsAddrInUse(nextErr) {
			return nil, nextErr
		}
	}
	return nil, err
}

func busyPortWarning(port string, urls []string) string {
	return fmt.Sprintf("Another server on this computer is using port %s; Ether still works at %s", port, reachableAddress(urls))
}

func discoveryBusyWarning(urls []string) string {
	return fmt.Sprintf("Another server on this computer is using discovery; Ether still works at %s", reachableAddress(urls))
}

func reachableAddress(urls []string) string {
	best := ""
	bestScore := -1
	for _, raw := range urls {
		score := reachableScore(raw)
		if score > bestScore {
			best, bestScore = raw, score
		}
	}
	if best == "" {
		return "this server"
	}
	return best
}

func reachableScore(raw string) int {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return -1
	}
	score := 15
	ip := net.ParseIP(u.Hostname())
	switch {
	case ip == nil && strings.EqualFold(u.Hostname(), "localhost"):
		return 0
	case ip == nil:
		score = 70
	case ip.IsLoopback():
		return 0
	case ip.To4() != nil && ip.To4()[0] == 192 && ip.To4()[1] == 168:
		score = 80
	case ip.IsPrivate() && ip.To4() != nil:
		score = 40
	case ip.To4() == nil:
		score = 10
	}
	if u.Path != "" && u.Path != "/" && score > 0 {
		score -= 5
	}
	return score
}

// connectionURLs converts actual bound addresses into LAN URLs, avoiding
// loopback, unspecified, and family-mismatched addresses that another device cannot reach.
func connectionURLs(address, prefix string, interfaces []net.Addr) []string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil
	}
	var hosts []string
	ip := net.ParseIP(host)
	v4Only := strings.TrimSpace(host) == "0.0.0.0"
	if ip != nil && ip.IsUnspecified() {
		for _, item := range interfaces {
			ifaceIP, _, err := net.ParseCIDR(item.String())
			if err != nil || ifaceIP.IsLoopback() || ifaceIP.IsUnspecified() || ifaceIP.IsLinkLocalUnicast() {
				continue
			}
			if v4Only && ifaceIP.To4() == nil {
				continue
			}
			hosts = append(hosts, ifaceIP.String())
		}
	} else if host != "" && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		hosts = append(hosts, host)
	}
	seen := map[string]bool{}
	var out []string
	for _, host := range hosts {
		u := url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: prefix}
		value := u.String()
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return reachableScore(out[i]) > reachableScore(out[j])
	})
	return out
}

type frontendQueryContextKey struct{}

// observe aggregates authentication failures per protocol and records recent
// client-visible base addresses in memory on successful responses. Request
// paths, credentials and client identifiers never enter failure summaries.
func (m *frontendListeners) observe(id frontend.Protocol, mountPrefix string, next http.Handler) http.Handler {
	if next == nil {
		return nil
	}
	next = m.observeTraffic(id, mountPrefix, next)
	observed := m.perf.wrap(string(id), catalogueDeadline(string(id), next))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			r = r.WithContext(context.WithValue(r.Context(), frontendQueryContextKey{}, true))
		}
		observed.ServeHTTP(w, r)
	})
}

func (m *frontendListeners) observeTraffic(id frontend.Protocol, mountPrefix string, next http.Handler) http.Handler {
	var tracker *faillog.Tracker
	if m.log != nil && id != "plex" {
		m.mu.Lock()
		if m.failures401 == nil {
			m.failures401 = make(map[frontend.Protocol]*faillog.Tracker)
		}
		tracker = m.failures401[id]
		if tracker == nil {
			tracker = faillog.New(m.log.With("frontend", id), "authentication")
			m.failures401[id] = tracker
		}
		m.mu.Unlock()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(frontend.WithMountPrefix(r.Context(), id, mountPrefix))
		var status int
		if tracker != nil {
			recorder := &statusReasonWriter{ResponseWriter: middleware.NewWrapResponseWriter(w, r.ProtoMajor)}
			next.ServeHTTP(recorder, r)
			status = recorder.Status()
			if status == http.StatusUnauthorized {
				tracker.Fail(errors.New("unauthorized"))
			}
			if (id == "jellyfin" || id == "emby") && status != 0 && status != http.StatusUnauthorized && (status < 200 || status >= 300) {
				// Path only: query strings carry tokens. Infuse 503s were silent
				// because this wrapper used to count 401s and ignore the rest.
				m.log.Warn("frontend response", "frontend", id, "method", r.Method, "route", r.URL.Path, "status", status, "reason", recorder.reason())
			}
		} else {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			status = ww.Status()
		}
		if m.observed != nil && status >= 200 && status < 400 {
			var trusted []netip.Prefix
			if m.trustedProxies != nil {
				trusted = m.trustedProxies()
			}
			m.observed.recordRequest(id, r, mountPrefix, status, trusted)
		}
	})
}

// statusReasonWriter keeps a short error body so a 503 can be logged without
// buffering successful media responses.
type statusReasonWriter struct {
	http.ResponseWriter
	code int
	body []byte
}

func (w *statusReasonWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusReasonWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	if w.code >= 400 && len(w.body) < 240 {
		need := 240 - len(w.body)
		if need > len(p) {
			need = len(p)
		}
		w.body = append(w.body, p[:need]...)
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusReasonWriter) Status() int {
	if w.code != 0 {
		return w.code
	}
	if rec, ok := w.ResponseWriter.(interface{ Status() int }); ok {
		return rec.Status()
	}
	return 0
}

func (w *statusReasonWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusReasonWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusReasonWriter) reason() string {
	s := strings.TrimSpace(string(w.body))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		if status := w.Status(); status != 0 {
			return http.StatusText(status)
		}
		return "error"
	}
	return s
}
