package flows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/events"
)

const (
	defaultTickInterval     = time.Second
	defaultLeaseDuration    = 60 * time.Second
	defaultMaxWorkers       = 16
	defaultRetentionAge     = 30 * 24 * time.Hour
	defaultRetentionMax     = 10000
	defaultPruneInterval    = time.Hour
	defaultBackoffThreshold = 5
	defaultBackoffBase      = 5 * time.Second
	defaultBackoffMax       = 15 * time.Minute
	defaultBusBuffer        = 512
	defaultClaimBatch       = 32
)

// EngineOptions configures a durable Flows Runtime.
type EngineOptions struct {
	Store      Store
	DB         *sql.DB
	Bus        *events.Bus
	Events     *events.Registry
	Actions    ActionRegistry
	Triggers   TriggerRegistry
	Cluster    cluster.Cluster
	ClusterBus clusterbus.Bus
	NodeID     string
	Logger     *slog.Logger
	Now        func() time.Time

	TickInterval  time.Duration
	LeaseDuration time.Duration
	MaxWorkers    int
	BusBuffer     int

	RetentionAge  time.Duration
	RetentionMax  int
	PruneInterval time.Duration

	BackoffThreshold int
	BackoffBase      time.Duration
	BackoffMax       time.Duration

	// EnableBridge forces the follower->leader OutboxBridge to run even when
	// Cluster is nil or Solo (useful in tests with simulated clusterbus.Hub).
	EnableBridge bool

	OnFlowsChanged  func(flowID string)
	OnRunUpdated    func(run Run)
	OnAutoDisable   func(flow Flow, run Run)
	OnLeaderStarted func()
}

// Engine is Ether's durable Flows Runtime implementation.
type Engine struct {
	opts   EngineOptions
	store  Store
	db     *sql.DB
	outbox *OutboxStore
	bridge *OutboxBridge
	cbus   clusterbus.Bus
	nodeID string
	origin string
	logger *slog.Logger
	now    func() time.Time

	leading     atomic.Bool
	subsVersion atomic.Uint64

	mu           sync.RWMutex
	enabledFlows []Flow
	flowsByID    map[string]Flow

	rateMu           sync.Mutex
	rateWindows      map[string][]time.Time // flowID -> trigger timestamps within last 1m
	throttleSampleAt map[string]time.Time   // flowID -> last sampled throttled run record

	cancelMu      sync.Mutex
	activeCancels map[string]context.CancelFunc
	cancelledRuns map[string]bool
	cancelledOrd  []string

	workCh   chan struct{}
	outboxCh chan struct{}
	reloadCh chan struct{}
}

var _ Runtime = (*Engine)(nil)

// NewEngine constructs a Flows Engine. Call Start(ctx) to begin processing.
func NewEngine(opts EngineOptions) *Engine {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Triggers == nil {
		opts.Triggers = NewTriggerRegistry()
	}
	if opts.TickInterval <= 0 {
		opts.TickInterval = defaultTickInterval
	}
	if opts.LeaseDuration <= 0 {
		opts.LeaseDuration = defaultLeaseDuration
	}
	if opts.MaxWorkers <= 0 {
		opts.MaxWorkers = defaultMaxWorkers
	}
	if opts.BusBuffer <= 0 {
		opts.BusBuffer = defaultBusBuffer
	}
	if opts.RetentionAge <= 0 {
		opts.RetentionAge = defaultRetentionAge
	}
	if opts.RetentionMax <= 0 {
		opts.RetentionMax = defaultRetentionMax
	}
	if opts.PruneInterval <= 0 {
		opts.PruneInterval = defaultPruneInterval
	}
	if opts.BackoffThreshold <= 0 {
		opts.BackoffThreshold = defaultBackoffThreshold
	}
	if opts.BackoffBase <= 0 {
		opts.BackoffBase = defaultBackoffBase
	}
	if opts.BackoffMax <= 0 {
		opts.BackoffMax = defaultBackoffMax
	}

	db := opts.DB
	if db == nil {
		if sqlStore, ok := opts.Store.(*SQLStore); ok {
			db = sqlStore.DB()
		}
	}

	cbus := opts.ClusterBus
	if cbus == nil && opts.Cluster != nil {
		cbus = opts.Cluster
	}

	nodeID := strings.TrimSpace(opts.NodeID)
	if nodeID == "" && opts.Cluster != nil {
		nodeID = opts.Cluster.Self().ID
	}
	if nodeID == "" {
		nodeID = "node_" + randomHex(4)
	}

	e := &Engine{
		opts:             opts,
		store:            opts.Store,
		db:               db,
		cbus:             cbus,
		nodeID:           nodeID,
		origin:           clusterbus.NewOrigin(),
		logger:           opts.Logger,
		now:              opts.Now,
		flowsByID:        map[string]Flow{},
		rateWindows:      map[string][]time.Time{},
		throttleSampleAt: map[string]time.Time{},
		activeCancels:    map[string]context.CancelFunc{},
		cancelledRuns:    map[string]bool{},
		workCh:           make(chan struct{}, 1),
		outboxCh:         make(chan struct{}, 1),
		reloadCh:         make(chan struct{}, 1),
	}

	if db != nil {
		e.outbox = NewOutboxStore(db, opts.Now)
		if opts.Bus != nil {
			e.bridge = NewOutboxBridge(OutboxBridgeOptions{
				DB:         db,
				Bus:        opts.Bus,
				ClusterBus: cbus,
				NodeID:     nodeID,
				IsLeader:   e.IsLeading,
				LoadSubs:   e.SubscribedEvents,
				Now:        opts.Now,
				Logger:     opts.Logger,
				BufferSize: opts.BusBuffer,
			})
		}
	}

	if sqlStore, ok := opts.Store.(*SQLStore); ok {
		sqlStore.AddOnChange(func(flowID string) {
			if e.IsLeading() {
				e.kickReload()
			} else if e.cbus != nil {
				pctx, cancel := context.WithTimeout(context.Background(), outboxPublishTimeout)
				_ = e.cbus.Publish(pctx, TopicFlowsChanged, []byte(flowID))
				cancel()
			}
			if e.opts.OnFlowsChanged != nil {
				e.opts.OnFlowsChanged(flowID)
			}
		})
	}

	return e
}

// IsLeading reports whether this Engine instance is currently executing leader duties.
func (e *Engine) IsLeading() bool { return e.leading.Load() }

// Bridge returns the follower->leader OutboxBridge attached to this Engine, if any.
func (e *Engine) Bridge() *OutboxBridge { return e.bridge }

// Start runs the Flows runtime until ctx is cancelled. In clustered mode,
// leader work executes exclusively inside cluster.RunAsLeader("flows", ...)
// while the follower->leader OutboxBridge runs for the lifetime of ctx.
func (e *Engine) Start(ctx context.Context) error {
	if e.store == nil {
		return errors.New("flows: Engine requires a non-nil Store")
	}

	useBridge := e.bridge != nil && (e.opts.EnableBridge || (e.opts.Cluster != nil && e.opts.Cluster.Mode() != cluster.ModeSolo))
	var bridgeWG sync.WaitGroup
	if useBridge {
		bridgeWG.Add(1)
		go func() {
			defer bridgeWG.Done()
			e.bridge.Run(ctx)
		}()
	}

	if e.opts.Cluster != nil {
		e.opts.Cluster.RunAsLeader(ctx, "flows", func(leadCtx context.Context) {
			e.runLeader(leadCtx)
		})
	} else {
		e.runLeader(ctx)
	}

	bridgeWG.Wait()
	return nil
}

