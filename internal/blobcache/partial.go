package blobcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/atomicfile"
	"github.com/braedonsaunders/ether/internal/segcache"
)

const (
	// DefaultPartialTTL bounds how long an incomplete resumable download may
	// stay on disk without progress before Sweep evicts it.
	DefaultPartialTTL = 24 * time.Hour

	partialsDirName        = ".partials"
	partialDiskCheckStride = 1 << 20 // recheck live disk floor / shared cap every 1 MiB
)

// PartialMeta stores HTTP validator and expected-size metadata alongside an
// incomplete resumable blob. Partial blobs are never visible to Open,
// LookupKey, or CacheOpen until Commit completes.
type PartialMeta struct {
	ETag          string  `json:"etag,omitempty"`
	LastModified  string  `json:"lastModified,omitempty"`
	ContentLength int64   `json:"contentLength,omitempty"` // -1 when unknown
	Size          int64   `json:"size,omitempty"`
	UpdatedAt     int64   `json:"updatedAt,omitempty"` // UnixNano
	RangeProgress []int64 `json:"rangeProgress,omitempty"`
	RangeSource   string  `json:"rangeSource,omitempty"`
}

// RangeBounds returns the half-open interval for a partition of ContentLength.
func (m PartialMeta) RangeBounds(index int) (start, end int64) {
	count := int64(len(m.RangeProgress))
	width, extra := m.ContentLength/count, m.ContentLength%count
	i := int64(index)
	start = i*width + min(i, extra)
	return start, start + width + min(1, max(0, extra-i))
}

// stagedEnd validates the persisted byte count and finds the minimum file
// length required to cover all checkpointed bytes, including sparse writes.
func (m PartialMeta) stagedEnd() (int64, bool) {
	if m.Size <= 0 || (m.ContentLength >= 0 && m.Size > m.ContentLength) {
		return 0, false
	}
	if len(m.RangeProgress) == 0 {
		return m.Size, true
	}
	if !m.validRangeLayout() {
		return 0, false
	}
	var size, end int64
	for i, written := range m.RangeProgress {
		start, limit := m.RangeBounds(i)
		if written < 0 || written > limit-start {
			return 0, false
		}
		if written > 0 {
			end = start + written
			size += written
		}
	}
	return end, size == m.Size
}

func (m PartialMeta) validRangeLayout() bool {
	count := len(m.RangeProgress)
	return count >= 2 && count <= 8 && int64(count) <= m.ContentLength
}

// CommitResult is returned by Partial.Commit once a partial blob has been
// verified, hashed, and atomically promoted into the readable cache.
type CommitResult struct {
	Ref   Ref
	Size  int64
	CRC32 string
	MD5   string
	SHA1  string
}

type partialEntry struct {
	Holes     int64 // file length not yet written by an active positioned transfer
	ID        string
	Size      int64
	UpdatedAt int64 // UnixNano
}

func hashPartialKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (n *Namespace) partialsDir() string {
	return filepath.Join(n.dir, partialsDirName)
}

func (n *Namespace) partialDataPath(id string) string {
	return filepath.Join(n.partialsDir(), id+".partial")
}

func (n *Namespace) partialMetaPath(id string) string {
	return filepath.Join(n.partialsDir(), id+".meta.json")
}

func (n *Namespace) partialTTLLocked() time.Duration {
	if n.quota.TTL > 0 && n.quota.TTL < DefaultPartialTTL {
		return n.quota.TTL
	}
	return DefaultPartialTTL
}

func (n *Namespace) isPartialExpiredLocked(e *partialEntry, now time.Time) bool {
	if e == nil || e.UpdatedAt <= 0 {
		return false
	}
	ttl := n.partialTTLLocked()
	if ttl <= 0 {
		return false
	}
	updated := time.Unix(0, e.UpdatedAt)
	return !now.Before(updated.Add(ttl))
}

