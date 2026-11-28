package experience

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

func init() {
	store.Register("experience_row_views", 1, `CREATE TABLE experience_row_views (id TEXT PRIMARY KEY, scope_hash TEXT NOT NULL UNIQUE, viewer_id INTEGER NOT NULL, device_id TEXT NOT NULL, protocol TEXT NOT NULL, snapshot TEXT NOT NULL, expires INTEGER NOT NULL, payload TEXT NOT NULL); CREATE INDEX experience_row_views_viewer ON experience_row_views(viewer_id,device_id);`)
	store.Register("experience_row_views", 2, `CREATE TABLE experience_row_view_manifests (scope_hash TEXT PRIMARY KEY, viewer_id INTEGER NOT NULL, device_id TEXT NOT NULL, protocol TEXT NOT NULL, generation TEXT NOT NULL, expires INTEGER NOT NULL, payload TEXT NOT NULL);`)
	store.Register("experience_row_views", 3, `CREATE UNIQUE INDEX experience_row_views_snapshot ON experience_row_views(snapshot);`)
	// Retention finds expired visits by expiry, across devices. Its own
	// component: experience_row_views' versions are pinned to the
	// delivered schema boundary.
	store.RegisterAfter("experience_row_views_retention", "experience_row_views")
	store.Register("experience_row_views_retention", 1, `CREATE INDEX experience_row_views_expires ON experience_row_views(expires); CREATE INDEX experience_row_view_manifests_expires ON experience_row_view_manifests(expires);`)
}

// rowViewPruneGrace is how long past expiry a row view is kept. An expired
// visit already answers stale; the grace keeps that answer (rather than
// not-found) for a client that returns within it.
const rowViewPruneGrace = 7 * 24 * time.Hour

// PruneRowViews deletes row views and manifests expired longer than
// rowViewPruneGrace, so rows of devices that never come back do not stay
// forever. Every surface's RowViewService shares these tables, so this
// runs once per database. Expiry is unix milliseconds.
func PruneRowViews(ctx context.Context, db *sql.DB) (int64, error) {
	if db == nil {
		return 0, nil
	}
	cutoff := time.Now().Add(-rowViewPruneGrace).UnixMilli()
	total, err := store.PruneBatched(ctx, db,
		`DELETE FROM experience_row_views WHERE id IN (SELECT id FROM experience_row_views WHERE expires<? ORDER BY expires LIMIT ?)`, cutoff)
	if err != nil {
		return total, err
	}
	n, err := store.PruneBatched(ctx, db,
		`DELETE FROM experience_row_view_manifests WHERE scope_hash IN (SELECT scope_hash FROM experience_row_view_manifests WHERE expires<? ORDER BY expires LIMIT ?)`, cutoff)
	return total + n, err
}

var ErrRowViewNotFound = fmt.Errorf("experience: row view unavailable: %w", frontend.ErrNotFound)
var ErrRowViewStale = frontend.ErrRowViewStale

// RowViewSnapshot is produced by the trusted row ranker after policy and
// layout. Entries are identities only; ResolveEntry must rehydrate them.
type RowViewSnapshot struct {
	Row       frontend.Row
	Situation core.HomeSituation
	Placement frontend.Placement
	ViewID    string
	Item      core.RatingKey
	Entries   []core.NeutralEntryRef
	Truncated bool
	Revision  string
}

type RowViewOptions struct {
	DB     *sql.DB
	TTL    time.Duration
	Now    func() time.Time
	Active func(context.Context, int64) (*core.User, error)
	Build  func(context.Context, frontend.RowViewRequest) ([]RowViewSnapshot, error)
	// ValidRow checks current placement, collection and viewer preference scope,
	// including the snapshot revision; it must fail closed on uncertainty.
	// PreparePolicy batches shared proofs for one policy pass only. It must
	// be called again after assembly and before issuing receipts.
	PreparePolicy func(context.Context, *core.User) (context.Context, error)
	// PrepareSnapshots optionally batches collection visibility and membership
	// proofs across all snapshots validated in one policy pass. Like
	// PreparePolicy, it is called fresh on every pass and never cached across passes.
	PrepareSnapshots func(context.Context, *core.User, []RowViewSnapshot) (context.Context, error)
	ValidRow         func(context.Context, *core.User, core.Device, core.Protocol, RowViewSnapshot) bool
	ResolveEntry     func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error)
	// ResolveEntries may batch rehydration; every call must perform fresh policy checks.
	ResolveEntries func(context.Context, *core.User, core.Device, core.Protocol, []core.NeutralEntryRef) ([]frontend.RowEntry, error)
	Receipts       *Receipts
}

