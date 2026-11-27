package plugins

import (
	"context"
	"net/http"
)

// Adapter reads can arrive at playback cadence. Persist their best-effort
// Inspector samples on the scheduler instead of blocking each client on the
// database writer. Bound the buffer so a stalled writer cannot exhaust RAM.
func (m *Manager) recordCall(ctx context.Context, c CallRecord) {
	if isFrontendAdapterPath(c.Route) && (c.Method == http.MethodGet || c.Method == http.MethodHead) {
		c.Created = m.now()
		c.Request = truncate(c.Request, 8192)
		c.Response = truncate(c.Response, 8192)
		select {
		case m.adapterCalls <- c:
		default:
		}
		return
	}
	m.store.addCall(ctx, c)
}

func (m *Manager) flushAdapterCalls(ctx context.Context) {
	for range len(m.adapterCalls) {
		if ctx.Err() != nil {
			return
		}
		m.store.addCall(ctx, <-m.adapterCalls)
	}
}
