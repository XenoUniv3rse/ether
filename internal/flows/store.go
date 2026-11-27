package flows

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/store"
)

// StoreOption customizes a SQLStore.
type StoreOption func(*SQLStore)

// WithStoreNow overrides the clock used by SQLStore (defaults to time.Now).
func WithStoreNow(now func() time.Time) StoreOption {
	return func(s *SQLStore) {
		if now != nil {
			s.now = now
		}
	}
}

// WithStoreActions supplies the schema for action parameters on graph writes.
func WithStoreActions(actions ActionRegistry) StoreOption {
	return func(s *SQLStore) { s.actions = actions }
}

// WithStoreOnChange registers a callback invoked after a flow definition is
// saved, deleted, or enabled/disabled.
func WithStoreOnChange(fn func(flowID string)) StoreOption {
	return func(s *SQLStore) {
		if fn != nil {
			s.onChanges = append(s.onChanges, fn)
		}
	}
}

// AddOnChange appends a callback invoked after a flow definition changes.
func (s *SQLStore) AddOnChange(fn func(flowID string)) {
	if fn == nil {
		return
	}
	s.cbMu.Lock()
	s.onChanges = append(s.onChanges, fn)
	s.cbMu.Unlock()
}

// SQLStore implements Store over *sql.DB (SQLite and PostgreSQL) using flows/1.
type SQLStore struct {
	db      *sql.DB
	dialect store.Dialect
	now     func() time.Time
	actions ActionRegistry

	cbMu      sync.RWMutex
	onChanges []func(flowID string)
}

var _ Store = (*SQLStore)(nil)

// NewSQLStore constructs a durable Store backed by db.
func NewSQLStore(db *sql.DB, opts ...StoreOption) *SQLStore {
	s := &SQLStore{
		db:      db,
		dialect: store.DialectOf(db),
		now:     time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// DB returns the underlying *sql.DB handle.
func (s *SQLStore) DB() *sql.DB { return s.db }

// NewFlowID generates a random flow identifier ("flow_<16 hex>").
func NewFlowID() string { return "flow_" + randomHex(8) }

// NewRunID generates a random run identifier ("run_<16 hex>").
func NewRunID() string { return "run_" + randomHex(8) }

// NewWaiterID generates a random waiter identifier ("wait_<16 hex>").
func NewWaiterID() string { return "wait_" + randomHex(8) }

// EventRunID derives a deterministic, replay-safe run ID for an event-triggered run.
func EventRunID(flowID, triggerNodeID, eventID string) string {
	sum := sha256.Sum256([]byte("ether-flow-evt:" + flowID + "\x00" + triggerNodeID + "\x00" + eventID))
	return "run_evt_" + hex.EncodeToString(sum[:12])
}

// ScheduleRunID derives a deterministic run ID for one schedule firing slot.
func ScheduleRunID(flowID, nodeID string, slot time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("ether-flow-sch:%s\x00%s\x00%d", flowID, nodeID, slot.UTC().UnixMilli())))
	return "run_sch_" + hex.EncodeToString(sum[:12])
}

// HashWebhookToken returns the SHA-256 hex digest stored in flows_definitions.webhook_token_hash.
func HashWebhookToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func msToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func timeToMS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixMilli()
}

func defaultJSON(raw json.RawMessage, fallback string) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return fallback
	}
	return trimmed
}

func (s *SQLStore) notifyChange(flowID string) {
	s.cbMu.RLock()
	cbs := slices.Clone(s.onChanges)
	s.cbMu.RUnlock()
	for _, fn := range cbs {
		fn(flowID)
	}
}

// --- Flow definitions ---

func scanFlow(scanner interface{ Scan(dest ...any) error }) (Flow, error) {
	var (
		f         Flow
		enabled   int
		graphJSON string
		limJSON   string
		tagsJSON  string
		createdMS int64
		updatedMS int64
		lastRunMS int64
		lastStat  string
	)
	err := scanner.Scan(
		&f.ID, &f.Name, &f.Description, &enabled, &f.DisabledReason,
		&f.Version, &graphJSON, &limJSON, &tagsJSON, &f.TemplateID,
		&f.ConsecutiveFailures, &createdMS, &updatedMS, &lastRunMS, &lastStat,
	)
	if err != nil {
		return Flow{}, err
	}
	f.Enabled = enabled != 0
	if err := json.Unmarshal([]byte(graphJSON), &f.Graph); err != nil {
		return Flow{}, fmt.Errorf("flows: decode graph for %q: %w", f.ID, err)
	}
	if strings.TrimSpace(limJSON) != "" {
		if err := json.Unmarshal([]byte(limJSON), &f.Limits); err != nil {
			return Flow{}, fmt.Errorf("flows: decode limits for %q: %w", f.ID, err)
		}
	}
	if strings.TrimSpace(tagsJSON) != "" && tagsJSON != "[]" && tagsJSON != "null" {
		if err := json.Unmarshal([]byte(tagsJSON), &f.Tags); err != nil {
			return Flow{}, fmt.Errorf("flows: decode tags for %q: %w", f.ID, err)
		}
	}
	f.CreatedAt = msToTime(createdMS)
	f.UpdatedAt = msToTime(updatedMS)
	f.LastRunAt = msToTime(lastRunMS)
	f.LastStatus = RunStatus(lastStat)
	return f, nil
}

const flowSelectCols = `id, name, description, enabled, disabled_reason, version, graph_json, limits_json, tags_json, template_id, consecutive_failures, created_at_ms, updated_at_ms, last_run_at_ms, last_status`

