package newznab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// On an indexer that reports no limits (assumed: the strictest private
// tier, 1,000 API requests and 1,000 grabs a day), work nobody is waiting
// on takes at most 10% of each; playback is never refused by that share,
// a warm-up promoted to Play is playback, and an NZB once downloaded is
// read from disk instead of being grabbed again.
func TestUnattendedWorkKeepsToItsShare(t *testing.T) {
	var hits, grabs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/nzb" {
			grabs.Add(1)
			w.Write([]byte(`<?xml version="1.0"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"><file subject="x"><segments><segment bytes="1" number="1">a@b</segment></segments></file></nzb>`))
			return
		}
		if r.URL.Query().Get("t") != "caps" {
			hits.Add(1)
		}
		w.Write([]byte(`<?xml version="1.0"?><rss><channel></channel></rss>`))
	}))
	defer srv.Close()
	m := NewManager(store.OpenTest(t), []config.Indexer{{ID: "ix", Name: "IX", URL: srv.URL, APIKey: "k", Enabled: true}}, Options{HTTPClient: srv.Client(), NZBDir: t.TempDir()})
	warm := core.WithUnattendedSearch(core.WithIndexerPurpose(context.Background(), core.IndexerPurposeWarmup))
	play := core.WithIndexerPurpose(context.Background(), core.IndexerPurposePlayback)
	search := func(ctx context.Context, n int, prefix string) {
		for i := range n {
			_, _ = m.Search(ctx, core.SearchRequest{Type: core.TypeMovie, Title: fmt.Sprint(prefix, i), Year: 2020})
		}
	}
	release := func(i int) core.Release {
		return core.Release{ID: fmt.Sprint("r", i), Name: fmt.Sprint("r", i), Indexer: "IX", NZBURL: srv.URL + "/nzb"}
	}
	search(warm, 150, "w")
	if hits.Load() != 100 {
		t.Fatalf("unattended searches reached the indexer %d times, want its 10%% share (100)", hits.Load())
	}
	search(play, 20, "p")
	promoted, promote := core.WithSpeculative(core.WithIndexerPurpose(context.Background(), core.IndexerPurposeWarmup))
	promote()
	search(promoted, 5, "q")
	if hits.Load() != 125 {
		t.Fatalf("playback and promoted warm-ups were refused: %d hits, want 125", hits.Load())
	}
	for i := range 130 {
		_, _ = m.FetchNZB(warm, release(i))
	}
	if grabs.Load() != 100 {
		t.Fatalf("unattended grabs reached the indexer %d times, want 100", grabs.Load())
	}
	for i := range 130 {
		if _, err := m.FetchNZB(play, release(i)); err != nil {
			t.Fatalf("playback grab %d refused: %v", i, err)
		}
	}
	if grabs.Load() != 130 {
		t.Fatalf("indexer grabs %d, want 130: a downloaded NZB is read from disk", grabs.Load())
	}
}
