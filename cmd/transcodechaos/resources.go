package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/transcode"
)

type trackingRunner struct {
	inner     transcode.Runner
	mu        sync.Mutex
	processes []transcode.Process
}

func (r *trackingRunner) Run(ctx context.Context, c transcode.Command) error {
	return r.inner.Run(ctx, c)
}
func (r *trackingRunner) Start(c transcode.Command) (transcode.Process, error) {
	p, e := r.inner.Start(c)
	if e == nil {
		r.mu.Lock()
		r.processes = append(r.processes, p)
		r.mu.Unlock()
	}
	return p, e
}
func (r *trackingRunner) check() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.processes {
		select {
		case <-p.Done():
		default:
			return fmt.Errorf("encoder process still running after stop")
		}
		if sample, ok := p.(interface{ PeakRSSBytes() (uint64, bool) }); ok {
			if n, valid := sample.PeakRSSBytes(); valid && n > 1<<30 {
				return fmt.Errorf("encoder peak RSS %d exceeds 1 GiB", n)
			}
		}
	}
	return nil
}
func diskBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !d.IsDir() {
			st, e := d.Info()
			if e != nil {
				return e
			}
			total += st.Size()
		}
		return nil
	})
	return total, err
}
func fdCount() int {
	v, err := os.ReadDir("/dev/fd")
	if err != nil {
		return -1
	}
	return len(v)
}

type resourceBaseline struct {
	goroutines, fds int
	mem             uint64
}

func takeBaseline() resourceBaseline {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return resourceBaseline{runtime.NumGoroutine(), fdCount(), m.Sys}
}
func (b resourceBaseline) check(root string, runner *trackingRunner) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := runner.check(); err != nil {
			if time.Now().Before(deadline) {
				time.Sleep(25 * time.Millisecond)
				continue
			}
			return err
		}
		break
	}
	n, err := diskBytes(root)
	if err != nil {
		return err
	}
	if n != 0 {
		return fmt.Errorf("%d transcode temp bytes remain after stop", n)
	}
	// Allow small runtime/HTTP housekeeping variations, not an accumulating leak.
	if g := runtime.NumGoroutine(); g > b.goroutines+12 {
		return fmt.Errorf("goroutines grew from %d to %d", b.goroutines, g)
	}
	if f := fdCount(); f >= 0 && b.fds >= 0 && f > b.fds+8 {
		return fmt.Errorf("file descriptors grew from %d to %d", b.fds, f)
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	if m.Sys > b.mem+512<<20 {
		return fmt.Errorf("Go memory grew by %d bytes", m.Sys-b.mem)
	}
	return nil
}
