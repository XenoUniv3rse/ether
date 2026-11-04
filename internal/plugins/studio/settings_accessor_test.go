package studio

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSettingsAccessorReadsSavedDraftWithoutSecrets(t *testing.T) {
	st, _, _ := newTestStudio(t)
	ctx := context.Background()
	p, err := st.CreateProject(ctx, CreateProjectOptions{Template: "list", Name: "Settings test"})
	if err != nil {
		t.Fatal(err)
	}
	var man map[string]any
	_ = json.Unmarshal([]byte(p.Files["manifest.json"]), &man)
	ether := man["ether"].(map[string]any)
	ether["settings"] = map[string]any{"$ref": "settings.schema.json"}
	ether["permissions"].(map[string]any)["secrets"] = []string{"declaredSecret"}
	raw, _ := json.Marshal(man)
	files := map[string]string{
		"manifest.json":        string(raw),
		"settings.schema.json": `{"fields":[{"key":"schemaSecret","kind":"secret","label":"Secret"},{"key":"maxItems","kind":"number","label":"Maximum"}]}`,
		"src/plugin.ts":        `export function handle() { return {status:200, body:ether.settings.get()}; } (globalThis as any).handle = handle;`,
	}
	_, compiled, err := st.SaveFiles(ctx, p.ID, files, nil, map[string]any{"maxItems": 7, "schemaSecret": "synthetic-one", "declaredSecret": "synthetic-two"})
	if err != nil || !compiled.OK {
		t.Fatalf("save: %v %+v", err, compiled)
	}
	result, err := st.RunProject(ctx, p.ID, RunRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(result.Body, &settings); err != nil {
		t.Fatal(err)
	}
	if result.Status != 200 || settings["maxItems"] != float64(7) || len(settings) != 1 {
		t.Fatalf("unexpected settings: status=%d body=%s", result.Status, result.Body)
	}
}
