package resolver

import (
	"context"
	"time"
)

type requestBookkeepingKey struct{}

type bookkeepingJob struct {
	write   func(context.Context)
	history []historyWork
}

// Client admission must not wait for optional evidence/history writes behind
// the catalog's SQLite writer. Keep a bounded queue and one lazy worker; idle
// resolvers have no worker. Memory state is updated before enqueuing persistence.
func (s *Service) persistBookkeeping(ctx context.Context, write func(context.Context)) {
	s.enqueueBookkeeping(ctx, write, false)
}

// A full optional-bookkeeping queue must not discard the selected release.
func (s *Service) persistChoice(ctx context.Context, write func(context.Context)) {
	s.enqueueBookkeeping(ctx, write, true)
}

func (s *Service) enqueueBookkeeping(ctx context.Context, write func(context.Context), required bool) {
	if foreground, _ := ctx.Value(requestBookkeepingKey{}).(bool); !foreground || s.db == nil {
		write(ctx)
		return
	}
	parent := context.WithValue(context.WithoutCancel(ctx), requestBookkeepingKey{}, false)
	job := func(_ context.Context) {
		wctx, cancel := context.WithTimeout(parent, 2*time.Second)
		stop := context.AfterFunc(s.bgCtx, cancel)
		defer stop()
		defer cancel()
		write(wctx)
	}
	if s.queueBookkeeping(bookkeepingJob{write: job}) && required {
		job(ctx)
	}
}

// queueBookkeeping reports saturation. A closed resolver drops all work.
func (s *Service) queueBookkeeping(job bookkeepingJob) bool {
	s.bgMu.Lock()
	if s.bgClosed {
		s.bgMu.Unlock()
		return false
	}
	const limit = 64
	units := max(1, len(job.history))
	if s.bookkeepingSize+units > limit {
		s.bgMu.Unlock()
		s.log.Debug("resolver bookkeeping queue full")
		return true
	}
	s.bookkeepingSize += units
	if len(job.history) != 0 && len(s.bookkeeping) != 0 {
		tail := &s.bookkeeping[len(s.bookkeeping)-1]
		const historyBatchSize = 32
		if len(tail.history) > 0 && len(tail.history)+len(job.history) <= historyBatchSize {
			tail.history = append(tail.history, job.history...)
			s.bgMu.Unlock()
			return false
		}
	}
	s.bookkeeping = append(s.bookkeeping, job)
	if s.bookkeepingRunning {
		s.bgMu.Unlock()
		return false
	}
	s.bookkeepingRunning = true
	s.bookkeepingIdle = make(chan struct{})
	s.bgWG.Add(1)
	s.bgMu.Unlock()
	go func() {
		defer s.bgWG.Done()
		for {
			s.bgMu.Lock()
			if s.bgClosed || len(s.bookkeeping) == 0 {
				s.bookkeeping = nil
				s.bookkeepingSize = 0
				s.bookkeepingRunning = false
				close(s.bookkeepingIdle)
				s.bgMu.Unlock()
				return
			}
			next := s.bookkeeping[0]
			s.bookkeepingSize -= max(1, len(next.history))
			s.bookkeeping[0] = bookkeepingJob{}
			s.bookkeeping = s.bookkeeping[1:]
			s.bgMu.Unlock()
			if len(next.history) != 0 {
				s.writeHistory(s.bgCtx, next.history)
			} else {
				next.write(s.bgCtx)
			}
		}
	}()
	return false
}

// flushBookkeeping is a deterministic barrier for evidence readers in tests.
// It waits for the current queue to drain, without joining unrelated discovery.
func (s *Service) flushBookkeeping(ctx context.Context) error {
	s.bgMu.Lock()
	idle := s.bookkeepingIdle
	s.bgMu.Unlock()
	if idle == nil {
		return nil
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
