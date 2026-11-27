package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Status classifies a finished call, following appkit-sandbox.
type Status string

const (
	StatusOK           Status = "ok"
	StatusInvalidInput Status = "invalid_input"
	StatusError        Status = "error"
	StatusTimeout      Status = "timeout"
	StatusForbidden    Status = "forbidden"
	StatusAborted      Status = "aborted"
	StatusGovernance   Status = "governance"
)

// Fault is a classified, non-ambient failure. A Host returns a *Fault to make
// the call end with that status; Code is one of the Status values other than
// ok (usually forbidden), or an application code the plugin can catch.
type Fault struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	RetryAfterSec int64  `json:"retryAfterSec,omitempty"`
	RetryAt       string `json:"retryAt,omitempty"`
}

func (f *Fault) Error() string { return f.Code + ": " + f.Message }

// Forbidden builds a forbidden Fault, e.g. for a revoked permission.
func Forbidden(message string) *Fault { return &Fault{Code: string(StatusForbidden), Message: message} }

// ErrPoolBusy is wrapped by an *Error when a call times out waiting for a free
// WASM instance in the pool. Pool contention is not a plugin crash.
var ErrPoolBusy = errors.New("timed out waiting for a plugin instance")

// ErrClosed is wrapped by an *Error when a call cannot run because the plugin
// runtime was closed.
var ErrClosed = errors.New("plugin is closed")

// ErrRequestEncode is wrapped by an *Error when the host cannot encode the
// request for the guest (e.g. a body that is not valid JSON). The guest
// never ran, so it is not a plugin crash.
var ErrRequestEncode = errors.New("host could not encode the plugin request")

// Error is the error type returned by Plugin calls. It carries the Outcome so
// WS7's health loop can read the status, trap flag and cost from one value.
type Error struct {
	Status  Status
	Message string
	Trap    bool  // the instance trapped (panic in guest) or was killed by a deadline
	Err     error // optional wrapped sentinel (e.g. ErrPoolBusy)
}

func (e *Error) Error() string { return "wasm: " + string(e.Status) + ": " + e.Message }
func (e *Error) Unwrap() error { return e.Err }

// Outcome accounts for one finished call.
type Outcome struct {
	Status         Status
	Units          int64 // host-call units plus wall-time units
	Duration       time.Duration
	Trap           bool
	SlowCandidates int // hot path: candidates that went neutral on budget
	MemoHits       int // hot path: candidates served from the memo
}

// FrontendUpload describes a staged raw body or multipart files and text fields.
type FrontendUpload struct {
	ContentType string               `json:"contentType,omitempty"`
	Blob        *FrontendUploadBlob  `json:"blob,omitempty"`
	Files       []FrontendUploadBlob `json:"files,omitempty"`
	Fields      map[string][]string  `json:"fields,omitempty"`
}

type FrontendUploadBlob struct {
	Ref         string `json:"ref"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	SHA1        string `json:"sha1"`
	MD5         string `json:"md5"`
	CRC32       string `json:"crc32"`
	ContentType string `json:"contentType,omitempty"`
	Field       string `json:"field,omitempty"`
	Filename    string `json:"filename,omitempty"`
}

// Request is the HTTP-shaped plugin request (SPEC 5.1.2 / 5.2).
type Request struct {
	Op      string              `json:"op,omitempty"`
	ID      string              `json:"id,omitempty"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   map[string][]string `json:"query,omitempty"`
	Headers map[string]string   `json:"headers,omitempty"`
	Body    json.RawMessage     `json:"body,omitempty"`
	// BodyBase64 is the exact request bytes when the caller needs them
	// byte-for-byte; ContentType is the caller's Content-Type. A non-JSON
	// body arrives only in BodyBase64.
	BodyBase64  string          `json:"bodyBase64,omitempty"`
	ContentType string          `json:"contentType,omitempty"`
	Upload      *FrontendUpload `json:"upload,omitempty"`
	// Foreground keeps an /ether/job/ call off the background throttle
	// because a viewer is waiting on it. Host-side only.
	Foreground bool `json:"-"`
	// Deadline optionally overrides the call deadline for this invocation
	// (e.g. a live per-plugin override, or an opted-in AI search route).
	// Zero uses the plugin's Limits. Host-side only.
	Deadline time.Duration `json:"-"`
	// AISearch marks a search route whose plugin has active permissions.ai:
	// "complete+search" so Deadline may extend the baseline search route
	// deadline up to the AI search bound (still capped by the plugin's
	// granted deadline). Host-side only.
	AISearch bool `json:"-"`
}