func (s *RowViewService) resolveAll(ctx context.Context, u *core.User, device core.Device, protocol core.Protocol, refs []core.NeutralEntryRef) ([]frontend.RowEntry, error) {
	var entries []frontend.RowEntry
	if s.o.ResolveEntries != nil {
		var err error
		entries, err = s.o.ResolveEntries(ctx, u, device, protocol, refs)
		if err != nil || len(entries) != len(refs) {
			return nil, ErrRowViewStale
		}
	} else {
		entries = make([]frontend.RowEntry, 0, len(refs))
		for _, ref := range refs {
			entry, err := s.o.ResolveEntry(ctx, u, device, protocol, ref)
			if err != nil {
				return nil, ErrRowViewStale
			}
			entries = append(entries, entry)
		}
	}
	for i, ref := range refs {
		if string(entries[i].Kind) != ref.Kind || entryIdentity(entries[i]) != ref.ID {
			return nil, ErrRowViewStale
		}
		entries[i].Badge = ref.Badge
		entries[i].Watchlist = ref.Watchlist
	}
	return entries, nil
}

type RowViewService struct {
	o            RowViewOptions
	receiptMu    sync.Mutex
	pageReceipts map[[32]byte]rowPageReceipts
}
type rowPageReceipts struct {
	tokens  []string
	expires int64
	epoch   int64
}

func NewRowViewService(o RowViewOptions) (*RowViewService, error) {
	if o.DB == nil || o.TTL < time.Second || o.TTL > 24*time.Hour || o.Active == nil || o.Build == nil || o.ValidRow == nil || o.ResolveEntry == nil || o.Receipts == nil {
		return nil, errors.New("experience: incomplete row view dependencies")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &RowViewService{o: o}, nil
}

func rowViewToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func normalizeRowViewID(id string) string {
	if len(id) == 36 {
		if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
			return ""
		}
		id = strings.ReplaceAll(id, "-", "")
	}
	if len(id) != 32 {
		return ""
	}
	raw, err := hex.DecodeString(id)
	if err != nil || raw[6]>>4 != 4 || raw[8]>>6 != 2 {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:])
}

func (s *RowViewService) viewer(ctx context.Context, v *core.User, d core.Device, p core.Protocol) (*core.User, error) {
	if v == nil || v.ID <= 0 || d.ID == "" || len(d.ID) > 512 || p == "" || len(p) > 128 {
		return nil, ErrRowViewNotFound
	}
	u, err := bootstrapActive(ctx, s.o.Active, v.ID)
	if err != nil || u == nil || u.ID != v.ID {
		return nil, ErrRowViewNotFound
	}
	return u, nil
}

