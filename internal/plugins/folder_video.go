package plugins

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/corona10/goimagehash"
	"golang.org/x/image/bmp"
)

const (
	folderVideoFormats = "mov,matroska,webm,avi,mpeg,mpegts,flv,asf,ogg,mxf,rm"
	folderMaxPixels    = "37748736" // 8192 x 4608
)

func (h *host) checkFolderVideoCall(ctx context.Context) error {
	rec, _, err := h.record(ctx)
	if err != nil {
		return err
	}
	path := callPathFromContext(ctx)
	if (path == "" && !isBackgroundCall(ctx)) || (path != "" && !MediaProbeAllowedPath(rec.Manifest, path, isBackgroundCall(ctx))) {
		return fmt.Errorf("%w: folder video operations require a job or background route", ErrPermissionDenied)
	}
	if h.m.deps.FFmpeg == "" {
		return ErrUnavailable
	}
	return nil
}

// Use the already opened rooted file across all platforms, with HTTP Range
// for seeks. The child never receives a filesystem path to reopen.
func serveFolderVideo(f *openedFolderFile) (string, func(), error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", nil, ErrUnavailable
	}
	path := "/" + rand.Text()
	server := &http.Server{
		ReadHeaderTimeout: 2 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != path || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeContent(w, r, "", time.Time{}, io.NewSectionReader(f, 0, f.info.Size()))
		}),
	}
	go func() { _ = server.Serve(listener) }()
	return "http://" + listener.Addr().String() + path, func() { _ = server.Close() }, nil
}

// Only self-contained demuxers and the loopback HTTP transport are enabled.
// Clear proxy variables so the file capability cannot leave the machine.
func runFolderMedia(ctx context.Context, binary string, args ...string) ([]byte, error) {
	cmd := mediaproc.CommandContext(ctx, 10*time.Second, binary, args...)
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if !strings.HasSuffix(strings.ToLower(key), "_proxy") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1")
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, context.DeadlineExceeded
		}
		return nil, fmt.Errorf("folders: media tool could not read the video")
	}
	return out, nil
}

func folderScreenshot(ctx context.Context, ffmpeg, input string, at float64, width int, perception bool) ([]byte, error) {
	args := []string{"-v", "error", "-nostdin", "-filter_threads", "1", "-ss", strconv.FormatFloat(at, 'f', -1, 64),
		"-protocol_whitelist", "http,tcp", "-format_whitelist", folderVideoFormats,
		"-threads", "1", "-max_pixels", folderMaxPixels, "-i", input}
	if !perception {
		args = append(args, "-map", "0:V:0")
	}
	args = append(args, "-an", "-sn", "-dn", "-frames:v", "1", "-threads", "1")
	if perception {
		// Stash scales to 160:-2. Bound extreme aspect ratios before allocating;
		// the caller rejects a frame that reaches this safety ceiling.
		args = append(args, "-vf", `scale=160:if(gte(ih\,iw*6.4)\,1024\,-2)`, "-c:v", "bmp", "-f", "rawvideo")
	} else {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:1024:force_original_aspect_ratio=decrease:force_divisible_by=2", width), "-c:v", "png", "-f", "image2pipe")
	}
	out, err := runFolderMedia(ctx, ffmpeg, append(args, "pipe:1")...)
	if err == nil && len(out) == 0 {
		err = fmt.Errorf("folders: no video frame at requested timestamp")
	}
	return out, err
}

func (h *host) videoPHash(ctx context.Context, root string, f *openedFolderFile) (string, error) {
	if h.m.deps.FFprobe == "" {
		return "", ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	input, closeInput, err := serveFolderVideo(f)
	if err != nil {
		return "", err
	}
	defer closeInput()
	out, err := runFolderMedia(ctx, h.m.deps.FFprobe, "-v", "error",
		"-protocol_whitelist", "http,tcp", "-format_whitelist", folderVideoFormats,
		"-threads", "1", "-max_pixels", folderMaxPixels, "-select_streams", "V", "-show_entries", "stream=index:format=duration", "-of", "json", input)
	if err != nil {
		return "", err
	}
	var probe struct {
		Streams []struct{ Index int }
		Format  struct{ Duration string }
	}
	if json.Unmarshal(out, &probe) != nil || len(probe.Streams) == 0 {
		return "", fmt.Errorf("folders.hash: phash requires a video stream")
	}
	duration, err := strconv.ParseFloat(probe.Format.Duration, 64)
	duration = math.Round(duration*100) / 100
	if err != nil || duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
		return "", fmt.Errorf("folders.hash: phash requires a known video duration")
	}
	return folderPerceptionHash(duration, func(at float64) (image.Image, error) {
		if _, err := h.folderCheck(ctx, root, f.path); err != nil {
			return nil, err
		}
		frame, err := folderScreenshot(ctx, h.m.deps.FFmpeg, input, at, 160, true)
		if err != nil {
			return nil, err
		}
		img, err := bmp.Decode(bytes.NewReader(frame))
		if err != nil {
			return nil, fmt.Errorf("folders.hash: invalid video frame")
		}
		return img, nil
	})
}

