package perfharness

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

func MatrixClients(live bool) []Client {
	clients := Clients()
	for i := range clients {
		if clients[i].Surface == "plex" && !live {
			for j, step := range clients[i].Steps {
				switch step.Name {
				case "live guide grid 3h", "live hubs":
					clients[i].Steps[j].ExpectedStatus = http.StatusNotFound
				}
			}
		}
		switch clients[i].Name {
		case "plex-appletv":
			clients[i].Steps = append(clients[i].Steps,
				Step{Name: "HEAD /hubs/search with query", Class: classSearch, Method: http.MethodHead, Path: "/hubs/search?query=star&limit=30"},
				Step{Name: "GET /hubs/search/voice with query", Class: classSearch, Path: "/hubs/search/voice?query=star&limit=30"},
				Step{Name: "HEAD /hubs/search/voice with query", Class: classSearch, Method: http.MethodHead, Path: "/hubs/search/voice?query=star&limit=30"},
				Step{Name: "GET /library/all/top movies", Class: classBrowse, Path: "/library/all/top?type=1"},
				Step{Name: "HEAD /library/all/top movies", Class: classBrowse, Method: http.MethodHead, Path: "/library/all/top?type=1"},
			)
		case "xtream":
			const auth = "?username={xtreamUser}&password={xtreamPass}"
			for j := range clients[i].Steps {
				step := &clients[i].Steps[j]
				switch step.Name {
				case "get_live_streams":
					step.Discover = discoverRegexp("xtreamLive", xtreamStreamID)
				case "get_vod_streams":
					old := step.Discover
					step.Discover = func(vars map[string]string, body []byte) {
						old(vars, body)
						discoverMatrixArtwork(vars, body, "xtreamCover", "/xtream/image/", "stream_icon")
					}
				}
			}
			if live {
				clients[i].Steps = append(clients[i].Steps,
					Step{Name: "get_short_epg", Class: classGuide, Path: "/player_api.php" + auth + "&action=get_short_epg&stream_id={xtreamLive}"},
					Step{Name: "get_simple_data_table", Class: classGuide, Path: "/player_api.php" + auth + "&action=get_simple_data_table&stream_id={xtreamLive}"},
				)
			}
			clients[i].Steps = append(clients[i].Steps,
				Step{Name: "VOD cover", Class: classDetail, Path: "/image/{xtreamCover}"},
				Step{Name: "VOD cover probe", Class: classDetail, Method: http.MethodHead, Path: "/image/{xtreamCover}"},
				Step{Name: "XMLTV guide export", Class: classGuide, Path: "/xmltv.php" + auth},
				Step{Name: "M3U lineup export", Class: classGuide, Path: "/get.php" + auth + "&type=m3u_plus"},
			)
		case "stremio":
			clients[i].Prefix = "/stremio"
			for j := range clients[i].Steps {
				step := &clients[i].Steps[j]
				step.Path = "/{stremio}" + step.Path
				if step.Name == "catalog movies" {
					old := step.Discover
					step.Discover = func(vars map[string]string, body []byte) {
						old(vars, body)
						var catalog struct {
							Metas json.RawMessage `json:"metas"`
						}
						if json.Unmarshal(body, &catalog) == nil {
							discoverMatrixArtwork(vars, catalog.Metas, "stremioPoster", "/stremio/image/", "poster")
						}
					}
				}
			}
			clients[i].Steps = append(clients[i].Steps,
				Step{Name: "movie stream choices", Class: classPlayback, Path: "/{stremio}/stream/movie/{movieMeta}.json", Discover: discoverMatrixStremioStream},
				Step{Name: "catalog poster", Class: classDetail, Path: "/image/{stremioPoster}"},
				Step{Name: "movie stream probe", Class: classPlayback, Method: http.MethodHead, Path: "/sessions/lazy/{stremioTicket}/file"},
			)
		case "vela":
			clients[i].Steps = append(clients[i].Steps,
				Step{Name: "bootstrap", Class: classHome, Path: "/bootstrap"},
				Step{Name: "recommendations rows", Class: classHome, Path: "/rows?limit=24"},
				Step{Name: "movies page", Class: classBrowse, Path: "/libraries/{movieLibrary}/items?limit=48", Discover: discoverMatrixVelaID("velaMovie")},
				Step{Name: "movie detail", Class: classDetail, Path: "/titles/{velaMovie}"},
				Step{Name: "movie versions", Class: classDetail, Path: "/titles/{velaMovie}/versions"},
				Step{Name: "movie continuation", Class: classDetail, Path: "/titles/{velaMovie}/continuation"},
				Step{Name: "movie extras", Class: classDetail, Path: "/titles/{velaMovie}/extras"},
				Step{Name: "movies facets", Class: classBrowse, Path: "/libraries/{movieLibrary}/facets/genre"},
				Step{Name: "movies rows", Class: classHome, Path: "/libraries/{movieLibrary}/rows"},
				Step{Name: "shows rows", Class: classHome, Path: "/libraries/{seriesLibrary}/rows"},
				Step{Name: "search", Class: classSearch, Method: http.MethodPost, Path: "/search", Body: `{"query":"star","limit":24}`},
				Step{Name: "appearance", Class: classStatic, Path: "/appearance"},
				Step{Name: "capabilities", Class: classStatic, Path: "/capabilities"},
				Step{Name: "favorites", Class: classBrowse, Path: "/favorites/titles?limit=48"},
				Step{Name: "playlists", Class: classBrowse, Path: "/playlists"},
				Step{Name: "recent", Class: classHome, Path: "/recent"},
				Step{Name: "shows page", Class: classBrowse, Path: "/libraries/{seriesLibrary}/items?limit=48"},
				Step{Name: "live channels", Class: classGuide, Path: "/live/channels"},
				Step{Name: "live guide", Class: classGuide, Path: "/live/guide?from={guideFrom}&until={guideUntil}&limit=100"},
			)
			if live {
				headers := clients[i].Headers
				clients[i].Headers = func(s *session) http.Header {
					h := headers(s)
					h.Set("X-Vela-Screen-Renderer", "19")
					return h
				}
				clients[i].Steps = append(clients[i].Steps,
					Step{Name: "sports overlay", Class: classDetail, Path: "/surfaces/matrixsports.scorebug/document"},
					Step{Name: "sports overlay data", Class: classDetail, Path: "/surfaces/matrixsports.scorebug/data?p.event=event%3Acommunity.example.matrixsports%3Asports%3Agame-0", Discover: discoverMatrixLiveTopic},
					Step{Name: "live events stream", Class: classGuide, Path: "/live/stream?alerts=1&topic={overlayTopic}", Stream: time.Minute},
				)
			}
		}
	}
	return clients
}

