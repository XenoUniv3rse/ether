package admin_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/core"
)

// fakeEngine is a scripted Personalization.
type fakeEngine struct {
	fakePages
	mu        sync.Mutex
	overrides []core.TasteOverride
	refreshed []int64
	fail      error
}

type viewerFieldErr struct{ field, msg string }

func (e viewerFieldErr) Error() string              { return e.field + ": " + e.msg }
func (e viewerFieldErr) InvalidFields() [][2]string { return [][2]string{{e.field, e.msg}} }

var poster = core.Title{Key: core.MovieKey(603), Type: core.TypeMovie, Title: "The Signal", Year: 1999, PosterPath: "/m.jpg",
	BackdropPath: "/b.jpg", Runtime: 136 * time.Minute, Genres: []string{"Science Fiction"}, ContentRating: "R", Rating: 8.2}

func (f *fakeEngine) ViewerStats(_ context.Context, ids []int64) (map[int64]core.ViewerStats, error) {
	out := map[int64]core.ViewerStats{}
	for _, id := range ids {
		out[id] = core.ViewerStats{UserID: id, Plays30d: int(id) * 2, HoursWatched30d: 1.5, LastActive: time.Date(2026, 10, 18, 20, 0, 0, 0, time.UTC)}
	}
	return out, nil
}
func (f *fakeEngine) RecentActivity(context.Context, int64, int) ([]core.ViewerActivity, error) {
	return []core.ViewerActivity{{Title: poster, At: time.Date(2026, 10, 18, 21, 0, 0, 0, time.UTC), Completed: true, Progress: 1}}, nil
}
func (f *fakeEngine) Taste(_ context.Context, u *core.User) (*core.TasteProfile, error) {
	return &core.TasteProfile{UserID: u.ID, Confidence: 0.7, Genres: []core.TasteWeight{{Name: "Science Fiction", Weight: 1}},
		People: []core.TastePerson{}, Keywords: []core.TasteWeight{}, Decades: []core.TasteDecade{}, Languages: []core.TasteLanguage{},
		Habits: core.TasteHabits{PeakHours: []int{21}}, Overrides: f.overrides}, nil
}
func (f *fakeEngine) SetTasteOverrides(ctx context.Context, u *core.User, o []core.TasteOverride) (*core.TasteProfile, error) {
	for _, x := range o {
		if x.Weight > 1 {
			return nil, viewerFieldErr{"overrides[0].weight", "must be between -1 and 1"}
		}
	}
	f.mu.Lock()
	f.overrides = o
	f.mu.Unlock()
	return f.Taste(ctx, u)
}
func (f *fakeEngine) Refresh(_ context.Context, u *core.User) error {
	f.mu.Lock()
	f.refreshed = append(f.refreshed, u.ID)
	f.mu.Unlock()
	return nil
}
func (f *fakeEngine) RecommendationReport(context.Context, int, int64) (core.RecommendationReport, error) {
	return core.RecommendationReport{}, nil
}

func (f *fakeEngine) RecommendationDetail(context.Context, int, string, int64) (core.RecommendationDetail, error) {
	return core.RecommendationDetail{}, nil
}

func (f *fakeEngine) RowInsights(context.Context, int) ([]core.RowInsight, error) {
	return []core.RowInsight{{HubID: "top-10", Title: "Top 10 for You", Impressions7d: 20, Plays7d: 5, PlayRate: 0.25}}, nil
}
func (f *fakeEngine) RankerInsight(context.Context) (core.RankerInsight, error) {
	if f.fail != nil {
		return core.RankerInsight{}, f.fail
	}
	at := time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC)
	return core.RankerInsight{TrainedAt: &at, Examples: 5200, Positives: 84, AUC: 0.71, BaselineAUC: 0.63, Active: true,
		Features: []core.RankerFeature{{Name: "fit", Label: "Taste fit", Weight: 0.82}, {Name: "fatigue", Label: "Shown on several days without a play", Weight: -0.4}}}, nil
}
func (f *fakeEngine) Activity(context.Context, int) ([]core.ActivityDay, error) {
	return []core.ActivityDay{{Date: "2026-10-18", Plays: 3, Hours: 2.5, UniqueViewers: 2}}, nil
}

func personalizationHandler(t *testing.T, eng admin.Personalization) http.Handler {
	t.Helper()
	store := newStore(t, nil)
	us := &fakeUsers{users: []core.User{{ID: 1, PlexID: 11, Username: "alice", Title: "Alice", IsOwner: true}, {ID: 2, PlexID: 22, Username: "bob"}}}
	return admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store), Users: us, Personalization: eng})
}

