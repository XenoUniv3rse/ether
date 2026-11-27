package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/flows"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"github.com/braedonsaunders/ether/internal/llm"
	"github.com/braedonsaunders/ether/internal/notify"
)

// FlowLLM is the configured chat model backing POST /api/flows/describe.
// *llm.Client satisfies it.
type FlowLLM interface {
	Enabled() bool
	Chat(ctx context.Context, msgs []llm.Message, jsonMode bool) (string, error)
}

var _ FlowLLM = (*llm.Client)(nil)

// FlowsDeps wires the Flows console API (docs/design/flows.md §7) to the
// durable store and runtime provided by internal/flows. Every field is
// optional: a nil Store or Runtime answers the routes that need it with
// 503, while validation, catalogues and templates keep working from the
// registries alone. Handlers never sit on the play path; triggers hand
// work to the Runtime, which executes asynchronously.
type FlowsDeps struct {
	// Store persists flow definitions, runs and steps.
	Store flows.Store
	// Runtime executes manual, webhook and test runs and cancels runs.
	Runtime flows.Runtime
	// Events is the live event schema registry (catalogue + validation).
	Events *events.Registry
	// Actions is the live action registry (catalogue + validation).
	Actions flows.ActionRegistry
	// Triggers lists trigger node kinds for the editor palette. Nil uses
	// the built-in trigger specs.
	Triggers flows.TriggerRegistry
	// Templates overrides the starter template gallery. Nil uses
	// DefaultFlowTemplates.
	Templates []flows.Template
	// StreamHub receives flows.changed / flows.run SSE events and backs
	// GET /api/flows/stream. Nil disables live updates.
	StreamHub *StreamHub
	// PublishStream, when set, replaces direct StreamHub publishing (the
	// app routes it through the cluster console stream so events reach
	// consoles connected to other nodes).
	PublishStream func(ctx context.Context, typ string, data any) error
	// NotifyChannels lists the owner's notification channels and
	// SaveNotifyChannel persists one; both back "Customize as Flow".
	// Nil answers that route 503.
	NotifyChannels    func(ctx context.Context) ([]notify.Channel, error)
	SaveNotifyChannel func(ctx context.Context, ch notify.Channel) error
	// LLM is the configured chat model backing POST /api/flows/describe.
	// Nil or disabled answers 409 ai_unavailable.
	LLM FlowLLM
	// DescribeTimeout bounds POST /api/flows/describe (including any single
	// validation repair pass). Zero defaults to 45 seconds.
	DescribeTimeout time.Duration
	// WebhookHMACSecret optionally resolves a per-flow HMAC secret for
	// incoming webhook signature checks. Nil keys HMAC by the token.
	WebhookHMACSecret func(ctx context.Context, flowID string) (string, bool)
	// WebhookMaxBodyBytes caps incoming webhook bodies. Default 256 KiB.
	WebhookMaxBodyBytes int64
	// WebhookRatePerIP and WebhookRatePerToken bound incoming webhook
	// requests per minute. Zero takes the package defaults.
	WebhookRatePerIP    int
	WebhookRatePerToken int
	// TrustedProxyCIDRs resolves trusted reverse-proxy ranges used to
	// derive the client IP for webhook rate limiting.
	TrustedProxyCIDRs func() []string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Now overrides the clock (tests). Nil uses time.Now.
	Now func() time.Time
	// NewFlowID overrides flow ID generation (tests).
	NewFlowID func() string
}

// FlowsAPI serves the owner-authenticated Flows console API and the
// public incoming-webhook ingress. Construct it with NewFlowsAPI.
type FlowsAPI struct {
	deps                FlowsDeps
	log                 *slog.Logger
	ipLimiter           *flowRateLimiter
	tokenLimiter        *flowRateLimiter
	webhookMaxBody      int64
	webhookRatePerIP    int
	webhookRatePerToken int
}

// NewFlowsAPI builds a FlowsAPI from deps, applying defaults.
func NewFlowsAPI(deps FlowsDeps) *FlowsAPI {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	maxBody := deps.WebhookMaxBodyBytes
	if maxBody <= 0 {
		maxBody = MaxWebhookBodyBytes
	}
	perIP := deps.WebhookRatePerIP
	if perIP <= 0 {
		perIP = defaultWebhookRatePerIP
	}
	perTok := deps.WebhookRatePerToken
	if perTok <= 0 {
		perTok = defaultWebhookRatePerToken
	}
	return &FlowsAPI{
		deps:                deps,
		log:                 log,
		ipLimiter:           newFlowRateLimiter(webhookRateWindow),
		tokenLimiter:        newFlowRateLimiter(webhookRateWindow),
		webhookMaxBody:      maxBody,
		webhookRatePerIP:    perIP,
		webhookRatePerToken: perTok,
	}
}

func (a *FlowsAPI) now() time.Time {
	if a.deps.Now != nil {
		return a.deps.Now()
	}
	return time.Now()
}

func (a *FlowsAPI) newFlowID() string {
	if a.deps.NewFlowID != nil {
		return a.deps.NewFlowID()
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("flow_%d", a.now().UnixNano())
	}
	return "flow_" + hex.EncodeToString(buf[:])
}

func (a *FlowsAPI) validateOpts() flows.ValidateOptions {
	return flows.ValidateOptions{
		Events:   a.deps.Events,
		Actions:  a.deps.Actions,
		Triggers: a.deps.Triggers,
	}
}