func discoverMatrixArtwork(vars map[string]string, body []byte, variable, prefix, field string) {
	delete(vars, variable)
	var items []map[string]json.RawMessage
	if json.Unmarshal(body, &items) != nil {
		return
	}
	for _, item := range items {
		var raw string
		if json.Unmarshal(item[field], &raw) != nil {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if secret, ok := strings.CutPrefix(u.Path, prefix); ok && secret != "" && !strings.Contains(secret, "/") {
			vars[variable] = url.PathEscape(secret)
			return
		}
	}
}

func discoverMatrixStremioStream(vars map[string]string, body []byte) {
	var result struct {
		Streams []struct {
			URL string `json:"url"`
		} `json:"streams"`
	}
	delete(vars, "stremioTicket")
	if json.Unmarshal(body, &result) != nil || len(result.Streams) == 0 {
		return
	}
	u, err := url.Parse(result.Streams[0].URL)
	if err != nil {
		return
	}
	rest, ok := strings.CutPrefix(u.Path, "/stremio/sessions/lazy/")
	token, tail, split := strings.Cut(rest, "/")
	if ok && split && token != "" && tail == "file" {
		vars["stremioTicket"] = url.PathEscape(token)
	}
}

func discoverMatrixVelaID(variable string) func(map[string]string, []byte) {
	return func(vars map[string]string, body []byte) {
		var page struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if json.Unmarshal(body, &page) == nil && len(page.Items) != 0 && page.Items[0].ID != "" {
			vars[variable] = url.PathEscape(page.Items[0].ID)
		}
	}
}

func discoverMatrixLiveTopic(vars map[string]string, body []byte) {
	var data struct {
		Live struct {
			Topic string `json:"topic"`
		} `json:"live"`
	}
	if json.Unmarshal(body, &data) == nil && data.Live.Topic != "" {
		vars["overlayTopic"] = url.QueryEscape(data.Live.Topic)
	}
}

type matrixKey struct {
	config, surface, route string
}

type matrixWork struct{ Queries, Rows, Writes int64 }

type matrixOffender struct {
	matrixKey
	step   string
	trace  TraceRecord
	budget matrixWork
	score  float64
}

type MatrixReport struct {
	offenders           map[matrixKey]matrixOffender
	gaps                []matrixGap
	successes, attempts int
	configs             []string
}

type matrixGap struct {
	matrixKey
	step, reason string
}

func (r *MatrixReport) GapCount() int      { return len(r.gaps) }
func (r *MatrixReport) OffenderCount() int { return len(r.offenders) }

func (r *MatrixReport) AddGap(config, surface, route, step, reason string) {
	r.gaps = append(r.gaps, matrixGap{matrixKey{config, surface, route}, step, reason})
}

func (r *MatrixReport) Add(configuration string, results []Result, trace map[string]TraceRecord, clients []Client) {
	if r.offenders == nil {
		r.offenders = make(map[matrixKey]matrixOffender)
	}
	r.configs = append(r.configs, configuration)
	r.attempts += len(results)
	steps := map[[2]string]Step{}
	for _, c := range clients {
		if c.SignIn != nil {
			steps[[2]string{c.Name, c.SignIn.Name}] = *c.SignIn
		}
		for _, st := range c.Steps {
			steps[[2]string{c.Name, st.Name}] = st
		}
	}
	for _, result := range results {
		key := matrixKey{configuration, result.Surface, result.Route}
		rec, ok := trace[result.Tag]
		step := steps[[2]string{result.Client, result.Step}]
		switch {
		case result.Skipped != "":
			r.AddGap(key.config, key.surface, key.route, result.Step, result.Skipped)
		case !ok:
			r.AddGap(key.config, key.surface, key.route, result.Step, "missing completed request trace")
		case !result.matrixSuccess(rec):
			r.AddGap(key.config, key.surface, key.route, result.Step, fmt.Sprintf("unexpected scenario result (HTTP %d)", result.Status))
		case step.Stream != 0 && rec.Duration < step.Stream-time.Second:
			r.AddGap(key.config, key.surface, key.route, result.Step, "stream closed before the observation window")
		default:
			r.successes++
		}
		if !ok {
			continue
		}
		budget := matrixBudget(step, rec)
		score := max(overBudget(rec.Queries, budget.Queries), overBudget(rec.Rows, budget.Rows), overBudget(rec.Writes-requiredDetailWrites(result.Route, rec), budget.Writes))
		if score > 1 && score > r.offenders[key].score {
			r.offenders[key] = matrixOffender{key, result.Client + " / " + result.Step, rec, budget, score}
		}
	}
}

// Emby item detail carries reversible numeric genre and studio identifiers.
// The first published identifier must survive restart and be resolvable on
// other cluster nodes. Only the two namespace batches that publish it are
// allowed here; any other write still counts against the read budget.
func requiredDetailWrites(route string, rec TraceRecord) int64 {
	if rec.Method != http.MethodGet && rec.Method != http.MethodHead {
		return 0
	}
	if route != rec.Method+" /emby/Items/{id}" && route != rec.Method+" /emby/Users/{userId}/Items/{id}" {
		return 0
	}
	var counters, mappings int64
	for _, statement := range rec.Statements {
		switch sql := strings.Join(strings.Fields(statement.SQL), " "); {
		case sql == "INSERT INTO frontend_compact_id_counters(surface,kind,last_id) VALUES(?,?,0) ON CONFLICT(surface,kind) DO UPDATE SET last_id=frontend_compact_id_counters.last_id":
			counters += statement.Count
		case sql == "INSERT INTO frontend_compact_ids(surface,kind,ref,id) VALUES(?,?,?,?) ON CONFLICT(surface,kind,ref) DO NOTHING",
			strings.HasPrefix(sql, "INSERT INTO frontend_compact_ids(surface,kind,ref,id) VALUES (?,?,?,?)"):
			mappings += statement.Count
		}
	}
	return min(rec.Writes, min(2, counters)+min(2, mappings))
}

func overBudget(actual, limit int64) float64 {
	if limit == 0 && actual > 0 {
		return float64(actual + 1)
	}
	return float64(actual) / float64(max(1, limit))
}

func matrixBudget(step Step, rec TraceRecord) matrixWork {
	b := matrixWork{Queries: 150, Rows: 5_000, Writes: 50}
	switch step.Class {
	case classStatic:
		b.Queries, b.Rows = 30, 500
	case classHome:
		b.Queries, b.Rows = 300, 10_000
	case classGuide:
		b.Queries, b.Rows = 150, 12_000
	case classBrowse, classConsole:
		b.Rows = 10_000
	}
	if step.QueriesBound > 0 {
		b.Queries = min(b.Queries, int64(step.QueriesBound))
	}
	if step.RowsBound > 0 {
		b.Rows = min(b.Rows, int64(step.RowsBound))
	}
	if rec.Method == http.MethodGet || rec.Method == http.MethodHead {
		b.Writes = 0
		for _, statement := range rec.Statements {
			query := strings.Join(strings.Fields(statement.SQL), " ")
			switch {
			case query == "UPDATE auth_users SET last_seen_at=? WHERE id=? AND last_seen_at<?":
				b.Writes += min(1, statement.Count)
			case rec.Surface == "plex" && plexVariantAllocation.MatchString(query):
				// The response issues Media IDs that a different node can be
				// asked to resolve immediately. Only unseen pairs are inserted.
				b.Writes += statement.Count
			case rec.Surface == "plex" && rec.Method == http.MethodGet && plexWatchMutation(rec.Path, query):
				// Plex playback clients use GET for these explicit watch
				// commands. Only their state updates count as allowed writes.
				b.Writes += statement.Count
			}
		}
	}
	return b
}

func plexWatchMutation(path, query string) bool {
	switch path {
	case "/:/timeline":
		switch query {
		case "DELETE FROM personalize_discovery_hidden WHERE user_id=? AND item_key=?",
			"DELETE FROM state_continue_hidden WHERE user_id=? AND item_key=?":
			return true
		}
		fallthrough
	case "/:/scrobble", "/:/unscrobble":
		return strings.HasPrefix(query, "INSERT INTO state_watch(user_id, rating_key, ") ||
			strings.HasPrefix(query, "UPDATE state_watch SET view_offset_ns=") ||
			strings.HasPrefix(query, "INSERT INTO state_history(user_id, ")
	}
	return false
}

var plexVariantAllocation = regexp.MustCompile(`^INSERT INTO variants_ids\(media_id,rating_key,release_id,last_seen\) VALUES \(\?,\?,\?,\?\)(,\(\?,\?,\?,\?\))* ON CONFLICT DO NOTHING$`)

var sqlLiteral = regexp.MustCompile(`'(?:[^']|'')*'`)

// Write emits one worst observed sample per route and configuration, using
// unexpanded route templates so grants, cursors and credentials stay private.
func (r *MatrixReport) Write(w io.Writer, elapsed time.Duration) {
	fmt.Fprintln(w, "# Ether performance matrix")
	fmt.Fprintf(w, "\nElapsed %s. %d attempts, %d successful, %d coverage gaps and %d over-budget route/configuration pairs.\n", elapsed.Round(time.Second), r.attempts, r.successes, r.GapCount(), r.OffenderCount())
	fmt.Fprintf(w, "\nConfigurations replayed: %s.\n", strings.Join(r.configs, ", "))
	counts := map[string]int{}
	list := make([]matrixOffender, 0, len(r.offenders))
	for _, o := range r.offenders {
		list = append(list, o)
		counts[o.surface]++
	}
	slices.SortFunc(list, func(a, b matrixOffender) int {
		return cmp.Or(cmp.Compare(b.score, a.score), cmp.Compare(b.trace.Queries, a.trace.Queries),
			cmp.Compare(a.surface, b.surface), cmp.Compare(a.route, b.route), cmp.Compare(a.config, b.config))
	})
	fmt.Fprintln(w, "\n## Offenders by surface\n\n| Surface | Route/configuration pairs |\n|---|---:|")
	var surfaces []string
	for surface := range counts {
		surfaces = append(surfaces, surface)
	}
	slices.Sort(surfaces)
	for _, surface := range surfaces {
		fmt.Fprintf(w, "| %s | %d |\n", surface, counts[surface])
	}
	fmt.Fprintln(w, "\n## Offenders\n\nRanked by largest budget overrun. The first 20 rows are the highest-ranked offenders. Values show observed / ceiling. Stream entries cover one minute. Rows count records returned by the driver.\n\n| # | Surface | Configuration | Route | Client / scenario | Statements | Rows | Writes | HTTP |\n|---:|---|---|---|---|---:|---:|---:|---:|")
	for i, o := range list {
		fmt.Fprintf(w, "| %d | %s | %s | `%s` | %s | %d / %d | %d / %d | %d / %d | %d |\n",
			i+1, o.surface, o.config, o.route, clip(o.step, 160), o.trace.Queries, o.budget.Queries,
			o.trace.Rows, o.budget.Rows, o.trace.Writes, o.budget.Writes, o.trace.Status)
	}
	fmt.Fprintln(w, "\n## Statement attribution")
	for i, o := range list {
		fmt.Fprintf(w, "\n### %d. %s · %s · %s\n", i+1, o.surface, o.config, o.route)
		stats := slices.Clone(o.trace.Statements)
		slices.SortFunc(stats, func(a, b statementStat) int {
			return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(b.Rows, a.Rows), cmp.Compare(a.SQL, b.SQL))
		})
		for _, st := range stats[:min(len(stats), 3)] {
			fmt.Fprintf(w, "\n- %d calls, %d rows, `%s`\n\n  `%s`\n", st.Count, st.Rows, clip(st.Site, 220), clip(sqlLiteral.ReplaceAllString(st.SQL, "?"), 360))
		}
	}
	fmt.Fprintln(w, "\n## Coverage gaps\n\n| Surface | Configuration | Route | Scenario | Reason |\n|---|---|---|---|---|")
	for _, gap := range r.gaps {
		fmt.Fprintf(w, "| %s | %s | `%s` | %s | %s |\n", gap.surface, gap.config, gap.route, clip(gap.step, 160), gap.reason)
	}
}
