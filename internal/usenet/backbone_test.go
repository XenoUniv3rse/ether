package usenet

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/usenet/nntptest"
)

// backboneProvider is a provider at priority prio on backbone b.
func backboneProvider(s *nntptest.Server, id string, prio int, b string) config.UsenetProvider {
	p := s.Provider(id, 2)
	p.Priority = prio
	p.Backbone = b
	return p
}

// A 430 from one account skips the other accounts on its backbone (case
// and spaces ignored) and goes straight to a different store.
func TestBackboneMissSkipsSiblings(t *testing.T) {
	a := nntptest.Start(t, nntptest.Options{})
	b := nntptest.Start(t, nntptest.Options{})
	c := nntptest.Start(t, nntptest.Options{})
	addSeg(t, c, "old@x", []byte("payload"))
	f := newFetcher(t, []config.UsenetProvider{
		backboneProvider(a, "a", 0, "Omicron"),
		backboneProvider(b, "b", 1, " omicron "),
		backboneProvider(c, "c", 2, "abavia"),
	}, Options{})

	got, err := f.Body(context.Background(), "old@x", nil, core.PriorityLive)
	if err != nil || string(got) != "payload" {
		t.Fatalf("Body = %q, %v", got, err)
	}
	if n := a.BodyRequests("old@x"); n != 1 {
		t.Fatalf("a asked %d times, want 1", n)
	}
	if n := b.BodyRequests("old@x"); n != 0 {
		t.Fatalf("b (same backbone as a) asked %d times, want 0", n)
	}

	// A retry within the miss TTL asks neither account on the backbone.
	if _, err := f.Body(context.Background(), "old@x", nil, core.PriorityLive); err != nil {
		t.Fatal(err)
	}
	if a.BodyRequests("old@x") != 1 || b.BodyRequests("old@x") != 0 {
		t.Fatalf("retry re-asked the backbone: a=%d b=%d", a.BodyRequests("old@x"), b.BodyRequests("old@x"))
	}
}

// With the article gone everywhere, siblings count as having answered:
// the result is still a proven miss, not "no providers".
func TestBackboneMissEverywhereIsStillMissing(t *testing.T) {
	a := nntptest.Start(t, nntptest.Options{})
	b := nntptest.Start(t, nntptest.Options{})
	c := nntptest.Start(t, nntptest.Options{})
	f := newFetcher(t, []config.UsenetProvider{
		backboneProvider(a, "a", 0, "omicron"),
		backboneProvider(b, "b", 1, "omicron"),
		backboneProvider(c, "c", 2, ""),
	}, Options{})

	_, err := f.Body(context.Background(), "gone@x", nil, core.PriorityLive)
	if !errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("err = %v, want ErrArticleMissing", err)
	}
	if b.BodyRequests("gone@x") != 0 {
		t.Fatal("same-backbone account was asked")
	}
	if c.BodyRequests("gone@x") != 1 {
		t.Fatal("account on its own store was not asked")
	}
}

// Without a backbone (or on different backbones) every account is asked,
// as before.
func TestNoBackboneAsksEveryAccount(t *testing.T) {
	for _, tc := range []struct{ name, ba, bb string }{
		{"empty", "", ""},
		{"different", "omicron", "abavia"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := nntptest.Start(t, nntptest.Options{})
			b := nntptest.Start(t, nntptest.Options{})
			addSeg(t, b, "x@x", []byte("x"))
			f := newFetcher(t, []config.UsenetProvider{
				backboneProvider(a, "a", 0, tc.ba),
				backboneProvider(b, "b", 1, tc.bb),
			}, Options{})
			if _, err := f.Body(context.Background(), "x@x", nil, core.PriorityLive); err != nil {
				t.Fatal(err)
			}
			if b.BodyRequests("x@x") != 1 {
				t.Fatal("b not asked")
			}
		})
	}
}

// A corrupt copy is one server's reply, not the store's: the sibling is
// still asked and serves the article.
func TestBackboneCorruptDoesNotSkipSiblings(t *testing.T) {
	a := nntptest.Start(t, nntptest.Options{})
	b := nntptest.Start(t, nntptest.Options{})
	addSeg(t, a, "bad@x", []byte("payload"))
	if err := a.Corrupt("bad@x"); err != nil {
		t.Fatal(err)
	}
	addSeg(t, b, "bad@x", []byte("payload"))
	f := newFetcher(t, []config.UsenetProvider{
		backboneProvider(a, "a", 0, "omicron"),
		backboneProvider(b, "b", 1, "omicron"),
	}, Options{})
	got, err := f.Body(context.Background(), "bad@x", nil, core.PriorityLive)
	if err != nil || string(got) != "payload" {
		t.Fatalf("Body = %q, %v", got, err)
	}
}

// Health STATs: a sibling is not asked about articles its backbone already
// lacks, and those still come back as proven misses.
func TestBackboneStatStreamSkipsSiblings(t *testing.T) {
	a := nntptest.Start(t, nntptest.Options{})
	b := nntptest.Start(t, nntptest.Options{})
	block := nntptest.Start(t, nntptest.Options{})
	addSeg(t, a, "here@x", []byte("a"))
	addSeg(t, block, "blockonly@x", []byte("b"))
	bp := backboneProvider(block, "block", 2, "")
	bp.Backfill = true
	f := newFetcher(t, []config.UsenetProvider{
		backboneProvider(a, "a", 0, "omicron"),
		backboneProvider(b, "b", 1, "omicron"),
		bp,
	}, Options{})

	got := map[string]bool{}
	err := f.StatStream(context.Background(), []string{"here@x", "blockonly@x", "nowhere@x"}, func(id string, present bool) bool {
		got[id] = present
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !got["here@x"] || !got["blockonly@x"] || got["nowhere@x"] {
		t.Fatalf("got %v", got)
	}
	if n := b.Stats().Commands["STAT"]; n != 0 {
		t.Fatalf("same-backbone account sent %d STATs, want 0", n)
	}
	if n := block.Stats().Commands["STAT"]; n == 0 {
		t.Fatal("backfill on its own store was not asked")
	}
}
