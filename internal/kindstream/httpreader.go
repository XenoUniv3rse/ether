package kindstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

const (
	probeHeadBytes     = 512
	defaultHTTPTimeout = 30 * time.Second
	maxRangeRetries    = 2

	seqBufMinWindow = 32 << 10 // 32 KiB initial read-ahead window
	seqBufMaxWindow = 4 << 20  // 4 MiB max sequential read-ahead window
)

// defaultStreamingHTTPClient bounds connection setup and response headers to
// defaultHTTPTimeout without imposing a fixed deadline on response body reads,
// so streaming range reads and whole-volume downloads honour the request
// context rather than failing after 30 seconds.
func defaultStreamingHTTPClient() *http.Client {
	if dt, ok := http.DefaultTransport.(*http.Transport); ok && dt != nil {
		tr := dt.Clone()
		tr.ResponseHeaderTimeout = defaultHTTPTimeout
		return &http.Client{Transport: tr}
	}
	return &http.Client{}
}

// errNoRangeSupport is returned by probe when an upstream server ignores Range
// and responds with 200 OK for the whole resource. When body is non-nil, the
// caller must close it (or stream it into blobcache when demoting to download).
type noRangeSupportError struct {
	resp *http.Response
	size int64
	head []byte
}

func (e *noRangeSupportError) Error() string {
	return "kindstream: upstream does not support byte ranges (returned 200 OK)"
}

func (e *noRangeSupportError) closeBody() {
	if e != nil && e.resp != nil && e.resp.Body != nil {
		_ = e.resp.Body.Close()
	}
}

// HostBackoff tracks per-host exponential backoff and Retry-After windows on
// 429 and 503 responses.
type HostBackoff struct {
	mu    sync.Mutex
	hosts map[string]hostBackoffEntry
}

type hostBackoffEntry struct {
	failures int
	until    time.Time
}

// NewHostBackoff creates a per-host backoff tracker.
func NewHostBackoff() *HostBackoff {
	return &HostBackoff{hosts: make(map[string]hostBackoffEntry)}
}

var defaultBackoff = NewHostBackoff()

// IsBackingOff reports whether rawURL's host currently has an active rate-limit
// or service-unavailable backoff window.
func (b *HostBackoff) IsBackingOff(rawURL string) bool {
	if b == nil {
		return false
	}
	host := extractHost(rawURL)
	if host == "" {
		return false
	}
	b.mu.Lock()
	entry, ok := b.hosts[host]
	b.mu.Unlock()
	return ok && time.Until(entry.until) > 0
}

