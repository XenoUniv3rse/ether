package pluginitems

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/store"
)

const (
	DefaultWalkPageLimit = 200
	DefaultChangesLimit  = 500
	MaxUpsertBatch       = plugins.MaxKindUpsertItems
)

// SyncOptions controls a single SyncKind invocation.
type SyncOptions struct {
	Manual      bool   // owner-started sync from the console
	Force       bool   // bypass the 50%-delete guard (admin override)
	FullWalk    bool   // force a full browse walk even if a change cursor exists
	Concurrency int    // max concurrent child browse requests (default MaxSyncConcurrency = 4)
	Caller      Caller // optional override for Service's configured Caller
	rebuildOnly bool   // A pushed batch arrived while the kind has a pending rebuild.
}

// SyncResult reports the outcome of a SyncKind run.
type SyncResult struct {
	Upserted   int
	Deleted    int
	Skipped    int
	Protected  int
	NextCursor string
	RetryAfter time.Duration
}

// SyncRunResult is delivered when an asynchronous manual sync completes.
type SyncRunResult struct {
	Result SyncResult
	Err    error
}

type syncWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type syncProgress struct {
	running     bool
	phase       string
	syncedItems int
	pages       int
	itemsTotal  int
}

func (s *Service) setSyncProgress(pluginID, kindKey string, p syncProgress) {
	if s == nil {
		return
	}
	key := workerKey(pluginID, kindKey)
	s.workersMu.Lock()
	if s.syncProgress == nil {
		s.syncProgress = map[string]syncProgress{}
	}
	s.syncProgress[key] = p
	s.workersMu.Unlock()
}

func (s *Service) clearSyncProgressIfPhase(pluginID, kindKey, phase string) {
	if s == nil {
		return
	}
	key := workerKey(pluginID, kindKey)
	s.workersMu.Lock()
	if cur, ok := s.syncProgress[key]; ok && cur.phase == phase {
		delete(s.syncProgress, key)
	}
	s.workersMu.Unlock()
}

func (s *Service) getSyncProgress(pluginID, kindKey string) (syncProgress, bool) {
	if s == nil {
		return syncProgress{}, false
	}
	key := workerKey(pluginID, kindKey)
	s.workersMu.Lock()
	p, ok := s.syncProgress[key]
	s.workersMu.Unlock()
	return p, ok
}

func (s *Service) setSyncOutcome(pluginID, kindKey string, out SyncOutcome) {
	if s == nil {
		return
	}
	key := workerKey(pluginID, kindKey)
	s.workersMu.Lock()
	if s.lastOutcome == nil {
		s.lastOutcome = map[string]SyncOutcome{}
	}
	s.lastOutcome[key] = out
	s.workersMu.Unlock()
}

func (s *Service) getSyncOutcome(pluginID, kindKey string) (SyncOutcome, bool) {
	if s == nil {
		return SyncOutcome{}, false
	}
	key := workerKey(pluginID, kindKey)
	s.workersMu.Lock()
	out, ok := s.lastOutcome[key]
	s.workersMu.Unlock()
	return out, ok
}

// IsSyncRunning reports whether a sync or reindex pass is active for (pluginID, kindKey).
func (s *Service) IsSyncRunning(pluginID, kindKey string) bool {
	if p, ok := s.getSyncProgress(pluginID, kindKey); ok && p.running {
		return true
	}
	return false
}

// StartManualSync launches an owner-started SyncKind on the service's
// long-lived background context (never tied to an HTTP request) so switching
// console tabs or proxies timing out never cancel the walk. If a sync is
// already running for (pluginID, kindKey), alreadyRunning is true.
func (s *Service) StartManualSync(pluginID, kindKey string, opts SyncOptions) (<-chan SyncRunResult, bool) {
	opts.Manual = true
	key := workerKey(pluginID, kindKey)

	s.workersMu.Lock()
	if cur, ok := s.syncProgress[key]; ok && cur.running {
		s.workersMu.Unlock()
		return nil, true
	}
	if mw, ok := s.manualSyncs[key]; ok && mw != nil {
		s.workersMu.Unlock()
		return nil, true
	}
	if s.syncProgress == nil {
		s.syncProgress = map[string]syncProgress{}
	}
	if s.manualSyncs == nil {
		s.manualSyncs = map[string]*syncWorker{}
	}
	s.syncProgress[key] = syncProgress{running: true, phase: "syncing"}
	baseCtx := s.bgCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithCancel(baseCtx)
	workerDone := make(chan struct{})
	s.manualSyncs[key] = &syncWorker{cancel: cancel, done: workerDone}
	s.workersMu.Unlock()

	resCh := make(chan SyncRunResult, 1)
	go func() {
		defer close(workerDone)
		defer s.clearSyncProgressIfPhase(pluginID, kindKey, "syncing")
		defer func() {
			s.workersMu.Lock()
			if cur, ok := s.manualSyncs[key]; ok && cur.done == workerDone {
				delete(s.manualSyncs, key)
			}
			s.workersMu.Unlock()
			cancel()
		}()
		res, err := s.SyncKind(ctx, pluginID, kindKey, opts)
		resCh <- SyncRunResult{Result: res, Err: err}
		close(resCh)
	}()
	return resCh, false
}

