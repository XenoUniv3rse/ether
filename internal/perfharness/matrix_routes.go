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

func (r *MatrixReport) CheckRoutes(configuration string, inventory MatrixRoutes, results []Result, trace map[string]TraceRecord, allowed map[MatrixRouteKey]string) {
	hits := map[[2]string]bool{}
	for _, result := range results {
		if tr, ok := trace[result.Tag]; ok && result.Err == "" && result.Status >= 200 && result.Status < 300 && tr.Status == result.Status {
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

// AppendConsoleReads supplies authenticated scripts for console GET routes
// with no identifiers. Endpoints with parameters, side effects, or protocol
// handshakes require authored scenarios.
func AppendConsoleReads(clients []Client, inventory MatrixRoutes, allowed map[MatrixRouteKey]string) {
	for i := range clients {
		c := &clients[i]
		if c.Surface != "console" {
			continue
		}
		seen := map[string]bool{}
		for _, st := range c.Steps {
			if st.Method == "" || st.Method == http.MethodGet {
				seen[strings.SplitN(c.Prefix+st.Path, "?", 2)[0]] = true
			}
		}
		entries := slices.Clone(inventory["console"])
		slices.SortFunc(entries, func(a, b conform.Route) int { return cmp.Compare(a.Pattern, b.Pattern) })
		for _, entry := range entries {
			path := entry.Pattern
			if entry.Method != http.MethodGet || seen[path] || !strings.HasPrefix(path, c.Prefix+"/") ||
				strings.ContainsAny(path, "{}*") || allowed[MatrixRouteKey{"console", entry.Method, path}] != "" {
				continue
			}
			seen[path] = true
			c.Steps = append(c.Steps, Step{Name: path, Class: classConsole, Path: strings.TrimPrefix(path, c.Prefix)})
		}
	}
}
