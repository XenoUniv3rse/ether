package livetv

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

// withProviderCapacity admits a viewer without holding the queue during
// media startup. Background recovery only attempts immediate acquisition.
func (s *Service) withProviderCapacity(ctx context.Context, c Channel, src StreamSource, admit func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := admit(ctx)
	if !errors.Is(err, ErrProviderBusy) {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	account := providerSourceID(c, src)
	entry, lookupErr := s.sourceEntry(ctx, account)
	if lookupErr == nil && entry.slotWait > 0 {
		err = s.providerWait.wait(ctx, account, entry.slotWait, err, admit)
	} else if s.preemptIdleProvider(account, c.ID) && ctx.Err() == nil {
		err = admit(ctx)
	}
	if errors.Is(err, ErrProviderBusy) && entry.limit > 0 {
		used := max(s.inputRelay.taps.inUse(account), entry.limit)
		s.emitProviderLimitReached(account, used, entry.limit, c.ID)
	}
	return err
}

type providerWaitQueue struct {
	mu      sync.Mutex
	sources map[string]*list.List
}

// Only the head retries. Periodic retries also notice durable login slots
// released by another process, which cannot signal this in-memory queue.
func (q *providerWaitQueue) wait(ctx context.Context, account string, budget time.Duration, busy error, admit func(context.Context) error) error {
	wait, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	q.mu.Lock()
	queue := q.sources[account]
	if queue == nil {
		if q.sources == nil {
			q.sources = map[string]*list.List{}
		}
		queue = list.New()
		q.sources[account] = queue
	}
	turn := make(chan struct{})
	elem := queue.PushBack(turn)
	if queue.Front() == elem {
		close(turn)
	}
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		head := queue.Front() == elem
		queue.Remove(elem)
		if next := queue.Front(); next == nil {
			delete(q.sources, account)
		} else if head {
			close(next.Value.(chan struct{}))
		}
		q.mu.Unlock()
	}()
	select {
	case <-turn:
	case <-wait.Done():
	}
	tick := time.NewTicker(75 * time.Millisecond)
	defer tick.Stop()
	for wait.Err() == nil {
		if err := admit(wait); !errors.Is(err, ErrProviderBusy) {
			if err == nil || wait.Err() == nil {
				return err
			}
			break
		}
		select {
		case <-tick.C:
		case <-wait.Done():
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return busy
}

func (s *Service) preemptIdleProvider(account, exceptChannel string) bool {
	if account == "" {
		return false
	}
	// Inspect only current provider reservations, never the library/lineup.
	s.mu.Lock()
	channels := map[string]bool{}
	for _, channel := range s.broadcastProviders[account] {
		if channel != exceptChannel {
			channels[channel] = true
		}
	}
	s.mu.Unlock()
	r := &s.broadcasts
	r.mu.Lock()
	waiting := map[*broadcast]bool{}
	for b, key := range r.retiring {
		if channels[key.channelID] {
			waiting[b] = true
		}
	}
	for key, b := range r.outputs {
		if !channels[key.channelID] {
			continue
		}
		b.mu.Lock()
		// A new output is awaiting its first viewer. Only previously attached
		// outputs qualify for preemption, even immediately after detachment.
		if b.viewers == 0 && b.attaching == 0 && b.hadViewer && !b.closed {
			b.closed = true
			b.cancel()
			delete(r.outputs, key)
			if r.retiring == nil {
				r.retiring = map[*broadcast]broadcastKey{}
			}
			r.retiring[b] = key
			waiting[b] = true
		}
		b.mu.Unlock()
	}
	r.mu.Unlock()
	for b := range waiting {
		b.close()
		r.mu.Lock()
		delete(r.retiring, b)
		r.mu.Unlock()
	}
	return len(waiting) > 0
}
