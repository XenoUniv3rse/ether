package vela

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func init() {
	frontend.RegisterMigration("vela_replays", 1, `CREATE TABLE vela_replays (key TEXT PRIMARY KEY, digest TEXT NOT NULL, response BLOB, expires INTEGER NOT NULL); CREATE INDEX vela_replays_expiry ON vela_replays(expires);`)
}

type savedResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

func encryption(key []byte) (cipher.AEAD, error) {
	c, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(c)
}

// A cached refresh result must still belong to a live session when replayed.
func (i *instance) writeMutationReplay(w http.ResponseWriter, r *http.Request, d device, result savedResponse) {
	if r.Pattern == "POST /vela/v0/auth/refresh" && result.Status == http.StatusOK {
		var tokens struct {
			AccessToken string `json:"accessToken"`
		}
		if err := json.Unmarshal(result.Body, &tokens); err != nil {
			fail(w, err, r)
			return
		}
		if _, err := i.grantScope(r.Context(), tokens.AccessToken, d.neutral()); err != nil {
			fail(w, err, r)
			return
		}
	}
	for k, v := range result.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(result.Status)
	_, _ = w.Write(result.Body)
}

// Replay contract for clients. A mutation retried with the same key either
// replays the recorded outcome or runs again; it never stays unresolved:
//   - auth mutations (sign-in, refresh, pairing) finish even if the caller
//     disconnects, so a retried refresh with the same key gets the same
//     rotated tokens while that access grant and family remain live;
//   - an outcome that did not commit (cancelled, 5xx on auth, any refusal
//     with Retry-After) frees the key for the retry;
//   - while another attempt still holds the key the answer is 409
//     mutation_outcome_unknown with retry.allowed and Retry-After, at most
//     replayLease.
//
// Refresh reuse: presenting an already-rotated refresh token under a new key
// is treated as theft. identity.Grants.Refresh revokes the family and its
// current access grant and answers 401, so the device signs in again.
//
// replayLease bounds a pending claim. A claim whose owner neither recorded
// an outcome nor released it (the process died mid-mutation, or both writes
// failed) is taken over by the next request with the same key once the lease
// lapses, so no key is ever stuck. In-process duplicates are already excluded
// by i.replays while the owner runs; the lease only arbitrates crashes and
// other cluster nodes. It must exceed authMutationBound.
const replayLease = time.Minute

// replayRetention is how long a recorded outcome answers retries.
const replayRetention = 24 * time.Hour

// replaySweepEvery throttles the expired-row sweep. It runs beside the
// request that triggered it, never in front of it: the sweep takes the sqlite
// writer, and on the lab a scan here held it 6 s and timed out a refresh.
const replaySweepEvery = time.Minute

// pendingError is a claim another owner still holds. It always resolves:
// the owner records or releases it, or the lease lapses.
type pendingError struct{ retryAfter time.Duration }

func (pendingError) Error() string        { return "mutation outcome pending" }
func (pendingError) Is(target error) bool { return target == errPending }

// claim persists before admission. A pending claim makes concurrent retries
// on other nodes wait instead of admitting a duplicate; see replayLease.
func (i *instance) claim(ctx context.Context, key, digest string) (*savedResponse, bool, error) {
	if i.db == nil || i.seal == nil {
		return nil, false, errors.New("mutation persistence unavailable")
	}
	now := time.Now().UnixMilli()
	i.sweepReplays(now)
	// A lapsed row (an expired outcome, or a pending claim past its lease) is
	// taken over in the same statement, so the sweep is never on this path.
	res, e := i.db.ExecContext(ctx, `INSERT INTO vela_replays(key,digest,expires) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET digest=excluded.digest,response=NULL,expires=excluded.expires WHERE vela_replays.expires<=?`, key, digest, now+replayLease.Milliseconds(), now)
	if e != nil {
		return nil, false, e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return nil, false, e
	}
	if n == 1 {
		return nil, false, nil
	}
	var stored string
	var data []byte
	var expires int64
	e = i.db.QueryRowContext(ctx, `SELECT digest,response,expires FROM vela_replays WHERE key=?`, key).Scan(&stored, &data, &expires)
	if errors.Is(e, sql.ErrNoRows) {
		// Released between the two statements: the owner did not commit.
		return i.claim(ctx, key, digest)
	}
	if e != nil {
		return nil, false, e
	}
	if stored != digest {
		return nil, true, nil
	}
	if len(data) < i.seal.NonceSize() {
		wait := time.Duration(expires-now) * time.Millisecond
		if wait < time.Second {
			wait = time.Second
		}
		return nil, false, pendingError{retryAfter: wait}
	}
	plain, e := i.seal.Open(nil, data[:i.seal.NonceSize()], data[i.seal.NonceSize():], []byte(key))
	if e != nil {
		return nil, false, e
	}
	var out savedResponse
	e = json.Unmarshal(plain, &out)
	return &out, false, e
}

func (i *instance) sweepReplays(now int64) {
	last := i.replaySwept.Load()
	if now-last < replaySweepEvery.Milliseconds() || !i.replaySwept.CompareAndSwap(last, now) {
		return
	}
	db := i.db
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// ORDER BY expires pins the sweep to vela_replays_expiry; without it
		// the planner scanned every stored response to find nothing.
		_, _ = db.ExecContext(ctx, `DELETE FROM vela_replays WHERE key IN (SELECT key FROM vela_replays WHERE expires<=? ORDER BY expires LIMIT 100)`, now)
	}()
}

// release drops a claim whose mutation did not commit, so the same key
// executes again instead of answering an outcome nobody recorded.
func (i *instance) release(ctx context.Context, key string) error {
	_, e := i.db.ExecContext(ctx, `DELETE FROM vela_replays WHERE key=? AND response IS NULL`, key)
	return e
}

// persistContext outlives the caller's connection: a client that gave up
// must still leave its key recorded or released, never pending.
func persistContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
}

func pendingProblem(w http.ResponseWriter, err error) {
	wait := replayLease
	var p pendingError
	if errors.As(err, &p) {
		wait = p.retryAfter
	}
	seconds := int((wait + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	send(w, 409, object{"error": object{"class": "conflict", "code": "mutation_outcome_unknown", "message": "mutation outcome pending", "retry": object{"allowed": true, "afterSeconds": seconds}}})
}

var errPending = errors.New("mutation outcome pending")

// save records the outcome for replay until the later of until and the
// default retention.
func (i *instance) save(ctx context.Context, key string, out savedResponse, until time.Time) error {
	b, e := json.Marshal(out)
	if e != nil {
		return e
	}
	nonce := make([]byte, i.seal.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return e
	}
	data := i.seal.Seal(nonce, nonce, b, []byte(key))
	if min := time.Now().Add(replayRetention); until.Before(min) {
		until = min
	}
	_, e = i.db.ExecContext(ctx, `UPDATE vela_replays SET response=?,expires=? WHERE key=?`, data, until.UnixMilli(), key)
	return e
}
