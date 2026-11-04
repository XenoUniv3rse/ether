package studio

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestPreviewRejectsUndeclaredCatalogueAndSecrets(t *testing.T) {
	st, _, _ := newTestStudio(t)
	p, err := st.CreateProject(context.Background(), CreateProjectOptions{Template: "list", Name: "Permission probe"})
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal([]byte(p.Files["manifest.json"]), &manifest); err != nil {
		t.Fatal(err)
	}
	ether := manifest["ether"].(map[string]any)
	ether["permissions"] = map[string]any{"catalog": "none"}
	raw, _ := json.Marshal(manifest)
	st.projects[p.ID].Files["manifest.json"] = string(raw)
	host := &devHost{s: st, projectID: p.ID}
	for _, name := range []string{"catalog.search", "catalog.get", "secret.get"} {
		if _, err := host.CallHost(context.Background(), name, json.RawMessage(`["synthetic-marker"]`)); err == nil || !strings.Contains(err.Error(), "not declared") {
			t.Fatalf("%s should reject undeclared access, got %v", name, err)
		}
	}
	ether["permissions"] = map[string]any{"catalog": "read", "secrets": []string{"synthetic-marker"}}
	raw, _ = json.Marshal(manifest)
	st.projects[p.ID].Files["manifest.json"] = string(raw)
	if _, err := host.CallHost(context.Background(), "catalog.search", json.RawMessage(`["synthetic-marker",1,0]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := host.CallHost(context.Background(), "secret.get", json.RawMessage(`["synthetic-marker"]`)); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("declared preview secret should clearly report unavailable: %v", err)
	}
}
