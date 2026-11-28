package admin

import (
	"context"
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
)

// ActivityStats supplies daily observed playback totals matching Activity › Viewing.
type ActivityStats interface {
	Activity(ctx context.Context, days int, tzMinutes ...int) ([]core.ActivityDay, error)
}

// Insights: which rows lead to plays, and server-wide viewing.

func (h *Handler) handleInsightRows(w http.ResponseWriter, r *http.Request) {
	rows, err := h.deps.Personalization.RowInsights(r.Context(), 7)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	if rows == nil {
		rows = []core.RowInsight{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

func (h *Handler) handleInsightRanker(w http.ResponseWriter, r *http.Request) {
	in, err := h.deps.Personalization.RankerInsight(r.Context())
	if err != nil {
		writeEngineError(w, err)
		return
	}
	if in.Features == nil {
		in.Features = []core.RankerFeature{}
	}
	writeJSON(w, http.StatusOK, in)
}

func (h *Handler) handleInsightActivity(w http.ResponseWriter, r *http.Request) {
	var (
		days []core.ActivityDay
		err  error
	)
	if h.deps.ActivityStats != nil {
		tz, _ := strconv.Atoi(r.URL.Query().Get("tz"))
		days, err = h.deps.ActivityStats.Activity(r.Context(), 30, tz)
	} else {
		days, err = h.deps.Personalization.Activity(r.Context(), 30)
	}
	if err != nil {
		writeEngineError(w, err)
		return
	}
	if days == nil {
		days = []core.ActivityDay{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days})
}

func (h *Handler) handleRecommendationReport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	days, _ := strconv.Atoi(q.Get("days"))
	viewer, _ := strconv.ParseInt(q.Get("viewer"), 10, 64)
	report, err := h.deps.Personalization.RecommendationReport(r.Context(), days, viewer)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (h *Handler) handleRecommendationDetail(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	days, _ := strconv.Atoi(q.Get("days"))
	viewer, _ := strconv.ParseInt(q.Get("viewer"), 10, 64)
	detail, err := h.deps.Personalization.RecommendationDetail(r.Context(), days, q.Get("kind"), viewer)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}
