package newznab

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// maxTorrentBytes bounds a downloaded .torrent document.
const maxTorrentBytes = 10 << 20

// errNotTorrent reports a .torrent download that was neither bencode nor a
// magnet redirect.
var errNotTorrent = errors.New("newznab: response is not a torrent file")

// NormalizeInfoHash returns a lower-case 40-character hex BitTorrent v1
// info hash, converting a 32-character base32 hash; "" when s is neither.
func NormalizeInfoHash(s string) string {
	s = strings.TrimSpace(s)
	switch len(s) {
	case 40:
		if _, err := hex.DecodeString(s); err == nil {
			return strings.ToLower(s)
		}
	case 32:
		if b, err := base32.StdEncoding.DecodeString(strings.ToUpper(s)); err == nil && len(b) == 20 {
			return hex.EncodeToString(b)
		}
	}
	return ""
}

// InfoHashFromMagnet extracts the v1 info hash from a magnet URI's
// xt=urn:btih: parameter; "" when absent.
func InfoHashFromMagnet(magnet string) string {
	if !strings.HasPrefix(strings.ToLower(magnet), "magnet:") {
		return ""
	}
	values := MagnetValues(magnet)
	for _, xt := range values["xt"] {
		if lower := strings.ToLower(xt); strings.HasPrefix(lower, "urn:btih:") {
			if h := NormalizeInfoHash(xt[len("urn:btih:"):]); h != "" {
				return h
			}
		}
	}
	return ""
}

// MagnetValues parses magnet query fields, tolerating malformed unrelated fields.
func MagnetValues(magnet string) url.Values {
	q := magnet[strings.IndexByte(magnet, ':')+1:]
	q = strings.TrimPrefix(q, "?")
	values, err := url.ParseQuery(q)
	if err != nil {
		values = url.Values{}
		for _, part := range strings.Split(q, "&") {
			if k, v, ok := strings.Cut(part, "="); ok {
				if uv, uerr := url.QueryUnescape(v); uerr == nil {
					v = uv
				}
				values.Add(k, v)
			}
		}
	}
	return values
}

// BareMagnet builds a trackerless magnet URI containing only the normalized
// v1 info hash.
func BareMagnet(infoHash string) string {
	h := NormalizeInfoHash(infoHash)
	if h == "" {
		return ""
	}
	return "magnet:?xt=urn:btih:" + h
}

// MagnetFor builds a magnet URI for an info hash.
func MagnetFor(infoHash, name string, trackers []string) string {
	v := "magnet:?xt=urn:btih:" + infoHash
	if name != "" {
		v += "&dn=" + url.QueryEscape(name)
	}
	for _, tr := range trackers {
		v += "&tr=" + url.QueryEscape(tr)
	}
	return v
}

// TorrentMeta is what a .torrent file contributes to a release.
type TorrentMeta struct {
	InfoHash string
	Name     string
	Trackers []string
	Private  bool
	Files    []core.ReleaseFile
}

// ParseTorrent computes the info hash (SHA-1 of the bencoded info
// dictionary) and reads the name, trackers and file list of a .torrent file.
func ParseTorrent(data []byte) (TorrentMeta, error) {
	d := &bdecoder{b: data}
	if d.peek() != 'd' {
		return TorrentMeta{}, errNotTorrent
	}
	d.pos++
	var meta TorrentMeta
	for d.peek() != 'e' {
		key, err := d.str()
		if err != nil {
			return TorrentMeta{}, err
		}
		start := d.pos
		switch string(key) {
		case "info":
			if err := d.skip(0); err != nil {
				return TorrentMeta{}, err
			}
			sum := sha1.Sum(data[start:d.pos])
			meta.InfoHash = hex.EncodeToString(sum[:])
			meta.Name, meta.Private, meta.Files = parseTorrentInfo(data[start:d.pos])
		case "announce":
			v, err := d.str()
			if err != nil {
				return TorrentMeta{}, err
			}
			meta.Trackers = appendTracker(meta.Trackers, string(v))
		case "announce-list":
			tiers, err := d.list()
			if err != nil {
				return TorrentMeta{}, err
			}
			for _, tier := range tiers {
				if trs, ok := tier.([]any); ok {
					for _, tr := range trs {
						if s, ok := tr.(string); ok {
							meta.Trackers = appendTracker(meta.Trackers, s)
						}
					}
				}
			}
		default:
			if err := d.skip(0); err != nil {
				return TorrentMeta{}, err
			}
		}
	}
	if meta.InfoHash == "" {
		return TorrentMeta{}, fmt.Errorf("%w: no info dictionary", errNotTorrent)
	}
	return meta, nil
}

