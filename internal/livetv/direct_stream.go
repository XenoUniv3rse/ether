package livetv

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const directResolveTimeout = 10 * time.Second

// DirectStreamResult is returned by a DirectStream's Resolve hook when a
// single-use or expiring upstream URL needs a fresh resolution.
type DirectStreamResult struct {
	URL     string
	Headers map[string]string
}

// newCachedManifest captures a fetched manifest for reuse across subsequent
// opens. Master playlists have every URI rewritten to its absolute URL so
// later opens can reuse the variant/rendition URLs directly without touching
// a single-use master URL again.
func newCachedManifest(raw string, body []byte, base *url.URL, hls bool) *cachedManifest {
	isMaster := hls && isMasterPlaylist(body)
	stored := bytes.Clone(body)
	if isMaster && base != nil {
		if abs, err := rewriteHLS(body, base, func(u string) string { return u }); err == nil {
			stored = abs
		}
	}
	return &cachedManifest{
		raw:      raw,
		body:     stored,
		base:     base,
		hls:      hls,
		isMaster: isMaster,
	}
}

func isExpiredStatus(status int) bool {
	return status == http.StatusForbidden || status == http.StatusGone
}

func expiredProviderStatus(err error) (int, bool) {
	var te *TuneError
	if errors.As(err, &te) && isExpiredStatus(te.UpstreamStatus) {
		return te.UpstreamStatus, true
	}
	return 0, false
}

func headersFromMap(in map[string]string) http.Header {
	h := http.Header{}
	for k, v := range in {
		h.Set(k, v)
	}
	return h
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// directStreamState coordinates cached master-manifest reuse and on-demand
// re-resolution across every reader of an ephemeral direct live stream
// (initial sniff/probe, passthrough, copy/re-encode relay, and Steady
// broadcast hand-off/retries).
type directStreamState struct {
	baseCtx context.Context
	resolve func(context.Context) (DirectStreamResult, error)

	mu            sync.Mutex
	url           string
	rawHdrs       map[string]string
	headers       http.Header
	manifest      *cachedManifest
	gen           uint64
	invalidated   bool
	expiredStatus int
	resolving     *directResolveCall
}

type directResolveCall struct {
	done    chan struct{}
	url     string
	rawHdrs map[string]string
	headers http.Header
	gen     uint64
	err     error
}

func newDirectStreamState(baseCtx context.Context, rawURL string, rawHeaders map[string]string, resolve func(context.Context) (DirectStreamResult, error)) *directStreamState {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	clean := CleanInputHeaders(rawHeaders)
	return &directStreamState{
		baseCtx: baseCtx,
		resolve: resolve,
		url:     strings.TrimSpace(rawURL),
		rawHdrs: clean,
		headers: headersFromMap(clean),
	}
}

func (d *directStreamState) canResolve() bool {
	return d != nil && d.resolve != nil
}

func (d *directStreamState) current() (rawURL string, rawHdrs map[string]string, headers http.Header, cm *cachedManifest, gen uint64, invalidated bool, expStatus int) {
	if d == nil {
		return "", nil, nil, nil, 0, false, 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	exp := d.expiredStatus
	if exp == 0 {
		exp = http.StatusForbidden
	}
	var m *cachedManifest
	if !d.invalidated {
		m = d.manifest
	}
	return d.url, cloneStringMap(d.rawHdrs), d.headers.Clone(), m, d.gen, d.invalidated, exp
}

func (d *directStreamState) setManifest(cm *cachedManifest) {
	if d == nil || cm == nil || !cm.isMaster {
		return
	}
	d.mu.Lock()
	if cm.raw == d.url {
		d.manifest = cm
		d.invalidated = false
		d.expiredStatus = 0
	}
	d.mu.Unlock()
}

func (d *directStreamState) manifestFor(raw string) *cachedManifest {
	if d == nil || raw == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.invalidated || d.manifest == nil || d.manifest.raw != raw {
		return nil
	}
	return d.manifest
}

func (d *directStreamState) invalidate(gen uint64, status int) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.gen == gen {
		d.manifest = nil
		d.invalidated = true
		if isExpiredStatus(status) {
			d.expiredStatus = status
		} else if d.expiredStatus == 0 {
			d.expiredStatus = http.StatusForbidden
		}
	}
	d.mu.Unlock()
}

func (d *directStreamState) needsRefresh(gen uint64) bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.gen > gen || d.invalidated
}

// refresh invokes the stream's re-resolve hook at most once per generation
// (singleflight across concurrent readers). If another reader has already
// advanced past failedGen, it returns the already-refreshed stream without
// calling the hook again.
func (d *directStreamState) refresh(ctx context.Context, failedGen uint64) (rawURL string, rawHdrs map[string]string, headers http.Header, gen uint64, err error) {
	if d == nil {
		return "", nil, nil, 0, core.ErrNotAvailable
	}
	d.mu.Lock()
	if d.gen > failedGen && !d.invalidated {
		u, rh, h, g := d.url, cloneStringMap(d.rawHdrs), d.headers.Clone(), d.gen
		d.mu.Unlock()
		return u, rh, h, g, nil
	}
	if d.resolve == nil {
		status := d.expiredStatus
		if !isExpiredStatus(status) {
			status = http.StatusForbidden
		}
		gen := d.gen
		d.mu.Unlock()
		return "", nil, nil, gen, providerResponseError(status)
	}
	if call := d.resolving; call != nil {
		d.mu.Unlock()
		select {
		case <-call.done:
			return call.url, cloneStringMap(call.rawHdrs), call.headers.Clone(), call.gen, call.err
		case <-ctx.Done():
			return "", nil, nil, failedGen, ctx.Err()
		}
	}
	call := &directResolveCall{done: make(chan struct{})}
	d.resolving = call
	d.mu.Unlock()

	go func() {
		rctx, cancel := context.WithTimeout(d.baseCtx, directResolveTimeout)
		defer cancel()
		res, rerr := d.callResolver(rctx)

		nextURL := strings.TrimSpace(res.URL)
		if rerr == nil && !validHTTP(nextURL) {
			rerr = core.ErrNotAvailable
		}
		d.mu.Lock()
		if rerr == nil {
			clean := CleanInputHeaders(res.Headers)
			hdrs := headersFromMap(clean)
			d.url = nextURL
			d.rawHdrs = clean
			d.headers = hdrs
			d.manifest = nil
			d.invalidated = false
			d.expiredStatus = 0
			d.gen++
			call.url = d.url
			call.rawHdrs = cloneStringMap(clean)
			call.headers = hdrs.Clone()
			call.gen = d.gen
		} else {
			call.err = rerr
		}
		d.resolving = nil
		close(call.done)
		d.mu.Unlock()
	}()

	select {
	case <-call.done:
		return call.url, cloneStringMap(call.rawHdrs), call.headers.Clone(), call.gen, call.err
	case <-ctx.Done():
		return "", nil, nil, failedGen, ctx.Err()
	}
}

// Resolve hooks run in a shared goroutine. A panicking hook must still wake
// all readers and allow their tune to tear down.
func (d *directStreamState) callResolver(ctx context.Context) (result DirectStreamResult, err error) {
	defer func() {
		if recover() != nil {
			err = core.ErrNotAvailable
		}
	}()
	return d.resolve(ctx)
}
