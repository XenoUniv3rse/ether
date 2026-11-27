package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("variants", 1, `
CREATE TABLE variants_ids (
  media_id INTEGER PRIMARY KEY,
  rating_key INTEGER NOT NULL,
  release_id TEXT NOT NULL
);
CREATE UNIQUE INDEX variants_ids_pair ON variants_ids(rating_key, release_id);`)
}

const (
	variantIDBase  = frontend.VariantIDBase
	variantIDSpace = frontend.VariantIDSpace

	maxVariantIDs = 200_000
)

// ---------------------------------------------------------------------------
// Variant Media/Part ids

// variantEntry is a registry id's mapping and its last_seen as last read
// or touched (Unix seconds; see variants_retention.go).
type variantEntry struct {
	ref  VariantRef
	seen int64
}

type VariantIDs struct {
	mu     sync.Mutex
	db     *sql.DB
	byID   map[int64]variantEntry
	byPair map[VariantRef]int64
	// pending holds ids whose last_seen write is queued for the flusher.
	pending map[int64]VariantRef
	now     func() time.Time
}

func NewVariantIDs(db *sql.DB, now func() time.Time) *VariantIDs {
	if now == nil {
		now = time.Now
	}
	return &VariantIDs{db: db, byID: map[int64]variantEntry{}, byPair: map[VariantRef]int64{}, pending: map[int64]VariantRef{}, now: now}
}

// Persisted mappings are fetched by indexed key on demand. Browsing one
// page must never load the registry for the entire library.
func (r *VariantIDs) loadIDLocked(id int64) {
	if _, ok := r.byID[id]; ok || r.db == nil {
		return
	}
	var rk, seen int64
	var release string
	if err := r.db.QueryRow(`SELECT rating_key,release_id,last_seen FROM variants_ids WHERE media_id=?`, id).Scan(&rk, &release, &seen); err == nil {
		ref := VariantRef{Key: core.RatingKey(rk), ReleaseID: release}
		r.cacheLocked(id, variantEntry{ref: ref, seen: seen})
	}
}

// VariantHash lives in internal/frontend (host_sessions_experience.go).
var VariantHash = frontend.VariantHash

// cacheLocked bounds both indexes, including when a reverse lookup loads an
// entry. Pending touches retain their own references across generations.
func (r *VariantIDs) cacheLocked(id int64, entry variantEntry) {
	if len(r.byID) >= maxVariantIDs {
		r.byID = make(map[int64]variantEntry)
		r.byPair = make(map[VariantRef]int64)
	}
	if old, ok := r.byID[id]; ok && old.ref != entry.ref {
		delete(r.byPair, old.ref)
	}
	if old, ok := r.byPair[entry.ref]; ok && old != id {
		delete(r.byID, old)
	}
	r.byID[id] = entry
	r.byPair[entry.ref] = id
}

func nextVariantID(id int64) int64 {
	if id >= variantIDBase+variantIDSpace {
		return variantIDBase + 1
	}
	return id + 1
}

// IDFor returns the stable Media/Part id of a non-Auto variant, or zero
// when the durable registry is unavailable.
func (r *VariantIDs) IDFor(key core.RatingKey, releaseID string) int64 {
	ref := VariantRef{Key: key, ReleaseID: releaseID}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ids, _ := r.IDsFor(ctx, []VariantRef{ref})
	return ids[ref]
}

