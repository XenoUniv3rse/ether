package newznab

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type searchArrivalKey struct{}

// SearchWith delivers complete items while the HTTP body is still arriving.
// Cache/backend answers use the same callback; each ID is emitted once even
// when fallback queries overlap. The cache only receives complete responses.
func (c *Client) SearchWith(ctx context.Context, req core.SearchRequest, arrived func([]core.Release)) ([]core.Release, error) {
	if arrived == nil {
		return c.Search(ctx, req)
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	emit := func(rs []core.Release) {
		if c.Config().UsesBackend() {
			rs = c.bindReleases(rs)
		} else {
			rs = c.feedReleases(rs)
		}
		mu.Lock()
		defer mu.Unlock()
		var batch []core.Release
		for _, r := range rs {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			// Feed results of a torrent indexer are torrents even with only
			// a .torrent link; backends (built-in sites, Stremio addons)
			// label each result themselves, Usenet and HTTP streams included.
			if cfg := c.Config(); cfg.IsTorrent() && !cfg.UsesBackend() {
				r.Protocol = core.ProtocolTorrent
			}
			batch = append(batch, r)
		}
		if len(batch) > 0 {
			arrived(batch)
		}
	}
	results, err := c.Search(context.WithValue(ctx, searchArrivalKey{}, emit), req)
	if err == nil {
		emit(results)
	}
	return results, err
}

func (c *Client) getSearchStream(ctx context.Context, rawURL string, emit func([]core.Release), id config.Identity) ([]core.Release, error) {
	hctx, releaseHost, err := c.acquireHost(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	defer releaseHost()
	// Same SSRF guard as Client.get: the streamed search goes to the
	// owner-configured indexer, but redirects away from it must be public.
	req, err := http.NewRequestWithContext(c.trustedCtx(hctx, rawURL), http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, redactURLError(err)
	}
	c.applyIdentity(req, id)
	resp, err := c.send(req, id)
	if err != nil {
		return nil, redactURLError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, c.recordRateLimit(hctx, resp.Header.Get("Retry-After"), rateLimitResponse(resp, c.now()))
	}
	const limit = 20 << 20
	decoded, derr := decodeBody(resp)
	if derr != nil {
		return nil, derr
	}
	lr := &io.LimitedReader{R: decoded, N: limit + 1}
	head := &headCapture{r: lr, max: 8192}
	if resp.StatusCode != http.StatusOK {
		body, rerr := io.ReadAll(head)
		if rerr != nil {
			return nil, rerr
		}
		if lr.N <= 0 {
			return nil, fmt.Errorf("newznab: response exceeds %d bytes", limit)
		}
		c.noteAPILimit(hctx, head.buf)
		if _, perr := parseFeed(body, c.Name()); perr != nil {
			var ae *APIError
			if errors.As(perr, &ae) {
				if ae.Budget {
					return nil, c.recordRateLimit(hctx, resp.Header.Get("Retry-After"), perr)
				}
				return nil, perr
			}
		}
		return nil, apiError(resp.StatusCode, resp.Status)
	}
	br := bufio.NewReader(head)
	// Sniff without waiting for EOF (XML is the usual Newznab format).
	for {
		ch, e := br.Peek(1)
		if e != nil {
			return nil, e
		}
		if ch[0] != ' ' && ch[0] != '\n' && ch[0] != '\r' && ch[0] != '\t' {
			break
		}
		_, _ = br.ReadByte()
	}
	first, _ := br.Peek(1)
	var rs []core.Release
	if first[0] == '<' {
		rs, err = parseXMLStream(br, c.Name(), emit)
	} else if first[0] == '{' || first[0] == '[' {
		rs, err = parseJSONStream(br, c.Name(), emit)
	} else {
		var body []byte
		body, err = io.ReadAll(br)
		if err == nil {
			rs, err = parseFeed(body, c.Name())
		}
	}
	c.noteAPILimit(hctx, head.buf)
	if lr.N <= 0 {
		return nil, fmt.Errorf("newznab: response exceeds %d bytes", limit)
	}
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Budget {
			return nil, c.recordRateLimit(hctx, resp.Header.Get("Retry-After"), err)
		}
		return nil, err
	}
	return rs, nil
}

// headCapture records up to max bytes of a streamed body so noteAPILimit can
// inspect <newznab:apilimits> without buffering the whole response.
type headCapture struct {
	r   io.Reader
	max int
	buf []byte
}

func (h *headCapture) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if n > 0 && len(h.buf) < h.max {
		take := min(n, h.max-len(h.buf))
		h.buf = append(h.buf, p[:take]...)
	}
	return n, err
}

// Decode complete JSON objects as they arrive, including nested RSS/channel
// envelopes. Final parsing retains the existing error-document semantics.
func parseJSONStream(r io.Reader, indexer string, emit func([]core.Release)) ([]core.Release, error) {
	dec := json.NewDecoder(r)
	var walk func(int) (any, error)
	walk = func(depth int) (any, error) {
		if depth > 64 {
			return nil, fmt.Errorf("newznab: JSON nesting exceeds limit")
		}
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch tok {
		case json.Delim('{'):
			obj := map[string]any{}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := k.(string)
				if !ok {
					return nil, fmt.Errorf("newznab: invalid JSON key")
				}
				v, err := walk(depth + 1)
				if err != nil {
					return nil, err
				}
				obj[key] = v
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			if _, ok := obj["title"]; ok && emit != nil {
				data, err := json.Marshal([]any{obj})
				if err != nil {
					return nil, err
				}
				rs, err := parseJSONFeed(data, indexer)
				if err != nil {
					return nil, err
				}
				emit(rs)
			}
			return obj, nil
		case json.Delim('['):
			var values []any
			for dec.More() {
				v, err := walk(depth + 1)
				if err != nil {
					return nil, err
				}
				values = append(values, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return values, nil
		default:
			return tok, nil
		}
	}
	root, err := walk(0)
	if err != nil {
		return nil, fmt.Errorf("newznab: parse JSON response: %w", err)
	}
	if _, err = dec.Token(); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("trailing JSON data")
		}
		return nil, err
	}
	data, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	return parseJSONFeed(data, indexer)
}