func (a *FlowsAPI) templates() []flows.Template {
	if a.deps.Templates != nil {
		return a.deps.Templates
	}
	return DefaultFlowTemplates()
}

// publishStream forwards one console SSE event without ever blocking the
// caller: publish failures are logged and dropped.
func (a *FlowsAPI) publishStream(ctx context.Context, typ string, data any) {
	var err error
	switch {
	case a.deps.PublishStream != nil:
		err = a.deps.PublishStream(ctx, typ, data)
	case a.deps.StreamHub != nil:
		var raw json.RawMessage
		raw, err = json.Marshal(data)
		if err == nil {
			_, err = a.deps.StreamHub.Publish(typ, raw)
		}
	default:
		return
	}
	if err != nil {
		a.log.Debug("flows: publish stream event failed", "type", typ, "err", err)
	}
}

func (a *FlowsAPI) publishFlowChanged(ctx context.Context, ev FlowChangedEvent) {
	if ev.At.IsZero() {
		ev.At = a.now()
	}
	a.publishStream(ctx, StreamFlowsChanged, ev)
}

func (a *FlowsAPI) publishRun(ctx context.Context, run *flows.Run) {
	if run == nil {
		return
	}
	a.publishStream(ctx, StreamFlowRun, FlowRunEvent{
		RunID:      run.ID,
		FlowID:     run.FlowID,
		FlowName:   run.FlowName,
		EventType:  run.EventType,
		Status:     run.Status,
		DryRun:     run.DryRun,
		StepCount:  run.StepCount,
		Error:      run.Error,
		StartedAt:  run.StartedAt,
		UpdatedAt:  run.UpdatedAt,
		FinishedAt: run.FinishedAt,
	})
}

// Handler serves the owner-authenticated Flows API with paths relative
// to the mount point ("/api/flows" stripped), so the app can mount it
// through Deps.Extensions["flows"] or Deps.Flows. Authentication is the
// caller's job. The incoming webhook ingress is separate: WebhookHandler.
func (a *FlowsAPI) Handler() http.Handler {
	stream := FlowsStreamHandler(a.deps.StreamHub, "", "")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A hand-rolled dispatcher: /{id}/enable and
		// /from-template/{templateId} (likewise /{id}/runs and
		// /runs/{runId}) overlap in ways http.ServeMux patterns reject.
		// Reserved first segments win; generated flow IDs (flow_<hex>)
		// can never collide with them.
		segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(segs) == 1 && segs[0] == "" {
			segs = nil
		}
		methodNotAllowed := func(allow string) {
			w.Header().Set("Allow", allow)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed for this endpoint")
		}
		switch {
		case len(segs) == 0:
			switch r.Method {
			case http.MethodGet:
				a.handleListFlows(w, r)
			case http.MethodPost:
				a.handleCreateFlow(w, r)
			default:
				methodNotAllowed("GET, POST")
			}
		case segs[0] == "validate" && len(segs) == 1:
			if r.Method != http.MethodPost {
				methodNotAllowed("POST")
				return
			}
			a.handleValidate(w, r)
		case segs[0] == "test" && len(segs) == 1:
			if r.Method != http.MethodPost {
				methodNotAllowed("POST")
				return
			}
			a.handleTestDraft(w, r)
		case segs[0] == "describe" && len(segs) == 1:
			if r.Method != http.MethodPost {
				methodNotAllowed("POST")
				return
			}
			a.handleDescribeFlow(w, r)
		case segs[0] == "catalogue" && len(segs) == 2 && (segs[1] == "events" || segs[1] == "actions"):
			if r.Method != http.MethodGet {
				methodNotAllowed("GET")
				return
			}
			if segs[1] == "events" {
				a.handleEventCatalogue(w, r)
			} else {
				a.handleActionCatalogue(w, r)
			}
		case segs[0] == "templates" && len(segs) == 1:
			if r.Method != http.MethodGet {
				methodNotAllowed("GET")
				return
			}
			a.handleListTemplates(w, r)
		case segs[0] == "from-template" && len(segs) == 2:
			if r.Method != http.MethodPost {
				methodNotAllowed("POST")
				return
			}
			r.SetPathValue("templateId", segs[1])
			a.handleFromTemplate(w, r)
		case segs[0] == "customize-notification" && len(segs) == 1:
			if r.Method != http.MethodPost {
				methodNotAllowed("POST")
				return
			}
			a.handleCustomizeNotification(w, r)
		case segs[0] == "stats" && len(segs) == 1:
			if r.Method != http.MethodGet {
				methodNotAllowed("GET")
				return
			}
			a.handleRunStats(w, r)
		case segs[0] == "runs":
			a.routeRuns(w, r, segs, methodNotAllowed)
		case segs[0] == "stream" && len(segs) == 1:
			stream.ServeHTTP(w, r)
		default:
			a.routeFlow(w, r, segs, methodNotAllowed)
		}
	})
}

