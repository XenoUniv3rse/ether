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

func TestProbeCompletesOnceThenUsesFeed(t *testing.T) {
	x, searches, _ := probeFixture(t, 0)
	x.cycleFor(t, time.Minute)
	if *searches == 0 {
		t.Fatal("due title did not get a first look")
	}
	before := *searches
	x.now = x.now.Add(8 * time.Hour)
	x.cycleFor(t, time.Minute)
	if *searches != before {
		t.Fatal("completed first look was repeated")
	}
}

// A search that timed out backs off for probeRetry; a spent budget stays due.
func TestProbeRetriesIncompleteFirstLookOnce(t *testing.T) {
	for _, failure := range []error{context.DeadlineExceeded, core.ErrBudgetExhausted} {
		t.Run(failure.Error(), func(t *testing.T) {
			x, searches, answer := probeFixture(t, 0)
			*answer = failure
			x.cycleFor(t, time.Minute)
			first := *searches
			row, err := x.s.titleCheck(t.Context(), core.MovieKey(1))
			if err != nil || row.next.IsZero() || first == 0 {
				t.Fatalf("first look did not queue its retry: %+v %v", row, err)
			}
			x.now = row.next
			x.cycleFor(t, time.Minute)
			second := *searches
			row, err = x.s.titleCheck(t.Context(), core.MovieKey(1))
			if err != nil || !row.next.IsZero() || second <= first {
				t.Fatalf("first look was not settled after retry: %+v %v", row, err)
			}
			x.now = x.now.Add(3 * time.Hour)
			x.cycleFor(t, time.Minute)
			if *searches != second {
				t.Fatal("asked again after the one retry")
			}
		})
	}
}
