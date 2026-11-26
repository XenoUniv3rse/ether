package watchlist

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type addedTitle struct {
	viewer int64
	key    core.RatingKey
}

func (s *Service) queueAdded(viewer int64, key core.RatingKey) {
	if s.o.Resolver == nil || viewer == householdUserID {
		return
	}
	job := addedTitle{viewer, key}
	s.addMu.Lock()
	if _, exists := s.addPending[job]; !exists {
		s.addPending[job] = struct{}{}
		s.addQueue = append(s.addQueue, job)
	}
	s.addMu.Unlock()
	select {
	case s.addWake <- struct{}{}:
	default:
	}
}

// RunAddedChecks waits for a membership change on this node. It uses the
// same resolver slot and pacing as the scheduled pass, independent of Prewarm.
func (s *Service) RunAddedChecks(ctx context.Context) {
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-s.addWake:
		}
		for ctx.Err() == nil {
			s.addMu.Lock()
			if len(s.addQueue) == 0 {
				s.addQueue = nil
				s.addMu.Unlock()
				break
			}
			job := s.addQueue[0]
			s.addQueue = s.addQueue[1:]
			s.addMu.Unlock()

			s.checkAdded(ctx, job)

			s.addMu.Lock()
			delete(s.addPending, job)
			s.addMu.Unlock()
		}
	}
}

func (s *Service) checkAdded(ctx context.Context, job addedTitle) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cfg, err := s.settings(ctx)
	if err != nil || !cfg.Enabled {
		return
	}
	viewer, err := s.user(ctx, job.viewer)
	if err != nil || viewer == nil {
		return
	}
	checked := 0
	if err := s.checkTitle(ctx, cfg, viewer.ID, viewer, viewer, job.key, &checked); err != nil && ctx.Err() == nil {
		s.o.Logger.Warn("watchlist add availability check failed", "viewer", viewer.ID, "item", job.key)
	}
}
