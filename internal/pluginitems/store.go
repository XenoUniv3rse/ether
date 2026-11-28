package pluginitems

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/store"
)

// KindRecord is one installed (plugin, kind) row in plugin_kinds (spec §2.3).
type KindRecord struct {
	PluginID       string
	KindKey        string
	LibraryID      core.LibraryID
	Decl           plugins.MediaKindMeta
	DeclHash       string
	SchemaVersion  int64
	rebuildPending bool
}

// SyncOutcome summarizes the most recent completed sync or keep action in memory.
type SyncOutcome struct {
	Mode       string    `json:"mode"` // "sync", "full", "force", "keep"
	Upserted   int       `json:"upserted,omitempty"`
	Deleted    int       `json:"deleted,omitempty"`
	Skipped    int       `json:"skipped,omitempty"`
	Protected  int       `json:"protected,omitempty"`
	Total      int       `json:"total"`
	Summary    string    `json:"summary"`
	FinishedAt time.Time `json:"finishedAt"`
}

// SyncState is the persisted sync status for one (plugin, kind) (spec §2.3),
// enriched with live in-memory progress while a background sync or reindex pass
// is running.
type SyncState struct {
	PluginID    string                `json:"pluginId"`
	KindKey     string                `json:"kindKey"`
	Cursor      string                `json:"cursor,omitempty"`
	Generation  uint64                `json:"generation"`
	LastStarted time.Time             `json:"lastStarted,omitzero"`
	LastOK      time.Time             `json:"lastOk,omitzero"`
	Failure     *plugins.RouteFailure `json:"failure,omitempty"`
	LastError   string                `json:"lastError,omitempty"`
	Items       int64                 `json:"items"`
	Running     bool                  `json:"running,omitempty"`
	Phase       string                `json:"phase,omitempty"`
	SyncedItems int                   `json:"syncedItems,omitempty"`
	Pages       int                   `json:"pages,omitempty"`
	DeleteGuard *SyncDeleteGuard      `json:"deleteGuard,omitempty"`
	LastOutcome *SyncOutcome          `json:"lastOutcome,omitempty"`
}

const itemCols = `item_ref, seq, plugin_id, kind_key, library_id, external_id, parent_ref, level, idx, title, sort_title, summary, year, released, rating, playable, child_count, leaf_count, hints, fields, etag, unavailable, added_at, recording`

func prefixedItemCols(prefix string) string {
	parts := strings.Split(itemCols, ", ")
	for i, p := range parts {
		parts[i] = prefix + p
	}
	return strings.Join(parts, ", ")
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanItemRow(sc rowScanner) (core.KindItem, core.LibraryID, int64, error) {
	return scanItemRowExtra(sc)
}

func scanItemRowExtra(sc rowScanner, extra ...any) (core.KindItem, core.LibraryID, int64, error) {
	var (
		it             core.KindItem
		refStr         string
		seq            int64
		pluginID       string
		kindKey        string
		libID          int64
		parentRef      string
		idx            int64
		year           int64
		playable       int
		childCount     int64
		leafCount      int64
		hintsRaw       string
		fieldsRaw      string
		unavailableRaw string
	)
	dest := make([]any, 0, 24+len(extra))
	dest = append(dest,
		&refStr, &seq, &pluginID, &kindKey, &libID, &it.ID, &parentRef, &it.Level, &idx,
		&it.Title, &it.SortTitle, &it.Summary, &year, &it.Released, &it.Rating,
		&playable, &childCount, &leafCount, &hintsRaw, &fieldsRaw, &it.ETag, &unavailableRaw, &it.AddedAt, &it.Recording,
	)
	dest = append(dest, extra...)
	if err := sc.Scan(dest...); err != nil {
		return core.KindItem{}, 0, 0, err
	}
	it.Ref = core.ItemRef(refStr)
	it.Key = core.PluginKey(seq)
	if parentRef != "" {
		it.ParentID = core.ItemRef(parentRef).External()
	}
	it.Index = int(idx)
	it.Year = int(year)
	if unavailableRaw != "" {
		if err := json.Unmarshal([]byte(unavailableRaw), &it.Unavailable); err != nil {
			return core.KindItem{}, 0, 0, err
		}
	}
	it.Playable = playable != 0 && it.Unavailable == nil
	it.ChildCount = int(childCount)
	it.LeafCount = int(leafCount)
	if hintsRaw != "" && hintsRaw != "{}" {
		it.Hints = json.RawMessage(hintsRaw)
	}
	if fieldsRaw != "" && fieldsRaw != "{}" {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(fieldsRaw), &m); err == nil && len(m) > 0 {
			it.Fields = m
		}
	}
	return it, core.LibraryID(libID), seq, nil
}

