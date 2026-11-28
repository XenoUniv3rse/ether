package flows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/events"
)

type execMode uint8

const (
	modeDurable execMode = iota
	modeTestLive
	modeTestDry
)

const defaultActionTimeout = 30 * time.Second

// runState is the durable execution state serialized into flows_runs.context_json.
type runState struct {
	Event       events.Envelope           `json:"event,omitempty"`
	Trigger     map[string]any            `json:"trigger,omitempty"`
	Vars        map[string]any            `json:"vars,omitempty"`
	Steps       map[string]map[string]any `json:"steps,omitempty"`
	Queue       []string                  `json:"queue,omitempty"`
	Visited     map[string]bool           `json:"visited,omitempty"`
	NextSeq     int                       `json:"nextSeq,omitempty"`
	WaitingNode string                    `json:"waitingNode,omitempty"`
	WaitKind    string                    `json:"waitKind,omitempty"` // "delay" | "wait_event"
	WaitStepSeq int                       `json:"waitStepSeq,omitempty"`
	WaitResult  *waitResumeResult         `json:"waitResult,omitempty"`
}

type waitResumeResult struct {
	Matched bool            `json:"matched"`
	Event   events.Envelope `json:"event,omitempty"`
}

func decodeRunState(raw json.RawMessage) (runState, error) {
	var st runState
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &st); err != nil {
			return runState{}, fmt.Errorf("flows: decode run context: %w", err)
		}
	}
	if st.Vars == nil {
		st.Vars = map[string]any{}
	}
	if st.Steps == nil {
		st.Steps = map[string]map[string]any{}
	}
	if st.Visited == nil {
		st.Visited = map[string]bool{}
	}
	if st.Trigger == nil {
		st.Trigger = map[string]any{}
	}
	if st.NextSeq <= 0 {
		st.NextSeq = 1
	}
	return st, nil
}

func encodeRunState(st runState) (json.RawMessage, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return nil, fmt.Errorf("flows: encode run context: %w", err)
	}
	return b, nil
}

func (st *runState) evalContext() (EvalContext, error) {
	return NewEvalContext(st.Event, st.Vars, st.Steps, st.Trigger)
}

func (st *runState) enqueue(targets []string) {
	for _, target := range targets {
		if target == "" || st.Visited[target] || slices.Contains(st.Queue, target) {
			continue
		}
		st.Queue = append(st.Queue, target)
	}
}

func (st *runState) popNode(nodeID string) {
	if len(st.Queue) > 0 && st.Queue[0] == nodeID {
		st.Queue = st.Queue[1:]
		return
	}
	if idx := slices.Index(st.Queue, nodeID); idx >= 0 {
		st.Queue = slices.Delete(st.Queue, idx, idx+1)
	}
}

func outgoingTargets(edgesBySource map[string][]Edge, sourceID string, handles ...string) []string {
	edges := edgesBySource[sourceID]
	if len(edges) == 0 {
		return nil
	}
	var out []string
	for _, e := range edges {
		for _, h := range handles {
			if e.SourceHandle == h || (h == HandleNext && e.SourceHandle == "") {
				if !slices.Contains(out, e.Target) {
					out = append(out, e.Target)
				}
				break
			}
		}
	}
	return out
}

func indexGraph(g Graph) (map[string]Node, map[string][]Edge) {
	nodes := make(map[string]Node, len(g.Nodes))
	for _, n := range g.Nodes {
		nodes[n.ID] = n
	}
	edges := make(map[string][]Edge, len(g.Nodes))
	for _, e := range g.Edges {
		edges[e.Source] = append(edges[e.Source], e)
	}
	return nodes, edges
}

