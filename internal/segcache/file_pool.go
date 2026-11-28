// Disk admission shared by complete plugin files and playback segments.
// The plugin store supplies a short, pool-wide allocation lock. Segment IO
// runs outside it. A reservation covers the bytes between admission and
// publication, including writes in progress on another NAS node.
package segcache

import (
	"context"
	"errors"
	"math"
	"time"
)

// FileCandidate is an idle whole file or a resumable partial. Evict rechecks
// the object's lease and returns the bytes it actually reclaimed.
type FileCandidate struct {
	LastUsed time.Time
	Evict    func() (int64, error)
}

// FileCandidates lists the oldest eligible files first. It is called only
// when a write or maintenance pass needs to reclaim space.
type FileCandidates func() ([]FileCandidate, error)

var ErrPoolFull = errors.New("media cache has insufficient evictable space")

// FilePool supplies reservations and eviction for whole plugin files.
type FilePool interface {
	// accounted includes retained files, partials and the unwritten portion of
	// outstanding reservations.
	WithAllocation(func(accounted, unwritten int64, files FileCandidates) error) error
	FileUsage() (retained, reserved int64, err error)
}

// SetFilePool attaches the file store to the media cache budget.
func (c *Cache) SetFilePool(files FilePool) {
	c.mu.Lock()
	c.files = files
	c.mu.Unlock()
	if c.shared != nil || c.indexed.Load() {
		c.trimPool()
	}
}

// MediaFileCapacity bounds one object before admission accounts for other
// media and the volume's free-space floor. Zero budget means unlimited.
func (c *Cache) MediaFileCapacity() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.policy.Mode == "off" {
		return 0
	}
	if c.budget == 0 {
		return math.MaxInt64
	}
	return c.budget
}

func (c *Cache) trimPool() bool { return c.trimPoolWatermarked(false) }
func (c *Cache) trimPoolWatermarked(watermarked bool) bool {
	c.mu.Lock()
	files := c.files
	c.mu.Unlock()
	if files == nil {
		return false
	}
	err := files.WithAllocation(func(accounted, unwritten int64, candidates FileCandidates) error {
		// Retain the segment worker's 95%/90% hysteresis and its last-
		// segment exception when there is no other class to compare.
		if watermarked && accounted == 0 {
			defer c.publishFileUsage(0)()
			if c.shared != nil {
				c.shared.sweepWatermarked()
			} else {
				c.evictLocal(true)
			}
			return nil
		}
		return c.makeRoom(accounted, unwritten, 0, candidates, watermarked)
	})
	if err != nil {
		c.logger.Debug("media pool sweep", "err", err)
	}
	return true
}

// MakeFileRoom is called under the pool allocation lock. Playback and
// plugin-file admissions use the same order when pressure requires eviction.
func (c *Cache) MakeFileRoom(accounted, unwritten, additional int64, candidates FileCandidates) error {
	return c.makeRoom(accounted, unwritten, additional, candidates, false)
}

func (c *Cache) makeRoom(accounted, unwritten, additional int64, candidates FileCandidates, watermarked bool) error {
	if accounted < 0 || unwritten < 0 || additional < 0 || unwritten > accounted {
		return errors.New("invalid media allocation")
	}
	defer c.publishFileUsage(accounted - unwritten)()
	cap := c.MediaFileCapacity()
	if additional > cap {
		return ErrTooLarge
	}
	if c.shared == nil && !c.indexed.Load() {
		// Maintenance runs again when the background index completes. A
		// writer must know about retained segments before spending its bytes.
		if additional == 0 {
			return nil
		}
		if err := c.indexDisk(context.Background()); err != nil {
			return err
		}
	}
	used, pending, err := c.segmentAllocation()
	if err != nil {
		return err
	}
	total, err := sumSizes(used, pending, accounted, additional)
	if err != nil {
		return err
	}
	shortage := max(total-cap, 0)
	if watermarked && cap > 0 && used >= highWatermark(cap) {
		shortage = max(shortage, total-lowWatermark(cap))
	}
	diskShort, err := c.volumeShortfall(used, unwritten, pending, additional)
	if err != nil {
		return err
	}
	shortage = max(shortage, diskShort)
	if shortage <= 0 {
		return nil
	}
	var files []FileCandidate
	if accounted > 0 && candidates != nil {
		files, err = candidates()
		if err != nil {
			return err
		}
	}
	// Walk the sorted file list. At each boundary, evict only playback
	// segments ranked older than that file. Its whole size is reclaimed
	// only if the older eligible segments did not meet the shortfall.
	for _, file := range files {
		if used > 0 {
			reclaimed, err := c.evictSegmentsBefore(shortage, file.LastUsed)
			if err != nil {
				return err
			}
			shortage -= reclaimed
			used -= reclaimed
		}
		if shortage <= 0 {
			return nil
		}
		reclaimed, err := file.Evict()
		if err != nil {
			return err
		}
		shortage -= reclaimed
		if shortage <= 0 {
			return nil
		}
	}
	if used > 0 {
		reclaimed, err := c.evictSegmentsBefore(shortage, time.Time{})
		if err != nil {
			return err
		}
		shortage -= reclaimed
	}
	if shortage > 0 {
		return ErrPoolFull
	}
	return nil
}

