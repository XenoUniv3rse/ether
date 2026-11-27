package newznab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestRecentSharesAccountBudgetWithPlayback(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("t") == "caps" {
			fmt.Fprint(w, `<caps/>`)
			return
		}
		if q.Get("q") != "" {
			fmt.Fprint(w, `<rss><channel><item><title>Film.2020.1080p</title><guid>play</guid></item></channel></rss>`)
			return
		}
		calls.Add(1)
		if q.Get("t") != "search" || q.Get("q") != "" || q.Get("limit") != "100" || q.Get("cat") != "2040,5000" {
			t.Errorf("unexpected request: %v", q)
		}
		fmt.Fprint(w, `<rss version="2.0"><channel><item><title>Film.2020.1080p.WEB-DL.x264</title><guid>one</guid></item></channel></rss>`)
	}))
	defer srv.Close()
	cfg := testConfig(srv.URL)
	cfg.DailyLimit = 15
	cfg.MovieCategories = []int{2040}
	db := store.OpenTest(t)
	m := NewManager(db, []config.Indexer{cfg}, Options{})
	var charged atomic.Int32
	bg := core.WithForegroundReserve(core.WithBackgroundBudget(t.Context(), func(_ context.Context, kind, source string, n int64) error {
		if kind != "hit" {
			t.Errorf("unexpected cost %s", kind)
		}
		charged.Add(1)
		return nil
	}), 80)
	for i := 0; i < 2; i++ {
		rs, e := m.Recent(bg, cfg.ID, 0, 100)
		if e != nil || len(rs) != 1 {
			t.Fatal(rs, e)
		}
	}
	beforeDenial := calls.Load()
	if _, e := m.Recent(bg, cfg.ID, 0, 100); !errors.Is(e, core.ErrBudgetExhausted) {
		t.Fatal("background invaded reserve", e)
	}
	if calls.Load() != beforeDenial {
		t.Fatal("denied work hit network")
	}
	// Foreground still uses the account's remaining allowance.
	if rs, e := m.Search(t.Context(), core.SearchRequest{Type: core.TypeMovie, Title: "Film"}); e != nil || len(rs) == 0 {
		t.Fatal("foreground lost its reserve", rs, e)
	}
}
func TestRecentDeniedBeforeNetwork(t *testing.T) {
	f := newFakeIndexer(t, "")
	cfg := testConfig(f.server.URL)
	m := NewManager(nil, []config.Indexer{cfg}, Options{})
	ctx := core.WithBackgroundBudget(t.Context(), func(context.Context, string, string, int64) error { return core.ErrBudgetExhausted })
	if _, e := m.Recent(ctx, cfg.ID, 0, 100); !errors.Is(e, core.ErrBudgetExhausted) {
		t.Fatal(e)
	}
	if f.hitsFor("search")+f.hitsFor("caps") != 0 {
		t.Fatal("spent denied allowance")
	}
}

func TestBackgroundCannotConsumeTinyForegroundReserve(t *testing.T) {
	f := newFakeIndexer(t, "")
	cfg := testConfig(f.server.URL)
	cfg.DailyLimit = 1
	m := NewManager(nil, []config.Indexer{cfg}, Options{})
	ctx := core.WithForegroundReserve(core.WithBackgroundBudget(t.Context(), func(context.Context, string, string, int64) error { return nil }), 80)
	if _, e := m.Recent(ctx, cfg.ID, 0, 100); !errors.Is(e, core.ErrBudgetExhausted) {
		t.Fatal("rounded away reserved capacity", e)
	}
	if f.hitsFor("search") != 0 {
		t.Fatal("spent foreground reserve")
	}
}

func TestRecentCanResumeAtAnOlderPage(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("t") == "caps" {
			fmt.Fprint(w, `<caps/>`)
			return
		}
		polls.Add(1)
		if r.URL.Query().Get("cat") == "" {
			t.Error("recent poll lost its categories")
		}
		fmt.Fprint(w, `<rss><channel/></rss>`)
	}))
	defer srv.Close()
	cfg := testConfig(srv.URL)
	m := NewManager(store.OpenTest(t), []config.Indexer{cfg}, Options{})
	if _, err := m.Recent(t.Context(), cfg.ID, 0, 100); err != nil {
		t.Fatal(err)
	}
	before := polls.Load()
	if _, err := m.Recent(t.Context(), cfg.ID, 100, 100); err != nil || polls.Load() <= before {
		t.Fatalf("gap page did not reach the source: %v", err)
	}
}
