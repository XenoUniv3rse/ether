package perfharness

import (
	"cmp"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/conform"
)

type MatrixRoutes map[string][]conform.Route

type matrixRoute struct {
	route conform.Route
	match *regexp.Regexp
	score int
}

var routePlaceholder = regexp.MustCompile(`\{[^}]+\}|\*`)

func (r *Runner) SetMatrixRoutes(routes MatrixRoutes) {
	r.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	r.matrixRoutes = make(map[string][]matrixRoute, len(routes))
	for surface, entries := range routes {
		for _, entry := range entries {
			var pattern strings.Builder
			pattern.WriteString("^")
			if surface == "jellyfin" || surface == "emby" || surface == "silo" || surface == "stremio" || surface == "xtream" {
				pattern.WriteString("(?i)")
			}
			pos, score := 0, 0
			for _, span := range routePlaceholder.FindAllStringIndex(entry.Pattern, -1) {
				literal := entry.Pattern[pos:span[0]]
				pattern.WriteString(regexp.QuoteMeta(literal))
				score += len(literal)*2 - 1
				switch token := entry.Pattern[span[0]:span[1]]; {
				case token == "{$}":
				case token == "*" || strings.HasSuffix(token, "...}"):
					pattern.WriteString(".*")
				default:
					pattern.WriteString("[^/]+")
				}
				pos = span[1]
			}
			pattern.WriteString(regexp.QuoteMeta(entry.Pattern[pos:]))
			score += 2 * (len(entry.Pattern) - pos)
			if strings.HasSuffix(entry.Pattern, "/") && entry.Method == "*" {
				pattern.WriteString(".*")
			}
			pattern.WriteString("$")
			r.matrixRoutes[surface] = append(r.matrixRoutes[surface], matrixRoute{entry, regexp.MustCompile(pattern.String()), score})
		}
		slices.SortStableFunc(r.matrixRoutes[surface], func(a, b matrixRoute) int {
			if a.score == b.score && (a.route.Method == "*") != (b.route.Method == "*") {
				if a.route.Method == "*" {
					return 1
				}
				return -1
			}
			return cmp.Compare(b.score, a.score)
		})
	}
}

func (r *Runner) matrixRoute(surface, method, path string) string {
	for _, entry := range r.matrixRoutes[surface] {
		if (entry.route.Method == method || entry.route.Method == "*") && entry.match.MatchString(path) {
			return entry.route.Method + " " + entry.route.Pattern
		}
	}
	return ""
}

type MatrixRouteKey struct {
	Surface, Method, Pattern string
}

func (result Result) matrixSuccess(trace TraceRecord) bool {
	if result.Err != "" || trace.Status != result.Status {
		return false
	}
	if result.ExpectedStatus != 0 {
		return result.Status == result.ExpectedStatus
	}
	return result.Status >= 200 && result.Status < 300
}

func (r *MatrixReport) CheckRoutes(configuration string, inventory MatrixRoutes, results []Result, trace map[string]TraceRecord, allowed map[MatrixRouteKey]string) {
	hits := map[[2]string]bool{}
	for _, result := range results {
		if tr, ok := trace[result.Tag]; ok && result.matrixSuccess(tr) {
			hits[[2]string{result.Surface, result.Route}] = true
		}
	}
	var surfaces []string
	for surface := range inventory {
		surfaces = append(surfaces, surface)
	}
	slices.Sort(surfaces)
	for _, surface := range surfaces {
		entries := slices.Clone(inventory[surface])
		slices.SortFunc(entries, func(a, b conform.Route) int {
			return cmp.Or(cmp.Compare(a.Pattern, b.Pattern), cmp.Compare(a.Method, b.Method))
		})
		for _, entry := range slices.Compact(entries) {
			id := entry.Method + " " + entry.Pattern
			if hits[[2]string{surface, id}] {
				continue
			}
			if allowed[MatrixRouteKey{surface, entry.Method, entry.Pattern}] != "" {
				continue
			}
			r.AddGap(configuration, surface, id, "", "registered route has no successful scenario")
		}
	}
}

// AppendMatrixReads adds requests for fixed, registered read paths on the
// primary client for each surface. Stateful workflows and parameterized
// routes still require scenarios with explicit prerequisites.
func AppendMatrixReads(clients []Client, inventory MatrixRoutes, allowed map[MatrixRouteKey]string) {
	primary := map[string]string{
		"console": "console", "plex": "plex-appletv", "jellyfin": "infuse-jellyfin",
		"emby": "emby-sdk", "silo": "silo-appletv", "vela": "vela",
	}
	seen := map[[3]string]bool{}
	for _, c := range clients {
		for _, st := range c.Steps {
			method := st.Method
			if method == "" {
				method = http.MethodGet
			}
			path := strings.SplitN(c.SharedPrefix+c.Prefix+st.Path, "?", 2)[0]
			seen[[3]string{c.Surface, method, strings.ToLower(path)}] = true
		}
	}
	for i := range clients {
		c := &clients[i]
		if c.Name != primary[c.Surface] {
			continue
		}
		entries := slices.Clone(inventory[c.Surface])
		slices.SortFunc(entries, func(a, b conform.Route) int {
			return cmp.Or(cmp.Compare(a.Pattern, b.Pattern), cmp.Compare(a.Method, b.Method))
		})
		for _, entry := range entries {
			if entry.Method != http.MethodGet && entry.Method != http.MethodHead {
				continue
			}
			path := strings.TrimSuffix(entry.Pattern, "{$}")
			relative, mounted := strings.CutPrefix(path, c.SharedPrefix+c.Prefix)
			key := [3]string{c.Surface, entry.Method, strings.ToLower(path)}
			if seen[key] || !mounted || !strings.HasPrefix(relative, "/") ||
				strings.ContainsAny(relative, "{}*") || !matrixReadPath(c.Surface, relative) ||
				allowed[MatrixRouteKey{c.Surface, entry.Method, entry.Pattern}] != "" {
				continue
			}
			seen[key] = true
			class := classBrowse
			if c.Surface == "console" {
				class = classConsole
			}
			c.Steps = append(c.Steps, Step{Name: entry.Method + " " + relative, Method: entry.Method, Class: class, Path: relative})
		}
	}
}

func matrixReadPath(surface, path string) bool {
	path = strings.ToLower(path)
	for _, segment := range strings.Split(path, "/") {
		switch segment {
		case "stream", "events", "eventsource", "websocket", "websockets":
			return false
		}
	}
	switch surface {
	case "console", "silo", "vela":
		return true
	case "plex":
		return strings.HasPrefix(path, "/library") || strings.HasPrefix(path, "/hubs") ||
			strings.HasPrefix(path, "/clients/profiles") || strings.HasPrefix(path, "/media/providers") ||
			path == "/identity" || path == "/servers" || path == "/system/agents" || path == "/activities"
	default:
		for _, prefix := range []string{"/system/info", "/library", "/genres", "/studios", "/artists", "/musicgenres", "/persons", "/channels", "/livetv", "/items", "/movies", "/shows", "/search"} {
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				return true
			}
		}
		return false
	}
}