func (a *FlowsAPI) routeRuns(w http.ResponseWriter, r *http.Request, segs []string, methodNotAllowed func(string)) {
	switch {
	case len(segs) == 1:
		if r.Method != http.MethodGet {
			methodNotAllowed("GET")
			return
		}
		a.handleListRuns(w, r)
	case len(segs) == 2:
		if r.Method != http.MethodGet {
			methodNotAllowed("GET")
			return
		}
		r.SetPathValue("runId", segs[1])
		a.handleGetRun(w, r)
	case len(segs) == 3 && segs[2] == "cancel":
		if r.Method != http.MethodPost {
			methodNotAllowed("POST")
			return
		}
		r.SetPathValue("runId", segs[1])
		a.handleCancelRun(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", "no such flows endpoint")
	}
}

func (a *FlowsAPI) routeFlow(w http.ResponseWriter, r *http.Request, segs []string, methodNotAllowed func(string)) {
	r.SetPathValue("id", segs[0])
	switch {
	case len(segs) == 1:
		switch r.Method {
		case http.MethodGet:
			a.handleGetFlow(w, r)
		case http.MethodPut:
			a.handleUpdateFlow(w, r)
		case http.MethodDelete:
			a.handleDeleteFlow(w, r)
		default:
			methodNotAllowed("GET, PUT, DELETE")
		}
	case len(segs) == 2 && segs[1] == "runs":
		if r.Method != http.MethodGet {
			methodNotAllowed("GET")
			return
		}
		a.handleListFlowRuns(w, r)
	case len(segs) == 2:
		if r.Method != http.MethodPost {
			methodNotAllowed("POST")
			return
		}
		switch segs[1] {
		case "enable":
			a.handleEnableFlow(w, r)
		case "disable":
			a.handleDisableFlow(w, r)
		case "test":
			a.handleTestRun(w, r)
		case "run":
			a.handleManualRun(w, r)
		default:
			writeError(w, http.StatusNotFound, "not_found", "no such flows endpoint")
		}
	case len(segs) == 3 && segs[1] == "webhook-token" && segs[2] == "rotate":
		if r.Method != http.MethodPost {
			methodNotAllowed("POST")
			return
		}
		a.handleRotateWebhookToken(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", "no such flows endpoint")
	}
}

// flowsRoutes mounts the Flows API on the admin mux when Deps.Flows is
// set: owner-authenticated under /api/flows, plus the public incoming
// webhook ingress at /api/flows/hooks/{token} outside console auth.
func (h *Handler) flowsRoutes(mux *route.Mux) {
	if h.deps.Flows == nil {
		return
	}
	deps := *h.deps.Flows
	if deps.Logger == nil {
		deps.Logger = h.log
	}
	api := NewFlowsAPI(deps)
	mux.Handle("/api/flows/hooks/", http.StripPrefix("/api/flows/hooks", api.WebhookHandler()))
	if _, viaExtension := h.deps.Extensions["flows"]; viaExtension {
		// The app mounted the owner API itself; only the public
		// ingress above is ours to add (mounting both would panic).
		return
	}
	owner := h.requireAuth(http.StripPrefix("/api/flows", api.Handler()).ServeHTTP)
	mux.Handle("/api/flows", owner)
	mux.Handle("/api/flows/", owner)
}

func (a *FlowsAPI) requireStore(w http.ResponseWriter) bool {
	if a.deps.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "flow storage is not configured")
		return false
	}
	return true
}

func (a *FlowsAPI) requireRuntime(w http.ResponseWriter) bool {
	if a.deps.Runtime == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "flow runtime is not configured")
		return false
	}
	return true
}

// writeStoreError maps Store/Runtime sentinel errors to HTTP responses.
func (a *FlowsAPI) writeStoreError(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, flows.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", what+" not found")
	case errors.Is(err, flows.ErrFlowDisabled):
		writeError(w, http.StatusConflict, "flow_disabled", "flow is disabled")
	case errors.Is(err, flows.ErrNoManualTrigger):
		writeError(w, http.StatusConflict, "no_manual_trigger", "flow has no manual trigger")
	case errors.Is(err, flows.ErrRunNotActive):
		writeError(w, http.StatusConflict, "run_not_active", "run is not active")
	case errors.Is(err, flows.ErrRateLimited), errors.Is(err, flows.ErrLoopDepthLimit):
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
	case errors.Is(err, flows.ErrValidationFailed):
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
	default:
		a.log.Error("flows: "+what+" failed", "err", err)
		writeError(w, http.StatusInternalServerError, "store_error", "could not "+what)
	}
}

func writeFlowValidation(w http.ResponseWriter, res flows.ValidationResult) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error": map[string]any{
			"code":    "validation_failed",
			"message": "flow definition is invalid",
		},
		"validation": res,
	})
}

// decodeOptionalJSON decodes a request body that may be empty.
func decodeOptionalJSON(r *http.Request, dst any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	return decodeJSONLenient(r, dst)
}

// flowTriggerSummary condenses a graph's triggers for list views.
type flowTriggerSummary struct {
	Kinds  []string `json:"kinds"`
	Events []string `json:"events,omitempty"`
}

func summarizeTriggers(g flows.Graph) flowTriggerSummary {
	sum := flowTriggerSummary{Kinds: []string{}}
	seen := map[string]bool{}
	for _, n := range g.Nodes {
		if n.Disabled || !n.Kind.IsTrigger() {
			continue
		}
		if !seen[string(n.Kind)] {
			seen[string(n.Kind)] = true
			sum.Kinds = append(sum.Kinds, string(n.Kind))
		}
	}
	sort.Strings(sum.Kinds)
	sum.Events = g.SubscribedEventPatterns()
	return sum
}

