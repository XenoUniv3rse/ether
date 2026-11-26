package pluginitems

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// upsertChunkSize bounds one write transaction inside an upsert batch
// (#646), and one keyset batch of a kind reindex. The per-kind lock is held
// across chunks, so a batch still looks atomic to the kind's own writers,
// but the database write gate is released between chunks and unrelated
// writers interleave instead of timing out behind a minutes-long republish.
const upsertChunkSize = 200

// An upsert batch sizes its chunks by how long the previous chunk held the
// writer, aiming for upsertChunkTarget. What a chunk costs varies by two
// orders of magnitude: re-seeing unchanged items is a few microseconds each,
// while a new item writes its row, ~10 field rows across five indexes, FTS
// tokens and tallies, and on a cold cache costs ~3 ms. A fixed 200 held the
// writer 0.6-3 s for new items on the lab, so playback and session writes
// queued behind it.
const (
	upsertChunkTarget = 25 * time.Millisecond
	upsertChunkFirst  = 8
	writeChunkBudget  = 200 * time.Millisecond
)

// nextUpsertChunkSize scales the chunk that held the writer for held toward
// upsertChunkTarget, at most doubling per step.
func nextUpsertChunkSize(n int, held time.Duration) int {
	if held <= 0 {
		return min(2*n, upsertChunkSize)
	}
	next := int(float64(n) * float64(upsertChunkTarget) / float64(held))
	return max(1, min(next, 2*n, upsertChunkSize))
}

type upsertChunkResult struct {
	added      int
	updated    int
	unchanged  int
	totalItems int64
	addedIDs   []string
	updatedIDs []string
	previous   []core.KindItem
	// held is how long the transaction held the writer.
	held time.Duration
}

// writeUpsertChunk writes one chunk of prepared items in a single
// transaction. Existing items whose content hash matches are not
// rewritten — no plugin_items index churn, no FTS replace, no field or
// art delete/reinsert — only their seen bookkeeping is refreshed, in one
// statement, and only when it would change. When captureUndo is true, the
// committed added/updated IDs are returned, and when snapshotPrevious is also
// true, pre-write copies are loaded only for items whose content hash changed.
func (s *Service) writeUpsertChunk(ctx context.Context, kind KindRecord, pluginID, kindKey string, chunk []preparedItem, seenAt, nowUnix int64, bumpGen, markOK, pin, captureUndo, snapshotPrevious bool) (upsertChunkResult, error) {
	// The kind lock serializes item content, search rows and facets for this
	// kind. Pin and seen bookkeeping can change independently and is merged
	// by the write statements.
	var plan *upsertChunkPlan
	var err error
	if store.DialectOf(s.db) == store.SQLite {
		plan, err = prepareUpsertChunk(ctx, s.db, kind, chunk, nowUnix, captureUndo, snapshotPrevious)
		if err != nil {
			return upsertChunkResult{}, err
		}
		if err := s.planUpsertFTS(ctx, kind.LibraryID, chunk, plan); err != nil {
			return upsertChunkResult{}, err
		}
		if err := prepareTallyRepairs(ctx, s.db, kind.LibraryID, kind.Decl, plan.fieldTally, plan.personTally); err != nil {
			return upsertChunkResult{}, err
		}
	}
	var res upsertChunkResult
	// The live cap callback reads plugin metadata through the pool and can
	// run its own writes. It must run before taking the SQLite writer.
	var maxItems int
	if plan == nil || plan.result.added > 0 {
		maxItems = s.MaxItemsFor(pluginID)
	}
	held, err := s.runWriteChunk(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := lockFieldPresence(ctx, tx, store.DialectOf(s.db) == store.Postgres, kind.LibraryID); err != nil {
			return err
		}
		if plan == nil {
			var err error
			plan, err = prepareUpsertChunk(ctx, tx, kind, chunk, nowUnix, captureUndo, snapshotPrevious)
			if err != nil {
				return err
			}
		}
		res = plan.result
		var err error
		res.totalItems, err = s.applyUpsertChunk(ctx, tx, kind, pluginID, kindKey, chunk, plan, maxItems, seenAt, nowUnix, bumpGen, markOK, pin)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrMaxItemsExceeded) {
			s.noteAttention(ctx, pluginID, fmt.Sprintf("plugin exceeded max item cap (%d)", maxItems))
		}
		return upsertChunkResult{}, err
	}
	res.held = held
	if res.added > 0 || res.updated > 0 {
		s.noteWrite()
	}
	return res, nil
}