func (e *Engine) runLeader(ctx context.Context) {
	e.leading.Store(true)
	defer e.leading.Store(false)

	var unsubs []func()
	if e.cbus != nil {
		unsubs = append(unsubs,
			e.cbus.Subscribe(TopicOutboxWake, func([]byte) {
				e.kickOutbox()
				e.kickWork()
			}),
			e.cbus.Subscribe(TopicFlowsChanged, func([]byte) {
				e.kickReload()
			}),
			e.cbus.Subscribe(clusterbus.TopicResync, func([]byte) {
				e.kickReload()
				e.kickOutbox()
				e.kickWork()
			}),
		)
	}
	defer func() {
		for _, u := range unsubs {
			u()
		}
	}()

	var busSub *events.Subscription
	var busCh <-chan events.Envelope
	if e.opts.Bus != nil {
		busSub = e.opts.Bus.Subscribe(events.SubscriptionOptions{
			Name:          "flows-runtime",
			Buffer:        e.opts.BusBuffer,
			Patterns:      []string{"*"},
			IncludeDryRun: false,
		})
		busCh = busSub.C()
		defer busSub.Unsubscribe()
	}

	if err := e.ReloadFlows(ctx); err != nil && ctx.Err() == nil {
		e.logger.Warn("flows: initial flow reload failed", "err", err)
	}
	e.broadcastSubscriptions(ctx)
	if e.opts.OnLeaderStarted != nil {
		e.opts.OnLeaderStarted()
	}

	sem := make(chan struct{}, e.opts.MaxWorkers)
	var workerWG sync.WaitGroup
	defer workerWG.Wait()

	// Immediate startup sweep: drain any unacked outbox rows and recover any
	// runs/waiters/schedules left over from a previous leader.
	e.drainOutbox(ctx)
	e.stepLeaderWork(ctx, sem, &workerWG)

	ticker := time.NewTicker(e.opts.TickInterval)
	defer ticker.Stop()

	pruneTicker := time.NewTicker(e.opts.PruneInterval)
	defer pruneTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-busCh:
			if !ok {
				busCh = nil
				continue
			}
			e.handleLocalBusEvent(ctx, ev)

		case <-e.outboxCh:
			e.drainOutbox(ctx)

		case <-e.reloadCh:
			if err := e.ReloadFlows(ctx); err != nil && ctx.Err() == nil {
				e.logger.Warn("flows: reload failed", "err", err)
			}
			e.broadcastSubscriptions(ctx)
			e.kickWork()

		case <-e.workCh:
			e.stepLeaderWork(ctx, sem, &workerWG)

		case <-ticker.C:
			// Periodic fallback sweep handles delays, waiter timeouts, schedules,
			// lease expiry recovery, and any missed clusterbus wake pings.
			e.drainOutbox(ctx)
			e.stepLeaderWork(ctx, sem, &workerWG)

		case <-pruneTicker.C:
			e.prune(ctx)
		}
	}
}

func (e *Engine) handleLocalBusEvent(ctx context.Context, ev events.Envelope) {
	if ev.Meta.DryRun {
		return
	}
	if ev.Meta.Durable && e.outbox != nil {
		if _, err := e.outbox.Enqueue(ctx, ev); err != nil && ctx.Err() == nil {
			e.logger.Warn("flows: failed to persist durable local event to outbox", "event", ev.Type, "err", err)
			// Fall through to direct ingestion so the event is still processed.
		} else {
			e.drainOutbox(ctx)
			return
		}
	}
	runsCreated, waitersMatched, err := e.ingestEvent(ctx, ev, 0)
	if err != nil && ctx.Err() == nil {
		e.logger.Warn("flows: ingest local event failed", "event", ev.Type, "id", ev.ID, "err", err)
		return
	}
	if runsCreated > 0 || waitersMatched > 0 {
		e.kickWork()
	}
}

// NotifyFlowsChanged refreshes the engine's compiled flow index, reconciles
// schedules, and broadcasts updated subscriptions across the cluster.
func (e *Engine) NotifyFlowsChanged(ctx context.Context, flowID string) {
	if e.IsLeading() {
		if err := e.ReloadFlows(ctx); err != nil && ctx.Err() == nil {
			e.logger.Warn("flows: reload after change failed", "flowId", flowID, "err", err)
		}
		e.broadcastSubscriptions(ctx)
		e.kickWork()
	} else if e.cbus != nil {
		pctx, cancel := context.WithTimeout(ctx, outboxPublishTimeout)
		_ = e.cbus.Publish(pctx, TopicFlowsChanged, []byte(flowID))
		cancel()
	}
	if e.opts.OnFlowsChanged != nil {
		e.opts.OnFlowsChanged(flowID)
	}
}

// ReloadFlows loads all saved flows from Store and reconciles schedules for
// enabled flows.
func (e *Engine) ReloadFlows(ctx context.Context) error {
	flowsList, err := e.store.ListFlows(ctx)
	if err != nil {
		return err
	}
	enabled := make([]Flow, 0, len(flowsList))
	byID := make(map[string]Flow, len(flowsList))
	now := e.now().UTC()
	schedStore, hasSched := e.store.(scheduleStore)

	for _, f := range flowsList {
		byID[f.ID] = f
		if f.Enabled {
			enabled = append(enabled, f)
			if hasSched {
				if err := schedStore.SyncFlowSchedules(ctx, f, now); err != nil && ctx.Err() == nil {
					e.logger.Warn("flows: sync schedules failed", "flowId", f.ID, "err", err)
				}
			}
		}
	}

	e.rateMu.Lock()
	for id := range e.rateWindows {
		if _, exists := byID[id]; !exists {
			delete(e.rateWindows, id)
		}
	}
	for id := range e.throttleSampleAt {
		if _, exists := byID[id]; !exists {
			delete(e.throttleSampleAt, id)
		}
	}
	e.rateMu.Unlock()

	// Workers can update byID through e.flowsByID once it is published.
	e.mu.Lock()
	e.enabledFlows = enabled
	e.flowsByID = byID
	e.mu.Unlock()
	return nil
}

// SubscribedEvents returns the sorted, deduplicated union of event patterns
// referenced by enabled flows and active flows_waiters rows.
func (e *Engine) SubscribedEvents(ctx context.Context) ([]string, error) {
	flowsList, err := e.store.ListFlows(ctx)
	if err != nil {
		return nil, err
	}
	var patterns []string
	for _, f := range flowsList {
		if !f.Enabled {
			continue
		}
		patterns = append(patterns, f.Graph.SubscribedEventPatterns()...)
	}
	if ws, ok := e.store.(waiterPatternStore); ok {
		waitPatterns, err := ws.ActiveWaiterEventPatterns(ctx)
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, waitPatterns...)
	}
	return normalizePatterns(patterns), nil
}