func (s *SQLStore) ListFlows(ctx context.Context) ([]Flow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+flowSelectCols+` FROM flows_definitions ORDER BY created_at_ms ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Flow
	for rows.Next() {
		f, err := scanFlow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *SQLStore) GetFlow(ctx context.Context, id string) (*Flow, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+flowSelectCols+` FROM flows_definitions WHERE id=?`, id)
	f, err := scanFlow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *SQLStore) SaveFlow(ctx context.Context, f Flow) (*Flow, error) {
	now := s.now().UTC()
	if strings.TrimSpace(f.ID) == "" {
		f.ID = NewFlowID()
	}
	limitsBytes, err := json.Marshal(f.Limits)
	if err != nil {
		return nil, fmt.Errorf("flows: marshal limits: %w", err)
	}
	patterns := f.Graph.SubscribedEventPatterns()
	sort.Strings(patterns)
	if patterns == nil {
		patterns = []string{}
	}
	triggerEventsBytes, err := json.Marshal(patterns)
	if err != nil {
		return nil, fmt.Errorf("flows: marshal trigger events: %w", err)
	}
	tags := f.Tags
	if tags == nil {
		tags = []string{}
	}
	tagsBytes, err := json.Marshal(tags)
	if err != nil {
		return nil, fmt.Errorf("flows: marshal tags: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var (
		existingVersion  int
		existingCreated  int64
		existingFailures int
		existingLastRun  int64
		existingLastStat string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT version, created_at_ms, consecutive_failures, last_run_at_ms, last_status FROM flows_definitions WHERE id=?`+store.ForUpdate(s.dialect),
		f.ID,
	).Scan(&existingVersion, &existingCreated, &existingFailures, &existingLastRun, &existingLastStat)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if markers := actionSecretMarkers(f.Graph, s.actions); len(markers) > 0 {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errMissingActionSecret
		}
		var previousJSON []byte
		if err := tx.QueryRowContext(ctx, "SELECT graph_json FROM flows_definitions WHERE id=?", f.ID).Scan(&previousJSON); err != nil {
			return nil, err
		}
		var previous Graph
		if err := json.Unmarshal(previousJSON, &previous); err != nil {
			return nil, err
		}
		f.Graph, err = restoreActionSecrets(f.Graph, previous, markers)
		if err != nil {
			return nil, err
		}
	}
	graphBytes, marshalErr := json.Marshal(f.Graph)
	if marshalErr != nil {
		return nil, fmt.Errorf("flows: marshal graph: %w", marshalErr)
	}

	switch {
	case errors.Is(err, sql.ErrNoRows):
		if f.Version <= 0 {
			f.Version = 1
		}
		if f.CreatedAt.IsZero() {
			f.CreatedAt = now
		}
		f.UpdatedAt = now
		if f.Enabled {
			f.DisabledReason = ""
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO flows_definitions(id, name, description, enabled, disabled_reason, version, graph_json, limits_json, trigger_events_json, tags_json, template_id, consecutive_failures, webhook_token_hash, created_at_ms, updated_at_ms, last_run_at_ms, last_status)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?)`,
			f.ID, f.Name, f.Description, store.BoolInt(f.Enabled), f.DisabledReason,
			f.Version, string(graphBytes), string(limitsBytes), string(triggerEventsBytes),
			string(tagsBytes), f.TemplateID, f.ConsecutiveFailures,
			timeToMS(f.CreatedAt), timeToMS(f.UpdatedAt), timeToMS(f.LastRunAt), string(f.LastStatus),
		)
		if err != nil {
			return nil, err
		}
	default:
		f.Version = existingVersion + 1
		f.CreatedAt = msToTime(existingCreated)
		f.UpdatedAt = now
		f.ConsecutiveFailures = existingFailures
		f.LastRunAt = msToTime(existingLastRun)
		f.LastStatus = RunStatus(existingLastStat)
		if f.Enabled {
			f.DisabledReason = ""
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE flows_definitions
			 SET name=?, description=?, enabled=?, disabled_reason=?, version=?, graph_json=?, limits_json=?, trigger_events_json=?, tags_json=?, template_id=?, updated_at_ms=?
			 WHERE id=?`,
			f.Name, f.Description, store.BoolInt(f.Enabled), f.DisabledReason,
			f.Version, string(graphBytes), string(limitsBytes), string(triggerEventsBytes),
			string(tagsBytes), f.TemplateID, timeToMS(f.UpdatedAt), f.ID,
		)
		if err != nil {
			return nil, err
		}
	}

	if err := syncFlowSchedulesTx(ctx, tx, f, now, true); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.notifyChange(f.ID)
	return &f, nil
}

func (s *SQLStore) DeleteFlow(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM flows_definitions WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	s.notifyChange(id)
	return nil
}

func (s *SQLStore) SetFlowEnabled(ctx context.Context, id string, enabled bool, reason string) error {
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var res sql.Result
	if enabled {
		res, err = tx.ExecContext(ctx,
			`UPDATE flows_definitions SET enabled=1, disabled_reason='', consecutive_failures=0, updated_at_ms=? WHERE id=?`,
			timeToMS(now), id,
		)
	} else {
		res, err = tx.ExecContext(ctx,
			`UPDATE flows_definitions SET enabled=0, disabled_reason=?, updated_at_ms=? WHERE id=?`,
			reason, timeToMS(now), id,
		)
	}
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}

	if !enabled {
		if _, err := tx.ExecContext(ctx, `DELETE FROM flows_schedules WHERE flow_id=?`, id); err != nil {
			return err
		}
	} else {
		row := tx.QueryRowContext(ctx, `SELECT `+flowSelectCols+` FROM flows_definitions WHERE id=?`, id)
		f, err := scanFlow(row)
		if err != nil {
			return err
		}
		if err := syncFlowSchedulesTx(ctx, tx, f, now, true); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyChange(id)
	return nil
}

