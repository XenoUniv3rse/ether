package app

import (
	"net/http"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/perfharness"
)

// Explicit exclusions name the registered route, including its method.
var perfMatrixRouteExceptions = map[perfharness.MatrixRouteKey]string{
	{Surface: "console", Method: http.MethodGet, Pattern: "/ether/api/logs/stream"}:   "owner log SSE requires a log producer and a dedicated stream scenario",
	{Surface: "console", Method: "*", Pattern: "/ether/api/"}:                         "fallback 404 for unregistered API paths",
	{Surface: "console", Method: http.MethodPost, Pattern: "/ether/api/setup/local"}:  "only reachable before setup, while the fixture has an active owner",
	{Surface: "console", Method: http.MethodPost, Pattern: "/ether/api/setup/reopen"}: "takes the fixture out of its configured state",
}

func perfMatrixInventory(t *testing.T, a *App, c perfMatrixCase, subsonic []string, report *perfharness.MatrixReport) perfharness.MatrixRoutes {
	t.Helper()
	inventory := perfharness.MatrixRoutes{"plex": plex.RouteInventory().Routes}
	for surface, prefix := range map[string]string{
		"jellyfin": "/jellyfin", "emby": "/emby", "silo": "/silo", "stremio": "/stremio", "xtream": "/xtream", "vela": "",
	} {
		inst, _ := a.frontends.registry.Instance(frontend.Protocol(surface))
		source, ok := inst.(frontend.RouteSource)
		if !ok {
			report.AddGap(c.name, surface, "", "", "frontend route inventory unavailable")
			continue
		}
		routes := source.Routes()
		for i := range routes {
			routes[i].Pattern = prefix + routes[i].Pattern
		}
		inventory[surface] = routes
	}
	if a.adminRoutes != nil {
		for _, entry := range a.adminRoutes.Routes() {
			if len(entry.Pattern) >= 5 && entry.Pattern[:5] == "/api/" {
				entry.Pattern = "/ether" + entry.Pattern
				inventory["console"] = append(inventory["console"], entry)
			}
		}
	} else {
		report.AddGap(c.name, "console", "", "", "owner API route inventory unavailable")
	}
	if a.pluginsRT != nil {
		inventory["plugin-host"], _ = route.Registered(a.pluginsRT.pluginHost)
	}
	if len(inventory["plugin-host"]) == 0 {
		report.AddGap(c.name, "plugin-host", "", "", "plugin host route inventory unavailable")
	}
	if len(subsonic) == 0 {
		report.AddGap(c.name, "subsonic", "", "", "no Subsonic adapter is installed in the fixture")
	}
	for _, name := range subsonic {
		inventory["subsonic"] = append(inventory["subsonic"], frontend.Route{Method: "*", Pattern: "/subsonic/rest/" + name + ".view"})
	}
	return inventory
}
