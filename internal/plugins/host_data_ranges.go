package plugins

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
)

var errFetchCacheRangeFallback = errors.New("plugins: fetchToCache cannot use parallel ranges")

func (h *host) fetchCacheConnections(ctx context.Context, req FetchCacheRequest) int {
	if req.Connections <= 1 || !h.allowsLongFetchToCache(ctx) {
		return 1
	}
	for key := range req.Headers {
		if strings.EqualFold(key, "Range") {
			return 1
		}
	}
	cap := DefaultMaxFetchPerHost
	if limiter := h.m.fetchHosts; limiter != nil && limiter.limit > 0 {
		cap = limiter.limit
	}
	return min(req.Connections, DefaultMaxFetchPerHost, cap)
}

func canFetchCacheRanges(resp *http.Response, meta blobcache.PartialMeta) bool {
	if resp.StatusCode != http.StatusOK || meta.ContentLength < 2 || resp.Request == nil || resp.Request.URL == nil {
		return false
	}
	acceptsBytes := false
	for _, value := range resp.Header.Values("Accept-Ranges") {
		for _, unit := range strings.Split(value, ",") {
			acceptsBytes = acceptsBytes || strings.EqualFold(strings.TrimSpace(unit), "bytes")
		}
	}
	ifRange := ifRangeHeaderValue(meta)
	if !acceptsBytes || ifRange == "" {
		return false
	}
	if meta.ETag != "" {
		return ifRange == meta.ETag
	}
	// HTTP permits a date in If-Range only when it is a strong validator.
	modified, modErr := http.ParseTime(meta.LastModified)
	date, dateErr := http.ParseTime(resp.Header.Get("Date"))
	return modErr == nil && dateErr == nil && date.Sub(modified) >= time.Minute
}

func fetchCacheRangeSource(resp *http.Response) string {
	// Store the resource identity without persisting a potentially signed URL.
	return fmt.Sprintf("%x", sha256.Sum256([]byte(resp.Request.URL.String())))
}

func matchesFetchCacheRangeSource(meta blobcache.PartialMeta, source string, headers http.Header) bool {
	if meta.RangeSource == source {
		return validatorsMatch(meta, headers)
	}
	// Replicas may have different modification dates. Across URLs, only a
	// matching strong ETag and total length (checked by the caller) suffice.
	return meta.ETag != "" && ifRangeHeaderValue(meta) == meta.ETag &&
		strings.TrimSpace(headers.Get("ETag")) == meta.ETag
}

func matchesFetchCacheRanges(saved, current blobcache.PartialMeta, headers http.Header) bool {
	return len(saved.RangeProgress) == 0 ||
		(saved.ContentLength == current.ContentLength && ifRangeHeaderValue(saved) == ifRangeHeaderValue(current) &&
			matchesFetchCacheRangeSource(saved, current.RangeSource, headers))
}

