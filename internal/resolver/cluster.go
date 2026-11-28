package resolver

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
)

// Cluster cache coherence. The resolver mirrors resolver_bad and
// resolver_reputation in memory and reads resolver_availability and
// resolver_verified through a cache; when another node writes them it
// publishes a hint and this node drops or reloads the affected entries:
//
//	bad:<release id>   reload that release's bad mark, forget its verification
//	grabfail:<cid>     reload cross-indexer failed-grab mark
//	avail:<rating key> forget the cached availability (re-read on demand)
//	reps               reload indexer reputations
//	pin:<rating key>   reload that title's owner pin (pinned, unpinned or
//	                   its last status changed)
//	vchoice:<uid>:<rk> reload that viewer's remembered version choice

// SetBus connects the cluster bus (nil disconnects). A standalone server
// never calls it.
func (s *Service) SetBus(bus clusterbus.Bus) {
	iv := clusterbus.NewInvalidator(bus, "resolver", s.log, s.applyRemote)
	s.invalMu.Lock()
	old := s.inval
	s.inval = iv
	s.invalMu.Unlock()
	old.Close()
}

func (s *Service) notify(hint string) {
	s.invalMu.Lock()
	iv := s.inval
	s.invalMu.Unlock()
	iv.Notify(hint)
}

// notifyPin tells the other nodes a title's pin changed.
func (s *Service) notifyPin(key core.RatingKey) {
	s.notify("pin:" + strconv.FormatInt(int64(key), 10))
}

func (s *Service) applyRemote(ctx context.Context, hints []string, all bool) {
	if all {
		s.mu.Lock()
		s.subVerdicts = nil
		s.subMiss = nil
		s.bad = map[string]badMark{}
		s.badContent = map[string]badMark{}
		s.contentByRelease = map[string]contentLink{}
		s.failedGrabs = map[string]failedGrabMark{}
		s.reps = map[string]reputation{}
		s.avail = map[int64]availEntry{}
		s.clearAvailabilityEpochLocked()
		s.verified = map[string]time.Time{}
		s.verifiedMiss = nil
		clear(s.seaDexMem)
		clear(s.seaDexDBMiss)
		clear(s.seaDexKeys)
		clear(s.seaDexKeyMiss)
		s.mu.Unlock()
		s.clearReleaseTitlesMem()
		s.loadPersisted(ctx)
		return
	}
	reps := false
	for _, h := range hints {
		kind, arg, _ := strings.Cut(h, ":")
		switch kind {
		case "sub":
			s.mu.Lock()
			delete(s.subVerdicts, arg)
			delete(s.subMiss, arg)
			s.mu.Unlock()
		case "rejections":
			if key, err := strconv.ParseInt(arg, 10, 64); err == nil {
				s.loadRejectionsFor(ctx, core.RatingKey(key))
			} else {
				s.loadRejections(ctx)
			}
		case "bad":
			s.reloadBad(ctx, arg)
		case "grabfail":
			s.reloadFailedGrab(ctx, arg)
		case "avail":
			if k, err := strconv.ParseInt(arg, 10, 64); err == nil {
				s.mu.Lock()
				delete(s.avail, k)
				s.forgetAvailabilityEpochLocked(k)
				s.mu.Unlock()
			}
		case "reps":
			reps = true
		case "pin":
			if k, err := strconv.ParseInt(arg, 10, 64); err == nil {
				s.reloadPin(ctx, core.RatingKey(k))
			}
		case "vchoice":
			s.reloadViewerChoice(ctx, arg)
		case "seadex":
			if k, err := strconv.ParseInt(arg, 10, 64); err == nil {
				s.mu.Lock()
				delete(s.seaDexMem, k)
				delete(s.seaDexDBMiss, k)
				s.mu.Unlock()
			}
		case "reltitle":
			s.loadReleaseTitles(ctx)
		}
	}
	if reps {
		s.reloadReps(ctx)
	}
}

func (s *Service) reloadBad(ctx context.Context, id string) {
	if s.db == nil || id == "" {
		return
	}
	var cid, reason string
	var at int64
	var manual int
	err := s.db.QueryRowContext(ctx, `SELECT content_id, reason, updated_at, manual FROM resolver_bad WHERE release_id=?`, id).Scan(&cid, &reason, &at, &manual)
	contentKeys := s.idContentKeysDB(ctx, id)
	if cid != "" {
		contentKeys = appendUniqueKey(contentKeys, cid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.verified, id)
	for _, k := range contentKeys {
		delete(s.verified, k)
	}
	switch {
	case err == nil:
		mark := badMark{reason: reason, at: time.Unix(at, 0), manual: manual == 1}
		s.bad[id] = mark
		s.markBadContentLocked(contentKeys, mark)
	case errors.Is(err, sql.ErrNoRows):
		delete(s.bad, id) // unblocked on another node
		for _, k := range contentKeys {
			delete(s.badContent, k)
		}
	}
}

func (s *Service) reloadReps(ctx context.Context) {
	if s.db == nil {
		return
	}
	rows, err := s.db.QueryContext(ctx, `SELECT indexer, successes, failures FROM resolver_reputation`)
	if err != nil {
		return
	}
	defer rows.Close()
	reps := map[string]reputation{}
	for rows.Next() {
		var name string
		var rep reputation
		if rows.Scan(&name, &rep.successes, &rep.failures) == nil {
			reps[name] = rep
		}
	}
	if rows.Err() != nil {
		return
	}
	s.mu.Lock()
	s.reps = reps
	s.mu.Unlock()
}
