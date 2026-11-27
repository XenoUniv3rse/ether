package personalize

import (
	"context"
	"database/sql"
	"math"
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

const recommendationMinSample = 20

const recommendationCountsSQL = `COUNT(*),
	COUNT(CASE WHEN play_id <> 0 THEN 1 END),
	COUNT(CASE WHEN play_id > 0 THEN 1 END),
	CAST(SUM(completed) AS BIGINT), CAST(SUM(watched_ms) AS BIGINT)`

// Wilson's 95% score interval pulls the ranking toward zero while evidence
// is scarce. The lower bound supplies the conservative ordering.
func recommendationInterval(plays, impressions int) (float64, float64) {
	const z = 1.959963984540
	n := float64(impressions)
	p := float64(plays) / n
	denom := 1 + z*z/n
	center := (p + z*z/(2*n)) / denom
	margin := (z / denom) * math.Sqrt((p*(1-p)+z*z/(4*n))/n)
	low, high := center-margin, center+margin
	if plays == 0 {
		low = 0
	}
	if plays == impressions {
		high = 1
	}
	return low, high
}

func (e *Engine) recommendationWindow(days int) (int, int) {
	now := e.now().In(e.loc)
	return dayOf(now.AddDate(0, 0, 1-days), e.loc), dayOf(now, e.loc)
}

func (e *Engine) RowInsights(ctx context.Context, days int) ([]core.RowInsight, error) {
	from, to := e.recommendationWindow(recommendationDays(days))
	report, err := e.store.recommendationRows(ctx, from, to, 0)
	if err != nil {
		return nil, err
	}
	out := make([]core.RowInsight, 0, len(report.Rows))
	for _, row := range report.Rows {
		out = append(out, core.RowInsight{HubID: row.Kind, Title: row.Title, Impressions7d: row.Impressions,
			Plays7d: row.Plays, PlayRate: float64(row.Plays) / float64(row.Impressions), LowSample: row.LowSample})
	}
	return out, nil
}

func recommendationDays(days int) int {
	switch days {
	case 7, 30, 90:
		return days
	default:
		return 30
	}
}

func recommendationLabel(kind, first, last string) string {
	info := infoFor(kind)
	if first != "" && !strings.Contains(first, "<") &&
		(first == last || strings.HasPrefix(kind, KindCollectionPrefix)) {
		return first
	}
	switch kind {
	case KindShowcase:
		return "Acclaimed by Genre"
	case KindEra:
		return "Best by Decade"
	case KindWorld:
		return "World Cinema for You"
	case KindTopGenre:
		return "Top-Rated by Genre"
	case KindBridge:
		return "Where Favourites Meet"
	case KindStudio:
		return "More by Studio"
	case KindNetwork:
		return "More by Network"
	}
	return info.title
}

func recommendationFilter(from, to int, viewer int64) (string, []any) {
	where, args := ` WHERE day BETWEEN ? AND ?`, []any{from, to}
	if viewer > 0 {
		where += ` AND user_id = ?`
		args = append(args, viewer)
	}
	return where, args
}

func (s *sqlStore) recommendationRows(ctx context.Context, from, to int, viewer int64) (core.RecommendationReport, error) {
	result := core.RecommendationReport{Rows: []core.RecommendationRow{}}
	where, args := recommendationFilter(from, to, viewer)
	err := queryRecommendations(ctx, s.db, `SELECT kind, MIN(title), MAX(title), COUNT(DISTINCT user_id), `+
		recommendationCountsSQL+` FROM personalize_recommendation_days`+where+` GROUP BY kind`, args, func(rows *sql.Rows) error {
		var row core.RecommendationRow
		var first, last string
		if err := rows.Scan(&row.Kind, &first, &last, &row.Viewers, &row.Impressions, &row.Plays, &row.Measured, &row.Completed, &row.WatchedMS); err != nil {
			return err
		}
		row.Title = recommendationLabel(row.Kind, first, last)
		row.LowSample = row.Impressions < recommendationMinSample
		row.Low, row.High = recommendationInterval(row.Plays, row.Impressions)
		row.RankRate = row.Low
		result.Rows = append(result.Rows, row)
		result.Impressions += row.Impressions
		result.Plays += row.Plays
		result.Measured += row.Measured
		result.Completed += row.Completed
		result.WatchedMS += row.WatchedMS
		return nil
	})
	sort.Slice(result.Rows, func(i, j int) bool {
		a, b := result.Rows[i], result.Rows[j]
		if a.LowSample != b.LowSample {
			return !a.LowSample
		}
		if !a.LowSample && a.RankRate != b.RankRate {
			return a.RankRate > b.RankRate
		}
		if a.Plays != b.Plays {
			return a.Plays > b.Plays
		}
		if a.Impressions != b.Impressions {
			return a.Impressions > b.Impressions
		}
		return a.Kind < b.Kind
	})
	return result, err
}

func (e *Engine) RecommendationReport(ctx context.Context, days int, viewer int64) (core.RecommendationReport, error) {
	days = recommendationDays(days)
	from, to := e.recommendationWindow(days)
	result, err := e.store.recommendationRows(ctx, from, to, viewer)
	if err != nil {
		return result, err
	}
	where, args := recommendationFilter(from, to, viewer)
	if err := e.store.db.QueryRowContext(ctx, `SELECT CAST(COALESCE(SUM(starts), 0) AS BIGINT),
		CAST(COALESCE(SUM(attributed), 0) AS BIGINT) FROM personalize_recommendation_starts`+where, args...).
		Scan(&result.Starts, &result.AttributedStarts); err != nil {
		return result, err
	}
	indices := make(map[string]int, len(result.Rows))
	for i := range result.Rows {
		indices[result.Rows[i].Kind] = i
		result.Rows[i].Trend = make([]*float64, days)
	}
	daysByIndex := map[int]int{}
	for day := dayTime(from, e.loc); dayOf(day, e.loc) <= to; day = day.AddDate(0, 0, 1) {
		daysByIndex[dayOf(day, e.loc)] = len(daysByIndex)
	}
	err = queryRecommendations(ctx, e.store.db, `SELECT kind, day, COUNT(*), COUNT(CASE WHEN play_id <> 0 THEN 1 END)
		FROM personalize_recommendation_days`+where+` GROUP BY kind, day`, args, func(rows *sql.Rows) error {
		var kind string
		var day, impressions, plays int
		if err := rows.Scan(&kind, &day, &impressions, &plays); err != nil {
			return err
		}
		if i, ok := indices[kind]; ok {
			rate := float64(plays) / float64(impressions)
			result.Rows[i].Trend[daysByIndex[day]] = &rate
		}
		return nil
	})
	return result, err
}

func (e *Engine) RecommendationDetail(ctx context.Context, days int, kind string, viewer int64) (core.RecommendationDetail, error) {
	from, to := e.recommendationWindow(recommendationDays(days))
	result := core.RecommendationDetail{}
	where, args := recommendationFilter(from, to, viewer)
	where += ` AND kind = ?`
	args = append(args, kind)
	for _, field := range []struct {
		key, filter, order string
		out                *[]core.RecommendationGroup
	}{
		{"position", " AND position > 0", "position", &result.Positions},
		{"user_id", "", "3 DESC, 2 DESC, user_id", &result.Viewers},
		{"app", "", "3 DESC, 2 DESC, app", &result.Apps},
	} {
		*field.out = []core.RecommendationGroup{}
		err := queryRecommendations(ctx, e.store.db, `SELECT `+field.key+`, `+recommendationCountsSQL+
			` FROM personalize_recommendation_days`+where+field.filter+
			` GROUP BY `+field.key+` ORDER BY `+field.order+` LIMIT 100`, args, func(rows *sql.Rows) error {
			var item core.RecommendationGroup
			if err := rows.Scan(&item.Key, &item.Impressions, &item.Plays, &item.Measured, &item.Completed, &item.WatchedMS); err != nil {
				return err
			}
			*field.out = append(*field.out, item)
			return nil
		})
		if err != nil {
			return result, err
		}
	}
	type titleResult struct {
		key  core.RatingKey
		item core.RecommendationTitle
	}
	var keys []core.RatingKey
	var results []titleResult
	err := queryRecommendations(ctx, e.store.db, `WITH totals AS (
		SELECT item_key, CAST(SUM(impressions) AS BIGINT) AS impressions, CAST(SUM(plays) AS BIGINT) AS plays
		FROM personalize_recommendation_titles`+where+` GROUP BY item_key
	), top AS (SELECT * FROM totals WHERE plays > 0 ORDER BY plays DESC, item_key LIMIT 8),
	unplayed AS (SELECT * FROM totals WHERE plays = 0 ORDER BY impressions DESC, item_key LIMIT 8)
	SELECT * FROM top UNION ALL SELECT * FROM unplayed ORDER BY plays DESC, impressions DESC, item_key`,
		args, func(rows *sql.Rows) error {
			var r titleResult
			if err := rows.Scan(&r.key, &r.item.Impressions, &r.item.Plays); err != nil {
				return err
			}
			keys = append(keys, r.key)
			results = append(results, r)
			return nil
		})
	if err != nil {
		return result, err
	}
	result.Top, result.Unplayed = []core.RecommendationTitle{}, []core.RecommendationTitle{}
	if len(keys) == 0 {
		return result, nil
	}
	var titles map[core.RatingKey]core.Title
	if catalog, ok := e.catalog.(interface {
		CachedGetMany(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)
	}); ok {
		titles, err = catalog.CachedGetMany(ctx, keys)
		if err != nil {
			return result, err
		}
	}
	for _, row := range results {
		row.item.Key = row.key.String()
		row.item.Title = titles[row.key].Title
		if row.item.Plays > 0 {
			result.Top = append(result.Top, row.item)
		} else {
			result.Unplayed = append(result.Unplayed, row.item)
		}
	}
	return result, nil
}

func queryRecommendations(ctx context.Context, db *sql.DB, query string, args []any, consume func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := consume(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