func (c *Cache) publishFileUsage(retained int64) func() {
	c.poolUsage.Store(&retained)
	return func() { c.poolUsage.CompareAndSwap(&retained, nil) }
}

func (c *Cache) evictSegmentsBefore(need int64, cutoff time.Time) (int64, error) {
	if c.shared == nil {
		return c.evictLocalTarget(false, need, cutoff)
	}
	var freed int64
	for freed < need {
		more, n, err := c.shared.evictTarget("", false, false, need-freed, cutoff)
		freed += n
		if err != nil || !more {
			return freed, err
		}
	}
	return freed, nil
}

// A fresh volume measurement is required before promising space to a writer.
// Publish the segment headroom to the playback planners as well.
func (c *Cache) volumeShortfall(used, unwritten, pending, additional int64) (int64, error) {
	c.floorMu.Lock()
	defer c.floorMu.Unlock()
	if c.diskFn == nil {
		return 0, nil
	}
	size, free, err := c.diskFn()
	if err != nil {
		return 0, err
	}
	if size <= 0 || free < 0 {
		return 0, errors.New("cannot measure media cache volume")
	}
	floor := MinFreeBytes(size)
	required, err := sumSizes(floor, unwritten, pending, additional)
	if err != nil {
		return 0, err
	}
	now := c.now()
	headroom := free - (required - additional)
	capacity := max(used+min(headroom, math.MaxInt64-used), 0)
	c.floor = floorStat{ok: true, at: now, cap: capacity, floor: floor, free: free}
	c.mu.Lock()
	c.floorEnabled = true
	c.floorOKSnap = true
	c.floorCapSnap = capacity
	c.floorAtNs = now.UnixNano()
	c.mu.Unlock()
	return max(required-free, 0), nil
}

func sumSizes(values ...int64) (int64, error) {
	var total int64
	for _, n := range values {
		if n < 0 || n > math.MaxInt64-total {
			return 0, ErrTooLarge
		}
		total += n
	}
	return total, nil
}

func (c *Cache) segmentAllocation() (used, pending int64, err error) {
	if c.shared == nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.used, c.segmentReserved, nil
	}
	s := c.shared
	if err = s.checkMount(); err != nil {
		return
	}
	ctx, cancel := s.context()
	defer cancel()
	err = s.db.QueryRowContext(ctx, `SELECT used, (SELECT COALESCE(SUM(bytes),0) FROM segcache_writes WHERE expires>`+s.nowSQL()+`) FROM segcache_pool WHERE id=1`).Scan(&used, &pending)
	return
}

func (c *Cache) reserveSegment(files FilePool, size int64) (string, error) {
	var lease string
	err := files.WithAllocation(func(accounted, unwritten int64, candidates FileCandidates) error {
		if err := c.MakeFileRoom(accounted, unwritten, size, candidates); err != nil {
			return err
		}
		if c.shared == nil {
			c.mu.Lock()
			c.segmentReserved += size
			c.mu.Unlock()
			return nil
		}
		s := c.shared
		lease = randomName()
		ctx, cancel := s.context()
		defer cancel()
		_, err := s.db.ExecContext(ctx, `INSERT INTO segcache_writes(id,bytes,expires) VALUES(?,?,`+s.nowSQL()+`+600)`, lease, size)
		return err
	})
	return lease, err
}

func (c *Cache) endReservation(lease string, size int64) {
	if c.shared == nil {
		c.mu.Lock()
		c.segmentReserved -= size
		c.mu.Unlock()
		return
	}
	s := c.shared
	ctx, cancel := s.context()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM segcache_writes WHERE id=?`, lease)
	s.log(err)
}
