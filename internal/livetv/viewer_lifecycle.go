package livetv

import "github.com/braedonsaunders/ether/internal/store"

// Per-viewer lifecycle for this component's tables: how a viewer's rows
// are purged on removal and moved on merge. See store.RegisterViewerTable.
func init() {
	store.RegisterViewerTable("livetv", "livetv_policies", "user_id", store.ViewerMergeMove)
	// Catch-up watch state moves with the viewer; on a collision the target keeps its own.
	store.RegisterViewerTable("livetv", "livetv_catchup_watch", "user_id", store.ViewerMergeMove, "programme_id")
}