// IDsFor reads a page's existing mappings by pair and inserts only misses.
// Chunks stay below both databases' bind limits, and unique constraints
// arbitrate collisions and allocations made concurrently on other nodes.
func (r *VariantIDs) IDsFor(ctx context.Context, refs []VariantRef) (map[VariantRef]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[VariantRef]int64, len(refs))
	if r.db == nil {
		for _, ref := range refs {
			if _, ok := out[ref]; ok {
				continue
			}
			id, ok := r.byPair[ref]
			if !ok {
				id = VariantHash(ref.Key, ref.ReleaseID)
				for {
					if _, occupied := r.byID[id]; !occupied {
						break
					}
					id = nextVariantID(id)
				}
				r.cacheLocked(id, variantEntry{ref: ref})
			}
			out[ref] = id
		}
		return out, nil
	}
	missing := make(map[VariantRef]int64)
	for _, ref := range refs {
		if id, ok := r.byPair[ref]; ok {
			out[ref] = id
			r.touchLocked(id)
		} else {
			missing[ref] = VariantHash(ref.Key, ref.ReleaseID)
		}
	}
	for len(missing) > 0 {
		batch := make([]VariantRef, 0, variantTouchChunk)
		for ref := range missing {
			batch = append(batch, ref)
			if len(batch) == cap(batch) {
				break
			}
		}
		read := func() error {
			return r.readPairs(ctx, batch, func(id int64, e variantEntry) {
				r.cacheLocked(id, e)
				r.touchLocked(id)
				out[e.ref] = id
				delete(missing, e.ref)
			})
		}
		if err := read(); err != nil {
			return nil, err
		}
		args := make([]any, 0, 4*len(batch))
		pending := batch[:0]
		seen := r.now().Unix()
		for _, ref := range batch {
			if id, ok := missing[ref]; ok {
				args = append(args, id, int64(ref.Key), ref.ReleaseID, seen)
				pending = append(pending, ref)
			}
		}
		batch = pending
		if len(batch) == 0 {
			continue
		}
		values := strings.TrimSuffix(strings.Repeat("(?,?,?,?),", len(batch)), ",")
		inserted, err := r.db.ExecContext(ctx, `INSERT INTO variants_ids(media_id,rating_key,release_id,last_seen) VALUES `+values+` ON CONFLICT DO NOTHING`, args...)
		if err != nil {
			return nil, err
		}
		if n, err := inserted.RowsAffected(); err == nil && n == int64(len(batch)) {
			// Every proposed mapping was inserted. Look up the durable winner
			// only if a concurrent writer or an ID collision rejected a row.
			for _, ref := range batch {
				id := missing[ref]
				r.cacheLocked(id, variantEntry{ref: ref, seen: seen})
				out[ref] = id
				delete(missing, ref)
			}
			continue
		}
		if err := read(); err != nil {
			return nil, err
		}
		for _, ref := range batch {
			if id, ok := missing[ref]; ok {
				missing[ref] = nextVariantID(id)
			}
		}
	}
	return out, nil
}

// readPairs joins a bounded VALUES list against the unique pair index.
func (r *VariantIDs) readPairs(ctx context.Context, refs []VariantRef, visit func(int64, variantEntry)) error {
	args := make([]any, 0, len(refs)*2)
	for _, ref := range refs {
		args = append(args, int64(ref.Key), ref.ReleaseID)
	}
	values := strings.TrimSuffix(strings.Repeat("(CAST(? AS BIGINT),CAST(? AS TEXT)),", len(refs)), ",")
	rows, err := r.db.QueryContext(ctx, `WITH wanted(rating_key,release_id) AS (VALUES `+values+`)
		SELECT v.media_id,v.rating_key,v.release_id,v.last_seen FROM wanted w
		JOIN variants_ids v ON v.rating_key=w.rating_key AND v.release_id=w.release_id`, args...)
	if err != nil {
		return fmt.Errorf("read variant IDs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var e variantEntry
		if err := rows.Scan(&id, &e.ref.Key, &e.ref.ReleaseID, &e.seen); err != nil {
			return err
		}
		visit(id, e)
	}
	return rows.Err()
}

func (r *VariantIDs) Lookup(id int64) (VariantRef, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadIDLocked(id)
	e, ok := r.byID[id]
	if ok {
		r.touchLocked(id)
	}
	return e.ref, ok
}

type VariantRef = frontend.VariantRef
