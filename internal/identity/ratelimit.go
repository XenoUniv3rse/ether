package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("identity_ratelimit", 1, `CREATE TABLE identity_rate_buckets (
 bucket_key TEXT PRIMARY KEY,
 window_start INTEGER NOT NULL,
 attempts INTEGER NOT NULL
 );
CREATE INDEX identity_rate_buckets_window ON identity_rate_buckets(window_start);`)
	store.Register("identity_ratelimit", 2, `ALTER TABLE identity_rate_buckets ADD COLUMN namespace TEXT NOT NULL DEFAULT 'login'; CREATE INDEX identity_rate_buckets_namespace_window ON identity_rate_buckets(namespace,window_start);`)
	store.Register("identity_ratelimit", 3, `ALTER TABLE identity_rate_buckets ADD COLUMN bucket_kind TEXT NOT NULL DEFAULT ''; ALTER TABLE identity_rate_buckets ADD COLUMN remote_ip TEXT NOT NULL DEFAULT ''; ALTER TABLE identity_rate_buckets ADD COLUMN identity_label TEXT NOT NULL DEFAULT ''; ALTER TABLE identity_rate_buckets ADD COLUMN app_name TEXT NOT NULL DEFAULT '';`)
}

type rateLimitAppKey struct{}

// WithRateLimitApp attaches the client app or product name to ctx so
// rate-limit lockouts can record and log which app tripped the limit.
func WithRateLimitApp(ctx context.Context, app string) context.Context {
	app = sanitizeRateLabel(app, 128)
	if app == "" {
		return ctx
	}
	return context.WithValue(ctx, rateLimitAppKey{}, app)
}

func rateLimitApp(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(rateLimitAppKey{}).(string); ok {
		return v
	}
	return ""
}

func sanitizeRateLabel(s string, maxBytes int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s))
	if len(s) <= maxBytes {
		return s
	}
	for len(s) > maxBytes {
		_, size := utf8.DecodeLastRuneInString(s)
		if size <= 0 {
			break
		}
		s = s[:len(s)-size]
	}
	return strings.TrimSpace(s)
}

// safeIdentityLabel returns a displayable identity label for storage in
// identity_rate_buckets. Poll buckets may be keyed by opaque Quick Connect
// secrets or PIN handles, which are never persisted in plaintext.
func safeIdentityLabel(namespace, identity string) string {
	if namespace == "poll" {
		if identity == "refresh" || strings.HasPrefix(identity, "trakt:") {
			return sanitizeRateLabel(identity, 128)
		}
		return ""
	}
	return sanitizeRateLabel(identity, 128)
}

// RateLimitEvent describes a rejected rate-limit attempt.
type RateLimitEvent struct {
	Namespace  string
	BucketKind string
	RemoteIP   string
	Identity   string
	App        string
	Attempts   int
	WindowSec  int
	Remaining  time.Duration
	FirstInWin bool
}

// RateLockout is an active rate-limit bucket that has reached its limit.
type RateLockout struct {
	Key         string        `json:"key"`
	Namespace   string        `json:"namespace"`
	Scope       string        `json:"scope"`
	RemoteIP    string        `json:"remoteIp,omitempty"`
	Identity    string        `json:"identity,omitempty"`
	App         string        `json:"app,omitempty"`
	Attempts    int           `json:"attempts"`
	Limit       int           `json:"limit"`
	Remaining   time.Duration `json:"-"`
	RemainingMs int64         `json:"remainingMs"`
	ExpiresAt   time.Time     `json:"expiresAt"`
}

// LoginLimiter counts attempts against both the client IP and the combination
// of identity and IP. Database updates serialize concurrent cluster nodes.
// Each call represents one attempt; ResetIdentity clears only the identity+IP
// bucket after a successful sign-in while leaving the IP-wide bucket intact.
type LoginLimiter struct {
	db                   *sql.DB
	now                  func() time.Time
	window               time.Duration
	perIP, perIdentityIP int
	namespace            string
	onLimited            atomic.Pointer[func(RateLimitEvent)]
}

// SetOnLimited installs an optional non-blocking observer notified whenever
// AllowWithRetry rejects an attempt due to rate limiting.
func (l *LoginLimiter) SetOnLimited(fn func(RateLimitEvent)) {
	if l == nil {
		return
	}
	if fn == nil {
		l.onLimited.Store(nil)
		return
	}
	l.onLimited.Store(&fn)
}

func NewLoginLimiter(db *sql.DB, window time.Duration, perIP, perIdentityIP int, now func() time.Time) (*LoginLimiter, error) {
	if db == nil || window < time.Millisecond || perIP <= 0 || perIdentityIP <= 0 {
		return nil, errors.New("identity: invalid login limiter configuration")
	}
	if now == nil {
		now = time.Now
	}
	return &LoginLimiter{db: db, now: now, window: window, perIP: perIP, perIdentityIP: perIdentityIP, namespace: "login"}, nil
}

