package perfharness

import (
	"net/http"
	"strings"
	"testing"
)

func TestMatrixSeparatesColdLineupBuildAndGatesSnapshotHit(t *testing.T) {
	const route = "GET /xtream/get.php"
	cold := Step{Name: "M3U lineup export cold", Class: classGuide, ColdLineup: true}
	hit := Step{Name: "M3U lineup export", Class: classGuide}
	clients := []Client{{Name: "xtream", Surface: "xtream", Steps: []Step{cold, hit}}}
	results := []Result{
		{Client: "xtream", Surface: "xtream", Route: route, Step: cold.Name, Tag: "cold", Status: http.StatusOK},
		{Client: "xtream", Surface: "xtream", Route: route, Step: hit.Name, Tag: "hit", Status: http.StatusOK},
	}
	traces := map[string]TraceRecord{
		"cold": {Method: http.MethodGet, Status: http.StatusOK, Queries: 1602, Rows: 700002},
		"hit":  {Method: http.MethodGet, Status: http.StatusOK, Queries: 151, Rows: 2},
	}
	var report MatrixReport
	report.AddBackground("baseline", []TraceRecord{{
		Work: "background build", ParentTag: "cold", Surface: "xtream", Path: "/xtream/get.php",
		Status: http.StatusOK, Queries: 1600, Rows: 700000, MaxRows: 5250,
	}})
	report.Add("baseline", results, traces, clients)
	if report.GapCount() != 0 || report.OffenderCount() != 1 {
		t.Fatalf("gaps=%d offenders=%d", report.GapCount(), report.OffenderCount())
	}
	if violation := report.offenders[matrixKey{"baseline", "xtream", route}]; violation.step != "xtream / "+hit.Name {
		t.Fatalf("cold build obscured a snapshot-hit regression: %+v", violation)
	}
	var log strings.Builder
	report.Write(&log, 0)
	if !strings.Contains(log.String(), "| baseline | background build | 1600 | 700000 | 0 | 5250 | 200 |") {
		t.Fatal("build measurements missing")
	}
}