// KeepExistingItems acknowledges a paused 50%-delete guard without removing any
// stored items: it clears the persisted guard/error state and resets
// missed_walks so existing items remain intact.
func (s *Service) KeepExistingItems(ctx context.Context, pluginID, kindKey string) (SyncState, error) {
	if s == nil || s.db == nil {
		return SyncState{}, fmt.Errorf("pluginitems: nil service")
	}
	syncMu := s.syncLock(pluginID, kindKey)
	syncMu.Lock()
	defer syncMu.Unlock()

	if _, err := s.GetKind(ctx, pluginID, kindKey); err != nil {
		return SyncState{}, err
	}
	for cursor := ""; ; {
		ids, err := s.missedWalkIDs(ctx, pluginID, kindKey, cursor)
		if err != nil {
			return SyncState{}, err
		}
		if len(ids) == 0 {
			break
		}
		if err := s.writeMissedWalks(ctx, pluginID, kindKey, ids,
			`UPDATE plugin_items SET missed_walks = 0 WHERE missed_walks <> 0`); err != nil {
			return SyncState{}, err
		}
		cursor = ids[len(ids)-1]
		if err := s.betweenWriteChunks(ctx); err != nil {
			return SyncState{}, err
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE plugin_sync_state SET last_error = '', failure_json = '', guard_json = '' WHERE plugin_id = ? AND kind_key = ?`,
		pluginID, kindKey); err != nil {
		return SyncState{}, err
	}
	total, _ := s.countKindItems(ctx, pluginID, kindKey)
	outcome := SyncOutcome{
		Mode:       "keep",
		Total:      total,
		Summary:    fmt.Sprintf("Kept all %d existing %s", total, pluralItems(total)),
		FinishedAt: s.now(),
	}
	s.setSyncOutcome(pluginID, kindKey, outcome)
	if tracker := s.jobTracker(); tracker != nil {
		tracker.UpdateSyncJob(pluginID, kindKey, "done", "", outcome.Summary, "")
	}
	return s.SyncStateOf(ctx, pluginID, kindKey)
}

func pluralItems(n int) string {
	if n == 1 {
		return "item"
	}
	return "items"
}

func formatSyncOutcome(mode string, res SyncResult, totalAfter int) string {
	var parts []string
	if res.Deleted > 0 {
		parts = append(parts, fmt.Sprintf("Removed %d missing %s", res.Deleted, pluralItems(res.Deleted)))
	}
	if res.Upserted > 0 {
		parts = append(parts, fmt.Sprintf("updated %d %s", res.Upserted, pluralItems(res.Upserted)))
	}
	if len(parts) == 0 {
		if res.Protected > 0 {
			return fmt.Sprintf("Up to date · %d %s (%d kept by viewer activity)", totalAfter, pluralItems(totalAfter), res.Protected)
		}
		return fmt.Sprintf("Up to date · %d %s", totalAfter, pluralItems(totalAfter))
	}
	summary := strings.ToUpper(parts[0][:1]) + parts[0][1:]
	if len(parts) > 1 {
		summary += ", " + strings.Join(parts[1:], ", ")
	}
	summary += fmt.Sprintf(" · %d %s in library", totalAfter, pluralItems(totalAfter))
	if res.Protected > 0 {
		summary += fmt.Sprintf(" (%d kept by viewer activity)", res.Protected)
	}
	_ = mode
	return summary
}

func syncJobLabel(libName string, opts SyncOptions) string {
	switch {
	case opts.Force:
		return "Remove missing items · " + libName
	case opts.FullWalk:
		return "Full re-sync · " + libName
	default:
		return "Sync · " + libName
	}
}

// SyncKind executes a pull sync for a registered plugin kind (spec §3.5, §4.4).
//
// Sync algorithm:
//  1. Initial sync (when cursor is empty and the store has 0 items for the kind):
//     walks /ether/v1/kinds/{kind}/browse from the root level (parentId="") with
//     limit=200, then breadth-first into child levels with bounded concurrency (4).
//  2. Delta sync (when cursor != "" and storedCount > 0 and !opts.FullWalk):
//     calls POST /ether/v1/kinds/{kind}/changes with {"cursor": cursor, "limit": 500}.
//     If the plugin returns 404/501 (changes not implemented), falls back to re-walk diff.
//     Otherwise chains while complete == false, and records nextCursor in plugin_sync_state.
//  3. Fallback re-walk (when changes is not supported or FullWalk is requested
//     on an already-populated kind):
//     re-walks /browse, diffs by etag (skipping unchanged items), resets missed_walks=0
//     on seen items, increments missed_walks on unseen items, and deletes items unseen
//     in 2 consecutive complete walks.
//  4. Guardrail: if >50% of existing items in a library would be deleted in a single
//     pass (and the library has at least 2 items) and !opts.Force, the delete is
//     aborted, plugin_sync_state records the error, OnAttention/Health is notified,
//     and ErrDeleteGuard is returned.
func (s *Service) SyncKind(ctx context.Context, pluginID, kindKey string, opts SyncOptions) (SyncResult, error) {
	if s == nil || s.db == nil {
		return SyncResult{}, fmt.Errorf("pluginitems: nil service")
	}
	if !opts.rebuildOnly && !s.leader() {
		return SyncResult{}, nil
	}

	syncMu := s.syncLock(pluginID, kindKey)
	syncMu.Lock()
	defer syncMu.Unlock()
	if err := ctx.Err(); err != nil {
		return SyncResult{}, err
	}

	rec, err := s.GetKind(ctx, pluginID, kindKey)
	if err != nil {
		return SyncResult{}, err
	}
	rec, err = s.rebuildKind(ctx, rec)
	if err != nil {
		return SyncResult{}, err
	}
	if opts.rebuildOnly || rec.Decl.Sync.Mode == plugins.KindSyncPush || rec.Decl.Sync.Mode == "" {
		return SyncResult{}, nil
	}

	caller := opts.Caller
	if caller == nil {
		s.workersMu.Lock()
		caller = s.caller
		s.workersMu.Unlock()
	}
	if caller == nil {
		return SyncResult{}, fmt.Errorf("pluginitems: nil plugin route caller")
	}

	state, err := s.SyncStateOf(ctx, pluginID, kindKey)
	if err != nil {
		return SyncResult{}, err
	}
	storedCount, err := s.countKindItems(ctx, pluginID, kindKey)
	if err != nil {
		return SyncResult{}, err
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	startedAt := s.now()
	nowStart := startedAt.Unix()
	_, _ = s.db.ExecContext(runCtx,
		`UPDATE plugin_sync_state SET last_started = ? WHERE plugin_id = ? AND kind_key = ?`,
		nowStart, pluginID, kindKey)
	s.setSyncProgress(pluginID, kindKey, syncProgress{
		running:    true,
		phase:      "syncing",
		itemsTotal: storedCount,
	})
	defer s.clearSyncProgressIfPhase(pluginID, kindKey, "syncing")
	libName := rec.Decl.Label
	if libName == "" {
		libName = rec.Decl.Library.Name
	}
	if libName == "" {
		libName = kindKey
	}
	trigger := "scheduled"
	if opts.Manual || opts.FullWalk || opts.Force {
		trigger = "manual"
	}
	var job SyncJobHandle
	if tracker := s.jobTracker(); tracker != nil {
		job = tracker.BeginSyncJob(pluginID, kindKey, "/ether/v1/kinds/"+kindKey+"/sync", syncJobLabel(libName, opts), cancelRun)
		if job != nil {
			job.SetProgress("Starting sync…")
		}
	}
	s.emitSyncEvent(SyncEvent{
		Phase:       "started",
		PluginID:    pluginID,
		KindKey:     kindKey,
		LibraryID:   int64(rec.LibraryID),
		LibraryName: libName,
		Trigger:     trigger,
	})

	var (
		res     SyncResult
		syncErr error
	)
	switch {
	case !opts.FullWalk && state.Cursor != "" && storedCount > 0:
		var notSupported bool
		res, notSupported, syncErr = s.syncViaChanges(runCtx, caller, rec, state.Cursor, storedCount, opts, job)
		if notSupported {
			res, syncErr = s.syncRewalkDiff(runCtx, caller, rec, storedCount, opts, job)
		}
	case storedCount == 0 || (state.Cursor == "" && state.LastOK.IsZero()):
		res, syncErr = s.syncInitialWalk(runCtx, caller, rec, opts, job)
	default:
		res, syncErr = s.syncRewalkDiff(runCtx, caller, rec, storedCount, opts, job)
	}

	nowEnd := s.now().Unix()
	durMS := max(s.now().Sub(startedAt).Milliseconds(), int64(0))
	var totalAfter int
	if syncErr == nil {
		nextCursor := res.NextCursor
		if nextCursor == "" {
			nextCursor = state.Cursor
		}
		_, syncErr = s.runWriteChunk(runCtx, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			totalAfter, err = countKindItems(ctx, tx, pluginID, kindKey)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `
				UPDATE plugin_sync_state
				SET cursor = ?, generation = ?, last_ok = ?, last_error = '', failure_json = '', guard_json = '', items = ?
				WHERE plugin_id = ? AND kind_key = ?`,
				nextCursor, int64(s.syncGen.Add(1)), nowEnd, int64(totalAfter), pluginID, kindKey)
			return err
		})
	}
	if syncErr != nil {
		var guardErr *DeleteGuardError
		var failure *plugins.RouteFailure
		switch {
		case errors.As(syncErr, &guardErr):
			guardJSON, _ := json.Marshal(guardErr.State())
			_, _ = s.db.ExecContext(context.Background(),
				`UPDATE plugin_sync_state SET last_error = ?, failure_json = '', guard_json = ? WHERE plugin_id = ? AND kind_key = ?`,
				syncErr.Error(), string(guardJSON), pluginID, kindKey)
			s.noteAttention(context.Background(), pluginID, guardErr.PlainMessage())
			if job != nil {
				job.Finish("error", "delete_guard", "", guardErr.PlainMessage(), nil)
			}
		case errors.Is(syncErr, context.Canceled):
			if job != nil {
				job.Finish("canceled", "", "", "Cancelled", nil)
			}
		default:
			var failureJSON string
			if errors.As(syncErr, &failure) {
				b, _ := json.Marshal(failure)
				failureJSON = string(b)
			}
			_, _ = s.db.ExecContext(context.Background(),
				`UPDATE plugin_sync_state SET last_error = ?, failure_json = ?, guard_json = '' WHERE plugin_id = ? AND kind_key = ?`,
				syncErr.Error(), failureJSON, pluginID, kindKey)
			if !errors.Is(syncErr, plugins.ErrBusy) && !errors.Is(syncErr, errWriteChunkBudget) {
				s.noteAttention(context.Background(), pluginID, syncErr.Error())
			}
			if job != nil {
				job.Finish("error", "", "", syncErr.Error(), failure)
			}
		}
		s.emitSyncEvent(SyncEvent{
			Phase:       "failed",
			PluginID:    pluginID,
			KindKey:     kindKey,
			LibraryID:   int64(rec.LibraryID),
			LibraryName: libName,
			Trigger:     trigger,
			DurationMS:  durMS,
			Error:       syncErr.Error(),
		})
		return res, syncErr
	}

	mode := "sync"
	if opts.Force {
		mode = "force"
	} else if opts.FullWalk {
		mode = "full"
	}
	outcome := SyncOutcome{
		Mode:       mode,
		Upserted:   res.Upserted,
		Deleted:    res.Deleted,
		Skipped:    res.Skipped,
		Protected:  res.Protected,
		Total:      totalAfter,
		Summary:    formatSyncOutcome(mode, res, totalAfter),
		FinishedAt: s.now(),
	}
	s.setSyncOutcome(pluginID, kindKey, outcome)
	if job != nil {
		job.Finish("done", "", outcome.Summary, "", nil)
	}
	firstSync := state.LastOK.IsZero() || storedCount == 0
	s.noteLibraryItems(rec.LibraryID, int64(totalAfter), firstSync)
	s.emitSyncEvent(SyncEvent{
		Phase:       "completed",
		PluginID:    pluginID,
		KindKey:     kindKey,
		LibraryID:   int64(rec.LibraryID),
		LibraryName: libName,
		Trigger:     trigger,
		Added:       res.Upserted,
		Removed:     res.Deleted,
		DurationMS:  durMS,
	})
	return res, nil
}

func (s *Service) countKindItems(ctx context.Context, pluginID, kindKey string) (int, error) {
	return countKindItems(ctx, s.db, pluginID, kindKey)
}

func countKindItems(ctx context.Context, q queryer, pluginID, kindKey string) (int, error) {
	var total int
	err := q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(t.n), 0)
		FROM plugin_item_tally t
		JOIN plugin_kinds pk ON pk.library_id = t.library_id
		WHERE pk.plugin_id = ? AND pk.kind_key = ?`,
		pluginID, kindKey,
	).Scan(&total)
	return total, err
}

func exceedsDeleteGuard(storedCount, deleteCount int, force bool) bool {
	if force || storedCount < 2 || deleteCount <= 0 {
		return false
	}
	return float64(deleteCount)/float64(storedCount) > DeleteGuardRatio
}

// syncInitialWalk performs the initial breadth-first walk over /browse,
// persisting each fetched page incrementally so items appear in frontends
// immediately and partial progress survives errors or restarts, then seeds
// cursor if the kind implements /changes.
func (s *Service) syncInitialWalk(ctx context.Context, caller Caller, rec KindRecord, opts SyncOptions, job SyncJobHandle) (SyncResult, error) {
	var (
		res        SyncResult
		progressMu sync.Mutex
		pages      int
		firstNoted bool
	)
	onPage := func(batch []core.KindItem) error {
		if len(batch) == 0 {
			return nil
		}
		var pageUpserted int
		for start := 0; start < len(batch); start += MaxUpsertBatch {
			end := min(start+MaxUpsertBatch, len(batch))
			n, err := s.upsertItemsForSync(ctx, rec.PluginID, rec.KindKey, batch[start:end])
			if err != nil {
				return err
			}
			pageUpserted += n
		}
		totalNow, _ := s.countKindItems(ctx, rec.PluginID, rec.KindKey)
		progressMu.Lock()
		res.Upserted += pageUpserted
		pages++
		upsertedSoFar := res.Upserted
		pagesSoFar := pages
		notifyFirst := !firstNoted && totalNow > 0
		if notifyFirst {
			firstNoted = true
		}
		progressMu.Unlock()

		s.setSyncProgress(rec.PluginID, rec.KindKey, syncProgress{
			running:     true,
			phase:       "syncing",
			syncedItems: upsertedSoFar,
			pages:       pagesSoFar,
			itemsTotal:  totalNow,
		})
		if job != nil {
			job.SetProgress(fmt.Sprintf("Page %d · %d %s synced", pagesSoFar, upsertedSoFar, pluralItems(upsertedSoFar)))
		}
		if notifyFirst {
			s.noteLibraryItems(rec.LibraryID, int64(totalNow), true)
		}
		return nil
	}

	_, retryAfter, err := s.walkBrowseTreeStream(ctx, caller, rec, opts.Concurrency, false, onPage)
	res.RetryAfter = retryAfter
	if err != nil {
		return res, err
	}
	// Probe /changes with empty cursor to capture baseline NextCursor if supported.
	page, notSupp, _, err := s.callKindChanges(ctx, caller, rec.PluginID, rec.KindKey, "", 1)
	if err == nil && !notSupp && page.NextCursor != "" {
		res.NextCursor = page.NextCursor
	}
	return res, nil
}

// syncViaChanges chains /changes pages from sinceCursor until Complete is true.
func (s *Service) syncViaChanges(ctx context.Context, caller Caller, rec KindRecord, sinceCursor string, storedCount int, opts SyncOptions, job SyncJobHandle) (SyncResult, bool, error) {
	var (
		res        SyncResult
		cursor     = sinceCursor
		allUpserts []core.KindItem
		allDeletes []string
	)
	seenChanges := 0
	checkpointable := true
	for pageIdx := 0; pageIdx < 1000; pageIdx++ {
		page, notSupp, retryAfter, err := s.callKindChanges(ctx, caller, rec.PluginID, rec.KindKey, cursor, DefaultChangesLimit)
		for attempt := 0; err != nil && waitRetryAfter(ctx, retryAfter, attempt); attempt++ {
			page, notSupp, retryAfter, err = s.callKindChanges(ctx, caller, rec.PluginID, rec.KindKey, cursor, DefaultChangesLimit)
		}
		if retryAfter > res.RetryAfter {
			res.RetryAfter = retryAfter
		}
		if notSupp {
			return res, true, nil
		}
		if err != nil {
			return res, false, err
		}
		allUpserts = append(allUpserts, page.Upserts...)
		allDeletes = append(allDeletes, page.Deletes...)
		seenChanges += len(page.Upserts) + len(page.Deletes)
		// Tombstones and ordinary feeds retain whole-pass validation. A producer
		// may opt in when its page is independently applicable and replayable.
		checkpointable = checkpointable && page.Checkpoint && len(page.Deletes) == 0
		if checkpointable && !page.Complete && page.NextCursor != "" {
			if page.NextCursor == cursor {
				return res, false, errors.New("pluginitems: checkpoint cursor did not advance")
			}
			if err := s.applyChangeUpserts(ctx, rec, allUpserts, &res); err != nil {
				return res, false, err
			}
			// Advance only after every upsert committed. An interruption before
			// this write replays the page through the idempotent upsert path.
			_, err := s.runWriteChunk(ctx, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, "UPDATE plugin_sync_state SET cursor=? WHERE plugin_id=? AND kind_key=?", page.NextCursor, rec.PluginID, rec.KindKey)
				return err
			})
			if err != nil {
				return res, false, err
			}
			allUpserts = nil
		}
		s.setSyncProgress(rec.PluginID, rec.KindKey, syncProgress{
			running:     true,
			phase:       "syncing",
			syncedItems: seenChanges,
			pages:       pageIdx + 1,
			itemsTotal:  storedCount,
		})
		if job != nil {
			job.SetProgress(fmt.Sprintf("Page %d · %d changes checked", pageIdx+1, seenChanges))
		}
		if page.NextCursor != "" {
			cursor = page.NextCursor
			res.NextCursor = page.NextCursor
		}
		if page.Complete || page.NextCursor == "" {
			break
		}
		if pageIdx == 999 {
			return res, false, errors.New("pluginitems: changes feed exceeded 1000 pages")
		}
	}

	// Only live tombstones contribute to the guard. On a retry the plugin
	// can replay the same cursor even though some deletes already committed.
	if exceedsDeleteGuard(storedCount, len(allDeletes), opts.Force) {
		existing, err := s.lookupLevelsByExternalIDs(ctx, rec.PluginID, rec.KindKey, allDeletes)
		if err != nil {
			return res, false, err
		}
		if exceedsDeleteGuard(storedCount, len(existing), opts.Force) {
			return res, false, &DeleteGuardError{
				PluginID: rec.PluginID,
				KindKey:  rec.KindKey,
				Source:   "changes",
				Missing:  len(existing),
				Total:    storedCount,
			}
		}
	}

	if err := s.applyChangeUpserts(ctx, rec, allUpserts, &res); err != nil {
		return res, false, err
	}

	for start := 0; start < len(allDeletes); start += MaxUpsertBatch {
		end := min(start+MaxUpsertBatch, len(allDeletes))
		if err := s.DeleteItems(ctx, rec.PluginID, rec.KindKey, allDeletes[start:end]); err != nil {
			return res, false, err
		}
		res.Deleted += end - start
	}

	return res, false, nil
}

