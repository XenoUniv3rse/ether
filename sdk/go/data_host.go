package etherplugin

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
)

// BlobHost provides bounded data services. RemoteHostClient implements it;
// obtain it with ctx.Host.(etherplugin.BlobHost) when starting from Host.
// The host enforces network grants and the plugin's library cache quota.
const (
	MinDataReadBytes    int64 = 8 << 20
	MaxDataReadBytes    int64 = 512 << 20
	MaxArchiveReadItems       = 4096
)

type BlobHost interface {
	RuntimeLimits(context.Context) (RuntimeLimits, error)
	FetchToCache(context.Context, FetchCacheRequest) (FetchCacheResult, error)
	CacheRead(context.Context, string, DataRange) ([]byte, error)
	CacheRasterize(context.Context, string, string, string, RasterizeOptions) (RasterizeResult, error)
	ArchiveList(context.Context, string) ([]ArchiveMember, error)
	ArchiveOpen(context.Context, string, int, DataRange) ([]byte, error)
	ArchiveRead(context.Context, string, []ArchiveReadItem) ([]ArchiveReadResult, error)
	ArchiveExtract(context.Context, string, int, ...ArchiveExtractOptions) (ArchiveExtractResult, error)
}

type DataRange struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}
type ArchiveReadItem struct {
	Member int   `json:"member"`
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}
type ArchiveReadResult struct {
	Index  int    `json:"index"`
	Member int    `json:"member"`
	Size   int    `json:"size"`
	Body   []byte `json:"body"`
}

// FetchCacheRequest streams a remote GET response into the plugin's own
// library blob cache. TimeoutMS defaults to 10 minutes (which is also the
// ceiling on viewer and interactive request paths); in a job, schedule, or
// background context TimeoutMS may extend up to the calling job's remaining
// deadline. Setting Resume to true continues an incomplete partial download
// using Range: bytes=<cached>- when the server supports ranges and ETag or
// Last-Modified validators still match, restarting cleanly from byte 0 if they
// do not; the blob becomes readable in cache only once complete. Connections
// defaults to 1. Jobs, schedules, and background calls can request up to 8
// concurrent ranges, subject to the host cap. Resume preserves each range's
// progress when parallel ranges are in use.
type FetchCacheRequest struct {
	TimeoutMS             int64             `json:"timeoutMs,omitempty"`
	URL                   string            `json:"url"`
	Headers               map[string]string `json:"headers,omitempty"`
	KeepHeadersOnRedirect []string          `json:"keepHeadersOnRedirect,omitempty"`
	LibraryKey            string            `json:"libraryKey"`
	Name                  string            `json:"name"`
	Resume                bool              `json:"resume,omitempty"`
	Connections           int               `json:"connections,omitempty"`
}
type FetchCacheResult struct {
	Blob   string `json:"blob"`
	Size   int64  `json:"size"`
	CRC32  string `json:"crc32"`
	MD5    string `json:"md5"`
	SHA1   string `json:"sha1"`
	Status int    `json:"status"`
}
type ArchiveMember struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Size  uint64 `json:"size"`
	// Unsafe names identify members only and must never be used as output paths.
	Unsafe       bool   `json:"unsafe,omitempty"`
	UnsafeReason string `json:"unsafeReason,omitempty"`
}
type ArchiveExtractOptions struct {
	LibraryKey string `json:"libraryKey,omitempty"`
	Name       string `json:"name,omitempty"`
}
type ArchiveExtractResult struct {
	Ref   string `json:"ref"`
	Size  int64  `json:"size"`
	SHA1  string `json:"sha1"`
	MD5   string `json:"md5"`
	CRC32 string `json:"crc32"`
}

// RasterizeOptions selects the output size and format for CacheRasterize.
// A zero Width or Height keeps the SVG aspect ratio; Format is "png"
// (default) or "jpeg". Each dimension is bounded at 2048 by the host.
type RasterizeOptions struct {
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Format string `json:"format,omitempty"`
}

