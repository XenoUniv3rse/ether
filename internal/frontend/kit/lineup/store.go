// Package lineup keeps replacement snapshots of large, credential-free exports.
package lineup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend/kit/reqmemo"
)

type Key struct {
	Viewer     int64
	Profile    string
	Restricted bool
}

// Revision separates changes that retire the old policy from refreshes that
// can serve the last successful snapshot while a replacement is built.
type Revision struct {
	Policy, Content string
}

type Options struct {
	Viewers  func(context.Context) ([]Key, error)
	Revision func(Key) Revision
	Build    func(context.Context, Key, io.Writer) error
}

type snapshot struct {
	path    string
	rev     Revision
	at      time.Time
	bytes   int64
	refs    int
	retired bool
}

type slot struct {
	ready  *snapshot
	used   time.Time
	retry  time.Time
	urgent bool
	wait   context.Context
	err    error
}

// Store uses one builder and atomically swaps complete, private files. All
// in-memory state is per slot, independent of the number of lineup entries.
type Store struct {
	dir     string
	opt     Options
	ctx     context.Context
	end     context.CancelFunc
	done    chan struct{}
	work    chan struct{}
	mu      sync.Mutex
	slots   map[Key]*slot
	changed chan struct{}
	busy    bool
	usage   int64
}

const (
	poll     = 2 * time.Second
	debounce = 5 * time.Second
	refresh  = 2 * time.Minute
	idle     = time.Hour
	maxSlots = 128
	maxBytes = 512 << 20
	maxDisk  = 2 << 30
)

func New(parent string, opt Options) *Store {
	if parent == "" || opt.Build == nil || opt.Revision == nil {
		return nil
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil
	}
	dir, err := os.MkdirTemp(parent, "lineup-")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Store{dir: dir, opt: opt, ctx: ctx, end: cancel, done: make(chan struct{}), work: make(chan struct{}, 1), changed: make(chan struct{}), slots: make(map[Key]*slot)}
	go s.run()
	return s
}

func (s *Store) Close() {
	if s == nil {
		return
	}
	s.end()
	<-s.done
	_ = os.RemoveAll(s.dir)
}

func (s *Store) Busy() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return 1
	}
	return 0
}

func (s *Store) signal() {
	select {
	case s.work <- struct{}{}:
	default:
	}
}

func (s *Store) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// Open returns a reader for the caller's policy, following an existing build
// when needed. Only an unregistered cold slot adopts the request's tracing
// context. The caller owns closing the returned reader.
func (s *Store) Open(ctx context.Context, key Key) (io.ReadCloser, error) {
	return s.open(ctx, key, true)
}

// Wait waits for the startup worker to prepare a default snapshot.
func (s *Store) Wait(ctx context.Context, key Key) error {
	f, err := s.open(ctx, key, false)
	if f != nil {
		_ = f.Close()
	}
	return err
}

func (s *Store) open(ctx context.Context, key Key, cold bool) (io.ReadCloser, error) {
	if s == nil {
		return nil, os.ErrNotExist
	}
	for {
		rev := s.opt.Revision(key)
		s.mu.Lock()
		now := time.Now()
		e := s.slots[key]
		if e == nil && cold {
			e = s.trackLocked(key, now)
			if e == nil {
				s.mu.Unlock()
				return nil, errors.New("lineup: no free snapshot slot")
			}
			e.wait = ctx
		}
		if e != nil {
			e.used = now
			if e.ready != nil && e.ready.rev.Policy == rev.Policy {
				current := e.ready
				f, err := os.Open(current.path)
				if err == nil {
					current.refs++
					s.mu.Unlock()
					s.signal()
					return &snapshotReader{File: f, store: s, snapshot: current}, nil
				}
				s.retireLocked(e)
			}
			if e.err != nil && now.Before(e.retry) {
				err := e.err
				s.mu.Unlock()
				return nil, err
			}
			e.urgent = true
		}
		changed := s.changed
		s.mu.Unlock()
		s.signal()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.ctx.Done():
			return nil, s.ctx.Err()
		case <-changed:
		}
	}
}

func (s *Store) trackLocked(key Key, now time.Time) *slot {
	if e := s.slots[key]; e != nil {
		return e
	}
	if len(s.slots) >= maxSlots {
		return nil
	}
	e := &slot{used: now}
	s.slots[key] = e
	return e
}