func (s *Service) applyChangeUpserts(ctx context.Context, rec KindRecord, items []core.KindItem, res *SyncResult) error {
	for start := 0; start < len(items); start += MaxUpsertBatch {
		end := min(start+MaxUpsertBatch, len(items))
		n, err := s.upsertItemsForSync(ctx, rec.PluginID, rec.KindKey, items[start:end])
		if err != nil {
			return err
		}
		res.Upserted += n
	}
	return nil
}

// syncRewalkDiff re-walks /browse, diffs items by etag, and deletes unpinned,
// unengaged items unseen in 2 consecutive complete walks (subject to the
// 50%-delete guard). Items that were live-searched, expanded, opened, played,
// favourited, watchlisted, playlisted, or resolved are protected from
// missed-walk pruning so search-API plugins never drop viewer-engaged items.
func (s *Service) syncRewalkDiff(ctx context.Context, caller Caller, rec KindRecord, storedCount int, opts SyncOptions, job SyncJobHandle) (SyncResult, error) {
	var (
		res         SyncResult
		progressMu  sync.Mutex
		walkedPages int
		walkedItems int
	)
	onPage := func(batch []core.KindItem) error {
		progressMu.Lock()
		walkedPages++
		walkedItems += len(batch)
		p := walkedPages
		n := walkedItems
		progressMu.Unlock()
		s.setSyncProgress(rec.PluginID, rec.KindKey, syncProgress{
			running:     true,
			phase:       "syncing",
			syncedItems: n,
			pages:       p,
			itemsTotal:  storedCount,
		})
		if job != nil {
			job.SetProgress(fmt.Sprintf("Page %d · %d %s checked", p, n, pluralItems(n)))
		}
		return nil
	}
	walked, retryAfter, err := s.walkBrowseTreeStream(ctx, caller, rec, opts.Concurrency, true, onPage)
	res.RetryAfter = retryAfter
	if err != nil {
		return res, err
	}

	existingRows, err := s.loadExistingSyncRows(ctx, rec.PluginID, rec.KindKey)
	if err != nil {
		return res, err
	}
	byExt := make(map[string]existingSyncRow, len(existingRows))
	byRef := make(map[core.ItemRef]existingSyncRow, len(existingRows))
	for _, r := range existingRows {
		byExt[r.extID] = r
		byRef[r.ref] = r
	}

	seen := make(map[string]bool, len(walked))
	var toUpsert []core.KindItem
	var seenUnchanged []string

	for _, item := range walked {
		extID := strings.TrimSpace(item.ID)
		if extID == "" {
			continue
		}
		seen[extID] = true
		old, exists := byExt[extID]
		if exists && item.ETag != "" && old.etag == item.ETag {
			res.Skipped++
			if old.missed > 0 {
				seenUnchanged = append(seenUnchanged, extID)
			}
			continue
		}
		toUpsert = append(toUpsert, item)
	}

	// Identify unseen items and protect any item that is pinned (live-searched,
	// expanded, or opened) or has user engagement (played, favourited,
	// watchlisted, playlisted, resolved, or manually matched), plus ancestors
	// of any seen or protected item so parent deletion cannot cascade to them.
	protected := make(map[string]bool)
	var unpinnedUnseen []existingSyncRow
	for _, r := range existingRows {
		if seen[r.extID] {
			continue
		}
		if r.pinned {
			protected[r.extID] = true
		} else {
			unpinnedUnseen = append(unpinnedUnseen, r)
		}
	}
	if len(unpinnedUnseen) > 0 {
		engaged, err := s.findEngagedUnseen(ctx, unpinnedUnseen)
		if err != nil {
			return res, err
		}
		for extID := range engaged {
			protected[extID] = true
		}
	}
	// Protect unseen ancestors of any seen or protected item.
	protectAncestors := func(parentRef core.ItemRef) {
		cur := parentRef
		for depth := 0; depth < 6 && cur != ""; depth++ {
			parent, ok := byRef[cur]
			if !ok {
				break
			}
			if !seen[parent.extID] {
				protected[parent.extID] = true
			}
			cur = parent.parentRef
		}
	}
	for _, r := range existingRows {
		if seen[r.extID] || protected[r.extID] {
			protectAncestors(r.parentRef)
		}
	}

	// Partition unseen prunable items into deletions and missed-walk increments.
	var (
		toIncrement []string
		toDelete    []string
	)
	for _, r := range existingRows {
		if seen[r.extID] || protected[r.extID] {
			continue
		}
		if opts.Force || r.missed+1 >= 2 {
			toDelete = append(toDelete, r.extID)
		} else {
			toIncrement = append(toIncrement, r.extID)
		}
	}

	protectedCount := len(protected)
	prunableTotal := len(existingRows) - protectedCount
	unseenCount := len(toIncrement) + len(toDelete)
	res.Protected = protectedCount
	if exceedsDeleteGuard(prunableTotal, len(toDelete), opts.Force) || exceedsDeleteGuard(prunableTotal, unseenCount, opts.Force) {
		return res, &DeleteGuardError{
			PluginID:  rec.PluginID,
			KindKey:   rec.KindKey,
			Source:    "walk",
			Missing:   unseenCount,
			Total:     prunableTotal,
			Protected: protectedCount,
		}
	}

	for start := 0; start < len(toUpsert); start += MaxUpsertBatch {
		end := min(start+MaxUpsertBatch, len(toUpsert))
		n, err := s.upsertItemsForSync(ctx, rec.PluginID, rec.KindKey, toUpsert[start:end])
		if err != nil {
			return res, err
		}
		res.Upserted += n
	}

	if len(seenUnchanged) > 0 {
		if err := s.updateMissedWalks(ctx, rec.PluginID, rec.KindKey, seenUnchanged, false); err != nil {
			return res, err
		}
	}
	if len(toIncrement) > 0 {
		if err := s.updateMissedWalks(ctx, rec.PluginID, rec.KindKey, toIncrement, true); err != nil {
			return res, err
		}
	}
	for start := 0; start < len(toDelete); start += MaxUpsertBatch {
		end := min(start+MaxUpsertBatch, len(toDelete))
		if err := s.DeleteItems(ctx, rec.PluginID, rec.KindKey, toDelete[start:end]); err != nil {
			return res, err
		}
		res.Deleted += end - start
	}

	return res, nil
}