type upsertChunkPlan struct {
	result      upsertChunkResult
	existing    map[core.ItemRef]existingItemMeta
	unchanged   []core.ItemRef
	changed     []bool
	fields      []itemFieldDiff
	fts         []upsertFTSChange
	fieldTally  map[fieldTallyKey]*fieldTallyChange
	personTally map[personTallyKey]*personTallyChange
	parentTally map[parentTallyKey]parentTallyDelta
}

func prepareUpsertChunk(ctx context.Context, q queryer, kind KindRecord, chunk []preparedItem, nowUnix int64, captureUndo, snapshotPrevious bool) (*upsertChunkPlan, error) {
	var res upsertChunkResult
	existing, err := lookupExistingItems(ctx, q, chunk)
	if err != nil {
		return nil, err
	}

	unchangedRefs := make([]core.ItemRef, 0, len(chunk))
	changed := make([]bool, len(chunk))
	for i := range chunk {
		ref := chunk[i].validated.Item.Ref
		ex, ok := existing[ref]
		switch {
		case !ok:
			changed[i] = true
			res.added++
			if captureUndo {
				res.addedIDs = append(res.addedIDs, chunk[i].validated.Item.ID)
			}
		case ex.hash != "" && ex.hash == chunk[i].hash:
			unchangedRefs = append(unchangedRefs, ref)
			res.unchanged++
		default:
			changed[i] = true
			res.updated++
			if captureUndo {
				res.updatedIDs = append(res.updatedIDs, chunk[i].validated.Item.ID)
			}
		}
	}

	changedExisting := make([]core.ItemRef, 0, len(chunk))
	for i := range chunk {
		if !changed[i] {
			continue
		}
		if _, ok := existing[chunk[i].validated.Item.Ref]; ok {
			changedExisting = append(changedExisting, chunk[i].validated.Item.Ref)
		}
	}
	var oldRowsByRef map[core.ItemRef][]existingFieldRow
	if len(changedExisting) > 0 {
		if captureUndo && snapshotPrevious {
			prev, err := queryItemsByRef(ctx, q, changedExisting)
			if err != nil {
				return nil, err
			}
			res.previous = prev
		}
		oldRowsByRef, err = lookupExistingFieldRows(ctx, q, changedExisting)
		if err != nil {
			return nil, err
		}
	}

	fields := make([]itemFieldDiff, len(chunk))
	fieldTallyDelta := map[fieldTallyKey]*fieldTallyChange{}
	personTallyDelta := map[personTallyKey]*personTallyChange{}
	for i := range chunk {
		if !changed[i] {
			continue
		}
		v := &chunk[i].validated
		it := &v.Item
		ex, isExisting := existing[it.Ref]
		addedAt := nowUnix
		if isExisting {
			addedAt = ex.addedAt
		}
		fields[i] = diffItemFields(oldRowsByRef[it.Ref], v.IndexedFields)
		var itemPersonHasThumb map[int64]bool
		for _, fv := range v.IndexedFields {
			if fv.HasThumb && core.IsPluginPersonID(fv.Int) {
				if itemPersonHasThumb == nil {
					itemPersonHasThumb = map[int64]bool{}
				}
				itemPersonHasThumb[fv.Int] = true
			}
		}

		refStr := string(it.Ref)
		newRank := tallyItemRank{ref: refStr, released: it.Released, addedAt: addedAt, year: int64(it.Year)}
		if !isExisting {
			for _, fv := range v.IndexedFields {
				if fv.Text == "" {
					continue
				}
				addFieldTallyChange(fieldTallyDelta, it.Level, fv.Field, fv.Text, 1, newRank, "")
				if core.IsPluginPersonID(fv.Int) {
					var th tallyItemRank
					if itemPersonHasThumb[fv.Int] {
						th = newRank
					}
					addPersonTallyChange(personTallyDelta, it.Level, fv.Field, fv.Int, 1, fv.Text, "", newRank, "", th, "", refStr)
				}
			}
		} else {
			oldRank := tallyItemRank{ref: refStr, released: ex.released, addedAt: ex.addedAt, year: ex.year}
			rankUp := newRank.beats(oldRank)
			rankDown := oldRank.beats(newRank)

			oldTextSet := make(map[fieldTallyKey]struct{}, len(oldRowsByRef[it.Ref]))
			oldPersonMap := map[personTallyKey]string{}
			for _, of := range oldRowsByRef[it.Ref] {
				if of.key.text == "" {
					continue
				}
				oldTextSet[fieldTallyKey{level: ex.level, field: of.key.field, text: of.key.text}] = struct{}{}
				if core.IsPluginPersonID(of.key.intV) {
					oldPersonMap[personTallyKey{level: ex.level, field: of.key.field, vInt: of.key.intV}] = of.key.text
				}
			}

			type newPersonVal struct {
				text     string
				hasThumb bool
			}
			newTextSet := make(map[fieldTallyKey]struct{}, len(v.IndexedFields))
			newPersonMap := map[personTallyKey]newPersonVal{}
			for _, fv := range v.IndexedFields {
				if fv.Text == "" {
					continue
				}
				newTextSet[fieldTallyKey{level: it.Level, field: fv.Field, text: fv.Text}] = struct{}{}
				if core.IsPluginPersonID(fv.Int) {
					newPersonMap[personTallyKey{level: it.Level, field: fv.Field, vInt: fv.Int}] = newPersonVal{
						text:     fv.Text,
						hasThumb: itemPersonHasThumb[fv.Int],
					}
				}
			}

			for k := range oldTextSet {
				if _, keep := newTextSet[k]; !keep {
					addFieldTallyChange(fieldTallyDelta, k.level, k.field, k.text, -1, tallyItemRank{}, refStr)
				}
			}
			for k := range newTextSet {
				if _, existed := oldTextSet[k]; !existed {
					addFieldTallyChange(fieldTallyDelta, k.level, k.field, k.text, 1, newRank, "")
				} else if rankUp {
					addFieldTallyChange(fieldTallyDelta, k.level, k.field, k.text, 0, newRank, "")
				} else if rankDown {
					addFieldTallyChange(fieldTallyDelta, k.level, k.field, k.text, 0, newRank, refStr)
				}
			}

			for k, oldText := range oldPersonMap {
				if _, keep := newPersonMap[k]; !keep {
					addPersonTallyChange(personTallyDelta, k.level, k.field, k.vInt, -1, "", oldText, tallyItemRank{}, refStr, tallyItemRank{}, refStr, refStr)
				}
			}
			for k, nv := range newPersonMap {
				oldText, existed := oldPersonMap[k]
				if !existed {
					var th tallyItemRank
					if nv.hasThumb {
						th = newRank
					}
					addPersonTallyChange(personTallyDelta, k.level, k.field, k.vInt, 1, nv.text, "", newRank, "", th, "", refStr)
					continue
				}
				var addTxt, remTxt string
				if oldText != nv.text {
					addTxt = nv.text
					remTxt = oldText
				}
				var addRep tallyItemRank
				var remRep string
				if rankUp || rankDown {
					addRep = newRank
				}
				if rankDown {
					remRep = refStr
				}
				var addTh tallyItemRank
				var remTh string
				if nv.hasThumb {
					addTh = newRank
					if rankDown {
						remTh = refStr
					}
				} else {
					remTh = refStr
				}
				addPersonTallyChange(personTallyDelta, k.level, k.field, k.vInt, 0, addTxt, remTxt, addRep, remRep, addTh, remTh, refStr)
			}
		}
	}
	var parentTally *parentTallyTracker
	if len(kind.Decl.Levels) >= 2 && kind.LibraryID > 0 {
		leafLevels := kind.Decl.LeafLevels()
		parentTally = newParentTallyTracker(q, kind.LibraryID, leafLevels, existing)
		primeParentRefs := make([]core.ItemRef, 0, len(chunk))
		primeLeafRefs := make([]core.ItemRef, 0, len(chunk))
		for i := range chunk {
			if !changed[i] {
				continue
			}
			p := &chunk[i]
			it := &p.validated.Item
			ex, isExisting := existing[it.Ref]
			newParent := core.ItemRef(p.parentRef)
			if isExisting && ex.parentRef == newParent && ex.level == it.Level {
				continue
			}
			if ex.parentRef != "" {
				primeParentRefs = append(primeParentRefs, ex.parentRef)
				primeLeafRefs = append(primeLeafRefs, ex.parentRef)
			}
			if newParent != "" {
				primeParentRefs = append(primeParentRefs, newParent)
				primeLeafRefs = append(primeLeafRefs, newParent)
			}
			if !leafLevels[it.Level] || (isExisting && !leafLevels[ex.level]) {
				primeLeafRefs = append(primeLeafRefs, it.Ref)
			}
		}
		if err := parentTally.primeParents(ctx, primeParentRefs); err != nil {
			return nil, err
		}
		if err := parentTally.primeLeaves(ctx, primeLeafRefs); err != nil {
			return nil, err
		}
		for i := range chunk {
			if !changed[i] {
				continue
			}
			p := &chunk[i]
			it := &p.validated.Item
			ex, isExisting := existing[it.Ref]
			if err := parentTally.apply(ctx, it.Ref, isExisting, ex.parentRef, core.ItemRef(p.parentRef), ex.level, it.Level); err != nil {
				return nil, fmt.Errorf("pluginitems: kind item %q: %w", it.ID, err)
			}
		}
	}

	var parentDelta map[parentTallyKey]parentTallyDelta
	if parentTally != nil {
		parentDelta = parentTally.delta
	}
	return &upsertChunkPlan{
		result: res, existing: existing, unchanged: unchangedRefs, changed: changed,
		fields: fields, fieldTally: fieldTallyDelta, personTally: personTallyDelta, parentTally: parentDelta,
	}, nil
}