func (e *Engine) broadcastSubscriptions(ctx context.Context) {
	patterns, err := e.SubscribedEvents(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.logger.Warn("flows: compute subscribed events failed", "err", err)
		}
		return
	}
	if e.bridge != nil {
		e.bridge.SetPatterns(patterns)
	}
	if e.cbus == nil {
		return
	}
	ver := e.subsVersion.Add(1)
	raw := encodeSubsBroadcast(e.origin, e.nodeID, ver, patterns)
	pctx, cancel := context.WithTimeout(ctx, outboxPublishTimeout)
	if err := e.cbus.Publish(pctx, TopicSubscriptions, raw); err != nil && ctx.Err() == nil {
		e.logger.Warn("flows: publish subscription set failed", "err", err)
	}
	cancel()
}

// --- Optional internal store extension interfaces ---

type dedupRunStore interface {
	CreateRunIfAbsent(ctx context.Context, run Run) (bool, error)
	RunExists(ctx context.Context, runID string) (bool, error)
}

type cancelRunStore interface {
	CancelRun(ctx context.Context, runID string, now time.Time) error
}

type waiterPatternStore interface {
	ActiveWaiterEventPatterns(ctx context.Context) ([]string, error)
}

type waiterWakeStore interface {
	ResolveWaiterAndWakeRun(ctx context.Context, commit WaiterMatchCommit, now time.Time) (bool, error)
	WakeRunAfterTimeout(ctx context.Context, runID string, mutate func(raw json.RawMessage) (json.RawMessage, error), now time.Time) (bool, error)
}

type outcomeStore interface {
	RecordFlowOutcome(ctx context.Context, flowID string, status RunStatus, finishedAt time.Time, autoDisableAfter int, errMsg string) (streak int, autoDisabled bool, disabledReason string, err error)
}

type scheduleStore interface {
	SyncFlowSchedules(ctx context.Context, f Flow, now time.Time) error
	ListDueSchedules(ctx context.Context, now time.Time, limit int) ([]ScheduleEntry, error)
	AdvanceScheduleAndCreateRun(ctx context.Context, flowID, nodeID string, expectedNext, lastRun, nextRun time.Time, run *Run) (bool, error)
}

type outboxCommitStore interface {
	CommitOutboxEvent(ctx context.Context, outboxID int64, ackedAt time.Time, runs []Run, waiters []WaiterMatchCommit) (runsCreated int, waitersMatched int, err error)
}

// --- Outbox draining & event ingestion ---

func (e *Engine) drainOutbox(ctx context.Context) {
	if e.outbox == nil {
		return
	}
	woke := false
	// Bounded batches per drain call so other leader work is never starved.
	for range 16 {
		if ctx.Err() != nil {
			return
		}
		rows, err := e.outbox.ListPending(ctx, defaultOutboxBatch)
		if err != nil {
			if ctx.Err() == nil {
				e.logger.Warn("flows: list pending outbox failed", "err", err)
			}
			return
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				return
			}
			var ev events.Envelope
			if err := json.Unmarshal(row.Envelope, &ev); err != nil {
				e.logger.Warn("flows: acking malformed outbox envelope", "outboxId", row.ID, "eventId", row.EventID, "err", err)
				_ = e.outbox.Ack(ctx, row.ID, e.now().UTC())
				continue
			}
			runsCreated, waitersMatched, err := e.ingestEvent(ctx, ev, row.ID)
			if err != nil {
				if ctx.Err() == nil {
					e.logger.Warn("flows: ingest outbox event failed", "outboxId", row.ID, "event", ev.Type, "err", err)
				}
				return
			}
			if runsCreated > 0 || waitersMatched > 0 {
				woke = true
			}
		}
		if len(rows) < defaultOutboxBatch {
			break
		}
	}
	if woke {
		e.kickWork()
	}
}

func (e *Engine) ingestEvent(ctx context.Context, ev events.Envelope, outboxID int64) (runsCreated int, waitersMatched int, err error) {
	now := e.now().UTC()
	if ev.Meta.DryRun {
		if outboxID > 0 && e.outbox != nil {
			_ = e.outbox.Ack(ctx, outboxID, now)
		}
		return 0, 0, nil
	}

	var evalCtx EvalContext
	evalReady := false
	getEvalCtx := func() (EvalContext, error) {
		if evalReady {
			return evalCtx, nil
		}
		var err error
		evalCtx, err = NewEvalContext(ev, nil, nil, nil)
		if err != nil {
			return EvalContext{}, err
		}
		evalReady = true
		return evalCtx, nil
	}

	// 1. Check active event waiters.
	var waiterCommits []WaiterMatchCommit
	waiters, err := e.store.MatchWaiters(ctx, ev.Type, ev.CorrelationKey(), now)
	if err != nil {
		return 0, 0, err
	}
	for _, w := range waiters {
		if len(strings.TrimSpace(string(w.Filter))) > 0 {
			var fg ConditionGroup
			if uErr := json.Unmarshal(w.Filter, &fg); uErr != nil {
				continue
			}
			if !fg.IsEmpty() {
				ec, ecErr := getEvalCtx()
				if ecErr != nil {
					continue
				}
				ok, _, evalErr := EvalConditionGroup(fg, ec)
				if evalErr != nil || !ok {
					continue
				}
			}
		}
		matchedEv := ev
		waiterCommits = append(waiterCommits, WaiterMatchCommit{
			WaiterID: w.ID,
			RunID:    w.RunID,
			NodeID:   w.NodeID,
			MutateContext: func(raw json.RawMessage) (json.RawMessage, error) {
				st, err := decodeRunState(raw)
				if err != nil {
					return nil, err
				}
				st.WaitResult = &waitResumeResult{Matched: true, Event: matchedEv}
				return encodeRunState(st)
			},
		})
	}

	// 2. Match enabled flow triggers (enforcing recursion depth cap, trigger pre-filter,
	//    failure backoff, and per-flow rate limit).
	var runsToCreate []Run
	if ev.Meta.FlowDepth < MaxFlowDepth {
		e.mu.RLock()
		flowsSnap := slices.Clone(e.enabledFlows)
		e.mu.RUnlock()

		for _, f := range flowsSnap {
			limits := f.Limits.Effective()
			for _, n := range f.Graph.Nodes {
				if n.Disabled || n.Kind != NodeTriggerEvent {
					continue
				}
				cfg, decErr := DecodeConfig[EventTriggerConfig](n)
				if decErr != nil {
					continue
				}
				if !matchesAnyPattern(cfg.Events, ev.Type) {
					continue
				}
				// Trigger pre-filter: evaluate before touching DB or rate-limit tokens.
				if cfg.Filter != nil && !cfg.Filter.IsEmpty() {
					ec, ecErr := getEvalCtx()
					if ecErr != nil {
						continue
					}
					ok, _, evalErr := EvalConditionGroup(*cfg.Filter, ec)
					if evalErr != nil || !ok {
						continue
					}
				}

				runID := NewRunID()
				if ev.ID != "" {
					runID = EventRunID(f.ID, n.ID, ev.ID)
					if ds, ok := e.store.(dedupRunStore); ok {
						if exists, err := ds.RunExists(ctx, runID); err == nil && exists {
							// Replay of an already-ingested event: skip without burning rate-limit tokens.
							continue
						}
					}
				}

				// Consecutive failure exponential backoff cooldown.
				if backoffUntil := e.failureBackoffUntil(f); !backoffUntil.IsZero() && now.Before(backoffUntil) {
					if e.shouldSampleThrottle(f.ID, now) {
						runsToCreate = append(runsToCreate, e.buildThrottledRun(f, NodeTriggerEvent, ev, now,
							fmt.Sprintf("failure backoff active until %s", backoffUntil.Format(time.RFC3339))))
					}
					continue
				}

				// Per-flow rate limit.
				if !e.allowRateLimit(f.ID, limits.RateLimitPerMin, now) {
					if e.shouldSampleThrottle(f.ID, now) {
						runsToCreate = append(runsToCreate, e.buildThrottledRun(f, NodeTriggerEvent, ev, now, "rate limit exceeded"))
					}
					continue
				}

				trigMeta := map[string]any{
					"kind":   string(NodeTriggerEvent),
					"nodeId": n.ID,
				}
				ctxRaw, encErr := encodeRunState(runState{
					Event:   ev,
					Trigger: trigMeta,
					Vars:    map[string]any{},
					Steps:   map[string]map[string]any{},
					Queue:   []string{n.ID},
					Visited: map[string]bool{},
					NextSeq: 1,
				})
				if encErr != nil {
					continue
				}
				runsToCreate = append(runsToCreate, Run{
					ID:             runID,
					FlowID:         f.ID,
					FlowName:       f.Name,
					FlowVersion:    f.Version,
					TriggerKind:    NodeTriggerEvent,
					EventID:        ev.ID,
					EventType:      ev.Type,
					CorrelationKey: ev.CorrelationKey(),
					Status:         RunPending,
					Attempt:        1,
					Trigger:        marshalJSONRaw(redactEventMap(e.opts.Events, ev)),
					Context:        ctxRaw,
					FlowDepth:      ev.Meta.FlowDepth + 1,
					StartedAt:      now,
					UpdatedAt:      now,
				})
			}
		}
	}

	// 3. Commit runs, waiter matches, and outbox ack atomically when backed by SQLStore.
	if ocs, ok := e.store.(outboxCommitStore); ok && (outboxID > 0 || len(runsToCreate) > 0 || len(waiterCommits) > 0) {
		runsCreated, waitersMatched, err = ocs.CommitOutboxEvent(ctx, outboxID, now, runsToCreate, waiterCommits)
		if err != nil {
			return 0, 0, err
		}
		if waitersMatched > 0 {
			e.broadcastSubscriptions(ctx)
		}
		return runsCreated, waitersMatched, nil
	}

	for _, run := range runsToCreate {
		if ds, ok := e.store.(dedupRunStore); ok {
			created, err := ds.CreateRunIfAbsent(ctx, run)
			if err != nil {
				return runsCreated, waitersMatched, err
			}
			if created && !run.Status.Terminal() {
				runsCreated++
			}
		} else {
			if err := e.store.CreateRun(ctx, run); err != nil {
				continue
			}
			if !run.Status.Terminal() {
				runsCreated++
			}
		}
	}
	for _, wc := range waiterCommits {
		if ws, ok := e.store.(waiterWakeStore); ok {
			matched, err := ws.ResolveWaiterAndWakeRun(ctx, wc, now)
			if err != nil {
				return runsCreated, waitersMatched, err
			}
			if matched {
				waitersMatched++
			}
		}
	}
	if outboxID > 0 && e.outbox != nil {
		if err := e.outbox.Ack(ctx, outboxID, now); err != nil {
			return runsCreated, waitersMatched, err
		}
	}
	if waitersMatched > 0 {
		e.broadcastSubscriptions(ctx)
	}
	return runsCreated, waitersMatched, nil
}