type existingSyncRow struct {
	ref       core.ItemRef
	parentRef core.ItemRef
	extID     string
	seq       int64
	etag      string
	missed    int
	pinned    bool
}

func (s *Service) loadExistingSyncRows(ctx context.Context, pluginID, kindKey string) ([]existingSyncRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT item_ref, parent_ref, external_id, seq, etag, missed_walks, pinned
		FROM plugin_items
		WHERE plugin_id = ? AND kind_key = ?`,
		pluginID, kindKey,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []existingSyncRow
	for rows.Next() {
		var r existingSyncRow
		var refStr, parentStr string
		var pinned int
		if err := rows.Scan(&refStr, &parentStr, &r.extID, &r.seq, &r.etag, &r.missed, &pinned); err != nil {
			return nil, err
		}
		r.ref = core.ItemRef(refStr)
		r.parentRef = core.ItemRef(parentStr)
		r.pinned = pinned != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) hasTableDB(ctx context.Context, table string) bool {
	var n int
	if store.DialectOf(s.db) == store.Postgres {
		_ = s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = ?`,
			table).Scan(&n)
		return n > 0
	}
	_ = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`,
		table).Scan(&n)
	return n > 0
}

// findEngagedUnseen returns the external IDs among candidates that have viewer
// engagement in watch/read state, play history, playlists, remembered sources,
// watchlists, favourites, resolutions, or owner-pinned matches. Queries are
// chunked over unseen unpinned items only and skip optional tables absent in
// isolated unit-test schemas.
func (s *Service) findEngagedUnseen(ctx context.Context, candidates []existingSyncRow) (map[string]bool, error) {
	engaged := make(map[string]bool)
	if len(candidates) == 0 {
		return engaged, nil
	}
	refToExt := make(map[string]string, len(candidates))
	keyToExt := make(map[int64]string, len(candidates))
	strKeyToExt := make(map[string]string, len(candidates))
	refs := make([]string, 0, len(candidates))
	keys := make([]int64, 0, len(candidates))
	strKeys := make([]string, 0, len(candidates))
	for _, r := range candidates {
		rs := string(r.ref)
		refToExt[rs] = r.extID
		refs = append(refs, rs)
		if r.seq > 0 && r.seq < core.PluginKeyMaxSeq {
			k := int64(core.PluginKey(r.seq))
			keyToExt[k] = r.extID
			keys = append(keys, k)
			sk := strconv.FormatInt(k, 10)
			strKeyToExt[sk] = r.extID
			strKeys = append(strKeys, sk)
		}
	}

	const chunkSize = 250
	queryRefs := func(table, queryPrefix string) error {
		if !s.hasTableDB(ctx, table) {
			return nil
		}
		for start := 0; start < len(refs); start += chunkSize {
			end := min(start+chunkSize, len(refs))
			chunk := refs[start:end]
			marks := make([]string, len(chunk))
			args := make([]any, len(chunk))
			for i, v := range chunk {
				marks[i] = "?"
				args[i] = v
			}
			rows, err := s.db.QueryContext(ctx, queryPrefix+"("+strings.Join(marks, ",")+")", args...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var ref string
				if err := rows.Scan(&ref); err != nil {
					rows.Close()
					return err
				}
				if ext, ok := refToExt[ref]; ok {
					engaged[ext] = true
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
		}
		return nil
	}

	queryIntKeys := func(table, col string) error {
		if len(keys) == 0 || !s.hasTableDB(ctx, table) {
			return nil
		}
		for start := 0; start < len(keys); start += chunkSize {
			end := min(start+chunkSize, len(keys))
			chunk := keys[start:end]
			marks := make([]string, len(chunk))
			args := make([]any, len(chunk))
			for i, v := range chunk {
				marks[i] = "?"
				args[i] = v
			}
			rows, err := s.db.QueryContext(ctx,
				fmt.Sprintf("SELECT DISTINCT %s FROM %s WHERE %s IN (%s)", col, table, col, strings.Join(marks, ",")),
				args...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var k int64
				if err := rows.Scan(&k); err != nil {
					rows.Close()
					return err
				}
				if ext, ok := keyToExt[k]; ok {
					engaged[ext] = true
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
		}
		return nil
	}

	if err := queryRefs("plugin_resolutions", `SELECT DISTINCT item_ref FROM plugin_resolutions WHERE item_ref IN `); err != nil {
		return nil, err
	}
	if err := queryRefs("plugin_matches", `SELECT DISTINCT item_ref FROM plugin_matches WHERE chosen = 1 AND item_ref IN `); err != nil {
		return nil, err
	}
	for _, tc := range [][2]string{
		{"state_watch", "rating_key"},
		{"state_history", "rating_key"},
		{"state_playlist_items", "rating_key"},
		{"state_last_source", "rating_key"},
		{"watchlist_items", "item_key"},
	} {
		if err := queryIntKeys(tc[0], tc[1]); err != nil {
			return nil, err
		}
	}
	if len(strKeys) > 0 && s.hasTableDB(ctx, "favorites_items") {
		for start := 0; start < len(strKeys); start += chunkSize {
			end := min(start+chunkSize, len(strKeys))
			chunk := strKeys[start:end]
			marks := make([]string, len(chunk))
			args := make([]any, 0, len(chunk)+1)
			args = append(args, "title")
			for i, v := range chunk {
				marks[i] = "?"
				args = append(args, v)
			}
			rows, err := s.db.QueryContext(ctx,
				`SELECT DISTINCT subject_id FROM favorites_items WHERE kind = ? AND subject_id IN (`+strings.Join(marks, ",")+`)`,
				args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var sid string
				if err := rows.Scan(&sid); err != nil {
					rows.Close()
					return nil, err
				}
				if ext, ok := strKeyToExt[sid]; ok {
					engaged[ext] = true
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			rows.Close()
		}
	}
	return engaged, nil
}

// updateMissedWalks groups independent bookkeeping changes into bounded
// indexed writes. The missing case only changes rows that still have zero
// misses, so replay cannot increment the same miss twice.
func (s *Service) updateMissedWalks(ctx context.Context, pluginID, kindKey string, externalIDs []string, missing bool) error {
	if missing {
		return s.writeMissedWalks(ctx, pluginID, kindKey, externalIDs,
			`UPDATE plugin_items SET missed_walks = 1 WHERE missed_walks = 0 AND pinned = 0`)
	}
	return s.writeMissedWalks(ctx, pluginID, kindKey, externalIDs,
		`UPDATE plugin_items SET seen_at = ?, missed_walks = 0 WHERE missed_walks <> 0`, s.now().Unix())
}

func (s *Service) writeMissedWalks(ctx context.Context, pluginID, kindKey string, ids []string, update string, updateArgs ...any) error {
	return s.writeChunks(ctx, len(ids), func(start, end int) (time.Duration, error) {
		args := make([]any, 0, len(updateArgs)+end-start)
		args = append(args, updateArgs...)
		for _, id := range ids[start:end] {
			args = append(args, string(core.NewItemRef(pluginID, kindKey, id)))
		}
		query := update + ` AND item_ref IN (` + store.Placeholders(end-start) + `)`
		return s.runWriteChunk(ctx, func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, query, args...)
			return err
		})
	})
}

func (s *Service) missedWalkIDs(ctx context.Context, pluginID, kindKey, after string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT external_id FROM plugin_items
WHERE plugin_id = ? AND kind_key = ? AND external_id > ? AND missed_walks <> 0
ORDER BY external_id LIMIT ?`, pluginID, kindKey, after, upsertChunkSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// walkBrowseTree walks /ether/v1/kinds/{kind}/browse starting at the root level
// (parentId="") with limit=200, then breadth-first into child levels with bounded
// concurrency (default MaxSyncConcurrency = 4).
func (s *Service) walkBrowseTree(ctx context.Context, caller Caller, rec KindRecord, concurrency int) ([]core.KindItem, time.Duration, error) {
	return s.walkBrowseTreeStream(ctx, caller, rec, concurrency, true, nil)
}

