package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/flows"
	"github.com/braedonsaunders/ether/internal/store"
)

const assistantTranscriptUpgrade = "v0.3.25-action-params"
const assistantUpgradeBatchSize = 64

func init() {
	store.Register("assistant_upgrades", 1, `CREATE TABLE assistant_upgrades (name TEXT PRIMARY KEY);`)
	// A logical restore imports the transcripts after schema setup. Its new
	// database checks the imported rows before opening the assistant.
	store.RegisterLocalTable("assistant_upgrades")
}

// migrateAssistantTranscripts runs after action registration and before Ask
// Ether opens. Keep only fixed error messages here and report counts only.
func (a *App) migrateAssistantTranscripts(ctx context.Context) (int, error) {
	unlock, err := store.SessionLock(ctx, a.db, assistantTranscriptUpgrade)
	if err != nil {
		return 0, errors.New("could not lock the assistant transcript upgrade")
	}
	if unlock != nil {
		defer unlock()
	}
	var marker string
	err = a.db.QueryRowContext(ctx, `SELECT name FROM assistant_upgrades WHERE name=?`, assistantTranscriptUpgrade).Scan(&marker)
	switch {
	case err == nil:
		return 0, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, errors.New("could not check the assistant transcript upgrade")
	}

	keys, err := a.assistantActionSecretKeys(ctx)
	if err != nil {
		return 0, errors.New("could not read action schemas for the assistant transcript upgrade")
	}
	scrub, err := newAssistantTranscriptScrub(ctx, a.db, keys)
	if err != nil {
		return 0, errors.New("could not read flow parameters for the assistant transcript upgrade")
	}
	changed, err := scrub.run(ctx, a.db)
	if err != nil {
		return changed, errors.New("could not update stored assistant records")
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO assistant_upgrades(name) VALUES(?) ON CONFLICT(name) DO NOTHING`, assistantTranscriptUpgrade); err != nil {
		return changed, errors.New("could not record the assistant transcript upgrade")
	}
	return changed, nil
}

func (a *App) assistantActionSecretKeys(ctx context.Context) (map[string][]string, error) {
	keys := map[string][]string{}
	for _, spec := range a.FlowsActions().List() {
		for _, param := range spec.Params {
			if param.Secret {
				keys[spec.Type] = append(keys[spec.Type], param.Key)
			}
		}
	}
	if a.pluginsRT != nil {
		records, err := a.pluginsRT.mgr.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			if record.Manifest.Ether == nil {
				continue
			}
			for _, action := range record.Manifest.Ether.Contributes.FlowActions {
				actionType := flows.PluginActionType(record.ID, action.Key)
				for _, param := range action.Params {
					if param.Secret && !slices.Contains(keys[actionType], param.Key) {
						keys[actionType] = append(keys[actionType], param.Key)
					}
				}
			}
		}
	}
	return keys, nil
}

type assistantTranscriptScrub struct {
	keys     map[string][]string
	literals *strings.Replacer
}

func newAssistantTranscriptScrub(ctx context.Context, db *sql.DB, keys map[string][]string) (assistantTranscriptScrub, error) {
	values := map[string]struct{}{}
	err := scanAssistantUpgradeRows(ctx, db, "flows_definitions", []string{"graph_json"}, false, func(_ *sql.Tx, _ string, cells []string) error {
		var graph flows.Graph
		if err := json.Unmarshal([]byte(cells[0]), &graph); err != nil {
			return err
		}
		for _, node := range graph.Nodes {
			if node.Kind != flows.NodeAction {
				continue
			}
			cfg, err := flows.DecodeConfig[flows.ActionNodeConfig](node)
			if err != nil {
				return err
			}
			for _, key := range keys[cfg.ActionType] {
				if value, ok := cfg.Params[key].(string); ok && value != "" && value != events.RedactedValue {
					values[value] = struct{}{}
				}
			}
		}
		return nil
	})
	if err != nil {
		return assistantTranscriptScrub{}, err
	}
	literals := make([]string, 0, len(values))
	for value := range values {
		literals = append(literals, value)
	}
	slices.SortFunc(literals, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	// Preserve masks during a retry and prefer longer matching values.
	pairs := []string{events.RedactedValue, events.RedactedValue}
	for _, value := range literals {
		pairs = append(pairs, value, events.RedactedValue)
	}
	return assistantTranscriptScrub{keys: keys, literals: strings.NewReplacer(pairs...)}, nil
}

func (s assistantTranscriptScrub) run(ctx context.Context, db *sql.DB) (int, error) {
	changes := 0
	for _, spec := range []struct {
		table string
		text  []string
		json  []string
	}{
		{table: "agent_messages", json: []string{"parts"}},
		{table: "agent_audit", text: []string{"error"}},
		{table: "agent_approvals", text: []string{"category", "description", "decision_note", "failure_reason"}, json: []string{"input", "details"}},
		{table: "agent_threads", text: []string{"title"}},
		{table: "agent_secret_requests", text: []string{"provider_label", "credential_label", "purpose", "help_url"}},
	} {
		columns := append(slices.Clone(spec.text), spec.json...)
		if err := scanAssistantUpgradeRows(ctx, db, spec.table, columns, true, func(tx *sql.Tx, id string, cells []string) error {
			for i, before := range cells {
				var after string
				if i < len(spec.text) {
					after = s.literals.Replace(before)
				} else {
					after = s.rewriteJSON(before, spec.table == "agent_messages")
				}
				if after == before {
					continue
				}
				column := columns[i]
				if _, err := tx.ExecContext(ctx, "UPDATE "+spec.table+" SET "+column+"=? WHERE id=?", after, id); err != nil {
					return err
				}
				changes++
			}
			return nil
		}); err != nil {
			return changes, err
		}
	}
	return changes, nil
}

// scanAssistantUpgradeRows reads and commits one primary-key page at a time.
// Table and column names are fixed by the callers.
func scanAssistantUpgradeRows(ctx context.Context, db *sql.DB, table string, columns []string, write bool, visit func(*sql.Tx, string, []string) error) error {
	query := "SELECT id," + strings.Join(columns, ",") + " FROM " + table + " WHERE id>? ORDER BY id LIMIT ?"
	if write {
		query += store.ForUpdate(store.DialectOf(db))
	}
	type row struct {
		id    string
		cells []string
	}
	var cursor string
	for {
		count, err := func() (int, error) {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return 0, err
			}
			defer tx.Rollback()
			rows, err := tx.QueryContext(ctx, query, cursor, assistantUpgradeBatchSize)
			if err != nil {
				return 0, err
			}
			var page []row
			for rows.Next() {
				item := row{cells: make([]string, len(columns))}
				dest := []any{&item.id}
				for i := range item.cells {
					dest = append(dest, &item.cells[i])
				}
				if err := rows.Scan(dest...); err != nil {
					rows.Close()
					return 0, err
				}
				page = append(page, item)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return 0, err
			}
			for _, item := range page {
				if err := visit(tx, item.id, item.cells); err != nil {
					return 0, err
				}
				cursor = item.id
			}
			return len(page), tx.Commit()
		}()
		if err != nil || count < assistantUpgradeBatchSize {
			return err
		}
	}
}

func (s assistantTranscriptScrub) rewriteJSON(raw string, transcript bool) string {
	if !json.Valid([]byte(raw)) {
		return s.literals.Replace(raw)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var v any
	if err := decoder.Decode(&v); err != nil {
		return raw
	}

	changed := false
	if parts, ok := v.([]any); transcript && ok {
		for _, item := range parts {
			p, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if p["type"] == "tool-flow_get" {
				changed = s.maskFlowToolOutput(p) || changed
			}
			for key, value := range p {
				switch key {
				case "type", "state", "status", "toolCallId", "callSignature", "requestId", "approvalId":
					continue
				}
				next, masked := s.rewriteValue(value)
				if masked {
					p[key], changed = next, true
				}
			}
		}
	} else {
		v, changed = s.rewriteValue(v)
	}
	if !changed {
		return raw
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return string(encoded)
}

// A stored flow view can still carry a value that has since been changed
// in the definition. Mask its declared secret fields directly.
func (s assistantTranscriptScrub) maskFlowToolOutput(p map[string]any) bool {
	output, _ := p["output"].(map[string]any)
	flow, _ := output["flow"].(map[string]any)
	graph, _ := flow["graph"].(map[string]any)
	nodes, _ := graph["nodes"].([]any)
	changed := false
	for _, item := range nodes {
		node, _ := item.(map[string]any)
		if node["kind"] != string(flows.NodeAction) {
			continue
		}
		config, _ := node["config"].(map[string]any)
		actionType, _ := config["actionType"].(string)
		params, _ := config["params"].(map[string]any)
		for _, key := range s.keys[actionType] {
			value := params[key]
			if value != nil && value != "" && value != events.RedactedValue {
				params[key], changed = events.RedactedValue, true
			}
		}
	}
	return changed
}

func (s assistantTranscriptScrub) rewriteValue(value any) (any, bool) {
	changed := false
	switch v := value.(type) {
	case string:
		masked := s.literals.Replace(v)
		return masked, masked != v
	case map[string]any:
		for key, item := range v {
			if next, masked := s.rewriteValue(item); masked {
				v[key], changed = next, true
			}
		}
	case []any:
		for i, item := range v {
			if next, masked := s.rewriteValue(item); masked {
				v[i], changed = next, true
			}
		}
	}
	return value, changed
}