func marshalJSONRaw(v any) json.RawMessage {
	if v == nil {
		return json.RawMessage(`{}`)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func redactActionParams(spec ActionSpec, params map[string]any) map[string]any {
	if len(params) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		if p, ok := spec.Param(k); ok && p.Secret {
			if v == nil || v == "" {
				continue
			}
			out[k] = events.RedactedValue
			continue
		}
		out[k] = v
	}
	return out
}

func redactEventMap(reg *events.Registry, ev events.Envelope) map[string]any {
	payload, err := ev.PayloadMap()
	if err != nil {
		payload = map[string]any{}
	}
	if reg != nil {
		if schema, ok := reg.Get(ev.Type); ok {
			payload = schema.Redact(payload, events.RedactOptions{StripPII: false})
		}
	}
	meta := ev.MetaMap()
	meta["payload"] = payload
	return meta
}

func fullEventMap(ev events.Envelope) map[string]any {
	payload, err := ev.PayloadMap()
	if err != nil {
		payload = map[string]any{}
	}
	meta := ev.MetaMap()
	meta["payload"] = payload
	return meta
}

func cloneShallowMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

type checkpointStore interface {
	CompleteStepAndAdvanceRun(ctx context.Context, step *RunStep, run Run, waiters []Waiter) error
}

type leaseRenewStore interface {
	RenewRunLease(ctx context.Context, runID, owner string, until time.Time) (bool, error)
}

// executeRun drives a single Run through the flow DAG until it completes,
// fails, is cancelled, suspends on delay/wait_event, or loses leadership.
func (e *Engine) executeRun(
	ctx context.Context,
	run Run,
	flow Flow,
	mode execMode,
	persist bool,
) (Run, []RunStep, error) {
	st, err := decodeRunState(run.Context)
	if err != nil {
		return e.failRun(ctx, run, flow, st, nil, nil, "", err.Error(), mode, persist)
	}

	nodesByID, edgesBySource := indexGraph(flow.Graph)
	limits := flow.Limits.Effective()

	var memSteps []RunStep
	recordStepLocal := func(step *RunStep) {
		if persist {
			return
		}
		for i := range memSteps {
			if memSteps[i].Seq == step.Seq {
				memSteps[i] = *step
				return
			}
		}
		memSteps = append(memSteps, *step)
	}

	checkpoint := func(step *RunStep, r *Run, waiters []Waiter) error {
		ctxRaw, encErr := encodeRunState(st)
		if encErr != nil {
			return encErr
		}
		r.Context = ctxRaw
		r.UpdatedAt = e.now().UTC()
		if !persist {
			if step != nil {
				recordStepLocal(step)
			}
			return nil
		}
		if cs, ok := e.store.(checkpointStore); ok {
			if err := cs.CompleteStepAndAdvanceRun(ctx, step, *r, waiters); err != nil {
				return err
			}
		} else {
			if step != nil {
				if step.ID == 0 {
					id, err := e.store.RecordStep(ctx, *step)
					if err != nil {
						return err
					}
					step.ID = id
				} else if err := e.store.UpdateStep(ctx, *step); err != nil {
					return err
				}
			}
			for _, w := range waiters {
				if err := e.store.CreateWaiter(ctx, w); err != nil {
					return err
				}
			}
			if err := e.store.UpdateRun(ctx, *r); err != nil {
				return err
			}
		}
		e.emitRunUpdated(*r)
		return nil
	}

	// Enforce max wall-clock run duration for durable runs.
	if mode == modeDurable && limits.MaxRunDurationSec > 0 && !run.StartedAt.IsZero() {
		now := e.now().UTC()
		if now.Sub(run.StartedAt) > time.Duration(limits.MaxRunDurationSec)*time.Second {
			run.Status = RunCancelled
			run.Error = "max run duration exceeded"
			run.FinishedAt = now
			run.WakeAt = time.Time{}
			run.LeaseOwner = ""
			run.LeaseUntil = time.Time{}
			if err := checkpoint(nil, &run, nil); err != nil {
				return run, memSteps, err
			}
			if cs, ok := e.store.(cancelRunStore); ok && persist {
				_ = cs.CancelRun(ctx, run.ID, now)
			}
			return run, memSteps, nil
		}
	}

	var existingSteps []RunStep
	if persist {
		existingSteps, err = e.store.ListRunSteps(ctx, run.ID)
		if err != nil {
			return run, nil, err
		}
		for _, s := range existingSteps {
			if s.Seq >= st.NextSeq {
				st.NextSeq = s.Seq + 1
			}
		}
	}

	// 1. Resume from a durable delay or wait_event suspension.
	var resumeStep *RunStep
	if st.WaitingNode != "" {
		waitNodeID := st.WaitingNode
		waitKind := st.WaitKind
		waitStep := findStepBySeq(existingSteps, st.WaitStepSeq, waitNodeID)
		now := e.now().UTC()
		if waitStep == nil {
			waitStep = &RunStep{
				RunID:     run.ID,
				Seq:       st.NextSeq,
				NodeID:    waitNodeID,
				NodeKind:  NodeLogicDelay,
				StartedAt: now,
			}
			if waitKind == "wait_event" {
				waitStep.NodeKind = NodeLogicWaitEvent
			}
			st.NextSeq++
		}

		switch waitKind {
		case "delay":
			waitStep.Status = StepCompleted
			waitStep.Branch = HandleNext
			waitStep.FinishedAt = now
			waitStep.DurationMS = max(0, now.Sub(waitStep.StartedAt).Milliseconds())
			outMap := map[string]any{
				"waitedMs":  waitStep.DurationMS,
				"resumedAt": now.Format(time.RFC3339Nano),
			}
			waitStep.Output = marshalJSONRaw(outMap)
			st.Steps[waitNodeID] = map[string]any{"output": outMap}
			st.Visited[waitNodeID] = true
			st.enqueue(outgoingTargets(edgesBySource, waitNodeID, HandleNext))

		case "wait_event":
			if st.WaitResult == nil {
				// Defensive: woken without a result -> treat as timeout.
				st.WaitResult = &waitResumeResult{Matched: false}
			}
			waitStep.Status = StepCompleted
			waitStep.FinishedAt = now
			waitStep.DurationMS = max(0, now.Sub(waitStep.StartedAt).Milliseconds())
			if st.WaitResult.Matched {
				waitStep.Branch = HandleMatched
				fullEv := fullEventMap(st.WaitResult.Event)
				redactedEv := redactEventMap(e.opts.Events, st.WaitResult.Event)
				st.Steps[waitNodeID] = map[string]any{
					"matched": true,
					"event":   fullEv,
					"output": map[string]any{
						"matched": true,
						"event":   fullEv,
					},
				}
				waitStep.Output = marshalJSONRaw(map[string]any{
					"matched": true,
					"event":   redactedEv,
				})
				st.enqueue(outgoingTargets(edgesBySource, waitNodeID, HandleMatched))
			} else {
				waitStep.Branch = HandleTimeout
				outMap := map[string]any{"matched": false, "timedOut": true}
				st.Steps[waitNodeID] = map[string]any{
					"matched":  false,
					"timedOut": true,
					"output":   outMap,
				}
				waitStep.Output = marshalJSONRaw(outMap)
				st.enqueue(outgoingTargets(edgesBySource, waitNodeID, HandleTimeout))
			}
		}

		st.WaitingNode = ""
		st.WaitKind = ""
		st.WaitStepSeq = 0
		st.WaitResult = nil
		if err := checkpoint(waitStep, &run, nil); err != nil {
			return run, memSteps, err
		}
	} else if mode == modeDurable && len(existingSteps) > 0 {
		// 2. Crash recovery: inspect whether the last persisted step was left in StepRunning.
		last := existingSteps[len(existingSteps)-1]
		if last.Status == StepRunning {
			node, ok := nodesByID[last.NodeID]
			if !ok {
				last.Status = StepFailed
				last.Error = fmt.Sprintf("node %q no longer exists during recovery", last.NodeID)
				last.FinishedAt = e.now().UTC()
				last.DurationMS = max(0, last.FinishedAt.Sub(last.StartedAt).Milliseconds())
				return e.failRun(ctx, run, flow, st, &last, memSteps, last.NodeID, last.Error, mode, persist)
			}
			if node.Kind == NodeAction {
				cfg, decErr := DecodeConfig[ActionNodeConfig](node)
				var spec ActionSpec
				hasSpec := false
				if decErr == nil && e.opts.Actions != nil {
					spec, hasSpec = e.opts.Actions.Spec(cfg.ActionType)
				}
				if decErr != nil || !hasSpec || !spec.Idempotent {
					// Ambiguous crash on a non-idempotent side-effecting action:
					// never blindly double-execute; mark StepUncertain.
					now := e.now().UTC()
					last.Status = StepUncertain
					last.Error = "step interrupted mid-execution; non-idempotent action not retried"
					last.FinishedAt = now
					last.DurationMS = max(0, now.Sub(last.StartedAt).Milliseconds())
					st.Steps[node.ID] = map[string]any{
						"status": string(StepUncertain),
						"error":  last.Error,
					}
					st.Visited[node.ID] = true
					st.popNode(node.ID)

					errTargets := outgoingTargets(edgesBySource, node.ID, HandleError)
					switch {
					case len(errTargets) > 0:
						last.Branch = HandleError
						st.enqueue(errTargets)
						if err := checkpoint(&last, &run, nil); err != nil {
							return run, memSteps, err
						}
					case decErr == nil && cfg.ContinueOnError:
						last.Branch = HandleNext
						st.enqueue(outgoingTargets(edgesBySource, node.ID, HandleNext))
						if err := checkpoint(&last, &run, nil); err != nil {
							return run, memSteps, err
						}
					default:
						return e.failRun(ctx, run, flow, st, &last, memSteps, node.ID, last.Error, mode, persist)
					}
				} else {
					// Idempotent action: safe to re-execute with the exact same IdempotencyKey.
					cp := last
					resumeStep = &cp
					if len(st.Queue) == 0 || st.Queue[0] != node.ID {
						st.Queue = append([]string{node.ID}, st.Queue...)
					}
				}
			} else {
				// Pure logic / condition / trigger step: safe to re-run from last committed state.
				cp := last
				resumeStep = &cp
				if len(st.Queue) == 0 || st.Queue[0] != node.ID {
					st.Queue = append([]string{node.ID}, st.Queue...)
				}
			}
		}
	}

	// 3. Execute queued DAG nodes in BFS order.
	for len(st.Queue) > 0 {
		if e.isCancelled(run.ID) {
			return run, memSteps, nil
		}
		if ctx.Err() != nil {
			// Leadership lost or shutdown: leave run in its last checkpointed state.
			return run, memSteps, nil
		}

		nodeID := st.Queue[0]
		node, ok := nodesByID[nodeID]
		if !ok {
			st.popNode(nodeID)
			resumeStep = nil
			continue
		}
		if st.Visited[nodeID] && resumeStep == nil {
			st.popNode(nodeID)
			continue
		}

		if run.StepCount >= limits.MaxSteps {
			msg := fmt.Sprintf("step limit exceeded (max %d)", limits.MaxSteps)
			return e.failRun(ctx, run, flow, st, nil, memSteps, nodeID, msg, mode, persist)
		}

		now := e.now().UTC()
		var step RunStep
		if resumeStep != nil && resumeStep.NodeID == nodeID {
			step = *resumeStep
			resumeStep = nil
		} else {
			step = RunStep{
				RunID:     run.ID,
				Seq:       st.NextSeq,
				NodeID:    node.ID,
				NodeKind:  node.Kind,
				Status:    StepRunning,
				StartedAt: now,
			}
			st.NextSeq++
		}

		if node.Disabled {
			step.Status = StepSkipped
			step.FinishedAt = now
			step.DurationMS = 0
			run.StepCount++
			st.Visited[node.ID] = true
			st.popNode(node.ID)
			if node.Kind == NodeAction || node.Kind == NodeLogicDelay {
				step.Branch = HandleNext
				st.enqueue(outgoingTargets(edgesBySource, node.ID, HandleNext))
			}
			if err := checkpoint(&step, &run, nil); err != nil {
				return run, memSteps, err
			}
			continue
		}

		evalCtx, err := st.evalContext()
		if err != nil {
			step.Status = StepFailed
			step.Error = err.Error()
			step.FinishedAt = e.now().UTC()
			return e.failRun(ctx, run, flow, st, &step, memSteps, node.ID, err.Error(), mode, persist)
		}

		switch node.Kind {
		case NodeTriggerEvent:
			cfg, err := DecodeConfig[EventTriggerConfig](node)
			if err != nil {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
			}
			redactedEv := redactEventMap(e.opts.Events, st.Event)
			outMap := map[string]any{
				"eventType": st.Event.Type,
				"eventId":   st.Event.ID,
				"matched":   true,
			}
			step.Input = marshalJSONRaw(redactedEv)
			if mode != modeDurable && cfg.Filter != nil && !cfg.Filter.IsEmpty() {
				matched, trace, evalErr := EvalConditionGroup(*cfg.Filter, evalCtx)
				if evalErr != nil {
					return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, evalErr, mode, persist)
				}
				outMap["matched"] = matched
				outMap["filter"] = trace
				step.Output = marshalJSONRaw(outMap)
				step.Status = StepCompleted
				step.FinishedAt = e.now().UTC()
				step.DurationMS = max(0, step.FinishedAt.Sub(step.StartedAt).Milliseconds())
				run.StepCount++
				st.Steps[node.ID] = map[string]any{"matched": matched, "output": outMap}
				st.Visited[node.ID] = true
				st.popNode(node.ID)
				if matched {
					step.Branch = HandleNext
					st.enqueue(outgoingTargets(edgesBySource, node.ID, HandleNext))
				}
				if err := checkpoint(&step, &run, nil); err != nil {
					return run, memSteps, err
				}
				continue
			}
			step.Status = StepCompleted
			step.Branch = HandleNext
			step.Output = marshalJSONRaw(outMap)
			step.FinishedAt = e.now().UTC()
			step.DurationMS = max(0, step.FinishedAt.Sub(step.StartedAt).Milliseconds())
			run.StepCount++
			st.Steps[node.ID] = map[string]any{"output": outMap}
			st.Visited[node.ID] = true
			st.popNode(node.ID)
			st.enqueue(outgoingTargets(edgesBySource, node.ID, HandleNext))
			if err := checkpoint(&step, &run, nil); err != nil {
				return run, memSteps, err
			}

		case NodeTriggerSchedule, NodeTriggerWebhook, NodeTriggerManual:
			step.Input = marshalJSONRaw(st.Trigger)
			step.Output = marshalJSONRaw(st.Trigger)
			step.Status = StepCompleted
			step.Branch = HandleNext
			step.FinishedAt = e.now().UTC()
			step.DurationMS = max(0, step.FinishedAt.Sub(step.StartedAt).Milliseconds())
			run.StepCount++
			st.Steps[node.ID] = map[string]any{"output": cloneShallowMap(st.Trigger)}
			st.Visited[node.ID] = true
			st.popNode(node.ID)
			st.enqueue(outgoingTargets(edgesBySource, node.ID, HandleNext))
			if err := checkpoint(&step, &run, nil); err != nil {
				return run, memSteps, err
			}

		case NodeCondition:
			cfg, err := DecodeConfig[ConditionNodeConfig](node)
			if err != nil {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
			}
			matched, trace, err := EvalConditionGroup(cfg.Condition, evalCtx)
			if err != nil {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
			}
			branch := HandleFalse
			if matched {
				branch = HandleTrue
			}
			outMap := map[string]any{"matched": matched, "trace": trace}
			step.Status = StepCompleted
			step.Branch = branch
			step.Output = marshalJSONRaw(outMap)
			step.FinishedAt = e.now().UTC()
			step.DurationMS = max(0, step.FinishedAt.Sub(step.StartedAt).Milliseconds())
			run.StepCount++
			st.Steps[node.ID] = map[string]any{"matched": matched, "branch": branch, "output": outMap}
			st.Visited[node.ID] = true
			st.popNode(node.ID)
			st.enqueue(outgoingTargets(edgesBySource, node.ID, branch))
			if err := checkpoint(&step, &run, nil); err != nil {
				return run, memSteps, err
			}

		case NodeLogicSwitch:
			cfg, err := DecodeConfig[SwitchNodeConfig](node)
			if err != nil {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
			}
			var branches []string
			traces := make(map[string]ConditionTrace, len(cfg.Cases))
			for _, c := range cfg.Cases {
				matched, trace, evalErr := EvalConditionGroup(c.Condition, evalCtx)
				if evalErr != nil {
					return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, evalErr, mode, persist)
				}
				traces[c.Key] = trace
				if matched {
					branches = append(branches, c.Key)
					if !cfg.MatchAll {
						break
					}
				}
			}
			if len(branches) == 0 {
				branches = []string{HandleDefault}
			}
			outMap := map[string]any{"branches": branches, "traces": traces}
			step.Status = StepCompleted
			step.Branch = strings.Join(branches, ",")
			step.Output = marshalJSONRaw(outMap)
			step.FinishedAt = e.now().UTC()
			step.DurationMS = max(0, step.FinishedAt.Sub(step.StartedAt).Milliseconds())
			run.StepCount++
			st.Steps[node.ID] = map[string]any{"branches": branches, "output": outMap}
			st.Visited[node.ID] = true
			st.popNode(node.ID)
			st.enqueue(outgoingTargets(edgesBySource, node.ID, branches...))
			if err := checkpoint(&step, &run, nil); err != nil {
				return run, memSteps, err
			}

		case NodeLogicDelay:
			cfg, err := DecodeConfig[DelayNodeConfig](node)
			if err != nil {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
			}
			wakeAt, delayDur, err := computeDelayWakeAt(cfg, evalCtx, now)
			if err != nil {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
			}
			step.Input = marshalJSONRaw(map[string]any{
				"wakeAt":     wakeAt.UTC().Format(time.RFC3339Nano),
				"durationMs": delayDur.Milliseconds(),
			})
			if mode != modeDurable || !wakeAt.After(now) {
				outMap := map[string]any{
					"wakeAt":     wakeAt.UTC().Format(time.RFC3339Nano),
					"durationMs": delayDur.Milliseconds(),
					"simulated":  mode != modeDurable,
				}
				step.Status = StepCompleted
				step.Branch = HandleNext
				step.Output = marshalJSONRaw(outMap)
				step.FinishedAt = e.now().UTC()
				step.DurationMS = max(0, step.FinishedAt.Sub(step.StartedAt).Milliseconds())
				run.StepCount++
				st.Steps[node.ID] = map[string]any{"output": outMap}
				st.Visited[node.ID] = true
				st.popNode(node.ID)
				st.enqueue(outgoingTargets(edgesBySource, node.ID, HandleNext))
				if err := checkpoint(&step, &run, nil); err != nil {
					return run, memSteps, err
				}
				continue
			}

			// Durable suspension: persist step as running, transition run to waiting, release worker.
			run.StepCount++
			st.popNode(node.ID)
			st.WaitingNode = node.ID
			st.WaitKind = "delay"
			st.WaitStepSeq = step.Seq
			run.Status = RunWaiting
			run.WakeAt = wakeAt.UTC()
			run.LeaseOwner = ""
			run.LeaseUntil = time.Time{}
			if err := checkpoint(&step, &run, nil); err != nil {
				return run, memSteps, err
			}
			return run, memSteps, nil

		case NodeLogicWaitEvent:
			cfg, err := DecodeConfig[WaitEventNodeConfig](node)
			if err != nil {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
			}
			timeoutDur, err := ParseDuration(cfg.Timeout)
			if err != nil || timeoutDur <= 0 || timeoutDur > MaxDelayDuration {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, fmt.Errorf("flows: invalid wait_event timeout %q", cfg.Timeout), mode, persist)
			}
			timeoutAt := now.Add(timeoutDur).UTC()
			step.Input = marshalJSONRaw(map[string]any{
				"eventTypes":       cfg.EventTypes,
				"matchCorrelation": cfg.MatchCorrelation,
				"timeoutAt":        timeoutAt.Format(time.RFC3339Nano),
			})
			if mode != modeDurable {
				outMap := map[string]any{"matched": false, "timedOut": true, "simulated": true}
				step.Status = StepCompleted
				step.Branch = HandleTimeout
				step.Output = marshalJSONRaw(outMap)
				step.FinishedAt = e.now().UTC()
				step.DurationMS = max(0, step.FinishedAt.Sub(step.StartedAt).Milliseconds())
				run.StepCount++
				st.Steps[node.ID] = map[string]any{"matched": false, "timedOut": true, "output": outMap}
				st.Visited[node.ID] = true
				st.popNode(node.ID)
				st.enqueue(outgoingTargets(edgesBySource, node.ID, HandleTimeout))
				if err := checkpoint(&step, &run, nil); err != nil {
					return run, memSteps, err
				}
				continue
			}

			var filterRaw json.RawMessage
			if cfg.Filter != nil && !cfg.Filter.IsEmpty() {
				filterRaw, err = json.Marshal(cfg.Filter)
				if err != nil {
					return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
				}
			}
			corrKey := ""
			if cfg.MatchCorrelation {
				corrKey = run.CorrelationKey
			}
			waiters := make([]Waiter, 0, len(cfg.EventTypes))
			for _, evPat := range cfg.EventTypes {
				waiters = append(waiters, Waiter{
					ID:             NewWaiterID(),
					RunID:          run.ID,
					FlowID:         run.FlowID,
					NodeID:         node.ID,
					EventType:      evPat,
					CorrelationKey: corrKey,
					Filter:         filterRaw,
					TimeoutAt:      timeoutAt,
					CreatedAt:      now,
					Status:         "waiting",
				})
			}

			run.StepCount++
			st.popNode(node.ID)
			st.WaitingNode = node.ID
			st.WaitKind = "wait_event"
			st.WaitStepSeq = step.Seq
			run.Status = RunWaiting
			run.WakeAt = time.Time{} // woken by MatchWaiters or ExpireWaiters
			run.LeaseOwner = ""
			run.LeaseUntil = time.Time{}
			if err := checkpoint(&step, &run, waiters); err != nil {
				return run, memSteps, err
			}
			e.broadcastSubscriptions(ctx)
			return run, memSteps, nil

		case NodeLogicWindow:
			cfg, err := DecodeConfig[WindowNodeConfig](node)
			if err != nil {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
			}
			winDur, err := ParseDuration(cfg.Window)
			if err != nil || winDur <= 0 {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, fmt.Errorf("flows: invalid window duration %q", cfg.Window), mode, persist)
			}
			groupKey, err := renderLogicKey(cfg.KeyTemplate, evalCtx, run.CorrelationKey)
			if err != nil {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
			}
			step.Input = marshalJSONRaw(map[string]any{"key": groupKey, "threshold": cfg.Threshold, "window": cfg.Window})
			var count int
			var reached bool
			if mode == modeTestDry || !persist {
				count = 1
				reached = count >= cfg.Threshold
			} else {
				resetAt := 0
				if cfg.ResetOnReach {
					resetAt = cfg.Threshold
				}
				count, reached, err = e.store.RecordWindowEvent(ctx, run.FlowID, node.ID, groupKey, now, winDur, resetAt)
				if err != nil {
					return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
				}
				if !cfg.ResetOnReach {
					reached = count >= cfg.Threshold
				}
			}
			branch := HandleBelow
			if reached {
				branch = HandleReached
			}
			outMap := map[string]any{"key": groupKey, "count": count, "threshold": cfg.Threshold, "reached": reached}
			step.Status = StepCompleted
			step.Branch = branch
			step.Output = marshalJSONRaw(outMap)
			step.FinishedAt = e.now().UTC()
			step.DurationMS = max(0, step.FinishedAt.Sub(step.StartedAt).Milliseconds())
			run.StepCount++
			st.Steps[node.ID] = map[string]any{"count": count, "reached": reached, "output": outMap}
			st.Visited[node.ID] = true
			st.popNode(node.ID)
			st.enqueue(outgoingTargets(edgesBySource, node.ID, branch))
			if err := checkpoint(&step, &run, nil); err != nil {
				return run, memSteps, err
			}

		case NodeLogicDedupe:
			cfg, err := DecodeConfig[DedupeNodeConfig](node)
			if err != nil {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
			}
			coolDur, err := ParseDuration(cfg.Cooldown)
			if err != nil || coolDur <= 0 {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, fmt.Errorf("flows: invalid dedupe cooldown %q", cfg.Cooldown), mode, persist)
			}
			dedupeKey, err := renderLogicKey(cfg.KeyTemplate, evalCtx, run.CorrelationKey)
			if err != nil {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
			}
			step.Input = marshalJSONRaw(map[string]any{"key": dedupeKey, "cooldown": cfg.Cooldown, "maxPerWindow": cfg.MaxPerWindow})
			var allowed bool
			var hits int
			if mode == modeTestDry || !persist {
				allowed = true
				hits = 1
			} else {
				allowed, hits, err = e.store.CheckDedupe(ctx, run.FlowID, node.ID, dedupeKey, now, coolDur, cfg.MaxPerWindow)
				if err != nil {
					return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
				}
			}
			branch := HandleSuppressed
			if allowed {
				branch = HandlePass
			}
			outMap := map[string]any{"key": dedupeKey, "allowed": allowed, "hits": hits}
			step.Status = StepCompleted
			step.Branch = branch
			step.Output = marshalJSONRaw(outMap)
			step.FinishedAt = e.now().UTC()
			step.DurationMS = max(0, step.FinishedAt.Sub(step.StartedAt).Milliseconds())
			run.StepCount++
			st.Steps[node.ID] = map[string]any{"allowed": allowed, "hits": hits, "output": outMap}
			st.Visited[node.ID] = true
			st.popNode(node.ID)
			st.enqueue(outgoingTargets(edgesBySource, node.ID, branch))
			if err := checkpoint(&step, &run, nil); err != nil {
				return run, memSteps, err
			}

		case NodeAction:
			cfg, err := DecodeConfig[ActionNodeConfig](node)
			if err != nil {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
			}
			if e.opts.Actions == nil {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, fmt.Errorf("flows: action registry not configured"), mode, persist)
			}
			handler, ok := e.opts.Actions.Get(cfg.ActionType)
			if !ok {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, fmt.Errorf("flows: unknown action %q", cfg.ActionType), mode, persist)
			}
			spec := handler.Spec()

			mergedParams := applyParamDefaults(spec, cfg.Params)
			renderedParams, err := RenderParams(mergedParams, evalCtx)
			if err != nil {
				return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
			}

			idemKey := step.IdempotencyKey
			if idemKey == "" {
				if strings.TrimSpace(cfg.IdempotencyKey) != "" {
					idemKey, err = RenderTemplate(cfg.IdempotencyKey, evalCtx)
					if err != nil {
						return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, err, mode, persist)
					}
				} else {
					idemKey = IdempotencyKey(run.ID, node.ID, run.Attempt)
				}
			}
			step.IdempotencyKey = idemKey
			step.Input = marshalJSONRaw(redactActionParams(spec, renderedParams))

			// Checkpoint StepRunning BEFORE invoking the action so an ambiguous crash
			// is detected on recovery and never blindly double-executes non-idempotent actions.
			if persist && step.ID == 0 {
				stepID, recErr := e.store.RecordStep(ctx, step)
				if recErr != nil {
					return run, memSteps, recErr
				}
				step.ID = stepID
			}

			actTimeout := defaultActionTimeout
			if cfg.Timeout != "" {
				if d, parseErr := ParseDuration(cfg.Timeout); parseErr == nil && d > 0 {
					actTimeout = min(d, MaxActionTimeout)
				}
			}

			runCtx, runCancel := e.withRunCancel(ctx, run.ID)
			actCtx, actCancel := context.WithTimeout(runCtx, actTimeout)

			var leaseStop chan struct{}
			if mode == modeDurable && persist {
				leaseStop = e.startLeaseRenewer(runCtx, run.ID)
			}

			var varsMu sync.Mutex
			actx := ActionContext{
				FlowID:         run.FlowID,
				RunID:          run.ID,
				NodeID:         node.ID,
				IdempotencyKey: idemKey,
				DryRun:         run.DryRun,
				FlowDepth:      run.FlowDepth,
				Event:          st.Event,
				Vars:           cloneShallowMap(st.Vars),
				SetVar: func(k string, v any) {
					varsMu.Lock()
					st.Vars[k] = v
					varsMu.Unlock()
				},
			}

			res, actErr := invokeActionSafely(actCtx, handler, actx, renderedParams)
			if leaseStop != nil {
				close(leaseStop)
			}
			actCancel()
			runCancel()

			if actErr != nil {
				if e.isCancelled(run.ID) {
					return run, memSteps, nil
				}
				// If our parent leader context was cancelled (leadership step-down or shutdown),
				// do NOT mark the step failed: leave StepRunning so the next leader recovers
				// it safely according to idempotency rules.
				if ctx.Err() != nil && errors.Is(actErr, context.Canceled) {
					return run, memSteps, nil
				}
				step.Status = StepFailed
				step.Error = actErr.Error()
				step.FinishedAt = e.now().UTC()
				step.DurationMS = max(0, step.FinishedAt.Sub(step.StartedAt).Milliseconds())
				run.StepCount++
				st.Steps[node.ID] = map[string]any{"status": "failed", "error": actErr.Error()}
				st.Visited[node.ID] = true
				st.popNode(node.ID)

				errTargets := outgoingTargets(edgesBySource, node.ID, HandleError)
				switch {
				case len(errTargets) > 0:
					step.Branch = HandleError
					st.enqueue(errTargets)
					if err := checkpoint(&step, &run, nil); err != nil {
						return run, memSteps, err
					}
					continue
				case cfg.ContinueOnError:
					step.Branch = HandleNext
					st.enqueue(outgoingTargets(edgesBySource, node.ID, HandleNext))
					if err := checkpoint(&step, &run, nil); err != nil {
						return run, memSteps, err
					}
					continue
				default:
					return e.failRun(ctx, run, flow, st, &step, memSteps, node.ID, actErr.Error(), mode, persist)
				}
			}

			branch := res.Branch
			if branch == "" {
				branch = HandleNext
			}
			out := res.Output
			if out == nil {
				out = map[string]any{}
			}
			step.Status = StepCompleted
			step.Branch = branch
			step.Output = marshalJSONRaw(out)
			step.FinishedAt = e.now().UTC()
			step.DurationMS = max(0, step.FinishedAt.Sub(step.StartedAt).Milliseconds())
			run.StepCount++
			st.Steps[node.ID] = map[string]any{"status": "completed", "branch": branch, "output": out}
			st.Visited[node.ID] = true
			st.popNode(node.ID)
			st.enqueue(outgoingTargets(edgesBySource, node.ID, branch))
			if err := checkpoint(&step, &run, nil); err != nil {
				return run, memSteps, err
			}

		default:
			return e.failStepOrRun(ctx, run, flow, st, &step, memSteps, node, fmt.Errorf("flows: unsupported node kind %q", node.Kind), mode, persist)
		}
	}

	// All queued work completed cleanly.
	now := e.now().UTC()
	run.Status = RunCompleted
	run.FinishedAt = now
	run.WakeAt = time.Time{}
	run.LeaseOwner = ""
	run.LeaseUntil = time.Time{}
	if err := checkpoint(nil, &run, nil); err != nil {
		return run, memSteps, err
	}
	if mode == modeDurable && persist {
		e.recordFlowOutcome(ctx, flow, run, "")
	}
	if persist {
		steps, err := e.store.ListRunSteps(ctx, run.ID)
		return run, steps, err
	}
	return run, memSteps, nil
}