func (s *Service) walkBrowseTreeStream(ctx context.Context, caller Caller, rec KindRecord, concurrency int, collectAll bool, onPage func([]core.KindItem) error) ([]core.KindItem, time.Duration, error) {
	if concurrency <= 0 {
		concurrency = MaxSyncConcurrency
	}
	childLevelsOf := make(map[string][]string, len(rec.Decl.Levels))
	rootLevel := "item"
	if len(rec.Decl.Levels) > 0 {
		rootLevel = rec.Decl.Levels[0].Key
		for i := 1; i < len(rec.Decl.Levels); i++ {
			lv := rec.Decl.Levels[i]
			parent := rec.Decl.LevelParent(lv.Key)
			if parent != "" {
				childLevelsOf[parent] = append(childLevelsOf[parent], lv.Key)
			}
		}
	}

	rootHasChildren := len(childLevelsOf[rootLevel]) > 0
	keepRoot := collectAll || rootHasChildren
	rootItems, maxRetry, err := s.fetchAllBrowsePagesStream(ctx, caller, rec.PluginID, rec.KindKey, rootLevel, "", keepRoot, onPage)
	if err != nil {
		return nil, maxRetry, err
	}

	var allItems []core.KindItem
	if collectAll {
		allItems = make([]core.KindItem, 0, len(rootItems))
		allItems = append(allItems, rootItems...)
	}

	type levelWave struct {
		level string
		items []core.KindItem
	}
	queue := []levelWave{{level: rootLevel, items: rootItems}}

	for len(queue) > 0 {
		wave := queue[0]
		queue = queue[1:]

		nextLevels := childLevelsOf[wave.level]
		if len(nextLevels) == 0 || len(wave.items) == 0 {
			continue
		}

		for _, nextLevel := range nextLevels {
			nextHasChildren := len(childLevelsOf[nextLevel]) > 0
			keepNext := collectAll || nextHasChildren

			type parentTask struct {
				parentExtID string
			}
			tasks := make([]parentTask, 0, len(wave.items))
			for _, p := range wave.items {
				if p.ID != "" {
					tasks = append(tasks, parentTask{parentExtID: p.ID})
				}
			}
			if len(tasks) == 0 {
				continue
			}

			type taskResult struct {
				idx        int
				items      []core.KindItem
				retryAfter time.Duration
				err        error
			}
			results := make([]taskResult, len(tasks))
			sem := make(chan struct{}, concurrency)
			var wg sync.WaitGroup

			for i, t := range tasks {
				i, t := i, t
				wg.Add(1)
				go func() {
					defer wg.Done()
					select {
					case sem <- struct{}{}:
					case <-ctx.Done():
						results[i] = taskResult{idx: i, err: ctx.Err()}
						return
					}
					defer func() { <-sem }()

					children, ra, err := s.fetchAllBrowsePagesStream(ctx, caller, rec.PluginID, rec.KindKey, nextLevel, t.parentExtID, keepNext, onPage)
					results[i] = taskResult{
						idx:        i,
						items:      children,
						retryAfter: ra,
						err:        err,
					}
				}()
			}
			wg.Wait()

			var nextWave []core.KindItem
			var canceled error
			for _, r := range results {
				if r.retryAfter > maxRetry {
					maxRetry = r.retryAfter
				}
				if errors.Is(r.err, context.Canceled) {
					canceled = r.err
					continue
				}
				if r.err != nil {
					return nil, maxRetry, r.err
				}
				for j := range r.items {
					if r.items[j].ParentID == "" {
						r.items[j].ParentID = tasks[r.idx].parentExtID
					}
					if r.items[j].Level == "" {
						r.items[j].Level = nextLevel
					}
				}
				if collectAll {
					allItems = append(allItems, r.items...)
				}
				if nextHasChildren {
					nextWave = append(nextWave, r.items...)
				}
			}

			if canceled != nil {
				return nil, maxRetry, canceled
			}
			if nextHasChildren && len(nextWave) > 0 {
				queue = append(queue, levelWave{level: nextLevel, items: nextWave})
			}
		}
	}

	// List levels (members: true) are roots of their own. A page of one may
	// carry the lists' members and their parents too; nothing below a list
	// is walked, its members are what it names.
	for _, lv := range rec.Decl.ListLevels() {
		items, retry, err := s.fetchAllBrowsePagesStream(ctx, caller, rec.PluginID, rec.KindKey, lv.Key, "", collectAll, onPage)
		if retry > maxRetry {
			maxRetry = retry
		}
		if err != nil {
			return nil, maxRetry, err
		}
		if collectAll {
			allItems = append(allItems, items...)
		}
	}

	return allItems, maxRetry, nil
}