func appendTracker(list []string, tr string) []string {
	if tr == "" || len(list) >= 20 {
		return list
	}
	for _, x := range list {
		if x == tr {
			return list
		}
	}
	return append(list, tr)
}

type torrentFileEntry struct {
	length   int64
	path     []string
	pathUTF8 []string
	pad      bool
}

func parseTorrentInfo(info []byte) (name string, private bool, files []core.ReleaseFile) {
	d := &bdecoder{b: info}
	if d.peek() != 'd' {
		return "", false, nil
	}
	d.pos++
	var (
		nameUTF8        string
		singleLength    int64
		hasSingleLength bool
		multi           []torrentFileEntry
	)
parseLoop:
	for d.peek() == '0' || (d.peek() >= '1' && d.peek() <= '9') {
		key, err := d.str()
		if err != nil {
			break
		}
		switch string(key) {
		case "name":
			v, err := d.str()
			if err != nil {
				break parseLoop
			}
			name = string(v)
		case "name.utf-8":
			v, err := d.str()
			if err != nil {
				break parseLoop
			}
			nameUTF8 = string(v)
		case "private":
			if n, ok := d.integer(); ok && n == 1 {
				private = true
			}
		case "length":
			if n, ok := d.integer(); ok && n >= 0 {
				singleLength = n
				hasSingleLength = true
			}
		case "files":
			if entries, ok := d.torrentFiles(); ok {
				multi = entries
			}
		default:
			if err := d.skip(0); err != nil {
				break parseLoop
			}
		}
	}
	if nameUTF8 != "" {
		name = nameUTF8
	}
	if len(multi) > 0 {
		files = make([]core.ReleaseFile, 0, len(multi))
		for _, entry := range multi {
			if entry.pad {
				continue
			}
			parts := entry.pathUTF8
			if len(parts) == 0 {
				parts = entry.path
			}
			if len(parts) == 0 || isTorrentPadPath(parts) {
				continue
			}
			clean := make([]string, 0, len(parts)+1)
			if name != "" {
				clean = append(clean, name)
			}
			for _, seg := range parts {
				if seg != "" {
					clean = append(clean, seg)
				}
			}
			if len(clean) == 0 {
				continue
			}
			files = append(files, core.ReleaseFile{
				Index: len(files),
				Path:  strings.Join(clean, "/"),
				Size:  entry.length,
			})
		}
	} else if hasSingleLength && singleLength > 0 && name != "" {
		files = []core.ReleaseFile{{Index: 0, Path: name, Size: singleLength}}
	}
	return name, private, files
}

func isTorrentPadPath(parts []string) bool {
	for _, seg := range parts {
		if strings.HasPrefix(seg, ".____padding_file") || strings.HasPrefix(seg, "_____padding_file") {
			return true
		}
	}
	return false
}

func (d *bdecoder) torrentFiles() ([]torrentFileEntry, bool) {
	if d.peek() != 'l' {
		_ = d.skip(0)
		return nil, false
	}
	d.pos++
	var out []torrentFileEntry
	for d.peek() != 'e' {
		if d.peek() == 0 {
			return out, false
		}
		if d.peek() != 'd' {
			if err := d.skip(0); err != nil {
				return out, false
			}
			continue
		}
		d.pos++
		var entry torrentFileEntry
		for d.peek() != 'e' {
			if d.peek() == 0 {
				return out, false
			}
			k, err := d.str()
			if err != nil {
				return out, false
			}
			switch string(k) {
			case "length":
				if n, ok := d.integer(); ok && n >= 0 {
					entry.length = n
				}
			case "path":
				list, err := d.list()
				if err != nil {
					return out, false
				}
				for _, item := range list {
					if s, ok := item.(string); ok {
						entry.path = append(entry.path, s)
					}
				}
			case "path.utf-8":
				list, err := d.list()
				if err != nil {
					return out, false
				}
				for _, item := range list {
					if s, ok := item.(string); ok {
						entry.pathUTF8 = append(entry.pathUTF8, s)
					}
				}
			case "attr":
				v, err := d.str()
				if err != nil {
					return out, false
				}
				if strings.Contains(string(v), "p") {
					entry.pad = true
				}
			default:
				if err := d.skip(0); err != nil {
					return out, false
				}
			}
		}
		d.pos++ // closing 'e' of file dict
		out = append(out, entry)
	}
	d.pos++ // closing 'e' of files list
	return out, true
}