// NewPollLimiter uses a separate persistent bucket namespace from login
// attempts. Use it for PIN and Quick Connect polling, keyed by the opaque
// handle or polling secret. Multiple nodes share the same limits.
func NewPollLimiter(db *sql.DB, window time.Duration, perIP, perHandleIP int, now func() time.Time) (*LoginLimiter, error) {
	l, err := NewLoginLimiter(db, window, perIP, perHandleIP, now)
	if err != nil {
		return nil, err
	}
	l.namespace = "poll"
	return l, nil
}

func rateKey(kind string, parts ...string) string {
	h := sha256.New()
	for _, part := range append([]string{kind}, parts...) {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Allow records an attempt before verification. It fails closed on database
// errors; callers must not skip this step for valid passwords or PINs.
func (l *LoginLimiter) Allow(ctx context.Context, identity string, ip netip.Addr) (bool, error) {
	allowed, _, err := l.AllowWithRetry(ctx, identity, ip)
	return allowed, err
}

// IPExceeded reports whether ip has already reached the per-IP limit in the
// active window without recording another attempt.
func (l *LoginLimiter) IPExceeded(ctx context.Context, ip netip.Addr) (bool, error) {
	if !ip.IsValid() {
		return false, errors.New("identity: invalid limiter key")
	}
	addr := ip.Unmap().String()
	cutoff := l.now().UnixMilli() - l.window.Milliseconds()
	var attempts int
	err := l.db.QueryRowContext(ctx, `SELECT attempts FROM identity_rate_buckets WHERE bucket_key=? AND namespace=? AND window_start>?`, rateKey(l.namespace, "ip", addr), l.namespace, cutoff).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return attempts >= l.perIP, nil
}

// AllowWithRetry records the same attempt as Allow and reports the remaining
// fixed-window delay when either bucket is exhausted. It never resets a bucket.
func (l *LoginLimiter) AllowWithRetry(ctx context.Context, identity string, ip netip.Addr) (bool, time.Duration, error) {
	if !ip.IsValid() || len(identity) > 1024 {
		return false, 0, errors.New("identity: invalid limiter key")
	}
	addr := ip.Unmap().String()
	app := rateLimitApp(ctx)
	idLabel := safeIdentityLabel(l.namespace, identity)
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()
	now := l.now().UnixMilli()
	cutoff := now - l.window.Milliseconds()
	update := func(key, kind, label string) (int, int64, string, error) {
		var n int
		var start int64
		var storedApp string
		err := tx.QueryRowContext(ctx, `INSERT INTO identity_rate_buckets(bucket_key,window_start,attempts,namespace,bucket_kind,remote_ip,identity_label,app_name) VALUES(?,?,1,?,?,?,?,?)
ON CONFLICT(bucket_key) DO UPDATE SET
 attempts=CASE WHEN identity_rate_buckets.window_start<=? THEN 1 ELSE identity_rate_buckets.attempts+1 END,
 window_start=CASE WHEN identity_rate_buckets.window_start<=? THEN ? ELSE identity_rate_buckets.window_start END,
 bucket_kind=excluded.bucket_kind,
 remote_ip=excluded.remote_ip,
 identity_label=excluded.identity_label,
 app_name=CASE WHEN identity_rate_buckets.window_start<=? THEN excluded.app_name WHEN excluded.app_name<>'' THEN excluded.app_name ELSE identity_rate_buckets.app_name END
RETURNING attempts,window_start,app_name`, key, now, l.namespace, kind, addr, label, app, cutoff, cutoff, now, cutoff).Scan(&n, &start, &storedApp)
		return n, start, storedApp, err
	}
	ipCount, ipStart, ipApp, err := update(rateKey(l.namespace, "ip", addr), "ip", "")
	if err != nil {
		return false, 0, err
	}
	pairCount, pairStart, pairApp, err := update(rateKey(l.namespace, "identity-ip", identity, addr), "identity-ip", idLabel)
	if err != nil {
		return false, 0, err
	}
	if err = tx.Commit(); err != nil {
		return false, 0, err
	}
	// The poll limiter is used by read endpoints. App maintenance sweeps
	// those buckets separately from the required admission writes.
	if l.namespace != "poll" {
		_, _ = l.Cleanup(ctx, 16)
	}
	var retry time.Duration
	if ipCount > l.perIP {
		retry = max(retry, time.Duration(ipStart+l.window.Milliseconds()-now)*time.Millisecond)
	}
	if pairCount > l.perIdentityIP {
		retry = max(retry, time.Duration(pairStart+l.window.Milliseconds()-now)*time.Millisecond)
	}
	allowed := ipCount <= l.perIP && pairCount <= l.perIdentityIP
	if !allowed {
		if fn := l.onLimited.Load(); fn != nil && *fn != nil {
			attempts := ipCount
			if pairCount > attempts {
				attempts = pairCount
			}
			winSec := int(l.window / time.Second)
			if winSec <= 0 {
				winSec = 1
			}
			bucketKind := "identity-ip"
			evIdentity := idLabel
			if ipCount > l.perIP {
				bucketKind = "ip"
				evIdentity = ""
			}
			evApp := app
			if evApp == "" {
				evApp = pairApp
			}
			if evApp == "" {
				evApp = ipApp
			}
			ipNewlyLocked := ipCount == l.perIP+1
			pairNewlyLocked := pairCount == l.perIdentityIP+1
			(*fn)(RateLimitEvent{
				Namespace:  l.namespace,
				BucketKind: bucketKind,
				RemoteIP:   addr,
				Identity:   evIdentity,
				App:        evApp,
				Attempts:   attempts,
				WindowSec:  winSec,
				Remaining:  retry,
				FirstInWin: ipNewlyLocked || (pairNewlyLocked && ipCount <= l.perIP),
			})
		}
	}
	return allowed, retry, nil
}

// ResetIdentity clears the identity+IP bucket for identity from ip after a
// successful sign-in. The IP-wide bucket is left intact so brute-force
// attempts against other identities from the same IP remain bounded.
func (l *LoginLimiter) ResetIdentity(ctx context.Context, identity string, ip netip.Addr) error {
	if l == nil || !ip.IsValid() || len(identity) > 1024 {
		return nil
	}
	addr := ip.Unmap().String()
	_, err := l.db.ExecContext(ctx, `DELETE FROM identity_rate_buckets WHERE bucket_key=? AND namespace=?`, rateKey(l.namespace, "identity-ip", identity, addr), l.namespace)
	return err
}

// ListLockouts returns active buckets in this limiter's namespace that have
// reached their lockout limit and have not yet expired.
func (l *LoginLimiter) ListLockouts(ctx context.Context) ([]RateLockout, error) {
	if l == nil {
		return []RateLockout{}, nil
	}
	now := l.now().UnixMilli()
	winMs := l.window.Milliseconds()
	cutoff := now - winMs
	legacyLimit := min(l.perIP, l.perIdentityIP)
	rows, err := l.db.QueryContext(ctx, `SELECT bucket_key, namespace, bucket_kind, remote_ip, identity_label, app_name, window_start, attempts
FROM identity_rate_buckets
WHERE namespace=? AND window_start>?
  AND ((bucket_kind='ip' AND attempts>=?) OR (bucket_kind='identity-ip' AND attempts>=?) OR (bucket_kind='' AND attempts>=?))
ORDER BY window_start DESC, bucket_key ASC
LIMIT 200`, l.namespace, cutoff, l.perIP, l.perIdentityIP, legacyLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RateLockout{}
	for rows.Next() {
		var item RateLockout
		var startMs int64
		if err := rows.Scan(&item.Key, &item.Namespace, &item.Scope, &item.RemoteIP, &item.Identity, &item.App, &startMs, &item.Attempts); err != nil {
			return nil, err
		}
		expiresMs := startMs + winMs
		remMs := expiresMs - now
		if remMs <= 0 {
			continue
		}
		switch item.Scope {
		case "ip":
			item.Limit = l.perIP
		case "identity-ip":
			item.Limit = l.perIdentityIP
		default:
			item.Limit = legacyLimit
		}
		item.Remaining = time.Duration(remMs) * time.Millisecond
		item.RemainingMs = remMs
		item.ExpiresAt = time.UnixMilli(expiresMs).UTC()
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ClearLockout deletes one rate-limit bucket in this limiter's namespace.
func (l *LoginLimiter) ClearLockout(ctx context.Context, key string) (bool, error) {
	if l == nil || strings.TrimSpace(key) == "" {
		return false, nil
	}
	res, err := l.db.ExecContext(ctx, `DELETE FROM identity_rate_buckets WHERE bucket_key=? AND namespace=?`, key, l.namespace)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ClearAll deletes all rate-limit buckets in this limiter's namespace.
func (l *LoginLimiter) ClearAll(ctx context.Context) (int64, error) {
	if l == nil {
		return 0, nil
	}
	res, err := l.db.ExecContext(ctx, `DELETE FROM identity_rate_buckets WHERE namespace=?`, l.namespace)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Cleanup removes at most limit expired buckets. The predicate is repeated
// outside the selection so a bucket refreshed by another node is retained.
func (l *LoginLimiter) Cleanup(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 || limit > 1000 {
		return 0, errors.New("identity: invalid cleanup limit")
	}
	cutoff := l.now().UnixMilli() - l.window.Milliseconds()
	r, err := l.db.ExecContext(ctx, `DELETE FROM identity_rate_buckets WHERE namespace=? AND window_start<=? AND bucket_key IN (SELECT bucket_key FROM identity_rate_buckets WHERE namespace=? AND window_start<=? ORDER BY window_start,bucket_key LIMIT ?)`, l.namespace, cutoff, l.namespace, cutoff, limit)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}