func (s *Service) fetchAllBrowsePages(ctx context.Context, caller Caller, pluginID, kindKey, level, parentID string) ([]core.KindItem, time.Duration, error) {
	return s.fetchAllBrowsePagesStream(ctx, caller, pluginID, kindKey, level, parentID, true, nil)
}

func (s *Service) fetchAllBrowsePagesStream(ctx context.Context, caller Caller, pluginID, kindKey, level, parentID string, collect bool, onPage func([]core.KindItem) error) ([]core.KindItem, time.Duration, error) {
	var (
		all        []core.KindItem
		cursor     string
		maxRetry   time.Duration
		seenCursor = make(map[string]bool)
	)
	for pageIdx := 0; pageIdx < 10000; pageIdx++ {
		req := core.KindBrowseRequest{
			ParentID: parentID,
			Level:    level,
			Cursor:   cursor,
			Limit:    DefaultWalkPageLimit,
		}
		page, ra, err := s.callKindBrowse(ctx, caller, pluginID, kindKey, req)
		for attempt := 0; err != nil && waitRetryAfter(ctx, ra, attempt); attempt++ {
			page, ra, err = s.callKindBrowse(ctx, caller, pluginID, kindKey, req)
		}
		if ra > maxRetry {
			maxRetry = ra
		}
		if err != nil {
			return nil, maxRetry, err
		}
		for i := range page.Items {
			if page.Items[i].Level == "" {
				page.Items[i].Level = level
			}
			if page.Items[i].ParentID == "" && parentID != "" {
				page.Items[i].ParentID = parentID
			}
		}
		if onPage != nil && len(page.Items) > 0 {
			if err := onPage(page.Items); err != nil {
				return nil, maxRetry, err
			}
		}
		if collect {
			all = append(all, page.Items...)
		}
		next := strings.TrimSpace(page.NextCursor)
		if next == "" || len(page.Items) == 0 || seenCursor[next] {
			break
		}
		seenCursor[next] = true
		cursor = next
	}
	return all, maxRetry, nil
}

