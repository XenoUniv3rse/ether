package collections_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestCollectionCardSeeksVisibleMixedMembers(t *testing.T) {
	e := newEnv(t, withOpts(collections.WithCollectionCardFilter(func(_ context.Context, u *core.User, typ core.MediaType) (core.Filter, bool, error) {
		return core.Filter{Type: typ, LocalOnly: true}, u.IsOwner || typ == core.TypeShow, nil
	})))
	sp := spec("Custom Mix", "mixed", catalogSource())
	sp.Artwork = core.CollectionArtwork{Mode: "custom", URL: "/custom-card.jpg"}
	saved, err := e.svc.CreateSpecDeferred(e.ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	k, err := core.ParseRatingKey(saved.RatingKey)
	if err != nil {
		t.Fatal(err)
	}
	db := e.db.(*sql.DB)
	var movie, show core.RatingKey
	for _, fixture := range []struct {
		typ core.MediaType
		key *core.RatingKey
	}{{core.TypeMovie, &movie}, {core.TypeShow, &show}} {
		if err = db.QueryRowContext(e.ctx, "SELECT key FROM catalog_titles WHERE type=? LIMIT 1", fixture.typ).Scan(fixture.key); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.ExecContext(e.ctx, "UPDATE collections_specs SET item_count=2,build_hash='test' WHERE id=?", k.TMDB()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(e.ctx, "INSERT INTO collections_members (spec_id,pos,key,shared) VALUES (?,0,?,1),(?,1,?,1)", k.TMDB(), show, k.TMDB(), movie); err != nil {
		t.Fatal(err)
	}
	owner := &core.User{ID: 1, IsOwner: true}
	first, rev, err := e.svc.CollectionCardPage(e.ctx, owner, k, -1, 0, 1)
	if err != nil || len(first) != 1 || first[0].Key != show || rev == "" {
		t.Fatalf("first page = %v, revision %q, err %v", first, rev, err)
	}
	second, _, err := e.svc.CollectionCardPage(e.ctx, owner, k, first[0].Position, 1, 2)
	if err != nil || len(second) != 1 || second[0].Key != movie {
		t.Fatalf("second page = %v, err %v", second, err)
	}
	restricted, _, err := e.svc.CollectionCardPage(e.ctx, &core.User{ID: 2}, k, -1, 0, 2)
	if err != nil || len(restricted) != 1 || restricted[0].Key != show {
		t.Fatalf("restricted page = %v, err %v", restricted, err)
	}
}