// loadPartials scans <ns.dir>/.partials on startup so surviving resumable
// downloads are accounted against namespace quotas, the shared cap, and the
// disk floor immediately, while orphaned or expired partial files are removed.
func (n *Namespace) loadPartials() {
	dir := n.partialsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	now := n.cache.now()
	dataSeen := make(map[string]int64)
	metaSeen := make(map[string]PartialMeta)
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		switch {
		case strings.HasSuffix(name, ".partial"):
			id := strings.TrimSuffix(name, ".partial")
			if len(id) != 64 {
				_ = os.Remove(filepath.Join(dir, name))
				continue
			}
			info, serr := de.Info()
			if serr != nil {
				_ = os.Remove(filepath.Join(dir, name))
				continue
			}
			dataSeen[id] = info.Size()
		case strings.HasSuffix(name, ".meta.json"):
			id := strings.TrimSuffix(name, ".meta.json")
			if len(id) != 64 {
				_ = os.Remove(filepath.Join(dir, name))
				continue
			}
			raw, rerr := os.ReadFile(filepath.Join(dir, name))
			if rerr != nil {
				_ = os.Remove(filepath.Join(dir, name))
				continue
			}
			var m PartialMeta
			if json.Unmarshal(raw, &m) != nil {
				_ = os.Remove(filepath.Join(dir, name))
				continue
			}
			metaSeen[id] = m
		default:
			_ = os.Remove(filepath.Join(dir, name))
		}
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	for id, diskSize := range dataSeen {
		meta, ok := metaSeen[id]
		end, valid := meta.stagedEnd()
		if !ok || !valid || diskSize < end {
			_ = os.Remove(n.partialDataPath(id))
			_ = os.Remove(n.partialMetaPath(id))
			delete(metaSeen, id)
			continue
		}
		if diskSize > end {
			_ = os.Truncate(n.partialDataPath(id), end)
		}
		pe := &partialEntry{ID: id, Size: end, UpdatedAt: meta.UpdatedAt}
		if pe.UpdatedAt <= 0 {
			pe.UpdatedAt = now.UnixNano()
		}
		if n.isPartialExpiredLocked(pe, now) {
			_ = os.Remove(n.partialDataPath(id))
			_ = os.Remove(n.partialMetaPath(id))
			delete(metaSeen, id)
			continue
		}
		n.partials[id] = pe
		n.partialUsed += end
		delete(metaSeen, id)
	}
	for id := range metaSeen {
		_ = os.Remove(n.partialMetaPath(id))
	}
}

func (n *Namespace) updatePartialLocked(id string, size, updatedAt int64, holes ...int64) {
	if old, ok := n.partials[id]; ok {
		n.partialUsed = max(n.partialUsed-old.Size, 0)
	}
	if size <= 0 {
		delete(n.partials, id)
		return
	}
	n.partials[id] = &partialEntry{ID: id, Size: size, UpdatedAt: updatedAt}
	if len(holes) > 0 {
		n.partials[id].Holes = holes[0]
	}
	n.partialUsed += size
}

func (n *Namespace) setPartialReservationLocked(id string, rem int64) {
	if old, ok := n.partialReservations[id]; ok {
		n.partialReserved = max(n.partialReserved-old, 0)
		delete(n.partialReservations, id)
	}
	if rem > 0 {
		n.partialReservations[id] = rem
		n.partialReserved += rem
	}
}

func (n *Namespace) evictPartial(id string) int64 {
	n.mu.Lock()
	if _, active := n.partialLocks[id]; active || n.remoteBusy("write", id) {
		n.mu.Unlock()
		return 0
	}
	pe, ok := n.partials[id]
	if !ok {
		n.mu.Unlock()
		return 0
	}
	freed := n.removePartialLocked(pe)
	n.mu.Unlock()
	return freed
}

func (n *Namespace) removePartialLocked(pe *partialEntry) int64 {
	if err := os.Remove(n.partialDataPath(pe.ID)); err != nil && !os.IsNotExist(err) {
		return 0
	}
	_ = os.Remove(n.partialMetaPath(pe.ID))
	delete(n.partials, pe.ID)
	n.partialUsed = max(n.partialUsed-pe.Size, 0)
	return pe.Size
}