func (s *SQLStore) SetWebhookTokenHash(ctx context.Context, flowID, tokenHash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE flows_definitions SET webhook_token_hash=?, updated_at_ms=? WHERE id=?`,
		tokenHash, timeToMS(s.now()), flowID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) FindFlowByWebhookHash(ctx context.Context, tokenHash string) (*Flow, error) {
	if strings.TrimSpace(tokenHash) == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+flowSelectCols+` FROM flows_definitions WHERE webhook_token_hash=? LIMIT 1`, tokenHash)
	f, err := scanFlow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// RecordFlowOutcome updates last_run_at_ms, last_status, and consecutive_failures
// atomically, tripping auto-disable when consecutive_failures reaches autoDisableAfter.
func (s *SQLStore) RecordFlowOutcome(
	ctx context.Context,
	flowID string,
	status RunStatus,
	finishedAt time.Time,
	autoDisableAfter int,
	errMsg string,
) (streak int, autoDisabled bool, disabledReason string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, "", err
	}
	defer tx.Rollback()

	var enabled int
	var curFailures int
	err = tx.QueryRowContext(ctx,
		`SELECT enabled, consecutive_failures FROM flows_definitions WHERE id=?`+store.ForUpdate(s.dialect),
		flowID,
	).Scan(&enabled, &curFailures)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, "", nil
	}
	if err != nil {
		return 0, false, "", err
	}

	nowMS := timeToMS(finishedAt)
	if nowMS == 0 {
		nowMS = timeToMS(s.now())
	}

	switch status {
	case RunCompleted:
		streak = 0
		_, err = tx.ExecContext(ctx,
			`UPDATE flows_definitions SET consecutive_failures=0, last_run_at_ms=?, last_status=? WHERE id=?`,
			nowMS, string(status), flowID,
		)
	case RunFailed:
		streak = curFailures + 1
		if enabled != 0 && autoDisableAfter > 0 && streak >= autoDisableAfter {
			autoDisabled = true
			disabledReason = fmt.Sprintf("auto-disabled after %d consecutive failures", streak)
			if strings.TrimSpace(errMsg) != "" {
				disabledReason += ": " + errMsg
			}
			_, err = tx.ExecContext(ctx,
				`UPDATE flows_definitions
				 SET enabled=0, disabled_reason=?, consecutive_failures=?, last_run_at_ms=?, last_status=?, updated_at_ms=?
				 WHERE id=?`,
				disabledReason, streak, nowMS, string(status), nowMS, flowID,
			)
			if err == nil {
				_, err = tx.ExecContext(ctx, `DELETE FROM flows_schedules WHERE flow_id=?`, flowID)
			}
		} else {
			_, err = tx.ExecContext(ctx,
				`UPDATE flows_definitions SET consecutive_failures=?, last_run_at_ms=?, last_status=? WHERE id=?`,
				streak, nowMS, string(status), flowID,
			)
		}
	default:
		streak = curFailures
		_, err = tx.ExecContext(ctx,
			`UPDATE flows_definitions SET last_run_at_ms=?, last_status=? WHERE id=?`,
			nowMS, string(status), flowID,
		)
	}
	if err != nil {
		return 0, false, "", err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, "", err
	}
	if autoDisabled {
		s.notifyChange(flowID)
	}
	return streak, autoDisabled, disabledReason, nil
}

// --- Runs ---

const runSelectCols = `r.id, r.flow_id, COALESCE(d.name, ''), r.flow_version, r.trigger_kind, r.event_id, r.event_type, r.correlation_key, r.status, r.attempt, r.dry_run, r.trigger_json, r.context_json, r.error, r.step_count, r.flow_depth, r.started_at_ms, r.updated_at_ms, r.finished_at_ms, r.wake_at_ms, r.lease_owner, r.lease_until_ms`

func scanRun(scanner interface{ Scan(dest ...any) error }) (Run, error) {
	var (
		r          Run
		trigKind   string
		status     string
		dryRun     int
		trigJSON   string
		ctxJSON    string
		startedMS  int64
		updatedMS  int64
		finishedMS int64
		wakeMS     int64
		leaseMS    int64
	)
	err := scanner.Scan(
		&r.ID, &r.FlowID, &r.FlowName, &r.FlowVersion, &trigKind,
		&r.EventID, &r.EventType, &r.CorrelationKey, &status, &r.Attempt, &dryRun,
		&trigJSON, &ctxJSON, &r.Error, &r.StepCount, &r.FlowDepth,
		&startedMS, &updatedMS, &finishedMS, &wakeMS, &r.LeaseOwner, &leaseMS,
	)
	if err != nil {
		return Run{}, err
	}
	r.TriggerKind = NodeKind(trigKind)
	r.Status = RunStatus(status)
	r.DryRun = dryRun != 0
	r.Trigger = json.RawMessage(trigJSON)
	r.Context = json.RawMessage(ctxJSON)
	r.StartedAt = msToTime(startedMS)
	r.UpdatedAt = msToTime(updatedMS)
	r.FinishedAt = msToTime(finishedMS)
	r.WakeAt = msToTime(wakeMS)
	r.LeaseUntil = msToTime(leaseMS)
	return r, nil
}

func (s *SQLStore) prepareRunForInsert(run Run) Run {
	now := s.now().UTC()
	if run.ID == "" {
		run.ID = NewRunID()
	}
	if run.FlowVersion <= 0 {
		run.FlowVersion = 1
	}
	if run.Status == "" {
		run.Status = RunPending
	}
	if run.Attempt <= 0 {
		run.Attempt = 1
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = now
	}
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = run.StartedAt
	}
	return run
}

func insertRunSQL(ignoreConflict bool) string {
	q := `INSERT INTO flows_runs(id, flow_id, flow_version, trigger_kind, event_id, event_type, correlation_key, status, attempt, dry_run, trigger_json, context_json, error, step_count, flow_depth, started_at_ms, updated_at_ms, finished_at_ms, wake_at_ms, lease_owner, lease_until_ms)
	VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if ignoreConflict {
		q += ` ON CONFLICT(id) DO NOTHING`
	}
	return q
}

func runInsertArgs(run Run) []any {
	return []any{
		run.ID, run.FlowID, run.FlowVersion, string(run.TriggerKind),
		run.EventID, run.EventType, run.CorrelationKey, string(run.Status),
		run.Attempt, store.BoolInt(run.DryRun),
		defaultJSON(run.Trigger, "{}"), defaultJSON(run.Context, "{}"),
		run.Error, run.StepCount, run.FlowDepth,
		timeToMS(run.StartedAt), timeToMS(run.UpdatedAt), timeToMS(run.FinishedAt),
		timeToMS(run.WakeAt), run.LeaseOwner, timeToMS(run.LeaseUntil),
	}
}

func (s *SQLStore) CreateRun(ctx context.Context, run Run) error {
	run = s.prepareRunForInsert(run)
	_, err := s.db.ExecContext(ctx, insertRunSQL(false), runInsertArgs(run)...)
	return err
}

