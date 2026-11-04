package availability

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

type pagedEpisodeCatalog struct{ *fakeCatalog }

func (c *pagedEpisodeCatalog) RecentEvidenceEpisodes(_ context.Context, since time.Time) ([]core.Title, error) {
	var out []core.Title
	for _, t := range c.titles {
		if t.Type == core.TypeEpisode && t.Released.After(since) {
			out = append(out, t)
		}
	}
	return out, nil
}

// A page must not use an episode decision invalidated by a release change.
func TestPagedEpisodeDecisionWaitsForInvalidation(t *testing.T) {
	x := setup(t)
	ep := core.Title{Key: core.EpisodeKey(99, 1, 1), Type: core.TypeEpisode, Title: "Episode", Released: x.now.Add(-time.Hour)}
	x.cat.titles[ep.Key] = ep
	x.s.d.Catalog = &pagedEpisodeCatalog{x.cat}
	x.s.d.Async = true
	defer x.s.Close()
	input := x.s.discoverySnapshot()
	x.s.visibilityMu.Lock()
	for _, kind := range []string{hiddenMovies, hiddenShows, hiddenEpisodes} {
		x.s.hiddenSets[kind] = &hiddenSet{key: hiddenKey(kind, input), at: x.now, covered: map[core.RatingKey]bool{ep.Key: true}, hidden: map[core.RatingKey]bool{}}
	}
	x.s.visibilityMu.Unlock()
	x.s.clearVisibility()
	f, err := x.s.PagingFilter(t.Context(), core.Filter{Type: core.TypeEpisode})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := x.s.db.QueryRow(`SELECT COUNT(*) FROM availability_hidden WHERE kind='episode' AND generation=? AND rating_key=?`, f.HiddenEpisodeGeneration, ep.Key).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("invalidated unavailable episode was not excluded")
	}
}

// A reader that captured an older generation must still see precisely that
// set after a later publication. This also covers the empty-set marker.
func TestPublishedHiddenGenerationsRemainImmutable(t *testing.T) {
	x := setup(t)
	ctx := t.Context()
	first := core.ShowKey(101)
	second := core.ShowKey(102)
	if err := x.s.publishHidden(ctx, hiddenShows, "test", []core.RatingKey{first}); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := x.s.db.QueryRowContext(ctx, `SELECT generation FROM availability_hidden_current WHERE kind=?`, hiddenShows).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := x.s.publishHidden(ctx, hiddenShows, "test", []core.RatingKey{second}); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := x.s.db.QueryRowContext(ctx, `SELECT generation FROM availability_hidden_current WHERE kind=?`, hiddenShows).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("different sets shared a generation")
	}
	for _, v := range []struct {
		gen string
		key core.RatingKey
	}{{before, first}, {after, second}} {
		var n int
		if err := x.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM availability_hidden WHERE kind=? AND generation=? AND rating_key=?`, hiddenShows, v.gen, v.key).Scan(&n); err != nil || n != 1 {
			t.Fatalf("generation lost its member: count=%d err=%v", n, err)
		}
	}
	if err := x.s.publishHidden(ctx, hiddenShows, "test", nil); err != nil {
		t.Fatal(err)
	}
	var empty string
	if err := x.s.db.QueryRowContext(ctx, `SELECT generation FROM availability_hidden_current WHERE kind=?`, hiddenShows).Scan(&empty); err != nil || empty == before || empty == after {
		t.Fatalf("empty generation not published: %v", err)
	}
}

// SQLite's 64-bit rating keys must remain 64-bit on PostgreSQL.
func TestHiddenMigrationTranslatesToPostgres(t *testing.T) {
	ddl := store.TranslateDDL(hiddenMigration)
	for _, want := range []string{"rating_key BIGINT NOT NULL", "created_at BIGINT NOT NULL", "PRIMARY KEY(kind,generation,rating_key)"} {
		if !strings.Contains(ddl, want) {
			t.Fatalf("translated migration missing %q", want)
		}
	}
}