// RasterizeResult is returned by CacheRasterize: the new cached bitmap
// blob, its content type and pixel dimensions.
type RasterizeResult struct {
	Blob        string `json:"blob"`
	ContentType string `json:"contentType"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Size        int64  `json:"size"`
}

func (c *RemoteHostClient) dataCall(ctx context.Context, name string, args []any, out any) error {
	_, err := c.callJSON(ctx, http.MethodPost, "/ether/plugin-host/data/"+name, args, out, false)
	return err
}
func (c *RemoteHostClient) FetchToCache(ctx context.Context, r FetchCacheRequest) (FetchCacheResult, error) {
	var out FetchCacheResult
	err := c.dataCall(ctx, "http.fetchToCache", []any{r}, &out)
	return out, err
}
func (c *RemoteHostClient) ArchiveList(ctx context.Context, blob string) ([]ArchiveMember, error) {
	var out []ArchiveMember
	err := c.dataCall(ctx, "archive.list", []any{blob}, &out)
	return out, err
}
func (c *RemoteHostClient) dataRead(ctx context.Context, name string, args []any) ([]byte, error) {
	var out struct {
		BodyBase64 string `json:"bodyBase64"`
	}
	if err := c.dataCall(ctx, name, args, &out); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(out.BodyBase64)
}
func (c *RemoteHostClient) RuntimeLimits(ctx context.Context) (RuntimeLimits, error) {
	var out RuntimeLimits
	_, err := c.callJSON(ctx, http.MethodGet, "/ether/plugin-host/limits", nil, &out, false)
	return out, err
}

func checkDataRange(r DataRange) error {
	if r.Offset < 0 || r.Offset > 1<<60 || r.Length <= 0 || r.Length > MaxDataReadBytes {
		return fmt.Errorf("data range needs nonnegative offset and length 1..%d", MaxDataReadBytes)
	}
	return nil
}
func (c *RemoteHostClient) CacheRead(ctx context.Context, blob string, r DataRange) ([]byte, error) {
	if err := checkDataRange(r); err != nil {
		return nil, err
	}
	return c.dataRead(ctx, "cache.read", []any{blob, r})
}

// ArchiveOpen reads a bounded uncompressed ZIP, 7z or RAR member range.
// Compressed reads restart at the member or solid block beginning per call.
// Prefer large sequential chunks; decoder work and memory obey host grants.
func (c *RemoteHostClient) ArchiveOpen(ctx context.Context, blob string, member int, r DataRange) ([]byte, error) {
	if err := checkDataRange(r); err != nil {
		return nil, err
	}
	return c.dataRead(ctx, "archive.open", []any{blob, member, r})
}

// ArchiveRead reads 1..4096 bounded archive member ranges in one host call
// while keeping the sum of requested lengths within the plugin's readBytes grant.
func (c *RemoteHostClient) ArchiveRead(ctx context.Context, blob string, reads []ArchiveReadItem) ([]ArchiveReadResult, error) {
	if len(reads) == 0 || len(reads) > MaxArchiveReadItems {
		return nil, fmt.Errorf("archive.read takes 1..%d reads", MaxArchiveReadItems)
	}
	var total int64
	for _, item := range reads {
		if item.Member < 0 {
			return nil, fmt.Errorf("archive.read member index must be nonnegative")
		}
		if err := checkDataRange(DataRange{Offset: item.Offset, Length: item.Length}); err != nil {
			return nil, err
		}
		if item.Length > MaxDataReadBytes-total {
			return nil, fmt.Errorf("archive.read requested bytes exceed maximum readBytes ceiling (%d)", MaxDataReadBytes)
		}
		total += item.Length
	}
	var wire []struct {
		Index      int    `json:"index"`
		Member     int    `json:"member"`
		Size       int    `json:"size"`
		BodyBase64 string `json:"bodyBase64"`
	}
	if err := c.dataCall(ctx, "archive.read", []any{blob, reads}, &wire); err != nil {
		return nil, err
	}
	out := make([]ArchiveReadResult, len(wire))
	for i, w := range wire {
		body, err := base64.StdEncoding.DecodeString(w.BodyBase64)
		if err != nil {
			return nil, err
		}
		out[i] = ArchiveReadResult{Index: w.Index, Member: w.Member, Size: w.Size, Body: body}
	}
	return out, nil
}

// CacheRasterize rasterises an SVG already in the plugin's blob cache into
// a new cached bitmap blob under libraryKey/name, using Ether's bounded SVG
// rasteriser (source and output sizes are host-bounded, unsafe documents
// are refused, and rendered variants are cached host-side).
func (c *RemoteHostClient) CacheRasterize(ctx context.Context, libraryKey, name, blob string, opts RasterizeOptions) (RasterizeResult, error) {
	if opts.Width < 0 || opts.Width > 2048 || opts.Height < 0 || opts.Height > 2048 {
		return RasterizeResult{}, fmt.Errorf("cache.rasterize width and height must be within 0..2048")
	}
	if opts.Format != "" && opts.Format != "png" && opts.Format != "jpeg" {
		return RasterizeResult{}, fmt.Errorf("cache.rasterize format must be png or jpeg")
	}
	args := []any{libraryKey, name, blob}
	if opts != (RasterizeOptions{}) {
		args = append(args, opts)
	}
	var out RasterizeResult
	err := c.dataCall(ctx, "cache.rasterize", args, &out)
	return out, err
}

// ArchiveExtract streams an uncompressed ZIP, 7z or RAR member straight into
// the plugin's own library blob cache in one pass and returns its blob ref and
// SHA-1/MD5/CRC32 digests without buffering the member in plugin memory.
func (c *RemoteHostClient) ArchiveExtract(ctx context.Context, blob string, member int, opts ...ArchiveExtractOptions) (ArchiveExtractResult, error) {
	args := []any{blob, member}
	if len(opts) > 0 && (opts[0].LibraryKey != "" || opts[0].Name != "") {
		args = append(args, opts[0])
	}
	var out ArchiveExtractResult
	err := c.dataCall(ctx, "archive.extract", args, &out)
	return out, err
}

// HashRequest requests an on-demand host-side remote digest without storing bytes.
// Optional Headers expand {secret:<name>} placeholders and follow redirects
// only within the same origin, strict same-site subdomains, or
// KeepHeadersOnRedirect (within granted network hosts, never across HTTPS->HTTP).
type HashRequest struct {
	URL                   string            `json:"url"`
	Algo                  string            `json:"algo"`
	ExpectSize            *int64            `json:"expectSize,omitempty"`
	Headers               map[string]string `json:"headers,omitempty"`
	KeepHeadersOnRedirect []string          `json:"keepHeadersOnRedirect,omitempty"`
}
type HashResult struct {
	Pending   bool   `json:"pending,omitempty"`
	RetryInMS int64  `json:"retryInMs,omitempty"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256,omitempty"`
	SHA1      string `json:"sha1,omitempty"`
	MD5       string `json:"md5,omitempty"`
}

func (c *RemoteHostClient) Hash(ctx context.Context, r HashRequest) (HashResult, error) {
	var out HashResult
	err := c.dataCall(ctx, "http.hash", []any{r}, &out)
	return out, err
}