func (s *Store) run() {
	defer close(s.done)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	var nextScan time.Time
	for s.ctx.Err() == nil {
		now := time.Now()
		if s.opt.Viewers != nil && !now.Before(nextScan) {
			ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
			viewers, err := s.opt.Viewers(ctx)
			cancel()
			nextScan = now.Add(30 * time.Second)
			if err == nil {
				s.mu.Lock()
				for _, key := range viewers {
					if e := s.trackLocked(key, now); e != nil {
						e.used = now
					}
				}
				s.notifyLocked()
				s.mu.Unlock()
			}
		}
		key, e, rev := s.next(now)
		if e != nil {
			s.build(key, e, rev)
			continue
		}
		select {
		case <-s.ctx.Done():
		case <-s.work:
		case <-ticker.C:
		}
	}
}

func (s *Store) next(now time.Time) (Key, *slot, Revision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var key Key
	var best *slot
	var revision Revision
	for k, e := range s.slots {
		if now.Sub(e.used) >= idle {
			s.retireLocked(e)
			delete(s.slots, k)
			continue
		}
		if now.Before(e.retry) {
			continue
		}
		rev := s.opt.Revision(k)
		missing := e.ready == nil || e.ready.rev.Policy != rev.Policy
		stale := false
		if e.ready != nil {
			age := now.Sub(e.ready.at)
			stale = age >= refresh || age >= debounce && e.ready.rev.Content != rev.Content
		}
		if !missing && !stale {
			continue
		}
		if best == nil || e.urgent && !best.urgent || e.urgent == best.urgent && e.used.After(best.used) {
			key, best, revision = k, e, rev
		}
	}
	s.busy = best != nil
	return key, best, revision
}

func (s *Store) build(key Key, e *slot, rev Revision) {
	parent := s.ctx
	// A true cache miss includes its catalogue work in the initiating
	// request's trace, even when another request also waits for that build.
	if e.wait != nil {
		parent = context.WithoutCancel(e.wait)
	}
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()

	var size int64
	out, err := os.CreateTemp(s.dir, "snapshot-")
	if err == nil {
		limited := &boundedWriter{w: out, remaining: maxBytes}
		err = s.encode(reqmemo.With(ctx), key, limited)
		size = int64(maxBytes - limited.remaining)
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
	}
	current := s.opt.Revision(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	e.wait, e.urgent, s.busy = nil, false, false
	e.err, e.retry = err, time.Time{}
	if err != nil {
		e.retry = time.Now().Add(debounce)
	}
	if out != nil {
		if err == nil && rev.Policy == current.Policy && ctx.Err() == nil {
			s.makeRoomLocked(e, size)
			if s.usage+size <= maxDisk {
				s.retireLocked(e)
				e.ready = &snapshot{path: out.Name(), rev: rev, at: time.Now(), bytes: size}
				s.usage += size
			} else {
				e.err = errors.New("lineup: snapshot disk limit reached")
				e.retry = time.Now().Add(debounce)
				_ = os.Remove(out.Name())
			}
		} else {
			_ = os.Remove(out.Name())
			e.retry = time.Now().Add(debounce)
		}
	}
	s.notifyLocked()
}

func (s *Store) encode(ctx context.Context, key Key, out io.Writer) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("lineup: build panicked: %v", p)
		}
	}()
	return s.opt.Build(ctx, key, out)
}

func (s *Store) makeRoomLocked(replacement *slot, size int64) {
	reclaim := int64(0)
	if replacement.ready != nil && replacement.ready.refs == 0 {
		reclaim = replacement.ready.bytes
	}
	for s.usage+size-reclaim > maxDisk {
		var oldestKey Key
		var oldest *slot
		for k, e := range s.slots {
			if e == replacement || e.ready == nil || e.ready.refs != 0 || e.urgent {
				continue
			}
			if oldest == nil || e.used.Before(oldest.used) {
				oldestKey, oldest = k, e
			}
		}
		if oldest == nil {
			return
		}
		s.retireLocked(oldest)
		delete(s.slots, oldestKey)
	}
	if reclaim > 0 {
		s.retireLocked(replacement)
	}
}

func (s *Store) retireLocked(e *slot) {
	if current := e.ready; current != nil {
		current.retired = true
		e.ready = nil
		if current.refs == 0 {
			s.removeLocked(current)
		}
	}
}

func (s *Store) removeLocked(current *snapshot) {
	_ = os.Remove(current.path)
	s.usage -= current.bytes
}

type snapshotReader struct {
	*os.File
	store    *Store
	snapshot *snapshot
	once     sync.Once
}

func (r *snapshotReader) Close() error {
	err := r.File.Close()
	r.once.Do(func() {
		r.store.mu.Lock()
		defer r.store.mu.Unlock()
		r.snapshot.refs--
		if r.snapshot.retired && r.snapshot.refs == 0 {
			r.store.removeLocked(r.snapshot)
		}
	})
	return err
}

type boundedWriter struct {
	w         io.Writer
	remaining int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, errors.New("lineup: snapshot too large")
	}
	n, err := w.w.Write(p)
	w.remaining -= n
	return n, err
}
