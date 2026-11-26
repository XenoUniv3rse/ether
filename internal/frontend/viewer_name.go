package frontend

import (
	"strconv"
	"strings"
)

// ViewerDisplayName labels authenticated viewers, including invite and code
// sign-ins that don't yet have a username. It never supplies a credential.
func ViewerDisplayName(v *Viewer) string {
	if name := strings.TrimSpace(v.Username); name != "" {
		return name
	}
	if title := strings.TrimSpace(v.Title); title != "" {
		return title
	}
	return strconv.FormatInt(v.ID, 10)
}
