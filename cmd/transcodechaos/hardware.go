package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/braedonsaunders/ether/internal/transcode"
)

// hardwareFaultRunner injects one hardware-encoder error after the client has
// received a segment. The real process still does the encoding; -re prevents
// this short fixture from finishing before the client can inject the fault.
type hardwareFaultRunner struct {
	mu             sync.Mutex
	hw             *hardwareFaultProcess
	softwareStarts int
}
type lockedWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.out == nil {
		return len(p), nil
	}
	return w.out.Write(p)
}

type hardwareFaultProcess struct {
	transcode.Process
	failed atomic.Bool
	stderr *lockedWriter
}

func (p *hardwareFaultProcess) Err() error {
	if p.failed.Load() {
		return errors.New("injected hardware encoder failure")
	}
	return p.Process.Err()
}
func (r *hardwareFaultRunner) Run(ctx context.Context, c transcode.Command) error {
	return (transcode.ExecRunner{}).Run(ctx, c)
}
func (r *hardwareFaultRunner) Start(c transcode.Command) (transcode.Process, error) {
	hardware := false
	software := false
	for _, a := range c.Args {
		if strings.Contains(a, "h264_videotoolbox") {
			hardware = true
		}
		if a == "libx264" {
			software = true
		}
	}
	if hardware {
		for i, a := range c.Args {
			if a == "-i" {
				c.Args = append(c.Args[:i], append([]string{"-re"}, c.Args[i:]...)...)
				break
			}
		}
	}
	stderr := &lockedWriter{out: c.Stderr}
	c.Stderr = stderr
	p, err := (transcode.ExecRunner{}).Start(c)
	if err != nil {
		return nil, err
	}
	wrapped := &hardwareFaultProcess{Process: p, stderr: stderr}
	r.mu.Lock()
	if hardware {
		r.hw = wrapped
	}
	if software {
		r.softwareStarts++
	}
	r.mu.Unlock()
	return wrapped, nil
}
func (r *hardwareFaultRunner) fail() error {
	r.mu.Lock()
	p := r.hw
	r.mu.Unlock()
	if p == nil {
		return errors.New("no hardware encoder was started")
	}
	select {
	case <-p.Done():
		return errors.New("hardware encoder exited before injection")
	default:
	}
	p.failed.Store(true)
	fmt.Fprintln(p.stderr, "videotoolbox encoder injected failure")
	p.Process.Stop()
	return nil
}
func (r *hardwareFaultRunner) software() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.softwareStarts > 0
}