func (e *Engine) buildThrottledRun(f Flow, kind NodeKind, ev events.Envelope, now time.Time, reason string) Run {
	return Run{
		ID:             NewRunID(),
		FlowID:         f.ID,
		FlowName:       f.Name,
		FlowVersion:    f.Version,
		TriggerKind:    kind,
		EventID:        ev.ID,
		EventType:      ev.Type,
		CorrelationKey: ev.CorrelationKey(),
		Status:         RunThrottled,
		Attempt:        1,
		Trigger:        marshalJSONRaw(redactEventMap(e.opts.Events, ev)),
		Context:        json.RawMessage(`{}`),
		Error:          reason,
		FlowDepth:      ev.Meta.FlowDepth + 1,
		StartedAt:      now,
		UpdatedAt:      now,
		FinishedAt:     now,
	}
}

// --- Due-work orchestration (waiter expiry, schedules, run claiming & worker pool) ---

func (e *Engine) stepLeaderWork(ctx context.Context, sem chan struct{}, wg *sync.WaitGroup) {
	if ctx.Err() != nil {
		return
	}
	e.expireDueWaiters(ctx)
	e.processDueSchedules(ctx)

	avail := cap(sem) - len(sem)
	if avail <= 0 {
		return
	}
	claimLimit := min(avail, defaultClaimBatch)
	runs, err := e.store.ClaimDueRuns(ctx, e.nodeID, e.now().UTC(), e.opts.LeaseDuration, claimLimit)
	if err != nil {
		if ctx.Err() == nil {
			e.logger.Warn("flows: claim due runs failed", "err", err)
		}
		return
	}
	for _, r := range runs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		wg.Add(1)
		go func(run Run) {
			defer wg.Done()
			defer func() {
				<-sem
				// Re-check queue in case another run for this flow/correlation key was waiting.
				e.kickWork()
			}()
			e.runClaimed(ctx, run)
		}(r)
	}
}

// ProcessPending performs one deterministic, synchronous sweep of the outbox,
// expired waiters, due schedules, and due runs on the caller's goroutine.
// It is primarily intended for deterministic tests without wall-clock sleeps.
func (e *Engine) ProcessPending(ctx context.Context) error {
	if err := e.ReloadFlows(ctx); err != nil {
		return err
	}
	e.drainOutbox(ctx)
	e.expireDueWaiters(ctx)
	e.processDueSchedules(ctx)
	for range 16 {
		runs, err := e.store.ClaimDueRuns(ctx, e.nodeID, e.now().UTC(), e.opts.LeaseDuration, defaultClaimBatch)
		if err != nil {
			return err
		}
		if len(runs) == 0 {
			return nil
		}
		for _, r := range runs {
			e.runClaimed(ctx, r)
		}
	}
	return nil
}

func (e *Engine) runClaimed(ctx context.Context, run Run) {
	flow, err := e.lookupFlow(ctx, run.FlowID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Flow was deleted after run creation.
			return
		}
		if ctx.Err() == nil {
			e.logger.Warn("flows: load flow for run failed", "runId", run.ID, "flowId", run.FlowID, "err", err)
		}
		return
	}
	if _, _, execErr := e.executeRun(ctx, run, *flow, modeDurable, true); execErr != nil && ctx.Err() == nil {
		e.logger.Warn("flows: execute run error", "runId", run.ID, "flowId", run.FlowID, "err", execErr)
	}
}

