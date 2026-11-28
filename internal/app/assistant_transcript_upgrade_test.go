package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/agent"
	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/flows"
	"github.com/braedonsaunders/ether/internal/flows/actions"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestAssistantTranscriptUpgrade(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	reg := flows.NewActionRegistry()
	reg.MustRegister(actions.NewHTTPWebhook(actions.Deps{}))
	flowStore := flows.NewSQLStore(db, flows.WithStoreActions(reg))
	a := &App{db: db, flowsRT: &flowsRuntime{store: flowStore, actions: reg}}

	const token = `fixture-"quoted"\token`
	const previousToken = "fixture-rotated"
	const unrelated = "An ordinary note about the webhook."
	graph := func(value string) flows.Graph {
		return flows.Graph{SchemaVersion: flows.SchemaVersion, Nodes: []flows.Node{
			{ID: "hook", Kind: flows.NodeAction, Config: flows.MustMarshalConfig(flows.ActionNodeConfig{
				ActionType: "http.webhook", Params: map[string]any{"bearerToken": value, "url": "https://example.test/hook"},
			})},
		}}
	}
	flow, err := flowStore.SaveFlow(ctx, flows.Flow{Name: "Test webhook", Graph: graph(token)})
	if err != nil {
		t.Fatal(err)
	}

	assistant := agent.NewStore(db)
	thread, err := assistant.CreateThread(ctx, unrelated)
	if err != nil {
		t.Fatal(err)
	}
	historical := *flow
	historical.Graph = graph(previousToken)
	parts, err := json.Marshal([]map[string]any{
		{"type": "tool-flow_get", "output": map[string]any{"flow": historical}},
		{"type": "text", "text": "Result: " + token},
		{"type": "text", "text": unrelated},
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := assistant.AppendMessage(ctx, thread.ID, "assistant", parts)
	if err != nil {
		t.Fatal(err)
	}
	unrelatedParts := json.RawMessage(`[ { "type": "text", "text": "An ordinary note about the webhook." } ]`)
	untouched, err := assistant.AppendMessage(ctx, thread.ID, "user", unrelatedParts)
	if err != nil {
		t.Fatal(err)
	}
	if err := assistant.Audit(ctx, "", "web_fetch", "mcp:viewer:1", "error", "Request failed: "+token, time.Second); err != nil {
		t.Fatal(err)
	}
	approval, err := assistant.CreateApproval(ctx, agent.Approval{Input: map[string]any{"name": token}, Description: "Inspect " + token})
	if err != nil {
		t.Fatal(err)
	}

	if changed, err := a.migrateAssistantTranscripts(ctx); err != nil || changed == 0 {
		t.Fatalf("transcript upgrade changed %d fields: %v", changed, err)
	}
	getParts := func(id string) string {
		t.Helper()
		var got string
		if err := db.QueryRowContext(ctx, "SELECT parts FROM agent_messages WHERE id=?", id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	masked := getParts(msg.ID)
	if !json.Valid([]byte(masked)) || strings.Count(masked, events.RedactedValue) != 2 ||
		!strings.Contains(masked, unrelated) || !strings.Contains(masked, "https://example.test/hook") ||
		strings.Contains(masked, previousToken) || strings.Contains(masked, "fixture-") {
		t.Fatal("stored assistant parts were not masked as expected")
	}
	if getParts(untouched.ID) != string(unrelatedParts) {
		t.Fatal("unrelated transcript changed")
	}

	var audit string
	if err := db.QueryRowContext(ctx, "SELECT error FROM agent_audit WHERE principal='mcp:viewer:1'").Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if audit != "Request failed: "+events.RedactedValue {
		t.Fatal("audit field was not masked")
	}
	saved, ok, err := assistant.GetApproval(ctx, approval.ID)
	if err != nil || !ok || saved.Input["name"] != events.RedactedValue || saved.Description != "Inspect "+events.RedactedValue {
		t.Fatal("stored approval was not masked")
	}
	storedFlow, err := flowStore.GetFlow(ctx, flow.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := flows.DecodeConfig[flows.ActionNodeConfig](storedFlow.Graph.Nodes[0])
	if err != nil || cfg.Params["bearerToken"] != token {
		t.Fatal("upgrade changed the flow definition")
	}
	if changed, err := a.migrateAssistantTranscripts(ctx); err != nil || changed != 0 || getParts(msg.ID) != masked {
		t.Fatalf("second upgrade changed %d fields: %v", changed, err)
	}
}