func (b *HostBackoff) wait(ctx context.Context, rawURL string) error {
	if b == nil {
		return nil
	}
	host := extractHost(rawURL)
	if host == "" {
		return nil
	}
	b.mu.Lock()
	entry, ok := b.hosts[host]
	b.mu.Unlock()
	if !ok {
		return nil
	}
	waitDur := time.Until(entry.until)
	if waitDur <= 0 {
		return nil
	}
	t := time.NewTimer(waitDur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (b *HostBackoff) onSuccess(rawURL string) {
	if b == nil {
		return
	}
	host := extractHost(rawURL)
	if host == "" {
		return
	}
	b.mu.Lock()
	delete(b.hosts, host)
	b.mu.Unlock()
}

func (b *HostBackoff) onRateLimit(rawURL string, retryAfterHeader string) {
	if b == nil {
		return
	}
	host := extractHost(rawURL)
	if host == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.hosts[host]
	e.failures++
	delay := time.Duration(100<<min(e.failures-1, 5)) * time.Millisecond // 100ms .. 3.2s
	if ra := parseRetryAfter(retryAfterHeader); ra > 0 {
		delay = min(ra, 10*time.Second)
	}
	e.until = time.Now().Add(delay)
	b.hosts[host] = e
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func extractHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

type httpReaderConfig struct {
	Connections           int
	Template              *URLTemplate
	Headers               map[string]string
	SecretHeaderKeys      []string
	KeepHeadersOnRedirect []safefetch.RedirectHeaderPattern
	ItemIndex             int
	KnownSize             int64
	Name                  string
	MIME                  string
	Client                *http.Client
	AllowHost             func(host string) bool
	Backoff               *HostBackoff
}

// HTTPReaderAt implements ByteSource (and io.ReaderAt) over an HTTP Range or
// range-templated URL source.
type HTTPReaderAt struct {
	connections           int
	validator             string
	validatorHeader       string
	resourceURL           string
	tmpl                  *URLTemplate
	headers               map[string]string
	secretHeaderKeys      []string
	keepHeadersOnRedirect []safefetch.RedirectHeaderPattern
	itemIndex             int
	size                  int64
	name                  string
	mime                  string
	client                *http.Client
	allowHost             func(host string) bool
	backoff               *HostBackoff

	head []byte // first <=512 bytes captured during initial probe
}

func newHTTPReaderAt(ctx context.Context, cfg httpReaderConfig) (*HTTPReaderAt, error) {
	client := cfg.Client
	if client == nil {
		client = defaultStreamingHTTPClient()
	}
	backoff := cfg.Backoff
	if backoff == nil {
		backoff = defaultBackoff
	}
	r := &HTTPReaderAt{
		connections:           cfg.Connections,
		tmpl:                  cfg.Template,
		headers:               cfg.Headers,
		secretHeaderKeys:      cfg.SecretHeaderKeys,
		keepHeadersOnRedirect: cfg.KeepHeadersOnRedirect,
		itemIndex:             cfg.ItemIndex,
		size:                  cfg.KnownSize,
		name:                  cfg.Name,
		mime:                  cfg.MIME,
		client:                client,
		allowHost:             cfg.AllowHost,
		backoff:               backoff,
	}
	if err := r.probe(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *HTTPReaderAt) probe(ctx context.Context) error {
	probeEnd := int64(probeHeadBytes - 1)
	if r.size > 0 && r.size-1 < probeEnd {
		probeEnd = r.size - 1
	}
	if probeEnd < 0 {
		probeEnd = 0
	}

	resp, targetURL, err := r.doRangeRequest(ctx, 0, probeEnd, false)
	if err != nil {
		return err
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		defer resp.Body.Close()
		totalSize, err := parseContentRangeTotal(resp.Header.Get("Content-Range"))
		if err == nil && totalSize > 0 {
			if r.size > 0 && r.size != totalSize {
				return fmt.Errorf("kindstream: size mismatch on probe: upstream reported %d, source declared %d", totalSize, r.size)
			}
			r.size = totalSize
		} else if r.size <= 0 {
			return errors.New("kindstream: 206 response missing total size in Content-Range and source Size is unset")
		}
		if r.connections > 1 && !r.tmpl.UsesRangePlaceholders() && err == nil {
			end := min(probeEnd, r.size-1)
			if !exactRange(resp, 0, end, r.size) {
				return errors.New("kindstream: incorrect probe range")
			}
			r.validatorHeader, r.validator = rangeValidator(resp.Header)
			if resp.Request != nil && resp.Request.URL != nil {
				r.resourceURL = resp.Request.URL.String()
			}
		}
		head, err := io.ReadAll(io.LimitReader(resp.Body, probeHeadBytes))
		if err != nil {
			return fmt.Errorf("kindstream: read probe body: %w", err)
		}
		r.head = head
		if r.name == "" {
			r.name = defaultNameFromURL(targetURL)
		}
		return nil

	case http.StatusOK:
		// If the URL template itself encodes the byte range ({byteRange} or
		// {byteStart}+{byteEnd}) or {byteStart}, the upstream may legitimately
		// return 200 OK for that slice.
		if r.tmpl.HasByteRange() || r.tmpl.HasByteStartOnly() {
			defer resp.Body.Close()
			head, err := io.ReadAll(io.LimitReader(resp.Body, probeHeadBytes))
			if err != nil {
				return fmt.Errorf("kindstream: read templated probe body: %w", err)
			}
			r.head = head
			if r.size <= 0 {
				if total, err := parseContentRangeTotal(resp.Header.Get("Content-Range")); err == nil && total > 0 {
					r.size = total
				} else if r.tmpl.HasByteStartOnly() && resp.ContentLength > 0 {
					r.size = resp.ContentLength
				}
			}
			if r.size <= 0 {
				return errors.New("kindstream: templated range source requires declared Size or Content-Range")
			}
			if r.name == "" {
				r.name = defaultNameFromURL(targetURL)
			}
			return nil
		}

		// Standard URL returned 200 OK to Range: bytes=0-511 -> upstream does
		// not support Range requests (or ignored Range). Read up to 512 bytes
		// for sniffing and let Open decide whether to demote to download.
		head, err := io.ReadAll(io.LimitReader(resp.Body, probeHeadBytes))
		if err != nil {
			_ = resp.Body.Close()
			return fmt.Errorf("kindstream: read 200 probe head: %w", err)
		}
		size := r.size
		if resp.ContentLength > 0 {
			if size > 0 && size != resp.ContentLength {
				_ = resp.Body.Close()
				return fmt.Errorf("kindstream: size mismatch on 200 response: Content-Length %d != declared %d", resp.ContentLength, size)
			}
			size = resp.ContentLength
		}
		return &noRangeSupportError{
			resp: resp,
			size: size,
			head: head,
		}

	default:
		_ = resp.Body.Close()
		return fmt.Errorf("kindstream: probe HTTP %d", resp.StatusCode)
	}
}

func (r *HTTPReaderAt) doRangeRequest(ctx context.Context, start, end int64, dedicated bool) (*http.Response, string, error) {
	targetURL, err := r.tmpl.Expand(TemplateVars{
		ItemIndex: r.itemIndex,
		ByteStart: start,
		ByteEnd:   end,
		HasRange:  true,
	}, r.allowHost)
	if err != nil {
		return nil, "", err
	}

	var lastErr error
	for attempt := 0; attempt <= maxRangeRetries; attempt++ {
		if err := r.backoff.wait(ctx, targetURL); err != nil {
			return nil, targetURL, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
		if err != nil {
			return nil, targetURL, err
		}
		req.Close = dedicated
		for k, v := range r.headers {
			req.Header.Set(k, v)
		}
		if r.validator != "" {
			req.Header.Set("If-Range", r.validator)
		}
		stripKeys := r.secretHeaderKeys
		if !r.tmpl.HasByteStartOnly() && !r.tmpl.HasByteRange() {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
			if len(stripKeys) > 0 {
				filtered := make([]string, 0, len(stripKeys))
				for _, k := range stripKeys {
					if k != "Range" {
						filtered = append(filtered, k)
					}
				}
				stripKeys = filtered
			}
		}
		client := wrapRedirectClient(r.client, req.URL, req.Header, stripKeys, r.keepHeadersOnRedirect, r.allowHost)
		resp, err := client.Do(req)
		if err != nil {
			if errors.Is(err, ErrHostNotAllowed) || errors.Is(err, ErrInvalidTemplate) {
				return nil, targetURL, err
			}
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			ra := resp.Header.Get("Retry-After")
			_ = resp.Body.Close()
			r.backoff.onRateLimit(targetURL, ra)
			lastErr = fmt.Errorf("kindstream: upstream HTTP %d", resp.StatusCode)
			continue
		}
		r.backoff.onSuccess(targetURL)
		return resp, targetURL, nil
	}
	return nil, targetURL, lastErr
}

// ReadAt reads len(p) bytes starting at byte offset off.
func (r *HTTPReaderAt) ReadAt(p []byte, off int64) (int, error) {
	return r.readAtCtx(context.Background(), p, off)
}

// ReadAtContext reads len(p) bytes starting at byte offset off using ctx.
func (r *HTTPReaderAt) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	return r.readAtCtx(ctx, p, off)
}

// ReadAtConcurrentContext implements core.ConcurrentRangeReaderAt. HTTPReaderAt
// issues independent ranged GET requests with no shared seek epoch, so parallel
// direct-play range requests never cancel one another.
func (r *HTTPReaderAt) ReadAtConcurrentContext(ctx context.Context, p []byte, off int64) (int, error) {
	return r.readAtCtx(ctx, p, off)
}

func (r *HTTPReaderAt) readAtCtx(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, fmt.Errorf("kindstream: negative read offset %d", off)
	}
	if off >= r.size {
		if len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}

	want := int64(len(p))
	hitEOF := false
	if rem := r.size - off; want > rem {
		want = rem
		hitEOF = true
	}

	// Serve directly from cached probe head when the read falls inside it.
	if off+want <= int64(len(r.head)) {
		n := copy(p[:want], r.head[off:off+want])
		if hitEOF {
			return n, io.EOF
		}
		return n, nil
	}

	if r.validator != "" && want >= 256<<10 {
		// Split the adaptive read-ahead window. Workers fill disjoint slices, and
		// finish before returning, so a seek leaves no speculative fetches behind.
		g, groupCtx := errgroup.WithContext(ctx)
		count := min(int64(r.connections), want/(64<<10))
		for i := int64(0); i < count; i++ {
			lo, hi := want*i/count, want*(i+1)/count
			g.Go(func() error {
				_, err := r.readRange(groupCtx, p[lo:hi], off+lo, true)
				return err
			})
		}
		if err := g.Wait(); err != nil {
			return 0, err
		}
	} else {
		n, err := r.readRange(ctx, p[:want], off, false)
		if err != nil {
			return n, err
		}
	}
	if hitEOF {
		return int(want), io.EOF
	}
	return int(want), nil
}

func rangeValidator(h http.Header) (string, string) {
	etag := strings.TrimSpace(h.Get("ETag"))
	if len(etag) >= 2 && etag[0] == '"' && etag[len(etag)-1] == '"' {
		return "ETag", etag
	}
	modified, merr := http.ParseTime(h.Get("Last-Modified"))
	date, derr := http.ParseTime(h.Get("Date"))
	if merr == nil && derr == nil && date.Sub(modified) >= time.Minute {
		return "Last-Modified", h.Get("Last-Modified")
	}
	return "", ""
}

func exactRange(resp *http.Response, start, end, size int64) bool {
	encoding := resp.Header.Get("Content-Encoding")
	if resp.Uncompressed || (encoding != "" && !strings.EqualFold(encoding, "identity")) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(resp.Header.Get("Content-Range")), fmt.Sprintf("bytes %d-%d/%d", start, end, size)) &&
		(resp.ContentLength < 0 || resp.ContentLength == end-start+1)
}

func (r *HTTPReaderAt) readRange(ctx context.Context, p []byte, start int64, dedicated bool) (int, error) {
	want := int64(len(p))
	end := start + want - 1
	resp, _, err := r.doRangeRequest(ctx, start, end, dedicated)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent && !(resp.StatusCode == http.StatusOK && r.tmpl.UsesRangePlaceholders()) {
		return 0, fmt.Errorf("kindstream: range read [%d-%d] unexpected HTTP %d", start, end, resp.StatusCode)
	}

	if r.validator != "" && (!exactRange(resp, start, end, r.size) ||
		resp.Header.Get(r.validatorHeader) != r.validator ||
		resp.Request == nil || resp.Request.URL == nil || resp.Request.URL.String() != r.resourceURL) {
		return 0, errors.New("kindstream: range representation changed or incorrect interval")
	}
	n, err := io.ReadFull(io.LimitReader(resp.Body, want), p[:want])
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return n, io.ErrUnexpectedEOF
		}
		return n, err
	}
	if r.validator != "" && resp.ContentLength < 0 {
		var tail [1]byte
		if n, err := io.ReadFull(resp.Body, tail[:]); n != 0 || err != io.EOF {
			return 0, errors.New("kindstream: invalid range length")
		}
	}
	return n, nil
}