func (e *Engine) expireDueWaiters(ctx context.Context) {
	now := e.now().UTC()
	expired, err := e.store.ExpireWaiters(ctx, now, 64)
	if err != nil {
		if ctx.Err() == nil {
			e.logger.Warn("flows: expire waiters failed", "err", err)
		}
		return
	}
	if len(expired) == 0 {
		return
	}
	ws, hasWake := e.store.(waiterWakeStore)
	wokeAny := false
	for _, w := range expired {
		mutate := func(raw json.RawMessage) (json.RawMessage, error) {
			st, err := decodeRunState(raw)
			if err != nil {
				return nil, err
			}
			st.WaitResult = &waitResumeResult{Matched: false}
			return encodeRunState(st)
		}
		if hasWake {
			ok, err := ws.WakeRunAfterTimeout(ctx, w.RunID, mutate, now)
			if err != nil && ctx.Err() == nil {
				e.logger.Warn("flows: wake run after waiter timeout failed", "runId", w.RunID, "err", err)
				continue
			}
			if ok {
				wokeAny = true
			}
		} else {
			r, err := e.store.GetRun(ctx, w.RunID)
			if err != nil || r.Status != RunWaiting {
				continue
			}
			updatedCtx, err := mutate(r.Context)
			if err != nil {
				continue
			}
			r.Context = updatedCtx
			r.Status = RunPending
			r.WakeAt = time.Time{}
			r.UpdatedAt = now
			if err := e.store.UpdateRun(ctx, *r); err == nil {
				wokeAny = true
			}
		}
	}
	e.broadcastSubscriptions(ctx)
	if wokeAny {
		e.kickWork()
	}
}

func (e *Engine) processDueSchedules(ctx context.Context) {
	schedStore, ok := e.store.(scheduleStore)
	if !ok {
		return
	}
	now := e.now().UTC()
	due, err := schedStore.ListDueSchedules(ctx, now, 64)
	if err != nil {
		if ctx.Err() == nil {
			e.logger.Warn("flows: list due schedules failed", "err", err)
		}
		return
	}
	createdAny := false
	for _, entry := range due {
		flow, err := e.lookupFlow(ctx, entry.FlowID)
		if err != nil || !flow.Enabled {
			continue
		}
		var trigNode *Node
		for i := range flow.Graph.Nodes {
			if flow.Graph.Nodes[i].ID == entry.NodeID && !flow.Graph.Nodes[i].Disabled && flow.Graph.Nodes[i].Kind == NodeTriggerSchedule {
				trigNode = &flow.Graph.Nodes[i]
				break
			}
		}
		if trigNode == nil {
			continue
		}
		cfg, err := DecodeConfig[ScheduleTriggerConfig](*trigNode)
		if err != nil {
			continue
		}
		// Wall-clock semantics: compute next run strictly after now so sleep/resume
		// coalesces missed ticks instead of creating a backlog storm.
		nextRun, err := NextScheduleTime(cfg, now)
		if err != nil {
			continue
		}

		var runPtr *Run
		limits := flow.Limits.Effective()
		backoffUntil := e.failureBackoffUntil(*flow)
		if !backoffUntil.IsZero() && now.Before(backoffUntil) {
			// Skip firing during backoff, still advance next_run_ms.
			runPtr = nil
		} else if !e.allowRateLimit(flow.ID, limits.RateLimitPerMin, now) {
			runPtr = nil
		} else {
			trigMeta := map[string]any{
				"kind":        string(NodeTriggerSchedule),
				"nodeId":      trigNode.ID,
				"scheduledAt": entry.NextRun.UTC().Format(time.RFC3339),
				"firedAt":     now.Format(time.RFC3339),
			}
			ev := events.Envelope{
				ID:         events.NewID(),
				Type:       "flows.schedule.fired",
				Version:    1,
				OccurredAt: now,
				Node:       e.nodeID,
				Source:     events.SourceBuiltin,
				Payload:    json.RawMessage(`{}`),
			}
			ctxRaw, err := encodeRunState(runState{
				Event:   ev,
				Trigger: trigMeta,
				Vars:    map[string]any{},
				Steps:   map[string]map[string]any{},
				Queue:   []string{trigNode.ID},
				Visited: map[string]bool{},
				NextSeq: 1,
			})
			if err != nil {
				continue
			}
			r := Run{
				ID:             ScheduleRunID(flow.ID, trigNode.ID, entry.NextRun),
				FlowID:         flow.ID,
				FlowName:       flow.Name,
				FlowVersion:    flow.Version,
				TriggerKind:    NodeTriggerSchedule,
				CorrelationKey: "schedule:" + flow.ID + ":" + trigNode.ID,
				Status:         RunPending,
				Attempt:        1,
				Trigger:        marshalJSONRaw(trigMeta),
				Context:        ctxRaw,
				FlowDepth:      1,
				StartedAt:      now,
				UpdatedAt:      now,
			}
			runPtr = &r
		}

		created, err := schedStore.AdvanceScheduleAndCreateRun(ctx, flow.ID, trigNode.ID, entry.NextRun, now, nextRun, runPtr)
		if err != nil {
			if ctx.Err() == nil {
				e.logger.Warn("flows: advance schedule failed", "flowId", flow.ID, "nodeId", trigNode.ID, "err", err)
			}
			continue
		}
		if created {
			createdAny = true
		}
	}
	if createdAny {
		e.kickWork()
	}
}

func (e *Engine) prune(ctx context.Context) {
	now := e.now().UTC()
	if _, err := e.store.PruneHistory(ctx, now.Add(-e.opts.RetentionAge), e.opts.RetentionMax); err != nil && ctx.Err() == nil {
		e.logger.Warn("flows: history prune failed", "err", err)
	}
	if e.outbox != nil {
		if _, err := e.outbox.PruneAcked(ctx, now.Add(-defaultOutboxMaxAge)); err != nil && ctx.Err() == nil {
			e.logger.Warn("flows: outbox prune failed", "err", err)
		}
	}
}

// --- Manual, Webhook, TestRun & Cancel ---

// TriggerManual creates a durable pending run for a flow's manual trigger node.
func (e *Engine) TriggerManual(ctx context.Context, flowID string, inputs map[string]any) (*Run, error) {
	flow, err := e.store.GetFlow(ctx, flowID)
	if err != nil {
		return nil, err
	}
	if !flow.Enabled {
		return nil, ErrFlowDisabled
	}
	var trigNode *Node
	for i := range flow.Graph.Nodes {
		if !flow.Graph.Nodes[i].Disabled && flow.Graph.Nodes[i].Kind == NodeTriggerManual {
			trigNode = &flow.Graph.Nodes[i]
			break
		}
	}
	if trigNode == nil {
		return nil, ErrNoManualTrigger
	}

	now := e.now().UTC()
	if !e.allowRateLimit(flow.ID, flow.Limits.Effective().RateLimitPerMin, now) {
		return nil, ErrRateLimited
	}

	cfg, _ := DecodeConfig[ManualTriggerConfig](*trigNode)
	merged := mergeManualInputs(cfg.Inputs, inputs)
	trigMeta := buildManualTriggerMap(trigNode.ID, merged)
	ev := events.Envelope{
		ID:         events.NewID(),
		Type:       "flows.manual.triggered",
		Version:    1,
		OccurredAt: now,
		Node:       e.nodeID,
		Source:     events.SourceBuiltin,
		Payload:    marshalJSONRaw(merged),
	}
	ctxRaw, err := encodeRunState(runState{
		Event:   ev,
		Trigger: trigMeta,
		Vars:    map[string]any{},
		Steps:   map[string]map[string]any{},
		Queue:   []string{trigNode.ID},
		Visited: map[string]bool{},
		NextSeq: 1,
	})
	if err != nil {
		return nil, err
	}

	run := Run{
		ID:             NewRunID(),
		FlowID:         flow.ID,
		FlowName:       flow.Name,
		FlowVersion:    flow.Version,
		TriggerKind:    NodeTriggerManual,
		CorrelationKey: "manual:" + flow.ID,
		Status:         RunPending,
		Attempt:        1,
		Trigger:        marshalJSONRaw(trigMeta),
		Context:        ctxRaw,
		FlowDepth:      1,
		StartedAt:      now,
		UpdatedAt:      now,
	}
	if err := e.store.CreateRun(ctx, run); err != nil {
		return nil, err
	}
	e.wakeClusterOrLocal(ctx)
	return &run, nil
}