func (e *Engine) failStepOrRun(
	ctx context.Context,
	run Run,
	flow Flow,
	st runState,
	step *RunStep,
	memSteps []RunStep,
	node Node,
	err error,
	mode execMode,
	persist bool,
) (Run, []RunStep, error) {
	now := e.now().UTC()
	step.Status = StepFailed
	step.Error = err.Error()
	step.FinishedAt = now
	step.DurationMS = max(0, now.Sub(step.StartedAt).Milliseconds())
	run.StepCount++
	st.Steps[node.ID] = map[string]any{"status": "failed", "error": err.Error()}
	st.Visited[node.ID] = true
	st.popNode(node.ID)

	return e.failRun(ctx, run, flow, st, step, memSteps, node.ID, err.Error(), mode, persist)
}

func (e *Engine) failRun(
	ctx context.Context,
	run Run,
	flow Flow,
	st runState,
	step *RunStep,
	memSteps []RunStep,
	failedNodeID string,
	errMsg string,
	mode execMode,
	persist bool,
) (Run, []RunStep, error) {
	now := e.now().UTC()
	run.Status = RunFailed
	run.Error = errMsg
	run.FinishedAt = now
	run.UpdatedAt = now
	run.WakeAt = time.Time{}
	run.LeaseOwner = ""
	run.LeaseUntil = time.Time{}

	if ctxRaw, encErr := encodeRunState(st); encErr == nil {
		run.Context = ctxRaw
	}

	if persist {
		if cs, ok := e.store.(checkpointStore); ok {
			_ = cs.CompleteStepAndAdvanceRun(ctx, step, run, nil)
		} else {
			if step != nil {
				if step.ID == 0 {
					id, _ := e.store.RecordStep(ctx, *step)
					step.ID = id
				} else {
					_ = e.store.UpdateStep(ctx, *step)
				}
			}
			_ = e.store.UpdateRun(ctx, run)
		}
		e.emitRunUpdated(run)
		if mode == modeDurable {
			e.recordFlowOutcome(ctx, flow, run, failedNodeID)
		}
		steps, err := e.store.ListRunSteps(ctx, run.ID)
		return run, steps, err
	}

	if step != nil {
		replaced := false
		for i := range memSteps {
			if memSteps[i].Seq == step.Seq {
				memSteps[i] = *step
				replaced = true
				break
			}
		}
		if !replaced {
			memSteps = append(memSteps, *step)
		}
	}
	return run, memSteps, nil
}

