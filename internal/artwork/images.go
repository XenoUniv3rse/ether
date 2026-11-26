// Package artwork serves and makes Ether's artwork: a caching proxy for
// TMDB and third-party images (memory LRU over a disk cache) and a pure-Go
// poster generator for collections (collage and backdrop treatments with the
// title set in the Go fonts).
package artwork

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/flight"
	"github.com/braedonsaunders/ether/internal/httpclient"
	"github.com/braedonsaunders/ether/internal/lru"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

var (
	// ErrInvalidPath rejects anything that is not a TMDB image path.
	ErrInvalidPath = errors.New("artwork: not a TMDB image path")
	// ErrNotFound means the image does not exist upstream.
	ErrNotFound = errors.New("artwork: image not found")
)

const (
	maxImageBytes = 15 << 20

	negPermanentTTL = 24 * time.Hour
	negInitialTTL   = 15 * time.Minute
	negMaxTTL       = 6 * time.Hour
	hostMaxBackoff  = 24 * time.Hour

	maxNegEntries  = 16384
	maxHostEntries = 4096

	diskMetaMagic = "ETHERIMG1\n"
)

// ValidPath reports whether p is a TMDB image path such as "/abc123.jpg":
// a single file name, so it can never traverse or address another host.
func ValidPath(p string) bool { return core.ValidArtworkPath(p) }

// tmdbWidths are the widths the TMDB CDN serves.
var tmdbWidths = []int{92, 154, 185, 300, 342, 500, 780, 1280}

// Size picks the smallest TMDB size at least w wide ("original" for 0,
// or a width above the largest variant).
func Size(w int) string {
	if w <= 0 {
		return "original"
	}
	for _, s := range tmdbWidths {
		if s >= w {
			return "w" + strconv.Itoa(s)
		}
	}
	return "original"
}

// ImagesOptions configure an image store.
type ImagesOptions struct {
	// Posters returns the current owner settings; nil preserves TMDB.
	Posters func() config.Posters

	// Dir is the disk cache directory ("" = memory only).
	Dir string
	// MemoryBytes bounds the in-memory LRU (default 64 MiB).
	MemoryBytes int64
	// DiskBytes bounds the disk cache (default 512 MiB).
	DiskBytes int64
	// HTTPClient fetches images (default: 20 s timeout).
	HTTPClient *http.Client
	// Guard guards untrusted URL fetches; nil uses a zero safefetch.Guard.
	Guard *safefetch.Guard
	// ImageURL builds TMDB URLs (default image.tmdb.org).
	ImageURL core.ImageURLFunc
	// Now returns the current time (default time.Now).
	Now    func() time.Time
	Logger *slog.Logger
}

// Images is a caching TMDB and third-party image store. Concurrent requests
// for the same image share one fetch. Safe for concurrent use.
type Images struct {
	o             ImagesOptions
	log           *slog.Logger
	guardedClient *http.Client

	mu           sync.Mutex
	items        *lru.Cache[string, *memImage]
	negCache     *lru.Cache[string, *negEntry]
	hostBackoff  *lru.Cache[string, *hostEntry]
	revalidating map[string]bool
	revalCond    *sync.Cond
	closed       bool
	bgCtx        context.Context
	bgCancel     context.CancelFunc

	inflight flight.Group[string, *memImage]

	posterSlots     chan struct{}
	posterHosts     map[string]*posterHost
	posterRefreshes int
	diskMu          sync.Mutex
	diskUsed        int64
	diskKnown       bool
}

type memImage struct {
	key          string
	data         []byte
	ct           string
	etag         string
	lastModified string
	cacheControl string
	expires      time.Time
	hasMeta      bool
}

type negEntry struct {
	err       error
	until     time.Time
	failures  int
	revalOnly bool
}

type hostEntry struct {
	until    time.Time
	failures int
}

type diskImageMeta struct {
	ContentType  string    `json:"contentType,omitempty"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"lastModified,omitempty"`
	CacheControl string    `json:"cacheControl,omitempty"`
	Expires      time.Time `json:"expires"`
}