// TriggerWebhook authenticates token against stored webhook hashes and creates
// a durable pending run for the matching flow's webhook trigger node.
func (e *Engine) TriggerWebhook(ctx context.Context, token string, payload map[string]any, headers map[string]string) (*Run, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrInvalidWebhook
	}
	flow, err := e.store.FindFlowByWebhookHash(ctx, HashWebhookToken(token))
	if errors.Is(err, ErrNotFound) {
		return nil, ErrInvalidWebhook
	}
	if err != nil {
		return nil, err
	}
	if !flow.Enabled {
		return nil, ErrFlowDisabled
	}

	var trigNode *Node
	for i := range flow.Graph.Nodes {
		if !flow.Graph.Nodes[i].Disabled && flow.Graph.Nodes[i].Kind == NodeTriggerWebhook {
			trigNode = &flow.Graph.Nodes[i]
			break
		}
	}
	if trigNode == nil {
		return nil, ErrInvalidWebhook
	}

	now := e.now().UTC()
	if !e.allowRateLimit(flow.ID, flow.Limits.Effective().RateLimitPerMin, now) {
		return nil, ErrRateLimited
	}

	if payload == nil {
		payload = map[string]any{}
	}
	hdrMap := make(map[string]any, len(headers))
	for k, v := range headers {
		hdrMap[k] = v
	}
	trigMeta := map[string]any{
		"kind":    string(NodeTriggerWebhook),
		"nodeId":  trigNode.ID,
		"headers": hdrMap,
		"payload": payload,
	}
	ev := events.Envelope{
		ID:         events.NewID(),
		Type:       "flows.webhook.received",
		Version:    1,
		OccurredAt: now,
		Node:       e.nodeID,
		Source:     events.SourceBuiltin,
		Payload:    marshalJSONRaw(payload),
	}
	ctxRaw, err := encodeRunState(runState{
		Event:   ev,
		Trigger: trigMeta,
		Vars:    map[string]any{},
		Steps:   map[string]map[string]any{},
		Queue:   []string{trigNode.ID},
		Visited: map[string]bool{},
		NextSeq: 1,
	})
	if err != nil {
		return nil, err
	}

	run := Run{
		ID:             NewRunID(),
		FlowID:         flow.ID,
		FlowName:       flow.Name,
		FlowVersion:    flow.Version,
		TriggerKind:    NodeTriggerWebhook,
		CorrelationKey: "webhook:" + flow.ID,
		Status:         RunPending,
		Attempt:        1,
		Trigger:        marshalJSONRaw(trigMeta),
		Context:        ctxRaw,
		FlowDepth:      1,
		StartedAt:      now,
		UpdatedAt:      now,
	}
	if err := e.store.CreateRun(ctx, run); err != nil {
		return nil, err
	}
	e.wakeClusterOrLocal(ctx)
	return &run, nil
}

// TestRun synchronously executes a flow (or unsaved draft graph) in either dry
// mode (zero persistent side effects, ActionContext.DryRun=true) or live mode
// (real action execution, ActionContext.DryRun=false).
func (e *Engine) TestRun(ctx context.Context, req TestRunRequest) (*TestRunResult, error) {
	var flow Flow
	persistedFlow := false
	if req.FlowID != "" {
		f, err := e.store.GetFlow(ctx, req.FlowID)
		if err != nil {
			if !errors.Is(err, ErrNotFound) || req.Graph == nil {
				return nil, err
			}
		} else {
			flow = *f
			persistedFlow = true
		}
	}
	if req.Graph != nil {
		flow.Graph = *req.Graph
		if flow.Name == "" {
			flow.Name = "Draft"
		}
	}
	if len(flow.Graph.Nodes) == 0 {
		return nil, ErrValidationFailed
	}

	trigNode := selectTestTrigger(flow.Graph, req)
	if trigNode == nil {
		return nil, ErrValidationFailed
	}

	now := e.now().UTC()
	ev := req.Event
	var trigMeta map[string]any

	switch trigNode.Kind {
	case NodeTriggerEvent:
		cfg, _ := DecodeConfig[EventTriggerConfig](*trigNode)
		if ev.Type == "" {
			ev = e.sampleEventForPatterns(cfg.Events, now)
		} else if trimmed := strings.TrimSpace(string(ev.Payload)); (trimmed == "" || trimmed == "null") && e.opts.Events != nil {
			if s, ok := e.opts.Events.Get(ev.Type); ok && s.Sample != nil {
				ev.Payload = marshalJSONRaw(s.Sample)
				if ev.Version <= 0 {
					ev.Version = max(s.Version, 1)
				}
			}
		}
		if ev.ID == "" {
			ev.ID = events.NewID()
		}
		if ev.OccurredAt.IsZero() {
			ev.OccurredAt = now
		}
		if ev.Version <= 0 {
			ev.Version = 1
		}
		if len(ev.Payload) == 0 {
			ev.Payload = json.RawMessage(`{}`)
		}
		if ev.Node == "" {
			ev.Node = e.nodeID
		}
		if ev.Source == "" {
			ev.Source = events.SourceBuiltin
		}
		ev.Meta.DryRun = req.DryRun
		trigMeta = map[string]any{
			"kind":   string(NodeTriggerEvent),
			"nodeId": trigNode.ID,
			"dryRun": req.DryRun,
		}

	case NodeTriggerManual:
		cfg, _ := DecodeConfig[ManualTriggerConfig](*trigNode)
		merged := mergeManualInputs(cfg.Inputs, req.Inputs)
		trigMeta = buildManualTriggerMap(trigNode.ID, merged)
		if ev.Type == "" {
			ev.Type = "flows.manual.triggered"
		}
		if len(ev.Payload) == 0 {
			ev.Payload = marshalJSONRaw(merged)
		}
		ev.ID = firstNonEmpty(ev.ID, events.NewID())
		ev.OccurredAt = now
		ev.Version = max(ev.Version, 1)
		ev.Meta.DryRun = req.DryRun

	case NodeTriggerWebhook:
		payload := req.Inputs
		if payload == nil {
			payload = map[string]any{}
		}
		trigMeta = map[string]any{
			"kind":    string(NodeTriggerWebhook),
			"nodeId":  trigNode.ID,
			"headers": map[string]any{},
			"payload": payload,
		}
		if ev.Type == "" {
			ev.Type = "flows.webhook.received"
		}
		if len(ev.Payload) == 0 {
			ev.Payload = marshalJSONRaw(payload)
		}
		ev.ID = firstNonEmpty(ev.ID, events.NewID())
		ev.OccurredAt = now
		ev.Version = max(ev.Version, 1)
		ev.Meta.DryRun = req.DryRun

	case NodeTriggerSchedule:
		trigMeta = map[string]any{
			"kind":        string(NodeTriggerSchedule),
			"nodeId":      trigNode.ID,
			"scheduledAt": now.Format(time.RFC3339),
			"firedAt":     now.Format(time.RFC3339),
		}
		if ev.Type == "" {
			ev.Type = "flows.schedule.fired"
		}
		if len(ev.Payload) == 0 {
			ev.Payload = json.RawMessage(`{}`)
		}
		ev.ID = firstNonEmpty(ev.ID, events.NewID())
		ev.OccurredAt = now
		ev.Version = max(ev.Version, 1)
		ev.Meta.DryRun = req.DryRun
	}

	ctxRaw, err := encodeRunState(runState{
		Event:   ev,
		Trigger: trigMeta,
		Vars:    map[string]any{},
		Steps:   map[string]map[string]any{},
		Queue:   []string{trigNode.ID},
		Visited: map[string]bool{},
		NextSeq: 1,
	})
	if err != nil {
		return nil, err
	}

	mode := modeTestLive
	persist := persistedFlow && !req.DryRun
	if req.DryRun {
		mode = modeTestDry
		persist = false
	}

	run := Run{
		ID:             NewRunID(),
		FlowID:         flow.ID,
		FlowName:       flow.Name,
		FlowVersion:    max(flow.Version, 1),
		TriggerKind:    trigNode.Kind,
		EventID:        ev.ID,
		EventType:      ev.Type,
		CorrelationKey: ev.CorrelationKey(),
		Status:         RunRunning,
		Attempt:        1,
		DryRun:         req.DryRun,
		Trigger:        marshalJSONRaw(trigMeta),
		Context:        ctxRaw,
		FlowDepth:      1,
		StartedAt:      now,
		UpdatedAt:      now,
		LeaseOwner:     e.nodeID,
		LeaseUntil:     now.Add(e.opts.LeaseDuration),
	}
	if persist {
		if err := e.store.CreateRun(ctx, run); err != nil {
			return nil, err
		}
	}

	finalRun, steps, err := e.executeRun(ctx, run, flow, mode, persist)
	if err != nil {
		return nil, err
	}
	return &TestRunResult{Run: finalRun, Steps: steps}, nil
}

