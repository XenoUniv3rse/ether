package resolver

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// playMarkIndexer holds the search until the shared resolution reads as a
// viewer's explicit Play (or a bound passes), then records what source
// preparation sees.
type playMarkIndexer struct {
	fakeIndexer
	entered chan struct{}
	mu      sync.Mutex
	marks   []bool
}

func (p *playMarkIndexer) Search(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	for deadline := time.Now().Add(5 * time.Second); !core.ExplicitPlay(ctx) && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	return p.fakeIndexer.Search(ctx, req)
}

func (p *playMarkIndexer) PrepareSources(ctx context.Context, releases []core.Release) []core.Release {
	p.mu.Lock()
	p.marks = append(p.marks, core.ExplicitPlay(ctx))
	p.mu.Unlock()
	return releases
}

// A Play that joins a resolution a detail open started makes the source
// preparation that follows a Play's.
func TestResolveFlightLatchesExplicitPlay(t *testing.T) {
	cfg := testCfg()
	cfg.Resolver.SourcePreference = PreferDebrid
	releases := routingReleases("The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-OTHER")
	second := releases[1]
	second.ID, second.InfoHash = "torrent2", ""
	idx := &playMarkIndexer{fakeIndexer: fakeIndexer{releases: append(releases, second)}, entered: make(chan struct{}, 1)}
	svc := New(idx, fakeHealth{}, nil, cfg, Options{})
	defer svc.Close()
	req := core.ResolveRequest{Title: routingTitle(false), Client: webCaps()}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = svc.Resolve(context.Background(), req) // a detail open
		done <- struct{}{}
	}()
	<-idx.entered
	go func() {
		_, _ = svc.Resolve(core.WithExplicitPlay(context.Background()), req) // Play joins
		done <- struct{}{}
	}()
	<-done
	<-done
	if idx.searches.Load() != 1 {
		t.Fatalf("searches=%d, want one shared resolution", idx.searches.Load())
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if len(idx.marks) == 0 {
		t.Fatal("source preparation did not run")
	}
	for _, m := range idx.marks {
		if !m {
			t.Fatalf("source preparation explicit Play %v after a Play joined", idx.marks)
		}
	}
}
