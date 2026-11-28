package newznab

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestCategoryListingReusesPaidForTitles(t *testing.T) {
	var searches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("t") == "caps" {
			fmt.Fprint(w, `<caps><categories><category id="5000"/></categories></caps>`)
			return
		}
		searches.Add(1)
		if q.Get("q") == "" {
			t.Error("category listing asked the indexer for a feed")
		}
		fmt.Fprint(w, `<rss><channel><item><title>Show.S01E03.1080p</title><guid>g-1</guid></item></channel></rss>`)
	}))
	defer srv.Close()
	cfg := testConfig(srv.URL)
	m := NewManager(store.OpenTest(t), []config.Indexer{cfg}, Options{})
	if rs, err := m.Search(t.Context(), core.SearchRequest{Title: "Show", Category: "tv"}); err != nil || len(rs) == 0 {
		t.Fatalf("targeted search: %v", err)
	}
	before := searches.Load()
	if rs, err := m.Search(t.Context(), core.SearchRequest{Category: "tv"}); err != nil || len(rs) == 0 || searches.Load() != before {
		t.Fatalf("listing did not reuse the targeted search: %v", err)
	}
}
