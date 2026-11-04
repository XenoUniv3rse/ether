package transcode

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

var errDownloadStalled = errors.New("transcode: download encoder stalled")

// downloadProgressWatch tracks meaningful output progress. The parser is
// driven by the runner's output goroutine; the monitor reads it concurrently.
type downloadProgressWatch struct {
	mu     sync.Mutex
	parser progressParser
	lines  lineWriter
	frame  int64
	out    time.Duration
	last   time.Time
	dst    io.Writer
}

func newDownloadProgressWatch(dst io.Writer) *downloadProgressWatch {
	w := &downloadProgressWatch{last: time.Now(), dst: dst}
	w.lines.fn = func(line string) {
		p, ok := w.parser.line(line)
		if !ok {
			return
		}
		w.mu.Lock()
		if p.Frame > w.frame || p.OutTime > w.out {
			w.last = time.Now()
			w.frame = p.Frame
			w.out = p.OutTime
		}
		w.mu.Unlock()
	}
	return w
}
func (w *downloadProgressWatch) Write(p []byte) (int, error) {
	_, _ = w.lines.Write(p)
	if w.dst != nil {
		return w.dst.Write(p)
	}
	return len(p), nil
}
func (w *downloadProgressWatch) idle(now time.Time) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return now.Sub(w.last)
}

// runDownloadWatched cancels and reaps a conversion that stops advancing.
// Unlike progressive output, a download's unpublished partial file can be
// discarded and retried from the start.
func (d *DownloadRemuxer) runDownloadWatched(ctx context.Context, cmd Command) error {
	timeout := d.m.opts.stallTimeout
	if timeout <= 0 {
		return d.m.runner.Run(ctx, cmd)
	}
	attempt, cancel := context.WithCancel(ctx)
	defer cancel()
	watch := newDownloadProgressWatch(cmd.Stdout)
	cmd.Stdout = watch
	done := make(chan error, 1)
	go func() { done <- d.m.runner.Run(attempt, cmd) }()
	every := timeout / 4
	if every < 100*time.Millisecond {
		every = 100 * time.Millisecond
	}
	if every > time.Second {
		every = time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			cancel()
			<-done
			return ctx.Err()
		case now := <-ticker.C:
			if watch.idle(now) >= timeout {
				select {
				case err := <-done:
					return err
				default:
				}
				cancel()
				if err := <-done; err == nil {
					return nil
				}
				return errDownloadStalled
			}
		}
	}
}