// CancelRun cancels an active pending, running, or waiting run and any active
// waiters belonging to it.
func (e *Engine) CancelRun(ctx context.Context, runID string) error {
	now := e.now().UTC()
	if cs, ok := e.store.(cancelRunStore); ok {
		if err := cs.CancelRun(ctx, runID, now); err != nil {
			return err
		}
	} else {
		r, err := e.store.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		if r.Status.Terminal() {
			return ErrRunNotActive
		}
		r.Status = RunCancelled
		r.FinishedAt = now
		r.UpdatedAt = now
		r.WakeAt = time.Time{}
		r.LeaseOwner = ""
		r.LeaseUntil = time.Time{}
		if err := e.store.UpdateRun(ctx, *r); err != nil {
			return err
		}
	}

	const maxTrackedCancels = 512
	e.cancelMu.Lock()
	if !e.cancelledRuns[runID] {
		e.cancelledRuns[runID] = true
		e.cancelledOrd = append(e.cancelledOrd, runID)
		if len(e.cancelledOrd) > maxTrackedCancels {
			oldest := e.cancelledOrd[0]
			e.cancelledOrd = e.cancelledOrd[1:]
			delete(e.cancelledRuns, oldest)
		}
	}
	cancel := e.activeCancels[runID]
	e.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}

	if e.IsLeading() {
		e.broadcastSubscriptions(ctx)
	}
	return nil
}

// --- Failure backoff, auto-disable, rate limiting & helpers ---

func (e *Engine) failureBackoffDuration(consecutiveFailures int) time.Duration {
	if consecutiveFailures < e.opts.BackoffThreshold {
		return 0
	}
	shift := min(consecutiveFailures-e.opts.BackoffThreshold, 12)
	d := e.opts.BackoffBase << shift
	if d <= 0 || d > e.opts.BackoffMax {
		return e.opts.BackoffMax
	}
	return d
}

func (e *Engine) failureBackoffUntil(f Flow) time.Time {
	if f.LastStatus != RunFailed || f.LastRunAt.IsZero() {
		return time.Time{}
	}
	d := e.failureBackoffDuration(f.ConsecutiveFailures)
	if d <= 0 {
		return time.Time{}
	}
	return f.LastRunAt.UTC().Add(d)
}

func (e *Engine) recordFlowOutcome(ctx context.Context, flow Flow, run Run, failedNodeID string) {
	limits := flow.Limits.Effective()
	now := run.FinishedAt
	if now.IsZero() {
		now = e.now().UTC()
	}

	var (
		streak         int
		autoDisabled   bool
		disabledReason string
		err            error
	)
	if os, ok := e.store.(outcomeStore); ok {
		streak, autoDisabled, disabledReason, err = os.RecordFlowOutcome(ctx, flow.ID, run.Status, now, limits.AutoDisableAfter, run.Error)
		if err != nil && ctx.Err() == nil {
			e.logger.Warn("flows: record flow outcome failed", "flowId", flow.ID, "err", err)
			return
		}
	} else if run.Status == RunFailed {
		streak = flow.ConsecutiveFailures + 1
		if limits.AutoDisableAfter > 0 && streak >= limits.AutoDisableAfter {
			autoDisabled = true
			disabledReason = fmt.Sprintf("auto-disabled after %d consecutive failures: %s", streak, run.Error)
			_ = e.store.SetFlowEnabled(ctx, flow.ID, false, disabledReason)
		}
	}

	// Update in-memory flow index immediately so subsequent events observe
	// updated backoff/auto-disable state without waiting for a reload tick.
	e.mu.Lock()
	if cur, ok := e.flowsByID[flow.ID]; ok {
		cur.LastRunAt = now
		cur.LastStatus = run.Status
		cur.ConsecutiveFailures = streak
		if autoDisabled {
			cur.Enabled = false
			cur.DisabledReason = disabledReason
		}
		e.flowsByID[flow.ID] = cur
		flow = cur
		if autoDisabled {
			e.enabledFlows = slices.DeleteFunc(e.enabledFlows, func(x Flow) bool { return x.ID == flow.ID })
		} else {
			for i := range e.enabledFlows {
				if e.enabledFlows[i].ID == flow.ID {
					e.enabledFlows[i] = cur
					break
				}
			}
		}
	}
	e.mu.Unlock()

	if run.Status == RunFailed && e.opts.Bus != nil {
		_ = e.opts.Bus.PublishTyped("flows.run.failed", map[string]any{
			"flowId":              flow.ID,
			"flowName":            flow.Name,
			"runId":               run.ID,
			"nodeId":              failedNodeID,
			"error":               run.Error,
			"consecutiveFailures": streak,
		},
			events.WithNode(e.nodeID),
			events.WithMeta(events.EnvelopeMeta{
				FlowDepth:   run.FlowDepth,
				OriginRunID: run.ID,
			}),
		)
	}

	if autoDisabled {
		if e.opts.Bus != nil {
			_ = e.opts.Bus.PublishTyped("flows.flow.auto_disabled", map[string]any{
				"flowId":              flow.ID,
				"flowName":            flow.Name,
				"consecutiveFailures": streak,
				"lastError":           run.Error,
			},
				events.WithNode(e.nodeID),
				events.WithMeta(events.EnvelopeMeta{
					Durable:     true,
					FlowDepth:   run.FlowDepth,
					OriginRunID: run.ID,
				}),
			)
		}
		if e.opts.OnAutoDisable != nil {
			e.opts.OnAutoDisable(flow, run)
		}
		if _, isSQL := e.store.(*SQLStore); !isSQL && e.opts.OnFlowsChanged != nil {
			e.opts.OnFlowsChanged(flow.ID)
		}
		e.broadcastSubscriptions(ctx)
	}
}