// flowSummary is one row of GET /api/flows.
type flowSummary struct {
	ID                  string             `json:"id"`
	Name                string             `json:"name"`
	Description         string             `json:"description,omitempty"`
	Enabled             bool               `json:"enabled"`
	DisabledReason      string             `json:"disabledReason,omitempty"`
	Version             int                `json:"version"`
	Tags                []string           `json:"tags,omitempty"`
	TemplateID          string             `json:"templateId,omitempty"`
	ConsecutiveFailures int                `json:"consecutiveFailures,omitempty"`
	Trigger             flowTriggerSummary `json:"trigger"`
	LastRunAt           time.Time          `json:"lastRunAt,omitzero"`
	LastStatus          flows.RunStatus    `json:"lastStatus,omitempty"`
	Runs24h             int                `json:"runs24h"`
	Failures24h         int                `json:"failures24h"`
	ActiveRuns          int                `json:"activeRuns"`
	CreatedAt           time.Time          `json:"createdAt"`
	UpdatedAt           time.Time          `json:"updatedAt"`
}

func (a *FlowsAPI) handleListFlows(w http.ResponseWriter, r *http.Request) {
	if !a.requireStore(w) {
		return
	}
	all, err := a.deps.Store.ListFlows(r.Context())
	if err != nil {
		a.writeStoreError(w, err, "list flows")
		return
	}
	// One bounded recent-runs query feeds every summary's 24h counts, so
	// listing stays O(1) in store calls however many flows exist.
	type counts struct{ runs, failures, active int }
	byFlow := map[string]*counts{}
	cutoff := a.now().Add(-24 * time.Hour)
	if recent, _, err := a.deps.Store.ListRuns(r.Context(), flows.RunFilter{Limit: 500}); err == nil {
		for _, run := range recent {
			c := byFlow[run.FlowID]
			if c == nil {
				c = &counts{}
				byFlow[run.FlowID] = c
			}
			if !run.Status.Terminal() {
				c.active++
			}
			if !run.StartedAt.Before(cutoff) {
				c.runs++
				if run.Status == flows.RunFailed {
					c.failures++
				}
			}
		}
	}
	out := make([]flowSummary, 0, len(all))
	for _, f := range all {
		s := flowSummary{
			ID: f.ID, Name: f.Name, Description: f.Description,
			Enabled: f.Enabled, DisabledReason: f.DisabledReason,
			Version: f.Version, Tags: f.Tags, TemplateID: f.TemplateID,
			ConsecutiveFailures: f.ConsecutiveFailures,
			Trigger:             summarizeTriggers(f.Graph),
			LastRunAt:           f.LastRunAt, LastStatus: f.LastStatus,
			CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt,
		}
		if c := byFlow[f.ID]; c != nil {
			s.Runs24h, s.Failures24h, s.ActiveRuns = c.runs, c.failures, c.active
		}
		out = append(out, s)
	}
	writeJSON(w, http.StatusOK, map[string]any{"flows": out})
}

// flowRequest is the create/update body. Server-owned fields (version,
// timestamps, run stats) are ignored on write.
type flowRequest struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Enabled     *bool            `json:"enabled,omitempty"`
	Graph       flows.Graph      `json:"graph"`
	Limits      flows.FlowLimits `json:"limits,omitempty"`
	TemplateID  string           `json:"templateId,omitempty"`
	Tags        []string         `json:"tags,omitempty"`
}

func (req flowRequest) toFlow(id string) flows.Flow {
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	g := req.Graph
	if g.SchemaVersion == 0 {
		g.SchemaVersion = flows.SchemaVersion
	}
	return flows.Flow{
		ID:          id,
		Name:        strings.TrimSpace(req.Name),
		Description: req.Description,
		Enabled:     enabled,
		Graph:       g,
		Limits:      req.Limits,
		TemplateID:  req.TemplateID,
		Tags:        req.Tags,
	}
}

func (a *FlowsAPI) handleCreateFlow(w http.ResponseWriter, r *http.Request) {
	if !a.requireStore(w) {
		return
	}
	var req flowRequest
	if err := decodeJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "request body is not valid JSON")
		return
	}
	f := req.toFlow(a.newFlowID())
	if res := flows.ValidateFlow(f, a.validateOpts()); !res.Valid {
		writeFlowValidation(w, res)
		return
	}
	saved, err := a.deps.Store.SaveFlow(r.Context(), f)
	if err != nil {
		a.writeStoreError(w, err, "create flow")
		return
	}
	a.publishFlowChanged(r.Context(), FlowChangedEvent{
		Action: "created", FlowID: saved.ID, Name: saved.Name,
		Version: saved.Version, Enabled: &saved.Enabled,
	})
	writeJSON(w, http.StatusCreated, map[string]any{"flow": saved})
}

func (a *FlowsAPI) handleGetFlow(w http.ResponseWriter, r *http.Request) {
	if !a.requireStore(w) {
		return
	}
	f, err := a.deps.Store.GetFlow(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeStoreError(w, err, "flow")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"flow": f,
		"webhook": map[string]any{
			// The plaintext token is never echoed after rotation; the
			// console shows the path shape and offers Rotate instead.
			"configured": len(activeWebhookTriggers(f.Graph)) > 0,
			"path":       "/api/flows/hooks/{token}",
		},
	})
}

