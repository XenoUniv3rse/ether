package plugins

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/plugins/wasm"
)

const (
	// DefaultFetchManyConcurrency and MaxFetchManyConcurrency bound the
	// per-call parallelism of http.fetchMany (safe host default, no owner setting).
	DefaultFetchManyConcurrency = 8
	MaxFetchManyConcurrency     = 8
	// MaxFetchManyRequests caps the number of requests in one http.fetchMany call.
	MaxFetchManyRequests = 64
	// DefaultMaxFetchPerHost bounds concurrent outbound plugin HTTP fetches
	// (http.fetch and http.fetchMany) to any single origin (scheme://host:port)
	// across the manager so plugins cannot flood an upstream host.
	DefaultMaxFetchPerHost = 6
	// MaxFetchManyBytes caps the aggregate raw response body bytes buffered across
	// all items in one http.fetchMany call so a batch cannot exhaust host or guest
	// memory. Each individual response remains bounded by MaxFetchBytes as well.
	MaxFetchManyBytes = MaxFetchBytes
)

// fetchHostLimiter bounds in-flight HTTP fetch requests per normalized origin
// (scheme://host:port). Idle origin entries are dropped once their last holder
// or waiter leaves.
type fetchHostLimiter struct {
	limit int
	mu    sync.Mutex
	hosts map[string]*fetchHostGate
}

type fetchHostGate struct {
	sem  chan struct{}
	refs int
}

func newFetchHostLimiter(limit int) *fetchHostLimiter {
	if limit == 0 {
		limit = DefaultMaxFetchPerHost
	}
	return &fetchHostLimiter{limit: limit}
}

// Acquire claims one in-flight slot for rawURL's normalized origin, waiting
// until a slot is free or ctx ends. The returned release func is idempotent.
func (l *fetchHostLimiter) Acquire(ctx context.Context, rawURL string) (func(), error) {
	if l == nil || l.limit < 0 {
		return func() {}, nil
	}
	key := fetchHostKey(rawURL)
	if key == "" {
		return func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.hosts == nil {
		l.hosts = make(map[string]*fetchHostGate)
	}
	g, ok := l.hosts[key]
	if !ok {
		g = &fetchHostGate{sem: make(chan struct{}, l.limit)}
		l.hosts[key] = g
	}
	g.refs++
	l.mu.Unlock()

	deref := func() {
		l.mu.Lock()
		g.refs--
		if g.refs == 0 && l.hosts[key] == g {
			delete(l.hosts, key)
		}
		l.mu.Unlock()
	}

	select {
	case g.sem <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-g.sem
				deref()
			})
		}, nil
	case <-ctx.Done():
		deref()
		return nil, ctx.Err()
	}
}

func fetchHostKey(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// LimitRangeTransport shares fetchToCache's per-origin budget with range streams.
func (m *Manager) LimitRangeTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if m == nil || m.fetchHosts == nil {
		return base
	}
	return limitedRangeTransport{base: base, limiter: m.fetchHosts}
}

type limitedRangeTransport struct {
	base    http.RoundTripper
	limiter *fetchHostLimiter
}

func (t limitedRangeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	release, err := t.limiter.Acquire(req.Context(), req.URL.String())
	if err != nil {
		return nil, err
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		release()
		return nil, err
	}
	resp.Body = &releaseOnCloseReadCloser{ReadCloser: resp.Body, release: release}
	return resp, nil
}

type releaseOnCloseReadCloser struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (r *releaseOnCloseReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(func() {
		if r.release != nil {
			r.release()
		}
	})
	return err
}

// fetchByteBudget tracks aggregate raw response body bytes reserved across
// concurrent workers in one http.fetchMany batch.
type fetchByteBudget struct {
	mu        sync.Mutex
	remaining int64
	limit     int64
}

func newFetchByteBudget(limit int64) *fetchByteBudget {
	return &fetchByteBudget{remaining: limit, limit: limit}
}

func (b *fetchByteBudget) reserve(n int64) bool {
	if b == nil || n <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.remaining {
		return false
	}
	b.remaining -= n
	return true
}

func (b *fetchByteBudget) refund(n int64) {
	if b == nil || n <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.remaining += n
	if b.remaining > b.limit {
		b.remaining = b.limit
	}
}