// bdecoder is a minimal bencode reader: enough to find the info dictionary
// and read string lists.
type bdecoder struct {
	b   []byte
	pos int
}

var errBencode = fmt.Errorf("%w: malformed bencode", errNotTorrent)

func (d *bdecoder) peek() byte {
	if d.pos >= len(d.b) {
		return 0
	}
	return d.b[d.pos]
}

func (d *bdecoder) integer() (int64, bool) {
	if d.peek() != 'i' {
		_ = d.skip(0)
		return 0, false
	}
	end := bytes.IndexByte(d.b[d.pos:], 'e')
	if end < 2 {
		d.pos = len(d.b)
		return 0, false
	}
	n, err := strconv.ParseInt(string(d.b[d.pos+1:d.pos+end]), 10, 64)
	d.pos += end + 1
	return n, err == nil
}

func (d *bdecoder) str() ([]byte, error) {
	colon := bytes.IndexByte(d.b[d.pos:], ':')
	if colon <= 0 || colon > 10 {
		return nil, errBencode
	}
	n, err := strconv.Atoi(string(d.b[d.pos : d.pos+colon]))
	if err != nil || n < 0 {
		return nil, errBencode
	}
	start := d.pos + colon + 1
	if start+n > len(d.b) {
		return nil, errBencode
	}
	d.pos = start + n
	return d.b[start:d.pos], nil
}

func (d *bdecoder) list() ([]any, error) {
	v, err := d.value(0)
	if err != nil {
		return nil, err
	}
	l, ok := v.([]any)
	if !ok {
		return nil, errBencode
	}
	return l, nil
}

