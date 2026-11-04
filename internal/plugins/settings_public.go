package plugins

import "encoding/json"

// NonSecretSettings applies declared defaults, then stored public settings.
// Explicit zero, false, empty and null values take precedence over defaults.
// Studio uses the same secret field classifier as installed settings storage.
func NonSecretSettings(settings map[string]any, schema json.RawMessage, declaredSecrets []string) map[string]any {
	secretNames := secretFieldNames(schema)
	for _, key := range declaredSecrets {
		secretNames[key] = true
	}
	out := make(map[string]any, len(settings))
	var doc map[string]any
	if json.Unmarshal(schema, &doc) == nil {
		visitSettingsFields(doc, func(key string, field map[string]any) {
			if field["kind"] == "secret" || field["type"] == "secret" {
				secretNames[key] = true
			}
		})
		visitSettingsFields(doc, func(key string, field map[string]any) {
			if value, exists := field["default"]; exists && !secretNames[key] {
				out[key] = value
			}
		})
	}
	for key, value := range settings {
		if !secretNames[key] {
			out[key] = value
		}
	}
	return out
}

// Only traverse the schema's supported field containers, never default values or
// arbitrary nested objects that happen to contain a key/default pair.
func visitSettingsFields(doc map[string]any, visit func(string, map[string]any)) {
	for _, container := range []string{"fields", "groups"} {
		entries, _ := doc[container].([]any)
		for _, entry := range entries {
			field, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if container == "fields" && field["kind"] != "group" {
				if key, ok := firstString(field, "key", "name", "id", "field"); ok {
					visit(key, field)
				}
			}
			visitSettingsFields(field, visit)
		}
	}
	properties, _ := doc["properties"].(map[string]any)
	for key, raw := range properties {
		if field, ok := raw.(map[string]any); ok {
			visit(key, field)
		}
	}
}

func settingsSecrets(rec Record) []string {
	if rec.Manifest.Ether == nil {
		return nil
	}
	return rec.Manifest.Ether.Permissions.Secrets
}
