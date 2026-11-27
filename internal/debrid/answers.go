package debrid

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/lru"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("debrid", 5, `
CREATE TABLE debrid_answers (
  account TEXT NOT NULL,
  info_hash TEXT NOT NULL,
  file_key TEXT NOT NULL,
  verdict TEXT NOT NULL,
  torrent_id TEXT NOT NULL,
  files TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  PRIMARY KEY (account, info_hash, file_key)
);
CREATE INDEX debrid_answers_expiry ON debrid_answers(expires_at, account, info_hash, file_key);`)
	store.RegisterLocalTable("debrid_answers")
}

const (
	answerLimit         = 4096
	answerFilesLimit    = 64 << 10
	answerHoldTTL       = 6 * time.Hour
	answerCached        = "cached"
	answerCold          = "cold"
	answerBlockedHash   = "blocked_hash"
	answerBlockedFile   = "blocked_file"
	answerTooLarge      = "too_large"
	answerPending       = "pending"
	answerReconcile     = "reconcile"
	answerReconcileWait = "reconcile_wait"
)

type answerKey struct {
	hash, file string
}

type answerFile struct {
	ID, Path string
	Size     int64
	Selected bool
}

type learnedAnswer struct {
	verdict, torrentID string
	files              []answerFile
	expires            time.Time
}

func (a learnedAnswer) needsReconciliation() bool {
	return a.verdict == answerReconcile || a.verdict == answerReconcileWait
}

func (a learnedAnswer) refusal() error {
	switch a.verdict {
	case answerCold:
		return core.ErrNotCached
	case answerBlockedHash:
		return ErrBlockedHash
	case answerTooLarge:
		return ErrTooLarge
	}
	return nil
}

type answerStore struct {
	db *sql.DB
	mu sync.Mutex // serializes the bounded write/prune transaction on this node
}

func (p *accountPacer) answer(ctx context.Context, key answerKey, now time.Time) learnedAnswer {
	key.hash = strings.ToLower(key.hash)
	if key.hash == "" || len(key.hash) > 128 {
		return learnedAnswer{}
	}
	var a learnedAnswer
	var missing bool
	if p.answers != nil && p.answers.db != nil {
		var at int64
		var files string
		err := p.answers.db.QueryRowContext(ctx, `SELECT verdict,torrent_id,files,expires_at FROM debrid_answers
