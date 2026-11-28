package route

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Mux is a ServeMux whose registrations can also be enumerated. Register
// routes during construction, before publishing the handler.
type Mux struct {
	mux    http.ServeMux
	routes []Route
}

func (m *Mux) Handle(pattern string, handler http.Handler) {
	m.mux.Handle(pattern, handler)
	method, path, hasMethod := strings.Cut(pattern, " ")
	if !hasMethod {
		method, path = "*", pattern
	}
	m.routes = append(m.routes, Route{Method: method, Pattern: path})
}

func (m *Mux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(handler))
}

func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.mux.ServeHTTP(w, r) }

func (m *Mux) Routes() []Route { return append([]Route(nil), m.routes...) }

// Registered returns the inventory of a handler that exposes its registrations.
func Registered(h http.Handler) ([]Route, bool) {
	if source, ok := h.(interface{ Routes() []Route }); ok {
		return source.Routes(), true
	}
	if router, ok := h.(chi.Routes); ok {
		var routes []Route
		err := chi.Walk(router, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			routes = append(routes, Route{Method: method, Pattern: pattern})
			return nil
		})
		return routes, err == nil
	}
	return nil, false
}