// SniffBytes returns up to the first 512 bytes captured during the initial probe.
func (r *HTTPReaderAt) SniffBytes() []byte {
	return r.head
}

// Size returns the total byte size of the resource.
func (r *HTTPReaderAt) Size() int64 { return r.size }

// Name returns the resource's filename.
func (r *HTTPReaderAt) Name() string { return r.name }

// MIME returns the resource's MIME type.
func (r *HTTPReaderAt) MIME() string { return r.mime }

// Close is a no-op for stateless HTTP range readers.
func (r *HTTPReaderAt) Close() error { return nil }

// seqBufferedReaderAt coalesces small sequential ReadAt calls (as issued by
// archive header walkers and deflate/LZMA/RAR decoders) into adaptive range
// reads while keeping random seeks cheap.
type seqBufferedReaderAt struct {
	ctx    context.Context
	r      io.ReaderAt
	hr     *HTTPReaderAt
	size   int64
	mu     sync.Mutex
	buf    []byte
	bufOff int64
	window int
}

// ReadAhead wraps r for one sequential consumer (one HTTP response): small
// sequential reads such as http.ServeContent's 32 KiB copies are coalesced
// into ranged reads that grow to 4 MiB, while a seek drops back to a small
// window. Create one per request; it is not meant to be shared.
func ReadAhead(ctx context.Context, r io.ReaderAt, size int64) io.ReaderAt {
	if _, ok := r.(*seqBufferedReaderAt); ok || size <= 0 {
		return r
	}
	return newSeqBufferedReaderAt(ctx, r, size)
}