func (a *FlowsAPI) handleUpdateFlow(w http.ResponseWriter, r *http.Request) {
	if !a.requireStore(w) {
		return
	}
	id := r.PathValue("id")
	existing, err := a.deps.Store.GetFlow(r.Context(), id)
	if err != nil {
		a.writeStoreError(w, err, "flow")
		return
	}
	var req flowRequest
	if err := decodeJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "request body is not valid JSON")
		return
	}
	f := req.toFlow(id)
	f.CreatedAt = existing.CreatedAt
	if res := flows.ValidateFlow(f, a.validateOpts()); !res.Valid {
		writeFlowValidation(w, res)
		return
	}
	saved, err := a.deps.Store.SaveFlow(r.Context(), f)
	if err != nil {
		a.writeStoreError(w, err, "update flow")
		return
	}
	a.publishFlowChanged(r.Context(), FlowChangedEvent{
		Action: "updated", FlowID: saved.ID, Name: saved.Name,
		Version: saved.Version, Enabled: &saved.Enabled,
	})
	writeJSON(w, http.StatusOK, map[string]any{"flow": saved})
}

// cancelActiveRuns best-effort cancels up to 100 active runs of a flow
// and returns how many were cancelled.
func (a *FlowsAPI) cancelActiveRuns(ctx context.Context, flowID string) int {
	if a.deps.Runtime == nil || a.deps.Store == nil {
		return 0
	}
	cancelled := 0
	for _, status := range []flows.RunStatus{flows.RunPending, flows.RunRunning, flows.RunWaiting} {
		runs, _, err := a.deps.Store.ListRuns(ctx, flows.RunFilter{FlowID: flowID, Status: status, Limit: 100})
		if err != nil {
			continue
		}
		for _, run := range runs {
			if err := a.deps.Runtime.CancelRun(ctx, run.ID); err == nil {
				cancelled++
				run.Status = flows.RunCancelled
				a.publishRun(ctx, &run)
			}
		}
	}
	return cancelled
}

func (a *FlowsAPI) handleDeleteFlow(w http.ResponseWriter, r *http.Request) {
	if !a.requireStore(w) {
		return
	}
	id := r.PathValue("id")
	f, err := a.deps.Store.GetFlow(r.Context(), id)
	if err != nil {
		a.writeStoreError(w, err, "flow")
		return
	}
	cancelled := a.cancelActiveRuns(r.Context(), id)
	if err := a.deps.Store.DeleteFlow(r.Context(), id); err != nil {
		a.writeStoreError(w, err, "delete flow")
		return
	}
	a.publishFlowChanged(r.Context(), FlowChangedEvent{
		Action: "deleted", FlowID: id, Name: f.Name, CancelledRuns: cancelled,
	})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "cancelledRuns": cancelled})
}

func (a *FlowsAPI) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool, cancelActive bool) {
	if !a.requireStore(w) {
		return
	}
	id := r.PathValue("id")
	if _, err := a.deps.Store.GetFlow(r.Context(), id); err != nil {
		a.writeStoreError(w, err, "flow")
		return
	}
	reason := ""
	if !enabled {
		reason = "disabled by owner"
	}
	if err := a.deps.Store.SetFlowEnabled(r.Context(), id, enabled, reason); err != nil {
		a.writeStoreError(w, err, "update flow")
		return
	}
	cancelled := 0
	if !enabled && cancelActive {
		cancelled = a.cancelActiveRuns(r.Context(), id)
	}
	saved, err := a.deps.Store.GetFlow(r.Context(), id)
	if err != nil {
		a.writeStoreError(w, err, "flow")
		return
	}
	action := "enabled"
	if !enabled {
		action = "disabled"
	}
	a.publishFlowChanged(r.Context(), FlowChangedEvent{
		Action: action, FlowID: saved.ID, Name: saved.Name,
		Version: saved.Version, Enabled: &saved.Enabled, Reason: reason,
		CancelledRuns: cancelled,
	})
	writeJSON(w, http.StatusOK, map[string]any{"flow": saved})
}

func (a *FlowsAPI) handleEnableFlow(w http.ResponseWriter, r *http.Request) {
	a.setEnabled(w, r, true, false)
}