// value decodes strings and lists of strings (other values decode to nil).
func (d *bdecoder) value(depth int) (any, error) {
	if depth > 32 {
		return nil, errBencode
	}
	switch c := d.peek(); {
	case c == 'l':
		d.pos++
		var out []any
		for d.peek() != 'e' {
			if d.peek() == 0 {
				return nil, errBencode
			}
			v, err := d.value(depth + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		d.pos++
		return out, nil
	case c >= '0' && c <= '9':
		s, err := d.str()
		return string(s), err
	default:
		return nil, d.skip(depth)
	}
}

// skip advances past one value of any type.
func (d *bdecoder) skip(depth int) error {
	if depth > 64 {
		return errBencode
	}
	switch c := d.peek(); {
	case c == 'i':
		end := bytes.IndexByte(d.b[d.pos:], 'e')
		if end < 0 {
			return errBencode
		}
		d.pos += end + 1
		return nil
	case c == 'l' || c == 'd':
		d.pos++
		for d.peek() != 'e' {
			if d.peek() == 0 {
				return errBencode
			}
			if c == 'd' {
				if _, err := d.str(); err != nil {
					return err
				}
			}
			if err := d.skip(depth + 1); err != nil {
				return err
			}
		}
		d.pos++
		return nil
	case c >= '0' && c <= '9':
		_, err := d.str()
		return err
	}
	return errBencode
}

// knownTorrent completes a release that already identifies its torrent.
func knownTorrent(r core.Release) (core.Release, bool) {
	if h := NormalizeInfoHash(r.InfoHash); h != "" {
		r.InfoHash = h
		if r.Private {
			r.MagnetURL = BareMagnet(h)
		} else if r.MagnetURL == "" {
			r.MagnetURL = MagnetFor(h, r.Name, nil)
		}
		return r, true
	}
	if h := InfoHashFromMagnet(r.MagnetURL); h != "" {
		r.InfoHash = h
		if r.Private {
			r.MagnetURL = BareMagnet(h)
		}
		return r, true
	}
	return r, false
}

// ResolveTorrent fills a torrent release's InfoHash and MagnetURL by
// downloading its .torrent link (counting as a grab) when the indexer only
// supplied a download URL. Releases that already carry both are returned
// unchanged without a request.
func (c *Client) ResolveTorrent(ctx context.Context, r core.Release) (core.Release, error) {
	out, _, err := c.resolveTorrentCached(ctx, r)
	return out, err
}

func (c *Client) resolveTorrentCached(ctx context.Context, r core.Release) (core.Release, bool, error) {
	if known, ok := knownTorrent(r); ok {
		return known, true, nil
	}
	key := torrentDescriptorKey(c.Config(), r)
	if hash, magnet, priv, files, ok := c.torrents.get(key, c.now()); ok {
		r.InfoHash, r.MagnetURL, r.Private = hash, magnet, r.Private || priv
		if len(files) > 0 {
			r.TorrentFiles = files
		}
		if r.Private {
			r.MagnetURL = BareMagnet(r.InfoHash)
		}
		c.emit(Event{Kind: "cache", Purpose: usagePurpose(ctx), Success: true, Cached: true})
		return r, true, nil
	}
	if err := ctx.Err(); err != nil {
		return r, false, err
	}
	started := time.Now()
	out, reserved, err := c.resolveTorrent(ctx, r)
	if err == nil && out.InfoHash != "" {
		c.torrents.put(key, out.InfoHash, out.MagnetURL, out.Private, out.TorrentFiles, c.now())
	}
	c.emitGrab(ctx, r.Name, started, reserved, err, false)
	return out, false, err
}

func (c *Client) resolveTorrent(ctx context.Context, r core.Release) (core.Release, bool, error) {
	if cfg := c.Config(); cfg.UsesBackend() {
		b, err := c.backend()
		if err != nil {
			return r, false, err
		}
		hctx := ctx
		if cfg.IsStremio() {
			var releaseHost func()
			hctx, releaseHost, err = c.acquireHost(ctx, cfg.URL)
			if err != nil {
				return r, false, err
			}
			defer releaseHost()
		}
		fetchRel := r
		if fetchRel.NZBURL != "" {
			target := fetchRel.NZBURL
			if !strings.Contains(target, "://") {
				resolved, rerr := c.resolveLink(target)
				if rerr != nil {
					return r, false, rerr
				}
				target = resolved
			}
			authURL, aerr := c.authenticateLink(target)
			if aerr != nil {
				return r, false, aerr
			}
			fetchRel.NZBURL = authURL
		}
		reserved := false
		out, err := b.ResolveTorrent(hctx, fetchRel, func(rctx context.Context, kind string) error {
			if err := c.reserve(rctx, kind); err != nil {
				return err
			}
			reserved = true
			return nil
		})
		if err == nil && r.NZBURL != fetchRel.NZBURL && out.NZBURL == fetchRel.NZBURL {
			out.NZBURL = r.NZBURL
		}
		return out, reserved, err
	}
	if r.NZBURL == "" {
		return r, false, fmt.Errorf("newznab: torrent %q has no info hash, magnet or download URL: %w", r.Name, core.ErrReleaseRejected)
	}
	rawURL, err := c.resolveLink(r.NZBURL)
	if err != nil {
		return r, false, err
	}
	rawURL, err = c.authenticateLink(rawURL)
	if err != nil {
		return r, false, err
	}
	hctx, releaseHost, err := c.acquireHost(ctx, rawURL)
	if err != nil {
		return r, false, err
	}
	defer releaseHost()
	// The download URL comes from the feed (untrusted): fetch it through
	// the safefetch guard, like FetchNZB. Only the owner-configured
	// indexer origin may be private; redirects away from it (other than
	// magnet, handled below) are revalidated at dial time.
	id := c.identityFor(config.RequestTorrentDownload, rawURL, c.releaseMediaType(r))
	rawURL = id.ApplyToURL(rawURL)
	req, err := http.NewRequestWithContext(c.trustedCtx(hctx, rawURL), http.MethodGet, rawURL, nil)
	if err != nil {
		return r, false, redactURLError(err)
	}
	c.applyIdentity(req, id)
	origin := c.identityOrigin(req.URL.String(), id)
	// Torznab download links commonly redirect to a magnet URI, which
	// net/http cannot follow: stop there and read the Location header.
	hc := *c.guarded
	inner := hc.CheckRedirect
	hc.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.URL.Scheme == "magnet" {
			return http.ErrUseLastResponse
		}
		c.dropForeignIdentityHeaders(next, origin, id)
		if inner != nil {
			return inner(next, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	if hc.Timeout == 0 {
		hc.Timeout = defaultHTTPTimeout
	}
	if err := c.reserve(hctx, "grab"); err != nil {
		return r, false, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		// url.Error embeds the URL, which can carry the indexer API key.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return r, true, fmt.Errorf("newznab: download torrent for %q: %w", r.Name, err)
	}
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode/100 == 3 && strings.HasPrefix(strings.ToLower(loc), "magnet:") {
		if h := InfoHashFromMagnet(loc); h != "" {
			r.InfoHash = h
			if r.Private {
				r.MagnetURL = BareMagnet(h)
			} else {
				r.MagnetURL = loc
			}
			return r, true, nil
		}
		return r, true, fmt.Errorf("newznab: torrent %q redirected to a magnet without an info hash: %w", r.Name, core.ErrReleaseRejected)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// Honor the throttle like API requests do, so the next play does
		// not immediately hit the throttled indexer again.
		return r, true, c.recordRateLimit(hctx, resp.Header.Get("Retry-After"), rateLimitResponse(resp, c.now()))
	}
	decoded, derr := decodeBody(resp)
	if derr != nil {
		return r, true, derr
	}
	body, err := io.ReadAll(io.LimitReader(decoded, maxTorrentBytes+1))
	if err != nil {
		return r, true, err
	}
	if len(body) > maxTorrentBytes {
		return r, true, fmt.Errorf("newznab: torrent exceeds %d bytes", maxTorrentBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return r, true, c.torrentResponseError(hctx, resp, body)
	}
	if trimmed := bytes.TrimSpace(body); bytes.HasPrefix(bytes.ToLower(trimmed), []byte("magnet:")) {
		if h := InfoHashFromMagnet(string(trimmed)); h != "" {
			r.InfoHash = h
			if r.Private {
				r.MagnetURL = BareMagnet(h)
			} else {
				r.MagnetURL = string(trimmed)
			}
			return r, true, nil
		}
	}
	meta, err := ParseTorrent(body)
	if err != nil {
		if apiErr := c.torrentResponseError(hctx, resp, body); apiErr != nil {
			return r, true, apiErr
		}
		return r, true, fmt.Errorf("newznab: torrent %q: %w", r.Name, err)
	}
	r.InfoHash = meta.InfoHash
	r.Private = r.Private || meta.Private
	r.TorrentFiles = meta.Files
	if r.Private {
		r.MagnetURL = BareMagnet(meta.InfoHash)
		return r, true, nil
	}
	name := meta.Name
	if name == "" {
		name = r.Name
	}
	r.MagnetURL = MagnetFor(meta.InfoHash, name, meta.Trackers)
	return r, true, nil
}

func (c *Client) torrentResponseError(ctx context.Context, resp *http.Response, body []byte) error {
	if _, err := parseFeed(body, c.Name()); err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			if ae.Budget {
				return c.recordRateLimit(ctx, resp.Header.Get("Retry-After"), ae)
			}
			return ae
		}
	}
	if resp.StatusCode != http.StatusOK {
		return apiError(resp.StatusCode, resp.Status)
	}
	return nil
}

// ResolveTorrent implements the debrid indexer's torrent resolution by
// routing to the release's originating indexer.
func (m *Manager) ResolveTorrent(ctx context.Context, r core.Release) (core.Release, error) {
	if known, ok := knownTorrent(r); ok {
		return known, nil
	}
	cl, err := m.releaseClient(r)
	if err != nil {
		return r, err
	}
	out, _, err := cl.resolveTorrentCached(ctx, r)
	return out, err
}