func TestViewersEndpoints(t *testing.T) {
	eng := &fakeEngine{}
	h := personalizationHandler(t, eng)

	rec := serve(h, req(http.MethodGet, "/ether/api/viewers", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("viewers: %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Viewers []map[string]any `json:"viewers"`
	}
	decode(t, rec, &list)
	if len(list.Viewers) != 2 || list.Viewers[0]["username"] != "alice" || list.Viewers[1]["title"] != "bob" ||
		list.Viewers[1]["plays30d"].(float64) != 4 || list.Viewers[0]["lastActive"] != "2026-10-18T20:00:00Z" || list.Viewers[0]["plexId"].(float64) != 11 {
		t.Fatalf("viewers = %+v", list.Viewers)
	}

	rec = serve(h, req(http.MethodGet, "/ether/api/viewers/1", ""))
	var one map[string]any
	decode(t, rec, &one)
	recent := one["recent"].([]any)
	card := recent[0].(map[string]any)["title"].(map[string]any)
	if one["id"].(float64) != 1 || card["key"] != "1000000000000603" || card["poster"] != "/ether/api/image?path=%2Fm.jpg&w=342" ||
		card["runtimeMinutes"].(float64) != 136 || recent[0].(map[string]any)["completed"] != true {
		t.Fatalf("viewer = %+v", one)
	}
	if rec := serve(h, req(http.MethodGet, "/ether/api/viewers/99", "")); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown viewer: %d", rec.Code)
	}
	if rec := serve(h, req(http.MethodGet, "/ether/api/viewers/x", "")); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", rec.Code)
	}

	rec = serve(h, req(http.MethodGet, "/ether/api/viewers/1/taste", ""))
	var taste core.TasteProfile
	decode(t, rec, &taste)
	if taste.UserID != 1 || taste.Genres[0].Name != "Science Fiction" {
		t.Fatalf("taste = %+v", taste)
	}

	rec = serve(h, req(http.MethodPut, "/ether/api/viewers/2/taste/overrides", `{"overrides":[{"kind":"genre","value":"Horror","weight":-1}]}`))
	if rec.Code != http.StatusOK || len(eng.overrides) != 1 || eng.overrides[0].Value != "Horror" {
		t.Fatalf("overrides: %d %s", rec.Code, rec.Body)
	}
	rec = serve(h, req(http.MethodPut, "/ether/api/viewers/2/taste/overrides", `{"overrides":[{"kind":"genre","value":"Horror","weight":5}]}`))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"overrides[0].weight"`) {
		t.Fatalf("invalid overrides: %d %s", rec.Code, rec.Body)
	}
	rec = serve(h, req(http.MethodPut, "/ether/api/viewers/2/taste/overrides", `{"overides":[]}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field accepted: %d", rec.Code)
	}
	// CSRF: mutations need the header.
	r := req(http.MethodPost, "/ether/api/viewers/1/refresh", "")
	r.Header.Del("X-Ether-Request")
	if rec := serve(h, r); rec.Code != http.StatusForbidden {
		t.Fatalf("refresh without CSRF header: %d", rec.Code)
	}
	if rec := serve(h, req(http.MethodPost, "/ether/api/viewers/1/refresh", "")); rec.Code != http.StatusNoContent || len(eng.refreshed) != 1 || eng.refreshed[0] != 1 {
		t.Fatalf("refresh: %d %v", rec.Code, eng.refreshed)
	}
}

