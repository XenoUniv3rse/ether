// Disk admission shared by complete plugin files and playback segments.
// The plugin store supplies a short, pool-wide allocation lock. Segment IO
// runs outside it. A reservation covers the bytes between admission and
// publication, including writes in progress on another NAS node.
package segcache

import (
	"context"
	"errors"
	"math"
)

// FilePool supplies reservations and eviction for whole plugin files.
type FilePool interface {
	// accounted includes retained files, partials and the unwritten portion of
	// outstanding reservations. evict reclaims eligible whole files.
	WithAllocation(func(accounted, unwritten int64, evict func(need int64) error) error) error
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

func (c *Cache) trimPool() {
	c.mu.Lock()
	files := c.files
	c.mu.Unlock()
	if files == nil {
		return
	}
	err := files.WithAllocation(func(accounted, unwritten int64, evict func(int64) error) error {
		if accounted == 0 {
			return nil
		}
		need, err := c.MakeFileRoom(accounted, unwritten, 0)
		if err != nil || need == 0 {
			return err
		}
		return evict(need)
	})
	if err != nil {
		c.logger.Debug("media pool sweep", "err", err)
	}
}

// MakeFileRoom is called while the pool allocation lock is held. It returns
// the remaining shortfall after trimming eligible segments, for the file
// store to free or reject. Pure reads and streams never enter this path.
func (c *Cache) MakeFileRoom(accounted, unwritten, additional int64) (int64, error) {
	if accounted < 0 || unwritten < 0 || additional < 0 || unwritten > accounted {
		return 0, errors.New("invalid media allocation")
	}
	cap := c.MediaFileCapacity()
	if additional > cap {
		return 0, ErrTooLarge
	}
	if c.shared == nil && !c.indexed.Load() {
		// Maintenance runs again when the background index completes. A
		// writer must know about retained segments before spending its bytes.
		if additional == 0 {
			return 0, nil
		}
		if err := c.indexDisk(context.Background()); err != nil {
			return 0, err
		}
	}
	used, pending, err := c.segmentAllocation()
	if err != nil {
		return 0, err
	}
	total, err := sumSizes(used, pending, accounted, additional)
	if err != nil {
		return 0, err
	}
	shortage := max(total-cap, 0)
	c.floorMu.Lock()
	disk := c.diskFn
	c.floorMu.Unlock()
	if disk != nil {
		size, free, err := disk()
		if err != nil {
			return 0, err
		}
		if size <= 0 || free < 0 {
			return 0, errors.New("cannot measure media cache volume")
		}
		// Reserved bytes may not yet exist on disk. Protect the floor from
		// outstanding writes even when two nodes download simultaneously.
		required, err := sumSizes(MinFreeBytes(size), unwritten, pending, additional)
		if err != nil {
			return 0, err
		}
		shortage = max(shortage, required-free)
	}
	if shortage <= 0 || used == 0 {
		return shortage, nil
	}
	target := max(used-shortage, 0)
	if c.shared == nil {
		c.evictLocalTarget(false, target)
	} else {
		for {
			more, err := c.shared.evictTarget("", false, false, target)
			if err != nil {
				return 0, err
			}
			if !more {
				break
			}
		}
	}
	remaining, stillPending, err := c.segmentAllocation()
	if err != nil {
		return 0, err
	}
	// A writer can transfer its reservation to retained bytes while eviction
	// runs. Count both sides of that transfer when measuring reclaimed space.
	before := used + pending
	after, err := sumSizes(remaining, stillPending)
	if err != nil {
		return 0, err
	}
	return max(shortage-max(before-after, 0), 0), nil
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
	err := files.WithAllocation(func(accounted, unwritten int64, evict func(int64) error) error {
		need, err := c.MakeFileRoom(accounted, unwritten, size)
		if err != nil {
			return err
		}
		if need > 0 {
			if need > accounted {
				return errors.New("media cache has insufficient evictable space")
			}
			if err = evict(need); err != nil {
				return err
			}
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
		_, err = s.db.ExecContext(ctx, `INSERT INTO segcache_writes(id,bytes,expires) VALUES(?,?,`+s.nowSQL()+`+600)`, lease, size)
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