func (n *Namespace) writePartialMeta(id string, meta PartialMeta) error {
	if check := n.cache.options.CheckVolume; check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(n.partialsDir(), 0o755); err != nil {
		return fmt.Errorf("blobcache: mkdir partials: %w", err)
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp := filepath.Join(n.partialsDir(), fmt.Sprintf(".%s.%d.tmp", id, n.tempSeq.Add(1)))
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, n.partialMetaPath(id)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// HostPartialBackend returns a resumable partial-blob opener suitable for
// wiring into plugins.KindBackends.CachePartial.
func (c *Cache) HostPartialBackend(quotaFor func(pluginID, libraryKey string) Quota) func(ctx context.Context, pluginID, libraryKey, key string) (*Partial, error) {
	return func(ctx context.Context, pluginID, libraryKey, key string) (*Partial, error) {
		c.BindPluginLibrary(pluginID, libraryKey)
		var q Quota
		if quotaFor != nil {
			q = quotaFor(pluginID, libraryKey)
		}
		ns := c.Namespace(libraryKey, q)
		return ns.OpenPartial(ctx, key)
	}
}

// Partial holds an exclusive handle on one incomplete resumable blob inside a
// Namespace. Bytes written to a Partial stay outside the readable blob index
// until Commit promotes the completed file into `<sha256[:2]>/<sha256>`.
type Partial struct {
	lease, staging *os.File
	n              *Namespace
	id             string
	lock           chan struct{}

	rangeMu   sync.Mutex // serializes positioned writes and checkpoints
	f         *os.File
	size      int64 // bytes written across all ranges
	fileEnd   int64 // logical file length, including any holes
	reserved  int64
	meta      PartialMeta
	committed bool
	discarded bool
	closed    bool

	capChecked    bool
	lastSharedCap int64
	sinceCapCheck int64
}

// OpenPartial acquires exclusive access to the resumable partial blob for key
// within n. If an unexpired, intact partial already exists on disk, it is
// reopened at its persisted byte offset; otherwise a fresh 0-byte partial is
// prepared. Callers must always call Close.
func (n *Namespace) OpenPartial(ctx context.Context, key string) (*Partial, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n.cache.mu.Lock()
	closed := n.cache.closed
	n.cache.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}

	if n.cache.options.CheckVolume != nil {
		if e := n.cache.options.CheckVolume(); e != nil {
			return nil, e
		}
	}
	id := hashPartialKey(key)
	var lease, staging *os.File
	if n.cache.options.Shared {
		// Try the per-key lease only while holding the volume lock. Nobody
		// waits on an unlinked inode, and a waiting node never counts itself
		// as the owner of another node's in-flight reservation.
		for {
			unlock, err := n.cache.guard(ctx)
			if err != nil {
				return nil, err
			}
			lease, err = n.lease("write", id, true)
			if err == nil {
				staging, err = os.OpenFile(filepath.Join(n.cache.dir, ".staging.lock"), os.O_RDWR, 0)
				if err == nil {
					var ok bool
					ok, err = tryFileLock(staging, false)
					if err == nil && !ok {
						err = ErrFileInUse
					}
				}
				if err != nil {
					lease.Close()
					if staging != nil {
						staging.Close()
					}
				} else {
					// A dead writer can leave a claim but cannot retain its lock.
					_ = os.Remove(filepath.Join(n.partialsDir(), id+".claim"))
					n.mu.Lock()
					n.setPartialReservationLocked(id, 0)
					n.mu.Unlock()
					defer unlock()
					break
				}
			}
			unlock()
			if !errors.Is(err, ErrFileInUse) {
				return nil, err
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	var lockCh chan struct{}
	for {
		n.mu.Lock()
		waitCh, busy := n.partialLocks[id]
		if !busy {
			lockCh = make(chan struct{})
			n.partialLocks[id] = lockCh
			n.mu.Unlock()
			break
		}
		n.mu.Unlock()
		select {
		case <-waitCh:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := os.MkdirAll(n.partialsDir(), 0755); err != nil {
		n.mu.Lock()
		delete(n.partialLocks, id)
		close(lockCh)
		n.mu.Unlock()
		if lease != nil {
			lease.Close()
			staging.Close()
		}
		return nil, fmt.Errorf("blobcache: mkdir partials: %w", err)
	}
	p := &Partial{n: n, id: id, lock: lockCh, lease: lease, staging: staging, meta: PartialMeta{ContentLength: -1}}
	// Attempt to reopen an intact, unexpired partial from disk.
	dataPath := n.partialDataPath(id)
	metaPath := n.partialMetaPath(id)
	now := n.cache.now()
	reopened := false
	if raw, err := os.ReadFile(metaPath); err == nil {
		var meta PartialMeta
		decodeErr := json.Unmarshal(raw, &meta)
		end, valid := meta.stagedEnd()
		if decodeErr == nil && valid {
			n.mu.Lock()
			expired := n.isPartialExpiredLocked(&partialEntry{ID: id, Size: meta.Size, UpdatedAt: meta.UpdatedAt}, now)
			maxBytes := n.quota.EffectiveMaxBytes()
			n.mu.Unlock()
			sharedCap := n.cache.EffectiveSharedCap()
			if !expired && end <= maxBytes && end <= sharedCap {
				if st, serr := os.Stat(dataPath); serr == nil && !st.IsDir() && st.Size() >= end {
					f, oerr := os.OpenFile(dataPath, os.O_RDWR, 0o644)
					if oerr == nil {
						ok := true
						if st.Size() > end {
							if err := f.Truncate(end); err != nil {
								ok = false
							}
						}
						if ok {
							if _, err := f.Seek(meta.Size, io.SeekStart); err != nil {
								ok = false
							}
						}
						if ok {
							p.f = f
							p.size = meta.Size
							p.fileEnd = end
							p.meta = meta
							reopened = true
							n.mu.Lock()
							n.updatePartialLocked(id, end, meta.UpdatedAt, end-meta.Size)
							n.mu.Unlock()
						} else {
							_ = f.Close()
						}
					}
				}
			}
		}
	}

	if !reopened {
		_ = os.Remove(dataPath)
		_ = os.Remove(metaPath)
		n.mu.Lock()
		n.updatePartialLocked(id, 0, 0)
		n.mu.Unlock()
	}

	return p, nil
}

// Size returns the number of bytes currently staged in the partial blob.
func (p *Partial) Size() int64 { return p.size }

// Meta returns a copy of the stored HTTP validators and expected length.
func (p *Partial) Meta() PartialMeta {
	meta := p.meta
	meta.RangeProgress = slices.Clone(meta.RangeProgress)
	return meta
}

// SetMeta updates validators and expected length without truncating staged bytes.
func (p *Partial) SetMeta(meta PartialMeta) error {
	if p.closed || p.committed {
		return ErrClosed
	}
	if len(p.meta.RangeProgress) != 0 || len(meta.RangeProgress) != 0 {
		return errors.New("blobcache: cannot update positioned partial metadata")
	}
	meta.Size = p.size
	meta.UpdatedAt = p.n.cache.now().UnixNano()
	p.meta = meta
	if p.size > 0 {
		if err := p.n.writePartialMeta(p.id, p.meta); err != nil {
			return err
		}
		p.n.mu.Lock()
		p.n.updatePartialLocked(p.id, p.fileEnd, p.meta.UpdatedAt, p.fileEnd-p.size)
		p.n.mu.Unlock()
	}
	return nil
}

// Reset discards any staged bytes, resets the partial to offset 0, and records
// new metadata for a fresh transfer.
func (p *Partial) Reset(meta PartialMeta) error {
	if len(meta.RangeProgress) != 0 {
		if !meta.validRangeLayout() {
			return errors.New("blobcache: invalid partial range layout")
		}
		meta.RangeProgress = make([]int64, len(meta.RangeProgress))
	}
	return p.reset(meta, false)
}

func (p *Partial) reset(meta PartialMeta, keepReservation bool) error {
	if check := p.n.cache.options.CheckVolume; check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	if p.closed || p.committed {
		return ErrClosed
	}
	if p.f != nil {
		_ = p.f.Close()
		p.f = nil
	}
	dataPath := p.n.partialDataPath(p.id)
	_ = os.Remove(dataPath)
	_ = os.Remove(p.n.partialMetaPath(p.id))

	if err := os.MkdirAll(p.n.partialsDir(), 0o755); err != nil {
		return fmt.Errorf("blobcache: mkdir partials: %w", err)
	}
	f, err := os.OpenFile(dataPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("blobcache: create partial: %w", err)
	}
	p.f = f
	p.size = 0
	p.fileEnd = 0
	if !keepReservation {
		if p.lease != nil {
			_ = os.Remove(filepath.Join(p.n.partialsDir(), p.id+".claim"))
		}
		p.reserved = 0
	}
	p.discarded = false
	p.capChecked = false
	p.sinceCapCheck = 0
	meta.Size = 0
	meta.UpdatedAt = p.n.cache.now().UnixNano()
	p.meta = meta

	p.n.mu.Lock()
	p.n.updatePartialLocked(p.id, 0, meta.UpdatedAt)
	if !keepReservation {
		p.n.setPartialReservationLocked(p.id, 0)
	}
	p.n.mu.Unlock()
	return p.n.writePartialMeta(p.id, p.meta)
}

// Reserve makes room for and reserves space up to total bytes for this partial
// (including any holes in the file) against the namespace quota, shared cache
// cap, and disk free-space floor. If the file or reservation already covers
// total, Reserve is a no-op. If sufficient evictable space cannot
// be freed, staged bytes and any prior reservation are preserved and
// ErrQuotaFull (matching ErrTooLarge) is returned.
func (p *Partial) Reserve(total int64) error {
	if p.closed || p.committed {
		return ErrClosed
	}
	if total <= 0 {
		return nil
	}
	p.n.cache.mu.Lock()
	closed := p.n.cache.closed
	p.n.cache.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if (!p.n.cache.options.Unified && total <= p.fileEnd) || total <= p.reserved {
		return nil
	}

	unlock, e := p.n.cache.guard(context.Background())
	if e != nil {
		return e
	}
	defer unlock()
	prevReserved := p.reserved
	prevRem := max(prevReserved-p.fileEnd, 0)
	curFootprint := max(p.fileEnd, prevReserved)
	delta := max(0, total-curFootprint)
	newRem := max(0, total-p.fileEnd)

	p.n.cache.evictMu.Lock()
	defer p.n.cache.evictMu.Unlock()

	p.n.mu.Lock()
	maxBytes := p.n.quota.EffectiveMaxBytes()
	p.n.mu.Unlock()
	if total > maxBytes {
		return fmt.Errorf("%w: partial exceeded %d bytes", ErrTooLarge, maxBytes)
	}

	sharedCap := p.n.cache.EffectiveSharedCap()
	p.capChecked = true
	p.lastSharedCap = sharedCap
	p.sinceCapCheck = 0
	if total > sharedCap {
		return quotaFullErrorf("partial exceeded cache cap %d bytes", sharedCap)
	}

	p.n.mu.Lock()
	maxBytes = p.n.quota.EffectiveMaxBytes()
	nsAccounted := p.n.used + p.n.partialUsed + p.n.partialReserved
	if total > maxBytes {
		p.n.mu.Unlock()
		return fmt.Errorf("%w: partial exceeded %d bytes", ErrTooLarge, maxBytes)
	}
	if nsAccounted+delta > maxBytes {
		p.n.mu.Unlock()
		p.n.sweep(delta, "")
		p.n.mu.Lock()
		maxBytes = p.n.quota.EffectiveMaxBytes()
		nsAccounted = p.n.used + p.n.partialUsed + p.n.partialReserved
		if total > maxBytes {
			p.n.mu.Unlock()
			return fmt.Errorf("%w: partial exceeded %d bytes", ErrTooLarge, maxBytes)
		}
		if nsAccounted+delta > maxBytes {
			p.n.mu.Unlock()
			return quotaFullErrorf("namespace quota %d bytes reached", maxBytes)
		}
	}
	p.n.mu.Unlock()
	if err := p.admit(delta); err != nil {
		return err
	}
	p.n.mu.Lock()
	p.n.setPartialReservationLocked(p.id, newRem)
	p.n.mu.Unlock()

	rollback := func() {
		p.n.mu.Lock()
		p.n.setPartialReservationLocked(p.id, prevRem)
		p.n.mu.Unlock()
	}

	sharedCap = p.n.cache.EffectiveSharedCap()
	p.lastSharedCap = sharedCap
	if total > sharedCap {
		rollback()
		return quotaFullErrorf("partial exceeded cache cap %d bytes", sharedCap)
	}
	totalAccounted := p.n.cache.totalAccounted()
	if totalAccounted > sharedCap {
		p.n.cache.enforceGlobalLimitLocked(0, nil, "")
		sharedCap = p.n.cache.EffectiveSharedCap()
		p.lastSharedCap = sharedCap
		totalAccounted = p.n.cache.totalAccounted()
		if total > sharedCap || totalAccounted > sharedCap {
			rollback()
			return quotaFullErrorf("cache cap %d bytes reached", sharedCap)
		}
	}

	if p.lease != nil {
		path := filepath.Join(p.n.partialsDir(), p.id+".claim")
		claim := strconv.FormatInt(total, 10)
		if len(p.meta.RangeProgress) > 0 {
			claim += " range"
		}
		if e := os.WriteFile(path, []byte(claim), 0600); e != nil {
			rollback()
			return e
		}
	}
	p.reserved = total
	return nil
}

// admit runs under the allocation lock. The disk floor charges sparse
// ranges for unwritten holes even after a high-offset write extends the file.
func (p *Partial) admit(delta int64) error {
	if makeRoom := p.n.cache.options.MakeFileRoom; makeRoom != nil {
		accounted, unwritten := p.n.cache.usage()
		err := makeRoom(accounted, unwritten, delta, p.n.cache.fileCandidates)
		if errors.Is(err, segcache.ErrPoolFull) {
			return quotaFullErrorf("%v", err)
		}
		return err
	}
	return nil
}

func (p *Partial) ensureSpace(add int64) error {
	p.n.cache.mu.Lock()
	closed := p.n.cache.closed
	p.n.cache.mu.Unlock()
	if closed {
		return ErrClosed
	}

	nextSize := p.fileEnd + add
	if p.n.cache.options.Unified {
		if nextSize <= p.reserved {
			// External processes may consume disk or the owner may change the
			// budget while a large transfer is underway.
			if p.sinceCapCheck >= 32<<20 {
				unlock, err := p.n.cache.guard(context.Background())
				if err != nil {
					return err
				}
				defer unlock()
				if p.reserved > p.n.cache.EffectiveSharedCap() {
					return ErrQuotaFull
				}
				if err := p.admit(0); err != nil {
					return err
				}
				p.sinceCapCheck = 0
			}
			return nil
		}
		return p.Reserve(nextSize)
	}
	curFootprint := max(p.fileEnd, p.reserved)
	nextFootprint := max(nextSize, p.reserved)
	delta := nextFootprint - curFootprint

	p.n.mu.Lock()
	maxBytes := p.n.quota.EffectiveMaxBytes()
	nsAccounted := p.n.used + p.n.partialUsed + p.n.partialReserved
	p.n.mu.Unlock()

	if nextSize > maxBytes {
		return fmt.Errorf("%w: partial exceeded %d bytes", ErrTooLarge, maxBytes)
	}
	if nsAccounted+delta > maxBytes {
		p.n.Sweep(delta)
		p.n.mu.Lock()
		maxBytes = p.n.quota.EffectiveMaxBytes()
		nsAccounted = p.n.used + p.n.partialUsed + p.n.partialReserved
		p.n.mu.Unlock()
		if nextSize > maxBytes {
			return fmt.Errorf("%w: partial exceeded %d bytes", ErrTooLarge, maxBytes)
		}
		if nsAccounted+delta > maxBytes {
			return quotaFullErrorf("namespace quota %d bytes reached", maxBytes)
		}
	}

	needCapCheck := !p.capChecked || p.sinceCapCheck >= partialDiskCheckStride || nextSize > p.lastSharedCap
	if !needCapCheck {
		if p.n.cache.totalAccounted()+delta > p.lastSharedCap {
			needCapCheck = true
		}
	}
	if needCapCheck {
		sharedCap := p.n.cache.EffectiveSharedCap()
		p.capChecked = true
		p.lastSharedCap = sharedCap
		p.sinceCapCheck = 0
		if nextSize > sharedCap {
			return quotaFullErrorf("partial exceeded cache cap %d bytes", sharedCap)
		}
		totalAccounted := p.n.cache.totalAccounted()
		if totalAccounted+delta > sharedCap {
			p.n.cache.enforceGlobalLimit(delta)
			sharedCap = p.n.cache.EffectiveSharedCap()
			p.lastSharedCap = sharedCap
			totalAccounted = p.n.cache.totalAccounted()
			if nextSize > sharedCap || totalAccounted+delta > sharedCap {
				return quotaFullErrorf("cache cap %d bytes reached", sharedCap)
			}
		}
	}
	return nil
}

// Write appends b to the partial blob while enforcing the namespace quota,
// shared cache cap, and disk free-space floor.
func (p *Partial) Write(b []byte) (int, error) {
	if p.closed || p.committed {
		return 0, ErrClosed
	}
	if len(p.meta.RangeProgress) != 0 {
		return 0, errors.New("blobcache: cannot append to a positioned partial")
	}
	if len(b) == 0 {
		return 0, nil
	}
	if err := p.ensureSpace(int64(len(b))); err != nil {
		return 0, err
	}
	if p.f == nil {
		if err := p.reset(p.meta, true); err != nil {
			return 0, err
		}
	}
	n, err := p.f.Write(b)
	p.accountWrite(n, p.fileEnd+int64(n))
	return n, err
}

// WriteRange appends bytes within one partition using a positioned write.
// Only WriteRange and Sync may be used concurrently on a Partial.
func (p *Partial) WriteRange(index int, b []byte) (int, error) {
	p.rangeMu.Lock()
	defer p.rangeMu.Unlock()
	if p.closed || p.committed || p.f == nil {
		return 0, ErrClosed
	}
	if index < 0 || index >= len(p.meta.RangeProgress) {
		return 0, errors.New("blobcache: invalid partial range index")
	}
	if len(b) == 0 {
		return 0, nil
	}
	start, end := p.meta.RangeBounds(index)
	offset := start + p.meta.RangeProgress[index]
	if int64(len(b)) > end-offset {
		return 0, errors.New("blobcache: write exceeds partial range")
	}
	if err := p.ensureSpace(max(offset+int64(len(b))-p.fileEnd, 0)); err != nil {
		return 0, err
	}
	n, err := p.f.WriteAt(b, offset)
	p.meta.RangeProgress[index] += int64(n)
	p.accountWrite(n, offset+int64(n))
	return n, err
}

func (p *Partial) accountWrite(n int, end int64) {
	if n > 0 {
		p.size += int64(n)
		// Some filesystems allocate the unwritten gaps in positioned files.
		p.fileEnd = max(p.fileEnd, end)
		p.sinceCapCheck += int64(n)
		nowNs := p.n.cache.now().UnixNano()
		p.meta.Size = p.size
		p.meta.UpdatedAt = nowNs
		p.n.mu.Lock()
		p.n.updatePartialLocked(p.id, p.fileEnd, nowNs, p.fileEnd-p.size)
		if p.reserved > 0 {
			p.n.setPartialReservationLocked(p.id, max(p.reserved-p.fileEnd, 0))
		}
		p.n.mu.Unlock()
	}
}

// Sync checkpoints staged progress and validators for a subsequent resume call.
func (p *Partial) Sync() error {
	p.rangeMu.Lock()
	defer p.rangeMu.Unlock()
	if p.size == 0 && len(p.meta.RangeProgress) != 0 {
		return nil
	}
	if p.closed || p.committed || p.discarded {
		return nil
	}
	if p.size <= 0 {
		return p.Discard()
	}
	if len(p.meta.RangeProgress) != 0 && p.f != nil {
		if err := p.f.Sync(); err != nil {
			return err
		}
	}
	nowNs := p.n.cache.now().UnixNano()
	p.meta.Size = p.size
	p.meta.UpdatedAt = nowNs
	if err := p.n.writePartialMeta(p.id, p.meta); err != nil {
		return err
	}
	p.n.mu.Lock()
	p.n.updatePartialLocked(p.id, p.fileEnd, nowNs, p.fileEnd-p.size)
	p.n.mu.Unlock()
	return nil
}

// Discard deletes the partial data and metadata files and immediately releases
// its quota accounting.
func (p *Partial) Discard() error {
	if check := p.n.cache.options.CheckVolume; check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	if p.closed || p.committed {
		return nil
	}
	if p.f != nil {
		_ = p.f.Close()
		p.f = nil
	}
	if err := os.Remove(p.n.partialDataPath(p.id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if p.lease != nil {
		_ = os.Remove(filepath.Join(p.n.partialsDir(), p.id+".claim"))
	}
	p.size = 0
	p.fileEnd = 0
	p.reserved = 0
	p.meta = PartialMeta{ContentLength: -1}
	p.discarded = true
	p.capChecked = false
	p.sinceCapCheck = 0
	_ = os.Remove(p.n.partialMetaPath(p.id))
	p.n.mu.Lock()
	p.n.updatePartialLocked(p.id, 0, 0)
	p.n.setPartialReservationLocked(p.id, 0)
	p.n.mu.Unlock()
	return nil
}

// Commit hashes the completed partial file in one pass, atomically promotes it
// into the readable blob cache under key, removes the partial metadata, sweeps
// if over quota, and returns its blob reference and digests.
func (p *Partial) Commit(ctx context.Context, key string) (CommitResult, error) {
	return p.commit(ctx, key, "", nil)
}
func (p *Partial) commit(ctx context.Context, key, mime string, streamed *Stat) (CommitResult, error) {
	if err := ctx.Err(); err != nil {
		return CommitResult{}, err
	}
	if check := p.n.cache.options.CheckVolume; check != nil {
		if err := check(); err != nil {
			return CommitResult{}, err
		}
	}
	if p.closed || p.committed {
		return CommitResult{}, ErrClosed
	}
	if len(p.meta.RangeProgress) != 0 && p.size != p.meta.ContentLength {
		return CommitResult{}, errors.New("blobcache: positioned partial is incomplete")
	}
	p.n.cache.mu.Lock()
	closed := p.n.cache.closed
	p.n.cache.mu.Unlock()
	if closed {
		return CommitResult{}, ErrClosed
	}

	p.n.mu.Lock()
	maxBytes := p.n.quota.EffectiveMaxBytes()
	p.n.mu.Unlock()
	sharedCap := p.n.cache.EffectiveSharedCap()
	limit := min(maxBytes, sharedCap)
	if p.size > maxBytes {
		return CommitResult{}, fmt.Errorf("%w: size %d exceeds limit %d", ErrTooLarge, p.size, limit)
	}
	if limit <= 0 || p.size > sharedCap {
		return CommitResult{}, quotaFullErrorf("size %d exceeds limit %d", p.size, limit)
	}

	dataPath := p.n.partialDataPath(p.id)
	if p.f == nil {
		if p.size != 0 {
			return CommitResult{}, fmt.Errorf("blobcache: partial file not open")
		}
		if err := os.MkdirAll(p.n.partialsDir(), 0o755); err != nil {
			return CommitResult{}, fmt.Errorf("blobcache: mkdir partials: %w", err)
		}
		f, err := os.OpenFile(dataPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
		if err != nil {
			return CommitResult{}, fmt.Errorf("blobcache: create empty partial: %w", err)
		}
		p.f = f
	}
	if p.n.cache.options.Unified {
		if err := p.f.Sync(); err != nil {
			return CommitResult{}, err
		}
	}
	if err := p.f.Close(); err != nil {
		p.f = nil
		return CommitResult{}, fmt.Errorf("blobcache: close partial: %w", err)
	}
	p.f = nil

	var stat Stat
	if streamed != nil {
		// A sequential Put hashes as it writes. A resumed or positioned transfer
		// must hash the final byte order before its reference can be published.
		stat = *streamed
	} else {
		rf, err := os.Open(dataPath)
		if err != nil {
			return CommitResult{}, fmt.Errorf("blobcache: open partial for hash: %w", err)
		}
		stat, err = copyWithDigests(io.Discard, &ctxReader{ctx, rf})
		rf.Close()
		if err != nil {
			return CommitResult{}, err
		}
	}
	hashed, digest := stat.Size, stat.SHA256
	if hashed != p.size {
		return CommitResult{}, fmt.Errorf("blobcache: partial size mismatch: got %d bytes, want %d", hashed, p.size)
	}
	unlock, err := p.n.cache.guard(ctx)
	if err != nil {
		return CommitResult{}, err
	}
	defer unlock()
	finalPath := p.n.blobPath(digest)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return CommitResult{}, fmt.Errorf("blobcache: mkdir shard: %w", err)
	}
	p.n.mu.Lock()
	indexed := p.n.blobs[digest] != nil
	p.n.mu.Unlock()
	marker := filepath.Join(p.n.dir, ".publishing")
	if p.n.cache.options.Unified && !indexed {
		if err := atomicfile.Write(marker, []byte("blob:"+digest), 0600); err != nil {
			return CommitResult{}, err
		}
	}
	if st, statErr := os.Stat(finalPath); indexed && statErr == nil && st.Mode().IsRegular() && st.Size() == hashed {
		// Keep the existing inode, including its open readers and allocation.
		err = os.Remove(dataPath)
	} else {
		err = os.Rename(dataPath, finalPath)
	}
	if err != nil {
		return CommitResult{}, fmt.Errorf("blobcache: publish partial: %w", err)
	}
	if p.n.cache.options.Unified && !indexed {
		if err := atomicfile.SyncDir(filepath.Dir(finalPath)); err != nil {
			return CommitResult{}, err
		}
	}
	_ = os.Remove(p.n.partialMetaPath(p.id))

	nowNs := p.n.cache.now().UnixNano()
	p.n.mu.Lock()
	p.n.updatePartialLocked(p.id, 0, 0)
	p.n.setPartialReservationLocked(p.id, 0)
	e, exists := p.n.blobs[digest]
	if !exists {
		e = &blobEntry{
			Hash:      digest,
			Size:      hashed,
			Name:      filepath.Base(key),
			CreatedAt: nowNs,
			LastOpen:  nowNs,
		}
		p.n.blobs[digest] = e
		p.n.used += hashed
	} else {
		if e.Size != hashed {
			p.n.used += hashed - e.Size
			e.Size = hashed
		}
		e.CreatedAt = nowNs
		e.LastOpen = nowNs
		if e.Name == "" && key != "" {
			e.Name = filepath.Base(key)
		}
	}
	if key != "" {
		p.n.keys[key] = digest
		if !containsString(e.Keys, key) {
			e.Keys = append(e.Keys, key)
		}
	}
	if mime != "" {
		e.MIME = mime
	}
	p.n.dirty = true
	overNamespace := p.n.used+p.n.partialUsed+p.n.partialReserved > p.n.quota.EffectiveMaxBytes()
	p.n.mu.Unlock()
	p.reserved = 0

	if overNamespace {
		p.n.sweep(0, digest)
	}
	p.n.cache.enforceGlobalLimitProtected(0, p.n, digest)
	if err := p.n.saveIndex(); err != nil {
		return CommitResult{}, err
	}
	if p.n.cache.options.Unified && !indexed {
		_ = os.Remove(marker)
	}
	if p.lease != nil {
		_ = os.Remove(filepath.Join(p.n.partialsDir(), p.id+".claim"))
	}
	p.committed = true
	rememberStat(stat)
	return CommitResult{
		Ref:   Ref("blob:" + digest),
		Size:  hashed,
		CRC32: stat.CRC32,
		MD5:   stat.MD5,
		SHA1:  stat.SHA1,
	}, nil
}

// Close flushes metadata for any surviving partial bytes, closes the file
// descriptor, and releases the exclusive per-key lock.
func (p *Partial) Close() error {
	if p.closed {
		return nil
	}
	var err error
	if !p.committed && !p.discarded {
		if p.size <= 0 {
			_ = p.Discard()
		} else {
			err = p.Sync()
		}
	}
	if p.f != nil {
		if cerr := p.f.Close(); cerr != nil && err == nil {
			err = cerr
		}
		p.f = nil
	}
	var unlock func()
	if p.lease != nil {
		var lockErr error
		unlock, lockErr = p.n.cache.lockPool(context.Background())
		if err == nil {
			err = lockErr
		}
		if unlock != nil {
			defer unlock()
		}
	}
	p.closed = true
	p.reserved = 0
	p.n.mu.Lock()
	p.n.setPartialReservationLocked(p.id, 0)
	if pe := p.n.partials[p.id]; pe != nil {
		pe.Holes = 0
	}
	if p.n.partialLocks[p.id] == p.lock {
		delete(p.n.partialLocks, p.id)
	}
	p.n.mu.Unlock()
	if p.lease != nil {
		if unlock != nil {
			_ = os.Remove(filepath.Join(p.n.partialsDir(), p.id+".claim"))
		}
		p.lease.Close()
		if unlock != nil {
			_ = os.Remove(p.n.leasePath("write", p.id))
		}
		p.staging.Close()
	}
	close(p.lock)
	return err
}

// globalPartialCandidate pairs a namespace with one evictable idle partial.
type globalPartialCandidate struct {
	ns        *Namespace
	id        string
	updatedAt int64
	size      int64
}

func (c *Cache) collectIdlePartials(nss []*Namespace) []globalPartialCandidate {
	var out []globalPartialCandidate
	for _, ns := range nss {
		ns.mu.Lock()
		for id, pe := range ns.partials {
			if _, active := ns.partialLocks[id]; active {
				continue
			}
			out = append(out, globalPartialCandidate{
				ns:        ns,
				id:        id,
				updatedAt: pe.UpdatedAt,
				size:      pe.Size,
			})
		}
		ns.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].updatedAt != out[j].updatedAt {
			return out[i].updatedAt < out[j].updatedAt
		}
		return out[i].id < out[j].id
	})
	return out
}

// sweepPartialsLocked collects expired idle partials (and, when needed, oldest
// idle partials to bring used+partialUsed+partialReserved back under target)
// for removal outside n.mu. Caller must hold n.mu.
func (n *Namespace) sweepPartialsLocked(now time.Time, target int64) {
	var active []*partialEntry
	for id, pe := range n.partials {
		if _, locked := n.partialLocks[id]; locked || n.remoteBusy("write", id) {
			continue
		}
		if n.isPartialExpiredLocked(pe, now) {
			n.removePartialLocked(pe)
			continue
		}
		cp := *pe
		active = append(active, &cp)
	}
	if n.used+n.partialUsed+n.partialReserved > target && len(active) > 0 {
		sort.Slice(active, func(i, j int) bool {
			if active[i].UpdatedAt != active[j].UpdatedAt {
				return active[i].UpdatedAt < active[j].UpdatedAt
			}
			return active[i].ID < active[j].ID
		})
		for _, pe := range active {
			if n.used+n.partialUsed+n.partialReserved <= target {
				break
			}
			n.removePartialLocked(pe)
		}
	}
}

// PartialStats returns the accounted file bytes and count of incomplete
// resumable partials in this namespace.
func (n *Namespace) PartialStats() (bytes int64, count int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.partialUsed, len(n.partials)
}
