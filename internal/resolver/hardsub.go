package resolver

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("resolver", 4, `
CREATE TABLE resolver_hardsub (release_id TEXT PRIMARY KEY, burned INTEGER NOT NULL);
CREATE TABLE resolver_rejected (rating_key INTEGER NOT NULL, release_id TEXT NOT NULL, release_json TEXT NOT NULL, PRIMARY KEY(rating_key,release_id));
`)
}

// SubtitleVerdict is shared by every title and viewer. Unknown is deliberately
// distinct from clean: failed or cancelled probes must be retried.
func (s *Service) SubtitleVerdict(ctx context.Context, id string) (bool, bool) {
	s.mu.RLock()
	v, ok := s.subVerdicts[id]
	missed := s.subMiss[id]
	s.mu.RUnlock()
	if ok || s.db == nil {
		return v, ok
	}
	if !missed.IsZero() && s.now().Sub(missed) < time.Minute {
		return false, false
	}
	var n int
	if s.db.QueryRowContext(ctx, `SELECT burned FROM resolver_hardsub WHERE release_id=?`, id).Scan(&n) != nil {
		s.mu.Lock()
		if len(s.subMiss) >= 10000 {
			clear(s.subMiss)
		}
		if s.subMiss == nil {
			s.subMiss = map[string]time.Time{}
		}
		s.subMiss[id] = s.now()
		s.mu.Unlock()
		return false, false
	}
	s.mu.Lock()
	if s.subVerdicts == nil {
		s.subVerdicts = map[string]bool{}
	}
	if len(s.subVerdicts) >= 10000 {
		clear(s.subVerdicts)
	}
	s.subVerdicts[id] = n != 0
	s.mu.Unlock()
	return n != 0, true
}

