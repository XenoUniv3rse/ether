package plugins

import (
	"context"
	"net/http"
	"strings"
)

// Client reads can arrive at playback cadence. Persist their best-effort
// Inspector samples on the scheduler to keep database writes off the read
// path. Bound the buffer so a stalled writer cannot exhaust RAM.
func (m *Manager) recordCall(ctx context.Context, c CallRecord) {
	adapterRead := isFrontendAdapterPath(c.Route) && (c.Method == http.MethodGet || c.Method == http.MethodHead)
	// Kind lyrics and live listings use POST to carry read parameters.
	kindRead := c.Method == http.MethodPost && strings.HasPrefix(c.Route, "/ether/v1/kinds/") &&
		(strings.HasSuffix(c.Route, "/lyrics") || strings.HasSuffix(c.Route, "/live"))
	if adapterRead || kindRead {
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
