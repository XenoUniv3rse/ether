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

func TestMatrixRouteRespectsPlexPathConstraints(t *testing.T) {
	runner := &Runner{http: &http.Client{}}
	runner.SetMatrixRoutes(MatrixRoutes{"plex": {
		{Method: "GET", Pattern: "/library/parts/{part}/{changestamp:[0-9]+}/{file}"},
		{Method: "GET", Pattern: "/library/parts/{part}/{file}"},
		{Method: "*", Pattern: "/{ttype:video|music|audio|subtitles}/:/transcode/universal/ping"},
	}})
	for path, want := range map[string]string{
		"/library/parts/17/12345/file.mp4":   "GET /library/parts/{part}/{changestamp:[0-9]+}/{file}",
		"/library/parts/17/asset/file.mp4":   "",
		"/library/parts/17/file.mp4":         "GET /library/parts/{part}/{file}",
		"/video/:/transcode/universal/ping":  "* /{ttype:video|music|audio|subtitles}/:/transcode/universal/ping",
		"/photos/:/transcode/universal/ping": "",
	} {
		if got := runner.matrixRoute("plex", http.MethodGet, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}
