package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/llm"
)

func (s *Service) DescribeCollectionAvailable() bool {
	return s != nil && s.store != nil && s.catalogue != nil && s.Status().Enabled
}

// DescribeCollection runs the console's exact Describe it route and model for
// one headless turn. Its isolated catalogue enforces the route's read-only tool
// boundary even if a model calls a tool that was never offered. The temporary
// transcript is removed after the loop unwinds, including on cancellation.
func (s *Service) DescribeCollection(ctx context.Context, query string) (spec *core.CollectionSpec, err error) {
	if !s.DescribeCollectionAvailable() {
		return nil, fmt.Errorf("collection agent is not available")
	}
	ctx = core.WithoutCatalogViewer(ctx)
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	p := tools.Principal{IsOwner: true, Surface: "console"}
	catalogue := tools.NewCatalogue()
	for _, tool := range s.catalogue.Visible(ctx, p) {
		if tool.ReadOnly && routeTools[DescribeCollectionRoute][tool.Name] {
			if err := catalogue.Register(tool); err != nil {
				return nil, err
			}
		}
	}
	if _, ok := catalogue.Lookup(ctx, p, "collection_draft"); !ok {
		return nil, fmt.Errorf("collection draft tool is not available")
	}
	// Share the configured model, prompts, token budget and actual tools. Cap
	// this headless turn at four model steps, keeping its last successful draft.
	runner := New(Options{Model: s.model, Catalogue: catalogue, Store: s.store, Logger: s.log,
		MaxSteps: min(s.maxSteps, 4), TurnTimeout: s.timeout, TokenBudget: s.budget})
	runner.headlessDescribe = true
	thread, err := s.store.CreateThread(ctx, threadTitle(query))
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if cleanupErr := s.store.DeleteThread(cleanup, thread.ID); cleanupErr != nil {
			spec = nil
			err = errors.Join(err, fmt.Errorf("remove headless collection transcript: %w", cleanupErr))
		}
	}()
	if _, err = s.store.AppendMessage(ctx, thread.ID, "user", marshalParts([]part{{"type": "text", "text": query}})); err != nil {
		return nil, err
	}
	calls := map[string]string{}
	var turnErr error
	emit := func(c Chunk) error {
		switch c["type"] {
		case "tool-input-available":
			id, _ := c["toolCallId"].(string)
			name, _ := c["toolName"].(string)
			calls[id] = name
		case "tool-output-available":
			id, _ := c["toolCallId"].(string)
			if calls[id] == "collection_draft" {
				raw, e := json.Marshal(c["output"])
				var output struct {
					Spec *core.CollectionSpec `json:"spec"`
				}
				if e == nil {
					e = json.Unmarshal(raw, &output)
				}
				if e != nil {
					return e
				}
				if output.Spec != nil {
					spec = output.Spec
				}
			}
		case "error":
			turnErr = fmt.Errorf("collection agent turn failed")
		case "finish":
			if c["finishReason"] != "stop" && !(c["finishReason"] == "tool-calls" && spec != nil) {
				turnErr = fmt.Errorf("collection agent did not finish its turn")
			}
		}
		return nil
	}
	_, err = runner.runAssistant(ctx, p, thread.ID, "", TurnContext{Route: DescribeCollectionRoute},
		[]llm.StreamMessage{{Role: "user", Content: "This is one headless turn with no human follow-up. Finalise with collection_draft without asking questions. If the words describe nothing you can interpret as films or series (random letters, a name that matches nothing), do not draft: reply in one sentence that nothing matches."}}, emit)
	if err = errors.Join(err, turnErr, ctx.Err()); err != nil {
		return nil, err
	}
	if spec == nil {
		// The turn finished without a draft: the agent found nothing the
		// words describe.
		return nil, core.ErrNothingDescribed
	}
	return spec, nil
}