func (s *RowViewService) ListRowViews(ctx context.Context, req frontend.RowViewRequest) ([]frontend.RowView, error) {
	u, err := s.viewer(ctx, req.Viewer, req.Device, req.Protocol)
	if err != nil {
		return nil, err
	}
	req.Viewer = u
	scopeBytes, err := json.Marshal([]any{u.ID, req.Device.ID, req.Device.Language, req.Protocol, req.Placement, req.ViewID, req.Item})
	if err != nil {
		return nil, err
	}
	scopeHash := sha256.Sum256(scopeBytes)
	manifestScope := hex.EncodeToString(scopeHash[:])
	if !req.Refresh {
		views, found, err := s.loadManifest(ctx, req, manifestScope)
		if err != nil && !errors.Is(err, ErrRowViewStale) {
			return nil, err
		}
		if found {
			return views, err
		}
		// Listing starts a visit. A cached manifest can outlive its members
		// or policy revision; replace it instead of rejecting a fresh visit.
		req.Refresh = errors.Is(err, ErrRowViewStale)
	}
	rows, err := s.o.Build(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(rows) > 100 {
		return nil, errors.New("experience: too many row views")
	}
	policyCtx := ctx
	if s.o.PreparePolicy != nil {
		policyCtx, err = s.o.PreparePolicy(ctx, u)
		if err != nil {
			return nil, err
		}
	}
	// One broken row must never take down the whole Home or library page:
	// a row that fails validation or no longer resolves is dropped (and
	// logged), and repeated entries inside a row are collapsed.
	normalized := make([]RowViewSnapshot, 0, len(rows))
	seen := map[string]bool{}
	for _, snap := range rows {
		if snap.Row.ID == "" || snap.Row.Kind == "" || seen[snap.Row.ID] {
			slog.Warn("experience: dropping row view", "row", snap.Row.ID, "kind", snap.Row.Kind, "reason", "missing or repeated id")
			continue
		}
		seen[snap.Row.ID] = true
		if snap.Placement != req.Placement || snap.ViewID != req.ViewID || snap.Item != req.Item {
			slog.Warn("experience: dropping row view", "row", snap.Row.ID, "kind", snap.Row.Kind, "reason", "placement mismatch")
			continue
		}
		entries := make([]core.NeutralEntryRef, 0, len(snap.Entries))
		seenEntries := map[core.NeutralEntryRef]bool{}
		for _, ref := range snap.Entries {
			if ref.ID == "" || seenEntries[ref] {
				continue
			}
			seenEntries[ref] = true
			entries = append(entries, ref)
		}
		snap.Entries = entries[:min(len(entries), 1000)]
		if len(snap.Entries) == 0 && !IsSurfaceRow(snap.Row) {
			continue
		}
		normalized = append(normalized, snap)
	}
	if s.o.PrepareSnapshots != nil && len(normalized) > 0 {
		if prepared, err := s.o.PrepareSnapshots(policyCtx, u, normalized); err == nil {
			policyCtx = prepared
		} else if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// ValidRow checks individual collection membership when the batch
		// proof is unavailable.
	}
	valid := make([]RowViewSnapshot, 0, len(normalized))
	for _, snap := range normalized {
		if !s.o.ValidRow(policyCtx, u, req.Device, req.Protocol, snap) {
			slog.Warn("experience: dropping row view", "row", snap.Row.ID, "kind", snap.Row.Kind, "reason", "row no longer valid")
			continue
		}
		valid = append(valid, snap)
	}
	var allRefs []core.NeutralEntryRef
	for _, snap := range valid {
		allRefs = append(allRefs, snap.Entries...)
	}
	if len(allRefs) > 0 {
		if _, err = s.resolveAll(policyCtx, u, req.Device, req.Protocol, allRefs); err != nil {
			// Find the rows that no longer resolve and keep the rest.
			kept := valid[:0]
			for _, snap := range valid {
				if _, e := s.resolveAll(policyCtx, u, req.Device, req.Protocol, snap.Entries); e != nil {
					slog.Warn("experience: dropping row view", "row", snap.Row.ID, "kind", snap.Row.Kind, "reason", "entries no longer resolve")
					continue
				}
				kept = append(kept, snap)
			}
			valid = kept
		}
	}
	type candidate struct{ scope, payload string }
	candidates := make([]candidate, 0, len(valid))
	for _, snap := range valid {
		snap.Row.Entries, snap.Row.Items = nil, nil
		snap.Row.Handle, snap.Row.ImpressionID = "", ""
		payload, err := json.Marshal(snap)
		if err != nil || len(payload) > 65536 {
			slog.Warn("experience: dropping row view", "row", snap.Row.ID, "kind", snap.Row.Kind, "reason", "snapshot too large")
			continue
		}
		scope, err := json.Marshal([]any{u.ID, req.Device.ID, req.Device.Language, req.Protocol, req.Placement, req.ViewID, req.Item, snap.Row.ID})
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256(scope)
		scopeID := hex.EncodeToString(hash[:])
		candidates = append(candidates, candidate{scope: scopeID, payload: string(payload)})
	}
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = store.LockKey(ctx, store.DialectOf(s.o.DB), tx, fmt.Sprintf("row-view:%d:%s", u.ID, req.Device.ID)); err != nil {
		return nil, err
	}
	now := s.o.Now().UnixMilli()
	var expires int64
	var oldPayload string
	err = tx.QueryRowContext(ctx, `SELECT expires,payload FROM experience_row_view_manifests WHERE scope_hash=?`, manifestScope).Scan(&expires, &oldPayload)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	exists := err == nil
	if err == nil && expires > now && !req.Refresh {
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		views, found, err := s.loadManifest(ctx, req, manifestScope)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, ErrRowViewStale
		}
		return views, nil
	}
	var old []manifestRow
	if oldPayload != "" && expires > now {
		if err = json.Unmarshal([]byte(oldPayload), &old); err != nil {
			return nil, err
		}
		if len(old) > 100 {
			return nil, ErrRowViewStale
		}
	}
	expireReplaced := func(keep map[string]bool) error {
		args := []any{now, u.ID, req.Device.ID, string(req.Protocol), now}
		var conditions []string
		for _, ref := range old {
			if !keep[ref.ID] {
				conditions = append(conditions, "(id=? AND snapshot=?)")
				args = append(args, ref.ID, ref.Snapshot)
			}
		}
		if len(conditions) == 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE experience_row_views SET expires=? WHERE viewer_id=? AND device_id=? AND protocol=? AND expires>? AND (`+strings.Join(conditions, " OR ")+`)`, args...)
		return err
	}
	// An empty visit has no snapshot revision to revalidate. Build it
	// again next time, when the viewer may have started or finished a title.
	if len(candidates) == 0 {
		if err := expireReplaced(nil); err != nil {
			return nil, err
		}
		if exists && expires > now {
			if _, err := tx.ExecContext(ctx, `DELETE FROM experience_row_view_manifests WHERE scope_hash=?`, manifestScope); err != nil {
				return nil, err
			}
		}
		return []frontend.RowView{}, tx.Commit()
	}
	if !exists || expires <= now {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_row_view_manifests WHERE viewer_id=? AND device_id=? AND expires>?`, u.ID, req.Device.ID, now).Scan(&count); err != nil {
			return nil, err
		}
		if count >= 1000 {
			return nil, errors.New("experience: row view manifest limit reached")
		}
	}
	refs := make([]manifestRow, 0, len(candidates))
	if len(candidates) > 0 {
		args := make([]any, len(candidates))
		for n, c := range candidates {
			args[n] = c.scope
		}
		rows, e := tx.QueryContext(ctx, `SELECT scope_hash,id,expires FROM experience_row_views WHERE scope_hash IN (`+store.Placeholders(len(args))+`)`, args...)
		if e != nil {
			return nil, e
		}
		existing := map[string]string{}
		keep := map[string]bool{}
		live := 0
		for rows.Next() {
			var scope, id string
			var expires int64
			if e = rows.Scan(&scope, &id, &expires); e != nil {
				rows.Close()
				return nil, e
			}
			existing[scope] = id
			keep[id] = true
			if expires > now {
				live++
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		if err := expireReplaced(keep); err != nil {
			return nil, err
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_row_views WHERE viewer_id=? AND device_id=? AND expires>?`, u.ID, req.Device.ID, now).Scan(&count); err != nil {
			return nil, err
		}
		if count+len(candidates)-live > 1000 {
			return nil, errors.New("experience: row view limit reached")
		}
		args = nil
		values := make([]string, 0, len(candidates))
		for _, c := range candidates {
			id := existing[c.scope]
			if id == "" {
				id, err = rowViewToken()
				if err != nil {
					return nil, err
				}
			}
			generation, e := rowViewToken()
			if e != nil {
				return nil, e
			}
			values = append(values, "(?,?,?,?,?,?,?,?)")
			args = append(args, id, c.scope, u.ID, req.Device.ID, string(req.Protocol), generation, now+s.o.TTL.Milliseconds(), c.payload)
			refs = append(refs, manifestRow{ID: id, Snapshot: generation})
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO experience_row_views(id,scope_hash,viewer_id,device_id,protocol,snapshot,expires,payload) VALUES `+strings.Join(values, ",")+` ON CONFLICT(scope_hash) DO UPDATE SET snapshot=excluded.snapshot,expires=excluded.expires,payload=excluded.payload`, args...)
		if err != nil {
			return nil, err
		}
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return nil, err
	}
	generation, err := rowViewToken()
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO experience_row_view_manifests(scope_hash,viewer_id,device_id,protocol,generation,expires,payload) VALUES(?,?,?,?,?,?,?) ON CONFLICT(scope_hash) DO UPDATE SET generation=excluded.generation,expires=excluded.expires,payload=excluded.payload`, manifestScope, u.ID, req.Device.ID, string(req.Protocol), generation, now+s.o.TTL.Milliseconds(), string(b))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	views, found, err := s.loadManifest(ctx, req, manifestScope)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrRowViewStale
	}
	return views, nil
}

type manifestRow struct{ ID, Snapshot string }

// loadManifest returns the shared ordered visit, revalidating every row and
// entry. Once initial validation succeeds, found stays true on errors so a
// concurrent refresh or policy change cannot trigger a silent rebuild.
// A revoked member invalidates the visit instead of shifting offsets.
func (s *RowViewService) loadManifest(ctx context.Context, req frontend.RowViewRequest, scope string) ([]frontend.RowView, bool, error) {
	var generation, payload string
	var expires int64
	err := s.o.DB.QueryRowContext(ctx, `SELECT generation,expires,payload FROM experience_row_view_manifests WHERE scope_hash=? AND viewer_id=? AND device_id=? AND protocol=?`, scope, req.Viewer.ID, req.Device.ID, string(req.Protocol)).Scan(&generation, &expires, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if expires <= s.o.Now().UnixMilli() {
		return nil, false, nil
	}
	var refs []manifestRow
	if err = json.Unmarshal([]byte(payload), &refs); err != nil || len(refs) == 0 || len(refs) > 100 {
		return nil, false, ErrRowViewStale
	}
	out := make([]frontend.RowView, 0, len(refs))
	snapshots := make([]RowViewSnapshot, 0, len(refs))
	type storedRow struct {
		id, payload string
		expires     int64
	}
	stored := make(map[string]storedRow, len(refs))
	if len(refs) > 0 {
		args := []any{req.Viewer.ID, req.Device.ID, string(req.Protocol)}
		for _, ref := range refs {
			args = append(args, ref.Snapshot)
		}
		query := `SELECT id,snapshot,payload,expires FROM experience_row_views WHERE viewer_id=? AND device_id=? AND protocol=? AND snapshot IN (` + strings.TrimSuffix(strings.Repeat("?,", len(refs)), ",") + `)`
		rows, e := s.o.DB.QueryContext(ctx, query, args...)
		if e != nil {
			return nil, false, e
		}
		for rows.Next() {
			var snapshot string
			var row storedRow
			if e = rows.Scan(&row.id, &snapshot, &row.payload, &row.expires); e != nil {
				rows.Close()
				return nil, false, e
			}
			stored[snapshot] = row
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, false, e
		}
	}
	policyCtx := ctx
	if s.o.PreparePolicy != nil {
		policyCtx, err = s.o.PreparePolicy(ctx, req.Viewer)
		if err != nil {
			return nil, false, err
		}
	}
	var allRefs []core.NeutralEntryRef
	for _, ref := range refs {
		row, found := stored[ref.Snapshot]
		if !found || row.id != ref.ID || row.expires <= s.o.Now().UnixMilli() {
			return nil, false, ErrRowViewStale
		}
		var snap RowViewSnapshot
		if json.Unmarshal([]byte(row.payload), &snap) != nil || snap.Placement != req.Placement || snap.ViewID != req.ViewID || snap.Item != req.Item {
			return nil, false, ErrRowViewStale
		}
		allRefs = append(allRefs, snap.Entries...)
		out = append(out, frontend.RowView{ExpiresAt: time.UnixMilli(row.expires), ID: ref.Snapshot, Key: snap.Row.ID, Title: snap.Row.Title, Kind: snap.Row.Kind, Purpose: snap.Row.Purpose, Layout: snap.Row.Layout, Rank: snap.Row.Rank})
		snapshots = append(snapshots, snap)
	}
	if s.o.PrepareSnapshots != nil && len(snapshots) > 0 {
		policyCtx, err = s.o.PrepareSnapshots(policyCtx, req.Viewer, snapshots)
		if err != nil {
			return nil, false, err
		}
	}
	for _, snap := range snapshots {
		if !s.o.ValidRow(policyCtx, req.Viewer, req.Device, req.Protocol, snap) {
			return nil, false, ErrRowViewStale
		}
	}
	// Rehydrate the visit as bounded batches, sharing title/parent reads across
	// shelves rather than starting a new catalogue pass for every row.
	if len(allRefs) > 0 {
		if _, err := s.resolveAll(ctx, req.Viewer, req.Device, req.Protocol, allRefs); err != nil {
			return nil, false, ErrRowViewStale
		}
	}
	var current string
	var currentExpiry int64
	if err = s.o.DB.QueryRowContext(ctx, `SELECT generation,expires FROM experience_row_view_manifests WHERE scope_hash=? AND viewer_id=? AND device_id=? AND protocol=?`, scope, req.Viewer.ID, req.Device.ID, string(req.Protocol)).Scan(&current, &currentExpiry); err != nil || current != generation || currentExpiry <= s.o.Now().UnixMilli() {
		return nil, true, ErrRowViewStale
	}
	active, err := s.viewer(ctx, req.Viewer, req.Device, req.Protocol)
	if err != nil {
		return nil, true, ErrRowViewNotFound
	}
	policyCtx = ctx
	if s.o.PreparePolicy != nil {
		policyCtx, err = s.o.PreparePolicy(ctx, active)
		if err != nil {
			return nil, true, err
		}
	}
	if s.o.PrepareSnapshots != nil && len(snapshots) > 0 {
		policyCtx, err = s.o.PrepareSnapshots(policyCtx, active, snapshots)
		if err != nil {
			return nil, true, err
		}
	}
	for _, snap := range snapshots {
		if !s.o.ValidRow(policyCtx, active, req.Device, req.Protocol, snap) {
			return nil, true, ErrRowViewStale
		}
	}
	if len(allRefs) > 0 {
		if _, err := s.resolveAll(ctx, active, req.Device, req.Protocol, allRefs); err != nil {
			return nil, true, ErrRowViewStale
		}
	}
	// A refresh can commit while the final policy pass is running.
	if err = s.o.DB.QueryRowContext(ctx, `SELECT generation,expires FROM experience_row_view_manifests WHERE scope_hash=? AND viewer_id=? AND device_id=? AND protocol=?`, scope, req.Viewer.ID, req.Device.ID, string(req.Protocol)).Scan(&current, &currentExpiry); err != nil || current != generation || currentExpiry <= s.o.Now().UnixMilli() {
		return nil, true, ErrRowViewStale
	}
	return out, true, nil
}

type rowViewPolicyPreflightKey struct{}
type rowViewPolicyPreflight struct{ titles []core.Title }

func (s *RowViewService) ResolveRowView(ctx context.Context, req frontend.RowViewPageRequest) (page frontend.RowViewPage, err error) {
	// A policy change while the page is read makes the snapshot stale: the
	// frontend rebuilds the visit once instead of dropping rows.
	defer func() {
		if errors.Is(err, viewerpolicy.ErrChanged) {
			err = ErrRowViewStale
		}
	}()
	req.ID = normalizeRowViewID(req.ID)
	u, err := s.viewer(ctx, req.Viewer, req.Device, req.Protocol)
	if err != nil {
		return page, err
	}
	if req.ID == "" || req.Offset < 0 || req.Limit <= 0 || req.Limit > 100 {
		return page, ErrRowViewNotFound
	}
	var generation, payload string
	var expires int64
	err = s.o.DB.QueryRowContext(ctx, `SELECT snapshot,expires,payload FROM experience_row_views WHERE snapshot=? AND viewer_id=? AND device_id=? AND protocol=?`, req.ID, u.ID, req.Device.ID, string(req.Protocol)).Scan(&generation, &expires, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return page, ErrRowViewNotFound
	}
	if err != nil {
		return page, err
	}
	if generation != req.ID || expires <= s.o.Now().UnixMilli() {
		return page, ErrRowViewStale
	}
	var snap RowViewSnapshot
	if err = json.Unmarshal([]byte(payload), &snap); err != nil {
		return page, err
	}
	if !s.o.ValidRow(ctx, u, req.Device, req.Protocol, snap) {
		return page, ErrRowViewStale
	}
	page.ExpiresAt = time.UnixMilli(expires)
	page.RankedTop10 = snap.Row.RankedTop10 || snap.Row.Layout.RankedTop10
	allowedKinds := map[frontend.RowEntryKind]bool{}
	for _, kind := range req.Kinds {
		allowedKinds[kind] = true
	}
	resolved, err := s.resolveAll(ctx, u, req.Device, req.Protocol, snap.Entries)
	if err != nil {
		return page, ErrRowViewStale
	}
	var entries []frontend.RowEntry
	for _, entry := range resolved {
		if len(allowedKinds) == 0 || allowedKinds[entry.Kind] {
			entry.Handle = ""
			entries = append(entries, entry)
		}
	}
	page.Total, page.TotalExact, page.Truncated = len(entries), !snap.Truncated, snap.Truncated
	start := min(req.Offset, len(entries))
	end := min(start+req.Limit, len(entries))
	page.More = end < len(entries)
	page.Entries = entries[start:end]
	current := func() bool {
		policyCtx := context.WithValue(ctx, rowViewPolicyPreflightKey{}, &rowViewPolicyPreflight{})
		var generation string
		var expiry int64
		err := s.o.DB.QueryRowContext(ctx, `SELECT snapshot,expires FROM experience_row_views WHERE snapshot=? AND viewer_id=? AND device_id=? AND protocol=?`, req.ID, u.ID, req.Device.ID, string(req.Protocol)).Scan(&generation, &expiry)
		active, activeErr := s.viewer(ctx, u, req.Device, req.Protocol)
		if err != nil || activeErr != nil || generation != req.ID || expiry <= s.o.Now().UnixMilli() || !s.o.ValidRow(policyCtx, active, req.Device, req.Protocol, snap) {
			return false
		}
		_, err = s.resolveAll(policyCtx, active, req.Device, req.Protocol, snap.Entries)
		if err != nil {
			return false
		}
		err = s.o.DB.QueryRowContext(ctx, `SELECT snapshot,expires FROM experience_row_views WHERE snapshot=? AND viewer_id=? AND device_id=? AND protocol=?`, req.ID, u.ID, req.Device.ID, string(req.Protocol)).Scan(&generation, &expiry)
		return err == nil && generation == req.ID && expiry > s.o.Now().UnixMilli()
	}
	if len(page.Entries) == 0 {
		if !current() {
			return frontend.RowViewPage{}, ErrRowViewStale
		}
		return page, nil
	}
	imp := &core.NeutralImpression{RowID: snap.Row.ID, Kind: snap.Row.Kind, Title: snap.Row.Title, Purpose: snap.Row.Purpose, Rank: snap.Row.Rank, Situation: snap.Situation, Collection: snap.Row.Collection, Placement: string(snap.Placement), ViewID: snap.ViewID, Item: snap.Item}
	for _, e := range page.Entries {
		imp.Entries = append(imp.Entries, core.NeutralEntryRef{Kind: string(e.Kind), ID: entryIdentity(e)})
		if e.Title != nil {
			imp.Items = append(imp.Items, e.Title.Key)
		}
	}
	subjects := make([]ReceiptSubject, 0, len(page.Entries)+1)
	subjects = append(subjects, ReceiptSubject{Kind: "row", ID: snap.Row.ID, Purpose: snap.Row.Purpose, Rank: snap.Row.Rank, Impression: imp})
	for i := range page.Entries {
		e := &page.Entries[i]
		subjects = append(subjects, ReceiptSubject{Kind: string(e.Kind), ID: fmt.Sprintf("%s/%s/%s", snap.Row.ID, e.Kind, entryIdentity(*e)), Purpose: snap.Row.Purpose, Rank: snap.Row.Rank, Impression: imp})
	}
	// Vela revisits immutable pages while idle. Reuse their already-issued
	// receipts after the same fresh policy checks, instead of consuming the
	// device's quota again on every bootstrap. Never cache a policy failure.
	var receiptKey [32]byte
	var receiptEpoch int64
	var tokens []string
	receiptExpiry := time.Unix(s.o.Receipts.opts.Now().Add(s.o.Receipts.opts.TTL).Unix(), 0)
	reuse := req.Protocol == "vela" && s.o.Receipts.opts.Generation == nil
	if reuse {
		b, e := json.Marshal([]any{u, req.Device.ID, req.Protocol, req.ID, req.Offset, req.Limit, req.Kinds, subjects})
		if e != nil {
			return frontend.RowViewPage{}, e
		}
		receiptKey = sha256.Sum256(b)
		receiptEpoch, err = viewerpolicy.Read(ctx, s.o.DB)
		if err != nil {
			return frontend.RowViewPage{}, err
		}
		s.receiptMu.Lock()
		cached, found := s.pageReceipts[receiptKey]
		s.receiptMu.Unlock()
		if found && cached.expires > s.o.Receipts.opts.Now().Unix() && cached.epoch == receiptEpoch {
			if !current() {
				return frontend.RowViewPage{}, ErrRowViewStale
			}
			after, e := viewerpolicy.Read(ctx, s.o.DB)
			if e != nil {
				return frontend.RowViewPage{}, e
			}
			if after != receiptEpoch {
				return frontend.RowViewPage{}, ErrRowViewStale
			}
			live, e := s.o.Receipts.receiptsLive(ctx, u.ID, req.Device.ID, req.Protocol, cached.tokens)
			if e != nil {
				return frontend.RowViewPage{}, e
			}
			if live {
				tokens = cached.tokens
				receiptExpiry = time.Unix(cached.expires, 0)
			}
		}
	}
	if tokens == nil {
		tokens, err = s.o.Receipts.issueBatchChecked(ctx, u, req.Device, req.Protocol, subjects, current, func(tx *sql.Tx) error {
			var expiry int64
			query := `SELECT expires FROM experience_row_views WHERE snapshot=? AND viewer_id=? AND device_id=? AND protocol=?`
			if store.DialectOf(s.o.DB) == store.Postgres {
				query += ` FOR SHARE`
			}
			if err := tx.QueryRowContext(ctx, query, req.ID, u.ID, req.Device.ID, string(req.Protocol)).Scan(&expiry); err != nil || expiry <= s.o.Now().UnixMilli() {
				return ErrRowViewStale
			}
			return nil
		})
		if err == nil && reuse {
			s.receiptMu.Lock()
			if s.pageReceipts == nil {
				s.pageReceipts = make(map[[32]byte]rowPageReceipts)
			}
			now := s.o.Receipts.opts.Now()
			for key, cached := range s.pageReceipts {
				if cached.expires <= now.Unix() {
					delete(s.pageReceipts, key)
				}
			}
			if _, exists := s.pageReceipts[receiptKey]; exists || len(s.pageReceipts) < 512 {
				s.pageReceipts[receiptKey] = rowPageReceipts{tokens: tokens, expires: min(expires/1000, receiptExpiry.Unix()), epoch: receiptEpoch}
			}
			s.receiptMu.Unlock()
		}
	}
	if errors.Is(err, viewerpolicy.ErrChanged) {
		return frontend.RowViewPage{}, ErrRowViewStale
	}
	if err != nil {
		return frontend.RowViewPage{}, err
	}
	if receiptExpiry.Before(page.ExpiresAt) {
		page.ExpiresAt = receiptExpiry
	}
	page.ImpressionHandle = tokens[0]
	for i := range page.Entries {
		page.Entries[i].Handle = tokens[i+1]
	}
	return page, nil
}

var _ frontend.RowViews = (*RowViewService)(nil)