// readFetchBody reads up to perItemMax bytes from body while enforcing an
// optional shared batch byte budget. On overflow or read error, reserved batch
// memory is refunded while totalRead still reports the wire bytes consumed for
// egress accounting.
func readFetchBody(body io.Reader, perItemMax int64, budget *fetchByteBudget) (data []byte, totalRead int64, err error) {
	var out []byte
	var reserved int64
	buf := make([]byte, 32<<10)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			totalRead += int64(n)
			if int64(len(out))+int64(n) > perItemMax {
				budget.refund(reserved)
				return nil, totalRead, fmt.Errorf("plugins: response larger than %d bytes", perItemMax)
			}
			if !budget.reserve(int64(n)) {
				budget.refund(reserved)
				return nil, totalRead, fmt.Errorf("plugins: fetchMany batch response larger than %d bytes", budget.limit)
			}
			reserved += int64(n)
			out = append(out, buf[:n]...)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return out, totalRead, nil
			}
			budget.refund(reserved)
			return nil, totalRead, rerr
		}
	}
}

// scheduleFetchOrder interleaves indices 0..len(reqs)-1 round-robin by
// normalized origin so requests to one saturated host do not head-of-line
// block requests to other origins in the same batch.
func scheduleFetchOrder(reqs []FetchRequest) []int {
	if len(reqs) <= 1 {
		out := make([]int, len(reqs))
		for i := range out {
			out[i] = i
		}
		return out
	}
	var keys []string
	buckets := make(map[string][]int)
	for i, req := range reqs {
		key := fetchHostKey(req.URL)
		if _, seen := buckets[key]; !seen {
			keys = append(keys, key)
		}
		buckets[key] = append(buckets[key], i)
	}
	order := make([]int, 0, len(reqs))
	for len(order) < len(reqs) {
		for _, k := range keys {
			q := buckets[k]
			if len(q) == 0 {
				continue
			}
			order = append(order, q[0])
			buckets[k] = q[1:]
		}
	}
	return order
}

func clampFetchManyConcurrency(requested, count int) int {
	c := requested
	if c <= 0 {
		c = DefaultFetchManyConcurrency
	}
	if c > MaxFetchManyConcurrency {
		c = MaxFetchManyConcurrency
	}
	if count > 0 && c > count {
		c = count
	}
	if c < 1 {
		c = 1
	}
	return c
}

func fetchManyErrorFrom(err error) *FetchManyError {
	mapped := mapHostError(err)
	if f, ok := mapped.(*wasm.Fault); ok {
		return &FetchManyError{Code: f.Code, Message: f.Message}
	}
	return &FetchManyError{Code: "host_error", Message: mapped.Error()}
}

func validateFetchManyArgs(reqs []FetchRequest, opts FetchManyOptions) error {
	if len(reqs) > MaxFetchManyRequests {
		return &ValidationError{Fields: []FieldError{{
			Field:   "requests",
			Message: fmt.Sprintf("http.fetchMany accepts at most %d requests per call", MaxFetchManyRequests),
		}}}
	}
	if opts.Concurrency < 0 {
		return &ValidationError{Fields: []FieldError{{
			Field:   "concurrency",
			Message: "concurrency must be a positive integer",
		}}}
	}
	return nil
}

// runFetchMany executes reqs concurrently up to opts.Concurrency (clamped to
// MaxFetchManyConcurrency), charging per-request governance units upfront,
// enforcing MaxFetchManyBytes across the batch, preserving input order, and
// stopping promptly when ctx is canceled or its deadline expires.
func runFetchMany(
	ctx context.Context,
	reqs []FetchRequest,
	opts FetchManyOptions,
	doOne func(ctx context.Context, req FetchRequest, budget *fetchByteBudget) (FetchResponse, error),
) ([]FetchManyResult, error) {
	if err := validateFetchManyArgs(reqs, opts); err != nil {
		return nil, err
	}
	if len(reqs) == 0 {
		return []FetchManyResult{}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(reqs) > 1 {
		if err := wasm.ChargeUnits(ctx, int64(len(reqs)-1)*wasm.FetchManyUnitsPerRequest); err != nil {
			return nil, err
		}
	}

	results := make([]FetchManyResult, len(reqs))
	budget := newFetchByteBudget(MaxFetchManyBytes)
	order := scheduleFetchOrder(reqs)
	jobs := make(chan int, len(order))
	for _, idx := range order {
		jobs <- idx
	}
	close(jobs)

	workers := clampFetchManyConcurrency(opts.Concurrency, len(reqs))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					return
				}
				resp, err := doOne(ctx, reqs[idx], budget)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					results[idx] = FetchManyResult{Error: fetchManyErrorFrom(err)}
					continue
				}
				results[idx] = FetchManyResult{Response: resp}
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