// Response is the plugin's answer. Body is the raw JSON the plugin produced
// as its body field (usually a JSON string), passed through untouched.
type Response struct {
	Blob    json.RawMessage   `json:"blob,omitempty"`
	Media   json.RawMessage   `json:"media,omitempty"`
	Art     json.RawMessage   `json:"art,omitempty"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

// Host is the plugin-to-Ether service surface (SPEC 5.1.5). Name is a dotted
// service method: runtime.limits, kv.get, kv.put, kv.delete, kv.list, kv.usage, badges.set, badges.clear, badges.usage, secret.get, secret.set, secret.delete,
// http.fetch, http.fetchMany, http.fetchToCache, archive.list, archive.open, archive.read, archive.extract, catalog.search, catalog.get, viewers.history,
// viewers.setWatchState, collections.upsert, kinds.upsert, kinds.delete,
// kinds.items, sources.resolve, cache.put, cache.open, cache.read, cache.stat, cache.rasterize, cache.delete, frontend.streamURL,
// folders.list, folders.stat, events.publish, notify.owner, log. Args is a JSON array of positional
// arguments; the result is any JSON value. Return a *Fault to classify the
// failure (forbidden for a missing or revoked grant).
type Host interface {
	CallHost(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
}

// HostFunc adapts a function to Host.
type HostFunc func(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)

// Default host-call unit costs, in the spirit of appkit-sandbox's priced
// host functions. Wall time adds 1 unit per WallUnitInterval; http.fetchMany
// adds FetchManyUnitsPerRequest for each request after the first; archive.read
// adds ArchiveReadUnitsPerMember per requested member; archive.extract
// additionally charges ArchiveExtractUnitsPerMiB per started MiB streamed.
var DefaultCosts = map[string]int64{
	"log": 1, "runtime.limits": 1, "kv.get": 5, "kv.put": 10, "kv.delete": 5, "kv.list": 10, "kv.usage": 5, "badges.set": 20, "badges.clear": 10, "badges.usage": 5,
	"secret.get": 5, "secret.set": 10, "secret.delete": 5, "http.fetch": 100, "http.fetchMany": 100, "http.fetchToCache": 100, "http.hash": 100, "cache.read": 20, "cache.stat": 20, "cache.rasterize": 50, "archive.list": 50, "archive.open": 50, "archive.read": 50, "archive.extract": 50, "addon.get": 100, "catalog.search": 50, "catalog.get": 20,
	"viewers.history": 50, "viewers.setWatchState": 50, "collections.upsert": 50,
	"kinds.upsert": 50, "kinds.delete": 50, "kinds.items": 20, "kinds.visible": 15, "kinds.browse": 20,
	"kinds.get": 15, "kinds.state": 15, "kinds.favorite": 20, "kinds.played": 20, "kinds.rate": 20, "kinds.favorites": 20,
	"kinds.playlists": 20, "kinds.playlist": 20, "kinds.playlistCreate": 50, "kinds.playlistUpdate": 50, "kinds.playlistDelete": 20, "kinds.expand": 50, "kinds.search": 50,
	"sources.resolve": 100, "cache.put": 50, "cache.open": 20, "cache.delete": 10, "frontend.streamURL": 5,
	"folders.frame": 20, "folders.hashStart": 20, "folders.hashPoll": 20, "folders.hashCancel": 20, "folders.hash": 20, "folders.list": 20, "folders.stat": 20, "events.publish": 20, "notify.owner": 20, "settings.get": 5,
	"ai.complete": 200, "media.probe": 100, "screens.invalidate": 10, "screens.putRecords": 20,
}

const (
	// DefaultRankPerCandidate and DefaultRankBatch are the hot-path budgets
	// (SPEC §5.2). A manifest may lower them, never raise them.
	DefaultRankPerCandidate = 2 * time.Millisecond
	DefaultRankBatch        = 50 * time.Millisecond
	// WallUnitInterval is the wall time worth one unit.
	WallUnitInterval = 100 * time.Microsecond
	// BaseCallUnits is charged once per handle call.
	BaseCallUnits = 10
	// FetchManyUnitsPerRequest is charged for each request beyond the first in
	// http.fetchMany on top of http.fetchMany's 100-unit base call cost, so a
	// batch of N requests pays N * 100 units just like N http.fetch calls.
	FetchManyUnitsPerRequest = 100
	// ArchiveReadUnitsPerMember is charged per requested item in archive.read
	// on top of archive.read's 50-unit base call cost, so large batches pay
	// proportional governance units instead of dodging per-call budgets.
	ArchiveReadUnitsPerMember = 2
	// ArchiveExtractUnitsPerMiB is charged per started MiB of uncompressed
	// archive member streamed by archive.extract on top of its base call cost.
	ArchiveExtractUnitsPerMiB = 10
	// MaxMemoryBytes is the hard ceiling for a plugin instance.
	MaxMemoryBytes = 4 << 30
	// DefaultPoolSize, DefaultPoolCeiling and MaxPoolSize bound instances per
	// hosted plugin pool. Plugins get DefaultPoolSize by default, manifest
	// requests clamp to the owner ceiling (default DefaultPoolCeiling), and
	// owners may raise the ceiling up to MaxPoolSize.
	DefaultPoolSize    = 2
	DefaultPoolCeiling = 4
	MaxPoolSize        = 16
	// MinDataReadBytes and MaxDataReadBytes bound the per-call readBytes grant
	// derived from memoryBytes: clamp(memoryBytes/4, 8 MiB, 512 MiB).
	MinDataReadBytes = 8 << 20
	MaxDataReadBytes = 512 << 20
	// MaxResponseBytes caps a plugin response.
	MaxResponseBytes = 8 << 20
	// MaxFetchBodyBytes caps a direct HTTP response before JSON encoding.
	MaxFetchBodyBytes = 10 << 20
	// MaxHostResultBytes is the baseline host-result floor; actual per-call
	// host results scale with HostResultBytes(memoryBytes) so base64-encoded
	// data reads up to readBytes always fit their JSON envelope.
	MaxHostResultBytes = 4 << 20
	maxLogsPerCall     = 100
)

// DataReadBytes computes the per-call byte ceiling for cache.read,
// archive.open, and archive.read from a plugin's memory grant:
// clamp(memoryBytes/4, 8 MiB, 512 MiB), treating 0 as the 32 MiB default.
func DataReadBytes(memoryBytes uint64) int64 {
	if memoryBytes == 0 {
		memoryBytes = 32 << 20
	}
	quarter := memoryBytes / 4
	if quarter < MinDataReadBytes {
		return MinDataReadBytes
	}
	if quarter > MaxDataReadBytes {
		return MaxDataReadBytes
	}
	return int64(quarter)
}

// HostResultBytes returns the maximum JSON host-result envelope for a plugin
// with the given memory grant, including base64 expansion of DataReadBytes
// plus 1 MiB for JSON framing.
func HostResultBytes(memoryBytes uint64) int64 {
	readCap := DataReadBytes(memoryBytes)
	envelope := ((readCap+2)/3)*4 + (1 << 20)
	if envelope < MaxHostResultBytes {
		return MaxHostResultBytes
	}
	return envelope
}

type unitChargerKey struct{}
type callLimitsKey struct{}
type backgroundCallKey struct{}
type callPathKey struct{}

// WithBackgroundCall marks ctx as belonging to a background plugin call
// (such as a scheduled route, background job, or catalog sync).
func WithBackgroundCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundCallKey{}, true)
}

// IsBackgroundCall reports whether ctx belongs to a background hosted plugin
// call (non-foreground /ether/job/*, kind sync, Live TV sync, /ether/event, or
// a call marked with WithBackgroundCall).
func IsBackgroundCall(ctx context.Context) bool {
	bg, _ := ctx.Value(backgroundCallKey{}).(bool)
	return bg
}

// CallPath returns the route path of the active hosted plugin call, or "" outside
// a hosted call.
func CallPath(ctx context.Context) string {
	p, _ := ctx.Value(callPathKey{}).(string)
	return p
}

// hostCallTimeout returns the default per-host-call deadline ceiling for a
// guest host service call. Even when a background route has a multi-hour
// granted deadline, an individual host call is bounded so one hung upstream
// cannot hold a plugin pool instance forever; http.fetchToCache in a job,
// schedule, or background context may raise its timeout via timeoutMs up to the
// calling job's remaining deadline.
var hostCallTimeoutOverride func(name string) time.Duration

func hostCallTimeout(name string) time.Duration {
	if fn := hostCallTimeoutOverride; fn != nil {
		if d := fn(name); d > 0 {
			return d
		}
	}
	switch name {
	case "http.fetch":
		return 60 * time.Second
	case "http.fetchMany":
		return 2 * time.Minute
	case "sources.resolve", "ai.complete":
		return 35 * time.Second
	case "media.probe":
		return 15 * time.Second
	case "http.fetchToCache", "http.hash",
		"archive.list", "archive.open", "archive.read", "archive.extract",
		"folders.hash", "folders.hashStart", "folders.hashPoll", "folders.hashCancel",
		"folders.list", "folders.stat", "folders.frame",
		"cache.put", "cache.open", "cache.read", "cache.rasterize", "cache.delete":
		return 10 * time.Minute
	default:
		return 30 * time.Second
	}
}

func (cs *callState) allowsLongFetchToCache() bool {
	if cs.path != "" && (ViewerRequestPath(cs.path) || FrontendAdapterPath(cs.path)) {
		return false
	}
	return BackgroundPoolPath(cs.path) || IsBackgroundCall(cs.ctx)
}

func fetchToCacheRequestedTimeout(args json.RawMessage) (time.Duration, bool) {
	var elems []json.RawMessage
	if err := json.Unmarshal(args, &elems); err != nil || len(elems) != 1 {
		return 0, false
	}
	var req struct {
		TimeoutMS int64 `json:"timeoutMs"`
	}
	if err := json.Unmarshal(elems[0], &req); err != nil || req.TimeoutMS <= 0 {
		return 0, false
	}
	const maxMS = int64(math.MaxInt64 / int64(time.Millisecond))
	if req.TimeoutMS > maxMS {
		return time.Duration(math.MaxInt64), true
	}
	return time.Duration(req.TimeoutMS) * time.Millisecond, true
}

func (cs *callState) hostCallTimeout(name string, args json.RawMessage) time.Duration {
	base := hostCallTimeout(name)
	if name != "http.fetchToCache" || !cs.allowsLongFetchToCache() {
		return base
	}
	requested, ok := fetchToCacheRequestedTimeout(args)
	if !ok || requested <= base {
		return base
	}
	if dl, ok := cs.ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 && requested > rem {
			return rem
		}
	}
	return requested
}

// CallLimits returns the effective per-call limits (after route deadline and
// viewer unit-budget clamps) when called inside an active hosted plugin call.
func CallLimits(ctx context.Context) (Limits, bool) {
	lim, ok := ctx.Value(callLimitsKey{}).(Limits)
	return lim, ok
}

// ChargeUnits charges additional governance units to the active guest call,
// returning a StatusGovernance *Fault when the call's UnitBudget is exceeded.
// Outside a governed wasm call it is a no-op.
func ChargeUnits(ctx context.Context, units int64) error {
	if units <= 0 {
		return nil
	}
	fn, _ := ctx.Value(unitChargerKey{}).(func(int64) *Fault)
	if fn == nil {
		return nil
	}
	if f := fn(units); f != nil {
		return f
	}
	return nil
}

// Limits bound one plugin. Zero fields take the defaults in normalize.
type Limits struct {
	MemoryBytes uint64        // per instance; default 32 MiB, max wasm32 4 GiB
	Deadline    time.Duration // per handle call; default 30s
	UnitBudget  int64         // per handle call; default UnitsForDeadline(Deadline)
	Stremio     bool          // plugin answers Stremio add-on routes (uses StremioDeadline)
	Busy        func() bool   // background work yields while playback or host load is high
	PoolSize    int           // instances per pool; default DefaultPoolSize (2), default ceiling DefaultPoolCeiling (4), max MaxPoolSize (16)
	// Hot-path budgets. A manifest may lower these, never raise them:
	// normalize clamps to the defaults below.
	RankPerCandidate time.Duration // default (and max) 2ms
	RankBatch        time.Duration // default (and max) 50ms
	Costs            map[string]int64
	// unclamped lets this package's tests raise the hot-path budgets so
	// correctness checks do not depend on machine speed. Other packages
	// cannot set it, so production budgets are always clamped.
	unclamped bool
}

func (l *Limits) normalize() {
	if l.MemoryBytes == 0 || l.MemoryBytes > MaxMemoryBytes {
		if l.MemoryBytes == 0 {
			l.MemoryBytes = 32 << 20
		} else {
			l.MemoryBytes = MaxMemoryBytes
		}
	}
	if l.Deadline <= 0 {
		l.Deadline = 30 * time.Second
	}
	if l.Deadline > 365*24*time.Hour {
		l.Deadline = 365 * 24 * time.Hour
	}
	if l.UnitBudget <= 0 {
		l.UnitBudget = UnitsForDeadline(l.Deadline)
	}
	if l.PoolSize <= 0 {
		l.PoolSize = DefaultPoolSize
	}
	if l.PoolSize > MaxPoolSize {
		l.PoolSize = MaxPoolSize
	}
	if l.RankPerCandidate <= 0 || (l.RankPerCandidate > DefaultRankPerCandidate && !l.unclamped) {
		l.RankPerCandidate = DefaultRankPerCandidate
	}
	if l.RankBatch <= 0 || (l.RankBatch > DefaultRankBatch && !l.unclamped) {
		l.RankBatch = DefaultRankBatch
	}
	if l.Costs == nil {
		l.Costs = DefaultCosts
	}
}

// hotKill is the hard deadline of one hot-path call: the point where the
// instance is killed because plugin code stopped returning (an endless
// loop). It is not the plugin's budget. The budgets bound the plugin's own
// time and are enforced softly between candidates (see hotPrelude), so a
// batch that keeps to them ends normally; the deadline only has to catch
// code that never yields back. It sits one batch budget past the batch
// budget so the host's own work for the call (encoding and parsing the
// facts) and a busy machine's scheduling pauses cannot turn a well-behaved
// batch into a killed one. A JavaScript batch takes the deadline twice:
// once for the host to hand over the facts, once for the plugin's code.
// When the kill and the budget were the same
// instant, the batch lost every score it had already computed, nothing was
// memoized, and the next resolve started from zero and timed out again.
func (l *Limits) hotKill() time.Duration { return 2 * l.RankBatch }

func (l *Limits) costOf(name string) int64 {
	if c, ok := l.Costs[name]; ok {
		return c
	}
	return 25 // unpriced service: charge a middle rate, never free
}

// PoolHolder names one active caller currently holding a plugin instance.
type PoolHolder struct {
	Route string        `json:"route"`
	Held  time.Duration `json:"held"`
}

// PoolStats is a point-in-time snapshot of a plugin's service instance pool.
type PoolStats struct {
	Size        int          `json:"size"`
	InUse       int          `json:"inUse"`
	Idle        int          `json:"idle"`
	Building    int          `json:"building"`
	Waiting     int          `json:"waiting"`
	Exhaustions int64        `json:"exhaustions"`
	Holders     []PoolHolder `json:"holders,omitempty"`
}

// Stats are the health counters WS7's backoff loop consumes.
type Stats struct {
	Calls          int64
	Errors         int64
	Timeouts       int64
	Traps          int64
	Forbidden      int64
	Aborted        int64
	Governance     int64
	SlowCandidates int64
	Units          int64
	Pool           PoolStats
}

type counters struct {
	calls, errors, timeouts, traps, forbidden, aborted, governance, slow, units atomic.Int64
}

func (c *counters) record(o Outcome) {
	c.calls.Add(1)
	c.units.Add(o.Units)
	c.slow.Add(int64(o.SlowCandidates))
	if o.Trap {
		c.traps.Add(1)
	}
	switch o.Status {
	case StatusError:
		c.errors.Add(1)
	case StatusTimeout:
		c.timeouts.Add(1)
	case StatusForbidden:
		c.forbidden.Add(1)
	case StatusAborted:
		c.aborted.Add(1)
	case StatusGovernance:
		c.governance.Add(1)
	}
}

func (c *counters) snapshot() Stats {
	return Stats{Calls: c.calls.Load(), Errors: c.errors.Load(), Timeouts: c.timeouts.Load(),
		Traps: c.traps.Load(), Forbidden: c.forbidden.Load(), Aborted: c.aborted.Load(),
		Governance: c.governance.Load(), SlowCandidates: c.slow.Load(), Units: c.units.Load()}
}

// callState is the per-call governance state shared with host functions.
// mu guards fields that can be touched by an in-flight host call goroutine
// (for example streaming unit charges) if dispatch aborts on deadline expiry.
type callState struct {
	mu             sync.Mutex
	ctx            context.Context
	host           Host
	lim            *Limits
	path           string
	start          time.Time
	last           time.Time
	units          int64
	logs           int
	background     bool
	backgroundPool bool
	hot            bool
	exceeded       bool
	aborted        bool
	abortCode      uint32
	logger         *slog.Logger
	pluginID       string
}

func formatBudgetDuration(d time.Duration) string {
	if d > 0 && d%time.Hour == 0 {
		return fmt.Sprintf("%d h", d/time.Hour)
	}
	if d > 0 && d%time.Minute == 0 {
		return fmt.Sprintf("%d min", d/time.Minute)
	}
	if d > 0 && d%time.Second == 0 {
		return fmt.Sprintf("%d s", d/time.Second)
	}
	if d > 0 && d%time.Millisecond == 0 {
		return fmt.Sprintf("%d ms", d/time.Millisecond)
	}
	return d.String()
}

func (cs *callState) governanceMessage() string {
	if cs == nil || cs.lim == nil {
		return "governance budget exceeded"
	}
	if cs.path != "" && ViewerRequestPath(cs.path) && cs.lim.UnitBudget == UnitsForDeadline(cs.lim.Deadline) {
		return fmt.Sprintf("used the whole %s budget for %s (%d/%d units)",
			formatBudgetDuration(cs.lim.Deadline), cs.path, cs.units, cs.lim.UnitBudget)
	}
	if cs.path != "" {
		return fmt.Sprintf("used the whole %d-unit budget for %s (%d/%d units)",
			cs.lim.UnitBudget, cs.path, cs.units, cs.lim.UnitBudget)
	}
	return fmt.Sprintf("governance budget exceeded (%d/%d units)", cs.units, cs.lim.UnitBudget)
}

func (cs *callState) timeoutMessage() string {
	if cs != nil && cs.lim != nil && cs.lim.Deadline > 0 && time.Since(cs.start)+50*time.Millisecond >= cs.lim.Deadline {
		if cs.path != "" {
			return fmt.Sprintf("call exceeded its %s deadline for %s", formatBudgetDuration(cs.lim.Deadline), cs.path)
		}
		return fmt.Sprintf("call exceeded its %s deadline", formatBudgetDuration(cs.lim.Deadline))
	}
	return "call exceeded its deadline"
}

func (cs *callState) chargeWallLocked() {
	now := time.Now()
	cs.units += int64(now.Sub(cs.last) / WallUnitInterval)
	cs.last = now
}

// charge accounts one host call. It returns a governance Fault when the
// budget is spent; the flag is sticky so a plugin that catches the error
// still ends the call as governance.
func (cs *callState) charge(name string) *Fault {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.chargeWallLocked()
	return cs.chargeUnitsLocked(cs.lim.costOf(name))
}

func (cs *callState) chargeUnits(units int64) *Fault {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.chargeUnitsLocked(units)
}

func (cs *callState) chargeUnitsLocked(units int64) *Fault {
	cs.units += units
	if cs.units > cs.lim.UnitBudget {
		cs.exceeded = true
		return &Fault{Code: string(StatusGovernance), Message: cs.governanceMessage()}
	}
	return nil
}

func (cs *callState) finishUnits() int64 {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.chargeWallLocked()
	if cs.units > cs.lim.UnitBudget {
		cs.exceeded = true
	}
	return cs.units
}

// dispatch runs one host service for a guest, applying governance and the
// hot-path lockdown. It is the single funnel for JS ether.* and raw
// ether.host_call.
func (cs *callState) dispatch(name string, args json.RawMessage) (json.RawMessage, *Fault) {
	if cs.ctx.Err() != nil {
		if errors.Is(cs.ctx.Err(), context.Canceled) {
			return nil, &Fault{Code: string(StatusError), Message: context.Canceled.Error()}
		}
		return nil, &Fault{Code: string(StatusTimeout), Message: cs.timeoutMessage()}
	}
	if cs.hot && name != "log" {
		return nil, Forbidden("host services are not available on the hot path: " + name)
	}
	if name == "log" {
		cs.logs++
		if cs.logs > maxLogsPerCall {
			return json.RawMessage("null"), nil // rate-limited: drop quietly
		}
		if f := cs.charge(name); f != nil {
			return nil, f
		}
		if cs.host != nil {
			_, _ = cs.host.CallHost(cs.ctx, name, args)
		} else {
			cs.logger.Debug("plugin log", "plugin", cs.pluginID, "args", string(args))
		}
		return json.RawMessage("null"), nil
	}
	if f := cs.charge(name); f != nil {
		return nil, f
	}
	if cs.host == nil {
		return nil, Forbidden("no host services are configured for this plugin")
	}

	hostTimeout := cs.hostCallTimeout(name, args)
	hostCtx, hostCancel := context.WithTimeout(cs.ctx, hostTimeout)
	defer hostCancel()

	var abandoned atomic.Bool
	var streamCharged atomic.Bool
	hostCtx = context.WithValue(hostCtx, unitChargerKey{}, func(units int64) *Fault {
		if abandoned.Load() {
			return &Fault{Code: string(StatusTimeout), Message: fmt.Sprintf("host call %s exceeded its deadline", name)}
		}
		streamCharged.Store(true)
		cs.mu.Lock()
		cs.last = time.Now()
		f := cs.chargeUnitsLocked(units)
		cs.mu.Unlock()
		return f
	})
	if cs.lim != nil {
		hostCtx = context.WithValue(hostCtx, callLimitsKey{}, *cs.lim)
	}
	if cs.backgroundPool {
		hostCtx = context.WithValue(hostCtx, backgroundCallKey{}, true)
	}
	if cs.path != "" {
		hostCtx = context.WithValue(hostCtx, callPathKey{}, cs.path)
	}

	type hostCallResult struct {
		out json.RawMessage
		err error
	}
	done := make(chan hostCallResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- hostCallResult{err: &Fault{Code: "host_error", Message: fmt.Sprintf("host call %s panicked: %v", name, r)}}
			}
		}()
		out, err := cs.host.CallHost(hostCtx, name, args)
		done <- hostCallResult{out: out, err: err}
	}()

	var out json.RawMessage
	var err error
	select {
	case res := <-done:
		out, err = res.out, res.err
	case <-hostCtx.Done():
		abandoned.Store(true)
		cs.mu.Lock()
		cs.last = time.Now()
		cs.mu.Unlock()
		if cs.ctx.Err() != nil {
			if errors.Is(cs.ctx.Err(), context.Canceled) {
				return nil, &Fault{Code: string(StatusError), Message: context.Canceled.Error()}
			}
			return nil, &Fault{Code: string(StatusTimeout), Message: cs.timeoutMessage()}
		}
		return nil, &Fault{Code: string(StatusTimeout), Message: fmt.Sprintf("host call %s exceeded its %s deadline", name, formatBudgetDuration(hostTimeout))}
	}
	// Time spent waiting on the host (a network fetch, a catalogue query) is
	// paid by the call's own unit price, not as guest wall time; otherwise
	// one slow upstream response spent the whole budget. The call deadline
	// still bounds the total.
	cs.mu.Lock()
	cs.last = time.Now()
	cs.mu.Unlock()
	if err != nil {
		if f, ok := err.(*Fault); ok {
			return nil, f
		}
		if cs.ctx.Err() != nil {
			if errors.Is(cs.ctx.Err(), context.Canceled) {
				return nil, &Fault{Code: string(StatusError), Message: context.Canceled.Error()}
			}
			return nil, &Fault{Code: string(StatusTimeout), Message: cs.timeoutMessage()}
		}
		if hostCtx.Err() != nil {
			return nil, &Fault{Code: string(StatusTimeout), Message: fmt.Sprintf("host call %s exceeded its %s deadline", name, formatBudgetDuration(hostTimeout))}
		}
		return nil, &Fault{Code: "host_error", Message: err.Error()}
	}
	if name == "archive.extract" && !streamCharged.Load() {
		var extracted struct {
			Size int64 `json:"size"`
		}
		if json.Unmarshal(out, &extracted) == nil && extracted.Size > 0 {
			mib := (extracted.Size + (1 << 20) - 1) >> 20
			if f := cs.chargeUnits(mib * ArchiveExtractUnitsPerMiB); f != nil {
				return nil, f
			}
		}
	}
	limit := HostResultBytes(cs.lim.MemoryBytes)
	// JSON can escape each text byte as six bytes. HTTPFetch and HTTPFetchMany
	// bound raw response bytes to 10 MiB; leave room for headers and JSON framing.
	if name == "http.fetch" || name == "http.fetchMany" {
		limit = max(limit, int64((6*MaxFetchBodyBytes)+(1<<20)))
	}
	if int64(len(out)) > limit {
		return nil, &Fault{Code: "host_error", Message: "host result exceeds size cap"}
	}
	if len(out) == 0 {
		out = json.RawMessage("null")
	}
	return out, nil
}

// caps records which ABI entry points a plugin defines.
type caps struct {
	Handle    bool
	RankKey   bool
	CondMatch bool
}

// instance is one live guest (a QuickJS runtime or a raw module instance).
// Instances are not safe for concurrent use; the pool lends one at a time.
// Any call that ends with Trap set, or with a timeout, poisons the instance.
type instance interface {
	handle(ctx context.Context, cs *callState, reqJSON []byte) ([]byte, *Error)
	rankOne(ctx context.Context, cs *callState, cand, cfg []byte) (int64, *Error)
	condOne(ctx context.Context, cs *callState, cand []byte, op string, val []byte) (bool, *Error)
	caps() caps
	closeInstance()
}

// batchRanker is implemented by instances (JavaScript) that can score a
// whole batch in one crossing. They apply the per-candidate and batch
// budgets themselves and enforce the hot kill deadline (Limits.hotKill)
// on ctx. Nil scores mark slow candidates.
type batchRanker interface {
	rankBatch(ctx context.Context, cs *callState, cands [][]byte, cfg []byte, lim *Limits) ([]*int64, *Error)
}

// batchMatcher is the rule-condition counterpart of batchRanker.
type batchMatcher interface {
	condBatch(ctx context.Context, cs *callState, cands [][]byte, op string, val []byte, lim *Limits) ([]*bool, *Error)
}

func statusForFaultCode(code string) Status {
	switch code {
	case string(StatusInvalidInput):
		return StatusInvalidInput
	case string(StatusForbidden):
		return StatusForbidden
	case string(StatusAborted):
		return StatusAborted
	case string(StatusGovernance):
		return StatusGovernance
	case string(StatusTimeout):
		return StatusTimeout
	default:
		return StatusError
	}
}

// pool lends instances up to size, discarding poisoned ones so crash
// policy (backoff, disable) can count fresh failures.
//
// A service pool builds on demand under the caller's context: a handle
// call has a seconds-long deadline, and a cold build fits in it.
//
// A warm pool (the hot path) never builds on the caller's path. Building
// an instance costs milliseconds (a QuickJS runtime is several), more than
// the 2ms per-candidate and 50ms per-batch budgets, so building inline
// would make the first concurrent call, and every call after a discarded
// instance, time out; on a slow host the hot path then never recovers.
// Instead the pool is filled at load, a discarded instance is replaced in
// the background under the service deadline, and a caller only ever waits
// for an idle instance within its own budget.
type poolLeaseInfo struct {
	route string
	since time.Time
}

type poolWaiter struct {
	ch       chan instance
	priority bool
}

type pool struct {
	makeFn      func(context.Context) (instance, error)
	size        int
	warm        bool
	mu          sync.Mutex // guards done, idle, live, inUse, building, waiting, waiters and holders
	done        bool
	idle        []instance
	live        int // idle + lent + building + in-flight waiter handoffs (<= size)
	inUse       int
	building    int
	waiting     int
	prioWaiters []*poolWaiter
	normWaiters []*poolWaiter
	exhaustions atomic.Int64
	nextLease   uint64
	holders     map[uint64]poolLeaseInfo
}

func newPool(size int, makeFn func(context.Context) (instance, error)) *pool {
	return &pool{
		makeFn:  makeFn,
		size:    size,
		idle:    make([]instance, 0, size),
		holders: make(map[uint64]poolLeaseInfo, size),
	}
}

func newWarmPool(size int, makeFn func(context.Context) (instance, error)) *pool {
	p := newPool(size, makeFn)
	p.warm = true
	return p
}

func (p *pool) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

func (p *pool) acquireLeaseLocked(route string, track bool) uint64 {
	p.inUse++
	if !track {
		return 0
	}
	p.nextLease++
	id := p.nextLease
	p.holders[id] = poolLeaseInfo{route: route, since: time.Now()}
	return id
}

func (p *pool) releaseLeaseLocked(leaseID uint64) {
	if leaseID != 0 {
		if _, ok := p.holders[leaseID]; ok {
			delete(p.holders, leaseID)
			if p.inUse > 0 {
				p.inUse--
			}
		}
		return
	}
	if p.inUse > 0 {
		p.inUse--
	}
}

func (p *pool) recordExhaustion() PoolStats {
	p.exhaustions.Add(1)
	return p.snapshot()
}

func (p *pool) snapshot() PoolStats {
	if p == nil {
		return PoolStats{}
	}
	now := time.Now()
	p.mu.Lock()
	holders := make([]PoolHolder, 0, len(p.holders))
	for _, info := range p.holders {
		held := now.Sub(info.since)
		if held < 0 {
			held = 0
		}
		route := info.route
		if route == "" {
			route = "(unknown)"
		}
		holders = append(holders, PoolHolder{Route: route, Held: held})
	}
	stats := PoolStats{
		Size:        p.size,
		InUse:       p.inUse,
		Idle:        len(p.idle),
		Building:    p.building,
		Waiting:     p.waiting,
		Exhaustions: p.exhaustions.Load(),
		Holders:     holders,
	}
	p.mu.Unlock()
	sort.Slice(stats.Holders, func(i, j int) bool {
		if stats.Holders[i].Held != stats.Holders[j].Held {
			return stats.Holders[i].Held > stats.Holders[j].Held
		}
		return stats.Holders[i].Route < stats.Holders[j].Route
	})
	return stats
}

func (p *pool) runMakeFn(ctx context.Context) (in instance, err error) {
	defer func() {
		if r := recover(); r != nil {
			in = nil
			err = &Error{Status: StatusError, Message: fmt.Sprintf("plugin instance build panic: %v", r), Trap: true}
		}
	}()
	return p.makeFn(ctx)
}

func (p *pool) get(ctx context.Context) (instance, error) {
	in, _, err := p.getInternal(ctx, "", false, false)
	return in, err
}

func (p *pool) getForRoute(ctx context.Context, route string) (instance, uint64, error) {
	return p.getInternal(ctx, route, true, false)
}

func (p *pool) getForRoutePriority(ctx context.Context, route string, priority bool) (instance, uint64, error) {
	return p.getInternal(ctx, route, true, priority)
}

func (p *pool) removeWaiterLocked(target *poolWaiter) bool {
	list := &p.normWaiters
	if target.priority {
		list = &p.prioWaiters
	}
	for i, w := range *list {
		if w == target {
			copy((*list)[i:], (*list)[i+1:])
			(*list)[len(*list)-1] = nil
			*list = (*list)[:len(*list)-1]
			return true
		}
	}
	return false
}

func (p *pool) popWaiterLocked() *poolWaiter {
	if len(p.prioWaiters) > 0 {
		w := p.prioWaiters[0]
		copy(p.prioWaiters, p.prioWaiters[1:])
		p.prioWaiters[len(p.prioWaiters)-1] = nil
		p.prioWaiters = p.prioWaiters[:len(p.prioWaiters)-1]
		return w
	}
	if len(p.normWaiters) > 0 {
		w := p.normWaiters[0]
		copy(p.normWaiters, p.normWaiters[1:])
		p.normWaiters[len(p.normWaiters)-1] = nil
		p.normWaiters = p.normWaiters[:len(p.normWaiters)-1]
		return w
	}
	return nil
}

func (p *pool) hasWaitersLocked() bool {
	return len(p.prioWaiters) > 0 || len(p.normWaiters) > 0
}

func (p *pool) getInternal(ctx context.Context, route string, track, priority bool) (instance, uint64, error) {
	p.mu.Lock()
	if p.done {
		p.mu.Unlock()
		return nil, 0, &Error{Status: StatusError, Message: ErrClosed.Error(), Err: ErrClosed}
	}
	if len(p.idle) > 0 && (priority || len(p.prioWaiters) == 0) {
		n := len(p.idle) - 1
		in := p.idle[n]
		p.idle[n] = nil
		p.idle = p.idle[:n]
		leaseID := p.acquireLeaseLocked(route, track)
		p.mu.Unlock()
		return in, leaseID, nil
	}
	if !p.warm && p.live < p.size {
		p.live++
		p.building++
		p.mu.Unlock()
		in, err := p.runMakeFn(ctx)
		if err != nil {
			p.mu.Lock()
			p.building--
			p.live--
			// If this caller gave up while other waiters are queued, keep a
			// replacement build going so waiting callers do not stall.
			if ctx.Err() != nil && p.hasWaitersLocked() {
				p.refillLocked()
			}
			p.mu.Unlock()
			return nil, 0, err
		}
		p.mu.Lock()
		p.building--
		if p.done {
			p.live--
			p.mu.Unlock()
			in.closeInstance()
			return nil, 0, &Error{Status: StatusError, Message: ErrClosed.Error(), Err: ErrClosed}
		}
		leaseID := p.acquireLeaseLocked(route, track)
		p.mu.Unlock()
		return in, leaseID, nil
	}
	w := &poolWaiter{ch: make(chan instance, 1), priority: priority}
	if priority {
		p.prioWaiters = append(p.prioWaiters, w)
	} else {
		p.normWaiters = append(p.normWaiters, w)
	}
	p.waiting++
	// A free slot means an earlier build failed or retired; retry it off-path.
	if p.live < p.size {
		p.refillLocked()
	}
	p.mu.Unlock()

	select {
	case in, ok := <-w.ch:
		p.mu.Lock()
		p.waiting--
		if !ok || in == nil {
			p.mu.Unlock()
			return nil, 0, &Error{Status: StatusError, Message: ErrClosed.Error(), Err: ErrClosed}
		}
		if p.done {
			p.live--
			p.mu.Unlock()
			in.closeInstance()
			return nil, 0, &Error{Status: StatusError, Message: ErrClosed.Error(), Err: ErrClosed}
		}
		leaseID := p.acquireLeaseLocked(route, track)
		p.mu.Unlock()
		return in, leaseID, nil
	case <-ctx.Done():
		p.mu.Lock()
		p.waiting--
		removed := p.removeWaiterLocked(w)
		p.mu.Unlock()
		if !removed {
			// A producer already popped w and delivered (or closed) w.ch.
			if in, ok := <-w.ch; ok && in != nil {
				p.adopt(in)
			}
			if p.isClosed() {
				return nil, 0, &Error{Status: StatusError, Message: ErrClosed.Error(), Err: ErrClosed}
			}
		}
		return nil, 0, &Error{Status: StatusTimeout, Message: ErrPoolBusy.Error(), Err: ErrPoolBusy}
	}
}

func (p *pool) put(in instance, healthy bool) {
	p.putLease(in, 0, healthy)
}

func (p *pool) putLease(in instance, leaseID uint64, healthy bool) {
	p.mu.Lock()
	p.releaseLeaseLocked(leaseID)
	if !healthy {
		p.live--
		needRefill := p.warm || p.hasWaitersLocked()
		if needRefill {
			p.refillLocked()
		}
		p.mu.Unlock()
		in.closeInstance()
		return
	}
	closeInst := p.adoptLocked(in)
	p.mu.Unlock()
	if closeInst {
		in.closeInstance()
	}
}

// adopt returns a usable instance to the highest-priority waiter or idle set,
// or closes it when the pool is closed or already full.
func (p *pool) adopt(in instance) {
	p.mu.Lock()
	closeInst := p.adoptLocked(in)
	p.mu.Unlock()
	if closeInst {
		in.closeInstance()
	}
}

func (p *pool) adoptLocked(in instance) (closeInst bool) {
	if !p.done {
		if w := p.popWaiterLocked(); w != nil {
			w.ch <- in
			return false
		}
		if len(p.idle) < p.size {
			p.idle = append(p.idle, in)
			return false
		}
	}
	p.live--
	return true
}

// fill builds instances synchronously until the pool is full or a build
// fails. Plugins call it at load, off any request path.
func (p *pool) fill() {
	for {
		p.mu.Lock()
		if p.done || p.live >= p.size {
			p.mu.Unlock()
			return
		}
		p.live++
		p.building++
		p.mu.Unlock()
		in, err := p.runMakeFn(context.Background())
		if err != nil {
			p.mu.Lock()
			p.building--
			p.live--
			p.mu.Unlock()
			return
		}
		p.mu.Lock()
		p.building--
		closeInst := p.adoptLocked(in)
		p.mu.Unlock()
		if closeInst {
			in.closeInstance()
		}
	}
}

// refillLocked builds one replacement in the background if a slot is free.
// Caller must hold p.mu.
func (p *pool) refillLocked() {
	if p.done || p.live >= p.size {
		return
	}
	p.live++
	p.building++
	go func() {
		in, err := p.runMakeFn(context.Background())
		if err != nil {
			p.mu.Lock()
			p.building--
			p.live--
			p.mu.Unlock()
			return
		}
		p.mu.Lock()
		p.building--
		closeInst := p.adoptLocked(in)
		p.mu.Unlock()
		if closeInst {
			in.closeInstance()
		}
	}()
}

func (p *pool) close() {
	p.mu.Lock()
	if p.done {
		p.mu.Unlock()
		return
	}
	p.done = true
	toClose := p.idle
	p.idle = nil
	p.live -= len(toClose)
	waiters := append(p.prioWaiters, p.normWaiters...)
	p.prioWaiters = nil
	p.normWaiters = nil
	for _, w := range waiters {
		close(w.ch)
	}
	p.mu.Unlock()
	for _, in := range toClose {
		in.closeInstance()
	}
}

// Engine compiles and caches guest code and builds Plugins. It owns the
// wazero runtimes used by raw modules; JavaScript instances manage their
// own (QuickJS) modules. Safe for concurrent use.
type Engine struct {
	logger *slog.Logger
	mu     sync.Mutex
	raw    map[uint32]*rawRuntime // by memory-limit pages
	closed bool
	// speedLog logs the engine's measured speed once (see HotScale).
	speedLog sync.Once
}

func NewEngine(logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	ensureQJS()
	return &Engine{logger: logger, raw: map[uint32]*rawRuntime{}}
}

func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	for _, rt := range e.raw {
		_ = rt.rt.Close(context.Background())
	}
	e.raw = map[uint32]*rawRuntime{}
	return nil
}

// Plugin is one loaded hosted plugin. Call serves the request ABI;
// RankBatch and CondMatch serve the hot path. Safe for concurrent use.
type Plugin struct {
	id, version     string
	eng             *Engine
	host            Host
	lim             Limits
	caps            caps
	svc             *pool
	hot             *pool
	jobSlots        chan struct{} // PoolSize>=3: sized PoolSize-2 so background jobs leave interactive + resolve slots
	nonResolveSlots chan struct{} // PoolSize>=3: sized PoolSize-1 so playback resolves always keep >= 1 reserved instance
	sharedWorkSlots chan struct{} // PoolSize==2: sized 1 for background jobs + frontend adapters so resolves/health keep 1 instance
	closeCh         chan struct{}
	stats           counters
	memo            *memo
	source          string // JS source (rebuild counts / cold start), raw: ""
	closed          atomic.Bool
}

func (p *Plugin) initPools(makeSvc, makeHot func(context.Context) (instance, error)) {
	p.closeCh = make(chan struct{})
	switch {
	case p.lim.PoolSize >= 3:
		p.nonResolveSlots = make(chan struct{}, p.lim.PoolSize-1)
		p.jobSlots = make(chan struct{}, p.lim.PoolSize-2)
	case p.lim.PoolSize == 2:
		p.sharedWorkSlots = make(chan struct{}, 1)
	}
	p.svc = newPool(p.lim.PoolSize, makeSvc)
	p.hot = newWarmPool(p.lim.PoolSize, makeHot)
}

// ID and Version identify the plugin for memo keys and logs.
func (p *Plugin) ID() string      { return p.id }
func (p *Plugin) Version() string { return p.version }

// HasHandle, HasRankKey and HasCondMatch report the exported entry points.
func (p *Plugin) HasHandle() bool { return p.caps.Handle }

// Budgets reports the plugin's effective hot-path budgets: the manifest's
// (lowered, never raised), scaled by the engine's measured speed.
func (p *Plugin) Budgets() (perCandidate, perBatch time.Duration) {
	return p.lim.RankPerCandidate, p.lim.RankBatch
}

func (p *Plugin) HasRankKey() bool   { return p.caps.RankKey }
func (p *Plugin) HasCondMatch() bool { return p.caps.CondMatch }

func (p *Plugin) PoolStats() PoolStats {
	if p == nil || p.svc == nil {
		return PoolStats{}
	}
	return p.svc.snapshot()
}

func (p *Plugin) Stats() Stats {
	s := p.stats.snapshot()
	s.Pool = p.PoolStats()
	return s
}

func formatPoolHolders(holders []PoolHolder) []string {
	out := make([]string, len(holders))
	for i, h := range holders {
		out[i] = fmt.Sprintf("%s (%s)", h.Route, h.Held.Round(time.Millisecond))
	}
	return out
}

func (p *Plugin) recordPoolExhaustion(route string) {
	if p == nil || p.svc == nil {
		return
	}
	snap := p.svc.recordExhaustion()
	if p.eng != nil && p.eng.logger != nil {
		p.eng.logger.Warn("wasm: plugin instance pool exhausted",
			"plugin", p.id,
			"route", route,
			"pool_size", snap.Size,
			"in_use", snap.InUse,
			"idle", snap.Idle,
			"building", snap.Building,
			"waiting", snap.Waiting,
			"exhaustions", snap.Exhaustions,
			"holders", formatPoolHolders(snap.Holders),
		)
	}
}

func (p *Plugin) Close() error {
	if p.closed.Swap(true) {
		return nil
	}
	if p.closeCh != nil {
		close(p.closeCh)
	}
	if p.svc != nil {
		p.svc.close()
	}
	if p.hot != nil {
		p.hot.close()
	}
	return nil
}

// Call runs the request ABI. The returned error, when non-nil, is a *Error
// classified for the host's health loop.
func (p *Plugin) Call(ctx context.Context, req Request) (Response, error) {
	resp, _, err := p.CallDetailed(ctx, req)
	return resp, err
}

func (p *Plugin) acquireLane(ctx context.Context, lane chan struct{}, route string) *Error {
	if lane == nil {
		return nil
	}
	select {
	case lane <- struct{}{}:
		return nil
	case <-p.closeCh:
		return &Error{Status: StatusError, Message: ErrClosed.Error(), Err: ErrClosed}
	case <-ctx.Done():
		p.recordPoolExhaustion(route)
		return &Error{Status: StatusTimeout, Message: ErrPoolBusy.Error(), Err: ErrPoolBusy}
	}
}

// CallDetailed is Call plus the Outcome (units, duration, trap flag).
func (p *Plugin) CallDetailed(ctx context.Context, req Request) (resp Response, out Outcome, callRetErr error) {
	start := time.Now()
	out = Outcome{Status: StatusOK}
	defer func() {
		out.Duration = time.Since(start)
		p.stats.record(out)
	}()
	if p.closed.Load() {
		out.Status = StatusError
		return Response{}, out, &Error{Status: StatusError, Message: ErrClosed.Error(), Err: ErrClosed}
	}
	if !p.caps.Handle {
		out.Status = StatusError
		return Response{}, out, &Error{Status: StatusError, Message: "plugin does not export handle"}
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		out.Status = StatusError
		return Response{}, out, &Error{Status: StatusError, Message: err.Error(), Err: ErrRequestEncode}
	}
	lim := limitsForRequest(p.lim, req.Path)
	if req.Deadline > 0 {
		if req.Deadline < lim.Deadline || (req.AISearch && req.Deadline <= p.lim.Deadline) {
			lim.Deadline = req.Deadline
			if ViewerRequestPath(req.Path) {
				lim.UnitBudget = min(p.lim.UnitBudget, UnitsForDeadline(lim.Deadline))
			}
		}
	}

	isResolve := ResolvePoolPath(req.Path)
	background := strings.HasPrefix(req.Path, "/ether/job/") && !req.Foreground

	var (
		callCtx context.Context
		cancel  context.CancelFunc
		in      instance
		leaseID uint64
		cs      *callState
	)
	if isResolve {
		// Playback resolves bound queue waiting separately from execution so a
		// brief pool wait never eats into the resolve's execution deadline.
		waitCtx, waitCancel := context.WithTimeout(ctx, lim.Deadline)
		in, leaseID, err = p.svc.getForRoutePriority(waitCtx, req.Path, true)
		waitCancel()
		if err != nil {
			if errors.Is(err, ErrPoolBusy) {
				p.recordPoolExhaustion(req.Path)
			}
			out.Status = StatusTimeout
			if e, ok := err.(*Error); ok {
				out.Status = e.Status
			}
			return Response{}, out, err
		}
		execStart := time.Now()
		callCtx, cancel = context.WithTimeout(ctx, lim.Deadline)
		defer cancel()
		if waited := execStart.Sub(start); waited >= 50*time.Millisecond && req.Headers != nil {
			if rawDL := req.Headers["X-Ether-Deadline"]; rawDL != "" {
				if parsed, perr := time.Parse(time.RFC3339, rawDL); perr == nil {
					hdrs := make(map[string]string, len(req.Headers))
					for k, v := range req.Headers {
						hdrs[k] = v
					}
					hdrs["X-Ether-Deadline"] = parsed.Add(waited).UTC().Format(time.RFC3339)
					reqCopy := req
					reqCopy.Headers = hdrs
					if updated, merr := json.Marshal(reqCopy); merr == nil {
						reqJSON = updated
					}
				}
			}
		}
		cs = &callState{
			ctx:      callCtx,
			host:     p.host,
			lim:      &lim,
			path:     req.Path,
			start:    execStart,
			last:     execStart,
			units:    BaseCallUnits,
			logger:   p.eng.logger,
			pluginID: p.id,
		}
	} else {
		callCtx, cancel = context.WithTimeout(ctx, lim.Deadline)
		defer cancel()
		if background {
			// Wait while Ether is busy BEFORE taking a lane slot or plugin instance:
			// a background job must never hold a pool instance while paused on Busy().
			if err := WaitBackground(callCtx, lim.Busy); err != nil {
				out.Status = StatusTimeout
				return Response{}, out, &Error{Status: StatusTimeout, Message: err.Error(), Err: err}
			}
		}
		// Strict semaphore order (jobSlots -> nonResolveSlots, or sharedWorkSlots -> svc)
		// with LIFO defer release guarantees putLease returns the instance before
		// freeing lane slots and prevents deadlocks.
		if BackgroundPoolPath(req.Path) && p.jobSlots != nil {
			if laneErr := p.acquireLane(callCtx, p.jobSlots, req.Path); laneErr != nil {
				out.Status = laneErr.Status
				return Response{}, out, laneErr
			}
			defer func() { <-p.jobSlots }()
		}
		if p.nonResolveSlots != nil {
			if laneErr := p.acquireLane(callCtx, p.nonResolveSlots, req.Path); laneErr != nil {
				out.Status = laneErr.Status
				return Response{}, out, laneErr
			}
			defer func() { <-p.nonResolveSlots }()
		}
		if p.sharedWorkSlots != nil && (BackgroundPoolPath(req.Path) || FrontendAdapterPath(req.Path)) {
			if laneErr := p.acquireLane(callCtx, p.sharedWorkSlots, req.Path); laneErr != nil {
				out.Status = laneErr.Status
				return Response{}, out, laneErr
			}
			defer func() { <-p.sharedWorkSlots }()
		}
		cs = &callState{
			ctx:            callCtx,
			host:           p.host,
			lim:            &lim,
			path:           req.Path,
			start:          start,
			last:           start,
			background:     background,
			backgroundPool: BackgroundPoolPath(req.Path) && !req.Foreground,
			units:          BaseCallUnits,
			logger:         p.eng.logger,
			pluginID:       p.id,
		}
		in, leaseID, err = p.svc.getForRoutePriority(callCtx, req.Path, false)
		if err != nil {
			if errors.Is(err, ErrPoolBusy) {
				p.recordPoolExhaustion(req.Path)
			}
			out.Status = StatusTimeout
			if e, ok := err.(*Error); ok {
				out.Status = e.Status
			}
			return Response{}, out, err
		}
	}
	healthy := false
	defer func() {
		if r := recover(); r != nil {
			healthy = false
			resp = Response{}
			out.Status = StatusError
			out.Trap = true
			callRetErr = &Error{Status: StatusError, Message: fmt.Sprintf("plugin call panic: %v", r), Trap: true}
		}
		p.svc.putLease(in, leaseID, healthy)
	}()
	// Waiting for a free instance is the host's queue, not the plugin's
	// work: concurrent calls (a metadata sweep) must not spend the budget
	// of the call at the back of the line.
	cs.mu.Lock()
	cs.last = time.Now()
	cs.mu.Unlock()
	respJSON, callErr := in.handle(callCtx, cs, reqJSON)
	out.Units = cs.finishUnits()
	healthy = callErr == nil || !callErr.Trap
	if (callErr != nil && callErr.Status == StatusTimeout) || callCtx.Err() != nil {
		healthy = false
	}
	if callErr != nil {
		out.Status, out.Trap = callErr.Status, callErr.Trap
		return Response{}, out, callErr
	}
	cs.mu.Lock()
	exceeded := cs.exceeded
	cs.mu.Unlock()
	if exceeded {
		out.Status = StatusGovernance
		return Response{}, out, &Error{Status: StatusGovernance, Message: cs.governanceMessage()}
	}
	if len(respJSON) > MaxResponseBytes {
		out.Status = StatusError
		return Response{}, out, &Error{Status: StatusError, Message: "plugin response exceeds size cap"}
	}
	if err := json.Unmarshal(respJSON, &resp); err != nil {
		out.Status = StatusError
		return Response{}, out, &Error{Status: StatusError, Message: "plugin returned invalid response JSON: " + err.Error()}
	}
	if resp.Status == 0 {
		resp.Status = 200
	}
	return resp, out, nil
}
