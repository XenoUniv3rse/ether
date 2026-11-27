package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/flows"
	"github.com/braedonsaunders/ether/internal/identity"
)

func TestFlowsAppWiringEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.SetupComplete = true
		c.Frontends = map[string]config.Frontend{"plex": {Enabled: false}}
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Inject a custom action into FlowActions to prove the registry is injectable
	// and wired through the Engine, Admin API, and MCP tools.
	var actionCalls atomic.Int32
	actionDone := make(chan map[string]any, 4)
	actions := flows.NewActionRegistry()
	actions.MustRegister(flows.ActionFunc{
		ActionSpec: flows.ActionSpec{
			Type:       "notify.send",
			Group:      "notifications",
			Label:      "Send notification",
			Idempotent: true,
			SideEffect: true,
			Params: []flows.ParamSchema{
				{Key: "channelIds", Type: events.FieldStringList, Label: "Channels", Required: true, Picker: flows.PickerChannel},
				{Key: "title", Type: events.FieldString, Label: "Title", Template: true},
				{Key: "message", Type: events.FieldString, Label: "Message", Template: true},
				{Key: "color", Type: events.FieldString, Label: "Accent color", Template: true},
				{Key: "priority", Type: events.FieldInt, Label: "Priority", Template: true},
				{Key: "credential", Type: events.FieldString, Label: "Credential", Secret: true},
				{Key: "optionalCredential", Type: events.FieldString, Label: "Optional credential", Secret: true},
				{Key: "bypassDigest", Type: events.FieldBool, Label: "Bypass digest"},
			},
		},
		Run: func(_ context.Context, actx flows.ActionContext, params map[string]any) (flows.ActionResult, error) {
			if !actx.DryRun {
				actionCalls.Add(1)
				select {
				case actionDone <- params:
				default:
				}
			}
			return flows.ActionResult{Output: map[string]any{"sent": true, "dryRun": actx.DryRun}}, nil
		},
	})

	a, err := New(ctx, Options{
		DataDir:     dir,
		Version:     "0.3.17-test",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		FlowActions: actions,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)

	if a.EventBus() == nil || a.EventRegistry() == nil {
		t.Fatal("expected EventBus and EventRegistry initialized")
	}
	if a.FlowsStore() == nil || a.FlowsEngine() == nil || a.FlowsActions() != actions {
		t.Fatal("expected FlowsStore, FlowsEngine, and injected FlowsActions wired")
	}
	if a.FlowsEngine().Bridge() == nil {
		t.Fatal("expected OutboxBridge attached to FlowsEngine")
	}

	// Mint an owner console token for /ether/api/flows/* requests.
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	ownerID, err := local.CreateOwner(ctx, "owner", "correct-horse-battery-staple")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(a.db, a.users, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	ownerToken, err := grants.Issue(ctx, ownerID, nil, "owner", core.Device{ID: "flows-test"})
	if err != nil {
		t.Fatal(err)
	}

	doAPI := func(method, path string, body any) *httptest.ResponseRecorder {
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req := httptest.NewRequest(method, "http://127.0.0.1:32400"+path, rdr)
		req.RemoteAddr = "127.0.0.1:54321"
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		req.Header.Set("X-Ether-Request", "1")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, req)
		return rec
	}

	// 1. Verify /ether/api/flows/catalogue/events and /actions and /templates are live.
	if rec := doAPI(http.MethodGet, "/ether/api/flows/catalogue/events", nil); rec.Code != http.StatusOK {
		t.Fatalf("GET catalogue/events = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doAPI(http.MethodGet, "/ether/api/flows/catalogue/actions", nil); rec.Code != http.StatusOK {
		t.Fatalf("GET catalogue/actions = %d: %s", rec.Code, rec.Body.String())
	} else if !bytes.Contains(rec.Body.Bytes(), []byte(`"notify.send"`)) {
		t.Fatalf("expected notify.send in actions catalogue: %s", rec.Body.String())
	}
	if rec := doAPI(http.MethodGet, "/ether/api/flows/templates", nil); rec.Code != http.StatusOK {
		t.Fatalf("GET templates = %d: %s", rec.Code, rec.Body.String())
	}
	// Verify POST /ether/api/flows/describe returns 409 ai_unavailable when LLM is off.
	if rec := doAPI(http.MethodPost, "/ether/api/flows/describe", map[string]any{"prompt": "Notify me on backup failure"}); rec.Code != http.StatusConflict || !bytes.Contains(rec.Body.Bytes(), []byte(`"ai_unavailable"`)) {
		t.Fatalf("POST describe without LLM = %d: %s", rec.Code, rec.Body.String())
	}
	// Verify POST /ether/api/flows/test executes an unsaved draft graph dry without incrementing live actionCalls.
	draftTestRec := doAPI(http.MethodPost, "/ether/api/flows/test", map[string]any{
		"dryRun": true,
		"event":  map[string]any{"type": events.EventSystemBackupFailed},
		"graph": flows.Graph{
			SchemaVersion: flows.SchemaVersion,
			Nodes: []flows.Node{
				{ID: "t1", Kind: flows.NodeTriggerEvent, Config: flows.MustMarshalConfig(flows.EventTriggerConfig{Events: []string{events.EventSystemBackupFailed}})},
				{ID: "a1", Kind: flows.NodeAction, Config: flows.MustMarshalConfig(flows.ActionNodeConfig{
					ActionType: "notify.send",
					Params:     map[string]any{"channelIds": []string{"discord-1"}, "title": "Draft", "message": "{{ payload.error }}"},
				})},
			},
			Edges: []flows.Edge{{ID: "e1", Source: "t1", SourceHandle: flows.HandleNext, Target: "a1"}},
		},
	})
	if draftTestRec.Code != http.StatusOK || !bytes.Contains(draftTestRec.Body.Bytes(), []byte(`"dryRun":true`)) {
		t.Fatalf("POST /ether/api/flows/test = %d: %s", draftTestRec.Code, draftTestRec.Body.String())
	}
	if actionCalls.Load() != 0 {
		t.Fatalf("draft dry test must not trigger live action, calls=%d", actionCalls.Load())
	}

	// 2. Create a flow via /ether/api/flows listening on system.backup.failed.
	createRec := doAPI(http.MethodPost, "/ether/api/flows", map[string]any{
		"name":    "Backup alert",
		"enabled": true,
		"graph": flows.Graph{
			SchemaVersion: flows.SchemaVersion,
			Nodes: []flows.Node{
				{
					ID:     "t1",
					Kind:   flows.NodeTriggerEvent,
					Label:  "Backup failed",
					Config: flows.MustMarshalConfig(flows.EventTriggerConfig{Events: []string{events.EventSystemBackupFailed}}),
				},
				{
					ID:    "a1",
					Kind:  flows.NodeAction,
					Label: "Notify",
					Config: flows.MustMarshalConfig(flows.ActionNodeConfig{
						ActionType: "notify.send",
						Params: map[string]any{
							"channelIds":         []string{"discord-1"},
							"title":              "Backup failed",
							"message":            "{{ event.payload.error }}",
							"credential":         "fixture-value-original",
							"optionalCredential": "",
						},
					}),
				},
			},
			Edges: []flows.Edge{{ID: "e1", Source: "t1", SourceHandle: flows.HandleNext, Target: "a1"}},
		},
	})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("POST /ether/api/flows = %d: %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		Flow flows.Flow `json:"flow"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	flowID := created.Flow.ID

	// Start the Flows engine (leader-only via cluster.RunAsLeader).
	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		a.runFlows(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-engineDone
	})

	// The startup reload sees the saved flow, and the live event must wait
	// until the runtime's bus subscription is installed.
	waitUntil(t, "Flows event subscription", func() bool {
		for _, sub := range a.EventBus().Stats().Subscribers {
			if sub.Name == "flows-runtime" {
				return true
			}
		}
		return false
	})
	a.publishEvent(events.EventSystemBackupFailed, events.SystemBackupFailedPayload{
		Kind:                "scheduled",
		Error:               "disk full",
		ConsecutiveFailures: 1,
	}, events.WithSource("backup"))

	select {
	case params := <-actionDone:
		if params["message"] != "disk full" {
			t.Fatalf("expected rendered message 'disk full', got %v", params["message"])
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for Flows engine to execute action on system.backup.failed event")
	}
	if actionCalls.Load() != 1 {
		t.Fatalf("expected 1 live action execution, got %d", actionCalls.Load())
	}

	// The action callback fires before the runtime persists its result.
	waitUntil(t, "persisted Flows run", func() bool {
		runs, _, err := a.FlowsStore().ListRuns(ctx, flows.RunFilter{FlowID: flowID, Status: flows.RunCompleted})
		if err != nil {
			t.Fatal(err)
		}
		return len(runs) > 0
	})

	// 4. Verify /ether/api/flows/{id}/runs returns the completed run.
	runsRec := doAPI(http.MethodGet, "/ether/api/flows/"+flowID+"/runs", nil)
	if runsRec.Code != http.StatusOK {
		t.Fatalf("GET runs = %d: %s", runsRec.Code, runsRec.Body.String())
	}
	if !bytes.Contains(runsRec.Body.Bytes(), []byte(events.EventSystemBackupFailed)) {
		t.Fatalf("expected system.backup.failed run in %s", runsRec.Body.String())
	}

	// 5. Exercise the MCP tools through a.extendRT.tools against the real App!
	owner := tools.Principal{ViewerID: ownerID, IsOwner: true, Surface: "mcp", Scopes: []string{"full"}}
	callMCP := func(name string, in map[string]any) tools.Result {
		t.Helper()
		tl, ok := a.extendRT.tools.Lookup(ctx, owner, name)
		if !ok {
			t.Fatalf("MCP tool %q not registered", name)
		}
		res, err := tl.Execute(ctx, owner, in)
		if err != nil {
			t.Fatalf("MCP tool %q failed: %v", name, err)
		}
		return res
	}

	listRes := callMCP("flows_list", map[string]any{"includeTemplates": true})
	if len(listRes.Data["flows"].([]map[string]any)) != 1 {
		t.Fatalf("expected 1 flow in flows_list, got %+v", listRes.Data)
	}
	if len(listRes.Data["templates"].([]map[string]any)) == 0 {
		t.Fatal("expected starter templates in flows_list")
	}

	getRes := callMCP("flow_get", map[string]any{"flowId": flowID})
	if getRes.Data["flow"].(map[string]any)["id"] != flowID {
		t.Fatalf("unexpected flow_get result: %+v", getRes.Data)
	}

	reader := owner
	reader.Scopes = []string{"read"}
	getFlow, ok := a.extendRT.tools.Lookup(ctx, reader, "flow_get")
	if !ok {
		t.Fatal("flow_get unavailable to read scope")
	}
	view, err := getFlow.Execute(ctx, reader, map[string]any{"flowId": flowID})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(view.Data["flow"])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("fixture-value-original")) {
		t.Fatal("flow output exposed an action credential")
	}
	var edit flows.Flow
	if err := json.Unmarshal(raw, &edit); err != nil {
		t.Fatal(err)
	}
	masked, err := flows.DecodeConfig[flows.ActionNodeConfig](edit.Graph.Nodes[1])
	if err != nil || masked.Params["credential"] != events.RedactedValue || masked.Params["optionalCredential"] != nil {
		t.Fatal("flow output did not distinguish set and unset credentials")
	}

	current, err := a.FlowsStore().GetFlow(ctx, flowID)
	if err != nil {
		t.Fatal(err)
	}
	action, err := flows.DecodeConfig[flows.ActionNodeConfig](current.Graph.Nodes[1])
	if err != nil {
		t.Fatal(err)
	}
	action.Params["credential"] = "fixture-value-rotated"
	current.Graph.Nodes[1].Config = flows.MustMarshalConfig(action)
	if _, err := a.FlowsStore().SaveFlow(ctx, *current); err != nil {
		t.Fatal(err)
	}
	saved, err := a.FlowsStore().SaveFlow(ctx, edit)
	if err != nil {
		t.Fatal(err)
	}
	action, err = flows.DecodeConfig[flows.ActionNodeConfig](saved.Graph.Nodes[1])
	if err != nil || action.Params["credential"] != "fixture-value-rotated" {
		t.Fatal("saving a masked graph did not retain the current credential")
	}

	// Dry-run test via MCP tool executes immediately without mutating live counter.
	testRes := callMCP("flow_test_run", map[string]any{
		"flowId":    flowID,
		"dryRun":    true,
		"eventType": events.EventSystemBackupFailed,
		"payload":   map[string]any{"kind": "manual", "error": "simulated", "consecutiveFailures": 1},
	})
	if testRes.Proposal != nil {
		t.Fatal("dry-run flow_test_run should not require approval")
	}
	if actionCalls.Load() != 1 {
		t.Fatalf("dry-run should not trigger live action, calls=%d", actionCalls.Load())
	}

	// Disable via MCP tool (proposal -> apply).
	enRes := callMCP("flow_enable", map[string]any{"flowId": flowID, "enabled": false, "reason": "pause test"})
	if enRes.Proposal == nil {
		t.Fatal("flow_enable must return a proposal")
	}
	if _, err := enRes.Proposal.Apply(ctx); err != nil {
		t.Fatalf("apply flow_enable: %v", err)
	}
	updated, err := a.FlowsStore().GetFlow(ctx, flowID)
	if err != nil || updated.Enabled {
		t.Fatalf("expected flow disabled after apply, got err=%v enabled=%v", err, updated.Enabled)
	}

	// Verify flow_runs lists runs via MCP tool.
	mcpRuns := callMCP("flow_runs", map[string]any{"flowId": flowID})
	if mcpRuns.Data["count"].(int) < 1 {
		t.Fatalf("expected >= 1 run in flow_runs, got %+v", mcpRuns.Data)
	}

	// Create a second flow from a starter template via MCP (proposal -> apply).
	tmplRes := callMCP("flow_create_from_template", map[string]any{
		"templateId": "notify-updates",
		"name":       "Release watcher",
		"params":     map[string]any{"channelIds": []any{"discord-1"}},
	})
	if tmplRes.Proposal == nil {
		t.Fatal("flow_create_from_template must return a proposal")
	}
	applied, err := tmplRes.Proposal.Apply(ctx)
	if err != nil {
		t.Fatalf("apply flow_create_from_template: %v", err)
	}
	if applied.Data["created"] != true {
		t.Fatalf("unexpected apply result: %+v", applied.Data)
	}
	allFlows, err := a.FlowsStore().ListFlows(ctx)
	if err != nil || len(allFlows) != 2 {
		t.Fatalf("expected 2 stored flows after template apply, got err=%v len=%d", err, len(allFlows))
	}
}

func TestFlowsDefaultBuiltinActionsAndPluginsWiring(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.SetupComplete = true
		c.Frontends = map[string]config.Frontend{"plex": {Enabled: false}}
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	a, err := New(ctx, Options{
		DataDir: dir,
		Version: "0.3.17-test",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)

	// Default App wiring registers all 13 built-in Flow actions.
	if got := len(a.FlowsActions().List()); got < 13 {
		t.Fatalf("expected >= 13 built-in Flow actions registered by default, got %d", got)
	}
	for _, want := range []string{"notify.send", "http.webhook", "mqtt.publish", "playback.stop_session", "viewer.restrict", "library.refresh"} {
		if _, ok := a.FlowsActions().Get(want); !ok {
			t.Fatalf("expected built-in Flow action %q registered", want)
		}
	}

	// Plugin Manager receives EventBus/EventRegistry/FlowActions and Manager.Run
	// subscribes the plugins.bridge listener on the shared EventBus.
	if a.pluginsRT == nil || a.pluginsRT.mgr == nil {
		t.Fatal("expected plugins runtime initialized")
	}
	deadline := time.Now().Add(3 * time.Second)
	var foundBridge bool
	for time.Now().Before(deadline) {
		for _, sub := range a.EventBus().Stats().Subscribers {
			if sub.Name == "plugins.bridge" {
				foundBridge = true
				break
			}
		}
		if foundBridge {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !foundBridge {
		t.Fatalf("expected plugins.bridge subscriber on EventBus, got %+v", a.EventBus().Stats().Subscribers)
	}
}
