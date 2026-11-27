package blobcache

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/braedonsaunders/ether/internal/atomicfile"
	"github.com/braedonsaunders/ether/internal/segcache"
)

// DurableMaxBytes is the fixed per-plugin quota for saved blobs.
const DurableMaxBytes int64 = 1 << 30

// DurableRefPrefix distinguishes saved references from evictable cache refs.
const DurableRefPrefix = "durable:"

// ParseDurableRef returns the digest from a durable reference.
func ParseDurableRef(ref Ref) (string, error) {
	hash, ok := strings.CutPrefix(string(ref), DurableRefPrefix)
	if !ok {
		return "", ErrBadRef
	}
	return ParseRef("blob:" + hash)
}

// Bound both staged disk space and the number of concurrent upload requests.
const (
	stagingMaxBytes    = 512 << 20
	stagingMaxRequests = 64
	durableMaxFiles    = 10000
)

type storedUpload struct {
	plugin, path, name, mime string
	stat                     Stat
}

type durableUsage struct {
	bytes int64
	files int
}

// DurableStore keeps request-scoped uploads and plugin-owned durable blobs
// outside the library cache's eviction and TTL machinery.
type DurableStore struct {
	root string
	disk segcache.DiskUsageFunc

	mu       sync.Mutex
	uploads  map[Ref]storedUpload
	usage    map[string]durableUsage
	reserved int64
	active   int
}

// OpenDurableStore opens the store and discards staging files from the previous process.
func OpenDurableStore(root string, disk segcache.DiskUsageFunc) (*DurableStore, error) {
	// Upload files have no lifetime across process restarts.
	if err := os.RemoveAll(filepath.Join(root, "uploads")); err != nil {
		return nil, err
	}
	return &DurableStore{
		root: root, disk: disk, uploads: make(map[Ref]storedUpload),
		usage: make(map[string]durableUsage),
	}, nil
}

func (s *DurableStore) pluginDir(plugin string) string {
	id := sha256.Sum256([]byte(plugin))
	return filepath.Join(s.root, "saved", hex.EncodeToString(id[:]))
}

// Upload holds the reservation and the files belonging to one adapter call.
// Stage and Close are used sequentially by the request handler.
type Upload struct {
	store               *DurableStore
	plugin              string
	remaining, reserved int64
	refs                []Ref
}

func (s *DurableStore) BeginUpload(plugin string, size int64) (*Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if size < 0 || size > stagingMaxBytes-s.reserved || s.active >= stagingMaxRequests {
		return nil, ErrQuotaFull
	}
	if s.disk != nil {
		if total, free, err := s.disk(); err == nil && total > 0 &&
			size+s.reserved > free-segcache.MinFreeBytes(total) {
			return nil, ErrQuotaFull
		}
	}
	s.reserved += size
	s.active++
	return &Upload{store: s, plugin: plugin, remaining: size, reserved: size}, nil
}

func (u *Upload) Stage(ctx context.Context, name, mime string, src io.Reader) (Ref, Stat, error) {
	dir := filepath.Join(u.store.root, "uploads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", Stat{}, err
	}
	f, err := os.CreateTemp(dir, "upload-")
	if err != nil {
		return "", Stat{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	stat, err := copyWithDigests(f, &boundedUploadReader{ctx: ctx, r: src, left: u.remaining})
	if err != nil {
		return "", Stat{}, err
	}
	if err := f.Close(); err != nil {
		return "", Stat{}, err
	}
	var id [sha256.Size]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", Stat{}, err
	}
	ref := Ref("blob:" + hex.EncodeToString(id[:]))
	u.store.mu.Lock()
	u.store.uploads[ref] = storedUpload{
		plugin: u.plugin, path: f.Name(), name: name, mime: mime, stat: stat,
	}
	u.store.mu.Unlock()
	u.remaining -= stat.Size
	u.refs = append(u.refs, ref)
	keep = true
	return ref, stat, nil
}

type boundedUploadReader struct {
	ctx  context.Context
	r    io.Reader
	left int64
}

func (r *boundedUploadReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p[:min(int64(len(p)), r.left+1)])
	r.left -= int64(n)
	if r.left < 0 {
		return 0, ErrTooLarge
	}
	return n, err
}