func (s *Service) callKindBrowse(ctx context.Context, caller Caller, pluginID, kindKey string, req core.KindBrowseRequest) (core.KindPage, time.Duration, error) {
	var page core.KindPage
	_, retryAfter, err := s.callSyncRoute(ctx, caller, pluginID, kindKey, "browse", req, &page)
	return page, retryAfter, err
}

func (s *Service) callKindChanges(ctx context.Context, caller Caller, pluginID, kindKey, cursor string, limit int) (core.KindChangePage, bool, time.Duration, error) {
	req := struct {
		Cursor     string `json:"cursor,omitempty"`
		SinceToken string `json:"since_token,omitempty"`
		Limit      int    `json:"limit"`
	}{Cursor: cursor, SinceToken: cursor, Limit: limit}
	var page core.KindChangePage
	unsupported, retryAfter, err := s.callSyncRoute(ctx, caller, pluginID, kindKey, "changes", req, &page)
	return page, unsupported, retryAfter, err
}

func (s *Service) callSyncRoute(ctx context.Context, caller Caller, pluginID, kindKey, route string, input, page any) (bool, time.Duration, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return false, 0, err
	}
	path := "/ether/v1/kinds/" + kindKey + "/" + route
	var checked bool
	check := func(raw json.RawMessage) error {
		checked = true
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || len(raw) > MaxBrowseBodyBytes || raw[0] != '{' {
			return plugins.ErrMalformedResponse
		}
		return json.Unmarshal(raw, page)
	}
	resp, err := caller.Call(plugins.WithBackgroundCall(ctx), pluginID, plugins.Request{
		PluginID:      pluginID,
		Method:        http.MethodPost,
		Path:          path,
		Headers:       map[string]string{"content-type": "application/json"},
		Body:          body,
		CheckResponse: check,
	})
	// A runtime crash can cancel the sync job as the runtime is closed.
	// Preserve the diagnosed failure from the call that triggered that close.
	if ctx.Err() != nil && resp.Failure == nil {
		return false, 0, ctx.Err()
	}
	if err != nil && resp.Failure == nil {
		return false, 0, err
	}
	// An optional changes feed can declare itself unavailable. A 404 with a
	// reason is a failed feed, so its diagnostic must reach the owner.
	if route == "changes" && (resp.Status == http.StatusNotImplemented ||
		resp.Status == http.StatusNotFound && len(bytes.TrimSpace(resp.Body)) == 0) {
		return true, 0, nil
	}
	failure := resp.Failure
	if failure == nil {
		failure = plugins.DiagnoseRouteResponse(path, resp, s.now(), nil)
	}
	if failure != nil {
		return false, time.Duration(failure.RetryAfterSeconds) * time.Second, failure
	}
	if !checked && check(resp.Body) != nil {
		return false, 0, &plugins.RouteFailure{Code: "malformed", Route: path, Status: resp.Status}
	}
	return false, 0, nil
}

