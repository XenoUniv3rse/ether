package plugins

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestSettingsDefaultsPreserveExplicitValuesAndExcludeSecrets(t *testing.T) {
	schema := json.RawMessage(`{"fields":[{"key":"max","kind":"number","default":20},{"key":"zero","default":9},{"key":"off","default":true},{"key":"empty","default":"fallback"},{"key":"null","default":"fallback"},{"key":"token","kind":"secret","default":"synthetic"},{"key":"declared","default":"synthetic"},{"key":"object","default":{"key":"injected","default":123}}],"groups":[{"fields":[{"key":"year","default":1950}]}],"properties":{"label":{"type":"string","default":"hello"},"propertySecret":{"kind":"secret","default":"synthetic"}}}`)
	stored := map[string]any{"zero": 0, "off": false, "empty": "", "null": nil, "token": "stored-secret", "declared": "stored-secret", "propertySecret": "stored-secret"}
	got := NonSecretSettings(stored, schema, []string{"declared"})
	want := map[string]any{"max": float64(20), "year": float64(1950), "label": "hello", "zero": 0, "off": false, "empty": "", "null": nil, "object": map[string]any{"key": "injected", "default": float64(123)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settings=%#v", got)
	}
	if len(stored) != 7 {
		t.Fatal("mutated stored settings")
	}
}

func TestInstalledSettingsDefaultsAgreeAcrossSetupRuntimeAndConfigure(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	rec := env.installHello(t, "1.0.0")
	row, err := env.m.store.settings(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.Schema = json.RawMessage(`{"fields":[{"key":"maxItems","kind":"number","required":true,"default":20},{"key":"minimumYear","kind":"number","default":1950},{"key":"token","kind":"secret","default":"must-not-appear"}]}`)
	row.Settings = map[string]any{}
	if err := env.m.store.putSettings(ctx, rec.ID, row); err != nil {
		t.Fatal(err)
	}
	setup, _, err := env.m.GetSettings(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := env.m.HostFor(rec.ID).Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := env.m.mergedSettings(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"maxItems": float64(20), "minimumYear": float64(1950)}
	for _, got := range []map[string]any{setup, runtime, delivered} {
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("settings=%#v", got)
		}
	}
	if _, err := env.m.Configure(ctx, rec.ID, map[string]any{"maxItems": 0}); err != nil {
		t.Fatal(err)
	}
	runtime, err = env.m.HostFor(rec.ID).Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runtime["maxItems"] != float64(0) || runtime["minimumYear"] != float64(1950) {
		t.Fatalf("configured=%#v", runtime)
	}
}
