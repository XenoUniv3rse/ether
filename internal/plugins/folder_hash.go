package plugins

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type folderAlgorithms uint8

const (
	folderCRC32 folderAlgorithms = 1 << iota
	folderMD5
	folderSHA1
	folderOSHash
	folderPHash
	folderDefaultHash
	folderWhole = folderCRC32 | folderMD5 | folderSHA1
	osHashBlock = 64 << 10
)

func parseFolderAlgorithms(options []FolderHashOptions) (folderAlgorithms, error) {
	if len(options) == 0 || len(options[0].Algorithms) == 0 {
		return folderWhole | folderOSHash | folderDefaultHash, nil
	}
	var mask folderAlgorithms
	for _, algo := range options[0].Algorithms {
		switch algo {
		case "crc32":
			mask |= folderCRC32
		case "md5":
			mask |= folderMD5
		case "sha1":
			mask |= folderSHA1
		case "oshash":
			mask |= folderOSHash
		case "phash":
			mask |= folderPHash
		default:
			return 0, fmt.Errorf("folders.hash: unsupported algorithm")
		}
	}
	return mask, nil
}

type openedFolderFile struct {
	*os.File
	root *os.Root
	info os.FileInfo
	path string
}

func (h *host) openFolderFile(ctx context.Context, root, path string) (*openedFolderFile, error) {
	if filepath.IsAbs(path) {
		return nil, ErrPermissionDenied
	}
	for _, segment := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment == ".." {
			return nil, ErrPermissionDenied
		}
	}
	if _, err := h.folderCheck(ctx, root, path); err != nil {
		return nil, err
	}
	fs, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("folders: cannot open picked folder")
	}
	before, err := fs.Stat(path)
	if err != nil || !before.Mode().IsRegular() {
		fs.Close()
		return nil, fmt.Errorf("folders: a regular file is required")
	}
	f, err := fs.OpenFile(path, folderReadFlags, 0)
	if err != nil {
		fs.Close()
		return nil, fmt.Errorf("folders: cannot open picked file")
	}
	info, err := f.Stat()
	if err != nil || !os.SameFile(before, info) || !info.Mode().IsRegular() {
		f.Close()
		fs.Close()
		return nil, fmt.Errorf("folders: a regular file is required")
	}
	return &openedFolderFile{File: f, root: fs, info: info, path: path}, nil
}

func (f *openedFolderFile) Close() {
	f.File.Close()
	f.root.Close()
}

func (f *openedFolderFile) unchanged() bool {
	current, err := f.root.Stat(f.path)
	return err == nil && os.SameFile(f.info, current) &&
		current.Size() == f.info.Size() && current.ModTime().Equal(f.info.ModTime())
}

func (f *openedFolderFile) cacheKey(operation string) string {
	dev, inode, ok := core.OpenFileID(f.File, f.info)
	if !ok {
		return ""
	}
	identity := fmt.Sprintf("%s:%d:%d:%d:%d", operation, dev, inode, f.info.Size(), f.info.ModTime().UnixNano())
	return fmt.Sprintf("folder.v1.%x", sha256.Sum256([]byte(identity)))
}

func openSubtitlesHash(f io.ReaderAt, size int64) (string, error) {
	if size < osHashBlock {
		return "", fmt.Errorf("folders.hash: oshash requires at least 64 KiB")
	}
	var block [osHashBlock]byte
	sum := uint64(size)
	for _, off := range []int64{0, size - osHashBlock} {
		if _, err := f.ReadAt(block[:], off); err != nil {
			return "", fmt.Errorf("folders.hash: could not read oshash blocks")
		}
		for i := 0; i < len(block); i += 8 {
			sum += binary.LittleEndian.Uint64(block[i : i+8])
		}
	}
	return fmt.Sprintf("%016x", sum), nil
}

