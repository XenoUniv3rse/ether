package favorites

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestPageListsOneRoleInOrderAcrossPages(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	roles := map[core.RatingKey]string{}
	hidden := map[string]bool{}
	opts := Options{DB: db, Marked: state.New(db, state.Options{}).Playlists().(core.TransactionalMarkedPlaylists),
		Now:       func() time.Time { now = now.Add(time.Second); return now },
		Active:    func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil },
		Allowed:   func(_ context.Context, _ *core.User, s Subject) bool { return !hidden[s.ID] },
		Changed:   func(context.Context, int64, Subject) {},
		TitleRole: func(_ context.Context, k core.RatingKey) string { return roles[k] }}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	u := &core.User{ID: 1}
	like := func(k core.RatingKey, role string) {
		roles[k] = role
		if _, err := s.Set(ctx, u, Subject{Kind: KindTitle, ID: k.String()}, true); err != nil {
			t.Fatal(err)
		}
	}
	var tracks []core.RatingKey
	for i := int64(1); i <= 7; i++ {
		k := core.PluginKey(100 + i)
		tracks = append(tracks, k)
		like(k, "track")
		like(core.MovieKey(i), "movie")
	}
	like(core.PluginKey(900), "album")
	hidden[tracks[4].String()] = true

	var got []string
	cursor := ""
	for pages := 0; pages < 5; pages++ {
		page, err := s.Page(ctx, u, PageQuery{Role: "track", Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 7 {
			t.Fatalf("total %d", page.Total)
		}
		for _, e := range page.Entries {
			got = append(got, e.Subject.ID)
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	// Newest first, the hidden track skipped, no movie or album.
	want := []string{tracks[6].String(), tracks[5].String(), tracks[3].String(), tracks[2].String(), tracks[1].String(), tracks[0].String()}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order: got %v want %v", got, want)
		}
	}
	if _, err := s.Page(ctx, u, PageQuery{Role: "track", Cursor: "not-a-cursor"}); err != ErrBadCursor {
		t.Fatalf("bad cursor: %v", err)
	}
}

func TestBackfillRolesTypesFavoritesSetBeforeRoles(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	base := Options{DB: db, Marked: state.New(db, state.Options{}).Playlists().(core.TransactionalMarkedPlaylists),
		Active:  func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil },
		Allowed: func(context.Context, *core.User, Subject) bool { return true },
		Changed: func(context.Context, int64, Subject) {}}
	old, err := New(base) // no TitleRole: rows stored untyped
	if err != nil {
		t.Fatal(err)
	}
	u := &core.User{ID: 3}
	for _, k := range []core.RatingKey{core.PluginKey(1), core.PluginKey(2), core.MovieKey(9)} {
		if _, err := old.Set(ctx, u, Subject{Kind: KindTitle, ID: k.String()}, true); err != nil {
			t.Fatal(err)
		}
	}
	base.TitleRole = func(_ context.Context, k core.RatingKey) string {
		if k.IsPluginKey() {
			return "track"
		}
		return "movie"
	}
	s, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BackfillRoles(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := s.Page(ctx, u, PageQuery{Role: "track"})
	if err != nil || page.Total != 2 || len(page.Entries) != 2 {
		t.Fatalf("typed page: %+v %v", page, err)
	}
}