type fetchTrafficEntry struct {
	host  string
	bytes int64
}

func (h *host) noteBatchFetchTraffic(entries []fetchTrafficEntry) {
	if len(entries) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.m.healthMu.Lock()
	defer h.m.healthMu.Unlock()
	if hh, err := h.m.store.health(ctx, h.plugin); err == nil {
		for _, e := range entries {
			if e.host != "" {
				hh.Observed[e.host]++
			}
			hh.EgressBytes += e.bytes
		}
		_ = h.m.store.putHealth(ctx, h.plugin, hh)
	}
}

func clampHostHTTPFetchTimeout(timeoutMS int64) time.Duration {
	if timeoutMS <= 0 {
		return defaultHostHTTPFetchTimeout
	}
	if timeoutMS >= maxHostHTTPFetchTimeout.Milliseconds() {
		return maxHostHTTPFetchTimeout
	}
	return time.Duration(timeoutMS) * time.Millisecond
}

func (h *host) fetchOne(
	ctx context.Context,
	req FetchRequest,
	budget *fetchByteBudget,
	onTraffic func(req FetchRequest, respBytes int64),
) (FetchResponse, error) {
	reqCtx, cancel := context.WithTimeout(ctx, clampHostHTTPFetchTimeout(req.TimeoutMS))
	defer cancel()
	resp, err := h.openFetchWithOptions(reqCtx, req, fetchOptions{applyHostLimit: true})
	if err != nil {
		return FetchResponse{}, err
	}
	hostName := ""
	if u, perr := url.Parse(req.URL); perr == nil && u != nil {
		hostName = u.Hostname()
	}
	defer resp.Body.Close() //nolint:errcheck
	body, totalRead, err := readFetchBody(resp.Body, MaxFetchBytes, budget)
	if err != nil {
		if totalRead > 0 && onTraffic != nil {
			onTraffic(req, totalRead)
		}
		if strings.HasPrefix(err.Error(), "plugins:") {
			return FetchResponse{}, err
		}
		return FetchResponse{}, fetchError(hostName, err)
	}
	if onTraffic != nil {
		onTraffic(req, int64(len(body)))
	}
	out := FetchResponse{Status: resp.StatusCode, Headers: map[string]string{}, Body: body}
	for k := range resp.Header {
		out.Headers[k] = resp.Header.Get(k)
	}
	return out, nil
}

// HTTPFetchMany runs up to MaxFetchManyRequests outbound HTTP calls concurrently
// on the host (bounded to MaxFetchManyConcurrency in-flight calls per batch and
// DefaultMaxFetchPerHost concurrent fetches per origin across the manager).
// Each request applies the plugin's live network grants, local/operator host
// checks, redirect credential policy (#688), per-request timeout, and egress
// accounting. Results are returned in input order, each carrying either a
// response or a per-item error. Canceling ctx aborts in-flight requests and
// returns ctx.Err().
func (h *host) HTTPFetchMany(ctx context.Context, reqs []FetchRequest, opts FetchManyOptions) ([]FetchManyResult, error) {
	var trafficMu sync.Mutex
	var traffic []fetchTrafficEntry
	recordTraffic := func(req FetchRequest, respBytes int64) {
		hostName := ""
		if u, err := url.Parse(req.URL); err == nil && u != nil {
			hostName = u.Hostname()
		}
		trafficMu.Lock()
		traffic = append(traffic, fetchTrafficEntry{
			host:  hostName,
			bytes: respBytes + int64(len(req.Body)),
		})
		trafficMu.Unlock()
	}
	results, err := runFetchMany(ctx, reqs, opts, func(itemCtx context.Context, req FetchRequest, budget *fetchByteBudget) (FetchResponse, error) {
		return h.fetchOne(itemCtx, req, budget, recordTraffic)
	})
	trafficMu.Lock()
	snapshot := traffic
	trafficMu.Unlock()
	h.noteBatchFetchTraffic(snapshot)
	return results, err
}