// NewImages creates an image store.
func NewImages(o ImagesOptions) *Images {
	if o.MemoryBytes <= 0 {
		o.MemoryBytes = 64 << 20
	}
	if o.DiskBytes <= 0 {
		o.DiskBytes = 512 << 20
	}
	if o.HTTPClient == nil {
		o.HTTPClient = egress.Client(egress.CapMetadata, 20*time.Second)
	}
	if o.ImageURL == nil {
		o.ImageURL = func(path, size string) string { return "https://image.tmdb.org/t/p/" + size + path }
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Dir != "" {
		_ = os.MkdirAll(o.Dir, 0o755)
	}
	guard := o.Guard
	if guard == nil {
		guard = &safefetch.Guard{}
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	im := &Images{
		posterSlots:   make(chan struct{}, maxPosterFetches),
		posterHosts:   make(map[string]*posterHost),
		o:             o,
		log:           o.Logger,
		guardedClient: guard.Client(o.HTTPClient),
		items: lru.New(lru.Options[string, *memImage]{MaxCost: o.MemoryBytes, Cost: func(_ string, img *memImage) int64 {
			return int64(len(img.data))
		}}),
		negCache:     lru.New(lru.Options[string, *negEntry]{MaxEntries: maxNegEntries}),
		hostBackoff:  lru.New(lru.Options[string, *hostEntry]{MaxEntries: maxHostEntries}),
		revalidating: make(map[string]bool),
		bgCtx:        bgCtx,
		bgCancel:     bgCancel,
	}
	im.revalCond = sync.NewCond(&im.mu)
	return im
}

func (im *Images) now() time.Time {
	if im.o.Now != nil {
		return im.o.Now()
	}
	return time.Now()
}

// Wait blocks until any in-flight background revalidations finish.
func (im *Images) Wait() {
	if im == nil {
		return
	}
	im.mu.Lock()
	for len(im.revalidating) > 0 {
		im.revalCond.Wait()
	}
	im.mu.Unlock()
}

// Close cancels background revalidations and waits for them to exit.
func (im *Images) Close() {
	if im == nil {
		return
	}
	im.mu.Lock()
	im.closed = true
	im.mu.Unlock()
	im.bgCancel()
	im.Wait()
}

// Get returns a TMDB image at the smallest size covering width w.
// TMDB paths are immutable and are never revalidated once stored.
func (im *Images) Get(ctx context.Context, path string, w int) ([]byte, string, error) {
	if !ValidPath(path) {
		return nil, "", ErrInvalidPath
	}
	size := Size(w)
	return im.getImmutableURL(ctx, size+path, im.o.HTTPClient, im.o.ImageURL(path, size))
}

// InvalidateTMDB removes cached renditions without fetching replacement bytes.
func (im *Images) InvalidateTMDB(paths []string) {
	seen := make(map[string]bool)
	sizes := []string{"original", "h632"}
	for _, width := range tmdbWidths {
		sizes = append(sizes, Size(width))
	}
	for _, path := range paths {
		if !ValidPath(path) || seen[path] {
			continue
		}
		seen[path] = true
		for _, size := range sizes {
			key := size + path
			im.mu.Lock()
			im.items.Remove(key)
			im.negCache.Remove(key)
			im.mu.Unlock()
			if im.o.Dir != "" {
				_ = os.Remove(im.diskPath(key))
			}
		}
	}
}

// GetCachedURL serves an already-authorized remote image URL through the disk
// store. Immutable TMDB image URLs share the same key as Get and are never
// revalidated; other HTTP(S) URLs follow third-party freshness and background
// conditional revalidation.
func (im *Images) GetCachedURL(ctx context.Context, rawURL string) ([]byte, string, error) {
	if !strings.HasPrefix(rawURL, "https://") && !strings.HasPrefix(rawURL, "http://") {
		return nil, "", ErrInvalidPath
	}
	if key, ok := im.tmdbKeyFromURL(rawURL); ok {
		return im.getImmutableURL(ctx, key, im.o.HTTPClient, rawURL)
	}
	return im.GetURL(ctx, rawURL)
}

// GetURL fetches an owner-supplied image URL (custom collection art). Only
// http(s) URLs to images are accepted.
func (im *Images) GetURL(ctx context.Context, rawURL string) ([]byte, string, error) {
	if !strings.HasPrefix(rawURL, "https://") && !strings.HasPrefix(rawURL, "http://") {
		return nil, "", ErrInvalidPath
	}
	sum := sha1.Sum([]byte(rawURL))
	return im.getRevalidatingURL(ctx, "url/"+hex.EncodeToString(sum[:]), im.o.HTTPClient, rawURL)
}

// GetUntrustedURL fetches a URL supplied by a remote feed, preventing it from
// reaching private addresses or rebinding across redirects. Owner-configured
// collection URLs continue to use GetURL and retain their existing behavior.
func (im *Images) GetUntrustedURL(ctx context.Context, rawURL string) ([]byte, string, error) {
	if !strings.HasPrefix(rawURL, "https://") && !strings.HasPrefix(rawURL, "http://") {
		return nil, "", ErrInvalidPath
	}
	sum := sha1.Sum([]byte(rawURL))
	return im.getRevalidatingURL(ctx, "untrusted-url/"+hex.EncodeToString(sum[:]), im.guardedClient, rawURL)
}

func (im *Images) tmdbKeyFromURL(rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	slash := strings.LastIndexByte(u.Path, '/')
	if slash <= 0 {
		return "", false
	}
	file := u.Path[slash:]
	if !ValidPath(file) {
		return "", false
	}
	prefix := u.Path[:slash]
	prev := strings.LastIndexByte(prefix, '/')
	size := prefix[prev+1:]
	if !validTMDBSizeSegment(size) {
		return "", false
	}
	if (im.o.ImageURL != nil && im.o.ImageURL(file, size) == rawURL) || "https://image.tmdb.org/t/p/"+size+file == rawURL {
		return size + file, true
	}
	return "", false
}

func validTMDBSizeSegment(size string) bool {
	if size == "original" {
		return true
	}
	if len(size) < 2 || (size[0] != 'w' && size[0] != 'h') {
		return false
	}
	for i := 1; i < len(size); i++ {
		if size[i] < '0' || size[i] > '9' {
			return false
		}
	}
	return true
}

// GetLoaded caches caller-loaded image bytes (such as Plex viewer avatars) in
// memory and on disk with single-flight coalescing. Load errors are not cached.
func (im *Images) GetLoaded(ctx context.Context, key string, load func(context.Context) ([]byte, string, error)) ([]byte, string, error) {
	im.mu.Lock()
	if img, ok := im.items.Get(key); ok {
		im.mu.Unlock()
		return img.data, img.ct, nil
	}
	im.mu.Unlock()
	img, err := im.inflight.Do(ctx, key, func(ctx context.Context) (*memImage, error) {
		im.mu.Lock()
		if img, ok := im.items.Get(key); ok {
			im.mu.Unlock()
			return img, nil
		}
		im.mu.Unlock()
		if entry, ok := im.readDiskEntry(key); ok {
			im.mu.Lock()
			im.addLocked(entry)
			im.mu.Unlock()
			return entry, nil
		}
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		data, ct, err := load(fctx)
		if err != nil {
			return nil, err
		}
		if err := fctx.Err(); err != nil {
			return nil, err
		}
		if len(data) == 0 {
			return nil, ErrNotFound
		}
		if ct == "" {
			ct = sniff(data)
		}
		entry := &memImage{key: key, data: data, ct: ct, hasMeta: true}
		im.writeDiskEntry(key, entry)
		im.mu.Lock()
		im.addLocked(entry)
		im.mu.Unlock()
		return entry, nil
	})
	if err != nil {
		return nil, "", err
	}
	return img.data, img.ct, nil
}

// get caches local derived bytes (such as resized provider posters) in memory and on disk.
func (im *Images) get(ctx context.Context, key string, load func(context.Context) ([]byte, string, error)) ([]byte, string, error) {
	im.mu.Lock()
	if img, ok := im.items.Get(key); ok {
		im.mu.Unlock()
		return img.data, img.ct, nil
	}
	im.mu.Unlock()
	img, err := im.inflight.Do(ctx, key, func(ctx context.Context) (*memImage, error) {
		im.mu.Lock()
		if img, ok := im.items.Get(key); ok {
			im.mu.Unlock()
			return img, nil
		}
		im.mu.Unlock()
		img, err := im.loadShared(ctx, key, load)
		if err == nil {
			im.mu.Lock()
			im.addLocked(img)
			im.mu.Unlock()
		}
		return img, err
	})
	if err != nil {
		return nil, "", err
	}
	return img.data, img.ct, nil
}

func (im *Images) loadShared(ctx context.Context, key string, load func(context.Context) ([]byte, string, error)) (*memImage, error) {
	if data, ct, ok := im.readDisk(key); ok {
		return &memImage{key: key, data: data, ct: ct}, nil
	}
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, ct, err := load(fctx)
	if err != nil {
		return nil, err
	}
	if err := fctx.Err(); err != nil {
		return nil, err
	}
	im.writeDisk(key, data)
	return &memImage{key: key, data: data, ct: ct}, nil
}

func (im *Images) getImmutableURL(ctx context.Context, key string, client *http.Client, rawURL string) ([]byte, string, error) {
	im.mu.Lock()
	if img, ok := im.items.Get(key); ok {
		im.mu.Unlock()
		return img.data, img.ct, nil
	}
	if err, ok := im.negErrorLocked(key, im.now()); ok {
		im.mu.Unlock()
		return nil, "", err
	}
	im.mu.Unlock()

	img, err := im.inflight.Do(ctx, key, func(ctx context.Context) (*memImage, error) {
		im.mu.Lock()
		if img, ok := im.items.Get(key); ok {
			im.mu.Unlock()
			return img, nil
		}
		if err, ok := im.negErrorLocked(key, im.now()); ok {
			im.mu.Unlock()
			return nil, err
		}
		im.mu.Unlock()

		if data, ct, ok := im.readDisk(key); ok {
			img := &memImage{key: key, data: data, ct: ct}
			im.mu.Lock()
			im.addLocked(img)
			im.mu.Unlock()
			return img, nil
		}

		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		res := im.fetchURL(fctx, client, rawURL, nil, false)
		if res.err != nil {
			if !res.hostPaused && ctx.Err() != context.Canceled {
				im.recordNeg(key, res, im.now(), false)
			}
			return nil, res.err
		}
		im.clearNeg(key)
		img := &memImage{key: key, data: res.data, ct: res.ct}
		im.writeDiskEntry(key, img)
		im.mu.Lock()
		im.addLocked(img)
		im.mu.Unlock()
		return img, nil
	})
	if err != nil {
		return nil, "", err
	}
	return img.data, img.ct, nil
}

func (im *Images) getRevalidatingURL(ctx context.Context, key string, client *http.Client, rawURL string) ([]byte, string, error) {
	host := urlHost(rawURL)
	im.mu.Lock()
	now := im.now()
	if img, ok := im.items.Get(key); ok {
		if !now.Before(img.expires) && !im.backoffActiveLocked(key, host, now) {
			im.startBackgroundRevalidateLocked(key, client, rawURL, img)
		}
		im.mu.Unlock()
		return img.data, img.ct, nil
	}
	if err, ok := im.negErrorLocked(key, now); ok {
		im.mu.Unlock()
		return nil, "", err
	}
	im.mu.Unlock()

	img, err := im.inflight.Do(ctx, key, func(ctx context.Context) (*memImage, error) {
		im.mu.Lock()
		now := im.now()
		if img, ok := im.items.Get(key); ok {
			if !now.Before(img.expires) && !im.backoffActiveLocked(key, host, now) {
				im.startBackgroundRevalidateLocked(key, client, rawURL, img)
			}
			im.mu.Unlock()
			return img, nil
		}
		if err, ok := im.negErrorLocked(key, now); ok {
			im.mu.Unlock()
			return nil, err
		}
		im.mu.Unlock()

		if img, ok := im.readDiskEntry(key); ok {
			im.mu.Lock()
			im.addLocked(img)
			now = im.now()
			if !now.Before(img.expires) && !im.backoffActiveLocked(key, host, now) {
				im.startBackgroundRevalidateLocked(key, client, rawURL, img)
			}
			im.mu.Unlock()
			return img, nil
		}

		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		res := im.fetchURL(fctx, client, rawURL, nil, true)
		if res.err != nil {
			if !res.hostPaused && ctx.Err() != context.Canceled {
				im.recordNeg(key, res, im.now(), false)
			}
			return nil, res.err
		}
		im.clearNeg(key)
		img := &memImage{
			key:          key,
			data:         res.data,
			ct:           res.ct,
			etag:         res.etag,
			lastModified: res.lastModified,
			cacheControl: res.cacheControl,
			expires:      res.expires,
			hasMeta:      true,
		}
		im.writeDiskEntry(key, img)
		im.mu.Lock()
		im.addLocked(img)
		im.mu.Unlock()
		return img, nil
	})
	if err != nil {
		return nil, "", err
	}
	return img.data, img.ct, nil
}

func (im *Images) startBackgroundRevalidateLocked(key string, client *http.Client, rawURL string, old *memImage) {
	if im.closed || im.revalidating[key] {
		return
	}
	im.revalidating[key] = true
	oldCopy := *old
	bgCtx := im.bgCtx
	go func() {
		defer func() {
			im.mu.Lock()
			delete(im.revalidating, key)
			im.revalCond.Broadcast()
			im.mu.Unlock()
		}()
		fctx, cancel := context.WithTimeout(bgCtx, 30*time.Second)
		defer cancel()
		res := im.fetchURL(fctx, client, rawURL, &oldCopy, true)
		if res.err != nil {
			if !res.hostPaused && bgCtx.Err() == nil {
				im.recordNeg(key, res, im.now(), true)
			}
			return
		}
		im.clearNeg(key)
		updated := &memImage{
			key:          key,
			data:         oldCopy.data,
			ct:           oldCopy.ct,
			etag:         res.etag,
			lastModified: res.lastModified,
			cacheControl: res.cacheControl,
			expires:      res.expires,
			hasMeta:      true,
		}
		if !res.notModified {
			updated.data = res.data
			updated.ct = res.ct
		}
		im.writeDiskEntry(key, updated)
		im.mu.Lock()
		im.addLocked(updated)
		im.mu.Unlock()
	}()
}

type fetchResult struct {
	status       int
	notModified  bool
	hostPaused   bool
	permanent    bool
	hasRetry     bool
	retryAfter   time.Time
	data         []byte
	ct           string
	etag         string
	lastModified string
	cacheControl string
	expires      time.Time
	err          error
}

func (im *Images) fetchURL(ctx context.Context, client *http.Client, rawURL string, old *memImage, withValidators bool) fetchResult {
	host := urlHost(rawURL)
	now := im.now()
	if until, paused := im.hostPausedUntil(host, now); paused {
		return fetchResult{
			hostPaused: true,
			err:        fmt.Errorf("artwork: host %s paused until %s", host, until.Format(time.RFC3339)),
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fetchResult{permanent: true, err: err}
	}
	req.Header.Set("User-Agent", httpclient.UserAgent())
	if withValidators && old != nil {
		setConditionalHeaders(req, old.etag, old.lastModified)
	}
	resp, err := client.Do(req)
	now = im.now()
	if err != nil {
		return fetchResult{err: err}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		retryUntil, hasRetry := parseRetryAfter(resp.Header, now, hostMaxBackoff)
		until := im.recordHostBackoff(host, retryUntil, hasRetry, now)
		if resp.Request != nil && resp.Request.URL != nil {
			if finalHost := urlHost(resp.Request.URL.String()); finalHost != "" && finalHost != host {
				im.recordHostBackoff(finalHost, retryUntil, hasRetry, now)
			}
		}
		return fetchResult{
			status:     resp.StatusCode,
			hasRetry:   hasRetry,
			retryAfter: until,
			err:        fmt.Errorf("artwork: fetch %s: %s", rawURL, resp.Status),
		}
	case resp.StatusCode == http.StatusNotModified && withValidators && old != nil:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		im.clearHostBackoff(host)
		etag, lastMod, cc, expires, _ := parseCacheHeaders(
			resp.Header, http.StatusNotModified, old.etag, old.lastModified, old.cacheControl, now, thirdPartyURLPolicy,
		)
		return fetchResult{
			status:       http.StatusNotModified,
			notModified:  true,
			data:         old.data,
			ct:           old.ct,
			etag:         etag,
			lastModified: lastMod,
			cacheControl: cc,
			expires:      expires,
		}
	case resp.StatusCode == http.StatusNotFound:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fetchResult{status: http.StatusNotFound, permanent: true, err: ErrNotFound}
	case resp.StatusCode != http.StatusOK:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fetchResult{status: resp.StatusCode, err: fmt.Errorf("artwork: fetch %s: %s", rawURL, resp.Status)}
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return fetchResult{err: err}
	}
	if len(data) > maxImageBytes {
		return fetchResult{status: http.StatusOK, permanent: true, err: fmt.Errorf("artwork: %s is too large", rawURL)}
	}
	ct := sniff(data)
	if len(data) == 0 || !strings.HasPrefix(ct, "image/") {
		return fetchResult{status: http.StatusOK, permanent: true, err: fmt.Errorf("artwork: %s is not an image", rawURL)}
	}
	im.clearHostBackoff(host)
	res := fetchResult{status: http.StatusOK, data: data, ct: ct}
	if withValidators {
		res.etag, res.lastModified, res.cacheControl, res.expires, _ = parseCacheHeaders(
			resp.Header, http.StatusOK, "", "", "", now, thirdPartyURLPolicy,
		)
	}
	return res
}

func (im *Images) negErrorLocked(key string, now time.Time) (error, bool) {
	e, ok := im.negCache.Peek(key)
	if !ok || e.revalOnly || !now.Before(e.until) {
		return nil, false
	}
	im.negCache.Get(key)
	return e.err, true
}

func (im *Images) backoffActiveLocked(key, host string, now time.Time) bool {
	if _, paused := im.hostPausedLocked(host, now); paused {
		return true
	}
	if e, ok := im.negCache.Peek(key); ok && now.Before(e.until) {
		return true
	}
	return false
}

func (im *Images) recordNeg(key string, res fetchResult, now time.Time, revalOnly bool) {
	im.mu.Lock()
	defer im.mu.Unlock()
	failures := 1
	if prev, ok := im.negCache.Peek(key); ok {
		failures = prev.failures + 1
	}
	var until time.Time
	switch {
	case res.permanent:
		until = now.Add(negPermanentTTL)
		failures = 1
	case res.hasRetry:
		until = res.retryAfter
	default:
		shift := min(failures-1, 6)
		d := min(negInitialTTL<<shift, negMaxTTL)
		until = now.Add(d)
	}
	im.negCache.Add(key, &negEntry{
		err:       res.err,
		until:     until,
		failures:  failures,
		revalOnly: revalOnly,
	})
}

func (im *Images) clearNeg(key string) {
	im.mu.Lock()
	im.negCache.Remove(key)
	im.mu.Unlock()
}

func (im *Images) hostPausedLocked(host string, now time.Time) (time.Time, bool) {
	if host == "" {
		return time.Time{}, false
	}
	e, ok := im.hostBackoff.Peek(host)
	if !ok || !now.Before(e.until) {
		return time.Time{}, false
	}
	im.hostBackoff.Get(host)
	return e.until, true
}

func (im *Images) hostPausedUntil(host string, now time.Time) (time.Time, bool) {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.hostPausedLocked(host, now)
}

func (im *Images) recordHostBackoff(host string, retryUntil time.Time, hasRetry bool, now time.Time) time.Time {
	if host == "" {
		return retryUntil
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	failures := 1
	var prevUntil time.Time
	if prev, ok := im.hostBackoff.Peek(host); ok {
		failures = prev.failures + 1
		prevUntil = prev.until
	}
	until := retryUntil
	if !hasRetry {
		shift := min(failures-1, 6)
		d := min(negInitialTTL<<shift, negMaxTTL)
		until = now.Add(d)
	}
	if prevUntil.After(until) {
		until = prevUntil
	}
	im.hostBackoff.Add(host, &hostEntry{until: until, failures: failures})
	return until
}

func (im *Images) clearHostBackoff(host string) {
	if host == "" {
		return
	}
	im.mu.Lock()
	im.hostBackoff.Remove(host)
	im.mu.Unlock()
}

func urlHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	h := strings.ToLower(u.Host)
	if (u.Scheme == "http" && strings.HasSuffix(h, ":80")) || (u.Scheme == "https" && strings.HasSuffix(h, ":443")) {
		if host, _, err := net.SplitHostPort(h); err == nil {
			if strings.Contains(host, ":") {
				return "[" + host + "]"
			}
			return host
		}
	}
	return h
}

func sniff(data []byte) string {
	ct := http.DetectContentType(data)
	if strings.HasPrefix(ct, "text/xml") || strings.HasPrefix(ct, "text/plain") {
		if strings.Contains(string(data[:min(len(data), 512)]), "<svg") {
			return "image/svg+xml"
		}
	}
	return ct
}

func (im *Images) addLocked(img *memImage) {
	// One image may take at most a quarter of the memory budget.
	if int64(len(img.data)) > im.o.MemoryBytes/4 {
		return
	}
	im.items.Add(img.key, img)
}

// Disk layout: <dir>/<sha1(key)[:2]>/<sha1(key)>, content type sniffed on read.
func (im *Images) diskPath(key string) string {
	sum := sha1.Sum([]byte(key))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(im.o.Dir, h[:2], h)
}

func (im *Images) readDiskRaw(key string) ([]byte, time.Time, bool) {
	if im.o.Dir == "" {
		return nil, time.Time{}, false
	}
	p := im.diskPath(key)
	fi, err := os.Stat(p)
	if err != nil || fi.Size() == 0 {
		return nil, time.Time{}, false
	}
	mod := fi.ModTime()
	data, err := os.ReadFile(p)
	if err != nil || len(data) == 0 {
		return nil, time.Time{}, false
	}
	now := im.now()
	_ = os.Chtimes(p, now, now) // LRU by modification time
	return data, mod, true
}

func (im *Images) readDisk(key string) ([]byte, string, bool) {
	data, _, ok := im.readDiskRaw(key)
	if !ok {
		return nil, "", false
	}
	return data, sniff(data), true
}

func (im *Images) readDiskEntry(key string) (*memImage, bool) {
	raw, mod, ok := im.readDiskRaw(key)
	if !ok {
		return nil, false
	}
	if bytes.HasPrefix(raw, []byte(diskMetaMagic)) {
		rest := raw[len(diskMetaMagic):]
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			return nil, false
		}
		var meta diskImageMeta
		if json.Unmarshal(rest[:nl], &meta) != nil {
			return nil, false
		}
		body := rest[nl+1:]
		if len(body) == 0 {
			return nil, false
		}
		ct := meta.ContentType
		if ct == "" {
			ct = sniff(body)
		}
		return &memImage{
			key:          key,
			data:         body,
			ct:           ct,
			etag:         meta.ETag,
			lastModified: meta.LastModified,
			cacheControl: meta.CacheControl,
			expires:      meta.Expires,
			hasMeta:      true,
		}, true
	}
	return &memImage{
		key:     key,
		data:    raw,
		ct:      sniff(raw),
		expires: mod.Add(thirdPartyURLPolicy.Floor),
	}, true
}

func (im *Images) writeDiskEntry(key string, img *memImage) {
	if !img.hasMeta {
		im.writeDisk(key, img.data)
		return
	}
	metaBytes, err := json.Marshal(diskImageMeta{
		ContentType:  img.ct,
		ETag:         img.etag,
		LastModified: img.lastModified,
		CacheControl: img.cacheControl,
		Expires:      img.expires,
	})
	if err != nil {
		return
	}
	buf := make([]byte, 0, len(diskMetaMagic)+len(metaBytes)+1+len(img.data))
	buf = append(buf, diskMetaMagic...)
	buf = append(buf, metaBytes...)
	buf = append(buf, '\n')
	buf = append(buf, img.data...)
	im.writeDisk(key, buf)
}

func (im *Images) writeDisk(key string, data []byte) {
	if im.o.Dir == "" {
		return
	}
	p := im.diskPath(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	im.diskMu.Lock()
	defer im.diskMu.Unlock()
	var oldSize int64
	if fi, err := os.Stat(p); err == nil {
		oldSize = fi.Size()
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return
	}
	if !im.diskKnown {
		im.diskUsed = dirSize(im.o.Dir)
		im.diskKnown = true
	} else {
		im.diskUsed += int64(len(data)) - oldSize
	}
	if im.diskUsed > im.o.DiskBytes {
		im.diskUsed = trimDir(im.o.Dir, im.o.DiskBytes*9/10)
	}
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// trimDir deletes the least recently used files until the directory is
// under target bytes, and returns the new size.
func trimDir(dir string, target int64) int64 {
	type file struct {
		path string
		size int64
		mod  time.Time
	}
	var files []file
	var total int64
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				files = append(files, file{p, fi.Size(), fi.ModTime()})
				total += fi.Size()
			}
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= target {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
	return total
}