func TestInsightsEndpoints(t *testing.T) {
	eng := &fakeEngine{}
	h := personalizationHandler(t, eng)
	rec := serve(h, req(http.MethodGet, "/ether/api/insights/rows", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"impressions7d":20`) || !strings.Contains(rec.Body.String(), `"playRate":0.25`) {
		t.Fatalf("insights rows: %d %s", rec.Code, rec.Body)
	}
	rec = serve(h, req(http.MethodGet, "/ether/api/insights/ranker", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"trainedAt":"2026-10-19T06:00:00Z"`) ||
		!strings.Contains(rec.Body.String(), `"auc":0.71,"baselineAuc":0.63,"active":true`) ||
		!strings.Contains(rec.Body.String(), `"features":[{"name":"fit","label":"Taste fit","weight":0.82}`) {
		t.Fatalf("insights ranker: %d %s", rec.Code, rec.Body)
	}
	rec = serve(h, req(http.MethodGet, "/ether/api/insights/activity", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"days":[{"date":"2026-10-18","plays":3,"hours":2.5,"uniqueViewers":2}]`) {
		t.Fatalf("insights activity: %d %s", rec.Code, rec.Body)
	}
}

// untrainedEngine has never trained its ranker.
type untrainedEngine struct{ fakeEngine }

func (*untrainedEngine) RankerInsight(context.Context) (core.RankerInsight, error) {
	return core.RankerInsight{}, nil
}

func TestRankerInsightUntrained(t *testing.T) {
	h := personalizationHandler(t, &untrainedEngine{})
	rec := serve(h, req(http.MethodGet, "/ether/api/insights/ranker", ""))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"examples":0,"positives":0,"auc":0,"baselineAuc":0,"active":false,"features":[]}` {
		t.Fatalf("untrained ranker: %d %s", rec.Code, rec.Body)
	}
	h = personalizationHandler(t, &fakeEngine{fail: fmt.Errorf("boom")})
	if rec := serve(h, req(http.MethodGet, "/ether/api/insights/ranker", "")); rec.Code < 400 {
		t.Fatalf("failing engine: %d %s", rec.Code, rec.Body)
	}
}

func TestImportedViewersRemainVisibleOutsideActiveMembership(t *testing.T) {
	store := newStore(t, nil)
	active := &fakeUsers{users: []core.User{{ID: 1, Username: "owner", IsOwner: true}}}
	directory := func(context.Context) ([]admin.ViewerRecord, error) {
		return []admin.ViewerRecord{{User: core.User{ID: 1, Username: "owner", IsOwner: true}, Active: true}, {User: core.User{ID: 32, Username: "imported-viewer"}, Origin: "imported"}}, nil
	}
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store), Users: active, ViewerDirectory: directory, Personalization: &fakeEngine{}})
	for _, path := range []string{"/ether/api/viewers", "/ether/api/users", "/ether/api/viewers/32"} {
		rec := serve(h, req(http.MethodGet, path, ""))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "imported-viewer") {
			t.Fatalf("%s hid imported history: %d %s", path, rec.Code, rec.Body)
		}
	}
	for _, path := range []string{"/ether/api/viewers", "/ether/api/viewers/32"} {
		rec := serve(h, req(http.MethodGet, path, ""))
		if !strings.Contains(rec.Body.String(), `"accessStatus":"imported"`) {
			t.Fatalf("missing status: %s", rec.Body)
		}
	}
	for _, path := range []string{"/ether/api/viewers/32/taste"} {
		rec := serve(h, req(http.MethodGet, path, ""))
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	users, _ := active.Users(context.Background())
	if len(users) != 1 || users[0].ID != 1 {
		t.Fatal("historical directory changed membership")
	}
}

func TestViewerDirectoryMembershipStatuses(t *testing.T) {
	store := newStore(t, nil)
	directory := func(context.Context) ([]admin.ViewerRecord, error) {
		return []admin.ViewerRecord{
			{User: core.User{ID: 1, IsOwner: true}, Active: true, Origin: "owner"},
			{User: core.User{ID: 2}, Active: true, Origin: "imported"},
			{User: core.User{ID: 3}, Origin: "imported"},
			{User: core.User{ID: 4}, Origin: "friend"},
		}, nil
	}
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store), Users: &fakeUsers{}, ViewerDirectory: directory, Personalization: &fakeEngine{}})
	for i, status := range []string{"owner", "active", "imported", "inactive"} {
		rec := serve(h, req(http.MethodGet, fmt.Sprintf("/ether/api/viewers/%d", i+1), ""))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"accessStatus":"`+status+`"`) {
			t.Fatalf("%s: %d %s", status, rec.Code, rec.Body)
		}
	}
}

type fakeActivityStats struct {
	gotDays int
	gotTZ   []int
	days    []core.ActivityDay
}

func (f *fakeActivityStats) Activity(_ context.Context, days int, tzMinutes ...int) ([]core.ActivityDay, error) {
	f.gotDays = days
	f.gotTZ = append([]int(nil), tzMinutes...)
	return f.days, nil
}

func TestInsightsActivityUsesStatsAndTimezone(t *testing.T) {
	store := newStore(t, nil)
	act := &fakeActivityStats{
		days: []core.ActivityDay{{Date: "2026-11-24", Plays: 1027, Hours: 28.1, WatchedMS: 101_160_000, UniqueViewers: 5}},
	}
	h := admin.NewHandler(admin.Deps{
		Config:          store,
		IsOwnerToken:    storedOwner(store),
		Personalization: &fakeEngine{},
		ActivityStats:   act,
	})
	rec := serve(h, req(http.MethodGet, "/ether/api/insights/activity?tz=-300", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if act.gotDays != 30 || len(act.gotTZ) != 1 || act.gotTZ[0] != -300 {
		t.Fatalf("ActivityStats called with days=%d tz=%v, want days=30 tz=[-300]", act.gotDays, act.gotTZ)
	}
	if !strings.Contains(rec.Body.String(), `"plays":1027,"hours":28.1,"watchedMs":101160000,"uniqueViewers":5`) {
		t.Fatalf("unexpected body: %s", rec.Body)
	}
}