// CreateRunIfAbsent inserts run atomically and reports whether a new row was created.
// Duplicate run IDs (e.g. replayed events) return (false, nil).
func (s *SQLStore) CreateRunIfAbsent(ctx context.Context, run Run) (bool, error) {
	run = s.prepareRunForInsert(run)
	res, err := s.db.ExecContext(ctx, insertRunSQL(true), runInsertArgs(run)...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RunExists reports whether a flows_runs row with id already exists.
func (s *SQLStore) RunExists(ctx context.Context, runID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM flows_runs WHERE id=? LIMIT 1`, runID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *SQLStore) UpdateRun(ctx context.Context, run Run) error {
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = s.now().UTC()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE flows_runs
		 SET status=?, attempt=?, trigger_json=?, context_json=?, error=?, step_count=?,
		     updated_at_ms=?, finished_at_ms=?, wake_at_ms=?, lease_owner=?, lease_until_ms=?
		 WHERE id=?`,
		string(run.Status), run.Attempt,
		defaultJSON(run.Trigger, "{}"), defaultJSON(run.Context, "{}"),
		run.Error, run.StepCount,
		timeToMS(run.UpdatedAt), timeToMS(run.FinishedAt), timeToMS(run.WakeAt),
		run.LeaseOwner, timeToMS(run.LeaseUntil), run.ID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) GetRun(ctx context.Context, runID string) (*Run, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runSelectCols+` FROM flows_runs r LEFT JOIN flows_definitions d ON d.id = r.flow_id WHERE r.id=?`,
		runID,
	)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func encodeRunCursor(startedMS int64, id string) string {
	raw := strconv.FormatInt(startedMS, 10) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeRunCursor(cursor string) (int64, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", fmt.Errorf("flows: invalid run cursor: %w", err)
	}
	msStr, id, ok := strings.Cut(string(b), "|")
	if !ok || id == "" {
		return 0, "", fmt.Errorf("flows: malformed run cursor")
	}
	ms, err := strconv.ParseInt(msStr, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("flows: malformed run cursor timestamp: %w", err)
	}
	return ms, id, nil
}

func (s *SQLStore) ListRuns(ctx context.Context, filter RunFilter) ([]Run, string, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	var conds []string
	var args []any
	if filter.FlowID != "" {
		conds = append(conds, "r.flow_id = ?")
		args = append(args, filter.FlowID)
	}
	if filter.Status != "" {
		conds = append(conds, "r.status = ?")
		args = append(args, string(filter.Status))
	}
	if filter.EventType != "" {
		conds = append(conds, "r.event_type = ?")
		args = append(args, filter.EventType)
	}
	if filter.Cursor != "" {
		ms, id, err := decodeRunCursor(filter.Cursor)
		if err != nil {
			return nil, "", err
		}
		conds = append(conds, "(r.started_at_ms < ? OR (r.started_at_ms = ? AND r.id < ?))")
		args = append(args, ms, ms, id)
	}

	query := `SELECT ` + runSelectCols + ` FROM flows_runs r LEFT JOIN flows_definitions d ON d.id = r.flow_id`
	if len(conds) > 0 {
		query += ` WHERE ` + strings.Join(conds, " AND ")
	}
	query += ` ORDER BY r.started_at_ms DESC, r.id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	var nextCursor string
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		nextCursor = encodeRunCursor(timeToMS(last.StartedAt), last.ID)
	}
	return out, nextCursor, nil
}

// RenewRunLease extends the lease of a running run owned by owner.
func (s *SQLStore) RenewRunLease(ctx context.Context, runID, owner string, until time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE flows_runs SET lease_until_ms=?, updated_at_ms=? WHERE id=? AND status='running' AND lease_owner=?`,
		timeToMS(until), timeToMS(s.now()), runID, owner,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// CancelRun transitions an active run to cancelled and cancels any waiting waiters.
func (s *SQLStore) CancelRun(ctx context.Context, runID string, now time.Time) error {
	if now.IsZero() {
		now = s.now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM flows_runs WHERE id=?`+store.ForUpdate(s.dialect), runID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if RunStatus(status).Terminal() {
		return ErrRunNotActive
	}

	nowMS := timeToMS(now)
	if _, err := tx.ExecContext(ctx,
		`UPDATE flows_runs SET status='cancelled', updated_at_ms=?, finished_at_ms=?, wake_at_ms=0, lease_owner='', lease_until_ms=0 WHERE id=?`,
		nowMS, nowMS, runID,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE flows_waiters SET status='cancelled' WHERE run_id=? AND status='waiting'`,
		runID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// CancelFlowActiveRuns cancels all non-terminal runs and active waiters for flowID.
func (s *SQLStore) CancelFlowActiveRuns(ctx context.Context, flowID string, now time.Time) (int64, error) {
	if now.IsZero() {
		now = s.now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	nowMS := timeToMS(now)
	res, err := tx.ExecContext(ctx,
		`UPDATE flows_runs SET status='cancelled', updated_at_ms=?, finished_at_ms=?, wake_at_ms=0, lease_owner='', lease_until_ms=0
		 WHERE flow_id=? AND status IN ('pending', 'running', 'waiting')`,
		nowMS, nowMS, flowID,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE flows_waiters SET status='cancelled' WHERE flow_id=? AND status='waiting'`,
		flowID,
	); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// ClaimDueRuns atomically leases up to limit due runs (pending, delay-due waiting,
// or lease-expired running), respecting each flow's concurrency and orderedByKey limits.
func (s *SQLStore) ClaimDueRuns(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]Run, error) {
	if now.IsZero() {
		now = s.now().UTC()
	}
	if lease <= 0 {
		lease = 60 * time.Second
	}
	if limit <= 0 {
		limit = 32
	}
	if limit > 256 {
		limit = 256
	}

	nowMS := timeToMS(now)
	leaseUntil := now.UTC().Add(lease)
	leaseUntilMS := timeToMS(leaseUntil)
	scanLimit := min(limit*4, 512)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	lockSuffix := ""
	if s.dialect == store.Postgres {
		lockSuffix = " FOR UPDATE SKIP LOCKED"
	}

	// Single-table SELECT so PostgreSQL FOR UPDATE SKIP LOCKED never conflicts with a LEFT JOIN.
	rows, err := tx.QueryContext(ctx,
		`SELECT id, flow_id, '', flow_version, trigger_kind, event_id, event_type, correlation_key,
		        status, attempt, dry_run, trigger_json, context_json, error, step_count, flow_depth,
		        started_at_ms, updated_at_ms, finished_at_ms, wake_at_ms, lease_owner, lease_until_ms
		 FROM flows_runs
		 WHERE (status = 'pending' AND wake_at_ms <= ?)
		    OR (status = 'waiting' AND wake_at_ms > 0 AND wake_at_ms <= ?)
		    OR (status = 'running' AND lease_until_ms <= ?)
		 ORDER BY started_at_ms ASC, id ASC
		 LIMIT ?`+lockSuffix,
		nowMS, nowMS, nowMS, scanLimit,
	)
	if err != nil {
		return nil, err
	}
	var candidates []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	type flowMeta struct {
		name         string
		limits       FlowLimits
		runningCount int
	}
	metaCache := make(map[string]*flowMeta)
	getMeta := func(flowID string) (*flowMeta, error) {
		if m, ok := metaCache[flowID]; ok {
			return m, nil
		}
		var name, limJSON string
		err := tx.QueryRowContext(ctx, `SELECT name, limits_json FROM flows_definitions WHERE id=?`, flowID).Scan(&name, &limJSON)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		var lim FlowLimits
		if strings.TrimSpace(limJSON) != "" {
			_ = json.Unmarshal([]byte(limJSON), &lim)
		}
		var runningCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM flows_runs WHERE flow_id=? AND status='running' AND lease_until_ms > ?`,
			flowID, nowMS,
		).Scan(&runningCount); err != nil {
			return nil, err
		}
		m := &flowMeta{name: name, limits: lim.Effective(), runningCount: runningCount}
		metaCache[flowID] = m
		return m, nil
	}

	claimedKeys := make(map[string]bool)
	claimed := make([]Run, 0, min(limit, len(candidates)))
	for _, r := range candidates {
		if len(claimed) >= limit {
			break
		}
		meta, err := getMeta(r.FlowID)
		if err != nil {
			return nil, err
		}
		if meta.runningCount >= meta.limits.Concurrency {
			continue
		}
		if meta.limits.OrderedByKey && r.CorrelationKey != "" {
			key := r.FlowID + "\x00" + r.CorrelationKey
			if claimedKeys[key] {
				continue
			}
			var earlier int
			err := tx.QueryRowContext(ctx,
				`SELECT 1 FROM flows_runs
				 WHERE flow_id=? AND correlation_key=? AND status IN ('pending', 'running', 'waiting')
				   AND (started_at_ms < ? OR (started_at_ms = ? AND id < ?))
				 LIMIT 1`,
				r.FlowID, r.CorrelationKey, timeToMS(r.StartedAt), timeToMS(r.StartedAt), r.ID,
			).Scan(&earlier)
			if err == nil {
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}

		newAttempt := r.Attempt
		if r.Status == RunRunning {
			// Lease expired on a running run -> crash recovery increments attempt.
			newAttempt++
		}

		res, err := tx.ExecContext(ctx,
			`UPDATE flows_runs
			 SET status='running', attempt=?, lease_owner=?, lease_until_ms=?, wake_at_ms=0, updated_at_ms=?
			 WHERE id=? AND (
			       (status = 'pending' AND wake_at_ms <= ?)
			    OR (status = 'waiting' AND wake_at_ms > 0 AND wake_at_ms <= ?)
			    OR (status = 'running' AND lease_until_ms <= ?)
			 )`,
			newAttempt, owner, leaseUntilMS, nowMS, r.ID, nowMS, nowMS, nowMS,
		)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}

		r.FlowName = meta.name
		r.Status = RunRunning
		r.Attempt = newAttempt
		r.LeaseOwner = owner
		r.LeaseUntil = leaseUntil
		r.WakeAt = time.Time{}
		r.UpdatedAt = now.UTC()
		meta.runningCount++
		if meta.limits.OrderedByKey && r.CorrelationKey != "" {
			claimedKeys[r.FlowID+"\x00"+r.CorrelationKey] = true
		}
		claimed = append(claimed, r)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

// --- Steps ---

func (s *SQLStore) RecordStep(ctx context.Context, step RunStep) (int64, error) {
	if step.Status == "" {
		step.Status = StepPending
	}
	if step.StartedAt.IsZero() {
		step.StartedAt = s.now().UTC()
	}
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO flows_run_steps(run_id, seq, node_id, node_kind, status, idempotency_key, branch, input_json, output_json, error, started_at_ms, finished_at_ms, duration_ms)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		step.RunID, step.Seq, step.NodeID, string(step.NodeKind), string(step.Status),
		step.IdempotencyKey, step.Branch,
		defaultJSON(step.Input, "{}"), defaultJSON(step.Output, "{}"),
		step.Error, timeToMS(step.StartedAt), timeToMS(step.FinishedAt), step.DurationMS,
	).Scan(&id)
	return id, err
}

func (s *SQLStore) UpdateStep(ctx context.Context, step RunStep) error {
	var (
		res sql.Result
		err error
	)
	if step.ID > 0 {
		res, err = s.db.ExecContext(ctx,
			`UPDATE flows_run_steps
			 SET status=?, idempotency_key=?, branch=?, input_json=?, output_json=?, error=?, finished_at_ms=?, duration_ms=?
			 WHERE id=?`,
			string(step.Status), step.IdempotencyKey, step.Branch,
			defaultJSON(step.Input, "{}"), defaultJSON(step.Output, "{}"),
			step.Error, timeToMS(step.FinishedAt), step.DurationMS, step.ID,
		)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE flows_run_steps
			 SET status=?, idempotency_key=?, branch=?, input_json=?, output_json=?, error=?, finished_at_ms=?, duration_ms=?
			 WHERE run_id=? AND seq=?`,
			string(step.Status), step.IdempotencyKey, step.Branch,
			defaultJSON(step.Input, "{}"), defaultJSON(step.Output, "{}"),
			step.Error, timeToMS(step.FinishedAt), step.DurationMS, step.RunID, step.Seq,
		)
	}
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) ListRunSteps(ctx context.Context, runID string) ([]RunStep, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, seq, node_id, node_kind, status, idempotency_key, branch, input_json, output_json, error, started_at_ms, finished_at_ms, duration_ms
		 FROM flows_run_steps
		 WHERE run_id=?
		 ORDER BY seq ASC, id ASC`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunStep
	for rows.Next() {
		var (
			st         RunStep
			nodeKind   string
			status     string
			inJSON     string
			outJSON    string
			startedMS  int64
			finishedMS int64
		)
		if err := rows.Scan(
			&st.ID, &st.RunID, &st.Seq, &st.NodeID, &nodeKind, &status,
			&st.IdempotencyKey, &st.Branch, &inJSON, &outJSON, &st.Error,
			&startedMS, &finishedMS, &st.DurationMS,
		); err != nil {
			return nil, err
		}
		st.NodeKind = NodeKind(nodeKind)
		st.Status = StepStatus(status)
		st.Input = json.RawMessage(inJSON)
		st.Output = json.RawMessage(outJSON)
		st.StartedAt = msToTime(startedMS)
		st.FinishedAt = msToTime(finishedMS)
		out = append(out, st)
	}
	return out, rows.Err()
}

// CompleteStepAndAdvanceRun updates (or inserts if step.ID == 0) a step row,
// optionally inserts waiters, and updates the run row in one atomic transaction.
func (s *SQLStore) CompleteStepAndAdvanceRun(ctx context.Context, step *RunStep, run Run, waiters []Waiter) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if step != nil {
		if step.StartedAt.IsZero() {
			step.StartedAt = s.now().UTC()
		}
		if step.ID == 0 {
			err = tx.QueryRowContext(ctx,
				`INSERT INTO flows_run_steps(run_id, seq, node_id, node_kind, status, idempotency_key, branch, input_json, output_json, error, started_at_ms, finished_at_ms, duration_ms)
				 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
				step.RunID, step.Seq, step.NodeID, string(step.NodeKind), string(step.Status),
				step.IdempotencyKey, step.Branch,
				defaultJSON(step.Input, "{}"), defaultJSON(step.Output, "{}"),
				step.Error, timeToMS(step.StartedAt), timeToMS(step.FinishedAt), step.DurationMS,
			).Scan(&step.ID)
			if err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx,
				`UPDATE flows_run_steps
				 SET status=?, idempotency_key=?, branch=?, input_json=?, output_json=?, error=?, finished_at_ms=?, duration_ms=?
				 WHERE id=?`,
				string(step.Status), step.IdempotencyKey, step.Branch,
				defaultJSON(step.Input, "{}"), defaultJSON(step.Output, "{}"),
				step.Error, timeToMS(step.FinishedAt), step.DurationMS, step.ID,
			); err != nil {
				return err
			}
		}
	}

	for _, w := range waiters {
		if w.ID == "" {
			w.ID = NewWaiterID()
		}
		if w.Status == "" {
			w.Status = "waiting"
		}
		if w.CreatedAt.IsZero() {
			w.CreatedAt = s.now().UTC()
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO flows_waiters(id, run_id, flow_id, node_id, event_type, correlation_key, filter_json, timeout_at_ms, created_at_ms, status)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			w.ID, w.RunID, w.FlowID, w.NodeID, w.EventType, w.CorrelationKey,
			strings.TrimSpace(string(w.Filter)), timeToMS(w.TimeoutAt), timeToMS(w.CreatedAt), w.Status,
		); err != nil {
			return err
		}
	}

	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = s.now().UTC()
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE flows_runs
		 SET status=?, attempt=?, trigger_json=?, context_json=?, error=?, step_count=?,
		     updated_at_ms=?, finished_at_ms=?, wake_at_ms=?, lease_owner=?, lease_until_ms=?
		 WHERE id=?`,
		string(run.Status), run.Attempt,
		defaultJSON(run.Trigger, "{}"), defaultJSON(run.Context, "{}"),
		run.Error, run.StepCount,
		timeToMS(run.UpdatedAt), timeToMS(run.FinishedAt), timeToMS(run.WakeAt),
		run.LeaseOwner, timeToMS(run.LeaseUntil), run.ID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// --- Waiters ---

func scanWaiter(scanner interface{ Scan(dest ...any) error }) (Waiter, error) {
	var (
		w         Waiter
		filterStr string
		timeoutMS int64
		createdMS int64
	)
	err := scanner.Scan(
		&w.ID, &w.RunID, &w.FlowID, &w.NodeID, &w.EventType,
		&w.CorrelationKey, &filterStr, &timeoutMS, &createdMS, &w.Status,
	)
	if err != nil {
		return Waiter{}, err
	}
	if strings.TrimSpace(filterStr) != "" {
		w.Filter = json.RawMessage(filterStr)
	}
	w.TimeoutAt = msToTime(timeoutMS)
	w.CreatedAt = msToTime(createdMS)
	return w, nil
}

func (s *SQLStore) CreateWaiter(ctx context.Context, w Waiter) error {
	if w.ID == "" {
		w.ID = NewWaiterID()
	}
	if w.Status == "" {
		w.Status = "waiting"
	}
	if w.CreatedAt.IsZero() {
		w.CreatedAt = s.now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO flows_waiters(id, run_id, flow_id, node_id, event_type, correlation_key, filter_json, timeout_at_ms, created_at_ms, status)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.RunID, w.FlowID, w.NodeID, w.EventType, w.CorrelationKey,
		strings.TrimSpace(string(w.Filter)), timeToMS(w.TimeoutAt), timeToMS(w.CreatedAt), w.Status,
	)
	return err
}

// MatchWaiters returns active, non-expired waiters matching eventType (exact or
// wildcard pattern) and correlationKey (or empty waiter correlationKey).
func (s *SQLStore) MatchWaiters(ctx context.Context, eventType, correlationKey string, now time.Time) ([]Waiter, error) {
	if now.IsZero() {
		now = s.now().UTC()
	}
	nowMS := timeToMS(now)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, flow_id, node_id, event_type, correlation_key, filter_json, timeout_at_ms, created_at_ms, status
		 FROM flows_waiters
		 WHERE status = 'waiting'
		   AND timeout_at_ms > ?
		   AND (correlation_key = '' OR correlation_key = ?)
		   AND (event_type = ? OR event_type = '*' OR event_type LIKE '%*' OR event_type LIKE '%.' OR event_type LIKE '%/')
		 ORDER BY created_at_ms ASC, id ASC`,
		nowMS, correlationKey, eventType,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Waiter
	for rows.Next() {
		w, err := scanWaiter(rows)
		if err != nil {
			return nil, err
		}
		if events.MatchPattern(w.EventType, eventType) {
			out = append(out, w)
		}
	}
	return out, rows.Err()
}

// ExpireWaiters atomically marks up to limit timed-out waiters as timed_out
// (cancelling sibling waiters for the same run/node so each wait_event times
// out once) and returns the expired rows.
func (s *SQLStore) ExpireWaiters(ctx context.Context, now time.Time, limit int) ([]Waiter, error) {
	if now.IsZero() {
		now = s.now().UTC()
	}
	if limit <= 0 {
		limit = 64
	}
	nowMS := timeToMS(now)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, run_id, flow_id, node_id, event_type, correlation_key, filter_json, timeout_at_ms, created_at_ms, status
		 FROM flows_waiters
		 WHERE status = 'waiting' AND timeout_at_ms <= ?
		 ORDER BY timeout_at_ms ASC, id ASC
		 LIMIT ?`+store.ForUpdate(s.dialect),
		nowMS, limit,
	)
	if err != nil {
		return nil, err
	}
	var due []Waiter
	for rows.Next() {
		w, err := scanWaiter(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(due) == 0 {
		return nil, nil
	}

	seenRunNode := make(map[string]bool, len(due))
	var expired []Waiter
	for _, w := range due {
		key := w.RunID + "\x00" + w.NodeID
		if seenRunNode[key] {
			continue
		}
		res, err := tx.ExecContext(ctx, `UPDATE flows_waiters SET status='timed_out' WHERE id=? AND status='waiting'`, w.ID)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE flows_waiters SET status='cancelled' WHERE run_id=? AND node_id=? AND id<>? AND status='waiting'`,
			w.RunID, w.NodeID, w.ID,
		); err != nil {
			return nil, err
		}
		seenRunNode[key] = true
		w.Status = "timed_out"
		expired = append(expired, w)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return expired, nil
}

// WaiterMatchCommit describes an atomic waiter match + run wake transition.
type WaiterMatchCommit struct {
	WaiterID      string
	RunID         string
	NodeID        string
	MutateContext func(raw json.RawMessage) (json.RawMessage, error)
}

// ResolveWaiterAndWakeRun atomically marks a waiting waiter as matched, cancels
// sibling waiters for (runID, nodeID), updates the waiting run's context, and
// transitions the run to pending so a worker claims it immediately.
func (s *SQLStore) ResolveWaiterAndWakeRun(ctx context.Context, commit WaiterMatchCommit, now time.Time) (bool, error) {
	if now.IsZero() {
		now = s.now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	matched, err := resolveWaiterInTx(ctx, tx, s.dialect, commit, now)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return matched, nil
}

func resolveWaiterInTx(ctx context.Context, tx *sql.Tx, dialect store.Dialect, commit WaiterMatchCommit, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE flows_waiters SET status='matched' WHERE id=? AND status='waiting'`, commit.WaiterID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE flows_waiters SET status='cancelled' WHERE run_id=? AND node_id=? AND id<>? AND status='waiting'`,
		commit.RunID, commit.NodeID, commit.WaiterID,
	); err != nil {
		return false, err
	}
	woken, err := wakeWaitingRunInTx(ctx, tx, dialect, commit.RunID, commit.MutateContext, now)
	if err != nil {
		return false, err
	}
	return woken, nil
}

// WakeRunAfterTimeout transitions a waiting run to pending with a timeout wait result.
func (s *SQLStore) WakeRunAfterTimeout(ctx context.Context, runID string, mutate func(raw json.RawMessage) (json.RawMessage, error), now time.Time) (bool, error) {
	if now.IsZero() {
		now = s.now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	ok, err := wakeWaitingRunInTx(ctx, tx, s.dialect, runID, mutate, now)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return ok, nil
}

func wakeWaitingRunInTx(ctx context.Context, tx *sql.Tx, dialect store.Dialect, runID string, mutate func(raw json.RawMessage) (json.RawMessage, error), now time.Time) (bool, error) {
	var ctxJSON string
	err := tx.QueryRowContext(ctx,
		`SELECT context_json FROM flows_runs WHERE id=? AND status='waiting'`+store.ForUpdate(dialect),
		runID,
	).Scan(&ctxJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	updatedCtx := json.RawMessage(ctxJSON)
	if mutate != nil {
		updatedCtx, err = mutate(updatedCtx)
		if err != nil {
			return false, err
		}
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE flows_runs SET status='pending', context_json=?, wake_at_ms=0, updated_at_ms=? WHERE id=? AND status='waiting'`,
		defaultJSON(updatedCtx, "{}"), timeToMS(now), runID,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ActiveWaiterEventPatterns returns distinct event patterns from active waiting rows.
func (s *SQLStore) ActiveWaiterEventPatterns(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT event_type FROM flows_waiters WHERE status='waiting' ORDER BY event_type ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		if p != "" {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// CommitOutboxEvent atomically inserts triggered runs (idempotently), resolves
// matched waiters, and marks the events_outbox row acknowledged in one transaction.
func (s *SQLStore) CommitOutboxEvent(ctx context.Context, outboxID int64, ackedAt time.Time, runs []Run, waiters []WaiterMatchCommit) (runsCreated int, waitersMatched int, err error) {
	if ackedAt.IsZero() {
		ackedAt = s.now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	for _, run := range runs {
		run = s.prepareRunForInsert(run)
		res, err := tx.ExecContext(ctx, insertRunSQL(true), runInsertArgs(run)...)
		if err != nil {
			return 0, 0, err
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 && !run.Status.Terminal() {
			runsCreated++
		}
	}

	for _, w := range waiters {
		ok, err := resolveWaiterInTx(ctx, tx, s.dialect, w, ackedAt)
		if err != nil {
			return 0, 0, err
		}
		if ok {
			waitersMatched++
		}
	}

	if outboxID > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE events_outbox SET acked_at_ms=? WHERE id=? AND acked_at_ms=0`,
			timeToMS(ackedAt), outboxID,
		); err != nil {
			return 0, 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return runsCreated, waitersMatched, nil
}

// --- Sliding window counters ---

func (s *SQLStore) RecordWindowEvent(ctx context.Context, flowID, nodeID, groupKey string, at time.Time, window time.Duration, resetAt int) (count int, reached bool, err error) {
	if at.IsZero() {
		at = s.now().UTC()
	}
	atMS := timeToMS(at)
	cutoffMS := timeToMS(at.UTC().Add(-window))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM flows_counters WHERE flow_id=? AND node_id=? AND group_key=? AND at_ms < ?`,
		flowID, nodeID, groupKey, cutoffMS,
	); err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO flows_counters(flow_id, node_id, group_key, at_ms) VALUES(?, ?, ?, ?)`,
		flowID, nodeID, groupKey, atMS,
	); err != nil {
		return 0, false, err
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM flows_counters WHERE flow_id=? AND node_id=? AND group_key=? AND at_ms >= ?`,
		flowID, nodeID, groupKey, cutoffMS,
	).Scan(&count); err != nil {
		return 0, false, err
	}

	if resetAt > 0 && count >= resetAt {
		reached = true
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM flows_counters WHERE flow_id=? AND node_id=? AND group_key=?`,
			flowID, nodeID, groupKey,
		); err != nil {
			return 0, false, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return count, reached, nil
}

// --- Dedupe / throttle ---

func (s *SQLStore) CheckDedupe(ctx context.Context, flowID, nodeID, dedupeKey string, at time.Time, cooldown time.Duration, maxPerWindow int) (allowed bool, hits int, err error) {
	if at.IsZero() {
		at = s.now().UTC()
	}
	if maxPerWindow <= 0 {
		maxPerWindow = 1
	}
	atMS := timeToMS(at)
	expiresMS := timeToMS(at.UTC().Add(cooldown))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()

	var curHits int
	var curExpiresMS int64
	err = tx.QueryRowContext(ctx,
		`SELECT hits, expires_at_ms FROM flows_dedupe WHERE flow_id=? AND node_id=? AND dedupe_key=?`+store.ForUpdate(s.dialect),
		flowID, nodeID, dedupeKey,
	).Scan(&curHits, &curExpiresMS)

	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && curExpiresMS <= atMS):
		hits = 1
		allowed = hits <= maxPerWindow
		_, err = tx.ExecContext(ctx,
			`INSERT INTO flows_dedupe(flow_id, node_id, dedupe_key, hits, first_at_ms, last_at_ms, expires_at_ms)
			 VALUES(?, ?, ?, 1, ?, ?, ?)
			 ON CONFLICT(flow_id, node_id, dedupe_key) DO UPDATE SET
			     hits=1, first_at_ms=excluded.first_at_ms, last_at_ms=excluded.last_at_ms, expires_at_ms=excluded.expires_at_ms`,
			flowID, nodeID, dedupeKey, atMS, atMS, expiresMS,
		)
	case err != nil:
		return false, 0, err
	default:
		hits = curHits + 1
		allowed = hits <= maxPerWindow
		_, err = tx.ExecContext(ctx,
			`UPDATE flows_dedupe SET hits=?, last_at_ms=? WHERE flow_id=? AND node_id=? AND dedupe_key=?`,
			hits, atMS, flowID, nodeID, dedupeKey,
		)
	}
	if err != nil {
		return false, 0, err
	}
	if err := tx.Commit(); err != nil {
		return false, 0, err
	}
	return allowed, hits, nil
}

// --- Schedules ---

// SyncFlowSchedules reconciles flows_schedules rows for f against f.Graph.
func (s *SQLStore) SyncFlowSchedules(ctx context.Context, f Flow, now time.Time) error {
	if now.IsZero() {
		now = s.now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := syncFlowSchedulesTx(ctx, tx, f, now, false); err != nil {
		return err
	}
	return tx.Commit()
}

func syncFlowSchedulesTx(ctx context.Context, tx *sql.Tx, f Flow, now time.Time, rebase bool) error {
	if !f.Enabled {
		_, err := tx.ExecContext(ctx, `DELETE FROM flows_schedules WHERE flow_id=?`, f.ID)
		return err
	}
	desired := make(map[string]ScheduleTriggerConfig)
	for _, n := range f.Graph.Nodes {
		if n.Disabled || n.Kind != NodeTriggerSchedule {
			continue
		}
		cfg, err := DecodeConfig[ScheduleTriggerConfig](n)
		if err != nil {
			continue
		}
		desired[n.ID] = cfg
	}

	// Read existing schedule rows for this flow first (close rows before writes for SQLite).
	rows, err := tx.QueryContext(ctx, `SELECT node_id, next_run_ms, last_run_ms FROM flows_schedules WHERE flow_id=?`, f.ID)
	if err != nil {
		return err
	}
	existing := make(map[string]ScheduleEntry)
	for rows.Next() {
		var nodeID string
		var nextMS, lastMS int64
		if err := rows.Scan(&nodeID, &nextMS, &lastMS); err != nil {
			rows.Close()
			return err
		}
		existing[nodeID] = ScheduleEntry{FlowID: f.ID, NodeID: nodeID, NextRun: msToTime(nextMS), LastRun: msToTime(lastMS)}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Remove schedule rows whose node was removed or disabled.
	for nodeID := range existing {
		if _, ok := desired[nodeID]; !ok {
			if _, err := tx.ExecContext(ctx, `DELETE FROM flows_schedules WHERE flow_id=? AND node_id=?`, f.ID, nodeID); err != nil {
				return err
			}
		}
	}

	// Seed or rebase next_run_ms for each active schedule trigger node.
	for nodeID, cfg := range desired {
		nextTime, err := NextScheduleTime(cfg, now)
		if err != nil {
			continue
		}
		cur, exists := existing[nodeID]
		if rebase || !exists || cur.NextRun.IsZero() || cur.LastRun.After(now) {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO flows_schedules(flow_id, node_id, next_run_ms, last_run_ms)
				 VALUES(?, ?, ?, ?)
				 ON CONFLICT(flow_id, node_id) DO UPDATE SET next_run_ms=excluded.next_run_ms`,
				f.ID, nodeID, timeToMS(nextTime), timeToMS(cur.LastRun),
			); err != nil {
				return err
			}
		}
	}
	return nil
}

// ListDueSchedules returns enabled schedule entries whose next_run_ms <= now.
func (s *SQLStore) ListDueSchedules(ctx context.Context, now time.Time, limit int) ([]ScheduleEntry, error) {
	if now.IsZero() {
		now = s.now().UTC()
	}
	if limit <= 0 {
		limit = 64
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.flow_id, s.node_id, s.next_run_ms, s.last_run_ms
		 FROM flows_schedules s
		 JOIN flows_definitions d ON d.id = s.flow_id
		 WHERE d.enabled = 1 AND s.next_run_ms > 0 AND s.next_run_ms <= ?
		 ORDER BY s.next_run_ms ASC, s.flow_id ASC, s.node_id ASC
		 LIMIT ?`,
		timeToMS(now), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduleEntry
	for rows.Next() {
		var e ScheduleEntry
		var nextMS, lastMS int64
		if err := rows.Scan(&e.FlowID, &e.NodeID, &nextMS, &lastMS); err != nil {
			return nil, err
		}
		e.NextRun = msToTime(nextMS)
		e.LastRun = msToTime(lastMS)
		out = append(out, e)
	}
	return out, rows.Err()
}

// AdvanceScheduleAndCreateRun atomically advances flows_schedules and inserts
// the scheduled run (if not already inserted for that slot).
func (s *SQLStore) AdvanceScheduleAndCreateRun(ctx context.Context, flowID, nodeID string, expectedNext, lastRun, nextRun time.Time, run *Run) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE flows_schedules SET next_run_ms=?, last_run_ms=? WHERE flow_id=? AND node_id=? AND next_run_ms=?`,
		timeToMS(nextRun), timeToMS(lastRun), flowID, nodeID, timeToMS(expectedNext),
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}

	created := false
	if run != nil {
		r := s.prepareRunForInsert(*run)
		rRes, err := tx.ExecContext(ctx, insertRunSQL(true), runInsertArgs(r)...)
		if err != nil {
			return false, err
		}
		if rn, err := rRes.RowsAffected(); err == nil && rn > 0 {
			created = true
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return created, nil
}

// --- Retention pruning ---

func (s *SQLStore) PruneHistory(ctx context.Context, olderThan time.Time, maxRuns int) (int64, error) {
	var total int64
	if !olderThan.IsZero() {
		cutoffMS := timeToMS(olderThan)
		n, err := store.PruneBatched(ctx, s.db,
			`DELETE FROM flows_runs WHERE id IN (
				SELECT id FROM flows_runs
				WHERE status IN ('completed', 'failed', 'cancelled', 'throttled')
				  AND ((finished_at_ms > 0 AND finished_at_ms < ?) OR (finished_at_ms = 0 AND updated_at_ms < ?))
				ORDER BY started_at_ms ASC, id ASC
				LIMIT ?
			)`,
			cutoffMS, cutoffMS,
		)
		total += n
		if err != nil {
			return total, err
		}
	}

	if maxRuns > 0 {
		var cutoffMS int64
		var cutoffID string
		err := s.db.QueryRowContext(ctx,
			`SELECT started_at_ms, id FROM flows_runs
			 WHERE status IN ('completed', 'failed', 'cancelled', 'throttled')
			 ORDER BY started_at_ms DESC, id DESC
			 LIMIT 1 OFFSET ?`,
			maxRuns-1,
		).Scan(&cutoffMS, &cutoffID)
		if err == nil {
			n, err := store.PruneBatched(ctx, s.db,
				`DELETE FROM flows_runs WHERE id IN (
					SELECT id FROM flows_runs
					WHERE status IN ('completed', 'failed', 'cancelled', 'throttled')
					  AND (started_at_ms < ? OR (started_at_ms = ? AND id < ?))
					ORDER BY started_at_ms ASC, id ASC
					LIMIT ?
				)`,
				cutoffMS, cutoffMS, cutoffID,
			)
			total += n
			if err != nil {
				return total, err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return total, err
		}
	}

	// Best-effort cleanup of expired dedupe entries, stale window counters, and old terminal waiters.
	now := s.now().UTC()
	_, _ = store.PruneBatched(ctx, s.db,
		`DELETE FROM flows_dedupe WHERE (flow_id, node_id, dedupe_key) IN (
			SELECT flow_id, node_id, dedupe_key FROM flows_dedupe WHERE expires_at_ms < ? ORDER BY expires_at_ms ASC LIMIT ?
		)`,
		timeToMS(now),
	)
	_, _ = store.PruneBatched(ctx, s.db,
		`DELETE FROM flows_counters WHERE id IN (
			SELECT id FROM flows_counters WHERE at_ms < ? ORDER BY at_ms ASC LIMIT ?
		)`,
		timeToMS(now.Add(-MaxWindowDuration)),
	)
	if !olderThan.IsZero() {
		_, _ = store.PruneBatched(ctx, s.db,
			`DELETE FROM flows_waiters WHERE id IN (
				SELECT id FROM flows_waiters WHERE status <> 'waiting' AND created_at_ms < ? ORDER BY created_at_ms ASC LIMIT ?
			)`,
			timeToMS(olderThan),
		)
	}

	return total, nil
}