// FoldersHash keeps small whole-file hashes synchronous. Oshash alone is a
// bounded read regardless of file size.
func (h *host) FoldersHash(ctx context.Context, root, path string, options ...FolderHashOptions) (FolderHash, error) {
	algos, err := parseFolderAlgorithms(options)
	if err != nil {
		return FolderHash{}, err
	}
	if algos&folderPHash != 0 {
		return FolderHash{}, fmt.Errorf("folders.hash: use hashStart for phash")
	}
	return h.hashFile(ctx, root, path, algos, false, nil)
}

func (h *host) hashFile(ctx context.Context, root, path string, algos folderAlgorithms, background bool, progress func(int64)) (FolderHash, error) {
	f, err := h.openFolderFile(ctx, root, path)
	if err != nil {
		return FolderHash{}, err
	}
	defer f.Close()
	const syncLimit = 256 << 20
	size := f.info.Size()
	if !background && algos&folderWhole != 0 && size > syncLimit {
		return FolderHash{}, fmt.Errorf("folders.hash: use hashStart for whole-file digests above 256 MiB")
	}
	if algos&folderDefaultHash != 0 {
		algos &^= folderDefaultHash
		if size < osHashBlock {
			algos &^= folderOSHash
		}
	}

	key := f.cacheKey(fmt.Sprintf("hash:%d", algos))
	if key != "" {
		raw, ok, err := h.m.store.hashCacheGet(ctx, h.plugin, key)
		if err != nil {
			return FolderHash{}, err
		}
		var cached FolderHash
		if ok && json.Unmarshal(raw, &cached) == nil && f.unchanged() {
			return cached, nil
		}
	}

	result := FolderHash{Size: size, ModTime: f.info.ModTime().Unix()}
	if algos&folderOSHash != 0 {
		result.OSHash, err = openSubtitlesHash(f, size)
		if err != nil {
			return FolderHash{}, err
		}
	}
	if algos&folderWhole != 0 {
		if err := h.hashWholeFile(ctx, root, f, algos, background, progress, &result); err != nil {
			return FolderHash{}, err
		}
	}
	if algos&folderPHash != 0 {
		result.PHash, err = h.videoPHash(ctx, root, f)
		if err != nil {
			return FolderHash{}, err
		}
	}
	if _, err := h.folderCheck(ctx, root, path); err != nil {
		return FolderHash{}, err
	}
	if !f.unchanged() {
		return FolderHash{}, fmt.Errorf("folders.hash: file changed while hashing")
	}
	if key != "" {
		raw, _ := json.Marshal(result)
		if err := h.m.store.hashCachePut(ctx, h.plugin, key, raw); err != nil {
			return FolderHash{}, err
		}
	}
	return result, nil
}

func (h *host) hashWholeFile(ctx context.Context, root string, f *openedFolderFile, algos folderAlgorithms, background bool, progress func(int64), result *FolderHash) error {
	var hashes []hash.Hash
	var writers []io.Writer
	var targets []*string
	digests := []struct {
		flag folderAlgorithms
		make func() hash.Hash
		dest *string
	}{
		{folderCRC32, func() hash.Hash { return crc32.NewIEEE() }, &result.CRC32},
		{folderMD5, md5.New, &result.MD5},
		{folderSHA1, sha1.New, &result.SHA1},
	}
	for _, d := range digests {
		if algos&d.flag != 0 {
			digest := d.make()
			hashes = append(hashes, digest)
			writers = append(writers, digest)
			targets = append(targets, d.dest)
		}
	}
	w := io.MultiWriter(writers...)
	buf := make([]byte, 1<<20)
	var total int64
	r := io.NewSectionReader(f, 0, f.info.Size())
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := h.folderCheck(ctx, root, f.path); err != nil {
			return err
		}
		n, err := r.Read(buf)
		total += int64(n)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if progress != nil {
				progress(total)
			}
			if background {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(2 * time.Millisecond):
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("folders.hash: could not read picked file")
		}
	}
	if total != f.info.Size() {
		return fmt.Errorf("folders.hash: file changed while hashing")
	}
	for i, digest := range hashes {
		*targets[i] = fmt.Sprintf("%x", digest.Sum(nil))
	}
	return nil
}