func (s *Service) queryItems(ctx context.Context, query string, args []any, hydrateArt bool) ([]core.KindItem, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []core.KindItem
	for rows.Next() {
		it, _, _, err := scanItemRow(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if hydrateArt && len(items) > 0 {
		if err := s.hydrateArtSlice(ctx, items); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func (s *Service) hydrateArtSlice(ctx context.Context, items []core.KindItem) error {
	if len(items) == 0 {
		return nil
	}
	byRef := make(map[core.ItemRef][]int, len(items))
	placeholders := make([]string, 0, len(items))
	args := make([]any, 0, len(items))
	for i := range items {
		ref := items[i].Ref
		if _, seen := byRef[ref]; !seen {
			placeholders = append(placeholders, "?")
			args = append(args, string(ref))
		}
		byRef[ref] = append(byRef[ref], i)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT item_ref, role, url, width, height FROM plugin_item_art WHERE item_ref IN (`+strings.Join(placeholders, ",")+`) ORDER BY item_ref, role`,
		args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var refStr, role, url string
		var w, h int64
		if err := rows.Scan(&refStr, &role, &url, &w, &h); err != nil {
			return err
		}
		art := core.KindArtRef{Role: role, URL: url, Width: int(w), Height: int(h)}
		for _, idx := range byRef[core.ItemRef(refStr)] {
			items[idx].Art = append(items[idx].Art, art)
		}
	}
	return rows.Err()
}

// UpsertKind installs or upgrades one (plugin, kind) declaration in
// plugin_kinds (spec §2.3, §2.5). On upgrade, decl_hash is compared against
// the stored declaration: additive changes bump schema_version; non-additive
// changes are refused with a ValidationError unless replaceContents is true,
// in which case the next sync rebuilds that kind.
func (s *Service) UpsertKind(ctx context.Context, pluginID string, libraryID core.LibraryID, decl plugins.MediaKindMeta, replaceContents bool) (KindRecord, error) {
	if strings.TrimSpace(pluginID) == "" || strings.TrimSpace(decl.Key) == "" || libraryID <= 0 {
		return KindRecord{}, errors.New("pluginitems: pluginID, kindKey and libraryID are required")
	}
	mu := s.kindLock(pluginID, decl.Key)
	mu.Lock()
	defer mu.Unlock()

	return s.upsertKindLocked(ctx, pluginID, libraryID, decl, replaceContents, false)
}

func (s *Service) upsertKindLocked(ctx context.Context, pluginID string, libraryID core.LibraryID, decl plugins.MediaKindMeta, replaceContents, rebuilt bool) (KindRecord, error) {
	declJSON, err := json.Marshal(decl)
	if err != nil {
		return KindRecord{}, err
	}
	declHash := decl.DeclHash()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return KindRecord{}, err
	}
	defer tx.Rollback()

	var (
		oldLibID        int64
		oldDeclJSON     string
		oldHash         string
		pending         string
		schemaVer       int64
		scheduleReindex bool
	)
	kindQuery := `SELECT library_id, decl, decl_hash, schema_version FROM plugin_kinds WHERE plugin_id = ? AND kind_key = ?`
	if store.DialectOf(s.db) == store.Postgres {
		kindQuery += " FOR UPDATE"
	}
	err = tx.QueryRowContext(ctx, kindQuery, pluginID, decl.Key).Scan(&oldLibID, &oldDeclJSON, &oldHash, &schemaVer)
	if err == nil {
		// Read consent after locking the registration so PostgreSQL sees
		// the latest queue entry even if it had to wait for another node.
		pending, err = readKindRebuild(ctx, tx, pluginID, decl.Key)
	}
	if rebuilt {
		if err != nil {
			return KindRecord{}, err
		}
		if pending != string(declJSON) {
			return KindRecord{}, ErrKindRebuildPending
		}
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		schemaVer = 1
		var hasPreExisting bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM plugin_items WHERE plugin_id = ? AND kind_key = ?)`,
			pluginID, decl.Key).Scan(&hasPreExisting); err != nil {
			return KindRecord{}, err
		}
		ftsVer := currentFTSVersion
		if hasPreExisting {
			// Defer reindexing existing rows to a chunked background worker
			// rather than holding the SQLite write lock across the install request.
			ftsVer = 0
			scheduleReindex = true
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO plugin_kinds(plugin_id, kind_key, library_id, decl, decl_hash, schema_version, fts_version) VALUES(?, ?, ?, ?, ?, ?, ?)`,
			pluginID, decl.Key, int64(libraryID), string(declJSON), declHash, schemaVer, ftsVer); err != nil {
			return KindRecord{}, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO plugin_sync_state(plugin_id, kind_key) VALUES(?, ?) ON CONFLICT(plugin_id, kind_key) DO NOTHING`,
			pluginID, decl.Key); err != nil {
			return KindRecord{}, err
		}
	case err != nil:
		return KindRecord{}, err
	default:
		if pending != "" && !rebuilt {
			if pending != string(declJSON) {
				var queued plugins.MediaKindMeta
				if err := json.Unmarshal([]byte(pending), &queued); err != nil {
					return KindRecord{}, err
				}
				// An additive follow-up can use the rebuild already scheduled
				// for this kind. A new non-additive shape needs new consent.
				if err := plugins.ValidateMediaKindsUpgrade([]plugins.MediaKindMeta{queued}, []plugins.MediaKindMeta{decl}); err != nil && !replaceContents {
					return KindRecord{}, ErrKindRebuildPending
				}
				if err := queueKindRebuild(ctx, tx, pluginID, decl.Key, string(declJSON)); err != nil {
					return KindRecord{}, err
				}
			}
			s.invalidateKindCache(pluginID, decl.Key, core.LibraryID(oldLibID))
			return kindRecordFromDecl(pluginID, decl.Key, oldLibID, oldDeclJSON, oldHash, schemaVer)
		}
		needReindex := false
		needParentTallyRebuild := false
		if oldHash != declHash {
			var oldDecl plugins.MediaKindMeta
			if err := json.Unmarshal([]byte(oldDeclJSON), &oldDecl); err != nil {
				return KindRecord{}, err
			}
			if upgErr := plugins.ValidateMediaKindsUpgrade([]plugins.MediaKindMeta{oldDecl}, []plugins.MediaKindMeta{decl}); upgErr != nil {
				if !rebuilt {
					if !replaceContents {
						return KindRecord{}, upgErr
					}
					if err := queueKindRebuild(ctx, tx, pluginID, decl.Key, string(declJSON)); err != nil {
						return KindRecord{}, err
					}
					s.invalidateKindCache(pluginID, decl.Key, core.LibraryID(oldLibID))
					return kindRecordFromDecl(pluginID, decl.Key, oldLibID, oldDeclJSON, oldHash, schemaVer)
				}
			} else {
				// Only the declared fields and search fields shape the
				// per-item index. A change to tabs, shelves, card shape or
				// labels must not rewrite every item under the writer lock.
				needReindex = indexShapeChanged(oldDecl, decl)
				needParentTallyRebuild = parentTallyShapeChanged(oldDecl, decl)
			}
			schemaVer++
		}
		ftsVer := currentFTSVersion
		if needReindex {
			var existingItems int64
			_ = tx.QueryRowContext(ctx,
				`SELECT COALESCE(SUM(n), 0) FROM plugin_item_tally WHERE library_id = ?`,
				int64(libraryID)).Scan(&existingItems)
			if existingItems > 0 {
				ftsVer = 0
				scheduleReindex = true
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE plugin_kinds SET library_id = ?, decl = ?, decl_hash = ?, schema_version = ?, fts_version = ? WHERE plugin_id = ? AND kind_key = ?`,
			int64(libraryID), string(declJSON), declHash, schemaVer, ftsVer, pluginID, decl.Key); err != nil {
			return KindRecord{}, err
		}
		if needParentTallyRebuild && !rebuilt && !scheduleReindex && hasTableTx(ctx, tx, store.DialectOf(s.db), "plugin_item_parent_tally") {
			if err := rebuildKindParentTallyForKindTx(ctx, tx, pluginID, decl.Key, libraryID, decl); err != nil {
				return KindRecord{}, err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO plugin_sync_state(plugin_id, kind_key) VALUES(?, ?) ON CONFLICT(plugin_id, kind_key) DO NOTHING`,
			pluginID, decl.Key); err != nil {
			return KindRecord{}, err
		}
		if rebuilt {
			if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_kind_rebuilds WHERE plugin_id = ? AND kind_key = ?`, pluginID, decl.Key); err != nil {
				return KindRecord{}, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE plugin_sync_state SET cursor = '', last_started = 0, last_ok = 0, last_error = '', failure_json = '', guard_json = '', items = 0, generation = ? WHERE plugin_id = ? AND kind_key = ?`, s.syncGen.Add(1), pluginID, decl.Key); err != nil {
				return KindRecord{}, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return KindRecord{}, err
	}
	rec := KindRecord{
		PluginID:      pluginID,
		KindKey:       decl.Key,
		LibraryID:     libraryID,
		Decl:          decl,
		DeclHash:      declHash,
		SchemaVersion: schemaVer,
	}
	if oldLibID > 0 && core.LibraryID(oldLibID) != libraryID {
		s.invalidateKindCache(pluginID, decl.Key, core.LibraryID(oldLibID))
	}
	s.cacheKindRecord(rec)
	if rebuilt {
		s.partsState.Store(0)
		s.noteWrite()
		s.forgetLibraryStats(libraryID)
	}
	if scheduleReindex {
		s.startKindReindex(pluginID, decl.Key, libraryID, decl)
	} else if !rebuilt && s.kindHasUnindexedArtistStrings(ctx, libraryID, decl) {
		s.startKindReindex(pluginID, decl.Key, libraryID, decl)
	}
	return rec, nil
}

// indexShapeChanged reports whether the parts of a kind declaration that
// extractIndexData reads (declared fields and search fields) differ.
func indexShapeChanged(oldDecl, newDecl plugins.MediaKindMeta) bool {
	o, err1 := json.Marshal(struct {
		F any
		S any
	}{oldDecl.Fields, oldDecl.Browse.SearchFields})
	n, err2 := json.Marshal(struct {
		F any
		S any
	}{newDecl.Fields, newDecl.Browse.SearchFields})
	return err1 != nil || err2 != nil || string(o) != string(n)
}

// parentTallyShapeChanged reports whether the declared level tree (level keys
// and effective parent edges) changed in a way that affects
// plugin_item_parent_tally (e.g. adding a branching level or upgrading from a
// single-level kind to a multi-level hierarchy).
func parentTallyShapeChanged(oldDecl, newDecl plugins.MediaKindMeta) bool {
	if len(newDecl.Levels) < 2 && len(oldDecl.Levels) < 2 {
		return false
	}
	if len(oldDecl.Levels) != len(newDecl.Levels) {
		return true
	}
	for i := range oldDecl.Levels {
		ok := oldDecl.Levels[i].Key
		nk := newDecl.Levels[i].Key
		if ok != nk || oldDecl.LevelParent(ok) != newDecl.LevelParent(nk) {
			return true
		}
	}
	return false
}

func kindCacheKey(pluginID, kindKey string) string {
	return pluginID + "\x00" + kindKey
}

func (s *Service) checkRestoreCaches() {
	gen := pluginItemsRestoreGen.Load()
	s.kindCacheMu.RLock()
	cur := s.kindCacheGen
	s.kindCacheMu.RUnlock()
	if cur == gen {
		return
	}
	s.kindCacheMu.Lock()
	if s.kindCacheGen != gen {
		s.kindCacheGen = gen
		clear(s.kindByLib)
		clear(s.kindByKey)
		s.partsState.Store(0)
	}
	s.kindCacheMu.Unlock()
}

func (s *Service) cacheKindRecord(rec KindRecord) {
	if rec.rebuildPending {
		return
	}
	s.checkRestoreCaches()
	s.kindCacheMu.Lock()
	if s.kindByLib == nil {
		s.kindByLib = map[core.LibraryID]KindRecord{}
	}
	if s.kindByKey == nil {
		s.kindByKey = map[string]KindRecord{}
	}
	if rec.LibraryID > 0 {
		s.kindByLib[rec.LibraryID] = rec
	}
	s.kindByKey[kindCacheKey(rec.PluginID, rec.KindKey)] = rec
	s.kindCacheMu.Unlock()
}

func (s *Service) invalidateKindCache(pluginID, kindKey string, libraryID core.LibraryID) {
	s.kindCacheMu.Lock()
	if libraryID > 0 {
		delete(s.kindByLib, libraryID)
	}
	if pluginID != "" && kindKey != "" {
		k := kindCacheKey(pluginID, kindKey)
		if prev, ok := s.kindByKey[k]; ok && prev.LibraryID > 0 {
			delete(s.kindByLib, prev.LibraryID)
		}
		delete(s.kindByKey, k)
	}
	s.kindCacheMu.Unlock()
}

func (s *Service) invalidatePluginKindCache(pluginID string) {
	s.kindCacheMu.Lock()
	for k, rec := range s.kindByKey {
		if rec.PluginID == pluginID {
			if rec.LibraryID > 0 {
				delete(s.kindByLib, rec.LibraryID)
			}
			delete(s.kindByKey, k)
		}
	}
	for libID, rec := range s.kindByLib {
		if rec.PluginID == pluginID {
			delete(s.kindByLib, libID)
		}
	}
	s.kindCacheMu.Unlock()
}

const kindRecordSelect = `SELECT plugin_id, kind_key, library_id, decl, decl_hash, schema_version,
	EXISTS(SELECT 1 FROM plugin_kind_rebuilds r WHERE r.plugin_id = k.plugin_id AND r.kind_key = k.kind_key)
	FROM plugin_kinds k`

// GetKind returns the installed KindRecord for (pluginID, kindKey).
func (s *Service) GetKind(ctx context.Context, pluginID, kindKey string) (KindRecord, error) {
	s.checkRestoreCaches()
	k := kindCacheKey(pluginID, kindKey)
	s.kindCacheMu.RLock()
	rec, ok := s.kindByKey[k]
	s.kindCacheMu.RUnlock()
	if ok {
		return rec, nil
	}
	rec, err := s.scanKind(s.db.QueryRowContext(ctx,
		kindRecordSelect+` WHERE plugin_id = ? AND kind_key = ?`,
		pluginID, kindKey))
	if err != nil {
		return KindRecord{}, err
	}
	s.cacheKindRecord(rec)
	return rec, nil
}

// GetKindByLibrary returns the installed KindRecord for libraryID.
func (s *Service) GetKindByLibrary(ctx context.Context, libraryID core.LibraryID) (KindRecord, error) {
	s.checkRestoreCaches()
	s.kindCacheMu.RLock()
	rec, ok := s.kindByLib[libraryID]
	s.kindCacheMu.RUnlock()
	if ok {
		return rec, nil
	}
	rec, err := s.scanKind(s.db.QueryRowContext(ctx,
		kindRecordSelect+` WHERE library_id = ?`,
		int64(libraryID)))
	if err != nil {
		return KindRecord{}, err
	}
	s.cacheKindRecord(rec)
	return rec, nil
}

// ListKinds returns all installed kinds for pluginID (or all plugins when pluginID == "").
func (s *Service) ListKinds(ctx context.Context, pluginID string) ([]KindRecord, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if pluginID == "" {
		rows, err = s.db.QueryContext(ctx,
			kindRecordSelect+` ORDER BY plugin_id, kind_key`)
	} else {
		rows, err = s.db.QueryContext(ctx,
			kindRecordSelect+` WHERE plugin_id = ? ORDER BY kind_key`,
			pluginID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KindRecord
	for rows.Next() {
		rec, err := s.scanKind(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Service) scanKind(sc rowScanner) (KindRecord, error) {
	var (
		rec      KindRecord
		libID    int64
		declJSON string
	)
	if err := sc.Scan(&rec.PluginID, &rec.KindKey, &libID, &declJSON, &rec.DeclHash, &rec.SchemaVersion, &rec.rebuildPending); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return KindRecord{}, core.ErrNotFound
		}
		return KindRecord{}, err
	}
	rec.LibraryID = core.LibraryID(libID)
	if err := json.Unmarshal([]byte(declJSON), &rec.Decl); err != nil {
		return KindRecord{}, err
	}
	return rec, nil
}

// KindUpsert implements the store side of plugins.KindBackends.KindUpsert
// (spec §3.6): validates and upserts items into the kind's library, updates
// tallies and FTS, and bumps the sync generation.
func (s *Service) KindUpsert(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (int, error) {
	return s.UpsertItems(ctx, pluginID, kindKey, items)
}

// KindUpsertWithStats is KindUpsert reporting what the batch did, for the
// host wire reply (#646).
func (s *Service) KindUpsertWithStats(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (plugins.KindUpsertResult, error) {
	stats, err := s.UpsertItemsWithStats(ctx, pluginID, kindKey, items)
	return plugins.KindUpsertResult{
		Accepted: stats.Accepted, Added: stats.Added,
		Updated: stats.Updated, Unchanged: stats.Unchanged,
	}, err
}

// KindUpsertWithUndo is KindUpsertWithStats plus the committed undo journal
// payload (#766): newly added IDs, content-changed existing IDs, and (only
// when snapshotPrevious is true) pre-write copies of modified items. Unchanged
// items with matching content hashes are never snapshotted.
func (s *Service) KindUpsertWithUndo(ctx context.Context, pluginID, kindKey string, items []core.KindItem, snapshotPrevious bool) (plugins.KindUpsertUndo, error) {
	mu := s.kindLock(pluginID, kindKey)
	mu.Lock()
	defer mu.Unlock()
	stats, undo, err := s.upsertItemsWithUndoLocked(ctx, pluginID, kindKey, items, s.now().Unix(), true, true, true, false, true, snapshotPrevious)
	return plugins.KindUpsertUndo{
		Result: plugins.KindUpsertResult{
			Accepted: stats.Accepted, Added: stats.Added,
			Updated: stats.Updated, Unchanged: stats.Unchanged,
		},
		AddedIDs:   undo.addedIDs,
		UpdatedIDs: undo.updatedIDs,
		Previous:   undo.previous,
	}, err
}

// UpsertItems validates and writes a batch of KindItems for (pluginID,
// kindKey), maintaining tallies, typed EAV fields, art and FTS. The batch
// commits in bounded chunks under the per-kind lock (#646).
func (s *Service) UpsertItems(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (int, error) {
	accepted, _, err := s.UpsertItemsWithAdded(ctx, pluginID, kindKey, items)
	return accepted, err
}

// UpsertItemsWithAdded validates and writes a batch of KindItems for
// (pluginID, kindKey), returning both accepted items and newly inserted items.
func (s *Service) UpsertItemsWithAdded(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (int, int, error) {
	stats, err := s.UpsertItemsWithStats(ctx, pluginID, kindKey, items)
	return stats.Accepted, stats.Added, err
}

// UpsertItemsWithStats is UpsertItemsWithAdded plus the updated/unchanged
// split: unchanged items are detected by content hash and not rewritten.
func (s *Service) UpsertItemsWithStats(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (UpsertStats, error) {
	mu := s.kindLock(pluginID, kindKey)
	mu.Lock()
	defer mu.Unlock()
	return s.upsertItemsWithStatsLocked(ctx, pluginID, kindKey, items, s.now().Unix(), true, true, true, false)
}

// UpsertPinnedItems writes items discovered on demand (such as live search
// results) and pins them so missed-walk pruning does not drop them when a
// subsequent /browse walk omits them.
func (s *Service) UpsertPinnedItems(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (int, error) {
	accepted, _, err := s.UpsertPinnedItemsWithAdded(ctx, pluginID, kindKey, items)
	return accepted, err
}

// UpsertPinnedItemsWithAdded writes items discovered on demand (live search or
// facet/container expansion), pinning them against missed-walk pruning while
// returning accepted and newly inserted counts.
func (s *Service) UpsertPinnedItemsWithAdded(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (int, int, error) {
	mu := s.kindLock(pluginID, kindKey)
	mu.Lock()
	defer mu.Unlock()
	stats, err := s.upsertItemsWithStatsLocked(ctx, pluginID, kindKey, items, s.now().Unix(), true, true, true, true)
	return stats.Accepted, stats.Added, err
}

// upsertItemsForSync writes a batch as part of a pull SyncKind pass; SyncKind
// sets last_ok only when the complete pass finishes and notifies the planner
// statistics refresher once items are present.
func (s *Service) upsertItemsForSync(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (int, error) {
	mu := s.kindLock(pluginID, kindKey)
	mu.Lock()
	defer mu.Unlock()
	stats, err := s.upsertItemsWithStatsLocked(ctx, pluginID, kindKey, items, s.now().Unix(), true, false, false, false)
	return stats.Accepted, err
}

func (s *Service) upsertItemsLocked(ctx context.Context, pluginID, kindKey string, items []core.KindItem, seenAt int64, bumpGen bool) (int, error) {
	stats, err := s.upsertItemsWithStatsLocked(ctx, pluginID, kindKey, items, seenAt, bumpGen, true, true, false)
	return stats.Accepted, err
}

type upsertUndoCapture struct {
	addedIDs   []string
	updatedIDs []string
	previous   []core.KindItem
}

func (s *Service) upsertItemsWithStatsLocked(ctx context.Context, pluginID, kindKey string, items []core.KindItem, seenAt int64, bumpGen, markOK, notifyStats, pin bool) (UpsertStats, error) {
	stats, _, err := s.upsertItemsWithUndoLocked(ctx, pluginID, kindKey, items, seenAt, bumpGen, markOK, notifyStats, pin, false, false)
	return stats, err
}

func (s *Service) upsertItemsWithUndoLocked(ctx context.Context, pluginID, kindKey string, items []core.KindItem, seenAt int64, bumpGen, markOK, notifyStats, pin, captureUndo, snapshotPrevious bool) (UpsertStats, upsertUndoCapture, error) {
	kind, err := s.GetKind(ctx, pluginID, kindKey)
	if err != nil {
		return UpsertStats{}, upsertUndoCapture{}, err
	}
	if pending, err := s.pendingKindRebuild(ctx, pluginID, kindKey); err != nil {
		return UpsertStats{}, upsertUndoCapture{}, err
	} else if pending != "" {
		// Push plugins have no scheduled pull worker. The first attempted
		// write starts the requested rebuild, then the plugin can retry.
		if markOK && !pin {
			s.StartManualSync(pluginID, kindKey, SyncOptions{rebuildOnly: true})
		}
		return UpsertStats{}, upsertUndoCapture{}, ErrKindRebuildPending
	}
	if len(items) == 0 {
		return UpsertStats{}, upsertUndoCapture{}, nil
	}

	// Track levels in the incoming batch so parent levels within the same
	// batch are verified even before DB insertion.
	batchLevels := make(map[string]string, len(items))
	for _, it := range items {
		if it.ID != "" && it.Level != "" {
			batchLevels[it.ID] = it.Level
		}
	}

	// Look up any referenced parents already in the database.
	parentIDs := make([]string, 0)
	seenParents := map[string]bool{}
	for _, it := range items {
		if it.ParentID != "" && !seenParents[it.ParentID] {
			if _, inBatch := batchLevels[it.ParentID]; !inBatch {
				seenParents[it.ParentID] = true
				parentIDs = append(parentIDs, it.ParentID)
			}
		}
	}
	dbParentLevels, err := s.lookupLevelsByExternalIDs(ctx, pluginID, kindKey, parentIDs)
	if err != nil {
		return UpsertStats{}, upsertUndoCapture{}, err
	}

	// Validate every item and deduplicate by ID (last occurrence in batch wins).
	validated := make([]ValidatedItem, 0, len(items))
	posByID := make(map[string]int, len(items))
	totalDroppedFields := 0
	for _, it := range items {
		parentLevel := ""
		if it.ParentID != "" {
			if lv, ok := batchLevels[it.ParentID]; ok {
				parentLevel = lv
			} else if lv, ok := dbParentLevels[it.ParentID]; ok {
				parentLevel = lv
			}
		}
		v, err := ValidateItem(kind.Decl, it, parentLevel)
		if err != nil {
			return UpsertStats{}, upsertUndoCapture{}, err
		}
		v.Item.Ref = core.NewItemRef(pluginID, kindKey, v.Item.ID)
		totalDroppedFields += v.DroppedFields
		if idx, dup := posByID[v.Item.ID]; dup {
			validated[idx] = v
		} else {
			posByID[v.Item.ID] = len(validated)
			validated = append(validated, v)
		}
	}
	if totalDroppedFields > 0 {
		s.noteAttention(ctx, pluginID, fmt.Sprintf("dropped %d undeclared field(s) on kind %s", totalDroppedFields, kindKey))
	}

	// Prepare every stored representation and content hash before the
	// first write transaction opens, then commit in bounded chunks so
	// the write gate is released between them (#646). The per-kind lock
	// is held throughout, so the batch stays atomic to this kind's
	// writers; a failure partway leaves earlier chunks committed.
	prepared := make([]preparedItem, len(validated))
	for i, v := range validated {
		p, err := prepareUpsertItem(pluginID, kindKey, kind.DeclHash, v)
		if err != nil {
			return UpsertStats{}, upsertUndoCapture{}, err
		}
		prepared[i] = p
	}

	nowUnix := s.now().Unix()
	if seenAt <= 0 {
		seenAt = nowUnix
	}
	stats := UpsertStats{Accepted: len(items)}
	var undo upsertUndoCapture
	var totalItems int64
	err = s.writeChunks(ctx, len(prepared), func(start, end int) (time.Duration, error) {
		res, err := s.writeUpsertChunk(ctx, kind, pluginID, kindKey, prepared[start:end], seenAt, nowUnix, bumpGen && end == len(prepared), markOK, pin, captureUndo, snapshotPrevious)
		if err != nil {
			return 0, err
		}
		stats.Added += res.added
		stats.Updated += res.updated
		stats.Unchanged += res.unchanged
		if captureUndo {
			undo.addedIDs = append(undo.addedIDs, res.addedIDs...)
			undo.updatedIDs = append(undo.updatedIDs, res.updatedIDs...)
			undo.previous = append(undo.previous, res.previous...)
		}
		if res.totalItems > 0 {
			totalItems = res.totalItems
		}
		return res.held, nil
	})
	if err != nil {
		return stats, undo, err
	}
	if notifyStats && totalItems > 0 {
		s.noteLibraryItems(kind.LibraryID, totalItems, false)
	}
	return stats, undo, nil
}

func applyMigration3SQLite(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range []string{
		`ALTER TABLE plugin_kinds ADD COLUMN fts_version INTEGER NOT NULL DEFAULT 0`,
		`DROP TABLE IF EXISTS plugin_item_fts`,
		`CREATE VIRTUAL TABLE plugin_item_fts USING fts5(item_ref UNINDEXED, library_id UNINDEXED, title, fields, people)`,
		`INSERT OR REPLACE INTO plugin_item_fts_config(k, v) VALUES ('rank', 'bm25(10.0, 2.0, 1.0)')`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return reindexAllTx(ctx, tx, false)
}

func applyMigration3Postgres(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE plugin_kinds ADD COLUMN fts_version BIGINT NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	return reindexAllTx(ctx, tx, true)
}

var migration4FoldBatchSize = 500

// ItemContentRating reads the same fields used by the indexed rating
// predicate and the in-process viewer policy check.
func ItemContentRating(fields map[string]json.RawMessage) string {
	for _, key := range []string{"contentRating", "certification", "mpaa"} {
		var value string
		if raw := fields[key]; len(raw) > 0 && json.Unmarshal(raw, &value) == nil {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func contentRatingLevel(fields map[string]json.RawMessage) int {
	return int(contentrating.EnforceLevelOf(ItemContentRating(fields)))
}

func applyMigration9(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE plugin_items ADD COLUMN rating_level INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	const batchSize = 500
	cursor := ""
	for {
		rows, err := tx.QueryContext(ctx, `
SELECT item_ref, fields FROM plugin_items
WHERE item_ref > ?
ORDER BY item_ref ASC
LIMIT ?`, cursor, batchSize)
		if err != nil {
			return err
		}
		type itemFields struct {
			ref    string
			fields string
		}
		var batch []itemFields
		for rows.Next() {
			var it itemFields
			if err := rows.Scan(&it.ref, &it.fields); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, it)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(batch) == 0 {
			return nil
		}
		for _, it := range batch {
			var fields map[string]json.RawMessage
			if json.Unmarshal([]byte(it.fields), &fields) != nil {
				continue
			}
			if lvl := contentRatingLevel(fields); lvl != 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE plugin_items SET rating_level = ? WHERE item_ref = ?`, lvl, it.ref); err != nil {
					return err
				}
			}
		}
		cursor = batch[len(batch)-1].ref
		if len(batch) < batchSize {
			return nil
		}
	}
}

func applyMigration12(ctx context.Context, tx *sql.Tx) error {
	const batchSize = 500
	cursor := ""
	for {
		rows, err := tx.QueryContext(ctx, `
SELECT item_ref, title, sort_title FROM plugin_items
WHERE item_ref > ?
ORDER BY item_ref ASC
LIMIT ?`, cursor, batchSize)
		if err != nil {
			return err
		}
		type itemSort struct {
			ref, title, sortTitle string
		}
		var batch []itemSort
		for rows.Next() {
			var it itemSort
			if err := rows.Scan(&it.ref, &it.title, &it.sortTitle); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, it)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(batch) == 0 {
			return nil
		}
		for _, it := range batch {
			want := core.FoldedSortTitleForRow(it.title, it.sortTitle)
			if want != "" && want != it.sortTitle {
				if _, err := tx.ExecContext(ctx, `UPDATE plugin_items SET sort_title = ?, content_hash = '' WHERE item_ref = ?`, want, it.ref); err != nil {
					return err
				}
			}
		}
		cursor = batch[len(batch)-1].ref
		if len(batch) < batchSize {
			return nil
		}
	}
}

func applyMigration4(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE plugin_item_fields ADD COLUMN v_fold TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := backfillFieldFoldsTx(ctx, tx, migration4FoldBatchSize); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `CREATE INDEX plugin_item_fields_fold ON plugin_item_fields(field, v_fold, v_text, v_int, item_ref)`)
	return err
}

// applyMigration6 adds plugin_item_fields_lookup_real and flags only existing
// plugin_kinds that declared previously-unindexed fields for lazy reindex on
// startup so all declared fields become index-backed for grouping without
// reindexing 1M-item kinds whose fields were already indexed.
func applyMigration6(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS plugin_item_fields_lookup_real ON plugin_item_fields(field, v_real, item_ref)`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT plugin_id, kind_key, decl FROM plugin_kinds`)
	if err != nil {
		return err
	}
	type kindPair struct {
		pluginID string
		kindKey  string
	}
	var toReindex []kindPair
	for rows.Next() {
		var pid, kkey, declJSON string
		if err := rows.Scan(&pid, &kkey, &declJSON); err != nil {
			rows.Close()
			return err
		}
		var decl plugins.MediaKindMeta
		if err := json.Unmarshal([]byte(declJSON), &decl); err != nil {
			continue
		}
		for _, f := range decl.Fields {
			if !f.Indexed && f.Type != "people" {
				toReindex = append(toReindex, kindPair{pluginID: pid, kindKey: kkey})
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, k := range toReindex {
		if _, err := tx.ExecContext(ctx,
			`UPDATE plugin_kinds SET fts_version = 0 WHERE plugin_id = ? AND kind_key = ?`,
			k.pluginID, k.kindKey); err != nil {
			return err
		}
	}
	return nil
}

func backfillFieldFoldsTx(ctx context.Context, tx *sql.Tx, batchSize int) error {
	if batchSize <= 0 {
		batchSize = 500
	}
	curField, curText := "", ""
	for {
		rows, err := tx.QueryContext(ctx, `
SELECT DISTINCT field, v_text
FROM plugin_item_fields
WHERE v_text <> '' AND (field > ? OR (field = ? AND v_text > ?))
ORDER BY field ASC, v_text ASC
LIMIT ?`, curField, curField, curText, batchSize)
		if err != nil {
			return err
		}
		type fieldTextPair struct {
			field string
			text  string
		}
		var batch []fieldTextPair
		for rows.Next() {
			var p fieldTextPair
			if err := rows.Scan(&p.field, &p.text); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, p)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(batch) == 0 {
			break
		}
		for _, p := range batch {
			if folded := FoldFieldText(p.text); folded != "" {
				if _, err := tx.ExecContext(ctx,
					`UPDATE plugin_item_fields SET v_fold = ? WHERE field = ? AND v_text = ?`,
					folded, p.field, p.text); err != nil {
					return err
				}
			}
		}
		last := batch[len(batch)-1]
		curField, curText = last.field, last.text
		if len(batch) < batchSize {
			break
		}
	}
	return nil
}

type pendingKindRow struct {
	pluginID  string
	kindKey   string
	libraryID core.LibraryID
	declJSON  string
}

func reindexAllTx(ctx context.Context, tx *sql.Tx, isPG bool) error {
	rows, err := tx.QueryContext(ctx, `SELECT plugin_id, kind_key, library_id, decl FROM plugin_kinds ORDER BY plugin_id, kind_key`)
	if err != nil {
		return err
	}
	var kinds []pendingKindRow
	for rows.Next() {
		var r pendingKindRow
		var libID int64
		if err := rows.Scan(&r.pluginID, &r.kindKey, &libID, &r.declJSON); err != nil {
			rows.Close()
			return err
		}
		r.libraryID = core.LibraryID(libID)
		kinds = append(kinds, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, k := range kinds {
		var decl plugins.MediaKindMeta
		if err := json.Unmarshal([]byte(k.declJSON), &decl); err != nil {
			continue
		}
		if err := reindexKindTx(ctx, tx, isPG, false, k.pluginID, k.kindKey, k.libraryID, decl); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE plugin_kinds SET fts_version = ? WHERE plugin_id = ? AND kind_key = ?`,
			currentFTSVersion, k.pluginID, k.kindKey); err != nil {
			return err
		}
	}

	if isPG {
		_, err = tx.ExecContext(ctx, `
UPDATE plugin_items
SET search_tsv = setweight(to_tsvector('simple', title), 'A')
WHERE NOT EXISTS (
	SELECT 1 FROM plugin_kinds k WHERE k.plugin_id = plugin_items.plugin_id AND k.kind_key = plugin_items.kind_key
)`)
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO plugin_item_fts(rowid, item_ref, library_id, title, fields, people)
SELECT p.seq, p.item_ref, p.library_id, p.title, '', ''
FROM plugin_items p
WHERE NOT EXISTS (
	SELECT 1 FROM plugin_kinds k WHERE k.plugin_id = p.plugin_id AND k.kind_key = p.kind_key
) AND p.title <> ''`)
	return err
}

func (s *Service) reindexPendingKinds(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT plugin_id, kind_key, library_id, decl FROM plugin_kinds WHERE fts_version < ? ORDER BY plugin_id, kind_key`,
		currentFTSVersion)
	if err != nil {
		return err
	}
	var pending []pendingKindRow
	seen := map[string]bool{}
	for rows.Next() {
		var r pendingKindRow
		var libID int64
		if err := rows.Scan(&r.pluginID, &r.kindKey, &libID, &r.declJSON); err != nil {
			rows.Close()
			return err
		}
		r.libraryID = core.LibraryID(libID)
		pending = append(pending, r)
		seen[workerKey(r.pluginID, r.kindKey)] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	isPG := store.DialectOf(s.db) == store.Postgres
	extra, hasOrphans, err := s.findKindsMissingSearchIndex(ctx, isPG, seen)
	if err != nil {
		return err
	}

	for _, k := range pending {
		var decl plugins.MediaKindMeta
		if err := json.Unmarshal([]byte(k.declJSON), &decl); err != nil {
			continue
		}
		if err := s.reindexKindChunked(ctx, isPG, k.pluginID, k.kindKey, k.libraryID, decl); err != nil {
			return err
		}
	}
	for _, k := range extra {
		var decl plugins.MediaKindMeta
		if err := json.Unmarshal([]byte(k.declJSON), &decl); err != nil {
			continue
		}
		if err := s.reindexSearchIndexChunked(ctx, isPG, k.pluginID, k.kindKey, k.libraryID, decl); err != nil {
			return err
		}
	}
	if hasOrphans {
		if err := s.reindexOrphanSearchChunked(ctx, isPG); err != nil {
			return err
		}
	}
	return nil
}

// reindexSearchIndexChunked recomputes only the engine-local search index
// (PostgreSQL plugin_items.search_tsv with title A / fields B / people C
// weighting, or SQLite plugin_item_fts) for rows missing an index entry in
// bounded keyset batches without touching plugin_item_fields or holding a
// full-table lock. Already-indexed rows are skipped so an interrupted run
// resumes cleanly on the next startup.
func (s *Service) reindexSearchIndexChunked(ctx context.Context, isPG bool, pluginID, kindKey string, fallbackLibID core.LibraryID, decl plugins.MediaKindMeta) error {
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		mu := s.kindLock(pluginID, kindKey)
		mu.Lock()
		nextCursor, done, wrote, err := s.reindexSearchIndexOneChunkLocked(ctx, isPG, pluginID, kindKey, fallbackLibID, decl, cursor)
		mu.Unlock()
		if err != nil {
			return err
		}
		cursor = nextCursor
		if done {
			return nil
		}
		if wrote {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(reindexChunkYield):
			}
			if err := store.WaitWALBudget(ctx, s.db); err != nil {
				return err
			}
		}
	}
}

func (s *Service) reindexSearchIndexOneChunkLocked(ctx context.Context, isPG bool, pluginID, kindKey string, fallbackLibID core.LibraryID, decl plugins.MediaKindMeta, cursor string) (string, bool, bool, error) {
	var q string
	if isPG {
		q = `
SELECT item_ref, seq, library_id, title, fields
FROM plugin_items
WHERE plugin_id = ? AND kind_key = ? AND item_ref > ?
  AND search_tsv = ''::tsvector
ORDER BY item_ref ASC
LIMIT ?`
	} else {
		q = `
SELECT p.item_ref, p.seq, p.library_id, p.title, p.fields
FROM plugin_items p
WHERE p.plugin_id = ? AND p.kind_key = ? AND p.item_ref > ?
  AND NOT EXISTS (SELECT 1 FROM plugin_item_fts f WHERE f.rowid = p.seq)
ORDER BY p.item_ref ASC
LIMIT ?`
	}
	rows, err := s.db.QueryContext(ctx, q, pluginID, kindKey, cursor, upsertChunkSize)
	if err != nil {
		return cursor, false, false, err
	}
	var batch []reindexItemRow
	for rows.Next() {
		var r reindexItemRow
		var libID int64
		if err := rows.Scan(&r.itemRef, &r.seq, &libID, &r.title, &r.fieldsJSON); err != nil {
			rows.Close()
			return cursor, false, false, err
		}
		if libID > 0 {
			r.libraryID = core.LibraryID(libID)
		} else {
			r.libraryID = fallbackLibID
		}
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return cursor, false, false, err
	}
	rows.Close()
	if len(batch) == 0 {
		return cursor, true, false, nil
	}

	var ftsWrites []reindexFTSWrite
	for _, r := range batch {
		_, searchFields, searchPeople := extractIndexData(decl, r.fieldsJSON)
		if !isPG && strings.TrimSpace(r.title) == "" && strings.TrimSpace(searchFields) == "" && strings.TrimSpace(searchPeople) == "" {
			continue
		}
		ftsWrites = append(ftsWrites, reindexFTSWrite{
			seq:       r.seq,
			itemRef:   r.itemRef,
			libraryID: r.libraryID,
			title:     r.title,
			fields:    searchFields,
			people:    searchPeople,
		})
	}

	nextCursor := batch[len(batch)-1].itemRef
	done := len(batch) < upsertChunkSize
	if len(ftsWrites) == 0 {
		return nextCursor, done, false, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return cursor, false, false, err
	}
	defer tx.Rollback()

	for _, fw := range ftsWrites {
		if isPG {
			if _, err := tx.ExecContext(ctx, `
UPDATE plugin_items
SET search_tsv = setweight(to_tsvector('simple', ?), 'A') || setweight(to_tsvector('simple', ?), 'B') || setweight(to_tsvector('simple', ?), 'C')
WHERE item_ref = ?
  AND search_tsv IS DISTINCT FROM (setweight(to_tsvector('simple', ?), 'A') || setweight(to_tsvector('simple', ?), 'B') || setweight(to_tsvector('simple', ?), 'C'))`,
				fw.title, fw.fields, fw.people, fw.itemRef, fw.title, fw.fields, fw.people); err != nil {
				return cursor, false, false, err
			}
		} else {
			if err := sqliteIndexFTS(ctx, tx, fw.seq, core.ItemRef(fw.itemRef), fw.libraryID, fw.title, fw.fields, fw.people); err != nil {
				return cursor, false, false, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return cursor, false, false, err
	}
	s.noteWrite()
	s.syncGen.Add(1)
	return nextCursor, done, true, nil
}

// findKindsMissingSearchIndex detects restored or unindexed plugin items whose
// kind already carries fts_version >= currentFTSVersion (for example a database
// restored from SQLite onto PostgreSQL before the restore hook existed) as well
// as orphan items without a plugin_kinds row.
func (s *Service) findKindsMissingSearchIndex(ctx context.Context, isPG bool, seen map[string]bool) ([]pendingKindRow, bool, error) {
	if isPG {
		// Guard with to_tsvector('simple', title) <> ''::tsvector so items with
		// punctuation-only titles (which legitimately produce an empty tsvector)
		// do not re-trigger a rebuild on every startup.
		rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT plugin_id, kind_key
FROM plugin_items
WHERE search_tsv = ''::tsvector
  AND title <> ''
  AND to_tsvector('simple', title) <> ''::tsvector
ORDER BY plugin_id, kind_key`)
		if err != nil {
			return nil, false, err
		}
		type kindPair struct{ pluginID, kindKey string }
		var missing []kindPair
		for rows.Next() {
			var p kindPair
			if err := rows.Scan(&p.pluginID, &p.kindKey); err != nil {
				rows.Close()
				return nil, false, err
			}
			if !seen[workerKey(p.pluginID, p.kindKey)] {
				missing = append(missing, p)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, false, err
		}
		rows.Close()
		if len(missing) == 0 {
			return nil, false, nil
		}
		var extra []pendingKindRow
		var hasOrphans bool
		for _, p := range missing {
			var r pendingKindRow
			var libID int64
			err := s.db.QueryRowContext(ctx,
				`SELECT plugin_id, kind_key, library_id, decl FROM plugin_kinds WHERE plugin_id = ? AND kind_key = ?`,
				p.pluginID, p.kindKey).Scan(&r.pluginID, &r.kindKey, &libID, &r.declJSON)
			if errors.Is(err, sql.ErrNoRows) {
				hasOrphans = true
				continue
			}
			if err != nil {
				return nil, false, err
			}
			r.libraryID = core.LibraryID(libID)
			seen[workerKey(r.pluginID, r.kindKey)] = true
			extra = append(extra, r)
		}
		return extra, hasOrphans, nil
	}

	// SQLite: O(1) probe for an empty plugin_item_fts alongside titled items
	// (e.g. a logical restore performed before pluginitems registered a restore
	// hook). Trim avoids re-triggering when only whitespace-only titles exist.
	var anyItem int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM plugin_items WHERE trim(title) <> '' LIMIT 1`).Scan(&anyItem); errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	var anyFTS int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM plugin_item_fts LIMIT 1`).Scan(&anyFTS)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		rows, err := s.db.QueryContext(ctx,
			`SELECT plugin_id, kind_key, library_id, decl FROM plugin_kinds ORDER BY plugin_id, kind_key`)
		if err != nil {
			return nil, false, err
		}
		var extra []pendingKindRow
		for rows.Next() {
			var r pendingKindRow
			var libID int64
			if err := rows.Scan(&r.pluginID, &r.kindKey, &libID, &r.declJSON); err != nil {
				rows.Close()
				return nil, false, err
			}
			r.libraryID = core.LibraryID(libID)
			if !seen[workerKey(r.pluginID, r.kindKey)] {
				seen[workerKey(r.pluginID, r.kindKey)] = true
				extra = append(extra, r)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, false, err
		}
		rows.Close()
		return extra, true, nil
	}
	if len(seen) > 0 {
		var orphan int
		err := s.db.QueryRowContext(ctx, `
SELECT 1
FROM plugin_items p
WHERE trim(p.title) <> ''
  AND NOT EXISTS (SELECT 1 FROM plugin_kinds k WHERE k.plugin_id = p.plugin_id AND k.kind_key = p.kind_key)
  AND NOT EXISTS (SELECT 1 FROM plugin_item_fts f WHERE f.rowid = p.seq)
LIMIT 1`).Scan(&orphan)
		if err == nil && orphan == 1 {
			return nil, true, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}
	}
	return nil, false, nil
}

// reindexOrphanSearchChunked rebuilds search indexes for any plugin_items rows
// without a matching plugin_kinds entry, in bounded keyset batches using
// title-only weight 'A' (matching reindexAllTx).
func (s *Service) reindexOrphanSearchChunked(ctx context.Context, isPG bool) error {
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var q string
		if isPG {
			q = `
SELECT p.item_ref, p.seq, p.library_id, p.title
FROM plugin_items p
WHERE p.item_ref > ?
  AND p.search_tsv = ''::tsvector
  AND p.title <> ''
  AND to_tsvector('simple', p.title) <> ''::tsvector
  AND NOT EXISTS (SELECT 1 FROM plugin_kinds k WHERE k.plugin_id = p.plugin_id AND k.kind_key = p.kind_key)
ORDER BY p.item_ref ASC
LIMIT ?`
		} else {
			q = `
SELECT p.item_ref, p.seq, p.library_id, p.title
FROM plugin_items p
WHERE p.item_ref > ?
  AND trim(p.title) <> ''
  AND NOT EXISTS (SELECT 1 FROM plugin_kinds k WHERE k.plugin_id = p.plugin_id AND k.kind_key = p.kind_key)
  AND NOT EXISTS (SELECT 1 FROM plugin_item_fts f WHERE f.rowid = p.seq)
ORDER BY p.item_ref ASC
LIMIT ?`
		}
		rows, err := s.db.QueryContext(ctx, q, cursor, upsertChunkSize)
		if err != nil {
			return err
		}
		type orphanRow struct {
			itemRef   string
			seq       int64
			libraryID core.LibraryID
			title     string
		}
		var batch []orphanRow
		for rows.Next() {
			var r orphanRow
			var libID int64
			if err := rows.Scan(&r.itemRef, &r.seq, &libID, &r.title); err != nil {
				rows.Close()
				return err
			}
			r.libraryID = core.LibraryID(libID)
			batch = append(batch, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(batch) == 0 {
			return nil
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, r := range batch {
			if isPG {
				if _, err := tx.ExecContext(ctx, `
UPDATE plugin_items
SET search_tsv = setweight(to_tsvector('simple', ?), 'A')
WHERE item_ref = ?
  AND search_tsv IS DISTINCT FROM setweight(to_tsvector('simple', ?), 'A')`,
					r.title, r.itemRef, r.title); err != nil {
					tx.Rollback()
					return err
				}
			} else {
				if err := sqliteIndexFTS(ctx, tx, r.seq, core.ItemRef(r.itemRef), r.libraryID, r.title, "", ""); err != nil {
					tx.Rollback()
					return err
				}
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		s.noteWrite()
		s.syncGen.Add(1)

		cursor = batch[len(batch)-1].itemRef
		if len(batch) < upsertChunkSize {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reindexChunkYield):
		}
		if err := store.WaitWALBudget(ctx, s.db); err != nil {
			return err
		}
	}
}

// scheduleStaleArtistReindexes checks existing kinds that declare string-typed
// artist fields ("artists", "album_artists", etc.) and triggers a non-blocking
// chunked background reindex when pre-upgrade rows still have v_int = 0.
func (s *Service) scheduleStaleArtistReindexes(ctx context.Context) {
	if s == nil || s.db == nil {
		return
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT plugin_id, kind_key, library_id, decl FROM plugin_kinds ORDER BY plugin_id, kind_key`)
	if err != nil {
		return
	}
	var all []pendingKindRow
	for rows.Next() {
		var r pendingKindRow
		var libID int64
		if err := rows.Scan(&r.pluginID, &r.kindKey, &libID, &r.declJSON); err != nil {
			rows.Close()
			return
		}
		r.libraryID = core.LibraryID(libID)
		all = append(all, r)
	}
	rows.Close()
	for _, k := range all {
		var decl plugins.MediaKindMeta
		if err := json.Unmarshal([]byte(k.declJSON), &decl); err != nil {
			continue
		}
		if s.kindHasUnindexedArtistStrings(ctx, k.libraryID, decl) {
			s.startKindReindex(k.pluginID, k.kindKey, k.libraryID, decl)
		}
	}
}

// ensureParentTallies verifies at startup that every multi-level plugin kind's
// plugin_item_parent_tally child_count total matches its non-root item count
// in plugin_item_tally (O(1) tally check per kind). Any drift (e.g. from a
// pre-fix chunked reindex, backup restore, or level-hierarchy upgrade) is
// rebuilt immediately so list projections never omit media.childCounts for
// containers with stored children.
func (s *Service) ensureParentTallies(ctx context.Context) {
	if s == nil || s.db == nil {
		return
	}
	dialect := store.DialectOf(s.db)
	if !s.hasTable(ctx, dialect, "plugin_item_parent_tally") || !s.hasTable(ctx, dialect, "plugin_item_tally") {
		return
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT plugin_id, kind_key, library_id, decl FROM plugin_kinds WHERE library_id > 0 ORDER BY plugin_id, kind_key`)
	if err != nil {
		return
	}
	var all []pendingKindRow
	for rows.Next() {
		var r pendingKindRow
		var libID int64
		if err := rows.Scan(&r.pluginID, &r.kindKey, &libID, &r.declJSON); err != nil {
			rows.Close()
			return
		}
		r.libraryID = core.LibraryID(libID)
		all = append(all, r)
	}
	rows.Close()
	for _, k := range all {
		var decl plugins.MediaKindMeta
		if err := json.Unmarshal([]byte(k.declJSON), &decl); err != nil || len(decl.Levels) < 2 {
			continue
		}
		// Items at a level with a parent level are the ones the tally
		// counts as children. Levels[0] is not the only root: a members
		// level (playlists) has no parent either, and counting it as a
		// child made every startup see drift and rebuild the whole tally.
		var childLevels []any
		for _, lv := range decl.Levels {
			if decl.LevelParent(lv.Key) != "" {
				childLevels = append(childLevels, lv.Key)
			}
		}
		if len(childLevels) == 0 {
			continue
		}
		var nonRootCount, talliedChildren int64
		if err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(n), 0) FROM plugin_item_tally WHERE library_id = ? AND level IN (`+store.Placeholders(len(childLevels))+`)`,
			append([]any{int64(k.libraryID)}, childLevels...)...).Scan(&nonRootCount); err != nil {
			continue
		}
		if err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(child_count), 0) FROM plugin_item_parent_tally WHERE library_id = ?`,
			int64(k.libraryID)).Scan(&talliedChildren); err != nil {
			continue
		}
		if nonRootCount == talliedChildren {
			continue
		}
		// Real drift (orphaned children, a restore, an older binary) is
		// repaired row by row in short transactions; only differing rows
		// are written.
		mu := s.kindLock(k.pluginID, k.kindKey)
		mu.Lock()
		if err := s.repairKindParentTallyLocked(ctx, k.pluginID, k.kindKey, k.libraryID, decl); err != nil {
			s.log.Warn("parent tally repair failed", "plugin", k.pluginID, "kind", k.kindKey, "err", err)
		}
		mu.Unlock()
	}
}

func (s *Service) kindHasUnindexedArtistStrings(ctx context.Context, libraryID core.LibraryID, decl plugins.MediaKindMeta) bool {
	if s == nil || s.db == nil || libraryID <= 0 {
		return false
	}
	for _, fd := range decl.Fields {
		if !isArtistStringField(fd) {
			continue
		}
		var one int
		err := s.db.QueryRowContext(ctx, `
SELECT 1
FROM plugin_item_fields f
JOIN plugin_items i ON i.item_ref = f.item_ref
WHERE i.library_id = ? AND f.field = ? AND f.v_text <> '' AND f.v_int = 0
LIMIT 1`, int64(libraryID), fd.Key).Scan(&one)
		if err == nil && one == 1 {
			return true
		}
	}
	return false
}

// startKindReindex schedules a chunked background reindex for (pluginID, kindKey)
// so index-shape upgrades return immediately from plugin install without holding
// the SQLite write lock across the library.
func (s *Service) startKindReindex(pluginID, kindKey string, libraryID core.LibraryID, decl plugins.MediaKindMeta) {
	if s == nil || s.db == nil {
		return
	}
	key := workerKey(pluginID, kindKey)
	s.workersMu.Lock()
	var prevDone chan struct{}
	if old, ok := s.reindexers[key]; ok && old != nil {
		old.cancel()
		prevDone = old.done
	}
	parent := s.bgCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	s.reindexers[key] = &syncWorker{cancel: cancel, done: done}
	s.workersMu.Unlock()

	isPG := store.DialectOf(s.db) == store.Postgres
	go func() {
		defer func() {
			s.clearSyncProgressIfPhase(pluginID, kindKey, "reindexing")
			s.workersMu.Lock()
			if cur, ok := s.reindexers[key]; ok && cur.done == done {
				delete(s.reindexers, key)
			}
			s.workersMu.Unlock()
			close(done)
		}()
		if prevDone != nil {
			<-prevDone
		}
		if ctx.Err() != nil {
			return
		}
		if err := s.reindexKindChunked(ctx, isPG, pluginID, kindKey, libraryID, decl); err != nil && !errors.Is(err, context.Canceled) {
			if s.log != nil {
				s.log.Warn("background kind reindex failed", "plugin", pluginID, "kind", kindKey, "err", err)
			}
		}
	}()
}

// reindexChunkYield spaces multi-chunk reindex write batches so interactive
// writers queued on the SQLite write gate run between chunks.
const reindexChunkYield = 5 * time.Millisecond

// reindexKindChunked rebuilds plugin_item_fields, FTS, and field tallies for one
// kind in bounded upsertChunkSize transactions so the database write gate is
// released between chunks and field tallies stay consistent incrementally
// without a monolithic post-loop rebuild transaction (#685).
func (s *Service) reindexKindChunked(ctx context.Context, isPG bool, pluginID, kindKey string, fallbackLibID core.LibraryID, decl plugins.MediaKindMeta) error {
	totalItems, _ := s.countKindItems(ctx, pluginID, kindKey)
	s.setSyncProgress(pluginID, kindKey, syncProgress{
		running:    true,
		phase:      "reindexing",
		itemsTotal: totalItems,
	})
	defer s.clearSyncProgressIfPhase(pluginID, kindKey, "reindexing")

	dialect := store.SQLite
	if isPG {
		dialect = store.Postgres
	}
	hasFieldTally := s.hasTable(ctx, dialect, "plugin_item_field_tally")

	cursor := ""
	processed := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		mu := s.kindLock(pluginID, kindKey)
		mu.Lock()
		n, nextCursor, done, wrote, err := s.reindexKindOneChunkLocked(ctx, isPG, hasFieldTally, pluginID, kindKey, fallbackLibID, decl, cursor)
		mu.Unlock()
		if err != nil {
			return err
		}
		processed += n
		cursor = nextCursor
		s.setSyncProgress(pluginID, kindKey, syncProgress{
			running:     true,
			phase:       "reindexing",
			syncedItems: processed,
			itemsTotal:  max(totalItems, processed),
		})
		if done {
			break
		}
		if wrote {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(reindexChunkYield):
			}
			// Bound WAL growth across long reindexes by passively checkpointing
			// and waiting out any pinning long read between chunks (#685).
			if err := store.WaitWALBudget(ctx, s.db); err != nil {
				return err
			}
		}
	}

	mu := s.kindLock(pluginID, kindKey)
	mu.Lock()
	defer mu.Unlock()
	if s.hasTable(ctx, dialect, "plugin_item_parent_tally") {
		if err := s.repairKindParentTallyLocked(ctx, pluginID, kindKey, fallbackLibID, decl); err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE plugin_kinds SET fts_version = ? WHERE plugin_id = ? AND kind_key = ?`,
		currentFTSVersion, pluginID, kindKey); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.syncGen.Add(1)
	return nil
}

type indexedFieldPK struct {
	field string
	text  string
	intV  int64
}

type indexedFieldExtra struct {
	realV float64
	fold  string
}

type reindexFieldDelete struct {
	itemRef string
	pk      indexedFieldPK
}

type reindexFieldUpsert struct {
	itemRef string
	pk      indexedFieldPK
	extra   indexedFieldExtra
}

type existingFTSRow struct {
	itemRef   string
	libraryID int64
	title     string
	fields    string
	people    string
}

type reindexFTSWrite struct {
	seq       int64
	itemRef   string
	libraryID core.LibraryID
	title     string
	fields    string
	people    string
}

func (s *Service) reindexKindOneChunkLocked(ctx context.Context, isPG, hasFieldTally bool, pluginID, kindKey string, fallbackLibID core.LibraryID, decl plugins.MediaKindMeta, cursor string) (int, string, bool, bool, error) {
	// Read items, existing indexed fields, and existing FTS rows outside a write
	// transaction so the SQLite write gate is never held during reads, JSON
	// unmarshaling, or diff computation — and unchanged chunks open no write tx.
	rows, err := s.db.QueryContext(ctx, `
SELECT item_ref, seq, library_id, level, title, fields, released, added_at, year
FROM plugin_items
WHERE plugin_id = ? AND kind_key = ? AND item_ref > ?
ORDER BY item_ref ASC
LIMIT ?`, pluginID, kindKey, cursor, upsertChunkSize)
	if err != nil {
		return 0, cursor, false, false, err
	}
	var batch []reindexItemRow
	for rows.Next() {
		var r reindexItemRow
		var libID int64
		if err := rows.Scan(&r.itemRef, &r.seq, &libID, &r.level, &r.title, &r.fieldsJSON, &r.released, &r.addedAt, &r.year); err != nil {
			rows.Close()
			return 0, cursor, false, false, err
		}
		if libID > 0 {
			r.libraryID = core.LibraryID(libID)
		} else {
			r.libraryID = fallbackLibID
		}
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, cursor, false, false, err
	}
	rows.Close()
	if len(batch) == 0 {
		return 0, cursor, true, false, nil
	}

	oldFieldsByRef, err := s.lookupExistingIndexedFields(ctx, batch)
	if err != nil {
		return 0, cursor, false, false, err
	}
	var oldFTSBySeq map[int64]existingFTSRow
	if !isPG {
		seqs := make([]any, len(batch))
		for i, r := range batch {
			seqs[i] = r.seq
		}
		oldFTSBySeq, err = s.lookupExistingSQLiteFTS(ctx, seqs)
		if err != nil {
			return 0, cursor, false, false, err
		}
	}

	var (
		fieldDeletes    []reindexFieldDelete
		fieldInserts    []reindexFieldUpsert
		fieldUpdates    []reindexFieldUpsert
		ftsWrites       []reindexFTSWrite
		fieldTallyLibs  map[core.LibraryID]map[fieldTallyKey]*fieldTallyChange
		personTallyLibs map[core.LibraryID]map[personTallyKey]*personTallyChange
	)
	deltaFor := func(libID core.LibraryID) map[fieldTallyKey]*fieldTallyChange {
		if fieldTallyLibs == nil {
			fieldTallyLibs = map[core.LibraryID]map[fieldTallyKey]*fieldTallyChange{}
		}
		d := fieldTallyLibs[libID]
		if d == nil {
			d = map[fieldTallyKey]*fieldTallyChange{}
			fieldTallyLibs[libID] = d
		}
		return d
	}
	personDeltaFor := func(libID core.LibraryID) map[personTallyKey]*personTallyChange {
		if personTallyLibs == nil {
			personTallyLibs = map[core.LibraryID]map[personTallyKey]*personTallyChange{}
		}
		d := personTallyLibs[libID]
		if d == nil {
			d = map[personTallyKey]*personTallyChange{}
			personTallyLibs[libID] = d
		}
		return d
	}

	for _, r := range batch {
		indexed, searchFields, searchPeople := extractIndexData(decl, r.fieldsJSON)
		newMap := make(map[indexedFieldPK]indexedFieldExtra, len(indexed))
		var itemPersonHasThumb map[int64]bool
		for _, fv := range indexed {
			newMap[indexedFieldPK{field: fv.Field, text: fv.Text, intV: fv.Int}] = indexedFieldExtra{realV: fv.Real, fold: fv.Fold}
			if fv.HasThumb && core.IsPluginPersonID(fv.Int) {
				if itemPersonHasThumb == nil {
					itemPersonHasThumb = map[int64]bool{}
				}
				itemPersonHasThumb[fv.Int] = true
			}
		}
		rank := tallyItemRank{ref: r.itemRef, released: r.released, addedAt: r.addedAt, year: r.year}
		oldMap := oldFieldsByRef[r.itemRef]
		for pk := range oldMap {
			if _, keep := newMap[pk]; !keep {
				fieldDeletes = append(fieldDeletes, reindexFieldDelete{itemRef: r.itemRef, pk: pk})
				if hasFieldTally && pk.text != "" && r.level != "" {
					addFieldTallyChange(deltaFor(r.libraryID), r.level, pk.field, pk.text, -1, tallyItemRank{}, r.itemRef)
					if core.IsPluginPersonID(pk.intV) {
						addPersonTallyChange(personDeltaFor(r.libraryID), r.level, pk.field, pk.intV, -1, "", pk.text, tallyItemRank{}, r.itemRef, tallyItemRank{}, r.itemRef, r.itemRef)
					}
				}
			}
		}
		for pk, nv := range newMap {
			ov, existed := oldMap[pk]
			if !existed {
				fieldInserts = append(fieldInserts, reindexFieldUpsert{itemRef: r.itemRef, pk: pk, extra: nv})
				if hasFieldTally && pk.text != "" && r.level != "" {
					addFieldTallyChange(deltaFor(r.libraryID), r.level, pk.field, pk.text, 1, rank, "")
					if core.IsPluginPersonID(pk.intV) {
						var th tallyItemRank
						if itemPersonHasThumb[pk.intV] {
							th = rank
						}
						addPersonTallyChange(personDeltaFor(r.libraryID), r.level, pk.field, pk.intV, 1, pk.text, "", rank, "", th, "", r.itemRef)
					}
				}
			} else if ov != nv {
				fieldUpdates = append(fieldUpdates, reindexFieldUpsert{itemRef: r.itemRef, pk: pk, extra: nv})
			}
		}

		if isPG {
			ftsWrites = append(ftsWrites, reindexFTSWrite{
				seq:       r.seq,
				itemRef:   r.itemRef,
				libraryID: r.libraryID,
				title:     r.title,
				fields:    searchFields,
				people:    searchPeople,
			})
		} else {
			wt := strings.TrimSpace(r.title)
			wf := strings.TrimSpace(searchFields)
			wp := strings.TrimSpace(searchPeople)
			wantEmpty := wt == "" && wf == "" && wp == ""
			cur, hasCur := oldFTSBySeq[r.seq]
			matches := (wantEmpty && !hasCur) || (!wantEmpty && hasCur &&
				cur.itemRef == r.itemRef &&
				cur.libraryID == int64(r.libraryID) &&
				cur.title == wt &&
				cur.fields == wf &&
				cur.people == wp)
			if !matches {
				ftsWrites = append(ftsWrites, reindexFTSWrite{
					seq:       r.seq,
					itemRef:   r.itemRef,
					libraryID: r.libraryID,
					title:     r.title,
					fields:    searchFields,
					people:    searchPeople,
				})
			}
		}
	}

	nextCursor := batch[len(batch)-1].itemRef
	done := len(batch) < upsertChunkSize
	if len(fieldDeletes) == 0 && len(fieldInserts) == 0 && len(fieldUpdates) == 0 && len(ftsWrites) == 0 {
		return len(batch), nextCursor, done, false, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, cursor, false, false, err
	}
	defer tx.Rollback()
	if err := lockFieldPresence(ctx, tx, isPG, fallbackLibID); err != nil {
		return 0, cursor, false, false, err
	}
	changedRefs := make(map[core.ItemRef]struct{})
	for _, f := range fieldDeletes {
		changedRefs[core.ItemRef(f.itemRef)] = struct{}{}
	}
	for _, f := range fieldInserts {
		changedRefs[core.ItemRef(f.itemRef)] = struct{}{}
	}
	presenceRefs := slices.Collect(maps.Keys(changedRefs))
	presence := fieldPresenceDelta{isPG: isPG}
	if err := presence.collect(ctx, tx, presenceRefs, -1); err != nil {
		return 0, cursor, false, false, err
	}

	for _, del := range fieldDeletes {
		var err error
		if isPG {
			_, err = tx.ExecContext(ctx,
				`DELETE FROM plugin_item_fields WHERE item_ref = ? AND field = ? AND md5(v_text) = ? AND v_text = ? AND v_int = ?`,
				del.itemRef, del.pk.field, pgTextMD5(del.pk.text), del.pk.text, del.pk.intV)
		} else {
			_, err = tx.ExecContext(ctx,
				`DELETE FROM plugin_item_fields WHERE item_ref = ? AND field = ? AND v_text = ? AND v_int = ?`,
				del.itemRef, del.pk.field, del.pk.text, del.pk.intV)
		}
		if err != nil {
			return 0, cursor, false, false, err
		}
	}
	fieldConflictCols := `item_ref, field, v_text, v_int`
	if isPG {
		fieldConflictCols = `item_ref, field, md5(v_text), v_int`
	}
	for _, ins := range fieldInserts {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO plugin_item_fields(item_ref, field, v_text, v_int, v_real, v_fold) VALUES(?, ?, ?, ?, ?, ?)
			 ON CONFLICT(`+fieldConflictCols+`) DO UPDATE SET v_real = excluded.v_real, v_fold = excluded.v_fold`,
			ins.itemRef, ins.pk.field, ins.pk.text, ins.pk.intV, ins.extra.realV, ins.extra.fold); err != nil {
			return 0, cursor, false, false, err
		}
	}
	for _, upd := range fieldUpdates {
		var err error
		if isPG {
			_, err = tx.ExecContext(ctx,
				`UPDATE plugin_item_fields SET v_real = ?, v_fold = ? WHERE item_ref = ? AND field = ? AND md5(v_text) = ? AND v_text = ? AND v_int = ?`,
				upd.extra.realV, upd.extra.fold, upd.itemRef, upd.pk.field, pgTextMD5(upd.pk.text), upd.pk.text, upd.pk.intV)
		} else {
			_, err = tx.ExecContext(ctx,
				`UPDATE plugin_item_fields SET v_real = ?, v_fold = ? WHERE item_ref = ? AND field = ? AND v_text = ? AND v_int = ?`,
				upd.extra.realV, upd.extra.fold, upd.itemRef, upd.pk.field, upd.pk.text, upd.pk.intV)
		}
		if err != nil {
			return 0, cursor, false, false, err
		}
	}
	for _, fw := range ftsWrites {
		if isPG {
			if _, err := tx.ExecContext(ctx, `
UPDATE plugin_items
SET search_tsv = setweight(to_tsvector('simple', ?), 'A') || setweight(to_tsvector('simple', ?), 'B') || setweight(to_tsvector('simple', ?), 'C')
WHERE item_ref = ?
  AND search_tsv IS DISTINCT FROM (setweight(to_tsvector('simple', ?), 'A') || setweight(to_tsvector('simple', ?), 'B') || setweight(to_tsvector('simple', ?), 'C'))`,
				fw.title, fw.fields, fw.people, fw.itemRef, fw.title, fw.fields, fw.people); err != nil {
				return 0, cursor, false, false, err
			}
		} else {
			if err := sqliteIndexFTS(ctx, tx, fw.seq, core.ItemRef(fw.itemRef), fw.libraryID, fw.title, fw.fields, fw.people); err != nil {
				return 0, cursor, false, false, err
			}
		}
	}
	if err := presence.finish(ctx, tx, presenceRefs); err != nil {
		return 0, cursor, false, false, err
	}
	if hasFieldTally && (len(fieldTallyLibs) > 0 || len(personTallyLibs) > 0) {
		libSet := make(map[core.LibraryID]struct{}, len(fieldTallyLibs)+len(personTallyLibs))
		for libID := range fieldTallyLibs {
			libSet[libID] = struct{}{}
		}
		for libID := range personTallyLibs {
			libSet[libID] = struct{}{}
		}
		libs := make([]core.LibraryID, 0, len(libSet))
		for libID := range libSet {
			libs = append(libs, libID)
		}
		slices.Sort(libs)
		dialect := store.SQLite
		if isPG {
			dialect = store.Postgres
		}
		hasPersonTally := len(personTallyLibs) > 0 && hasTableTx(ctx, tx, dialect, "plugin_item_person_tally")
		for _, libID := range libs {
			emptied, err := applyFieldTallyDeltaTx(ctx, tx, isPG, libID, fieldTallyLibs[libID])
			if err != nil {
				return 0, cursor, false, false, err
			}
			if hasPersonTally && len(personTallyLibs[libID]) > 0 {
				if err := applyPersonTallyDeltaTx(ctx, tx, isPG, libID, decl, personTallyLibs[libID], emptied); err != nil {
					return 0, cursor, false, false, err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, cursor, false, false, err
	}
	s.invalidateKindCache(pluginID, kindKey, fallbackLibID)
	s.noteWrite()
	s.syncGen.Add(1)
	return len(batch), nextCursor, done, true, nil
}

func (s *Service) lookupExistingIndexedFields(ctx context.Context, batch []reindexItemRow) (map[string]map[indexedFieldPK]indexedFieldExtra, error) {
	out := make(map[string]map[indexedFieldPK]indexedFieldExtra, len(batch))
	if len(batch) == 0 {
		return out, nil
	}
	const chunkSize = 250
	for start := 0; start < len(batch); start += chunkSize {
		end := min(start+chunkSize, len(batch))
		chunk := batch[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, r := range chunk {
			placeholders[i] = "?"
			args[i] = r.itemRef
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT item_ref, field, v_text, v_int, v_real, v_fold FROM plugin_item_fields WHERE item_ref IN (`+strings.Join(placeholders, ",")+`)`,
			args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var ref, field, text, fold string
			var iv int64
			var rv float64
			if err := rows.Scan(&ref, &field, &text, &iv, &rv, &fold); err != nil {
				rows.Close()
				return nil, err
			}
			m := out[ref]
			if m == nil {
				m = map[indexedFieldPK]indexedFieldExtra{}
				out[ref] = m
			}
			m[indexedFieldPK{field: field, text: text, intV: iv}] = indexedFieldExtra{realV: rv, fold: fold}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func (s *Service) lookupExistingSQLiteFTS(ctx context.Context, seqs []any) (map[int64]existingFTSRow, error) {
	out := make(map[int64]existingFTSRow, len(seqs))
	if len(seqs) == 0 {
		return out, nil
	}
	const chunkSize = 250
	for start := 0; start < len(seqs); start += chunkSize {
		end := min(start+chunkSize, len(seqs))
		rows, err := s.db.QueryContext(ctx,
			`SELECT rowid, item_ref, library_id, title, fields, people FROM plugin_item_fts WHERE rowid IN (`+store.Placeholders(end-start)+`)`, seqs[start:end]...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var seq int64
			var r existingFTSRow
			if err := rows.Scan(&seq, &r.itemRef, &r.libraryID, &r.title, &r.fields, &r.people); err != nil {
				rows.Close()
				return nil, err
			}
			out[seq] = r
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func (s *Service) hasTable(ctx context.Context, d store.Dialect, table string) bool {
	var n int
	if d == store.Postgres {
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

type reindexItemRow struct {
	itemRef    string
	seq        int64
	libraryID  core.LibraryID
	level      string
	title      string
	fieldsJSON string
	released   int64
	addedAt    int64
	year       int64
}

func reindexKindTx(ctx context.Context, tx *sql.Tx, isPG, hasFold bool, pluginID, kindKey string, fallbackLibID core.LibraryID, decl plugins.MediaKindMeta) error {
	const batchSize = 500
	cursor := ""
	for {
		rows, err := tx.QueryContext(ctx, `
SELECT item_ref, seq, library_id, title, fields
FROM plugin_items
WHERE plugin_id = ? AND kind_key = ? AND item_ref > ?
ORDER BY item_ref ASC
LIMIT ?`, pluginID, kindKey, cursor, batchSize)
		if err != nil {
			return err
		}
		var batch []reindexItemRow
		for rows.Next() {
			var r reindexItemRow
			var libID int64
			if err := rows.Scan(&r.itemRef, &r.seq, &libID, &r.title, &r.fieldsJSON); err != nil {
				rows.Close()
				return err
			}
			if libID > 0 {
				r.libraryID = core.LibraryID(libID)
			} else {
				r.libraryID = fallbackLibID
			}
			batch = append(batch, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(batch) == 0 {
			break
		}
		for _, r := range batch {
			indexed, searchFields, searchPeople := extractIndexData(decl, r.fieldsJSON)
			if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_fields WHERE item_ref = ?`, r.itemRef); err != nil {
				return err
			}
			for _, fv := range indexed {
				if hasFold {
					if _, err := tx.ExecContext(ctx,
						`INSERT INTO plugin_item_fields(item_ref, field, v_text, v_int, v_real, v_fold) VALUES(?, ?, ?, ?, ?, ?)
						 ON CONFLICT(item_ref, field, v_text, v_int) DO UPDATE SET v_real = excluded.v_real, v_fold = excluded.v_fold`,
						r.itemRef, fv.Field, fv.Text, fv.Int, fv.Real, fv.Fold); err != nil {
						return err
					}
				} else {
					if _, err := tx.ExecContext(ctx,
						`INSERT INTO plugin_item_fields(item_ref, field, v_text, v_int, v_real) VALUES(?, ?, ?, ?, ?)
						 ON CONFLICT(item_ref, field, v_text, v_int) DO UPDATE SET v_real = excluded.v_real`,
						r.itemRef, fv.Field, fv.Text, fv.Int, fv.Real); err != nil {
						return err
					}
				}
			}
			if isPG {
				if _, err := tx.ExecContext(ctx, `
UPDATE plugin_items
SET search_tsv = setweight(to_tsvector('simple', ?), 'A') || setweight(to_tsvector('simple', ?), 'B') || setweight(to_tsvector('simple', ?), 'C')
WHERE item_ref = ?`,
					r.title, searchFields, searchPeople, r.itemRef); err != nil {
					return err
				}
			} else {
				if err := sqliteIndexFTS(ctx, tx, r.seq, core.ItemRef(r.itemRef), r.libraryID, r.title, searchFields, searchPeople); err != nil {
					return err
				}
			}
		}
		cursor = batch[len(batch)-1].itemRef
		if len(batch) < batchSize {
			break
		}
	}
	dialect := store.SQLite
	if isPG {
		dialect = store.Postgres
	}
	if hasTableTx(ctx, tx, dialect, "plugin_item_field_tally") {
		if err := rebuildKindFieldTallyTx(ctx, tx, isPG, pluginID, kindKey, fallbackLibID); err != nil {
			return err
		}
	}
	if hasTableTx(ctx, tx, dialect, "plugin_item_parent_tally") {
		if err := rebuildKindParentTallyForKindTx(ctx, tx, pluginID, kindKey, fallbackLibID, decl); err != nil {
			return err
		}
	}
	return nil
}

func rebuildKindParentTallyForKindTx(ctx context.Context, tx *sql.Tx, pluginID, kindKey string, fallbackLibID core.LibraryID, decl plugins.MediaKindMeta) error {
	libSet := map[int64]bool{}
	if fallbackLibID > 0 {
		libSet[int64(fallbackLibID)] = true
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT library_id FROM plugin_items WHERE plugin_id = ? AND kind_key = ? AND library_id > 0`,
		pluginID, kindKey)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		libSet[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	libs := make([]int64, 0, len(libSet))
	for id := range libSet {
		libs = append(libs, id)
	}
	slices.Sort(libs)
	for _, libID := range libs {
		if err := rebuildKindParentTallyTx(ctx, tx, core.LibraryID(libID), decl); err != nil {
			return err
		}
	}
	return nil
}

func rebuildKindFieldTallyTx(ctx context.Context, tx *sql.Tx, isPG bool, pluginID, kindKey string, fallbackLibID core.LibraryID) error {
	libSet := map[int64]bool{}
	if fallbackLibID > 0 {
		libSet[int64(fallbackLibID)] = true
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT library_id FROM plugin_items WHERE plugin_id = ? AND kind_key = ? AND library_id > 0`,
		pluginID, kindKey)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		libSet[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	dialect := store.SQLite
	if isPG {
		dialect = store.Postgres
	}
	hasPersonTally := hasTableTx(ctx, tx, dialect, "plugin_item_person_tally")
	hasRepCols := hasColumnTx(ctx, tx, dialect, "plugin_item_field_tally", "rep_ref")

	libs := make([]int64, 0, len(libSet))
	for id := range libSet {
		libs = append(libs, id)
	}
	slices.Sort(libs)
	for _, libID := range libs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_field_tally WHERE library_id = ?`, libID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_item_field_tally(library_id, level, field, v_text, cnt)
SELECT i.library_id, i.level, f.field, f.v_text, COUNT(*)
FROM plugin_item_fields f
JOIN plugin_items i ON i.item_ref = f.item_ref
WHERE i.library_id = ? AND i.level <> '' AND f.v_text <> ''
GROUP BY i.library_id, i.level, f.field, f.v_text`, libID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_item_field_tally(library_id, level, field, v_text, cnt)
SELECT library_id, '', field, v_text, CAST(SUM(cnt) AS BIGINT)
FROM plugin_item_field_tally
WHERE library_id = ? AND level <> ''
GROUP BY library_id, field, v_text`, libID); err != nil {
			return err
		}
		if err := backfillFieldTallyCaseTx(ctx, tx, isPG, libID); err != nil {
			return err
		}
		if hasPersonTally {
			if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_person_tally WHERE library_id = ?`, libID); err != nil {
				return err
			}
			if err := backfillPersonTallyTx(ctx, tx, isPG, libID); err != nil {
				return err
			}
		}
		if hasRepCols {
			if err := backfillTallyRepsTx(ctx, tx, isPG, libID); err != nil {
				return err
			}
		}
	}
	return nil
}

func extractIndexData(decl plugins.MediaKindMeta, fieldsJSON string) ([]IndexedFieldValue, string, string) {
	if fieldsJSON == "" || fieldsJSON == "{}" {
		return nil, "", ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil || len(fields) == 0 {
		return nil, "", ""
	}
	declaredFields := make(map[string]plugins.KindField, len(decl.Fields))
	for _, f := range decl.Fields {
		declaredFields[f.Key] = f
	}
	searchFieldSet := make(map[string]bool, len(decl.Browse.SearchFields))
	for _, sf := range decl.Browse.SearchFields {
		searchFieldSet[sf] = true
	}
	var (
		indexed     []IndexedFieldValue
		searchParts []string
		peopleParts []string
	)
	fieldKeys := make([]string, 0, len(fields))
	for k := range fields {
		fieldKeys = append(fieldKeys, k)
	}
	slices.Sort(fieldKeys)
	for _, k := range fieldKeys {
		raw := fields[k]
		fdecl, exists := declaredFields[k]
		if !exists {
			continue
		}
		vals, texts, err := validateFieldValue(fdecl, raw)
		if err != nil {
			continue
		}
		indexed = append(indexed, vals...)
		if isPeopleIndexedField(fdecl) {
			if len(texts) > 0 {
				peopleParts = append(peopleParts, strings.Join(texts, " "))
			}
		}
		if fdecl.Type != "people" && searchFieldSet[k] && len(texts) > 0 {
			searchParts = append(searchParts, strings.Join(texts, " "))
		}
	}
	searchText := strings.Join(searchParts, " ")
	if len(searchText) > MaxFTSTextBytes {
		searchText = clipUTF8(searchText, MaxFTSTextBytes)
	}
	peopleText := strings.Join(peopleParts, " ")
	if len(peopleText) > MaxFTSTextBytes {
		peopleText = clipUTF8(peopleText, MaxFTSTextBytes)
	}
	return indexed, searchText, peopleText
}

type existingItemMeta struct {
	seq             int64
	level           string
	parentRef       core.ItemRef
	addedAt         int64
	released        int64
	year            int64
	hash            string
	recording       string
	presenceCounted bool
}

func lookupExistingItems(ctx context.Context, tx queryer, items []preparedItem) (map[core.ItemRef]existingItemMeta, error) {
	out := make(map[core.ItemRef]existingItemMeta, len(items))
	const chunkSize = 250
	for start := 0; start < len(items); start += chunkSize {
		end := min(start+chunkSize, len(items))
		chunk := items[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, v := range chunk {
			placeholders[i] = "?"
			args[i] = string(v.validated.Item.Ref)
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT item_ref, seq, level, parent_ref, added_at, released, year, content_hash, recording, presence_counted = 1 FROM plugin_items WHERE item_ref IN (`+strings.Join(placeholders, ",")+`)`,
			args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var refStr, lv, parentStr, hash, rec string
			var seq, added, released, yr int64
			var presenceCounted bool
			if err := rows.Scan(&refStr, &seq, &lv, &parentStr, &added, &released, &yr, &hash, &rec, &presenceCounted); err != nil {
				rows.Close()
				return nil, err
			}
			out[core.ItemRef(refStr)] = existingItemMeta{
				seq:             seq,
				level:           lv,
				parentRef:       core.ItemRef(parentStr),
				addedAt:         added,
				released:        released,
				year:            yr,
				hash:            hash,
				recording:       rec,
				presenceCounted: presenceCounted,
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func (s *Service) lookupLevelsByExternalIDs(ctx context.Context, pluginID, kindKey string, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(ids))
	const chunkSize = 250
	for start := 0; start < len(ids); start += chunkSize {
		end := min(start+chunkSize, len(ids))
		chunk := ids[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, 2+len(chunk))
		args = append(args, pluginID, kindKey)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT external_id, level FROM plugin_items WHERE plugin_id = ? AND kind_key = ? AND external_id IN (`+strings.Join(placeholders, ",")+`)`,
			args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var extID, lv string
			if err := rows.Scan(&extID, &lv); err != nil {
				rows.Close()
				return nil, err
			}
			out[extID] = lv
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// allocateSeqRangeTx reserves count sequence numbers in plugin_items_seq
// and returns the first reserved sequence number. Sequence numbers are
// never decremented or reused across deletes or plugin removals (spec §2.2).
func allocateSeqRangeTx(ctx context.Context, tx *sql.Tx, count int64) (int64, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE plugin_items_seq SET next = next + ? WHERE id = 1`, count); err != nil {
		return 0, err
	}
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT next FROM plugin_items_seq WHERE id = 1`).Scan(&next); err != nil {
		return 0, err
	}
	first := next - count
	if first+count >= core.PluginKeyMaxSeq {
		return 0, errors.New("pluginitems: PluginKey sequence space exhausted")
	}
	return first, nil
}

func applyTallyDeltaTx(ctx context.Context, tx *sql.Tx, libraryID core.LibraryID, delta map[string]int64) error {
	for level, d := range delta {
		if d == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_item_tally(library_id, level, n) VALUES(?, ?, ?)
ON CONFLICT(library_id, level) DO UPDATE SET n = CASE
	WHEN plugin_item_tally.n + excluded.n < 0 THEN 0
	ELSE plugin_item_tally.n + excluded.n
END`, int64(libraryID), level, d); err != nil {
			return err
		}
	}
	return nil
}

type fieldTallyKey struct {
	level string
	field string
	text  string
}

type existingTextField struct {
	ref   core.ItemRef
	field string
	text  string
	intV  int64
}

func lookupExistingTextFields(ctx context.Context, q queryer, refs []core.ItemRef) ([]existingTextField, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	var out []existingTextField
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
		rows, err := q.QueryContext(ctx,
			`SELECT item_ref, field, v_text, v_int FROM plugin_item_fields WHERE item_ref IN (`+strings.Join(placeholders, ",")+`) AND v_text <> ''`,
			args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var refStr, field, text string
			var intV int64
			if err := rows.Scan(&refStr, &field, &text, &intV); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, existingTextField{ref: core.ItemRef(refStr), field: field, text: text, intV: intV})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// bumpKindSyncStateTx records a new generation and the kind's item
// count from the per-level tally (maintained in the same transactions as
// the items), never a COUNT(*) over the kind's items (#646).
func (s *Service) bumpKindSyncStateTx(ctx context.Context, tx *sql.Tx, pluginID, kindKey string, libraryID core.LibraryID, nowUnix int64, markOK bool) (int64, error) {
	var totalItems int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(n), 0) FROM plugin_item_tally WHERE library_id = ?`,
		int64(libraryID)).Scan(&totalItems); err != nil {
		return 0, err
	}
	gen := int64(s.syncGen.Add(1))
	if !markOK {
		_, err := tx.ExecContext(ctx, `
INSERT INTO plugin_sync_state(plugin_id, kind_key, generation, last_started, last_ok, last_error, items)
VALUES(?, ?, ?, 0, 0, '', ?)
ON CONFLICT(plugin_id, kind_key) DO UPDATE SET
	generation = excluded.generation,
	items      = excluded.items`,
			pluginID, kindKey, gen, totalItems)
		return totalItems, err
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO plugin_sync_state(plugin_id, kind_key, generation, last_started, last_ok, last_error, guard_json, items)
VALUES(?, ?, ?, 0, ?, '', '', ?)
ON CONFLICT(plugin_id, kind_key) DO UPDATE SET
	generation = excluded.generation,
	last_ok    = excluded.last_ok,
	last_error = '', failure_json = '',
	guard_json = '',
	items      = excluded.items`,
		pluginID, kindKey, gen, nowUnix, totalItems)
	return totalItems, err
}

// KindDelete implements the store side of plugins.KindBackends.KindDelete
// (spec §3.6).
func (s *Service) KindDelete(ctx context.Context, pluginID, kindKey string, externalIDs []string) error {
	return s.DeleteItems(ctx, pluginID, kindKey, externalIDs)
}

// DeleteItems deletes the named externalIDs (and any descendant items) from
// (pluginID, kindKey), maintaining tallies, FTS, matches and resolutions.
func (s *Service) DeleteItems(ctx context.Context, pluginID, kindKey string, externalIDs []string) error {
	mu := s.kindLock(pluginID, kindKey)
	mu.Lock()
	defer mu.Unlock()
	return s.deleteItemsLocked(ctx, pluginID, kindKey, externalIDs, true)
}

func (s *Service) deleteItemsLocked(ctx context.Context, pluginID, kindKey string, externalIDs []string, bumpGen bool) error {
	kind, err := s.GetKind(ctx, pluginID, kindKey)
	if err != nil {
		return err
	}
	if pending, err := s.pendingKindRebuild(ctx, pluginID, kindKey); err != nil {
		return err
	} else if pending != "" {
		s.StartManualSync(pluginID, kindKey, SyncOptions{rebuildOnly: true})
		return ErrKindRebuildPending
	}
	if len(externalIDs) == 0 {
		return nil
	}
	refs := make([]core.ItemRef, 0, len(externalIDs))
	seen := make(map[core.ItemRef]bool, len(externalIDs))
	for _, id := range externalIDs {
		ref := core.NewItemRef(pluginID, kindKey, id)
		if !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}

	leafLevels := kind.Decl.LeafLevels()
	levelCount := len(kind.Decl.Levels)
	targets, err := s.loadDeleteTargets(ctx, refs, levelCount, leafLevels)
	if err != nil {
		return err
	}

	// Commit children before their parents. If interrupted, a replay can
	// still discover every remaining descendant from the original roots.
	depth := func(t deleteTarget) int {
		n := 0
		for parent, ok := targets[t.parentRef]; ok && n < levelCount; parent, ok = targets[parent.parentRef] {
			n++
		}
		return n
	}
	ordered := make([]deleteTarget, 0, len(targets))
	for _, t := range targets {
		ordered = append(ordered, t)
	}
	slices.SortFunc(ordered, func(a, b deleteTarget) int {
		if d := depth(b) - depth(a); d != 0 {
			return d
		}
		return strings.Compare(string(a.ref), string(b.ref))
	})

	// Keep the state update in the last successful transaction, including
	// for a tombstone that was already applied.
	now := s.now().Unix()
	isSQLite := store.DialectOf(s.db) == store.SQLite
	write := func(start, end int) (time.Duration, error) {
		var finish func(context.Context, *sql.Tx) error
		if bumpGen && end == len(ordered) {
			finish = func(ctx context.Context, tx *sql.Tx) error {
				_, err := s.bumpKindSyncStateTx(ctx, tx, pluginID, kindKey, kind.LibraryID, now, true)
				return err
			}
		}
		return s.writeDeleteChunk(ctx, kind, ordered[start:end], isSQLite, leafLevels, false, finish)
	}
	if len(ordered) == 0 {
		if bumpGen {
			_, err = write(0, 0)
		}
	} else {
		err = s.writeChunks(ctx, len(ordered), write)
	}
	if err != nil {
		return err
	}
	s.forgetPinnedRefs(refs)
	return nil
}

// writeDeleteChunk shares the normal deletion plan and writer budget with a
// rebuild. A rebuild saves the item and part keys in the same transaction.
func (s *Service) writeDeleteChunk(ctx context.Context, kind KindRecord, chunk []deleteTarget, isSQLite bool, leafLevels map[string]bool, rebuild bool, finish func(context.Context, *sql.Tx) error) (time.Duration, error) {
	var plan *deleteChunkPlan
	if isSQLite && len(chunk) > 0 {
		var err error
		plan, err = prepareDeleteChunk(ctx, s.db, kind, chunk, leafLevels)
		if err != nil {
			return 0, err
		}
		if err := prepareTallyRepairs(ctx, s.db, kind.LibraryID, kind.Decl, plan.fieldTally, plan.personTally); err != nil {
			return 0, err
		}
	}
	held, err := s.runWriteChunk(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := lockFieldPresence(ctx, tx, !isSQLite, kind.LibraryID); err != nil {
			return err
		}
		if len(chunk) > 0 {
			if plan == nil {
				var err error
				plan, err = prepareDeleteChunk(ctx, tx, kind, chunk, leafLevels)
				if err != nil {
					return err
				}
			}
			if rebuild {
				if err := saveRebuildKeys(ctx, tx, kind.LibraryID, isSQLite, chunk); err != nil {
					return err
				}
			}
			if err := deleteTargetsTx(ctx, tx, kind, isSQLite, chunk, plan); err != nil {
				return err
			}
		}
		if finish != nil {
			return finish(ctx, tx)
		}
		return nil
	})
	if err == nil {
		s.forgetPinnedRefs(refsFromDeleteTargets(chunk))
		s.noteWrite()
	}
	return held, err
}

type deleteTarget struct {
	ref       core.ItemRef
	seq       int64
	level     string
	parentRef core.ItemRef
	recording string
}

func refsFromDeleteTargets(targets []deleteTarget) []core.ItemRef {
	refs := make([]core.ItemRef, len(targets))
	for i, t := range targets {
		refs[i] = t.ref
	}
	return refs
}

func (s *Service) loadDeleteTargets(ctx context.Context, rootRefs []core.ItemRef, levelCount int, leafLevels map[string]bool) (map[core.ItemRef]deleteTarget, error) {
	allTargets := make(map[core.ItemRef]deleteTarget, len(rootRefs))
	frontier := append([]core.ItemRef(nil), rootRefs...)

	// Walk up to 5 hierarchy levels to collect all targets and their descendants.
	for depth := 0; depth < 6 && len(frontier) > 0; depth++ {
		var nextFrontier []core.ItemRef
		const chunkSize = 250
		for start := 0; start < len(frontier); start += chunkSize {
			end := min(start+chunkSize, len(frontier))
			chunk := frontier[start:end]
			placeholders := make([]string, len(chunk))
			args := make([]any, len(chunk))
			for i, r := range chunk {
				placeholders[i] = "?"
				args[i] = string(r)
			}
			inClause := strings.Join(placeholders, ",")
			if depth == 0 {
				rows, err := s.db.QueryContext(ctx,
					`SELECT item_ref, seq, level, parent_ref, recording FROM plugin_items WHERE item_ref IN (`+inClause+`)`, args...)
				if err != nil {
					return nil, err
				}
				for rows.Next() {
					var refStr, lv, parentStr, recording string
					var seq int64
					if err := rows.Scan(&refStr, &seq, &lv, &parentStr, &recording); err != nil {
						rows.Close()
						return nil, err
					}
					r := core.ItemRef(refStr)
					allTargets[r] = deleteTarget{ref: r, seq: seq, level: lv, parentRef: core.ItemRef(parentStr), recording: recording}
				}
				if err := rows.Err(); err != nil {
					rows.Close()
					return nil, err
				}
				rows.Close()
			}
			// Find direct children of this chunk. A single-level kind
			// has none, and an item at the deepest level cannot be a
			// parent, so neither reaches the database. The lookup
			// itself must stay an index seek on a flat kind, where
			// every row's parent_ref is '' and planner statistics
			// otherwise talk SQLite into scanning the whole table
			// (#647): parent_ref <> '' excludes that bucket on both
			// dialects, and SQLite is pinned to the parent index.
			if levelCount == 1 {
				continue
			}
			parents := chunk
			parentArgs := args
			if len(leafLevels) > 0 {
				parents = make([]core.ItemRef, 0, len(chunk))
				parentArgs = parentArgs[:0]
				for _, r := range chunk {
					if t, known := allTargets[r]; known && leafLevels[t.level] {
						continue
					}
					parents = append(parents, r)
					parentArgs = append(parentArgs, string(r))
				}
				if len(parents) == 0 {
					continue
				}
				placeholders = make([]string, len(parents))
				for i := range parents {
					placeholders[i] = "?"
				}
				inClause = strings.Join(placeholders, ",")
			}
			childrenQuery := `SELECT item_ref, seq, level, parent_ref, recording FROM plugin_items WHERE parent_ref IN (` + inClause + `) AND parent_ref <> ''`
			if store.DialectOf(s.db) == store.SQLite {
				childrenQuery = `SELECT item_ref, seq, level, parent_ref, recording FROM plugin_items INDEXED BY plugin_items_parent WHERE parent_ref IN (` + inClause + `) AND parent_ref <> ''`
			}
			crows, err := s.db.QueryContext(ctx, childrenQuery, parentArgs...)
			if err != nil {
				return nil, err
			}
			for crows.Next() {
				var refStr, lv, parentStr, recording string
				var seq int64
				if err := crows.Scan(&refStr, &seq, &lv, &parentStr, &recording); err != nil {
					crows.Close()
					return nil, err
				}
				r := core.ItemRef(refStr)
				if _, already := allTargets[r]; !already {
					allTargets[r] = deleteTarget{ref: r, seq: seq, level: lv, parentRef: core.ItemRef(parentStr), recording: recording}
					nextFrontier = append(nextFrontier, r)
				}
			}
			if err := crows.Err(); err != nil {
				crows.Close()
				return nil, err
			}
			crows.Close()
		}
		frontier = nextFrontier
	}

	return allTargets, nil
}

type deleteChunkPlan struct {
	refs        []core.ItemRef
	fieldTally  map[fieldTallyKey]*fieldTallyChange
	personTally map[personTallyKey]*personTallyChange
	parentTally map[parentTallyKey]parentTallyDelta
	levelTally  map[string]int64
	recordings  map[string]int64
}

func prepareDeleteChunk(ctx context.Context, q queryer, kind KindRecord, targets []deleteTarget, leafLevels map[string]bool) (*deleteChunkPlan, error) {
	libraryID := kind.LibraryID
	levelCount := len(kind.Decl.Levels)
	targetRefs := refsFromDeleteTargets(targets)
	allTargets := make(map[core.ItemRef]deleteTarget, len(targets))
	for _, t := range targets {
		allTargets[t.ref] = t
	}

	var parentTally *parentTallyTracker
	if levelCount > 1 && libraryID > 0 {
		parentTally = newParentTallyTracker(q, libraryID, leafLevels, nil)
		skipSet := make(map[core.ItemRef]struct{}, len(allTargets))
		for r := range allTargets {
			skipSet[r] = struct{}{}
		}
		cutRoots := make([]deleteTarget, 0, len(targets))
		primeParents := make([]core.ItemRef, 0, len(targets))
		primeLeaves := make([]core.ItemRef, 0, len(targets))
		for _, r := range targetRefs {
			t := allTargets[r]
			if t.parentRef == "" {
				continue
			}
			if _, deletingParent := allTargets[t.parentRef]; deletingParent {
				continue
			}
			cutRoots = append(cutRoots, t)
			primeParents = append(primeParents, t.parentRef)
			primeLeaves = append(primeLeaves, t.parentRef)
			if !leafLevels[t.level] {
				primeLeaves = append(primeLeaves, t.ref)
			}
		}
		if len(cutRoots) > 0 {
			if err := parentTally.primeParents(ctx, primeParents); err != nil {
				return nil, err
			}
			if err := parentTally.primeLeaves(ctx, primeLeaves); err != nil {
				return nil, err
			}
			for _, t := range cutRoots {
				var childLeaves int64
				if !leafLevels[t.level] {
					var err error
					childLeaves, err = parentTally.leafCount(ctx, t.ref)
					if err != nil {
						return nil, err
					}
				}
				sub := parentTally.ownLeaf(t.level) + childLeaves
				parentTally.addChild(t.parentRef, t.level, -1)
				if sub > 0 {
					if err := parentTally.addLeafAncestors(ctx, t.parentRef, t.ref, t.level, -sub, skipSet); err != nil {
						return nil, err
					}
				}
			}
		}
	}

	oldFields, err := lookupExistingTextFields(ctx, q, targetRefs)
	if err != nil {
		return nil, err
	}
	fieldTallyDelta := map[fieldTallyKey]*fieldTallyChange{}
	personTallyDelta := map[personTallyKey]*personTallyChange{}
	for _, of := range oldFields {
		if t, ok := allTargets[of.ref]; ok {
			refStr := string(of.ref)
			addFieldTallyChange(fieldTallyDelta, t.level, of.field, of.text, -1, tallyItemRank{}, refStr)
			if core.IsPluginPersonID(of.intV) {
				addPersonTallyChange(personTallyDelta, t.level, of.field, of.intV, -1, "", of.text, tallyItemRank{}, refStr, tallyItemRank{}, refStr, refStr)
			}
		}
	}

	plan := &deleteChunkPlan{
		refs: targetRefs, fieldTally: fieldTallyDelta, personTally: personTallyDelta,
		levelTally: make(map[string]int64), recordings: make(map[string]int64),
	}
	if parentTally != nil {
		plan.parentTally = parentTally.delta
	}
	for _, t := range targets {
		plan.levelTally[t.level]--
		if t.recording != "" {
			plan.recordings[t.recording]--
		}
	}
	return plan, nil
}

func deleteTargetsTx(ctx context.Context, tx *sql.Tx, kind KindRecord, isSQLite bool, targets []deleteTarget, plan *deleteChunkPlan) error {
	libraryID, decl := kind.LibraryID, kind.Decl
	presence := fieldPresenceDelta{isPG: !isSQLite}
	if err := presence.collect(ctx, tx, plan.refs, -1); err != nil {
		return err
	}
	for _, t := range targets {
		if isSQLite {
			if err := sqliteDeleteFTS(ctx, tx, t.seq); err != nil {
				return err
			}
		}
		refStr := string(t.ref)
		partPrefix := refStr + "#part:"
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_resolutions WHERE item_ref = ?`, refStr); err != nil {
			return err
		}
		// Resolution keys use binary collation on both databases. The range
		// seeks to this item's part keys through the primary index.
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_resolutions WHERE item_ref >= ? AND item_ref < ?`, partPrefix, refStr+"#part;"); err != nil {
			return err
		}
		for _, tbl := range []string{
			"plugin_item_fields",
			"plugin_item_art",
			"plugin_item_parts",
			"plugin_item_members",
			"plugin_matches",
			"plugin_items",
		} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+tbl+` WHERE item_ref = ?`, refStr); err != nil {
				return err
			}
		}
	}
	if err := presence.finish(ctx, tx, nil); err != nil {
		return err
	}
	if err := applyTallyDeltaTx(ctx, tx, libraryID, plan.levelTally); err != nil {
		return err
	}
	if err := applyRecordingTallyDeltaTx(ctx, tx, libraryID, plan.recordings); err != nil {
		return err
	}
	emptiedText, err := applyFieldTallyDeltaTx(ctx, tx, !isSQLite, libraryID, plan.fieldTally)
	if err != nil {
		return err
	}
	if err := applyPersonTallyDeltaTx(ctx, tx, !isSQLite, libraryID, decl, plan.personTally, emptiedText); err != nil {
		return err
	}
	if plan.parentTally != nil {
		if err := deleteParentTallyRefsTx(ctx, tx, libraryID, plan.refs); err != nil {
			return err
		}
		if err := applyParentTallyDeltaTx(ctx, tx, libraryID, plan.parentTally); err != nil {
			return err
		}
	}
	return nil
}

// KindItems implements the store side of plugins.KindBackends.KindItems
// (spec §3.6): reads the plugin's own library back, keyset-paged across all
// levels by external_id.
func (s *Service) KindItems(ctx context.Context, pluginID, kindKey, cursor string, limit int) (core.KindPage, error) {
	if _, err := s.GetKind(ctx, pluginID, kindKey); err != nil {
		return core.KindPage{}, err
	}
	if limit <= 0 {
		limit = DefaultBrowseLimit
	}
	if limit > MaxBrowseLimit {
		limit = MaxBrowseLimit
	}
	items, err := s.queryItems(ctx, `
SELECT `+itemCols+`
FROM plugin_items
WHERE plugin_id = ? AND kind_key = ? AND external_id > ?
ORDER BY external_id ASC
LIMIT ?`,
		[]any{pluginID, kindKey, cursor, limit + 1}, true)
	if err != nil {
		return core.KindPage{}, err
	}
	var nextCursor string
	if len(items) > limit {
		items = items[:limit]
		nextCursor = items[len(items)-1].ID
	}
	total, err := s.countKindItems(ctx, pluginID, kindKey)
	if err != nil {
		return core.KindPage{}, err
	}
	return core.KindPage{
		Items:      items,
		NextCursor: nextCursor,
		Total:      &total,
	}, nil
}

// KindItemsByID implements the store side of plugins.KindBackends.KindItemsByID:
// fetches only the matching stored items for (pluginID, kindKey) by external_id
// via primary-key lookups on item_ref, hydrating art only for matching rows.
func (s *Service) KindItemsByID(ctx context.Context, pluginID, kindKey string, externalIDs []string) ([]core.KindItem, error) {
	if _, err := s.GetKind(ctx, pluginID, kindKey); err != nil {
		return nil, err
	}
	if len(externalIDs) == 0 {
		return nil, nil
	}
	refs := make([]string, 0, len(externalIDs))
	seen := make(map[string]bool, len(externalIDs))
	for _, id := range externalIDs {
		if !plugins.ValidExternalID(id) {
			continue
		}
		ref := string(core.NewItemRef(pluginID, kindKey, id))
		if !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return nil, nil
	}
	var out []core.KindItem
	const chunkSize = 500
	for start := 0; start < len(refs); start += chunkSize {
		end := min(start+chunkSize, len(refs))
		chunk := refs[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, r := range chunk {
			placeholders[i] = "?"
			args[i] = r
		}
		items, err := s.queryItems(ctx,
			`SELECT `+itemCols+` FROM plugin_items WHERE item_ref IN (`+strings.Join(placeholders, ",")+`)`,
			args, true)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	slices.SortFunc(out, func(a, b core.KindItem) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// GetByRef returns one stored KindItem and its LibraryID by ItemRef.
func (s *Service) GetByRef(ctx context.Context, ref core.ItemRef) (core.KindItem, core.LibraryID, error) {
	return s.getOne(ctx, `SELECT `+itemCols+` FROM plugin_items WHERE item_ref = ?`, string(ref))
}

// GetByKey returns one stored KindItem and its LibraryID by its PluginKey RatingKey.
func (s *Service) GetByKey(ctx context.Context, key core.RatingKey) (core.KindItem, core.LibraryID, error) {
	seq := key.PluginSeq()
	if seq <= 0 {
		return core.KindItem{}, 0, core.ErrNotFound
	}
	return s.getOne(ctx, `SELECT `+itemCols+` FROM plugin_items WHERE seq = ?`, seq)
}

// GetByExternal returns one stored KindItem and its LibraryID by (pluginID, kindKey, externalID).
func (s *Service) GetByExternal(ctx context.Context, pluginID, kindKey, externalID string) (core.KindItem, core.LibraryID, error) {
	return s.GetByRef(ctx, core.NewItemRef(pluginID, kindKey, externalID))
}

func (s *Service) getOne(ctx context.Context, query string, arg any) (core.KindItem, core.LibraryID, error) {
	it, libID, _, err := scanItemRow(s.db.QueryRowContext(ctx, query, arg))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.KindItem{}, 0, core.ErrNotFound
		}
		return core.KindItem{}, 0, err
	}
	slice := []core.KindItem{it}
	if err := s.hydrateArtSlice(ctx, slice); err != nil {
		return core.KindItem{}, 0, err
	}
	return slice[0], libID, nil
}

// Children returns the direct children of parentRef ordered by (idx ASC, sort_title ASC, item_ref ASC).
func (s *Service) Children(ctx context.Context, parentRef core.ItemRef, cursor string, limit int) (BrowsePage, error) {
	_, libID, err := s.GetByRef(ctx, parentRef)
	if err != nil {
		return BrowsePage{}, err
	}
	return s.Browse(ctx, BrowseQuery{
		LibraryID: libID,
		ParentRef: parentRef,
		Sort:      "index",
		Cursor:    cursor,
		Limit:     limit,
	})
}

// LibraryTally returns the maintained per-level item counts for libraryID
// from plugin_item_tally (spec §2.3, §3.4).
// PluginItemCount returns the total stored item count across all libraries
// owned by pluginID, read in O(1) from plugin_item_tally.
func (s *Service) PluginItemCount(ctx context.Context, pluginID string) (int, error) {
	if s == nil || strings.TrimSpace(pluginID) == "" {
		return 0, nil
	}
	var total int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(t.n), 0)
		 FROM plugin_item_tally t
		 JOIN plugin_kinds pk ON pk.library_id = t.library_id
		 WHERE pk.plugin_id = ?`, pluginID).Scan(&total)
	if err != nil {
		return 0, err
	}
	return int(total), nil
}

func (s *Service) LibraryTally(ctx context.Context, libraryID core.LibraryID) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT level, n FROM plugin_item_tally WHERE library_id = ?`, int64(libraryID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var lv string
		var n int64
		if err := rows.Scan(&lv, &n); err != nil {
			return nil, err
		}
		out[lv] = int(n)
	}
	return out, rows.Err()
}

// SyncStateOf returns the current SyncState row for (pluginID, kindKey).
func (s *Service) SyncStateOf(ctx context.Context, pluginID, kindKey string) (SyncState, error) {
	var (
		st          SyncState
		gen         int64
		lastStarted int64
		lastOK      int64
		guardRaw    string
		failureRaw  string
	)
	err := s.db.QueryRowContext(ctx, `
SELECT plugin_id, kind_key, cursor, generation, last_started, last_ok, last_error, guard_json, items, failure_json
FROM plugin_sync_state
WHERE plugin_id = ? AND kind_key = ?`, pluginID, kindKey).Scan(
		&st.PluginID, &st.KindKey, &st.Cursor, &gen, &lastStarted, &lastOK, &st.LastError, &guardRaw, &st.Items, &failureRaw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SyncState{}, core.ErrNotFound
		}
		return SyncState{}, err
	}
	st.Generation = uint64(gen)
	if lastStarted > 0 {
		st.LastStarted = time.Unix(lastStarted, 0).UTC()
	}
	if lastOK > 0 {
		st.LastOK = time.Unix(lastOK, 0).UTC()
	}
	if failureRaw != "" {
		_ = json.Unmarshal([]byte(failureRaw), &st.Failure)
	}
	if guardRaw != "" {
		var g SyncDeleteGuard
		if json.Unmarshal([]byte(guardRaw), &g) == nil && g.Missing > 0 && g.Total > 0 {
			st.DeleteGuard = &g
		}
	}
	if st.DeleteGuard == nil && st.LastError != "" {
		if parsed := ParseDeleteGuardError(st.LastError); parsed != nil {
			st.DeleteGuard = parsed
		}
	}
	if prog, ok := s.getSyncProgress(pluginID, kindKey); ok && prog.running {
		st.Running = true
		st.Phase = prog.phase
		st.SyncedItems = prog.syncedItems
		st.Pages = prog.pages
		if int64(prog.itemsTotal) > st.Items {
			st.Items = int64(prog.itemsTotal)
		}
	}
	if out, ok := s.getSyncOutcome(pluginID, kindKey); ok {
		outCopy := out
		st.LastOutcome = &outCopy
	}
	return st, nil
}

// PurgeKind removes all items, fields, art, tallies, matches, resolutions,
// sync state and kind registration for one (pluginID, kindKey) (spec §2.5).
func (s *Service) PurgeKind(ctx context.Context, pluginID, kindKey string) error {
	s.StopKindSync(pluginID, kindKey)
	mu := s.kindLock(pluginID, kindKey)
	mu.Lock()
	defer mu.Unlock()

	var libID int64
	_ = s.db.QueryRowContext(ctx,
		`SELECT library_id FROM plugin_kinds WHERE plugin_id = ? AND kind_key = ?`,
		pluginID, kindKey).Scan(&libID)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := s.purgeKindTx(ctx, tx, pluginID, kindKey, core.LibraryID(libID)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidateKindCache(pluginID, kindKey, core.LibraryID(libID))
	s.partsState.Store(0)
	if libID > 0 {
		s.forgetLibraryStats(core.LibraryID(libID))
	}
	s.clearPinnedCache()
	s.noteWrite()
	s.syncGen.Add(1)
	return nil
}

// PurgePlugin is the idempotent removal sequence for all kinds owned by
// pluginID (spec §2.5): stops sync goroutines, deletes watch-state rows for
// the plugin's RatingKeys, and purges items, fields, art, FTS, tallies,
// matches, resolutions, sync state and plugin_kinds rows.
func (s *Service) PurgePlugin(ctx context.Context, pluginID string) error {
	s.StopPluginSync(pluginID)

	kinds, err := s.ListKinds(ctx, pluginID)
	if err != nil {
		return err
	}
	for _, k := range kinds {
		mu := s.kindLock(pluginID, k.KindKey)
		mu.Lock()
		defer mu.Unlock()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, k := range kinds {
		if err := s.purgeKindTx(ctx, tx, pluginID, k.KindKey, k.LibraryID); err != nil {
			return err
		}
	}
	// Catch any orphan rows for pluginID even if plugin_kinds was already empty.
	if err := s.purgeKindTx(ctx, tx, pluginID, "", 0); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidatePluginKindCache(pluginID)
	s.partsState.Store(0)
	for _, k := range kinds {
		if k.LibraryID > 0 {
			s.forgetLibraryStats(k.LibraryID)
		}
	}
	s.clearPinnedCache()
	s.noteWrite()
	s.syncGen.Add(1)
	return nil
}

func (s *Service) purgeKindTx(ctx context.Context, tx *sql.Tx, pluginID, kindKey string, libraryID core.LibraryID) error {
	if err := lockFieldPresence(ctx, tx, store.DialectOf(s.db) == store.Postgres, libraryID); err != nil {
		return err
	}
	whereItems := `plugin_id = ? AND kind_key = ?`
	args := []any{pluginID, kindKey}
	if kindKey == "" {
		whereItems = `plugin_id = ?`
		args = []any{pluginID}
	}

	// Delete watch-state rows for the plugin's RatingKeys by seq range when
	// state_watch is present in the schema (spec §2.5).
	var minSeq, maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MIN(seq), MAX(seq) FROM plugin_items WHERE `+whereItems, args...).Scan(&minSeq, &maxSeq); err != nil {
		return err
	}
	if minSeq.Valid && maxSeq.Valid && minSeq.Int64 > 0 {
		minKey := int64(core.PluginKey(minSeq.Int64))
		maxKey := int64(core.PluginKey(maxSeq.Int64))
		offset := int64(core.PluginKey(1)) - 1
		if hasTableTx(ctx, tx, store.DialectOf(s.db), "state_watch") {
			delArgs := append([]any{minKey, maxKey, offset}, args...)
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM state_watch WHERE rating_key BETWEEN ? AND ? AND rating_key IN (SELECT ? + seq FROM plugin_items WHERE `+whereItems+`)`,
				delArgs...); err != nil {
				return err
			}
			partArgs := append([]any{offset}, args...)
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM state_watch WHERE rating_key IN (SELECT ? + seq FROM plugin_item_parts WHERE item_ref IN (SELECT item_ref FROM plugin_items WHERE `+whereItems+`))`,
				partArgs...); err != nil {
				return err
			}
		}
	}

	if store.DialectOf(s.db) == store.SQLite {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM plugin_item_fts WHERE rowid IN (SELECT seq FROM plugin_items WHERE `+whereItems+`)`,
			args...); err != nil {
			return err
		}
	}
	for _, tbl := range []string{
		"plugin_item_fields",
		"plugin_item_art",
		"plugin_item_parts",
		"plugin_item_members",
		"plugin_matches",
	} {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+tbl+` WHERE item_ref IN (SELECT item_ref FROM plugin_items WHERE `+whereItems+`)`,
			args...); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM plugin_resolutions WHERE EXISTS (SELECT 1 FROM plugin_items WHERE (`+whereItems+`) AND (plugin_resolutions.item_ref = plugin_items.item_ref OR SUBSTR(plugin_resolutions.item_ref, 1, LENGTH(plugin_items.item_ref) + 6) = plugin_items.item_ref || '#part:'))`,
		args...); err != nil {
		return err
	}
	if libraryID > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_field_presence WHERE library_id = ?`, int64(libraryID)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_tally WHERE library_id = ?`, int64(libraryID)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_field_tally WHERE library_id = ?`, int64(libraryID)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_person_tally WHERE library_id = ?`, int64(libraryID)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_parent_tally WHERE library_id = ?`, int64(libraryID)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_recording_tally WHERE library_id = ?`, int64(libraryID)); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM plugin_item_field_presence WHERE library_id IN (SELECT DISTINCT library_id FROM plugin_items WHERE `+whereItems+`)`,
			args...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM plugin_item_tally WHERE library_id IN (SELECT DISTINCT library_id FROM plugin_items WHERE `+whereItems+`)`,
			args...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM plugin_item_field_tally WHERE library_id IN (SELECT DISTINCT library_id FROM plugin_items WHERE `+whereItems+`)`,
			args...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM plugin_item_person_tally WHERE library_id IN (SELECT DISTINCT library_id FROM plugin_items WHERE `+whereItems+`)`,
			args...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM plugin_item_parent_tally WHERE library_id IN (SELECT DISTINCT library_id FROM plugin_items WHERE `+whereItems+`)`,
			args...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM plugin_item_recording_tally WHERE library_id IN (SELECT DISTINCT library_id FROM plugin_items WHERE `+whereItems+`)`,
			args...); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_items WHERE `+whereItems, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_facet_expansions WHERE `+whereItems, args...); err != nil {
		return err
	}
	if libraryID > 0 {
		if hasTableTx(ctx, tx, store.DialectOf(s.db), "state_watch") {
			offset := int64(core.PluginKey(1)) - 1
			if _, err := tx.ExecContext(ctx, `DELETE FROM state_watch WHERE rating_key IN (SELECT ? + seq FROM plugin_rebuild_keys WHERE library_id = ?)`, offset, int64(libraryID)); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_rebuild_keys WHERE library_id = ?`, int64(libraryID)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_sync_state WHERE `+whereItems, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_kind_rebuilds WHERE `+whereItems, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_kinds WHERE `+whereItems, args...); err != nil {
		return err
	}
	return nil
}

func hasTableTx(ctx context.Context, tx *sql.Tx, d store.Dialect, table string) bool {
	var n int
	if d == store.Postgres {
		_ = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = ?`,
			table).Scan(&n)
		return n > 0
	}
	_ = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`,
		table).Scan(&n)
	return n > 0
}

func hasColumnTx(ctx context.Context, tx *sql.Tx, d store.Dialect, table, column string) bool {
	var n int
	if d == store.Postgres {
		_ = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
			table, column).Scan(&n)
		return n > 0
	}
	_ = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
		table, column).Scan(&n)
	return n > 0
}

// ResolutionCacheMaxTTL is the maximum positive cache window for a resolved
// PlayableSource (spec §4.4: min(expiresAt, now+15m)).
const (
	ResolutionCacheMaxTTL      = 15 * time.Minute
	ResolutionNegativeCacheTTL = 60 * time.Second
)

func resolutionStorageKey(ref core.ItemRef, partIdx *int) string {
	if partIdx == nil {
		return string(ref)
	}
	return fmt.Sprintf("%s#part:%d", ref, *partIdx)
}

// SaveResolution caches a successful PlayableSource for ref until
// min(src.ExpiresAt, now+15m) (spec §4.4).
func (s *Service) SaveResolution(ctx context.Context, ref core.ItemRef, src core.PlayableSource) error {
	return s.SavePartResolution(ctx, ref, nil, src)
}

// SavePartResolution caches a successful PlayableSource for ref and optional
// zero-based partIdx until min(src.ExpiresAt, now+15m) (spec §4.4, #576).
func (s *Service) SavePartResolution(ctx context.Context, ref core.ItemRef, partIdx *int, src core.PlayableSource) error {
	now := s.now()
	exp := now.Add(ResolutionCacheMaxTTL).Unix()
	if src.ExpiresAt > 0 && src.ExpiresAt < exp {
		exp = src.ExpiresAt
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO plugin_resolutions(item_ref, pattern, source, negative, expires_at)
VALUES(?, ?, ?, 0, ?)
ON CONFLICT(item_ref) DO UPDATE SET
	pattern    = excluded.pattern,
	source     = excluded.source,
	negative   = 0,
	expires_at = excluded.expires_at`,
		resolutionStorageKey(ref, partIdx), src.Pattern, string(raw), exp)
	return err
}

// SaveNegativeResolution caches a not_available resolution result for 60s so
// a dead kind cannot stampede the plugin (spec §4.4).
func (s *Service) SaveNegativeResolution(ctx context.Context, ref core.ItemRef) error {
	return s.SaveNegativePartResolution(ctx, ref, nil)
}

// SaveNegativePartResolution caches a not_available resolution result for ref
// and optional partIdx for 60s (spec §4.4, #576).
func (s *Service) SaveNegativePartResolution(ctx context.Context, ref core.ItemRef, partIdx *int) error {
	exp := s.now().Add(ResolutionNegativeCacheTTL).Unix()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO plugin_resolutions(item_ref, pattern, source, candidates, negative, expires_at)
VALUES(?, '', '{}', '', 1, ?)
ON CONFLICT(item_ref) DO UPDATE SET
	pattern    = '',
	source     = '{}',
	candidates = '',
	negative   = 1,
	expires_at = excluded.expires_at`,
		resolutionStorageKey(ref, partIdx), exp)
	return err
}

// SavePartCandidates caches the full ordered list of PlayableSources returned
// by a plugin's /resolve for ref and optional partIdx until
// min(min(src.ExpiresAt), now+15m). An existing winning source in the row is
// preserved while still positive.
func (s *Service) SavePartCandidates(ctx context.Context, ref core.ItemRef, partIdx *int, sources []core.PlayableSource) error {
	if len(sources) == 0 {
		return nil
	}
	now := s.now()
	exp := now.Add(ResolutionCacheMaxTTL).Unix()
	for _, src := range sources {
		if src.ExpiresAt > 0 && src.ExpiresAt < exp {
			exp = src.ExpiresAt
		}
	}
	raw, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO plugin_resolutions(item_ref, pattern, source, candidates, negative, expires_at)
VALUES(?, '', '{}', ?, 0, ?)
ON CONFLICT(item_ref) DO UPDATE SET
	pattern    = CASE WHEN plugin_resolutions.negative != 0 THEN '' ELSE plugin_resolutions.pattern END,
	source     = CASE WHEN plugin_resolutions.negative != 0 THEN '{}' ELSE plugin_resolutions.source END,
	candidates = excluded.candidates,
	negative   = 0,
	expires_at = excluded.expires_at`,
		resolutionStorageKey(ref, partIdx), string(raw), exp)
	return err
}

// GetPartCandidates looks up a non-expired cached /resolve candidate list for
// ref and optional partIdx. If a negative cache entry is active, it returns
// (nil, true, core.ErrNotAvailable).
func (s *Service) GetPartCandidates(ctx context.Context, ref core.ItemRef, partIdx *int) ([]core.PlayableSource, bool, error) {
	nowUnix := s.now().Unix()
	var (
		candidatesJSON string
		negative       int
		expiresAt      int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT candidates, negative, expires_at FROM plugin_resolutions WHERE item_ref = ?`,
		resolutionStorageKey(ref, partIdx)).Scan(&candidatesJSON, &negative, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if expiresAt <= nowUnix {
		return nil, false, nil
	}
	if negative != 0 {
		return nil, true, core.ErrNotAvailable
	}
	if candidatesJSON == "" {
		return nil, false, nil
	}
	var sources []core.PlayableSource
	if err := json.Unmarshal([]byte(candidatesJSON), &sources); err != nil {
		return nil, false, err
	}
	if len(sources) == 0 {
		return nil, false, nil
	}
	return sources, true, nil
}

// GetResolution looks up a non-expired cached resolution for ref (spec §4.4).
// If a negative cache entry is active, it returns (zero, true, core.ErrNotAvailable).
func (s *Service) GetResolution(ctx context.Context, ref core.ItemRef) (core.PlayableSource, bool, error) {
	return s.GetPartResolution(ctx, ref, nil)
}

// GetPartResolution looks up a non-expired cached resolution for ref and
// optional partIdx (spec §4.4, #576).
func (s *Service) GetPartResolution(ctx context.Context, ref core.ItemRef, partIdx *int) (core.PlayableSource, bool, error) {
	src, ok, _, err := s.partResolution(ctx, ref, partIdx, false)
	return src, ok, err
}

// GetPartResolutionForOpen allows the opener to inspect expired successful
// resolutions. The opener must prove that such a source is still reusable.
func (s *Service) GetPartResolutionForOpen(ctx context.Context, ref core.ItemRef, partIdx *int) (src core.PlayableSource, found, expired bool, err error) {
	return s.partResolution(ctx, ref, partIdx, true)
}

func (s *Service) partResolution(ctx context.Context, ref core.ItemRef, partIdx *int, allowExpired bool) (core.PlayableSource, bool, bool, error) {
	nowUnix := s.now().Unix()
	var (
		pattern    string
		sourceJSON string
		negative   int
		expiresAt  int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT pattern, source, negative, expires_at FROM plugin_resolutions WHERE item_ref = ?`,
		resolutionStorageKey(ref, partIdx)).Scan(&pattern, &sourceJSON, &negative, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.PlayableSource{}, false, false, nil
		}
		return core.PlayableSource{}, false, false, err
	}
	expired := expiresAt <= nowUnix
	if expired && (!allowExpired || negative != 0) {
		return core.PlayableSource{}, false, false, nil
	}
	if negative != 0 {
		return core.PlayableSource{}, true, false, core.ErrNotAvailable
	}
	if pattern == "" || sourceJSON == "" || sourceJSON == "{}" {
		return core.PlayableSource{}, false, false, nil
	}
	var src core.PlayableSource
	if err := json.Unmarshal([]byte(sourceJSON), &src); err != nil {
		if expired {
			return core.PlayableSource{}, false, false, nil
		}
		return core.PlayableSource{}, false, false, err
	}
	return src, true, expired, nil
}

// InvalidateResolution deletes cached whole-item and per-part resolutions for
// ref, e.g. on a mid-stream upstream failure (spec §4.4, #576).
func (s *Service) InvalidateResolution(ctx context.Context, ref core.ItemRef) error {
	refStr := string(ref)
	partPrefix := refStr + "#part:"
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM plugin_resolutions WHERE item_ref = ? OR SUBSTR(item_ref, 1, ?) = ?`,
		refStr, len(partPrefix), partPrefix)
	return err
}

// InvalidatePartResolution deletes a single part's cached resolution when
// partIdx is non-nil, or invalidates the whole item (and its parts) when nil.
func (s *Service) InvalidatePartResolution(ctx context.Context, ref core.ItemRef, partIdx *int) error {
	if partIdx == nil {
		return s.InvalidateResolution(ctx, ref)
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM plugin_resolutions WHERE item_ref = ?`,
		resolutionStorageKey(ref, partIdx))
	return err
}
