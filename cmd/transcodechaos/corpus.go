package main

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Public media is cached, never vendored. Git blob hashes and SHA-256s pin the upstream content.
type corpusAsset struct {
	name, url, license, hashKind, digest string
	size                                 int64
	full                                 bool
}

var publicAssets = []corpusAsset{
	{"matroska-test2.mkv", "https://raw.githubusercontent.com/ietf-wg-cellar/matroska-test-files/e6965e5ca666322ed93e2748a10a4f132309e005/test_files/test2.mkv", "CC-BY Blender Foundation / Netherlands Media Art Institute; https://github.com/ietf-wg-cellar/matroska-test-files#license", "git-sha1", "56f290ba264523a5ca81d1d4aa63daf1ada95433", 21142764, false},
	{"matroska-test3.mkv", "https://raw.githubusercontent.com/ietf-wg-cellar/matroska-test-files/e6965e5ca666322ed93e2748a10a4f132309e005/test_files/test3.mkv", "CC-BY Blender Foundation / Netherlands Media Art Institute; https://github.com/ietf-wg-cellar/matroska-test-files#license", "git-sha1", "35c823fa73217a205c9fec04818b3d6d4cf3c258", 21061472, true},
	{"matroska-test4.mkv", "https://raw.githubusercontent.com/ietf-wg-cellar/matroska-test-files/e6965e5ca666322ed93e2748a10a4f132309e005/test_files/test4.mkv", "CC-BY Blender Foundation; https://github.com/ietf-wg-cellar/matroska-test-files#license", "git-sha1", "0d7ec1676a0d0442d9c6387dcc8f36c1a3150db6", 21313902, true},
	{"gstreamer-raw-h264.mp4", "https://gitlab.freedesktop.org/gstreamer/gst-integration-testsuites/-/raw/6934d5a736a76cff60549e7d5de62fff39263620/defaults/mp4/raw_h264.0.mp4", "GStreamer upstream integration test media; https://github.com/GStreamer/gst-integration-testsuites", "sha256", "8ab75537557bdc13f7cc53e391e3c4d681f6e1f3fba5fa40251da5c28b2e1886", 1013457, false},
	{"gstreamer-h264-srt.mkv", "https://gitlab.freedesktop.org/gstreamer/gst-integration-testsuites/-/raw/6934d5a736a76cff60549e7d5de62fff39263620/defaults/matroska/h264_srt.mkv", "GStreamer upstream integration test media; https://github.com/GStreamer/gst-integration-testsuites", "sha256", "121fe2833853acf4fc64f8acb4cbcb7b5386565774eb1c602f5355c217311499", 9159, true},
	{"chromium-bear.webm", "https://raw.githubusercontent.com/chromium/chromium/1c6305184488af7e85ac8c5345b4f17cd27e7623/media/test/data/bear-320x240.webm", "Chromium media test data; https://chromium.googlesource.com/chromium/src/+/main/media/test/data/README.md", "git-sha1", "a1b4150b32f880fae91fdf274437faa91be1ba6f", 219229, false},
	{"chromium-bear.mp4", "https://raw.githubusercontent.com/chromium/chromium/1c6305184488af7e85ac8c5345b4f17cd27e7623/media/test/data/bear-1280x720.mp4", "Chromium media test data; https://chromium.googlesource.com/chromium/src/+/main/media/test/data/README.md", "git-sha1", "b424a0f7d1284d3b27e00ed3f85b175ac8df8885", 715963, true},
	{"chromium-bear-vtt.webm", "https://raw.githubusercontent.com/chromium/chromium/1c6305184488af7e85ac8c5345b4f17cd27e7623/media/test/data/bear-vp8-webvtt.webm", "Chromium media test data; https://chromium.googlesource.com/chromium/src/+/main/media/test/data/README.md", "git-sha1", "c4b4d14ee6bfed17db0085a18f1ed2aaf6fd2dbe", 219285, true},
	{"chromium-hdr10.mp4", "https://raw.githubusercontent.com/chromium/chromium/1c6305184488af7e85ac8c5345b4f17cd27e7623/media/test/data/bear-1280x720-hevc-10bit-hdr10.mp4", "Chromium media test data; https://chromium.googlesource.com/chromium/src/+/main/media/test/data/README.md", "git-sha1", "0852a336c567709270ad32983f374a46e3e8894e", 319643, true},
	{"chromium-dv5.mp4", "https://raw.githubusercontent.com/chromium/chromium/1c6305184488af7e85ac8c5345b4f17cd27e7623/media/test/data/glass-blowing2-dolby-vision-profile-5-frag.mp4", "Chromium media test data; https://chromium.googlesource.com/chromium/src/+/main/media/test/data/README.md", "git-sha1", "70e53b60d17e74ae68d18819d15dfeaaacc93ad5", 4537752, true},
	{"chromium-dv8.mp4", "https://raw.githubusercontent.com/chromium/chromium/1c6305184488af7e85ac8c5345b4f17cd27e7623/media/test/data/glass-blowing2-dolby-vision-profile-8-1-frag.mp4", "Chromium media test data; https://chromium.googlesource.com/chromium/src/+/main/media/test/data/README.md", "git-sha1", "129881c2d1b599c03b759ac47a0320919adca8f1", 3528195, true},
	{"netflix-isobmff.mp4", "https://s3.amazonaws.com/download.opencontent.netflix.com/ISOBMFF-packages/C_V2_9615614722_136_2397.mp4", "CC-BY-4.0 Netflix; https://opencontent.netflix.com/home", "sha256", "4e26426158423d5f64e3711a3032324f7a097fc77209e07ad0fc1f34c473ca08", 25706490, true},
	{"apple-bipbop.ts", "https://devstreaming-cdn.apple.com/videos/streaming/examples/bipbop_16x9/gear1/main.ts", "Apple developer HLS example; https://developer.apple.com/streaming/examples/", "sha256", "e1854ce49fdddf919e87f4f5d65b719bd985ee8fa4f84c0b58c9b641f383db43", 980232, true},
}

