package experience

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"database/sql"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

// IssueBatch atomically issues receipts and shared admission in input order.
func (s *Receipts) IssueBatch(ctx context.Context, viewer *core.User, device core.Device, protocol core.Protocol, subjects []ReceiptSubject) ([]string, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	return s.issueBatchChecked(ctx, viewer, device, protocol, subjects, nil, nil)
}

func (s *Receipts) issueBatchChecked(ctx context.Context, viewer *core.User, device core.Device, protocol core.Protocol, subjects []ReceiptSubject, current func() bool, fence func(*sql.Tx) error) ([]string, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	for attempt := 0; ; attempt++ {
		tokens, err := s.issueBatchAttempt(ctx, viewer, device, protocol, subjects, current, fence)
		// A bootstrap proof is fixed to the visit. The outer assembly must
		// restart to observe the new epoch.
		if !errors.Is(err, viewerpolicy.ErrChanged) || attempt == 1 || ctx.Err() != nil ||
			(viewer != nil && bootstrapProofFor(ctx, viewer.ID) != nil) {
			return tokens, err
		}
	}
}

func (s *Receipts) issueBatchAttempt(ctx context.Context, viewer *core.User, device core.Device, protocol core.Protocol, subjects []ReceiptSubject, current func() bool, fence func(*sql.Tx) error) ([]string, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if s.opts.PolicyLease != nil {
		release, err := s.opts.PolicyLease(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	epoch, err := viewerpolicy.Read(ctx, s.opts.DB)
	if err != nil {
		return nil, err
	}
	if viewer == nil || viewer.ID <= 0 || device.ID == "" || protocol == "" || len(subjects) == 0 || len(subjects) > s.opts.MaxPerDevice {
		return nil, ErrReceipt
	}
	// An unrestricted viewer's Home revision carries no policy epoch (0):
	// policy changes don't reach it, so it has nothing to fence.
	if proof := bootstrapProofFor(ctx, viewer.ID); proof != nil && proof.epoch != 0 && proof.epoch != epoch {
		return nil, viewerpolicy.ErrChanged
	}
	snapshots := make([]string, len(subjects))
	encoded := make(map[*core.NeutralImpression]string)
	for i, subject := range subjects {
		if subject.Kind == "" || subject.ID == "" {
			return nil, ErrReceipt
		}
		if imp := subject.Impression; imp != nil {
			if !snapshotMatches(subject, imp) || len(imp.Items) > 1000 || len(imp.Entries) > 1000 || len(imp.Placement) > 128 || len(imp.ViewID) > 512 || len(imp.RowID) > 512 || len(imp.Kind) > 512 || len(imp.Title) > 2048 {
				return nil, ErrReceipt
			}
			value, ok := encoded[imp]
			if !ok {
				b, err := json.Marshal(imp)
				if err != nil || len(b) > 32768 {
					return nil, ErrReceipt
				}
				value = string(b)
				encoded[imp] = value
			}
			snapshots[i] = value
		}
	}
	active, err := bootstrapActive(ctx, s.opts.Active, viewer.ID)
	if err != nil || active == nil || active.ID != viewer.ID {
		return nil, ErrReceipt
	}
	if s.opts.Prefetch != nil {
		// The titles the subjects and their rows' entries name, plus parent
		// shows for episodes and seasons so inherited visibility never falls
		// back to per-show single-key reads.
		var keys []core.RatingKey
		seen := map[core.RatingKey]bool{}
		addKey := func(key core.RatingKey) {
			if key == 0 || seen[key] {
				return
			}
			seen[key] = true
			keys = append(keys, key)
			if key.Kind() == core.KindEpisode || key.Kind() == core.KindSeason {
				show := key.Parent()
				if key.Kind() == core.KindEpisode {
					show = show.Parent()
				}
				if show != 0 && !seen[show] {
					seen[show] = true
					keys = append(keys, show)
				}
			}
		}
		add := func(kind, id string) {
			if kind != string(frontend.RowEntryTitle) {
				return
			}
			if key, err := core.ParseRatingKey(id); err == nil {
				addKey(key)
			}
		}
		for _, subject := range subjects {
			add(subject.Kind, subject.ID)
			if imp := subject.Impression; imp != nil {
				for _, e := range imp.Entries {
					add(string(e.Kind), e.ID)
				}
			}
		}
		if len(keys) > 1 {
			s.opts.Prefetch(ctx, keys)
		}
	}
	ctx = context.WithValue(ctx, rowInteractionBatchKey{}, make(map[*core.NeutralImpression]bool))
	if s.opts.PrepareBatch != nil {
		ctx = s.opts.PrepareBatch(ctx, active, subjects)
	}
	for _, subject := range subjects {
		if !s.opts.Allowed(ctx, active, device, protocol, subject) {
			return nil, ErrReceipt
		}
	}
	var generation int64
	if s.opts.Generation != nil {
		generation, err = s.opts.Generation(ctx, active, device, protocol)
		if err != nil || generation < 0 {
			return nil, ErrReceipt
		}
	}
	if current != nil && !current() {
		return nil, ErrRowViewStale
	}
	// Ranking a full device's authority sorts up to MaxPerDevice rows; do it
	// as a plain read so the write transaction only deletes by digest.
	plan, err := s.planEviction(ctx, viewer.ID, device.ID, protocol, generation, len(subjects))
	if err != nil {
		return nil, err
	}
	tx, err := s.opts.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = capability.LockHeaderScopeInTx(ctx, s.opts.DB, tx, capability.HeaderScope{Namespace: ReceiptCapabilityNamespace, ViewerID: viewer.ID, Surface: string(protocol), DeviceID: device.ID}); err != nil {
		return nil, err
	}
	if err = store.LockKey(ctx, store.DialectOf(s.opts.DB), tx, fmt.Sprintf("experience:receipts:%d:%s", viewer.ID, device.ID)); err != nil {
		return nil, err
	}
	// PostgreSQL must lock the shared epoch before admitting authority. On
	// SQLite the first INSERT obtains the single writer; a concurrent policy
	// commit after this transaction's read makes that upgrade fail, rather
	// than allowing stale authority. Rechecking below needs no no-op UPDATE.
	checkEpoch := func() error {
		var actual int64
		if e := tx.QueryRowContext(ctx, `SELECT generation FROM viewer_policy_epoch WHERE id=1`).Scan(&actual); e != nil {
			return e
		}
		if actual != epoch {
			return viewerpolicy.ErrChanged
		}
		return nil
	}
	if store.DialectOf(s.opts.DB) == store.Postgres {
		err = viewerpolicy.Fence(ctx, s.opts.DB, tx, epoch)
	} else {
		err = checkEpoch()
	}
	if err != nil {
		return nil, err
	}
	now := s.opts.Now().Unix()
	if err = s.sweepExpired(ctx, tx, viewer.ID, device.ID, now); err != nil {
		return nil, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts WHERE viewer_id=? AND device_id=? AND expires>?`, viewer.ID, device.ID, now).Scan(&count); err != nil {
		return nil, err
	}
	if count+len(subjects) > s.opts.MaxPerDevice {
		// Retire only this device's oldest dispensable authority, as planned
		// before the transaction. The delete triggers retire headers and
		// claims in the same transaction.
		need := int64(count + len(subjects) - s.opts.MaxPerDevice)
		removed, evictErr := s.evictPlanned(ctx, tx, viewer.ID, device.ID, now, plan, need)
		if evictErr != nil {
			return nil, evictErr
		}
		// A planned row that was claimed, or retired by another issue, since
		// the read leaves a shortfall; rank in the transaction only then.
		if removed < need {
			more, evictErr := s.evictRanked(ctx, tx, viewer.ID, device.ID, now, protocol, generation, need-removed)
			if evictErr != nil {
				return nil, evictErr
			}
			removed += more
		}
		// Recount live authority after the bounded eviction.
		if removed > 0 {
			if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts WHERE viewer_id=? AND device_id=? AND expires>?`, viewer.ID, device.ID, now).Scan(&count); err != nil {
				return nil, err
			}
		}
		if count+len(subjects) > s.opts.MaxPerDevice {
			var earliest int64
			if err = tx.QueryRowContext(ctx, `SELECT MIN(expires) FROM experience_receipts WHERE viewer_id=? AND device_id=? AND expires>?`, viewer.ID, device.ID, now).Scan(&earliest); err != nil {
				return nil, err
			}
			return nil, &frontend.ReceiptCapacityError{RetryAfter: time.Duration(max(1, earliest-now)) * time.Second}
		}
	}
	tokens := make([]string, len(subjects))
	hashes := make([]string, len(subjects))
	expires := s.opts.Now().Add(s.opts.TTL).Unix()
	millis, e := receiptExpiresMillis(expires)
	if e != nil {
		return nil, e
	}
	for i := range subjects {
		tokens[i], hashes[i], err = s.codec.Issue()
		if err != nil {
			return nil, err
		}
	}

	headers := make([]capability.Header, 0, len(hashes))
	for _, hash := range hashes {
		headers = append(headers, capability.Header{HeaderScope: capability.HeaderScope{Namespace: ReceiptCapabilityNamespace, ViewerID: viewer.ID, Surface: string(protocol), DeviceID: device.ID}, Digest: hash, ExpiresMillis: millis})
	}
	if err = s.bindHeaders(ctx, tx, headers); err != nil {
		return nil, err
	}
	rows := make([][]any, 0, len(subjects))
	for i, subject := range subjects {
		rows = append(rows, []any{hashes[i], viewer.ID, device.ID, string(protocol), expires, subject.Kind, subject.ID, string(subject.Purpose), subject.Rank, snapshots[i], generation})
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	fields := []string{"TEXT", "BIGINT", "TEXT", "TEXT", "BIGINT", "TEXT", "TEXT", "TEXT", "BIGINT", "TEXT", "BIGINT"}
	cols := make([]string, len(fields))
	for i, typ := range fields {
		if store.DialectOf(s.opts.DB) == store.Postgres {
			cols[i] = fmt.Sprintf("CAST(value->>%d AS %s)", i, typ)
		} else {
			cols[i] = fmt.Sprintf("CAST(json_extract(value,'$[%d]') AS %s)", i, typ)
		}
	}
	source := "json_each(?)"
	if store.DialectOf(s.opts.DB) == store.Postgres {
		source = "json_array_elements(CAST(? AS JSON))"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO experience_receipts(hash,viewer_id,device_id,protocol,expires,kind,subject,purpose,rank,impression,generation) SELECT `+strings.Join(cols, ",")+` FROM `+source, string(b)); err != nil {
		return nil, err
	}
	if err = checkEpoch(); err != nil {
		return nil, err
	}
	if fence != nil {
		if err = fence(tx); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return tokens, nil
}

// evictionPlan is a device's oldest dispensable live authority, read outside
// the write transaction. superseded holds the digests whose reason cannot
// lapse before the transaction: a newer duplicate (eviction retires oldest
// first, so it outlives the older row) or another generation. The rest were
// unclaimed and are rechecked when deleted.
type evictionPlan struct {
	superseded, unclaimed []string
}

// The window ranks only one quota-bounded device, never the library; older
// duplicates are superseded even if an impression was reported.
const receiptEvictionRank = `SELECT r.hash, CASE WHEN r.recency>1 OR (?=1 AND r.protocol=? AND r.generation<>?) THEN 1 ELSE 0 END FROM (
 SELECT hash,expires,protocol,generation,
 ROW_NUMBER() OVER (PARTITION BY protocol,kind,subject,purpose,rank,generation ORDER BY expires DESC,hash DESC) AS recency
 FROM experience_receipts WHERE viewer_id=? AND device_id=? AND expires>?
 ) r WHERE r.recency>1 OR (?=1 AND r.protocol=? AND r.generation<>?) OR NOT EXISTS (
 SELECT 1 FROM capability_claims c WHERE c.namespace=? AND c.digest=r.hash)
 ORDER BY r.expires,r.hash LIMIT ?`

func (s *Receipts) evictionRankArgs(viewer int64, device string, now int64, protocol core.Protocol, generation, limit int64) []any {
	gen := store.BoolInt(s.opts.Generation != nil)
	return []any{gen, string(protocol), generation, viewer, device, now, gen, string(protocol), generation, ReceiptCapabilityNamespace, limit}
}

func (s *Receipts) planEviction(ctx context.Context, viewer int64, device string, protocol core.Protocol, generation int64, incoming int) (evictionPlan, error) {
	var plan evictionPlan
	now := s.opts.Now().Unix()
	var count int
	if err := s.opts.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts WHERE viewer_id=? AND device_id=? AND expires>?`, viewer, device, now).Scan(&count); err != nil {
		return plan, err
	}
	need := count + incoming - s.opts.MaxPerDevice
	if need <= 0 {
		return plan, nil
	}
	rows, err := s.opts.DB.QueryContext(ctx, receiptEvictionRank, s.evictionRankArgs(viewer, device, now, protocol, generation, int64(need))...)
	if err != nil {
		return plan, err
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		var superseded int
		if err = rows.Scan(&hash, &superseded); err != nil {
			return plan, err
		}
		if superseded == 1 {
			plan.superseded = append(plan.superseded, hash)
		} else {
			plan.unclaimed = append(plan.unclaimed, hash)
		}
	}
	return plan, rows.Err()
}

// evictPlanned retires up to need planned digests that are still this
// device's live authority, oldest first, by primary key. Unclaimed ones are
// skipped if an impression was reported since the plan. The delete triggers
// retire headers and claims in the same transaction.
func (s *Receipts) evictPlanned(ctx context.Context, tx *sql.Tx, viewer int64, device string, now int64, plan evictionPlan, need int64) (int64, error) {
	if len(plan.superseded)+len(plan.unclaimed) == 0 || need <= 0 {
		return 0, nil
	}
	all, err := json.Marshal(append(append([]string{}, plan.superseded...), plan.unclaimed...))
	if err != nil {
		return 0, err
	}
	superseded, err := json.Marshal(append([]string{}, plan.superseded...))
	if err != nil {
		return 0, err
	}
	source := "SELECT value FROM json_each(?)"
	if store.DialectOf(s.opts.DB) == store.Postgres {
		source = "SELECT value FROM json_array_elements_text(CAST(? AS JSON))"
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM experience_receipts WHERE hash IN (
 SELECT r.hash FROM (`+source+`) planned CROSS JOIN experience_receipts r WHERE r.hash=planned.value AND r.viewer_id=? AND r.device_id=? AND r.expires>? AND (r.hash IN (`+source+`) OR NOT EXISTS (
 SELECT 1 FROM capability_claims c WHERE c.namespace=? AND c.digest=r.hash))
 ORDER BY r.expires,r.hash LIMIT ?)`, string(all), viewer, device, now, string(superseded), ReceiptCapabilityNamespace, need)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// evictRanked ranks and retires in the transaction; the fallback when the
// plan came up short.
func (s *Receipts) evictRanked(ctx context.Context, tx *sql.Tx, viewer int64, device string, now int64, protocol core.Protocol, generation, need int64) (int64, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM experience_receipts WHERE viewer_id=? AND device_id=? AND hash IN (SELECT hash FROM (`+receiptEvictionRank+`) ranked)`, append([]any{viewer, device}, s.evictionRankArgs(viewer, device, now, protocol, generation, need)...)...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// receiptsLive checks only the fixed page's digests, so cache reuse cannot
// publish tokens retired by quota recovery on this or another node.
func (s *Receipts) receiptsLive(ctx context.Context, viewer int64, device string, protocol core.Protocol, tokens []string) (bool, error) {
	if len(tokens) == 0 {
		return true, nil
	}
	if len(tokens) > s.opts.MaxPerDevice {
		return false, nil
	}
	hashes := make([]string, len(tokens))
	for n, token := range tokens {
		hash, ok := s.codec.Hash(token)
		if !ok {
			return false, nil
		}
		hashes[n] = hash
	}
	b, err := json.Marshal(hashes)
	if err != nil {
		return false, err
	}
	source := "SELECT value FROM json_each(?)"
	if store.DialectOf(s.opts.DB) == store.Postgres {
		source = "SELECT value FROM json_array_elements_text(CAST(? AS JSON))"
	}
	var count int
	err = s.opts.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts WHERE viewer_id=? AND device_id=? AND protocol=? AND expires>? AND hash IN (`+source+`)`, viewer, device, string(protocol), s.opts.Now().Unix(), string(b)).Scan(&count)
	return count == len(tokens), err
}
