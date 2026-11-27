package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/agent/builtin"
	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/flows"
	"github.com/braedonsaunders/ether/internal/flows/actions"
	"github.com/braedonsaunders/ether/internal/llm"
)

// flowsRuntime owns Ether's durable Flows store, action/trigger registries,
// and leader-elected execution engine.
type flowsRuntime struct {
	store        *flows.SQLStore
	actions      flows.ActionRegistry
	triggers     flows.TriggerRegistry
	engine       *flows.Engine
	restrictions *actions.RestrictionStore
}

// FlowsStore returns the durable Flows SQL store.
func (a *App) FlowsStore() *flows.SQLStore {
	if a == nil || a.flowsRT == nil {
		return nil
	}
	return a.flowsRT.store
}

// FlowsEngine returns the Flows execution engine.
func (a *App) FlowsEngine() *flows.Engine {
	if a == nil || a.flowsRT == nil {
		return nil
	}
	return a.flowsRT.engine
}

// FlowsActions returns the live Flows action registry so built-in actions
// (F3) and plugins can register action handlers dynamically.
func (a *App) FlowsActions() flows.ActionRegistry {
	if a == nil || a.flowsRT == nil {
		return nil
	}
	return a.flowsRT.actions
}

// buildFlows constructs the Flows store, action/trigger registries, and
// engine bound to the process-wide eventBus and eventRegistry. Safe to call
// multiple times; subsequent calls are no-ops.
func (a *App) buildFlows() {
	if a == nil || a.flowsRT != nil || a.db == nil {
		return
	}
	a.initEventBus()

	actionReg := a.opts.FlowActions
	var rstore *actions.RestrictionStore
	if actionReg == nil {
		actionReg = flows.NewActionRegistry()
		deps := a.flowActionDeps()
		rstore = deps.Restrictions
		if err := actions.RegisterAll(actionReg, deps); err != nil && a.log != nil {
			a.log.Warn("flows: register built-in actions failed", "err", err)
		}
	}
	triggers := flows.NewTriggerRegistry()
	st := flows.NewSQLStore(a.db, flows.WithStoreActions(actionReg))

	var cbus clusterbus.Bus
	if a.cluster != nil {
		cbus = a.cluster
	}
	var logger *slog.Logger
	if a.log != nil {
		logger = a.log.With("component", "flows")
	}

	eng := flows.NewEngine(flows.EngineOptions{
		Store:      st,
		DB:         a.db,
		Bus:        a.eventBus,
		Events:     a.eventRegistry,
		Actions:    actionReg,
		Triggers:   triggers,
		Cluster:    a.cluster,
		ClusterBus: cbus,
		NodeID:     a.clusterNodeID(),
		Logger:     logger,
		OnRunUpdated: func(run flows.Run) {
			if a.streamRT == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = a.streamRT.Publish(ctx, admin.StreamFlowRun, admin.FlowRunEvent{
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
		},
		OnAutoDisable: func(flow flows.Flow, run flows.Run) {
			if a.streamRT == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			enabled := false
			_ = a.streamRT.Publish(ctx, admin.StreamFlowsChanged, admin.FlowChangedEvent{
				Action:  "auto_disabled",
				FlowID:  flow.ID,
				Name:    flow.Name,
				Version: flow.Version,
				Enabled: &enabled,
				Reason:  run.Error,
				At:      time.Now().UTC(),
			})
		},
	})

	a.flowsRT = &flowsRuntime{
		store:        st,
		actions:      actionReg,
		triggers:     triggers,
		engine:       eng,
		restrictions: rstore,
	}
}

// runFlows runs the Flows runtime for the lifetime of ctx. Engine.Start runs
// leader work exclusively inside cluster.RunAsLeader("flows", ...) and runs
// the follower->leader OutboxBridge whenever clustered.
func (a *App) runFlows(ctx context.Context) {
	if a == nil || a.flowsRT == nil || a.flowsRT.engine == nil {
		return
	}
	var wg sync.WaitGroup
	if a.flowsRT.restrictions != nil && a.users != nil {
		rstore := a.flowsRT.restrictions
		wg.Add(1)
		go func() {
			defer wg.Done()
			runReverter := func(leadCtx context.Context) {
				rstore.RunReverter(leadCtx, appFlowViewers{app: a}, time.Minute)
			}
			if a.cluster != nil {
				a.cluster.RunAsLeader(ctx, "flow restriction reverter", runReverter)
			} else {
				runReverter(ctx)
			}
		}()
	}
	if err := a.flowsRT.engine.Start(ctx); err != nil && ctx.Err() == nil && a.log != nil {
		a.log.Warn("flows engine exited", "err", err)
	}
	wg.Wait()
}

// flowsAdminDeps builds the FlowsDeps injected into admin.Deps so
// /ether/api/flows/* and /ether/api/flows/hooks/* go live.
func (a *App) flowsAdminDeps() *admin.FlowsDeps {
	if a == nil || a.flowsRT == nil {
		return nil
	}
	var logger *slog.Logger
	if a.log != nil {
		logger = a.log.With("component", "flows-api")
	}
	d := &admin.FlowsDeps{
		Store:    a.flowsRT.store,
		Runtime:  a.flowsRT.engine,
		Events:   a.eventRegistry,
		Actions:  a.flowsRT.actions,
		Triggers: a.flowsRT.triggers,
		Logger:   logger,
	}
	if a.cfg != nil {
		d.TrustedProxyCIDRs = a.cfg.TrustedProxyCIDRs
	}
	if a.streamRT != nil {
		d.StreamHub = a.streamRT.hub
		d.PublishStream = a.streamRT.Publish
	}
	if a.notifications != nil {
		d.NotifyChannels = a.notifications.Channels
		d.SaveNotifyChannel = a.notifications.Save
	}
	if a.llm != nil && a.llm.Client() != nil {
		// Share the personalization client so applyConfig updates reach Flows.
		d.LLM = a.llm.Client()
	} else if a.cfg != nil {
		chat := llm.NewClient(llm.ClientOptions{Config: a.cfg.Read().LLM, Logger: logger})
		a.cfg.Subscribe(func(c config.Config) { chat.UpdateConfig(c.LLM) })
		d.LLM = chat
	}
	return d
}

// appFlowsService adapts App's flowsRuntime to builtin.FlowsService for Ask
// Ether and MCP tools.
type appFlowsService struct {
	a *App
}

var _ builtin.FlowsService = (*appFlowsService)(nil)

func (s *appFlowsService) ready() (*flowsRuntime, error) {
	if s == nil || s.a == nil || s.a.flowsRT == nil || s.a.flowsRT.store == nil {
		return nil, errors.New("flows runtime unavailable")
	}
	return s.a.flowsRT, nil
}

func (s *appFlowsService) ListFlows(ctx context.Context) ([]flows.Flow, error) {
	rt, err := s.ready()
	if err != nil {
		return nil, err
	}
	return rt.store.ListFlows(ctx)
}

func (s *appFlowsService) GetFlow(ctx context.Context, id string) (*flows.Flow, error) {
	rt, err := s.ready()
	if err != nil {
		return nil, err
	}
	f, err := rt.store.GetFlow(ctx, id)
	if err != nil {
		return nil, err
	}
	f.Graph = flows.RedactGraphActions(f.Graph, rt.actions)
	return f, nil
}

func (s *appFlowsService) ListTemplates() []flows.Template {
	return admin.DefaultFlowTemplates()
}

func (s *appFlowsService) instantiate(templateID, name, description string, enabled *bool, params map[string]any) (flows.Flow, error) {
	tmpls := admin.DefaultFlowTemplates()
	var tmpl *flows.Template
	ids := make([]string, 0, len(tmpls))
	for i := range tmpls {
		ids = append(ids, tmpls[i].ID)
		if tmpls[i].ID == templateID {
			tmpl = &tmpls[i]
		}
	}
	if tmpl == nil {
		return flows.Flow{}, fmt.Errorf("unknown template %q (available: %s)", templateID, strings.Join(ids, ", "))
	}
	f, fieldErrs := admin.InstantiateFlowTemplate(*tmpl, name, description, enabled, params)
	if len(fieldErrs) > 0 {
		parts := make([]string, 0, len(fieldErrs))
		for _, fe := range fieldErrs {
			parts = append(parts, fe.Field+": "+fe.Message)
		}
		return flows.Flow{}, fmt.Errorf("invalid template parameters: %s", strings.Join(parts, "; "))
	}
	if rt, err := s.ready(); err == nil {
		res := flows.ValidateFlow(f, flows.ValidateOptions{
			Events:   s.a.eventRegistry,
			Actions:  rt.actions,
			Triggers: rt.triggers,
		})
		if !res.Valid {
			parts := make([]string, 0, len(res.Errors))
			for _, ve := range res.Errors {
				if ve.NodeID != "" {
					parts = append(parts, ve.NodeID+": "+ve.Message)
				} else {
					parts = append(parts, ve.Message)
				}
			}
			return flows.Flow{}, fmt.Errorf("flow validation failed: %s", strings.Join(parts, "; "))
		}
	}
	return f, nil
}

func (s *appFlowsService) PreviewTemplate(_ context.Context, templateID, name, description string, enabled *bool, params map[string]any) (*flows.Flow, error) {
	f, err := s.instantiate(templateID, name, description, enabled, params)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *appFlowsService) CreateFromTemplate(ctx context.Context, templateID, name, description string, enabled *bool, params map[string]any) (*flows.Flow, error) {
	rt, err := s.ready()
	if err != nil {
		return nil, err
	}
	f, err := s.instantiate(templateID, name, description, enabled, params)
	if err != nil {
		return nil, err
	}
	f.ID = flows.NewFlowID()
	saved, err := rt.store.SaveFlow(ctx, f)
	if err != nil {
		return nil, err
	}
	if s.a.streamRT != nil {
		_ = s.a.streamRT.Publish(ctx, admin.StreamFlowsChanged, admin.FlowChangedEvent{
			Action:  "created",
			FlowID:  saved.ID,
			Name:    saved.Name,
			Version: saved.Version,
			Enabled: &saved.Enabled,
			At:      time.Now().UTC(),
		})
	}
	return saved, nil
}

func (s *appFlowsService) SetFlowEnabled(ctx context.Context, id string, enabled bool, reason string) error {
	rt, err := s.ready()
	if err != nil {
		return err
	}
	if err := rt.store.SetFlowEnabled(ctx, id, enabled, reason); err != nil {
		return err
	}
	if s.a.streamRT != nil {
		action := "disabled"
		if enabled {
			action = "enabled"
		}
		_ = s.a.streamRT.Publish(ctx, admin.StreamFlowsChanged, admin.FlowChangedEvent{
			Action:  action,
			FlowID:  id,
			Enabled: &enabled,
			Reason:  reason,
			At:      time.Now().UTC(),
		})
	}
	return nil
}

func (s *appFlowsService) TestRun(ctx context.Context, req flows.TestRunRequest) (*flows.TestRunResult, error) {
	rt, err := s.ready()
	if err != nil {
		return nil, err
	}
	if rt.engine == nil {
		return nil, errors.New("flows engine unavailable")
	}
	return rt.engine.TestRun(ctx, req)
}

func (s *appFlowsService) ListRuns(ctx context.Context, filter flows.RunFilter) ([]flows.Run, string, error) {
	rt, err := s.ready()
	if err != nil {
		return nil, "", err
	}
	return rt.store.ListRuns(ctx, filter)
}

func (s *appFlowsService) GetRun(ctx context.Context, runID string) (*flows.Run, []flows.RunStep, error) {
	rt, err := s.ready()
	if err != nil {
		return nil, nil, err
	}
	run, err := rt.store.GetRun(ctx, runID)
	if err != nil {
		return nil, nil, err
	}
	steps, err := rt.store.ListRunSteps(ctx, runID)
	if err != nil {
		return nil, nil, err
	}
	return run, steps, nil
}