// maxSyncRetryWait and maxSyncRetries bound how long one sync waits out a
// plugin's rate limit (a 429 or 503 with Retry-After) before giving up.
const (
	maxSyncRetryWait = 2 * time.Minute
	maxSyncRetries   = 8
)

// waitRetryAfter waits out a short Retry-After so a rate-limited page is
// retried in place instead of failing the whole sync. It reports whether
// the caller should try the same request again.
func waitRetryAfter(ctx context.Context, ra time.Duration, attempt int) bool {
	if ra <= 0 || ra > maxSyncRetryWait || attempt >= maxSyncRetries {
		return false
	}
	t := time.NewTimer(ra)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// StartKindSync starts (or restarts) a background pull-sync worker for a kind
// without blocking the caller on an in-flight previous pass. Any superseded
// worker is cancelled immediately and drained by the new worker goroutine
// before starting the next pass, preserving strict per-kind serialization.
func (s *Service) StartKindSync(parentCtx context.Context, pluginID, kindKey string, interval time.Duration) {
	if s == nil {
		return
	}
	if interval <= 0 {
		interval = 15 * time.Minute
	}

	ctx, cancel := context.WithCancel(parentCtx)
	done := make(chan struct{})
	key := workerKey(pluginID, kindKey)

	s.workersMu.Lock()
	var prevDone chan struct{}
	if prev, ok := s.workers[key]; ok && prev != nil {
		prev.cancel()
		prevDone = prev.done
	} else if d, ok := s.draining[key]; ok {
		prevDone = d
	}
	var reindexDone chan struct{}
	if rw, ok := s.reindexers[key]; ok && rw != nil {
		reindexDone = rw.done
	}
	delete(s.draining, key)
	s.workers[key] = &syncWorker{cancel: cancel, done: done}
	s.workersMu.Unlock()

	go func() {
		defer close(done)
		if prevDone != nil {
			// prev was already cancelled; wait for it to finish so Close/StopKindSync
			// transitively drain every superseded worker before returning.
			<-prevDone
		}
		if ctx.Err() != nil {
			return
		}
		if reindexDone != nil {
			select {
			case <-reindexDone:
			case <-ctx.Done():
				return
			}
		}
		res, _ := s.SyncKind(ctx, pluginID, kindKey, SyncOptions{})
		wait := interval
		if res.RetryAfter > wait {
			wait = res.RetryAfter
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				res, _ := s.SyncKind(ctx, pluginID, kindKey, SyncOptions{})
				nextWait := interval
				if res.RetryAfter > nextWait {
					nextWait = res.RetryAfter
				}
				timer.Reset(nextWait)
			}
		}
	}()
}

// CancelKindSync cancels any running background sync worker for (pluginID,
// kindKey) without blocking the caller on an in-flight network or plugin call.
// Subsequent StartKindSync, StopKindSync, PurgeKind, or Close calls still wait
// for the cancelled worker to exit.
func (s *Service) CancelKindSync(pluginID, kindKey string) {
	if s == nil {
		return
	}
	key := workerKey(pluginID, kindKey)
	s.workersMu.Lock()
	if w, ok := s.workers[key]; ok {
		delete(s.workers, key)
		if w != nil {
			s.draining[key] = w.done
			w.cancel()
		}
	}
	s.workersMu.Unlock()
}

// StopKindSync stops the background sync and reindex workers for (pluginID,
// kindKey) and waits for any in-flight pass to exit.
func (s *Service) StopKindSync(pluginID, kindKey string) {
	if s == nil {
		return
	}
	key := workerKey(pluginID, kindKey)
	s.workersMu.Lock()
	w, ok := s.workers[key]
	if ok {
		delete(s.workers, key)
	}
	mw := s.manualSyncs[key]
	delete(s.manualSyncs, key)
	drain := s.draining[key]
	delete(s.draining, key)
	rw := s.reindexers[key]
	delete(s.reindexers, key)
	s.workersMu.Unlock()
	if ok && w != nil {
		w.cancel()
		<-w.done
	}
	if mw != nil {
		mw.cancel()
		<-mw.done
	}
	if drain != nil {
		<-drain
	}
	if rw != nil {
		rw.cancel()
		<-rw.done
	}
}

// StopPluginSync stops all background sync and reindex workers belonging to pluginID.
func (s *Service) StopPluginSync(pluginID string) {
	if s == nil {
		return
	}
	prefix := pluginID + "\x00"
	s.workersMu.Lock()
	var toWait []*syncWorker
	var drains []chan struct{}
	for k, w := range s.workers {
		if strings.HasPrefix(k, prefix) {
			delete(s.workers, k)
			toWait = append(toWait, w)
		}
	}
	for k, mw := range s.manualSyncs {
		if strings.HasPrefix(k, prefix) {
			delete(s.manualSyncs, k)
			toWait = append(toWait, mw)
		}
	}
	for k, d := range s.draining {
		if strings.HasPrefix(k, prefix) {
			delete(s.draining, k)
			drains = append(drains, d)
		}
	}
	for k, rw := range s.reindexers {
		if strings.HasPrefix(k, prefix) {
			delete(s.reindexers, k)
			toWait = append(toWait, rw)
		}
	}
	s.workersMu.Unlock()
	for _, w := range toWait {
		if w != nil {
			w.cancel()
			<-w.done
		}
	}
	for _, d := range drains {
		if d != nil {
			<-d
		}
	}
}

// Close stops all background sync workers, reindexers, and planner statistics refreshes.
func (s *Service) Close() {
	if s == nil {
		return
	}
	if s.bgCancel != nil {
		s.bgCancel()
	}
	s.workersMu.Lock()
	var toWait []*syncWorker
	var drains []chan struct{}
	for k, w := range s.workers {
		delete(s.workers, k)
		toWait = append(toWait, w)
	}
	for k, mw := range s.manualSyncs {
		delete(s.manualSyncs, k)
		toWait = append(toWait, mw)
	}
	for k, d := range s.draining {
		delete(s.draining, k)
		drains = append(drains, d)
	}
	for k, rw := range s.reindexers {
		delete(s.reindexers, k)
		toWait = append(toWait, rw)
	}
	s.workersMu.Unlock()
	for _, w := range toWait {
		if w != nil {
			w.cancel()
			<-w.done
		}
	}
	for _, d := range drains {
		if d != nil {
			<-d
		}
	}
	if s.pinDone != nil {
		<-s.pinDone
	}
	s.stats.close()
}

func workerKey(pluginID, kindKey string) string {
	return pluginID + "\x00" + kindKey
}