const maxCorpusBytes int64 = 10_000_000_000

//go:embed expected-failures.json
var expectedFailuresJSON []byte

type expectedFailure struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Reason string `json:"reason"`
}

func expectedCorpusFailure(a corpusAsset) (string, error) {
	var entries []expectedFailure
	if err := json.Unmarshal(expectedFailuresJSON, &entries); err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Name == a.name && entry.Digest == a.digest && entry.Reason != "" {
			return entry.Reason, nil
		}
	}
	return "", nil
}

func corpusHash(a corpusAsset) hash.Hash {
	if a.hashKind == "git-sha1" {
		h := sha1.New()
		fmt.Fprintf(h, "blob %d\x00", a.size)
		return h
	}
	return sha256.New()
}
func verifiedCorpus(path string, a corpusAsset) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	h := corpusHash(a)
	n, e := io.Copy(h, f)
	if e != nil {
		return e
	}
	if n != a.size {
		return fmt.Errorf("size mismatch")
	}
	if a.digest != "" && hex.EncodeToString(h.Sum(nil)) != a.digest {
		return fmt.Errorf("checksum mismatch")
	}
	return nil
}
func withinDir(parent, child string) bool {
	rel, e := filepath.Rel(parent, child)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func outsideRepo(path string) error {
	abs, e := filepath.Abs(path)
	if e != nil {
		return e
	}
	repo, e := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if e != nil {
		return nil
	}
	root, e := filepath.EvalSymlinks(strings.TrimSpace(string(repo)))
	if e != nil {
		return e
	}
	// Resolve the nearest existing ancestor before creating anything.
	ancestor := abs
	for {
		if _, e := os.Lstat(ancestor); e == nil {
			break
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			return fmt.Errorf("cannot resolve cache path")
		}
		ancestor = next
	}
	resolved, e := filepath.EvalSymlinks(ancestor)
	if e != nil {
		return e
	}
	suffix, e := filepath.Rel(ancestor, abs)
	if e != nil {
		return e
	}
	if withinDir(root, filepath.Join(resolved, suffix)) {
		return fmt.Errorf("corpus cache must be outside the repository")
	}
	return nil
}
func downloadCorpus(ctx context.Context, cache, mode, ff string) (map[string]string, []result, error) {
	if err := outsideRepo(cache); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(cache, 0700); err != nil {
		return nil, nil, err
	}
	lock := filepath.Join(cache, ".transcodechaos-lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return nil, nil, fmt.Errorf("corpus cache is locked: %w", err)
	}
	defer os.Remove(lock)
	used, err := diskBytes(cache)
	if err != nil {
		return nil, nil, err
	}
	if used > maxCorpusBytes {
		return nil, nil, fmt.Errorf("corpus cache exceeds 10 GB")
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	files := map[string]string{}
	var results []result
	for _, a := range publicAssets {
		if mode == "short" && a.full {
			continue
		}
		start := time.Now()
		path := filepath.Join(cache, a.name)
		detail := ""
		status := "PASS"
		if st, err := os.Lstat(path); err == nil {
			if !st.Mode().IsRegular() {
				detail = "cached asset is not a regular file"
			} else if err = verifiedCorpus(path, a); err != nil {
				detail = "cached asset invalid: " + err.Error()
			}
		} else if !os.IsNotExist(err) {
			detail = err.Error()
		} else if used+a.size > maxCorpusBytes {
			detail = "10 GB cache limit would be exceeded"
		} else {
			url := a.url
			if strings.Contains(url, "gitlab.freedesktop.org") {
				url += fmt.Sprintf("?transcodechaos=%d", time.Now().UnixNano())
			}
			req, e := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if e != nil {
				detail = e.Error()
			} else {
				if a.name == "apple-bipbop.ts" {
					req.Header.Set("Range", "bytes=0-980231")
				}
				resp, e := client.Do(req)
				if e != nil {
					detail = "download failed"
				} else {
					validStatus := resp.StatusCode == http.StatusOK
					if a.name == "apple-bipbop.ts" {
						validStatus = resp.StatusCode == http.StatusPartialContent && strings.HasPrefix(resp.Header.Get("Content-Range"), "bytes 0-980231/")
					}
					if !validStatus {
						detail = fmt.Sprintf("download HTTP %d", resp.StatusCode)
					} else {
						tmp, e := os.CreateTemp(cache, ".download-*")
						if e != nil {
							detail = e.Error()
						} else {
							tmpName := tmp.Name()
							h := corpusHash(a)
							n, copyErr := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, a.size+1))
							closeErr := tmp.Close()
							if copyErr != nil || closeErr != nil || n != a.size {
								detail = "download incomplete or larger than declared"
							} else if a.digest != "" && hex.EncodeToString(h.Sum(nil)) != a.digest {
								detail = "download checksum mismatch"
							} else if e = os.Link(tmpName, path); e != nil {
								detail = "cache publication failed"
							} else {
								used += n
							}
							_ = os.Remove(tmpName)
						}
					}
					resp.Body.Close()
				}
			}
		}
		if detail != "" {
			status = "FAIL"
		} else {
			if strings.HasPrefix(a.name, "netflix-") {
				path, err = trimCorpusAsset(ctx, ff, cache, path, a.digest)
				if err != nil {
					status = "FAIL"
					detail = "could not create a bounded 30-second cut"
				}
			}
		}
		if status == "PASS" {
			files[a.name] = path
		}
		used, err = diskBytes(cache)
		if err != nil {
			return files, results, err
		}
		res := result{a.name, "corpus/download", status, detail, time.Since(start).Milliseconds()}
		results = append(results, res)
		fmt.Printf("%-32s %-20s %-4s %6dms %s\n", res.Scenario, res.Path, res.Status, res.Milliseconds, res.Detail)
	}
	return files, results, nil
}