// Match Stash v0.31.1's sample positions and row-major montage, then call the
// same PerceptionHash implementation used by Stash.
func folderPerceptionHash(duration float64, frameAt func(float64) (image.Image, error)) (string, error) {
	const columns, rows, width = 5, 5, 160
	var tile *image.NRGBA
	var height int
	for i := range columns * rows {
		at := 0.05*duration + float64(i)*(0.9*duration/float64(columns*rows))
		frame, err := frameAt(at)
		if err != nil {
			return "", err
		}
		b := frame.Bounds()
		if b.Dx() != width || b.Dy() < 1 || b.Dy() >= 1024 {
			return "", fmt.Errorf("folders.hash: unsupported frame dimensions")
		}
		if tile == nil {
			height = b.Dy()
			tile = image.NewNRGBA(image.Rect(0, 0, width*columns, height*rows))
		}
		point := image.Pt(i%columns*width, i/columns*height)
		draw.Draw(tile, b.Sub(b.Min).Add(point), frame, b.Min, draw.Src)
	}
	hash, err := goimagehash.PerceptionHash(tile)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%016x", hash.GetHash()), nil
}

func (h *host) FoldersFrame(ctx context.Context, root, path string, opts FolderFrameOptions) (FolderFrame, error) {
	if err := h.checkFolderVideoCall(ctx); err != nil {
		return FolderFrame{}, err
	}
	if opts.AtSec < 0 || math.IsNaN(opts.AtSec) || math.IsInf(opts.AtSec, 0) || opts.Width < 16 || opts.Width > 1024 {
		return FolderFrame{}, fmt.Errorf("folders.frame: atSec must be finite and nonnegative and width must be 16–1024")
	}
	lib, err := h.archiveExtractCheck(ctx, opts.LibraryKey)
	if err != nil {
		return FolderFrame{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	f, err := h.openFolderFile(ctx, root, path)
	if err != nil {
		return FolderFrame{}, err
	}
	defer f.Close()

	key := f.cacheKey("frame:" + strconv.FormatFloat(opts.AtSec, 'g', -1, 64) + ":" + strconv.Itoa(opts.Width))
	if key == "" {
		key = "frame-" + rand.Text()
	}
	key += ".png"
	if lookup := h.m.kindBackends().CacheLookup; lookup != nil {
		if ref, ok := lookup(h.plugin, lib, key); ok && f.unchanged() {
			return FolderFrame{Ref: ref, LibraryKey: lib}, nil
		}
	}
	if !h.m.rateOK(h.m.folderFrameTimes, h.plugin, 20, time.Minute) {
		return FolderFrame{}, fmt.Errorf("%w: folders.frame exceeds 20 uncached calls per minute", ErrRateLimited)
	}
	select {
	case folderHashSlots <- struct{}{}:
		defer func() { <-folderHashSlots }()
	default:
		return FolderFrame{}, fmt.Errorf("%w: folder media workers are busy", ErrRateLimited)
	}
	input, closeInput, err := serveFolderVideo(f)
	if err != nil {
		return FolderFrame{}, err
	}
	defer closeInput()
	frame, err := folderScreenshot(ctx, h.m.deps.FFmpeg, input, opts.AtSec, opts.Width, false)
	if err != nil {
		return FolderFrame{}, err
	}
	if _, err := h.folderCheck(ctx, root, path); err != nil {
		return FolderFrame{}, err
	}
	if !f.unchanged() {
		return FolderFrame{}, fmt.Errorf("folders.frame: file changed while extracting a frame")
	}

	ref, err := h.CachePut(ctx, lib, key, bytes.NewReader(frame))
	if err != nil {
		return FolderFrame{}, err
	}
	return FolderFrame{Ref: ref, LibraryKey: lib}, nil
}