WHERE account=? AND info_hash=? AND file_key=? AND expires_at>?`, p.identity, key.hash, key.file, now.Unix()).
			Scan(&a.verdict, &a.torrentID, &files, &at)
		if err == nil {
			a.expires = time.Unix(at, 0)
			_ = json.Unmarshal([]byte(files), &a.files)
			return a
		}
		missing = errors.Is(err, sql.ErrNoRows)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.learned != nil {
		a, _ = p.learned.Get(key)
		// An ambiguous POST must still be reconciled if the durable
		// write failed. A database miss invalidates other local snapshots.
		if now.Before(a.expires) && (!missing || a.needsReconciliation()) {
			return a
		}
	}
	return learnedAnswer{}
}

func (p *accountPacer) writeAnswer(ctx context.Context, key answerKey, a learnedAnswer, now time.Time) {
	key.hash = strings.ToLower(key.hash)
	if key.hash == "" || len(key.hash) > 128 {
		return
	}
	switch a.verdict {
	case answerCached:
		a.expires = now.Add(24 * time.Hour)
	case answerCold, answerTooLarge, answerPending, answerReconcile, answerReconcileWait:
		a.expires = now.Add(answerHoldTTL)
	case answerBlockedHash, answerBlockedFile:
		a.expires = now.Add(7 * 24 * time.Hour)
	}
	files, _ := json.Marshal(a.files)
	if len(files) > answerFilesLimit {
		a.files, files = nil, []byte("null")
	}
	p.mu.Lock()
	if p.learned == nil {
		p.learned = lru.New(lru.Options[answerKey, learnedAnswer]{MaxEntries: 256, MaxCost: 4 << 20, Cost: func(_ answerKey, a learnedAnswer) int64 {
			cost := 128
			for _, f := range a.files {
				cost += 64 + len(f.ID) + len(f.Path)
			}
			return int64(cost)
		}})
	}
	// A zero expiry is a tombstone until the bounded memory entry is evicted.
	if p.answers == nil || p.answers.db == nil {
		p.learned.RemoveFunc(func(_ answerKey, previous learnedAnswer) bool { return !now.Before(previous.expires) })
	}
	p.learned.Add(key, a)
	p.mu.Unlock()
	if p.answers == nil || p.answers.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	p.answers.mu.Lock()
	defer p.answers.mu.Unlock()
	tx, err := p.answers.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	if store.DialectOf(p.answers.db) == store.Postgres {
		// Two nodes must not both prune an over-cap snapshot that cannot
		// see the other's insert. Reads remain concurrent.
		if _, err := tx.ExecContext(ctx, `LOCK TABLE debrid_answers IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return
		}
	}
	if a.verdict == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM debrid_answers WHERE account=? AND info_hash=? AND file_key=?`, p.identity, key.hash, key.file)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO debrid_answers(account,info_hash,file_key,verdict,torrent_id,files,expires_at)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(account,info_hash,file_key) DO UPDATE SET
verdict=excluded.verdict,torrent_id=excluded.torrent_id,files=excluded.files,expires_at=excluded.expires_at`,
			p.identity, key.hash, key.file, a.verdict, a.torrentID, string(files), a.expires.Unix())
	}
	if err != nil {
		return
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM debrid_answers WHERE expires_at<=?`, now.Unix()); err != nil {
		return
	}
	// The LIMIT makes this statement portable across SQLite and PostgreSQL.
	if _, err = tx.ExecContext(ctx, `DELETE FROM debrid_answers WHERE (account,info_hash,file_key) IN
(SELECT account,info_hash,file_key FROM debrid_answers ORDER BY expires_at DESC,account DESC,info_hash DESC,file_key DESC LIMIT ? OFFSET ?)`, answerLimit, answerLimit); err == nil {
		_ = tx.Commit()
	}
}

func fileAnswerKey(hash, path string, size int64, id string) answerKey {
	identity := strings.TrimLeft(path, "/")
	if identity == "" {
		identity = id
	}
	digest := sha256.Sum256([]byte(identity + "\x00" + strconv.FormatInt(size, 10)))
	return answerKey{hash: hash, file: hex.EncodeToString(digest[:])}
}

// lockHash lets a waiter select its own file from the winner's account item.
// Background work can use an idle hash immediately and otherwise yields.
func (p *accountPacer) lockHash(ctx context.Context, hash string) (func(), error) {
	hash = strings.ToLower(hash)
	for {
		p.mu.Lock()
		done := p.acquiring[hash]
		if done == nil {
			if p.acquiring == nil {
				p.acquiring = make(map[string]chan struct{})
			}
			done = make(chan struct{})
			p.acquiring[hash] = done
			p.mu.Unlock()
			return func() {
				p.mu.Lock()
				delete(p.acquiring, hash)
				close(done)
				p.mu.Unlock()
			}, nil
		}
		p.mu.Unlock()
		if lowPriority(ctx) {
			return nil, errBackgroundUndecided
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				if err := p.cooldownFor(ctx); err != nil {
					return nil, err
				}
			}
			return nil, ctx.Err()
		case <-done:
		}
	}
}

var errSelectionMismatch = fmt.Errorf("debrid: requested file is not selected in the account torrent: %w", core.ErrNotCached)

// acquireRemembered shares the mutation and learns solely from provider
// observations. File metadata here carries no provider links.
func (p *accountPacer) acquireRemembered(ctx context.Context, req AcquireRequest, now func() time.Time, acquire func(context.Context, AcquireRequest) (*Acquired, error)) (*Acquired, error) {
	if req.InfoHash == "" {
		return acquire(ctx, req)
	}
	unlock, err := p.lockHash(ctx, req.InfoHash)
	if err != nil {
		return nil, err
	}
	defer unlock()
	key := answerKey{hash: req.InfoHash}
	a := p.answer(ctx, key, now())
	if err := a.refusal(); err != nil && (a.verdict != answerCold || req.CachedOnly) {
		p.noteRemembered()
		return nil, err
	}
	if a.torrentID != "" && req.TorrentID == "" {
		req.TorrentID = a.torrentID
		p.noteRemembered()
	}
	req.rememberedFiles = a.files
	choose := req.Choose
	var files []answerFile
	var chosen File
	req.Choose = func(raw []File) (File, error) {
		files = answerFiles(raw)
		f, err := choose(raw)
		chosen = f
		if err == nil && p.answer(ctx, fileAnswerKey(req.InfoHash, f.Path, f.Size, f.ID), now()).verdict == answerBlockedFile {
			p.noteRemembered()
			return File{}, ErrBlockedFile
		}
		return f, err
	}
	req.selectedFiles = func(raw []File) { files = answerFiles(raw) }
	got, err := acquire(ctx, req)
	var untracked *untrackedPending
	_, queued := pendingQueue(err)
	switch {
	case err == nil:
		// Keep freshly confirmed identical maps without taking a write
		// lock on every episode; revalidation extends them near expiry.
		at := now()
		if a.verdict != answerCached || a.torrentID != got.TorrentID || !slices.Equal(a.files, files) || a.expires.Sub(at) <= time.Hour {
			p.writeAnswer(ctx, key, learnedAnswer{verdict: answerCached, torrentID: got.TorrentID, files: files}, at)
		}
	case errors.Is(err, ErrBlockedFile) && chosen.ID != "":
		file := fileAnswerKey(req.InfoHash, chosen.Path, chosen.Size, chosen.ID)
		if p.answer(ctx, file, now()).verdict != answerBlockedFile {
			p.writeAnswer(ctx, file, learnedAnswer{verdict: answerBlockedFile}, now())
		}
	case errors.Is(err, ErrBlockedHash) || errors.Is(err, ErrBlocked) && !errors.Is(err, ErrBlockedFile):
		err = ErrBlockedHash
		p.writeAnswer(ctx, key, learnedAnswer{verdict: answerBlockedHash}, now())
	case errors.Is(err, ErrTooLarge):
		p.writeAnswer(ctx, key, learnedAnswer{verdict: answerTooLarge}, now())
	case pendingTorrent(err) != "":
		p.writeAnswer(ctx, key, learnedAnswer{verdict: answerPending, torrentID: pendingTorrent(err)}, now())
	case !queued && !errors.As(err, &untracked) && errors.Is(err, core.ErrNotCached) && req.TorrentID == "" && !refusal(err) && !errors.Is(err, errSelectionMismatch) && !errors.Is(err, core.ErrDownloadLimit):
		p.writeAnswer(ctx, key, learnedAnswer{verdict: answerCold}, now())
	case errors.Is(err, ErrGone) && req.TorrentID != "" && req.TorrentID == a.torrentID:
		p.writeAnswer(ctx, key, learnedAnswer{}, now())
	}
	return got, err
}

func answerFiles(raw []File) []answerFile {
	files := make([]answerFile, 0, len(raw))
	for _, f := range raw {
		id := f.ID
		if id == f.Link || strings.Contains(id, "://") {
			id = "" // AllDebrid identifies files by a provider-side URL.
		}
		files = append(files, answerFile{ID: id, Path: f.Path, Size: f.Size, Selected: f.selected})
	}
	return files
}

// selectedPack is the ready selection observed in the last account info
// response. Pack matching needs only identities and never unlocks a link.
func (p *accountPacer) selectedPack(ctx context.Context, hash string) []File {
	now := p.now()
	a := p.answer(ctx, answerKey{hash: hash}, now)
	if a.verdict != answerCached || len(a.files) == 0 {
		return nil
	}
	blocked := make(map[string]bool)
	if p.answers != nil && p.answers.db != nil {
		rows, err := p.answers.db.QueryContext(ctx, `SELECT file_key FROM debrid_answers
WHERE account=? AND info_hash=? AND verdict=? AND expires_at>?`, p.identity, hash, answerBlockedFile, now.Unix())
		if err != nil {
			return nil
		}
		defer rows.Close()
		for rows.Next() {
			var file string
			if err := rows.Scan(&file); err != nil {
				return nil
			}
			blocked[file] = true
		}
		if rows.Err() != nil {
			return nil
		}
	} else {
		p.mu.Lock()
		if p.learned != nil {
			for key, answer := range p.learned.All() {
				if key.hash == hash && answer.verdict == answerBlockedFile && now.Before(answer.expires) {
					blocked[key.file] = true
				}
			}
		}
		p.mu.Unlock()
	}

	files := make([]File, 0, len(a.files))
	for _, f := range a.files {
		if f.Selected && !blocked[fileAnswerKey(hash, f.Path, f.Size, f.ID).file] {
			files = append(files, File{ID: f.ID, Path: f.Path, Size: f.Size, selected: true})
		}
	}
	return files
}

func (p *accountPacer) linkRemembered(ctx context.Context, h Handle, now func() time.Time, link func(context.Context, Handle) (DirectLink, error)) (DirectLink, error) {
	key := fileAnswerKey(h.InfoHash, h.Path, h.Size, h.FileID)
	if p.answer(ctx, answerKey{hash: h.InfoHash}, now()).verdict == answerBlockedHash {
		p.noteRemembered()
		return DirectLink{}, ErrBlockedHash
	}
	if p.answer(ctx, key, now()).verdict == answerBlockedFile {
		p.noteRemembered()
		return DirectLink{}, ErrBlockedFile
	}
	result, err := link(ctx, h)
	switch {
	case errors.Is(err, ErrBlockedHash):
		p.writeAnswer(ctx, answerKey{hash: h.InfoHash}, learnedAnswer{verdict: answerBlockedHash}, now())
	case errors.Is(err, ErrBlocked):
		p.writeAnswer(ctx, key, learnedAnswer{verdict: answerBlockedFile}, now())
		err = ErrBlockedFile
	}
	return result, err
}