func (a *FlowsAPI) handleDisableFlow(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CancelActive bool `json:"cancelActive,omitempty"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "request body is not valid JSON")
		return
	}
	a.setEnabled(w, r, false, body.CancelActive)
}

func (a *FlowsAPI) handleValidate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string           `json:"name,omitempty"`
		Graph  *flows.Graph     `json:"graph,omitempty"`
		Limits flows.FlowLimits `json:"limits,omitempty"`
	}
	if err := decodeJSONLenient(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "request body is not valid JSON")
		return
	}
	if body.Graph == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "graph is required")
		return
	}
	g := *body.Graph
	if g.SchemaVersion == 0 {
		g.SchemaVersion = flows.SchemaVersion
	}
	res := flows.ValidateFlow(flows.Flow{Name: body.Name, Graph: g, Limits: body.Limits}, a.validateOpts())
	// A draft without a name yet is still editor-validatable: drop the
	// name requirement when the caller did not supply one.
	if strings.TrimSpace(body.Name) == "" {
		res.Errors = removeValidationCode(res.Errors, "missing_name")
		res.Valid = len(res.Errors) == 0
	}
	writeJSON(w, http.StatusOK, res)
}

func removeValidationCode(issues []flows.ValidationIssue, code string) []flows.ValidationIssue {
	out := issues[:0]
	for _, i := range issues {
		if i.Code != code {
			out = append(out, i)
		}
	}
	return out
}

// testRunBody keeps dryRun tri-state: omitted means dry (the design
// default), so a plain {} can never fire real side effects.
type testRunBody struct {
	Graph   *flows.Graph    `json:"graph,omitempty"`
	DryRun  *bool           `json:"dryRun,omitempty"`
	Event   events.Envelope `json:"event,omitempty"`
	Inputs  map[string]any  `json:"inputs,omitempty"`
	Trigger flows.NodeKind  `json:"trigger,omitempty"`
}

func (a *FlowsAPI) handleTestDraft(w http.ResponseWriter, r *http.Request) {
	a.runTest(w, r, "", true)
}

func (a *FlowsAPI) handleTestRun(w http.ResponseWriter, r *http.Request) {
	a.runTest(w, r, r.PathValue("id"), false)
}

func (a *FlowsAPI) runTest(w http.ResponseWriter, r *http.Request, flowID string, requireGraph bool) {
	if !a.requireRuntime(w) {
		return
	}
	var body testRunBody
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "request body is not valid JSON")
		return
	}
	if requireGraph && body.Graph == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "graph is required")
		return
	}
	dry := true
	if body.DryRun != nil {
		dry = *body.DryRun
	}
	if body.Graph != nil {
		g := *body.Graph
		if g.SchemaVersion == 0 {
			g.SchemaVersion = flows.SchemaVersion
		}
		if res := flows.ValidateGraph(g, a.validateOpts()); !res.Valid {
			writeFlowValidation(w, res)
			return
		}
		body.Graph = &g
	}
	result, err := a.deps.Runtime.TestRun(r.Context(), flows.TestRunRequest{
		FlowID:  flowID,
		Graph:   body.Graph,
		DryRun:  dry,
		Event:   body.Event,
		Inputs:  body.Inputs,
		Trigger: body.Trigger,
	})
	if err != nil {
		a.writeStoreError(w, err, "test run")
		return
	}
	if result != nil && result.Run.FlowID != "" {
		a.publishRun(r.Context(), &result.Run)
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *FlowsAPI) handleManualRun(w http.ResponseWriter, r *http.Request) {
	if !a.requireRuntime(w) {
		return
	}
	var body struct {
		Inputs map[string]any `json:"inputs,omitempty"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "request body is not valid JSON")
		return
	}
	run, err := a.deps.Runtime.TriggerManual(r.Context(), r.PathValue("id"), body.Inputs)
	if err != nil {
		a.writeStoreError(w, err, "start run")
		return
	}
	a.publishRun(r.Context(), run)
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run})
}

func (a *FlowsAPI) handleRotateWebhookToken(w http.ResponseWriter, r *http.Request) {
	if !a.requireStore(w) {
		return
	}
	id := r.PathValue("id")
	f, err := a.deps.Store.GetFlow(r.Context(), id)
	if err != nil {
		a.writeStoreError(w, err, "flow")
		return
	}
	plain, hash, err := generateWebhookToken()
	if err != nil {
		a.log.Error("flows: generate webhook token failed", "err", err)
		writeError(w, http.StatusInternalServerError, "token_failed", "could not generate a webhook token")
		return
	}
	if err := a.deps.Store.SetWebhookTokenHash(r.Context(), id, hash); err != nil {
		a.writeStoreError(w, err, "rotate webhook token")
		return
	}
	// The event deliberately carries no token material.
	a.publishFlowChanged(r.Context(), FlowChangedEvent{
		Action: "token_rotated", FlowID: id, Name: f.Name, Version: f.Version,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      plain,
		"webhookUrl": "/api/flows/hooks/" + plain,
	})
}

func runLimit(q queryValues) int {
	limit := 50
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}
	return limit
}

type queryValues map[string][]string