func trimCorpusAsset(ctx context.Context, ff, cache, input, digest string) (string, error) {
	out := filepath.Join(cache, "netflix-cut-"+digest[:16]+".mp4")
	if st, err := os.Lstat(out); err == nil {
		if !st.Mode().IsRegular() || st.Size() == 0 {
			return "", fmt.Errorf("invalid cached cut")
		}
		return out, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	used, err := diskBytes(cache)
	if err != nil {
		return "", err
	}
	remaining := maxCorpusBytes - used
	if remaining <= 0 {
		return "", fmt.Errorf("cache budget exhausted")
	}
	tmp, err := os.CreateTemp(cache, ".cut-*.mp4")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(name)
	if err = command(ctx, ff, "-hide_banner", "-loglevel", "error", "-y", "-i", input, "-t", "30", "-map", "0:v:0", "-map", "0:a?", "-c", "copy", "-fs", fmt.Sprint(remaining), "-movflags", "+faststart", "-f", "mp4", name); err != nil {
		return "", err
	}
	st, err := os.Stat(name)
	if err != nil || st.Size() == 0 || st.Size() > remaining {
		return "", fmt.Errorf("invalid or oversized cut")
	}
	if err = os.Link(name, out); err != nil {
		return "", err
	}
	return out, nil
}