// Distinguish this deadline from the caller's when deciding whether to split.
var errWriteChunkBudget = fmt.Errorf("%w", context.DeadlineExceeded)

// runWriteChunk starts the budget after writer admission. The transaction's
// context also covers SQLite's commit, including FTS flushes.
func (s *Service) runWriteChunk(ctx context.Context, write func(context.Context, *sql.Tx) error) (time.Duration, error) {
	workCtx := ctx
	var cancel context.CancelCauseFunc
	if store.DialectOf(s.db) == store.SQLite {
		workCtx, cancel = context.WithCancelCause(ctx)
		defer cancel(nil)
	}
	tx, err := s.db.BeginTx(workCtx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	began := time.Now()
	if cancel != nil {
		timer := time.AfterFunc(writeChunkBudget, func() { cancel(errWriteChunkBudget) })
		defer timer.Stop()
	}
	if err = write(workCtx, tx); err == nil {
		err = tx.Commit()
	}
	if err != nil && ctx.Err() == nil && errors.Is(context.Cause(workCtx), errWriteChunkBudget) {
		return 0, errWriteChunkBudget
	}
	return time.Since(began), err
}

// betweenWriteChunks hands the writer to queued foreground work, then checks
// WAL pressure without holding the writer.
func (s *Service) betweenWriteChunks(ctx context.Context) error {
	runtime.Gosched()
	return store.WaitWALBudget(ctx, s.db)
}

// writeChunks retries only transactions that rolled back on the local
// budget. At one item, give transient I/O one retry before stopping.
func (s *Service) writeChunks(ctx context.Context, count int, write func(start, end int) (time.Duration, error)) error {
	size := upsertChunkFirst
	retriedSingle := false
	for start := 0; start < count; {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+size, count)
		held, err := write(start, end)
		if errors.Is(err, errWriteChunkBudget) && end-start > 1 {
			size = (end - start) / 2
		} else if errors.Is(err, errWriteChunkBudget) && !retriedSingle {
			retriedSingle = true
		} else if err != nil {
			return err
		} else {
			size = nextUpsertChunkSize(end-start, held)
			retriedSingle = false
			start = end
		}
		if start < count {
			if err := s.betweenWriteChunks(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// planUpsertFTS reads stored search documents in one rowid lookup and
// keeps token writes out of updates whose indexed text is unchanged.
func (s *Service) planUpsertFTS(ctx context.Context, libraryID core.LibraryID, chunk []preparedItem, plan *upsertChunkPlan) error {
	var seqs []any
	for i, p := range chunk {
		if ex, ok := plan.existing[p.validated.Item.Ref]; ok && plan.changed[i] {
			seqs = append(seqs, ex.seq)
		}
	}
	old, err := s.lookupExistingSQLiteFTS(ctx, seqs)
	if err != nil {
		return err
	}
	plan.fts = make([]upsertFTSChange, len(chunk))
	for i, p := range chunk {
		if !plan.changed[i] {
			continue
		}
		v := p.validated
		want := existingFTSRow{
			itemRef: string(v.Item.Ref), libraryID: int64(libraryID),
			title: strings.TrimSpace(v.Item.Title), fields: strings.TrimSpace(v.SearchFields),
			people: strings.TrimSpace(v.SearchPeople),
		}
		cur, found := old[plan.existing[v.Item.Ref].seq]
		if found && cur == want {
			continue
		}
		plan.fts[i] = upsertFTSChange{remove: found}
		if want.title != "" || want.fields != "" || want.people != "" {
			plan.fts[i].insert = &want
		}
	}
	return nil
}

type upsertFTSChange struct {
	remove bool
	insert *existingFTSRow
}

func (s *Service) applyUpsertChunk(ctx context.Context, tx *sql.Tx, kind KindRecord, pluginID, kindKey string, chunk []preparedItem, plan *upsertChunkPlan, maxItems int, seenAt, nowUnix int64, bumpGen, markOK, pin bool) (int64, error) {
	existing, changed, unchangedRefs := plan.existing, plan.changed, plan.unchanged
	isPG := store.DialectOf(s.db) == store.Postgres
	var presenceBefore, presenceAfter []core.ItemRef
	for i, item := range chunk {
		ref := item.validated.Item.Ref
		old := existing[ref]
		if old.presenceCounted {
			if !changed[i] || (len(plan.fields[i].removed) == 0 && len(plan.fields[i].written) == 0 &&
				old.level == item.validated.Item.Level && (old.parentRef == "") == (item.parentRef == "")) {
				continue
			}
			presenceBefore = append(presenceBefore, ref)
		}
		presenceAfter = append(presenceAfter, ref)
	}
	presence := fieldPresenceDelta{isPG: isPG}
	if err := presence.collect(ctx, tx, presenceBefore, -1); err != nil {
		return 0, err
	}

	if plan.result.added > 0 {
		// The tally is maintained in the same transactions as the items,
		// so summing its per-level rows (bounded by kinds x levels)
		// replaces a COUNT(*) over the plugin's whole item table.
		var pluginTotal int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(n), 0) FROM plugin_item_tally WHERE library_id IN (SELECT library_id FROM plugin_kinds WHERE plugin_id = ?)`,
			pluginID).Scan(&pluginTotal); err != nil {
			return 0, err
		}
		if pluginTotal+int64(plan.result.added) > int64(maxItems) {
			return 0, ErrMaxItemsExceeded
		}
	}

	var nextSeq int64
	var restored map[core.ItemRef]rebuildKey
	if plan.result.added > 0 {
		var err error
		restored, err = takeRebuildItemKeys(ctx, tx, chunk, existing)
		if err != nil {
			return 0, err
		}
		if fresh := plan.result.added - len(restored); fresh > 0 {
			nextSeq, err = allocateSeqRangeTx(ctx, tx, int64(fresh))
			if err != nil {
				return 0, err
			}
		}
	}

	if len(unchangedRefs) > 0 {
		placeholders := make([]string, len(unchangedRefs))
		args := make([]any, 0, len(unchangedRefs)+2)
		args = append(args, seenAt)
		for i, r := range unchangedRefs {
			placeholders[i] = "?"
			args = append(args, string(r))
		}
		args = append(args, seenAt)
		updateUnchangedSQL := `UPDATE plugin_items SET seen_at = ?, missed_walks = 0 WHERE item_ref IN (` + strings.Join(placeholders, ",") + `) AND (missed_walks <> 0 OR seen_at <> ?)`
		if pin {
			updateUnchangedSQL = `UPDATE plugin_items SET seen_at = ?, missed_walks = 0, pinned = 1 WHERE item_ref IN (` + strings.Join(placeholders, ",") + `) AND (missed_walks <> 0 OR seen_at <> ? OR pinned = 0)`
		}
		if _, err := tx.ExecContext(ctx, updateUnchangedSQL, args...); err != nil {
			return 0, err
		}
	}

	pinInt := 0
	if pin {
		pinInt = 1
	}

	tallyDelta := map[string]int64{}
	recordingDelta := map[string]int64{}

	fieldConflictCols := `item_ref, field, v_text, v_int`
	if isPG {
		fieldConflictCols = `item_ref, field, md5(v_text), v_int`
	}

	for i := range chunk {
		if !changed[i] {
			continue
		}
		p := &chunk[i]
		v := &p.validated
		it := &v.Item
		ex, isExisting := existing[it.Ref]
		if ex.recording != it.Recording {
			recordingDelta[ex.recording]--
			recordingDelta[it.Recording]++
		}
		var seq int64
		var addedAt int64
		if isExisting {
			seq = ex.seq
			addedAt = ex.addedAt
			if ex.level != it.Level {
				tallyDelta[ex.level]--
				tallyDelta[it.Level]++
			}
		} else {
			if key, ok := restored[it.Ref]; ok {
				seq, addedAt = key.seq, key.addedAt
			} else {
				seq = nextSeq
				nextSeq++
				addedAt = nowUnix
			}
			tallyDelta[it.Level]++
		}
		it.Key = core.PluginKey(seq)

		if isPG {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_items(
	item_ref, seq, plugin_id, kind_key, library_id, external_id, parent_ref, level, idx,
	title, sort_title, summary, year, released, rating, rating_level, playable, child_count, leaf_count,
	unavailable, hints, fields, etag, added_at, updated_at, seen_at, missed_walks, pinned, content_hash, recording, search_tsv
) VALUES(
	?, ?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?,
	setweight(to_tsvector('simple', ?), 'A') || setweight(to_tsvector('simple', ?), 'B') || setweight(to_tsvector('simple', ?), 'C')
) ON CONFLICT(item_ref) DO UPDATE SET
	parent_ref   = excluded.parent_ref,
	level        = excluded.level,
	idx          = excluded.idx,
	title        = excluded.title,
	sort_title   = excluded.sort_title,
	summary      = excluded.summary,
	year         = excluded.year,
	released     = excluded.released,
	rating       = excluded.rating,
	rating_level = excluded.rating_level,
	playable     = excluded.playable,
	child_count  = excluded.child_count,
	leaf_count   = excluded.leaf_count,
	unavailable  = excluded.unavailable,
	hints        = excluded.hints,
	fields       = excluded.fields,
	etag         = excluded.etag,
	updated_at   = excluded.updated_at,
	seen_at      = excluded.seen_at,
	missed_walks = 0,
	pinned       = CASE WHEN plugin_items.pinned <> 0 OR excluded.pinned <> 0 THEN 1 ELSE 0 END,
	content_hash = excluded.content_hash,
	recording    = excluded.recording,
	search_tsv   = excluded.search_tsv`,
				string(it.Ref), seq, pluginID, kindKey, int64(kind.LibraryID), it.ID, p.parentRef, it.Level, it.Index,
				it.Title, it.SortTitle, it.Summary, it.Year, it.Released, it.Rating, p.ratingLevel, p.playableInt, it.ChildCount, it.LeafCount,
				p.unavailableStr, p.hintsStr, p.fieldsStr, it.ETag, addedAt, nowUnix, seenAt, pinInt, p.hash, it.Recording,
				it.Title, v.SearchFields, v.SearchPeople,
			); err != nil {
				return 0, fmt.Errorf("pluginitems: kind item %q: %w", it.ID, err)
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_items(
	item_ref, seq, plugin_id, kind_key, library_id, external_id, parent_ref, level, idx,
	title, sort_title, summary, year, released, rating, rating_level, playable, child_count, leaf_count,
	unavailable, hints, fields, etag, added_at, updated_at, seen_at, missed_walks, pinned, content_hash, recording
) VALUES(
	?, ?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?
) ON CONFLICT(item_ref) DO UPDATE SET
	parent_ref   = excluded.parent_ref,
	level        = excluded.level,
	idx          = excluded.idx,
	title        = excluded.title,
	sort_title   = excluded.sort_title,
	summary      = excluded.summary,
	year         = excluded.year,
	released     = excluded.released,
	rating       = excluded.rating,
	rating_level = excluded.rating_level,
	playable     = excluded.playable,
	child_count  = excluded.child_count,
	leaf_count   = excluded.leaf_count,
	unavailable  = excluded.unavailable,
	hints        = excluded.hints,
	fields       = excluded.fields,
	etag         = excluded.etag,
	updated_at   = excluded.updated_at,
	seen_at      = excluded.seen_at,
	missed_walks = 0,
	pinned       = CASE WHEN plugin_items.pinned <> 0 OR excluded.pinned <> 0 THEN 1 ELSE 0 END,
	content_hash = excluded.content_hash,
	recording    = excluded.recording`,
				string(it.Ref), seq, pluginID, kindKey, int64(kind.LibraryID), it.ID, p.parentRef, it.Level, it.Index,
				it.Title, it.SortTitle, it.Summary, it.Year, it.Released, it.Rating, p.ratingLevel, p.playableInt, it.ChildCount, it.LeafCount,
				p.unavailableStr, p.hintsStr, p.fieldsStr, it.ETag, addedAt, nowUnix, seenAt, pinInt, p.hash, it.Recording,
			); err != nil {
				return 0, fmt.Errorf("pluginitems: kind item %q: %w", it.ID, err)
			}
			fts := plan.fts[i]
			if fts.remove {
				if err := sqliteDeleteFTS(ctx, tx, seq); err != nil {
					return 0, fmt.Errorf("pluginitems: kind item %q search: %w", it.ID, err)
				}
			}
			if doc := fts.insert; doc != nil {
				if err := sqliteInsertFTS(ctx, tx, seq, it.Ref, kind.LibraryID, doc.title, doc.fields, doc.people); err != nil {
					return 0, fmt.Errorf("pluginitems: kind item %q search: %w", it.ID, err)
				}
			}
		}

		// Replace indexed fields for this item. An existing item's rows are
		// diffed against what is stored: a typical republish changes few or
		// no indexed values, and rewriting every row churned five indexes
		// per field inside the write transaction.
		if err := writeItemFieldsTx(ctx, tx, isPG, it.ID, it.Ref, plan.fields[i], fieldConflictCols); err != nil {
			return 0, err
		}

		if lv, _, ok := kind.Decl.Level(it.Level); ok && lv.Members {
			if err := writeMembersTx(ctx, tx, isPG, it.Ref, len(it.Members), p.membersJSON); err != nil {
				return 0, fmt.Errorf("pluginitems: kind item %q members: %w", it.ID, err)
			}
		}

		// Replace artwork rows for this item.
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_art WHERE item_ref = ?`, string(it.Ref)); err != nil {
			return 0, fmt.Errorf("pluginitems: kind item %q: %w", it.ID, err)
		}
		for _, a := range it.Art {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO plugin_item_art(item_ref, role, url, width, height) VALUES(?, ?, ?, ?, ?)`,
				string(it.Ref), a.Role, a.URL, a.Width, a.Height); err != nil {
				return 0, fmt.Errorf("pluginitems: kind item %q art %q: %w", it.ID, a.Role, err)
			}
		}
	}

	if err := presence.finish(ctx, tx, presenceAfter); err != nil {
		return 0, err
	}
	if err := applyRecordingTallyDeltaTx(ctx, tx, kind.LibraryID, recordingDelta); err != nil {
		return 0, err
	}
	if err := applyTallyDeltaTx(ctx, tx, kind.LibraryID, tallyDelta); err != nil {
		return 0, err
	}
	emptiedText, err := applyFieldTallyDeltaTx(ctx, tx, isPG, kind.LibraryID, plan.fieldTally)
	if err != nil {
		return 0, err
	}
	if err := applyPersonTallyDeltaTx(ctx, tx, isPG, kind.LibraryID, kind.Decl, plan.personTally, emptiedText); err != nil {
		return 0, err
	}
	if err := applyParentTallyDeltaTx(ctx, tx, kind.LibraryID, plan.parentTally); err != nil {
		return 0, err
	}
	if bumpGen {
		total, err := s.bumpKindSyncStateTx(ctx, tx, pluginID, kindKey, kind.LibraryID, nowUnix, markOK)
		return total, err
	}
	return 0, nil
}

// queryItemsByRef loads full KindItems (including art) for refs ordered to match refs. It closes the plugin_items cursor before querying
// plugin_item_art so PostgreSQL pgx connections never see two open cursors on
// the same transaction.
func queryItemsByRef(ctx context.Context, tx queryer, refs []core.ItemRef) ([]core.KindItem, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(refs))
	args := make([]any, len(refs))
	for i, r := range refs {
		placeholders[i] = "?"
		args[i] = string(r)
	}
	inClause := strings.Join(placeholders, ",")
	rows, err := tx.QueryContext(ctx, `SELECT `+itemCols+` FROM plugin_items WHERE item_ref IN (`+inClause+`)`, args...)
	if err != nil {
		return nil, err
	}
	items := make([]core.KindItem, 0, len(refs))
	for rows.Next() {
		it, _, _, err := scanItemRow(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	byRefIdx := make(map[core.ItemRef][]int, len(items))
	for i := range items {
		byRefIdx[items[i].Ref] = append(byRefIdx[items[i].Ref], i)
	}
	artRows, err := tx.QueryContext(ctx,
		`SELECT item_ref, role, url, width, height FROM plugin_item_art WHERE item_ref IN (`+inClause+`) ORDER BY item_ref, role`,
		args...)
	if err != nil {
		return nil, err
	}
	defer artRows.Close()
	for artRows.Next() {
		var refStr, role, url string
		var w, h int64
		if err := artRows.Scan(&refStr, &role, &url, &w, &h); err != nil {
			return nil, err
		}
		art := core.KindArtRef{Role: role, URL: url, Width: int(w), Height: int(h)}
		for _, idx := range byRefIdx[core.ItemRef(refStr)] {
			items[idx].Art = append(items[idx].Art, art)
		}
	}
	if err := artRows.Err(); err != nil {
		return nil, err
	}
	byRef := make(map[core.ItemRef]core.KindItem, len(items))
	for _, it := range items {
		byRef[it.Ref] = it
	}
	ordered := make([]core.KindItem, 0, len(items))
	for _, ref := range refs {
		if it, ok := byRef[ref]; ok {
			ordered = append(ordered, it)
		}
	}
	return ordered, nil
}

// queryer is a read handle: the pool, or a transaction.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type fieldRowKey struct {
	field string
	text  string
	intV  int64
}

// existingFieldRow is one stored plugin_item_fields row of an item being
// rewritten.
type existingFieldRow struct {
	key  fieldRowKey
	real float64
	fold string
}

// lookupExistingFieldRows loads every stored indexed field row for refs,
// grouped by item, in primary-key order.
func lookupExistingFieldRows(ctx context.Context, tx queryer, refs []core.ItemRef) (map[core.ItemRef][]existingFieldRow, error) {
	out := make(map[core.ItemRef][]existingFieldRow, len(refs))
	const chunkSize = 250
	for start := 0; start < len(refs); start += chunkSize {
		end := min(start+chunkSize, len(refs))
		chunk := refs[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, r := range chunk {
			placeholders[i] = "?"
			args[i] = string(r)
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT item_ref, field, v_text, v_int, v_real, v_fold FROM plugin_item_fields WHERE item_ref IN (`+strings.Join(placeholders, ",")+`) ORDER BY item_ref, field, v_text, v_int`,
			args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var refStr string
			var r existingFieldRow
			if err := rows.Scan(&refStr, &r.key.field, &r.key.text, &r.key.intV, &r.real, &r.fold); err != nil {
				rows.Close()
				return nil, err
			}
			out[core.ItemRef(refStr)] = append(out[core.ItemRef(refStr)], r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

type itemFieldDiff struct {
	removed []fieldRowKey
	written []IndexedFieldValue
}

// diffItemFields keeps unchanged rows out of the write, including their
// secondary indexes. Last occurrence wins for duplicate keys.
func diffItemFields(old []existingFieldRow, fields []IndexedFieldValue) itemFieldDiff {
	var diff itemFieldDiff
	last := make(map[fieldRowKey]int, len(fields))
	for i, fv := range fields {
		last[fieldRowKey{field: fv.Field, text: fv.Text, intV: fv.Int}] = i
	}
	stored := make(map[fieldRowKey]existingFieldRow, len(old))
	for _, r := range old {
		stored[r.key] = r
		if _, keep := last[r.key]; !keep {
			diff.removed = append(diff.removed, r.key)
		}
	}
	for i, fv := range fields {
		k := fieldRowKey{field: fv.Field, text: fv.Text, intV: fv.Int}
		if last[k] != i {
			continue
		}
		if r, ok := stored[k]; ok && r.real == fv.Real && r.fold == fv.Fold {
			continue
		}
		diff.written = append(diff.written, fv)
	}
	return diff
}

func writeItemFieldsTx(ctx context.Context, tx *sql.Tx, isPG bool, id string, ref core.ItemRef, diff itemFieldDiff, conflictCols string) error {
	refStr := string(ref)
	for _, k := range diff.removed {
		var err error
		if isPG {
			_, err = tx.ExecContext(ctx, `DELETE FROM plugin_item_fields WHERE item_ref = ? AND field = ? AND md5(v_text) = ? AND v_text = ? AND v_int = ?`,
				refStr, k.field, pgTextMD5(k.text), k.text, k.intV)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM plugin_item_fields WHERE item_ref = ? AND field = ? AND v_text = ? AND v_int = ?`,
				refStr, k.field, k.text, k.intV)
		}
		if err != nil {
			return fmt.Errorf("pluginitems: kind item %q field %q: %w", id, k.field, err)
		}
	}
	for _, fv := range diff.written {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO plugin_item_fields(item_ref, field, v_text, v_int, v_real, v_fold) VALUES(?, ?, ?, ?, ?, ?)
			 ON CONFLICT(`+conflictCols+`) DO UPDATE SET v_real = excluded.v_real, v_fold = excluded.v_fold`,
			refStr, fv.Field, fv.Text, fv.Int, fv.Real, fv.Fold); err != nil {
			return fmt.Errorf("pluginitems: kind item %q field %q: %w", id, fv.Field, err)
		}
	}
	return nil
}