// FetchManyResultWire encodes FetchManyResult entries for the hosted JSON wire
// shape: successful items match FetchResult ({status, headers, body,
// bodyBase64?}) and failed items carry {error: {code, message}}.
func FetchManyResultWire(results []FetchManyResult) []map[string]any {
	out := make([]map[string]any, len(results))
	for i, r := range results {
		if r.Error != nil {
			out[i] = map[string]any{"error": r.Error}
			continue
		}
		out[i] = FetchResult(r.Response)
	}
	return out
}

type wireFetchRequest struct {
	Method                string            `json:"method"`
	URL                   string            `json:"url"`
	Headers               map[string]string `json:"headers"`
	Body                  string            `json:"body"`
	BodyBase64            string            `json:"bodyBase64"`
	TimeoutMS             int64             `json:"timeoutMs"`
	KeepHeadersOnRedirect []string          `json:"keepHeadersOnRedirect"`
}

func (w wireFetchRequest) toFetchRequest() (FetchRequest, *wasm.Fault) {
	method := w.Method
	if method == "" {
		method = "GET"
	}
	body := []byte(w.Body)
	if w.BodyBase64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(w.BodyBase64)
		if err != nil {
			return FetchRequest{}, &wasm.Fault{Code: "bad_arguments", Message: "bodyBase64 is not valid base64"}
		}
		body = decoded
	}
	return FetchRequest{
		Method:                method,
		URL:                   w.URL,
		Headers:               w.Headers,
		Body:                  body,
		TimeoutMS:             w.TimeoutMS,
		KeepHeadersOnRedirect: w.KeepHeadersOnRedirect,
	}, nil
}

// ParseFetchManyArgs decodes the positional JSON arguments [requests, options?]
// for the http.fetchMany host service.
func ParseFetchManyArgs(args []json.RawMessage) ([]FetchRequest, FetchManyOptions, *wasm.Fault) {
	var reqs []FetchRequest
	if len(args) > 0 && len(args[0]) > 0 && strings.TrimSpace(string(args[0])) != "null" {
		var wire []wireFetchRequest
		if err := json.Unmarshal(args[0], &wire); err != nil {
			return nil, FetchManyOptions{}, &wasm.Fault{Code: "bad_arguments", Message: "http.fetchMany takes (requests[], options?)"}
		}
		if len(wire) > MaxFetchManyRequests {
			return nil, FetchManyOptions{}, &wasm.Fault{
				Code:    string(wasm.StatusInvalidInput),
				Message: fmt.Sprintf("http.fetchMany accepts at most %d requests per call", MaxFetchManyRequests),
			}
		}
		reqs = make([]FetchRequest, len(wire))
		for i, item := range wire {
			req, fault := item.toFetchRequest()
			if fault != nil {
				return nil, FetchManyOptions{}, fault
			}
			reqs[i] = req
		}
	}
	var opts FetchManyOptions
	if len(args) > 1 && len(args[1]) > 0 && strings.TrimSpace(string(args[1])) != "null" {
		var rawOpts struct {
			Concurrency *float64 `json:"concurrency"`
		}
		if err := json.Unmarshal(args[1], &rawOpts); err != nil {
			return nil, FetchManyOptions{}, &wasm.Fault{Code: "bad_arguments", Message: "http.fetchMany options must be {concurrency?}"}
		}
		if rawOpts.Concurrency != nil {
			c := *rawOpts.Concurrency
			if math.IsNaN(c) || math.IsInf(c, 0) || math.Trunc(c) != c || c < 0 || c > math.MaxInt32 {
				return nil, FetchManyOptions{}, &wasm.Fault{Code: string(wasm.StatusInvalidInput), Message: "http.fetchMany concurrency must be a non-negative integer"}
			}
			opts.Concurrency = int(c)
		}
	}
	return reqs, opts, nil
}