func (u *Upload) Close() {
	s := u.store
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ref := range u.refs {
		_ = os.Remove(s.uploads[ref].path)
		delete(s.uploads, ref)
	}
	s.reserved -= u.reserved
	s.active--
}

func (s *DurableStore) Open(plugin string, ref Ref) (ByteSource, error) {
	if !strings.HasPrefix(string(ref), DurableRefPrefix) {
		s.mu.Lock()
		defer s.mu.Unlock()
		u, ok := s.uploads[ref]
		if !ok || u.plugin != plugin {
			return nil, ErrNotFound
		}
		f, err := os.Open(u.path)
		if err != nil {
			return nil, err
		}
		return &stagedSource{fileSource: &fileSource{f: f, size: u.stat.Size, name: u.name, mime: u.mime}, stat: u.stat}, nil
	}
	hash, err := ParseDurableRef(ref)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(s.pluginDir(plugin), hash))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &fileSource{f: f, size: info.Size(), name: hash, mime: "application/octet-stream"}, nil
}

// usageLocked loads accounting on first use. Named files are the source of
// truth, so a process interruption cannot leave quota counters out of sync.
func (s *DurableStore) usageLocked(plugin string) (durableUsage, error) {
	if usage, ok := s.usage[plugin]; ok {
		return usage, nil
	}
	dir := s.pluginDir(plugin)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return durableUsage{}, err
	}
	var usage durableUsage
	for _, entry := range entries {
		if _, err := ParseRef("blob:" + entry.Name()); err != nil || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return durableUsage{}, err
		}
		usage.bytes += info.Size()
		usage.files++
	}
	s.usage[plugin] = usage
	return usage, nil
}

// Promote hard-links a staged upload into the durable namespace and returns
// its permanent content-addressed reference. The staged reference retains its
// request lifetime, including when multiple parts have identical contents.
func (s *DurableStore) Promote(plugin string, ref Ref) (Ref, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[ref]
	if !ok || u.plugin != plugin {
		return "", ErrNotFound
	}
	out := Ref(DurableRefPrefix + u.stat.SHA256)
	dir := s.pluginDir(plugin)
	target := filepath.Join(dir, u.stat.SHA256)
	if _, err := os.Stat(target); err == nil {
		rememberStat(u.stat)
		return out, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	usage, err := s.usageLocked(plugin)
	if err != nil {
		return "", err
	}
	if u.stat.Size > DurableMaxBytes-usage.bytes || usage.files >= durableMaxFiles {
		return "", ErrQuotaFull
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(u.path, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	err = f.Sync()
	_ = f.Close()
	if err != nil {
		return "", err
	}
	if err := os.Link(u.path, target); err != nil {
		return "", err
	}
	rememberStat(u.stat)
	usage.bytes += u.stat.Size
	usage.files++
	s.usage[plugin] = usage
	// The link is already committed. Keep accounting consistent even on
	// platforms or filesystems that cannot sync directory entries.
	for _, path := range []string{dir, filepath.Dir(dir), s.root, filepath.Dir(s.root)} {
		if err := atomicfile.SyncDir(path); err != nil {
			slog.Warn("plugin storage: could not sync durable blob directory", "err", err)
			break
		}
	}
	return out, nil
}

func (s *DurableStore) Delete(plugin string, ref Ref) (bool, error) {
	hash, err := ParseDurableRef(ref)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	target := filepath.Join(s.pluginDir(plugin), hash)
	info, err := os.Stat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	usage, err := s.usageLocked(plugin)
	if err != nil {
		return false, err
	}
	if err := os.Remove(target); err != nil {
		return false, err
	}
	usage.bytes -= info.Size()
	usage.files--
	s.usage[plugin] = usage
	if err := atomicfile.SyncDir(filepath.Dir(target)); err != nil {
		slog.Warn("plugin storage: could not sync durable blob deletion", "err", err)
	}
	return true, nil
}

func (s *DurableStore) Purge(plugin string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.usage, plugin)
	return os.RemoveAll(s.pluginDir(plugin))
}
