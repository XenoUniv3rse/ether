package state

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/reqmemo"
	"github.com/braedonsaunders/ether/internal/store"
)

// A viewer may prefer one release of a recording (the same song on an album,
// a single and a compilation) and hide the others from their own lists.
func init() {
	store.Register("state_recording_prefs", 1, `
CREATE TABLE state_recording_pref(user_id INTEGER NOT NULL, library_id INTEGER NOT NULL, recording TEXT NOT NULL, preferred_key INTEGER NOT NULL, updated_at INTEGER NOT NULL, PRIMARY KEY(user_id, library_id, recording));
CREATE TABLE state_recording_hidden(user_id INTEGER NOT NULL, item_key INTEGER NOT NULL, library_id INTEGER NOT NULL, recording TEXT NOT NULL, PRIMARY KEY(user_id, item_key));
CREATE INDEX state_recording_hidden_recording ON state_recording_hidden(user_id, library_id, recording);`)
	store.RegisterViewerTable("state", "state_recording_pref", "user_id", store.ViewerMergeMove, "library_id", "recording")
	store.RegisterViewerTable("state", "state_recording_hidden", "user_id", store.ViewerMergeMove, "item_key")
}

// SetRecordingPreference records a viewer's preferred release of a recording
// and which of its other releases they hide (replacing earlier hides).
func (s *State) SetRecordingPreference(ctx context.Context, uid int64, library core.LibraryID, recording string, preferred core.RatingKey, hide []core.RatingKey) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO state_recording_pref(user_id, library_id, recording, preferred_key, updated_at) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(user_id, library_id, recording) DO UPDATE SET preferred_key = excluded.preferred_key, updated_at = excluded.updated_at`,
		uid, int64(library), recording, int64(preferred), time.Now().UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM state_recording_hidden WHERE user_id = ? AND library_id = ? AND recording = ?`, uid, int64(library), recording); err != nil {
		return err
	}
	for _, k := range hide {
		if k == preferred {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO state_recording_hidden(user_id, item_key, library_id, recording) VALUES(?, ?, ?, ?)
			ON CONFLICT(user_id, item_key) DO UPDATE SET library_id = excluded.library_id, recording = excluded.recording`, uid, int64(k), int64(library), recording); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	reqmemo.Forget(ctx, hiddenRecordingMemoKey(uid))
	return nil
}

// RecordingPreferences answers a viewer's preferred release of each listed
// recording in a library (absent when none).
func (s *State) RecordingPreferences(ctx context.Context, uid int64, library core.LibraryID, recordings []string) (map[string]core.RatingKey, error) {
	out := map[string]core.RatingKey{}
	if len(recordings) == 0 {
		return out, nil
	}
	args := []any{uid, int64(library)}
	ph := make([]string, len(recordings))
	for i, r := range recordings {
		ph[i] = "?"
		args = append(args, r)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT recording, preferred_key FROM state_recording_pref WHERE user_id = ? AND library_id = ? AND recording IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rec string
		var key int64
		if err := rows.Scan(&rec, &key); err != nil {
			return nil, err
		}
		out[rec] = core.RatingKey(key)
	}
	return out, rows.Err()
}

// HiddenRecordingCopies lists the releases a viewer hid in favour of another
// release of the same recording.
func (s *State) HiddenRecordingCopies(ctx context.Context, uid int64) ([]core.RatingKey, error) {
	keys, err := reqmemo.Do(ctx, hiddenRecordingMemoKey(uid), func() ([]core.RatingKey, error) {
		return s.hiddenRecordingCopies(ctx, uid)
	})
	return slices.Clone(keys), err
}

func hiddenRecordingMemoKey(uid int64) string {
	return "state:hidden-recordings:" + strconv.FormatInt(uid, 10)
}

func (s *State) hiddenRecordingCopies(ctx context.Context, uid int64) ([]core.RatingKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT item_key FROM state_recording_hidden WHERE user_id = ? ORDER BY item_key LIMIT 10000`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.RatingKey
	for rows.Next() {
		var k int64
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, core.RatingKey(k))
	}
	return out, rows.Err()
}