// subtitleVerdicts reads only these candidates, once per bounded SQL batch.
// Keep the result local to the ranking pass: a shared-cache eviction must not
// turn the subsequent candidate loop back into scalar database reads.
func (s *Service) subtitleVerdicts(ctx context.Context, ids []string) map[string]bool {
	now := s.now()
	out := make(map[string]bool, len(ids))
	var missing []string
	s.mu.RLock()
	for _, id := range ids {
		if v, ok := s.subVerdicts[id]; ok {
			out[id] = v
		} else if at := s.subMiss[id]; at.IsZero() || now.Before(at) || now.Sub(at) >= time.Minute {
			missing = append(missing, id)
		}
	}
	s.mu.RUnlock()
	if s.db == nil {
		return out
	}
	slices.Sort(missing)
	missing = slices.Compact(missing)
	for batch := range store.Chunks(missing, 500) {
		args := make([]any, len(batch))
		for n, id := range batch {
			args[n] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT release_id,burned FROM resolver_hardsub WHERE release_id IN (`+store.Placeholders(len(batch))+`)`, args...)
		if err != nil {
			continue
		}
		found := map[string]bool{}
		for rows.Next() {
			var id string
			var burned int
			if err = rows.Scan(&id, &burned); err != nil {
				break
			}
			found[id] = burned != 0
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			continue
		}
		s.mu.Lock()
		if s.subVerdicts == nil {
			s.subVerdicts = map[string]bool{}
		}
		if s.subMiss == nil {
			s.subMiss = map[string]time.Time{}
		}
		if len(s.subVerdicts)+len(found) > 10000 {
			clear(s.subVerdicts)
		}
		if len(s.subMiss)+len(batch) > 10000 {
			clear(s.subMiss)
		}
		for _, id := range batch {
			// A probe may have completed while the batch was being read.
			if v, ok := s.subVerdicts[id]; ok {
				out[id] = v
				continue
			}
			if v, ok := found[id]; ok {
				out[id], s.subVerdicts[id] = v, v
				delete(s.subMiss, id)
			} else {
				s.subMiss[id] = now
			}
		}
		s.mu.Unlock()
	}
	return out
}

func (s *Service) RememberSubtitleVerdict(ctx context.Context, id string, v bool) error {
	if s.db != nil {
		now := s.now().Unix()
		if _, err := s.db.ExecContext(ctx, `INSERT INTO resolver_hardsub(release_id,burned,updated_at) VALUES (?,?,?) ON CONFLICT(release_id) DO UPDATE SET burned=excluded.burned,updated_at=excluded.updated_at`, id, store.BoolInt(v), now); err != nil {
			return err
		}
	}
	s.mu.Lock()
	if s.subVerdicts == nil {
		s.subVerdicts = map[string]bool{}
	}
	if len(s.subVerdicts) >= 10000 {
		clear(s.subVerdicts)
	}
	s.subVerdicts[id] = v
	delete(s.subMiss, id)
	s.mu.Unlock()
	s.notify("sub:" + id)
	return nil
}
func (s *Service) hardSubbed(ctx context.Context, r core.Release) bool {
	v, _ := s.SubtitleVerdict(ctx, r.ID)
	return v || releaseparse.HardSubbed(r.Name)
}

// RejectRelease remembers a deliberate rejection for a title across viewers.
// It survives source search expiry, restart and a provider assigning a new ID.
func (s *Service) RejectRelease(ctx context.Context, key core.RatingKey, r core.Release) error {
	if key == 0 || r.ID == "" {
		return core.ErrNotFound
	}
	if s.db != nil {
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err = s.db.ExecContext(ctx, `INSERT INTO resolver_rejected VALUES (?,?,?) ON CONFLICT(rating_key,release_id) DO UPDATE SET release_json=excluded.release_json`, int64(key), r.ID, string(raw)); err != nil {
			return err
		}
	}
	s.mu.Lock()
	if s.rejections == nil {
		s.rejections = map[core.RatingKey][]core.Release{}
	}
	found := false
	for i, old := range s.rejections[key] {
		if old.ID == r.ID {
			s.rejections[key][i] = r
			found = true
			break
		}
	}
	if !found {
		s.rejections[key] = append(s.rejections[key], r)
	}
	s.mu.Unlock()
	s.notify("rejections")
	return s.forgetAvailability(ctx, key)
}
func nearRelease(a, b core.Release) bool {
	if a.ID != "" && a.ID == b.ID || a.InfoHash != "" && strings.EqualFold(a.InfoHash, b.InfoHash) || a.GUID != "" && a.GUID == b.GUID && a.Indexer != "" && a.Indexer == b.Indexer || a.SourceHash != "" && a.SourceHash == b.SourceHash {
		return true
	}
	an, bn := strings.Join(releaseparse.Tokens(releaseBase(a.Name)), ""), strings.Join(releaseparse.Tokens(releaseBase(b.Name)), "")
	if an != "" && an == bn {
		return true
	}
	ag, bg := a.Info.Group, b.Info.Group
	if ag == "" {
		ag = releaseparse.Parse(a.Name).Group
	}
	if bg == "" {
		bg = releaseparse.Parse(b.Name).Group
	}
	// Group alone is insufficient: a group can publish genuinely different cuts.
	return ag != "" && strings.EqualFold(ag, bg) && a.Size > 0 && b.Size > 0 && a.Size/(64<<20) == b.Size/(64<<20)
}
func (s *Service) rejectedFor(key core.RatingKey, r core.Release) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, old := range s.rejections[key] {
		if nearRelease(old, r) {
			return true
		}
	}
	return false
}
func (s *Service) loadRejections(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT rating_key,release_json FROM resolver_rejected`)
	if err != nil {
		return
	}
	defer rows.Close()
	snapshot := map[core.RatingKey][]core.Release{}
	for rows.Next() {
		var k core.RatingKey
		var raw string
		var r core.Release
		if rows.Scan(&k, &raw) == nil && json.Unmarshal([]byte(raw), &r) == nil {
			snapshot[k] = append(snapshot[k], r)
		}
	}
	if rows.Err() == nil {
		s.mu.Lock()
		s.rejections = snapshot
		s.mu.Unlock()
	}
}

// TitleReleaseRejected also protects warmed, resumed and alternate sources.
func (s *Service) TitleReleaseRejected(ctx context.Context, key core.RatingKey, r core.Release) bool {
	if s.db != nil {
		s.loadRejectionsFor(ctx, key)
	}
	return s.rejectedFor(key, r)
}
func (s *Service) forgetRejection(ctx context.Context, id string) error {
	if s.db != nil {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM resolver_rejected WHERE release_id=?`, id); err != nil {
			return err
		}
	}
	s.mu.Lock()
	for k, rs := range s.rejections {
		var keep []core.Release
		for _, r := range rs {
			if r.ID != id {
				keep = append(keep, r)
			}
		}
		s.rejections[k] = keep
	}
	s.mu.Unlock()
	s.notify("rejections")
	return nil
}

// HardSubtitleBlocked is an explicit owner policy, separate from keywords.
func (s *Service) HardSubtitleBlocked(ctx context.Context, r core.Release) bool {
	return s.resolverCfg().HardSubtitles == "exclude" && s.hardSubbed(ctx, r)
}

func releaseBase(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, ext := range []string{".mkv", ".mp4", ".avi", ".nzb", ".torrent"} {
		name = strings.TrimSuffix(name, ext)
	}
	return name
}

func (s *Service) loadRejectionsFor(ctx context.Context, key core.RatingKey) {
	snapshot, err := s.readRejectionsFor(ctx, []core.RatingKey{key})
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.rejections == nil {
		s.rejections = map[core.RatingKey][]core.Release{}
	}
	s.rejections[key] = snapshot[key]
	s.mu.Unlock()
}

func (s *Service) readRejectionsFor(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey][]core.Release, error) {
	rejections := make(map[core.RatingKey][]core.Release)
	for chunk := range store.Chunks(keys, 500) {
		args := make([]any, len(chunk))
		for i, key := range chunk {
			args[i] = int64(key)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT rating_key,release_json FROM resolver_rejected WHERE rating_key IN (`+store.Placeholders(len(args))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key core.RatingKey
			var payload string
			if err := rows.Scan(&key, &payload); err != nil {
				rows.Close()
				return nil, err
			}
			var release core.Release
			if json.Unmarshal([]byte(payload), &release) == nil {
				rejections[key] = append(rejections[key], release)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return rejections, nil
}

// ClaimSubtitleCheck admits one content inspection attempt for an immutable
// release, shared across titles, viewers, nodes and process restarts. Failed or
// interrupted attempts stay unknown; they never become a false clean verdict.
func (s *Service) ClaimSubtitleCheck(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	if s.db != nil {
		res, err := s.db.ExecContext(ctx, `INSERT INTO resolver_subtitle_checks(release_id,updated_at) VALUES (?,?) ON CONFLICT(release_id) DO NOTHING`, id, s.now().Unix())
		if err != nil {
			return false, err
		}
		n, err := res.RowsAffected()
		return n == 1, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subtitleChecks == nil {
		s.subtitleChecks = map[string]bool{}
	}
	if s.subtitleChecks[id] {
		return false, nil
	}
	s.subtitleChecks[id] = true
	return true, nil
}
func init() {
	store.Register("resolver", 5, `CREATE TABLE resolver_subtitle_checks (release_id TEXT PRIMARY KEY);`)
	store.RegisterAfter("resolver_retention", "resolver")
	store.Register("resolver_retention", 1, `
ALTER TABLE resolver_hardsub ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE resolver_subtitle_checks ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX resolver_hardsub_updated ON resolver_hardsub(updated_at) WHERE updated_at>0;
CREATE INDEX resolver_subtitle_checks_updated ON resolver_subtitle_checks(updated_at) WHERE updated_at>0;
`)
}
