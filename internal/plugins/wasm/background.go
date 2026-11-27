package wasm

import (
	"context"
	"strings"
	"time"
)

// ViewerRequestPath identifies calls on a viewer's browse/search/playback path.
// Administrative actions, setup, catalogue sync and background jobs use
// owner-granted limits.
func ViewerRequestPath(path string) bool {
	for _, prefix := range []string{"/stream/", "/catalog/", "/meta/", "/subtitles/", "/ether/open", "/ether/search", "/ether/files/", "/ether/metadata/", "/ether/subtitles/", "/ether/markers"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return (strings.HasPrefix(path, "/ether/v1/kinds/") && !KindSyncPath(path)) ||
		strings.HasPrefix(path, "/ether/v1/live/") ||
		strings.HasPrefix(path, "/ether/v1/screens/")
}

// IsSourceSearchPath identifies a stream_source plugin's category search
// (/ether/search) and release file listing (/ether/files/{id}) routes (#792).
func IsSourceSearchPath(path string) bool {
	p, _, _ := strings.Cut(path, "?")
	return p == "/ether/search" || strings.HasPrefix(p, "/ether/files/")
}

// KindSyncPath identifies a media kind's background catalogue calls (browse,
// changes, and expand). Ether makes them in the background to fill a library,
// never while a viewer waits, so they run on the plugin's granted limits (the
// manifest's request, capped only by the owner's ceilings) instead of the
// viewer budget. A big first sync or facet expansion can then take as long as
// it needs.
func KindSyncPath(path string) bool {
	p, _, _ := strings.Cut(path, "?")
	return strings.HasPrefix(p, "/ether/v1/kinds/") &&
		(strings.HasSuffix(p, "/browse") || strings.HasSuffix(p, "/changes") || strings.HasSuffix(p, "/expand"))
}

// LiveTVSyncPath identifies a Live TV source plugin's background lineup and
// guide refresh calls (/ether/channels.json and /ether/guide/*). Ether makes
// them in the background to import channels and programmes, never while a
// viewer waits, so they run on the plugin's granted limits.
func LiveTVSyncPath(path string) bool {
	p, _, _ := strings.Cut(path, "?")
	return p == "/ether/channels.json" || strings.HasPrefix(p, "/ether/guide/")
}

// GrantedLimitsPath reports whether path runs on the plugin's owner-granted
// deadline and unit budget rather than a fixed protocol or viewer cap:
// background kind sync (/browse, /changes, /expand), Live TV lineup and guide sync
// (/ether/channels.json, /ether/guide/*), jobs, setup, owner actions,
// agent tools, and event deliveries.
func GrantedLimitsPath(path string) bool {
	p, _, _ := strings.Cut(path, "?")
	return KindSyncPath(p) ||
		LiveTVSyncPath(p) ||
		strings.HasPrefix(p, "/ether/job/") ||
		strings.HasPrefix(p, "/ether/v1/setup/") ||
		strings.HasPrefix(p, "/ether/action/") ||
		strings.HasPrefix(p, "/ether/tool/") ||
		p == "/ether/event"
}

// BackgroundPoolPath reports whether path is a background job, sync, or event
// delivery call bounded below PoolSize so interactive viewer resolves,
// frontend adapter requests and health checks retain free instances.
func BackgroundPoolPath(path string) bool {
	p, _, _ := strings.Cut(path, "?")
	return strings.HasPrefix(p, "/ether/job/") || KindSyncPath(p) || LiveTVSyncPath(p) || p == "/ether/event"
}

// ResolvePoolPath reports whether path is a playback resolution or stream-open
// call that receives priority access and a reserved pool lane so slow
// frontend-adapter requests or background jobs cannot starve playback.
func ResolvePoolPath(path string) bool {
	p, _, _ := strings.Cut(path, "?")
	if strings.HasPrefix(p, "/ether/open") || strings.HasPrefix(p, "/stream/") || IsSourceSearchPath(p) {
		return true
	}
	return strings.HasPrefix(p, "/ether/v1/kinds/") &&
		(strings.HasSuffix(p, "/resolve") || strings.HasSuffix(p, "/match"))
}

// FrontendAdapterPath reports whether path targets a plugin-contributed
// frontend adapter route (/ether/v1/frontend/*).
func FrontendAdapterPath(path string) bool {
	p, _, _ := strings.Cut(path, "?")
	return strings.HasPrefix(p, "/ether/v1/frontend/") && len(p) > len("/ether/v1/frontend/")
}

// UnitsForDeadline converts a call deadline into its matching governance unit
// budget at one unit per WallUnitInterval (100 µs, or 10,000 units per second),
// so a viewer route's unit budget matches the deadline it is advertised.
func UnitsForDeadline(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64(d / WallUnitInterval)
}

// StremioDeadline is the per-route deadline for a third-party Stremio add-on
// or an add-on-backed plugin (ether.stremio: true). Their stream searches
// routinely take seconds (they search trackers and debrid caches live); the
// caller's own deadline still bounds every call.
func StremioDeadline(path string) time.Duration {
	switch {
	case strings.HasPrefix(path, "/stream/"), strings.HasPrefix(path, "/catalog/"), strings.HasPrefix(path, "/manifest.json"):
		return 20 * time.Second
	case strings.HasPrefix(path, "/meta/"), strings.HasPrefix(path, "/subtitles/"):
		return 10 * time.Second
	default:
		return 30 * time.Second
	}
}

// DeadlineFor returns the baseline per-route deadline from SPEC §5.1.2.
// Routes in GrantedLimitsPath use the plugin's owner-granted deadline instead
// when dispatched through EffectiveDeadline.
func DeadlineFor(path string) time.Duration {
	p, _, _ := strings.Cut(path, "?")
	switch {
	case strings.HasPrefix(p, "/ether/configure"), strings.HasPrefix(p, "/ether/health"):
		return 5 * time.Second
	case strings.HasPrefix(p, "/ether/test"):
		return 15 * time.Second
	case strings.HasPrefix(p, "/ether/options"), strings.HasPrefix(p, "/ether/view"):
		return 2 * time.Second
	case strings.HasPrefix(p, "/ether/open"), strings.HasPrefix(p, "/stream/"):
		return 3 * time.Second
	case IsSourceSearchPath(p):
		// A category search or file listing asks the plugin's own tracker
		// or engine live; the caller's search deadline still bounds it.
		return 10 * time.Second
	case strings.HasPrefix(p, "/ether/tool"):
		return 15 * time.Second
	case strings.HasPrefix(p, "/ether/job"):
		return 5 * time.Minute
	case strings.HasPrefix(p, "/meta/"), strings.HasPrefix(p, "/ether/metadata/"), strings.HasPrefix(p, "/catalog/"), strings.HasPrefix(p, "/ether/event"), strings.HasPrefix(p, "/ether/action"), strings.HasPrefix(p, "/ether/markers"):
		return 10 * time.Second
	case strings.HasPrefix(p, "/subtitles/"), strings.HasPrefix(p, "/ether/subtitles/"):
		return 5 * time.Second
	case strings.HasPrefix(p, "/ether/v1/setup/"):
		return 20 * time.Second
	case strings.HasPrefix(p, "/ether/v1/flow-actions/"):
		return 60 * time.Second
	case strings.HasPrefix(p, "/ether/v1/kinds/"):
		if strings.HasSuffix(p, "/resolve") {
			// Leave room for a parallel indexer fan-out and the guest's
			// matching and source checks within a playback slice.
			return 12 * time.Second
		}
		if strings.HasSuffix(p, "/match") {
			return 5 * time.Second
		}
		return 10 * time.Second
	case strings.HasPrefix(p, "/ether/v1/live/"):
		return 15 * time.Second
	case strings.HasPrefix(p, "/ether/v1/screens/"):
		// A screen's data read or action: a viewer is waiting on it.
		if strings.Contains(p, "/actions/") {
			return 8 * time.Second
		}
		return 4 * time.Second
	default:
		return 30 * time.Second
	}
}

// EffectiveDeadline returns the enforced deadline for path given the plugin's
// granted deadline ceiling and whether it speaks Stremio add-on routes.
func EffectiveDeadline(path string, granted time.Duration, stremio bool) time.Duration {
	if GrantedLimitsPath(path) {
		if granted > 0 {
			return granted
		}
		return DeadlineFor(path)
	}
	route := DeadlineFor(path)
	if stremio {
		route = StremioDeadline(path)
	}
	if granted > 0 && granted < route {
		return granted
	}
	return route
}

// WaitBackground yields before starting work and at each host-service boundary.
func WaitBackground(ctx context.Context, busy func() bool) error {
	for busy != nil && busy() {
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}

func limitsForRequest(l Limits, path string) Limits {
	l.Deadline = EffectiveDeadline(path, l.Deadline, l.Stremio)
	if ViewerRequestPath(path) {
		l.UnitBudget = min(l.UnitBudget, UnitsForDeadline(l.Deadline))
	}
	return l
}