func (q queryValues) Get(key string) string {
	if vs := q[key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

func (a *FlowsAPI) listRuns(w http.ResponseWriter, r *http.Request, flowID string) {
	if !a.requireStore(w) {
		return
	}
	q := queryValues(r.URL.Query())
	if flowID == "" {
		flowID = strings.TrimSpace(q.Get("flowId"))
	}
	filter := flows.RunFilter{
		FlowID:    flowID,
		Status:    flows.RunStatus(strings.TrimSpace(q.Get("status"))),
		EventType: strings.TrimSpace(q.Get("eventType")),
		Cursor:    strings.TrimSpace(q.Get("cursor")),
		Limit:     runLimit(q),
	}
	runs, next, err := a.deps.Store.ListRuns(r.Context(), filter)
	if err != nil {
		a.writeStoreError(w, err, "list runs")
		return
	}
	if runs == nil {
		runs = []flows.Run{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "nextCursor": next})
}

func (a *FlowsAPI) handleListRuns(w http.ResponseWriter, r *http.Request) {
	a.listRuns(w, r, "")
}

func (a *FlowsAPI) handleListFlowRuns(w http.ResponseWriter, r *http.Request) {
	a.listRuns(w, r, r.PathValue("id"))
}

// runStatsWindow resolves ?range= (24h, 7d or 30d) and ?tz= (IANA, the
// browser's zone) into whole local buckets ending with the current one:
// 24 hours, 28 six-hour slots, or 30 days.
func runStatsWindow(rng, tz string, now time.Time) (string, flows.RunStatsQuery) {
	loc := time.UTC
	if tz = strings.TrimSpace(tz); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	local := now.In(loc)
	switch rng {
	case "7d":
		slot := time.Date(local.Year(), local.Month(), local.Day(), local.Hour()/6*6, 0, 0, 0, loc)
		return rng, flows.RunStatsQuery{Since: slot.Add(-27 * 6 * time.Hour), Bucket: 6 * time.Hour, Buckets: 28}
	case "30d":
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
		return rng, flows.RunStatsQuery{Since: day.AddDate(0, 0, -29), Bucket: 24 * time.Hour, Buckets: 30}
	default:
		hour := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, loc)
		return "24h", flows.RunStatsQuery{Since: hour.Add(-23 * time.Hour), Bucket: time.Hour, Buckets: 24}
	}
}

// handleRunStats serves GET /api/flows/stats: the Flows overview's
// aggregates, computed in the database over one bounded window.
func (a *FlowsAPI) handleRunStats(w http.ResponseWriter, r *http.Request) {
	if !a.requireStore(w) {
		return
	}
	statter, ok := a.deps.Store.(flows.RunStatsStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "run statistics are not available for this store")
		return
	}
	q := r.URL.Query()
	rng, query := runStatsWindow(q.Get("range"), q.Get("tz"), a.now())
	stats, err := statter.RunStats(r.Context(), query)
	if err != nil {
		a.writeStoreError(w, err, "run statistics")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"range": rng, "stats": stats})
}

func (a *FlowsAPI) handleGetRun(w http.ResponseWriter, r *http.Request) {
	if !a.requireStore(w) {
		return
	}
	runID := r.PathValue("runId")
	run, err := a.deps.Store.GetRun(r.Context(), runID)
	if err != nil {
		a.writeStoreError(w, err, "run")
		return
	}
	steps, err := a.deps.Store.ListRunSteps(r.Context(), runID)
	if err != nil {
		a.writeStoreError(w, err, "list run steps")
		return
	}
	if steps == nil {
		steps = []flows.RunStep{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "steps": steps})
}

func (a *FlowsAPI) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if !a.requireRuntime(w) {
		return
	}
	runID := r.PathValue("runId")
	if err := a.deps.Runtime.CancelRun(r.Context(), runID); err != nil {
		a.writeStoreError(w, err, "cancel run")
		return
	}
	if a.deps.Store != nil {
		if run, err := a.deps.Store.GetRun(r.Context(), runID); err == nil && run != nil {
			a.publishRun(r.Context(), run)
			writeJSON(w, http.StatusOK, run)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": runID, "status": flows.RunCancelled})
}

// catalogueField is a FieldSchema plus the condition operators the
// editor may offer for it.
type catalogueField struct {
	events.FieldSchema
	Operators []flows.Operator `json:"operators"`
}

// catalogueEvent mirrors events.EventSchema with enriched fields.
type catalogueEvent struct {
	Type        string           `json:"type"`
	Version     int              `json:"version"`
	Domain      string           `json:"domain"`
	Label       string           `json:"label"`
	Description string           `json:"description,omitempty"`
	Owner       string           `json:"owner"`
	Critical    bool             `json:"critical,omitempty"`
	Deprecated  bool             `json:"deprecated,omitempty"`
	ReplacedBy  string           `json:"replacedBy,omitempty"`
	LegacyNames []string         `json:"legacyNames,omitempty"`
	Fields      []catalogueField `json:"fields"`
	Sample      any              `json:"sample,omitempty"`
}

func toCatalogueEvent(s events.EventSchema) catalogueEvent {
	fields := make([]catalogueField, 0, len(s.Fields))
	for _, f := range s.Fields {
		fields = append(fields, catalogueField{
			FieldSchema: f,
			Operators:   flows.OperatorsForFieldType(f.Type),
		})
	}
	return catalogueEvent{
		Type: s.Type, Version: s.Version, Domain: s.Domain, Label: s.Label,
		Description: s.Description, Owner: s.Owner, Critical: s.Critical,
		Deprecated: s.Deprecated, ReplacedBy: s.ReplacedBy,
		LegacyNames: s.LegacyNames, Fields: fields, Sample: s.Sample,
	}
}

func (a *FlowsAPI) handleEventCatalogue(w http.ResponseWriter, r *http.Request) {
	operators := map[string][]flows.Operator{}
	for _, t := range []events.FieldType{
		events.FieldString, events.FieldInt, events.FieldFloat, events.FieldBool,
		events.FieldDurationMS, events.FieldTimestamp, events.FieldEnum,
		events.FieldStringList, events.FieldObject,
	} {
		operators[string(t)] = flows.OperatorsForFieldType(t)
	}
	all := []catalogueEvent{}
	type domainGroup struct {
		Domain string           `json:"domain"`
		Events []catalogueEvent `json:"events"`
	}
	domains := []domainGroup{}
	if a.deps.Events != nil {
		byDomain := map[string]int{}
		for _, s := range a.deps.Events.List() {
			ev := toCatalogueEvent(s)
			all = append(all, ev)
			idx, ok := byDomain[ev.Domain]
			if !ok {
				domains = append(domains, domainGroup{Domain: ev.Domain})
				idx = len(domains) - 1
				byDomain[ev.Domain] = idx
			}
			domains[idx].Events = append(domains[idx].Events, ev)
		}
		sort.Slice(domains, func(i, j int) bool { return domains[i].Domain < domains[j].Domain })
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events":               all,
		"domains":              domains,
		"operatorsByFieldType": operators,
	})
}

