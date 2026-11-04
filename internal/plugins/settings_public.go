package plugins

import "encoding/json"

// NonSecretSettings copies public settings, excluding declared and schema secrets.
// Studio uses the same secret field classifier as installed settings storage.
func NonSecretSettings(settings map[string]any, schema json.RawMessage, declaredSecrets []string) map[string]any {
	secretNames := secretFieldNames(schema)
	for _, key := range declaredSecrets {
		secretNames[key] = true
	}
	out := make(map[string]any, len(settings))
	for key, value := range settings {
		if !secretNames[key] {
			out[key] = value
		}
	}
	return out
}
