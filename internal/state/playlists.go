package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/google/uuid"
)

func init() {
	// Some older databases recorded state/1 without the optional member
	// table. Ensure that legacy shape exists before copying its entries.
	store.Register("state", 7, `
 CREATE TABLE IF NOT EXISTS state_playlist_items (
 playlist_id INTEGER NOT NULL REFERENCES state_playlists(id) ON DELETE CASCADE,
 position INTEGER NOT NULL,
 rating_key INTEGER NOT NULL,
 PRIMARY KEY (playlist_id, position));
 ALTER TABLE state_playlists ADD COLUMN guid TEXT NOT NULL DEFAULT '';
 ALTER TABLE state_playlists ADD COLUMN summary TEXT NOT NULL DEFAULT '';
 ALTER TABLE state_playlists ADD COLUMN added_at INTEGER NOT NULL DEFAULT 0;
 UPDATE state_playlists SET added_at=updated_at;
 CREATE TABLE state_playlist_items_v2 (
 item_id INTEGER PRIMARY KEY AUTOINCREMENT,
 playlist_id INTEGER NOT NULL REFERENCES state_playlists(id) ON DELETE CASCADE,
 position INTEGER NOT NULL,
 rating_key INTEGER NOT NULL,
 UNIQUE(playlist_id,position));
 INSERT INTO state_playlist_items_v2(playlist_id,position,rating_key) SELECT playlist_id,position,rating_key FROM state_playlist_items ORDER BY playlist_id,position;
 DROP TABLE state_playlist_items;
 ALTER TABLE state_playlist_items_v2 RENAME TO state_playlist_items;
 `)
}

func init() {
	// When each entry was added. Older entries take their playlist's creation time.
	store.Register("state", 13, `
 ALTER TABLE state_playlist_items ADD COLUMN added_at INTEGER NOT NULL DEFAULT 0;
 UPDATE state_playlist_items SET added_at=COALESCE((SELECT p.added_at FROM state_playlists p WHERE p.id=state_playlist_items.playlist_id),0);
 `)
}

func playlistID(key core.RatingKey) (int64, error) {
	if key.Kind() != core.KindPlaylist {
		return 0, fmt.Errorf("%v: not a playlist key", key)
	}
	return key.TMDB(), nil // payload accessor: for non-season/episode kinds TMDB() returns the payload
}

type playlistQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *State) loadPlaylist(ctx context.Context, userID, id int64) (*core.Playlist, error) {
	return loadPlaylistFrom(ctx, s.db, userID, id)
}
func loadPlaylistFrom(ctx context.Context, db playlistQuerier, userID, id int64) (*core.Playlist, error) {
	var p core.Playlist
	var smart int
	var upd, added int64
	err := db.QueryRowContext(ctx, `SELECT title, smart, updated_at, smart_uri, guid, summary, added_at FROM state_playlists WHERE id=? AND user_id=?`, id, userID).
		Scan(&p.Title, &smart, &upd, &p.SmartURI, &p.GUID, &p.Summary, &added)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Key = core.PlaylistKey(id)
	p.AddedAt = time.Unix(0, added).UTC()
	if p.GUID == "" {
		p.GUID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("ether:playlist:%d", id))).String()
	}
	p.UserID = userID
	p.Smart = smart != 0
	if upd > 0 {
		p.UpdatedAt = time.Unix(0, upd).UTC()
	}
	rows, err := db.QueryContext(ctx, `SELECT rating_key, item_id, added_at FROM state_playlist_items WHERE playlist_id=? ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	p.Items = []core.RatingKey{}
	for rows.Next() {
		var k, itemID, added int64
		if err := rows.Scan(&k, &itemID, &added); err != nil {
			return nil, err
		}
		p.Items = append(p.Items, core.RatingKey(k))
		p.ItemIDs = append(p.ItemIDs, itemID)
		at := time.Time{}
		if added > 0 {
			at = time.Unix(0, added).UTC()
		}
		p.ItemAddedAt = append(p.ItemAddedAt, at)
	}
	return &p, rows.Err()
}

// List returns the user's playlists (with items), most recently updated first.
func (s *State) List(ctx context.Context, userID int64) ([]core.Playlist, error) {
	out, err := s.ListHeaders(ctx, userID)
	if err != nil || len(out) == 0 {
		return out, err
	}
	byID := make(map[int64]int, len(out))
	args := make([]any, 0, len(out))
	for i := range out {
		out[i].Items = []core.RatingKey{}
		id := out[i].Key.TMDB()
		byID[id] = i
		args = append(args, id)
	}
	entries := "json_group_array(json_array(rating_key,item_id))"
	ordered := "SELECT playlist_id,rating_key,item_id FROM state_playlist_items WHERE playlist_id IN (%s) ORDER BY playlist_id,position"
	if store.DialectOf(s.db) == store.Postgres {
		entries = "CAST(json_agg(json_build_array(rating_key,item_id) ORDER BY position) AS TEXT)"
		ordered = "SELECT playlist_id,rating_key,item_id,position FROM state_playlist_items WHERE playlist_id IN (%s)"
	}
	for chunk := range store.Chunks(args, 400) {
		query := "SELECT playlist_id," + entries + " FROM (" +
			fmt.Sprintf(ordered, store.Placeholders(len(chunk))) + ") AS members GROUP BY playlist_id"
		rows, err := s.db.QueryContext(ctx, query, chunk...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var pid int64
			var payload string
			if err := rows.Scan(&pid, &payload); err != nil {
				rows.Close()
				return nil, err
			}
			var members [][2]int64
			if err := json.Unmarshal([]byte(payload), &members); err != nil {
				rows.Close()
				return nil, err
			}
			playlist := &out[byID[pid]]
			for _, member := range members {
				playlist.Items = append(playlist.Items, core.RatingKey(member[0]))
				playlist.ItemIDs = append(playlist.ItemIDs, member[1])
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListHeaders returns the user's playlists without their items, in List's
// order, for callers that do not read the items (row cards, title search).
func (s *State) ListHeaders(ctx context.Context, userID int64) ([]core.Playlist, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, smart, updated_at, smart_uri, guid, summary, added_at FROM state_playlists WHERE user_id=? ORDER BY updated_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Playlist
	for rows.Next() {
		var id, upd, added int64
		var smart int
		var p core.Playlist
		if err := rows.Scan(&id, &p.Title, &smart, &upd, &p.SmartURI, &p.GUID, &p.Summary, &added); err != nil {
			return nil, err
		}
		p.Key, p.UserID, p.Smart = core.PlaylistKey(id), userID, smart != 0
		p.AddedAt = time.Unix(0, added).UTC()
		if p.GUID == "" {
			p.GUID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("ether:playlist:%d", id))).String()
		}
		if upd > 0 {
			p.UpdatedAt = time.Unix(0, upd).UTC()
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get returns one playlist owned by the user, or core.ErrNotFound.
func (s *State) GetPlaylist(ctx context.Context, userID int64, key core.RatingKey) (*core.Playlist, error) {
	id, err := playlistID(key)
	if err != nil {
		return nil, err
	}
	return s.loadPlaylist(ctx, userID, id)
}

// GetPlaylistHeader returns one playlist header without reading its items, or
// core.ErrNotFound.
func (s *State) GetPlaylistHeader(ctx context.Context, userID int64, key core.RatingKey) (*core.Playlist, error) {
	id, err := playlistID(key)
	if err != nil {
		return nil, err
	}
	var p core.Playlist
	var smart int
	var upd, added int64
	err = s.db.QueryRowContext(ctx, `SELECT title, smart, updated_at, smart_uri, guid, summary, added_at FROM state_playlists WHERE id=? AND user_id=?`, id, userID).
		Scan(&p.Title, &smart, &upd, &p.SmartURI, &p.GUID, &p.Summary, &added)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Key, p.UserID, p.Smart = core.PlaylistKey(id), userID, smart != 0
	p.AddedAt = time.Unix(0, added).UTC()
	if p.GUID == "" {
		p.GUID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("ether:playlist:%d", id))).String()
	}
	if upd > 0 {
		p.UpdatedAt = time.Unix(0, upd).UTC()
	}
	return &p, nil
}

// GetPlaylistHeaders returns headers for the requested playlists owned by the
// user in one query per chunk, without reading state_playlist_items.
func (s *State) GetPlaylistHeaders(ctx context.Context, userID int64, keys []core.RatingKey) (map[core.RatingKey]core.Playlist, error) {
	out := make(map[core.RatingKey]core.Playlist, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	ids := make([]any, 0, len(keys))
	seen := make(map[int64]bool, len(keys))
	for _, key := range keys {
		id, err := playlistID(key)
		if err != nil || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out, nil
	}
	for chunk := range store.Chunks(ids, 400) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, userID)
		args = append(args, chunk...)
		rows, err := s.db.QueryContext(ctx, `SELECT id, title, smart, updated_at, smart_uri, guid, summary, added_at FROM state_playlists WHERE user_id=? AND id IN (`+store.Placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, upd, added int64
			var smart int
			var p core.Playlist
			if err := rows.Scan(&id, &p.Title, &smart, &upd, &p.SmartURI, &p.GUID, &p.Summary, &added); err != nil {
				rows.Close()
				return nil, err
			}
			p.Key, p.UserID, p.Smart = core.PlaylistKey(id), userID, smart != 0
			p.AddedAt = time.Unix(0, added).UTC()
			if p.GUID == "" {
				p.GUID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("ether:playlist:%d", id))).String()
			}
			if upd > 0 {
				p.UpdatedAt = time.Unix(0, upd).UTC()
			}
			out[p.Key] = p
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Create makes a playlist with the given items in order.
func (s *State) Create(ctx context.Context, userID int64, title string, items []core.RatingKey) (*core.Playlist, error) {
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO state_playlists(user_id, title, smart, updated_at,added_at,guid) VALUES(?,?,0,?,?,?) RETURNING id`,
		userID, title, now.UnixNano(), now.UnixNano(), uuid.NewString()).Scan(&id); err != nil {
		return nil, err
	}
	for i, k := range items {
		if _, err := tx.ExecContext(ctx, `INSERT INTO state_playlist_items(playlist_id, position, rating_key, added_at) VALUES(?,?,?,?)`, id, i, int64(k), now.UnixNano()); err != nil {
			return nil, err
		}
	}
	snapshot, err := loadPlaylistFrom(ctx, tx, userID, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// rewriteItems reads, mutates and rewrites a playlist's items in one
// transaction. Touching the playlist row first takes its row lock, so a
// concurrent rewrite on another node waits and then reads the committed list
// instead of overwriting it with a stale one.
func (s *State) rewriteItems(ctx context.Context, userID int64, key core.RatingKey, mutate func([]playlistEntry) ([]playlistEntry, error)) error {
	id, err := playlistID(key)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE state_playlists SET updated_at=? WHERE id=? AND user_id=?`, now.UnixNano(), id, userID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return core.ErrNotFound
	}
	cur, err := playlistItemsTx(ctx, tx, id)
	if err != nil {
		return err
	}
	items, err := mutate(cur)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM state_playlist_items WHERE playlist_id=?`, id); err != nil {
		return err
	}
	for i, k := range items {
		var err error
		if k.id == 0 {
			_, err = tx.ExecContext(ctx, `INSERT INTO state_playlist_items(playlist_id, position, rating_key, added_at) VALUES(?,?,?,?)`, id, i, int64(k.key), now.UnixNano())
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO state_playlist_items(item_id,playlist_id, position, rating_key, added_at) VALUES(?,?,?,?,?)`, k.id, id, i, int64(k.key), k.added)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

type playlistEntry struct {
	key   core.RatingKey
	id    int64
	added int64
}

func playlistItemsTx(ctx context.Context, tx *sql.Tx, id int64) ([]playlistEntry, error) {
	rows, err := tx.QueryContext(ctx, `SELECT rating_key,item_id,added_at FROM state_playlist_items WHERE playlist_id=? ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []playlistEntry
	for rows.Next() {
		var k, itemID, added int64
		if err := rows.Scan(&k, &itemID, &added); err != nil {
			return nil, err
		}
		out = append(out, playlistEntry{core.RatingKey(k), itemID, added})
	}
	return out, rows.Err()
}

// Add appends items to a playlist.
func (s *State) Add(ctx context.Context, userID int64, key core.RatingKey, items []core.RatingKey) error {
	return s.rewriteItems(ctx, userID, key, func(cur []playlistEntry) ([]playlistEntry, error) {
		for _, key := range items {
			cur = append(cur, playlistEntry{key: key})
		}
		return cur, nil
	})
}

// Remove deletes the item at itemIndex (0-based).
func (s *State) Remove(ctx context.Context, userID int64, key core.RatingKey, itemIndex int) error {
	return s.rewriteItems(ctx, userID, key, func(cur []playlistEntry) ([]playlistEntry, error) {
		if itemIndex < 0 || itemIndex >= len(cur) {
			return nil, fmt.Errorf("item index %d out of range (len %d)", itemIndex, len(cur))
		}
		return append(cur[:itemIndex], cur[itemIndex+1:]...), nil
	})
}

// Move moves the item at from so it lands at index to (both 0-based, to is
// the final position in the resulting list).
func (s *State) Move(ctx context.Context, userID int64, key core.RatingKey, from, to int) error {
	return s.rewriteItems(ctx, userID, key, func(cur []playlistEntry) ([]playlistEntry, error) {
		if from < 0 || from >= len(cur) || to < 0 || to >= len(cur) {
			return nil, fmt.Errorf("move %d -> %d out of range (len %d)", from, to, len(cur))
		}
		item := cur[from]
		cur = append(cur[:from], cur[from+1:]...)
		cur = append(cur, playlistEntry{})
		copy(cur[to+1:], cur[to:])
		cur[to] = item
		return cur, nil
	})
}

// Delete removes a playlist. Deleting another user's playlist is ErrNotFound.
func (s *State) Delete(ctx context.Context, userID int64, key core.RatingKey) error {
	id, err := playlistID(key)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM state_playlists WHERE id=? AND user_id=?`, id, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return core.ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM state_playlist_items WHERE playlist_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// playlistsView adapts State to core.Playlists. core.State.Get and
// core.Playlists.Get have different signatures, so one Go type cannot
// implement both interfaces directly; State implements core.State and
// Playlists() returns the core.Playlists view of the same store.
type playlistsView struct{ s *State }

func (v playlistsView) List(ctx context.Context, userID int64) ([]core.Playlist, error) {
	return v.s.List(ctx, userID)
}
func (v playlistsView) ListHeaders(ctx context.Context, userID int64) ([]core.Playlist, error) {
	return v.s.ListHeaders(ctx, userID)
}
func (v playlistsView) GetHeader(ctx context.Context, userID int64, key core.RatingKey) (*core.Playlist, error) {
	return v.s.GetPlaylistHeader(ctx, userID, key)
}
func (v playlistsView) GetHeaders(ctx context.Context, userID int64, keys []core.RatingKey) (map[core.RatingKey]core.Playlist, error) {
	return v.s.GetPlaylistHeaders(ctx, userID, keys)
}

// PlaylistArtworkHeads returns the first ordered member keys for each owned
// playlist in the artwork page; an empty playlist still has a map entry.
func (v playlistsView) PlaylistArtworkHeads(ctx context.Context, userID int64, keys []core.RatingKey, limit int) (map[core.RatingKey][]core.RatingKey, error) {
	out := make(map[core.RatingKey][]core.RatingKey, len(keys))
	aggregate := "json_group_array(rating_key)"
	if store.DialectOf(v.s.db) == store.Postgres {
		aggregate = "CAST(json_agg(rating_key ORDER BY position) AS TEXT)"
	}
	for part := range store.Chunks(keys, 400) {
		args := []any{limit, userID}
		for _, key := range part {
			id, err := playlistID(key)
			if err != nil {
				return nil, err
			}
			args = append(args, id)
		}
		rows, err := v.s.db.QueryContext(ctx, `SELECT p.id, COALESCE((SELECT `+aggregate+` FROM (
			SELECT i.rating_key,i.position FROM state_playlist_items i WHERE i.playlist_id=p.id ORDER BY i.position LIMIT ?
		) AS head), '[]') FROM state_playlists p WHERE p.user_id=? AND p.id IN (`+store.Placeholders(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var payload string
			if err := rows.Scan(&id, &payload); err != nil {
				rows.Close()
				return nil, err
			}
			var members []core.RatingKey
			if err := json.Unmarshal([]byte(payload), &members); err != nil {
				rows.Close()
				return nil, err
			}
			out[core.PlaylistKey(id)] = members
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// PlaylistItemKeys reads one ordered window for artwork selection.
func (v playlistsView) PlaylistItemKeys(ctx context.Context, userID int64, key core.RatingKey, offset, limit int) ([]core.RatingKey, error) {
	id, err := playlistID(key)
	if err != nil {
		return nil, err
	}
	rows, err := v.s.db.QueryContext(ctx, `SELECT i.rating_key
		FROM state_playlist_items i JOIN state_playlists p ON p.id=i.playlist_id
		WHERE i.playlist_id=? AND p.user_id=? ORDER BY i.position LIMIT ? OFFSET ?`, id, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []core.RatingKey
	for rows.Next() {
		var k core.RatingKey
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (v playlistsView) Get(ctx context.Context, userID int64, key core.RatingKey) (*core.Playlist, error) {
	return v.s.GetPlaylist(ctx, userID, key)
}
func (v playlistsView) Create(ctx context.Context, userID int64, title string, items []core.RatingKey) (*core.Playlist, error) {
	return v.s.Create(ctx, userID, title, items)
}
func (v playlistsView) CreateSmartPlaylist(ctx context.Context, userID int64, title, uri string) (*core.Playlist, error) {
	tx, err := v.s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO state_playlists(user_id,title,smart,updated_at,smart_uri,added_at,guid) VALUES(?,?,1,?,?,?,?) RETURNING id`, userID, title, v.s.now().UTC().UnixNano(), uri, v.s.now().UTC().UnixNano(), uuid.NewString()).Scan(&id)
	if err != nil {
		return nil, err
	}
	snapshot, err := loadPlaylistFrom(ctx, tx, userID, id)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}
func (v playlistsView) Add(ctx context.Context, userID int64, key core.RatingKey, items []core.RatingKey) error {
	return v.s.Add(ctx, userID, key, items)
}
func (v playlistsView) Remove(ctx context.Context, userID int64, key core.RatingKey, itemIndex int) error {
	return v.s.Remove(ctx, userID, key, itemIndex)
}
func (v playlistsView) Move(ctx context.Context, userID int64, key core.RatingKey, from, to int) error {
	return v.s.Move(ctx, userID, key, from, to)
}
func (v playlistsView) Delete(ctx context.Context, userID int64, key core.RatingKey) error {
	return v.s.Delete(ctx, userID, key)
}

var _ core.Playlists = playlistsView{}

// Playlists returns s as a core.Playlists.
func (s *State) Playlists() core.Playlists { return playlistsView{s} }

func (v playlistsView) EditPlaylist(ctx context.Context, userID int64, key core.RatingKey, edit core.PlaylistEdit) (*core.Playlist, error) {
	id, err := playlistID(key)
	if err != nil {
		return nil, err
	}
	tx, err := v.s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE state_playlists SET title=COALESCE(?,title),summary=COALESCE(?,summary),smart_uri=COALESCE(?,smart_uri),updated_at=? WHERE id=? AND user_id=?`, edit.Title, edit.Summary, edit.SmartURI, v.s.now().UTC().UnixNano(), id, userID)
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, core.ErrNotFound
	}
	snapshot, err := loadPlaylistFrom(ctx, tx, userID, id)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// MutatePlaylist serializes the mutation and captures the response before releasing
// the row lock, including stable identity lookup under that lock.
func (v playlistsView) MutatePlaylist(ctx context.Context, userID int64, key core.RatingKey, m core.PlaylistMutation) (*core.Playlist, int, error) {
	id, err := playlistID(key)
	if err != nil {
		return nil, 0, err
	}
	tx, err := v.s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE state_playlists SET updated_at=? WHERE id=? AND user_id=?`, v.s.now().UTC().UnixNano(), id, userID)
	if err != nil {
		return nil, 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, 0, err
	}
	if n == 0 {
		return nil, 0, core.ErrNotFound
	}
	pl, err := loadPlaylistFrom(ctx, tx, userID, id)
	if err != nil {
		return nil, 0, err
	}
	if pl.Smart {
		return nil, 0, fmt.Errorf("immutable smart membership")
	}
	entries, err := playlistItemsTx(ctx, tx, id)
	if err != nil {
		return nil, 0, err
	}
	changed := 0
	switch m.Operation {
	case "add":
		seen := map[core.RatingKey]bool{}
		for _, e := range entries {
			seen[e.key] = true
		}
		for _, key := range m.Items {
			if !seen[key] {
				entries = append(entries, playlistEntry{key: key})
				seen[key] = true
				changed++
			}
		}
	case "clear":
		entries = nil
	case "remove", "move":
		from := -1
		for i, e := range entries {
			if e.id == m.ItemID {
				from = i
				break
			}
		}
		if from < 0 {
			return nil, 0, core.ErrNotFound
		}
		item := entries[from]
		if m.Operation == "move" && m.ItemID == m.AfterID {
			break
		}
		entries = append(entries[:from], entries[from+1:]...)
		if m.Operation == "move" {
			to := 0
			for i, e := range entries {
				if e.id == m.AfterID {
					to = i + 1
					break
				}
			}
			entries = append(entries, playlistEntry{})
			copy(entries[to+1:], entries[to:])
			entries[to] = item
		}
	default:
		return nil, 0, fmt.Errorf("unsupported playlist operation")
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM state_playlist_items WHERE playlist_id=?`, id); err != nil {
		return nil, 0, err
	}
	for i, e := range entries {
		if e.id == 0 {
			_, err = tx.ExecContext(ctx, `INSERT INTO state_playlist_items(playlist_id,position,rating_key) VALUES(?,?,?)`, id, i, int64(e.key))
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO state_playlist_items(item_id,playlist_id,position,rating_key) VALUES(?,?,?,?)`, e.id, id, i, int64(e.key))
		}
		if err != nil {
			return nil, 0, err
		}
	}
	pl, err = loadPlaylistFrom(ctx, tx, userID, id)
	if err != nil {
		return nil, 0, err
	}
	if err = tx.Commit(); err != nil {
		return nil, 0, err
	}
	return pl, changed, nil
}
