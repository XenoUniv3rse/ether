package availability

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func probeFixture(t *testing.T, digital time.Duration) (*fixture, *int, *error) {
	t.Helper()
	x := setup(t)
	x.c.Availability.Verify = false
	x.c.Availability.CatchUp = false
	title := x.cat.titles[core.MovieKey(1)]
	title.Released = x.now.Add(-20 * 24 * time.Hour)
	title.IDs.IMDB = "tt0000001"
	x.cat.titles[title.Key] = title
	x.s.d.Catalog = probeCatalog{fakeCatalog: x.cat, homes: map[core.RatingKey]time.Time{title.Key: x.now.Add(digital)}}
	searches := new(int)
	fail := new(error)
	x.s.d.Search = func(context.Context, core.SearchRequest) ([]core.Release, error) {
		*searches++
		return nil, *fail
	}
	return x, searches, fail
}

func (x *fixture) cycleFor(t *testing.T, d time.Duration) {
	t.Helper()
	for end := x.now.Add(d); x.now.Before(end); x.now = x.now.Add(time.Minute) {
		if err := x.s.Cycle(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProbeBoundedRetrySchedule(t *testing.T) {
	anchor := int64(1794441600)
	end := anchor + int64((30 * 24 * time.Hour).Seconds())
	if got := nextRetryAfter(anchor, end, anchor-1); got != anchor {
		t.Fatalf("first search should wait for the release: %d", got)
	}
	day := int64((24 * time.Hour).Seconds())
	if got := nextRetryAfter(anchor, end, anchor+3*day); got != anchor+7*day {
		t.Fatalf("next long-tail search should be weekly: %d", got)
	}
	x, searches, _ := probeFixture(t, 0)
	x.cycleFor(t, 8*time.Hour+time.Minute)
	if *searches == 0 || *searches > 3 {
		t.Fatalf("targeted retries did not stay within the early slots: %d", *searches)
	}
	before := *searches
	x.cycleFor(t, 15*time.Hour)
	if *searches != before {
		t.Fatalf("searched during the overnight backoff: %d", *searches)
	}
}

func TestProbeWaitsForAFutureDigitalDate(t *testing.T) {
	x, searches, _ := probeFixture(t, 10*time.Hour)
	x.cycleFor(t, 9*time.Hour)
	if *searches != 0 {
		t.Fatalf("searched before the digital date: %d", *searches)
	}
	x.cycleFor(t, 75*time.Minute)
	if *searches == 0 {
		t.Fatal("the digital date did not trigger a search")
	}
}

// A search that timed out backs off for probeRetry; a spent budget stays due.
func TestProbeTimeoutIsNotRetriedEveryCycle(t *testing.T) {
	x, searches, fail := probeFixture(t, 0)
	*fail = context.DeadlineExceeded
	x.cycleFor(t, 30*time.Minute)
	if *searches != 1 {
		t.Fatalf("timed-out search retried %d times in 30 minutes", *searches)
	}

	y, budgetSearches, budgetFail := probeFixture(t, 0)
	*budgetFail = core.ErrBudgetExhausted
	y.cycleFor(t, 3*time.Minute)
	if *budgetSearches > 1 {
		t.Fatalf("budget-denied title retried before the reset: %d", *budgetSearches)
	}
}