func (a *FlowsAPI) handleActionCatalogue(w http.ResponseWriter, r *http.Request) {
	actions := []flows.ActionSpec{}
	if a.deps.Actions != nil {
		actions = a.deps.Actions.List()
	}
	triggers := flows.BuiltinTriggerSpecs()
	if a.deps.Triggers != nil {
		triggers = a.deps.Triggers.List()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"actions":  actions,
		"triggers": triggers,
		"nodeKinds": []flows.NodeKind{
			flows.NodeTriggerEvent, flows.NodeTriggerSchedule,
			flows.NodeTriggerWebhook, flows.NodeTriggerManual,
			flows.NodeCondition, flows.NodeLogicSwitch, flows.NodeLogicDelay,
			flows.NodeLogicWaitEvent, flows.NodeLogicWindow,
			flows.NodeLogicDedupe, flows.NodeAction,
		},
	})
}

func (a *FlowsAPI) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"templates": a.templates()})
}

func (a *FlowsAPI) handleFromTemplate(w http.ResponseWriter, r *http.Request) {
	if !a.requireStore(w) {
		return
	}
	id := r.PathValue("templateId")
	var tmpl *flows.Template
	templates := a.templates()
	for i := range templates {
		if templates[i].ID == id {
			tmpl = &templates[i]
			break
		}
	}
	if tmpl == nil {
		writeError(w, http.StatusNotFound, "not_found", "template not found")
		return
	}
	var req fromTemplateRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "request body is not valid JSON")
		return
	}
	f, fieldErrs := instantiateFlowTemplate(*tmpl, req)
	if len(fieldErrs) > 0 {
		writeValidationError(w, fieldErrs)
		return
	}
	f.ID = a.newFlowID()
	if res := flows.ValidateFlow(f, a.validateOpts()); !res.Valid {
		writeFlowValidation(w, res)
		return
	}
	saved, err := a.deps.Store.SaveFlow(r.Context(), f)
	if err != nil {
		a.writeStoreError(w, err, "create flow")
		return
	}
	a.publishFlowChanged(r.Context(), FlowChangedEvent{
		Action: "created", FlowID: saved.ID, Name: saved.Name,
		Version: saved.Version, Enabled: &saved.Enabled,
	})
	writeJSON(w, http.StatusCreated, map[string]any{"flow": saved})
}

func (a *FlowsAPI) handleCustomizeNotification(w http.ResponseWriter, r *http.Request) {
	if !a.requireStore(w) {
		return
	}
	if a.deps.NotifyChannels == nil || a.deps.SaveNotifyChannel == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "notification channels are not configured")
		return
	}
	var req customizeNotificationRequest
	if err := decodeJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "request body is not valid JSON")
		return
	}
	if strings.TrimSpace(req.ChannelID) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "channelId is required")
		return
	}
	channels, err := a.deps.NotifyChannels(r.Context())
	if err != nil {
		a.log.Error("flows: list notification channels failed", "err", err)
		writeError(w, http.StatusInternalServerError, "store_error", "could not list notification channels")
		return
	}
	var ch *notify.Channel
	for i := range channels {
		if channels[i].ID == req.ChannelID {
			ch = &channels[i]
			break
		}
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "not_found", "notification channel not found")
		return
	}
	f, updated, migrated, kept, notes, err := buildFlowFromNotification(*ch, req, a.deps.Events)
	if err != nil {
		switch {
		case errors.Is(err, errCrashWatcherOnly), errors.Is(err, errNoMatchingRules):
			writeError(w, http.StatusConflict, "cannot_customize", err.Error())
		default:
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}
	f.ID = a.newFlowID()
	if res := flows.ValidateFlow(f, a.validateOpts()); !res.Valid {
		writeFlowValidation(w, res)
		return
	}
	saved, err := a.deps.Store.SaveFlow(r.Context(), f)
	if err != nil {
		a.writeStoreError(w, err, "create flow")
		return
	}
	removeLegacy := req.RemoveLegacyRule == nil || *req.RemoveLegacyRule
	if removeLegacy {
		if err := a.deps.SaveNotifyChannel(r.Context(), updated); err != nil {
			// Roll back the new flow so the owner never ends up with the
			// rule delivered twice (legacy channel plus flow).
			if derr := a.deps.Store.DeleteFlow(r.Context(), saved.ID); derr != nil {
				a.log.Error("flows: rollback customized flow failed", "flow", saved.ID, "err", derr)
			}
			a.log.Error("flows: save notification channel failed", "err", err)
			writeError(w, http.StatusInternalServerError, "store_error", "could not update the notification channel")
			return
		}
	}
	a.publishFlowChanged(r.Context(), FlowChangedEvent{
		Action: "created", FlowID: saved.ID, Name: saved.Name,
		Version: saved.Version, Enabled: &saved.Enabled,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"flow":      saved,
		"channelId": ch.ID,
		"migrated":  migrated,
		"kept":      kept,
		"notes":     notes,
	})
}
