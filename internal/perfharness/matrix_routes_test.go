package perfharness

import (
	"net/http"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform"
)

func TestMatrixRoutePrefersExactMountedLeaf(t *testing.T) {
	runner := &Runner{http: &http.Client{}}
	runner.SetMatrixRoutes(MatrixRoutes{"console": []conform.Route{
		{Method: "*", Pattern: "/ether/api/example/"},
		{Method: "GET", Pattern: "/ether/api/example/{$}"},
		{Method: "GET", Pattern: "/ether/api/example/{id}"},
	}})
	for path, want := range map[string]string{
		"/ether/api/example/":   "GET /ether/api/example/{$}",
		"/ether/api/example/42": "GET /ether/api/example/{id}",
	} {
		if got := runner.matrixRoute("console", "GET", path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}
