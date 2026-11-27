package availability

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

type gateSeedValidator struct {
	fakeValidator
	check func(context.Context, *core.Title) (string, error)
}

func (v gateSeedValidator) FeedCandidate(ctx context.Context, title *core.Title, r core.Release) (core.Release, string, error) {
	reason, err := v.check(ctx, title)
	return r, reason, err
}

func TestGateSeedPublishesAndResumesWithoutHidingRetainedEvidence(t *testing.T) {
	ctx := t.Context()
	db := store.OpenTest(t)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	cfg := config.Defaults(t.TempDir())
	cfg.Availability.Mode = config.ModePlayable
	cfg.Usenet.Providers = []config.UsenetProvider{{Enabled: true, Host: "fixture"}}
	c, err := catalog.New(db, nil, cfg.Catalog, nil, catalog.WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	last := core.MovieKey(gateSeedBatch + 1)
	for i := int64(1); i <= gateSeedBatch+1; i++ {
		key := core.MovieKey(i)
		title := core.Title{Key: key, Type: core.TypeMovie, Title: fmt.Sprintf("Movie %03d", i), Released: now.Add(-time.Hour)}
		if err := c.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO availability_releases
 (id,rating_key,source_id,generation,payload,title,name,protocol,first_seen,last_seen,state)
 VALUES (?,?,'probe','','{"ID":"test","Name":"Test","Protocol":"usenet"}','','','usenet',?,?,'verified')`, key.String(), key, now.Unix(), now.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	newService := func(check func(context.Context, *core.Title) (string, error)) *Service {
		t.Helper()
		s, err := New(Deps{DB: db, Config: func() config.Config { return cfg }, Feeds: &fakeFeeds{},
			Catalog: c, Validator: &gateSeedValidator{check: check}, Now: func() time.Time { return now }, Async: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		return s
	}
	started := make(chan struct{})
	s := newService(func(ctx context.Context, title *core.Title) (string, error) {
		if title.Key == last && ctx.Value(offRequestKey{}) != nil {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		}
		if title.Key == last {
			return "quality", nil
		}
		return "", nil
	})
	browse := func(s *Service, want int) {
		t.Helper()
		f, err := s.PagingFilter(ctx, core.Filter{Type: core.TypeMovie})
		if err != nil {
			t.Fatal(err)
		}
		page, err := c.Browse(ctx, core.BrowseQuery{Filter: f, Sort: core.SortTitle, Limit: 2})
		if err != nil || page.Total != want {
			t.Fatalf("browse total=%d want=%d error=%v", page.Total, want, err)
		}
	}
	browse(s, gateSeedBatch+1)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !s.isGateOpen(ctx, s.currentGatePolicy(), core.MovieKey(1)) ||
		s.isGateOpen(ctx, s.currentGatePolicy(), last) {
		t.Fatal("expected the committed first batch and an unfinished last batch")
	}
	browse(s, gateSeedBatch+1)
	lastTitle, err := c.CachedTitle(ctx, last)
	if err != nil || len(s.FilterDiscovery(ctx, []core.Title{*lastTitle})) != 1 {
		t.Fatal("detail did not provisionally admit retained evidence")
	}
	s.Close()

	resumed := newService(func(_ context.Context, title *core.Title) (string, error) {
		if title.Key != last {
			t.Errorf("completed batch was evaluated again for key %v", title.Key)
		}
		return "quality", nil
	})
	resumed.requestGateSeed(resumed.discoverySnapshot())
	resumed.bg.Wait()
	browse(resumed, gateSeedBatch)
	if len(resumed.FilterDiscovery(ctx, []core.Title{*lastTitle})) != 0 {
		t.Fatal("detail retained provisional admission after seeding completed")
	}
}