func (e *Engine) allowRateLimit(flowID string, limitPerMin int, now time.Time) bool {
	if limitPerMin <= 0 {
		return true
	}
	cutoff := now.Add(-time.Minute)
	e.rateMu.Lock()
	defer e.rateMu.Unlock()

	ts := e.rateWindows[flowID]
	keep := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	if len(keep) >= limitPerMin {
		e.rateWindows[flowID] = keep
		return false
	}
	e.rateWindows[flowID] = append(keep, now)
	return true
}

func (e *Engine) shouldSampleThrottle(flowID string, now time.Time) bool {
	e.rateMu.Lock()
	defer e.rateMu.Unlock()
	last := e.throttleSampleAt[flowID]
	if !last.IsZero() && now.Sub(last) < time.Minute {
		return false
	}
	e.throttleSampleAt[flowID] = now
	return true
}

func (e *Engine) withRunCancel(parent context.Context, runID string) (context.Context, func()) {
	rctx, cancel := context.WithCancel(parent)
	e.cancelMu.Lock()
	if e.cancelledRuns[runID] {
		e.cancelMu.Unlock()
		cancel()
		return rctx, func() {}
	}
	e.activeCancels[runID] = cancel
	e.cancelMu.Unlock()
	return rctx, func() {
		cancel()
		e.cancelMu.Lock()
		delete(e.activeCancels, runID)
		e.cancelMu.Unlock()
	}
}

func (e *Engine) isCancelled(runID string) bool {
	e.cancelMu.Lock()
	defer e.cancelMu.Unlock()
	return e.cancelledRuns[runID]
}

func (e *Engine) lookupFlow(ctx context.Context, flowID string) (*Flow, error) {
	e.mu.RLock()
	f, ok := e.flowsByID[flowID]
	e.mu.RUnlock()
	if ok {
		return &f, nil
	}
	return e.store.GetFlow(ctx, flowID)
}

func (e *Engine) wakeClusterOrLocal(ctx context.Context) {
	if e.IsLeading() {
		e.kickWork()
	}
	// Always ping the cluster too: a node that just lost leadership still
	// reports leading until its loop exits, and a run it creates in that
	// window must still reach the new leader. A duplicate kick is harmless.
	if e.cbus != nil {
		pctx, cancel := context.WithTimeout(ctx, outboxPublishTimeout)
		_ = e.cbus.Publish(pctx, TopicOutboxWake, []byte(e.nodeID))
		cancel()
	}
}

func (e *Engine) emitRunUpdated(run Run) {
	if e.opts.OnRunUpdated != nil {
		e.opts.OnRunUpdated(run)
	}
}

func (e *Engine) kickWork() {
	select {
	case e.workCh <- struct{}{}:
	default:
	}
}

func (e *Engine) kickOutbox() {
	select {
	case e.outboxCh <- struct{}{}:
	default:
	}
}

func (e *Engine) kickReload() {
	select {
	case e.reloadCh <- struct{}{}:
	default:
	}
}

func (e *Engine) sampleEventForPatterns(patterns []string, now time.Time) events.Envelope {
	eventType := "flows.test.event"
	var sample any = map[string]any{}
	version := 1

	for _, p := range patterns {
		if p == "" {
			continue
		}
		if e.opts.Events != nil {
			if s, ok := e.opts.Events.Get(p); ok {
				eventType = s.Type
				version = max(s.Version, 1)
				if s.Sample != nil {
					sample = s.Sample
				}
				break
			}
			for _, s := range e.opts.Events.List() {
				if events.MatchPattern(p, s.Type) {
					eventType = s.Type
					version = max(s.Version, 1)
					if s.Sample != nil {
						sample = s.Sample
					}
					goto done
				}
			}
		}
		if !strings.Contains(p, "*") && !strings.HasSuffix(p, ".") && !strings.HasSuffix(p, "/") {
			eventType = p
			break
		}
	}
done:
	return events.Envelope{
		ID:         events.NewID(),
		Type:       eventType,
		Version:    version,
		OccurredAt: now,
		Node:       e.nodeID,
		Source:     events.SourceBuiltin,
		Payload:    marshalJSONRaw(sample),
	}
}

func selectTestTrigger(g Graph, req TestRunRequest) *Node {
	if req.Trigger != "" {
		for i := range g.Nodes {
			if !g.Nodes[i].Disabled && g.Nodes[i].Kind == req.Trigger {
				return &g.Nodes[i]
			}
		}
	}
	if req.Event.Type != "" {
		var firstEventTrig *Node
		for i := range g.Nodes {
			n := &g.Nodes[i]
			if n.Disabled || n.Kind != NodeTriggerEvent {
				continue
			}
			if firstEventTrig == nil {
				firstEventTrig = n
			}
			if cfg, err := DecodeConfig[EventTriggerConfig](*n); err == nil && matchesAnyPattern(cfg.Events, req.Event.Type) {
				return n
			}
		}
		if firstEventTrig != nil {
			return firstEventTrig
		}
	}
	if len(req.Inputs) > 0 {
		for i := range g.Nodes {
			if !g.Nodes[i].Disabled && g.Nodes[i].Kind == NodeTriggerManual {
				return &g.Nodes[i]
			}
		}
	}
	for i := range g.Nodes {
		if !g.Nodes[i].Disabled && g.Nodes[i].Kind.IsTrigger() {
			return &g.Nodes[i]
		}
	}
	return nil
}

func matchesAnyPattern(patterns []string, eventType string) bool {
	for _, p := range patterns {
		if events.MatchPattern(p, eventType) {
			return true
		}
	}
	return false
}

func mergeManualInputs(schema []ParamSchema, provided map[string]any) map[string]any {
	out := make(map[string]any, len(schema)+len(provided))
	for _, p := range schema {
		if p.Default != nil {
			out[p.Key] = p.Default
		}
	}
	for k, v := range provided {
		out[k] = v
	}
	return out
}

func buildManualTriggerMap(nodeID string, inputs map[string]any) map[string]any {
	out := make(map[string]any, len(inputs)+3)
	for k, v := range inputs {
		out[k] = v
	}
	out["kind"] = string(NodeTriggerManual)
	out["nodeId"] = nodeID
	out["inputs"] = inputs
	return out
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