func newSeqBufferedReaderAt(ctx context.Context, r io.ReaderAt, size int64) io.ReaderAt {
	if ctx == nil {
		ctx = context.Background()
	}
	hr, _ := r.(*HTTPReaderAt)
	return &seqBufferedReaderAt{
		ctx:    ctx,
		r:      r,
		hr:     hr,
		size:   size,
		bufOff: -1,
		window: seqBufMinWindow,
	}
}

func (b *seqBufferedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, fmt.Errorf("kindstream: negative read offset %d", off)
	}
	if off >= b.size {
		if len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if len(p) >= seqBufMaxWindow {
		return b.rawReadAt(p, off)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	total := 0
	for total < len(p) && off < b.size {
		if err := b.ctx.Err(); err != nil {
			return total, err
		}
		if b.bufOff >= 0 && off >= b.bufOff && off < b.bufOff+int64(len(b.buf)) {
			start := int(off - b.bufOff)
			n := copy(p[total:], b.buf[start:])
			total += n
			off += int64(n)
			continue
		}

		sequential := b.bufOff >= 0 && off == b.bufOff+int64(len(b.buf))
		if sequential {
			b.window = min(b.window*4, seqBufMaxWindow)
		} else {
			b.window = seqBufMinWindow
		}

		fetchLen := max(b.window, len(p)-total)
		fetchLen = min(fetchLen, seqBufMaxWindow)
		if rem := b.size - off; int64(fetchLen) > rem {
			fetchLen = int(rem)
		}
		if cap(b.buf) < fetchLen {
			b.buf = make([]byte, fetchLen)
		} else {
			b.buf = b.buf[:fetchLen]
		}
		n, err := b.rawReadAt(b.buf, off)
		if n > 0 {
			b.buf = b.buf[:n]
			b.bufOff = off
			copied := copy(p[total:], b.buf)
			total += copied
			off += int64(copied)
		} else {
			b.buf = b.buf[:0]
			b.bufOff = -1
		}
		if err != nil {
			if errors.Is(err, io.EOF) && off < b.size {
				return total, io.ErrUnexpectedEOF
			}
			return total, err
		}
	}
	if off >= b.size && total < len(p) {
		return total, io.EOF
	}
	return total, nil
}

func (b *seqBufferedReaderAt) rawReadAt(p []byte, off int64) (int, error) {
	if b.hr != nil {
		return b.hr.readAtCtx(b.ctx, p, off)
	}
	if cr, ok := b.r.(core.ConcurrentRangeReaderAt); ok {
		return cr.ReadAtConcurrentContext(b.ctx, p, off)
	}
	if bs, ok := b.r.(ByteSource); ok {
		if ms := UnwrapMediaSource(bs); ms != nil {
			if cr, ok := ms.(core.ConcurrentRangeReaderAt); ok {
				return cr.ReadAtConcurrentContext(b.ctx, p, off)
			}
		}
	}
	if cr, ok := b.r.(core.ContextReaderAt); ok {
		return cr.ReadAtContext(b.ctx, p, off)
	}
	return b.r.ReadAt(p, off)
}

func parseContentRangeTotal(hdr string) (int64, error) {
	hdr = strings.TrimSpace(hdr)
	if hdr == "" {
		return 0, errors.New("empty Content-Range")
	}
	slash := strings.LastIndexByte(hdr, '/')
	if slash < 0 || slash+1 >= len(hdr) {
		return 0, fmt.Errorf("malformed Content-Range %q", hdr)
	}
	totalStr := strings.TrimSpace(hdr[slash+1:])
	if totalStr == "*" {
		return 0, errors.New("unknown total in Content-Range")
	}
	total, err := strconv.ParseInt(totalStr, 10, 64)
	if err != nil || total < 0 {
		return 0, fmt.Errorf("invalid Content-Range total %q", totalStr)
	}
	return total, nil
}

func defaultNameFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "stream"
	}
	base := path.Base(u.Path)
	if base == "" || base == "." || base == "/" {
		return "stream"
	}
	return base
}