func findStepBySeq(steps []RunStep, seq int, nodeID string) *RunStep {
	if seq > 0 {
		for i := range steps {
			if steps[i].Seq == seq {
				cp := steps[i]
				return &cp
			}
		}
	}
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].NodeID == nodeID {
			cp := steps[i]
			return &cp
		}
	}
	return nil
}

func applyParamDefaults(spec ActionSpec, params map[string]any) map[string]any {
	out := make(map[string]any, len(params)+len(spec.Params))
	for _, p := range spec.Params {
		if p.Default != nil {
			out[p.Key] = p.Default
		}
	}
	for k, v := range params {
		out[k] = v
	}
	return out
}

func computeDelayWakeAt(cfg DelayNodeConfig, ctx EvalContext, now time.Time) (time.Time, time.Duration, error) {
	switch {
	case strings.TrimSpace(cfg.Duration) != "":
		d, err := ParseDuration(strings.TrimSpace(cfg.Duration))
		if err != nil || d < 0 || d > MaxDelayDuration {
			return time.Time{}, 0, fmt.Errorf("flows: invalid delay duration %q", cfg.Duration)
		}
		return now.Add(d).UTC(), d, nil

	case strings.TrimSpace(cfg.Until) != "":
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(cfg.Until))
		if err != nil {
			return time.Time{}, 0, fmt.Errorf("flows: invalid delay until %q: %w", cfg.Until, err)
		}
		d := max(t.Sub(now), 0)
		if d > MaxDelayDuration {
			return time.Time{}, 0, fmt.Errorf("flows: delay until exceeds maximum duration %s", MaxDelayDuration)
		}
		return t.UTC(), d, nil

	case strings.TrimSpace(cfg.DurationField) != "":
		val, ok := ResolvePath(cfg.DurationField, ctx)
		if !ok || val == nil {
			return now.UTC(), 0, nil
		}
		var d time.Duration
		switch x := val.(type) {
		case string:
			parsed, err := ParseDuration(strings.TrimSpace(x))
			if err != nil {
				return time.Time{}, 0, fmt.Errorf("flows: invalid durationField %q value %q: %w", cfg.DurationField, x, err)
			}
			d = parsed
		default:
			ms, ok := asFloat(val)
			if !ok {
				return time.Time{}, 0, fmt.Errorf("flows: durationField %q must be a duration string or milliseconds", cfg.DurationField)
			}
			d = time.Duration(ms) * time.Millisecond
		}
		if d < 0 {
			d = 0
		}
		if d > MaxDelayDuration {
			return time.Time{}, 0, fmt.Errorf("flows: durationField exceeds maximum duration %s", MaxDelayDuration)
		}
		return now.Add(d).UTC(), d, nil

	default:
		return time.Time{}, 0, fmt.Errorf("flows: delay node missing duration, durationField, or until")
	}
}

func renderLogicKey(tmpl string, evalCtx EvalContext, fallback string) (string, error) {
	if strings.TrimSpace(tmpl) != "" {
		key, err := RenderTemplate(tmpl, evalCtx)
		if err != nil {
			return "", err
		}
		if key != "" {
			return key, nil
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "default", nil
}

func invokeActionSafely(ctx context.Context, h ActionHandler, actx ActionContext, params map[string]any) (res ActionResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("action %q panicked: %v", h.Spec().Type, r)
		}
	}()
	return h.Execute(ctx, actx, params)
}

func (e *Engine) startLeaseRenewer(ctx context.Context, runID string) chan struct{} {
	stop := make(chan struct{})
	renewer, ok := e.store.(leaseRenewStore)
	if !ok {
		return stop
	}
	interval := max(e.opts.LeaseDuration/3, 50*time.Millisecond)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = renewer.RenewRunLease(ctx, runID, e.nodeID, e.now().UTC().Add(e.opts.LeaseDuration))
			}
		}
	}()
	return stop
}
