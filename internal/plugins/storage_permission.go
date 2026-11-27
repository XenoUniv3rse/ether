package plugins

import (
	"bytes"
	"encoding/json"
)

// StoragePermission is permissions.storage: "", "none" or "durable".
// Manifests written before this permission existed sometimes carried an
// object here ({"kv": true}), which Ether ignored; it still parses, as
// "" (no durable storage), unless the object says durable.
type StoragePermission string

func (p *StoragePermission) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '{' {
		var obj struct {
			Durable bool `json:"durable"`
		}
		if err := json.Unmarshal(b, &obj); err != nil {
			return err
		}
		if obj.Durable {
			*p = "durable"
		} else {
			*p = ""
		}
		return nil
	}
	if string(b) == "null" {
		*p = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*p = StoragePermission(s)
	return nil
}