func (h *host) fetchCacheRanges(
	ctx context.Context, req FetchCacheRequest, attempt FetchRequest, partial *blobcache.Partial,
	current blobcache.PartialMeta, connections int,
) (FetchCacheResult, int64, error) {
	saved := partial.Meta()
	if len(saved.RangeProgress) == 0 {
		current.RangeProgress = make([]int64, min(int64(connections), current.ContentLength))
		if err := partial.Reset(current); err != nil {
			return FetchCacheResult{}, 0, err
		}
		saved = current
	}
	// The host call already paid for the initial GET. Charge each additional
	// range request against the same budget used for other plugin HTTP fan-out.
	var pending int64
	for i, written := range saved.RangeProgress {
		start, end := saved.RangeBounds(i)
		if written < end-start {
			pending++
		}
	}
	if err := wasm.ChargeUnits(ctx, pending*wasm.FetchManyUnitsPerRequest); err != nil {
		return FetchCacheResult{}, 0, err
	}
	if err := partial.Reserve(saved.ContentLength); err != nil {
		return FetchCacheResult{}, 0, err
	}

	rangeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		bytes int64
		err   error
	}
	results := make([]result, len(saved.RangeProgress))
	workers := min(connections, len(results))
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Go(func() {
			for i := worker; i < len(results) && rangeCtx.Err() == nil; i += workers {
				start, end := saved.RangeBounds(i)
				start += saved.RangeProgress[i]
				if start == end {
					continue
				}
				n, err := h.fetchCacheRange(rangeCtx, attempt, partial, current, i, start, end)
				if err == nil && req.Resume {
					err = partial.Sync()
				}
				results[i] = result{n, err}
				if errors.Is(err, errFetchCacheRangeFallback) {
					cancel()
				}
			}
		})
	}
	wg.Wait()
	var transferred int64
	var firstErr error
	for _, r := range results {
		transferred += r.bytes
		if r.err != nil && (firstErr == nil || errors.Is(r.err, errFetchCacheRangeFallback)) {
			firstErr = r.err
		}
	}
	if firstErr != nil {
		return FetchCacheResult{}, transferred, firstErr
	}
	committed, err := partial.Commit(ctx, req.Name)
	if err != nil {
		return FetchCacheResult{}, transferred, err
	}
	return FetchCacheResult{
		Blob: committed.Ref.String(), Size: committed.Size, CRC32: committed.CRC32,
		MD5: committed.MD5, SHA1: committed.SHA1, Status: http.StatusOK,
	}, transferred, nil
}

type cacheRangeWriter struct {
	partial *blobcache.Partial
	index   int
}

func (w cacheRangeWriter) Write(b []byte) (int, error) {
	return w.partial.WriteRange(w.index, b)
}

func (h *host) fetchCacheRange(
	ctx context.Context, req FetchRequest, partial *blobcache.Partial, meta blobcache.PartialMeta,
	index int, start, end int64,
) (int64, error) {
	headers := make(map[string]string, len(req.Headers)+2)
	for key, value := range req.Headers {
		if !strings.EqualFold(key, "If-Range") {
			headers[key] = value
		}
	}
	headers["Range"] = fmt.Sprintf("bytes=%d-%d", start, end-1)
	headers["If-Range"] = ifRangeHeaderValue(meta)
	req.Headers = headers
	resp, err := h.openFetchWithOptions(ctx, req, fetchOptions{applyHostLimit: true, dedicatedConnection: true})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK, http.StatusPreconditionFailed, http.StatusRequestedRangeNotSatisfiable:
		return 0, errFetchCacheRangeFallback
	default:
		return 0, fmt.Errorf("plugins: fetchToCache HTTP %d", resp.StatusCode)
	}
	cr, ok := parseContentRange(resp.Header.Get("Content-Range"))
	length := end - start
	if !ok || cr.start != start || cr.end != end-1 || cr.total != meta.ContentLength ||
		(resp.ContentLength >= 0 && resp.ContentLength != length) || isCompressedResponse(resp) ||
		resp.Request == nil || resp.Request.URL == nil || !matchesFetchCacheRangeSource(meta, fetchCacheRangeSource(resp), resp.Header) {
		return 0, errFetchCacheRangeFallback
	}

	// Hold back the last byte until EOF confirms the range is exact. A
	// concurrent checkpoint can then never record an unverified full range.
	reader := &boundedDataReader{ctx: ctx, r: resp.Body, left: length}
	writer := cacheRangeWriter{partial, index}
	n, err := io.CopyBuffer(writer, io.LimitReader(reader, length-1), make([]byte, 64<<10))
	if err != nil {
		return reader.size, err
	}
	if n != length-1 {
		return reader.size, io.ErrUnexpectedEOF
	}
	tail, err := io.ReadAll(reader) // The bounded reader can return at most two bytes here.
	if errors.Is(err, errDataExceedsSizeLimit) {
		return reader.size, errFetchCacheRangeFallback
	}
	if err != nil {
		return reader.size, err
	}
	if len(tail) != 1 {
		return reader.size, io.ErrUnexpectedEOF
	}
	_, err = writer.Write(tail)
	return reader.size, err
}
