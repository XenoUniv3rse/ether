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
		switch clients[i].Name {
		case "xtream":
			const auth = "?username={xtreamUser}&password={xtreamPass}"
			clients[i].Steps = append(clients[i].Steps,
				Step{Name: "XMLTV guide export", Class: classGuide, Path: "/xmltv.php" + auth},
				Step{Name: "M3U lineup export", Class: classGuide, Path: "/get.php" + auth + "&type=m3u_plus"},
			)
		case "silo-appletv":
			clients[i].Steps = append(clients[i].Steps,
				Step{Name: "set series audio preference", Class: classState, Method: http.MethodPut, Path: "/api/v2/audio-prefs/{seriesId}", Body: `{"audio_language":"en"}`},
				Step{Name: "clear series audio preference", Class: classState, Method: http.MethodDelete, Path: "/api/v2/audio-prefs/{seriesId}"},
			)
		case "stremio":
			clients[i].Steps = append(clients[i].Steps,
				Step{Name: "movie stream choices", Class: classPlayback, Path: "/stream/movie/{movieMeta}.json"},
			)
		case "vela":
			clients[i].Steps = append(clients[i].Steps,
				Step{Name: "bootstrap", Class: classHome, Path: "/bootstrap"},
				Step{Name: "recommendations rows", Class: classHome, Path: "/rows?limit=24"},
				Step{Name: "movies page", Class: classBrowse, Path: "/libraries/{movieLibrary}/items?limit=48", Discover: discoverMatrixVelaID("velaMovie")},
				Step{Name: "movie detail", Class: classDetail, Path: "/titles/{velaMovie}"},
				Step{Name: "movie versions", Class: classDetail, Path: "/titles/{velaMovie}/versions"},
				Step{Name: "movie extras", Class: classDetail, Path: "/titles/{velaMovie}/extras"},
				Step{Name: "music facet", Class: classBrowse, Path: "/libraries/{matrixMusicLibrary}/facets/genre"},
				Step{Name: "music page", Class: classBrowse, Path: "/libraries/{matrixMusicLibrary}/items?limit=24", Discover: discoverMatrixVelaID("matrixMusicTrack")},
				Step{Name: "music continuation", Class: classDetail, Path: "/titles/{matrixMusicTrack}/continuation"},
				Step{Name: "movies rows", Class: classHome, Path: "/libraries/{movieLibrary}/rows"},
				Step{Name: "shows rows", Class: classHome, Path: "/libraries/{seriesLibrary}/rows"},
				Step{Name: "search", Class: classSearch, Method: http.MethodPost, Path: "/search", Body: `{"query":"star","mode":"fast","limit":24}`},
				Step{Name: "appearance", Class: classStatic, Path: "/appearance"},
				Step{Name: "capabilities", Class: classStatic, Path: "/capabilities"},
				Step{Name: "favorites", Class: classBrowse, Path: "/favorites/titles?role=track&limit=48"},
				Step{Name: "playlists", Class: classBrowse, Path: "/playlists"},
				Step{Name: "history recap", Class: classDetail, Path: "/history/recap?period=month"},
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
		case result.Status < 200 || result.Status >= 300 || result.Err != "":
			r.AddGap(key.config, key.surface, key.route, result.Step, fmt.Sprintf("unsuccessful scenario (HTTP %d)", result.Status))
		case step.Stream != 0 && rec.Duration < step.Stream-time.Second:
			r.AddGap(key.config, key.surface, key.route, result.Step, "stream closed before the observation window")
		default:
			r.successes++
		}
		if !ok {
			continue
		}
		budget := matrixBudget(step, rec, result.Route)
		score := max(overBudget(rec.Queries, budget.Queries), overBudget(rec.Rows, budget.Rows), overBudget(rec.Writes, budget.Writes))
		if score > 1 && score > r.offenders[key].score {
			r.offenders[key] = matrixOffender{key, result.Client + " / " + result.Step, rec, budget, score}
		}
	}
}

func overBudget(actual, limit int64) float64 {
	if limit == 0 && actual > 0 {
		return float64(actual + 1)
	}
	return float64(actual) / float64(max(1, limit))
}

func matrixBudget(step Step, rec TraceRecord, route string) matrixWork {
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
		b.Writes = allowedReadWrites(route, rec.Statements)
	}
	return b
}

// These writes publish capabilities carried by read responses. Other writes,
// including cleanup and cache fill, still consume the zero-write budget.
func allowedReadWrites(route string, statements []statementStat) int64 {
	const (
		lastSeen   = "UPDATE auth_users SET last_seen_at=? WHERE id=? AND last_seen_at<?"
		epochFence = "UPDATE viewer_policy_epoch SET generation=generation WHERE id=1"
	)
	var writes, seenWrites, receiptInserts, headerInserts, offerInserts int64
	velaRows := route == "GET /vela/v0/bootstrap" || route == "GET /vela/v0/libraries/{id}/rows" ||
		route == "GET /vela/v0/rows" || route == "GET /vela/v0/rows/{id}"
	siloOffers := route == "GET /silo/api/v2/catalog/items/{id}" || route == "GET /silo/api/v2/catalog/items/{id}/versions" ||
		route == "GET /silo/api/v2/watch/{id}"
	velaVersions := route == "GET /vela/v0/titles/{id}/versions"
	for _, statement := range statements {
		sql := strings.Join(strings.Fields(statement.SQL), " ")
		switch {
		case sql == lastSeen:
			seenWrites = max(seenWrites, min(1, statement.Count))
		case velaRows && strings.HasPrefix(sql, "INSERT INTO experience_row_views(id,scope_hash,viewer_id,device_id,protocol,snapshot,expires,payload) "):
			writes += min(2, statement.Count)
		case velaRows && strings.HasPrefix(sql, "INSERT INTO experience_row_view_manifests(scope_hash,viewer_id,device_id,protocol,generation,expires,payload) "):
			writes += min(2, statement.Count)
		case velaRows && strings.HasPrefix(sql, "UPDATE experience_row_views SET expires=? WHERE viewer_id=? AND device_id=? AND protocol=? AND expires>? AND ("):
			writes += min(2, statement.Count)
		case velaRows && sql == "DELETE FROM experience_row_view_manifests WHERE scope_hash=?":
			writes += min(2, statement.Count)
		case velaRows && strings.HasPrefix(sql, "INSERT INTO experience_receipts(hash,viewer_id,device_id,protocol,expires,kind,subject,purpose,rank,impression,generation) "):
			receiptInserts += min(3, statement.Count)
		case (velaRows || siloOffers) && strings.HasPrefix(sql, "INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires) "):
			headerInserts += statement.Count
		case (siloOffers || velaVersions) && strings.HasPrefix(sql, "INSERT INTO sessions_version_ids(handle,surface,item_id,release_id,seen_at) VALUES "):
			writes += min(1, statement.Count)
		case siloOffers && strings.HasPrefix(sql, "INSERT INTO sessions_source_offers(namespace,digest,item_id,kind,reference,revision) "):
			offerInserts += min(128, statement.Count)
		case siloOffers && strings.HasPrefix(sql, "INSERT INTO silo_source_offer_ids(id,offer_id) "):
			writes += min(128, statement.Count)
		case siloOffers && strings.HasPrefix(sql, "UPDATE capability_headers SET expires=? WHERE namespace=? AND digest=?"):
			writes += min(128, statement.Count)
		case (siloOffers || velaVersions) && sql == epochFence:
			writes += min(2, statement.Count)
		}
	}
	return writes + seenWrites + receiptInserts + offerInserts + min(headerInserts, receiptInserts+offerInserts)
}

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
